package app

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/Mag1cFall/AIStudio2API/internal/api"
	"github.com/Mag1cFall/AIStudio2API/internal/config"
)

func validRuntimeConfig() api.RuntimeConfig {
	cfg := config.Default()
	value := runtimeConfigDTO(cfg)
	return value
}

// TestUpdateRuntimeConfigErrors 无效配置返回 400 invalid_config；现有 .env 读不出时拒绝保存，不用默认值覆盖管理密码
func TestUpdateRuntimeConfigErrors(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	path := filepath.Join(t.TempDir(), ".env")
	admin := &runtimeAdmin{configPath: path, requests: newRequestRegistry(ctx)}

	invalid := validRuntimeConfig()
	invalid.RequestTimeout = "abc"
	_, err := admin.UpdateRuntimeConfig(ctx, invalid)
	var operation *adminOperationError
	if !errors.As(err, &operation) || operation.status != http.StatusBadRequest || operation.code != "invalid_config" {
		t.Fatalf("无效时长: err=%v", err)
	}

	broken := []byte("ADMIN_PASSWORD=secret\nnot a valid line\n")
	if err := os.WriteFile(path, broken, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = admin.UpdateRuntimeConfig(ctx, validRuntimeConfig())
	if !errors.As(err, &operation) || operation.code != "config_unreadable" {
		t.Fatalf("读不出 .env: err=%v", err)
	}
	if data, _ := os.ReadFile(path); string(data) != string(broken) {
		t.Fatalf(".env 被覆盖: %q", data)
	}

	if err := os.WriteFile(path, []byte("ADMIN_PASSWORD=secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.UpdateRuntimeConfig(ctx, validRuntimeConfig()); err != nil {
		t.Fatal(err)
	}
	saved, err := config.Load(path)
	if err != nil || saved.AdminPassword != "secret" {
		t.Fatalf("保存后管理密码=%q err=%v", saved.AdminPassword, err)
	}
}
