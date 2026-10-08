package api

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// interactionEvent 为解析出的一条 Interactions SSE 事件
type interactionEvent struct {
	name string
	data map[string]any
}

// interactionSSE 解析 Interactions 流式响应，并确认每条事件 JSON 里的 event_type 与事件名一致
func interactionSSE(t *testing.T, body string) []interactionEvent {
	t.Helper()
	var events []interactionEvent
	for _, block := range strings.Split(body, "\n\n") {
		var event interactionEvent
		for _, line := range strings.Split(block, "\n") {
			switch {
			case strings.HasPrefix(line, "event: "):
				event.name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event.data); err != nil {
					t.Fatalf("事件数据不是 JSON: %q", line)
				}
			}
		}
		if event.name == "" {
			continue
		}
		if event.data["event_type"] != event.name {
			t.Fatalf("事件 %s 的 event_type 为 %v", event.name, event.data["event_type"])
		}
		events = append(events, event)
	}
	return events
}

// interactionEventNames 返回事件名序列；step.start 与 step.delta 附上步骤或增量的类型，便于比较顺序
func interactionEventNames(events []interactionEvent) []string {
	names := make([]string, 0, len(events))
	for _, event := range events {
		name := event.name
		switch event.name {
		case "step.start":
			name += ":" + event.data["step"].(map[string]any)["type"].(string)
		case "step.delta":
			name += ":" + event.data["delta"].(map[string]any)["type"].(string)
		}
		names = append(names, name)
	}
	return names
}

// decodeInteraction 确认响应为 200 并解析交互资源
func decodeInteraction(t *testing.T, recorder *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是 JSON: %s", recorder.Body.String())
	}
	return body
}

// interactionResponseSteps 返回非流式交互资源中的 steps
func interactionResponseSteps(t *testing.T, body map[string]any) []map[string]any {
	t.Helper()
	raw, _ := body["steps"].([]any)
	steps := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		steps = append(steps, item.(map[string]any))
	}
	return steps
}

// TestInteractionStreamEventOrder 流式按 interaction.created → 各步骤 start/delta/stop → interaction.completed 输出，
// 思考签名在流式中作为 thought_signature 增量发出
func TestInteractionStreamEventOrder(t *testing.T) {
	service := &scriptedService{events: []aistudio.Event{
		{Kind: aistudio.EventReasoning, Text: "think", ThoughtSignature: "sig-think"},
		{Kind: aistudio.EventText, Text: "Hel"},
		{Kind: aistudio.EventText, Text: "lo"},
		{Kind: aistudio.EventFinish, FinishReason: "stop", Usage: &aistudio.Usage{InputTokens: 3, OutputTokens: 2, TotalTokens: 5}},
	}}
	handler := NewHandler(service, Config{APIKey: "sk-test"})
	recorder := postJSON(t, handler, "/v1beta/interactions", `{"model":"test-model","input":"hi","stream":true}`, nil)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Header().Get("Content-Type"), "event-stream") {
		t.Fatalf("status=%d content-type=%q body=%s", recorder.Code, recorder.Header().Get("Content-Type"), recorder.Body.String())
	}
	events := interactionSSE(t, recorder.Body.String())
	want := []string{
		"interaction.created",
		"step.start:thought", "step.delta:thought_summary", "step.delta:thought_signature", "step.stop",
		"step.start:model_output", "step.delta:text", "step.delta:text", "step.stop",
		"interaction.completed",
	}
	if got := interactionEventNames(events); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("事件顺序为 %v，期望 %v", got, want)
	}
	if index := events[5].data["index"]; index != float64(1) {
		t.Fatalf("第二个步骤的 index 为 %v", index)
	}
	completed := events[len(events)-1].data["interaction"].(map[string]any)
	usage, _ := completed["usage"].(map[string]any)
	if completed["status"] != "completed" || usage["total_tokens"] != float64(5) || !strings.HasPrefix(completed["id"].(string), "int_") {
		t.Fatalf("终态不对: %v", completed)
	}
}

// TestInteractionErrorsSanitised 流式中途出错的 error 事件与非流式错误都按官方措辞返回，不带内部中文说明与账户邮箱
func TestInteractionErrorsSanitised(t *testing.T) {
	internal := fmt.Errorf("账户 someone@example.com 的上游连接中断: %w", errUpstreamStream)
	handler := NewHandler(&scriptedService{events: []aistudio.Event{
		{Kind: aistudio.EventText, Text: "partial"},
		{Kind: aistudio.EventError, Err: internal},
	}}, Config{APIKey: "sk-test"})
	stream := postJSON(t, handler, "/v1/interactions", `{"model":"test-model","input":"hi","stream":true}`, nil)
	if strings.Contains(stream.Body.String(), "someone@example.com") || containsCJK(stream.Body.String()) {
		t.Fatalf("流式错误带出了内部说明: %s", stream.Body.String())
	}
	events := interactionSSE(t, stream.Body.String())
	last := events[len(events)-1]
	if last.name != "error" {
		t.Fatalf("最后一个事件为 %s，期望 error: %v", last.name, interactionEventNames(events))
	}
	failure := last.data["error"].(map[string]any)
	if failure["code"] != "INTERNAL" || failure["message"] != officialInternalMessage {
		t.Fatalf("error 事件不是官方措辞: %v", failure)
	}

	plain := postJSON(t, handler, "/v1/interactions", `{"model":"test-model","input":"hi"}`, nil)
	if plain.Code != http.StatusInternalServerError || strings.Contains(plain.Body.String(), "someone@example.com") || containsCJK(plain.Body.String()) {
		t.Fatalf("非流式错误: status=%d body=%s", plain.Code, plain.Body.String())
	}
	var body struct {
		Error struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
			Status  string `json:"status"`
		} `json:"error"`
	}
	if err := json.Unmarshal(plain.Body.Bytes(), &body); err != nil || body.Error.Status != "INTERNAL" || body.Error.Message != officialInternalMessage {
		t.Fatalf("非流式错误不是 Gemini 格式: %s", plain.Body.String())
	}

	// 请求内容里的中文内部错误（工具结果中的图片解码失败）同样换成官方措辞
	invalid := postJSON(t, handler, "/v1/interactions", `{"model":"test-model","input":[
		{"type":"function_call","id":"c1","name":"read","arguments":{}},
		{"type":"function_result","call_id":"c1","result":[{"type":"image","data":"%%%","mime_type":"image/png"}]}]}`, nil)
	if invalid.Code != http.StatusBadRequest || containsCJK(invalid.Body.String()) || !strings.Contains(invalid.Body.String(), "INVALID_ARGUMENT") {
		t.Fatalf("参数错误: status=%d body=%s", invalid.Code, invalid.Body.String())
	}
}

// TestInteractionPassesStreamAndModelAlias 模型后缀照常解析（隐藏思维链、最低思考），Stream 按请求传给生成服务供降级判定使用
func TestInteractionPassesStreamAndModelAlias(t *testing.T) {
	service := &scriptedService{
		modelListService: modelListService{models: []aistudio.Model{{
			ID: "test-model", Methods: []string{"generateContent"},
			Capabilities: map[string]bool{"chat_model": true, "thinking_level": true},
		}}},
		events: []aistudio.Event{
			{Kind: aistudio.EventReasoning, Text: "secret thinking", ThoughtSignature: "sig-hidden"},
			{Kind: aistudio.EventText, Text: "ok"},
			{Kind: aistudio.EventFinish, FinishReason: "stop"},
		},
	}
	handler := NewHandler(service, Config{APIKey: "sk-test"})
	stream := postJSON(t, handler, "/v1beta/interactions", `{"model":"models/test-model-nothinking","input":"hi","stream":true}`, nil)
	if stream.Code != http.StatusOK || service.last == nil {
		t.Fatalf("status=%d body=%s", stream.Code, stream.Body.String())
	}
	if !service.last.Stream || service.last.Model != "test-model" || service.last.Config.ReasoningEffort != "high" {
		t.Fatalf("流式请求传给生成服务的参数不对: stream=%v model=%q effort=%q", service.last.Stream, service.last.Model, service.last.Config.ReasoningEffort)
	}
	if strings.Contains(stream.Body.String(), "secret thinking") || !strings.Contains(stream.Body.String(), "sig-hidden") {
		t.Fatalf("-nothinking 应隐藏思维链并保留签名: %s", stream.Body.String())
	}
	created := interactionSSE(t, stream.Body.String())[0].data["interaction"].(map[string]any)
	if created["model"] != "test-model-nothinking" {
		t.Fatalf("交互资源应报告客户端请求的模型名: %v", created["model"])
	}

	plain := postJSON(t, handler, "/v1/interactions", `{"model":"test-model-128","input":"hi"}`, nil)
	decodeInteraction(t, plain)
	if service.last.Stream || service.last.Model != "test-model" || service.last.Config.ReasoningEffort != "minimal" {
		t.Fatalf("非流式请求传给生成服务的参数不对: stream=%v model=%q effort=%q", service.last.Stream, service.last.Model, service.last.Config.ReasoningEffort)
	}
}

// TestInteractionFunctionCallRoundTrip 上游不带 ID 的函数调用输出本地 ID；该 ID 经 previous_interaction_id
// 续接或无状态回传时都能对应回调用，函数结果补齐名称，调用前的签名挂回调用
func TestInteractionFunctionCallRoundTrip(t *testing.T) {
	service := &scriptedService{events: []aistudio.Event{
		{Kind: aistudio.EventToolCall, ToolCall: &aistudio.FunctionCall{Name: "lookup", Arguments: json.RawMessage(`{"q":"x"}`)}, ThoughtSignature: "sig-call"},
		{Kind: aistudio.EventFinish, FinishReason: "stop"},
	}}
	handler := NewHandler(service, Config{APIKey: "sk-test"})
	tools := `"tools":[{"type":"function","name":"lookup","parameters":{"type":"object","properties":{"q":{"type":"string"}}}}]`
	first := decodeInteraction(t, postJSON(t, handler, "/v1/interactions", `{"model":"test-model","input":"find x",`+tools+`}`, nil))
	steps := interactionResponseSteps(t, first)
	if first["status"] != "requires_action" || len(steps) != 2 || steps[0]["type"] != "thought" || steps[0]["signature"] != "sig-call" || steps[1]["type"] != "function_call" {
		t.Fatalf("第一轮输出不对: %v", first)
	}
	callID, _ := steps[1]["id"].(string)
	if !strings.HasPrefix(callID, "call_local_") {
		t.Fatalf("函数调用 id 为 %q，期望本地补发的非空 ID", callID)
	}

	service.events = []aistudio.Event{{Kind: aistudio.EventText, Text: "done"}, {Kind: aistudio.EventFinish, FinishReason: "stop"}}
	second := postJSON(t, handler, "/v1/interactions", fmt.Sprintf(`{"model":"test-model","previous_interaction_id":%q,"input":[
		{"type":"function_result","call_id":%q,"result":{"answer":42}}]}`, first["id"], callID), nil)
	decodeInteraction(t, second)
	assertCallRoundTrip := func(label string, contents []aistudio.Content) {
		t.Helper()
		var call *aistudio.FunctionCall
		var callSignature string
		var result *aistudio.FunctionResult
		for _, content := range contents {
			for _, part := range content.Parts {
				if part.FunctionCall != nil {
					call, callSignature = part.FunctionCall, part.ThoughtSignature
				}
				if part.FunctionResult != nil {
					result = part.FunctionResult
				}
			}
		}
		if call == nil || call.ID != callID || callSignature != "sig-call" {
			t.Fatalf("%s：历史中的函数调用不对: %+v（签名 %q）", label, call, callSignature)
		}
		if result == nil || result.ID != callID || result.Name != "lookup" {
			t.Fatalf("%s：函数结果没有对应回调用: %+v", label, result)
		}
	}
	assertCallRoundTrip("续接", service.last.Contents)

	stateless := postJSON(t, handler, "/v1/interactions", fmt.Sprintf(`{"model":"test-model","input":[
		{"type":"user_input","content":[{"type":"text","text":"find x"}]},
		{"type":"thought","signature":"sig-call"},
		{"type":"function_call","id":%q,"name":"lookup","arguments":{"q":"x"}},
		{"type":"function_result","call_id":%q,"result":{"answer":42}}]}`, callID, callID), nil)
	decodeInteraction(t, stateless)
	assertCallRoundTrip("无状态回传", service.last.Contents)

	unknown := postJSON(t, handler, "/v1/interactions", `{"model":"test-model","input":[{"type":"function_result","call_id":"missing","result":{}}]}`, nil)
	if unknown.Code != http.StatusBadRequest || !strings.Contains(unknown.Body.String(), "INVALID_ARGUMENT") {
		t.Fatalf("对不上调用的函数结果应返回 400: status=%d body=%s", unknown.Code, unknown.Body.String())
	}

	// 流式同样输出非空 id，函数调用作为完整步骤在 step.start 中给出
	service.events = []aistudio.Event{
		{Kind: aistudio.EventToolCall, ToolCall: &aistudio.FunctionCall{Name: "lookup", Arguments: json.RawMessage(`{}`)}},
		{Kind: aistudio.EventFinish, FinishReason: "stop"},
	}
	stream := postJSON(t, handler, "/v1/interactions", `{"model":"test-model","input":"find x","stream":true,`+tools+`}`, nil)
	for _, event := range interactionSSE(t, stream.Body.String()) {
		if step, ok := event.data["step"].(map[string]any); ok && step["type"] == "function_call" {
			if id, _ := step["id"].(string); !strings.HasPrefix(id, "call_local_") {
				t.Fatalf("流式函数调用 id 为 %q", id)
			}
			return
		}
	}
	t.Fatalf("流式响应没有函数调用步骤: %s", stream.Body.String())
}

// TestInteractionRejectsResponsesPreviousID resp_ 开头的 Responses 响应 ID 不能用于续接交互，即使它存在于共用的状态里
func TestInteractionRejectsResponsesPreviousID(t *testing.T) {
	service := &scriptedService{events: []aistudio.Event{{Kind: aistudio.EventText, Text: "ok"}, {Kind: aistudio.EventFinish, FinishReason: "stop"}}}
	handler := NewHandler(service, Config{APIKey: "sk-test"})
	response := decodeInteraction(t, postJSON(t, handler, "/v1/responses", `{"model":"test-model","input":"hi"}`, nil))
	responseID, _ := response["id"].(string)
	if !strings.HasPrefix(responseID, "resp_") {
		t.Fatalf("Responses 响应 ID 为 %q", responseID)
	}
	service.last = nil
	for _, test := range []struct {
		id      string
		message string
	}{
		{id: responseID, message: "previous_response_id"},
		{id: "resp_123", message: "previous_response_id"},
		{id: "int_404", message: "was not found"},
	} {
		recorder := postJSON(t, handler, "/v1/interactions", fmt.Sprintf(`{"model":"test-model","input":"hi","previous_interaction_id":%q}`, test.id), nil)
		var body struct {
			Error struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
				Status  string `json:"status"`
			} `json:"error"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil || recorder.Code != http.StatusBadRequest ||
			body.Error.Status != "INVALID_ARGUMENT" || !strings.Contains(body.Error.Message, test.message) {
			t.Fatalf("%s: status=%d body=%s", test.id, recorder.Code, recorder.Body.String())
		}
	}
	if service.last != nil {
		t.Fatal("续接 ID 无效时不应调用生成服务")
	}
}

// TestInteractionRequiresAPIKey 不带密钥时三个入口都返回 Gemini 格式的 401，不调用生成服务
func TestInteractionRequiresAPIKey(t *testing.T) {
	service := &scriptedService{}
	handler := NewHandler(service, Config{APIKey: "sk-test", TraceDir: t.TempDir()})
	for _, path := range []string{"/v1/interactions", "/v1beta/interactions", "/trace/v1/interactions"} {
		request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"model":"test-model","input":"hi"}`))
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		var body struct {
			Error struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
				Status  string `json:"status"`
			} `json:"error"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil || recorder.Code != http.StatusUnauthorized ||
			body.Error.Code != http.StatusUnauthorized || body.Error.Status != "UNAUTHENTICATED" || body.Error.Message != officialInvalidKeyMessage {
			t.Fatalf("%s: status=%d body=%s", path, recorder.Code, recorder.Body.String())
		}
	}
	if service.last != nil {
		t.Fatal("鉴权失败时不应调用生成服务")
	}
}

// TestInteractionAudioNotStored 语音输出照常返回完整 WAV，但不放进共用的续接状态；音频分片带的签名仍保留
func TestInteractionAudioNotStored(t *testing.T) {
	service := &scriptedService{events: []aistudio.Event{
		{Kind: aistudio.EventMedia, Media: &aistudio.Media{MIME: "audio/l16;rate=24000", Data: []byte{1, 2, 3, 4}}},
		{Kind: aistudio.EventMedia, Media: &aistudio.Media{MIME: "audio/l16;rate=24000", Data: []byte{5, 6}}, ThoughtSignature: "sig-audio"},
		{Kind: aistudio.EventFinish, FinishReason: "stop"},
	}}
	handler := NewHandler(service, Config{APIKey: "sk-test"})
	first := decodeInteraction(t, postJSON(t, handler, "/v1/interactions",
		`{"model":"test-tts","input":"say hi","response_format":{"type":"audio"},"generation_config":{"speech_config":[{"voice":"Kore"}]}}`, nil))
	if service.last.Config.SpeechConfig == nil || service.last.Config.SpeechConfig.VoiceName != "Kore" {
		t.Fatalf("语音配置没有传给生成服务: %+v", service.last.Config.SpeechConfig)
	}
	steps := interactionResponseSteps(t, first)
	if len(steps) != 1 {
		t.Fatalf("音频应汇总为一个步骤: %v", steps)
	}
	audio := steps[0]["content"].([]any)[0].(map[string]any)
	data, err := base64.StdEncoding.DecodeString(audio["data"].(string))
	if err != nil || audio["mime_type"] != "audio/wav" || audio["sample_rate"] != float64(24000) ||
		len(data) != 44+6 || string(data[:4]) != "RIFF" || string(data[44:]) != "\x01\x02\x03\x04\x05\x06" {
		t.Fatalf("音频内容不对: mime=%v rate=%v len=%d", audio["mime_type"], audio["sample_rate"], len(data))
	}

	service.events = []aistudio.Event{{Kind: aistudio.EventText, Text: "ok"}, {Kind: aistudio.EventFinish, FinishReason: "stop"}}
	decodeInteraction(t, postJSON(t, handler, "/v1/interactions",
		fmt.Sprintf(`{"model":"test-tts","input":"again","previous_interaction_id":%q}`, first["id"]), nil))
	signed := false
	for _, content := range service.last.Contents {
		for _, part := range content.Parts {
			if part.InlineData != nil && strings.HasPrefix(part.InlineData.MIME, "audio/") {
				t.Fatalf("续接历史中带有音频数据: %d 字节", len(part.InlineData.Data))
			}
			signed = signed || content.Role == aistudio.RoleAssistant && part.ThoughtSignature == "sig-audio"
		}
	}
	if !signed {
		t.Fatalf("音频分片的签名没有保留: %+v", service.last.Contents)
	}

	// 流式默认输出 PCM 增量
	service.events = []aistudio.Event{
		{Kind: aistudio.EventMedia, Media: &aistudio.Media{MIME: "audio/l16;rate=24000", Data: []byte{1, 2}}},
		{Kind: aistudio.EventMedia, Media: &aistudio.Media{MIME: "audio/l16;rate=24000", Data: []byte{3, 4}}},
		{Kind: aistudio.EventFinish, FinishReason: "stop"},
	}
	stream := postJSON(t, handler, "/v1/interactions", `{"model":"test-tts","input":"say hi","stream":true,"response_format":{"type":"audio"}}`, nil)
	deltas := 0
	for _, event := range interactionSSE(t, stream.Body.String()) {
		if delta, ok := event.data["delta"].(map[string]any); ok && delta["type"] == "audio" {
			deltas++
			if delta["mime_type"] != "audio/l16" || delta["sample_rate"] != float64(24000) {
				t.Fatalf("PCM 增量不对: %v", delta)
			}
		}
	}
	if deltas != 2 {
		t.Fatalf("流式应逐片输出 PCM，得到 %d 个音频增量", deltas)
	}
}

// TestInteractionFunctionResultMedia function_result 中以 Base64 内嵌的图片先经 splitFunctionResultMedia 取出，作为真正的图片发送
func TestInteractionFunctionResultMedia(t *testing.T) {
	service := &scriptedService{events: []aistudio.Event{{Kind: aistudio.EventText, Text: "ok"}, {Kind: aistudio.EventFinish, FinishReason: "stop"}}}
	handler := NewHandler(service, Config{APIKey: "sk-test"})
	decodeInteraction(t, postJSON(t, handler, "/v1/interactions", `{"model":"test-model","input":[
		{"type":"function_call","id":"c1","name":"screenshot","arguments":{}},
		{"type":"function_result","call_id":"c1","result":[{"type":"text","text":"see image"},{"type":"image","data":"iVBORw0KGgo=","mime_type":"image/png"}]}]}`, nil))
	var image *aistudio.Blob
	var result *aistudio.FunctionResult
	for _, content := range service.last.Contents {
		for _, part := range content.Parts {
			if part.InlineData != nil {
				image = part.InlineData
			}
			if part.FunctionResult != nil {
				result = part.FunctionResult
			}
		}
	}
	if image == nil || image.MIME != "image/png" || string(image.Data) != "\x89PNG\r\n\x1a\n" {
		t.Fatalf("工具结果中的图片没有作为图片发送: %+v", image)
	}
	if result == nil || strings.Contains(string(result.Content), "iVBORw0KGgo") {
		t.Fatalf("工具结果仍带着图片的 Base64: %+v", result)
	}
	// Interactions 写法的 document 与 audio 内容块同样取出
	_, media, err := splitFunctionResultMedia(json.RawMessage(`[
		{"type":"document","data":"JVBERi0=","mime_type":"application/pdf"},
		{"type":"audio","data":"AAAA","mime_type":"audio/wav"}]`))
	if err != nil || len(media) != 2 || media[0].InlineData.MIME != "application/pdf" || media[1].InlineData.MIME != "audio/wav" {
		t.Fatalf("document 与 audio 内容块没有取出: %+v, %v", media, err)
	}
}

// TestInteractionEmptyRequest 既没有输入也没有系统指令时选号前返回 400；只有系统指令时照常生成
func TestInteractionEmptyRequest(t *testing.T) {
	service := &scriptedService{events: []aistudio.Event{{Kind: aistudio.EventText, Text: "ok"}, {Kind: aistudio.EventFinish, FinishReason: "stop"}}}
	handler := NewHandler(service, Config{APIKey: "sk-test"})
	empty := postJSON(t, handler, "/v1/interactions", `{"model":"test-model","input":"  "}`, nil)
	if empty.Code != http.StatusBadRequest || service.last != nil {
		t.Fatalf("空请求: status=%d body=%s", empty.Code, empty.Body.String())
	}
	decodeInteraction(t, postJSON(t, handler, "/v1/interactions", `{"model":"test-model","system_instruction":"say hi"}`, nil))
	if service.last == nil || service.last.System != "say hi" {
		t.Fatalf("只有系统指令的请求没有交给生成服务: %+v", service.last)
	}
}

// TestInteractionSignatureSteps 两个都带签名的 thought 步骤不合并；回传时连续的签名各自保留，后一个挂到随后的调用上
func TestInteractionSignatureSteps(t *testing.T) {
	var result generationResult
	for _, event := range []aistudio.Event{
		{Kind: aistudio.EventReasoning, Text: "a", ThoughtSignature: "sig-a"},
		{Kind: aistudio.EventToolCall, ToolCall: &aistudio.FunctionCall{ID: "c1", Name: "read", ThoughtSignature: "sig-b"}},
		{Kind: aistudio.EventFinish, FinishReason: "stop"},
	} {
		if err := result.apply(event); err != nil {
			t.Fatal(err)
		}
	}
	steps, err := interactionSteps(result, "wav", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 3 || steps[0]["signature"] != "sig-a" || steps[1]["signature"] != "sig-b" || steps[2]["type"] != "function_call" {
		t.Fatalf("签名步骤不对: %v", steps)
	}
	handler := NewHandler(&scriptedService{events: result.events}, Config{APIKey: "sk-test"})
	stream := postJSON(t, handler, "/v1/interactions", `{"model":"test-model","input":"hi","stream":true}`, nil)
	want := []string{
		"interaction.created",
		"step.start:thought", "step.delta:thought_summary", "step.delta:thought_signature", "step.stop",
		"step.start:thought", "step.delta:thought_signature", "step.stop",
		"step.start:function_call", "step.stop",
		"interaction.completed",
	}
	if got := interactionEventNames(interactionSSE(t, stream.Body.String())); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("流式签名步骤为 %v，期望 %v", got, want)
	}
	contents, err := interactionContents(json.RawMessage(`[
		{"type":"user_input","content":[{"type":"text","text":"hi"}]},
		{"type":"thought","signature":"sig-a"},
		{"type":"thought","signature":"sig-b"},
		{"type":"function_call","id":"c1","name":"read","arguments":{}}]`))
	if err != nil {
		t.Fatal(err)
	}
	if len(contents) != 2 || contents[1].Role != aistudio.RoleAssistant || len(contents[1].Parts) != 2 ||
		contents[1].Parts[0].ThoughtSignature != "sig-a" || contents[1].Parts[1].FunctionCall == nil || contents[1].Parts[1].ThoughtSignature != "sig-b" {
		t.Fatalf("回传的签名不对: %+v", contents)
	}
}

// TestInteractionStatus 只有输出上限与内容拦截算提前终止，停止序列按正常结束
func TestInteractionStatus(t *testing.T) {
	for reason, want := range map[string]string{
		"stop": "completed", "stop_sequence": "completed", "malformed_function_call": "completed",
		"max_tokens": "incomplete", "safety": "incomplete",
	} {
		if got := interactionStatus(generationResult{finishReason: reason}); got != want {
			t.Fatalf("%s 的状态为 %s，期望 %s", reason, got, want)
		}
	}
	if got := interactionStatus(generationResult{finishReason: "stop", toolCalls: []aistudio.FunctionCall{{ID: "c1"}}}); got != "requires_action" {
		t.Fatalf("有函数调用时状态为 %s", got)
	}
}
