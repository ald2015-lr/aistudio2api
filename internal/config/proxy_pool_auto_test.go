package config

import (
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"
)

// 坏 IP 自动换的上限（PROXY_POOL_FIXED_AUTO_REPLACE）的读取、保存、校验与 JSON

const proxyPoolAutoBase = "PROXY_POOL_MODE=static\nPROXY_POOL_URL=http://user:pass@proxy.example.com:10000\nPROXY_POOL_PORT_START=10001\nPROXY_POOL_PORT_END=10004\nPROXY_POOL_FIXED=true\n"

// TestProxyPoolFixedAutoReplaceDefaults C1：Default 与没写时为 10；0、25、5000 照写；进程环境变量覆盖 .env；键紧跟 PROXY_POOL_FIXED，
// 启动即报错、不是凭据键；保存写出这一行；config.Config{} 字面量为 0（不自动换）
func TestProxyPoolFixedAutoReplaceDefaults(t *testing.T) {
	clearProxyEnvironment(t)
	if Default().ProxyPoolFixedAutoReplace != DefaultProxyPoolFixedAutoReplace || DefaultProxyPoolFixedAutoReplace != 10 ||
		MaxProxyPoolFixedAutoReplace != 5000 || (Config{}).ProxyPoolFixedAutoReplace != 0 {
		t.Fatalf("默认值 = %d / %d", Default().ProxyPoolFixedAutoReplace, DefaultProxyPoolFixedAutoReplace)
	}
	index := slices.Index(configKeys[:], "PROXY_POOL_FIXED_AUTO_REPLACE")
	if index < 0 || configKeys[index-1] != "PROXY_POOL_FIXED" {
		t.Fatalf("PROXY_POOL_FIXED_AUTO_REPLACE 应紧跟 PROXY_POOL_FIXED：下标 %d", index)
	}
	if !fatalConfigKeys["PROXY_POOL_FIXED_AUTO_REPLACE"] || secretConfigKeys["PROXY_POOL_FIXED_AUTO_REPLACE"] {
		t.Fatal("应是启动即报错的键、不是凭据键")
	}
	cfg, _, err := writeProxyPoolEnv(t, proxyPoolAutoBase)
	if err != nil || cfg.ProxyPoolFixedAutoReplace != 10 {
		t.Fatalf("没写时 = %d (%v)", cfg.ProxyPoolFixedAutoReplace, err)
	}
	cfg, _, err = writeProxyPoolEnv(t, proxyPoolAutoBase+"PROXY_POOL_FIXED_AUTO_REPLACE=\n")
	if err != nil || cfg.ProxyPoolFixedAutoReplace != 10 {
		t.Fatalf("空值应按默认 = %d (%v)", cfg.ProxyPoolFixedAutoReplace, err)
	}
	for value, want := range map[string]int{"0": 0, "25": 25, " 5000 ": 5000, "10": 10} {
		cfg, path, err := writeProxyPoolEnv(t, proxyPoolAutoBase+"PROXY_POOL_FIXED_AUTO_REPLACE="+value+"\n")
		if err != nil || cfg.ProxyPoolFixedAutoReplace != want {
			t.Fatalf("%q: = %d (%v)", value, cfg.ProxyPoolFixedAutoReplace, err)
		}
		if err := cfg.Save(path); err != nil {
			t.Fatal(err)
		}
		saved, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		line := "\nPROXY_POOL_FIXED=true\nPROXY_POOL_FIXED_AUTO_REPLACE=" + strings.TrimSpace(value) + "\n"
		if !strings.Contains(string(saved), line) {
			t.Fatalf("%q: 保存应写规范值且紧跟 PROXY_POOL_FIXED：\n%s", value, saved)
		}
		reloaded, err := Load(path)
		if err != nil || reloaded.ProxyPoolFixedAutoReplace != want {
			t.Fatalf("%q: 往返 = %d (%v)", value, reloaded.ProxyPoolFixedAutoReplace, err)
		}
	}
	t.Setenv("PROXY_POOL_FIXED_AUTO_REPLACE", "3")
	cfg, _, err = writeProxyPoolEnv(t, proxyPoolAutoBase+"PROXY_POOL_FIXED_AUTO_REPLACE=7\n")
	if err != nil || cfg.ProxyPoolFixedAutoReplace != 3 {
		t.Fatalf("进程环境变量应覆盖 .env：%d (%v)", cfg.ProxyPoolFixedAutoReplace, err)
	}
}

// TestProxyPoolFixedAutoReplaceInvalid C2/C3：写错（负数、非整数、超过 5000、小数）时 Load 启动即报错，固定文案、不回显原值；Validate 拦下越界值
func TestProxyPoolFixedAutoReplaceInvalid(t *testing.T) {
	clearProxyEnvironment(t)
	const message = "PROXY_POOL_FIXED_AUTO_REPLACE 必须是 0 到 5000 的整数"
	for _, value := range []string{"-1", "abc", "5001", "1.5", "１０"} {
		_, _, err := writeProxyPoolEnv(t, proxyPoolAutoBase+"PROXY_POOL_FIXED_AUTO_REPLACE="+value+"\n")
		if err == nil || err.Error() != message {
			t.Fatalf("%q: 写错应启动即报错：%v", value, err)
		}
	}
	for _, limit := range []int{-1, 5001} {
		cfg := Default()
		cfg.ProxyPoolFixedAutoReplace = limit
		if err := cfg.Validate(); err == nil || err.Error() != message {
			t.Fatalf("%d: Validate 应拦下：%v", limit, err)
		}
	}
	cfg := Default()
	cfg.ProxyPoolFixedAutoReplace = 0
	if err := cfg.Validate(); err != nil {
		t.Fatalf("0 应合法：%v", err)
	}
}

// TestProxyPoolFixedAutoReplaceJSON C4：没有 proxy_pool_fixed_auto_replace 时（旧数据）解码为 10；0 与 25 往返不变
func TestProxyPoolFixedAutoReplaceJSON(t *testing.T) {
	data, err := json.Marshal(Default())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"proxy_pool_fixed_auto_replace":10`) {
		t.Fatalf("JSON 应带默认值：%s", data)
	}
	var payload map[string]any
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatal(err)
	}
	delete(payload, "proxy_pool_fixed_auto_replace")
	legacy, _ := json.Marshal(payload)
	var decoded Config
	if err := json.Unmarshal(legacy, &decoded); err != nil || decoded.ProxyPoolFixedAutoReplace != 10 {
		t.Fatalf("旧数据应解码为 10：%d (%v)", decoded.ProxyPoolFixedAutoReplace, err)
	}
	for _, limit := range []int{0, 25} {
		cfg := Default()
		cfg.ProxyPoolFixedAutoReplace = limit
		data, err := json.Marshal(cfg)
		if err != nil {
			t.Fatal(err)
		}
		var back Config
		if err := json.Unmarshal(data, &back); err != nil || back.ProxyPoolFixedAutoReplace != limit {
			t.Fatalf("%d: 往返 = %d (%v)", limit, back.ProxyPoolFixedAutoReplace, err)
		}
	}
}
