package api

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

type geminiRequest struct {
	Contents          []geminiContent        `json:"contents"`
	SystemInstruction *geminiContent         `json:"systemInstruction"`
	GenerationConfig  geminiGenerationConfig `json:"generationConfig"`
	Tools             []geminiToolGroup      `json:"tools"`
	ToolConfig        geminiToolConfig       `json:"toolConfig"`
	// 兼容写法：snake_case 字段名（Gemini 官方文档的 curl 示例就用 system_instruction，官方接口两种写法都接受），
	// 以及 countTokens 的 generateContentRequest 包装
	SystemInstructionSnake *geminiContent  `json:"system_instruction"`
	GenerationConfigSnake  json.RawMessage `json:"generation_config"`
	ToolConfigSnake        json.RawMessage `json:"tool_config"`
	GenerateContentRequest *geminiRequest  `json:"generateContentRequest"`
	// requests 为批量写法（{"requests": [{...}]}），content 为单数写法；只支持一条请求
	Requests []geminiRequest `json:"requests"`
	Content  *geminiContent  `json:"content"`
}

type geminiContent struct {
	Role  string       `json:"role"`
	Parts []geminiPart `json:"parts"`
}

type geminiBlobPart struct {
	MIMEType      string `json:"mimeType"`
	MIMETypeSnake string `json:"mime_type"`
	Data          string `json:"data"`
}

func (b *geminiBlobPart) MIME() string {
	if b == nil {
		return ""
	}
	if b.MIMEType != "" {
		return b.MIMEType
	}
	return b.MIMETypeSnake
}

type geminiFilePart struct {
	MIMEType      string `json:"mimeType"`
	MIMETypeSnake string `json:"mime_type"`
	FileURI       string `json:"fileUri"`
	FileURISnake  string `json:"file_uri"`
	DisplayName   string `json:"displayName"`
}

func (f *geminiFilePart) MIME() string {
	if f == nil {
		return ""
	}
	if f.MIMEType != "" {
		return f.MIMEType
	}
	return f.MIMETypeSnake
}

func (f *geminiFilePart) URI() string {
	if f == nil {
		return ""
	}
	if f.FileURI != "" {
		return f.FileURI
	}
	return f.FileURISnake
}

type geminiPart struct {
	Text             *string         `json:"text"`
	Thought          bool            `json:"thought"`
	ThoughtSignature string          `json:"thoughtSignature"`
	InlineData       *geminiBlobPart `json:"inlineData"`
	InlineDataSnake  *geminiBlobPart `json:"inline_data"`
	FileData         *geminiFilePart `json:"fileData"`
	FileDataSnake    *geminiFilePart `json:"file_data"`
	FunctionCall     *struct {
		ID   string          `json:"id"`
		Name string          `json:"name"`
		Args json.RawMessage `json:"args"`
	} `json:"functionCall"`
	FunctionResponse *struct {
		ID       string          `json:"id"`
		Name     string          `json:"name"`
		Response json.RawMessage `json:"response"`
	} `json:"functionResponse"`
	ExecutableCode *struct {
		Language string `json:"language"`
		Code     string `json:"code"`
	} `json:"executableCode"`
	CodeExecutionResult *struct {
		Outcome string `json:"outcome"`
		Output  string `json:"output"`
		Error   string `json:"error"`
	} `json:"codeExecutionResult"`
	SpeechMetadata      *aistudio.SpeechMetadata `json:"speechMetadata"`
	SpeechMetadataSnake *aistudio.SpeechMetadata `json:"speech_metadata"`
}

func (p geminiPart) speechMetadata() *aistudio.SpeechMetadata {
	metadata := p.SpeechMetadata
	if metadata == nil {
		metadata = p.SpeechMetadataSnake
	}
	if metadata == nil || strings.TrimSpace(metadata.Speaker) == "" && strings.TrimSpace(metadata.Style) == "" {
		return nil
	}
	return &aistudio.SpeechMetadata{Speaker: strings.TrimSpace(metadata.Speaker), Style: strings.TrimSpace(metadata.Style)}
}

func (p geminiPart) inline() *geminiBlobPart {
	if p.InlineData != nil {
		return p.InlineData
	}
	return p.InlineDataSnake
}

func (p geminiPart) file() *geminiFilePart {
	if p.FileData != nil {
		return p.FileData
	}
	return p.FileDataSnake
}

type geminiGenerationConfig struct {
	Temperature         *float64                   `json:"temperature"`
	TopP                *float64                   `json:"topP"`
	TopK                *int                       `json:"topK"`
	FrequencyPenalty    *float64                   `json:"frequencyPenalty"`
	PresencePenalty     *float64                   `json:"presencePenalty"`
	CandidateCount      *int64                     `json:"candidateCount"`
	ResponseLogprobs    *bool                      `json:"responseLogprobs"`
	Logprobs            *int64                     `json:"logprobs"`
	MaxOutputTokens     *int64                     `json:"maxOutputTokens"`
	StopSequences       []string                   `json:"stopSequences"`
	ResponseMIMEType    string                     `json:"responseMimeType"`
	ResponseSchema      json.RawMessage            `json:"responseSchema"`
	ResponseJSONSchema  json.RawMessage            `json:"responseJsonSchema"`
	ResponseModalities  []string                   `json:"responseModalities"`
	ImageConfig         *geminiImageConfig         `json:"imageConfig"`
	SpeechConfig        *geminiSpeechConfig        `json:"speechConfig"`
	TranscriptionConfig *geminiTranscriptionConfig `json:"transcriptionConfig"`
	Seed                *int64                     `json:"seed"`
	ThinkingConfig      *struct {
		ThinkingBudget  *int64 `json:"thinkingBudget"`
		ThinkingLevel   string `json:"thinkingLevel"`
		IncludeThoughts *bool  `json:"includeThoughts"`
	} `json:"thinkingConfig"`
}

type geminiTranscriptionConfig struct {
	LanguageCodes      []string `json:"languageCodes"`
	CustomVocabulary   []string `json:"customVocabulary"`
	WordTimestamps     *bool    `json:"wordTimestamps"`
	SpeakerLabels      *bool    `json:"speakerLabels"`
	SmartTranscription bool     `json:"smartTranscription"`
}

type geminiImageConfig struct {
	AspectRatio string `json:"aspectRatio"`
	ImageSize   string `json:"imageSize"`
}

type geminiVoiceConfig struct {
	PrebuiltVoiceConfig *struct {
		VoiceName string `json:"voiceName"`
	} `json:"prebuiltVoiceConfig"`
}

type geminiSpeakerVoiceConfig struct {
	Speaker     string            `json:"speaker"`
	VoiceConfig geminiVoiceConfig `json:"voiceConfig"`
}

type geminiSpeechConfig struct {
	VoiceConfig             *geminiVoiceConfig `json:"voiceConfig"`
	MultiSpeakerVoiceConfig *struct {
		Mode                string                     `json:"mode"`
		SpeakerVoiceConfigs []geminiSpeakerVoiceConfig `json:"speakerVoiceConfigs"`
	} `json:"multiSpeakerVoiceConfig"`
}

type geminiToolGroup struct {
	FunctionDeclarations []struct {
		Name                 string          `json:"name"`
		Description          string          `json:"description"`
		Parameters           json.RawMessage `json:"parameters"`
		ParametersJSONSchema json.RawMessage `json:"parametersJsonSchema"`
	} `json:"functionDeclarations"`
	GoogleSearch          json.RawMessage `json:"googleSearch"`
	GoogleSearchRetrieval json.RawMessage `json:"googleSearchRetrieval"`
	URLContext            json.RawMessage `json:"urlContext"`
	CodeExecution         json.RawMessage `json:"codeExecution"`
	GoogleMaps            json.RawMessage `json:"googleMaps"`
	ImageSearch           json.RawMessage `json:"imageSearch"`
}

type geminiToolConfig struct {
	FunctionCallingConfig struct {
		Mode                 string   `json:"mode"`
		AllowedFunctionNames []string `json:"allowedFunctionNames"`
	} `json:"functionCallingConfig"`
}

func (s *server) handleGeminiModels(w http.ResponseWriter, r *http.Request) {
	models, err := s.service.Models(r.Context())
	if err != nil {
		if shouldWriteRequestError(r, err) {
			writeGeminiRequestError(w, err)
		}
		return
	}
	models = expandModelAliases(models)
	data := make([]map[string]any, 0, len(models))
	for _, model := range models {
		data = append(data, geminiModelObject(model))
	}
	writeJSON(w, http.StatusOK, map[string]any{"models": data})
}

func (s *server) handleGeminiModel(w http.ResponseWriter, r *http.Request) {
	modelID := strings.TrimPrefix(r.PathValue("model"), "models/")
	models, err := s.service.Models(r.Context())
	if err != nil {
		if shouldWriteRequestError(r, err) {
			writeGeminiRequestError(w, err)
		}
		return
	}
	if model, ok := lookupPublicModel(models, modelID); ok {
		writeJSON(w, http.StatusOK, geminiModelObject(model))
		return
	}
	writeGeminiError(w, http.StatusNotFound, "NOT_FOUND", fmt.Sprintf("model %q is unavailable", modelID))
}

func geminiModelObject(model aistudio.Model) map[string]any {
	item := map[string]any{
		"name":                       "models/" + model.ID,
		"displayName":                model.Name,
		"description":                model.Description,
		"supportedGenerationMethods": model.Methods,
		"inputTokenLimit":            model.InputTokenLimit,
		"outputTokenLimit":           model.OutputTokenLimit,
	}
	if len(model.Capabilities) > 0 {
		item["capabilities"] = model.Capabilities
	}
	if len(model.CapabilityOptions) > 0 {
		item["capabilityOptions"] = model.CapabilityOptions
	}
	if len(model.AccessModes) > 0 {
		item["accessModes"] = model.AccessModes
	}
	if len(model.Channels) > 0 {
		item["channels"] = model.Channels
	}
	if model.Paid {
		item["paid"] = true
	}
	return item
}

func (s *server) handleGeminiAction(w http.ResponseWriter, r *http.Request) {
	action := r.PathValue("action")
	separator := strings.LastIndex(action, ":")
	if separator < 1 {
		writeGeminiError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "expected models/{model}:{method}")
		return
	}
	model := strings.TrimPrefix(action[:separator], "models/")
	method := action[separator+1:]
	if method == "predictLongRunning" {
		s.handleGeminiVideoCreate(w, r, model)
		return
	}
	switch method {
	case "countTokens", "generateContent", "streamGenerateContent":
	default:
		// 先判断方法：embedContent 等不支持的方法直接说明，不再被误报成 contents is required
		writeGeminiError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "unknown method: "+method)
		return
	}
	// 小请求体先读进内存：请求里没有内容时，能在管理日志里列出它的顶层字段，判断客户端发的是什么
	var smallBody []byte
	if r.ContentLength > 0 && r.ContentLength <= geminiSmallBodyLimit {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			writeGeminiInvalid(w, err)
			return
		}
		smallBody = body
		r.Body = io.NopCloser(bytes.NewReader(body))
	}
	var request geminiRequest
	if err := decodeJSON(r, &request); err != nil {
		writeGeminiInvalid(w, err)
		return
	}
	if err := request.normalizeVariants(); err != nil {
		writeGeminiInvalid(w, err)
		return
	}
	// 只有 systemInstruction、没有 contents 的请求（中转把 OpenAI 格式里只有 system 的对话转过来时就是这样）交给
	// 生成服务，按系统提示作为用户消息处理；两者都没有才报错
	if len(request.Contents) == 0 && request.SystemInstruction == nil {
		// 管理日志里记下请求体大小与工具数（不含内容），客户端仍只收到官方措辞
		recordResponseError(w, fmt.Errorf("contents is required（方法 %s，请求体 %d 字节，没有 contents 与 systemInstruction；顶层字段：%s）",
			method, r.ContentLength, geminiTopLevelKeys(smallBody)), "")
		writeGeminiError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "contents is required")
		return
	}
	generateRequest, err := request.toGenerateRequest(newID("resp"), model)
	if err != nil {
		writeGeminiInvalid(w, err)
		return
	}
	switch method {
	case "countTokens":
		s.handleGeminiCountTokens(w, r, generateRequest)
	case "generateContent":
		s.handleGeminiGenerate(w, r, generateRequest, false)
	case "streamGenerateContent":
		s.handleGeminiGenerate(w, r, generateRequest, true)
	default:
		writeGeminiError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "unknown method: "+method)
	}
}

func (request geminiRequest) toGenerateRequest(id string, model string) (aistudio.GenerateRequest, error) {
	if err := request.GenerationConfig.validate(); err != nil {
		return aistudio.GenerateRequest{}, err
	}
	var system string
	if request.SystemInstruction != nil {
		parts, _, err := mapGeminiParts(nonEmptyGeminiParts(request.SystemInstruction.Parts))
		if err != nil {
			return aistudio.GenerateRequest{}, fmt.Errorf("systemInstruction: %w", err)
		}
		var text strings.Builder
		for _, part := range parts {
			if part.Text == "" && (part.InlineData != nil || part.File != nil || part.FunctionCall != nil || part.FunctionResult != nil) {
				return aistudio.GenerateRequest{}, fmt.Errorf("systemInstruction must contain text")
			}
			text.WriteString(part.Text)
		}
		system = text.String()
	}
	contents := make([]aistudio.Content, 0, len(request.Contents))
	for _, content := range request.Contents {
		parts, hasResult, err := mapGeminiParts(content.Parts)
		if err != nil {
			return aistudio.GenerateRequest{}, err
		}
		if len(parts) == 0 {
			continue
		}
		role, err := geminiRole(content.Role)
		if err != nil {
			return aistudio.GenerateRequest{}, err
		}
		if hasResult {
			role = aistudio.RoleTool
		}
		contents = append(contents, aistudio.Content{Role: role, Parts: parts})
	}
	tools, err := mapGeminiTools(request.Tools, request.ToolConfig)
	if err != nil {
		return aistudio.GenerateRequest{}, err
	}
	config := aistudio.GenerationConfig{
		Temperature:      request.GenerationConfig.Temperature,
		TopP:             request.GenerationConfig.TopP,
		TopK:             request.GenerationConfig.TopK,
		MaxOutputTokens:  request.GenerationConfig.MaxOutputTokens,
		StopSequences:    normalizeStopSequences(request.GenerationConfig.StopSequences),
		ResponseMIMEType: request.GenerationConfig.ResponseMIMEType,
		Seed:             request.GenerationConfig.Seed,
	}
	config.ResponseModalities, err = mapGeminiResponseModalities(request.GenerationConfig.ResponseModalities)
	if err != nil {
		return aistudio.GenerateRequest{}, err
	}
	if image := request.GenerationConfig.ImageConfig; image != nil {
		config.ImageConfig = &aistudio.ImageConfig{AspectRatio: image.AspectRatio, ImageSize: image.ImageSize}
	}
	config.SpeechConfig, err = mapGeminiSpeechConfig(request.GenerationConfig.SpeechConfig)
	if err != nil {
		return aistudio.GenerateRequest{}, err
	}
	config.TranscriptionConfig, err = mapGeminiTranscriptionConfig(request.GenerationConfig.TranscriptionConfig)
	if err != nil {
		return aistudio.GenerateRequest{}, err
	}
	// responseSchema/responseJsonSchema 写成 null 时按未设置处理，只保留 JSON 模式
	if geminiRawObjectPresent(request.GenerationConfig.ResponseSchema) {
		config.ResponseSchema = request.GenerationConfig.ResponseSchema
	}
	if geminiRawObjectPresent(request.GenerationConfig.ResponseJSONSchema) {
		config.ResponseSchema = request.GenerationConfig.ResponseJSONSchema
	}
	if request.GenerationConfig.ThinkingConfig != nil {
		thinking := request.GenerationConfig.ThinkingConfig
		// thinkingBudget=-1 是 Gemini API 的“动态思考”，按未设置处理，使用模型默认强度。
		// 原先原样传下去，-1 被当作明确的低预算，Gemini 3 上落到最低思考
		if budget := thinking.ThinkingBudget; budget == nil || *budget != -1 {
			config.ThinkingBudget = budget
		}
		config.ReasoningEffort = thinking.ThinkingLevel
		// includeThoughts=false：照常思考，但不返回思考正文（保留多轮调用需要的签名）；未设置时保持原行为返回思考
		if thinking.IncludeThoughts != nil && !*thinking.IncludeThoughts {
			config.HideThinking = true
		}
	}
	return aistudio.GenerateRequest{
		ID: id, Model: model, System: system, Contents: contents, Config: config, Tools: tools,
	}, nil
}

func (config geminiGenerationConfig) validate() error {
	if config.FrequencyPenalty != nil && *config.FrequencyPenalty != 0 {
		return fmt.Errorf("generationConfig.frequencyPenalty must be 0")
	}
	if config.PresencePenalty != nil && *config.PresencePenalty != 0 {
		return fmt.Errorf("generationConfig.presencePenalty must be 0")
	}
	if config.CandidateCount != nil && *config.CandidateCount != 1 {
		return fmt.Errorf("generationConfig.candidateCount must be 1")
	}
	if config.ResponseLogprobs != nil && *config.ResponseLogprobs {
		return fmt.Errorf("generationConfig.responseLogprobs must be false")
	}
	if config.Logprobs != nil && *config.Logprobs != 0 {
		return fmt.Errorf("generationConfig.logprobs must be 0")
	}
	return nil
}

func mapGeminiTranscriptionConfig(input *geminiTranscriptionConfig) (*aistudio.TranscriptionConfig, error) {
	if input == nil {
		return nil, nil
	}
	config := &aistudio.TranscriptionConfig{
		SmartTranscription: input.SmartTranscription,
	}
	if input.WordTimestamps != nil {
		config.WordTimestamps = *input.WordTimestamps
	}
	if input.SpeakerLabels != nil {
		config.SpeakerLabels = *input.SpeakerLabels
	}
	for _, vocabulary := range input.CustomVocabulary {
		if vocabulary = strings.TrimSpace(vocabulary); vocabulary != "" {
			config.CustomVocabulary = append(config.CustomVocabulary, vocabulary)
		}
	}
	for _, language := range input.LanguageCodes {
		language = strings.TrimSpace(language)
		if language != "" && !strings.EqualFold(language, "detect") {
			config.LanguageCodes = append(config.LanguageCodes, language)
		}
	}
	if config.SmartTranscription {
		if input.WordTimestamps != nil && *input.WordTimestamps || input.SpeakerLabels != nil && *input.SpeakerLabels {
			return nil, fmt.Errorf("transcriptionConfig.smartTranscription cannot be combined with wordTimestamps or speakerLabels")
		}
		config.WordTimestamps = false
		config.SpeakerLabels = false
	}
	return config, nil
}

func mapGeminiResponseModalities(input []string) ([]aistudio.ResponseModality, error) {
	if input == nil {
		return nil, nil
	}
	modalities := make([]aistudio.ResponseModality, 0, len(input))
	for _, raw := range input {
		modality := aistudio.ResponseModality(strings.ToUpper(strings.TrimSpace(raw)))
		switch modality {
		case aistudio.ResponseModalityText, aistudio.ResponseModalityImage, aistudio.ResponseModalityAudio:
			modalities = append(modalities, modality)
		default:
			return nil, fmt.Errorf("unsupported response modality %q", raw)
		}
	}
	return modalities, nil
}

func mapGeminiSpeechConfig(input *geminiSpeechConfig) (*aistudio.SpeechConfig, error) {
	if input == nil {
		return nil, nil
	}
	if input.VoiceConfig != nil && input.MultiSpeakerVoiceConfig != nil {
		return nil, fmt.Errorf("speechConfig cannot contain both voiceConfig and multiSpeakerVoiceConfig")
	}
	config := &aistudio.SpeechConfig{}
	if input.VoiceConfig != nil {
		if input.VoiceConfig.PrebuiltVoiceConfig == nil || strings.TrimSpace(input.VoiceConfig.PrebuiltVoiceConfig.VoiceName) == "" {
			return nil, fmt.Errorf("speechConfig.voiceConfig requires prebuiltVoiceConfig.voiceName")
		}
		config.VoiceName = input.VoiceConfig.PrebuiltVoiceConfig.VoiceName
	}
	if input.MultiSpeakerVoiceConfig != nil {
		config.Mode = input.MultiSpeakerVoiceConfig.Mode
		for index, speaker := range input.MultiSpeakerVoiceConfig.SpeakerVoiceConfigs {
			if strings.TrimSpace(speaker.Speaker) == "" || speaker.VoiceConfig.PrebuiltVoiceConfig == nil || strings.TrimSpace(speaker.VoiceConfig.PrebuiltVoiceConfig.VoiceName) == "" {
				return nil, fmt.Errorf("speechConfig.multiSpeakerVoiceConfig.speakerVoiceConfigs[%d] requires speaker and voiceName", index)
			}
			config.Speakers = append(config.Speakers, aistudio.SpeakerVoiceConfig{
				Speaker: speaker.Speaker, VoiceName: speaker.VoiceConfig.PrebuiltVoiceConfig.VoiceName,
			})
		}
	}
	return config, nil
}

func geminiRole(role string) (aistudio.Role, error) {
	switch role {
	case "", "user":
		return aistudio.RoleUser, nil
	case "model", "assistant":
		return aistudio.RoleAssistant, nil
	case "function", "tool":
		return aistudio.RoleTool, nil
	default:
		return "", fmt.Errorf("unsupported content role %q", role)
	}
}

func mapGeminiParts(input []geminiPart) ([]aistudio.Part, bool, error) {
	parts := make([]aistudio.Part, 0, len(input))
	hasResult := false
	for index, part := range input {
		variants := 0
		if part.Text != nil {
			variants++
		}
		inline := part.inline()
		if inline != nil {
			variants++
		}
		file := part.file()
		if file != nil {
			variants++
		}
		if part.FunctionCall != nil {
			variants++
		}
		if part.FunctionResponse != nil {
			variants++
		}
		if part.ExecutableCode != nil {
			variants++
		}
		if part.CodeExecutionResult != nil {
			variants++
		}
		if variants == 0 && part.ThoughtSignature != "" {
			parts = append(parts, aistudio.Part{ThoughtSignature: part.ThoughtSignature})
			continue
		}
		if variants != 1 {
			return nil, false, fmt.Errorf("parts[%d] must contain exactly one data field", index)
		}
		switch {
		case inline != nil:
			mime := inline.MIME()
			if mime == "" || inline.Data == "" {
				return nil, false, fmt.Errorf("inlineData requires mimeType and data")
			}
			data, err := decodeBase64Flexible(inline.Data)
			if err != nil {
				return nil, false, fmt.Errorf("inlineData.data: %w", err)
			}
			mimeType, data := normalizeImagePayload(mime, data)
			parts = append(parts, aistudio.Part{
				InlineData:       &aistudio.Blob{MIME: mimeType, Data: data},
				ThoughtSignature: part.ThoughtSignature,
			})
		case file != nil:
			uri := file.URI()
			mime := file.MIME()
			if uri == "" || mime == "" {
				return nil, false, fmt.Errorf("fileData requires fileUri and mimeType")
			}
			if media, ok := aistudio.ExternalMediaForURL(uri); ok {
				parts = append(parts, aistudio.Part{ExternalMedia: media, ThoughtSignature: part.ThoughtSignature})
			} else {
				parts = append(parts, aistudio.Part{
					File: &aistudio.FileRef{
						ID: uri, Name: file.DisplayName, MIME: mime,
					},
					ThoughtSignature: part.ThoughtSignature,
				})
			}
		case part.FunctionCall != nil:
			if part.FunctionCall.Name == "" {
				return nil, false, fmt.Errorf("functionCall requires name")
			}
			arguments, err := geminiJSONObject(part.FunctionCall.Args, "functionCall.args")
			if err != nil {
				return nil, false, err
			}
			parts = append(parts, aistudio.Part{
				FunctionCall: &aistudio.FunctionCall{
					ID: part.FunctionCall.ID, Name: part.FunctionCall.Name, Arguments: arguments, ThoughtSignature: part.ThoughtSignature,
				},
				ThoughtSignature: part.ThoughtSignature,
			})
		case part.FunctionResponse != nil:
			if part.FunctionResponse.Name == "" {
				return nil, false, fmt.Errorf("functionResponse requires name")
			}
			response, err := geminiJSONObject(part.FunctionResponse.Response, "functionResponse.response")
			if err != nil {
				return nil, false, err
			}
			parts = append(parts, aistudio.Part{
				FunctionResult: &aistudio.FunctionResult{
					ID: part.FunctionResponse.ID, Name: part.FunctionResponse.Name, Content: response,
				},
				ThoughtSignature: part.ThoughtSignature,
			})
			hasResult = true
		case part.ExecutableCode != nil:
			parts = append(parts, aistudio.Part{
				ExecutableCode: &aistudio.ExecutableCode{
					Language: part.ExecutableCode.Language, Code: part.ExecutableCode.Code,
				},
				ThoughtSignature: part.ThoughtSignature,
			})
		case part.CodeExecutionResult != nil:
			parts = append(parts, aistudio.Part{
				CodeExecutionResult: &aistudio.CodeExecutionResult{
					Outcome: part.CodeExecutionResult.Outcome,
					Output:  part.CodeExecutionResult.Output,
					Error:   part.CodeExecutionResult.Error,
				},
				ThoughtSignature: part.ThoughtSignature,
			})
		default:
			if part.Thought || *part.Text == "" {
				if part.ThoughtSignature != "" {
					parts = append(parts, aistudio.Part{ThoughtSignature: part.ThoughtSignature})
				}
				continue
			}
			parts = append(parts, aistudio.Part{Text: *part.Text, ThoughtSignature: part.ThoughtSignature, SpeechMetadata: part.speechMetadata()})
		}
	}
	return parts, hasResult, nil
}

func geminiJSONObject(raw json.RawMessage, field string) (json.RawMessage, error) {
	if len(raw) == 0 {
		return json.RawMessage(`{}`), nil
	}
	var object map[string]any
	if err := json.Unmarshal(raw, &object); err != nil || object == nil {
		return nil, fmt.Errorf("%s must be an object", field)
	}
	return raw, nil
}

func mapGeminiGoogleSearch(raw json.RawMessage) (*aistudio.GoogleSearchOptions, error) {
	if !geminiRawObjectPresent(raw) {
		return nil, nil
	}
	var config struct {
		SearchTypes *struct {
			WebSearch   json.RawMessage `json:"webSearch"`
			ImageSearch json.RawMessage `json:"imageSearch"`
		} `json:"searchTypes"`
		TimeRangeFilter *struct {
			StartTime string `json:"startTime"`
			EndTime   string `json:"endTime"`
		} `json:"timeRangeFilter"`
	}
	if err := json.Unmarshal(raw, &config); err != nil {
		return nil, fmt.Errorf("googleSearch must be an object")
	}
	options := &aistudio.GoogleSearchOptions{}
	if config.SearchTypes == nil {
		options.WebSearch = true
	} else {
		options.WebSearch = geminiRawObjectPresent(config.SearchTypes.WebSearch)
		options.ImageSearch = geminiRawObjectPresent(config.SearchTypes.ImageSearch)
		if !options.WebSearch && !options.ImageSearch {
			options.WebSearch = true
		}
	}
	if config.TimeRangeFilter != nil {
		timeRange := &aistudio.GoogleSearchTimeRange{}
		if config.TimeRangeFilter.StartTime != "" {
			value, err := time.Parse(time.RFC3339Nano, config.TimeRangeFilter.StartTime)
			if err != nil {
				return nil, fmt.Errorf("googleSearch.timeRangeFilter.startTime: %w", err)
			}
			timeRange.StartTime = value
		}
		if config.TimeRangeFilter.EndTime != "" {
			value, err := time.Parse(time.RFC3339Nano, config.TimeRangeFilter.EndTime)
			if err != nil {
				return nil, fmt.Errorf("googleSearch.timeRangeFilter.endTime: %w", err)
			}
			timeRange.EndTime = value
		}
		options.TimeRange = timeRange
	}
	return options, nil
}

func geminiRawObjectPresent(raw json.RawMessage) bool {
	return len(raw) > 0 && strings.TrimSpace(string(raw)) != "null"
}

func mapGeminiTools(groups []geminiToolGroup, config geminiToolConfig) (aistudio.Tools, error) {
	var mapped aistudio.Tools
	for _, group := range groups {
		for _, declaration := range group.FunctionDeclarations {
			if declaration.Name == "" {
				return aistudio.Tools{}, fmt.Errorf("function declaration name is required")
			}
			parameters := declaration.Parameters
			// parametersJsonSchema 为 null 时不能覆盖 parameters 里的真实参数
			if geminiRawObjectPresent(declaration.ParametersJSONSchema) {
				parameters = declaration.ParametersJSONSchema
			}
			if len(parameters) == 0 {
				parameters = json.RawMessage(`{"type":"object","properties":{}}`)
			}
			mapped.Functions = append(mapped.Functions, aistudio.FunctionDeclaration{
				Name: declaration.Name, Description: declaration.Description, Parameters: parameters,
			})
		}
		search, err := mapGeminiGoogleSearch(group.GoogleSearch)
		if err != nil {
			return aistudio.Tools{}, err
		}
		if search != nil {
			if mapped.GoogleSearch == nil {
				mapped.GoogleSearch = search
			} else {
				mapped.GoogleSearch.WebSearch = mapped.GoogleSearch.WebSearch || search.WebSearch
				mapped.GoogleSearch.ImageSearch = mapped.GoogleSearch.ImageSearch || search.ImageSearch
				if search.TimeRange != nil {
					mapped.GoogleSearch.TimeRange = search.TimeRange
				}
			}
		}
		retrieval, err := geminiEmptyObjectPresent(group.GoogleSearchRetrieval, "googleSearchRetrieval")
		if err != nil {
			return aistudio.Tools{}, err
		}
		if retrieval {
			mapped.Google = appendUnique(mapped.Google, "google_search")
		}
		if geminiRawObjectPresent(group.URLContext) {
			mapped.Google = appendUnique(mapped.Google, "url_context")
		}
		if geminiRawObjectPresent(group.CodeExecution) {
			mapped.Google = appendUnique(mapped.Google, "code_execution")
		}
		if geminiRawObjectPresent(group.GoogleMaps) {
			mapped.Google = appendUnique(mapped.Google, "google_maps")
		}
		if geminiRawObjectPresent(group.ImageSearch) {
			mapped.Google = appendUnique(mapped.Google, "image_search")
		}
	}
	toolConfig := aistudio.ToolConfig{AllowedFunctionNames: config.FunctionCallingConfig.AllowedFunctionNames}
	switch strings.ToUpper(config.FunctionCallingConfig.Mode) {
	case "", "AUTO":
		toolConfig.Mode = "auto"
	case "ANY":
		toolConfig.Mode = "required"
	case "VALIDATED":
		toolConfig.Mode = "validated"
	case "NONE":
		toolConfig.Mode = "none"
	default:
		return aistudio.Tools{}, fmt.Errorf("unsupported functionCallingConfig mode %q", config.FunctionCallingConfig.Mode)
	}
	mapped.ToolConfig = toolConfig
	return mapped, nil
}

func geminiEmptyObjectPresent(raw json.RawMessage, field string) (bool, error) {
	if !geminiRawObjectPresent(raw) {
		return false, nil
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil || object == nil {
		return false, fmt.Errorf("%s must be an object", field)
	}
	if len(object) != 0 {
		return false, fmt.Errorf("%s only accepts an empty object", field)
	}
	return true, nil
}

func (s *server) handleGeminiCountTokens(w http.ResponseWriter, r *http.Request, request aistudio.GenerateRequest) {
	contents, _ := normalizeTurns(request.Contents)
	count, err := s.service.CountTokens(r.Context(), aistudio.TokenCountRequest{
		Model: s.resolveModelName(r.Context(), request.Model), System: request.System, Contents: contents,
		Tools: request.Tools,
	})
	if err != nil {
		if shouldWriteRequestError(r, err) {
			writeGeminiRequestError(w, err)
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]int64{"totalTokens": count.InputTokens})
}

func (s *server) handleGeminiGenerate(w http.ResponseWriter, r *http.Request, request aistudio.GenerateRequest, stream bool) {
	hideThought := s.prepareGenerate(r.Context(), &request)
	request.Stream = stream
	events, err := s.service.Generate(r.Context(), request)
	if err == nil && stream {
		events, err = awaitStreamStart(r.Context(), events)
	}
	if err != nil {
		if shouldWriteRequestError(r, err) {
			writeGeminiRequestError(w, err)
		}
		return
	}
	events = withHiddenReasoning(r.Context(), events, hideThought)
	if stream {
		s.streamGemini(w, r, request, events)
		return
	}
	result, err := consumeEvents(r.Context(), events, nil)
	if err != nil {
		if shouldWriteRequestError(r, err) {
			writeGeminiRequestError(w, err)
		}
		return
	}
	writeJSON(w, http.StatusOK, buildGeminiResponse(request, result))
}

func buildGeminiResponse(request aistudio.GenerateRequest, result generationResult) map[string]any {
	candidate := map[string]any{
		"content": map[string]any{"role": "model", "parts": geminiOutputParts(result)},
		"index":   0,
	}
	setGeminiFinish(candidate, result.finishReason)
	if result.grounding != nil {
		candidate["groundingMetadata"] = geminiGroundingMetadata(*result.grounding)
	} else if len(result.citations) > 0 {
		candidate["citationMetadata"] = geminiCitationMetadata(result.citations)
	}
	response := map[string]any{
		"candidates":   []any{candidate},
		"modelVersion": request.Model,
		"responseId":   request.ID,
	}
	if result.providerModel != "" {
		response["modelVersion"] = result.providerModel
	}
	if result.usage != nil {
		response["usageMetadata"] = geminiUsage(result.usage)
	}
	return response
}

// geminiOutputParts 生成非流式响应的 parts。
// 上游按流式返回，正文是许多小段增量；这里把连续的正文（以及连续的思考）合并为一个 part，
// 与 Gemini API 非流式响应一致。逐段各成一个 part 时，SillyTavern 等客户端会用换行或空行拼接多个 part，
// 导致句子中间断行、数字被拆开、出现多余空行
func geminiOutputParts(result generationResult) []map[string]any {
	parts := make([]map[string]any, 0)
	// merging 记录最后一个 part 可以继续追加的文本类型："text"、"thought" 或空（不可追加）
	merging := ""
	appendText := func(kind string, event aistudio.Event, build func() map[string]any) {
		if merging == kind && len(parts) > 0 {
			last := parts[len(parts)-1]
			_, signed := last["thoughtSignature"]
			// 已带签名的 part 再遇到新签名时另起一个，避免覆盖签名
			if text, ok := last["text"].(string); ok && !(signed && event.ThoughtSignature != "") {
				last["text"] = text + event.Text
				if event.ThoughtSignature != "" {
					last["thoughtSignature"] = event.ThoughtSignature
				}
				return
			}
		}
		parts = append(parts, geminiSignedPart(build(), event.ThoughtSignature))
		merging = kind
	}
	for _, event := range result.events {
		switch event.Kind {
		case aistudio.EventText:
			if event.Transcript != nil {
				// 带说话人元数据的转写文本保持独立
				parts = append(parts, geminiSignedPart(geminiTextPart(event), event.ThoughtSignature))
				merging = ""
				continue
			}
			appendText("text", event, func() map[string]any { return geminiTextPart(event) })
			continue
		case aistudio.EventReasoning:
			appendText("thought", event, func() map[string]any { return map[string]any{"text": event.Text, "thought": true} })
			continue
		case aistudio.EventThoughtSignature:
			if event.ThoughtSignature == "" {
				continue
			}
			// 单独到达的签名挂到前一个文本 part 上：单独成 part 时客户端拼接会多出一处空行
			if merging != "" && len(parts) > 0 {
				if _, signed := parts[len(parts)-1]["thoughtSignature"]; !signed {
					parts[len(parts)-1]["thoughtSignature"] = event.ThoughtSignature
					continue
				}
			}
			parts = append(parts, map[string]any{"thoughtSignature": event.ThoughtSignature})
			merging = ""
			continue
		}
		merging = ""
		switch event.Kind {
		case aistudio.EventToolCall:
			if event.ToolCall != nil {
				parts = append(parts, geminiSignedPart(geminiFunctionCallPart(*event.ToolCall), event.ThoughtSignature))
			}
		case aistudio.EventExecutableCode:
			if event.ExecutableCode != nil {
				parts = append(parts, geminiSignedPart(map[string]any{"executableCode": map[string]any{
					"language": event.ExecutableCode.Language, "code": event.ExecutableCode.Code,
				}}, event.ThoughtSignature))
			}
		case aistudio.EventCodeExecutionResult:
			if event.CodeExecutionResult != nil {
				parts = append(parts, geminiSignedPart(map[string]any{
					"codeExecutionResult": geminiCodeExecutionResult(*event.CodeExecutionResult),
				}, event.ThoughtSignature))
			}
		case aistudio.EventMedia:
			if event.Media != nil {
				if len(event.Media.Data) > 0 {
					parts = append(parts, geminiSignedPart(map[string]any{"inlineData": map[string]any{
						"mimeType": event.Media.MIME, "data": base64.StdEncoding.EncodeToString(event.Media.Data),
					}}, event.ThoughtSignature))
				} else if event.Media.URL != "" {
					parts = append(parts, geminiSignedPart(map[string]any{"fileData": map[string]any{
						"mimeType": event.Media.MIME, "fileUri": event.Media.URL, "displayName": event.Media.Name,
					}}, event.ThoughtSignature))
				}
			}
		}
	}
	return parts
}

func geminiTextPart(event aistudio.Event) map[string]any {
	part := map[string]any{"text": event.Text}
	if event.Transcript == nil {
		return part
	}
	metadata := map[string]any{}
	if event.Transcript.Speaker != "" {
		metadata["speaker"] = event.Transcript.Speaker
	}
	if len(event.Transcript.Timestamps) > 0 {
		timestamps := make([]map[string]any, 0, len(event.Transcript.Timestamps))
		for _, timestamp := range event.Transcript.Timestamps {
			timestamps = append(timestamps, map[string]any{
				"start": geminiTranscriptDuration(timestamp.Start),
				"end":   geminiTranscriptDuration(timestamp.End),
			})
		}
		metadata["timestamps"] = timestamps
	}
	part["transcriptionMetadata"] = metadata
	return part
}

func geminiTranscriptDuration(duration aistudio.TranscriptDuration) map[string]int64 {
	return map[string]int64{"seconds": duration.Seconds, "nanos": duration.Nanos}
}

func geminiFunctionCallPart(call aistudio.FunctionCall) map[string]any {
	part := map[string]any{"functionCall": map[string]any{
		"id": call.ID, "name": call.Name, "args": call.Arguments,
	}}
	if call.ThoughtSignature != "" {
		part["thoughtSignature"] = call.ThoughtSignature
	}
	return part
}

func geminiSignedPart(part map[string]any, signature string) map[string]any {
	if signature != "" {
		part["thoughtSignature"] = signature
	}
	return part
}

func geminiCodeExecutionResult(result aistudio.CodeExecutionResult) map[string]any {
	output := map[string]any{"outcome": result.Outcome}
	if result.Outcome == "OUTCOME_OK" {
		output["output"] = result.Output
	} else {
		output["error"] = result.Error
	}
	return output
}

func geminiCitationMetadata(citations []aistudio.Citation) map[string]any {
	sources := make([]map[string]any, 0, len(citations))
	for _, citation := range citations {
		sources = append(sources, map[string]any{
			"uri": citation.URL, "title": citation.Title, "startIndex": citation.Start, "endIndex": citation.End,
		})
	}
	return map[string]any{"citationSources": sources}
}

func geminiGroundingMetadata(metadata aistudio.GroundingMetadata) map[string]any {
	output := map[string]any{}
	if metadata.SearchEntryPoint != nil {
		entry := map[string]any{}
		if metadata.SearchEntryPoint.RenderedContent != "" {
			entry["renderedContent"] = metadata.SearchEntryPoint.RenderedContent
		}
		if metadata.SearchEntryPoint.SDKBlob != "" {
			entry["sdkBlob"] = metadata.SearchEntryPoint.SDKBlob
		}
		output["searchEntryPoint"] = entry
	}
	if len(metadata.Chunks) > 0 {
		chunks := make([]map[string]any, 0, len(metadata.Chunks))
		for _, chunk := range metadata.Chunks {
			value := map[string]any{"uri": chunk.URI, "title": chunk.Title}
			switch chunk.Source {
			case "web":
				chunks = append(chunks, map[string]any{"web": value})
			case "retrieved_context":
				value["text"] = chunk.Text
				chunks = append(chunks, map[string]any{"retrievedContext": value})
			case "maps":
				value["text"] = chunk.Text
				value["placeId"] = chunk.PlaceID
				chunks = append(chunks, map[string]any{"maps": value})
			}
		}
		output["groundingChunks"] = chunks
	}
	if len(metadata.Supports) > 0 {
		supports := make([]map[string]any, 0, len(metadata.Supports))
		for _, support := range metadata.Supports {
			value := map[string]any{
				"segment": map[string]any{
					"partIndex": support.Segment.PartIndex, "startIndex": support.Segment.StartIndex,
					"endIndex": support.Segment.EndIndex, "text": support.Segment.Text,
				},
				"groundingChunkIndices": support.ChunkIndices,
			}
			if len(support.ConfidenceScores) > 0 {
				value["confidenceScores"] = support.ConfidenceScores
			}
			supports = append(supports, value)
		}
		output["groundingSupports"] = supports
	}
	if metadata.DynamicRetrievalScore != nil {
		output["retrievalMetadata"] = map[string]any{
			"googleSearchDynamicRetrievalScore": *metadata.DynamicRetrievalScore,
		}
	}
	if len(metadata.WebSearchQueries) > 0 {
		output["webSearchQueries"] = metadata.WebSearchQueries
	}
	if metadata.MapsWidgetContextToken != "" {
		output["googleMapsWidgetContextToken"] = metadata.MapsWidgetContextToken
	}
	return output
}

func geminiFinishReason(reason string) string {
	switch strings.ToLower(strings.TrimSpace(reason)) {
	case "", "stop", "stop_sequence":
		return "STOP"
	case "unspecified":
		return "FINISH_REASON_UNSPECIFIED"
	case "max_tokens", "max_output_tokens", "length":
		return "MAX_TOKENS"
	case "safety", "content_filter", "blocked":
		return "SAFETY"
	case "recitation":
		return "RECITATION"
	case "language":
		return "LANGUAGE"
	case "other":
		return "OTHER"
	case "blocklist":
		return "BLOCKLIST"
	case "prohibited_content":
		return "PROHIBITED_CONTENT"
	case "spii":
		return "SPII"
	case "malformed_function_call":
		return "MALFORMED_FUNCTION_CALL"
	case "image_safety":
		return "IMAGE_SAFETY"
	case "unexpected_tool_call":
		return "UNEXPECTED_TOOL_CALL"
	case "too_many_tool_calls":
		return "TOO_MANY_TOOL_CALLS"
	case "image_prohibited_content":
		return "IMAGE_PROHIBITED_CONTENT"
	case "image_other":
		return "IMAGE_OTHER"
	case "no_image":
		return "NO_IMAGE"
	case "image_recitation":
		return "IMAGE_RECITATION"
	case "missing_thought_signature":
		return "OTHER"
	default:
		return "OTHER"
	}
}

func setGeminiFinish(candidate map[string]any, reason string) {
	candidate["finishReason"] = geminiFinishReason(reason)
	normalized := strings.ToLower(strings.TrimSpace(reason))
	if normalized == "missing_thought_signature" {
		candidate["finishMessage"] = "Missing thought signature"
	} else if strings.HasPrefix(normalized, "provider_") {
		candidate["finishMessage"] = "AI Studio finish reason " + strings.TrimPrefix(normalized, "provider_")
	}
}

func geminiUsage(usage *aistudio.Usage) map[string]any {
	return map[string]any{
		"promptTokenCount":        usage.InputTokens,
		"candidatesTokenCount":    usage.OutputTokens,
		"thoughtsTokenCount":      usage.ReasoningTokens,
		"toolUsePromptTokenCount": usage.ToolTokens,
		"totalTokenCount":         usage.TotalTokens,
	}
}

func (s *server) streamGemini(w http.ResponseWriter, r *http.Request, request aistudio.GenerateRequest, events <-chan aistudio.Event) {
	if err := streamHeaders(w); err != nil {
		return
	}
	result, err := consumeStreamEvents(r.Context(), events, func(event aistudio.Event) error {
		response := map[string]any{"responseId": request.ID, "modelVersion": request.Model}
		switch event.Kind {
		case aistudio.EventText:
			response["candidates"] = []any{geminiStreamCandidate(geminiSignedPart(geminiTextPart(event), event.ThoughtSignature))}
		case aistudio.EventReasoning:
			response["candidates"] = []any{geminiStreamCandidate(geminiSignedPart(map[string]any{"text": event.Text, "thought": true}, event.ThoughtSignature))}
		case aistudio.EventToolCall:
			if event.ToolCall == nil {
				return nil
			}
			response["candidates"] = []any{geminiStreamCandidate(geminiSignedPart(geminiFunctionCallPart(*event.ToolCall), event.ThoughtSignature))}
		case aistudio.EventExecutableCode:
			if event.ExecutableCode == nil {
				return nil
			}
			part := map[string]any{"executableCode": map[string]any{
				"language": event.ExecutableCode.Language, "code": event.ExecutableCode.Code,
			}}
			response["candidates"] = []any{geminiStreamCandidate(geminiSignedPart(part, event.ThoughtSignature))}
		case aistudio.EventCodeExecutionResult:
			if event.CodeExecutionResult == nil {
				return nil
			}
			part := map[string]any{
				"codeExecutionResult": geminiCodeExecutionResult(*event.CodeExecutionResult),
			}
			response["candidates"] = []any{geminiStreamCandidate(geminiSignedPart(part, event.ThoughtSignature))}
		case aistudio.EventGrounding:
			if event.Grounding == nil {
				return nil
			}
			response["candidates"] = []any{map[string]any{
				"index": 0, "groundingMetadata": geminiGroundingMetadata(*event.Grounding),
			}}
		case aistudio.EventCitation:
			if event.Citation == nil {
				return nil
			}
			response["candidates"] = []any{map[string]any{
				"index": 0, "citationMetadata": geminiCitationMetadata([]aistudio.Citation{*event.Citation}),
			}}
		case aistudio.EventMedia:
			if event.Media == nil {
				return nil
			}
			var part map[string]any
			if len(event.Media.Data) > 0 {
				part = map[string]any{"inlineData": map[string]any{
					"mimeType": event.Media.MIME, "data": base64.StdEncoding.EncodeToString(event.Media.Data),
				}}
			} else if event.Media.URL != "" {
				part = map[string]any{"fileData": map[string]any{
					"mimeType": event.Media.MIME, "fileUri": event.Media.URL, "displayName": event.Media.Name,
				}}
			} else {
				return nil
			}
			response["candidates"] = []any{geminiStreamCandidate(geminiSignedPart(part, event.ThoughtSignature))}
		case aistudio.EventThoughtSignature:
			if event.ThoughtSignature == "" {
				return nil
			}
			response["candidates"] = []any{geminiStreamCandidate(map[string]any{"thoughtSignature": event.ThoughtSignature})}
		default:
			return nil
		}
		return writeSSE(w, "", response)
	}, func() error { return writeSSEHeartbeat(w) })
	if err != nil {
		if shouldWriteRequestError(r, err) {
			_ = writeSSE(w, "", geminiStreamError(w, err))
		}
		return
	}
	model := request.Model
	if result.providerModel != "" {
		model = result.providerModel
	}
	candidate := map[string]any{"index": 0}
	setGeminiFinish(candidate, result.finishReason)
	final := map[string]any{
		"responseId": request.ID, "modelVersion": model,
		"candidates": []any{candidate},
	}
	if result.usage != nil {
		final["usageMetadata"] = geminiUsage(result.usage)
	}
	_ = writeSSE(w, "", final)
}

func geminiStreamCandidate(part map[string]any) map[string]any {
	return map[string]any{"index": 0, "content": map[string]any{"role": "model", "parts": []any{part}}}
}

// nonEmptyGeminiParts 去掉不含任何数据的分段（如 {} 或 {"text": null}）。系统指令里出现这种空分段时原先整个请求
// 返回 400 "parts[0] must contain exactly one data field"，空分段本身没有内容，忽略即可
func nonEmptyGeminiParts(parts []geminiPart) []geminiPart {
	kept := make([]geminiPart, 0, len(parts))
	for _, part := range parts {
		if part.Text == nil && part.inline() == nil && part.file() == nil && part.FunctionCall == nil &&
			part.FunctionResponse == nil && part.ExecutableCode == nil && part.CodeExecutionResult == nil &&
			part.ThoughtSignature == "" {
			continue
		}
		kept = append(kept, part)
	}
	return kept
}

// geminiSmallBodyLimit 以内的请求体先读进内存，便于在出错时列出顶层字段
const geminiSmallBodyLimit = 64 << 10

// normalizeVariants 把兼容写法合并到标准字段：countTokens 的 generateContentRequest 包装、snake_case 字段名。
// 两种写法同时出现时以 camelCase 为准
func (request *geminiRequest) normalizeVariants() error {
	// 包装写法可能嵌套（如 requests 里再包 generateContentRequest），最多解开几层
	for depth := 0; depth < 3 && len(request.Contents) == 0 && request.SystemInstruction == nil &&
		request.SystemInstructionSnake == nil; depth++ {
		switch {
		case request.GenerateContentRequest != nil:
			*request = *request.GenerateContentRequest
		case len(request.Requests) == 1:
			*request = request.Requests[0]
		case len(request.Requests) > 1:
			return fmt.Errorf("requests contains %d items; generateContent accepts one request per call", len(request.Requests))
		default:
			depth = 3
		}
	}
	request.GenerateContentRequest = nil
	request.Requests = nil
	if len(request.Contents) == 0 && request.Content != nil {
		request.Contents = []geminiContent{*request.Content}
	}
	request.Content = nil
	if request.SystemInstruction == nil && request.SystemInstructionSnake != nil {
		request.SystemInstruction = request.SystemInstructionSnake
	}
	if geminiRawObjectPresent(request.GenerationConfigSnake) && reflect.ValueOf(request.GenerationConfig).IsZero() {
		if err := decodeSnakeJSON(request.GenerationConfigSnake, &request.GenerationConfig); err != nil {
			return fmt.Errorf("generation_config: %w", err)
		}
	}
	if geminiRawObjectPresent(request.ToolConfigSnake) && reflect.ValueOf(request.ToolConfig).IsZero() {
		if err := decodeSnakeJSON(request.ToolConfigSnake, &request.ToolConfig); err != nil {
			return fmt.Errorf("tool_config: %w", err)
		}
	}
	return nil
}

// decodeSnakeJSON 把对象里的 snake_case 键名转成 camelCase 后解码；只用于 generation_config 这类小对象。
// 结构化输出的 schema（response_schema 等）里是用户自己定义的属性名，原样保留
func decodeSnakeJSON(raw json.RawMessage, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return err
	}
	converted, err := json.Marshal(camelizeKeys(value))
	if err != nil {
		return err
	}
	return json.Unmarshal(converted, target)
}

func camelizeKeys(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		result := make(map[string]any, len(typed))
		for key, item := range typed {
			name := snakeToCamel(key)
			if name == "responseSchema" || name == "responseJsonSchema" {
				result[name] = item
				continue
			}
			result[name] = camelizeKeys(item)
		}
		return result
	case []any:
		for index, item := range typed {
			typed[index] = camelizeKeys(item)
		}
		return typed
	}
	return value
}

func snakeToCamel(key string) string {
	if !strings.Contains(key, "_") {
		return key
	}
	parts := strings.Split(key, "_")
	for index := 1; index < len(parts); index++ {
		if parts[index] != "" {
			parts[index] = strings.ToUpper(parts[index][:1]) + parts[index][1:]
		}
	}
	return strings.Join(parts, "")
}

// geminiTopLevelKeys 列出请求体的顶层字段名（不含内容）
func geminiTopLevelKeys(body []byte) string {
	if len(body) == 0 {
		return "请求体较大，未列出"
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(body, &fields) != nil {
		return "不是 JSON 对象"
	}
	if len(fields) == 0 {
		return "空对象"
	}
	keys := make([]string, 0, len(fields))
	for key := range fields {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return strings.Join(keys, ", ")
}
