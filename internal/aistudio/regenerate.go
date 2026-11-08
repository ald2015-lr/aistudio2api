package aistudio

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

// 客户端还没收到内容时重新生成
//
// 上游以函数调用格式错误（MALFORMED_FUNCTION_CALL）或意外的工具调用（UNEXPECTED_TOOL_CALL）结束，或流在完成帧之前中断、
// 流尾为 5xx 状态，而这次请求还没有向客户端交出任何正文、思考或工具调用时（协议层的数据型心跳不算内容），在同一租约、同一账号上
// 重新发送，客户端只看到最后一次的结果：
//   - 因结束原因重新生成时换一个输入：上游对完全相同的输入返回完全相同的回复（见 app/duplicates.go 的排查结论），原样重发多半还是
//     同一个错误调用。app 层在加过随机后缀的请求上挂换后缀的函数（ContextWithRegenerationVariant），重发前换成新的后缀、按第一次发送
//     的步骤重建线格式请求；没加随机后缀的请求（客户端指定了 seed、温度为 0、最后一轮是工具结果、REPEAT_PROMPT_NONCE 关闭）不因
//     结束原因重新生成，写一行说明。流中断与流尾 5xx 不是确定性的，照旧原样重发
//   - 每个请求最多 regenerationLimit 次，换号重试共用这个次数（app 层每个请求建一个 RegenerationBudget 挂到各次尝试上），
//     不会因为换了账号再各来一轮
//   - 每次重新生成先经 AccountLease.ReserveRegeneration：账号已冷却、每分钟名额（ACCOUNT_RPM_LIMIT）已满或出口令牌桶不准入时
//     不重新生成，原样交出结果（错误照旧由 app 层冷却、换号），说明里写明是哪一项；准入时像取得租约一样预占名额，发送时抵掉。
//     与 long- 续写的下一轮共用同一套预占、判断与退还（见 resend_admission.go）
//   - long- 自动续写（continuation.go）：重新生成只可能落在第一轮（还没交出事件），重发经 sendPlayground 按同一份请求重新生成续写计划，
//     官网续写的编码照旧；续写决定发第二轮之后不再重新生成（longOutputRounds），第二轮起的失败按续写规则体面结束。
//     重新生成次数与续写轮数互不占用
//   - 只交出最后一次的用量（前面几次的用量与结束事件丢弃），用量账本与访问日志只记一次；降级判定的计时从第一块正文开始，
//     重新生成发生在任何正文之前，不受影响
//   - 三次生成共用首事件前期限（REQUEST_TIMEOUT；必须调用时扣住上游事件期间为总时长上限，见 tool_guard.go）：剩余期限不到本次耗时的
//     1.2 倍时不重新生成，按这次的结果结束；
//     流尾 504（DEADLINE_EXCEEDED）不在同账号重来，交给 app 层换号
//   - 首事件超时（FIRST_EVENT_TIMEOUT，app 层经 ContextWithFirstEventClock 交来）按每次发送计：每次重发重新起算（重发前与开始向上游
//     发送时），FIRST_EVENT_TIMEOUT 不到本次等首事件耗时的 1.2 倍时不重新生成（重发多半被首事件计时打断，原本能交出的结果反倒成了
//     首事件超时）。等首事件的耗时一般就是本次耗时；工具约束要求必须调用、本次发送扣住过事件时为到扣住第一个事件的耗时（首事件计时
//     那时已暂停，见 tool_guard.go），剩余期限仍按整段耗时判断
//   - 重新生成的发送不报告请求阶段（app 层的“WAA proof 完成”“上游已返回响应头”只写一次），每次重新生成写一行请求说明
//     （ReportRequestNote：有观察者时只交给观察者，app 层写进该请求的进度日志与排查记录的时间线；没有观察者时写排查记录），
//     附上被放弃那次的耗时与原文（结束说明、流尾错误说明或中断时的错误）
//   - 工具约束违约（要求必须调用却只回复文字、调用了未选择的函数、strict 参数不符，见 tool_guard.go）同样在还没交出事件时同账号重新生成，
//     共用这里的次数、准入、时间判断与首事件计时；必须调用时事件先扣住到调用出现，扣住的事件不算交出。这次的输入会与上一次相同
//     （没有随机后缀可换、工具要求已是最高的第 2 级）时同样不原样重发
//   - 已经交出内容的照旧结束：思考、正文或工具调用已经交给 app 层就不再重新生成。非流式请求同样以此为界：思考摘要先交出、之后才
//     以函数调用格式错误结束或断流的，虽然客户端要等全部收齐才看到响应，也不重新生成（交出的事件已经过 app 层的首事件判定、
//     降级判定与访问日志，没有撤回的办法）
const regenerationLimit = 2

// RegenerationBudget 为一个请求剩余的重新生成次数，同一请求的各次尝试共用
type RegenerationBudget struct {
	remaining atomic.Int32
	used      atomic.Int32
}

// NewRegenerationBudget 返回一个请求的重新生成次数（regenerationLimit）
func NewRegenerationBudget() *RegenerationBudget {
	budget := &RegenerationBudget{}
	budget.remaining.Store(regenerationLimit)
	return budget
}

// take 占用一次重新生成，次数用完时返回假；返回占用后的序号（从 1 开始）
func (budget *RegenerationBudget) take() (int, bool) {
	for {
		remaining := budget.remaining.Load()
		if remaining <= 0 {
			return 0, false
		}
		if budget.remaining.CompareAndSwap(remaining, remaining-1) {
			return int(budget.used.Add(1)), true
		}
	}
}

// giveBack 退还刚占用的一次（占用后账号不能再发，没有重新生成）
func (budget *RegenerationBudget) giveBack() {
	budget.used.Add(-1)
	budget.remaining.Add(1)
}

// Used 返回已经重新生成的次数
func (budget *RegenerationBudget) Used() int {
	if budget == nil {
		return 0
	}
	return int(budget.used.Load())
}

type regenerationBudgetContextKey struct{}

// ContextWithRegenerationBudget 把请求的重新生成次数挂到上下文上；没有挂时每次 Generate 各自按 regenerationLimit 计
func ContextWithRegenerationBudget(ctx context.Context, budget *RegenerationBudget) context.Context {
	if budget == nil {
		return ctx
	}
	return context.WithValue(ctx, regenerationBudgetContextKey{}, budget)
}

func regenerationBudgetFrom(ctx context.Context) *RegenerationBudget {
	if budget, _ := ctx.Value(regenerationBudgetContextKey{}).(*RegenerationBudget); budget != nil {
		return budget
	}
	return NewRegenerationBudget()
}

type regenerationVariantContextKey struct{}

// ContextWithRegenerationVariant 挂上因结束原因（MALFORMED_FUNCTION_CALL、UNEXPECTED_TOOL_CALL）重新生成时换输入的函数：
// 传入整理成线格式之前的请求，返回换好输入的请求与真；返回假或没有挂时不因结束原因重新生成（原样重发多半得到同样的结果）
func ContextWithRegenerationVariant(ctx context.Context, variant func(GenerateRequest) (GenerateRequest, bool)) context.Context {
	if variant == nil {
		return ctx
	}
	return context.WithValue(ctx, regenerationVariantContextKey{}, variant)
}

// regenerationVariant 按上下文里的换输入函数为因结束原因的重新生成换一个输入；没有挂或换不了时返回假
func regenerationVariant(ctx context.Context, request GenerateRequest) (GenerateRequest, bool) {
	variant, _ := ctx.Value(regenerationVariantContextKey{}).(func(GenerateRequest) (GenerateRequest, bool))
	if variant == nil {
		return request, false
	}
	return variant(request)
}

// regenerationNoVariantNote 为因结束原因、但请求没有可换的输入而不重新生成时的说明
const regenerationNoVariantNote = "客户端还没收到内容，但请求没有加随机后缀（客户端指定了 seed、温度为 0、最后一轮是工具结果或 REPEAT_PROMPT_NONCE 关闭），" +
	"原样重发多半得到同样的结果，不因结束原因重新生成"

// FirstEventClock 为 app 层每次尝试的首事件计时（FIRST_EVENT_TIMEOUT）。重新生成与续写回退的重发都在任何事件交出之前，
// 每次重发重新起算，免得几次生成加起来被首事件计时打断、把上游按时返回的结果记成首事件超时
type FirstEventClock interface {
	// FirstEventWindow 返回首事件超时，0 为关闭
	FirstEventWindow() time.Duration
	// RestartFirstEvent 为重发重新起算首事件计时；计时已到（这次尝试正被取消）或已停止时返回假
	RestartFirstEvent() bool
	// PauseFirstEvent 暂停首事件计时：生成服务扣住了上游已经返回的事件（工具约束要求必须调用、调用还没出现，见 tool_guard.go），
	// 上游已经有了响应；同一尝试内的重发照常经 RestartFirstEvent 重新起算。app 层同时让请求上下文退出首事件前的固定期限、
	// 改按上游活动判空闲（不论是否开启首事件超时）。计时已到或已停止时不做任何事
	PauseFirstEvent()
}

type firstEventClockContextKey struct{}

// ContextWithFirstEventClock 把这次尝试的首事件计时挂到上下文上（app 层每次尝试一个）
func ContextWithFirstEventClock(ctx context.Context, clock FirstEventClock) context.Context {
	if clock == nil {
		return ctx
	}
	return context.WithValue(ctx, firstEventClockContextKey{}, clock)
}

func firstEventClockFrom(ctx context.Context) FirstEventClock {
	clock, _ := ctx.Value(firstEventClockContextKey{}).(FirstEventClock)
	return clock
}

// restartFirstEvent 为重发重新起算首事件计时；没有计时时返回真
func restartFirstEvent(ctx context.Context) bool {
	if clock := firstEventClockFrom(ctx); clock != nil {
		return clock.RestartFirstEvent()
	}
	return true
}

// resendContext 返回重发用的上下文：不报告请求阶段（app 层的阶段日志只按第一次发送写），开始向上游发送时重新起算首事件计时
// （WAA proof 排队与生成不计入，与第一次发送相同）
func resendContext(ctx context.Context) context.Context {
	clock := firstEventClockFrom(ctx)
	if clock == nil {
		return ContextWithRequestPhaseObserver(ctx, nil)
	}
	return ContextWithRequestPhaseObserver(ctx, func(phase RequestPhase) {
		if phase == RequestPhaseSendingUpstream {
			_ = clock.RestartFirstEvent()
		}
	})
}

// regenerationTimeShort 判断剩余时间够不够再来一次（按耗时的 1.2 倍估计）：请求上下文期限的剩余（首事件前为 REQUEST_TIMEOUT，
// 扣住上游事件期间为总时长上限，见 app/generate_context.go）要够本次发送的整段耗时 elapsed，首事件超时（FIRST_EVENT_TIMEOUT，
// 每次重发重新起算）要够等首事件的耗时 firstEvent（没有扣住过事件时与 elapsed 相同；扣住过时为到扣住第一个事件的耗时，首事件计时
// 那时已暂停）。不够时返回写进说明的原因，够时为空
func regenerationTimeShort(ctx context.Context, elapsed, firstEvent time.Duration) string {
	if deadline, ok := ctx.Deadline(); ok {
		if remaining := time.Until(deadline); remaining < elapsed+elapsed/5 {
			return fmt.Sprintf("剩余期限不足（不到本次耗时的 1.2 倍），不重新生成 | 剩余=%s", max(remaining, 0).Round(time.Millisecond))
		}
	}
	if clock := firstEventClockFrom(ctx); clock != nil {
		if window := clock.FirstEventWindow(); window > 0 && window < firstEvent+firstEvent/5 {
			what := "本次耗时"
			if firstEvent != elapsed {
				what = "本次到扣住首个事件的耗时（" + firstEvent.Round(time.Millisecond).String() + "）"
			}
			return fmt.Sprintf("首事件超时不到%s的 1.2 倍（重发多半被首事件计时打断），不重新生成 | FIRST_EVENT_TIMEOUT=%s", what, window)
		}
	}
	return ""
}

// regenerableFinish 判断结束原因可以在客户端还没收到内容时同账号重新生成：函数调用格式错误（MALFORMED_FUNCTION_CALL）、
// 意外的工具调用（UNEXPECTED_TOOL_CALL）
func regenerableFinish(reason string) bool {
	return reason == "malformed_function_call" || reason == "unexpected_tool_call"
}

// regenerationReason 返回可以在同一账号重新生成的原因（写进请求说明），不能重新生成时为空。readErr 为读取上游响应时的非 EOF 错误
func regenerationReason(err error, readErr error, finish *Event) string {
	if err == nil {
		if finish != nil && regenerableFinish(finish.FinishReason) {
			return "结束原因=" + finish.FinishReason
		}
		return ""
	}
	var rpcError *RPCError
	if errors.As(err, &rpcError) {
		// 流尾 DEADLINE_EXCEEDED（504）是上游在这个账号上超时：同账号再来多半还是超时，交给 app 层按 5xx 换号
		if rpcError.StatusCode >= http.StatusInternalServerError && rpcError.StatusCode != http.StatusGatewayTimeout {
			return fmt.Sprintf("流尾错误=HTTP %d", rpcError.StatusCode)
		}
		return ""
	}
	if readErr != nil || StreamInterrupted(err) {
		return "流中断"
	}
	return ""
}

// streamReadFailure 记录读取上游响应时的非 EOF 错误（连接重置、HTTP/2 流被重置等），供判断流是否在完成帧之前中断
type streamReadFailure struct {
	source io.Reader
	err    error
}

func (reader *streamReadFailure) Read(buffer []byte) (int, error) {
	count, err := reader.source.Read(buffer)
	if err != nil && err != io.EOF && reader.err == nil {
		reader.err = err
	}
	return count, err
}

// currentBody 为解码 goroutine 当前持有的响应正文：重新生成后换成新的响应，panic 恢复时关闭的是这一个
type currentBody struct {
	body io.Closer
}

func (current *currentBody) Close() error {
	if current == nil || current.body == nil {
		return nil
	}
	return current.body.Close()
}

// regenerationNoteLimit 为重新生成说明里上游说明与错误的最大字数
const regenerationNoteLimit = 200

// regenerationDetail 返回被放弃这次的耗时与原文，附在重新生成说明后面：结束说明（finishMessage，例如出错的那段函数调用）、
// 流尾错误的说明或流中断时的错误。app 层只给最终交出的结束事件写“上游结束说明”，前几次的原文只在这里
func regenerationDetail(err error, finish *Event, elapsed time.Duration) string {
	detail := "本次耗时=" + elapsed.Round(time.Millisecond).String()
	label, message := "上游说明", ""
	var rpcError *RPCError
	switch {
	case errors.As(err, &rpcError):
		message = rpcError.Message
	case err != nil:
		label, message = "错误", err.Error()
	case finish != nil:
		message = finish.FinishMessage
	}
	if message = strings.TrimSpace(message); message == "" {
		return detail
	}
	if runes := []rune(message); len(runes) > regenerationNoteLimit {
		message = string(runes[:regenerationNoteLimit]) + "…"
	}
	return detail + " | " + label + "=" + message
}
