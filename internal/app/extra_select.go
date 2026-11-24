package app

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/config"
)

// 补充池（EXTRA_POOL）的请求侧（批 3）：一条不阻塞的借用选号通道。
//
// generateForModel 在选号上下文上挂一份借用计划（extraPlan，整个请求一份），acquireLease 每次选号按它建一个 extraRun，在六个插入点上动作
// （没有计划、不符合借用条件或补充池关闭时 extraRun 为 nil 或这一轮不生效，所有方法都是空操作，与没有补充池时相同）：
//   - I1 主分类出错：只能由补充池服务的请求（默认地址、EXTRA_POOL_DEFAULT_PAID，模型不在本路由的列表里、在补充池的列表里）把主分组当空继续；
//   - I2 剔除：从主分组的热 Worker 里去掉可借出的补充池 Worker（L），它们只经借用通道取；已不是成员、等待归还的补充池 Worker 照常留在主分组；
//   - I3 一起分担（share）：按两边空闲数成比例轮取，轮到补充池时先借；
//   - I4 主号池热 Worker 段之后：share 本轮没试过就借；只在不够时补充（overflow）满足触发条件才借；
//   - I5 主号池既没有空闲热 Worker、也不能现场冷启动：两种方式都先借，借不到时最多发起一次补充池后台扩容，补充池有忙碌的可借 Worker
//     或扩容还没结束时按忙碌等待；
//   - I6 只能由补充池服务的请求收尾：补充池的候选全部冷却时按冷却等待或 429，否则按 503（模型在默认地址的列表里，不返回 404）。
// 借用的租约选号用 Pool=PoolScopeExtra、AllowedAccountIDs 为 L 的子集，权益规则照常按账户真实权益执行（Free 永远不在付费模型的候选里）。
// 借到的账户的家分区不在路由号池内时为跨号池借用：那次尝试的号池级 403、裸限流判定记在 PoolScopeExtra 键下，Worker 重建后按 Pool=Extra 重放

const (
	// extraOverflowQueueWait 为 overflow 方式下本次选号排队多久之后改借补充池：短于主用模型默认 10 秒的排队预算，长于一次正常的调度复查
	extraOverflowQueueWait = 3 * time.Second
)

// extraRoute 为请求的入口：补充池按入口决定能补充哪些模型
type extraRoute uint8

const (
	// extraRouteNone 为没有写入号池的请求（后台任务、测试）：只补充号池原本提供的模型
	extraRouteNone extraRoute = iota
	// extraRouteDefault 为默认地址（含 /trace 默认前缀）：EXTRA_POOL_DEFAULT_PAID 时还能借补充池服务本路由列表之外的模型
	extraRouteDefault
	// extraRoutePro、extraRouteUltra 为 /pro、/ultra：始终只补充自己的模型
	extraRoutePro
	extraRouteUltra
)

// extraRouteOf 返回请求的入口：没有写入号池为 none，Pro、Ultra 为对应入口，其余为默认地址
func extraRouteOf(ctx context.Context) extraRoute {
	scope, ok := aistudio.LookupPoolScope(ctx)
	switch {
	case !ok:
		return extraRouteNone
	case scope == aistudio.PoolScopePro:
		return extraRoutePro
	case scope == aistudio.PoolScopeUltra:
		return extraRouteUltra
	default:
		return extraRouteDefault
	}
}

// extraPlan 为一个生成请求（一次 generateForModel）的借用计划：请求本身的属性与请求内的借用记录。
// 借用与否按租约指针记（borrowedLease，不给 AccountLease 加字段），served 记账户最近一次被借用时是否跨号池（重放据此判断）。
// own、extra 惰性计算：只在补充池开启后第一次需要时查账户池，请求内缓存
type extraPlan struct {
	service  *trackedService
	route    extraRoute
	scope    aistudio.PoolScope
	modelID  string
	eligible bool

	mu            sync.Mutex
	borrowedLease *aistudio.AccountLease
	served        map[string]bool
	ownKnown, own bool
	extraKnown    bool
	extraModel    bool
	overflowNoted bool
}

type extraPlanKey struct{}

// newExtraPlan 返回请求的借用计划；eligible 为请求本身可以借用（不固定账户、不引用文件或视频、不需要 Drive、不在降级判定中）。
// 只读 ctx，不查账户池：补充池关闭时的开销只有这一次分配
func (service *trackedService) newExtraPlan(requestCtx context.Context, scope aistudio.PoolScope, modelID string, eligible bool) *extraPlan {
	if service == nil || service.workers == nil || service.pool == nil {
		return nil
	}
	return &extraPlan{service: service, route: extraRouteOf(requestCtx), scope: scope, modelID: modelID, eligible: eligible}
}

// withExtraPlan 把借用计划放进选号上下文；plan 为 nil 时原样返回
func withExtraPlan(ctx context.Context, plan *extraPlan) context.Context {
	if plan == nil {
		return ctx
	}
	return context.WithValue(ctx, extraPlanKey{}, plan)
}

// extraPlanFrom 返回选号上下文上的借用计划；没有时为 nil
func extraPlanFrom(ctx context.Context) *extraPlan {
	plan, _ := ctx.Value(extraPlanKey{}).(*extraPlan)
	return plan
}

// ownModelLocked 判断本路由的号池原本就提供这个模型（与 /v1/models 同口径）；调用方持有 plan.mu
func (plan *extraPlan) ownModelLocked() bool {
	if !plan.ownKnown {
		plan.own, plan.ownKnown = plan.service.pool.ModelServableIn(plan.scope, plan.modelID), true
	}
	return plan.own
}

// extraModelLocked 判断补充池的号池（按补充池的权益掩码）提供这个模型；调用方持有 plan.mu
func (plan *extraPlan) extraModelLocked() bool {
	if !plan.extraKnown {
		plan.extraModel, plan.extraKnown = plan.service.pool.ModelServableIn(aistudio.PoolScopeExtra, plan.modelID), true
	}
	return plan.extraModel
}

// modelAllowed 判断这次请求的模型能否借用补充池，only 为只能由补充池服务（默认地址、EXTRA_POOL_DEFAULT_PAID，模型不在本路由的列表里、
// 在补充池的列表里）。本路由原本提供的模型总能借；/pro、/ultra 始终只补充自己的模型（/pro 不借 Ultra 成员跑 ULTRA_ONLY 模型）；
// 其余不借，也不剔除，与补充池关闭时相同
func (plan *extraPlan) modelAllowed(settings extraPoolSettings) (allowed bool, only bool) {
	plan.mu.Lock()
	defer plan.mu.Unlock()
	if plan.ownModelLocked() {
		return true, false
	}
	if plan.route == extraRouteDefault && settings.defaultPaid && plan.extraModelLocked() {
		return true, true
	}
	return false, false
}

// recordBorrow 记下借用得到的租约与这个账户是否跨号池借用
func (plan *extraPlan) recordBorrow(lease *aistudio.AccountLease, cross bool) {
	plan.mu.Lock()
	defer plan.mu.Unlock()
	plan.borrowedLease = lease
	if plan.served == nil {
		plan.served = make(map[string]bool, 1)
	}
	plan.served[lease.Account().ID] = cross
}

// borrowed 判断这次尝试的租约借自补充池（按租约指针比较；计划为 nil 或从没借过时为假）
func (plan *extraPlan) borrowed(lease *aistudio.AccountLease) bool {
	if plan == nil || lease == nil {
		return false
	}
	plan.mu.Lock()
	defer plan.mu.Unlock()
	return plan.borrowedLease == lease
}

// noteAttempt 记下这次尝试的账户是否经借用、是否跨号池（返回后者）：没有经借用的尝试去掉账户的借用记录，之后在它上面的重放按原号池
func (plan *extraPlan) noteAttempt(accountID string, borrowed bool) bool {
	if plan == nil {
		return false
	}
	plan.mu.Lock()
	defer plan.mu.Unlock()
	if !borrowed {
		delete(plan.served, accountID)
		return false
	}
	return plan.served[accountID]
}

// extraReplay 为 Worker 重建后在原账户重放时的处理
type extraReplay int

const (
	// extraReplayHome 不是借来的账户（或最近一次尝试没有经借用）：照旧按原号池重放
	extraReplayHome extraReplay = iota
	// extraReplayExtra 跨号池借来的账户：选号用 Pool=PoolScopeExtra 固定到原账户，启动按留存口径计入补充池
	extraReplayExtra
	// extraReplaySwitch 跨号池借来的账户已不是补充池成员（补充池已关闭、权益已不在补充池范围、冷却已结束、全局冷却、没有能用的模型、
	// 整户不可调度或账户已删除）：不重放，换号
	extraReplaySwitch
	// extraReplayOwn 本路由借来的账户（家分区在路由号池内）：照旧按原号池重放，启动按留存口径计入补充池，已不是成员（或补充池已关闭）时
	// 按家分区启动（fallbackHome）；否则重开会落到家分区，家分区满时要排队等槽位或替换一个健康的家分区 Worker，补充池的槽位白白空出
	extraReplayOwn
)

// replayAs 返回在原账户重放时的处理（见 extraReplay）。跨号池借来的账户按留存口径（与重放启动的 ensureWorker 同一判断）仍是成员才重放；
// 原先只把补充池已关闭、权益不在范围与账户已删除判为换号，冷却已结束（或全局冷却、没有能用的模型、整户不可调度）时照旧按 Pool=Extra 重放，
// 重开注定以 errExtraMemberGone 失败，之后的选号以没有可用账号一类结束时请求以这个内部错误结束（对外 502），还盖掉了 Worker 失败的原错误
func (plan *extraPlan) replayAs(accountID string) extraReplay {
	if plan == nil {
		return extraReplayHome
	}
	plan.mu.Lock()
	cross, borrowed := plan.served[accountID]
	plan.mu.Unlock()
	if !borrowed {
		return extraReplayHome
	}
	if !cross {
		return extraReplayOwn
	}
	manager := plan.service.workers
	if !manager.extraSettingsValue().enabled {
		return extraReplaySwitch
	}
	if plan.service.pool.ExtraMemberStates([]string{accountID}, time.Now(), 0, manager.extraUsable())[accountID] != aistudio.ExtraStay {
		return extraReplaySwitch
	}
	return extraReplayExtra
}

// extraReplaySwitchMessage 为跨号池借来的账号已不是补充池成员、放弃在原账号重放时的进度与 trace 文案
const extraReplaySwitchMessage = "补充池已关闭或账号已不是补充池成员，不在原账号重放，换号"

// replayStart 返回重放选号的补充池启动选项：跨号池按留存口径计入补充池（已不是成员时 errExtraMemberGone）；本路由的另带 fallbackHome
func (replay extraReplay) replayStart() (extraStartOptions, bool) {
	switch replay {
	case extraReplayExtra:
		return extraStartOptions{stay: true}, true
	case extraReplayOwn:
		return extraStartOptions{stay: true, fallbackHome: true}, true
	}
	return extraStartOptions{}, false
}

// extraReplayAccount 返回固定账户的选号里启动要计入补充池的账户（没有时为空）：跨号池借来的账户按 Pool=Extra 重放，总按补充池判断容量
// （已不是成员时 ensureWorker 返回 errExtraMemberGone）；本路由借来的账户重放时 ctx 带 fallbackHome 的补充池启动选项，按留存口径仍是成员时
// 按补充池判断，否则按家分区（与 ensureWorker 的 extraStartAs 同一判断）。ctx 上没有补充池启动选项时为空（与没有补充池时相同）
func (manager *accountWorkerManager) extraReplayAccount(ctx context.Context, selection aistudio.AccountSelection) string {
	accountID := strings.TrimSpace(selection.AccountID)
	if accountID == "" {
		return ""
	}
	if selection.Pool == aistudio.PoolScopeExtra {
		return accountID
	}
	if options, ok := extraStartFrom(ctx); ok && options.fallbackHome && manager.extraStartAs(accountID, options) == extraAsExtra {
		return accountID
	}
	return ""
}

// adoptReplay 把重放取得的租约记为借用（那次尝试同样由补充池服务；跨号池的守卫键记在补充池下）；不计借出次数、不另写“由补充池服务”。
// 跨号池的重放总由补充池服务（已不是成员时启动返回 errExtraMemberGone）；本路由的只在账户此刻持有补充池 Worker 时记为借用，
// 已按家分区重开的由家分区服务，不记
func (plan *extraPlan) adoptReplay(lease *aistudio.AccountLease, replay extraReplay) {
	if plan == nil || lease == nil {
		return
	}
	switch replay {
	case extraReplayExtra:
		plan.recordBorrow(lease, true)
	case extraReplayOwn:
		if plan.service.workers.extraOwnedHas(lease.Account().ID) {
			plan.recordBorrow(lease, false)
		}
	}
}

// attemptCeiling 返回请求的换号上限：请求开始时补充池开启、请求可以借用、模型允许借用时，把可借出的补充池 Worker 计入
// （max(current, min(可调度账户 + L, generateSwitchCeiling))），每请求换号硬上限不变；否则原样返回
func (plan *extraPlan) attemptCeiling(current int, eligible int) int {
	if plan == nil || !plan.eligible {
		return current
	}
	manager := plan.service.workers
	settings := manager.extraSettingsValue()
	if !settings.enabled {
		return current
	}
	lendable := len(manager.extraLendableIDs())
	if lendable == 0 {
		return current
	}
	if allowed, _ := plan.modelAllowed(settings); !allowed {
		return current
	}
	return max(current, min(eligible+lendable, generateSwitchCeiling))
}

// extraLendableIDs 返回可借出的补充池 Worker（L）：带补充池标记的账户去掉最近一次对账判为已不是成员、等待归还或释放的（按 ID 排序）。
// 刚打上标记的 Worker 立即可借出（启动或接手前已按进入口径核实过）；已不是成员的最多在一个对账周期内仍被借出
func (manager *accountWorkerManager) extraLendableIDs() []string {
	owned := manager.extraOwnedIDs()
	if len(owned) == 0 {
		return nil
	}
	view := manager.extraView.Load()
	if view == nil || len(view.returning) == 0 {
		return owned
	}
	return slices.DeleteFunc(owned, func(accountID string) bool { return hasKey(view.returning, accountID) })
}

type extraOverflowWaitKey struct{}

// withExtraOverflowWait 改写 overflow 方式的排队门槛（只供测试验证边界）
func withExtraOverflowWait(ctx context.Context, wait time.Duration) context.Context {
	return context.WithValue(ctx, extraOverflowWaitKey{}, wait)
}

// extraOverflowWaitFrom 返回 overflow 方式的排队门槛：默认 extraOverflowQueueWait
func extraOverflowWaitFrom(ctx context.Context) time.Duration {
	if wait, ok := ctx.Value(extraOverflowWaitKey{}).(time.Duration); ok {
		return wait
	}
	return extraOverflowQueueWait
}

// extraRun 为一次选号（acquireLease）的借用状态；为 nil 时所有方法都是空操作。每轮复查开头 begin 读一次设置，
// 借用分类按需各做一次（热分类：只看 L；全量分类：只在要扩容或判断只能由补充池服务的请求的结尾时做）
type extraRun struct {
	service   *trackedService
	plan      *extraPlan
	selection aistudio.AccountSelection
	failed    map[string]struct{}
	startedAt time.Time
	queueWait time.Duration
	warmOnly  bool
	// expanding 为这次选号发起的补充池后台扩容，结束时关闭（每次选号最多一个在途）
	expanding <-chan struct{}

	// 以下为本轮复查的状态（begin 清空）
	settings extraPoolSettings
	active   bool
	only     bool
	warm     []string
	lendable []string
	lendSet  map[string]struct{}
	// mainStandby 为剔除之后主分组里没冷却的备用账户数（去掉正在启动、坏号与出口受限之前），overflow 判断“主号池没有能立即用的”用；
	// mainBusy 为主分组的忙碌热 Worker 去掉 L 之后的数（L 里忙碌的留在主分组 WarmBusy 里，见 strip），overflow 判断“主号池热 Worker 都在忙”
	// 与排队快照的热忙碌数用
	mainStandby int
	mainBusy    int
	tried       bool
	hotDone     bool
	hot         aistudio.AccountCandidateGroups
	fullDone    bool
	fullAny     bool
	fullCold    []string
	fullUntil   time.Time
}

// extraRunFor 返回一次选号的借用状态：选号上下文上有计划且请求可以借用、不是不等待的选号、不固定账户或资源、不带限定集合、
// 不是 Build 试探、号池就是请求的号池时返回非 nil，否则为 nil（与没有补充池时相同）
func (service *trackedService) extraRunFor(
	ctx context.Context, selection aistudio.AccountSelection, immediate bool, fixedAccount bool, failed map[string]struct{},
) *extraRun {
	plan := extraPlanFrom(ctx)
	if plan == nil || !plan.eligible || immediate || fixedAccount || selection.AllowedAccountIDs != nil || selection.BuildProbe ||
		selection.Pool != plan.scope {
		return nil
	}
	return &extraRun{
		service: service, plan: plan, selection: selection, failed: failed, startedAt: time.Now(),
		queueWait: extraOverflowWaitFrom(ctx), warmOnly: coldStartBlocked(ctx),
	}
}

// begin 开始一轮复查：读一次补充池设置，开关关闭或模型不允许借用时这一轮所有插入点都不生效；取这一轮的 L
func (run *extraRun) begin(warm []string) {
	if run == nil {
		return
	}
	run.active, run.only, run.tried = false, false, false
	run.hotDone, run.fullDone, run.fullAny = false, false, false
	run.hot, run.fullCold, run.fullUntil = aistudio.AccountCandidateGroups{}, nil, time.Time{}
	run.lendable, run.lendSet, run.mainStandby, run.mainBusy = nil, nil, 0, 0
	run.warm = warm
	settings := run.service.workers.extraSettingsValue()
	if !settings.enabled {
		return
	}
	allowed, only := run.plan.modelAllowed(settings)
	if !allowed {
		return
	}
	run.settings, run.active, run.only = settings, true, only
	run.lendable = run.service.workers.extraLendableIDs()
	run.lendSet = make(map[string]struct{}, len(run.lendable))
	for _, accountID := range run.lendable {
		run.lendSet[accountID] = struct{}{}
	}
}

// onlyFor 为 I1：主分类出错时，只能由补充池服务的请求把主分组当空继续：模型不在路由号池的目录里，或路由号池里一个账户都没有
// （例如只有 Pro 与 Ultra 账户、PRO_POOL=true 时的默认地址：/v1/models 照样合并了补充池的模型）。路由号池有账户、只是目录还没加载时
// 照原错误返回
func (run *extraRun) onlyFor(err error) bool {
	if run == nil || !run.active || !run.only {
		return false
	}
	if errors.Is(err, aistudio.ErrModelNotFound) {
		return true
	}
	var notReady *aistudio.AccountsNotReadyError
	return errors.As(err, &notReady) && !run.service.pool.HasAccountsIn(run.selection.Pool)
}

// onlyExtra 判断这一轮是只能由补充池服务的请求（I6）
func (run *extraRun) onlyExtra() bool {
	return run != nil && run.active && run.only
}

// strip 为 I2：从主分组的空闲热 Worker 里去掉 L（只剔除可借出的补充池 Worker，已不是成员、等待归还的照常服务本路由），空闲的 L 只经借用
// 通道取。忙碌的 L 留在 WarmBusy 里：它们此刻本来就借不到，留在主分组里，主路径的热候选数、账号全部忙碌的等待、额度 429 后只用已启动 Worker
// 的提前返回、主用模型快速失败（不为补充池等待）都与补充池关闭时相同（本路由的账户在关闭时就在主分组里）；跨号池的 L 本来就不在主分类里。
// 同时记下剔除之后主分组里没冷却的备用账户数与去掉 L 的忙碌热 Worker 数（overflow 的触发条件按不含 L 的主号池判断）
func (run *extraRun) strip(groups *aistudio.AccountCandidateGroups) {
	if run == nil || !run.active {
		return
	}
	run.mainBusy = len(groups.WarmBusy)
	if len(run.lendSet) > 0 {
		groups.WarmReady = excludeAccountIDs(groups.WarmReady, run.lendSet)
		groups.WarmAvailable = excludeAccountIDs(groups.WarmAvailable, run.lendSet)
		run.mainBusy = len(excludeAccountIDs(groups.WarmBusy, run.lendSet))
	}
	run.mainStandby = len(groups.StandbyReady)
}

// borrowSelection 返回借用选号的条件：主选择的副本改为 Pool=PoolScopeExtra，通道限制、排除列表、方法与能力、出口优先原样带过来
func (run *extraRun) borrowSelection() aistudio.AccountSelection {
	selection := run.selection
	selection.Pool = aistudio.PoolScopeExtra
	selection.AllowedAccountIDs = nil
	selection.AccountID, selection.ResourceID = "", ""
	selection.BuildProbe = false
	return selection
}

// hotGroups 返回这一轮的热分类（只在需要借用时、每轮最多一次）：在 L 里按借用条件分类，热分组再与 L 取交集、去掉本次启动失败的账户；
// L 为空时不分类，分类出错（补充池的目录里没有这个模型）时按不可借处理
func (run *extraRun) hotGroups(ctx context.Context) aistudio.AccountCandidateGroups {
	if run.hotDone {
		return run.hot
	}
	run.hotDone = true
	if !run.active || len(run.lendable) == 0 {
		return run.hot
	}
	selection := run.borrowSelection()
	selection.AllowedAccountIDs = run.lendable
	run.service.workers.extraClassified.Add(1)
	groups, err := run.service.pool.ClassifyCandidates(ctx, selection, run.warm)
	if err != nil {
		return run.hot
	}
	keep := func(accountIDs []string) []string {
		return slices.DeleteFunc(slices.Clone(accountIDs), func(accountID string) bool {
			return !hasKey(run.lendSet, accountID) || hasKey(run.failed, accountID)
		})
	}
	run.hot = aistudio.AccountCandidateGroups{
		WarmReady: keep(groups.WarmReady), WarmAvailable: keep(groups.WarmAvailable), WarmBusy: keep(groups.WarmBusy),
	}
	return run.hot
}

// fullGroups 做这一轮的全量分类（不限 L，每轮最多一次）：记下补充池的候选里是否有没冷却的、最早恢复时间，以及可以扩容的冷成员
// （没冷却的备用账户与成员取交集，去掉占槽位的、正在启动的、本次启动失败的、坏号与被其他进程占用的，按 PreferWarmPool 排好）
func (run *extraRun) fullGroups(ctx context.Context) {
	if run.fullDone {
		return
	}
	run.fullDone = true
	if !run.active {
		return
	}
	manager := run.service.workers
	manager.extraClassified.Add(1)
	groups, err := run.service.pool.ClassifyCandidates(ctx, run.borrowSelection(), run.warm)
	if err != nil {
		return
	}
	run.fullAny = len(groups.WarmReady)+len(groups.WarmAvailable)+len(groups.WarmBusy)+len(groups.StandbyReady)+len(groups.StandbyBusy) > 0
	run.fullUntil = groups.EarliestCooldown
	view := manager.extraView.Load()
	if view == nil || len(view.memberSet) == 0 || len(groups.StandbyReady) == 0 {
		return
	}
	held := make(map[string]struct{})
	for _, accountID := range manager.WarmAccountIDs() {
		held[accountID] = struct{}{}
	}
	cold := slices.DeleteFunc(slices.Clone(groups.StandbyReady), func(accountID string) bool {
		return !hasKey(view.memberSet, accountID) || hasKey(held, accountID) || hasKey(run.failed, accountID)
	})
	cold, _ = manager.withoutOpening(cold)
	cold = manager.withoutBadAccounts(cold)
	cold, _ = manager.runtimeAvailable(cold)
	if len(cold) > 1 {
		cold = run.service.pool.PreferWarmPool(cold)
	}
	run.fullCold = cold
}

// take 为借用动作：在热分类的空闲 Worker 里取租约（平均调用开启且两类都有时先取没有进行中请求的）。取到时记下借用、计借出次数，
// 写请求进度“由补充池服务”（overflow 第一次借用时另写一行原因，reason 为空时不写）
func (run *extraRun) take(ctx context.Context, reason string) *aistudio.AccountLease {
	run.tried = true
	hot := run.hotGroups(ctx)
	idle := append(slices.Clone(hot.WarmReady), hot.WarmAvailable...)
	if len(idle) == 0 {
		return nil
	}
	selection := run.borrowSelection()
	var lease *aistudio.AccountLease
	if run.service.workers.balancedOn() && len(hot.WarmReady) > 0 && len(hot.WarmAvailable) > 0 {
		candidate := selection
		candidate.AllowedAccountIDs = hot.WarmReady
		lease, _, _ = run.service.pool.TryAcquireFor(ctx, candidate)
	}
	if lease == nil {
		candidate := selection
		candidate.AllowedAccountIDs = idle
		lease, _, _ = run.service.pool.TryAcquireFor(ctx, candidate)
	}
	if lease == nil {
		return nil
	}
	accountID := lease.Account().ID
	home := run.service.pool.PartitionMembers().Of(accountID)
	run.plan.recordBorrow(lease, !run.plan.scope.Contains(home))
	run.service.workers.extraBorrows.Add(1)
	mode := "分担"
	overflow := run.settings.mode == config.ExtraPoolModeOverflow
	if overflow {
		mode = "补充"
	}
	requestID, label := leaseRequestID(ctx), lease.Account().Config.Label
	trace := aistudio.TraceFromContext(ctx)
	if overflow && reason != "" {
		run.plan.mu.Lock()
		first := !run.plan.overflowNoted
		run.plan.overflowNoted = true
		run.plan.mu.Unlock()
		if first {
			message := "主号池没有可用账号，改用补充池 | 原因=" + reason
			trace.Note(message)
			if requestID != "" {
				run.service.requests.logRequestProgress(requestID, "service", "INFO", message)
			}
		}
	}
	message := fmt.Sprintf("由补充池服务 | 方式=%s | 账号所在号池=%s", mode, home.Label())
	trace.Note(message)
	if requestID != "" {
		run.service.requests.logRequestProgress(requestID, label, "INFO", message)
	}
	return lease
}

// shareFirst 为 I3（只在一起分担方式下）：设主号池空闲热 Worker 数为 m、补充池空闲数为 e，e>0、不是优先填满、且 m==0 或轮到补充池
// （manager 级计数器 % (m+e) >= m）时先借，否则留给 I4。平均调用开启时两边都先比没有进行中请求的 Worker：只有一边有时先取那一边，
// 两边都有时按它们的个数轮取。整体上等价于在两边的空闲 Worker 的并集上轮询
func (run *extraRun) shareFirst(ctx context.Context, groups aistudio.AccountCandidateGroups) *aistudio.AccountLease {
	if run == nil || !run.active || run.settings.mode != config.ExtraPoolModeShare || len(run.lendable) == 0 {
		return nil
	}
	hot := run.hotGroups(ctx)
	main, extra := len(groups.WarmReady)+len(groups.WarmAvailable), len(hot.WarmReady)+len(hot.WarmAvailable)
	if extra == 0 {
		return nil
	}
	if run.service.workers.balancedOn() {
		mainIdle, extraIdle := len(groups.WarmReady), len(hot.WarmReady)
		switch {
		case mainIdle > 0 && extraIdle == 0:
			return nil
		case extraIdle > 0 && mainIdle == 0:
			return run.take(ctx, "")
		case mainIdle > 0:
			main, extra = mainIdle, extraIdle
		}
	}
	if run.service.pool.RoutingFillFirst() {
		return nil
	}
	if main > 0 && run.service.workers.extraTurn.Add(1)%uint64(main+extra) < uint64(main) {
		return nil
	}
	return run.take(ctx, "")
}

// overflowReason 为 overflow 方式的触发条件（O1）：额度 429 之后只能用已启动的 Worker、主号池热 Worker 都在忙、主号池没有能立即用的
// （全部冷却、没有候选、只剩忙碌的备用号），或本次选号已排队满门槛；返回写进进度的原因。主号池按不含 L 的主分组判断（忙碌的 L 虽留在
// WarmBusy 里，也不算主号池的热 Worker）
func (run *extraRun) overflowReason(ctx context.Context, warmAvailable int) (string, bool) {
	switch {
	case run.warmOnly:
		return "额度 429 后只能用已启动的 Worker", true
	case warmAvailable == 0 && run.mainBusy > 0:
		return "账号全部忙碌", true
	case warmAvailable+run.mainBusy == 0 && run.mainStandby == 0:
		return "全部冷却", true
	case time.Since(run.startedAt) >= run.queueWait:
		last := "-"
		if progress := leaseWaitProgressFromContext(ctx); progress != nil && progress.reason != "" {
			last = progress.reason
		}
		return fmt.Sprintf("排队 %s（最后原因=%s）", extraQueueWaitText(run.queueWait), last), true
	}
	return "", false
}

// extraQueueWaitText 为排队门槛的写法：整秒写“N 秒”，否则按 Duration
func extraQueueWaitText(wait time.Duration) string {
	if wait > 0 && wait%time.Second == 0 {
		return fmt.Sprintf("%d 秒", int(wait/time.Second))
	}
	return wait.String()
}

// afterMainWarm 为 I4（主号池热 Worker 段没取到租约之后）：share 本轮没试过就借；overflow 满足 O1 才借
func (run *extraRun) afterMainWarm(ctx context.Context, warmAvailable int) *aistudio.AccountLease {
	if run == nil || !run.active || run.tried || len(run.lendable) == 0 {
		return nil
	}
	if run.settings.mode == config.ExtraPoolModeOverflow {
		reason, ready := run.overflowReason(ctx, warmAvailable)
		if !ready {
			return nil
		}
		return run.take(ctx, reason)
	}
	return run.take(ctx, "")
}

// atMainExhausted 为 I5（主号池既没有空闲热 Worker、也不能现场冷启动）：本轮还没借过就先借（两种方式都借）；借不到、不是只用已启动 Worker 的
// 选号、各主分区首轮预热都已出结果时，最多发起一次补充池后台扩容（每次选号一个在途）。返回 pending 为补充池有忙碌的可借 Worker 或这次的
// 扩容还没结束（主用模型快速失败时恒为假：不为补充池等待），reason 为对应的等待原因
func (run *extraRun) atMainExhausted(ctx context.Context, warmAvailable int, failFast bool) (*aistudio.AccountLease, bool, string) {
	if run == nil || !run.active {
		return nil, false, ""
	}
	if !run.tried && len(run.lendable) > 0 {
		reason := ""
		if run.settings.mode == config.ExtraPoolModeOverflow {
			if reason, _ = run.overflowReason(ctx, warmAvailable); reason == "" {
				reason = "账号全部忙碌"
			}
		}
		if lease := run.take(ctx, reason); lease != nil {
			return lease, false, ""
		}
	}
	if run.expanding != nil {
		select {
		case <-run.expanding:
			run.expanding = nil
		default:
		}
	}
	if run.expanding == nil && !run.warmOnly {
		if accountID := run.expandable(ctx); accountID != "" {
			run.expanding = run.service.workers.extraExpandInBackground(accountID, run.selection.ModelID)
		}
	}
	if failFast {
		return nil, false, ""
	}
	switch {
	case run.expanding != nil:
		return nil, true, waitReasonExtraExpanding
	case len(run.hotGroups(ctx).WarmBusy) > 0:
		return nil, true, waitReasonExtraBusy
	}
	return nil, false, ""
}

// expandable 返回可以为这次请求在补充池后台扩容的冷成员：各主分区首轮预热都已出结果，补充池未满、或已满但有可替换的补充池空闲 Worker
func (run *extraRun) expandable(ctx context.Context) string {
	manager := run.service.workers
	if len(manager.firstWarmPendingIn(aistudio.PoolScopeAll)) > 0 {
		return ""
	}
	run.fullGroups(ctx)
	if len(run.fullCold) == 0 || !manager.extraExpandRoom(run.selection.ModelID) {
		return ""
	}
	return run.fullCold[0]
}

// extraExpandRoom 判断补充池还能为请求扩容：未到峰值，或已到峰值但有按需替换能淘汰的补充池空闲 Worker（本分区每分钟的替换未达上限）
func (manager *accountWorkerManager) extraExpandRoom(modelID string) bool {
	peak := manager.maxActiveFor(aistudio.PoolScopeExtra)
	if peak <= 0 {
		return false
	}
	members := manager.slotMembers()
	manager.rebalanceMu.Lock()
	occupancy, _ := manager.partitionOccupancyLocked(aistudio.PoolScopeExtra, members)
	manager.rebalanceMu.Unlock()
	if occupancy.effectiveSlots() < peak {
		return true
	}
	return manager.replacementAllowed(context.Background(), aistudio.PoolScopeExtra) &&
		manager.replaceVictimFor("", modelID, aistudio.PoolScopeExtra) != ""
}

// earliest 返回只能由补充池服务的请求的最早恢复时间：补充池的候选全部冷却（全量分类里没有任何没冷却的候选）时为其中最早的恢复，
// 否则为零（有没冷却的候选、只是此刻借不到：按 503 结束）
func (run *extraRun) earliest(ctx context.Context) time.Time {
	run.fullGroups(ctx)
	if run.fullAny {
		return time.Time{}
	}
	return run.fullUntil
}

// annotate 在这一轮做过热分类时给排队快照补上补充池的空闲与忙碌数（借用不生效时不写）；主号池的热忙碌数改为不含 L 的
// （忙碌的 L 留在主分组里，否则会在热忙碌与补充池忙碌里各算一次）
func (run *extraRun) annotate(snapshot *queueSnapshot) {
	if run == nil || !run.active || !run.hotDone {
		return
	}
	snapshot.warmBusy = run.mainBusy
	snapshot.extra = true
	snapshot.extraIdle = len(run.hot.WarmReady) + len(run.hot.WarmAvailable)
	snapshot.extraBusy = len(run.hot.WarmBusy)
}

// extraDefaultPaidFor 判断默认地址的请求可以借补充池服务本路由列表之外的模型（补充池开启、EXTRA_POOL_DEFAULT_PAID，请求已写入号池且
// 不是 /pro、/ultra）：/v1/models 合并补充池的模型、别名在本路由解析不到时再按补充池解析。读账户池里的补充池规则（与按号池规则版本
// 缓存的别名快照同一个来源），无锁
func (service *trackedService) extraDefaultPaidFor(ctx context.Context) bool {
	if service == nil || service.pool == nil || extraRouteOf(ctx) != extraRouteDefault {
		return false
	}
	live := service.pool.ExtraPoolLive()
	return live.Enabled && live.DefaultPaid
}

// extraOnlyModel 判断默认地址上的模型只能由补充池服务：本路由的号池不提供、补充池的号池提供（count_tokens 据此借用补充池）
func (service *trackedService) extraOnlyModel(ctx context.Context, model string) bool {
	if !service.extraDefaultPaidFor(ctx) {
		return false
	}
	return !service.pool.ModelServableIn(aistudio.PoolScopeFromContext(ctx), model) && service.pool.ModelServableIn(aistudio.PoolScopeExtra, model)
}

// canonicalModelFor 在请求的号池内解析模型别名；解析不到（原样返回）、而默认地址能借补充池服务付费模型时再按补充池的目录解析一次
func (service *trackedService) canonicalModelFor(ctx context.Context, model string) string {
	resolved := service.pool.CanonicalModelIDIn(aistudio.PoolScopeFromContext(ctx), model)
	if resolved == model && service.extraDefaultPaidFor(ctx) {
		return service.pool.CanonicalModelIDIn(aistudio.PoolScopeExtra, model)
	}
	return resolved
}

// mergeEligibleModels 合并两份模型列表（都按 catalog 的顺序、带各自的可用通道）：按 catalog 的顺序，同 ID 合并可用通道（先 own 的，再补 extra 独有的）
func mergeEligibleModels(catalog []aistudio.Model, own []aistudio.Model, extra []aistudio.Model) []aistudio.Model {
	if len(extra) == 0 {
		return own
	}
	ownByID := make(map[string]aistudio.Model, len(own))
	for _, model := range own {
		ownByID[model.ID] = model
	}
	extraByID := make(map[string]aistudio.Model, len(extra))
	for _, model := range extra {
		extraByID[model.ID] = model
	}
	merged := make([]aistudio.Model, 0, len(own)+len(extra))
	seen := make(map[string]struct{}, len(own)+len(extra))
	for _, entry := range catalog {
		if _, done := seen[entry.ID]; done {
			continue
		}
		model, inOwn := ownByID[entry.ID]
		other, inExtra := extraByID[entry.ID]
		switch {
		case inOwn && inExtra:
			channels := slices.Clone(model.Channels)
			for _, channel := range other.Channels {
				if !slices.Contains(channels, channel) {
					channels = append(channels, channel)
				}
			}
			model.Channels = channels
		case inExtra:
			model = other
		case !inOwn:
			continue
		}
		seen[entry.ID] = struct{}{}
		merged = append(merged, model)
	}
	return merged
}
