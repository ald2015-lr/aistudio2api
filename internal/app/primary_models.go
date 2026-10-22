package app

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// 主用模型快速失败（PRIMARY_MODELS / PRIMARY_QUEUE_WAIT 与 ULTRA_PRIMARY_MODELS / ULTRA_PRIMARY_QUEUE_WAIT）。
//
// 号池的同时生成数约为 峰值 Worker 数 × 单账户并发（MAX_ACTIVE_WORKERS × PER_ACCOUNT_CONCURRENCY，Ultra 分区另算），每个账号每分钟的
// 生成次数另受 ACCOUNT_RPM_LIMIT 约束（满了按忙碌换号，见 aistudio/rate_limit.go）。并发超过它时
// 请求在 acquireLease 里排队等空闲的账号或 Worker，原先一直等到首事件前期限（REQUEST_TIMEOUT，默认 5 分钟）：客户端看到的是几分钟的
// 首字延迟与随后的 504，排队的请求越积越多。列表中的模型的生成请求改为最多排队到排队上限，超过后按 CapacityBusyError
// 结束（对外 429 + Retry-After），由客户端稍后重试，不换号、不冷却、不记降级记忆、不退避。
//
// 主用模型按号池分两份列表，各有自己的排队上限（见 primaryPolicyFor）：普通列表（PRIMARY_MODELS）只作用于普通号池，Ultra 列表
// （ULTRA_PRIMARY_MODELS，默认 gemini-4-argon）只作用于 Ultra 号池（/ultra 请求）；ULTRA_EXCLUSIVE=false 时普通路径不限号池，
// 先按普通列表查，没有再按 Ultra 列表查（只有 Ultra 账号能服务的模型多半只在 Ultra 列表里）。分开的原因：同一个模型在两个号池里的
// 处境不同，普通号池的模型不该在 /ultra 上按普通规则排队，Ultra 号池稀缺的 Worker 也不该被普通号池的列表拿去判断死槽位。
//
// 普通列表的模型候选全部冷却时仍按原规则排队（一分钟内恢复的冷却原地等，等待不计入预算）或返回 429，额度 429 照常换号。
// Ultra 列表的模型另外在没有额度时立即返回 429（quotaFailFast），不在号池里挨个换号：原先额度 429 不计入连续失败上限，
// 换号上限是号池的可调度账号数（Ultra 约 20），一个请求会把 Ultra 号池挨个试一遍，换到备用账号还要现场冷启动（10–30s 以上），
// 最后对外还是不带 Retry-After 的上游 429。现在：
//   - 选号时候选全部冷却立即返回 AllCoolingError（Retry-After 为最早恢复），不原地等一分钟；
//   - 额度失败分两类计数，各有换号上限（见 primaryQuotaClassOf）：每日类（每日限额，以及剩余不少于 rotationMinRemaining 的长冷却）
//     最多再换 primaryQuotaSwitchLimit 次账号；限流类（分钟限额、没有额度信息的限流）按账号计、一个账号用满不代表其他账号也满，
//     最多再换 primaryRateLimitSwitchLimit 次。租约账号在发出前被同账号别的 429 连带拦下（ErrAccountCoolingDown，请求没有发到上游）
//     只在拦下它的冷却剩余不少于 rotationMinRemaining 时记一次每日类，短冷却不计数；
//   - 每次额度失败后紧接着的那次选号只用已启动的 Worker（withoutColdStart）：不现场冷启动备用账号，只有备用账号时为其中一个发起
//     后台扩容后立即返回；额度失败单独计数，不计入连续失败上限，也不退避；
//   - 结束时按 PrimaryQuotaError 返回（对外 429 + Retry-After，规则见 primaryQuotaRetryAt）；
//   - 流式请求在请求自己现场冷启动期间推迟写出响应头（最多推迟排队预算能抵扣的启动时长，且不超过开始等首个事件后 50 秒），
//     启动后才遇到的额度不足通常同样以 HTTP 429 返回（见 holdStreamStart）。
// 第一次尝试照常可以现场冷启动一个备用账号（还不知道它有没有额度）；额度以外的失败与普通主用模型相同，换到的 Worker 因额度以外的
// 原因失败后，下一次选号同样按普通规则（可能现场冷启动）。
//
// 预算按请求计：generateWithRetry 在第一次选号前创建一份，换号重试共用，不再每次选号各一份（原先 403 或额度 429 换号后
// 又是一份完整预算，加上失败尝试与退避，10s 的上限实际 24s 以上才返回 429）。截止时间按墙钟计 = 创建时间 + 排队上限：
// 选号的分类与复查、等待冷启动名额、失败的上游尝试（403、额度 429）、换号退避都计入；请求自己现场冷启动的浏览器启动本身
// 按实测时长延后截止时间（请求在等自己的 Worker，不是在排队；合计不超过 INIT_TIMEOUT），候选全部冷却的等待同样不计入。
// 截止时间已过后容量原因的等待立即返回 CapacityBusyError：失败后的下一次尝试只接受立即可用的账号，否则按 429 结束。
// 预算不用 context 超时，不会取消进行中的启动；不等待的选号（acquireWarmLeaseNow）本来就不排队。
// 这只是对症状的处理：根本上要让容量跟上并发（更多 Worker；单账号并发受每分钟次数上限约束（ACCOUNT_RPM_LIMIT），不宜调高），
// 或让冷却的账号及时让出 Worker。

const (
	// primaryRetryFallback 为排队上限为 0（不排队）时 Retry-After 的建议间隔，也是 Ultra 主用模型额度不足而号池里可能还有额度时的建议间隔
	primaryRetryFallback = 5 * time.Second
	// primaryRetryMin 与 primaryRetryMax 为 Retry-After 建议间隔的范围
	primaryRetryMin = time.Second
	primaryRetryMax = 30 * time.Second
	// primaryStreamStartGrace 为流式请求写出响应头前，在排队上限之外再多等首个事件的余量：截止时间到后还要复查一次候选、
	// 经过账户池锁才返回错误，留出余量让 429 以 HTTP 状态返回而不是 200 之后的流内错误
	primaryStreamStartGrace = 5 * time.Second
	// primaryQuotaSwitchLimit 为 Ultra 主用模型遇到每日类额度失败（每日限额，以及剩余不少于 rotationMinRemaining 的长冷却）后
	// 最多再换的账号数：遇到每日类额度失败的账号超过 2 个就结束（同账号另一通道的探测不另计，见 primaryQuotaTracker）。
	// 每日限额多半是号池里一批账号一起用完，挨个换下去只是把请求拖到超时；换两次还不行就让客户端稍后重试
	primaryQuotaSwitchLimit = 2
	// primaryRateLimitSwitchLimit 为限流类额度失败（分钟限额、没有额度信息的 30 秒限流）后最多再换的账号数，与每日类分开计：
	// 分钟窗口按账号计（每账号每分钟 12 次，所有模型、两个通道共用），一个账号用满不代表其他账号也满。
	// 取 5，与 Ultra 分区默认峰值 ULTRA_MAX_ACTIVE_WORKERS=5 对齐
	primaryRateLimitSwitchLimit = 5
	// waitReasonCooling 为候选全部冷却时的等待原因：这类等待不受排队上限限制，等待的时间延后截止时间，沿用原有的冷却排队规则
	waitReasonCooling = "账号冷却中"
	// waitReasonNoSlot 为有没冷却的候选、只是所在分区没有空闲 Worker 槽位时的等待原因：按容量原因计入排队上限
	waitReasonNoSlot = "没有空闲的 Worker 槽位"
)

// Ultra 主用模型额度不足时 PrimaryQuotaError 的原因
const (
	// primaryQuotaReasonLimit 为遇到每日类额度失败的账号数超过换号上限（primaryQuotaSwitchLimit）
	primaryQuotaReasonLimit = "额度失败已达换号上限"
	// primaryQuotaReasonRateLimit 为限流类额度失败（分钟限额、限流）的次数超过换号上限（primaryRateLimitSwitchLimit）
	primaryQuotaReasonRateLimit = "限流失败已达换号上限"
	// primaryQuotaReasonCooling 为额度 429 后换号时其余候选全部冷却
	primaryQuotaReasonCooling = "其余候选全部冷却"
	// primaryQuotaReasonNoWarm 为额度 429 后号池里没有已启动的 Worker（只剩要现场冷启动的备用账号），也是那次选号的 noImmediateLeaseError 原因
	primaryQuotaReasonNoWarm = "额度 429 后没有已启动的 Worker"
	// primaryQuotaReasonNoOther 为额度 429 后号池里没有其他可换的账号（都已尝试过、剩下的暂时不可调度，或换号次数已用完）
	primaryQuotaReasonNoOther = "没有其他可换的账号"
)

// primaryModelList 为一份生效的主用模型列表（按降级判定的规则整理：去空白、去 models/ 前缀、小写）与它的排队上限
type primaryModelList struct {
	models    map[string]struct{}
	queueWait time.Duration
}

// newPrimaryModelList 整理一份主用模型列表；排队上限为负时按 0
func newPrimaryModelList(models []string, queueWait time.Duration) primaryModelList {
	list := primaryModelList{models: make(map[string]struct{}, len(models)), queueWait: max(queueWait, 0)}
	for _, model := range models {
		if name := normalizeGuardModel(model); name != "" {
			list.models[name] = struct{}{}
		}
	}
	return list
}

// contains 判断已整理的模型名是否在列表里
func (list primaryModelList) contains(name string) bool {
	_, listed := list.models[name]
	return listed
}

// sorted 返回排序后的模型名，交给 Worker 管理器判断死槽位
func (list primaryModelList) sorted() []string {
	return slices.Sorted(maps.Keys(list.models))
}

// primaryModelSettings 为生效的两份主用模型列表：normal 只作用于普通号池（PRIMARY_MODELS），ultra 只作用于 Ultra 号池
// （ULTRA_PRIMARY_MODELS）；整份替换（原子指针），读取方看到的两份列表总是同一次设置的
type primaryModelSettings struct {
	normal primaryModelList
	ultra  primaryModelList
}

// primaryPolicy 为一个请求（号池、模型）适用的主用模型规则：queueWait 为排队上限，quotaFailFast 为没有额度时立即返回 429
// （Ultra 列表的模型，见本文件开头）
type primaryPolicy struct {
	queueWait     time.Duration
	quotaFailFast bool
}

// setPrimaryModelSettings 应用两份主用模型列表与排队上限（启动时与服务配置页保存后，立即生效）；主用模型同步给 Worker 管理器，
// 按分区判断死槽位、挑替补与放置其他模型的请求（见 worker_drain.go）
func (service *trackedService) setPrimaryModelSettings(normal []string, normalWait time.Duration, ultra []string, ultraWait time.Duration) {
	service.storePrimaryModels(&primaryModelSettings{
		normal: newPrimaryModelList(normal, normalWait), ultra: newPrimaryModelList(ultra, ultraWait),
	})
}

// setPrimaryModels 只替换普通号池的主用模型列表与排队上限，Ultra 列表保持不变（测试与只改普通列表的调用方使用）
func (service *trackedService) setPrimaryModels(models []string, queueWait time.Duration) {
	normal := newPrimaryModelList(models, queueWait)
	for {
		current := service.primaryModels.Load()
		next := &primaryModelSettings{normal: normal}
		if current != nil {
			next.ultra = current.ultra
		}
		if service.primaryModels.CompareAndSwap(current, next) {
			service.syncWorkerPrimaryModels(next)
			return
		}
	}
}

// storePrimaryModels 整份替换主用模型设置并同步给 Worker 管理器
func (service *trackedService) storePrimaryModels(settings *primaryModelSettings) {
	service.primaryModels.Store(settings)
	service.syncWorkerPrimaryModels(settings)
}

func (service *trackedService) syncWorkerPrimaryModels(settings *primaryModelSettings) {
	if service.workers != nil {
		service.workers.setPrimaryModels(settings.normal.sorted(), settings.ultra.sorted())
	}
}

// primaryPolicyFor 返回请求（号池、模型；别名已解析为规范 ID）适用的主用模型规则以及模型是否在对应号池的列表里：
// Ultra 号池只查 Ultra 列表（额度快速失败），普通号池只查普通列表；不限号池（ULTRA_EXCLUSIVE=false 的普通路径）先查普通列表，
// 命中按普通规则，没有再查 Ultra 列表，命中按 Ultra 规则
func (service *trackedService) primaryPolicyFor(pool aistudio.PoolScope, model string) (primaryPolicy, bool) {
	settings := service.primaryModels.Load()
	if settings == nil {
		return primaryPolicy{}, false
	}
	name := normalizeGuardModel(model)
	normal := primaryPolicy{queueWait: settings.normal.queueWait}
	ultra := primaryPolicy{queueWait: settings.ultra.queueWait, quotaFailFast: true}
	switch pool {
	case aistudio.PoolScopeUltra:
		return ultra, settings.ultra.contains(name)
	case aistudio.PoolScopeNormal:
		return normal, settings.normal.contains(name)
	default:
		if settings.normal.contains(name) {
			return normal, true
		}
		return ultra, settings.ultra.contains(name)
	}
}

// StreamStartWait 返回流式请求写出响应头前等待首个事件的下限：主用模型为所在号池列表的排队上限加余量。预算按请求计，失败的尝试
// 与退避都计入截止时间，排队超限与 Ultra 主用模型额度不足的 429 在这之前以 HTTP 状态返回，不是 200 之后的流内错误。请求自己现场
// 冷启动延后截止时间时，Ultra 主用模型另外通过 aistudio.StreamStartHold 推迟写出响应头（见 holdStreamStart）；普通号池的主用模型
// 在现场冷启动或候选冷却而延后截止时间时仍可能是流内错误。其他模型返回 0，由 API 层按默认心跳间隔处理。号池取请求 context 里的号池（/ultra 与
// 普通路径的标记，见 api.poolScopeMiddleware）；模型名是 API 层解析好的规范 ID，这里不再解析别名（那要在热路径上拿账户池锁）
func (service *trackedService) StreamStartWait(ctx context.Context, model string) time.Duration {
	policy, listed := service.primaryPolicyFor(aistudio.PoolScopeFromContext(ctx), model)
	if !listed {
		return 0
	}
	return policy.queueWait + primaryStreamStartGrace
}

// primaryBudget 为主用模型的请求创建排队预算（整个请求共用一份，换号重试不重新计时）；不在号池对应的列表里时返回 nil
func (service *trackedService) primaryBudget(modelID string, pool aistudio.PoolScope) *leaseWaitBudget {
	policy, listed := service.primaryPolicyFor(pool, modelID)
	if !listed {
		return nil
	}
	return &leaseWaitBudget{
		limit: policy.queueWait, startedAt: time.Now(), modelID: modelID, pool: pool, quotaFailFast: policy.quotaFailFast,
	}
}

// primaryQueueExceededMessage 为排队超限时写进请求进度的一句话，末尾追加超限时的候选快照（busy.Detail，见 waitQueued）
func primaryQueueExceededMessage(busy *aistudio.CapacityBusyError) string {
	if busy.Limit <= 0 {
		return fmt.Sprintf("主用模型不排队，立即返回 429 | 原因=%s | 总耗时=%s | 尝试=%d%s",
			busy.Reason, busy.Elapsed.Round(time.Millisecond), busy.Attempt, busy.Detail)
	}
	return fmt.Sprintf("主用模型排队超过 %s，立即返回 429 | 原因=%s | 已排队=%s | 总耗时=%s | 尝试=%d%s",
		busy.Limit, busy.Reason, busy.Waited.Round(time.Millisecond), busy.Elapsed.Round(time.Millisecond), busy.Attempt, busy.Detail)
}

// leaseWaitBudget 为一个主用模型请求的排队预算：截止时间 = 创建时间 + 排队上限 + 现场冷启动抵扣 + 冷却等待，按墙钟计。
// 截止时间已过后对账号忙碌、没有空闲槽位、Worker 启动或扩容中这些容量原因返回 CapacityBusyError；冷却原因不受限，
// 等待的时间也不计入。只在请求 goroutine 内使用（generateWithRetry 与它同步调用的选号、现场冷启动），不需要加锁
type leaseWaitBudget struct {
	limit     time.Duration
	startedAt time.Time
	// coldStart 为请求自己现场冷启动的浏览器启动时长（合计不超过 INIT_TIMEOUT），cooling 为候选全部冷却时排队的时间：都延后截止时间
	coldStart time.Duration
	cooling   time.Duration
	// attempts 为已开始的尝试次数（每次选号加一），写进排队超限的错误
	attempts int
	modelID  string
	pool     aistudio.PoolScope
	// quotaFailFast 为没有额度时立即返回 429（Ultra 主用模型，见本文件开头）；创建后不再改变
	quotaFailFast bool
	// streamHold 为流式请求写出响应头前的等待延长记录（只有 quotaFailFast 的流式请求有，见 holdStreamStart），
	// 请求自己现场冷启动时推迟写出响应头；streamHeld 为进行中的现场冷启动是否推迟了写出响应头（beginColdStart 设置，creditColdStart 清除）
	streamHold *aistudio.StreamStartHold
	streamHeld bool
}

type leaseWaitBudgetKey struct{}

// withLeaseWaitBudget 把排队预算放进选号上下文：acquireLease 的签名不变，不等待的选号与其他调用方不受影响；budget 为 nil 时原样返回
func withLeaseWaitBudget(ctx context.Context, budget *leaseWaitBudget) context.Context {
	if budget == nil {
		return ctx
	}
	return context.WithValue(ctx, leaseWaitBudgetKey{}, budget)
}

// leaseWaitBudgetFromContext 返回选号上下文里的排队预算；没有时为 nil，wait 退化为不限时的等待
func leaseWaitBudgetFromContext(ctx context.Context) *leaseWaitBudget {
	budget, _ := ctx.Value(leaseWaitBudgetKey{}).(*leaseWaitBudget)
	return budget
}

// deadline 返回排队截止时间
func (budget *leaseWaitBudget) deadline() time.Time {
	return budget.startedAt.Add(budget.limit + budget.coldStart + budget.cooling)
}

// failFastOnQuota 判断请求是否在没有额度时立即返回 429（Ultra 主用模型）；没有预算时为 false
func (budget *leaseWaitBudget) failFastOnQuota() bool {
	return budget != nil && budget.quotaFailFast
}

// remaining 返回距排队截止时间还有多久（已过时不大于 0）
func (budget *leaseWaitBudget) remaining() time.Duration {
	return time.Until(budget.deadline())
}

// expired 判断排队截止时间是否已过；没有预算时为 false
func (budget *leaseWaitBudget) expired() bool {
	return budget != nil && budget.remaining() <= 0
}

// beginAttempt 记一次尝试（generateWithRetry 每次选号前调用）；没有预算时不做任何事
func (budget *leaseWaitBudget) beginAttempt() {
	if budget != nil {
		budget.attempts++
	}
}

// holdStreamStart 让 Ultra 主用模型的流式请求在自己现场冷启动时推迟写出响应头（hold 来自请求 context，非流式请求为 nil）：
// API 层写出响应头前只等 StreamStartWait（排队上限加余量），第一次尝试现场冷启动备用账号要 10–30 秒以上，启动结束后才回的额度 429
// 原先成了 200 之后的流内错误，客户端与中转拿不到 429 和 Retry-After。启动进行中写出响应头前的等待先延后这次启动还能抵扣的时长，
// 启动结束后改为按抵扣的时长延后，与排队截止时间同步：截止时间之前的 429 都以 HTTP 状态返回。推迟的总时长不超过抵扣的上限
// （INIT_TIMEOUT）：抵扣用完后（例如第一次启动挂到 INIT_TIMEOUT 失败、随后又现场冷启动另一个备用账号）不再推迟，抵扣不了的启动时间
// 不会让 429 以 HTTP 状态返回，多等只会让客户端更久收不到任何字节。API 层另把推迟限制在开始等首个事件后 50 秒内（见
// api.streamStartHoldMax），免得前面的反向代理按读超时断开。普通号池的主用模型不推迟（行为不变），那里冷启动后的排队超限仍可能是流内错误
func (budget *leaseWaitBudget) holdStreamStart(hold *aistudio.StreamStartHold) {
	if budget.failFastOnQuota() {
		budget.streamHold = hold
	}
}

// beginColdStart 标记请求自己的现场冷启动（浏览器启动本身）开始，之后必须调用 creditColdStart；limit 为抵扣的上限（INIT_TIMEOUT）。
// 写出响应头前的等待最多再推迟这次启动还能抵扣的时长，抵扣已用完时不推迟。没有预算时不做任何事
func (budget *leaseWaitBudget) beginColdStart(limit time.Duration) {
	if budget == nil || budget.streamHold == nil {
		return
	}
	if credit := limit - budget.coldStart; credit > 0 {
		budget.streamHold.BeginColdStart(credit)
		budget.streamHeld = true
	}
}

// creditColdStart 把请求自己现场冷启动的浏览器启动时长从排队预算中抵扣（延后截止时间），合计不超过 limit（INIT_TIMEOUT）。
// 等待冷启动名额照常计入；同时结束 beginColdStart 的标记，写出响应头前的等待延后同样的时长。没有预算时不做任何事
func (budget *leaseWaitBudget) creditColdStart(elapsed time.Duration, limit time.Duration) {
	if budget == nil {
		return
	}
	credited := budget.coldStart
	if elapsed > 0 {
		budget.coldStart = max(budget.coldStart, min(budget.coldStart+elapsed, limit))
	}
	if budget.streamHeld {
		budget.streamHeld = false
		budget.streamHold.EndColdStart(budget.coldStart - credited)
	}
}

// wait 在截止时间前等待调度信号、兜底复查或请求取消：没有预算时与 waitScheduling 相同；等待原因是冷却时同样不限时，
// 等待的时间延后截止时间；截止时间已过时立即返回 CapacityBusyError；兜底复查计时器到期时刚好过了截止时间同样返回；
// 被调度信号唤醒时返回 nil，让调用方再复查一次候选
func (budget *leaseWaitBudget) wait(ctx context.Context, changed <-chan struct{}, delay time.Duration, reason string) error {
	if budget == nil {
		return waitScheduling(ctx, changed, delay)
	}
	if reason == waitReasonCooling {
		startedAt := time.Now()
		err := waitScheduling(ctx, changed, delay)
		budget.cooling += time.Since(startedAt)
		return err
	}
	remaining := budget.remaining()
	if remaining <= 0 {
		return budget.exhausted(reason)
	}
	timer := time.NewTimer(min(delay, remaining))
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-changed:
		return nil
	case <-timer.C:
		if budget.expired() {
			return budget.exhausted(reason)
		}
		return nil
	}
}

// exhausted 构造排队超限错误：已排队为总耗时减去现场冷启动与冷却等待，建议重试间隔为排队上限（为 0 时 5s），限定在 1s 到 30s
func (budget *leaseWaitBudget) exhausted(reason string) error {
	retry := budget.limit
	if retry <= 0 {
		retry = primaryRetryFallback
	}
	retry = min(max(retry, primaryRetryMin), primaryRetryMax)
	now := time.Now()
	elapsed := now.Sub(budget.startedAt)
	return &aistudio.CapacityBusyError{
		ModelID: budget.modelID, Pool: budget.pool, Reason: reason,
		Waited: max(elapsed-budget.coldStart-budget.cooling, 0), Limit: budget.limit,
		Elapsed: elapsed, ColdStart: budget.coldStart, Attempt: budget.attempts, Until: now.Add(retry),
	}
}

// primaryModelsSummary 为热更新日志里的一句话概况：普通列表与 Ultra 列表各一段
func primaryModelsSummary(models []string, queueWait time.Duration, ultraModels []string, ultraQueueWait time.Duration) string {
	return primaryListSummary(models, queueWait) + " | Ultra 主用模型=" + primaryListSummary(ultraModels, ultraQueueWait)
}

func primaryListSummary(models []string, queueWait time.Duration) string {
	if len(models) == 0 {
		return "关闭"
	}
	return strings.Join(models, ",") + "（排队上限 " + queueWait.String() + "）"
}

type coldStartBlockedKey struct{}

// withoutColdStart 标记 Ultra 主用模型每次额度失败后紧接着的那次选号只用已启动的 Worker（见 acquireLease）：不现场冷启动、不回收或替换
// 空闲 Worker、不排队等后台扩容；热 Worker 都忙时照常排队（受排队预算限制），只剩备用账号时为其中一个发起后台扩容后立即返回
// *noImmediateLeaseError。只加在那一次选号的 context 上（generateWithRetry 的 selectCtx），不进整个请求共用的选号 context。
// 没有复用不等待的选号（acquireWarmLeaseNow）：它在分区有空槽时会现场冷启动，热 Worker 都忙时也不等，额度以外的容量原因
// （热 Worker 忙）会被当成没有额度提前返回
func withoutColdStart(ctx context.Context) context.Context {
	return context.WithValue(ctx, coldStartBlockedKey{}, true)
}

// coldStartBlocked 判断选号是否只用已启动的 Worker
func coldStartBlocked(ctx context.Context) bool {
	blocked, _ := ctx.Value(coldStartBlockedKey{}).(bool)
	return blocked
}

// primaryQuotaClass 为 Ultra 主用模型额度失败的类别，各有自己的换号上限
type primaryQuotaClass uint8

const (
	// primaryQuotaDaily 为每日类：换号上限 primaryQuotaSwitchLimit
	primaryQuotaDaily primaryQuotaClass = iota + 1
	// primaryQuotaRate 为限流类（分钟限额、限流）：换号上限 primaryRateLimitSwitchLimit
	primaryQuotaRate
)

// primaryQuotaClassOf 返回一次额度 429 的类别：每日限额，或剩余不少于 rotationMinRemaining 的冷却（RetryInfo 很长的分钟限额、限流）
// 按每日类，其余按限流类
func primaryQuotaClassOf(cooldown aistudio.QuotaCooldown, now time.Time) primaryQuotaClass {
	if cooldown.Kind == aistudio.DailyQuotaKind || cooldown.Until.Sub(now) >= rotationMinRemaining {
		return primaryQuotaDaily
	}
	return primaryQuotaRate
}

// primaryQuotaTracker 记录一个 Ultra 主用模型请求遇到的额度失败：daily 为遇到每日类的账号数、rate 为限流类的次数（上游额度 429，
// 以及租约账号在发出前被长冷却拦下，见 coolingDownLong）；until 为全部额度失败中最早的冷却恢复时间，rateUntil 为限流类中最早的（不知道时为零值）；
// last 为最近一次的类别，lastErr 为最后一次额度错误。每日类按账号计（dailyAccounts 为已记过每日类的账号）：每日限额的通道共用额度
// 还在观察中（或定期复核）时会在同账号另一通道再试一次，那次探测不是换号，再遇到每日类不重复计数。
// 与连续失败上限分开计数；只在请求 goroutine 内使用，不加锁
type primaryQuotaTracker struct {
	daily, rate      int
	until, rateUntil time.Time
	last             primaryQuotaClass
	lastErr          error
	dailyAccounts    map[string]struct{}
}

// hits 返回两类额度失败的总次数
func (tracker *primaryQuotaTracker) hits() int {
	return tracker.daily + tracker.rate
}

// record 记一次额度失败；accountID 为这次尝试的账号，until 为这次写入的冷却恢复时间，零值表示不知道。
// 每日类同一账号只计一次（同账号换通道的探测不算换号），只更新最近一次的类别、错误与恢复时间
func (tracker *primaryQuotaTracker) record(class primaryQuotaClass, accountID string, err error, until time.Time) {
	tracker.last, tracker.lastErr = class, err
	tracker.until = earlierTime(tracker.until, until)
	if class == primaryQuotaRate {
		tracker.rate++
		tracker.rateUntil = earlierTime(tracker.rateUntil, until)
		return
	}
	if _, counted := tracker.dailyAccounts[accountID]; counted {
		return
	}
	if tracker.dailyAccounts == nil {
		tracker.dailyAccounts = make(map[string]struct{})
	}
	tracker.dailyAccounts[accountID] = struct{}{}
	tracker.daily++
}

// exhaustedReason 判断最近一次额度失败所在类别的次数是否已超过它的换号上限，返回结束原因
func (tracker *primaryQuotaTracker) exhaustedReason() (string, bool) {
	switch {
	case tracker.last == primaryQuotaDaily && tracker.daily > primaryQuotaSwitchLimit:
		return primaryQuotaReasonLimit, true
	case tracker.last == primaryQuotaRate && tracker.rate > primaryRateLimitSwitchLimit:
		return primaryQuotaReasonRateLimit, true
	}
	return "", false
}

// failure 构造额度不足的 429：cooling 为“其余候选全部冷却”时选号返回的全部冷却错误，cause 为写进内部日志的最后一次错误
func (tracker *primaryQuotaTracker) failure(budget *leaseWaitBudget, reason string, cooling *aistudio.AllCoolingError, cause error) *aistudio.PrimaryQuotaError {
	now := time.Now()
	if cause == nil {
		cause = tracker.lastErr
	}
	// 限流失败达到换号上限时只看限流类的恢复：每日类的长冷却不代表已启动的 Worker 何时能再用
	quotaUntil := tracker.until
	if reason == primaryQuotaReasonRateLimit {
		quotaUntil = tracker.rateUntil
	}
	return &aistudio.PrimaryQuotaError{
		ModelID: budget.modelID, Pool: budget.pool, Reason: reason, QuotaHits: tracker.daily, RateHits: tracker.rate,
		Elapsed: now.Sub(budget.startedAt), Until: primaryQuotaRetryAt(reason, quotaUntil, cooling, now), Cause: cause,
	}
}

// primaryQuotaRetryAt 返回 Ultra 主用模型额度不足时的建议重试时间（Retry-After），不早于 1 秒后：
//   - 其余候选全部冷却：号池里能服务它的账号都在冷却，取选号给出的最早恢复与本请求遇到的额度冷却中较早者，不设上限
//     （与 AllCoolingError 一致，每日限额就写真实的恢复时间）；
//   - 额度 429 后没有已启动的 Worker：5 秒后（限定在 1 到 30 秒），这次选号已为备用账号发起后台扩容，届时多半已经启动；
//   - 限流失败已达换号上限：已启动的 Worker 多半刚用满这一分钟，取本请求遇到的限流冷却中最早的恢复，不超过 cooldownQueueLimit
//     （分钟窗口），不知道时 5 秒；
//   - 每日类的换号上限、没有其他可换的账号：号池里可能还有没冷却的账号，判断要在账户池锁内分类全部候选、成本高，
//     取 5 秒后与本请求遇到的额度冷却中较早者
func primaryQuotaRetryAt(reason string, quotaUntil time.Time, cooling *aistudio.AllCoolingError, now time.Time) time.Time {
	floor := now.Add(primaryRetryMin)
	fallback := now.Add(min(max(primaryRetryFallback, primaryRetryMin), primaryRetryMax))
	var until time.Time
	switch reason {
	case primaryQuotaReasonNoWarm:
		return fallback
	case primaryQuotaReasonCooling:
		if cooling != nil {
			until = cooling.Until
		}
		until = earlierTime(until, quotaUntil)
		if until.IsZero() {
			until = fallback
		}
	case primaryQuotaReasonRateLimit:
		// 已启动的 Worker 多半刚用满这一分钟：取本请求遇到的限流冷却中最早的恢复，不超过 cooldownQueueLimit；不知道时 5 秒
		until = quotaUntil
		if until.IsZero() {
			until = fallback
		}
		until = earlierTime(until, now.Add(cooldownQueueLimit))
	default:
		until = earlierTime(fallback, quotaUntil)
	}
	if until.Before(floor) {
		return floor
	}
	return until
}

// earlierTime 返回两个时间中较早的一个，零值表示不知道、不参与比较
func earlierTime(left time.Time, right time.Time) time.Time {
	switch {
	case left.IsZero():
		return right
	case right.IsZero() || left.Before(right):
		return left
	default:
		return right
	}
}

// primaryQuotaMessage 为 Ultra 主用模型额度不足立即返回时写进请求进度的一句话：额度失败为遇到每日类额度失败的账号数，有限流类失败时另写限流失败
func primaryQuotaMessage(quota *aistudio.PrimaryQuotaError) string {
	hits := fmt.Sprintf("额度失败=%d", quota.QuotaHits)
	if quota.RateHits > 0 {
		hits += fmt.Sprintf(" | 限流失败=%d", quota.RateHits)
	}
	return fmt.Sprintf("Ultra 主用模型额度不足，立即返回 429 | 原因=%s | %s | 总耗时=%s | 建议重试=%s",
		quota.Reason, hits, quota.Elapsed.Round(time.Millisecond), retryAfterText(quota.Until))
}

// primaryQuotaSwitchMessage 为 Ultra 主用模型额度失败后换号时写进请求进度的一句话：按最近一次额度失败的类别写，两类各按 计数/换号上限
// （每日类为账号数：同账号换通道的探测与紧接着的换号同为 N），第 N 次某类失败后是这一类的第 N 次换号，超过该类换号上限的那次失败
// 不再换号、按额度不足结束；kind 为冷却类型。
// sameAccount 为这次不换号、在同账号另一通道再试（每日限额的通道共用额度还在观察中，或冷却只写在本通道）
func primaryQuotaSwitchMessage(tracker *primaryQuotaTracker, kind string, sameAccount bool, cause error) string {
	retry := "换号重试（只用已启动的 Worker）"
	if sameAccount {
		retry = "同账号换通道重试 "
	}
	if tracker.last == primaryQuotaRate {
		return fmt.Sprintf("限流 429，%s| 限流失败=%d/%d | 类型=%s | 额度失败=%d/%d | 原因=%s",
			retry, tracker.rate, primaryRateLimitSwitchLimit, kind, tracker.daily, primaryQuotaSwitchLimit, progressReason(cause))
	}
	return fmt.Sprintf("额度 429，%s| 额度失败=%d/%d | 类型=%s | 原因=%s",
		retry, tracker.daily, primaryQuotaSwitchLimit, kind, progressReason(cause))
}

// primaryAllCoolingMessage 为 Ultra 主用模型还没遇到额度 429 时选号就遇到候选全部冷却、立即返回时写进请求进度的一句话：
// 最早恢复写还剩多久（与 Retry-After 一致）
func primaryAllCoolingMessage(cooling *aistudio.AllCoolingError) string {
	return "Ultra 主用模型候选全部冷却，立即返回 429 | 最早恢复=" + retryAfterText(cooling.Until)
}

// retryAfterText 返回建议重试时间距现在多久（按秒取整，至少 1 秒），与对外的 Retry-After 一致
func retryAfterText(until time.Time) string {
	return max(time.Until(until).Round(time.Second), time.Second).String()
}

// primaryQuotaSelectionReason 判断 Ultra 主用模型额度失败后的选号错误是否按额度不足结束，返回原因与（全部冷却时的）冷却错误：
// 只用已启动 Worker 的选号没有立即可用的账号、其余候选全部冷却、没有其他符合条件的账号（已尝试的都被排除）。
// 号池一侧的暂时性原因（AccountsNotReadyError）在这里不算：调用方先照原样回到没等到关闭的原账号，回不去而上一次是额度失败时
// 再按没有其他可换的账号结束（见 generateWithRetry）；号池繁忙（已经是 429）、请求结束等其他错误照原样处理
func primaryQuotaSelectionReason(err error) (string, *aistudio.AllCoolingError, bool) {
	var noImmediate *noImmediateLeaseError
	var cooling *aistudio.AllCoolingError
	var notReady *aistudio.AccountsNotReadyError
	switch {
	case errors.As(err, &noImmediate):
		return primaryQuotaReasonNoWarm, nil, true
	case errors.As(err, &cooling):
		return primaryQuotaReasonCooling, cooling, true
	case errors.As(err, &notReady):
		return "", nil, false
	case errors.Is(err, aistudio.ErrNoEligibleAccount):
		return primaryQuotaReasonNoOther, nil, true
	default:
		return "", nil, false
	}
}

// noWarmWorker 判断选号错误是否为只用已启动 Worker 的选号没有热候选（见 withoutColdStart）
func noWarmWorker(err error) bool {
	var noImmediate *noImmediateLeaseError
	return errors.As(err, &noImmediate) && noImmediate.reason == primaryQuotaReasonNoWarm
}
