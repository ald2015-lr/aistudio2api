package api

import (
	"net/http"
	"strings"
	"testing"

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

// TestInputTokenLimitAlways400 输入 token 超过上限（提示词过长）不看上游状态码：部分账号的 131072 输入上限由上游按 429 返回，
// 也一律按 400 返回谷歌原文，OpenAI 附 context_length_exceeded，Anthropic 以 prompt is too long 开头；普通额度 429 仍按 429
func TestInputTokenLimitAlways400(t *testing.T) {
	const capMessage = "The input token count exceeds the maximum number of tokens allowed 131072"
	for _, test := range []struct {
		name string
		err  *aistudio.RPCError
	}{
		{name: "Playground 429", err: &aistudio.RPCError{Method: "GenerateContent", StatusCode: http.StatusTooManyRequests, Code: 8, Message: capMessage}},
		{name: "Build 429", err: &aistudio.RPCError{Method: "ProxyStreamedCall", StatusCode: http.StatusTooManyRequests, Code: 8, Message: capMessage}},
		{name: "Build 502 无协议码", err: &aistudio.RPCError{Method: "ProxyStreamedCall", StatusCode: http.StatusBadGateway, Message: capMessage}},
		{name: "400", err: &aistudio.RPCError{Method: "GenerateContent", StatusCode: http.StatusBadRequest, Code: 3, Message: "The input token count (2000000) exceeds the maximum number of tokens allowed (1048576)."}},
	} {
		t.Run(test.name, func(t *testing.T) {
			public := publicErrorFor(test.err, "gemini-3.1-pro-preview")
			if public.Status != http.StatusBadRequest || public.RPC != "INVALID_ARGUMENT" || public.Kind != publicKindContextLength || public.Message != test.err.Message {
				t.Fatalf("得到 %+v，期望 400 INVALID_ARGUMENT、context_length 且保留谷歌原文", public)
			}
			openai := openAIErrorBody(public)["error"].(map[string]any)
			if openai["code"] != "context_length_exceeded" || openai["param"] != "messages" {
				t.Fatalf("OpenAI 错误 = %v，期望 code=context_length_exceeded param=messages", openai)
			}
			anthropic := anthropicErrorBody(public, "req-1")["error"].(map[string]string)
			if anthropic["type"] != "invalid_request_error" || !strings.HasPrefix(anthropic["message"], "prompt is too long: ") {
				t.Fatalf("Anthropic 错误 = %v，期望 invalid_request_error 且以 prompt is too long: 开头", anthropic)
			}
			gemini := geminiErrorBody(public)["error"].(map[string]any)
			if gemini["code"] != http.StatusBadRequest || gemini["status"] != "INVALID_ARGUMENT" {
				t.Fatalf("Gemini 错误 = %v，期望 400 INVALID_ARGUMENT", gemini)
			}
		})
	}
	quota := publicErrorFor(&aistudio.RPCError{Method: "GenerateContent", StatusCode: http.StatusTooManyRequests, Code: 8, Message: "You exceeded your current quota"}, "gemini-3.1-pro-preview")
	if quota.Status != http.StatusTooManyRequests || quota.Kind != publicKindRateLimit {
		t.Fatalf("额度 429 得到 %+v，期望仍按 429 限流返回", quota)
	}
}
