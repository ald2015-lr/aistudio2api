package app

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/camoufoxnative"
	"github.com/Mag1cFall/AIStudio2API/internal/config"
)

const (
	partitionNormalA = "normal-a@example.com"
	partitionNormalB = "normal-b@example.com"
	partitionNormalC = "normal-c@example.com"
	partitionUltraA  = "ultra-a@example.com"
	partitionUltraB  = "ultra-b@example.com"
	partitionUltraC  = "ultra-c@example.com"
	// partitionUltraModel 只在 Ultra 账户 B 的目录中出现，用来让请求只能使用 Ultra 账户 B
	partitionUltraModel = "ultra-only-model"
)

// partitionTestManager 创建纯 Go 后端（启动立即返回 stub Worker）的管理器：ultraIDs 的权益为 Ultra；
// 普通分区与 Ultra 分区的常驻数、峰值数分别为 normalWarm/normalMax 与 ultraWarm/ultraMax
func partitionTestManager(
	t *testing.T, normalIDs []string, ultraIDs []string, normalWarm, normalMax, ultraWarm, ultraMax int,
) (*accountWorkerManager, *aistudio.AccountPool, *requestRegistry) {
	t.Helper()
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	pool, accounts := testAccountPool(t, append(slices.Clone(normalIDs), ultraIDs...)...)
	for _, accountID := range ultraIDs {
		models := []aistudio.Model{{
			ID: testRuntimeModel, Methods: []string{"generateContent", "countTokens", "bidiGenerateContent"},
			Capabilities: map[string]bool{"chat_model": true, "transcription_output": true},
		}}
		if accountID == partitionUltraB {
			models = append(models, aistudio.Model{
				ID: partitionUltraModel, Methods: []string{"generateContent"}, Capabilities: map[string]bool{"chat_model": true},
			})
		}
		if err := pool.SetCatalog(accountID, aistudio.BenefitTierUltra, models); err != nil {
			t.Fatal(err)
		}
	}
	requests := newRequestRegistry(ctx)
	manager := newAccountWorkerManager(pool, accounts, requests, "", "", time.Minute, normalWarm, normalMax, 2, false)
	manager.setUltraCapacity(ultraWarm, ultraMax)
	manager.launch = func(_ context.Context, accountID string, _ camoufoxnative.Options) (*aistudio.NativeWorker, error) {
		worker, _ := aistudio.NewStubWorker(accountID, 0)
		return worker, nil
	}
	t.Cleanup(func() { _ = manager.Close() })
	return manager, pool, requests
}

func sortedWarm(manager *accountWorkerManager) []string {
	warm := manager.WarmAccountIDs()
	slices.Sort(warm)
	return warm
}

// holdLease 让账户处于忙碌状态（不能作为淘汰对象），返回释放函数
func holdLease(t *testing.T, pool *aistudio.AccountPool, accountID string) func() {
	t.Helper()
	lease, err := pool.AcquireFor(context.Background(), aistudio.AccountSelection{AccountID: accountID})
	if err != nil {
		t.Fatal(err)
	}
	return func() { _ = lease.Release() }
}

// TestWorkerPartitionCaps 两个分区的峰值数分别计算：普通分区满了不影响 Ultra 账户启动；分区已满时只淘汰同分区的空闲 Worker，
// 不会为普通账户淘汰 Ultra Worker，也不会为 Ultra 账户淘汰普通 Worker；调大峰值数后立即可以启动
func TestWorkerPartitionCaps(t *testing.T) {
	manager, pool, _ := partitionTestManager(t,
		[]string{partitionNormalA, partitionNormalB}, []string{partitionUltraA, partitionUltraB}, 1, 1, 1, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := manager.promote(ctx, partitionNormalA, testRuntimeModel); err != nil {
		t.Fatalf("普通账户启动失败: %v", err)
	}
	if _, err := manager.promote(ctx, partitionUltraA, testRuntimeModel); err != nil {
		t.Fatalf("普通分区已满时 Ultra 账户应能启动: %v", err)
	}
	if got := sortedWarm(manager); !slices.Equal(got, []string{partitionNormalA, partitionUltraA}) {
		t.Fatalf("驻留 Worker = %v", got)
	}

	// 普通分区已满且唯一的普通 Worker 正忙：普通账户不能启动，空闲的 Ultra Worker 不能被淘汰
	release := holdLease(t, pool, partitionNormalA)
	if _, err := manager.promote(ctx, partitionNormalB, testRuntimeModel); !errors.Is(err, errAccountWorkerCapacity) {
		t.Fatalf("普通分区已满：err = %v，期望 Worker 槽位已满", err)
	}
	if got := sortedWarm(manager); !slices.Equal(got, []string{partitionNormalA, partitionUltraA}) {
		t.Fatalf("普通账户启动不应淘汰 Ultra Worker，驻留 Worker = %v", got)
	}
	if victim := manager.idleWarmVictimFor(partitionNormalB, testRuntimeModel, false, aistudio.PoolScopeNormal); victim != "" {
		t.Fatalf("普通分区的淘汰对象 = %q，期望没有（普通 Worker 正忙，Ultra Worker 不属于该分区）", victim)
	}

	// Ultra 分区已满：淘汰同分区空闲的 Ultra Worker，普通 Worker 不受影响
	if _, err := manager.promote(ctx, partitionUltraB, testRuntimeModel); err != nil {
		t.Fatalf("Ultra 分区内替换失败: %v", err)
	}
	if got := sortedWarm(manager); !slices.Equal(got, []string{partitionNormalA, partitionUltraB}) {
		t.Fatalf("Ultra 账户启动只应淘汰 Ultra Worker，驻留 Worker = %v", got)
	}
	release()

	// 热更新调大普通分区峰值数后立即可以扩容，不影响 Ultra 分区
	cfg := config.Default()
	cfg.WarmWorkerLimit, cfg.MaxActiveWorkers, cfg.WarmStartupConcurrency = 1, 2, 1
	cfg.UltraWarmWorkerLimit, cfg.UltraMaxActiveWorkers = 1, 1
	manager.applyLiveSettings(cfg)
	release = holdLease(t, pool, partitionNormalA)
	defer release()
	if _, err := manager.promote(ctx, partitionNormalB, testRuntimeModel); err != nil {
		t.Fatalf("调大普通分区峰值数后应能扩容: %v", err)
	}
	if got := sortedWarm(manager); !slices.Equal(got, []string{partitionNormalA, partitionNormalB, partitionUltraB}) {
		t.Fatalf("扩容后驻留 Worker = %v", got)
	}
	if occupied := manager.occupiedWorkersIn(aistudio.PoolScopeUltra, pool.UltraAccountIDs()); occupied.slots != 1 {
		t.Fatalf("Ultra 分区占用 %d 个槽位，期望 1", occupied.slots)
	}
	if occupied := manager.occupiedWorkersIn(aistudio.PoolScopeNormal, pool.UltraAccountIDs()); occupied.slots != 2 {
		t.Fatalf("普通分区占用 %d 个槽位，期望 2", occupied.slots)
	}
}

// TestPrewarmTargetsPerPartition 预热把每个分区从自己的账户补齐到自己的常驻数；Ultra 常驻数为 0 时只按需启动
func TestPrewarmTargetsPerPartition(t *testing.T) {
	for _, test := range []struct {
		name               string
		ultraWarm          int
		wantNormal, wantUl int
	}{
		{name: "两个分区分别补齐", ultraWarm: 2, wantNormal: 1, wantUl: 2},
		{name: "Ultra 常驻数为 0", ultraWarm: 0, wantNormal: 1, wantUl: 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			manager, _, _ := partitionTestManager(t,
				[]string{partitionNormalA, partitionNormalB, partitionNormalC},
				[]string{partitionUltraA, partitionUltraB, partitionUltraC}, 1, 3, test.ultraWarm, 3)
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			if err := <-manager.StartPrewarm(ctx); err != nil {
				t.Fatalf("预热失败: %v", err)
			}
			manager.waitPrewarm()
			if got := len(manager.warmAccountIDsIn(aistudio.PoolScopeNormal)); got != test.wantNormal {
				t.Fatalf("普通分区驻留 %d 个，期望 %d", got, test.wantNormal)
			}
			if got := len(manager.warmAccountIDsIn(aistudio.PoolScopeUltra)); got != test.wantUl {
				t.Fatalf("Ultra 分区驻留 %d 个，期望 %d", got, test.wantUl)
			}
			if got := manager.PrewarmTarget(); got != test.wantNormal+test.wantUl {
				t.Fatalf("预热目标 = %d，期望 %d", got, test.wantNormal+test.wantUl)
			}
			if manager.prewarmNeeded() {
				t.Fatal("两个分区都已达标，不应再预热")
			}
		})
	}
}

// TestPrewarmWithoutUltraAccounts 没有 Ultra 账户时预热与原来一样只补齐普通分区，不为空的 Ultra 分区分类候选
func TestPrewarmWithoutUltraAccounts(t *testing.T) {
	manager, _, _ := partitionTestManager(t, []string{partitionNormalA, partitionNormalB, partitionNormalC}, nil, 2, 5, 2, 5)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := <-manager.StartPrewarm(ctx); err != nil {
		t.Fatalf("预热失败: %v", err)
	}
	manager.waitPrewarm()
	if got := len(manager.WarmAccountIDs()); got != 2 {
		t.Fatalf("驻留 %d 个，期望 2", got)
	}
	if manager.PrewarmTarget() != 2 || manager.prewarmTargetFor(aistudio.PoolScopeUltra) != 0 || manager.prewarmNeeded() {
		t.Fatalf("没有 Ultra 账户时预热目标 = %d（Ultra %d），期望 2（Ultra 0）", manager.PrewarmTarget(), manager.prewarmTargetFor(aistudio.PoolScopeUltra))
	}
	if reason := manager.prewarmState().Reason; reason != "" {
		t.Fatalf("预热不应记录等待原因: %q", reason)
	}
}

// TestReapIdleWorkerPerPartition 空闲回收按分区各自的常驻数：普通分区多出的 Worker 被回收，Ultra Worker 不受影响；
// Ultra 常驻数调为 0 后回收 Ultra Worker
func TestReapIdleWorkerPerPartition(t *testing.T) {
	manager, _, _ := partitionTestManager(t, []string{partitionNormalA, partitionNormalB}, []string{partitionUltraA}, 1, 3, 1, 3)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for _, accountID := range []string{partitionNormalA, partitionNormalB, partitionUltraA} {
		if _, err := manager.promote(ctx, accountID, testRuntimeModel); err != nil {
			t.Fatal(err)
		}
	}
	later := time.Now().Add(time.Hour)
	for manager.reapIdleWorker(later) {
	}
	if normal, ultra := len(manager.warmAccountIDsIn(aistudio.PoolScopeNormal)), len(manager.warmAccountIDsIn(aistudio.PoolScopeUltra)); normal != 1 || ultra != 1 {
		t.Fatalf("回收后普通 %d 个、Ultra %d 个，期望各 1 个", normal, ultra)
	}
	manager.setUltraCapacity(0, 3)
	for manager.reapIdleWorker(later) {
	}
	if normal, ultra := len(manager.warmAccountIDsIn(aistudio.PoolScopeNormal)), len(manager.warmAccountIDsIn(aistudio.PoolScopeUltra)); normal != 1 || ultra != 0 {
		t.Fatalf("Ultra 常驻数为 0 后普通 %d 个、Ultra %d 个，期望 1 与 0", normal, ultra)
	}
}

// TestStandbyCapacityPerPartition 请求调度按备用账户所在分区判断能否启动：普通分区已满时选同一批候选里的 Ultra 账户；
// 都满时只有允许淘汰且同分区有空闲 Worker 才选
func TestStandbyCapacityPerPartition(t *testing.T) {
	manager, _, _ := partitionTestManager(t, []string{partitionNormalA, partitionNormalB}, []string{partitionUltraA, partitionUltraB}, 1, 1, 1, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := manager.promote(ctx, partitionNormalA, testRuntimeModel); err != nil {
		t.Fatal(err)
	}
	capacity := manager.standbyCapacity(manager.WarmAccountIDs(), testRuntimeModel)
	standby := []string{partitionNormalB, partitionUltraA}
	if got := capacity.promotable(standby, false); got != partitionUltraA {
		t.Fatalf("普通分区已满时应选 Ultra 账户，实际 %q", got)
	}
	if got := capacity.expandable(standby); got != partitionUltraA {
		t.Fatalf("普通分区已满时后台扩容应选 Ultra 账户，实际 %q", got)
	}
	if got := capacity.promotable([]string{partitionNormalB}, false); got != "" {
		t.Fatalf("普通分区已满且不允许淘汰时不应选普通账户，实际 %q", got)
	}
	if got := capacity.promotable([]string{partitionNormalB}, true); got != partitionNormalB {
		t.Fatalf("允许淘汰同分区空闲 Worker 时应选普通账户，实际 %q", got)
	}
	if _, err := manager.promote(ctx, partitionUltraA, testRuntimeModel); err != nil {
		t.Fatal(err)
	}
	capacity = manager.standbyCapacity(manager.WarmAccountIDs(), testRuntimeModel)
	if got := capacity.promotable([]string{partitionUltraB}, false); got != "" {
		t.Fatalf("Ultra 分区已满时不应选 Ultra 账户，实际 %q", got)
	}
}

// TestUltraRequestWaitsForUltraCapacity Ultra 请求在 Ultra 分区已满且没有空闲 Worker 时排队，不淘汰空闲的普通 Worker；
// 同分区的 Worker 空闲后被唤醒，在 Ultra 分区内替换完成调度
func TestUltraRequestWaitsForUltraCapacity(t *testing.T) {
	manager, pool, requests := partitionTestManager(t, []string{partitionNormalA}, []string{partitionUltraA, partitionUltraB}, 1, 3, 1, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for _, accountID := range []string{partitionNormalA, partitionUltraA} {
		if _, err := manager.promote(ctx, accountID, testRuntimeModel); err != nil {
			t.Fatal(err)
		}
	}
	service := &trackedService{lifecycle: ctx, pool: pool, requests: requests, workers: manager}
	release := holdLease(t, pool, partitionUltraA)

	type acquired struct {
		lease *aistudio.AccountLease
		err   error
	}
	result := make(chan acquired, 1)
	ultraCtx := aistudio.ContextWithPoolScope(ctx, aistudio.PoolScopeUltra)
	go func() {
		lease, err := service.acquireWarmLease(ultraCtx, aistudio.AccountSelection{ModelID: partitionUltraModel, Method: "generateContent"})
		result <- acquired{lease: lease, err: err}
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		manager.dispatch.mu.Lock()
		waiting := len(manager.dispatch.queues)
		manager.dispatch.mu.Unlock()
		if waiting > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Ultra 请求没有进入等待队列")
		}
		time.Sleep(time.Millisecond)
	}
	if got := sortedWarm(manager); !slices.Equal(got, []string{partitionNormalA, partitionUltraA}) {
		t.Fatalf("等待期间不应淘汰普通 Worker，驻留 Worker = %v", got)
	}
	releasedAt := time.Now()
	release()
	select {
	case got := <-result:
		if got.err != nil {
			t.Fatalf("Ultra 请求调度失败: %v", got.err)
		}
		defer got.lease.Release()
		if got.lease.Account().ID != partitionUltraB {
			t.Fatalf("Ultra 请求用到了 %s", got.lease.Account().ID)
		}
		if waited := time.Since(releasedAt); waited >= schedulingRecheck {
			t.Fatalf("同分区 Worker 空闲后 %s 才完成调度，期望被立即唤醒", waited)
		}
	case <-ctx.Done():
		t.Fatal("Ultra 请求没有被唤醒")
	}
	if got := sortedWarm(manager); !slices.Equal(got, []string{partitionNormalA, partitionUltraB}) {
		t.Fatalf("Ultra 请求只应替换 Ultra Worker，驻留 Worker = %v", got)
	}
}

// TestStatusShowsWorkerPartitions 管理页状态分别显示普通分区与 Ultra 分区的 Worker 计数，以及 Ultra 账户计数
func TestStatusShowsWorkerPartitions(t *testing.T) {
	manager, pool, requests := partitionTestManager(t, []string{partitionNormalA, partitionNormalB}, []string{partitionUltraA}, 2, 4, 1, 3)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for _, accountID := range []string{partitionNormalA, partitionUltraA} {
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
	if status.Workers.Warm != 1 || status.Workers.Max != 4 || status.Workers.Target != 2 || status.Workers.Occupied != 1 {
		t.Fatalf("普通分区计数 = %+v", status.Workers)
	}
	if len(status.Workers.WarmIDs) != 2 {
		t.Fatalf("Worker 列表应包含两个分区: %v", status.Workers.WarmIDs)
	}
	ultra := status.UltraWorkers
	if ultra.Warm != 1 || ultra.Occupied != 1 || ultra.Target != 1 || ultra.WarmLimit != 1 || ultra.Max != 3 {
		t.Fatalf("Ultra 分区计数 = %+v", ultra)
	}
	if status.Accounts.Total != 3 || status.UltraAccounts.Total != 1 || status.UltraAccounts.Ready != 1 {
		t.Fatalf("账户计数 = %+v，Ultra 账户计数 = %+v", status.Accounts, status.UltraAccounts)
	}
}

// TestRotationStaysInPartition 冷却轮换按分区统计与淘汰：Ultra 分区有可接替的 Ultra 账户时只替换 Ultra Worker；
// 普通分区没有可接替的普通账户时，即使有空闲的 Ultra 账户也不淘汰普通 Worker
func TestRotationStaysInPartition(t *testing.T) {
	const ultraD = "ultra-d@example.com"
	normal := []string{partitionNormalA, partitionNormalB, partitionNormalC}
	ultra := []string{partitionUltraA, partitionUltraB, partitionUltraC}
	manager, pool, requests := partitionTestManager(t, normal, append(slices.Clone(ultra), ultraD), 3, 5, 2, 5)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	until := time.Now().Add(2 * time.Hour)
	for _, accountID := range append(slices.Clone(normal), ultra...) {
		if _, err := manager.promote(ctx, accountID, testRuntimeModel); err != nil {
			t.Fatal(err)
		}
		manager.accounts[accountID].readyAt.Store(time.Now().Add(-time.Hour).UnixNano())
		if err := pool.MarkCooldownIfGeneration(accountID, testRuntimeModel, pool.ModelAccessGeneration(accountID), time.Now(), until, "每日限额"); err != nil {
			t.Fatal(err)
		}
	}
	service := &trackedService{lifecycle: ctx, pool: pool, requests: requests, workers: manager}
	service.rotateCooledWorkers(ctx)
	manager.waitPrewarm()
	if got := len(manager.warmAccountIDsIn(aistudio.PoolScopeNormal)); got != 3 {
		t.Fatalf("普通分区没有可接替的普通账户，不应淘汰普通 Worker，实际驻留 %d 个", got)
	}
	rotatedUltra := 0
	for _, accountID := range ultra {
		if !slices.Contains(manager.WarmAccountIDs(), accountID) {
			rotatedUltra++
		}
	}
	if rotatedUltra != 1 {
		t.Fatalf("Ultra 分区应替换 1 个长时间冷却的 Worker，实际 %d 个", rotatedUltra)
	}
	if hot := manager.hotModelList(); !slices.Equal(hot, []string{testRuntimeModel}) {
		t.Fatalf("热门冷却模型 = %v", hot)
	}
}
