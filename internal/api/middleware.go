package api

import (
	"bytes"
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
	"sync/atomic"
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
	pool            string
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
	// fallbackFrom 为额度用完改用其他模型（MODEL_FALLBACKS）前的模型，model 为改用后实际使用的模型；没有改用时为空
	fallbackFrom string
	// startedAt 为请求进入服务的时间；queueWait 为进入服务到取得最终执行账户的时间
	startedAt time.Time
	queueWait time.Duration
	// proof 为等待并生成 WAA proof 的累计时间
	proof time.Duration
	// attempts 为最终结果之前未成功的上游尝试
	attempts []RequestAttempt
	// authorized 表示请求已通过公开 API key 校验，只有这类请求写入用量账本
	authorized bool
	// warmup 表示 Anthropic max_tokens 为 0 的预热请求（见 MarkAccessLogWarmup）：不调用上游，请求日志照写，不写入用量账本
	warmup bool
	// lastUpstreamEvent 为上游最近一次事件的时刻（UnixNano，0 为还没有事件），由生成服务逐个事件写入（MarkAccessLogUpstreamEvent），
	// 客户端断开时读取，不加锁
	lastUpstreamEvent atomic.Int64
	// disconnect 为客户端断开那一刻的现场（见 requestLoggingMiddleware），没有断开时为 nil
	disconnect *accessDisconnect
}

// accessDisconnect 为请求被取消那一刻的现场，请求结束时转成 RequestDisconnect 写进请求日志
type accessDisconnect struct {
	at time.Time
	// client 为客户端断开了连接（请求的 context 被取消）；为假时是服务端取消（管理端取消请求、停止或重启服务）
	client      bool
	headersSent bool
	bytes       int64
	chunks      int64
	// lastWrite 为最后一次写给客户端的时刻（含心跳与响应头），没写过时为零值
	lastWrite time.Time
	// headerAt 为提交响应头的时刻，没提交时为零值；lastData 为最后一次写出中转会转发的数据（见 dataWrite）的时刻，没写过时为零值
	headerAt time.Time
	lastData time.Time
	// firstEvent 为断开前已收到上游首个事件；upstreamAt 为上游最近一次事件的时刻，没有事件时为零值
	firstEvent bool
	upstreamAt time.Time
	started    time.Time
}

type accessLogSnapshot struct {
	generation      bool
	model           string
	account         string
	channel         string
	finishReason    string
	replyHash       string
	servedModel     string
	fallbackFrom    string
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
	warmup          bool
	disconnect      *accessDisconnect
}

type accessLogResponseWriter struct {
	http.ResponseWriter
	status   int
	metadata *accessLogMetadata
	// stats 为写给客户端的统计：处理请求的 goroutine 写入，客户端断开时由 context.AfterFunc 的 goroutine 读取，所以用原子值
	stats accessWriteStats
}

// accessWriteStats 统计写给客户端的字节、块数与时刻（UnixNano）
type accessWriteStats struct {
	headerAt  atomic.Int64
	bytes     atomic.Int64
	chunks    atomic.Int64
	pending   atomic.Int64
	lastWrite atomic.Int64
	// lastData 为最后一次写出数据的时刻：只有空白（换行心跳）或只有 SSE 注释（": ping"）的写出不算，中转会丢掉它们
	lastData atomic.Int64
}

func (writer *accessLogResponseWriter) WriteHeader(status int) {
	if writer.status != 0 {
		return
	}
	writer.status = status
	now := time.Now().UnixNano()
	writer.stats.headerAt.Store(now)
	writer.stats.lastWrite.Store(now)
	writer.ResponseWriter.WriteHeader(status)
}

func (writer *accessLogResponseWriter) Write(data []byte) (int, error) {
	if writer.status == 0 {
		writer.WriteHeader(http.StatusOK)
	}
	written, err := writer.ResponseWriter.Write(data)
	if written > 0 {
		now := time.Now().UnixNano()
		writer.stats.bytes.Add(int64(written))
		writer.stats.pending.Add(int64(written))
		writer.stats.lastWrite.Store(now)
		if dataWrite(data[:written]) {
			writer.stats.lastData.Store(now)
		}
	}
	return written, err
}

// dataWrite 判断一次写出里有没有中转会转发的数据：new-api 等中转逐行读取上游 SSE，只转发 data 行，空行（Gemini 的换行心跳）与
// SSE 注释（OpenAI、Responses、Interactions 的 ": ping"）都被丢弃，传不到中转后面的客户端。有一行不是空白也不以 ":" 开头就算数据
// （SSE 的 data、event 行，非 SSE 响应体的内容）
func dataWrite(data []byte) bool {
	for len(data) > 0 {
		line := data
		if index := bytes.IndexByte(data, '\n'); index >= 0 {
			line, data = data[:index], data[index+1:]
		} else {
			data = nil
		}
		line = bytes.TrimSpace(line)
		if len(line) > 0 && line[0] != ':' {
			return true
		}
	}
	return false
}

// FlushError 记录状态并向响应控制器返回底层刷新错误；上次刷新后写过数据的每次刷新计为一块（SSE 每个事件、Gemini JSON 数组每个元素）
func (writer *accessLogResponseWriter) FlushError() error {
	if writer.status == 0 {
		writer.WriteHeader(http.StatusOK)
	}
	if writer.stats.pending.Swap(0) > 0 {
		writer.stats.chunks.Add(1)
	}
	return http.NewResponseController(writer.ResponseWriter).Flush()
}

// committed 判断响应头是否已经提交
func (writer *accessLogResponseWriter) committed() bool {
	return writer.status != 0
}

// disconnectSnapshot 读取此刻写给客户端的统计与上游最近一次事件，client 为是否客户端断开。可在任意 goroutine 调用
func (writer *accessLogResponseWriter) disconnectSnapshot(client bool) accessDisconnect {
	stats := &writer.stats
	chunks := stats.chunks.Load()
	if stats.pending.Load() > 0 {
		// 写了还没刷新的数据（非流式响应体）按一块计
		chunks++
	}
	snapshot := accessDisconnect{
		at: time.Now(), client: client, headersSent: stats.headerAt.Load() != 0,
		bytes: stats.bytes.Load(), chunks: chunks,
		lastWrite: unixNanoTime(stats.lastWrite.Load()), upstreamAt: unixNanoTime(writer.metadata.lastUpstreamEvent.Load()),
		headerAt: unixNanoTime(stats.headerAt.Load()), lastData: unixNanoTime(stats.lastData.Load()),
	}
	writer.metadata.mu.Lock()
	snapshot.firstEvent = writer.metadata.firstEvent > 0
	snapshot.started = writer.metadata.startedAt
	writer.metadata.mu.Unlock()
	return snapshot
}

func unixNanoTime(value int64) time.Time {
	if value == 0 {
		return time.Time{}
	}
	return time.Unix(0, value)
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
		Method: metadata.method, Path: metadata.path, Pool: metadata.pool, Model: metadata.model, Account: metadata.account, Channel: metadata.channel,
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

// setPanic 记下处理请求时恢复的 panic：请求按 500 失败记录
func (metadata *accessLogMetadata) setPanic(err error) {
	metadata.mu.Lock()
	metadata.err = panicSummary(err)
	metadata.canceled = false
	metadata.failureStatus = http.StatusInternalServerError
	metadata.mu.Unlock()
}

// markDisconnect 记下客户端断开那一刻的现场（只记第一次）
func (metadata *accessLogMetadata) markDisconnect(snapshot accessDisconnect) {
	metadata.mu.Lock()
	if metadata.disconnect == nil {
		metadata.disconnect = &snapshot
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

// SetAccessLogModelFallback 记下额度用完改用其他模型（MODEL_FALLBACKS，见 app/model_fallback.go）：请求日志与用量账本的模型改记 target，
// 请求日志另记改用前的 from。target 为空时只把模型恢复为 from、清除改用标记（改用之后仍按源模型的错误结束时用 RestoreAccessLogSource，
// 模型、改用标记与账户、通道在同一把锁内一起恢复）
func SetAccessLogModelFallback(ctx context.Context, from string, target string) {
	if metadata, ok := ctx.Value(accessLogContextKey{}).(*accessLogMetadata); ok {
		metadata.mu.Lock()
		if target == "" {
			metadata.model, metadata.fallbackFrom = strings.TrimPrefix(strings.TrimSpace(from), "models/"), ""
		} else {
			metadata.model, metadata.fallbackFrom = strings.TrimPrefix(strings.TrimSpace(target), "models/"), strings.TrimSpace(from)
		}
		metadata.mu.Unlock()
	}
}

// AccessLogTarget 返回请求日志当前记下的账户与通道（额度用完改用其他模型时记下源模型运行结束时的值，见 RestoreAccessLogSource）
func AccessLogTarget(ctx context.Context) (string, string) {
	if metadata, ok := ctx.Value(accessLogContextKey{}).(*accessLogMetadata); ok {
		metadata.mu.Lock()
		defer metadata.mu.Unlock()
		return metadata.account, metadata.channel
	}
	return "", ""
}

// RestoreAccessLogSource 把请求日志与用量账本的模型恢复为源模型 model、清除改用标记（fallback_from），账户与通道恢复为 account、channel
// （可以为空）：额度用完改用其他模型、所有目标也都不可用而按源模型的错误结束时，恢复为源模型运行结束时的值（源模型候选全部冷却、一次也没有
// 发出时为空），不记成目标模型最后用过的账户与通道。四项在同一把锁内写入：访问日志随时可能取快照（例如客户端断开 499 时立即取），
// 不会记下模型已是源模型、没有 fallback_from，账户与通道却还是目标模型的一半状态
func RestoreAccessLogSource(ctx context.Context, model string, account string, channel string) {
	if metadata, ok := ctx.Value(accessLogContextKey{}).(*accessLogMetadata); ok {
		metadata.mu.Lock()
		metadata.model, metadata.fallbackFrom = strings.TrimPrefix(strings.TrimSpace(model), "models/"), ""
		metadata.account, metadata.channel = strings.TrimSpace(account), strings.TrimSpace(channel)
		metadata.mu.Unlock()
	}
}

// TagAccessLogAttempts 给已记下、还没有写模型的上游尝试记上 model：额度用完改用其他模型时，第一次真正改用时（追加改用备注之前）把改用前
// 源模型的尝试记源模型，之后目标模型的尝试在记下时就写目标模型（见 RequestAttempt.Model）；目标都被跳过、没有改用时不调用
func TagAccessLogAttempts(ctx context.Context, model string) {
	model = strings.TrimPrefix(strings.TrimSpace(model), "models/")
	if model == "" {
		return
	}
	if metadata, ok := ctx.Value(accessLogContextKey{}).(*accessLogMetadata); ok {
		metadata.mu.Lock()
		for index := range metadata.attempts {
			if metadata.attempts[index].Model == "" {
				metadata.attempts[index].Model = model
			}
		}
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
		requestErr: metadata.err, canceled: metadata.canceled, fallbackFrom: metadata.fallbackFrom,
		failureStatus: metadata.failureStatus,
		firstEvent:    metadata.firstEvent,
		upstreamBytes: metadata.upstreamBytes, usage: metadata.usage, toolCalls: metadata.toolCalls,
		inputMessages: metadata.inputMessages, inputTextChars: metadata.inputTextChars,
		inputMedia: metadata.inputMedia, inputMediaBytes: metadata.inputMediaBytes, inputFiles: metadata.inputFiles,
		temperature: metadata.temperature, topP: metadata.topP, topK: metadata.topK,
		thinking: metadata.thinking, maxOutputTokens: metadata.maxOutputTokens, seed: metadata.seed, requestID: metadata.requestID,
		queueWait: metadata.queueWait, proof: metadata.proof,
		attempts: slices.Clone(metadata.attempts), authorized: metadata.authorized, warmup: metadata.warmup, disconnect: metadata.disconnect,
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

// isWarmup 判断请求是否已标记为预热请求（见 MarkAccessLogWarmup）
func (metadata *accessLogMetadata) isWarmup() bool {
	metadata.mu.Lock()
	defer metadata.mu.Unlock()
	return metadata.warmup
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

// MarkAccessLogWarmup 标记请求为 Anthropic max_tokens 为 0 的预热请求，并写入请求的模型（没有账户）：预热不调用上游、不占额度，
// 请求日志照常写一行（带模型，便于辨认），用量账本不记（与 token 计数请求一样，见 app 的 ledgerEntry），
// 不然用量页的请求数与成功数虚增、耗时分位被约 1 ms 的记录拉低，请求记录里还会多出没有账户的“请求完成”
func MarkAccessLogWarmup(ctx context.Context, model string) {
	if metadata, ok := ctx.Value(accessLogContextKey{}).(*accessLogMetadata); ok {
		metadata.mu.Lock()
		metadata.warmup = true
		metadata.mu.Unlock()
		metadata.setTarget(model, "")
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

// MarkAccessLogUpstreamEvent 记下上游最近一次事件的时刻（生成服务每转发一个上游事件调用一次）：客户端断开时据此写出断开前上游静默了多久
func MarkAccessLogUpstreamEvent(ctx context.Context) {
	if metadata, ok := ctx.Value(accessLogContextKey{}).(*accessLogMetadata); ok {
		metadata.lastUpstreamEvent.Store(time.Now().UnixNano())
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
		pool := ""
		if ultraRequest(r) {
			// Ultra 路由的请求在管理日志里显示带 /ultra 前缀的路径，并标记号池
			path = ultraPrefix + path
			pool = aistudio.PoolScopeUltra.String()
		}
		if aistudio.TraceFromContext(r.Context()) != nil {
			// 排查路由的请求在管理日志里显示带 /trace 前缀的路径（/trace/ultra/... 保持原顺序）
			path = tracePrefix + path
		}
		metadata := &accessLogMetadata{admin: admin, method: r.Method, path: path, pool: pool, requestID: newID("req"), startedAt: started}
		writer := &accessLogResponseWriter{ResponseWriter: w, metadata: metadata}
		// 每个请求另挂一份改用模型的记录：生成服务额度用完改用其他模型时写入，协议层据此在响应里报告实际使用的模型（见 servedModel）
		ctx := aistudio.ContextWithModelSwitch(context.WithValue(r.Context(), accessLogContextKey{}, metadata), aistudio.NewModelSwitch())
		request := r.WithContext(ctx)
		// 客户端断开（请求的 context 被取消）那一刻记下现场：是否已提交响应头、已写出多少、多久没写出、上游多久没有事件。
		// 处理函数返回后 net/http 也会取消 context，所以返回前先停止观察；观察已经开始时等它记完
		watched := make(chan struct{})
		stopWatch := context.AfterFunc(r.Context(), func() {
			defer close(watched)
			metadata.markDisconnect(writer.disconnectSnapshot(true))
		})
		aborted := serveRecovering(next, writer, request)
		// 停止观察之后只读一次 r.Context() 的结果，下面的补记与 499 判定都按它：处理函数正常返回之后才断开的不算取消
		watchStopped := stopWatch()
		requestErr := r.Context().Err()
		if !watchStopped {
			<-watched
		} else if requestErr != nil {
			// 观察还没被通知到就被停下：r.Context() 取消时先设好 Err、关闭 Done，再按 map 顺序逐个通知子 context，
			// 处理函数可能先看到取消并返回，赶在这个观察被通知之前。r.Context() 已经取消（本服务不设 BaseContext，处理函数
			// 返回之前只有客户端断开会取消它），同样按客户端断开记下现场，不当成服务端取消
			metadata.markDisconnect(writer.disconnectSnapshot(true))
		}
		status := writer.status
		if status == 0 {
			status = http.StatusOK
		}
		snapshot := metadata.snapshot()
		var disconnect *RequestDisconnect
		if snapshot.canceled || errors.Is(requestErr, context.Canceled) {
			status = 499
			// 客户端没有断开却以取消结束：管理端取消了请求，或服务正在停止、重启
			moment := snapshot.disconnect
			if moment == nil {
				service := writer.disconnectSnapshot(false)
				moment = &service
			}
			disconnect = moment.report()
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
				RequestID: snapshot.requestID, Pool: pool,
				Method: r.Method, Path: path, Model: snapshot.model, Account: snapshot.account, Channel: snapshot.channel,
				FinishReason: snapshot.finishReason, ReplyHash: snapshot.replyHash, Error: snapshot.requestErr,
				ServedModel: snapshot.servedModel, Downgrade: snapshot.downgrade, FallbackFrom: snapshot.fallbackFrom,
				Canceled: snapshot.canceled, Generation: snapshot.generation, Attempts: snapshot.attempts,
				Authorized: snapshot.authorized, Warmup: snapshot.warmup, Disconnect: disconnect,
			})
		}
		if aborted {
			// 处理函数主动中止连接（http.ErrAbortHandler），或响应头提交后 panic：请求日志写完后交给 net/http 中止连接（不再打印栈）
			panic(http.ErrAbortHandler)
		}
	})
}

// rightAfterHeadersWindow 为“写响应头后立即断开”的时限（见 RequestDisconnect.RightAfterHeaders）：中转发现它的客户端已走、关闭到本服务的连接
// 只要一个往返，1 秒足够
const rightAfterHeadersWindow = time.Second

// report 转成请求日志里的断开现场
func (moment *accessDisconnect) report() *RequestDisconnect {
	report := &RequestDisconnect{
		Initiator: "service", Phase: "before_first_event", HeadersSent: moment.headersSent,
		WrittenBytes: moment.bytes, WrittenChunks: moment.chunks,
		ElapsedMS: durationMS(moment.at.Sub(moment.started)),
	}
	if moment.client {
		report.Initiator = "client"
	}
	if moment.firstEvent {
		report.Phase = "mid_stream"
	}
	lastWrite := moment.lastWrite
	if lastWrite.IsZero() {
		lastWrite = moment.started
	}
	report.SinceLastWriteMS = durationMS(moment.at.Sub(lastWrite))
	if !moment.lastData.IsZero() {
		since := durationMS(moment.at.Sub(moment.lastData))
		report.SinceLastDataWriteMS = &since
	}
	if !moment.headerAt.IsZero() {
		since := moment.at.Sub(moment.headerAt)
		sinceMS := durationMS(since)
		report.SinceHeadersSentMS = &sinceMS
		report.RightAfterHeaders = moment.client && since < rightAfterHeadersWindow
	}
	if !moment.upstreamAt.IsZero() {
		since := durationMS(moment.at.Sub(moment.upstreamAt))
		report.SinceUpstreamEventMS = &since
	}
	return report
}

// durationMS 把时长换成毫秒（保留一位小数，负值按 0）
func durationMS(duration time.Duration) float64 {
	if duration < 0 {
		return 0
	}
	return float64(duration.Round(100*time.Microsecond)) / float64(time.Millisecond)
}

func setAccessLogResponseError(w http.ResponseWriter, message string) {
	if writer, ok := w.(interface{ setError(string) }); ok {
		writer.setError(message)
	}
}

func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-API-Key, X-Goog-API-Key, Api-Key, Anthropic-Version, Anthropic-Beta")
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
		if !requestAPIKeyMatches(r, key) {
			writeAuthError(w, r)
			return
		}
		// 只有真正校验过密钥的请求才标记：没有配置密钥（只在测试中出现）时不算通过校验，不写入用量账本
		markAccessLogAuthorized(r.Context())
		next.ServeHTTP(w, r)
	})
}

// apiKeyHeaders 为直接携带本服务 API key 的请求头（Authorization 另按 Bearer 读取）。排查记录按同一份列表隐藏请求头，
// 新增密钥来源时两处不会漏改：api-key 成为有效凭据后曾因排查记录未隐藏它而把密钥明文写进 logs/trace/
var apiKeyHeaders = []string{"X-Goog-API-Key", "X-API-Key", "Api-Key"}

// requestAPIKeyMatches 判断 ?key=、X-Goog-API-Key、X-API-Key、api-key 与 Authorization Bearer 中是否有任一与配置的密钥相同。
// 原先只看优先级最高的一个来源：SillyTavern 经 new-api 转发时 ?key= 里是下游的旧令牌、请求头里才是正确密钥，结果返回 401，
// 整个渠道被自动停用。每个来源都按常量时间比较
func requestAPIKeyMatches(r *http.Request, key string) bool {
	expected := []byte(key)
	matched := false
	candidates := make([]string, 0, len(apiKeyHeaders)+2)
	candidates = append(candidates, r.URL.Query().Get("key"))
	for _, name := range apiKeyHeaders {
		candidates = append(candidates, r.Header.Get(name))
	}
	candidates = append(candidates, bearerToken(r.Header.Get("Authorization")))
	for _, candidate := range candidates {
		candidate = strings.TrimSpace(candidate)
		if candidate != "" && subtle.ConstantTimeCompare([]byte(candidate), expected) == 1 {
			matched = true
		}
	}
	return matched
}

// bearerToken 返回 Authorization: Bearer 的凭据，其他认证方式返回空
func bearerToken(authorization string) string {
	scheme, token, ok := strings.Cut(strings.TrimSpace(authorization), " ")
	if ok && strings.EqualFold(scheme, "Bearer") {
		return strings.TrimSpace(token)
	}
	return ""
}
