package app

import (
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
		Channel: entry.Channel,
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

// finishedRequestLog 投影请求结果日志，失败且没有错误信息时记录 HTTP 状态
func finishedRequestLog(entry api.AccessLog) (*api.RequestLog, string, string) {
	data := requestLogData(entry)
	state, level, message := requestOutcome(entry)
	data.State = state
	if state == "failed" && data.Error == "" {
		data.Error = fmt.Sprintf("HTTP %d", entry.Status)
	}
	return data, level, message
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
		Attempts: entry.Attempts, ServedModel: entry.ServedModel, ReplyHash: entry.ReplyHash,
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

// requestProtocol 按公开 API 路径归类请求协议；排查路由 /trace/ 的请求先去掉前缀，与主路由归为同一协议
func requestProtocol(path string) string {
	path = strings.TrimPrefix(path, "/trace")
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
