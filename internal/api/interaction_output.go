package api

import (
	"encoding/base64"
	"fmt"
	"mime"
	"net/http"
	"strconv"
	"strings"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// interactionMedia 投影音频容器、采样参数及其他媒体内容
func interactionMedia(media aistudio.Media, audioFormat string) (map[string]any, error) {
	baseType, parameters, err := mime.ParseMediaType(media.MIME)
	if err != nil {
		return nil, err
	}
	typeName := strings.SplitN(baseType, "/", 2)[0]
	if typeName != "audio" && typeName != "video" && typeName != "image" {
		typeName = "document"
	}
	content := map[string]any{"type": typeName, "mime_type": baseType}
	if media.URL != "" {
		content["uri"] = media.URL
		return content, nil
	}
	data := media.Data
	if typeName == "audio" {
		data, _, err = encodeSpeechResponse(media, audioFormat)
		if err != nil {
			return nil, err
		}
		if audioFormat == "wav" {
			content["mime_type"] = "audio/wav"
		}
		if rate, parseErr := strconv.Atoi(parameters["rate"]); parseErr == nil {
			content["sample_rate"] = rate
		}
		channels := 1
		if value, parseErr := strconv.Atoi(parameters["channels"]); parseErr == nil {
			channels = value
		}
		content["channels"] = channels
	}
	content["data"] = base64.StdEncoding.EncodeToString(data)
	return content, nil
}

// interactionStep 将单个规范事件投影为步骤内容与对应增量（流式按顺序逐个发送）
func interactionStep(event aistudio.Event, audioFormat string) (map[string]any, []map[string]any, error) {
	var content map[string]any
	switch event.Kind {
	case aistudio.EventText:
		content = map[string]any{"type": "text", "text": event.Text}
	case aistudio.EventReasoning:
		// 带签名的思考在流式中另发 thought_signature 增量，原先只发摘要，签名在流式响应里丢失
		step := map[string]any{"type": "thought"}
		var deltas []map[string]any
		if event.Text != "" {
			content = map[string]any{"type": "text", "text": event.Text}
			step["summary"] = []map[string]any{content}
			deltas = append(deltas, map[string]any{"type": "thought_summary", "content": content})
		}
		if event.ThoughtSignature != "" {
			step["signature"] = event.ThoughtSignature
			deltas = append(deltas, map[string]any{"type": "thought_signature", "signature": event.ThoughtSignature})
		}
		if len(deltas) == 0 {
			return nil, nil, nil
		}
		return step, deltas, nil
	case aistudio.EventThoughtSignature:
		if event.ThoughtSignature == "" {
			return nil, nil, nil
		}
		return map[string]any{"type": "thought", "signature": event.ThoughtSignature},
			[]map[string]any{{"type": "thought_signature", "signature": event.ThoughtSignature}}, nil
	case aistudio.EventToolCall:
		if event.ToolCall == nil {
			return nil, nil, nil
		}
		call := event.ToolCall
		return map[string]any{"type": "function_call", "id": call.ID, "name": call.Name, "arguments": call.Arguments}, nil, nil
	case aistudio.EventMedia:
		if event.Media == nil {
			return nil, nil, nil
		}
		var err error
		content, err = interactionMedia(*event.Media, audioFormat)
		if err != nil {
			return nil, nil, err
		}
	case aistudio.EventExecutableCode, aistudio.EventCodeExecutionResult:
		if text := renderCodeExecution(event); text != "" {
			content = map[string]any{"type": "text", "text": text}
		}
	}
	if content == nil {
		return nil, nil, nil
	}
	return map[string]any{"type": "model_output", "content": []map[string]any{content}}, []map[string]any{content}, nil
}

// interactionStepEvents 把一个规范事件展开为按输出顺序排列的步骤事件：函数调用携带的签名在调用之前作为 thought
// 步骤输出（客户端回传时挂回调用）；不返回思考摘要时思考只保留签名
func interactionStepEvents(event aistudio.Event, summaries bool) []aistudio.Event {
	if event.Kind == aistudio.EventReasoning && !summaries {
		if event.ThoughtSignature == "" {
			return nil
		}
		event = aistudio.Event{Kind: aistudio.EventThoughtSignature, ThoughtSignature: event.ThoughtSignature}
	}
	if signature := interactionCallSignature(event); signature != "" {
		return []aistudio.Event{{Kind: aistudio.EventThoughtSignature, ThoughtSignature: signature}, event}
	}
	return []aistudio.Event{event}
}

// signedThoughtStep 判断步骤是否为带签名的 thought
func signedThoughtStep(step map[string]any) bool {
	_, signed := step["signature"]
	return step["type"] == "thought" && signed
}

// signatureOnlyStep 判断步骤是否为只带签名、没有思考摘要的 thought。
// 正文进行中到达的这类步骤暂存到正文结束后再输出：gemini-4 等模型在正文分片之间夹带只有签名的分片，
// 立即输出会把一段正文切成许多个 model_output 步骤，按步骤分段显示的客户端会在句子中间断行
func signatureOnlyStep(step map[string]any) bool {
	_, summary := step["summary"]
	return signedThoughtStep(step) && !summary
}

// signatureStep 生成只带签名的 thought 步骤及其增量
func signatureStep(signature string) (map[string]any, []map[string]any) {
	return map[string]any{"type": "thought", "signature": signature},
		[]map[string]any{{"type": "thought_signature", "signature": signature}}
}

// interactionSteps 汇总完整内容并将所有 PCM 分块封装为一段音频。
// 相邻的同类步骤合并；两个都带签名的 thought 步骤不合并，避免后一个签名覆盖前一个；
// 正文中途只带签名的 thought 步骤排到这段正文之后（见 signatureOnlyStep）
func interactionSteps(result generationResult, audioFormat string, summaries bool) ([]map[string]any, error) {
	steps := make([]map[string]any, 0)
	audioWritten := false
	var pending deferredSignatures
	appendStep := func(step map[string]any) {
		if len(steps) == 0 {
			steps = append(steps, step)
			return
		}
		previous := steps[len(steps)-1]
		mergeable := step["type"] == previous["type"] && (step["type"] == "model_output" || step["type"] == "thought")
		if !mergeable || signedThoughtStep(step) && signedThoughtStep(previous) {
			steps = append(steps, step)
			return
		}
		field := "content"
		if step["type"] == "thought" {
			field = "summary"
		}
		items, _ := step[field].([]map[string]any)
		prior, _ := previous[field].([]map[string]any)
		for _, item := range items {
			if len(prior) > 0 && item["type"] == "text" && prior[len(prior)-1]["type"] == "text" {
				prior[len(prior)-1]["text"] = prior[len(prior)-1]["text"].(string) + item["text"].(string)
			} else {
				prior = append(prior, item)
			}
		}
		if len(prior) > 0 {
			previous[field] = prior
		}
		if signature, ok := step["signature"]; ok {
			previous["signature"] = signature
		}
	}
	flush := func() {
		for _, signature := range pending.take() {
			step, _ := signatureStep(signature)
			appendStep(step)
		}
	}
	for _, source := range result.events {
		for _, event := range interactionStepEvents(source, summaries) {
			if event.Kind == aistudio.EventMedia && event.Media != nil && strings.HasPrefix(event.Media.MIME, "audio/") {
				if audioWritten {
					continue
				}
				audio, err := joinedAudio(result.media)
				if err != nil {
					return nil, err
				}
				event.Media = &audio
				audioWritten = true
			}
			step, _, err := interactionStep(event, audioFormat)
			if err != nil {
				return nil, err
			}
			if step == nil {
				continue
			}
			if signatureOnlyStep(step) && len(steps) > 0 && steps[len(steps)-1]["type"] == "model_output" {
				pending.add(step["signature"].(string))
				continue
			}
			if step["type"] != "model_output" {
				flush()
			}
			appendStep(step)
		}
	}
	flush()
	return steps, nil
}

// streamInteraction 输出创建、步骤增量、步骤结束与交互终态
func (s *server) streamInteraction(w http.ResponseWriter, r *http.Request, request interactionRequest, generate aistudio.GenerateRequest, model string, current []aistudio.Content, created string, events <-chan aistudio.Event) {
	if err := streamHeaders(w); err != nil {
		return
	}
	send := func(kind string, value map[string]any) error {
		value["event_type"] = kind
		return writeSSE(w, kind, value)
	}
	if err := send("interaction.created", map[string]any{"interaction": interactionObject(generate.ID, model, created, "in_progress", nil)}); err != nil {
		return
	}
	audioFormat := request.audioFormat()
	summaries := request.Generation.ThinkingSummaries != "none"
	index := -1
	active := ""
	activeSigned := false
	closeStep := func() error {
		if active == "" {
			return nil
		}
		active = ""
		return send("step.stop", map[string]any{"index": index})
	}
	// write 输出一个步骤的增量，步骤类型变化时先结束上一步、开始新一步
	write := func(step map[string]any, deltas []map[string]any) error {
		kind := step["type"].(string)
		signed := signedThoughtStep(step)
		// 函数调用各自成步；已带签名的 thought 再遇到签名时另起一步，客户端按步骤保存签名，不会被覆盖
		if active != kind || kind == "function_call" || signed && activeSigned {
			if err := closeStep(); err != nil {
				return err
			}
			index++
			active = kind
			activeSigned = false
			start := map[string]any{"type": kind}
			switch kind {
			case "model_output":
				start["content"] = []any{}
			case "thought":
				start["summary"] = []any{}
			case "function_call":
				start = step
			}
			if err := send("step.start", map[string]any{"index": index, "step": start}); err != nil {
				return err
			}
		}
		activeSigned = activeSigned || signed
		for _, delta := range deltas {
			if err := send("step.delta", map[string]any{"index": index, "delta": delta}); err != nil {
				return err
			}
		}
		return nil
	}
	// pending 暂存 model_output 进行中到达的只带签名的 thought 步骤，正文步骤结束后再输出（见 signatureOnlyStep）
	var pending deferredSignatures
	flush := func() error {
		for _, signature := range pending.take() {
			if err := write(signatureStep(signature)); err != nil {
				return err
			}
		}
		return nil
	}
	emit := func(source aistudio.Event) error {
		for _, event := range interactionStepEvents(source, summaries) {
			step, deltas, err := interactionStep(event, audioFormat)
			if err != nil {
				return err
			}
			if step == nil {
				continue
			}
			if active == "model_output" && signatureOnlyStep(step) {
				pending.add(step["signature"].(string))
				continue
			}
			if step["type"] != "model_output" {
				if err := flush(); err != nil {
					return err
				}
			}
			if err := write(step, deltas); err != nil {
				return err
			}
		}
		return nil
	}
	bufferWAV := audioFormat == "wav"
	result, err := consumeStreamEvents(r.Context(), events, func(event aistudio.Event) error {
		if bufferWAV && event.Kind == aistudio.EventMedia && event.Media != nil && strings.HasPrefix(event.Media.MIME, "audio/") {
			return nil
		}
		return emit(event)
	}, func() error { return writeSSEHeartbeat(w) })
	if err == nil && bufferWAV {
		for _, media := range result.media {
			if !strings.HasPrefix(media.MIME, "audio/") {
				continue
			}
			var audio aistudio.Media
			audio, err = joinedAudio(result.media)
			if err == nil {
				err = emit(aistudio.Event{Kind: aistudio.EventMedia, Media: &audio})
			}
			break
		}
	}
	if err == nil {
		err = validateInteractionResult(generate, result)
	}
	if err != nil {
		SetAccessLogError(r.Context(), err)
		if shouldWriteRequestError(r, err) {
			// 与其他协议一样按官方措辞脱敏，内部原因（中文说明、账户邮箱）只写管理日志
			_ = send("error", interactionStreamError(w, err))
		}
		return
	}
	if err := flush(); err != nil {
		return
	}
	if err := closeStep(); err != nil {
		return
	}
	if request.Store == nil || *request.Store {
		s.storeInteractionState(generate.ID, request.PreviousID, current, result)
	}
	if err := send("interaction.completed", map[string]any{"interaction": interactionObject(generate.ID, model, created, interactionStatus(result), result.usage)}); err != nil {
		SetAccessLogError(r.Context(), fmt.Errorf("interaction completion: %w", err))
	}
}

// interactionCallSignature 提取函数调用携带的可回传思考签名
func interactionCallSignature(event aistudio.Event) string {
	if event.Kind != aistudio.EventToolCall || event.ToolCall == nil {
		return ""
	}
	if event.ToolCall.ThoughtSignature != "" {
		return event.ToolCall.ThoughtSignature
	}
	return event.ThoughtSignature
}
