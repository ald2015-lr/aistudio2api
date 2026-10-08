package app

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// stallingService 是测试用的上游：stuck 中的账户永远不发事件（直到本次尝试被取消），其余账户延迟 delay 后正常返回一段正文
type stallingService struct {
	delay time.Duration

	mu        sync.Mutex
	stuck     map[string]bool
	calls     []string
	causes    map[string]error
	exited    map[string]bool
	stuckDone chan struct{}
}

func newStallingService(stuck ...string) *stallingService {
	service := &stallingService{
		stuck: make(map[string]bool), causes: make(map[string]error), exited: make(map[string]bool),
		stuckDone: make(chan struct{}, len(stuck)),
	}
	for _, accountID := range stuck {
		service.stuck[accountID] = true
	}
	return service
}

func (service *stallingService) Models(context.Context) ([]aistudio.Model, error) { return nil, nil }

func (service *stallingService) CountTokens(context.Context, aistudio.TokenCountRequest) (aistudio.TokenCount, error) {
	return aistudio.TokenCount{}, nil
}

func (service *stallingService) Generate(ctx context.Context, request aistudio.GenerateRequest) (<-chan aistudio.Event, error) {
	service.mu.Lock()
	service.calls = append(service.calls, request.AccountID)
	stuck := service.stuck[request.AccountID]
	service.mu.Unlock()
	events := make(chan aistudio.Event)
	go func() {
		defer close(events)
		if stuck {
			<-ctx.Done()
			service.mu.Lock()
			service.causes[request.AccountID] = context.Cause(ctx)
			service.exited[request.AccountID] = true
			service.mu.Unlock()
			service.stuckDone <- struct{}{}
			return
		}
		timer := time.NewTimer(service.delay)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-ctx.Done():
			return
		}
		for _, event := range []aistudio.Event{
			{Kind: aistudio.EventText, Text: "hello", ProviderModel: testRuntimeModel},
			{Kind: aistudio.EventFinish, FinishReason: "STOP", ProviderModel: testRuntimeModel},
		} {
			select {
			case events <- event:
			case <-ctx.Done():
				return
			}
		}
	}()
	return events, nil
}

// firstEventTestService 返回带就绪 Worker 的生成服务，首事件超时为 timeout
func firstEventTestService(t *testing.T, upstream aistudio.Service, timeout time.Duration, accountIDs ...string) (*trackedService, *aistudio.AccountPool) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	requests := newRequestRegistry(ctx)
	pool, accounts := testAccountPool(t, accountIDs...)
	workers := newAccountWorkerManager(pool, accounts, requests, "", "", time.Minute, len(accounts), len(accounts), 1, false)
	t.Cleanup(func() { _ = workers.Close() })
	for _, account := range accounts {
		worker, _ := aistudio.NewStubWorker(account.ID, 0)
		workers.accounts[account.ID].worker = worker
		workers.accounts[account.ID].warm.Store(true)
	}
	service := &trackedService{
		lifecycle: ctx, service: upstream, pool: pool, requests: requests, workers: workers,
		forbidden: newForbiddenTracker(), quota: newQuotaSharing("", requests),
	}
	service.timeout.Store(int64(time.Minute))
	service.firstEventTimeout.Store(int64(timeout))
	return service, pool
}

// runFirstEventRequest 执行一次生成，返回收到的正文与错误
func runFirstEventRequest(t *testing.T, service *trackedService) (string, error) {
	t.Helper()
	request := aistudio.GenerateRequest{
		ID: "req-first-event", Model: testRuntimeModel,
		Contents: []aistudio.Content{{Role: aistudio.RoleUser, Parts: []aistudio.Part{{Text: "hi"}}}},
	}
	requestCtx, requestCancel := context.WithTimeout(service.lifecycle, 20*time.Second)
	defer requestCancel()
	events := make(chan aistudio.Event, 8)
	done := make(chan struct{})
	go func() {
		defer close(done)
		service.generateWithRetry(service.lifecycle, requestCtx, requestCancel, time.Now(), request, "", events, newGenerationDiagnostics(request, "未发送"))
	}()
	var text string
	var err error
	for event := range events {
		switch event.Kind {
		case aistudio.EventText:
			text += event.Text
		case aistudio.EventError:
			err = event.Err
		}
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("generateWithRetry 没有结束")
	}
	return text, err
}

// assertLeasesReleased 断言请求结束后所有账户的租约都已释放，且没有因首事件超时进入冷却
func assertLeasesReleased(t *testing.T, pool *aistudio.AccountPool) {
	t.Helper()
	for _, status := range pool.Status() {
		if status.State != aistudio.AccountReady {
			t.Fatalf("账户 %s 状态 = %s，期望租约已释放且没有冷却", status.ID, status.State)
		}
	}
}

// TestFirstEventTimeoutRetriesOnAnotherAccount 一次尝试在首事件超时内没有任何上游事件时只取消这一次尝试，换到另一个账号完成请求
func TestFirstEventTimeoutRetriesOnAnotherAccount(t *testing.T) {
	upstream := newStallingService("alice@example.com")
	service, pool := firstEventTestService(t, upstream, 150*time.Millisecond, "alice@example.com", "bob@example.com")
	text, err := runFirstEventRequest(t, service)
	if err != nil || text != "hello" {
		t.Fatalf("应换号后完成：text=%q err=%v", text, err)
	}
	upstream.mu.Lock()
	calls, cause, exited := append([]string(nil), upstream.calls...), upstream.causes["alice@example.com"], upstream.exited["alice@example.com"]
	upstream.mu.Unlock()
	if len(calls) != 2 || calls[0] != "alice@example.com" || calls[1] != "bob@example.com" {
		t.Fatalf("尝试顺序 = %v，期望先 alice 超时再 bob", calls)
	}
	var timeout *firstEventTimeoutError
	if !exited || !errors.As(cause, &timeout) {
		t.Fatalf("超时的尝试应被取消并退出：exited=%v cause=%v", exited, cause)
	}
	assertLeasesReleased(t, pool)
}

// TestFirstEventTimeoutWithoutRetryIsDeadline 不能再换号时按上游超时结束（对外 504），不是取消
func TestFirstEventTimeoutWithoutRetryIsDeadline(t *testing.T) {
	upstream := newStallingService("alice@example.com")
	service, pool := firstEventTestService(t, upstream, 100*time.Millisecond, "alice@example.com")
	started := time.Now()
	_, err := runFirstEventRequest(t, service)
	var timeout *firstEventTimeoutError
	if !errors.As(err, &timeout) || !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v，期望首事件超时（满足 DeadlineExceeded、不是取消）", err)
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("首事件超时后应尽快结束，实际用时 %s", elapsed)
	}
	select {
	case <-upstream.stuckDone:
	default:
		t.Fatal("超时的尝试在请求结束前应已退出")
	}
	assertLeasesReleased(t, pool)
}

// TestFirstEventTimeoutDisabledWaits 首事件超时为 0（默认）时不中断慢的首事件；超时大于实际等待时同样不中断
func TestFirstEventTimeoutDisabledWaits(t *testing.T) {
	for _, timeout := range []time.Duration{0, 2 * time.Second} {
		upstream := newStallingService()
		upstream.delay = 300 * time.Millisecond
		service, pool := firstEventTestService(t, upstream, timeout, "alice@example.com")
		text, err := runFirstEventRequest(t, service)
		if err != nil || text != "hello" {
			t.Fatalf("超时=%s：text=%q err=%v，期望正常完成", timeout, text, err)
		}
		upstream.mu.Lock()
		calls := len(upstream.calls)
		upstream.mu.Unlock()
		if calls != 1 {
			t.Fatalf("超时=%s：尝试次数 = %d，期望 1", timeout, calls)
		}
		assertLeasesReleased(t, pool)
	}
}

// TestFirstEventDeadlineStopFire 停止与触发互斥：停止后不会再取消尝试，触发后停止返回超时错误；0 表示关闭
func TestFirstEventDeadlineStopFire(t *testing.T) {
	var cancelled []error
	var mu sync.Mutex
	cancel := func(err error) {
		mu.Lock()
		cancelled = append(cancelled, err)
		mu.Unlock()
	}
	disabled := newFirstEventDeadline(0, cancel)
	disabled.start()
	if disabled.stop() != nil {
		t.Fatal("关闭时不应触发")
	}
	stopped := newFirstEventDeadline(20*time.Millisecond, cancel)
	stopped.start()
	if stopped.stop() != nil {
		t.Fatal("及时停止时不应返回超时")
	}
	fired := newFirstEventDeadline(10*time.Millisecond, cancel)
	fired.start()
	fired.start()
	time.Sleep(100 * time.Millisecond)
	err := fired.stop()
	var timeout *firstEventTimeoutError
	if !errors.As(err, &timeout) {
		t.Fatalf("触发后停止应返回超时错误，得到 %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(cancelled) != 1 || !errors.As(cancelled[0], &timeout) {
		t.Fatalf("应只取消一次且带超时原因，得到 %v", cancelled)
	}
}
