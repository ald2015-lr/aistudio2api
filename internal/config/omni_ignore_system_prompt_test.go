package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// TestOmniIgnoreSystemPromptConfig 视频模型忽略系统提示默认开启，可显式关闭，非布尔值按默认值加载并记入无效项；进程环境变量优先于 .env；
// .env 与管理接口的 JSON 往返不变，旧版 JSON 没有该字段时按默认开启
func TestOmniIgnoreSystemPromptConfig(t *testing.T) {
	t.Setenv("OMNI_IGNORE_SYSTEM_PROMPT", "")
	os.Unsetenv("OMNI_IGNORE_SYSTEM_PROMPT")
	if !slices.Contains(configKeys[:], "OMNI_IGNORE_SYSTEM_PROMPT") {
		t.Fatal("configKeys 缺少 OMNI_IGNORE_SYSTEM_PROMPT：Save 会丢掉这个键，进程环境变量也覆盖不了它")
	}
	if !Default().OmniIgnoreSystemPrompt {
		t.Fatal("OMNI_IGNORE_SYSTEM_PROMPT 默认应开启")
	}
	for _, test := range []struct {
		content string
		want    bool
		fail    bool
	}{
		{content: "", want: true},
		{content: "OMNI_IGNORE_SYSTEM_PROMPT=\n", want: true},
		{content: "OMNI_IGNORE_SYSTEM_PROMPT=false\n", want: false},
		{content: "OMNI_IGNORE_SYSTEM_PROMPT= true \n", want: true},
		{content: "OMNI_IGNORE_SYSTEM_PROMPT=0\n", want: false},
		{content: "OMNI_IGNORE_SYSTEM_PROMPT=off\n", fail: true},
	} {
		path := filepath.Join(t.TempDir(), ".env")
		if err := os.WriteFile(path, []byte(test.content), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err := Load(path)
		if test.fail {
			if requireInvalidKey(t, test.content, cfg, err, "OMNI_IGNORE_SYSTEM_PROMPT"); !cfg.OmniIgnoreSystemPrompt {
				t.Fatalf("%q：无效时应取默认值 true", test.content)
			}
			continue
		}
		if err != nil || cfg.OmniIgnoreSystemPrompt != test.want {
			t.Fatalf("%q: OmniIgnoreSystemPrompt=%v err=%v，期望 %v", test.content, cfg.OmniIgnoreSystemPrompt, err, test.want)
		}
	}
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte("OMNI_IGNORE_SYSTEM_PROMPT=true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OMNI_IGNORE_SYSTEM_PROMPT", "false")
	if cfg, err := Load(path); err != nil || cfg.OmniIgnoreSystemPrompt {
		t.Fatalf("环境变量 false 应覆盖 .env: %v %v", cfg.OmniIgnoreSystemPrompt, err)
	}
	os.Unsetenv("OMNI_IGNORE_SYSTEM_PROMPT")

	cfg := Default()
	cfg.OmniIgnoreSystemPrompt = false
	path = filepath.Join(t.TempDir(), ".env")
	if err := cfg.Save(path); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if !containsLine(string(data), "OMNI_IGNORE_SYSTEM_PROMPT=false") {
		t.Fatalf(".env 缺少 OMNI_IGNORE_SYSTEM_PROMPT=false:\n%s", data)
	}
	if loaded, err := Load(path); err != nil || loaded.OmniIgnoreSystemPrompt {
		t.Fatalf("保存后读回 OmniIgnoreSystemPrompt=%v err=%v", loaded.OmniIgnoreSystemPrompt, err)
	}

	encoded, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var decoded Config
	if err := json.Unmarshal(encoded, &decoded); err != nil || decoded.OmniIgnoreSystemPrompt {
		t.Fatalf("JSON 往返 OmniIgnoreSystemPrompt=%v err=%v", decoded.OmniIgnoreSystemPrompt, err)
	}
	var legacy map[string]any
	_ = json.Unmarshal(encoded, &legacy)
	if _, ok := legacy["omni_ignore_system_prompt"]; !ok {
		t.Fatalf("管理接口的 JSON 应带 omni_ignore_system_prompt: %s", encoded)
	}
	delete(legacy, "omni_ignore_system_prompt")
	legacyJSON, _ := json.Marshal(legacy)
	if err := json.Unmarshal(legacyJSON, &decoded); err != nil || !decoded.OmniIgnoreSystemPrompt {
		t.Fatalf("旧版 JSON 没有该字段时应按默认开启: %v %v", decoded.OmniIgnoreSystemPrompt, err)
	}
}
