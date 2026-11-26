package app

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

// 坏 IP 自动换的保护规则（第 1 轮复查）：动态按会话计数、静态相对门槛（能用的端口不到一半时停止）、暂停端口的 halted 标签只给触发停止与停止后又坏
// 的端口、停止状态（原因、触发者、停止后又坏）写盘、状态里“停用的端口”不含自动换过的端口、恢复自动换会话后连不通的账户按新计数自动换

// failPortOnce 借端口上的第一个固定账户做一次出口连不通的启动失败（端口暂停、没有账户时什么也不做）
func (env *fixedTestEnv) failPortOnce(port int) {
	env.t.Helper()
	users := env.usersOf(port)
	if len(users) == 0 {
		return
	}
	lease := env.pool.acquire(users[0], users[0], proxyPoolKindWorker, users[0], env.global)
	if lease.Refused() != nil {
		return
	}
	lease.launchFailed(upstreamFailure, false)
}

// portCounts 返回端口 → 固定账户数
func (env *fixedTestEnv) portCounts(ids []string) map[int]int {
	counts := make(map[int]int)
	for _, id := range ids {
		counts[env.port(id)]++
	}
	return counts
}

// TestFixedAutoDynamicPerSession D7：动态固定会话按会话计数，同一账户换出的新会话 30 分钟内又连不通时按连锁熔断（第 2 轮复查：上限 10 时 10 分钟的
// 最短间隔让一个账户 30 分钟内最多 3 个坏会话，只按个数永远不熔断，每个账户每 10 分钟换一次会话）：第 1 轮自动换，第 2 轮熔断（halted_reason=
// chain，这一个不换）；停止后同一会话再坏不算“之后又坏”；写盘的计数键是账户 ID 加会话号。恢复自动换后连锁记录清空：同一账户再连不通照常自动换。
// 8 个账户、上限 10、全部会话持续连不通：前 4 个自动换，第 5 个账户按“连不通的账户超过一半”熔断，之后不再换
func TestFixedAutoDynamicPerSession(t *testing.T) {
	t.Run("same account", func(t *testing.T) {
		ids := proxyPoolTestIDs(2)
		env := fixedDynamicAutoEnv(t, 10, ids...)
		var sessions []string
		for round := range 2 {
			sessions = append(sessions, env.owner.binding(ids[0]).Slot)
			env.failSession(ids[0])
			if round == 0 {
				if env.generation(ids[0]) != 1 {
					t.Fatalf("第 1 轮应自动换会话：%v", proxyPoolLogLines(env.registry))
				}
				env.clock.Advance(proxyPoolRotateInterval + time.Minute)
			}
		}
		auto := env.autoStatus()
		if env.generation(ids[0]) != 1 || auto == nil || !auto.Halted || auto.HaltedCount != 2 || auto.HaltedReason != fixedHaltChain || auto.Recent != 2 || auto.Skipped != 0 {
			t.Fatalf("同一账户换出的新会话又连不通应按连锁熔断：代号 %d 状态 %+v", env.generation(ids[0]), auto)
		}
		if env.count("WARN service IP 池坏会话过多，已停止自动换会话，请手动处理 | 换出的新会话又连不通：这个账户 30 分钟内已自动换过会话（像是代理商整体出了问题） | 30 分钟内连不通的会话=2（上限 10）") != 1 {
			t.Fatalf("应写连锁的 L10：%v", proxyPoolLogLines(env.registry))
		}
		if stuck, ok := env.stuckOf(ids[0]); !ok || stuck.session != sessions[1] {
			t.Fatalf("触发停止的会话应记为连不通：%+v", stuck)
		}
		env.pool.flush()
		text := env.fixedRecordText()
		for _, session := range sessions {
			if !strings.Contains(text, fmt.Sprintf("%q", fixedSessionKey(ids[0], session))) {
				t.Fatalf("计数键应为账户 ID 加会话号（%s）：\n%s", session, text)
			}
		}
		if !strings.Contains(text, fmt.Sprintf(`"halted_key": %q`, "s"+fixedSessionKey(ids[0], sessions[1]))) || !strings.Contains(text, `"halted_reason": "chain"`) {
			t.Fatalf("应写下触发停止的会话与原因：\n%s", text)
		}
		env.clock.Advance(proxyPoolRotateInterval + time.Minute)
		env.failSession(ids[0])
		if auto := env.autoStatus(); auto.Skipped != 0 || env.generation(ids[0]) != 1 {
			t.Fatalf("触发停止的会话再坏不算之后又坏、也不换：%+v", auto)
		}
		// 恢复：窗口与连锁记录清空，同一账户（换过会话的）再连不通时照常自动换
		if result, err := env.pool.fixedAutoResume(); err != nil || !result.Resumed {
			t.Fatalf("恢复 = %+v (%v)", result, err)
		}
		env.clock.Advance(proxyPoolRotateInterval + time.Minute)
		env.failSession(ids[0])
		if auto := env.autoStatus(); env.generation(ids[0]) != 2 || auto.Halted || auto.Recent != 1 {
			t.Fatalf("恢复后同一账户再连不通应照常自动换：代号 %d 状态 %+v", env.generation(ids[0]), auto)
		}
	})
	t.Run("all bad", func(t *testing.T) {
		ids := proxyPoolTestIDs(8)
		env := fixedDynamicAutoEnv(t, 10, ids...)
		rotations := 0
		for range 6 {
			for _, id := range ids {
				before := env.generation(id)
				env.failSession(id)
				if env.generation(id) != before {
					rotations++
				}
			}
			env.clock.Advance(proxyPoolRotateInterval)
		}
		auto := env.autoStatus()
		if rotations != 4 || auto == nil || !auto.Halted || auto.HaltedCount != 5 || auto.HaltedReason != fixedHaltAccounts {
			t.Fatalf("全部会话持续连不通：换了 %d 次，状态 %+v", rotations, auto)
		}
		if env.count("WARN service IP 池坏会话过多，已停止自动换会话，请手动处理 | 连不通的账户超过一半：30 分钟内 5/8 个账户的会话连不通（像是代理商整体出了问题） | 30 分钟内连不通的会话=5（上限 10）") != 1 {
			t.Fatalf("应写按账户的 L10：%v", proxyPoolLogLines(env.registry))
		}
	})
}

// TestFixedAutoDynamicResumeStuck D8：恢复自动换会话之后，停止期间连不通、没有换的账户不立刻换；它的 Worker 再启动失败（达到换会话条件）时按新的
// 计数自动换（确认框照此说明）
func TestFixedAutoDynamicResumeStuck(t *testing.T) {
	ids := proxyPoolTestIDs(3)
	env := fixedDynamicAutoEnv(t, 1, ids...)
	env.failSession(ids[0])
	env.failSession(ids[1])
	if _, stuck := env.stuckOf(ids[1]); !stuck || !env.autoStatus().Halted || env.generation(ids[1]) != 0 {
		t.Fatalf("第 2 个应熔断、记为连不通：%+v", env.autoStatus())
	}
	if result, err := env.pool.fixedAutoResume(); err != nil || !result.Resumed {
		t.Fatalf("恢复 = %+v (%v)", result, err)
	}
	if _, stuck := env.stuckOf(ids[1]); !stuck || env.generation(ids[1]) != 0 {
		t.Fatal("恢复时不立刻换会话")
	}
	env.clock.Advance(time.Minute)
	lease := env.pool.acquire(ids[1], ids[1], proxyPoolKindWorker, ids[1], env.global)
	lease.launchFailed(upstreamFailure, false)
	if _, stuck := env.stuckOf(ids[1]); stuck || env.generation(ids[1]) != 1 ||
		env.count(ids[1]+" IP 池换会话 | 会话=") != 1 || env.count("原因=连续 2 次出口连不通，固定会话自动换会话（30 分钟内第 1 个，上限 1）") != 2 {
		t.Fatalf("恢复后再启动失败应按新的计数自动换：%v", proxyPoolLogLines(env.registry))
	}
}

// TestFixedAutoHalfHalt A23：静态的相对门槛与连锁。4 个端口、8 个账户、上限 10，4 个端口交替两次出口连不通（整体故障保护的门槛 5 到不了）：第 1 个
// 自动换，第 2 个端口上有刚被挪过来的账户（换过的新 IP 也坏了）按连锁熔断（halted_reason=chain），后两个按停止后又坏处理；只有 1 个端口停用，
// 冷却到期后账户都在原来的 3 个端口上。相对门槛单独看：6 个端口里 2 个在上限为 0 时冷却（不计数），自动换 1 个之后再坏 1 个只剩 2/6 能用，按
// 相对门槛熔断（halted_reason=half）；恢复后停用的端口仍占一半时下一个坏的再次熔断。2 个端口：第 1 个自动换，另一个再坏熔断（不是 no_target）；
// 1 个端口：没有可挪的端口（no_target），不熔断
func TestFixedAutoHalfHalt(t *testing.T) {
	t.Run("four ports outage", func(t *testing.T) {
		ids := proxyPoolTestIDs(8)
		env := newFixedAutoEnv(t, "", 10, fixedStaticPool(10001, 10004), ids...)
		for port := 10001; port <= 10004; port++ {
			env.failPortOnce(port)
		}
		for port := 10001; port <= 10004; port++ {
			env.failPortOnce(port)
			env.clock.Advance(time.Minute)
		}
		if !env.excluded(10001) || env.excluded(10002) || env.excluded(10003) || env.excluded(10004) {
			t.Fatalf("应只自动换第 1 个：%v", proxyPoolLogLines(env.registry))
		}
		auto := env.autoStatus()
		if auto == nil || !auto.Halted || auto.HaltedReason != fixedHaltChain || auto.HaltedCount != 2 || auto.Skipped != 2 || auto.ReplacedTotal != 1 {
			t.Fatalf("第 2 个应按连锁熔断：%+v", auto)
		}
		if env.count("WARN service IP 池坏 IP 过多，已停止自动换 IP，请手动处理 | 换过的新 IP 又坏了：这个端口上 1 个账户是 30 分钟内自动换 IP 挪过来的（像是代理商整体出了问题） | 30 分钟内坏掉=2（上限 10） | 端口=10002 按暂停处理") != 1 ||
			env.count("IP 池多数端口同时出错") != 0 {
			t.Fatalf("应写连锁的 L3（整体故障保护没有触发）：%v", proxyPoolLogLines(env.registry))
		}
		for _, port := range []int{10002, 10003, 10004} {
			if entry := env.pausedEntry(port); entry == nil || entry.Auto != fixedAutoHalted {
				t.Fatalf("%d 应暂停并标 halted：%+v", port, entry)
			}
		}
		if counts := env.portCounts(ids); counts[10002] != 3 || counts[10003] != 3 || counts[10004] != 2 {
			t.Fatalf("只挪了第 1 个端口的账户：%v", counts)
		}
		// 重启后停止原因仍是 chain
		env.pool.flush()
		clock := newProxyPoolTestClock()
		clock.Advance(env.clock.Now().Sub(clock.Now()))
		reloaded, _, _ := newFixedPool(t, env.dir, clock, env.locks)
		reloaded.setFixedAutoReplace(10)
		reloaded.configure(fixedStaticPool(10001, 10004), env.global, 8)
		if auto := reloaded.status().Fixed.Auto; auto == nil || !auto.Halted || auto.HaltedReason != fixedHaltChain || auto.Skipped != 2 {
			t.Fatalf("重启后停止状态 = %+v", auto)
		}
		env.clock.Advance(time.Hour)
		env.pool.recheckFired()
		for _, id := range ids {
			if hold := env.owner.binding(id).Hold; hold != "" {
				t.Fatalf("冷却到期后 %s 不应再暂停：%q", id, hold)
			}
		}
		checkFixedInvariants(t, env)
		// 恢复后连锁与按账户的记录清空：之前自动挪过来的账户所在端口再坏也照常自动换
		if result, err := env.pool.fixedAutoResume(); err != nil || !result.Resumed || result.Skipped != 2 {
			t.Fatalf("恢复 = %+v (%v)", result, err)
		}
		env.coolPort(10002)
		if auto := env.autoStatus(); !env.excluded(10002) || auto.Halted || auto.Recent != 1 {
			t.Fatalf("恢复后应照常自动换：%+v", auto)
		}
		checkFixedInvariants(t, env)
	})
	t.Run("half", func(t *testing.T) {
		// 6 个端口、12 个账户：2 个端口在上限为 0 时冷却（不计数），出错端口始终不到整体故障保护的 5 个
		ids := proxyPoolTestIDs(12)
		env := newFixedAutoEnv(t, "", 0, fixedStaticPool(10001, 10006), ids...)
		for _, port := range []int{10005, 10006} {
			env.coolPort(port)
		}
		env.pool.setFixedAutoReplace(10)
		env.coolPort(10001)
		if !env.excluded(10001) {
			t.Fatal("第 1 个应自动换（能用的还剩 3/6）")
		}
		env.clock.Advance(time.Minute)
		env.coolPort(10004)
		auto := env.autoStatus()
		if env.excluded(10004) || auto == nil || !auto.Halted || auto.HaltedReason != fixedHaltHalf || auto.HaltedCount != 2 {
			t.Fatalf("再自动换只剩 2/6 能用时应按相对门槛熔断：%+v", auto)
		}
		if env.count("WARN service IP 池坏 IP 过多，已停止自动换 IP，请手动处理 | 能用的端口不到一半：再自动换只剩 2/6 个（停用与冷却中的不算） | 30 分钟内坏掉=2（上限 10） | 端口=10004 按暂停处理") != 1 ||
			env.count("IP 池多数端口同时出错") != 0 {
			t.Fatalf("应写相对门槛的 L3：%v", proxyPoolLogLines(env.registry))
		}
		if entry := env.pausedEntry(10004); entry == nil || entry.Auto != fixedAutoHalted {
			t.Fatalf("10004 应标 halted：%+v", entry)
		}
		env.pool.flush()
		clock := newProxyPoolTestClock()
		clock.Advance(env.clock.Now().Sub(clock.Now()))
		reloaded, _, _ := newFixedPool(t, env.dir, clock, env.locks)
		reloaded.setFixedAutoReplace(10)
		reloaded.configure(fixedStaticPool(10001, 10006), env.global, 8)
		if auto := reloaded.status().Fixed.Auto; auto == nil || !auto.Halted || auto.HaltedReason != fixedHaltHalf {
			t.Fatalf("重启后停止状态 = %+v", auto)
		}
		// 恢复后停用的端口仍占一半：手动换 IP 再停用 2 个（共 3/6），再坏一个只剩 2/6，再次熔断、不挪
		env.clock.Advance(time.Hour)
		env.pool.recheckFired()
		if result, err := env.pool.fixedAutoResume(); err != nil || !result.Resumed {
			t.Fatalf("恢复 = %+v (%v)", result, err)
		}
		for _, port := range []int{10002, 10003} {
			if _, _, err := env.pool.fixedReassign(port, false); err != nil {
				t.Fatal(err)
			}
		}
		env.coolPort(10004)
		if auto := env.autoStatus(); env.excluded(10004) || !auto.Halted || auto.HaltedReason != fixedHaltHalf || auto.HaltedCount != 1 {
			t.Fatalf("停用的端口仍占一半时应再次熔断：%+v", auto)
		}
		checkFixedInvariants(t, env)
	})
	t.Run("two ports", func(t *testing.T) {
		ids := proxyPoolTestIDs(4)
		env := newFixedAutoEnv(t, "", 10, fixedStaticPool(10001, 10002), ids...)
		env.coolPort(10001)
		if !env.excluded(10001) || len(env.usersOf(10002)) != 4 {
			t.Fatal("2 个端口坏 1 个：自动换（能用的仍有一半）")
		}
		env.clock.Advance(time.Minute)
		env.coolPort(10002)
		auto := env.autoStatus()
		if env.excluded(10002) || !auto.Halted || auto.HaltedReason != fixedHaltHalf {
			t.Fatalf("另一个再坏应熔断：%+v", auto)
		}
		if entry := env.pausedEntry(10002); entry == nil || entry.Auto != fixedAutoHalted {
			t.Fatalf("应标 halted、不是 no_target：%+v", entry)
		}
	})
	t.Run("one port", func(t *testing.T) {
		ids := proxyPoolTestIDs(2)
		env := newFixedAutoEnv(t, "", 10, fixedStaticPool(10001, 10001), ids...)
		env.coolPort(10001)
		if auto := env.autoStatus(); auto.Halted || env.excluded(10001) {
			t.Fatalf("只有一个端口时没有可挪的端口、不熔断：%+v", auto)
		}
		if entry := env.pausedEntry(10001); entry == nil || entry.Auto != fixedAutoNoTarget {
			t.Fatalf("应标 no_target：%+v", entry)
		}
	})
}

// TestFixedAutoHaltedLabel A24：停止自动换后只给触发停止的与停止之后又坏的端口标 halted：停止前因为没有可挪的端口而暂停的仍是 no_target，被其他进程
// 占用的不标，上限为 0 时冷却的端口不标；停止原因、触发者与停止之后又坏的端口写盘，重启后 skipped 不归 0、标签不变
func TestFixedAutoHaltedLabel(t *testing.T) {
	t.Run("no target stays", func(t *testing.T) {
		ids := proxyPoolTestIDs(5)
		env := newFixedAutoEnv(t, "", 2, fixedStaticPool(10001, 10005), ids...)
		for _, port := range []int{10002, 10003, 10004, 10005} {
			env.locks.setOther(port, true)
			if lease, _ := env.worker(env.usersOf(port)[0]); lease.Refused() == nil {
				t.Fatal("被其他进程锁住的端口应拒绝")
			}
		}
		// 其余端口都被其他进程占用（不算坏、不计入相对门槛）：没有可挪的端口
		env.coolPort(10001)
		if entry := env.pausedEntry(10001); env.excluded(10001) || entry == nil || entry.Auto != fixedAutoNoTarget {
			t.Fatalf("其余端口都被占用：no_target：%+v", entry)
		}
		for _, port := range []int{10002, 10003, 10004} {
			env.locks.setOther(port, false)
		}
		env.clock.Advance(2 * time.Minute)
		env.pool.recheckFired()
		env.coolPort(10002)
		if !env.excluded(10002) {
			t.Fatalf("第 2 个应自动换（能用的 2/4，被其他进程占用的 10005 不计入）：%+v", env.autoStatus())
		}
		env.clock.Advance(time.Minute)
		env.coolPort(10003)
		env.clock.Advance(time.Minute)
		env.coolPort(10004)
		auto := env.autoStatus()
		if auto == nil || !auto.Halted || auto.HaltedReason != fixedHaltLimit || auto.HaltedCount != 3 || auto.Skipped != 1 {
			t.Fatalf("第 3 个应熔断、第 4 个为停止后又坏：%+v", auto)
		}
		labels := map[int]string{}
		for _, port := range []int{10001, 10003, 10004, 10005} {
			if entry := env.pausedEntry(port); entry != nil {
				labels[port] = entry.Auto
			}
		}
		if labels[10001] != fixedAutoNoTarget || labels[10003] != fixedAutoHalted || labels[10004] != fixedAutoHalted || labels[10005] != "" {
			t.Fatalf("暂停端口的标签 = %v", labels)
		}
		// 重启：停止状态、触发者与停止之后又坏的端口读回（冷却也从 proxy-pool.json 读回），skipped 不归 0、标签不变；no_target 只在内存
		env.pool.flush()
		text := env.fixedRecordText()
		if !strings.Contains(text, `"halted_reason": "limit"`) || !strings.Contains(text, `"halted_key": "p10003"`) || !strings.Contains(text, `"p10004"`) {
			t.Fatalf("停止状态应写盘：\n%s", text)
		}
		clock := newProxyPoolTestClock()
		clock.Advance(env.clock.Now().Sub(clock.Now()))
		pool, _, _ := newFixedPool(t, env.dir, clock, env.locks)
		pool.setFixedAutoReplace(2)
		pool.configure(fixedStaticPool(10001, 10005), env.global, 8)
		reloaded := pool.status().Fixed
		if reloaded.Auto == nil || !reloaded.Auto.Halted || reloaded.Auto.Skipped != 1 || reloaded.Auto.HaltedReason != fixedHaltLimit {
			t.Fatalf("重启后停止状态 = %+v", reloaded.Auto)
		}
		labels = map[int]string{}
		for _, entry := range reloaded.Paused {
			labels[entry.Port] = entry.Auto
		}
		if labels[10003] != fixedAutoHalted || labels[10004] != fixedAutoHalted || labels[10001] != "" {
			t.Fatalf("重启后暂停端口的标签 = %v（%+v）", labels, reloaded.Paused)
		}
	})
	t.Run("cooled while zero", func(t *testing.T) {
		ids := proxyPoolTestIDs(16)
		env := newFixedAutoEnv(t, "", 0, fixedStaticPool(10001, 10008), ids...)
		env.coolPort(10008)
		env.pool.setFixedAutoReplace(1)
		env.coolPort(10001)
		env.clock.Advance(time.Minute)
		env.coolPort(10002)
		if auto := env.autoStatus(); auto == nil || !auto.Halted || !env.excluded(10001) || env.excluded(10002) {
			t.Fatalf("改成 1 后第 2 个应熔断：%+v", auto)
		}
		if entry := env.pausedEntry(10002); entry == nil || entry.Auto != fixedAutoHalted {
			t.Fatalf("触发停止的端口标 halted：%+v", entry)
		}
		if entry := env.pausedEntry(10008); entry == nil || entry.Auto != "" {
			t.Fatalf("上限为 0 时冷却的端口不标：%+v", entry)
		}
	})
}

// TestFixedAutoExcludedList A25：上限大于 0 时状态的 excluded 只列不是自动换过的停用端口（自动换过的在 auto.replaced 里，excluded_total 仍为全部
// 停用端口数）：自动换过的超过 50 个时，手动换 IP 停用的端口照样列出（前端按 excluded_total − replaced_total 显示计数）。改成 0 后没有 auto 区块，
// excluded 照常列全部停用端口
func TestFixedAutoExcludedList(t *testing.T) {
	ids := proxyPoolTestIDs(240)
	env := newFixedAutoEnv(t, "", 10, fixedStaticPool(10001, 10200), ids...)
	for _, port := range []int{10199, 10200} {
		if _, _, err := env.pool.fixedReassign(port, false); err != nil {
			t.Fatal(err)
		}
	}
	for index := range 60 {
		port := env.healthyPortWithUsers()
		env.coolPort(port)
		if !env.excluded(port) {
			t.Fatalf("第 %d 个 %d 应自动换：%+v", index+1, port, env.autoStatus())
		}
		env.clock.Advance(16 * time.Minute)
	}
	status := env.pool.status().Fixed
	if !slices.Equal(status.Excluded, []int{10199, 10200}) || status.ExcludedTotal != 62 || status.Auto.ReplacedTotal != 60 || len(status.Auto.Replaced) != proxyPoolFixedAutoListLimit {
		t.Fatalf("停用端口列表 = %v（总数 %d），自动换过 %d（列出 %d）", status.Excluded, status.ExcludedTotal, status.Auto.ReplacedTotal, len(status.Auto.Replaced))
	}
	env.pool.setFixedAutoReplace(0)
	status = env.pool.status().Fixed
	replaced := env.replaced()
	autoListed := 0
	for _, port := range status.Excluded {
		if _, ok := replaced[port]; ok {
			autoListed++
		}
	}
	if status.Auto != nil || len(status.Excluded) != proxyPoolFixedListLimit || status.ExcludedTotal != 62 || autoListed == 0 {
		t.Fatalf("上限为 0 时照常列全部停用端口：%v（总数 %d，其中自动换过 %d）", status.Excluded, status.ExcludedTotal, autoListed)
	}
}
