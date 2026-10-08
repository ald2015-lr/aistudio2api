package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// handleInteraction 将公开 Interactions 请求接入规范生成链路：模型后缀与思考强度、降级判定的流式标记、
// 流式首事件前的错误返回 HTTP 状态，与其他四个生成入口一致；错误一律按 Gemini 官方格式脱敏返回
func (s *server) handleInteraction(w http.ResponseWriter, r *http.Request) {
	var request interactionRequest
	if err := decodeJSON(r, &request); err != nil {
		writeGeminiInvalid(w, err)
		return
	}
	generate, err := request.toGenerateRequest(newID("int"))
	if err != nil {
		writeGeminiInvalid(w, err)
		return
	}
	contents := generate.Contents
	start := 0
	if id := request.PreviousID; id != "" {
		// Responses 与 Interactions 共用 responseStates：resp_ 开头的是 Responses 的响应 ID，
		// 它的系统指令另存在 Responses 的状态里，接到这里会丢失，明确拒绝而不是按找不到处理
		if strings.HasPrefix(id, "resp_") {
			writeGeminiError(w, http.StatusBadRequest, "INVALID_ARGUMENT",
				"previous_interaction_id must be an interaction ID (int_...); resp_... IDs belong to the Responses API, continue them with previous_response_id on /v1/responses")
			return
		}
		previous, _, ok := s.responseStates.Load(id)
		if !ok || !strings.HasPrefix(id, "int_") {
			writeGeminiError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "previous_interaction_id was not found")
			return
		}
		start = len(previous)
		contents = append(previous, contents...)
	}
	// 既没有输入、续接历史也没有系统指令时在选号前返回 400；只有系统指令时按用户消息生成，与 Gemini 入口一致
	if len(contents) == 0 && strings.TrimSpace(generate.System) == "" {
		writeGeminiError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "input is required")
		return
	}
	if err := resolveInteractionResults(contents); err != nil {
		writeGeminiInvalid(w, err)
		return
	}
	current := cloneResponseContents(contents[start:])
	generate.Contents = contents
	model := generate.Model
	hideThought := s.prepareGenerate(r.Context(), &generate)
	generate.Stream = request.Stream
	events, err := s.service.Generate(r.Context(), generate)
	if err == nil && request.Stream {
		events, err = awaitStreamStart(r.Context(), events)
	}
	if err != nil {
		if shouldWriteRequestError(r, err) {
			writeGeminiRequestError(w, err)
		}
		return
	}
	events = withHiddenReasoning(r.Context(), withLocalCallIDs(r.Context(), events), hideThought)
	created := time.Now().UTC().Format(time.RFC3339)
	if request.Stream {
		s.streamInteraction(w, r, request, generate, model, current, created, events)
		return
	}
	result, err := consumeEvents(r.Context(), events, nil)
	if err == nil {
		err = validateInteractionResult(generate, result)
	}
	var steps []map[string]any
	if err == nil {
		steps, err = interactionSteps(result, request.audioFormat(), request.Generation.ThinkingSummaries != "none")
	}
	if err != nil {
		if shouldWriteRequestError(r, err) {
			writeGeminiRequestError(w, err)
		}
		return
	}
	if request.Store == nil || *request.Store {
		s.storeInteractionState(generate.ID, request.PreviousID, current, result)
	}
	response := interactionObject(generate.ID, model, created, interactionStatus(result), result.usage)
	response["steps"] = steps
	writeJSON(w, http.StatusOK, response)
}

// withLocalCallIDs 为没有 ID 的函数调用补本地 ID（与 aistudio 事件层同一规则，已带 ID 的原样通过）：
// function_result 只能按 call_id 对应回调用，空 id 的调用无法续接
func withLocalCallIDs(ctx context.Context, events <-chan aistudio.Event) <-chan aistudio.Event {
	out := make(chan aistudio.Event, 16)
	go func() {
		defer close(out)
		for event := range events {
			select {
			case out <- aistudio.AssignLocalCallID(event):
			case <-ctx.Done():
				// 客户端已断开：继续读完上游，让生产方正常结束
				for range events {
				}
				return
			}
		}
	}()
	return out
}

// storeInteractionState 保存续接用的上下文。音频输出（TTS、音乐）不放进共用的 responseStates：
// 几十秒的 PCM 就有数 MB，256 个节点会常驻大量内存，下一轮也不会把音频当作模型输入；
// 音频分片带的签名改为纯签名 Part 保留
func (s *server) storeInteractionState(id string, parentID string, current []aistudio.Content, result generationResult) {
	events := make([]aistudio.Event, 0, len(result.events))
	for _, event := range result.events {
		if event.Kind == aistudio.EventMedia && event.Media != nil && strings.HasPrefix(event.Media.MIME, "audio/") {
			if event.ThoughtSignature == "" {
				continue
			}
			event = aistudio.Event{Kind: aistudio.EventThoughtSignature, ThoughtSignature: event.ThoughtSignature}
		}
		events = append(events, event)
	}
	result.events = events
	s.storeResponseState(id, parentID, current, nil, result)
}

// validateInteractionResult 确认请求音频时已收到可用音频内容
func validateInteractionResult(request aistudio.GenerateRequest, result generationResult) error {
	for _, modality := range request.Config.ResponseModalities {
		if modality == aistudio.ResponseModalityAudio {
			_, err := joinedAudio(result.media)
			return err
		}
	}
	return nil
}

// resolveInteractionResults 从完整调用历史补齐函数结果名称，并拒绝对不上任何调用的结果。
// 补名称时复制结果再替换：续接历史与已保存的状态共用同一个结果对象，不能原地修改
func resolveInteractionResults(contents []aistudio.Content) error {
	calls := make(map[string]string)
	for _, content := range contents {
		for index, part := range content.Parts {
			if part.FunctionCall != nil {
				calls[part.FunctionCall.ID] = part.FunctionCall.Name
			}
			result := part.FunctionResult
			if result == nil {
				continue
			}
			name := calls[result.ID]
			if name == "" || result.Name != "" && result.Name != name {
				return fmt.Errorf("function result %q must match a preceding function call", result.ID)
			}
			if result.Name == "" {
				resolved := *result
				resolved.Name = name
				content.Parts[index].FunctionResult = &resolved
			}
		}
	}
	return nil
}

// interactionObject 构造 SDK 使用的资源标识、状态与用量；model 为客户端请求的模型名（含后缀）
func interactionObject(id string, model string, created string, status string, usage *aistudio.Usage) map[string]any {
	object := map[string]any{
		"id": id, "object": "interaction", "model": model,
		"created": created, "updated": time.Now().UTC().Format(time.RFC3339), "status": status,
	}
	if usage != nil {
		object["usage"] = map[string]any{
			"total_input_tokens": usage.InputTokens, "total_output_tokens": usage.OutputTokens,
			"total_thought_tokens": usage.ReasoningTokens, "total_tool_use_tokens": usage.ToolTokens,
			"total_tokens": usage.TotalTokens,
		}
	}
	return object
}

// interactionStatus 将生成终态转换为交互资源状态：只有输出上限与内容拦截算提前终止，
// 停止序列、malformed_function_call 等其他原因与其他协议一样按正常结束
func interactionStatus(result generationResult) string {
	switch openAIFinishReason(result.finishReason, false) {
	case "length", "content_filter":
		return "incomplete"
	}
	if len(result.toolCalls) > 0 {
		return "requires_action"
	}
	return "completed"
}
