package app

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/config"
)

// 补充池（见 extra_pool.go）批 1 的 app 层测试：叠加视图、进入门槛、配置的读写与生效比较、热更新只存值

// samePartitionSet 判断两个分区快照是同一个 map 对象
func samePartitionSet(a, b aistudio.PartitionSet) bool {
	return reflect.ValueOf(a).UnsafePointer() == reflect.ValueOf(b).UnsafePointer()
}

// tagExtra 在 rebalanceMu 内给账户打上或去掉补充池标记（与生产代码的锁序相同）
func tagExtra(manager *accountWorkerManager, accountID string, on bool) {
	manager.rebalanceMu.Lock()
	defer manager.rebalanceMu.Unlock()
	manager.setExtraOwnedLocked(accountID, on)
}

// TestSlotMembersIdentityWhenEmpty 没有补充池标记时叠加视图就是账户池的分区快照（同一个 map 对象，与没有补充池时完全相同）；
// 有标记时带标记的账户记为补充池、其余同家分区，账户池的快照不被修改；只在标记版本或账户池快照变化时才重新克隆
func TestSlotMembersIdentityWhenEmpty(t *testing.T) {
	manager, pool, _ := proPartitionManager(t, []string{partitionNormalA}, []string{partitionUltraA}, []string{proPartitionA, proPartitionB}, 1, 2, 1, 2)
	home := pool.PartitionMembers()
	if !samePartitionSet(manager.slotMembers(), home) {
		t.Fatal("没有标记时叠加视图应直接返回账户池的快照")
	}
	tagExtra(manager, proPartitionA, true)
	first := manager.slotMembers()
	if samePartitionSet(first, home) || first.Of(proPartitionA) != aistudio.PoolScopeExtra || first.Of(proPartitionB) != aistudio.PoolScopePro ||
		first.Of(partitionUltraA) != aistudio.PoolScopeUltra || first.Of(partitionNormalA) != aistudio.PoolScopeNormal {
		t.Fatalf("有标记时叠加视图 = %v", first)
	}
	if home.Of(proPartitionA) != aistudio.PoolScopePro || !samePartitionSet(pool.PartitionMembers(), home) {
		t.Fatal("叠加视图不应修改账户池的快照")
	}
	if !samePartitionSet(manager.slotMembers(), first) {
		t.Fatal("标记与账户池快照都没变时应复用缓存，不重新克隆")
	}
	// 重复设置同一个值不改版本
	tagExtra(manager, proPartitionA, true)
	if !samePartitionSet(manager.slotMembers(), first) {
		t.Fatal("标记没有实际变化时不应重新克隆")
	}
	tagExtra(manager, partitionNormalA, true)
	second := manager.slotMembers()
	if samePartitionSet(second, first) || second.Of(partitionNormalA) != aistudio.PoolScopeExtra || manager.extraOwnedCount() != 2 {
		t.Fatalf("标记变化后应重新克隆：%v", second)
	}
	// 账户池成员变化（PRO_POOL 关闭）：快照重建，叠加视图随之重建，带标记的账户仍记为补充池
	pool.SetProPool(false)
	third := manager.slotMembers()
	if samePartitionSet(third, second) || third.Of(proPartitionA) != aistudio.PoolScopeExtra || third.Of(proPartitionB) != aistudio.PoolScopeNormal {
		t.Fatalf("账户池快照变化后叠加视图 = %v", third)
	}
	if got := manager.extraOwnedIDs(); len(got) != 2 || got[0] != partitionNormalA || got[1] != proPartitionA {
		t.Fatalf("标记 = %v", got)
	}
	tagExtra(manager, proPartitionA, false)
	manager.rebalanceMu.Lock()
	manager.clearExtraOwnedLocked(partitionNormalA)
	manager.clearExtraOwnedLocked("missing@example.com")
	manager.rebalanceMu.Unlock()
	if !samePartitionSet(manager.slotMembers(), pool.PartitionMembers()) || manager.extraOwnedCount() != 0 || manager.extraOwnedHas(proPartitionA) {
		t.Fatal("标记全部去掉后叠加视图应回到账户池的快照")
	}
}

// TestExtraEntryThresholdIsRotationMin 补充池的进入门槛与长冷却口径（淘汰、轮换）的门槛相同，以后不能只改一边
func TestExtraEntryThresholdIsRotationMin(t *testing.T) {
	if extraEntryMin != rotationMinRemaining {
		t.Fatalf("进入门槛 %s 与 rotationMinRemaining %s 不同", extraEntryMin, rotationMinRemaining)
	}
}

// TestExtraPartitionNames 补充池的容量与名称：关闭时容量为 0；开启后按设置；名称表有自己的写法；主用模型列表为普通与 Ultra 的并集
func TestExtraPartitionNames(t *testing.T) {
	manager, _, _ := proPartitionManager(t, []string{partitionNormalA}, nil, nil, 1, 1, 0, 1)
	if manager.warmTargetFor(aistudio.PoolScopeExtra) != 0 || manager.maxActiveFor(aistudio.PoolScopeExtra) != 0 {
		t.Fatal("没有写入设置时补充池容量应为 0")
	}
	settings := extraSettingsOf(config.Default())
	settings.warm, settings.max = 3, 4
	manager.setExtraSettings(settings)
	if manager.warmTargetFor(aistudio.PoolScopeExtra) != 0 || manager.maxActiveFor(aistudio.PoolScopeExtra) != 0 {
		t.Fatal("补充池关闭时容量应为 0")
	}
	settings.enabled = true
	manager.setExtraSettings(settings)
	if manager.warmTargetFor(aistudio.PoolScopeExtra) != 3 || manager.maxActiveFor(aistudio.PoolScopeExtra) != 4 {
		t.Fatal("补充池开启时容量应取设置")
	}
	if partitionLabel(aistudio.PoolScopeExtra) != "补充池" || workerLogName(aistudio.PoolScopeExtra) != "补充池 WAA Worker" ||
		workerSlotName(aistudio.PoolScopeExtra) != "补充池 Worker" || balancedPartitionName(aistudio.PoolScopeExtra) != "补充池" ||
		partitionLabel(aistudio.PoolScopeNormalUltra) != "普通分区" {
		t.Fatal("补充池的名称表不符，或其他非分区取值的名称变了")
	}
	manager.setPrimaryModels([]string{"gemini-a", "gemini-b"}, []string{"gemini-a", "gemini-c"})
	if got := manager.primaryModelListFor(aistudio.PoolScopeExtra); strings.Join(got, ",") != "gemini-a,gemini-b,gemini-c" {
		t.Fatalf("补充池的主用模型 = %v，期望普通与 Ultra 列表的并集", got)
	}
}

// TestRuntimeConfigExtraFields 管理接口的补充池设置：读取总是返回 6 个字段；旧版页面不带这些字段时沿用 .env 的值（漏掉时峰值为 0、
// 保存失败）；只带部分字段时其余沿用；方式写回小写；无效组合返回 400；生效比较与热更新比较都包含它们，只改它们不要求重启生成服务
func TestRuntimeConfigExtraFields(t *testing.T) {
	clearAppConfigEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte("EXTRA_POOL=true\nEXTRA_POOL_MODE=overflow\nEXTRA_POOL_DEFAULT_PAID=false\nEXTRA_POOL_INCLUDE_ULTRA=true\nEXTRA_WARM_WORKER_LIMIT=3\nEXTRA_MAX_ACTIVE_WORKERS=7\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	admin := &runtimeAdmin{configPath: path, requests: newRequestRegistry(ctx)}
	load := func() config.Config {
		t.Helper()
		saved, err := config.Load(path)
		if err != nil {
			t.Fatal(err)
		}
		return saved
	}
	want := extraConfigFields{true, "overflow", false, true, 3, 7}
	fieldsOf := func(cfg config.Config) extraConfigFields {
		return extraSettingsFromInput(runtimeConfigDTO(cfg), cfg)
	}
	legacy := validRuntimeConfig()
	legacy.ExtraPool, legacy.ExtraPoolMode, legacy.ExtraPoolDefaultPaid = nil, nil, nil
	legacy.ExtraPoolIncludeUltra, legacy.ExtraWarmWorkerLimit, legacy.ExtraMaxActiveWorkers = nil, nil, nil
	updated, err := admin.UpdateRuntimeConfig(ctx, legacy)
	if err != nil {
		t.Fatalf("旧版页面不带补充池字段时保存失败（cfg 字面量漏了字段？）: %v", err)
	}
	if got := fieldsOf(load()); got != want {
		t.Fatalf("旧版页面保存后 .env = %+v，期望沿用 %+v", got, want)
	}
	if updated.ExtraPool == nil || updated.ExtraPoolMode == nil || updated.ExtraPoolDefaultPaid == nil || updated.ExtraPoolIncludeUltra == nil ||
		updated.ExtraWarmWorkerLimit == nil || updated.ExtraMaxActiveWorkers == nil || *updated.ExtraMaxActiveWorkers != 7 {
		t.Fatalf("保存结果应总是带 6 个补充池字段: %+v", updated)
	}
	partial := validRuntimeConfig()
	partial.ExtraPoolMode, partial.ExtraPoolDefaultPaid, partial.ExtraPoolIncludeUltra = nil, nil, nil
	partial.ExtraWarmWorkerLimit, partial.ExtraMaxActiveWorkers = nil, nil
	off := false
	partial.ExtraPool = &off
	if _, err := admin.UpdateRuntimeConfig(ctx, partial); err != nil {
		t.Fatal(err)
	}
	if got := fieldsOf(load()); got != (extraConfigFields{false, "overflow", false, true, 3, 7}) {
		t.Fatalf("只提交开关时其余应沿用：%+v", got)
	}
	full := validRuntimeConfig()
	on, shouty, paid, ultra, warm, peak := true, " SHARE ", true, false, 0, 1
	full.ExtraPool, full.ExtraPoolMode, full.ExtraPoolDefaultPaid, full.ExtraPoolIncludeUltra = &on, &shouty, &paid, &ultra
	full.ExtraWarmWorkerLimit, full.ExtraMaxActiveWorkers = &warm, &peak
	if updated, err = admin.UpdateRuntimeConfig(ctx, full); err != nil || updated.ExtraPoolMode == nil || *updated.ExtraPoolMode != "share" {
		t.Fatalf("保存全部字段：%v %v", updated.ExtraPoolMode, err)
	}
	if got := fieldsOf(load()); got != (extraConfigFields{true, "share", true, false, 0, 1}) {
		t.Fatalf("保存全部字段后 .env = %+v", got)
	}
	data, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(data), "EXTRA_POOL_MODE=share\n") {
		t.Fatalf(".env 里方式应写小写：%s %v", data, err)
	}
	for _, mutate := range []func(*int, *int, *string){
		func(warm, peak *int, _ *string) { *warm, *peak = 5, 4 },
		func(_, peak *int, _ *string) { *peak = 0 },
		func(warm, _ *int, _ *string) { *warm = -1 },
		func(_, _ *int, mode *string) { *mode = "both" },
	} {
		invalid := validRuntimeConfig()
		bad, badPeak, badMode := 1, 2, "share"
		mutate(&bad, &badPeak, &badMode)
		invalid.ExtraWarmWorkerLimit, invalid.ExtraMaxActiveWorkers, invalid.ExtraPoolMode = &bad, &badPeak, &badMode
		var operation *adminOperationError
		if _, err := admin.UpdateRuntimeConfig(ctx, invalid); err == nil || !asAdminOperation(err, &operation) || operation.code != "invalid_config" {
			t.Fatalf("无效的补充池设置（常驻 %d、峰值 %d、方式 %q）应返回 invalid_config：%v", bad, badPeak, badMode, err)
		}
	}
	dto := runtimeConfigDTO(config.Default())
	if dto.ExtraPool == nil || *dto.ExtraPool || dto.ExtraPoolMode == nil || *dto.ExtraPoolMode != "share" || dto.ExtraPoolDefaultPaid == nil ||
		!*dto.ExtraPoolDefaultPaid || dto.ExtraPoolIncludeUltra == nil || *dto.ExtraPoolIncludeUltra || *dto.ExtraWarmWorkerLimit != 40 ||
		*dto.ExtraMaxActiveWorkers != 50 {
		t.Fatalf("默认配置的 DTO = %+v", dto)
	}
	// 生效比较：每个字段都参与热更新比较与生效配置比较，都按热更新处理；旧版页面不带这些字段时按生效值比较
	active := config.Default()
	for name, change := range map[string]func(*config.Config){
		"EXTRA_POOL":               func(cfg *config.Config) { cfg.ExtraPool = true },
		"EXTRA_POOL_MODE":          func(cfg *config.Config) { cfg.ExtraPoolMode = config.ExtraPoolModeOverflow },
		"EXTRA_POOL_DEFAULT_PAID":  func(cfg *config.Config) { cfg.ExtraPoolDefaultPaid = false },
		"EXTRA_POOL_INCLUDE_ULTRA": func(cfg *config.Config) { cfg.ExtraPoolIncludeUltra = true },
		"EXTRA_WARM_WORKER_LIMIT":  func(cfg *config.Config) { cfg.ExtraWarmWorkerLimit = 1 },
		"EXTRA_MAX_ACTIVE_WORKERS": func(cfg *config.Config) { cfg.ExtraMaxActiveWorkers = 60 },
	} {
		changed := active
		change(&changed)
		if liveFieldsEqual(changed, active) {
			t.Fatalf("liveFieldsEqual 应包含 %s，否则只改它的保存永远不生效", name)
		}
		if requiresRebuild(changed, active) {
			t.Fatalf("%s 应按热更新处理，不重建生成服务", name)
		}
		dto := runtimeConfigDTO(changed)
		if sameDataConfig(dto, active, dataConfigOverrides{}) || !sameDataConfig(dto, changed, dataConfigOverrides{}) {
			t.Fatalf("生效配置比较应包含 %s，否则管理页会误报需要重启", name)
		}
		dto.ExtraPool, dto.ExtraPoolMode, dto.ExtraPoolDefaultPaid = nil, nil, nil
		dto.ExtraPoolIncludeUltra, dto.ExtraWarmWorkerLimit, dto.ExtraMaxActiveWorkers = nil, nil, nil
		if !sameDataConfig(dto, active, dataConfigOverrides{}) || !sameDataConfig(dto, changed, dataConfigOverrides{}) {
			t.Fatalf("%s：旧版页面不带补充池字段时应按生效值比较", name)
		}
	}
	shouting := active
	shouting.ExtraPoolMode = "SHARE"
	if !liveFieldsEqual(shouting, active) {
		t.Fatal("方式按规范写法比较，大小写不同不算变化")
	}
}

// asAdminOperation 判断错误是管理接口的操作错误
func asAdminOperation(err error, target **adminOperationError) bool {
	operation, ok := err.(*adminOperationError)
	if ok {
		*target = operation
	}
	return ok
}

// TestExtraHotReloadStoresSettings 保存补充池设置后立即生效、不重启生成服务：Worker 管理器的设置与容量、账户池的补充池规则都按新值，
// 写一行补充池设置已更新（配置已热更新的汇总行不带补充池字段）；Worker 不重启。保存本身不启停任何 Worker（补充池预热在下一次
// keepWarm 的对账里进行，见 TestExtraHotToggle；这里的账户也都不是成员）。再保存相同的值不重复写；关闭后容量回到 0
func TestExtraHotReloadStoresSettings(t *testing.T) {
	env := newIncludeProRuntime(t)
	for _, accountID := range []string{includeProAccount, partitionUltraA} {
		if _, err := env.workers.promote(env.ctx, accountID, testRuntimeModel); err != nil {
			t.Fatal(err)
		}
	}
	workerOf := func(accountID string) any {
		env.workers.mu.RLock()
		account := env.workers.accounts[accountID]
		env.workers.mu.RUnlock()
		account.mu.Lock()
		defer account.mu.Unlock()
		return account.worker
	}
	before := map[string]any{includeProAccount: workerOf(includeProAccount), partitionUltraA: workerOf(partitionUltraA)}
	save := func(change func(*config.Config)) {
		t.Helper()
		change(&env.cfg)
		updated, err := env.manager.UpdateRuntimeConfig(env.ctx, runtimeConfigDTO(env.cfg))
		if err != nil {
			t.Fatal(err)
		}
		if updated.ServiceRestartRequired {
			t.Fatal("只改补充池设置不应要求重启生成服务")
		}
	}
	// 测试构造的账户池没有写过权益规则：先按配置写入（与装配时相同），之后只看补充池设置带来的版本变化
	env.pool.SetModelTierRules(env.cfg.UltraOnlyModels, env.cfg.PaidOnlyModels)
	version := env.pool.PoolRuleVersion()
	logs := len(serviceLogMessages(env.requests))
	save(func(cfg *config.Config) {
		cfg.ExtraPool, cfg.ExtraPoolMode, cfg.ExtraPoolIncludeUltra = true, config.ExtraPoolModeOverflow, true
		cfg.ExtraWarmWorkerLimit, cfg.ExtraMaxActiveWorkers = 3, 4
	})
	if got := env.workers.extraSettingsValue(); got != (extraPoolSettings{enabled: true, mode: "overflow", defaultPaid: true, includeUltra: true, warm: 3, max: 4}) {
		t.Fatalf("Worker 管理器的补充池设置 = %+v", got)
	}
	if env.workers.warmTargetFor(aistudio.PoolScopeExtra) != 3 || env.workers.maxActiveFor(aistudio.PoolScopeExtra) != 4 {
		t.Fatal("补充池容量应立即按新设置")
	}
	if got := env.pool.ExtraPoolLive(); got != (aistudio.ExtraPoolRules{Enabled: true, IncludeUltra: true, DefaultPaid: true}) {
		t.Fatalf("账户池的补充池规则 = %+v", got)
	}
	if env.pool.PoolRuleVersion() != version+1 {
		t.Fatalf("开启补充池应递增一次号池规则版本：%d → %d", version, env.pool.PoolRuleVersion())
	}
	added := serviceLogMessages(env.requests)[logs:]
	settingLine := "补充池设置已更新并立即生效 | 开启=是 | 方式=补充 | 默认地址借付费模型=是 | 含 Ultra=是 | 常驻 Worker=3 | 峰值 Worker=4"
	if countLogs(env.requests, settingLine) != 1 {
		t.Fatalf("应写一行补充池设置已更新，新日志 = %q", added)
	}
	for _, message := range added {
		if strings.HasPrefix(message, "配置已热更新") && strings.Contains(message, "补充池") {
			t.Fatalf("配置已热更新的汇总行不应带补充池字段: %q", message)
		}
		if strings.Contains(message, "WAA Worker 停止") || strings.Contains(message, "WAA Worker 就绪") {
			t.Fatalf("保存补充池设置本身不应启停任何 Worker: %q", message)
		}
	}
	for accountID, worker := range before {
		if workerOf(accountID) != worker {
			t.Fatalf("%s 的 Worker 不应重启", accountID)
		}
	}
	if manager := env.workers; manager.extraOwnedCount() != 0 || !samePartitionSet(manager.slotMembers(), env.pool.PartitionMembers()) {
		t.Fatal("保存补充池设置本身不应打补充池标记")
	}
	save(func(*config.Config) {})
	if countLogs(env.requests, "补充池设置已更新并立即生效") != 1 {
		t.Fatal("设置没有变化时不应再写补充池设置行")
	}
	save(func(cfg *config.Config) { cfg.ExtraPool = false })
	if countLogs(env.requests, "补充池设置已更新并立即生效 | 开启=否") != 1 {
		t.Fatal("关闭补充池应写一行补充池设置已更新")
	}
	if env.workers.warmTargetFor(aistudio.PoolScopeExtra) != 0 || env.workers.maxActiveFor(aistudio.PoolScopeExtra) != 0 ||
		env.pool.ExtraPoolLive() != (aistudio.ExtraPoolRules{}) {
		t.Fatal("关闭后补充池容量与规则应回到关闭")
	}
}

// TestExtraPoolWiringRuntime 装配时的接线：EXTRA_POOL=true 时 Worker 管理器的设置与账户池的补充池规则按配置写入，写一行补充池已开启；
// 关闭（默认）时都是关闭，不写这一行，号池规则版本与没有补充池时相同
func TestExtraPoolWiringRuntime(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		cfg := wiringConfig(t, true)
		cfg.ExtraPool, cfg.ExtraWarmWorkerLimit, cfg.ExtraMaxActiveWorkers = enabled, 2, 3
		manager, admin, _ := newWiringManager(t, cfg, true)
		wantRules := aistudio.ExtraPoolRules{}
		wantLogs, wantWarm := 0, 0
		if enabled {
			wantRules, wantLogs, wantWarm = aistudio.ExtraPoolRules{Enabled: true, DefaultPaid: true}, 1, 2
		}
		if got := admin.pool.ExtraPoolLive(); got != wantRules {
			t.Fatalf("EXTRA_POOL=%t：账户池的补充池规则 = %+v", enabled, got)
		}
		if got := admin.workers.warmTargetFor(aistudio.PoolScopeExtra); got != wantWarm {
			t.Fatalf("EXTRA_POOL=%t：补充池常驻数 = %d", enabled, got)
		}
		line := "补充池已开启 | 开启=是 | 方式=分担 | 默认地址借付费模型=是 | 含 Ultra=否 | 常驻 Worker=2 | 峰值 Worker=3"
		if got := countLogs(manager.requests, line); got != wantLogs {
			t.Fatalf("EXTRA_POOL=%t：补充池已开启的日志 %d 行，期望 %d", enabled, got, wantLogs)
		}
		if !enabled && countLogs(manager.requests, "补充池") != 0 {
			t.Fatal("关闭时不应写任何补充池日志")
		}
	}
}

// TestSlotViewCountsTaggedWorker 改用叠加视图的计数点：带补充池标记的 Worker 不再计入家分区、计入补充池（驻留列表、分区占用、
// 状态接口的分区计数）；去掉标记后回到家分区。测试直接打标记（真实的置标记路径见 extra_lifecycle_test.go）
func TestSlotViewCountsTaggedWorker(t *testing.T) {
	manager, pool, requests := proPartitionManager(t, []string{partitionNormalA}, nil, []string{proPartitionA, proPartitionB}, 1, 2, 1, 2)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for _, accountID := range []string{proPartitionA, proPartitionB} {
		if _, err := manager.promote(ctx, accountID, testRuntimeModel); err != nil {
			t.Fatal(err)
		}
	}
	admin := &runtimeAdmin{pool: pool, service: retireTestService(ctx, manager, pool, requests), requests: requests, workers: manager}
	proWarm := func() int {
		t.Helper()
		status, err := admin.Status(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return status.ProWorkers.Warm
	}
	if got := manager.warmAccountIDsIn(aistudio.PoolScopePro); len(got) != 2 || proWarm() != 2 {
		t.Fatalf("没有标记时 Pro 分区驻留 = %v", got)
	}
	tagExtra(manager, proPartitionA, true)
	if got := manager.warmAccountIDsIn(aistudio.PoolScopePro); len(got) != 1 || got[0] != proPartitionB {
		t.Fatalf("带标记的 Worker 不应计入家分区：Pro 分区驻留 = %v", got)
	}
	if got := manager.warmAccountIDsIn(aistudio.PoolScopeExtra); len(got) != 1 || got[0] != proPartitionA {
		t.Fatalf("带标记的 Worker 应计入补充池：%v", got)
	}
	if occupied := manager.occupiedWorkersIn(aistudio.PoolScopePro, manager.slotMembers()); occupied.slots != 1 {
		t.Fatalf("Pro 分区占用 %d 个槽位，期望 1", occupied.slots)
	}
	if got := proWarm(); got != 1 {
		t.Fatalf("状态接口的 Pro Worker = %d，期望不含补充池 Worker", got)
	}
	if pool.PartitionMembers().Of(proPartitionA) != aistudio.PoolScopePro {
		t.Fatal("标记不应改变账户的家分区")
	}
	tagExtra(manager, proPartitionA, false)
	if got := manager.warmAccountIDsIn(aistudio.PoolScopePro); len(got) != 2 || proWarm() != 2 {
		t.Fatalf("去掉标记后应回到家分区：%v", got)
	}
}
