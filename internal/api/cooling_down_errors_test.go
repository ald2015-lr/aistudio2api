package api

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// TestAccountCoolingDownPublicStatus 租约账号在请求发出前进入冷却、又不能再换号：与全部冷却一样按 429 RESOURCE_EXHAUSTED 返回，
// 不再落到兜底的 502/500。裸哨兵（测试替身）不带 Retry-After；带类型时 Retry-After 为冷却恢复时间加 0–3 秒抖动；
// errors.Join 包装后仍是 429
func TestAccountCoolingDownPublicStatus(t *testing.T) {
	writers := map[string]func(http.ResponseWriter, error){
		"openai": writeOpenAIRequestError, "gemini": writeGeminiRequestError, "anthropic": writeAnthropicRequestError,
	}
	bare := aistudio.ErrAccountCoolingDown
	if got := statusFromError(bare); got != http.StatusTooManyRequests {
		t.Fatalf("裸哨兵 statusFromError = %d，期望 429", got)
	}
	if public := publicErrorFor(bare, "test-model"); public.Status != http.StatusTooManyRequests || public.RPC != "RESOURCE_EXHAUSTED" ||
		public.Kind != publicKindRateLimit || public.Message != officialExhaustedMessage {
		t.Fatalf("裸哨兵 publicErrorFor = %+v，期望 429 RESOURCE_EXHAUSTED", public)
	}
	for name, write := range writers {
		recorder := httptest.NewRecorder()
		write(recorder, bare)
		if recorder.Code != http.StatusTooManyRequests || recorder.Header().Get("Retry-After") != "" {
			t.Fatalf("%s 裸哨兵：状态码 = %d，Retry-After = %q，期望 429 且不带 Retry-After",
				name, recorder.Code, recorder.Header().Get("Retry-After"))
		}
	}

	typed := &aistudio.AccountCoolingDownError{
		AccountID: "a@example.com", ModelID: "test-model", Until: time.Now().Add(40 * time.Second),
		Reason: aistudio.MinuteQuotaKind + ": AI Studio GenerateContent 返回 HTTP 429",
	}
	for label, err := range map[string]error{"带类型": typed, "errors.Join 包装": errors.Join(typed, errors.New("清理失败"))} {
		if got := statusFromError(err); got != http.StatusTooManyRequests {
			t.Fatalf("%s statusFromError = %d，期望 429", label, got)
		}
		if code := codeFromError(err, ""); code != "rate_limit_exceeded" {
			t.Fatalf("%s codeFromError = %q，期望 rate_limit_exceeded", label, code)
		}
		for name, write := range writers {
			recorder := httptest.NewRecorder()
			write(recorder, err)
			if recorder.Code != http.StatusTooManyRequests {
				t.Fatalf("%s %s 状态码 = %d，期望 429", label, name, recorder.Code)
			}
			seconds, parseErr := strconv.Atoi(recorder.Header().Get("Retry-After"))
			if parseErr != nil || seconds < 40 || seconds > 43 {
				t.Fatalf("%s %s Retry-After = %q，期望 40 到 43 秒", label, name, recorder.Header().Get("Retry-After"))
			}
		}
	}
}
