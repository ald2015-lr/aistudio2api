package aistudio

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func testStorageState(value string) StorageState {
	cookies := make([]StateCookie, 0, 3)
	for _, name := range []string{"SAPISID", "__Secure-1PAPISID", "__Secure-3PAPISID"} {
		cookies = append(cookies, StateCookie{
			Name: name, Value: value, Domain: ".google.com", Path: "/", Expires: -1, Secure: true, SameSite: "None",
		})
	}
	return StorageState{Cookies: cookies, Origins: []StorageOrigin{}}
}

func testPoolWithAccount(t *testing.T, id string) *AccountPool {
	t.Helper()
	store := NewAccountStore(t.TempDir())
	pool := NewAccountPool(nil, 2)
	account, publish, err := store.Create(DefaultAccountConfig(id), testStorageState("OLD"))
	if err != nil {
		t.Fatal(err)
	}
	if err := pool.Add(account); err != nil {
		t.Fatal(err)
	}
	if err := publish.Release(); err != nil {
		t.Fatal(err)
	}
	return pool
}

func diskSAPISID(t *testing.T, path string) string {
	t.Helper()
	state, err := LoadStorageState(path)
	if err != nil {
		t.Fatal(err)
	}
	value, _ := state.CookieValue("SAPISID", aiStudioOrigin+"/", time.Now())
	return value
}

// TestStaleLeaseDoesNotOverwriteRefreshedCookies 续签前开始的租约写回旧 Cookie 时不能覆盖续签结果
func TestStaleLeaseDoesNotOverwriteRefreshedCookies(t *testing.T) {
	pool := testPoolWithAccount(t, "alice@example.com")
	ctx := context.Background()
	refresher, err := pool.AcquireFor(ctx, AccountSelection{AccountID: "alice@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	defer refresher.Release()
	stale, err := pool.AcquireFor(ctx, AccountSelection{AccountID: "alice@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	defer stale.Release()

	end, ok := refresher.BeginAuthRefresh()
	if !ok {
		t.Fatal("BeginAuthRefresh 被拒绝")
	}
	inUpdate := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer end()
		err := refresher.RefreshStorageState(func(state *StorageState) error {
			close(inUpdate)
			time.Sleep(100 * time.Millisecond)
			state.Cookies = testStorageState("NEW").Cookies
			return nil
		}, func() (func(bool), error) { return func(bool) {}, nil })
		if err != nil {
			t.Error(err)
		}
	}()
	<-inUpdate
	// 并发请求在续签前导出的浏览器 Cookie，在续签提交后才拿到 storageMu
	if err := stale.ReplaceCookies(testStorageState("OLD").Cookies); err != nil {
		t.Fatal(err)
	}
	if err := stale.MergeSetCookieHeaders([]string{"SAPISID=OLD2; Domain=.google.com; Path=/; Secure"}, aiStudioOrigin+"/", time.Now()); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	if got := diskSAPISID(t, refresher.Account().StoragePath); got != "NEW" {
		t.Fatalf("续签后的 SAPISID 被覆盖为 %q", got)
	}

	// 续签之后开始的租约照常写回（每号并发为 2，先释放旧租约腾出槽位）
	if err := stale.Release(); err != nil {
		t.Fatal(err)
	}
	fresh, err := pool.AcquireFor(ctx, AccountSelection{AccountID: "alice@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Release()
	if err := fresh.ReplaceCookies(testStorageState("ROTATED").Cookies); err != nil {
		t.Fatal(err)
	}
	if got := diskSAPISID(t, fresh.Account().StoragePath); got != "ROTATED" {
		t.Fatalf("续签后开始的租约没有写回 Cookie，SAPISID=%q", got)
	}
}

// TestReadRuntimeTolerant 运行态文件含未知字段时照常读取；损坏时加载账户按空运行态继续且不移动文件，
// 持锁读写时备份并用内存中的运行态重写
func TestReadRuntimeTolerant(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, runtimeStateName)
	if err := os.WriteFile(path, []byte(`{"cooldowns":{},"future_field":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	state, err := readRuntime(path)
	if err != nil || state.Cooldowns == nil {
		t.Fatalf("未知字段: state=%+v err=%v", state, err)
	}
	if err := os.WriteFile(path, []byte(`{"cooldowns":`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readRuntime(path); !errors.Is(err, errRuntimeCorrupt) {
		t.Fatalf("损坏文件应返回 errRuntimeCorrupt: %v", err)
	}
	state, err = readRuntimeOrEmpty(path)
	if err != nil || state.Cooldowns == nil || len(state.Cooldowns) != 0 {
		t.Fatalf("加载时损坏文件: state=%+v err=%v", state, err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("未持锁时不应移动损坏文件: %v", err)
	}

	memory := emptyRuntimeState()
	memory.Resources["files/abc"] = ResourceBinding{}
	state, err = readRuntimeLocked(path, memory)
	if err != nil || len(state.Resources) != 1 {
		t.Fatalf("持锁时应沿用内存运行态: state=%+v err=%v", state, err)
	}
	if _, err := os.Stat(path + ".corrupt"); err != nil {
		t.Fatalf("损坏文件没有备份: %v", err)
	}
	rewritten, err := readRuntime(path)
	if err != nil || len(rewritten.Resources) != 1 {
		t.Fatalf("应按内存运行态重写文件: state=%+v err=%v", rewritten, err)
	}
}

// TestLoginSummaryReportsMissingFiles 文件缺失被标为不可用的账户在轻量状态里有标记，目录监视据此在文件恢复后重新同步
func TestLoginSummaryReportsMissingFiles(t *testing.T) {
	pool := testPoolWithAccount(t, "alice@example.com")
	account, err := pool.Account("alice@example.com")
	if err != nil {
		t.Fatal(err)
	}
	pool.markStaleAccountUnavailable(account)
	summaries := pool.LoginSummaries()
	if len(summaries) != 1 || !summaries[0].FilesMissing || summaries[0].Directory != account.Directory {
		t.Fatalf("summaries=%+v", summaries)
	}
	if err := pool.MarkUnavailable("alice@example.com", "其他原因"); err != nil {
		t.Fatal(err)
	}
	if pool.LoginSummaries()[0].FilesMissing {
		t.Fatal("其他原因的不可用不应标记为文件缺失")
	}
}

// TestStaleLeaseDoesNotOverwriteSavedLogin 保存新登录状态之后，之前开始的租约写回旧 Cookie 被丢弃
func TestStaleLeaseDoesNotOverwriteSavedLogin(t *testing.T) {
	pool := testPoolWithAccount(t, "alice@example.com")
	ctx := context.Background()
	stale, err := pool.AcquireFor(ctx, AccountSelection{AccountID: "alice@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	defer stale.Release()
	saver, err := pool.AcquireFor(ctx, AccountSelection{AccountID: "alice@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if err := saver.SaveStorageState(testStorageState("RELOGIN")); err != nil {
		t.Fatal(err)
	}
	if err := saver.Release(); err != nil {
		t.Fatal(err)
	}
	if err := stale.ReplaceCookies(testStorageState("OLD").Cookies); err != nil {
		t.Fatal(err)
	}
	if got := diskSAPISID(t, stale.Account().StoragePath); got != "RELOGIN" {
		t.Fatalf("重新登录后的 SAPISID 被旧租约覆盖为 %q", got)
	}
}

// TestCorruptRuntimeKeepsInMemoryBindings 运行中运行态文件损坏时保留内存里正确的资源绑定，并把文件重写回来
func TestCorruptRuntimeKeepsInMemoryBindings(t *testing.T) {
	pool := testPoolWithAccount(t, "alice@example.com")
	if err := pool.BindResource("files/abc", "alice@example.com"); err != nil {
		t.Fatal(err)
	}
	account, err := pool.Account("alice@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(account.RuntimePath, []byte(`{"resources":{"files/abc":{`), 0o600); err != nil {
		t.Fatal(err)
	}
	lease, err := pool.AcquireAccount(context.Background(), "alice@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
	pool.mu.Lock()
	owner := pool.resources["files/abc"]
	pool.mu.Unlock()
	if owner != "alice@example.com" {
		t.Fatalf("读到损坏的运行态文件后内存绑定丢失 owner=%q", owner)
	}
	if state, err := readRuntime(account.RuntimePath); err != nil || len(state.Resources) != 1 {
		t.Fatalf("运行态文件应按内存重写: state=%+v err=%v", state, err)
	}
}
