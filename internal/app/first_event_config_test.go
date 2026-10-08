package app

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/config"
)

// TestUpdateRuntimeConfigFirstEventTimeout 管理页面保存首事件超时：正数时长与 0 写入 .env，旧版页面不带该字段时沿用现值，
// 负数或不小于请求超时返回 400 invalid_config；该字段可热更新
func TestUpdateRuntimeConfigFirstEventTimeout(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte("FIRST_EVENT_TIMEOUT=30s\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	admin := &runtimeAdmin{configPath: path, requests: newRequestRegistry(ctx)}
	load := func() time.Duration {
		t.Helper()
		saved, err := config.Load(path)
		if err != nil {
			t.Fatal(err)
		}
		return saved.FirstEventTimeout
	}

	legacy := validRuntimeConfig()
	legacy.FirstEventTimeout = ""
	if _, err := admin.UpdateRuntimeConfig(ctx, legacy); err != nil || load() != 30*time.Second {
		t.Fatalf("旧版页面不带该字段时应沿用现值：%s err=%v", load(), err)
	}
	value := validRuntimeConfig()
	value.FirstEventTimeout = "90s"
	saved, err := admin.UpdateRuntimeConfig(ctx, value)
	if err != nil || load() != 90*time.Second || saved.FirstEventTimeout != "1m30s" {
		t.Fatalf("保存 90s：返回 %q，.env %s err=%v", saved.FirstEventTimeout, load(), err)
	}
	value.FirstEventTimeout = "0s"
	if _, err := admin.UpdateRuntimeConfig(ctx, value); err != nil || load() != 0 {
		t.Fatalf("保存 0s 应关闭：%s err=%v", load(), err)
	}
	for _, invalid := range []string{"-5s", "abc", "5m"} {
		value.FirstEventTimeout = invalid
		_, err := admin.UpdateRuntimeConfig(ctx, value)
		var operation *adminOperationError
		if !errors.As(err, &operation) || operation.status != http.StatusBadRequest || operation.code != "invalid_config" {
			t.Fatalf("%q 应返回 400 invalid_config，得到 %v", invalid, err)
		}
	}

	active := config.Default()
	changed := active
	changed.FirstEventTimeout = time.Minute
	if liveFieldsEqual(changed, active) || requiresRebuild(changed, active) {
		t.Fatal("首事件超时变化应按热更新处理")
	}
	dto := runtimeConfigDTO(changed)
	if sameDataConfig(dto, active, dataConfigOverrides{}) || !sameDataConfig(dto, changed, dataConfigOverrides{}) {
		t.Fatal("生效配置比较应包含首事件超时")
	}
}
