package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/camoufoxnative"
	"github.com/Mag1cFall/AIStudio2API/internal/config"
)

// 补充池 Worker 的生命周期（见 extra_pool.go 批 2）：补充池预热、请求扩容入口、空闲回收、原地接手、对账（归还与释放）、残留标记、
// IP 池容量与状态接口

// extraCoolModel 为成员冷却的模型：账户目录里另有 testRuntimeModel（初始化模型，照常能用）。测试账户只启用 Playground 通道，
// 在它上面写冷却同时满足状态口径（成员）与长冷却口径（冷却淘汰、冷却轮换，即情形 B）
const extraCoolModel = "extra-cool-model"

// extraEnv 为补充池生命周期测试的环境：普通账户（Free）与 Pro 账户（PRO_POOL=true），纯 Go 后端（启动返回 stub Worker），
// launched 记录冷启动的账户，fail 中的账户启动失败
type extraEnv struct {
	ctx      context.Context
	manager  *accountWorkerManager
	pool     *aistudio.AccountPool
	requests *requestRegistry
	service  *trackedService
	mu       sync.Mutex
	launched []string
	closed   []func() int
	fail     map[string]error
	gate     chan struct{}
}

func newExtraEnv(t *testing.T, normalIDs []string, proIDs []string, normalWarm, normalMax, proWarm, proMax, extraWarm, extraMax int) *extraEnv {
	t.Helper()
	manager, pool, requests := proPartitionManager(t, normalIDs, nil, proIDs, normalWarm, normalMax, proWarm, proMax)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	env := &extraEnv{ctx: ctx, manager: manager, pool: pool, requests: requests, fail: make(map[string]error)}
	manager.launch = func(launchCtx context.Context, accountID string, _ camoufoxnative.Options) (*aistudio.NativeWorker, error) {
		env.mu.Lock()
		env.launched = append(env.launched, accountID)
		failure, gate := env.fail[accountID], env.gate
		env.mu.Unlock()
		if gate != nil {
			select {
			case <-gate:
			case <-launchCtx.Done():
				return nil, launchCtx.Err()
			}
		}
		if failure != nil {
			return nil, failure
		}
		worker, closed := aistudio.NewStubWorker(accountID, 0)
		env.mu.Lock()
		env.closed = append(env.closed, closed)
		env.mu.Unlock()
		return worker, nil
	}
	env.service = retireTestService(ctx, manager, pool, requests)
	env.setExtra(true, extraWarm, extraMax)
	return env
}

// setExtra 写入补充池设置与账户池的补充池规则（与装配、热更新相同）
func (env *extraEnv) setExtra(enabled bool, warm, maxActive int) {
	env.manager.setExtraSettings(extraPoolSettings{
		enabled: enabled, mode: config.ExtraPoolModeShare, defaultPaid: true, warm: warm, max: maxActive,
	})
	env.pool.SetExtraPoolRules(aistudio.ExtraPoolRules{Enabled: enabled, DefaultPaid: true})
}

// member 让账户成为补充池成员：目录为 testRuntimeModel（照常能用）与 extraCoolModel，后者冷却 2 小时
func (env *extraEnv) member(t *testing.T, tier aistudio.BenefitTier, accountIDs ...string) {
	t.Helper()
	for _, accountID := range accountIDs {
		if err := env.pool.SetCatalog(accountID, tier, []aistudio.Model{
			{ID: testRuntimeModel, Methods: []string{"generateContent", "countTokens"}, Capabilities: map[string]bool{"chat_model": true}},
			{ID: extraCoolModel, Methods: []string{"generateContent", "countTokens"}, Capabilities: map[string]bool{"chat_model": true}},
		}); err != nil {
			t.Fatal(err)
		}
		coolModel(t, env.pool, accountID, extraCoolModel, 2*time.Hour, "每日限额")
	}
}

// recover 清除成员的模型冷却：账户恢复为完全可用（不再是成员）
func (env *extraEnv) recover(t *testing.T, accountIDs ...string) {
	t.Helper()
	for _, accountID := range accountIDs {
		if err := env.pool.ClearCooldownIfGeneration(accountID, extraCoolModel, env.pool.ModelAccessGeneration(accountID), time.Now()); err != nil {
			t.Fatal(err)
		}
	}
}

func (env *extraEnv) launches() []string {
	env.mu.Lock()
	defer env.mu.Unlock()
	return slices.Clone(env.launched)
}

// liveBrowsers 返回启动后还没关闭的 stub 浏览器数
func (env *extraEnv) liveBrowsers() int {
	env.mu.Lock()
	defer env.mu.Unlock()
	live := 0
	for _, closed := range env.closed {
		live += 1 - min(closed(), 1)
	}
	return live
}

// pass 跑一次补充池对账并等补充池预热的启动结束
func (env *extraEnv) pass(t *testing.T) {
	t.Helper()
	env.service.extraPass(env.ctx)
	env.waitLaunches(t)
}

// waitLaunches 等补充池预热在途的启动都结束（每个成功的启动会接着补下一个）
func (env *extraEnv) waitLaunches(t *testing.T) {
	t.Helper()
	if !waitUntil(10*time.Second, func() bool {
		env.manager.extraMu.Lock()
		defer env.manager.extraMu.Unlock()
		return len(env.manager.extraStarting) == 0
	}) {
		t.Fatal("补充池预热的启动没有在期限内结束")
	}
	env.manager.extraLaunches.Wait()
}

// promote 现场启动账户的 Worker（按家分区）
func (env *extraEnv) promote(t *testing.T, accountIDs ...string) {
	t.Helper()
	for _, accountID := range accountIDs {
		if _, err := env.manager.promote(env.ctx, accountID, testRuntimeModel); err != nil {
			t.Fatalf("%s 启动失败: %v", accountID, err)
		}
	}
}

// workerOf 返回账户当前的 Worker 实例（没有时为 nil）
func (env *extraEnv) workerOf(accountID string) *aistudio.NativeWorker {
	account := env.manager.extraAccount(accountID)
	if account == nil {
		return nil
	}
	account.mu.Lock()
	defer account.mu.Unlock()
	return account.worker
}

// in 返回分区（按叠加视图）驻留的 Worker，排好序
func (env *extraEnv) in(partition aistudio.PoolScope) []string {
	warm := env.manager.warmAccountIDsIn(partition)
	slices.Sort(warm)
	return warm
}

// extraIDsOf 返回一组带编号的测试账户
func extraIDsOf(prefix string, count int) []string {
	ids := make([]string, 0, count)
	for index := range count {
		ids = append(ids, fmt.Sprintf("%s-%02d@example.com", prefix, index))
	}
	return ids
}

// TestExtraStartOnlyColdAccounts 补充池预热只启动冷成员：已有主分区 Worker 的成员不被补充池启动（也不改记到补充池），完全没冷却的账户
// 不是成员；冷成员被启动并带上补充池标记，就绪行末尾写“分区=补充池”，主分区的就绪行不带这个字段
func TestExtraStartOnlyColdAccounts(t *testing.T) {
	env := newExtraEnv(t, []string{partitionNormalA, partitionNormalB}, []string{proPartitionA, proPartitionB}, 1, 3, 1, 3, 3, 4)
	env.member(t, aistudio.BenefitTierPro, proPartitionA, proPartitionB)
	env.member(t, aistudio.BenefitTierFree, partitionNormalA)
	env.promote(t, proPartitionA)
	before := len(env.launches())
	env.pass(t)
	if got := env.in(aistudio.PoolScopeExtra); !slices.Equal(got, []string{partitionNormalA, proPartitionB}) {
		t.Fatalf("补充池 Worker = %v，期望只有两个冷成员", got)
	}
	if got := env.launches()[before:]; len(got) != 2 || slices.Contains(got, proPartitionA) || slices.Contains(got, partitionNormalB) {
		t.Fatalf("补充池预热启动了 %v：不应启动已有 Worker 的成员或不是成员的账户", got)
	}
	if env.manager.extraOwnedHas(proPartitionA) || !slices.Contains(env.in(aistudio.PoolScopePro), proPartitionA) {
		t.Fatal("已有主分区 Worker 的成员应留在家分区")
	}
	if got := countLogs(env.requests, "WAA Worker 就绪", "来源=预热", "起因=空槽位 | 分区=补充池"); got != 2 {
		t.Fatalf("补充池 Worker 的就绪行 %d 条，期望 2 条（末尾带分区=补充池）", got)
	}
	if countLogs(env.requests, "WAA Worker 就绪", "来源=按需", "分区=补充池") != 0 {
		t.Fatal("主分区 Worker 的就绪行不应带分区=补充池")
	}
	// 再对账一次：已补到成员数，不再启动
	env.pass(t)
	if got := len(env.launches()); got != before+2 {
		t.Fatalf("成员都已有 Worker 后不应再启动，冷启动 %d 次", got-before)
	}
}

// TestExtraStartCountsInExtra 补充池 Worker 计入补充池、不占家分区的槽位：补充池启动之后家分区仍能扩容到自己的峰值
func TestExtraStartCountsInExtra(t *testing.T) {
	env := newExtraEnv(t, nil, []string{proPartitionA, proPartitionB, "pro-c@example.com"}, 0, 1, 1, 2, 1, 1)
	env.member(t, aistudio.BenefitTierPro, proPartitionA)
	env.pass(t)
	if got := env.in(aistudio.PoolScopeExtra); !slices.Equal(got, []string{proPartitionA}) {
		t.Fatalf("补充池 Worker = %v", got)
	}
	env.promote(t, proPartitionB, "pro-c@example.com")
	if got := env.in(aistudio.PoolScopePro); !slices.Equal(got, []string{proPartitionB, "pro-c@example.com"}) {
		t.Fatalf("家分区应能扩容到自己的峰值 2：%v", got)
	}
	members := env.manager.slotMembers()
	env.manager.rebalanceMu.Lock()
	pro, _ := env.manager.partitionOccupancyLocked(aistudio.PoolScopePro, members)
	extra, _ := env.manager.partitionOccupancyLocked(aistudio.PoolScopeExtra, members)
	env.manager.rebalanceMu.Unlock()
	if pro.slots != 2 || extra.slots != 1 {
		t.Fatalf("占用：Pro 分区 %d、补充池 %d，期望 2 与 1", pro.slots, extra.slots)
	}
}

// TestExtraPrewarmNoReplace 补充池已满（常驻 2、峰值 2）时补充池预热返回容量已满，不替换、不关闭任何 Worker
func TestExtraPrewarmNoReplace(t *testing.T) {
	ids := extraIDsOf("prewarm-full", 3)
	env := newExtraEnv(t, ids, nil, 0, 3, 0, 1, 2, 2)
	env.member(t, aistudio.BenefitTierFree, ids...)
	env.pass(t)
	extra := env.in(aistudio.PoolScopeExtra)
	if len(extra) != 2 {
		t.Fatalf("补充池 Worker = %v，期望补到 2", extra)
	}
	cold := ""
	for _, accountID := range ids {
		if !slices.Contains(extra, accountID) {
			cold = accountID
		}
	}
	ctx := withExtraStart(withWorkerStartSource(withBackgroundStartup(env.ctx), workerStartPrewarm), extraStartOptions{prewarm: true})
	if _, err := env.manager.ensureWorker(ctx, cold, "", false); !errors.Is(err, errAccountWorkerCapacity) {
		t.Fatalf("补充池已满时补充池预热 err = %v，期望容量已满", err)
	}
	if got := env.in(aistudio.PoolScopeExtra); !slices.Equal(got, extra) || countLogs(env.requests, "WAA Worker 停止") != 0 {
		t.Fatalf("补充池预热不应替换空闲 Worker：%v", got)
	}
	if env.manager.extraOwnedHas(cold) {
		t.Fatal("没有启动的账户不应带补充池标记")
	}
}

// TestExtraExpandReplacesWithinExtra 补充池已满、其中一个 Worker 对请求的模型是死槽位（在该模型上长时间冷却）：请求触发的扩容只替换
// 补充池里那一个（按需替换的起因、补充池内不接手），主分区不受影响；每分钟替换上限按补充池单独计
func TestExtraExpandReplacesWithinExtra(t *testing.T) {
	ids := extraIDsOf("expand", 3)
	env := newExtraEnv(t, append(slices.Clone(ids), partitionNormalA), nil, 1, 1, 0, 1, 2, 2)
	env.member(t, aistudio.BenefitTierFree, ids...)
	env.promote(t, partitionNormalA)
	env.pass(t)
	extra := env.in(aistudio.PoolScopeExtra)
	if len(extra) != 2 {
		t.Fatalf("补充池 Worker = %v", extra)
	}
	cold := ""
	for _, accountID := range ids {
		if !slices.Contains(extra, accountID) {
			cold = accountID
		}
	}
	dead := extra[0]
	coolModel(t, env.pool, dead, testRuntimeModel, 2*time.Hour, "每日限额")
	env.manager.replacements.limit.Store(1)
	<-env.manager.extraExpandInBackground(cold, testRuntimeModel)
	got := env.in(aistudio.PoolScopeExtra)
	if !slices.Contains(got, cold) || slices.Contains(got, dead) || len(got) != 2 {
		t.Fatalf("扩容应替换补充池里的死槽位 %s：补充池 Worker = %v", dead, got)
	}
	if env.manager.extraOwnedHas(dead) || !env.manager.extraOwnedHas(cold) {
		t.Fatal("被替换的补充池 Worker 应去掉标记，新启动的应带标记")
	}
	if got := env.in(aistudio.PoolScopeNormal); !slices.Equal(got, []string{partitionNormalA}) {
		t.Fatalf("主分区不应受影响：%v", got)
	}
	if countLogs(env.requests, "补充池 WAA Worker 按需替换") != 1 || countLogs(env.requests, "WAA Worker 停止", "起因=替换") != 1 ||
		countLogs(env.requests, "补充池接手 Worker") != 0 {
		t.Fatal("应写一行补充池 WAA Worker 按需替换并关闭死槽位，补充池内的替换不接手")
	}
	if !env.manager.replacementAllowed(context.Background(), aistudio.PoolScopeNormal) ||
		env.manager.replacementAllowed(context.Background(), aistudio.PoolScopeExtra) {
		t.Fatal("每分钟替换上限应按补充池单独计")
	}
}

// TestExtraWarmTargetFill 补充池预热补到常驻数与成员数取小：常驻 2、峰值 3，4 个成员时补到 2，只有 1 个成员时补到 1
func TestExtraWarmTargetFill(t *testing.T) {
	for _, tc := range []struct {
		members int
		want    int
	}{{4, 2}, {1, 1}} {
		t.Run(fmt.Sprintf("成员%d", tc.members), func(t *testing.T) {
			ids := extraIDsOf("fill", 4)
			env := newExtraEnv(t, ids, nil, 0, 4, 0, 1, 2, 3)
			env.member(t, aistudio.BenefitTierFree, ids[:tc.members]...)
			for range 3 {
				env.pass(t)
			}
			if got := len(env.in(aistudio.PoolScopeExtra)); got != tc.want {
				t.Fatalf("补充池 Worker %d 个，期望 %d", got, tc.want)
			}
			if got := len(env.launches()); got != tc.want {
				t.Fatalf("冷启动 %d 次，期望 %d", got, tc.want)
			}
		})
	}
}

// logsFrom 返回来源为 source 的运行日志
func logsFrom(requests *requestRegistry, source string) []string {
	requests.mu.Lock()
	defer requests.mu.Unlock()
	var messages []string
	for _, entry := range requests.logs {
		if entry.Source == source {
			messages = append(messages, entry.Message)
		}
	}
	return messages
}

// countFrom 返回来源为 source、包含全部片段的日志条数
func countFrom(requests *requestRegistry, source string, fragments ...string) int {
	count := 0
	for _, message := range logsFrom(requests, source) {
		matched := true
		for _, fragment := range fragments {
			matched = matched && strings.Contains(message, fragment)
		}
		if matched {
			count++
		}
	}
	return count
}

// TestExtraFillWaitsFirstWarm 首轮预热期间补充池预热不发起启动（写一行“首轮预热未完成”的预热等待），冷启动里没有补充池账户；
// 首轮全部出结果后才启动；状态接口的 FirstWarmPending 从不出现补充池
func TestExtraFillWaitsFirstWarm(t *testing.T) {
	ids := extraIDsOf("first-warm", 3)
	env := newExtraEnv(t, ids, nil, 1, 3, 0, 1, 2, 2)
	env.member(t, aistudio.BenefitTierFree, ids[1:]...)
	gate := make(chan struct{})
	env.mu.Lock()
	env.gate = gate
	env.mu.Unlock()
	first := env.manager.startFirstPrewarm(env.ctx)
	if !waitUntil(5*time.Second, func() bool { return len(env.launches()) == 1 }) {
		t.Fatalf("首轮预热应启动一个 Worker：%v", env.launches())
	}
	mainLaunch := env.launches()[0]
	if len(env.manager.firstWarmPendingIn(aistudio.PoolScopeAll)) == 0 {
		t.Fatal("首轮预热期间普通分区应还没出结果")
	}
	env.service.extraPass(env.ctx)
	time.Sleep(50 * time.Millisecond)
	if got := env.launches(); len(got) != 1 {
		t.Fatalf("首轮预热期间补充池不应发起启动：%v", got)
	}
	if countLogs(env.requests, "补充池 WAA Worker 预热等待", "原因=首轮预热未完成") != 1 {
		t.Fatal("首轮预热期间应写一行补充池预热等待（首轮预热未完成）")
	}
	for _, name := range env.manager.firstWarmPendingNames() {
		if name == aistudio.PoolScopeExtra.String() {
			t.Fatal("首轮预热状态不应出现补充池")
		}
	}
	env.mu.Lock()
	env.gate = nil
	env.mu.Unlock()
	close(gate)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	env.manager.waitPrewarm()
	if len(env.manager.firstWarmPendingIn(aistudio.PoolScopeAll)) != 0 {
		t.Fatal("首轮预热结束后不应还有待定分区")
	}
	env.pass(t)
	extra := env.launches()[1:]
	if len(extra) == 0 || slices.Contains(extra, mainLaunch) {
		t.Fatalf("首轮预热出结果后补充池应启动冷成员：%v（首轮启动了 %s）", extra, mainLaunch)
	}
	for _, accountID := range extra {
		if !env.manager.extraOwnedHas(accountID) {
			t.Fatalf("%s 应计入补充池", accountID)
		}
	}
}

// TestExtraLaunchRoom 补充池预热的在途上限：主预热不在运行时为 max(1,WSC−1)；主预热在运行时占满名额为 0、有空余名额为 1、
// 补充池已连续 2 分钟没有发起启动（防饿死）为 1；补充池自己在途的启动从上限里扣掉
func TestExtraLaunchRoom(t *testing.T) {
	env := newExtraEnv(t, []string{partitionNormalA}, nil, 1, 1, 0, 1, 1, 1)
	manager := env.manager
	manager.warmConcurrency.Store(3)
	recent := time.Now().UnixNano()
	manager.extraLastLaunch.Store(recent)
	if got := manager.extraLaunchRoom(); got != 2 {
		t.Fatalf("主预热不在运行：%d，期望 max(1,WSC−1)=2", got)
	}
	manager.fillActive.Store(true)
	defer manager.fillActive.Store(false)
	manager.fillInflight.Store(3)
	if got := manager.extraLaunchRoom(); got != 0 {
		t.Fatalf("主预热占满名额：%d，期望 0", got)
	}
	manager.fillInflight.Store(2)
	if got := manager.extraLaunchRoom(); got != 1 {
		t.Fatalf("主预热有空余名额：%d，期望 1", got)
	}
	manager.fillInflight.Store(3)
	manager.extraLastLaunch.Store(time.Now().Add(-extraStarveAfter - time.Second).UnixNano())
	if got := manager.extraLaunchRoom(); got != 1 {
		t.Fatalf("补充池连续 2 分钟没有启动：%d，期望 1（防饿死）", got)
	}
	manager.extraMu.Lock()
	manager.extraStarting = map[string]struct{}{partitionNormalA: {}}
	manager.extraMu.Unlock()
	if got := manager.extraLaunchRoom(); got != 0 {
		t.Fatalf("补充池已有一个在途：%d，期望 0", got)
	}
	manager.extraMu.Lock()
	manager.extraStarting = nil
	manager.extraMu.Unlock()
	manager.fillActive.Store(false)
	manager.warmConcurrency.Store(1)
	if got := manager.extraLaunchRoom(); got != 1 {
		t.Fatalf("WSC=1、主预热不在运行：%d，期望 1", got)
	}
	// 主预热占满名额且没有饿死时，低于目标的补充池写一行预热等待（主预热占满启动名额），不发起启动
	env.member(t, aistudio.BenefitTierFree, partitionNormalA)
	manager.warmConcurrency.Store(2)
	manager.fillActive.Store(true)
	manager.fillInflight.Store(2)
	manager.extraLastLaunch.Store(recent)
	env.pass(t)
	if len(env.launches()) != 0 || countLogs(env.requests, "补充池 WAA Worker 预热等待 | Worker=0/1 | 原因=主预热占满启动名额") != 1 {
		t.Fatalf("主预热占满名额时补充池不应启动、应写预热等待：冷启动 %v", env.launches())
	}
}

// TestExtraUsesBackgroundSlots 补充池预热按后台冷启动名额排队：名额被占满、补充池预热在排队时来了一个主号池按需启动，主号池先取得名额
func TestExtraUsesBackgroundSlots(t *testing.T) {
	manager, gate := slotTestManager(t, "camoufox-test", 1, "slot-a@example.com", "slot-m@example.com", "slot-n@example.com")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	service := retireTestService(ctx, manager, manager.pool, manager.requests)
	manager.setExtraSettings(extraPoolSettings{enabled: true, mode: config.ExtraPoolModeShare, defaultPaid: true, warm: 1, max: 1})
	manager.pool.SetExtraPoolRules(aistudio.ExtraPoolRules{Enabled: true, DefaultPaid: true})
	(&extraEnv{pool: manager.pool}).member(t, aistudio.BenefitTierFree, "slot-m@example.com")
	results := make(chan error, 2)
	go func() {
		_, err := manager.ensureWorker(withBackgroundStartup(ctx), "slot-a@example.com", "", false)
		results <- err
	}()
	gate.waitStarted(t, "slot-a@example.com", "占满名额的预热")
	service.extraPass(ctx)
	waitStartupUsage(t, &manager.startupSlots, 1, 0, 1)
	if gate.isStarted("slot-m@example.com") {
		t.Fatal("名额已满时补充池预热不应开始启动")
	}
	go func() {
		_, err := manager.Worker(ctx, "slot-n@example.com", testRuntimeModel)
		results <- err
	}()
	gate.waitStarted(t, "slot-n@example.com", "主号池的按需启动")
	if gate.isStarted("slot-m@example.com") {
		t.Fatal("主号池的按需启动应先取得名额，补充池预热继续排队")
	}
	gate.open()
	for range 2 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	manager.extraLaunches.Wait()
	if !gate.isStarted("slot-m@example.com") || !gate.launchedInBackground("slot-m@example.com") || !manager.extraOwnedHas("slot-m@example.com") {
		t.Fatal("名额空出后补充池预热应按后台启动完成，并计入补充池")
	}
}

// TestExtraColdStartGateExpand 出口限流判定成立时补充池的冷启动一律按超出常驻暂停（不占补位并发），写一行出口限流的预热等待；
// 同时主分区低于常驻数的补位照常放行
func TestExtraColdStartGateExpand(t *testing.T) {
	env := newEgressEnv(t, nil, []string{"gate-a@example.com", "gate-m@example.com"}, 1, 2, 0)
	requireEgressPool(t, env.pool)
	env.manager.setExtraSettings(extraPoolSettings{enabled: true, mode: config.ExtraPoolModeShare, defaultPaid: true, warm: 1, max: 1})
	env.pool.SetExtraPoolRules(aistudio.ExtraPoolRules{Enabled: true, DefaultPaid: true})
	(&extraEnv{pool: env.pool}).member(t, aistudio.BenefitTierFree, "gate-m@example.com")
	forceEgressLimited(env.service, 30*time.Second)
	ctx := env.service.lifecycle
	env.service.extraPass(ctx)
	env.manager.extraLaunches.Wait()
	if got := env.launches(); len(got) != 0 {
		t.Fatalf("出口限流时补充池不应冷启动：%v", got)
	}
	if env.manager.extraOwnedHas("gate-m@example.com") || env.manager.extraPausedUntil.Load() <= time.Now().UnixNano() {
		t.Fatal("补充池的冷启动被闸门暂停后应进入退避，不留标记")
	}
	env.service.extraPass(ctx)
	if countLogs(env.service.requests, "补充池 WAA Worker 预热等待", "原因="+egressColdStartPausedReason) != 1 {
		t.Fatal("退避期内应写一行出口限流的补充池预热等待")
	}
	if _, err := env.manager.promote(ctx, "gate-a@example.com", testRuntimeModel); err != nil {
		t.Fatalf("主分区低于常驻数的补位应照常放行: %v", err)
	}
	if got := env.launches(); !slices.Equal(got, []string{"gate-a@example.com"}) {
		t.Fatalf("冷启动 = %v，期望只有主分区的补位", got)
	}
}

// TestExtraIdleReap 补充池 Worker 超出补充池常驻数（调小常驻数后）、空闲满 5 分钟：空闲回收关闭一个，起因为空闲回收，名称为补充池 WAA Worker；
// 关闭后标记去掉；主分区不受影响
func TestExtraIdleReap(t *testing.T) {
	ids := extraIDsOf("reap", 2)
	env := newExtraEnv(t, append(slices.Clone(ids), partitionNormalA), nil, 1, 1, 0, 1, 2, 3)
	env.member(t, aistudio.BenefitTierFree, ids...)
	env.promote(t, partitionNormalA)
	env.pass(t)
	if got := env.in(aistudio.PoolScopeExtra); !slices.Equal(got, ids) {
		t.Fatalf("补充池 Worker = %v", got)
	}
	env.setExtra(true, 1, 3)
	if !env.manager.reapIdleWorker(time.Now().Add(time.Hour)) {
		t.Fatal("补充池超出常驻数时应回收一个空闲 Worker")
	}
	for env.manager.reapIdleWorker(time.Now().Add(time.Hour)) {
	}
	got := env.in(aistudio.PoolScopeExtra)
	if len(got) != 1 || env.manager.extraOwnedCount() != 1 {
		t.Fatalf("回收后补充池 Worker = %v、标记 %v，期望各 1 个", got, env.manager.extraOwnedIDs())
	}
	if countLogs(env.requests, "补充池 WAA Worker 空闲回收 | Worker=1/3") != 1 || countLogs(env.requests, "WAA Worker 停止", "起因=空闲回收") != 1 {
		t.Fatal("应写一行补充池 WAA Worker 空闲回收，并以空闲回收关闭")
	}
	if !slices.Equal(env.in(aistudio.PoolScopeNormal), []string{partitionNormalA}) {
		t.Fatal("主分区不应被补充池的空闲回收影响")
	}
	// 补充池关闭后常驻数为 0：剩下的空闲补充池 Worker 同样被回收
	env.setExtra(false, 1, 3)
	for env.manager.reapIdleWorker(time.Now().Add(time.Hour)) {
	}
	if len(env.in(aistudio.PoolScopeExtra)) != 0 || env.manager.extraOwnedCount() != 0 {
		t.Fatal("补充池关闭后空闲的补充池 Worker 应被回收")
	}
}

// handOffEnv 为接手测试的环境：Pro 分区里有一个情形 B 的成员 Worker（victim，只在 extraCoolModel 上长时间冷却，两种口径都满足）、
// 一个没有冷却的备用 Pro 账号（spare，替补用），补充池常驻 2、峰值 3
func handOffEnv(t *testing.T, proWarm, proMax int) (*extraEnv, string, string) {
	t.Helper()
	victim, spare := proPartitionA, proPartitionB
	env := newExtraEnv(t, nil, []string{victim, spare}, 0, 1, proWarm, proMax, 2, 3)
	env.member(t, aistudio.BenefitTierPro, victim)
	// 备用账号的目录同样有 extraCoolModel、没有冷却：冷却淘汰与轮换要求分区里有能服务登记模型的可接替账户
	env.member(t, aistudio.BenefitTierPro, spare)
	env.recover(t, spare)
	env.promote(t, victim)
	if !env.pool.AccountLongCooling(victim, extraCoolModel, rotationMinRemaining) {
		t.Fatal("情形 B 的成员应同时满足长冷却口径")
	}
	return env, victim, spare
}

// TestExtraHandOff 主分区关闭成员的空闲 Worker 时由补充池原地接手（起因在白名单里、补充池低于常驻数）：没有停止行，浏览器与版本不变，
// 已打上补充池标记，家分区计数减一；替补照常启动；原日志行末尾写“去向=补充池接手”，另有一行“补充池接手 Worker”
func TestExtraHandOff(t *testing.T) {
	cases := []struct {
		name    string
		cause   string
		proWarm int
		proMax  int
		// trigger 走真实的关闭路径
		trigger func(t *testing.T, env *extraEnv, victim string, spare string)
		// line 为原日志行（带去向）；replacement 为替补是否照常启动
		line        []string
		replacement bool
	}{
		{
			name: "冷却淘汰", cause: workerStopCoolingRetire, proWarm: 1, proMax: 1,
			trigger: func(t *testing.T, env *extraEnv, victim string, _ string) {
				if !env.manager.retireWorker(victim, extraCoolModel) {
					t.Fatal("应登记淘汰")
				}
			},
			line: []string{"WAA Worker 冷却淘汰", "Pro Worker=0/1 | 去向=补充池接手"}, replacement: true,
		},
		{
			name: "冷却轮换", cause: workerStopCoolingRotation, proWarm: 1, proMax: 1,
			trigger: func(t *testing.T, env *extraEnv, victim string, _ string) {
				// 轮换只换就绪超过宽限、在热门模型上长时间冷却的 Worker：直接走 rotatePartition（热门模型的 5% 阈值另有测试）
				env.manager.extraAccount(victim).readyAt.Store(time.Now().Add(-time.Hour).UnixNano())
				rotated, _ := env.service.rotatePartition(env.ctx, aistudio.PoolScopePro, []string{victim}, nil, []string{extraCoolModel}, 1)
				if rotated != 1 {
					t.Fatalf("应轮换 1 个，实际 %d", rotated)
				}
				// 与 rotateCooledWorkers 相同：低于预热目标的部分由预热补齐，其余由替补
				plan := env.manager.replacementsFor([]evictedWorker{{partition: aistudio.PoolScopePro, models: []string{extraCoolModel}}})
				env.manager.StartPrewarm(env.ctx)
				env.manager.startReplacements(plan, "冷却轮换")
			},
			replacement: true,
		},
		{
			name: "回收冷却占位", cause: workerStopReclaimPlaceholder, proWarm: 1, proMax: 1,
			trigger: func(t *testing.T, env *extraEnv, victim string, _ string) {
				if got, err := env.manager.reclaimDeadWorker(env.ctx, aistudio.PoolScopePro, extraCoolModel); got != victim || err != nil {
					t.Fatalf("回收冷却占位 = %q %v", got, err)
				}
			},
			line: []string{"WAA Worker 回收冷却占位", "模型=" + extraCoolModel, "Pro Worker=0/1 | 去向=补充池接手"},
		},
		{
			name: "空闲回收", cause: workerStopIdleReclaim, proWarm: 0, proMax: 1,
			trigger: func(t *testing.T, env *extraEnv, _ string, _ string) {
				if !env.manager.reapIdleWorker(time.Now().Add(time.Hour)) {
					t.Fatal("超出常驻数的空闲 Worker 应被回收")
				}
			},
			line: []string{"Pro WAA Worker 空闲回收 | Worker=0/1 | 去向=补充池接手"},
		},
		{
			name: "按需替换", cause: workerStopReplace, proWarm: 1, proMax: 1,
			trigger: func(t *testing.T, env *extraEnv, _ string, spare string) {
				env.promote(t, spare)
			},
			line: []string{"Pro WAA Worker 按需替换 | Worker=1/1", "去向=补充池接手"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env, victim, spare := handOffEnv(t, tc.proWarm, tc.proMax)
			worker, generation := env.workerOf(victim), env.manager.WorkerGeneration(victim)
			tc.trigger(t, env, victim, spare)
			if !waitUntil(5*time.Second, func() bool { return env.manager.extraOwnedHas(victim) }) {
				t.Fatalf("%s 应被补充池接手，日志 = %q", tc.name, serviceLogMessages(env.requests))
			}
			if env.workerOf(victim) != worker || env.manager.WorkerGeneration(victim) != generation {
				t.Fatal("接手不应重启浏览器")
			}
			if countFrom(env.requests, victim, "WAA Worker 停止") != 0 {
				t.Fatal("接手不应写停止行")
			}
			if !slices.Equal(env.in(aistudio.PoolScopeExtra), []string{victim}) || slices.Contains(env.in(aistudio.PoolScopePro), victim) {
				t.Fatalf("接手后应计入补充池、不计入家分区：补充池 %v，Pro %v", env.in(aistudio.PoolScopeExtra), env.in(aistudio.PoolScopePro))
			}
			if countFrom(env.requests, victim, "补充池接手 Worker | 原分区=Pro 分区 | 起因="+tc.cause+" | 补充池 Worker=1/2") != 1 {
				t.Fatalf("应写一行补充池接手 Worker，日志 = %q", logsFrom(env.requests, victim))
			}
			if len(tc.line) > 0 && countLogs(env.requests, tc.line...) != 1 {
				t.Fatalf("原日志行应带去向，日志 = %q", serviceLogMessages(env.requests))
			}
			if tc.replacement && !waitUntil(5*time.Second, func() bool { return slices.Equal(env.in(aistudio.PoolScopePro), []string{spare}) }) {
				t.Fatalf("替补应照常在家分区启动：Pro %v", env.in(aistudio.PoolScopePro))
			}
			if env.manager.retirementCount() != 0 {
				t.Fatal("接手后不应留下淘汰登记")
			}
		})
	}
}

// TestExtraNoHandOff 不接手的情况照常关闭：平均调用轮换、冷却清扫、不可调度释放（起因不在白名单）、补充池已到常驻数、补充池关闭
func TestExtraNoHandOff(t *testing.T) {
	cases := []struct {
		name    string
		cause   string
		prepare func(t *testing.T, env *extraEnv)
	}{
		{name: "平均调用轮换", cause: workerStopBalanced},
		{name: "冷却清扫", cause: workerStopCoolingSweep},
		{name: "不可调度释放", cause: workerStopUnschedulable},
		{name: "补充池已到常驻数", cause: workerStopCoolingRetire, prepare: func(t *testing.T, env *extraEnv) {
			env.setExtra(true, 1, 3)
			env.member(t, aistudio.BenefitTierFree, partitionNormalA)
			env.pass(t)
			if !slices.Equal(env.in(aistudio.PoolScopeExtra), []string{partitionNormalA}) {
				t.Fatalf("补充池应已到常驻数 1：%v", env.in(aistudio.PoolScopeExtra))
			}
		}},
		{name: "补充池关闭", cause: workerStopCoolingRetire, prepare: func(_ *testing.T, env *extraEnv) { env.setExtra(false, 2, 3) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			victim := proPartitionA
			env := newExtraEnv(t, []string{partitionNormalA}, []string{victim}, 0, 1, 1, 1, 2, 3)
			env.member(t, aistudio.BenefitTierPro, victim)
			env.promote(t, victim)
			if tc.prepare != nil {
				tc.prepare(t, env)
			}
			evicted, err := env.manager.evictIdleWorkerFor(env.ctx, victim, tc.cause)
			if !evicted || err != nil {
				t.Fatalf("evict = %t %v", evicted, err)
			}
			if env.manager.extraOwnedHas(victim) || env.workerOf(victim) != nil {
				t.Fatal("不接手时应照常关闭")
			}
			if countFrom(env.requests, victim, "WAA Worker 停止", "起因="+tc.cause) != 1 || countLogs(env.requests, "补充池接手 Worker") != 0 {
				t.Fatalf("应照常写停止行、不写接手行：%q", logsFrom(env.requests, victim))
			}
		})
	}
}

// TestExtraHandOffLockBusy 接手时账户的 startupMu 被占用：只 TryLock、不等待，不接手、照常关闭（等 startupMu 释放后关闭）；
// 等待期间 rebalanceMu 是空闲的，其他账户照常启动，不死锁
func TestExtraHandOffLockBusy(t *testing.T) {
	env, victim, _ := handOffEnv(t, 1, 2)
	account := env.manager.extraAccount(victim)
	account.startupMu.Lock()
	done := make(chan error, 1)
	go func() {
		_, err := env.manager.evictIdleWorkerFor(env.ctx, victim, workerStopCoolingRetire)
		done <- err
	}()
	// 等它进入 ResetFor（独占租约已取得、接手已放弃）：期间 rebalanceMu 必须空闲
	time.Sleep(200 * time.Millisecond)
	if !env.manager.rebalanceMu.TryLock() {
		account.startupMu.Unlock()
		t.Fatal("接手不应在持有 rebalanceMu 时等待账户的 startupMu")
	}
	env.manager.rebalanceMu.Unlock()
	if env.manager.extraOwnedHas(victim) {
		account.startupMu.Unlock()
		t.Fatal("startupMu 被占用时不应接手")
	}
	if _, err := env.manager.promote(env.ctx, proPartitionB, testRuntimeModel); err != nil {
		account.startupMu.Unlock()
		t.Fatalf("等待期间其他账户应照常启动: %v", err)
	}
	account.startupMu.Unlock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("startupMu 释放后应照常关闭")
	}
	if env.manager.extraOwnedHas(victim) || env.workerOf(victim) != nil || countFrom(env.requests, victim, "起因=冷却淘汰") != 2 {
		t.Fatalf("应照常关闭、不接手：%q", logsFrom(env.requests, victim))
	}
}

// TestExtraReturnInPlace 补充池 Worker 的成员冷却到期、家分区有空位：原地归还（浏览器与版本不变），写一行补充池 Worker 归还
func TestExtraReturnInPlace(t *testing.T) {
	env := newExtraEnv(t, nil, []string{proPartitionA}, 0, 1, 1, 2, 1, 2)
	env.member(t, aistudio.BenefitTierPro, proPartitionA)
	env.pass(t)
	if !env.manager.extraOwnedHas(proPartitionA) {
		t.Fatal("冷成员应被补充池启动")
	}
	worker, generation := env.workerOf(proPartitionA), env.manager.WorkerGeneration(proPartitionA)
	env.recover(t, proPartitionA)
	env.pass(t)
	if env.manager.extraOwnedHas(proPartitionA) || env.workerOf(proPartitionA) != worker || env.manager.WorkerGeneration(proPartitionA) != generation {
		t.Fatal("冷却到期后应原地归还家分区，不重启浏览器")
	}
	if !slices.Equal(env.in(aistudio.PoolScopePro), []string{proPartitionA}) {
		t.Fatalf("归还后应计入家分区：%v", env.in(aistudio.PoolScopePro))
	}
	if countFrom(env.requests, proPartitionA, "补充池 Worker 归还 | 分区=Pro 分区 | 原因=冷却已结束 | Pro Worker=1/2") != 1 ||
		countLogs(env.requests, "WAA Worker 停止") != 0 {
		t.Fatalf("应写一行补充池 Worker 归还、不关闭：%q", serviceLogMessages(env.requests))
	}
}

// TestExtraReleaseWhenHomeFull 家分区已满时已不是成员的补充池 Worker 在空闲时以“补充池释放”关闭；忙碌时等它空闲，下一轮再试
func TestExtraReleaseWhenHomeFull(t *testing.T) {
	env := newExtraEnv(t, nil, []string{proPartitionA, proPartitionB}, 0, 1, 1, 1, 1, 2)
	env.member(t, aistudio.BenefitTierPro, proPartitionA)
	env.pass(t)
	env.promote(t, proPartitionB)
	env.recover(t, proPartitionA)
	release := holdExclusive(t, env.pool, proPartitionA)
	env.pass(t)
	if !env.manager.extraOwnedHas(proPartitionA) || env.workerOf(proPartitionA) == nil {
		release()
		t.Fatal("忙碌的补充池 Worker 不应被释放")
	}
	release()
	env.pass(t)
	if env.manager.extraOwnedHas(proPartitionA) || env.workerOf(proPartitionA) != nil {
		t.Fatal("家分区已满时应在空闲后释放")
	}
	if countFrom(env.requests, proPartitionA, "WAA Worker 停止", "起因=补充池释放") != 1 ||
		countFrom(env.requests, proPartitionA, "补充池 Worker 释放 | 原因=家分区已满 | 补充池 Worker=0/2") != 1 {
		t.Fatalf("释放日志不符：%q", logsFrom(env.requests, proPartitionA))
	}
	if !slices.Equal(env.in(aistudio.PoolScopePro), []string{proPartitionB}) {
		t.Fatal("家分区的 Worker 不应受影响")
	}
}

// TestExtraMidnightRecoveryBudget 10 个补充池 Worker 同时恢复（太平洋时间零点）、家分区已满：每轮对账最多释放 max(WSC,1) 个，分几轮收敛
func TestExtraMidnightRecoveryBudget(t *testing.T) {
	ids := extraIDsOf("midnight", 10)
	env := newExtraEnv(t, append(slices.Clone(ids), partitionNormalA), nil, 1, 1, 0, 1, 10, 10)
	env.member(t, aistudio.BenefitTierFree, ids...)
	env.promote(t, partitionNormalA)
	for round := 0; round < 30 && len(env.in(aistudio.PoolScopeExtra)) < 10; round++ {
		env.pass(t)
	}
	if got := len(env.in(aistudio.PoolScopeExtra)); got != 10 {
		t.Fatalf("补充池应补到 10 个，实际 %d", got)
	}
	env.recover(t, ids...)
	budget := max(env.manager.warmConcurrencyValue(), 1)
	for round := 1; round <= 10/budget; round++ {
		env.pass(t)
		if got := countLogs(env.requests, "WAA Worker 停止", "起因=补充池释放"); got != round*budget {
			t.Fatalf("第 %d 轮后释放 %d 个，期望每轮 %d 个", round, got, budget)
		}
	}
	if len(env.in(aistudio.PoolScopeExtra)) != 0 || env.manager.extraOwnedCount() != 0 {
		t.Fatal("几轮之后应全部释放")
	}
}

// TestExtraTagClearedOnClose 补充池 Worker 经任何路径关闭后标记都已清除（释放、空闲回收、不可调度释放、失败恢复、删除），启动失败时
// 不留标记；不占任何槽位却仍带标记的残留由对账兜底清除
func TestExtraTagClearedOnClose(t *testing.T) {
	cases := []struct {
		name  string
		close func(t *testing.T, env *extraEnv, accountID string)
	}{
		{name: "释放", close: func(t *testing.T, env *extraEnv, accountID string) {
			if evicted, err := env.manager.evictIdleWorkerFor(env.ctx, accountID, workerStopExtraRelease); !evicted || err != nil {
				t.Fatalf("释放 = %t %v", evicted, err)
			}
		}},
		{name: "空闲回收", close: func(t *testing.T, env *extraEnv, _ string) {
			env.setExtra(true, 0, 1)
			if !env.manager.reapIdleWorker(time.Now().Add(time.Hour)) {
				t.Fatal("应回收补充池的空闲 Worker")
			}
		}},
		{name: "不可调度释放", close: func(t *testing.T, env *extraEnv, accountID string) {
			if err := env.pool.MarkAuthRequired(accountID, "测试"); err != nil {
				t.Fatal(err)
			}
			env.service.registerUnschedulableWorkers()
			if !waitUntil(5*time.Second, func() bool { return env.workerOf(accountID) == nil }) {
				t.Fatal("不可调度的补充池 Worker 应被释放")
			}
		}},
		{name: "失败恢复", close: func(t *testing.T, env *extraEnv, accountID string) {
			if err := env.manager.ResetFor(accountID, workerStopRecovery); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "删除", close: func(t *testing.T, env *extraEnv, accountID string) {
			if err := env.manager.Remove(accountID); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newExtraEnv(t, []string{partitionNormalA}, nil, 0, 1, 0, 1, 1, 1)
			env.member(t, aistudio.BenefitTierFree, partitionNormalA)
			env.pass(t)
			if !env.manager.extraOwnedHas(partitionNormalA) {
				t.Fatal("冷成员应被补充池启动")
			}
			tc.close(t, env, partitionNormalA)
			if env.manager.extraOwnedHas(partitionNormalA) || env.manager.extraOwnedCount() != 0 {
				t.Fatalf("%s 之后标记应已清除", tc.name)
			}
			if !samePartitionSet(env.manager.slotMembers(), env.pool.PartitionMembers()) {
				t.Fatal("没有标记后叠加视图应回到账户池的快照")
			}
		})
	}
	t.Run("启动失败", func(t *testing.T) {
		env := newExtraEnv(t, []string{partitionNormalA}, nil, 0, 1, 0, 1, 1, 1)
		env.member(t, aistudio.BenefitTierFree, partitionNormalA)
		env.fail[partitionNormalA] = errors.New("测试启动失败")
		env.pass(t)
		if len(env.launches()) != 1 || env.manager.extraOwnedHas(partitionNormalA) {
			t.Fatalf("启动失败后不应留标记：冷启动 %v", env.launches())
		}
		if countLogs(env.requests, "补充池 WAA Worker 预热失败 | 错误=") != 1 {
			t.Fatal("应写一行补充池预热失败")
		}
		// 5 分钟内不再选它，也不重复写失败
		env.pass(t)
		if len(env.launches()) != 1 || countLogs(env.requests, "补充池 WAA Worker 预热失败") != 1 {
			t.Fatal("预热失败的账户 5 分钟内不应再启动")
		}
	})
	t.Run("残留标记", func(t *testing.T) {
		env := newExtraEnv(t, []string{partitionNormalA}, nil, 0, 1, 0, 1, 1, 1)
		tagExtra(env.manager, partitionNormalA, true)
		env.setExtra(false, 1, 1)
		env.pass(t)
		if env.manager.extraOwnedHas(partitionNormalA) {
			t.Fatal("不占任何槽位的残留标记应由对账清除")
		}
	})
}

// TestExtraWorkersInvisibleToMainLoops 补充池 Worker 对主分区的循环不可见：在热门、主用模型上长时间冷却也不被补登记淘汰、不被冷却登记、
// 不被冷却清扫与冷却轮换关闭，也不把它冷却的模型抬成热门；先登记、后被接手的登记由 retirePass 丢弃
func TestExtraWorkersInvisibleToMainLoops(t *testing.T) {
	ids := extraIDsOf("invisible", 3)
	env := newExtraEnv(t, append(slices.Clone(ids), partitionNormalA, partitionNormalB), nil, 1, 3, 0, 1, 3, 3)
	env.member(t, aistudio.BenefitTierFree, ids...)
	env.member(t, aistudio.BenefitTierFree, partitionNormalB)
	env.recover(t, partitionNormalB)
	env.manager.setPrimaryModels([]string{extraCoolModel}, nil)
	for round := 0; round < 5 && len(env.in(aistudio.PoolScopeExtra)) < 3; round++ {
		env.pass(t)
	}
	if got := env.in(aistudio.PoolScopeExtra); !slices.Equal(got, ids) {
		t.Fatalf("补充池 Worker = %v", got)
	}
	workers := map[string]*aistudio.NativeWorker{}
	for _, accountID := range ids {
		workers[accountID] = env.workerOf(accountID)
	}
	if added := env.service.registerDeadWorkers(); added != 0 || env.manager.retirementCount() != 0 {
		t.Fatalf("补登记淘汰不应碰补充池 Worker：新增 %d", added)
	}
	// retireCooledWorker 不登记补充池 Worker：持有 retireMu 时它也能立即返回（登记要取 retireMu）
	env.manager.retireMu.Lock()
	returned := make(chan struct{})
	go func() {
		env.service.retireCooledWorker(ids[0], extraCoolModel, aistudio.QuotaCooldown{Until: time.Now().Add(2 * time.Hour)})
		close(returned)
	}()
	select {
	case <-returned:
		env.manager.retireMu.Unlock()
	case <-time.After(time.Second):
		env.manager.retireMu.Unlock()
		<-returned
		t.Fatal("retireCooledWorker 不应为补充池 Worker 登记淘汰")
	}
	coolGlobally(t, env.pool, ids[1])
	if closed := env.service.sweepDeadWorkers(env.ctx); closed != 0 {
		t.Fatalf("冷却清扫不应关闭补充池 Worker：%d", closed)
	}
	env.service.rotateCooledWorkers(env.ctx)
	if hot := env.manager.hotModelList(); len(hot) != 0 {
		t.Fatalf("补充池 Worker 冷却的模型不应被算成热门：%v", hot)
	}
	for _, accountID := range ids {
		if env.workerOf(accountID) != workers[accountID] {
			t.Fatalf("%s 的补充池 Worker 不应被主分区的循环关闭", accountID)
		}
	}
	// 主分区的成员 Worker：先登记淘汰，随后被补充池接手（接手前的竞态），登记由 retirePass 丢弃
	env.setExtra(true, 4, 4)
	env.member(t, aistudio.BenefitTierFree, partitionNormalA)
	env.promote(t, partitionNormalA)
	if !env.manager.extraAdopt(partitionNormalA, workerStopCoolingRetire) {
		t.Fatal("补充池低于常驻数时应接手")
	}
	if _, ok := env.manager.registerRetirement(partitionNormalA, extraCoolModel); !ok {
		t.Fatal("应能写入一条登记（模拟接手前取到的快照）")
	}
	env.manager.retirePass()
	if env.manager.retirementCount() != 0 || env.workerOf(partitionNormalA) == nil || !env.manager.extraOwnedHas(partitionNormalA) {
		t.Fatal("被接手的 Worker 的淘汰登记应被丢弃，Worker 照常留在补充池")
	}
}

// TestMainRetireRotationUnchangedWithExtraOn 补充池开启、已到常驻数（常驻 0，不接手也不预热）时，情形 B 的主分区 Worker 的冷却轮换、
// 冷却淘汰与替补和关闭时相同
func TestMainRetireRotationUnchangedWithExtraOn(t *testing.T) {
	cooled := extraIDsOf("rotate-a", 3)
	spares := extraIDsOf("rotate-b", 3)
	run := func(t *testing.T, enabled bool) []string {
		env := newExtraEnv(t, append(slices.Clone(cooled), spares...), nil, 3, 3, 0, 1, 0, 1)
		env.setExtra(enabled, 0, 1)
		env.member(t, aistudio.BenefitTierFree, cooled...)
		env.member(t, aistudio.BenefitTierFree, spares...)
		env.recover(t, spares...)
		// 冷却的模型是主用模型：预热与替补先选还能服务它的备用号（否则近期成功过的冷却号又排在前面）
		env.manager.setPrimaryModels([]string{extraCoolModel}, nil)
		env.promote(t, cooled...)
		for _, accountID := range cooled {
			env.manager.extraAccount(accountID).readyAt.Store(time.Now().Add(-time.Hour).UnixNano())
		}
		settled := func(want int) {
			t.Helper()
			if !waitUntil(5*time.Second, func() bool {
				replaced := 0
				for _, accountID := range env.in(aistudio.PoolScopeNormal) {
					if slices.Contains(spares, accountID) {
						replaced++
					}
				}
				return replaced == want && len(env.in(aistudio.PoolScopeNormal)) == 3 && len(env.manager.OpeningAccountIDs()) == 0 && !env.manager.fillActive.Load()
			}) {
				t.Fatalf("开启=%t：应有 %d 个备用号接上：%v，日志 %q", enabled, want, env.in(aistudio.PoolScopeNormal), serviceLogMessages(env.requests))
			}
		}
		// 冷却轮换：3 个驻留号都在主用模型上长时间冷却（达到热门阈值），一轮最多换 WSC 个
		env.service.rotateCooledWorkers(env.ctx)
		settled(2)
		// 冷却淘汰：剩下的那个登记淘汰，由替补接上
		for _, accountID := range env.in(aistudio.PoolScopeNormal) {
			if slices.Contains(cooled, accountID) && !env.manager.retireWorker(accountID, extraCoolModel) {
				t.Fatal("应登记淘汰")
			}
		}
		settled(3)
		env.service.keepWarmPass(env.ctx, time.Now())
		env.manager.waitPrewarm()
		env.waitLaunches(t)
		if env.manager.extraOwnedCount() != 0 {
			t.Fatal("补充池已到常驻数时不应接手或预热")
		}
		// 只比较 Worker 的登记、轮换、淘汰、启停与替补（开启时另有补充池成员变化的汇总行）
		var lines []string
		for _, line := range normalizeLogs(serviceLogMessages(env.requests)) {
			if strings.HasPrefix(line, "WAA Worker") {
				lines = append(lines, line)
			}
		}
		slices.Sort(lines)
		return lines
	}
	off, on := run(t, false), run(t, true)
	if !slices.Equal(off, on) {
		t.Fatalf("补充池开启（不接手）时主分区的日志应与关闭时相同：\n关闭 %q\n开启 %q", off, on)
	}
	if !slices.ContainsFunc(off, func(line string) bool { return strings.HasPrefix(line, "WAA Worker 冷却轮换 | 替换=2") }) ||
		!slices.ContainsFunc(off, func(line string) bool { return strings.HasPrefix(line, "WAA Worker 冷却淘汰") }) {
		t.Fatalf("场景应走到冷却轮换与冷却淘汰：%q", off)
	}
}

// TestExtraUnschedulableReleased 补充池 Worker 的账户变为需要登录：经现有的不可调度释放关闭（释放行的槽位名为补充池 Worker），对账不按其他原因释放
func TestExtraUnschedulableReleased(t *testing.T) {
	env := newExtraEnv(t, []string{partitionNormalA}, nil, 0, 1, 0, 1, 1, 2)
	env.member(t, aistudio.BenefitTierFree, partitionNormalA)
	env.pass(t)
	if err := env.pool.MarkAuthRequired(partitionNormalA, "测试"); err != nil {
		t.Fatal(err)
	}
	env.service.keepWarmPass(env.ctx, time.Now())
	// 淘汰循环在后台关闭 Worker，“WAA Worker 释放”在关闭之后才写（带关闭后的补充池计数，见 finishRelease）：
	// 等到释放行出现再核对，只等 Worker 被移除时释放行可能还没写
	if !waitUntil(5*time.Second, func() bool {
		return countFrom(env.requests, partitionNormalA, "WAA Worker 释放 | 原因=需要登录") > 0
	}) {
		t.Fatalf("需要登录的补充池 Worker 应经不可调度释放关闭：%q", logsFrom(env.requests, partitionNormalA))
	}
	if env.workerOf(partitionNormalA) != nil {
		t.Fatal("需要登录的补充池 Worker 应被释放")
	}
	if countFrom(env.requests, partitionNormalA, "WAA Worker 停止", "起因=不可调度释放") != 1 ||
		countFrom(env.requests, partitionNormalA, "WAA Worker 释放 | 原因=需要登录", "补充池 Worker=0/2") != 1 {
		t.Fatalf("应经不可调度释放关闭：%q", logsFrom(env.requests, partitionNormalA))
	}
	if countLogs(env.requests, "补充池 Worker 释放") != 0 || env.manager.extraOwnedHas(partitionNormalA) {
		t.Fatal("对账不应按其他原因释放，关闭后标记已清除")
	}
}

// TestExtraHotToggle 补充池的热切换（经管理接口保存，立即生效、不重建生成服务）：打开后补充池启动冷成员；调小峰值时只写一次超峰值 WARN；
// 关闭后家分区有空位的原地归还、没有空位的释放，写出补充池设置行，主分区 Worker 不重启；再打开后重新补齐；“配置已热更新”的汇总行
// 与原模板逐字段相同（不带补充池字段）
func TestExtraHotToggle(t *testing.T) {
	env := newIncludeProRuntime(t)
	members := &extraEnv{pool: env.pool}
	members.member(t, aistudio.BenefitTierFree, partitionNormalA)
	members.member(t, aistudio.BenefitTierPro, includeProAccount)
	if _, err := env.workers.promote(env.ctx, partitionUltraA, testRuntimeModel); err != nil {
		t.Fatal(err)
	}
	ultraWorker := func() *aistudio.NativeWorker {
		account := env.workers.extraAccount(partitionUltraA)
		account.mu.Lock()
		defer account.mu.Unlock()
		return account.worker
	}
	before := ultraWorker()
	save := func(change func(*config.Config)) {
		t.Helper()
		change(&env.cfg)
		updated, err := env.manager.UpdateRuntimeConfig(env.ctx, runtimeConfigDTO(env.cfg))
		if err != nil {
			t.Fatal(err)
		}
		if updated.ServiceRestartRequired {
			t.Fatal("补充池设置应热更新，不要求重启生成服务")
		}
	}
	pass := func(rounds int) {
		t.Helper()
		for range rounds {
			env.service.extraPass(env.ctx)
			if !waitUntil(5*time.Second, func() bool {
				env.workers.extraMu.Lock()
				defer env.workers.extraMu.Unlock()
				return len(env.workers.extraStarting) == 0
			}) {
				t.Fatal("补充池预热没有结束")
			}
			env.workers.extraLaunches.Wait()
		}
	}
	extraWorkers := func() []string {
		warm := env.workers.warmAccountIDsIn(aistudio.PoolScopeExtra)
		slices.Sort(warm)
		return warm
	}
	save(func(cfg *config.Config) {
		cfg.ExtraPool, cfg.ExtraWarmWorkerLimit, cfg.ExtraMaxActiveWorkers = true, 2, 2
	})
	if countLogs(env.requests, "补充池设置已更新并立即生效 | 开启=是 | 方式=分担 | 默认地址借付费模型=是 | 含 Ultra=否 | 常驻 Worker=2 | 峰值 Worker=2") != 1 {
		t.Fatal("打开补充池应写一行补充池设置已更新")
	}
	pass(3)
	if got := extraWorkers(); len(got) != 2 {
		t.Fatalf("打开后补充池应启动两个冷成员：%v", got)
	}
	// 调小峰值：不强制关闭，只在越过峰值时写一次 WARN
	save(func(cfg *config.Config) { cfg.ExtraWarmWorkerLimit, cfg.ExtraMaxActiveWorkers = 1, 1 })
	pass(2)
	if got := countLogs(env.requests, "补充池 Worker 超过峰值 | 当前=2 | 峰值=1 | 空闲 Worker 将逐步回收，可调大 EXTRA_MAX_ACTIVE_WORKERS"); got != 1 {
		t.Fatalf("超峰值 WARN 写了 %d 次，期望只写一次", got)
	}
	if len(extraWorkers()) != 2 {
		t.Fatal("调小峰值不应强制关闭补充池 Worker")
	}
	// 关闭（同时把普通分区峰值调成 1）：一个原地归还普通分区，另一个家分区已满、空闲时释放
	save(func(cfg *config.Config) { cfg.ExtraPool, cfg.MaxActiveWorkers = false, 1 })
	if countLogs(env.requests, "补充池设置已更新并立即生效 | 开启=否") != 1 {
		t.Fatal("关闭补充池应写一行补充池设置已更新")
	}
	pass(2)
	if len(extraWorkers()) != 0 || env.workers.extraOwnedCount() != 0 {
		t.Fatalf("关闭后补充池 Worker 应全部归还或释放：%v", extraWorkers())
	}
	if countLogs(env.requests, "补充池 Worker 归还 | 分区=普通分区 | 原因=补充池已关闭") != 1 ||
		countLogs(env.requests, "补充池 Worker 释放 | 原因=补充池已关闭，家分区已满") != 1 {
		t.Fatalf("关闭后应一个归还、一个释放：%q", serviceLogMessages(env.requests))
	}
	if ultraWorker() != before {
		t.Fatal("切换补充池不应重启主分区的 Worker")
	}
	// 再打开：释放掉的冷成员重新补进补充池；已归还、驻留在普通分区的成员不再由补充池启动
	save(func(cfg *config.Config) {
		cfg.ExtraPool, cfg.ExtraWarmWorkerLimit, cfg.ExtraMaxActiveWorkers, cfg.MaxActiveWorkers = true, 2, 2, 2
	})
	pass(3)
	if got := extraWorkers(); len(got) != 1 {
		t.Fatalf("再打开后应重新补齐释放掉的冷成员：%v", got)
	}
	if ultraWorker() != before {
		t.Fatal("切换补充池不应重启主分区的 Worker")
	}
	// “配置已热更新”汇总行的字段与原模板相同，不带补充池字段
	wantKeys := []string{
		"常驻 Worker", "峰值 Worker", "Ultra 常驻 Worker", "Ultra 峰值 Worker", "Ultra 独占", "Pro 并入 Ultra", "Pro 独立号池", "Pro 常驻 Worker",
		"Pro 峰值 Worker", "权益规则", "预热并发", "单账户并发", "单账户每分钟上限", "策略", "上游通道", "请求超时", "首事件超时", "忽略客户端 seed",
		"降级判定", "流式优先 Playground", "主用模型", "Ultra/Pro 主用模型", "自动续写", "IP 池",
	}
	summaries := 0
	for _, message := range serviceLogMessages(env.requests) {
		if !strings.HasPrefix(message, "配置已热更新 | ") {
			continue
		}
		summaries++
		var keys []string
		for _, field := range strings.Split(strings.TrimPrefix(message, "配置已热更新 | "), " | ") {
			key, _, _ := strings.Cut(field, "=")
			keys = append(keys, key)
		}
		if !slices.Equal(keys, wantKeys) {
			t.Fatalf("配置已热更新的汇总行字段 = %q，期望与原模板相同", keys)
		}
	}
	if summaries == 0 {
		t.Fatal("每次热更新应写一行配置已热更新")
	}
}

// TestExtraCountsNeverDouble 补充池启动、主预热、接手、归还与释放、关闭、空闲回收交错执行 1000 次：任何时刻（在 rebalanceMu 内取样）
// 每个账户只计入一处、各处的驻留与启动中 Worker 之和等于驻留与启动中的账户数，且每处都不超过各自的峰值；结束后浏览器数等于有 Worker 的账户数
// （没有账户有两个浏览器），每个补充池标记都对应一个 Worker
func TestExtraCountsNeverDouble(t *testing.T) {
	normal := extraIDsOf("double-n", 6)
	pro := extraIDsOf("double-p", 4)
	// 家分区常驻等于峰值：归还时家分区常常已满，释放与归还都会走到
	env := newExtraEnv(t, normal, pro, 2, 2, 1, 1, 2, 3)
	all := append(slices.Clone(normal), pro...)
	env.member(t, aistudio.BenefitTierFree, normal...)
	env.member(t, aistudio.BenefitTierPro, pro...)
	env.recover(t, normal[4], normal[5], pro[3])
	partitions := []aistudio.PoolScope{aistudio.PoolScopeNormal, aistudio.PoolScopeUltra, aistudio.PoolScopePro, aistudio.PoolScopeExtra}
	check := func() error {
		manager := env.manager
		manager.rebalanceMu.Lock()
		slots := manager.slotMembers()
		held := make(map[string]struct{})
		for _, accountID := range manager.WarmAccountIDs() {
			held[accountID] = struct{}{}
		}
		var openingsAt []string
		for accountID := range manager.openings {
			held[accountID] = struct{}{}
			openingsAt = append(openingsAt, accountID)
		}
		// 先启后停的按需替换（主预热在分区已满时同样会替换空闲 Worker）：新 Worker 启动期间旧 Worker 还在，旧的已预留为淘汰对象，
		// 分区最多短暂多出预留数个
		var replacing partitionCounts
		manager.victimMu.Lock()
		for accountID := range manager.victims {
			replacing[partitionOf(accountID, slots)]++
		}
		manager.victimMu.Unlock()
		manager.rebalanceMu.Unlock()
		var counts partitionCounts
		for accountID := range held {
			counts[partitionOf(accountID, slots)]++
		}
		total := 0
		for _, partition := range partitions {
			total += counts[partition]
			if counts[partition] > env.manager.maxActiveFor(partition)+replacing[partition] {
				var detail []string
				for accountID := range held {
					if partitionOf(accountID, slots) == partition {
						detail = append(detail, accountID)
					}
				}
				return fmt.Errorf("%s 有 %d 个 Worker（%v，启动中 %v），超过峰值 %d 加进行中的替换 %d",
					partitionLabel(partition), counts[partition], detail, openingsAt, env.manager.maxActiveFor(partition), replacing[partition])
			}
		}
		if total != len(held) {
			return fmt.Errorf("各处之和 %d 不等于驻留与启动中的账户数 %d", total, len(held))
		}
		return nil
	}
	// interleave 交错执行 1000 次操作，同时取样核对；返回取样发现的问题
	interleave := func(round int) error {
		stop := make(chan struct{})
		checked := make(chan error, 1)
		go func() {
			samples := 0
			for {
				select {
				case <-stop:
					checked <- nil
					return
				default:
				}
				if err := check(); err != nil {
					checked <- fmt.Errorf("第 %d 轮第 %d 次取样：%w", round, samples, err)
					return
				}
				samples++
				time.Sleep(200 * time.Microsecond)
			}
		}()
		var operations sync.WaitGroup
		var done atomic.Int32
		for worker := range 4 {
			operations.Add(1)
			go func(seed int64) {
				defer operations.Done()
				random := rand.New(rand.NewSource(seed))
				for range 250 {
					accountID := all[random.Intn(len(all))]
					switch random.Intn(7) {
					case 0, 1:
						env.service.extraPass(env.ctx)
					case 2:
						env.manager.StartPrewarm(env.ctx)
					case 3:
						_, _ = env.manager.evictIdleWorkerFor(env.ctx, accountID, workerStopCoolingRetire)
					case 4:
						// 冷却结束（多半挑补充池 Worker 的账号，让归还与释放常常走到）或重新冷却
						if owned := env.manager.extraOwnedIDs(); len(owned) > 0 && random.Intn(3) > 0 {
							accountID = owned[random.Intn(len(owned))]
							_ = env.pool.ClearCooldownIfGeneration(accountID, extraCoolModel, env.pool.ModelAccessGeneration(accountID), time.Now())
						} else if random.Intn(2) == 0 {
							_ = env.pool.ClearCooldownIfGeneration(accountID, extraCoolModel, env.pool.ModelAccessGeneration(accountID), time.Now())
						} else {
							_ = env.pool.MarkCooldownIfGeneration(accountID, extraCoolModel, env.pool.ModelAccessGeneration(accountID), time.Now(),
								time.Now().Add(2*time.Hour), "每日限额")
						}
					case 5:
						_ = env.manager.ResetFor(accountID, workerStopRecovery)
					case 6:
						// 热调小补充池常驻数后空闲回收（补充池超出常驻数，空闲回收关闭补充池 Worker），之后多半调回 2
						env.setExtra(true, 1, 3)
						env.manager.reapIdleWorker(time.Now().Add(time.Hour))
						if random.Intn(4) > 0 {
							env.setExtra(true, 2, 3)
						}
					}
					done.Add(1)
				}
			}(int64(round*10 + worker + 1))
		}
		operations.Wait()
		close(stop)
		if err := <-checked; err != nil {
			return err
		}
		if done.Load() != 1000 {
			return fmt.Errorf("第 %d 轮只执行了 %d 次", round, done.Load())
		}
		return nil
	}
	// 交错里各条路径都要真的走到：调度不确定，没走全时再交错一轮（最多 5 轮，每轮 1000 次）
	fragments := []string{"分区=补充池", "补充池接手 Worker", "补充池 Worker 归还", "补充池 Worker 释放 |", "起因=空闲回收", "起因=失败恢复"}
	events := map[string]int{}
	for round := 1; ; round++ {
		if err := interleave(round); err != nil {
			t.Fatal(err)
		}
		missing := ""
		for _, fragment := range fragments {
			if events[fragment] = countLogs(env.requests, fragment); events[fragment] == 0 && missing == "" {
				missing = fragment
			}
		}
		if missing == "" {
			break
		}
		if round == 5 {
			t.Fatalf("交错执行 %d 轮仍没有走到 %q：%v", round, missing, events)
		}
	}
	env.manager.waitPrewarm()
	env.waitLaunches(t)
	env.service.extraPass(env.ctx)
	env.waitLaunches(t)
	env.manager.waitPrewarm()
	if err := check(); err != nil {
		t.Fatal(err)
	}
	withWorker := 0
	for _, accountID := range all {
		if env.workerOf(accountID) != nil {
			withWorker++
		}
	}
	if live := env.liveBrowsers(); live != withWorker {
		t.Fatalf("还开着 %d 个浏览器，有 Worker 的账户 %d 个：有账户开了两个浏览器或浏览器没有关", live, withWorker)
	}
	for _, accountID := range env.manager.extraOwnedIDs() {
		if env.workerOf(accountID) == nil {
			t.Fatalf("%s 带补充池标记却没有 Worker", accountID)
		}
	}
	t.Logf("补充池路径计数：%v", events)
}

// TestProxyCapacityExtra IP 池容量：补充池关闭时与原公式相同；开启时加补充池峰值与它的排空上限（默认 50+4）；按当前生效设置计算的同样
func TestProxyCapacityExtra(t *testing.T) {
	cfg := config.Default()
	base := browserCapacity(cfg.WarmStartupConcurrency, cfg.MaxActiveWorkers, cfg.UltraMaxActiveWorkers, cfg.ProMaxActiveWorkers)
	if got := proxyPoolCapacity(cfg, true); got != base {
		t.Fatalf("补充池关闭时容量 = %d，期望原公式 %d", got, base)
	}
	cfg.ExtraPool = true
	if got := proxyPoolCapacity(cfg, true) - base; got != 54 || got != cfg.ExtraMaxActiveWorkers+drainLimitFor(cfg.ExtraMaxActiveWorkers, cfg.WarmStartupConcurrency) {
		t.Fatalf("补充池开启时多出 %d 个，期望峰值加排空上限 50+4", got)
	}
	env := newExtraEnv(t, []string{partitionNormalA}, nil, 1, 2, 0, 1, 40, 50)
	env.setExtra(false, 40, 50)
	live := env.manager.liveProxyPoolCapacity(false)
	env.setExtra(true, 40, 50)
	if got := env.manager.liveProxyPoolCapacity(false) - live; got != 50+drainLimitFor(50, env.manager.warmConcurrencyValue()) {
		t.Fatalf("按生效设置开启补充池多出 %d 个", got)
	}
}

// TestAdminStatusExtra 状态接口：补充池关闭且没有补充池 Worker 时 JSON 里没有 extra_pool、extra_ids；开启后补充池的 Worker 计数、成员、
// 冷成员、可借出、归还中都正确，主分区的计数不含补充池 Worker
func TestAdminStatusExtra(t *testing.T) {
	env := newExtraEnv(t, []string{partitionNormalA, partitionNormalB}, []string{proPartitionA, proPartitionB}, 1, 3, 1, 3, 2, 3)
	admin := &runtimeAdmin{pool: env.pool, service: env.service, requests: env.requests, workers: env.manager}
	statusJSON := func() map[string]any {
		t.Helper()
		status, err := admin.Status(env.ctx)
		if err != nil {
			t.Fatal(err)
		}
		data, err := json.Marshal(status)
		if err != nil {
			t.Fatal(err)
		}
		var value map[string]any
		if err := json.Unmarshal(data, &value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	env.setExtra(false, 2, 3)
	value := statusJSON()
	if _, ok := value["extra_pool"]; ok {
		t.Fatal("关闭时不应输出 extra_pool")
	}
	if _, ok := value["workers"].(map[string]any)["extra_ids"]; ok {
		t.Fatal("没有补充池 Worker 时不应输出 extra_ids")
	}
	env.setExtra(true, 2, 3)
	env.member(t, aistudio.BenefitTierFree, partitionNormalA)
	env.member(t, aistudio.BenefitTierPro, proPartitionA, proPartitionB)
	env.promote(t, proPartitionB, partitionNormalB)
	env.pass(t)
	env.pass(t)
	// 补充池 Worker：normal-a 与 pro-a（冷成员补到常驻 2）；pro-b 是成员、驻留在 Pro 分区
	env.recover(t, partitionNormalA)
	env.pass(t)
	status, err := admin.Status(env.ctx)
	if err != nil {
		t.Fatal(err)
	}
	extra := status.ExtraPool
	if extra == nil || !extra.Enabled || extra.Mode != "share" || !extra.DefaultPaid || extra.IncludeUltra {
		t.Fatalf("补充池概况 = %+v", extra)
	}
	// 归还：normal-a 冷却已结束、普通分区有空位，原地归还；成员只剩两个 Pro
	if extra.Members != 2 || extra.Pro != 2 || extra.Free != 0 || extra.ColdMembers != 0 {
		t.Fatalf("成员计数 = %+v", extra)
	}
	if extra.Workers.Warm != 1 || extra.Workers.Max != 3 || extra.Workers.WarmLimit != 2 || extra.Lendable != 1 || extra.Returning != 0 {
		t.Fatalf("补充池 Worker 计数 = %+v（可借出 %d、归还中 %d）", extra.Workers, extra.Lendable, extra.Returning)
	}
	if status.ProWorkers.Warm != 1 || status.Workers.Warm != 2 || !slices.Equal(status.Workers.ExtraIDs, []string{proPartitionA}) {
		t.Fatalf("主分区计数应不含补充池 Worker：Pro %d、普通 %d、extra_ids %v", status.ProWorkers.Warm, status.Workers.Warm, status.Workers.ExtraIDs)
	}
	// 家分区已满时已不是成员的补充池 Worker 记为归还中（忙碌、等待释放）
	env.setExtra(true, 2, 3)
	env.manager.setProCapacity(1, 1)
	env.recover(t, proPartitionA)
	release := holdExclusive(t, env.pool, proPartitionA)
	defer release()
	env.pass(t)
	if status, err = admin.Status(env.ctx); err != nil {
		t.Fatal(err)
	}
	if status.ExtraPool.Returning != 1 || status.ExtraPool.Lendable != 0 || status.ExtraPool.Members != 1 {
		t.Fatalf("归还中的补充池 Worker 计数 = %+v", status.ExtraPool)
	}
}

// TestExtraMembershipLog 成员变化汇总：有进出时写一行，最多每 30 秒一行（间隔内的变化合并到下一行）；没有进出时不写；补充池关闭后从零比较
func TestExtraMembershipLog(t *testing.T) {
	env := newExtraEnv(t, []string{partitionNormalA}, nil, 1, 1, 0, 1, 2, 3)
	member := func(id string, tier aistudio.BenefitTier) aistudio.ExtraMember {
		return aistudio.ExtraMember{ID: id, Tier: tier}
	}
	start := time.Now()
	lines := func() []string {
		var result []string
		for _, message := range serviceLogMessages(env.requests) {
			if strings.HasPrefix(message, "补充池成员变化") {
				result = append(result, message)
			}
		}
		return result
	}
	env.manager.noteMembershipChange([]aistudio.ExtraMember{member("a", aistudio.BenefitTierFree), member("b", aistudio.BenefitTierPro)}, start)
	env.manager.noteMembershipChange([]aistudio.ExtraMember{member("a", aistudio.BenefitTierFree)}, start.Add(10*time.Second))
	env.manager.noteMembershipChange([]aistudio.ExtraMember{member("a", aistudio.BenefitTierFree), member("c", aistudio.BenefitTierUltra)},
		start.Add(31*time.Second))
	env.manager.noteMembershipChange([]aistudio.ExtraMember{member("a", aistudio.BenefitTierFree), member("c", aistudio.BenefitTierUltra)},
		start.Add(90*time.Second))
	want := []string{
		"补充池成员变化 | 加入=2 | 退出=0 | 成员=2（Free 1 · Pro 1 · Ultra 0） | 补充池 Worker=0/3",
		"补充池成员变化 | 加入=1 | 退出=1 | 成员=2（Free 1 · Pro 0 · Ultra 1） | 补充池 Worker=0/3",
	}
	if got := lines(); !slices.Equal(got, want) {
		t.Fatalf("成员变化日志 = %q，期望 %q", got, want)
	}
	env.manager.resetMembershipLog()
	env.manager.noteMembershipChange([]aistudio.ExtraMember{member("a", aistudio.BenefitTierFree)}, start.Add(100*time.Second))
	if got := lines(); len(got) != 3 || got[2] != "补充池成员变化 | 加入=1 | 退出=0 | 成员=1（Free 1 · Pro 0 · Ultra 0） | 补充池 Worker=0/3" {
		t.Fatalf("清空后应从零比较：%q", got)
	}
}
