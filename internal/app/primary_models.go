package app

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// 主用模型快速失败（PRIMARY_MODELS / PRIMARY_QUEUE_WAIT）。
//
// 号池的同时生成数约为 峰值 Worker 数 × 单账户并发（MAX_ACTIVE_WORKERS × PER_ACCOUNT_CONCURRENCY，Ultra 分区另算）。并发超过它时
// 请求在 acquireLease 里排队等空闲的账号或 Worker，原先一直等到请求超时（REQUEST_TIMEOUT，默认 5 分钟）：客户端看到的是几分钟的
// 首字延迟与随后的 504，排队的请求越积越多。列表中的模型的生成请求改为最多排队 PRIMARY_QUEUE_WAIT，超过后按 CapacityBusyError
// 结束（对外 429 + Retry-After），由客户端稍后重试，不换号、不冷却、不记降级记忆、不退避；候选全部冷却仍按原规则排队或返回 429。
//
// 预算按请求计：generateWithRetry 在第一次选号前创建一份，换号重试共用，不再每次选号各一份（原先 403 或额度 429 换号后
// 又是一份完整预算，加上失败尝试与退避，10s 的上限实际 24s 以上才返回 429）。截止时间按墙钟计 = 创建时间 + 排队上限：
// 选号的分类与复查、等待冷启动名额、失败的上游尝试（403、额度 429）、换号退避都计入；请求自己现场冷启动的浏览器启动本身
// 按实测时长延后截止时间（请求在等自己的 Worker，不是在排队；合计不超过 INIT_TIMEOUT），候选全部冷却的等待同样不计入。
// 截止时间已过后容量原因的等待立即返回 CapacityBusyError：失败后的下一次尝试只接受立即可用的账号，否则按 429 结束。
// 预算不用 context 超时，不会取消进行中的启动；Ultra 请求同样受限；不等待的选号（acquireWarmLeaseNow）本来就不排队。
// 这只是对症状的处理：根本上要让容量跟上并发（更多 Worker、更高的单账户并发），或让冷却的账号及时让出 Worker。

const (
	// primaryRetryFallback 为排队上限为 0（不排队）时 Retry-After 的建议间隔
	primaryRetryFallback = 5 * time.Second
	// primaryRetryMin 与 primaryRetryMax 为 Retry-After 建议间隔的范围
	primaryRetryMin = time.Second
	primaryRetryMax = 30 * time.Second
	// primaryStreamStartGrace 为流式请求写出响应头前，在排队上限之外再多等首个事件的余量：截止时间到后还要复查一次候选、
	// 经过账户池锁才返回错误，留出余量让 429 以 HTTP 状态返回而不是 200 之后的流内错误
	primaryStreamStartGrace = 5 * time.Second
	// waitReasonCooling 为候选全部冷却时的等待原因：这类等待不受排队上限限制，等待的时间延后截止时间，沿用原有的冷却排队规则
	waitReasonCooling = "账号冷却中"
)

// primaryModelSettings 为生效的主用模型列表（按降级判定的规则整理：去空白、去 models/ 前缀、小写）与排队上限
type primaryModelSettings struct {
	models    map[string]struct{}
	queueWait time.Duration
}

// setPrimaryModels 应用主用模型列表与排队上限（启动时与服务配置页保存后，立即生效）
func (service *trackedService) setPrimaryModels(models []string, queueWait time.Duration) {
	settings := &primaryModelSettings{models: make(map[string]struct{}, len(models)), queueWait: max(queueWait, 0)}
	for _, model := range models {
		if name := normalizeGuardModel(model); name != "" {
			settings.models[name] = struct{}{}
		}
	}
	service.primaryModels.Store(settings)
}

// primaryQueueWait 返回模型（别名已解析为规范 ID）是否为主用模型，以及它的排队上限
func (service *trackedService) primaryQueueWait(model string) (time.Duration, bool) {
	settings := service.primaryModels.Load()
	if settings == nil {
		return 0, false
	}
	_, listed := settings.models[normalizeGuardModel(model)]
	return settings.queueWait, listed
}

// StreamStartWait 返回流式请求写出响应头前等待首个事件的下限：主用模型为排队上限加余量。预算按请求计，失败的尝试与退避
// 都计入截止时间，排队超限的 429 在这之前以 HTTP 状态返回，不是 200 之后的流内错误（请求自己现场冷启动或候选冷却而延后
// 截止时间时除外）；其他模型返回 0，由 API 层按默认心跳间隔处理
func (service *trackedService) StreamStartWait(model string) time.Duration {
	wait, listed := service.primaryQueueWait(model)
	if !listed {
		return 0
	}
	return wait + primaryStreamStartGrace
}

// primaryBudget 为主用模型的请求创建排队预算（整个请求共用一份，换号重试不重新计时）；不在列表里时返回 nil
func (service *trackedService) primaryBudget(modelID string, pool aistudio.PoolScope) *leaseWaitBudget {
	wait, listed := service.primaryQueueWait(modelID)
	if !listed {
		return nil
	}
	return &leaseWaitBudget{limit: wait, startedAt: time.Now(), modelID: modelID, pool: pool}
}

// primaryQueueExceededMessage 为排队超限时写进请求进度的一句话
func primaryQueueExceededMessage(busy *aistudio.CapacityBusyError) string {
	if busy.Limit <= 0 {
		return fmt.Sprintf("主用模型不排队，立即返回 429 | 原因=%s | 总耗时=%s | 尝试=%d",
			busy.Reason, busy.Elapsed.Round(time.Millisecond), busy.Attempt)
	}
	return fmt.Sprintf("主用模型排队超过 %s，立即返回 429 | 原因=%s | 已排队=%s | 总耗时=%s | 尝试=%d",
		busy.Limit, busy.Reason, busy.Waited.Round(time.Millisecond), busy.Elapsed.Round(time.Millisecond), busy.Attempt)
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

// creditColdStart 把请求自己现场冷启动的浏览器启动时长从排队预算中抵扣（延后截止时间），合计不超过 limit（INIT_TIMEOUT）。
// 等待冷启动名额照常计入；没有预算时不做任何事
func (budget *leaseWaitBudget) creditColdStart(elapsed time.Duration, limit time.Duration) {
	if budget == nil || elapsed <= 0 {
		return
	}
	budget.coldStart = max(budget.coldStart, min(budget.coldStart+elapsed, limit))
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

// primaryModelsSummary 为热更新日志里的一句话概况
func primaryModelsSummary(models []string, queueWait time.Duration) string {
	if len(models) == 0 {
		return "关闭"
	}
	return strings.Join(models, ",") + "（排队上限 " + queueWait.String() + "）"
}
