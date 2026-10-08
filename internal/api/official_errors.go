package api

import (
	"context"
	"errors"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// 返回给客户端的错误统一用官方格式与官方措辞（Google Gemini API 原文）。
// 内部详细原因（中文、账号、上游协议细节）只写管理日志，不再原样返回给客户端。
const (
	officialInternalMessage    = "An internal error has occurred. Please retry or report in https://developers.generativeai.google/guide/troubleshooting"
	officialUnavailableMessage = "The service is currently unavailable."
	officialOverloadedMessage  = "The model is overloaded. Please try again later."
	officialExhaustedMessage   = "Resource has been exhausted (e.g. check quota)."
	officialDeadlineMessage    = "Deadline expired before operation could complete."
	officialInvalidMessage     = "Request contains an invalid argument."
	officialNotFoundMessage    = "Requested entity was not found."
	officialInvalidKeyMessage  = "API key not valid. Please pass a valid API key."
	officialTooLargeMessage    = "Request payload size exceeds the limit."
	// officialToolContractMessage 用于上游回复不满足客户端要求的工具约束（没有官方原文，措辞与官方风格一致）
	officialToolContractMessage = "The model response did not satisfy the requested tool constraints (tool_choice, parallel tool calls or strict schema). Please retry."
	// officialUltraUnavailableMessage 用于 /ultra 请求时 Ultra 号池没有可用账户（没有官方原文，措辞沿用服务不可用的官方说法）
	officialUltraUnavailableMessage = "The service is currently unavailable: no account in the Ultra pool is ready."
)

// 错误类别：决定 OpenAI 的 code/param 与 Anthropic 的错误类型、措辞
const (
	publicKindContextLength = "context_length"
	publicKindBlocked       = "blocked"
	publicKindRateLimit     = "rate_limit"
	publicKindModelNotFound = "model_not_found"
	publicKindAuth          = "auth"
	publicKindOverloaded    = "overloaded"
)

// publicError 为返回给客户端的错误：HTTP 状态码、Google RPC 状态名、官方措辞的英文消息与类别
type publicError struct {
	Status  int
	RPC     string
	Message string
	Kind    string
	// Code 为 OpenAI 错误体的 code（类别没有对应 code 时使用）
	Code string
}

var rpcStatusNames = map[int64]string{
	1: "CANCELLED", 2: "UNKNOWN", 3: "INVALID_ARGUMENT", 4: "DEADLINE_EXCEEDED", 5: "NOT_FOUND",
	6: "ALREADY_EXISTS", 7: "PERMISSION_DENIED", 8: "RESOURCE_EXHAUSTED", 9: "FAILED_PRECONDITION",
	10: "ABORTED", 11: "OUT_OF_RANGE", 12: "UNIMPLEMENTED", 13: "INTERNAL", 14: "UNAVAILABLE",
	15: "DATA_LOSS", 16: "UNAUTHENTICATED",
}

// blockReasonDescriptions 为 Gemini API BlockReason 枚举的官方说明
var blockReasonDescriptions = map[string]string{
	"SAFETY":             "Prompt was blocked due to safety reasons.",
	"OTHER":              "Prompt was blocked due to unknown reasons.",
	"BLOCKLIST":          "Prompt was blocked due to the terms which are included from the terminology blocklist.",
	"PROHIBITED_CONTENT": "Prompt was blocked due to prohibited content.",
	"IMAGE_SAFETY":       "Candidates blocked due to unsafe image generation content.",
}

var (
	// 上游消息里的内部标记，例如 "[original: beyond::dependency::INVALID_ARGUMENT] ..." 与 "(qos=CRITICAL_PLUS)"
	upstreamOriginalTag = regexp.MustCompile(`\[(?i:original)[^\]]*\]\s*`)
	upstreamQoSTag      = regexp.MustCompile(`\s*\((?i:qos)=[^)]*\)`)
	upstreamGenericTag  = regexp.MustCompile(`\b(?:generic|beyond)::[A-Za-z_:]+:\s*`)
)

// publicErrorFor 把内部错误映射为官方格式的客户端错误。
// 号池内部问题（账号认证、权限、目录、未就绪、重启中）一律按服务不可用返回：
// 401/403 透传给 new-api 等中转会让它自动禁用整个渠道，而这些问题与客户端的密钥无关
func publicErrorFor(err error, model string) publicError {
	if err == nil {
		return publicError{Status: http.StatusInternalServerError, RPC: "INTERNAL", Message: officialInternalMessage}
	}
	// 因降级拒绝：与上游输入被拦截（PROHIBITED_CONTENT）返回同样的 400 与错误类别（OpenAI code 为 content_policy_violation），
	// 措辞用 Google 内容策略拦截的官方说明
	var downgraded *aistudio.ModelDowngradedError
	if errors.As(err, &downgraded) {
		// 服务配置可改为 503：如实说明换了模型，客户端与中转按服务暂时不可用重试或切换渠道
		if downgraded.HTTPStatus() == http.StatusServiceUnavailable {
			return publicError{Status: http.StatusServiceUnavailable, RPC: "UNAVAILABLE", Message: aistudio.DowngradeUnavailableMessage, Kind: publicKindOverloaded}
		}
		return publicError{Status: http.StatusBadRequest, RPC: "INVALID_ARGUMENT", Message: aistudio.DowngradePublicMessage, Kind: publicKindBlocked}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return publicError{Status: http.StatusGatewayTimeout, RPC: "DEADLINE_EXCEEDED", Message: officialDeadlineMessage}
	}
	if errors.Is(err, context.Canceled) {
		return unavailablePublicError()
	}
	// 上游回复违反了客户端要求的工具约束：按网关错误返回，客户端与中转可以重试
	var contract *aistudio.ToolContractError
	if errors.As(err, &contract) {
		return publicError{Status: http.StatusBadGateway, RPC: "INTERNAL", Message: officialToolContractMessage}
	}
	var blocked *aistudio.PromptFeedbackError
	if errors.As(err, &blocked) {
		return publicError{
			Status: http.StatusBadRequest, RPC: "INVALID_ARGUMENT",
			Message: blockedPromptMessage(blocked.Reason), Kind: publicKindBlocked,
		}
	}
	var rpcError *aistudio.RPCError
	if errors.As(err, &rpcError) {
		return upstreamPublicError(rpcError)
	}
	var cooling *aistudio.AllCoolingError
	if errors.As(err, &cooling) {
		return rateLimitPublicError()
	}
	// 号池一侧的暂时性原因（账户需要重新登录、已停用、被占用，号池为空或目录尚未加载）：按服务暂时不可用返回
	var notReady *aistudio.AccountsNotReadyError
	if errors.As(err, &notReady) {
		if notReady.Pool == aistudio.PoolScopeUltra {
			// /ultra 请求：说明是 Ultra 号池暂时没有可用账户，调用方可以改走普通路径或稍后重试
			return publicError{Status: http.StatusServiceUnavailable, RPC: "UNAVAILABLE", Message: officialUltraUnavailableMessage}
		}
		return unavailablePublicError()
	}
	if errors.Is(err, aistudio.ErrModelNotFound) {
		return modelNotFoundPublicError(model)
	}
	// 号池中没有任何账户能处理请求的模型、能力或通道组合：属于请求本身的原因，指明模型时按模型不存在返回 404
	if errors.Is(err, aistudio.ErrNoEligibleAccount) {
		if strings.TrimSpace(model) != "" {
			return modelNotFoundPublicError(model)
		}
		return invalidPublicError(err)
	}
	if errors.Is(err, aistudio.ErrInvalidArgument) || isUnverifiedProtocolError(err) {
		return invalidPublicError(err)
	}
	return statusPublicError(statusFromError(err), err)
}

// upstreamPublicError 映射 AI Studio 返回的上游错误：优先按 RPC 状态码判断，其次按 HTTP 状态码
func upstreamPublicError(rpcError *aistudio.RPCError) publicError {
	name := rpcStatusNames[rpcError.Code]
	status := rpcError.StatusCode
	switch {
	case name == "RESOURCE_EXHAUSTED" || name == "" && status == http.StatusTooManyRequests:
		return rateLimitPublicError()
	case name == "UNAUTHENTICATED" || name == "PERMISSION_DENIED" || name == "NOT_FOUND" ||
		name == "" && (status == http.StatusUnauthorized || status == http.StatusForbidden || status == http.StatusNotFound):
		return unavailablePublicError()
	case name == "UNAVAILABLE" || name == "" && status == http.StatusServiceUnavailable:
		return publicError{Status: http.StatusServiceUnavailable, RPC: "UNAVAILABLE", Message: officialOverloadedMessage, Kind: publicKindOverloaded}
	case name == "DEADLINE_EXCEEDED" || name == "" && status == http.StatusGatewayTimeout:
		return publicError{Status: http.StatusGatewayTimeout, RPC: "DEADLINE_EXCEEDED", Message: officialDeadlineMessage}
	case name == "UNIMPLEMENTED":
		return publicError{Status: http.StatusNotImplemented, RPC: name, Message: upstreamMessageOr(rpcError, "Method not implemented.")}
	case name == "INVALID_ARGUMENT" || name == "FAILED_PRECONDITION" || name == "OUT_OF_RANGE" ||
		name == "" && status >= http.StatusBadRequest && status < http.StatusInternalServerError:
		// 客户端输入问题（如 token 超限、参数不合法）：返回谷歌原文，去掉内部标记
		rpc := name
		if rpc == "" {
			rpc = "INVALID_ARGUMENT"
		}
		httpStatus := status
		if httpStatus < http.StatusBadRequest || httpStatus >= http.StatusInternalServerError {
			// Build 通道的上游错误统一记为 502，这里按 RPC 状态码还原为 400
			httpStatus = http.StatusBadRequest
		}
		result := publicError{Status: httpStatus, RPC: rpc, Message: upstreamMessageOr(rpcError, officialInvalidMessage)}
		if isContextLengthMessage(result.Message) {
			result.Kind = publicKindContextLength
		}
		return result
	}
	return publicError{Status: http.StatusInternalServerError, RPC: "INTERNAL", Message: officialInternalMessage}
}

// statusPublicError 按内部错误的 HTTP 状态码给出官方措辞
func statusPublicError(status int, err error) publicError {
	switch {
	case status == http.StatusTooManyRequests:
		return rateLimitPublicError()
	case status == http.StatusServiceUnavailable:
		return unavailablePublicError()
	case status == http.StatusGatewayTimeout:
		return publicError{Status: status, RPC: "DEADLINE_EXCEEDED", Message: officialDeadlineMessage}
	case status >= http.StatusInternalServerError:
		return publicError{Status: http.StatusInternalServerError, RPC: "INTERNAL", Message: officialInternalMessage}
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return unavailablePublicError()
	case status == http.StatusNotFound:
		return publicError{Status: status, RPC: "NOT_FOUND", Message: englishMessageOr(err, officialNotFoundMessage)}
	case status == http.StatusRequestEntityTooLarge:
		return publicError{Status: status, RPC: "INVALID_ARGUMENT", Message: englishMessageOr(err, officialTooLargeMessage)}
	case status >= http.StatusBadRequest:
		result := invalidPublicError(err)
		result.Status = status
		return result
	}
	return publicError{Status: http.StatusInternalServerError, RPC: "INTERNAL", Message: officialInternalMessage}
}

func unavailablePublicError() publicError {
	return publicError{Status: http.StatusServiceUnavailable, RPC: "UNAVAILABLE", Message: officialUnavailableMessage}
}

// rateLimitPublicError 号池限流：不能用谷歌新版配额文案 "You exceeded your current quota"，
// 它在 new-api 默认的自动禁用关键词里，会让中转把整个渠道禁掉
func rateLimitPublicError() publicError {
	return publicError{Status: http.StatusTooManyRequests, RPC: "RESOURCE_EXHAUSTED", Message: officialExhaustedMessage, Kind: publicKindRateLimit}
}

func invalidPublicError(err error) publicError {
	return publicError{Status: http.StatusBadRequest, RPC: "INVALID_ARGUMENT", Message: publicInvalidMessage(err)}
}

func modelNotFoundPublicError(model string) publicError {
	model = strings.TrimPrefix(strings.TrimSpace(model), "models/")
	if model == "" {
		return publicError{Status: http.StatusNotFound, RPC: "NOT_FOUND", Message: officialNotFoundMessage, Kind: publicKindModelNotFound}
	}
	return publicError{
		Status: http.StatusNotFound, RPC: "NOT_FOUND", Kind: publicKindModelNotFound,
		Message: "models/" + model + " is not found for API version v1beta, or is not supported for generateContent. " +
			"Call ListModels to see the list of available models and their supported methods.",
	}
}

// blockedPromptMessage 输入被安全策略拦截：官方 API 返回 promptFeedback.blockReason，这里给出同样的枚举与官方说明
func blockedPromptMessage(reason string) string {
	name := strings.ToUpper(strings.TrimSpace(reason))
	name = strings.NewReplacer(" ", "_", "-", "_").Replace(name)
	name = strings.TrimPrefix(name, "BLOCK_REASON_")
	if name == "" {
		name = "BLOCK_REASON_UNSPECIFIED"
	}
	description, ok := blockReasonDescriptions[name]
	if !ok {
		description = "Prompt was blocked."
	}
	return description + " (blockReason: " + name + ")"
}

// publicInvalidMessage 请求参数错误：英文说明原样保留，中文等内部说明换成官方通用措辞
func publicInvalidMessage(err error) string {
	if err == nil {
		return officialInvalidMessage
	}
	message := strings.TrimSpace(err.Error())
	message = strings.TrimSpace(strings.TrimPrefix(message, aistudio.ErrInvalidArgument.Error()))
	message = strings.TrimSpace(strings.TrimPrefix(message, ":"))
	if message == "" || containsCJK(message) || strings.Contains(message, "AI Studio") {
		return officialInvalidMessage
	}
	return message
}

func englishMessageOr(err error, fallback string) string {
	if err == nil {
		return fallback
	}
	message := strings.TrimSpace(err.Error())
	if message == "" || containsCJK(message) || strings.Contains(message, "AI Studio") {
		return fallback
	}
	return message
}

func upstreamMessageOr(rpcError *aistudio.RPCError, fallback string) string {
	message := cleanUpstreamMessage(rpcError.Message)
	if message == "" || containsCJK(message) || message == http.StatusText(rpcError.StatusCode) {
		return fallback
	}
	return message
}

// cleanUpstreamMessage 去掉上游消息里的内部标记，只保留谷歌官方原文
func cleanUpstreamMessage(message string) string {
	message = upstreamOriginalTag.ReplaceAllString(message, "")
	message = upstreamQoSTag.ReplaceAllString(message, "")
	message = upstreamGenericTag.ReplaceAllString(message, "")
	return strings.TrimSpace(message)
}

func isContextLengthMessage(message string) bool {
	lower := strings.ToLower(message)
	return strings.Contains(lower, "input token count") || strings.Contains(lower, "maximum number of tokens") ||
		strings.Contains(lower, "context length") || strings.Contains(lower, "context window")
}

// containsCJK 判断消息是否含中日韩文字或全角符号（内部说明），这类消息不直接返回给客户端
func containsCJK(message string) bool {
	for _, r := range message {
		if unicode.In(r, unicode.Han, unicode.Hiragana, unicode.Katakana, unicode.Hangul) ||
			r >= 0x3000 && r <= 0x303F || r >= 0xFF00 && r <= 0xFFEF {
			return true
		}
	}
	return false
}

// genericStatusMessage 为只有状态码、没有可用英文说明时的官方措辞
func genericStatusMessage(status int) string {
	switch {
	case status == http.StatusUnauthorized:
		return officialInvalidKeyMessage
	case status == http.StatusNotFound:
		return officialNotFoundMessage
	case status == http.StatusRequestEntityTooLarge:
		return officialTooLargeMessage
	case status == http.StatusTooManyRequests:
		return officialExhaustedMessage
	case status == http.StatusServiceUnavailable:
		return officialUnavailableMessage
	case status == http.StatusGatewayTimeout:
		return officialDeadlineMessage
	case status >= http.StatusInternalServerError:
		return officialInternalMessage
	default:
		return officialInvalidMessage
	}
}

// publicText 兜底：直接传入的消息含中文等内部说明时换成对应状态码的官方措辞
func publicText(status int, message string) string {
	message = strings.TrimSpace(message)
	if message == "" || containsCJK(message) {
		return genericStatusMessage(status)
	}
	return message
}

func kindForOpenAICode(code string) string {
	switch code {
	case "invalid_api_key":
		return publicKindAuth
	case "model_not_found":
		return publicKindModelNotFound
	case "rate_limit_exceeded":
		return publicKindRateLimit
	default:
		return ""
	}
}

// openAIErrorBody 为 OpenAI 官方错误结构：message、type、param、code
func openAIErrorBody(public publicError) map[string]any {
	var code, param any
	switch public.Kind {
	case publicKindContextLength:
		code, param = "context_length_exceeded", "messages"
	case publicKindRateLimit:
		code = "rate_limit_exceeded"
	case publicKindModelNotFound:
		code, param = "model_not_found", "model"
	case publicKindBlocked:
		code = "content_policy_violation"
	case publicKindAuth:
		code = "invalid_api_key"
	default:
		if public.Code != "" {
			code = public.Code
		}
	}
	return map[string]any{"error": map[string]any{
		"message": public.Message,
		"type":    openAIErrorType(public.Status, ""),
		"param":   param,
		"code":    code,
	}}
}

// geminiErrorBody 为 Gemini API 官方错误结构：code、message、status
func geminiErrorBody(public publicError) map[string]any {
	return map[string]any{"error": map[string]any{
		"code":    public.Status,
		"message": public.Message,
		"status":  public.RPC,
	}}
}

// anthropicErrorBody 为 Anthropic 官方错误结构；token 超限按官方措辞以 "prompt is too long" 开头，
// Claude Code 等客户端据此自动压缩上下文
func anthropicErrorBody(public publicError, requestID string) map[string]any {
	message := public.Message
	if public.Kind == publicKindContextLength && !strings.HasPrefix(strings.ToLower(message), "prompt is too long") {
		message = "prompt is too long: " + message
	}
	body := map[string]any{
		"type":  "error",
		"error": map[string]string{"type": anthropicTypeForStatus(public.Status, public.Kind), "message": message},
	}
	if requestID != "" {
		body["request_id"] = requestID
	}
	return body
}

func anthropicTypeForStatus(status int, kind string) string {
	switch {
	case status == http.StatusBadRequest:
		return "invalid_request_error"
	case status == http.StatusUnauthorized:
		return "authentication_error"
	case status == http.StatusForbidden:
		return "permission_error"
	case status == http.StatusNotFound:
		return "not_found_error"
	case status == http.StatusRequestEntityTooLarge:
		return "request_too_large"
	case status == http.StatusTooManyRequests:
		return "rate_limit_error"
	case kind == publicKindOverloaded:
		return "overloaded_error"
	case status >= http.StatusInternalServerError:
		return "api_error"
	default:
		return "invalid_request_error"
	}
}

// retryAtProvider 由能给出最早恢复时间的错误实现（如全部候选账户冷却）
type retryAtProvider interface {
	RetryAt() time.Time
}

// setRetryAfter 在 429、503 且已知最早恢复时间时写入 Retry-After（整秒，向上取整，至少 1 秒），
// 客户端与中转据此退避，不必盲目重试
func setRetryAfter(w http.ResponseWriter, err error, status int) {
	if status != http.StatusTooManyRequests && status != http.StatusServiceUnavailable {
		return
	}
	var provider retryAtProvider
	if !errors.As(err, &provider) {
		return
	}
	until := provider.RetryAt()
	if until.IsZero() {
		return
	}
	seconds := max(int64(math.Ceil(time.Until(until).Seconds())), 1)
	w.Header().Set("Retry-After", strconv.FormatInt(seconds, 10))
}

// writeOpenAIRequestError 以 OpenAI 官方格式返回请求错误
func writeOpenAIRequestError(w http.ResponseWriter, err error) {
	public := publicErrorFor(err, accessLogModel(w))
	recordResponseError(w, err, public.Message)
	setRetryAfter(w, err, public.Status)
	writeJSON(w, public.Status, openAIErrorBody(public))
}

// writeOpenAIInvalid 以 OpenAI 官方格式返回请求体解析、校验错误（400）
func writeOpenAIInvalid(w http.ResponseWriter, err error) {
	public := invalidPublicError(err)
	recordResponseError(w, err, public.Message)
	writeJSON(w, public.Status, openAIErrorBody(public))
}

// writeGeminiRequestError 以 Gemini API 官方格式返回请求错误
func writeGeminiRequestError(w http.ResponseWriter, err error) {
	public := publicErrorFor(err, accessLogModel(w))
	recordResponseError(w, err, public.Message)
	setRetryAfter(w, err, public.Status)
	writeJSON(w, public.Status, geminiErrorBody(public))
}

// writeGeminiInvalid 以 Gemini API 官方格式返回请求体解析、校验错误（400）
func writeGeminiInvalid(w http.ResponseWriter, err error) {
	public := invalidPublicError(err)
	recordResponseError(w, err, public.Message)
	writeJSON(w, public.Status, geminiErrorBody(public))
}

// writeAnthropicRequestError 以 Anthropic 官方格式返回请求错误
func writeAnthropicRequestError(w http.ResponseWriter, err error) {
	public := publicErrorFor(err, accessLogModel(w))
	recordResponseError(w, err, public.Message)
	setRetryAfter(w, err, public.Status)
	writeJSON(w, public.Status, anthropicErrorBody(public, accessLogRequestID(w)))
}

// writeAnthropicInvalid 以 Anthropic 官方格式返回请求体解析、校验错误（400）
func writeAnthropicInvalid(w http.ResponseWriter, err error) {
	public := invalidPublicError(err)
	recordResponseError(w, err, public.Message)
	writeJSON(w, public.Status, anthropicErrorBody(public, accessLogRequestID(w)))
}

// openAIStreamError 为流式响应中途出错时写出的 OpenAI 官方错误结构
func openAIStreamError(w http.ResponseWriter, err error) map[string]any {
	return openAIErrorBody(publicErrorFor(err, accessLogModel(w)))
}

// geminiStreamError 为流式响应中途出错时写出的 Gemini 官方错误结构
func geminiStreamError(w http.ResponseWriter, err error) map[string]any {
	return geminiErrorBody(publicErrorFor(err, accessLogModel(w)))
}

// interactionStreamError 为 Interactions 流式响应中途出错时 error 事件的内容：与 geminiStreamError 同样按官方措辞脱敏，
// code 为 Google RPC 状态名
func interactionStreamError(w http.ResponseWriter, err error) map[string]any {
	public := publicErrorFor(err, accessLogModel(w))
	return map[string]any{"error": map[string]any{"code": public.RPC, "message": public.Message}}
}

// anthropicStreamError 为流式响应中途出错时写出的 Anthropic error 事件
func anthropicStreamError(w http.ResponseWriter, err error) map[string]any {
	return anthropicErrorBody(publicErrorFor(err, accessLogModel(w)), "")
}

// responsesErrorObject 为 Responses API response.failed 事件中的 error 对象
func responsesErrorObject(w http.ResponseWriter, err error) map[string]any {
	public := publicErrorFor(err, accessLogModel(w))
	code := "server_error"
	switch {
	case public.Kind == publicKindContextLength:
		// Codex 等客户端看到 context_length_exceeded 才会自动压缩上下文，invalid_prompt 只会让本轮失败
		code = "context_length_exceeded"
	case public.Status == http.StatusTooManyRequests:
		code = "rate_limit_exceeded"
	case public.Status < http.StatusInternalServerError:
		code = "invalid_prompt"
	}
	return map[string]any{"code": code, "message": public.Message}
}

// accessLogWriterOf 沿响应写入器的包装链找到访问日志写入器
func accessLogWriterOf(w http.ResponseWriter) *accessLogResponseWriter {
	for depth := 0; w != nil && depth < 8; depth++ {
		if writer, ok := w.(*accessLogResponseWriter); ok {
			return writer
		}
		unwrapper, ok := w.(interface{ Unwrap() http.ResponseWriter })
		if !ok {
			return nil
		}
		w = unwrapper.Unwrap()
	}
	return nil
}

func accessLogModel(w http.ResponseWriter) string {
	writer := accessLogWriterOf(w)
	if writer == nil {
		return ""
	}
	writer.metadata.mu.Lock()
	defer writer.metadata.mu.Unlock()
	return writer.metadata.model
}

func accessLogRequestID(w http.ResponseWriter) string {
	writer := accessLogWriterOf(w)
	if writer == nil {
		return ""
	}
	writer.metadata.mu.Lock()
	defer writer.metadata.mu.Unlock()
	return writer.metadata.requestID
}

// recordResponseError 把内部详细原因写入管理日志：运行时已写入完整错误链时不覆盖；客户端只收到官方措辞
func recordResponseError(w http.ResponseWriter, err error, message string) {
	writer := accessLogWriterOf(w)
	if writer == nil {
		return
	}
	detail := strings.TrimSpace(message)
	if err != nil {
		detail = strings.TrimSpace(err.Error())
	}
	writer.metadata.setErrorIfEmpty(detail)
}
