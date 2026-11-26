package app

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// 坏 IP 自动换的保护规则（第 2 轮复查）：连锁（换过的新 IP、新会话 30 分钟内又坏）、按账户的门槛（30 分钟内 IP 坏掉的账户超过走 IP 池的账户的
// 一半）与相对门槛不把被其他进程占用的端口算作能用。账户少时一次代理商整体故障里同时出错的端口不到整体故障保护的 5 个、也到不了上限，没有前两条
// 会每隔十几分钟给每个账户换一次 IP（或会话），故障过去后全部账户都换了 IP

// outageStatic 模拟代理商整体故障：每 5 分钟（预热失败退避）每个账户启动一次 Worker、出口连不通（端口暂停时申请被拒、不算），持续 duration
func (env *fixedTestEnv) outageStatic(ids []string, duration time.Duration) {
	env.t.Helper()
	start := env.clock.Now()
	for env.clock.Now().Sub(start) < duration {
		for _, id := range ids {
			lease := env.pool.acquire(id, id, proxyPoolKindWorker, id, env.global)
			if lease.Refused() == nil {
				lease.launchFailed(upstreamFailure, false)
			} else {
				lease.release()
			}
		}
		env.clock.Advance(5 * time.Minute)
		env.pool.recheckFired()
	}
}

// TestFixedAutoStaticFewAccountsOutage R2-1：静态、端口段 200 个、上限 10，代理商整体故障 3 小时。1 个账户：第 1 个坏端口自动换，换过去的新端口
// 又坏按连锁熔断（halted_reason=chain，上限与相对门槛都到不了）；3 个账户：第 2 个账户的端口坏时 IP 坏掉的账户 2/3 超过一半，按账户熔断
// （halted_reason=accounts）。两种都只自动换 1 个端口，故障过去后最多 1 个账户换了 IP（之前每个账户每十几分钟换一次、几十个端口被停用）
func TestFixedAutoStaticFewAccountsOutage(t *testing.T) {
	for _, test := range []struct {
		accounts int
		reason   string
		detail   string
	}{
		{accounts: 1, reason: fixedHaltChain, detail: "换过的新 IP 又坏了：这个端口上 1 个账户是 30 分钟内自动换 IP 挪过来的（像是代理商整体出了问题） | 30 分钟内坏掉=2（上限 10）"},
		{accounts: 3, reason: fixedHaltAccounts, detail: "IP 坏掉的账户超过一半：30 分钟内 2/3 个账户所在的端口坏了（像是代理商整体出了问题） | 30 分钟内坏掉=2（上限 10）"},
	} {
		t.Run(fmt.Sprintf("accounts=%d", test.accounts), func(t *testing.T) {
			ids := proxyPoolTestIDs(test.accounts)
			env := newFixedAutoEnv(t, "", 10, fixedStaticPool(10001, 10200), ids...)
			original := env.portCounts(ids)
			before := make(map[string]int, len(ids))
			for _, id := range ids {
				before[id] = env.port(id)
			}
			env.outageStatic(ids, 3*time.Hour)
			auto := env.autoStatus()
			if auto == nil || !auto.Halted || auto.HaltedReason != test.reason || auto.ReplacedTotal != 1 {
				t.Fatalf("应只自动换 1 个端口后按 %s 熔断：%+v", test.reason, auto)
			}
			if env.count("WARN service IP 池坏 IP 过多，已停止自动换 IP，请手动处理 | "+test.detail) != 1 {
				t.Fatalf("应写熔断的 L3：%v", proxyPoolLogLines(env.registry))
			}
			changed := 0
			for _, id := range ids {
				if env.port(id) != before[id] {
					changed++
				}
			}
			if changed != 1 {
				t.Fatalf("故障过去后应只有 1 个账户换了 IP：%d（之前 %v，现在 %v）", changed, original, env.portCounts(ids))
			}
			checkFixedInvariants(t, env)
		})
	}
}

// TestFixedAutoChainRules R2-1：连锁只认“自动换 IP 挪过来、30 分钟内、恢复之后”的账户：手动换 IP 挪过去的账户所在端口再坏不算连锁；恢复自动换后
// 连锁记录清空，之前自动挪过来的账户所在端口再坏照常自动换；窗口内坏掉的端口上的账户（affected）写盘、重启后照样判连锁
func TestFixedAutoChainRules(t *testing.T) {
	t.Run("manual move is not chain", func(t *testing.T) {
		ids := proxyPoolTestIDs(1)
		env := newFixedAutoEnv(t, "", 10, fixedStaticPool(10001, 10008), ids...)
		first := env.port(ids[0])
		env.coolPort(first)
		moved := env.port(ids[0])
		if moved == first || !env.excluded(first) {
			t.Fatal("第 1 个应自动换")
		}
		if _, _, err := env.pool.fixedReassign(moved, false); err != nil {
			t.Fatal(err)
		}
		manual := env.port(ids[0])
		env.clock.Advance(time.Minute)
		env.coolPort(manual)
		if auto := env.autoStatus(); auto.Halted || !env.excluded(manual) {
			t.Fatalf("手动换 IP 挪过去的账户所在端口再坏不是连锁：%+v", auto)
		}
	})
	t.Run("resume clears chain", func(t *testing.T) {
		ids := proxyPoolTestIDs(1)
		env := newFixedAutoEnv(t, "", 10, fixedStaticPool(10001, 10008), ids...)
		env.coolPort(env.port(ids[0]))
		movedAt := env.clock.Now()
		moved := env.port(ids[0])
		env.clock.Advance(time.Minute)
		env.coolPort(moved)
		if auto := env.autoStatus(); !auto.Halted || auto.HaltedReason != fixedHaltChain || env.excluded(moved) {
			t.Fatalf("换过去的新端口又坏应按连锁熔断：%+v", auto)
		}
		if result, err := env.pool.fixedAutoResume(); err != nil || !result.Resumed {
			t.Fatalf("恢复 = %+v (%v)", result, err)
		}
		// 冷却到期后、仍在挪动后的 30 分钟里再坏：恢复时清空了连锁记录，照常自动换
		env.clock.Advance(16 * time.Minute)
		env.pool.recheckFired()
		if env.clock.Now().Sub(movedAt) >= proxyPoolFixedAutoWindow {
			t.Fatal("应仍在挪动后的 30 分钟里")
		}
		env.coolPort(moved)
		if auto := env.autoStatus(); auto.Halted || !env.excluded(moved) || auto.Recent != 1 {
			t.Fatalf("恢复后应照常自动换：%+v", auto)
		}
		checkFixedInvariants(t, env)
	})
	t.Run("persisted", func(t *testing.T) {
		ids := proxyPoolTestIDs(1)
		env := newFixedAutoEnv(t, "", 10, fixedStaticPool(10001, 10008), ids...)
		env.coolPort(env.port(ids[0]))
		moved := env.port(ids[0])
		env.pool.flush()
		var record proxyPoolFixedRecord
		text := env.fixedRecordText()
		if err := json.Unmarshal([]byte(text), &record); err != nil || record.Auto == nil || record.Auto.Affected[ids[0]].IsZero() {
			t.Fatalf("坏掉的端口上的账户应写盘：%s (%v)", text, err)
		}
		assertNoPoolSecret(t, "固定表文件", text)
		clock := newProxyPoolTestClock()
		clock.Advance(env.clock.Now().Add(time.Minute).Sub(clock.Now()))
		reloaded, _, _ := newFixedPool(t, env.dir, clock, env.locks)
		reloaded.setFixedAutoReplace(10)
		reloaded.configure(fixedStaticPool(10001, 10008), env.global, 8)
		reloaded.mu.Lock()
		chained := reloaded.fixedAutoChainedLocked(reloaded.fixedUsersLocked(moved), reloaded.now())
		reloaded.mu.Unlock()
		if chained != 1 {
			t.Fatalf("重启后应照样判连锁：%d", chained)
		}
	})
}

// TestFixedAutoLockedSqueeze R2-2：相对门槛不把被其他进程占用的端口算作能用。10 个端口里 5 个被其他进程占用，20 个账户、上限 10，其余 5 个依次冷却：
// 第 3 个时能用的只剩 2/5（占用的不计入）按相对门槛熔断，日志写明占用的个数；之前占用的端口算作能用，4 个都自动换，可挪的 5 个端口上的账户全部
// 挤到最后一个端口
func TestFixedAutoLockedSqueeze(t *testing.T) {
	ids := proxyPoolTestIDs(20)
	env := newFixedAutoEnv(t, "", 10, fixedStaticPool(10001, 10010), ids...)
	for _, port := range []int{10006, 10007, 10008, 10009, 10010} {
		env.locks.setOther(port, true)
		if lease, _ := env.worker(env.usersOf(port)[0]); lease.Refused() == nil {
			t.Fatal("被其他进程锁住的端口应拒绝")
		}
	}
	for _, port := range []int{10005, 10004} {
		env.coolPort(port)
		if !env.excluded(port) {
			t.Fatalf("%d 应自动换：%+v", port, env.autoStatus())
		}
		env.clock.Advance(time.Minute)
	}
	env.coolPort(10003)
	auto := env.autoStatus()
	if env.excluded(10003) || auto == nil || !auto.Halted || auto.HaltedReason != fixedHaltHalf || auto.HaltedCount != 3 {
		t.Fatalf("第 3 个应按相对门槛熔断：%+v", auto)
	}
	if env.count("WARN service IP 池坏 IP 过多，已停止自动换 IP，请手动处理 | 能用的端口不到一半：再自动换只剩 2/5 个（停用与冷却中的不算，另有 5 个被其他进程占用、不计入） | 30 分钟内坏掉=3（上限 10） | 端口=10003 按暂停处理") != 1 {
		t.Fatalf("应写相对门槛的 L3：%v", proxyPoolLogLines(env.registry))
	}
	counts := env.portCounts(ids)
	for port, count := range counts {
		if count > 4 {
			t.Fatalf("不应把账户挤到少数端口上：%v", counts)
		}
		if port >= 10006 && count != 2 {
			t.Fatalf("被其他进程占用的端口上的账户不动：%v", counts)
		}
	}
	checkFixedInvariants(t, env)
}

// TestFixedAutoDynamicFewAccountsOutage R2-1：动态固定会话、上限 10，全部会话持续连不通 3 小时：1 个账户第 2 个坏会话按连锁熔断，2～4 个账户在会话
// 连不通的账户超过一半时按账户熔断；都只换几次会话（之前每个账户每 10 分钟换一次，3 小时 30～52 次，永不熔断）
func TestFixedAutoDynamicFewAccountsOutage(t *testing.T) {
	for _, test := range []struct {
		accounts, rotations int
		reason              string
	}{
		{accounts: 1, rotations: 1, reason: fixedHaltChain},
		{accounts: 2, rotations: 1, reason: fixedHaltAccounts},
		{accounts: 3, rotations: 1, reason: fixedHaltAccounts},
		{accounts: 4, rotations: 2, reason: fixedHaltAccounts},
	} {
		t.Run(fmt.Sprintf("accounts=%d", test.accounts), func(t *testing.T) {
			ids := proxyPoolTestIDs(test.accounts)
			env := fixedDynamicAutoEnv(t, 10, ids...)
			start := env.clock.Now()
			rotations := 0
			for env.clock.Now().Sub(start) < 3*time.Hour {
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
			if rotations != test.rotations || auto == nil || !auto.Halted || auto.HaltedReason != test.reason {
				t.Fatalf("换了 %d 次会话，状态 %+v", rotations, auto)
			}
			if !strings.Contains(strings.Join(proxyPoolLogLines(env.registry), "\n"), "IP 池坏会话过多，已停止自动换会话，请手动处理 | ") {
				t.Fatal("应写 L10")
			}
			assertNoPoolSecretInLogs(t, env.registry)
		})
	}
}
