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

// TestPrimaryModelsConfig 主用模型默认为空、排队上限默认 10s；模型列表逗号或空白分隔、去 models/ 前缀并去重；
// 排队上限可以为 0（不排队），留空按默认值；进程环境变量优先于 .env
func TestPrimaryModelsConfig(t *testing.T) {
	for _, key := range []string{"PRIMARY_MODELS", "PRIMARY_QUEUE_WAIT"} {
		t.Setenv(key, "")
		os.Unsetenv(key)
	}
	if cfg := Default(); len(cfg.PrimaryModels) != 0 || cfg.PrimaryQueueWait != 10*time.Second {
		t.Fatalf("默认值 = %v / %s，期望为空 / 10s", cfg.PrimaryModels, cfg.PrimaryQueueWait)
	}
	for _, test := range []struct {
		content string
		models  []string
		wait    time.Duration
	}{
		{content: "", models: nil, wait: 10 * time.Second},
		{content: "PRIMARY_MODELS=\nPRIMARY_QUEUE_WAIT=\n", models: nil, wait: 10 * time.Second},
		{content: "PRIMARY_MODELS=model-a\nPRIMARY_QUEUE_WAIT=0\n", models: []string{"model-a"}, wait: 0},
		{content: "PRIMARY_MODELS=\" model-a, models/model-b，model-a  model-c \"\nPRIMARY_QUEUE_WAIT=30s\n", models: []string{"model-a", "model-b", "model-c"}, wait: 30 * time.Second},
	} {
		path := filepath.Join(t.TempDir(), ".env")
		if err := os.WriteFile(path, []byte(test.content), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err := Load(path)
		if err != nil || !slices.Equal(cfg.PrimaryModels, test.models) || cfg.PrimaryQueueWait != test.wait {
			t.Fatalf("%q：PrimaryModels = %v，PrimaryQueueWait = %s，err=%v，期望 %v / %s", test.content, cfg.PrimaryModels, cfg.PrimaryQueueWait, err, test.models, test.wait)
		}
	}
	for _, content := range []string{"PRIMARY_QUEUE_WAIT=-1s\n", "PRIMARY_QUEUE_WAIT=abc\n", "PRIMARY_QUEUE_WAIT=6m\nREQUEST_TIMEOUT=5m\n"} {
		path := filepath.Join(t.TempDir(), ".env")
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "PRIMARY_QUEUE_WAIT") {
			t.Fatalf("%q 应读取失败并指明 PRIMARY_QUEUE_WAIT，实际 err=%v", content, err)
		}
	}

	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte("PRIMARY_MODELS=model-a\nPRIMARY_QUEUE_WAIT=30s\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PRIMARY_MODELS", "model-env")
	t.Setenv("PRIMARY_QUEUE_WAIT", "3s")
	cfg, err := Load(path)
	if err != nil || !slices.Equal(cfg.PrimaryModels, []string{"model-env"}) || cfg.PrimaryQueueWait != 3*time.Second {
		t.Fatalf("环境变量应覆盖 .env：%v / %s err=%v", cfg.PrimaryModels, cfg.PrimaryQueueWait, err)
	}
}

// TestPrimaryModelsValidate 模型 ID 不能为空、不能包含空白或逗号、不能重复；排队上限不能为负、不能超过请求超时
func TestPrimaryModelsValidate(t *testing.T) {
	for _, models := range [][]string{{""}, {"model a"}, {"model-a,model-b"}, {"model-a", "model-a"}} {
		cfg := Default()
		cfg.PrimaryModels = models
		if err := cfg.Validate(); err == nil {
			t.Fatalf("%q 应校验失败", models)
		}
	}
	cfg := Default()
	cfg.PrimaryModels = []string{"model-a", "Model-B"}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("有效列表校验失败: %v", err)
	}
	cfg.PrimaryQueueWait = -time.Second
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "PRIMARY_QUEUE_WAIT") {
		t.Fatalf("负数排队上限应校验失败，实际 err=%v", err)
	}
	cfg.PrimaryQueueWait = cfg.RequestTimeout + time.Second
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "REQUEST_TIMEOUT") {
		t.Fatalf("超过请求超时的排队上限应校验失败，实际 err=%v", err)
	}
	for _, wait := range []time.Duration{0, time.Second, cfg.RequestTimeout} {
		cfg.PrimaryQueueWait = wait
		if err := cfg.Validate(); err != nil {
			t.Fatalf("排队上限 %s 校验失败: %v", wait, err)
		}
	}
}

// TestPrimaryModelsRoundTrip 保存后再读取、JSON 往返都保留列表与排队上限；清空后保存为空；旧版 JSON 没有这些字段时为空与默认 10s
func TestPrimaryModelsRoundTrip(t *testing.T) {
	for _, key := range configKeys {
		t.Setenv(key, "")
		os.Unsetenv(key)
	}
	cfg := Default()
	cfg.PrimaryModels = []string{"model-a", "model-b"}
	cfg.PrimaryQueueWait = 0
	path := filepath.Join(t.TempDir(), ".env")
	if err := cfg.Save(path); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path)
	if err != nil || !slices.Equal(loaded.PrimaryModels, cfg.PrimaryModels) || loaded.PrimaryQueueWait != 0 {
		t.Fatalf(".env 往返 = %v / %s err=%v，期望 %v / 0s", loaded.PrimaryModels, loaded.PrimaryQueueWait, err, cfg.PrimaryModels)
	}
	cleared := loaded
	cleared.PrimaryModels = nil
	cleared.PrimaryQueueWait = 20 * time.Second
	if err := cleared.Save(path); err != nil {
		t.Fatal(err)
	}
	if loaded, err = Load(path); err != nil || len(loaded.PrimaryModels) != 0 || loaded.PrimaryQueueWait != 20*time.Second {
		t.Fatalf("清空后 .env 往返 = %v / %s err=%v，期望为空 / 20s", loaded.PrimaryModels, loaded.PrimaryQueueWait, err)
	}

	cfg.PrimaryQueueWait = 15 * time.Second
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var decoded Config
	if err := json.Unmarshal(data, &decoded); err != nil || !slices.Equal(decoded.PrimaryModels, cfg.PrimaryModels) || decoded.PrimaryQueueWait != 15*time.Second {
		t.Fatalf("JSON 往返 = %v / %s err=%v，期望 %v / 15s", decoded.PrimaryModels, decoded.PrimaryQueueWait, err, cfg.PrimaryModels)
	}
	var legacy map[string]any
	if err := json.Unmarshal(data, &legacy); err != nil {
		t.Fatal(err)
	}
	delete(legacy, "primary_models")
	delete(legacy, "primary_queue_wait")
	legacyData, _ := json.Marshal(legacy)
	var legacyDecoded Config
	if err := json.Unmarshal(legacyData, &legacyDecoded); err != nil || len(legacyDecoded.PrimaryModels) != 0 || legacyDecoded.PrimaryQueueWait != 10*time.Second {
		t.Fatalf("旧版 JSON：PrimaryModels = %v，PrimaryQueueWait = %s，err=%v，期望为空与 10s", legacyDecoded.PrimaryModels, legacyDecoded.PrimaryQueueWait, err)
	}
}
