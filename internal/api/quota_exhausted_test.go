package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// 普通号池换号上限与上游出口限流的额度不足（PrimaryQuotaError 的两个新 Kind）：对外映射与 Ultra 主用模型相同

// exhaustedUpstreamCause 为带上游原文的额度 429（其中的 "You exceeded your current quota" 不能出现在对外消息里）
func exhaustedUpstreamCause() error {
	return &aistudio.RPCError{
		Method: "GenerateContent", StatusCode: http.StatusTooManyRequests, Code: 8,
		Message: "You exceeded your current quota, please check your plan and billing details.",
	}
}

// switchLimitQuotaError 返回普通号池限流类换号已达上限的额度不足
func switchLimitQuotaError(retry time.Duration) *aistudio.PrimaryQuotaError {
	return &aistudio.PrimaryQuotaError{
		ModelID: "test-model", Pool: aistudio.PoolScopeNormal, Reason: "限流失败已达换号上限", RateHits: 3, Accounts: 3,
		Elapsed: 1200 * time.Millisecond, Until: time.Now().Add(retry), Cause: exhaustedUpstreamCause(), Kind: aistudio.QuotaFailSwitchLimit,
	}
}

// egressQuotaError 返回上游出口限流、换号已达上限的额度不足
func egressQuotaError(retry time.Duration) *aistudio.PrimaryQuotaError {
	return &aistudio.PrimaryQuotaError{
		ModelID: "test-model", Reason: "上游出口限流", RateHits: 3, Accounts: 3, Elapsed: 8400 * time.Millisecond,
		Until: time.Now().Add(retry), Cause: exhaustedUpstreamCause(), Kind: aistudio.QuotaFailEgress, Egress: "直连", EgressLimit: 24,
	}
}

// TestQuotaExhaustedKindsPublicStatus 两个新 Kind 与 Ultra 主用模型一样按 429 RESOURCE_EXHAUSTED、rate_limit_exceeded 与固定的官方文案返回，
// 不被当成上游 RPCError，带 Retry-After；内部文案不含 Ultra 主用模型的“主用模型”字样
func TestQuotaExhaustedKindsPublicStatus(t *testing.T) {
	for _, quota := range []*aistudio.PrimaryQuotaError{switchLimitQuotaError(5 * time.Second), egressQuotaError(7 * time.Second)} {
		var rpcError *aistudio.RPCError
		if errors.As(quota, &rpcError) {
			t.Fatalf("Kind %d：额度不足错误不应展开成上游 RPCError", quota.Kind)
		}
		public := publicErrorFor(quota, "test-model")
		if public.Status != http.StatusTooManyRequests || public.RPC != "RESOURCE_EXHAUSTED" || public.Kind != publicKindRateLimit ||
			public.Message != officialExhaustedMessage {
			t.Fatalf("Kind %d：得到 %+v，期望 429 RESOURCE_EXHAUSTED 与固定文案", quota.Kind, public)
		}
		if strings.Contains(public.Message, "You exceeded your current quota") {
			t.Fatalf("Kind %d：对外消息不应包含上游原文", quota.Kind)
		}
		if got := statusFromError(quota); got != http.StatusTooManyRequests {
			t.Fatalf("Kind %d：statusFromError = %d，期望 429", quota.Kind, got)
		}
		if code := codeFromError(quota, ""); code != "rate_limit_exceeded" {
			t.Fatalf("Kind %d：codeFromError = %q", quota.Kind, code)
		}
		if strings.Contains(quota.Error(), "主用模型") {
			t.Fatalf("Kind %d：内部文案不应含“主用模型”（诊断脚本按它识别 Ultra）: %s", quota.Kind, quota.Error())
		}
	}
	switchLimit := switchLimitQuotaError(5 * time.Second).Error()
	for _, fragment := range []string{"额度不足：普通号池的模型 test-model 换号重试已达上限，立即返回（原因：限流失败已达换号上限", "额度失败 0 次", "限流失败 3 次",
		"尝试账号 3 个", "总耗时 1.2s", "建议重试时间"} {
		if !strings.Contains(switchLimit, fragment) {
			t.Fatalf("换号上限文案 %q 缺少 %q", switchLimit, fragment)
		}
	}
	egress := egressQuotaError(7 * time.Second)
	for _, fragment := range []string{"上游限流：模型 test-model 所在出口 直连 正在限流，立即返回（限速 24 次/分钟", "限流失败 3 次", "尝试账号 3 个", "总耗时 8.4s"} {
		if !strings.Contains(egress.Error(), fragment) {
			t.Fatalf("出口限流文案 %q 缺少 %q", egress.Error(), fragment)
		}
	}
	egress.EgressLimit = 0
	if !strings.Contains(egress.Error(), "（限速已停止，") {
		t.Fatalf("限速已停止时的文案 = %q", egress.Error())
	}
}

// TestQuotaExhaustedProtocolShapes 两个新 Kind 在五种协议入口（流式与非流式）都以 HTTP 429、官方格式的错误对象与 Retry-After 返回，
// 流式请求不会先回 200 再在流里报错；消息是固定文案，不含上游原文与内部原因
func TestQuotaExhaustedProtocolShapes(t *testing.T) {
	anthropic := map[string]string{"Anthropic-Version": "2023-06-01"}
	for _, kind := range []struct {
		name  string
		err   *aistudio.PrimaryQuotaError
		retry int
	}{
		{name: "换号上限", err: switchLimitQuotaError(5 * time.Second), retry: 5},
		{name: "出口限流", err: egressQuotaError(7 * time.Second), retry: 7},
	} {
		for _, test := range []struct {
			name   string
			path   string
			body   string
			header map[string]string
			check  func(t *testing.T, body map[string]any)
		}{
			{name: "Chat 非流式", path: "/v1/chat/completions", body: `{"model":"test-model","messages":[{"role":"user","content":"hi"}]}`, check: openAIRateLimitShape},
			{name: "Chat 流式", path: "/v1/chat/completions", body: `{"model":"test-model","stream":true,"messages":[{"role":"user","content":"hi"}]}`, check: openAIRateLimitShape},
			{name: "Responses 非流式", path: "/v1/responses", body: `{"model":"test-model","input":"hi"}`, check: openAIRateLimitShape},
			{name: "Responses 流式", path: "/v1/responses", body: `{"model":"test-model","stream":true,"input":"hi"}`, check: openAIRateLimitShape},
			{name: "Anthropic 非流式", path: "/v1/messages", body: `{"model":"test-model","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`, header: anthropic, check: anthropicQuotaShape},
			{name: "Anthropic 流式", path: "/v1/messages", body: `{"model":"test-model","stream":true,"max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`, header: anthropic, check: anthropicQuotaShape},
			{name: "Gemini 非流式", path: "/v1beta/models/test-model:generateContent", body: `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`, check: geminiRateLimitShape},
			{name: "Gemini 流式", path: "/v1beta/models/test-model:streamGenerateContent?alt=sse", body: `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`, check: geminiRateLimitShape},
			{name: "Interactions 非流式", path: "/v1beta/interactions", body: `{"model":"test-model","input":"hi"}`, check: geminiRateLimitShape},
			{name: "Interactions 流式", path: "/v1beta/interactions", body: `{"model":"test-model","stream":true,"input":"hi"}`, check: geminiRateLimitShape},
		} {
			t.Run(kind.name+" · "+test.name, func(t *testing.T) {
				failure := aistudio.Event{Kind: aistudio.EventError, Err: kind.err}
				handler := NewHandler(&scriptedService{events: []aistudio.Event{failure}}, Config{APIKey: "sk-test"})
				recorder := postJSON(t, handler, test.path, test.body, test.header)
				if recorder.Code != http.StatusTooManyRequests || strings.Contains(recorder.Header().Get("Content-Type"), "event-stream") {
					t.Fatalf("status=%d content-type=%q body=%s", recorder.Code, recorder.Header().Get("Content-Type"), recorder.Body.String())
				}
				// Retry-After 为建议重试时间加 0–3 秒抖动（见 retryAfterSeconds）
				if seconds, err := strconv.Atoi(recorder.Header().Get("Retry-After")); err != nil || seconds < kind.retry-1 || seconds > kind.retry+4 {
					t.Fatalf("Retry-After = %q，期望约 %d 秒", recorder.Header().Get("Retry-After"), kind.retry)
				}
				raw := recorder.Body.String()
				for _, leaked := range []string{"You exceeded your current quota", "额度", "换号", "上游限流", "出口", "直连", "Ultra"} {
					if strings.Contains(raw, leaked) {
						t.Fatalf("对外错误不应包含 %q: %s", leaked, raw)
					}
				}
				var body map[string]any
				if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil || body["error"] == nil {
					t.Fatalf("错误响应不是 JSON 错误对象: %s", raw)
				}
				test.check(t, body["error"].(map[string]any))
			})
		}
	}
}

// TestPrimaryQuotaErrorUltraTextUnchanged Kind 零值（Ultra 主用模型）的文案与原格式逐字相同：诊断脚本与既有测试依赖它
func TestPrimaryQuotaErrorUltraTextUnchanged(t *testing.T) {
	until := time.Date(2026, 10, 23, 8, 0, 5, 0, time.UTC)
	cause := exhaustedUpstreamCause()
	for _, test := range []struct {
		name  string
		quota *aistudio.PrimaryQuotaError
		want  string
	}{
		{
			name: "完整",
			quota: &aistudio.PrimaryQuotaError{
				ModelID: "gemini-4-argon", Pool: aistudio.PoolScopeUltra, Reason: "额度失败已达换号上限", QuotaHits: 3, RateHits: 2,
				Elapsed: 1500 * time.Millisecond, Until: until, Cause: cause,
				// Accounts 只写进普通号池的文案，Ultra 主用模型的文案不变
				Accounts: 5,
			},
			want: fmt.Sprintf("额度不足：Ultra 号池的 Ultra 主用模型 gemini-4-argon 没有可用额度，立即返回（原因：额度失败已达换号上限，额度失败 3 次，"+
				"限流失败 2 次，总耗时 1.5s，建议重试时间 %s）：%s", until.Local().Format(time.RFC3339), cause.Error()),
		},
		{
			name:  "不限号池、没有限流失败与时间",
			quota: &aistudio.PrimaryQuotaError{ModelID: "gemini-4-argon", Reason: "没有其他可换的账号", QuotaHits: 1},
			want:  "额度不足：不限号池的 Ultra 主用模型 gemini-4-argon 没有可用额度，立即返回（原因：没有其他可换的账号，额度失败 1 次）",
		},
	} {
		if test.quota.Kind != aistudio.QuotaFailUltraPrimary {
			t.Fatalf("%s：Kind 零值应为 Ultra 主用模型", test.name)
		}
		if got := test.quota.Error(); got != test.want {
			t.Fatalf("%s：文案 = %q\n期望 %q", test.name, got, test.want)
		}
	}
}
