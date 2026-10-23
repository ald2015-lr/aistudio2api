package app

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// TestBadAccountMemoryStreakAndPersistence 按账号连续 2 次裸限流、期间没有成功就标坏号（写一条日志）；成功一次即清除；
// 候选整理去掉坏号、近期成功的排前，全是坏号时原样返回；记录写盘后重启（重新打开）仍在，过期的丢掉
func TestBadAccountMemoryStreakAndPersistence(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	requests := newRequestRegistry(ctx)
	path := filepath.Join(t.TempDir(), badAccountFileName)
	now := time.Unix(2_000_000_000, 0)
	clock := func() time.Time { return now }
	memory := openBadAccountMemory(path, requests)
	memory.now = clock
	memory.observe("bad@example.com", "bad", egressBare)
	if memory.bad("bad@example.com") {
		t.Fatal("一次裸限流不应标坏号")
	}
	memory.observe("bad@example.com", "bad", egressOther)
	memory.observe("bad@example.com", "bad", egressBare)
	if !memory.bad("bad@example.com") || memory.count() != 1 {
		t.Fatal("连续 2 次裸限流（中间的其他失败不算成功）应标坏号")
	}
	if !hasLogContaining(requests, "账号连续裸限流，移出预热与替补候选 | 连续=2 | 时长=2h0m0s | 恢复=") {
		t.Fatalf("应记录标坏号，日志 = %v", serviceLogMessages(requests))
	}
	memory.observe("good@example.com", "good", egressSuccess)
	memory.observe("flip@example.com", "flip", egressBare)
	memory.observe("flip@example.com", "flip", egressSuccess)
	memory.observe("flip@example.com", "flip", egressBare)
	if memory.bad("flip@example.com") {
		t.Fatal("中间有成功时连续次数应清零")
	}
	ordered := memory.order([]string{"bad@example.com", "neutral@example.com", "good@example.com"})
	if !slices.Equal(ordered, []string{"good@example.com", "neutral@example.com"}) {
		t.Fatalf("候选整理 = %v，期望近期成功的在前、坏号去掉", ordered)
	}
	if only := memory.order([]string{"bad@example.com"}); !slices.Equal(only, []string{"bad@example.com"}) {
		t.Fatalf("全是坏号时应原样返回，实际 %v", only)
	}
	if err := memory.close(); err != nil {
		t.Fatal(err)
	}
	reopened := openBadAccountMemory(path, requests)
	reopened.now = clock
	if !reopened.bad("bad@example.com") || reopened.bad("flip@example.com") {
		t.Fatal("重启后坏号记录应保留")
	}
	if ordered := reopened.order([]string{"neutral@example.com", "good@example.com"}); ordered[0] != "good@example.com" {
		t.Fatalf("重启后近期成功记录应保留，候选整理 = %v", ordered)
	}
	// 坏号到期后（2 小时）重新打开：不再是坏号
	later := openBadAccountMemory(path, requests)
	later.now = func() time.Time { return now.Add(badAccountHold + time.Minute) }
	if later.bad("bad@example.com") {
		t.Fatal("到期的坏号记录应丢掉")
	}
	reopened.observe("bad@example.com", "bad", egressSuccess)
	if reopened.bad("bad@example.com") {
		t.Fatal("成功一次应清除坏号")
	}
}

// TestBadAccountsSavedOnStop 停止服务时立即写出坏号记忆（不等 30 秒的合并写盘）：重建运行时先停旧服务、再建新一代（新一代此时读文件），
// 最后才关闭旧一代，原先新一代读到的是旧数据
func TestBadAccountsSavedOnStop(t *testing.T) {
	manager, pool, requests := partitionTestManager(t, []string{partitionNormalA}, nil, 1, 1, 0, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	service := retireTestService(ctx, manager, pool, requests)
	path := filepath.Join(t.TempDir(), badAccountFileName)
	memory := openBadAccountMemory(path, requests)
	manager.badAccounts.Store(memory)
	t.Cleanup(func() { _ = memory.close() })
	memory.observe(partitionNormalA, "", egressBare)
	memory.observe(partitionNormalA, "", egressBare)
	if next := openBadAccountMemory(path, requests); next.bad(partitionNormalA) {
		t.Fatal("停止前还没到合并写盘的时间，文件里不应已有记录")
	}
	modelRefreshDone := make(chan struct{})
	close(modelRefreshDone)
	service.finishStop(make(chan struct{}), modelRefreshDone)
	if next := openBadAccountMemory(path, requests); !next.bad(partitionNormalA) {
		t.Fatal("停止服务后新一代应读到坏号记录")
	}
}

// TestBadAccountsSkippedByPrewarm 预热不选坏号（还有其他候选时），近期成功过的账号优先
func TestBadAccountsSkippedByPrewarm(t *testing.T) {
	manager, _, requests := partitionTestManager(t, []string{partitionNormalA, partitionNormalB, partitionNormalC}, nil, 1, 3, 0, 1)
	memory := openBadAccountMemory("", requests)
	memory.observe(partitionNormalA, "", egressBare)
	memory.observe(partitionNormalA, "", egressBare)
	memory.observe(partitionNormalC, "", egressSuccess)
	manager.badAccounts.Store(memory)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := <-manager.StartPrewarm(ctx); err != nil {
		t.Fatal(err)
	}
	if warm := sortedWarm(manager); !slices.Equal(warm, []string{partitionNormalC}) {
		t.Fatalf("预热应选近期成功过的 C、不选坏号 A，驻留 Worker = %v", warm)
	}
	if got := manager.withoutBadAccounts([]string{partitionNormalA, partitionNormalB}); !slices.Equal(got, []string{partitionNormalB}) {
		t.Fatalf("按需提升与扩容的候选应去掉坏号，实际 %v", got)
	}
}

// TestEgressFreshRejectsExcludeBadAccounts 出口判定的“冷号首发被拒”不计已标坏号的账号
func TestEgressFreshRejectsExcludeBadAccounts(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	guard := newEgressGuard(nil, newRequestRegistry(ctx))
	bad := map[string]bool{"bad@example.com": true}
	guard.badAccount = func(accountID string) bool { return bad[accountID] }
	now := time.Now()
	guard.observeOutcome(egressObservation{Key: "", AccountID: "bad@example.com", Outcome: egressBare, At: now})
	guard.observeOutcome(egressObservation{Key: "", AccountID: "fresh@example.com", Outcome: egressBare, At: now})
	status := guard.status()
	if len(status) != 1 || status[0].FreshRejects2m != 1 || status[0].Bare1m != 2 {
		t.Fatalf("出口状态 = %+v，期望裸 429 计 2 次、冷号首发只计不是坏号的 1 次", status)
	}
}

// TestReplaceVictimConsistency 现场提升判断“能否替换”与按需替换用同一个口径（N3）：同分区有 Worker 在启动时，对请求模型不可服务的
// 健康空闲 Worker 也可以换掉，两处结论一致；能服务、刚用过的健康 Worker 两处都不换。ensureWorker 据此换掉它，不再报 Worker 容量已满
func TestReplaceVictimConsistency(t *testing.T) {
	const otherModel = "other-model"
	manager, pool, _ := partitionTestManager(t, []string{partitionNormalA, partitionNormalB}, nil, 1, 2, 0, 1)
	if err := pool.SetCatalog(partitionNormalB, aistudio.BenefitTierFree, []aistudio.Model{
		{ID: testRuntimeModel, Methods: []string{"generateContent"}, Capabilities: map[string]bool{"chat_model": true}},
		{ID: otherModel, Methods: []string{"generateContent"}, Capabilities: map[string]bool{"chat_model": true}},
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := manager.promote(ctx, partitionNormalA, testRuntimeModel); err != nil {
		t.Fatal(err)
	}
	releaseLease := holdLease(t, pool, partitionNormalA)
	releaseLease()
	// 模拟同分区另一个账号正在启动（占一个槽位，分区已满）
	const opening = "opening@example.com"
	manager.rebalanceMu.Lock()
	manager.openings[opening] = make(chan struct{})
	manager.openingSet.Store(opening, struct{}{})
	manager.rebalanceMu.Unlock()
	defer func() {
		manager.rebalanceMu.Lock()
		delete(manager.openings, opening)
		manager.openingSet.Delete(opening)
		manager.rebalanceMu.Unlock()
	}()
	normal := aistudio.PoolScopeNormal
	if victim := manager.replaceVictimFor(partitionNormalB, otherModel, normal); victim != partitionNormalA {
		t.Fatalf("A 对 %s 不可服务，有 Worker 在启动时也应可替换，实际 %q", otherModel, victim)
	}
	other := manager.standbyCapacity(manager.WarmAccountIDs(), otherModel)
	if promoted := other.promotable([]string{partitionNormalB}, true); promoted != partitionNormalB {
		t.Fatalf("现场提升应与替换口径一致、选 B，实际 %q", promoted)
	}
	if victim := manager.replaceVictimFor(partitionNormalB, testRuntimeModel, normal); victim != "" {
		t.Fatalf("A 能服务 %s、刚用过，有 Worker 在启动时不应替换，实际 %q", testRuntimeModel, victim)
	}
	same := manager.standbyCapacity(manager.WarmAccountIDs(), testRuntimeModel)
	if promoted := same.promotable([]string{partitionNormalB}, true); promoted != "" {
		t.Fatalf("没有可替换的 Worker 时现场提升也不应选号，实际 %q", promoted)
	}
	if _, err := manager.ensureWorker(ctx, partitionNormalB, otherModel, false); err != nil {
		t.Fatalf("按需替换应换掉不可服务的 A，实际 %v", err)
	}
	if warm := sortedWarm(manager); !slices.Equal(warm, []string{partitionNormalB}) {
		t.Fatalf("驻留 Worker = %v，期望 B 换下 A", warm)
	}
}

// TestReplaceVictimWarmupGrace 按需替换与扩容不换刚就绪不到 rotationWarmupGrace 的 Worker（N6），不可调度的除外
func TestReplaceVictimWarmupGrace(t *testing.T) {
	manager, pool, _ := partitionTestManager(t, []string{partitionNormalA, partitionNormalB}, nil, 1, 1, 0, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := manager.promote(ctx, partitionNormalA, testRuntimeModel); err != nil {
		t.Fatal(err)
	}
	normal := aistudio.PoolScopeNormal
	if victim := manager.pickReplaceVictim("", testRuntimeModel, victimAny, normal); victim != partitionNormalA {
		t.Fatalf("没有宽限时应可替换 A，实际 %q", victim)
	}
	manager.replaceGrace.Store(int64(rotationWarmupGrace))
	if victim := manager.pickReplaceVictim("", testRuntimeModel, victimAny, normal); victim != "" {
		t.Fatalf("刚就绪的 A 在宽限内不应被替换，实际 %q", victim)
	}
	if victim := manager.pickIdleVictim("", testRuntimeModel, victimAny, normal); victim != partitionNormalA {
		t.Fatalf("宽限只作用于替换与扩容，其余淘汰照旧，实际 %q", victim)
	}
	if err := pool.MarkAuthRequired(partitionNormalA, "HTTP 401"); err != nil {
		t.Fatal(err)
	}
	if victim := manager.pickReplaceVictim("", testRuntimeModel, victimAny, normal); victim != partitionNormalA {
		t.Fatalf("不可调度的 Worker 不受宽限，实际 %q", victim)
	}
}

// TestReplacementRatePerMinute 每个分区每分钟的按需替换不超过上限（N6），预热补位不受限；没有设上限时（测试构造的管理器）不限
func TestReplacementRatePerMinute(t *testing.T) {
	manager, _, _ := partitionTestManager(t, []string{partitionNormalA}, nil, 1, 1, 0, 1)
	ctx := context.Background()
	normal := aistudio.PoolScopeNormal
	for range 20 {
		manager.noteReplacement(ctx, normal)
	}
	if !manager.replacementAllowed(ctx, normal) {
		t.Fatal("没有设上限时不应限制")
	}
	manager.replacements.limit.Store(2)
	manager.noteReplacement(ctx, normal)
	manager.noteReplacement(ctx, normal)
	if manager.replacementAllowed(ctx, normal) {
		t.Fatal("达到上限后不应再替换")
	}
	if !manager.replacementAllowed(ctx, aistudio.PoolScopeUltra) {
		t.Fatal("上限按分区计")
	}
	if !manager.replacementAllowed(withWorkerStartSource(ctx, workerStartPrewarm), normal) {
		t.Fatal("预热补位不受上限限制")
	}
	manager.replacements.mu.Lock()
	for index := range manager.replacements.times[normal] {
		manager.replacements.times[normal][index] = time.Now().Add(-2 * replacementWindow)
	}
	manager.replacements.mu.Unlock()
	if !manager.replacementAllowed(ctx, normal) {
		t.Fatal("窗口外的替换不再计数")
	}
}

// TestPrimaryBudgetCreditsOwnExpansion 主用模型等这次选号发起的后台扩容（N4）：等待的时间延后截止时间（与现场冷启动共用 INIT_TIMEOUT 的抵扣），
// 不再排队上限一到就 429；抵扣用完后按普通容量原因处理
func TestPrimaryBudgetCreditsOwnExpansion(t *testing.T) {
	budget := &leaseWaitBudget{limit: 50 * time.Millisecond, startedAt: time.Now(), expansionCredit: time.Second, modelID: testRuntimeModel}
	ctx := context.Background()
	for range 4 {
		if err := budget.wait(ctx, nil, 60*time.Millisecond, waitReasonExpanding); err != nil {
			t.Fatalf("等后台扩容时不应按排队超限结束，实际 %v", err)
		}
	}
	if budget.coldStart < 200*time.Millisecond || budget.expired() {
		t.Fatalf("抵扣 = %s，截止时间应已延后", budget.coldStart)
	}
	limited := &leaseWaitBudget{limit: 20 * time.Millisecond, startedAt: time.Now(), expansionCredit: 30 * time.Millisecond, modelID: testRuntimeModel}
	var err error
	for range 10 {
		if err = limited.wait(ctx, nil, 20*time.Millisecond, waitReasonExpanding); err != nil {
			break
		}
	}
	if !isCapacityBusy(err) || limited.coldStart > 30*time.Millisecond {
		t.Fatalf("抵扣用完后应按排队超限结束: err=%v 抵扣=%s", err, limited.coldStart)
	}
	plain := &leaseWaitBudget{limit: 20 * time.Millisecond, startedAt: time.Now(), expansionCredit: time.Second, modelID: testRuntimeModel}
	for range 10 {
		if err = plain.wait(ctx, nil, 20*time.Millisecond, "账号全部忙碌"); err != nil {
			break
		}
	}
	if !isCapacityBusy(err) || plain.coldStart != 0 {
		t.Fatalf("其他容量原因不抵扣: err=%v 抵扣=%s", err, plain.coldStart)
	}
}

// TestPrimaryBudgetExpansionHoldsStreamStart 等后台扩容延后截止时间时，Ultra 主用模型的流式请求写出响应头前的等待同步推迟
// （与现场冷启动相同）：截止时间之前的 429 仍以 HTTP 状态返回；排队超限的文案把等后台扩容与现场冷启动分开写
func TestPrimaryBudgetExpansionHoldsStreamStart(t *testing.T) {
	hold := aistudio.NewStreamStartHold()
	budget := &leaseWaitBudget{
		limit: 20 * time.Millisecond, startedAt: time.Now(), expansionCredit: 80 * time.Millisecond,
		modelID: testRuntimeModel, pool: aistudio.PoolScopeUltra, quotaFailFast: true,
	}
	budget.holdStreamStart(hold)
	ctx := context.Background()
	waited := make(chan struct{})
	go func() {
		defer close(waited)
		_ = budget.wait(ctx, nil, 60*time.Millisecond, waitReasonExpanding)
	}()
	deadline := time.Now().Add(time.Second)
	for {
		if _, starting := hold.State(); starting {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("等后台扩容期间写出响应头前的等待应被推迟（hold 标为启动中）")
		}
		time.Sleep(time.Millisecond)
	}
	<-waited
	extension, starting := hold.State()
	if starting || extension <= 0 || extension != budget.coldStart || budget.expansion != budget.coldStart {
		t.Fatalf("等待结束后 hold 延长 %s（启动中=%v），抵扣 %s、其中扩容 %s，期望延长等于抵扣", extension, starting, budget.coldStart, budget.expansion)
	}
	var err error
	for range 20 {
		if err = budget.wait(ctx, nil, 20*time.Millisecond, waitReasonExpanding); err != nil {
			break
		}
	}
	var busy *aistudio.CapacityBusyError
	if !errors.As(err, &busy) {
		t.Fatalf("抵扣用完后应排队超限，实际 %v", err)
	}
	if busy.ColdStart != 0 || busy.Expansion != budget.coldStart || budget.coldStart > 80*time.Millisecond {
		t.Fatalf("排队超限：现场冷启动 %s、等后台扩容 %s，抵扣 %s", busy.ColdStart, busy.Expansion, budget.coldStart)
	}
	if text := busy.Error(); strings.Contains(text, "含现场冷启动") || !strings.Contains(text, "（含等后台扩容 ") {
		t.Fatalf("文案应只写等后台扩容：%s", text)
	}
	if extension, _ := hold.State(); extension != budget.coldStart {
		t.Fatalf("hold 延长 %s，抵扣 %s，两者应同步", extension, budget.coldStart)
	}
}

// isCapacityBusy 判断错误是否为排队超限
func isCapacityBusy(err error) bool {
	_, ok := err.(*aistudio.CapacityBusyError)
	return ok
}
