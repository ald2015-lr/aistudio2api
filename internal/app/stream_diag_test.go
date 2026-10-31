package app

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/api"
	"github.com/Mag1cFall/AIStudio2API/internal/config"
)

// TestStreamDataHeartbeatHotConfig 数据型心跳保存后立即生效、不提示需要重启：写进 .env，读取时总是返回；旧版页面不带该字段时沿用现值
func TestStreamDataHeartbeatHotConfig(t *testing.T) {
	t.Setenv("STREAM_DATA_HEARTBEAT", "")
	os.Unsetenv("STREAM_DATA_HEARTBEAT")
	manager, path := newMediaHotManager(t)
	manager.streamDataHeartbeat.Store(true)
	ctx := context.Background()
	if !manager.activeStreamDataHeartbeat() {
		t.Fatal("默认应开启")
	}
	value := runtimeConfigDTO(config.Default())
	if value.StreamDataHeartbeat == nil || !*value.StreamDataHeartbeat {
		t.Fatalf("读取的默认值 = %v", value.StreamDataHeartbeat)
	}
	off := false
	value.StreamDataHeartbeat = &off
	updated, err := manager.UpdateRuntimeConfig(ctx, value)
	if err != nil {
		t.Fatal(err)
	}
	if updated.StreamDataHeartbeat == nil || *updated.StreamDataHeartbeat || updated.ServiceRestartRequired || updated.ManagementRestartRequired {
		t.Fatalf("保存后返回 = %v，需要重启 = %v/%v", updated.StreamDataHeartbeat, updated.ServiceRestartRequired, updated.ManagementRestartRequired)
	}
	if manager.activeStreamDataHeartbeat() {
		t.Fatal("保存后应立即生效")
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "STREAM_DATA_HEARTBEAT=false\n") {
		t.Fatalf(".env 缺少 STREAM_DATA_HEARTBEAT=false:\n%s", data)
	}
	if !hasLogContaining(manager.requests, "数据型心跳设置已更新并立即生效", "开启=false") {
		t.Fatal("缺少数据型心跳生效日志")
	}
	read, err := manager.RuntimeConfig(ctx)
	if err != nil || read.StreamDataHeartbeat == nil || *read.StreamDataHeartbeat || read.ServiceRestartRequired {
		t.Fatalf("读取配置: %v %v", read.StreamDataHeartbeat, err)
	}
	// 旧版页面不带该字段：沿用现值
	legacy := read
	legacy.StreamDataHeartbeat = nil
	if _, err := manager.UpdateRuntimeConfig(ctx, legacy); err != nil {
		t.Fatal(err)
	}
	if manager.activeStreamDataHeartbeat() {
		t.Fatal("旧版页面保存后应沿用现值（关闭）")
	}
	on := true
	read.StreamDataHeartbeat = &on
	if _, err := manager.UpdateRuntimeConfig(ctx, read); err != nil || !manager.activeStreamDataHeartbeat() {
		t.Fatalf("重新开启应立即生效: %v", err)
	}
}

// TestCancelledRequestLogShowsDisconnect 取消的请求在 request.finished 的摘要后附上断开现场，载荷里带 disconnect 字段；其余请求不变
func TestCancelledRequestLogShowsDisconnect(t *testing.T) {
	quiet, data, headers := 31200.0, 29800.0, 41000.0
	entry := api.AccessLog{
		Status: 499, Canceled: true, Latency: 42 * time.Second, RequestID: "resp_1",
		Disconnect: &api.RequestDisconnect{
			Initiator: "client", Phase: "mid_stream", HeadersSent: true, WrittenBytes: 1536, WrittenChunks: 4,
			SinceLastWriteMS: 9800, SinceLastDataWriteMS: &data, SinceHeadersSentMS: &headers, SinceUpstreamEventMS: &quiet, ElapsedMS: 42000,
		},
	}
	logged, level, message := finishedRequestLog(entry)
	want := "请求已取消 | 客户端断开 | 阶段=流中 | 响应头=已提交（断开前 41s） | 已写出=4 块 1.5 KB | 距最近写出=9.8s | 距最近数据=29.8s | 距上游最近事件=31.2s | 已用时=42s"
	if logged.State != "cancelled" || level != "WARN" || message != want {
		t.Fatalf("state=%s level=%s message=%q\n期望 %q", logged.State, level, message, want)
	}
	encoded, _ := json.Marshal(logged)
	if !strings.Contains(string(encoded), `"disconnect":{"initiator":"client","phase":"mid_stream","headers_sent":true,"written_bytes":1536,"written_chunks":4,"since_last_write_ms":9800,"since_last_data_write_ms":29800,"since_headers_sent_ms":41000,"since_upstream_event_ms":31200,"elapsed_ms":42000}`) {
		t.Fatalf("请求日志载荷缺少断开现场: %s", encoded)
	}
	// 写响应头后 1 秒内断开：经 new-api 时首个事件早到、响应头随它提交，阶段为“流中”，摘要末尾另附一句
	right, zero := 120.0, 0.0
	entry.Disconnect = &api.RequestDisconnect{
		Initiator: "client", Phase: "mid_stream", HeadersSent: true, WrittenBytes: 210, WrittenChunks: 1,
		SinceLastWriteMS: 120, SinceLastDataWriteMS: &right, SinceHeadersSentMS: &right, SinceUpstreamEventMS: &zero, ElapsedMS: 3400, RightAfterHeaders: true,
	}
	if logged, _, message := finishedRequestLog(entry); message != "请求已取消 | 客户端断开 | 阶段=流中 | 响应头=已提交（断开前 100ms） | 已写出=1 块 210 B | 距最近写出=100ms | 距最近数据=100ms | 距上游最近事件=0s | 已用时=3.4s | 写响应头后立即断开（经中转时多为下游在响应头之前已断开）" {
		t.Fatalf("写响应头后立即断开的摘要 = %q", message)
	} else if encoded, _ := json.Marshal(logged); !strings.Contains(string(encoded), `"right_after_headers":true`) {
		t.Fatalf("载荷缺少 right_after_headers: %s", encoded)
	}
	entry.Disconnect = &api.RequestDisconnect{Initiator: "service", Phase: "before_first_event", SinceLastWriteMS: 500, ElapsedMS: 500}
	if _, _, message := finishedRequestLog(entry); message != "请求已取消 | 服务端取消（管理端取消请求或服务停止、重启） | 阶段=首事件前 | 响应头=未提交 | 已写出=0 块 0 B | 距最近写出=500ms | 距最近数据=无 | 距上游最近事件=无 | 已用时=500ms" {
		t.Fatalf("服务端取消的摘要 = %q", message)
	}
	if _, _, message := finishedRequestLog(api.AccessLog{Status: http.StatusOK}); message != "请求完成" {
		t.Fatalf("正常完成的摘要 = %q", message)
	}
}

// scriptedUpstream 每次生成返回 events；panicOnGenerate 为真时 Generate 直接 panic
type scriptedUpstream struct {
	events          []aistudio.Event
	panicOnGenerate bool
}

func (upstream *scriptedUpstream) Models(context.Context) ([]aistudio.Model, error) { return nil, nil }

func (upstream *scriptedUpstream) CountTokens(context.Context, aistudio.TokenCountRequest) (aistudio.TokenCount, error) {
	return aistudio.TokenCount{}, nil
}

func (upstream *scriptedUpstream) Generate(context.Context, aistudio.GenerateRequest) (<-chan aistudio.Event, error) {
	if upstream.panicOnGenerate {
		panic("上游调用 panic")
	}
	events := make(chan aistudio.Event, len(upstream.events))
	for _, event := range upstream.events {
		event.ProviderModel = testRuntimeModel
		events <- event
	}
	close(events)
	return events, nil
}

// TestGenerateWithRetryPanicRecovered 换号重试中 panic（上游调用 panic）：进程不退出，本请求以 500 的错误事件结束、状态 failed，
// 事件流关闭，租约释放
func TestGenerateWithRetryPanicRecovered(t *testing.T) {
	service, pool := firstEventTestService(t, &scriptedUpstream{panicOnGenerate: true}, 0, "alice@example.com")
	states := watchRequestStates(t, service)
	request := deadlineRequest("req-retry-panic")
	result := runDeadlineRequest(t, service, states, request, newGenerateContext(service.lifecycle, time.Now(), 10*time.Second, time.Hour))
	var recovered *generationPanicError
	if !errors.As(result.err, &recovered) || recovered.where != "生成重试" || recovered.HTTPStatus() != http.StatusInternalServerError {
		t.Fatalf("err = %v，期望恢复后的 panic 错误（500）", result.err)
	}
	if result.state != "failed" || result.errEvents != 1 {
		t.Fatalf("state = %s，错误事件 %d 个，期望 failed 与 1 个错误事件", result.state, result.errEvents)
	}
	assertLeasesReleased(t, pool)
	if queueProgressCount(service, "ERROR", "生成重试时发生 panic，已恢复并结束本请求 | 错误=上游调用 panic") != 1 {
		t.Fatal("缺少 panic 的请求进度日志")
	}
}

// TestForwardEventsPanicRecovered 转发上游事件时 panic（交付结束事件后标记模型资格时访问账户池）：已转发的事件保留，随后以 500 的错误事件
// 结束本请求，事件流关闭，租约照常释放
func TestForwardEventsPanicRecovered(t *testing.T) {
	service, pool := firstEventTestService(t, &pacedService{}, 0, "alice@example.com")
	lease, err := pool.AcquireFor(context.Background(), aistudio.AccountSelection{AccountID: "alice@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	gc := newGenerateContext(service.lifecycle, time.Now(), time.Minute, 10*time.Second)
	if !gc.beginStreaming() {
		t.Fatal("beginStreaming 应成功")
	}
	// 交付结束事件后标记模型资格时访问 service.pool：置空让转发在这一步 panic（租约自己持有账户池，释放不受影响）
	service.pool = nil
	source := make(chan aistudio.Event, 1)
	source <- aistudio.Event{Kind: aistudio.EventFinish, FinishReason: "stop", ProviderModel: testRuntimeModel}
	close(source)
	destination := make(chan aistudio.Event, 8)
	request := deadlineRequest("req-forward-panic")
	done := make(chan struct{})
	go func() {
		defer close(done)
		service.forwardEvents(service.lifecycle, gc, gc.cancel, request.ID,
			aistudio.Event{Kind: aistudio.EventText, Text: "部分正文", ProviderModel: testRuntimeModel},
			source, destination, lease, nil, &upstreamActivity{}, testRuntimeModel, newGenerationDiagnostics(request, "未发送"))
	}()
	events, err := collectEvents(t, destination, 5*time.Second)
	<-done
	var recovered *generationPanicError
	if len(events) != 3 || events[0].Text != "部分正文" || events[1].Kind != aistudio.EventFinish || !errors.As(err, &recovered) ||
		recovered.where != "转发上游事件" {
		t.Fatalf("events=%v err=%v，期望保留已转发的事件并以恢复后的 panic 错误结束", events, err)
	}
	assertLeasesReleased(t, pool)
}

// TestFinishReasonAdviceLogged 上游以缺少思考签名、工具调用过多结束时，请求进度写一行 WARN 说明原因与建议；正常结束不写
func TestFinishReasonAdviceLogged(t *testing.T) {
	for reason, fragment := range map[string]string{
		"missing_thought_signature": "上游以缺少思考签名结束（MISSING_THOUGHT_SIGNATURE） | 原因=",
		"too_many_tool_calls":       "上游以工具调用过多结束（TOO_MANY_TOOL_CALLS） | 原因=",
	} {
		upstream := &scriptedUpstream{events: []aistudio.Event{
			{Kind: aistudio.EventText, Text: "x"}, {Kind: aistudio.EventFinish, FinishReason: reason},
		}}
		service, _ := firstEventTestService(t, upstream, 0, "alice@example.com")
		if _, err := runFirstEventRequest(t, service); err != nil {
			t.Fatal(err)
		}
		lines := queueProgressLines(service, "req-first-event", fragment)
		if len(lines) != 1 || lines[0].Level != "WARN" || !strings.Contains(lines[0].Message, "| 建议=") {
			t.Fatalf("%s: 进度日志 = %+v", reason, lines)
		}
	}
	if finishReasonAdvice("stop") != "" || finishReasonAdvice("malformed_function_call") != "" {
		t.Fatal("其他结束原因不应写这一行")
	}
}

// TestForwardEventsLogsDownstreamDisconnect 下游（客户端或协议层）在终态之前结束时，请求进度写一行“下游已断开”，带距上游最近事件的时长
func TestForwardEventsLogsDownstreamDisconnect(t *testing.T) {
	upstream := &pacedService{script: pacedScript{interval: 50 * time.Millisecond, reasoning: -1}}
	service, _ := firstEventTestService(t, upstream, 0, "alice@example.com")
	// 与 startGenerate 相同：请求上下文从客户端上下文派生，客户端断开时两者都取消
	client, disconnect := context.WithCancel(service.lifecycle)
	gc := newGenerateContext(client, time.Now(), 10*time.Second, time.Hour)
	request := deadlineRequest("req-downstream-gone")
	service.requests.start(client, request, gc.cancel)
	events := make(chan aistudio.Event, 8)
	done := make(chan struct{})
	go func() {
		defer close(done)
		service.generateWithRetry(client, gc, gc.cancel, time.Now(), request, "", events, newGenerationDiagnostics(request, "未发送"))
	}()
	select {
	case <-events:
	case <-time.After(5 * time.Second):
		t.Fatal("没有收到首个事件")
	}
	time.Sleep(120 * time.Millisecond)
	disconnect()
	go drainEventsForTest(events)
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("请求没有结束")
	}
	if !waitUntil(5*time.Second, func() bool {
		return len(queueProgressLines(service, request.ID, "下游已断开，结束转发 | 阶段=流中")) == 1
	}) {
		t.Fatal("缺少下游断开的进度日志")
	}
	line := queueProgressLines(service, request.ID, "下游已断开")[0].Message
	if strings.Contains(line, "距上游最近事件=无") || !strings.Contains(line, "| 推理=") {
		t.Fatalf("进度日志应带距上游最近事件的时长与已转发的事件数: %s", line)
	}
}

func drainEventsForTest(events <-chan aistudio.Event) {
	for range events {
	}
}

// withPanicHook 在测试期间设置 hook：requestID 为 target 时 panic
func withPanicHook(t *testing.T, hook *atomic.Pointer[func(string)], target string, value string) {
	t.Helper()
	fn := func(requestID string) {
		if requestID == target {
			panic(value)
		}
	}
	hook.Store(&fn)
	t.Cleanup(func() { hook.Store(nil) })
}

// TestForwardEventsProloguePanicCleansUp 转发前置步骤（取账号、接上降级判定、核对服务模型）里 panic：清理与恢复已最先注册，租约照常释放、
// 事件流关闭、请求以 500 的错误事件结束（状态 failed）。原先清理注册在前置步骤之后，generateWithRetry 看到已交给转发就直接返回，
// 租约永久占用、协议层一直等
func TestForwardEventsProloguePanicCleansUp(t *testing.T) {
	upstream := &scriptedUpstream{events: []aistudio.Event{{Kind: aistudio.EventText, Text: "x"}, {Kind: aistudio.EventFinish, FinishReason: "stop"}}}
	service, pool := firstEventTestService(t, upstream, 0, "alice@example.com")
	states := watchRequestStates(t, service)
	request := deadlineRequest("req-forward-prologue-panic")
	withPanicHook(t, &forwardEventsPrologueHook, request.ID, "前置步骤 panic")
	result := runDeadlineRequest(t, service, states, request, newGenerateContext(service.lifecycle, time.Now(), 10*time.Second, time.Hour))
	var recovered *generationPanicError
	if !errors.As(result.err, &recovered) || recovered.where != "转发上游事件" || recovered.HTTPStatus() != http.StatusInternalServerError {
		t.Fatalf("err = %v，期望转发时恢复的 panic 错误（500）", result.err)
	}
	if result.state != "failed" || result.errEvents != 1 || len(result.events) != 1 {
		t.Fatalf("state=%s 错误事件 %d 个、共 %d 个事件，期望 failed、只有 1 个错误事件（没有转发内容）", result.state, result.errEvents, len(result.events))
	}
	assertLeasesReleased(t, pool)
	if queueProgressCount(service, "ERROR", "转发上游事件时发生 panic，已恢复并结束本请求 | 错误=前置步骤 panic") != 1 {
		t.Fatal("缺少 panic 的请求进度日志")
	}
}

// TestStartGeneratePanicFinishesRequest 登记请求之后、交给 generateWithRetry 之前 panic（文件资源刷新等）：Generate 返回 500 的恢复错误，
// 请求登记按失败结束，不留下一条一直“进行中”的请求
func TestStartGeneratePanicFinishesRequest(t *testing.T) {
	service, pool := runningGenerateService(t, &scriptedUpstream{}, context.Background(), time.Minute, "alice@example.com")
	states := watchRequestStates(t, service)
	request := deadlineRequest("req-start-panic")
	withPanicHook(t, &startGeneratePanicHook, request.ID, "开始生成 panic")
	events, err := service.Generate(context.Background(), request)
	var recovered *generationPanicError
	if events != nil || !errors.As(err, &recovered) || recovered.where != "开始生成" || recovered.HTTPStatus() != http.StatusInternalServerError {
		t.Fatalf("events=%v err=%v，期望恢复后的 panic 错误（500）", events, err)
	}
	if state := states.await(t, request.ID); state != "failed" {
		t.Fatalf("请求登记状态 = %s，期望 failed", state)
	}
	assertLeasesReleased(t, pool)
}

// TestDowngradeLoopPanicRecovered 降级判定 goroutine 里 panic（判定设置为空，缓存第一个事件时就 panic）：严格模式还没放行时 wait 返回 500 的
// 恢复错误；快速模式已转发思考时往流里写错误事件。两种都取消上游、读完它的事件流并关闭输出流，进程不退出
func TestDowngradeLoopPanicRecovered(t *testing.T) {
	for _, mode := range []string{gateModeStrict, gateModeFast} {
		t.Run(mode, func(t *testing.T) {
			var once sync.Once
			canceled := make(chan struct{})
			gate := &downgradeGate{requestID: "req-guard-panic", mode: mode, clientCtx: context.Background()}
			upstream := make(chan aistudio.Event, 4)
			upstream <- aistudio.Event{Kind: aistudio.EventReasoning, Text: "想"}
			upstream <- aistudio.Event{Kind: aistudio.EventText, Text: "正文"}
			out, wait := gate.start(context.Background(), func() { once.Do(func() { close(canceled) }) }, upstream)
			var recovered *generationPanicError
			if mode == gateModeStrict {
				if err := wait(); !errors.As(err, &recovered) || recovered.where != "降级判定" || recovered.HTTPStatus() != http.StatusInternalServerError {
					t.Fatalf("wait() = %v，期望恢复后的 panic 错误（500）", err)
				}
			}
			select {
			case <-canceled:
			case <-time.After(5 * time.Second):
				t.Fatal("恢复后应取消上游")
			}
			close(upstream)
			var kinds []aistudio.EventKind
			for event := range out {
				kinds = append(kinds, event.Kind)
				if event.Kind == aistudio.EventError && (!errors.As(event.Err, &recovered) || recovered.where != "降级判定") {
					t.Fatalf("错误事件 = %v", event.Err)
				}
			}
			if mode == gateModeFast && (len(kinds) != 2 || kinds[0] != aistudio.EventReasoning || kinds[1] != aistudio.EventError) {
				t.Fatalf("快速模式输出 = %v，期望已转发的思考之后是错误事件", kinds)
			}
			if mode == gateModeStrict && len(kinds) != 0 {
				t.Fatalf("严格模式还没放行时不应往流里写事件: %v", kinds)
			}
		})
	}
}

// TestForwardEventsMarksUpstreamEvent 完整服务流程：转发每个上游事件时记下时刻（MarkAccessLogUpstreamEvent），客户端断开时断开现场带
// “距上游最近事件”；去掉 forwardEvents 里这一行时它会一直缺失
func TestForwardEventsMarksUpstreamEvent(t *testing.T) {
	upstream := &lingeringService{events: []aistudio.Event{{Kind: aistudio.EventReasoning, Text: "思考中"}}, closeDelay: 10 * time.Millisecond, exited: make(chan struct{})}
	service, pool := runningGenerateService(t, upstream, context.Background(), time.Minute, "alice@example.com")
	admin := &accessLogCapture{}
	server := httptest.NewServer(api.NewHandler(service, api.Config{APIKey: "sk-test", Admin: admin}))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	request, _ := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/v1beta/models/"+testRuntimeModel+":streamGenerateContent?alt=sse",
		strings.NewReader(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`))
	request.Header.Set("Authorization", "Bearer sk-test")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(response.Body)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("读取失败: %v", err)
		}
		if strings.HasPrefix(line, "data: ") {
			break
		}
	}
	time.Sleep(200 * time.Millisecond)
	cancel()
	response.Body.Close()
	var entry api.AccessLog
	if !waitUntil(5*time.Second, func() bool {
		admin.mu.Lock()
		defer admin.mu.Unlock()
		if len(admin.entries) == 0 {
			return false
		}
		entry = admin.entries[0]
		return true
	}) {
		t.Fatal("没有写出访问日志")
	}
	disconnect := entry.Disconnect
	if entry.Status != 499 || disconnect == nil || disconnect.Phase != "mid_stream" || disconnect.SinceUpstreamEventMS == nil || *disconnect.SinceUpstreamEventMS < 150 {
		t.Fatalf("status=%d disconnect=%+v，期望流中断开并带约 200ms 的距上游最近事件", entry.Status, disconnect)
	}
	select {
	case <-upstream.exited:
	case <-time.After(5 * time.Second):
		t.Fatal("上游没有退出")
	}
	if !waitUntil(5*time.Second, func() bool { return pool.Status()[0].State == aistudio.AccountReady }) {
		t.Fatal("租约没有释放")
	}
}
