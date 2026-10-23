package app

import (
	"context"
	"fmt"
	"math/rand"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/api"
	"github.com/Mag1cFall/AIStudio2API/internal/config"
)

// IP 池与 429 治理合并后的接线（见 proxy_pool_egress.go）：出口通道随绑定重挂、出口键按端口与会话区分、冷启动闸门、端口冷却撤回误伤、
// 动态换会话、出口状态瘦身与日志带出口

// proxyPoolTestPasswordSessionURL 为会话号写在密码里的动态 IP 地址（出口键要靠 EgressTag 区分会话）
const proxyPoolTestPasswordSessionURL = "http://customer-a1:pw-{session}@gate.example.com:58688"

func dynamicPasswordProxyPool() config.ProxyPool {
	return config.ProxyPool{Mode: config.ProxyPoolDynamic, URL: proxyPoolTestPasswordSessionURL}
}

// verifyEgressOwners 对每个账户池核对出口通道的不变量（绑定变化与重挂一致、各通道账户数、空闲编号）
func verifyEgressOwners(t *testing.T, step string, owners ...*aistudio.AccountPool) {
	t.Helper()
	if err := egressOwnersError(step, owners...); err != nil {
		t.Fatal(err)
	}
}

// egressOwnersError 为 verifyEgressOwners 的实现（并发的协程里不能调 t.Fatal，返回错误）
func egressOwnersError(step string, owners ...*aistudio.AccountPool) error {
	for index, owner := range owners {
		if err := owner.VerifyEgressForTest(); err != nil {
			return fmt.Errorf("%s：账户池 %d 的出口通道不一致：%w", step, index, err)
		}
	}
	return nil
}

// egressKeyCounts 返回各出口键上的账户数
func egressKeyCounts(pool *aistudio.AccountPool, ids []string) map[string]int {
	counts := make(map[string]int, len(ids))
	for _, id := range ids {
		key, _ := pool.EgressKeyOf(id)
		counts[key]++
	}
	return counts
}

// TestProxyPoolEgressRandomOps 合并后测试（§18 第 8 条）：随机申请、归还、启动失败、登录租约、改配置（静态两种端口段、动态两种会话写法、关闭）、
// 切换账户池、refresh、裸 429、成功与换会话之后，两个账户池的出口通道都与绑定一致（verifyEgressLocked）。先单协程跑，再多协程并发
func TestProxyPoolEgressRandomOps(t *testing.T) {
	ids := proxyPoolTestIDs(16)
	const own = "http://203.0.113.50:3128"
	first, _ := testAccountPool(t, ids...)
	second, _ := testAccountPool(t, ids...)
	setAccountProxy(t, first, ids[15], own)
	setAccountProxy(t, second, ids[15], own)
	clock := newProxyPoolTestClock()
	ipPool := newProxyPool(proxyPoolOptions{now: clock.Now, relayLimit: func() int { return 1 << 20 }})
	configs := []config.ProxyPool{
		staticProxyPool(10001, 10008), staticProxyPool(10005, 10020), dynamicProxyPool(), dynamicPasswordProxyPool(),
		{Mode: config.ProxyPoolOff, URL: proxyPoolTestStaticURL},
	}
	ipPool.configure(configs[0], "", 12)
	ipPool.attach(first)
	owners := []*aistudio.AccountPool{first, second}
	var ownerMu sync.Mutex
	current := first
	run := func(seed int64, steps int) error {
		random := rand.New(rand.NewSource(seed))
		var held []*proxyPoolLease
		for step := 0; step < steps; step++ {
			id := ids[random.Intn(len(ids))]
			if len(held) >= 4 {
				index := random.Intn(len(held))
				held[index].release()
				held = append(held[:index], held[index+1:]...)
			}
			operation := random.Intn(100)
			switch {
			case operation < 30:
				lease := ipPool.acquire(id, id, proxyPoolKindWorker, id, "")
				if random.Intn(4) == 0 {
					lease.launchFailed(upstreamFailure, false)
				} else {
					lease.attach(&fakeProxyCloser{})
					held = append(held, lease)
				}
			case operation < 45:
				if len(held) > 0 {
					index := random.Intn(len(held))
					held[index].release()
					held = append(held[:index], held[index+1:]...)
				}
			case operation < 50:
				held = append(held, ipPool.acquire(id, id, proxyPoolKindVerify, id, ""))
			case operation < 60:
				ipPool.configure(configs[random.Intn(len(configs))], "", 8+random.Intn(8))
			case operation < 65:
				ownerMu.Lock()
				current = owners[random.Intn(len(owners))]
				next := current
				ownerMu.Unlock()
				ipPool.attach(next)
			case operation < 72:
				ownerMu.Lock()
				owner := current
				ownerMu.Unlock()
				ipPool.refresh(owner, id)
			case operation < 82:
				ipPool.rateLimited(id, false)
			case operation < 90:
				ipPool.succeeded(id)
			case operation < 95:
				rotateSessionForTest(ipPool, id, "测试")
			default:
				clock.Advance(time.Duration(random.Intn(90)) * time.Second)
				ipPool.recheckFired()
			}
			if err := egressOwnersError(fmt.Sprintf("种子 %d 第 %d 步（操作 %d）", seed, step, operation), owners...); err != nil {
				for _, lease := range held {
					lease.release()
				}
				return err
			}
		}
		for _, lease := range held {
			lease.release()
		}
		return nil
	}
	if err := run(1, 1500); err != nil {
		t.Fatal(err)
	}
	var wait sync.WaitGroup
	errs := make(chan error, 8)
	for worker := 0; worker < 8; worker++ {
		wait.Add(1)
		go func(seed int64) {
			defer wait.Done()
			errs <- run(seed, 250)
		}(int64(worker + 2))
	}
	wait.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	verifyEgressOwners(t, "全部归还后", owners...)
	// 固定了自己代理的账户始终按自己的代理出网
	for _, owner := range owners {
		if key, _ := owner.EgressKeyOf(ids[15]); key != aistudio.EgressKey(own) {
			t.Fatalf("固定代理账户的出口 = %q", key)
		}
	}
}

// TestProxyPoolEgressKeysPerAccount 合并后测试（§18 第 8 条）：IP 池开启、PROXY 为空时，150 个账户的 Worker 分布在 150 个出口键上，
// “直连”下没有账户（没有 Worker 的默认出口同样不落在直连上）；动态 IP 不论会话号写在用户名还是密码里，默认出口就是每个账户一个出口
func TestProxyPoolEgressKeysPerAccount(t *testing.T) {
	ids := proxyPoolTestIDs(150)
	pool, _ := testAccountPool(t, ids...)
	cases := []struct {
		name    string
		cfg     config.ProxyPool
		dynamic bool
	}{
		{"静态", staticProxyPool(10001, 10200), false},
		{"动态（会话号在用户名里）", dynamicProxyPool(), true},
		{"动态（会话号在密码里）", dynamicPasswordProxyPool(), true},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			ipPool, _ := newTestProxyPool(t, newProxyPoolTestClock(), "", "")
			ipPool.configure(testCase.cfg, "", 188)
			ipPool.attach(pool)
			defaults := egressKeyCounts(pool, ids)
			if defaults[""] != 0 {
				t.Fatalf("默认出口有 %d 个账户落在直连上", defaults[""])
			}
			if testCase.dynamic && len(defaults) != len(ids) {
				t.Fatalf("动态 IP 的默认出口 %d 个，期望每个账户一个（%d）", len(defaults), len(ids))
			}
			leases := make([]*proxyPoolLease, 0, len(ids))
			for _, id := range ids {
				lease := ipPool.acquire(id, id, proxyPoolKindWorker, id, "")
				lease.attach(&fakeProxyCloser{})
				leases = append(leases, lease)
			}
			workers := egressKeyCounts(pool, ids)
			if len(workers) != len(ids) || workers[""] != 0 {
				t.Fatalf("150 个 Worker 分布在 %d 个出口键上（直连 %d 个），期望 150 个、直连 0 个", len(workers), workers[""])
			}
			for _, status := range pool.EgressSnapshot() {
				if status.Key == "" && status.Lanes[aistudio.EgressLaneNormal].Accounts+status.Lanes[aistudio.EgressLaneUltra].Accounts > 0 {
					t.Fatalf("直连出口仍有账户：%+v", status)
				}
			}
			verifyEgressOwners(t, testCase.name, pool)
			for _, lease := range leases {
				lease.release()
			}
			// 下一个子测试换一个 IP 池推送：先把这个 IP 池关掉，推送清除绑定
			ipPool.configure(config.ProxyPool{Mode: config.ProxyPoolOff}, "", 188)
			verifyEgressOwners(t, testCase.name+"关闭后", pool)
		})
	}
}

// TestEgressGateBypassesPooledAccounts 冷启动闸门（§18 第 3 条）：IP 池开启、账户没有自己的代理时不经闸门（真正启动时另分出口），
// 有自己代理的账户与 IP 池关闭后照常按出口判定暂停
func TestEgressGateBypassesPooledAccounts(t *testing.T) {
	env := newEgressEnv(t, nil, []string{"a@example.com", "b@example.com"}, 2, 2, 0)
	setAccountProxy(t, env.pool, "b@example.com", "http://203.0.113.50:3128")
	ipPool, _ := newTestProxyPool(t, newProxyPoolTestClock(), "", "")
	ipPool.configure(staticProxyPool(10001, 10010), "", 4)
	ipPool.attach(env.pool)
	env.manager.proxyPool = ipPool
	limitAll := func() {
		for _, id := range []string{"a@example.com", "b@example.com"} {
			key, _ := env.pool.EgressKeyOf(id)
			env.service.egress.forceVerdictForTest(key, egressVerdict{
				Mode: egressLimited, Limited: true, Label: "测试", Limit: egressMinLimit, RetryAt: time.Now().Add(time.Minute),
			})
		}
	}
	limitAll()
	gate := env.service.egressColdStartGate()
	if release, ok := gate("a@example.com", testRuntimeModel, coldStartReplace); !ok {
		t.Fatal("IP 池分配出口的账户不应被冷启动闸门暂停")
	} else {
		release(true)
	}
	if _, ok := gate("b@example.com", testRuntimeModel, coldStartReplace); ok {
		t.Fatal("有自己代理的账户所在出口限流时，替换路径应暂停")
	}
	ipPool.configure(config.ProxyPool{Mode: config.ProxyPoolOff}, "", 4)
	limitAll()
	if _, ok := gate("a@example.com", testRuntimeModel, coldStartReplace); ok {
		t.Fatal("IP 池关闭后账户按自己的出口经闸门")
	}
}

// TestLimitedStandbyKeepsPooledAccounts 出口限流期间剔除受限出口上的备用账户时留下由 IP 池分配出口的账户（复查 1-2，与冷启动闸门一致）：
// 端口全被占用时 a 的默认出口退回 PROXY，PROXY 判定为限流；a 真正启动时池另分出口，转写等候选与现场提升、扩容的过滤都不剔除它；
// 固定了 PROXY 的 p 照常剔除。IP 池关闭后 a 同样按出口剔除
func TestLimitedStandbyKeepsPooledAccounts(t *testing.T) {
	const proxy = "http://203.0.113.9:3128"
	env := newEgressEnv(t, []string{"z@example.com"}, []string{"a@example.com", "p@example.com"}, 1, 3, 0)
	env.pool.SetGlobalProxy(proxy)
	setAccountProxy(t, env.pool, "p@example.com", proxy)
	for _, accountID := range []string{"z@example.com", "a@example.com", "p@example.com"} {
		if err := env.pool.SetCatalog(accountID, aistudio.BenefitTierFree, []aistudio.Model{{
			ID: testRuntimeModel, Methods: []string{"generateContent", "countTokens"},
			Capabilities: map[string]bool{"chat_model": true, "transcription_output": true},
		}}); err != nil {
			t.Fatal(err)
		}
	}
	ipPool, _ := newTestProxyPool(t, newProxyPoolTestClock(), "", "")
	ipPool.configure(staticProxyPool(10001, 10001), proxy, 2)
	ipPool.attach(env.pool)
	env.manager.proxyPool = ipPool
	// z 的 Worker 占着唯一的端口：a 的默认出口退回 PROXY
	worker := ipPool.acquire("z@example.com", "z@example.com", proxyPoolKindWorker, "z", "")
	worker.attach(&fakeProxyCloser{})
	defer worker.release()
	proxyKey := aistudio.EgressKey(proxy)
	if key, _ := env.pool.EgressKeyOf("a@example.com"); key != proxyKey {
		t.Fatalf("前提：端口被占时 a 的默认出口应退回 PROXY，得到 %q", key)
	}
	env.service.egress.forceVerdictForTest(proxyKey, egressVerdict{
		Mode: egressLimited, Limited: true, Label: "测试", Limit: egressMinLimit, RetryAt: time.Now().Add(time.Minute),
	})
	limited := env.service.egress.limitedKeys()
	ids := []string{"a@example.com", "p@example.com"}
	if kept, removed := env.service.filterLimitedStandby(ids, limited); !slices.Equal(kept, []string{"a@example.com"}) ||
		!slices.Equal(removed, []string{"p@example.com"}) {
		t.Fatalf("留下 %v、剔除 %v，期望留下 IP 池分配出口的 a、剔除固定 PROXY 的 p", kept, removed)
	}
	if kept, removed := env.service.filterLimitedStandby([]string{"a@example.com"}, limited); !slices.Equal(kept, []string{"a@example.com"}) ||
		removed != nil {
		t.Fatalf("只剩池账户时不应剔除：%v %v", kept, removed)
	}
	ctx, cancel := context.WithTimeout(env.service.lifecycle, 10*time.Second)
	defer cancel()
	if candidates, err := env.service.transcriptionCandidates(ctx, testRuntimeModel); err != nil ||
		!slices.Equal(candidates, []string{"z@example.com", "a@example.com"}) {
		t.Fatalf("转写候选 %v err=%v，期望热 Worker 的 z 与 IP 池分配出口的 a", candidates, err)
	}
	ipPool.configure(config.ProxyPool{Mode: config.ProxyPoolOff, URL: proxyPoolTestStaticURL}, proxy, 2)
	if kept, removed := env.service.filterLimitedStandby(ids, limited); len(kept) != 0 || len(removed) != 2 {
		t.Fatalf("IP 池关闭后两个账户都在 PROXY 上，应都剔除：%v %v", kept, removed)
	}
}

// TestProxyPortCooledRelaxesLadder 端口冷却撤回误伤（§18 第 4 条，规则 A）：a 在端口上裸限流被逐级冷却，归还后 b 拿到同一端口也裸限流，
// 端口冷却；回调撤回 a 与 b 的逐级冷却（包括 b 刚写的这一次）、让正用端口的 b 换端口重启。裸 429 的冷却行末尾带端口
func TestProxyPortCooledRelaxesLadder(t *testing.T) {
	skipNearQuotaDay(t, time.Hour)
	upstream := &bareRateUpstream{limited: map[string]bool{"a@example.com": true, "b@example.com": true}, midStream: map[string]bool{}}
	service, pool := rateLadderService(t, upstream, "a@example.com", "b@example.com")
	// 第 1 档 2 分钟：比撤回后的短冷却长，又不到淘汰门槛（不登记淘汰，不干扰后台关闭的检查）
	service.rateLimitLadder = []time.Duration{2 * time.Minute, 5 * time.Minute, rateLimitLevel3, rateLimitLevel4}
	service.egress = newEgressGuard(pool, service.requests)
	service.egress.shortCooldown = 100 * time.Millisecond
	t.Cleanup(service.egress.relaxWG.Wait)
	ipPool, _ := newTestProxyPool(t, newProxyPoolTestClock(), "", "")
	ipPool.configure(staticProxyPool(10001, 10001), "", 2)
	ipPool.attach(pool)
	service.workers.proxyPool = ipPool
	ipPool.setPortCooledHook(service.proxyPortCooled)

	leaseA := ipPool.acquire("a@example.com", "a@example.com", proxyPoolKindWorker, "a", "")
	leaseA.attach(&fakeProxyCloser{})
	failRateRequest(t, service, "r-a", "a@example.com")
	if until, _ := rateCooldowns(t, pool, "a@example.com"); time.Until(until) < time.Minute {
		t.Fatalf("a 应被逐级冷却第 1 档（2 分钟），到期 %s", until)
	}
	leaseA.release()
	leaseB := ipPool.acquire("b@example.com", "b@example.com", proxyPoolKindWorker, "b", "")
	leaseB.attach(&fakeProxyCloser{})
	defer leaseB.release()
	if leaseB.Slot() != "10001" {
		t.Fatalf("b 的端口 = %q", leaseB.Slot())
	}
	// b 刚在别的出口上成功过：到这个端口就被拒是 IP 的证据
	markProxyPoolProven(ipPool, "b@example.com")
	failRateRequest(t, service, "r-b", "b@example.com")
	service.egress.relaxWG.Wait()
	if status := ipPool.status(); status.Cooling != 1 {
		t.Fatalf("两个账号在同一端口裸限流后应冷却：%+v", status)
	}
	for _, id := range []string{"a@example.com", "b@example.com"} {
		status, _ := pool.StatusOf(id)
		cooldown := status.Cooldowns[rateLadderModel]
		if !strings.HasPrefix(cooldown.Reason, "限流: 出口限流撤回（原第 1 档），") || time.Until(cooldown.Until) > time.Second {
			t.Fatalf("%s 的冷却应撤回到短冷却：%+v", id, cooldown)
		}
	}
	if got := countSourceLogs(service.requests, "service", "IP 池端口冷却：撤回逐级冷却并换端口重启 Worker", "端口=10001",
		"规则=不同账号返回限流", "撤回账号=2", "重启 Worker=1"); got != 1 {
		t.Fatalf("撤回日志 %d 条", got)
	}
	if got := countSourceLogs(service.requests, "", "账号冷却 | 类型=限流", "连续限流=第 1 档", "| 端口=10001"); got < 2 {
		t.Fatalf("裸 429 的冷却行应带端口，得到 %d 条", got)
	}
	deadline := time.Now().Add(10 * time.Second)
	for service.workers.hasWorker("b@example.com") {
		if time.Now().After(deadline) {
			t.Fatal("正用冷却端口的 b 的 Worker 应在后台关闭")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !service.workers.hasWorker("a@example.com") {
		t.Fatal("已经不用这个端口的 a 不应被关闭 Worker")
	}
	waitForLog(t, service.requests, "IP 池端口冷却，已关闭用这个端口的 Worker")
}

// TestProxyPortCooledPardonsBadAccount 端口冷却撤回误伤也撤回坏号记忆（复查 1-1、2-2）：a 在独占端口上连续两次裸 429 被标坏号
// （只在这个端口上被拒），归还后刚在别的出口上成功过的 b 在同一端口裸 429，规则 A 冷却端口；a 不再是坏号（回到预热、扩容与替补的候选），
// b 的连续次数清零（换到好端口后再偶发一次裸 429 不标坏号），撤回日志带撤回坏号数
func TestProxyPortCooledPardonsBadAccount(t *testing.T) {
	skipNearQuotaDay(t, time.Hour)
	upstream := &bareRateUpstream{limited: map[string]bool{"a@example.com": true, "b@example.com": true}, midStream: map[string]bool{}}
	service, pool := rateLadderService(t, upstream, "a@example.com", "b@example.com")
	service.egress = newEgressGuard(pool, service.requests)
	service.egress.shortCooldown = 100 * time.Millisecond
	t.Cleanup(service.egress.relaxWG.Wait)
	memory := openBadAccountMemory("", service.requests)
	service.workers.badAccounts.Store(memory)
	ipPool, _ := newTestProxyPool(t, newProxyPoolTestClock(), "", "")
	ipPool.configure(staticProxyPool(10001, 10001), "", 2)
	ipPool.attach(pool)
	service.workers.proxyPool = ipPool
	ipPool.setPortCooledHook(service.proxyPortCooled)

	leaseA := ipPool.acquire("a@example.com", "a@example.com", proxyPoolKindWorker, "a", "")
	leaseA.attach(&fakeProxyCloser{})
	failRateRequest(t, service, "r-a1", "a@example.com")
	waitRateCooldownExpired(t, pool, "a@example.com")
	failRateRequest(t, service, "r-a2", "a@example.com")
	if !memory.bad("a@example.com") {
		t.Fatal("前提：a 连续两次裸 429 后应标坏号")
	}
	leaseA.release()
	leaseB := ipPool.acquire("b@example.com", "b@example.com", proxyPoolKindWorker, "b", "")
	leaseB.attach(&fakeProxyCloser{})
	defer leaseB.release()
	markProxyPoolProven(ipPool, "b@example.com")
	failRateRequest(t, service, "r-b", "b@example.com")
	service.egress.relaxWG.Wait()
	if status := ipPool.status(); status.Cooling != 1 {
		t.Fatalf("端口应冷却：%+v", status)
	}
	if memory.bad("a@example.com") {
		t.Fatal("端口按规则 A 冷却（被拒的是 IP）后，a 不应再是坏号")
	}
	memory.observe("b@example.com", "b", egressBare)
	if memory.bad("b@example.com") {
		t.Fatal("b 的连续裸限流次数应随撤回清零，之后一次裸 429 不标坏号")
	}
	if got := countSourceLogs(service.requests, "service", "IP 池端口冷却：撤回逐级冷却并换端口重启 Worker", "端口=10001",
		"规则=不同账号返回限流", "撤回账号=2", "撤回坏号=1"); got != 1 {
		t.Fatalf("撤回日志应带撤回坏号数，得到 %d 条", got)
	}
}

// TestProxyPortCooledResetsWorkerOnUpstream 规则 B（连续 2 次连不上上游代理）冷却端口时只让正用端口的 Worker 换端口重启，不撤回冷却；
// “WAA Worker 启动失败”行末尾带这次启动用的端口
func TestProxyPortCooledResetsWorkerOnUpstream(t *testing.T) {
	env := newPoolWorkerEnv(t, staticProxyPool(10001, 10001), 3, nil, "x@example.com", "y@example.com")
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	service := &trackedService{lifecycle: ctx, pool: env.pool, requests: env.requests, workers: env.manager}
	env.ipPool.setPortCooledHook(service.proxyPortCooled)
	if err := env.promote(t, "x@example.com"); err != nil {
		t.Fatal(err)
	}
	env.launches.mu.Lock()
	env.launches.fail = func(_ context.Context, accountID string) error {
		if accountID == "y@example.com" {
			return upstreamFailure
		}
		return nil
	}
	env.launches.mu.Unlock()
	for attempt := 0; attempt < 2; attempt++ {
		if err := env.promote(t, "y@example.com"); err == nil {
			t.Fatal("y 的启动应失败")
		}
	}
	if status := env.ipPool.status(); status.Cooling != 1 {
		t.Fatalf("连续 2 次连不上上游后端口应冷却：%+v", status)
	}
	if got := countSourceLogs(env.requests, "", "WAA Worker 启动失败", "| 端口=10001"); got != 2 {
		t.Fatalf("启动失败行应带端口，得到 %d 条", got)
	}
	deadline := time.Now().Add(10 * time.Second)
	for env.manager.hasWorker("x@example.com") {
		if time.Now().After(deadline) {
			t.Fatal("正用冷却端口的 x 的 Worker 应在后台关闭")
		}
		time.Sleep(10 * time.Millisecond)
	}
	waitForLog(t, env.requests, "IP 池端口冷却：撤回逐级冷却并换端口重启 Worker | 端口=10001 | 规则=连续出口连不通 | 撤回账号=0")
}

// TestRotateSessionOnEgressLimited 动态换会话（§18 第 5 条）：账户所在出口判定为限流时换会话，没有 Worker 的账户立即切到新会话的出口；
// 10 分钟内不再换
func TestRotateSessionOnEgressLimited(t *testing.T) {
	skipNearQuotaDay(t, time.Hour)
	upstream := &bareRateUpstream{limited: map[string]bool{"a@example.com": true}, midStream: map[string]bool{}}
	service, pool := rateLadderService(t, upstream, "a@example.com")
	service.egress = newEgressGuard(pool, service.requests)
	service.egress.shortCooldown = 50 * time.Millisecond
	t.Cleanup(service.egress.relaxWG.Wait)
	ipPool, logs := newTestProxyPool(t, newProxyPoolTestClock(), "", "")
	ipPool.configure(dynamicProxyPool(), "", 2)
	ipPool.attach(pool)
	service.workers.proxyPool = ipPool
	limit := func(key string) {
		service.egress.forceVerdictForTest(key, egressVerdict{
			Mode: egressLimited, Limited: true, Label: "测试", Limit: egressMinLimit, RetryAt: time.Now().Add(time.Minute),
		})
	}
	before, _ := pool.EgressKeyOf("a@example.com")
	limit(before)
	failRateRequest(t, service, "r-rotate-1", "a@example.com")
	after, _ := pool.EgressKeyOf("a@example.com")
	if after == before || after == "" {
		t.Fatalf("出口限流判定成立后应换会话：%q → %q", before, after)
	}
	if got := countProxyPoolLogs(logs, "原因=出口限流判定成立 | 生效=立即（没有 Worker）"); got != 1 {
		t.Fatalf("换会话日志 %d 条：%v", got, proxyPoolLogLines(logs))
	}
	verifyEgressOwners(t, "换会话后", pool)
	limit(after)
	time.Sleep(100 * time.Millisecond)
	failRateRequest(t, service, "r-rotate-2", "a@example.com")
	if again, _ := pool.EgressKeyOf("a@example.com"); again != after || countProxyPoolLogs(logs, "IP 池换会话") != 1 {
		t.Fatalf("10 分钟内不应再换会话：%q → %q", after, again)
	}
}

// TestRotateSessionOnlyForOwnSession 动态换会话只在判定的出口就是账户当前的会话时发生（复查 2-3）：开启动态 IP 之前启动、仍走 PROXY 的
// 直通 Worker 所在的 PROXY 判定为限流时，账户的会话没用过，不换会话、不记日志、不占 10 分钟间隔；正在服务的 Worker 仍用换会话之前的
// 旧会话时，旧会话的出口判定同样不换（新会话还没用过）
func TestRotateSessionOnlyForOwnSession(t *testing.T) {
	skipNearQuotaDay(t, time.Hour)
	const proxy = "http://203.0.113.9:3128"
	upstream := &bareRateUpstream{limited: map[string]bool{"a@example.com": true}, midStream: map[string]bool{}}
	service, pool := rateLadderService(t, upstream, "a@example.com")
	pool.SetGlobalProxy(proxy)
	service.egress = newEgressGuard(pool, service.requests)
	service.egress.shortCooldown = 50 * time.Millisecond
	t.Cleanup(service.egress.relaxWG.Wait)
	ipPool, logs := newTestProxyPool(t, newProxyPoolTestClock(), "", "")
	ipPool.configure(dynamicProxyPool(), proxy, 2)
	ipPool.attach(pool)
	service.workers.proxyPool = ipPool
	passthrough := ipPool.passthrough("a@example.com", "a@example.com", proxyPoolKindWorker, "a", proxy)
	key, _ := pool.EgressKeyOf("a@example.com")
	if key != aistudio.EgressKey(proxy) {
		t.Fatalf("前提：直通 Worker 服务时出口应为 PROXY，得到 %q", key)
	}
	service.egress.forceVerdictForTest(key, egressVerdict{
		Mode: egressLimited, Limited: true, Label: "测试", Limit: egressMinLimit, RetryAt: time.Now().Add(time.Minute),
	})
	failRateRequest(t, service, "r-passthrough", "a@example.com")
	if countProxyPoolLogs(logs, "IP 池换会话") != 0 || ipPool.status().Rotations != 0 {
		t.Fatalf("PROXY 判定成立时不应换没用过的会话：%v", proxyPoolLogLines(logs))
	}
	passthrough.release()
	// 池分配的 Worker 用当前会话；换会话之后、Worker 还没重启时，判定落在它仍在用的旧会话上也不再换
	worker := ipPool.acquire("a@example.com", "a@example.com", proxyPoolKindWorker, "a", "")
	worker.attach(&fakeProxyCloser{})
	defer worker.release()
	serving, _ := pool.EgressKeyOf("a@example.com")
	if ipPool.rotateSession("a@example.com", aistudio.EgressKey(proxy), "测试") {
		t.Fatal("判定的出口不是账户的会话时不应换")
	}
	if !ipPool.rotateSession("a@example.com", serving, "测试") {
		t.Fatal("判定的出口是正在服务的当前会话时应换")
	}
	ipPool.mu.Lock()
	ipPool.sessions["a@example.com"].rotatedAt = time.Time{}
	ipPool.mu.Unlock()
	if ipPool.rotateSession("a@example.com", serving, "测试") {
		t.Fatal("Worker 仍用旧会话（新会话还没用过）时不应再换")
	}
}

// TestEgressAdminStatusSummary 出口状态瘦身（§18 第 6 条）：出口不多（不超过 egressStatusFullLimit）时状态接口全部列出（含正常的，
// 诊断第 0 段要看门槛附近的计数），多时只列出判定成立或已撤销的出口、正常的只计数；空闲的正常出口状态在 10 分钟历史过后删除，
// 已撤销的保留到撤销到期
func TestEgressAdminStatusSummary(t *testing.T) {
	guard := newEgressGuard(nil, nil)
	now := time.Now()
	guard.now = func() time.Time { return now }
	const normalKey, rejectedKey = "http://203.0.113.1:3128", "http://203.0.113.2:3128"
	for _, key := range []string{normalKey, rejectedKey} {
		guard.observeOutcome(egressObservation{Key: key, AccountID: "a@example.com", Outcome: egressSuccess, At: now})
	}
	if err := guard.withdraw(aistudio.EgressID(rejectedKey), now); err != nil {
		t.Fatal(err)
	}
	entries, summary := guard.adminStatus()
	if len(entries) != 2 || entries[0].Mode != "rejected" || entries[0].ID != aistudio.EgressID(rejectedKey) ||
		entries[1].Mode != "normal" || entries[1].ID != aistudio.EgressID(normalKey) || entries[1].Success1m != 1 ||
		summary == nil || *summary != (api.AdminEgressSummary{Total: 2, Normal: 1, Rejected: 1}) {
		t.Fatalf("出口不多时应全部列出（已撤销的在前）：%+v，汇总 = %+v", entries, summary)
	}
	if all := guard.status(); len(all) != 2 {
		t.Fatalf("内部全量状态应有 2 个出口：%+v", all)
	}
	// 出口多于 egressStatusFullLimit 时只列出非正常的，正常的只计入汇总
	for index := range egressStatusFullLimit {
		guard.observeOutcome(egressObservation{
			Key: fmt.Sprintf("http://198.51.100.%d:3128", index+1), AccountID: "a@example.com", Outcome: egressSuccess, At: now,
		})
	}
	entries, summary = guard.adminStatus()
	if len(entries) != 1 || entries[0].Mode != "rejected" ||
		*summary != (api.AdminEgressSummary{Total: egressStatusFullLimit + 2, Normal: egressStatusFullLimit + 1, Rejected: 1}) {
		t.Fatalf("出口多时只列出已撤销的：%d 条，汇总 = %+v", len(entries), summary)
	}
	now = now.Add(11 * time.Minute)
	guard.tick(now)
	guard.mu.Lock()
	_, normalKept := guard.states[normalKey]
	_, rejectedKept := guard.states[rejectedKey]
	_, idKept := guard.byID[aistudio.EgressID(normalKey)]
	guard.mu.Unlock()
	if normalKept || idKept || !rejectedKept {
		t.Fatalf("空闲的正常出口应删除（%v、%v），已撤销的保留（%v）", normalKept, idKept, rejectedKept)
	}
	if _, summary := guard.adminStatus(); summary.Total != 1 || summary.Rejected != 1 {
		t.Fatalf("删除后汇总 = %+v", summary)
	}
	now = now.Add(31 * time.Minute)
	guard.tick(now)
	if entries, summary := guard.adminStatus(); len(entries) != 0 || summary.Total != 0 {
		t.Fatalf("撤销到期、空闲后出口状态应删除：%+v %+v", entries, summary)
	}
	// 再有结果时从零重建
	guard.observeOutcome(egressObservation{Key: normalKey, AccountID: "a@example.com", Outcome: egressBare, At: now})
	if _, summary := guard.adminStatus(); summary.Total != 1 || summary.Normal != 1 {
		t.Fatalf("重建后汇总 = %+v", summary)
	}
}

// TestEgressAdminStatusPoolTotal 状态接口的出口汇总按账户池给的总数计正常出口、只取有判定状态的出口的计数（复查 2-1）：
// 动态 IP 下每个账户一个会话出口，汇总仍等于全量快照的条数；已撤销的出口带账户池计数；判定器有状态、账户池里没有记录的出口另计
func TestEgressAdminStatusPoolTotal(t *testing.T) {
	ids := proxyPoolTestIDs(40)
	pool, _ := testAccountPool(t, ids...)
	ipPool, _ := newTestProxyPool(t, newProxyPoolTestClock(), "", "")
	ipPool.configure(dynamicPasswordProxyPool(), "", 4)
	ipPool.attach(pool)
	guard := newEgressGuard(pool, nil)
	rejected, _ := pool.EgressKeyOf(ids[3])
	const orphan = "http://198.51.100.7:3128"
	now := time.Now()
	for _, key := range []string{rejected, orphan} {
		guard.observeOutcome(egressObservation{Key: key, AccountID: ids[3], Outcome: egressSuccess, At: now})
	}
	if err := guard.withdraw(aistudio.EgressID(rejected), now); err != nil {
		t.Fatal(err)
	}
	entries, summary := guard.adminStatus()
	if total := len(pool.EgressSnapshot()); total != len(ids) {
		t.Fatalf("前提：每个账户一个会话出口，得到 %d 个", total)
	}
	if summary == nil || *summary != (api.AdminEgressSummary{Total: len(ids) + 1, Normal: len(ids), Rejected: 1}) {
		t.Fatalf("汇总 = %+v", summary)
	}
	if len(entries) != 1 || entries[0].ID != aistudio.EgressID(rejected) || entries[0].Accounts != 1 {
		t.Fatalf("状态接口出口 = %+v，期望只有已撤销的那个、带账户池的账户数", entries)
	}
	if all := guard.status(); len(all) != len(ids)+1 {
		t.Fatalf("全量状态 %d 个出口", len(all))
	}
}

// waitForLog 等运行日志里出现含 fragment 的一行（后台关闭 Worker 的日志在 goroutine 里写）
func waitForLog(t *testing.T, requests *requestRegistry, fragment string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for countSourceLogs(requests, "", fragment) == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("没有等到日志：%s", fragment)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestProxyPortCooledRechecksWorkerPort 端口冷却后重启 Worker 之前再核对账户仍用这个端口服务（合并复查第 2 轮 2-3）：回调在后台执行，
// 期间账户已换成别的端口上的新浏览器时不关；日志的“重启 Worker”只计真正登记的关闭，ResetAll、Close 期间不登记的计入“未登记”（1-2）
func TestProxyPortCooledRechecksWorkerPort(t *testing.T) {
	const id = "x@example.com"
	env := newPoolWorkerEnv(t, staticProxyPool(10001, 10002), 3, nil, id)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	service := &trackedService{lifecycle: ctx, pool: env.pool, requests: env.requests, workers: env.manager}
	if err := env.promote(t, id); err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(env.assignment(id))
	if err != nil {
		t.Fatalf("x 的端口 = %q", env.assignment(id))
	}
	cooling := proxyPoolCooling{Port: port, Rule: proxyPoolRuleUpstream, Workers: []string{id}}
	const line = "IP 池端口冷却：撤回逐级冷却并换端口重启 Worker"
	// 端口冷却（回调还没执行）期间，先启后停的新浏览器在另一个端口上启动（绑定跟到它），冷却回调这时才执行
	env.ipPool.mu.Lock()
	env.ipPool.coolLocked(port, proxyPoolRuleUpstream, "测试", env.clock.Now())
	env.ipPool.mu.Unlock()
	moved := env.ipPool.acquire(id, id, proxyPoolKindWorker, "x", "")
	moved.attach(&fakeProxyCloser{})
	other, _ := strconv.Atoi(moved.Slot())
	if other == port || env.ipPool.servesOnPort(id, port) || !env.ipPool.servesOnPort(id, other) {
		t.Fatalf("前提：x 应已在另一个端口上服务（原端口 %d、新端口 %s）", port, moved.Slot())
	}
	service.relaxProxyPort(cooling)
	if got := countSourceLogs(env.requests, "service", line); got != 0 {
		t.Fatalf("账户已不在冷却端口上，不应登记关闭：%d 行", got)
	}
	if !env.manager.hasWorker(id) {
		t.Fatal("已不用冷却端口的 Worker 不应被关闭")
	}
	moved.release()
	// ResetAll、Close 期间不登记：不算重启，计入“未登记”
	env.manager.resetMu.Lock()
	env.manager.resetHolds++
	env.manager.resetMu.Unlock()
	service.relaxProxyPort(cooling)
	env.manager.resetMu.Lock()
	env.manager.resetHolds--
	env.manager.resetMu.Unlock()
	if got := countSourceLogs(env.requests, "service", line, "重启 Worker=0", "未登记=1"); got != 1 {
		t.Fatalf("没有登记的关闭应计入未登记：%v", serviceLogMessages(env.requests))
	}
	service.relaxProxyPort(cooling)
	if got := countSourceLogs(env.requests, "service", line, "重启 Worker=1 | 耗时="); got != 1 {
		t.Fatalf("登记了关闭应计入重启 Worker：%v", serviceLogMessages(env.requests))
	}
	waitForLog(t, env.requests, "IP 池端口冷却，已关闭用这个端口的 Worker")
}

// TestColdStartAttemptsFollowWorkerEgress 冷启动请求的结果记在 Worker 实际分到的端口上（合并复查第 2 轮 1-1）：端口全被占用、PROXY 已设时
// 备用账户的默认出口都是 PROXY；现场提升先取租约再启动 Worker，Worker 另分（这里是共用的）端口，租约随之跟到这个端口。12 个账号的
// 冷启动首发都被裸 429 拒绝后，PROXY 上没有任何结果（原先全部记在 PROXY 上、凑成“冷号首发被拒”判定成立），每个端口上的账号凑不够门槛
func TestColdStartAttemptsFollowWorkerEgress(t *testing.T) {
	skipNearQuotaDay(t, time.Hour)
	const proxy = "http://203.0.113.9:3128"
	ids := proxyPoolTestIDs(12)
	env := newEgressEnv(t, nil, ids, 0, len(ids), 0)
	upstream := &bareRateUpstream{limited: map[string]bool{}, midStream: map[string]bool{}}
	for _, id := range ids {
		upstream.limited[id] = true
	}
	env.service.service = upstream
	env.pool.SetGlobalProxy(proxy)
	ipPool, _ := newTestProxyPool(t, newProxyPoolTestClock(), "", "")
	ipPool.configure(staticProxyPool(10001, 10004), proxy, len(ids))
	ipPool.attach(env.pool)
	env.manager.proxyPool = ipPool
	// 4 个端口全被登录占着：备用账户的默认出口退回 PROXY
	for index := range 4 {
		login := ipPool.acquire(fmt.Sprintf("login:%d", index), "", proxyPoolKindLogin, "auth", proxy)
		defer login.release()
	}
	proxyKey := aistudio.EgressKey(proxy)
	for _, id := range ids {
		if key, _ := env.pool.EgressKeyOf(id); key != proxyKey {
			t.Fatalf("前提：%s 的默认出口应是 PROXY，得到 %s", id, aistudio.EgressLabel(key))
		}
	}
	// 每个账号一个指定账户的请求：同样经 acquireLease 先取租约、再现场提升，被拒后不换号，不用等别的账号的冷却
	for index, id := range ids {
		runModelRequest(t, env.service, fmt.Sprintf("req-cold-egress-%d", index), id, testRuntimeModel)
	}
	tried := map[string]struct{}{}
	for _, call := range upstream.called() {
		tried[strings.SplitN(call, "/", 2)[0]] = struct{}{}
	}
	if len(tried) != len(ids) {
		t.Fatalf("前提：%d 个账号的冷启动首发都被拒，只有 %d 个：%v", len(ids), len(tried), upstream.called())
	}
	env.service.egress.mu.Lock()
	_, onProxy := env.service.egress.states[proxyKey]
	var portKeys []string
	for key := range env.service.egress.states {
		portKeys = append(portKeys, key)
	}
	env.service.egress.mu.Unlock()
	if onProxy {
		t.Fatalf("冷启动请求不是从 PROXY 发出的，PROXY 上不应有结果；出口：%v", portKeys)
	}
	if _, limited := env.service.egress.limitedKeys()[proxyKey]; limited {
		t.Fatal("PROXY 不应因冷启动首发被拒判定为限流")
	}
	for _, key := range portKeys {
		if !strings.Contains(aistudio.EgressLabel(key), "isp.example.com:1000") {
			t.Fatalf("结果应记在 Worker 分到的端口上，出现了 %s", aistudio.EgressLabel(key))
		}
	}
	if len(env.service.egress.limitedKeys()) != 0 {
		t.Fatalf("每个端口上的账号凑不够判定门槛：%v", env.service.egress.limitedKeys())
	}
	// 账号冷却行里的端口与出口一致：每行的端口就是记结果的出口
	if got := countSourceLogs(env.service.requests, "", "账号冷却 | 类型=限流", "| 端口=1000"); got < len(tried) {
		t.Fatalf("裸 429 的冷却行应带 Worker 的端口，得到 %d 行", got)
	}
}

// TestColdStartLeaseKeyIsWorkerPort 端口充足、账户没有亲和时，默认出口（探测序列上的空闲端口）多半不是 Worker 分到的端口
// （合并复查第 2 轮 1-1）：现场提升后这次尝试的结果记在 Worker 的端口上，与账户启动后的出口键一致；默认出口上没有结果
func TestColdStartLeaseKeyIsWorkerPort(t *testing.T) {
	ids := proxyPoolTestIDs(8)
	env := newEgressEnv(t, nil, ids, 0, len(ids), 0)
	ipPool, _ := newTestProxyPool(t, newProxyPoolTestClock(), "", "")
	ipPool.configure(staticProxyPool(10001, 10010), "", len(ids))
	ipPool.attach(env.pool)
	env.manager.proxyPool = ipPool
	defaults := make(map[string]string, len(ids))
	for _, id := range ids {
		defaults[id], _ = env.pool.EgressKeyOf(id)
	}
	moved := 0
	for index, id := range ids {
		if text, err, _ := runModelRequest(t, env.service, fmt.Sprintf("req-cold-port-%d", index), id, testRuntimeModel); err != nil || text == "" {
			t.Fatalf("%s 应生成成功：%q %v", id, text, err)
		}
		serving, _ := env.pool.EgressKeyOf(id)
		if serving != defaults[id] {
			moved++
		}
		env.service.egress.mu.Lock()
		state := env.service.egress.states[serving]
		env.service.egress.mu.Unlock()
		if state == nil {
			t.Fatalf("%s 的结果应记在 Worker 的端口 %s 上", id, aistudio.EgressLabel(serving))
		}
	}
	if moved == 0 {
		t.Fatal("前提：至少一个账户的默认出口不是 Worker 分到的端口")
	}
	env.service.egress.mu.Lock()
	recorded := len(env.service.egress.states)
	env.service.egress.mu.Unlock()
	if recorded != len(ids) {
		t.Fatalf("结果应分别记在 %d 个 Worker 端口上，得到 %d 个出口", len(ids), recorded)
	}
}

// TestEgressLeaseRebindConcurrent 冷启动租约跟到实际出口的并发（合并复查第 2 轮 1-1）：Worker 租约申请、启动与归还（绑定随之重挂）、
// 生成租约取得、重挂（RebindEgress）、发送与释放、按键快照（修剪空闲出口记录）同时进行，之后出口通道不变量成立、预占全部退还
func TestEgressLeaseRebindConcurrent(t *testing.T) {
	ids := proxyPoolTestIDs(24)
	pool, _ := testAccountPool(t, ids...)
	ipPool, _ := newTestProxyPool(t, newProxyPoolTestClock(), "", "")
	ipPool.configure(staticProxyPool(10001, 10016), "", 16)
	ipPool.attach(pool)
	keys := make([]string, 0, 16)
	for port := 10001; port <= 10016; port++ {
		keys = append(keys, aistudio.EgressKey(staticPortURL(t, port)))
	}
	// 一个端口限速：重挂时退还与消耗令牌的路径也走到
	pool.SetEgressRateLimit(keys[0], aistudio.EgressLaneNormal, 600)
	errs := make(chan error, 64)
	var wg sync.WaitGroup
	for worker := 0; worker < 6; worker++ {
		wg.Add(1)
		go func(seed int64) {
			defer wg.Done()
			random := rand.New(rand.NewSource(seed))
			var held []*proxyPoolLease
			for step := 0; step < 300; step++ {
				id := ids[random.Intn(len(ids))]
				switch random.Intn(5) {
				case 0, 1:
					lease := ipPool.acquire(id, id, proxyPoolKindWorker, id, "")
					lease.attach(&fakeProxyCloser{})
					held = append(held, lease)
					if len(held) > 3 {
						held[0].release()
						held = held[1:]
					}
				case 2, 3:
					lease, _, err := pool.TryAcquireFor(context.Background(), aistudio.AccountSelection{
						ModelID: testRuntimeModel, Method: "generateContent", AccountID: id,
					})
					if err != nil || lease == nil {
						continue
					}
					if random.Intn(2) == 0 {
						lease.RebindEgress()
					}
					if random.Intn(2) == 0 {
						lease.RecordGenerationSent()
					}
					_ = lease.EgressKey()
					if err := lease.Release(); err != nil {
						select {
						case errs <- err:
						default:
						}
					}
				case 4:
					pool.EgressSnapshotFor([]string{keys[random.Intn(len(keys))]})
				}
			}
			for _, lease := range held {
				lease.release()
			}
		}(int64(worker + 1))
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if err := pool.VerifyEgressForTest(); err != nil {
		t.Fatal(err)
	}
	for _, status := range pool.EgressSnapshot() {
		for _, lane := range status.Lanes {
			if lane.Pending != 0 {
				t.Fatalf("出口 %s 仍有预占 %d", status.Label, lane.Pending)
			}
		}
	}
}
