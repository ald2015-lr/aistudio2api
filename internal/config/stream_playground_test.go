package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// TestStreamPlaygroundModelsConfig 流式优先 Playground 的模型默认为空；逗号或空白分隔，去掉 models/ 前缀并去重；
// 进程环境变量优先于 .env
func TestStreamPlaygroundModelsConfig(t *testing.T) {
	t.Setenv("STREAM_PLAYGROUND_MODELS", "")
	os.Unsetenv("STREAM_PLAYGROUND_MODELS")
	if models := Default().StreamPlaygroundModels; len(models) != 0 {
		t.Fatalf("默认值 = %v，期望为空", models)
	}
	for _, test := range []struct {
		content string
		want    []string
	}{
		{content: "", want: nil},
		{content: "STREAM_PLAYGROUND_MODELS=\n", want: nil},
		{content: "STREAM_PLAYGROUND_MODELS=model-a\n", want: []string{"model-a"}},
		{content: "STREAM_PLAYGROUND_MODELS=\" model-a, models/model-b，model-a  model-c \"\n", want: []string{"model-a", "model-b", "model-c"}},
	} {
		path := filepath.Join(t.TempDir(), ".env")
		if err := os.WriteFile(path, []byte(test.content), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err := Load(path)
		if err != nil || !slices.Equal(cfg.StreamPlaygroundModels, test.want) {
			t.Fatalf("%q：StreamPlaygroundModels = %v err=%v，期望 %v", test.content, cfg.StreamPlaygroundModels, err, test.want)
		}
	}

	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte("STREAM_PLAYGROUND_MODELS=model-a\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("STREAM_PLAYGROUND_MODELS", "model-env")
	cfg, err := Load(path)
	if err != nil || !slices.Equal(cfg.StreamPlaygroundModels, []string{"model-env"}) {
		t.Fatalf("环境变量应覆盖 .env：%v err=%v", cfg.StreamPlaygroundModels, err)
	}
}

// TestStreamPlaygroundModelsValidate 模型 ID 不能为空、不能包含空白或逗号、不能重复
func TestStreamPlaygroundModelsValidate(t *testing.T) {
	for _, models := range [][]string{{""}, {"model a"}, {"model-a,model-b"}, {"model-a", "model-a"}} {
		cfg := Default()
		cfg.StreamPlaygroundModels = models
		if err := cfg.Validate(); err == nil {
			t.Fatalf("%q 应校验失败", models)
		}
	}
	cfg := Default()
	cfg.StreamPlaygroundModels = []string{"model-a", "Model-B"}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("有效列表校验失败: %v", err)
	}
}

// TestStreamPlaygroundModelsRoundTrip 保存后再读取、JSON 往返都保留列表；清空后保存为空；旧版 JSON 没有该字段时为空
func TestStreamPlaygroundModelsRoundTrip(t *testing.T) {
	for _, key := range configKeys {
		t.Setenv(key, "")
		os.Unsetenv(key)
	}
	cfg := Default()
	cfg.StreamPlaygroundModels = []string{"model-a", "model-b"}
	path := filepath.Join(t.TempDir(), ".env")
	if err := cfg.Save(path); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path)
	if err != nil || !slices.Equal(loaded.StreamPlaygroundModels, cfg.StreamPlaygroundModels) {
		t.Fatalf(".env 往返 = %v err=%v，期望 %v", loaded.StreamPlaygroundModels, err, cfg.StreamPlaygroundModels)
	}
	cleared := loaded
	cleared.StreamPlaygroundModels = nil
	if err := cleared.Save(path); err != nil {
		t.Fatal(err)
	}
	if loaded, err = Load(path); err != nil || len(loaded.StreamPlaygroundModels) != 0 {
		t.Fatalf("清空后 .env 往返 = %v err=%v，期望为空", loaded.StreamPlaygroundModels, err)
	}

	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var decoded Config
	if err := json.Unmarshal(data, &decoded); err != nil || !slices.Equal(decoded.StreamPlaygroundModels, cfg.StreamPlaygroundModels) {
		t.Fatalf("JSON 往返 = %v err=%v，期望 %v", decoded.StreamPlaygroundModels, err, cfg.StreamPlaygroundModels)
	}
	var legacy map[string]any
	if err := json.Unmarshal(data, &legacy); err != nil {
		t.Fatal(err)
	}
	delete(legacy, "stream_playground_models")
	legacyData, _ := json.Marshal(legacy)
	var legacyDecoded Config
	if err := json.Unmarshal(legacyData, &legacyDecoded); err != nil || len(legacyDecoded.StreamPlaygroundModels) != 0 {
		t.Fatalf("旧版 JSON：StreamPlaygroundModels = %v err=%v，期望为空", legacyDecoded.StreamPlaygroundModels, err)
	}
}
