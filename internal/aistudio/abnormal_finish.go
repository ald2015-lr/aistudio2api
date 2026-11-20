package aistudio

import (
	"fmt"
	"strings"
)

// 重新生成用完仍异常结束且没有输出（ABNORMAL_FINISH_ERROR，按上游 Mag1cFall/AIStudio2API cd3d4ab 移植）
//
// 上游以函数调用格式错误（MALFORMED_FUNCTION_CALL）、意外的工具调用（UNEXPECTED_TOOL_CALL）或响应格式异常（MALFORMED_RESPONSE）结束、
// 客户端还没收到任何内容时，生成服务先同账号重新生成（见 regenerate.go）。重新生成用完（或不能重新生成：请求没有随机后缀、剩余期限
// 不够、账号这时不准入）仍这样结束时，原先照上游结果交出一个空回复，结束原因原样。开关打开时 app 层在转发事件处把这样的结束改成
// *AbnormalFinishError（对外 502，见 api/official_errors.go），客户端与中转按网关错误重试；用量照旧记进请求日志与用量账本，
// 不算账户的问题（不冷却、不换号、不触发 MODEL_FALLBACKS）。Gemini 原生接口按官方形状保留结束原因（GenerateRequest.KeepAbnormalFinish）。
// 判定放在 app 层（app/abnormal_finish.go），Generate 的结束处理不变

// AbnormalFinishError 表示上游以异常原因结束生成、这次请求没有向客户端交出任何内容
type AbnormalFinishError struct {
	// Reason 为上游的结束原因（小写，malformed_function_call、unexpected_tool_call 或 malformed_response）
	Reason string
	// Message 为上游对结束原因的说明（finishMessage），只写日志，不对外
	Message string
	// Usage 为上游最后给出的用量
	Usage *Usage
	// Generations 为这次请求在交出结果的账号上共生成的次数（含同账号重新生成），0 表示不知道
	Generations int
}

// abnormalFinishMessageLimit 为错误文本里上游说明的最大字数（完整说明另有“上游结束说明”一行）
const abnormalFinishMessageLimit = 200

// Error 返回写进管理日志的中文原因
func (e *AbnormalFinishError) Error() string {
	message := "AI Studio 异常结束生成且没有输出: " + strings.ToUpper(e.Reason)
	if e.Generations > 1 {
		message += fmt.Sprintf("（共生成 %d 次）", e.Generations)
	}
	if detail := strings.TrimSpace(e.Message); detail != "" {
		if runes := []rune(detail); len(runes) > abnormalFinishMessageLimit {
			detail = string(runes[:abnormalFinishMessageLimit]) + "…"
		}
		message += ": " + detail
	}
	return message
}

// PublicMessage 返回对客户端的英文说明：写明结束原因与共生成了几次，上游说明不对外（其中可能有会让中转自动禁用渠道的字样）
func (e *AbnormalFinishError) PublicMessage() string {
	tried := ""
	if e.Generations > 1 {
		tried = fmt.Sprintf(" (tried %d times)", e.Generations)
	}
	reason := "an abnormal finish reason"
	if AbnormalFinishReason(e.Reason) {
		reason = strings.ToUpper(strings.TrimSpace(e.Reason))
	}
	return "The model ended the response with " + reason + " and returned no content" + tried + ". Please retry."
}

// AbnormalFinishReason 判断结束原因是否为可以重新生成的异常原因（与 regenerableFinish 相同，不区分大小写）
func AbnormalFinishReason(reason string) bool {
	return regenerableFinish(strings.ToLower(strings.TrimSpace(reason)))
}
