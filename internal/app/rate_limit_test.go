package app

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// rateUpstream 是每分钟限速测试的上游：像 WorkerProtectedTransport 一样在发送时为请求的租约记一次生成请求，记录调用的账号，正常返回
type rateUpstream struct {
	mu    sync.Mutex
	calls []string
}

func (upstream *rateUpstream) Models(context.Context) ([]aistudio.Model, error) {
	return nil, nil
}

func (upstream *rateUpstream) CountTokens(context.Context, aistudio.TokenCountRequest) (aistudio.TokenCount, error) {
	return aistudio.TokenCount{}, nil
}

func (upstream *rateUpstream) Generate(ctx context.Context, request aistudio.GenerateRequest) (<-chan aistudio.Event, error) {
	if lease, ok := aistudio.AccountLeaseFromContext(ctx); ok {
		lease.RecordGenerationSent()
	}
	upstream.mu.Lock()
	upstream.calls = append(upstream.calls, request.AccountID)
	upstream.mu.Unlock()
	events := make(chan aistudio.Event, 2)
	events <- aistudio.Event{Kind: aistudio.EventText, Text: "hello"}
	events <- aistudio.Event{Kind: aistudio.EventFinish, FinishReason: "STOP"}
	close(events)
	return events, nil
}

func (upstream *rateUpstream) called() []string {
	upstream.mu.Lock()
	defer upstream.mu.Unlock()
	return slices.Clone(upstream.calls)
}

// rateTestService 返回纯 Go 后端（启动立即返回 stub Worker）的生成服务：普通分区常驻 warm、峰值 maxActive，上游为 rateUpstream
func rateTestService(t *testing.T, accounts []string, warm int, maxActive int) (*trackedService, *rateUpstream) {
	t.Helper()
	manager, pool, requests := partitionTestManager(t, accounts, nil, warm, maxActive, 0, 1)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	upstream := &rateUpstream{}
	service := retireTestService(ctx, manager, pool, requests)
	service.service = upstream
	service.timeout.Store(int64(time.Minute))
	return service, upstream
}

// fillRateWindow 用生成租约为账户记 sends 次发送并释放：上限不超过 sends 时账户的每分钟窗口随即占满（窗口 60 秒，测试内不会过期）
func fillRateWindow(t *testing.T, pool *aistudio.AccountPool, accountID string, sends int) {
	t.Helper()
	for range sends {
		lease, err := pool.AcquireFor(context.Background(), aistudio.AccountSelection{
			AccountID: accountID, ModelID: testRuntimeModel, Method: "generateContent",
		})
		if err != nil {
			t.Fatal(err)
		}
		lease.RecordGenerationSent()
		if err := lease.Release(); err != nil {
			t.Fatal(err)
		}
	}
}

// progressLoggedAll 判断请求进度日志里是否有一条同时包含全部片段的记录
func progressLoggedAll(service *trackedService, fragments ...string) bool {
	service.requests.mu.Lock()
	defer service.requests.mu.Unlock()
	for _, entry := range service.requests.logs {
		if entry.Event != "request.progress" {
			continue
		}
		matched := true
		for _, fragment := range fragments {
			matched = matched && strings.Contains(entry.Message, fragment)
		}
		if matched {
			return true
		}
	}
	return false
}

// rateLimitedCount 返回生成选择分类时因每分钟请求已满按忙碌分组的候选数
func rateLimitedCount(t *testing.T, pool *aistudio.AccountPool, warm []string) int {
	t.Helper()
	groups, err := pool.ClassifyCandidates(context.Background(), aistudio.AccountSelection{ModelID: testRuntimeModel, Method: "generateContent"}, warm)
	if err != nil {
		t.Fatal(err)
	}
	return groups.RateLimited
}

// TestRateLimitedWarmWorkersTriggerExpansion 热账号 A 每分钟请求已满、备用账号 B 空闲且分区有空位：非主用模型的请求把 A 当作忙碌，
// 为 B 后台扩容，最后由 B 服务，A 没有收到请求
func TestRateLimitedWarmWorkersTriggerExpansion(t *testing.T) {
	service, upstream := rateTestService(t, []string{partitionNormalA, partitionNormalB}, 1, 2)
	ctx, cancel := context.WithTimeout(service.lifecycle, 20*time.Second)
	defer cancel()
	if _, err := service.workers.promote(ctx, partitionNormalA, testRuntimeModel); err != nil {
		t.Fatal(err)
	}
	service.pool.SetAccountRateLimit(1)
	fillRateWindow(t, service.pool, partitionNormalA, 1)
	if got := rateLimitedCount(t, service.pool, service.workers.ReadyWarmAccountIDs()); got != 1 {
		t.Fatalf("每分钟已满的候选数 = %d，期望 1", got)
	}
	text, err, _ := runScopedPrimaryRequest(t, service, aistudio.PoolScopeNormal, "req-rate-expand", "")
	if err != nil || text != "hello" {
		t.Fatalf("应由扩容出来的 B 完成：text=%q err=%v", text, err)
	}
	if calls := upstream.called(); !slices.Equal(calls, []string{partitionNormalB}) {
		t.Fatalf("上游调用 = %v，期望只有 B", calls)
	}
	if !slices.Contains(service.workers.WarmAccountIDs(), partitionNormalB) {
		t.Fatalf("B 应已后台扩容，驻留 Worker = %v", service.workers.WarmAccountIDs())
	}
	if !hasLogContaining(service.requests, "按需扩容") {
		t.Fatalf("应记录按需扩容，日志 = %v", serviceLogMessages(service.requests))
	}
}

// TestRateLimitedPartitionFullQueuesAsBusy 分区已满（峰值 1）、驻留的 A 每分钟请求已满：非主用模型的请求按“账号全部忙碌”排队，
// 进度写明每分钟已满的候选数，调大上限后立即被唤醒完成；主用模型受排队预算限制，到期按号池繁忙返回，不按额度不足或全部冷却
func TestRateLimitedPartitionFullQueuesAsBusy(t *testing.T) {
	service, upstream := rateTestService(t, []string{partitionNormalA, partitionNormalB}, 1, 1)
	ctx, cancel := context.WithTimeout(service.lifecycle, 20*time.Second)
	defer cancel()
	if _, err := service.workers.promote(ctx, partitionNormalA, testRuntimeModel); err != nil {
		t.Fatal(err)
	}
	service.pool.SetAccountRateLimit(1)
	fillRateWindow(t, service.pool, partitionNormalA, 1)

	type result struct {
		text string
		err  error
	}
	done := make(chan result, 1)
	go func() {
		text, err, _ := runScopedPrimaryRequest(t, service, aistudio.PoolScopeNormal, "req-rate-queued", "")
		done <- result{text, err}
	}()
	queued := func() bool {
		return progressLoggedAll(service, "排队等待 | 原因=账号全部忙碌", "| 每分钟已满=1")
	}
	if !waitUntil(5*time.Second, queued) {
		t.Fatal("排队进度应写明账号全部忙碌与每分钟已满=1")
	}
	select {
	case got := <-done:
		t.Fatalf("窗口释放前请求不应结束：%+v", got)
	default:
	}
	raisedAt := time.Now()
	service.pool.SetAccountRateLimit(2)
	select {
	case got := <-done:
		if got.err != nil || got.text != "hello" {
			t.Fatalf("调大上限后应完成：text=%q err=%v", got.text, got.err)
		}
		if waited := time.Since(raisedAt); waited > 1500*time.Millisecond {
			t.Fatalf("调大上限后 %s 才完成，期望立即被唤醒", waited)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("调大上限后请求没有完成")
	}
	if calls := upstream.called(); !slices.Equal(calls, []string{partitionNormalA}) {
		t.Fatalf("上游调用 = %v，期望由 A 完成", calls)
	}

	// A 已发出 2 次，按上限 2 又满了：主用模型排队到上限后按号池繁忙返回
	service.setPrimaryModels([]string{testRuntimeModel}, 200*time.Millisecond)
	_, err, elapsed := runScopedPrimaryRequest(t, service, aistudio.PoolScopeNormal, "req-rate-primary", "")
	var busy *aistudio.CapacityBusyError
	var quota *aistudio.PrimaryQuotaError
	var cooling *aistudio.AllCoolingError
	if !errors.As(err, &busy) || errors.As(err, &quota) || errors.As(err, &cooling) || elapsed < 200*time.Millisecond {
		t.Fatalf("主用模型 err=%v 用时 %s，期望排队约 200ms 后返回号池繁忙", err, elapsed)
	}
	if busy.Reason != "账号全部忙碌" {
		t.Fatalf("号池繁忙原因 = %q，期望账号全部忙碌", busy.Reason)
	}
	if calls := upstream.called(); len(calls) != 1 {
		t.Fatalf("主用模型不应发到上游，上游调用 = %v", calls)
	}
}

// TestUltraPrimaryRateLimitedNoQuotaHit Ultra 热 Worker 全部每分钟请求已满、没有备用账号：Ultra 主用模型按“账号全部忙碌”排队到预算，
// 返回号池繁忙，不计额度失败、不写额度进度，也不发到上游
func TestUltraPrimaryRateLimitedNoQuotaHit(t *testing.T) {
	accounts := ultraTestIDs(2)
	upstream := &rateUpstream{}
	service, pool := primaryBusyService(t, upstream, accounts...)
	markUltra(t, pool, accounts...)
	pool.SetAccountRateLimit(1)
	for _, accountID := range accounts {
		fillRateWindow(t, pool, accountID, 1)
	}
	setUltraPrimaryModels(service, []string{testRuntimeModel}, 200*time.Millisecond)
	_, err, elapsed := runScopedPrimaryRequest(t, service, aistudio.PoolScopeUltra, "req-ultra-rate", "")
	var busy *aistudio.CapacityBusyError
	var quota *aistudio.PrimaryQuotaError
	if !errors.As(err, &busy) || errors.As(err, &quota) || elapsed < 200*time.Millisecond {
		t.Fatalf("err=%v 用时 %s，期望排队约 200ms 后返回号池繁忙", err, elapsed)
	}
	if calls := upstream.called(); len(calls) != 0 {
		t.Fatalf("上游调用 = %v，期望没有", calls)
	}
	for _, fragment := range []string{"额度 429", "限流 429", "Ultra 主用模型额度不足", "候选全部冷却"} {
		if progressLogged(service, fragment) {
			t.Fatalf("每分钟已满不应写 %q 的进度", fragment)
		}
	}
	if !progressLogged(service, "主用模型排队超过 200ms，立即返回 429 | 原因=账号全部忙碌") {
		t.Fatal("请求进度应写明按账号全部忙碌排队超限")
	}
	assertNoBusyAccounts(t, pool)
}

// TestRateLimitedWorkerNotEvictedOrRetired 每分钟请求已满不写冷却表：空闲的 A 在淘汰选号里不算冷却或死槽位，不被补登记淘汰；
// 对照的 B 全局长时间冷却照常被选中与登记
func TestRateLimitedWorkerNotEvictedOrRetired(t *testing.T) {
	service, _ := rateTestService(t, []string{partitionNormalA, partitionNormalB}, 2, 2)
	ctx, cancel := context.WithTimeout(service.lifecycle, 20*time.Second)
	defer cancel()
	for _, accountID := range []string{partitionNormalA, partitionNormalB} {
		if _, err := service.workers.promote(ctx, accountID, testRuntimeModel); err != nil {
			t.Fatal(err)
		}
	}
	service.setPrimaryModels([]string{testRuntimeModel}, 10*time.Second)
	service.pool.SetAccountRateLimit(1)
	fillRateWindow(t, service.pool, partitionNormalA, 1)
	if got := rateLimitedCount(t, service.pool, service.workers.ReadyWarmAccountIDs()); got != 1 {
		t.Fatalf("每分钟已满的候选数 = %d，期望 1", got)
	}
	states := service.pool.WarmWorkerStates([]string{partitionNormalA}, testRuntimeModel, rotationMinRemaining)
	if state := states[partitionNormalA]; state.Busy || state.Cooling || state.Dead {
		t.Fatalf("每分钟已满的 A 状态 = %+v，期望空闲、不算冷却或死槽位", state)
	}
	if service.pool.AccountCoolingDown(partitionNormalA, testRuntimeModel) {
		t.Fatal("每分钟已满不应写冷却表")
	}
	if got := service.workers.idleWarmVictimFor("", testRuntimeModel, true, aistudio.PoolScopeNormal); got != "" {
		t.Fatalf("只选冷却账号时不应选每分钟已满的 A，实际 %q", got)
	}
	if added := service.registerDeadWorkers(); added != 0 || service.workers.retirementCount() != 0 {
		t.Fatalf("每分钟已满不应补登记淘汰：新增 %d，登记数 %d", added, service.workers.retirementCount())
	}
	coolGlobally(t, service.pool, partitionNormalB)
	if got := service.workers.idleWarmVictimFor("", testRuntimeModel, true, aistudio.PoolScopeNormal); got != partitionNormalB {
		t.Fatalf("只选冷却账号时应选长时间冷却的 B，实际 %q", got)
	}
	if added := service.registerDeadWorkers(); added != 1 || service.workers.retirementCount() != 1 {
		t.Fatalf("应只补登记 B：新增 %d，登记数 %d", added, service.workers.retirementCount())
	}
}

// TestRateWindowFilledLoggedOnce 同一账号 10 分钟内两次占满每分钟窗口只写一条“账号每分钟请求已满”
func TestRateWindowFilledLoggedOnce(t *testing.T) {
	service, upstream := rateTestService(t, []string{partitionNormalA}, 1, 1)
	ctx, cancel := context.WithTimeout(service.lifecycle, 20*time.Second)
	defer cancel()
	if _, err := service.workers.promote(ctx, partitionNormalA, testRuntimeModel); err != nil {
		t.Fatal(err)
	}
	service.pool.SetAccountRateLimit(1)
	if text, err, _ := runScopedPrimaryRequest(t, service, aistudio.PoolScopeNormal, "req-rate-first", ""); err != nil || text != "hello" {
		t.Fatalf("第一次请求 text=%q err=%v", text, err)
	}
	// 调大上限后第二次请求又占满窗口（已发出 1 + 预占 1 = 2）
	service.pool.SetAccountRateLimit(2)
	if text, err, _ := runScopedPrimaryRequest(t, service, aistudio.PoolScopeNormal, "req-rate-second", ""); err != nil || text != "hello" {
		t.Fatalf("第二次请求 text=%q err=%v", text, err)
	}
	if calls := upstream.called(); len(calls) != 2 {
		t.Fatalf("上游调用 = %v，期望 2 次", calls)
	}
	count := 0
	for _, message := range serviceLogMessages(service.requests) {
		if strings.Contains(message, "账号每分钟请求已满") {
			count++
			if !strings.Contains(message, "上限=1 次/60 秒") {
				t.Fatalf("日志 %q 应写明第一次占满时的上限", message)
			}
		}
	}
	if count != 1 {
		t.Fatalf("“账号每分钟请求已满”日志 %d 条，期望 1 条", count)
	}
	if got := rateLimitedCount(t, service.pool, service.workers.ReadyWarmAccountIDs()); got != 1 {
		t.Fatalf("两次发送后按上限 2 应已满，候选数 = %d", got)
	}
}

// TestRateWindowFilledLoggedAfterSilentFill 不写日志的路径（转写经 AcquireFor 自取的生成租约）先占满过窗口、没有发出就释放后，
// 聊天请求再占满窗口时照常写一条“账号每分钟请求已满”：10 分钟的名额只在写日志时占用
func TestRateWindowFilledLoggedAfterSilentFill(t *testing.T) {
	service, upstream := rateTestService(t, []string{partitionNormalA}, 1, 1)
	ctx, cancel := context.WithTimeout(service.lifecycle, 20*time.Second)
	defer cancel()
	if _, err := service.workers.promote(ctx, partitionNormalA, testRuntimeModel); err != nil {
		t.Fatal(err)
	}
	service.pool.SetAccountRateLimit(1)
	// 与转写相同的选号（context 里没有租约时 resolveAccountLease 经 AcquireFor 自取）：租约占满窗口，没有人写日志
	silent, err := service.pool.AcquireFor(ctx, aistudio.AccountSelection{ModelID: testRuntimeModel, Method: "generateContent"})
	if err != nil {
		t.Fatal(err)
	}
	if err := silent.Release(); err != nil {
		t.Fatal(err)
	}
	if text, err, _ := runScopedPrimaryRequest(t, service, aistudio.PoolScopeNormal, "req-rate-after-silent", ""); err != nil || text != "hello" {
		t.Fatalf("聊天请求 text=%q err=%v", text, err)
	}
	if calls := upstream.called(); len(calls) != 1 {
		t.Fatalf("上游调用 = %v，期望 1 次", calls)
	}
	count := 0
	for _, message := range serviceLogMessages(service.requests) {
		if strings.Contains(message, "账号每分钟请求已满") {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("“账号每分钟请求已满”日志 %d 条，期望 1 条", count)
	}
}

// TestRateWindowReleaseKeepsQueueOrder 每分钟窗口按时间释放时号池发调度信号，先排队的请求先取得名额：单账号 A、上限 1，
// R1 在窗口满时排队，R2 在窗口释放后 600ms 到达（这时直接 TryAcquireFor，不看排队序）。R1 在窗口释放时即被唤醒完成，R2 不能抢先，
// 要等 R1 那次发送的窗口释放。R1 的兜底复查（schedulingRecheck，约每 1s 一次）不落在窗口释放到 R2 到达之间，
// 没有窗口释放的调度信号时 R1 要等到约 3s 的兜底复查，R2 先完成
func TestRateWindowReleaseKeepsQueueOrder(t *testing.T) {
	const window = 2200 * time.Millisecond
	const r2Arrival = 2800 * time.Millisecond
	service, upstream := rateTestService(t, []string{partitionNormalA}, 1, 1)
	ctx, cancel := context.WithTimeout(service.lifecycle, 20*time.Second)
	defer cancel()
	if _, err := service.workers.promote(ctx, partitionNormalA, testRuntimeModel); err != nil {
		t.Fatal(err)
	}
	service.pool.SetRateWindowForTest(window)
	service.pool.SetAccountRateLimit(1)
	filledAt := time.Now()
	fillRateWindow(t, service.pool, partitionNormalA, 1)
	releaseAt := time.Now().Add(window)

	type result struct {
		id   string
		at   time.Time
		text string
		err  error
	}
	done := make(chan result, 2)
	run := func(id string) {
		text, err, _ := runScopedPrimaryRequest(t, service, aistudio.PoolScopeNormal, id, "")
		done <- result{id, time.Now(), text, err}
	}
	// R1 立即到达并排队（窗口满），兜底复查约在 1s、2s、3s：窗口约在 2.2s 释放、R2 在 2.8s 到达，其间没有兜底复查
	go run("req-rate-r1")
	time.Sleep(time.Until(filledAt.Add(r2Arrival)))
	go run("req-rate-r2")
	var order []result
	for range 2 {
		select {
		case got := <-done:
			if got.err != nil || got.text != "hello" {
				t.Fatalf("%s text=%q err=%v", got.id, got.text, got.err)
			}
			order = append(order, got)
		case <-time.After(10 * time.Second):
			t.Fatalf("请求没有全部完成，已完成 %v", order)
		}
	}
	if order[0].id != "req-rate-r1" {
		t.Fatalf("完成顺序 = [%s %s]，期望先排队的 R1 先完成", order[0].id, order[1].id)
	}
	// 兜底复查约在窗口释放后 800ms，600ms 内完成说明是窗口释放时唤醒的
	if late := order[0].at.Sub(releaseAt); late > 600*time.Millisecond {
		t.Fatalf("R1 在窗口释放后 %s 才完成，期望窗口释放时即被唤醒", late)
	}
	if calls := upstream.called(); len(calls) != 2 {
		t.Fatalf("上游调用 = %v，期望 2 次", calls)
	}
}
