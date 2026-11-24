package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// extraPoolKeys 为补充池的 6 个配置键
var extraPoolKeys = []string{
	"EXTRA_POOL", "EXTRA_POOL_MODE", "EXTRA_POOL_DEFAULT_PAID", "EXTRA_POOL_INCLUDE_ULTRA", "EXTRA_WARM_WORKER_LIMIT", "EXTRA_MAX_ACTIVE_WORKERS",
}

// extraValues 为补充池设置的一组取值（测试比较用）
type extraValues struct {
	pool, defaultPaid, includeUltra bool
	mode                            string
	warm, max                       int
}

func extraValuesOf(cfg Config) extraValues {
	return extraValues{
		pool: cfg.ExtraPool, mode: cfg.ExtraPoolMode, defaultPaid: cfg.ExtraPoolDefaultPaid, includeUltra: cfg.ExtraPoolIncludeUltra,
		warm: cfg.ExtraWarmWorkerLimit, max: cfg.ExtraMaxActiveWorkers,
	}
}

var extraDefaults = extraValues{mode: "share", defaultPaid: true, warm: 40, max: 50}

// TestExtraConfigDefaults 补充池默认关闭：方式 share、默认地址可借付费模型、不含 Ultra、常驻 40、峰值 50；6 个键都在 configKeys 里，
// 取值无效时都不阻止启动
func TestExtraConfigDefaults(t *testing.T) {
	clearConfigEnv(t)
	if got := extraValuesOf(Default()); got != extraDefaults {
		t.Fatalf("默认值 = %+v，期望 %+v", got, extraDefaults)
	}
	if err := Default().Validate(); err != nil {
		t.Fatalf("默认配置校验失败: %v", err)
	}
	for _, key := range extraPoolKeys {
		if !slices.Contains(configKeys[:], key) {
			t.Fatalf("configKeys 缺少 %s：Save 会丢掉这个键，进程环境变量也覆盖不了它", key)
		}
		if fatalConfigKeys[key] || secretConfigKeys[key] {
			t.Fatalf("%s 无效时不应阻止启动，也不是凭据键", key)
		}
	}
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil || extraValuesOf(cfg) != extraDefaults || len(cfg.Invalid) != 0 {
		t.Fatalf("空 .env：%+v 无效项=%v err=%v", extraValuesOf(cfg), cfg.Invalid, err)
	}
}

// TestExtraConfigLoad 补充池设置的解析与联动校验：空值沿用默认；方式不区分大小写、写回小写；写法无效、常驻为负、峰值为 0、
// 峰值小于常驻时记入无效项并改回默认值，不阻止启动；EXTRA_POOL=false 时数值照样校验；进程环境变量优先于 .env
func TestExtraConfigLoad(t *testing.T) {
	clearConfigEnv(t)
	for _, test := range []struct {
		content string
		want    extraValues
		invalid string
		reason  string
	}{
		{content: "", want: extraDefaults},
		{content: "EXTRA_POOL=\nEXTRA_POOL_MODE=\nEXTRA_POOL_DEFAULT_PAID=\nEXTRA_POOL_INCLUDE_ULTRA=\nEXTRA_WARM_WORKER_LIMIT=\nEXTRA_MAX_ACTIVE_WORKERS=\n", want: extraDefaults},
		{
			content: "EXTRA_POOL=true\nEXTRA_POOL_MODE= OVERFLOW \nEXTRA_POOL_DEFAULT_PAID=false\nEXTRA_POOL_INCLUDE_ULTRA=true\nEXTRA_WARM_WORKER_LIMIT=0\nEXTRA_MAX_ACTIVE_WORKERS=3\n",
			want:    extraValues{pool: true, mode: "overflow", includeUltra: true, warm: 0, max: 3},
		},
		{content: "EXTRA_POOL_MODE=Share\n", want: extraDefaults},
		{content: "EXTRA_POOL=on\n", invalid: "EXTRA_POOL", reason: "EXTRA_POOL 必须是 true 或 false", want: extraDefaults},
		{content: "EXTRA_POOL_DEFAULT_PAID=nope\n", invalid: "EXTRA_POOL_DEFAULT_PAID", want: extraDefaults},
		{content: "EXTRA_POOL_INCLUDE_ULTRA=2\n", invalid: "EXTRA_POOL_INCLUDE_ULTRA", want: extraDefaults},
		{content: "EXTRA_POOL=true\nEXTRA_POOL_MODE=both\n", invalid: "EXTRA_POOL_MODE", reason: "EXTRA_POOL_MODE 必须是 share 或 overflow",
			want: extraValues{pool: true, mode: "share", defaultPaid: true, warm: 40, max: 50}},
		{content: "EXTRA_WARM_WORKER_LIMIT=-1\n", invalid: "EXTRA_WARM_WORKER_LIMIT", want: extraDefaults},
		{content: "EXTRA_MAX_ACTIVE_WORKERS=0\n", invalid: "EXTRA_MAX_ACTIVE_WORKERS", want: extraDefaults},
		{content: "EXTRA_WARM_WORKER_LIMIT=60\n", invalid: "EXTRA_WARM_WORKER_LIMIT",
			reason: "EXTRA_MAX_ACTIVE_WORKERS 必须大于或等于 EXTRA_WARM_WORKER_LIMIT", want: extraDefaults},
		{content: "EXTRA_WARM_WORKER_LIMIT=5\nEXTRA_MAX_ACTIVE_WORKERS=4\n", invalid: "EXTRA_MAX_ACTIVE_WORKERS",
			reason: "EXTRA_MAX_ACTIVE_WORKERS 必须大于或等于 EXTRA_WARM_WORKER_LIMIT", want: extraDefaults},
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
		} else if err != nil || len(cfg.Invalid) != 0 {
			t.Fatalf("%q：err=%v 无效项=%v", test.content, err, cfg.Invalid)
		}
		if got := extraValuesOf(cfg); got != test.want {
			t.Fatalf("%q：得到 %+v，期望 %+v", test.content, got, test.want)
		}
		if err := cfg.Validate(); err != nil {
			t.Fatalf("%q：改回默认值后仍校验失败: %v", test.content, err)
		}
		// 其他分区的设置不受影响
		if cfg.ProWarmWorkerLimit != 2 || cfg.ProMaxActiveWorkers != 5 || cfg.WarmWorkerLimit != 5 || cfg.MaxActiveWorkers != 10 {
			t.Fatalf("%q：其他分区的设置不应受影响", test.content)
		}
	}
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte("EXTRA_POOL=false\nEXTRA_POOL_MODE=share\nEXTRA_MAX_ACTIVE_WORKERS=50\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("EXTRA_POOL", "true")
	t.Setenv("EXTRA_POOL_MODE", "Overflow")
	t.Setenv("EXTRA_MAX_ACTIVE_WORKERS", "45")
	cfg, err := Load(path)
	if err != nil || !cfg.ExtraPool || cfg.ExtraPoolMode != "overflow" || cfg.ExtraMaxActiveWorkers != 45 {
		t.Fatalf("环境变量应覆盖 .env：%+v err=%v", extraValuesOf(cfg), err)
	}
	t.Setenv("EXTRA_WARM_WORKER_LIMIT", "46")
	if cfg, err = Load(path); err != nil {
		t.Fatal(err)
	}
	if item := requireInvalidKey(t, "环境变量", cfg, err, "EXTRA_WARM_WORKER_LIMIT"); !item.Environment {
		t.Fatalf("环境变量里的无效值应标明来源: %+v", item)
	}
}

// TestExtraConfigRoundTrip 保存写出 6 个键（关闭与 false、0 都写明，方式写小写）；.env 与 JSON 往返保留它们；旧版 JSON 没有这些字段时
// 按默认处理（关闭、share、默认地址可借付费模型、不含 Ultra、40、50），缺少 extra_pool_default_paid 时为真；resetKey 覆盖每个键
func TestExtraConfigRoundTrip(t *testing.T) {
	clearConfigEnv(t)
	cfg := Default()
	cfg.ExtraPool, cfg.ExtraPoolMode, cfg.ExtraPoolDefaultPaid, cfg.ExtraPoolIncludeUltra = true, "OVERFLOW", false, true
	cfg.ExtraWarmWorkerLimit, cfg.ExtraMaxActiveWorkers = 0, 7
	path := filepath.Join(t.TempDir(), ".env")
	if err := cfg.Save(path); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{
		"EXTRA_POOL=true\n", "EXTRA_POOL_MODE=overflow\n", "EXTRA_POOL_DEFAULT_PAID=false\n", "EXTRA_POOL_INCLUDE_ULTRA=true\n",
		"EXTRA_WARM_WORKER_LIMIT=0\n", "EXTRA_MAX_ACTIVE_WORKERS=7\n",
	} {
		if !strings.Contains(string(data), line) {
			t.Fatalf(".env 缺少 %q:\n%s", line, data)
		}
	}
	want := extraValues{pool: true, mode: "overflow", includeUltra: true, warm: 0, max: 7}
	if loaded, err := Load(path); err != nil || extraValuesOf(loaded) != want {
		t.Fatalf(".env 往返 = %+v err=%v", extraValuesOf(loaded), err)
	}
	encoded, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{
		`"extra_pool":true`, `"extra_pool_mode":"overflow"`, `"extra_pool_default_paid":false`, `"extra_pool_include_ultra":true`,
		`"extra_warm_worker_limit":0`, `"extra_max_active_workers":7`,
	} {
		if !strings.Contains(string(encoded), field) {
			t.Fatalf("JSON 缺少 %s: %s", field, encoded)
		}
	}
	var decoded Config
	if err := json.Unmarshal(encoded, &decoded); err != nil || extraValuesOf(decoded) != want {
		t.Fatalf("JSON 往返 = %+v err=%v", extraValuesOf(decoded), err)
	}
	var legacy map[string]any
	if err := json.Unmarshal(encoded, &legacy); err != nil {
		t.Fatal(err)
	}
	for _, key := range extraPoolKeys {
		delete(legacy, strings.ToLower(key))
	}
	legacyData, _ := json.Marshal(legacy)
	if err := json.Unmarshal(legacyData, &decoded); err != nil || extraValuesOf(decoded) != extraDefaults {
		t.Fatalf("旧版 JSON = %+v err=%v，期望默认 %+v", extraValuesOf(decoded), err, extraDefaults)
	}
	// 只缺默认地址借付费模型一项时按默认开启
	delete(legacy, "extra_pool_default_paid")
	legacy["extra_pool"] = true
	legacyData, _ = json.Marshal(legacy)
	if err := json.Unmarshal(legacyData, &decoded); err != nil || !decoded.ExtraPool || !decoded.ExtraPoolDefaultPaid {
		t.Fatalf("缺少 extra_pool_default_paid 时应为真：%+v err=%v", extraValuesOf(decoded), err)
	}
	// 无效值改回默认：resetKey 覆盖每个键（漏掉时联动校验改不回默认值，整个服务起不来）
	reset := cfg
	for _, key := range extraPoolKeys {
		reset.resetKey(key)
	}
	if got := extraValuesOf(reset); got != extraDefaults {
		t.Fatalf("resetKey 后 = %+v，期望 %+v", got, extraDefaults)
	}
}
