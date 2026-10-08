package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// scriptedService 按预设返回事件，并记录最后一次生成请求
type scriptedService struct {
	modelListService
	events []aistudio.Event
	last   *aistudio.GenerateRequest
}

func (s *scriptedService) Generate(_ context.Context, request aistudio.GenerateRequest) (<-chan aistudio.Event, error) {
	copied := request
	s.last = &copied
	events := make(chan aistudio.Event, len(s.events))
	for _, event := range s.events {
		events <- event
	}
	close(events)
	return events, nil
}

func postJSON(t *testing.T, handler http.Handler, path string, body string, header map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer sk-test")
	request.Header.Set("Content-Type", "application/json")
	for name, value := range header {
		request.Header.Set(name, value)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

// TestStreamErrorBeforeFirstEventReturnsHTTPStatus 首个事件就是错误时，流式请求按非流式返回 HTTP 状态与错误对象
func TestStreamErrorBeforeFirstEventReturnsHTTPStatus(t *testing.T) {
	failure := aistudio.Event{Kind: aistudio.EventError, Err: aistudio.ErrInvalidArgument}
	for _, test := range []struct {
		name   string
		path   string
		body   string
		header map[string]string
	}{
		{name: "Chat", path: "/v1/chat/completions", body: `{"model":"gemini-2.5-flash","stream":true,"messages":[{"role":"user","content":"hi"}]}`},
		{name: "Responses", path: "/v1/responses", body: `{"model":"gemini-2.5-flash","stream":true,"input":"hi"}`},
		{name: "Anthropic", path: "/v1/messages", body: `{"model":"gemini-2.5-flash","stream":true,"max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`, header: map[string]string{"Anthropic-Version": "2023-06-01"}},
		{name: "Gemini", path: "/v1beta/models/gemini-2.5-flash:streamGenerateContent?alt=sse", body: `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`},
		{name: "Interactions", path: "/v1beta/interactions", body: `{"model":"test-model","stream":true,"input":"hi"}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			handler := NewHandler(&scriptedService{events: []aistudio.Event{failure}}, Config{APIKey: "sk-test"})
			recorder := postJSON(t, handler, test.path, test.body, test.header)
			if recorder.Code != http.StatusBadRequest || strings.Contains(recorder.Header().Get("Content-Type"), "event-stream") {
				t.Fatalf("status=%d content-type=%q body=%s", recorder.Code, recorder.Header().Get("Content-Type"), recorder.Body.String())
			}
			var body map[string]any
			if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil || body["error"] == nil {
				t.Fatalf("错误响应不是 JSON 错误对象: %s", recorder.Body.String())
			}
		})
	}
}

// TestAwaitStreamStartReplaysEvents 首个事件正常时完整重放事件流
func TestAwaitStreamStartReplaysEvents(t *testing.T) {
	source := make(chan aistudio.Event, 3)
	source <- aistudio.Event{Kind: aistudio.EventText, Text: "a"}
	source <- aistudio.Event{Kind: aistudio.EventText, Text: "b"}
	source <- aistudio.Event{Kind: aistudio.EventFinish}
	close(source)
	events, err := awaitStreamStart(context.Background(), source)
	if err != nil {
		t.Fatal(err)
	}
	var texts []string
	count := 0
	for event := range events {
		count++
		texts = append(texts, event.Text)
	}
	if count != 3 || strings.Join(texts, "") != "ab" {
		t.Fatalf("重放 %d 个事件: %v", count, texts)
	}
	empty := make(chan aistudio.Event)
	close(empty)
	if _, err := awaitStreamStart(context.Background(), empty); err == nil {
		t.Fatal("首个事件前流结束应返回错误")
	}
}

// TestAnthropicPrefillContinues 以 assistant 消息结尾的 Anthropic 请求按续写处理，不再返回 400
func TestAnthropicPrefillContinues(t *testing.T) {
	service := &scriptedService{events: []aistudio.Event{
		{Kind: aistudio.EventText, Text: "world"},
		{Kind: aistudio.EventFinish, FinishReason: "STOP"},
	}}
	handler := NewHandler(service, Config{APIKey: "sk-test"})
	recorder := postJSON(t, handler, "/v1/messages",
		`{"model":"gemini-2.5-flash","max_tokens":16,"messages":[{"role":"user","content":"say hello world"},{"role":"assistant","content":"hello"}]}`,
		map[string]string{"Anthropic-Version": "2023-06-01"})
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	contents := service.last.Contents
	last := contents[len(contents)-1]
	if last.Role != aistudio.RoleUser || len(last.Parts) == 0 || last.Parts[0].Text != prefillContinuationPrompt {
		t.Fatalf("末尾没有补续写消息: %+v", contents)
	}
}
