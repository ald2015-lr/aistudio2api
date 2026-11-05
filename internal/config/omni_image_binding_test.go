package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// TestOmniImageBindingConfig 视频模型带图校验值默认开启，可显式关闭，非布尔值按默认值加载并记入无效项；进程环境变量优先于 .env；
// .env 与管理接口的 JSON 往返不变，旧版 JSON 没有该字段时按默认开启
func TestOmniImageBindingConfig(t *testing.T) {
	t.Setenv("OMNI_IMAGE_BINDING", "")
	os.Unsetenv("OMNI_IMAGE_BINDING")
	if !slices.Contains(configKeys[:], "OMNI_IMAGE_BINDING") {
		t.Fatal("configKeys 缺少 OMNI_IMAGE_BINDING：Save 会丢掉这个键，进程环境变量也覆盖不了它")
	}
	if !Default().OmniImageBinding {
		t.Fatal("OMNI_IMAGE_BINDING 默认应开启")
	}
	for _, test := range []struct {
		content string
		want    bool
		fail    bool
	}{
		{content: "", want: true},
		{content: "OMNI_IMAGE_BINDING=\n", want: true},
		{content: "OMNI_IMAGE_BINDING=false\n", want: false},
		{content: "OMNI_IMAGE_BINDING= true \n", want: true},
		{content: "OMNI_IMAGE_BINDING=0\n", want: false},
		{content: "OMNI_IMAGE_BINDING=off\n", fail: true},
	} {
		path := filepath.Join(t.TempDir(), ".env")
		if err := os.WriteFile(path, []byte(test.content), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err := Load(path)
		if test.fail {
			if requireInvalidKey(t, test.content, cfg, err, "OMNI_IMAGE_BINDING"); !cfg.OmniImageBinding {
				t.Fatalf("%q：无效时应取默认值 true", test.content)
			}
			continue
		}
		if err != nil || cfg.OmniImageBinding != test.want {
			t.Fatalf("%q: OmniImageBinding=%v err=%v，期望 %v", test.content, cfg.OmniImageBinding, err, test.want)
		}
	}
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte("OMNI_IMAGE_BINDING=true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OMNI_IMAGE_BINDING", "false")
	if cfg, err := Load(path); err != nil || cfg.OmniImageBinding {
		t.Fatalf("环境变量 false 应覆盖 .env: %v %v", cfg.OmniImageBinding, err)
	}
	os.Unsetenv("OMNI_IMAGE_BINDING")

	cfg := Default()
	cfg.OmniImageBinding = false
	path = filepath.Join(t.TempDir(), ".env")
	if err := cfg.Save(path); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if !containsLine(string(data), "OMNI_IMAGE_BINDING=false") {
		t.Fatalf(".env 缺少 OMNI_IMAGE_BINDING=false:\n%s", data)
	}
	if loaded, err := Load(path); err != nil || loaded.OmniImageBinding {
		t.Fatalf("保存后读回 OmniImageBinding=%v err=%v", loaded.OmniImageBinding, err)
	}

	encoded, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var decoded Config
	if err := json.Unmarshal(encoded, &decoded); err != nil || decoded.OmniImageBinding {
		t.Fatalf("JSON 往返 OmniImageBinding=%v err=%v", decoded.OmniImageBinding, err)
	}
	var legacy map[string]any
	_ = json.Unmarshal(encoded, &legacy)
	if _, ok := legacy["omni_image_binding"]; !ok {
		t.Fatalf("管理接口的 JSON 应带 omni_image_binding: %s", encoded)
	}
	delete(legacy, "omni_image_binding")
	legacyJSON, _ := json.Marshal(legacy)
	if err := json.Unmarshal(legacyJSON, &decoded); err != nil || !decoded.OmniImageBinding {
		t.Fatalf("旧版 JSON 没有该字段时应按默认开启: %v %v", decoded.OmniImageBinding, err)
	}
}
