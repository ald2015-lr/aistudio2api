package aistudio

import (
	"context"
	"errors"
	"fmt"
)

// 函数调用之后上游结束生成（参照上游 Mag1cFall/AIStudio2API a68acad，按本服务的生成循环改写；完成帧之后才出现流尾错误时与上游不同，
// 见 finishAfterCall）
//
// 上游的函数调用 Part 总是整个到达：已经交给客户端的函数调用是完整的。之后上游以流尾错误结束、在完成帧之前中断、拦截请求
// （frame 0 为 null 的 promptFeedback）或以内容策略类原因（safety、recitation、prohibited_content 等）结束时，原先整次请求以错误
// 结束（流式为流内错误，非流式为 5xx 或 400），OpenAI Chat 记为 content_filter、Anthropic 记为 refusal，客户端拿不到已经生成的调用、
// Codex 等编码客户端按失败重试，额度白烧一次。现在 Playground 与 Build 通道（Interaction 通道不输出函数调用）这一轮按 stop
// 正常结束：各协议照常按“含函数调用的 stop”输出（tool_calls、tool_use、completed、STOP），用量缺失时按已交出的输出本地估算；
// 原错误或原因挂在结束事件的 TrailingError 上，生成服务据此写一行 WARN、写进访问日志与用量账本的错误详情，上游错误照流内错误的
// 规则处理账号状态（见 app/runtime.go 的 settleStreamError）。long- 自动续写第二轮起失败（finishFailedRound 按 max_tokens 体面结束）
// 而前面已交出函数调用时同样按 stop 结束，原因为 ContinuationErr（账号状态已在续写进度里处理过，生成服务不再重复处理）。
//
// 只看“交给客户端”的函数调用：必须调用的工具约束扣住期间（见 tool_guard.go）观察到但没有放出的调用不算；本服务自己的结束原因
// （工具约束违约 *ToolContractError、原生续写被拒改换号 *PrefillRetryError、命中停止序列、客户端断开或取消）不在此列，照旧处理。
// 重新生成只在还没交出事件时发生（regenerate.go），交出函数调用之后不会再重新生成，两者互不影响。

// ContentPolicyFinishError 为函数调用之后上游以内容策略类原因结束：结束事件按 stop 交给客户端，原因与上游说明（finishMessage）
// 留在结束事件的 TrailingError 上。Cause 为完成帧之后上游又以错误结束（流尾错误、断流）时的那个错误：拦截原因照旧作为终止原因，
// 上游错误照流内错误的规则处理账号状态（errors.As 经 Unwrap 取得）
type ContentPolicyFinishError struct {
	Reason  string
	Message string
	Cause   error
}

// Error 返回结束原因，上游给出说明时附在后面，之后又有上游错误时再附上
func (e *ContentPolicyFinishError) Error() string {
	text := "上游以内容策略类原因结束生成: " + e.Reason
	if e.Message != "" {
		text = fmt.Sprintf("%s: %s", text, e.Message)
	}
	if e.Cause != nil {
		text += "；之后上游以错误结束: " + e.Cause.Error()
	}
	return text
}

// Unwrap 返回完成帧之后的上游错误
func (e *ContentPolicyFinishError) Unwrap() error {
	return e.Cause
}

// TrailingFinishReason 返回 TrailingError 里上游的拦截类原因（内容策略类结束原因，或拦截请求的 block reason），写进访问日志的终止原因；
// 其他错误（流尾错误、流中断等）返回空串
func TrailingFinishReason(err error) string {
	var policy *ContentPolicyFinishError
	if errors.As(err, &policy) {
		return policy.Reason
	}
	var feedback *PromptFeedbackError
	if errors.As(err, &feedback) {
		return feedback.Reason
	}
	return ""
}

// TrailingAccountError 判断 TrailingError 含上游错误（流尾错误、流中断、拦截请求、没有完成帧，包括内容策略类完成帧之后的这些错误）：
// 原先作为流内错误交出，生成服务照流内错误的规则处理账号状态、不把这次结束当作模型资格验证成功。只有内容策略类结束原因时原先就是
// 正常的结束事件，返回假
func TrailingAccountError(err error) bool {
	var policy *ContentPolicyFinishError
	if errors.As(err, &policy) {
		return policy.Cause != nil
	}
	return err != nil
}

// trailingAfterCall 判断 err 是上游在函数调用之后结束生成的错误，可以按 stop 正常结束：本服务自己的结束原因（工具约束违约、
// 原生续写被拒改换号、命中停止序列、取消与期限）与 ctx 已结束时不算
func trailingAfterCall(ctx context.Context, err error) bool {
	if err == nil || ctx.Err() != nil {
		return false
	}
	var violation *ToolContractError
	var prefill *PrefillRetryError
	switch {
	case errors.As(err, &violation), errors.As(err, &prefill), errors.Is(err, errStopSequenceMatched),
		errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return false
	}
	return true
}

// finishAfterCall 在已交出函数调用时改写本次的结束：trailing 为已经从错误换来的原因（见 trailingAfterCall），完成帧之前就结束时
// finish 为空白的结束事件。上游以内容策略类原因结束时原因留在 TrailingError（完成帧之后又有 trailing 时挂在 Cause 上，拦截原因照旧
// 记为终止原因）；long- 续写第二轮起失败（按 max_tokens 体面结束）时 TrailingError 为续写失败的原因（ContinuationErr 保留：生成服务
// 据此不重复处理账号状态）；这几种按 stop 结束，结束说明属于原来的结束原因，一并去掉。上游已给出其他结束原因（max_tokens、
// malformed_function_call 等）之后才来 trailing 时原因与结束说明照旧交给客户端，只挂上 TrailingError：同一个上游结果是否带流尾错误
// 不改变客户端看到的结束原因
func finishAfterCall(finish *Event, trailing error) {
	switch reason := finish.FinishReason; {
	case contentBlockedFinish(reason):
		trailing = &ContentPolicyFinishError{Reason: reason, Message: finish.FinishMessage, Cause: trailing}
	case finish.ContinuationErr != nil:
		if trailing == nil {
			trailing = finish.ContinuationErr
		}
	case trailing == nil:
		return
	case reason != "" && reason != "stop":
		finish.TrailingError = trailing
		return
	}
	finish.FinishReason, finish.FinishMessage, finish.StopSequence = "stop", "", ""
	finish.TrailingError = trailing
}
