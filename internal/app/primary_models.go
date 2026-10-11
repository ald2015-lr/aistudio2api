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
// 首字延迟与随后的 504，排队的请求越积越多。列表中的模型的生成请求改为只排队 PRIMARY_QUEUE_WAIT：预算只计 acquireLease 里 wait()
// 的等待时间（冷启动、Worker 替换、账户池锁等待都不计，也不用 context 超时，不会取消进行中的启动），用尽后按 CapacityBusyError
// 结束（对外 429 + Retry-After），由客户端稍后重试，不换号、不冷却、不记降级记忆、不退避；候选全部冷却仍按原规则排队或返回 429。
// 每次选号（含换号重试）各一份预算，Ultra 请求同样受限；不等待的选号（acquireWarmLeaseNow）本来就不排队。
// 这只是对症状的处理：根本上要让容量跟上并发（更多 Worker、更高的单账户并发），或让冷却的账号及时让出 Worker。

const (
	// primaryRetryFallback 为排队上限为 0（不排队）时 Retry-After 的建议间隔
	primaryRetryFallback = 5 * time.Second
	// primaryRetryMin 与 primaryRetryMax 为 Retry-After 建议间隔的范围
	primaryRetryMin = time.Second
	primaryRetryMax = 30 * time.Second
	// primaryStreamStartGrace 为流式请求写出响应头前，在排队上限之外再多等首个事件的余量：预算用尽后还要复查一次候选、
	// 经过账户池锁才返回错误，留出余量让 429 以 HTTP 状态返回而不是 200 之后的流内错误
	primaryStreamStartGrace = 5 * time.Second
	// waitReasonCooling 为候选全部冷却时的等待原因：这类等待不计入排队预算，沿用原有的冷却排队规则
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

// StreamStartWait 返回流式请求写出响应头前等待首个事件的下限：主用模型为排队上限加余量，保证排队超限的 429 以 HTTP 状态返回，
// 不是 200 之后的流内错误；其他模型返回 0，由 API 层按默认心跳间隔处理
func (service *trackedService) StreamStartWait(model string) time.Duration {
	wait, listed := service.primaryQueueWait(model)
	if !listed {
		return 0
	}
	return wait + primaryStreamStartGrace
}

// primaryAcquireContext 为主用模型的选号附上排队预算：每次尝试一份新预算（换号重试同样受限），不在列表里时原样返回
func (service *trackedService) primaryAcquireContext(ctx context.Context, selection aistudio.AccountSelection) context.Context {
	wait, listed := service.primaryQueueWait(selection.ModelID)
	if !listed {
		return ctx
	}
	return withLeaseWaitBudget(ctx, &leaseWaitBudget{limit: wait, modelID: selection.ModelID, pool: selection.Pool})
}

// primaryQueueExceededMessage 为排队超限时写进请求进度的一句话
func primaryQueueExceededMessage(busy *aistudio.CapacityBusyError) string {
	if busy.Limit <= 0 {
		return "主用模型不排队，立即返回 429 | 原因=" + busy.Reason
	}
	return fmt.Sprintf("主用模型排队超过 %s，立即返回 429 | 原因=%s | 已排队=%s", busy.Limit, busy.Reason, busy.Waited.Round(time.Millisecond))
}

// leaseWaitBudget 为一次选号的排队预算：累计 acquireLease 里 wait() 的等待时间，用尽后对账号忙碌、没有空闲槽位、Worker 启动或
// 扩容中这些容量原因返回 CapacityBusyError；冷却原因不计入、不受限。只在选号 goroutine 内使用，不需要加锁
type leaseWaitBudget struct {
	limit   time.Duration
	waited  time.Duration
	modelID string
	pool    aistudio.PoolScope
}

type leaseWaitBudgetKey struct{}

// withLeaseWaitBudget 把排队预算放进选号上下文：acquireLease 的签名不变，不等待的选号与其他调用方不受影响
func withLeaseWaitBudget(ctx context.Context, budget *leaseWaitBudget) context.Context {
	return context.WithValue(ctx, leaseWaitBudgetKey{}, budget)
}

// leaseWaitBudgetFromContext 返回选号上下文里的排队预算；没有时为 nil，wait 退化为不限时的等待
func leaseWaitBudgetFromContext(ctx context.Context) *leaseWaitBudget {
	budget, _ := ctx.Value(leaseWaitBudgetKey{}).(*leaseWaitBudget)
	return budget
}

// wait 在预算内等待调度信号、兜底复查或请求取消：没有预算或等待原因是冷却时与 waitScheduling 相同；预算已用尽时立即返回
// CapacityBusyError；等到兜底复查计时器结束且刚好用尽预算时同样返回；被调度信号唤醒时返回 nil，让调用方再复查一次候选
func (budget *leaseWaitBudget) wait(ctx context.Context, changed <-chan struct{}, delay time.Duration, reason string) error {
	if budget == nil || reason == waitReasonCooling {
		return waitScheduling(ctx, changed, delay)
	}
	remaining := budget.limit - budget.waited
	if remaining <= 0 {
		return budget.exhausted(reason)
	}
	timer := time.NewTimer(min(delay, remaining))
	defer timer.Stop()
	startedAt := time.Now()
	select {
	case <-ctx.Done():
		budget.waited += time.Since(startedAt)
		return ctx.Err()
	case <-changed:
		budget.waited += time.Since(startedAt)
		return nil
	case <-timer.C:
		budget.waited += time.Since(startedAt)
		if budget.waited >= budget.limit {
			return budget.exhausted(reason)
		}
		return nil
	}
}

// exhausted 构造排队超限错误：建议重试间隔为排队上限（为 0 时 5s），限定在 1s 到 30s
func (budget *leaseWaitBudget) exhausted(reason string) error {
	retry := budget.limit
	if retry <= 0 {
		retry = primaryRetryFallback
	}
	retry = min(max(retry, primaryRetryMin), primaryRetryMax)
	return &aistudio.CapacityBusyError{
		ModelID: budget.modelID, Pool: budget.pool, Reason: reason,
		Waited: budget.waited, Limit: budget.limit, Until: time.Now().Add(retry),
	}
}

// primaryModelsSummary 为热更新日志里的一句话概况
func primaryModelsSummary(models []string, queueWait time.Duration) string {
	if len(models) == 0 {
		return "关闭"
	}
	return strings.Join(models, ",") + "（排队上限 " + queueWait.String() + "）"
}
