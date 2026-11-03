package app

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
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

// TestRuntimeConfigReportsInvalidValues 读取设置时列出 .env 里取值无效、已按默认值生效的项（管理页顶部提示）；在管理页保存后
// .env 里的都变成有效值，只剩进程环境变量里的无效项（标明来源）
func TestRuntimeConfigReportsInvalidValues(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte("INIT_TIMEOUT=abc\nADMIN_PASSWORD=secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	admin := &runtimeAdmin{configPath: path, requests: newRequestRegistry(ctx)}
	value, err := admin.RuntimeConfig(ctx)
	if err != nil {
		t.Fatalf("取值无效时应按默认值读出设置：%v", err)
	}
	if len(value.InvalidConfig) != 1 || value.InvalidConfig[0].Key != "INIT_TIMEOUT" || value.InvalidConfig[0].Value != "abc" ||
		value.InvalidConfig[0].Environment || value.InitTimeout != config.Default().InitTimeout.String() {
		t.Fatalf("无效项 = %+v，INIT_TIMEOUT = %s", value.InvalidConfig, value.InitTimeout)
	}
	t.Setenv("ROUTING_STRATEGY", "random")
	saved, err := admin.UpdateRuntimeConfig(ctx, validRuntimeConfig())
	if err != nil {
		t.Fatal(err)
	}
	if len(saved.InvalidConfig) != 1 || saved.InvalidConfig[0].Key != "ROUTING_STRATEGY" || !saved.InvalidConfig[0].Environment {
		t.Fatalf("保存后应只剩环境变量里的无效项：%+v", saved.InvalidConfig)
	}
	if reloaded, err := config.Load(path); err != nil || reloaded.InitTimeout != config.Default().InitTimeout || reloaded.AdminPassword != "secret" {
		t.Fatalf("保存后 .env 应写入有效值并保留管理密码：%+v %v", reloaded.InitTimeout, err)
	}
}

// TestUpdateRuntimeConfigKeepsUneditedInvalidLines 管理页不编辑的 REQUEST_BODY_LOG 在 .env 里取值无效（按默认值 false 生效）时，
// 保存其他设置沿用 .env 里的原行，不把默认值写回抹掉原值，管理页照旧列出这一项；AUTO_START 等本页不编辑的有效值照常沿用。
// AUTO_START 写错时读不出 .env（启动即报错），读取与保存设置都拒绝，不把 AUTO_START=true 写回
func TestUpdateRuntimeConfigKeepsUneditedInvalidLines(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for _, key := range []string{"AUTO_START", "REQUEST_BODY_LOG", "ADMIN_PASSWORD", "INIT_TIMEOUT"} {
		t.Setenv(key, "")
		os.Unsetenv(key)
	}
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte("AUTO_START=false\nREQUEST_BODY_LOG=on\nINIT_TIMEOUT=abc\nADMIN_PASSWORD=secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	admin := &runtimeAdmin{configPath: path, requests: newRequestRegistry(ctx)}
	saved, err := admin.UpdateRuntimeConfig(ctx, validRuntimeConfig())
	if err != nil {
		t.Fatal(err)
	}
	if len(saved.InvalidConfig) != 1 || saved.InvalidConfig[0].Key != "REQUEST_BODY_LOG" || saved.InvalidConfig[0].Value != "on" {
		t.Fatalf("保存后应只剩本页不编辑、沿用原行的 REQUEST_BODY_LOG：%+v", saved.InvalidConfig)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{"REQUEST_BODY_LOG=on\n", "AUTO_START=false\n", "ADMIN_PASSWORD=secret\n", "INIT_TIMEOUT=" + config.Default().InitTimeout.String() + "\n"} {
		if !strings.Contains(string(data), line) {
			t.Fatalf("保存后的 .env 应含 %q：\n%s", line, data)
		}
	}

	broken := []byte("AUTO_START=off\nADMIN_PASSWORD=secret\n")
	if err := os.WriteFile(path, broken, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.RuntimeConfig(ctx); err == nil || !strings.Contains(err.Error(), "AUTO_START") {
		t.Fatalf("AUTO_START 写错时读取设置应报错：%v", err)
	}
	var operation *adminOperationError
	if _, err := admin.UpdateRuntimeConfig(ctx, validRuntimeConfig()); !errors.As(err, &operation) || operation.code != "config_unreadable" {
		t.Fatalf("AUTO_START 写错时保存应拒绝：%v", err)
	}
	if data, _ := os.ReadFile(path); string(data) != string(broken) {
		t.Fatalf(".env 被覆盖: %q", data)
	}
}
