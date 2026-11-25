package app

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/api"
	"github.com/Mag1cFall/AIStudio2API/internal/config"
)

// IP 池固定分配第 2 轮复查的回归测试：重新平均分配不清理正在新增的账户、端口段撤离只按账户数、后台扩容遇到端口暂停不写失败、
// 端口暂停与恢复和开关切换推送账户列表、“换 IP”预演与执行的关闭数同一口径

// fixedKnownOf 返回调用方在锁外取的账户池快照（与 RebalanceProxyPool 的 fixedKnownAccounts 相同）
func fixedKnownOf(owner *fakeProxyOwner) map[string]struct{} {
	known := make(map[string]struct{})
	for _, id := range owner.AccountIDs() {
		known[id] = struct{}{}
	}
	return known
}

// TestProxyPoolFixedRebalanceKeepsAccountBeingAdded 新账户已固定（登录 adopt、导入 adopt、目录扫描 prime）、还没进账户池时管理员
// 重新平均分配：不把它当成已不在账户池的记录清理（预演与执行都是 pruned=0），进账户池后仍是登录或导入用的端口，不惰性改分；
// 真正不在账户池的记录（目录移走后移出账户池的）照常清理；“正在新增”的记号过期后（注册失败、没有回滚）不再挡住清理
func TestProxyPoolFixedRebalanceKeepsAccountBeingAdded(t *testing.T) {
	ids := proxyPoolTestIDs(8)
	env := newFixedTestEnv(t, "", fixedStaticPool(10001, 10003), nil, ids...)
	env.pool.configure(fixedStaticPool(10001, 10004), env.global, 8)
	for _, victim := range env.usersOf(10002)[:2] {
		env.owner.remove(victim)
		env.pool.refresh(env.owner, victim)
		env.pool.releaseFixed(victim, "账户已删除")
	}
	const extra = "extra@example.com"
	env.owner.add(extra, "")
	env.pool.prime(&aistudio.Account{ID: extra})
	env.pool.refresh(env.owner, extra)

	// 1. 浏览器登录：adopt（固定到登录用的端口）之后、AccountPool.Add 之前
	login := env.pool.acquire("login:new", "", proxyPoolKindLogin, "auth", env.global)
	loginPort := leasePort(t, login)
	const loggedIn = "fresh@example.com"
	login.adopt(loggedIn)
	// 2. Chrome 导入：claim adopt 之后、workers.Add（prime）之前，登录租约已归还
	imp := env.pool.acquire("login:imp", "", proxyPoolKindImport, "auth", env.global)
	importPort := leasePort(t, imp)
	claim := imp.releaseClaim()
	const imported = "imported@example.com"
	claim.adopt(imported)
	// 3. 目录扫描：workers.Add（prime 惰性分配）之后、AccountPool.Add 之前
	const scanned = "scanned@example.com"
	env.pool.prime(&aistudio.Account{ID: scanned})
	scannedPort := env.port(scanned)
	if loginPort == 0 || importPort == 0 || scannedPort == 0 {
		t.Fatalf("三个新账户都应已固定：登录 %d 导入 %d 扫描 %d", loginPort, importPort, scannedPort)
	}
	// 真正的 stray：在账户池里出现过，目录移走后移出账户池（没有全量同步）
	detached := env.usersOf(10001)[0]
	env.owner.remove(detached)

	if status := env.pool.status(); status.Fixed == nil || status.Fixed.Stray != 1 {
		t.Fatalf("状态里已不在账户池的记录应只有 %s：%+v", detached, status.Fixed)
	}
	known := fixedKnownOf(env.owner)
	dry, err := env.pool.fixedRebalance(known, true)
	if err != nil || dry.Pruned != 1 {
		t.Fatalf("预演只应清理移出账户池的 %s：%+v %v", detached, dry, err)
	}
	result, err := env.pool.fixedRebalance(known, false)
	if err != nil || result.Pruned != 1 || result.Pruned != dry.Pruned {
		t.Fatalf("执行应与预演一致、只清理 1 条：%+v %v", result, err)
	}
	if env.port(detached) != 0 {
		t.Fatalf("移出账户池的 %s 应被清理", detached)
	}
	for _, id := range []string{loggedIn, imported, scanned} {
		if env.port(id) == 0 {
			t.Fatalf("正在新增的 %s 被清理了", id)
		}
	}
	want := map[string]int{loggedIn: env.port(loggedIn), imported: env.port(imported), scanned: env.port(scanned)}
	// 进账户池：refresh 推送，登录租约随后归还
	for _, id := range []string{loggedIn, imported, scanned} {
		env.owner.add(id, "")
		if id == imported || id == scanned {
			env.pool.prime(&aistudio.Account{ID: id})
		}
		env.pool.refresh(env.owner, id)
	}
	login.release()
	for id, port := range want {
		if got := env.port(id); got != port {
			t.Fatalf("%s 进账户池后换了端口：%d → %d", id, port, got)
		}
		if binding := env.owner.binding(id); binding == nil || binding.URL != staticPortURL(t, port) {
			t.Fatalf("%s 的出口应是固定端口 %d：%s", id, port, fixedBindingText(binding))
		}
	}
	env.pool.mu.Lock()
	pending := len(env.pool.fixed.adding)
	env.pool.mu.Unlock()
	if pending != 0 {
		t.Fatalf("进账户池后“正在新增”的记号应去掉：%d", pending)
	}
	checkFixedInvariants(t, env)

	// 注册失败（workers.Add 之后 AccountPool.Add 失败、目录扫描不回滚）：记号过期前不清理，过期后清理
	const failed = "failed@example.com"
	env.pool.prime(&aistudio.Account{ID: failed})
	if result, _ := env.pool.fixedRebalance(fixedKnownOf(env.owner), true); result.Pruned != 0 {
		t.Fatalf("记号有效期内不应清理：%+v", result)
	}
	env.clock.Advance(proxyPoolFixedAddingTTL)
	if result, _ := env.pool.fixedRebalance(fixedKnownOf(env.owner), false); result.Pruned != 1 || env.port(failed) != 0 {
		t.Fatalf("记号过期后应清理注册失败的 %s：%+v", failed, result)
	}
	// 新增回滚（releaseFixed）去掉记号
	env.pool.prime(&aistudio.Account{ID: "rolled@example.com"})
	env.pool.releaseFixed("rolled@example.com", "新增失败")
	env.pool.mu.Lock()
	_, still := env.pool.fixed.adding["rolled@example.com"]
	env.pool.mu.Unlock()
	if still {
		t.Fatal("新增回滚后“正在新增”的记号应去掉")
	}
}

// TestProxyPoolFixedRebalanceLiveSnapshot 调用方锁外取快照之后才加入账户池的账户（记号已被同步去掉）：锁内再取一次账户池，不清理
func TestProxyPoolFixedRebalanceLiveSnapshot(t *testing.T) {
	ids := proxyPoolTestIDs(4)
	env := newFixedTestEnv(t, "", fixedStaticPool(10001, 10002), nil, ids...)
	known := fixedKnownOf(env.owner)
	const late = "late@example.com"
	env.owner.add(late, "")
	env.pool.prime(&aistudio.Account{ID: late})
	env.pool.refresh(env.owner, late)
	port := env.port(late)
	result, err := env.pool.fixedRebalance(known, false)
	if err != nil || result.Pruned != 0 || env.port(late) != port || port == 0 {
		t.Fatalf("快照之后加入账户池的 %s 不应清理：%+v %v（端口 %d → %d）", late, result, err, port, env.port(late))
	}
}

// TestProxyPoolFixedEvacuateIgnoresReservations 端口段变化撤离段外账户只按固定账户数选端口：同样的固定表与端口段，有没有进行中的登录
// 预留，撤离结果相同；登录失败归还后分布仍均匀
func TestProxyPoolFixedEvacuateIgnoresReservations(t *testing.T) {
	run := func(withLogin bool) map[string]int {
		env := newFixedTestEnv(t, "", fixedStaticPool(10001, 10003), nil, proxyPoolTestIDs(9)...)
		env.pool.configure(fixedStaticPool(10001, 10004), env.global, 8)
		var lease *proxyPoolLease
		if withLogin {
			lease = env.pool.acquire("login:x", "", proxyPoolKindLogin, "auth", env.global)
			if got := leasePort(t, lease); got != 10004 {
				t.Fatalf("登录应预留在最少的 10004：%d", got)
			}
		}
		env.pool.configure(fixedStaticPool(10002, 10004), env.global, 8)
		// 登录失败：预留撤销
		lease.release()
		checkFixedInvariants(t, env)
		return env.table()
	}
	without, with := run(false), run(true)
	if fmt.Sprint(without) != fmt.Sprint(with) {
		t.Fatalf("撤离结果随进行中的登录预留变化：无=%v 有=%v", fixedCountsOf(without), fixedCountsOf(with))
	}
	for port, count := range fixedCountsOf(with) {
		if count != 3 {
			t.Fatalf("撤离后应 3/3/3：端口 %d 有 %d 个（%v）", port, count, fixedCountsOf(with))
		}
	}
}

// TestFixedExpandPausedNotFailure 账户被选来按需扩容之后、启动之前固定端口冷却：后台扩容按跳过处理，不写“后台扩容失败”WARN
func TestFixedExpandPausedNotFailure(t *testing.T) {
	env := newPoolWorkerEnv(t, staticProxyPool(10001, 10002), 3, nil, "a@example.com", "b@example.com")
	env.ipPool.configure(fixedStaticPool(10001, 10002), proxyPoolTestGlobal, 3)
	coolFixedPort(t, env.ipPool, "a@example.com")
	select {
	case <-env.manager.expandInBackground("a@example.com", testRuntimeModel):
	case <-time.After(10 * time.Second):
		t.Fatal("后台扩容没有结束")
	}
	if n := countProxyPoolLogs(env.requests, "后台扩容失败"); n != 0 {
		t.Fatalf("后台扩容把固定端口暂停记为失败：%v", proxyPoolLogLines(env.requests))
	}
	if env.launches.last("a@example.com") != "" {
		t.Fatal("暂停的账户不应启动浏览器")
	}
}

// TestFixedExtraExpandPausedNotFailure 补充池成员被选来按需扩容之后固定端口暂停（成员判断之后、启动之前）：补充池后台扩容同样按跳过处理，
// 不写失败 WARN
func TestFixedExtraExpandPausedNotFailure(t *testing.T) {
	ids := extraIDsOf("pause", 3)
	env := newExtraEnv(t, slices.Clone(ids), nil, 1, 1, 0, 1, 1, 2)
	env.member(t, aistudio.BenefitTierFree, ids...)
	ipPool, _ := newTestProxyPool(t, newProxyPoolTestClock(), "", "")
	env.manager.proxyPool = ipPool
	ipPool.configure(fixedStaticPool(10001, 10002), proxyPoolTestGlobal, 3)
	ipPool.attach(env.pool)
	env.pass(t)
	extra := env.in(aistudio.PoolScopeExtra)
	if len(extra) != 1 {
		t.Fatalf("补充池预热应启动 1 个：%v", extra)
	}
	cold := ""
	for _, accountID := range ids {
		if !slices.Contains(extra, accountID) {
			cold = accountID
			break
		}
	}
	port := fixedPoolPort(ipPool, cold)
	if port == 0 {
		t.Fatalf("%s 应有固定端口", cold)
	}
	// 选号与成员判断之后、启动之前端口被其他进程锁住（账户池还没收到暂停的推送，账户仍是成员）：启动在预检被拒
	ipPool.mu.Lock()
	ipPool.lockedElsewhere[port] = ipPool.now().Add(time.Hour)
	ipPool.mu.Unlock()
	select {
	case <-env.manager.extraExpandInBackground(cold, testRuntimeModel):
	case <-time.After(10 * time.Second):
		t.Fatal("补充池后台扩容没有结束")
	}
	if n := countLogs(env.requests, "后台扩容失败"); n != 0 {
		t.Fatalf("补充池后台扩容把固定端口暂停记为失败：%v", serviceLogMessages(env.requests))
	}
	if slices.Contains(env.in(aistudio.PoolScopeExtra), cold) {
		t.Fatal("暂停的账户不应启动")
	}
}

// TestProxyPoolFixedNotifiesAccounts 端口暂停与恢复、热开启与关闭、换代理商之后推送账户列表（账户页的“IP 暂停”与固定端口不用等下一次请求）；
// 没有可见变化时不推送；固定分配关闭时任何操作都不推送
func TestProxyPoolFixedNotifiesAccounts(t *testing.T) {
	ids := proxyPoolTestIDs(4)
	env := newFixedTestEnv(t, "", fixedStaticPool(10001, 10002), nil, ids...)
	var calls atomic.Int32
	env.pool.setAccountsChangedHook(func() { calls.Add(1) })
	expect := func(step string, want int32) {
		t.Helper()
		if got := calls.Swap(0); got != want {
			t.Fatalf("%s：推送 %d 次，应为 %d", step, got, want)
		}
	}
	lease, _ := env.worker(ids[0])
	lease.release()
	expect("普通的申请与归还", 0)
	port := env.port(ids[0])
	env.coolUpstream(ids[0])
	expect("端口暂停", 1)
	env.pool.recheckFired()
	expect("暂停期间的检查", 0)
	env.pool.mu.Lock()
	until := env.pool.ports[port].coolUntil
	env.pool.mu.Unlock()
	env.clock.Advance(until.Sub(env.clock.Now()) + time.Second)
	env.pool.recheckFired()
	expect("冷却到期恢复", 1)
	// 被其他进程占用：暂停与释放后恢复
	env.locks.setOther(port, true)
	blocked := env.pool.acquire(ids[0], ids[0], proxyPoolKindWorker, ids[0], env.global)
	if blocked.Refused() == nil {
		t.Fatal("端口被其他进程锁住时应拒绝")
	}
	expect("被其他进程占用", 1)
	env.locks.setOther(port, false)
	env.clock.Advance(proxyPoolLockedRetry + time.Second)
	env.pool.recheckFired()
	expect("其他进程释放", 1)
	// 热关闭、热开启
	env.pool.configure(staticProxyPool(10001, 10002), env.global, 8)
	expect("热关闭", 1)
	env.pool.configure(fixedStaticPool(10001, 10002), env.global, 8)
	expect("热开启", 1)
	env.pool.configure(fixedStaticPool(10001, 10002), env.global, 9)
	expect("只改容量", 0)
	// 换代理商：暂停记录清掉
	other := fixedStaticPool(10001, 10002)
	other.URL = "http://user:pass@proxy.example.com:10000"
	env.pool.configure(other, env.global, 9)
	expect("换代理商", 1)
	assertNoPoolSecretInLogs(t, env.registry)

	// 关闭时：同样的操作一次也不推送
	off := newFixedTestEnv(t, "", staticProxyPool(10001, 10002), nil, ids...)
	off.pool.setAccountsChangedHook(func() { calls.Add(1) })
	off.coolUpstream(ids[0])
	off.clock.Advance(time.Hour)
	off.pool.recheckFired()
	off.pool.configure(staticProxyPool(10001, 10003), off.global, 8)
	off.pool.configure(dynamicProxyPool(), off.global, 8)
	expect("固定分配关闭", 0)
}

// TestProxyPoolFixedReassignClosingCount “换 IP”的 closing 为此刻用这个端口服务、会登记后台关闭的 Worker 数（含过渡 Worker），预演与
// 执行同一口径；端口上没有 Worker 在服务时为 0（被挪账户的浏览器在别的端口上，不关闭）
func TestProxyPoolFixedReassignClosingCount(t *testing.T) {
	ids := []string{"a@example.com", "b@example.com", "c@example.com", "d@example.com"}
	env := newFixedTestEnv(t, "", fixedStaticPool(10001, 10003), nil, ids...)
	now := env.clock.Now()
	env.pool.mu.Lock()
	for _, id := range ids[:3] {
		env.pool.fixedSetLocked(id, 10001, now, fixedSourceBalance)
	}
	env.pool.fixedSetLocked(ids[3], 10002, now, fixedSourceBalance)
	env.pool.pushLocked(ids...)
	env.pool.mu.Unlock()
	for _, id := range ids[:3] {
		env.worker(id)
	}
	if result, err := env.pool.fixedRebalance(nil, false); err != nil || result.Moved == 0 {
		t.Fatalf("重新平均 = %+v (%v)", result, err)
	}
	transitional := ""
	for _, id := range ids[:3] {
		if env.port(id) != 10001 {
			transitional = id
			break
		}
	}
	if transitional == "" {
		t.Fatal("应有被挪走、Worker 仍在 10001 的过渡账户")
	}
	movedPort := env.port(transitional)
	preview, serving, err := env.pool.fixedReassign(movedPort, true)
	if err != nil || preview.Closing != len(serving) || preview.Closing != 0 {
		t.Fatalf("端口 %d 上没有 Worker 在服务：closing 应为 0（%+v，serving=%v）", movedPort, preview, serving)
	}
	if preview.Running == 0 {
		t.Fatalf("被挪账户 %s 的浏览器在 10001 上运行，running 应计入：%+v", transitional, preview)
	}
	preview, serving, err = env.pool.fixedReassign(10001, true)
	if err != nil || preview.Closing != 3 || len(serving) != 3 {
		t.Fatalf("10001 上 3 个 Worker 在服务（含过渡的 %s）：%+v serving=%v", transitional, preview, serving)
	}
	result, serving, err := env.pool.fixedReassign(10001, false)
	if err != nil || result.Closing != preview.Closing || result.Closing != len(serving) {
		t.Fatalf("执行的 closing 应与预演一致：预演 %+v 执行 %+v serving=%v", preview, result, serving)
	}
}

// TestProxyPoolFixedNotifySentinel 待推送时取走的固定分配日志末尾带一个推送回调：emit 在锁外调用它、不写成日志，只取一次；没有登记回调时
// 不带（测试与关闭时的日志不变）
func TestProxyPoolFixedNotifySentinel(t *testing.T) {
	pool, registry := newTestProxyPool(t, newProxyPoolTestClock(), "", "")
	pool.mu.Lock()
	pool.fixed.notify = true
	none := pool.takeFixedLogsLocked()
	pool.mu.Unlock()
	if len(none) != 0 {
		t.Fatalf("没有登记回调时不应带推送：%+v", none)
	}
	var published atomic.Int32
	pool.setAccountsChangedHook(func() {
		// 在锁外调用：回调里能再取 IP 池的锁
		pool.mu.Lock()
		published.Add(1)
		pool.mu.Unlock()
	})
	pool.mu.Lock()
	pool.fixed.notify = true
	logs := pool.takeFixedLogsLocked()
	again := pool.takeFixedLogsLocked()
	pool.mu.Unlock()
	if len(logs) != 1 || logs[0].notify == nil || logs[0].message != "" || len(again) != 0 {
		t.Fatalf("待推送时应带一个推送回调、只取一次：%+v %+v", logs, again)
	}
	pool.emit(logs)
	if published.Load() != 1 || len(proxyPoolLogLines(registry)) != 0 {
		t.Fatalf("emit 应调用推送回调且不写日志：%d %v", published.Load(), proxyPoolLogLines(registry))
	}
}

// TestNewRuntimeWiresFixedAccountsHook 生成服务装配时把账户列表的合并推送接到 IP 池：回调触发后管理页收到 accounts 事件，账户带固定端口
func TestNewRuntimeWiresFixedAccountsHook(t *testing.T) {
	isolateDisableNotes(t)
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	root := filepath.Join(t.TempDir(), "auth")
	ids := []string{"a@example.com", "b@example.com", "c@example.com"}
	for _, id := range ids {
		writeEnableTestAccount(t, root, id, true)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	requests := newRequestRegistry(ctx)
	cfg := config.Default()
	cfg.AuthStates = root
	cfg.WAABackend = config.WAABackendGo
	cfg.ProxyPool = fixedStaticPool(10001, 10002)
	ipPool := newProxyPool(proxyPoolOptions{requests: requests, relayLimit: func() int { return 1 << 20 }})
	lifecycle, stop := context.WithCancel(ctx)
	_, admin, closeRuntime, err := newRuntime(ctx, lifecycle, cfg, requests, ipPool)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.pool.WaitBackground)
	defer func() {
		stop()
		_ = closeRuntime()
	}()
	ipPool.mu.Lock()
	hook := ipPool.onAccountsChanged
	ipPool.mu.Unlock()
	if hook == nil {
		t.Fatal("装配后 IP 池应登记账户列表推送的回调")
	}
	events := requests.activateSubscriber(requests.subscribe(ctx), nil, nil)
	hook()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case event := <-events:
			if event.Type != "accounts" {
				continue
			}
			data, _ := event.Data.(map[string]any)
			accounts, _ := data["accounts"].([]api.AdminAccount)
			for _, account := range accounts {
				if account.ID == ids[0] && account.ProxyPoolFixedPort != 0 {
					return
				}
			}
		case <-deadline:
			t.Fatal("回调触发后管理页没有收到带固定端口的账户列表")
		}
	}
}
