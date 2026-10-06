package aistudio

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// TraceFinding 为排查记录结束时自动判断出的一项问题或提示
type TraceFinding struct {
	// Level 为 错误、异常 或 提示
	Level  string `json:"level"`
	Title  string `json:"title"`
	Detail string `json:"detail"`
}

// TraceTimelineEntry 为请求时间线上的一条记录，与管理日志里该请求的进度记录一致
type TraceTimelineEntry struct {
	MS      float64 `json:"ms"`
	Level   string  `json:"level"`
	Source  string  `json:"source,omitempty"`
	Message string  `json:"message"`
}

// traceTimelineLimit 为时间线最多保留的条数
const traceTimelineLimit = 400

// Timeline 追加一条请求进度（等待账号、换号原因、首个事件、重复回复等），与管理日志一致
func (t *Trace) Timeline(level, source, message string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.finished || len(t.data.Timeline) >= traceTimelineLimit {
		return
	}
	t.data.Timeline = append(t.data.Timeline, TraceTimelineEntry{
		MS: traceMS(time.Since(t.data.StartedAt)), Level: level, Source: source, Message: message,
	})
}

// OnFinish 注册记录结束时执行的回调
func (t *Trace) OnFinish(callback func()) {
	if t == nil || callback == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.finished {
		callback()
		return
	}
	t.onFinish = append(t.onFinish, callback)
}

func traceMS(duration time.Duration) float64 {
	return float64(duration) / float64(time.Millisecond)
}

// analyzeLocked 根据整条记录判断这次请求有没有问题，结论放在记录最前面（summary、flags、findings），
// 不用逐段读原始数据就能看出问题出在哪一步
func (t *Trace) analyzeLocked() {
	data := &t.data
	reply := &data.Reply
	findings := make([]TraceFinding, 0, 4)
	add := func(level, title, detail string) {
		findings = append(findings, TraceFinding{Level: level, Title: title, Detail: detail})
	}
	status, latency, model, channel, errText, canceled := 0, traceMS(data.FinishedAt.Sub(data.StartedAt)), reply.ProviderModel, "", reply.Error, false
	if outcome := data.Outcome; outcome != nil {
		status, canceled, channel = outcome.Status, outcome.Canceled, outcome.Channel
		if outcome.LatencyMS > 0 {
			latency = outcome.LatencyMS
		}
		if outcome.Model != "" {
			model = outcome.Model
		}
		if outcome.Error != "" {
			errText = outcome.Error
		}
	} else if data.Response != nil {
		status = data.Response.Status
	}
	var think, output, input int64
	if usage := reply.Usage; usage != nil {
		think, output, input = usage.ReasoningTokens, usage.OutputTokens, usage.InputTokens+usage.ToolTokens
	}
	success := status > 0 && status < 400 && !canceled
	servedModel := ""
	if data.Outcome != nil {
		servedModel = data.Outcome.ServedModel
	}
	requested := model
	if count := len(data.Parameters); count > 0 && data.Parameters[count-1].Model != "" {
		requested = data.Parameters[count-1].Model
	}
	// 预填充：客户端最后一条是模型消息，转换时在末尾补了一条请模型接着写的用户消息（见 notes）
	prefill := false
	for _, note := range data.Notes {
		if strings.Contains(note, "预填充") {
			prefill = true
			break
		}
	}

	// 1. 请求结果
	switch {
	case canceled || status == 499:
		add("异常", "客户端提前断开", fmt.Sprintf(
			"客户端在第 %.1f 秒断开了连接，此时上游已返回正文 %d 字节。常见原因：客户端超时设得太短、用户手动停止、网络中断",
			latency/1000, reply.TextBytes))
	case status >= 400:
		add("错误", fmt.Sprintf("请求失败（HTTP %d）", status), traceFirstNonEmpty(errText, "没有记录到详细错误"))
	}

	// 降级判定（只有被拦截的模型才有）：拒绝时列为错误，放行时作为提示附上判定依据
	if data.Outcome != nil && data.Outcome.Downgrade != nil {
		decision := data.Outcome.Downgrade
		switch decision.Verdict {
		case "rejected":
			add("错误", "因降级拒绝", decision.Basis)
		case "passed":
			add("提示", "降级判定：正常", decision.Basis)
		default:
			add("提示", "降级判定：数据不足，放行", decision.Basis)
		}
	}

	// 2. 换号重试
	var failures []string
	var retrySpent float64
	for _, attempt := range data.Attempts {
		if attempt.Error == "" && (attempt.Result == "" || strings.HasPrefix(attempt.Result, "成功")) {
			continue
		}
		retrySpent += attempt.ElapsedMS
		failures = append(failures, fmt.Sprintf("第 %d 次（%s，%s，%.1f 秒）：%s", attempt.Index,
			traceFirstNonEmpty(attempt.Channel, "未知通道"), traceFirstNonEmpty(attempt.Account, "未分到账号"),
			attempt.ElapsedMS/1000, traceShorten(traceFirstNonEmpty(attempt.Error, attempt.Result), 200)))
	}
	if len(failures) > 0 {
		level := "提示"
		if status >= 400 || len(failures) >= 3 || retrySpent >= 10000 {
			level = "异常"
		}
		add(level, fmt.Sprintf("换号重试 %d 次", len(failures)),
			fmt.Sprintf("失败的尝试共用了 %.1f 秒。%s", retrySpent/1000, strings.Join(failures, "；")))
	}

	// 3. 等待账号与首字
	if len(data.Attempts) > 0 {
		if wait := traceMS(data.Attempts[0].StartedAt.Sub(data.StartedAt)); wait >= 5000 {
			add("异常", "等待账号", fmt.Sprintf("请求等了 %.1f 秒才分到第一个账号（号池繁忙或可用账号不足）", wait/1000))
		}
	}
	if reply.FirstEventMS >= 30000 {
		detail := fmt.Sprintf("上游第一个事件在第 %.1f 秒才到达", reply.FirstEventMS/1000)
		if count := len(data.Attempts); count > 0 {
			start := traceMS(data.Attempts[count-1].StartedAt.Sub(data.StartedAt))
			if start > 0 && start < reply.FirstEventMS {
				detail += fmt.Sprintf("（最后一次尝试从第 %.1f 秒开始，上游处理了 %.1f 秒）", start/1000, (reply.FirstEventMS-start)/1000)
			}
		}
		if input > 0 {
			detail += fmt.Sprintf("；输入 %d token", input)
		}
		if think > 0 {
			detail += fmt.Sprintf("，思考 %d token", think)
		}
		add("异常", "首字慢", detail)
	}

	// 4. 输出中途停顿、生成结束后迟迟没有收尾
	if reply.MaxGapMS >= 20000 {
		add("异常", "输出中途停顿", fmt.Sprintf("上游在第 %.1f 秒处停了 %.1f 秒才继续输出", reply.MaxGapAtMS/1000, reply.MaxGapMS/1000))
	}
	if reply.LastEventMS > 0 && latency-reply.LastEventMS >= 10000 {
		add("异常", "收尾慢", fmt.Sprintf("上游最后一个事件在第 %.1f 秒，请求到第 %.1f 秒才结束，中间空等了 %.1f 秒",
			reply.LastEventMS/1000, latency/1000, (latency-reply.LastEventMS)/1000))
	}

	// 5. 回复内容与结束原因
	finish := strings.ToLower(reply.FinishReason)
	if success && reply.TextBytes == 0 && reply.ToolCalls == 0 {
		detail := "上游没有返回任何正文"
		if reply.ThoughtBytes > 0 {
			detail = fmt.Sprintf("上游只返回了思考内容（%d 字节），没有正文", reply.ThoughtBytes)
		}
		if finish != "" {
			detail += "；结束原因 " + reply.FinishReason
		}
		add("异常", "回复为空", detail)
	}
	switch finish {
	case "", "stop", "end_turn", "unspecified":
	case "stop_sequence":
		add("提示", "遇到停止词结束", "回复遇到客户端设置的停止词后结束；如果回复看起来不完整，检查客户端的停止词设置")
	case "max_tokens", "max_output_tokens", "length":
		config := traceLastConfig(data.Parameters)
		limit := "未设置（用模型上限）"
		if config.MaxOutputTokens != nil {
			limit = strconv.FormatInt(*config.MaxOutputTokens, 10)
		}
		for _, note := range data.Notes {
			if strings.Contains(note, "最大输出") {
				limit += "，发送时已按下限调整（见 notes）"
				break
			}
		}
		add("异常", "达到最大输出被截断", fmt.Sprintf("客户端设置的最大输出：%s；实际用了思考 %d + 正文 %d token，思考也算在最大输出里", limit, think, output))
	default:
		add("异常", "上游提前终止", fmt.Sprintf("结束原因 %s：%s", reply.FinishReason, traceFinishExplanation(finish)))
	}

	// 5b. 上游标明的实际模型与请求不同（Build 通道最后一块报告实际服务的模型）
	downgraded := false
	for _, version := range reply.ModelVersions {
		if requested != "" && ModelFamily(version) != ModelFamily(requested) {
			downgraded = true
			add("异常", "上游实际由其他模型生成", fmt.Sprintf(
				"请求的模型是 %s，上游在响应里依次标明 %s：前面的内容块沿用请求的模型名，最后一块（含最终用量）报告实际服务的模型。"+
					"生成速度、思考长度、回复质量都会与请求的模型不同；同一段对话换账号重试通常也一样",
				requested, strings.Join(reply.ModelVersions, " → ")))
			break
		}
	}
	// 最后一条是模型消息（预填充）：模型直接接着写，通常不先思考；偶尔只输出思考、没有正文
	if prefill {
		add("提示", "最后一条消息是模型消息（预填充）", "客户端在末尾放了一条模型消息让模型接着写（已转成\"从上一条结尾接着写\"的用户消息）："+
			"这类请求常常跳过思考直接输出（思考 0 token），偶尔也会只输出思考、没有正文")
	}

	// 6. 生成速度：远高于正常时，很可能实际由更快的模型生成，或是上游重放了已经生成过的结果
	end := reply.FinishEventMS
	if end <= 0 {
		end = reply.LastEventMS
	}
	if tokens := think + output; !downgraded && tokens >= 500 && end-reply.FirstEventMS >= 1000 {
		seconds := (end - reply.FirstEventMS) / 1000
		speed := float64(tokens) / seconds
		threshold, normal := 400.0, "Flash 中位数约 190～250 tok/s"
		if strings.Contains(strings.ToLower(model), "pro") {
			threshold, normal = 200, "Pro 中位数约 110 tok/s"
		}
		if speed >= threshold {
			add("异常", "生成速度异常快", fmt.Sprintf(
				"从首个事件到结束 %.0f tok/s（思考 %d + 正文 %d token 用了 %.1f 秒），远高于正常（%s）："+
					"很可能实际由更快的模型（如 3.1-flash-lite）生成，或是上游重放了已有结果。Playground 通道的响应里没有模型名，无法直接核对",
				speed, think, output, seconds, normal))
		}
	}

	// 7. 客户端实际收到的内容与上游返回的不一致（格式转换出错、输出中断、重复输出）
	if response := data.Response; response != nil && !response.Truncated && success && reply.TextBytes >= 200 {
		if text, thought, ok := clientVisibleText(response.ContentType, response.Head); ok {
			switch {
			case text < reply.TextBytes*9/10 && text+thought < reply.TextBytes*9/10:
				add("异常", "客户端收到的正文比上游少", fmt.Sprintf("上游返回正文 %d 字节，实际发给客户端的约 %d 字节", reply.TextBytes, text))
			case text+thought > (reply.TextBytes+reply.ThoughtBytes)*11/10+64:
				add("异常", "客户端收到的内容比上游多", fmt.Sprintf("上游返回正文 %d、思考 %d 字节，实际发给客户端的正文约 %d、思考约 %d 字节（可能有重复输出）",
					reply.TextBytes, reply.ThoughtBytes, text, thought))
			}
		}
	}

	// 8. 输入很长
	if input >= 200000 {
		add("提示", "输入很长", fmt.Sprintf("输入 %d token，首字时间和总耗时都会明显变长", input))
	}

	// 9. 时间线里的警告与错误（换号原因、重复回复、模型不一致等）
	seen := make(map[string]bool)
	var warnings []string
	for _, entry := range data.Timeline {
		if entry.Level != "WARN" && entry.Level != "ERROR" {
			continue
		}
		message := traceShorten(entry.Message, 240)
		if seen[message] || len(warnings) >= 6 {
			continue
		}
		seen[message] = true
		warnings = append(warnings, fmt.Sprintf("第 %.1f 秒 %s", entry.MS/1000, message))
	}
	if len(warnings) > 0 {
		add("提示", "时间线中的警告", strings.Join(warnings, "；"))
	}

	flags := make([]string, 0, len(findings))
	for _, finding := range findings {
		if finding.Level != "提示" {
			flags = append(flags, finding.Title)
		}
	}
	parts := []string{fmt.Sprintf("HTTP %d", status)}
	if model != "" {
		if servedModel != "" {
			model += "（上游实际 " + servedModel + "）"
		}
		parts = append(parts, model)
	}
	if channel != "" {
		parts = append(parts, channel)
	}
	parts = append(parts, fmt.Sprintf("耗时 %.1fs", latency/1000))
	if reply.FirstEventMS > 0 {
		parts = append(parts, fmt.Sprintf("首字 %.1fs", reply.FirstEventMS/1000))
	}
	if reply.Usage != nil {
		parts = append(parts, fmt.Sprintf("输入 %d · 思考 %d · 正文 %d token", input, think, output))
	}
	if data.Client != nil {
		if agent := data.Client.Headers["User-Agent"]; agent != "" {
			parts = append(parts, "客户端 "+traceShorten(agent, 48))
		}
	}
	// 降级判定（只有被拦截的模型才有）：摘要里始终写明结论与测得的速度，放行的请求也能直接看到依据
	if data.Outcome != nil && data.Outcome.Downgrade != nil {
		parts = append(parts, traceDowngradeSummary(*data.Outcome.Downgrade))
	}
	if len(flags) == 0 {
		parts = append(parts, "未发现异常")
	} else {
		parts = append(parts, "发现："+strings.Join(flags, "、"))
	}
	data.Summary = strings.Join(parts, " · ")
	data.Flags = flags
	data.Findings = findings
}

// traceLastConfig 返回最后一个阶段的生成参数
func traceLastConfig(parameters []TraceParameters) GenerationConfig {
	for index := len(parameters) - 1; index >= 0; index-- {
		var config GenerationConfig
		if len(parameters[index].Config) > 0 && json.Unmarshal(parameters[index].Config, &config) == nil {
			return config
		}
	}
	return GenerationConfig{}
}

// traceFinishExplanation 解释上游的非正常结束原因
func traceFinishExplanation(reason string) string {
	switch reason {
	case "safety", "prohibited_content", "blocklist", "spii", "image_safety", "image_prohibited_content":
		return "内容被谷歌的安全策略拦截"
	case "recitation", "image_recitation":
		return "回复与已有的受版权保护内容过于相似，被上游拦截"
	case "malformed_function_call", "unexpected_tool_call", "too_many_tool_calls":
		return "模型生成的工具调用不符合要求"
	case "language":
		return "使用了上游不支持的语言"
	case "other":
		return "上游没有说明原因"
	}
	return "上游没有正常结束"
}

// clientVisibleText 从返回给客户端的响应里统计正文与思考的字节数，兼容 OpenAI（含 Responses）、Anthropic、
// Gemini 的流式与非流式格式；解析不了时 ok 为 false
func clientVisibleText(contentType, body string) (text int, thought int, ok bool) {
	var counter traceTextCounter
	if strings.Contains(strings.ToLower(contentType), "event-stream") {
		for _, line := range strings.Split(body, "\n") {
			line = strings.TrimSpace(line)
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if payload == "" || payload == "[DONE]" {
				continue
			}
			var event any
			if json.Unmarshal([]byte(payload), &event) != nil {
				continue
			}
			counter.parsed = true
			counter.event(event)
		}
	} else {
		var value any
		if json.Unmarshal([]byte(body), &value) != nil {
			return 0, 0, false
		}
		counter.parsed = true
		counter.event(value)
	}
	return counter.text, counter.thought, counter.parsed
}

type traceTextCounter struct {
	text    int
	thought int
	parsed  bool
}

// traceSkippedKeys 为不属于回复文字的字段（工具参数、用量、签名、引用等）
var traceSkippedKeys = map[string]bool{
	"tool_calls": true, "function_call": true, "functionCall": true, "arguments": true, "input": true,
	"partial_json": true, "usage": true, "usageMetadata": true, "signature": true, "thoughtSignature": true,
	"logprobs": true, "annotations": true, "citations": true, "groundingMetadata": true, "citationMetadata": true,
}

// event 处理一个完整事件；OpenAI Responses 接口除 *.delta 以外的事件会重复携带完整内容，跳过
func (c *traceTextCounter) event(value any) {
	if object, ok := value.(map[string]any); ok {
		if kind, _ := object["type"].(string); strings.HasPrefix(kind, "response.") && !strings.HasSuffix(kind, ".delta") {
			return
		}
	}
	c.walk(value, false)
}

func (c *traceTextCounter) walk(value any, thought bool) {
	switch typed := value.(type) {
	case []any:
		for _, item := range typed {
			c.walk(item, thought)
		}
	case map[string]any:
		kind, _ := typed["type"].(string)
		lower := strings.ToLower(kind)
		inThought := thought || typed["thought"] == true || strings.Contains(lower, "reasoning") ||
			strings.Contains(lower, "thinking") || strings.Contains(lower, "summary")
		for key, item := range typed {
			if traceSkippedKeys[key] {
				continue
			}
			text, isString := item.(string)
			switch key {
			case "text", "content":
				if isString {
					c.add(text, inThought)
				} else {
					c.walk(item, inThought)
				}
			case "reasoning_content", "reasoning", "thinking":
				if isString {
					c.add(text, true)
				} else {
					c.walk(item, true)
				}
			case "delta":
				switch {
				case !isString:
					c.walk(item, inThought)
				case strings.Contains(lower, "output_text"):
					c.add(text, false)
				case strings.Contains(lower, "reasoning") || strings.Contains(lower, "summary"):
					c.add(text, true)
				}
			default:
				if !isString {
					c.walk(item, inThought)
				}
			}
		}
	}
}

func (c *traceTextCounter) add(text string, thought bool) {
	if thought {
		c.thought += len(text)
	} else {
		c.text += len(text)
	}
}

// traceShorten 压缩空白并按字符截断
func traceShorten(value string, limit int) string {
	value = strings.Join(strings.Fields(value), " ")
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit]) + "…"
}

func traceFirstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

// ModelFamily 粗分模型系列：pro、flash、flash-lite；版本日期等细节不影响判断
func ModelFamily(model string) string {
	name := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(model), "models/"))
	switch {
	case strings.Contains(name, "flash-lite"):
		return "flash-lite"
	case strings.Contains(name, "flash"):
		return "flash"
	case strings.Contains(name, "pro"):
		return "pro"
	default:
		return name
	}
}

// traceDowngradeSummary 为排查记录摘要里的一句降级判定结论，例如"降级判定：正常 128 tok/s · playground"
func traceDowngradeSummary(decision DowngradeDecision) string {
	verdict := decision.Verdict
	switch decision.Verdict {
	case "rejected":
		verdict = "拒绝"
	case "passed":
		verdict = "正常"
	case "unjudged":
		verdict = "数据不足，放行"
	}
	text := "降级判定：" + verdict
	if decision.Memory {
		return text + "（同一段对话近期被判定为降级）"
	}
	if decision.Speed > 0 {
		text += fmt.Sprintf(" %.0f tok/s", decision.Speed)
		if decision.Estimated && !decision.CountTokens {
			text += "（估算）"
		}
	}
	if decision.ServedModel != "" {
		text += "（上游标明 " + decision.ServedModel + "）"
	}
	if decision.Channel != "" {
		text += " · " + decision.Channel
	}
	return text
}
