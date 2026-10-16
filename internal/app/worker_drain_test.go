package app

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// drainProModel 为测试里的主用模型；其他模型的请求用 testRuntimeModel
const drainProModel = "drain-pro-model"

// drainEnv 为排空与替补测试的环境：每个账户的目录都有主用模型与其他模型，账号可以只在主用模型上长时间冷却而仍能服务其他模型
type drainEnv struct {
	manager  *accountWorkerManager
	pool     *aistudio.AccountPool
	requests *requestRegistry
	service  *trackedService
	ctx      context.Context
}

func newDrainEnv(t *testing.T, accounts []string, warmTarget int, maxActive int) *drainEnv {
	t.Helper()
	manager, pool, requests := partitionTestManager(t, accounts, nil, warmTarget, maxActive, 0, 1)
	catalog := []aistudio.Model{
		{ID: testRuntimeModel, Methods: []string{"generateContent"}, Capabilities: map[string]bool{"chat_model": true}},
		{ID: drainProModel, Methods: []string{"generateContent"}, Capabilities: map[string]bool{"chat_model": true}},
	}
	for _, accountID := range accounts {
		if err := pool.SetCatalog(accountID, aistudio.BenefitTierFree, catalog); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	service := retireTestService(ctx, manager, pool, requests)
	service.setPrimaryModels([]string{drainProModel}, 10*time.Second)
	return &drainEnv{manager: manager, pool: pool, requests: requests, service: service, ctx: ctx}
}

// promote 启动账户的 Worker
func (env *drainEnv) promote(t *testing.T, accountIDs ...string) {
	t.Helper()
	for _, accountID := range accountIDs {
		if _, err := env.manager.promote(env.ctx, accountID, testRuntimeModel); err != nil {
			t.Fatal(err)
		}
	}
}

// coolPro 只在主用模型上写入 2 小时的每日限额冷却（测试账户只启用 Playground 通道），不登记淘汰
func (env *drainEnv) coolPro(t *testing.T, accountID string) {
	t.Helper()
	until := time.Now().Add(2 * time.Hour)
	if err := env.pool.MarkCooldownIfGeneration(accountID, drainProModel, env.pool.ModelAccessGeneration(accountID), time.Now(), until, "每日限额"); err != nil {
		t.Fatal(err)
	}
	if !env.pool.AccountLongCooling(accountID, drainProModel, rotationMinRemaining) || env.pool.AccountLongCooling(accountID, "", rotationMinRemaining) {
		t.Fatalf("%s 应只在主用模型上长时间冷却", accountID)
	}
}

// retirePro 在主用模型上冷却并像 applyQuotaCooldown 一样登记淘汰
func (env *drainEnv) retirePro(t *testing.T, accountID string) {
	t.Helper()
	env.coolPro(t, accountID)
	env.service.retireCooledWorker(accountID, drainProModel, aistudio.QuotaCooldown{Until: time.Now().Add(2 * time.Hour), Kind: aistudio.DailyQuotaKind})
}

// flashLoad 启动 loopers 个循环，经真实调度（acquireWarmLease）不断取得其他模型的租约、持有 hold 后释放；
// 各循环错开启动，同一账号上的租约互相重叠（饱和时 Worker 永远等不到空闲）。返回停止函数与各账号的租约数
func (env *drainEnv) flashLoad(loopers int, hold time.Duration) (func(), *sync.Map) {
	ctx, cancel := context.WithCancel(env.ctx)
	var loops sync.WaitGroup
	counts := &sync.Map{}
	for index := 0; index < loopers; index++ {
		loops.Add(1)
		go func(index int) {
			defer loops.Done()
			time.Sleep(time.Duration(index) * hold / time.Duration(loopers))
			for ctx.Err() == nil {
				lease, err := env.service.acquireWarmLease(ctx, aistudio.AccountSelection{ModelID: testRuntimeModel, Method: "generateContent"})
				if err != nil {
					continue
				}
				value, _ := counts.LoadOrStore(lease.Account().ID, new(atomic.Int64))
				value.(*atomic.Int64).Add(1)
				select {
				case <-ctx.Done():
				case <-time.After(hold):
				}
				_ = lease.Release()
			}
		}(index)
	}
	return func() { cancel(); loops.Wait() }, counts
}

func leaseCount(counts *sync.Map, accountID string) int64 {
	value, ok := counts.Load(accountID)
	if !ok {
		return 0
	}
	return value.(*atomic.Int64).Load()
}

// browsers 精确返回分区内真正持有浏览器的账户数：在账户锁内看 Worker 或待关闭的 Worker，加上还没有 Worker 的启动中账户。
// 不用 occupiedWorkersIn：它把锁被占用的账户按 1 个槽位估计（容量判断宁可多算），测试取样时会把正被短暂加锁、
// 并没有浏览器的账户也算进去，误报超过 峰值 + drainLimit
func (env *drainEnv) browsers() int {
	manager := env.manager
	ultra := env.pool.UltraAccountIDs()
	manager.mu.RLock()
	accounts := make([]*accountWorker, 0, len(manager.accounts))
	for _, account := range manager.accounts {
		if partitionOf(account.id, ultra) == aistudio.PoolScopeNormal {
			accounts = append(accounts, account)
		}
	}
	manager.mu.RUnlock()
	opening := manager.OpeningAccountIDs()
	count := 0
	for _, account := range accounts {
		account.mu.Lock()
		held := 0
		if account.worker != nil {
			held++
		}
		if account.cleanupWorker != nil {
			held++
		}
		account.mu.Unlock()
		if held == 0 && slices.Contains(opening, account.id) {
			held = 1
		}
		count += held
	}
	return count
}

// watchBrowsers 在后台每 2ms 记录一次分区浏览器数的最大值，返回停止并取得最大值的函数
func (env *drainEnv) watchBrowsers() func() int {
	done := make(chan struct{})
	var peak atomic.Int64
	var watcher sync.WaitGroup
	watcher.Add(1)
	go func() {
		defer watcher.Done()
		for {
			if current := int64(env.browsers()); current > peak.Load() {
				peak.Store(current)
			}
			select {
			case <-done:
				return
			case <-time.After(2 * time.Millisecond):
			}
		}
	}()
	return func() int {
		close(done)
		watcher.Wait()
		return int(peak.Load())
	}
}

// TestDrainRetiresBusyDeadWorkersUnderLoad 饱和的其他模型负载下（每个 Worker 一直有重叠的租约，永远等不到空闲），
// 三个对主用模型是死槽位的登记 Worker 依次排空、在租约时长的几倍内被淘汰，替补换上对主用模型没有冷却的账号，
// 驻留数保持 4，浏览器总数从不超过 峰值 + drainLimit，结束后没有账号停在排空中
func TestDrainRetiresBusyDeadWorkersUnderLoad(t *testing.T) {
	accounts := []string{"drain-a@example.com", "drain-b@example.com", "drain-c@example.com", "drain-d@example.com",
		"drain-e@example.com", "drain-f@example.com", "drain-g@example.com", "drain-h@example.com"}
	env := newDrainEnv(t, accounts, 4, 4)
	env.promote(t, accounts[:4]...)
	limit := env.manager.drainLimit(aistudio.PoolScopeNormal)
	if limit != 1 {
		t.Fatalf("峰值 4 时 drainLimit 应为 1，实际 %d", limit)
	}
	stopPeak := env.watchBrowsers()
	stop, counts := env.flashLoad(8, 300*time.Millisecond)
	defer stop()
	time.Sleep(400 * time.Millisecond)
	dead := accounts[:3]
	startedAt := time.Now()
	for _, accountID := range dead {
		env.retirePro(t, accountID)
	}
	before := make(map[string]int64, len(dead))
	for _, accountID := range dead {
		before[accountID] = leaseCount(counts, accountID)
	}
	retired := waitUntil(10*time.Second, func() bool {
		warm := env.manager.WarmAccountIDs()
		for _, accountID := range dead {
			if slices.Contains(warm, accountID) {
				return false
			}
		}
		return true
	})
	elapsed := time.Since(startedAt)
	if !retired {
		t.Fatalf("负载持续时死槽位应被排空淘汰，驻留 Worker = %v，登记数 = %d", sortedWarm(env.manager), env.manager.retirementCount())
	}
	// 三个排空依次进行，每个等进行中的租约（300ms）结束再关闭
	if elapsed > 6*time.Second {
		t.Fatalf("排空淘汰用了 %s", elapsed)
	}
	if !waitUntil(3*time.Second, func() bool { return len(env.manager.WarmAccountIDs()) == 4 }) {
		t.Fatalf("替补后驻留数应恢复为 4，实际 %v", sortedWarm(env.manager))
	}
	stop()
	if peak := stopPeak(); peak > 4+limit {
		t.Fatalf("浏览器总数最多为 峰值 + drainLimit = %d，实际 %d", 4+limit, peak)
	}
	for _, accountID := range sortedWarm(env.manager) {
		if env.pool.AccountLongCooling(accountID, drainProModel, rotationMinRemaining) {
			t.Fatalf("替补不应是主用模型冷却的账号: %s，驻留 Worker = %v", accountID, sortedWarm(env.manager))
		}
	}
	for _, accountID := range accounts {
		if env.pool.Draining(accountID) {
			t.Fatalf("%s 仍在排空中", accountID)
		}
	}
	if env.manager.drainCount(aistudio.PoolScopeNormal) != 0 || env.manager.retirementCount() != 0 {
		t.Fatalf("淘汰后不应留下排空或登记: 排空 %d，登记 %d", env.manager.drainCount(aistudio.PoolScopeNormal), env.manager.retirementCount())
	}
	if !hasLogContaining(env.requests, "WAA Worker 排空", "范围="+drainProModel) || !hasLogContaining(env.requests, "冷却淘汰", "排空=") {
		t.Fatalf("应记录排空与淘汰日志，日志 = %v", serviceLogMessages(env.requests))
	}
	if !hasLogContaining(env.requests, "替补启动", "原因=排空") {
		t.Fatalf("排空开始时应启动替补，日志 = %v", serviceLogMessages(env.requests))
	}
	total := int64(0)
	for _, accountID := range dead {
		total += leaseCount(counts, accountID)
	}
	if total == 0 {
		t.Fatal("负载期间死槽位应一直在服务其他模型的请求")
	}
}

// startBusyDrain 让账号 a 被固定账户的租约占住、只在主用模型上冷却并登记，等它进入排空；返回释放租约的函数
func (env *drainEnv) startBusyDrain(t *testing.T, accountID string) func() {
	t.Helper()
	release := holdLease(t, env.pool, accountID)
	env.retirePro(t, accountID)
	if !waitUntil(3*time.Second, func() bool { return env.pool.Draining(accountID) }) {
		release()
		t.Fatalf("忙碌的登记应进入排空，日志 = %v", serviceLogMessages(env.requests))
	}
	return release
}

// TestDrainStartsReplacementBeforeRetirement 先启后停：WARM_WORKER_LIMIT（2）低于驻留数（4）时，排空开始就启动替补，
// 驻留数在排空期间为 5（峰值 + drainLimit），状态接口的占用按排空让出计；此时不能再启动第 6 个；排空中的账号不再分给不固定账户的请求，
// 固定账户的请求照常使用；租约释放后旧 Worker 关闭，驻留数回到 4，不再多补一个
func TestDrainStartsReplacementBeforeRetirement(t *testing.T) {
	accounts := []string{"drain-a@example.com", "drain-b@example.com", "drain-c@example.com", "drain-d@example.com",
		"drain-e@example.com", "drain-f@example.com"}
	env := newDrainEnv(t, accounts, 2, 4)
	env.promote(t, accounts[:4]...)
	release := env.startBusyDrain(t, accounts[0])
	defer release()
	if !waitUntil(3*time.Second, func() bool { return len(env.manager.WarmAccountIDs()) == 5 }) {
		t.Fatalf("排空开始时应启动替补，驻留 Worker = %v", sortedWarm(env.manager))
	}
	if got := env.browsers(); got != 4+env.manager.drainLimit(aistudio.PoolScopeNormal) {
		t.Fatalf("排空期间浏览器数应为 峰值 + drainLimit，实际 %d", got)
	}
	normalOccupied, _ := env.manager.occupiedSlotsApprox(env.pool.UltraAccountIDs())
	if normalOccupied != 4 {
		t.Fatalf("状态接口的占用应按排空让出计为 4，实际 %d", normalOccupied)
	}
	var spare string
	for _, accountID := range accounts[4:] {
		if !slices.Contains(env.manager.WarmAccountIDs(), accountID) {
			spare = accountID
		}
	}
	if _, err := env.manager.ensureWorker(withImmediateStartup(env.ctx), spare, testRuntimeModel, false); !errors.Is(err, errAccountWorkerCapacity) {
		t.Fatalf("峰值 + drainLimit 已满时不应再启动 Worker: err = %v", err)
	}
	// 其他 4 个 Worker 共 8 个租约位：不固定账户的请求一个都不落到排空中的账号上
	var leases []*aistudio.AccountLease
	for range 6 {
		lease, err := env.service.acquireWarmLease(env.ctx, aistudio.AccountSelection{ModelID: testRuntimeModel, Method: "generateContent"})
		if err != nil {
			t.Fatal(err)
		}
		if lease.Account().ID == accounts[0] {
			t.Fatal("排空中的账号不应得到不固定账户的新租约")
		}
		leases = append(leases, lease)
	}
	for _, lease := range leases {
		_ = lease.Release()
	}
	pinned, err := env.pool.AcquireFor(env.ctx, aistudio.AccountSelection{AccountID: accounts[0], ModelID: testRuntimeModel})
	if err != nil {
		t.Fatalf("固定账户的请求应能使用排空中的账号: %v", err)
	}
	_ = pinned.Release()
	if err := env.pool.BindResource("files/drain-test", accounts[0]); err != nil {
		t.Fatal(err)
	}
	bound, err := env.service.acquireWarmLease(env.ctx, aistudio.AccountSelection{ResourceID: "files/drain-test", ModelID: testRuntimeModel, Method: "generateContent"})
	if err != nil || bound.Account().ID != accounts[0] {
		t.Fatalf("文件绑定的请求应能使用排空中的账号: lease=%v err=%v", bound, err)
	}
	_ = bound.Release()
	release()
	if !waitUntil(3*time.Second, func() bool { return !slices.Contains(env.manager.WarmAccountIDs(), accounts[0]) }) {
		t.Fatalf("租约释放后排空的 Worker 应被淘汰，驻留 Worker = %v", sortedWarm(env.manager))
	}
	time.Sleep(500 * time.Millisecond)
	if got := sortedWarm(env.manager); len(got) != 4 {
		t.Fatalf("淘汰后驻留数应保持 4（WARM_WORKER_LIMIT=2 时预热不补），实际 %v", got)
	}
	if env.pool.Draining(accounts[0]) {
		t.Fatal("淘汰后排空标记应被清除")
	}
}

// TestDrainClearedOnExit 排空在每条退出路径上清除：冷却被清除（放弃淘汰）、排空超时（推迟再试）、ResetAll 与 Close
func TestDrainClearedOnExit(t *testing.T) {
	accounts := []string{"drain-a@example.com", "drain-b@example.com", "drain-c@example.com"}
	t.Run("冷却清除", func(t *testing.T) {
		env := newDrainEnv(t, accounts, 2, 2)
		env.promote(t, accounts[:2]...)
		release := env.startBusyDrain(t, accounts[0])
		defer release()
		if err := env.pool.ClearCooldownIfGeneration(accounts[0], drainProModel, env.pool.ModelAccessGeneration(accounts[0]), time.Now()); err != nil {
			t.Fatal(err)
		}
		if !waitUntil(3*time.Second, func() bool { return !env.pool.Draining(accounts[0]) && env.manager.retirementCount() == 0 }) {
			t.Fatalf("冷却清除后排空与登记应被放弃: 排空=%v 登记=%d", env.pool.Draining(accounts[0]), env.manager.retirementCount())
		}
		if !slices.Contains(env.manager.WarmAccountIDs(), accounts[0]) {
			t.Fatal("冷却清除后不应淘汰 Worker")
		}
	})
	t.Run("排空超时", func(t *testing.T) {
		env := newDrainEnv(t, accounts, 2, 2)
		env.manager.requestTimeout.Store(int64(time.Second))
		env.promote(t, accounts[:2]...)
		release := env.startBusyDrain(t, accounts[0])
		defer release()
		drain := env.manager.drainOf(accounts[0])
		if drain == nil {
			t.Fatal("应有排空状态")
		}
		drain.startedAt.Store(time.Now().Add(-env.manager.drainTimeout() - time.Second).UnixNano())
		env.manager.wakeRetire()
		if !waitUntil(5*time.Second, func() bool { return !env.pool.Draining(accounts[0]) }) {
			t.Fatal("排空超时后标记应被清除")
		}
		// 日志在清除标记之后写入
		if !waitUntil(time.Second, func() bool { return hasLogContaining(env.requests, "排空超时") }) {
			t.Fatalf("应记录排空超时，日志 = %v", serviceLogMessages(env.requests))
		}
		env.manager.retireMu.Lock()
		entry := env.manager.retirements[accounts[0]]
		env.manager.retireMu.Unlock()
		if entry == nil || time.Until(entry.retryAt) < 30*time.Second {
			t.Fatalf("超时后登记应保留并推迟约 1 分钟再试: %+v", entry)
		}
		time.Sleep(600 * time.Millisecond)
		if env.pool.Draining(accounts[0]) {
			t.Fatal("推迟期间不应再次排空")
		}
	})
	t.Run("ResetAll", func(t *testing.T) {
		env := newDrainEnv(t, accounts, 2, 2)
		env.promote(t, accounts[:2]...)
		release := env.startBusyDrain(t, accounts[0])
		defer release()
		if err := env.manager.ResetAll(); err != nil {
			t.Fatal(err)
		}
		if env.pool.Draining(accounts[0]) || env.manager.drainCount(aistudio.PoolScopeNormal) != 0 {
			t.Fatal("ResetAll 后不应留下排空")
		}
	})
	t.Run("Close", func(t *testing.T) {
		env := newDrainEnv(t, accounts, 2, 2)
		env.promote(t, accounts[:2]...)
		release := env.startBusyDrain(t, accounts[0])
		defer release()
		if err := env.manager.Close(); err != nil {
			t.Fatal(err)
		}
		if env.pool.Draining(accounts[0]) || env.manager.drainCount(aistudio.PoolScopeNormal) != 0 {
			t.Fatal("Close 后不应留下排空")
		}
	})
}

// TestRetirementNeedsSpareForSameModel 唯一可接替的账号对同一模型也已冷却时不淘汰、不排空（不会把死槽位换成另一个死槽位）；
// 按旧口径（不看模型）它会被算作可接替
func TestRetirementNeedsSpareForSameModel(t *testing.T) {
	accounts := []string{"drain-a@example.com", "drain-b@example.com"}
	env := newDrainEnv(t, accounts, 1, 2)
	env.promote(t, accounts[0])
	env.coolPro(t, accounts[1])
	occupied := map[string]struct{}{accounts[0]: {}}
	if got := env.pool.SpareAccounts(aistudio.PoolScopeNormal, occupied, nil, rotationMinRemaining, 0); got != 1 {
		t.Fatalf("不看模型时 B 可接替，实际 %d", got)
	}
	if got := env.pool.SpareAccounts(aistudio.PoolScopeNormal, occupied, []string{drainProModel}, rotationMinRemaining, 0); got != 0 {
		t.Fatalf("按主用模型统计时 B 不可接替，实际 %d", got)
	}
	env.retirePro(t, accounts[0])
	time.Sleep(600 * time.Millisecond)
	if !slices.Contains(env.manager.WarmAccountIDs(), accounts[0]) || env.manager.retirementCount() != 1 {
		t.Fatalf("唯一可接替账号对同一模型冷却时不应淘汰: 驻留 %v 登记 %d", sortedWarm(env.manager), env.manager.retirementCount())
	}
	release := holdLease(t, env.pool, accounts[0])
	defer release()
	env.manager.retireMu.Lock()
	env.manager.retirements[accounts[0]].retryAt = time.Time{}
	env.manager.retireMu.Unlock()
	env.manager.wakeRetire()
	time.Sleep(600 * time.Millisecond)
	if env.pool.Draining(accounts[0]) {
		t.Fatal("没有按模型可接替的账号时不应排空")
	}
}

// TestOtherModelsPreferDeadSlots 其他模型的请求先落在对主用模型是死槽位的 Worker 上，主用模型的请求随后立即取得还能服务它的 Worker；
// 死槽位的租约位用完后其他模型的请求再用其余 Worker
func TestOtherModelsPreferDeadSlots(t *testing.T) {
	accounts := []string{"drain-c@example.com", "drain-d@example.com"}
	env := newDrainEnv(t, accounts, 2, 2)
	env.promote(t, accounts...)
	dead, capable := accounts[1], accounts[0]
	env.coolPro(t, dead)
	flash := aistudio.AccountSelection{ModelID: testRuntimeModel, Method: "generateContent"}
	var leases []*aistudio.AccountLease
	defer func() {
		for _, lease := range leases {
			_ = lease.Release()
		}
	}()
	for range 2 {
		lease, err := env.service.acquireWarmLease(env.ctx, flash)
		if err != nil {
			t.Fatal(err)
		}
		leases = append(leases, lease)
		if lease.Account().ID != dead {
			t.Fatalf("其他模型的请求应先用主用模型的死槽位 %s，实际 %s", dead, lease.Account().ID)
		}
	}
	startedAt := time.Now()
	pro, err := env.service.acquireWarmLease(env.ctx, aistudio.AccountSelection{ModelID: drainProModel, Method: "generateContent"})
	if err != nil {
		t.Fatal(err)
	}
	leases = append(leases, pro)
	if pro.Account().ID != capable || time.Since(startedAt) > 200*time.Millisecond {
		t.Fatalf("主用模型的请求应立即取得 %s，实际 %s，用时 %s", capable, pro.Account().ID, time.Since(startedAt))
	}
	spill, err := env.service.acquireWarmLease(env.ctx, flash)
	if err != nil {
		t.Fatal(err)
	}
	leases = append(leases, spill)
	if spill.Account().ID != capable {
		t.Fatalf("死槽位满后其他模型的请求应用其余 Worker，实际 %s", spill.Account().ID)
	}
}

// TestRequestPathStandbyAvoidsPrimaryCooled 请求路径为其他模型启动备用账号（后台扩容与现场启动）时优先没有在主用模型上冷却的账号，
// 不直接启动一个主用模型已冷却的死槽位；预热的排序同样考虑主用模型（重启后还没学到热门模型时也有效）
func TestRequestPathStandbyAvoidsPrimaryCooled(t *testing.T) {
	accounts := []string{"drain-s1@example.com", "drain-s2@example.com", "drain-w@example.com"}
	env := newDrainEnv(t, accounts, 1, 3)
	env.promote(t, accounts[2])
	env.coolPro(t, accounts[0])
	groups, err := env.pool.ClassifyCandidates(env.ctx, aistudio.AccountSelection{ModelID: testRuntimeModel, Method: "generateContent"}, env.manager.ReadyWarmAccountIDs())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(groups.StandbyReady, accounts[:2]) {
		t.Fatalf("备用账号应按 ID 排列: %v", groups.StandbyReady)
	}
	capacity := env.manager.standbyCapacity(env.manager.WarmAccountIDs(), testRuntimeModel)
	if got := capacity.expandable(groups.StandbyReady); got != accounts[1] {
		t.Fatalf("后台扩容应选主用模型没有冷却的 %s，实际 %q", accounts[1], got)
	}
	if got := capacity.promotable(groups.StandbyReady, false); got != accounts[1] {
		t.Fatalf("现场启动应选主用模型没有冷却的 %s，实际 %q", accounts[1], got)
	}
	if got := env.manager.preferUncooled(groups.StandbyReady); !slices.Equal(got, []string{accounts[1], accounts[0]}) {
		t.Fatalf("预热排序应考虑主用模型: %v", got)
	}
	// 端到端：唯一的热 Worker 被占满，其他模型的请求扩容启动的是 s2
	releaseFirst := holdLease(t, env.pool, accounts[2])
	defer releaseFirst()
	releaseSecond := holdLease(t, env.pool, accounts[2])
	defer releaseSecond()
	lease, err := env.service.acquireWarmLease(env.ctx, aistudio.AccountSelection{ModelID: testRuntimeModel, Method: "generateContent"})
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	if lease.Account().ID != accounts[1] || slices.Contains(env.manager.WarmAccountIDs(), accounts[0]) {
		t.Fatalf("应启动 %s，实际租约 %s，驻留 Worker = %v", accounts[1], lease.Account().ID, sortedWarm(env.manager))
	}
}

// TestReaperPrefersDeadSlot 空闲回收先关闭在主用模型上长时间冷却的死槽位，即使还能服务主用模型的 Worker 更久没用
func TestReaperPrefersDeadSlot(t *testing.T) {
	accounts := []string{"drain-a@example.com", "drain-b@example.com"}
	env := newDrainEnv(t, accounts, 1, 3)
	env.promote(t, accounts...)
	env.coolPro(t, accounts[0])
	holdLease(t, env.pool, accounts[1])()
	time.Sleep(10 * time.Millisecond)
	holdLease(t, env.pool, accounts[0])()
	if !env.manager.reapIdleWorker(time.Now().Add(time.Hour)) {
		t.Fatal("超出常驻数时应回收一个空闲 Worker")
	}
	if got := sortedWarm(env.manager); !slices.Equal(got, []string{accounts[1]}) {
		t.Fatalf("应回收主用模型的死槽位 A，留下 B，实际 %v", got)
	}
}

// TestRegisterDeadWorkers 驻留 Worker 中的死槽位每个周期补登记（重启后、其他进程写入的冷却同样登记）：全局冷却按全局、主用模型冷却按模型，
// 不是热门或主用的模型不登记，热门模型学到后再登记；登记数与状态页的冷却占位一致，重复调用不重复登记
func TestRegisterDeadWorkers(t *testing.T) {
	accounts := []string{"drain-a@example.com", "drain-b@example.com", "drain-c@example.com", "drain-d@example.com"}
	env := newDrainEnv(t, accounts, 4, 4)
	env.promote(t, accounts...)
	coolGlobally(t, env.pool, accounts[0])
	env.coolPro(t, accounts[1])
	until := time.Now().Add(2 * time.Hour)
	if err := env.pool.MarkCooldownIfGeneration(accounts[2], testRuntimeModel, env.pool.ModelAccessGeneration(accounts[2]), time.Now(), until, "每日限额"); err != nil {
		t.Fatal(err)
	}
	if got := env.service.registerDeadWorkers(); got != 2 {
		t.Fatalf("应补登记 2 个死槽位，实际 %d", got)
	}
	env.manager.retireMu.Lock()
	global, scoped := env.manager.retirements[accounts[0]], env.manager.retirements[accounts[1]]
	_, other := env.manager.retirements[accounts[2]]
	env.manager.retireMu.Unlock()
	if global == nil || !global.global() || scoped == nil || !slices.Equal(scoped.models, []string{drainProModel}) || other {
		t.Fatalf("登记范围不对: 全局=%+v 主用=%+v 其他模型已登记=%v", global, scoped, other)
	}
	admin := &runtimeAdmin{lifecycle: env.ctx, pool: env.pool, service: env.service, requests: env.requests, workers: env.manager}
	status, err := admin.Status(env.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if status.Workers.Dead != env.manager.retirementCount() {
		t.Fatalf("状态页的冷却占位 %d 应与登记数 %d 一致", status.Workers.Dead, env.manager.retirementCount())
	}
	if got := env.service.registerDeadWorkers(); got != 0 {
		t.Fatalf("重复调用不应新增登记，实际 %d", got)
	}
	env.manager.setHotModels([]string{testRuntimeModel})
	if got := env.service.registerDeadWorkers(); got != 1 {
		t.Fatalf("热门模型学到后应再登记 1 个，实际 %d", got)
	}
	if !hasLogContaining(env.requests, "补登记淘汰") {
		t.Fatalf("应记录补登记日志，日志 = %v", serviceLogMessages(env.requests))
	}
}

// TestCapacityCountsOpeningsAndDrains 容量判断口径一致：备用账户的分区容量同 ensureWorker 一样把启动中的 Worker 计入占用
// （分区被启动中的 Worker 占满时不再发起注定失败的后台扩容），排空中的 Worker 按已让出计，浏览器总数最多 峰值 + drainLimit
func TestCapacityCountsOpeningsAndDrains(t *testing.T) {
	accounts := []string{"drain-a@example.com", "drain-b@example.com", "drain-c@example.com", "drain-d@example.com"}
	env := newDrainEnv(t, accounts, 1, 2)
	env.promote(t, accounts[0])
	env.manager.rebalanceMu.Lock()
	env.manager.openings[accounts[1]] = make(chan struct{})
	env.manager.openingSet.Store(accounts[1], struct{}{})
	env.manager.rebalanceMu.Unlock()
	defer func() {
		env.manager.rebalanceMu.Lock()
		delete(env.manager.openings, accounts[1])
		env.manager.openingSet.Delete(accounts[1])
		env.manager.rebalanceMu.Unlock()
	}()
	capacity := env.manager.standbyCapacity(env.manager.WarmAccountIDs(), testRuntimeModel)
	if got := capacity.expandable([]string{accounts[2]}); got != "" {
		t.Fatalf("驻留 1 个加启动中 1 个已占满峰值 2，不应再扩容，实际 %q", got)
	}
	if _, err := env.manager.ensureWorker(withImmediateStartup(env.ctx), accounts[2], testRuntimeModel, false); !errors.Is(err, errAccountWorkerCapacity) {
		t.Fatalf("分区已满时不应启动: err = %v", err)
	}
	if drain := env.manager.startDrain(accounts[0], aistudio.PoolScopeNormal); drain == nil {
		t.Fatal("应能排空")
	}
	defer env.manager.clearDrain(accounts[0])
	capacity = env.manager.standbyCapacity(env.manager.WarmAccountIDs(), testRuntimeModel)
	if got := capacity.expandable([]string{accounts[2]}); got != accounts[2] {
		t.Fatalf("排空中的 Worker 按已让出计，应能扩容，实际 %q", got)
	}
	if _, err := env.manager.ensureWorker(withImmediateStartup(env.ctx), accounts[2], testRuntimeModel, false); err != nil {
		t.Fatalf("排空让出槽位后应能启动: %v", err)
	}
	if _, err := env.manager.ensureWorker(withImmediateStartup(env.ctx), accounts[3], testRuntimeModel, false); !errors.Is(err, errAccountWorkerCapacity) {
		t.Fatalf("峰值 + drainLimit 已满时不应再启动: err = %v", err)
	}
	if got := env.manager.startDrain(accounts[2], aistudio.PoolScopeNormal); got != nil {
		t.Fatal("分区排空数达到 drainLimit 时不应再排空")
	}
}

// TestDrainLimit 同时排空的上限：max(WARM_STARTUP_CONCURRENCY, 1) × 2，且不超过分区峰值的 1/4（至少 1）
func TestDrainLimit(t *testing.T) {
	manager, _, _ := partitionTestManager(t, []string{partitionNormalA}, nil, 1, 4, 0, 0)
	cases := []struct {
		concurrency, maxActive, want int
	}{
		{2, 4, 1}, {2, 40, 4}, {8, 150, 16}, {8, 40, 10}, {0, 100, 2}, {2, 3, 1},
	}
	for _, testCase := range cases {
		manager.warmConcurrency.Store(int64(testCase.concurrency))
		manager.maxActive.Store(int64(testCase.maxActive))
		if got := manager.drainLimit(aistudio.PoolScopeNormal); got != testCase.want {
			t.Fatalf("并发 %d 峰值 %d：drainLimit = %d，期望 %d", testCase.concurrency, testCase.maxActive, got, testCase.want)
		}
	}
	if got := manager.drainLimit(aistudio.PoolScopeUltra); got != 0 {
		t.Fatalf("峰值为 0 的分区不排空，实际 %d", got)
	}
}

// TestDrainReplacementNotDoubled 分区离峰值还有空槽时，排空开始时启动的替补就是这个死槽位的替补：旧 Worker 排空关闭后不再多补一个，
// 驻留数保持排空前的 4（WARM_WORKER_LIMIT=2、峰值 8）
func TestDrainReplacementNotDoubled(t *testing.T) {
	accounts := []string{"drain-a@example.com", "drain-b@example.com", "drain-c@example.com", "drain-d@example.com",
		"drain-e@example.com", "drain-f@example.com", "drain-g@example.com"}
	env := newDrainEnv(t, accounts, 2, 8)
	env.promote(t, accounts[:4]...)
	release := env.startBusyDrain(t, accounts[0])
	defer release()
	if !waitUntil(3*time.Second, func() bool { return len(env.manager.WarmAccountIDs()) == 5 }) {
		t.Fatalf("排空开始时应启动替补，驻留 Worker = %v", sortedWarm(env.manager))
	}
	release()
	if !waitUntil(3*time.Second, func() bool { return !slices.Contains(env.manager.WarmAccountIDs(), accounts[0]) }) {
		t.Fatalf("租约释放后排空的 Worker 应被淘汰，驻留 Worker = %v", sortedWarm(env.manager))
	}
	time.Sleep(500 * time.Millisecond)
	if got := sortedWarm(env.manager); len(got) != 4 {
		t.Fatalf("排空淘汰后不应再补第二个替补，驻留 Worker = %v", got)
	}
}

// TestRetiredSlotReplacedAboveWarmLimit WARM_WORKER_LIMIT（2）低于驻留数（4）时预热不会补齐淘汰空出的槽位：空闲死槽位的冷却淘汰与
// 全局冷却的清扫之后都由替补补上，驻留数保持 4，替补对主用模型没有冷却；低于预热目标时由预热补齐，不重复启动替补
func TestRetiredSlotReplacedAboveWarmLimit(t *testing.T) {
	accounts := []string{"drain-a@example.com", "drain-b@example.com", "drain-c@example.com", "drain-d@example.com",
		"drain-e@example.com", "drain-f@example.com", "drain-g@example.com"}
	env := newDrainEnv(t, accounts, 2, 6)
	env.promote(t, accounts[:4]...)
	env.coolPro(t, accounts[5])
	env.retirePro(t, accounts[0])
	if !waitUntil(3*time.Second, func() bool {
		warm := env.manager.WarmAccountIDs()
		return !slices.Contains(warm, accounts[0]) && len(warm) == 4
	}) {
		t.Fatalf("空闲死槽位淘汰后应由替补补齐到 4，驻留 Worker = %v", sortedWarm(env.manager))
	}
	if slices.Contains(env.manager.WarmAccountIDs(), accounts[5]) {
		t.Fatal("替补不应选主用模型已冷却的账号")
	}
	if !hasLogContaining(env.requests, "替补启动", "原因=冷却淘汰") {
		t.Fatalf("应记录替补日志，日志 = %v", serviceLogMessages(env.requests))
	}
	coolGlobally(t, env.pool, accounts[1])
	env.manager.dropRetirement(accounts[1])
	if got := env.service.sweepDeadWorkers(env.ctx); got != 1 {
		t.Fatalf("清扫应关闭全局冷却的 B，实际 %d", got)
	}
	if !waitUntil(3*time.Second, func() bool {
		warm := env.manager.WarmAccountIDs()
		return !slices.Contains(warm, accounts[1]) && len(warm) == 4
	}) {
		t.Fatalf("清扫后应由替补补齐到 4，驻留 Worker = %v", sortedWarm(env.manager))
	}
	time.Sleep(300 * time.Millisecond)
	if got := len(env.manager.WarmAccountIDs()); got != 4 {
		t.Fatalf("每个淘汰只补一个，驻留数应为 4，实际 %d", got)
	}
}

// TestDrainRefusedAfterWorkerClosed 排空只写给仍有驻留 Worker、没有被其他路径选中淘汰的账号：retirePass 核对驻留之后，
// 现场回收或清扫可能已关闭 Worker 并删除登记，随后写入的排空没有任何路径再清除（账号一直按排空中计、占着 drainLimit，
// 不再参与不固定账户的调度），startDrain 因此在 drainMu 内再核对一次
func TestDrainRefusedAfterWorkerClosed(t *testing.T) {
	accounts := []string{"drain-a@example.com", "drain-b@example.com", "drain-c@example.com", "drain-d@example.com"}
	env := newDrainEnv(t, accounts, 2, 4)
	env.promote(t, accounts[0], accounts[1])
	env.coolPro(t, accounts[0])
	// 现场回收关闭 A（retirePass 已核对过 A 驻留、正要排空）
	if victim, err := env.manager.reclaimDeadWorker(env.ctx, aistudio.PoolScopeNormal, drainProModel); err != nil || victim != accounts[0] {
		t.Fatalf("应回收 A: victim=%q err=%v", victim, err)
	}
	if drain := env.manager.startDrain(accounts[0], aistudio.PoolScopeNormal); drain != nil {
		t.Fatal("Worker 已关闭的账号不应再排空")
	}
	if env.pool.Draining(accounts[0]) || env.manager.drainOf(accounts[0]) != nil || env.manager.drainCount(aistudio.PoolScopeNormal) != 0 {
		t.Fatalf("不应留下排空: 账户池=%v 排空状态=%v 排空数=%d",
			env.pool.Draining(accounts[0]), env.manager.drainOf(accounts[0]), env.manager.drainCount(aistudio.PoolScopeNormal))
	}
	// 已被其他路径选中淘汰的账号同样不排空（不另起排空的替补）
	if !env.manager.tryReserveVictim(accounts[1]) {
		t.Fatal("应能预留 B")
	}
	if drain := env.manager.startDrain(accounts[1], aistudio.PoolScopeNormal); drain != nil {
		t.Fatal("已被选中淘汰的账号不应排空")
	}
	env.manager.releaseVictim(accounts[1])
	if env.pool.Draining(accounts[1]) {
		t.Fatal("已被选中淘汰的账号不应标记排空")
	}
	// 对照：仍驻留、没有被选中的账号照常排空
	if drain := env.manager.startDrain(accounts[1], aistudio.PoolScopeNormal); drain == nil || !env.pool.Draining(accounts[1]) {
		t.Fatal("驻留的账号应能排空")
	}
	env.manager.clearDrain(accounts[1])
}

// TestReaperSkipsRecentlyUsedDeadSlots 空闲回收只在空闲超时的 Worker 中挑选：其他模型的请求经调度落在主用模型的死槽位上
// （先用死槽位），死槽位刚用过时回收不因它们排在最前而整轮放弃，空闲超时、超出常驻数的 Worker 照常关闭，刚用过的死槽位保留
func TestReaperSkipsRecentlyUsedDeadSlots(t *testing.T) {
	accounts := []string{"drain-a@example.com", "drain-b@example.com", "drain-c@example.com", "drain-d@example.com"}
	env := newDrainEnv(t, accounts, 1, 4)
	env.promote(t, accounts...)
	env.coolPro(t, accounts[0])
	env.coolPro(t, accounts[1])
	for _, accountID := range accounts {
		holdLease(t, env.pool, accountID)()
	}
	time.Sleep(20 * time.Millisecond)
	cutoff := time.Now()
	time.Sleep(20 * time.Millisecond)
	flash := aistudio.AccountSelection{ModelID: testRuntimeModel, Method: "generateContent"}
	for range 6 {
		lease, err := env.service.acquireWarmLease(env.ctx, flash)
		if err != nil {
			t.Fatal(err)
		}
		if id := lease.Account().ID; id != accounts[0] && id != accounts[1] {
			t.Fatalf("其他模型的请求应先用死槽位，实际 %s", id)
		}
		_ = lease.Release()
	}
	reaped := 0
	for env.manager.reapIdleWorker(cutoff) {
		reaped++
	}
	if got := sortedWarm(env.manager); reaped != 2 || !slices.Equal(got, accounts[:2]) {
		t.Fatalf("应回收空闲超时的 C、D，保留刚用过的死槽位：回收 %d，驻留 Worker = %v", reaped, got)
	}
}

// drainNicheModel 为既不是主用也不是热门的模型
const drainNicheModel = "drain-niche-model"

// TestNicheModelForbiddenDoesNotDrain 只在少用的模型上冷却的 Worker（如账号对该模型没有权限的 403）还能服务主用模型：忙碌时不排空，
// 不固定账户的主用模型请求照常分给它；登记保留，空闲后照常淘汰
func TestNicheModelForbiddenDoesNotDrain(t *testing.T) {
	a, b := "drain-a@example.com", "drain-b@example.com"
	// 换号时 C、D 可能都被扩容启动（峰值 4），E 保证 A 空闲后仍有可接替的账号
	accounts := []string{a, b, "drain-c@example.com", "drain-d@example.com", "drain-e@example.com"}
	env := newDrainEnv(t, accounts, 2, 4)
	catalog := []aistudio.Model{
		{ID: testRuntimeModel, Methods: []string{"generateContent"}, Capabilities: map[string]bool{"chat_model": true}},
		{ID: drainProModel, Methods: []string{"generateContent"}, Capabilities: map[string]bool{"chat_model": true}},
		{ID: drainNicheModel, Methods: []string{"generateContent"}, Capabilities: map[string]bool{"chat_model": true}},
	}
	for _, accountID := range accounts {
		if err := env.pool.SetCatalog(accountID, aistudio.BenefitTierFree, catalog); err != nil {
			t.Fatal(err)
		}
	}
	env.pool.SetPerAccountConcurrency(2)
	upstream := &forbiddenUpstream{}
	env.service.service = &modelDeniedUpstream{forbiddenUpstream: upstream, accountID: a, modelID: drainNicheModel}
	env.service.timeout.Store(int64(time.Minute))
	env.promote(t, a, b)
	// A 正在服务一个请求，B 满载：少用模型的请求落在 A 上，403 后换号完成
	releaseA := holdLease(t, env.pool, a)
	defer releaseA()
	releaseB1 := holdLease(t, env.pool, b)
	releaseB2 := holdLease(t, env.pool, b)
	if text, err, _ := runModelRequest(t, env.service, "req-niche", "", drainNicheModel); err != nil || text != "hello" {
		t.Fatalf("少用模型的请求应换号完成：text=%q err=%v calls=%v", text, err, upstream.called())
	}
	if calls := upstream.called(); len(calls) == 0 || calls[0] != a {
		t.Fatalf("第一次尝试应落在 A 上：%v", calls)
	}
	if env.manager.retirementCount() != 1 {
		t.Fatalf("A 在少用模型上冷却后应登记淘汰，登记数 = %d", env.manager.retirementCount())
	}
	env.manager.wakeRetire()
	time.Sleep(600 * time.Millisecond)
	if env.pool.Draining(a) || env.manager.drainCount(aistudio.PoolScopeNormal) != 0 {
		t.Fatal("只在少用模型上冷却的忙碌 Worker 不应排空")
	}
	releaseB1()
	releaseB2()
	lease, _, err := env.pool.TryAcquireFor(env.ctx, aistudio.AccountSelection{
		ModelID: drainProModel, Method: "generateContent", AllowedAccountIDs: []string{a},
	})
	if err != nil || lease == nil {
		t.Fatalf("不固定账户的主用模型请求应照常分给 A：lease=%v err=%v", lease, err)
	}
	_ = lease.Release()
	if env.manager.retirementCount() != 1 {
		t.Fatalf("登记应保留，登记数 = %d", env.manager.retirementCount())
	}
	releaseA()
	if !waitUntil(3*time.Second, func() bool { return !slices.Contains(env.manager.WarmAccountIDs(), a) }) {
		t.Fatalf("A 空闲后应照常淘汰，驻留 Worker = %v", sortedWarm(env.manager))
	}
}

// modelDeniedUpstream 只让 accountID 对 modelID 返回 403 无权限，其余照常；调用记在 forbiddenUpstream 中
type modelDeniedUpstream struct {
	*forbiddenUpstream
	accountID string
	modelID   string
}

func (upstream *modelDeniedUpstream) Generate(ctx context.Context, request aistudio.GenerateRequest) (<-chan aistudio.Event, error) {
	if request.AccountID != upstream.accountID || strings.TrimPrefix(request.Model, "models/") != upstream.modelID {
		return upstream.forbiddenUpstream.Generate(ctx, request)
	}
	upstream.mu.Lock()
	upstream.calls = append(upstream.calls, request.AccountID)
	upstream.mu.Unlock()
	return nil, &aistudio.RPCError{
		Method: "GenerateContent", StatusCode: http.StatusForbidden, Code: 7, Message: "The caller does not have permission",
	}
}

// TestStandbyPreferenceNeedsPrimarySupport 备用账号与预热的排序按“能否服务主用模型”判断，不是只看有没有冷却：
// 目录里根本没有主用模型的账号（没有冷却记录）排在能服务主用模型的账号之后，不会因为“没冷却”被优先启动成一个服务不了主用模型的 Worker
func TestStandbyPreferenceNeedsPrimarySupport(t *testing.T) {
	accounts := []string{"drain-n1@example.com", "drain-n2@example.com", "drain-w@example.com"}
	env := newDrainEnv(t, accounts, 1, 3)
	// n1 的目录只有其他模型，没有主用模型，也没有任何冷却；n2 能服务主用模型
	if err := env.pool.SetCatalog(accounts[0], aistudio.BenefitTierFree, []aistudio.Model{
		{ID: testRuntimeModel, Methods: []string{"generateContent"}, Capabilities: map[string]bool{"chat_model": true}},
	}); err != nil {
		t.Fatal(err)
	}
	env.promote(t, accounts[2])
	groups, err := env.pool.ClassifyCandidates(env.ctx, aistudio.AccountSelection{ModelID: testRuntimeModel, Method: "generateContent"}, env.manager.ReadyWarmAccountIDs())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(groups.StandbyReady, accounts[:2]) {
		t.Fatalf("备用账号应按 ID 排列: %v", groups.StandbyReady)
	}
	if env.pool.AccountLongCooling(accounts[0], drainProModel, rotationMinRemaining) {
		t.Fatalf("%s 没有冷却记录，只看冷却会把它当成能服务主用模型", accounts[0])
	}
	capacity := env.manager.standbyCapacity(env.manager.WarmAccountIDs(), testRuntimeModel)
	if got := capacity.expandable(groups.StandbyReady); got != accounts[1] {
		t.Fatalf("后台扩容应选能服务主用模型的 %s，实际 %q", accounts[1], got)
	}
	if got := capacity.promotable(groups.StandbyReady, false); got != accounts[1] {
		t.Fatalf("现场启动应选能服务主用模型的 %s，实际 %q", accounts[1], got)
	}
	if got := env.manager.preferUncooled(groups.StandbyReady); !slices.Equal(got, []string{accounts[1], accounts[0]}) {
		t.Fatalf("预热排序应把能服务主用模型的账号排在前面: %v", got)
	}
}

// TestRuntimeAvailableLockFree 调度过滤占用退避中的账户只读原子值、不取账户锁：账户锁被别处占着时也不阻塞，
// 也就不会让 occupiedWorkersIn 把正被读取的备用账户误算成浏览器槽位；占用退避期内的账户被排除并返回原因，解除后恢复
func TestRuntimeAvailableLockFree(t *testing.T) {
	accounts := []string{"drain-x@example.com", "drain-y@example.com"}
	env := newDrainEnv(t, accounts, 1, 2)
	env.manager.mu.RLock()
	busy := env.manager.accounts[accounts[0]]
	env.manager.mu.RUnlock()
	if err := env.manager.markRuntimeBusy(busy, errors.New("另一个进程持有运行锁")); !errors.Is(err, errAccountRuntimeBusy) {
		t.Fatalf("markRuntimeBusy 返回 %v", err)
	}
	busy.mu.Lock()
	result := make(chan []string, 1)
	var reason error
	go func() {
		available, err := env.manager.runtimeAvailable(accounts)
		reason = err
		result <- available
	}()
	select {
	case available := <-result:
		busy.mu.Unlock()
		if !slices.Equal(available, accounts[1:]) || !errors.Is(reason, errAccountRuntimeBusy) || !strings.Contains(reason.Error(), "另一个进程持有运行锁") {
			t.Fatalf("可用账户 %v，原因 %v；期望排除占用中的 %s 并返回原因", available, reason, accounts[0])
		}
	case <-time.After(2 * time.Second):
		busy.mu.Unlock()
		t.Fatal("runtimeAvailable 不应等待账户锁")
	}
	env.manager.clearRuntimeBusy(busy)
	if available, err := env.manager.runtimeAvailable(accounts); !slices.Equal(available, accounts) || err != nil {
		t.Fatalf("解除占用后可用账户 %v，原因 %v", available, err)
	}
}
