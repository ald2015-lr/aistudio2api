package app

import (
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// 模型名带 long- 前缀的自动续写（续写本身见 aistudio/continuation.go，前缀解析见 api/model_alias.go）。生成服务这一侧负责：
//   - 开关与默认总上限（LONG_OUTPUT_ENABLED、LONG_OUTPUT_MAX_TOKENS，可热更新）：关闭时 long- 前缀照常解析，按普通请求处理；
//   - 只在 Playground GenerateContent 续写：选号先在所有账号的 Playground 里选（PlaygroundFirst，没有可用的 Playground 账号时
//     去掉再选，见 generateWithRetry），不拿来做 Build 试探；Interaction 模型与经 Build 发送的尝试按普通请求处理，各写一行说明；
//     按模型目录不续写时（总上限不超过单轮上限等）同样写一行原因；
//   - 期限：首事件前期限与首事件超时只管第一轮（收到首事件后就停了）；决定发下一轮时总时长上限从此刻重新计算、开始发送时再算一次
//     （照上游每轮重新计时），但不超过续写的绝对总时长上限（总时长上限的 longOutputDurationFactor 倍，见 generate_context.go）；
//     轮与轮之间等号池准入时暂停首事件后空闲期限的计时，开始发送下一轮时从此刻重新计（准备 WAA proof、等响应头各刷新一次），
//     不会把两轮之间的准备误判为上游空闲。第二轮起本服务的首事件后空闲期限、总时长上限或绝对总时长上限到期时只结束当前这一轮
//     （generateContext.beginLongRound 登记这一轮的结束函数），续写按这一轮失败体面结束：已输出的内容完整返回、按 max_tokens 结束；
//     离绝对总时长上限不到一个首事件后空闲期限时不再发下一轮；
//   - 每一轮结束写一行请求进度：续写第 N 轮、方式、本轮结束原因、本轮与累计输出 token、下一步；第二轮起失败时写一行原因，
//     并照流内错误的规则写上游 4xx 证据行、处理账号状态（额度冷却、登录失效），请求按 max_tokens 正常结束。
//
// 续写在同一个租约上进行：不换号、不重新排队（主用模型的排队预算只在第一轮选号时生效），租约在整段输出结束后才释放；
// 用量账本、重复回复检测与降级判定只看合并后的一条事件流，各记一次

// longOutputBuildNote 为 long- 请求的尝试经 Build 通道发送时写的说明
const longOutputBuildNote = "模型名带 long- 前缀，但本次经 Build 通道发送：自动续写只在 Playground 通道进行，按普通请求处理"

// setLongOutput 应用自动续写设置（启动时与服务配置页保存后，之后开始的请求立即生效）
func (service *trackedService) setLongOutput(enabled bool, maxTokens int) {
	service.longOutputEnabled.Store(enabled)
	service.longOutputMaxTokens.Store(int64(maxTokens))
}

// prepareLongOutput 在选号前处理 long- 请求：开关关闭或模型经 Interaction 生成时清除续写标记并写一行说明；否则填上默认总上限，
// 写一行“已识别”（是否续写在发送时按该账号的模型目录决定，不续写时另写原因）。request.Model 已是规范模型 ID
func (service *trackedService) prepareLongOutput(request *aistudio.GenerateRequest) {
	if !request.LongOutput {
		return
	}
	switch {
	case !service.longOutputEnabled.Load():
		request.LongOutput = false
		service.requests.logRequestProgress(request.ID, "service", "INFO",
			"模型名带 long- 前缀，但自动续写已关闭（LONG_OUTPUT_ENABLED=false），按普通请求处理")
	case service.pool.InteractionModel(request.Model):
		request.LongOutput = false
		service.requests.logRequestProgress(request.ID, "service", "INFO",
			"模型名带 long- 前缀，但该模型经 Interaction 通道生成：自动续写只在 Playground GenerateContent 进行，按普通请求处理")
	default:
		request.LongOutputDefaultTokens = service.longOutputMaxTokens.Load()
		limit := "未给出（能力码 91 的模型按官网续写总量，其他模型按 LONG_OUTPUT_MAX_TOKENS）"
		if request.Config.MaxOutputTokens != nil {
			limit = fmt.Sprintf("%d", *request.Config.MaxOutputTokens)
		}
		service.requests.logRequestProgress(request.ID, "service", "INFO", fmt.Sprintf(
			"模型名带 long- 前缀已识别，发送时按模型目录决定是否自动续写 | 客户端输出上限=%s | LONG_OUTPUT_MAX_TOKENS=%d",
			limit, request.LongOutputDefaultTokens))
	}
}

// longOutputNotes 为一个 long- 请求在各次尝试之间共用的日志去重：不续写的原因最多写一次
type longOutputNotes struct {
	skipped atomic.Bool
}

// continuationAttempt 为一次尝试的续写进度接收方所需的上下文
type continuationAttempt struct {
	requestID, accountLabel string
	// modelID 为请求的模型（额度冷却的范围），pool 为请求的号池
	modelID  string
	pool     aistudio.PoolScope
	lease    *aistudio.AccountLease
	phase    *generateContext
	activity *upstreamActivity
	notes    *longOutputNotes
	// rounds 为本次尝试进入了续写的后续轮次（与收到首个上游事件共用一个标记）：之后的请求阶段只来自续写的后续轮次，
	// 阶段观察者只刷新空闲计时，不再逐阶段写日志，也不启动首事件计时（首事件计时只管第一轮，见 generateWithRetry）
	rounds *atomic.Bool
}

// continuationObserver 返回一次尝试的续写进度接收方（不续写的原因在发送前、其余在该尝试的解码 goroutine 里依次调用）：写每轮进度；
// 一轮结束要接着续写时暂停首事件后空闲期限的计时（等号池准入不算空闲），开始发送下一轮时恢复计时并重新计算总时长上限；
// 第二轮起失败时写原因并照流内错误处理账号状态
func (service *trackedService) continuationObserver(attempt continuationAttempt) func(aistudio.ContinuationProgress) {
	requestID, accountLabel, activity := attempt.requestID, attempt.accountLabel, attempt.activity
	type waitKey struct {
		round  int
		reason aistudio.ContinuationWaitReason
	}
	var waitNoted waitKey
	return func(progress aistudio.ContinuationProgress) {
		if progress.Stage == aistudio.ContinuationSkipped {
			// 发送前报告（还没有上游活动），一个请求最多写一次
			if attempt.notes == nil || attempt.notes.skipped.CompareAndSwap(false, true) {
				service.requests.logRequestProgress(requestID, accountLabel, "INFO",
					"模型名带 long- 前缀，本次不自动续写："+progress.Stop+"，按普通请求处理")
			}
			return
		}
		activity.touch()
		switch progress.Stage {
		case aistudio.ContinuationRoundEnd:
			// 刚结束的这一轮不再登记结束函数：不续写时之后的期限到期按原规则结束（终态马上就发出）
			attempt.phase.endLongRound()
			service.requests.logRequestProgress(requestID, accountLabel, "INFO", continuationRoundMessage(progress))
			if progress.Next {
				if attempt.rounds != nil {
					attempt.rounds.Store(true)
				}
				// 接着发下一轮：等号池准入的这段时间不算上游空闲，开始发送时恢复计时；总时长上限从此刻重新计算，
				// 并登记这一轮的结束函数：本服务的期限到期时只结束这一轮
				activity.pause()
				remaining, ok := attempt.phase.beginLongRound(progress.Interrupt)
				if idle := attempt.phase.idleTimeout(); ok && progress.Interrupt != nil && remaining < idle {
					// 离绝对总时长上限不到一个首事件后空闲期限：下一轮多半来不及写出内容就被截断，白占一次请求额度
					progress.Interrupt(fmt.Errorf("离续写的绝对总时长上限 %s 不到 %s，不再续写", attempt.phase.longLimit, idle))
				}
			}
		case aistudio.ContinuationWaiting:
			if key := (waitKey{progress.Round, progress.WaitReason}); waitNoted != key {
				waitNoted = key
				service.requests.logRequestProgress(requestID, accountLabel, "INFO", continuationWaitMessage(progress))
			}
		case aistudio.ContinuationSending:
			// 同一轮第二次发送（原生预填充被拒、改补用户消息重发）的说明由生成服务经请求说明写一行，与客户端预填充被拒时同一写法
			activity.resume()
			attempt.phase.renewTotal()
		case aistudio.ContinuationFailed:
			attempt.phase.endLongRound()
			activity.resume()
			level := "WARN"
			if aistudio.InputTokenLimitError(progress.Err) {
				level = "INFO"
			}
			service.requests.logRequestProgress(requestID, accountLabel, level, fmt.Sprintf(
				"自动续写 | %s | 方式=%s | 累计输出=%d | 总上限=%d | 原因=%s",
				progress.Stop, progress.Mode, progress.Output, progress.Total, progressReason(progress.Err)))
			service.continuationFailureState(attempt, progress.Round, progress.Err)
		}
	}
}

// continuationFailureState 对续写后续轮次（第 round 轮）的失败照流内错误的规则处理（见 forwardEvents）：上游 4xx（发送时或流内）同样
// 记一行证据，末尾加“自动续写第 N 轮”（这一轮的失败不走 forwardEvents，见 upstream_error_log.go）；上游 401 经账户池的复核标记登录失效，
// 额度 429 写回冷却（已输出内容后的失败，不升档）；发出前被冷却拦下、输入超过上限等其他错误不处理
func (service *trackedService) continuationFailureState(attempt continuationAttempt, round int, err error) {
	if err == nil {
		return
	}
	service.logUpstreamClientError(attempt.requestID, attempt.accountLabel, err, fmt.Sprintf("自动续写第 %d 轮", round))
	lease := attempt.lease
	if lease == nil {
		return
	}
	var cooling *aistudio.AccountCoolingDownError
	if errors.As(err, &cooling) {
		return
	}
	if aistudio.DefinitiveAuthenticationFailure(err) {
		marked, stateErr := lease.MarkAuthenticationFailure(err)
		switch {
		case stateErr != nil:
			service.requests.logRequestProgress(attempt.requestID, attempt.accountLabel, "WARN", "自动续写失败后写回账号状态失败 | 原因="+progressReason(stateErr))
		case marked:
			service.workers.releaseUnschedulable(lease.Account().ID, unschedulableAuthRequired)
			service.noteAuthRequired(lease.Account().ID, attempt.accountLabel, authFromStream, err)
		}
		return
	}
	if _, ok, stateErr := service.applyQuotaCooldown(
		lease, attempt.accountLabel, attempt.modelID, err, "自动续写的后续轮次", attempt.pool, false, true,
	); ok && stateErr != nil {
		service.requests.logRequestProgress(attempt.requestID, attempt.accountLabel, "WARN", "自动续写失败后写回账号冷却失败 | 原因="+progressReason(stateErr))
	}
}

// continuationWaitMessage 为下一轮等号池准入的进度行（同一轮同一原因最多一行）：原因的写法与重新生成、续写回退不准入时的请求说明相同
// （每分钟名额、出口令牌或冷却截止，见 aistudio/resend_admission.go）
func continuationWaitMessage(progress aistudio.ContinuationProgress) string {
	wait := progress.Wait.Round(100 * time.Millisecond)
	detail := progress.WaitDetail
	if detail == "" {
		detail = "-"
	}
	return fmt.Sprintf("自动续写第 %d 轮等待%s | 方式=%s | 原因=%s | 等待=%s", progress.Round, progress.WaitReason, progress.Mode, detail, wait)
}

// continuationRoundMessage 为一轮结束的进度行
func continuationRoundMessage(progress aistudio.ContinuationProgress) string {
	next := "结束（" + progress.Stop + "）"
	if progress.Next {
		next = fmt.Sprintf("第 %d 轮", progress.Round+1)
		if progress.Prefill != "" {
			next += "（" + progress.Prefill + "）"
		}
	}
	finish := progress.RoundFinish
	if finish == "" {
		finish = "-"
	}
	return fmt.Sprintf("自动续写 | 第 %d 轮结束 | 方式=%s | 本轮结束原因=%s | 本轮输入=%d | 本轮输出=%d | 累计输出=%d | 总上限=%d | 下一步=%s",
		progress.Round, progress.Mode, finish, progress.RoundInput, progress.RoundOutput, progress.Output, progress.Total, next)
}
