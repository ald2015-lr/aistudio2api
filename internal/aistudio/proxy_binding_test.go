package aistudio

import (
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
)

// A1：EffectiveProxy 的优先级为账户固定代理 > IP 池绑定 > 全局代理；绑定 URL 为空时取全局；nil 账户取全局；清除绑定后回到全局
func TestEffectiveProxyPrefersAccountThenBinding(t *testing.T) {
	const global = "http://global.example:8080"
	var empty *Account
	if got := empty.EffectiveProxy(" " + global + " "); got != global {
		t.Fatalf("nil 账户 = %q", got)
	}
	empty.SetProxyBinding(&ProxyBinding{URL: "http://x"})
	if _, ok := empty.CurrentProxyBinding(); ok {
		t.Fatal("nil 账户不应有绑定")
	}

	account := indexTestAccount("a@example.com", BenefitTierFree, nil)
	if got := account.EffectiveProxy(global); got != global {
		t.Fatalf("没有绑定 = %q", got)
	}
	account.SetProxyBinding(&ProxyBinding{URL: "http://user:S3cr3t-P@ss@pool.example:10023", Slot: "10023", Active: true})
	if got := account.EffectiveProxy(global); got != "http://user:S3cr3t-P@ss@pool.example:10023" {
		t.Fatalf("绑定 = %q", got)
	}
	binding, ok := account.CurrentProxyBinding()
	if !ok || binding.Slot != "10023" || !binding.Active {
		t.Fatalf("CurrentProxyBinding = %+v %v", binding, ok)
	}
	account.SetProxyBinding(&ProxyBinding{Slot: "x"})
	if got := account.EffectiveProxy(global); got != global {
		t.Fatalf("绑定 URL 为空 = %q", got)
	}
	account.SetProxyBinding(&ProxyBinding{URL: "http://pool.example:10024"})
	account.Config.Proxy = " http://own.example:3128 "
	if got := account.EffectiveProxy(global); got != "http://own.example:3128" {
		t.Fatalf("账户固定代理 = %q", got)
	}
	account.Config.Proxy = ""
	account.SetProxyBinding(nil)
	if got := account.EffectiveProxy(global); got != global {
		t.Fatalf("清除绑定 = %q", got)
	}
	if _, ok := account.CurrentProxyBinding(); ok {
		t.Fatal("清除后仍有绑定")
	}
}

// A2：账户状态只在没有自己的代理时带 IP 池槽位
func TestAccountStatusProxySlot(t *testing.T) {
	pooled := indexTestAccount("a@example.com", BenefitTierFree, nil)
	own := indexTestAccount("b@example.com", BenefitTierFree, nil)
	own.Config.Proxy = "http://own.example:3128"
	pool := NewAccountPool([]*Account{pooled, own}, 2)
	pool.SetProxyBindings([]ProxyBindingUpdate{
		{AccountID: pooled.ID, Binding: &ProxyBinding{URL: "http://pool.example:10001", Slot: "10001", Active: true}},
		{AccountID: own.ID, Binding: &ProxyBinding{URL: "http://pool.example:10002", Slot: "10002", Active: true}},
	})
	statuses := map[string]AccountStatus{}
	for _, status := range pool.Status() {
		statuses[status.ID] = status
	}
	if status := statuses[pooled.ID]; status.ProxySlot != "10001" || !status.ProxySlotActive {
		t.Fatalf("池账户状态 = %+v", status)
	}
	if status := statuses[own.ID]; status.ProxySlot != "" || status.ProxySlotActive {
		t.Fatalf("有自己代理的账户不应带槽位：%+v", status)
	}
	// 缓存期内推送新绑定，状态立即可见
	pool.SetProxyBindings([]ProxyBindingUpdate{{AccountID: pooled.ID, Binding: &ProxyBinding{URL: "http://pool.example:10003", Slot: "10003"}}})
	for _, status := range pool.Status() {
		if status.ID == pooled.ID && (status.ProxySlot != "10003" || status.ProxySlotActive) {
			t.Fatalf("推送后状态 = %+v", status)
		}
	}
}

// A3：OnClosed 只在 Close 成功后调用一次；关闭失败不调用、重试成功后调用；关闭后再登记立即调用；回调里登记不死锁
func TestNativeWorkerOnClosed(t *testing.T) {
	worker, closed := newStubWorker("a@example.com", &stubRuntime{closeFailures: 1})
	var calls atomic.Int32
	worker.OnClosed(func() {
		calls.Add(1)
		worker.OnClosed(func() { calls.Add(10) })
	})
	if err := worker.Close(); err == nil {
		t.Fatal("第一次关闭应失败")
	}
	if calls.Load() != 0 {
		t.Fatalf("关闭失败时调用了回调：%d", calls.Load())
	}
	if err := worker.Close(); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 11 || closed() != 1 {
		t.Fatalf("关闭成功后回调 = %d，关闭次数 = %d", calls.Load(), closed())
	}
	worker.OnClosed(func() { calls.Add(100) })
	if calls.Load() != 111 {
		t.Fatalf("已关闭后登记应立即调用：%d", calls.Load())
	}
	if err := worker.Close(); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 111 {
		t.Fatalf("回调被重复调用：%d", calls.Load())
	}
	var nilWorker *NativeWorker
	nilWorker.OnClosed(func() { t.Fatal("nil Worker 不应调用回调") })

	// 并发登记与关闭：每个回调恰好调用一次
	concurrent, _ := newStubWorker("b@example.com", &stubRuntime{})
	var hits atomic.Int32
	var wait sync.WaitGroup
	for index := 0; index < 32; index++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			concurrent.OnClosed(func() { hits.Add(1) })
		}()
	}
	wait.Add(1)
	go func() {
		defer wait.Done()
		_ = concurrent.Close()
	}()
	wait.Wait()
	if hits.Load() != 32 {
		t.Fatalf("并发登记的回调调用次数 = %d", hits.Load())
	}
}

// A4：SetProxyBindings / UpdateProxyBindings 按 ID 写当前号池里的对象；不在号池的 ID 忽略；同 ID 新旧两个对象只写号池里那个
func TestAccountPoolProxyBindings(t *testing.T) {
	current := indexTestAccount("a@example.com", BenefitTierFree, nil)
	stale := indexTestAccount("a@example.com", BenefitTierFree, nil)
	other := indexTestAccount("b@example.com", BenefitTierFree, nil)
	other.Config.Proxy = "http://own.example:3128"
	pool := NewAccountPool([]*Account{current, other}, 2)

	written := pool.SetProxyBindings([]ProxyBindingUpdate{
		{AccountID: current.ID, Binding: &ProxyBinding{URL: "http://pool.example:10001", Slot: "10001"}},
		{AccountID: "missing@example.com", Binding: &ProxyBinding{URL: "http://pool.example:10002"}},
	})
	if written != 1 {
		t.Fatalf("写入个数 = %d", written)
	}
	if _, ok := stale.CurrentProxyBinding(); ok {
		t.Fatal("号池外的同 ID 对象被写入")
	}
	if got := current.EffectiveProxy(""); got != "http://pool.example:10001" {
		t.Fatalf("号池里的对象 = %q", got)
	}
	if written := pool.SetProxyBindings([]ProxyBindingUpdate{{AccountID: current.ID, Binding: &ProxyBinding{URL: "http://pool.example:10001", Slot: "10001"}}}); written != 0 {
		t.Fatalf("内容相同的推送不算写入：%d", written)
	}

	seen := map[string]string{}
	pool.UpdateProxyBindings(func(accountID, accountProxy string) *ProxyBinding {
		seen[accountID] = accountProxy
		if accountID == current.ID {
			return nil
		}
		return &ProxyBinding{URL: "http://pool.example:10009", Slot: "10009"}
	})
	if len(seen) != 2 || seen[other.ID] != "http://own.example:3128" || seen[current.ID] != "" {
		t.Fatalf("compute 收到 = %v", seen)
	}
	if _, ok := current.CurrentProxyBinding(); ok {
		t.Fatal("返回 nil 时应清除绑定")
	}
	if binding, ok := other.CurrentProxyBinding(); !ok || binding.Slot != "10009" {
		t.Fatalf("other 绑定 = %+v %v", binding, ok)
	}
	if got := other.EffectiveProxy(""); got != "http://own.example:3128" {
		t.Fatalf("账户固定代理优先于绑定：%q", got)
	}
	ids := pool.AccountIDs()
	if len(ids) != 2 {
		t.Fatalf("AccountIDs = %v", ids)
	}
	var nilPool *AccountPool
	if nilPool.SetProxyBindings([]ProxyBindingUpdate{{AccountID: "x"}}) != 0 || nilPool.AccountIDs() != nil {
		t.Fatal("nil 号池应安全返回")
	}
	nilPool.UpdateProxyBindings(func(string, string) *ProxyBinding { return nil })
}

// A5：TryLockProxyPort 两个句柄互斥（同进程两次打开即可验证按文件描述互斥）；释放后可再锁；目录不可写时报错
func TestTryLockProxyPort(t *testing.T) {
	directory := t.TempDir()
	first, locked, err := TryLockProxyPort(directory, "0123456789abcdef", 10001)
	if err != nil || !locked || first == nil {
		t.Fatalf("第一次加锁 = %v %v %v", first, locked, err)
	}
	second, locked, err := TryLockProxyPort(directory, "0123456789abcdef", 10001)
	if err != nil || locked || second != nil {
		t.Fatalf("同一端口第二个句柄应锁不到：%v %v %v", second, locked, err)
	}
	other, locked, err := TryLockProxyPort(directory, "0123456789abcdef", 10002)
	if err != nil || !locked {
		t.Fatalf("另一个端口 = %v %v", locked, err)
	}
	if err := other.Release(); err != nil {
		t.Fatal(err)
	}
	if err := first.Release(); err != nil {
		t.Fatal(err)
	}
	if err := first.Release(); err != nil {
		t.Fatalf("重复释放：%v", err)
	}
	again, locked, err := TryLockProxyPort(directory, "0123456789abcdef", 10001)
	if err != nil || !locked {
		t.Fatalf("释放后再锁 = %v %v", locked, err)
	}
	_ = again.Release()
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() != "proxy-port-0123456789abcdef-10001.lock" && entry.Name() != "proxy-port-0123456789abcdef-10002.lock" {
			t.Fatalf("锁文件名 = %q", entry.Name())
		}
	}
	var nilLock *ProxyPortLock
	if err := nilLock.Release(); err != nil {
		t.Fatal(err)
	}

	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, locked, err := TryLockProxyPort(filepath.Join(blocker, "sub"), "0123456789abcdef", 10001); err == nil || locked {
		t.Fatalf("目录不可用时应报错：%v %v", locked, err)
	}
	if _, _, err := TryLockProxyPort(directory, "", 10001); err == nil {
		t.Fatal("缺少 endpointKey 应报错")
	}
	if directory, err := ProxyPortLockDirectory(); err == nil && filepath.Base(directory) != "runtime-leases" {
		t.Fatalf("锁目录 = %q", directory)
	}
}
