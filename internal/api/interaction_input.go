package api

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// interactionRequest 表示公开 Interactions 创建请求
type interactionRequest struct {
	Model      string          `json:"model"`
	Input      json.RawMessage `json:"input"`
	System     string          `json:"system_instruction"`
	Stream     bool            `json:"stream"`
	Store      *bool           `json:"store"`
	PreviousID string          `json:"previous_interaction_id"`
	Background bool            `json:"background"`
	Formats    json.RawMessage `json:"response_format"`
	Tools      []responsesTool `json:"tools"`
	Generation struct {
		Temperature       *float64        `json:"temperature"`
		TopP              *float64        `json:"top_p"`
		TopK              *int            `json:"top_k"`
		MaxOutputTokens   *int64          `json:"max_output_tokens"`
		Seed              *int64          `json:"seed"`
		StopSequences     []string        `json:"stop_sequences"`
		ThinkingLevel     string          `json:"thinking_level"`
		ThinkingSummaries string          `json:"thinking_summaries"`
		Speech            json.RawMessage `json:"speech_config"`
		ToolChoice        json.RawMessage `json:"tool_choice"`
	} `json:"generation_config"`
}

// interactionFormat 表示文本、图片与音频的返回配置
type interactionFormat struct {
	Type        string          `json:"type"`
	MIME        string          `json:"mime_type"`
	Schema      json.RawMessage `json:"schema"`
	SampleRate  int             `json:"sample_rate"`
	BitRate     int             `json:"bit_rate"`
	Delivery    string          `json:"delivery"`
	AspectRatio string          `json:"aspect_ratio"`
	ImageSize   string          `json:"image_size"`
}

// interactionInput 表示输入内容块或执行步骤
type interactionInput struct {
	Type        string             `json:"type"`
	Text        *string            `json:"text"`
	Data        string             `json:"data"`
	URI         string             `json:"uri"`
	MIME        string             `json:"mime_type"`
	Content     []interactionInput `json:"content"`
	Summary     []interactionInput `json:"summary"`
	ID          string             `json:"id"`
	CallID      string             `json:"call_id"`
	Name        string             `json:"name"`
	Arguments   json.RawMessage    `json:"arguments"`
	Result      json.RawMessage    `json:"result"`
	Signature   string             `json:"signature"`
	Annotations []struct {
		Type    string `json:"type"`
		Speaker string `json:"speaker"`
		Style   string `json:"style"`
	} `json:"annotations"`
}

// interactionList 解析协议中的单对象或数组联合字段
func interactionList[T any](raw json.RawMessage) ([]T, error) {
	value := strings.TrimSpace(string(raw))
	if value == "" || value == "null" {
		return nil, nil
	}
	var values []T
	if strings.HasPrefix(value, "[") {
		err := json.Unmarshal(raw, &values)
		return values, err
	}
	var item T
	if err := json.Unmarshal(raw, &item); err != nil {
		return nil, err
	}
	return []T{item}, nil
}

// toGenerateRequest 映射模型、结构化输入、生成参数与函数声明。
// 只要求模型：没有输入时由处理函数结合系统指令与续接历史判断是否为空请求
func (request interactionRequest) toGenerateRequest(id string) (aistudio.GenerateRequest, error) {
	generate := aistudio.GenerateRequest{ID: id, Model: strings.TrimPrefix(strings.TrimSpace(request.Model), "models/"), System: request.System}
	if generate.Model == "" {
		return generate, fmt.Errorf("model is required")
	}
	if request.Background {
		return generate, fmt.Errorf("background must be false")
	}
	var err error
	generate.Contents, err = interactionContents(request.Input)
	if err != nil {
		return generate, err
	}
	generation := request.Generation
	generate.Config = aistudio.GenerationConfig{
		Temperature: generation.Temperature, TopP: generation.TopP, TopK: generation.TopK,
		MaxOutputTokens: generation.MaxOutputTokens, Seed: generation.Seed,
		StopSequences: normalizeStopSequences(generation.StopSequences), ReasoningEffort: generation.ThinkingLevel,
	}
	if generation.ThinkingSummaries != "" && generation.ThinkingSummaries != "auto" && generation.ThinkingSummaries != "none" {
		return generate, fmt.Errorf("thinking_summaries must be auto or none")
	}
	generate.Config.SpeechConfig, err = interactionSpeech(generation.Speech)
	if err != nil {
		return generate, err
	}
	formats, err := interactionList[interactionFormat](request.Formats)
	if err != nil {
		return generate, fmt.Errorf("response_format: %w", err)
	}
	for _, format := range formats {
		if format.Delivery != "" && format.Delivery != "inline" {
			return generate, fmt.Errorf("response_format.delivery must be inline")
		}
		switch format.Type {
		case "text":
			generate.Config.ResponseModalities = append(generate.Config.ResponseModalities, aistudio.ResponseModalityText)
			if format.MIME != "" && format.MIME != "text/plain" && format.MIME != "application/json" {
				return generate, fmt.Errorf("text mime_type must be text/plain or application/json")
			}
			generate.Config.ResponseMIMEType = format.MIME
			// schema 写成 null 时按未设置处理，与 Gemini responseSchema 一致
			if geminiRawObjectPresent(format.Schema) {
				if format.MIME != "application/json" {
					return generate, fmt.Errorf("response_format.schema requires application/json")
				}
				generate.Config.ResponseSchema = format.Schema
			}
		case "audio":
			if format.MIME != "" && format.MIME != "audio/wav" && format.MIME != "audio/l16" {
				return generate, fmt.Errorf("audio mime_type must be audio/wav or audio/l16")
			}
			if format.SampleRate != 0 && format.SampleRate != 24000 || format.BitRate != 0 {
				return generate, fmt.Errorf("audio output uses 24000 Hz PCM with no bit_rate setting")
			}
			generate.Config.ResponseModalities = append(generate.Config.ResponseModalities, aistudio.ResponseModalityAudio)
		case "image":
			if format.MIME != "" {
				return generate, fmt.Errorf("image mime_type is selected by the model")
			}
			generate.Config.ResponseModalities = append(generate.Config.ResponseModalities, aistudio.ResponseModalityImage)
			generate.Config.ImageConfig = &aistudio.ImageConfig{AspectRatio: format.AspectRatio, ImageSize: format.ImageSize}
		default:
			return generate, fmt.Errorf("unsupported response_format type %q", format.Type)
		}
	}
	tools := append([]responsesTool(nil), request.Tools...)
	for i := range tools {
		switch tools[i].Type {
		case "function", "url_context", "google_maps":
		case "google_search":
			tools[i].Type = "web_search"
		case "code_execution":
			tools[i].Type = "code_interpreter"
		default:
			return generate, fmt.Errorf("unsupported interaction tool %q", tools[i].Type)
		}
	}
	if rawJSONConfigured(generation.ToolChoice) {
		var mode string
		if json.Unmarshal(generation.ToolChoice, &mode) != nil || mode != "auto" && mode != "none" {
			return generate, fmt.Errorf("generation_config.tool_choice must be auto or none")
		}
	}
	generate.Tools, err = mapResponsesTools(tools, generation.ToolChoice)
	return generate, err
}

// interactionContents 保留输入步骤顺序并合并同一角色的内容块。
// thought 步骤只带签名：签名挂到随后的模型内容或函数调用上，随后是用户内容时作为纯签名 Part 留在前一轮模型消息末尾
func interactionContents(raw json.RawMessage) ([]aistudio.Content, error) {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		if strings.TrimSpace(text) == "" {
			return nil, nil
		}
		return []aistudio.Content{{Role: aistudio.RoleUser, Parts: []aistudio.Part{{Text: text}}}}, nil
	}
	items, err := interactionList[interactionInput](raw)
	if err != nil {
		return nil, fmt.Errorf("input: %w", err)
	}
	var contents []aistudio.Content
	pendingSignature := ""
	for _, item := range items {
		role := aistudio.RoleUser
		var parts []aistudio.Part
		switch item.Type {
		case "user_input", "model_output":
			if item.Type == "model_output" {
				role = aistudio.RoleAssistant
			}
			for _, content := range item.Content {
				mapped, mapErr := content.parts()
				if mapErr != nil {
					return nil, mapErr
				}
				parts = append(parts, mapped...)
			}
		case "function_call":
			// id 必须回传：函数结果只能按 call_id 对应回调用（本服务输出的调用一定带 id，上游没给时为本地 ID）
			if item.ID == "" || item.Name == "" {
				return nil, fmt.Errorf("function_call requires id and name")
			}
			arguments, mapErr := geminiJSONObject(item.Arguments, "function_call.arguments")
			if mapErr != nil {
				return nil, mapErr
			}
			role = aistudio.RoleAssistant
			parts = []aistudio.Part{{
				FunctionCall:     &aistudio.FunctionCall{ID: item.ID, Name: item.Name, Arguments: arguments, ThoughtSignature: pendingSignature},
				ThoughtSignature: pendingSignature,
			}}
			pendingSignature = ""
		case "function_result":
			if item.CallID == "" || len(item.Result) == 0 {
				return nil, fmt.Errorf("function_result requires call_id and result")
			}
			// 结果中以 Base64 内嵌的图片与文件取出来作为真正的媒体发送，与其他协议的工具结果一致
			cleaned, media, mapErr := splitFunctionResultMedia(item.Result)
			if mapErr != nil {
				return nil, fmt.Errorf("function_result: %w", mapErr)
			}
			result, mapErr := normalizeFunctionResultContent(cleaned)
			if mapErr != nil {
				return nil, fmt.Errorf("function_result: %w", mapErr)
			}
			role = aistudio.RoleTool
			parts = append([]aistudio.Part{{FunctionResult: &aistudio.FunctionResult{ID: item.CallID, Name: item.Name, Content: result}}}, media...)
		case "thought":
			if item.Signature == "" {
				continue
			}
			// 连续的 thought 步骤各带签名时，前一个签名作为纯签名 Part 先放进模型消息，不被后一个覆盖
			if pendingSignature != "" {
				contents = appendInteractionParts(contents, aistudio.RoleAssistant, []aistudio.Part{{ThoughtSignature: pendingSignature}})
			}
			pendingSignature = item.Signature
			continue
		default:
			parts, err = item.parts()
			if err != nil {
				return nil, err
			}
		}
		if len(parts) == 0 {
			continue
		}
		if pendingSignature != "" {
			if role == aistudio.RoleAssistant {
				parts[0].ThoughtSignature = pendingSignature
			} else if len(contents) > 0 && contents[len(contents)-1].Role == aistudio.RoleAssistant {
				contents = appendInteractionParts(contents, aistudio.RoleAssistant, []aistudio.Part{{ThoughtSignature: pendingSignature}})
			}
			pendingSignature = ""
		}
		contents = appendInteractionParts(contents, role, parts)
	}
	if pendingSignature != "" && len(contents) > 0 && contents[len(contents)-1].Role == aistudio.RoleAssistant {
		contents = appendInteractionParts(contents, aistudio.RoleAssistant, []aistudio.Part{{ThoughtSignature: pendingSignature}})
	}
	return contents, nil
}

// appendInteractionParts 把内容块并入同一角色的上一轮，角色不同时新起一轮
func appendInteractionParts(contents []aistudio.Content, role aistudio.Role, parts []aistudio.Part) []aistudio.Content {
	if len(contents) > 0 && contents[len(contents)-1].Role == role {
		contents[len(contents)-1].Parts = append(contents[len(contents)-1].Parts, parts...)
		return contents
	}
	return append(contents, aistudio.Content{Role: role, Parts: parts})
}

// parts 复用 Gemini 的文本、媒体与文件输入映射
func (content interactionInput) parts() ([]aistudio.Part, error) {
	var part geminiPart
	switch content.Type {
	case "text":
		if content.Text == nil {
			return nil, fmt.Errorf("text content requires text")
		}
		part.Text = content.Text
		for _, annotation := range content.Annotations {
			if annotation.Type == "speech_metadata" {
				part.SpeechMetadata = &aistudio.SpeechMetadata{Speaker: annotation.Speaker, Style: annotation.Style}
			}
		}
	case "image", "audio", "video", "document":
		if content.MIME == "" || (content.Data == "") == (content.URI == "") {
			return nil, fmt.Errorf("%s content requires mime_type and exactly one of data or uri", content.Type)
		}
		if content.Data != "" {
			part.InlineData = &geminiBlobPart{MIMEType: content.MIME, Data: content.Data}
		} else {
			part.FileData = &geminiFilePart{MIMEType: content.MIME, FileURI: content.URI}
		}
	default:
		return nil, fmt.Errorf("unsupported input type %q", content.Type)
	}
	parts, _, err := mapGeminiParts([]geminiPart{part})
	return parts, err
}

// interactionSpeech 转换单人声音列表与多说话人配置
func interactionSpeech(raw json.RawMessage) (*aistudio.SpeechConfig, error) {
	if !rawJSONConfigured(raw) {
		return nil, nil
	}
	type voice struct {
		Voice    string `json:"voice"`
		Speaker  string `json:"speaker"`
		Language string `json:"language"`
	}
	var config struct {
		Mode     string  `json:"mode"`
		Speakers []voice `json:"speakers"`
	}
	if strings.HasPrefix(strings.TrimSpace(string(raw)), "[") {
		if err := json.Unmarshal(raw, &config.Speakers); err != nil {
			return nil, fmt.Errorf("speech_config: %w", err)
		}
	} else if err := json.Unmarshal(raw, &config); err != nil {
		return nil, fmt.Errorf("speech_config: %w", err)
	}
	if len(config.Speakers) == 0 || len(config.Speakers) > 2 {
		return nil, fmt.Errorf("speech_config requires one or two voices")
	}
	speech := &aistudio.SpeechConfig{Mode: strings.ToUpper(config.Mode)}
	for _, speaker := range config.Speakers {
		if strings.TrimSpace(speaker.Voice) == "" {
			return nil, fmt.Errorf("speech_config voice is required")
		}
		if speaker.Language != "" {
			return nil, fmt.Errorf("speech language is determined by the input text")
		}
		if len(config.Speakers) == 1 && speaker.Speaker == "" {
			speech.VoiceName = speaker.Voice
		} else {
			if strings.TrimSpace(speaker.Speaker) == "" {
				return nil, fmt.Errorf("speech_config speaker is required for named voices")
			}
			speech.Speakers = append(speech.Speakers, aistudio.SpeakerVoiceConfig{Speaker: speaker.Speaker, VoiceName: speaker.Voice})
		}
	}
	return speech, nil
}

// audioFormat 按请求模态选择完整 WAV 或流式 PCM
func (request interactionRequest) audioFormat() string {
	formats, _ := interactionList[interactionFormat](request.Formats)
	for _, format := range formats {
		if format.Type == "audio" {
			if format.MIME == "audio/wav" {
				return "wav"
			}
			if format.MIME == "audio/l16" {
				return "pcm"
			}
		}
	}
	if request.Stream {
		return "pcm"
	}
	return "wav"
}
