package aistudio

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"io"
	"strings"
	"sync"
	"time"
)

// Trace 记录一次经 /trace/ 排查路由发出的请求在各环节的完整细节：客户端原始请求、协议转换后的参数、
// 真正发给 AI Studio 的请求体、每次尝试的账号与结果、上游原始响应的开头、完整回复与返回给客户端的内容。
// 只有排查路由会创建 Trace；主路由的上下文里没有它，下面所有方法对 nil 接收者都是空操作，不产生额外开销
type Trace struct {
	mu           sync.Mutex
	active       bool
	finished     bool
	data         traceData
	replyText    strings.Builder
	replyThought strings.Builder
	replyDigest  hash.Hash
	onFinish     []func()
}

// 各部分的记录上限，防止异常大的请求把排查文件撑爆
const (
	traceBodyLimit     = 4 << 20
	traceUpstreamLimit = 256 << 10
	traceReplyLimit    = 1 << 20
	traceThoughtLimit  = 256 << 10
	// traceInlineLimit 以上的 base64 字符串（图片等媒体）在排查记录里只保留长度、指纹与开头
	traceInlineLimit = 4096
)

type traceData struct {
	StartedAt  time.Time         `json:"started_at"`
	FinishedAt time.Time         `json:"finished_at"`
	RequestID  string            `json:"request_id,omitempty"`
	// Summary、Flags、Findings 为记录结束时自动判断的结论，放在最前面
	Summary    string            `json:"summary"`
	Flags      []string          `json:"flags"`
	Findings   []TraceFinding    `json:"findings"`
	Client     *TraceClient      `json:"client,omitempty"`
	Parameters []TraceParameters `json:"parameters,omitempty"`
	Prompt     *TracePrompt      `json:"prompt,omitempty"`
	Attempts   []*TraceAttempt   `json:"attempts,omitempty"`
	Reply      TraceReply        `json:"reply"`
	Response   *TraceResponse    `json:"response,omitempty"`
	Outcome    *TraceOutcome     `json:"outcome,omitempty"`
	Notes      []string          `json:"notes,omitempty"`
	// Timeline 为该请求的进度记录（等待账号、换号原因、首个事件、重复回复等），与管理日志一致
	Timeline   []TraceTimelineEntry `json:"timeline,omitempty"`
}

// TraceClient 为客户端发来的原始请求（鉴权头已隐藏）
type TraceClient struct {
	Method     string            `json:"method"`
	Path       string            `json:"path"`
	Query      string            `json:"query,omitempty"`
	RemoteAddr string            `json:"remote_addr,omitempty"`
	Headers    map[string]string `json:"headers,omitempty"`
	Body       TracePayload      `json:"body"`
}

// TracePayload 保存一段请求体：合法 JSON 原样嵌入，否则按文本保存；超过上限只保留开头
type TracePayload struct {
	Bytes     int             `json:"bytes"`
	SHA256    string          `json:"sha256"`
	Truncated bool            `json:"truncated,omitempty"`
	Compacted bool            `json:"media_compacted,omitempty"`
	JSON      json.RawMessage `json:"json,omitempty"`
	Text      string          `json:"text,omitempty"`
}

// TraceParameters 为某一阶段的生成参数快照
type TraceParameters struct {
	Stage       string          `json:"stage"`
	Model       string          `json:"model"`
	Config      json.RawMessage `json:"config,omitempty"`
	SystemBytes int             `json:"system_bytes"`
	Messages    int             `json:"messages"`
}

// TracePrompt 为发给上游的对话摘要：整体指纹与每条消息的角色、字节数、指纹
type TracePrompt struct {
	Fingerprint string               `json:"fingerprint"`
	SystemBytes int                  `json:"system_bytes"`
	SystemHash  string               `json:"system_hash,omitempty"`
	Messages    []TracePromptMessage `json:"messages"`
}

// TracePromptMessage 为一条消息的摘要
type TracePromptMessage struct {
	Role  string `json:"role"`
	Parts int    `json:"parts"`
	Bytes int    `json:"text_bytes"`
	Media int    `json:"media,omitempty"`
	Hash  string `json:"text_hash"`
}

// TraceAttempt 为一次账号尝试：发出的请求体、上游响应开头与结果
type TraceAttempt struct {
	Index         int         `json:"index"`
	StartedAt     time.Time   `json:"started_at"`
	ElapsedMS     float64     `json:"elapsed_ms,omitempty"`
	Account       string      `json:"account,omitempty"`
	Channel       string      `json:"channel,omitempty"`
	Requests      []TraceWire `json:"requests,omitempty"`
	UpstreamBytes int64       `json:"upstream_bytes,omitempty"`
	UpstreamHead  string      `json:"upstream_head,omitempty"`
	Result        string      `json:"result,omitempty"`
	Error         string      `json:"error,omitempty"`
	head          []byte
	done          bool
}

// TraceWire 为真正发给 AI Studio 的一次请求
type TraceWire struct {
	Method string       `json:"method"`
	URL    string       `json:"url"`
	Body   TracePayload `json:"body"`
}

// TraceReply 为上游返回的完整回复
type TraceReply struct {
	ProviderModel string `json:"provider_model,omitempty"`
	// ModelVersions 为上游依次标明的模型（去掉相邻重复）：Build 通道最后一块报告实际服务的模型
	ModelVersions []string `json:"model_versions,omitempty"`
	Events        int    `json:"events"`
	TextBytes     int    `json:"text_bytes"`
	TextHash      string `json:"text_hash,omitempty"`
	Text          string `json:"text,omitempty"`
	ThoughtBytes  int    `json:"thought_bytes"`
	Thought       string `json:"thought,omitempty"`
	FinishReason  string `json:"finish_reason,omitempty"`
	Usage         *Usage `json:"usage,omitempty"`
	ToolCalls     int    `json:"tool_calls,omitempty"`
	Error         string `json:"error,omitempty"`
	// 各事件相对请求开始的时间（毫秒）：区分慢在上游生成，还是生成结束后迟迟没有收尾
	FirstEventMS  float64 `json:"first_event_ms,omitempty"`
	LastEventMS   float64 `json:"last_event_ms,omitempty"`
	FinishEventMS float64 `json:"finish_event_ms,omitempty"`
	// MaxGapMS 为相邻两个上游事件之间最长的间隔，MaxGapAtMS 为这次停顿开始的时间：判断输出中途卡住
	MaxGapMS   float64 `json:"max_gap_ms,omitempty"`
	MaxGapAtMS float64 `json:"max_gap_at_ms,omitempty"`
}

// TraceResponse 为返回给客户端的响应
type TraceResponse struct {
	Status      int    `json:"status"`
	ContentType string `json:"content_type,omitempty"`
	Bytes       int64  `json:"bytes"`
	Truncated   bool   `json:"truncated,omitempty"`
	Head        string `json:"head,omitempty"`
}

type traceContextKey struct{}

// NewTrace 创建一条排查记录
func NewTrace() *Trace {
	return &Trace{data: traceData{StartedAt: time.Now()}, replyDigest: sha256.New()}
}

// ContextWithTrace 把排查记录放入请求上下文
func ContextWithTrace(ctx context.Context, trace *Trace) context.Context {
	if trace == nil {
		return ctx
	}
	return context.WithValue(ctx, traceContextKey{}, trace)
}

// TraceFromContext 返回上下文中的排查记录；主路由请求返回 nil
func TraceFromContext(ctx context.Context) *Trace {
	if ctx == nil {
		return nil
	}
	trace, _ := ctx.Value(traceContextKey{}).(*Trace)
	return trace
}

// NewTracePayload 按记录上限整理请求体
func NewTracePayload(body []byte) TracePayload {
	sum := sha256.Sum256(body)
	payload := TracePayload{Bytes: len(body), SHA256: hex.EncodeToString(sum[:])}
	if json.Valid(body) {
		// 图片等媒体的 base64 只保留长度、指纹与开头，避免两三张图片就把请求体撑过记录上限
		if compacted, ok := compactTraceJSON(body, 0); ok {
			body = compacted
			payload.Compacted = true
		}
		if len(body) <= traceBodyLimit {
			payload.JSON = append(json.RawMessage(nil), body...)
			return payload
		}
	}
	if len(body) > traceBodyLimit {
		body = body[:traceBodyLimit]
		payload.Truncated = true
	}
	payload.Text = string(body)
	return payload
}

// Activate 标记请求已通过鉴权，结束时需要写出排查文件
func (t *Trace) Activate() {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.active = true
	t.mu.Unlock()
}

// Active 返回是否需要写出排查文件
func (t *Trace) Active() bool {
	if t == nil {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.active
}

// SetClient 保存客户端原始请求
func (t *Trace) SetClient(client TraceClient) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.finished {
		t.data.Client = &client
	}
}

// SetRequestID 保存生成请求 ID（与管理日志中的请求 ID 相同）
func (t *Trace) SetRequestID(id string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.finished {
		t.data.RequestID = strings.TrimSpace(id)
	}
}

// RequestID 返回生成请求 ID
func (t *Trace) RequestID() string {
	if t == nil {
		return ""
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.data.RequestID
}

// AddParameters 保存某一阶段的生成参数快照（立即序列化，之后请求被修改不影响记录）
func (t *Trace) AddParameters(stage string, request GenerateRequest) {
	if t == nil {
		return
	}
	entry := TraceParameters{Stage: stage, Model: request.Model, SystemBytes: len(request.System), Messages: len(request.Contents)}
	if encoded, err := json.Marshal(request.Config); err == nil {
		entry.Config = encoded
	} else {
		entry.Config = nil
		t.Note("生成参数无法序列化: " + err.Error())
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.finished {
		t.data.Parameters = append(t.data.Parameters, entry)
	}
}

// SetPrompt 保存发给上游的对话摘要
func (t *Trace) SetPrompt(fingerprint string, request GenerateRequest) {
	if t == nil {
		return
	}
	prompt := &TracePrompt{Fingerprint: fingerprint, SystemBytes: len(request.System)}
	if request.System != "" {
		sum := sha256.Sum256([]byte(request.System))
		prompt.SystemHash = hex.EncodeToString(sum[:])[:12]
	}
	for _, content := range request.Contents {
		message := TracePromptMessage{Role: string(content.Role), Parts: len(content.Parts)}
		digest := sha256.New()
		for _, part := range content.Parts {
			message.Bytes += len(part.Text)
			digest.Write([]byte(part.Text))
			if part.InlineData != nil || part.ExternalMedia != nil || part.File != nil {
				message.Media++
			}
		}
		message.Hash = hex.EncodeToString(digest.Sum(nil))[:12]
		prompt.Messages = append(prompt.Messages, message)
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.finished {
		t.data.Prompt = prompt
	}
}

// StartAttempt 开始记录一次账号尝试
func (t *Trace) StartAttempt(account string, channel string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.finished {
		return
	}
	t.data.Attempts = append(t.data.Attempts, &TraceAttempt{
		Index: len(t.data.Attempts) + 1, StartedAt: time.Now(), Account: account, Channel: channel,
	})
}

// FinishAttempt 记录当前尝试的结果
func (t *Trace) FinishAttempt(err error) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.finished {
		return
	}
	attempt := t.currentAttemptLocked()
	if attempt.done {
		return
	}
	attempt.done = true
	attempt.ElapsedMS = float64(time.Since(attempt.StartedAt)) / float64(time.Millisecond)
	if err != nil {
		attempt.Result = "失败"
		attempt.Error = err.Error()
	} else {
		attempt.Result = "成功（收到首个事件）"
	}
}

// RecordRequest 记录真正发给 AI Studio 的请求体
func (t *Trace) RecordRequest(method string, url string, body []byte) {
	if t == nil {
		return
	}
	wire := TraceWire{Method: method, URL: url, Body: NewTracePayload(body)}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.finished {
		return
	}
	attempt := t.currentAttemptLocked()
	attempt.Requests = append(attempt.Requests, wire)
}

// TeeUpstream 返回在读取时同时记录上游原始响应开头的读取器
func (t *Trace) TeeUpstream(body io.ReadCloser) io.ReadCloser {
	if t == nil || body == nil {
		return body
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.finished {
		return body
	}
	return &traceUpstreamReader{source: body, trace: t, attempt: t.currentAttemptLocked()}
}

// RecordEvent 记录一个上游事件：正文、思考、结束原因、用量与错误
func (t *Trace) RecordEvent(event Event) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.finished {
		return
	}
	reply := &t.data.Reply
	reply.Events++
	elapsed := float64(time.Since(t.data.StartedAt)) / float64(time.Millisecond)
	if reply.FirstEventMS == 0 {
		reply.FirstEventMS = elapsed
	} else if gap := elapsed - reply.LastEventMS; gap > reply.MaxGapMS {
		reply.MaxGapMS = gap
		reply.MaxGapAtMS = reply.LastEventMS
	}
	reply.LastEventMS = elapsed
	if event.ProviderModel != "" {
		reply.ProviderModel = event.ProviderModel
		if count := len(reply.ModelVersions); (count == 0 || reply.ModelVersions[count-1] != event.ProviderModel) && count < 8 {
			reply.ModelVersions = append(reply.ModelVersions, event.ProviderModel)
		}
	}
	switch event.Kind {
	case EventText:
		reply.TextBytes += len(event.Text)
		t.replyDigest.Write([]byte(event.Text))
		appendTraceLimited(&t.replyText, event.Text, traceReplyLimit)
	case EventReasoning:
		reply.ThoughtBytes += len(event.Text)
		appendTraceLimited(&t.replyThought, event.Text, traceThoughtLimit)
	case EventFinish:
		reply.FinishReason = event.FinishReason
		reply.FinishEventMS = elapsed
	case EventUsage:
		if event.Usage != nil {
			usage := *event.Usage
			reply.Usage = &usage
		}
	case EventToolCall:
		if event.ToolCall != nil {
			reply.ToolCalls++
		}
	case EventError:
		if event.Err != nil {
			reply.Error = event.Err.Error()
		}
	}
}

// SetResponse 保存返回给客户端的响应
func (t *Trace) SetResponse(response TraceResponse) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.finished {
		t.data.Response = &response
	}
}

// Note 追加一条说明
func (t *Trace) Note(message string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.finished {
		t.data.Notes = append(t.data.Notes, time.Now().Format("15:04:05.000")+" "+message)
	}
}

// Finish 结束记录并返回 JSON；之后的写入全部忽略
func (t *Trace) Finish() ([]byte, error) {
	if t == nil {
		return nil, nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.finished = true
	t.data.FinishedAt = time.Now()
	t.data.Reply.Text = t.replyText.String()
	t.data.Reply.Thought = t.replyThought.String()
	if t.data.Reply.TextBytes > 0 {
		// 与管理日志中的回复指纹（reply_hash）算法相同，可直接对照
		t.data.Reply.TextHash = hex.EncodeToString(t.replyDigest.Sum(nil))[:12]
	}
	for _, attempt := range t.data.Attempts {
		attempt.UpstreamHead = string(attempt.head)
	}
	t.analyzeLocked()
	callbacks := t.onFinish
	t.onFinish = nil
	for _, callback := range callbacks {
		callback()
	}
	return json.MarshalIndent(t.data, "", "  ")
}

// currentAttemptLocked 返回当前尝试；没有经过号池调度的请求补一条无账号的尝试
func (t *Trace) currentAttemptLocked() *TraceAttempt {
	if len(t.data.Attempts) == 0 {
		t.data.Attempts = append(t.data.Attempts, &TraceAttempt{Index: 1, StartedAt: time.Now()})
	}
	return t.data.Attempts[len(t.data.Attempts)-1]
}

func appendTraceLimited(builder *strings.Builder, value string, limit int) {
	room := limit - builder.Len()
	if room <= 0 {
		return
	}
	if len(value) > room {
		value = value[:room]
	}
	builder.WriteString(value)
}

type traceUpstreamReader struct {
	source  io.ReadCloser
	trace   *Trace
	attempt *TraceAttempt
}

// Read 读取上游响应并记录开头部分
func (reader *traceUpstreamReader) Read(buffer []byte) (int, error) {
	count, err := reader.source.Read(buffer)
	if count > 0 {
		reader.trace.mu.Lock()
		if !reader.trace.finished {
			reader.attempt.UpstreamBytes += int64(count)
			if room := traceUpstreamLimit - len(reader.attempt.head); room > 0 {
				chunk := buffer[:count]
				if len(chunk) > room {
					chunk = chunk[:room]
				}
				reader.attempt.head = append(reader.attempt.head, chunk...)
			}
		}
		reader.trace.mu.Unlock()
	}
	return count, err
}

// Close 关闭上游响应
func (reader *traceUpstreamReader) Close() error {
	return reader.source.Close()
}

// TraceOutcome 为管理日志记录的请求结果，含内部详细错误
type TraceOutcome struct {
	Status       int     `json:"status"`
	LatencyMS    float64 `json:"latency_ms"`
	Model        string  `json:"model,omitempty"`
	Account      string  `json:"account,omitempty"`
	Channel      string  `json:"channel,omitempty"`
	FinishReason string  `json:"finish_reason,omitempty"`
	Seed         string  `json:"seed,omitempty"`
	Error        string  `json:"error,omitempty"`
	Canceled     bool    `json:"canceled,omitempty"`
	// ServedModel 为上游标明的实际服务模型（与请求的模型系列不同时）
	ServedModel string `json:"served_model,omitempty"`
	// Downgrade 为降级判定的依据（只有被拦截的模型才有）
	Downgrade *DowngradeDecision `json:"downgrade,omitempty"`
}

// SetOutcome 保存请求结果
func (t *Trace) SetOutcome(outcome TraceOutcome) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.finished {
		t.data.Outcome = &outcome
	}
}

// compactTraceJSON 把 JSON 里的长 base64 字符串换成摘要，嵌套在字符串里的 JSON（如 Build 代理请求体）一并处理
func compactTraceJSON(data []byte, depth int) ([]byte, bool) {
	if len(data) < traceInlineLimit {
		return nil, false
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, false
	}
	changed := false
	value = compactTraceValue(value, depth, &changed)
	if !changed {
		return nil, false
	}
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, false
	}
	return bytes.TrimRight(buffer.Bytes(), "\n"), true
}

func compactTraceValue(value any, depth int, changed *bool) any {
	switch typed := value.(type) {
	case map[string]any:
		for key, item := range typed {
			typed[key] = compactTraceValue(item, depth, changed)
		}
		return typed
	case []any:
		for index, item := range typed {
			typed[index] = compactTraceValue(item, depth, changed)
		}
		return typed
	case string:
		return compactTraceString(typed, depth, changed)
	}
	return value
}

func compactTraceString(value string, depth int, changed *bool) string {
	if len(value) < traceInlineLimit {
		return value
	}
	if value[0] == '{' || value[0] == '[' {
		if depth < 3 {
			if nested, ok := compactTraceJSON([]byte(value), depth+1); ok {
				*changed = true
				return string(nested)
			}
		}
		return value
	}
	prefix, data := "", value
	if strings.HasPrefix(value, "data:") {
		if comma := strings.IndexByte(value, ','); comma > 0 && comma < 200 {
			prefix, data = value[:comma+1], value[comma+1:]
		}
	}
	if !looksLikeBase64(data) {
		return value
	}
	*changed = true
	sum := sha256.Sum256([]byte(data))
	return fmt.Sprintf("%s<已省略 %d 字节 base64 媒体数据，sha256=%s，开头=%s>", prefix, len(data), hex.EncodeToString(sum[:])[:16], data[:64])
}

func looksLikeBase64(value string) bool {
	if len(value) < traceInlineLimit {
		return false
	}
	for index := 0; index < len(value); index++ {
		c := value[index]
		if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' ||
			c == '+' || c == '/' || c == '=' || c == '-' || c == '_' || c == '\n' || c == '\r') {
			return false
		}
	}
	return true
}
