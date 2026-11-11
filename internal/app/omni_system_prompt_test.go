package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/api"
	"github.com/Mag1cFall/AIStudio2API/internal/config"
)

// TestOmniIgnoreSystemPromptHotConfig 视频模型忽略系统提示保存后立即生效、不提示需要重启：写进 .env，读取时总是返回；
// 旧版页面不带该字段时沿用现值
func TestOmniIgnoreSystemPromptHotConfig(t *testing.T) {
	t.Setenv("OMNI_IGNORE_SYSTEM_PROMPT", "")
	os.Unsetenv("OMNI_IGNORE_SYSTEM_PROMPT")
	manager, path := newMediaHotManager(t)
	manager.omniIgnoreSystemPrompt.Store(true)
	ctx := context.Background()
	value := runtimeConfigDTO(config.Default())
	if value.OmniIgnoreSystemPrompt == nil || !*value.OmniIgnoreSystemPrompt {
		t.Fatalf("读取的默认值 = %v", value.OmniIgnoreSystemPrompt)
	}
	off := false
	value.OmniIgnoreSystemPrompt = &off
	updated, err := manager.UpdateRuntimeConfig(ctx, value)
	if err != nil {
		t.Fatal(err)
	}
	if updated.OmniIgnoreSystemPrompt == nil || *updated.OmniIgnoreSystemPrompt || updated.ServiceRestartRequired || updated.ManagementRestartRequired {
		t.Fatalf("保存后返回 = %v，需要重启 = %v/%v", updated.OmniIgnoreSystemPrompt, updated.ServiceRestartRequired, updated.ManagementRestartRequired)
	}
	if manager.activeOmniIgnoreSystemPrompt() {
		t.Fatal("保存后应立即生效")
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "OMNI_IGNORE_SYSTEM_PROMPT=false\n") {
		t.Fatalf(".env 缺少 OMNI_IGNORE_SYSTEM_PROMPT=false:\n%s", data)
	}
	if !hasLogContaining(manager.requests, "视频模型忽略系统提示设置已更新并立即生效", "开启=false") {
		t.Fatal("缺少视频模型忽略系统提示生效日志")
	}
	read, err := manager.RuntimeConfig(ctx)
	if err != nil || read.OmniIgnoreSystemPrompt == nil || *read.OmniIgnoreSystemPrompt || read.ServiceRestartRequired {
		t.Fatalf("读取配置: %v %v", read.OmniIgnoreSystemPrompt, err)
	}
	// 旧版页面不带该字段：沿用现值
	legacy := read
	legacy.OmniIgnoreSystemPrompt = nil
	if _, err := manager.UpdateRuntimeConfig(ctx, legacy); err != nil {
		t.Fatal(err)
	}
	if manager.activeOmniIgnoreSystemPrompt() {
		t.Fatal("旧版页面保存后应沿用现值（关闭）")
	}
	on := true
	read.OmniIgnoreSystemPrompt = &on
	if _, err := manager.UpdateRuntimeConfig(ctx, read); err != nil || !manager.activeOmniIgnoreSystemPrompt() {
		t.Fatalf("重新开启应立即生效: %v", err)
	}
}

// TestOmniIgnoreSystemPromptLiveFields 视频模型忽略系统提示是热更新字段：只有它不同时 liveFieldsEqual 为假、requiresRebuild 为假；
// 生效配置比较包含它，旧版页面不带这个字段时按生效值比较
func TestOmniIgnoreSystemPromptLiveFields(t *testing.T) {
	active := config.Default()
	changed := active
	changed.OmniIgnoreSystemPrompt = false
	if liveFieldsEqual(changed, active) || requiresRebuild(changed, active) {
		t.Fatal("视频模型忽略系统提示变化应按热更新处理")
	}
	dto := runtimeConfigDTO(changed)
	if sameDataConfig(dto, active, dataConfigOverrides{}) || !sameDataConfig(dto, changed, dataConfigOverrides{}) {
		t.Fatal("生效配置比较应包含视频模型忽略系统提示")
	}
	dto.OmniIgnoreSystemPrompt = nil
	if !sameDataConfig(dto, active, dataConfigOverrides{}) {
		t.Fatal("旧版页面不带这个字段时按生效值比较")
	}
}

// TestOmniIgnoreSystemPromptFollowsStartService 手动改过 .env 后停止再启动：新生成服务读到的 OMNI_IGNORE_SYSTEM_PROMPT 立即生效
func TestOmniIgnoreSystemPromptFollowsStartService(t *testing.T) {
	clearAppConfigEnv(t)
	t.Setenv("OMNI_IGNORE_SYSTEM_PROMPT", "")
	os.Unsetenv("OMNI_IGNORE_SYSTEM_PROMPT")
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
	manager.omniIgnoreSystemPrompt.Store(true)
	edited := cfg
	edited.OmniIgnoreSystemPrompt = false
	if err := edited.Save(path); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.StartService(ctx); err != nil {
		t.Fatal(err)
	}
	if manager.current.config.OmniIgnoreSystemPrompt {
		t.Fatal("新生成服务应读到 OMNI_IGNORE_SYSTEM_PROMPT=false")
	}
	if manager.activeOmniIgnoreSystemPrompt() {
		t.Fatal("生成服务配置 OMNI_IGNORE_SYSTEM_PROMPT=false，公开 API 仍去掉系统提示")
	}
	if !hasLogContaining(manager.requests, "视频模型忽略系统提示设置已更新并立即生效", "开启=false") {
		t.Fatal("缺少视频模型忽略系统提示生效日志")
	}
}

// omniPromptService 为带 Omni 目录的生成服务：记录协议层交给生成服务的请求，再交给 trackedService（选号前写请求日志说明）
type omniPromptService struct {
	tracked *trackedService
	last    *aistudio.GenerateRequest
}

func (service *omniPromptService) Models(context.Context) ([]aistudio.Model, error) {
	return []aistudio.Model{{
		ID: "gemini-omni-1.1-flash", Name: "Gemini Omni 1.1 Flash", Methods: []string{"generateContent"},
		Capabilities: map[string]bool{"chat_model": true, "thinking": true, "thinking_level": true, "thinking_summaries": true, "interactions_api": true},
		CapabilityOptions: map[string][]string{
			"video_aspect_ratios": {"16:9", "9:16"}, "video_output_resolutions": {"360p", "720p"},
			"video_duration_range": {"min=4", "max=15", "step=1", "default=10"}, "video_frame_rates": {"24fps"},
		},
	}}, nil
}

func (service *omniPromptService) CountTokens(context.Context, aistudio.TokenCountRequest) (aistudio.TokenCount, error) {
	return aistudio.TokenCount{}, nil
}

func (service *omniPromptService) Generate(ctx context.Context, request aistudio.GenerateRequest) (<-chan aistudio.Event, error) {
	copied := request
	service.last = &copied
	return service.tracked.Generate(ctx, request)
}

// requestLogMessages 返回该请求的管理日志里的 WARN 消息
func requestLogMessages(requests *requestRegistry, id string) []string {
	requests.mu.Lock()
	defer requests.mu.Unlock()
	var messages []string
	for _, entry := range requests.logs {
		if entry.Level == "WARN" && entry.Request != nil && entry.Request.ID == id {
			messages = append(messages, entry.Message)
		}
	}
	return messages
}

// TestOmniIgnoreSystemPromptRequestLog 公开 API（与 runServer 相同，经 publicAPIConfig 构造）每个请求读取管理器当前的设置：开启时 Omni 请求的
// 系统提示不交给生成服务，该请求的管理日志记一行去掉的字数（不含原文）；在管理页关闭后下一个请求立即照常带上系统提示、不记说明
func TestOmniIgnoreSystemPromptRequestLog(t *testing.T) {
	t.Setenv("OMNI_IGNORE_SYSTEM_PROMPT", "")
	os.Unsetenv("OMNI_IGNORE_SYSTEM_PROMPT")
	manager, _ := newMediaHotManager(t)
	manager.omniIgnoreSystemPrompt.Store(true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	requests := newRequestRegistry(ctx)
	service := &omniPromptService{tracked: &trackedService{
		lifecycle: ctx, pool: aistudio.NewAccountPool(nil, 1), requests: requests,
		forbidden: newForbiddenTracker(), quota: newQuotaSharing("", requests),
	}}
	handler := api.NewHandler(service, publicAPIConfig(manager, manager.activeManagement, "", nil))
	send := func() aistudio.GenerateRequest {
		t.Helper()
		service.last = nil
		body := `{"model":"gemini-omni-1.1-flash-9x16","messages":[{"role":"system","content":"You are a helpful assistant."},{"role":"user","content":"随机生成一个视频"}]}`
		request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer "+manager.activeAPIKey())
		request.Header.Set("Content-Type", "application/json")
		handler.ServeHTTP(httptest.NewRecorder(), request)
		if service.last == nil {
			t.Fatal("请求没有交给生成服务")
		}
		return *service.last
	}
	const note = "视频模型忽略系统提示：已去掉 28 字的系统提示，不发给上游（OMNI_IGNORE_SYSTEM_PROMPT）"
	sent := send()
	if sent.System != "" {
		t.Fatalf("开启时系统提示不应交给生成服务: %q", sent.System)
	}
	found := false
	for _, message := range requestLogMessages(requests, sent.ID) {
		found = found || message == note
		if strings.Contains(message, "helpful") {
			t.Fatalf("请求日志不应写系统提示原文: %q", message)
		}
	}
	if !found {
		t.Fatalf("该请求的管理日志应有说明 %q: %q", note, requestLogMessages(requests, sent.ID))
	}

	read, err := manager.RuntimeConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	off := false
	read.OmniIgnoreSystemPrompt = &off
	if _, err := manager.UpdateRuntimeConfig(ctx, read); err != nil {
		t.Fatal(err)
	}
	sent = send()
	if sent.System != "You are a helpful assistant." {
		t.Fatalf("关闭后下一个请求应照常带上系统提示: %q", sent.System)
	}
	for _, message := range requestLogMessages(requests, sent.ID) {
		if strings.Contains(message, "视频模型忽略系统提示") {
			t.Fatalf("关闭后不应记说明: %q", message)
		}
	}
}

// TestPublicAPIConfigFollowsManager runServer 交给公开 API 的热更新设置都读取管理器的当前值：切换后下一次读取即为新值
// （读取函数为 nil 时公开 API 按默认值处理，接线丢了也不会报错，这里逐项核对）
func TestPublicAPIConfigFollowsManager(t *testing.T) {
	manager, _ := newMediaHotManager(t)
	value := publicAPIConfig(manager, manager.activeManagement, "token", nil)
	if value.OmniIgnoreSystemPrompt == nil || value.StreamDataHeartbeat == nil || value.UltraExclusive == nil ||
		value.APIKeyFunc == nil || value.MediaLinks == nil || value.Admin == nil || value.AdminToken != "token" {
		t.Fatalf("热更新设置应接到管理器上: %+v", value)
	}
	for _, enabled := range []bool{true, false, true} {
		manager.applyOmniIgnoreSystemPrompt(enabled)
		manager.applyStreamDataHeartbeat(enabled)
		manager.applyUltraExclusive(enabled)
		if value.OmniIgnoreSystemPrompt() != enabled || value.StreamDataHeartbeat() != enabled || value.UltraExclusive() != enabled {
			t.Fatalf("切换为 %v 后读取: 忽略系统提示=%v 数据型心跳=%v Ultra 独占=%v", enabled,
				value.OmniIgnoreSystemPrompt(), value.StreamDataHeartbeat(), value.UltraExclusive())
		}
	}
	manager.applyAPIKey("sk-updated")
	if value.APIKeyFunc() != "sk-updated" {
		t.Fatalf("API Key 应立即切换: %q", value.APIKeyFunc())
	}
}
