package aistudio

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gofrs/flock"
)

// writeTestAccountDirectory 在 root 下创建账户目录：account.json 使用给定原文，认证文件带登录签名 Cookie
func writeTestAccountDirectory(t *testing.T, root string, id string, config string) string {
	t.Helper()
	directory := filepath.Join(root, id)
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, accountConfigName), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteStorageState(filepath.Join(directory, storageStateName), testStorageState("V")); err != nil {
		t.Fatal(err)
	}
	return directory
}

// TestReadAccountConfigEnabledDefault account.json 缺少 enabled 字段时按启用载入（就绪），显式 false 仍为停用
func TestReadAccountConfigEnabledDefault(t *testing.T) {
	root := t.TempDir()
	missing := writeTestAccountDirectory(t, root, "missing@example.com",
		`{"label":"missing@example.com","proxy":"","locale":"en-US","timezone":"UTC"}`)
	explicit := writeTestAccountDirectory(t, root, "off@example.com",
		`{"label":"off@example.com","enabled":false,"proxy":"","locale":"en-US","timezone":"UTC"}`)

	account, err := loadAccount(missing)
	if err != nil {
		t.Fatal(err)
	}
	if !account.Config.Enabled || account.State != AccountReady {
		t.Fatalf("缺少 enabled 字段：enabled=%t state=%s，期望启用且就绪", account.Config.Enabled, account.State)
	}
	account, err = loadAccount(explicit)
	if err != nil {
		t.Fatal(err)
	}
	if account.Config.Enabled || account.State != AccountDisabled {
		t.Fatalf("显式 enabled=false：enabled=%t state=%s，期望停用", account.Config.Enabled, account.State)
	}
}

// TestEnableLoadedAccount 载入后改为启用：写回 account.json（其余字段不变）并按登录 Cookie 推导为就绪；
// 另一个进程持有账户租约时不写文件、返回 ErrAccountLeased，但内存仍为启用；目录已删除时不重新创建
func TestEnableLoadedAccount(t *testing.T) {
	root := t.TempDir()
	directory := writeTestAccountDirectory(t, root, "a@example.com",
		`{"label":"a@example.com","enabled":false,"proxy":"http://127.0.0.1:8080","locale":"ja-JP","timezone":"Asia/Tokyo"}`)
	account, err := loadAccount(directory)
	if err != nil {
		t.Fatal(err)
	}
	if err := EnableLoadedAccount(account); err != nil {
		t.Fatal(err)
	}
	if !account.Config.Enabled || account.State != AccountReady {
		t.Fatalf("启用后 enabled=%t state=%s，期望启用且就绪", account.Config.Enabled, account.State)
	}
	saved, err := readAccountConfig(account.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if !saved.Enabled || saved.Proxy != "http://127.0.0.1:8080" || saved.Locale != "ja-JP" || saved.Timezone != "Asia/Tokyo" {
		t.Fatalf("写回的 account.json = %+v，期望只改 enabled", saved)
	}

	// 另一个进程持有账户租约
	busyDirectory := writeTestAccountDirectory(t, root, "busy@example.com",
		`{"label":"busy@example.com","enabled":false,"proxy":"","locale":"en-US","timezone":"UTC"}`)
	busy, err := loadAccount(busyDirectory)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, ".leases"), 0o700); err != nil {
		t.Fatal(err)
	}
	external := flock.New(filepath.Join(root, ".leases", "busy@example.com.lock"))
	if locked, err := external.TryLock(); err != nil || !locked {
		t.Fatalf("模拟其他进程的租约失败：locked=%t err=%v", locked, err)
	}
	defer external.Unlock()
	if err := EnableLoadedAccount(busy); !errors.Is(err, ErrAccountLeased) {
		t.Fatalf("租约被占用：err=%v，期望 ErrAccountLeased", err)
	}
	if !busy.Config.Enabled || busy.State != AccountReady {
		t.Fatal("租约被占用时内存中仍应启用")
	}
	content, err := os.ReadFile(busy.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), `"enabled":false`) {
		t.Fatalf("租约被占用时不应写回 account.json：%s", content)
	}

	// 目录在载入后被删除
	goneDirectory := writeTestAccountDirectory(t, root, "gone@example.com",
		`{"label":"gone@example.com","enabled":false,"proxy":"","locale":"en-US","timezone":"UTC"}`)
	gone, err := loadAccount(goneDirectory)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(goneDirectory); err != nil {
		t.Fatal(err)
	}
	if err := EnableLoadedAccount(gone); !errors.Is(err, ErrAccountNotFound) {
		t.Fatalf("目录已删除：err=%v，期望 ErrAccountNotFound", err)
	}
	if _, err := os.Stat(goneDirectory); !os.IsNotExist(err) {
		t.Fatalf("目录已删除时不应重新创建：%v", err)
	}
}
