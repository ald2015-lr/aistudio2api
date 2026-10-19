package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestUltraIncludeProLoad ULTRA_INCLUDE_PRO 默认关闭；true/false 不区分大小写、两侧空白忽略，空值沿用默认；
// 非法值报错；进程环境变量优先于 .env
func TestUltraIncludeProLoad(t *testing.T) {
	clearConfigEnv(t)
	if Default().UltraIncludePro {
		t.Fatal("ULTRA_INCLUDE_PRO 默认应关闭")
	}
	if !slices.Contains(configKeys[:], "ULTRA_INCLUDE_PRO") {
		t.Fatal("configKeys 缺少 ULTRA_INCLUDE_PRO：Save 会丢掉这个键，进程环境变量也覆盖不了它")
	}
	for _, test := range []struct {
		content string
		want    bool
		invalid bool
	}{
		{content: "", want: false},
		{content: "ULTRA_INCLUDE_PRO=\n", want: false},
		{content: "ULTRA_INCLUDE_PRO=true\n", want: true},
		{content: "ULTRA_INCLUDE_PRO= TRUE \n", want: true},
		{content: "ULTRA_INCLUDE_PRO=false\n", want: false},
		{content: "ULTRA_INCLUDE_PRO=maybe\n", invalid: true},
	} {
		path := filepath.Join(t.TempDir(), ".env")
		if err := os.WriteFile(path, []byte(test.content), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err := Load(path)
		if test.invalid {
			if err == nil || err.Error() != "ULTRA_INCLUDE_PRO 必须是 true 或 false" {
				t.Fatalf("%q 应报 ULTRA_INCLUDE_PRO 错误，得到 %v", test.content, err)
			}
			continue
		}
		if err != nil || cfg.UltraIncludePro != test.want {
			t.Fatalf("%q：得到 %t（%v），期望 %t", test.content, cfg.UltraIncludePro, err, test.want)
		}
		// 开关与独占设置互不影响
		if !cfg.UltraExclusive {
			t.Fatalf("%q：ULTRA_EXCLUSIVE 不应受影响", test.content)
		}
	}

	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte("ULTRA_INCLUDE_PRO=false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ULTRA_INCLUDE_PRO", "true")
	if cfg, err := Load(path); err != nil || !cfg.UltraIncludePro {
		t.Fatalf("环境变量 ULTRA_INCLUDE_PRO=true 应覆盖 .env: %t %v", cfg.UltraIncludePro, err)
	}
}

// TestUltraIncludeProRoundTrip 保存后 .env 写出该键并能读回；JSON 往返保留它；旧版 JSON 没有该字段时按关闭处理
func TestUltraIncludeProRoundTrip(t *testing.T) {
	clearConfigEnv(t)
	cfg := Default()
	cfg.UltraIncludePro = true
	path := filepath.Join(t.TempDir(), ".env")
	if err := cfg.Save(path); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "ULTRA_INCLUDE_PRO=true\n") {
		t.Fatalf(".env 缺少 ULTRA_INCLUDE_PRO=true:\n%s", data)
	}
	if loaded, err := Load(path); err != nil || !loaded.UltraIncludePro {
		t.Fatalf(".env 往返 = %t err=%v", loaded.UltraIncludePro, err)
	}

	encoded, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"ultra_include_pro":true`) {
		t.Fatalf("JSON 缺少 ultra_include_pro: %s", encoded)
	}
	var decoded Config
	if err := json.Unmarshal(encoded, &decoded); err != nil || !decoded.UltraIncludePro {
		t.Fatalf("JSON 往返 = %t err=%v", decoded.UltraIncludePro, err)
	}
	var legacy map[string]any
	if err := json.Unmarshal(encoded, &legacy); err != nil {
		t.Fatal(err)
	}
	delete(legacy, "ultra_include_pro")
	legacyData, _ := json.Marshal(legacy)
	if err := json.Unmarshal(legacyData, &decoded); err != nil || decoded.UltraIncludePro {
		t.Fatalf("旧版 JSON = %t err=%v，期望关闭", decoded.UltraIncludePro, err)
	}
}
