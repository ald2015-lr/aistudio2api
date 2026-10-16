package app

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/camoufoxnative"
)

const (
	resetTestAlice = "alice@example.com"
	resetTestBob   = "bob@example.com"
)

// waaRuntimeFailure 返回上游的协议 Code 5（WAA 运行态失效，DefinitiveWAARuntimeFailure），需要关闭当前 Worker 版本
func waaRuntimeFailure(method string) error {
	return &aistudio.RPCError{Method: method, StatusCode: http.StatusNotFound, Code: 5, Message: "Ambiguous request for service ''"}
}

// workerFailureService 是测试用的上游：failures 中账户的前几次调用先执行 beforeFail（模拟同账号还有请求在输出、等其他请求到齐等），
// 再返回协议 Code 5；其余调用正常返回一段正文
type workerFailureService struct {
	mu         sync.Mutex
	failures   map[string]int
	beforeFail func(accountID string)
	calls      []string
}

func newWorkerFailureService(failures map[string]int) *workerFailureService {
	return &workerFailureService{failures: failures}
}

func (service *workerFailureService) Models(context.Context) ([]aistudio.Model, error) {
	return nil, nil
}

func (service *workerFailureService) CountTokens(context.Context, aistudio.TokenCountRequest) (aistudio.TokenCount, error) {
	return aistudio.TokenCount{}, nil
}

func (service *workerFailureService) Generate(_ context.Context, request aistudio.GenerateRequest) (<-chan aistudio.Event, error) {
	service.mu.Lock()
	service.calls = append(service.calls, request.AccountID)
	fail := service.failures[request.AccountID] > 0
	if fail {
		service.failures[request.AccountID]--
	}
	hook := service.beforeFail
	service.mu.Unlock()
	if fail {
		if hook != nil {
			hook(request.AccountID)
		}
		return nil, waaRuntimeFailure("GenerateContent")
	}
	events := make(chan aistudio.Event, 2)
	events <- aistudio.Event{Kind: aistudio.EventText, Text: "hello", ProviderModel: testRuntimeModel}
	events <- aistudio.Event{Kind: aistudio.EventFinish, FinishReason: "STOP", ProviderModel: testRuntimeModel}
	close(events)
	return events, nil
}

func (service *workerFailureService) called() []string {
	service.mu.Lock()
	defer service.mu.Unlock()
	return slices.Clone(service.calls)
}

// workerResetTestService 返回带就绪 stub Worker 的生成服务：账号按 ID 顺序优先填满（不轮询），Worker 关闭后现场冷启动新的 stub Worker；
// 不固定账号的请求等待关闭 50ms。返回各账号原 Worker 的成功关闭次数
func workerResetTestService(t *testing.T, upstream aistudio.Service, accountIDs ...string) (*trackedService, map[string]func() int) {
	t.Helper()
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	requests := newRequestRegistry(ctx)
	pool, accounts := testAccountPool(t, accountIDs...)
	pool.SetRoutingStrategy("fill-first")
	manager := newAccountWorkerManager(pool, accounts, requests, "camoufox-test", "", time.Minute,
		len(accounts), len(accounts), 8, false)
	manager.launch = func(_ context.Context, accountID string, _ camoufoxnative.Options) (*aistudio.NativeWorker, error) {
		worker, _ := aistudio.NewStubWorker(accountID, 0)
		return worker, nil
	}
	t.Cleanup(func() { _ = manager.Close() })
	closed := make(map[string]func() int, len(accounts))
	for _, account := range accounts {
		worker, closedCount := aistudio.NewStubWorker(account.ID, 0)
		manager.accounts[account.ID].worker = worker
		manager.accounts[account.ID].warm.Store(true)
		closed[account.ID] = closedCount
	}
	service := &trackedService{
		lifecycle: ctx, service: upstream, pool: pool, requests: requests, workers: manager,
		forbidden: newForbiddenTracker(), quota: newQuotaSharing("", requests),
	}
	service.timeout.Store(int64(time.Minute))
	setWorkerResetWait(service, 50*time.Millisecond)
	return service, closed
}

func setWorkerResetWait(service *trackedService, wait time.Duration) {
	service.resetWait.Store(&wait)
}

// resetTestResult 为一次生成的结果
type resetTestResult struct {
	text    string
	err     error
	elapsed time.Duration
}

// startResetTestRequest 在后台执行一次生成（accountID 非空时固定账号），返回结果通道
func startResetTestRequest(ctx context.Context, service *trackedService, id string, accountID string) <-chan resetTestResult {
	request := aistudio.GenerateRequest{
		ID: id, Model: testRuntimeModel, AccountID: accountID,
		Contents: []aistudio.Content{{Role: aistudio.RoleUser, Parts: []aistudio.Part{{Text: "hi"}}}},
	}
	results := make(chan resetTestResult, 1)
	startedAt := time.Now()
	go func() {
		requestCtx, requestCancel := context.WithTimeout(ctx, 20*time.Second)
		defer requestCancel()
		events := make(chan aistudio.Event, 8)
		done := make(chan struct{})
		go func() {
			defer close(done)
			service.generateWithRetry(service.lifecycle, requestCtx, requestCancel, time.Now(), request, "", events,
				newGenerationDiagnostics(request, "未发送"))
		}()
		var result resetTestResult
		for event := range events {
			switch event.Kind {
			case aistudio.EventText:
				result.text += event.Text
			case aistudio.EventError:
				result.err = event.Err
			}
		}
		<-done
		result.elapsed = time.Since(startedAt)
		results <- result
	}()
	return results
}

func awaitResetTestResult(t *testing.T, results <-chan resetTestResult, timeout time.Duration) resetTestResult {
	t.Helper()
	select {
	case result := <-results:
		return result
	case <-time.After(timeout):
		t.Fatalf("请求 %s 内没有结束", timeout)
		return resetTestResult{}
	}
}

// siblingHolder 在上游返回失败前占住同账号的另一个并发槽位，模拟同账号还有请求在输出
type siblingHolder struct {
	pool  *aistudio.AccountPool
	held  chan *aistudio.AccountLease
	fails chan error
}

func newSiblingHolder(pool *aistudio.AccountPool) *siblingHolder {
	return &siblingHolder{pool: pool, held: make(chan *aistudio.AccountLease, 4), fails: make(chan error, 4)}
}

func (holder *siblingHolder) hold(accountID string) {
	lease, err := holder.pool.AcquireFor(context.Background(), aistudio.AccountSelection{AccountID: accountID})
	if err != nil {
		holder.fails <- err
		return
	}
	holder.held <- lease
}

// lease 返回已占住的槽位
func (holder *siblingHolder) lease(t *testing.T) *aistudio.AccountLease {
	t.Helper()
	select {
	case lease := <-holder.held:
		return lease
	case err := <-holder.fails:
		t.Fatalf("占住同账号槽位失败: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("上游没有收到失败的调用")
	}
	return nil
}

// tryLease 在 timeout 内取得指定账户的租约，取得后立即释放
func tryLease(pool *aistudio.AccountPool, accountID string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	lease, err := pool.AcquireFor(ctx, aistudio.AccountSelection{AccountID: accountID})
	if err != nil {
		return err
	}
	return lease.Release()
}

// pendingResets 返回登记中的后台关闭数
func pendingResets(manager *accountWorkerManager) int {
	manager.resetMu.Lock()
	defer manager.resetMu.Unlock()
	return len(manager.resets)
}

// waitResetsSettled 等全部后台关闭结束
func waitResetsSettled(t *testing.T, manager *accountWorkerManager) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for pendingResets(manager) > 0 {
		if time.Now().After(deadline) {
			t.Fatal("后台关闭没有结束")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func currentWorker(manager *accountWorkerManager, accountID string) *aistudio.NativeWorker {
	manager.mu.RLock()
	account := manager.accounts[accountID]
	manager.mu.RUnlock()
	account.mu.Lock()
	defer account.mu.Unlock()
	return account.worker
}

// returnsWithin 断言 call 在 limit 内返回且没有错误
func returnsWithin(t *testing.T, limit time.Duration, what string, call func() error) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- call() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	case <-time.After(limit):
		t.Fatalf("%s 超过 %s 没有返回", what, limit)
	}
}

// workerResetLogged 判断管理日志里是否有包含 text 的一行
func workerResetLogged(service *trackedService, text string) bool {
	service.requests.mu.Lock()
	defer service.requests.mu.Unlock()
	for _, entry := range service.requests.logs {
		if strings.Contains(entry.Message, text) {
			return true
		}
	}
	return false
}

// TestWorkerResetTimeoutSwitchesAccount 不固定账号的请求遇到 Code 5、同账号还有请求在输出时只等一小会儿就换号成功，不再干等同账号的关闭；
// 关闭在后台继续：期间账号不发出新租约（并发槽位还有空也不行），其他请求结束后失败的 Worker 只关闭一次，账号恢复接受请求
func TestWorkerResetTimeoutSwitchesAccount(t *testing.T) {
	upstream := newWorkerFailureService(map[string]int{resetTestAlice: 1})
	service, closed := workerResetTestService(t, upstream, resetTestAlice, resetTestBob)
	manager := service.workers
	holder := newSiblingHolder(service.pool)
	upstream.beforeFail = holder.hold
	generation := manager.WorkerGeneration(resetTestAlice)

	result := awaitResetTestResult(t, startResetTestRequest(service.lifecycle, service, "req-reset-switch", ""), 10*time.Second)
	sibling := holder.lease(t)
	if result.err != nil || result.text != "hello" {
		t.Fatalf("text=%q err=%v，期望换号后成功", result.text, result.err)
	}
	if result.elapsed > 2*time.Second {
		t.Fatalf("请求用时 %s，期望只等一小会儿就换号", result.elapsed)
	}
	if calls := upstream.called(); !slices.Equal(calls, []string{resetTestAlice, resetTestBob}) {
		t.Fatalf("上游调用 = %v，期望 alice 失败后换到 bob", calls)
	}
	if !progressLogged(service, "后台继续关闭失败的 Worker，换号重试") {
		t.Fatal("请求进度应写明后台继续关闭、换号重试")
	}
	if got := closed[resetTestAlice](); got != 0 {
		t.Fatalf("同账号还有请求在输出时关闭了 %d 次 Worker", got)
	}
	if err := tryLease(service.pool, resetTestAlice, 200*time.Millisecond); err == nil {
		t.Fatal("后台关闭期间账号不应发出新租约")
	}
	if err := sibling.Release(); err != nil {
		t.Fatal(err)
	}
	waitResetsSettled(t, manager)
	if got := closed[resetTestAlice](); got != 1 {
		t.Fatalf("Worker 关闭 %d 次，期望 1 次", got)
	}
	if got := manager.WorkerGeneration(resetTestAlice); got != generation+1 {
		t.Fatalf("Worker 版本 = %d，期望 %d", got, generation+1)
	}
	if currentWorker(manager, resetTestAlice) != nil {
		t.Fatal("失败的 Worker 应已移除")
	}
	if err := tryLease(service.pool, resetTestAlice, 2*time.Second); err != nil {
		t.Fatalf("关闭结束后账号应恢复接受请求: %v", err)
	}
}

// TestWorkerResetFastPathReplaysOnSameAccount 同账号没有其他请求时关闭在等待时间内完成，照旧在原账号的新 Worker 上重放
func TestWorkerResetFastPathReplaysOnSameAccount(t *testing.T) {
	upstream := newWorkerFailureService(map[string]int{resetTestAlice: 1})
	service, closed := workerResetTestService(t, upstream, resetTestAlice, resetTestBob)
	// 使用默认等待（2 秒）：-race 下关闭也来得及完成
	service.resetWait.Store(nil)
	generation := service.workers.WorkerGeneration(resetTestAlice)

	result := awaitResetTestResult(t, startResetTestRequest(service.lifecycle, service, "req-reset-fast", ""), 10*time.Second)
	if result.err != nil || result.text != "hello" {
		t.Fatalf("text=%q err=%v，期望在原账号重放成功", result.text, result.err)
	}
	if calls := upstream.called(); !slices.Equal(calls, []string{resetTestAlice, resetTestAlice}) {
		t.Fatalf("上游调用 = %v，期望在 alice 上重放", calls)
	}
	if got := closed[resetTestAlice](); got != 1 {
		t.Fatalf("Worker 关闭 %d 次，期望 1 次", got)
	}
	if got := service.workers.WorkerGeneration(resetTestAlice); got != generation+1 {
		t.Fatalf("Worker 版本 = %d，期望 %d", got, generation+1)
	}
	if !workerResetLogged(service, "WAA Worker 重建") {
		t.Fatal("应记录 Worker 重建、重放当前请求")
	}
}

// TestPinnedRequestWaitsForWorkerReset 固定账号的请求只能用这个账号：照旧等同账号其他请求结束、关闭完成后在原账号重放
func TestPinnedRequestWaitsForWorkerReset(t *testing.T) {
	upstream := newWorkerFailureService(map[string]int{resetTestAlice: 1})
	service, closed := workerResetTestService(t, upstream, resetTestAlice, resetTestBob)
	holder := newSiblingHolder(service.pool)
	upstream.beforeFail = holder.hold

	results := startResetTestRequest(service.lifecycle, service, "req-reset-pinned", resetTestAlice)
	sibling := holder.lease(t)
	select {
	case result := <-results:
		t.Fatalf("固定账号的请求不应在关闭完成前结束: text=%q err=%v", result.text, result.err)
	case <-time.After(300 * time.Millisecond):
	}
	if calls := upstream.called(); !slices.Equal(calls, []string{resetTestAlice}) {
		t.Fatalf("上游调用 = %v，期望只在 alice 上失败一次、没有换号", calls)
	}
	if err := sibling.Release(); err != nil {
		t.Fatal(err)
	}
	result := awaitResetTestResult(t, results, 10*time.Second)
	if result.err != nil || result.text != "hello" {
		t.Fatalf("text=%q err=%v，期望关闭后在原账号重放成功", result.text, result.err)
	}
	if calls := upstream.called(); !slices.Equal(calls, []string{resetTestAlice, resetTestAlice}) {
		t.Fatalf("上游调用 = %v，期望在 alice 上重放", calls)
	}
	if got := closed[resetTestAlice](); got != 1 {
		t.Fatalf("Worker 关闭 %d 次，期望 1 次", got)
	}
}

// TestWorkerResetFallsBackWhenSwitchFindsNothing 只有原账号支持该模型时，没等到关闭就换号会找不到账号：回到原账号，
// 一直等后台关闭结束后在原账号重放，不按 Code 5 失败（原先在原账号等完关闭再重放能成功）
func TestWorkerResetFallsBackWhenSwitchFindsNothing(t *testing.T) {
	upstream := newWorkerFailureService(map[string]int{resetTestAlice: 1})
	service, closed := workerResetTestService(t, upstream, resetTestAlice, resetTestBob)
	// bob 不支持该模型，但仍是启用账号：换号次数按启用账号数计为 2
	if err := service.pool.SetCatalog(resetTestBob, aistudio.BenefitTierFree, []aistudio.Model{{
		ID: "other-model", Methods: []string{"generateContent"}, Capabilities: map[string]bool{"chat_model": true},
	}}); err != nil {
		t.Fatal(err)
	}
	holder := newSiblingHolder(service.pool)
	upstream.beforeFail = holder.hold

	results := startResetTestRequest(service.lifecycle, service, "req-reset-fallback", "")
	sibling := holder.lease(t)
	deadline := time.Now().Add(5 * time.Second)
	for !progressLogged(service, "换号没有可用账号，回到原账号等待 Worker 恢复后重放") {
		if time.Now().After(deadline) {
			t.Fatal("换号找不到账号时应回到原账号等待")
		}
		time.Sleep(10 * time.Millisecond)
	}
	select {
	case result := <-results:
		t.Fatalf("关闭完成前请求不应结束: text=%q err=%v", result.text, result.err)
	case <-time.After(100 * time.Millisecond):
	}
	if err := sibling.Release(); err != nil {
		t.Fatal(err)
	}
	result := awaitResetTestResult(t, results, 10*time.Second)
	if result.err != nil || result.text != "hello" {
		t.Fatalf("text=%q err=%v，期望回到原账号重放成功", result.text, result.err)
	}
	if calls := upstream.called(); !slices.Equal(calls, []string{resetTestAlice, resetTestAlice}) {
		t.Fatalf("上游调用 = %v，期望在 alice 上重放", calls)
	}
	if got := closed[resetTestAlice](); got != 1 {
		t.Fatalf("Worker 关闭 %d 次，期望 1 次", got)
	}
}

// TestPrimaryBudgetLimitsWorkerResetWait 主用模型的请求带排队预算：等待关闭最多 1 秒且不超过剩余预算的四分之一，
// 不在等关闭上用完预算（用完后只接受立即可用的账号，否则按 429 结束）
func TestPrimaryBudgetLimitsWorkerResetWait(t *testing.T) {
	upstream := newWorkerFailureService(map[string]int{resetTestAlice: 1})
	service, _ := workerResetTestService(t, upstream, resetTestAlice, resetTestBob)
	setWorkerResetWait(service, 5*time.Second)
	service.setPrimaryModels([]string{testRuntimeModel}, time.Second)
	holder := newSiblingHolder(service.pool)
	upstream.beforeFail = holder.hold

	result := awaitResetTestResult(t, startResetTestRequest(service.lifecycle, service, "req-reset-primary", ""), 10*time.Second)
	sibling := holder.lease(t)
	defer func() { _ = sibling.Release() }()
	if result.err != nil || result.text != "hello" {
		t.Fatalf("text=%q err=%v，期望换号后成功", result.text, result.err)
	}
	if result.elapsed > time.Second {
		t.Fatalf("请求用时 %s，期望按剩余预算的四分之一等待（约 250ms）后换号", result.elapsed)
	}
	if calls := upstream.called(); !slices.Equal(calls, []string{resetTestAlice, resetTestBob}) {
		t.Fatalf("上游调用 = %v，期望 alice 失败后换到 bob", calls)
	}
}

// TestWorkerResetWaitFor 等待上限：默认 2 秒，可改为 0（不等待）；主用模型最多 1 秒且不超过剩余预算的四分之一，预算用完时不等待
func TestWorkerResetWaitFor(t *testing.T) {
	service := &trackedService{}
	if got := service.workerResetWaitFor(nil); got != defaultWorkerResetWait {
		t.Fatalf("默认等待 = %s，期望 %s", got, defaultWorkerResetWait)
	}
	setWorkerResetWait(service, 0)
	if got := service.workerResetWaitFor(nil); got != 0 {
		t.Fatalf("设为 0 时等待 = %s，期望不等待", got)
	}
	service.resetWait.Store(nil)
	long := &leaseWaitBudget{limit: 20 * time.Second, startedAt: time.Now()}
	if got := service.workerResetWaitFor(long); got != primaryWorkerResetWait {
		t.Fatalf("预算充足时等待 = %s，期望 %s", got, primaryWorkerResetWait)
	}
	short := &leaseWaitBudget{limit: 800 * time.Millisecond, startedAt: time.Now()}
	if got := service.workerResetWaitFor(short); got > 200*time.Millisecond || got < 150*time.Millisecond {
		t.Fatalf("剩余预算 800ms 时等待 = %s，期望约 200ms", got)
	}
	expired := &leaseWaitBudget{limit: time.Second, startedAt: time.Now().Add(-2 * time.Second)}
	if got := service.workerResetWaitFor(expired); got != 0 {
		t.Fatalf("预算用完时等待 = %s，期望不等待", got)
	}
}

// TestConcurrentWorkerFailuresResetOnce 同一 Worker 版本上并发失败的请求共用一次后台关闭：Worker 只关闭一次、版本只加一，两个请求都重放成功
func TestConcurrentWorkerFailuresResetOnce(t *testing.T) {
	upstream := newWorkerFailureService(map[string]int{resetTestAlice: 2})
	// 只有一个账号：两个请求都落在 alice 上
	service, closed := workerResetTestService(t, upstream, resetTestAlice)
	arrived := make(chan struct{}, 2)
	release := make(chan struct{})
	upstream.beforeFail = func(string) {
		arrived <- struct{}{}
		<-release
	}
	generation := service.workers.WorkerGeneration(resetTestAlice)
	first := startResetTestRequest(service.lifecycle, service, "req-reset-concurrent-1", "")
	second := startResetTestRequest(service.lifecycle, service, "req-reset-concurrent-2", "")
	for range 2 {
		select {
		case <-arrived:
		case <-time.After(5 * time.Second):
			t.Fatal("两个请求没有同时落在 alice 上")
		}
	}
	close(release)
	for _, results := range []<-chan resetTestResult{first, second} {
		if result := awaitResetTestResult(t, results, 10*time.Second); result.err != nil || result.text != "hello" {
			t.Fatalf("text=%q err=%v，期望重放成功", result.text, result.err)
		}
	}
	if got := closed[resetTestAlice](); got != 1 {
		t.Fatalf("Worker 关闭 %d 次，期望同一版本只关闭一次", got)
	}
	if got := service.workers.WorkerGeneration(resetTestAlice); got != generation+1 {
		t.Fatalf("Worker 版本 = %d，期望 %d", got, generation+1)
	}
}

// TestQueuedRequestNotLeasedBeforeWorkerReset 失败请求释放租约时排队等这个账号的请求不会抢在后台关闭前取得租约：
// 释放与登记独占等待在同一次加锁内完成，排队的请求只在失败的 Worker 关闭之后才拿到账号
func TestQueuedRequestNotLeasedBeforeWorkerReset(t *testing.T) {
	upstream := newWorkerFailureService(map[string]int{resetTestAlice: 1})
	service, closed := workerResetTestService(t, upstream, resetTestAlice, resetTestBob)
	holder := newSiblingHolder(service.pool)
	type queuedLease struct {
		lease  *aistudio.AccountLease
		closed int
	}
	queued := make(chan queuedLease, 1)
	queuedWaiting := make(chan struct{})
	upstream.beforeFail = func(accountID string) {
		holder.hold(accountID)
		// 两个并发槽位都被占用：排队的请求等这个账号
		go func() {
			lease, err := service.pool.AcquireFor(context.Background(), aistudio.AccountSelection{AccountID: accountID})
			if err == nil {
				queued <- queuedLease{lease: lease, closed: closed[accountID]()}
			}
		}()
		time.Sleep(50 * time.Millisecond)
		close(queuedWaiting)
	}

	result := awaitResetTestResult(t, startResetTestRequest(service.lifecycle, service, "req-reset-queued", ""), 10*time.Second)
	<-queuedWaiting
	sibling := holder.lease(t)
	if result.err != nil || result.text != "hello" {
		t.Fatalf("text=%q err=%v，期望换号后成功", result.text, result.err)
	}
	select {
	case got := <-queued:
		_ = got.lease.Release()
		t.Fatal("后台关闭前排队的请求取得了失败账号的租约")
	case <-time.After(200 * time.Millisecond):
	}
	if err := sibling.Release(); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-queued:
		defer func() { _ = got.lease.Release() }()
		if got.closed != 1 {
			t.Fatalf("排队的请求取得租约时 Worker 关闭了 %d 次，期望已经关闭", got.closed)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("关闭结束后排队的请求应取得租约")
	}
}

// TestCanceledRequestSchedulesResetWithoutWaiting 失败时请求已经取消：只登记后台关闭、不等待（固定账号也不等），请求立即结束；
// 原先请求取消后完全不恢复，坏掉的 Worker 留到下一个请求再失败一次
func TestCanceledRequestSchedulesResetWithoutWaiting(t *testing.T) {
	upstream := newWorkerFailureService(map[string]int{resetTestAlice: 1})
	service, closed := workerResetTestService(t, upstream, resetTestAlice, resetTestBob)
	holder := newSiblingHolder(service.pool)
	ctx, cancel := context.WithCancel(service.lifecycle)
	defer cancel()
	upstream.beforeFail = func(accountID string) {
		holder.hold(accountID)
		cancel()
	}

	result := awaitResetTestResult(t, startResetTestRequest(ctx, service, "req-reset-canceled", resetTestAlice), 5*time.Second)
	sibling := holder.lease(t)
	if result.err == nil {
		t.Fatal("取消的请求应以错误结束")
	}
	if result.elapsed > time.Second {
		t.Fatalf("请求用时 %s，期望不等关闭就结束", result.elapsed)
	}
	if calls := upstream.called(); !slices.Equal(calls, []string{resetTestAlice}) {
		t.Fatalf("上游调用 = %v，期望取消后不重放", calls)
	}
	if pendingResets(service.workers) != 1 {
		t.Fatal("请求取消时仍应登记后台关闭")
	}
	if err := sibling.Release(); err != nil {
		t.Fatal(err)
	}
	waitResetsSettled(t, service.workers)
	if got := closed[resetTestAlice](); got != 1 {
		t.Fatalf("Worker 关闭 %d 次，期望 1 次", got)
	}
}

// TestWorkerResetCloseErrorSwitchesAccount 不固定账号的请求遇到关闭失败（浏览器关不掉）时换号，不按关闭的错误结束请求
func TestWorkerResetCloseErrorSwitchesAccount(t *testing.T) {
	upstream := newWorkerFailureService(map[string]int{resetTestAlice: 1})
	service, _ := workerResetTestService(t, upstream, resetTestAlice, resetTestBob)
	service.resetWait.Store(nil)
	account := service.workers.accounts[resetTestAlice]
	account.mu.Lock()
	account.worker, _ = aistudio.NewStubWorker(resetTestAlice, 1)
	account.mu.Unlock()

	result := awaitResetTestResult(t, startResetTestRequest(service.lifecycle, service, "req-reset-close-error", ""), 10*time.Second)
	if result.err != nil || result.text != "hello" {
		t.Fatalf("text=%q err=%v，期望关闭出错后换号成功", result.text, result.err)
	}
	if calls := upstream.called(); !slices.Equal(calls, []string{resetTestAlice, resetTestBob}) {
		t.Fatalf("上游调用 = %v，期望 alice 关闭出错后换到 bob", calls)
	}
	if !progressLogged(service, "关闭失败的 Worker 出错") {
		t.Fatal("请求进度应写明关闭出错后换号")
	}
}

// TestWorkerResetSurvivesClientCancel 固定账号的请求在等待关闭时取消：请求随之结束，后台关闭照常继续，其他请求结束后关闭失败的 Worker
func TestWorkerResetSurvivesClientCancel(t *testing.T) {
	upstream := newWorkerFailureService(map[string]int{resetTestAlice: 1})
	service, closed := workerResetTestService(t, upstream, resetTestAlice, resetTestBob)
	holder := newSiblingHolder(service.pool)
	upstream.beforeFail = holder.hold
	ctx, cancel := context.WithCancel(service.lifecycle)
	defer cancel()

	results := startResetTestRequest(ctx, service, "req-reset-cancel", resetTestAlice)
	sibling := holder.lease(t)
	time.Sleep(100 * time.Millisecond)
	cancel()
	if result := awaitResetTestResult(t, results, 5*time.Second); result.err == nil {
		t.Fatal("取消后的请求应以错误结束")
	}
	if got := closed[resetTestAlice](); got != 0 {
		t.Fatalf("同账号还有请求在输出时关闭了 %d 次 Worker", got)
	}
	if pendingResets(service.workers) != 1 {
		t.Fatal("请求取消后后台关闭应继续登记")
	}
	if err := sibling.Release(); err != nil {
		t.Fatal(err)
	}
	waitResetsSettled(t, service.workers)
	if got := closed[resetTestAlice](); got != 1 {
		t.Fatalf("Worker 关闭 %d 次，期望请求取消后后台照常关闭一次", got)
	}
}

// TestScheduleWorkerResetDedupAndStaleGeneration 同一账户的同一 Worker 版本只登记一次后台关闭；版本已经关闭过时直接结束，
// 不再等独占租约
func TestScheduleWorkerResetDedupAndStaleGeneration(t *testing.T) {
	service, closed := workerResetTestService(t, nil, resetTestAlice)
	manager := service.workers
	generation := manager.WorkerGeneration(resetTestAlice)
	release := holdLease(t, service.pool, resetTestAlice)
	first, err := manager.scheduleWorkerReset(resetTestAlice, generation, nil, workerResetRequest)
	if err != nil {
		t.Fatal(err)
	}
	second, err := manager.scheduleWorkerReset(resetTestAlice, generation, nil, workerResetSweep)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatal("同一版本应沿用同一次后台关闭")
	}
	select {
	case <-first.done:
		t.Fatal("还有进行中的请求时后台关闭不应结束")
	case <-time.After(100 * time.Millisecond):
	}
	release()
	select {
	case <-first.done:
	case <-time.After(5 * time.Second):
		t.Fatal("请求结束后后台关闭应完成")
	}
	if !first.reset || first.err != nil || first.expired || closed[resetTestAlice]() != 1 {
		t.Fatalf("reset=%v expired=%v err=%v 关闭=%d，期望关闭一次", first.reset, first.expired, first.err, closed[resetTestAlice]())
	}
	if pendingResets(manager) != 0 {
		t.Fatal("结束的后台关闭应移出登记表")
	}

	release = holdLease(t, service.pool, resetTestAlice)
	defer release()
	stale, err := manager.scheduleWorkerReset(resetTestAlice, generation, nil, workerResetRequest)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-stale.done:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("已经关闭过的版本不应等独占租约")
	}
	if stale.reset || !stale.replayable() {
		t.Fatalf("reset=%v replayable=%v，期望不再关闭、可以重放", stale.reset, stale.replayable())
	}
}

// TestWorkerResetBackgroundLimit 后台等不到独占租约超过上限时放弃：失败的 Worker 不关闭，账号恢复接受请求，日志写明放弃
func TestWorkerResetBackgroundLimit(t *testing.T) {
	service, closed := workerResetTestService(t, nil, resetTestAlice)
	manager := service.workers
	manager.resetLimit.Store(int64(100 * time.Millisecond))
	release := holdLease(t, service.pool, resetTestAlice)
	defer release()
	pending, err := manager.scheduleWorkerReset(resetTestAlice, manager.WorkerGeneration(resetTestAlice), nil, workerResetRequest)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-pending.done:
	case <-time.After(2 * time.Second):
		t.Fatal("超过上限后后台关闭应放弃")
	}
	if !pending.expired || pending.reset || pending.err != nil || pending.replayable() {
		t.Fatalf("expired=%v reset=%v err=%v，期望放弃且不能重放", pending.expired, pending.reset, pending.err)
	}
	if got := closed[resetTestAlice](); got != 0 {
		t.Fatalf("放弃后关闭了 %d 次 Worker", got)
	}
	if pendingResets(manager) != 0 {
		t.Fatal("放弃的后台关闭应移出登记表")
	}
	if err := tryLease(service.pool, resetTestAlice, 200*time.Millisecond); err != nil {
		t.Fatalf("放弃后账号应恢复接受请求: %v", err)
	}
	if !workerResetLogged(service, "WAA Worker 后台恢复放弃") || !workerResetLogged(service, "再有请求失败时重新登记关闭") {
		t.Fatal("应记录后台恢复放弃，并写明 Worker 状态正常时由下次失败重新登记")
	}
}

// TestWorkerResetCanceledByResetAllAndClose ResetAll 与 Close 取消进行中的后台关闭并等它们退出；ResetAll 之后可以再登记
func TestWorkerResetCanceledByResetAllAndClose(t *testing.T) {
	t.Run("ResetAll", func(t *testing.T) {
		service, closed := workerResetTestService(t, nil, resetTestAlice)
		manager := service.workers
		release := holdLease(t, service.pool, resetTestAlice)
		defer release()
		pending, err := manager.scheduleWorkerReset(resetTestAlice, manager.WorkerGeneration(resetTestAlice), nil, workerResetRequest)
		if err != nil {
			t.Fatal(err)
		}
		returnsWithin(t, 2*time.Second, "ResetAll", manager.ResetAll)
		// ResetAll 等后台 goroutine 退出后才返回：这里先不经 done 同步直接读取结果，没有等待时 -race 会报告与后台写入的数据竞争
		if !pending.expired || pending.reset {
			t.Fatalf("expired=%v reset=%v，期望被取消", pending.expired, pending.reset)
		}
		select {
		case <-pending.done:
		default:
			t.Fatal("ResetAll 返回时后台关闭应已退出")
		}
		if got := closed[resetTestAlice](); got != 1 {
			t.Fatalf("Worker 关闭 %d 次，期望由 ResetAll 关闭一次", got)
		}
		manager.resetMu.Lock()
		holds := manager.resetHolds
		manager.resetMu.Unlock()
		if holds != 0 {
			t.Fatalf("ResetAll 之后 resetHolds=%d，期望恢复登记", holds)
		}
		manager.accounts[resetTestAlice].mu.Lock()
		worker, _ := aistudio.NewStubWorker(resetTestAlice, 0)
		manager.accounts[resetTestAlice].worker = worker
		manager.accounts[resetTestAlice].mu.Unlock()
		again, err := manager.scheduleWorkerReset(resetTestAlice, manager.WorkerGeneration(resetTestAlice), nil, workerResetRequest)
		if err != nil {
			t.Fatal(err)
		}
		release()
		select {
		case <-again.done:
		case <-time.After(5 * time.Second):
			t.Fatal("ResetAll 之后登记的后台关闭应正常完成")
		}
		if !again.reset {
			t.Fatalf("ResetAll 之后登记的后台关闭 reset=%v expired=%v err=%v", again.reset, again.expired, again.err)
		}
	})
	t.Run("Close", func(t *testing.T) {
		service, _ := workerResetTestService(t, nil, resetTestAlice)
		manager := service.workers
		release := holdLease(t, service.pool, resetTestAlice)
		defer release()
		pending, err := manager.scheduleWorkerReset(resetTestAlice, manager.WorkerGeneration(resetTestAlice), nil, workerResetRequest)
		if err != nil {
			t.Fatal(err)
		}
		returnsWithin(t, 2*time.Second, "Close", manager.Close)
		// 同上：Close 返回前已等后台 goroutine 退出，直接读取结果不会与后台写入竞争
		if !pending.expired {
			t.Fatalf("expired=%v，期望被取消", pending.expired)
		}
		select {
		case <-pending.done:
		default:
			t.Fatal("Close 返回时后台关闭应已退出")
		}
		late, _ := manager.scheduleWorkerReset(resetTestAlice, manager.WorkerGeneration(resetTestAlice), nil, workerResetRequest)
		select {
		case <-late.done:
		default:
			t.Fatal("Close 之后不应再登记后台关闭")
		}
	})
}

// TestSweepFailedWorkersDoesNotBlock 失败 Worker 巡检只登记后台关闭、不等待，不推迟 keepWarm 的其余步骤；
// 后台关闭期间账号一直不接新请求（原先每轮最多等 3 秒，等不到又放开账号），重复巡检沿用同一次，其他请求结束后关闭并写日志
func TestSweepFailedWorkersDoesNotBlock(t *testing.T) {
	service, closed := workerResetTestService(t, nil, resetTestAlice)
	manager := service.workers
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// stub Worker 不生成 proof：准备失败后 Worker 进入失败状态
	if _, err := currentWorker(manager, resetTestAlice).Prepare(ctx, aistudio.ProtectedRequest{}); err == nil {
		t.Fatal("stub Worker 应准备失败")
	}
	if !manager.WorkerFailed(resetTestAlice) {
		t.Fatal("Worker 应处于失败状态")
	}
	release := holdLease(t, service.pool, resetTestAlice)
	for round := range 2 {
		startedAt := time.Now()
		manager.sweepFailedWorkers(ctx)
		if elapsed := time.Since(startedAt); elapsed > 500*time.Millisecond {
			t.Fatalf("第 %d 轮巡检用时 %s，期望只登记不等待", round+1, elapsed)
		}
		if err := tryLease(service.pool, resetTestAlice, 200*time.Millisecond); err == nil {
			t.Fatalf("第 %d 轮：后台关闭期间账号不应发出新租约", round+1)
		}
		if got := pendingResets(manager); got != 1 {
			t.Fatalf("第 %d 轮登记了 %d 次后台关闭，期望沿用同一次", round+1, got)
		}
	}
	release()
	waitResetsSettled(t, manager)
	if got := closed[resetTestAlice](); got != 1 {
		t.Fatalf("Worker 关闭 %d 次，期望 1 次", got)
	}
	if !workerResetLogged(service, "常驻 Worker 已失败，后台关闭以便重新预热") {
		t.Fatal("巡检发起的关闭完成后应写日志")
	}
}

// bidiRecoveryService 是测试用的 Live 上游：打开会话时报告 Worker 版本后按协议 Code 5 失败，记录 RecoverWAARuntime 的结果
type bidiRecoveryService struct {
	workerFailureService
	generation uint64
	recovered  chan bidiRecoveryResult
}

type bidiRecoveryResult struct {
	recovered bool
	err       error
}

func (service *bidiRecoveryService) OpenBidi(ctx context.Context, request aistudio.BidiRequest) (*aistudio.BidiSession, error) {
	request.ObserveWAARuntime(request.AccountID, service.generation)
	recovered, err := request.RecoverWAARuntime(ctx, request.AccountID, waaRuntimeFailure("BidiGenerateContent"))
	service.recovered <- bidiRecoveryResult{recovered: recovered, err: err}
	return nil, errors.New("测试会话不建立")
}

// TestBidiRecoveryWaitsForWorkerReset Live 会话的 Worker 恢复保持原来的语义：一直等同账号其他请求结束、关闭完成后返回可以重放；
// 等待中取消时返回取消错误，后台关闭照常继续
func TestBidiRecoveryWaitsForWorkerReset(t *testing.T) {
	for _, cancelEarly := range []bool{false, true} {
		name := "等待关闭完成"
		if cancelEarly {
			name = "等待中取消"
		}
		t.Run(name, func(t *testing.T) {
			upstream := &bidiRecoveryService{recovered: make(chan bidiRecoveryResult, 1)}
			service, closed := workerResetTestService(t, upstream, resetTestAlice)
			service.state.Store(serviceRunning)
			service.dataContext = service.lifecycle
			upstream.generation = service.workers.WorkerGeneration(resetTestAlice)
			release := holdLease(t, service.pool, resetTestAlice)
			ctx, cancel := context.WithCancel(service.lifecycle)
			defer cancel()
			opened := make(chan struct{})
			go func() {
				defer close(opened)
				_, _ = service.OpenBidi(ctx, aistudio.BidiRequest{Model: testRuntimeModel, AccountID: resetTestAlice})
			}()
			select {
			case result := <-upstream.recovered:
				t.Fatalf("关闭完成前恢复不应返回: %+v", result)
			case <-time.After(300 * time.Millisecond):
			}
			if cancelEarly {
				cancel()
			} else {
				release()
			}
			var result bidiRecoveryResult
			select {
			case result = <-upstream.recovered:
			case <-time.After(5 * time.Second):
				t.Fatal("恢复没有返回")
			}
			<-opened
			if cancelEarly {
				if !errors.Is(result.err, context.Canceled) || result.recovered {
					t.Fatalf("recovered=%v err=%v，期望返回取消错误", result.recovered, result.err)
				}
				release()
			} else if !result.recovered || result.err != nil {
				t.Fatalf("recovered=%v err=%v，期望关闭完成后可以重放", result.recovered, result.err)
			}
			waitResetsSettled(t, service.workers)
			if got := closed[resetTestAlice](); got != 1 {
				t.Fatalf("Worker 关闭 %d 次，期望 1 次", got)
			}
		})
	}
}
