package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// TestAbnormalFinishErrorConfig 异常结束按错误返回（ABNORMAL_FINISH_ERROR）默认关闭，可显式打开，非布尔值按默认值加载并记入无效项；
// 进程环境变量优先于 .env；.env 与管理接口的 JSON 往返不变，旧版 JSON 没有该字段时按默认关闭
func TestAbnormalFinishErrorConfig(t *testing.T) {
	t.Setenv("ABNORMAL_FINISH_ERROR", "")
	os.Unsetenv("ABNORMAL_FINISH_ERROR")
	if !slices.Contains(configKeys[:], "ABNORMAL_FINISH_ERROR") {
		t.Fatal("configKeys 缺少 ABNORMAL_FINISH_ERROR：Save 会丢掉这个键，进程环境变量也覆盖不了它")
	}
	if Default().AbnormalFinishError {
		t.Fatal("ABNORMAL_FINISH_ERROR 默认应关闭")
	}
	for _, test := range []struct {
		content string
		want    bool
		fail    bool
	}{
		{content: "", want: false},
		{content: "ABNORMAL_FINISH_ERROR=\n", want: false},
		{content: "ABNORMAL_FINISH_ERROR=true\n", want: true},
		{content: "ABNORMAL_FINISH_ERROR= 1 \n", want: true},
		{content: "ABNORMAL_FINISH_ERROR=false\n", want: false},
		{content: "ABNORMAL_FINISH_ERROR=on\n", fail: true},
	} {
		path := filepath.Join(t.TempDir(), ".env")
		if err := os.WriteFile(path, []byte(test.content), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err := Load(path)
		if test.fail {
			if requireInvalidKey(t, test.content, cfg, err, "ABNORMAL_FINISH_ERROR"); cfg.AbnormalFinishError {
				t.Fatalf("%q：无效时应取默认值 false", test.content)
			}
			continue
		}
		if err != nil || cfg.AbnormalFinishError != test.want {
			t.Fatalf("%q: AbnormalFinishError=%v err=%v，期望 %v", test.content, cfg.AbnormalFinishError, err, test.want)
		}
	}
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte("ABNORMAL_FINISH_ERROR=false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ABNORMAL_FINISH_ERROR", "true")
	if cfg, err := Load(path); err != nil || !cfg.AbnormalFinishError {
		t.Fatalf("环境变量 true 应覆盖 .env: %v %v", cfg.AbnormalFinishError, err)
	}
	os.Unsetenv("ABNORMAL_FINISH_ERROR")

	cfg := Default()
	cfg.AbnormalFinishError = true
	path = filepath.Join(t.TempDir(), ".env")
	if err := cfg.Save(path); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if !containsLine(string(data), "ABNORMAL_FINISH_ERROR=true") {
		t.Fatalf(".env 缺少 ABNORMAL_FINISH_ERROR=true:\n%s", data)
	}
	if loaded, err := Load(path); err != nil || !loaded.AbnormalFinishError {
		t.Fatalf("保存后读回 AbnormalFinishError=%v err=%v", loaded.AbnormalFinishError, err)
	}

	encoded, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var decoded Config
	if err := json.Unmarshal(encoded, &decoded); err != nil || !decoded.AbnormalFinishError {
		t.Fatalf("JSON 往返 AbnormalFinishError=%v err=%v", decoded.AbnormalFinishError, err)
	}
	var legacy map[string]any
	_ = json.Unmarshal(encoded, &legacy)
	if _, ok := legacy["abnormal_finish_error"]; !ok {
		t.Fatalf("管理接口的 JSON 应带 abnormal_finish_error: %s", encoded)
	}
	delete(legacy, "abnormal_finish_error")
	legacyJSON, _ := json.Marshal(legacy)
	if err := json.Unmarshal(legacyJSON, &decoded); err != nil || decoded.AbnormalFinishError {
		t.Fatalf("旧版 JSON 没有该字段时应按默认关闭: %v %v", decoded.AbnormalFinishError, err)
	}
}
