package app

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/config"
)

// emptyCatalog 是测试用的模型目录服务，热更新同步模型快照时返回空目录
type emptyCatalog struct{}

func (emptyCatalog) RefreshAccountModels(context.Context, string) ([]aistudio.Model, error) {
	return nil, nil
}
func (emptyCatalog) CachedModels() []aistudio.Model { return nil }

// TestUpdateRuntimeConfigUltraSettings 管理页面保存 Ultra 设置：写入 .env；旧版页面不带这些字段时沿用现值；
// 常驻数大于峰值数或峰值为 0 返回 400 invalid_config；读取时总是返回三个字段
func TestUpdateRuntimeConfigUltraSettings(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte("ULTRA_EXCLUSIVE=false\nULTRA_WARM_WORKER_LIMIT=0\nULTRA_MAX_ACTIVE_WORKERS=12\n"), 0o600); err != nil {
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

	legacy := validRuntimeConfig()
	legacy.UltraExclusive, legacy.UltraWarmWorkerLimit, legacy.UltraMaxActiveWorkers = nil, nil, nil
	if _, err := admin.UpdateRuntimeConfig(ctx, legacy); err != nil {
		t.Fatal(err)
	}
	if saved := load(); saved.UltraExclusive || saved.UltraWarmWorkerLimit != 0 || saved.UltraMaxActiveWorkers != 12 {
		t.Fatalf("旧版页面不带 Ultra 字段时应沿用现值，得到 %t/%d/%d", saved.UltraExclusive, saved.UltraWarmWorkerLimit, saved.UltraMaxActiveWorkers)
	}

	value := validRuntimeConfig()
	exclusive, warm, maxActive := true, 10, 20
	value.UltraExclusive, value.UltraWarmWorkerLimit, value.UltraMaxActiveWorkers = &exclusive, &warm, &maxActive
	updated, err := admin.UpdateRuntimeConfig(ctx, value)
	if err != nil {
		t.Fatal(err)
	}
	if saved := load(); !saved.UltraExclusive || saved.UltraWarmWorkerLimit != 10 || saved.UltraMaxActiveWorkers != 20 {
		t.Fatalf("保存后 .env 为 %t/%d/%d，期望 true/10/20", saved.UltraExclusive, saved.UltraWarmWorkerLimit, saved.UltraMaxActiveWorkers)
	}
	if updated.UltraExclusive == nil || !*updated.UltraExclusive || updated.UltraWarmWorkerLimit == nil || *updated.UltraWarmWorkerLimit != 10 ||
		updated.UltraMaxActiveWorkers == nil || *updated.UltraMaxActiveWorkers != 20 {
		t.Fatal("保存结果应返回三个 Ultra 字段")
	}
	read, err := admin.RuntimeConfig(ctx)
	if err != nil || read.UltraWarmWorkerLimit == nil || *read.UltraWarmWorkerLimit != 10 {
		t.Fatalf("读取配置应返回 Ultra 字段: %v", err)
	}

	for _, invalid := range [][2]int{{6, 5}, {0, 0}, {-1, 5}} {
		bad := validRuntimeConfig()
		bad.UltraWarmWorkerLimit, bad.UltraMaxActiveWorkers = &invalid[0], &invalid[1]
		_, err := admin.UpdateRuntimeConfig(ctx, bad)
		var operation *adminOperationError
		if !errors.As(err, &operation) || operation.status != http.StatusBadRequest || operation.code != "invalid_config" {
			t.Fatalf("常驻 %d / 峰值 %d 应返回 400 invalid_config，得到 %v", invalid[0], invalid[1], err)
		}
	}

	active := config.Default()
	for _, change := range []func(*config.Config){
		func(cfg *config.Config) { cfg.UltraExclusive = false },
		func(cfg *config.Config) { cfg.UltraWarmWorkerLimit = 0 },
		func(cfg *config.Config) { cfg.UltraMaxActiveWorkers = 9 },
	} {
		changed := active
		change(&changed)
		if liveFieldsEqual(changed, active) || requiresRebuild(changed, active) {
			t.Fatal("Ultra 设置变化应按热更新处理")
		}
		dto := runtimeConfigDTO(changed)
		if sameDataConfig(dto, active, dataConfigOverrides{}) || !sameDataConfig(dto, changed, dataConfigOverrides{}) {
			t.Fatal("生效配置比较应包含 Ultra 设置")
		}
	}
}

// TestUltraSettingsHotReload 保存配置后 Ultra 分区的常驻数、峰值数与独占设置立即生效，不重启生成服务
func TestUltraSettingsHotReload(t *testing.T) {
	clearAppConfigEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	path := filepath.Join(t.TempDir(), ".env")
	cfg := config.Default()
	if err := cfg.Save(path); err != nil {
		t.Fatal(err)
	}
	requests := newRequestRegistry(ctx)
	pool, accounts := testAccountPool(t, ultraTestNormalA)
	workers := newAccountWorkerManager(pool, accounts, requests, "", "", time.Minute, cfg.WarmWorkerLimit, cfg.MaxActiveWorkers, cfg.WarmStartupConcurrency, false)
	workers.setUltraCapacity(cfg.UltraWarmWorkerLimit, cfg.UltraMaxActiveWorkers)
	t.Cleanup(func() { _ = workers.Close() })
	service := &trackedService{
		lifecycle: ctx, catalog: emptyCatalog{}, pool: pool, requests: requests, workers: workers,
		forbidden: newForbiddenTracker(), quota: newQuotaSharing("", requests),
	}
	admin := &runtimeAdmin{lifecycle: ctx, pool: pool, service: service, requests: requests, workers: workers, configPath: path, config: cfg}
	manager := &runtimeManager{
		lifecycle: ctx, configPath: path, activeManagement: cfg, requests: requests, apiKey: newAPIKeyHolder(cfg.ProxyAPIKey),
		intent: &serviceIntent{}, current: &runtimeGeneration{service: service, admin: admin, config: cfg},
	}
	manager.ultraExclusive.Store(cfg.UltraExclusive)
	if workers.warmTargetFor(aistudio.PoolScopeUltra) != 2 || workers.maxActiveFor(aistudio.PoolScopeUltra) != 5 || !manager.activeUltraExclusive() {
		t.Fatal("初始 Ultra 设置应为默认值")
	}

	value := runtimeConfigDTO(cfg)
	exclusive, warm, maxActive := false, 10, 20
	value.UltraExclusive, value.UltraWarmWorkerLimit, value.UltraMaxActiveWorkers = &exclusive, &warm, &maxActive
	updated, err := manager.UpdateRuntimeConfig(ctx, value)
	if err != nil {
		t.Fatal(err)
	}
	if updated.ServiceRestartRequired {
		t.Fatal("Ultra 设置热更新后不应要求重启生成服务")
	}
	if workers.warmTargetFor(aistudio.PoolScopeUltra) != 10 || workers.maxActiveFor(aistudio.PoolScopeUltra) != 20 {
		t.Fatalf("Ultra 分区容量 = %d/%d，期望 10/20", workers.warmTargetFor(aistudio.PoolScopeUltra), workers.maxActiveFor(aistudio.PoolScopeUltra))
	}
	if workers.warmTargetFor(aistudio.PoolScopeNormal) != cfg.WarmWorkerLimit || workers.maxActiveFor(aistudio.PoolScopeNormal) != cfg.MaxActiveWorkers {
		t.Fatal("修改 Ultra 设置不应改变普通分区容量")
	}
	if manager.activeUltraExclusive() {
		t.Fatal("ULTRA_EXCLUSIVE=false 应立即生效")
	}
	if manager.current.config.UltraMaxActiveWorkers != 20 {
		t.Fatal("热更新后生效配置应记录新的 Ultra 设置")
	}
}

// TestUltraExclusiveFollowsEffectiveConfig 公开 API 使用的独占设置总是与生效配置一致：手动改过 .env 后停止再启动，
// 新生成服务读到的 ULTRA_EXCLUSIVE 立即生效；进程环境变量覆盖 .env 时，保存配置后生效的是读取配置时显示的值
func TestUltraExclusiveFollowsEffectiveConfig(t *testing.T) {
	t.Run("启动时读取 .env", func(t *testing.T) {
		clearAppConfigEnv(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		path := filepath.Join(t.TempDir(), ".env")
		cfg := config.Default()
		if err := cfg.Save(path); err != nil {
			t.Fatal(err)
		}
		manager := &runtimeManager{
			lifecycle: ctx, requests: newRequestRegistry(ctx), intent: &serviceIntent{}, configPath: path,
			activeManagement: cfg, apiKey: newAPIKeyHolder(cfg.ProxyAPIKey), current: restartTestGeneration("STOPPED"),
			factory: func(_ context.Context, _ context.Context, next config.Config, _ *requestRegistry) (*runtimeGeneration, error) {
				generation := restartTestGeneration("STOPPED")
				generation.config = next
				return generation, nil
			},
		}
		manager.current.config = cfg
		manager.ultraExclusive.Store(cfg.UltraExclusive)
		edited := cfg
		edited.UltraExclusive = false
		if err := edited.Save(path); err != nil {
			t.Fatal(err)
		}
		if _, err := manager.StartService(ctx); err != nil {
			t.Fatal(err)
		}
		if manager.current.config.UltraExclusive {
			t.Fatal("新生成服务应读到 ULTRA_EXCLUSIVE=false")
		}
		if manager.activeUltraExclusive() {
			t.Fatal("生成服务配置 ULTRA_EXCLUSIVE=false，公开 API 仍按独占划分号池")
		}
	})
	t.Run("环境变量覆盖 .env", func(t *testing.T) {
		clearAppConfigEnv(t)
		t.Setenv("ULTRA_EXCLUSIVE", "true")
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		path := filepath.Join(t.TempDir(), ".env")
		cfg := config.Default()
		if err := cfg.Save(path); err != nil {
			t.Fatal(err)
		}
		requests := newRequestRegistry(ctx)
		manager := &runtimeManager{
			lifecycle: ctx, configPath: path, activeManagement: cfg, requests: requests, apiKey: newAPIKeyHolder(cfg.ProxyAPIKey),
			intent: &serviceIntent{}, current: &runtimeGeneration{admin: &runtimeAdmin{configPath: path, requests: requests}, config: cfg},
		}
		manager.ultraExclusive.Store(cfg.UltraExclusive)
		value := runtimeConfigDTO(cfg)
		exclusive := false
		value.UltraExclusive = &exclusive
		if _, err := manager.UpdateRuntimeConfig(ctx, value); err != nil {
			t.Fatal(err)
		}
		read, err := manager.RuntimeConfig(ctx)
		if err != nil || read.UltraExclusive == nil {
			t.Fatalf("读取配置失败: %v", err)
		}
		if !*read.UltraExclusive || manager.activeUltraExclusive() != *read.UltraExclusive {
			t.Fatalf("读取配置显示 ULTRA_EXCLUSIVE=%t，公开 API 实际使用的独占设置=%t", *read.UltraExclusive, manager.activeUltraExclusive())
		}
	})
}

func clearAppConfigEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{"ULTRA_EXCLUSIVE", "ULTRA_WARM_WORKER_LIMIT", "ULTRA_MAX_ACTIVE_WORKERS", "WARM_WORKER_LIMIT", "MAX_ACTIVE_WORKERS"} {
		t.Setenv(key, "")
		os.Unsetenv(key)
	}
}
