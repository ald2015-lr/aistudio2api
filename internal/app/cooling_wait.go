package app

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// 候选全部冷却时排队等恢复的累计上限（按上游 Mag1cFall/AIStudio2API cd3d4ab 1A 与 6d8ad7d 7C 的期限部分移植）：原先每次选号各自判断
// “最早恢复不超过 cooldownQueueLimit（1 分钟）就原地等”，一个生成请求换号重试、分钟限额刚恢复又被打满时能反复各等将近 1 分钟，
// 一直等到首事件前期限以 504 结束；最早恢复晚于请求期限时也照等，等到期限同样是 504。现在整个请求（generateWithRetry，含 MODEL_FALLBACKS
// 改用的目标模型）共用一份累计：已等的冷却加上这次距最早恢复超过 cooldownQueueLimit，或最早恢复晚于选号上下文的期限（生成请求为首事件前
// 期限）时立即按全部冷却返回（AllCoolingError，对外 429 + Retry-After，MODEL_FALLBACKS 照常据此改用其他模型）。只计候选全部冷却时的
// 等待：账号忙碌、没有空闲槽位、每分钟请求已满、出口令牌等容量原因的等待不计入，照旧按排队预算（PRIMARY_QUEUE_WAIT）与出口排队上限处理

// cooldownQueueLimit 为全部候选账户冷却时一个请求排队等待恢复的累计上限，覆盖分钟限额窗口（与数据接口的 aistudio.CooldownQueueLimit 相同）
const cooldownQueueLimit = aistudio.CooldownQueueLimit

// coolingWait 为一个生成请求因候选全部冷却排队的累计时长；nil 安全
type coolingWait struct {
	total atomic.Int64
	// limit 为累计上限，0 为 cooldownQueueLimit（只有包内测试经 withCoolingWaitLimit 设别的值）
	limit time.Duration
}

type coolingWaitKey struct{}

// coolingWaitLimitKey 为测试注入的累计上限（time.Duration）：withRequestCoolingWait 新建累计时按它设上限，生产路径不设
type coolingWaitLimitKey struct{}

// withCoolingWait 给上下文挂上一份新的冷却排队累计（上限为 cooldownQueueLimit）
func withCoolingWait(ctx context.Context) context.Context {
	return context.WithValue(ctx, coolingWaitKey{}, &coolingWait{})
}

// withRequestCoolingWait 给生成请求的上下文挂上一份新的冷却排队累计（generateWithRetry 入口调用：换号重试与 MODEL_FALLBACKS 改用的
// 目标模型共用这一份）；上下文带 withCoolingWaitLimit 注入的上限时按它
func withRequestCoolingWait(ctx context.Context) context.Context {
	limit, _ := ctx.Value(coolingWaitLimitKey{}).(time.Duration)
	return context.WithValue(ctx, coolingWaitKey{}, &coolingWait{limit: limit})
}

// withCoolingWaitLimit 只给包内测试：之后 withRequestCoolingWait 新建的累计以 limit 为上限
func withCoolingWaitLimit(ctx context.Context, limit time.Duration) context.Context {
	return context.WithValue(ctx, coolingWaitLimitKey{}, limit)
}

// coolingWaitFromContext 返回上下文里的冷却排队累计；没有时为 nil（acquireLease 只按这次选号自己累计）
func coolingWaitFromContext(ctx context.Context) *coolingWait {
	wait, _ := ctx.Value(coolingWaitKey{}).(*coolingWait)
	return wait
}

// waited 返回已累计的冷却排队时长
func (wait *coolingWait) waited() time.Duration {
	if wait == nil {
		return 0
	}
	return time.Duration(wait.total.Load())
}

// add 累计一段冷却排队
func (wait *coolingWait) add(elapsed time.Duration) {
	if wait != nil && elapsed > 0 {
		wait.total.Add(int64(elapsed))
	}
}

// exceeded 判断候选全部冷却、最早恢复为 until 时是否不再排队：距恢复超过累计上限减去已累计的冷却排队，或恢复晚于 ctx 的期限
func (wait *coolingWait) exceeded(ctx context.Context, until time.Time, now time.Time) bool {
	limit := cooldownQueueLimit
	if wait != nil && wait.limit > 0 {
		limit = wait.limit
	}
	if until.Sub(now) > limit-wait.waited() {
		return true
	}
	deadline, bounded := ctx.Deadline()
	return bounded && until.After(deadline)
}
