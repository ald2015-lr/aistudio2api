package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/api"
	"github.com/Mag1cFall/AIStudio2API/internal/config"
)

func clearPrimaryConfigEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{"PRIMARY_MODELS", "PRIMARY_QUEUE_WAIT"} {
		t.Setenv(key, "")
		os.Unsetenv(key)
	}
}

// TestUpdateRuntimeConfigPrimaryModels 管理页面保存主用模型与排队上限：整理后写入 .env；旧版页面不带这些字段时沿用现值；
// 空数组表示清空，0s 表示不排队；无效或超过请求超时的排队上限返回 invalid_config；读取与保存结果总是返回这些字段
func TestUpdateRuntimeConfigPrimaryModels(t *testing.T) {
	clearPrimaryConfigEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte("PRIMARY_MODELS=model-a,model-b\nPRIMARY_QUEUE_WAIT=20s\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	admin := &runtimeAdmin{configPath: path, requests: newRequestRegistry(ctx)}
	load := func() ([]string, time.Duration) {
		t.Helper()
		saved, err := config.Load(path)
		if err != nil {
			t.Fatal(err)
		}
		return saved.PrimaryModels, saved.PrimaryQueueWait
	}

	// 旧版页面：JSON 里没有这些字段
	legacyData, err := json.Marshal(validRuntimeConfig())
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(legacyData, &fields); err != nil {
		t.Fatal(err)
	}
	delete(fields, "primary_models")
	delete(fields, "primary_queue_wait")
	legacyData, _ = json.Marshal(fields)
	var legacy api.RuntimeConfig
	if err := json.Unmarshal(legacyData, &legacy); err != nil || legacy.PrimaryModels != nil || legacy.PrimaryQueueWait != "" {
		t.Fatalf("旧版页面的配置应没有这些字段: %v", err)
	}
	if _, err := admin.UpdateRuntimeConfig(ctx, legacy); err != nil {
		t.Fatal(err)
	}
	if models, wait := load(); !slices.Equal(models, []string{"model-a", "model-b"}) || wait != 20*time.Second {
		t.Fatalf("旧版页面不带这些字段时应沿用现值：%v / %s", models, wait)
	}

	value := validRuntimeConfig()
	models := []string{" models/model-c ", "model-d", "model-c"}
	value.PrimaryModels = &models
	value.PrimaryQueueWait = "7s"
	saved, err := admin.UpdateRuntimeConfig(ctx, value)
	want := []string{"model-c", "model-d"}
	if err != nil || saved.PrimaryModels == nil || !slices.Equal(*saved.PrimaryModels, want) || saved.PrimaryQueueWait != "7s" {
		t.Fatalf("保存返回 %v / %q err=%v，期望 %v / 7s", saved.PrimaryModels, saved.PrimaryQueueWait, err, want)
	}
	if got, wait := load(); !slices.Equal(got, want) || wait != 7*time.Second {
		t.Fatalf("保存后 .env = %v / %s，期望 %v / 7s", got, wait, want)
	}

	empty := []string{}
	value.PrimaryModels = &empty
	value.PrimaryQueueWait = "0s"
	saved, err = admin.UpdateRuntimeConfig(ctx, value)
	if err != nil {
		t.Fatal(err)
	}
	if got, wait := load(); len(got) != 0 || wait != 0 {
		t.Fatalf("空数组与 0s 应清空列表并关闭排队：%v / %s", got, wait)
	}
	data, _ := json.Marshal(saved)
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	if list, ok := fields["primary_models"].([]any); !ok || len(list) != 0 || fields["primary_queue_wait"] != "0s" {
		t.Fatalf("为空时也应返回空数组与 0s，实际 %v / %v", fields["primary_models"], fields["primary_queue_wait"])
	}
	read, err := admin.RuntimeConfig(ctx)
	if err != nil || read.PrimaryModels == nil || read.PrimaryQueueWait != "0s" {
		t.Fatalf("读取配置应返回这些字段: %+v err=%v", read, err)
	}

	var operation *adminOperationError
	for _, wait := range []string{"abc", "-1s", "10m"} {
		value.PrimaryQueueWait = wait
		if _, err := admin.UpdateRuntimeConfig(ctx, value); !errors.As(err, &operation) || operation.status != http.StatusBadRequest || operation.code != "invalid_config" {
			t.Fatalf("排队上限 %q 应返回 invalid_config，实际 err=%v", wait, err)
		}
	}

	active := config.Default()
	changed := active
	changed.PrimaryModels = []string{"model-a"}
	if liveFieldsEqual(changed, active) || requiresRebuild(changed, active) {
		t.Fatal("主用模型列表变化应按热更新处理")
	}
	waitChanged := active
	waitChanged.PrimaryQueueWait = 3 * time.Second
	if liveFieldsEqual(waitChanged, active) || requiresRebuild(waitChanged, active) {
		t.Fatal("排队上限变化应按热更新处理")
	}
	dto := runtimeConfigDTO(changed)
	if sameDataConfig(dto, active, dataConfigOverrides{}) || !sameDataConfig(dto, changed, dataConfigOverrides{}) {
		t.Fatal("生效配置比较应包含主用模型列表")
	}
	dto.PrimaryModels = nil
	dto.PrimaryQueueWait = ""
	if !sameDataConfig(dto, active, dataConfigOverrides{}) {
		t.Fatal("旧版页面不带这些字段时按生效值比较")
	}
	dto.PrimaryQueueWait = "3s"
	if sameDataConfig(dto, active, dataConfigOverrides{}) || !sameDataConfig(dto, waitChanged, dataConfigOverrides{}) {
		t.Fatal("生效配置比较应包含排队上限")
	}
}

// TestPrimaryModelsHotReload 保存配置后主用模型列表与排队上限立即生效，不重启生成服务
func TestPrimaryModelsHotReload(t *testing.T) {
	clearAppConfigEnv(t)
	clearPrimaryConfigEnv(t)
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
	t.Cleanup(func() { _ = workers.Close() })
	service := &trackedService{
		lifecycle: ctx, catalog: emptyCatalog{}, pool: pool, requests: requests, workers: workers,
		forbidden: newForbiddenTracker(), quota: newQuotaSharing("", requests),
	}
	service.setPrimaryModels(cfg.PrimaryModels, cfg.PrimaryQueueWait)
	admin := &runtimeAdmin{lifecycle: ctx, pool: pool, service: service, requests: requests, workers: workers, configPath: path, config: cfg}
	manager := &runtimeManager{
		lifecycle: ctx, configPath: path, activeManagement: cfg, requests: requests, apiKey: newAPIKeyHolder(cfg.ProxyAPIKey),
		intent: &serviceIntent{}, current: &runtimeGeneration{service: service, admin: admin, config: cfg},
	}
	if _, listed := service.primaryQueueWait(testRuntimeModel); listed {
		t.Fatal("默认列表为空")
	}
	if manager.StreamStartWait(testRuntimeModel) != 0 {
		t.Fatal("不在列表里的模型不延长流式首事件等待")
	}

	value := runtimeConfigDTO(cfg)
	models := []string{"Models/Other", "models/" + testRuntimeModel}
	value.PrimaryModels = &models
	value.PrimaryQueueWait = "3s"
	updated, err := manager.UpdateRuntimeConfig(ctx, value)
	if err != nil {
		t.Fatal(err)
	}
	if updated.ServiceRestartRequired {
		t.Fatal("主用模型热更新后不应要求重启生成服务")
	}
	if wait, listed := service.primaryQueueWait("models/" + testRuntimeModel); !listed || wait != 3*time.Second {
		t.Fatalf("保存后列表应立即生效：listed=%v wait=%s", listed, wait)
	}
	if got := manager.StreamStartWait(testRuntimeModel); got != 3*time.Second+primaryStreamStartGrace {
		t.Fatalf("主用模型的流式首事件等待下限 = %s，期望排队上限加余量", got)
	}
	if !slices.Equal(manager.current.config.PrimaryModels, []string{"Models/Other", testRuntimeModel}) || manager.current.config.PrimaryQueueWait != 3*time.Second {
		t.Fatalf("热更新后生效配置 = %v / %s", manager.current.config.PrimaryModels, manager.current.config.PrimaryQueueWait)
	}

	value.PrimaryQueueWait = "0s"
	if _, err := manager.UpdateRuntimeConfig(ctx, value); err != nil {
		t.Fatal(err)
	}
	if wait, listed := service.primaryQueueWait(testRuntimeModel); !listed || wait != 0 {
		t.Fatalf("排队上限改为 0 后应立即生效：listed=%v wait=%s", listed, wait)
	}

	empty := []string{}
	value.PrimaryModels = &empty
	if _, err := manager.UpdateRuntimeConfig(ctx, value); err != nil {
		t.Fatal(err)
	}
	if _, listed := service.primaryQueueWait(testRuntimeModel); listed {
		t.Fatal("清空后应立即恢复默认排队")
	}
}
