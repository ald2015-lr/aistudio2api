package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/api"
	"github.com/Mag1cFall/AIStudio2API/internal/config"
)

// 坏 IP 自动换（PROXY_POOL_FIXED_AUTO_REPLACE）的 IP 池单元测试：上限为 0 时与原来相同、少数坏了自动换、窗口与熔断、没有可挪的端口、
// 被其他进程占用与整体故障保护不算坏、恢复、重启、热更新、手动操作、端口段变化、空端口、动态固定会话的自动换会话与手动换会话

// newFixedAutoEnv 与 newFixedTestEnv 相同，limit 不小于 0 时在第一次 configure 之前设置坏 IP 自动换的上限（与 newRuntime 的顺序相同）
func newFixedAutoEnv(t *testing.T, dir string, limit int, cfg config.ProxyPool, ids ...string) *fixedTestEnv {
	t.Helper()
	clock := newProxyPoolTestClock()
	locks := &proxyPoolGoldenLocks{held: make(map[int]bool), other: make(map[int]bool)}
	pool, registry, dir := newFixedPool(t, dir, clock, locks)
	env := &fixedTestEnv{t: t, clock: clock, pool: pool, registry: registry, locks: locks, dir: dir, global: proxyPoolTestGlobal}
	env.owner = newFakeProxyOwner()
	accounts := make([]*aistudio.Account, 0, len(ids))
	for _, id := range ids {
		env.owner.add(id, "")
		accounts = append(accounts, &aistudio.Account{ID: id, Config: aistudio.AccountConfig{Enabled: true}})
	}
	if limit >= 0 {
		pool.setFixedAutoReplace(limit)
	}
	pool.configure(cfg, env.global, 8)
	pool.prepareFixed(accounts)
	for _, account := range accounts {
		pool.prime(account)
	}
	pool.attach(env.owner)
	return env
}

// autoStatus 返回状态接口的自动换字段（固定分配没有生效时为 nil）
func (env *fixedTestEnv) autoStatus() *api.AdminProxyPoolFixedAuto {
	status := env.pool.status()
	if status == nil || status.Fixed == nil {
		return nil
	}
	return status.Fixed.Auto
}

// excluded 判断端口已停用
func (env *fixedTestEnv) excluded(port int) bool {
	env.pool.mu.Lock()
	defer env.pool.mu.Unlock()
	return env.pool.fixedExcludedLocked(port)
}

// replaced 返回自动换过的端口记录
func (env *fixedTestEnv) replaced() map[int]proxyPoolFixedReplaced {
	env.pool.mu.Lock()
	defer env.pool.mu.Unlock()
	copied := make(map[int]proxyPoolFixedReplaced, len(env.pool.fixed.auto.replaced))
	for port, record := range env.pool.fixed.auto.replaced {
		copied[port] = record
	}
	return copied
}

// pausedEntry 返回状态接口里端口的暂停记录（没有时为 nil）
func (env *fixedTestEnv) pausedEntry(port int) *api.AdminProxyPoolPausedPort {
	status := env.pool.status()
	if status == nil || status.Fixed == nil {
		return nil
	}
	for index := range status.Fixed.Paused {
		if status.Fixed.Paused[index].Port == port {
			return &status.Fixed.Paused[index]
		}
	}
	return nil
}

// holdText 返回账户此刻在 IP 池里的暂停说明（与 bindingLocked 同一口径）
func (env *fixedTestEnv) expectedHold(accountID string) string {
	env.pool.mu.Lock()
	defer env.pool.mu.Unlock()
	port := env.pool.fixed.entries[accountID].port
	hold := env.pool.fixedHoldTextLocked(port, env.pool.now())
	if lease := env.pool.bindingLeaseLocked(accountID); lease != nil && hold == "" {
		hold = env.pool.fixedAutoHoldLocked(lease, port)
	}
	return hold
}

// coolPort 用规则 B 冷却端口（借它的第一个固定账户连续两次上游失败的启动）
func (env *fixedTestEnv) coolPort(port int) {
	env.t.Helper()
	users := env.usersOf(port)
	if len(users) == 0 {
		env.t.Fatalf("端口 %d 上没有固定账户", port)
	}
	env.coolUpstream(users[0])
}

// healthyPortWithUsers 返回最大的、有固定账户、没有停用也不暂停的端口
func (env *fixedTestEnv) healthyPortWithUsers() int {
	env.pool.mu.Lock()
	defer env.pool.mu.Unlock()
	now := env.pool.now()
	for port := env.pool.settings.portEnd; port >= env.pool.settings.portStart; port-- {
		if env.pool.fixedCountLocked(port) == 0 || env.pool.fixedExcludedLocked(port) {
			continue
		}
		if reason, _ := env.pool.fixedPauseLocked(port, now); reason != "" {
			continue
		}
		return port
	}
	return 0
}

// cooledHook 登记冷却钩子，返回收到的冷却
func cooledHook(pool *proxyPool) *[]proxyPoolCooling {
	var cooled []proxyPoolCooling
	pool.setPortCooledHook(func(cooling proxyPoolCooling) { cooled = append(cooled, cooling) })
	return &cooled
}

// notifyCounter 登记账户列表推送回调，返回计数
func notifyCounter(pool *proxyPool) *atomic.Int64 {
	var count atomic.Int64
	pool.setAccountsChangedHook(func() { count.Add(1) })
	return &count
}

// fixedRecordText 返回固定表文件文本（不存在时为空）
func (env *fixedTestEnv) fixedRecordText() string {
	data, err := os.ReadFile(filepath.Join(env.dir, proxyPoolFixedFileName))
	if err != nil {
		return ""
	}
	return string(data)
}

// TestFixedAutoZeroIsBaseline A1：上限为 0（从不设置与设置为 0 两种）时规则 B 冷却按原来的规则暂停：不挪、不停用、不记计数，Hold 与日志原文，
// 状态没有 auto 区块，固定表文件不被改写
func TestFixedAutoZeroIsBaseline(t *testing.T) {
	for _, limit := range []int{-1, 0} {
		t.Run(fmt.Sprintf("limit=%d", limit), func(t *testing.T) {
			ids := proxyPoolTestIDs(8)
			env := newFixedAutoEnv(t, "", limit, fixedStaticPool(10001, 10004), ids...)
			env.pool.flush()
			before := env.fixedRecordText()
			port := env.port(ids[0])
			users := env.usersOf(port)
			env.coolPort(port)
			for _, id := range users {
				if env.port(id) != port {
					t.Fatalf("上限为 0 时不应挪账户：%s → %d", id, env.port(id))
				}
				if binding := env.owner.binding(id); binding == nil || !strings.HasSuffix(binding.Hold, "；固定分配不换端口") {
					t.Fatalf("应按原规则暂停：%s", fixedBindingText(binding))
				}
			}
			env.pool.mu.Lock()
			empty := len(env.pool.fixed.auto.ports) == 0 && len(env.pool.fixed.auto.replaced) == 0 && !env.pool.fixed.auto.halted()
			env.pool.mu.Unlock()
			if env.excluded(port) || !empty || env.autoStatus() != nil {
				t.Fatalf("上限为 0 时不应有任何自动换状态：excluded=%v empty=%v", env.excluded(port), empty)
			}
			if entry := env.pausedEntry(port); entry == nil || entry.Auto != "" {
				t.Fatalf("暂停记录不应带 auto：%+v", entry)
			}
			if env.count("自动换") != 0 || env.count(fmt.Sprintf("IP 池固定端口暂停使用 | 端口=%d | 原因=冷却中（连续出口连不通）", port)) != 1 {
				t.Fatalf("日志应为原文：%v", proxyPoolLogLines(env.registry))
			}
			if data, _ := json.Marshal(env.pool.status()); strings.Contains(string(data), `"auto"`) {
				t.Fatalf("状态不应有 auto：%s", data)
			}
			env.pool.flush()
			if after := env.fixedRecordText(); after != before {
				t.Fatalf("固定表文件不应改变：\n%s\n---\n%s", before, after)
			}
			checkFixedInvariants(t, env)
		})
	}
	// 从没开过固定分配：上限为 0 时不出现固定表文件
	env := newFixedAutoEnv(t, "", 0, staticProxyPool(10001, 10002), proxyPoolTestIDs(2)...)
	env.pool.flush()
	if _, err := os.Stat(filepath.Join(env.dir, proxyPoolFixedFileName)); !os.IsNotExist(err) {
		t.Fatal("从没开过时不应出现固定表文件")
	}
}

// TestFixedAutoReplaceUpstream A2/A20：规则 B 冷却时端口上的账户按 ID 升序逐个挪到负载最少的端口（不挤到同一端口），端口停用并记下自动换过，
// 条目来源 auto，每个账户一行 L1、汇总一行 L2；没有浏览器的账户绑定立即是新端口、没有暂停；写盘带 auto；日志、状态、文件不含账号密码
func TestFixedAutoReplaceUpstream(t *testing.T) {
	ids := proxyPoolTestIDs(12)
	env := newFixedAutoEnv(t, "", 10, fixedStaticPool(10001, 10004), ids...)
	notifies := notifyCounter(env.pool)
	port := 10001
	users := env.usersOf(port)
	if len(users) != 3 {
		t.Fatalf("每端口应 3 个账户：%v", users)
	}
	env.coolPort(port)
	targets := make(map[int]bool)
	for _, id := range users {
		moved := env.port(id)
		if moved == port || targets[moved] {
			t.Fatalf("%s 应挪到不同的其他端口：%d（已用 %v）", id, moved, targets)
		}
		targets[moved] = true
		binding := env.owner.binding(id)
		if binding == nil || binding.URL != staticPortURL(t, moved) || binding.Hold != "" || binding.Active {
			t.Fatalf("没有浏览器的 %s 应立即用新端口：%s", id, fixedBindingText(binding))
		}
		env.pool.mu.Lock()
		source := env.pool.fixed.entries[id].source
		env.pool.mu.Unlock()
		if source != fixedSourceAuto {
			t.Fatalf("条目来源 = %s", source)
		}
		if env.count(fmt.Sprintf("INFO %s IP 池固定分配换端口 | 端口=%d → %d | 原因=自动换 IP（连续出口连不通）", id, port, moved)) != 1 {
			t.Fatalf("应写 L1：%v", proxyPoolLogLines(env.registry))
		}
	}
	if !env.excluded(port) || env.replaced()[port] != (proxyPoolFixedReplaced{at: env.clock.Now(), rule: proxyPoolRuleUpstream, moved: 3}) {
		t.Fatalf("端口应停用并记下自动换过：%v %+v", env.excluded(port), env.replaced())
	}
	if env.count("WARN service IP 池固定端口自动换 IP | 端口=10001 | 原因=连续出口连不通 | 挪动=3 | 挪到=10002、10003、10004 | 30 分钟内坏掉=1/10 | 这个端口不再分配（可在 IP 池设置里恢复），用它的 Worker 在进行中的请求结束后关闭") != 1 {
		t.Fatalf("应写 L2：%v", proxyPoolLogLines(env.registry))
	}
	auto := env.autoStatus()
	if auto == nil || auto.Limit != 10 || auto.WindowMinutes != 30 || auto.Recent != 1 || auto.Halted || auto.ReplacedTotal != 1 ||
		len(auto.Replaced) != 1 || auto.Replaced[0].Port != port || auto.Replaced[0].Rule != proxyPoolRuleUpstream || auto.Replaced[0].Moved != 3 {
		t.Fatalf("状态 = %+v", auto)
	}
	if notifies.Load() == 0 {
		t.Fatal("自动换后应推送账户列表")
	}
	if err := fixedLogsDrained(env.pool); err != nil {
		t.Fatal(err)
	}
	env.pool.flush()
	text := env.fixedRecordText()
	var record proxyPoolFixedRecord
	if err := json.Unmarshal([]byte(text), &record); err != nil || record.Auto == nil || len(record.Auto.Ports) != 1 ||
		record.Auto.Replaced["10001"].Moved != 3 || record.Excluded["10001"].IsZero() || record.Accounts[users[0]].Source != fixedSourceAuto {
		t.Fatalf("固定表文件 = %s (%v)", text, err)
	}
	checkFixedInvariants(t, env)
	assertNoPoolSecretInLogs(t, env.registry)
	assertNoPoolSecret(t, "固定表文件", text)
	assertNoPoolSecret(t, "状态", mustJSON(t, env.pool.status()))
	for _, forbidden := range []string{"user-static", "isp.example.com:"} {
		if strings.Contains(text, forbidden) || strings.Contains(mustJSON(t, env.pool.status()), forbidden) {
			t.Fatalf("不应出现 %q", forbidden)
		}
	}
}

// TestFixedAutoReplaceRateLimited A3/A4：规则 A 冷却同样自动换，原因写“不同账号返回限流”；撤回的账号照旧（钩子参数）；被挪账户已启动的 Worker 进
// cooling.Workers（钩子关闭它们），绑定仍是旧端口、带自动换的暂停，Worker 关闭后解除并推送账户列表
func TestFixedAutoReplaceRateLimited(t *testing.T) {
	ids := proxyPoolTestIDs(8)
	env := newFixedAutoEnv(t, "", 10, fixedStaticPool(10001, 10004), ids...)
	cooled := cooledHook(env.pool)
	port := 10002
	users := env.usersOf(port)
	closers := make(map[string]*fakeProxyCloser)
	for _, id := range users {
		_, closers[id] = env.worker(id)
	}
	markProxyPoolProven(env.pool, users[0])
	env.pool.rateLimited(users[0], false)
	env.pool.rateLimited(users[1], false)
	if len(*cooled) != 1 {
		t.Fatalf("应冷却一次：%+v", *cooled)
	}
	cooling := (*cooled)[0]
	if cooling.Rule != proxyPoolRuleRateLimited || !cooling.Fixed || cooling.Replaced != 2 ||
		!slices.Equal(cooling.RateLimitedAccounts, users) || !slices.Equal(cooling.Workers, users) {
		t.Fatalf("钩子参数 = %+v", cooling)
	}
	notifies := notifyCounter(env.pool)
	for _, id := range users {
		moved := env.port(id)
		if moved == port || env.count(fmt.Sprintf("IP 池固定分配换端口 | 端口=%d → %d | 原因=自动换 IP（不同账号返回限流）", port, moved)) != 1 {
			t.Fatalf("%s 应挪走并写原因：%d", id, moved)
		}
		binding := env.owner.binding(id)
		want := fmt.Sprintf("IP 池固定端口 %d 已自动换 IP，这个账户改用端口 %d；用旧端口的 Worker 关闭后恢复", port, moved)
		if binding == nil || binding.URL != staticPortURL(t, port) || !binding.Active || binding.Hold != want {
			t.Fatalf("旧端口上的 Worker 关闭前应带自动换的暂停：%s", fixedBindingText(binding))
		}
	}
	checkFixedInvariants(t, env)
	closers[users[0]].close()
	binding := env.owner.binding(users[0])
	if binding == nil || binding.URL != staticPortURL(t, env.port(users[0])) || binding.Hold != "" || notifies.Load() == 0 {
		t.Fatalf("Worker 关闭后应改用新端口、解除暂停并推送账户列表：%s（推送 %d）", fixedBindingText(binding), notifies.Load())
	}
	if env.count("原因=自动换 IP（不同账号返回限流）") != 2 || env.count("IP 池固定端口自动换 IP | 端口=10002 | 原因=不同账号返回限流 | 挪动=2") != 1 {
		t.Fatalf("日志：%v", proxyPoolLogLines(env.registry))
	}
	checkFixedInvariants(t, env)
}

// TestFixedAutoWorkersAndTransitional A4/A5：被挪账户已启动的 Worker 与端口上别的账户的过渡 Worker 都进 cooling.Workers；挪动时正在旧端口上启动
// 的 Worker（已申请、没有 attach）让绑定带自动换的暂停，attach 后仍带，归还后解除
func TestFixedAutoWorkersAndTransitional(t *testing.T) {
	ids := proxyPoolTestIDs(12)
	env := newFixedAutoEnv(t, "", 10, fixedStaticPool(10001, 10004), ids...)
	cooled := cooledHook(env.pool)
	port := 10001
	users := env.usersOf(port)
	failing, serving, starting := users[0], users[1], users[2]
	env.worker(serving)
	// 过渡 Worker：另一个账户的 Worker 在这个端口上运行，固定端口在别处（先临时分到这个端口启动 Worker，再改回原端口）
	other := env.usersOf(10004)[0]
	env.pool.mu.Lock()
	env.pool.fixedSetLocked(other, port, env.pool.now(), fixedSourceRebalance)
	env.pool.fixed.logs = nil
	env.pool.mu.Unlock()
	transitional := env.pool.acquire(other, other, proxyPoolKindWorker, other, env.global)
	transitional.attach(&fakeProxyCloser{})
	env.pool.mu.Lock()
	env.pool.fixedSetLocked(other, 10004, env.pool.now(), fixedSourceRebalance)
	env.pool.pushLocked(other)
	env.pool.fixed.logs = nil
	env.pool.mu.Unlock()
	if leasePort(t, transitional) != port {
		t.Fatalf("过渡 Worker 应在 %d 上：%d", port, leasePort(t, transitional))
	}
	startingLease := env.pool.acquire(starting, starting, proxyPoolKindWorker, starting, env.global)
	env.coolUpstream(failing)
	if len(*cooled) != 1 {
		t.Fatalf("应冷却一次：%+v", *cooled)
	}
	workers := (*cooled)[0].Workers
	sort.Strings(workers)
	want := []string{other, serving}
	sort.Strings(want)
	if !slices.Equal(workers, want) || (*cooled)[0].Replaced != 3 {
		t.Fatalf("cooling.Workers 应含被挪账户的 Worker 与过渡 Worker：%v（挪动 %d）", workers, (*cooled)[0].Replaced)
	}
	if hold := env.owner.binding(starting).Hold; !strings.Contains(hold, "已自动换 IP") || hold != env.expectedHold(starting) {
		t.Fatalf("启动中的 Worker 在旧端口上时应带自动换的暂停：%q", hold)
	}
	startingLease.attach(&fakeProxyCloser{})
	if hold := env.owner.binding(starting).Hold; !strings.Contains(hold, "已自动换 IP") {
		t.Fatalf("attach 后仍应带：%q", hold)
	}
	checkFixedInvariants(t, env)
	startingLease.release()
	if binding := env.owner.binding(starting); binding.Hold != "" || binding.URL != staticPortURL(t, env.port(starting)) {
		t.Fatalf("归还后应解除：%s", fixedBindingText(binding))
	}
	// 过渡 Worker 的账户同样在坏 IP 上：带自动换的暂停，Worker 关闭后解除
	if binding := env.owner.binding(other); !strings.Contains(binding.Hold, "已自动换 IP") || binding.URL != staticPortURL(t, port) {
		t.Fatalf("过渡 Worker 的账户也应带自动换的暂停：%s", fixedBindingText(binding))
	}
	transitional.release()
	if binding := env.owner.binding(other); binding.Hold != "" || binding.URL != staticPortURL(t, 10004) {
		t.Fatalf("过渡 Worker 关闭后应回到固定端口：%s", fixedBindingText(binding))
	}
	checkFixedInvariants(t, env)
}

// TestFixedAutoWindowHalt A6/A22：30 分钟内 10 个端口依次坏 → 全部自动换；第 11 个 → 不换、按原规则暂停、L3、熔断（halted_count=11、
// halted_reason=limit、skipped=0：skipped 只数停止之后又坏的，不含触发停止的第 11 个）；第 12 个 → L4、skipped=1；第 11 个冷却到期后再坏仍不算
// “之后又坏”；前 10 个仍停用、账户不回去
func TestFixedAutoWindowHalt(t *testing.T) {
	ids := proxyPoolTestIDs(40)
	env := newFixedAutoEnv(t, "", 10, fixedStaticPool(10001, 10040), ids...)
	for _, id := range ids {
		env.worker(id)
	}
	var replacedPorts []int
	for index := range 10 {
		port := env.healthyPortWithUsers()
		env.coolPort(port)
		if !env.excluded(port) {
			t.Fatalf("第 %d 个坏端口 %d 应自动换", index+1, port)
		}
		if env.count(fmt.Sprintf("IP 池固定端口自动换 IP | 端口=%d |", port)) != 1 || env.count(fmt.Sprintf("| 30 分钟内坏掉=%d/10 |", index+1)) != 1 {
			t.Fatalf("第 %d 个应写 L2 带计数：%v", index+1, proxyPoolLogLines(env.registry))
		}
		replacedPorts = append(replacedPorts, port)
		env.clock.Advance(30 * time.Second)
	}
	notifies := notifyCounter(env.pool)
	eleventh := env.healthyPortWithUsers()
	stayed := env.usersOf(eleventh)
	env.coolPort(eleventh)
	if env.excluded(eleventh) || !slices.Equal(env.usersOf(eleventh), stayed) {
		t.Fatal("第 11 个不应自动换")
	}
	if env.count(fmt.Sprintf("WARN service IP 池坏 IP 过多，已停止自动换 IP，请手动处理 | 30 分钟内坏掉=11（上限 10） | 端口=%d 按暂停处理 | 已自动换过的不撤回；处理后在 IP 池设置里点“恢复自动换 IP”", eleventh)) != 1 {
		t.Fatalf("应写 L3：%v", proxyPoolLogLines(env.registry))
	}
	if env.count(fmt.Sprintf("WARN service IP 池固定端口暂停使用 | 端口=%d | 原因=冷却中（连续出口连不通）", eleventh)) != 1 {
		t.Fatal("第 11 个应按原规则暂停（原 WARN）")
	}
	for _, id := range stayed {
		if hold := env.owner.binding(id).Hold; !strings.HasSuffix(hold, "；固定分配不换端口") {
			t.Fatalf("第 11 个端口的账户应按原规则暂停：%q", hold)
		}
	}
	auto := env.autoStatus()
	if auto == nil || !auto.Halted || auto.HaltedCount != 11 || auto.HaltedReason != fixedHaltLimit || auto.Skipped != 0 || auto.HaltedAt.IsZero() || auto.ReplacedTotal != 10 || notifies.Load() == 0 {
		t.Fatalf("熔断状态 = %+v（推送 %d）", auto, notifies.Load())
	}
	if entry := env.pausedEntry(eleventh); entry == nil || entry.Auto != fixedAutoHalted {
		t.Fatalf("暂停记录应带 halted：%+v", entry)
	}
	twelfth := env.healthyPortWithUsers()
	env.coolPort(twelfth)
	if env.excluded(twelfth) || env.count(fmt.Sprintf("INFO service IP 池自动换 IP 已停止（坏 IP 过多），这个端口按暂停处理 | 端口=%d | 原因=连续出口连不通 | 账户=1 | 停止后又坏=1", twelfth)) != 1 {
		t.Fatalf("第 12 个应写 L4：%v", proxyPoolLogLines(env.registry))
	}
	if auto := env.autoStatus(); auto.Skipped != 1 || auto.Recent != 12 {
		t.Fatalf("skipped = %+v", auto)
	}
	if entry := env.pausedEntry(twelfth); entry == nil || entry.Auto != fixedAutoHalted {
		t.Fatalf("停止后又坏的端口应带 halted：%+v", entry)
	}
	// 触发停止的第 11 个冷却到期后再坏：同一个端口，不算“停止之后又坏”
	env.clock.Advance(16 * time.Minute)
	env.pool.recheckFired()
	env.coolPort(eleventh)
	if auto := env.autoStatus(); auto.Skipped != 1 || env.excluded(eleventh) {
		t.Fatalf("触发停止的端口再坏不应计入 skipped：%+v", auto)
	}
	for _, port := range replacedPorts {
		if !env.excluded(port) || len(env.usersOf(port)) != 0 {
			t.Fatalf("已经自动换过的 %d 不应撤回", port)
		}
	}
	checkFixedInvariants(t, env)
}

// TestFixedAutoWindowBoundary A7：t0..t0+9 分钟坏 10 个；t0+30 分钟正好时第一个已不在窗口（<）→ 第 11 个照常自动换；t0+29m59s 时熔断
func TestFixedAutoWindowBoundary(t *testing.T) {
	for _, test := range []struct {
		name   string
		offset time.Duration
		halted bool
	}{
		{name: "30m", offset: 30 * time.Minute, halted: false},
		{name: "29m59s", offset: 30*time.Minute - time.Second, halted: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			ids := proxyPoolTestIDs(40)
			env := newFixedAutoEnv(t, "", 10, fixedStaticPool(10001, 10040), ids...)
			for _, id := range ids {
				env.worker(id)
			}
			start := env.clock.Now()
			for index := range 10 {
				env.coolPort(env.healthyPortWithUsers())
				if index < 9 {
					env.clock.Advance(time.Minute)
				}
			}
			env.clock.Advance(start.Add(test.offset).Sub(env.clock.Now()))
			port := env.healthyPortWithUsers()
			env.coolPort(port)
			if env.excluded(port) == test.halted || env.autoStatus().Halted != test.halted {
				t.Fatalf("偏移 %s：excluded=%v halted=%v", test.offset, env.excluded(port), env.autoStatus().Halted)
			}
			if !test.halted && env.count("| 30 分钟内坏掉=10/10 |") != 2 {
				t.Fatalf("窗口外的第一个应剪掉：%v", proxyPoolLogLines(env.registry))
			}
		})
	}
}

// TestFixedAutoNoTarget A8/A9：其余端口都被锁住时不挪、不停用、计入窗口、L5、暂停（原 Hold）、paused[].auto=no_target；冷却到期又坏仍没有目标时
// 同一端口只算一个；目标恢复后再坏 → 自动换并去掉 no_target
func TestFixedAutoNoTarget(t *testing.T) {
	ids := proxyPoolTestIDs(3)
	env := newFixedAutoEnv(t, "", 10, fixedStaticPool(10001, 10003), ids...)
	port := 10001
	victim := env.usersOf(port)[0]
	for _, other := range []int{10002, 10003} {
		env.locks.setOther(other, true)
		if lease, _ := env.worker(env.usersOf(other)[0]); lease.Refused() == nil {
			t.Fatal("被其他进程锁住的端口应拒绝")
		}
	}
	if auto := env.autoStatus(); auto == nil || auto.Recent != 0 || env.count("自动换") != 0 {
		t.Fatalf("被其他进程占用不算坏：%+v", auto)
	}
	env.coolPort(port)
	if env.excluded(port) || env.port(victim) != port {
		t.Fatal("没有可挪的端口时不应挪、不应停用")
	}
	if env.count("WARN service IP 池固定端口没有自动换 IP：没有可挪的端口（其余端口都暂停或停用），这个端口上的账户暂停使用 | 端口=10001 | 原因=连续出口连不通 | 账户=1 | 30 分钟内坏掉=1/10") != 1 {
		t.Fatalf("应写 L5：%v", proxyPoolLogLines(env.registry))
	}
	if entry := env.pausedEntry(port); entry == nil || entry.Auto != fixedAutoNoTarget || entry.Reason != fixedPauseCoolingUpstream {
		t.Fatalf("暂停记录应带 no_target：%+v", entry)
	}
	if hold := env.owner.binding(victim).Hold; !strings.HasSuffix(hold, "；固定分配不换端口") {
		t.Fatalf("应按原规则暂停：%q", hold)
	}
	// 冷却到期：no_target 去掉；仍没有目标时再坏，同一端口窗口内只算一个
	env.clock.Advance(16 * time.Minute)
	env.pool.recheckFired()
	if entry := env.pausedEntry(port); entry != nil {
		t.Fatalf("冷却到期后应恢复：%+v", entry)
	}
	env.pool.mu.Lock()
	_, stale := env.pool.fixed.auto.noTarget[port]
	env.pool.mu.Unlock()
	if stale {
		t.Fatal("端口恢复后应去掉 no_target 记录")
	}
	env.coolPort(port)
	if auto := env.autoStatus(); auto.Recent != 1 || env.count("没有可挪的端口") != 2 || env.count("| 30 分钟内坏掉=1/10") != 2 {
		t.Fatalf("同一端口窗口内只算一个：%+v %v", auto, proxyPoolLogLines(env.registry))
	}
	// 目标恢复后再坏：自动换
	env.locks.setOther(10002, false)
	env.clock.Advance(31 * time.Minute)
	env.pool.recheckFired()
	env.coolPort(port)
	if !env.excluded(port) || env.port(victim) != 10002 {
		t.Fatalf("目标恢复后应自动换：excluded=%v port=%d", env.excluded(port), env.port(victim))
	}
	env.pool.mu.Lock()
	_, stale = env.pool.fixed.auto.noTarget[port]
	env.pool.mu.Unlock()
	if stale {
		t.Fatal("自动换后应去掉 no_target")
	}
	checkFixedInvariants(t, env)
}

// TestFixedAutoAvoidsLocked A10：被其他进程锁住的端口暂停、不计数、不写自动换日志，也不是自动换的目标
func TestFixedAutoAvoidsLocked(t *testing.T) {
	ids := proxyPoolTestIDs(12)
	env := newFixedAutoEnv(t, "", 10, fixedStaticPool(10001, 10004), ids...)
	env.locks.setOther(10004, true)
	if lease, _ := env.worker(env.usersOf(10004)[0]); lease.Refused() == nil {
		t.Fatal("应拒绝")
	}
	if auto := env.autoStatus(); auto.Recent != 0 || env.count("自动换") != 0 || env.pausedEntry(10004) == nil || env.pausedEntry(10004).Auto != "" {
		t.Fatalf("被其他进程占用不算坏：%+v %+v", auto, env.pausedEntry(10004))
	}
	users := env.usersOf(10001)
	env.coolPort(10001)
	for _, id := range users {
		if port := env.port(id); port == 10004 || port == 10001 {
			t.Fatalf("%s 不应挪到被锁住的端口：%d", id, port)
		}
	}
	if env.autoStatus().Recent != 1 {
		t.Fatal("只计规则 B 的冷却")
	}
}

// TestFixedAutoSystemicProtection A11：整体故障保护拦下的冷却不发生：不计数、不自动换、原 WARN
func TestFixedAutoSystemicProtection(t *testing.T) {
	ids := proxyPoolTestIDs(12)
	env := newFixedAutoEnv(t, "", 10, fixedStaticPool(10001, 10006), ids...)
	env.pool.mu.Lock()
	for port := 10001; port <= 10006; port++ {
		env.pool.portLocked(port).upstreamFails = 1
	}
	env.pool.mu.Unlock()
	lease := env.pool.acquire(ids[0], ids[0], proxyPoolKindWorker, ids[0], env.global)
	port := leasePort(t, lease)
	lease.launchFailed(upstreamFailure, false)
	if env.excluded(port) || env.autoStatus().Recent != 0 || env.count("IP 池多数端口同时出错，按上游或代理商整体问题处理，暂不冷却端口") != 1 || env.count("自动换") != 0 {
		t.Fatalf("整体故障保护拦下时不应自动换：%v", proxyPoolLogLines(env.registry))
	}
}

// TestFixedAutoResume A12/A19：熔断后恢复 → halted=false、窗口清空、L8、立即写盘；之后再坏 → 自动换；没有熔断时 resumed=false、不写日志。
// 端口段用 8 个端口：4 个端口时恢复后的第 3 个坏端口（1 个停用、1 个冷却）按相对门槛再次熔断（见 TestFixedAutoHalfHalt）
func TestFixedAutoResume(t *testing.T) {
	ids := proxyPoolTestIDs(16)
	env := newFixedAutoEnv(t, "", 1, fixedStaticPool(10001, 10008), ids...)
	if result, err := env.pool.fixedAutoResume(); err != nil || result.Resumed {
		t.Fatalf("没有熔断时 = %+v (%v)", result, err)
	}
	env.coolPort(10001)
	env.clock.Advance(time.Minute)
	halting := env.healthyPortWithUsers()
	env.coolPort(halting)
	haltedAt := env.clock.Now()
	if auto := env.autoStatus(); !auto.Halted || auto.HaltedCount != 2 {
		t.Fatalf("应熔断：%+v", auto)
	}
	env.pool.flush()
	if !strings.Contains(env.fixedRecordText(), `"halted_at"`) {
		t.Fatal("熔断应写盘")
	}
	env.clock.Advance(time.Minute)
	result, err := env.pool.fixedAutoResume()
	if err != nil || !result.Resumed || !result.HaltedAt.Equal(haltedAt) || result.Skipped != 0 {
		t.Fatalf("恢复 = %+v (%v)", result, err)
	}
	if err := fixedLogsDrained(env.pool); err != nil {
		t.Fatal(err)
	}
	if text := env.fixedRecordText(); strings.Contains(text, `"halted_at"`) || strings.Contains(text, `"ports"`) {
		t.Fatalf("恢复应立即写盘并清空窗口：\n%s", text)
	}
	if env.count(fmt.Sprintf("INFO service IP 池已恢复自动换坏 IP（管理页） | 停止于=%s | 停止后又坏=0（仍按暂停处理，需要时手动换 IP） | 30 分钟计数从 0 开始", haltedAt.UTC().Format(time.RFC3339))) != 1 {
		t.Fatalf("应写 L8：%v", proxyPoolLogLines(env.registry))
	}
	if auto := env.autoStatus(); auto.Halted || auto.Recent != 0 || auto.Skipped != 0 || env.excluded(halting) {
		t.Fatalf("恢复后 = %+v；此刻暂停的端口不补做", auto)
	}
	port := env.healthyPortWithUsers()
	env.coolPort(port)
	if !env.excluded(port) || env.count("| 30 分钟内坏掉=1/1 |") != 2 {
		t.Fatalf("恢复后再坏应自动换：%v", proxyPoolLogLines(env.registry))
	}
	if again, _ := env.pool.fixedAutoResume(); again.Resumed || env.count("IP 池已恢复自动换坏 IP") != 1 {
		t.Fatal("没有熔断时不应再写 L8")
	}
	// 固定分配与固定会话都没开时 409
	env.pool.configure(staticProxyPool(10001, 10004), env.global, 8)
	var operation *adminOperationError
	if _, err := env.pool.fixedAutoResume(); !errors.As(err, &operation) || operation.code != "proxy_pool_not_fixed" || operation.status != http.StatusConflict {
		t.Fatalf("没开时：%v", err)
	}
}

// TestFixedAutoRestart A13：重启后熔断保留、窗口内记录保留（超出窗口的剪掉）、自动换过的端口只保留仍停用的；第一次生效时上限大于 0 且熔断写 L7
func TestFixedAutoRestart(t *testing.T) {
	ids := proxyPoolTestIDs(8)
	env := newFixedAutoEnv(t, "", 1, fixedStaticPool(10001, 10004), ids...)
	env.coolPort(10001)
	env.clock.Advance(20 * time.Minute)
	halting := env.healthyPortWithUsers()
	env.coolPort(halting)
	env.pool.flush()
	data := env.fixedRecordText()
	load := func(limit int, offset time.Duration, text string) (*proxyPool, *requestRegistry) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, proxyPoolFixedFileName), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
		clock := newProxyPoolTestClock()
		clock.Advance(offset)
		pool, registry, _ := newFixedPool(t, dir, clock, nil)
		pool.setFixedAutoReplace(limit)
		pool.configure(fixedStaticPool(10001, 10004), proxyPoolTestGlobal, 8)
		return pool, registry
	}
	pool, registry := load(1, 35*time.Minute, data)
	pool.mu.Lock()
	auto := pool.fixed.auto
	_, keptFirst := auto.ports[10001]
	_, keptSecond := auto.ports[halting]
	_, replaced := auto.replaced[10001]
	pool.mu.Unlock()
	if !auto.halted() || auto.haltedCount != 2 || keptFirst || !keptSecond || !replaced {
		t.Fatalf("重启后 = halted %v count %d first %v second %v replaced %v", auto.halted(), auto.haltedCount, keptFirst, keptSecond, replaced)
	}
	if countProxyPoolLogs(registry, "WARN service IP 池自动换坏 IP 仍处于停止状态（2026-10-23T08:20:00Z 因坏 IP 过多停止），请在 IP 池设置里处理后点“恢复自动换 IP”") != 1 {
		t.Fatalf("启动应写 L7：%v", proxyPoolLogLines(registry))
	}
	if _, quiet := load(0, 35*time.Minute, data); countProxyPoolLogs(quiet, "仍处于停止状态") != 0 {
		t.Fatal("上限为 0 时不写 L7")
	}
	// 自动换过的端口已不在停用里：载入时丢掉
	var record proxyPoolFixedRecord
	if err := json.Unmarshal([]byte(data), &record); err != nil {
		t.Fatal(err)
	}
	delete(record.Excluded, "10001")
	edited, _ := json.Marshal(record)
	pool, _ = load(1, 35*time.Minute, string(edited))
	pool.mu.Lock()
	_, replaced = pool.fixed.auto.replaced[10001]
	pool.mu.Unlock()
	if replaced {
		t.Fatal("不再停用的端口不应留在自动换过的记录里")
	}
}

// TestFixedAutoHotLimit A14：热更新上限：10→0 之后坏的暂停、状态没有 auto；0→10 自动换；改小到不超过窗口内个数时下一个坏就熔断；熔断不随改值解除
func TestFixedAutoHotLimit(t *testing.T) {
	ids := proxyPoolTestIDs(16)
	env := newFixedAutoEnv(t, "", 10, fixedStaticPool(10001, 10008), ids...)
	if env.pool.setFixedAutoReplace(10) {
		t.Fatal("值不变时不算变化")
	}
	if !env.pool.setFixedAutoReplace(0) {
		t.Fatal("改值应返回变化")
	}
	paused := env.healthyPortWithUsers()
	env.coolPort(paused)
	if env.excluded(paused) || env.autoStatus() != nil {
		t.Fatal("改成 0 后坏的应暂停、状态没有 auto")
	}
	env.pool.setFixedAutoReplace(10)
	replaced := env.healthyPortWithUsers()
	env.coolPort(replaced)
	if !env.excluded(replaced) || env.autoStatus().Recent != 1 {
		t.Fatal("改回 10 后应自动换（上限为 0 时坏的不计数）")
	}
	env.pool.setFixedAutoReplace(1)
	halting := env.healthyPortWithUsers()
	env.coolPort(halting)
	if env.excluded(halting) || !env.autoStatus().Halted {
		t.Fatal("改小后下一个坏就熔断")
	}
	env.pool.setFixedAutoReplace(10)
	if !env.autoStatus().Halted {
		t.Fatal("熔断不随改值解除")
	}
}

// TestFixedAutoManualOps A15：手动换 IP、重新平均、恢复不计数、不熔断；恢复自动换过的端口 → 从 replaced 去掉，还在它上面的旧 Worker 的账户暂停解除
func TestFixedAutoManualOps(t *testing.T) {
	ids := proxyPoolTestIDs(12)
	env := newFixedAutoEnv(t, "", 1, fixedStaticPool(10001, 10004), ids...)
	users := env.usersOf(10001)
	env.worker(users[1])
	env.coolUpstream(users[0])
	if !env.excluded(10001) || !strings.Contains(env.owner.binding(users[1]).Hold, "已自动换 IP") {
		t.Fatal("应自动换并带暂停")
	}
	if _, _, err := env.pool.fixedReassign(10002, false); err != nil {
		t.Fatal(err)
	}
	if _, err := env.pool.fixedRebalance(nil, false); err != nil {
		t.Fatal(err)
	}
	if auto := env.autoStatus(); auto.Recent != 1 || auto.Halted || auto.ReplacedTotal != 1 {
		t.Fatalf("手动操作不计数、不熔断：%+v", auto)
	}
	result, err := env.pool.fixedRestore(10001)
	if err != nil || !result.Restored {
		t.Fatalf("恢复 = %+v (%v)", result, err)
	}
	if _, ok := env.replaced()[10001]; ok || env.autoStatus().ReplacedTotal != 0 {
		t.Fatal("恢复后应从自动换过的端口里去掉")
	}
	if hold := env.owner.binding(users[1]).Hold; hold != "" {
		t.Fatalf("恢复后旧 Worker 的账户应解除暂停：%q", hold)
	}
	if auto := env.autoStatus(); auto.Recent != 1 || auto.Halted {
		t.Fatalf("恢复不计数：%+v", auto)
	}
	checkFixedInvariants(t, env)
}

// TestFixedAutoRangeChange A16：端口段变化：段外的自动换端口从 excluded、replaced、noTarget 去掉
func TestFixedAutoRangeChange(t *testing.T) {
	ids := proxyPoolTestIDs(8)
	env := newFixedAutoEnv(t, "", 10, fixedStaticPool(10001, 10004), ids...)
	env.coolPort(10004)
	env.pool.mu.Lock()
	env.pool.fixed.auto.noTarget[10004] = env.pool.now()
	env.pool.mu.Unlock()
	if !env.excluded(10004) {
		t.Fatal("应自动换")
	}
	env.pool.configure(fixedStaticPool(10001, 10003), env.global, 8)
	env.pool.mu.Lock()
	_, noTarget := env.pool.fixed.auto.noTarget[10004]
	env.pool.mu.Unlock()
	if _, ok := env.replaced()[10004]; ok || noTarget {
		t.Fatal("段外的端口应去掉自动换记录")
	}
	env.pool.mu.Lock()
	_, excluded := env.pool.fixed.excluded[10004]
	env.pool.mu.Unlock()
	if excluded {
		t.Fatal("段外的停用端口应去掉")
	}
}

// TestFixedAutoEmptyPort A17：没有固定账户的端口（只有过渡 Worker）冷却：不计数、不停用，过渡 Worker 照旧进 cooling.Workers
func TestFixedAutoEmptyPort(t *testing.T) {
	ids := proxyPoolTestIDs(8)
	env := newFixedAutoEnv(t, "", 10, fixedStaticPool(10001, 10004), ids...)
	port := 10004
	users := env.usersOf(port)
	env.worker(users[0])
	env.pool.mu.Lock()
	for _, id := range users {
		env.pool.fixedSetLocked(id, 10001, env.pool.now(), fixedSourceRebalance)
	}
	env.pool.pushLocked(users...)
	env.pool.fixed.logs = nil
	logs, cooled := env.pool.coolLocked(port, proxyPoolRuleUpstream, "测试", env.pool.now())
	logs = append(logs, env.pool.takeFixedLogsLocked()...)
	env.pool.mu.Unlock()
	env.pool.emit(logs)
	if len(cooled) != 1 || cooled[0].Replaced != 0 || !slices.Equal(cooled[0].Workers, users[:1]) {
		t.Fatalf("空端口冷却：%+v", cooled)
	}
	if env.excluded(port) || env.autoStatus().Recent != 0 || env.count("自动换") != 0 {
		t.Fatal("空端口不算坏、不停用")
	}
}

// TestFixedAutoOffNoEffect A18/M15：固定分配关闭时上限 10 不生效：开过又关（休眠的分配表里这个端口有账户）后冷却按原来处理（换端口重启，
// cooling.Fixed 为假），不挪、不停用、不计数，没有任何自动换状态
func TestFixedAutoOffNoEffect(t *testing.T) {
	ids := proxyPoolTestIDs(4)
	env := newFixedAutoEnv(t, "", 10, fixedStaticPool(10001, 10004), ids...)
	port := env.port(ids[0])
	env.pool.configure(staticProxyPool(10001, 10004), env.global, 8)
	cooled := cooledHook(env.pool)
	lease := env.pool.acquire(ids[0], ids[0], proxyPoolKindWorker, ids[0], env.global)
	lease.launchFailed(upstreamFailure, false)
	env.pool.mu.Lock()
	dormant := env.pool.fixedCountLocked(port)
	logs, direct := env.pool.coolLocked(port, proxyPoolRuleUpstream, "测试", env.pool.now())
	logs = append(logs, env.pool.takeFixedLogsLocked()...)
	env.pool.mu.Unlock()
	env.pool.emit(logs)
	env.pool.mu.Lock()
	empty := len(env.pool.fixed.auto.ports) == 0 && len(env.pool.fixed.excluded) == 0 && len(env.pool.fixed.auto.replaced) == 0
	env.pool.mu.Unlock()
	if dormant == 0 {
		t.Fatal("休眠的分配表里这个端口应有账户")
	}
	if len(direct) != 1 || direct[0].Fixed || direct[0].Replaced != 0 || !empty || len(*cooled) != 0 || env.port(ids[0]) != port {
		t.Fatalf("关闭时不应有自动换：%+v empty=%v", direct, empty)
	}
	if status := env.pool.status(); status.Fixed != nil || env.count("自动换") != 0 {
		t.Fatal("关闭时状态没有 fixed、不写自动换日志")
	}
}

// fixedDynamicAutoEnv 为动态固定会话、带坏会话自动换上限的环境
func fixedDynamicAutoEnv(t *testing.T, limit int, ids ...string) *fixedTestEnv {
	t.Helper()
	dynamic := dynamicProxyPool()
	dynamic.Fixed = true
	return newFixedAutoEnv(t, "", limit, dynamic, ids...)
}

// failSession 让账户连续两次出口连不通的启动失败（间隔 1 分钟）
func (env *fixedTestEnv) failSession(accountID string) {
	env.t.Helper()
	for index := range proxyPoolUpstreamFails {
		lease := env.pool.acquire(accountID, accountID, proxyPoolKindWorker, accountID, env.global)
		lease.launchFailed(upstreamFailure, false)
		if index == 0 {
			env.clock.Advance(time.Minute)
		}
	}
}

// generation 返回账户的会话代号
func (env *fixedTestEnv) generation(accountID string) int {
	env.pool.mu.Lock()
	defer env.pool.mu.Unlock()
	if session := env.pool.sessions[accountID]; session != nil {
		return session.generation
	}
	return 0
}

// stuckOf 返回账户的“会话连不通”记录
func (env *fixedTestEnv) stuckOf(accountID string) (proxyPoolFixedStuck, bool) {
	env.pool.mu.Lock()
	defer env.pool.mu.Unlock()
	stuck, ok := env.pool.fixed.auto.stuck[accountID]
	return stuck, ok
}

// TestFixedAutoDynamicZero D1：动态固定会话、上限为 0：两次出口连不通 → 原 WARN、不换；状态没有 auto
func TestFixedAutoDynamicZero(t *testing.T) {
	ids := proxyPoolTestIDs(2)
	env := fixedDynamicAutoEnv(t, 0, ids...)
	session := env.owner.binding(ids[0]).Slot
	env.failSession(ids[0])
	if env.generation(ids[0]) != 0 || env.count("IP 池换会话") != 0 || env.autoStatus() != nil ||
		env.count(fmt.Sprintf("WARN %s IP 池固定会话不换会话 | 会话=%s | 原因=连续 2 次出口连不通 | 代理商恢复这个会话前，这个账户的 Worker 启动会失败", ids[0], session)) != 1 {
		t.Fatalf("上限为 0 时与原来相同：%v", proxyPoolLogLines(env.registry))
	}
}

// TestFixedAutoDynamicRotate D2/D6：上限 10：两次出口连不通 → 换会话（原因带计数）；10 分钟内再两次 → 不换（间隔）；出口判定仍不换
func TestFixedAutoDynamicRotate(t *testing.T) {
	ids := proxyPoolTestIDs(2)
	env := fixedDynamicAutoEnv(t, 10, ids...)
	session := env.owner.binding(ids[0]).Slot
	env.failSession(ids[0])
	rotated := env.owner.binding(ids[0]).Slot
	if env.generation(ids[0]) != 1 || rotated == session ||
		env.count(fmt.Sprintf("WARN %s IP 池换会话 | 会话=%s → %s | 原因=连续 2 次出口连不通，固定会话自动换会话（30 分钟内第 1 个，上限 10） | 生效=立即（没有 Worker）", ids[0], session, rotated)) != 1 {
		t.Fatalf("应自动换会话：%v", proxyPoolLogLines(env.registry))
	}
	if _, stuck := env.stuckOf(ids[0]); stuck {
		t.Fatal("换会话后不应留连不通记录")
	}
	env.failSession(ids[0])
	if env.generation(ids[0]) != 1 || env.count("IP 池换会话 |") != 1 {
		t.Fatal("10 分钟内不应再换")
	}
	env.clock.Advance(proxyPoolRotateInterval + time.Minute)
	if rotateSessionForTest(env.pool, ids[1], "出口受限") {
		t.Fatal("固定会话下出口判定仍不换会话")
	}
	if auto := env.autoStatus(); auto == nil || auto.Recent != 1 || auto.Halted {
		t.Fatalf("状态 = %+v", auto)
	}
}

// TestFixedAutoDynamicHalt D3：上限 2：第 3 个不同账户 → L10、不换、stuck 有它；之后 L12（同账户 30 分钟一次）；skipped 只数停止之后又坏的会话
// （ids[3] 的同一会话再坏只算一个，不含触发停止的 ids[2]）
func TestFixedAutoDynamicHalt(t *testing.T) {
	ids := proxyPoolTestIDs(4)
	env := fixedDynamicAutoEnv(t, 2, ids...)
	for _, id := range ids[:2] {
		env.failSession(id)
	}
	session := env.owner.binding(ids[2]).Slot
	env.failSession(ids[2])
	if env.generation(ids[2]) != 0 || env.count("WARN service IP 池坏会话过多，已停止自动换会话，请手动处理 | 30 分钟内连不通的会话=3（上限 2） | 已自动换过的不撤回；可在 IP 池设置里给账户“换会话”，处理后点“恢复自动换会话”") != 1 {
		t.Fatalf("第 3 个应熔断：%v", proxyPoolLogLines(env.registry))
	}
	if stuck, ok := env.stuckOf(ids[2]); !ok || stuck.session != session {
		t.Fatalf("熔断的账户应记为会话连不通：%+v", stuck)
	}
	env.failSession(ids[3])
	warn := fmt.Sprintf("WARN %s IP 池固定会话不换会话 | 会话=%s | 原因=连续 2 次出口连不通 | 已停止自动换会话（坏会话过多），可在 IP 池设置里给这个账户换会话", ids[3], env.owner.binding(ids[3]).Slot)
	if env.generation(ids[3]) != 0 || env.count(warn) != 1 {
		t.Fatalf("熔断后应写 L12：%v", proxyPoolLogLines(env.registry))
	}
	env.clock.Advance(proxyPoolRotateInterval + time.Minute)
	env.failSession(ids[3])
	if env.count(warn) != 1 {
		t.Fatal("L12 同账户 30 分钟一次")
	}
	auto := env.autoStatus()
	if auto == nil || !auto.Halted || auto.HaltedCount != 3 || auto.Skipped != 1 || auto.StuckTotal != 2 || len(auto.Stuck) != 2 || auto.Stuck[0].AccountID != ids[2] {
		t.Fatalf("动态熔断状态 = %+v", auto)
	}
	// 动态下固定表从没迁移过（没有 since、没有条目）：熔断照样写盘，恢复后要改写文件去掉熔断（否则重启后又是停止状态）
	env.pool.flush()
	if text := env.fixedRecordText(); !strings.Contains(text, `"halted_at"`) || !strings.Contains(text, `"sessions"`) {
		t.Fatalf("动态熔断应写盘：\n%s", text)
	}
	result, err := env.pool.fixedAutoResume()
	if err != nil || !result.Resumed || env.count("INFO service IP 池已恢复自动换坏会话（管理页） | 停止于=") != 1 {
		t.Fatalf("动态恢复 = %+v (%v)", result, err)
	}
	if text := env.fixedRecordText(); strings.Contains(text, `"halted_at"`) || strings.Contains(text, `"sessions"`) || strings.Contains(text, `"auto"`) {
		t.Fatalf("动态恢复应立即改写文件、去掉熔断与计数：\n%s", text)
	}
}

// TestFixedAutoDynamicManual D4/D5/A19：手动换会话：代号 +1、绑定推送、stuck 清除、不计数；非固定会话 409、有自己代理 409、未登记 404；
// Worker 在连不通的会话上启动成功 → stuck 清除
func TestFixedAutoDynamicManual(t *testing.T) {
	const own = "http://203.0.113.50:3128"
	ids := proxyPoolTestIDs(3)
	dynamic := dynamicProxyPool()
	dynamic.Fixed = true
	env := newFixedAutoEnv(t, "", 0, dynamic, ids...)
	env.owner.add("own@example.com", own)
	env.pool.refresh(env.owner, "own@example.com")
	env.failSession(ids[0])
	if _, ok := env.stuckOf(ids[0]); !ok {
		t.Fatal("上限为 0 时也记下会话连不通（只在内存）")
	}
	before := env.owner.binding(ids[0]).Slot
	result, err := env.pool.fixedRotateSession(ids[0])
	if err != nil || result.AccountID != ids[0] || result.From != before || result.To == before || env.owner.binding(ids[0]).Slot != result.To || env.generation(ids[0]) != 1 {
		t.Fatalf("手动换会话 = %+v (%v)", result, err)
	}
	if _, ok := env.stuckOf(ids[0]); ok {
		t.Fatal("手动换会话后应清除连不通记录")
	}
	if err := fixedLogsDrained(env.pool); err != nil {
		t.Fatal(err)
	}
	env.pool.mu.Lock()
	counted := len(env.pool.fixed.auto.sessions)
	env.pool.mu.Unlock()
	if counted != 0 {
		t.Fatal("手动换会话不计数")
	}
	var operation *adminOperationError
	if _, err := env.pool.fixedRotateSession("own@example.com"); !errors.As(err, &operation) || operation.code != "proxy_pool_account_not_pooled" || operation.status != http.StatusConflict {
		t.Fatalf("有自己代理：%v", err)
	}
	if _, err := env.pool.fixedRotateSession("ghost@example.com"); !errors.As(err, &operation) || operation.code != "account_not_found" || operation.status != http.StatusNotFound {
		t.Fatalf("未登记：%v", err)
	}
	// D5：Worker 在连不通的会话上启动成功 → stuck 清除
	env.failSession(ids[1])
	if _, ok := env.stuckOf(ids[1]); !ok {
		t.Fatal("应记下连不通")
	}
	lease := env.pool.acquire(ids[1], ids[1], proxyPoolKindWorker, ids[1], env.global)
	lease.attach(&fakeProxyCloser{})
	if _, ok := env.stuckOf(ids[1]); ok {
		t.Fatal("会话通了应清除记录")
	}
	if !env.pool.servesSession(ids[1], lease.Slot()) || env.pool.servesSession(ids[1], "other") {
		t.Fatal("servesSession 应只认绑定所用的已启动 Worker 的会话")
	}
	env.pool.configure(fixedStaticPool(10001, 10002), env.global, 8)
	if _, err := env.pool.fixedRotateSession(ids[0]); !errors.As(err, &operation) || operation.code != "proxy_pool_not_fixed_dynamic" {
		t.Fatalf("非固定会话：%v", err)
	}
	assertNoPoolSecretInLogs(t, env.registry)
}

// TestFixedAutoConcurrency 上限 2 时 12 个协程随机申请、启动成功与失败（冷却、自动换、熔断）、裸限流、时钟前进与定时器、手动换 IP、恢复、
// 恢复自动换、重新平均、状态与写盘、热更新上限；每步之后待写日志已取走，最后不变量成立（含自动换的暂停说明）、日志不含账号密码
func TestFixedAutoConcurrency(t *testing.T) {
	ids := proxyPoolTestIDs(24)
	env := newFixedAutoEnv(t, "", 2, fixedStaticPool(10001, 10008), ids...)
	done := make(chan []*proxyPoolLease, 12)
	errs := make(chan error, 12)
	for worker := range 12 {
		go func(seed int) {
			random := rand.New(rand.NewSource(int64(seed)))
			var leases []*proxyPoolLease
			defer func() { done <- leases }()
			for range 120 {
				id := ids[random.Intn(len(ids))]
				switch random.Intn(14) {
				case 0, 1, 2:
					lease, _ := env.worker(id)
					leases = append(leases, lease)
				case 3, 4:
					lease := env.pool.acquire(id, id, proxyPoolKindWorker, id, env.global)
					lease.launchFailed(upstreamFailure, false)
				case 5:
					if len(leases) > 0 {
						index := random.Intn(len(leases))
						leases[index].release()
						leases = append(leases[:index], leases[index+1:]...)
					}
				case 6:
					env.pool.rateLimited(id, false)
				case 7:
					env.clock.Advance(time.Duration(random.Intn(10)) * time.Minute)
					env.pool.recheckFired()
				case 8:
					_, _, _ = env.pool.fixedReassign(10001+random.Intn(8), random.Intn(2) == 0)
				case 9:
					_, _ = env.pool.fixedRestore(10001 + random.Intn(8))
				case 10:
					_, _ = env.pool.fixedAutoResume()
				case 11:
					_ = env.pool.status()
					env.pool.flush()
				case 12:
					env.pool.setFixedAutoReplace(random.Intn(4))
				case 13:
					_, _ = env.pool.fixedRebalance(nil, random.Intn(2) == 0)
				}
				if err := fixedLogsDrained(env.pool); err != nil {
					errs <- err
					return
				}
			}
		}(worker)
	}
	var leases []*proxyPoolLease
	for range 12 {
		leases = append(leases, <-done...)
	}
	select {
	case err := <-errs:
		t.Fatal(err)
	default:
	}
	checkFixedInvariants(t, env)
	for _, lease := range leases {
		lease.release()
	}
	env.clock.Advance(3 * time.Hour)
	env.pool.recheckFired()
	checkFixedInvariants(t, env)
	env.pool.flush()
	assertNoPoolSecretInLogs(t, env.registry)
	assertNoPoolSecret(t, "固定表文件", env.fixedRecordText())
}
