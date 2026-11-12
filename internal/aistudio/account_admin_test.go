package aistudio

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// diskAccountEnabled 读取 account.json 的 enabled 字段
func diskAccountEnabled(t *testing.T, account *Account) bool {
	t.Helper()
	content, err := os.ReadFile(account.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	var value AccountConfig
	if err := json.Unmarshal(content, &value); err != nil {
		t.Fatal(err)
	}
	return value.Enabled
}

// TestSetEnabledWithoutLease 管理页启停不取租约：账户有进行中的请求时停用立即写回 account.json 并生效（不再发出新的租约，
// 进行中的请求照常结束），启用按登录 Cookie 恢复为就绪；已经是目标状态时不写文件
func TestSetEnabledWithoutLease(t *testing.T) {
	pool := testPoolWithAccount(t, "alice@example.com")
	account, err := pool.Account("alice@example.com")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	inflight, err := pool.AcquireFor(ctx, AccountSelection{AccountID: account.ID})
	if err != nil {
		t.Fatal(err)
	}
	// 前提：停用之前同一个选择能取到租约（每账户并发为 2，进行中的请求只占一个）
	if lease, _, err := pool.TryAcquireFor(ctx, AccountSelection{AllowedAccountIDs: []string{account.ID}}); err != nil || lease == nil {
		t.Fatalf("停用之前应能取得租约：%v", err)
	} else if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, changed, err := pool.SetEnabled(account.ID, false)
		if err == nil && !changed {
			err = errors.New("停用应有变化")
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("有进行中的请求时停用不应等待")
	}
	status, _ := pool.StatusOf(account.ID)
	if status.Enabled || status.State != AccountDisabled || diskAccountEnabled(t, account) {
		t.Fatalf("停用应立即生效并写回 account.json：%+v", status)
	}
	tryCtx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	if lease, _, err := pool.TryAcquireFor(tryCtx, AccountSelection{AllowedAccountIDs: []string{account.ID}}); err == nil && lease != nil {
		_ = lease.Release()
		t.Fatal("停用后不应再发出不固定账户的租约")
	}
	if err := inflight.Release(); err != nil {
		t.Fatalf("进行中的请求应照常结束: %v", err)
	}
	if _, changed, err := pool.SetEnabled(account.ID, false); err != nil || changed {
		t.Fatalf("已经停用时不应再写：changed=%t err=%v", changed, err)
	}
	if _, changed, err := pool.SetEnabled(account.ID, true); err != nil || !changed {
		t.Fatalf("启用应有变化：changed=%t err=%v", changed, err)
	}
	status, _ = pool.StatusOf(account.ID)
	if !status.Enabled || status.State != AccountReady || !diskAccountEnabled(t, account) {
		t.Fatalf("启用后应按登录 Cookie 恢复为就绪：%+v", status)
	}
}

// TestSetEnabledRespectsFileLease 另一个进程持有账户文件锁时不写 account.json、返回 ErrAccountLeased；
// 账户目录已被删除时返回 ErrAccountNotFound，不重新创建目录
func TestSetEnabledRespectsFileLease(t *testing.T) {
	pool := testPoolWithAccount(t, "alice@example.com")
	account, err := pool.Account("alice@example.com")
	if err != nil {
		t.Fatal(err)
	}
	external, _, err := acquireAccountFileLease(account.StoragePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := pool.SetEnabled(account.ID, false); !errors.Is(err, ErrAccountLeased) {
		_ = external.Unlock()
		t.Fatalf("另一个进程持有文件锁时应返回 ErrAccountLeased，得到 %v", err)
	}
	if err := external.Unlock(); err != nil {
		t.Fatal(err)
	}
	if status, _ := pool.StatusOf(account.ID); !status.Enabled || !diskAccountEnabled(t, account) {
		t.Fatal("写入失败时不应改变启用状态")
	}
	if err := os.RemoveAll(account.Directory); err != nil {
		t.Fatal(err)
	}
	if _, _, err := pool.SetEnabled(account.ID, false); !errors.Is(err, ErrAccountNotFound) {
		t.Fatalf("账户目录已删除时应返回 ErrAccountNotFound，得到 %v", err)
	}
	if _, err := os.Stat(account.Directory); !os.IsNotExist(err) {
		t.Fatal("不应重新创建已删除的账户目录")
	}
}

// TestSetTransitionShownInStatus 启停的收尾阶段与进行中的请求数出现在账户状态里，清除后不再显示
func TestSetTransitionShownInStatus(t *testing.T) {
	pool := testPoolWithAccount(t, "alice@example.com")
	lease, err := pool.AcquireFor(context.Background(), AccountSelection{AccountID: "alice@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	if status, _ := pool.StatusOf("alice@example.com"); status.Transition != "" || status.ActiveRequests != 0 {
		t.Fatalf("没有收尾时不应显示阶段与请求数：%+v", status)
	}
	if !pool.SetTransition("alice@example.com", AccountDraining) {
		t.Fatal("账户存在时应返回 true")
	}
	if status, _ := pool.StatusOf("alice@example.com"); status.Transition != AccountDraining || status.ActiveRequests != 1 {
		t.Fatalf("收尾阶段应为 draining、进行中的请求 1 个：%+v", status)
	}
	// 请求结束、没有独占操作：下一步就是关闭 Worker，按 closing 显示；有独占操作（登录、验证等）时仍为 draining、请求数 0
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
	if status, _ := pool.StatusOf("alice@example.com"); status.Transition != AccountClosing || status.ActiveRequests != 0 {
		t.Fatalf("没有要等的请求与独占操作时应显示 closing：%+v", status)
	}
	exclusive, err := pool.AcquireAccount(context.Background(), "alice@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if status, _ := pool.StatusOf("alice@example.com"); status.Transition != AccountDraining || status.ActiveRequests != 0 {
		t.Fatalf("有独占操作时应仍为 draining、请求数 0：%+v", status)
	}
	if err := exclusive.Release(); err != nil {
		t.Fatal(err)
	}
	pool.SetTransition("alice@example.com", "")
	if status, _ := pool.StatusOf("alice@example.com"); status.Transition != "" || status.ActiveRequests != 0 {
		t.Fatalf("清除后不应显示阶段与请求数：%+v", status)
	}
	if pool.SetTransition("nobody@example.com", AccountSyncing) {
		t.Fatal("账户不存在时应返回 false")
	}
}

// TestCreateRejectsExistingAccount 同一邮箱的账户目录已经存在时，在写临时目录之前返回 ErrAccountExists（原先到改名时才报 file exists），
// 不留下临时目录，已有账户的文件不变
func TestCreateRejectsExistingAccount(t *testing.T) {
	root := t.TempDir()
	store := NewAccountStore(root)
	account, publish, err := store.Create(DefaultAccountConfig("alice@example.com"), testStorageState("OLD"))
	if err != nil {
		t.Fatal(err)
	}
	if err := publish.Release(); err != nil {
		t.Fatal(err)
	}
	_, _, err = store.Create(DefaultAccountConfig("Alice@Example.com"), testStorageState("NEW"))
	if !errors.Is(err, ErrAccountExists) {
		t.Fatalf("已存在的账户应返回 ErrAccountExists，得到 %v", err)
	}
	if value := diskSAPISID(t, account.StoragePath); value != "OLD" {
		t.Fatalf("已有账户的认证文件不应改变：%q", value)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if matched, _ := filepath.Match(".account-*.tmp", entry.Name()); matched {
			t.Fatalf("不应留下临时目录：%s", entry.Name())
		}
	}
}
