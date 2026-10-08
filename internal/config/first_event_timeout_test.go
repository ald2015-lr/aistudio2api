package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestFirstEventTimeoutConfig 首事件超时默认关闭（0），可设为正数时长，负数、无法解析或不小于请求超时时报错
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
		{content: "FIRST_EVENT_TIMEOUT=-1s\n", invalid: true},
		{content: "FIRST_EVENT_TIMEOUT=abc\n", invalid: true},
		{content: "REQUEST_TIMEOUT=1m\nFIRST_EVENT_TIMEOUT=1m\n", invalid: true},
	} {
		path := filepath.Join(t.TempDir(), ".env")
		if err := os.WriteFile(path, []byte(test.content), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err := Load(path)
		if test.invalid {
			if err == nil {
				t.Fatalf("%q 应报错", test.content)
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
