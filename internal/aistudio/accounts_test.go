package aistudio

import (
	"context"
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
