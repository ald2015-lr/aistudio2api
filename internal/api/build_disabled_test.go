package api

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// TestBuildDisabledPublicErrors 官网停用了 Build 通道的生成时给客户端的错误：输入超过 Playground 上限按输入过长返回 400 并写出上限
// （OpenAI code 为 context_length_exceeded、Anthropic 以 prompt is too long 开头，客户端可以压缩上下文），只能经 Build 调用的模型按 503，
// PROXY_GENERATION_DISABLED 原样落到 API 层时也按 503（原文带 "[original: …]"、"generic::…:" 等内部标记时同样）；
// 三种协议的响应体都不含 PROXY_GENERATION_DISABLED 原文
func TestBuildDisabledPublicErrors(t *testing.T) {
	disabled := &aistudio.RPCError{
		Method: "ProxyStreamedCall", StatusCode: http.StatusBadRequest, Code: 3,
		Message: "PROXY_GENERATION_DISABLED: streamGenerateContent is not supported",
	}
	tagged := &aistudio.RPCError{
		Method: "ProxyStreamedCall", StatusCode: http.StatusBadRequest, Code: 3,
		Message: "[original: beyond::dependency::FAILED_PRECONDITION] PROXY_GENERATION_DISABLED: streamGenerateContent is not supported",
	}
	generic := &aistudio.RPCError{
		Method: "ProxyUnaryCall", StatusCode: http.StatusBadRequest, Code: 3,
		Message: "generic::failed_precondition: PROXY_GENERATION_DISABLED: generateContent is not supported",
	}
	capErr := &aistudio.RPCError{
		Method: "GenerateContent", StatusCode: http.StatusTooManyRequests, Code: 8,
		Message: "The input token count exceeds the maximum number of tokens allowed 131072",
	}
	input := &aistudio.BuildGenerationUnavailableError{
		Reason: aistudio.BuildUnavailableInputLimit, Model: "gemini-3.1-pro-preview", InputLimit: 131072, Cause: errors.Join(capErr, disabled),
	}
	model := &aistudio.BuildGenerationUnavailableError{Reason: aistudio.BuildUnavailableModel, Model: "gemini-4-argon", Cause: disabled}
	writers := map[string]func(http.ResponseWriter, error){
		"openai": writeOpenAIRequestError, "gemini": writeGeminiRequestError, "anthropic": writeAnthropicRequestError,
	}
	for _, test := range []struct {
		name     string
		err      error
		status   int
		contains []string
	}{
		{"输入超过 Playground 上限", input, http.StatusBadRequest, []string{"131072", "Build channel"}},
		{"输入超过 Playground 上限（errors.Join 包装）", errors.Join(input, errors.New("删除临时副本失败")), http.StatusBadRequest, []string{"131072"}},
		{"只能经 Build 调用的模型", model, http.StatusServiceUnavailable, []string{"gemini-4-argon", "only available through"}},
		{"原样落到 API 层的 PROXY_GENERATION_DISABLED", fmt.Errorf("换号结束: %w", disabled), http.StatusServiceUnavailable, []string{"Build channel"}},
		// 原文带上游内部标记：识别前同样去掉标记，对外不含原文
		{"带内部标记的 PROXY_GENERATION_DISABLED", fmt.Errorf("换号结束: %w", tagged), http.StatusServiceUnavailable, []string{"Build channel"}},
		{"带 generic 前缀的 PROXY_GENERATION_DISABLED", generic, http.StatusServiceUnavailable, []string{"Build channel"}},
	} {
		if got := statusFromError(test.err); got != test.status {
			t.Fatalf("%s：statusFromError = %d，期望 %d", test.name, got, test.status)
		}
		public := publicErrorFor(test.err, "gemini-3.1-pro-preview")
		if public.Status != test.status || strings.Contains(public.Message, "PROXY_GENERATION_DISABLED") {
			t.Fatalf("%s：publicErrorFor = %+v", test.name, public)
		}
		if test.status == http.StatusBadRequest && (public.Kind != publicKindContextLength || public.RPC != "INVALID_ARGUMENT") {
			t.Fatalf("%s：应按输入过长返回，得到 %+v", test.name, public)
		}
		if test.status == http.StatusServiceUnavailable && public.RPC != "UNAVAILABLE" {
			t.Fatalf("%s：应按 UNAVAILABLE 返回，得到 %+v", test.name, public)
		}
		for protocol, write := range writers {
			recorder := httptest.NewRecorder()
			write(recorder, test.err)
			body := recorder.Body.String()
			if recorder.Code != test.status || strings.Contains(body, "PROXY_GENERATION_DISABLED") {
				t.Fatalf("%s %s：状态码 %d，响应 %s", test.name, protocol, recorder.Code, body)
			}
			for _, fragment := range test.contains {
				if !strings.Contains(body, fragment) {
					t.Fatalf("%s %s：响应应包含 %q：%s", test.name, protocol, fragment, body)
				}
			}
			if test.status == http.StatusBadRequest {
				switch protocol {
				case "openai":
					if !strings.Contains(body, `"code":"context_length_exceeded"`) {
						t.Fatalf("OpenAI 输入超限应带 context_length_exceeded：%s", body)
					}
				case "anthropic":
					if !strings.Contains(body, "prompt is too long") {
						t.Fatalf("Anthropic 输入超限应以 prompt is too long 开头：%s", body)
					}
				}
			}
		}
	}
	// 普通的 Build 400（参数错误）照旧按请求本身的错误返回上游原文
	invalid := &aistudio.RPCError{Method: "ProxyStreamedCall", StatusCode: http.StatusBadRequest, Code: 3, Message: "Request contains an invalid argument."}
	if public := publicErrorFor(invalid, "m"); public.Status != http.StatusBadRequest || public.Message != "Request contains an invalid argument." {
		t.Fatalf("普通 400 = %+v，期望不受影响", public)
	}
}
