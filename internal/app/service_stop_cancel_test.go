package app

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/api"
)

// realStopUpstream 为经真实 Stop() 停止生成服务的测试上游：生成与转写都一直等到本次尝试的上下文结束、按它的错误返回
// （与真实上游被取消时一样）；streaming 为真时生成先交出一条正文再等。开始等待时把调用名写进 started
type realStopUpstream struct {
	fallbackUpstream
	started   chan string
	streaming bool
	generates atomic.Int32
}

func (upstream *realStopUpstream) Generate(ctx context.Context, _ aistudio.GenerateRequest) (<-chan aistudio.Event, error) {
	upstream.generates.Add(1)
	if lease, ok := aistudio.AccountLeaseFromContext(ctx); ok {
		lease.RecordGenerationSent()
	}
	if !upstream.streaming {
		upstream.started <- "Generate"
		<-ctx.Done()
		return nil, ctx.Err()
	}
	events := make(chan aistudio.Event)
	go func() {
		defer close(events)
		select {
		case events <- aistudio.Event{Kind: aistudio.EventText, Text: "hello", ProviderModel: testRuntimeModel}:
		case <-ctx.Done():
			return
		}
		upstream.started <- "Generate"
		<-ctx.Done()
		select {
		case events <- aistudio.Event{Kind: aistudio.EventError, Err: ctx.Err()}:
		case <-time.After(time.Second):
		}
	}()
	return events, nil
}

func (upstream *realStopUpstream) Transcribe(ctx context.Context, _ aistudio.TranscriptionRequest) (aistudio.TranscriptionResult, error) {
	upstream.started <- "Transcribe"
	<-ctx.Done()
	return aistudio.TranscriptionResult{}, ctx.Err()
}

// realStopHTTP 经协议层发一次 OpenAI 对话请求（不流式），返回响应记录、访问日志与请求结束的通知
func realStopHTTP(service *trackedService) (*httptest.ResponseRecorder, *accessLogCapture, <-chan struct{}) {
	admin := &accessLogCapture{}
	handler := api.NewHandler(service, api.Config{APIKey: "sk-test", Admin: admin})
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"`+testRuntimeModel+`","messages":[{"role":"user","content":"hi"}]}`))
	request.Header.Set("Authorization", "Bearer sk-test")
	recorder := httptest.NewRecorder()
	served := make(chan struct{})
	go func() {
		defer close(served)
		handler.ServeHTTP(recorder, request)
	}()
	return recorder, admin, served
}

// activeRequestID 等到请求登记表里恰好有一个请求，返回它的编号
func activeRequestID(t *testing.T, service *trackedService) string {
	t.Helper()
	var id string
	if !waitUntil(5*time.Second, func() bool {
		active := service.requests.list()
		if len(active) == 1 {
			id = active[0].ID
		}
		return id != ""
	}) {
		t.Fatalf("请求没有登记：%+v", service.requests.list())
	}
	return id
}

// assertServiceStoppedHTTP 断言请求经协议层以 503 service_stopped 结束：访问日志不是取消（没有断开现场），请求登记为失败
func assertServiceStoppedHTTP(t *testing.T, recorder *httptest.ResponseRecorder, admin *accessLogCapture, served <-chan struct{}, states *requestStates, id string) {
	t.Helper()
	select {
	case <-served:
	case <-time.After(5 * time.Second):
		t.Fatal("停止服务后请求没有结束")
	}
	// 对客户端两者都是官方的 503 不可用文案（OpenAI code 为 null），不透出内部原因；区别在访问日志与请求登记
	if body := recorder.Body.String(); recorder.Code != http.StatusServiceUnavailable || !strings.Contains(body, "The service is currently unavailable.") ||
		strings.Contains(body, "服务已停止") {
		t.Fatalf("status=%d body=%s，期望官方的 503 不可用文案", recorder.Code, body)
	}
	if state := states.await(t, id); state != "failed" {
		t.Fatalf("请求登记为 %s，期望按服务停止记为 failed（不是 cancelled）", state)
	}
	admin.mu.Lock()
	entries := append([]api.AccessLog(nil), admin.entries...)
	admin.mu.Unlock()
	if len(entries) != 1 {
		t.Fatalf("访问日志条数 = %d，期望 1", len(entries))
	}
	// 访问日志：503 service_stopped（serviceStoppedError），不是取消（499、request_canceled、“服务端取消”的断开现场）
	if entry := entries[0]; entry.Status != http.StatusServiceUnavailable || entry.Canceled || entry.Disconnect != nil ||
		entry.Error != (&serviceStoppedError{}).Error() {
		t.Fatalf("访问日志 status=%d canceled=%t error=%q disconnect=%+v，期望 503 服务已停止、不是取消",
			entry.Status, entry.Canceled, entry.Error, entry.Disconnect)
	}
}

// assertServiceStoppedError 断言 err 是 serviceStoppedError（503，错误码 service_stopped），不是取消
func assertServiceStoppedError(t *testing.T, err error) {
	t.Helper()
	var stopped *serviceStoppedError
	if !errors.As(err, &stopped) || errors.Is(err, context.Canceled) || finalRequestState(err) != "failed" ||
		stopped.HTTPStatus() != http.StatusServiceUnavailable || stopped.ErrorCode() != "service_stopped" {
		t.Fatalf("err = %v，期望 serviceStoppedError（503 service_stopped，失败，不是取消）", err)
	}
}

// stopEnv 经真实的 trackedService.Stop() 停止生成服务（与管理页停止、配置保存重建、进程内重启同一条路径：
// Stop 先 dataCancel()，后台的 finishStop 再 requests.cancelAll()）
func stopEnv(t *testing.T, env *firstWarmEnv) {
	t.Helper()
	if stopped, err := env.service.Stop(); err != nil || !stopped {
		t.Fatalf("Stop = (%t, %v)，期望停止成功", stopped, err)
	}
	if state := env.service.State(); state != "STOPPED" {
		t.Fatalf("Stop 返回后状态为 %s，期望 STOPPED", state)
	}
}

// TestRealStopEndsRequestsAsServiceStopped 经真实 Stop()（不是直接取消数据上下文）停止生成服务时，进行中的请求以 503 service_stopped
// 结束、请求登记为失败：排队等首轮预热第一个 Worker 的生成请求、上游首事件前与流中的生成请求、转写请求。原先 Stop 先 dataCancel()、
// finishStop 的 requests.cancelAll() 几乎总是赶在服务停止的回调之前，按普通取消结束它们（cancelled、499/503 request_canceled、
// 访问日志“服务端取消”）。每个场景重复多次：两者谁先到原先由调度决定，修复后与次序无关
func TestRealStopEndsRequestsAsServiceStopped(t *testing.T) {
	const rounds = 5
	t.Run("等首轮预热", func(t *testing.T) {
		for range rounds {
			env := newFirstWarmEnv(t, []string{partitionNormalA, partitionNormalB}, nil, 1, 2, 0, 1)
			release := env.gate(partitionNormalA, partitionNormalB)
			env.start(t)
			env.waitLaunched(t, 1)
			states := watchRequestStates(t, env.service)
			recorder, admin, served := realStopHTTP(env.service)
			waitQueued(t, env.manager)
			id := activeRequestID(t, env.service)
			stopEnv(t, env)
			assertServiceStoppedHTTP(t, recorder, admin, served, states, id)
			release()
			assertLeasesReleased(t, env.pool)
		}
	})
	for _, streaming := range []bool{false, true} {
		t.Run(map[bool]string{false: "首事件前", true: "流中"}[streaming], func(t *testing.T) {
			for range rounds {
				env := newFirstWarmEnv(t, []string{partitionNormalA}, nil, 1, 1, 0, 1)
				upstream := &realStopUpstream{started: make(chan string, 4), streaming: streaming}
				env.service.service = upstream
				env.start(t)
				if !waitUntil(5*time.Second, func() bool { return len(env.manager.WarmAccountIDs()) == 1 }) {
					t.Fatal("首轮预热没有就绪的 Worker")
				}
				states := watchRequestStates(t, env.service)
				var recorder *httptest.ResponseRecorder
				var admin *accessLogCapture
				var served <-chan struct{}
				var events <-chan aistudio.Event
				if streaming {
					// 流中：直接读事件流（协议层不流式时会等整段回复），先收到正文
					var err error
					events, err = env.service.Generate(aistudio.ContextWithPoolScope(env.ctx, aistudio.PoolScopeNormal), deadlineRequest("req-real-stop-stream"))
					if err != nil {
						t.Fatal(err)
					}
					if event := <-events; event.Kind != aistudio.EventText {
						t.Fatalf("首个事件 = %+v，期望正文", event)
					}
				} else {
					recorder, admin, served = realStopHTTP(env.service)
				}
				select {
				case <-upstream.started:
				case <-time.After(5 * time.Second):
					t.Fatal("上游没有收到请求")
				}
				id := activeRequestID(t, env.service)
				stopEnv(t, env)
				if !streaming {
					assertServiceStoppedHTTP(t, recorder, admin, served, states, id)
				} else {
					var last error
					for event := range events {
						if event.Kind == aistudio.EventError {
							last = event.Err
						}
					}
					assertServiceStoppedError(t, last)
					if state := states.await(t, id); state != "failed" {
						t.Fatalf("请求登记为 %s，期望按服务停止记为 failed", state)
					}
				}
				if calls := upstream.generates.Load(); calls != 1 {
					t.Fatalf("服务停止不应换号重试：上游收到 %d 次", calls)
				}
				assertLeasesReleased(t, env.pool)
			}
		})
	}
	t.Run("转写", func(t *testing.T) {
		for range rounds {
			env := newFirstWarmEnv(t, []string{partitionNormalA}, nil, 1, 1, 0, 1)
			upstream := &realStopUpstream{started: make(chan string, 4)}
			env.service.service = upstream
			env.start(t)
			if !waitUntil(5*time.Second, func() bool { return len(env.manager.WarmAccountIDs()) == 1 }) {
				t.Fatal("首轮预热没有就绪的 Worker")
			}
			states := watchRequestStates(t, env.service)
			done := make(chan error, 1)
			go func() {
				_, err := env.service.Transcribe(aistudio.ContextWithPoolScope(env.ctx, aistudio.PoolScopeNormal), aistudio.TranscriptionRequest{
					ID: "req-real-stop-transcribe", Model: testRuntimeModel, Name: "a.wav", MIME: "audio/wav", Size: 4, Reader: strings.NewReader("RIFF"),
				})
				done <- err
			}()
			select {
			case <-upstream.started:
			case err := <-done:
				t.Fatalf("上游调用之前就结束了: %v", err)
			case <-time.After(5 * time.Second):
				t.Fatal("上游没有收到转写请求")
			}
			stopEnv(t, env)
			var err error
			select {
			case err = <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("停止服务后转写没有结束")
			}
			assertServiceStoppedError(t, err)
			if state := states.await(t, "req-real-stop-transcribe"); state != "failed" {
				t.Fatalf("转写请求登记为 %s，期望 failed", state)
			}
			assertLeasesReleased(t, env.pool)
		}
	})
}

// TestRealStopKeepsOtherOutcomes 经真实 Stop() 的其他结局不变：服务运行中管理端单独取消的请求仍记为取消，之后停止服务不改它；
// 结束事件已经交给客户端、只在等上游事件流关闭时停止服务，仍按终态记为完成（200，没有错误）
func TestRealStopKeepsOtherOutcomes(t *testing.T) {
	t.Run("运行中管理端取消", func(t *testing.T) {
		env := newFirstWarmEnv(t, []string{partitionNormalA, partitionNormalB}, nil, 1, 2, 0, 1)
		release := env.gate(partitionNormalA, partitionNormalB)
		defer release()
		env.start(t)
		env.waitLaunched(t, 1)
		states := watchRequestStates(t, env.service)
		events, err := env.service.Generate(aistudio.ContextWithPoolScope(env.ctx, aistudio.PoolScopeNormal), deadlineRequest("req-admin-cancel"))
		if err != nil {
			t.Fatal(err)
		}
		waitQueued(t, env.manager)
		if err := env.service.requests.cancel("req-admin-cancel"); err != nil {
			t.Fatal(err)
		}
		var last error
		for event := range events {
			if event.Kind == aistudio.EventError {
				last = event.Err
			}
		}
		var stopped *serviceStoppedError
		if errors.As(last, &stopped) || !errors.Is(last, context.Canceled) {
			t.Fatalf("管理端取消的错误 = %v，期望取消", last)
		}
		if state := states.await(t, "req-admin-cancel"); state != "cancelled" {
			t.Fatalf("请求登记为 %s，期望 cancelled", state)
		}
		stopEnv(t, env)
		states.mu.Lock()
		state := states.states["req-admin-cancel"]
		states.mu.Unlock()
		if state != "cancelled" {
			t.Fatalf("停止服务后请求登记变为 %s，期望仍是 cancelled", state)
		}
	})
	t.Run("终态已交出", func(t *testing.T) {
		env := newFirstWarmEnv(t, []string{partitionNormalA}, nil, 1, 1, 0, 1)
		upstream := &afterTerminalUpstream{delivered: make(chan struct{}), exited: make(chan struct{})}
		env.service.service = upstream
		env.start(t)
		if !waitUntil(5*time.Second, func() bool { return len(env.manager.WarmAccountIDs()) == 1 }) {
			t.Fatal("首轮预热没有就绪的 Worker")
		}
		states := watchRequestStates(t, env.service)
		admin := &accessLogCapture{}
		handler := api.NewHandler(env.service, api.Config{APIKey: "sk-test", Admin: admin})
		request := httptest.NewRequest(http.MethodPost, "/v1beta/models/"+testRuntimeModel+":streamGenerateContent?alt=sse",
			strings.NewReader(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`))
		request.Header.Set("Authorization", "Bearer sk-test")
		recorder := httptest.NewRecorder()
		served := make(chan struct{})
		go func() {
			defer close(served)
			handler.ServeHTTP(recorder, request)
		}()
		select {
		case <-upstream.delivered:
		case <-time.After(5 * time.Second):
			t.Fatal("转发没有交出结束事件")
		}
		id := activeRequestID(t, env.service)
		stopEnv(t, env)
		select {
		case <-served:
		case <-time.After(5 * time.Second):
			t.Fatal("请求没有结束")
		}
		if body := recorder.Body.String(); recorder.Code != http.StatusOK || !strings.Contains(body, `"finishReason":"STOP"`) || strings.Contains(body, `"error"`) {
			t.Fatalf("status=%d body=%s，期望完整的回复、没有错误帧", recorder.Code, body)
		}
		if state := states.await(t, id); state != "completed" {
			t.Fatalf("请求登记为 %s，期望按已交出的终态记为 completed", state)
		}
		admin.mu.Lock()
		entries := append([]api.AccessLog(nil), admin.entries...)
		admin.mu.Unlock()
		if len(entries) != 1 || entries[0].Status != http.StatusOK || entries[0].Error != "" || entries[0].Canceled {
			t.Fatalf("访问日志 = %+v，期望一条 200、没有错误", entries)
		}
		assertLeasesReleased(t, env.pool)
	})
}

// TestRequestCancelAfterDataContextEnds 请求上下文的取消函数（登记在请求登记表里，管理端取消与停止时的 cancelAll 调用它）：
// 数据上下文已经结束（生成服务正在停止或重启）时按服务停止结束；服务运行中的取消、客户端先断开、请求已经结束后再调用都不记成服务停止。
// 生成请求、其他数据请求（转写等）与 Live 会话三种上下文都一样
func TestRequestCancelAfterDataContextEnds(t *testing.T) {
	kinds := []struct {
		name  string
		build func(service *trackedService, parent context.Context) (context.Context, context.CancelFunc, error)
	}{
		{"生成", (*trackedService).generateRequestContext},
		{"数据", (*trackedService).dataRequestContext},
		{"Live", (*trackedService).bidiRequestContext},
	}
	for _, kind := range kinds {
		for _, how := range []string{"停止后取消", "运行中取消", "客户端先断开", "结束后再取消"} {
			t.Run(kind.name+"/"+how, func(t *testing.T) {
				data, stopData := context.WithCancel(context.Background())
				defer stopData()
				service := &trackedService{dataContext: data}
				service.state.Store(serviceRunning)
				service.timeout.Store(int64(time.Minute))
				var parent context.Context = context.Background()
				disconnect := func() {}
				if how == "客户端先断开" {
					if kind.name == "生成" {
						// 父上下文已经结束、经 context.AfterFunc 登记的回调还没执行（见 lateCanceledParent）：取消函数先到
						late := &lateCanceledParent{Context: context.Background(), done: make(chan struct{})}
						parent, disconnect = late, late.cancel
					} else {
						client, cancelClient := context.WithCancel(context.Background())
						defer cancelClient()
						parent, disconnect = client, cancelClient
					}
				}
				requestCtx, cancel, err := kind.build(service, parent)
				if err != nil {
					t.Fatal(err)
				}
				switch how {
				case "停止后取消":
					stopData()
					cancel()
				case "运行中取消":
					// 管理端单独取消，服务照常运行
					cancel()
				case "客户端先断开":
					disconnect()
					stopData()
					cancel()
				default:
					// 结束后再取消：请求结束时调用一次（转发收尾、管理端取消），停止时 cancelAll 又调用一次
					cancel()
					stopData()
					cancel()
				}
				if !waitUntil(time.Second, func() bool { return requestCtx.Err() != nil }) || !errors.Is(requestCtx.Err(), context.Canceled) {
					t.Fatalf("Err() = %v，期望取消", requestCtx.Err())
				}
				want := how == "停止后取消"
				if got := serviceStoppedRequest(requestCtx); got != want {
					t.Fatalf("记成服务停止 = %t，期望 %t", got, want)
				}
				var stopped *serviceStoppedError
				if err := endedRequestError(requestCtx, context.Canceled); errors.As(err, &stopped) != want {
					t.Fatalf("最终出口 = %v，期望换成 serviceStoppedError=%t", err, want)
				}
			})
		}
	}
}

// detachBlockingData 为生成服务的数据上下文的替身，始终没有结束（服务一直运行）：实现 AfterFunc，标准库 context.AfterFunc 经它登记
// （见 context.propagateCancel），请求的取消函数第一次解除登记时停在这里，直到 release 关闭。用来固定“同一个取消函数的第一次调用
// 已经解除登记、还没结束请求”的那段时间，第二次调用就在这时到达
type detachBlockingData struct {
	context.Context
	done     chan struct{}
	detached chan struct{}
	release  chan struct{}
}

func newDetachBlockingData() *detachBlockingData {
	return &detachBlockingData{Context: context.Background(), done: make(chan struct{}), detached: make(chan struct{}), release: make(chan struct{})}
}

func (d *detachBlockingData) Done() <-chan struct{} { return d.done }

func (d *detachBlockingData) Err() error { return nil }

func (d *detachBlockingData) AfterFunc(func()) func() bool {
	var once sync.Once
	return func() bool {
		first := false
		once.Do(func() {
			first = true
			close(d.detached)
		})
		if first {
			<-d.release
		}
		return first
	}
}

// TestConcurrentCancelWhileRunning 服务运行中两处同时调用同一个请求的取消函数（管理页连点取消、两个管理页同时取消、管理端取消与
// 降级判定拒绝撞在一起）：第二次调用到达时第一次已经解除服务停止的登记、还没结束请求，第二次调用看到解除失败。请求应照旧按取消结束，
// 不记成服务停止（503 service_stopped、请求登记为失败）。原先只看解除失败，这时生成请求走 stopService、数据请求与 Live 会话以
// errServiceStoppedCause 取消。生成请求、其他数据请求（转写等）与 Live 会话三种上下文都一样
func TestConcurrentCancelWhileRunning(t *testing.T) {
	kinds := []struct {
		name  string
		build func(service *trackedService, parent context.Context) (context.Context, context.CancelFunc, error)
	}{
		{"生成", (*trackedService).generateRequestContext},
		{"数据", (*trackedService).dataRequestContext},
		{"Live", (*trackedService).bidiRequestContext},
	}
	for _, kind := range kinds {
		t.Run(kind.name, func(t *testing.T) {
			data := newDetachBlockingData()
			service := &trackedService{dataContext: data}
			service.state.Store(serviceRunning)
			service.timeout.Store(int64(time.Minute))
			requestCtx, cancel, err := kind.build(service, context.Background())
			if err != nil {
				t.Fatal(err)
			}
			first := make(chan struct{})
			go func() {
				defer close(first)
				cancel()
			}()
			release := sync.OnceFunc(func() { close(data.release) })
			defer func() {
				release()
				<-first
			}()
			select {
			case <-data.detached:
			case <-time.After(5 * time.Second):
				t.Fatal("第一次取消没有解除服务停止的登记")
			}
			second := make(chan struct{})
			go func() {
				defer close(second)
				cancel()
			}()
			select {
			case <-second:
			case <-time.After(5 * time.Second):
				t.Fatal("第二次取消没有返回")
			}
			check := func(when string) {
				t.Helper()
				if !errors.Is(requestCtx.Err(), context.Canceled) {
					t.Fatalf("%s：Err() = %v，期望取消", when, requestCtx.Err())
				}
				if serviceStoppedRequest(requestCtx) {
					t.Fatalf("%s：服务运行中的取消被记成服务停止", when)
				}
				var stopped *serviceStoppedError
				if err := endedRequestError(requestCtx, context.Canceled); errors.As(err, &stopped) || !errors.Is(err, context.Canceled) {
					t.Fatalf("%s：最终出口 = %v，期望照旧是取消", when, err)
				}
			}
			check("第二次取消返回后")
			release()
			<-first
			check("两次取消都返回后")
		})
	}
}
