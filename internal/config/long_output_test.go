package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestLongOutputConfig 自动续写设置（LONG_OUTPUT_ENABLED、LONG_OUTPUT_MAX_TOKENS）：默认开启、默认总上限 262144，可显式关闭、写 0 到 1048576；
// 取值无效时与其他非网络形态的键一样按默认值加载并记入无效项（不让整个服务起不来）；管理页保存时 Validate 照旧拒绝越界值；
// .env 与管理接口的 JSON 往返不变，旧版 JSON 没有这两个字段时按默认值；进程环境变量优先于 .env
func TestLongOutputConfig(t *testing.T) {
	clearConfigEnv(t)
	defaults := Default()
	if !defaults.LongOutputEnabled || defaults.LongOutputMaxTokens != 262144 {
		t.Fatalf("默认值 = %v/%d，期望 true/262144", defaults.LongOutputEnabled, defaults.LongOutputMaxTokens)
	}
	for _, key := range []string{"LONG_OUTPUT_ENABLED", "LONG_OUTPUT_MAX_TOKENS"} {
		if !slices.Contains(configKeys[:], key) {
			t.Fatalf("configKeys 缺少 %s：Save 会丢掉这个键，进程环境变量也覆盖不了它", key)
		}
		if fatalConfigKeys[key] {
			t.Fatalf("%s 不决定网络形态与发送路径，取值无效时应按默认值启动", key)
		}
	}
	for _, test := range []struct {
		content string
		enabled bool
		tokens  int
		invalid string
	}{
		{content: "", enabled: true, tokens: 262144},
		{content: "LONG_OUTPUT_ENABLED=\nLONG_OUTPUT_MAX_TOKENS=\n", enabled: true, tokens: 262144},
		{content: "LONG_OUTPUT_ENABLED=false\nLONG_OUTPUT_MAX_TOKENS=0\n", enabled: false, tokens: 0},
		{content: "LONG_OUTPUT_MAX_TOKENS=1048576\n", enabled: true, tokens: 1048576},
		{content: "LONG_OUTPUT_ENABLED=maybe\n", enabled: true, tokens: 262144, invalid: "LONG_OUTPUT_ENABLED"},
		{content: "LONG_OUTPUT_MAX_TOKENS=abc\n", enabled: true, tokens: 262144, invalid: "LONG_OUTPUT_MAX_TOKENS"},
		{content: "LONG_OUTPUT_MAX_TOKENS=-1\n", enabled: true, tokens: 262144, invalid: "LONG_OUTPUT_MAX_TOKENS"},
		{content: "LONG_OUTPUT_MAX_TOKENS=2000000\nLONG_OUTPUT_ENABLED=false\n", enabled: false, tokens: 262144, invalid: "LONG_OUTPUT_MAX_TOKENS"},
	} {
		cfg, err := Load(writeEnv(t, test.content))
		if test.invalid != "" {
			item := requireInvalidKey(t, test.content, cfg, err, test.invalid)
			if item.Value == "" || !strings.Contains(item.Reason, test.invalid) {
				t.Fatalf("%q：无效项应记原值与原因：%+v", test.content, item)
			}
		} else if err != nil || len(cfg.Invalid) != 0 {
			t.Fatalf("%q：err=%v 无效项=%+v", test.content, err, cfg.Invalid)
		}
		if cfg.LongOutputEnabled != test.enabled || cfg.LongOutputMaxTokens != test.tokens {
			t.Fatalf("%q：得到 %v/%d，期望 %v/%d", test.content, cfg.LongOutputEnabled, cfg.LongOutputMaxTokens, test.enabled, test.tokens)
		}
	}
	// 管理页保存走 Validate：越界值照旧拒绝，不改用默认值
	for _, tokens := range []int{-1, 1048577} {
		cfg := Default()
		cfg.LongOutputMaxTokens = tokens
		if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "LONG_OUTPUT_MAX_TOKENS") {
			t.Fatalf("LONG_OUTPUT_MAX_TOKENS=%d 保存时应被拒绝：%v", tokens, err)
		}
	}
	// 进程环境变量优先于 .env，环境变量里的无效值标明来源
	path := writeEnv(t, "LONG_OUTPUT_ENABLED=true\nLONG_OUTPUT_MAX_TOKENS=300000\n")
	t.Setenv("LONG_OUTPUT_ENABLED", "false")
	t.Setenv("LONG_OUTPUT_MAX_TOKENS", "lots")
	cfg, err := Load(path)
	item := requireInvalidKey(t, "环境变量", cfg, err, "LONG_OUTPUT_MAX_TOKENS")
	if cfg.LongOutputEnabled || cfg.LongOutputMaxTokens != 262144 || !item.Environment {
		t.Fatalf("环境变量应覆盖 .env：%v/%d 无效项=%+v", cfg.LongOutputEnabled, cfg.LongOutputMaxTokens, item)
	}
	os.Unsetenv("LONG_OUTPUT_ENABLED")
	os.Unsetenv("LONG_OUTPUT_MAX_TOKENS")
	// .env 往返
	saved := Default()
	saved.LongOutputEnabled, saved.LongOutputMaxTokens = false, 100000
	path = filepath.Join(t.TempDir(), ".env")
	if err := saved.Save(path); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if !containsLine(string(data), "LONG_OUTPUT_ENABLED=false") || !containsLine(string(data), "LONG_OUTPUT_MAX_TOKENS=100000") {
		t.Fatalf(".env 缺少自动续写设置：\n%s", data)
	}
	if loaded, err := Load(path); err != nil || loaded.LongOutputEnabled || loaded.LongOutputMaxTokens != 100000 {
		t.Fatalf("保存后读回 %v/%d err=%v", loaded.LongOutputEnabled, loaded.LongOutputMaxTokens, err)
	}
	// 管理接口 JSON 往返与旧版 JSON
	encoded, err := json.Marshal(saved)
	if err != nil {
		t.Fatal(err)
	}
	var decoded Config
	if err := json.Unmarshal(encoded, &decoded); err != nil || decoded.LongOutputEnabled || decoded.LongOutputMaxTokens != 100000 {
		t.Fatalf("JSON 往返 %v/%d err=%v", decoded.LongOutputEnabled, decoded.LongOutputMaxTokens, err)
	}
	var legacy map[string]any
	_ = json.Unmarshal(encoded, &legacy)
	delete(legacy, "long_output_enabled")
	delete(legacy, "long_output_max_tokens")
	legacyJSON, _ := json.Marshal(legacy)
	if err := json.Unmarshal(legacyJSON, &decoded); err != nil || !decoded.LongOutputEnabled || decoded.LongOutputMaxTokens != 262144 {
		t.Fatalf("旧版 JSON 没有这两个字段时应按默认值：%v/%d err=%v", decoded.LongOutputEnabled, decoded.LongOutputMaxTokens, err)
	}
}

// TestInvalidMergedKeysStartWithDefaults 几批合并进来的布尔与整数项（STREAM_DATA_HEARTBEAT、OMNI_IGNORE_SYSTEM_PROMPT、LONG_OUTPUT_*）
// 同在一份 .env 里写错时一起按默认值启动并逐项记入无效项，其余有效项照常生效
func TestInvalidMergedKeysStartWithDefaults(t *testing.T) {
	clearConfigEnv(t)
	content := strings.Join([]string{
		"STREAM_DATA_HEARTBEAT=off",
		"OMNI_IGNORE_SYSTEM_PROMPT=nope",
		"LONG_OUTPUT_ENABLED=sometimes",
		"LONG_OUTPUT_MAX_TOKENS=9999999",
		"MIN_OUTPUT_TOKENS=1234",
		"",
	}, "\n")
	cfg, err := Load(writeEnv(t, content))
	if err != nil {
		t.Fatalf("取值无效应按默认值启动：%v", err)
	}
	for _, key := range []string{"STREAM_DATA_HEARTBEAT", "OMNI_IGNORE_SYSTEM_PROMPT", "LONG_OUTPUT_ENABLED", "LONG_OUTPUT_MAX_TOKENS"} {
		requireInvalidKey(t, content, cfg, nil, key)
	}
	if !cfg.StreamDataHeartbeat || !cfg.OmniIgnoreSystemPrompt || !cfg.LongOutputEnabled || cfg.LongOutputMaxTokens != 262144 || cfg.MinOutputTokens != 1234 {
		t.Fatalf("无效项应取默认值、有效项照常生效：%+v", cfg)
	}
	if len(cfg.Invalid) != 4 {
		t.Fatalf("无效项 = %+v，期望 4 项", cfg.Invalid)
	}
}
