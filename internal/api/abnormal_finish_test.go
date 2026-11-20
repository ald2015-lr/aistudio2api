package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// TestAbnormalFinishErrorIsBadGateway 重新生成用完仍异常结束且没有输出（ABNORMAL_FINISH_ERROR）按 502 返回：英文说明写明结束原因与共生成次数，
// 上游说明不对外，不含 new-api 的自动禁用关键词
func TestAbnormalFinishErrorIsBadGateway(t *testing.T) {
	err := fmt.Errorf("包装: %w", &aistudio.AbnormalFinishError{
		Reason: "malformed_function_call", Message: "You exceeded your current quota: Permission denied", Generations: 3,
	})
	public := publicErrorFor(err, "test-model")
	if public.Status != http.StatusBadGateway || public.RPC != "INTERNAL" ||
		public.Message != "The model ended the response with MALFORMED_FUNCTION_CALL and returned no content (tried 3 times). Please retry." {
		t.Fatalf("public error = %+v", public)
	}
	if retried, seconds := publicErrorWithRetry(err, "test-model"); seconds != 0 || retried.Message != public.Message {
		t.Fatalf("502 不应带重试提示: %+v %d", retried, seconds)
	}
	for _, keyword := range []string{"You exceeded your current quota", "Permission denied", "Operation not allowed", "billing"} {
		if strings.Contains(strings.ToLower(public.Message), strings.ToLower(keyword)) {
			t.Fatalf("说明不应含上游说明或自动禁用关键词 %q: %s", keyword, public.Message)
		}
	}
	once := publicErrorFor(&aistudio.AbnormalFinishError{Reason: "malformed_response", Generations: 1}, "test-model")
	if once.Message != "The model ended the response with MALFORMED_RESPONSE and returned no content. Please retry." {
		t.Fatalf("只生成一次时不写次数: %q", once.Message)
	}
}

// TestAbnormalFinishErrorResponses 生成服务以 *aistudio.AbnormalFinishError 结束时，各协议的非流式与首个事件就是错误的流式请求都按 HTTP 502 返回错误对象；
// Gemini 原生接口的请求标为 KeepAbnormalFinish（照官方形状保留结束原因），其他协议不标
func TestAbnormalFinishErrorResponses(t *testing.T) {
	failure := aistudio.Event{Kind: aistudio.EventError, Err: &aistudio.AbnormalFinishError{Reason: "unexpected_tool_call", Generations: 3}}
	for _, test := range []struct {
		name   string
		path   string
		body   string
		header map[string]string
		keep   bool
	}{
		{name: "Chat", path: "/v1/chat/completions", body: `{"model":"gemini-2.5-flash","messages":[{"role":"user","content":"hi"}]}`},
		{name: "ChatStream", path: "/v1/chat/completions", body: `{"model":"gemini-2.5-flash","stream":true,"messages":[{"role":"user","content":"hi"}]}`},
		{name: "Responses", path: "/v1/responses", body: `{"model":"gemini-2.5-flash","input":"hi"}`},
		{name: "Anthropic", path: "/v1/messages", body: `{"model":"gemini-2.5-flash","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`, header: map[string]string{"Anthropic-Version": "2023-06-01"}},
		{name: "Interactions", path: "/v1beta/interactions", body: `{"model":"test-model","input":"hi"}`},
		{name: "Gemini", path: "/v1beta/models/gemini-2.5-flash:generateContent", body: `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`, keep: true},
		{name: "GeminiStream", path: "/v1beta/models/gemini-2.5-flash:streamGenerateContent?alt=sse", body: `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`, keep: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			service := &scriptedService{events: []aistudio.Event{failure}}
			recorder := postJSON(t, NewHandler(service, Config{APIKey: "sk-test"}), test.path, test.body, test.header)
			if recorder.Code != http.StatusBadGateway {
				t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
			}
			var body map[string]any
			if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil || body["error"] == nil ||
				!strings.Contains(recorder.Body.String(), "UNEXPECTED_TOOL_CALL") {
				t.Fatalf("错误响应 = %s", recorder.Body.String())
			}
			if service.last == nil || service.last.KeepAbnormalFinish != test.keep {
				t.Fatalf("KeepAbnormalFinish 应为 %v: %+v", test.keep, service.last)
			}
		})
	}
}
