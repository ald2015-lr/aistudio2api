package api

import (
	"encoding/json"
	"fmt"
	"mime"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

type chatRequest struct {
	Model               string            `json:"model"`
	Messages            []chatMessage     `json:"messages"`
	Stream              bool              `json:"stream"`
	StreamOptions       chatStreamOptions `json:"stream_options"`
	Tools               []openAITool      `json:"tools"`
	ToolChoice          json.RawMessage   `json:"tool_choice"`
	Temperature         *float64          `json:"temperature"`
	TopP                *float64          `json:"top_p"`
	MaxTokens           *int64            `json:"max_tokens"`
	MaxCompletionTokens *int64            `json:"max_completion_tokens"`
	N                   *int64            `json:"n"`
	ParallelToolCalls   *bool             `json:"parallel_tool_calls"`
	Stop                json.RawMessage   `json:"stop"`
	ResponseFormat      json.RawMessage   `json:"response_format"`
	ReasoningEffort     string            `json:"reasoning_effort"`
	Reasoning           json.RawMessage   `json:"reasoning"`
	Seed                *int64            `json:"seed"`
	WebSearchOptions    json.RawMessage   `json:"web_search_options"`
	// VideoConfig 为 Omni 视频设置扩展字段（OpenAI SDK 的 extra_body 会合并到顶层），见 omni_video.go
	VideoConfig *videoConfigRequest `json:"video_config"`
}

type chatStreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type chatMessage struct {
	Role       string           `json:"role"`
	Content    json.RawMessage  `json:"content"`
	Name       string           `json:"name"`
	ToolCallID string           `json:"tool_call_id"`
	ToolCalls  []openAIToolCall `json:"tool_calls"`
}

type openAIToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
	ExtraContent struct {
		Google struct {
			ThoughtSignature string `json:"thought_signature"`
		} `json:"google"`
	} `json:"extra_content"`
}

type openAITool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
		Strict      *bool           `json:"strict"`
	} `json:"function"`
}

var assistantImagePattern = regexp.MustCompile(`!\[[^\]]*\]\((data:image/[A-Za-z0-9.+-]+;base64,[A-Za-z0-9+/_=\r\n-]+)\)`)

func (s *server) handleOpenAIModels(w http.ResponseWriter, r *http.Request) {
	models, err := s.service.Models(r.Context())
	if err != nil {
		if shouldWriteRequestError(r, err) {
			writeOpenAIRequestError(w, err)
		}
		return
	}
	models = expandModelAliases(models)
	if r.Header.Get("Anthropic-Version") != "" {
		writeAnthropicModels(w, models)
		return
	}
	data := make([]map[string]any, 0, len(models))
	for _, model := range models {
		data = append(data, openAIModelObject(model))
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

// handleOpenAIModel 查询单个模型（OpenAI/Anthropic SDK 的 models.retrieve）；带 Anthropic-Version 时返回 Anthropic 格式
func (s *server) handleOpenAIModel(w http.ResponseWriter, r *http.Request) {
	anthropic := r.Header.Get("Anthropic-Version") != ""
	models, err := s.service.Models(r.Context())
	if err != nil {
		if shouldWriteRequestError(r, err) {
			if anthropic {
				writeAnthropicRequestError(w, err)
			} else {
				writeOpenAIRequestError(w, err)
			}
		}
		return
	}
	modelID := strings.TrimPrefix(r.PathValue("model"), "models/")
	model, ok := lookupLongPublicModel(models, modelID)
	switch {
	case !ok && anthropic:
		writeAnthropicError(w, http.StatusNotFound, "not_found_error", "model: "+modelID)
	case !ok:
		writeOpenAIError(w, http.StatusNotFound, "model_not_found", fmt.Sprintf("The model '%s' does not exist or you do not have access to it.", modelID))
	case anthropic:
		writeJSON(w, http.StatusOK, anthropicModelObject(model))
	default:
		writeJSON(w, http.StatusOK, openAIModelObject(model))
	}
}

func openAIModelObject(model aistudio.Model) map[string]any {
	item := map[string]any{
		"id":                           model.ID,
		"object":                       "model",
		"created":                      0,
		"owned_by":                     "google",
		"name":                         model.Name,
		"description":                  model.Description,
		"supported_generation_methods": model.Methods,
		"input_token_limit":            model.InputTokenLimit,
		"output_token_limit":           model.OutputTokenLimit,
	}
	if len(model.Capabilities) > 0 {
		item["capabilities"] = model.Capabilities
	}
	if len(model.CapabilityOptions) > 0 {
		item["capability_options"] = model.CapabilityOptions
	}
	if len(model.AccessModes) > 0 {
		item["access_modes"] = model.AccessModes
	}
	if len(model.Channels) > 0 {
		item["channels"] = model.Channels
	}
	if model.Paid {
		item["paid"] = true
	}
	return item
}

func (s *server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	var request chatRequest
	if err := decodeJSON(r, &request); err != nil {
		writeOpenAIInvalid(w, err)
		return
	}
	if request.Model == "" || len(request.Messages) == 0 {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "model and messages are required")
		return
	}
	requestID := newID("chatcmpl")
	generateRequest, err := request.toGenerateRequest(requestID)
	if err != nil {
		writeOpenAIInvalid(w, err)
		return
	}
	s.thoughtSignatures.Restore(s.thoughtSignatureScope(r.Context(), generateRequest.Model), generateRequest.Contents)
	// 先删除系统提示里 Claude Code 的计费行、按 OMNI_IGNORE_SYSTEM_PROMPT 去掉发给 Omni 视频模型的系统提示（见 omni_system.go 的
	// prepareClientSystem），之后恢复媒体与整理分轮都按整理后的对话进行
	s.prepareClientSystem(r.Context(), &generateRequest)
	// 回传历史里本服务的媒体链接恢复为媒体输入（见 restoreMediaLinks）
	s.restoreMediaLinks(r.Context(), &generateRequest)
	hideThought, err := s.prepareGenerate(r.Context(), &generateRequest)
	if err != nil {
		writeOpenAIInvalid(w, err)
		return
	}
	// 响应里的 model 报告去掉后缀的目录模型名（见 responseModel）
	request.Model = responseModel(generateRequest)
	generateRequest.Stream = request.Stream
	// 首个事件前的错误按非流式返回 HTTP 状态，不再先回 200 再在流里报错（见 generate）
	events, eventReady, err := s.generateStream(r.Context(), generateRequest)
	if err != nil {
		if shouldWriteRequestError(r, err) {
			writeOpenAIRequestError(w, err)
		}
		return
	}
	events = withHiddenReasoning(r.Context(), events, hideThought)
	created := time.Now().Unix()
	if request.Stream {
		// 额度用完改用了其他模型时报告实际使用的模型（流式收到首个事件后才确定，流内各块另按它更新，见 servedModel）
		request.Model = servedModel(r.Context(), request.Model)
		// 隐藏思维链时首个事件（思考）在过滤中被丢掉：按首个事件没到处理，提交时照样写一块心跳（见 handleGeminiGenerate）
		s.streamChatCompletion(w, r, request, requestID, created, events, eventReady && !hideThought)
		return
	}
	result, err := consumeEvents(r.Context(), events, nil)
	if err != nil {
		if shouldWriteRequestError(r, err) {
			writeOpenAIRequestError(w, err)
		}
		return
	}
	// 额度用完改用了其他模型时：响应报告实际使用的模型，函数调用的签名登记在目标模型的作用域下
	request.Model = servedModel(r.Context(), request.Model)
	s.thoughtSignatures.Remember(s.thoughtSignatureScope(r.Context(), servedModel(r.Context(), generateRequest.Model)), result.toolCalls)
	writeJSON(w, http.StatusOK, buildChatCompletion(requestID, created, request.Model, result, s.mediaOutputFor(r)))
}

func (request chatRequest) toGenerateRequest(id string) (aistudio.GenerateRequest, error) {
	var system []string
	contents := make([]aistudio.Content, 0, len(request.Messages))
	for _, message := range request.Messages {
		if message.Role == "system" || message.Role == "developer" {
			text, err := openAITextContent(message.Content)
			if err != nil {
				return aistudio.GenerateRequest{}, fmt.Errorf("%s message: %w", message.Role, err)
			}
			if text != "" {
				system = append(system, text)
			}
			continue
		}
		content, err := chatMessageContent(message)
		if err != nil {
			return aistudio.GenerateRequest{}, err
		}
		if len(content.Parts) == 0 {
			continue
		}
		contents = append(contents, content)
	}
	tools, err := mapOpenAITools(request.Tools, request.ToolChoice)
	if err != nil {
		return aistudio.GenerateRequest{}, err
	}
	if rawJSONConfigured(request.WebSearchOptions) {
		var options struct {
			SearchContextSize string          `json:"search_context_size"`
			UserLocation      json.RawMessage `json:"user_location"`
		}
		if err := json.Unmarshal(request.WebSearchOptions, &options); err != nil {
			return aistudio.GenerateRequest{}, fmt.Errorf("web_search_options must be an object")
		}
		tools.GoogleSearch, err = mapSearchOptions(options.SearchContextSize, options.UserLocation, nil)
		if err != nil {
			return aistudio.GenerateRequest{}, err
		}
		tools.Google = appendUnique(tools.Google, "google_search")
	}
	// parallel_tool_calls=false：一次回复最多调用一个函数
	tools.ToolConfig.ParallelCalls = request.ParallelToolCalls
	config, err := request.generationConfig()
	if err != nil {
		return aistudio.GenerateRequest{}, err
	}
	return aistudio.GenerateRequest{
		ID:       id,
		Model:    request.Model,
		System:   strings.Join(system, "\n"),
		Contents: contents,
		Config:   config,
		Tools:    tools,
	}, nil
}

func chatMessageContent(message chatMessage) (aistudio.Content, error) {
	role, err := openAIRole(message.Role)
	if err != nil {
		return aistudio.Content{}, err
	}
	if role == aistudio.RoleTool {
		cleaned, media, err := splitFunctionResultMedia(message.Content)
		if err != nil {
			return aistudio.Content{}, fmt.Errorf("tool message content: %w", err)
		}
		content, err := normalizeFunctionResultContent(cleaned)
		if err != nil {
			return aistudio.Content{}, fmt.Errorf("tool message content: %w", err)
		}
		parts := []aistudio.Part{{FunctionResult: &aistudio.FunctionResult{
			ID:      message.ToolCallID,
			Name:    message.Name,
			Content: content,
		}}}
		return aistudio.Content{Role: role, Parts: append(parts, media...)}, nil
	}
	parts, err := openAIContentParts(message.Content)
	if err != nil {
		return aistudio.Content{}, fmt.Errorf("%s message content: %w", message.Role, err)
	}
	if role == aistudio.RoleAssistant {
		// assistant 正文（字符串或数组的 text 块）里内联的 Markdown 图片拆回图片，见 restoreAssistantInlineImages
		parts = restoreAssistantInlineImages(parts)
	}
	for _, call := range message.ToolCalls {
		if call.Type != "" && call.Type != "function" {
			return aistudio.Content{}, fmt.Errorf("unsupported tool call type %q", call.Type)
		}
		parts = append(parts, aistudio.Part{FunctionCall: &aistudio.FunctionCall{
			ID:               call.ID,
			Name:             call.Function.Name,
			Arguments:        historyFunctionArguments(call.Function.Arguments),
			ThoughtSignature: call.ExtraContent.Google.ThoughtSignature,
		}})
	}
	return aistudio.Content{Role: role, Parts: parts}, nil
}

// historyFunctionArguments 返回历史工具调用的参数：空串为 {}；不是 JSON object 的原文（被截断的 JSON、字符串、数组等）
// 包成 {"_raw":"<原文>"}。Cline、Roo 等客户端回放一次被截断的调用后，原先这个会话之后每一轮都返回 400
func historyFunctionArguments(arguments string) json.RawMessage {
	if strings.TrimSpace(arguments) == "" {
		return json.RawMessage(`{}`)
	}
	var object map[string]json.RawMessage
	if json.Unmarshal([]byte(arguments), &object) == nil && object != nil {
		return json.RawMessage(arguments)
	}
	encoded, _ := json.Marshal(map[string]string{"_raw": arguments})
	return encoded
}

// assistantImageParts 把 assistant 正文里内联的 Markdown 图片（![…](data:image/…;base64,…)，本服务输出图片的写法，见 renderMediaMarkdown）
// 拆成文本与图片 part，回传历史时上游看到的是图片而不是几 MB 的 Base64 文本。只拆上游能收的图片：Base64 解码得出、按文件头识别为 PNG、
// JPEG、WebP、HEIC、HEIF、BMP 或 TIFF 的（GIF 先由 normalizedImageBlob 转成 PNG，判断与 Omni 直接内联的图片相同，见
// aistudio.InteractionInlineImage），拆出的 part 按识别出的格式写标准 MIME（声明为 image/svg+xml、image/jpg 等而数据是 PNG、JPEG 的，
// Playground 与 Build 会把声明的类型原样发给上游）。解码不出、SVG、声明为图片而数据不是这些格式的，以及落在围栏代码块或行内代码里的
// （模型写给用户看的 Markdown 源码，规则按 CommonMark，见 markdownCodeRanges）留在原文本里照样发送：这段历史每轮都会回传，拆成上游不收的
// 图片会让整段会话从此每轮失败，拆掉代码里的源码模型也看不到自己写过什么。一个 part 里能拆的与不能拆的各按各的处理；一处都不能拆时 ok 为 false
func assistantImageParts(text string) ([]aistudio.Part, bool) {
	matches := assistantImagePattern.FindAllStringSubmatchIndex(text, -1)
	if len(matches) == 0 {
		return nil, false
	}
	code := markdownCodeRanges(text)
	var parts []aistudio.Part
	// position 为已经写进 parts 的位置，不拆的图片留在之后的文本里
	position := 0
	for _, match := range matches {
		for len(code) > 0 && code[0][1] <= match[0] {
			code = code[1:]
		}
		if len(code) > 0 && code[0][0] <= match[0] {
			continue
		}
		part, err := fileOrInlinePart(text[match[2]:match[3]], "")
		if err != nil {
			continue
		}
		mimeType, ok := aistudio.InteractionInlineImageMIME(part)
		if !ok {
			continue
		}
		part.InlineData.MIME = mimeType
		if match[0] > position {
			parts = append(parts, aistudio.Part{Text: text[position:match[0]]})
		}
		parts = append(parts, part)
		position = match[1]
	}
	if len(parts) == 0 {
		return nil, false
	}
	if position < len(text) {
		parts = append(parts, aistudio.Part{Text: text[position:]})
	}
	return parts, true
}

// markdownCodeRanges 返回文本里 Markdown 代码的字节区间（按位置排序、互不重叠，右端不含），规则按 CommonMark：
//   - 围栏代码块：行首（最多缩进 3 个空格）三个以上反引号或波浪号起，到行首（最多缩进 3 个空格）同样的字符不少于起始个数、之后只有空白的
//     一行止，没有闭合时到文本末尾（与 Markdown 的渲染相同）；反引号围栏起始行的其余部分里还有反引号时不是围栏。写在列表项标记之后的
//     围栏（“- ```go”“1. ```”）以标记与之后空格的宽度为内容列，闭合行的缩进比内容列多不超过 3 个空格，遇到缩进不到内容列的非空行
//     （列表项结束）时止于前一行。不认这种围栏时，它缩进不到 4 个空格的闭合行会被当成没有闭合的起始围栏，之后的图片全被当成代码
//   - 行内代码：n 个反引号起，到同一段落里之后恰好 n 个反引号止；没有配对的反引号按普通字符。紧挨在奇数个反斜杠之后的一串反引号第一个
//     已被转义，其余的才作为起点（结尾不看反斜杠：行内代码里的反斜杠是普通字符）。段落在围栏、空行（只有空白的行）、ATX 标题行、分隔线
//     与 setext 标题的下划线处结束，列表项开头与引用开头（引用层数比上一行多）另起一段，引用标记（>）之后的内容按同样规则判断
//
// 段落边界宁多勿少：多算一处只会少认一段行内代码（代价是拆走代码里的图片、模型看不到这段源码），少算则会把正常显示的图片当成代码，
// 几 MB 的 Base64 每轮按文本回传。所以 CommonMark 里不能打断段落的列表标记（段落中间以 2. 开头或缩进 4 个空格以上的一行等）也算另起
// 一段，这些少数写法会少认行内代码。整段只扫描一遍（配对按每种长度的下一串预先算好），文本再长、反引号再多也是线性时间
func markdownCodeRanges(text string) [][2]int {
	if !strings.ContainsAny(text, "`~") {
		return nil
	}
	var ranges [][2]int
	// segment 为当前段落（行内代码配对的范围）的起点，depth 为上一行的引用层数
	segment, depth := 0, 0
	split := func(at int) {
		ranges = append(ranges, markdownInlineCode(text, segment, at)...)
		segment = at
	}
	for lineStart := 0; lineStart < len(text); {
		lineEnd := markdownLineEnd(text, lineStart)
		line := text[lineStart:lineEnd]
		if fence, ok := markdownFenceOpen(line); ok {
			split(lineStart)
			end := markdownFenceEnd(text, lineEnd, fence)
			ranges = append(ranges, [2]int{lineStart, end})
			segment, lineStart, depth = end, end+1, 0
			continue
		}
		lineDepth, content := markdownQuoteContent(line)
		before, alone := markdownParagraphBreak(content)
		if before || alone || lineDepth > depth {
			split(lineStart)
		}
		if alone {
			split(lineEnd)
		}
		depth, lineStart = lineDepth, lineEnd+1
	}
	split(len(text))
	return ranges
}

// markdownLineEnd 返回从 lineStart 起这一行的行尾（换行符的位置，最后一行为文本长度）
func markdownLineEnd(text string, lineStart int) int {
	if end := strings.IndexByte(text[lineStart:], '\n'); end >= 0 {
		return lineStart + end
	}
	return len(text)
}

// markdownFence 为围栏代码块的起始围栏：marker 为围栏字符，run 为个数，indent 为内容列（写在列表项标记之后时为行首到围栏的宽度，否则为 0）
type markdownFence struct {
	marker      byte
	run, indent int
}

// markdownFenceOpen 判断一行是围栏代码块的起始行：行首最多缩进 3 个空格，或列表项标记（前面缩进不限，嵌套列表）之后 1 到 4 个空格
func markdownFenceOpen(line string) (markdownFence, bool) {
	trimmed := strings.TrimLeft(line, " ")
	indent := 0
	if marker := markdownListMarker(trimmed); marker > 0 {
		content := strings.TrimLeft(trimmed[marker:], " ")
		if spaces := len(trimmed) - marker - len(content); spaces >= 1 && spaces <= 4 {
			indent, trimmed = len(line)-len(content), content
		}
	} else if len(line)-len(trimmed) > 3 {
		return markdownFence{}, false
	}
	if trimmed == "" || trimmed[0] != '`' && trimmed[0] != '~' {
		return markdownFence{}, false
	}
	marker := trimmed[0]
	run := len(trimmed) - len(strings.TrimLeft(trimmed, string(marker)))
	if run < 3 || marker == '`' && strings.Contains(trimmed[run:], "`") {
		return markdownFence{}, false
	}
	return markdownFence{marker: marker, run: run, indent: indent}, true
}

// markdownFenceEnd 返回起始围栏那一行的行尾 openLineEnd 之后代码块的结尾：闭合围栏那一行的行尾；写在列表项里的遇到缩进不到内容列的
// 非空行时为前一行的行尾（这一行照常判断，可能另起围栏）；没有闭合时为文本末尾
func markdownFenceEnd(text string, openLineEnd int, fence markdownFence) int {
	for lineStart := openLineEnd + 1; lineStart < len(text); {
		lineEnd := markdownLineEnd(text, lineStart)
		line := text[lineStart:lineEnd]
		trimmed := strings.TrimLeft(line, " ")
		indent := len(line) - len(trimmed)
		if indent < fence.indent && strings.TrimSpace(trimmed) != "" {
			return lineStart - 1
		}
		run := len(trimmed) - len(strings.TrimLeft(trimmed, string(fence.marker)))
		if indent-fence.indent <= 3 && run >= fence.run && strings.TrimSpace(trimmed[run:]) == "" {
			return lineEnd
		}
		lineStart = lineEnd + 1
	}
	return len(text)
}

// markdownListMarker 返回一行（已去掉行首空格）开头列表项标记的长度：-、*、+，或 1 到 9 位数字加 . 或 )，之后是空白或行尾；不是时为 0
func markdownListMarker(line string) int {
	length := 0
	if line != "" && strings.IndexByte("-*+", line[0]) >= 0 {
		length = 1
	} else {
		for length < len(line) && length < 9 && line[length] >= '0' && line[length] <= '9' {
			length++
		}
		if length == 0 || length == len(line) || line[length] != '.' && line[length] != ')' {
			return 0
		}
		length++
	}
	if length < len(line) && strings.IndexByte(" \t\r", line[length]) < 0 {
		return 0
	}
	return length
}

// markdownQuoteContent 返回一行的引用层数与引用标记（最多缩进 3 个空格的 >，以及之后的一个空格）之后的内容
func markdownQuoteContent(line string) (int, string) {
	depth := 0
	for {
		trimmed := strings.TrimLeft(line, " ")
		if len(line)-len(trimmed) > 3 || !strings.HasPrefix(trimmed, ">") {
			return depth, line
		}
		depth++
		line = strings.TrimPrefix(trimmed[1:], " ")
	}
}

// markdownParagraphBreak 判断一行（引用标记之后的内容）在哪里结束段落：before 为这一行另起一段（列表项开头），alone 为这一行自成一段
// （空行、ATX 标题行、分隔线与 setext 标题的下划线）
func markdownParagraphBreak(content string) (before, alone bool) {
	trimmed := strings.TrimLeft(content, " \t")
	if strings.TrimSpace(trimmed) == "" {
		return false, true
	}
	if len(content)-len(trimmed) <= 3 && (markdownATXHeading(trimmed) || markdownRuleLine(trimmed)) {
		return false, true
	}
	return markdownListMarker(trimmed) > 0, false
}

// markdownATXHeading 判断一行（已去掉行首空格）是 ATX 标题：1 到 6 个 #，之后是空白或行尾
func markdownATXHeading(line string) bool {
	level := len(line) - len(strings.TrimLeft(line, "#"))
	return level >= 1 && level <= 6 && (level == len(line) || strings.IndexByte(" \t\r", line[level]) >= 0)
}

// markdownRuleLine 判断一行（已去掉行首空格）是分隔线（三个以上 -、* 或 _，之间可有空白）或 setext 标题的下划线（只有 = 或只有 -）
func markdownRuleLine(line string) bool {
	line = strings.TrimRight(line, " \t\r")
	if line == "" {
		return false
	}
	marker := line[0]
	if (marker == '=' || marker == '-') && strings.Trim(line, string(marker)) == "" {
		return true
	}
	if marker != '-' && marker != '*' && marker != '_' {
		return false
	}
	count := 0
	for index := 0; index < len(line); index++ {
		switch line[index] {
		case marker:
			count++
		case ' ', '\t':
		default:
			return false
		}
	}
	return count >= 3
}

// markdownInlineCode 返回 text[from:to]（一个段落）里成对反引号的行内代码区间：紧挨在奇数个反斜杠之后的一串反引号第一个已被转义，
// 其余的才作为起点（只有一个时不是起点）；结尾不看反斜杠
func markdownInlineCode(text string, from, to int) [][2]int {
	type backtickRun struct {
		start, end int
		escaped    bool
	}
	var runs []backtickRun
	for index := from; index < to; {
		next := strings.IndexByte(text[index:to], '`')
		if next < 0 {
			break
		}
		start := index + next
		end := start
		for end < to && text[end] == '`' {
			end++
		}
		backslashes := 0
		for start-backslashes > from && text[start-backslashes-1] == '\\' {
			backslashes++
		}
		runs = append(runs, backtickRun{start, end, backslashes%2 == 1})
		index = end
	}
	if len(runs) < 2 {
		return nil
	}
	// same[i]、shorter[i] 为第 i 串之后第一串与它同样长度、比它少一个反引号的位置（没有时为 -1）：转义的一串按少一个的配对
	same, shorter := make([]int, len(runs)), make([]int, len(runs))
	latest := make(map[int]int)
	for index := len(runs) - 1; index >= 0; index-- {
		length := runs[index].end - runs[index].start
		same[index], shorter[index] = -1, -1
		if found, ok := latest[length]; ok {
			same[index] = found
		}
		if found, ok := latest[length-1]; ok {
			shorter[index] = found
		}
		latest[length] = index
	}
	var ranges [][2]int
	for index := 0; index < len(runs); {
		start, closer := runs[index].start, same[index]
		if runs[index].escaped {
			start, closer = start+1, shorter[index]
		}
		if closer < 0 {
			index++
			continue
		}
		ranges = append(ranges, [2]int{start, runs[closer].end})
		index = closer + 1
	}
	return ranges
}

// restoreAssistantInlineImages 把 assistant（Gemini 原生为 model）轮文本 part 里内联的 Markdown 图片拆回图片 part（规则见 assistantImageParts），
// Chat Completions（content 为字符串或数组的 text 块）、Messages 与 Gemini 原生三个入口共用。图片总是以 data URI 内联返回（MEDIA_OUTPUT 只对
// 视频与音频生效）：new-api 等中转把 Gemini 的 inlineData 转成 ![image](data:…) 交给客户端，下一轮这段 Markdown 作为 model 文本回到 Gemini
// 原生入口；Chat Completions 与 Messages 的图片本来就以这种 Markdown 写在正文里。原先 Gemini 原生与 Messages 把整段 Base64 当作文本发给上游，
// 输入 token 暴涨、模型也看不到原图。只处理 assistant 轮（用户消息里的同样文本照原样发送），思考与语音元数据 part 不动，思考签名留在拆出的
// 第一个 part 上；不能拆的保留原文本，不返回 400（Chat Completions 的字符串 content 原先对解码不出的 data URI 返回 400，这段历史每轮回传，
// 整段会话从此每轮失败，现在同样按文本发送）
func restoreAssistantInlineImages(parts []aistudio.Part) []aistudio.Part {
	var restored []aistudio.Part
	for index, part := range parts {
		var split []aistudio.Part
		if part.Text != "" && !part.Thought && part.SpeechMetadata == nil && strings.Contains(part.Text, "data:image/") {
			split, _ = assistantImageParts(part.Text)
		}
		if split == nil {
			if restored != nil {
				restored = append(restored, part)
			}
			continue
		}
		if restored == nil {
			restored = append(make([]aistudio.Part, 0, len(parts)+len(split)), parts[:index]...)
		}
		split[0].ThoughtSignature = part.ThoughtSignature
		restored = append(restored, split...)
	}
	if restored == nil {
		return parts
	}
	return restored
}

func openAIRole(role string) (aistudio.Role, error) {
	switch role {
	case "user":
		return aistudio.RoleUser, nil
	case "assistant":
		return aistudio.RoleAssistant, nil
	case "tool", "function":
		return aistudio.RoleTool, nil
	default:
		return "", fmt.Errorf("unsupported message role %q", role)
	}
}

func openAITextContent(raw json.RawMessage) (string, error) {
	parts, err := openAIContentParts(raw)
	if err != nil {
		return "", err
	}
	var text strings.Builder
	for _, part := range parts {
		if part.Text == "" && (part.InlineData != nil || part.File != nil) {
			return "", fmt.Errorf("system content must be text")
		}
		text.WriteString(part.Text)
	}
	return text.String(), nil
}

// openAIContentParts 转换文本与媒体并省略空文本占位
func openAIContentParts(raw json.RawMessage) ([]aistudio.Part, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		if text == "" {
			return nil, nil
		}
		return []aistudio.Part{{Text: text}}, nil
	}
	var blocks []json.RawMessage
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil, fmt.Errorf("content must be a string or array")
	}
	parts := make([]aistudio.Part, 0, len(blocks))
	for _, block := range blocks {
		part, err := openAIContentPart(block)
		if err != nil {
			return nil, err
		}
		if part == (aistudio.Part{}) {
			continue
		}
		parts = append(parts, part)
	}
	return parts, nil
}

func openAIContentPart(raw json.RawMessage) (aistudio.Part, error) {
	var block struct {
		Type     string          `json:"type"`
		Text     string          `json:"text"`
		ImageURL json.RawMessage `json:"image_url"`
		VideoURL json.RawMessage `json:"video_url"`
		FileID   string          `json:"file_id"`
		Filename string          `json:"filename"`
		FileData string          `json:"file_data"`
		FileURL  string          `json:"file_url"`
		// File 为 Chat Completions 的写法：{"type":"file","file":{"file_id"|"file_data","filename"}}；
		// Responses 的 input_file 把这些字段放在顶层
		File *struct {
			FileID   string `json:"file_id"`
			Filename string `json:"filename"`
			FileData string `json:"file_data"`
		} `json:"file"`
		InputAudio *struct {
			Data   string `json:"data"`
			Format string `json:"format"`
		} `json:"input_audio"`
	}
	if err := json.Unmarshal(raw, &block); err != nil {
		return aistudio.Part{}, err
	}
	switch block.Type {
	case "text", "input_text", "output_text":
		return aistudio.Part{Text: block.Text}, nil
	case "image_url", "input_image":
		url, err := imageURLString(block.ImageURL)
		if err != nil {
			return aistudio.Part{}, err
		}
		if remote, ok := remoteMediaPart(url, "image/*"); ok {
			return remote, nil
		}
		return fileOrInlinePart(url, "")
	case "video_url", "input_video":
		url, err := imageURLString(block.VideoURL)
		if err != nil {
			return aistudio.Part{}, err
		}
		media, ok := aistudio.ExternalMediaForURL(url)
		if !ok {
			return aistudio.Part{}, fmt.Errorf("video_url must contain a YouTube video URL")
		}
		return aistudio.Part{ExternalMedia: media}, nil
	case "file", "input_file":
		if block.File != nil {
			block.FileID = firstNonEmpty(block.File.FileID, block.FileID)
			block.Filename = firstNonEmpty(block.File.Filename, block.Filename)
			block.FileData = firstNonEmpty(block.File.FileData, block.FileData)
		}
		if block.FileData != "" {
			return fileDataPart(block.FileData, block.Filename)
		}
		if block.FileURL != "" {
			if remote, ok := remoteMediaPart(block.FileURL, mimeForFilename(block.Filename, "application/pdf")); ok {
				return remote, nil
			}
			return aistudio.Part{}, fmt.Errorf("file_url must be an http or https URL")
		}
		if strings.TrimSpace(block.FileID) == "" {
			return aistudio.Part{}, fmt.Errorf("file part requires file_id, file_data or file_url")
		}
		return aistudio.Part{File: &aistudio.FileRef{ID: block.FileID, Name: block.Filename}}, nil
	case "input_audio":
		if block.InputAudio == nil {
			return aistudio.Part{}, fmt.Errorf("input_audio is required")
		}
		data, err := decodeBase64Flexible(block.InputAudio.Data)
		if err != nil {
			return aistudio.Part{}, fmt.Errorf("input_audio.data: %w", err)
		}
		return aistudio.Part{InlineData: &aistudio.Blob{MIME: audioMIME(block.InputAudio.Format), Data: data}}, nil
	default:
		return aistudio.Part{}, fmt.Errorf("unsupported content type %q", block.Type)
	}
}

func imageURLString(raw json.RawMessage) (string, error) {
	var value string
	if err := json.Unmarshal(raw, &value); err == nil {
		return value, nil
	}
	var object struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(raw, &object); err != nil || object.URL == "" {
		return "", fmt.Errorf("image_url must contain url")
	}
	return object.URL, nil
}

// remoteMediaPart 把 YouTube 以外的 http/https 地址作为外部媒体交给上游读取（与 Anthropic 的 URL source 一致）。
// 原先这类地址被当作已上传文件的 ID，标准的 OpenAI 看图请求（image_url 为 https 图片地址）返回 404，
// 每次还要刷新全部账户的运行态查找这个“文件”
func remoteMediaPart(value string, fallbackMIME string) (aistudio.Part, bool) {
	value = strings.TrimSpace(value)
	if media, ok := aistudio.ExternalMediaForURL(value); ok {
		return aistudio.Part{ExternalMedia: media}, true
	}
	parsed, err := url.Parse(value)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return aistudio.Part{}, false
	}
	return aistudio.Part{ExternalMedia: &aistudio.ExternalMedia{MIME: mimeForFilename(parsed.Path, fallbackMIME), URL: value}}, true
}

// mimeForFilename 按扩展名推断媒体类型；未知时返回 fallback
func mimeForFilename(name string, fallback string) string {
	if detected := mime.TypeByExtension(strings.ToLower(path.Ext(name))); detected != "" {
		if mediaType, _, err := mime.ParseMediaType(detected); err == nil {
			return mediaType
		}
	}
	return fallback
}

// fileDataPart 解析文件内容：data URL，或不带前缀的 base64（按文件名推断类型，默认 PDF；内容是文本时按 text/plain，见 attachmentMIME）
func fileDataPart(value string, filename string) (aistudio.Part, error) {
	if strings.HasPrefix(value, "data:") {
		return fileOrInlinePart(value, filename)
	}
	data, err := decodeBase64Flexible(value)
	if err != nil {
		return aistudio.Part{}, fmt.Errorf("file_data must be a data URL or base64: %w", err)
	}
	return aistudio.Part{InlineData: &aistudio.Blob{MIME: attachmentMIME(mimeForFilename(filename, "application/pdf"), data), Data: data}}, nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func fileOrInlinePart(value string, name string) (aistudio.Part, error) {
	if media, ok := aistudio.ExternalMediaForURL(value); ok {
		return aistudio.Part{ExternalMedia: media}, nil
	}
	if !strings.HasPrefix(value, "data:") {
		return aistudio.Part{File: &aistudio.FileRef{ID: value, Name: name}}, nil
	}
	metadata, encoded, ok := strings.Cut(strings.TrimPrefix(value, "data:"), ",")
	if !ok || !strings.HasSuffix(metadata, ";base64") {
		return aistudio.Part{}, fmt.Errorf("data URL must use base64")
	}
	data, err := decodeBase64Flexible(encoded)
	if err != nil {
		return aistudio.Part{}, fmt.Errorf("data URL: %w", err)
	}
	// 声明的类型不是文本类、内容却是文本时按 text/plain 发送（见 attachmentMIME）
	return aistudio.Part{InlineData: normalizedImageBlob(attachmentMIME(strings.TrimSuffix(metadata, ";base64"), data), data)}, nil
}

func audioMIME(format string) string {
	switch strings.ToLower(format) {
	case "mp3", "mpeg":
		return "audio/mpeg"
	case "wav":
		return "audio/wav"
	default:
		return "audio/" + strings.ToLower(format)
	}
}

func mapOpenAITools(tools []openAITool, choice json.RawMessage) (aistudio.Tools, error) {
	var mapped aistudio.Tools
	for _, tool := range tools {
		switch tool.Type {
		case "function":
			if tool.Function.Name == "" {
				return aistudio.Tools{}, fmt.Errorf("function tool name is required")
			}
			parameters := tool.Function.Parameters
			if len(parameters) == 0 {
				parameters = json.RawMessage(`{"type":"object","properties":{}}`)
			}
			mapped.Functions = append(mapped.Functions, aistudio.FunctionDeclaration{
				Name:        tool.Function.Name,
				Description: tool.Function.Description,
				Parameters:  parameters,
				Strict:      tool.Function.Strict != nil && *tool.Function.Strict,
			})
		case "web_search", "web_search_preview":
			mapped.Google = appendUnique(mapped.Google, "google_search")
		case "code_interpreter":
			mapped.Google = appendUnique(mapped.Google, "code_execution")
		case "url_context":
			mapped.Google = appendUnique(mapped.Google, "url_context")
		case "google_maps":
			mapped.Google = appendUnique(mapped.Google, "google_maps")
		case "image_search":
			mapped.Google = appendUnique(mapped.Google, "image_search")
		default:
			return aistudio.Tools{}, fmt.Errorf("unsupported tool type %q", tool.Type)
		}
	}
	config, err := openAIToolChoice(choice)
	if err != nil {
		return aistudio.Tools{}, err
	}
	mapped.ToolConfig = config
	return mapped, nil
}

func openAIToolChoice(raw json.RawMessage) (aistudio.ToolConfig, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return aistudio.ToolConfig{Mode: "auto"}, nil
	}
	var mode string
	if err := json.Unmarshal(raw, &mode); err == nil {
		switch mode {
		case "auto", "none", "required":
			return aistudio.ToolConfig{Mode: mode}, nil
		default:
			return aistudio.ToolConfig{}, fmt.Errorf("unsupported tool_choice %q", mode)
		}
	}
	// 指定函数：Chat 为 {"type":"function","function":{"name":...}}，Responses 为 {"type":"function","name":...,"namespace":...}
	var object struct {
		Type      string `json:"type"`
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
		Function  *struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if err := json.Unmarshal(raw, &object); err != nil {
		return aistudio.ToolConfig{}, fmt.Errorf("invalid tool_choice: %w", err)
	}
	if object.Function != nil {
		object.Name = object.Function.Name
	}
	if object.Type != "function" || object.Name == "" {
		return aistudio.ToolConfig{}, fmt.Errorf("named tool_choice requires type function and a name")
	}
	if object.Namespace != "" {
		object.Name = object.Namespace + "." + object.Name
	}
	return aistudio.ToolConfig{Mode: "required", AllowedFunctionNames: []string{object.Name}}, nil
}

func appendUnique(values []string, value string) []string {
	for _, current := range values {
		if current == value {
			return values
		}
	}
	return append(values, value)
}

func (request chatRequest) generationConfig() (aistudio.GenerationConfig, error) {
	// 只接受单个候选：多候选会把一个请求扇出为多次生成
	if request.N != nil && *request.N != 1 {
		return aistudio.GenerationConfig{}, fmt.Errorf("n must be 1")
	}
	// frequency_penalty、presence_penalty、logprobs、top_logprobs、logit_bias 上游没有对应参数，忽略：
	// SillyTavern、LobeChat 等客户端常带非 0 的 penalty，经 new-api 原样转发，原先整个请求返回 400
	config := aistudio.GenerationConfig{
		Temperature:     request.Temperature,
		TopP:            request.TopP,
		ReasoningEffort: request.ReasoningEffort,
		Seed:            request.Seed,
		VideoConfig:     request.VideoConfig.videoConfig(),
	}
	if request.MaxCompletionTokens != nil {
		config.MaxOutputTokens = request.MaxCompletionTokens
	} else {
		config.MaxOutputTokens = request.MaxTokens
	}
	stop, err := decodeStopSequences(request.Stop)
	if err != nil {
		return config, err
	}
	config.StopSequences = stop
	if len(request.Reasoning) > 0 && string(request.Reasoning) != "null" {
		var reasoning struct {
			Effort string `json:"effort"`
		}
		if err := json.Unmarshal(request.Reasoning, &reasoning); err != nil {
			return config, fmt.Errorf("invalid reasoning: %w", err)
		}
		if reasoning.Effort != "" {
			config.ReasoningEffort = reasoning.Effort
		}
	}
	if len(request.ResponseFormat) > 0 && string(request.ResponseFormat) != "null" {
		var format struct {
			Type       string `json:"type"`
			JSONSchema struct {
				Schema json.RawMessage `json:"schema"`
			} `json:"json_schema"`
		}
		if err := json.Unmarshal(request.ResponseFormat, &format); err != nil {
			return config, fmt.Errorf("invalid response_format: %w", err)
		}
		switch format.Type {
		case "json_object":
			config.ResponseMIMEType = "application/json"
		case "json_schema":
			config.ResponseMIMEType = "application/json"
			// schema 为 null 时按未设置处理，只保留 JSON 模式
			if geminiRawObjectPresent(format.JSONSchema.Schema) {
				config.ResponseSchema = format.JSONSchema.Schema
			}
		case "", "text":
		default:
			return config, fmt.Errorf("unsupported response_format type %q", format.Type)
		}
	}
	return config, nil
}

func decodeStopSequences(raw json.RawMessage) ([]string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var single string
	if err := json.Unmarshal(raw, &single); err == nil {
		return normalizeStopSequences([]string{single}), nil
	}
	var multiple []string
	if err := json.Unmarshal(raw, &multiple); err != nil {
		return nil, fmt.Errorf("stop must be a string or string array")
	}
	return normalizeStopSequences(multiple), nil
}

// normalizeStopSequences 删除不会形成停止条件的空字符串
func normalizeStopSequences(values []string) []string {
	var normalized []string
	for _, value := range values {
		if value != "" {
			normalized = append(normalized, value)
		}
	}
	return normalized
}

func buildChatCompletion(id string, created int64, model string, result generationResult, media mediaOutput) map[string]any {
	rendered := renderedContent(result.events, media)
	content := any(rendered)
	if rendered == "" && len(result.toolCalls) > 0 {
		content = nil
	}
	message := map[string]any{"role": "assistant", "content": content}
	if result.reasoning.Len() > 0 {
		message["reasoning_content"] = result.reasoning.String()
	}
	if len(result.toolCalls) > 0 {
		message["tool_calls"] = openAIToolCallOutput(result.toolCalls)
	}
	if len(result.citations) > 0 {
		message["annotations"] = openAICitations(result.citations)
	}
	// logprobs 与官方一致恒为 null：请求里的 logprobs 被忽略，不返回对数概率
	choice := map[string]any{
		"index":         0,
		"message":       message,
		"logprobs":      nil,
		"finish_reason": openAIFinishReason(result.finishReason, len(result.toolCalls) > 0),
	}
	if providerReason := providerFinishReason(result.finishReason); providerReason != "" {
		choice["provider_finish_reason"] = providerReason
	}
	response := map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"created": created,
		"model":   model,
		"choices": []any{choice},
	}
	if result.providerModel != "" {
		response["provider_model"] = result.providerModel
	}
	if result.usage != nil {
		response["usage"] = openAIUsage(result.usage)
	}
	return response
}

// streamChatCompletion 输出 OpenAI Chat 流式响应；eventReady 为写出响应头前已收到首个事件（见 generateStream）
func (s *server) streamChatCompletion(
	w http.ResponseWriter, r *http.Request, request chatRequest, id string, created int64, events <-chan aistudio.Event, eventReady bool,
) {
	if err := streamHeaders(w); err != nil {
		return
	}
	if err := writeChatChunk(w, id, created, request.Model, map[string]any{"role": "assistant", "content": ""}, nil, request.StreamOptions.IncludeUsage); err != nil {
		return
	}
	dataHeartbeat := s.config.streamDataHeartbeat()
	signatureModel := request.Model
	signatureScope := s.thoughtSignatureScope(r.Context(), request.Model)
	// currentModel 返回之后各块报告的模型：提交响应头之后才决定改用其他模型（额度用完）时，之后的块（含心跳块）与签名登记按实际使用的模型，
	// 已写出的 role 块保留请求的模型
	currentModel := func() string {
		request.Model = servedModel(r.Context(), request.Model)
		if request.Model != signatureModel {
			signatureModel = request.Model
			signatureScope = s.thoughtSignatureScope(r.Context(), request.Model)
		}
		return request.Model
	}
	heartbeat := func() error {
		return chatHeartbeat(w, dataHeartbeat, id, created, currentModel(), request.StreamOptions.IncludeUsage)()
	}
	if dataHeartbeat && !eventReady {
		// 首个事件还没到就提交了响应头：紧跟 role 块再写一块空 delta。new-api 的 OpenAI 渠道落后一块转发（relay-openai.go 的 OaiStreamHandler
		// 收到下一条 data 时才把上一条写给它的客户端），不写这一块时 role 块要等到下一次心跳或首个事件才到它的客户端
		if err := heartbeat(); err != nil {
			return
		}
	}
	toolIndex := 0
	hasContent := false
	contentEndsWithNewline := false
	media := s.mediaOutputFor(r)
	result, err := consumeStreamEvents(r.Context(), events, media.joinStreamAudio(func(event aistudio.Event) error {
		currentModel()
		switch event.Kind {
		case aistudio.EventText:
			hasContent = hasContent || event.Text != ""
			contentEndsWithNewline = strings.HasSuffix(event.Text, "\n")
			return writeChatChunk(w, id, created, request.Model, map[string]any{"content": event.Text}, nil, request.StreamOptions.IncludeUsage)
		case aistudio.EventReasoning:
			return writeChatChunk(w, id, created, request.Model, map[string]any{"reasoning_content": event.Text}, nil, request.StreamOptions.IncludeUsage)
		case aistudio.EventToolCall:
			if event.ToolCall == nil {
				return nil
			}
			call := event.ToolCall
			s.thoughtSignatures.Remember(signatureScope, []aistudio.FunctionCall{*call})
			toolCall := map[string]any{
				"index": toolIndex,
				"id":    call.ID,
				"type":  "function",
				"function": map[string]any{
					"name":      call.Name,
					"arguments": string(call.Arguments),
				},
			}
			if call.ThoughtSignature != "" {
				toolCall["extra_content"] = openAIGoogleThoughtSignature(call.ThoughtSignature)
			}
			delta := map[string]any{"tool_calls": []any{toolCall}}
			toolIndex++
			return writeChatChunk(w, id, created, request.Model, delta, nil, request.StreamOptions.IncludeUsage)
		case aistudio.EventMedia:
			if event.Media == nil {
				return nil
			}
			content := media.markdown(*event.Media)
			if hasContent && !contentEndsWithNewline {
				content = "\n" + content
			}
			hasContent = true
			contentEndsWithNewline = false
			return writeChatChunk(w, id, created, request.Model, map[string]any{"content": content}, nil, request.StreamOptions.IncludeUsage)
		case aistudio.EventExecutableCode, aistudio.EventCodeExecutionResult:
			content := renderCodeExecution(event)
			if content == "" {
				return nil
			}
			if hasContent && !contentEndsWithNewline {
				content = "\n" + content
			}
			content += "\n"
			hasContent = true
			contentEndsWithNewline = true
			return writeChatChunk(w, id, created, request.Model, map[string]any{"content": content}, nil, request.StreamOptions.IncludeUsage)
		}
		return nil
	}), heartbeat)
	if err != nil {
		if shouldWriteRequestError(r, err) {
			_ = writeSSE(w, "", openAIStreamError(w, err))
		}
		return
	}
	if len(result.citations) > 0 {
		_ = writeChatChunk(w, id, created, request.Model, map[string]any{"annotations": openAICitations(result.citations)}, nil, request.StreamOptions.IncludeUsage)
	}
	finish := openAIFinishReason(result.finishReason, len(result.toolCalls) > 0)
	finalDelta := map[string]any{}
	if providerReason := providerFinishReason(result.finishReason); providerReason != "" {
		finalDelta["provider_finish_reason"] = providerReason
	}
	if err := writeChatChunk(w, id, created, request.Model, finalDelta, &finish, request.StreamOptions.IncludeUsage); err != nil {
		return
	}
	if request.StreamOptions.IncludeUsage && result.usage != nil {
		chunk := map[string]any{
			"id":      id,
			"object":  "chat.completion.chunk",
			"created": created,
			"model":   request.Model,
			"choices": []any{},
			"usage":   openAIUsage(result.usage),
		}
		if err := writeSSE(w, "", chunk); err != nil {
			return
		}
	}
	_ = writeSSEText(w, "[DONE]")
}

func writeChatChunk(w http.ResponseWriter, id string, created int64, model string, delta map[string]any, finish *string, includeUsage bool) error {
	choice := map[string]any{
		"index":         0,
		"delta":         delta,
		"finish_reason": finish,
	}
	if providerReason, ok := delta["provider_finish_reason"].(string); ok && providerReason != "" {
		delete(delta, "provider_finish_reason")
		choice["provider_finish_reason"] = providerReason
	}
	chunk := map[string]any{
		"id":      id,
		"object":  "chat.completion.chunk",
		"created": created,
		"model":   model,
		"choices": []any{choice},
	}
	if includeUsage {
		chunk["usage"] = nil
	}
	return writeSSE(w, "", chunk)
}

func openAIFinishReason(reason string, hasTools bool) string {
	switch strings.ToLower(strings.TrimSpace(reason)) {
	case "max_tokens", "max_output_tokens", "length", "continuation":
		return "length"
	case "stop_sequence":
		return "stop"
	default:
		if finishFiltered(reason) {
			return "content_filter"
		}
		if hasTools {
			return "tool_calls"
		}
		return "stop"
	}
}

func openAIToolCallOutput(calls []aistudio.FunctionCall) []map[string]any {
	output := make([]map[string]any, 0, len(calls))
	for _, call := range calls {
		item := map[string]any{
			"id":   call.ID,
			"type": "function",
			"function": map[string]any{
				"name":      call.Name,
				"arguments": string(call.Arguments),
			},
		}
		if call.ThoughtSignature != "" {
			item["extra_content"] = openAIGoogleThoughtSignature(call.ThoughtSignature)
		}
		output = append(output, item)
	}
	return output
}

func openAIGoogleThoughtSignature(signature string) map[string]any {
	return map[string]any{"google": map[string]string{"thought_signature": signature}}
}

func openAICitations(citations []aistudio.Citation) []map[string]any {
	output := make([]map[string]any, 0, len(citations))
	for _, citation := range citations {
		output = append(output, map[string]any{
			"type": "url_citation",
			"url_citation": map[string]any{
				"start_index": citation.Start,
				"end_index":   citation.End,
				"title":       citation.Title,
				"url":         citation.URL,
			},
		})
	}
	return output
}

func openAIUsage(usage *aistudio.Usage) map[string]any {
	return map[string]any{
		"prompt_tokens":     inputTokens(usage),
		"completion_tokens": outputTokens(usage),
		"total_tokens":      usage.TotalTokens,
		"completion_tokens_details": map[string]any{
			"reasoning_tokens": usage.ReasoningTokens,
		},
	}
}

// renderedContent 返回 Chat 消息正文：正文原样拼接，媒体按 media 渲染为 Markdown（data URI 或本服务链接），代码执行渲染为代码块
func renderedContent(events []aistudio.Event, media mediaOutput) string {
	var content strings.Builder
	for _, event := range events {
		switch event.Kind {
		case aistudio.EventText:
			content.WriteString(event.Text)
		case aistudio.EventMedia:
			if event.Media == nil {
				continue
			}
			if content.Len() > 0 && !strings.HasSuffix(content.String(), "\n") {
				content.WriteByte('\n')
			}
			content.WriteString(media.markdown(*event.Media))
		case aistudio.EventExecutableCode, aistudio.EventCodeExecutionResult:
			rendered := renderCodeExecution(event)
			if rendered == "" {
				continue
			}
			if content.Len() > 0 && !strings.HasSuffix(content.String(), "\n") {
				content.WriteByte('\n')
			}
			content.WriteString(rendered)
			content.WriteByte('\n')
		}
	}
	return content.String()
}
