package app

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/api"
)

// IP 池固定分配第 1 轮复查的回归测试：换 IP 关闭端口上的全部 Worker、登录或导入期间换 IP、删除后才到的推送、没有登记的暂停说明、
// 没有固定账户的端口被其他进程锁过、动态固定会话的会话记录、新账户自动处理遇到端口暂停

// TestProxyPoolFixedReassignClosesTransitional 重新平均分配挪走了还开着浏览器的账户（过渡 Worker 仍在旧端口），之后对旧端口换 IP：
// 登记关闭的 Worker 包括这个过渡 Worker；它的验证不再跟随过渡 Worker 走已停用的端口，改用固定端口；Worker 关闭后 Go 侧出口切到固定端口
func TestProxyPoolFixedReassignClosesTransitional(t *testing.T) {
	ids := []string{"a@example.com", "b@example.com", "c@example.com", "d@example.com"}
	env := newFixedTestEnv(t, "", fixedStaticPool(10001, 10002), nil, ids...)
	now := env.clock.Now()
	env.pool.mu.Lock()
	for _, id := range ids[:3] {
		env.pool.fixedSetLocked(id, 10001, now, fixedSourceBalance)
	}
	env.pool.fixedSetLocked(ids[3], 10002, now, fixedSourceBalance)
	env.pool.pushLocked(ids...)
	env.pool.mu.Unlock()
	closers := make(map[string]*fakeProxyCloser)
	for _, id := range ids[:3] {
		lease, closer := env.worker(id)
		if got := leasePort(t, lease); got != 10001 {
			t.Fatalf("%s 的 Worker 端口 %d", id, got)
		}
		closers[id] = closer
	}
	if result, err := env.pool.fixedRebalance(nil, false); err != nil || result.Moved != 1 {
		t.Fatalf("重新平均 = %+v (%v)", result, err)
	}
	moved := ""
	for _, id := range ids[:3] {
		if env.port(id) == 10002 {
			moved = id
		}
	}
	if moved == "" {
		t.Fatal("应有一个账户被挪到 10002")
	}
	_, serving, err := env.pool.fixedReassign(10001, false)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(serving, ",") != strings.Join(ids[:3], ",") {
		t.Fatalf("应登记关闭 10001 上全部在服务的 Worker（含过渡的 %s）：%v", moved, serving)
	}
	// 后台关闭之前：验证不跟随已停用端口上的过渡 Worker，用固定端口；同端口仍不暂停时照常分配
	verify := env.pool.acquire(moved, moved, proxyPoolKindVerify, moved, env.global)
	if got := leasePort(t, verify); got != 10002 {
		t.Fatalf("%s 的验证应用固定端口 10002，而不是已停用的 10001：%d", moved, got)
	}
	if env.count("来源=跟随运行中的 Worker") != 0 {
		t.Fatalf("不应跟随已停用端口上的 Worker：%v", env.newLogs())
	}
	verify.release()
	// 过渡 Worker 关闭（后台关闭完成）：Go 侧出口切到固定端口
	closers[moved].close()
	if binding := env.owner.binding(moved); binding == nil || binding.Slot != "10002" || binding.Active {
		t.Fatalf("Worker 关闭后应切到固定端口 10002：%s", fixedBindingText(binding))
	}
	for _, id := range ids[:3] {
		closers[id].close()
	}
	env.pool.refresh(env.owner, moved)
	checkFixedInvariants(t, env)
}

// TestFixedReassignRestartsTransitionalWorker 接入层：换 IP 时端口上的过渡 Worker（固定端口已在别处）同样登记后台关闭，计入重启数
func TestFixedReassignRestartsTransitionalWorker(t *testing.T) {
	env := newPoolWorkerEnv(t, staticProxyPool(10001, 10003), 3, nil, "x@example.com", "y@example.com", "z@example.com")
	env.ipPool.configure(fixedStaticPool(10001, 10003), proxyPoolTestGlobal, 3)
	if err := env.promote(t, "x@example.com"); err != nil {
		t.Fatal(err)
	}
	old := fixedPoolPort(env.ipPool, "x@example.com")
	// 固定端口改到别处、浏览器还没关闭（重新平均分配之后）
	target := 10001
	if old == target {
		target = 10002
	}
	env.ipPool.mu.Lock()
	env.ipPool.fixedSetLocked("x@example.com", target, env.ipPool.now(), fixedSourceRebalance)
	env.ipPool.mu.Unlock()
	_, serving, err := env.ipPool.fixedReassign(old, false)
	if err != nil || len(serving) != 1 || serving[0] != "x@example.com" {
		t.Fatalf("换 IP 应登记过渡 Worker：%v (%v)", serving, err)
	}
	if restarted := env.manager.resetFixedWorkers(serving, old); restarted != 1 {
		t.Fatalf("应登记 1 个后台关闭：%d", restarted)
	}
	deadline := time.Now().Add(10 * time.Second)
	for env.manager.hasWorker("x@example.com") {
		if time.Now().After(deadline) {
			t.Fatal("已停用端口上的过渡 Worker 应在后台关闭")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := env.promote(t, "x@example.com"); err != nil {
		t.Fatal(err)
	}
	if launched := env.launches.last("x@example.com"); launched != staticPortURL(t, target) {
		t.Fatalf("下次启动应用固定端口 %d", target)
	}
}

// TestProxyPoolFixedAdoptAfterReassign 新账户登录或 Chrome 导入进行中，管理员对它预留的端口换 IP：adopt 不把新账户固定到已停用的端口，
// 改分到账户最少的端口并写明原因；预留归零
func TestProxyPoolFixedAdoptAfterReassign(t *testing.T) {
	ids := proxyPoolTestIDs(4)
	env := newFixedTestEnv(t, "", fixedStaticPool(10001, 10004), nil, ids...)
	excluded := func(port int) bool {
		env.pool.mu.Lock()
		defer env.pool.mu.Unlock()
		return env.pool.fixedExcludedLocked(port)
	}
	login := env.pool.acquire("login:x", "", proxyPoolKindLogin, "auth", env.global)
	loginPort := leasePort(t, login)
	if _, _, err := env.pool.fixedReassign(loginPort, false); err != nil {
		t.Fatal(err)
	}
	login.adopt("new@example.com")
	login.release()
	if got := env.port("new@example.com"); got == 0 || excluded(got) {
		t.Fatalf("登录的新账户不应固定到已停用的端口：%d（登录用 %d）", got, loginPort)
	}
	if env.count(fmt.Sprintf("IP 池固定分配 | 端口=%d | 来源=登录 | 该端口账户=", env.port("new@example.com"))) != 1 ||
		env.count(fmt.Sprintf("登录用的端口 %d 已换 IP 停用，改分到账户最少的端口", loginPort)) != 1 {
		t.Fatalf("应写明改分的原因：%v", env.newLogs())
	}
	importLease := env.pool.acquire("login:y", "", proxyPoolKindImport, "auth", env.global)
	importPort := leasePort(t, importLease)
	claim := importLease.releaseClaim()
	if claim == nil {
		t.Fatal("导入应留下出口记录")
	}
	if _, _, err := env.pool.fixedReassign(importPort, false); err != nil {
		t.Fatal(err)
	}
	claim.adopt("imp@example.com")
	if got := env.port("imp@example.com"); got == 0 || excluded(got) {
		t.Fatalf("导入的新账户不应固定到已停用的端口：%d（导入用 %d）", got, importPort)
	}
	if env.count(fmt.Sprintf("导入用的端口 %d 已换 IP 停用，改分到账户最少的端口", importPort)) != 1 {
		t.Fatalf("应写明改分的原因：%v", env.newLogs())
	}
	env.pool.mu.Lock()
	reserved := len(env.pool.fixed.reserved)
	source := env.pool.fixed.entries["imp@example.com"].source
	env.pool.mu.Unlock()
	if reserved != 0 || source != fixedSourceImport {
		t.Fatalf("预留 %d、来源 %s", reserved, source)
	}
	env.owner.add("new@example.com", "")
	env.owner.add("imp@example.com", "")
	env.pool.attach(env.owner)
	checkFixedInvariants(t, env)
}

// TestProxyPoolFixedDeletedNotResurrected 删除账户（releaseFixed）之后才归还的租约（就地重导入期间删除）推送它时不把它写回固定表，
// 状态里没有残留；同一账户重新加入时按新增分到最少的端口
func TestProxyPoolFixedDeletedNotResurrected(t *testing.T) {
	ids := proxyPoolTestIDs(8)
	env := newFixedTestEnv(t, "", fixedStaticPool(10001, 10004), nil, ids...)
	victim := ids[0]
	lease := env.pool.acquire(victim, victim, proxyPoolKindImport, victim, env.global)
	if lease.Refused() != nil {
		t.Fatal(lease.Refused())
	}
	env.owner.remove(victim)
	env.pool.releaseFixed(victim, "账户已删除")
	lease.release()
	// 删除之后才到的申请同样不写表
	late := env.pool.acquire(victim, victim, proxyPoolKindVerify, victim, env.global)
	late.release()
	if port, ok := env.table()[victim]; ok {
		t.Fatalf("已删除的账户被写回固定表（端口 %d）：%v", port, env.newLogs())
	}
	if env.count("IP 池固定分配 | 端口=") != 0 {
		t.Fatalf("不应为已删除的账户写分配日志：%v", env.newLogs())
	}
	status := env.pool.status()
	if status.Fixed.Stray != 0 || status.Fixed.Accounts != len(ids)-1 {
		t.Fatalf("状态 stray=%d accounts=%d", status.Fixed.Stray, status.Fixed.Accounts)
	}
	// 重新加入（目录扫描载入）：prime 重新登记，分到最少的端口
	env.owner.add(victim, "")
	env.pool.refresh(env.owner, victim)
	if env.port(victim) == 0 {
		t.Fatal("重新加入的账户应分到端口")
	}
	checkFixedInvariants(t, env)
}

// TestFixedCreateAccountRollbackReleased 管理页新账户登录在 adopt 之后、进账户池之前失败（新增回滚）：随后归还的登录租约不把回滚的
// 账户写回固定表
func TestFixedCreateAccountRollbackReleased(t *testing.T) {
	admin, ipPool, _, _ := fixedAdminEnv(t, fixedStaticPool(10001, 10004), "a@example.com")
	// 公共头提供者已关闭：headers.Add 失败、走回滚
	admin.headers.Close()
	if _, err := admin.CreateAccount(context.Background(), api.AccountCreateInput{}); err == nil {
		t.Fatal("新增应失败")
	}
	if _, ok := admin.pool.StatusOf("new@example.com"); ok {
		t.Fatal("回滚的账户不应在号池")
	}
	if port := fixedPoolPort(ipPool, "new@example.com"); port != 0 {
		t.Fatalf("回滚的账户被写回固定表（端口 %d）：%v", port, proxyPoolLogLines(ipPool.requests))
	}
	if countSourceLogs(ipPool.requests, "new@example.com", "IP 池固定分配 | 端口=", "来源=新增") != 0 {
		t.Fatalf("不应有惰性分配：%v", proxyPoolLogLines(ipPool.requests))
	}
}

// TestProxyPoolFixedStaleHoldCleared 导入的新账户被分到“被其他进程占用”、还没登记暂停（端口上原先没有固定账户）的端口：同一次 wake
// 里检测确认其他进程已释放时，绑定上的暂停说明随之清除，账户不会一直暂停
func TestProxyPoolFixedStaleHoldCleared(t *testing.T) {
	ids := proxyPoolTestIDs(8)
	env := newFixedTestEnv(t, "", fixedStaticPool(10001, 10004), nil, ids...)
	env.pool.configure(fixedStaticPool(10001, 10005), env.global, 8)
	// 另一个端口长期暂停（定时器保持安排）
	env.locks.setOther(10001, true)
	if lease, _ := env.worker(env.usersOf(10001)[0]); lease.Refused() == nil {
		t.Fatal("10001 应被拒")
	}
	importLease := env.pool.acquire("login:imp", "", proxyPoolKindImport, "auth", env.global)
	if got := leasePort(t, importLease); got != 10005 {
		t.Fatalf("导入端口 %d", got)
	}
	claim := importLease.releaseClaim()
	// 其他进程锁住 10005；另一次新账户登录试 10005 锁不到（10005 上还没有固定账户，不登记暂停）
	env.locks.setOther(10005, true)
	login := env.pool.acquire("login:other", "", proxyPoolKindLogin, "auth", env.global)
	if got := leasePort(t, login); got == 10005 {
		t.Fatal("10005 被占用时不应分到它")
	}
	login.release()
	env.clock.Advance(proxyPoolLockedRetry + time.Second)
	env.locks.setOther(10005, false)
	claim.adopt("imp@example.com")
	env.owner.add("imp@example.com", "")
	env.pool.refresh(env.owner, "imp@example.com")
	if hold := env.owner.binding("imp@example.com").Hold; hold != "" {
		t.Fatalf("10005 已恢复，暂停说明应清除：%q", hold)
	}
	env.clock.Advance(30 * time.Minute)
	env.pool.recheckFired()
	if hold := env.owner.binding("imp@example.com").Hold; hold != "" {
		t.Fatalf("暂停说明不应留着：%q", hold)
	}
	// 10001 仍被占用：检测顺延重试时间，暂停记录的下次检查随之顺延（不停留在已过去的时刻）
	env.pool.mu.Lock()
	nextCheck, retry := env.pool.fixed.nextCheck, env.pool.lockedElsewhere[10001]
	env.pool.mu.Unlock()
	if !nextCheck.Equal(retry) || !retry.After(env.clock.Now()) {
		t.Fatalf("下次检查 %s，重试时间 %s", nextCheck, retry)
	}
	env.locks.setOther(10001, false)
	env.pool.recheckFired()
	checkFixedInvariants(t, env)
}

// TestProxyPoolFixedRepushStaleHold 端口没有暂停记录、绑定却带着暂停说明时，端口的暂停重算（fixedPortChangedLocked）重推这些账户
func TestProxyPoolFixedRepushStaleHold(t *testing.T) {
	ids := proxyPoolTestIDs(4)
	env := newFixedTestEnv(t, "", fixedStaticPool(10001, 10002), nil, ids...)
	victim := env.usersOf(10001)[0]
	env.pool.mu.Lock()
	// 模拟先推送了暂停、没有登记：端口锁记录写入后推送绑定，再删掉记录（held 一直为空）
	env.pool.lockedElsewhere[10001] = env.clock.Now().Add(time.Minute)
	env.pool.pushLocked(victim)
	delete(env.pool.lockedElsewhere, 10001)
	_, held := env.pool.fixed.held[10001]
	env.pool.mu.Unlock()
	if held || env.owner.binding(victim).Hold == "" {
		t.Fatal("前提：绑定带暂停说明、没有暂停记录")
	}
	env.pool.mu.Lock()
	env.pool.fixedPortChangedLocked(10001, env.clock.Now())
	logs := env.pool.takeFixedLogsLocked()
	env.pool.mu.Unlock()
	env.pool.emit(logs)
	if hold := env.owner.binding(victim).Hold; hold != "" {
		t.Fatalf("暂停说明应清除：%q", hold)
	}
	checkFixedInvariants(t, env)
}

// TestProxyPoolFixedLockedEmptyPortRechecked 没有固定账户的端口被其他进程锁过（新账户登录试它时锁不到）：照常按重试时间检测，
// 对方释放后新账户登录与新增账户都能用上它（不会一直被避开）
func TestProxyPoolFixedLockedEmptyPortRechecked(t *testing.T) {
	ids := proxyPoolTestIDs(3)
	env := newFixedTestEnv(t, "", fixedStaticPool(10001, 10004), nil, ids...)
	if len(env.usersOf(10004)) != 0 {
		t.Fatalf("前提：10004 是空端口：%v", env.table())
	}
	env.locks.setOther(10004, true)
	login := env.pool.acquire("login:a", "", proxyPoolKindLogin, "auth", env.global)
	if got := leasePort(t, login); got == 10004 {
		t.Fatal("被占用时不应分到 10004")
	}
	login.release()
	env.pool.mu.Lock()
	armed := !env.pool.recheckAt.IsZero()
	env.pool.mu.Unlock()
	if !armed {
		t.Fatal("空端口被其他进程占用时也应安排按重试时间检测")
	}
	env.locks.setOther(10004, false)
	env.clock.Advance(proxyPoolLockedRetry + time.Second)
	env.pool.recheckFired()
	env.pool.mu.Lock()
	_, locked := env.pool.lockedElsewhere[10004]
	env.pool.mu.Unlock()
	if locked {
		t.Fatal("对方释放后检测应清掉记录")
	}
	if preview := env.pool.fixedLoginPreview(t); preview != 10004 {
		t.Fatalf("新账户登录此刻应分到最少的 10004：%d", preview)
	}
	login = env.pool.acquire("login:b", "", proxyPoolKindLogin, "auth", env.global)
	if got := leasePort(t, login); got != 10004 {
		t.Fatalf("新账户登录应分到 10004：%d", got)
	}
	login.release()
	// 新增账户惰性分配同样分到它
	env.owner.add("new@example.com", "")
	env.pool.refresh(env.owner, "new@example.com")
	if got := env.port("new@example.com"); got != 10004 {
		t.Fatalf("新增账户应分到最少的 10004：%d（%v）", got, env.table())
	}
	assertFixedTableBalanced(t, env.table(), []int{10001, 10002, 10003, 10004})
	checkFixedInvariants(t, env)
}

// TestProxyPoolFixedDynamicSessionKept 动态固定会话：账户离开账户池超过 30 天再回来，会话号不变（不修剪）；删除账户时释放会话记录
func TestProxyPoolFixedDynamicSessionKept(t *testing.T) {
	ids := []string{"a@example.com", "b@example.com"}
	cfg := dynamicProxyPool()
	env := newFixedTestEnv(t, "", cfg, nil, ids...)
	if !rotateSessionForTest(env.pool, ids[0], "测试") {
		t.Fatal("开启前应能换会话")
	}
	cfg.Fixed = true
	env.pool.configure(cfg, env.global, 8)
	before := env.owner.binding(ids[0]).Slot
	env.owner.remove(ids[0])
	env.pool.refresh(env.owner, ids[0])
	env.clock.Advance(31 * 24 * time.Hour)
	env.pool.mu.Lock()
	env.pool.markDirtyLocked()
	env.pool.mu.Unlock()
	env.pool.flush()
	env.owner.add(ids[0], "")
	env.pool.refresh(env.owner, ids[0])
	if after := env.owner.binding(ids[0]).Slot; after != before {
		t.Fatalf("动态固定会话下账户回来后会话号变了：%s → %s", before, after)
	}
	// 删除账户：会话记录释放
	env.owner.remove(ids[0])
	env.pool.releaseFixed(ids[0], "账户已删除")
	env.pool.mu.Lock()
	_, kept := env.pool.sessions[ids[0]]
	env.pool.mu.Unlock()
	if kept {
		t.Fatal("删除账户时应释放动态固定会话的会话记录")
	}
}

// TestProxyPoolDynamicSessionPrunedWhenNotFixed 对照：没有开启固定会话时仍按 30 天修剪（与原来一致）
func TestProxyPoolDynamicSessionPrunedWhenNotFixed(t *testing.T) {
	ids := []string{"a@example.com", "b@example.com"}
	env := newFixedTestEnv(t, "", dynamicProxyPool(), nil, ids...)
	if !rotateSessionForTest(env.pool, ids[0], "测试") {
		t.Fatal("应能换会话")
	}
	env.owner.remove(ids[0])
	env.pool.refresh(env.owner, ids[0])
	env.clock.Advance(31 * 24 * time.Hour)
	env.pool.mu.Lock()
	env.pool.markDirtyLocked()
	env.pool.mu.Unlock()
	env.pool.flush()
	env.pool.mu.Lock()
	_, kept := env.pool.sessions[ids[0]]
	env.pool.mu.Unlock()
	if kept {
		t.Fatal("没有开启固定会话时应按 30 天修剪")
	}
}

// TestOnboardingDefersPausedFixedPort 新账户自动处理遇到固定端口暂停：不记为出错、不消耗重试次数、不写停用原因，放回队列等端口恢复
// 后再处理；恢复后照常验证
func TestOnboardingDefersPausedFixedPort(t *testing.T) {
	admin, ipPool, _, _ := fixedAdminEnv(t, fixedStaticPool(10001, 10001), "a@example.com")
	notes := accountDisableNotes
	onboarder := newTestOnboarder(admin, t.TempDir())
	t.Cleanup(onboarder.flush)
	admin.onboard = onboarder
	coolFixedPort(t, ipPool, "a@example.com")
	onboarder.enqueue([]string{"a@example.com"})
	started := time.Now()
	batch, policy := onboarder.takeBatch(started.Add(onboardingMaxWait))
	if len(batch) != 1 {
		t.Fatalf("本批 = %v", batch)
	}
	result, detail, retry := onboarder.process(context.Background(), "a@example.com", policy)
	if result != onboardDeferred || retry || !strings.Contains(detail, "暂停使用") {
		t.Fatalf("结果 = %s %q %v，期望暂缓", result, detail, retry)
	}
	if final := onboarder.finish("a@example.com", result, detail, retry); final != "" {
		t.Fatalf("暂缓不应是最终结果：%s", final)
	}
	onboarder.mu.Lock()
	item := onboarder.pending["a@example.com"]
	_, recorded := onboarder.records["a@example.com"]
	onboarder.mu.Unlock()
	if item == nil || item.attempts != 0 || item.notBefore.Before(started.Add(onboardingRetryDelay)) || recorded {
		t.Fatalf("应放回队列、不计重试次数、不早于重试间隔：%+v recorded=%v", item, recorded)
	}
	notes.mu.Lock()
	_, noted := notes.notes["a@example.com"]
	notes.mu.Unlock()
	if noted {
		t.Fatal("端口暂停不应写停用原因")
	}
	if batch, _ := onboarder.takeBatch(started.Add(onboardingMaxWait)); len(batch) != 0 {
		t.Fatalf("暂缓期间不应再处理：%v", batch)
	}
	if countSourceLogs(admin.requests, "a@example.com", "新账户自动处理暂缓 | 原因=IP 池固定端口暂停使用 | 端口=10001 | 下次=") != 1 {
		t.Fatalf("应写暂缓日志：%v", proxyPoolLogLines(admin.requests))
	}
	// 端口恢复后照常处理
	ipPool.mu.Lock()
	ipPool.ports[10001].coolUntil = time.Time{}
	ipPool.mu.Unlock()
	ipPool.recheckFired()
	batch, policy = onboarder.takeBatch(item.notBefore.Add(time.Second))
	if len(batch) != 1 {
		t.Fatalf("暂缓到期后应再处理：%v", batch)
	}
	result, _, _ = onboarder.process(context.Background(), "a@example.com", policy)
	if result == onboardDeferred || result == onboardError {
		t.Fatalf("端口恢复后应照常处理：%s", result)
	}
}

// TestProxyPoolFixedToggleClearsReleasedHold 固定分配开启时端口被其他进程占用（暂停），关掉开关后对方释放，重试时间过后再开启：
// 开启时的全量推送先写了暂停说明、同一次 wake 里检测确认已释放，这个端口上的账户不会一直暂停
func TestProxyPoolFixedToggleClearsReleasedHold(t *testing.T) {
	ids := proxyPoolTestIDs(6)
	env := newFixedTestEnv(t, "", fixedStaticPool(10001, 10002), nil, ids...)
	victims := env.usersOf(10001)
	env.locks.setOther(10001, true)
	if lease, _ := env.worker(victims[0]); lease.Refused() == nil {
		t.Fatal("10001 被占用时应被拒")
	}
	env.pool.configure(staticProxyPool(10001, 10002), env.global, 8)
	// 关闭期间另一个账户的浏览器独占 10002：其他账户的默认出口没有可用端口、按其他进程锁的重试时间等待（定时检查已安排）
	busy := env.usersOf(10002)[0]
	if lease, _ := env.worker(busy); leasePort(t, lease) != 10002 {
		t.Fatal("关闭后浏览器应独占 10002")
	}
	env.pool.mu.Lock()
	armed := !env.pool.recheckAt.IsZero()
	env.pool.mu.Unlock()
	if !armed {
		t.Fatal("前提：关闭期间已安排定时检查")
	}
	env.locks.setOther(10001, false)
	env.clock.Advance(proxyPoolLockedRetry + time.Second)
	env.pool.configure(fixedStaticPool(10001, 10002), env.global, 8)
	for _, id := range victims {
		if hold := env.owner.binding(id).Hold; hold != "" {
			t.Fatalf("%s：10001 已释放，暂停说明应清除：%q", id, hold)
		}
	}
	checkFixedInvariants(t, env)
}
