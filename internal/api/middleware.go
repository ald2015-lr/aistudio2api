package api

import (
	"context"
	"crypto/subtle"
	"errors"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/config"
)

type accessLogContextKey struct{}

type accessLogMetadata struct {
	mu              sync.Mutex
	admin           AdminService
	method          string
	path            string
	started         bool
	generation      bool
	model           string
	account         string
	channel         string
	finishReason    string
	replyHash       string
	servedModel     string
	downgrade       *aistudio.DowngradeDecision
	err             string
	canceled        bool
	failureStatus   int
	firstEvent      time.Duration
	upstreamBytes   int64
	usage           *aistudio.Usage
	toolCalls       int
	inputMessages   int
	inputTextChars  int
	inputMedia      int
	inputMediaBytes int64
	inputFiles      int
	temperature     string
	topP            string
	topK            string
	thinking        string
	maxOutputTokens string
	seed            string
	requestID       string
	// startedAt 为请求进入服务的时间；queueWait 为进入服务到取得最终执行账户的时间
	startedAt time.Time
	queueWait time.Duration
	// proof 为等待并生成 WAA proof 的累计时间
	proof time.Duration
	// attempts 为最终结果之前未成功的上游尝试
	attempts []RequestAttempt
	// authorized 表示请求已通过公开 API key 校验，只有这类请求写入用量账本
	authorized bool
}

type accessLogSnapshot struct {
	generation      bool
	model           string
	account         string
	channel         string
	finishReason    string
	replyHash       string
	servedModel     string
	downgrade       *aistudio.DowngradeDecision
	requestErr      string
	canceled        bool
	failureStatus   int
	firstEvent      time.Duration
	upstreamBytes   int64
	usage           *aistudio.Usage
	toolCalls       int
	inputMessages   int
	inputTextChars  int
	inputMedia      int
	inputMediaBytes int64
	inputFiles      int
	temperature     string
	topP            string
	topK            string
	thinking        string
	maxOutputTokens string
	seed            string
	requestID       string
	queueWait       time.Duration
	proof           time.Duration
	attempts        []RequestAttempt
	authorized      bool
}

type accessLogResponseWriter struct {
	http.ResponseWriter
	status   int
	metadata *accessLogMetadata
}

func (writer *accessLogResponseWriter) WriteHeader(status int) {
	if writer.status != 0 {
		return
	}
	writer.status = status
	writer.ResponseWriter.WriteHeader(status)
}

func (writer *accessLogResponseWriter) Write(data []byte) (int, error) {
	if writer.status == 0 {
		writer.WriteHeader(http.StatusOK)
	}
	return writer.ResponseWriter.Write(data)
}

// FlushError 记录状态并向响应控制器返回底层刷新错误
func (writer *accessLogResponseWriter) FlushError() error {
	if writer.status == 0 {
		writer.WriteHeader(http.StatusOK)
	}
	return http.NewResponseController(writer.ResponseWriter).Flush()
}

func (writer *accessLogResponseWriter) Unwrap() http.ResponseWriter {
	return writer.ResponseWriter
}

func (writer *accessLogResponseWriter) setError(message string) {
	writer.metadata.setError(message)
}

func (metadata *accessLogMetadata) setTarget(model string, account string) {
	metadata.mu.Lock()
	if model = strings.TrimSpace(model); model != "" {
		metadata.model = strings.TrimPrefix(model, "models/")
	}
	if account = strings.TrimSpace(account); account != "" {
		metadata.account = account
	}
	metadata.mu.Unlock()
	metadata.start(false)
}

func (metadata *accessLogMetadata) start(force bool) {
	metadata.mu.Lock()
	if metadata.started || !force && metadata.account == "" {
		metadata.mu.Unlock()
		return
	}
	metadata.started = true
	admin := metadata.admin
	entry := AccessLog{
		Method: metadata.method, Path: metadata.path, Model: metadata.model, Account: metadata.account, Channel: metadata.channel,
		Temperature: metadata.temperature, TopP: metadata.topP, TopK: metadata.topK, Thinking: metadata.thinking,
		MaxOutputTokens: metadata.maxOutputTokens, Seed: metadata.seed, Generation: metadata.generation, RequestID: metadata.requestID,
		InputMessages: metadata.inputMessages, InputTextChars: metadata.inputTextChars,
		InputMedia: metadata.inputMedia, InputMediaBytes: metadata.inputMediaBytes, InputFiles: metadata.inputFiles,
	}
	metadata.mu.Unlock()
	if admin != nil {
		admin.RecordAccessStart(entry)
	}
}

func (metadata *accessLogMetadata) setError(message string) {
	message = strings.TrimSpace(message)
	if message == "" {
		return
	}
	metadata.mu.Lock()
	metadata.err = message
	metadata.mu.Unlock()
}

// setErrorIfEmpty 只在还没有记录错误时写入：运行时写入的完整错误链优先
func (metadata *accessLogMetadata) setErrorIfEmpty(message string) {
	message = strings.TrimSpace(message)
	if message == "" {
		return
	}
	metadata.mu.Lock()
	if metadata.err == "" {
		metadata.err = message
	}
	metadata.mu.Unlock()
}

func (metadata *accessLogMetadata) setRequestError(err error) {
	if err == nil {
		return
	}
	metadata.mu.Lock()
	metadata.err = strings.TrimSpace(err.Error())
	metadata.canceled = errors.Is(err, context.Canceled)
	if metadata.canceled {
		metadata.failureStatus = 499
	} else {
		// 与返回给客户端的状态码一致（号池内部的 401/403 等已按官方格式改为 503；指明模型时没有账户支持按 404）
		metadata.failureStatus = publicErrorFor(err, metadata.model).Status
	}
	metadata.mu.Unlock()
}

func (metadata *accessLogMetadata) setFinishReason(reason string) {
	metadata.mu.Lock()
	metadata.finishReason = strings.TrimSpace(reason)
	metadata.mu.Unlock()
}

// SetAccessLogServedModel 写入上游标明的实际服务模型（只在与请求的模型系列不同时写）
func SetAccessLogServedModel(ctx context.Context, model string) {
	if metadata, ok := ctx.Value(accessLogContextKey{}).(*accessLogMetadata); ok {
		metadata.mu.Lock()
		metadata.servedModel = strings.TrimSpace(model)
		metadata.mu.Unlock()
	}
}

// SetAccessLogDowngrade 写入降级判定的依据（只有被拦截的模型才有；同一请求多次写入时保留最后一次）
func SetAccessLogDowngrade(ctx context.Context, decision aistudio.DowngradeDecision) {
	if metadata, ok := ctx.Value(accessLogContextKey{}).(*accessLogMetadata); ok {
		metadata.mu.Lock()
		metadata.downgrade = &decision
		metadata.mu.Unlock()
	}
}

// SetAccessLogReplyHash 写入回复正文的指纹（只用于判断两次回复是否相同，不保存内容）
func SetAccessLogReplyHash(ctx context.Context, hash string) {
	if metadata, ok := ctx.Value(accessLogContextKey{}).(*accessLogMetadata); ok {
		metadata.mu.Lock()
		metadata.replyHash = hash
		metadata.mu.Unlock()
	}
}

func (metadata *accessLogMetadata) setGenerationResult(
	usage *aistudio.Usage,
	toolCalls int,
) {
	metadata.mu.Lock()
	if usage != nil {
		value := *usage
		metadata.usage = &value
	}
	metadata.toolCalls = toolCalls
	metadata.mu.Unlock()
}

func (metadata *accessLogMetadata) setGenerationConfig(config aistudio.GenerationConfig) {
	metadata.mu.Lock()
	metadata.generation = true
	metadata.temperature = formatLogFloat(config.Temperature)
	metadata.topP = formatLogFloat(config.TopP)
	metadata.topK = formatLogTopK(config.TopK)
	metadata.thinking = formatLogThinking(config)
	metadata.maxOutputTokens = formatLogInt(config.MaxOutputTokens)
	metadata.seed = formatLogInt(config.Seed)
	metadata.mu.Unlock()
}

// MarkAccessLogMaxOutputRaised 记下最大输出被提高到下限（实际值不超过模型上限）
func MarkAccessLogMaxOutputRaised(ctx context.Context, floor int64) {
	if metadata, ok := ctx.Value(accessLogContextKey{}).(*accessLogMetadata); ok {
		metadata.mu.Lock()
		metadata.maxOutputTokens += "，已提高到 " + strconv.FormatInt(floor, 10)
		metadata.mu.Unlock()
	}
}

// MarkAccessLogSeedNote 在 seed 记录后追加说明（如"重复提示词已加随机后缀"）
func MarkAccessLogSeedNote(ctx context.Context, note string) {
	if metadata, ok := ctx.Value(accessLogContextKey{}).(*accessLogMetadata); ok {
		metadata.mu.Lock()
		if metadata.seed == "" {
			metadata.seed = note
		} else {
			metadata.seed += "，" + note
		}
		metadata.mu.Unlock()
	}
}

// MarkAccessLogSeedRandom 标记本次使用了服务生成的随机 seed（客户端未传，或已按配置忽略）
func MarkAccessLogSeedRandom(ctx context.Context, seed int64) {
	if metadata, ok := ctx.Value(accessLogContextKey{}).(*accessLogMetadata); ok {
		// 记下实际发送的 seed 值，排查重复回复时可以确认两次请求的 seed 是否真的不同
		value := "随机 " + strconv.FormatInt(seed, 10)
		metadata.mu.Lock()
		if metadata.seed == "" || metadata.seed == "默认" {
			metadata.seed = value
		} else {
			metadata.seed += "，改用" + value
		}
		metadata.mu.Unlock()
	}
}

// MarkAccessLogSeedIgnored 标记客户端传入的 seed 已按配置忽略
func MarkAccessLogSeedIgnored(ctx context.Context) {
	if metadata, ok := ctx.Value(accessLogContextKey{}).(*accessLogMetadata); ok {
		metadata.mu.Lock()
		metadata.seed += "（已忽略）"
		metadata.mu.Unlock()
	}
}

func (metadata *accessLogMetadata) setGenerationInput(request aistudio.GenerateRequest) {
	textChars := utf8.RuneCountInString(request.System)
	media := 0
	var mediaBytes int64
	files := 0
	for _, content := range request.Contents {
		for _, part := range content.Parts {
			textChars += utf8.RuneCountInString(part.Text)
			if part.InlineData != nil {
				media++
				mediaBytes += int64(len(part.InlineData.Data))
			}
			if part.ExternalMedia != nil {
				media++
			}
			if part.File != nil {
				files++
			}
		}
	}
	metadata.mu.Lock()
	// 生成请求没有 ID 时保留请求日志生成的 ID：账本按 ID 去重，空 ID 的记录会互相覆盖
	if id := strings.TrimSpace(request.ID); id != "" {
		metadata.requestID = id
	}
	metadata.inputMessages = len(request.Contents)
	metadata.inputTextChars = textChars
	metadata.inputMedia = media
	metadata.inputMediaBytes = mediaBytes
	metadata.inputFiles = files
	metadata.mu.Unlock()
}

func (metadata *accessLogMetadata) setUpstreamBytes(bytes int64) {
	metadata.mu.Lock()
	metadata.upstreamBytes = bytes
	metadata.mu.Unlock()
}

func (metadata *accessLogMetadata) setFirstEvent(firstEvent time.Duration) {
	metadata.mu.Lock()
	if metadata.firstEvent == 0 {
		metadata.firstEvent = firstEvent
	}
	metadata.mu.Unlock()
}

func (metadata *accessLogMetadata) snapshot() accessLogSnapshot {
	metadata.mu.Lock()
	snapshot := accessLogSnapshot{
		generation: metadata.generation,
		model:      metadata.model, account: metadata.account, channel: metadata.channel, finishReason: metadata.finishReason, replyHash: metadata.replyHash, servedModel: metadata.servedModel, downgrade: metadata.downgrade,
		requestErr: metadata.err, canceled: metadata.canceled,
		failureStatus: metadata.failureStatus,
		firstEvent:    metadata.firstEvent,
		upstreamBytes: metadata.upstreamBytes, usage: metadata.usage, toolCalls: metadata.toolCalls,
		inputMessages: metadata.inputMessages, inputTextChars: metadata.inputTextChars,
		inputMedia: metadata.inputMedia, inputMediaBytes: metadata.inputMediaBytes, inputFiles: metadata.inputFiles,
		temperature: metadata.temperature, topP: metadata.topP, topK: metadata.topK,
		thinking: metadata.thinking, maxOutputTokens: metadata.maxOutputTokens, seed: metadata.seed, requestID: metadata.requestID,
		queueWait: metadata.queueWait, proof: metadata.proof,
		attempts: slices.Clone(metadata.attempts), authorized: metadata.authorized,
	}
	metadata.mu.Unlock()
	return snapshot
}

// id 返回请求日志使用的请求 ID
func (metadata *accessLogMetadata) id() string {
	metadata.mu.Lock()
	defer metadata.mu.Unlock()
	return metadata.requestID
}

// MarkAccessLogScheduled 记录请求从进入服务到取得本次执行账户的排队时间（换号重试时以最后一次为准）
func MarkAccessLogScheduled(ctx context.Context) {
	if metadata, ok := ctx.Value(accessLogContextKey{}).(*accessLogMetadata); ok {
		metadata.mu.Lock()
		if !metadata.startedAt.IsZero() {
			metadata.queueWait = time.Since(metadata.startedAt)
		}
		metadata.mu.Unlock()
	}
}

// markAccessLogAuthorized 标记请求已通过公开 API key 校验
func markAccessLogAuthorized(ctx context.Context) {
	if metadata, ok := ctx.Value(accessLogContextKey{}).(*accessLogMetadata); ok {
		metadata.mu.Lock()
		metadata.authorized = true
		metadata.mu.Unlock()
	}
}

// AddAccessLogAttempt 追加一次未成功的上游尝试
func AddAccessLogAttempt(ctx context.Context, attempt RequestAttempt) {
	if metadata, ok := ctx.Value(accessLogContextKey{}).(*accessLogMetadata); ok {
		metadata.mu.Lock()
		metadata.attempts = append(metadata.attempts, attempt)
		metadata.mu.Unlock()
	}
}

// AddAccessLogProof 累计请求等待并生成 WAA proof 的时间
func AddAccessLogProof(ctx context.Context, duration time.Duration) {
	if metadata, ok := ctx.Value(accessLogContextKey{}).(*accessLogMetadata); ok {
		metadata.mu.Lock()
		metadata.proof += duration
		metadata.mu.Unlock()
	}
}

// SetAccessLogFirstEvent 写入首个上游语义事件耗时
func SetAccessLogFirstEvent(ctx context.Context, firstEvent time.Duration) {
	if metadata, ok := ctx.Value(accessLogContextKey{}).(*accessLogMetadata); ok {
		metadata.setFirstEvent(firstEvent)
	}
}

// SetAccessLogGenerationConfig 写入生成请求采用的参数
func SetAccessLogGenerationConfig(ctx context.Context, config aistudio.GenerationConfig) {
	if metadata, ok := ctx.Value(accessLogContextKey{}).(*accessLogMetadata); ok {
		metadata.setGenerationConfig(config)
	}
}

// SetAccessLogGenerationInput 写入生成请求输入摘要
func SetAccessLogGenerationInput(ctx context.Context, request aistudio.GenerateRequest) {
	if metadata, ok := ctx.Value(accessLogContextKey{}).(*accessLogMetadata); ok {
		metadata.setGenerationInput(request)
	}
}

// SetAccessLogUpstreamBytes 写入上游响应体字节数
func SetAccessLogUpstreamBytes(ctx context.Context, bytes int64) {
	if metadata, ok := ctx.Value(accessLogContextKey{}).(*accessLogMetadata); ok {
		metadata.setUpstreamBytes(bytes)
	}
}

// StartAccessLog 立即写入已经完成解析的请求开始记录
func StartAccessLog(ctx context.Context) {
	if metadata, ok := ctx.Value(accessLogContextKey{}).(*accessLogMetadata); ok {
		metadata.start(true)
	}
}

func formatLogFloat(value *float64) string {
	if value == nil {
		return "默认"
	}
	return strconv.FormatFloat(*value, 'f', -1, 64)
}

func formatLogInt(value *int64) string {
	if value == nil {
		return "默认"
	}
	return strconv.FormatInt(*value, 10)
}

func formatLogTopK(value *int) string {
	if value == nil {
		return "默认"
	}
	return strconv.Itoa(*value)
}

func formatLogThinking(config aistudio.GenerationConfig) string {
	if effort := strings.TrimSpace(config.ReasoningEffort); effort != "" {
		return effort
	}
	if config.ThinkingBudget != nil {
		return "预算" + strconv.FormatInt(*config.ThinkingBudget, 10)
	}
	return "默认"
}

// SetAccessLogTarget 写入请求实际使用的模型与账户
func SetAccessLogTarget(ctx context.Context, model string, account string) {
	if metadata, ok := ctx.Value(accessLogContextKey{}).(*accessLogMetadata); ok {
		metadata.setTarget(model, account)
	}
}

// SetAccessLogChannel 写入请求实际使用的上游通道
func SetAccessLogChannel(ctx context.Context, channel string) {
	if metadata, ok := ctx.Value(accessLogContextKey{}).(*accessLogMetadata); ok {
		metadata.mu.Lock()
		metadata.channel = strings.TrimSpace(channel)
		metadata.mu.Unlock()
	}
}

// SetAccessLogError 写入请求最终错误
func SetAccessLogError(ctx context.Context, err error) {
	if err == nil {
		return
	}
	if metadata, ok := ctx.Value(accessLogContextKey{}).(*accessLogMetadata); ok {
		metadata.setRequestError(err)
	}
}

// SetAccessLogFinishReason 写入生成请求的上游终止原因
func SetAccessLogFinishReason(ctx context.Context, reason string) {
	if metadata, ok := ctx.Value(accessLogContextKey{}).(*accessLogMetadata); ok {
		metadata.setFinishReason(reason)
	}
}

// SetAccessLogGenerationResult 写入生成流的完成摘要
func SetAccessLogGenerationResult(
	ctx context.Context,
	usage *aistudio.Usage,
	toolCalls int,
) {
	if metadata, ok := ctx.Value(accessLogContextKey{}).(*accessLogMetadata); ok {
		metadata.setGenerationResult(usage, toolCalls)
	}
}

func requestLoggingMiddleware(admin AdminService, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		path := r.URL.Path
		if aistudio.TraceFromContext(r.Context()) != nil {
			// 排查路由的请求在管理日志里显示带 /trace 前缀的路径
			path = tracePrefix + path
		}
		metadata := &accessLogMetadata{admin: admin, method: r.Method, path: path, requestID: newID("req"), startedAt: started}
		writer := &accessLogResponseWriter{ResponseWriter: w, metadata: metadata}
		request := r.WithContext(context.WithValue(r.Context(), accessLogContextKey{}, metadata))
		next.ServeHTTP(writer, request)
		status := writer.status
		if status == 0 {
			status = http.StatusOK
		}
		snapshot := metadata.snapshot()
		if snapshot.canceled || errors.Is(r.Context().Err(), context.Canceled) {
			status = 499
		} else if status < http.StatusBadRequest && snapshot.failureStatus >= http.StatusBadRequest {
			status = snapshot.failureStatus
		}
		if trace := aistudio.TraceFromContext(r.Context()); trace != nil {
			// 排查记录里保留管理日志的请求结果与内部详细错误（客户端只收到官方措辞）
			trace.SetOutcome(aistudio.TraceOutcome{
				Status: status, LatencyMS: float64(time.Since(started)) / float64(time.Millisecond),
				Model: snapshot.model, Account: snapshot.account, Channel: snapshot.channel,
				FinishReason: snapshot.finishReason, Seed: snapshot.seed, Error: snapshot.requestErr, Canceled: snapshot.canceled,
				ServedModel: snapshot.servedModel, Downgrade: snapshot.downgrade,
			})
		}
		if admin != nil {
			admin.RecordAccessLog(AccessLog{
				Status: status, Latency: time.Since(started), FirstEvent: snapshot.firstEvent,
				QueueWait: snapshot.queueWait, Proof: snapshot.proof,
				UpstreamBytes: snapshot.upstreamBytes, Usage: snapshot.usage, ToolCalls: snapshot.toolCalls,
				InputMessages: snapshot.inputMessages, InputTextChars: snapshot.inputTextChars,
				InputMedia: snapshot.inputMedia, InputMediaBytes: snapshot.inputMediaBytes, InputFiles: snapshot.inputFiles,
				Temperature: snapshot.temperature, TopP: snapshot.topP, TopK: snapshot.topK,
				Thinking: snapshot.thinking, MaxOutputTokens: snapshot.maxOutputTokens, Seed: snapshot.seed,
				RequestID: snapshot.requestID,
				Method:    r.Method, Path: path, Model: snapshot.model, Account: snapshot.account, Channel: snapshot.channel,
				FinishReason: snapshot.finishReason, ReplyHash: snapshot.replyHash, Error: snapshot.requestErr,
				ServedModel: snapshot.servedModel, Downgrade: snapshot.downgrade,
				Canceled: snapshot.canceled, Generation: snapshot.generation, Attempts: snapshot.attempts,
				Authorized: snapshot.authorized,
			})
		}
	})
}

func setAccessLogResponseError(w http.ResponseWriter, message string) {
	if writer, ok := w.(interface{ setError(string) }); ok {
		writer.setError(message)
	}
}

func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-API-Key, X-Goog-API-Key, Anthropic-Version, Anthropic-Beta")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// loopbackHost 判断 Host 或 Origin 主机名是否为 localhost 或回环地址
func loopbackHost(host string) bool {
	name := host
	if hostname, _, err := net.SplitHostPort(host); err == nil {
		name = hostname
	}
	name = strings.Trim(name, "[]")
	if strings.EqualFold(name, "localhost") {
		return true
	}
	ip := net.ParseIP(name)
	return ip != nil && ip.IsLoopback()
}

// browserOriginMiddleware 在没有配置自定义 API key 时拒绝外部网页与 null 来源的浏览器请求。
// 默认密钥随源码公开，等同于没有密钥：任何网页都能在用户浏览器里带着它调用本机接口，因此仍按来源拦截
func browserOriginMiddleware(requiredKey func() string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if key := requiredKey(); key != "" && key != config.DefaultProxyAPIKey {
			next.ServeHTTP(w, r)
			return
		}
		originValue := strings.TrimSpace(r.Header.Get("Origin"))
		if originValue == "" {
			next.ServeHTTP(w, r)
			return
		}
		origin, err := url.Parse(originValue)
		webOrigin := err != nil || originValue == "null" || origin.Scheme == "http" || origin.Scheme == "https"
		if webOrigin && (err != nil || !loopbackHost(origin.Host)) {
			writeAuthError(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// maxPublicBodyBytes 为公开接口请求体上限，可容纳 Base64 编码后的最大文件
const maxPublicBodyBytes = openAIFileMaxBytes/3*4 + openAIFileRequestOverhead

// bodyLimitMiddleware 限制公开接口请求体大小
func bodyLimitMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, maxPublicBodyBytes)
		next.ServeHTTP(w, r)
	})
}

// sameOriginMiddleware 拒绝来自其他网站的浏览器请求：浏览器标明跨站（Sec-Fetch-Site: cross-site）、
// Origin 与请求主机不同、Origin 不是 http/https 或带用户信息时返回 403。非浏览器客户端（不带这些头）照常放行
func sameOriginMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.EqualFold(strings.TrimSpace(r.Header.Get("Sec-Fetch-Site")), "cross-site") {
			writeAdminError(w, http.StatusForbidden, "control_plane_origin_forbidden", "Control plane requires a same-origin browser request")
			return
		}
		originValue := strings.TrimSpace(r.Header.Get("Origin"))
		if originValue == "" {
			next.ServeHTTP(w, r)
			return
		}
		origin, err := url.Parse(originValue)
		if err != nil || origin.Host == "" || origin.User != nil ||
			(origin.Scheme != "http" && origin.Scheme != "https") || !strings.EqualFold(origin.Host, r.Host) {
			writeAdminError(w, http.StatusForbidden, "control_plane_origin_forbidden", "Control plane requires a same-origin browser request")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// maxControlBodyBytes 为管理接口请求体上限：导入账户的 storage-state 通常几十 KB，留足余量
const maxControlBodyBytes = 8 << 20

// controlPlaneMiddleware 为管理接口限制请求体大小，并禁止缓存响应（响应可能含账户与配置信息）
func controlPlaneMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, maxControlBodyBytes)
		}
		next.ServeHTTP(w, r)
	})
}

func authMiddleware(requiredKey func() string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := requiredKey()
		if key == "" {
			next.ServeHTTP(w, r)
			return
		}
		provided := requestAPIKey(r)
		if subtle.ConstantTimeCompare([]byte(provided), []byte(key)) != 1 {
			writeAuthError(w, r)
			return
		}
		// 只有真正校验过密钥的请求才标记：没有配置密钥（只在测试中出现）时不算通过校验，不写入用量账本
		markAccessLogAuthorized(r.Context())
		next.ServeHTTP(w, r)
	})
}

func requestAPIKey(r *http.Request) string {
	if key := strings.TrimSpace(r.URL.Query().Get("key")); key != "" {
		return key
	}
	if key := strings.TrimSpace(r.Header.Get("X-Goog-API-Key")); key != "" {
		return key
	}
	if key := strings.TrimSpace(r.Header.Get("X-API-Key")); key != "" {
		return key
	}
	authorization := strings.TrimSpace(r.Header.Get("Authorization"))
	scheme, key, ok := strings.Cut(authorization, " ")
	if ok && strings.EqualFold(scheme, "Bearer") {
		return strings.TrimSpace(key)
	}
	return ""
}
