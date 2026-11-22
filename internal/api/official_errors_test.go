package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// TestDowngradeRejectStatus 因降级拒绝按服务配置返回 400（默认，内容策略拦截格式）或 503（服务暂时不可用）
func TestDowngradeRejectStatus(t *testing.T) {
	for _, test := range []struct {
		name    string
		status  int
		want    int
		rpc     string
		message string
	}{
		{name: "未设置按 400", status: 0, want: http.StatusBadRequest, rpc: "INVALID_ARGUMENT", message: aistudio.DowngradePublicMessage},
		{name: "400", status: http.StatusBadRequest, want: http.StatusBadRequest, rpc: "INVALID_ARGUMENT", message: aistudio.DowngradePublicMessage},
		{name: "503", status: http.StatusServiceUnavailable, want: http.StatusServiceUnavailable, rpc: "UNAVAILABLE", message: aistudio.DowngradeUnavailableMessage},
	} {
		t.Run(test.name, func(t *testing.T) {
			public := publicErrorFor(&aistudio.ModelDowngradedError{Model: "gemini-3.1-pro-preview", Status: test.status}, "gemini-3.1-pro-preview")
			if public.Status != test.want || public.RPC != test.rpc || public.Message != test.message {
				t.Fatalf("得到 %+v", public)
			}
			if test.want == http.StatusServiceUnavailable && anthropicTypeForStatus(public.Status, public.Kind) != "overloaded_error" {
				t.Fatalf("503 应映射为 Anthropic overloaded_error，得到 %s", anthropicTypeForStatus(public.Status, public.Kind))
			}
		})
	}
}

// TestInputTokenLimitAlways400 输入 token 超过上限（提示词过长）不看上游状态码，一律按 400 返回：
// 账号上限（131072 写法，上游常按 429 返回）按与安全拦截相同的文案与类别返回（OpenAI code 为 content_policy_violation），
// 不暴露账号的输入上限；带计数的模型本身上限返回谷歌原文，OpenAI 附 context_length_exceeded，Anthropic 以 prompt is too long 开头；
// 普通额度 429 仍按 429
func TestInputTokenLimitAlways400(t *testing.T) {
	const capMessage = "The input token count exceeds the maximum number of tokens allowed 131072."
	const blockedMessage = "Prompt was blocked due to prohibited content. (blockReason: PROHIBITED_CONTENT)"
	for _, test := range []struct {
		name string
		err  *aistudio.RPCError
	}{
		{name: "Playground 429", err: &aistudio.RPCError{Method: "GenerateContent", StatusCode: http.StatusTooManyRequests, Code: 8, Message: capMessage}},
		{name: "Build 429", err: &aistudio.RPCError{Method: "ProxyStreamedCall", StatusCode: http.StatusTooManyRequests, Code: 8, Message: capMessage}},
		{name: "Build 502 无协议码", err: &aistudio.RPCError{Method: "ProxyStreamedCall", StatusCode: http.StatusBadGateway, Message: capMessage}},
		{name: "Playground 400", err: &aistudio.RPCError{Method: "GenerateContent", StatusCode: http.StatusBadRequest, Code: 3, Message: capMessage}},
	} {
		t.Run(test.name, func(t *testing.T) {
			public := publicErrorFor(test.err, "gemini-3.1-pro-preview")
			if public.Status != http.StatusBadRequest || public.RPC != "INVALID_ARGUMENT" || public.Kind != publicKindBlocked || public.Message != blockedMessage {
				t.Fatalf("得到 %+v，期望 400 INVALID_ARGUMENT、blocked 且文案与安全拦截相同", public)
			}
			openai := openAIErrorBody(public)["error"].(map[string]any)
			if openai["code"] != "content_policy_violation" || openai["message"] != blockedMessage {
				t.Fatalf("OpenAI 错误 = %v，期望 code=content_policy_violation 且文案与安全拦截相同", openai)
			}
			anthropic := anthropicErrorBody(public, "req-1")["error"].(map[string]string)
			if anthropic["type"] != "invalid_request_error" || anthropic["message"] != blockedMessage {
				t.Fatalf("Anthropic 错误 = %v，期望 invalid_request_error 且文案与安全拦截相同", anthropic)
			}
			gemini := geminiErrorBody(public)["error"].(map[string]any)
			if gemini["code"] != http.StatusBadRequest || gemini["status"] != "INVALID_ARGUMENT" || gemini["message"] != blockedMessage {
				t.Fatalf("Gemini 错误 = %v，期望 400 INVALID_ARGUMENT 且文案与安全拦截相同", gemini)
			}
		})
	}
	modelLimit := &aistudio.RPCError{Method: "GenerateContent", StatusCode: http.StatusBadRequest, Code: 3, Message: "The input token count (2000000) exceeds the maximum number of tokens allowed (1048576)."}
	public := publicErrorFor(modelLimit, "gemini-3.1-pro-preview")
	if public.Status != http.StatusBadRequest || public.Kind != publicKindContextLength || public.Message != modelLimit.Message {
		t.Fatalf("模型上限得到 %+v，期望 400 context_length 且保留谷歌原文", public)
	}
	if openai := openAIErrorBody(public)["error"].(map[string]any); openai["code"] != "context_length_exceeded" || openai["param"] != "messages" {
		t.Fatalf("模型上限 OpenAI 错误 = %v，期望 code=context_length_exceeded param=messages", openai)
	}
	if anthropic := anthropicErrorBody(public, "req-1")["error"].(map[string]string); !strings.HasPrefix(anthropic["message"], "prompt is too long: ") {
		t.Fatalf("模型上限 Anthropic 错误 = %v，期望以 prompt is too long: 开头", anthropic)
	}
	quota := publicErrorFor(&aistudio.RPCError{Method: "GenerateContent", StatusCode: http.StatusTooManyRequests, Code: 8, Message: "You exceeded your current quota"}, "gemini-3.1-pro-preview")
	if quota.Status != http.StatusTooManyRequests || quota.Kind != publicKindRateLimit {
		t.Fatalf("额度 429 得到 %+v，期望仍按 429 限流返回", quota)
	}
}

// TestAnthropicStreamServerErrorOverloaded Anthropic 已开始流式响应后，5xx 的流内 error 事件类型为 overloaded_error（Claude Code 会在流内自动重试），
// 消息仍是官方措辞；4xx 的流内类型、提交响应头之前与非流式的 5xx 错误体（api_error）不变
func TestAnthropicStreamServerErrorOverloaded(t *testing.T) {
	header := map[string]string{"Anthropic-Version": "2023-06-01"}
	const streamBody = `{"model":"gemini-2.5-flash","stream":true,"max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`
	errorType := func(t *testing.T, body string) (string, string) {
		t.Helper()
		for _, line := range strings.Split(body, "\n") {
			if payload, ok := strings.CutPrefix(line, "data: "); ok && strings.Contains(payload, `"type":"error"`) {
				var event struct {
					Error struct{ Type, Message string } `json:"error"`
				}
				if err := json.Unmarshal([]byte(payload), &event); err != nil {
					t.Fatal(err)
				}
				return event.Error.Type, event.Error.Message
			}
		}
		t.Fatalf("流里没有 error 事件: %s", body)
		return "", ""
	}
	for _, test := range []struct {
		name string
		err  error
		want string
	}{
		{name: "504", err: fmt.Errorf("上游首事件超时: %w", context.DeadlineExceeded), want: "overloaded_error"},
		{name: "502", err: &aistudio.RPCError{Method: "GenerateContent", StatusCode: http.StatusBadGateway, Message: "upstream response ended before finish frame"}, want: "overloaded_error"},
		{name: "429", err: &aistudio.AllCoolingError{Until: time.Now().Add(time.Minute)}, want: "rate_limit_error"},
		{name: "400", err: aistudio.ErrInvalidArgument, want: "invalid_request_error"},
	} {
		t.Run(test.name, func(t *testing.T) {
			events := []aistudio.Event{{Kind: aistudio.EventText, Text: "hello"}, {Kind: aistudio.EventError, Err: test.err}}
			recorder := postJSON(t, NewHandler(&scriptedService{events: events}, Config{APIKey: "sk-test"}), "/v1/messages", streamBody, header)
			kind, message := errorType(t, recorder.Body.String())
			// 重试秒数带随机抖动（见 retryAfterJitterStep），只比较之前的官方措辞
			public, _ := publicErrorWithRetry(test.err, "gemini-2.5-flash")
			official, _, _ := strings.Cut(public.Message, " Please try again in ")
			if recorder.Code != http.StatusOK || kind != test.want || !strings.HasPrefix(message, official) {
				t.Fatalf("status=%d type=%q message=%q，期望流内 %s、官方措辞 %q", recorder.Code, kind, message, test.want, public.Message)
			}
		})
	}
	// 提交响应头之前（第一个事件就是错误）与非流式：仍按 HTTP 状态返回 api_error
	failure := []aistudio.Event{{Kind: aistudio.EventError, Err: fmt.Errorf("上游首事件超时: %w", context.DeadlineExceeded)}}
	for _, body := range []string{streamBody, strings.Replace(streamBody, `"stream":true`, `"stream":false`, 1)} {
		recorder := postJSON(t, NewHandler(&scriptedService{events: failure}, Config{APIKey: "sk-test"}), "/v1/messages", body, header)
		var response struct {
			Error struct{ Type string } `json:"error"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil || recorder.Code != http.StatusGatewayTimeout || response.Error.Type != "api_error" {
			t.Fatalf("响应头之前 / 非流式：%d %s，期望 504 api_error", recorder.Code, recorder.Body.String())
		}
	}
}
