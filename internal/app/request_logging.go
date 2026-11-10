package app

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/api"
	"github.com/Mag1cFall/AIStudio2API/internal/requestdb"
)

// activeTraces 为正在记录的排查请求（请求 ID → 排查记录）：该请求的进度日志同步写进排查记录的时间线
var activeTraces sync.Map

// requestLogData 投影请求身份、参数与端到端用量
func requestLogData(entry api.AccessLog) *api.RequestLog {
	data := &api.RequestLog{
		ID: entry.RequestID, Model: entry.Model, Method: entry.Method, Path: entry.Path,
		Status: entry.Status, DurationMS: float64(entry.Latency) / float64(time.Millisecond),
		ToolCalls: entry.ToolCalls, FinishReason: entry.FinishReason, ReplyHash: entry.ReplyHash, Error: entry.Error, ServedModel: entry.ServedModel, Downgrade: entry.Downgrade,
		InputMessages: entry.InputMessages, InputTextChars: entry.InputTextChars,
		InputMedia: entry.InputMedia, InputMediaBytes: entry.InputMediaBytes, InputFiles: entry.InputFiles,
		FirstEventMS: float64(entry.FirstEvent) / float64(time.Millisecond), UpstreamBytes: entry.UpstreamBytes,
		QueueMS: float64(entry.QueueWait) / float64(time.Millisecond), ProofMS: float64(entry.Proof) / float64(time.Millisecond),
		Channel: entry.Channel, Pool: entry.Pool, Disconnect: entry.Disconnect, FallbackFrom: entry.FallbackFrom,
	}
	if entry.Generation {
		data.Parameters = map[string]string{
			"temperature": entry.Temperature, "top_p": entry.TopP, "top_k": entry.TopK,
			"thinking": strings.ToLower(entry.Thinking), "max_output_tokens": entry.MaxOutputTokens,
			"seed": entry.Seed,
		}
	}
	if usage := entry.Usage; usage != nil {
		output := usage.OutputTokens + usage.ReasoningTokens
		data.Usage = &api.RequestLogUsage{
			InputTokens: usage.InputTokens + usage.ToolTokens, ReasoningTokens: usage.ReasoningTokens,
			ReplyTokens: usage.OutputTokens, OutputTokens: output, TotalTokens: usage.TotalTokens,
		}
		if entry.Latency > 0 {
			data.Usage.AverageTokensPerSecond = float64(output) / entry.Latency.Seconds()
		}
	}
	return data
}

// requestPoolLabel 返回活动请求与请求日志使用的号池标记：经 /ultra 进入的请求为 ultra，其余为空
func requestPoolLabel(ctx context.Context) string {
	if aistudio.PoolScopeFromContext(ctx) == aistudio.PoolScopeUltra {
		return aistudio.PoolScopeUltra.String()
	}
	return ""
}

// RecordAccessStart 保存可与后续事件关联的请求开始记录
func (admin *runtimeAdmin) RecordAccessStart(entry api.AccessLog) {
	data := requestLogData(entry)
	data.State = "running"
	admin.recordRequestLog(entry.Account, "INFO", "request.started", "请求开始", data)
}

// requestOutcome 判定请求结果状态、日志级别与摘要，区分工具调用、限制与失败
func requestOutcome(entry api.AccessLog) (string, string, string) {
	switch {
	case entry.Canceled || entry.Status == 499:
		return "cancelled", "WARN", "请求已取消"
	case entry.Status >= http.StatusBadRequest || entry.Error != "":
		return "failed", "ERROR", "请求失败"
	case entry.FinishReason == "max_tokens" || entry.FinishReason == "max_output_tokens" || entry.FinishReason == "length":
		return "limited", "WARN", "达到输出上限"
	case entry.FinishReason != "" && entry.FinishReason != "stop" && entry.FinishReason != "stop_sequence":
		return "blocked", "WARN", "上游终止生成"
	case entry.ToolCalls > 0:
		return "tool_calls", "INFO", "工具调用完成"
	}
	return "completed", "INFO", "请求完成"
}

// upstreamFinishMessageLimit 为管理日志里上游结束说明的最大字数：MALFORMED_FUNCTION_CALL 的说明会带上整段出错的调用
const upstreamFinishMessageLimit = 2000

// upstreamFinishLog 返回上游结束说明（finishMessage）的管理日志级别与内容：正常结束为 INFO，其余为 WARN；过长的说明截断
func upstreamFinishLog(reason string, message string) (string, string) {
	level := "WARN"
	if strings.EqualFold(reason, "stop") || strings.EqualFold(reason, "stop_sequence") {
		level = "INFO"
	}
	if runes := []rune(message); len(runes) > upstreamFinishMessageLimit {
		message = string(runes[:upstreamFinishMessageLimit]) + "…"
	}
	return level, "上游结束说明 | 结束原因=" + reason + " | 说明=" + message
}

// finishReasonAdvice 为需要写清原因与建议的上游结束原因返回一行管理日志，其余原因返回空串。客户端经 new-api 等中转时只看到
// content_filter 或空回复（中转把不认识的结束原因一律当作内容拦截），原因只能在这里看
func finishReasonAdvice(reason string) string {
	switch strings.ToLower(strings.TrimSpace(reason)) {
	case "missing_thought_signature":
		return "上游以缺少思考签名结束（MISSING_THOUGHT_SIGNATURE） | 原因=历史里的函数调用没有带回上一轮的思考签名" +
			"（中转丢掉了签名、客户端改写了调用，或服务重启后本服务记的签名已不在）" +
			" | 建议=让客户端或中转原样回传 functionCall 的 thoughtSignature（OpenAI 格式为 extra_content.google.thought_signature）；" +
			"经 new-api 时保留调用 ID 原样回传，本服务按调用 ID 还原本进程签发的签名（服务重启后还原不到）"
	case "too_many_tool_calls":
		return "上游以工具调用过多结束（TOO_MANY_TOOL_CALLS） | 原因=模型在这一轮连续或并行调用工具的次数超过上游上限" +
			" | 建议=减少一次回复内的工具调用（限制客户端的检索轮数、合并工具或缩小每次返回的结果），或在提示词里要求先根据已有结果作答"
	default:
		return ""
	}
}

// finishedRequestLog 投影请求结果日志，失败且没有错误信息时记录 HTTP 状态；取消的请求在摘要后附上断开现场（见 disconnectSummary）
func finishedRequestLog(entry api.AccessLog) (*api.RequestLog, string, string) {
	data := requestLogData(entry)
	state, level, message := requestOutcome(entry)
	data.State = state
	if state == "failed" && data.Error == "" {
		data.Error = fmt.Sprintf("HTTP %d", entry.Status)
	}
	if state == "cancelled" && entry.Disconnect != nil {
		message += " | " + disconnectSummary(entry.Disconnect)
	}
	return data, level, message
}

// disconnectSummary 为取消请求的断开现场：谁断开、断开阶段、是否已提交响应头（提交后多久断开）、已写出多少、最后一次写出（含心跳）、
// 最后一次写出中转会转发的数据与上游最近一次事件距断开多久。
// 例："客户端断开 | 阶段=首事件前 | 响应头=已提交（断开前 9.8s） | 已写出=2 块 312 B | 距最近写出=9.8s | 距最近数据=9.8s | 距上游最近事件=无 | 已用时=20s"；
// 写响应头后 1 秒内断开时末尾另附“写响应头后立即断开（经中转时多为下游在响应头之前已断开）”（见 RequestDisconnect.RightAfterHeaders）
func disconnectSummary(disconnect *api.RequestDisconnect) string {
	who := "客户端断开"
	if disconnect.Initiator != "client" {
		who = "服务端取消（管理端取消请求或服务停止、重启）"
	}
	phase := "首事件前"
	if disconnect.Phase == "mid_stream" {
		phase = "流中"
	}
	headers := "未提交"
	if disconnect.HeadersSent {
		headers = "已提交"
		if since := disconnect.SinceHeadersSentMS; since != nil {
			headers += "（断开前 " + formatLogMS(*since) + "）"
		}
	}
	data := "无"
	if since := disconnect.SinceLastDataWriteMS; since != nil {
		data = formatLogMS(*since)
	}
	upstream := "无"
	if since := disconnect.SinceUpstreamEventMS; since != nil {
		upstream = formatLogMS(*since)
	}
	summary := fmt.Sprintf("%s | 阶段=%s | 响应头=%s | 已写出=%d 块 %s | 距最近写出=%s | 距最近数据=%s | 距上游最近事件=%s | 已用时=%s",
		who, phase, headers, disconnect.WrittenChunks, formatLogBytes(disconnect.WrittenBytes),
		formatLogMS(disconnect.SinceLastWriteMS), data, upstream, formatLogMS(disconnect.ElapsedMS))
	if disconnect.RightAfterHeaders {
		summary += " | 写响应头后立即断开（经中转时多为下游在响应头之前已断开）"
	}
	return summary
}

// formatLogMS 把毫秒写成管理日志里的时长（秒，保留一位小数）
func formatLogMS(ms float64) string {
	return (time.Duration(ms * float64(time.Millisecond))).Round(100 * time.Millisecond).String()
}

// formatLogBytes 把字节数写成管理日志里的大小
func formatLogBytes(bytes int64) string {
	switch {
	case bytes >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(bytes)/(1<<20))
	case bytes >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(bytes)/(1<<10))
	default:
		return fmt.Sprintf("%d B", bytes)
	}
}

// RecordAccessLog 保存请求结果日志
func (admin *runtimeAdmin) RecordAccessLog(entry api.AccessLog) {
	data, level, message := finishedRequestLog(entry)
	admin.recordRequestLog(entry.Account, level, "request.finished", message, data)
}

// ledgerEntry 判断请求是否写入用量账本：通过 API key 校验的 POST 请求，token 计数请求除外
func ledgerEntry(entry api.AccessLog) bool {
	return entry.Method == http.MethodPost && entry.Authorized && requestProtocol(entry.Path) != "count_tokens"
}

// requestRow 将 POST 请求结果投影为用量账本记录；账户、错误与请求日志显示的内容相同，不含密钥与请求头
func requestRow(entry api.AccessLog, finished time.Time) requestdb.Row {
	data, _, _ := finishedRequestLog(entry)
	row := requestdb.Row{
		ID: entry.RequestID, Time: finished, Protocol: requestProtocol(entry.Path), Path: entry.Path,
		Model: entry.Model, Account: entry.Account, Channel: entry.Channel, Status: entry.Status, State: data.State,
		Duration: entry.Latency, FirstEvent: entry.FirstEvent, Queue: entry.QueueWait, ToolCalls: entry.ToolCalls, Error: data.Error,
		Attempts: entry.Attempts, ServedModel: entry.ServedModel, ReplyHash: entry.ReplyHash, Pool: entry.Pool,
	}
	if entry.Downgrade != nil {
		row.Downgrade = entry.Downgrade.Verdict
	}
	if usage := data.Usage; usage != nil {
		row.InputTokens, row.ReasoningTokens, row.ReplyTokens, row.TotalTokens =
			usage.InputTokens, usage.ReasoningTokens, usage.ReplyTokens, usage.TotalTokens
	}
	return row
}

// requestProtocol 按公开 API 路径归类请求协议；排查路由 /trace/ 与 Ultra 路由 /ultra/ 的请求先去掉前缀，与主路由归为同一协议
func requestProtocol(path string) string {
	path = strings.TrimPrefix(strings.TrimPrefix(path, "/trace"), "/ultra")
	switch {
	case api.CountTokensPath(path):
		return "count_tokens"
	case path == "/v1/chat/completions":
		return "openai-chat"
	case path == "/v1/responses":
		return "openai-responses"
	case strings.HasPrefix(path, "/v1/messages"):
		return "anthropic"
	case path == "/v1/interactions" || path == "/v1beta/interactions":
		// Interactions API 在 /v1 与 /v1beta 下各有一个入口，单独归类，不并入 gemini
		return "interactions"
	case strings.HasPrefix(path, "/v1beta/"):
		return "gemini"
	case strings.HasPrefix(path, "/v1/images/"):
		return "images"
	case strings.HasPrefix(path, "/v1/audio/"):
		return "audio"
	case strings.HasPrefix(path, "/v1/videos"):
		return "videos"
	case strings.HasPrefix(path, "/v1/files"):
		return "files"
	}
	return "other"
}

// recordRequestLog 统一请求日志的账户来源与事件载荷
func (admin *runtimeAdmin) recordRequestLog(source, level, event, message string, data *api.RequestLog) {
	if source == "" {
		source = "request"
	}
	admin.requests.recordLog(api.AdminLog{Source: source, Level: level, Event: event, Message: message, Request: data})
}

// logRequestProgress 将等待与恢复事件关联到所属请求
func (registry *requestRegistry) logRequestProgress(id, source, level, message string) {
	registry.recordLog(api.AdminLog{Source: source, Level: level, Event: "request.progress", Message: message,
		Request: &api.RequestLog{ID: id, State: "running"}})
	if value, ok := activeTraces.Load(id); ok {
		value.(*aistudio.Trace).Timeline(level, source, message)
	}
}
