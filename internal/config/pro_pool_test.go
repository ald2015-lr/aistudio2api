package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestProPoolConfigLoad PRO_POOL 默认开启：空值沿用默认、true/false 照写，写法无效时记入无效项、保持开启且不阻止启动；
// PRO_WARM_WORKER_LIMIT（默认 2，可以为 0）与 PRO_MAX_ACTIVE_WORKERS（默认 5，至少 1 且不小于常驻数）的解析与联动校验；
// 进程环境变量优先于 .env；configKeys 包含这三个键
func TestProPoolConfigLoad(t *testing.T) {
	clearConfigEnv(t)
	defaults := Default()
	if !defaults.ProPool || defaults.ProWarmWorkerLimit != 2 || defaults.ProMaxActiveWorkers != 5 {
		t.Fatalf("默认值 = %t/%d/%d，期望 true/2/5", defaults.ProPool, defaults.ProWarmWorkerLimit, defaults.ProMaxActiveWorkers)
	}
	for _, key := range []string{"PRO_POOL", "PRO_WARM_WORKER_LIMIT", "PRO_MAX_ACTIVE_WORKERS"} {
		if !slices.Contains(configKeys[:], key) {
			t.Fatalf("configKeys 缺少 %s：Save 会丢掉这个键，进程环境变量也覆盖不了它", key)
		}
		if fatalConfigKeys[key] {
			t.Fatalf("%s 无效时不应阻止启动", key)
		}
	}
	for _, test := range []struct {
		content string
		pool    bool
		warm    int
		max     int
		invalid string
		reason  string
	}{
		{content: "", pool: true, warm: 2, max: 5},
		{content: "PRO_POOL=\nPRO_WARM_WORKER_LIMIT=\nPRO_MAX_ACTIVE_WORKERS=\n", pool: true, warm: 2, max: 5},
		{content: "PRO_POOL=false\n", pool: false, warm: 2, max: 5},
		{content: "PRO_POOL= TRUE \n", pool: true, warm: 2, max: 5},
		{content: "PRO_WARM_WORKER_LIMIT=0\nPRO_MAX_ACTIVE_WORKERS=1\n", pool: true, warm: 0, max: 1},
		{content: "PRO_WARM_WORKER_LIMIT=10\nPRO_MAX_ACTIVE_WORKERS=30\n", pool: true, warm: 10, max: 30},
		{content: "PRO_POOL=off\n", invalid: "PRO_POOL", reason: "PRO_POOL 必须是 true 或 false"},
		{content: "PRO_WARM_WORKER_LIMIT=-1\n", invalid: "PRO_WARM_WORKER_LIMIT"},
		{content: "PRO_MAX_ACTIVE_WORKERS=0\n", invalid: "PRO_MAX_ACTIVE_WORKERS"},
		{content: "PRO_WARM_WORKER_LIMIT=6\n", invalid: "PRO_WARM_WORKER_LIMIT", reason: "PRO_MAX_ACTIVE_WORKERS 必须大于或等于 PRO_WARM_WORKER_LIMIT"},
		{content: "PRO_WARM_WORKER_LIMIT=3\nPRO_MAX_ACTIVE_WORKERS=2\n", invalid: "PRO_MAX_ACTIVE_WORKERS", reason: "PRO_MAX_ACTIVE_WORKERS 必须大于或等于 PRO_WARM_WORKER_LIMIT"},
	} {
		path := filepath.Join(t.TempDir(), ".env")
		if err := os.WriteFile(path, []byte(test.content), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err := Load(path)
		if test.invalid != "" {
			item := requireInvalidKey(t, test.content, cfg, err, test.invalid)
			if test.reason != "" && item.Reason != test.reason {
				t.Fatalf("%q：无效原因 %q，期望 %q", test.content, item.Reason, test.reason)
			}
			if !cfg.ProPool || cfg.ProWarmWorkerLimit != 2 || cfg.ProMaxActiveWorkers != 5 {
				t.Fatalf("%q：应改回默认 true/2/5，得到 %t/%d/%d", test.content, cfg.ProPool, cfg.ProWarmWorkerLimit, cfg.ProMaxActiveWorkers)
			}
			continue
		}
		if err != nil || cfg.ProPool != test.pool || cfg.ProWarmWorkerLimit != test.warm || cfg.ProMaxActiveWorkers != test.max {
			t.Fatalf("%q：得到 %t/%d/%d（%v），期望 %t/%d/%d", test.content,
				cfg.ProPool, cfg.ProWarmWorkerLimit, cfg.ProMaxActiveWorkers, err, test.pool, test.warm, test.max)
		}
		// 与 Ultra 号池的设置互不影响
		if !cfg.UltraExclusive || cfg.UltraIncludePro || cfg.UltraWarmWorkerLimit != 2 || cfg.UltraMaxActiveWorkers != 5 {
			t.Fatalf("%q：Ultra 设置不应受影响", test.content)
		}
	}
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte("PRO_POOL=true\nPRO_MAX_ACTIVE_WORKERS=7\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PRO_POOL", "false")
	t.Setenv("PRO_MAX_ACTIVE_WORKERS", "9")
	if cfg, err := Load(path); err != nil || cfg.ProPool || cfg.ProMaxActiveWorkers != 9 {
		t.Fatalf("环境变量应覆盖 .env：%t/%d %v", cfg.ProPool, cfg.ProMaxActiveWorkers, err)
	}
}

// TestProPoolRoundTrip 保存写出三个键（PRO_POOL=false 不会因漏写而读回默认开启）；JSON 往返保留它们；旧版 JSON 没有这些字段时
// 按默认（开启、2、5）处理，显式的 false 与 0 保留
func TestProPoolRoundTrip(t *testing.T) {
	clearConfigEnv(t)
	cfg := Default()
	cfg.ProPool, cfg.ProWarmWorkerLimit, cfg.ProMaxActiveWorkers = false, 0, 7
	path := filepath.Join(t.TempDir(), ".env")
	if err := cfg.Save(path); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{"PRO_POOL=false\n", "PRO_WARM_WORKER_LIMIT=0\n", "PRO_MAX_ACTIVE_WORKERS=7\n"} {
		if !strings.Contains(string(data), line) {
			t.Fatalf(".env 缺少 %q:\n%s", line, data)
		}
	}
	if loaded, err := Load(path); err != nil || loaded.ProPool || loaded.ProWarmWorkerLimit != 0 || loaded.ProMaxActiveWorkers != 7 {
		t.Fatalf(".env 往返 = %t/%d/%d err=%v", loaded.ProPool, loaded.ProWarmWorkerLimit, loaded.ProMaxActiveWorkers, err)
	}
	encoded, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{`"pro_pool":false`, `"pro_warm_worker_limit":0`, `"pro_max_active_workers":7`} {
		if !strings.Contains(string(encoded), field) {
			t.Fatalf("JSON 缺少 %s: %s", field, encoded)
		}
	}
	var decoded Config
	if err := json.Unmarshal(encoded, &decoded); err != nil || decoded.ProPool || decoded.ProWarmWorkerLimit != 0 || decoded.ProMaxActiveWorkers != 7 {
		t.Fatalf("JSON 往返 = %t/%d/%d err=%v", decoded.ProPool, decoded.ProWarmWorkerLimit, decoded.ProMaxActiveWorkers, err)
	}
	var legacy map[string]any
	if err := json.Unmarshal(encoded, &legacy); err != nil {
		t.Fatal(err)
	}
	delete(legacy, "pro_pool")
	delete(legacy, "pro_warm_worker_limit")
	delete(legacy, "pro_max_active_workers")
	legacyData, _ := json.Marshal(legacy)
	if err := json.Unmarshal(legacyData, &decoded); err != nil || !decoded.ProPool || decoded.ProWarmWorkerLimit != 2 || decoded.ProMaxActiveWorkers != 5 {
		t.Fatalf("旧版 JSON = %t/%d/%d err=%v，期望默认 true/2/5", decoded.ProPool, decoded.ProWarmWorkerLimit, decoded.ProMaxActiveWorkers, err)
	}
	// 无效值改回默认：resetKey 覆盖两个数量键
	reset := cfg
	reset.resetKey("PRO_WARM_WORKER_LIMIT")
	reset.resetKey("PRO_MAX_ACTIVE_WORKERS")
	if reset.ProWarmWorkerLimit != 2 || reset.ProMaxActiveWorkers != 5 {
		t.Fatalf("resetKey 后 = %d/%d，期望 2/5", reset.ProWarmWorkerLimit, reset.ProMaxActiveWorkers)
	}
}

// TestKeyConfigured 升级提示判断 PRO_POOL 是否写过：.env 或进程环境变量里有非空值才算，空值与没有这一项都不算
func TestKeyConfigured(t *testing.T) {
	clearConfigEnv(t)
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	if KeyConfigured(path, "PRO_POOL") {
		t.Fatal("没有 .env 时不应算写过")
	}
	for _, test := range []struct {
		content string
		want    bool
	}{
		{content: "PROXY=\n", want: false},
		{content: "PRO_POOL=\n", want: false},
		{content: "PRO_POOL=true\n", want: true},
		{content: "PRO_POOL=false\n", want: true},
	} {
		if err := os.WriteFile(path, []byte(test.content), 0o600); err != nil {
			t.Fatal(err)
		}
		if got := KeyConfigured(path, "PRO_POOL"); got != test.want {
			t.Fatalf("%q：KeyConfigured = %t，期望 %t", test.content, got, test.want)
		}
	}
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PRO_POOL", "true")
	if !KeyConfigured(path, "PRO_POOL") {
		t.Fatal("进程环境变量写了 PRO_POOL 时应算写过")
	}
	t.Setenv("PRO_POOL", " ")
	if KeyConfigured(path, "PRO_POOL") {
		t.Fatal("进程环境变量的空值不算写过")
	}
}

// TestSaveKeepingUnset unset 里的键在现有文件里没有（或为空）时不写这一行：仍按默认值生效、KeyConfigured 仍为假（PRO_POOL 默认开启的升级提示
// 不因保存其他设置消失）；文件里写过时照常写新值。其他键与 Save 写的相同
func TestSaveKeepingUnset(t *testing.T) {
	clearConfigEnv(t)
	path := filepath.Join(t.TempDir(), ".env")
	cfg := Default()
	if err := cfg.SaveKeepingUnset(path, nil, []string{"PRO_POOL"}); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(content), "PRO_POOL=") || !strings.Contains(string(content), "PRO_WARM_WORKER_LIMIT=2\n") || KeyConfigured(path, "PRO_POOL") {
		t.Fatalf("没有写过 PRO_POOL 时不应写出这一行：\n%s", content)
	}
	loaded, err := Load(path)
	if err != nil || !loaded.ProPool {
		t.Fatalf("没写 PRO_POOL 时按默认开启：%t（%v）", loaded.ProPool, err)
	}
	full := filepath.Join(t.TempDir(), ".env")
	if err := cfg.Save(full); err != nil {
		t.Fatal(err)
	}
	fullContent, err := os.ReadFile(full)
	if err != nil {
		t.Fatal(err)
	}
	if strings.ReplaceAll(string(fullContent), "PRO_POOL=true\n", "") != string(content) {
		t.Fatal("除 PRO_POOL 外应与 Save 写的相同")
	}
	// 文件里写过（即使与默认值相同）：照常写新值
	cfg.ProPool = false
	if err := cfg.SaveKeepingUnset(full, nil, []string{"PRO_POOL"}); err != nil {
		t.Fatal(err)
	}
	if loaded, err := Load(full); err != nil || loaded.ProPool || !KeyConfigured(full, "PRO_POOL") {
		t.Fatalf("写过 PRO_POOL 时应照常写新值：%t（%v）", loaded.ProPool, err)
	}
}
