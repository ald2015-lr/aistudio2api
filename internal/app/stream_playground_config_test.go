package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/api"
	"github.com/Mag1cFall/AIStudio2API/internal/config"
)

// TestUpdateRuntimeConfigStreamPlaygroundModels 管理页面保存流式优先 Playground 的模型：整理后写入 .env；旧版页面不带该字段时
// 沿用现值；空数组表示清空；读取与保存结果总是返回该字段（为空时是空数组）
func TestUpdateRuntimeConfigStreamPlaygroundModels(t *testing.T) {
	t.Setenv("STREAM_PLAYGROUND_MODELS", "")
	os.Unsetenv("STREAM_PLAYGROUND_MODELS")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte("STREAM_PLAYGROUND_MODELS=model-a,model-b\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	admin := &runtimeAdmin{configPath: path, requests: newRequestRegistry(ctx)}
	load := func() []string {
		t.Helper()
		saved, err := config.Load(path)
		if err != nil {
			t.Fatal(err)
		}
		return saved.StreamPlaygroundModels
	}

	// 旧版页面：JSON 里没有该字段
	legacyData, err := json.Marshal(validRuntimeConfig())
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(legacyData, &fields); err != nil {
		t.Fatal(err)
	}
	delete(fields, "stream_playground_models")
	legacyData, _ = json.Marshal(fields)
	var legacy api.RuntimeConfig
	if err := json.Unmarshal(legacyData, &legacy); err != nil || legacy.StreamPlaygroundModels != nil {
		t.Fatalf("旧版页面的配置应没有该字段: %v", err)
	}
	if _, err := admin.UpdateRuntimeConfig(ctx, legacy); err != nil || !slices.Equal(load(), []string{"model-a", "model-b"}) {
		t.Fatalf("旧版页面不带该字段时应沿用现值：%v err=%v", load(), err)
	}

	value := validRuntimeConfig()
	models := []string{" models/model-c ", "model-d", "model-c"}
	value.StreamPlaygroundModels = &models
	saved, err := admin.UpdateRuntimeConfig(ctx, value)
	want := []string{"model-c", "model-d"}
	if err != nil || !slices.Equal(load(), want) || saved.StreamPlaygroundModels == nil || !slices.Equal(*saved.StreamPlaygroundModels, want) {
		t.Fatalf("保存后 .env = %v，返回 %v，err=%v，期望 %v", load(), saved.StreamPlaygroundModels, err, want)
	}

	empty := []string{}
	value.StreamPlaygroundModels = &empty
	saved, err = admin.UpdateRuntimeConfig(ctx, value)
	if err != nil || len(load()) != 0 {
		t.Fatalf("空数组应清空：%v err=%v", load(), err)
	}
	data, _ := json.Marshal(saved)
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	if list, ok := fields["stream_playground_models"].([]any); !ok || len(list) != 0 {
		t.Fatalf("为空时也应返回空数组，实际 %v", fields["stream_playground_models"])
	}
	read, err := admin.RuntimeConfig(ctx)
	if err != nil || read.StreamPlaygroundModels == nil {
		t.Fatalf("读取配置应返回该字段: %v", err)
	}

	active := config.Default()
	changed := active
	changed.StreamPlaygroundModels = []string{"model-a"}
	if liveFieldsEqual(changed, active) || requiresRebuild(changed, active) {
		t.Fatal("流式优先 Playground 的模型变化应按热更新处理")
	}
	dto := runtimeConfigDTO(changed)
	if sameDataConfig(dto, active, dataConfigOverrides{}) || !sameDataConfig(dto, changed, dataConfigOverrides{}) {
		t.Fatal("生效配置比较应包含流式优先 Playground 的模型")
	}
	dto.StreamPlaygroundModels = nil
	if !sameDataConfig(dto, active, dataConfigOverrides{}) {
		t.Fatal("旧版页面不带该字段时按生效值比较")
	}
}

// TestStreamPlaygroundModelsHotReload 保存配置后流式优先 Playground 的模型立即生效，不重启生成服务
func TestStreamPlaygroundModelsHotReload(t *testing.T) {
	clearAppConfigEnv(t)
	t.Setenv("STREAM_PLAYGROUND_MODELS", "")
	os.Unsetenv("STREAM_PLAYGROUND_MODELS")
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
	service.setStreamPlaygroundModels(cfg.StreamPlaygroundModels)
	admin := &runtimeAdmin{lifecycle: ctx, pool: pool, service: service, requests: requests, workers: workers, configPath: path, config: cfg}
	manager := &runtimeManager{
		lifecycle: ctx, configPath: path, activeManagement: cfg, requests: requests, apiKey: newAPIKeyHolder(cfg.ProxyAPIKey),
		intent: &serviceIntent{}, current: &runtimeGeneration{service: service, admin: admin, config: cfg},
	}
	if service.streamPlaygroundListed(streamTestModel) {
		t.Fatal("默认列表为空")
	}

	value := runtimeConfigDTO(cfg)
	models := []string{"Models/Other", "models/" + streamTestModel}
	value.StreamPlaygroundModels = &models
	updated, err := manager.UpdateRuntimeConfig(ctx, value)
	if err != nil {
		t.Fatal(err)
	}
	if updated.ServiceRestartRequired {
		t.Fatal("流式优先 Playground 的模型热更新后不应要求重启生成服务")
	}
	if !service.streamPlaygroundListed(streamTestModel) || !service.streamPlaygroundListed("models/"+streamTestModel) {
		t.Fatal("保存后列表应立即生效")
	}
	if !slices.Equal(manager.current.config.StreamPlaygroundModels, []string{"Models/Other", streamTestModel}) {
		t.Fatalf("热更新后生效配置 = %v", manager.current.config.StreamPlaygroundModels)
	}

	empty := []string{}
	value.StreamPlaygroundModels = &empty
	if _, err := manager.UpdateRuntimeConfig(ctx, value); err != nil {
		t.Fatal(err)
	}
	if service.streamPlaygroundListed(streamTestModel) {
		t.Fatal("清空后应立即恢复默认轮询")
	}
}

// TestCatalogMarksBuildUnary 管理页目录标出需要订阅权益（在 Build 通道一次性返回）的模型，免费模型不标。
// 服务配置页只把 channels 同时含 playground 与 build 的这类模型列为可加入的提示：只启用 Build 时目录的 channels 不含 playground，
// 加入列表也不会先试 Playground
func TestCatalogMarksBuildUnary(t *testing.T) {
	service, pool, _ := streamChannelService(t, []string{streamPlaygroundA}, []string{streamBuildA}, nil)
	service.models = []aistudio.Model{
		{ID: "tier-model", Methods: []string{"generateContent"}, AccessModes: []int64{3}},
		{ID: "free-model", Methods: []string{"generateContent"}},
	}
	catalog := service.catalogModels()
	if len(catalog) != 2 || !catalog[0].BuildUnary || catalog[1].BuildUnary {
		t.Fatalf("目录 BuildUnary = %v，期望只标出需要订阅权益的模型", catalog)
	}
	if service.models[0].BuildUnary {
		t.Fatal("标记只应写在返回的目录里，不改动模型快照")
	}

	service.models = []aistudio.Model{{ID: streamTestModel, Methods: []string{"generateContent"}, AccessModes: []int64{3}}}
	if entry := service.catalogModels()[0]; !entry.BuildUnary || !slices.Equal(entry.Channels, []string{"playground", "build"}) {
		t.Fatalf("两个通道都启用时目录 = %+v，期望标出 BuildUnary 且 channels 含 playground 与 build", entry)
	}
	pool.SetUpstreamChannels([]aistudio.Channel{aistudio.ChannelBuild})
	if entry := service.catalogModels()[0]; !entry.BuildUnary || slices.Contains(entry.Channels, "playground") {
		t.Fatalf("只启用 Build 时目录 = %+v，channels 不应含 playground", entry)
	}
	service.setStreamPlaygroundModels([]string{streamTestModel})
	if service.streamPrefersPlayground(aistudio.GenerateRequest{Model: streamTestModel, Stream: true},
		aistudio.AccountSelection{ModelID: streamTestModel, Method: "generateContent"}) {
		t.Fatal("只启用 Build 时加入列表不应先试 Playground")
	}
}
