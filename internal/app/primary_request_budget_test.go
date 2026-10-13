package app

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/api"
	"github.com/Mag1cFall/AIStudio2API/internal/camoufoxnative"
)

// primaryBudgetSlack 为排队截止时间之外允许的误差：截止时间到后还要复查一次候选、经过账户池锁才返回
const primaryBudgetSlack = 250 * time.Millisecond

// TestPrimaryBudgetSharedAcrossAttempts 主用模型的排队预算整个请求共用一份：失败的尝试（403、额度 429）计入截止时间，
// 换号后不再得到一份新预算，号池繁忙的 429 在排队上限加少量误差内返回；失败的尝试晚于截止时间时，下一次尝试没有立即
// 可用的账号就立即返回，不再排队。错误写明总耗时与第几次尝试
func TestPrimaryBudgetSharedAcrossAttempts(t *testing.T) {
	const budget = time.Second
	for _, test := range []struct {
		name     string
		denied   []string
		quota    []string
		delay    time.Duration
		busy     string
		accounts []string
		attempt  int
		minimum  time.Duration
	}{
		// 原先：300ms 的 403 + 200ms 退避 + 一份新的 1s 预算，约 1.5s
		{name: "403 后号池忙碌", denied: []string{forbiddenBad}, delay: 300 * time.Millisecond, busy: forbiddenGood,
			accounts: []string{forbiddenBad, forbiddenGood}, attempt: 2, minimum: budget},
		// 原先：1.2s 的 403 + 200ms 退避 + 1s，约 2.4s
		{name: "403 晚于截止时间", denied: []string{forbiddenBad}, delay: 1200 * time.Millisecond, busy: forbiddenGood,
			accounts: []string{forbiddenBad, forbiddenGood}, attempt: 2, minimum: 1200 * time.Millisecond},
		// 原先：两次 300ms 的额度 429（不退避、每次换号一份新预算）+ 1s，约 1.6s
		{name: "额度 429 连续换号", quota: []string{"q1@example.com", "q2@example.com"}, delay: 300 * time.Millisecond, busy: "zb@example.com",
			accounts: []string{"q1@example.com", "q2@example.com", "zb@example.com"}, attempt: 3, minimum: budget},
	} {
		t.Run(test.name, func(t *testing.T) {
			upstream := &forbiddenUpstream{denied: map[string]bool{}, quota: map[string]bool{}, delay: test.delay}
			for _, id := range test.denied {
				upstream.denied[id] = true
			}
			for _, id := range test.quota {
				upstream.quota[id] = true
			}
			service, pool := primaryBusyService(t, upstream, test.accounts...)
			release := holdLease(t, pool, test.busy)
			defer release()
			service.setPrimaryModels([]string{testRuntimeModel}, budget)
			_, err, elapsed := runPrimaryRequest(t, service, "req-budget")
			var busy *aistudio.CapacityBusyError
			if !errors.As(err, &busy) {
				t.Fatalf("err=%v，期望号池繁忙", err)
			}
			if elapsed < test.minimum || elapsed > test.minimum+primaryBudgetSlack {
				t.Fatalf("请求用时 %s，期望约 %s（排队预算整个请求共用一份）", elapsed, test.minimum)
			}
			if busy.Attempt != test.attempt || busy.Elapsed < test.minimum || busy.Limit != budget {
				t.Fatalf("号池繁忙错误 = %+v，期望第 %d 次尝试、总耗时不少于 %s", busy, test.attempt, test.minimum)
			}
			if message := err.Error(); !strings.Contains(message, "上限 1s，总耗时 ") || !strings.Contains(message, "次尝试，最后等待原因：") {
				t.Fatalf("错误文案 = %q，期望写明总耗时与第几次尝试", message)
			}
			if calls := upstream.called(); len(calls) != test.attempt-1 {
				t.Fatalf("上游调用 = %v，期望失败的账号各一次", calls)
			}
			if !progressLogged(service, "总耗时=") {
				t.Fatal("请求进度应写明总耗时")
			}
			if waiters := dispatchWaiters(service); waiters != 0 {
				t.Fatalf("请求结束后仍有 %d 个等待者", waiters)
			}
		})
	}
}

// coldStartTestService 返回浏览器后端的生成服务：每次冷启动耗时 launchDelay，fail 中的账号启动失败；账号都没有驻留 Worker
func coldStartTestService(t *testing.T, launchDelay time.Duration, fail map[string]bool, accountIDs ...string) *trackedService {
	t.Helper()
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	pool, accounts := testAccountPool(t, accountIDs...)
	pool.SetPerAccountConcurrency(1)
	manager := newAccountWorkerManager(pool, accounts, newRequestRegistry(ctx), "camoufox-test", "", time.Minute,
		len(accounts), len(accounts), 8, false)
	manager.launch = func(ctx context.Context, accountID string, _ camoufoxnative.Options) (*aistudio.NativeWorker, error) {
		select {
		case <-time.After(launchDelay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		if fail[accountID] {
			return nil, errors.New("模拟浏览器启动失败")
		}
		worker, _ := aistudio.NewStubWorker(accountID, 0)
		return worker, nil
	}
	t.Cleanup(func() { _ = manager.Close() })
	service := &trackedService{
		lifecycle: ctx, pool: pool, requests: manager.requests, workers: manager,
		forbidden: newForbiddenTracker(), quota: newQuotaSharing("", manager.requests),
	}
	service.timeout.Store(int64(time.Minute))
	return service
}

// TestPrimaryBudgetCreditsColdStart 请求自己现场冷启动的浏览器启动时长延后排队截止时间：600ms 的冷启动失败后，
// 300ms 的排队上限还剩 300ms，期间账号空闲就取得租约（不计抵扣时截止时间早已过去，会立即返回 429）；
// 一直没有空闲账号时约在 冷启动 + 排队上限 时返回 429，错误写明含现场冷启动
func TestPrimaryBudgetCreditsColdStart(t *testing.T) {
	const launch = 600 * time.Millisecond
	const limit = 300 * time.Millisecond
	t.Run("抵扣后等到空闲账号", func(t *testing.T) {
		service := coldStartTestService(t, launch, map[string]bool{"c@example.com": true}, "a@example.com", "c@example.com")
		service.setPrimaryModels([]string{testRuntimeModel}, limit)
		release := holdLease(t, service.pool, "a@example.com")
		// a 没有驻留 Worker：c 现场冷启动（同步）失败后，a 在 700ms 时空闲，还在抵扣后的截止时间（900ms）之内
		timer := time.AfterFunc(launch+100*time.Millisecond, release)
		defer timer.Stop()
		ctx, cancel := context.WithTimeout(service.lifecycle, 10*time.Second)
		defer cancel()
		lease, err, elapsed := acquirePrimary(ctx, service, aistudio.PoolScopeNormal)
		if err != nil {
			t.Fatalf("冷启动抵扣后应等到空闲账号，err=%v 用时 %s", err, elapsed)
		}
		defer func() { _ = lease.Release() }()
		if lease.Account().ID != "a@example.com" {
			t.Fatalf("取得 %s，期望空闲后的 a", lease.Account().ID)
		}
	})
	t.Run("没有空闲账号", func(t *testing.T) {
		service := coldStartTestService(t, launch, map[string]bool{"c@example.com": true}, "a@example.com", "c@example.com")
		service.setPrimaryModels([]string{testRuntimeModel}, limit)
		release := holdLease(t, service.pool, "a@example.com")
		defer release()
		ctx, cancel := context.WithTimeout(service.lifecycle, 10*time.Second)
		defer cancel()
		lease, err, elapsed := acquirePrimary(ctx, service, aistudio.PoolScopeNormal)
		var busy *aistudio.CapacityBusyError
		if lease != nil || !errors.As(err, &busy) {
			t.Fatalf("lease=%v err=%v，期望号池繁忙", lease, err)
		}
		if elapsed < launch+limit || elapsed > launch+limit+primaryBudgetSlack {
			t.Fatalf("用时 %s，期望约 冷启动 %s + 排队上限 %s", elapsed, launch, limit)
		}
		if busy.ColdStart < launch || busy.ColdStart > launch+primaryBudgetSlack || !strings.Contains(err.Error(), "含现场冷启动") {
			t.Fatalf("号池繁忙错误 = %+v（%q），期望记录现场冷启动时长", busy, err.Error())
		}
		if busy.Waited > limit+primaryBudgetSlack {
			t.Fatalf("已排队 %s，期望不含冷启动时长", busy.Waited)
		}
	})
}

// TestLeaseWaitBudgetCredits 冷启动抵扣合计不超过 INIT_TIMEOUT；没有预算时各方法不做任何事；候选冷却的等待延后截止时间
func TestLeaseWaitBudgetCredits(t *testing.T) {
	budget := &leaseWaitBudget{limit: time.Second, startedAt: time.Now()}
	budget.creditColdStart(700*time.Millisecond, time.Second)
	budget.creditColdStart(700*time.Millisecond, time.Second)
	if budget.coldStart != time.Second {
		t.Fatalf("冷启动抵扣 = %s，期望封顶 1s", budget.coldStart)
	}
	if remaining := budget.remaining(); remaining < 1900*time.Millisecond || remaining > 2*time.Second {
		t.Fatalf("剩余 %s，期望约 2s", remaining)
	}
	var none *leaseWaitBudget
	none.creditColdStart(time.Second, time.Minute)
	none.beginAttempt()
	if none.expired() {
		t.Fatal("没有预算时不应过期")
	}
	expired := &leaseWaitBudget{limit: 100 * time.Millisecond, startedAt: time.Now().Add(-time.Second)}
	if !expired.expired() {
		t.Fatal("截止时间已过应为过期")
	}
	changed := make(chan struct{})
	if err := expired.wait(context.Background(), changed, time.Second, "账号全部忙碌"); err == nil {
		t.Fatal("截止时间已过时容量原因的等待应立即返回号池繁忙")
	}
	if err := expired.wait(context.Background(), changed, 50*time.Millisecond, waitReasonCooling); err != nil {
		t.Fatalf("冷却等待不受截止时间限制：%v", err)
	}
	if expired.cooling < 50*time.Millisecond {
		t.Fatalf("冷却等待 %s 应计入延后时间", expired.cooling)
	}
}

// TestStreamedPrimaryBusyReturnsHTTPStatus 流式的主用模型请求第一次尝试 403、随后号池忙碌：429 以 HTTP 状态返回，
// 不是先回 200 再在流内报错。原先换号后又是一份完整预算，403 的 5.5s + 退避 + 5s 超过了写响应头前的 10s 等待
func TestStreamedPrimaryBusyReturnsHTTPStatus(t *testing.T) {
	if testing.Short() {
		t.Skip("需要约 6 秒")
	}
	t.Parallel()
	const limit = 5 * time.Second
	upstream := &forbiddenUpstream{denied: map[string]bool{forbiddenBad: true}, delay: 5500 * time.Millisecond}
	service, pool := primaryBusyService(t, upstream, forbiddenBad, forbiddenGood)
	release := holdLease(t, pool, forbiddenGood)
	defer release()
	service.setPrimaryModels([]string{testRuntimeModel}, limit)
	service.state.Store(serviceRunning)
	service.dataContext = service.lifecycle
	if wait := service.StreamStartWait(testRuntimeModel); wait != limit+primaryStreamStartGrace {
		t.Fatalf("StreamStartWait = %s", wait)
	}
	handler := api.NewHandler(service, api.Config{APIKey: "sk-test"})
	body := `{"model":"` + testRuntimeModel + `","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer sk-test")
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	startedAt := time.Now()
	handler.ServeHTTP(recorder, request)
	elapsed := time.Since(startedAt)
	if recorder.Code != http.StatusTooManyRequests || strings.Contains(recorder.Header().Get("Content-Type"), "event-stream") {
		t.Fatalf("status=%d content-type=%q 用时 %s body=%s，期望以 HTTP 429 返回", recorder.Code,
			recorder.Header().Get("Content-Type"), elapsed, recorder.Body.String())
	}
	if recorder.Header().Get("Retry-After") == "" {
		t.Fatal("429 应带 Retry-After")
	}
	if elapsed > 5500*time.Millisecond+primaryBudgetSlack {
		t.Fatalf("用时 %s，期望 403 返回后立即按 429 结束", elapsed)
	}
	if calls := upstream.called(); len(calls) != 1 || calls[0] != forbiddenBad {
		t.Fatalf("上游调用 = %v，期望只有第一次 403", calls)
	}
}

// TestExcludedWaiterPassesWake 换号重试的请求（带排除列表）与同模型的请求同队：它在队首被唤醒、空出的槽位却在它排除的账号上时，
// 把唤醒交给后面的请求，后面的请求立即取得这个槽位，不等兜底复查（1s）
func TestExcludedWaiterPassesWake(t *testing.T) {
	service, pool := primaryBusyService(t, nil, primaryTestA, primaryTestB)
	releaseA := holdLease(t, pool, primaryTestA)
	releaseB := holdLease(t, pool, primaryTestB)
	defer releaseB()
	ctx, cancel := context.WithTimeout(service.lifecycle, 10*time.Second)
	defer cancel()
	selection := aistudio.AccountSelection{ModelID: testRuntimeModel, Method: "generateContent", Pool: aistudio.PoolScopeNormal}
	excluded := selection
	excluded.ExcludedAccountIDs = []string{primaryTestA}
	type acquired struct {
		lease *aistudio.AccountLease
		err   error
		at    time.Time
	}
	results := make(chan acquired, 2)
	acquire := func(selection aistudio.AccountSelection) {
		lease, err := service.acquireWarmLease(ctx, selection)
		results <- acquired{lease: lease, err: err, at: time.Now()}
	}
	go acquire(excluded)
	if !waitUntil(2*time.Second, func() bool { return dispatchWaiters(service) == 1 }) {
		t.Fatal("带排除列表的请求应排进等待队列")
	}
	go acquire(selection)
	if !waitUntil(2*time.Second, func() bool { return dispatchWaiters(service) == 2 }) {
		t.Fatal("第二个请求应排在同一队列")
	}
	if key := dispatchKey(excluded); key != dispatchKey(selection) {
		t.Fatal("排除列表不应进队列键")
	}
	time.Sleep(100 * time.Millisecond)
	releasedAt := time.Now()
	releaseA()
	var got acquired
	select {
	case got = <-results:
	case <-ctx.Done():
		t.Fatal("A 空闲后没有请求取得租约")
	}
	if got.err != nil || got.lease.Account().ID != primaryTestA {
		t.Fatalf("A 空闲后取得 lease=%v err=%v，期望不排除 A 的请求取得 A", got.lease, got.err)
	}
	if waited := got.at.Sub(releasedAt); waited >= 500*time.Millisecond {
		t.Fatalf("A 空闲后 %s 才取得，期望队首把唤醒交给后面的请求", waited)
	}
	if err := got.lease.Release(); err != nil {
		t.Fatal(err)
	}
	// 收尾：带排除列表的请求等 B
	releaseB()
	select {
	case got = <-results:
		if got.err != nil || got.lease.Account().ID != primaryTestB {
			t.Fatalf("带排除列表的请求取得 lease=%v err=%v，期望 B", got.lease, got.err)
		}
		_ = got.lease.Release()
	case <-ctx.Done():
		t.Fatal("带排除列表的请求没有取得 B")
	}
}

// TestDispatchPassOn passOn 只唤醒紧随其后的一个请求；队尾与不在队列里的请求不做任何事
func TestDispatchPassOn(t *testing.T) {
	queue := newDispatchQueue()
	first, second, third := queue.join("k"), queue.join("k"), queue.join("k")
	for _, waiter := range []*dispatchWaiter{first, second, third} {
		drainWake(waiter)
	}
	queue.passOn(first)
	if !woken(second) || woken(third) || woken(first) {
		t.Fatal("passOn 应只唤醒下一个请求")
	}
	queue.passOn(third)
	if woken(first) || woken(second) || woken(third) {
		t.Fatal("队尾 passOn 不应唤醒任何请求")
	}
	queue.leave(second)
	drainWake(third)
	queue.passOn(second)
	queue.passOn(nil)
	if woken(first) || woken(third) {
		t.Fatal("不在队列里的请求 passOn 不应唤醒任何请求")
	}
}

func drainWake(waiter *dispatchWaiter) {
	select {
	case <-waiter.wake:
	default:
	}
}

func woken(waiter *dispatchWaiter) bool {
	select {
	case <-waiter.wake:
		return true
	default:
		return false
	}
}

// TestExcludedHeadYieldsDuringPromote 带排除列表的队首（换号重试）现场冷启动备用账号期间不看调度信号：开始前离开等待队列并把唤醒
// 交给下一个请求。原先它一直占着队首，空出的账户（被它排除）的唤醒只落在它身上，后面的主用模型请求等不到兜底复查就按 429 返回，
// 而那个账户一直空闲
func TestExcludedHeadYieldsDuringPromote(t *testing.T) {
	const a, s1 = "a@example.com", "s1@example.com"
	service := coldStartTestService(t, 4*time.Second, nil, a, s1)
	service.workers.maxActive.Store(1)
	stub, _ := aistudio.NewStubWorker(a, 0)
	service.workers.accounts[a].worker = stub
	service.workers.accounts[a].warm.Store(true)
	service.setPrimaryModels([]string{testRuntimeModel}, 1500*time.Millisecond)
	releaseA := holdLease(t, service.pool, a)
	ctx, cancel := context.WithTimeout(service.lifecycle, 15*time.Second)
	defer cancel()
	selection := aistudio.AccountSelection{ModelID: testRuntimeModel, Method: "generateContent", Pool: aistudio.PoolScopeNormal}
	retried := selection
	retried.ExcludedAccountIDs = []string{a}
	go func() {
		if lease, _ := service.acquireWarmLease(ctx, retried); lease != nil {
			_ = lease.Release()
		}
	}()
	if !waitUntil(2*time.Second, func() bool { return dispatchWaiters(service) == 1 }) {
		t.Fatal("带排除列表的请求应排进等待队列")
	}
	type result struct {
		lease *aistudio.AccountLease
		err   error
		at    time.Time
	}
	done := make(chan result, 1)
	startedAt := time.Now()
	go func() {
		lease, err := service.acquireWarmLease(withLeaseWaitBudget(ctx, service.primaryBudget(testRuntimeModel, aistudio.PoolScopeNormal)), selection)
		done <- result{lease: lease, err: err, at: time.Now()}
	}()
	if !waitUntil(2*time.Second, func() bool { return dispatchWaiters(service) == 2 }) {
		t.Fatal("主用模型的请求应排在同一队列")
	}
	// A 在主用模型请求第一次兜底复查之后、截止时间之前空闲
	time.Sleep(time.Until(startedAt.Add(1100 * time.Millisecond)))
	releasedAt := time.Now()
	releaseA()
	got := <-done
	if got.lease != nil {
		defer got.lease.Release()
	}
	if got.err != nil || got.lease == nil || got.lease.Account().ID != a {
		t.Fatalf("A 空闲后主用模型的请求应取得 A：lease=%v err=%v", got.lease, got.err)
	}
	if waited := got.at.Sub(releasedAt); waited >= 300*time.Millisecond {
		t.Fatalf("A 空闲后 %s 才取得，期望队首冷启动前把唤醒交给后面的请求", waited)
	}
}

// retryQueueUpstream 让 forbiddenBad 600ms 后返回 403 无权限，其余账号 300ms 后正常返回；调用在等待之前记录
type retryQueueUpstream struct {
	forbiddenUpstream
}

func (upstream *retryQueueUpstream) Generate(ctx context.Context, request aistudio.GenerateRequest) (<-chan aistudio.Event, error) {
	upstream.mu.Lock()
	upstream.calls = append(upstream.calls, request.AccountID)
	upstream.mu.Unlock()
	delay := 300 * time.Millisecond
	if request.AccountID == forbiddenBad {
		delay = 600 * time.Millisecond
	}
	select {
	case <-time.After(delay):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if request.AccountID == forbiddenBad {
		return nil, &aistudio.RPCError{
			Method: "GenerateContent", StatusCode: http.StatusForbidden, Code: 7, Message: "The caller does not have permission",
		}
	}
	events := make(chan aistudio.Event, 2)
	events <- aistudio.Event{Kind: aistudio.EventText, Text: "hello"}
	events <- aistudio.Event{Kind: aistudio.EventFinish, FinishReason: "STOP"}
	close(events)
	return events, nil
}

// TestRetryKeepsQueuePosition 换号重试按请求到达的先后排回队列：R 先到、第一次尝试 403 时 N1-N3 已在排队，R 的重试排在它们前面，
// 先取得空出的账号。原先重试排到队尾，主用模型的截止时间却照常从请求开始计，最早到的 R 反而 429，按到达顺序四个请求都来得及
func TestRetryKeepsQueuePosition(t *testing.T) {
	upstream := &retryQueueUpstream{}
	service, pool := primaryBusyService(t, upstream, forbiddenBad, forbiddenGood)
	service.setPrimaryModels([]string{testRuntimeModel}, 1500*time.Millisecond)
	releaseGood := holdLease(t, pool, forbiddenGood)
	startedAt := time.Now()
	type result struct {
		name string
		err  error
		at   time.Duration
	}
	results := make(chan result, 4)
	run := func(name string) {
		_, err, _ := runPrimaryRequest(t, service, "req-"+name)
		results <- result{name: name, err: err, at: time.Since(startedAt)}
	}
	go run("R")
	if !waitUntil(2*time.Second, func() bool { return len(upstream.called()) == 1 }) {
		t.Fatal("R 应先取得 forbiddenBad")
	}
	time.Sleep(time.Until(startedAt.Add(350 * time.Millisecond)))
	for index, name := range []string{"N1", "N2", "N3"} {
		go run(name)
		if !waitUntil(2*time.Second, func() bool { return dispatchWaiters(service) == index+1 }) {
			t.Fatalf("%s 应排队", name)
		}
	}
	if !waitUntil(2*time.Second, func() bool { return dispatchWaiters(service) == 4 }) {
		t.Fatal("R 的换号重试应排队")
	}
	time.Sleep(time.Until(startedAt.Add(700 * time.Millisecond)))
	releaseGood()
	finished := make(map[string]time.Duration, 4)
	for range 4 {
		got := <-results
		if got.err != nil {
			t.Errorf("%s 在 %s 失败：%v", got.name, got.at.Round(time.Millisecond), got.err)
		}
		finished[got.name] = got.at
	}
	for _, name := range []string{"N1", "N2", "N3"} {
		if finished["R"] >= finished[name] {
			t.Fatalf("先到的 R 应先完成：%v", finished)
		}
	}
}
