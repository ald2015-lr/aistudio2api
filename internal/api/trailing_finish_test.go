package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// TestTrailingFinishProtocols 上游在函数调用之后结束生成（生成服务交出带 TrailingError 的 stop，见 aistudio/trailing.go）：四个公开协议的流式与
// 非流式都按含函数调用的正常结束输出（tool_calls、completed、tool_use、STOP），不带错误对象、不带 provider_finish_reason，也不透出上游错误的原文
func TestTrailingFinishProtocols(t *testing.T) {
	const leaked = "exceeded your current quota"
	reply := []aistudio.Event{
		{Kind: aistudio.EventToolCall, ToolCall: &aistudio.FunctionCall{ID: "call_1", Name: "lookup", Arguments: json.RawMessage(`{"q":"x"}`)}},
		{Kind: aistudio.EventUsage, Usage: &aistudio.Usage{InputTokens: 7, OutputTokens: 3, TotalTokens: 10}},
		{Kind: aistudio.EventFinish, FinishReason: "stop", TrailingError: &aistudio.RPCError{
			Method: "GenerateContent", StatusCode: http.StatusTooManyRequests, Code: 8, Message: "You " + leaked + ".",
		}},
	}
	tools := `"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object","properties":{"q":{"type":"string"}}}}}]`
	responsesTools := `"tools":[{"type":"function","name":"lookup","parameters":{"type":"object","properties":{"q":{"type":"string"}}}}]`
	anthropicTools := `"tools":[{"name":"lookup","input_schema":{"type":"object","properties":{"q":{"type":"string"}}}}]`
	geminiTools := `"tools":[{"functionDeclarations":[{"name":"lookup","parameters":{"type":"object","properties":{"q":{"type":"string"}}}}]}]`
	for _, test := range []struct {
		name, path, body string
		want             []string
	}{
		{name: "Chat", path: "/v1/chat/completions",
			body: `{"model":"gemini-3.8-flash","messages":[{"role":"user","content":"hi"}],` + tools + `}`,
			want: []string{`"finish_reason":"tool_calls"`, `"name":"lookup"`}},
		{name: "Chat 流式", path: "/v1/chat/completions",
			body: `{"model":"gemini-3.8-flash","stream":true,"messages":[{"role":"user","content":"hi"}],` + tools + `}`,
			want: []string{`"finish_reason":"tool_calls"`, `"name":"lookup"`, "data: [DONE]"}},
		{name: "Responses", path: "/v1/responses",
			body: `{"model":"gemini-3.8-flash","input":"hi",` + responsesTools + `}`,
			want: []string{`"status":"completed"`, `"type":"function_call"`, `"name":"lookup"`}},
		{name: "Responses 流式", path: "/v1/responses",
			body: `{"model":"gemini-3.8-flash","stream":true,"input":"hi",` + responsesTools + `}`,
			want: []string{"event: response.completed", `"type":"function_call"`}},
		{name: "Anthropic", path: "/v1/messages",
			body: `{"model":"gemini-3.8-flash","max_tokens":100,"messages":[{"role":"user","content":"hi"}],` + anthropicTools + `}`,
			want: []string{`"stop_reason":"tool_use"`, `"type":"tool_use"`}},
		{name: "Anthropic 流式", path: "/v1/messages",
			body: `{"model":"gemini-3.8-flash","stream":true,"max_tokens":100,"messages":[{"role":"user","content":"hi"}],` + anthropicTools + `}`,
			want: []string{`"stop_reason":"tool_use"`, "event: message_stop"}},
		{name: "Gemini", path: "/v1beta/models/gemini-3.8-flash:generateContent",
			body: `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],` + geminiTools + `}`,
			want: []string{`"finishReason":"STOP"`, `"functionCall"`}},
		{name: "Gemini 流式", path: "/v1beta/models/gemini-3.8-flash:streamGenerateContent?alt=sse",
			body: `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],` + geminiTools + `}`,
			want: []string{`"finishReason":"STOP"`, `"functionCall"`}},
	} {
		t.Run(test.name, func(t *testing.T) {
			handler := NewHandler(&sequenceService{replies: [][]aistudio.Event{reply}}, Config{APIKey: "sk-test"})
			recorder := postJSON(t, handler, test.path, test.body, nil)
			body := recorder.Body.String()
			if recorder.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", recorder.Code, body)
			}
			for _, want := range test.want {
				if !strings.Contains(body, want) {
					t.Fatalf("响应缺少 %s: %s", want, body)
				}
			}
			for _, unwanted := range []string{leaked, `"error":{`, `"type":"error"`, "provider_finish_reason", "content_filter", "refusal", "event: error"} {
				if strings.Contains(body, unwanted) {
					t.Fatalf("响应不应含 %s: %s", unwanted, body)
				}
			}
		})
	}
}
