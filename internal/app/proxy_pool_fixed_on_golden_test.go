package app

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/config"
)

// IP 池固定分配开启时的金样：静态 6 端口 9 账户首次迁移、Worker 申请与 attach、规则 B 冷却（暂停、Hold、钩子参数）、规则 A 冷却、
// 其他进程锁住、冷却到期恢复、手动换 IP 与恢复、重新平均、整体故障保护拦下、动态固定会话不换会话。每一步记下规范化日志、各账户绑定
// （含 Hold）、租约、状态接口 JSON、proxy-pool.json 与固定表文件文本（去掉 saved_at）、冷却钩子参数与账户列表推送次数，与
// testdata/proxy_pool_fixed_on_golden.json 逐字节比较。金样在加入“坏 IP 自动换”（PROXY_POOL_FIXED_AUTO_REPLACE）之前的代码上用
// -update 生成：设置为 0（或从不设置）时固定分配的全部可观察输出必须与它相同

const proxyPoolFixedOnGoldenFile = "proxy_pool_fixed_on_golden.json"

// proxyPoolFixedOnEnv 为固定分配开启金样的环境（在 proxyPoolGoldenEnv 上加冷却钩子、推送计数与固定表文件）
type proxyPoolFixedOnEnv struct {
	*proxyPoolGoldenEnv
	cooled   []string
	notifies int
}

// recordFixed 记下这一步：在 proxyPoolGoldenEnv.record 之外，绑定带 Hold，加上固定表文件文本、钩子参数与推送次数
func (env *proxyPoolFixedOnEnv) recordFixed(name string, extra ...string) {
	env.t.Helper()
	env.record(name, extra...)
	step := &env.steps[len(env.steps)-1]
	if env.owner != nil {
		step.Bindings = step.Bindings[:0]
		ids := env.owner.AccountIDs()
		sort.Strings(ids)
		for _, id := range ids {
			binding := env.owner.binding(id)
			if binding == nil {
				step.Bindings = append(step.Bindings, id+" nil")
				continue
			}
			step.Bindings = append(step.Bindings, fmt.Sprintf("%s url=%s slot=%s active=%v tag=%s hold=%q",
				id, config.RedactProxy(binding.URL), binding.Slot, binding.Active, binding.EgressTag, binding.Hold))
		}
	}
	for _, cooling := range env.cooled {
		step.Extra = append(step.Extra, "cooled "+cooling)
	}
	env.cooled = nil
	step.Extra = append(step.Extra, fmt.Sprintf("notifies=%d", env.notifies))
	env.notifies = 0
	if data, err := os.ReadFile(filepath.Join(env.dir, proxyPoolFixedFileName)); err == nil {
		step.Extra = append(step.Extra, "fixed_record="+proxyPoolGoldenSavedAt.ReplaceAllString(string(data), ""))
	} else if !os.IsNotExist(err) {
		env.t.Fatal(err)
	}
}

// fixedOnPortsWith 返回固定表里账户数为 count 的端口（升序）
func fixedOnPortsWith(pool *proxyPool, count int) []int {
	pool.mu.Lock()
	defer pool.mu.Unlock()
	var ports []int
	for port, users := range pool.fixed.byPort {
		if len(users) == count {
			ports = append(ports, port)
		}
	}
	sort.Ints(ports)
	return ports
}

// fixedOnUsers 返回端口上的固定账户（排序）
func fixedOnUsers(pool *proxyPool, port int) []string {
	pool.mu.Lock()
	defer pool.mu.Unlock()
	return pool.fixedUsersLocked(port)
}

// runProxyPoolFixedOnScenario 跑一遍固定分配开启的金样场景，返回编码后的 JSON 与各步名称；setup 不为 nil 时在第一次 configure 之前
// 调用（与 newRuntime 的装配顺序相同）
func runProxyPoolFixedOnScenario(t *testing.T, setup func(pool *proxyPool)) ([]byte, []proxyPoolGoldenStep) {
	t.Helper()
	dir := t.TempDir()
	env := &proxyPoolFixedOnEnv{proxyPoolGoldenEnv: newProxyPoolGoldenEnv(t, dir)}
	pool, clock, locks := env.pool, env.clock, env.locks
	pool.setPortCooledHook(func(cooling proxyPoolCooling) {
		env.cooled = append(env.cooled, fmt.Sprintf("port=%d until=%s level=%d rule=%s limited=%v holders=%v workers=%v fixed=%v",
			cooling.Port, cooling.Until.UTC().Format(time.RFC3339), cooling.Level, cooling.Rule, cooling.RateLimitedAccounts,
			cooling.Holders, cooling.Workers, cooling.Fixed))
	})
	pool.setAccountsChangedHook(func() { env.notifies++ })
	if setup != nil {
		setup(pool)
	}
	ids := proxyPoolTestIDs(9)
	owner := newFakeProxyOwner()
	accounts := make([]*aistudio.Account, 0, len(ids))
	for _, id := range ids {
		owner.add(id, "")
		accounts = append(accounts, &aistudio.Account{ID: id, Config: aistudio.AccountConfig{Enabled: true}})
	}

	// 首次迁移：configure → prepareFixed → prime → attach（与 newRuntime 相同）
	pool.configure(fixedStaticPool(10001, 10006), proxyPoolTestGlobal, 9)
	pool.prepareFixed(accounts)
	for _, account := range accounts {
		pool.prime(account)
	}
	env.owner = owner
	pool.attach(owner)
	env.recordFixed("enable")

	// Worker：两账户端口 P（规则 B）上先到的账户、两账户端口 Q（规则 A）上的两个账户、三号两账户端口 S 上的两个账户启动成功
	pairs := fixedOnPortsWith(pool, 2)
	singles := fixedOnPortsWith(pool, 1)
	if len(pairs) != 3 || len(singles) != 3 {
		t.Fatalf("9 个账户应分成 3 个两账户端口与 3 个一账户端口：%v %v", pairs, singles)
	}
	portP, portQ, portS, portR := pairs[0], pairs[1], pairs[2], singles[0]
	usersP, usersQ, usersS, usersR := fixedOnUsers(pool, portP), fixedOnUsers(pool, portQ), fixedOnUsers(pool, portS), fixedOnUsers(pool, portR)
	closers := make(map[string]*fakeProxyCloser)
	for _, id := range []string{usersP[0], usersQ[0], usersQ[1], usersS[0], usersS[1]} {
		lease := env.acquire("worker:"+id, id, id, proxyPoolKindWorker)
		closers[id] = &fakeProxyCloser{}
		lease.attach(closers[id])
	}
	env.acquire("verify:"+usersS[0], usersS[0], usersS[0], proxyPoolKindVerify)
	env.recordFixed("workers", fmt.Sprintf("P=%d Q=%d S=%d R=%d", portP, portQ, portS, portR))
	env.release("verify:" + usersS[0])

	// 规则 B：P 上另一个账户连续两次上游失败的启动
	failing := usersP[1]
	env.acquire("worker:fail1", failing, failing, proxyPoolKindWorker)
	env.launchFailed("worker:fail1", upstreamFailure)
	clock.Advance(time.Second)
	env.acquire("worker:fail2", failing, failing, proxyPoolKindWorker)
	env.launchFailed("worker:fail2", upstreamFailure)
	refused := pool.acquire(failing, failing, proxyPoolKindWorker, failing, proxyPoolTestGlobal)
	env.recordFixed("rule B", fmt.Sprintf("refused=%v", refused.Refused()),
		fmt.Sprintf("pausedError=%v", pool.fixedPausedError(failing)))

	// 规则 A：Q 上两个账户返回裸限流，其中一个近期在别处成功过
	clock.Advance(time.Second)
	markProxyPoolProven(pool, usersQ[0])
	pool.rateLimited(usersQ[0], false)
	pool.rateLimited(usersQ[1], false)
	env.recordFixed("rule A")

	// 被其他进程锁住：R 的账户申请 Worker 锁不到
	locks.setOther(portR, true)
	lockedOut := pool.acquire(usersR[0], usersR[0], proxyPoolKindWorker, usersR[0], proxyPoolTestGlobal)
	env.recordFixed("locked elsewhere", fmt.Sprintf("refused=%v", lockedOut.Refused()))

	// 冷却到期、其他进程释放：全部恢复
	locks.setOther(portR, false)
	clock.Advance(proxyPoolCoolBase + time.Minute)
	pool.recheckFired()
	env.recordFixed("recovered")

	// 手动换 IP（S 上两个账户都有 Worker）与恢复
	reassign, serving, err := pool.fixedReassign(portS, false)
	if err != nil {
		t.Fatal(err)
	}
	reassignJSON, _ := json.Marshal(reassign)
	env.recordFixed("reassign", "result="+string(reassignJSON), fmt.Sprintf("serving=%v", serving), "log="+fixedReassignLog(reassign))
	restore, err := pool.fixedRestore(portS)
	if err != nil {
		t.Fatal(err)
	}
	restoreJSON, _ := json.Marshal(restore)
	env.recordFixed("restore", "result="+string(restoreJSON))

	// 重新平均
	rebalance, err := pool.fixedRebalance(nil, false)
	if err != nil {
		t.Fatal(err)
	}
	rebalanceJSON, _ := json.Marshal(rebalance)
	env.recordFixed("rebalance", "result="+string(rebalanceJSON))

	// 整体故障保护：多数端口出错时不再冷却
	pool.mu.Lock()
	for port := 10001; port <= 10006; port++ {
		pool.portLocked(port).upstreamFails = 1
	}
	pool.mu.Unlock()
	systemic := usersR[0]
	env.acquire("worker:sys", systemic, systemic, proxyPoolKindWorker)
	env.launchFailed("worker:sys", upstreamFailure)
	pool.mu.Lock()
	for port := 10001; port <= 10006; port++ {
		pool.portLocked(port).upstreamFails = 0
	}
	pool.mu.Unlock()
	env.recordFixed("systemic protection")

	// 全部归还
	names := make([]string, 0, len(env.leases))
	for name := range env.leases {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		env.release(name)
	}
	for _, id := range []string{usersP[0], usersQ[0], usersQ[1], usersS[0], usersS[1]} {
		closers[id].close()
	}
	clock.Advance(3 * time.Hour)
	pool.recheckFired()
	env.recordFixed("all released")

	// 动态固定会话：连续两次出口连不通不换会话；出口判定也不换
	dynamic := dynamicProxyPool()
	dynamic.Fixed = true
	pool.configure(dynamic, proxyPoolTestGlobal, 9)
	env.recordFixed("dynamic fixed")
	env.acquire("worker:dyn", ids[0], ids[0], proxyPoolKindWorker)
	env.launchFailed("worker:dyn", upstreamFailure)
	clock.Advance(time.Minute)
	env.acquire("worker:dyn", ids[0], ids[0], proxyPoolKindWorker)
	env.launchFailed("worker:dyn", upstreamFailure)
	clock.Advance(proxyPoolRotateInterval + time.Minute)
	rotated := rotateSessionForTest(pool, ids[1], "出口受限")
	env.recordFixed("dynamic no rotate", fmt.Sprintf("rotated=%v", rotated))
	pool.configure(config.DefaultProxyPool(), proxyPoolTestGlobal, 9)
	env.recordFixed("off")

	data, err := json.MarshalIndent(env.steps, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	assertNoPoolSecret(t, "固定分配金样", string(data))
	if strings.Contains(string(data), "user-static") {
		t.Fatal("金样不应含代理用户名")
	}
	return append(data, '\n'), env.steps
}

// TestProxyPoolFixedOnGolden 固定分配开启、没有开启坏 IP 自动换时，固定分配场景的全部可观察输出与加入自动换之前的代码逐字节相同
func TestProxyPoolFixedOnGolden(t *testing.T) {
	path := filepath.Join("testdata", proxyPoolFixedOnGoldenFile)
	got, _ := runProxyPoolFixedOnScenario(t, nil)
	if again, _ := runProxyPoolFixedOnScenario(t, nil); string(again) != string(got) {
		t.Fatalf("同一场景跑两遍结果不同：%s", goldenDiff(got, again))
	}
	if *updateGolden {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取金样失败（先用 -update 生成）: %v", err)
	}
	if string(want) != string(got) {
		t.Fatalf("与金样不同：%s", goldenDiff(want, got))
	}
}

// TestProxyPoolFixedOnGoldenAutoReplace 坏 IP 自动换的上限为 0（设置为 0、先 10 后改回 0）时固定分配场景与金样逐字节相同；上限为 10 时在规则 B
// 那一步开始不同（金样能抓到自动换）
func TestProxyPoolFixedOnGoldenAutoReplace(t *testing.T) {
	if *updateGolden {
		t.Skip("生成金样时不跑")
	}
	want, err := os.ReadFile(filepath.Join("testdata", proxyPoolFixedOnGoldenFile))
	if err != nil {
		t.Fatal(err)
	}
	for name, setup := range map[string]func(pool *proxyPool){
		"0":    func(pool *proxyPool) { pool.setFixedAutoReplace(0) },
		"10→0": func(pool *proxyPool) { pool.setFixedAutoReplace(10); pool.setFixedAutoReplace(0) },
	} {
		got, _ := runProxyPoolFixedOnScenario(t, setup)
		if string(got) != string(want) {
			t.Fatalf("上限 %s 时与金样不同：%s", name, goldenDiff(want, got))
		}
	}
	var golden []proxyPoolGoldenStep
	if err := json.Unmarshal(want, &golden); err != nil {
		t.Fatal(err)
	}
	_, steps := runProxyPoolFixedOnScenario(t, func(pool *proxyPool) { pool.setFixedAutoReplace(10) })
	// 上限大于 0 时状态接口从第一步起多一个 fixed.auto 区块：比较时去掉它，看行为（日志、绑定、租约、文件、钩子）从哪一步开始不同
	withoutAuto := func(step proxyPoolGoldenStep) proxyPoolGoldenStep {
		var status map[string]any
		if json.Unmarshal(step.Status, &status) == nil {
			if fixed, ok := status["fixed"].(map[string]any); ok {
				delete(fixed, "auto")
			}
			step.Status, _ = json.Marshal(status)
		}
		return step
	}
	first := ""
	for index := range min(len(golden), len(steps)) {
		left, _ := json.Marshal(withoutAuto(golden[index]))
		right, _ := json.Marshal(withoutAuto(steps[index]))
		if string(left) != string(right) {
			first = steps[index].Name
			break
		}
	}
	if first != "rule B" {
		t.Fatalf("上限 10 时应在规则 B 那一步开始不同，实际第一处不同在 %q", first)
	}
}
