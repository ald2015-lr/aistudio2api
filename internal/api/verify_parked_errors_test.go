package api

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// TestVerifyParkedPublicError 候选全部只被需要验证账号的冷却挡住（Parked 的全部冷却）、或租约账号在发出前被需要验证账号的冷却拦下：
// 三种协议都按 503 UNAVAILABLE 返回，OpenAI code 为 model_unavailable，不带 Retry-After、消息里没有 “Please try again”，
// 不带 429 原因头，不含 new-api 自动禁用的关键词；/ultra、/pro 用号池的说法。对照：不 Parked 的全部冷却仍是 429 + Retry-After
func TestVerifyParkedPublicError(t *testing.T) {
	until := time.Now().Add(30 * 24 * time.Hour)
	for _, test := range []struct {
		name    string
		err     error
		message string
	}{
		{name: "全部单个模型需要验证", err: &aistudio.AllCoolingError{ModelID: "test-model", Until: until, Parked: true},
			message: "Model test-model is listed, but no account in the pool can serve this request right now. Please try again later."},
		{name: "包了一层", err: fmt.Errorf("选号: %w", &aistudio.AllCoolingError{ModelID: "test-model", Until: until, Parked: true}),
			message: "Model test-model is listed, but no account in the pool can serve this request right now. Please try again later."},
		{name: "Pro 号池", err: &aistudio.AllCoolingError{ModelID: "test-model", Until: until, Parked: true, Pool: aistudio.PoolScopePro},
			message: officialProUnavailableMessage},
		{name: "Ultra 号池", err: &aistudio.AllCoolingError{ModelID: "test-model", Until: until, Parked: true, Pool: aistudio.PoolScopeUltra},
			message: officialUltraUnavailableMessage},
		{name: "发出前被拦下", err: errors.Join(errors.New("尝试失败"), &aistudio.AccountCoolingDownError{
			AccountID: "a@example.com", ModelID: "test-model", Until: until, Reason: aistudio.ModelVerifyRequiredReason,
		}), message: "Model test-model is listed, but no account in the pool can serve this request right now. Please try again later."},
		// 生成路径在请求以它结束时补上请求的号池：/pro、/ultra 与选号就判成 Parked 时同一说法
		{name: "发出前被拦下（Pro 号池）", err: &aistudio.AccountCoolingDownError{
			AccountID: "a@example.com", ModelID: "test-model", Until: until, Reason: aistudio.ModelVerifyRequiredReason, Pool: aistudio.PoolScopePro,
		}, message: officialProUnavailableMessage},
		{name: "发出前被拦下（Ultra 号池，整户暂停）", err: errors.Join(errors.New("尝试失败"), &aistudio.AccountCoolingDownError{
			AccountID: "a@example.com", ModelID: "test-model", Until: until, Reason: aistudio.VerifyRequiredPauseReason, Pool: aistudio.PoolScopeUltra,
		}), message: officialUltraUnavailableMessage},
	} {
		if got := statusFromError(test.err); got != http.StatusServiceUnavailable {
			t.Fatalf("%s：statusFromError = %d，期望 503", test.name, got)
		}
		if got := openAIErrorCode(test.err); got != "model_unavailable" {
			t.Fatalf("%s：OpenAI code = %q", test.name, got)
		}
		public, seconds := publicErrorWithRetry(test.err, "test-model")
		if public.Status != http.StatusServiceUnavailable || public.RPC != "UNAVAILABLE" || public.Code != "model_unavailable" ||
			seconds != 0 || public.Message != test.message {
			t.Fatalf("%s：对外错误 = %+v 秒数=%d", test.name, public, seconds)
		}
		for name, write := range map[string]func(http.ResponseWriter, error){
			"openai": writeOpenAIRequestError, "gemini": writeGeminiRequestError, "anthropic": writeAnthropicRequestError,
		} {
			recorder := httptest.NewRecorder()
			write(recorder, test.err)
			body := recorder.Body.String()
			if recorder.Code != http.StatusServiceUnavailable || recorder.Header().Get("Retry-After") != "" ||
				recorder.Header().Get(LimitReasonHeader) != "" || strings.Contains(body, "Please try again in") ||
				strings.Contains(body, "You exceeded your current quota") || strings.Contains(body, "verify") {
				t.Fatalf("%s/%s：状态 %d Retry-After=%q 原因头=%q 正文=%s", test.name, name, recorder.Code,
					recorder.Header().Get("Retry-After"), recorder.Header().Get(LimitReasonHeader), body)
			}
		}
	}
	// 对照：有会自行恢复的候选（不 Parked）仍按 429 与 Retry-After
	cooling := &aistudio.AllCoolingError{ModelID: "test-model", Until: time.Now().Add(time.Hour)}
	recorder := httptest.NewRecorder()
	writeOpenAIRequestError(recorder, cooling)
	if recorder.Code != http.StatusTooManyRequests || recorder.Header().Get("Retry-After") == "" || statusFromError(cooling) != http.StatusTooManyRequests {
		t.Fatalf("不 Parked 的全部冷却应仍为 429 + Retry-After: %d %q", recorder.Code, recorder.Header().Get("Retry-After"))
	}
	quota := &aistudio.AccountCoolingDownError{Until: time.Now().Add(time.Hour), Reason: "每日限额: x"}
	if statusFromError(quota) != http.StatusTooManyRequests {
		t.Fatal("每日限额拦下的仍为 429")
	}
}
