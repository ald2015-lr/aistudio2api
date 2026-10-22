package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// 排队进度日志：选号排队时写明在等什么。
//
// 原先只有主用模型在排队超限返回 429 时写一句原因（primaryQueueExceededMessage）；其他模型排队时原因只交给等待函数，
// 一直等到 REQUEST_TIMEOUT 以 504 结束，管理日志只看得到 context deadline exceeded。generateWithRetry 把请求 ID 与一份排队进度
// 放进选号上下文（与排队预算、到达序号的做法相同，acquireLease 的签名不变），acquireLease 的每次等待都经 waitQueued：
//   - 一次尝试排队满 queueProgressFirstDelay 仍没有取得账号时写一条 INFO "排队等待"（原因、已等待、尝试、模型、同条件队列位置、
//     候选计数）；很快取得租约的请求（加入队列时作为队首被立即唤醒、扩容中的短暂等待）不写，不增加正常请求的日志量；
//   - 之后仍在等待的请求每个最多每 queueProgressInterval 写一条 WARN，全进程再经令牌桶限流（每秒补 queueProgressRate 条、
//     最多攒 queueProgressBurst 条），被省略的条数在下一次放行时另写一条服务级日志，不挂到其他请求的时间线上：几千个请求同时排队时
//     不会挤掉管理日志只保留的 1500 条记录，日志锁（requestRegistry.mu）也不会成为热点；
//   - 主用模型容量原因的等待不写：它们受排队上限限制，超限时已有"主用模型排队超过…"一句；冷却等待不受上限限制，照常写；
//   - 请求在排队中超时或被取消时，选号返回的 ctx 错误带上最后的等待原因与已等待时间（%w 包装，errors.Is 的 504/499 映射不变），
//     原因随之写进 request.finished 与用量账本。
//
// 已等待只算排队的时间：每次选号从第一次等待到这次选号结束（acquireLease 返回）按墙钟计，各次选号累加；两次选号之间
// 上游处理、退避与 Worker 恢复的时间不算。原先从请求第一次排队一直按墙钟算，上游输出了几分钟后失败、换号又排队十几秒就超时的请求
// 报成“排队 5m0s 后请求超时”，看起来像号池饱和了五分钟。
// 现场启动（promote）内部的等待（替换空闲 Worker、等待冷启动名额、浏览器启动）不单独写，排队之后的现场启动按墙钟计入已等待。
// 进度只在请求 goroutine 内修改（generateWithRetry 与它同步调用的选号，与排队预算相同），不加锁；requestID 构造后不变

const (
	// queueProgressFirstDelay 为一次尝试第一条排队进度的宽限：排队这么久仍没有取得账号才写
	queueProgressFirstDelay = 2 * time.Second
	// queueProgressInterval 为同一请求重复写排队进度的最短间隔，与其他等待诊断（请求准备等待、事件流停顿）相同
	queueProgressInterval = streamStallThreshold
	// queueProgressRate 与 queueProgressBurst 为重复的排队进度全进程的令牌桶：每秒补 2 条，最多攒 30 条
	queueProgressRate  = 2.0
	queueProgressBurst = 30.0
)

// leaseWaitProgress 为一个请求在选号中的排队进度，整个请求共用一份（换号重试的已等待接着累加）
type leaseWaitProgress struct {
	requestID string
	// firstDelay 为每次尝试第一条进度的宽限，interval 为之后的最短间隔
	firstDelay time.Duration
	interval   time.Duration
	// queued 为此前已结束的各次选号排队的时间之和；queuedSince 为本次选号第一次排队的时刻，选号结束（acquireEnded）时计入 queued
	// 并清零：已等待 = queued + 本次选号已排队的时间，不含两次选号之间上游处理与退避的时间
	queued      time.Duration
	queuedSince time.Time
	// attemptStartedAt 为本次尝试第一次排队的时刻（宽限从这里算），loggedAt 为本次尝试最近一次该写进度的时刻
	// （被全局限流省略时同样推进，每个请求最多每 interval 尝试一次）；都在 beginAttempt 时清零
	attemptStartedAt time.Time
	loggedAt         time.Time
	// reason 为最近一次等待的原因，每次等待都更新
	reason  string
	attempt int
}

type leaseWaitProgressKey struct{}

// newLeaseWaitProgress 为请求创建排队进度；请求 ID 为空时返回 nil（不写进度，选号照常）
func newLeaseWaitProgress(requestID string) *leaseWaitProgress {
	requestID = strings.TrimSpace(requestID)
	if requestID == "" {
		return nil
	}
	return &leaseWaitProgress{requestID: requestID, firstDelay: queueProgressFirstDelay, interval: queueProgressInterval}
}

// withLeaseWaitProgress 把排队进度放进选号上下文；progress 为 nil 时原样返回
func withLeaseWaitProgress(ctx context.Context, progress *leaseWaitProgress) context.Context {
	if progress == nil {
		return ctx
	}
	return context.WithValue(ctx, leaseWaitProgressKey{}, progress)
}

// leaseWaitProgressFromContext 返回选号上下文里的排队进度；没有时为 nil
func leaseWaitProgressFromContext(ctx context.Context) *leaseWaitProgress {
	progress, _ := ctx.Value(leaseWaitProgressKey{}).(*leaseWaitProgress)
	return progress
}

// leaseRequestID 返回选号所属请求的 ID：排队进度里的优先（主路由也有），否则用 /trace/ 排查记录的；都没有时为空
func leaseRequestID(ctx context.Context) string {
	if progress := leaseWaitProgressFromContext(ctx); progress != nil {
		return progress.requestID
	}
	return aistudio.TraceFromContext(ctx).RequestID()
}

// beginAttempt 记一次尝试（generateWithRetry 每次选号前调用）：本次尝试的宽限与间隔重新计，已等待接着此前各次选号累加
func (progress *leaseWaitProgress) beginAttempt() {
	if progress == nil {
		return
	}
	progress.attempt++
	progress.attemptStartedAt = time.Time{}
	progress.loggedAt = time.Time{}
}

// track 记录一次等待的原因（每次等待都调用），本次选号与本次尝试的第一次等待时记下开始时刻
func (progress *leaseWaitProgress) track(now time.Time, reason string) {
	if progress == nil {
		return
	}
	progress.reason = reason
	if progress.queuedSince.IsZero() {
		progress.queuedSince = now
	}
	if progress.attemptStartedAt.IsZero() {
		progress.attemptStartedAt = now
	}
}

// acquireEnded 在一次选号结束时（acquireLease 返回，取得租约或出错）把这次选号排队的时间计入已等待；没有排队过时不变。
// 之后到下一次选号第一次等待之间（上游处理、退避、Worker 恢复）不计入
func (progress *leaseWaitProgress) acquireEnded(now time.Time) {
	if progress == nil || progress.queuedSince.IsZero() {
		return
	}
	progress.queued += max(now.Sub(progress.queuedSince), 0)
	progress.queuedSince = time.Time{}
}

// due 判断这次等待是否该写进度，该写时推进 loggedAt：first 为本次尝试的第一条（排队满宽限后），之后每 interval 最多一次
func (progress *leaseWaitProgress) due(now time.Time) (first bool, ok bool) {
	if progress == nil || progress.attemptStartedAt.IsZero() {
		return false, false
	}
	if progress.loggedAt.IsZero() {
		if now.Sub(progress.attemptStartedAt) < progress.firstDelay {
			return false, false
		}
		progress.loggedAt = now
		return true, true
	}
	if now.Sub(progress.loggedAt) < progress.interval {
		return false, false
	}
	progress.loggedAt = now
	return false, true
}

// waited 返回到 now 为止排队的时间：此前各次选号排队的时间之和，加上本次选号已排队的时间；没有排队过时为 0
func (progress *leaseWaitProgress) waited(now time.Time) time.Duration {
	if progress == nil {
		return 0
	}
	waited := progress.queued
	if !progress.queuedSince.IsZero() {
		waited += max(now.Sub(progress.queuedSince), 0)
	}
	return waited
}

// message 为一条排队进度；detail 为选号补充的模型、队列位置与候选计数（以 " | " 开头）
func (progress *leaseWaitProgress) message(now time.Time, detail string) string {
	if progress == nil {
		return ""
	}
	return fmt.Sprintf("排队等待 | 原因=%s | 已等待=%s | 尝试=%d%s",
		progress.reason, progress.waited(now).Round(time.Millisecond), max(progress.attempt, 1), detail)
}

// queueEnded 在请求排队中超时或被取消时为 ctx 错误带上最后的等待原因与已等待时间（%w 包装，errors.Is 判断不变）；
// 本次尝试没有排队过或 err 不是请求结束的错误时原样返回
func (progress *leaseWaitProgress) queueEnded(err error) error {
	if progress == nil || err == nil || progress.attemptStartedAt.IsZero() || progress.reason == "" {
		return err
	}
	outcome := ""
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		outcome = "请求超时"
	case errors.Is(err, context.Canceled):
		outcome = "请求已取消"
	default:
		return err
	}
	return fmt.Errorf("排队 %s 后%s（最后原因=%s）: %w",
		progress.waited(time.Now()).Round(time.Millisecond), outcome, progress.reason, err)
}

// queueProgressLimiter 为重复的排队进度全进程共用的令牌桶；零值可用，nil 时不限流
type queueProgressLimiter struct {
	mu         sync.Mutex
	tokens     float64
	last       time.Time
	suppressed int
}

// allow 取一个令牌：取到时返回 true 与此前被省略的条数（随即清零），取不到时记一条省略
func (limiter *queueProgressLimiter) allow(now time.Time) (bool, int) {
	if limiter == nil {
		return true, 0
	}
	limiter.mu.Lock()
	defer limiter.mu.Unlock()
	if limiter.last.IsZero() {
		limiter.tokens = queueProgressBurst
		limiter.last = now
	} else if now.After(limiter.last) {
		limiter.tokens = min(queueProgressBurst, limiter.tokens+now.Sub(limiter.last).Seconds()*queueProgressRate)
		limiter.last = now
	}
	if limiter.tokens < 1 {
		limiter.suppressed++
		return false, 0
	}
	limiter.tokens--
	suppressed := limiter.suppressed
	limiter.suppressed = 0
	return true, suppressed
}

// queueSnapshot 为一次复查时的候选计数，写进排队进度；rateLimited 为其中因每分钟请求已满（ACCOUNT_RPM_LIMIT）按忙碌分组的候选数，
// 在排除本请求已失败的 Worker 之前取，只作提示
type queueSnapshot struct {
	warmIdle, warmBusy, standby, standbyBusy int
	rateLimited                              int
	opening                                  bool
	earliestCooldown                         time.Time
}

func (snapshot queueSnapshot) text(now time.Time) string {
	text := fmt.Sprintf(" | 热空闲=%d | 热忙碌=%d | 备用=%d | 备用忙碌=%d",
		snapshot.warmIdle, snapshot.warmBusy, snapshot.standby, snapshot.standbyBusy)
	if snapshot.rateLimited > 0 {
		text += fmt.Sprintf(" | 每分钟已满=%d", snapshot.rateLimited)
	}
	if snapshot.opening {
		text += " | 启动中=是"
	}
	if remaining := snapshot.earliestCooldown.Sub(now); remaining > 0 {
		// 一秒以上按秒取整，一秒内按毫秒（分钟限额的冷却常常只剩几百毫秒）
		if remaining >= time.Second {
			remaining = remaining.Round(time.Second)
		} else {
			remaining = remaining.Round(time.Millisecond)
		}
		text += " | 最早恢复=" + remaining.String()
	}
	return text
}

// waitQueued 为 acquireLease 的一次等待：等待本身与 budget.wait 完全相同（同一个唤醒通道、复查间隔与原因），
// 只在等待前按需写排队进度；请求在等待中超时或被取消时返回的 ctx 错误带上最后的等待原因。
// 换号的选号带着没等到关闭就换走的原账号时另有一个唤醒：原账号关闭结束、可以重放时返回 errWorkerResetReady。
// 主用模型排队超限（CapacityBusyError，只经这里产生）时把这一刻的候选快照与主用模型供给里的驻留冷却数记进错误的 Detail，
// 由“主用模型排队超过…”一句写进请求进度
func (service *trackedService) waitQueued(
	ctx context.Context, progress *leaseWaitProgress, budget *leaseWaitBudget, changed <-chan struct{},
	delay time.Duration, reason string, detail func(time.Time) string,
) error {
	service.noteQueueWait(progress, budget, time.Now(), reason, detail)
	// 换号的选号带着没等到关闭就换走的原账号时，原账号关闭结束、可以重放就提前结束（errWorkerResetReady，见 worker_reset.go）
	err := workerResetWakeFromContext(ctx).wakeWait(ctx, func(waitCtx context.Context) error {
		return budget.wait(waitCtx, changed, delay, reason)
	})
	if err != nil && ctx.Err() != nil {
		return progress.queueEnded(err)
	}
	var busy *aistudio.CapacityBusyError
	if detail != nil && errors.As(err, &busy) {
		busy.Detail = detail(time.Now()) + service.workers.supplyCoolingField(busy.Pool, busy.ModelID)
	}
	return err
}

// noteQueueWait 记录等待原因，排队满宽限或距上一条满间隔时写排队进度（重复的经全局限流）
func (service *trackedService) noteQueueWait(
	progress *leaseWaitProgress, budget *leaseWaitBudget, now time.Time, reason string, detail func(time.Time) string,
) {
	if progress == nil {
		return
	}
	progress.track(now, reason)
	// 主用模型容量原因的等待受排队上限限制，超限时由"主用模型排队超过…"一句写原因，这里不再写；冷却等待不受上限限制，照常写
	if budget != nil && reason != waitReasonCooling {
		return
	}
	first, ok := progress.due(now)
	if !ok || service.requests == nil {
		return
	}
	level := "INFO"
	if !first {
		allowed, suppressed := service.queueProgress.allow(now)
		if !allowed {
			return
		}
		if suppressed > 0 {
			// 省略数是全进程的，作为服务日志单独写，不挂到当前请求的时间线上
			service.requests.log("service", "WARN", fmt.Sprintf(
				"排队进度日志已省略 %d 条（全局限流：每秒最多 %g 条）", suppressed, queueProgressRate,
			))
		}
		level = "WARN"
	}
	text := ""
	if detail != nil {
		text = detail(now)
	}
	service.requests.logRequestProgress(progress.requestID, "service", level, progress.message(now, text))
}
