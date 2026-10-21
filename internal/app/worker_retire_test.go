package app

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/camoufoxnative"
)

// globalDailyQuotaError 构造上游“每日限额、整个账号冻结”的 429：quota_id 带 PerProjectPerUser 且没有按模型标记，
// 另给 2 小时的重试建议，保证冷却剩余不少于 rotationMinRemaining（不受太平洋时间离次日零点多近影响）
func globalDailyQuotaError() error {
	return &aistudio.RPCError{
		StatusCode: http.StatusTooManyRequests, Message: "You exceeded your current quota",
		Metadata: map[string]string{"quota_id": "GenerateRequestsPerDayPerProjectPerUser"}, RetryDelay: 2 * time.Hour,
	}
}

// minuteQuotaError 构造分钟限额的 429，一分钟内恢复
func minuteQuotaError() error {
	return &aistudio.RPCError{
		StatusCode: http.StatusTooManyRequests, Message: "Resource has been exhausted",
		Metadata: map[string]string{"quota_limit": "GenerateRequestsPerMinutePerProjectPerModel-FreeTier", "quota_id": "per_model"},
	}
}

// retireTestService 创建带冷却淘汰回调（淘汰后立即补齐预热）的生成服务
func retireTestService(ctx context.Context, manager *accountWorkerManager, pool *aistudio.AccountPool, requests *requestRegistry) *trackedService {
	service := &trackedService{
		lifecycle: ctx, pool: pool, requests: requests, workers: manager,
		forbidden: newForbiddenTracker(), quota: newQuotaSharing("", requests),
	}
	refill := func() { manager.StartPrewarm(ctx) }
	manager.afterRetire.Store(&refill)
	return service
}

// coolGlobally 给账户写入 2 小时的全局冷却
func coolGlobally(t *testing.T, pool *aistudio.AccountPool, accountID string) {
	t.Helper()
	until := time.Now().Add(2 * time.Hour)
	if err := pool.MarkCooldownIfGeneration(accountID, "", pool.ModelAccessGeneration(accountID), time.Now(), until, "每日限额"); err != nil {
		t.Fatal(err)
	}
}

// holdExclusive 用不受冷却限制的独占租约让账户处于忙碌状态（冷却中的账户 AcquireFor 会等冷却结束），返回释放函数
func holdExclusive(t *testing.T, pool *aistudio.AccountPool, accountID string) func() {
	t.Helper()
	lease, err := pool.AcquireAccount(context.Background(), accountID)
	if err != nil {
		t.Fatal(err)
	}
	return func() { _ = lease.Release() }
}

// waitUntil 轮询等待条件成立，超时返回 false
func waitUntil(timeout time.Duration, condition func() bool) bool {
	deadline := time.Now().Add(timeout)
	for !condition() {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(5 * time.Millisecond)
	}
	return true
}

// hasLogContaining 判断运行日志中是否有包含全部片段的记录
func hasLogContaining(requests *requestRegistry, fragments ...string) bool {
	for _, message := range serviceLogMessages(requests) {
		matched := true
		for _, fragment := range fragments {
			matched = matched && strings.Contains(message, fragment)
		}
		if matched {
			return true
		}
	}
	return false
}

// TestCooldownRetiresIdleWorkerPromptly 账号在请求中触发全局每日限额：请求释放租约后它的驻留 Worker 立刻被淘汰（不等轮换），
// 随后预热用没有冷却的账户补齐；分钟限额不登记淘汰
func TestCooldownRetiresIdleWorkerPromptly(t *testing.T) {
	manager, pool, requests := partitionTestManager(t, []string{partitionNormalA, partitionNormalB}, nil, 1, 2, 0, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	service := retireTestService(ctx, manager, pool, requests)
	if _, err := manager.promote(ctx, partitionNormalA, testRuntimeModel); err != nil {
		t.Fatal(err)
	}
	lease, err := pool.AcquireFor(ctx, aistudio.AccountSelection{AccountID: partitionNormalA, ModelID: testRuntimeModel})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, stateErr := service.applyQuotaCooldown(lease, partitionNormalA, testRuntimeModel, minuteQuotaError(), ""); !ok || stateErr != nil {
		t.Fatalf("分钟限额应被识别为额度错误: ok=%v err=%v", ok, stateErr)
	}
	if got := manager.retirementCount(); got != 0 {
		t.Fatalf("分钟限额不应登记淘汰，登记数 = %d", got)
	}
	if _, ok, stateErr := service.applyQuotaCooldown(lease, partitionNormalA, testRuntimeModel, globalDailyQuotaError(), ""); !ok || stateErr != nil {
		t.Fatalf("每日限额应被识别为额度错误: ok=%v err=%v", ok, stateErr)
	}
	if got := manager.retirementCount(); got != 1 {
		t.Fatalf("全局每日限额应登记淘汰，登记数 = %d", got)
	}
	releasedAt := time.Now()
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
	if !waitUntil(3*time.Second, func() bool { return !slices.Contains(manager.WarmAccountIDs(), partitionNormalA) }) {
		t.Fatalf("租约释放后冷却账号的 Worker 应被淘汰，驻留 Worker = %v", manager.WarmAccountIDs())
	}
	if waited := time.Since(releasedAt); waited >= 2*time.Second {
		t.Fatalf("租约释放后 %s 才淘汰，期望立即淘汰", waited)
	}
	if !waitUntil(time.Second, func() bool { return manager.retirementCount() == 0 }) {
		t.Fatalf("淘汰后登记应被清除，登记数 = %d", manager.retirementCount())
	}
	if !hasLogContaining(requests, "冷却淘汰", "范围=全局") {
		t.Fatalf("应记录冷却淘汰日志，日志 = %v", serviceLogMessages(requests))
	}
	// 淘汰后由回调补齐预热：没有冷却的账户 B 接替
	if !waitUntil(5*time.Second, func() bool { return slices.Contains(manager.WarmAccountIDs(), partitionNormalB) }) {
		t.Fatalf("淘汰后应预热没有冷却的账户，驻留 Worker = %v", manager.WarmAccountIDs())
	}
}

// TestCooldownRetiresBusyWorkerAfterRelease 冷却时账号还有其他进行中的请求：Worker 不被关闭，等最后一个租约释放后再淘汰
func TestCooldownRetiresBusyWorkerAfterRelease(t *testing.T) {
	manager, pool, requests := partitionTestManager(t, []string{partitionNormalA, partitionNormalB}, nil, 1, 2, 0, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	service := retireTestService(ctx, manager, pool, requests)
	if _, err := manager.promote(ctx, partitionNormalA, testRuntimeModel); err != nil {
		t.Fatal(err)
	}
	releaseOther := holdLease(t, pool, partitionNormalA)
	lease, err := pool.AcquireFor(ctx, aistudio.AccountSelection{AccountID: partitionNormalA, ModelID: testRuntimeModel})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, stateErr := service.applyQuotaCooldown(lease, partitionNormalA, testRuntimeModel, globalDailyQuotaError(), ""); !ok || stateErr != nil {
		t.Fatalf("每日限额应被识别为额度错误: ok=%v err=%v", ok, stateErr)
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(600 * time.Millisecond)
	if !slices.Contains(manager.WarmAccountIDs(), partitionNormalA) {
		t.Fatal("账号仍在服务其他请求时不应淘汰它的 Worker")
	}
	if got := manager.retirementCount(); got != 1 {
		t.Fatalf("忙碌期间登记应保留，登记数 = %d", got)
	}
	releaseOther()
	if !waitUntil(3*time.Second, func() bool { return !slices.Contains(manager.WarmAccountIDs(), partitionNormalA) }) {
		t.Fatalf("最后一个租约释放后应淘汰冷却账号的 Worker，驻留 Worker = %v", manager.WarmAccountIDs())
	}
}

// TestRetirementKeepsWorkerWithoutSpareAccount 分区内没有可接替的账户时不淘汰冷却的 Worker：
// 小规模部署全部账号都冷却时保留 Worker，冷却到期后能立即服务；登记保留等以后再试
func TestRetirementKeepsWorkerWithoutSpareAccount(t *testing.T) {
	manager, pool, requests := partitionTestManager(t, []string{partitionNormalA, partitionNormalB}, nil, 1, 2, 0, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	service := retireTestService(ctx, manager, pool, requests)
	if _, err := manager.promote(ctx, partitionNormalA, testRuntimeModel); err != nil {
		t.Fatal(err)
	}
	coolGlobally(t, pool, partitionNormalB)
	lease, err := pool.AcquireFor(ctx, aistudio.AccountSelection{AccountID: partitionNormalA, ModelID: testRuntimeModel})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := service.applyQuotaCooldown(lease, partitionNormalA, testRuntimeModel, globalDailyQuotaError(), ""); !ok {
		t.Fatal("每日限额应被识别为额度错误")
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(600 * time.Millisecond)
	if !slices.Contains(manager.WarmAccountIDs(), partitionNormalA) {
		t.Fatal("没有可接替的账户时不应淘汰冷却账号的 Worker")
	}
	if got := manager.retirementCount(); got != 1 {
		t.Fatalf("没有可接替的账户时登记应保留，登记数 = %d", got)
	}
	if service.sweepDeadWorkers(ctx) != 0 || !slices.Contains(manager.WarmAccountIDs(), partitionNormalA) {
		t.Fatal("清扫同样不应在没有可接替账户时淘汰 Worker")
	}
}

// TestSweepDeadWorkersIgnoresThreshold 全局冷却的空闲 Worker 清扫不受按模型的 5% / 最少 3 个阈值限制：
// 预热池 12 个里只有 4 个全局冷却也会被替换；一轮最多关闭启动并发（测试为 2）个，替代 Worker 由预热用没有冷却的账户补齐
func TestSweepDeadWorkersIgnoresThreshold(t *testing.T) {
	normal := make([]string, 0, 16)
	for index := 0; index < 16; index++ {
		normal = append(normal, "sweep-"+string(rune('a'+index))+"@example.com")
	}
	manager, pool, requests := partitionTestManager(t, normal, nil, 12, 14, 0, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	service := retireTestService(ctx, manager, pool, requests)
	warm := normal[:12]
	for _, accountID := range warm {
		if _, err := manager.promote(ctx, accountID, testRuntimeModel); err != nil {
			t.Fatal(err)
		}
	}
	dead := warm[:4]
	for _, accountID := range dead {
		coolGlobally(t, pool, accountID)
	}
	if _, counts := service.hotModelsIn(warm); len(counts) != 0 {
		t.Fatalf("全局冷却不应计入按模型的统计: %v", counts)
	}
	service.rotateCooledWorkers(ctx)
	manager.waitPrewarm()
	for _, accountID := range dead {
		if !slices.Contains(manager.WarmAccountIDs(), accountID) {
			t.Fatalf("按模型的轮换没有热门模型，不应淘汰 %s", accountID)
		}
	}
	if got := service.sweepDeadWorkers(ctx); got != 2 {
		t.Fatalf("第一轮清扫应关闭启动并发个（2）Worker，实际 %d", got)
	}
	manager.waitPrewarm()
	if got := service.sweepDeadWorkers(ctx); got != 2 {
		t.Fatalf("第二轮清扫应关闭剩下的 2 个，实际 %d", got)
	}
	manager.waitPrewarm()
	current := manager.WarmAccountIDs()
	for _, accountID := range dead {
		if slices.Contains(current, accountID) {
			t.Fatalf("全局冷却的 %s 仍驻留，驻留 Worker = %v", accountID, current)
		}
	}
	if len(current) != 12 {
		t.Fatalf("清扫后预热应补齐到 12 个，实际 %d: %v", len(current), current)
	}
	if got := service.sweepDeadWorkers(ctx); got != 0 {
		t.Fatalf("没有全局冷却的 Worker 时清扫不应关闭，实际 %d", got)
	}
	if !hasLogContaining(requests, "冷却清扫", "关闭全局冷却的空闲 Worker=2") {
		t.Fatalf("应记录冷却清扫日志，日志 = %v", serviceLogMessages(requests))
	}
}

// TestSweepDeadWorkersSkipsBusy 清扫不关闭正在服务请求的全局冷却 Worker
func TestSweepDeadWorkersSkipsBusy(t *testing.T) {
	manager, pool, requests := partitionTestManager(t, []string{partitionNormalA, partitionNormalB, partitionNormalC}, nil, 2, 3, 0, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	service := retireTestService(ctx, manager, pool, requests)
	for _, accountID := range []string{partitionNormalA, partitionNormalB} {
		if _, err := manager.promote(ctx, accountID, testRuntimeModel); err != nil {
			t.Fatal(err)
		}
		coolGlobally(t, pool, accountID)
	}
	release := holdExclusive(t, pool, partitionNormalA)
	defer release()
	if got := service.sweepDeadWorkers(ctx); got != 1 {
		t.Fatalf("只应关闭空闲的那个全局冷却 Worker，实际 %d", got)
	}
	manager.waitPrewarm()
	if !slices.Contains(manager.WarmAccountIDs(), partitionNormalA) {
		t.Fatal("正在服务请求的 Worker 不应被清扫")
	}
	if slices.Contains(manager.WarmAccountIDs(), partitionNormalB) {
		t.Fatal("空闲的全局冷却 Worker 应被清扫")
	}
}

// TestAcquireLeaseReclaimsDeadWorker 分区已满而驻留 Worker 的账号都全局冷却时，请求现场回收一个死槽位、启动备用账号，
// 不等待“没有空闲的 Worker 槽位”复查，并在请求时间线写入回收记录
func TestAcquireLeaseReclaimsDeadWorker(t *testing.T) {
	manager, pool, requests := partitionTestManager(t, []string{partitionNormalA, partitionNormalB, partitionNormalC}, nil, 2, 2, 0, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	service := retireTestService(ctx, manager, pool, requests)
	for _, accountID := range []string{partitionNormalA, partitionNormalB} {
		if _, err := manager.promote(ctx, accountID, testRuntimeModel); err != nil {
			t.Fatal(err)
		}
		coolGlobally(t, pool, accountID)
	}
	trace := aistudio.NewTrace()
	trace.SetRequestID("req-reclaim")
	startedAt := time.Now()
	lease, err := service.acquireWarmLease(aistudio.ContextWithTrace(ctx, trace), aistudio.AccountSelection{ModelID: testRuntimeModel, Method: "generateContent"})
	if err != nil {
		t.Fatalf("选号失败: %v", err)
	}
	defer lease.Release()
	if lease.Account().ID != partitionNormalC {
		t.Fatalf("应启动备用账号 C，实际 %s", lease.Account().ID)
	}
	if elapsed := time.Since(startedAt); elapsed >= schedulingRecheck {
		t.Fatalf("回收死槽位用了 %s，不应等待调度复查", elapsed)
	}
	warm := sortedWarm(manager)
	if len(warm) != 2 || !slices.Contains(warm, partitionNormalC) {
		t.Fatalf("应只回收一个死槽位，驻留 Worker = %v", warm)
	}
	if !hasLogContaining(requests, "回收冷却占位") {
		t.Fatalf("应记录回收日志，日志 = %v", serviceLogMessages(requests))
	}
	requests.mu.Lock()
	defer requests.mu.Unlock()
	for _, entry := range requests.logs {
		if entry.Event == "request.progress" && entry.Request != nil && entry.Request.ID == "req-reclaim" && strings.Contains(entry.Message, "回收冷却占位") {
			return
		}
	}
	t.Fatal("请求时间线应有回收死槽位的记录")
}

// TestAcquireLeaseNeverReclaimsBusyDeadWorker 死槽位的账号仍有进行中的请求时不被回收；还有没冷却的备用账号，
// 请求按没有空闲的 Worker 槽位排队到超时，不按全部冷却返回
func TestAcquireLeaseNeverReclaimsBusyDeadWorker(t *testing.T) {
	manager, pool, requests := partitionTestManager(t, []string{partitionNormalA, partitionNormalB, partitionNormalC}, nil, 2, 2, 0, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	service := retireTestService(ctx, manager, pool, requests)
	for _, accountID := range []string{partitionNormalA, partitionNormalB} {
		if _, err := manager.promote(ctx, accountID, testRuntimeModel); err != nil {
			t.Fatal(err)
		}
		coolGlobally(t, pool, accountID)
	}
	releaseA := holdExclusive(t, pool, partitionNormalA)
	defer releaseA()
	releaseB := holdExclusive(t, pool, partitionNormalB)
	defer releaseB()
	requestCtx, requestCancel := context.WithTimeout(ctx, 1500*time.Millisecond)
	defer requestCancel()
	lease, err := service.acquireWarmLease(requestCtx, aistudio.AccountSelection{ModelID: testRuntimeModel, Method: "generateContent"})
	if lease != nil {
		_ = lease.Release()
	}
	var cooling *aistudio.AllCoolingError
	if !errors.Is(err, context.DeadlineExceeded) || errors.As(err, &cooling) {
		t.Fatalf("两个死槽位都在服务请求时 err = %v，期望排队到超时（不是全部冷却）", err)
	}
	if got := sortedWarm(manager); !slices.Equal(got, []string{partitionNormalA, partitionNormalB}) {
		t.Fatalf("正在服务请求的 Worker 不应被回收，驻留 Worker = %v", got)
	}
	if hasLogContaining(requests, "回收冷却占位") {
		t.Fatal("不应记录回收日志")
	}
}

// TestReclaimDeadWorkerStaysInPartition 回收死槽位只在备用账号所在的分区内进行：Ultra 请求回收 Ultra 分区的死槽位，
// 不动普通分区；普通请求反之
func TestReclaimDeadWorkerStaysInPartition(t *testing.T) {
	manager, pool, requests := partitionTestManager(t,
		[]string{partitionNormalA, partitionNormalB}, []string{partitionUltraA, partitionUltraB}, 1, 1, 1, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	service := retireTestService(ctx, manager, pool, requests)
	for _, accountID := range []string{partitionNormalA, partitionUltraA} {
		if _, err := manager.promote(ctx, accountID, testRuntimeModel); err != nil {
			t.Fatal(err)
		}
		coolGlobally(t, pool, accountID)
	}
	ultraCtx := aistudio.ContextWithPoolScope(ctx, aistudio.PoolScopeUltra)
	ultraLease, err := service.acquireWarmLease(ultraCtx, aistudio.AccountSelection{ModelID: testRuntimeModel, Method: "generateContent"})
	if err != nil {
		t.Fatalf("Ultra 请求选号失败: %v", err)
	}
	defer ultraLease.Release()
	if ultraLease.Account().ID != partitionUltraB {
		t.Fatalf("Ultra 请求应启动 Ultra 备用账号，实际 %s", ultraLease.Account().ID)
	}
	if got := sortedWarm(manager); !slices.Equal(got, []string{partitionNormalA, partitionUltraB}) {
		t.Fatalf("Ultra 请求只应回收 Ultra 分区的死槽位，驻留 Worker = %v", got)
	}
	normalCtx := aistudio.ContextWithPoolScope(ctx, aistudio.PoolScopeNormal)
	normalLease, err := service.acquireWarmLease(normalCtx, aistudio.AccountSelection{ModelID: testRuntimeModel, Method: "generateContent"})
	if err != nil {
		t.Fatalf("普通请求选号失败: %v", err)
	}
	defer normalLease.Release()
	if normalLease.Account().ID != partitionNormalB {
		t.Fatalf("普通请求应启动普通备用账号，实际 %s", normalLease.Account().ID)
	}
	if got := sortedWarm(manager); !slices.Equal(got, []string{partitionNormalB, partitionUltraB}) {
		t.Fatalf("普通请求只应回收普通分区的死槽位，驻留 Worker = %v", got)
	}
}

// TestIdleVictimPrefersDeadWorker 淘汰选号：死槽位优先于冷却剩余不少于 ShortCooldownMax 的账号，再优先于最久未用的账号；
// 账户池只加锁一次，结果与原先逐个查询一致
func TestIdleVictimPrefersDeadWorker(t *testing.T) {
	manager, pool, _ := partitionTestManager(t, []string{partitionNormalA, partitionNormalB, partitionNormalC}, nil, 3, 3, 0, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	// 先启动 C 再启动 B、A，让 C 最久未用
	for _, accountID := range []string{partitionNormalC, partitionNormalB, partitionNormalA} {
		if _, err := manager.promote(ctx, accountID, testRuntimeModel); err != nil {
			t.Fatal(err)
		}
		lease, err := pool.AcquireFor(ctx, aistudio.AccountSelection{AccountID: accountID})
		if err != nil {
			t.Fatal(err)
		}
		_ = lease.Release()
		time.Sleep(2 * time.Millisecond)
	}
	if got := manager.idleWarmVictimFor("", testRuntimeModel, false, aistudio.PoolScopeNormal); got != partitionNormalC {
		t.Fatalf("没有冷却时应选最久未用的 C，实际 %q", got)
	}
	if got := manager.idleWarmVictimFor("", testRuntimeModel, true, aistudio.PoolScopeNormal); got != "" {
		t.Fatalf("没有冷却时只选冷却账号应为空，实际 %q", got)
	}
	// B 冷却剩余不少于 ShortCooldownMax（剩余不到它的短冷却不算冷却中），A 全局长时间冷却
	if err := pool.MarkCooldownIfGeneration(partitionNormalB, testRuntimeModel, pool.ModelAccessGeneration(partitionNormalB),
		time.Now(), time.Now().Add(3*time.Minute), "分钟限额"); err != nil {
		t.Fatal(err)
	}
	if got := manager.idleWarmVictimFor("", testRuntimeModel, false, aistudio.PoolScopeNormal); got != partitionNormalB {
		t.Fatalf("冷却中的 B 应优先于最久未用的 C，实际 %q", got)
	}
	coolGlobally(t, pool, partitionNormalA)
	if got := manager.idleWarmVictimFor("", testRuntimeModel, false, aistudio.PoolScopeNormal); got != partitionNormalA {
		t.Fatalf("长时间冷却的死槽位 A 应优先于冷却中的 B，实际 %q", got)
	}
	if got := manager.pickIdleVictim("", testRuntimeModel, victimDead, aistudio.PoolScopeNormal); got != partitionNormalA {
		t.Fatalf("只选死槽位应为 A，实际 %q", got)
	}
	release := holdExclusive(t, pool, partitionNormalA)
	defer release()
	if got := manager.pickIdleVictim("", testRuntimeModel, victimDead, aistudio.PoolScopeNormal); got != "" {
		t.Fatalf("死槽位忙碌时不应被选中，实际 %q", got)
	}
	if got := manager.idleWarmVictimFor("", testRuntimeModel, false, aistudio.PoolScopeNormal); got != partitionNormalB {
		t.Fatalf("死槽位忙碌时应选冷却中的 B，实际 %q", got)
	}
}

// TestIdleVictimIgnoresShortCooling 淘汰选号：剩余不到 ShortCooldownMax 的短冷却（分钟限额）不算冷却中，与健康的空闲 Worker 同档
// 按最久未用排序，只选冷却账号时不选它；冷却剩余不少于 ShortCooldownMax 时照常算冷却中
func TestIdleVictimIgnoresShortCooling(t *testing.T) {
	manager, pool, _ := partitionTestManager(t, []string{partitionNormalA, partitionNormalB, partitionNormalC}, nil, 3, 3, 0, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	// 先启动 C 再启动 B、A，让 C 最久未用
	for _, accountID := range []string{partitionNormalC, partitionNormalB, partitionNormalA} {
		if _, err := manager.promote(ctx, accountID, testRuntimeModel); err != nil {
			t.Fatal(err)
		}
		lease, err := pool.AcquireFor(ctx, aistudio.AccountSelection{AccountID: accountID})
		if err != nil {
			t.Fatal(err)
		}
		_ = lease.Release()
		time.Sleep(2 * time.Millisecond)
	}
	// B 全局分钟限额，30 秒后恢复
	if err := pool.MarkCooldownIfGeneration(partitionNormalB, "", pool.ModelAccessGeneration(partitionNormalB),
		time.Now(), time.Now().Add(30*time.Second), aistudio.MinuteQuotaKind+": 每分钟 12 次"); err != nil {
		t.Fatal(err)
	}
	if got := manager.idleWarmVictimFor("", testRuntimeModel, false, aistudio.PoolScopeNormal); got != partitionNormalC {
		t.Fatalf("短冷却的 B 不应优先，应选最久未用的 C，实际 %q", got)
	}
	if got := manager.idleWarmVictimFor("", testRuntimeModel, true, aistudio.PoolScopeNormal); got != "" {
		t.Fatalf("只选冷却账号时短冷却的 B 不算，应为空，实际 %q", got)
	}
	if got := manager.idleWarmVictimFor("", "", false, aistudio.PoolScopeNormal); got != partitionNormalC {
		t.Fatalf("不针对模型时同样应选最久未用的 C，实际 %q", got)
	}
	// 改为 3 分钟后恢复：剩余不少于 ShortCooldownMax，算冷却中
	if err := pool.MarkCooldownIfGeneration(partitionNormalB, "", pool.ModelAccessGeneration(partitionNormalB),
		time.Now(), time.Now().Add(3*time.Minute), aistudio.MinuteQuotaKind+": 每分钟 12 次"); err != nil {
		t.Fatal(err)
	}
	if got := manager.idleWarmVictimFor("", testRuntimeModel, true, aistudio.PoolScopeNormal); got != partitionNormalB {
		t.Fatalf("冷却剩余 3 分钟的 B 应算冷却中，实际 %q", got)
	}
}

// TestStatusCountsDeadWorkers 状态接口按分区给出死槽位数
func TestStatusCountsDeadWorkers(t *testing.T) {
	manager, pool, requests := partitionTestManager(t, []string{partitionNormalA, partitionNormalB}, []string{partitionUltraA}, 2, 4, 1, 3)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for _, accountID := range []string{partitionNormalA, partitionNormalB, partitionUltraA} {
		if _, err := manager.promote(ctx, accountID, testRuntimeModel); err != nil {
			t.Fatal(err)
		}
	}
	service := &trackedService{lifecycle: ctx, pool: pool, requests: requests, workers: manager}
	admin := &runtimeAdmin{lifecycle: ctx, pool: pool, service: service, requests: requests, workers: manager}
	status, err := admin.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if status.Workers.Dead != 0 || status.UltraWorkers.Dead != 0 {
		t.Fatalf("没有冷却时死槽位应为 0: %+v / %+v", status.Workers, status.UltraWorkers)
	}
	coolGlobally(t, pool, partitionNormalA)
	coolGlobally(t, pool, partitionUltraA)
	status, err = admin.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if status.Workers.Dead != 1 || status.Workers.Warm != 2 {
		t.Fatalf("普通分区应有 1 个死槽位: %+v", status.Workers)
	}
	if status.UltraWorkers.Dead != 1 || status.UltraWorkers.Warm != 1 {
		t.Fatalf("Ultra 分区应有 1 个死槽位: %+v", status.UltraWorkers)
	}
}

// TestRetirementDroppedWhenCooldownCleared 登记后冷却被清除（如管理页手动清除）时放弃淘汰
func TestRetirementDroppedWhenCooldownCleared(t *testing.T) {
	manager, pool, requests := partitionTestManager(t, []string{partitionNormalA, partitionNormalB}, nil, 1, 2, 0, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	service := retireTestService(ctx, manager, pool, requests)
	if _, err := manager.promote(ctx, partitionNormalA, testRuntimeModel); err != nil {
		t.Fatal(err)
	}
	release := holdLease(t, pool, partitionNormalA)
	lease, err := pool.AcquireFor(ctx, aistudio.AccountSelection{AccountID: partitionNormalA, ModelID: testRuntimeModel})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := service.applyQuotaCooldown(lease, partitionNormalA, testRuntimeModel, globalDailyQuotaError(), ""); !ok {
		t.Fatal("每日限额应被识别为额度错误")
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
	if err := pool.ClearCooldownIfGeneration(partitionNormalA, "", pool.ModelAccessGeneration(partitionNormalA), time.Now()); err != nil {
		t.Fatal(err)
	}
	release()
	if !waitUntil(3*time.Second, func() bool { return manager.retirementCount() == 0 }) {
		t.Fatalf("冷却清除后登记应被放弃，登记数 = %d", manager.retirementCount())
	}
	time.Sleep(300 * time.Millisecond)
	if !slices.Contains(manager.WarmAccountIDs(), partitionNormalA) {
		t.Fatal("冷却清除后不应淘汰 Worker")
	}
}

// TestReclaimStartsNoReplacement 现场回收不启动替补：调用的请求随后自己启动备用账号补上空出的槽位，另起替补会与它争同一个槽位
// （替补先占到时请求按替换流程再淘汰一个空闲 Worker：一个空槽两次冷启动、多淘汰一个 Worker）。
// WARM_WORKER_LIMIT（1）低于驻留数（3）时原先替补每次都会启动
func TestReclaimStartsNoReplacement(t *testing.T) {
	a, b, x, c, d := "drain-a@example.com", "drain-b@example.com", "drain-x@example.com", "drain-c@example.com", "drain-d@example.com"
	t.Run("只回收", func(t *testing.T) {
		env := newDrainEnv(t, []string{a, b, x, c, d}, 1, 3)
		env.promote(t, a, b, x)
		env.coolPro(t, a)
		if victim, err := env.manager.reclaimDeadWorker(env.ctx, aistudio.PoolScopeNormal, drainProModel); err != nil || victim != a {
			t.Fatalf("应回收 A: victim=%q err=%v", victim, err)
		}
		time.Sleep(300 * time.Millisecond)
		if got := sortedWarm(env.manager); !slices.Equal(got, []string{b, x}) || len(env.manager.OpeningAccountIDs()) != 0 {
			t.Fatalf("回收后不应启动替补，驻留 Worker = %v，启动中 = %v", got, env.manager.OpeningAccountIDs())
		}
		if hasLogContaining(env.requests, "替补启动") {
			t.Fatalf("回收后不应记录替补，日志 = %v", serviceLogMessages(env.requests))
		}
	})
	t.Run("请求路径", func(t *testing.T) {
		env := newDrainEnv(t, []string{a, b, x, c, d}, 1, 3)
		// B 不支持其他模型；A 在其他模型上每日限额冷却（死槽位），X 只冷却 1 分钟：分区已满、没有热 Worker 能服务请求
		if err := env.pool.SetCatalog(b, aistudio.BenefitTierFree, []aistudio.Model{
			{ID: drainProModel, Methods: []string{"generateContent"}, Capabilities: map[string]bool{"chat_model": true}},
		}); err != nil {
			t.Fatal(err)
		}
		env.promote(t, a, b, x)
		holdLease(t, env.pool, b)()
		holdLease(t, env.pool, x)()
		now := time.Now()
		if err := env.pool.MarkCooldownIfGeneration(a, testRuntimeModel, env.pool.ModelAccessGeneration(a), now, now.Add(2*time.Hour), "每日限额"); err != nil {
			t.Fatal(err)
		}
		if err := env.pool.MarkCooldownIfGeneration(x, testRuntimeModel, env.pool.ModelAccessGeneration(x), now, now.Add(time.Minute), "每分钟限额"); err != nil {
			t.Fatal(err)
		}
		var launches atomic.Int32
		launch := env.manager.launch
		env.manager.launch = func(ctx context.Context, accountID string, options camoufoxnative.Options) (*aistudio.NativeWorker, error) {
			launches.Add(1)
			time.Sleep(150 * time.Millisecond)
			return launch(ctx, accountID, options)
		}
		lease, err := env.service.acquireWarmLease(env.ctx, aistudio.AccountSelection{ModelID: testRuntimeModel, Method: "generateContent"})
		if err != nil {
			t.Fatal(err)
		}
		_ = lease.Release()
		time.Sleep(500 * time.Millisecond)
		if got := sortedWarm(env.manager); launches.Load() != 1 || !slices.Contains(got, x) || slices.Contains(got, a) {
			t.Fatalf("回收 A 后只应冷启动请求自己的备用账号、不淘汰 X：冷启动 %d 次，驻留 Worker = %v", launches.Load(), got)
		}
	})
}
