package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestProxyPoolFixedDefaults 固定分配默认关闭；键在 configKeys 里紧跟 PROXY_POOL_PORT_END，写错时启动即报错（fatal）
func TestProxyPoolFixedDefaults(t *testing.T) {
	clearProxyEnvironment(t)
	if DefaultProxyPool().Fixed || Default().ProxyPool.Fixed {
		t.Fatal("固定分配应默认关闭")
	}
	index := slices.Index(configKeys[:], "PROXY_POOL_FIXED")
	if index < 0 || configKeys[index-1] != "PROXY_POOL_PORT_END" {
		t.Fatalf("PROXY_POOL_FIXED 应紧跟 PROXY_POOL_PORT_END：下标 %d", index)
	}
	if !fatalConfigKeys["PROXY_POOL_FIXED"] || secretConfigKeys["PROXY_POOL_FIXED"] {
		t.Fatal("PROXY_POOL_FIXED 应是启动即报错的键、不是凭据键")
	}
	cfg, _, err := writeProxyPoolEnv(t, "PROXY_POOL_MODE=off\n")
	if err != nil || cfg.ProxyPool.Fixed {
		t.Fatalf("没写时 = %v (%v)", cfg.ProxyPool.Fixed, err)
	}
}

// TestProxyPoolFixedEnvRoundTrip true/false/1/0/大小写往返；无效值 Load 返回固定文案的错误（不按 false 启动）；进程环境变量覆盖 .env
func TestProxyPoolFixedEnvRoundTrip(t *testing.T) {
	clearProxyEnvironment(t)
	base := "PROXY_POOL_MODE=static\nPROXY_POOL_URL=http://user:pass@proxy.example.com:10000\nPROXY_POOL_PORT_START=10001\nPROXY_POOL_PORT_END=10004\n"
	for value, want := range map[string]bool{"true": true, "false": false, "1": true, "0": false, "TRUE": true, " True ": true, "": false} {
		cfg, path, err := writeProxyPoolEnv(t, base+"PROXY_POOL_FIXED="+value+"\n")
		if err != nil || cfg.ProxyPool.Fixed != want {
			t.Fatalf("%q: Fixed = %v (%v)", value, cfg.ProxyPool.Fixed, err)
		}
		if err := cfg.Save(path); err != nil {
			t.Fatal(err)
		}
		saved, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		line := "\nPROXY_POOL_PORT_END=10004\nPROXY_POOL_FIXED=" + map[bool]string{true: "true", false: "false"}[want] + "\n"
		if !strings.Contains(string(saved), line) {
			t.Fatalf("%q: 保存应写规范值且紧跟端口段：\n%s", value, saved)
		}
		reloaded, err := Load(path)
		if err != nil || reloaded.ProxyPool != cfg.ProxyPool {
			t.Fatalf("%q: 往返不一致 %+v → %+v (%v)", value, cfg.ProxyPool, reloaded.ProxyPool, err)
		}
	}
	for _, value := range []string{"yes", "on", "2", "开"} {
		_, _, err := writeProxyPoolEnv(t, base+"PROXY_POOL_FIXED="+value+"\n")
		if err == nil || err.Error() != "PROXY_POOL_FIXED 必须是 true 或 false" {
			t.Fatalf("%q: 写错应启动即报错：%v", value, err)
		}
	}
	t.Setenv("PROXY_POOL_FIXED", "true")
	cfg, _, err := writeProxyPoolEnv(t, base+"PROXY_POOL_FIXED=false\n")
	if err != nil || !cfg.ProxyPool.Fixed {
		t.Fatalf("进程环境变量应覆盖 .env：%v (%v)", cfg.ProxyPool.Fixed, err)
	}
}

// TestProxyPoolFixedJSONAndEqual 关闭时 JSON 与原来逐字节一致（omitempty）；开启时带 "fixed":true；Equal 只差 Fixed 时为假
func TestProxyPoolFixedJSONAndEqual(t *testing.T) {
	pool := ProxyPool{Mode: ProxyPoolStatic, URL: "http://user:pass@proxy.example.com:10000", PortStart: 10001, PortEnd: 10004}
	data, err := json.Marshal(pool)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != `{"mode":"static","url":"http://user:pass@proxy.example.com:10000","port_start":10001,"port_end":10004}` {
		t.Fatalf("关闭时 JSON 应与原来相同：%s", data)
	}
	fixed := pool
	fixed.Fixed = true
	if data, err = json.Marshal(fixed); err != nil || !strings.HasSuffix(string(data), `,"fixed":true}`) {
		t.Fatalf("开启时 JSON = %s (%v)", data, err)
	}
	var decoded ProxyPool
	if err := json.Unmarshal(data, &decoded); err != nil || decoded != fixed {
		t.Fatalf("JSON 往返 = %+v (%v)", decoded, err)
	}
	if pool.Equal(fixed) || !fixed.Equal(fixed) {
		t.Fatal("Equal 应区分固定分配")
	}
	cfg := Default()
	cfg.ProxyPool = fixed
	if data, err = json.Marshal(cfg); err != nil {
		t.Fatal(err)
	}
	var config Config
	if err := json.Unmarshal(data, &config); err != nil || !config.ProxyPool.Fixed {
		t.Fatalf("Config JSON 往返应保留固定分配：%+v (%v)", config.ProxyPool, err)
	}
}

// TestProxyPoolFixedSummary 关闭时摘要不变；开启时静态追加“，固定分配”、动态追加“，固定会话”；off 仍为“关闭”
func TestProxyPoolFixedSummary(t *testing.T) {
	static := ProxyPool{Mode: ProxyPoolStatic, URL: "http://user:pass@proxy.example.com:10000", PortStart: 10001, PortEnd: 10004}
	dynamic := ProxyPool{Mode: ProxyPoolDynamic, URL: "http://user-{session}:pass@gate.example.com:7777"}
	if static.Summary() != "静态 http://proxy.example.com 端口 10001-10004（4 个）" || dynamic.Summary() != "动态 http://gate.example.com" {
		t.Fatalf("关闭时摘要改变：%s / %s", static.Summary(), dynamic.Summary())
	}
	static.Fixed, dynamic.Fixed = true, true
	if static.Summary() != "静态 http://proxy.example.com 端口 10001-10004（4 个），固定分配" ||
		dynamic.Summary() != "动态 http://gate.example.com，固定会话" {
		t.Fatalf("开启时摘要 = %s / %s", static.Summary(), dynamic.Summary())
	}
	if summary := (ProxyPool{Mode: ProxyPoolOff, Fixed: true}).Summary(); summary != "关闭" {
		t.Fatalf("off 时摘要 = %s", summary)
	}
	for _, summary := range []string{static.Summary(), dynamic.Summary()} {
		if strings.Contains(summary, "pass") || strings.Contains(summary, "user") {
			t.Fatalf("摘要含账号密码：%s", summary)
		}
	}
	// 写错的值记进无效项检查的键列表（fatal）
	path := filepath.Join(t.TempDir(), ".env")
	_ = os.WriteFile(path, []byte("PROXY_POOL_FIXED=maybe\n"), 0o600)
	if _, err := Load(path); err == nil {
		t.Fatal("写错的 PROXY_POOL_FIXED 应启动即报错")
	}
}
