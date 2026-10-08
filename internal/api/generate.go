package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

var errIncompleteStream = errors.New("upstream stream closed before finish")
var errUpstreamStream = errors.New("upstream stream error")

const streamHeartbeatInterval = 10 * time.Second

type generationResult struct {
	events        []aistudio.Event
	text          strings.Builder
	reasoning     strings.Builder
	toolCalls     []aistudio.FunctionCall
	citations     []aistudio.Citation
	grounding     *aistudio.GroundingMetadata
	media         []aistudio.Media
	usage         *aistudio.Usage
	finishReason  string
	stopSequence  string
	providerModel string
	finished      bool
}

func (result *generationResult) apply(event aistudio.Event) error {
	if event.ProviderModel != "" {
		result.providerModel = event.ProviderModel
	}
	if event.Usage != nil {
		usage := *event.Usage
		result.usage = &usage
	}
	if event.Kind == aistudio.EventError {
		if event.Err != nil {
			return event.Err
		}
		return errUpstreamStream
	}
	switch event.Kind {
	case aistudio.EventText:
		result.text.WriteString(event.Text)
	case aistudio.EventReasoning:
		result.reasoning.WriteString(event.Text)
	case aistudio.EventToolCall:
		if event.ToolCall != nil {
			call := *event.ToolCall
			if len(call.Arguments) == 0 {
				call.Arguments = json.RawMessage(`{}`)
			}
			if !json.Valid(call.Arguments) {
				return fmt.Errorf("upstream tool call %q arguments are not JSON", call.Name)
			}
			event.ToolCall = &call
			result.toolCalls = append(result.toolCalls, call)
		}
	case aistudio.EventGrounding:
		if event.Grounding != nil {
			grounding := *event.Grounding
			result.grounding = &grounding
			result.citations = append(result.citations, groundingCitations(grounding)...)
		}
	case aistudio.EventCitation:
		if event.Citation != nil {
			result.citations = append(result.citations, *event.Citation)
		}
	case aistudio.EventMedia:
		if event.Media != nil {
			if event.Media.MIME == "" || (len(event.Media.Data) == 0 && event.Media.URL == "") {
				return fmt.Errorf("upstream media is missing MIME or content")
			}
			media := *event.Media
			media.Data = append([]byte(nil), media.Data...)
			// 只拼接前后都不带签名的 PCM（audio/l16）分片：裸采样数据可以直接相接，其他格式的分片各自带文件头或帧结构；
			// 带签名的分片保持独立，否则合并后只剩前一片的签名
			if isPCMAudio(media.MIME) && len(media.Data) > 0 && media.URL == "" && event.ThoughtSignature == "" && len(result.events) > 0 {
				previous := &result.events[len(result.events)-1]
				if previous.Kind == aistudio.EventMedia && previous.Media != nil && previous.Media.MIME == media.MIME && previous.Media.URL == "" &&
					previous.ThoughtSignature == "" {
					previous.Media.Data = append(previous.Media.Data, media.Data...)
					result.media[len(result.media)-1].Data = append(result.media[len(result.media)-1].Data, media.Data...)
					return nil
				}
			}
			event.Media = &media
			result.media = append(result.media, media)
		}
	case aistudio.EventFinish:
		result.finishReason = event.FinishReason
		result.stopSequence = event.StopSequence
		result.finished = true
	}
	result.events = append(result.events, event)
	return nil
}

// awaitStreamStart 在写出流式响应头前等待首个事件：首个事件为错误时返回该错误，由调用方按非流式返回
// HTTP 状态和错误对象（参数错误、无可用账号、全部冷却、降级拒绝等不再变成 200 之后的流内错误）；
// 心跳间隔内没有事件时照常开始推流，返回的事件流从首个事件开始重放
func awaitStreamStart(ctx context.Context, events <-chan aistudio.Event) (<-chan aistudio.Event, error) {
	timer := time.NewTimer(streamHeartbeatInterval)
	defer timer.Stop()
	var first aistudio.Event
	select {
	case <-ctx.Done():
		go drainEvents(events)
		return nil, ctx.Err()
	case <-timer.C:
		return events, nil
	case event, ok := <-events:
		if !ok {
			return nil, errIncompleteStream
		}
		if event.Kind == aistudio.EventError {
			go drainEvents(events)
			if event.Err != nil {
				return nil, event.Err
			}
			return nil, errUpstreamStream
		}
		first = event
	}
	replay := make(chan aistudio.Event)
	go func() {
		defer close(replay)
		for event, ok := first, true; ok; event, ok = <-events {
			select {
			case replay <- event:
			case <-ctx.Done():
				// 客户端已断开：继续读完上游，让生产方正常结束
				drainEvents(events)
				return
			}
		}
	}()
	return replay, nil
}

func drainEvents(events <-chan aistudio.Event) {
	for range events {
	}
}

func consumeEvents(ctx context.Context, events <-chan aistudio.Event, emit func(aistudio.Event) error) (result generationResult, resultErr error) {
	return consumeEventsWithHeartbeat(ctx, events, emit, nil)
}

func consumeStreamEvents(
	ctx context.Context,
	events <-chan aistudio.Event,
	emit func(aistudio.Event) error,
	heartbeat func() error,
) (generationResult, error) {
	return consumeEventsWithHeartbeat(ctx, events, emit, heartbeat)
}

func consumeEventsWithHeartbeat(
	ctx context.Context,
	events <-chan aistudio.Event,
	emit func(aistudio.Event) error,
	heartbeat func() error,
) (result generationResult, resultErr error) {
	defer func() {
		SetAccessLogError(ctx, resultErr)
	}()
	var heartbeatTimer *time.Timer
	var heartbeatTick <-chan time.Time
	if heartbeat != nil {
		heartbeatTimer = time.NewTimer(streamHeartbeatInterval)
		heartbeatTick = heartbeatTimer.C
		defer heartbeatTimer.Stop()
	}
	resetHeartbeat := func() {
		if heartbeatTimer == nil {
			return
		}
		if !heartbeatTimer.Stop() {
			select {
			case <-heartbeatTimer.C:
			default:
			}
		}
		heartbeatTimer.Reset(streamHeartbeatInterval)
	}
	for {
		select {
		case <-ctx.Done():
			return result, ctx.Err()
		case <-heartbeatTick:
			if err := heartbeat(); err != nil {
				return result, err
			}
			resetHeartbeat()
		case event, ok := <-events:
			if !ok {
				if !result.finished {
					return result, errIncompleteStream
				}
				return result, nil
			}
			if err := result.apply(event); err != nil {
				return result, err
			}
			if emit != nil {
				if err := emit(event); err != nil {
					return result, err
				}
			}
			resetHeartbeat()
		}
	}
}

func outputTokens(usage *aistudio.Usage) int64 {
	if usage == nil {
		return 0
	}
	return usage.OutputTokens + usage.ReasoningTokens
}

func inputTokens(usage *aistudio.Usage) int64 {
	if usage == nil {
		return 0
	}
	return usage.InputTokens + usage.ToolTokens
}

// providerFinishReason 返回需要原样告诉客户端的上游结束原因：标准原因（正常结束、长度、停止序列）之外的都返回，
// 例如 malformed_function_call、safety、provider_N，便于排查；标准原因返回空串
func providerFinishReason(reason string) string {
	switch normalized := strings.ToLower(strings.TrimSpace(reason)); normalized {
	case "", "stop", "max_tokens", "max_output_tokens", "length", "stop_sequence", "pause_turn":
		return ""
	default:
		return normalized
	}
}

// finishFiltered 判断结束原因是否为内容拦截（安全、版权背诵、屏蔽词、违禁内容、个人敏感信息、图片安全）。
// 其他非标准原因（malformed_function_call、unexpected_tool_call、other、unspecified、provider_N 等）不是拒答：
// 原先一律映射为 content_filter / refusal，Claude Code 会显示违反使用政策，Codex 会把回复当作不完整重试
func finishFiltered(reason string) bool {
	switch strings.ToLower(strings.TrimSpace(reason)) {
	case "safety", "recitation", "blocklist", "prohibited_content", "spii",
		"image_safety", "image_prohibited_content", "image_recitation":
		return true
	default:
		return false
	}
}

// splitFunctionResultMedia 取出工具结果中以 base64 内嵌的图片，作为真正的图片发给上游。
// 原先整个工具结果都按 JSON 发送，图片的 base64 会变成一大串文字：模型看不到图片，
// 输入 token 也会暴涨，连续读取两张图片基本必然超过上限而报错。
// 支持 OpenAI（image_url / input_image 的 data URL）、Anthropic（image + base64 source）、
// MCP（image + data + mimeType）格式，以及 MCP 工具结果对象 {"content": [...]}。
// 取出的图片在原位置替换为文字说明；网络地址形式的图片保持原样
func splitFunctionResultMedia(raw json.RawMessage) (json.RawMessage, []aistudio.Part, error) {
	trimmed := strings.TrimSpace(string(raw))
	if strings.HasPrefix(trimmed, "{") {
		var object map[string]json.RawMessage
		if err := json.Unmarshal([]byte(trimmed), &object); err != nil {
			return raw, nil, nil
		}
		content, exists := object["content"]
		if !exists {
			return raw, nil, nil
		}
		cleaned, media, err := splitFunctionResultMedia(content)
		if err != nil || len(media) == 0 {
			return raw, nil, err
		}
		object["content"] = cleaned
		encoded, err := json.Marshal(object)
		if err != nil {
			return nil, nil, err
		}
		return encoded, media, nil
	}
	if !strings.HasPrefix(trimmed, "[") {
		return raw, nil, nil
	}
	var blocks []json.RawMessage
	if err := json.Unmarshal([]byte(trimmed), &blocks); err != nil {
		return raw, nil, nil
	}
	var media []aistudio.Part
	for index, block := range blocks {
		part, extracted, err := functionResultMediaPart(block)
		if err != nil {
			return nil, nil, err
		}
		if !extracted {
			continue
		}
		part.FromToolResult = true
		media = append(media, part)
		kind := mediaKind(part)
		placeholder, err := json.Marshal(map[string]string{
			"type": "text", "text": fmt.Sprintf("[%s %d：已作为%s附在随后的消息中]", kind, len(media), kind),
		})
		if err != nil {
			return nil, nil, err
		}
		blocks[index] = placeholder
	}
	if len(media) == 0 {
		return raw, nil, nil
	}
	encoded, err := json.Marshal(blocks)
	if err != nil {
		return nil, nil, err
	}
	return encoded, media, nil
}

// mediaKind 返回附件在说明文字里的称呼：图片或文件
func mediaKind(part aistudio.Part) string {
	if part.InlineData != nil && !strings.HasPrefix(part.InlineData.MIME, "image/") {
		return "文件"
	}
	return "图片"
}

// functionResultMediaPart 把工具结果中的一个内容块转成内嵌媒体；不是 base64 图片或文件时返回 false。
// 除图片外还支持 Anthropic document（如 Claude Code 读取 PDF）、MCP resource 的 blob 与 audio、
// OpenAI file / input_file 的 file_data，以及 Interactions 带 data 与 mime_type 的 image、audio、document 内容块：
// 原先这些按 JSON 文本发送，base64 会让输入 token 暴涨
func functionResultMediaPart(raw json.RawMessage) (aistudio.Part, bool, error) {
	var block struct {
		Type     string          `json:"type"`
		ImageURL json.RawMessage `json:"image_url"`
		Data     string          `json:"data"`
		MimeType string          `json:"mimeType"`
		// MimeTypeSnake 为 Interactions 内容块的写法 {"type":"image","data":...,"mime_type":...}
		MimeTypeSnake string `json:"mime_type"`
		Source        *struct {
			Type      string `json:"type"`
			MediaType string `json:"media_type"`
			Data      string `json:"data"`
		} `json:"source"`
		Resource *struct {
			MimeType string `json:"mimeType"`
			Blob     string `json:"blob"`
			URI      string `json:"uri"`
		} `json:"resource"`
		Filename string `json:"filename"`
		FileData string `json:"file_data"`
		File     *struct {
			Filename string `json:"filename"`
			FileData string `json:"file_data"`
		} `json:"file"`
	}
	if err := json.Unmarshal(raw, &block); err != nil {
		return aistudio.Part{}, false, nil
	}
	block.MimeType = firstNonEmpty(block.MimeType, block.MimeTypeSnake)
	switch block.Type {
	case "image_url", "input_image":
		url, err := imageURLString(block.ImageURL)
		if err != nil || !strings.HasPrefix(url, "data:") {
			return aistudio.Part{}, false, nil
		}
		part, err := fileOrInlinePart(url, "")
		if err != nil {
			return aistudio.Part{}, false, fmt.Errorf("工具结果中的图片: %w", err)
		}
		return part, true, nil
	case "image":
		encoded, mimeType := block.Data, block.MimeType
		if block.Source != nil {
			if block.Source.Type != "base64" {
				return aistudio.Part{}, false, nil
			}
			encoded, mimeType = block.Source.Data, block.Source.MediaType
		}
		if encoded == "" {
			return aistudio.Part{}, false, nil
		}
		data, err := decodeBase64Flexible(encoded)
		if err != nil {
			return aistudio.Part{}, false, fmt.Errorf("工具结果中的图片: %w", err)
		}
		mimeType, data = normalizeImagePayload(mimeType, data)
		return aistudio.Part{InlineData: &aistudio.Blob{MIME: mimeType, Data: data}}, true, nil
	case "document":
		// Interactions document 内容块直接带 data 与 mime_type
		if block.Source == nil && block.Data != "" && block.MimeType != "" {
			return inlineToolMedia(block.Data, block.MimeType)
		}
		// Anthropic document：只取 base64 来源；text、content 等来源本来就是文字，保持原样
		if block.Source == nil || block.Source.Type != "base64" || block.Source.Data == "" {
			return aistudio.Part{}, false, nil
		}
		return inlineToolMedia(block.Source.Data, firstNonEmpty(block.Source.MediaType, "application/pdf"))
	case "audio":
		if block.Data == "" || block.MimeType == "" {
			return aistudio.Part{}, false, nil
		}
		return inlineToolMedia(block.Data, block.MimeType)
	case "resource":
		if block.Resource == nil || block.Resource.Blob == "" {
			return aistudio.Part{}, false, nil
		}
		return inlineToolMedia(block.Resource.Blob, firstNonEmpty(block.Resource.MimeType, mimeForFilename(block.Resource.URI, "application/octet-stream")))
	case "file", "input_file":
		filename, fileData := block.Filename, block.FileData
		if block.File != nil {
			filename, fileData = firstNonEmpty(block.File.Filename, filename), firstNonEmpty(block.File.FileData, fileData)
		}
		if fileData == "" {
			return aistudio.Part{}, false, nil
		}
		part, err := fileDataPart(fileData, filename)
		if err != nil {
			return aistudio.Part{}, false, fmt.Errorf("工具结果中的文件: %w", err)
		}
		return part, part.InlineData != nil, nil
	}
	return aistudio.Part{}, false, nil
}

// inlineToolMedia 解码工具结果中的 base64 内容
func inlineToolMedia(encoded string, mimeType string) (aistudio.Part, bool, error) {
	data, err := decodeBase64Flexible(encoded)
	if err != nil {
		return aistudio.Part{}, false, fmt.Errorf("工具结果中的文件: %w", err)
	}
	if strings.HasPrefix(mimeType, "image/") {
		mimeType, data = normalizeImagePayload(mimeType, data)
	}
	return aistudio.Part{InlineData: &aistudio.Blob{MIME: mimeType, Data: data}}, true, nil
}

func normalizeFunctionResultContent(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 {
		raw = json.RawMessage("null")
	}
	if !json.Valid(raw) {
		return nil, fmt.Errorf("function result must be JSON")
	}
	trimmed := strings.TrimSpace(string(raw))
	if strings.HasPrefix(trimmed, "{") {
		return raw, nil
	}
	return json.RawMessage(`{"result":` + trimmed + `}`), nil
}

func renderMediaMarkdown(media aistudio.Media) string {
	url := media.URL
	if len(media.Data) > 0 {
		url = "data:" + media.MIME + ";base64," + base64.StdEncoding.EncodeToString(media.Data)
	}
	label := media.Name
	if label == "" {
		label = "media"
	}
	if strings.HasPrefix(media.MIME, "image/") {
		return fmt.Sprintf("![%s](%s)", label, url)
	}
	return fmt.Sprintf("[%s](%s)", label, url)
}

func groundingCitations(metadata aistudio.GroundingMetadata) []aistudio.Citation {
	citations := make([]aistudio.Citation, 0)
	if len(metadata.Supports) == 0 {
		for _, chunk := range metadata.Chunks {
			if chunk.URI != "" {
				citations = append(citations, aistudio.Citation{URL: chunk.URI, Title: chunk.Title, Publisher: chunk.Source})
			}
		}
		return citations
	}
	for _, support := range metadata.Supports {
		for _, index := range support.ChunkIndices {
			if index < 0 || index >= len(metadata.Chunks) {
				continue
			}
			chunk := metadata.Chunks[index]
			if chunk.URI == "" {
				continue
			}
			citations = append(citations, aistudio.Citation{
				URL: chunk.URI, Title: chunk.Title, Publisher: chunk.Source,
				Start: support.Segment.StartIndex, End: support.Segment.EndIndex,
			})
		}
	}
	return citations
}

func renderCodeExecution(event aistudio.Event) string {
	switch event.Kind {
	case aistudio.EventExecutableCode:
		if event.ExecutableCode == nil {
			return ""
		}
		language := strings.ToLower(event.ExecutableCode.Language)
		if language == "language_unspecified" {
			language = "text"
		}
		return "```" + language + "\n" + strings.TrimSuffix(event.ExecutableCode.Code, "\n") + "\n```"
	case aistudio.EventCodeExecutionResult:
		if event.CodeExecutionResult == nil {
			return ""
		}
		value := event.CodeExecutionResult.Output
		if event.CodeExecutionResult.Outcome != "OUTCOME_OK" {
			value = event.CodeExecutionResult.Error
		}
		if value == "" {
			return ""
		}
		return "```text\n" + strings.TrimSuffix(value, "\n") + "\n```"
	default:
		return ""
	}
}

func renderCitationsMarkdown(citations []aistudio.Citation) string {
	if len(citations) == 0 {
		return ""
	}
	var output strings.Builder
	output.WriteString("Sources:\n")
	for _, citation := range citations {
		label := citation.Title
		if label == "" {
			label = citation.URL
		}
		fmt.Fprintf(&output, "- [%s](%s)\n", label, citation.URL)
	}
	return strings.TrimSuffix(output.String(), "\n")
}
