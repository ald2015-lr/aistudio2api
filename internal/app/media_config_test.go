package app

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/api"
	"github.com/Mag1cFall/AIStudio2API/internal/config"
	"github.com/Mag1cFall/AIStudio2API/internal/mediastore"
)

// newMediaHotManager 创建带一个生成服务实例的管理器（与 IP 池热更新测试相同的装配），配置为默认值
func newMediaHotManager(t *testing.T) (*runtimeManager, string) {
	t.Helper()
	clearAppConfigEnv(t)
	for _, key := range []string{"MEDIA_OUTPUT", "MEDIA_BASE_URL", "MEDIA_RETENTION", "MEDIA_MAX_SIZE"} {
		t.Setenv(key, "")
		os.Unsetenv(key)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
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
	admin := &runtimeAdmin{lifecycle: ctx, pool: pool, service: service, requests: requests, workers: workers, configPath: path, config: cfg}
	manager := &runtimeManager{
		lifecycle: ctx, configPath: path, activeManagement: cfg, requests: requests, apiKey: newAPIKeyHolder(cfg.ProxyAPIKey),
		intent: &serviceIntent{}, current: &runtimeGeneration{service: service, admin: admin, config: cfg},
	}
	media := cfg.Media
	manager.media.Store(&media)
	return manager, path
}

// TestMediaSettingsHotConfig 媒体设置保存后立即生效，不提示需要重启：链接模式与服务地址在下一个请求就按新值；
// 写进 .env 与返回给页面的都是规范写法；旧版页面不带这些字段时沿用现值；无效值返回 400 且不改动生效值；保存期限变化时立即清理一次
func TestMediaSettingsHotConfig(t *testing.T) {
	manager, path := newMediaHotManager(t)
	ctx := context.Background()
	mediaDir := t.TempDir()
	manager.mediaCleaner = mediastore.NewCleaner(mediaDir, manager.mediaCleanupSettings, func(level, message string) {
		manager.requests.log("service", level, message)
	})
	cleanerCtx, stopCleaner := context.WithCancel(ctx)
	defer stopCleaner()
	go manager.mediaCleaner.Run(cleanerCtx)

	expired := filepath.Join(mediaDir, strings.Repeat("a", 64)+".mp4")
	if err := os.WriteFile(expired, []byte("old video"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-3 * time.Hour)
	if err := os.Chtimes(expired, old, old); err != nil {
		t.Fatal(err)
	}
	if enabled, _ := manager.mediaLinks(); enabled {
		t.Fatal("默认应为 inline")
	}

	value := runtimeConfigDTO(config.Default())
	if value.MediaOutput != "inline" || value.MediaBaseURL == nil || *value.MediaBaseURL != "" || value.MediaRetention != "7d" || value.MediaMaxSize != "20GB" {
		t.Fatalf("读取的默认媒体设置 = %q %v %q %q", value.MediaOutput, value.MediaBaseURL, value.MediaRetention, value.MediaMaxSize)
	}
	base := " https://ai.example.com/ "
	value.MediaOutput, value.MediaBaseURL, value.MediaRetention, value.MediaMaxSize = "URL", &base, "2h", "512mb"
	updated, err := manager.UpdateRuntimeConfig(ctx, value)
	if err != nil {
		t.Fatal(err)
	}
	if updated.MediaOutput != "url" || *updated.MediaBaseURL != "https://ai.example.com" || updated.MediaRetention != "2h" || updated.MediaMaxSize != "512MB" {
		t.Fatalf("保存后返回 = %q %q %q %q", updated.MediaOutput, *updated.MediaBaseURL, updated.MediaRetention, updated.MediaMaxSize)
	}
	if updated.ServiceRestartRequired || updated.ManagementRestartRequired {
		t.Fatal("媒体设置保存后立即生效，不应提示需要重启")
	}
	if enabled, linkBase := manager.mediaLinks(); !enabled || linkBase != "https://ai.example.com" {
		t.Fatalf("保存后立即生效: %v %q", enabled, linkBase)
	}
	if retention, maxBytes := manager.mediaCleanupSettings(); retention != 2*time.Hour || maxBytes != 512<<20 {
		t.Fatalf("清理设置 = %s %d", retention, maxBytes)
	}
	data, _ := os.ReadFile(path)
	for _, line := range []string{"MEDIA_OUTPUT=url\n", "MEDIA_BASE_URL=https://ai.example.com\n", "MEDIA_RETENTION=2h\n", "MEDIA_MAX_SIZE=512MB\n"} {
		if !strings.Contains(string(data), line) {
			t.Fatalf(".env 缺少 %q", line)
		}
	}
	if !hasLogContaining(manager.requests, "媒体输出设置已更新并立即生效", "输出=url", "链接地址=https://ai.example.com") {
		t.Fatal("缺少媒体设置生效日志")
	}
	// 保存期限从 7 天改为 2 小时：立即清理一次，3 小时前的文件被删除，并写一行汇总日志（删除之后才写，所以一起等）
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, statErr := os.Lstat(expired)
		if errors.Is(statErr, os.ErrNotExist) && hasLogContaining(manager.requests, "媒体目录已清理", "删除=1 个文件") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("保存期限变化后应立即清理并写汇总日志: %v", statErr)
		}
		time.Sleep(10 * time.Millisecond)
	}

	read, err := manager.RuntimeConfig(ctx)
	if err != nil || read.MediaOutput != "url" || read.ServiceRestartRequired {
		t.Fatalf("读取配置: %+v %v", read.MediaOutput, err)
	}

	// 旧版页面不带媒体字段：沿用现值
	legacy := read
	legacy.MediaOutput, legacy.MediaBaseURL, legacy.MediaRetention, legacy.MediaMaxSize = "", nil, "", ""
	if _, err := manager.UpdateRuntimeConfig(ctx, legacy); err != nil {
		t.Fatal(err)
	}
	if enabled, linkBase := manager.mediaLinks(); !enabled || linkBase != "https://ai.example.com" {
		t.Fatalf("旧版页面保存后应沿用现值: %v %q", enabled, linkBase)
	}

	// 无效值返回 400，生效值不变
	for _, invalid := range []func(*api.RuntimeConfig){
		func(value *api.RuntimeConfig) { value.MediaOutput = "link" },
		func(value *api.RuntimeConfig) { bad := "ftp://ai.example.com"; value.MediaBaseURL = &bad },
		func(value *api.RuntimeConfig) {
			bad := "https://user:secret-value@ai.example.com"
			value.MediaBaseURL = &bad
		},
		func(value *api.RuntimeConfig) { value.MediaRetention = "10m" },
		func(value *api.RuntimeConfig) { value.MediaMaxSize = "20" },
	} {
		submitted := read
		invalid(&submitted)
		_, err := manager.UpdateRuntimeConfig(ctx, submitted)
		var operation *adminOperationError
		if !errors.As(err, &operation) || operation.status != http.StatusBadRequest || strings.Contains(err.Error(), "secret-value") {
			t.Fatalf("无效的媒体设置应返回 400 且不含密码: %v", err)
		}
	}
	if enabled, linkBase := manager.mediaLinks(); !enabled || linkBase != "https://ai.example.com" {
		t.Fatalf("无效值不应改动生效值: %v %q", enabled, linkBase)
	}

	// 切回 inline 并清空地址：立即生效
	empty := ""
	read.MediaOutput, read.MediaBaseURL = "inline", &empty
	if _, err := manager.UpdateRuntimeConfig(ctx, read); err != nil {
		t.Fatal(err)
	}
	if enabled, linkBase := manager.mediaLinks(); enabled || linkBase != "" {
		t.Fatalf("切回 inline 后 = %v %q", enabled, linkBase)
	}
}

// TestMediaSettingsFollowStartService 手动改过 .env 后停止再启动：新生成服务读到的媒体设置立即生效
func TestMediaSettingsFollowStartService(t *testing.T) {
	for _, key := range []string{"MEDIA_OUTPUT", "MEDIA_BASE_URL", "MEDIA_RETENTION", "MEDIA_MAX_SIZE"} {
		t.Setenv(key, "")
		os.Unsetenv(key)
	}
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
	media := cfg.Media
	manager.media.Store(&media)
	edited := cfg
	edited.Media.Output = config.MediaOutputURL
	edited.Media.BaseURL = "https://media.example.com"
	if err := edited.Save(path); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.StartService(ctx); err != nil {
		t.Fatal(err)
	}
	if enabled, base := manager.mediaLinks(); !enabled || base != "https://media.example.com" {
		t.Fatalf("启动后应按 .env 的媒体设置: %v %q", enabled, base)
	}
}

// TestRootHandlerRoutesMedia /media/ 交给公开 API（不需要管理令牌，也不经过管理页面）
func TestRootHandlerRoutesMedia(t *testing.T) {
	var reached string
	apiHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = r.URL.Path
		w.WriteHeader(http.StatusTeapot)
	})
	path := "/media/" + strings.Repeat("a", 64) + ".mp4"
	recorder := httptest.NewRecorder()
	rootHandler(apiHandler, "", "secret-token").ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
	if recorder.Code != http.StatusTeapot || reached != path {
		t.Fatalf("/media/ 应交给公开 API: %d %q", recorder.Code, reached)
	}
}
