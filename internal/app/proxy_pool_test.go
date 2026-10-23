package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/api"
	"github.com/Mag1cFall/AIStudio2API/internal/config"
	"github.com/Mag1cFall/AIStudio2API/internal/proxyrelay"
)

// 测试用的占位地址：断言日志、状态与记录文件里都不出现密码、用户名与主机名
const (
	proxyPoolTestPassword   = "S3cr3t-P@ss"
	proxyPoolTestStaticURL  = "isp.example.com:10000:user-static:" + proxyPoolTestPassword
	proxyPoolTestDynamicURL = "http://customer-a1-sessid-{session}:" + proxyPoolTestPassword + "@gate.example.com:58688"
	proxyPoolTestGlobal     = "http://203.0.113.9:3128"
)

// proxyPoolTestClock 为可前进的假时钟（并发安全）
type proxyPoolTestClock struct {
	mu  sync.Mutex
	now time.Time
}

// rotateSessionForTest 以账户当前会话的出口键调用 rotateSession（出口判定落在账户自己的会话上）；账户当前不是用自己的会话时传空键
func rotateSessionForTest(pool *proxyPool, accountID, reason string) bool {
	pool.mu.Lock()
	key, _ := pool.sessionEgressKeyLocked(accountID)
	pool.mu.Unlock()
	return pool.rotateSession(accountID, key, reason)
}

func newProxyPoolTestClock() *proxyPoolTestClock {
	return &proxyPoolTestClock{now: time.Date(2026, 10, 23, 8, 0, 0, 0, time.UTC)}
}

func (clock *proxyPoolTestClock) Now() time.Time {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	return clock.now
}

func (clock *proxyPoolTestClock) Advance(duration time.Duration) {
	clock.mu.Lock()
	clock.now = clock.now.Add(duration)
	clock.mu.Unlock()
}

// fakeProxyOwner 为账户池替身：按 ID 保存账户自己的代理与 IP 池推送的绑定
type fakeProxyOwner struct {
	mu       sync.Mutex
	ids      []string
	proxies  map[string]string
	bindings map[string]*aistudio.ProxyBinding
	pushes   int
	updated  int // SetProxyBindings 实际写入的账户绑定数（重写绑定的次数）
}

func newFakeProxyOwner(ids ...string) *fakeProxyOwner {
	owner := &fakeProxyOwner{proxies: make(map[string]string), bindings: make(map[string]*aistudio.ProxyBinding)}
	for _, id := range ids {
		owner.add(id, "")
	}
	return owner
}

func (owner *fakeProxyOwner) SetProxyBindings(updates []aistudio.ProxyBindingUpdate) int {
	owner.mu.Lock()
	defer owner.mu.Unlock()
	count := 0
	for _, update := range updates {
		if _, ok := owner.proxies[update.AccountID]; ok {
			owner.bindings[update.AccountID] = update.Binding
			count++
		}
	}
	owner.pushes++
	owner.updated += count
	return count
}

func (owner *fakeProxyOwner) UpdateProxyBindings(compute func(accountID, accountProxy string) *aistudio.ProxyBinding) {
	owner.mu.Lock()
	defer owner.mu.Unlock()
	for _, id := range owner.ids {
		owner.bindings[id] = compute(id, owner.proxies[id])
	}
}

func (owner *fakeProxyOwner) AccountIDs() []string {
	owner.mu.Lock()
	defer owner.mu.Unlock()
	return slices.Clone(owner.ids)
}

func (owner *fakeProxyOwner) add(id, proxy string) {
	owner.mu.Lock()
	defer owner.mu.Unlock()
	if _, ok := owner.proxies[id]; !ok {
		owner.ids = append(owner.ids, id)
	}
	owner.proxies[id] = proxy
}

func (owner *fakeProxyOwner) remove(id string) {
	owner.mu.Lock()
	defer owner.mu.Unlock()
	owner.ids = slices.DeleteFunc(owner.ids, func(candidate string) bool { return candidate == id })
	delete(owner.proxies, id)
	delete(owner.bindings, id)
}

func (owner *fakeProxyOwner) binding(id string) *aistudio.ProxyBinding {
	owner.mu.Lock()
	defer owner.mu.Unlock()
	return owner.bindings[id]
}

// effective 按 Account.EffectiveProxy 的优先级返回账户实际使用的代理：账户代理 > 绑定 URL > 全局
func (owner *fakeProxyOwner) effective(id, global string) string {
	owner.mu.Lock()
	defer owner.mu.Unlock()
	if proxy := owner.proxies[id]; proxy != "" {
		return proxy
	}
	if binding := owner.bindings[id]; binding != nil && binding.URL != "" {
		return binding.URL
	}
	return global
}

// fakeProxyCloser 为浏览器替身：记下 OnClosed 回调，close 时调用
type fakeProxyCloser struct {
	mu    sync.Mutex
	hooks []func()
}

func (closer *fakeProxyCloser) OnClosed(hook func()) {
	closer.mu.Lock()
	closer.hooks = append(closer.hooks, hook)
	closer.mu.Unlock()
}

func (closer *fakeProxyCloser) close() {
	closer.mu.Lock()
	hooks := closer.hooks
	closer.hooks = nil
	closer.mu.Unlock()
	for _, hook := range hooks {
		hook()
	}
}

// markProxyPoolProven 记下这些账户刚在别的出口（不经 IP 池端口）上生成成功过：规则 A 要求计入的账号里至少一个近期在别的出口上成功过
func markProxyPoolProven(pool *proxyPool, accountIDs ...string) {
	pool.mu.Lock()
	defer pool.mu.Unlock()
	for _, accountID := range accountIDs {
		pool.successes[accountID] = proxyPoolSuccess{at: pool.now()}
	}
}

func newTestProxyPool(t *testing.T, clock *proxyPoolTestClock, path, lockDir string) (*proxyPool, *requestRegistry) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	registry := newRequestRegistry(ctx)
	pool := newProxyPool(proxyPoolOptions{
		path: path, lockDir: lockDir, requests: registry, now: clock.Now,
		relayLimit: func() int { return 1 << 20 },
	})
	return pool, registry
}

func staticProxyPool(start, end int) config.ProxyPool {
	return config.ProxyPool{Mode: config.ProxyPoolStatic, URL: proxyPoolTestStaticURL, PortStart: start, PortEnd: end}
}

func dynamicProxyPool() config.ProxyPool {
	return config.ProxyPool{Mode: config.ProxyPoolDynamic, URL: proxyPoolTestDynamicURL}
}

func proxyPoolTestIDs(count int) []string {
	ids := make([]string, count)
	for index := range ids {
		ids[index] = fmt.Sprintf("user%03d@example.com", index)
	}
	return ids
}

func proxyPoolLogLines(registry *requestRegistry) []string {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	lines := make([]string, 0, len(registry.logs))
	for _, entry := range registry.logs {
		lines = append(lines, entry.Level+" "+entry.Source+" "+entry.Message)
	}
	return lines
}

func countProxyPoolLogs(registry *requestRegistry, fragment string) int {
	count := 0
	for _, line := range proxyPoolLogLines(registry) {
		if strings.Contains(line, fragment) {
			count++
		}
	}
	return count
}

func staticPortURL(t *testing.T, port int) string {
	t.Helper()
	normalized, err := config.NormalizeProxyPool(staticProxyPool(10001, 10200))
	if err != nil {
		t.Fatal(err)
	}
	return normalized.StaticURL(port)
}

func leasePort(t *testing.T, lease *proxyPoolLease) int {
	t.Helper()
	port, err := strconv.Atoi(lease.Slot())
	if err != nil || !lease.Pooled() {
		t.Fatalf("租约不是静态端口：pooled=%v slot=%q", lease.Pooled(), lease.Slot())
	}
	return port
}

var upstreamFailure = fmt.Errorf("WAA Worker 启动失败；本机代理中转：连不上上游代理%w", proxyrelay.ErrUpstream)

// TestProxyPoolNilSafe IP 池与租约为 nil 时全部方法安全返回
func TestProxyPoolNilSafe(t *testing.T) {
	var pool *proxyPool
	if pool.configure(staticProxyPool(1, 2), "", 1) || pool.acquire("a", "a", proxyPoolKindWorker, "a", "x") != nil ||
		pool.status() != nil || pool.slotLabel("a") != "" || pool.pinnedDefault() != nil || pool.rotateSession("a", "", "x") ||
		pool.startCheck(context.Background(), staticProxyPool(1, 2), nil) {
		t.Fatal("nil IP 池应全部返回零值")
	}
	pool.prime(nil)
	pool.attach(nil)
	pool.refresh(nil, "a")
	pool.rateLimited("a", false)
	pool.succeeded("a")
	pool.flush()
	if _, ok := pool.loginPreview(); ok {
		t.Fatal("nil IP 池不应有登录出口")
	}
	var lease *proxyPoolLease
	lease.release()
	lease.attach(nil)
	lease.launchFailed(errors.New("x"), false)
	lease.adopt("a")
	if lease.URL() != "" || lease.Pooled() || lease.Slot() != "" {
		t.Fatal("nil 租约应返回零值")
	}
}

// TestProxyPoolStaticExclusive P1：150 个账户各申请一次得到 150 个不同端口，绑定跟随租约；归还后可再分
func TestProxyPoolStaticExclusive(t *testing.T) {
	clock := newProxyPoolTestClock()
	pool, _ := newTestProxyPool(t, clock, "", "")
	ids := proxyPoolTestIDs(150)
	owner := newFakeProxyOwner(ids...)
	if !pool.configure(staticProxyPool(10001, 10200), "", 188) {
		t.Fatal("第一次 configure 应返回有变化")
	}
	pool.attach(owner)
	seen := make(map[int]string)
	var leases []*proxyPoolLease
	for _, id := range ids {
		lease := pool.acquire(id, id, proxyPoolKindWorker, id, "")
		port := leasePort(t, lease)
		if other, ok := seen[port]; ok {
			t.Fatalf("端口 %d 同时分给了 %s 与 %s", port, other, id)
		}
		seen[port] = id
		if lease.URL() != staticPortURL(t, port) {
			t.Fatalf("端口 %d 的 URL = %q", port, lease.URL())
		}
		binding := owner.binding(id)
		if binding == nil || !binding.Active || binding.URL != lease.URL() || binding.Slot != lease.Slot() {
			t.Fatalf("%s 的绑定 = %+v", id, binding)
		}
		leases = append(leases, lease)
	}
	if status := pool.status(); status.InUse != 150 || status.Leases != 150 || status.Shared != 0 || status.Total != 200 {
		t.Fatalf("状态 = %+v", status)
	}
	for _, lease := range leases {
		lease.release()
		lease.release()
	}
	status := pool.status()
	if status.InUse != 0 || status.Leases != 0 || len(status.Assignments) != 0 {
		t.Fatalf("全部归还后状态 = %+v", status)
	}
	for _, id := range ids {
		if binding := owner.binding(id); binding != nil && binding.Active {
			t.Fatalf("%s 归还后仍是活动绑定", id)
		}
	}
	again := pool.acquire(ids[0], ids[0], proxyPoolKindWorker, ids[0], "")
	if !again.Pooled() {
		t.Fatal("归还后应能再分配")
	}
	again.release()
}

// TestProxyPoolAffinity P2：同账户回到上次的端口；端口被别人用过就不再抢回；跨进程重启（读同一文件）后仍回到原端口
func TestProxyPoolAffinity(t *testing.T) {
	clock := newProxyPoolTestClock()
	path := filepath.Join(t.TempDir(), "runtime", proxyPoolFileName)
	pool, _ := newTestProxyPool(t, clock, path, "")
	owner := newFakeProxyOwner("a@example.com", "b@example.com", "c@example.com", "d@example.com")
	pool.configure(staticProxyPool(10001, 10010), "", 5)
	pool.attach(owner)

	first := pool.acquire("a@example.com", "a@example.com", proxyPoolKindWorker, "a", "")
	port := leasePort(t, first)
	first.release()
	clock.Advance(time.Minute)
	second := pool.acquire("a@example.com", "a@example.com", proxyPoolKindWorker, "a", "")
	if leasePort(t, second) != port {
		t.Fatalf("应回到上次的端口 %d，得到 %s", port, second.Slot())
	}
	second.release()
	if status := pool.status(); status.AffinityHits != 1 || status.AffinityMisses != 0 {
		t.Fatalf("亲和计数 = %d/%d", status.AffinityHits, status.AffinityMisses)
	}
	pool.flush()

	// 重启：新实例读同一文件
	restarted, _ := newTestProxyPool(t, clock, path, "")
	restarted.configure(staticProxyPool(10001, 10010), "", 5)
	restarted.attach(owner)
	third := restarted.acquire("a@example.com", "a@example.com", proxyPoolKindWorker, "a", "")
	if leasePort(t, third) != port {
		t.Fatalf("重启后应回到端口 %d，得到 %s", port, third.Slot())
	}
	third.release()

	// 端口被别人独占过（归属变了）：亲和失效、不抢回
	small, _ := newTestProxyPool(t, clock, "", "")
	small.configure(staticProxyPool(10001, 10002), "", 2)
	small.attach(owner)
	a := small.acquire("a@example.com", "a@example.com", proxyPoolKindWorker, "a", "")
	aPort := leasePort(t, a)
	a.release()
	other := 10001 + 10002 - aPort
	c := small.acquire("c@example.com", "c@example.com", proxyPoolKindWorker, "c", "")
	if leasePort(t, c) != other {
		t.Fatalf("c 应先拿从没分过的端口 %d，得到 %s", other, c.Slot())
	}
	b := small.acquire("b@example.com", "b@example.com", proxyPoolKindWorker, "b", "")
	if leasePort(t, b) != aPort {
		t.Fatalf("b 应拿到 a 归还的端口 %d", aPort)
	}
	c.release()
	clock.Advance(time.Minute)
	b.release()
	a = small.acquire("a@example.com", "a@example.com", proxyPoolKindWorker, "a", "")
	if status := small.status(); status.AffinityMisses != 1 || status.AffinityHits != 0 || leasePort(t, a) != other {
		t.Fatalf("端口被 b 用过后 a 的亲和应失效、改用最久未用的端口 %d：%s %d/%d", other, a.Slot(), status.AffinityHits, status.AffinityMisses)
	}
	a.release()
	// b 回到自己的端口：a 没有把它抢回去
	b = small.acquire("b@example.com", "b@example.com", proxyPoolKindWorker, "b", "")
	if status := small.status(); leasePort(t, b) != aPort || status.AffinityHits != 1 {
		t.Fatalf("b 应命中亲和回到 %d：%s %+v", aPort, b.Slot(), status)
	}
	b.release()
}

// TestProxyPoolPickOrder P3：可分配端口按“从没用过 > 孤儿 > 别人的归属”挑选，同档最久未用
func TestProxyPoolPickOrder(t *testing.T) {
	clock := newProxyPoolTestClock()
	pool, _ := newTestProxyPool(t, clock, "", "")
	pool.configure(staticProxyPool(10001, 10005), "", 5)
	now := clock.Now()
	pool.mu.Lock()
	// 10001、10002 归 x、y（亲和有效），10003 是孤儿（z 的亲和已指向别处），10004、10005 从没用过（10005 更早用过但没有归属）
	pool.portLocked(10001).lastAccount, pool.portLocked(10001).lastUsedAt = "x", now.Add(-time.Hour)
	pool.portLocked(10002).lastAccount, pool.portLocked(10002).lastUsedAt = "y", now.Add(-2*time.Hour)
	pool.affinity["x"] = proxyPoolAffinity{port: 10001, at: now}
	pool.affinity["y"] = proxyPoolAffinity{port: 10002, at: now}
	pool.portLocked(10003).lastAccount = "z"
	pool.affinity["z"] = proxyPoolAffinity{port: 10009, at: now}
	pool.portLocked(10005).lastUsedAt = now.Add(-time.Minute)
	pool.mu.Unlock()
	var order []int
	for index := 0; index < 5; index++ {
		id := fmt.Sprintf("n%d", index)
		lease := pool.acquire(id, id, proxyPoolKindWorker, id, "")
		order = append(order, leasePort(t, lease))
	}
	// 从没用过：10004（零时间）早于 10005；然后孤儿 10003；然后别人的归属按最久未用：10002 早于 10001
	if want := []int{10004, 10005, 10003, 10002, 10001}; !slices.Equal(order, want) {
		t.Fatalf("挑选次序 = %v，期望 %v", order, want)
	}
}

// TestProxyPoolSameKeyShares P4：同一账户两次申请共用一个端口；一个归还后仍被另一个持有
func TestProxyPoolSameKeyShares(t *testing.T) {
	clock := newProxyPoolTestClock()
	pool, _ := newTestProxyPool(t, clock, "", "")
	owner := newFakeProxyOwner("a@example.com", "b@example.com")
	pool.configure(staticProxyPool(10001, 10003), "", 3)
	pool.attach(owner)
	first := pool.acquire("a@example.com", "a@example.com", proxyPoolKindWorker, "a", "")
	second := pool.acquire("a@example.com", "a@example.com", proxyPoolKindLogin, "a", "")
	if first.Slot() != second.Slot() || second.shared {
		t.Fatalf("同账户应共用端口：%s / %s", first.Slot(), second.Slot())
	}
	first.release()
	b := pool.acquire("b@example.com", "b@example.com", proxyPoolKindWorker, "b", "")
	if b.Slot() == second.Slot() {
		t.Fatal("仍被同账户持有的端口不应分给别人")
	}
	if binding := owner.binding("a@example.com"); binding == nil || !binding.Active || binding.Slot != second.Slot() {
		t.Fatalf("一个租约归还后绑定应指向仍持有的租约：%+v", binding)
	}
	second.release()
	b.release()
	if pool.status().InUse != 0 {
		t.Fatal("全部归还后不应有占用")
	}
}

// TestProxyPoolShortage P5：端口不足时与持有最少的端口共用，WARN 每分钟一行并带这一分钟内另有的次数
func TestProxyPoolShortage(t *testing.T) {
	clock := newProxyPoolTestClock()
	pool, registry := newTestProxyPool(t, clock, "", "")
	ids := proxyPoolTestIDs(6)
	pool.configure(staticProxyPool(10001, 10003), "", 6)
	if countProxyPoolLogs(registry, "IP 池端口少于可能同时运行的浏览器数") != 1 {
		t.Fatal("端口少于容量时应 WARN")
	}
	pool.attach(newFakeProxyOwner(ids...))
	var leases []*proxyPoolLease
	for _, id := range ids[:5] {
		leases = append(leases, pool.acquire(id, id, proxyPoolKindWorker, id, ""))
	}
	holders := make(map[string]int)
	shared := 0
	for _, lease := range leases {
		holders[lease.Slot()]++
		if lease.shared {
			shared++
		}
	}
	if shared != 2 || len(holders) != 3 {
		t.Fatalf("应有 2 个共用、3 个端口全部占用：shared=%d holders=%v", shared, holders)
	}
	for slot, count := range holders {
		if count > 2 {
			t.Fatalf("端口 %s 有 %d 个持有者，应选持有最少的", slot, count)
		}
	}
	if got := countProxyPoolLogs(registry, "IP 池端口不足，与其他账号共用"); got != 1 {
		t.Fatalf("一分钟内只应 WARN 一行，得到 %d", got)
	}
	clock.Advance(61 * time.Second)
	leases = append(leases, pool.acquire(ids[5], ids[5], proxyPoolKindWorker, ids[5], ""))
	if got := countProxyPoolLogs(registry, "这一分钟内另有 1 次"); got != 1 {
		t.Fatalf("应带这一分钟内另有的次数：%v", proxyPoolLogLines(registry))
	}
	if status := pool.status(); status.Shared != 3 || status.InUse != 3 {
		t.Fatalf("状态 = %+v", status)
	}
	for _, lease := range leases {
		lease.release()
	}
	pool.configure(staticProxyPool(10001, 10010), "", 6)
	if countProxyPoolLogs(registry, "IP 池端口已足够") != 1 {
		t.Fatal("端口恢复够用时应 INFO 一行")
	}
}

// TestProxyPoolRuleA P6：规则 A——窗口内两个不同账号的裸 429、其间没有生成成功才冷却；启动成功不清规则 A
func TestProxyPoolRuleA(t *testing.T) {
	setup := func(t *testing.T) (*proxyPool, *proxyPoolTestClock, *requestRegistry, *fakeProxyOwner) {
		clock := newProxyPoolTestClock()
		pool, registry := newTestProxyPool(t, clock, "", "")
		owner := newFakeProxyOwner("a@example.com", "b@example.com", "c@example.com", "idle@example.com")
		pool.configure(staticProxyPool(10001, 10001), "", 1)
		pool.attach(owner)
		return pool, clock, registry, owner
	}
	// 换手：A 裸 429 → 归还 → B 申请并启动成功 → B 裸 429 → 冷却
	t.Run("handover", func(t *testing.T) {
		pool, clock, registry, owner := setup(t)
		var cooled []proxyPoolCooling
		pool.setPortCooledHook(func(cooling proxyPoolCooling) {
			// 回调在锁外：这里可以调用 IP 池的方法
			_ = pool.status()
			cooled = append(cooled, cooling)
		})
		a := pool.acquire("a@example.com", "a@example.com", proxyPoolKindWorker, "a", "")
		a.attach(&fakeProxyCloser{})
		pool.rateLimited("a@example.com", false)
		pool.rateLimited("a@example.com", false)
		a.release()
		clock.Advance(time.Minute)
		b := pool.acquire("b@example.com", "b@example.com", proxyPoolKindWorker, "b", "")
		b.attach(&fakeProxyCloser{})
		if b.Slot() != "10001" {
			t.Fatal("只有一个端口")
		}
		if owner.binding("idle@example.com") == nil {
			// 被占前空闲账户的默认出口在 10001；被 b 占用后重推（没有其他端口、PROXY 为空时退到哈希端口共用）
			t.Log("空闲账户没有默认出口")
		}
		// b 刚在别的出口上成功过：到这个端口就被拒是 IP 的证据
		markProxyPoolProven(pool, "b@example.com")
		pool.rateLimited("b@example.com", false)
		if len(cooled) != 1 || cooled[0].Port != 10001 || cooled[0].Level != 1 || cooled[0].Rule != proxyPoolRuleRateLimited ||
			!slices.Equal(sortedStrings(cooled[0].RateLimitedAccounts), []string{"a@example.com", "b@example.com"}) ||
			!slices.Equal(cooled[0].Holders, []string{"b@example.com"}) {
			t.Fatalf("冷却回调 = %+v", cooled)
		}
		if countProxyPoolLogs(registry, "IP 池端口冷却 | 端口=10001 | 第 1 档 | 时长=15m0s") != 1 {
			t.Fatalf("冷却日志：%v", proxyPoolLogLines(registry))
		}
		// 冷却不影响已有持有者；新租约（没有其他端口时）用最早恢复的冷却端口并 WARN
		if binding := owner.binding("b@example.com"); binding == nil || !binding.Active || binding.Slot != "10001" {
			t.Fatalf("冷却不应影响已有持有者：%+v", binding)
		}
		c := pool.acquire("c@example.com", "c@example.com", proxyPoolKindWorker, "c", "")
		if !c.shared || countProxyPoolLogs(registry, "IP 池端口全部冷却") != 1 {
			t.Fatal("全部冷却时应共用最早恢复的端口并 WARN")
		}
		status := pool.status()
		if status.Cooling != 1 || len(status.CoolingPorts) != 1 || status.CoolingPorts[0].Level != 1 ||
			len(status.HotPorts) != 1 || status.HotPorts[0].RateLimited != 3 || status.HotPorts[0].Accounts != 2 {
			t.Fatalf("状态 = %+v", status)
		}
		c.release()
		b.release()
	})
	t.Run("same-account", func(t *testing.T) {
		pool, _, _, _ := setup(t)
		a := pool.acquire("a@example.com", "a@example.com", proxyPoolKindWorker, "a", "")
		for index := 0; index < 5; index++ {
			pool.rateLimited("a@example.com", false)
		}
		if pool.status().Cooling != 0 {
			t.Fatal("同一账号多次裸 429 不应冷却端口")
		}
		a.release()
	})
	t.Run("success-between", func(t *testing.T) {
		pool, clock, _, _ := setup(t)
		a := pool.acquire("a@example.com", "a@example.com", proxyPoolKindWorker, "a", "")
		pool.rateLimited("a@example.com", false)
		pool.succeeded("a@example.com")
		a.release()
		clock.Advance(time.Minute)
		b := pool.acquire("b@example.com", "b@example.com", proxyPoolKindWorker, "b", "")
		pool.rateLimited("b@example.com", false)
		if pool.status().Cooling != 0 {
			t.Fatal("中间有生成成功不应冷却")
		}
		b.release()
	})
	t.Run("window", func(t *testing.T) {
		pool, clock, _, _ := setup(t)
		a := pool.acquire("a@example.com", "a@example.com", proxyPoolKindWorker, "a", "")
		pool.rateLimited("a@example.com", false)
		a.release()
		clock.Advance(proxyPoolFailureWindow)
		b := pool.acquire("b@example.com", "b@example.com", proxyPoolKindWorker, "b", "")
		pool.rateLimited("b@example.com", false)
		if pool.status().Cooling != 0 {
			t.Fatal("窗口过期的裸 429 不应计入")
		}
		b.release()
	})
	t.Run("levels", func(t *testing.T) {
		pool, clock, registry, _ := setup(t)
		cool := func() {
			a := pool.acquire("a@example.com", "a@example.com", proxyPoolKindWorker, "a", "")
			pool.rateLimited("a@example.com", false)
			a.release()
			b := pool.acquire("b@example.com", "b@example.com", proxyPoolKindWorker, "b", "")
			markProxyPoolProven(pool, "b@example.com")
			pool.rateLimited("b@example.com", false)
			b.release()
		}
		for level, want := range []string{"15m0s", "30m0s", "1h0m0s", "2h0m0s", "2h0m0s"} {
			cool()
			if countProxyPoolLogs(registry, fmt.Sprintf("第 %d 档 | 时长=%s", level+1, want)) != 1 {
				t.Fatalf("第 %d 档时长应为 %s：%v", level+1, want, proxyPoolLogLines(registry))
			}
			clock.Advance(3 * time.Hour)
		}
		clock.Advance(proxyPoolLevelResetAfter)
		cool()
		if countProxyPoolLogs(registry, "第 1 档 | 时长=15m0s") != 2 {
			t.Fatal("24 小时没有再冷却后档位应清零")
		}
	})
	// 有自己代理的账户、直通租约、没有活动租约的账户都不计入
	t.Run("not-counted", func(t *testing.T) {
		pool, _, _, _ := setup(t)
		pool.rateLimited("nobody@example.com", false)
		pool.configure(config.ProxyPool{Mode: config.ProxyPoolOff, URL: proxyPoolTestStaticURL, PortStart: 10001, PortEnd: 10001}, "", 1)
		a := pool.acquire("a@example.com", "a@example.com", proxyPoolKindWorker, "a", proxyPoolTestGlobal)
		if a.Pooled() || a.URL() != proxyPoolTestGlobal {
			t.Fatal("IP 池关闭时应是直通租约")
		}
		pool.rateLimited("a@example.com", false)
		pool.configure(staticProxyPool(10001, 10001), "", 1)
		b := pool.acquire("b@example.com", "b@example.com", proxyPoolKindWorker, "b", "")
		pool.rateLimited("b@example.com", false)
		if pool.status().Cooling != 0 {
			t.Fatal("直通租约的裸 429 不应计入端口")
		}
		a.release()
		b.release()
	})
}

func sortedStrings(values []string) []string {
	values = slices.Clone(values)
	sort.Strings(values)
	return values
}

// TestProxyPoolRuleB P7：连续 2 次代理上游失败的启动失败冷却端口并清除相关亲和；取消、非上游失败不计；中间启动成功重计
func TestProxyPoolRuleB(t *testing.T) {
	clock := newProxyPoolTestClock()
	pool, registry := newTestProxyPool(t, clock, "", "")
	owner := newFakeProxyOwner("a@example.com", "b@example.com")
	pool.configure(staticProxyPool(10001, 10002), "", 2)
	pool.attach(owner)
	acquire := func(id string) *proxyPoolLease {
		lease := pool.acquire(id, id, proxyPoolKindWorker, id, "")
		if lease.Slot() != "10001" {
			t.Fatalf("应一直拿到 10001（亲和），得到 %s", lease.Slot())
		}
		return lease
	}
	acquire("a@example.com").launchFailed(upstreamFailure, true)
	acquire("a@example.com").launchFailed(errors.New("WAA 初始化失败"), false)
	acquire("a@example.com").launchFailed(nil, false)
	acquire("a@example.com").launchFailed(upstreamFailure, false)
	if countProxyPoolLogs(registry, "IP 池出口连不通 | 端口=10001 | 连续=1/2") != 1 {
		t.Fatalf("只有未取消的上游失败才计入：%v", proxyPoolLogLines(registry))
	}
	// 中间启动成功重计
	success := acquire("a@example.com")
	success.attach(&fakeProxyCloser{})
	success.release()
	acquire("a@example.com").launchFailed(upstreamFailure, false)
	if pool.status().Cooling != 0 {
		t.Fatal("启动成功后应重新计数")
	}
	acquire("a@example.com").launchFailed(upstreamFailure, false)
	status := pool.status()
	if status.Cooling != 1 || status.CoolingPorts[0].Port != 10001 || countProxyPoolLogs(registry, "原因=连续 2 次出口连不通") != 1 {
		t.Fatalf("连续 2 次上游失败应冷却：%+v %v", status, proxyPoolLogLines(registry))
	}
	pool.mu.Lock()
	_, hasAffinity := pool.affinity["a@example.com"]
	owned := pool.ports[10001].lastAccount
	pool.mu.Unlock()
	if hasAffinity || owned != "" {
		t.Fatal("规则 B 冷却应清除指向该端口的亲和与归属")
	}
	if next := pool.acquire("a@example.com", "a@example.com", proxyPoolKindWorker, "a", ""); next.Slot() != "10002" {
		t.Fatalf("冷却中的端口不应再分出，得到 %s", next.Slot())
	}
}

// TestProxyPoolSystemicProtection P8：多数端口同时出错时不冷却，日志每个窗口一行
func TestProxyPoolSystemicProtection(t *testing.T) {
	clock := newProxyPoolTestClock()
	pool, registry := newTestProxyPool(t, clock, "", "")
	ids := proxyPoolTestIDs(12)
	pool.configure(staticProxyPool(10001, 10020), "", 20)
	pool.attach(newFakeProxyOwner(ids...))
	for _, id := range ids[:10] {
		pool.acquire(id, id, proxyPoolKindWorker, id, "").attach(&fakeProxyCloser{})
	}
	// 6 个端口有规则 A 记录：加上要冷却的端口共 7 个，不少于 max(5, 11 个使用中 × 50%)
	for _, id := range ids[:6] {
		pool.rateLimited(id, false)
	}
	for _, id := range ids[10:12] {
		for attempt := 0; attempt < 2; attempt++ {
			pool.acquire(id, id, proxyPoolKindWorker, id, "").launchFailed(upstreamFailure, false)
		}
	}
	if status := pool.status(); status.Cooling != 0 {
		t.Fatalf("多数端口同时出错时不应冷却：%+v", status)
	}
	if got := countProxyPoolLogs(registry, "IP 池多数端口同时出错"); got != 1 {
		t.Fatalf("整体故障保护日志每个窗口一行，得到 %d", got)
	}
}

// TestProxyPoolDynamic P9：会话号格式、确定、不同账户不同；裸 429 不换会话；2 次上游失败换代号；10 分钟内不重复换；
// 启动成功清零；30 分钟窗口；代号到 1295 后回到 1；有活动租约时绑定不变、归还后切到新会话
func TestProxyPoolDynamic(t *testing.T) {
	clock := newProxyPoolTestClock()
	path := filepath.Join(t.TempDir(), proxyPoolFileName)
	pool, registry := newTestProxyPool(t, clock, path, "")
	owner := newFakeProxyOwner("a@example.com", "b@example.com", "idle@example.com")
	pool.configure(dynamicProxyPool(), "", 10)
	pool.attach(owner)
	format := regexp.MustCompile(`^[a-z0-9]{12}(r[a-z0-9]{1,2})?$`)
	idle := owner.binding("idle@example.com")
	if idle == nil || idle.Active || !format.MatchString(idle.Slot) || idle.URL != pool.settings.cfg.SessionURL(idle.Slot) ||
		!strings.Contains(idle.URL, "sessid-"+idle.Slot+":") || idle.EgressTag != "" {
		t.Fatalf("空闲账户的默认出口 = %+v", idle)
	}
	a := pool.acquire("a@example.com", "a@example.com", proxyPoolKindWorker, "a", "")
	b := pool.acquire("b@example.com", "b@example.com", proxyPoolKindWorker, "b", "")
	if !a.Pooled() || a.Slot() == b.Slot() || a.Slot() != proxyPoolHashSession("a@example.com") {
		t.Fatalf("会话号应按账户确定且不同：%s / %s", a.Slot(), b.Slot())
	}
	original := a.Slot()
	for index := 0; index < 5; index++ {
		pool.rateLimited("a@example.com", false)
	}
	a.release()
	if again := pool.acquire("a@example.com", "a@example.com", proxyPoolKindWorker, "a", ""); again.Slot() != original {
		t.Fatal("裸 429 不应换会话")
	} else {
		again.launchFailed(upstreamFailure, false)
	}
	// 启动成功清零
	success := pool.acquire("a@example.com", "a@example.com", proxyPoolKindWorker, "a", "")
	success.attach(&fakeProxyCloser{})
	success.release()
	pool.acquire("a@example.com", "a@example.com", proxyPoolKindWorker, "a", "").launchFailed(upstreamFailure, false)
	// 30 分钟窗口：超过窗口的那次不算
	clock.Advance(proxyPoolSessionWindow + time.Minute)
	pool.acquire("a@example.com", "a@example.com", proxyPoolKindWorker, "a", "").launchFailed(upstreamFailure, false)
	if binding := owner.binding("a@example.com"); binding.Slot != original {
		t.Fatalf("还不应换会话：%+v", binding)
	}
	// 有活动租约时不立即切换
	holder := pool.acquire("a@example.com", "a@example.com", proxyPoolKindWorker, "a", "")
	pool.acquire("a@example.com", "a@example.com", proxyPoolKindWorker, "a", "").launchFailed(upstreamFailure, false)
	rotated := original + "r1"
	if binding := owner.binding("a@example.com"); binding.Slot != original || !binding.Active {
		t.Fatalf("有活动租约时绑定不变：%+v", binding)
	}
	if countProxyPoolLogs(registry, "IP 池换会话 | 会话="+original+" → "+rotated+" | 原因=连续 2 次出口连不通 | 生效=下次启动 Worker") != 1 {
		t.Fatalf("换会话日志：%v", proxyPoolLogLines(registry))
	}
	holder.release()
	if binding := owner.binding("a@example.com"); binding.Slot != rotated || binding.Active {
		t.Fatalf("归还后应切到新会话：%+v", binding)
	}
	// 10 分钟内不重复换
	pool.acquire("a@example.com", "a@example.com", proxyPoolKindWorker, "a", "").launchFailed(upstreamFailure, false)
	pool.acquire("a@example.com", "a@example.com", proxyPoolKindWorker, "a", "").launchFailed(upstreamFailure, false)
	if binding := owner.binding("a@example.com"); binding.Slot != rotated {
		t.Fatalf("10 分钟内不应再换：%+v", binding)
	}
	clock.Advance(proxyPoolRotateInterval)
	pool.acquire("a@example.com", "a@example.com", proxyPoolKindWorker, "a", "").launchFailed(upstreamFailure, false)
	if binding := owner.binding("a@example.com"); binding.Slot != original+"r2" || !format.MatchString(binding.Slot) {
		t.Fatalf("应换到第 2 代：%+v", binding)
	}
	if countProxyPoolLogs(registry, "生效=立即（没有 Worker）") != 1 {
		t.Fatal("没有 Worker 时换会话立即生效")
	}
	// 代号上限回到 1
	pool.mu.Lock()
	pool.sessions["b@example.com"] = &proxyPoolSession{generation: proxyPoolMaxGeneration}
	pool.rotateLocked("b@example.com", clock.Now())
	wrapped := pool.sessionLocked("b@example.com")
	pool.sessions["b@example.com"].generation = proxyPoolMaxGeneration
	longest := pool.sessionLocked("b@example.com")
	pool.mu.Unlock()
	if wrapped != proxyPoolHashSession("b@example.com")+"r1" || len(longest) != 15 || !format.MatchString(longest) {
		t.Fatalf("代号回绕 = %q，最长 = %q", wrapped, longest)
	}
	if !rotateSessionForTest(pool, "idle@example.com", "出口受限") || rotateSessionForTest(pool, "idle@example.com", "出口受限") {
		t.Fatal("rotateSession 应受 10 分钟间隔约束")
	}
	b.release()
	pool.flush()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var record proxyPoolRecord
	if err := json.Unmarshal(data, &record); err != nil || record.Dynamic.Accounts["a@example.com"].Generation != 2 {
		t.Fatalf("代号应持久化：%s (%v)", data, err)
	}
	if status := pool.status(); status.Rotations != 4 || status.Mode != config.ProxyPoolDynamic || status.Total != 0 {
		t.Fatalf("状态 = %+v", status)
	}
	// 会话号写在密码里：EgressTag 为会话号
	inPassword, _ := newTestProxyPool(t, clock, "", "")
	inPassword.configure(config.ProxyPool{Mode: config.ProxyPoolDynamic, URL: "gate.example.com:7777:customer-a1:pw_session-{session}"}, "", 1)
	lease := inPassword.acquire("a@example.com", "a@example.com", proxyPoolKindWorker, "a", "")
	if lease.egressTag != lease.Slot() || !strings.Contains(lease.URL(), "pw_session-"+lease.Slot()+"@") {
		t.Fatalf("会话号在密码里时 EgressTag 应为会话号：%+v", lease)
	}
}

// TestProxyPoolConfigure P10：模式切换、端口段、地址、容量变化的生效方式与日志
func TestProxyPoolConfigure(t *testing.T) {
	clock := newProxyPoolTestClock()
	pool, registry := newTestProxyPool(t, clock, "", "")
	owner := newFakeProxyOwner("worker@example.com", "idle@example.com", "own@example.com", "pinned@example.com")
	owner.add("own@example.com", "http://198.51.100.1:8080")
	owner.add("pinned@example.com", proxyPoolTestGlobal)
	off := config.ProxyPool{Mode: config.ProxyPoolOff, URL: proxyPoolTestStaticURL}
	if !pool.configure(off, proxyPoolTestGlobal, 188) {
		t.Fatal("第一次 configure 应返回有变化")
	}
	pool.attach(owner)
	if pool.configure(off, proxyPoolTestGlobal, 188) {
		t.Fatal("相同配置应返回无变化")
	}
	if len(proxyPoolLogLines(registry)) != 0 || pool.status() != nil {
		t.Fatalf("关闭时不记日志、状态为空：%v", proxyPoolLogLines(registry))
	}
	worker := pool.acquire("worker@example.com", "worker@example.com", proxyPoolKindWorker, "worker", proxyPoolTestGlobal)
	if worker.Pooled() || len(proxyPoolLogLines(registry)) != 0 {
		t.Fatal("IP 池关闭时为直通租约，不记日志")
	}
	// off → static：直通租约账户的 Go 侧仍走 PROXY，其他账户立即切到默认出口
	pool.configure(staticProxyPool(10001, 10200), proxyPoolTestGlobal, 188)
	if owner.effective("worker@example.com", proxyPoolTestGlobal) != proxyPoolTestGlobal {
		t.Fatal("直通租约的账户在 Worker 关闭前仍走 PROXY")
	}
	idle := owner.binding("idle@example.com")
	if idle == nil || idle.Active || !strings.Contains(idle.URL, "isp.example.com:") {
		t.Fatalf("空闲账户应立即切到池内端口：%+v", idle)
	}
	if owner.binding("own@example.com") != nil || owner.binding("pinned@example.com") != nil {
		t.Fatal("有自己代理的账户不应有绑定")
	}
	status := pool.status()
	if status.Unpooled != 1 || status.PinnedDefault != 1 || status.Capacity != 188 || status.Endpoint != "http://isp.example.com" {
		t.Fatalf("状态 = %+v", status)
	}
	for _, fragment := range []string{
		"IP 池已更新 | 模式=静态 | 出口=http://isp.example.com | 端口=10001-10200（200 个） | 需要=188",
		"IP 池已开启，运行中的 Worker 仍走默认代理，关闭后改用 IP 池 | Worker=1",
		"IP 池开启后，没有自己代理的账户不再使用默认代理 PROXY",
		"有账户固定了与默认代理相同的地址，不会使用 IP 池 | 账户=1",
	} {
		if countProxyPoolLogs(registry, fragment) != 1 {
			t.Fatalf("缺少日志 %q：%v", fragment, proxyPoolLogLines(registry))
		}
	}
	if ids := pool.pinnedDefault(); !slices.Equal(ids, []string{"pinned@example.com"}) {
		t.Fatalf("pinnedDefault = %v", ids)
	}
	worker.release()
	if binding := owner.binding("worker@example.com"); binding == nil || binding.Active || !strings.Contains(binding.URL, "isp.example.com") {
		t.Fatalf("直通租约归还后应切到 IP 池：%+v", binding)
	}
	// static → off：池租约计入 stale，空闲账户回到 PROXY
	pooled := pool.acquire("worker@example.com", "worker@example.com", proxyPoolKindWorker, "worker", "")
	pool.configure(off, proxyPoolTestGlobal, 188)
	if owner.binding("idle@example.com") != nil || owner.effective("idle@example.com", proxyPoolTestGlobal) != proxyPoolTestGlobal {
		t.Fatal("关闭后空闲账户应回到 PROXY")
	}
	if status := pool.status(); status == nil || status.Stale != 1 || status.Mode != config.ProxyPoolOff {
		t.Fatalf("关闭后仍有浏览器在用池出口时应返回状态：%+v", status)
	}
	if countProxyPoolLogs(registry, "IP 池已关闭 | 仍在使用旧出口的浏览器=1（关闭后归还）") != 1 {
		t.Fatal("缺少关闭日志")
	}
	pooled.release()
	if pool.status() != nil {
		t.Fatal("关闭且没有旧出口时状态为空")
	}
	// 缩小端口段与改地址：段外的租约照常归还、之后不再分出
	pool.configure(staticProxyPool(10001, 10200), proxyPoolTestGlobal, 188)
	held := make([]*proxyPoolLease, 0, 3)
	for _, id := range []string{"worker@example.com", "idle@example.com"} {
		held = append(held, pool.acquire(id, id, proxyPoolKindWorker, id, ""))
	}
	pool.configure(staticProxyPool(10101, 10200), proxyPoolTestGlobal, 188)
	if status := pool.status(); status.Stale != 2 || status.InUse != 0 {
		t.Fatalf("缩小端口段后段外租约计入 stale：%+v", status)
	}
	moved := pool.acquire("new@example.com", "new@example.com", proxyPoolKindWorker, "new", "")
	if port := leasePort(t, moved); port < 10101 {
		t.Fatalf("新租约只用新段，得到 %d", port)
	}
	changedURL := staticProxyPool(10101, 10200)
	changedURL.URL = "isp.example.com:10000:user-static:another"
	pool.configure(changedURL, proxyPoolTestGlobal, 188)
	if status := pool.status(); status.Stale != 3 {
		t.Fatalf("改地址后旧 URL 的租约计入 stale：%+v", status)
	}
	for _, lease := range append(held, moved) {
		lease.release()
	}
	// 只改容量：只判断端口是否够，状态切换时一行日志（100 个端口、需要 188 时已 WARN）
	if pool.configure(changedURL, proxyPoolTestGlobal, 90) {
		t.Fatal("只改容量应返回无变化")
	}
	if countProxyPoolLogs(registry, "IP 池端口已足够 | 端口=100 | 需要=90") != 1 || pool.status().Capacity != 90 {
		t.Fatalf("端口恢复够用时应 INFO 一行：%v", proxyPoolLogLines(registry))
	}
	before := len(proxyPoolLogLines(registry))
	pool.configure(changedURL, proxyPoolTestGlobal, 95)
	if len(proxyPoolLogLines(registry)) != before {
		t.Fatal("容量变化但状态没有切换时不记日志")
	}
	pool.configure(changedURL, proxyPoolTestGlobal, 101)
	if countProxyPoolLogs(registry, "IP 池端口少于可能同时运行的浏览器数，超出的会与其他账号共用端口 | 端口=100 | 需要=101") != 1 {
		t.Fatalf("容量超过端口数时应 WARN：%v", proxyPoolLogLines(registry))
	}
	pool.configure(changedURL, proxyPoolTestGlobal, 102)
	if countProxyPoolLogs(registry, "需要=102") != 0 {
		t.Fatal("状态没有切换时不重复 WARN")
	}
	for _, line := range proxyPoolLogLines(registry) {
		if strings.Contains(line, proxyPoolTestPassword) || strings.Contains(line, "user-static") {
			t.Fatalf("日志含账号密码：%s", line)
		}
	}
}

// TestProxyPoolStartupWarnings 启动时账户在 attach 之后才知道：固定了默认代理的账户在 attach 时提示一次；nil 账户池安全
func TestProxyPoolStartupWarnings(t *testing.T) {
	clock := newProxyPoolTestClock()
	pool, registry := newTestProxyPool(t, clock, "", "")
	pool.configure(staticProxyPool(10001, 10010), proxyPoolTestGlobal, 5)
	pinned := "有账户固定了与默认代理相同的地址"
	if countProxyPoolLogs(registry, pinned) != 0 {
		t.Fatal("还不知道账户时不应提示")
	}
	owner := newFakeProxyOwner("a@example.com", "pinned@example.com")
	owner.add("pinned@example.com", " "+proxyPoolTestGlobal+" ")
	pool.attach(owner)
	pool.refresh(owner, "a@example.com")
	pool.attach(owner)
	if countProxyPoolLogs(registry, pinned+"，不会使用 IP 池 | 账户=1") != 1 {
		t.Fatalf("attach 后应提示一次：%v", proxyPoolLogLines(registry))
	}
	pool.configure(config.ProxyPool{Mode: config.ProxyPoolOff}, proxyPoolTestGlobal, 5)
	pool.configure(staticProxyPool(10001, 10010), proxyPoolTestGlobal, 5)
	if countProxyPoolLogs(registry, pinned) != 2 {
		t.Fatal("重新开启后应再提示一次")
	}
	var nilPool *aistudio.AccountPool
	pool.attach(nilPool)
	pool.refresh(nilPool, "a@example.com")
	if pool.acquire("a@example.com", "a@example.com", proxyPoolKindWorker, "a", "").Pooled() == false {
		t.Fatal("没有账户池时照常分配")
	}
}

// TestProxyPoolModeRoundTrip P11：static→off→static、static→dynamic→static 期间 Worker 一直持有端口，回到 static 后这些端口不会再分给别人
func TestProxyPoolModeRoundTrip(t *testing.T) {
	for _, middle := range []config.ProxyPool{
		{Mode: config.ProxyPoolOff, URL: proxyPoolTestStaticURL},
		dynamicProxyPool(),
	} {
		t.Run(middle.Mode, func(t *testing.T) {
			clock := newProxyPoolTestClock()
			pool, _ := newTestProxyPool(t, clock, "", "")
			ids := proxyPoolTestIDs(4)
			pool.configure(staticProxyPool(10001, 10003), "", 3)
			pool.attach(newFakeProxyOwner(ids...))
			held := pool.acquire(ids[0], ids[0], proxyPoolKindWorker, "held", "")
			heldPort := leasePort(t, held)
			pool.configure(middle, "", 3)
			pool.configure(staticProxyPool(10001, 10003), "", 3)
			for _, id := range ids[1:3] {
				if lease := pool.acquire(id, id, proxyPoolKindWorker, id, ""); leasePort(t, lease) == heldPort || lease.shared {
					t.Fatalf("仍被持有的端口 %d 不应分给 %s", heldPort, id)
				}
			}
			held.release()
			if lease := pool.acquire(ids[3], ids[3], proxyPoolKindWorker, ids[3], ""); leasePort(t, lease) != heldPort {
				t.Fatalf("归还后端口 %d 应可再分", heldPort)
			}
		})
	}
}

// TestProxyPoolDefaultOutlet P12：默认出口按“归自己的端口 > 空闲端口 > PROXY > 哈希端口”；端口被占、冷却时重推；关闭时清除
func TestProxyPoolDefaultOutlet(t *testing.T) {
	clock := newProxyPoolTestClock()
	pool, _ := newTestProxyPool(t, clock, "", "")
	owner := newFakeProxyOwner("a@example.com", "b@example.com", "c@example.com")
	pool.configure(staticProxyPool(10001, 10002), proxyPoolTestGlobal, 2)
	pool.attach(owner)
	// a 独占过 10001（归属 a）：归还后 a 的默认出口回到 10001
	a := pool.acquire("a@example.com", "a@example.com", proxyPoolKindWorker, "a", "")
	aPort := leasePort(t, a)
	a.release()
	if binding := owner.binding("a@example.com"); binding == nil || binding.Slot != strconv.Itoa(aPort) || binding.Active {
		t.Fatalf("a 的默认出口应回到自己的端口：%+v", binding)
	}
	// b 的默认出口在某个空闲端口上；被别人占用后重推到另一个空闲端口
	bDefault := owner.binding("b@example.com")
	if bDefault == nil || bDefault.Active {
		t.Fatalf("b 应有默认出口：%+v", bDefault)
	}
	occupier := pool.acquire("c@example.com", "c@example.com", proxyPoolKindWorker, "c", "")
	if occupier.Slot() == owner.binding("b@example.com").Slot {
		t.Fatal("端口被占用后默认出口应换到别处")
	}
	// 两个端口都被占：没有空闲端口时退到 PROXY（清除绑定）
	other := pool.acquire("a@example.com", "a@example.com", proxyPoolKindWorker, "a", "")
	if binding := owner.binding("b@example.com"); binding != nil || owner.effective("b@example.com", proxyPoolTestGlobal) != proxyPoolTestGlobal {
		t.Fatalf("没有空闲端口时应退到 PROXY：%+v", binding)
	}
	other.release()
	occupier.release()
	// PROXY 为空、没有空闲端口时用哈希端口（与别人共用）
	pool.configure(staticProxyPool(10001, 10002), "", 2)
	x := pool.acquire("x", "x", proxyPoolKindWorker, "x", "")
	y := pool.acquire("y", "y", proxyPoolKindWorker, "y", "")
	if binding := owner.binding("b@example.com"); binding == nil || binding.Active || binding.URL == "" {
		t.Fatalf("PROXY 为空时应退到哈希端口：%+v", binding)
	}
	x.release()
	y.release()
	// 冷却时重推落在它上面的默认出口
	pool.mu.Lock()
	defaultPort := pool.accounts["b@example.com"].defaultPort
	pool.mu.Unlock()
	pool.mu.Lock()
	pool.coolLocked(defaultPort, proxyPoolRuleUpstream, "测试", clock.Now())
	pool.mu.Unlock()
	if binding := owner.binding("b@example.com"); binding == nil || binding.Slot == strconv.Itoa(defaultPort) {
		t.Fatalf("冷却后默认出口应重推：%+v", binding)
	}
	// 关闭时清除
	pool.configure(config.ProxyPool{Mode: config.ProxyPoolOff}, "", 2)
	for _, id := range []string{"a@example.com", "b@example.com", "c@example.com"} {
		if owner.binding(id) != nil {
			t.Fatalf("关闭后 %s 的绑定应清除", id)
		}
	}
	if label := pool.slotLabel("a@example.com"); label != "" {
		t.Fatalf("关闭后 slotLabel = %q", label)
	}
}

// TestProxyPoolLoginLeases P13：并发的新账户登录各自独占；adopt 后首次 Worker 回到该端口；动态 adopt 后账户会话号 = 登录临时会话号并持久化；
// 已有 Worker 的账户重新登录共用它的端口
func TestProxyPoolLoginLeases(t *testing.T) {
	clock := newProxyPoolTestClock()
	path := filepath.Join(t.TempDir(), proxyPoolFileName)
	pool, registry := newTestProxyPool(t, clock, path, "")
	owner := newFakeProxyOwner("old@example.com")
	pool.configure(staticProxyPool(10001, 10010), "", 5)
	pool.attach(owner)
	first := pool.acquire("login:one", "", proxyPoolKindLogin, "auth", "")
	second := pool.acquire("login:two", "", proxyPoolKindLogin, "auth", "")
	if first.Slot() == second.Slot() || !first.Pooled() || !second.Pooled() {
		t.Fatalf("并发登录应各自独占：%s / %s", first.Slot(), second.Slot())
	}
	if status := pool.status(); len(status.Assignments) != 2 || status.Assignments[0].Kind != proxyPoolKindLogin || status.Assignments[0].AccountID != "" {
		t.Fatalf("登录租约的状态 = %+v", status.Assignments)
	}
	first.adopt("new@example.com")
	owner.add("new@example.com", "")
	pool.refresh(owner, "new@example.com")
	if binding := owner.binding("new@example.com"); binding == nil || !binding.Active || binding.Slot != first.Slot() {
		t.Fatalf("adopt 后登录期间账户绑定为登录出口：%+v", binding)
	}
	first.release()
	second.release()
	worker := pool.acquire("new@example.com", "new@example.com", proxyPoolKindWorker, "new", "")
	if worker.Slot() != first.Slot() {
		t.Fatalf("adopt 后首次 Worker 应回到登录端口 %s，得到 %s", first.Slot(), worker.Slot())
	}
	relogin := pool.acquire("new@example.com", "new@example.com", proxyPoolKindLogin, "new", "")
	if relogin.Slot() != worker.Slot() || countProxyPoolLogs(registry, "来源=同账号 | 用途=登录") != 1 {
		t.Fatal("已有 Worker 的账户重新登录应共用它的端口")
	}
	relogin.release()
	worker.release()

	pool.configure(dynamicProxyPool(), "", 5)
	login := pool.acquire("login:three", "", proxyPoolKindVerify, "auth", "")
	if !regexp.MustCompile(`^n[0-9a-f]{11}$`).MatchString(login.Slot()) {
		t.Fatalf("新账户登录的临时会话号 = %q", login.Slot())
	}
	login.adopt("dyn@example.com")
	login.release()
	owner.add("dyn@example.com", "")
	pool.refresh(owner, "dyn@example.com")
	if binding := owner.binding("dyn@example.com"); binding == nil || binding.Slot != login.Slot() {
		t.Fatalf("动态 adopt 后账户会话号应为登录用的会话号：%+v", binding)
	}
	dynWorker := pool.acquire("dyn@example.com", "dyn@example.com", proxyPoolKindWorker, "dyn", "")
	if dynWorker.URL() != login.URL() {
		t.Fatal("登录与之后的 Worker 应同一出口")
	}
	dynWorker.release()
	pool.flush()
	restarted, _ := newTestProxyPool(t, clock, path, "")
	restarted.configure(dynamicProxyPool(), "", 5)
	if lease := restarted.acquire("dyn@example.com", "dyn@example.com", proxyPoolKindWorker, "dyn", ""); lease.Slot() != login.Slot() {
		t.Fatalf("采用的会话号应持久化：%s", lease.Slot())
	}
	if probe := pool.probeSession(); !regexp.MustCompile(`^t[0-9a-f]{14}$`).MatchString(probe) {
		t.Fatalf("抽测会话号 = %q", probe)
	}
}

// TestProxyPoolExternal P14：账户固定代理或 PROXY 指向段内端口时不分配、计入 external；账户改代理后 refresh 更新
func TestProxyPoolExternal(t *testing.T) {
	clock := newProxyPoolTestClock()
	pool, registry := newTestProxyPool(t, clock, "", "")
	owner := newFakeProxyOwner("a@example.com", "fixed@example.com")
	owner.add("fixed@example.com", "http://other:pw@isp.example.com:10002")
	global := "isp.example.com:10003:user-static:" + proxyPoolTestPassword
	pool.configure(staticProxyPool(10001, 10004), global, 2)
	pool.attach(owner)
	if status := pool.status(); status.External != 2 {
		t.Fatalf("external = %d", status.External)
	}
	if countProxyPoolLogs(registry, "IP 池端口段与账户固定代理或 PROXY 重叠，这些端口不再分配 | 端口数=2 | 账户数=1") != 1 {
		t.Fatalf("外部占用日志：%v", proxyPoolLogLines(registry))
	}
	var ports []int
	for index := 0; index < 2; index++ {
		id := fmt.Sprintf("w%d", index)
		ports = append(ports, leasePort(t, pool.acquire(id, id, proxyPoolKindWorker, id, "")))
	}
	sort.Ints(ports)
	if !slices.Equal(ports, []int{10001, 10004}) {
		t.Fatalf("外部占用的端口不应分配：%v", ports)
	}
	owner.add("fixed@example.com", "http://198.51.100.1:8080")
	pool.refresh(owner, "fixed@example.com")
	if status := pool.status(); status.External != 1 {
		t.Fatalf("改代理后 external = %d", status.External)
	}
	if lease := pool.acquire("w9", "w9", proxyPoolKindWorker, "w9", ""); lease.Slot() != "10002" {
		t.Fatalf("不再外部占用的端口应可分配，得到 %s", lease.Slot())
	}
}

// TestProxyPoolCrossProcessLock P15：两个池实例用同一锁目录不会分出同一端口；被锁的端口 1 分钟后再试；共用端口不重复加锁；
// 归还释放锁；锁目录不可用时 WARN 一次、照常分配
func TestProxyPoolCrossProcessLock(t *testing.T) {
	clock := newProxyPoolTestClock()
	lockDir := filepath.Join(t.TempDir(), "runtime-leases")
	first, _ := newTestProxyPool(t, clock, "", lockDir)
	second, _ := newTestProxyPool(t, clock, "", lockDir)
	for _, pool := range []*proxyPool{first, second} {
		pool.configure(staticProxyPool(10001, 10003), "", 3)
	}
	a := first.acquire("a", "a", proxyPoolKindWorker, "a", "")
	sameKey := first.acquire("a", "a", proxyPoolKindLogin, "a", "")
	b := second.acquire("b", "b", proxyPoolKindWorker, "b", "")
	if a.Slot() == b.Slot() || sameKey.Slot() != a.Slot() {
		t.Fatalf("两个进程不应分出同一端口：%s / %s", a.Slot(), b.Slot())
	}
	if status := second.status(); status.External != 1 {
		t.Fatalf("被其他进程锁住的端口应计入 external：%+v", status)
	}
	entries, err := os.ReadDir(lockDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if !regexp.MustCompile(`^proxy-port-[0-9a-f]{16}-1000[1-3]\.lock$`).MatchString(entry.Name()) {
			t.Fatalf("锁文件名 %q 不应含明文", entry.Name())
		}
	}
	sameKey.release()
	a.release()
	// 端口归还后另一个进程在重试时间之后可以拿到
	c := second.acquire("c", "c", proxyPoolKindWorker, "c", "")
	if c.Slot() == a.Slot() {
		t.Fatal("被锁住的端口在重试时间之前不应再试")
	}
	clock.Advance(proxyPoolLockedRetry)
	d := second.acquire("d", "d", proxyPoolKindWorker, "d", "")
	if d.Slot() != a.Slot() {
		t.Fatalf("重试时间之后应能锁到已归还的端口 %s，得到 %s", a.Slot(), d.Slot())
	}
	for _, lease := range []*proxyPoolLease{b, c, d} {
		lease.release()
	}
	second.mu.Lock()
	for port, state := range second.ports {
		if state.lock != nil {
			t.Errorf("端口 %d 归还后仍持有锁", port)
		}
	}
	second.mu.Unlock()

	// 锁目录不可用（是个文件）：WARN 一次、照常分配
	blocked := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blocked, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	broken, registry := newTestProxyPool(t, clock, "", filepath.Join(blocked, "locks"))
	broken.configure(staticProxyPool(10001, 10003), "", 3)
	for _, id := range []string{"x", "y"} {
		if !broken.acquire(id, id, proxyPoolKindWorker, id, "").Pooled() {
			t.Fatal("锁目录不可用时应照常分配")
		}
	}
	if countProxyPoolLogs(registry, "IP 池跨进程端口锁不可用") != 1 {
		t.Fatalf("锁目录不可用应 WARN 一次：%v", proxyPoolLogLines(registry))
	}
}

// TestProxyPoolOwnerSwitch P16：attach 新账户池后旧账户池的 refresh 被忽略；旧实例租约的归还推给新账户池；
// 新账户池没有该 ID 时忽略；已删除账户的租约计入 orphaned
func TestProxyPoolOwnerSwitch(t *testing.T) {
	clock := newProxyPoolTestClock()
	pool, registry := newTestProxyPool(t, clock, "", "")
	old := newFakeProxyOwner("a@example.com", "gone@example.com")
	pool.configure(staticProxyPool(10001, 10010), "", 5)
	pool.attach(old)
	a := pool.acquire("a@example.com", "a@example.com", proxyPoolKindWorker, "a", "")
	gone := pool.acquire("gone@example.com", "gone@example.com", proxyPoolKindWorker, "gone", "")
	fresh := newFakeProxyOwner("a@example.com", "b@example.com")
	pool.attach(fresh)
	if binding := fresh.binding("a@example.com"); binding == nil || !binding.Active || binding.Slot != a.Slot() {
		t.Fatalf("attach 后新账户池应按活动租约推送：%+v", binding)
	}
	old.add("a@example.com", "http://198.51.100.1:8080")
	pool.refresh(old, "a@example.com")
	if binding := fresh.binding("a@example.com"); binding == nil || !binding.Active {
		t.Fatal("旧账户池的 refresh 应被忽略")
	}
	oldPushes := old.pushes
	a.release()
	if old.pushes != oldPushes {
		t.Fatal("归还不应再推给旧账户池")
	}
	if binding := fresh.binding("a@example.com"); binding == nil || binding.Active {
		t.Fatalf("旧实例租约的归还应推给新账户池：%+v", binding)
	}
	status := pool.status()
	if status.Orphaned != 1 || countProxyPoolLogs(registry, "IP 池租约泄漏 | 已删除账户仍占用=1") != 1 {
		t.Fatalf("已删除账户的租约应计入 orphaned：%+v", status)
	}
	pool.status()
	if countProxyPoolLogs(registry, "IP 池租约泄漏") != 1 {
		t.Fatal("orphaned 没有增加时不重复 WARN")
	}
	gone.release()
	if pool.status().Orphaned != 0 {
		t.Fatal("归还后 orphaned 应清零")
	}
	// prime 写进还没进入账户池的对象
	account := &aistudio.Account{ID: "b@example.com"}
	pool.prime(account)
	if binding, ok := account.CurrentProxyBinding(); !ok || binding.Active || !strings.Contains(binding.URL, "isp.example.com") {
		t.Fatalf("prime 应写默认出口：%+v %v", binding, ok)
	}
	if !strings.HasPrefix(account.EffectiveProxy(proxyPoolTestGlobal), "http://user-static:") {
		t.Fatal("prime 后 EffectiveProxy 应是池出口")
	}
	own := &aistudio.Account{ID: "own@example.com", Config: aistudio.AccountConfig{Proxy: "http://198.51.100.1:8080"}}
	pool.prime(own)
	if _, ok := own.CurrentProxyBinding(); ok {
		t.Fatal("有自己代理的账户不应有绑定")
	}
}

// TestProxyPoolEndpointKeyChange P17：运行中换代理商或子账号时清掉端口冷却与归属，holders 不变
func TestProxyPoolEndpointKeyChange(t *testing.T) {
	clock := newProxyPoolTestClock()
	pool, _ := newTestProxyPool(t, clock, "", "")
	pool.configure(staticProxyPool(10001, 10003), "", 3)
	held := pool.acquire("a", "a", proxyPoolKindWorker, "a", "")
	pool.mu.Lock()
	pool.coolLocked(10003, proxyPoolRuleUpstream, "测试", clock.Now())
	pool.mu.Unlock()
	changed := staticProxyPool(10001, 10003)
	changed.URL = "isp.example.com:10000:user-other:" + proxyPoolTestPassword
	pool.configure(changed, "", 3)
	pool.mu.Lock()
	defer pool.mu.Unlock()
	if pool.coolingLocked(10003, clock.Now()) || pool.ports[leasePortLocked(held)].lastAccount != "" {
		t.Fatal("EndpointKey 变化应清掉冷却与归属")
	}
	if len(pool.ports[leasePortLocked(held)].holders) != 1 {
		t.Fatal("EndpointKey 变化不应动 holders")
	}
	if pool.portsKey != changed.EndpointKey() {
		t.Fatal("端口记录应归到新的 EndpointKey")
	}
}

func leasePortLocked(lease *proxyPoolLease) int { return lease.port }

// TestProxyPoolPersistence P18：记录往返；坏文件 WARN 后从空开始；endpoint_key 不同丢掉端口记录；权限 0600；
// 合并写入与 flush；文件不含主机名、用户名、密码
func TestProxyPoolPersistence(t *testing.T) {
	clock := newProxyPoolTestClock()
	directory := filepath.Join(t.TempDir(), "runtime")
	path := filepath.Join(directory, proxyPoolFileName)
	pool, _ := newTestProxyPool(t, clock, path, "")
	owner := newFakeProxyOwner("a@example.com", "b@example.com")
	pool.configure(staticProxyPool(10001, 10010), "", 5)
	pool.attach(owner)
	a := pool.acquire("a@example.com", "a@example.com", proxyPoolKindWorker, "a", "")
	pool.mu.Lock()
	pool.coolLocked(10005, proxyPoolRuleUpstream, "测试", clock.Now())
	pool.sessions["b@example.com"] = &proxyPoolSession{base: "nabcdef01234", generation: 3, rotatedAt: clock.Now(), usedAt: clock.Now()}
	timer := pool.saveTimer != nil
	pool.mu.Unlock()
	if !timer {
		t.Fatal("记录变化后应安排合并写盘")
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("合并写盘：延迟内不应立即写文件")
	}
	pool.flush()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("记录文件权限 = %v", info.Mode().Perm())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{proxyPoolTestPassword, "S3cr3t", "user-static", "isp.example.com", "http"} {
		if strings.Contains(string(data), secret) {
			t.Fatalf("记录文件含 %q：%s", secret, data)
		}
	}
	a.release()
	pool.flush()

	reloaded, _ := newTestProxyPool(t, clock, path, "")
	reloaded.configure(staticProxyPool(10001, 10010), "", 5)
	reloaded.mu.Lock()
	affinity := reloaded.affinity["a@example.com"]
	cooling := reloaded.coolingLocked(10005, clock.Now())
	session := reloaded.sessionLocked("b@example.com")
	owned := reloaded.ports[affinity.port].lastAccount
	reloaded.mu.Unlock()
	if affinity.port != leasePort(t, a) || !cooling || session != "nabcdef01234r3" || owned != "a@example.com" {
		t.Fatalf("往返不一致：affinity=%+v cooling=%v session=%q owned=%q", affinity, cooling, session, owned)
	}

	// endpoint_key 不同：丢掉端口记录（冷却、归属），保留账户亲和
	other := staticProxyPool(10001, 10010)
	other.URL = "isp2.example.com:10000:user-static:" + proxyPoolTestPassword
	switched, _ := newTestProxyPool(t, clock, path, "")
	switched.configure(other, "", 5)
	switched.mu.Lock()
	if switched.coolingLocked(10005, clock.Now()) || switched.ports[affinity.port].lastAccount != "" || switched.affinity["a@example.com"].port != affinity.port {
		t.Error("endpoint_key 不同应丢掉端口记录、保留亲和")
	}
	switched.mu.Unlock()

	// 坏文件：WARN 后从空开始；版本不认识同样处理
	for _, content := range []string{"{坏", `{"version": 9}`} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		broken, registry := newTestProxyPool(t, clock, path, "")
		if countProxyPoolLogs(registry, "IP 池记录读取失败，重新开始记录") != 1 || len(broken.affinity) != 0 {
			t.Fatalf("坏文件 %q 应 WARN 后从空开始", content)
		}
	}

	// 修剪：不在账户池里且 30 天没有用过的账户记录不再写盘
	pruned, _ := newTestProxyPool(t, clock, path, "")
	pruned.configure(staticProxyPool(10001, 10010), "", 5)
	pruned.attach(newFakeProxyOwner("kept@example.com"))
	pruned.mu.Lock()
	pruned.affinity["kept@example.com"] = proxyPoolAffinity{port: 10001, at: clock.Now().Add(-60 * 24 * time.Hour)}
	pruned.affinity["recent@example.com"] = proxyPoolAffinity{port: 10002, at: clock.Now().Add(-time.Hour)}
	pruned.affinity["old@example.com"] = proxyPoolAffinity{port: 10003, at: clock.Now().Add(-31 * 24 * time.Hour)}
	pruned.markDirtyLocked()
	pruned.mu.Unlock()
	pruned.flush()
	var record proxyPoolRecord
	data, _ = os.ReadFile(path)
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatal(err)
	}
	if _, ok := record.Static.Accounts["old@example.com"]; ok || len(record.Static.Accounts) != 2 {
		t.Fatalf("修剪结果 = %v", record.Static.Accounts)
	}
}

// TestProxyPoolStatus P19：状态接口的计数、冷却端口、裸 429 最多的端口、assignments 与容量
func TestProxyPoolStatus(t *testing.T) {
	clock := newProxyPoolTestClock()
	pool, _ := newTestProxyPool(t, clock, "", "")
	ids := proxyPoolTestIDs(3)
	owner := newFakeProxyOwner(ids...)
	pool.configure(staticProxyPool(10001, 10030), "", 25)
	pool.attach(owner)
	var leases []*proxyPoolLease
	for _, id := range ids {
		leases = append(leases, pool.acquire(id, id, proxyPoolKindWorker, id, ""))
	}
	login := pool.acquire("login:x", "", proxyPoolKindLogin, "auth", "")
	for index := 0; index < 3; index++ {
		pool.rateLimited(ids[0], false)
	}
	pool.rateLimited(ids[1], false)
	pool.mu.Lock()
	for port := 10021; port <= 10030; port++ {
		state := pool.portLocked(port)
		state.coolUntil, state.coolLevel = clock.Now().Add(time.Duration(port-10000)*time.Minute), 1
	}
	pool.mu.Unlock()
	status := pool.status()
	if status.Mode != config.ProxyPoolStatic || status.Total != 30 || status.InUse != 4 || status.Leases != 4 || status.Capacity != 25 ||
		status.PortStart != 10001 || status.PortEnd != 10030 || status.RelayLimit != 1<<20 {
		t.Fatalf("状态 = %+v", status)
	}
	if status.Cooling != 10 || len(status.CoolingPorts) != 10 || status.CoolingPorts[0].Port != 10021 {
		t.Fatalf("冷却端口 = %+v", status.CoolingPorts)
	}
	if len(status.HotPorts) != 2 || status.HotPorts[0].Port != leasePort(t, leases[0]) || status.HotPorts[0].RateLimited != 3 {
		t.Fatalf("hot_ports = %+v", status.HotPorts)
	}
	if len(status.Assignments) != 4 {
		t.Fatalf("assignments = %+v", status.Assignments)
	}
	for _, assignment := range status.Assignments {
		if assignment.Slot == login.Slot() {
			if assignment.Kind != proxyPoolKindLogin || assignment.AccountID != "" {
				t.Fatalf("登录租约 = %+v", assignment)
			}
		} else if assignment.Kind != proxyPoolKindWorker || !slices.Contains(ids, assignment.AccountID) {
			t.Fatalf("Worker 租约 = %+v", assignment)
		}
	}
	if label := pool.slotLabel(ids[0]); label != "端口="+leases[0].Slot() {
		t.Fatalf("slotLabel = %q", label)
	}
	encoded, _ := json.Marshal(status)
	if strings.Contains(string(encoded), proxyPoolTestPassword) || strings.Contains(string(encoded), "user-static") {
		t.Fatalf("状态含账号密码：%s", encoded)
	}
	for _, lease := range append(leases, login) {
		lease.release()
	}
}

// TestProxyPoolCheck “检查全部端口”：单飞、并发不超过 10、重复组与失败端口、错误脱敏、进度写进状态
func TestProxyPoolCheck(t *testing.T) {
	clock := newProxyPoolTestClock()
	pool, registry := newTestProxyPool(t, clock, "", "")
	pool.configure(staticProxyPool(10001, 10030), "", 10)
	var running, peak atomic.Int32
	release := make(chan struct{})
	probe := func(ctx context.Context, upstream string) api.ProxyTestProbe {
		current := running.Add(1)
		defer running.Add(-1)
		for {
			old := peak.Load()
			if current <= old || peak.CompareAndSwap(old, current) {
				break
			}
		}
		<-release
		port := upstream[strings.LastIndex(upstream, ":")+1:]
		switch port {
		case "10003":
			return api.ProxyTestProbe{Error: "407 " + upstream}
		case "10004", "10005":
			return api.ProxyTestProbe{IP: "203.0.113.200"}
		}
		return api.ProxyTestProbe{IP: "203.0.113." + port[3:]}
	}
	if !pool.startCheck(context.Background(), staticProxyPool(10001, 10030), probe) {
		t.Fatal("应启动检查")
	}
	if pool.startCheck(context.Background(), staticProxyPool(10001, 10030), probe) {
		t.Fatal("已有检查在进行时应返回 false")
	}
	if pool.startCheck(context.Background(), dynamicProxyPool(), probe) {
		t.Fatal("只支持静态")
	}
	if status := pool.status(); status.Check == nil || !status.Check.Running || status.Check.Total != 30 {
		t.Fatalf("检查进行中的状态 = %+v", status.Check)
	}
	close(release)
	deadline := time.Now().Add(10 * time.Second)
	for {
		status := pool.status()
		if !status.Check.Running {
			check := status.Check
			if check.Done != 30 || check.OK != 29 || check.DistinctIPs != 28 || len(check.Duplicates) != 1 ||
				!slices.Equal(check.Duplicates[0].Ports, []int{10004, 10005}) || len(check.Failed) != 1 || check.Failed[0].Port != 10003 {
				t.Fatalf("检查结果 = %+v", check)
			}
			if strings.Contains(check.Failed[0].Error, proxyPoolTestPassword) || strings.Contains(check.Failed[0].Error, "user-static") {
				t.Fatalf("失败说明未脱敏：%q", check.Failed[0].Error)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("检查没有结束")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if peak.Load() > proxyPoolCheckConcurrency {
		t.Fatalf("并发 %d 超过上限", peak.Load())
	}
	if countProxyPoolLogs(registry, "IP 池检查完成 | 出口=http://isp.example.com | 端口=30 | 成功=29 | 不同 IP=28 | 重复=1 组 | 失败=1") != 1 {
		t.Fatalf("检查日志：%v", proxyPoolLogLines(registry))
	}
	if !pool.startCheck(context.Background(), staticProxyPool(10001, 10002), func(context.Context, string) api.ProxyTestProbe {
		return api.ProxyTestProbe{IP: "198.51.100.1"}
	}) {
		t.Fatal("上一次结束后应能再次检查")
	}
}

// checkProxyPoolInvariants 断言 IP 池的不变量（P20）：持有计数等于活动租约数；同一端口不同账户只在共用时同时持有；
// 端口锁只在有持有者时持有；有活动租约的账户绑定等于绑定所用的租约（最新的已启动 Worker，没有时为最新租约），其余不是活动绑定；
// 推送缓存与账户池一致；默认出口在等的账户没有活动租约
func checkProxyPoolInvariants(t *testing.T, pool *proxyPool, owner *fakeProxyOwner) {
	t.Helper()
	if err := proxyPoolInvariantError(pool, owner); err != nil {
		t.Fatal(err)
	}
}

// proxyPoolInvariantError 为 checkProxyPoolInvariants 的实现：返回第一个不成立的不变量
func proxyPoolInvariantError(pool *proxyPool, owner *fakeProxyOwner) error {
	pool.mu.Lock()
	defer pool.mu.Unlock()
	type holder struct {
		port int
		key  string
	}
	counts := make(map[holder]int, pool.pooledLeases)
	exclusive := make(map[int]string)
	pooled := 0
	for key, leases := range pool.byKey {
		for _, lease := range leases {
			if lease.released || lease.key != key {
				return fmt.Errorf("byKey 里有已归还或键不一致的租约：%+v", lease)
			}
			if !lease.pooled {
				continue
			}
			pooled++
			if lease.port == 0 {
				continue
			}
			counts[holder{lease.port, key}]++
			if !lease.shared {
				// 独占按账户算：同一账户的不同键（adopt 过来的登录租约与 Worker）共用一个端口不算破坏独占
				owner := lease.accountID
				if owner == "" {
					owner = key
				}
				if other, ok := exclusive[lease.port]; ok && other != owner {
					return fmt.Errorf("端口 %d 同时被不同账户独占：%s、%s", lease.port, other, owner)
				}
				exclusive[lease.port] = owner
			}
		}
	}
	if pooled != pool.pooledLeases {
		return fmt.Errorf("pooledLeases = %d，实际 %d", pool.pooledLeases, pooled)
	}
	entries := 0
	for port, state := range pool.ports {
		for key, count := range state.holders {
			entries++
			if counts[holder{port, key}] != count {
				return fmt.Errorf("端口 %d 键 %s 持有 %d、租约 %d", port, key, count, counts[holder{port, key}])
			}
		}
		if state.lock != nil && len(state.holders) == 0 {
			return fmt.Errorf("端口 %d 没有持有者却仍持有锁", port)
		}
	}
	if entries != len(counts) {
		return fmt.Errorf("持有者 %d 项与租约 %d 项不一致", entries, len(counts))
	}
	if owner == nil || pool.owner != proxyPoolOwner(owner) {
		return nil
	}
	owner.mu.Lock()
	defer owner.mu.Unlock()
	for _, id := range owner.ids {
		binding := owner.bindings[id]
		state := pool.accounts[id]
		if state != nil && state.known && !proxyBindingEqual(binding, state.binding) {
			return fmt.Errorf("%s 的推送缓存 %+v 与账户池 %+v 不一致", id, state.binding, binding)
		}
		if owner.proxies[id] != "" {
			continue
		}
		if lease := pool.bindingLeaseLocked(id); lease != nil {
			if binding == nil || !binding.Active || binding.URL != lease.url || binding.Slot != lease.slot {
				return fmt.Errorf("%s 有活动租约，绑定应等于绑定所用的租约：%+v / %+v", id, binding, lease)
			}
		} else if binding != nil && binding.Active {
			return fmt.Errorf("%s 没有活动租约却是活动绑定：%+v", id, binding)
		}
	}
	for id := range pool.defaultWaiting {
		if len(pool.byAccount[id]) > 0 {
			return fmt.Errorf("%s 有活动租约却记在 defaultWaiting 里", id)
		}
	}
	return nil
}

// TestProxyPoolConcurrency P20：64 个协程随机申请、归还、登录与 adopt、裸 429、成功、启动失败、切换设置、切换账户池、refresh、状态，
// 每步后断言不变量；全部归还后所有端口没有持有者、锁全部释放
func TestProxyPoolConcurrency(t *testing.T) {
	clock := newProxyPoolTestClock()
	lockDir := filepath.Join(t.TempDir(), "locks")
	// 不接运行日志：这里只看不变量，几十万行日志只会拖慢测试
	pool := newProxyPool(proxyPoolOptions{
		path: filepath.Join(t.TempDir(), proxyPoolFileName), lockDir: lockDir, now: clock.Now,
		relayLimit: func() int { return 1 << 20 },
	})
	ids := proxyPoolTestIDs(40)
	var ownerMu sync.Mutex
	owner := newFakeProxyOwner(ids...)
	owner.add(ids[39], "isp.example.com:10005:x:y")
	pool.configure(staticProxyPool(10001, 10030), proxyPoolTestGlobal, 30)
	pool.attach(owner)
	configs := []config.ProxyPool{
		staticProxyPool(10001, 10030), staticProxyPool(10011, 10040), staticProxyPool(10001, 10010), dynamicProxyPool(),
		{Mode: config.ProxyPoolOff, URL: proxyPoolTestStaticURL},
	}
	goroutines, steps := 64, 2000
	if testing.Short() {
		steps = 200
	}
	var wait sync.WaitGroup
	for worker := 0; worker < goroutines; worker++ {
		wait.Add(1)
		go func(seed int64) {
			defer wait.Done()
			random := rand.New(rand.NewSource(seed))
			var held []*proxyPoolLease
			for step := 0; step < steps; step++ {
				id := ids[random.Intn(len(ids))]
				if len(held) >= 3 {
					// 每个协程最多同时持有 3 个租约：64 个协程合计约 190 个浏览器，30 个端口时多数申请走共用
					index := random.Intn(len(held))
					held[index].release()
					held = slices.Delete(held, index, index+1)
				}
				switch operation := random.Intn(100); {
				case operation < 30:
					lease := pool.acquire(id, id, proxyPoolKindWorker, id, proxyPoolTestGlobal)
					if random.Intn(4) == 0 {
						lease.launchFailed(upstreamFailure, random.Intn(3) == 0)
					} else {
						lease.attach(&fakeProxyCloser{})
						held = append(held, lease)
					}
				case operation < 50:
					if len(held) > 0 {
						index := random.Intn(len(held))
						held[index].release()
						held = slices.Delete(held, index, index+1)
					}
				case operation < 56:
					login := pool.acquire(fmt.Sprintf("login:%d-%d", seed, step), "", proxyPoolKindLogin, "auth", "")
					if random.Intn(2) == 0 {
						login.adopt(id)
					}
					held = append(held, login)
				case operation < 70:
					pool.rateLimited(id, false)
				case operation < 80:
					pool.succeeded(id)
				case operation < 83:
					pool.configure(configs[random.Intn(len(configs))], proxyPoolTestGlobal, 20+random.Intn(20))
				case operation < 85:
					ownerMu.Lock()
					next := newFakeProxyOwner(ids...)
					next.add(ids[39], "isp.example.com:10005:x:y")
					owner = next
					ownerMu.Unlock()
					pool.attach(next)
				case operation < 88:
					ownerMu.Lock()
					current := owner
					ownerMu.Unlock()
					pool.refresh(current, id)
				case operation < 92:
					if status := pool.status(); status != nil && status.Leases < 0 {
						panic("负数租约")
					}
				case operation < 94:
					pool.prime(&aistudio.Account{ID: id})
				case operation < 96:
					clock.Advance(time.Duration(random.Intn(120)) * time.Second)
					// 定时器到期的路径：冷却到期、其他进程锁的重试时间到时重推默认出口在等的账户
					pool.recheckFired()
				default:
					_ = pool.slotLabel(id)
				}
				ownerMu.Lock()
				current := owner
				ownerMu.Unlock()
				checkProxyPoolInvariants(t, pool, current)
			}
			for _, lease := range held {
				lease.release()
			}
		}(int64(worker + 1))
	}
	wait.Wait()
	checkProxyPoolInvariants(t, pool, owner)
	pool.mu.Lock()
	defer pool.mu.Unlock()
	if pool.pooledLeases != 0 || len(pool.byKey) != 0 || len(pool.byAccount) != 0 {
		t.Fatalf("全部归还后仍有租约：%d %d %d", pool.pooledLeases, len(pool.byKey), len(pool.byAccount))
	}
	for port, state := range pool.ports {
		if len(state.holders) != 0 || state.lock != nil {
			t.Fatalf("端口 %d 全部归还后仍有持有者或锁", port)
		}
	}
}

// TestProxyPoolWorkerJoinsAdoptedLogin 复查 1-2：新账户 adopt 之后、登录租约归还之前启动的 Worker 与登录共用端口（同账号），
// 不另选端口；登录租约归还后亲和仍是这个端口，端口不会分给别人
func TestProxyPoolWorkerJoinsAdoptedLogin(t *testing.T) {
	clock := newProxyPoolTestClock()
	pool, registry := newTestProxyPool(t, clock, "", "")
	owner := newFakeProxyOwner()
	pool.configure(staticProxyPool(10001, 10010), "", 5)
	pool.attach(owner)
	login := pool.acquire("login:one", "", proxyPoolKindLogin, "auth", "")
	login.adopt("new@example.com")
	owner.add("new@example.com", "")
	pool.refresh(owner, "new@example.com")
	worker := pool.acquire("new@example.com", "new@example.com", proxyPoolKindWorker, "new", "")
	if worker.Slot() != login.Slot() || worker.shared || countProxyPoolLogs(registry, "来源=同账号 | 用途=Worker") != 1 {
		t.Fatalf("登录租约归还前启动的 Worker 应与登录共用端口 %s，得到 %s：%v", login.Slot(), worker.Slot(), proxyPoolLogLines(registry))
	}
	worker.attach(&fakeProxyCloser{})
	login.release()
	checkProxyPoolInvariants(t, pool, owner)
	pool.mu.Lock()
	affinity := pool.affinity["new@example.com"].port
	holders := len(pool.ports[leasePortLocked(worker)].holders)
	pool.mu.Unlock()
	if affinity != leasePort(t, login) || holders != 1 {
		t.Fatalf("登录归还后亲和 = %d、持有者 = %d，期望 %s、1", affinity, holders, login.Slot())
	}
	if binding := owner.binding("new@example.com"); binding == nil || !binding.Active || binding.Slot != worker.Slot() {
		t.Fatalf("绑定应为 Worker 的出口：%+v", binding)
	}
	worker.release()
	if other := pool.acquire("other", "other", proxyPoolKindWorker, "other", ""); other.Slot() == login.Slot() {
		t.Fatal("归新账户的端口不应先分给别人")
	}
}

// TestProxyPoolBindingFollowsAttachedWorker 复查 1-4：先启后停时新 Worker 启动成功（attach）之前，绑定、裸 429 与 slotLabel
// 都跟着正在服务的旧浏览器；验证与 Worker 共用冷却中的端口；启动失败不来回切换，attach 后切到新浏览器
func TestProxyPoolBindingFollowsAttachedWorker(t *testing.T) {
	clock := newProxyPoolTestClock()
	pool, _ := newTestProxyPool(t, clock, "", "")
	const id = "a@example.com"
	owner := newFakeProxyOwner(id)
	pool.configure(staticProxyPool(10001, 10003), "", 3)
	pool.attach(owner)
	oldCloser := &fakeProxyCloser{}
	old := pool.acquire(id, id, proxyPoolKindWorker, "a", "")
	old.attach(oldCloser)
	oldPort := leasePort(t, old)
	pool.mu.Lock()
	pool.coolLocked(oldPort, proxyPoolRuleUpstream, "测试", clock.Now())
	pool.mu.Unlock()

	next := pool.acquire(id, id, proxyPoolKindWorker, "a", "")
	if next.Slot() == old.Slot() {
		t.Fatal("旧端口在冷却：新 Worker 应换到新端口")
	}
	if binding := owner.binding(id); binding == nil || !binding.Active || binding.Slot != old.Slot() {
		t.Fatalf("新浏览器起来之前绑定应仍是旧浏览器的出口 %s：%+v", old.Slot(), binding)
	}
	pool.rateLimited(id, false)
	pool.mu.Lock()
	oldHits, nextHits := len(pool.ports[oldPort].hits), len(pool.portLocked(leasePortLocked(next)).hits)
	pool.mu.Unlock()
	if oldHits != 1 || nextHits != 0 {
		t.Fatalf("裸 429 应记在绑定所用的端口上：旧 %d、新 %d", oldHits, nextHits)
	}
	if label := pool.slotLabel(id); label != "端口="+old.Slot() {
		t.Fatalf("slotLabel = %q", label)
	}
	verify := pool.acquire(id, id, proxyPoolKindVerify, "a", "")
	if verify.Slot() != old.Slot() {
		t.Fatalf("验证不因冷却拒绝，应与正在服务的 Worker 共用 %s，得到 %s", old.Slot(), verify.Slot())
	}
	if binding := owner.binding(id); binding == nil || binding.Slot != old.Slot() {
		t.Fatalf("验证期间绑定仍是 Worker 的出口：%+v", binding)
	}
	verify.release()
	next.launchFailed(errors.New("页面加载失败"), false)
	if binding := owner.binding(id); binding == nil || !binding.Active || binding.Slot != old.Slot() {
		t.Fatalf("启动失败后绑定仍是旧出口：%+v", binding)
	}
	checkProxyPoolInvariants(t, pool, owner)

	retry := pool.acquire(id, id, proxyPoolKindWorker, "a", "")
	if binding := owner.binding(id); binding == nil || binding.Slot != old.Slot() {
		t.Fatalf("attach 之前绑定不变：%+v", binding)
	}
	retry.attach(&fakeProxyCloser{})
	if binding := owner.binding(id); binding == nil || !binding.Active || binding.Slot != retry.Slot() {
		t.Fatalf("attach 后绑定应切到新浏览器 %s：%+v", retry.Slot(), binding)
	}
	oldCloser.close()
	if binding := owner.binding(id); binding == nil || binding.Slot != retry.Slot() {
		t.Fatalf("旧浏览器关闭后仍是新出口：%+v", binding)
	}
	checkProxyPoolInvariants(t, pool, owner)
	retry.release()
}

// TestProxyPoolDefaultWaitingRepush 复查 1-3、2-1：端口用满时默认出口退回 PROXY（PROXY 为空时与别人共用哈希端口）的账户，
// 端口归还后等 proxyPoolWakeDelay 合并重推（期间端口又被 Worker 占用时不挪动、到时也不重算），冷却到期时（状态接口顺带检查或定时器）
// 重推回 IP 池；没有在等的账户时定时器停掉
func TestProxyPoolDefaultWaitingRepush(t *testing.T) {
	clock := newProxyPoolTestClock()
	pool, _ := newTestProxyPool(t, clock, "", "")
	const idle = "idle@example.com"
	owner := newFakeProxyOwner(idle, "a@example.com", "b@example.com")
	pool.configure(staticProxyPool(10001, 10002), proxyPoolTestGlobal, 2)
	pool.attach(owner)
	pushes := func() int {
		owner.mu.Lock()
		defer owner.mu.Unlock()
		return owner.pushes
	}
	a := pool.acquire("a@example.com", "a@example.com", proxyPoolKindWorker, "a", "")
	b := pool.acquire("b@example.com", "b@example.com", proxyPoolKindWorker, "b", "")
	aPort := leasePort(t, a)
	if binding := owner.binding(idle); binding != nil {
		t.Fatalf("端口用满时应退回 PROXY：%+v", binding)
	}
	// 归还后马上又有 Worker 来占：在等的账户不挪过去再挪回来，合并时刻到了也不重算（没有可用端口）
	a.release()
	if binding := owner.binding(idle); binding != nil {
		t.Fatalf("端口归还后应等 %s 合并重推：%+v", proxyPoolWakeDelay, binding)
	}
	a = pool.acquire("a@example.com", "a@example.com", proxyPoolKindWorker, "a", "")
	before := pushes()
	clock.Advance(proxyPoolWakeDelay)
	pool.status()
	if binding := owner.binding(idle); binding != nil || pushes() != before {
		t.Fatalf("端口又被占用时不应重推：%+v，推送 %d → %d", binding, before, pushes())
	}
	a.release()
	clock.Advance(proxyPoolWakeDelay - time.Second)
	pool.status()
	if binding := owner.binding(idle); binding != nil {
		t.Fatalf("合并时刻之前不应重推：%+v", binding)
	}
	clock.Advance(time.Second)
	pool.status()
	if binding := owner.binding(idle); binding == nil || binding.Slot != a.Slot() {
		t.Fatalf("合并时刻到了应回到 IP 池：%+v", binding)
	}

	// 归还的端口在冷却：仍走 PROXY，冷却到期后状态接口顺带重推
	cool := func() time.Time {
		t.Helper()
		pool.mu.Lock()
		defer pool.mu.Unlock()
		pool.coolLocked(aPort, proxyPoolRuleUpstream, "测试", clock.Now())
		return pool.ports[aPort].coolUntil
	}
	a = pool.acquire("a@example.com", "a@example.com", proxyPoolKindWorker, "a", "")
	if leasePort(t, a) != aPort || owner.binding(idle) != nil {
		t.Fatalf("a 回到 %d 后 idle 应退回 PROXY：%s %+v", aPort, a.Slot(), owner.binding(idle))
	}
	until := cool()
	a.release()
	if binding := owner.binding(idle); binding != nil {
		t.Fatalf("冷却中的端口不应做默认出口：%+v", binding)
	}
	clock.Advance(until.Sub(clock.Now()) - time.Second)
	pool.status()
	if binding := owner.binding(idle); binding != nil {
		t.Fatalf("冷却到期之前不应重推：%+v", binding)
	}
	clock.Advance(time.Second)
	pool.status()
	if binding := owner.binding(idle); binding == nil || binding.Slot != strconv.Itoa(aPort) {
		t.Fatalf("冷却到期后应回到 IP 池：%+v", binding)
	}

	// 定时器：没有申请、归还与状态查询时同样重推（先到合并时刻，端口仍在冷却，再到冷却到期）
	a = pool.acquire("a@example.com", "a@example.com", proxyPoolKindWorker, "a", "")
	until = cool()
	a.release()
	wake := clock.Now().Add(proxyPoolWakeDelay)
	armedAt := func() (bool, time.Time) {
		pool.mu.Lock()
		defer pool.mu.Unlock()
		return pool.recheckArmed, pool.recheckAt
	}
	if armed, at := armedAt(); owner.binding(idle) != nil || !armed || !at.Equal(wake) {
		t.Fatalf("应按合并时刻安排检查：armed=%v at=%v wake=%v", armed, at, wake)
	}
	clock.Advance(proxyPoolWakeDelay)
	pool.recheckFired()
	if armed, at := armedAt(); owner.binding(idle) != nil || !armed || !at.Equal(until) {
		t.Fatalf("端口仍在冷却：应改按冷却到期时刻安排检查：armed=%v at=%v until=%v", armed, at, until)
	}
	clock.Advance(until.Sub(clock.Now()))
	pool.recheckFired()
	if binding := owner.binding(idle); binding == nil || binding.Slot != strconv.Itoa(aPort) {
		t.Fatalf("定时器到期后应回到 IP 池：%+v", binding)
	}

	// PROXY 为空：端口用满时与别人共用哈希端口，端口空出来、到合并时刻后换到它
	pool.configure(staticProxyPool(10001, 10002), "", 2)
	a = pool.acquire("a@example.com", "a@example.com", proxyPoolKindWorker, "a", "")
	pool.mu.Lock()
	_, waiting := pool.defaultWaiting[idle]
	pool.mu.Unlock()
	if binding := owner.binding(idle); binding == nil || !waiting {
		t.Fatalf("PROXY 为空时应共用哈希端口并等待：%+v waiting=%v", binding, waiting)
	}
	b.release()
	clock.Advance(proxyPoolWakeDelay)
	pool.status()
	pool.mu.Lock()
	_, waiting = pool.defaultWaiting[idle]
	pool.mu.Unlock()
	if binding := owner.binding(idle); binding == nil || binding.Slot != b.Slot() || waiting {
		t.Fatalf("应换到空出来的端口 %s：%+v waiting=%v", b.Slot(), binding, waiting)
	}
	a.release()
	checkProxyPoolInvariants(t, pool, owner)
	pool.mu.Lock()
	defer pool.mu.Unlock()
	if pool.recheckArmed || len(pool.defaultWaiting) != 0 || !pool.wakeAt.IsZero() {
		t.Fatalf("没有在等的账户时定时器应停掉：armed=%v waiting=%v wakeAt=%v", pool.recheckArmed, pool.defaultWaiting, pool.wakeAt)
	}
}

// fakeProxyPortLock 为端口锁替身：Release 时从 held 里删掉自己的端口
type fakeProxyPortLock struct {
	mu   *sync.Mutex
	held map[int]bool
	port int
}

func (lock *fakeProxyPortLock) Release() error {
	lock.mu.Lock()
	delete(lock.held, lock.port)
	lock.mu.Unlock()
	return nil
}

// TestProxyPoolLockErrorBackoff 复查 1-5：端口锁出错后 5 分钟内照常分配、不加锁，到期再试，一段出错期只 WARN 一次；
// 恢复时给出错期间分出、仍持有的端口补锁，归还后锁全部释放
func TestProxyPoolLockErrorBackoff(t *testing.T) {
	clock := newProxyPoolTestClock()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	registry := newRequestRegistry(ctx)
	var mu sync.Mutex
	held := make(map[int]bool)
	failing, calls := true, 0
	pool := newProxyPool(proxyPoolOptions{
		lockDir: "locks", requests: registry, now: clock.Now, relayLimit: func() int { return 1 << 20 },
		tryLock: func(_, _ string, port int) (proxyPoolPortLock, bool, error) {
			mu.Lock()
			defer mu.Unlock()
			calls++
			if failing {
				return nil, false, errors.New("too many open files")
			}
			held[port] = true
			return &fakeProxyPortLock{mu: &mu, held: held, port: port}, true, nil
		},
	})
	pool.configure(staticProxyPool(10001, 10004), "", 4)
	state := func() (int, int) {
		mu.Lock()
		defer mu.Unlock()
		return calls, len(held)
	}
	a := pool.acquire("a", "a", proxyPoolKindWorker, "a", "")
	clock.Advance(time.Minute)
	b := pool.acquire("b", "b", proxyPoolKindWorker, "b", "")
	if count, locked := state(); !a.Pooled() || !b.Pooled() || count != 1 || locked != 0 {
		t.Fatalf("出错后 5 分钟内照常分配、不再加锁：调用 %d 次、持有 %d 把", count, locked)
	}
	clock.Advance(proxyPoolLockErrorRetry)
	c := pool.acquire("c", "c", proxyPoolKindWorker, "c", "")
	if count, _ := state(); count != 2 || countProxyPoolLogs(registry, "IP 池跨进程端口锁不可用") != 1 {
		t.Fatalf("到期后应再试一次、同一段出错期只 WARN 一次：调用 %d 次：%v", count, proxyPoolLogLines(registry))
	}
	c.release()
	clock.Advance(proxyPoolLockErrorRetry)
	mu.Lock()
	failing = false
	mu.Unlock()
	d := pool.acquire("d", "d", proxyPoolKindWorker, "d", "")
	if count, locked := state(); count != 5 || locked != 3 ||
		countProxyPoolLogs(registry, "INFO service IP 池跨进程端口锁已恢复 | 补锁端口=2 | 已被其他进程占用=0") != 1 {
		t.Fatalf("恢复后应给 a、b 补锁：调用 %d 次、持有 %d 把：%v", count, locked, proxyPoolLogLines(registry))
	}
	checkProxyPoolInvariants(t, pool, nil)
	for _, lease := range []*proxyPoolLease{a, b, d} {
		lease.release()
	}
	if _, locked := state(); locked != 0 {
		t.Fatalf("归还后仍持有 %d 把锁", locked)
	}
}

// TestProxyPoolDefaultAvoidsOtherProcess 复查 1-6：默认出口避开被其他进程锁住的端口，重试时间过了也只检测、不持有，
// 检测到锁已释放才再用；检测不影响其他进程之后加锁
func TestProxyPoolDefaultAvoidsOtherProcess(t *testing.T) {
	clock := newProxyPoolTestClock()
	lockDir := filepath.Join(t.TempDir(), "runtime-leases")
	first, _ := newTestProxyPool(t, clock, "", lockDir)
	second, _ := newTestProxyPool(t, clock, "", lockDir)
	first.configure(staticProxyPool(10001, 10002), "", 2)
	const idle = "idle@example.com"
	owner := newFakeProxyOwner(idle, "c@example.com")
	second.configure(staticProxyPool(10001, 10002), proxyPoolTestGlobal, 2)
	second.attach(owner)
	held := first.acquire("x", "x", proxyPoolKindWorker, "x", "")
	c := second.acquire("c@example.com", "c@example.com", proxyPoolKindWorker, "c", "")
	if c.Slot() == held.Slot() {
		t.Fatal("两个进程不应分出同一端口")
	}
	if binding := owner.binding(idle); binding != nil {
		t.Fatalf("一个端口被其他进程锁住、另一个被占用：应退回 PROXY：%+v", binding)
	}
	clock.Advance(proxyPoolLockedRetry)
	second.status()
	second.refresh(owner, idle)
	if binding := owner.binding(idle); binding != nil {
		t.Fatalf("重试时间过了但其他进程仍持有：默认出口应继续避开 %s：%+v", held.Slot(), binding)
	}
	held.release()
	clock.Advance(proxyPoolLockedRetry)
	second.status()
	if binding := owner.binding(idle); binding == nil || binding.Slot != held.Slot() {
		t.Fatalf("检测到锁已释放后应回到 IP 池 %s：%+v", held.Slot(), binding)
	}
	second.mu.Lock()
	remaining := len(second.lockedElsewhere)
	second.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("锁已释放的记录应清掉，剩 %d 条", remaining)
	}
	again := first.acquire("x", "x", proxyPoolKindWorker, "x", "")
	if again.Slot() != held.Slot() {
		t.Fatalf("检测不应持有锁：其他进程应能再锁到 %s，得到 %s", held.Slot(), again.Slot())
	}
	again.release()
	c.release()
}

// TestProxyPoolProbeRelocksSharedPort 复查 1-6：本进程当初没锁到、只能共用的端口，检测到其他进程已释放时补上锁（不再与之后的进程重复）
func TestProxyPoolProbeRelocksSharedPort(t *testing.T) {
	clock := newProxyPoolTestClock()
	lockDir := filepath.Join(t.TempDir(), "runtime-leases")
	first, _ := newTestProxyPool(t, clock, "", lockDir)
	second, _ := newTestProxyPool(t, clock, "", lockDir)
	first.configure(staticProxyPool(10001, 10001), "", 1)
	owner := newFakeProxyOwner("idle@example.com", "c@example.com")
	second.configure(staticProxyPool(10001, 10001), proxyPoolTestGlobal, 1)
	second.attach(owner)
	held := first.acquire("x", "x", proxyPoolKindWorker, "x", "")
	shared := second.acquire("c@example.com", "c@example.com", proxyPoolKindWorker, "c", "")
	if !shared.shared || shared.Slot() != held.Slot() {
		t.Fatalf("唯一的端口被其他进程锁住时只能共用：%+v", shared)
	}
	held.release()
	clock.Advance(proxyPoolLockedRetry)
	second.status()
	second.mu.Lock()
	relocked := second.ports[10001].lock != nil
	second.mu.Unlock()
	if !relocked {
		t.Fatal("其他进程释放后应给仍在共用的端口补锁")
	}
	if again := first.acquire("x", "x", proxyPoolKindWorker, "x", ""); !again.shared {
		t.Fatal("补锁后其他进程不应再独占这个端口")
	} else {
		again.release()
	}
	shared.release()
	second.mu.Lock()
	defer second.mu.Unlock()
	if second.ports[10001].lock != nil {
		t.Fatal("归还后应释放补上的锁")
	}
}

// TestProxyPoolDefaultSpread 复查 2-1：从零预热时默认出口按各账户自己的探测序列分散，每启动一个 Worker 只挪走落在它端口上的少数账户；
// 预热结束时空闲账户均匀落在空闲端口上；端口用满时一次归还紧接着一次申请不挪动空闲账户
func TestProxyPoolDefaultSpread(t *testing.T) {
	const accounts, workers, ports = 3000, 150, 200
	clock := newProxyPoolTestClock()
	pool, _ := newTestProxyPool(t, clock, "", "")
	ids := proxyPoolTestIDs(accounts)
	owner := newFakeProxyOwner(ids...)
	pool.configure(staticProxyPool(10001, 10000+ports), "", workers)
	pool.attach(owner)
	updated := func() int {
		owner.mu.Lock()
		defer owner.mu.Unlock()
		return owner.updated
	}
	startedAt := time.Now()
	start, moved := updated(), 0
	for index := 0; index < workers; index++ {
		before := updated()
		lease := pool.acquire(ids[index], ids[index], proxyPoolKindWorker, ids[index], "")
		lease.attach(&fakeProxyCloser{})
		// 申请者自己的绑定换成活动绑定算 1 次，其余为被挪走的空闲账户
		moved = max(moved, updated()-before-1)
	}
	total := updated() - start
	perPort := make(map[string]int)
	for _, id := range ids[workers:] {
		binding := owner.binding(id)
		if binding == nil || binding.Active {
			t.Fatalf("%s 应有默认出口：%+v", id, binding)
		}
		perPort[binding.Slot]++
	}
	busiest := 0
	for _, count := range perPort {
		busiest = max(busiest, count)
	}
	average := (accounts - workers) / (ports - workers)
	t.Logf("预热 %d 个 Worker：重写绑定 %d 次，单次最多挪走 %d 个，最多的端口 %d 个（平均 %d），耗时 %s",
		workers, total, moved, busiest, average, time.Since(startedAt))
	if total > 3*accounts || moved > 3*average || busiest > 3*average || len(perPort) != ports-workers {
		t.Fatalf("默认出口应分散：重写 %d 次、单次最多 %d、最多的端口 %d、用到 %d 个端口", total, moved, busiest, len(perPort))
	}
	checkProxyPoolInvariants(t, pool, owner)

	// 端口用满（PROXY 不为空，空闲账户退回 PROXY）：归还后马上被另一个 Worker 占用，空闲账户一个都不挪
	full, _ := newTestProxyPool(t, clock, "", "")
	fullOwner := newFakeProxyOwner(ids...)
	full.configure(staticProxyPool(10001, 10000+workers), proxyPoolTestGlobal, workers)
	full.attach(fullOwner)
	held := make([]*proxyPoolLease, workers)
	for index := range held {
		held[index] = full.acquire(ids[index], ids[index], proxyPoolKindWorker, ids[index], "")
		held[index].attach(&fakeProxyCloser{})
	}
	if binding := fullOwner.binding(ids[workers]); binding != nil {
		t.Fatalf("端口用满时空闲账户应退回 PROXY：%+v", binding)
	}
	fullOwner.mu.Lock()
	before := fullOwner.updated
	fullOwner.mu.Unlock()
	held[0].release()
	next := full.acquire(ids[workers], ids[workers], proxyPoolKindWorker, ids[workers], "")
	clock.Advance(proxyPoolWakeDelay)
	full.status()
	fullOwner.mu.Lock()
	rebound := fullOwner.updated - before
	fullOwner.mu.Unlock()
	// 只有归还者自己（先回到归自己的端口，端口被占后退回 PROXY，2 次）与申请者（换成活动绑定，1 次）
	if next.Slot() != held[0].Slot() || rebound != 3 {
		t.Fatalf("归还后马上被占用时不应挪动空闲账户：重写 %d 次", rebound)
	}
	checkProxyPoolInvariants(t, full, fullOwner)
}

// TestProxyPoolWorkerAvoidsSharedSameAccount 复查 2-2：同一账户的登录租约是端口不足时与别人共用的，Worker 不加入它：有空闲端口时另分，
// 确实没有可分配端口时才回到这个共用端口（仍是这个账户的 IP）
func TestProxyPoolWorkerAvoidsSharedSameAccount(t *testing.T) {
	clock := newProxyPoolTestClock()
	pool, _ := newTestProxyPool(t, clock, "", "")
	const id = "a@example.com"
	owner := newFakeProxyOwner("b@example.com", "c@example.com")
	pool.configure(staticProxyPool(10001, 10002), "", 2)
	pool.attach(owner)
	b := pool.acquire("b@example.com", "b@example.com", proxyPoolKindWorker, "b", "")
	c := pool.acquire("c@example.com", "c@example.com", proxyPoolKindWorker, "c", "")
	login := pool.acquire("login:a", "", proxyPoolKindLogin, "auth", "")
	if !login.shared {
		t.Fatal("端口用满时新账户登录应与别人共用")
	}
	login.adopt(id)
	owner.add(id, "")
	pool.refresh(owner, id)

	fallback := pool.acquire(id, id, proxyPoolKindWorker, "a", "")
	if fallback.Slot() != login.Slot() || !fallback.shared {
		t.Fatalf("没有可分配端口时 Worker 应回到同一账户正在共用的端口 %s：%s shared=%v", login.Slot(), fallback.Slot(), fallback.shared)
	}
	fallback.release()

	freed := c
	if login.Slot() == c.Slot() {
		freed = b
	}
	freed.release()
	worker := pool.acquire(id, id, proxyPoolKindWorker, "a", "")
	if worker.Slot() != freed.Slot() || worker.shared {
		t.Fatalf("有空闲端口时 Worker 不应加入共用的登录租约：%s shared=%v，期望 %s", worker.Slot(), worker.shared, freed.Slot())
	}
	worker.attach(&fakeProxyCloser{})
	checkProxyPoolInvariants(t, pool, owner)
	login.release()
	pool.mu.Lock()
	affinity := pool.affinity[id].port
	pool.mu.Unlock()
	if binding := owner.binding(id); binding == nil || binding.Slot != worker.Slot() || strconv.Itoa(affinity) != worker.Slot() {
		t.Fatalf("登录归还后账户应只在 Worker 的端口上：%+v 亲和 %d", binding, affinity)
	}
	checkProxyPoolInvariants(t, pool, owner)
	worker.release()
	for _, lease := range []*proxyPoolLease{b, c} {
		lease.release()
	}
}

// TestProxyPoolLoginFollowsServingExit 复查 2-3：登录、验证跟随账户正在用的出口：动态 IP 换会话之后、Worker 重启之前仍用 Worker 的会话号；
// 正在服务的 Worker 走直通出口（IP 池开启前启动）时用同一个上游、不另分端口或会话；只有 Worker 按新会话或新端口走
func TestProxyPoolLoginFollowsServingExit(t *testing.T) {
	const id = "a@example.com"
	t.Run("dynamic", func(t *testing.T) {
		pool, registry := newTestProxyPool(t, newProxyPoolTestClock(), "", "")
		owner := newFakeProxyOwner(id)
		pool.configure(dynamicProxyPool(), "", 2)
		pool.attach(owner)
		worker := pool.acquire(id, id, proxyPoolKindWorker, "a", "")
		worker.attach(&fakeProxyCloser{})
		serving := worker.Slot()
		if !rotateSessionForTest(pool, id, "测试") {
			t.Fatal("应换会话")
		}
		verify := pool.acquire(id, id, proxyPoolKindVerify, "a", "")
		if verify.Slot() != serving || verify.URL() != worker.URL() || !verify.Pooled() ||
			countProxyPoolLogs(registry, "IP 池分配 | 会话="+serving+" | 来源=同账号 | 用途=验证") != 1 {
			t.Fatalf("验证应沿用正在服务的 Worker 的会话 %s，得到 %s：%v", serving, verify.Slot(), proxyPoolLogLines(registry))
		}
		if binding := owner.binding(id); binding == nil || binding.Slot != serving {
			t.Fatalf("验证期间绑定仍是 Worker 的会话：%+v", binding)
		}
		next := pool.acquire(id, id, proxyPoolKindWorker, "a", "")
		if next.Slot() == serving || next.Slot() != serving+"r1" {
			t.Fatalf("Worker 应按新会话走：%s", next.Slot())
		}
		checkProxyPoolInvariants(t, pool, owner)
		for _, lease := range []*proxyPoolLease{verify, next, worker} {
			lease.release()
		}
		if login := pool.acquire(id, id, proxyPoolKindLogin, "a", ""); login.Slot() != serving+"r1" {
			t.Fatalf("没有浏览器时登录用账户当前的会话：%s", login.Slot())
		} else {
			login.release()
		}
	})
	for _, cfg := range []config.ProxyPool{staticProxyPool(10001, 10003), dynamicProxyPool()} {
		t.Run("passthrough-"+cfg.Mode, func(t *testing.T) {
			pool, registry := newTestProxyPool(t, newProxyPoolTestClock(), "", "")
			owner := newFakeProxyOwner(id)
			pool.configure(config.ProxyPool{Mode: config.ProxyPoolOff}, proxyPoolTestGlobal, 2)
			pool.attach(owner)
			worker := pool.acquire(id, id, proxyPoolKindWorker, "a", proxyPoolTestGlobal)
			worker.attach(&fakeProxyCloser{})
			pool.configure(cfg, "http://203.0.113.10:3128", 2)
			verify := pool.acquire(id, id, proxyPoolKindVerify, "a", "http://203.0.113.10:3128")
			if verify.Pooled() || verify.URL() != proxyPoolTestGlobal ||
				countProxyPoolLogs(registry, "IP 池不分配，与仍走默认代理的 Worker 用同一出口 | 用途=验证") != 1 {
				t.Fatalf("验证应与仍走默认代理的 Worker 同一上游：pooled=%v url=%q", verify.Pooled(), verify.URL())
			}
			if status := pool.status(); status.InUse != 0 || status.Leases != 0 {
				t.Fatalf("跟随直通出口不应占用端口或会话：%+v", status)
			}
			if binding := owner.binding(id); binding == nil || binding.URL != proxyPoolTestGlobal {
				t.Fatalf("绑定仍是 Worker 的直通出口：%+v", binding)
			}
			if next := pool.acquire(id, id, proxyPoolKindWorker, "a", ""); !next.Pooled() {
				t.Fatal("Worker 应按 IP 池分配")
			} else {
				next.release()
			}
			checkProxyPoolInvariants(t, pool, owner)
			verify.release()
			worker.release()
		})
	}
}

// TestProxyPoolImportClaims 复查 2-2：导入租约归还时留下出口记录，账户新建后按它登记到名下；归还后端口已被别人占用、换了代理商时不登记；
// 动态记下临时会话号；共用的端口不留记录
func TestProxyPoolImportClaims(t *testing.T) {
	clock := newProxyPoolTestClock()
	pool, _ := newTestProxyPool(t, clock, "", "")
	owner := newFakeProxyOwner("other@example.com")
	pool.configure(staticProxyPool(10001, 10002), "", 2)
	pool.attach(owner)
	lease := pool.acquire("login:x", "", proxyPoolKindImport, "auth", "")
	port := leasePort(t, lease)
	claim := lease.releaseClaim()
	if claim == nil || claim.port != port || pool.status().InUse != 0 {
		t.Fatalf("导入结束应归还端口并留下记录：%+v", claim)
	}
	if again := lease.releaseClaim(); again != nil {
		t.Fatal("重复归还不应再留记录")
	}
	claim.adopt("x@example.com")
	owner.add("x@example.com", "")
	pool.refresh(owner, "x@example.com")
	if binding := owner.binding("x@example.com"); binding == nil || binding.Slot != strconv.Itoa(port) || binding.Active {
		t.Fatalf("登记后默认出口应回到导入用的端口：%+v", binding)
	}
	if worker := pool.acquire("x@example.com", "x@example.com", proxyPoolKindWorker, "x", ""); leasePort(t, worker) != port {
		t.Fatalf("之后的 Worker 应回到导入用的端口 %d：%s", port, worker.Slot())
	} else {
		worker.release()
	}

	// 归还后端口已被别的账号占用：不抢
	lease = pool.acquire("login:y", "", proxyPoolKindImport, "auth", "")
	claim = lease.releaseClaim()
	taken := pool.acquire("other@example.com", "other@example.com", proxyPoolKindWorker, "other", "")
	if leasePort(t, taken) != claim.port {
		t.Skipf("另一个账号没有分到同一端口（%s / %d）", taken.Slot(), claim.port)
	}
	claim.adopt("y@example.com")
	pool.mu.Lock()
	_, adopted := pool.affinity["y@example.com"]
	pool.mu.Unlock()
	if adopted {
		t.Fatal("端口已被别人占用时不应登记")
	}
	taken.release()

	// 换了代理商：同一端口号已经不是同一个 IP
	lease = pool.acquire("login:z", "", proxyPoolKindImport, "auth", "")
	claim = lease.releaseClaim()
	pool.configure(config.ProxyPool{Mode: config.ProxyPoolStatic, URL: "http://other.example.com:10000", PortStart: 10001, PortEnd: 10002}, "", 2)
	claim.adopt("z@example.com")
	pool.mu.Lock()
	_, adopted = pool.affinity["z@example.com"]
	pool.mu.Unlock()
	if adopted {
		t.Fatal("换了代理商后不应登记")
	}

	// 共用的端口不留记录
	a := pool.acquire("a", "a", proxyPoolKindWorker, "a", "")
	b := pool.acquire("b", "b", proxyPoolKindWorker, "b", "")
	if shared := pool.acquire("login:s", "", proxyPoolKindImport, "auth", ""); !shared.shared || shared.releaseClaim() != nil {
		t.Fatal("与别人共用端口的导入不应留记录")
	}
	a.release()
	b.release()

	// 动态：记下临时会话号
	pool.configure(dynamicProxyPool(), "", 2)
	lease = pool.acquire("login:d", "", proxyPoolKindImport, "auth", "")
	claim = lease.releaseClaim()
	if claim == nil || claim.session != lease.Slot() {
		t.Fatalf("动态导入应记下临时会话号：%+v", claim)
	}
	claim.adopt("d@example.com")
	owner.add("d@example.com", "")
	pool.refresh(owner, "d@example.com")
	if binding := owner.binding("d@example.com"); binding == nil || binding.Slot != lease.Slot() {
		t.Fatalf("登记后会话号应为导入用的会话号：%+v", binding)
	}
	var nilClaim *proxyPoolClaim
	nilClaim.adopt("n@example.com")
	checkProxyPoolInvariants(t, pool, owner)
}

// TestProxyPoolPinnedAndPreview 复查 2-2、2-3：markPinned 按当前生效的 PROXY 标出固定了默认代理的账户（IP 池关闭时不标）；
// accountPreview 给出账户清空自己的代理后的默认出口（不改账户记录）
func TestProxyPoolPinnedAndPreview(t *testing.T) {
	pool, _ := newTestProxyPool(t, newProxyPoolTestClock(), "", "")
	owner := newFakeProxyOwner()
	owner.add("pinned@example.com", proxyPoolTestGlobal)
	owner.add("own@example.com", "http://203.0.113.50:3128")
	owner.add("free@example.com", "")
	pool.configure(staticProxyPool(10001, 10003), proxyPoolTestGlobal, 2)
	pool.attach(owner)
	accounts := []api.AdminAccount{{ID: "pinned@example.com"}, {ID: "own@example.com"}, {ID: "free@example.com"}}
	pool.markPinned(accounts)
	if !accounts[0].ProxyPoolPinned || accounts[1].ProxyPoolPinned || accounts[2].ProxyPoolPinned {
		t.Fatalf("固定标记 = %+v", accounts)
	}

	preview, ok := pool.accountPreview("own@example.com")
	pool.mu.Lock()
	state := pool.accounts["own@example.com"]
	recorded := state.defaultPort
	pool.mu.Unlock()
	if !ok || preview.url != staticPortURL(t, mustAtoi(t, preview.slot)) || recorded != 0 {
		t.Fatalf("清空后的默认出口 = %+v（%v），账户记录的默认端口 %d", preview, ok, recorded)
	}
	held := []*proxyPoolLease{
		pool.acquire("x", "x", proxyPoolKindWorker, "x", ""),
		pool.acquire("y", "y", proxyPoolKindWorker, "y", ""),
		pool.acquire("z", "z", proxyPoolKindWorker, "z", ""),
	}
	if _, ok := pool.accountPreview("own@example.com"); ok {
		t.Fatal("端口用满、PROXY 不为空时应回到 PROXY")
	}
	for _, lease := range held {
		lease.release()
	}
	pool.configure(dynamicProxyPool(), proxyPoolTestGlobal, 2)
	if preview, ok := pool.accountPreview("own@example.com"); !ok || preview.slot != proxyPoolHashSession("own@example.com") {
		t.Fatalf("动态清空后的出口 = %+v", preview)
	}

	pool.configure(config.ProxyPool{Mode: config.ProxyPoolOff}, proxyPoolTestGlobal, 2)
	accounts[0].ProxyPoolPinned = false
	pool.markPinned(accounts)
	if accounts[0].ProxyPoolPinned {
		t.Fatal("IP 池关闭时不应标出")
	}
	if _, ok := pool.accountPreview("own@example.com"); ok {
		t.Fatal("IP 池关闭时没有池出口")
	}
	var nilPool *proxyPool
	nilPool.markPinned(accounts)
	if _, ok := nilPool.accountPreview("own@example.com"); ok {
		t.Fatal("nil IP 池没有出口")
	}
}

func mustAtoi(t *testing.T, value string) int {
	t.Helper()
	number, err := strconv.Atoi(value)
	if err != nil {
		t.Fatal(err)
	}
	return number
}

// TestProxyPoolProbeSequence 默认出口的探测序列不重复地走遍全部端口，不同账户的步长分散
func TestProxyPoolProbeSequence(t *testing.T) {
	for _, size := range []int{1, 2, 7, 200, 5000} {
		steps := proxyPoolSteps(size)
		stepsSeen := make(map[int]struct{})
		for _, id := range proxyPoolTestIDs(50) {
			offset, step := proxyPoolProbe(id, size, steps)
			stepsSeen[step] = struct{}{}
			visited := make(map[int]struct{}, size)
			index := offset
			for range size {
				visited[index] = struct{}{}
				if index += step; index >= size {
					index -= size
				}
			}
			if len(visited) != size {
				t.Fatalf("端口数 %d、%s 的探测序列只走到 %d 个端口", size, id, len(visited))
			}
		}
		if size >= 200 && len(stepsSeen) < 20 {
			t.Fatalf("端口数 %d 时 50 个账户只用了 %d 种步长", size, len(stepsSeen))
		}
	}
}

// TestProxyPoolRuleAAccountEvidence 规则 A 只计入被拒像是 IP 问题的账号（合并复查第 2 轮 2-1）：坏号记忆的依据是裸 429 多半跟着账号走，
// 额度用完的账号在端口换手后接连被拒不能算成 IP 的问题。计入的要求：自最近一次成功以来只在这个端口上被拒、第一次被拒前还不是坏号；
// 冷却还要求其中至少一个近期在别的出口上成功过。撤回后在新端口上又被拒的账号不计入新端口，误判不会沿着换端口传下去
func TestProxyPoolRuleAAccountEvidence(t *testing.T) {
	setup := func(t *testing.T, end int) (*proxyPool, *proxyPoolTestClock, *[]proxyPoolCooling) {
		clock := newProxyPoolTestClock()
		pool, _ := newTestProxyPool(t, clock, "", "")
		pool.configure(staticProxyPool(10001, end), "", 4)
		pool.attach(newFakeProxyOwner("a@example.com", "b@example.com", "c@example.com", "d@example.com"))
		cooled := &[]proxyPoolCooling{}
		pool.setPortCooledHook(func(cooling proxyPoolCooling) { *cooled = append(*cooled, cooling) })
		return pool, clock, cooled
	}
	worker := func(pool *proxyPool, id string) *proxyPoolLease {
		lease := pool.acquire(id, id, proxyPoolKindWorker, id, "")
		lease.attach(&fakeProxyCloser{})
		return lease
	}
	failures := func(pool *proxyPool, port int) []string {
		pool.mu.Lock()
		defer pool.mu.Unlock()
		var ids []string
		if state := pool.ports[port]; state != nil {
			for _, failure := range state.failures {
				ids = append(ids, failure.account)
			}
		}
		return sortedStrings(ids)
	}
	// 复查的场景：a 在 X 上连续两次被拒标坏号，X 换手给 b、b 首发被拒；两人都没有在别处成功过的记录，分不清是 IP 还是账号，不冷却
	t.Run("no-proven", func(t *testing.T) {
		pool, _, cooled := setup(t, 10001)
		a := worker(pool, "a@example.com")
		pool.rateLimited("a@example.com", false)
		pool.rateLimited("a@example.com", false)
		a.release()
		b := worker(pool, "b@example.com")
		defer b.release()
		pool.rateLimited("b@example.com", false)
		if len(*cooled) != 0 || pool.status().Cooling != 0 {
			t.Fatalf("没有近期在别处成功过的账号时不应冷却：%+v", *cooled)
		}
		if got := failures(pool, 10001); !slices.Equal(got, []string{"a@example.com", "b@example.com"}) {
			t.Fatalf("两人都只在这个端口上被拒，仍记着：%v", got)
		}
	})
	// 第一次被拒之前已是坏号（证据不来自这个 IP，例如重启前标上的）：不计入，直到成功一次
	t.Run("bad-before", func(t *testing.T) {
		pool, _, cooled := setup(t, 10001)
		a := worker(pool, "a@example.com")
		pool.rateLimited("a@example.com", true)
		pool.rateLimited("a@example.com", true)
		a.release()
		b := worker(pool, "b@example.com")
		markProxyPoolProven(pool, "b@example.com")
		pool.rateLimited("b@example.com", false)
		if len(*cooled) != 0 {
			t.Fatalf("早已是坏号的 a 不应计入：%+v", *cooled)
		}
		b.release()
		c := worker(pool, "c@example.com")
		defer c.release()
		pool.rateLimited("c@example.com", false)
		if len(*cooled) != 1 || !slices.Equal(sortedStrings((*cooled)[0].RateLimitedAccounts), []string{"b@example.com", "c@example.com"}) {
			t.Fatalf("b 与 c 计入后应冷却、撤回名单不含 a：%+v", *cooled)
		}
		// 成功一次后 a 重新开始一串：之后在这个端口上被拒照常计入
		pool.succeeded("a@example.com")
		pool.mu.Lock()
		_, kept := pool.strikes["a@example.com"]
		pool.mu.Unlock()
		if kept {
			t.Fatal("生成成功应清除账户的裸限流记录")
		}
	})
	// 在别的出口上也被拒（换了 IP 照样被拒）：从第一个端口的记录里删掉，也不计入之后的端口
	t.Run("refused-elsewhere", func(t *testing.T) {
		pool, _, cooled := setup(t, 10001)
		a := worker(pool, "a@example.com")
		pool.rateLimited("a@example.com", false)
		a.release()
		if got := failures(pool, 10001); !slices.Equal(got, []string{"a@example.com"}) {
			t.Fatalf("a 第一次被拒应计入：%v", got)
		}
		// a 没有池租约时（走 PROXY 或直通出口）又被拒
		pool.rateLimited("a@example.com", false)
		if got := failures(pool, 10001); len(got) != 0 {
			t.Fatalf("a 在别的出口上也被拒后应从 10001 的记录里删掉：%v", got)
		}
		b := worker(pool, "b@example.com")
		defer b.release()
		markProxyPoolProven(pool, "b@example.com")
		pool.rateLimited("b@example.com", false)
		if len(*cooled) != 0 {
			t.Fatalf("只剩 b 一个计入的账号，不应冷却：%+v", *cooled)
		}
		// a 回到这个端口再被拒也不计入（这一串在别处也被拒过）
		a = worker(pool, "a@example.com")
		defer a.release()
		pool.rateLimited("a@example.com", false)
		if len(*cooled) != 0 || !slices.Equal(failures(pool, 10001), []string{"b@example.com"}) {
			t.Fatalf("在别处也被拒的 a 不应计入：%+v %v", *cooled, failures(pool, 10001))
		}
	})
	// 一次冷却之后，计入的账号换到新端口又被拒：不计入新端口（撤回只撤一次，误判不会沿着换端口传下去）
	t.Run("no-chain", func(t *testing.T) {
		pool, clock, cooled := setup(t, 10002)
		a := worker(pool, "a@example.com")
		if a.Slot() != "10001" {
			t.Fatalf("a 的端口 = %s", a.Slot())
		}
		pool.rateLimited("a@example.com", false)
		a.release()
		clock.Advance(time.Minute)
		// 10002 从没分过、比刚归还的 10001 优先：先把 10002 占住，b 才拿到 10001
		hold := worker(pool, "d@example.com")
		b := worker(pool, "b@example.com")
		if b.Slot() != "10001" {
			t.Fatalf("b 的端口 = %s", b.Slot())
		}
		markProxyPoolProven(pool, "b@example.com")
		pool.rateLimited("b@example.com", false)
		if len(*cooled) != 1 || (*cooled)[0].Port != 10001 {
			t.Fatalf("a、b 在 10001 上被拒、b 近期在别处成功过，应冷却 10001：%+v", *cooled)
		}
		b.release()
		hold.release()
		// 撤回之后 a、b 换到 10002 又被拒（这次换 c 带着近期成功）：a、b 这一串已在 10001 上被拒过，只有 c 计入，不冷却 10002
		next := worker(pool, "a@example.com")
		if next.Slot() != "10002" {
			t.Fatalf("10001 冷却中，a 应换到 10002：%s", next.Slot())
		}
		pool.rateLimited("a@example.com", false)
		next.release()
		next = worker(pool, "b@example.com")
		pool.rateLimited("b@example.com", false)
		next.release()
		next = worker(pool, "c@example.com")
		defer next.release()
		markProxyPoolProven(pool, "c@example.com")
		pool.rateLimited("c@example.com", false)
		if len(*cooled) != 1 || !slices.Equal(failures(pool, 10002), []string{"c@example.com"}) {
			t.Fatalf("换端口后又被拒的 a、b 不应计入 10002：%+v %v", *cooled, failures(pool, 10002))
		}
	})
	// 近期成功要在别的出口上：只在这个端口上成功过的账号到这里被拒不算 IP 的证据
	t.Run("proven-elsewhere-only", func(t *testing.T) {
		pool, _, cooled := setup(t, 10001)
		a := worker(pool, "a@example.com")
		pool.rateLimited("a@example.com", false)
		a.release()
		b := worker(pool, "b@example.com")
		defer b.release()
		pool.succeeded("b@example.com")
		pool.rateLimited("b@example.com", false)
		if len(*cooled) != 0 {
			t.Fatalf("b 只在这个端口上成功过，不应冷却：%+v", *cooled)
		}
	})
}
