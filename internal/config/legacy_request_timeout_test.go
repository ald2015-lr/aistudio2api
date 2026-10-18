package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestLegacyShortRequestTimeoutLoads 旧配置的 REQUEST_TIMEOUT 短于 10s 且没有 PRIMARY_QUEUE_WAIT：升级后 .env 与 JSON 都要能载入
// （排队上限按默认 10s 与请求超时中较小者），不能因为没启用的功能让整个服务起不来；显式配置超过请求超时的排队上限仍然拒绝
func TestLegacyShortRequestTimeoutLoads(t *testing.T) {
	for _, key := range configKeys {
		t.Setenv(key, "")
		os.Unsetenv(key)
	}
	for _, test := range []struct {
		content string
		wait    time.Duration
	}{
		{content: "AISTUDIO_AUTH_STATES=auth\nREQUEST_TIMEOUT=5s\n", wait: 5 * time.Second},
		{content: "AISTUDIO_AUTH_STATES=auth\nREQUEST_TIMEOUT=8s\nPRIMARY_QUEUE_WAIT=\n", wait: 8 * time.Second},
		{content: "AISTUDIO_AUTH_STATES=auth\nREQUEST_TIMEOUT=30s\n", wait: 10 * time.Second},
		{content: "AISTUDIO_AUTH_STATES=auth\nREQUEST_TIMEOUT=8s\nPRIMARY_QUEUE_WAIT=3s\n", wait: 3 * time.Second},
	} {
		path := filepath.Join(t.TempDir(), ".env")
		if err := os.WriteFile(path, []byte(test.content), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("%q：旧 .env 应能载入，实际错误: %v", test.content, err)
		}
		if cfg.PrimaryQueueWait != test.wait {
			t.Fatalf("%q：PrimaryQueueWait = %s，期望 %s", test.content, cfg.PrimaryQueueWait, test.wait)
		}
		// Ultra 主用模型的排队上限同规则：没有配置时同样按请求超时封顶，旧 .env 升级后照常载入
		if want := min(10*time.Second, cfg.RequestTimeout); cfg.UltraPrimaryQueueWait != want {
			t.Fatalf("%q：UltraPrimaryQueueWait = %s，期望 %s", test.content, cfg.UltraPrimaryQueueWait, want)
		}
	}
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte("AISTUDIO_AUTH_STATES=auth\nREQUEST_TIMEOUT=8s\nPRIMARY_QUEUE_WAIT=9s\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "PRIMARY_QUEUE_WAIT") {
		t.Fatalf("显式超过请求超时的排队上限应读取失败并指明 PRIMARY_QUEUE_WAIT，实际 err=%v", err)
	}

	// 进程环境变量给出的短请求超时同样适用
	t.Setenv("REQUEST_TIMEOUT", "6s")
	if cfg, err := Load(path + ".missing"); err != nil || cfg.PrimaryQueueWait != 6*time.Second {
		t.Fatalf("环境变量 REQUEST_TIMEOUT=6s：PrimaryQueueWait = %s err=%v，期望 6s", cfg.PrimaryQueueWait, err)
	}
	os.Unsetenv("REQUEST_TIMEOUT")

	// 旧版 JSON 没有 primary_queue_wait 字段时同样按请求超时封顶
	legacy := Default()
	legacy.RequestTimeout = 5 * time.Second
	legacy.PrimaryQueueWait = 5 * time.Second
	legacy.UltraPrimaryQueueWait = 5 * time.Second
	data, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	delete(fields, "primary_queue_wait")
	delete(fields, "primary_models")
	delete(fields, "ultra_primary_queue_wait")
	delete(fields, "ultra_primary_models")
	legacyData, _ := json.Marshal(fields)
	var decoded Config
	if err := json.Unmarshal(legacyData, &decoded); err != nil {
		t.Fatalf("旧版 JSON（request_timeout=5s，没有 primary_queue_wait）应能解析，实际错误: %v", err)
	}
	if decoded.PrimaryQueueWait != 5*time.Second || len(decoded.PrimaryModels) != 0 {
		t.Fatalf("旧版 JSON：PrimaryQueueWait = %s，PrimaryModels = %v，期望 5s 与空", decoded.PrimaryQueueWait, decoded.PrimaryModels)
	}
	// 没有 Ultra 主用模型字段的旧版 JSON：列表取默认，排队上限同样按请求超时封顶
	if decoded.UltraPrimaryQueueWait != 5*time.Second || !slices.Equal(decoded.UltraPrimaryModels, []string{"gemini-4-argon"}) {
		t.Fatalf("旧版 JSON：UltraPrimaryQueueWait = %s，UltraPrimaryModels = %v，期望 5s 与默认列表", decoded.UltraPrimaryQueueWait, decoded.UltraPrimaryModels)
	}
}
