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
