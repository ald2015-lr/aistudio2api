package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

func capacityBusyError(retry time.Duration) *aistudio.CapacityBusyError {
	return &aistudio.CapacityBusyError{
		ModelID: "test-model", Reason: "账号全部忙碌", Waited: 10 * time.Second, Limit: 10 * time.Second,
		Until: time.Now().Add(retry),
	}
}

// TestCapacityBusyPublicStatus 号池繁忙与全部冷却一样按 429 RESOURCE_EXHAUSTED 返回，三种协议的错误响应都带 Retry-After
func TestCapacityBusyPublicStatus(t *testing.T) {
	busy := capacityBusyError(5 * time.Second)
	if public := publicErrorFor(busy, "test-model"); public.Status != http.StatusTooManyRequests || public.RPC != "RESOURCE_EXHAUSTED" || public.Kind != publicKindRateLimit {
		t.Fatalf("得到 %+v，期望 429 RESOURCE_EXHAUSTED", public)
	}
	if got := statusFromError(busy); got != http.StatusTooManyRequests {
		t.Fatalf("statusFromError = %d，期望 429", got)
	}
	for name, write := range map[string]func(http.ResponseWriter, error){
		"openai": writeOpenAIRequestError, "gemini": writeGeminiRequestError, "anthropic": writeAnthropicRequestError,
	} {
		recorder := httptest.NewRecorder()
		write(recorder, busy)
		if recorder.Code != http.StatusTooManyRequests {
			t.Fatalf("%s 状态码 = %d，期望 429", name, recorder.Code)
		}
		seconds, err := strconv.Atoi(recorder.Header().Get("Retry-After"))
		if err != nil || seconds < 4 || seconds > 6 {
			t.Fatalf("%s Retry-After = %q，期望约 5 秒", name, recorder.Header().Get("Retry-After"))
		}
		if body := recorder.Body.String(); strings.Contains(body, "号池繁忙") || strings.Contains(body, "账号全部忙碌") {
			t.Fatalf("%s 内部原因不能出现在返回给客户端的错误里: %s", name, body)
		}
	}
}

// TestCapacityBusyProtocolShapes 号池繁忙在五种协议入口（流式与非流式）都以 HTTP 429 与官方格式的错误对象返回，带 Retry-After，
// 流式请求不会先回 200 再在流里报错
func TestCapacityBusyProtocolShapes(t *testing.T) {
	failure := aistudio.Event{Kind: aistudio.EventError, Err: capacityBusyError(5 * time.Second)}
	for _, test := range []struct {
		name   string
		path   string
		body   string
		header map[string]string
		check  func(t *testing.T, body map[string]any)
	}{
		{name: "Chat 非流式", path: "/v1/chat/completions", body: `{"model":"gemini-2.5-flash","messages":[{"role":"user","content":"hi"}]}`, check: openAIRateLimitShape},
		{name: "Chat 流式", path: "/v1/chat/completions", body: `{"model":"gemini-2.5-flash","stream":true,"messages":[{"role":"user","content":"hi"}]}`, check: openAIRateLimitShape},
		{name: "Responses 流式", path: "/v1/responses", body: `{"model":"gemini-2.5-flash","stream":true,"input":"hi"}`, check: openAIRateLimitShape},
		{name: "Anthropic 流式", path: "/v1/messages", body: `{"model":"gemini-2.5-flash","stream":true,"max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`, header: map[string]string{"Anthropic-Version": "2023-06-01"}, check: anthropicRateLimitShape},
		{name: "Gemini 流式", path: "/v1beta/models/gemini-2.5-flash:streamGenerateContent?alt=sse", body: `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`, check: geminiRateLimitShape},
		{name: "Interactions 流式", path: "/v1beta/interactions", body: `{"model":"test-model","stream":true,"input":"hi"}`, check: geminiRateLimitShape},
	} {
		t.Run(test.name, func(t *testing.T) {
			handler := NewHandler(&scriptedService{events: []aistudio.Event{failure}}, Config{APIKey: "sk-test"})
			recorder := postJSON(t, handler, test.path, test.body, test.header)
			if recorder.Code != http.StatusTooManyRequests || strings.Contains(recorder.Header().Get("Content-Type"), "event-stream") {
				t.Fatalf("status=%d content-type=%q body=%s", recorder.Code, recorder.Header().Get("Content-Type"), recorder.Body.String())
			}
			if seconds, err := strconv.Atoi(recorder.Header().Get("Retry-After")); err != nil || seconds < 4 || seconds > 6 {
				t.Fatalf("Retry-After = %q，期望约 5 秒", recorder.Header().Get("Retry-After"))
			}
			var body map[string]any
			if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil || body["error"] == nil {
				t.Fatalf("错误响应不是 JSON 错误对象: %s", recorder.Body.String())
			}
			test.check(t, body["error"].(map[string]any))
		})
	}
}

func openAIRateLimitShape(t *testing.T, body map[string]any) {
	t.Helper()
	if body["code"] != "rate_limit_exceeded" || body["message"] != officialExhaustedMessage {
		t.Fatalf("OpenAI 错误对象 = %v", body)
	}
}

func anthropicRateLimitShape(t *testing.T, body map[string]any) {
	t.Helper()
	if body["type"] != "rate_limit_error" {
		t.Fatalf("Anthropic 错误对象 = %v", body)
	}
}

func geminiRateLimitShape(t *testing.T, body map[string]any) {
	t.Helper()
	if body["status"] != "RESOURCE_EXHAUSTED" || body["message"] != officialExhaustedMessage {
		t.Fatalf("Gemini 错误对象 = %v", body)
	}
}

// streamWaitService 是声明流式首事件等待下限的生成服务
type streamWaitService struct {
	scriptedService
	wait time.Duration
}

func (s *streamWaitService) StreamStartWait(string) time.Duration { return s.wait }

// TestStreamStartWaitUsesProvider 写出流式响应头前的首事件等待默认为一个心跳间隔；生成服务为主用模型要求更久时按它，
// 要求更短或不支持时仍按心跳间隔
func TestStreamStartWaitUsesProvider(t *testing.T) {
	plain := &server{service: &scriptedService{}}
	if got := plain.streamStartWait("test-model"); got != streamHeartbeatInterval {
		t.Fatalf("不支持时等待 = %s，期望心跳间隔 %s", got, streamHeartbeatInterval)
	}
	longer := &server{service: &streamWaitService{wait: streamHeartbeatInterval + 5*time.Second}}
	if got := longer.streamStartWait("test-model"); got != streamHeartbeatInterval+5*time.Second {
		t.Fatalf("要求更久时等待 = %s，期望 %s", got, streamHeartbeatInterval+5*time.Second)
	}
	shorter := &server{service: &streamWaitService{wait: time.Second}}
	if got := shorter.streamStartWait("test-model"); got != streamHeartbeatInterval {
		t.Fatalf("要求更短时等待 = %s，期望仍为心跳间隔", got)
	}
}
