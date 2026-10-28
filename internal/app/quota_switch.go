package app

// 普通号池生成请求的额度换号规则：额度失败按类计数（每日类、限流类）的换号上限、限流类换号前的短暂抖动与每请求换号硬上限，
// 由 primaryQuotaTracker（primary_models.go）按请求选取的规则执行；Ultra 主用模型的规则保持原样
//
// 原先只有 Ultra 主用模型（quotaFailFast）有额度换号上限：普通号池（主用与非主用模型）的额度 429、发出前已冷却都不计入连续失败上限
// （generateFailureLimit），也不退避，换号上限是号池的可调度账号数（线上 1.15 万）。上游对整个出口限流（裸 “Quota exceeded”，
// 新换上的账号第一次请求就被拒）时，一个请求会在同一个出口上把账号挨个试下去：线上诊断里 pro 每请求白试 7.7 次，每个到达请求打出
// 2.9–3.9 次上游发送，每次失败还逐级冷却一个账号、换到备用账号时现场冷启动。现在（不固定账号或文件绑定的请求，按请求选取一次）：
//   - 每日类（每日限额、usage limit、剩余不少于 rotationMinRemaining 的冷却）：遇到它的账号超过 normalQuotaSwitchLimit 个就结束。
//     每日额度按账号计，号池里多数账号当天还有额度，换号就是正确做法：不退避，下一次选号照常可以现场冷启动；
//     4 次封住“一批账号同时用完”的长尾（最多 5 次发送）；
//   - 限流类（裸限流，以及分钟限额等剩余不到 rotationMinRemaining 的冷却）：超过 normalRateLimitSwitchLimit 次就结束，
//     失败后紧接着的那次选号只用已启动的 Worker（同 Ultra 主用模型的 withoutColdStart）。诊断里裸限流之后同账号下一次尝试成功的只有 6%，
//     出口被限时第 3 次以后的换号只是在烧出口额度；2 次换号即每请求最多 3 次发送。裸限流（QuotaCooldown.Escalate）一律按限流类计，
//     不按写入后的冷却时长归类：逐级冷却升到第 3 档（30 分钟）后按时长会被当成每日类，出口限流时这是错的；
//   - 出口限流判定成立且限速生效时限流类仍换 2 次：每次换号都经出口令牌桶排队（取租约时占一个令牌），发送总量被限速封顶、不放大，
//     不再抖动；不做同账号换通道（另一通道同样经被限的出口）。限速判为无效（Suspended）时拒绝与发送速率无关、近似按概率拒绝，
//     每次换号是一次独立机会，放宽到 egressSuspendedRateSwitches 次；
//   - 出口正常时第 2 次起的限流类换号前等待 300–800ms（quotaJitterDelay）：错开并发请求的重试，不是降速（降速靠出口令牌桶），
//     排队预算已过时不等待，等待截到预算剩余；不改 generateRetryBackoff；
//   - 发出前已冷却（ErrAccountCoolingDown，请求没有发到上游、不消耗出口额度）不计入额度上限、不退避，只计入每请求硬上限；
//   - 每请求硬上限 generateSwitchCeiling：最多在这么多个不同账号上取得租约发起尝试，不受 attempted 的删除与 maxAttempts 的补偿影响。
// 结束时按 PrimaryQuotaError（Kind 为 QuotaFailSwitchLimit 或 QuotaFailEgress）返回：对外 429 + Retry-After（见 primaryQuotaRetryAt）。
// 指定账号的请求只能用这个账号，不计数，按原错误结束

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

const (
	// normalQuotaSwitchLimit 为普通号池遇到每日类额度失败的账号数上限：超过它就结束（最多 5 次发送）
	normalQuotaSwitchLimit = 4
	// normalRateLimitSwitchLimit 为普通号池限流类额度失败的次数上限（出口正常、出口限流且限速生效时）：超过它就结束（最多 3 次发送）
	normalRateLimitSwitchLimit = 2
	// generateSwitchCeiling 为不固定账号、文件绑定的生成请求最多尝试的不同账号数。大于 generateFailureLimit+1：
	// 只在额度失败、发出前已冷却这类不计入连续失败上限的失败混在一起时起作用
	generateSwitchCeiling = 8
	// authFailureSwitchLimit 为一个生成请求因登录失效（上游 401 / Code 16、登录页跳转）换号的次数上限，与额度换号分开计数：
	// 超过它按原错误结束（对外 503）。一批账号同时 401 时（出口被拒、Cookie 批量作废）不把号池逐个试一遍
	authFailureSwitchLimit = 2
	// dataAttemptCeiling 为 CountTokens、Live、转写、Veo 这些不固定账户的数据请求的换号上限（1 次加换 3 个，见 aistudio.WithAttemptCeiling）
	dataAttemptCeiling = 4
	// quotaJitterMin 与 quotaJitterSpread 为出口正常时第 2 次起的限流类换号前的等待：quotaJitterMin 加 [0, quotaJitterSpread) 的随机值
	quotaJitterMin    = 300 * time.Millisecond
	quotaJitterSpread = 500 * time.Millisecond
)

// quotaSwitchPolicy 为一个生成请求的额度换号规则（按请求选取一次）；限流类上限按最近一次限流类失败时的出口判定取（rateLimitFor）
type quotaSwitchPolicy struct {
	// counted 为额度失败计入换号上限：不固定账号或文件绑定的请求（指定账号的请求只试这一个账号，不计数、按原错误结束）
	counted bool
	// ultra 为 Ultra 主用模型（quotaFailFast）：上限、文案、warm-only 与“发出前已冷却”的计数规则全部沿用原样
	ultra bool
	// dailyLimit 为每日类失败的账号数上限，rateLimit 为出口正常与限速生效时限流类失败的次数上限：超过就结束
	dailyLimit int
	rateLimit  int
	// warmAfterDaily、warmAfterRate 为该类失败后紧接着的那次选号只用已启动的 Worker（withoutColdStart）
	warmAfterDaily, warmAfterRate bool
	// jitter 为出口正常时第 2 次起的限流类换号前短暂等待
	jitter bool
}

// quotaPolicyFor 返回请求的额度换号规则：budget 为主用模型的排队预算（Ultra 主用模型按原规则），counted 为不固定账号或文件绑定的请求
func (service *trackedService) quotaPolicyFor(budget *leaseWaitBudget, counted bool) quotaSwitchPolicy {
	switch {
	case budget.failFastOnQuota():
		return quotaSwitchPolicy{
			counted: true, ultra: true, dailyLimit: primaryQuotaSwitchLimit, rateLimit: primaryRateLimitSwitchLimit,
			warmAfterDaily: true, warmAfterRate: true,
		}
	case counted:
		return quotaSwitchPolicy{
			counted: true, dailyLimit: normalQuotaSwitchLimit, rateLimit: normalRateLimitSwitchLimit,
			warmAfterRate: true, jitter: true,
		}
	default:
		return quotaSwitchPolicy{}
	}
}

// classOf 返回一次额度 429 的类别。Ultra 主用模型沿用 primaryQuotaClassOf；普通策略下裸限流（Escalate，含出口限流期间的
// 短冷却与按账号降级的冷却）一律为限流类，不按写入后的恢复时间归类，其余每日限额或剩余不少于 rotationMinRemaining 为每日类
func (policy quotaSwitchPolicy) classOf(cooldown aistudio.QuotaCooldown, now time.Time) primaryQuotaClass {
	if !policy.ultra && cooldown.Escalate {
		return primaryQuotaRate
	}
	return primaryQuotaClassOf(cooldown, now)
}

// rateLimitFor 返回按出口判定调整后的限流类上限：出口限流期间限速已判为无效（Suspended）时为 egressSuspendedRateSwitches（Ultra 不调整）
func (policy quotaSwitchPolicy) rateLimitFor(verdict egressVerdict) int {
	if !policy.ultra && verdict.Limited && verdict.Suspended {
		return egressSuspendedRateSwitches
	}
	return policy.rateLimit
}

// warmAfter 判断这一类额度失败后紧接着的那次选号是否只用已启动的 Worker
func (policy quotaSwitchPolicy) warmAfter(class primaryQuotaClass) bool {
	switch class {
	case primaryQuotaDaily:
		return policy.warmAfterDaily
	case primaryQuotaRate:
		return policy.warmAfterRate
	default:
		return false
	}
}

// quotaFailureMeta 为构造额度不足错误所需的请求信息：startedAt 为生成开始时间（generateWithRetry 的入参），accounts 为尝试过的不同账号数
type quotaFailureMeta struct {
	modelID   string
	pool      aistudio.PoolScope
	startedAt time.Time
	accounts  int
}

// finishQuotaFailure 构造额度不足的 429 并写进请求进度：出口限流换号已达上限时另记一次本地返回的出口限速 429
// （侧栏的“本地限速拒绝”，见 egress_guard.go）
func (service *trackedService) finishQuotaFailure(
	requestID string, label string, tracker *primaryQuotaTracker, meta quotaFailureMeta,
	reason string, cooling *aistudio.AllCoolingError, cause error,
) *aistudio.PrimaryQuotaError {
	failure := tracker.failure(meta, reason, cooling, cause)
	if failure.Reason == primaryQuotaReasonEgress {
		service.egress.noteLocalReject(tracker.egressKey, time.Now())
	}
	service.requests.logRequestProgress(requestID, label, "WARN", primaryQuotaMessage(failure))
	return failure
}

// quotaJitterDelay 返回第 2 次起的限流类换号前的等待：quotaJitter 为 nil 时为 300–800ms 随机（只给包内测试替换）
func (service *trackedService) quotaJitterDelay() time.Duration {
	if service.quotaJitter != nil {
		return service.quotaJitter()
	}
	return quotaJitterMin + rand.N(quotaJitterSpread)
}

// waitQuotaJitter 在限流类换号前等待 delay：主用模型的等待截到排队预算剩余（budget 为 nil 时不截），不大于 0 时不等待；
// 请求结束时返回它的错误
func waitQuotaJitter(ctx context.Context, delay time.Duration, budget *leaseWaitBudget) error {
	if budget != nil {
		delay = min(delay, budget.remaining())
	}
	if delay <= 0 {
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// egressOtherOutcome 判断一次失败的生成尝试是否作为出口的“其他失败”交给判定器（计入被拒比例的分母）：只统计已发往上游的生成尝试；
// 发出前已冷却（没有发到上游）、裸限流（由 applyRateLimitCooldown 交给判定器）与请求自己结束（客户端取消、期限到期）不计
func egressOtherOutcome(requestCtx context.Context, generated, coolingDown, bare bool, err error) bool {
	return generated && !coolingDown && !bare && requestCtx.Err() == nil && egressCountsOther(err)
}

// egressCountsOther 判断失败是否计入出口的“其他失败”（5xx、首事件超时、带指标的每日或分钟限额、403、404 等）：
// 输入超限、参数错误、上游 400（内容、安全审核类）、官网停用 Build 通道的生成（PROXY_GENERATION_DISABLED）、请求取消与
// 本地 Worker 替换与出口无关，不计
func egressCountsOther(err error) bool {
	if err == nil || inputTokenLimitError(err) || errors.Is(err, aistudio.ErrInvalidArgument) ||
		aistudio.BuildGenerationDisabledFailure(err) ||
		errors.Is(err, context.Canceled) || errors.Is(err, errAccountWorkerReplaced) {
		return false
	}
	var blocked *aistudio.PromptFeedbackError
	if errors.As(err, &blocked) {
		return false
	}
	var rpcError *aistudio.RPCError
	return !errors.As(err, &rpcError) || rpcError.StatusCode != http.StatusBadRequest
}

// fileOwnerUnavailable 判断文件绑定请求的所属账号是否不可用、要改为复制文件到其他账号再试：没有符合条件的账号、全部冷却，
// 或所属账号是同出口的备用账号、出口限流期间冷启动被闸门暂停（errEgressColdStartPaused：复制到已启动的账号即可，不必等冷启动配额）
func fileOwnerUnavailable(err error) bool {
	var cooling *aistudio.AllCoolingError
	return errors.Is(err, aistudio.ErrNoEligibleAccount) || errors.As(err, &cooling) || errors.Is(err, errEgressColdStartPaused)
}

// egressLimitText 为进度日志里的出口限速：limit 次/分钟，0 为已停止（或还没启用）
func egressLimitText(limit int) string {
	if limit > 0 {
		return fmt.Sprintf("%d/分钟", limit)
	}
	return "已停止"
}
