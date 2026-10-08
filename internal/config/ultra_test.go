package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func clearConfigEnv(t *testing.T) {
	t.Helper()
	for _, key := range configKeys {
		t.Setenv(key, "")
		os.Unsetenv(key)
	}
}

// TestUltraConfigLoad Ultra 号池设置的默认值、解析与校验：默认独占、常驻 2、峰值 5；常驻可以为 0，
// 峰值至少为 1 且不小于常驻数；空值沿用默认
func TestUltraConfigLoad(t *testing.T) {
	clearConfigEnv(t)
	defaults := Default()
	if !defaults.UltraExclusive || defaults.UltraWarmWorkerLimit != 2 || defaults.UltraMaxActiveWorkers != 5 {
		t.Fatalf("默认值 = %t/%d/%d，期望 true/2/5", defaults.UltraExclusive, defaults.UltraWarmWorkerLimit, defaults.UltraMaxActiveWorkers)
	}
	for _, test := range []struct {
		content   string
		exclusive bool
		warm      int
		max       int
		invalid   string
	}{
		{content: "", exclusive: true, warm: 2, max: 5},
		{content: "ULTRA_EXCLUSIVE=\nULTRA_WARM_WORKER_LIMIT=\nULTRA_MAX_ACTIVE_WORKERS=\n", exclusive: true, warm: 2, max: 5},
		{content: "ULTRA_EXCLUSIVE=false\n", exclusive: false, warm: 2, max: 5},
		{content: "ULTRA_WARM_WORKER_LIMIT=0\n", exclusive: true, warm: 0, max: 5},
		{content: "ULTRA_WARM_WORKER_LIMIT=10\nULTRA_MAX_ACTIVE_WORKERS=20\n", exclusive: true, warm: 10, max: 20},
		{content: "ULTRA_WARM_WORKER_LIMIT=0\nULTRA_MAX_ACTIVE_WORKERS=1\n", exclusive: true, warm: 0, max: 1},
		{content: "ULTRA_EXCLUSIVE=maybe\n", invalid: "ULTRA_EXCLUSIVE"},
		{content: "ULTRA_WARM_WORKER_LIMIT=-1\n", invalid: "ULTRA_WARM_WORKER_LIMIT"},
		{content: "ULTRA_WARM_WORKER_LIMIT=abc\n", invalid: "ULTRA_WARM_WORKER_LIMIT"},
		{content: "ULTRA_MAX_ACTIVE_WORKERS=0\n", invalid: "ULTRA_MAX_ACTIVE_WORKERS"},
		{content: "ULTRA_WARM_WORKER_LIMIT=6\n", invalid: "ULTRA_MAX_ACTIVE_WORKERS"},
		{content: "ULTRA_WARM_WORKER_LIMIT=3\nULTRA_MAX_ACTIVE_WORKERS=2\n", invalid: "ULTRA_MAX_ACTIVE_WORKERS"},
	} {
		path := filepath.Join(t.TempDir(), ".env")
		if err := os.WriteFile(path, []byte(test.content), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err := Load(path)
		if test.invalid != "" {
			if err == nil || !strings.Contains(err.Error(), test.invalid) {
				t.Fatalf("%q 应报 %s 错误，得到 %v", test.content, test.invalid, err)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%q：%v", test.content, err)
		}
		if cfg.UltraExclusive != test.exclusive || cfg.UltraWarmWorkerLimit != test.warm || cfg.UltraMaxActiveWorkers != test.max {
			t.Fatalf("%q：得到 %t/%d/%d，期望 %t/%d/%d", test.content,
				cfg.UltraExclusive, cfg.UltraWarmWorkerLimit, cfg.UltraMaxActiveWorkers, test.exclusive, test.warm, test.max)
		}
	}

	// 普通分区的设置照旧校验，与 Ultra 分区互不影响
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte("WARM_WORKER_LIMIT=1\nMAX_ACTIVE_WORKERS=1\nWARM_STARTUP_CONCURRENCY=1\nULTRA_WARM_WORKER_LIMIT=4\nULTRA_MAX_ACTIVE_WORKERS=8\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if cfg, err := Load(path); err != nil || cfg.MaxActiveWorkers != 1 || cfg.UltraMaxActiveWorkers != 8 {
		t.Fatalf("两个分区分别设置失败: %+v %v", cfg, err)
	}

	// 进程环境变量优先于 .env
	t.Setenv("ULTRA_EXCLUSIVE", "false")
	if cfg, err := Load(path); err != nil || cfg.UltraExclusive {
		t.Fatalf("环境变量 ULTRA_EXCLUSIVE=false 应生效: %t %v", cfg.UltraExclusive, err)
	}
}

// TestUltraConfigRoundTrip 保存后再读取、JSON 往返都保留 Ultra 设置；旧版 JSON 没有这些字段时按默认值
func TestUltraConfigRoundTrip(t *testing.T) {
	clearConfigEnv(t)
	cfg := Default()
	cfg.UltraExclusive = false
	cfg.UltraWarmWorkerLimit = 0
	cfg.UltraMaxActiveWorkers = 20
	path := filepath.Join(t.TempDir(), ".env")
	if err := cfg.Save(path); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{"ULTRA_EXCLUSIVE=false\n", "ULTRA_WARM_WORKER_LIMIT=0\n", "ULTRA_MAX_ACTIVE_WORKERS=20\n"} {
		if !strings.Contains(string(data), line) {
			t.Fatalf(".env 缺少 %q:\n%s", line, data)
		}
	}
	loaded, err := Load(path)
	if err != nil || loaded.UltraExclusive || loaded.UltraWarmWorkerLimit != 0 || loaded.UltraMaxActiveWorkers != 20 {
		t.Fatalf(".env 往返 = %t/%d/%d err=%v", loaded.UltraExclusive, loaded.UltraWarmWorkerLimit, loaded.UltraMaxActiveWorkers, err)
	}

	encoded, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var decoded Config
	if err := json.Unmarshal(encoded, &decoded); err != nil || decoded.UltraExclusive || decoded.UltraWarmWorkerLimit != 0 || decoded.UltraMaxActiveWorkers != 20 {
		t.Fatalf("JSON 往返 = %t/%d/%d err=%v", decoded.UltraExclusive, decoded.UltraWarmWorkerLimit, decoded.UltraMaxActiveWorkers, err)
	}
	var legacy map[string]any
	if err := json.Unmarshal(encoded, &legacy); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"ultra_exclusive", "ultra_warm_worker_limit", "ultra_max_active_workers"} {
		delete(legacy, key)
	}
	legacyData, _ := json.Marshal(legacy)
	if err := json.Unmarshal(legacyData, &decoded); err != nil || !decoded.UltraExclusive || decoded.UltraWarmWorkerLimit != 2 || decoded.UltraMaxActiveWorkers != 5 {
		t.Fatalf("旧版 JSON = %t/%d/%d err=%v，期望默认值", decoded.UltraExclusive, decoded.UltraWarmWorkerLimit, decoded.UltraMaxActiveWorkers, err)
	}

	invalid := Default()
	invalid.UltraWarmWorkerLimit = 6
	if err := invalid.Save(filepath.Join(t.TempDir(), ".env")); err == nil {
		t.Fatal("Ultra 常驻数大于峰值数时不应保存")
	}
}
