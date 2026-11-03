package aistudio

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// 同一租约内再发一次生成请求的号池准入：客户端还没收到内容时的重新生成（regenerate.go）、原生续写被拒后改补用户消息的重发
// （prefill.go）与 long- 自动续写的下一轮（continuation.go）共用同一套预占、判断与退还。
//
// 这几种发送都在同一个租约上进行，不重新选号，但照样遵守号池的准入规则，各项的口径相同：
//   - 预占（reserveResendLocked）：每账号每分钟名额（ACCOUNT_RPM_LIMIT）与取得租约时一样 rate.pending 加一（新的选号看得到它）；
//     出口通道先跟到账户当前的通道（rebindEgressLocked），限速时同样按 GCRA 消耗一个令牌。已有没发出的预占时不重复预占。
//     发送时 beginGenerationSend 照常抵掉（只抵一次），没有发出时随租约释放退还（releaseRateLocked、releaseEgressLocked，
//     消耗过的令牌退还）；
//   - 判断（resendAdmissionLocked）：账户已不在号池或已被替换；按租约通道在该模型上的范围与全局冷却（accountCooldown）；
//     每分钟窗口内已发出数加其他租约的预占（不含本租约，也不含登记晚于本租约的续写预占）不少于上限；出口令牌没到。
//     不准入的说明写法相同（resendCooldownDenial、resendRateWaitLocked、resendEgressDenial），写进重新生成、续写回退的请求说明与
//     续写等待的进度日志，排查时分得清该等冷却、调每分钟上限还是调出口限速；
//   - 区别只在不准入时怎么办：重新生成与续写回退的重发是可选的，不准入时不发（ReserveRegeneration 不预占、返回原因，
//     原样交出结果或交给 app 层换号）；续写的下一轮已经承诺要发，先预占（ReserveContinuation）再等到准入（continuationAdmission
//     判断还要等多久与原因，continuation.go 的 waitNextRound 照它等待）：
//     同账号的续写预占按登记先后发出，后登记的不挡先登记的（两个续写互相等对方的预占会一直等下去）；账号短冷却（分钟限额、限流这类
//     剩余不到 ShortCooldownMax 的冷却，shortCooldownActive）等它结束，更长的冷却不等，照常发送，由 beginGenerationSend 返回冷却错误
//     （续写按 max_tokens 结束，见 continuation.go）。
//
// 另外 MarkLongOutput 标记服务 long- 请求的租约：这类请求每轮重新计算总时长上限，可以远超普通请求的 1 小时，app 层的排空超时与
// 后台关闭失败 Worker 的等待上限对有这类租约的账号按续写的绝对总时长上限计算（LongOutputActive）

// ContinuationWaitReason 为续写下一轮等待的原因（写进请求日志）
type ContinuationWaitReason string

const (
	// ContinuationWaitRate 为等账号每分钟窗口（ACCOUNT_RPM_LIMIT）
	ContinuationWaitRate ContinuationWaitReason = "账号每分钟窗口"
	// ContinuationWaitEgress 为等出口令牌（出口被判定限流时的令牌桶）
	ContinuationWaitEgress ContinuationWaitReason = "出口令牌"
	// ContinuationWaitCooldown 为等账号短冷却（分钟限额、限流）结束
	ContinuationWaitCooldown ContinuationWaitReason = "账号短冷却结束"
)

// continuationPendingPoll 为只因其他租约的预占占满每分钟窗口时复查的间隔：预占在发出或释放后才转成有限的截止时刻
const continuationPendingPoll = time.Second

// resendDenialReasonLimit 为不准入说明里冷却原因的最大字数
const resendDenialReasonLimit = 80

// resendAdmission 为同一租约内再发一次的准入判断结果（见 resendAdmissionLocked）
type resendAdmission struct {
	// gone 为账户已不在号池或已被替换
	gone bool
	// cooling 为租约账号在本模型范围内正处于冷却，short 为其中可以等它结束的短冷却（shortCooldownActive）
	cooling, short bool
	// reason 为要等的原因（为空时准入），wait 为按现在的状态还要等多久，detail 为写进请求说明与进度日志的具体原因
	reason ContinuationWaitReason
	wait   time.Duration
	detail string
}

// admitted 判断可以立即发送
func (admission resendAdmission) admitted() bool {
	return !admission.gone && !admission.cooling && admission.wait <= 0
}

// resendAdmissionLocked 判断租约现在能不能在同一租约内再发一次生成请求（重新生成、续写回退的重发、续写的下一轮共用，见文件开头说明）。
// scope 为冷却范围（CooldownScope）。调用方持有 p.mu
func (p *AccountPool) resendAdmissionLocked(l *AccountLease, scope string, now time.Time) resendAdmission {
	current := p.byID[l.account.ID]
	if current == nil {
		return resendAdmission{gone: true, detail: "账号已不在号池或已被替换"}
	}
	if cooldown, active := accountCooldown(current, scope, now); active {
		return resendAdmission{
			cooling: true, short: shortCooldownActive(cooldown, now), reason: ContinuationWaitCooldown,
			wait: cooldown.Until.Sub(now), detail: resendCooldownDenial(cooldown),
		}
	}
	if current != l.account {
		return resendAdmission{gone: true, detail: "账号已不在号池或已被替换"}
	}
	nowNanos := now.UnixNano()
	if wait, detail := p.resendRateWaitLocked(l, nowNanos); wait > 0 {
		return resendAdmission{reason: ContinuationWaitRate, wait: wait, detail: detail}
	}
	if rate := l.egress; rate != nil && rate.limit > 0 {
		wait := time.Duration(0)
		switch {
		case l.egressPending && l.egressCharged:
			// 本租约已预占并消耗了令牌（续写的下一轮）：等到预占时记下的可以使用的时刻
			wait = time.Duration(l.egressAdmitAt - nowNanos)
		case !l.egressPending:
			if ok, at := p.egressAdmitLocked(rate, l.egressPrimary, nowNanos); !ok {
				wait = time.Duration(at - nowNanos)
			}
		}
		if wait > 0 {
			return resendAdmission{reason: ContinuationWaitEgress, wait: wait, detail: resendEgressDenial(rate)}
		}
	}
	return resendAdmission{}
}

// resendRateWaitLocked 返回按每分钟上限还要等多久与说明：窗口内已发出数加其他租约的预占（不含本租约，也不含登记晚于本租约的续写预占）
// 不少于上限时，等第 excess+1 早的发送过期；只因其他租约的预占已满时按 continuationPendingPoll 复查。上限为 0 时为 0。调用方持有 p.mu
func (p *AccountPool) resendRateWaitLocked(l *AccountLease, now int64) (time.Duration, string) {
	limit := p.accountRateLimit
	rate := l.account.rate
	if limit <= 0 || rate == nil {
		return 0, ""
	}
	others := rate.pending
	if l.ratePending {
		others--
		if l.rateContinuation != 0 {
			for _, seq := range rate.continuing {
				if seq > l.rateContinuation {
					others--
				}
			}
		}
	}
	others = max(others, 0)
	window := int64(p.rateWindowLocked())
	start := sort.Search(len(rate.sent), func(i int) bool { return rate.sent[i] > now-window })
	live := len(rate.sent) - start
	excess := live + others - limit
	if excess < 0 {
		return 0, ""
	}
	detail := fmt.Sprintf("每分钟名额 %d/%d（已发出 %d、预占 %d，ACCOUNT_RPM_LIMIT）", live+others, limit, live, others)
	if excess < live {
		return time.Duration(rate.sent[start+excess] + window - now), detail
	}
	return continuationPendingPoll, detail
}

// reserveResendLocked 为同一租约内再发一次预占每分钟名额与出口通道（见文件开头说明）：continuation 为续写的下一轮，名额另登记序号
// （同账号的续写预占按登记先后发出）；出口先跟到账户当前的通道，限速时消耗一个令牌，令牌当前不可用时记下它可以使用的时刻
// （与这次选择在那一刻准入、消耗令牌的结果相同，之后的选择排在它后面）。已有没发出的预占时不重复预占。调用方持有 p.mu，
// 租约没有释放、账户仍在号池中
func (p *AccountPool) reserveResendLocked(l *AccountLease, now time.Time, continuation bool) {
	account := l.account
	if !l.ratePending {
		if continuation {
			p.continuationSeq++
			l.rateContinuation = p.continuationSeq
			if account.rate == nil {
				account.rate = &accountRate{}
			}
			account.rate.continuing = append(account.rate.continuing, l.rateContinuation)
		}
		p.reserveRateLocked(account, l, now)
	}
	nowNanos := now.UnixNano()
	p.rebindEgressLocked(l, nowNanos)
	rate := l.egress
	if rate == nil || l.egressPending {
		return
	}
	rate.pending++
	l.egressPending = true
	l.egressCharged = false
	l.egressAdmitAt = 0
	if rate.limit <= 0 {
		return
	}
	if ok, at := p.egressAdmitLocked(rate, l.egressPrimary, nowNanos); !ok {
		l.egressAdmitAt = at
	}
	rate.tat = max(rate.tat, nowNanos) + rate.interval
	l.egressCharged = true
	p.egressArmLocked(rate, nowNanos)
}

// ReserveRegeneration 为同一租约内的重新生成（见 regenerate.go）与原生续写被拒后改补用户消息的重发（见 prefill.go）占一个发送名额：
// 按 resendAdmissionLocked 判断，不准入（账户已不在号池或被替换、已冷却、每分钟窗口没有余量、出口令牌没到）时不预占，返回具体是哪一项
// （写进请求说明）；准入时返回空串，并按 reserveResendLocked 预占每分钟名额与出口令牌（发送时抵掉，没发出时随租约释放退还）
func (l *AccountLease) ReserveRegeneration(modelID string) string {
	if l == nil || l.pool == nil || l.account == nil {
		return ""
	}
	scope := l.CooldownScope(strings.TrimPrefix(strings.TrimSpace(modelID), "models/"))
	l.operation.Lock()
	defer l.operation.Unlock()
	if l.released {
		return "租约已释放"
	}
	p := l.pool
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	// 先跟到账户当前的出口通道再判断（冷启动的 Worker 另分了端口时，这次发送实际从新端口发出）
	if p.byID[l.account.ID] == l.account {
		p.rebindEgressLocked(l, now.UnixNano())
	}
	if admission := p.resendAdmissionLocked(l, scope, now); !admission.admitted() {
		return admission.detail
	}
	p.reserveResendLocked(l, now, false)
	return ""
}

// ReserveContinuation 在决定发送续写的下一轮时为本租约重新预占（reserveResendLocked，名额登记序号，见文件开头说明），并标记租约服务
// long- 请求；之后由 continuationAdmission 判断还要等多久（waitNextRound 照它等待）。租约已释放、账户已不在池中时不做任何事；
// 已有预占时不重复预占
func (l *AccountLease) ReserveContinuation() {
	if l == nil || l.pool == nil || l.account == nil {
		return
	}
	l.operation.Lock()
	defer l.operation.Unlock()
	if l.released {
		return
	}
	p := l.pool
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.byID[l.account.ID] != l.account {
		return
	}
	p.markLongOutputLocked(l)
	p.reserveResendLocked(l, time.Now(), true)
}

// continuationAdmission 返回续写的下一轮还要等多久才能发送、等待的原因与写进进度日志的说明（为 0 时立即发送）。判断与
// ReserveRegeneration 相同（resendAdmissionLocked），说明的写法也相同；账户已不在池中或被替换时不等；长冷却不等（照常发送，
// beginGenerationSend 返回冷却错误）
func (l *AccountLease) continuationAdmission(modelID string, now time.Time) resendAdmission {
	if l == nil || l.pool == nil || l.account == nil {
		return resendAdmission{}
	}
	scope := l.CooldownScope(strings.TrimPrefix(strings.TrimSpace(modelID), "models/"))
	p := l.pool
	p.mu.Lock()
	defer p.mu.Unlock()
	admission := p.resendAdmissionLocked(l, scope, now)
	if admission.gone || admission.cooling && !admission.short || admission.wait <= 0 {
		return resendAdmission{}
	}
	return admission
}

// resendCooldownDenial 为账号已冷却时的不准入说明：冷却截止时刻（本地时间）与原因
func resendCooldownDenial(cooldown CooldownState) string {
	reason := strings.Join(strings.Fields(cooldown.Reason), " ")
	if runes := []rune(reason); len(runes) > resendDenialReasonLimit {
		reason = string(runes[:resendDenialReasonLimit]) + "…"
	}
	if reason == "" {
		reason = "-"
	}
	return fmt.Sprintf("账号冷却至 %s（原因=%s）", cooldown.Until.Local().Format("15:04:05"), reason)
}

// resendEgressDenial 为出口令牌桶不准入时的不准入说明：出口的脱敏显示（EgressLabel）、通道与每分钟上限
func resendEgressDenial(rate *egressRate) string {
	lane := "普通"
	if rate.lane == EgressLaneUltra {
		lane = "Ultra"
	}
	label := "-"
	if rate.record != nil {
		label = rate.record.label
	}
	return fmt.Sprintf("出口 %s（%s通道）限速 %d 次/分钟，令牌未到", label, lane, rate.limit)
}

// MarkLongOutput 标记租约服务 long- 自动续写请求（app 层在 Playground 取得 long- 请求的租约后调用，续写预占时也会标记）：
// 租约释放前，账号的排空超时与后台关闭等待上限按续写的绝对总时长上限计算（见 LongOutputActive）。租约已释放、账户已不在池中或
// 已标记时不做任何事
func (l *AccountLease) MarkLongOutput() {
	if l == nil || l.pool == nil || l.account == nil {
		return
	}
	l.operation.Lock()
	defer l.operation.Unlock()
	if l.released {
		return
	}
	p := l.pool
	p.mu.Lock()
	defer p.mu.Unlock()
	p.markLongOutputLocked(l)
}

// markLongOutputLocked 为 MarkLongOutput 的实现；调用方持有 l.operation 与 p.mu，租约没有释放
func (p *AccountPool) markLongOutputLocked(l *AccountLease) {
	if l.longOutput || p.byID[l.account.ID] != l.account {
		return
	}
	l.longOutput = true
	l.account.longOutputs++
}

// LongOutputActive 判断账户上有服务 long- 自动续写请求、还没释放的租约
func (p *AccountPool) LongOutputActive(accountID string) bool {
	if p == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	account := p.byID[strings.TrimSpace(accountID)]
	return account != nil && account.longOutputs > 0
}
