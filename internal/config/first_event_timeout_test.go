package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestFirstEventTimeoutConfig 首事件超时默认关闭（0），可设为正数时长、off（关闭）或整数秒（上游 Mag1cFall/AIStudio2API 的写法），
// 负数、无法解析或不小于请求超时时报错。原先 off 与 60 按无效值退回关闭（照抄上游配置的用户以为开了首事件超时）
func TestFirstEventTimeoutConfig(t *testing.T) {
	for _, key := range []string{"FIRST_EVENT_TIMEOUT", "REQUEST_TIMEOUT"} {
		t.Setenv(key, "")
		os.Unsetenv(key)
	}
	for _, test := range []struct {
		content string
		want    time.Duration
		invalid bool
	}{
		{content: "", want: 0},
		{content: "FIRST_EVENT_TIMEOUT=\n", want: 0},
		{content: "FIRST_EVENT_TIMEOUT=0\n", want: 0},
		{content: "FIRST_EVENT_TIMEOUT=90s\n", want: 90 * time.Second},
		{content: "FIRST_EVENT_TIMEOUT=off\n", want: 0},
		{content: "FIRST_EVENT_TIMEOUT=OFF\n", want: 0},
		{content: "FIRST_EVENT_TIMEOUT=60\n", want: 60 * time.Second},
		{content: "FIRST_EVENT_TIMEOUT= 45 \n", want: 45 * time.Second},
		{content: "FIRST_EVENT_TIMEOUT=-1s\n", invalid: true},
		{content: "FIRST_EVENT_TIMEOUT=-60\n", invalid: true},
		{content: "FIRST_EVENT_TIMEOUT=60x\n", invalid: true},
		{content: "FIRST_EVENT_TIMEOUT=99999999999999999\n", invalid: true},
		{content: "FIRST_EVENT_TIMEOUT=abc\n", invalid: true},
		{content: "REQUEST_TIMEOUT=1m\nFIRST_EVENT_TIMEOUT=60\n", invalid: true},
		{content: "REQUEST_TIMEOUT=1m\nFIRST_EVENT_TIMEOUT=1m\n", invalid: true},
	} {
		path := filepath.Join(t.TempDir(), ".env")
		if err := os.WriteFile(path, []byte(test.content), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err := Load(path)
		if test.invalid {
			// 写法无效或不小于 REQUEST_TIMEOUT 时按默认值（关闭）启动并记入无效项
			requireInvalidKey(t, test.content, cfg, err, "FIRST_EVENT_TIMEOUT")
			if cfg.FirstEventTimeout != 0 {
				t.Fatalf("%q：无效时应取默认值 0，得到 %s", test.content, cfg.FirstEventTimeout)
			}
			continue
		}
		if err != nil || cfg.FirstEventTimeout != test.want {
			t.Fatalf("%q：FirstEventTimeout = %s err=%v，期望 %s", test.content, cfg.FirstEventTimeout, err, test.want)
		}
	}
}

// TestFirstEventTimeoutRoundTrip 保存后再读取、JSON 往返都保留首事件超时；旧版 JSON 没有该字段时按关闭处理
func TestFirstEventTimeoutRoundTrip(t *testing.T) {
	for _, key := range configKeys {
		t.Setenv(key, "")
		os.Unsetenv(key)
	}
	cfg := Default()
	if cfg.FirstEventTimeout != 0 {
		t.Fatalf("默认值 = %s，期望关闭", cfg.FirstEventTimeout)
	}
	cfg.FirstEventTimeout = 45 * time.Second
	path := filepath.Join(t.TempDir(), ".env")
	if err := cfg.Save(path); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path)
	if err != nil || loaded.FirstEventTimeout != cfg.FirstEventTimeout {
		t.Fatalf(".env 往返 = %s err=%v，期望 %s", loaded.FirstEventTimeout, err, cfg.FirstEventTimeout)
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var decoded Config
	if err := json.Unmarshal(data, &decoded); err != nil || decoded.FirstEventTimeout != cfg.FirstEventTimeout {
		t.Fatalf("JSON 往返 = %s err=%v，期望 %s", decoded.FirstEventTimeout, err, cfg.FirstEventTimeout)
	}
	var legacy map[string]any
	if err := json.Unmarshal(data, &legacy); err != nil {
		t.Fatal(err)
	}
	delete(legacy, "first_event_timeout")
	legacyData, _ := json.Marshal(legacy)
	if err := json.Unmarshal(legacyData, &decoded); err != nil || decoded.FirstEventTimeout != 0 {
		t.Fatalf("旧版 JSON：FirstEventTimeout = %s err=%v，期望关闭", decoded.FirstEventTimeout, err)
	}
}

// TestFirstEventTimeoutParseShared 管理页保存与启动加载共用 ParseFirstEventTimeout：两种新写法与原有时长写法都认，错误文本写明可用写法；
// 共用 parseOptionalDuration 的两个主用模型排队上限不放宽（没有上游写法要兼容，0 已表示不排队）：off 与整数仍按无效值处理
func TestFirstEventTimeoutParseShared(t *testing.T) {
	for value, want := range map[string]time.Duration{"off": 0, "Off": 0, "0": 0, "0s": 0, "60": time.Minute, "+90": 90 * time.Second, "2m": 2 * time.Minute} {
		if got, err := ParseFirstEventTimeout(value); err != nil || got != want {
			t.Fatalf("ParseFirstEventTimeout(%q) = %s, %v，期望 %s", value, got, err, want)
		}
	}
	// 18446744074 秒乘以 1e9 纳秒会溢出成约 0.29 秒：没有溢出检查时会当成一个很短的首事件超时
	for _, value := range []string{"-1", "-1s", "1.5", "on", "", "18446744074"} {
		if _, err := ParseFirstEventTimeout(value); err == nil || !strings.Contains(err.Error(), "off") {
			t.Fatalf("ParseFirstEventTimeout(%q) 应报错并写明可用写法，得到 %v", value, err)
		}
	}
	for _, key := range []string{"PRIMARY_QUEUE_WAIT", "ULTRA_PRIMARY_QUEUE_WAIT"} {
		for _, value := range []string{"off", "10"} {
			if _, err := parseOptionalDuration(key, value); err == nil {
				t.Fatalf("%s=%s 应仍按无效值处理", key, value)
			}
		}
	}
}
