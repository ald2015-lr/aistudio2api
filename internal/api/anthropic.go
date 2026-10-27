package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

type anthropicRequest struct {
	Model         string             `json:"model"`
	Messages      []anthropicMessage `json:"messages"`
	System        json.RawMessage    `json:"system"`
	MaxTokens     *int64             `json:"max_tokens"`
	StopSequences []string           `json:"stop_sequences"`
	Stream        bool               `json:"stream"`
	Temperature   *float64           `json:"temperature"`
	TopP          *float64           `json:"top_p"`
	TopK          *int               `json:"top_k"`
	Tools         []anthropicTool    `json:"tools"`
	ToolChoice    json.RawMessage    `json:"tool_choice"`
	Thinking      *struct {
		Type         string `json:"type"`
		BudgetTokens *int64 `json:"budget_tokens"`
	} `json:"thinking"`
	OutputConfig *struct {
		Effort string                 `json:"effort"`
		Format *anthropicOutputFormat `json:"format"`
	} `json:"output_config"`
	// OutputFormat 为结构化输出的 beta 写法，与 output_config.format 同时出现时以后者为准
	OutputFormat *anthropicOutputFormat `json:"output_format"`
	// VideoConfig 为 Omni 视频设置扩展字段，与 OpenAI Chat 的顶层 video_config 相同，见 omni_video.go
	VideoConfig *videoConfigRequest `json:"video_config"`
}

// anthropicOutputFormat 为 output_config.format 与 output_format 的结构化输出格式
type anthropicOutputFormat struct {
	Type   string          `json:"type"`
	Schema json.RawMessage `json:"schema"`
}

type anthropicMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type anthropicTool struct {
	Type        string                     `json:"type"`
	Name        string                     `json:"name"`
	Description string                     `json:"description"`
	InputSchema json.RawMessage            `json:"input_schema"`
	Options     map[string]json.RawMessage `json:"-"`
}

func (tool *anthropicTool) UnmarshalJSON(data []byte) error {
	type knownTool anthropicTool
	var known knownTool
	if err := json.Unmarshal(data, &known); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	delete(fields, "type")
	delete(fields, "name")
	delete(fields, "description")
	delete(fields, "input_schema")
	*tool = anthropicTool(known)
	tool.Options = fields
	return nil
}

type anthropicContentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	Thinking  *string         `json:"thinking,omitempty"`
	Data      string          `json:"data,omitempty"`
	Signature string          `json:"signature,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
}

func (s *server) handleAnthropicMessages(w http.ResponseWriter, r *http.Request) {
	var request anthropicRequest
	if err := decodeJSON(r, &request); err != nil {
		writeAnthropicInvalid(w, err)
		return
	}
	if request.Model == "" || len(request.Messages) == 0 || request.MaxTokens == nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "model, messages and max_tokens are required")
		return
	}
	messageID := newID("msg")
	if err := s.decodeAnthropicSearchHistory(&request); err != nil {
		writeAnthropicInvalid(w, err)
		return
	}
	generateRequest, err := request.toGenerateRequest(messageID)
	if err != nil {
		writeAnthropicInvalid(w, err)
		return
	}
	// 回传历史里本服务的媒体链接恢复为媒体输入（见 restoreMediaLinks）
	s.restoreMediaLinks(generateRequest.Contents)
	hideThought, err := s.prepareGenerate(r.Context(), &generateRequest)
	if err != nil {
		writeAnthropicInvalid(w, err)
		return
	}
	// 响应里的 model 报告去掉后缀的目录模型名（见 responseModel）
	request.Model = responseModel(generateRequest)
	generateRequest.Stream = request.Stream
	if request.Stream {
		// 先拿到事件流再写响应头与 message_start：降级判定（流式严格模式）要在判定之前保留返回 400 的机会；
		// 生成服务在此之前失败时也和官方 API 一样直接返回 HTTP 错误
		inputTokens := aistudio.EstimatedInputTokens(generateRequest)
		events, err := s.generate(r.Context(), generateRequest)
		if err != nil {
			if shouldWriteRequestError(r, err) {
				writeAnthropicRequestError(w, err)
			}
			return
		}
		events = withHiddenReasoning(r.Context(), events, hideThought)
		if err := streamHeaders(w); err != nil {
			return
		}
		writer := &anthropicStreamWriter{
			w: w, id: messageID, model: request.Model,
			inputTokens: inputTokens,
			searchKey:   s.config.currentAPIKey(),
			media:       s.mediaOutputFor(r),
		}
		if err := writer.start(); err != nil {
			return
		}
		s.streamAnthropic(r, writer, events)
		return
	}
	events, err := s.service.Generate(r.Context(), generateRequest)
	if err != nil {
		if shouldWriteRequestError(r, err) {
			writeAnthropicRequestError(w, err)
		}
		return
	}
	events = withHiddenReasoning(r.Context(), events, hideThought)
	result, err := consumeEvents(r.Context(), events, nil)
	if err != nil {
		if shouldWriteRequestError(r, err) {
			writeAnthropicRequestError(w, err)
		}
		return
	}
	writeJSON(w, http.StatusOK, buildAnthropicResponse(messageID, request.Model, result, s.config.currentAPIKey(), s.mediaOutputFor(r)))
}

func (s *server) handleAnthropicCountTokens(w http.ResponseWriter, r *http.Request) {
	var request anthropicRequest
	if err := decodeJSON(r, &request); err != nil {
		writeAnthropicInvalid(w, err)
		return
	}
	if request.Model == "" || len(request.Messages) == 0 {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "model and messages are required")
		return
	}
	if err := s.decodeAnthropicSearchHistory(&request); err != nil {
		writeAnthropicInvalid(w, err)
		return
	}
	generateRequest, err := request.toGenerateRequest(newID("count"))
	if err != nil {
		writeAnthropicInvalid(w, err)
		return
	}
	s.restoreMediaLinks(generateRequest.Contents)
	// 与生成请求一样整理对话轮次（包括把工具结果中的图片移到之后的用户消息），计数与实际发送的内容一致
	contents, _ := normalizeTurns(generateRequest.Contents)
	count, err := s.service.CountTokens(r.Context(), aistudio.TokenCountRequest{
		Model: s.resolveModelName(r.Context(), generateRequest.Model), System: generateRequest.System, Contents: contents,
		Tools: generateRequest.Tools,
	})
	if err != nil {
		if shouldWriteRequestError(r, err) {
			writeAnthropicRequestError(w, err)
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]int64{"input_tokens": count.InputTokens})
}

func (request anthropicRequest) toGenerateRequest(id string) (aistudio.GenerateRequest, error) {
	if request.Thinking != nil {
		switch request.Thinking.Type {
		case "enabled":
			if request.Thinking.BudgetTokens == nil {
				return aistudio.GenerateRequest{}, fmt.Errorf("thinking.budget_tokens is required when thinking.type is enabled")
			}
		case "adaptive":
		case "disabled":
		default:
			return aistudio.GenerateRequest{}, fmt.Errorf("thinking.type must be enabled, adaptive or disabled")
		}
	}
	system, err := anthropicSystemText(request.System)
	if err != nil {
		return aistudio.GenerateRequest{}, err
	}
	contents := make([]aistudio.Content, 0, len(request.Messages))
	for _, message := range request.Messages {
		if message.Role == "system" {
			text, err := anthropicSystemText(message.Content)
			if err != nil {
				return aistudio.GenerateRequest{}, fmt.Errorf("system message: %w", err)
			}
			if strings.TrimSpace(text) != "" {
				contents = append(contents, aistudio.Content{Role: aistudio.RoleUser, Parts: []aistudio.Part{{Text: "<system-reminder>\n" + text + "\n</system-reminder>"}}})
			}
			continue
		}
		role, err := anthropicRole(message.Role)
		if err != nil {
			return aistudio.GenerateRequest{}, err
		}
		parts, err := anthropicParts(message.Content)
		if err != nil {
			return aistudio.GenerateRequest{}, fmt.Errorf("%s message: %w", message.Role, err)
		}
		if len(parts) == 0 {
			continue
		}
		contents = append(contents, aistudio.Content{Role: role, Parts: parts})
	}
	tools, err := mapAnthropicTools(request.Tools, request.ToolChoice)
	if err != nil {
		return aistudio.GenerateRequest{}, err
	}
	config := aistudio.GenerationConfig{
		Temperature:     request.Temperature,
		TopP:            request.TopP,
		TopK:            request.TopK,
		MaxOutputTokens: request.MaxTokens,
		StopSequences:   normalizeStopSequences(request.StopSequences),
		VideoConfig:     request.VideoConfig.videoConfig(),
	}
	if request.Thinking != nil && request.Thinking.Type == "enabled" {
		config.ThinkingBudget = request.Thinking.BudgetTokens
	}
	if request.OutputConfig != nil && strings.TrimSpace(request.OutputConfig.Effort) != "" {
		config.ReasoningEffort = request.OutputConfig.Effort
	}
	if err := applyAnthropicOutputFormat(&config, request.outputFormat()); err != nil {
		return aistudio.GenerateRequest{}, err
	}
	// 关闭思考：AI Studio 的思考模型不能完全关闭（只支持思考预算的模型预算为 0 会被拒绝），
	// 按最低强度生成，不返回思考正文（保留签名）；优先于 output_config.effort
	if request.Thinking != nil && request.Thinking.Type == "disabled" {
		config.ReasoningEffort = "minimal"
		config.ThinkingBudget = nil
		config.HideThinking = true
	}
	return aistudio.GenerateRequest{
		ID: id, Model: request.Model, System: system, Contents: contents, Config: config, Tools: tools,
	}, nil
}

// outputFormat 返回请求的结构化输出格式：output_config.format 优先，其次 beta 写法 output_format
func (request anthropicRequest) outputFormat() *anthropicOutputFormat {
	if request.OutputConfig != nil && request.OutputConfig.Format != nil {
		return request.OutputConfig.Format
	}
	return request.OutputFormat
}

// applyAnthropicOutputFormat 把 json_schema 结构化输出映射为与 OpenAI response_format、Gemini responseSchema 相同的
// JSON 模式与 responseSchema（Schema 无法直接编码时同样降级，仍按 JSON 输出）。原先这两个字段被静默忽略，客户端拿到自由文本
func applyAnthropicOutputFormat(config *aistudio.GenerationConfig, format *anthropicOutputFormat) error {
	if format == nil {
		return nil
	}
	if format.Type != "json_schema" {
		return fmt.Errorf("unsupported output format type %q; expected json_schema", format.Type)
	}
	config.ResponseMIMEType = "application/json"
	// schema 为 null 或省略时只保留 JSON 模式
	if geminiRawObjectPresent(format.Schema) {
		config.ResponseSchema = format.Schema
	}
	return nil
}

func anthropicSystemText(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text, nil
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return "", fmt.Errorf("system must be a string or text block array")
	}
	texts := make([]string, 0, len(blocks))
	for _, block := range blocks {
		if block.Type != "text" {
			return "", fmt.Errorf("unsupported system block type %q", block.Type)
		}
		if block.Text != "" {
			texts = append(texts, block.Text)
		}
	}
	return strings.Join(texts, "\n"), nil
}

func anthropicRole(role string) (aistudio.Role, error) {
	switch role {
	case "user":
		return aistudio.RoleUser, nil
	case "assistant":
		return aistudio.RoleAssistant, nil
	default:
		return "", fmt.Errorf("unsupported message role %q", role)
	}
}

// anthropicParts 转换消息块并保留工具调用的思考签名
func anthropicParts(raw json.RawMessage) ([]aistudio.Part, error) {
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		if text == "" {
			return nil, nil
		}
		return []aistudio.Part{{Text: text}}, nil
	}
	var blocks []json.RawMessage
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil, fmt.Errorf("content must be a string or block array")
	}
	parts := make([]aistudio.Part, 0, len(blocks))
	pendingSignature := ""
	flushPendingSignature := func() {
		if pendingSignature == "" {
			return
		}
		parts = append(parts, aistudio.Part{ThoughtSignature: pendingSignature})
		pendingSignature = ""
	}
	for _, rawBlock := range blocks {
		var block struct {
			Type      string          `json:"type"`
			Text      string          `json:"text"`
			Data      string          `json:"data"`
			ID        string          `json:"id"`
			Name      string          `json:"name"`
			Input     json.RawMessage `json:"input"`
			ToolUseID string          `json:"tool_use_id"`
			Content   json.RawMessage `json:"content"`
			IsError   bool            `json:"is_error"`
			Signature string          `json:"signature"`
			Source    json.RawMessage `json:"source"`
			Title     string          `json:"title"`
		}
		if err := json.Unmarshal(rawBlock, &block); err != nil {
			return nil, err
		}
		switch block.Type {
		case "text":
			if block.Text == "" {
				continue
			}
			flushPendingSignature()
			parts = append(parts, aistudio.Part{Text: block.Text})
		case "thinking":
			flushPendingSignature()
			pendingSignature = block.Signature
		case "redacted_thinking":
			flushPendingSignature()
			if block.Data == "" {
				return nil, fmt.Errorf("redacted_thinking data is required")
			}
			pendingSignature = block.Data
		case "image", "document":
			flushPendingSignature()
			media, err := anthropicSourceParts(block.Type, block.Source)
			if err != nil {
				return nil, err
			}
			parts = append(parts, media...)
		case "tool_use":
			input := block.Input
			if len(input) == 0 {
				input = json.RawMessage(`{}`)
			}
			parts = append(parts, aistudio.Part{FunctionCall: &aistudio.FunctionCall{
				ID: block.ID, Name: block.Name, Arguments: input, ThoughtSignature: pendingSignature,
			}})
			pendingSignature = ""
		case "tool_result":
			flushPendingSignature()
			content := block.Content
			if block.IsError {
				if len(content) == 0 {
					content = json.RawMessage("null")
				}
				if !json.Valid(content) {
					return nil, fmt.Errorf("tool_result: function result must be JSON")
				}
				content = json.RawMessage(`{"error":` + strings.TrimSpace(string(content)) + `}`)
			}
			var media []aistudio.Part
			if !block.IsError {
				cleaned, extracted, err := splitFunctionResultMedia(content)
				if err != nil {
					return nil, fmt.Errorf("tool_result: %w", err)
				}
				media = extracted
				content, err = normalizeFunctionResultContent(cleaned)
				if err != nil {
					return nil, fmt.Errorf("tool_result: %w", err)
				}
			}
			parts = append(parts, aistudio.Part{FunctionResult: &aistudio.FunctionResult{
				ID: block.ToolUseID, Content: content,
			}})
			parts = append(parts, media...)
		case "search_result":
			// 客户端提供的搜索结果块：来源与标题作为文本，正文按规范只含 text 与 image 块
			flushPendingSignature()
			var source string
			_ = json.Unmarshal(block.Source, &source)
			nested, err := anthropicInnerParts(block.Content)
			if err != nil {
				return nil, fmt.Errorf("search_result content: %w", err)
			}
			parts = append(parts, aistudio.Part{Text: "Search result: " + block.Title + "\n" + source})
			parts = append(parts, nested...)
		case "server_tool_use", "web_fetch_tool_result", "code_execution_tool_result", "bash_code_execution_tool_result",
			"text_editor_code_execution_tool_result", "tool_search_tool_result":
			// 其他服务端工具（web_fetch、code_execution、tool_search 等）的历史调用与结果：上游没有对应的历史格式，
			// 转成文本保留上下文（web_search 历史已由 decodeAnthropicSearchHistory 改写）。会话从别的 Claude 渠道切过来时常带这些块
			flushPendingSignature()
			history, err := anthropicServerToolHistoryParts(block.Type, block.Name, block.Input, block.Content)
			if err != nil {
				return nil, err
			}
			parts = append(parts, history...)
		default:
			return nil, fmt.Errorf("unsupported content block type %q", block.Type)
		}
	}
	flushPendingSignature()
	return parts, nil
}

// anthropicSourceParts 转换 image / document 块的 source：两种块都接受 base64、url 与 file（file_id 按已上传文件引用，
// 与 OpenAI 的 file_id 相同）；document 另外接受 text（纯文本文档）与 content（由内容块组成的文档）
func anthropicSourceParts(blockType string, raw json.RawMessage) ([]aistudio.Part, error) {
	if !rawJSONConfigured(raw) {
		return nil, fmt.Errorf("%s source is required", blockType)
	}
	var source struct {
		Type      string          `json:"type"`
		MediaType string          `json:"media_type"`
		Data      string          `json:"data"`
		URL       string          `json:"url"`
		FileID    string          `json:"file_id"`
		Content   json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(raw, &source); err != nil {
		return nil, fmt.Errorf("%s source: %w", blockType, err)
	}
	switch {
	case source.Type == "base64":
		data, err := decodeBase64Flexible(source.Data)
		if err != nil {
			return nil, fmt.Errorf("%s source data: %w", blockType, err)
		}
		mediaType, data := normalizeImagePayload(source.MediaType, data)
		return []aistudio.Part{{InlineData: &aistudio.Blob{MIME: mediaType, Data: data}}}, nil
	case source.Type == "url":
		if media, ok := aistudio.ExternalMediaForURL(source.URL); ok {
			return []aistudio.Part{{ExternalMedia: media}}, nil
		}
		mime := source.MediaType
		if mime == "" && blockType == "image" {
			mime = "image/*"
		}
		if mime == "" {
			mime = "application/pdf"
		}
		if strings.TrimSpace(source.URL) == "" {
			return nil, fmt.Errorf("%s source URL is required", blockType)
		}
		return []aistudio.Part{{ExternalMedia: &aistudio.ExternalMedia{MIME: mime, URL: source.URL}}}, nil
	case source.Type == "file":
		if strings.TrimSpace(source.FileID) == "" {
			return nil, fmt.Errorf("%s source file_id is required", blockType)
		}
		return []aistudio.Part{{File: &aistudio.FileRef{ID: strings.TrimSpace(source.FileID), MIME: source.MediaType}}}, nil
	case source.Type == "text" && blockType == "document":
		if source.Data == "" {
			return nil, nil
		}
		return []aistudio.Part{{Text: source.Data}}, nil
	case source.Type == "content" && blockType == "document":
		nested, err := anthropicInnerParts(source.Content)
		if err != nil {
			return nil, fmt.Errorf("document source content: %w", err)
		}
		return nested, nil
	default:
		return nil, fmt.Errorf("unsupported source type %q", source.Type)
	}
}

// anthropicInnerParts 转换嵌在其他块里的内容（search_result 的 content、document 的 content source）：按 Anthropic 规范
// 只含 text 与 image 块，字符串按文本处理，其他块返回错误。这里不调用 anthropicParts：嵌套块若再递归转换，
// 每层都要重新解析并复制剩余的 JSON，几千层嵌套、约 300 KB 的请求体就要耗费数 GB 内存与数十秒；
// image 的 source 不再含内容块，整个转换最多只解析固定的几层
func anthropicInnerParts(raw json.RawMessage) ([]aistudio.Part, error) {
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		if text == "" {
			return nil, nil
		}
		return []aistudio.Part{{Text: text}}, nil
	}
	var blocks []struct {
		Type   string          `json:"type"`
		Text   string          `json:"text"`
		Source json.RawMessage `json:"source"`
	}
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil, fmt.Errorf("content must be a string or block array")
	}
	parts := make([]aistudio.Part, 0, len(blocks))
	for _, block := range blocks {
		switch block.Type {
		case "text":
			if block.Text != "" {
				parts = append(parts, aistudio.Part{Text: block.Text})
			}
		case "image":
			media, err := anthropicSourceParts("image", block.Source)
			if err != nil {
				return nil, err
			}
			parts = append(parts, media...)
		default:
			return nil, fmt.Errorf("nested content only accepts text and image blocks, got %q", block.Type)
		}
	}
	return parts, nil
}

// anthropicWebFetchDocument 转换 web_fetch 结果里的文档：按规范是单个 document 块，按 document 的 source 转换，
// 不经 anthropicParts 递归（原因见 anthropicInnerParts）
func anthropicWebFetchDocument(raw json.RawMessage) ([]aistudio.Part, error) {
	var document struct {
		Type   string          `json:"type"`
		Source json.RawMessage `json:"source"`
	}
	if err := json.Unmarshal(raw, &document); err != nil {
		return nil, err
	}
	if document.Type != "document" {
		return nil, fmt.Errorf("content must be a document block, got %q", document.Type)
	}
	return anthropicSourceParts("document", document.Source)
}

// anthropicServerToolHistoryParts 把服务端工具的历史调用或结果渲染为文本；web_fetch 结果里的文档照常转换（文本与 PDF 等附件），
// 抓取失败时写明 error_code
func anthropicServerToolHistoryParts(blockType string, name string, input json.RawMessage, content json.RawMessage) ([]aistudio.Part, error) {
	switch blockType {
	case "server_tool_use":
		return []aistudio.Part{{Text: "Server tool call " + name + ": " + string(input)}}, nil
	case "web_fetch_tool_result":
		var result struct {
			URL       string          `json:"url"`
			ErrorCode string          `json:"error_code"`
			Content   json.RawMessage `json:"content"`
		}
		if err := json.Unmarshal(content, &result); err != nil {
			return nil, fmt.Errorf("web_fetch_tool_result content: %w", err)
		}
		if result.ErrorCode != "" {
			return []aistudio.Part{{Text: "Web fetch failed: " + result.ErrorCode}}, nil
		}
		parts := []aistudio.Part{{Text: "Web fetch: " + result.URL}}
		if rawJSONConfigured(result.Content) {
			document, err := anthropicWebFetchDocument(result.Content)
			if err != nil {
				return nil, fmt.Errorf("web_fetch_tool_result content: %w", err)
			}
			parts = append(parts, document...)
		}
		return parts, nil
	default:
		return []aistudio.Part{{Text: "Server tool result " + blockType + ": " + string(content)}}, nil
	}
}

func mapAnthropicTools(tools []anthropicTool, choice json.RawMessage) (aistudio.Tools, error) {
	var mapped aistudio.Tools
	for _, tool := range tools {
		typeName := strings.ToLower(tool.Type)
		// 服务端工具按类型前缀识别，web_search_*、web_fetch_*、code_execution_* 的任何日期版本都接受：
		// Anthropic 会定期发布新版本（如 web_search_20260209），原先只认几个固定日期，其他版本一律返回 400
		switch {
		case strings.HasPrefix(typeName, "web_search_"):
			if err := validateAnthropicServerTool(tool, "web_search"); err != nil {
				return aistudio.Tools{}, err
			}
			// max_uses：搜索次数由上游决定，没有对应参数，忽略。
			// allowed_domains、blocked_domains（site: / -site: 查询）与 user_location 以系统指令提示传给上游（软约束）
			location := tool.Options["user_location"]
			allowed, blocked := tool.Options["allowed_domains"], tool.Options["blocked_domains"]
			if rawJSONConfigured(location) || rawJSONConfigured(allowed) || rawJSONConfigured(blocked) {
				filters, _ := json.Marshal(map[string]json.RawMessage{"allowed_domains": allowed, "blocked_domains": blocked})
				search, err := mapSearchOptions("", location, filters)
				if err != nil {
					return aistudio.Tools{}, err
				}
				mapped.GoogleSearch = search
			}
			mapped.Google = appendUnique(mapped.Google, "google_search")
		case typeName == "image_search":
			if err := validateAnthropicServerTool(tool, "image_search"); err != nil {
				return aistudio.Tools{}, err
			}
			mapped.Google = appendUnique(mapped.Google, "image_search")
		case strings.HasPrefix(typeName, "web_fetch_"):
			if err := validateAnthropicServerTool(tool, "web_fetch"); err != nil {
				return aistudio.Tools{}, err
			}
			// allowed_domains、blocked_domains 是客户端显式限定的抓取范围，URL Context 没有对应参数，
			// 忽略会悄悄放开限制，所以返回 400；max_uses、citations、max_content_tokens 上游没有对应参数，忽略
			for _, field := range []string{"allowed_domains", "blocked_domains"} {
				domains, err := searchDomainList(tool.Options[field], field)
				if err != nil {
					return aistudio.Tools{}, err
				}
				if len(domains) > 0 {
					return aistudio.Tools{}, fmt.Errorf("tool type %q option %q is not supported", tool.Type, field)
				}
			}
			mapped.Google = appendUnique(mapped.Google, "url_context")
		case strings.HasPrefix(typeName, "code_execution_"):
			if err := validateAnthropicServerTool(tool, "code_execution"); err != nil {
				return aistudio.Tools{}, err
			}
			mapped.Google = appendUnique(mapped.Google, "code_execution")
		case strings.HasPrefix(typeName, "tool_search_tool_"), strings.Contains(typeName, "toolset"):
			// 工具搜索（tool_search_tool_regex_* / _bm25_*）只决定 Anthropic 侧按需加载哪些工具，MCP toolset 由 Anthropic 连接
			// 远端 MCP 服务器，上游都没有对应能力，忽略；被延迟加载的函数照常全部声明
		case typeName == "url_context":
			if err := validateAnthropicServerTool(tool, "url_context"); err != nil {
				return aistudio.Tools{}, err
			}
			mapped.Google = appendUnique(mapped.Google, "url_context")
		case typeName == "google_maps":
			if err := validateAnthropicServerTool(tool, "google_maps"); err != nil {
				return aistudio.Tools{}, err
			}
			mapped.Google = appendUnique(mapped.Google, "google_maps")
		case typeName == "", typeName == "custom":
			if tool.Name == "" {
				return aistudio.Tools{}, fmt.Errorf("tool name is required")
			}
			// cache_control、defer_loading、input_examples 等只影响 Anthropic 自身的缓存与加载，忽略即可；strict 按参数校验处理
			var strict bool
			if raw := tool.Options["strict"]; rawJSONConfigured(raw) {
				if err := json.Unmarshal(raw, &strict); err != nil {
					return aistudio.Tools{}, fmt.Errorf("tool strict must be a boolean")
				}
			}
			parameters := tool.InputSchema
			if len(parameters) == 0 {
				parameters = json.RawMessage(`{"type":"object","properties":{}}`)
			}
			mapped.Functions = append(mapped.Functions, aistudio.FunctionDeclaration{
				Name: tool.Name, Description: tool.Description, Parameters: parameters, Strict: strict,
			})
		default:
			return aistudio.Tools{}, fmt.Errorf("unsupported tool type %q", tool.Type)
		}
	}
	config, err := anthropicToolChoice(choice)
	if err != nil {
		return aistudio.Tools{}, err
	}
	mapped.ToolConfig = config
	return mapped, nil
}

// validateAnthropicServerTool 校验 server tool 的 name 与不应出现的 custom tool 字段。其他字段（cache_control、
// defer_loading 等只影响 Anthropic 自身缓存与加载的字段，以及未知字段）与 custom tool 一样忽略，各 type 的文档化字段由调用方处理
func validateAnthropicServerTool(tool anthropicTool, name string) error {
	if tool.Name != name {
		return fmt.Errorf("tool type %q requires name %q", tool.Type, name)
	}
	if tool.Description != "" || rawJSONConfigured(tool.InputSchema) {
		return fmt.Errorf("tool type %q does not accept description or input_schema", tool.Type)
	}
	return nil
}

func anthropicToolChoice(raw json.RawMessage) (aistudio.ToolConfig, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return aistudio.ToolConfig{Mode: "auto"}, nil
	}
	var choice struct {
		Type            string `json:"type"`
		Name            string `json:"name"`
		DisableParallel *bool  `json:"disable_parallel_tool_use"`
	}
	if err := json.Unmarshal(raw, &choice); err != nil {
		return aistudio.ToolConfig{}, fmt.Errorf("invalid tool_choice: %w", err)
	}
	config := aistudio.ToolConfig{}
	if choice.DisableParallel != nil {
		parallel := !*choice.DisableParallel
		config.ParallelCalls = &parallel
	}
	switch choice.Type {
	case "auto":
		config.Mode = "auto"
	case "none":
		config.Mode = "none"
	case "any":
		config.Mode = "required"
	case "tool":
		if choice.Name == "" {
			return aistudio.ToolConfig{}, fmt.Errorf("tool_choice name is required")
		}
		config.Mode, config.AllowedFunctionNames = "required", []string{choice.Name}
	default:
		return aistudio.ToolConfig{}, fmt.Errorf("unsupported tool_choice type %q", choice.Type)
	}
	return config, nil
}

func buildAnthropicResponse(id string, model string, result generationResult, searchKey string, media mediaOutput) map[string]any {
	stopReason, stopSequence := anthropicStop(result.finishReason, len(result.toolCalls) > 0, result.stopSequence)
	searchBlocks, searchCount := anthropicSearchBlocks(result.events, searchKey)
	if searchCount > 0 {
		result.citations = nil
	}
	response := map[string]any{
		"id":            id,
		"type":          "message",
		"role":          "assistant",
		"model":         model,
		"content":       append(anthropicBlocks(result, media), searchBlocks...),
		"stop_reason":   stopReason,
		"stop_sequence": stopSequence,
	}
	if result.providerModel != "" {
		response["provider_model"] = result.providerModel
	}
	if providerReason := providerFinishReason(result.finishReason); providerReason != "" {
		response["provider_finish_reason"] = providerReason
	}
	if result.usage != nil {
		response["usage"] = anthropicUsage(result.usage)
	}
	if searchCount > 0 {
		usage, ok := response["usage"].(map[string]any)
		if !ok {
			usage = map[string]any{"input_tokens": 0, "output_tokens": 0}
			response["usage"] = usage
		}
		usage["server_tool_use"] = map[string]int{"web_search_requests": searchCount, "web_fetch_requests": 0}
	}
	return response
}

func anthropicBlocks(result generationResult, media mediaOutput) []anthropicContentBlock {
	blocks := make([]anthropicContentBlock, 0)
	// pending 暂存正文块打开时单独到达的签名，正文块结束后再作为 redacted_thinking 输出，见 deferredSignatures
	var pending deferredSignatures
	flush := func() {
		for _, signature := range pending.take() {
			blocks = append(blocks, anthropicContentBlock{Type: "redacted_thinking", Data: signature})
		}
	}
	textOpen := func() bool { return len(blocks) > 0 && blocks[len(blocks)-1].Type == "text" }
	// openText 另起一个文本块，先输出暂存的签名
	openText := func(text string) {
		flush()
		blocks = append(blocks, anthropicContentBlock{Type: "text", Text: text})
	}
	for _, event := range result.events {
		switch event.Kind {
		case aistudio.EventText:
			if textOpen() {
				blocks[len(blocks)-1].Text += event.Text
			} else {
				openText(event.Text)
			}
		case aistudio.EventReasoning:
			flush()
			if len(blocks) > 0 && blocks[len(blocks)-1].Type == "thinking" {
				*blocks[len(blocks)-1].Thinking += event.Text
				if event.ThoughtSignature != "" {
					blocks[len(blocks)-1].Signature = event.ThoughtSignature
				}
			} else {
				thinking := event.Text
				blocks = append(blocks, anthropicContentBlock{Type: "thinking", Thinking: &thinking, Signature: event.ThoughtSignature})
			}
		case aistudio.EventThoughtSignature:
			if event.ThoughtSignature == "" {
				continue
			}
			if textOpen() {
				pending.add(event.ThoughtSignature)
				continue
			}
			flush()
			blocks = append(blocks, anthropicContentBlock{Type: "redacted_thinking", Data: event.ThoughtSignature})
		case aistudio.EventToolCall:
			if event.ToolCall != nil {
				// 暂存的签名不能紧挨在不带签名的工具调用前面：回传时紧邻 tool_use 的 redacted_thinking 会成为该调用的签名。
				// 带签名的调用先输出暂存的签名，再输出它自己的签名；不带签名的调用之后再输出
				if event.ToolCall.ThoughtSignature != "" {
					flush()
				}
				if event.ToolCall.ThoughtSignature != "" {
					if len(blocks) > 0 && blocks[len(blocks)-1].Type == "thinking" {
						blocks[len(blocks)-1].Signature = event.ToolCall.ThoughtSignature
					} else {
						blocks = append(blocks, anthropicContentBlock{
							Type: "redacted_thinking", Data: event.ToolCall.ThoughtSignature,
						})
					}
				}
				blocks = append(blocks, anthropicContentBlock{
					Type: "tool_use", ID: event.ToolCall.ID, Name: event.ToolCall.Name, Input: event.ToolCall.Arguments,
				})
			}
		case aistudio.EventMedia:
			if event.Media != nil {
				openText(media.markdown(*event.Media))
			}
		case aistudio.EventExecutableCode, aistudio.EventCodeExecutionResult:
			rendered := renderCodeExecution(event)
			if rendered == "" {
				continue
			}
			if textOpen() {
				blocks[len(blocks)-1].Text += "\n" + rendered + "\n"
			} else {
				openText(rendered + "\n")
			}
		}
	}
	if sources := renderCitationsMarkdown(result.citations); sources != "" {
		if textOpen() {
			blocks[len(blocks)-1].Text += "\n\n" + sources
		} else {
			openText(sources)
		}
	}
	flush()
	return blocks
}

func anthropicStop(reason string, hasTools bool, stopSequence string) (string, *string) {
	normalized := strings.ToLower(strings.TrimSpace(reason))
	switch normalized {
	case "max_tokens", "max_output_tokens", "length":
		return "max_tokens", nil
	case "stop_sequence":
		if stopSequence != "" {
			return "stop_sequence", &stopSequence
		}
		return "stop_sequence", nil
	case "pause_turn":
		return "pause_turn", nil
	default:
		if finishFiltered(reason) {
			return "refusal", nil
		}
		if hasTools {
			return "tool_use", nil
		}
		return "end_turn", nil
	}
}

func anthropicUsage(usage *aistudio.Usage) map[string]any {
	return map[string]any{
		"input_tokens":  inputTokens(usage),
		"output_tokens": outputTokens(usage),
	}
}

func writeAnthropicModels(w http.ResponseWriter, models []aistudio.Model) {
	data := make([]map[string]any, 0, len(models))
	for _, model := range models {
		data = append(data, anthropicModelObject(model))
	}
	response := map[string]any{"data": data, "has_more": false, "first_id": nil, "last_id": nil}
	if len(models) > 0 {
		response["first_id"] = models[0].ID
		response["last_id"] = models[len(models)-1].ID
	}
	writeJSON(w, http.StatusOK, response)
}

func anthropicModelObject(model aistudio.Model) map[string]any {
	return map[string]any{"id": model.ID, "type": "model", "display_name": model.Name, "created_at": "1970-01-01T00:00:00Z"}
}

type anthropicStreamWriter struct {
	w                 http.ResponseWriter
	id                string
	model             string
	inputTokens       int64
	blockIndex        int
	currentBlock      string
	thinkingSignature string
	searchKey         string
	// pendingSignatures 暂存正文块打开时单独到达的签名，正文块关闭后再输出 redacted_thinking
	pendingSignatures deferredSignatures
	// media 为生成媒体的输出方式（data URI 或本服务链接）
	media mediaOutput
}

func (s *server) streamAnthropic(r *http.Request, writer *anthropicStreamWriter, events <-chan aistudio.Event) {
	result, err := consumeStreamEvents(r.Context(), events, writer.media.joinStreamAudio(writer.live), writer.heartbeat)
	if err != nil {
		if shouldWriteRequestError(r, err) {
			_ = writer.error(err)
		}
		return
	}
	_ = writer.finish(result)
}

func (writer *anthropicStreamWriter) emit(eventType string, payload any) error {
	return writeSSE(writer.w, eventType, payload)
}

// heartbeat 按官方格式发送 ping 事件保持连接：Anthropic SDK 识别并跳过 ping，new-api 按官方事件转发
func (writer *anthropicStreamWriter) heartbeat() error {
	return writer.emit("ping", map[string]string{"type": "ping"})
}

func (writer *anthropicStreamWriter) start() error {
	return writer.emit("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": writer.id, "type": "message", "role": "assistant", "model": writer.model,
			"content": []any{}, "stop_reason": nil, "stop_sequence": nil,
			"usage": map[string]int64{"input_tokens": writer.inputTokens, "output_tokens": 0},
		},
	})
}

func (writer *anthropicStreamWriter) live(event aistudio.Event) error {
	switch event.Kind {
	case aistudio.EventText:
		if err := writer.ensureBlock("text"); err != nil {
			return err
		}
		return writer.emit("content_block_delta", map[string]any{
			"type": "content_block_delta", "index": writer.blockIndex,
			"delta": map[string]any{"type": "text_delta", "text": event.Text},
		})
	case aistudio.EventReasoning:
		if err := writer.ensureBlock("thinking"); err != nil {
			return err
		}
		if event.ThoughtSignature != "" {
			writer.thinkingSignature = event.ThoughtSignature
		}
		return writer.emit("content_block_delta", map[string]any{
			"type": "content_block_delta", "index": writer.blockIndex,
			"delta": map[string]any{"type": "thinking_delta", "thinking": event.Text},
		})
	case aistudio.EventThoughtSignature:
		if event.ThoughtSignature == "" {
			return nil
		}
		// 与非流式一致：签名以 redacted_thinking 输出，随下一轮带回上游。正文块打开时先暂存，正文块关闭后再输出：
		// gemini-4 等模型在正文分片之间夹带只有签名的分片，立即输出会把一段正文切成许多个文本块，
		// 按块分段显示的客户端会在句子中间断行
		if writer.currentBlock == "text" {
			writer.pendingSignatures.add(event.ThoughtSignature)
			return nil
		}
		return writer.redactedThinking(event.ThoughtSignature)
	case aistudio.EventToolCall:
		if event.ToolCall == nil {
			return nil
		}
		call := event.ToolCall
		if call.ThoughtSignature != "" {
			if writer.currentBlock == "thinking" {
				writer.thinkingSignature = call.ThoughtSignature
			} else if err := writer.redactedThinking(call.ThoughtSignature); err != nil {
				return err
			}
		}
		if err := writer.closeBlock(); err != nil {
			return err
		}
		if err := writer.emit("content_block_start", map[string]any{
			"type": "content_block_start", "index": writer.blockIndex,
			"content_block": map[string]any{"type": "tool_use", "id": call.ID, "name": call.Name, "input": map[string]any{}},
		}); err != nil {
			return err
		}
		writer.currentBlock = "tool_use"
		if err := writer.emit("content_block_delta", map[string]any{
			"type": "content_block_delta", "index": writer.blockIndex,
			"delta": map[string]any{"type": "input_json_delta", "partial_json": string(call.Arguments)},
		}); err != nil {
			return err
		}
		return writer.closeBlock()
	case aistudio.EventMedia:
		if event.Media == nil {
			return nil
		}
		if err := writer.closeBlock(); err != nil {
			return err
		}
		if err := writer.ensureBlock("text"); err != nil {
			return err
		}
		return writer.emit("content_block_delta", map[string]any{
			"type": "content_block_delta", "index": writer.blockIndex,
			"delta": map[string]any{"type": "text_delta", "text": writer.media.markdown(*event.Media)},
		})
	case aistudio.EventExecutableCode, aistudio.EventCodeExecutionResult:
		rendered := renderCodeExecution(event)
		if rendered == "" {
			return nil
		}
		if err := writer.ensureBlock("text"); err != nil {
			return err
		}
		return writer.emit("content_block_delta", map[string]any{
			"type": "content_block_delta", "index": writer.blockIndex,
			"delta": map[string]any{"type": "text_delta", "text": "\n" + rendered + "\n"},
		})
	}
	return nil
}

func (writer *anthropicStreamWriter) ensureBlock(blockType string) error {
	if writer.currentBlock == blockType {
		return nil
	}
	if err := writer.closeBlock(); err != nil {
		return err
	}
	if err := writer.flushSignatures(); err != nil {
		return err
	}
	block := map[string]any{"type": blockType}
	if blockType == "thinking" {
		block["thinking"] = ""
	} else {
		block["text"] = ""
	}
	if err := writer.emit("content_block_start", map[string]any{
		"type": "content_block_start", "index": writer.blockIndex, "content_block": block,
	}); err != nil {
		return err
	}
	writer.currentBlock = blockType
	return nil
}

func (writer *anthropicStreamWriter) redactedThinking(data string) error {
	if err := writer.closeBlock(); err != nil {
		return err
	}
	if err := writer.flushSignatures(); err != nil {
		return err
	}
	return writer.redactedBlock(data)
}

// flushSignatures 输出正文中途暂存的签名，调用方保证当前没有打开的块。
// 不在关闭正文块时立即输出：随后是不带签名的工具调用时，紧邻 tool_use 的 redacted_thinking 在回传时会成为该调用的签名，
// 所以等到下一个块（正文、思考、redacted_thinking）开始前或输出结束时再输出
func (writer *anthropicStreamWriter) flushSignatures() error {
	for _, signature := range writer.pendingSignatures.take() {
		if err := writer.redactedBlock(signature); err != nil {
			return err
		}
	}
	return nil
}

// redactedBlock 输出一个 redacted_thinking 块，调用方保证当前没有打开的块
func (writer *anthropicStreamWriter) redactedBlock(data string) error {
	if err := writer.emit("content_block_start", map[string]any{
		"type": "content_block_start", "index": writer.blockIndex,
		"content_block": map[string]any{"type": "redacted_thinking", "data": data},
	}); err != nil {
		return err
	}
	if err := writer.emit("content_block_stop", map[string]any{
		"type": "content_block_stop", "index": writer.blockIndex,
	}); err != nil {
		return err
	}
	writer.blockIndex++
	return nil
}

func (writer *anthropicStreamWriter) closeBlock() error {
	if writer.currentBlock == "" {
		return nil
	}
	if writer.currentBlock == "thinking" && writer.thinkingSignature != "" {
		if err := writer.emit("content_block_delta", map[string]any{
			"type": "content_block_delta", "index": writer.blockIndex,
			"delta": map[string]any{"type": "signature_delta", "signature": writer.thinkingSignature},
		}); err != nil {
			return err
		}
	}
	if err := writer.emit("content_block_stop", map[string]any{
		"type": "content_block_stop", "index": writer.blockIndex,
	}); err != nil {
		return err
	}
	writer.blockIndex++
	writer.currentBlock = ""
	writer.thinkingSignature = ""
	return nil
}

// deferredSignatures 暂存一段正文中途单独到达的思考签名，正文结束后按到达顺序输出；与上一个相同的签名不重复保存
type deferredSignatures struct {
	values []string
}

func (pending *deferredSignatures) add(signature string) {
	if count := len(pending.values); count > 0 && pending.values[count-1] == signature {
		return
	}
	pending.values = append(pending.values, signature)
}

// take 返回暂存的签名并清空
func (pending *deferredSignatures) take() []string {
	values := pending.values
	pending.values = nil
	return values
}

func (writer *anthropicStreamWriter) finish(result generationResult) error {
	searchBlocks, searchCount := anthropicSearchBlocks(result.events, writer.searchKey)
	if searchCount > 0 {
		result.citations = nil
	}
	if sources := renderCitationsMarkdown(result.citations); sources != "" {
		prefix := ""
		if writer.currentBlock == "text" {
			prefix = "\n\n"
		}
		if err := writer.ensureBlock("text"); err != nil {
			return err
		}
		if err := writer.emit("content_block_delta", map[string]any{
			"type": "content_block_delta", "index": writer.blockIndex,
			"delta": map[string]any{"type": "text_delta", "text": prefix + sources},
		}); err != nil {
			return err
		}
	}
	if err := writer.closeBlock(); err != nil {
		return err
	}
	if err := writer.flushSignatures(); err != nil {
		return err
	}
	for _, block := range searchBlocks {
		if err := writer.emit("content_block_start", map[string]any{
			"type": "content_block_start", "index": writer.blockIndex, "content_block": block,
		}); err != nil {
			return err
		}
		writer.currentBlock = block.Type
		if err := writer.closeBlock(); err != nil {
			return err
		}
	}
	usage := map[string]any{"output_tokens": 0}
	if result.usage != nil {
		usage["input_tokens"] = inputTokens(result.usage)
		usage["output_tokens"] = outputTokens(result.usage)
	}
	if searchCount > 0 {
		usage["server_tool_use"] = map[string]int{"web_search_requests": searchCount, "web_fetch_requests": 0}
	}
	stopReason, stopSequence := anthropicStop(result.finishReason, len(result.toolCalls) > 0, result.stopSequence)
	delta := map[string]any{
		"stop_reason": stopReason, "stop_sequence": stopSequence,
	}
	if providerReason := providerFinishReason(result.finishReason); providerReason != "" {
		delta["provider_finish_reason"] = providerReason
	}
	if err := writer.emit("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": delta,
		"usage": usage,
	}); err != nil {
		return err
	}
	return writer.emit("message_stop", map[string]string{"type": "message_stop"})
}

func (writer *anthropicStreamWriter) error(err error) error {
	return writer.emit("error", anthropicStreamError(writer.w, err))
}
