package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/api"
)

// pacedEnd 为 pacedService 发完推理事件后的结束方式
type pacedEnd int

const (
	pacedFinish          pacedEnd = iota // 发一段正文与 finish 后关闭事件流
	pacedHang                            // 不再发事件，挂起到本次尝试被取消
	pacedHangAfterFinish                 // 发完正文与 finish 后不关闭事件流，挂起到本次尝试被取消
)

// pacedScript 为 pacedService 的一次生成：首事件前等待 firstDelay，之后每隔 interval 发一条推理（共 reasoning 条，-1 为一直发），
// 再按 end 结束；hangFirst 为首事件前就挂起（不发任何事件）；firstAfterCancel 为 Generate 阻塞到本次尝试被取消后，
// 返回一个已放入一条推理事件的事件流（模拟首事件与期限同时到达）
type pacedScript struct {
	firstDelay       time.Duration
	interval         time.Duration
	reasoning        int
	end              pacedEnd
	hangFirst        bool
	firstAfterCancel bool
}

// pacedService 是按脚本出事件的假上游：记录调用次数、各次尝试被取消的原因（context.Cause）、是否在最后一个事件之前被取消，
// 以及上游 goroutine 是否已退出，全部受锁保护；事件通道带缓冲 1。所有脚本在本次尝试被取消后都会退出
type pacedService struct {
	script pacedScript

	mu            sync.Mutex
	calls         []string
	causes        []error
	canceledEarly bool
	running       int
	exited        int
}

func (service *pacedService) Models(context.Context) ([]aistudio.Model, error) { return nil, nil }

func (service *pacedService) CountTokens(context.Context, aistudio.TokenCountRequest) (aistudio.TokenCount, error) {
	return aistudio.TokenCount{}, nil
}

func (service *pacedService) Generate(ctx context.Context, request aistudio.GenerateRequest) (<-chan aistudio.Event, error) {
	service.mu.Lock()
	service.calls = append(service.calls, request.AccountID)
	script := service.script
	service.running++
	service.mu.Unlock()
	if script.firstAfterCancel {
		<-ctx.Done()
		events := make(chan aistudio.Event, 1)
		events <- aistudio.Event{Kind: aistudio.EventReasoning, Text: "think", ProviderModel: testRuntimeModel}
		close(events)
		service.finished(ctx, false)
		return events, nil
	}
	events := make(chan aistudio.Event, 1)
	go func() {
		defer close(events)
		early := false
		defer func() { service.finished(ctx, early) }()
		send := func(event aistudio.Event) bool {
			event.ProviderModel = testRuntimeModel
			select {
			case events <- event:
				return true
			case <-ctx.Done():
				early = true
				return false
			}
		}
		pause := func(delay time.Duration) bool {
			timer := time.NewTimer(delay)
			defer timer.Stop()
			select {
			case <-timer.C:
				return true
			case <-ctx.Done():
				early = true
				return false
			}
		}
		if script.hangFirst {
			<-ctx.Done()
			return
		}
		if !pause(script.firstDelay) {
			return
		}
		for i := 0; script.reasoning < 0 || i < script.reasoning; i++ {
			if i > 0 && !pause(script.interval) {
				return
			}
			if !send(aistudio.Event{Kind: aistudio.EventReasoning, Text: "think"}) {
				return
			}
		}
		if script.end == pacedHang {
			<-ctx.Done()
			return
		}
		if !send(aistudio.Event{Kind: aistudio.EventText, Text: "hello"}) ||
			!send(aistudio.Event{Kind: aistudio.EventFinish, FinishReason: "STOP"}) {
			return
		}
		if script.end == pacedHangAfterFinish {
			<-ctx.Done()
		}
	}()
	return events, nil
}

// finished 记录一次上游 goroutine 退出：取消原因与是否在最后一个事件之前被取消
func (service *pacedService) finished(ctx context.Context, early bool) {
	service.mu.Lock()
	defer service.mu.Unlock()
	service.running--
	service.exited++
	if ctx.Err() != nil {
		service.causes = append(service.causes, context.Cause(ctx))
	}
	if early {
		service.canceledEarly = true
	}
}

// snapshot 返回调用次数、各次取消原因、是否提前取消与还没退出的上游 goroutine 数
func (service *pacedService) snapshot() (int, []error, bool, int) {
	service.mu.Lock()
	defer service.mu.Unlock()
	return len(service.calls), append([]error(nil), service.causes...), service.canceledEarly, service.running
}

// deadlineResult 为一次生成的结果：收到的事件、错误事件、首个非错误事件与结束的时刻
type deadlineResult struct {
	events    []aistudio.Event
	err       error
	errEvents int
	startedAt time.Time
	firstAt   time.Time
	endedAt   time.Time
	state     string
}

func (result deadlineResult) nonError() int {
	count := 0
	for _, event := range result.events {
		if event.Kind != aistudio.EventError {
			count++
		}
	}
	return count
}

// requestStates 订阅请求登记表，记录各请求的最终状态（completed、failed、cancelled）
type requestStates struct {
	mu     sync.Mutex
	states map[string]string
}

func watchRequestStates(t *testing.T, service *trackedService) *requestStates {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	subscriber := service.requests.subscribe(ctx)
	events := service.requests.activateSubscriber(subscriber, nil, nil)
	states := &requestStates{states: make(map[string]string)}
	go func() {
		for event := range events {
			request, ok := event.Data.(api.AdminRequest)
			if event.Type != "request" || !ok {
				continue
			}
			switch request.State {
			case "completed", "failed", "cancelled":
				states.mu.Lock()
				states.states[request.ID] = request.State
				states.mu.Unlock()
			}
		}
	}()
	return states
}

// await 等请求 id 的最终状态
func (states *requestStates) await(t *testing.T, id string) string {
	t.Helper()
	var state string
	if !waitUntil(5*time.Second, func() bool {
		states.mu.Lock()
		defer states.mu.Unlock()
		state = states.states[id]
		return state != ""
	}) {
		t.Fatalf("请求 %s 没有登记最终状态", id)
	}
	return state
}

// deadlineRequest 返回一次普通的生成请求
func deadlineRequest(id string) aistudio.GenerateRequest {
	return aistudio.GenerateRequest{
		ID: id, Model: testRuntimeModel,
		Contents: []aistudio.Content{{Role: aistudio.RoleUser, Parts: []aistudio.Part{{Text: "hi"}}}},
	}
}

// startDeadlineRequest 以 gc 为请求上下文在后台执行 generateWithRetry（与 startGenerate 相同：请求登记在管理端、取消函数为 gc.cancel），
// 返回结果通道；onFirst 在收到首个非错误事件时调用
func startDeadlineRequest(service *trackedService, request aistudio.GenerateRequest, gc *generateContext, onFirst func()) <-chan deadlineResult {
	results := make(chan deadlineResult, 1)
	service.requests.start(service.lifecycle, request, gc.cancel)
	events := make(chan aistudio.Event, 8)
	done := make(chan struct{})
	go func() {
		defer close(done)
		service.generateWithRetry(service.lifecycle, gc, gc.cancel, time.Now(), request, "", events, newGenerationDiagnostics(request, "未发送"))
	}()
	go func() {
		result := deadlineResult{startedAt: gc.startedAt}
		for event := range events {
			result.events = append(result.events, event)
			if event.Kind == aistudio.EventError {
				result.err = event.Err
				result.errEvents++
				continue
			}
			if result.firstAt.IsZero() {
				result.firstAt = time.Now()
				if onFirst != nil {
					onFirst()
				}
			}
		}
		result.endedAt = time.Now()
		<-done
		results <- result
	}()
	return results
}

// runDeadlineRequest 执行一次生成并等它结束（含 generateWithRetry 返回）
func runDeadlineRequest(t *testing.T, service *trackedService, states *requestStates, request aistudio.GenerateRequest, gc *generateContext) deadlineResult {
	t.Helper()
	select {
	case result := <-startDeadlineRequest(service, request, gc, nil):
		if states != nil {
			result.state = states.await(t, request.ID)
		}
		return result
	case <-time.After(20 * time.Second):
		t.Fatal("generateWithRetry 没有结束")
		return deadlineResult{}
	}
}

// assertServiceDeadline 断言错误是本服务的期限（对外 504）：满足 DeadlineExceeded、不满足 Canceled、状态 failed，文本以 prefix 开头
func assertServiceDeadline(t *testing.T, err error, prefix string) {
	t.Helper()
	var cut *serviceDeadlineError
	if !errors.As(err, &cut) || !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) || finalRequestState(err) != "failed" {
		t.Fatalf("err = %v，期望本服务期限（满足 DeadlineExceeded、不是取消）", err)
	}
	if !strings.HasPrefix(err.Error(), prefix) {
		t.Fatalf("错误文本 = %q，期望以 %q 开头", err.Error(), prefix)
	}
}

// TestGenerateLongStreamOutlivesRequestTimeout I1：开始输出后只要上游持续有事件就不按 REQUEST_TIMEOUT 截断，
// 总时长是首事件前期限的 5 倍仍完整结束，上游只调用一次、结束前没有被取消
func TestGenerateLongStreamOutlivesRequestTimeout(t *testing.T) {
	upstream := &pacedService{script: pacedScript{firstDelay: 20 * time.Millisecond, interval: 50 * time.Millisecond, reasoning: 30}}
	service, pool := firstEventTestService(t, upstream, 0, "alice@example.com", "bob@example.com")
	states := watchRequestStates(t, service)
	gc := newGenerateContext(service.lifecycle, time.Now(), 300*time.Millisecond, time.Hour)
	result := runDeadlineRequest(t, service, states, deadlineRequest("req-long-stream"), gc)
	if result.err != nil || result.state != "completed" {
		t.Fatalf("err=%v state=%s，期望长流完整结束", result.err, result.state)
	}
	if elapsed := result.endedAt.Sub(result.startedAt); elapsed < 1400*time.Millisecond {
		t.Fatalf("用时 %s，测试要求长流远超首事件前期限", elapsed)
	}
	kinds := make([]aistudio.EventKind, 0, len(result.events))
	for _, event := range result.events {
		kinds = append(kinds, event.Kind)
	}
	if len(kinds) != 32 || kinds[0] != aistudio.EventReasoning || kinds[29] != aistudio.EventReasoning ||
		kinds[30] != aistudio.EventText || kinds[31] != aistudio.EventFinish {
		t.Fatalf("事件 = %v，期望 30 条推理、正文与 finish", kinds)
	}
	calls, _, early, running := upstream.snapshot()
	if calls != 1 || early || running != 0 {
		t.Fatalf("上游调用=%d 提前取消=%v 未退出=%d，期望只调用一次且结束前没有被取消", calls, early, running)
	}
	assertLeasesReleased(t, pool)
}

// TestGenerateStartDeadlineIsServiceDeadline I2：首事件前上游一直没有事件：首事件前期限到期即结束，对外 504，
// 错误写明是本服务的首事件前期限与停在哪一步；两个账号都挂起时同样只调用一次上游
func TestGenerateStartDeadlineIsServiceDeadline(t *testing.T) {
	for _, accounts := range [][]string{{"alice@example.com"}, {"alice@example.com", "bob@example.com"}} {
		upstream := &pacedService{script: pacedScript{hangFirst: true}}
		service, pool := firstEventTestService(t, upstream, 0, accounts...)
		states := watchRequestStates(t, service)
		const limit = 200 * time.Millisecond
		gc := newGenerateContext(service.lifecycle, time.Now(), limit, 10*time.Second)
		result := runDeadlineRequest(t, service, states, deadlineRequest("req-start-deadline"), gc)
		assertServiceDeadline(t, result.err, "本服务首事件前期限 200ms 到期（停在 等待上游首事件）")
		if elapsed := result.endedAt.Sub(result.startedAt); elapsed < limit-deadlineEarly || elapsed > limit+deadlineSlack {
			t.Fatalf("用时 %s，期望约 %s", elapsed, limit)
		}
		if result.state != "failed" || result.nonError() != 0 || result.errEvents != 1 {
			t.Fatalf("state=%s 事件=%v，期望只有一条错误事件、状态 failed", result.state, result.events)
		}
		if !progressLogged(service, "本服务期限截断 | 阶段=首事件前 | 期限=200ms") || !progressLogged(service, "停在=等待上游首事件") {
			t.Fatal("应写一条本服务首事件前期限截断的进度日志")
		}
		calls, causes, _, running := upstream.snapshot()
		if calls != 1 || running != 0 || len(causes) != 1 || !errors.Is(causes[0], context.DeadlineExceeded) {
			t.Fatalf("上游调用=%d 未退出=%d 取消原因=%v，期望到期即结束、只调用一次", calls, running, causes)
		}
		assertLeasesReleased(t, pool)
	}
}

// TestGenerateFirstEventAtDeadline I3：首事件恰在首事件前期限到期之后到达（与 ctx.Done 同时就绪，两条路径各约一半）：
// 这个事件不转发（没有任何输出），只有一条 504 错误事件，上游至多调用一次，租约释放
func TestGenerateFirstEventAtDeadline(t *testing.T) {
	upstream := &pacedService{script: pacedScript{firstAfterCancel: true}}
	service, pool := firstEventTestService(t, upstream, 0, "alice@example.com", "bob@example.com")
	states := watchRequestStates(t, service)
	sameTime := 0
	for i := 0; i < 50; i++ {
		before, _, _, _ := upstream.snapshot()
		request := deadlineRequest(fmt.Sprintf("req-first-at-deadline-%d", i))
		gc := newGenerateContext(service.lifecycle, time.Now(), 80*time.Millisecond, 10*time.Second)
		result := runDeadlineRequest(t, service, states, request, gc)
		assertServiceDeadline(t, result.err, "本服务首事件前期限 80ms 到期")
		if result.nonError() != 0 || result.errEvents != 1 || result.state != "failed" {
			t.Fatalf("第 %d 次：事件=%v state=%s，期望不转发首事件、只有一条错误事件", i, result.events, result.state)
		}
		if strings.Contains(result.err.Error(), "（停在 首事件与期限同时到达（已丢弃，未向客户端输出））") {
			sameTime++
		}
		if after, _, _, running := upstream.snapshot(); after-before > 1 || running != 0 {
			t.Fatalf("第 %d 次：上游调用 %d 次、未退出 %d，期望至多一次且已退出", i, after-before, running)
		}
		assertLeasesReleased(t, pool)
	}
	if sameTime > 0 && !progressLogged(service, "上游首事件到达时请求已结束，不转发") {
		t.Fatal("首事件与期限同时到达时应写进度日志")
	}
	t.Logf("50 次中 %d 次在切换时发现期限已到（其余在等首事件时到期）", sameTime)
}

// TestGenerateIdleDeadlineAfterFirstEvent I4：首事件后上游挂起：连续首事件后空闲期限没有事件时以本服务空闲期限结束（对外 504），
// 错误写明已输出的事件数；上游被取消且在 generateWithRetry 返回前退出，租约释放
func TestGenerateIdleDeadlineAfterFirstEvent(t *testing.T) {
	upstream := &pacedService{script: pacedScript{firstDelay: 10 * time.Millisecond, reasoning: 1, end: pacedHang}}
	service, pool := firstEventTestService(t, upstream, 0, "alice@example.com")
	states := watchRequestStates(t, service)
	const limit = 200 * time.Millisecond
	gc := newGenerateContext(service.lifecycle, time.Now(), limit, 10*time.Second)
	result := runDeadlineRequest(t, service, states, deadlineRequest("req-idle-deadline"), gc)
	assertServiceDeadline(t, result.err, "本服务首事件后空闲期限 200ms 到期（已输出 推理=1 正文=0）: context deadline exceeded")
	if result.err.Error() != "本服务首事件后空闲期限 200ms 到期（已输出 推理=1 正文=0）: context deadline exceeded" {
		t.Fatalf("错误文本 = %q", result.err.Error())
	}
	if idle := result.endedAt.Sub(result.firstAt); idle < limit-deadlineEarly || idle > limit+deadlineSlack {
		t.Fatalf("首事件后 %s 结束，期望约 %s", idle, limit)
	}
	calls, causes, _, running := upstream.snapshot()
	if calls != 1 || running != 0 || len(causes) != 1 || !errors.Is(causes[0], context.DeadlineExceeded) {
		t.Fatalf("上游调用=%d 未退出=%d 取消原因=%v，期望上游以期限取消并在返回前退出", calls, running, causes)
	}
	if result.state != "failed" || !progressLogged(service, "本服务期限截断 | 阶段=首事件后空闲 | 期限=200ms") {
		t.Fatalf("state=%s，期望 failed 并写本服务期限截断日志", result.state)
	}
	assertLeasesReleased(t, pool)
}

// forwardTest 为直接测 forwardEvents 的环境：已切到首事件后的生成请求上下文、租约、自己控制的事件流与网络活动
type forwardTest struct {
	service  *trackedService
	pool     *aistudio.AccountPool
	gc       *generateContext
	source   chan aistudio.Event
	activity *upstreamActivity
	done     chan struct{}
}

func startForwardTest(t *testing.T, idle time.Duration, destination chan aistudio.Event) *forwardTest {
	t.Helper()
	service, pool := firstEventTestService(t, &pacedService{}, 0, "alice@example.com")
	lease, err := pool.AcquireFor(context.Background(), aistudio.AccountSelection{AccountID: "alice@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	gc := newGenerateContext(service.lifecycle, time.Now(), idle, 10*time.Second)
	if !gc.beginStreaming() {
		t.Fatal("beginStreaming 应成功")
	}
	env := &forwardTest{
		service: service, pool: pool, gc: gc, source: make(chan aistudio.Event, 8), activity: &upstreamActivity{}, done: make(chan struct{}),
	}
	request := deadlineRequest("req-forward")
	first := aistudio.Event{Kind: aistudio.EventReasoning, Text: "think", ProviderModel: testRuntimeModel}
	go func() {
		defer close(env.done)
		service.forwardEvents(service.lifecycle, gc, gc.cancel, request.ID, first, env.source, destination, lease, nil,
			env.activity, testRuntimeModel, newGenerationDiagnostics(request, "未发送"))
	}()
	return env
}

// collect 读完 destination，返回事件与错误
func collectEvents(t *testing.T, destination <-chan aistudio.Event, limit time.Duration) ([]aistudio.Event, error) {
	t.Helper()
	var events []aistudio.Event
	var err error
	timeout := time.After(limit)
	for {
		select {
		case event, ok := <-destination:
			if !ok {
				return events, err
			}
			events = append(events, event)
			if event.Kind == aistudio.EventError {
				err = event.Err
			}
		case <-timeout:
			t.Fatalf("%s 内没有结束转发，已收到 %v", limit, events)
			return events, err
		}
	}
}

// TestForwardNetworkBytesKeepStreamAlive I5：首事件后只有网络字节、没有事件（例如隐藏思考在本地丢弃）也不算空闲：
// 持续 4 倍空闲期限后 finish 仍完整结束
func TestForwardNetworkBytesKeepStreamAlive(t *testing.T) {
	const idle = 200 * time.Millisecond
	destination := make(chan aistudio.Event, 8)
	env := startForwardTest(t, idle, destination)
	stop := time.After(4 * idle)
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for waiting := true; waiting; {
		select {
		case <-ticker.C:
			env.activity.observe(10)
		case <-stop:
			waiting = false
		}
	}
	env.source <- aistudio.Event{Kind: aistudio.EventFinish, FinishReason: "STOP", ProviderModel: testRuntimeModel}
	close(env.source)
	events, err := collectEvents(t, destination, 5*time.Second)
	if err != nil || len(events) != 2 || events[1].Kind != aistudio.EventFinish {
		t.Fatalf("事件=%v err=%v，期望网络字节持续时不截断", events, err)
	}
	<-env.done
	if env.gc.deadlineCause() != nil {
		t.Fatalf("不应按本服务期限结束: %+v", env.gc.deadlineCause())
	}
	assertLeasesReleased(t, env.pool)
}

// TestForwardSlowConsumerNotIdle I6：下游读得慢（交付被阻塞超过空闲期限）不算上游空闲，照常完整结束
func TestForwardSlowConsumerNotIdle(t *testing.T) {
	const idle = 200 * time.Millisecond
	destination := make(chan aistudio.Event)
	env := startForwardTest(t, idle, destination)
	for range 3 {
		env.source <- aistudio.Event{Kind: aistudio.EventReasoning, Text: "think", ProviderModel: testRuntimeModel}
	}
	env.source <- aistudio.Event{Kind: aistudio.EventText, Text: "hello", ProviderModel: testRuntimeModel}
	env.source <- aistudio.Event{Kind: aistudio.EventFinish, FinishReason: "STOP", ProviderModel: testRuntimeModel}
	close(env.source)
	select {
	case event := <-destination:
		if event.Kind != aistudio.EventReasoning {
			t.Fatalf("首个事件 = %v", event)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("没有收到首事件")
	}
	time.Sleep(3 * idle)
	events, err := collectEvents(t, destination, 5*time.Second)
	if err != nil || len(events) != 5 || events[4].Kind != aistudio.EventFinish {
		t.Fatalf("事件=%v err=%v，期望下游慢读时不截断", events, err)
	}
	<-env.done
	if env.gc.deadlineCause() != nil {
		t.Fatalf("不应按本服务期限结束: %+v", env.gc.deadlineCause())
	}
	assertLeasesReleased(t, env.pool)
}

// TestGenerateTotalDurationLimit I7：上游一直以低速出事件（空闲期限截不断）时到总时长上限结束，错误写明已输出的事件数
func TestGenerateTotalDurationLimit(t *testing.T) {
	upstream := &pacedService{script: pacedScript{interval: 50 * time.Millisecond, reasoning: -1}}
	service, pool := firstEventTestService(t, upstream, 0, "alice@example.com")
	states := watchRequestStates(t, service)
	const total = 500 * time.Millisecond
	gc := newGenerateContext(service.lifecycle, time.Now(), 200*time.Millisecond, total)
	result := runDeadlineRequest(t, service, states, deadlineRequest("req-total-limit"), gc)
	assertServiceDeadline(t, result.err, "本服务总时长上限 500ms 到期（已输出 推理=")
	var cut *serviceDeadlineError
	_ = errors.As(result.err, &cut)
	if cut.reasoning == 0 || cut.content != 0 || cut.stage != serviceDeadlineTotal {
		t.Fatalf("cut = %+v，期望已输出推理事件", cut)
	}
	if elapsed := result.endedAt.Sub(result.startedAt); elapsed < total-deadlineEarly || elapsed > total+deadlineSlack {
		t.Fatalf("用时 %s，期望约 %s", elapsed, total)
	}
	if result.state != "failed" || !progressLogged(service, "本服务期限截断 | 阶段=总时长上限 | 期限=500ms") {
		t.Fatalf("state=%s，期望 failed 并写本服务期限截断日志", result.state)
	}
	// 总时长上限经请求上下文结束转发，不像空闲期限那样先读完事件流：上游 goroutine 在取消后自行退出，可能晚于 generateWithRetry 返回
	if !waitUntil(2*time.Second, func() bool {
		_, _, _, running := upstream.snapshot()
		return running == 0
	}) {
		t.Fatal("上游应在取消后退出")
	}
	assertLeasesReleased(t, pool)
}

// TestGenerateTerminalThenUpstreamHangs I8：上游发完 finish 后不关闭事件流：回复完整交给客户端、状态 completed，
// 空闲期限后结束上游与租约并写日志，不把已完整的回复记成失败
func TestGenerateTerminalThenUpstreamHangs(t *testing.T) {
	upstream := &pacedService{script: pacedScript{reasoning: 1, end: pacedHangAfterFinish}}
	service, pool := firstEventTestService(t, upstream, 0, "alice@example.com")
	states := watchRequestStates(t, service)
	const idle = 200 * time.Millisecond
	gc := newGenerateContext(service.lifecycle, time.Now(), idle, 10*time.Second)
	result := runDeadlineRequest(t, service, states, deadlineRequest("req-terminal-hang"), gc)
	if result.err != nil || result.nonError() != 3 || result.state != "completed" {
		t.Fatalf("err=%v 事件=%v state=%s，期望完整结果、状态 completed", result.err, result.events, result.state)
	}
	if elapsed := result.endedAt.Sub(result.firstAt); elapsed < idle-deadlineEarly || elapsed > idle+deadlineSlack {
		t.Fatalf("首事件后 %s 结束转发，期望约空闲期限 %s", elapsed, idle)
	}
	calls, causes, _, running := upstream.snapshot()
	if calls != 1 || running != 0 || len(causes) != 1 {
		t.Fatalf("上游调用=%d 未退出=%d 取消原因=%v，期望上游被取消并退出", calls, running, causes)
	}
	if !progressLogged(service, "终态后上游事件流 200ms 内没有结束，结束转发") {
		t.Fatal("应写终态后上游事件流没有结束的日志")
	}
	assertLeasesReleased(t, pool)
}

// runningGenerateService 返回经 startGenerate 的生成服务（生成服务运行中，dataContext 为 data，REQUEST_TIMEOUT 为 timeout）
func runningGenerateService(t *testing.T, upstream aistudio.Service, data context.Context, timeout time.Duration, accountIDs ...string) (*trackedService, *aistudio.AccountPool) {
	t.Helper()
	service, pool := firstEventTestService(t, upstream, 0, accountIDs...)
	service.state.Store(serviceRunning)
	service.dataContext = data
	service.timeout.Store(int64(timeout))
	return service, pool
}

// startGenerated 经 Generate（startGenerate）发起请求，在后台读完事件流
func startGenerated(t *testing.T, service *trackedService, request aistudio.GenerateRequest, onFirst func()) <-chan deadlineResult {
	t.Helper()
	results := make(chan deadlineResult, 1)
	startedAt := time.Now()
	events, err := service.Generate(service.lifecycle, request)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		result := deadlineResult{startedAt: startedAt}
		for event := range events {
			result.events = append(result.events, event)
			if event.Kind == aistudio.EventError {
				result.err = event.Err
				result.errEvents++
				continue
			}
			if result.firstAt.IsZero() {
				result.firstAt = time.Now()
				if onFirst != nil {
					onFirst()
				}
			}
		}
		result.endedAt = time.Now()
		results <- result
	}()
	return results
}

// assertCanceledPromptly 断言请求在取消后 1s 内以取消结束（不带本服务期限的文本）
func assertCanceledPromptly(t *testing.T, results <-chan deadlineResult, canceledAt func() time.Time) deadlineResult {
	t.Helper()
	select {
	case result := <-results:
		if !errors.Is(result.err, context.Canceled) || finalRequestState(result.err) != "cancelled" || strings.Contains(result.err.Error(), "本服务") {
			t.Fatalf("err = %v，期望以取消结束、不带本服务期限的文本", result.err)
		}
		if elapsed := result.endedAt.Sub(canceledAt()); elapsed > time.Second {
			t.Fatalf("取消后 %s 才结束，期望 1s 内", elapsed)
		}
		return result
	case <-time.After(5 * time.Second):
		t.Fatal("取消后请求没有结束")
		return deadlineResult{}
	}
}

// TestGenerateCancelBothPhases I9、I10：管理端取消与生成服务停止在首事件前、首事件后两个阶段都立即结束请求（取消，不是期限）
func TestGenerateCancelBothPhases(t *testing.T) {
	for _, how := range []string{"管理端取消", "生成服务停止"} {
		for _, streaming := range []bool{false, true} {
			t.Run(how+map[bool]string{false: "首事件前", true: "首事件后"}[streaming], func(t *testing.T) {
				script := pacedScript{hangFirst: true}
				if streaming {
					script = pacedScript{interval: 50 * time.Millisecond, reasoning: -1}
				}
				upstream := &pacedService{script: script}
				data, stopData := context.WithCancel(context.Background())
				defer stopData()
				service, pool := runningGenerateService(t, upstream, data, 10*time.Second, "alice@example.com")
				states := watchRequestStates(t, service)
				request := deadlineRequest("req-cancel-" + how)
				var mu sync.Mutex
				var canceledAt time.Time
				cancel := func() {
					mu.Lock()
					canceledAt = time.Now()
					mu.Unlock()
					if how == "管理端取消" {
						if err := service.requests.cancel(request.ID); err != nil {
							t.Error(err)
						}
					} else {
						stopData()
					}
				}
				first := make(chan struct{})
				var once sync.Once
				results := startGenerated(t, service, request, func() { once.Do(func() { close(first) }) })
				if streaming {
					select {
					case <-first:
					case <-time.After(5 * time.Second):
						t.Fatal("没有收到首事件")
					}
				} else if !waitUntil(5*time.Second, func() bool { calls, _, _, _ := upstream.snapshot(); return calls == 1 }) {
					t.Fatal("上游没有收到请求")
				}
				cancel()
				assertCanceledPromptly(t, results, func() time.Time { mu.Lock(); defer mu.Unlock(); return canceledAt })
				if state := states.await(t, request.ID); state != "cancelled" {
					t.Fatalf("state = %s，期望 cancelled", state)
				}
				if !waitUntil(time.Second, func() bool { _, _, _, running := upstream.snapshot(); return running == 0 }) {
					t.Fatal("上游应已被取消并退出")
				}
				_, causes, _, _ := upstream.snapshot()
				if len(causes) != 1 || !errors.Is(causes[0], context.Canceled) {
					t.Fatalf("上游取消原因 = %v，期望 Canceled", causes)
				}
				assertLeasesReleased(t, pool)
			})
		}
	}
}

// TestGenerateHotReloadAffectsNewRequests I11：热更新 REQUEST_TIMEOUT 只影响新请求：进行中的请求仍按建立时的空闲期限，
// 更新后开始的请求按新值在首事件后空闲到期
func TestGenerateHotReloadAffectsNewRequests(t *testing.T) {
	const before, after, interval = 1500 * time.Millisecond, 150 * time.Millisecond, 450 * time.Millisecond
	script := pacedScript{interval: interval, reasoning: 4}
	upstream := &pacedService{script: script}
	service, _ := runningGenerateService(t, upstream, context.Background(), before, "alice@example.com", "bob@example.com")
	reloaded := make(chan struct{})
	var once sync.Once
	resultA := startGenerated(t, service, deadlineRequest("req-hot-a"), func() {
		once.Do(func() {
			service.setRequestTimeout(after)
			close(reloaded)
		})
	})
	select {
	case <-reloaded:
	case <-time.After(5 * time.Second):
		t.Fatal("请求 A 没有收到首事件")
	}
	resultB := startGenerated(t, service, deadlineRequest("req-hot-b"), nil)
	b := <-resultB
	assertServiceDeadline(t, b.err, "本服务首事件后空闲期限 150ms 到期（已输出 推理=1 正文=0）")
	if idle := b.endedAt.Sub(b.firstAt); idle < after-deadlineEarly || idle > after+deadlineSlack {
		t.Fatalf("请求 B 首事件后 %s 结束，期望约新的空闲期限 %s", idle, after)
	}
	a := <-resultA
	if a.err != nil || a.nonError() != 6 {
		t.Fatalf("请求 A：err=%v 事件=%v，期望按建立时的期限照常完成", a.err, a.events)
	}
}

// TestGenerateClientDisconnect I12：客户端断开（请求上下文的父上下文取消）在两个阶段都以取消结束，状态 cancelled
func TestGenerateClientDisconnect(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		script := pacedScript{hangFirst: true}
		if streaming {
			script = pacedScript{interval: 50 * time.Millisecond, reasoning: -1}
		}
		upstream := &pacedService{script: script}
		service, pool := firstEventTestService(t, upstream, 0, "alice@example.com")
		states := watchRequestStates(t, service)
		client, disconnect := context.WithCancel(service.lifecycle)
		gc := newGenerateContext(client, time.Now(), 10*time.Second, time.Hour)
		first := make(chan struct{})
		var once sync.Once
		request := deadlineRequest("req-client-disconnect")
		results := startDeadlineRequest(service, request, gc, func() { once.Do(func() { close(first) }) })
		if streaming {
			<-first
		} else if !waitUntil(5*time.Second, func() bool { calls, _, _, _ := upstream.snapshot(); return calls == 1 }) {
			t.Fatal("上游没有收到请求")
		}
		canceledAt := time.Now()
		disconnect()
		assertCanceledPromptly(t, results, func() time.Time { return canceledAt })
		if state := states.await(t, request.ID); state != "cancelled" {
			t.Fatalf("首事件后=%v：state = %s，期望 cancelled", streaming, state)
		}
		assertLeasesReleased(t, pool)
	}
}

// TestGenerateFirstEventTimeoutAtStartDeadline I13：FIRST_EVENT_TIMEOUT 与首事件前期限同时到期：每次都按期限（504、failed）结束，
// 不会因为尝试被首事件超时取消而变成取消（原来偶发 503/cancelled）
func TestGenerateFirstEventTimeoutAtStartDeadline(t *testing.T) {
	const limit = 100 * time.Millisecond
	upstream := &pacedService{script: pacedScript{hangFirst: true}}
	service, pool := firstEventTestService(t, upstream, limit, "alice@example.com")
	for i := 0; i < 20; i++ {
		// 首事件超时从拿到事件流时开始计，比请求上下文晚一点：首事件前期限逐次加长 0.1ms（最多 1.9ms），覆盖两者的不同先后
		gc := newGenerateContext(service.lifecycle, time.Now(), limit+time.Duration(i)*100*time.Microsecond, 10*time.Second)
		result := runDeadlineRequest(t, service, nil, deadlineRequest("req-first-event-at-deadline"), gc)
		if !errors.Is(result.err, context.DeadlineExceeded) || errors.Is(result.err, context.Canceled) || finalRequestState(result.err) != "failed" {
			t.Fatalf("第 %d 次：err = %v，期望满足 DeadlineExceeded、不是取消", i, result.err)
		}
		assertLeasesReleased(t, pool)
	}
}

// TestGenerateQueueBoundedByStartDeadline I14：排队受首事件前期限约束（不是总时长上限），错误带排队原因
func TestGenerateQueueBoundedByStartDeadline(t *testing.T) {
	upstream := &pacedService{script: pacedScript{reasoning: 1}}
	service, pool := firstEventTestService(t, upstream, 0, "alice@example.com")
	pool.SetPerAccountConcurrency(1)
	release := holdLease(t, pool, "alice@example.com")
	defer release()
	const limit = 300 * time.Millisecond
	gc := newGenerateContext(service.lifecycle, time.Now(), limit, 10*time.Second)
	result := runDeadlineRequest(t, service, nil, deadlineRequest("req-queue-deadline"), gc)
	assertServiceDeadline(t, result.err, "本服务首事件前期限 300ms 到期: 排队 ")
	if elapsed := result.endedAt.Sub(result.startedAt); elapsed < limit-deadlineEarly || elapsed > limit+deadlineSlack {
		t.Fatalf("用时 %s，期望约首事件前期限 %s（不是总时长上限）", elapsed, limit)
	}
	if calls, _, _, _ := upstream.snapshot(); calls != 0 {
		t.Fatalf("上游调用 %d 次，期望排队中结束、没有发出", calls)
	}
}

// TestGenerateDeferredResetBoundedByStartDeadline I15：回到原账号等关闭失败 Worker 受首事件前期限约束，错误带等待原因与上次失败
func TestGenerateDeferredResetBoundedByStartDeadline(t *testing.T) {
	upstream := newWorkerFailureService(map[string]int{resetTestAlice: 1})
	service, _ := workerResetTestService(t, upstream, resetTestAlice, resetTestBob)
	// bob 不支持该模型：换号找不到账号，回到原账号等待
	if err := service.pool.SetCatalog(resetTestBob, aistudio.BenefitTierFree, []aistudio.Model{{
		ID: "other-model", Methods: []string{"generateContent"}, Capabilities: map[string]bool{"chat_model": true},
	}}); err != nil {
		t.Fatal(err)
	}
	holder := newSiblingHolder(service.pool)
	upstream.beforeFail = holder.hold
	const limit = time.Second
	gc := newGenerateContext(service.lifecycle, time.Now(), limit, 10*time.Second)
	results := startDeadlineRequest(service, deadlineRequest("req-deferred-deadline"), gc, nil)
	sibling := holder.lease(t)
	defer func() { _ = sibling.Release() }()
	var result deadlineResult
	select {
	case result = <-results:
	case <-time.After(10 * time.Second):
		t.Fatal("请求没有结束")
	}
	assertServiceDeadline(t, result.err, "本服务首事件前期限 1s 到期: 回到原账号等待 ")
	if !strings.Contains(result.err.Error(), "协议错误码 5") {
		t.Fatalf("错误文本 = %q，期望带上次的失败", result.err.Error())
	}
	if elapsed := result.endedAt.Sub(result.startedAt); elapsed < limit-deadlineEarly || elapsed > limit+deadlineSlack {
		t.Fatalf("用时 %s，期望约首事件前期限 %s", elapsed, limit)
	}
}

// longStreamResetService 是 I16 的上游：请求 ID 为 longID 的按 script 慢速长流，其余请求返回协议 Code 5
type longStreamResetService struct {
	pacedService
	longID string
}

func (service *longStreamResetService) Generate(ctx context.Context, request aistudio.GenerateRequest) (<-chan aistudio.Event, error) {
	if request.ID != service.longID {
		return nil, waaRuntimeFailure("GenerateContent")
	}
	return service.pacedService.Generate(ctx, request)
}

// TestLongStreamDuringWorkerReset I16：同账号长流 A 已超过首事件前期限时，固定账号的请求 B 遇到 Code 5：B 在自己的首事件前期限内 504，
// A 照常完成；A 结束后后台关闭完成（Worker 版本加一），全程没有后台恢复放弃
func TestLongStreamDuringWorkerReset(t *testing.T) {
	if testing.Short() {
		t.Skip("需要约 2 秒")
	}
	upstream := &longStreamResetService{
		pacedService: pacedService{script: pacedScript{interval: 100 * time.Millisecond, reasoning: 15}},
		longID:       "req-long-a",
	}
	service, _ := workerResetTestService(t, upstream, resetTestAlice)
	service.workers.requestTimeout.Store(int64(300 * time.Millisecond))
	generation := service.workers.WorkerGeneration(resetTestAlice)
	const limit = 300 * time.Millisecond
	requestA := deadlineRequest("req-long-a")
	gcA := newGenerateContext(service.lifecycle, time.Now(), limit, 10*time.Second)
	resultsA := startDeadlineRequest(service, requestA, gcA, nil)
	if !waitUntil(5*time.Second, func() bool { calls, _, _, _ := upstream.snapshot(); return calls == 1 }) {
		t.Fatal("长流 A 没有开始")
	}
	time.Sleep(time.Until(gcA.startedAt.Add(limit + 100*time.Millisecond)))
	requestB := deadlineRequest("req-pinned-b")
	requestB.AccountID = resetTestAlice
	gcB := newGenerateContext(service.lifecycle, time.Now(), limit, 10*time.Second)
	b := <-startDeadlineRequest(service, requestB, gcB, nil)
	assertServiceDeadline(t, b.err, "本服务首事件前期限 300ms 到期")
	if elapsed := b.endedAt.Sub(b.startedAt); elapsed > limit+deadlineSlack {
		t.Fatalf("请求 B 用时 %s，期望在自己的首事件前期限内结束", elapsed)
	}
	a := <-resultsA
	if a.err != nil || a.nonError() != 17 {
		t.Fatalf("长流 A：err=%v 事件数=%d，期望照常完成", a.err, a.nonError())
	}
	if !waitUntil(5*time.Second, func() bool { return service.workers.WorkerGeneration(resetTestAlice) == generation+1 }) {
		t.Fatalf("长流结束后后台关闭应完成：Worker 版本 %d → %d", generation, service.workers.WorkerGeneration(resetTestAlice))
	}
	if workerResetLogged(service, "WAA Worker 后台恢复放弃") {
		t.Fatal("长流期间不应放弃后台恢复")
	}
}

// lingeringService 是访问日志结果字段测试的假上游：依次放入 events 后不再发事件，本次尝试被取消后再过 closeDelay 才关闭事件流
// （模拟上游被取消后迟迟才退出）；exited 在事件流关闭后关闭
type lingeringService struct {
	events     []aistudio.Event
	closeDelay time.Duration
	exited     chan struct{}
}

func (service *lingeringService) Models(context.Context) ([]aistudio.Model, error) { return nil, nil }

func (service *lingeringService) CountTokens(context.Context, aistudio.TokenCountRequest) (aistudio.TokenCount, error) {
	return aistudio.TokenCount{}, nil
}

func (service *lingeringService) Generate(ctx context.Context, _ aistudio.GenerateRequest) (<-chan aistudio.Event, error) {
	events := make(chan aistudio.Event, len(service.events))
	for _, event := range service.events {
		event.ProviderModel = testRuntimeModel
		events <- event
	}
	go func() {
		defer close(service.exited)
		<-ctx.Done()
		timer := time.NewTimer(service.closeDelay)
		defer timer.Stop()
		<-timer.C
		close(events)
	}()
	return events, nil
}

// accessLogCapture 记录 API 层写出的访问日志（requests.db 与 request.finished 的来源）
type accessLogCapture struct {
	api.AdminService
	mu      sync.Mutex
	entries []api.AccessLog
}

func (admin *accessLogCapture) RecordAccessStart(api.AccessLog) {}

func (admin *accessLogCapture) RecordAccessLog(entry api.AccessLog) {
	admin.mu.Lock()
	admin.entries = append(admin.entries, entry)
	admin.mu.Unlock()
}

// TestAccessLogResultBeforeErrorEvent 首事件后以错误结束、事件流在错误之后迟迟不关闭时（本服务空闲期限截断后要读完事件流；
// 上游错误之后不关闭事件流），API 层收到错误事件就写访问日志：工具调用数与用量必须在错误事件交给下游之前写好，不能等事件流关闭
func TestAccessLogResultBeforeErrorEvent(t *testing.T) {
	toolCall := aistudio.Event{Kind: aistudio.EventToolCall, ToolCall: &aistudio.FunctionCall{ID: "call-1", Name: "lookup", Arguments: []byte(`{}`)}}
	usage := aistudio.Event{Kind: aistudio.EventUsage, Usage: &aistudio.Usage{InputTokens: 7, OutputTokens: 3, TotalTokens: 10}}
	for _, test := range []struct {
		name   string
		events []aistudio.Event
		status int
	}{
		{name: "本服务空闲期限", events: []aistudio.Event{toolCall, usage}, status: http.StatusGatewayTimeout},
		{name: "上游错误", events: []aistudio.Event{toolCall, usage, {Kind: aistudio.EventError, Err: errors.New("上游流内错误")}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			upstream := &lingeringService{events: test.events, closeDelay: 300 * time.Millisecond, exited: make(chan struct{})}
			service, pool := runningGenerateService(t, upstream, context.Background(), 200*time.Millisecond, "alice@example.com")
			admin := &accessLogCapture{}
			handler := api.NewHandler(service, api.Config{APIKey: "sk-test", Admin: admin})
			body := `{"model":"` + testRuntimeModel + `","stream":true,"messages":[{"role":"user","content":"hi"}]}`
			request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
			request.Header.Set("Authorization", "Bearer sk-test")
			request.Header.Set("Content-Type", "application/json")
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, request)
			admin.mu.Lock()
			entries := append([]api.AccessLog(nil), admin.entries...)
			admin.mu.Unlock()
			if len(entries) != 1 {
				t.Fatalf("访问日志条数 = %d，期望 1", len(entries))
			}
			entry := entries[0]
			if test.status != 0 && entry.Status != test.status {
				t.Fatalf("访问日志 status=%d error=%q，期望 %d", entry.Status, entry.Error, test.status)
			}
			if entry.Error == "" {
				t.Fatal("访问日志应记下错误")
			}
			if entry.ToolCalls != 1 || entry.Usage == nil || entry.Usage.TotalTokens != 10 {
				t.Fatalf("访问日志 tool_calls=%d usage=%+v，期望错误事件之前已写入工具调用数与用量", entry.ToolCalls, entry.Usage)
			}
			select {
			case <-upstream.exited:
			case <-time.After(5 * time.Second):
				t.Fatal("上游没有退出")
			}
			if !waitUntil(5*time.Second, func() bool { return pool.Status()[0].State == aistudio.AccountReady }) {
				t.Fatal("租约没有释放")
			}
		})
	}
}
