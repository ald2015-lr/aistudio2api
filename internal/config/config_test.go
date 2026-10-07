package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestProxyAPIKeyDefaultsWhenEmpty 没有设置或留空的 PROXY_API_KEY 使用默认密钥，公开 API 不会免密钥访问
func TestProxyAPIKeyDefaultsWhenEmpty(t *testing.T) {
	for name, content := range map[string]string{
		"missing": "LISTEN_ADDR=127.0.0.1:2048\n",
		"empty":   "PROXY_API_KEY=\n",
		"blank":   "PROXY_API_KEY=\"  \"\n",
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("PROXY_API_KEY", "")
			os.Unsetenv("PROXY_API_KEY")
			path := filepath.Join(t.TempDir(), ".env")
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := Load(path)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.ProxyAPIKey != DefaultProxyAPIKey {
				t.Fatalf("ProxyAPIKey = %q，期望默认密钥", cfg.ProxyAPIKey)
			}
		})
	}
}

// TestProxyAPIKeyCustomValueKept 自定义密钥原样生效
func TestProxyAPIKeyCustomValueKept(t *testing.T) {
	t.Setenv("PROXY_API_KEY", "")
	os.Unsetenv("PROXY_API_KEY")
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte("PROXY_API_KEY=sk-custom\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ProxyAPIKey != "sk-custom" {
		t.Fatalf("ProxyAPIKey = %q", cfg.ProxyAPIKey)
	}
	if EffectiveProxyAPIKey(" ") != DefaultProxyAPIKey || EffectiveProxyAPIKey(" k ") != "k" {
		t.Fatal("EffectiveProxyAPIKey 结果错误")
	}
}

// TestDowngradeRejectStatusConfig 降级拒绝返回码默认 400，可设为 503，其他值报错
func TestDowngradeRejectStatusConfig(t *testing.T) {
	t.Setenv("DOWNGRADE_REJECT_STATUS", "")
	os.Unsetenv("DOWNGRADE_REJECT_STATUS")
	for _, test := range []struct {
		content string
		want    int
		invalid bool
	}{
		{content: "", want: DowngradeRejectBlocked},
		{content: "DOWNGRADE_REJECT_STATUS=503\n", want: DowngradeRejectUnavailable},
		{content: "DOWNGRADE_REJECT_STATUS=400\n", want: DowngradeRejectBlocked},
		{content: "DOWNGRADE_REJECT_STATUS=502\n", invalid: true},
	} {
		path := filepath.Join(t.TempDir(), ".env")
		if err := os.WriteFile(path, []byte(test.content), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err := Load(path)
		if err == nil {
			err = cfg.Validate()
		}
		if test.invalid {
			if err == nil {
				t.Fatalf("%q 应报错", test.content)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%q: %v", test.content, err)
		}
		if cfg.DowngradeGuard.RejectStatus != test.want {
			t.Fatalf("%q: RejectStatus = %d", test.content, cfg.DowngradeGuard.RejectStatus)
		}
	}
}

// TestEnvFileParsing .env 兼容 export 前缀、BOM、Windows 路径与引号后的行内注释
func TestEnvFileParsing(t *testing.T) {
	for _, key := range []string{"PROXY_API_KEY", "AISTUDIO_AUTH_STATES", "PROXY"} {
		t.Setenv(key, "")
		os.Unsetenv(key)
	}
	for _, test := range []struct {
		name    string
		content string
		key     func(Config) string
		want    string
	}{
		{name: "export", content: "export PROXY_API_KEY=sk-export\n", key: func(c Config) string { return c.ProxyAPIKey }, want: "sk-export"},
		{name: "BOM", content: "\ufeffPROXY_API_KEY=sk-bom\n", key: func(c Config) string { return c.ProxyAPIKey }, want: "sk-bom"},
		{name: "BOM 注释", content: "\ufeff# 注释\nPROXY_API_KEY=sk-bom2\n", key: func(c Config) string { return c.ProxyAPIKey }, want: "sk-bom2"},
		{name: "Windows 路径", content: `AISTUDIO_AUTH_STATES="D:\accounts\new"` + "\n", key: func(c Config) string { return c.AuthStates }, want: `D:\accounts\new`},
		{name: "双引号行内注释", content: `PROXY_API_KEY="sk a" # 注释` + "\n", key: func(c Config) string { return c.ProxyAPIKey }, want: "sk a"},
		{name: "单引号行内注释", content: "PROXY_API_KEY='sk b' # 注释\n", key: func(c Config) string { return c.ProxyAPIKey }, want: "sk b"},
		{name: "转义引号", content: `PROXY_API_KEY="sk\"q\\x"` + "\n", key: func(c Config) string { return c.ProxyAPIKey }, want: `sk"q\x`},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), ".env")
			if err := os.WriteFile(path, []byte(test.content), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := Load(path)
			if err != nil {
				t.Fatal(err)
			}
			if got := test.key(cfg); got != test.want {
				t.Fatalf("得到 %q，期望 %q", got, test.want)
			}
		})
	}
}

// TestEnvFileRoundTrip 保存后再读取得到相同的值（含反斜杠、引号、空格与 #）
func TestEnvFileRoundTrip(t *testing.T) {
	for _, key := range configKeys {
		t.Setenv(key, "")
		os.Unsetenv(key)
	}
	cfg := Default()
	cfg.AuthStates = `D:\auth dir\#1`
	cfg.ProxyAPIKey = `sk-"quoted"\back`
	cfg.AdminPassword = `p a s s '#'`
	path := filepath.Join(t.TempDir(), ".env")
	if err := cfg.Save(path); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.AuthStates != cfg.AuthStates || loaded.ProxyAPIKey != cfg.ProxyAPIKey || loaded.AdminPassword != cfg.AdminPassword {
		t.Fatalf("往返不一致: %q %q %q", loaded.AuthStates, loaded.ProxyAPIKey, loaded.AdminPassword)
	}
}

// TestEmptyEnvironmentDoesNotOverride 空的进程环境变量不覆盖 .env
func TestEmptyEnvironmentDoesNotOverride(t *testing.T) {
	t.Setenv("PROXY_API_KEY", "")
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte("PROXY_API_KEY=sk-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ProxyAPIKey != "sk-file" {
		t.Fatalf("ProxyAPIKey = %q", cfg.ProxyAPIKey)
	}
	t.Setenv("PROXY_API_KEY", "sk-env")
	if cfg, _ = Load(path); cfg.ProxyAPIKey != "sk-env" {
		t.Fatalf("非空环境变量应覆盖 .env，得到 %q", cfg.ProxyAPIKey)
	}
}
