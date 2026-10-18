package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// primaryQuotaError 返回 Ultra 主用模型额度不足的错误：Cause 为带上游原文的额度 429
func primaryQuotaError(retry time.Duration) *aistudio.PrimaryQuotaError {
	return &aistudio.PrimaryQuotaError{
		ModelID: "test-model", Pool: aistudio.PoolScopeUltra, Reason: "额度失败已达换号上限", QuotaHits: 3,
		Elapsed: 1200 * time.Millisecond, Until: time.Now().Add(retry),
		Cause: &aistudio.RPCError{
			Method: "GenerateContent", StatusCode: http.StatusTooManyRequests, Code: 8,
			Message: "You exceeded your current quota, please check your plan and billing details.",
		},
	}
}

// TestPrimaryQuotaPublicStatus 额度不足与全部冷却一样按 429 RESOURCE_EXHAUSTED 返回、带 Retry-After，不被当成上游 RPCError：
// 对外消息为固定的官方文案，不含上游原文（其中的 "You exceeded your current quota" 会让中转自动禁用渠道）与内部原因
func TestPrimaryQuotaPublicStatus(t *testing.T) {
	quota := primaryQuotaError(5 * time.Second)
	var rpcError *aistudio.RPCError
	if errors.As(quota, &rpcError) {
		t.Fatal("额度不足错误不应展开成上游 RPCError")
	}
	if public := publicErrorFor(quota, "test-model"); public.Status != http.StatusTooManyRequests || public.RPC != "RESOURCE_EXHAUSTED" ||
		public.Kind != publicKindRateLimit || public.Message != officialExhaustedMessage {
		t.Fatalf("得到 %+v，期望 429 RESOURCE_EXHAUSTED 与固定文案", public)
	}
	if got := statusFromError(quota); got != http.StatusTooManyRequests {
		t.Fatalf("statusFromError = %d，期望 429", got)
	}
	if code := codeFromError(quota, ""); code != "rate_limit_exceeded" {
		t.Fatalf("codeFromError = %q", code)
	}
}

// TestPrimaryQuotaProtocolShapes 额度不足在五种协议入口（流式与非流式）都以 HTTP 429、官方格式的错误对象与 Retry-After 返回，
// 流式请求不会先回 200 再在流里报错；消息是固定文案，不含上游原文与内部原因
func TestPrimaryQuotaProtocolShapes(t *testing.T) {
	failure := aistudio.Event{Kind: aistudio.EventError, Err: primaryQuotaError(5 * time.Second)}
	anthropic := map[string]string{"Anthropic-Version": "2023-06-01"}
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
		t.Run(test.name, func(t *testing.T) {
			handler := NewHandler(&scriptedService{events: []aistudio.Event{failure}}, Config{APIKey: "sk-test"})
			recorder := postJSON(t, handler, test.path, test.body, test.header)
			if recorder.Code != http.StatusTooManyRequests || strings.Contains(recorder.Header().Get("Content-Type"), "event-stream") {
				t.Fatalf("status=%d content-type=%q body=%s", recorder.Code, recorder.Header().Get("Content-Type"), recorder.Body.String())
			}
			if seconds, err := strconv.Atoi(recorder.Header().Get("Retry-After")); err != nil || seconds < 4 || seconds > 6 {
				t.Fatalf("Retry-After = %q，期望约 5 秒", recorder.Header().Get("Retry-After"))
			}
			raw := recorder.Body.String()
			for _, leaked := range []string{"You exceeded your current quota", "额度", "换号", "Ultra"} {
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

func anthropicQuotaShape(t *testing.T, body map[string]any) {
	t.Helper()
	if body["type"] != "rate_limit_error" || body["message"] != officialExhaustedMessage {
		t.Fatalf("Anthropic 错误对象 = %v", body)
	}
}

// scopeRecordingService 记录 StreamStartWait 收到的号池，并返回按号池给出的等待下限
type scopeRecordingService struct {
	scriptedService
	mu     sync.Mutex
	scopes []aistudio.PoolScope
}

func (s *scopeRecordingService) StreamStartWait(ctx context.Context, _ string) time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.scopes = append(s.scopes, aistudio.PoolScopeFromContext(ctx))
	return 0
}

var _ StreamStartWaitProvider = (*scopeRecordingService)(nil)

// TestStreamStartWaitGetsRequestPool 五种协议的流式入口都把请求的 context 交给 StreamStartWait：/ultra 请求为 Ultra 号池，
// 普通路径在独占模式下为普通号池
func TestStreamStartWaitGetsRequestPool(t *testing.T) {
	anthropic := map[string]string{"Anthropic-Version": "2023-06-01"}
	streams := []struct {
		path   string
		body   string
		header map[string]string
	}{
		{path: "/v1/chat/completions", body: `{"model":"test-model","stream":true,"messages":[{"role":"user","content":"hi"}]}`},
		{path: "/v1/responses", body: `{"model":"test-model","stream":true,"input":"hi"}`},
		{path: "/v1/messages", body: `{"model":"test-model","stream":true,"max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`, header: anthropic},
		{path: "/v1beta/models/test-model:streamGenerateContent?alt=sse", body: `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`},
		{path: "/v1beta/interactions", body: `{"model":"test-model","stream":true,"input":"hi"}`},
	}
	for _, prefix := range []string{"", "/ultra"} {
		want := aistudio.PoolScopeNormal
		if prefix != "" {
			want = aistudio.PoolScopeUltra
		}
		for _, stream := range streams {
			service := &scopeRecordingService{scriptedService: scriptedService{events: []aistudio.Event{
				{Kind: aistudio.EventError, Err: primaryQuotaError(5 * time.Second)},
			}}}
			handler := NewHandler(service, Config{APIKey: "sk-test"})
			recorder := postJSON(t, handler, prefix+stream.path, stream.body, stream.header)
			if recorder.Code != http.StatusTooManyRequests {
				t.Fatalf("%s%s：status=%d body=%s", prefix, stream.path, recorder.Code, recorder.Body.String())
			}
			service.mu.Lock()
			scopes := append([]aistudio.PoolScope(nil), service.scopes...)
			service.mu.Unlock()
			if len(scopes) != 1 || scopes[0] != want {
				t.Fatalf("%s%s：StreamStartWait 收到的号池 = %v，期望 %s", prefix, stream.path, scopes, want)
			}
		}
	}
}

// holdRecordingService 记录每次 Generate 收到的写出响应头前的等待延长记录
type holdRecordingService struct {
	scriptedService
	holds []*aistudio.StreamStartHold
}

func (s *holdRecordingService) Generate(ctx context.Context, request aistudio.GenerateRequest) (<-chan aistudio.Event, error) {
	s.holds = append(s.holds, aistudio.StreamStartHoldFromContext(ctx))
	return s.scriptedService.Generate(ctx, request)
}

// TestStreamRequestsCarryStartHold 五种协议的流式入口都把一份写出响应头前的等待延长记录交给生成服务（生成服务为请求现场冷启动时
// 用它推迟写出响应头）；非流式请求没有
func TestStreamRequestsCarryStartHold(t *testing.T) {
	anthropic := map[string]string{"Anthropic-Version": "2023-06-01"}
	for _, test := range []struct {
		name   string
		path   string
		body   string
		header map[string]string
		stream bool
	}{
		{name: "Chat 流式", path: "/v1/chat/completions", body: `{"model":"test-model","stream":true,"messages":[{"role":"user","content":"hi"}]}`, stream: true},
		{name: "Chat 非流式", path: "/v1/chat/completions", body: `{"model":"test-model","messages":[{"role":"user","content":"hi"}]}`},
		{name: "Responses 流式", path: "/v1/responses", body: `{"model":"test-model","stream":true,"input":"hi"}`, stream: true},
		{name: "Responses 非流式", path: "/v1/responses", body: `{"model":"test-model","input":"hi"}`},
		{name: "Anthropic 流式", path: "/v1/messages", body: `{"model":"test-model","stream":true,"max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`, header: anthropic, stream: true},
		{name: "Anthropic 非流式", path: "/v1/messages", body: `{"model":"test-model","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`, header: anthropic},
		{name: "Gemini 流式", path: "/v1beta/models/test-model:streamGenerateContent?alt=sse", body: `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`, stream: true},
		{name: "Gemini 非流式", path: "/v1beta/models/test-model:generateContent", body: `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`},
		{name: "Interactions 流式", path: "/v1beta/interactions", body: `{"model":"test-model","stream":true,"input":"hi"}`, stream: true},
		{name: "Interactions 非流式", path: "/v1beta/interactions", body: `{"model":"test-model","input":"hi"}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			service := &holdRecordingService{scriptedService: scriptedService{events: []aistudio.Event{
				{Kind: aistudio.EventError, Err: primaryQuotaError(5 * time.Second)},
			}}}
			recorder := postJSON(t, NewHandler(service, Config{APIKey: "sk-test"}), test.path, test.body, test.header)
			if recorder.Code != http.StatusTooManyRequests {
				t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
			}
			if len(service.holds) != 1 || (service.holds[0] != nil) != test.stream {
				t.Fatalf("Generate 收到的等待延长记录 = %v，流式=%v", service.holds, test.stream)
			}
		})
	}
}

// TestAwaitStreamStartHoldsDuringColdStart 生成服务现场冷启动进行中，写出响应头前的等待过了也不开始推流：启动结束后才来的错误
// （例如冷启动的账号返回额度 429）仍按 HTTP 状态返回；没有等待延长记录时照旧在等待时间到时开始推流
func TestAwaitStreamStartHoldsDuringColdStart(t *testing.T) {
	const wait = 50 * time.Millisecond
	const launch = 300 * time.Millisecond
	run := func(ctx context.Context, hold *aistudio.StreamStartHold) (<-chan aistudio.Event, error, time.Duration) {
		events := make(chan aistudio.Event, 1)
		hold.BeginColdStart(time.Second)
		go func() {
			time.Sleep(launch)
			hold.EndColdStart(launch)
			events <- aistudio.Event{Kind: aistudio.EventError, Err: primaryQuotaError(5 * time.Second)}
			close(events)
		}()
		startedAt := time.Now()
		stream, err := awaitStreamStart(ctx, events, wait)
		return stream, err, time.Since(startedAt)
	}
	hold := aistudio.NewStreamStartHold()
	stream, err, elapsed := run(aistudio.WithStreamStartHold(context.Background(), hold), hold)
	var quota *aistudio.PrimaryQuotaError
	if stream != nil || !errors.As(err, &quota) {
		t.Fatalf("stream=%v err=%v 用时 %s，期望冷启动结束后的额度不足在写出响应头前返回", stream, err, elapsed)
	}
	if elapsed < launch {
		t.Fatalf("用时 %s，期望等到冷启动结束（%s）", elapsed, launch)
	}
	if extension, starting := hold.State(); starting || extension != launch {
		t.Fatalf("等待延长 %s（进行中=%v），期望 %s", extension, starting, launch)
	}
	// 没有等待延长记录（非 Ultra 主用模型的请求不改动它，效果相同）：等待时间到就开始推流，错误留在流里
	stream, err, elapsed = run(context.Background(), nil)
	if stream == nil || err != nil || elapsed >= launch {
		t.Fatalf("stream=%v err=%v 用时 %s，没有等待延长记录时期望约 %s 后开始推流", stream, err, elapsed, wait)
	}
	drainEvents(stream)
}

// TestAwaitStreamStartExtendsAfterColdStart 现场冷启动结束后写出响应头前的等待截止时间延后启动时长：等待时间早已过去、
// 启动结束后仍没有事件时，在 原等待 + 延长 时开始推流，不是启动一结束就开始，也不会等到启动开始时给出的上限
func TestAwaitStreamStartExtendsAfterColdStart(t *testing.T) {
	const wait = 50 * time.Millisecond
	const extend = 400 * time.Millisecond
	hold := aistudio.NewStreamStartHold()
	hold.BeginColdStart(5 * time.Second)
	timer := time.AfterFunc(100*time.Millisecond, func() { hold.EndColdStart(extend) })
	defer timer.Stop()
	events := make(chan aistudio.Event)
	defer close(events)
	ctx, cancel := context.WithTimeout(aistudio.WithStreamStartHold(context.Background(), hold), 5*time.Second)
	defer cancel()
	startedAt := time.Now()
	stream, err := awaitStreamStart(ctx, events, wait)
	elapsed := time.Since(startedAt)
	if stream == nil || err != nil {
		t.Fatalf("stream=%v err=%v 用时 %s，期望延长的等待结束后开始推流", stream, err, elapsed)
	}
	if elapsed < wait+extend-10*time.Millisecond || elapsed > wait+extend+time.Second {
		t.Fatalf("用时 %s，期望约 %s（原等待 + 延长）", elapsed, wait+extend)
	}
}

// awaitHeldStream 在启动一直不结束的等待延长记录下等首个事件（事件流一直没有事件），返回开始推流前的用时
func awaitHeldStream(t *testing.T, hold *aistudio.StreamStartHold, wait time.Duration, holdMax time.Duration) time.Duration {
	t.Helper()
	events := make(chan aistudio.Event)
	defer close(events)
	ctx, cancel := context.WithTimeout(aistudio.WithStreamStartHold(context.Background(), hold), 5*time.Second)
	defer cancel()
	startedAt := time.Now()
	stream, err := awaitStreamStartWithin(ctx, events, wait, holdMax)
	elapsed := time.Since(startedAt)
	if stream == nil || err != nil {
		t.Fatalf("stream=%v err=%v 用时 %s，期望到时开始推流，不是一直压着响应头直到请求取消", stream, err, elapsed)
	}
	return elapsed
}

// TestAwaitStreamStartHoldBoundedByCredit 现场冷启动进行中写出响应头前的等待只延后这次启动还能抵扣的时长，启动比它久时到时照常
// 开始推流：原先启动进行中一直不写出响应头，排队预算的抵扣（INIT_TIMEOUT）用完后再现场冷启动一次，客户端要多等整段启动
// （默认最长约 等待 + 2×INIT_TIMEOUT）都收不到任何字节，换来的额度不足仍是流内错误
func TestAwaitStreamStartHoldBoundedByCredit(t *testing.T) {
	const wait = 50 * time.Millisecond
	const credit = 250 * time.Millisecond
	hold := aistudio.NewStreamStartHold()
	hold.BeginColdStart(credit)
	elapsed := awaitHeldStream(t, hold, wait, time.Minute)
	if elapsed < wait+credit-10*time.Millisecond || elapsed > wait+credit+time.Second {
		t.Fatalf("用时 %s，期望约 %s（原等待 + 还能抵扣的时长）", elapsed, wait+credit)
	}
	// 抵扣已用完（上限 0）：启动进行中也不推迟
	none := aistudio.NewStreamStartHold()
	none.BeginColdStart(0)
	if elapsed := awaitHeldStream(t, none, wait, time.Minute); elapsed > wait+time.Second {
		t.Fatalf("用时 %s，抵扣用完时期望约 %s 后开始推流", elapsed, wait)
	}
}

// TestAwaitStreamStartHoldMax 推迟写出响应头最多到开始等待后 holdMax：前面的反向代理与中转读不到响应头时按读超时断开（nginx 默认
// 60 秒），断开还会取消进行中的启动，原先一次 50 秒的冷启动在默认配置的 nginx 后面就成了 504；写出响应头前的等待本来就比 holdMax
// 长时照旧等满，不因上限提前开始推流
func TestAwaitStreamStartHoldMax(t *testing.T) {
	if streamStartHoldMax >= 60*time.Second {
		t.Fatalf("streamStartHoldMax = %s，应短于 nginx 默认的读超时 60 秒", streamStartHoldMax)
	}
	hold := aistudio.NewStreamStartHold()
	hold.BeginColdStart(time.Minute)
	const holdMax = 300 * time.Millisecond
	if elapsed := awaitHeldStream(t, hold, 50*time.Millisecond, holdMax); elapsed < holdMax-10*time.Millisecond || elapsed > holdMax+time.Second {
		t.Fatalf("用时 %s，期望约 %s（推迟上限）", elapsed, holdMax)
	}
	const wait = 400 * time.Millisecond
	if elapsed := awaitHeldStream(t, hold, wait, 100*time.Millisecond); elapsed < wait-10*time.Millisecond || elapsed > wait+time.Second {
		t.Fatalf("用时 %s，等待本来就比推迟上限长时期望等满 %s", elapsed, wait)
	}
}
