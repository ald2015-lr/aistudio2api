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

// TestUltraPrimaryModelsConfig Ultra 主用模型默认为 gemini-4-argon、排队上限默认 10s；键不存在时保持默认，写空值表示关闭；
// 列表的整理与排队上限的解析规则与普通主用模型相同；进程环境变量优先于 .env，但空值不生效（关闭只能写在 .env 里）
func TestUltraPrimaryModelsConfig(t *testing.T) {
	for _, key := range configKeys {
		t.Setenv(key, "")
		os.Unsetenv(key)
	}
	defaults := Default()
	if !slices.Equal(defaults.UltraPrimaryModels, []string{"gemini-4-argon"}) || defaults.UltraPrimaryQueueWait != 10*time.Second {
		t.Fatalf("默认值 = %v / %s，期望 [gemini-4-argon] / 10s", defaults.UltraPrimaryModels, defaults.UltraPrimaryQueueWait)
	}
	// 默认列表是副本：改动一份 Default() 不影响下一份
	defaults.UltraPrimaryModels[0] = "changed"
	if again := Default(); again.UltraPrimaryModels[0] != "gemini-4-argon" {
		t.Fatalf("Default() 应返回默认列表的副本，实际 %v", again.UltraPrimaryModels)
	}
	for _, test := range []struct {
		content string
		models  []string
		wait    time.Duration
	}{
		{content: "", models: []string{"gemini-4-argon"}, wait: 10 * time.Second},
		{content: "PRIMARY_MODELS=model-a\n", models: []string{"gemini-4-argon"}, wait: 10 * time.Second},
		{content: "ULTRA_PRIMARY_MODELS=\nULTRA_PRIMARY_QUEUE_WAIT=\n", models: nil, wait: 10 * time.Second},
		{content: "ULTRA_PRIMARY_MODELS=model-u\nULTRA_PRIMARY_QUEUE_WAIT=0\n", models: []string{"model-u"}, wait: 0},
		{content: "ULTRA_PRIMARY_MODELS=\" models/model-u, model-v，model-u \"\nULTRA_PRIMARY_QUEUE_WAIT=30s\n", models: []string{"model-u", "model-v"}, wait: 30 * time.Second},
		{content: "REQUEST_TIMEOUT=6s\n", models: []string{"gemini-4-argon"}, wait: 6 * time.Second},
	} {
		path := filepath.Join(t.TempDir(), ".env")
		if err := os.WriteFile(path, []byte(test.content), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err := Load(path)
		if err != nil || !slices.Equal(cfg.UltraPrimaryModels, test.models) || cfg.UltraPrimaryQueueWait != test.wait {
			t.Fatalf("%q：UltraPrimaryModels = %v，UltraPrimaryQueueWait = %s，err=%v，期望 %v / %s",
				test.content, cfg.UltraPrimaryModels, cfg.UltraPrimaryQueueWait, err, test.models, test.wait)
		}
		// 普通主用模型不受 Ultra 列表影响
		if strings.Contains(test.content, "ULTRA_PRIMARY_MODELS") && len(cfg.PrimaryModels) != 0 {
			t.Fatalf("%q：PrimaryModels = %v，期望为空", test.content, cfg.PrimaryModels)
		}
	}
	for _, content := range []string{"ULTRA_PRIMARY_QUEUE_WAIT=-1s\n", "ULTRA_PRIMARY_QUEUE_WAIT=abc\n", "ULTRA_PRIMARY_QUEUE_WAIT=6m\nREQUEST_TIMEOUT=5m\n"} {
		path := filepath.Join(t.TempDir(), ".env")
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "ULTRA_PRIMARY_QUEUE_WAIT") {
			t.Fatalf("%q 应读取失败并指明 ULTRA_PRIMARY_QUEUE_WAIT，实际 err=%v", content, err)
		}
	}

	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte("ULTRA_PRIMARY_MODELS=model-u\nULTRA_PRIMARY_QUEUE_WAIT=30s\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ULTRA_PRIMARY_MODELS", "model-env")
	t.Setenv("ULTRA_PRIMARY_QUEUE_WAIT", "3s")
	cfg, err := Load(path)
	if err != nil || !slices.Equal(cfg.UltraPrimaryModels, []string{"model-env"}) || cfg.UltraPrimaryQueueWait != 3*time.Second {
		t.Fatalf("环境变量应覆盖 .env：%v / %s err=%v", cfg.UltraPrimaryModels, cfg.UltraPrimaryQueueWait, err)
	}
	// 进程环境变量的空值视为未设置：.env 的值照常生效，不能用空环境变量关闭
	t.Setenv("ULTRA_PRIMARY_MODELS", "")
	t.Setenv("ULTRA_PRIMARY_QUEUE_WAIT", "")
	if cfg, err = Load(path); err != nil || !slices.Equal(cfg.UltraPrimaryModels, []string{"model-u"}) || cfg.UltraPrimaryQueueWait != 30*time.Second {
		t.Fatalf("空环境变量不应覆盖 .env：%v / %s err=%v", cfg.UltraPrimaryModels, cfg.UltraPrimaryQueueWait, err)
	}
}

// TestUltraPrimaryModelsValidate 模型 ID 的校验与普通主用模型相同；排队上限不能为负、不能超过请求超时
func TestUltraPrimaryModelsValidate(t *testing.T) {
	for _, models := range [][]string{{""}, {"model a"}, {"model-a,model-b"}, {"model-a", "model-a"}} {
		cfg := Default()
		cfg.UltraPrimaryModels = models
		if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "ULTRA_PRIMARY_MODELS") {
			t.Fatalf("%q 应校验失败并指明 ULTRA_PRIMARY_MODELS，实际 err=%v", models, err)
		}
	}
	cfg := Default()
	cfg.UltraPrimaryModels = nil
	if err := cfg.Validate(); err != nil {
		t.Fatalf("空列表（关闭）校验失败: %v", err)
	}
	cfg.UltraPrimaryQueueWait = -time.Second
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "ULTRA_PRIMARY_QUEUE_WAIT") {
		t.Fatalf("负数排队上限应校验失败，实际 err=%v", err)
	}
	cfg.UltraPrimaryQueueWait = cfg.RequestTimeout + time.Second
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "ULTRA_PRIMARY_QUEUE_WAIT 不能超过 REQUEST_TIMEOUT") {
		t.Fatalf("超过请求超时的排队上限应校验失败，实际 err=%v", err)
	}
	for _, wait := range []time.Duration{0, time.Second, cfg.RequestTimeout} {
		cfg.UltraPrimaryQueueWait = wait
		if err := cfg.Validate(); err != nil {
			t.Fatalf("排队上限 %s 校验失败: %v", wait, err)
		}
	}
}

// TestUltraPrimaryModelsRoundTrip 保存后再读取、JSON 往返都保留列表与排队上限；清空后保存为空值，再读取仍为关闭（不回到默认）；
// 旧版 JSON 没有这些字段时取默认列表与默认排队上限，显式的空数组为关闭
func TestUltraPrimaryModelsRoundTrip(t *testing.T) {
	for _, key := range configKeys {
		t.Setenv(key, "")
		os.Unsetenv(key)
	}
	cfg := Default()
	cfg.UltraPrimaryModels = []string{"model-u", "model-v"}
	cfg.UltraPrimaryQueueWait = 0
	path := filepath.Join(t.TempDir(), ".env")
	if err := cfg.Save(path); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path)
	if err != nil || !slices.Equal(loaded.UltraPrimaryModels, cfg.UltraPrimaryModels) || loaded.UltraPrimaryQueueWait != 0 {
		t.Fatalf(".env 往返 = %v / %s err=%v，期望 %v / 0s", loaded.UltraPrimaryModels, loaded.UltraPrimaryQueueWait, err, cfg.UltraPrimaryModels)
	}
	cleared := loaded
	cleared.UltraPrimaryModels = nil
	cleared.UltraPrimaryQueueWait = 20 * time.Second
	if err := cleared.Save(path); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(content), "\nULTRA_PRIMARY_MODELS=\n") || !strings.Contains(string(content), "\nULTRA_PRIMARY_QUEUE_WAIT=20s\n") {
		t.Fatalf("清空后 .env 应写空值与具体的排队上限: %q err=%v", content, err)
	}
	if loaded, err = Load(path); err != nil || len(loaded.UltraPrimaryModels) != 0 || loaded.UltraPrimaryQueueWait != 20*time.Second {
		t.Fatalf("清空后 .env 往返 = %v / %s err=%v，期望为空 / 20s", loaded.UltraPrimaryModels, loaded.UltraPrimaryQueueWait, err)
	}
	// 默认值保存时同样写出具体值（不做跟随普通排队上限的三态）
	if err := Default().Save(path); err != nil {
		t.Fatal(err)
	}
	if content, err = os.ReadFile(path); err != nil || !strings.Contains(string(content), "\nULTRA_PRIMARY_MODELS=gemini-4-argon\nULTRA_PRIMARY_QUEUE_WAIT=10s\n") {
		t.Fatalf("默认配置的 .env 应写出 Ultra 主用模型与排队上限: %q err=%v", content, err)
	}

	cfg.UltraPrimaryQueueWait = 15 * time.Second
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var decoded Config
	if err := json.Unmarshal(data, &decoded); err != nil || !slices.Equal(decoded.UltraPrimaryModels, cfg.UltraPrimaryModels) || decoded.UltraPrimaryQueueWait != 15*time.Second {
		t.Fatalf("JSON 往返 = %v / %s err=%v，期望 %v / 15s", decoded.UltraPrimaryModels, decoded.UltraPrimaryQueueWait, err, cfg.UltraPrimaryModels)
	}
	// 清空后的 JSON 写空数组，往返后仍为关闭
	empty := cfg
	empty.UltraPrimaryModels = nil
	if data, err = json.Marshal(empty); err != nil || !strings.Contains(string(data), `"ultra_primary_models":[]`) {
		t.Fatalf("清空的列表应写成空数组: %s err=%v", data, err)
	}
	if err := json.Unmarshal(data, &decoded); err != nil || len(decoded.UltraPrimaryModels) != 0 {
		t.Fatalf("空数组往返 = %v err=%v，期望关闭", decoded.UltraPrimaryModels, err)
	}
	var legacy map[string]any
	if err := json.Unmarshal(data, &legacy); err != nil {
		t.Fatal(err)
	}
	delete(legacy, "ultra_primary_models")
	delete(legacy, "ultra_primary_queue_wait")
	legacyData, _ := json.Marshal(legacy)
	var legacyDecoded Config
	if err := json.Unmarshal(legacyData, &legacyDecoded); err != nil ||
		!slices.Equal(legacyDecoded.UltraPrimaryModels, []string{"gemini-4-argon"}) || legacyDecoded.UltraPrimaryQueueWait != 10*time.Second {
		t.Fatalf("旧版 JSON：UltraPrimaryModels = %v，UltraPrimaryQueueWait = %s，err=%v，期望默认列表与 10s",
			legacyDecoded.UltraPrimaryModels, legacyDecoded.UltraPrimaryQueueWait, err)
	}
	legacy["ultra_primary_models"] = nil
	legacyData, _ = json.Marshal(legacy)
	if err := json.Unmarshal(legacyData, &legacyDecoded); err != nil || !slices.Equal(legacyDecoded.UltraPrimaryModels, []string{"gemini-4-argon"}) {
		t.Fatalf("null 与缺字段相同取默认列表，实际 %v err=%v", legacyDecoded.UltraPrimaryModels, err)
	}
	legacy["ultra_primary_queue_wait"] = "6m"
	legacyData, _ = json.Marshal(legacy)
	if err := json.Unmarshal(legacyData, &legacyDecoded); err == nil || !strings.Contains(err.Error(), "ULTRA_PRIMARY_QUEUE_WAIT") {
		t.Fatalf("超过请求超时的排队上限应解析失败，实际 err=%v", err)
	}
}
