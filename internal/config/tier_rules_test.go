package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestTierRuleModelsLoad 权益规则：默认 ULTRA_ONLY_MODELS=gemini-4-carbon、PAID_ONLY_MODELS=gemini-3.1-pro-preview；键不存在时保持默认，
// 写空值表示没有规则；列表按其他模型列表整理（去空白、models/ 前缀、重复）；long- 开头的名称无效，这一项按默认值启动并记入无效项
// （另一项不受影响）；进程环境变量优先于 .env，但空值不生效
func TestTierRuleModelsLoad(t *testing.T) {
	clearConfigEnv(t)
	defaults := Default()
	if !slices.Equal(defaults.UltraOnlyModels, []string{"gemini-4-carbon"}) || !slices.Equal(defaults.PaidOnlyModels, []string{"gemini-3.1-pro-preview"}) {
		t.Fatalf("默认值 = %v / %v", defaults.UltraOnlyModels, defaults.PaidOnlyModels)
	}
	defaults.UltraOnlyModels[0], defaults.PaidOnlyModels[0] = "changed", "changed"
	if again := Default(); again.UltraOnlyModels[0] != "gemini-4-carbon" || again.PaidOnlyModels[0] != "gemini-3.1-pro-preview" {
		t.Fatal("Default() 应返回默认列表的副本")
	}
	for _, key := range []string{"ULTRA_ONLY_MODELS", "PAID_ONLY_MODELS"} {
		if !slices.Contains(configKeys[:], key) {
			t.Fatalf("configKeys 缺少 %s：Save 会丢掉这个键，进程环境变量也覆盖不了它", key)
		}
	}
	for _, test := range []struct {
		content          string
		ultraOnly, paid  []string
		invalidUltraOnly bool
		invalidPaid      bool
	}{
		{content: "", ultraOnly: []string{"gemini-4-carbon"}, paid: []string{"gemini-3.1-pro-preview"}},
		{content: "ULTRA_ONLY_MODELS=\nPAID_ONLY_MODELS=\n", ultraOnly: nil, paid: nil},
		{content: "ULTRA_ONLY_MODELS=\" models/model-a, model-b，model-a \"\n", ultraOnly: []string{"model-a", "model-b"}, paid: []string{"gemini-3.1-pro-preview"}},
		{content: "PAID_ONLY_MODELS=model-p\n", ultraOnly: []string{"gemini-4-carbon"}, paid: []string{"model-p"}},
		{content: "ULTRA_ONLY_MODELS=long-gemini-4-carbon,model-a\nPAID_ONLY_MODELS=model-p\n",
			ultraOnly: []string{"gemini-4-carbon"}, paid: []string{"model-p"}, invalidUltraOnly: true},
		{content: "ULTRA_ONLY_MODELS=model-a\nPAID_ONLY_MODELS=LONG-gemini-3.1-pro-preview\n",
			ultraOnly: []string{"model-a"}, paid: []string{"gemini-3.1-pro-preview"}, invalidPaid: true},
	} {
		cfg, err := Load(writeEnv(t, test.content))
		if test.invalidUltraOnly {
			item := requireInvalidKey(t, test.content, cfg, err, "ULTRA_ONLY_MODELS")
			if !strings.Contains(item.Reason, "long-") || item.Value != "long-gemini-4-carbon,model-a" {
				t.Fatalf("%q：无效项 %+v", test.content, item)
			}
		}
		if test.invalidPaid {
			requireInvalidKey(t, test.content, cfg, err, "PAID_ONLY_MODELS")
		}
		if !test.invalidUltraOnly && !test.invalidPaid && (err != nil || len(cfg.Invalid) != 0) {
			t.Fatalf("%q：err=%v 无效项 %+v", test.content, err, cfg.Invalid)
		}
		if !slices.Equal(cfg.UltraOnlyModels, test.ultraOnly) || !slices.Equal(cfg.PaidOnlyModels, test.paid) {
			t.Fatalf("%q：得到 %v / %v，期望 %v / %v", test.content, cfg.UltraOnlyModels, cfg.PaidOnlyModels, test.ultraOnly, test.paid)
		}
	}

	path := writeEnv(t, "ULTRA_ONLY_MODELS=model-a\nPAID_ONLY_MODELS=\n")
	t.Setenv("ULTRA_ONLY_MODELS", "model-env")
	t.Setenv("PAID_ONLY_MODELS", "model-env-paid")
	if cfg, err := Load(path); err != nil || !slices.Equal(cfg.UltraOnlyModels, []string{"model-env"}) || !slices.Equal(cfg.PaidOnlyModels, []string{"model-env-paid"}) {
		t.Fatalf("环境变量应覆盖 .env：%v / %v err=%v", cfg.UltraOnlyModels, cfg.PaidOnlyModels, err)
	}
	t.Setenv("ULTRA_ONLY_MODELS", "")
	t.Setenv("PAID_ONLY_MODELS", " ")
	if cfg, err := Load(path); err != nil || !slices.Equal(cfg.UltraOnlyModels, []string{"model-a"}) || len(cfg.PaidOnlyModels) != 0 {
		t.Fatalf("空环境变量不应覆盖 .env：%v / %v err=%v", cfg.UltraOnlyModels, cfg.PaidOnlyModels, err)
	}
	t.Setenv("ULTRA_ONLY_MODELS", "long-model-env")
	cfg, err := Load(path)
	if item := requireInvalidKey(t, "环境变量", cfg, err, "ULTRA_ONLY_MODELS"); !item.Environment || !slices.Equal(cfg.UltraOnlyModels, []string{"gemini-4-carbon"}) {
		t.Fatalf("环境变量里的无效值：%+v，生效 %v", item, cfg.UltraOnlyModels)
	}
}

// TestTierRuleModelsValidate 管理页保存时按 Validate 拒绝 long- 开头的名称与其他模型列表的无效写法，并指明配置键
func TestTierRuleModelsValidate(t *testing.T) {
	for _, test := range []struct {
		key   string
		apply func(*Config, []string)
	}{
		{key: "ULTRA_ONLY_MODELS", apply: func(cfg *Config, models []string) { cfg.UltraOnlyModels = models }},
		{key: "PAID_ONLY_MODELS", apply: func(cfg *Config, models []string) { cfg.PaidOnlyModels = models }},
	} {
		for _, models := range [][]string{{"long-gemini-4-carbon"}, {"Long-model"}, {"model a"}, {"model-a", "model-a"}, {""}} {
			cfg := Default()
			test.apply(&cfg, models)
			if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), test.key) {
				t.Fatalf("%s=%q 应校验失败并指明配置键，实际 %v", test.key, models, err)
			}
		}
		cfg := Default()
		test.apply(&cfg, []string{"gemini-4-carbon-high", "longcat-model"})
		if err := cfg.Validate(); err != nil {
			t.Fatalf("%s：带后缀的名称在配置里认不出来（运行时提示），long 不带连字符的名称也应通过：%v", test.key, err)
		}
	}
}

// TestTierRuleModelsRoundTrip 保存后再读取、JSON 往返都保留列表；清空后保存为空值，再读取仍为没有规则（不回到默认）；
// 旧版 JSON 没有这些字段（或为 null）时取默认列表
func TestTierRuleModelsRoundTrip(t *testing.T) {
	clearConfigEnv(t)
	path := filepath.Join(t.TempDir(), ".env")
	cfg := Default()
	cfg.UltraOnlyModels = []string{"model-a", "model-b"}
	cfg.PaidOnlyModels = []string{"model-p"}
	if err := cfg.Save(path); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path)
	if err != nil || !slices.Equal(loaded.UltraOnlyModels, cfg.UltraOnlyModels) || !slices.Equal(loaded.PaidOnlyModels, cfg.PaidOnlyModels) {
		t.Fatalf(".env 往返 = %v / %v err=%v", loaded.UltraOnlyModels, loaded.PaidOnlyModels, err)
	}
	cleared := cfg
	cleared.UltraOnlyModels, cleared.PaidOnlyModels = nil, nil
	if err := cleared.Save(path); err != nil {
		t.Fatal(err)
	}
	content, _ := os.ReadFile(path)
	if !strings.Contains(string(content), "\nULTRA_ONLY_MODELS=\nPAID_ONLY_MODELS=\n") {
		t.Fatalf("清空后 .env 应写空值：\n%s", content)
	}
	if loaded, err = Load(path); err != nil || len(loaded.UltraOnlyModels) != 0 || len(loaded.PaidOnlyModels) != 0 {
		t.Fatalf("清空后 .env 往返 = %v / %v err=%v，期望没有规则", loaded.UltraOnlyModels, loaded.PaidOnlyModels, err)
	}
	if err := Default().Save(path); err != nil {
		t.Fatal(err)
	}
	if content, _ = os.ReadFile(path); !strings.Contains(string(content), "\nULTRA_ONLY_MODELS=gemini-4-carbon\nPAID_ONLY_MODELS=gemini-3.1-pro-preview\n") {
		t.Fatalf("默认配置的 .env：\n%s", content)
	}
	cfg.UltraOnlyModels = []string{"long-model"}
	if err := cfg.Save(path); err == nil || !strings.Contains(err.Error(), "ULTRA_ONLY_MODELS") {
		t.Fatalf("保存 long- 开头的名称应失败：%v", err)
	}

	for _, value := range []Config{cleared, func() Config { c := Default(); c.UltraOnlyModels = []string{"model-a"}; return c }()} {
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		var decoded Config
		if err := json.Unmarshal(data, &decoded); err != nil || !slices.Equal(decoded.UltraOnlyModels, value.UltraOnlyModels) ||
			!slices.Equal(decoded.PaidOnlyModels, value.PaidOnlyModels) {
			t.Fatalf("JSON 往返 = %v / %v err=%v，期望 %v / %v", decoded.UltraOnlyModels, decoded.PaidOnlyModels, err, value.UltraOnlyModels, value.PaidOnlyModels)
		}
	}
	legacy, _ := json.Marshal(Default())
	var fields map[string]any
	_ = json.Unmarshal(legacy, &fields)
	delete(fields, "ultra_only_models")
	fields["paid_only_models"] = nil
	legacy, _ = json.Marshal(fields)
	var decoded Config
	if err := json.Unmarshal(legacy, &decoded); err != nil || !slices.Equal(decoded.UltraOnlyModels, []string{"gemini-4-carbon"}) ||
		!slices.Equal(decoded.PaidOnlyModels, []string{"gemini-3.1-pro-preview"}) {
		t.Fatalf("旧版 JSON = %v / %v err=%v，期望默认列表", decoded.UltraOnlyModels, decoded.PaidOnlyModels, err)
	}
}
