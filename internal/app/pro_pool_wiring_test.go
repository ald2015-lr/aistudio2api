package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/api"
	"github.com/Mag1cFall/AIStudio2API/internal/config"
)

// Pro 独立号池的接线测试：经真实的 newRuntimeManager（Go 后端、不启动生成服务、不发上游请求）构造，确认装配、换实例、后台周期与保存
// 这几处都按 PRO_POOL 的规则接上，不只是直接调用 checkProPoolNotice、syncGenerationPoolRules 的单元测试

const (
	wiringFree = "wire-free@example.com"
	wiringPro  = "wire-pro@example.com"
)

// writeWiringAccount 写一个启用的测试账户；pro 为真时 runtime-state.json 记下 Pro 权益（载入时即为 Pro，与目录缓存无关）
func writeWiringAccount(t *testing.T, root string, id string, pro bool) {
	t.Helper()
	directory := writeEnableTestAccount(t, root, id, true)
	if pro {
		if err := os.WriteFile(filepath.Join(directory, "runtime-state.json"), []byte(`{"benefit_tier":1}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// newWiringManager 按 cfg 写 .env（configured 为假时不写 PRO_POOL 这一行）并经 newRuntimeManager 构造，返回管理器与当前生成服务
func newWiringManager(t *testing.T, cfg config.Config, configured bool) (*runtimeManager, *runtimeAdmin, string) {
	t.Helper()
	isolateDisableNotes(t)
	useTempCatalogCacheDir(t)
	clearAppConfigEnv(t)
	for _, key := range []string{
		"PRO_POOL", "PRO_WARM_WORKER_LIMIT", "PRO_MAX_ACTIVE_WORKERS",
		"EXTRA_POOL", "EXTRA_POOL_MODE", "EXTRA_POOL_DEFAULT_PAID", "EXTRA_POOL_INCLUDE_ULTRA", "EXTRA_WARM_WORKER_LIMIT", "EXTRA_MAX_ACTIVE_WORKERS",
	} {
		t.Setenv(key, "")
		os.Unsetenv(key)
	}
	path := filepath.Join(t.TempDir(), ".env")
	var unset []string
	if !configured {
		unset = []string{"PRO_POOL"}
	}
	if err := cfg.SaveKeepingUnset(path, nil, unset); err != nil {
		t.Fatal(err)
	}
	if config.KeyConfigured(path, "PRO_POOL") != configured {
		t.Fatalf("测试前提：PRO_POOL 写过=%t", configured)
	}
	ctx, cancel := context.WithCancel(context.Background())
	lifecycle, stop := context.WithCancel(ctx)
	manager, err := newRuntimeManager(ctx, lifecycle, path, cfg, dataConfigOverrides{})
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	admin := manager.current.admin.(*runtimeAdmin)
	// 生产上管理器与生成服务都读写工作目录下的 .env；测试里两者都指向临时文件，不写到包目录
	admin.configPath = path
	t.Cleanup(func() {
		stop()
		if err := manager.Close(); err != nil {
			t.Error(err)
		}
		admin.pool.WaitBackground()
		cancel()
	})
	return manager, admin, path
}

func wiringConfig(t *testing.T, pro bool) config.Config {
	t.Helper()
	root := filepath.Join(t.TempDir(), "auth")
	writeWiringAccount(t, root, wiringFree, false)
	if pro {
		writeWiringAccount(t, root, wiringPro, true)
	}
	cfg := config.Default()
	cfg.AuthStates = root
	cfg.WAABackend = config.WAABackendGo
	return cfg
}

// TestProPoolWiringRuntime 装配与后台周期的接线：.env 与进程环境变量都没有写 PRO_POOL、保存的 ULTRA_INCLUDE_PRO=true、有一个 Pro 账户时，
// 构造后 /pro 开关随账户池开启、记下 PRO_POOL 没写过；装配时写一行“ULTRA_INCLUDE_PRO 不生效”；IP 池的浏览器上限计入 Pro 分区；
// 后台周期写一行升级提示 WARN（来源分区为 Ultra），状态接口带 pro_pool_notice。只保存其他设置不写 PRO_POOL、提示照旧，
// 保存中 Worker 设置变化时再写一次 WARN；改了 Pro 号池的设置后写出 PRO_POOL、提示消失
func TestProPoolWiringRuntime(t *testing.T) {
	cfg := wiringConfig(t, true)
	cfg.UltraIncludePro = true
	manager, admin, path := newWiringManager(t, cfg, false)
	service := admin.service
	if !manager.activeProPool() || !admin.pool.ProPool() || service.proPoolConfigured.Load() {
		t.Fatalf("构造后：/pro=%t 账户池=%t PRO_POOL 写过=%t", manager.activeProPool(), admin.pool.ProPool(), service.proPoolConfigured.Load())
	}
	if got := admin.pool.PoolOf(wiringPro); got != aistudio.PoolScopePro {
		t.Fatalf("Pro 账户号池 = %v", got)
	}
	if got := countLogs(manager.requests, "ULTRA_INCLUDE_PRO=true 在 PRO_POOL=true 时不生效"); got != 1 {
		t.Fatalf("装配时应写一行 ULTRA_INCLUDE_PRO 不生效的提示，实际 %d 行", got)
	}
	manager.proxyPool.mu.Lock()
	capacity := manager.proxyPool.settings.capacity
	manager.proxyPool.mu.Unlock()
	if capacity != proxyPoolCapacity(cfg, true) || capacity == proxyPoolCapacity(cfg, false) {
		t.Fatalf("有 Pro 账户时浏览器上限 = %d，期望 %d（含 Pro 分区）", capacity, proxyPoolCapacity(cfg, true))
	}

	notice := "PRO_POOL 默认开启：1 个 Pro 账户只服务 /pro 前缀"
	if countLogs(manager.requests, notice) != 0 {
		t.Fatal("后台周期之前不应已有升级提示")
	}
	service.keepWarmPass(manager.lifecycle, time.Now())
	service.keepWarmPass(manager.lifecycle, time.Now())
	if got := countLogs(manager.requests, notice); got != 1 || !hasLogContaining(manager.requests, notice, "Ultra 分区可相应减为") {
		t.Fatalf("后台周期应写一次升级提示（来源分区 Ultra），实际 %d 次：%v", got, serviceLogMessages(manager.requests))
	}
	status, err := admin.Status(manager.lifecycle)
	if err != nil {
		t.Fatal(err)
	}
	if status.ProPoolNotice == nil || status.ProPoolNotice.Source != "ultra" || status.ProPoolNotice.ProAccounts != 1 {
		t.Fatalf("状态接口的升级提示 = %+v", status.ProPoolNotice)
	}

	save := func(mutate func(*api.RuntimeConfig)) {
		t.Helper()
		value, err := manager.RuntimeConfig(manager.lifecycle)
		if err != nil {
			t.Fatal(err)
		}
		mutate(&value)
		if _, err := manager.UpdateRuntimeConfig(manager.lifecycle, value); err != nil {
			t.Fatal(err)
		}
	}
	envHasProPool := func() bool {
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return strings.Contains(string(content), "\nPRO_POOL=")
	}
	// 只改其他设置（请求超时与常驻 Worker 数）：不写 PRO_POOL，提示照旧；Worker 设置变化，保存后的检查再写一次 WARN
	save(func(value *api.RuntimeConfig) {
		warm := value.WarmWorkerLimit + 1
		value.RequestTimeout, value.WarmWorkerLimit = "4m0s", warm
	})
	if envHasProPool() || service.proPoolConfigured.Load() {
		t.Fatal("只保存其他设置时不应写出 PRO_POOL")
	}
	if status, err := admin.Status(manager.lifecycle); err != nil || status.ProPoolNotice == nil {
		t.Fatalf("只保存其他设置后提示应仍在：%+v（%v）", status.ProPoolNotice, err)
	}
	if got := countLogs(manager.requests, notice); got != 2 {
		t.Fatalf("保存改了 Worker 设置后应再写一次升级提示，实际 %d 次", got)
	}
	// 改 Pro 号池的设置：写出 PRO_POOL，算运维做过选择，提示消失
	save(func(value *api.RuntimeConfig) {
		warm := 3
		value.ProWarmWorkerLimit = &warm
	})
	if !envHasProPool() || !service.proPoolConfigured.Load() {
		t.Fatal("改了 Pro 号池的设置后应写出 PRO_POOL")
	}
	if status, err := admin.Status(manager.lifecycle); err != nil || status.ProPoolNotice != nil {
		t.Fatalf("写出 PRO_POOL 后不应再提示：%+v（%v）", status.ProPoolNotice, err)
	}
}

// TestProPoolSaveWritesProPool .env 与进程环境变量都没有写 PRO_POOL 时，经管理页只改 Pro 号池的任一设置（关闭开关、只改常驻数、只改峰值）
// 并保存：写出 PRO_POOL（关闭时为 false，重新载入仍为关闭，账户池与 /pro 立即关闭），记下写过，升级提示消失
func TestProPoolSaveWritesProPool(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*api.RuntimeConfig)
		want   bool
	}{
		{name: "关闭开关", want: false, mutate: func(value *api.RuntimeConfig) {
			off := false
			value.ProPool = &off
		}},
		{name: "只改常驻数", want: true, mutate: func(value *api.RuntimeConfig) {
			warm := *value.ProWarmWorkerLimit + 1
			value.ProWarmWorkerLimit = &warm
		}},
		{name: "只改峰值", want: true, mutate: func(value *api.RuntimeConfig) {
			peak := *value.ProMaxActiveWorkers + 1
			value.ProMaxActiveWorkers = &peak
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := wiringConfig(t, true)
			manager, admin, path := newWiringManager(t, cfg, false)
			if status, err := admin.Status(manager.lifecycle); err != nil || status.ProPoolNotice == nil {
				t.Fatalf("测试前提：没写 PRO_POOL、有 Pro 账户时应有升级提示：%+v（%v）", status.ProPoolNotice, err)
			}
			value, err := manager.RuntimeConfig(manager.lifecycle)
			if err != nil {
				t.Fatal(err)
			}
			tc.mutate(&value)
			if _, err := manager.UpdateRuntimeConfig(manager.lifecycle, value); err != nil {
				t.Fatal(err)
			}
			line := fmt.Sprintf("\nPRO_POOL=%t\n", tc.want)
			if content := mustReadFile(t, path); !strings.Contains(content, line) {
				t.Fatalf(".env 应写出 %q：\n%s", strings.TrimSpace(line), content)
			}
			reloaded, err := config.Load(path)
			if err != nil {
				t.Fatal(err)
			}
			if reloaded.ProPool != tc.want || admin.pool.ProPool() != tc.want || manager.activeProPool() != tc.want {
				t.Fatalf("重新载入=%t 账户池=%t /pro=%t，期望都为 %t", reloaded.ProPool, admin.pool.ProPool(), manager.activeProPool(), tc.want)
			}
			if !admin.service.proPoolConfigured.Load() {
				t.Fatal("写出 PRO_POOL 后应记下写过")
			}
			if status, err := admin.Status(manager.lifecycle); err != nil || status.ProPoolNotice != nil {
				t.Fatalf("写出 PRO_POOL 后不应再提示：%+v（%v）", status.ProPoolNotice, err)
			}
		})
	}
}

// TestProPoolWiringNoProAccounts 没有 Pro 账户时装配的浏览器上限不计 Pro 分区；.env 写了 PRO_POOL 时记下写过、不提示；
// ULTRA_INCLUDE_PRO=false 时装配不写“不生效”的提示
func TestProPoolWiringNoProAccounts(t *testing.T) {
	cfg := wiringConfig(t, false)
	manager, admin, _ := newWiringManager(t, cfg, true)
	manager.proxyPool.mu.Lock()
	capacity := manager.proxyPool.settings.capacity
	manager.proxyPool.mu.Unlock()
	if capacity != proxyPoolCapacity(cfg, false) || capacity == proxyPoolCapacity(cfg, true) {
		t.Fatalf("没有 Pro 账户时浏览器上限 = %d，期望 %d（不含 Pro 分区）", capacity, proxyPoolCapacity(cfg, false))
	}
	if !admin.service.proPoolConfigured.Load() || !manager.activeProPool() {
		t.Fatal(".env 写了 PRO_POOL=true：应记下写过，/pro 开启")
	}
	if hasLogContaining(manager.requests, "ULTRA_INCLUDE_PRO=true 在 PRO_POOL=true 时不生效") {
		t.Fatal("ULTRA_INCLUDE_PRO=false 时不应提示")
	}
}

// TestProxyPoolCapacityFollowsProPartition 装配时没有已知的 Pro 账户（首次启动时权益尚未读取，按 Free 计），之后读到 Pro 权益：
// 后台周期把 IP 池的浏览器上限改为计入 Pro 分区，不用等下一次保存配置或重启；账户的权益再变回 Free 后 Pro 分区为空，上限去掉 Pro 分区
func TestProxyPoolCapacityFollowsProPartition(t *testing.T) {
	cfg := wiringConfig(t, false)
	manager, admin, _ := newWiringManager(t, cfg, true)
	capacity := func() int {
		manager.proxyPool.mu.Lock()
		defer manager.proxyPool.mu.Unlock()
		return manager.proxyPool.settings.capacity
	}
	if got := capacity(); got != proxyPoolCapacity(cfg, false) {
		t.Fatalf("装配时浏览器上限 = %d，期望 %d（不含 Pro 分区）", got, proxyPoolCapacity(cfg, false))
	}
	catalog := []aistudio.Model{{ID: "wire-model", Methods: []string{"generateContent"}}}
	if err := admin.pool.SetCatalog(wiringFree, aistudio.BenefitTierPro, catalog); err != nil {
		t.Fatal(err)
	}
	if got := admin.pool.PoolOf(wiringFree); got != aistudio.PoolScopePro {
		t.Fatalf("读到 Pro 权益后号池 = %v", got)
	}
	admin.service.keepWarmPass(manager.lifecycle, time.Now())
	if got := capacity(); got != proxyPoolCapacity(cfg, true) {
		t.Fatalf("Pro 分区有账户后浏览器上限 = %d，期望 %d（含 Pro 分区）", got, proxyPoolCapacity(cfg, true))
	}
	if err := admin.pool.SetCatalog(wiringFree, aistudio.BenefitTierFree, catalog); err != nil {
		t.Fatal(err)
	}
	admin.service.keepWarmPass(manager.lifecycle, time.Now())
	if got := capacity(); got != proxyPoolCapacity(cfg, false) {
		t.Fatalf("Pro 分区为空后浏览器上限 = %d，期望 %d", got, proxyPoolCapacity(cfg, false))
	}
}

// TestStartServiceInstallsGeneration 启动生成服务换上新实例时经 installGeneration：/pro 开关随新实例（没有账户池的实例为关闭，
// 不沿用上一个实例的开启），并按 .env 重新记下 PRO_POOL 是否写过；StartService 按它的返回值取消并关闭被换下的旧实例，新实例照常运行
func TestStartServiceInstallsGeneration(t *testing.T) {
	env := newIncludeProRuntime(t)
	env.pool.SetProPool(true)
	env.manager.installGeneration(env.manager.current)
	if !env.manager.activeProPool() || !env.service.proPoolConfigured.Load() {
		t.Fatal("installGeneration 应按账户池开启 /pro，并按 .env 记下 PRO_POOL 写过")
	}
	var kept []string
	for _, line := range strings.SplitAfter(mustReadFile(t, env.path), "\n") {
		if !strings.HasPrefix(line, "PRO_POOL=") {
			kept = append(kept, line)
		}
	}
	if err := os.WriteFile(env.path, []byte(strings.Join(kept, "")), 0o600); err != nil {
		t.Fatal(err)
	}
	env.manager.installGeneration(env.manager.current)
	if env.service.proPoolConfigured.Load() {
		t.Fatal(".env 去掉 PRO_POOL 后 installGeneration 应记下没写过")
	}

	// 换下的是旧实例：StartService 取消并关闭 installGeneration 返回的实例，新实例保持运行、成为当前实例
	type counted struct{ canceled, closed int }
	track := func(generation *runtimeGeneration, counts *counted) *runtimeGeneration {
		generation.lifecycleCancel = func() { counts.canceled++ }
		generation.closeRuntime = func() error {
			counts.closed++
			return nil
		}
		return generation
	}
	var oldCounts, nextCounts counted
	previous := track(restartTestGeneration("STOPPED"), &oldCounts)
	next := track(restartTestGeneration("STOPPED"), &nextCounts)
	env.manager.current = previous
	env.manager.factory = func(context.Context, context.Context, config.Config, *requestRegistry) (*runtimeGeneration, error) {
		return next, nil
	}
	env.manager.proPool.Store(true)
	if _, err := env.manager.StartService(env.ctx); err != nil {
		t.Fatal(err)
	}
	if env.manager.activeProPool() {
		t.Fatal("换上没有账户池的新实例后 /pro 开关应为关闭（随新实例，不沿用上一个实例）")
	}
	env.manager.mu.Lock()
	current := env.manager.current
	env.manager.mu.Unlock()
	if current != next || oldCounts.canceled == 0 || oldCounts.closed != 1 || nextCounts != (counted{}) {
		t.Fatalf("应关闭被换下的旧实例、保留新实例：当前是新实例=%t 旧实例取消 %d 次、关闭 %d 次，新实例 %+v",
			current == next, oldCounts.canceled, oldCounts.closed, nextCounts)
	}
}

func mustReadFile(t *testing.T, path string) string {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(content)
}
