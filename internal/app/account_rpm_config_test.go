package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/api"
	"github.com/Mag1cFall/AIStudio2API/internal/config"
)

// clearAccountRPMEnv 清掉进程环境里的 ACCOUNT_RPM_LIMIT（写法与 clearAppConfigEnv 相同），免得覆盖 .env 中的值
func clearAccountRPMEnv(t *testing.T) {
	t.Helper()
	t.Setenv("ACCOUNT_RPM_LIMIT", "")
	os.Unsetenv("ACCOUNT_RPM_LIMIT")
}

// withoutAccountRPMLimit 模拟旧版页面提交：把配置编码成 JSON 后删掉 account_rpm_limit 键再解码
func withoutAccountRPMLimit(t *testing.T, value api.RuntimeConfig) api.RuntimeConfig {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	if _, ok := fields["account_rpm_limit"]; !ok {
		t.Fatal("读取配置应总是带 account_rpm_limit")
	}
	delete(fields, "account_rpm_limit")
	if data, err = json.Marshal(fields); err != nil {
		t.Fatal(err)
	}
	var legacy api.RuntimeConfig
	if err := json.Unmarshal(data, &legacy); err != nil {
		t.Fatal(err)
	}
	if legacy.AccountRPMLimit != nil {
		t.Fatal("删掉键后应为 nil")
	}
	return legacy
}

// TestUpdateRuntimeConfigAccountRPMLimit 管理页面保存单账户每分钟上限：写入 .env；旧版页面不带这个字段时沿用现值；
// 超出 0 到 1000 返回 invalid_config；读取时总是返回，显式 0（关闭）读回仍是 0
func TestUpdateRuntimeConfigAccountRPMLimit(t *testing.T) {
	clearAppConfigEnv(t)
	clearAccountRPMEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	path := filepath.Join(t.TempDir(), ".env")
	if err := config.Default().Save(path); err != nil {
		t.Fatal(err)
	}
	admin := &runtimeAdmin{configPath: path, requests: newRequestRegistry(ctx)}
	load := func() int {
		t.Helper()
		saved, err := config.Load(path)
		if err != nil {
			t.Fatal(err)
		}
		return saved.AccountRPMLimit
	}

	value := validRuntimeConfig()
	limit := 7
	value.AccountRPMLimit = &limit
	saved, err := admin.UpdateRuntimeConfig(ctx, value)
	if err != nil || saved.AccountRPMLimit == nil || *saved.AccountRPMLimit != 7 {
		t.Fatalf("保存返回 %v err=%v，期望 7", saved.AccountRPMLimit, err)
	}
	if got := load(); got != 7 {
		t.Fatalf("保存后 .env 的上限 = %d，期望 7", got)
	}
	data, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(data), "ACCOUNT_RPM_LIMIT=7") {
		t.Fatalf(".env 应写出 ACCOUNT_RPM_LIMIT=7：%q err=%v", data, err)
	}

	// 旧版页面：没有这个字段，沿用现值
	if _, err := admin.UpdateRuntimeConfig(ctx, withoutAccountRPMLimit(t, validRuntimeConfig())); err != nil {
		t.Fatal(err)
	}
	if got := load(); got != 7 {
		t.Fatalf("旧版页面不带这个字段时应沿用现值 7，实际 %d", got)
	}

	var operation *adminOperationError
	for _, invalid := range []int{-1, 1001} {
		value.AccountRPMLimit = &invalid
		if _, err := admin.UpdateRuntimeConfig(ctx, value); !errors.As(err, &operation) || operation.status != http.StatusBadRequest || operation.code != "invalid_config" {
			t.Fatalf("上限 %d 应返回 invalid_config，实际 err=%v", invalid, err)
		}
	}
	if got := load(); got != 7 {
		t.Fatalf("无效的提交不应改写 .env，上限 = %d", got)
	}

	// 显式 0 表示关闭：保存、读取都返回 0，不当成缺省
	zero := 0
	value.AccountRPMLimit = &zero
	if saved, err = admin.UpdateRuntimeConfig(ctx, value); err != nil || saved.AccountRPMLimit == nil || *saved.AccountRPMLimit != 0 {
		t.Fatalf("保存 0 返回 %v err=%v", saved.AccountRPMLimit, err)
	}
	read, err := admin.RuntimeConfig(ctx)
	if err != nil || read.AccountRPMLimit == nil || *read.AccountRPMLimit != 0 {
		t.Fatalf("读取配置应返回显式的 0：%v err=%v", read.AccountRPMLimit, err)
	}
	encoded, err := json.Marshal(read)
	if err != nil || !strings.Contains(string(encoded), `"account_rpm_limit":0`) {
		t.Fatalf("读取结果的 JSON 应带 account_rpm_limit=0：%s err=%v", encoded, err)
	}
	if got := load(); got != 0 {
		t.Fatalf("保存 0 后 .env 的上限 = %d", got)
	}
}

// TestAccountRPMLimitHotReload 只改单账户每分钟上限：保存后立即热更新到账户池（不重启生成服务），生效配置随之更新，汇总日志写明上限
func TestAccountRPMLimitHotReload(t *testing.T) {
	clearAppConfigEnv(t)
	clearPrimaryConfigEnv(t)
	clearAccountRPMEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	path := filepath.Join(t.TempDir(), ".env")
	cfg := config.Default()
	if err := cfg.Save(path); err != nil {
		t.Fatal(err)
	}
	requests := newRequestRegistry(ctx)
	pool, accounts := testAccountPool(t, ultraTestNormalA)
	configurePool(pool, cfg)
	if got := pool.AccountRateLimit(); got != cfg.AccountRPMLimit {
		t.Fatalf("configurePool 后账户池的上限 = %d，期望 %d", got, cfg.AccountRPMLimit)
	}
	workers := newAccountWorkerManager(pool, accounts, requests, "", "", time.Minute, cfg.WarmWorkerLimit, cfg.MaxActiveWorkers, cfg.WarmStartupConcurrency, false)
	t.Cleanup(func() { _ = workers.Close() })
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

	for _, test := range []struct {
		limit   int
		summary string
	}{
		{limit: 3, summary: "单账户每分钟上限=3 次"},
		{limit: 0, summary: "单账户每分钟上限=关闭"},
	} {
		value := runtimeConfigDTO(cfg)
		limit := test.limit
		value.AccountRPMLimit = &limit
		updated, err := manager.UpdateRuntimeConfig(ctx, value)
		if err != nil {
			t.Fatal(err)
		}
		if updated.ServiceRestartRequired {
			t.Fatalf("上限改为 %d 后不应要求重启生成服务", test.limit)
		}
		if got := pool.AccountRateLimit(); got != test.limit {
			t.Fatalf("账户池的上限 = %d，期望立即热更新为 %d", got, test.limit)
		}
		if got := manager.current.config.AccountRPMLimit; got != test.limit {
			t.Fatalf("热更新后生效配置的上限 = %d，期望 %d", got, test.limit)
		}
		if !hasLogContaining(requests, "配置已热更新", test.summary) {
			t.Fatalf("热更新日志应写明 %q，日志 = %v", test.summary, serviceLogMessages(requests))
		}
	}
}

// TestAccountRPMLimitLiveFields 单账户每分钟上限是热更新字段：只有它不同时 liveFieldsEqual 为假、requiresRebuild 为假；
// 生效配置比较包含它，旧版页面不带这个字段时按生效值比较
func TestAccountRPMLimitLiveFields(t *testing.T) {
	active := config.Default()
	changed := active
	changed.AccountRPMLimit = 3
	if liveFieldsEqual(changed, active) || requiresRebuild(changed, active) {
		t.Fatal("单账户每分钟上限变化应按热更新处理")
	}
	dto := runtimeConfigDTO(changed)
	if sameDataConfig(dto, active, dataConfigOverrides{}) || !sameDataConfig(dto, changed, dataConfigOverrides{}) {
		t.Fatal("生效配置比较应包含单账户每分钟上限")
	}
	dto.AccountRPMLimit = nil
	if !sameDataConfig(dto, active, dataConfigOverrides{}) {
		t.Fatal("旧版页面不带这个字段时按生效值比较")
	}
	if got := rpmLimitSummary(0); got != "关闭" {
		t.Fatalf("上限 0 的汇总 = %q", got)
	}
	if got := rpmLimitSummary(10); got != "10 次" {
		t.Fatalf("上限 10 的汇总 = %q", got)
	}
}
