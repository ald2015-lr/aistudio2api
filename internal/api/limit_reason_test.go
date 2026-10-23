package api

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// TestLimitReasonFor 429 的原因响应头区分本地排队、本地冷却、上游额度与出口限流；不是 429 的错误不写
func TestLimitReasonFor(t *testing.T) {
	until := time.Now().Add(5 * time.Second)
	for name, test := range map[string]struct {
		err  error
		want string
	}{
		"号池繁忙":     {err: capacityBusyError(5 * time.Second), want: LimitReasonLocalQueue},
		"出口限速排队超限": {err: &aistudio.CapacityBusyError{ModelID: "m", Reason: aistudio.EgressBusyReason, Until: until}, want: LimitReasonEgressLimited},
		"全部冷却":     {err: &aistudio.AllCoolingError{Until: until}, want: LimitReasonLocalCooling},
		"发出前已冷却":   {err: aistudio.ErrAccountCoolingDown, want: LimitReasonLocalCooling},
		"上游 429":   {err: &aistudio.RPCError{Method: "GenerateContent", StatusCode: http.StatusTooManyRequests, Code: 8}, want: LimitReasonUpstreamQuota},
		"换号上限":     {err: &aistudio.PrimaryQuotaError{ModelID: "m", Kind: aistudio.QuotaFailSwitchLimit, Until: until}, want: LimitReasonUpstreamQuota},
		"上游出口限流":   {err: &aistudio.PrimaryQuotaError{ModelID: "m", Kind: aistudio.QuotaFailEgress, Until: until}, want: LimitReasonEgressLimited},
		// Ultra 主用模型在出口限流时达到换号上限：Kind 不变（文案不变），按 Reason 认出口限流
		"Ultra 出口限流": {err: &aistudio.PrimaryQuotaError{ModelID: "m", Kind: aistudio.QuotaFailUltraPrimary, Reason: aistudio.QuotaReasonEgress, Until: until}, want: LimitReasonEgressLimited},
		"Ultra 额度不足": {err: &aistudio.PrimaryQuotaError{ModelID: "m", Kind: aistudio.QuotaFailUltraPrimary, Reason: "限流失败已达换号上限", Until: until}, want: LimitReasonUpstreamQuota},
		"其他错误":       {err: errors.New("x"), want: ""},
		// 出口限流期间要冷启动的 Worker 被闸门暂停（转写、Live、Veo，指定账户的生成）：不是本地排队超限
		"冷启动被出口闸门暂停": {err: &aistudio.CapacityBusyError{ModelID: "m", Reason: aistudio.EgressColdStartReason, Until: until}, want: LimitReasonEgressLimited},
	} {
		if got := limitReasonFor(test.err); got != test.want {
			t.Fatalf("%s: 原因 = %q，期望 %q", name, got, test.want)
		}
	}
	recorder := httptest.NewRecorder()
	writeOpenAIRequestError(recorder, &aistudio.RPCError{Method: "GenerateContent", StatusCode: http.StatusServiceUnavailable})
	if recorder.Header().Get(LimitReasonHeader) != "" {
		t.Fatalf("非 429 不应写原因响应头: %q", recorder.Header().Get(LimitReasonHeader))
	}
}

// TestLimitReasonHeaderProtocols 四种协议（OpenAI Chat/Responses、Anthropic、Gemini、Interactions）流式与非流式的 429 都带原因响应头，
// 文案与状态码不变
func TestLimitReasonHeaderProtocols(t *testing.T) {
	for _, failure := range []struct {
		err  error
		want string
	}{
		{err: capacityBusyError(5 * time.Second), want: LimitReasonLocalQueue},
		{err: &aistudio.CapacityBusyError{ModelID: "test-model", Reason: aistudio.EgressBusyReason, Until: time.Now().Add(5 * time.Second)}, want: LimitReasonEgressLimited},
		{err: &aistudio.PrimaryQuotaError{ModelID: "test-model", Kind: aistudio.QuotaFailSwitchLimit, Until: time.Now().Add(5 * time.Second)}, want: LimitReasonUpstreamQuota},
		{err: &aistudio.AllCoolingError{Until: time.Now().Add(5 * time.Second)}, want: LimitReasonLocalCooling},
	} {
		event := aistudio.Event{Kind: aistudio.EventError, Err: failure.err}
		for _, test := range []struct {
			name   string
			path   string
			body   string
			header map[string]string
		}{
			{name: "Chat 非流式", path: "/v1/chat/completions", body: `{"model":"gemini-2.5-flash","messages":[{"role":"user","content":"hi"}]}`},
			{name: "Chat 流式", path: "/v1/chat/completions", body: `{"model":"gemini-2.5-flash","stream":true,"messages":[{"role":"user","content":"hi"}]}`},
			{name: "Responses 流式", path: "/v1/responses", body: `{"model":"gemini-2.5-flash","stream":true,"input":"hi"}`},
			{name: "Anthropic 流式", path: "/v1/messages", body: `{"model":"gemini-2.5-flash","stream":true,"max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`, header: map[string]string{"Anthropic-Version": "2023-06-01"}},
			{name: "Gemini 非流式", path: "/v1beta/models/gemini-2.5-flash:generateContent", body: `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`},
			{name: "Gemini 流式", path: "/v1beta/models/gemini-2.5-flash:streamGenerateContent?alt=sse", body: `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`},
			{name: "Interactions 流式", path: "/v1beta/interactions", body: `{"model":"test-model","stream":true,"input":"hi"}`},
		} {
			handler := NewHandler(&scriptedService{events: []aistudio.Event{event}}, Config{APIKey: "sk-test"})
			recorder := postJSON(t, handler, test.path, test.body, test.header)
			if recorder.Code != http.StatusTooManyRequests {
				t.Fatalf("%s %s: status=%d body=%s", test.name, failure.want, recorder.Code, recorder.Body.String())
			}
			if got := recorder.Header().Get(LimitReasonHeader); got != failure.want {
				t.Fatalf("%s: %s = %q，期望 %q", test.name, LimitReasonHeader, got, failure.want)
			}
		}
	}
}
