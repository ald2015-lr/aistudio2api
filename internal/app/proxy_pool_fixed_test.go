package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/api"
	"github.com/Mag1cFall/AIStudio2API/internal/config"
)

// IP 池固定分配（PROXY_POOL_FIXED）的单元测试：分配表、暂停与恢复、新账户与导入、删除、端口段变化、手动操作、持久化与动态固定会话

// fixedStaticPool 返回开启固定分配的静态设置
func fixedStaticPool(start, end int) config.ProxyPool {
	pool := staticProxyPool(start, end)
	pool.Fixed = true
	return pool
}

// fixedTestEnv 为带记录文件、假端口锁与假时钟的 IP 池（与生产相同的装配顺序：configure → prepareFixed → attach）
type fixedTestEnv struct {
	t        *testing.T
	clock    *proxyPoolTestClock
	pool     *proxyPool
	registry *requestRegistry
	owner    *fakeProxyOwner
	locks    *proxyPoolGoldenLocks
	dir      string
	global   string
	logged   int
}

// newFixedPool 建一个 IP 池（还没有 configure）：dir 为空时用新的临时目录
func newFixedPool(t *testing.T, dir string, clock *proxyPoolTestClock, locks *proxyPoolGoldenLocks) (*proxyPool, *requestRegistry, string) {
	t.Helper()
	if dir == "" {
		dir = t.TempDir()
	}
	if locks == nil {
		locks = &proxyPoolGoldenLocks{held: make(map[int]bool), other: make(map[int]bool)}
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	registry := newRequestRegistry(ctx)
	pool := newProxyPool(proxyPoolOptions{
		path: filepath.Join(dir, proxyPoolFileName), lockDir: "locks", requests: registry, now: clock.Now,
		random: &proxyPoolGoldenReader{}, tryLock: locks.tryLock, relayLimit: func() int { return 1 << 20 },
	})
	// 在临时目录清理之前停掉定时器并写盘（Cleanup 后进先出）
	t.Cleanup(func() {
		pool.mu.Lock()
		if pool.recheckTimer != nil {
			pool.recheckTimer.Stop()
		}
		pool.mu.Unlock()
		pool.flush()
	})
	return pool, registry, dir
}

// newFixedTestEnv 建环境并按生产顺序装配：configure（容量 8）→ prepareFixed（own 里的账户有自己的代理）→ attach
func newFixedTestEnv(t *testing.T, dir string, cfg config.ProxyPool, own map[string]string, ids ...string) *fixedTestEnv {
	t.Helper()
	clock := newProxyPoolTestClock()
	locks := &proxyPoolGoldenLocks{held: make(map[int]bool), other: make(map[int]bool)}
	pool, registry, dir := newFixedPool(t, dir, clock, locks)
	env := &fixedTestEnv{t: t, clock: clock, pool: pool, registry: registry, locks: locks, dir: dir, global: proxyPoolTestGlobal}
	env.owner = newFakeProxyOwner()
	accounts := make([]*aistudio.Account, 0, len(ids))
	for _, id := range ids {
		env.owner.add(id, own[id])
		accounts = append(accounts, &aistudio.Account{ID: id, Config: aistudio.AccountConfig{Enabled: true, Proxy: own[id]}})
	}
	pool.configure(cfg, env.global, 8)
	pool.prepareFixed(accounts)
	for _, account := range accounts {
		pool.prime(account)
	}
	pool.attach(env.owner)
	return env
}

// table 返回固定表（账户 → 端口）
func (env *fixedTestEnv) table() map[string]int {
	env.pool.mu.Lock()
	defer env.pool.mu.Unlock()
	table := make(map[string]int, len(env.pool.fixed.entries))
	for accountID, entry := range env.pool.fixed.entries {
		table[accountID] = entry.port
	}
	return table
}

// port 返回账户的固定端口（没有时为 0）
func (env *fixedTestEnv) port(accountID string) int {
	return env.table()[accountID]
}

// usersOf 返回端口上的固定账户
func (env *fixedTestEnv) usersOf(port int) []string {
	env.pool.mu.Lock()
	defer env.pool.mu.Unlock()
	return env.pool.fixedUsersLocked(port)
}

// worker 申请并启动 Worker（attach 到假浏览器），返回租约与浏览器
func (env *fixedTestEnv) worker(accountID string) (*proxyPoolLease, *fakeProxyCloser) {
	env.t.Helper()
	lease := env.pool.acquire(accountID, accountID, proxyPoolKindWorker, accountID, env.global)
	closer := &fakeProxyCloser{}
	if lease.Refused() == nil {
		lease.attach(closer)
	}
	return lease, closer
}

// coolUpstream 用规则 B 冷却账户的固定端口（同端口连续两次上游失败的启动）
func (env *fixedTestEnv) coolUpstream(accountID string) {
	env.t.Helper()
	for range proxyPoolUpstreamFails {
		lease := env.pool.acquire(accountID, accountID, proxyPoolKindWorker, accountID, env.global)
		if lease.Refused() != nil {
			env.t.Fatalf("冷却前申请被拒：%v", lease.Refused())
		}
		lease.launchFailed(upstreamFailure, false)
	}
}

// newLogs 返回上次调用以来新增的日志行
func (env *fixedTestEnv) newLogs() []string {
	lines := proxyPoolLogLines(env.registry)
	fresh := lines[env.logged:]
	env.logged = len(lines)
	return fresh
}

// logged 返回含 fragment 的日志行数
func (env *fixedTestEnv) count(fragment string) int {
	return countProxyPoolLogs(env.registry, fragment)
}

// fixedBindingText 返回绑定的脱敏描述（失败信息里不带账号密码）
func fixedBindingText(binding any) string {
	var value *aistudio.ProxyBinding
	switch typed := binding.(type) {
	case *aistudio.ProxyBinding:
		value = typed
	case aistudio.ProxyBinding:
		value = &typed
	}
	if value == nil {
		return "<nil>"
	}
	return fmt.Sprintf("url=%s slot=%s active=%v hold=%q", config.RedactProxy(value.URL), value.Slot, value.Active, value.Hold)
}

// fixedCountsOf 返回每个端口的账户数
func fixedCountsOf(table map[string]int) map[int]int {
	counts := make(map[int]int)
	for _, port := range table {
		counts[port]++
	}
	return counts
}

// assertFixedTableBalanced 断言固定表在 ports 上均衡
func assertFixedTableBalanced(t *testing.T, table map[string]int, ports []int) {
	t.Helper()
	assertFixedBalanced(t, "固定表", table, ports)
}

// fixedInvariantError 检查固定分配的不变量：反查表一致、预留计数一致、待写日志已取走、持有计数与租约一致、
// 参与者的绑定是段内端口（直通租约除外）且暂停原因与固定端口一致
func fixedInvariantError(pool *proxyPool, owner *fakeProxyOwner) error {
	pool.mu.Lock()
	defer pool.mu.Unlock()
	fixed := &pool.fixed
	if len(fixed.logs) != 0 {
		return fmt.Errorf("公共方法返回后待写日志没有取走：%d 行", len(fixed.logs))
	}
	indexed := 0
	for port, users := range fixed.byPort {
		if len(users) == 0 {
			return fmt.Errorf("端口 %d 的反查表为空集合", port)
		}
		for accountID := range users {
			if entry, ok := fixed.entries[accountID]; !ok || entry.port != port {
				return fmt.Errorf("反查表 %d → %s 与固定表不一致", port, accountID)
			}
			indexed++
		}
	}
	if indexed != len(fixed.entries) {
		return fmt.Errorf("反查表 %d 个，固定表 %d 个", indexed, len(fixed.entries))
	}
	reserved := make(map[int]int)
	for _, reservation := range fixed.reserved {
		reserved[reservation.port]++
	}
	for port, count := range fixed.reservedBy {
		if count <= 0 || reserved[port] != count {
			return fmt.Errorf("端口 %d 的预留计数 %d，令牌 %d", port, count, reserved[port])
		}
	}
	for port, count := range reserved {
		if fixed.reservedBy[port] != count {
			return fmt.Errorf("端口 %d 的预留令牌 %d，计数 %d", port, count, fixed.reservedBy[port])
		}
	}
	holders := make(map[int]map[string]int)
	pooled := 0
	for key, leases := range pool.byKey {
		for _, lease := range leases {
			if lease.released || lease.refused != nil {
				return fmt.Errorf("租约 %s 已归还或被拒却仍登记", key)
			}
			if lease.pooled {
				pooled++
				if lease.port != 0 {
					if holders[lease.port] == nil {
						holders[lease.port] = make(map[string]int)
					}
					holders[lease.port][lease.key]++
				}
			}
		}
	}
	if pooled != pool.pooledLeases {
		return fmt.Errorf("池租约 %d 个，计数 %d", pooled, pool.pooledLeases)
	}
	for port, state := range pool.ports {
		for key, count := range state.holders {
			if holders[port][key] != count {
				return fmt.Errorf("端口 %d 键 %s 持有 %d，租约 %d", port, key, count, holders[port][key])
			}
		}
		for key, count := range holders[port] {
			if state.holders[key] != count {
				return fmt.Errorf("端口 %d 键 %s 租约 %d，持有 %d", port, key, count, state.holders[key])
			}
		}
	}
	if owner == nil || !pool.fixedStaticLocked() {
		return nil
	}
	now := pool.now()
	for _, accountID := range owner.AccountIDs() {
		state := pool.accounts[accountID]
		if state == nil || !state.known || state.proxy != "" {
			continue
		}
		entry, ok := fixed.entries[accountID]
		if !ok || !pool.inRangeLocked(entry.port) {
			return fmt.Errorf("参与者 %s 没有段内的固定端口", accountID)
		}
		binding := owner.binding(accountID)
		if binding == nil || binding.URL == "" {
			return fmt.Errorf("参与者 %s 的绑定为空（会回落到默认代理或直连）", accountID)
		}
		lease := pool.bindingLeaseLocked(accountID)
		if lease == nil && binding.URL != pool.staticURLLocked(entry.port) {
			return fmt.Errorf("没有租约的 %s 的绑定不是固定端口 %d", accountID, entry.port)
		}
		if binding.URL == pool.settings.global && (lease == nil || lease.pooled) {
			return fmt.Errorf("参与者 %s 的绑定是默认代理", accountID)
		}
		if want := pool.fixedHoldTextLocked(entry.port, now); binding.Hold != want {
			return fmt.Errorf("%s 的暂停原因 %q，固定端口 %d 此刻应为 %q", accountID, binding.Hold, entry.port, want)
		}
	}
	return nil
}

func checkFixedInvariants(t *testing.T, env *fixedTestEnv) {
	t.Helper()
	if err := fixedInvariantError(env.pool, env.owner); err != nil {
		t.Fatal(err)
	}
}

// TestProxyPoolFixedFirstEnableHot 非固定下跑出亲和与独占后热开启：首次迁移保留没超过份额的亲和（运行中独占 Worker 的归属者留在原端口），
// 其余按最少；没有活动租约的账户立即切到固定端口，运行中的 Worker 仍随它的租约；F1 计数正确；没有惰性分配；立即写盘
func TestProxyPoolFixedFirstEnableHot(t *testing.T) {
	ids := proxyPoolTestIDs(8)
	env := newFixedTestEnv(t, "", staticProxyPool(10001, 10004), nil, ids...)
	a, _ := env.worker(ids[0])
	b, _ := env.worker(ids[1])
	portA, portB := leasePort(t, a), leasePort(t, b)
	b.release()
	env.clock.Advance(time.Minute)
	env.newLogs()

	env.pool.configure(fixedStaticPool(10001, 10004), env.global, 8)
	table := env.table()
	assertFixedTableBalanced(t, table, fixedTestPorts(10001, 4))
	if table[ids[0]] != portA || table[ids[1]] != portB {
		t.Fatalf("有亲和的账户应留在原端口：a %d→%d b %d→%d", portA, table[ids[0]], portB, table[ids[1]])
	}
	if env.count("INFO service IP 池固定分配已开启 | 端口=10001-10004（参与 4 个） | 账户=8 | 每端口=2-2 | 保留原端口=2 | 新分配=6 | 运行中不在固定端口的 Worker=0") != 1 {
		t.Fatalf("F1 汇总行不对：%v", env.newLogs())
	}
	if env.count("IP 池已更新 | 模式=静态 | 出口=http://isp.example.com | 端口=10001-10004（4 个） | 需要=8 | 固定分配=开启") != 1 {
		t.Fatalf("IP 池已更新应带固定分配后缀：%v", proxyPoolLogLines(env.registry))
	}
	for _, id := range ids[1:] {
		binding := env.owner.binding(id)
		if binding == nil || binding.URL != staticPortURL(t, table[id]) || binding.Active || binding.Hold != "" {
			t.Fatalf("没有租约的 %s 应立即切到固定端口 %d：%s", id, table[id], fixedBindingText(binding))
		}
	}
	if binding := env.owner.binding(ids[0]); binding == nil || !binding.Active || binding.Slot != strconv.Itoa(portA) {
		t.Fatalf("运行中的 Worker 仍随它的租约：%s", fixedBindingText(binding))
	}
	env.pool.mu.Lock()
	lazy := env.pool.fixed.lazyAssigned
	env.pool.mu.Unlock()
	if lazy != 0 {
		t.Fatalf("首次开启不应惰性分配：%d", lazy)
	}
	if _, err := os.Stat(filepath.Join(env.dir, proxyPoolFixedFileName)); err != nil {
		t.Fatalf("首次迁移应立即写盘：%v", err)
	}
	checkFixedInvariants(t, env)
	assertNoPoolSecretInLogs(t, env.registry)
}

// TestProxyPoolFixedStartupPrepare 带分配表文件新建池（重启、更新后）：configure → prepareFixed → prime（第一份绑定就是固定端口）→ attach，
// 与重启前逐个相同，写 F2，没有惰性分配
func TestProxyPoolFixedStartupPrepare(t *testing.T) {
	ids := proxyPoolTestIDs(11)
	env := newFixedTestEnv(t, "", fixedStaticPool(10001, 10004), nil, ids...)
	before := env.table()
	assertFixedTableBalanced(t, before, fixedTestPorts(10001, 4))
	env.pool.flush()

	clock := newProxyPoolTestClock()
	clock.Advance(48 * time.Hour)
	pool, registry, _ := newFixedPool(t, env.dir, clock, nil)
	pool.configure(fixedStaticPool(10001, 10004), proxyPoolTestGlobal, 8)
	accounts := make([]*aistudio.Account, 0, len(ids))
	for _, id := range ids {
		accounts = append(accounts, &aistudio.Account{ID: id, Config: aistudio.AccountConfig{Enabled: true}})
	}
	pool.prepareFixed(accounts)
	for _, account := range accounts {
		pool.prime(account)
		binding, ok := account.CurrentProxyBinding()
		if !ok || binding.URL != staticPortURL(t, before[account.ID]) {
			t.Fatalf("%s 的第一份绑定应是固定端口 %d：%s", account.ID, before[account.ID], fixedBindingText(binding))
		}
	}
	owner := newFakeProxyOwner(ids...)
	pool.attach(owner)
	pool.mu.Lock()
	after := make(map[string]int)
	for id, entry := range pool.fixed.entries {
		after[id] = entry.port
	}
	lazy := pool.fixed.lazyAssigned
	pool.mu.Unlock()
	for id, port := range before {
		if after[id] != port {
			t.Fatalf("重启后 %s 的端口 %d → %d", id, port, after[id])
		}
	}
	if lazy != 0 || countProxyPoolLogs(registry, "IP 池固定分配已开启（沿用分配表） | 端口=10001-10004（参与 4 个） | 账户=11 | 新分配=0 | 释放=0 | 段外重分=0 | 每端口=2-3") != 1 {
		t.Fatalf("重启应写 F2、不惰性分配（%d）：%v", lazy, proxyPoolLogLines(registry))
	}
	if err := fixedInvariantError(pool, owner); err != nil {
		t.Fatal(err)
	}
}

// TestProxyPoolFixedRestartUnchanged 申请、归还、冷却、其他进程锁住、热更新容量与 PROXY、切到动态再切回、开关关了又开、换代理商之后固定表都不变
func TestProxyPoolFixedRestartUnchanged(t *testing.T) {
	ids := proxyPoolTestIDs(9)
	env := newFixedTestEnv(t, "", fixedStaticPool(10001, 10003), nil, ids...)
	want := env.table()
	check := func(step string) {
		t.Helper()
		if got := env.table(); fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("%s 之后固定表变了：\n%v\n%v", step, want, got)
		}
		checkFixedInvariants(t, env)
	}
	a, closer := env.worker(ids[0])
	_ = a
	login := env.pool.acquire(ids[1], ids[1], proxyPoolKindLogin, ids[1], env.global)
	login.release()
	check("申请与归还")
	env.coolUpstream(ids[2])
	check("冷却")
	env.locks.setOther(env.port(ids[3]), true)
	if lease, _ := env.worker(ids[3]); lease.Refused() == nil {
		lease.release()
	}
	check("其他进程锁住")
	env.pool.setCapacity(40)
	env.pool.configure(fixedStaticPool(10001, 10003), "http://203.0.113.77:3128", 12)
	check("热更新容量与 PROXY")
	dynamic := dynamicProxyPool()
	dynamic.Fixed = true
	env.pool.configure(dynamic, env.global, 12)
	env.pool.configure(fixedStaticPool(10001, 10003), env.global, 12)
	check("切到动态再切回")
	env.pool.configure(staticProxyPool(10001, 10003), env.global, 12)
	env.pool.configure(fixedStaticPool(10001, 10003), env.global, 12)
	check("关了又开")
	other := fixedStaticPool(10001, 10003)
	other.URL = "isp.example.com:10000:user-other:" + proxyPoolTestPassword
	env.pool.configure(other, env.global, 12)
	check("换代理商")
	closer.close()
	env.clock.Advance(3 * time.Hour)
	env.pool.recheckFired()
	check("冷却与锁到期")
}

// TestProxyPoolFixedSharedPort 同端口 4 个账户同时开 Worker：都在固定端口上、不独占、没有端口不足 WARN；第一个加锁、最后一个放锁
func TestProxyPoolFixedSharedPort(t *testing.T) {
	ids := proxyPoolTestIDs(4)
	env := newFixedTestEnv(t, "", fixedStaticPool(10001, 10001), nil, ids...)
	var closers []*fakeProxyCloser
	for _, id := range ids {
		lease, closer := env.worker(id)
		if lease.Refused() != nil || leasePort(t, lease) != 10001 || lease.shared {
			t.Fatalf("%s 应共用固定端口 10001：refused=%v slot=%s shared=%v", id, lease.Refused(), lease.Slot(), lease.shared)
		}
		closers = append(closers, closer)
	}
	env.pool.mu.Lock()
	holders := len(env.pool.ports[10001].holders)
	locked := env.pool.ports[10001].lock != nil
	_, hasAffinity := env.pool.affinity[ids[0]]
	env.pool.mu.Unlock()
	if holders != 4 || !locked || hasAffinity {
		t.Fatalf("持有 %d、加锁 %v、写了亲和 %v", holders, locked, hasAffinity)
	}
	if env.count("端口不足") != 0 || env.count("IP 池分配 | 端口=10001 | 来源=固定 | 用途=Worker | 使用中=1/1") != 4 {
		t.Fatalf("固定分配不应写端口不足：%v", proxyPoolLogLines(env.registry))
	}
	env.locks.mu.Lock()
	calls := env.locks.calls
	env.locks.mu.Unlock()
	if calls != 1 {
		t.Fatalf("应只在第一个持有者时加锁：%d 次", calls)
	}
	for index, closer := range closers {
		closer.close()
		if held := env.locks.heldPorts(); index < 3 && len(held) != 1 || index == 3 && len(held) != 0 {
			t.Fatalf("第 %d 个归还后锁 = %v", index+1, held)
		}
	}
	checkFixedInvariants(t, env)
}

// TestProxyPoolFixedNeverFallsBack 冷却、其他进程锁住、全部端口暂停时：绑定始终是固定端口；被拒的租约不登记、不分配、URL 为空，
// 归还等全部是空操作；Go 侧出口不是 PROXY 也不是直连
func TestProxyPoolFixedNeverFallsBack(t *testing.T) {
	ids := proxyPoolTestIDs(4)
	env := newFixedTestEnv(t, "", fixedStaticPool(10001, 10002), nil, ids...)
	table := env.table()
	env.coolUpstream(ids[0])
	env.pool.mu.Lock()
	leases := env.pool.pooledLeases
	env.pool.mu.Unlock()
	for _, kind := range []string{proxyPoolKindWorker, proxyPoolKindLogin, proxyPoolKindVerify, proxyPoolKindImport} {
		lease := env.pool.acquire(ids[0], ids[0], kind, ids[0], env.global)
		var paused *proxyPoolPausedError
		if err := lease.Refused(); !errors.As(err, &paused) || !errors.Is(err, errProxyPoolPortPaused) || paused.Port != table[ids[0]] ||
			paused.Reason != fixedPauseCoolingUpstream || lease.URL() != "" || lease.Pooled() {
			t.Fatalf("%s：冷却时应拒绝、不填默认代理：%v %q", kind, err, lease.URL())
		}
		lease.release()
		lease.attach(&fakeProxyCloser{})
		lease.launchFailed(upstreamFailure, false)
		if claim := lease.releaseClaim(); claim != nil {
			t.Fatal("被拒的租约不应留下导入记录")
		}
	}
	env.pool.mu.Lock()
	registered := len(env.pool.byAccount[ids[0]])
	after := env.pool.pooledLeases
	env.pool.mu.Unlock()
	if registered != 0 || after != leases {
		t.Fatalf("被拒的租约不应登记：%d 个、池租约 %d → %d", registered, leases, after)
	}
	if effective := env.owner.effective(ids[0], env.global); effective != staticPortURL(t, table[ids[0]]) {
		t.Fatalf("暂停时 Go 侧出口应仍是固定端口：%s", config.RedactProxy(effective))
	}
	// 另一个端口被其他进程锁住：两个端口都暂停；新账户登录没有可用端口
	other := 10001
	if table[ids[0]] == 10001 {
		other = 10002
	}
	env.locks.setOther(other, true)
	var holder string
	for id, port := range table {
		if port == other {
			holder = id
		}
	}
	if lease, _ := env.worker(holder); lease.Refused() == nil {
		t.Fatal("被其他进程锁住的端口应拒绝")
	}
	login := env.pool.acquire("login:new", "", proxyPoolKindLogin, "auth", env.global)
	var paused *proxyPoolPausedError
	if !errors.As(login.Refused(), &paused) || paused.Port != 0 || paused.Error() != "IP 池固定分配没有可用的端口（全部暂停使用）" {
		t.Fatalf("全部暂停时新账户应拒绝：%v", login.Refused())
	}
	for _, id := range ids {
		if effective := env.owner.effective(id, env.global); effective == env.global || effective == "" || env.owner.binding(id).Hold == "" {
			t.Fatalf("%s 暂停时出口 %s、Hold %q", id, config.RedactProxy(effective), env.owner.binding(id).Hold)
		}
	}
	if env.count("IP 池固定分配没有可用的端口（全部暂停使用），不分配 | 用途=登录") != 1 {
		t.Fatalf("新账户被拒应写 F11：%v", proxyPoolLogLines(env.registry))
	}
	checkFixedInvariants(t, env)
	assertNoPoolSecretInLogs(t, env.registry)
	// 关闭固定分配：暂停原因随之清掉，按原方式分配（端口都不可用时按原方式退回默认代理，绑定为空）
	env.locks.setOther(other, false)
	env.clock.Advance(proxyPoolLockedRetry + time.Second)
	env.pool.recheckFired()
	env.pool.configure(staticProxyPool(10001, 10002), env.global, 8)
	bound := 0
	for _, id := range ids {
		binding := env.owner.binding(id)
		if binding != nil && binding.Hold != "" {
			t.Fatalf("关闭后 %s 不应再暂停：%s", id, fixedBindingText(binding))
		}
		if binding != nil {
			bound++
		}
	}
	if bound == 0 {
		t.Fatal("关闭后没有冷却的端口应照常做默认出口")
	}
}

// TestProxyPoolFixedHoldCooling 规则 B 冷却：这个端口的账户暂停（Hold、F9），其他端口不变；冷却到期只靠定时器也恢复（F10）；
// 从记录文件读回的冷却在启动后第一次 wake 就登记暂停
func TestProxyPoolFixedHoldCooling(t *testing.T) {
	ids := proxyPoolTestIDs(4)
	env := newFixedTestEnv(t, "", fixedStaticPool(10001, 10002), nil, ids...)
	table := env.table()
	port := table[ids[0]]
	env.coolUpstream(ids[0])
	env.pool.mu.Lock()
	until := env.pool.ports[port].coolUntil
	armed, recheckAt := env.pool.recheckArmed, env.pool.recheckAt
	env.pool.mu.Unlock()
	hold := fmt.Sprintf("IP 池固定端口 %d 冷却中（连续出口连不通），恢复=%s；固定分配不换端口", port, until.UTC().Format(time.RFC3339))
	for id, fixedPort := range table {
		binding := env.owner.binding(id)
		if fixedPort == port && binding.Hold != hold || fixedPort != port && binding.Hold != "" {
			t.Fatalf("%s（端口 %d）的 Hold = %q", id, fixedPort, binding.Hold)
		}
	}
	if env.count(fmt.Sprintf("WARN service IP 池固定端口暂停使用 | 端口=%d | 原因=冷却中（连续出口连不通） | 恢复=%s | 账户=2 | 固定分配不换端口",
		port, until.UTC().Format(time.RFC3339))) != 1 {
		t.Fatalf("应写 F9：%v", proxyPoolLogLines(env.registry))
	}
	if !armed || !recheckAt.Equal(until) {
		t.Fatalf("暂停时应按冷却到期安排检查：armed=%v at=%v until=%v", armed, recheckAt, until)
	}
	checkFixedInvariants(t, env)
	assertNoPoolSecretInLogs(t, env.registry)
	assertNoPoolSecret(t, "暂停原因", hold)
	env.clock.Advance(until.Sub(env.clock.Now()) + time.Second)
	// 只靠定时器（不调其他方法）恢复
	env.pool.recheckFired()
	for _, id := range env.usersOf(port) {
		if binding := env.owner.binding(id); binding.Hold != "" {
			t.Fatalf("冷却到期后 %s 仍暂停：%q", id, binding.Hold)
		}
	}
	if env.count(fmt.Sprintf("INFO service IP 池固定端口恢复使用 | 端口=%d | 账户=2 | 暂停=", port)) != 1 {
		t.Fatalf("应写 F10：%v", proxyPoolLogLines(env.registry))
	}
	checkFixedInvariants(t, env)

	// 读回的冷却：重启后规则未知，启动后第一次 wake 就登记
	env.coolUpstream(ids[0])
	env.pool.flush()
	clock := newProxyPoolTestClock()
	clock.Advance(env.clock.Now().Sub(clock.Now()))
	pool, registry, _ := newFixedPool(t, env.dir, clock, nil)
	pool.configure(fixedStaticPool(10001, 10002), proxyPoolTestGlobal, 8)
	accounts := make([]*aistudio.Account, 0, len(ids))
	for _, id := range ids {
		accounts = append(accounts, &aistudio.Account{ID: id, Config: aistudio.AccountConfig{Enabled: true}})
	}
	pool.prepareFixed(accounts)
	for _, account := range accounts {
		pool.prime(account)
	}
	owner := newFakeProxyOwner(ids...)
	pool.attach(owner)
	if binding := owner.binding(ids[0]); binding == nil || !strings.Contains(binding.Hold, fmt.Sprintf("IP 池固定端口 %d 冷却中，恢复=", port)) {
		t.Fatalf("读回的冷却应暂停（规则未知）：%s", fixedBindingText(binding))
	}
	if countProxyPoolLogs(registry, fmt.Sprintf("IP 池固定端口暂停使用 | 端口=%d | 原因=冷却中 |", port)) != 1 {
		t.Fatalf("启动后应登记暂停：%v", proxyPoolLogLines(registry))
	}
	if err := fixedInvariantError(pool, owner); err != nil {
		t.Fatal(err)
	}
}

// TestProxyPoolFixedHoldLockedElsewhere 端口被其他进程锁住：申请被拒、这个端口的账户暂停（不换端口）；其他进程释放后到重试时间探测恢复
func TestProxyPoolFixedHoldLockedElsewhere(t *testing.T) {
	ids := proxyPoolTestIDs(4)
	env := newFixedTestEnv(t, "", fixedStaticPool(10001, 10002), nil, ids...)
	port := env.port(ids[0])
	env.locks.setOther(port, true)
	lease, _ := env.worker(ids[0])
	var paused *proxyPoolPausedError
	if !errors.As(lease.Refused(), &paused) || paused.Reason != fixedPauseLockedElsewhere || env.port(ids[0]) != port {
		t.Fatalf("锁不到应拒绝、不换端口：%v", lease.Refused())
	}
	hold := fmt.Sprintf("IP 池固定端口 %d 被其他进程占用，释放后恢复；固定分配不换端口", port)
	for _, id := range env.usersOf(port) {
		if env.owner.binding(id).Hold != hold {
			t.Fatalf("%s 应暂停：%q", id, env.owner.binding(id).Hold)
		}
	}
	if env.count(fmt.Sprintf("IP 池固定端口暂停使用 | 端口=%d | 原因=被其他进程占用（多个进程不要共用同一端口段） | 下次检查=", port)) != 1 ||
		env.count(fmt.Sprintf("IP 池固定端口暂停使用，不分配 | 端口=%d | 原因=被其他进程占用 | 用途=Worker", port)) != 1 {
		t.Fatalf("应写 F9 与 F11：%v", proxyPoolLogLines(env.registry))
	}
	// 重试时间到、仍被锁住：不重复写 F9
	env.clock.Advance(proxyPoolLockedRetry + time.Second)
	env.pool.recheckFired()
	if env.count(fmt.Sprintf("IP 池固定端口暂停使用 | 端口=%d |", port)) != 1 {
		t.Fatalf("仍被锁住时不应重复写 F9：%v", proxyPoolLogLines(env.registry))
	}
	env.locks.setOther(port, false)
	env.clock.Advance(proxyPoolLockedRetry + time.Second)
	env.pool.recheckFired()
	for _, id := range env.usersOf(port) {
		if env.owner.binding(id).Hold != "" {
			t.Fatalf("释放后 %s 应恢复", id)
		}
	}
	if env.count(fmt.Sprintf("IP 池固定端口恢复使用 | 端口=%d | 账户=2", port)) != 1 {
		t.Fatalf("应写 F10：%v", proxyPoolLogLines(env.registry))
	}
	if lease, _ := env.worker(ids[0]); lease.Refused() != nil || leasePort(t, lease) != port {
		t.Fatalf("恢复后应在固定端口上启动：%v", lease.Refused())
	}
	checkFixedInvariants(t, env)
}

// TestProxyPoolFixedNewAccountLogin 新账户登录选负载最少、不暂停的端口并预留；锁不到换下一个；并发 5 个登录分到 5 个端口；
// 失败归还撤销预留；adopt 写表（来源 login）、不写亲和
func TestProxyPoolFixedNewAccountLogin(t *testing.T) {
	ids := proxyPoolTestIDs(5)
	env := newFixedTestEnv(t, "", fixedStaticPool(10001, 10006), nil, ids...)
	counts := fixedCountsOf(env.table())
	var logins []*proxyPoolLease
	seen := make(map[int]bool)
	for index := range 5 {
		lease := env.pool.acquire(fmt.Sprintf("login:%d", index), "", proxyPoolKindLogin, "auth", env.global)
		port := leasePort(t, lease)
		if lease.fixedToken == "" || seen[port] && len(seen) < 6 {
			t.Fatalf("第 %d 个登录应预留并分散：端口 %d %v", index, port, seen)
		}
		seen[port] = true
		logins = append(logins, lease)
	}
	// 账户少的端口先分（5 个账户 6 个端口：先给空端口）
	for port := range seen {
		if counts[port] > 1 {
			t.Fatalf("应先分负载最少的端口：%d 已有 %d 个", port, counts[port])
		}
	}
	env.pool.mu.Lock()
	reserved := len(env.pool.fixed.reserved)
	env.pool.mu.Unlock()
	if reserved != 5 {
		t.Fatalf("预留 = %d", reserved)
	}
	for _, lease := range logins[1:] {
		lease.release()
	}
	env.owner.add("new@example.com", "")
	logins[0].adopt("new@example.com")
	env.pool.refresh(env.owner, "new@example.com")
	env.pool.mu.Lock()
	reserved = len(env.pool.fixed.reserved)
	entry := env.pool.fixed.entries["new@example.com"]
	_, affinity := env.pool.affinity["new@example.com"]
	env.pool.mu.Unlock()
	if reserved != 0 || entry.port != logins[0].port || entry.source != fixedSourceLogin || affinity {
		t.Fatalf("adopt 后预留 %d、表 %+v、亲和 %v", reserved, entry, affinity)
	}
	if env.count(fmt.Sprintf("INFO new@example.com IP 池固定分配 | 端口=%d | 来源=登录 | 该端口账户=", entry.port)) != 1 {
		t.Fatalf("应写 F6：%v", proxyPoolLogLines(env.registry))
	}
	logins[0].release()
	// 最少的端口被其他进程锁住：换下一个（新账户还没有分配，换端口不算挪动）
	least := env.pool.fixedLoginPreview(t)
	env.locks.setOther(least, true)
	lease := env.pool.acquire("login:x", "", proxyPoolKindLogin, "auth", env.global)
	if lease.Refused() != nil || leasePort(t, lease) == least {
		t.Fatalf("锁不到应换下一个端口：%v %d", lease.Refused(), lease.port)
	}
	lease.release()
	checkFixedInvariants(t, env)
}

// fixedLoginPreview 返回新账户此刻会分到的端口（测试用）
func (pool *proxyPool) fixedLoginPreview(t *testing.T) int {
	t.Helper()
	pool.mu.Lock()
	defer pool.mu.Unlock()
	return pool.fixedLoginPreviewLocked(pool.now())
}

// TestProxyPoolFixedImportClaims 整批导入 10 个分散到 10 个端口；adopt 写表（来源 import，不检查端口有没有被别的键持有、不看 EndpointKey）；
// 中途失败 discard 撤销；1 小时后过期撤销
func TestProxyPoolFixedImportClaims(t *testing.T) {
	ids := proxyPoolTestIDs(10)
	env := newFixedTestEnv(t, "", fixedStaticPool(10001, 10010), nil, ids...)
	var claims []*proxyPoolClaim
	ports := make(map[int]bool)
	for index := range 10 {
		lease := env.pool.acquire(fmt.Sprintf("login:i%d", index), "", proxyPoolKindImport, "auth", env.global)
		claim := lease.releaseClaim()
		if claim == nil || !claim.fixed || claim.fixedToken == "" {
			t.Fatalf("第 %d 个导入应留下固定分配的记录：%+v", index, claim)
		}
		ports[claim.port] = true
		claims = append(claims, claim)
	}
	if len(ports) != 10 {
		t.Fatalf("整批导入应分散到 10 个端口：%v", ports)
	}
	// 别的键正持有 claims[0] 的端口（共用是常态）：adopt 照样写表
	holder, _ := env.worker(env.usersOf(claims[0].port)[0])
	env.owner.add("imp0@example.com", "")
	claims[0].adopt("imp0@example.com")
	env.owner.add("imp1@example.com", "")
	claims[1].adopt("imp1@example.com")
	claims[1].adopt("imp1@example.com")
	table := env.table()
	if table["imp0@example.com"] != claims[0].port || table["imp1@example.com"] != claims[1].port {
		t.Fatalf("adopt 应写导入用的端口：%v", table)
	}
	holder.release()
	claims[2].discard()
	claims[2].discard()
	claims[0].discard()
	env.pool.mu.Lock()
	reserved := len(env.pool.fixed.reserved)
	env.pool.mu.Unlock()
	if reserved != 7 {
		t.Fatalf("adopt 与 discard 后应剩 7 个预留：%d", reserved)
	}
	env.clock.Advance(proxyPoolFixedReserveTTL + time.Second)
	lease := env.pool.acquire("login:late", "", proxyPoolKindImport, "auth", env.global)
	lease.release()
	env.pool.mu.Lock()
	reserved = len(env.pool.fixed.reserved)
	env.pool.mu.Unlock()
	if reserved != 0 {
		t.Fatalf("1 小时后过期的预留应撤销：%d", reserved)
	}
	for _, claim := range claims[3:] {
		claim.discard()
	}
	env.pool.refresh(env.owner, "imp0@example.com")
	env.pool.refresh(env.owner, "imp1@example.com")
	checkFixedInvariants(t, env)
}

// TestProxyPoolFixedDelete 删除只释放这一条、其他账户不动、立即写盘；休眠的表同样释放；没有条目时不写日志、不写盘
func TestProxyPoolFixedDelete(t *testing.T) {
	ids := proxyPoolTestIDs(6)
	env := newFixedTestEnv(t, "", fixedStaticPool(10001, 10003), nil, ids...)
	before := env.table()
	env.owner.remove(ids[0])
	env.pool.releaseFixed(ids[0], "账户已删除")
	after := env.table()
	if _, ok := after[ids[0]]; ok || len(after) != len(before)-1 {
		t.Fatalf("应只释放这一条：%v", after)
	}
	for id, port := range after {
		if before[id] != port {
			t.Fatalf("删除不应重排其他账户：%s %d → %d", id, before[id], port)
		}
	}
	data, err := os.ReadFile(filepath.Join(env.dir, proxyPoolFixedFileName))
	if err != nil || strings.Contains(string(data), ids[0]) {
		t.Fatalf("删除应立即写盘：%v\n%s", err, data)
	}
	if env.count(fmt.Sprintf("INFO %s IP 池固定分配已释放 | 端口=%d | 原因=账户已删除 | 该端口账户=1", ids[0], before[ids[0]])) != 1 {
		t.Fatalf("应写 F7：%v", proxyPoolLogLines(env.registry))
	}
	// 休眠的表（开关关了）照样释放
	env.pool.configure(staticProxyPool(10001, 10003), env.global, 8)
	env.pool.releaseFixed(ids[1], "账户已删除")
	if _, ok := env.table()[ids[1]]; ok {
		t.Fatal("休眠的表也应释放")
	}
	// 没有条目：不写日志、不写盘
	stat, _ := os.Stat(filepath.Join(env.dir, proxyPoolFixedFileName))
	logs := len(proxyPoolLogLines(env.registry))
	env.pool.releaseFixed("missing@example.com", "账户已删除")
	again, _ := os.Stat(filepath.Join(env.dir, proxyPoolFixedFileName))
	if len(proxyPoolLogLines(env.registry)) != logs || !again.ModTime().Equal(stat.ModTime()) {
		t.Fatal("没有条目时不应写日志、写盘")
	}
}

// TestProxyPoolFixedMissingKept 账户不在账户池（载入失败、目录暂时移走）时不释放：sync 与重启都保留，status 计 stray，回来时回原端口；
// 服务运行中的重新平均分配清理它，服务停止时不清理
func TestProxyPoolFixedMissingKept(t *testing.T) {
	ids := proxyPoolTestIDs(6)
	env := newFixedTestEnv(t, "", fixedStaticPool(10001, 10003), nil, ids...)
	want := env.port(ids[0])
	env.owner.remove(ids[0])
	env.pool.attach(env.owner)
	if env.port(ids[0]) != want {
		t.Fatal("不在账户池的账户不应释放")
	}
	if status := env.pool.status(); status.Fixed == nil || status.Fixed.Stray != 1 || status.Fixed.Accounts != 6 {
		t.Fatalf("status = %+v", status.Fixed)
	}
	// 重启时这个账户的目录读失败（LoadTolerant 跳过）
	accounts := make([]*aistudio.Account, 0, len(ids))
	for _, id := range ids[1:] {
		accounts = append(accounts, &aistudio.Account{ID: id})
	}
	env.pool.prepareFixed(accounts)
	if env.port(ids[0]) != want {
		t.Fatal("重启时读不到的账户不应释放")
	}
	env.owner.add(ids[0], "")
	env.pool.refresh(env.owner, ids[0])
	if env.port(ids[0]) != want || env.owner.binding(ids[0]).URL != staticPortURL(t, want) {
		t.Fatal("回来时应回原端口")
	}
	env.owner.remove(ids[0])
	env.pool.attach(env.owner)
	if result, err := env.pool.fixedRebalance(nil, false); err != nil || result.Pruned != 0 || env.port(ids[0]) != want {
		t.Fatalf("服务停止时不清理：%+v %v", result, err)
	}
	known := make(map[string]struct{})
	for _, id := range env.owner.AccountIDs() {
		known[id] = struct{}{}
	}
	if result, err := env.pool.fixedRebalance(known, false); err != nil || result.Pruned != 1 || env.port(ids[0]) != 0 {
		t.Fatalf("服务运行中的重新平均分配应清理：%+v %v", result, err)
	}
	checkFixedInvariants(t, env)
}

// TestProxyPoolFixedOwnProxy 改用自己的代理释放（F7）；清空后分到最少的端口（F6）；休眠期间改用自己代理的账户再开启时释放
func TestProxyPoolFixedOwnProxy(t *testing.T) {
	const own = "http://203.0.113.50:3128"
	ids := proxyPoolTestIDs(5)
	env := newFixedTestEnv(t, "", fixedStaticPool(10001, 10002), map[string]string{ids[4]: own}, ids...)
	if _, ok := env.table()[ids[4]]; ok {
		t.Fatal("有自己代理的账户不参与分配")
	}
	port := env.port(ids[0])
	env.owner.add(ids[0], own)
	env.pool.refresh(env.owner, ids[0])
	if _, ok := env.table()[ids[0]]; ok || env.count(fmt.Sprintf("INFO %s IP 池固定分配已释放 | 端口=%d | 原因=账户改用自己的代理", ids[0], port)) != 1 {
		t.Fatalf("改用自己的代理应释放：%v", proxyPoolLogLines(env.registry))
	}
	env.owner.add(ids[0], "")
	env.pool.refresh(env.owner, ids[0])
	if env.port(ids[0]) == 0 || env.count(fmt.Sprintf("INFO %s IP 池固定分配 | 端口=%d | 来源=新增", ids[0], env.port(ids[0]))) != 1 {
		t.Fatalf("清空后应分到最少的端口：%v", proxyPoolLogLines(env.registry))
	}
	// 休眠期间改用自己的代理：开关关着时不动表，再开启时释放
	env.pool.configure(staticProxyPool(10001, 10002), env.global, 8)
	env.owner.add(ids[1], own)
	env.pool.refresh(env.owner, ids[1])
	if _, ok := env.table()[ids[1]]; !ok {
		t.Fatal("开关关着时不应改表")
	}
	env.pool.configure(fixedStaticPool(10001, 10002), env.global, 8)
	if _, ok := env.table()[ids[1]]; ok {
		t.Fatal("再开启时应释放改用自己代理的账户")
	}
	checkFixedInvariants(t, env)
}

// TestProxyPoolFixedRangeChange 缩小、平移只挪段外的账户（F8），其他不动；扩大谁都不挪；休眠期间变段再开启同样只挪段外的；段外的停用记录删除
func TestProxyPoolFixedRangeChange(t *testing.T) {
	ids := proxyPoolTestIDs(12)
	env := newFixedTestEnv(t, "", fixedStaticPool(10001, 10004), nil, ids...)
	if _, _, err := env.pool.fixedReassign(10004, false); err != nil {
		t.Fatal(err)
	}
	before := env.table()
	env.pool.configure(fixedStaticPool(10001, 10003), env.global, 8)
	after := env.table()
	for id, port := range before {
		if port <= 10003 && after[id] != port || after[id] < 10001 || after[id] > 10003 {
			t.Fatalf("缩小时只挪段外的：%s %d → %d", id, port, after[id])
		}
	}
	if env.count("WARN service IP 池端口段变化") != 0 {
		// 10004 已被换 IP 挪空：没有要挪的账户时不写 F8
		t.Fatalf("没有挪动时不应写 F8：%v", proxyPoolLogLines(env.registry))
	}
	env.pool.mu.Lock()
	_, excluded := env.pool.fixed.excluded[10004]
	env.pool.mu.Unlock()
	if excluded {
		t.Fatal("段外的停用记录应删除")
	}
	// 平移：10001 移出段外
	before = after
	env.pool.configure(fixedStaticPool(10002, 10005), env.global, 8)
	after = env.table()
	moved := 0
	for id, port := range before {
		if port != 10001 && after[id] != port {
			t.Fatalf("平移时段内的账户不应动：%s", id)
		}
		if port == 10001 {
			moved++
			if after[id] < 10002 || after[id] > 10005 {
				t.Fatalf("段外的账户应挪进新段：%s → %d", id, after[id])
			}
		}
	}
	if env.count(fmt.Sprintf("WARN service IP 池端口段变化，固定分配只挪段外的账户 | 端口=10002-10005（参与 4 个） | 挪动=%d | 每端口=", moved)) != 1 {
		t.Fatalf("应写 F8：%v", proxyPoolLogLines(env.registry))
	}
	// 扩大：谁都不挪
	before = after
	env.pool.configure(fixedStaticPool(10001, 10008), env.global, 8)
	if fmt.Sprint(env.table()) != fmt.Sprint(before) {
		t.Fatal("扩大时谁都不应挪")
	}
	// 休眠期间变段，再开启
	env.pool.configure(staticProxyPool(10001, 10003), env.global, 8)
	if fmt.Sprint(env.table()) != fmt.Sprint(before) {
		t.Fatal("开关关着时不应改表")
	}
	env.pool.configure(fixedStaticPool(10001, 10003), env.global, 8)
	for id, port := range env.table() {
		if port > 10003 || before[id] <= 10003 && port != before[id] {
			t.Fatalf("再开启时只挪段外的：%s %d → %d", id, before[id], port)
		}
	}
	checkFixedInvariants(t, env)
}

// TestProxyPoolFixedRebalanceReassignRestore dry-run 不改表；重新平均分配后均衡、挪动数最少；换 IP 后端口停用，新账户与重新平均都不用它；
// 恢复后可用；运行中的计数
func TestProxyPoolFixedRebalanceReassignRestore(t *testing.T) {
	ids := proxyPoolTestIDs(8)
	env := newFixedTestEnv(t, "", fixedStaticPool(10001, 10002), nil, ids...)
	// 加 IP：端口段扩大到 4 个，原来的账户不动（不均匀）
	env.pool.configure(fixedStaticPool(10001, 10004), env.global, 8)
	before := env.table()
	running, _ := env.worker(ids[0])
	dry, err := env.pool.fixedRebalance(nil, true)
	if err != nil || !dry.DryRun || dry.Moved != 4 || fmt.Sprint(env.table()) != fmt.Sprint(before) || dry.MinPerPort != 2 || dry.MaxPerPort != 2 {
		t.Fatalf("dry-run = %+v (%v)", dry, err)
	}
	result, err := env.pool.fixedRebalance(nil, false)
	if err != nil || result.Moved != dry.Moved || result.Running != dry.Running {
		t.Fatalf("执行 = %+v (%v)", result, err)
	}
	after := env.table()
	assertFixedTableBalanced(t, after, fixedTestPorts(10001, 4))
	if after[ids[0]] != before[ids[0]] {
		t.Fatal("有浏览器在固定端口上服务的账户应尽量不挪")
	}
	if again, _ := env.pool.fixedRebalance(nil, false); again.Moved != 0 {
		t.Fatalf("均衡后再执行应不挪：%+v", again)
	}
	if env.count("INFO service IP 池固定分配已重新平均 | 账户=8 | 端口=4 | 挪动=4 | 清理=0 | 每端口=2-2") != 1 ||
		env.count("IP 池固定分配换端口 | 端口=") != 4 {
		t.Fatalf("应写 F14 与 F15：%v", proxyPoolLogLines(env.registry))
	}
	// 换 IP：10001 上的账户挪到其他最少的端口，10001 停用
	movers := env.usersOf(10001)
	dryReassign, serving, err := env.pool.fixedReassign(10001, true)
	if err != nil || dryReassign.Moved != len(movers) || dryReassign.Excluded || env.port(movers[0]) != 10001 {
		t.Fatalf("换 IP dry-run = %+v (%v)", dryReassign, err)
	}
	reassign, serving, err := env.pool.fixedReassign(10001, false)
	if err != nil || reassign.Moved != len(movers) || !reassign.Excluded || len(reassign.Targets) == 0 {
		t.Fatalf("换 IP = %+v (%v)", reassign, err)
	}
	if after[ids[0]] == 10001 && (len(serving) != 1 || serving[0] != ids[0] || reassign.Running != 1) {
		t.Fatalf("在旧端口上服务的应列出：%v %+v", serving, reassign)
	}
	for _, id := range movers {
		if port := env.port(id); port == 10001 || port == 0 {
			t.Fatalf("%s 应挪走：%d", id, port)
		}
	}
	login := env.pool.acquire("login:z", "", proxyPoolKindLogin, "auth", env.global)
	if leasePort(t, login) == 10001 {
		t.Fatal("停用的端口不应再分给新账户")
	}
	login.release()
	if result, _ := env.pool.fixedRebalance(nil, false); result.Ports != 3 {
		t.Fatalf("停用的端口不参与重新平均：%+v", result)
	}
	for _, port := range env.table() {
		if port == 10001 {
			t.Fatal("重新平均不应把账户放回停用的端口")
		}
	}
	if restored, err := env.pool.fixedRestore(10001); err != nil || !restored.Restored {
		t.Fatalf("恢复 = %+v (%v)", restored, err)
	}
	if restored, _ := env.pool.fixedRestore(10001); restored.Restored {
		t.Fatal("没有停用时 restored 应为假")
	}
	if login := env.pool.acquire("login:y", "", proxyPoolKindLogin, "auth", env.global); leasePort(t, login) != 10001 {
		t.Fatal("恢复后最少的 10001 应可再分配")
	} else {
		login.release()
	}
	if env.count("INFO service IP 池固定端口恢复分配 | 端口=10001") != 1 {
		t.Fatalf("应写 F17：%v", proxyPoolLogLines(env.registry))
	}
	// 错误
	if _, _, err := env.pool.fixedReassign(20001, false); !strings.Contains(fmt.Sprint(err), "端口不在当前端口段内") {
		t.Fatalf("段外端口：%v", err)
	}
	running.release()
	checkFixedInvariants(t, env)
	env.pool.configure(staticProxyPool(10001, 10004), env.global, 8)
	if _, err := env.pool.fixedRebalance(nil, false); !strings.Contains(fmt.Sprint(err), "固定分配没有开启") {
		t.Fatalf("没开启时：%v", err)
	}
}

// TestProxyPoolFixedTransitional 换 IP 之前就在旧端口上运行的 Worker（过渡 Worker）：绑定仍随它，验证跟随它（同一账户同时只用一个 IP），
// status 计 transitional；Worker 关闭后绑定切到新的固定端口，新 Worker 上新端口
func TestProxyPoolFixedTransitional(t *testing.T) {
	ids := proxyPoolTestIDs(3)
	env := newFixedTestEnv(t, "", fixedStaticPool(10001, 10003), nil, ids...)
	id := ids[0]
	lease, closer := env.worker(id)
	old := leasePort(t, lease)
	if old != env.port(id) {
		t.Fatalf("Worker 应在固定端口上：%d %d", old, env.port(id))
	}
	// 固定端口改到别的端口、浏览器还没关闭（重新平均分配之后；“换 IP”停用的端口不跟随，见 TestProxyPoolFixedReassignClosesTransitional）
	moved := 10001
	if old == moved {
		moved = 10002
	}
	env.pool.mu.Lock()
	env.pool.fixedSetLocked(id, moved, env.clock.Now(), fixedSourceRebalance)
	env.pool.pushLocked(id)
	env.pool.mu.Unlock()
	fixedPort := env.port(id)
	if binding := env.owner.binding(id); fixedPort == old || binding.Slot != strconv.Itoa(old) || !binding.Active {
		t.Fatalf("过渡期绑定应仍随运行中的 Worker：%s（固定端口 %d）", fixedBindingText(binding), fixedPort)
	}
	if status := env.pool.status(); status.Fixed.Transitional != 1 {
		t.Fatalf("transitional = %d", status.Fixed.Transitional)
	}
	verify := env.pool.acquire(id, id, proxyPoolKindVerify, id, env.global)
	if leasePort(t, verify) != old || env.count(fmt.Sprintf("IP 池分配 | 端口=%d | 来源=跟随运行中的 Worker | 用途=验证", old)) != 1 {
		t.Fatalf("验证应跟随运行中的过渡 Worker：%d %v", verify.port, proxyPoolLogLines(env.registry))
	}
	verify.release()
	// 过渡 Worker 的端口暂停时验证不跟随它，用固定端口
	closer.close()
	if binding := env.owner.binding(id); binding.Slot != strconv.Itoa(fixedPort) || binding.Active {
		t.Fatalf("Worker 关闭后应切到固定端口 %d：%s", fixedPort, fixedBindingText(binding))
	}
	if lease, _ := env.worker(id); leasePort(t, lease) != fixedPort {
		t.Fatal("新 Worker 应在固定端口上")
	}
	if status := env.pool.status(); status.Fixed.Transitional != 0 {
		t.Fatalf("transitional = %d", status.Fixed.Transitional)
	}
	checkFixedInvariants(t, env)
}

// TestProxyPoolFixedCoolingWorkers 固定分配下端口冷却：回调只列出过渡 Worker（固定端口不是这个端口的），固定端口就是它的不重启
func TestProxyPoolFixedCoolingWorkers(t *testing.T) {
	ids := proxyPoolTestIDs(4)
	env := newFixedTestEnv(t, "", fixedStaticPool(10001, 10002), nil, ids...)
	users := env.usersOf(10001)
	transitional, resident := users[0], users[1]
	_, closer := env.worker(transitional)
	_, residentCloser := env.worker(resident)
	// transitional 的固定端口改到 10002（模拟手动挪动之后、浏览器还没关闭）
	env.pool.mu.Lock()
	env.pool.fixedSetLocked(transitional, 10002, env.clock.Now(), fixedSourceRebalance)
	env.pool.mu.Unlock()
	var cooled []proxyPoolCooling
	env.pool.setPortCooledHook(func(cooling proxyPoolCooling) { cooled = append(cooled, cooling) })
	env.coolUpstream(resident)
	if len(cooled) != 1 || !cooled[0].Fixed || len(cooled[0].Workers) != 1 || cooled[0].Workers[0] != transitional {
		t.Fatalf("冷却回调应只列出过渡 Worker：%+v", cooled)
	}
	closer.close()
	residentCloser.close()
	env.pool.refresh(env.owner, transitional)
	checkFixedInvariants(t, env)
}

// TestProxyPoolFixedExternal 外部占用的端口照常分配、不暂停、不挪，固定分配下的外部占用 WARN 一次
func TestProxyPoolFixedExternal(t *testing.T) {
	ids := proxyPoolTestIDs(4)
	env := newFixedTestEnv(t, "", fixedStaticPool(10001, 10002), nil, ids...)
	before := env.table()
	env.pool.configure(fixedStaticPool(10001, 10002), "http://isp.example.com:10001", 8)
	env.pool.configure(fixedStaticPool(10001, 10002), "http://isp.example.com:10001", 9)
	if fmt.Sprint(env.table()) != fmt.Sprint(before) {
		t.Fatal("外部占用不应挪账户")
	}
	for _, id := range env.usersOf(10001) {
		if env.owner.binding(id).Hold != "" {
			t.Fatal("外部占用不应暂停")
		}
		if lease, _ := env.worker(id); lease.Refused() != nil || leasePort(t, lease) != 10001 {
			t.Fatal("外部占用的端口照常分配")
		}
	}
	if env.count("WARN service IP 池端口段与账户固定代理或 PROXY 重叠，固定分配照常使用这些端口（与它们共用 IP） | 端口数=1 | 账户数=0 | 可用“换 IP”挪走这些端口上的固定账户") != 1 ||
		env.count("这些端口不再分配") != 0 {
		t.Fatalf("应写一次 F21：%v", proxyPoolLogLines(env.registry))
	}
	if status := env.pool.status(); status.Fixed.ExternalPorts != 1 {
		t.Fatalf("external_ports = %d", status.Fixed.ExternalPorts)
	}
}

// TestProxyPoolFixedEndpointKeyChange 换代理商或子账号：固定表不变，冷却与暂停清掉，写 F22
func TestProxyPoolFixedEndpointKeyChange(t *testing.T) {
	ids := proxyPoolTestIDs(4)
	env := newFixedTestEnv(t, "", fixedStaticPool(10001, 10002), nil, ids...)
	before := env.table()
	env.coolUpstream(ids[0])
	if env.owner.binding(ids[0]).Hold == "" {
		t.Fatal("冷却后应暂停")
	}
	other := fixedStaticPool(10001, 10002)
	other.URL = "isp.example.com:10000:user-other:" + proxyPoolTestPassword
	env.pool.configure(other, env.global, 8)
	if fmt.Sprint(env.table()) != fmt.Sprint(before) {
		t.Fatal("换代理商不应改固定表")
	}
	for _, id := range ids {
		if env.owner.binding(id).Hold != "" {
			t.Fatalf("换代理商后冷却清掉，%s 不应暂停", id)
		}
	}
	if env.count("INFO service IP 池换了代理商或子账号，固定分配按端口号保留（同一端口号的 IP 可能已变）") != 1 {
		t.Fatalf("应写 F22：%v", proxyPoolLogLines(env.registry))
	}
	checkFixedInvariants(t, env)
}

// TestProxyPoolFixedDynamicNoRotate 动态固定会话：连续 2 次上游失败不换会话（代号不变、没有“IP 池换会话”、有 F18）；出口判定也不换
func TestProxyPoolFixedDynamicNoRotate(t *testing.T) {
	ids := proxyPoolTestIDs(2)
	dynamic := dynamicProxyPool()
	dynamic.Fixed = true
	env := newFixedTestEnv(t, "", dynamic, nil, ids...)
	before := env.owner.binding(ids[0]).Slot
	for range 4 {
		lease := env.pool.acquire(ids[0], ids[0], proxyPoolKindWorker, ids[0], env.global)
		lease.launchFailed(upstreamFailure, false)
	}
	env.clock.Advance(proxyPoolRotateInterval + time.Minute)
	if rotateSessionForTest(env.pool, ids[0], "出口受限") {
		t.Fatal("固定会话下出口判定也不应换会话")
	}
	env.pool.mu.Lock()
	generation := 0
	if session := env.pool.sessions[ids[0]]; session != nil {
		generation = session.generation
	}
	env.pool.mu.Unlock()
	if generation != 0 || env.owner.binding(ids[0]).Slot != before || env.count("IP 池换会话") != 0 {
		t.Fatalf("不应换会话：代号 %d、会话 %s → %s", generation, before, env.owner.binding(ids[0]).Slot)
	}
	if env.count(fmt.Sprintf("WARN %s IP 池固定会话不换会话 | 会话=%s | 原因=连续 2 次出口连不通 | 代理商恢复这个会话前，这个账户的 Worker 启动会失败", ids[0], before)) != 1 {
		t.Fatalf("应写一次 F18（30 分钟内去重）：%v", proxyPoolLogLines(env.registry))
	}
	if env.count("IP 池已更新 | 模式=动态 | 出口=http://gate.example.com | 固定会话=开启") != 1 {
		t.Fatalf("IP 池已更新应带固定会话后缀：%v", proxyPoolLogLines(env.registry))
	}
	// 关闭后恢复换会话；F5
	env.pool.configure(dynamicProxyPool(), env.global, 8)
	if env.count("INFO service IP 池固定会话已关闭，连续出口连不通时恢复自动换会话") != 1 {
		t.Fatalf("应写 F5：%v", proxyPoolLogLines(env.registry))
	}
	if !rotateSessionForTest(env.pool, ids[1], "出口受限") {
		t.Fatal("关闭后出口判定应照常换会话")
	}
	env.pool.configure(dynamic, env.global, 8)
	if env.count("INFO service IP 池固定会话已开启：不再自动换会话 | 账户=2") != 1 {
		t.Fatalf("应写 F4：%v", proxyPoolLogLines(env.registry))
	}
	if status := env.pool.status(); status.Fixed == nil || status.Fixed.Mode != "dynamic" || status.Fixed.Accounts != 2 {
		t.Fatalf("动态固定的状态 = %+v", status.Fixed)
	}
}

// TestProxyPoolFixedPoolsAccount 静态固定下 poolsAccount 为假、pooledAccounts 为 nil（出口判定照常适用）；动态固定与关闭时同原来
func TestProxyPoolFixedPoolsAccount(t *testing.T) {
	ids := proxyPoolTestIDs(2)
	env := newFixedTestEnv(t, "", fixedStaticPool(10001, 10002), nil, ids...)
	if env.pool.poolsAccount(ids[0]) || env.pool.pooledAccounts(ids) != nil {
		t.Fatal("静态固定下应按真实出口走闸门")
	}
	env.pool.configure(staticProxyPool(10001, 10002), env.global, 8)
	if !env.pool.poolsAccount(ids[0]) || len(env.pool.pooledAccounts(ids)) != 2 {
		t.Fatal("关闭时同原来")
	}
	dynamic := dynamicProxyPool()
	dynamic.Fixed = true
	env.pool.configure(dynamic, env.global, 8)
	if !env.pool.poolsAccount(ids[0]) || len(env.pool.pooledAccounts(ids)) != 2 {
		t.Fatal("动态固定同原来")
	}
}

// TestProxyPoolFixedCapacityLogs 固定分配不写端口不足；从不足切到固定不写“已足够”；切回非固定按原样 WARN
func TestProxyPoolFixedCapacityLogs(t *testing.T) {
	clock := newProxyPoolTestClock()
	pool, registry, _ := newFixedPool(t, "", clock, nil)
	pool.configure(staticProxyPool(10001, 10002), "", 10)
	if countProxyPoolLogs(registry, "IP 池端口少于可能同时运行的浏览器数") != 1 {
		t.Fatal("非固定时端口不足应 WARN")
	}
	pool.configure(fixedStaticPool(10001, 10002), "", 10)
	pool.setCapacity(20)
	if countProxyPoolLogs(registry, "IP 池端口少于可能同时运行的浏览器数") != 1 || countProxyPoolLogs(registry, "IP 池端口已足够") != 0 {
		t.Fatalf("固定分配不应写端口不足或已足够：%v", proxyPoolLogLines(registry))
	}
	pool.configure(staticProxyPool(10001, 10002), "", 20)
	if countProxyPoolLogs(registry, "WARN service IP 池端口少于可能同时运行的浏览器数，超出的会与其他账号共用端口 | 端口=2 | 需要=20") != 1 {
		t.Fatalf("切回非固定应原样 WARN：%v", proxyPoolLogLines(registry))
	}
}

// TestProxyPoolFixedStatus 状态字段：端口、账户、每端口最少与最多、空端口、预留、停用、暂停（按恢复时间）；JSON 不含账号密码
func TestProxyPoolFixedStatus(t *testing.T) {
	ids := proxyPoolTestIDs(5)
	env := newFixedTestEnv(t, "", fixedStaticPool(10001, 10004), nil, ids...)
	login := env.pool.acquire("login:s", "", proxyPoolKindLogin, "auth", env.global)
	defer login.release()
	env.coolUpstream(ids[0])
	if _, _, err := env.pool.fixedReassign(10004, false); err != nil {
		t.Fatal(err)
	}
	status := env.pool.status()
	fixed := status.Fixed
	if fixed == nil || fixed.Mode != "static" || fixed.Ports != 3 || fixed.Accounts != 5 || fixed.Reserved != 1 ||
		fixed.ExcludedTotal != 1 || len(fixed.Excluded) != 1 || fixed.Excluded[0] != 10004 || fixed.PausedPorts != 1 ||
		fixed.PausedAccounts != len(env.usersOf(env.port(ids[0]))) || len(fixed.Paused) != 1 ||
		fixed.Paused[0].Reason != fixedPauseCoolingUpstream || fixed.Paused[0].Port != env.port(ids[0]) || fixed.Since.IsZero() {
		t.Fatalf("status.fixed = %+v", fixed)
	}
	if fixed.MinPerPort != 1 || fixed.MaxPerPort != 2 {
		t.Fatalf("每端口 = %d-%d", fixed.MinPerPort, fixed.MaxPerPort)
	}
	data := mustJSON(t, status)
	assertNoPoolSecret(t, "状态", data)
	if !strings.Contains(data, `"fixed":{"mode":"static"`) {
		t.Fatalf("状态 JSON：%s", data)
	}
	env.pool.configure(staticProxyPool(10001, 10004), env.global, 8)
	if status := env.pool.status(); status.Fixed != nil || strings.Contains(mustJSON(t, status), `"fixed"`) {
		t.Fatal("关闭时状态不应有 fixed")
	}
}

// TestProxyPoolFixedPersistence 往返（since、段、条目、来源、停用）；坏文件与未知版本改名保留、F23、按亲和确定性重新迁移；文件不含账号密码与主机；
// 从没开过不创建；proxy-pool.json 不因固定操作改变
func TestProxyPoolFixedPersistence(t *testing.T) {
	ids := proxyPoolTestIDs(6)
	env := newFixedTestEnv(t, "", fixedStaticPool(10001, 10003), nil, ids...)
	if _, _, err := env.pool.fixedReassign(10003, false); err != nil {
		t.Fatal(err)
	}
	env.pool.flush()
	path := filepath.Join(env.dir, proxyPoolFixedFileName)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"http", "isp.example.com", "user-static", proxyPoolTestPassword, "endpoint"} {
		if strings.Contains(string(data), forbidden) {
			t.Fatalf("固定表文件不应含 %q：\n%s", forbidden, data)
		}
	}
	var record proxyPoolFixedRecord
	if err := json.Unmarshal(data, &record); err != nil || record.Version != 1 || record.Since.IsZero() || record.PortStart != 10001 ||
		record.PortEnd != 10003 || len(record.Accounts) != 6 || len(record.Excluded) != 1 {
		t.Fatalf("记录 = %+v (%v)", record, err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("权限 = %v (%v)", info.Mode(), err)
	}
	want := env.table()
	pool, _, _ := newFixedPool(t, env.dir, newProxyPoolTestClock(), nil)
	pool.mu.Lock()
	loaded := make(map[string]int)
	sources := make(map[string]string)
	for id, entry := range pool.fixed.entries {
		loaded[id], sources[id] = entry.port, entry.source
	}
	_, excluded := pool.fixed.excluded[10003]
	pool.mu.Unlock()
	if fmt.Sprint(loaded) != fmt.Sprint(want) || !excluded || sources[ids[0]] == "" {
		t.Fatalf("往返不一致：%v → %v（停用 %v）", want, loaded, excluded)
	}
	// 坏文件：改名保留，从空表开始
	for _, bad := range []string{"{not json", `{"version":2,"accounts":{}}`} {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, proxyPoolFixedFileName), []byte(bad), 0o600); err != nil {
			t.Fatal(err)
		}
		pool, registry, _ := newFixedPool(t, dir, newProxyPoolTestClock(), nil)
		pool.mu.Lock()
		empty := len(pool.fixed.entries) == 0 && pool.fixed.since.IsZero()
		pool.mu.Unlock()
		renamed := filepath.Join(dir, proxyPoolFixedFileName+".bad-20261023T080000Z")
		kept, err := os.ReadFile(renamed)
		if !empty || err != nil || string(kept) != bad || countProxyPoolLogs(registry, "WARN service IP 池固定分配记录读取失败，已改名保留为 proxy-pool-fixed.json.bad-20261023T080000Z，从头分配 | 错误=") != 1 {
			t.Fatalf("坏文件应改名保留：empty=%v %v %v", empty, err, proxyPoolLogLines(registry))
		}
		if _, err := os.Stat(filepath.Join(dir, proxyPoolFixedFileName)); !os.IsNotExist(err) {
			t.Fatal("坏文件不应被覆盖")
		}
	}
	// 从没开过：不创建；固定操作不改 proxy-pool.json
	clock := newProxyPoolTestClock()
	plain, _, dir := newFixedPool(t, "", clock, nil)
	plain.configure(staticProxyPool(10001, 10003), "", 3)
	owner := newFakeProxyOwner(ids...)
	plain.attach(owner)
	plain.releaseFixed(ids[0], "账户已删除")
	plain.flush()
	if _, err := os.Stat(filepath.Join(dir, proxyPoolFixedFileName)); !os.IsNotExist(err) {
		t.Fatal("从没开过时不应创建固定表文件")
	}
	poolFile := filepath.Join(env.dir, proxyPoolFileName)
	before, _ := os.ReadFile(poolFile)
	if _, err := env.pool.fixedRebalance(nil, false); err != nil {
		t.Fatal(err)
	}
	if _, err := env.pool.fixedRestore(10003); err != nil {
		t.Fatal(err)
	}
	env.pool.flush()
	after, _ := os.ReadFile(poolFile)
	if proxyPoolGoldenSavedAt.ReplaceAllString(string(before), "") != proxyPoolGoldenSavedAt.ReplaceAllString(string(after), "") {
		t.Fatalf("固定操作不应改 proxy-pool.json：\n%s\n---\n%s", before, after)
	}
}

// TestProxyPoolFixedDormantNoEffect 开关关闭时休眠的固定表对分配、绑定、日志、状态与 proxy-pool.json 没有任何影响：带休眠表跑金样场景，
// 结果与金样逐字节相同（只差固定表文件是否存在），休眠表文件不被改写
func TestProxyPoolFixedDormantNoEffect(t *testing.T) {
	if *updateGolden {
		t.Skip("生成金样时不跑")
	}
	want, err := os.ReadFile(filepath.Join("testdata", proxyPoolOffGoldenFile))
	if err != nil {
		t.Fatal(err)
	}
	dormant := fmt.Sprintf(`{"version":1,"saved_at":"2026-10-01T00:00:00Z","since":"2026-10-01T00:00:00Z","port_start":10001,"port_end":10006,`+
		`"accounts":{"a@example.com":{"port":10003,"at":"2026-10-01T00:00:00Z","source":"balance"},"b@example.com":{"port":10003,"at":"2026-10-01T00:00:00Z","source":"balance"},`+
		`"c@example.com":{"port":10006,"at":"2026-10-01T00:00:00Z","source":"affinity"}},"excluded":{"10002":"2026-10-01T00:00:00Z"}}%s`, "\n")
	var seededDir string
	got := runProxyPoolOffScenarioIn(t, func(dir string) {
		seededDir = dir
		if err := os.WriteFile(filepath.Join(dir, proxyPoolFixedFileName), []byte(dormant), 0o600); err != nil {
			t.Fatal(err)
		}
	})
	normalized := strings.ReplaceAll(string(got), `"fixed_file": true`, `"fixed_file": false`)
	if normalized != string(want) {
		t.Fatalf("休眠表改变了关闭时的行为：%s", goldenDiff(want, []byte(normalized)))
	}
	if data, err := os.ReadFile(filepath.Join(seededDir, proxyPoolFixedFileName)); err != nil || string(data) != dormant {
		t.Fatalf("休眠表文件不应被改写：%v\n%s", err, data)
	}
}

// adminAccountsFor 返回只有 ID 的账户列表
func adminAccountsFor(ids []string) []api.AdminAccount {
	accounts := make([]api.AdminAccount, 0, len(ids))
	for _, id := range ids {
		accounts = append(accounts, api.AdminAccount{ID: id})
	}
	return accounts
}

// TestProxyPoolFixedAccountsMarked 账户列表的固定端口（只有端口号；pinned 为 0 时也有）；有自己代理的没有；关闭时没有
func TestProxyPoolFixedAccountsMarked(t *testing.T) {
	const own = "http://203.0.113.50:3128"
	ids := proxyPoolTestIDs(3)
	env := newFixedTestEnv(t, "", fixedStaticPool(10001, 10002), map[string]string{ids[2]: own}, ids...)
	accounts := adminAccountsFor(ids)
	env.pool.markAccounts(accounts)
	for _, account := range accounts {
		want := env.port(account.ID)
		if account.ProxyPoolFixedPort != want || account.ID == ids[2] && want != 0 {
			t.Fatalf("%s 的固定端口 = %d，应为 %d", account.ID, account.ProxyPoolFixedPort, want)
		}
	}
	assertNoPoolSecret(t, "账户列表", mustJSON(t, accounts))
	env.pool.configure(staticProxyPool(10001, 10002), env.global, 8)
	accounts = adminAccountsFor(ids)
	env.pool.markAccounts(accounts)
	for _, account := range accounts {
		if account.ProxyPoolFixedPort != 0 {
			t.Fatal("关闭时不应标固定端口")
		}
	}
}

// TestProxyPoolFixedConcurrency 16 个协程随机申请（Worker、登录、验证、导入）、归还、启动成功与失败、adopt、claim 与 discard、冷却、
// 其他进程锁住与释放、时钟前进与定时器、热更新（改段、开关、PROXY）、重新平均、换 IP、恢复、删除、状态与写盘；每步之后不变量成立，
// 全部归还与 discard 之后预留为 0，最后一次重新平均之后每端口相差不超过 1
func TestProxyPoolFixedConcurrency(t *testing.T) {
	ids := proxyPoolTestIDs(24)
	env := newFixedTestEnv(t, "", fixedStaticPool(10001, 10006), nil, ids...)
	configs := []config.ProxyPool{fixedStaticPool(10001, 10006), fixedStaticPool(10001, 10008), fixedStaticPool(10003, 10008), staticProxyPool(10001, 10006)}
	done := make(chan []*proxyPoolClaim, 16)
	errs := make(chan error, 16)
	for worker := range 16 {
		go func(seed int) {
			random := rand.New(rand.NewSource(int64(seed)))
			var claims []*proxyPoolClaim
			var leases []*proxyPoolLease
			defer func() { done <- claims }()
			for step := range 150 {
				id := ids[random.Intn(len(ids))]
				switch op := random.Intn(20); op {
				case 0, 1, 2:
					lease, _ := env.worker(id)
					leases = append(leases, lease)
				case 3:
					leases = append(leases, env.pool.acquire(id, id, proxyPoolKindVerify, id, env.global))
				case 4:
					key := fmt.Sprintf("login:%d-%d", seed, step)
					lease := env.pool.acquire(key, "", proxyPoolKindImport, "auth", env.global)
					claims = append(claims, lease.releaseClaim())
				case 5:
					key := fmt.Sprintf("login:%d-%d", seed, step)
					leases = append(leases, env.pool.acquire(key, "", proxyPoolKindLogin, "auth", env.global))
				case 6, 7, 8:
					if len(leases) > 0 {
						index := random.Intn(len(leases))
						if random.Intn(3) == 0 {
							leases[index].launchFailed(upstreamFailure, false)
						} else {
							leases[index].release()
						}
						leases = append(leases[:index], leases[index+1:]...)
					}
				case 9:
					if len(claims) > 0 {
						index := random.Intn(len(claims))
						if random.Intn(2) == 0 {
							claims[index].adopt(id)
						} else {
							claims[index].discard()
						}
					}
				case 10:
					port := 10001 + random.Intn(8)
					env.locks.setOther(port, random.Intn(2) == 0)
				case 11:
					env.clock.Advance(time.Duration(random.Intn(20)) * time.Minute)
					env.pool.recheckFired()
				case 12:
					if random.Intn(4) == 0 {
						env.pool.configure(configs[random.Intn(len(configs))], env.global, 8+random.Intn(4))
					}
				case 13:
					_, _ = env.pool.fixedRebalance(nil, random.Intn(2) == 0)
				case 14:
					_, _, _ = env.pool.fixedReassign(10001+random.Intn(8), random.Intn(2) == 0)
				case 15:
					_, _ = env.pool.fixedRestore(10001 + random.Intn(8))
				case 16:
					_ = env.pool.status()
				case 17:
					env.pool.flush()
				case 18:
					env.pool.rateLimited(id, false)
				case 19:
					env.pool.refresh(env.owner, id)
				}
				if err := fixedLogsDrained(env.pool); err != nil {
					errs <- err
					return
				}
			}
			for _, lease := range leases {
				lease.release()
			}
		}(worker)
	}
	var claims []*proxyPoolClaim
	for range 16 {
		claims = append(claims, <-done...)
	}
	select {
	case err := <-errs:
		t.Fatal(err)
	default:
	}
	for _, claim := range claims {
		claim.discard()
	}
	env.locks.mu.Lock()
	clear(env.locks.other)
	env.locks.mu.Unlock()
	env.pool.configure(fixedStaticPool(10001, 10006), env.global, 8)
	env.clock.Advance(3 * time.Hour)
	env.pool.recheckFired()
	env.pool.recheckFired()
	checkFixedInvariants(t, env)
	env.pool.mu.Lock()
	reserved := len(env.pool.fixed.reserved)
	env.pool.mu.Unlock()
	if reserved != 0 {
		t.Fatalf("全部归还与 discard 之后预留应为 0：%d", reserved)
	}
	if _, err := env.pool.fixedRebalance(nil, false); err != nil {
		t.Fatal(err)
	}
	ports := env.pool.fixedPortsForTest()
	counts := fixedCountsOf(env.table())
	low, high := len(ids), 0
	for _, port := range ports {
		low, high = min(low, counts[port]), max(high, counts[port])
	}
	if high-low > 1 {
		t.Fatalf("最后一次重新平均之后每端口应相差不超过 1：%v", counts)
	}
	checkFixedInvariants(t, env)
	assertNoPoolSecretInLogs(t, env.registry)
}

// fixedLogsDrained 断言公共方法返回后待写日志为空
func fixedLogsDrained(pool *proxyPool) error {
	pool.mu.Lock()
	defer pool.mu.Unlock()
	if len(pool.fixed.logs) != 0 {
		return fmt.Errorf("公共方法返回后待写日志没有取走：%v", pool.fixed.logs)
	}
	return nil
}

// fixedPortsForTest 返回参与分配的端口
func (pool *proxyPool) fixedPortsForTest() []int {
	pool.mu.Lock()
	defer pool.mu.Unlock()
	ports := pool.fixedPortsLocked()
	sort.Ints(ports)
	return ports
}
