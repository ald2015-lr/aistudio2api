package app

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/config"
)

// ULTRA_INCLUDE_PRO 在 app 层的测试：配置读写与生效比较、热更新（Worker 分区、派生缓存、日志）、并发保存、
// 需要重建的字段同时变化、排空分区改标、生成请求的号池、额度共享分组

// includeProRuntime 为热更新测试的运行时：普通账号 partitionNormalA（Free）与 partitionNormalB（Pro），Ultra 账号 partitionUltraA，
// 后两个各有一个驻留 Worker；配置的 Ultra 峰值为 1，开关开启后 Ultra 分区超过峰值
type includeProRuntime struct {
	ctx      context.Context
	path     string
	cfg      config.Config
	pool     *aistudio.AccountPool
	workers  *accountWorkerManager
	requests *requestRegistry
	service  *trackedService
	admin    *runtimeAdmin
	manager  *runtimeManager
}

const includeProAccount = partitionNormalB

func newIncludeProRuntime(t *testing.T) *includeProRuntime {
	t.Helper()
	clearAppConfigEnv(t)
	clearPrimaryConfigEnv(t)
	for _, key := range []string{"ULTRA_PRIMARY_MODELS", "ULTRA_PRIMARY_QUEUE_WAIT"} {
		t.Setenv(key, "")
		os.Unsetenv(key)
	}
	cfg := config.Default()
	cfg.WarmWorkerLimit, cfg.MaxActiveWorkers, cfg.WarmStartupConcurrency = 1, 2, 1
	cfg.UltraWarmWorkerLimit, cfg.UltraMaxActiveWorkers = 1, 1
	cfg.UltraPrimaryModels = []string{testRuntimeModel}
	path := filepath.Join(t.TempDir(), ".env")
	if err := cfg.Save(path); err != nil {
		t.Fatal(err)
	}
	workers, pool, requests := partitionTestManager(t,
		[]string{partitionNormalA, includeProAccount}, []string{partitionUltraA},
		cfg.WarmWorkerLimit, cfg.MaxActiveWorkers, cfg.UltraWarmWorkerLimit, cfg.UltraMaxActiveWorkers)
	if err := pool.SetCatalog(includeProAccount, aistudio.BenefitTierPro, []aistudio.Model{{
		ID: testRuntimeModel, Methods: []string{"generateContent", "countTokens"}, Capabilities: map[string]bool{"chat_model": true},
	}}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	service := &trackedService{
		lifecycle: ctx, catalog: emptyCatalog{}, pool: pool, requests: requests, workers: workers,
		forbidden: newForbiddenTracker(), quota: newQuotaSharing("", requests),
	}
	service.setPrimaryModelSettings(cfg.PrimaryModels, cfg.PrimaryQueueWait, cfg.UltraPrimaryModels, cfg.UltraPrimaryQueueWait)
	admin := &runtimeAdmin{lifecycle: ctx, pool: pool, service: service, requests: requests, workers: workers, configPath: path, config: cfg}
	manager := &runtimeManager{
		lifecycle: ctx, configPath: path, activeManagement: cfg, requests: requests, apiKey: newAPIKeyHolder(cfg.ProxyAPIKey),
		intent: &serviceIntent{}, current: &runtimeGeneration{service: service, admin: admin, config: cfg},
	}
	manager.ultraExclusive.Store(cfg.UltraExclusive)
	return &includeProRuntime{
		ctx: ctx, path: path, cfg: cfg, pool: pool, workers: workers, requests: requests, service: service, admin: admin, manager: manager,
	}
}

// save 经管理接口保存开关并按页面的方式立即热更新
func (env *includeProRuntime) save(t *testing.T, on bool) {
	t.Helper()
	value := runtimeConfigDTO(env.cfg)
	value.UltraIncludePro = &on
	updated, err := env.manager.UpdateRuntimeConfig(env.ctx, value)
	if err != nil {
		t.Fatal(err)
	}
	if updated.ServiceRestartRequired {
		t.Fatalf("保存 ULTRA_INCLUDE_PRO=%t 后不应要求重启生成服务", on)
	}
	if updated.UltraIncludePro == nil || *updated.UltraIncludePro != on {
		t.Fatalf("保存结果的 ultra_include_pro = %v，期望 %t", updated.UltraIncludePro, on)
	}
}

// countLogs 返回包含全部片段的日志条数
func countLogs(requests *requestRegistry, fragments ...string) int {
	count := 0
	for _, message := range serviceLogMessages(requests) {
		matched := true
		for _, fragment := range fragments {
			matched = matched && strings.Contains(message, fragment)
		}
		if matched {
			count++
		}
	}
	return count
}

// TestUltraIncludeProConfigFlow 管理页面保存开关：旧版页面不带字段时沿用现值，带 true 时写入；读取与保存结果总是返回该字段；
// 生效比较包含它；它只需热更新，不要求重建
func TestUltraIncludeProConfigFlow(t *testing.T) {
	clearAppConfigEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte("ULTRA_INCLUDE_PRO=true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	admin := &runtimeAdmin{configPath: path, requests: newRequestRegistry(ctx)}
	load := func() bool {
		t.Helper()
		saved, err := config.Load(path)
		if err != nil {
			t.Fatal(err)
		}
		return saved.UltraIncludePro
	}
	legacy := validRuntimeConfig()
	legacy.UltraIncludePro = nil
	if _, err := admin.UpdateRuntimeConfig(ctx, legacy); err != nil {
		t.Fatal(err)
	}
	if !load() {
		t.Fatal("旧版页面不带 ultra_include_pro 时应沿用现值")
	}
	off := false
	value := validRuntimeConfig()
	value.UltraIncludePro = &off
	updated, err := admin.UpdateRuntimeConfig(ctx, value)
	if err != nil || load() || updated.UltraIncludePro == nil || *updated.UltraIncludePro {
		t.Fatalf("保存 false 后 .env=%t 返回=%v err=%v", load(), updated.UltraIncludePro, err)
	}
	on := true
	value.UltraIncludePro = &on
	if updated, err = admin.UpdateRuntimeConfig(ctx, value); err != nil || !load() || updated.UltraIncludePro == nil || !*updated.UltraIncludePro {
		t.Fatalf("保存 true 后 .env=%t 返回=%v err=%v", load(), updated.UltraIncludePro, err)
	}
	read, err := admin.RuntimeConfig(ctx)
	if err != nil || read.UltraIncludePro == nil || !*read.UltraIncludePro {
		t.Fatalf("读取配置应总是返回 ultra_include_pro: %v %v", read.UltraIncludePro, err)
	}
	if dto := runtimeConfigDTO(config.Default()); dto.UltraIncludePro == nil || *dto.UltraIncludePro {
		t.Fatal("默认配置的 DTO 也应返回 ultra_include_pro=false")
	}

	active := config.Default()
	changed := active
	changed.UltraIncludePro = true
	if liveFieldsEqual(changed, active) {
		t.Fatal("liveFieldsEqual 应包含 ULTRA_INCLUDE_PRO，否则只改它的保存永远不生效")
	}
	if requiresRebuild(changed, active) {
		t.Fatal("ULTRA_INCLUDE_PRO 应按热更新处理，不重建生成服务")
	}
	dto := runtimeConfigDTO(changed)
	if sameDataConfig(dto, active, dataConfigOverrides{}) || !sameDataConfig(dto, changed, dataConfigOverrides{}) {
		t.Fatal("生效配置比较应包含 ULTRA_INCLUDE_PRO")
	}
	dto.UltraIncludePro = nil
	if !sameDataConfig(dto, active, dataConfigOverrides{}) || !sameDataConfig(dto, changed, dataConfigOverrides{}) {
		t.Fatal("旧版页面不带该字段时按生效值比较")
	}
}

// TestUltraIncludeProHotReload 保存后立即生效，不重启生成服务：账户池开关与成员、Worker 分区计数、管理状态、预热概况缓存、
// 主用模型供给都按新成员；日志写明生效、分区超峰值与独占提示；再保存 false 全部恢复
func TestUltraIncludeProHotReload(t *testing.T) {
	env := newIncludeProRuntime(t)
	for _, accountID := range []string{includeProAccount, partitionUltraA} {
		if _, err := env.workers.promote(env.ctx, accountID, testRuntimeModel); err != nil {
			t.Fatal(err)
		}
	}
	// 预热概况缓存：账户超过 200 个时缓存一分钟。直接放入一份旧概况，开关切换后下一次调用必须重算
	env.workers.bootstrapMu.Lock()
	env.workers.bootstrapCache = aistudio.BootstrapSummary{Available: 500, UltraAvailable: 7}
	env.workers.bootstrapAt = time.Now()
	env.workers.bootstrapVersion = env.pool.PoolRuleVersion()
	env.workers.bootstrapMu.Unlock()
	if got := env.workers.bootstrapSummary(); got.UltraAvailable != 7 {
		t.Fatalf("版本未变时应使用缓存的预热概况，得到 %+v", got)
	}
	start := time.Now()
	ultraWarm := func() int {
		t.Helper()
		for _, entry := range env.workers.primarySupplyStatus() {
			if entry.Pool == "ultra" && entry.Model == testRuntimeModel {
				return entry.Warm
			}
		}
		t.Fatalf("缺少 Ultra 主用模型供给: %+v", env.workers.primarySupplyStatus())
		return 0
	}
	if !env.workers.refreshPrimarySupply(start) || ultraWarm() != 1 {
		t.Fatalf("切换前 Ultra 主用模型供给的驻留数 = %d，期望 1", ultraWarm())
	}
	if env.workers.refreshPrimarySupply(start.Add(time.Second)) {
		t.Fatal("版本与列表都没变时主用模型供给应使用缓存")
	}

	env.save(t, true)
	if !env.pool.UltraIncludePro() || !env.manager.current.config.UltraIncludePro {
		t.Fatal("保存后账户池与生效配置都应开启 ULTRA_INCLUDE_PRO")
	}
	if _, member := env.pool.UltraAccountIDs()[includeProAccount]; !member {
		t.Fatal("保存后 Ultra 号池成员应包含 Pro 账户")
	}
	if got := sortedUnique(env.workers.warmAccountIDsIn(aistudio.PoolScopeUltra)); !slices.Equal(got, []string{includeProAccount, partitionUltraA}) {
		t.Fatalf("Ultra 分区的驻留 Worker = %v，期望含 Pro", got)
	}
	status, err := env.admin.Status(env.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if status.UltraWorkers.Warm != 2 || status.UltraAccounts.Total != 2 || status.Workers.Warm != 0 {
		t.Fatalf("管理状态 Ultra Worker=%d Ultra 账户=%d 普通 Worker=%d，期望 2/2/0", status.UltraWorkers.Warm, status.UltraAccounts.Total, status.Workers.Warm)
	}
	if got := env.workers.bootstrapSummary(); got.UltraAvailable != 2 || got.Available != 3 {
		t.Fatalf("切换后预热概况应按新成员重算，得到 %+v", got)
	}
	if !env.workers.refreshPrimarySupply(start.Add(2*time.Second)) || ultraWarm() != 2 {
		t.Fatalf("切换后下一次 keepWarm 应重算主用模型供给，Ultra 驻留数 = %d，期望 2", ultraWarm())
	}
	for _, fragments := range [][]string{
		{"Pro 账户并入 Ultra 号池设置已更新并立即生效 | 并入=true | Ultra 号池账户=2 | Ultra 分区 Worker=2/1 | 普通分区 Worker=0/2"},
		{"Ultra 分区 Worker 超过峰值 | 当前=2 | 峰值=1 | 空闲 Worker 将逐步回收，可调大对应峰值"},
		{"ULTRA_EXCLUSIVE=true 时 Pro 账户不再服务普通路径；Pro 账户上的文件与视频改从 /ultra 引用"},
		{"配置已热更新", "Pro 并入 Ultra=true"},
	} {
		if !hasLogContaining(env.requests, fragments...) {
			t.Fatalf("缺少日志 %q，日志 = %v", fragments, serviceLogMessages(env.requests))
		}
	}

	env.save(t, false)
	if env.pool.UltraIncludePro() || env.manager.current.config.UltraIncludePro {
		t.Fatal("再保存 false 后应关闭")
	}
	if _, member := env.pool.UltraAccountIDs()[includeProAccount]; member {
		t.Fatal("关闭后 Ultra 号池成员不应包含 Pro 账户")
	}
	if got := env.workers.warmAccountIDsIn(aistudio.PoolScopeNormal); !slices.Equal(got, []string{includeProAccount}) {
		t.Fatalf("关闭后普通分区的驻留 Worker = %v，期望 Pro 回到普通分区", got)
	}
	if got := env.workers.bootstrapSummary(); got.UltraAvailable != 1 {
		t.Fatalf("关闭后预热概况 = %+v", got)
	}
	if !env.workers.refreshPrimarySupply(start.Add(3*time.Second)) || ultraWarm() != 1 {
		t.Fatalf("关闭后 Ultra 主用模型供给的驻留数 = %d，期望 1", ultraWarm())
	}
	if !hasLogContaining(env.requests, "Pro 账户并入 Ultra 号池设置已更新并立即生效 | 并入=false | Ultra 号池账户=1 | Ultra 分区 Worker=1/1 | 普通分区 Worker=1/2") {
		t.Fatalf("缺少关闭的生效日志，日志 = %v", serviceLogMessages(env.requests))
	}
	if got := countLogs(env.requests, "Pro 账户不再服务普通路径"); got != 1 {
		t.Fatalf("独占提示应只在开启时记录一次，实际 %d 次", got)
	}
	if got := countLogs(env.requests, "Worker 超过峰值"); got != 1 {
		t.Fatalf("关闭后两个分区都没有超过峰值，超峰值日志应只有开启时的一次，实际 %d 次", got)
	}
	// 再次保存同样的值不再切换
	env.save(t, false)
	if got := countLogs(env.requests, "Pro 账户并入 Ultra 号池设置已更新并立即生效"); got != 2 {
		t.Fatalf("开关没有变化时不应再记录生效日志，实际 %d 条", got)
	}
}

// TestUltraIncludeProConcurrentSaves 两个页面同时交替保存开与关：结束后账户池的开关、生效配置与 .env 三者一致
func TestUltraIncludeProConcurrentSaves(t *testing.T) {
	env := newIncludeProRuntime(t)
	var group sync.WaitGroup
	for _, on := range []bool{true, false} {
		group.Add(1)
		go func() {
			defer group.Done()
			for index := 0; index < 8; index++ {
				value := runtimeConfigDTO(env.cfg)
				next := on == (index%2 == 0)
				value.UltraIncludePro = &next
				if _, err := env.manager.UpdateRuntimeConfig(env.ctx, value); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	group.Wait()
	saved, err := config.Load(env.path)
	if err != nil {
		t.Fatal(err)
	}
	env.manager.mu.RLock()
	active := env.manager.current.config.UltraIncludePro
	env.manager.mu.RUnlock()
	if pool := env.pool.UltraIncludePro(); pool != saved.UltraIncludePro || active != saved.UltraIncludePro {
		t.Fatalf("并发保存后 账户池=%t 生效配置=%t .env=%t，三者应一致", pool, active, saved.UltraIncludePro)
	}
	_, member := env.pool.UltraAccountIDs()[includeProAccount]
	if member != saved.UltraIncludePro {
		t.Fatalf("并发保存后 Ultra 号池含 Pro=%t，与 .env=%t 不一致", member, saved.UltraIncludePro)
	}
}

// TestUltraIncludeProWithRebuildField 同一次保存还改了需要重建生成服务的字段：开关不热生效，页面显示需要重启；
// 重启时新一代运行时读到的配置带这个开关，由 configurePool 写入新账户池
func TestUltraIncludeProWithRebuildField(t *testing.T) {
	env := newIncludeProRuntime(t)
	value := runtimeConfigDTO(env.cfg)
	on := true
	value.UltraIncludePro = &on
	value.AuthStates = env.cfg.AuthStates + "-other"
	updated, err := env.manager.UpdateRuntimeConfig(env.ctx, value)
	if err != nil {
		t.Fatal(err)
	}
	if !updated.ServiceRestartRequired {
		t.Fatal("同时修改账户目录时应要求重启生成服务")
	}
	if env.pool.UltraIncludePro() || env.manager.current.config.UltraIncludePro {
		t.Fatal("需要重建时开关不应热生效")
	}

	var launched config.Config
	env.manager.current = restartTestGeneration("STOPPED")
	env.manager.current.config = env.cfg
	env.manager.factory = func(_ context.Context, _ context.Context, next config.Config, _ *requestRegistry) (*runtimeGeneration, error) {
		launched = next
		generation := restartTestGeneration("STOPPED")
		generation.config = next
		return generation, nil
	}
	if _, err := env.manager.StartService(env.ctx); err != nil {
		t.Fatal(err)
	}
	if !launched.UltraIncludePro || launched.AuthStates != value.AuthStates {
		t.Fatalf("新一代运行时的配置 ULTRA_INCLUDE_PRO=%t 账户目录=%q，期望带上保存的值", launched.UltraIncludePro, launched.AuthStates)
	}

	// configurePool：新账户池在创建 Worker 管理器之前就按配置划分号池
	pool, _ := testAccountPool(t, partitionNormalA, includeProAccount)
	if err := pool.SetCatalog(includeProAccount, aistudio.BenefitTierPro, nil); err != nil {
		t.Fatal(err)
	}
	launched.UpstreamChannels = []string{"playground", "build"}
	configurePool(pool, launched)
	if !pool.UltraIncludePro() || pool.PoolOf(includeProAccount) != aistudio.PoolScopeUltra || !pool.BuildEnabled() {
		t.Fatal("configurePool 应写入 ULTRA_INCLUDE_PRO 与上游通道")
	}
	launched.UltraIncludePro = false
	configurePool(pool, launched)
	if pool.UltraIncludePro() || pool.PoolOf(includeProAccount) != aistudio.PoolScopeNormal {
		t.Fatal("configurePool 关闭时 Pro 应在普通号池")
	}
}

// TestPartitionFollowsIncludePro 开关切换后 Pro 的 Worker 改到 Ultra 分区计数，不重启：容量判断计入它，Ultra 常驻数为 0 时空闲回收它，
// Ultra 分区已满时冷启动可以淘汰空闲的 Pro、Pro 忙碌时被挡住；普通分区因此不足时由预热补齐
func TestPartitionFollowsIncludePro(t *testing.T) {
	newManager := func(t *testing.T, ultraWarm int, ultraMax int) (*accountWorkerManager, *aistudio.AccountPool, context.Context) {
		t.Helper()
		manager, pool, _ := partitionTestManager(t,
			[]string{partitionNormalA, includeProAccount}, []string{partitionUltraA, partitionUltraB}, 1, 2, ultraWarm, ultraMax)
		if err := pool.SetCatalog(includeProAccount, aistudio.BenefitTierPro, []aistudio.Model{{
			ID: testRuntimeModel, Methods: []string{"generateContent", "countTokens"}, Capabilities: map[string]bool{"chat_model": true},
		}}); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		t.Cleanup(cancel)
		if _, err := manager.promote(ctx, includeProAccount, testRuntimeModel); err != nil {
			t.Fatal(err)
		}
		return manager, pool, ctx
	}
	toggle := func(manager *accountWorkerManager, pool *aistudio.AccountPool, on bool) {
		if pool.SetUltraIncludePro(on) {
			manager.poolRuleChanged(true)
		}
	}

	t.Run("分区计数与容量", func(t *testing.T) {
		manager, pool, ctx := newManager(t, 1, 2)
		if _, err := manager.promote(ctx, partitionUltraA, testRuntimeModel); err != nil {
			t.Fatal(err)
		}
		toggle(manager, pool, true)
		if got := sortedUnique(manager.warmAccountIDsIn(aistudio.PoolScopeUltra)); !slices.Equal(got, []string{includeProAccount, partitionUltraA}) {
			t.Fatalf("开启后 Ultra 分区的驻留 Worker = %v", got)
		}
		if got := manager.warmAccountIDsIn(aistudio.PoolScopeNormal); len(got) != 0 {
			t.Fatalf("开启后普通分区的驻留 Worker = %v，期望为空", got)
		}
		capacity := manager.standbyCapacity(manager.WarmAccountIDs(), testRuntimeModel)
		if capacity.partition(includeProAccount) != aistudio.PoolScopeUltra || capacity.active[aistudio.PoolScopeUltra] != 2 || capacity.room(aistudio.PoolScopeUltra) {
			t.Fatalf("容量判断应把 Pro 计入 Ultra 分区：占用 %d", capacity.active[aistudio.PoolScopeUltra])
		}
		if occupied := manager.occupiedWorkersIn(aistudio.PoolScopeUltra, pool.UltraAccountIDs()); occupied.slots != 2 {
			t.Fatalf("Ultra 分区占用 %d 个槽位，期望 2", occupied.slots)
		}
		toggle(manager, pool, false)
		if got := manager.warmAccountIDsIn(aistudio.PoolScopeNormal); !slices.Equal(got, []string{includeProAccount}) {
			t.Fatalf("关闭后普通分区的驻留 Worker = %v", got)
		}
	})

	t.Run("Ultra 常驻数为 0 时回收空闲的 Pro Worker", func(t *testing.T) {
		manager, pool, _ := newManager(t, 0, 2)
		before := time.Now().Add(time.Minute)
		if manager.reapIdleWorkerIn(aistudio.PoolScopeUltra, before) || manager.reapIdleWorkerIn(aistudio.PoolScopeNormal, before) {
			t.Fatal("关闭时 Pro Worker 在普通分区，未超过常驻数，不应回收")
		}
		toggle(manager, pool, true)
		if !manager.reapIdleWorkerIn(aistudio.PoolScopeUltra, before) {
			t.Fatal("开启后 Ultra 分区超过常驻数 0，应回收空闲的 Pro Worker")
		}
		if slices.Contains(manager.WarmAccountIDs(), includeProAccount) {
			t.Fatal("Pro Worker 应已被回收")
		}
	})

	t.Run("Ultra 分区已满时淘汰空闲的 Pro", func(t *testing.T) {
		manager, pool, ctx := newManager(t, 1, 2)
		if _, err := manager.promote(ctx, partitionUltraA, testRuntimeModel); err != nil {
			t.Fatal(err)
		}
		toggle(manager, pool, true)
		releasePro := holdLease(t, pool, includeProAccount)
		releaseUltra := holdLease(t, pool, partitionUltraA)
		defer releaseUltra()
		if _, err := manager.promote(ctx, partitionUltraB, testRuntimeModel); !errors.Is(err, errAccountWorkerCapacity) {
			t.Fatalf("Ultra 分区已满且 Pro 忙碌：err = %v，期望 Worker 槽位已满", err)
		}
		releasePro()
		if _, err := manager.promote(ctx, partitionUltraB, testRuntimeModel); err != nil {
			t.Fatalf("Pro 空闲后 Ultra 冷启动应淘汰它: %v", err)
		}
		if got := sortedWarm(manager); !slices.Equal(got, []string{partitionUltraA, partitionUltraB}) {
			t.Fatalf("驻留 Worker = %v，期望空闲的 Pro 被淘汰", got)
		}
	})

	t.Run("普通分区不足时预热补齐", func(t *testing.T) {
		manager, pool, ctx := newManager(t, 1, 2)
		if partition, short := manager.shortPartition(); short && partition == aistudio.PoolScopeNormal {
			t.Fatal("关闭时 Pro Worker 补足了普通分区，不应缺少")
		}
		toggle(manager, pool, true)
		if partition, short := manager.shortPartition(); !short || partition != aistudio.PoolScopeNormal {
			t.Fatalf("开启后普通分区应缺少 Worker：%v %t", partition, short)
		}
		if err := <-manager.StartPrewarm(ctx); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(10 * time.Second)
		for !slices.Contains(manager.warmAccountIDsIn(aistudio.PoolScopeNormal), partitionNormalA) {
			if time.Now().After(deadline) {
				t.Fatalf("预热没有补齐普通分区，驻留 Worker = %v", sortedWarm(manager))
			}
			time.Sleep(20 * time.Millisecond)
		}
	})
}

// TestDrainRetaggedOnIncludePro 排空状态按当前成员记分区：切换后排空中的 Pro 改记到新分区，让出额度只计入新分区；
// retirePass 按旧成员快照算出分区、切换后才开始排空时不写入（可接替账户等判断都是对旧分区做的，留给下一轮），
// 按新分区开始的排空记在新分区
func TestDrainRetaggedOnIncludePro(t *testing.T) {
	manager, pool, _ := partitionTestManager(t, []string{partitionNormalA, includeProAccount}, []string{partitionUltraA}, 1, 4, 1, 4)
	if err := pool.SetCatalog(includeProAccount, aistudio.BenefitTierPro, []aistudio.Model{{
		ID: testRuntimeModel, Methods: []string{"generateContent"}, Capabilities: map[string]bool{"chat_model": true},
	}}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := manager.promote(ctx, includeProAccount, testRuntimeModel); err != nil {
		t.Fatal(err)
	}
	release := holdLease(t, pool, includeProAccount)
	defer release()
	expect := func(step string, normal int, ultra int) {
		t.Helper()
		gotNormal, gotUltra := manager.drainCounts()
		if gotNormal != normal || gotUltra != ultra {
			t.Fatalf("%s：排空计数 普通 %d · Ultra %d，期望 %d · %d", step, gotNormal, gotUltra, normal, ultra)
		}
		if manager.drainCredit(aistudio.PoolScopeNormal, nil) != normal || manager.drainCredit(aistudio.PoolScopeUltra, nil) != ultra {
			t.Fatalf("%s：让出额度 普通 %d · Ultra %d，期望 %d · %d", step,
				manager.drainCredit(aistudio.PoolScopeNormal, nil), manager.drainCredit(aistudio.PoolScopeUltra, nil), normal, ultra)
		}
	}
	if manager.startDrain(includeProAccount, aistudio.PoolScopeNormal) == nil {
		t.Fatal("应能开始排空")
	}
	expect("关闭时开始排空", 1, 0)
	pool.SetUltraIncludePro(true)
	manager.poolRuleChanged(true)
	expect("开启后改标", 0, 1)
	manager.clearDrain(includeProAccount)
	expect("清除排空", 0, 0)
	// retirePass 一轮开始时按旧成员算出分区、切换后才开始排空：不写入，账号也没有被标记排空
	if manager.startDrain(includeProAccount, aistudio.PoolScopeNormal) != nil {
		t.Fatal("账号已换到 Ultra 分区，按旧分区参数不应开始排空")
	}
	expect("开启后用旧分区参数开始排空", 0, 0)
	if pool.Draining(includeProAccount) {
		t.Fatal("没有开始排空时账号不应被标记排空")
	}
	if manager.startDrain(includeProAccount, aistudio.PoolScopeUltra) == nil {
		t.Fatal("按新分区应能再次开始排空")
	}
	expect("开启后用新分区参数开始排空", 0, 1)
	pool.SetUltraIncludePro(false)
	manager.poolRuleChanged(true)
	expect("关闭后改标", 1, 0)
	manager.clearDrain(includeProAccount)
}

// TestReplacementFollowsIncludePro 替补与排空、淘汰空出的槽位在同一个分区：排空开始后 ULTRA_INCLUDE_PRO 切换（排空已改记到
// Ultra 分区），按排空开始时的普通分区启动的替补改在 Ultra 分区挑号；选中淘汰之后才切换的，关闭 Pro Worker 后按当前成员在
// Ultra 分区补替补，不会在普通分区冷启动 Free 账号、让 Ultra 分区少一个 Worker
func TestReplacementFollowsIncludePro(t *testing.T) {
	newManager := func(t *testing.T) (*accountWorkerManager, *aistudio.AccountPool) {
		t.Helper()
		// 两个分区的常驻数都为 0：淘汰之后预热不补，替补全部由 replacementsFor 启动，结果只取决于替补选的分区
		manager, pool, _ := partitionTestManager(t, []string{partitionNormalA, includeProAccount}, []string{partitionUltraA}, 0, 4, 0, 4)
		if err := pool.SetCatalog(includeProAccount, aistudio.BenefitTierPro, []aistudio.Model{{
			ID: testRuntimeModel, Methods: []string{"generateContent"}, Capabilities: map[string]bool{"chat_model": true},
		}}); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		t.Cleanup(cancel)
		if _, err := manager.promote(ctx, includeProAccount, testRuntimeModel); err != nil {
			t.Fatal(err)
		}
		return manager, pool
	}
	expectReplacement := func(t *testing.T, manager *accountWorkerManager) {
		t.Helper()
		if !waitUntil(5*time.Second, func() bool { return slices.Contains(manager.WarmAccountIDs(), partitionUltraA) }) {
			t.Fatalf("替补应在 Ultra 分区启动，驻留 Worker = %v", sortedWarm(manager))
		}
		time.Sleep(100 * time.Millisecond)
		if slices.Contains(manager.WarmAccountIDs(), partitionNormalA) {
			t.Fatalf("不应在普通分区启动替补，驻留 Worker = %v", sortedWarm(manager))
		}
	}

	t.Run("排空开始后切换", func(t *testing.T) {
		manager, pool := newManager(t)
		release := holdLease(t, pool, includeProAccount)
		defer release()
		drain := manager.startDrain(includeProAccount, aistudio.PoolScopeNormal)
		if drain == nil {
			t.Fatal("应能开始排空")
		}
		defer manager.clearDrain(includeProAccount)
		pool.SetUltraIncludePro(true)
		manager.poolRuleChanged(true)
		// 与 retirePass 相同：用排空开始时的分区参数启动替补
		manager.startReplacement(aistudio.PoolScopeNormal, []string{testRuntimeModel}, "排空", drain)
		if !waitUntil(5*time.Second, func() bool { return drain.replacement.Load() != replacementPending }) {
			t.Fatal("替补应结束")
		}
		if drain.replacementFailed() {
			t.Fatalf("Ultra 分区有空闲的 Ultra 账号，替补不应失败，驻留 Worker = %v", sortedWarm(manager))
		}
		expectReplacement(t, manager)
	})

	t.Run("选中淘汰后切换", func(t *testing.T) {
		manager, pool := newManager(t)
		// 登记的模型真的长时间冷却：关闭后的 Pro 账号不会被选回来做自己的替补
		until := time.Now().Add(2 * time.Hour)
		if err := pool.MarkCooldownIfGeneration(includeProAccount, testRuntimeModel, pool.ModelAccessGeneration(includeProAccount), time.Now(), until, "每日限额"); err != nil {
			t.Fatal(err)
		}
		if !pool.AccountLongCooling(includeProAccount, testRuntimeModel, rotationMinRemaining) {
			t.Fatal("Pro 账号应在登记的模型上长时间冷却")
		}
		if !manager.tryReserveVictim(includeProAccount) {
			t.Fatal("应能选中淘汰")
		}
		// 与 retirePass 相同：选中时按旧成员快照记下普通分区，关闭之前开关切换
		victim := retirementCandidate{
			accountID: includeProAccount,
			entry:     retirement{models: []string{testRuntimeModel}, since: time.Now()},
			partition: aistudio.PoolScopeNormal,
		}
		pool.SetUltraIncludePro(true)
		manager.poolRuleChanged(true)
		manager.finishRetirement(victim)
		if slices.Contains(manager.WarmAccountIDs(), includeProAccount) {
			t.Fatal("空闲的 Pro Worker 应已关闭")
		}
		expectReplacement(t, manager)
	})
}

// TestGenerateExcludesProWhenIncluded 生成请求按号池换号：开关开启且独占时普通路径不再尝试 Pro，/ultra 尝试 Pro 与 Ultra；
// 不独占（不限号池）时普通路径照常可以用 Pro；关闭时与原行为一致
func TestGenerateExcludesProWhenIncluded(t *testing.T) {
	all := []string{ultraTestNormalA, ultraTestNormalB, ultraTestUltraA, ultraTestUltraB}
	for _, test := range []struct {
		name       string
		includePro bool
		scope      aistudio.PoolScope
		want       []string
	}{
		{name: "开启 · 独占的普通路径", includePro: true, scope: aistudio.PoolScopeNormal, want: []string{ultraTestNormalA}},
		{name: "开启 · Ultra 路径", includePro: true, scope: aistudio.PoolScopeUltra, want: []string{ultraTestNormalB, ultraTestUltraA, ultraTestUltraB}},
		{name: "开启 · 不独占的普通路径", includePro: true, scope: aistudio.PoolScopeAll, want: all},
		{name: "关闭 · 独占的普通路径", scope: aistudio.PoolScopeNormal, want: []string{ultraTestNormalA, ultraTestNormalB}},
		{name: "关闭 · Ultra 路径", scope: aistudio.PoolScopeUltra, want: []string{ultraTestUltraA, ultraTestUltraB}},
	} {
		t.Run(test.name, func(t *testing.T) {
			upstream := &quotaUpstream{}
			service, pool := ultraTestService(t, upstream, []string{ultraTestUltraA, ultraTestUltraB}, all...)
			if err := pool.SetCatalog(ultraTestNormalB, aistudio.BenefitTierPro, []aistudio.Model{{
				ID: testRuntimeModel, Methods: []string{"generateContent", "countTokens", "bidiGenerateContent"},
				Capabilities: map[string]bool{"chat_model": true},
			}}); err != nil {
				t.Fatal(err)
			}
			pool.SetUltraIncludePro(test.includePro)
			err := runScopedGenerate(t, service, test.scope, true)
			if !quotaExhaustedAfterUpstream429(err) {
				t.Fatalf("err = %v，期望号池内账号都返回每日限额后按普通号池的换号规则返回额度不足", err)
			}
			if got := sortedUnique(upstream.called()); !slices.Equal(got, test.want) {
				t.Fatalf("尝试的账号 = %v，期望 %v", got, test.want)
			}
		})
	}
}

// TestUltraPrimaryFailFastWithPro 只有 Pro 账户（模型只在它的 Build 目录里）且开启开关：/ultra 的 Ultra 主用模型由 Pro 经 Build 服务；
// Pro 在该模型上冷却后立即返回全部冷却（429，带恢复时间），不排队
func TestUltraPrimaryFailFastWithPro(t *testing.T) {
	upstream := &ultraPrimaryUpstream{}
	service, pool := primaryBusyService(t, upstream, primaryTestA)
	pool.SetUpstreamChannels([]aistudio.Channel{aistudio.ChannelPlayground, aistudio.ChannelBuild})
	other := aistudio.Model{ID: "other-model", Methods: []string{"generateContent"}, Capabilities: map[string]bool{"chat_model": true}}
	if err := pool.SetCatalog(primaryTestA, aistudio.BenefitTierPro, []aistudio.Model{other}); err != nil {
		t.Fatal(err)
	}
	if err := pool.SetBuildCatalog(primaryTestA, []aistudio.Model{{
		ID: testRuntimeModel, Methods: []string{"generateContent"}, Capabilities: map[string]bool{"chat_model": true},
	}}); err != nil {
		t.Fatal(err)
	}
	setUltraPrimaryModels(service, []string{testRuntimeModel}, 10*time.Second)
	_, err, _ := runScopedPrimaryRequest(t, service, aistudio.PoolScopeUltra, "req-ultra-pro-off", "")
	var notReady *aistudio.AccountsNotReadyError
	if !errors.As(err, &notReady) || len(upstream.called()) != 0 {
		t.Fatalf("关闭时没有 Ultra 号池账户：err = %v，调用 %v", err, upstream.called())
	}

	pool.SetUltraIncludePro(true)
	text, err, _ := runScopedPrimaryRequest(t, service, aistudio.PoolScopeUltra, "req-ultra-pro", "")
	if err != nil || text != "hello" || !slices.Equal(upstream.called(), []string{primaryTestA}) {
		t.Fatalf("开启后应由 Pro 服务：text=%q err=%v 调用 %v", text, err, upstream.called())
	}

	until := time.Now().Add(30 * time.Second)
	scope := aistudio.ChannelCooldownScope(aistudio.ChannelBuild, testRuntimeModel)
	if err := pool.MarkCooldownIfGeneration(primaryTestA, scope, pool.ModelAccessGeneration(primaryTestA), time.Now(), until, "每日限额"); err != nil {
		t.Fatal(err)
	}
	_, err, elapsed := runScopedPrimaryRequest(t, service, aistudio.PoolScopeUltra, "req-ultra-pro-cooling", "")
	var cooling *aistudio.AllCoolingError
	if !errors.As(err, &cooling) || cooling.HTTPStatus() != http.StatusTooManyRequests || elapsed > time.Second {
		t.Fatalf("Pro 冷却后：err = %v 用时 %s，期望立即返回全部冷却（429）", err, elapsed)
	}
	if diff := cooling.RetryAt().Sub(until); diff < -time.Second || diff > time.Second {
		t.Fatalf("最早恢复 = %s，期望约 %s", cooling.RetryAt(), until)
	}
	if len(upstream.called()) != 1 {
		t.Fatalf("冷却后不应向上游发送，调用 %v", upstream.called())
	}
}

// TestUltraPrimaryInputLimitProToUltra /ultra 上并入的 Pro 碰到权益带来的输入上限：更高权益重试按真实权益比较，
// 改用 Ultra 账号再试一次并完成
func TestUltraPrimaryInputLimitProToUltra(t *testing.T) {
	const pro = "limit-p@example.com"
	upstream := &inputLimitUpstream{
		failing: map[string]bool{pro: true}, status: http.StatusTooManyRequests, code: 8, message: inputCapMessage,
	}
	service, pool := inputLimitService(t, upstream, []aistudio.Channel{aistudio.ChannelPlayground, aistudio.ChannelBuild},
		map[string]inputLimitAccount{
			pro:         {tier: aistudio.BenefitTierPro, playground: true, build: true},
			inputLimitU: {tier: aistudio.BenefitTierUltra, playground: true, build: true},
		})
	pool.SetUltraIncludePro(true)
	setUltraPrimaryModels(service, []string{streamTestModel}, 10*time.Second)
	_, _, err := runStreamChannelRequest(t, service, &upstream.channelRecorder, aistudio.ContextWithPoolScope(service.lifecycle, aistudio.PoolScopeUltra), true, nil)
	if err != nil {
		t.Fatalf("err = %v，期望在 Ultra 账号上完成", err)
	}
	calls := upstream.called()
	if len(calls) != 3 || calls[0] != pro+"/playground" || calls[1] != pro+"/build" || !strings.HasPrefix(calls[2], inputLimitU+"/") {
		t.Fatalf("尝试 = %v，期望 Pro 的两个通道之后改用 Ultra 账号", calls)
	}
	if !progressLogged(service, "改用更高权益的账号再试一次") {
		t.Fatal("请求进度应写明改用更高权益的账号")
	}
}

// TestQuotaSharingGroupsByRealTier 额度共享学习按真实权益分组：开关开启后 Pro 的每日限额证据仍记在模型 ID 键下，
// Ultra 账号的记在 模型|ultra 键下
func TestQuotaSharingGroupsByRealTier(t *testing.T) {
	service, pool := ultraTestService(t, &quotaUpstream{}, []string{ultraTestUltraA}, ultraTestNormalB, ultraTestUltraA)
	if err := pool.SetCatalog(ultraTestNormalB, aistudio.BenefitTierPro, []aistudio.Model{{
		ID: testRuntimeModel, Methods: []string{"generateContent"}, Capabilities: map[string]bool{"chat_model": true},
	}}); err != nil {
		t.Fatal(err)
	}
	pool.SetUltraIncludePro(true)
	daily := &aistudio.RPCError{
		Method: "GenerateContent", StatusCode: http.StatusTooManyRequests, Code: 8,
		Message: "You exceeded your current quota", RetryDelay: 2 * time.Hour,
		Metadata: map[string]string{"quota_limit": "GenerateRequestsPerDayPerProjectPerModel-FreeTier"},
	}
	for _, test := range []struct {
		accountID string
		want      aistudio.PoolScope
	}{
		{accountID: ultraTestNormalB, want: aistudio.PoolScopeNormal},
		{accountID: ultraTestUltraA, want: aistudio.PoolScopeUltra},
	} {
		lease, err := pool.AcquireFor(context.Background(), aistudio.AccountSelection{AccountID: test.accountID})
		if err != nil {
			t.Fatal(err)
		}
		cooldown, ok, stateErr := service.applyQuotaCooldown(lease, test.accountID, testRuntimeModel, daily, "", aistudio.PoolScopeNormal, true, true)
		_ = lease.Release()
		if !ok || stateErr != nil || cooldown.Kind != dailyQuotaKind || cooldown.Global {
			t.Fatalf("%s：额度冷却 = %+v ok=%t err=%v，期望模型级每日限额", test.accountID, cooldown, ok, stateErr)
		}
		service.quota.mu.Lock()
		pending, exists := service.quota.pending[quotaSharingKey(test.accountID, testRuntimeModel, aistudio.ChannelBuild)]
		service.quota.mu.Unlock()
		if !exists || pending.pool != test.want {
			t.Fatalf("%s：等待验证的观察 = %+v（存在=%t），期望按真实权益记为 %v", test.accountID, pending, exists, test.want)
		}
		if got := quotaStatsKey(testRuntimeModel, pending.pool); (test.want == aistudio.PoolScopeUltra) != strings.HasSuffix(got, "|ultra") {
			t.Fatalf("%s：判定键 = %q", test.accountID, got)
		}
	}
}
