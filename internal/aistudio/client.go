package aistudio

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	// MakerSuiteRPCBase 是现场确认的 AI Studio RPC 根地址
	MakerSuiteRPCBase = "https://alkalimakersuite-pa.clients6.google.com/$rpc/google.internal.alkali.applications.makersuite.v1.MakerSuiteService/"
	// JSONProtobufContentType 是 MakerSuite 使用的数组协议媒体类型
	JSONProtobufContentType = "application/json+protobuf"
)

// RPCRequest 描述交给认证传输层的单次 MakerSuite 请求
type RPCRequest struct {
	Method    string
	URL       string
	AccountID string
	RequestID string
	Header    http.Header
	Body      []byte
	Streaming bool
}

// RPCResponse 描述认证传输层返回的响应头和实时正文
type RPCResponse struct {
	StatusCode int
	Header     http.Header
	Body       io.ReadCloser
}

// RPCTransport 负责认证、账户租约、Cookie 写回和真实网络发送
type RPCTransport interface {
	Do(context.Context, RPCRequest) (*RPCResponse, error)
}

// ProtectedTransport 原子完成 fresh WAA proof、field 5 写入和同 context 发送
type ProtectedTransport interface {
	DoProtected(context.Context, GenerateRequest, RPCRequest) (*RPCResponse, error)
}

// VideoProtectedTransport 原子完成 Veo fresh WAA proof、field 8 写入和同 context 发送
type VideoProtectedTransport interface {
	DoProtectedVideo(context.Context, VideoRequest, RPCRequest) (*RPCResponse, error)
}

// ProtectedTransportFunc 将函数适配为 ProtectedTransport
type ProtectedTransportFunc func(context.Context, GenerateRequest, RPCRequest) (*RPCResponse, error)

// DoProtected 调用受保护传输函数
func (f ProtectedTransportFunc) DoProtected(ctx context.Context, request GenerateRequest, rpc RPCRequest) (*RPCResponse, error) {
	return f(ctx, request, rpc)
}

// RequestContext 保存账户运行时提供的协议上下文
type RequestContext struct {
	Timezone string
}

// RequestContextProvider 按账户返回当前协议上下文
type RequestContextProvider interface {
	RequestContext(context.Context, string) (RequestContext, error)
}

// RequestContextProviderFunc 将函数适配为 RequestContextProvider
type RequestContextProviderFunc func(context.Context, string) (RequestContext, error)

// RequestContext 调用上下文函数
func (f RequestContextProviderFunc) RequestContext(ctx context.Context, accountID string) (RequestContext, error) {
	return f(ctx, accountID)
}

// ClientOptions 定义协议客户端的窄依赖
type ClientOptions struct {
	Transport       RPCTransport
	Protected       ProtectedTransport
	ContextProvider RequestContextProvider
}

// Client 实现 AI Studio 私有协议核心
type Client struct {
	transport       RPCTransport
	protected       ProtectedTransport
	contextProvider RequestContextProvider
	catalogMu       sync.RWMutex
	catalogs        map[string]modelCatalog
	tierMu          sync.RWMutex
	tiers           map[string]BenefitTier
}

var _ Service = (*Client)(nil)

// NewClient 创建协议客户端
func NewClient(options ClientOptions) (*Client, error) {
	if options.Transport == nil {
		return nil, fmt.Errorf("AI Studio transport 不能为空")
	}
	if options.Protected == nil {
		return nil, fmt.Errorf("AI Studio protected transport 不能为空")
	}
	return &Client{
		transport:       options.Transport,
		protected:       options.Protected,
		contextProvider: options.ContextProvider,
		catalogs:        make(map[string]modelCatalog),
		tiers:           make(map[string]BenefitTier),
	}, nil
}

// RPCError 保存上游状态和协议错误码
type RPCError struct {
	Method     string
	StatusCode int
	Code       int64
	Message    string
	// Metadata 为 ErrorInfo 的元数据，并补上 QuotaFailure（含 GeminiApiErrorDetails 里的同形状）违规项的字段：
	// quota_metric、quota_id、quota_limit_value（违规项的 quota_value），以及维度 quota_dimension_<键>
	// （如 quota_dimension_model=gemini-3.1-pro、quota_dimension_location=global）；ErrorInfo 已给出的字段不覆盖
	Metadata map[string]string
	// RetryDelay 为上游建议的重试等待时间，依次取 google.rpc.RetryInfo、文案里的 “retry in <时长>”、HTTP Retry-After；0 表示没有
	RetryDelay time.Duration
	// DetailTypes 为状态 details 里出现过的全部类型（去掉 type.googleapis.com/ 前缀，按出现顺序去重），
	// 包括没有解析的类型：冷却日志在没有识别出额度指标时写出它，用来核实裸 429 带了哪些详情
	DetailTypes []string
	// UnparsedDetailType 与 UnparsedDetail 为带着 GeminiApiErrorDetails、却没有解析出额度指标（全部详情处理完后 Metadata 没有 quota_metric，
	// 即冷却日志没有“指标”、带“详情类型=”）时，其中一个 GeminiApiErrorDetails 的类型（去掉 type.googleapis.com/ 前缀）与值的原文
	// （json.Compact 压缩，超过 unparsedDetailLimit 字节时按 UTF-8 边界截断并注明总字节数）：优先取第一个没有解析出违规项的，
	// 都解析出违规项（只有 quota_id、维度等，没有指标）时取第一个。这种 429 的布局与已知样本不同，冷却只能按文案判断
	// （裸 “Quota exceeded” 按裸限流逐级兜底），app 层把原文写进管理日志（“额度 429 详情未识别”），据此扩展解析。
	// 任何详情（ErrorInfo、QuotaFailure 或 GeminiApiErrorDetails）解析出指标时为空，不影响分类
	UnparsedDetailType string
	UnparsedDetail     string
}

// unparsedDetailLimit 为 RPCError.UnparsedDetail 的最大字节数
const unparsedDetailLimit = 1200

// maxRetryDelay 为采纳上游重试建议的上限，异常的超大值按此截断
const maxRetryDelay = 24 * time.Hour

// rpcDetailTypePrefix 为 details 类型 URL 的通用前缀，DetailTypes 去掉它
const rpcDetailTypePrefix = "type.googleapis.com/"

// retryInMessagePattern 匹配错误文案里的恢复时间（Gemini API 形状的 429 只在文案里给出，如 “Please retry in 16h6m4.716518794s.”），
// 时长为 Go 的 time.ParseDuration 格式；“Retry with an alternative Gemini model” 不匹配
var retryInMessagePattern = regexp.MustCompile(`(?i)\bretry in ((?:\d+(?:\.\d+)?(?:h|ms|us|µs|ns|m|s))+)`)

// Error 返回结构化上游错误
func (e *RPCError) Error() string {
	if e.Code != 0 {
		return fmt.Sprintf("AI Studio %s 返回 HTTP %d、协议错误码 %d: %s", e.Method, e.StatusCode, e.Code, e.Message)
	}
	return fmt.Sprintf("AI Studio %s 返回 HTTP %d: %s", e.Method, e.StatusCode, e.Message)
}

// HTTPStatus 返回上游 HTTP 状态
func (e *RPCError) HTTPStatus() int {
	return e.StatusCode
}

func (c *Client) do(ctx context.Context, method string, accountID string, requestID string, body []byte, streaming bool) (*RPCResponse, error) {
	rpc := newRPCRequest(method, accountID, requestID, body, streaming)
	c.applyBenefitTier(method, accountID, rpc.Header)
	response, err := c.transport.Do(ctx, rpc)
	if err != nil {
		return nil, fmt.Errorf("发送 AI Studio %s: %w", method, err)
	}
	return validateRPCResponse(method, response)
}

func (c *Client) doProtected(ctx context.Context, request GenerateRequest, body []byte) (*RPCResponse, error) {
	rpc := newRPCRequest("GenerateContent", request.AccountID, request.ID, body, true)
	c.applyBenefitTier(rpc.Method, request.AccountID, rpc.Header)
	if request.ImageRoute {
		// 图像请求使用独立于文本预热页的扩展头默认值
		rpc.Header["X-Goog-Ext-519733851-Bin"] = nil
	}
	trace := TraceFromContext(ctx)
	trace.RecordRequest(rpc.Method, rpc.URL, body)
	response, err := c.protected.DoProtected(ctx, request, rpc)
	if err != nil {
		return nil, fmt.Errorf("发送 AI Studio GenerateContent: %w", err)
	}
	response, err = validateRPCResponse("GenerateContent", response)
	if err != nil {
		return nil, err
	}
	response.Body = trace.TeeUpstream(response.Body)
	return response, nil
}

func (c *Client) doProtectedVideo(ctx context.Context, request VideoRequest, body []byte) (*RPCResponse, error) {
	transport, ok := c.protected.(VideoProtectedTransport)
	if !ok {
		return nil, fmt.Errorf("AI Studio protected transport 不支持 GenerateVideo")
	}
	rpc := newRPCRequest("GenerateVideo", request.AccountID, "", body, false)
	c.applyBenefitTier(rpc.Method, request.AccountID, rpc.Header)
	response, err := transport.DoProtectedVideo(ctx, request, rpc)
	if err != nil {
		return nil, fmt.Errorf("发送 AI Studio GenerateVideo: %w", err)
	}
	return validateRPCResponse("GenerateVideo", response)
}

func newRPCRequest(method string, accountID string, requestID string, body []byte, streaming bool) RPCRequest {
	return RPCRequest{
		Method:    method,
		URL:       MakerSuiteRPCBase + method,
		AccountID: accountID,
		RequestID: requestID,
		Header: http.Header{
			"Content-Type": []string{JSONProtobufContentType},
		},
		Body:      append([]byte(nil), body...),
		Streaming: streaming,
	}
}

func validateRPCResponse(method string, response *RPCResponse) (*RPCResponse, error) {
	if response == nil || response.Body == nil {
		return nil, fmt.Errorf("AI Studio %s transport 返回空响应", method)
	}
	if response.StatusCode != http.StatusOK {
		defer response.Body.Close()
		raw, readErr := io.ReadAll(response.Body)
		if readErr != nil {
			return nil, fmt.Errorf("读取 AI Studio %s 错误响应: %w", method, readErr)
		}
		rpcError := DecodeRPCError(method, response.StatusCode, raw)
		if rpcError.RetryDelay <= 0 {
			rpcError.RetryDelay = parseRetryAfter(response.Header.Get("Retry-After"), time.Now())
		}
		return nil, rpcError
	}
	contentType := response.Header.Get("Content-Type")
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil || !strings.EqualFold(mediaType, JSONProtobufContentType) {
		response.Body.Close()
		return nil, fmt.Errorf("AI Studio %s 返回未识别的 Content-Type %q", method, contentType)
	}
	return response, nil
}

// DecodeRPCError 解析独立状态和流式封装中的上游错误
func DecodeRPCError(method string, statusCode int, raw []byte) *RPCError {
	rpcError := &RPCError{
		Method:     method,
		StatusCode: statusCode,
		Message:    http.StatusText(statusCode),
	}
	value, err := decodeJSONValue(raw)
	if err != nil {
		return rpcError
	}
	root, err := rawArray(value, "$", value)
	if err != nil || len(root) == 0 {
		return rpcError
	}
	// 两种形状：独立状态 [code, message, details]，以及流式封装 [null, [code, message, details]]。
	// 状态可以没有 message（[8,null,details]、[12]），这时仍要保留状态码与详情
	provider, providerPath := root, "$"
	if len(root) > 1 && bytes.HasPrefix(bytes.TrimSpace(root[1]), []byte("[")) {
		provider, err = rawArray(root[1], "$[1]", value)
		if err != nil || len(provider) == 0 {
			return rpcError
		}
		providerPath = "$[1]"
	}
	if code, err := rawInt64(provider[0], providerPath+"[0]", value); err == nil {
		rpcError.Code = code
	}
	if len(provider) > 1 && !isJSONNull(provider[1]) {
		if message, err := rawString(provider[1], providerPath+"[1]", value); err == nil && message != "" {
			rpcError.Message = message
		}
	}
	if len(provider) > 2 && !isJSONNull(provider[2]) {
		decodeRPCErrorMetadata(rpcError, provider[2])
	}
	// 没有 RetryInfo 时采纳文案里的 “retry in <时长>”；validateRPCResponse 只在两者都没有时才读 Retry-After 头
	if rpcError.RetryDelay <= 0 {
		rpcError.RetryDelay = retryDelayFromMessage(rpcError.Message)
	}
	return rpcError
}

// retryDelayFromMessage 解析错误文案里的 “retry in <Go 时长>”：没有或无法解析时返回 0，超过 maxRetryDelay 时截到 maxRetryDelay
// （不像 RetryInfo 那样丢弃：文案给出的是额度恢复时间，免费层每日限额可能接近一整天）
func retryDelayFromMessage(message string) time.Duration {
	match := retryInMessagePattern.FindStringSubmatch(message)
	if match == nil {
		return 0
	}
	delay, err := time.ParseDuration(match[1])
	if err != nil || delay <= 0 {
		return 0
	}
	return min(delay, maxRetryDelay)
}

// decodeRPCErrorMetadata 读取状态 details（[[类型 URL, 值]...]）：RetryInfo、QuotaFailure、GeminiApiErrorDetails 与 ErrorInfo，
// 并把出现过的全部类型记进 DetailTypes。单个详情项或字段解析失败只跳过它，不影响其余详情
func decodeRPCErrorMetadata(rpcError *RPCError, raw json.RawMessage) {
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return
	}
	// recordedUnparsed 为已记下的 GeminiApiErrorDetails 原文来自没有解析出违规项的那一个
	recordedUnparsed := false
	for _, item := range items {
		var detail []json.RawMessage
		if json.Unmarshal(item, &detail) != nil || len(detail) == 0 {
			continue
		}
		var typeURL string
		if err := json.Unmarshal(detail[0], &typeURL); err != nil {
			continue
		}
		if name := strings.TrimPrefix(strings.TrimSpace(typeURL), rpcDetailTypePrefix); name != "" && !slices.Contains(rpcError.DetailTypes, name) {
			rpcError.DetailTypes = append(rpcError.DetailTypes, name)
		}
		if len(detail) < 2 {
			continue
		}
		if typeURL == "type.googleapis.com/google.rpc.RetryInfo" {
			var info []json.RawMessage
			if json.Unmarshal(detail[1], &info) == nil && len(info) > 0 {
				rpcError.RetryDelay = max(rpcError.RetryDelay, decodeRPCRetryDuration(info[0]))
			}
			continue
		}
		if typeURL == "type.googleapis.com/google.rpc.QuotaFailure" {
			decodeRPCQuotaFailure(rpcError, detail[1])
			continue
		}
		// AI Studio 网页后端的每日限额（free 每天 10 次、pro/ultra 每天 50 次）只在这里给出指标、限额名、模型与限额值，
		// 文案是通用的 “You exceeded your current quota” 或 “You've reached your usage limit”。按后缀匹配，包名变化时仍能识别
		// 不论是否解析出违规项都记下原文：优先第一个没有解析出违规项的，都解析出时为第一个；最后解析出指标时清空（见 RPCError.UnparsedDetail）
		if strings.HasSuffix(typeURL, ".GeminiApiErrorDetails") {
			parsed := decodeGeminiAPIErrorDetails(rpcError, detail[1])
			if rpcError.UnparsedDetailType == "" || (!parsed && !recordedUnparsed) {
				rpcError.UnparsedDetailType = strings.TrimPrefix(strings.TrimSpace(typeURL), rpcDetailTypePrefix)
				rpcError.UnparsedDetail = unparsedDetailText(detail[1])
				recordedUnparsed = !parsed
			}
			continue
		}
		if typeURL != "type.googleapis.com/google.rpc.ErrorInfo" {
			continue
		}
		var info []json.RawMessage
		if err := json.Unmarshal(detail[1], &info); err != nil || len(info) < 3 {
			continue
		}
		var metadata [][]string
		if err := json.Unmarshal(info[2], &metadata); err != nil {
			continue
		}
		for _, pair := range metadata {
			if len(pair) < 2 || pair[0] == "" {
				continue
			}
			if rpcError.Metadata == nil {
				rpcError.Metadata = make(map[string]string)
			}
			rpcError.Metadata[pair[0]] = pair[1]
		}
	}
	// 与冷却日志写“详情类型=”的条件（没有指标）一致：ErrorInfo、QuotaFailure 或 GeminiApiErrorDetails 给出了指标时不需要原文
	if quotaMetricName(rpcError.Metadata["quota_metric"]) != "" {
		rpcError.UnparsedDetailType, rpcError.UnparsedDetail = "", ""
	}
}

// decodeRPCRetryDuration 读取 RetryInfo.retry_delay（google.protobuf.Duration 的数组形式 [秒, 纳秒]）
func decodeRPCRetryDuration(raw json.RawMessage) time.Duration {
	var fields []json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || len(fields) == 0 {
		return 0
	}
	var seconds int64
	if err := json.Unmarshal(fields[0], &seconds); err != nil {
		// 数组协议中 int64 可能编码为字符串
		var text string
		if json.Unmarshal(fields[0], &text) != nil {
			return 0
		}
		if seconds, err = strconv.ParseInt(text, 10, 64); err != nil {
			return 0
		}
	}
	if seconds < 0 || seconds > int64(maxRetryDelay/time.Second) {
		return 0
	}
	delay := time.Duration(seconds) * time.Second
	if len(fields) > 1 && !isJSONNull(fields[1]) {
		var nanos int64
		if json.Unmarshal(fields[1], &nanos) != nil || nanos < 0 || nanos >= int64(time.Second) {
			return 0
		}
		delay += time.Duration(nanos)
	}
	return delay
}

// parseRetryAfter 解析 HTTP Retry-After：整数秒或 HTTP 日期；无法识别、已过期或超过上限时返回 0
func parseRetryAfter(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	var delay time.Duration
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil {
		if seconds <= 0 || seconds > int64(maxRetryDelay/time.Second) {
			return 0
		}
		delay = time.Duration(seconds) * time.Second
	} else if until, err := http.ParseTime(value); err == nil {
		delay = until.Sub(now)
	}
	if delay <= 0 || delay > maxRetryDelay {
		return 0
	}
	return delay
}

// geminiAPIErrorQuotaFailureIndex 为 GeminiApiErrorDetails 数组里 QuotaFailure 形状的下标（现场样本：前 28 个元素为 null）
const geminiAPIErrorQuotaFailureIndex = 28

// decodeGeminiAPIErrorDetails 读取 GeminiApiErrorDetails（AI Studio 网页后端的私有详情）里的 QuotaFailure 形状 [[违规项...]]：
// 先试下标 28，不是这种形状时扫描第一层的其余元素，取第一个像 QuotaFailure 的（见 quotaFailureShape），按 decodeRPCQuotaFailure 读取。
// 返回是否解析出违规项；解析失败只跳过
func decodeGeminiAPIErrorDetails(rpcError *RPCError, raw json.RawMessage) bool {
	var fields []json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return false
	}
	if len(fields) > geminiAPIErrorQuotaFailureIndex && quotaFailureShape(fields[geminiAPIErrorQuotaFailureIndex]) {
		return decodeRPCQuotaFailure(rpcError, fields[geminiAPIErrorQuotaFailureIndex])
	}
	for index, field := range fields {
		if index != geminiAPIErrorQuotaFailureIndex && quotaFailureShape(field) {
			return decodeRPCQuotaFailure(rpcError, field)
		}
	}
	return false
}

// unparsedDetailText 返回详情值压缩后的原文（压缩失败时为去掉首尾空白的原样），超过 unparsedDetailLimit 字节时按 UTF-8 边界截断，
// 末尾注明 “…（已截断，共 N 字节）”，N 为压缩后的总字节数
func unparsedDetailText(raw json.RawMessage) string {
	text := string(bytes.TrimSpace(raw))
	var compact bytes.Buffer
	if json.Compact(&compact, raw) == nil {
		text = compact.String()
	}
	if len(text) <= unparsedDetailLimit {
		return text
	}
	cut := unparsedDetailLimit
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return fmt.Sprintf("%s…（已截断，共 %d 字节）", text[:cut], len(text))
}

// quotaFailureShape 判断 raw 是否像数组协议的 QuotaFailure：[[违规项...]]，且至少一个违规项的下标 3 是带 “/” 的指标名
// （如 generativelanguage.googleapis.com/generate_requests_per_model_per_user），或下标 4 是带 “Per” 的限额名
// （如 GenerateRequestsPerProjectPerUserPerModelPerDay），免得把其他嵌套数组误认成违规项
func quotaFailureShape(raw json.RawMessage) bool {
	var failure []json.RawMessage
	if json.Unmarshal(raw, &failure) != nil || len(failure) == 0 {
		return false
	}
	for _, violation := range quotaViolations(failure[0]) {
		var metric, id string
		if len(violation) > 3 && json.Unmarshal(violation[3], &metric) == nil && strings.Contains(metric, "/") {
			return true
		}
		if len(violation) > 4 && json.Unmarshal(violation[4], &id) == nil && strings.Contains(id, "Per") {
			return true
		}
	}
	return false
}

// decodeRPCQuotaFailure 读取 google.rpc.QuotaFailure 违规项（数组协议下标为 proto 字段号减一）：3=quota_metric、4=quota_id、
// 5=quota_dimensions（[[键, 值]...]，键的顺序不固定，写成 quota_dimension_<键>，如 quota_dimension_model、quota_dimension_location）、
// 6=quota_value（int64，字符串或数字都接受，写成与 ErrorInfo 同名的 quota_limit_value）。
// Gemini API 形状的 429 只在这里给出额度周期（如 GenerateRequestsPerDayPerProjectPerModel-FreeTier），
// 文案只是通用的 "You exceeded your current quota"；ErrorInfo 已给出的字段不覆盖。
// 常见的免费层 429 同时列出分钟和每日违规项且分钟在前：有每日违规项时取它，否则取第一个，
// 避免每日额度已耗尽的账户按分钟限额冷却后整天被反复选中。返回是否读出了违规项；解析失败的字段只跳过
func decodeRPCQuotaFailure(rpcError *RPCError, raw json.RawMessage) bool {
	var failure []json.RawMessage
	if json.Unmarshal(raw, &failure) != nil || len(failure) == 0 {
		return false
	}
	var chosen map[string]string
	for _, violation := range quotaViolations(failure[0]) {
		fields := quotaViolationFields(violation)
		if len(fields) == 0 {
			continue
		}
		daily := dailyQuotaEvidence(strings.ToLower(fields["quota_metric"] + " " + fields["quota_id"]))
		if chosen == nil || daily {
			chosen = fields
		}
		if daily {
			break
		}
	}
	for key, value := range chosen {
		if rpcError.Metadata == nil {
			rpcError.Metadata = make(map[string]string)
		}
		if rpcError.Metadata[key] == "" {
			rpcError.Metadata[key] = value
		}
	}
	return chosen != nil
}

// quotaViolations 读取 QuotaFailure 的违规项列表，不是数组的违规项跳过
func quotaViolations(raw json.RawMessage) [][]json.RawMessage {
	var items []json.RawMessage
	if json.Unmarshal(raw, &items) != nil {
		return nil
	}
	violations := make([][]json.RawMessage, 0, len(items))
	for _, item := range items {
		var violation []json.RawMessage
		if json.Unmarshal(item, &violation) == nil && len(violation) > 0 {
			violations = append(violations, violation)
		}
	}
	return violations
}

// quotaDimensionPrefix 为 QuotaFailure 违规项维度写进 RPCError.Metadata 的键前缀
const quotaDimensionPrefix = "quota_dimension_"

// quotaViolationFields 读取一个违规项的指标、限额名、维度与限额值；都没有时返回空
func quotaViolationFields(violation []json.RawMessage) map[string]string {
	fields := make(map[string]string, 4)
	for key, index := range map[string]int{"quota_metric": 3, "quota_id": 4} {
		var value string
		if len(violation) > index && json.Unmarshal(violation[index], &value) == nil && strings.TrimSpace(value) != "" {
			fields[key] = strings.TrimSpace(value)
		}
	}
	var dimensions []json.RawMessage
	if len(violation) > 5 && json.Unmarshal(violation[5], &dimensions) == nil {
		for _, item := range dimensions {
			var pair []json.RawMessage
			var key, value string
			if json.Unmarshal(item, &pair) != nil || len(pair) < 2 || json.Unmarshal(pair[0], &key) != nil || json.Unmarshal(pair[1], &value) != nil {
				continue
			}
			if key, value = strings.TrimSpace(key), strings.TrimSpace(value); key != "" && value != "" {
				fields[quotaDimensionPrefix+key] = value
			}
		}
	}
	if len(violation) > 6 {
		if value, ok := quotaValueText(violation[6]); ok {
			fields["quota_limit_value"] = value
		}
	}
	return fields
}

// quotaValueText 读取 quota_value（int64：数组协议里可能编码为字符串，也可能是数字），返回十进制文本
func quotaValueText(raw json.RawMessage) (string, bool) {
	var text string
	if json.Unmarshal(raw, &text) != nil {
		var number json.Number
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		if decoder.Decode(&number) != nil {
			return "", false
		}
		text = number.String()
	}
	value, err := strconv.ParseInt(strings.TrimSpace(text), 10, 64)
	if err != nil {
		return "", false
	}
	return strconv.FormatInt(value, 10), true
}
