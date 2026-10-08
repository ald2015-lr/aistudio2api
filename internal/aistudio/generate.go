package aistudio

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"
)

var errStopSequenceMatched = errors.New("stop sequence matched")

// tokenCountResult 保存并发输入计数结果
type tokenCountResult struct {
	count TokenCount
	err   error
}

// EncodeGenerateContentRequest 编码当前成功基线的 GenerateContent 数组
func EncodeGenerateContentRequest(request GenerateRequest, defaults GenerationDefaults, runtime RequestContext) ([]byte, error) {
	request.Config = applyOutputFloor(request.Config, request.MinOutputTokens, defaults.MaxOutputTokens)
	tools, explicitTools, err := encodeRequestedTools(request.Tools)
	if err != nil {
		return nil, err
	}
	contents, err := encodeContents(request.Contents)
	if err != nil {
		return nil, err
	}
	if len(contents) == 0 {
		return nil, fmt.Errorf("%w: GenerateContent contents 不能为空", ErrInvalidArgument)
	}
	config, err := encodeGenerationConfig(request.Config, defaults)
	if err != nil {
		return nil, err
	}
	serverSideTools := explicitTools && len(request.Tools.Functions) > 0 && (len(request.Tools.Google) > 0 || request.Tools.GoogleSearch != nil)
	// 与网页版一致：普通对话请求共 11 项、不带时区；只有同时使用内置工具与函数调用时才需要第 14 项工具配置
	length := 11
	if serverSideTools {
		length = 14
	}
	wire := make([]any, length)
	wire[0] = wireModelName(request.Model)
	wire[1] = contents
	if !defaults.ImageRoute {
		wire[2] = observedSafetySettings()
	}
	wire[3] = config
	if request.System != "" {
		wire[5] = encodeSystemInstruction(request.System)
	}
	switch {
	case explicitTools:
		wire[6] = tools
	case defaults.OutputResolution:
		// 可设置分辨率模型的默认扩展工具字段
		wire[6] = []any{[]any{nil, nil, nil, []any{nil, []any{}}}}
	}
	wire[10] = int64(1)
	if serverSideTools {
		// 同时使用内置工具与函数调用时开启 include_server_side_tool_invocations
		wire[13] = []any{nil, nil, true}
	}
	return json.Marshal(wire)
}

func encodeGenerationConfig(config GenerationConfig, defaults GenerationDefaults) ([]any, error) {
	var responseSchema []any
	var err error
	if len(config.ResponseSchema) > 0 {
		responseSchema, err = encodeResponseSchema(config.ResponseSchema)
		if err != nil {
			return nil, fmt.Errorf("response schema: %w", err)
		}
	}
	thinkingLevel := defaults.DefaultThinkingLevel
	thinkingBudget := config.ThinkingBudget
	// 兼容各家客户端的思考强度写法：xhigh、max 视为最高，auto、default 与未知写法按未设置处理
	effort := normalizeReasoningEffort(config.ReasoningEffort)
	hasReasoningEffort := effort != ""
	switch effort {
	case "low":
		thinkingLevel = 1
	case "medium":
		thinkingLevel = 2
	case "high":
		thinkingLevel = 3
	case "minimal":
		thinkingLevel = 4
	case "none":
		thinkingLevel = 4
		if !defaults.ThinkingLevel {
			hasReasoningEffort = false
			if defaults.ThinkingBudget && thinkingBudget == nil {
				zero := int64(0)
				thinkingBudget = &zero
			}
		}
	}
	if hasReasoningEffort && defaults.ThinkingLevel {
		thinkingLevel = closestSupportedThinkingLevel(thinkingLevel, defaults.ThinkingLevels)
	}
	if hasReasoningEffort && !defaults.ThinkingLevel {
		// 模型只支持思考预算时按强度换算预算；模型不支持调节思考时忽略该参数，而不是整次请求失败
		if defaults.ThinkingBudget && thinkingBudget == nil {
			budget := thinkingBudgetForLevel(thinkingLevel)
			thinkingBudget = &budget
		}
		hasReasoningEffort = false
	}
	if thinkingBudget != nil && !defaults.ThinkingBudget {
		if defaults.ThinkingLevel && !hasReasoningEffort {
			thinkingLevel = closestSupportedThinkingLevel(thinkingLevelForBudget(*thinkingBudget), defaults.ThinkingLevels)
		}
		// 模型只支持思考等级时已换算为等级；两者都不支持时忽略客户端传来的预算
		thinkingBudget = nil
	}
	includeMaxOutput := config.SpeechConfig == nil || config.MaxOutputTokens != nil
	maxOutput := defaults.MaxOutputTokens
	if config.MaxOutputTokens != nil && *config.MaxOutputTokens > 0 {
		maxOutput = *config.MaxOutputTokens
	}
	if includeMaxOutput && maxOutput <= 0 {
		return nil, fmt.Errorf("模型目录缺少有效 output token limit")
	}
	// 客户端要求的输出上限超过模型上限时按模型上限发送：模型本来最多也只能输出到上限，
	// 没必要让整次请求失败（常见于把 max_tokens 写死成较大值的客户端）
	if includeMaxOutput && defaults.MaxOutputTokens > 0 && maxOutput > defaults.MaxOutputTokens {
		maxOutput = defaults.MaxOutputTokens
	}
	temperature := defaults.Temperature
	if config.Temperature != nil {
		temperature = config.Temperature
	}
	if temperature != nil && (*temperature < 0 || *temperature > 2) {
		return nil, fmt.Errorf("temperature 必须在 0 到 2 之间")
	}
	topP := defaults.TopP
	if config.TopP != nil {
		topP = config.TopP
	}
	if topP != nil && (*topP < 0 || *topP > 1) {
		return nil, fmt.Errorf("top_p 必须在 0 到 1 之间")
	}
	topK := defaults.TopK
	if config.TopK != nil {
		topK = config.TopK
	}
	if topK != nil && *topK < 0 {
		return nil, fmt.Errorf("top_k 不能为负数")
	}
	responseModalities, err := encodeResponseModalities(config.ResponseModalities)
	if err != nil {
		return nil, err
	}
	imageConfig := encodeImageConfig(config.ImageConfig)
	if defaults.OutputResolution && imageConfig == nil {
		// 可设置分辨率模型的默认输出尺寸
		imageConfig = []any{nil, "1K"}
	}
	speechConfig, err := encodeSpeechConfig(config.SpeechConfig)
	if err != nil {
		return nil, err
	}
	transcriptionConfig, err := encodeTranscriptionConfig(config.TranscriptionConfig)
	if err != nil {
		return nil, err
	}
	includeThinking := defaults.Thinking || defaults.ThinkingBudget || defaults.ThinkingLevel || thinkingBudget != nil || hasReasoningEffort
	length := 14
	if responseModalities != nil {
		length = 15
	}
	if speechConfig != nil {
		length = 16
	}
	if includeThinking {
		if length < 17 {
			length = 17
		}
	}
	if config.Seed != nil {
		if length < 19 {
			length = 19
		}
	}
	if imageConfig != nil {
		length = 27
	}
	if transcriptionConfig != nil {
		length = 32
	}
	wire := make([]any, length)
	if len(config.StopSequences) > 0 {
		wire[1] = append([]string(nil), config.StopSequences...)
	}
	if includeMaxOutput {
		wire[3] = maxOutput
	}
	if temperature != nil {
		wire[4] = *temperature
	}
	if topP != nil {
		wire[5] = *topP
	}
	if topK != nil {
		wire[6] = *topK
	}
	if config.ResponseMIMEType != "" {
		wire[7] = config.ResponseMIMEType
	}
	if responseSchema != nil {
		wire[8] = responseSchema
	}
	wire[13] = int64(1)
	if responseModalities != nil {
		wire[14] = responseModalities
	}
	if speechConfig != nil {
		wire[15] = speechConfig
	}
	if includeThinking {
		thinking := []any{int64(1)}
		if defaults.ThinkingLevel {
			thinking = []any{int64(1), nil, nil, thinkingLevel}
		}
		if thinkingBudget != nil {
			if len(thinking) < 2 {
				thinking = append(thinking, nil)
			}
			thinking[1] = *thinkingBudget
		}
		wire[16] = thinking
	}
	if config.Seed != nil {
		wire[18] = *config.Seed
	}
	if imageConfig != nil {
		wire[26] = imageConfig
	}
	if transcriptionConfig != nil {
		wire[31] = transcriptionConfig
	}
	return wire, nil
}

var thinkingLevelsByEffort = []int64{4, 1, 2, 3}

// normalizeReasoningEffort 统一各家客户端的思考强度写法，返回空串表示未设置（使用模型默认强度）
func normalizeReasoningEffort(value string) string {
	switch effort := strings.ToLower(strings.TrimSpace(value)); effort {
	case "none", "minimal", "low", "medium", "high":
		return effort
	case "xhigh", "max", "maximum", "highest":
		return "high"
	case "off", "disabled":
		return "none"
	case "min":
		return "minimal"
	default:
		return ""
	}
}

// thinkingBudgetForLevel 把思考强度换算为思考预算，供只支持预算的模型使用，档位与 thinkingLevelForBudget 对应
func thinkingBudgetForLevel(level int64) int64 {
	switch level {
	case 4:
		return 128
	case 1:
		return 1024
	case 2:
		return 8192
	default:
		return 24576
	}
}

// thinkingLevelForBudget 按 Gemini OpenAI 兼容层的 1024、8192、24576 档位把思考预算换算为 thinking level
func thinkingLevelForBudget(budget int64) int64 {
	switch {
	case budget <= 0:
		return 4
	case budget <= 1024:
		return 1
	case budget <= 8192:
		return 2
	default:
		return 3
	}
}

func closestSupportedThinkingLevel(requested int64, supported []int64) int64 {
	requestedRank := slices.Index(thinkingLevelsByEffort, requested)
	if requestedRank < 0 || len(supported) == 0 || slices.Contains(supported, requested) {
		return requested
	}
	for distance := 1; distance < len(thinkingLevelsByEffort); distance++ {
		lower := requestedRank - distance
		if lower >= 0 && slices.Contains(supported, thinkingLevelsByEffort[lower]) {
			return thinkingLevelsByEffort[lower]
		}
		upper := requestedRank + distance
		if upper < len(thinkingLevelsByEffort) && slices.Contains(supported, thinkingLevelsByEffort[upper]) {
			return thinkingLevelsByEffort[upper]
		}
	}
	return requested
}

func encodeResponseModalities(modalities []ResponseModality) ([]int64, error) {
	if modalities == nil {
		return nil, nil
	}
	hasText := false
	hasImage := false
	hasAudio := false
	for _, modality := range modalities {
		switch ResponseModality(strings.ToUpper(strings.TrimSpace(string(modality)))) {
		case ResponseModalityText:
			hasText = true
		case ResponseModalityImage:
			hasImage = true
		case ResponseModalityAudio:
			hasAudio = true
		default:
			return nil, fmt.Errorf("response modality %q 不受支持", modality)
		}
	}
	if hasAudio && (hasText || hasImage) {
		return nil, fmt.Errorf("AUDIO 不能和其他 response modality 同时使用")
	}
	switch {
	case hasAudio:
		return []int64{3}, nil
	case hasImage && hasText:
		return []int64{2, 1}, nil
	case hasImage:
		return []int64{2}, nil
	case hasText:
		return []int64{1}, nil
	default:
		return []int64{}, nil
	}
}

func encodeImageConfig(config *ImageConfig) []any {
	if config == nil {
		return nil
	}
	aspectRatio := strings.TrimSpace(config.AspectRatio)
	imageSize := strings.TrimSpace(config.ImageSize)
	if aspectRatio == "" && imageSize == "" {
		return nil
	}
	if imageSize == "" {
		return []any{aspectRatio}
	}
	var aspect any
	if aspectRatio != "" {
		aspect = aspectRatio
	}
	return []any{aspect, imageSize}
}

func encodeSpeechConfig(config *SpeechConfig) ([]any, error) {
	if config == nil {
		return nil, nil
	}
	voiceName := strings.TrimSpace(config.VoiceName)
	if voiceName != "" && len(config.Speakers) > 0 {
		return nil, fmt.Errorf("speech config 不能同时设置 voice 和 multi-speaker")
	}
	var wire []any
	if voiceName != "" {
		wire = []any{[]any{[]any{voiceName}}}
	}
	if len(config.Speakers) > 0 {
		speakers := make([]any, 0, len(config.Speakers))
		for index, speaker := range config.Speakers {
			name := strings.TrimSpace(speaker.Speaker)
			voice := strings.TrimSpace(speaker.VoiceName)
			if name == "" || voice == "" {
				return nil, fmt.Errorf("speech config speakers[%d] 需要 speaker 和 voiceName", index)
			}
			speakers = append(speakers, []any{name, []any{[]any{voice}}})
		}
		if wire == nil {
			wire = make([]any, 3)
		} else {
			for len(wire) < 3 {
				wire = append(wire, nil)
			}
		}
		multi := []any{nil, speakers}
		switch strings.ToUpper(strings.TrimSpace(config.Mode)) {
		case "":
		case "VERBATIM":
			multi = append(multi, int64(1))
		case "CONVERSATIONAL":
			multi = append(multi, int64(2))
		default:
			return nil, fmt.Errorf("speech config mode 必须是 VERBATIM 或 CONVERSATIONAL")
		}
		wire[2] = multi
	} else if strings.TrimSpace(config.Mode) != "" {
		return nil, fmt.Errorf("speech config mode 只能用于 multi-speaker")
	}
	return wire, nil
}

func applyModelMediaDefaults(config GenerationConfig, model Model) GenerationConfig {
	if model.Capabilities["image_route"] && imageModalityNeedsText(config.ResponseModalities) {
		// 图像输出同时请求文本模态
		config.ResponseModalities = []ResponseModality{ResponseModalityImage, ResponseModalityText}
		return config
	}
	if config.ResponseModalities != nil {
		return config
	}
	switch {
	case model.Capabilities["speech_route"], model.Capabilities["music_route"]:
		config.ResponseModalities = []ResponseModality{ResponseModalityAudio}
	case model.Capabilities["image_route"]:
		config.ResponseModalities = []ResponseModality{ResponseModalityImage, ResponseModalityText}
	}
	return config
}

// imageModalityNeedsText 判断图像模型请求是否缺 TEXT 模态
func imageModalityNeedsText(modalities []ResponseModality) bool {
	if modalities == nil {
		return true
	}
	hasImage := false
	hasText := false
	for _, modality := range modalities {
		switch ResponseModality(strings.ToUpper(strings.TrimSpace(string(modality)))) {
		case ResponseModalityImage:
			hasImage = true
		case ResponseModalityText:
			hasText = true
		default:
			return false
		}
	}
	return hasImage && !hasText
}

func applySpeechTranscript(contents []Content, model Model, config GenerationConfig) []Content {
	if !model.Capabilities["speech_route"] || config.SpeechConfig == nil {
		return contents
	}
	if model.Capabilities["speech_metadata"] {
		return splitSpeakerSegments(contents, config.SpeechConfig.Speakers)
	}
	result := foldSpeechMetadata(contents)
	for contentIndex, content := range result {
		parts := append([]Part(nil), content.Parts...)
		for partIndex, part := range parts {
			text := strings.TrimSpace(part.Text)
			if text == "" || strings.HasPrefix(text, "## Transcript:") {
				continue
			}
			parts[partIndex].Text = "## Transcript:\n" + text
			result[contentIndex].Parts = parts
			return result
		}
	}
	return result
}

// foldSpeechMetadata 把分段说话人与风格写回旧 TTS 模型的台词文本
func foldSpeechMetadata(contents []Content) []Content {
	result := append([]Content(nil), contents...)
	for contentIndex, content := range result {
		if !slices.ContainsFunc(content.Parts, func(part Part) bool { return part.SpeechMetadata != nil }) {
			continue
		}
		parts := append([]Part(nil), content.Parts...)
		for index, part := range parts {
			if part.SpeechMetadata == nil {
				continue
			}
			if part.SpeechMetadata.Speaker != "" {
				part.Text = part.SpeechMetadata.Speaker + ": " + part.Text
			}
			if part.SpeechMetadata.Style != "" {
				part.Text = part.SpeechMetadata.Style + "\n\n" + part.Text
			}
			part.SpeechMetadata = nil
			parts[index] = part
		}
		result[contentIndex].Parts = parts
	}
	return result
}

// splitSpeakerSegments 把 "说话人: 台词" 文本按多说话人配置拆成带 SpeechMetadata 的分段
func splitSpeakerSegments(contents []Content, speakers []SpeakerVoiceConfig) []Content {
	if len(speakers) == 0 {
		return contents
	}
	names := make([]string, 0, len(speakers))
	for _, speaker := range speakers {
		names = append(names, regexp.QuoteMeta(strings.TrimSpace(speaker.Speaker)))
	}
	pattern := regexp.MustCompile(`^\s*(` + strings.Join(names, "|") + `)\s*:\s*(.*)$`)
	result := append([]Content(nil), contents...)
	for contentIndex, content := range result {
		parts := make([]Part, 0, len(content.Parts))
		changed := false
		for _, part := range content.Parts {
			if part.Text == "" || part.SpeechMetadata != nil && part.SpeechMetadata.Speaker != "" {
				parts = append(parts, part)
				continue
			}
			segments := speakerSegments(part, pattern)
			if len(segments) == 0 {
				parts = append(parts, part)
				continue
			}
			parts = append(parts, segments...)
			changed = true
		}
		if changed {
			result[contentIndex].Parts = parts
		}
	}
	return result
}

// speakerSegments 返回文本中以说话人开头的各段台词，首个说话人之前的文本不进入分段
func speakerSegments(part Part, pattern *regexp.Regexp) []Part {
	style := ""
	if part.SpeechMetadata != nil {
		style = part.SpeechMetadata.Style
	}
	var segments []Part
	for _, line := range strings.Split(part.Text, "\n") {
		line = strings.TrimRight(line, "\r")
		if match := pattern.FindStringSubmatch(line); match != nil {
			segments = append(segments, Part{Text: match[2], SpeechMetadata: &SpeechMetadata{Speaker: match[1], Style: style}})
			continue
		}
		if len(segments) > 0 {
			segments[len(segments)-1].Text += "\n" + line
		}
	}
	result := segments[:0]
	for _, segment := range segments {
		segment.Text = strings.TrimSpace(segment.Text)
		if segment.Text != "" {
			result = append(result, segment)
		}
	}
	return result
}

func observedSafetySettings() []any {
	settings := make([]any, 0, 4)
	for category := int64(7); category <= 10; category++ {
		settings = append(settings, []any{nil, nil, category, int64(5)})
	}
	return settings
}

func (c *Client) Generate(ctx context.Context, request GenerateRequest) (<-chan Event, error) {
	lease, leased := AccountLeaseFromContext(ctx)
	build := leased && lease.Channel() == ChannelBuild
	var entry modelEntry
	var err error
	if build {
		entry, err = c.buildModelEntry(ctx, request, lease)
	} else {
		entry, err = c.modelEntry(ctx, request.AccountID, request.Model)
	}
	if err != nil {
		return nil, err
	}
	if err := validateRequestedTools(request.Tools, entry.model); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidArgument, err)
	}
	if err := validateTranscriptionConfig(request.Config.TranscriptionConfig, entry.model); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidArgument, err)
	}
	if request.Truncate && entry.model.InputTokenLimit > 0 {
		if request, err = c.truncateRequest(ctx, request, entry.model.InputTokenLimit); err != nil {
			return nil, err
		}
	}
	if entry.defaults.InteractionStream && !build {
		return c.generateInteraction(ctx, request, entry)
	}
	request.Config = applyModelMediaDefaults(request.Config, entry.model)
	request.ImageRoute = entry.defaults.ImageRoute
	request.Contents = applySpeechTranscript(request.Contents, entry.model, request.Config)
	wireRequest := request
	wireRequest.Config.StopSequences = nil
	var response *RPCResponse
	var decodeStream func(io.Reader, func(Event) error) error
	if build {
		response, decodeStream, err = c.sendBuild(ctx, wireRequest, entry)
	} else {
		response, decodeStream, err = c.sendPlayground(ctx, wireRequest, entry)
	}
	if err != nil {
		return nil, err
	}
	matcher := newStopSequenceMatcher(request.Config.StopSequences)
	var stopTokenCount <-chan tokenCountResult
	cancelTokenCount := func() {}
	if matcher != nil {
		countContext, cancel := context.WithCancel(ctx)
		cancelTokenCount = cancel
		results := make(chan tokenCountResult, 1)
		stopTokenCount = results
		countRequest := TokenCountRequest{
			Model: request.Model, System: request.System, Contents: request.Contents, Tools: request.Tools,
		}
		go func() {
			count, countErr := c.CountTokensForAccount(countContext, request.AccountID, countRequest)
			results <- tokenCountResult{count: count, err: countErr}
			close(results)
		}()
	}
	events := make(chan Event, 8)
	go func() {
		defer close(events)
		defer cancelTokenCount()
		stopClose := context.AfterFunc(ctx, func() {
			_ = response.Body.Close()
		})
		defer stopClose()
		send := func(event Event) error {
			// Build 通道的事件已带有上游报告的实际模型，其余事件标为本次请求的模型
			if event.ProviderModel == "" {
				event.ProviderModel = entry.model.ID
			}
			select {
			case events <- event:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		var usage *Usage
		// usageModel 为用量所在块标明的模型（Build 通道最后一块报告实际服务的模型），重发用量事件时保留
		usageModel := ""
		var finish *Event
		var output generatedOutputParts
		matchedStopSequence := ""
		emitEvent := func(event Event) error {
			switch event.Kind {
			case EventUsage:
				if event.Usage != nil {
					value := *event.Usage
					usage = &value
					usageModel = event.ProviderModel
				}
				return nil
			case EventFinish:
				value := event
				finish = &value
				return nil
			default:
				event = assignLocalCallID(event)
				output.observe(event)
				if request.Config.HideThinking && event.Kind == EventReasoning {
					if event.ThoughtSignature == "" {
						return nil
					}
					event = hiddenThought(event)
				}
				return send(event)
			}
		}
		emit := func(event Event) error {
			if matcher == nil {
				return emitEvent(event)
			}
			if pending := matcher.boundary(event.Kind); pending != "" {
				if err := emitEvent(Event{Kind: EventText, Text: pending, ProviderModel: event.ProviderModel}); err != nil {
					return err
				}
			}
			if event.Kind != EventText {
				return emitEvent(event)
			}
			text, matched := matcher.write(event.Text)
			if text != "" {
				event.Text = text
				if err := emitEvent(event); err != nil {
					return err
				}
			}
			if matched != "" {
				matchedStopSequence = matched
				return errStopSequenceMatched
			}
			return nil
		}
		err := decodeStream(observeStreamActivity(ctx, response.Body), emit)
		if errors.Is(err, errStopSequenceMatched) {
			_ = response.Body.Close()
			select {
			case result := <-stopTokenCount:
				if result.err == nil {
					usage = countedCompleteUsage(request, output, result.count)
					if err := send(Event{Kind: EventUsage, Usage: usage}); err != nil {
						return
					}
				}
			case <-ctx.Done():
				return
			}
			_ = send(Event{Kind: EventFinish, FinishReason: "stop_sequence", StopSequence: matchedStopSequence})
			return
		}
		if closeErr := response.Body.Close(); err == nil {
			err = closeErr
		}
		if ctx.Err() == nil && matcher != nil {
			if pending := matcher.flush(); pending != "" {
				if flushErr := emitEvent(Event{Kind: EventText, Text: pending}); err == nil {
					err = flushErr
				}
			}
		}
		if err != nil {
			if ctx.Err() == nil {
				_ = send(Event{Kind: EventError, Err: err})
			}
			return
		}
		if usage == nil {
			usage = localCompleteUsage(request, output)
		} else if usage.OutputTokensMissing {
			outputTokens := usage.TotalTokens - usage.InputTokens - usage.ToolTokens - usage.ReasoningTokens
			if outputTokens < 0 {
				outputTokens = localPartsTokens(output.visible)
			}
			usage.OutputTokens = outputTokens
			usage.OutputTokensMissing = false
		}
		if err := send(Event{Kind: EventUsage, Usage: usage, ProviderModel: usageModel}); err != nil {
			return
		}
		if finish != nil {
			_ = send(*finish)
		}
	}()
	return events, nil
}

// sendPlayground 编码并发送 Playground GenerateContent，返回响应与含完成帧校验的流解码
func (c *Client) sendPlayground(ctx context.Context, request GenerateRequest, entry modelEntry) (*RPCResponse, func(io.Reader, func(Event) error) error, error) {
	runtime := RequestContext{}
	if c.contextProvider != nil {
		var err error
		runtime, err = c.contextProvider.RequestContext(ctx, request.AccountID)
		if err != nil {
			return nil, nil, fmt.Errorf("读取 AI Studio 请求上下文: %w", err)
		}
	}
	body, err := EncodeGenerateContentRequest(request, entry.defaults, runtime)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %v", ErrInvalidArgument, err)
	}
	response, err := c.doProtected(ctx, request, body)
	if err != nil {
		return nil, nil, err
	}
	decoder := NewFrameDecoder()
	return response, func(source io.Reader, emit func(Event) error) error {
		if err := DecodeGenerateStream(source, decoder, emit); err != nil {
			return err
		}
		return decoder.End()
	}, nil
}

// DecodeGenerateStream 按网络到达顺序解码 GenerateContent repeated 帧
func DecodeGenerateStream(source io.Reader, decoder *FrameDecoder, emit func(Event) error) error {
	return decodeGenerateItems(source, func(raw json.RawMessage) error {
		events, err := decoder.Decode(raw)
		if err != nil {
			return err
		}
		for _, event := range events {
			if err := emit(event); err != nil {
				return err
			}
		}
		return nil
	})
}

// applyOutputFloor 客户端设置的最大输出低于下限时提高到下限，且不超过模型上限。
// Gemini 的思考也算在最大输出里：客户端设得太小（如 50、10）时，思考就把额度用完，正文被截断甚至为空。
// 客户端没有设置时沿用模型默认值（即模型上限），不做改动
func applyOutputFloor(config GenerationConfig, floor int64, modelLimit int64) GenerationConfig {
	if floor <= 0 || config.MaxOutputTokens == nil {
		return config
	}
	target := floor
	if modelLimit > 0 && target > modelLimit {
		target = modelLimit
	}
	if *config.MaxOutputTokens >= target {
		return config
	}
	value := target
	config.MaxOutputTokens = &value
	return config
}

// hiddenThought 把不返回正文的思考事件换成只带签名的事件，多轮工具调用需要的签名不丢失
func hiddenThought(event Event) Event {
	return Event{
		Kind: EventThoughtSignature, ThoughtSignature: event.ThoughtSignature,
		Usage: event.Usage, ProviderModel: event.ProviderModel,
	}
}

// truncateMaxCounts 为一次截断最多调用 CountTokens 的次数；用完后按当前内容发送，由上游判定是否超限
const truncateMaxCounts = 6

// truncateRequest 在输入超过模型上下文窗口时按权威计数删除最早的完整对话轮次（Responses truncation=auto）。
// 本地估算不到上限一半时不计数，避免每个请求都多一次 CountTokens；超出时按超出比例一次删除若干轮
func (c *Client) truncateRequest(ctx context.Context, request GenerateRequest, limit int64) (GenerateRequest, error) {
	if EstimatedInputTokens(request)*2 < limit {
		return request, nil
	}
	for attempt := 0; attempt < truncateMaxCounts; attempt++ {
		count, err := c.CountTokensForAccount(ctx, request.AccountID, TokenCountRequest{
			Model: request.Model, System: request.System, Contents: request.Contents, Tools: request.Tools,
		})
		if err != nil {
			return request, err
		}
		if count.InputTokens <= limit {
			return request, nil
		}
		estimate := EstimatedInputTokens(request)
		excess := max(estimate*(count.InputTokens-limit)/count.InputTokens, 1)
		var removed int64
		for removed < excess {
			start := nextConversationTurn(request.Contents)
			if start < 0 {
				return request, fmt.Errorf("%w: 最新一轮对话已超过模型上下文窗口 %d", ErrInvalidArgument, limit)
			}
			removed += localContentsTokens(request.Contents[:start])
			request.Contents = request.Contents[start:]
		}
	}
	return request, nil
}

// nextConversationTurn 返回下一轮用户消息的位置（跳过工具结果，保持工具调用与结果的配对）；没有时返回 -1
func nextConversationTurn(contents []Content) int {
	for index := 1; index < len(contents); index++ {
		if contents[index].Role == RoleUser && !slices.ContainsFunc(contents[index].Parts, func(part Part) bool { return part.FunctionResult != nil }) {
			return index
		}
	}
	return -1
}
