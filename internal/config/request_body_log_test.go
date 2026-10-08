package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestRequestBodyLogConfig 请求正文记录默认关闭，可显式开启，非布尔值报错，保存后往返不变
func TestRequestBodyLogConfig(t *testing.T) {
	t.Setenv("REQUEST_BODY_LOG", "")
	os.Unsetenv("REQUEST_BODY_LOG")
	if Default().RequestBodyLog {
		t.Fatal("REQUEST_BODY_LOG 默认应关闭")
	}
	for _, test := range []struct {
		content string
		want    bool
		fail    bool
	}{
		{content: "", want: false},
		{content: "REQUEST_BODY_LOG=\n", want: false},
		{content: "REQUEST_BODY_LOG=true\n", want: true},
		{content: "REQUEST_BODY_LOG=yes\n", fail: true},
	} {
		path := filepath.Join(t.TempDir(), ".env")
		if err := os.WriteFile(path, []byte(test.content), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err := Load(path)
		if test.fail {
			if err == nil {
				t.Fatalf("%q 应报错", test.content)
			}
			continue
		}
		if err != nil || cfg.RequestBodyLog != test.want {
			t.Fatalf("%q: RequestBodyLog=%v err=%v，期望 %v", test.content, cfg.RequestBodyLog, err, test.want)
		}
	}
	cfg := Default()
	cfg.RequestBodyLog = true
	path := filepath.Join(t.TempDir(), ".env")
	if err := cfg.Save(path); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path)
	if err != nil || !loaded.RequestBodyLog {
		t.Fatalf("保存后读回 RequestBodyLog=%v err=%v", loaded.RequestBodyLog, err)
	}
}
