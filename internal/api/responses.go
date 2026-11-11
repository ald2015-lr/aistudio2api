package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

type responsesRequest struct {
	Model              string            `json:"model"`
	Input              json.RawMessage   `json:"input"`
	Instructions       string            `json:"instructions"`
	Stream             bool              `json:"stream"`
	Tools              []responsesTool   `json:"tools"`
	ToolChoice         json.RawMessage   `json:"tool_choice"`
	Temperature        *float64          `json:"temperature"`
	TopP               *float64          `json:"top_p"`
	MaxOutputTokens    *int64            `json:"max_output_tokens"`
	Reasoning          json.RawMessage   `json:"reasoning"`
	Text               json.RawMessage   `json:"text"`
	PreviousResponseID string            `json:"previous_response_id"`
	ParallelToolCalls  *bool             `json:"parallel_tool_calls"`
	Truncation         string            `json:"truncation"`
	Metadata           map[string]string `json:"metadata"`
	Store              *bool             `json:"store"`
	// VideoConfig 为 Omni 视频设置扩展字段，与 OpenAI Chat 的顶层 video_config 相同，见 omni_video.go
	VideoConfig *videoConfigRequest `json:"video_config"`
}

type responsesTool struct {
	Type              string          `json:"type"`
	Name              string          `json:"name,omitempty"`
	Description       string          `json:"description,omitempty"`
	Parameters        json.RawMessage `json:"parameters,omitempty"`
	SearchContextSize string          `json:"search_context_size,omitempty"`
	UserLocation      json.RawMessage `json:"user_location,omitempty"`
	Filters           json.RawMessage `json:"filters,omitempty"`
	Container         json.RawMessage `json:"container,omitempty"`
	Strict            *bool           `json:"strict,omitempty"`
	Tools             []responsesTool `json:"tools,omitempty"`
}

type responsesInputItem struct {
	Type             string          `json:"type"`
	Role             string          `json:"role"`
	Content          json.RawMessage `json:"content"`
	CallID           string          `json:"call_id"`
	Name             string          `json:"name"`
	Namespace        string          `json:"namespace"`
	Arguments        string          `json:"arguments"`
	Output           json.RawMessage `json:"output"`
	EncryptedContent string          `json:"encrypted_content"`
}

type responseState struct {
	ParentID           string
	Contents           []aistudio.Content
	InlineInstructions []string
}

const responseStateCapacity = 256

type responseStateStore struct {
	mu     sync.Mutex
	states map[string]responseState
	order  []string
	// persist 非空时把节点的变化写进状态库（见 response_state_persist.go），服务重启后读回；为 nil 时只在内存里保存
	persist ResponseStatePersister
	// unpersisted 为不写进状态库的节点：超过单个节点的上限或编码失败的，以及续接起点在其中的（见 markUnpersisted）。
	// 只在 persist 非空时使用，节点淘汰时去掉
	unpersisted map[string]bool
}

func newResponseStateStore() *responseStateStore {
	return &responseStateStore{states: make(map[string]responseState, responseStateCapacity)}
}

func (store *responseStateStore) Load(id string) ([]aistudio.Content, []string, bool) {
	store.mu.Lock()
	defer store.mu.Unlock()
	chain := make([]responseState, 0)
	for id != "" {
		state, exists := store.states[id]
		if !exists {
			return nil, nil, false
		}
		chain = append(chain, state)
		id = state.ParentID
	}
	contents := make([]aistudio.Content, 0)
	instructions := make([]string, 0)
	for index := len(chain) - 1; index >= 0; index-- {
		contents = append(contents, cloneResponseContents(chain[index].Contents)...)
		instructions = append(instructions, chain[index].InlineInstructions...)
	}
	return contents, instructions, true
}

func (store *responseStateStore) Store(id string, state responseState) {
	store.mu.Lock()
	defer store.mu.Unlock()
	state.Contents = cloneResponseContents(state.Contents)
	dropSourceGIF(state.Contents)
	state.InlineInstructions = append([]string(nil), state.InlineInstructions...)
	store.states[id] = state
	store.order = append(store.order, id)
	if store.persist != nil {
		// 续接起点不入库时它也不入库：重启后接不上，写了也用不上（见 markUnpersisted）
		if store.unpersisted[state.ParentID] {
			store.unpersisted[id] = true
		} else {
			delete(store.unpersisted, id)
		}
	}
	touched, removed := []string{id}, []string(nil)
	for len(store.order) > responseStateCapacity {
		evicted, children := store.evictOldest()
		removed = append(removed, evicted)
		touched = append(touched, children...)
	}
	store.persistLocked(touched, removed)
}

// evictOldest 淘汰最早保存的节点，把它的上下文并入以它为续接起点的子节点；返回淘汰的节点与改动过的子节点
func (store *responseStateStore) evictOldest() (string, []string) {
	id := store.order[0]
	store.order = store.order[1:]
	parent := store.states[id]
	var children []string
	for childID, child := range store.states {
		if child.ParentID != id {
			continue
		}
		child.ParentID = parent.ParentID
		child.Contents = append(cloneResponseContents(parent.Contents), child.Contents...)
		child.InlineInstructions = append(append([]string(nil), parent.InlineInstructions...), child.InlineInstructions...)
		store.states[childID] = child
		children = append(children, childID)
	}
	delete(store.states, id)
	delete(store.unpersisted, id)
	return id, children
}

// dropSourceGIF 去掉协议层把 GIF 转成 PNG 时留下的原 GIF（Blob.SourceGIF，只用于本次请求计 Interaction 模型的转换额度）：
// 续接状态最多 256 个、只按个数淘汰，动图原文件往往远大于首帧 PNG，留着会让每个状态多常驻一份完整动图（所有模型都一样）；
// 续接历史里的这张 PNG 早已转好，按普通 PNG 内联，不再计入转换额度、也不换回 GIF。contents 须是已复制的 Part 切片：
// 复制 Blob 再替换，本次请求持有的 Blob 不变
func dropSourceGIF(contents []aistudio.Content) {
	for _, content := range contents {
		for index, part := range content.Parts {
			if part.InlineData == nil || part.InlineData.SourceGIF == nil {
				continue
			}
			blob := *part.InlineData
			blob.SourceGIF = nil
			content.Parts[index].InlineData = &blob
		}
	}
}

func cloneResponseContents(contents []aistudio.Content) []aistudio.Content {
	cloned := append([]aistudio.Content(nil), contents...)
	for index := range cloned {
		cloned[index].Parts = append([]aistudio.Part(nil), contents[index].Parts...)
	}
	return cloned
}

func (s *server) handleResponses(w http.ResponseWriter, r *http.Request) {
	var request responsesRequest
	if err := decodeJSON(r, &request); err != nil {
		writeOpenAIInvalid(w, err)
		return
	}
	if request.Model == "" || len(request.Input) == 0 {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "model and input are required")
		return
	}
	responseID := newID("resp")
	generateRequest, inlineInstructions, err := request.toGenerateRequest(responseID)
	if err != nil {
		writeOpenAIInvalid(w, err)
		return
	}
	currentContents := cloneResponseContents(generateRequest.Contents)
	currentInlineInstructions := append([]string(nil), inlineInstructions...)
	if request.PreviousResponseID != "" {
		previousContents, previousInstructions, ok := s.responseStates.Load(request.PreviousResponseID)
		if !ok {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request", fmt.Sprintf("previous response %q was not found", request.PreviousResponseID))
			return
		}
		generateRequest.Contents = append(previousContents, generateRequest.Contents...)
		inlineInstructions = append(previousInstructions, inlineInstructions...)
		instructions := make([]string, 0, 1+len(inlineInstructions))
		if request.Instructions != "" {
			instructions = append(instructions, request.Instructions)
		}
		instructions = append(instructions, inlineInstructions...)
		generateRequest.System = strings.Join(instructions, "\n")
	}
	// 整理客户端的系统提示（含续接带上的系统指令）：删除计费行、按 OMNI_IGNORE_SYSTEM_PROMPT 去掉发给 Omni 视频模型的系统提示
	// （见 omni_system.go）；续接用的状态已在上面存下原样的系统指令，每次续接都重新整理
	s.prepareClientSystem(r.Context(), &generateRequest)
	hideThought, err := s.prepareGenerate(r.Context(), &generateRequest)
	if err != nil {
		writeOpenAIInvalid(w, err)
		return
	}
	// 响应里的 model 报告去掉后缀的目录模型名（见 responseModel）
	request.Model = responseModel(generateRequest)
	generateRequest.Stream = request.Stream
	events, err := s.generate(r.Context(), generateRequest)
	if err != nil {
		if shouldWriteRequestError(r, err) {
			writeOpenAIRequestError(w, err)
		}
		return
	}
	events = withHiddenReasoning(r.Context(), events, hideThought)
	created := time.Now().Unix()
	if request.Stream {
		// 额度用完改用了其他模型时报告实际使用的模型（见 servedModel）
		request.Model = servedModel(r.Context(), request.Model)
		s.streamResponses(w, r, request, currentContents, currentInlineInstructions, responseID, created, events)
		return
	}
	result, err := consumeEvents(r.Context(), events, nil)
	if err != nil {
		if shouldWriteRequestError(r, err) {
			writeOpenAIRequestError(w, err)
		}
		return
	}
	request.Model = servedModel(r.Context(), request.Model)
	response, err := buildResponsesObject(responseID, created, request, result)
	if err != nil {
		writeOpenAIRequestError(w, err)
		return
	}
	if request.Store == nil || *request.Store {
		s.storeResponseState(responseID, request.PreviousResponseID, currentContents, currentInlineInstructions, result)
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *server) storeResponseState(id string, parentID string, contents []aistudio.Content, inlineInstructions []string, result generationResult) {
	if output := responseHistoryOutput(result); len(output.Parts) > 0 {
		contents = append(contents, output)
	}
	s.responseStates.Store(id, responseState{
		ParentID:           parentID,
		Contents:           contents,
		InlineInstructions: append([]string(nil), inlineInstructions...),
	})
}

func responseHistoryOutput(result generationResult) aistudio.Content {
	output := aistudio.Content{Role: aistudio.RoleAssistant}
	for _, event := range result.events {
		switch event.Kind {
		case aistudio.EventText:
			if event.Text != "" {
				output.Parts = append(output.Parts, aistudio.Part{Text: event.Text, ThoughtSignature: event.ThoughtSignature})
			}
		case aistudio.EventToolCall:
			if event.ToolCall != nil {
				call := *event.ToolCall
				call.Arguments = append(json.RawMessage(nil), call.Arguments...)
				output.Parts = append(output.Parts, aistudio.Part{FunctionCall: &call, ThoughtSignature: event.ThoughtSignature})
			}
		case aistudio.EventMedia:
			if event.Media != nil && len(event.Media.Data) > 0 {
				blob := &aistudio.Blob{MIME: event.Media.MIME, Data: append([]byte(nil), event.Media.Data...)}
				output.Parts = append(output.Parts, aistudio.Part{InlineData: blob, ThoughtSignature: event.ThoughtSignature})
			}
		case aistudio.EventReasoning, aistudio.EventThoughtSignature:
			if event.ThoughtSignature != "" {
				output.Parts = append(output.Parts, aistudio.Part{ThoughtSignature: event.ThoughtSignature})
			}
		}
	}
	return output
}

func (request responsesRequest) toGenerateRequest(id string) (aistudio.GenerateRequest, []string, error) {
	switch request.Truncation {
	case "", "disabled", "auto":
	default:
		return aistudio.GenerateRequest{}, nil, fmt.Errorf("unsupported truncation %q", request.Truncation)
	}
	contents, inlineInstructions, err := responsesContents(request.Input)
	if err != nil {
		return aistudio.GenerateRequest{}, nil, err
	}
	instructions := make([]string, 0, 1+len(inlineInstructions))
	if request.Instructions != "" {
		instructions = append(instructions, request.Instructions)
	}
	instructions = append(instructions, inlineInstructions...)
	tools, err := mapResponsesTools(request.Tools, request.ToolChoice)
	if err != nil {
		return aistudio.GenerateRequest{}, nil, err
	}
	tools.ToolConfig.ParallelCalls = request.ParallelToolCalls
	config := aistudio.GenerationConfig{
		Temperature:     request.Temperature,
		TopP:            request.TopP,
		MaxOutputTokens: request.MaxOutputTokens,
		VideoConfig:     request.VideoConfig.videoConfig(),
	}
	if len(request.Reasoning) > 0 && string(request.Reasoning) != "null" {
		var reasoning struct {
			Effort string `json:"effort"`
		}
		if err := json.Unmarshal(request.Reasoning, &reasoning); err != nil {
			return aistudio.GenerateRequest{}, nil, fmt.Errorf("invalid reasoning: %w", err)
		}
		config.ReasoningEffort = reasoning.Effort
	}
	if len(request.Text) > 0 && string(request.Text) != "null" {
		var text struct {
			Format struct {
				Type   string          `json:"type"`
				Schema json.RawMessage `json:"schema"`
			} `json:"format"`
		}
		if err := json.Unmarshal(request.Text, &text); err != nil {
			return aistudio.GenerateRequest{}, nil, fmt.Errorf("invalid text config: %w", err)
		}
		switch text.Format.Type {
		case "json_object":
			config.ResponseMIMEType = "application/json"
		case "json_schema":
			config.ResponseMIMEType = "application/json"
			if geminiRawObjectPresent(text.Format.Schema) {
				config.ResponseSchema = text.Format.Schema
			}
		case "", "text":
		default:
			return aistudio.GenerateRequest{}, nil, fmt.Errorf("unsupported text format %q", text.Format.Type)
		}
	}
	return aistudio.GenerateRequest{
		ID:       id,
		Model:    request.Model,
		System:   strings.Join(instructions, "\n"),
		Contents: contents,
		Config:   config,
		Tools:    tools,
		Truncate: request.Truncation == "auto",
	}, inlineInstructions, nil
}

func responsesContents(raw json.RawMessage) ([]aistudio.Content, []string, error) {
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return []aistudio.Content{{Role: aistudio.RoleUser, Parts: []aistudio.Part{{Text: text}}}}, nil, nil
	}
	var items []responsesInputItem
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, nil, fmt.Errorf("input must be a string or item array")
	}
	contents := make([]aistudio.Content, 0, len(items))
	var instructions []string
	pendingSignature := ""
	for _, item := range items {
		switch item.Type {
		case "", "message":
			pendingSignature = ""
			if item.Role == "system" || item.Role == "developer" {
				text, err := openAITextContent(item.Content)
				if err != nil {
					return nil, nil, fmt.Errorf("%s message: %w", item.Role, err)
				}
				if text != "" {
					instructions = append(instructions, text)
				}
				continue
			}
			role, err := openAIRole(item.Role)
			if err != nil {
				return nil, nil, err
			}
			parts, err := openAIContentParts(item.Content)
			if err != nil {
				return nil, nil, err
			}
			contents = append(contents, aistudio.Content{Role: role, Parts: parts})
		case "agent_message":
			// Codex 多智能体会话里智能体之间的消息（与上游 Mag1cFall/AIStudio2API 42fa603 相同）：按 user 轮发送。原先按不支持的类型返回 400，
			// 这类会话一出现这种历史就整个请求失败
			pendingSignature = ""
			parts, err := agentMessageParts(item.Content)
			if err != nil {
				return nil, nil, fmt.Errorf("agent_message: %w", err)
			}
			contents = append(contents, aistudio.Content{Role: aistudio.RoleUser, Parts: parts})
		case "function_call":
			// 命名空间内的函数以 "命名空间.函数名" 的全名发给上游，不同命名空间可以有同名函数
			if item.Namespace != "" {
				item.Name = item.Namespace + "." + item.Name
			}
			// 参数不是 JSON object 时包成 _raw，见 historyFunctionArguments
			contents = append(contents, aistudio.Content{Role: aistudio.RoleAssistant, Parts: []aistudio.Part{{FunctionCall: &aistudio.FunctionCall{
				ID: item.CallID, Name: item.Name, Arguments: historyFunctionArguments(item.Arguments), ThoughtSignature: pendingSignature,
			}}}})
			pendingSignature = ""
		case "function_call_output":
			pendingSignature = ""
			cleaned, media, err := splitFunctionResultMedia(item.Output)
			if err != nil {
				return nil, nil, fmt.Errorf("function_call_output: %w", err)
			}
			output, err := normalizeFunctionResultContent(cleaned)
			if err != nil {
				return nil, nil, fmt.Errorf("function_call_output: %w", err)
			}
			parts := []aistudio.Part{{FunctionResult: &aistudio.FunctionResult{
				ID: item.CallID, Content: output,
			}}}
			contents = append(contents, aistudio.Content{Role: aistudio.RoleTool, Parts: append(parts, media...)})
		case "reasoning":
			pendingSignature = item.EncryptedContent
		case "web_search_call", "code_interpreter_call", "image_generation_call", "item_reference":
			// 本服务自己输出的内置工具调用项（以及引用之前输出项的 item_reference）：无状态客户端会把上一轮的
			// output 原样放回 input，原先返回 400。这些调用在上游执行，结果已体现在随后的助手消息里，跳过即可
		default:
			return nil, nil, fmt.Errorf("unsupported input item type %q", item.Type)
		}
	}
	return contents, instructions, nil
}

// agentMessageParts 把 Codex 智能体间消息（agent_message）的 content 转为文本片段：input_text 取 text，encrypted_content 取
// encrypted_content（里面是本服务的模型写出的消息原文，与上游 Mag1cFall/AIStudio2API 的 agentMessageParts 相同），其他类型返回错误
// （说明为英文，原样返回客户端）。空文本与普通 message 一样省略（见 openAIContentParts）：空的文本 Part 编码时按“必须且只能设置一种内容”
// 拒绝，整个请求会以 400 结束（上游 42fa603 同样没有省略）；全部为空时这一轮没有 Part，发送时跳过
func agentMessageParts(raw json.RawMessage) ([]aistudio.Part, error) {
	var content []struct {
		Type             string `json:"type"`
		Text             string `json:"text"`
		EncryptedContent string `json:"encrypted_content"`
	}
	if err := json.Unmarshal(raw, &content); err != nil {
		return nil, fmt.Errorf("content must be an array of content parts")
	}
	parts := make([]aistudio.Part, 0, len(content))
	for _, part := range content {
		switch part.Type {
		case "input_text":
			if part.Text != "" {
				parts = append(parts, aistudio.Part{Text: part.Text})
			}
		case "encrypted_content":
			if part.EncryptedContent != "" {
				parts = append(parts, aistudio.Part{Text: part.EncryptedContent})
			}
		default:
			return nil, fmt.Errorf("unsupported content type %q", part.Type)
		}
	}
	return parts, nil
}

func mapResponsesTools(tools []responsesTool, choice json.RawMessage) (aistudio.Tools, error) {
	var mapped aistudio.Tools
	names := make(map[string]bool)
	addFunction := func(tool responsesTool) error {
		if tool.Name == "" {
			return fmt.Errorf("function tool name is required")
		}
		if names[tool.Name] {
			return fmt.Errorf("function tool name %q is duplicated", tool.Name)
		}
		parameters := tool.Parameters
		if len(parameters) == 0 {
			parameters = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		names[tool.Name] = true
		mapped.Functions = append(mapped.Functions, aistudio.FunctionDeclaration{
			Name: tool.Name, Description: tool.Description, Parameters: parameters, Strict: tool.Strict != nil && *tool.Strict,
		})
		return nil
	}
	for _, tool := range tools {
		switch tool.Type {
		case "function":
			if err := addFunction(tool); err != nil {
				return aistudio.Tools{}, err
			}
		case "namespace":
			if tool.Name == "" {
				return aistudio.Tools{}, fmt.Errorf("namespace name is required")
			}
			for _, inner := range tool.Tools {
				if inner.Type != "function" {
					return aistudio.Tools{}, fmt.Errorf("namespace %q tool type %q is not supported", tool.Name, inner.Type)
				}
				if inner.Name == "" {
					return aistudio.Tools{}, fmt.Errorf("function tool name is required")
				}
				inner.Name = tool.Name + "." + inner.Name
				if err := addFunction(inner); err != nil {
					return aistudio.Tools{}, err
				}
			}
		case "web_search", "web_search_2025_08_26", "web_search_preview", "web_search_preview_2025_03_11":
			search, err := mapSearchOptions(tool.SearchContextSize, tool.UserLocation, tool.Filters)
			if err != nil {
				return aistudio.Tools{}, err
			}
			mapped.GoogleSearch = search
			mapped.Google = appendUnique(mapped.Google, "google_search")
		case "code_interpreter":
			if err := validateResponsesCodeContainer(tool.Container); err != nil {
				return aistudio.Tools{}, err
			}
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

func buildResponsesObject(id string, created int64, request responsesRequest, result generationResult) (map[string]any, error) {
	output := make([]any, 0, 2+len(result.toolCalls)+len(result.media))
	if result.reasoning.Len() > 0 {
		output = append(output, map[string]any{
			"id":     "rs_" + id,
			"type":   "reasoning",
			"status": "completed",
			"summary": []any{map[string]any{
				"type": "summary_text",
				"text": result.reasoning.String(),
			}},
		})
	}
	output = append(output, responseCodeInterpreterItems(id, result.events)...)
	if responsesUsesWebSearch(request.Tools) {
		output = append(output, responseWebSearchItems(id, result.events)...)
	}
	if result.text.Len() > 0 {
		output = append(output, map[string]any{
			"id":     "msg_" + id,
			"type":   "message",
			"status": "completed",
			"role":   "assistant",
			"content": []any{map[string]any{
				"type":        "output_text",
				"text":        result.text.String(),
				"annotations": responsesCitations(result.citations),
			}},
		})
	}
	for _, call := range result.toolCalls {
		if call.ThoughtSignature != "" {
			output = append(output, responseReasoningSignature(call))
		}
		output = append(output, responseFunctionCall(call, request.Tools))
	}
	for index, media := range result.media {
		item, err := responseImageGenerationItem(id, index, media)
		if err != nil {
			return nil, err
		}
		output = append(output, item)
	}
	status := "completed"
	incompleteReason := ""
	switch openAIFinishReason(result.finishReason, false) {
	case "length":
		status = "incomplete"
		incompleteReason = "max_output_tokens"
	case "content_filter":
		status = "incomplete"
		incompleteReason = "content_filter"
	}
	response := responseShell(id, created, status, request)
	response["completed_at"] = time.Now().Unix()
	if status == "incomplete" {
		response["incomplete_details"] = map[string]any{"reason": incompleteReason}
	}
	response["output"] = output
	response["output_text"] = result.text.String()
	if result.providerModel != "" {
		response["provider_model"] = result.providerModel
	}
	if providerReason := providerFinishReason(result.finishReason); providerReason != "" {
		response["provider_finish_reason"] = providerReason
	}
	if result.usage != nil {
		response["usage"] = responsesUsage(result.usage)
	}
	return response, nil
}

func responseShell(id string, created int64, status string, request responsesRequest) map[string]any {
	parallelToolCalls := true
	if request.ParallelToolCalls != nil {
		parallelToolCalls = *request.ParallelToolCalls
	}
	truncation := request.Truncation
	if truncation == "" {
		truncation = "disabled"
	}
	var instructions any
	if request.Instructions != "" {
		instructions = request.Instructions
	}
	text := rawJSONValue(request.Text, map[string]any{"format": map[string]string{"type": "text"}})
	reasoning := rawJSONValue(request.Reasoning, nil)
	toolChoice := rawJSONValue(request.ToolChoice, "auto")
	tools := request.Tools
	if tools == nil {
		tools = []responsesTool{}
	}
	var previousResponseID any
	if request.PreviousResponseID != "" {
		previousResponseID = request.PreviousResponseID
	}
	return map[string]any{
		"id":                   id,
		"object":               "response",
		"created_at":           created,
		"completed_at":         nil,
		"status":               status,
		"error":                nil,
		"incomplete_details":   nil,
		"instructions":         instructions,
		"metadata":             request.Metadata,
		"model":                request.Model,
		"output":               []any{},
		"output_text":          "",
		"parallel_tool_calls":  parallelToolCalls,
		"previous_response_id": previousResponseID,
		"reasoning":            reasoning,
		"temperature":          request.Temperature,
		"text":                 text,
		"tool_choice":          toolChoice,
		"tools":                tools,
		"top_p":                request.TopP,
		"truncation":           truncation,
		"max_output_tokens":    request.MaxOutputTokens,
		"usage":                nil,
	}
}

func rawJSONValue(raw json.RawMessage, defaultValue any) any {
	if len(raw) == 0 || string(raw) == "null" {
		return defaultValue
	}
	var value any
	_ = json.Unmarshal(raw, &value)
	return value
}

func rawJSONConfigured(raw json.RawMessage) bool {
	value := strings.TrimSpace(string(raw))
	return value != "" && value != "null"
}

func responseFunctionCall(call aistudio.FunctionCall, tools []responsesTool) map[string]any {
	item := map[string]any{
		"id":        "fc_" + call.ID,
		"type":      "function_call",
		"status":    "completed",
		"call_id":   call.ID,
		"name":      call.Name,
		"arguments": string(call.Arguments),
	}
	if namespace := responsesNamespace(tools, call.Name); namespace != "" {
		item["namespace"] = namespace
		item["name"] = strings.TrimPrefix(call.Name, namespace+".")
	}
	return item
}

// responsesNamespace 返回函数所属的命名空间工具名
func responsesNamespace(tools []responsesTool, name string) string {
	for _, tool := range tools {
		if tool.Type != "namespace" {
			continue
		}
		for _, inner := range tool.Tools {
			if tool.Name+"."+inner.Name == name {
				return tool.Name
			}
		}
	}
	return ""
}

func responseReasoningSignature(call aistudio.FunctionCall) map[string]any {
	return map[string]any{
		"id": "rs_" + call.ID, "type": "reasoning", "status": "completed",
		"summary": []any{}, "encrypted_content": call.ThoughtSignature,
	}
}

func responseCodeInterpreterItems(responseID string, events []aistudio.Event) []any {
	type codeCall struct {
		code   string
		result *aistudio.CodeExecutionResult
	}
	calls := make([]codeCall, 0)
	for _, event := range events {
		switch event.Kind {
		case aistudio.EventExecutableCode:
			if event.ExecutableCode != nil {
				calls = append(calls, codeCall{code: event.ExecutableCode.Code})
			}
		case aistudio.EventCodeExecutionResult:
			if event.CodeExecutionResult != nil && len(calls) > 0 {
				result := *event.CodeExecutionResult
				calls[len(calls)-1].result = &result
			}
		}
	}
	items := make([]any, 0, len(calls))
	for index, call := range calls {
		status := "incomplete"
		var outputs any
		if call.result != nil {
			status = "completed"
			logs := call.result.Output
			if call.result.Outcome != "OUTCOME_OK" {
				status = "failed"
				logs = call.result.Error
				if logs != "" {
					logs = "stderr:\n" + logs
				}
			}
			if logs != "" {
				outputs = []any{map[string]any{"type": "logs", "logs": logs}}
			}
		}
		items = append(items, map[string]any{
			"id": fmt.Sprintf("ci_%s_%d", responseID, index), "type": "code_interpreter_call",
			"status": status, "code": call.code, "container_id": "aistudio", "outputs": outputs,
		})
	}
	return items
}

func responseImageGenerationItem(responseID string, index int, media aistudio.Media) (map[string]any, error) {
	if !strings.HasPrefix(media.MIME, "image/") || len(media.Data) == 0 {
		return nil, fmt.Errorf("Responses API cannot represent media %q without inline image data", media.MIME)
	}
	return map[string]any{
		"id":     fmt.Sprintf("ig_%s_%d", responseID, index),
		"type":   "image_generation_call",
		"status": "completed",
		"result": base64.StdEncoding.EncodeToString(media.Data),
	}, nil
}

func responsesCitations(citations []aistudio.Citation) []map[string]any {
	output := make([]map[string]any, 0, len(citations))
	for _, citation := range citations {
		output = append(output, map[string]any{
			"type":        "url_citation",
			"start_index": citation.Start,
			"end_index":   citation.End,
			"title":       citation.Title,
			"url":         citation.URL,
		})
	}
	return output
}

func responsesUsage(usage *aistudio.Usage) map[string]any {
	return map[string]any{
		"input_tokens":  inputTokens(usage),
		"output_tokens": outputTokens(usage),
		"total_tokens":  usage.TotalTokens,
		"input_tokens_details": map[string]any{
			"cached_tokens": 0,
		},
		"output_tokens_details": map[string]any{
			"reasoning_tokens": usage.ReasoningTokens,
		},
	}
}

type responsesStreamWriter struct {
	w             http.ResponseWriter
	sequence      int
	id            string
	created       int64
	request       responsesRequest
	indexes       map[string]int
	reasoningOpen bool
	textOpen      bool
	mediaCount    int
	codeCount     int
	searchCount   int
	searchQueries map[string]struct{}
	searchProbe   bool
	pendingText   []string
	pendingBytes  int
	pendingSince  time.Time
	pendingCode   *responsesPendingCode
	// lastData 为上一次写出事件（data）的时间，SSE 注释心跳不算；静默满 responsesProgressInterval 时改发 response.in_progress（见 keepProgress）
	lastData time.Time
}

// 声明了 web_search 时先暂存正文，让搜索调用项排在消息之前（与官方输出顺序一致）；
// 但 Gemini 的搜索来源通常在最后才到，模型不搜索时更是一直没有，原先整段回复要等到结束才一次发出。
// 暂存最多 responsesSearchHoldTime 或 responsesSearchHoldBytes，之后照常流式输出，迟到的搜索项排在消息之后
const (
	responsesSearchHoldTime  = time.Second
	responsesSearchHoldBytes = 300
)

// responsesProgressAfter 为 Responses 流连续这么久没有写出任何事件（data）时，改发一次 response.in_progress 代替 SSE 注释心跳。
// Codex 读 SSE 时空闲 5 分钟就断开，计时只看事件，": ping" 注释不算；new-api 等只转发 data 行的中转还会丢掉注释，中转后面的客户端
// 那一段连接一直零字节。强制调用扣住、重新生成、long- 续写换轮、隐藏思考（只有签名与用量、不写出事件）期间都可能长时间没有事件。
// 取 30 秒：低于常见的 60 秒读超时（nginx proxy_read_timeout 的默认值），离 Codex 的 5 分钟也有充足余量；平时事件不断的流不受影响，
// 静默时仍先按心跳间隔（10 秒）写注释，静默满 30 秒那一次心跳改发 response.in_progress，之后每满 30 秒再发一次
const responsesProgressAfter = 30 * time.Second

// responsesProgressAfterOverride 非 0 时代替 responsesProgressAfter（纳秒），只供测试缩短
var responsesProgressAfterOverride atomic.Int64

// responsesProgressInterval 返回 Responses 流静默多久后改发 response.in_progress
func responsesProgressInterval() time.Duration {
	if override := responsesProgressAfterOverride.Load(); override > 0 {
		return time.Duration(override)
	}
	return responsesProgressAfter
}

type responsesPendingCode struct {
	id    string
	index int
	code  string
}

func (s *server) streamResponses(w http.ResponseWriter, r *http.Request, request responsesRequest, contents []aistudio.Content, inlineInstructions []string, id string, created int64, events <-chan aistudio.Event) {
	if err := streamHeaders(w); err != nil {
		return
	}
	writer := &responsesStreamWriter{
		w: w, id: id, created: created, request: request,
		indexes: make(map[string]int), searchProbe: responsesUsesWebSearch(request.Tools),
	}
	if err := writer.emit("response.created", map[string]any{"response": responseShell(id, created, "in_progress", request)}); err != nil {
		return
	}
	if err := writer.emit("response.in_progress", map[string]any{"response": responseShell(id, created, "in_progress", request)}); err != nil {
		return
	}
	// 心跳平时为 SSE 注释（不受 STREAM_DATA_HEARTBEAT 影响：Responses 的事件都有官方语义并带递增的 sequence_number，没有官方客户端会
	// 忽略的空数据事件）。连续 responsesProgressAfter（30 秒）没有写出任何事件时改发一次 response.in_progress（带当前的响应对象，
	// sequence_number 接着递增）：Codex 只按事件计空闲、5 分钟就断开，只转发 data 行的中转会丢掉注释。官方 SDK 与 Codex 对重复的
	// response.in_progress 都只当作状态提示（Codex 落在未处理事件分支、直接忽略），不推进输出项。上游事件只有签名、用量这类不写出事件的
	// 内容时心跳计时会被重置，所以每处理完一个事件也按同一阈值检查一次。
	// 提交响应头之后才决定改用其他模型（额度用完）时，之后的输出项与 response.completed 按实际使用的模型（见 servedModel）；
	// 已写出的 response.created、response.in_progress 保留请求的模型
	result, err := consumeStreamEvents(r.Context(), events, func(event aistudio.Event) error {
		writer.request.Model = servedModel(r.Context(), writer.request.Model)
		if err := writer.live(event); err != nil {
			return err
		}
		_, err := writer.keepProgress(r.Context())
		return err
	}, func() error {
		if sent, err := writer.keepProgress(r.Context()); sent || err != nil {
			return err
		}
		return writeSSEHeartbeat(w)
	})
	request.Model = servedModel(r.Context(), request.Model)
	writer.request.Model = request.Model
	if err != nil {
		if shouldWriteRequestError(r, err) {
			_ = writer.flushPendingText()
			_ = writer.failed(err)
		}
		return
	}
	response, err := buildResponsesObject(id, created, request, result)
	if err != nil {
		_ = writer.failed(err)
		return
	}
	if request.Store == nil || *request.Store {
		s.storeResponseState(id, request.PreviousResponseID, contents, inlineInstructions, result)
	}
	if err := writer.finish(result, response); err != nil {
		_ = writer.failed(err)
	}
}

func (writer *responsesStreamWriter) emit(eventType string, payload map[string]any) error {
	payload["type"] = eventType
	payload["sequence_number"] = writer.sequence
	writer.sequence++
	writer.lastData = time.Now()
	return writeSSE(writer.w, eventType, payload)
}

// keepProgress 距上一次写出事件已满 responsesProgressInterval 时重发一次 response.in_progress（当前的响应对象、状态 in_progress，
// model 为此刻实际使用的模型），返回是否写出；不满时什么也不写
func (writer *responsesStreamWriter) keepProgress(ctx context.Context) (bool, error) {
	if time.Since(writer.lastData) < responsesProgressInterval() {
		return false, nil
	}
	writer.request.Model = servedModel(ctx, writer.request.Model)
	return true, writer.emit("response.in_progress", map[string]any{
		"response": responseShell(writer.id, writer.created, "in_progress", writer.request),
	})
}

func (writer *responsesStreamWriter) live(event aistudio.Event) error {
	switch event.Kind {
	case aistudio.EventReasoning:
		index, err := writer.ensureReasoning()
		if err != nil {
			return err
		}
		return writer.emit("response.reasoning_summary_text.delta", map[string]any{
			"item_id": "rs_" + writer.id, "output_index": index, "summary_index": 0, "delta": event.Text,
		})
	case aistudio.EventText:
		if writer.searchProbe {
			if len(writer.pendingText) == 0 {
				writer.pendingSince = time.Now()
			}
			writer.pendingText = append(writer.pendingText, event.Text)
			writer.pendingBytes += len(event.Text)
			if writer.pendingBytes < responsesSearchHoldBytes && time.Since(writer.pendingSince) < responsesSearchHoldTime {
				return nil
			}
			return writer.flushPendingText()
		}
		return writer.emitText(event.Text)
	case aistudio.EventToolCall:
		if event.ToolCall != nil {
			return writer.emitToolCall(*event.ToolCall)
		}
	case aistudio.EventMedia:
		if event.Media != nil {
			return writer.emitMedia(*event.Media)
		}
	case aistudio.EventExecutableCode:
		if event.ExecutableCode != nil {
			return writer.emitExecutableCode(*event.ExecutableCode)
		}
	case aistudio.EventCodeExecutionResult:
		if event.CodeExecutionResult != nil {
			return writer.emitCodeExecutionResult(*event.CodeExecutionResult)
		}
	case aistudio.EventGrounding:
		if event.Grounding != nil {
			emitted, err := writer.emitGrounding(*event.Grounding)
			if err != nil {
				return err
			}
			if writer.searchProbe && emitted {
				if err := writer.flushPendingText(); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (writer *responsesStreamWriter) emitText(text string) error {
	index, err := writer.ensureText()
	if err != nil {
		return err
	}
	return writer.emit("response.output_text.delta", map[string]any{
		"item_id": "msg_" + writer.id, "output_index": index, "content_index": 0, "delta": text, "logprobs": []any{},
	})
}

func (writer *responsesStreamWriter) flushPendingText() error {
	writer.searchProbe = false
	for _, text := range writer.pendingText {
		if err := writer.emitText(text); err != nil {
			return err
		}
	}
	writer.pendingText = nil
	writer.pendingBytes = 0
	return nil
}

func (writer *responsesStreamWriter) ensureReasoning() (int, error) {
	id := "rs_" + writer.id
	if index, ok := writer.indexes[id]; ok {
		return index, nil
	}
	index := len(writer.indexes)
	writer.indexes[id] = index
	item := map[string]any{"id": id, "type": "reasoning", "status": "in_progress", "summary": []any{}}
	if err := writer.emit("response.output_item.added", map[string]any{"output_index": index, "item": item}); err != nil {
		return 0, err
	}
	if err := writer.emit("response.reasoning_summary_part.added", map[string]any{
		"item_id": id, "output_index": index, "summary_index": 0,
		"part": map[string]any{"type": "summary_text", "text": ""},
	}); err != nil {
		return 0, err
	}
	writer.reasoningOpen = true
	return index, nil
}

func (writer *responsesStreamWriter) ensureText() (int, error) {
	id := "msg_" + writer.id
	if index, ok := writer.indexes[id]; ok {
		return index, nil
	}
	index := len(writer.indexes)
	writer.indexes[id] = index
	item := map[string]any{"id": id, "type": "message", "status": "in_progress", "role": "assistant", "content": []any{}}
	if err := writer.emit("response.output_item.added", map[string]any{"output_index": index, "item": item}); err != nil {
		return 0, err
	}
	if err := writer.emit("response.content_part.added", map[string]any{
		"item_id": id, "output_index": index, "content_index": 0,
		"part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}},
	}); err != nil {
		return 0, err
	}
	writer.textOpen = true
	return index, nil
}

func (writer *responsesStreamWriter) emitToolCall(call aistudio.FunctionCall) error {
	if call.ThoughtSignature != "" {
		id := "rs_" + call.ID
		index := len(writer.indexes)
		writer.indexes[id] = index
		item := map[string]any{"id": id, "type": "reasoning", "status": "in_progress", "summary": []any{}}
		if err := writer.emit("response.output_item.added", map[string]any{"output_index": index, "item": item}); err != nil {
			return err
		}
		if err := writer.emit("response.output_item.done", map[string]any{
			"output_index": index, "item": responseReasoningSignature(call),
		}); err != nil {
			return err
		}
	}
	id := "fc_" + call.ID
	index := len(writer.indexes)
	writer.indexes[id] = index
	item := map[string]any{
		"id": id, "type": "function_call", "status": "in_progress",
		"call_id": call.ID, "name": call.Name, "arguments": "",
	}
	if namespace := responsesNamespace(writer.request.Tools, call.Name); namespace != "" {
		item["namespace"] = namespace
		item["name"] = strings.TrimPrefix(call.Name, namespace+".")
	}
	if err := writer.emit("response.output_item.added", map[string]any{"output_index": index, "item": item}); err != nil {
		return err
	}
	arguments := string(call.Arguments)
	if err := writer.emit("response.function_call_arguments.delta", map[string]any{
		"item_id": id, "output_index": index, "delta": arguments,
	}); err != nil {
		return err
	}
	if err := writer.emit("response.function_call_arguments.done", map[string]any{
		"item_id": id, "output_index": index, "arguments": arguments, "name": item["name"],
	}); err != nil {
		return err
	}
	return writer.emit("response.output_item.done", map[string]any{"output_index": index, "item": responseFunctionCall(call, writer.request.Tools)})
}

func (writer *responsesStreamWriter) emitMedia(media aistudio.Media) error {
	completed, err := responseImageGenerationItem(writer.id, writer.mediaCount, media)
	if err != nil {
		return err
	}
	writer.mediaCount++
	id := completed["id"].(string)
	index := len(writer.indexes)
	writer.indexes[id] = index
	inProgress := map[string]any{"id": id, "type": "image_generation_call", "status": "in_progress", "result": nil}
	if err := writer.emit("response.output_item.added", map[string]any{"output_index": index, "item": inProgress}); err != nil {
		return err
	}
	if err := writer.emit("response.image_generation_call.in_progress", map[string]any{"item_id": id, "output_index": index}); err != nil {
		return err
	}
	if err := writer.emit("response.image_generation_call.completed", map[string]any{"item_id": id, "output_index": index}); err != nil {
		return err
	}
	return writer.emit("response.output_item.done", map[string]any{"output_index": index, "item": completed})
}

func (writer *responsesStreamWriter) emitExecutableCode(code aistudio.ExecutableCode) error {
	id := fmt.Sprintf("ci_%s_%d", writer.id, writer.codeCount)
	writer.codeCount++
	index := len(writer.indexes)
	writer.indexes[id] = index
	writer.pendingCode = &responsesPendingCode{id: id, index: index, code: code.Code}
	item := map[string]any{
		"id": id, "type": "code_interpreter_call", "status": "in_progress",
		"code": "", "container_id": "aistudio", "outputs": nil,
	}
	if err := writer.emit("response.output_item.added", map[string]any{"output_index": index, "item": item}); err != nil {
		return err
	}
	if err := writer.emit("response.code_interpreter_call.in_progress", map[string]any{"item_id": id, "output_index": index}); err != nil {
		return err
	}
	if code.Code != "" {
		if err := writer.emit("response.code_interpreter_call_code.delta", map[string]any{"item_id": id, "output_index": index, "delta": code.Code}); err != nil {
			return err
		}
		if err := writer.emit("response.code_interpreter_call_code.done", map[string]any{"item_id": id, "output_index": index, "code": code.Code}); err != nil {
			return err
		}
	}
	return writer.emit("response.code_interpreter_call.interpreting", map[string]any{"item_id": id, "output_index": index})
}

func (writer *responsesStreamWriter) emitCodeExecutionResult(result aistudio.CodeExecutionResult) error {
	if writer.pendingCode == nil {
		return fmt.Errorf("code execution result arrived before executable code")
	}
	call := writer.pendingCode
	status := "completed"
	logs := result.Output
	if result.Outcome != "OUTCOME_OK" {
		status = "failed"
		logs = result.Error
		if logs != "" {
			logs = "stderr:\n" + logs
		}
	}
	var outputs any
	if logs != "" {
		outputs = []any{map[string]any{"type": "logs", "logs": logs}}
	}
	item := map[string]any{
		"id": call.id, "type": "code_interpreter_call", "status": status,
		"code": call.code, "container_id": "aistudio", "outputs": outputs,
	}
	if err := writer.emit("response.code_interpreter_call.completed", map[string]any{"item_id": call.id, "output_index": call.index}); err != nil {
		return err
	}
	if err := writer.emit("response.output_item.done", map[string]any{"output_index": call.index, "item": item}); err != nil {
		return err
	}
	writer.pendingCode = nil
	return nil
}

func (writer *responsesStreamWriter) finish(result generationResult, response map[string]any) error {
	if writer.reasoningOpen {
		id := "rs_" + writer.id
		index := writer.indexes[id]
		part := map[string]any{"type": "summary_text", "text": result.reasoning.String()}
		if err := writer.emit("response.reasoning_summary_text.done", map[string]any{
			"item_id": id, "output_index": index, "summary_index": 0, "text": result.reasoning.String(),
		}); err != nil {
			return err
		}
		if err := writer.emit("response.reasoning_summary_part.done", map[string]any{
			"item_id": id, "output_index": index, "summary_index": 0, "part": part,
		}); err != nil {
			return err
		}
		item := map[string]any{"id": id, "type": "reasoning", "status": "completed", "summary": []any{part}}
		if err := writer.emit("response.output_item.done", map[string]any{"output_index": index, "item": item}); err != nil {
			return err
		}
	}
	if err := writer.flushPendingText(); err != nil {
		return err
	}
	if writer.textOpen {
		id := "msg_" + writer.id
		index := writer.indexes[id]
		for annotationIndex, annotation := range responsesCitations(result.citations) {
			if err := writer.emit("response.output_text.annotation.added", map[string]any{
				"item_id": id, "output_index": index, "content_index": 0,
				"annotation_index": annotationIndex, "annotation": annotation,
			}); err != nil {
				return err
			}
		}
		part := map[string]any{"type": "output_text", "text": result.text.String(), "annotations": responsesCitations(result.citations)}
		if err := writer.emit("response.output_text.done", map[string]any{
			"item_id": id, "output_index": index, "content_index": 0, "text": result.text.String(), "logprobs": []any{},
		}); err != nil {
			return err
		}
		if err := writer.emit("response.content_part.done", map[string]any{
			"item_id": id, "output_index": index, "content_index": 0, "part": part,
		}); err != nil {
			return err
		}
		item := map[string]any{"id": id, "type": "message", "status": "completed", "role": "assistant", "content": []any{part}}
		if err := writer.emit("response.output_item.done", map[string]any{"output_index": index, "item": item}); err != nil {
			return err
		}
	}
	orderResponsesOutput(response, writer.indexes)
	eventType := "response.completed"
	if response["status"] == "incomplete" {
		eventType = "response.incomplete"
	}
	return writer.emit(eventType, map[string]any{"response": response})
}

func orderResponsesOutput(response map[string]any, indexes map[string]int) {
	output, ok := response["output"].([]any)
	if !ok {
		return
	}
	sort.SliceStable(output, func(left int, right int) bool {
		leftIndex, leftExists := responseOutputIndex(output[left], indexes)
		rightIndex, rightExists := responseOutputIndex(output[right], indexes)
		if !leftExists {
			return false
		}
		if !rightExists {
			return true
		}
		return leftIndex < rightIndex
	})
	response["output"] = output
}

func responseOutputIndex(item any, indexes map[string]int) (int, bool) {
	object, ok := item.(map[string]any)
	if !ok {
		return 0, false
	}
	id, ok := object["id"].(string)
	if !ok {
		return 0, false
	}
	index, exists := indexes[id]
	return index, exists
}

func (writer *responsesStreamWriter) failed(err error) error {
	response := responseShell(writer.id, writer.created, "failed", writer.request)
	response["error"] = responsesErrorObject(writer.w, err)
	return writer.emit("response.failed", map[string]any{"response": response})
}
