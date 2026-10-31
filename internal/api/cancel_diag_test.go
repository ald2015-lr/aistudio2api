package api

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// stallingService 先按 before 发出事件（每个事件同时像生成服务一样记下上游事件时刻与首事件耗时），之后一直不再有事件，直到请求结束
type stallingService struct {
	modelListService
	before []aistudio.Event
}

func (service *stallingService) Generate(ctx context.Context, _ aistudio.GenerateRequest) (<-chan aistudio.Event, error) {
	events := make(chan aistudio.Event)
	go func() {
		defer close(events)
		for index, event := range service.before {
			if index == 0 {
				SetAccessLogFirstEvent(ctx, time.Millisecond)
			}
			MarkAccessLogUpstreamEvent(ctx)
			select {
			case events <- event:
			case <-ctx.Done():
				return
			}
		}
		<-ctx.Done()
	}()
	return events, nil
}

// awaitFinished 等请求日志写出一条完成记录
func awaitFinished(t *testing.T, admin *capturingAdmin) AccessLog {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if entries := admin.finished(); len(entries) > 0 {
			return entries[0]
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("没有写出请求完成记录")
	return AccessLog{}
}

// startStream 经真实 TCP 发起一次流式请求，返回响应与取消函数（取消即断开连接）
func startStream(t *testing.T, server *httptest.Server, path string, body string) (*http.Response, context.CancelFunc, error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	request, _ := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+path, strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer sk-test")
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	return response, cancel, err
}

// TestCancelDiagnosticsBeforeFirstEvent 客户端在首个事件之前、响应头提交之前断开：记为 499，断开现场为客户端断开、首事件前、
// 响应头未提交、没有写出任何字节、没有上游事件，距最近写出从请求进入服务算起
func TestCancelDiagnosticsBeforeFirstEvent(t *testing.T) {
	admin := &capturingAdmin{}
	server := httptest.NewServer(NewHandler(&stallingService{}, Config{APIKey: "sk-test", Admin: admin}))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	request, _ := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/v1beta/models/gemini-3.8-flash:streamGenerateContent?alt=sse",
		strings.NewReader(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`))
	request.Header.Set("Authorization", "Bearer sk-test")
	done := make(chan struct{})
	go func() {
		defer close(done)
		if response, err := http.DefaultClient.Do(request); err == nil {
			response.Body.Close()
		}
	}()
	time.Sleep(200 * time.Millisecond)
	cancel()
	<-done
	entry := awaitFinished(t, admin)
	disconnect := entry.Disconnect
	if entry.Status != 499 || disconnect == nil {
		t.Fatalf("status=%d disconnect=%+v，期望 499 并带断开现场", entry.Status, disconnect)
	}
	if disconnect.Initiator != "client" || disconnect.Phase != "before_first_event" || disconnect.HeadersSent ||
		disconnect.WrittenBytes != 0 || disconnect.WrittenChunks != 0 || disconnect.SinceUpstreamEventMS != nil ||
		disconnect.SinceLastDataWriteMS != nil || disconnect.SinceHeadersSentMS != nil {
		t.Fatalf("断开现场 = %+v", disconnect)
	}
	if disconnect.SinceLastWriteMS < 150 || disconnect.ElapsedMS < 150 || disconnect.SinceLastWriteMS > disconnect.ElapsedMS+1 {
		t.Fatalf("没有写出过时距最近写出应从请求进入服务算起: %+v", disconnect)
	}
}

// TestCancelDiagnosticsMidStream 客户端收到首个事件后断开：断开现场为客户端断开、流中、响应头已提交、写出的字节与块数（首个事件很快到达，
// 提交响应头时不写心跳块，只有思考这一块），最后一次写出、最后一次数据写出、响应头提交与上游最近一次事件都在断开前约 200ms
func TestCancelDiagnosticsMidStream(t *testing.T) {
	admin := &capturingAdmin{}
	service := &stallingService{before: []aistudio.Event{{Kind: aistudio.EventReasoning, Text: "思考中"}}}
	server := httptest.NewServer(NewHandler(service, Config{APIKey: "sk-test", Admin: admin}))
	defer server.Close()
	response, cancel, err := startStream(t, server, "/v1beta/models/gemini-3.8-flash:streamGenerateContent?alt=sse",
		`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`)
	if err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(response.Body)
	received := 0
	for received < 1 {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("读取失败: %v", err)
		}
		if strings.HasPrefix(line, "data: ") {
			received++
		}
	}
	time.Sleep(200 * time.Millisecond)
	cancel()
	response.Body.Close()
	entry := awaitFinished(t, admin)
	disconnect := entry.Disconnect
	if entry.Status != 499 || disconnect == nil {
		t.Fatalf("status=%d disconnect=%+v", entry.Status, disconnect)
	}
	if disconnect.Initiator != "client" || disconnect.Phase != "mid_stream" || !disconnect.HeadersSent ||
		disconnect.WrittenBytes <= 0 || disconnect.WrittenChunks != 1 {
		t.Fatalf("断开现场 = %+v，期望流中、已提交响应头、写出 1 块（思考）", disconnect)
	}
	if disconnect.SinceLastWriteMS < 150 || disconnect.SinceUpstreamEventMS == nil || *disconnect.SinceUpstreamEventMS < 150 ||
		disconnect.SinceLastDataWriteMS == nil || *disconnect.SinceLastDataWriteMS < 150 ||
		disconnect.SinceHeadersSentMS == nil || *disconnect.SinceHeadersSentMS < 150 {
		t.Fatalf("最后一次写出、数据写出、响应头提交与上游最近一次事件应在断开前约 200ms: %+v", disconnect)
	}
}

// TestCancelDiagnosticsRightAfterHeaders 写响应头后立即断开与阶段无关：首个事件在等待内就到、响应头随它提交，客户端（经 new-api 时是 new-api
// 发现手机已走）一收到响应头就断开，阶段为“流中”，right_after_headers 为真；收到数据 1 秒以后才断开的不算；服务端取消的不算
func TestCancelDiagnosticsRightAfterHeaders(t *testing.T) {
	for _, test := range []struct {
		name  string
		delay time.Duration
		want  bool
	}{
		{name: "收到响应头就断开", delay: 0, want: true},
		{name: "1 秒以后才断开", delay: 1200 * time.Millisecond, want: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			admin := &capturingAdmin{}
			service := &stallingService{before: []aistudio.Event{{Kind: aistudio.EventReasoning, Text: "思考中"}}}
			server := httptest.NewServer(NewHandler(service, Config{APIKey: "sk-test", Admin: admin}))
			defer server.Close()
			response, cancel, err := startStream(t, server, "/v1beta/models/gemini-3.8-flash:streamGenerateContent?alt=sse",
				`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`)
			if err != nil {
				t.Fatal(err)
			}
			time.Sleep(test.delay)
			cancel()
			response.Body.Close()
			disconnect := awaitFinished(t, admin).Disconnect
			if disconnect == nil || disconnect.Initiator != "client" || disconnect.Phase != "mid_stream" || !disconnect.HeadersSent {
				t.Fatalf("断开现场 = %+v，期望客户端断开、流中、响应头已提交", disconnect)
			}
			if disconnect.RightAfterHeaders != test.want {
				t.Fatalf("right_after_headers = %v，期望 %v: %+v（提交响应头后 %v）", disconnect.RightAfterHeaders, test.want, disconnect, *disconnect.SinceHeadersSentMS)
			}
		})
	}
	// 只按现场推算：服务端取消、没提交响应头的都不算
	now := time.Now()
	for _, moment := range []accessDisconnect{
		{at: now, client: false, headersSent: true, headerAt: now.Add(-100 * time.Millisecond), started: now.Add(-time.Second)},
		{at: now, client: true, started: now.Add(-time.Second)},
	} {
		if report := moment.report(); report.RightAfterHeaders {
			t.Fatalf("不应算写响应头后立即断开: %+v", report)
		}
	}
	right := accessDisconnect{at: now, client: true, headersSent: true, headerAt: now.Add(-999 * time.Millisecond), firstEvent: true, started: now.Add(-3 * time.Second)}
	if report := right.report(); !report.RightAfterHeaders || report.Phase != "mid_stream" {
		t.Fatalf("提交响应头后 999ms 断开应算写响应头后立即断开: %+v", report)
	}
}

// TestCancelDiagnosticsServiceInitiated 客户端没有断开、请求以取消结束（管理端取消或服务停止）：仍记 499，断开现场标为服务端取消
func TestCancelDiagnosticsServiceInitiated(t *testing.T) {
	admin := &capturingAdmin{}
	service := &scriptedService{events: []aistudio.Event{{Kind: aistudio.EventError, Err: context.Canceled}}}
	handler := NewHandler(service, Config{APIKey: "sk-test", Admin: admin})
	postJSON(t, handler, "/v1beta/models/gemini-3.8-flash:generateContent", `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`, nil)
	entry := awaitFinished(t, admin)
	if entry.Status != 499 || entry.Disconnect == nil || entry.Disconnect.Initiator != "service" || !entry.Disconnect.HeadersSent {
		t.Fatalf("status=%d disconnect=%+v，期望 499 并标为服务端取消（错误响应已写出）", entry.Status, entry.Disconnect)
	}
	// 正常完成的请求没有断开现场
	admin = &capturingAdmin{}
	handler = NewHandler(&scriptedService{events: []aistudio.Event{{Kind: aistudio.EventText, Text: "ok"}, {Kind: aistudio.EventFinish}}}, Config{APIKey: "sk-test", Admin: admin})
	postJSON(t, handler, "/v1beta/models/gemini-3.8-flash:generateContent", `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`, nil)
	if entry := awaitFinished(t, admin); entry.Status != http.StatusOK || entry.Disconnect != nil {
		t.Fatalf("status=%d disconnect=%+v", entry.Status, entry.Disconnect)
	}
}

// panickingService 的 Generate 直接 panic
type panickingService struct {
	modelListService
}

func (panickingService) Generate(context.Context, aistudio.GenerateRequest) (<-chan aistudio.Event, error) {
	panic("测试用 panic")
}

// TestHandlerPanicRecovered 处理请求时 panic：进程不退出，响应头还没提交时按请求的协议返回官方结构的 500，请求日志记 500 与 panic 说明。
// 流式请求在 generateWithin 的 goroutine 里调用生成服务，那里的 panic 同样转成 500
func TestHandlerPanicRecovered(t *testing.T) {
	for _, test := range []struct {
		name, path, body, errorField string
	}{
		{name: "Gemini 非流式", path: "/v1beta/models/gemini-3.8-flash:generateContent", body: `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`, errorField: `"status":"INTERNAL"`},
		{name: "Gemini 流式", path: "/v1beta/models/gemini-3.8-flash:streamGenerateContent?alt=sse", body: `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`, errorField: `"status":"INTERNAL"`},
		{name: "OpenAI", path: "/v1/chat/completions", body: `{"model":"gemini-3.8-flash","messages":[{"role":"user","content":"hi"}]}`, errorField: `"type":"server_error"`},
	} {
		t.Run(test.name, func(t *testing.T) {
			admin := &capturingAdmin{}
			handler := NewHandler(panickingService{}, Config{APIKey: "sk-test", Admin: admin})
			recorder := postJSON(t, handler, test.path, test.body, nil)
			if recorder.Code != http.StatusInternalServerError || !strings.Contains(recorder.Body.String(), test.errorField) ||
				strings.Contains(recorder.Body.String(), "测试用 panic") {
				t.Fatalf("status=%d body=%s，期望官方结构的 500、不带 panic 原文", recorder.Code, recorder.Body.String())
			}
			entry := awaitFinished(t, admin)
			if entry.Status != http.StatusInternalServerError || !strings.Contains(entry.Error, "panic") || !strings.Contains(entry.Error, "测试用 panic") {
				t.Fatalf("请求日志 status=%d error=%q", entry.Status, entry.Error)
			}
		})
	}
}

// TestHandlerPanicAfterHeadersCommitted 响应头提交后 panic：请求日志记 500，随后中止连接（不写 chunked 结尾），客户端读到意外断开、
// 知道回复不完整；原先恢复后正常收尾，客户端读到干净的 EOF，把截断的流当成完整回复（Gemini SSE 没有结束标记，JSON 数组模式还会被
// 收尾的 "]" 补成合法 JSON）。处理函数主动中止连接（http.ErrAbortHandler）时写完请求日志后照旧向上 panic
func TestHandlerPanicAfterHeadersCommitted(t *testing.T) {
	for _, test := range []struct {
		name    string
		handler http.HandlerFunc
	}{
		{name: "SSE", handler: func(w http.ResponseWriter, r *http.Request) {
			_ = streamHeaders(w)
			_ = writeSSEText(w, "partial")
			panic(errors.New("写到一半出错"))
		}},
		{name: "Gemini JSON 数组", handler: func(w http.ResponseWriter, r *http.Request) {
			chunks, err := newGeminiChunkWriter(w, r, "resp_1", "m", true, false)
			if err != nil {
				return
			}
			defer chunks.close()
			_ = chunks.chunk(map[string]any{"responseId": "resp_1"})
			panic(errors.New("写到一半出错"))
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			admin := &capturingAdmin{}
			server := httptest.NewServer(requestLoggingMiddleware(admin, test.handler))
			defer server.Close()
			response, err := http.Post(server.URL+"/v1beta/models/m:streamGenerateContent", "application/json", nil)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			body, readErr := io.ReadAll(response.Body)
			if response.StatusCode != http.StatusOK || len(body) == 0 || readErr == nil {
				t.Fatalf("status=%d body=%q readErr=%v，期望已写出的数据之后连接被中止（读取返回错误）", response.StatusCode, body, readErr)
			}
			if entry := awaitFinished(t, admin); entry.Status != http.StatusInternalServerError || !strings.Contains(entry.Error, "写到一半出错") {
				t.Fatalf("请求日志 status=%d error=%q", entry.Status, entry.Error)
			}
		})
	}

	admin := &capturingAdmin{}
	aborting := requestLoggingMiddleware(admin, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic(http.ErrAbortHandler)
	}))
	func() {
		defer func() {
			if value := recover(); value != http.ErrAbortHandler {
				t.Fatalf("recover = %v，期望照旧向上 panic http.ErrAbortHandler", value)
			}
		}()
		aborting.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))
	}()
	if len(admin.finished()) != 1 {
		t.Fatal("中止连接前应写出请求日志")
	}
}

// TestCancelDiagnosticsDataWrite 距最近数据写出只算中转会转发的数据：换行与 ": ping" 心跳不算（中转丢掉它们），数据型心跳与内容块算。
// 关闭数据型心跳时一直只写换行心跳：距最近写出很短，距最近数据写出从最后一块内容算起
func TestCancelDiagnosticsDataWrite(t *testing.T) {
	for _, test := range []struct {
		data string
		want bool
	}{
		{"\n", false}, {": ping\n\n", false}, {"\r\n", false}, {"data: {}\n\n", true}, {"event: ping\ndata: {}\n\n", true},
		{": ping\ndata: x\n\n", true}, {"[", true}, {"\r\n]", true}, {"", false},
	} {
		if got := dataWrite([]byte(test.data)); got != test.want {
			t.Fatalf("dataWrite(%q) = %v，期望 %v", test.data, got, test.want)
		}
	}
	withHeartbeatInterval(t, 30*time.Millisecond)
	admin := &capturingAdmin{}
	service := &stallingService{before: []aistudio.Event{{Kind: aistudio.EventReasoning, Text: "思考中"}}}
	server := httptest.NewServer(NewHandler(service, Config{APIKey: "sk-test", Admin: admin, StreamDataHeartbeat: func() bool { return false }}))
	defer server.Close()
	response, cancel, err := startStream(t, server, "/v1beta/models/gemini-3.8-flash:streamGenerateContent?alt=sse",
		`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`)
	if err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(response.Body)
	if line, err := reader.ReadString('\n'); err != nil || !strings.HasPrefix(line, "data: ") {
		t.Fatalf("首行 = %q err=%v", line, err)
	}
	time.Sleep(300 * time.Millisecond)
	cancel()
	response.Body.Close()
	disconnect := awaitFinished(t, admin).Disconnect
	if disconnect == nil || disconnect.SinceLastDataWriteMS == nil {
		t.Fatalf("断开现场 = %+v，期望带距最近数据写出", disconnect)
	}
	if disconnect.SinceLastWriteMS > 150 || *disconnect.SinceLastDataWriteMS < 250 {
		t.Fatalf("换行心跳一直在写：距最近写出应很短（%.1fms），距最近数据写出应约 300ms（%.1fms）", disconnect.SinceLastWriteMS, *disconnect.SinceLastDataWriteMS)
	}
}

// TestGenerateWithinLateResult 生成服务到时还没返回时照常开始推流，之后返回的错误转成流内的错误事件，返回的事件流原样转发；
// 按时返回时原样返回（错误仍由调用方按 HTTP 状态返回）
func TestGenerateWithinLateResult(t *testing.T) {
	slow := &delayedService{delay: 150 * time.Millisecond, err: aistudio.ErrInvalidArgument}
	startedAt := time.Now()
	events, err := generateWithin(context.Background(), slow, aistudio.GenerateRequest{}, time.Now().Add(30*time.Millisecond))
	if err != nil || events == nil || time.Since(startedAt) > 120*time.Millisecond {
		t.Fatalf("到时应立即返回之后接上的事件流: events=%v err=%v 用时 %s", events, err, time.Since(startedAt))
	}
	event, ok := <-events
	if !ok || event.Kind != aistudio.EventError || !errors.Is(event.Err, aistudio.ErrInvalidArgument) {
		t.Fatalf("迟到的错误应转成错误事件: %+v ok=%v", event, ok)
	}
	if _, ok := <-events; ok {
		t.Fatal("错误事件之后事件流应关闭")
	}

	late := &delayedService{delay: 100 * time.Millisecond, events: []aistudio.Event{{Kind: aistudio.EventText, Text: "a"}, {Kind: aistudio.EventFinish}}}
	events, err = generateWithin(context.Background(), late, aistudio.GenerateRequest{}, time.Now().Add(20*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	var kinds []aistudio.EventKind
	for event := range events {
		kinds = append(kinds, event.Kind)
	}
	if len(kinds) != 2 || kinds[0] != aistudio.EventText || kinds[1] != aistudio.EventFinish {
		t.Fatalf("迟到的事件流应原样转发: %v", kinds)
	}

	quick := &delayedService{err: aistudio.ErrInvalidArgument}
	if _, err := generateWithin(context.Background(), quick, aistudio.GenerateRequest{}, time.Now().Add(time.Second)); !errors.Is(err, aistudio.ErrInvalidArgument) {
		t.Fatalf("按时返回的错误应原样返回: %v", err)
	}
}

// delayedService 等 delay 后返回 err 或 events
type delayedService struct {
	modelListService
	delay  time.Duration
	err    error
	events []aistudio.Event
}

func (service *delayedService) Generate(ctx context.Context, _ aistudio.GenerateRequest) (<-chan aistudio.Event, error) {
	select {
	case <-time.After(service.delay):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if service.err != nil {
		return nil, service.err
	}
	events := make(chan aistudio.Event, len(service.events))
	for _, event := range service.events {
		events <- event
	}
	close(events)
	return events, nil
}

// TestStreamHoldCappedWhenGenerateBlocks 生成服务一直不返回（降级判定严格模式在判定出结果前不返回）时，扣住响应头最多到上限，
// 之后照常提交响应头开始推流；原先要等生成服务返回后才开始计时
func TestStreamHoldCappedWhenGenerateBlocks(t *testing.T) {
	blocking := &delayedService{delay: time.Minute}
	s := &server{service: blocking}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startedAt := time.Now()
	events, err := generateWithin(ctx, s.service, aistudio.GenerateRequest{Stream: true}, startedAt.Add(80*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	stream, _, err := awaitStreamStartSince(ctx, events, 40*time.Millisecond, 80*time.Millisecond, startedAt)
	elapsed := time.Since(startedAt)
	if err != nil || stream == nil || elapsed < 70*time.Millisecond || elapsed > time.Second {
		t.Fatalf("stream=%v err=%v 用时 %s，期望约 80ms 时开始推流", stream, err, elapsed)
	}
}
