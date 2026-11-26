package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// interactionFinalService 用 aistudio.DecodeInteractionStream 解码一段 CreateInteractionStream 原始响应交给协议层：与 generateInteraction
// 一样，还没交出事件就出错时按 Generate 的错误返回，交出之后以错误事件结束。从上游原始字节到客户端响应的端到端
type interactionFinalService struct {
	modelListService
	body []byte
}

func (service *interactionFinalService) Generate(ctx context.Context, _ aistudio.GenerateRequest) (<-chan aistudio.Event, error) {
	var decoded []aistudio.Event
	err := aistudio.DecodeInteractionStream(bytes.NewReader(service.body), func(event aistudio.Event) error {
		decoded = append(decoded, event)
		return nil
	})
	if err != nil && len(decoded) == 0 {
		return nil, err
	}
	if err != nil {
		decoded = append(decoded, aistudio.Event{Kind: aistudio.EventError, Err: err})
	}
	events := make(chan aistudio.Event, len(decoded))
	for _, event := range decoded {
		events <- event
	}
	close(events)
	return events, nil
}

// interactionFinalBody 拼出一段 CreateInteractionStream 响应：text 不为空时先有一段正文 delta，最终 interaction 的状态为 status，
// failure 不为空时写进 field 28（下标 27），用量写在 field 13
func interactionFinalBody(t *testing.T, text string, status int, failure string) []byte {
	t.Helper()
	interaction := make([]any, 28)
	interaction[1] = status
	interaction[12] = []any{4, nil, nil, nil, 6, nil, nil, nil, nil, 10}
	if failure != "" {
		interaction[27] = json.RawMessage(failure)
	}
	final := make([]any, 20)
	final[19] = []any{interaction}
	events := []any{}
	if text != "" {
		delta := make([]any, 11)
		delta[10] = []any{nil, []any{[]any{text}}}
		events = append(events, delta)
	}
	events = append(events, final)
	body, err := json.Marshal([]any{events, []any{0}})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

// interactionExits 为四个公开协议（OpenAI Chat、Responses、Anthropic Messages、Gemini）的流式与非流式请求
var interactionExits = []struct {
	name, path, body string
}{
	{name: "Chat", path: "/v1/chat/completions", body: `{"model":"interaction-test","messages":[{"role":"user","content":"hi"}]}`},
	{name: "Chat 流式", path: "/v1/chat/completions", body: `{"model":"interaction-test","stream":true,"messages":[{"role":"user","content":"hi"}]}`},
	{name: "Responses", path: "/v1/responses", body: `{"model":"interaction-test","input":"hi"}`},
	{name: "Responses 流式", path: "/v1/responses", body: `{"model":"interaction-test","stream":true,"input":"hi"}`},
	{name: "Anthropic", path: "/v1/messages", body: `{"model":"interaction-test","max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`},
	{name: "Anthropic 流式", path: "/v1/messages",
		body: `{"model":"interaction-test","stream":true,"max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`},
	{name: "Gemini", path: "/v1beta/models/interaction-test:generateContent", body: `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`},
	{name: "Gemini 流式", path: "/v1beta/models/interaction-test:streamGenerateContent?alt=sse",
		body: `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`},
}

// TestInteractionMaxTokensExits Interaction 最终状态 6（输出达到上限）按 max_tokens 结束：四个协议的流式与非流式都按各自的截断写法输出
// （OpenAI length、Responses incomplete 与 max_output_tokens、Anthropic max_tokens、Gemini MAX_TOKENS），正文照常交出、不带错误。
// 原先已交出正文时按 other 结束（客户端看到正常结束，不知道被截断），没交出时报“未识别的最终状态”、对外 500
func TestInteractionMaxTokensExits(t *testing.T) {
	want := map[string][]string{
		"Chat":         {`"finish_reason":"length"`, `"content":"Hello"`},
		"Chat 流式":      {`"finish_reason":"length"`, "data: [DONE]"},
		"Responses":    {`"status":"incomplete"`, `"incomplete_details":{"reason":"max_output_tokens"}`, `"output_text":"Hello"`},
		"Responses 流式": {"event: response.incomplete", `"reason":"max_output_tokens"`},
		"Anthropic":    {`"stop_reason":"max_tokens"`, `"text":"Hello"`},
		"Anthropic 流式": {`"stop_reason":"max_tokens"`, "event: message_stop"},
		"Gemini":       {`"finishReason":"MAX_TOKENS"`, `"text":"Hello"`},
		"Gemini 流式":    {`"finishReason":"MAX_TOKENS"`, `"text":"Hello"`},
	}
	for _, exit := range interactionExits {
		t.Run(exit.name, func(t *testing.T) {
			handler := NewHandler(&interactionFinalService{body: interactionFinalBody(t, "Hello", 6, "")}, Config{APIKey: "sk-test"})
			recorder := postJSON(t, handler, exit.path, exit.body, nil)
			body := recorder.Body.String()
			if recorder.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", recorder.Code, body)
			}
			for _, expected := range want[exit.name] {
				if !strings.Contains(body, expected) {
					t.Fatalf("响应缺少 %s: %s", expected, body)
				}
			}
			for _, unwanted := range []string{`"error":{`, "event: error", "provider_finish_reason", `"finish_reason":"stop"`, `"stop_reason":"end_turn"`,
				`"finishReason":"STOP"`, `"finishReason":"OTHER"`, "event: response.completed"} {
				if strings.Contains(body, unwanted) {
					t.Fatalf("响应不应含 %s: %s", unwanted, body)
				}
			}
		})
	}
}

// TestInteractionFailedPublicError Interaction 最终状态 4 带着 field 28 的原因（含 new-api 自动禁用渠道的单词与账户邮箱）：原因只附在内部
// 错误文案里（管理日志），四个协议的流式与非流式对外都是上游 5xx 的官方通用措辞，不含原因、自动禁用单词与邮箱；还没交出正文时为 HTTP 500，
// 已交出正文时以流内错误结束（非流式同样为 500）
func TestInteractionFailedPublicError(t *testing.T) {
	failure := `[["PERMISSION_DENIED","The caller does not have permission: quota for someone@example.com is insufficient"]]`
	for _, text := range []string{"", "Hello"} {
		for _, exit := range interactionExits {
			t.Run(exit.name+"/正文="+text, func(t *testing.T) {
				handler := NewHandler(&interactionFinalService{body: interactionFinalBody(t, text, 4, failure)}, Config{APIKey: "sk-test"})
				recorder := postJSON(t, handler, exit.path, exit.body, nil)
				body := recorder.Body.String()
				streamed := strings.Contains(exit.name, "流式") && text != ""
				if !streamed && recorder.Code != http.StatusInternalServerError {
					t.Fatalf("status=%d，期望 500：%s", recorder.Code, body)
				}
				if !strings.Contains(body, officialInternalMessage) {
					t.Fatalf("响应应为官方通用措辞：%s", body)
				}
				lower := strings.ToLower(body)
				for _, unwanted := range []string{"interaction failed", "permission", "quota", "insufficient", "denied", "example.com", "someone"} {
					if strings.Contains(lower, unwanted) {
						t.Fatalf("对外响应不应含 %q：%s", unwanted, body)
					}
				}
			})
		}
	}
}
