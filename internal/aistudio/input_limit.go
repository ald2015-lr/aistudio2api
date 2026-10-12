package aistudio

import "strings"

// InputTokenLimitError 判断上游错误是否为"输入 token 超过上限"（提示词过长）。
//
// 这类错误与账号无关：换号、冷却都不会让同一段输入变短。上游有时按 400 INVALID_ARGUMENT 返回
// （"The input token count (N) exceeds the maximum number of tokens allowed (M)."），
// 部分账号的 Playground 与 Build 对输入另有上限（如 131072 token），超过时按 429 RESOURCE_EXHAUSTED 返回
// （"The input token count exceeds the maximum number of tokens allowed 131072"）。两种写法都按文案识别，
// 不看状态码：调度不冷却账号、不换号（Playground 超限仍可改走 Build 再试一次），对外一律按 400 返回
func InputTokenLimitError(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	if strings.Contains(message, "input token count") && strings.Contains(message, "exceed") {
		return true
	}
	return strings.Contains(message, "exceeds the maximum number of tokens allowed")
}
