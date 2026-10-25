package aistudio

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
)

// localStorageTestPool 创建一个账户的号池，认证文件里保存 AI Studio 的旧 localStorage 与另一个 origin 的数据
func localStorageTestPool(t *testing.T, id string) *AccountPool {
	t.Helper()
	store := NewAccountStore(t.TempDir())
	pool := NewAccountPool(nil, 2)
	t.Cleanup(pool.WaitBackground)
	state := testStorageState("OLD")
	state.Origins = []StorageOrigin{
		{Origin: "https://accounts.google.com", LocalStorage: []StorageItem{{Name: "keep", Value: "1"}}},
		{Origin: aiStudioOrigin, LocalStorage: []StorageItem{
			{Name: "aiStudioUserPreference", Value: `{"format":"old"}`},
			{Name: "xap-tour", Value: "1"},
		}},
		{Origin: "https://one.google.com", LocalStorage: []StorageItem{{Name: "other", Value: "2"}}},
	}
	account, publish, err := store.Create(DefaultAccountConfig(id), state)
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

// originsText 把 origins 写成便于比较的文本
func originsText(origins []StorageOrigin) string {
	parts := make([]string, 0, len(origins))
	for _, origin := range origins {
		items := make([]string, 0, len(origin.LocalStorage))
		for _, item := range origin.LocalStorage {
			items = append(items, item.Name+"="+item.Value)
		}
		parts = append(parts, origin.Origin+"["+strings.Join(items, ",")+"]")
	}
	return strings.Join(parts, " ")
}

func diskOrigins(t *testing.T, pool *AccountPool, id string) []StorageOrigin {
	t.Helper()
	account, err := pool.Account(id)
	if err != nil {
		t.Fatal(err)
	}
	state, err := LoadStorageState(account.StoragePath)
	if err != nil {
		t.Fatal(err)
	}
	return state.Origins
}

// TestMergeBrowserLocalStorage Worker 启动后写回浏览器 localStorage：替换该 origin 的全部项、其他 origin 原样保留且顺序不变，
// 内存同步更新；认证代际变化（期间保存过新登录或续签）时不写；与磁盘相同、浏览器没有导出任何项时不写
func TestMergeBrowserLocalStorage(t *testing.T) {
	const id = "storage@example.com"
	pool := localStorageTestPool(t, id)
	before := originsText(diskOrigins(t, pool, id))
	generation := pool.AuthGeneration(id)
	fresh := StorageOrigin{Origin: aiStudioOrigin, LocalStorage: []StorageItem{
		{Name: "aiStudioUserPreference", Value: `{"format":"new"}`},
		{Name: "selectedProject", Value: "p"},
	}}
	if written, err := pool.MergeBrowserLocalStorage(id, generation+1, fresh); err != nil || written {
		t.Fatalf("代际不符时不应写入: written=%t err=%v", written, err)
	}
	if got := originsText(diskOrigins(t, pool, id)); got != before {
		t.Fatalf("代际不符时磁盘 = %s，期望不变", got)
	}
	written, err := pool.MergeBrowserLocalStorage(id, generation, fresh)
	if err != nil || !written {
		t.Fatalf("代际相同应写入: written=%t err=%v", written, err)
	}
	want := `https://accounts.google.com[keep=1] https://aistudio.google.com[aiStudioUserPreference={"format":"new"},selectedProject=p] https://one.google.com[other=2]`
	if got := originsText(diskOrigins(t, pool, id)); got != want {
		t.Fatalf("写回后磁盘 = %s\n期望 %s", got, want)
	}
	account, err := pool.Account(id)
	if err != nil {
		t.Fatal(err)
	}
	if got := originsText(account.StorageState.Origins); got != want {
		t.Fatalf("写回后内存 = %s，期望与磁盘相同", got)
	}
	if values := diskSAPISID(t, account.StoragePath); values != "OLD" {
		t.Fatalf("写回 localStorage 不应改动 Cookie，SAPISID = %q", values)
	}
	// 写回的是浏览器导出项的副本：调用方之后改动自己的切片不影响账户状态
	fresh.LocalStorage[1].Value = "changed"
	if got := originsText(account.StorageState.Origins); got != want {
		t.Fatalf("调用方改动切片后内存 = %s", got)
	}
	fresh.LocalStorage[1].Value = "p"
	if written, err := pool.MergeBrowserLocalStorage(id, generation, fresh); err != nil || written {
		t.Fatalf("与磁盘相同时不应再写: written=%t err=%v", written, err)
	}
	if written, err := pool.MergeBrowserLocalStorage(id, generation, StorageOrigin{Origin: aiStudioOrigin}); err != nil || written {
		t.Fatalf("浏览器没有导出任何项时不应写: written=%t err=%v", written, err)
	}
	if got := originsText(diskOrigins(t, pool, id)); got != want {
		t.Fatalf("空导出后磁盘 = %s，期望不变", got)
	}
}

// TestMergeBrowserLocalStorageLimit 总大小超过 512 KB 时不写回，错误只写大小、不含值；正好等于上限时照常写
func TestMergeBrowserLocalStorageLimit(t *testing.T) {
	const id = "large@example.com"
	pool := localStorageTestPool(t, id)
	before := originsText(diskOrigins(t, pool, id))
	generation := pool.AuthGeneration(id)
	secret := "SECRET-" + strings.Repeat("v", BrowserLocalStorageLimit)
	_, err := pool.MergeBrowserLocalStorage(id, generation, StorageOrigin{
		Origin: aiStudioOrigin, LocalStorage: []StorageItem{{Name: "big", Value: secret}},
	})
	if !errors.Is(err, ErrBrowserLocalStorageTooLarge) {
		t.Fatalf("超过上限应返回 ErrBrowserLocalStorageTooLarge，实际 %v", err)
	}
	if strings.Contains(err.Error(), "SECRET-") || len(err.Error()) > 300 {
		t.Fatalf("错误文案不应含值: %.200s", err.Error())
	}
	if got := originsText(diskOrigins(t, pool, id)); got != before {
		t.Fatal("超过上限时不应写入")
	}
	exact := StorageOrigin{Origin: aiStudioOrigin, LocalStorage: []StorageItem{
		{Name: "k", Value: strings.Repeat("v", BrowserLocalStorageLimit-1)},
	}}
	if written, err := pool.MergeBrowserLocalStorage(id, generation, exact); err != nil || !written {
		t.Fatalf("正好等于上限时应写入: written=%t err=%v", written, err)
	}
}

// TestReplaceStorageOrigin 同 origin 的项放在原来第一次出现的位置，重复的合并掉；没有时追加到末尾
func TestReplaceStorageOrigin(t *testing.T) {
	fresh := StorageOrigin{Origin: "https://b", LocalStorage: []StorageItem{{Name: "n", Value: "new"}}}
	origins := []StorageOrigin{
		{Origin: "https://a"},
		{Origin: "https://b", LocalStorage: []StorageItem{{Name: "n", Value: "old"}}},
		{Origin: "https://c"},
		{Origin: "https://b", LocalStorage: []StorageItem{{Name: "dup", Value: "x"}}},
	}
	if got := originsText(replaceStorageOrigin(origins, fresh)); got != "https://a[] https://b[n=new] https://c[]" {
		t.Fatalf("替换结果 = %s", got)
	}
	if got := originsText(replaceStorageOrigin(origins[:1], fresh)); got != "https://a[] https://b[n=new]" {
		t.Fatalf("追加结果 = %s", got)
	}
}

// TestNativeWorkerBrowserLocalStorage NativeWorker 解析 runtime 导出的 localStorage；runtime 不导出（纯 Go runtime、stub 没有设置）时返回 false
func TestNativeWorkerBrowserLocalStorage(t *testing.T) {
	ctx := context.Background()
	encoded, err := json.Marshal(StorageOrigin{Origin: aiStudioOrigin, LocalStorage: []StorageItem{{Name: "k", Value: "v"}}})
	if err != nil {
		t.Fatal(err)
	}
	worker, _ := NewStubWorkerWithHooks("a@example.com", StubWorkerHooks{
		LocalStorage: func(context.Context) ([]byte, error) { return encoded, nil },
	})
	origin, ok, err := worker.BrowserLocalStorage(ctx)
	if err != nil || !ok || origin.Origin != aiStudioOrigin || !slices.Equal(origin.LocalStorage, []StorageItem{{Name: "k", Value: "v"}}) {
		t.Fatalf("导出 = %+v %t %v", origin, ok, err)
	}
	failing, _ := NewStubWorkerWithHooks("a@example.com", StubWorkerHooks{
		LocalStorage: func(context.Context) ([]byte, error) {
			return nil, errors.New("导出浏览器 localStorage: 页面 origin 不是 AI Studio")
		},
	})
	if _, ok, err := failing.BrowserLocalStorage(ctx); err == nil || ok {
		t.Fatalf("导出失败时应返回错误: %t %v", ok, err)
	}
	garbage, _ := NewStubWorkerWithHooks("a@example.com", StubWorkerHooks{
		LocalStorage: func(context.Context) ([]byte, error) { return []byte("SECRET{"), nil },
	})
	if _, _, err := garbage.BrowserLocalStorage(ctx); err == nil || strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("无法解析时应报错且不含内容: %v", err)
	}
	empty, _ := NewStubWorker("a@example.com", 0)
	if _, ok, err := empty.BrowserLocalStorage(ctx); err != nil || ok {
		t.Fatalf("stub 没有设置导出时应返回 false: %t %v", ok, err)
	}
	goWorker := &NativeWorker{accountID: "a@example.com", runtime: &goWAARuntime{}}
	if _, ok, err := goWorker.BrowserLocalStorage(ctx); err != nil || ok {
		t.Fatalf("纯 Go runtime 没有浏览器页面，应返回 false: %t %v", ok, err)
	}
}

// TestBrowserWritebackSkipsExternalLogin 外部直接替换认证文件（重新上传）、扫描还没载入时，Worker 启动后的写回不写：
// 新登录的签名 Cookie 与 localStorage 不被旧浏览器的值覆盖，内存仍是原来的登录（扫描比较登录签名时能发现），并记下账户由扫描载入
func TestBrowserWritebackSkipsExternalLogin(t *testing.T) {
	const id = "external@example.com"
	pool := localStorageTestPool(t, id)
	t.Cleanup(func() { externalLoginChanges.Delete(id) })
	generation := pool.AuthGeneration(id)
	before, _ := pool.LoginIdentity(id)
	account, err := pool.Account(id)
	if err != nil {
		t.Fatal(err)
	}
	uploaded := testStorageState("NEW")
	uploaded.Origins = []StorageOrigin{{Origin: aiStudioOrigin, LocalStorage: []StorageItem{{Name: "aiStudioUserPreference", Value: "new-login"}}}}
	if err := WriteStorageState(account.StoragePath, uploaded); err != nil {
		t.Fatal(err)
	}
	stale := StorageOrigin{Origin: aiStudioOrigin, LocalStorage: []StorageItem{{Name: "aiStudioUserPreference", Value: "old-browser"}}}
	if written, err := pool.MergeBrowserLocalStorage(id, generation, stale); err != nil || written {
		t.Fatalf("外部换了登录时不应写回 localStorage: written=%t err=%v", written, err)
	}
	if got := originsText(diskOrigins(t, pool, id)); got != aiStudioOrigin+"[aiStudioUserPreference=new-login]" {
		t.Fatalf("新登录的 localStorage 被改成 %s", got)
	}
	if _, recorded := externalLoginChanges.LoadAndDelete(id); !recorded {
		t.Fatal("应记下外部换了登录的账户，由扫描载入")
	}
	if written, err := pool.MergeBrowserCookies(id, generation, testStorageState("OLD").Cookies); err != nil || written {
		t.Fatalf("外部换了登录时不应写回 Cookie: written=%t err=%v", written, err)
	}
	if got := diskSAPISID(t, account.StoragePath); got != "NEW" {
		t.Fatalf("新登录的签名 Cookie 被改成 %q", got)
	}
	if _, recorded := externalLoginChanges.LoadAndDelete(id); !recorded {
		t.Fatal("写回 Cookie 时同样应记下账户")
	}
	if identity, _ := pool.LoginIdentity(id); identity != before || identity == LoginIdentity(uploaded) {
		t.Fatal("内存应保持原来的登录，扫描比较登录签名时才能发现重新上传")
	}
	if pool.AuthGeneration(id) != generation {
		t.Fatal("跳过写回不应推进认证代际")
	}
}

// TestMergeBrowserLocalStorageOrigin 只写回 AI Studio 这个 origin：其他 origin 报错、不写（多进程比较认证文件时只排除这个 origin）
func TestMergeBrowserLocalStorageOrigin(t *testing.T) {
	const id = "origin@example.com"
	pool := localStorageTestPool(t, id)
	before := originsText(diskOrigins(t, pool, id))
	other := StorageOrigin{Origin: "https://one.google.com", LocalStorage: []StorageItem{{Name: "other", Value: "changed"}}}
	if written, err := pool.MergeBrowserLocalStorage(id, pool.AuthGeneration(id), other); err == nil || written {
		t.Fatalf("其他 origin 应报错且不写: written=%t err=%v", written, err)
	}
	if got := originsText(diskOrigins(t, pool, id)); got != before {
		t.Fatalf("其他 origin 不应写入，磁盘 = %s", got)
	}
}
