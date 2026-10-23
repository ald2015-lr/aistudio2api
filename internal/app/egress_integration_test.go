package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/api"
	"github.com/Mag1cFall/AIStudio2API/internal/camoufoxnative"
)

// 出口限流判定成立期间的行为（走真实的选号、冷却、冷启动闸门）。判定多数用 forceVerdictForTest 写入（直连出口），
// 需要按账号降级等真实判定的测试直接喂观察结果。依赖账户池令牌桶与出口分组（WP-B2）的测试在账户池还是骨架时跳过

// egressCoverageModel 为只有备用账号支持的模型（驻留 Worker 不能服务它，用于补覆盖）
const egressCoverageModel = "egress-coverage-model"

// egressEnv 为出口测试的环境：纯 Go 后端（启动返回 stub Worker，耗时 launchDelay），warm 中的账号已有驻留 Worker，其余为备用；
// 接上判定器与冷启动闸门；launched 记录冷启动的账号
type egressEnv struct {
	service  *trackedService
	pool     *aistudio.AccountPool
	manager  *accountWorkerManager
	mu       sync.Mutex
	launched []string
}

func newEgressEnv(t *testing.T, warm []string, standby []string, warmTarget int, maxActive int, launchDelay time.Duration) *egressEnv {
	t.Helper()
	manager, pool, requests := partitionTestManager(t, append(slices.Clone(warm), standby...), nil, warmTarget, maxActive, 0, 1)
	env := &egressEnv{pool: pool, manager: manager}
	manager.launch = func(ctx context.Context, accountID string, _ camoufoxnative.Options) (*aistudio.NativeWorker, error) {
		env.mu.Lock()
		env.launched = append(env.launched, accountID)
		env.mu.Unlock()
		select {
		case <-time.After(launchDelay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		worker, _ := aistudio.NewStubWorker(accountID, 0)
		return worker, nil
	}
	for _, accountID := range warm {
		worker, _ := aistudio.NewStubWorker(accountID, 0)
		manager.accounts[accountID].worker = worker
		manager.accounts[accountID].warm.Store(true)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	service := retireTestService(ctx, manager, pool, requests)
	service.service = &rateUpstream{}
	service.timeout.Store(int64(time.Minute))
	service.egress = newEgressGuard(pool, requests)
	gate := service.egressColdStartGate()
	manager.coldStartGate.Store(&gate)
	env.service = service
	t.Cleanup(service.egress.relaxWG.Wait)
	return env
}

func (env *egressEnv) launches() []string {
	env.mu.Lock()
	defer env.mu.Unlock()
	return slices.Clone(env.launched)
}

// forceEgressLimited 把直连出口的判定写成成立、限速生效，建议重试时间为 retryIn 之后
func forceEgressLimited(service *trackedService, retryIn time.Duration) egressVerdict {
	verdict := egressVerdict{
		Mode: egressLimited, Limited: true, Throttled: true, Label: "直连", Limit: egressMinLimit, RetryAt: time.Now().Add(retryIn),
	}
	service.egress.forceVerdictForTest("", verdict)
	return verdict
}

// requireEgressPool 账户池还是出口骨架（WP-B2 未合入：没有出口分组与令牌桶）时跳过
func requireEgressPool(t *testing.T, pool *aistudio.AccountPool) {
	t.Helper()
	if len(pool.EgressSnapshot()) == 0 {
		t.Skip("账户池的出口分组与令牌桶尚未接入（WP-B2 合入后运行）")
	}
}

// addCoverageModel 让账号的目录同时支持 egressCoverageModel
func addCoverageModel(t *testing.T, pool *aistudio.AccountPool, accountIDs ...string) {
	t.Helper()
	for _, accountID := range accountIDs {
		if err := pool.SetCatalog(accountID, aistudio.BenefitTierFree, []aistudio.Model{
			{ID: testRuntimeModel, Methods: []string{"generateContent"}, Capabilities: map[string]bool{"chat_model": true}},
			{ID: egressCoverageModel, Methods: []string{"generateContent"}, Capabilities: map[string]bool{"chat_model": true}},
		}); err != nil {
			t.Fatal(err)
		}
	}
}

// coolModel 给账号在模型上写入 d 的冷却（reason 为原因）
func coolModel(t *testing.T, pool *aistudio.AccountPool, accountID string, model string, d time.Duration, reason string) {
	t.Helper()
	if err := pool.MarkCooldownIfGeneration(accountID, model, pool.ModelAccessGeneration(accountID), time.Now(), time.Now().Add(d), reason); err != nil {
		t.Fatal(err)
	}
}

// TestEgressLimitedShortCooldownNoStrike 判定成立期间裸 429 只冷却本通道（测试改小为 200ms）、不建档位，日志写“出口限流不升档 | 出口=直连”；
// 冷却到期后再失败仍是短冷却，不升到第 2 档
func TestEgressLimitedShortCooldownNoStrike(t *testing.T) {
	upstream := &bareRateUpstream{limited: map[string]bool{"a@example.com": true}, midStream: map[string]bool{}}
	service, pool := firstEventTestService(t, upstream, 0, "a@example.com")
	service.rateLimitLadder = testRateLadder
	service.rateLimits = newRateLimitGuard()
	service.egress = newEgressGuard(pool, service.requests)
	service.egress.shortCooldown = 200 * time.Millisecond
	forceEgressLimited(service, 7*time.Second)
	for round := 1; round <= 2; round++ {
		startedAt := time.Now()
		text, err, _ := runModelRequest(t, service, fmt.Sprintf("req-egress-short-%d", round), "a@example.com", testRuntimeModel)
		var rpcError *aistudio.RPCError
		if text != "" || !errors.As(err, &rpcError) || rpcError.StatusCode != http.StatusTooManyRequests {
			t.Fatalf("第 %d 次：text=%q err=%v，期望固定账号的请求按原错误结束", round, text, err)
		}
		until, reason, ok := modelCooldownUntil(t, pool, "a@example.com", testRuntimeModel)
		if !ok || until.Sub(startedAt) > time.Second || !strings.HasPrefix(reason, "限流: 出口限流短冷却，") || strings.Contains(reason, "连续限流第") {
			t.Fatalf("第 %d 次冷却 = %s 后、原因 %q，期望约 200ms 的出口限流短冷却、没有档位", round, until.Sub(startedAt), reason)
		}
		if got := countSourceLogs(service.requests, "", "账号冷却 | 类型=限流", "出口限流不升档 | 出口=直连"); got != round {
			t.Fatalf("“出口限流不升档”日志 %d 条，期望 %d 条", got, round)
		}
		time.Sleep(time.Until(until) + 20*time.Millisecond)
	}
	if countSourceLogs(service.requests, "", "连续限流=第") != 0 || countSourceLogs(service.requests, "", "号池级限流") != 0 {
		t.Fatal("判定成立期间不应走逐级冷却与号池级判定")
	}
}

// TestEgressDemoteWritesAccountCooldown 判定期间账号连续被拒、与出口按比例拒绝区分得开时写 30 分钟账号冷却（原因不以“连续限流第”开头，
// 进入判定时的撤回不动它），成为死槽位并登记淘汰
func TestEgressDemoteWritesAccountCooldown(t *testing.T) {
	upstream := &bareRateUpstream{limited: map[string]bool{"a@example.com": true}, midStream: map[string]bool{}}
	service, pool := firstEventTestService(t, upstream, 0, "a@example.com", "b@example.com")
	service.rateLimitLadder = testRateLadder
	service.egress = newEgressGuard(pool, service.requests)
	service.egress.shortCooldown = 100 * time.Millisecond
	now := time.Now()
	for index := range 10 {
		service.egress.observeOutcome(egressObservation{AccountID: coldAccount(index), Outcome: egressBare, At: now})
	}
	if !service.egress.anyLimited() {
		t.Fatal("判定应成立")
	}
	// 出口比例降到约 0.2：动态门槛 3 次
	for index := range 50 {
		service.egress.observeOutcome(egressObservation{AccountID: fmt.Sprintf("ok-%d@example.com", index), Outcome: egressSuccess, At: now})
	}
	for round := 1; round <= 3; round++ {
		runModelRequest(t, service, fmt.Sprintf("req-egress-demote-%d", round), "a@example.com", testRuntimeModel)
		if round < 3 {
			until, _, _ := modelCooldownUntil(t, pool, "a@example.com", testRuntimeModel)
			time.Sleep(time.Until(until) + 20*time.Millisecond)
		}
	}
	until, reason, ok := modelCooldownUntil(t, pool, "a@example.com", testRuntimeModel)
	if !ok || time.Until(until) < egressDemoteCooldown-time.Minute || !strings.HasPrefix(reason, "限流: 出口限流期间账号持续被拒（连续 3 次），") {
		t.Fatalf("降级冷却 = %s、原因 %q，期望 30 分钟、出口限流期间账号持续被拒", time.Until(until).Round(time.Second), reason)
	}
	if strings.HasPrefix(reason, "限流: 连续限流第") {
		t.Fatal("降级冷却的原因不能以“连续限流第”开头（撤回只动逐级冷却）")
	}
	if countSourceLogs(service.requests, "", "账号冷却 | 类型=限流", "出口限流按账号冷却 | 连续=3 | 出口=直连") != 1 {
		t.Fatalf("缺少降级冷却日志：%v", serviceLogMessages(service.requests))
	}
	if !waitUntil(5*time.Second, func() bool {
		return service.workers.retirePending.Load() > 0 || !service.workers.accounts["a@example.com"].warm.Load()
	}) {
		t.Fatal("降级后的驻留 Worker 应登记淘汰（死槽位）")
	}
	if entry := service.egress.status(); len(entry) != 1 || entry[0].Demoted != 1 {
		t.Fatalf("状态 = %+v，期望降级 1 个", entry)
	}
}

// TestEgressEnterRelaxesLadder 进入判定时撤回出口上第 2 档以上的逐级冷却：第 3 档的冷却缩短，登记的淘汰随之撤销
func TestEgressEnterRelaxesLadder(t *testing.T) {
	env := newEgressEnv(t, []string{"a@example.com"}, []string{"b@example.com"}, 1, 2, 0)
	requireEgressPool(t, env.pool)
	coolModel(t, env.pool, "a@example.com", testRuntimeModel, rateLimitLevel3, "限流: 连续限流第 3 档，Quota exceeded")
	env.service.retireCooledWorker("a@example.com", testRuntimeModel, aistudio.QuotaCooldown{Until: time.Now().Add(rateLimitLevel3)})
	if env.manager.retirePending.Load() == 0 && env.manager.accounts["a@example.com"].warm.Load() {
		t.Fatal("第 3 档冷却应登记淘汰")
	}
	now := time.Now()
	for index := range 10 {
		env.service.egress.observeOutcome(egressObservation{AccountID: coldAccount(index), Outcome: egressBare, At: now})
	}
	env.service.egress.relaxWG.Wait()
	until, reason, ok := modelCooldownUntil(t, env.pool, "a@example.com", testRuntimeModel)
	if ok && time.Until(until) > time.Minute {
		t.Fatalf("撤回后冷却还剩 %s（原因 %q），期望缩到约 10 秒", time.Until(until).Round(time.Second), reason)
	}
	if !hasLogContaining(env.service.requests, "出口限流：撤回逐级冷却 | 出口=直连 | 账号=1") {
		t.Fatalf("缺少撤回日志：%v", serviceLogMessages(env.service.requests))
	}
}

// TestEgressLimitedNoOnDemandColdStart 判定成立期间同出口的备用账号不现场提升、不扩容：热 A 在 10 秒短冷却时按“出口限速”排队，
// 主用模型以 CapacityBusyError{Reason: 出口限速} 结束，Until 为判定的建议重试时间；热 A 忙碌时同样不冷启动 B
func TestEgressLimitedNoOnDemandColdStart(t *testing.T) {
	t.Run("热 Worker 短冷却", func(t *testing.T) {
		env := newEgressEnv(t, []string{"a@example.com"}, []string{"b@example.com"}, 1, 2, 0)
		verdict := forceEgressLimited(env.service, 7*time.Second)
		coolModel(t, env.pool, "a@example.com", testRuntimeModel, egressLimitedCooldown, "限流: 出口限流短冷却，Quota exceeded")
		env.service.setPrimaryModels([]string{testRuntimeModel}, 300*time.Millisecond)
		ctx, cancel := context.WithTimeout(env.service.lifecycle, 10*time.Second)
		defer cancel()
		lease, err, elapsed := acquirePrimary(ctx, env.service, aistudio.PoolScopeNormal)
		var busy *aistudio.CapacityBusyError
		if lease != nil || !errors.As(err, &busy) || busy.Reason != waitReasonEgress {
			t.Fatalf("lease=%v err=%v，期望按出口限速排队超限", lease, err)
		}
		if !busy.Until.Equal(verdict.RetryAt) || elapsed > 3*time.Second {
			t.Fatalf("建议重试 = %s、用时 %s，期望判定的建议重试时间", busy.Until.Sub(verdict.RetryAt), elapsed)
		}
		if launched := env.launches(); len(launched) != 0 {
			t.Fatalf("判定成立期间冷启动了 %v", launched)
		}
		if waiters := dispatchWaiters(env.service); waiters != 0 {
			t.Fatalf("退出后仍有 %d 个等待者", waiters)
		}
		if entry := env.service.egress.status(); len(entry) != 1 || entry[0].LocalRejects1m != 1 {
			t.Fatalf("状态 = %+v，期望记一次本地 429", entry)
		}
	})
	t.Run("热 Worker 忙碌", func(t *testing.T) {
		env := newEgressEnv(t, []string{"a@example.com"}, []string{"b@example.com"}, 1, 2, 0)
		forceEgressLimited(env.service, 7*time.Second)
		release := holdLease(t, env.pool, "a@example.com")
		defer release()
		env.pool.SetPerAccountConcurrency(1)
		env.service.setPrimaryModels([]string{testRuntimeModel}, 300*time.Millisecond)
		ctx, cancel := context.WithTimeout(env.service.lifecycle, 10*time.Second)
		defer cancel()
		lease, err, _ := acquirePrimary(ctx, env.service, aistudio.PoolScopeNormal)
		var busy *aistudio.CapacityBusyError
		if lease != nil || !errors.As(err, &busy) {
			t.Fatalf("lease=%v err=%v，期望排队超限", lease, err)
		}
		time.Sleep(100 * time.Millisecond)
		if launched := env.launches(); len(launched) != 0 {
			t.Fatalf("热 Worker 忙碌时为同出口的备用账号冷启动了 %v（应不扩容）", launched)
		}
	})
}

// TestEgressCoverageStandby 驻留里没有能服务模型 X 的热 Worker：放回一个同出口的备用账号按补覆盖启动（日志来源=补覆盖）；
// 同时到达的第二个同模型请求不再启动另一个，等它就绪后共用
func TestEgressCoverageStandby(t *testing.T) {
	env := newEgressEnv(t, []string{"a@example.com"}, []string{"b@example.com", "c@example.com"}, 1, 3, 300*time.Millisecond)
	addCoverageModel(t, env.pool, "b@example.com", "c@example.com")
	forceEgressLimited(env.service, 7*time.Second)
	ctx, cancel := context.WithTimeout(env.service.lifecycle, 10*time.Second)
	defer cancel()
	selection := aistudio.AccountSelection{ModelID: egressCoverageModel, Method: "generateContent"}
	results := make(chan *aistudio.AccountLease, 2)
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for index := range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			time.Sleep(time.Duration(index) * 80 * time.Millisecond)
			lease, err := env.service.acquireWarmLease(ctx, selection)
			if err != nil {
				errs <- err
				return
			}
			results <- lease
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		t.Fatalf("补覆盖期间的请求失败：%v", err)
	}
	served := map[string]int{}
	for lease := range results {
		served[lease.Account().ID]++
		_ = lease.Release()
	}
	launched := env.launches()
	if len(launched) != 1 || served[launched[0]] != 2 {
		t.Fatalf("冷启动 %v、服务 %v，期望只补覆盖一个备用账号并由它服务两个请求", launched, served)
	}
	if !hasLogContaining(env.service.requests, "WAA Worker 就绪", "来源=补覆盖") {
		t.Fatalf("缺少补覆盖来源的就绪日志：%v", serviceLogMessages(env.service.requests))
	}
	if entry := env.service.egress.status(); len(entry) == 0 || entry[0].Lanes[0].ColdStarted1m != 1 {
		t.Fatalf("状态 = %+v，期望一分钟内闸门放行 1 个冷启动", entry)
	}
}

// TestEgressRemovedStandbyNotAllCooling 剔除同出口的备用账号后、热 Worker 都在长冷却、补覆盖配额又已满：按“驻留账号该模型额度已用完”排队，
// 不按全部冷却返回带几小时 Retry-After 的 429；非主用模型同样受出口排队上限约束
func TestEgressRemovedStandbyNotAllCooling(t *testing.T) {
	for _, primary := range []bool{true, false} {
		t.Run(fmt.Sprintf("主用模型=%v", primary), func(t *testing.T) {
			env := newEgressEnv(t, []string{"a@example.com"}, []string{"b@example.com"}, 1, 2, 0)
			forceEgressLimited(env.service, 7*time.Second)
			coolModel(t, env.pool, "a@example.com", testRuntimeModel, 2*time.Hour, "每日限额")
			for range egressCoveragePerMinute {
				release, ok := env.service.egress.admitColdStart("", false, testRuntimeModel, coldStartCoverage, 2)
				if !ok {
					t.Fatal("预先占满补覆盖配额失败")
				}
				release(true)
			}
			ctx, cancel := context.WithTimeout(env.service.lifecycle, 10*time.Second)
			defer cancel()
			var err error
			if primary {
				env.service.setPrimaryModels([]string{testRuntimeModel}, 300*time.Millisecond)
				_, err, _ = acquirePrimary(ctx, env.service, aistudio.PoolScopeNormal)
			} else {
				env.service.egressQueueWait = 300 * time.Millisecond
				_, err = env.service.acquireWarmLease(ctx, aistudio.AccountSelection{ModelID: testRuntimeModel, Method: "generateContent"})
			}
			var busy *aistudio.CapacityBusyError
			var cooling *aistudio.AllCoolingError
			if errors.As(err, &cooling) || !errors.As(err, &busy) || busy.Reason != waitReasonResidentExhausted {
				t.Fatalf("err=%v，期望按驻留账号该模型额度已用完排队超限，而不是全部冷却", err)
			}
			if time.Until(busy.Until) > time.Minute {
				t.Fatalf("建议重试 %s 后，期望不超过一分钟", time.Until(busy.Until).Round(time.Second))
			}
			if launched := env.launches(); len(launched) != 0 {
				t.Fatalf("补覆盖配额已满时仍冷启动了 %v", launched)
			}
		})
	}
}

// TestEgressLimitedNonPrimaryQueueCap 非主用模型按出口排队整个请求累计不超过上限（测试改小为 400ms）：超过按“出口限速”429 返回，
// 建议重试时间为判定的；请求里已累计的排队时间照样计入
func TestEgressLimitedNonPrimaryQueueCap(t *testing.T) {
	env := newEgressEnv(t, []string{"a@example.com"}, []string{"b@example.com"}, 1, 2, 0)
	verdict := forceEgressLimited(env.service, 9*time.Second)
	coolModel(t, env.pool, "a@example.com", testRuntimeModel, egressLimitedCooldown, "限流: 出口限流短冷却，Quota exceeded")
	env.service.egressQueueWait = 400 * time.Millisecond
	selection := aistudio.AccountSelection{ModelID: testRuntimeModel, Method: "generateContent"}
	ctx, cancel := context.WithTimeout(env.service.lifecycle, 10*time.Second)
	defer cancel()
	startedAt := time.Now()
	_, err := env.service.acquireWarmLease(ctx, selection)
	elapsed := time.Since(startedAt)
	var busy *aistudio.CapacityBusyError
	if !errors.As(err, &busy) || busy.Reason != waitReasonEgress || busy.Limit != 400*time.Millisecond || busy.Waited < 400*time.Millisecond {
		t.Fatalf("err=%v，期望非主用模型按出口限速排队 400ms 后返回", err)
	}
	if elapsed < 350*time.Millisecond || elapsed > 3*time.Second || !busy.Until.Equal(verdict.RetryAt) {
		t.Fatalf("用时 %s、建议重试偏差 %s", elapsed, busy.Until.Sub(verdict.RetryAt))
	}
	// 同一请求换号重试时已排队 300ms：这次只再排约 100ms
	requestCtx := withEgressWaitBudget(ctx)
	egressWaitBudgetFrom(requestCtx).add(300 * time.Millisecond)
	startedAt = time.Now()
	_, err = env.service.acquireWarmLease(requestCtx, selection)
	if elapsed := time.Since(startedAt); !errors.As(err, &busy) || busy.Waited < 400*time.Millisecond || elapsed > busy.Waited-150*time.Millisecond {
		t.Fatalf("err=%v 用时 %s，期望按整个请求累计的上限更早返回（已累计 300ms）", err, elapsed)
	}
	if entry := env.service.egress.status(); len(entry) != 1 || entry[0].LocalRejects1m != 2 {
		t.Fatalf("状态 = %+v，期望记两次本地 429", entry)
	}
}

// TestEgressLimitedOtherEgressExpands 备用账号在另一个（未受限的）代理出口上时照常提升
func TestEgressLimitedOtherEgressExpands(t *testing.T) {
	env := newEgressEnv(t, []string{"a@example.com"}, []string{"b@example.com"}, 1, 2, 0)
	requireEgressPool(t, env.pool)
	owner, err := env.pool.AcquireAccount(context.Background(), "b@example.com")
	if err != nil {
		t.Fatal(err)
	}
	accountConfig := owner.Account().Config
	accountConfig.Proxy = "http://other-egress.example:3128"
	saveErr := owner.SaveConfig(accountConfig)
	if err := errors.Join(saveErr, owner.Release()); err != nil {
		t.Fatal(err)
	}
	if key, _ := env.pool.EgressKeyOf("b@example.com"); key == "" {
		t.Skip("账户池还没有按账户代理分组（WP-B2 合入后运行）")
	}
	forceEgressLimited(env.service, 7*time.Second)
	coolModel(t, env.pool, "a@example.com", testRuntimeModel, 2*time.Hour, "每日限额")
	ctx, cancel := context.WithTimeout(env.service.lifecycle, 10*time.Second)
	defer cancel()
	lease, err := env.service.acquireWarmLease(ctx, aistudio.AccountSelection{ModelID: testRuntimeModel, Method: "generateContent"})
	if err != nil || lease.Account().ID != "b@example.com" {
		t.Fatalf("lease=%v err=%v，期望提升另一个出口上的 b", lease, err)
	}
	_ = lease.Release()
}

// TestEgressPausedColdStartNotWarmFailure 预热被出口闸门暂停（补位并发已满）不算启动失败、不进预热失败黑名单，预热等待原因写
// “出口限流中，冷启动按配额进行”，不写“槽位已满”；替补同样写明原因
func TestEgressPausedColdStartNotWarmFailure(t *testing.T) {
	env := newEgressEnv(t, []string{"a@example.com"}, []string{"b@example.com", "c@example.com"}, 3, 3, 0)
	forceEgressLimited(env.service, 7*time.Second)
	for range egressRefillConcurrencyMin {
		if _, ok := env.service.egress.admitColdStart("", false, "", coldStartRefill, 2); !ok {
			t.Fatal("预先占满补位并发失败")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := <-env.manager.StartPrewarm(ctx); err != nil {
		t.Fatalf("预热：%v", err)
	}
	env.manager.waitPrewarm()
	if launched := env.launches(); len(launched) != 0 {
		t.Fatalf("补位并发已满时预热冷启动了 %v", launched)
	}
	if failures := env.manager.recentWarmFailures(time.Now()); len(failures) != 0 {
		t.Fatalf("出口闸门暂停的账号进了预热失败黑名单：%v", failures)
	}
	if !hasLogContaining(env.service.requests, "预热等待", "原因="+egressColdStartPausedReason) ||
		hasLogContaining(env.service.requests, "预热等待", "槽位已满") {
		t.Fatalf("预热等待原因不对：%v", serviceLogMessages(env.service.requests))
	}
	if env.manager.runReplacement(ctx, aistudio.PoolScopeNormal, nil, "测试") {
		t.Fatal("补位并发已满时替补不应启动")
	}
	if !hasLogContaining(env.service.requests, "替补未启动 | 原因=测试 | "+egressColdStartPausedReason) {
		t.Fatalf("缺少替补暂停日志：%v", serviceLogMessages(env.service.requests))
	}
	if failures := env.manager.recentWarmFailures(time.Now()); len(failures) != 0 {
		t.Fatalf("替补暂停进了预热失败黑名单：%v", failures)
	}
}

// globalDailyUpstream 让 daily 中的账号对生成返回每日限额（整个账号冻结）的 429，其余正常返回
type globalDailyUpstream struct {
	rateUpstream
	daily map[string]bool
}

func (upstream *globalDailyUpstream) Generate(ctx context.Context, request aistudio.GenerateRequest) (<-chan aistudio.Event, error) {
	if upstream.daily[request.AccountID] {
		return nil, globalDailyQuotaError()
	}
	return upstream.rateUpstream.Generate(ctx, request)
}

// TestEgressLimitedRotationAllowed 判定成立期间每日额度长冷却的驻留 Worker 照常淘汰（轮换与淘汰不受出口判定影响），
// 淘汰后低于常驻数的补位走补位路径、照常冷启动
func TestEgressLimitedRotationAllowed(t *testing.T) {
	env := newEgressEnv(t, []string{"a@example.com"}, []string{"b@example.com"}, 1, 2, 0)
	env.service.service = &globalDailyUpstream{daily: map[string]bool{"a@example.com": true}}
	forceEgressLimited(env.service, 7*time.Second)
	runModelRequest(t, env.service, "req-egress-daily", "a@example.com", testRuntimeModel)
	if !waitUntil(5*time.Second, func() bool {
		return !env.manager.accounts["a@example.com"].warm.Load() && env.manager.accounts["b@example.com"].warm.Load()
	}) {
		t.Fatalf("每日额度冷却的 a 应被淘汰、b 补位就绪：启动 %v，日志 %v", env.launches(), serviceLogMessages(env.service.requests))
	}
	if hasLogContaining(env.service.requests, "出口限流暂停冷启动") {
		t.Fatal("低于常驻数的补位不应被暂停")
	}
	// 冷却登记发生在请求还持有租约时：淘汰扫描赶在租约释放前时走排空（替补先启动，来源=替补），赶在释放后时空闲即关、由预热补位
	// （来源=预热）。两种都按低于常驻数的补位经闸门放行（排空中的 Worker 按已让出槽位计），哪一种取决于请求释放租约的快慢
	// （生成路径在释放租约前还要把这次的“其他失败”交给出口判定器），这里不区分，只要求 b 经闸门放行一次
	if !hasLogContaining(env.service.requests, "WAA Worker 就绪", "来源=预热") &&
		!hasLogContaining(env.service.requests, "WAA Worker 就绪", "来源=替补") {
		t.Fatalf("补位应由预热或排空替补完成：%v", serviceLogMessages(env.service.requests))
	}
	if entry := env.service.egress.status(); len(entry) != 1 || len(entry[0].Lanes) == 0 || entry[0].Lanes[0].ColdStarted1m != 1 {
		t.Fatalf("状态 = %+v，期望补位经冷启动闸门放行一次", entry)
	}
}

// TestEgressStreamStartWait 判定成立期间不在主用列表里的模型写出响应头前多等 出口排队上限 + 余量（默认 35s），主用模型不变；没有判定时为 0
func TestEgressStreamStartWait(t *testing.T) {
	service, pool := firstEventTestService(t, &channelRecorder{}, 0, "a@example.com")
	service.egress = newEgressGuard(pool, service.requests)
	ctx := context.Background()
	if got := service.StreamStartWait(ctx, "other-model"); got != 0 {
		t.Fatalf("没有判定时 = %s", got)
	}
	forceEgressLimited(service, 7*time.Second)
	if got := service.StreamStartWait(ctx, "other-model"); got != egressQueueWaitMax+primaryStreamStartGrace || got > 50*time.Second {
		t.Fatalf("判定成立时非主用模型 = %s，期望 35s", got)
	}
	service.egressQueueWait = 2 * time.Second
	if got := service.StreamStartWait(ctx, "other-model"); got != 7*time.Second {
		t.Fatalf("出口排队上限 2s 时 = %s", got)
	}
	service.setPrimaryModels([]string{testRuntimeModel}, 10*time.Second)
	if got := service.StreamStartWait(ctx, testRuntimeModel); got != 10*time.Second+primaryStreamStartGrace {
		t.Fatalf("主用模型 = %s，期望不受出口判定影响", got)
	}
}

// transcribeAcquireUpstream 为转写测试的上游：像 transport_http 一样用请求的 context 经 AcquireFor 取生成租约
type transcribeAcquireUpstream struct {
	rateUpstream
	pool *aistudio.AccountPool
}

func (upstream *transcribeAcquireUpstream) Transcribe(ctx context.Context, request aistudio.TranscriptionRequest) (aistudio.TranscriptionResult, error) {
	lease, err := upstream.pool.AcquireFor(ctx, aistudio.AccountSelection{
		ModelID: strings.TrimPrefix(request.Model, "models/"), Method: "generateContent", AllowedAccountIDs: request.CandidateAccountIDs,
	})
	if err != nil {
		return aistudio.TranscriptionResult{}, err
	}
	_ = lease.Release()
	return aistudio.TranscriptionResult{Text: "ok"}, nil
}

var _ aistudio.TranscriptionService = (*transcribeAcquireUpstream)(nil)

// TestEgressTranscriptionWaitCap 转写不经 acquireLease，出口令牌桶已满时 AcquireFor 的累计等待受出口排队上限约束，按“出口限速”429 返回
func TestEgressTranscriptionWaitCap(t *testing.T) {
	service, pool := firstEventTestService(t, &channelRecorder{}, 0, "a@example.com")
	requireEgressPool(t, pool)
	service.service = &transcribeAcquireUpstream{pool: pool}
	// Transcribe 经 dataRequestContext 取请求上下文，要求服务处于运行状态
	service.state.Store(serviceRunning)
	service.dataContext = service.lifecycle
	service.egressQueueWait = 300 * time.Millisecond
	pool.SetEgressRateLimit("", aistudio.EgressLaneNormal, 1)
	defer pool.SetEgressRateLimit("", aistudio.EgressLaneNormal, 0)
	startedAt := time.Now()
	_, err := service.Transcribe(context.Background(), aistudio.TranscriptionRequest{ID: "req-egress-transcribe", Model: testRuntimeModel})
	var busy *aistudio.CapacityBusyError
	if !errors.As(err, &busy) || busy.Reason != waitReasonEgress || time.Since(startedAt) > 5*time.Second {
		t.Fatalf("err=%v 用时 %s，期望约 300ms 后按出口限速返回", err, time.Since(startedAt))
	}
}

// TestEgressLimitedConcurrentRequests 30 个并发请求、出口限速 5 次/分钟（从空桶开始）：发送不超过 限速 + 桶容量，其余排队到上限后 429；
// 结束后没有泄漏的预占与租约
func TestEgressLimitedConcurrentRequests(t *testing.T) {
	accounts := make([]string, 10)
	for index := range accounts {
		accounts[index] = fmt.Sprintf("conc-%d@example.com", index)
	}
	env := newEgressEnv(t, accounts, nil, len(accounts), len(accounts), 0)
	requireEgressPool(t, env.pool)
	upstream := &rateUpstream{}
	env.service.service = upstream
	env.service.egressQueueWait = 500 * time.Millisecond
	forceEgressLimited(env.service, 7*time.Second)
	env.pool.SetEgressRateLimit("", aistudio.EgressLaneNormal, 5)
	defer env.pool.SetEgressRateLimit("", aistudio.EgressLaneNormal, 0)
	var limited atomic.Int32
	var wg sync.WaitGroup
	for index := range 30 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err, _ := runModelRequest(t, env.service, fmt.Sprintf("req-egress-conc-%d", index), "", testRuntimeModel)
			var busy *aistudio.CapacityBusyError
			if errors.As(err, &busy) && busy.Reason == waitReasonEgress {
				limited.Add(1)
			}
		}()
	}
	wg.Wait()
	sent := len(upstream.called())
	if sent > 5+egressMinBurstForTest || int(limited.Load())+sent < 30 {
		t.Fatalf("发送 %d 次、出口限速 429 %d 次，期望发送不超过限速 + 桶容量、其余按出口限速返回", sent, limited.Load())
	}
	for _, status := range env.pool.EgressSnapshot() {
		for _, lane := range status.Lanes {
			if lane.Pending != 0 {
				t.Fatalf("出口 %s 通道 %d 还有 %d 个预占没有退还", status.Label, lane.Lane, lane.Pending)
			}
		}
	}
	assertNoBusyAccounts(t, env.pool)
}

// egressMinBurstForTest 为限速 5 次/分钟时的令牌桶容量（max(2, ceil(5×0.1))）
const egressMinBurstForTest = 2

// TestWorkerReadySourceLogged “WAA Worker 就绪”末尾写启动来源：预热、扩容、按需、补覆盖、重开、替补
func TestWorkerReadySourceLogged(t *testing.T) {
	ids := []string{"src-a@example.com", "src-b@example.com", "src-c@example.com", "src-d@example.com", "src-e@example.com", "src-f@example.com"}
	manager, _, requests := partitionTestManager(t, ids, nil, 1, len(ids), 0, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := <-manager.StartPrewarm(ctx); err != nil {
		t.Fatal(err)
	}
	manager.waitPrewarm()
	warm := manager.WarmAccountIDs()
	standby := func() string {
		active := manager.WarmAccountIDs()
		for _, accountID := range ids {
			if !slices.Contains(active, accountID) {
				return accountID
			}
		}
		t.Fatal("没有备用账号了")
		return ""
	}
	<-manager.expandInBackground(standby(), testRuntimeModel)
	if _, err := manager.promote(ctx, standby(), testRuntimeModel); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.promote(withWorkerStartSource(ctx, workerStartCoverage), standby(), testRuntimeModel); err != nil {
		t.Fatal(err)
	}
	// 生成请求的尝试把发送时的冷启动标为重开（见 generateWithRetry）
	if _, err := manager.Worker(withWorkerStartSource(ctx, workerStartReopen), standby(), testRuntimeModel); err != nil {
		t.Fatal(err)
	}
	if !manager.runReplacement(ctx, aistudio.PoolScopeNormal, nil, "测试") {
		t.Fatalf("替补没有启动：%v", serviceLogMessages(requests))
	}
	for _, source := range []string{workerStartPrewarm, workerStartExpand, workerStartOnDemand, workerStartCoverage, workerStartReopen, workerStartReplacement} {
		if got := countSourceLogs(requests, "", "WAA Worker 就绪 | 页面模型=", "| 来源="+source); got != 1 {
			t.Fatalf("来源=%s 的就绪日志 %d 条（预热后驻留 %v），日志 %v", source, got, warm, serviceLogMessages(requests))
		}
	}
}

// TestOnDemandLogsUnconditional “按需扩容”“按需替换”无条件写并标明是否超出常驻；预热不写“按需扩容”
func TestOnDemandLogsUnconditional(t *testing.T) {
	ids := []string{"od-a@example.com", "od-b@example.com", "od-c@example.com"}
	manager, _, requests := partitionTestManager(t, ids, nil, 2, 3, 0, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := <-manager.StartPrewarm(ctx); err != nil {
		t.Fatal(err)
	}
	manager.waitPrewarm()
	if hasLogContaining(requests, "按需扩容") {
		t.Fatalf("预热不应写按需扩容：%v", serviceLogMessages(requests))
	}
	for _, accountID := range ids {
		if _, err := manager.promote(ctx, accountID, testRuntimeModel); err != nil {
			t.Fatal(err)
		}
	}
	if !hasLogContaining(requests, "WAA Worker 按需扩容 | Worker=3/3 | 超出常驻=是") {
		t.Fatalf("超出常驻的扩容日志缺失：%v", serviceLogMessages(requests))
	}

	below, _, belowRequests := partitionTestManager(t, ids, nil, 3, 3, 0, 1)
	if _, err := below.promote(ctx, ids[0], testRuntimeModel); err != nil {
		t.Fatal(err)
	}
	if !hasLogContaining(belowRequests, "按需扩容 | Worker=1/3 | 超出常驻=否") {
		t.Fatalf("低于常驻数的现场启动也应写按需扩容：%v", serviceLogMessages(belowRequests))
	}

	full, _, fullRequests := partitionTestManager(t, ids[:2], nil, 1, 1, 0, 1)
	if _, err := full.promote(ctx, ids[0], testRuntimeModel); err != nil {
		t.Fatal(err)
	}
	if _, err := full.promote(ctx, ids[1], testRuntimeModel); err != nil {
		t.Fatal(err)
	}
	if !hasLogContaining(fullRequests, "按需替换 | Worker=1/1 | 超出常驻=否") {
		t.Fatalf("替换空闲 Worker 也应写按需替换：%v", serviceLogMessages(fullRequests))
	}
}

// TestEgressAdminAction 管理动作撤销与恢复判定后返回最新状态（状态里带出口）；未知编号按 404，未知动作报错；
// runtimeManager 把动作转给当前生成服务
func TestEgressAdminAction(t *testing.T) {
	env := newEgressEnv(t, []string{"a@example.com"}, nil, 1, 1, 0)
	env.service.state.Store(serviceRunning)
	now := time.Now()
	for index := range 10 {
		env.service.egress.observeOutcome(egressObservation{AccountID: coldAccount(index), Outcome: egressBare, At: now})
	}
	admin := &runtimeAdmin{pool: env.pool, service: env.service, requests: env.service.requests, workers: env.manager}
	manager := &runtimeManager{current: &runtimeGeneration{admin: admin}}
	ctx := context.Background()
	id := aistudio.EgressID("")
	status, err := manager.EgressAction(ctx, id, api.EgressActionWithdraw)
	if err != nil {
		t.Fatal(err)
	}
	if len(status.Egress) != 1 || status.Egress[0].Mode != "rejected" || status.Egress[0].RejectedBy != "管理操作" || status.Egress[0].ID != id {
		t.Fatalf("撤销后状态 = %+v", status.Egress)
	}
	status, err = admin.EgressAction(ctx, id, api.EgressActionResume)
	if err != nil || len(status.Egress) != 1 || status.Egress[0].Mode != "normal" {
		t.Fatalf("恢复后状态 = %+v，err=%v", status.Egress, err)
	}
	var notFound interface{ HTTPStatus() int }
	if _, err := admin.EgressAction(ctx, "000000000000", api.EgressActionWithdraw); !errors.As(err, &notFound) || notFound.HTTPStatus() != http.StatusNotFound {
		t.Fatalf("未知编号 err=%v，期望 404", err)
	}
	if _, err := admin.EgressAction(ctx, id, "bogus"); err == nil {
		t.Fatal("未知动作应报错")
	}
}
