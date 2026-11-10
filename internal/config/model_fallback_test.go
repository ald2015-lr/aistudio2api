package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestModelFallbacksConfig MODEL_FALLBACKS 默认为 gemini-4-argon=gemini-4-carbon、MODEL_FALLBACK_SCOPE 默认为 ultra；键不存在时保持默认，
// MODEL_FALLBACKS 写空值表示没有规则；规则整理成规范写法；写法无效时按默认值启动并记入无效项，Validate（管理页保存）拒绝；
// 保存与 JSON 往返保留两项
func TestModelFallbacksConfig(t *testing.T) {
	for _, key := range configKeys {
		t.Setenv(key, "")
		os.Unsetenv(key)
	}
	defaults := Default()
	if defaults.ModelFallbacks != "gemini-4-argon=gemini-4-carbon" || defaults.ModelFallbackScope != ModelFallbackScopeUltra {
		t.Fatalf("默认值 = %q / %q", defaults.ModelFallbacks, defaults.ModelFallbackScope)
	}
	load := func(t *testing.T, content string) (Config, error) {
		t.Helper()
		path := filepath.Join(t.TempDir(), ".env")
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return Load(path)
	}
	for _, test := range []struct {
		content, rules, scope string
	}{
		{content: "", rules: DefaultModelFallbacks, scope: ModelFallbackScopeUltra},
		{content: "MODEL_FALLBACKS=\nMODEL_FALLBACK_SCOPE=\n", rules: "", scope: ModelFallbackScopeUltra},
		{content: "MODEL_FALLBACK_SCOPE= ALL \n", rules: DefaultModelFallbacks, scope: ModelFallbackScopeAll},
		{content: "MODEL_FALLBACK_SCOPE=off\n", rules: DefaultModelFallbacks, scope: ModelFallbackScopeOff},
		{
			content: "MODEL_FALLBACKS=\" models/model-a = model-b | models/model-c ，model-d=model-a,\"\n",
			rules:   "model-a=model-b|model-c,model-d=model-a", scope: ModelFallbackScopeUltra,
		},
	} {
		cfg, err := load(t, test.content)
		if err != nil || cfg.ModelFallbacks != test.rules || cfg.ModelFallbackScope != test.scope || len(cfg.Invalid) != 0 {
			t.Fatalf("%q：%q / %q invalid=%v err=%v，期望 %q / %q", test.content, cfg.ModelFallbacks, cfg.ModelFallbackScope, cfg.Invalid, err, test.rules, test.scope)
		}
	}
	for _, test := range []struct {
		content, key, reason string
	}{
		{content: "MODEL_FALLBACKS=model-a=model-a\n", key: "MODEL_FALLBACKS", reason: "与源模型相同"},
		{content: "MODEL_FALLBACKS=model-a=model-b,MODEL-A=model-c\n", key: "MODEL_FALLBACKS", reason: "重复"},
		{content: "MODEL_FALLBACKS=model-a=model-b|model-b\n", key: "MODEL_FALLBACKS", reason: "重复"},
		{content: "MODEL_FALLBACKS=model-a=\n", key: "MODEL_FALLBACKS", reason: "不能为空"},
		{content: "MODEL_FALLBACKS==model-b\n", key: "MODEL_FALLBACKS", reason: "不能为空"},
		{content: "MODEL_FALLBACKS=model-a\n", key: "MODEL_FALLBACKS", reason: "应写成"},
		{content: "MODEL_FALLBACKS=\"model a=model-b\"\n", key: "MODEL_FALLBACKS", reason: "不能包含空白"},
		{content: "MODEL_FALLBACKS=model-a=model-b=model-c\n", key: "MODEL_FALLBACKS", reason: "不能包含"},
		{content: "MODEL_FALLBACK_SCOPE=sometimes\n", key: "MODEL_FALLBACK_SCOPE", reason: "ultra、all 或 off"},
	} {
		cfg, err := load(t, test.content)
		item := requireInvalidKey(t, test.content, cfg, err, test.key)
		if !strings.Contains(item.Reason, test.reason) {
			t.Fatalf("%q：原因 = %q，期望含 %q", test.content, item.Reason, test.reason)
		}
		// 无效项按默认值生效
		if cfg.ModelFallbacks != DefaultModelFallbacks || cfg.ModelFallbackScope != ModelFallbackScopeUltra {
			t.Fatalf("%q：生效值 = %q / %q，期望默认值", test.content, cfg.ModelFallbacks, cfg.ModelFallbackScope)
		}
	}
	// 管理页保存：Validate 拒绝无效值
	for _, mutate := range []func(*Config){
		func(c *Config) { c.ModelFallbacks = "model-a=model-a" },
		func(c *Config) { c.ModelFallbacks = "model-a=" },
		func(c *Config) { c.ModelFallbackScope = "" },
		func(c *Config) { c.ModelFallbackScope = "ULTRA" },
	} {
		cfg := Default()
		mutate(&cfg)
		if err := cfg.Validate(); err == nil {
			t.Fatalf("%q / %q 应校验失败", cfg.ModelFallbacks, cfg.ModelFallbackScope)
		}
	}
	// 保存写出两项（空规则写空值），重新加载得到同样的值
	cfg := Default()
	cfg.ModelFallbacks, cfg.ModelFallbackScope = "", ModelFallbackScopeAll
	path := filepath.Join(t.TempDir(), ".env")
	if err := cfg.Save(path); err != nil {
		t.Fatal(err)
	}
	content, _ := os.ReadFile(path)
	if !strings.Contains(string(content), "\nMODEL_FALLBACKS=\n") || !strings.Contains(string(content), "\nMODEL_FALLBACK_SCOPE=all\n") {
		t.Fatalf(".env 应写出两项：\n%s", content)
	}
	if reloaded, err := Load(path); err != nil || reloaded.ModelFallbacks != "" || reloaded.ModelFallbackScope != ModelFallbackScopeAll {
		t.Fatalf("重新加载 = %q / %q err=%v", reloaded.ModelFallbacks, reloaded.ModelFallbackScope, err)
	}
	// JSON 往返：空规则保持为空；旧版数据没有这两个字段时按默认值
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var decoded Config
	if err := json.Unmarshal(data, &decoded); err != nil || decoded.ModelFallbacks != "" || decoded.ModelFallbackScope != ModelFallbackScopeAll {
		t.Fatalf("JSON 往返 = %q / %q err=%v", decoded.ModelFallbacks, decoded.ModelFallbackScope, err)
	}
	var legacy map[string]any
	_ = json.Unmarshal(data, &legacy)
	delete(legacy, "model_fallbacks")
	delete(legacy, "model_fallback_scope")
	data, _ = json.Marshal(legacy)
	if err := json.Unmarshal(data, &decoded); err != nil || decoded.ModelFallbacks != DefaultModelFallbacks || decoded.ModelFallbackScope != ModelFallbackScopeUltra {
		t.Fatalf("旧版数据 = %q / %q err=%v，期望默认值", decoded.ModelFallbacks, decoded.ModelFallbackScope, err)
	}
	legacy["model_fallbacks"] = "a=a"
	data, _ = json.Marshal(legacy)
	if err := json.Unmarshal(data, &decoded); err == nil {
		t.Fatal("JSON 里无效的规则应解析失败")
	}
}

// TestParseModelFallbacks 规则解析：规范写法、目标顺序与比较不分大小写
func TestParseModelFallbacks(t *testing.T) {
	rules, err := ParseModelFallbacks("a=b|C, d = e")
	if err != nil || len(rules) != 2 || rules[0].Source != "a" || strings.Join(rules[0].Targets, ",") != "b,C" || rules[1].Source != "d" {
		t.Fatalf("rules=%+v err=%v", rules, err)
	}
	if FormatModelFallbacks(rules) != "a=b|C,d=e" {
		t.Fatalf("规范写法 = %q", FormatModelFallbacks(rules))
	}
	for _, value := range []string{"a=A", "a=b|B", "a=b,A=c"} {
		if _, err := ParseModelFallbacks(value); err == nil {
			t.Fatalf("%q 应解析失败（比较不分大小写）", value)
		}
	}
	if rules, err := ParseModelFallbacks(" , \n"); err != nil || len(rules) != 0 {
		t.Fatalf("只有分隔符时应没有规则：%+v %v", rules, err)
	}
}
