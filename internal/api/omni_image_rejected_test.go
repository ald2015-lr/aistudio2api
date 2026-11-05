package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// TestInteractionImageRejectedPublicError Omni 带图请求被上游以 403 拒绝、不再换号（aistudio.InteractionImageRejectedError）时对外按
// 400 FAILED_PRECONDITION 返回英文说明（写明 Omni 拒绝了带图请求、可以去掉图片重试），不按里面上游错误的 403 映射成 503，
// 也不透出上游的 “does not have permission” 原文；Gemini、OpenAI Chat、Anthropic 三种格式（含流式）一致
func TestInteractionImageRejectedPublicError(t *testing.T) {
	cause := &aistudio.RPCError{Method: "CreateInteractionStream", StatusCode: http.StatusForbidden, Code: 7, Message: "The caller does not have permission"}
	rejected := &aistudio.InteractionImageRejectedError{Model: "gemini-omni-1.1-flash", Tried: 2, Images: 1, ImageBinding: true, Cause: cause}
	for _, err := range []error{rejected, fmt.Errorf("包装: %w", rejected)} {
		public := publicErrorFor(err, "gemini-omni-1.1-flash")
		if public.Status != http.StatusBadRequest || public.RPC != "FAILED_PRECONDITION" || public.Message != rejected.PublicMessage() {
			t.Fatalf("%v -> %+v", err, public)
		}
		if status := statusFromError(err); status != http.StatusBadRequest {
			t.Fatalf("statusFromError = %d", status)
		}
		if kind := anthropicErrorType(err); kind != "invalid_request_error" {
			t.Fatalf("Anthropic 错误类型 = %q", kind)
		}
	}
	// 对照：同一个上游 403 不经包装时仍按服务不可用返回（纯文字请求与其他模型的 403 不变）
	if public := publicErrorFor(cause, "gemini-omni-1.1-flash"); public.Status != http.StatusServiceUnavailable {
		t.Fatalf("上游 403 -> %+v", public)
	}
	service := &driveRequiredService{modelListService: modelListService{models: omniAliasModels()}, err: rejected}
	for _, test := range []struct {
		path, body string
		header     map[string]string
	}{
		{path: "/v1beta/models/gemini-omni-1.1-flash:generateContent", body: `{"contents":[{"role":"user","parts":[{"text":"生成视频"}]}]}`},
		{path: "/v1beta/models/gemini-omni-1.1-flash:streamGenerateContent?alt=sse", body: `{"contents":[{"role":"user","parts":[{"text":"生成视频"}]}]}`},
		{path: "/v1/chat/completions", body: `{"model":"gemini-omni-1.1-flash","messages":[{"role":"user","content":"生成视频"}]}`},
		{path: "/v1/chat/completions", body: `{"model":"gemini-omni-1.1-flash","stream":true,"messages":[{"role":"user","content":"生成视频"}]}`},
		{path: "/v1/messages", body: `{"model":"gemini-omni-1.1-flash","max_tokens":16,"messages":[{"role":"user","content":"生成视频"}]}`,
			header: map[string]string{"Anthropic-Version": "2023-06-01"}},
	} {
		handler, _ := newMediaHandler(t, service, &mediaSettingsStub{})
		recorder := postJSON(t, handler, test.path, test.body, test.header)
		var response struct {
			Error struct {
				Message string `json:"message"`
				Status  string `json:"status"`
				Type    string `json:"type"`
			} `json:"error"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil || recorder.Code != http.StatusBadRequest ||
			response.Error.Message != rejected.PublicMessage() {
			t.Fatalf("%s: %d %s", test.path, recorder.Code, recorder.Body.String())
		}
		if strings.HasPrefix(test.path, "/v1beta") && response.Error.Status != "FAILED_PRECONDITION" {
			t.Fatalf("Gemini 错误状态 = %q", response.Error.Status)
		}
		if test.path == "/v1/messages" && response.Error.Type != "invalid_request_error" {
			t.Fatalf("Anthropic 错误类型 = %q", response.Error.Type)
		}
		if body := strings.ToLower(recorder.Body.String()); strings.Contains(body, "permission") || strings.Contains(body, "caller") {
			t.Fatalf("%s: 对外错误不应含上游原文: %s", test.path, recorder.Body.String())
		}
	}
}
