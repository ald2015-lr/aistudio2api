package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/api"
	"github.com/gofrs/flock"
)

// TestEndedRequestErrorOnlyForServiceStop 只有被生成服务停止或重启结束的请求在最终出口换成 serviceStoppedError（参照上游 792d67a）：
// 上下文的 Err() 仍是 context.Canceled；客户端先断开、管理端先取消、期限先到期的请求照旧按原来的错误结束，没有错误时不换
func TestEndedRequestErrorOnlyForServiceStop(t *testing.T) {
	var stopped *serviceStoppedError
	t.Run("生成请求", func(t *testing.T) {
		c := newGenerateContext(context.Background(), time.Now(), time.Minute, time.Hour)
		attempt, cancelAttempt := context.WithCancel(c)
		defer cancelAttempt()
		c.stopService()
		if !errors.Is(c.Err(), context.Canceled) || !errors.Is(attempt.Err(), context.Canceled) {
			t.Fatalf("服务停止后 Err() 应仍是取消：%v / %v", c.Err(), attempt.Err())
		}
		if err := endedRequestError(attempt, context.Canceled); !errors.As(err, &stopped) || errors.Is(err, context.Canceled) {
			t.Fatalf("最终出口应换成 serviceStoppedError，得到 %v", err)
		}
		if endedRequestError(c, nil) != nil {
			t.Fatal("没有错误时不应换")
		}
		canceled := newGenerateContext(context.Background(), time.Now(), time.Minute, time.Hour)
		canceled.cancel()
		canceled.stopService()
		if err := endedRequestError(canceled, context.Canceled); !errors.Is(err, context.Canceled) {
			t.Fatalf("管理端先取消的请求应照旧按取消结束，得到 %v", err)
		}
		client, disconnect := context.WithCancel(context.Background())
		disconnected := newGenerateContext(client, time.Now(), time.Minute, time.Hour)
		disconnect()
		if !waitUntil(time.Second, func() bool { return disconnected.Err() != nil }) {
			t.Fatal("客户端断开后请求上下文应结束")
		}
		disconnected.stopService()
		if err := endedRequestError(disconnected, context.Canceled); !errors.Is(err, context.Canceled) {
			t.Fatalf("客户端先断开的请求应照旧按取消结束，得到 %v", err)
		}
	})
	t.Run("数据请求", func(t *testing.T) {
		for _, how := range []string{"服务停止", "管理端取消", "客户端断开"} {
			data, stopData := context.WithCancel(context.Background())
			service := &trackedService{dataContext: data}
			service.state.Store(serviceRunning)
			service.timeout.Store(int64(time.Minute))
			client, disconnect := context.WithCancel(context.Background())
			requestCtx, cancel, err := service.dataRequestContext(client)
			if err != nil {
				t.Fatal(err)
			}
			observed := aistudio.WithAttemptCeiling(requestCtx, 2)
			switch how {
			case "服务停止":
				stopData()
			case "管理端取消":
				cancel()
				stopData()
			default:
				disconnect()
				stopData()
			}
			if !waitUntil(time.Second, func() bool { return observed.Err() != nil }) || !errors.Is(observed.Err(), context.Canceled) {
				t.Fatalf("%s：Err() 应为取消，得到 %v", how, observed.Err())
			}
			got := endedRequestError(observed, context.Canceled)
			if want := how == "服务停止"; errors.As(got, &stopped) != want {
				t.Fatalf("%s：最终错误 %v，期望换成 serviceStoppedError=%v", how, got, want)
			}
			cancel()
			disconnect()
		}
	})
}

// lateCanceledParent 模拟父上下文（客户端连接）已经结束、经 context.AfterFunc 登记的回调还没执行的那段时间：标准库在新 goroutine 里
// 调用这个回调，生成服务停止的回调可能先到。Err() 在 cancel 之后就是取消，登记的回调只在 deliver 时调用
type lateCanceledParent struct {
	context.Context
	done  chan struct{}
	mu    sync.Mutex
	err   error
	funcs []func()
}

func (p *lateCanceledParent) Done() <-chan struct{} { return p.done }

func (p *lateCanceledParent) Err() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.err
}

func (p *lateCanceledParent) AfterFunc(f func()) func() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.funcs = append(p.funcs, f)
	return func() bool { return false }
}

func (p *lateCanceledParent) cancel() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.err = context.Canceled
}

func (p *lateCanceledParent) deliver() {
	p.mu.Lock()
	funcs := p.funcs
	p.funcs = nil
	p.mu.Unlock()
	for _, f := range funcs {
		f()
	}
}

// TestStopServiceAfterParentCanceled 客户端先断开、父上下文的结束还没传到请求上下文时生成服务停止：按父上下文的原因（取消）结束，
// 不记成服务停止，最终出口照旧是取消（客户端断开为 499，不是 503 service_stopped）。原先 stopService 只看自己的 err，这种情况记成服务停止
func TestStopServiceAfterParentCanceled(t *testing.T) {
	parent := &lateCanceledParent{Context: context.Background(), done: make(chan struct{})}
	requestCtx := newGenerateContext(parent, time.Now(), time.Minute, time.Hour)
	parent.cancel()
	requestCtx.stopService()
	if requestCtx.stoppedByService() {
		t.Fatal("父上下文已经结束时不应记成服务停止")
	}
	if !errors.Is(requestCtx.Err(), context.Canceled) {
		t.Fatalf("请求上下文应按父上下文的取消结束，得到 %v", requestCtx.Err())
	}
	var stopped *serviceStoppedError
	if err := endedRequestError(requestCtx, context.Canceled); errors.As(err, &stopped) || !errors.Is(err, context.Canceled) {
		t.Fatalf("客户端先断开的请求最终应照旧是取消，得到 %v", err)
	}
	parent.deliver()
	if requestCtx.stoppedByService() || !errors.Is(requestCtx.Err(), context.Canceled) {
		t.Fatalf("父上下文的回调随后到达不应改变结束原因：%v / 服务停止=%v", requestCtx.Err(), requestCtx.stoppedByService())
	}

	// 父上下文还没结束时照旧记成服务停止
	alive := &lateCanceledParent{Context: context.Background(), done: make(chan struct{})}
	running := newGenerateContext(alive, time.Now(), time.Minute, time.Hour)
	running.stopService()
	if err := endedRequestError(running, context.Canceled); !running.stoppedByService() || !errors.As(err, &stopped) {
		t.Fatalf("父上下文还在时应记成服务停止，得到 %v", err)
	}
}

// countTokensBlockingUpstream 为 CountTokens 一直等到请求上下文结束、按它的错误返回的上游
type countTokensBlockingUpstream struct {
	fallbackUpstream
	started chan struct{}
}

func (upstream *countTokensBlockingUpstream) CountTokens(ctx context.Context, _ aistudio.TokenCountRequest) (aistudio.TokenCount, error) {
	upstream.started <- struct{}{}
	<-ctx.Done()
	return aistudio.TokenCount{}, ctx.Err()
}

// TestServiceStopEndsInflightRequests 停止生成服务时进行中的请求以 serviceStoppedError 结束：生成请求（两个账号，上游一直等到取消）
// 不换号、不冷却，上游收到的仍是取消；CountTokens 同样按服务已停止返回
func TestServiceStopEndsInflightRequests(t *testing.T) {
	var stopped *serviceStoppedError
	t.Run("生成", func(t *testing.T) {
		accounts := []string{"stop-a@example.com", "stop-b@example.com"}
		upstream := &fallbackUpstream{block: map[string]bool{testRuntimeModel: true}, blocked: make(chan string, 4)}
		data, stopData := context.WithCancel(context.Background())
		defer stopData()
		service, pool := runningGenerateService(t, upstream, data, 10*time.Second, accounts...)
		results := startGenerated(t, service, deadlineRequest("req-service-stop"), nil)
		select {
		case <-upstream.blocked:
		case <-time.After(5 * time.Second):
			t.Fatal("上游没有收到请求")
		}
		stopData()
		select {
		case result := <-results:
			if !errors.As(result.err, &stopped) || errors.Is(result.err, context.Canceled) {
				t.Fatalf("err = %v，期望 serviceStoppedError", result.err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("停止服务后请求没有结束")
		}
		if calls := upstream.models(); len(calls) != 1 {
			t.Fatalf("服务停止的取消不应换号：上游收到 %d 次", len(calls))
		}
		for _, accountID := range accounts {
			if status, _ := pool.StatusOf(accountID); len(status.Cooldowns) != 0 {
				t.Fatalf("服务停止不应冷却账号：%s %+v", accountID, status.Cooldowns)
			}
		}
		assertLeasesReleased(t, pool)
	})
	t.Run("CountTokens", func(t *testing.T) {
		upstream := &countTokensBlockingUpstream{started: make(chan struct{}, 1)}
		data, stopData := context.WithCancel(context.Background())
		defer stopData()
		service, _ := runningGenerateService(t, upstream, data, 10*time.Second, "stop-count@example.com")
		done := make(chan error, 1)
		go func() {
			_, err := service.CountTokens(context.Background(), aistudio.TokenCountRequest{Model: testRuntimeModel})
			done <- err
		}()
		select {
		case <-upstream.started:
		case <-time.After(5 * time.Second):
			t.Fatal("上游没有收到计数请求")
		}
		stopData()
		select {
		case err := <-done:
			if !errors.As(err, &stopped) {
				t.Fatalf("err = %v，期望 serviceStoppedError", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("停止服务后计数请求没有结束")
		}
	})
}

// TestForwardErrorEventAfterServiceStop 首事件之后上游以错误事件结束、这时生成服务已停止：账号按原错误处理完之后，交给客户端的是
// serviceStoppedError（转发收尾两条路径都要换：错误事件与请求上下文结束谁先被选中是随机的，这里多跑几次覆盖错误事件那一条）
func TestForwardErrorEventAfterServiceStop(t *testing.T) {
	var stopped *serviceStoppedError
	for iteration := range 16 {
		service, pool := firstEventTestService(t, &pacedService{}, 0, "alice@example.com")
		lease, err := pool.AcquireFor(context.Background(), aistudio.AccountSelection{AccountID: "alice@example.com"})
		if err != nil {
			t.Fatal(err)
		}
		gc := newGenerateContext(service.lifecycle, time.Now(), time.Minute, 10*time.Second)
		if !gc.beginStreaming() {
			t.Fatal("beginStreaming 应成功")
		}
		gc.stopService()
		request := deadlineRequest("req-forward-stop")
		destination := make(chan aistudio.Event, 8)
		source := make(chan aistudio.Event)
		close(source)
		first := aistudio.Event{Kind: aistudio.EventError, Err: context.Canceled}
		service.forwardEvents(service.lifecycle, gc, gc.cancel, request.ID, first, source, destination, lease, nil,
			&upstreamActivity{}, testRuntimeModel, newGenerationDiagnostics(request, "未发送"))
		for event := range destination {
			if event.Kind == aistudio.EventError && (!errors.As(event.Err, &stopped) || errors.Is(event.Err, context.Canceled)) {
				t.Fatalf("第 %d 次：交给客户端的错误 = %v，期望 serviceStoppedError", iteration+1, event.Err)
			}
		}
		assertLeasesReleased(t, pool)
	}
}

// afterTerminalUpstream 交出正文与结束事件后不关闭事件流：事件流不带缓冲，结束事件之后再交出一条推理事件，送达时转发一定已经把
// 结束事件交给下游、回到等上游事件的那一步（终态之后的事件被丢弃），这时关闭 delivered；之后挂起到本次尝试被取消
type afterTerminalUpstream struct {
	fallbackUpstream
	delivered chan struct{}
	exited    chan struct{}
}

func (upstream *afterTerminalUpstream) Generate(ctx context.Context, _ aistudio.GenerateRequest) (<-chan aistudio.Event, error) {
	events := make(chan aistudio.Event)
	go func() {
		defer close(upstream.exited)
		defer close(events)
		for _, event := range []aistudio.Event{
			{Kind: aistudio.EventText, Text: "hello"},
			{Kind: aistudio.EventFinish, FinishReason: "STOP"},
			{Kind: aistudio.EventReasoning},
		} {
			event.ProviderModel = testRuntimeModel
			select {
			case events <- event:
			case <-ctx.Done():
				return
			}
		}
		close(upstream.delivered)
		<-ctx.Done()
	}()
	return events, nil
}

// TestTerminalDeliveredThenRequestEndsKeepsResult 结束事件已经交给客户端、上游事件流迟迟不关闭时生成服务停止或管理端取消：
// 只结束上游与租约，结果仍按终态记录（请求登记为完成，访问日志 200 且没有错误），不把已完整交出的回复记成 503 service_stopped
// （改动前是取消、499）
func TestTerminalDeliveredThenRequestEndsKeepsResult(t *testing.T) {
	for _, how := range []string{"服务停止", "管理端取消"} {
		t.Run(how, func(t *testing.T) {
			upstream := &afterTerminalUpstream{delivered: make(chan struct{}), exited: make(chan struct{})}
			data, stopData := context.WithCancel(context.Background())
			defer stopData()
			service, pool := runningGenerateService(t, upstream, data, 10*time.Second, "terminal-stop@example.com")
			states := watchRequestStates(t, service)
			admin := &accessLogCapture{}
			handler := api.NewHandler(service, api.Config{APIKey: "sk-test", Admin: admin})
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
			active := service.requests.list()
			if len(active) != 1 {
				t.Fatalf("结束事件之后请求应仍在等上游事件流关闭：%+v", active)
			}
			requestID := active[0].ID
			if how == "服务停止" {
				stopData()
			} else if err := service.requests.cancel(requestID); err != nil {
				t.Fatal(err)
			}
			select {
			case <-served:
			case <-time.After(5 * time.Second):
				t.Fatal("请求没有结束")
			}
			body := recorder.Body.String()
			if recorder.Code != http.StatusOK || !strings.Contains(body, `"finishReason":"STOP"`) || strings.Contains(body, `"error"`) {
				t.Fatalf("status=%d body=%s，期望完整的回复、没有错误帧", recorder.Code, body)
			}
			if state := states.await(t, requestID); state != "completed" {
				t.Fatalf("请求登记为 %s，期望按已交出的终态记为 completed", state)
			}
			admin.mu.Lock()
			entries := append([]api.AccessLog(nil), admin.entries...)
			admin.mu.Unlock()
			if len(entries) != 1 {
				t.Fatalf("访问日志条数 = %d，期望 1", len(entries))
			}
			if entry := entries[0]; entry.Status != http.StatusOK || entry.Error != "" || entry.Canceled {
				t.Fatalf("访问日志 status=%d error=%q canceled=%t，期望 200 且没有错误", entry.Status, entry.Error, entry.Canceled)
			}
			select {
			case <-upstream.exited:
			case <-time.After(5 * time.Second):
				t.Fatal("上游应在请求结束后退出")
			}
			assertLeasesReleased(t, pool)
		})
	}
}

// TestTerminalUndeliveredThenServiceStop 结束事件还没交给下游（下游没有在读）时生成服务停止：回复并不完整，照旧按服务停止
// 记为失败，只有已经交出的终态才保留
func TestTerminalUndeliveredThenServiceStop(t *testing.T) {
	service, pool := firstEventTestService(t, &pacedService{}, 0, "alice@example.com")
	states := watchRequestStates(t, service)
	lease, err := pool.AcquireFor(context.Background(), aistudio.AccountSelection{AccountID: "alice@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	gc := newGenerateContext(service.lifecycle, time.Now(), time.Minute, 10*time.Second)
	if !gc.beginStreaming() {
		t.Fatal("beginStreaming 应成功")
	}
	request := deadlineRequest("req-terminal-undelivered")
	service.requests.start(service.lifecycle, request, gc.cancel)
	destination := make(chan aistudio.Event)
	source := make(chan aistudio.Event, 1)
	source <- aistudio.Event{Kind: aistudio.EventFinish, FinishReason: "STOP"}
	done := make(chan struct{})
	go func() {
		defer close(done)
		service.forwardEvents(service.lifecycle, gc, gc.cancel, request.ID, aistudio.Event{Kind: aistudio.EventText, Text: "hi"}, source, destination, lease, nil,
			&upstreamActivity{}, testRuntimeModel, newGenerationDiagnostics(request, "未发送"))
	}()
	if event := <-destination; event.Kind != aistudio.EventText {
		t.Fatalf("首个事件 = %+v，期望正文", event)
	}
	// 不再读：等转发取走结束事件（停在交给下游那一步），这时生成服务停止
	if !waitUntil(5*time.Second, func() bool { return len(source) == 0 }) {
		t.Fatal("转发没有取走结束事件")
	}
	gc.stopService()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("停止服务后转发没有结束")
	}
	for event := range destination {
		t.Fatalf("停止之后不应再交出事件：%+v", event)
	}
	if state := states.await(t, request.ID); state != "failed" {
		t.Fatalf("请求登记为 %s，期望结束事件没有交出时按服务停止记为 failed", state)
	}
	assertLeasesReleased(t, pool)
}

// stopBlockingUpstream 为视频、文件、转写与 Live 的假上游：每个调用都一直等到请求上下文结束，按它的错误返回（与真实上游被取消时一样）；
// 开始等待时把调用名写进 started。bodyBlocks 为真时 DownloadFile 立即返回，读正文时才等到请求上下文结束
type stopBlockingUpstream struct {
	fallbackUpstream
	started    chan string
	bodyBlocks bool
}

func (upstream *stopBlockingUpstream) block(ctx context.Context, name string) error {
	upstream.started <- name
	<-ctx.Done()
	return ctx.Err()
}

func (upstream *stopBlockingUpstream) GenerateVideo(ctx context.Context, _ aistudio.VideoRequest) (aistudio.VideoOperation, error) {
	return aistudio.VideoOperation{}, upstream.block(ctx, "GenerateVideo")
}

func (upstream *stopBlockingUpstream) GetGenerateVideoOperation(ctx context.Context, _ string) (aistudio.VideoOperation, error) {
	return aistudio.VideoOperation{}, upstream.block(ctx, "GetGenerateVideoOperation")
}

func (upstream *stopBlockingUpstream) DownloadFile(ctx context.Context, _ string) (aistudio.MediaStream, error) {
	if upstream.bodyBlocks {
		return aistudio.MediaStream{Body: &stopBlockingBody{ctx: ctx, upstream: upstream}, MIME: "video/mp4"}, nil
	}
	return aistudio.MediaStream{}, upstream.block(ctx, "DownloadFile")
}

func (upstream *stopBlockingUpstream) UploadFile(ctx context.Context, _ aistudio.UploadRequest) (aistudio.FileRef, error) {
	return aistudio.FileRef{}, upstream.block(ctx, "UploadFile")
}

func (upstream *stopBlockingUpstream) FileMetadata(ctx context.Context, _ string) (aistudio.FileMetadata, error) {
	return aistudio.FileMetadata{}, upstream.block(ctx, "FileMetadata")
}

func (upstream *stopBlockingUpstream) DeleteFile(ctx context.Context, _ string) error {
	return upstream.block(ctx, "DeleteFile")
}

func (upstream *stopBlockingUpstream) Transcribe(ctx context.Context, _ aistudio.TranscriptionRequest) (aistudio.TranscriptionResult, error) {
	return aistudio.TranscriptionResult{}, upstream.block(ctx, "Transcribe")
}

func (upstream *stopBlockingUpstream) OpenBidi(ctx context.Context, _ aistudio.BidiRequest) (*aistudio.BidiSession, error) {
	return nil, upstream.block(ctx, "OpenBidi")
}

// stopBlockingBody 为下载的正文：读取一直等到下载请求的上下文结束，按读错误返回（与连接被取消时一样，不是 EOF）
type stopBlockingBody struct {
	ctx      context.Context
	upstream *stopBlockingUpstream
}

func (body *stopBlockingBody) Read([]byte) (int, error) {
	return 0, body.upstream.block(body.ctx, "DownloadFile.Read")
}

func (body *stopBlockingBody) Close() error { return nil }

// TestServiceStopEndsInflightDataRequests 停止生成服务时进行中的视频、文件、下载、转写与 Live 会话建立以 serviceStoppedError 结束
// （503 service_stopped，不是取消），每个最终出口都要换：上游调用（视频创建与查询、下载打开与读正文、文件上传、元数据与删除、转写、
// Live 建立）进行中停止服务；转写与 Live 选候选、Live 等后台关闭这几处在上游调用之前的出口另测（见下面的说明）
func TestServiceStopEndsInflightDataRequests(t *testing.T) {
	const account = "stop-data@example.com"
	type dataCall struct {
		name       string
		bodyBlocks bool
		// pendingReset 为请求前给账户登记一次一直没有结束的后台关闭（Live 建立先等它，见 awaitLiveReset）
		pendingReset bool
		// preStopped 为请求开始时客户端上下文已带着服务停止的原因结束：数据请求上下文随之同步结束、原因相同，等价于生成服务在建立请求
		// 上下文之后、选候选之前停止（真实停止经 context.AfterFunc 在另一个 goroutine 里取消，落在这一步之前还是之后不确定）
		preStopped bool
		call       func(ctx context.Context, service *trackedService) error
	}
	transcription := func(ctx context.Context, service *trackedService) error {
		_, err := service.Transcribe(ctx, aistudio.TranscriptionRequest{
			ID: "req-stop-transcribe", Model: testRuntimeModel, Name: "a.wav", MIME: "audio/wav", Size: 4, Reader: strings.NewReader("RIFF"),
		})
		return err
	}
	for _, test := range []dataCall{
		{name: "GenerateVideo", call: func(ctx context.Context, service *trackedService) error {
			_, err := service.GenerateVideo(ctx, aistudio.VideoRequest{Model: testRuntimeModel, AccountID: account})
			return err
		}},
		{name: "GetGenerateVideoOperation", call: func(ctx context.Context, service *trackedService) error {
			_, err := service.GetGenerateVideoOperation(ctx, "operations/test")
			return err
		}},
		{name: "DownloadFile", call: func(ctx context.Context, service *trackedService) error {
			_, err := service.DownloadFile(ctx, "file-test")
			return err
		}},
		{name: "DownloadFile.Read", bodyBlocks: true, call: func(ctx context.Context, service *trackedService) error {
			media, err := service.DownloadFile(ctx, "file-test")
			if err != nil {
				return fmt.Errorf("打开下载不应失败: %w", err)
			}
			defer media.Body.Close()
			_, err = media.Body.Read(make([]byte, 16))
			return err
		}},
		{name: "UploadFile", call: func(ctx context.Context, service *trackedService) error {
			_, err := service.UploadFile(ctx, aistudio.UploadRequest{Name: "a.txt", MIME: "text/plain", Size: 1})
			return err
		}},
		{name: "FileMetadata", call: func(ctx context.Context, service *trackedService) error {
			_, err := service.FileMetadata(ctx, "file-test")
			return err
		}},
		{name: "DeleteFile", call: func(ctx context.Context, service *trackedService) error {
			return service.DeleteFile(ctx, "file-test")
		}},
		{name: "Transcribe", call: transcription},
		{name: "Transcribe 选候选", preStopped: true, call: transcription},
		{name: "OpenBidi", call: func(ctx context.Context, service *trackedService) error {
			_, err := service.OpenBidi(ctx, aistudio.BidiRequest{Model: testRuntimeModel, AccountID: account})
			return err
		}},
		{name: "OpenBidi 选候选", preStopped: true, call: func(ctx context.Context, service *trackedService) error {
			_, err := service.OpenBidi(ctx, aistudio.BidiRequest{Model: testRuntimeModel})
			return err
		}},
		{name: "OpenBidi 等后台关闭", pendingReset: true, call: func(ctx context.Context, service *trackedService) error {
			_, err := service.OpenBidi(ctx, aistudio.BidiRequest{Model: testRuntimeModel, AccountID: account})
			return err
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			upstream := &stopBlockingUpstream{started: make(chan string, 4), bodyBlocks: test.bodyBlocks}
			data, stopData := context.WithCancel(context.Background())
			defer stopData()
			service, pool := runningGenerateService(t, upstream, data, 10*time.Second, account)
			states := watchRequestStates(t, service)
			if test.pendingReset {
				service.workers.resetMu.Lock()
				service.workers.resets = map[workerResetKey]*workerReset{
					{accountID: account, generation: service.workers.WorkerGeneration(account)}: {done: make(chan struct{})},
				}
				service.workers.resetMu.Unlock()
			}
			client := context.Background()
			if test.preStopped {
				stoppedClient, stop := context.WithCancelCause(context.Background())
				stop(errServiceStoppedCause)
				client = stoppedClient
			}
			done := make(chan error, 1)
			go func() { done <- test.call(client, service) }()
			if !test.preStopped && !test.pendingReset {
				select {
				case <-upstream.started:
				case err := <-done:
					t.Fatalf("上游调用之前就结束了: %v", err)
				case <-time.After(5 * time.Second):
					t.Fatal("上游没有收到请求")
				}
			}
			if !test.preStopped {
				stopData()
			}
			var err error
			select {
			case err = <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("停止服务后请求没有结束")
			}
			var stopped *serviceStoppedError
			if !errors.As(err, &stopped) || errors.Is(err, context.Canceled) || finalRequestState(err) != "failed" {
				t.Fatalf("err = %v，期望 serviceStoppedError（失败，不是取消）", err)
			}
			select {
			case name := <-upstream.started:
				if test.preStopped || test.pendingReset {
					t.Fatalf("应在上游调用之前的出口结束，却调用了上游 %s", name)
				}
			default:
			}
			if strings.HasPrefix(test.name, "Transcribe") {
				if state := states.await(t, "req-stop-transcribe"); state != "failed" {
					t.Fatalf("转写请求登记为 %s，期望 failed", state)
				}
			}
			assertLeasesReleased(t, pool)
		})
	}
}

// TestServiceStopDuringFileResourceRefresh 生成请求引用已上传的文件、刷新文件所属账户的运行状态时生成服务停止：开始生成就以
// serviceStoppedError 结束（请求登记为失败），不按取消或锁定失败返回。账户运行状态的跨进程锁被另一个持有者拿着，刷新一直等锁到请求上下文结束
func TestServiceStopDuringFileResourceRefresh(t *testing.T) {
	const account, fileID = "stop-resource@example.com", "files/stop-resource"
	data, stopData := context.WithCancel(context.Background())
	defer stopData()
	service, pool := runningGenerateService(t, &fallbackUpstream{}, data, 10*time.Second, account)
	states := watchRequestStates(t, service)
	if err := pool.BindResource(fileID, account); err != nil {
		t.Fatal(err)
	}
	owner, err := pool.Account(account)
	if err != nil || owner.Directory == "" || owner.RuntimePath == "" {
		t.Fatalf("前提：账户应有目录与运行状态文件：%v", err)
	}
	// 与 aistudio.lockRuntimeState 同一个锁文件
	leases := filepath.Join(filepath.Dir(owner.Directory), ".leases")
	if err := os.MkdirAll(leases, 0o700); err != nil {
		t.Fatal(err)
	}
	lock := flock.New(filepath.Join(leases, filepath.Base(owner.Directory)+".runtime.lock"))
	if locked, err := lock.TryLock(); err != nil || !locked {
		t.Fatalf("前提：应能取得账户运行状态锁（locked=%t）：%v", locked, err)
	}
	defer func() { _ = lock.Unlock() }()
	request := deadlineRequest("req-stop-resource")
	request.Contents[0].Parts = append(request.Contents[0].Parts, aistudio.Part{File: &aistudio.FileRef{ID: fileID}})
	done := make(chan error, 1)
	go func() {
		_, err := service.Generate(context.Background(), request)
		done <- err
	}()
	// 请求登记之后才刷新文件资源（见 startGenerate）
	if !waitUntil(5*time.Second, func() bool { return len(service.requests.list()) == 1 }) {
		t.Fatal("请求没有登记")
	}
	stopData()
	select {
	case err = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("停止服务后请求没有结束")
	}
	var stopped *serviceStoppedError
	if !errors.As(err, &stopped) || errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v，期望 serviceStoppedError", err)
	}
	if state := states.await(t, request.ID); state != "failed" {
		t.Fatalf("请求登记为 %s，期望 failed", state)
	}
}
