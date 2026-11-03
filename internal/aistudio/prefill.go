package aistudio

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
)

// 以模型轮结尾的请求（预填充、续写）
//
// 3.5 以前的模型接受以 model 轮结尾的请求，模型从这段文字结尾处原生续写（上游 Mag1cFall/AIStudio2API 的 nativePrefillModels 名单，
// 另加 gemma- 前缀）：协议层对这些模型原样发送（GenerateRequest.PrefillTail），其余模型照旧在末尾补一条请模型接着写的用户消息
// （PrefillContinuationPrompt）。名单内的模型上游仍返回 400 “Requests ending with a model turn are not supported” 之类时，
// 同一请求改回补用户消息重发一次、写一行请求说明，并按模型与通道记住 prefillRejectionTTL，期间同模型同通道直接补用户消息。
// 重发与重新生成一样先占每分钟名额与出口令牌（AccountLease.ReserveRegeneration，不占重新生成次数）；不准入（账号已冷却、
// 每分钟名额已满或出口限速）时交出 *PrefillRetryError，app 层按可换号、不冷却的错误换号（指定账号的请求在原账号）再试，
// 下一次尝试经 applyPrefillMemory 补用户消息，不把上游的 400 交给客户端。
// Omni（gemini-omni-*）经 CreateInteractionStream 编码，不在名单里，照旧补用户消息

// nativePrefillModels 为接受以 model 轮结尾请求的规范模型 ID（gemma- 前缀的模型同样接受）
var nativePrefillModels = map[string]bool{
	"gemini-3.1-pro-preview":      true,
	"gemini-pro-latest":           true,
	"gemini-3.1-flash-lite":       true,
	"gemini-3-flash-preview":      true,
	"gemini-nano-banana-2.1":      true,
	"gemini-3.1-flash-image":      true,
	"gemini-3.1-flash-lite-image": true,
	"gemini-3-pro-image":          true,
	"gemini-2.5-flash-image":      true,
}

// PrefillContinuationPrompt 为历史以模型消息结尾时补在最后的用户消息：请模型从上一条消息结尾处接着写。
// 客户端会把输出接在原消息后面，所以要求模型不要重复已有内容
const PrefillContinuationPrompt = "Continue exactly from where your previous message ended. " +
	"Do not repeat any of it; output only the continuation."

// prefillRejectionTTL 为名单内模型被上游拒绝原生续写后，同模型同通道直接补用户消息的时长
const prefillRejectionTTL = time.Hour

// NativePrefillModel 判断模型是否接受以 model 轮结尾的请求（规范模型 ID，可带 models/ 前缀）
func NativePrefillModel(model string) bool {
	model = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(model), "models/"))
	return nativePrefillModels[model] || strings.HasPrefix(model, "gemma-")
}

// EndsWithModelText 判断历史以模型的文字消息结尾（以工具调用结尾说明缺少工具结果，不算）
func EndsWithModelText(contents []Content) bool {
	count := len(contents)
	if count == 0 || contents[count-1].Role != RoleAssistant {
		return false
	}
	return !slices.ContainsFunc(contents[count-1].Parts, func(part Part) bool { return part.FunctionCall != nil })
}

// AppendPrefillContinuation 在以模型文字消息结尾的历史末尾补一条请模型接着写的用户消息，其他历史原样返回（不改动传入的切片）
func AppendPrefillContinuation(contents []Content) []Content {
	if !EndsWithModelText(contents) {
		return contents
	}
	return append(slices.Clip(contents), Content{Role: RoleUser, Parts: []Part{{Text: PrefillContinuationPrompt}}})
}

// prefillRejections 记录上游拒绝过原生续写的模型与通道（键为 通道|模型，值为记下的时刻）
var prefillRejections sync.Map

func prefillRejectionKey(channel Channel, model string) string {
	return string(channel) + "|" + strings.ToLower(strings.TrimPrefix(strings.TrimSpace(model), "models/"))
}

// prefillRecentlyRejected 判断该模型在该通道近期被拒绝过原生续写
func prefillRecentlyRejected(channel Channel, model string, now time.Time) bool {
	value, ok := prefillRejections.Load(prefillRejectionKey(channel, model))
	if !ok {
		return false
	}
	at, _ := value.(time.Time)
	if now.Sub(at) < prefillRejectionTTL {
		return true
	}
	prefillRejections.CompareAndDelete(prefillRejectionKey(channel, model), value)
	return false
}

func rememberPrefillRejected(channel Channel, model string, now time.Time) {
	prefillRejections.Store(prefillRejectionKey(channel, model), now)
}

// prefillRejectedError 判断上游以 400 拒绝了以 model 轮结尾的请求（"Requests ending with a model turn are not supported" 之类）
func prefillRejectedError(err error) bool {
	var rpcError *RPCError
	if !errors.As(err, &rpcError) || rpcError.StatusCode != http.StatusBadRequest {
		return false
	}
	message := strings.ToLower(rpcError.Message)
	for _, marker := range []string{"model turn", "ending with a model", "last turn", "final turn", "last message"} {
		if strings.Contains(message, marker) {
			return true
		}
	}
	return false
}

// withPrefillContinuation 把以模型轮原样发送的请求改回补用户消息，并清除 PrefillTail
func withPrefillContinuation(request GenerateRequest) GenerateRequest {
	request.Contents = AppendPrefillContinuation(request.Contents)
	request.PrefillTail = false
	return request
}

// prefillFallbackNote 为原生续写被拒、改回补用户消息时写进请求日志的说明
func prefillFallbackNote(model string, channel Channel, err error) string {
	return "上游不接受以模型轮结尾的请求，改回在末尾补用户消息重试一次 | 模型=" + model + " | 通道=" + string(channel) +
		" | 原因=" + rpcErrorMessage(err)
}

// prefillFallbackDeniedNote 为原生续写被拒、但账号已不能再发（denied 为 ReserveRegeneration 给出的具体原因）而不在本账号重发时
// 写进请求日志的说明：交给 app 层换号（指定账号的请求在原账号）再试，下一次尝试补用户消息
func prefillFallbackDeniedNote(model string, channel Channel, denied string, err error) string {
	return "上游不接受以模型轮结尾的请求，但" + denied + "，不在本次尝试重发，换号再试（下一次补用户消息） | 模型=" + model +
		" | 通道=" + string(channel) + " | 原因=" + rpcErrorMessage(err)
}

// PrefillRetryError 表示原生续写被上游以 400 拒绝、本账号又不能马上改回补用户消息重发（Denied 为 ReserveRegeneration 给出的原因）。
// app 层按可换号的错误处理（retryableGenerateAccountError）：不写额度冷却、不计入出口判定与连续失败上限，换号（指定账号的请求在原账号）
// 再试，下一次尝试经 applyPrefillMemory 补用户消息。Unwrap 返回上游的 400，不能再试时照旧按这个 400 交给客户端
type PrefillRetryError struct {
	Err    error
	Denied string
}

func (e *PrefillRetryError) Error() string {
	if e == nil || e.Err == nil {
		return "上游不接受以模型轮结尾的请求"
	}
	return e.Err.Error()
}

func (e *PrefillRetryError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

func rpcErrorMessage(err error) string {
	var rpcError *RPCError
	if errors.As(err, &rpcError) && rpcError.Message != "" {
		return rpcError.Message
	}
	if err == nil {
		return ""
	}
	return err.Error()
}

// applyPrefillMemory 在发送前处理以模型轮原样发送的请求：Interaction 模型与近期在该通道被拒过的模型直接补用户消息
func applyPrefillMemory(ctx context.Context, request GenerateRequest, channel Channel, model string, interaction bool) GenerateRequest {
	if !request.PrefillTail {
		return request
	}
	if !EndsWithModelText(request.Contents) {
		request.PrefillTail = false
		return request
	}
	if interaction {
		return withPrefillContinuation(request)
	}
	if prefillRecentlyRejected(channel, model, time.Now()) {
		TraceFromContext(ctx).Note("该模型近期在 " + string(channel) + " 通道拒绝过以模型轮结尾的请求：直接在末尾补用户消息")
		return withPrefillContinuation(request)
	}
	return request
}
