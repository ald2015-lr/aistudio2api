package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// geminiFinishCandidate 返回响应（或流式最后一帧）第一个 candidate
func geminiFinishCandidate(t *testing.T, body []byte, stream bool) map[string]any {
	t.Helper()
	var response map[string]any
	if stream {
		frames := strings.Split(strings.TrimSpace(string(body)), "\n\n")
		data, ok := strings.CutPrefix(strings.TrimSpace(frames[len(frames)-1]), "data: ")
		if !ok {
			t.Fatalf("最后一帧不是 SSE data: %s", body)
		}
		body = []byte(data)
	}
	if err := json.Unmarshal(body, &response); err != nil {
		t.Fatalf("响应无法解析: %v %s", err, body)
	}
	candidates, _ := response["candidates"].([]any)
	if len(candidates) == 0 {
		t.Fatalf("没有 candidate: %s", body)
	}
	return candidates[0].(map[string]any)
}

// TestGeminiFinishMessageFromUpstream Gemini 输出的 finishMessage 优先用上游原文（流式与非流式）；
// 上游没有说明时仍只为 missing_thought_signature 与 provider_* 补说明
func TestGeminiFinishMessageFromUpstream(t *testing.T) {
	body := `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`
	for _, test := range []struct {
		reason  string
		message string
		want    string
	}{
		{reason: "malformed_function_call", message: "Malformed function call: lookup(", want: "Malformed function call: lookup("},
		{reason: "provider_99", message: "Upstream explanation", want: "Upstream explanation"},
		{reason: "provider_99", want: "AI Studio finish reason 99"},
		{reason: "missing_thought_signature", want: "Missing thought signature"},
		{reason: "safety"},
		{reason: "stop"},
	} {
		events := []aistudio.Event{
			{Kind: aistudio.EventText, Text: "answer"},
			{Kind: aistudio.EventFinish, FinishReason: test.reason, FinishMessage: test.message},
		}
		for _, path := range []string{":generateContent", ":streamGenerateContent?alt=sse"} {
			handler := NewHandler(&scriptedService{events: events}, Config{APIKey: "sk-test"})
			recorder := postJSON(t, handler, "/v1beta/models/test-model"+path, body, nil)
			if recorder.Code != http.StatusOK {
				t.Fatalf("%s %s: status=%d body=%s", test.reason, path, recorder.Code, recorder.Body.String())
			}
			candidate := geminiFinishCandidate(t, recorder.Body.Bytes(), strings.Contains(path, "stream"))
			got, exists := candidate["finishMessage"]
			if test.want == "" {
				if exists {
					t.Fatalf("%s %s: 不应有 finishMessage，得到 %v", test.reason, path, got)
				}
				continue
			}
			if got != test.want {
				t.Fatalf("%s %s: finishMessage=%v，期望 %q", test.reason, path, got, test.want)
			}
		}
	}
}

// TestFinishMessageOnlyInGeminiOutput 其他协议的输出不带上游 finishMessage 原文
func TestFinishMessageOnlyInGeminiOutput(t *testing.T) {
	events := []aistudio.Event{
		{Kind: aistudio.EventText, Text: "answer"},
		{Kind: aistudio.EventFinish, FinishReason: "malformed_function_call", FinishMessage: "Malformed function call: lookup("},
	}
	handler := NewHandler(&scriptedService{events: events}, Config{APIKey: "sk-test"})
	recorder := postJSON(t, handler, "/v1/chat/completions", `{"model":"test-model","messages":[{"role":"user","content":"hi"}]}`, nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if strings.Contains(recorder.Body.String(), "Malformed function call") {
		t.Fatalf("OpenAI 输出不应带上游说明: %s", recorder.Body.String())
	}
}

// TestBlockReasonMessageNotInPublicError 上游的拦截说明只进日志：各协议的公开错误文案仍按拦截原因统一生成
func TestBlockReasonMessageNotInPublicError(t *testing.T) {
	withMessage := publicErrorFor(&aistudio.PromptFeedbackError{Reason: "prohibited_content", Message: "upstream block detail"}, "test-model")
	without := publicErrorFor(&aistudio.PromptFeedbackError{Reason: "prohibited_content"}, "test-model")
	if withMessage != without || strings.Contains(withMessage.Message, "upstream block detail") {
		t.Fatalf("公开错误不应随上游说明变化: %+v / %+v", withMessage, without)
	}
}
