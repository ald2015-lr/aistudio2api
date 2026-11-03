package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestStreamDataHeartbeatConfig 数据型心跳默认开启，可显式关闭，非布尔值报错；.env 与管理接口的 JSON 往返不变，旧版 JSON 没有该字段时按默认开启
func TestStreamDataHeartbeatConfig(t *testing.T) {
	t.Setenv("STREAM_DATA_HEARTBEAT", "")
	os.Unsetenv("STREAM_DATA_HEARTBEAT")
	if !Default().StreamDataHeartbeat {
		t.Fatal("STREAM_DATA_HEARTBEAT 默认应开启")
	}
	for _, test := range []struct {
		content string
		want    bool
		fail    bool
	}{
		{content: "", want: true},
		{content: "STREAM_DATA_HEARTBEAT=\n", want: true},
		{content: "STREAM_DATA_HEARTBEAT=false\n", want: false},
		{content: "STREAM_DATA_HEARTBEAT=true\n", want: true},
		{content: "STREAM_DATA_HEARTBEAT=off\n", fail: true},
	} {
		path := filepath.Join(t.TempDir(), ".env")
		if err := os.WriteFile(path, []byte(test.content), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err := Load(path)
		if test.fail {
			if requireInvalidKey(t, test.content, cfg, err, "STREAM_DATA_HEARTBEAT"); !cfg.StreamDataHeartbeat {
				t.Fatalf("%q：无效时应取默认值 true", test.content)
			}
			continue
		}
		if err != nil || cfg.StreamDataHeartbeat != test.want {
			t.Fatalf("%q: StreamDataHeartbeat=%v err=%v，期望 %v", test.content, cfg.StreamDataHeartbeat, err, test.want)
		}
	}
	// 进程环境变量优先于 .env
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte("STREAM_DATA_HEARTBEAT=true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("STREAM_DATA_HEARTBEAT", "false")
	if cfg, err := Load(path); err != nil || cfg.StreamDataHeartbeat {
		t.Fatalf("环境变量 false 应覆盖 .env: %v %v", cfg.StreamDataHeartbeat, err)
	}
	os.Unsetenv("STREAM_DATA_HEARTBEAT")

	cfg := Default()
	cfg.StreamDataHeartbeat = false
	path = filepath.Join(t.TempDir(), ".env")
	if err := cfg.Save(path); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if !containsLine(string(data), "STREAM_DATA_HEARTBEAT=false") {
		t.Fatalf(".env 缺少 STREAM_DATA_HEARTBEAT=false:\n%s", data)
	}
	if loaded, err := Load(path); err != nil || loaded.StreamDataHeartbeat {
		t.Fatalf("保存后读回 StreamDataHeartbeat=%v err=%v", loaded.StreamDataHeartbeat, err)
	}

	encoded, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var decoded Config
	if err := json.Unmarshal(encoded, &decoded); err != nil || decoded.StreamDataHeartbeat {
		t.Fatalf("JSON 往返 StreamDataHeartbeat=%v err=%v", decoded.StreamDataHeartbeat, err)
	}
	var legacy map[string]any
	_ = json.Unmarshal(encoded, &legacy)
	delete(legacy, "stream_data_heartbeat")
	legacyJSON, _ := json.Marshal(legacy)
	if err := json.Unmarshal(legacyJSON, &decoded); err != nil || !decoded.StreamDataHeartbeat {
		t.Fatalf("旧版 JSON 没有该字段时应按默认开启: %v %v", decoded.StreamDataHeartbeat, err)
	}
}

func containsLine(text string, line string) bool {
	for start := 0; start < len(text); {
		end := start
		for end < len(text) && text[end] != '\n' {
			end++
		}
		if text[start:end] == line {
			return true
		}
		start = end + 1
	}
	return false
}
