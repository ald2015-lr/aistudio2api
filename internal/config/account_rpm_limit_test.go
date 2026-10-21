package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// TestAccountRPMLimitLoad ACCOUNT_RPM_LIMIT 默认 10；写 0 表示关闭，空值沿用默认；负数、超过 1000 与非数字报错；
// 进程环境变量优先于 .env；configKeys 必须包含这个键（否则 Save 丢掉它，环境变量也覆盖不了）
func TestAccountRPMLimitLoad(t *testing.T) {
	clearConfigEnv(t)
	if Default().AccountRPMLimit != 10 {
		t.Fatalf("ACCOUNT_RPM_LIMIT 默认值 = %d，期望 10", Default().AccountRPMLimit)
	}
	if !slices.Contains(configKeys[:], "ACCOUNT_RPM_LIMIT") {
		t.Fatal("configKeys 缺少 ACCOUNT_RPM_LIMIT：Save 会丢掉这个键，进程环境变量也覆盖不了它")
	}
	for _, test := range []struct {
		content string
		want    int
		invalid bool
	}{
		{content: "", want: 10},
		{content: "ACCOUNT_RPM_LIMIT=\n", want: 10},
		{content: "ACCOUNT_RPM_LIMIT=0\n", want: 0},
		{content: "ACCOUNT_RPM_LIMIT= 8 \n", want: 8},
		{content: "ACCOUNT_RPM_LIMIT=1000\n", want: 1000},
		{content: "ACCOUNT_RPM_LIMIT=-1\n", invalid: true},
		{content: "ACCOUNT_RPM_LIMIT=1001\n", invalid: true},
		{content: "ACCOUNT_RPM_LIMIT=abc\n", invalid: true},
	} {
		path := filepath.Join(t.TempDir(), ".env")
		if err := os.WriteFile(path, []byte(test.content), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err := Load(path)
		if test.invalid {
			if err == nil || !strings.Contains(err.Error(), "ACCOUNT_RPM_LIMIT") {
				t.Fatalf("%q 应报 ACCOUNT_RPM_LIMIT 错误，得到 %v", test.content, err)
			}
			continue
		}
		if err != nil || cfg.AccountRPMLimit != test.want {
			t.Fatalf("%q：得到 %d（%v），期望 %d", test.content, cfg.AccountRPMLimit, err, test.want)
		}
	}

	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte("ACCOUNT_RPM_LIMIT=3\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ACCOUNT_RPM_LIMIT", "7")
	if cfg, err := Load(path); err != nil || cfg.AccountRPMLimit != 7 {
		t.Fatalf("环境变量 ACCOUNT_RPM_LIMIT=7 应覆盖 .env: %d %v", cfg.AccountRPMLimit, err)
	}
	// 环境变量的空值视为未设置，沿用 .env 的值
	t.Setenv("ACCOUNT_RPM_LIMIT", " ")
	if cfg, err := Load(path); err != nil || cfg.AccountRPMLimit != 3 {
		t.Fatalf("空的环境变量不应覆盖 .env: %d %v", cfg.AccountRPMLimit, err)
	}
}

// TestAccountRPMLimitRoundTrip 保存后 .env 写出该键并能读回；JSON 往返保留它；旧版 JSON 没有该字段时取默认 10，显式 0 往返仍是 0
func TestAccountRPMLimitRoundTrip(t *testing.T) {
	clearConfigEnv(t)
	for _, limit := range []int{25, 0} {
		cfg := Default()
		cfg.AccountRPMLimit = limit
		path := filepath.Join(t.TempDir(), ".env")
		if err := cfg.Save(path); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if line := "ACCOUNT_RPM_LIMIT=" + strconv.Itoa(limit) + "\n"; !strings.Contains(string(data), line) {
			t.Fatalf(".env 缺少 %q:\n%s", line, data)
		}
		if loaded, err := Load(path); err != nil || loaded.AccountRPMLimit != limit {
			t.Fatalf(".env 往返 = %d err=%v，期望 %d", loaded.AccountRPMLimit, err, limit)
		}

		encoded, err := json.Marshal(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if field := `"account_rpm_limit":` + strconv.Itoa(limit); !strings.Contains(string(encoded), field) {
			t.Fatalf("JSON 缺少 %s: %s", field, encoded)
		}
		var decoded Config
		if err := json.Unmarshal(encoded, &decoded); err != nil || decoded.AccountRPMLimit != limit {
			t.Fatalf("JSON 往返 = %d err=%v，期望 %d", decoded.AccountRPMLimit, err, limit)
		}

		var legacy map[string]any
		if err := json.Unmarshal(encoded, &legacy); err != nil {
			t.Fatal(err)
		}
		delete(legacy, "account_rpm_limit")
		legacyData, _ := json.Marshal(legacy)
		var legacyDecoded Config
		if err := json.Unmarshal(legacyData, &legacyDecoded); err != nil || legacyDecoded.AccountRPMLimit != 10 {
			t.Fatalf("旧版 JSON = %d err=%v，期望默认 10", legacyDecoded.AccountRPMLimit, err)
		}
	}

	cfg := Default()
	cfg.AccountRPMLimit = 1001
	if err := cfg.Save(filepath.Join(t.TempDir(), ".env")); err == nil || err.Error() != "ACCOUNT_RPM_LIMIT 必须是 0 到 1000 的整数" {
		t.Fatalf("超过 1000 应拒绝保存，得到 %v", err)
	}
}
