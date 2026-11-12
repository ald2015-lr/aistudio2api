package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/api"
)

// backoffFailureService 是首事件前期限在换号退避中到期的测试上游：每次调用都按 fail 失败（直接返回错误，或 stream 为真时返回一个
// 没有任何事件就关闭的事件流），hang 为真时首事件前一直不发事件、挂起到本次尝试被取消（配合 FIRST_EVENT_TIMEOUT）。记录调用次数
type backoffFailureService struct {
	fail   error
	stream bool
	hang   bool

	mu    sync.Mutex
	calls int
}

func (service *backoffFailureService) Models(context.Context) ([]aistudio.Model, error) {
	return nil, nil
}

func (service *backoffFailureService) CountTokens(context.Context, aistudio.TokenCountRequest) (aistudio.TokenCount, error) {
	return aistudio.TokenCount{}, nil
}

func (service *backoffFailureService) Generate(ctx context.Context, _ aistudio.GenerateRequest) (<-chan aistudio.Event, error) {
	service.mu.Lock()
	service.calls++
	service.mu.Unlock()
	switch {
	case service.hang:
		events := make(chan aistudio.Event)
		go func() {
			defer close(events)
			<-ctx.Done()
		}()
		return events, nil
	case service.stream:
		events := make(chan aistudio.Event)
		close(events)
		return events, nil
	}
	return nil, service.fail
}

func (service *backoffFailureService) callCount() int {
	service.mu.Lock()
	defer service.mu.Unlock()
	return service.calls
}

// backoffClientView 为客户端看到的错误：HTTP 状态、Gemini 原生接口的 status 与 code，以及 OpenAI 接口的 error.code 与 error.type
type backoffClientView struct {
	status     int
	message    string
	rpc        string
	openAICode string // JSON 片段：null 或带引号的字符串
	openAIType string
}

// TestGenerateStartDeadlineInRetryBackoff 首事件前期限在两次尝试之间的等待中到期（第一次尝试以可换号的错误失败，退避 200ms 期间期限到期；
// 429 时第二次限流后、换号前等待 2s 期间到期）：原先上一次是上游 RPC 错误（协议错误码 4、5xx、429）或断流时最终错误只留上游原文、
// 没有本服务前缀，请求日志与账本看起来像 Google 侧期限（码 4）或上游错误；现在 request.error 为
// “本服务首事件前期限 <期限> 到期（停在 换号退避|限流换号前等待）: <上一次错误>”，写一行本服务期限截断的进度。
// 客户端看到的与修复前逐字节相同（OpenAI 与 Gemini 原生两种接口）：码 4 与首事件超时为 504 官方原句，上游 503、429 照旧按上一次的
// 错误返回 503、429，其他 5xx（协议错误码 13 的 500 INTERNAL）与首事件前断流为 500，访问日志（账本）的状态随之相同、不是取消
func TestGenerateStartDeadlineInRetryBackoff(t *testing.T) {
	const limit = 180 * time.Millisecond
	twoAccounts := []string{"alice@example.com", "bob@example.com"}
	deadline := backoffClientView{
		status: http.StatusGatewayTimeout, message: "Deadline expired before operation could complete.", rpc: "DEADLINE_EXCEEDED",
		openAICode: "null", openAIType: "server_error",
	}
	for _, test := range []struct {
		name     string
		upstream *backoffFailureService
		accounts []string
		// firstEvent 为 FIRST_EVENT_TIMEOUT（只有挂起的上游用）；jitter 为限流换号前等待的时长（0 为默认）
		firstEvent, jitter time.Duration
		prior, where       string
		calls              int
		client             backoffClientView
	}{
		{
			name: "协议错误码4",
			upstream: &backoffFailureService{fail: &aistudio.RPCError{
				Method: "GenerateContent", StatusCode: http.StatusGatewayTimeout, Code: 4,
				Message: "Deadline expired before operation could complete.",
			}},
			accounts: twoAccounts, where: "换号退避", calls: 1, client: deadline,
			prior: "AI Studio GenerateContent 返回 HTTP 504、协议错误码 4: Deadline expired before operation could complete.",
		},
		{
			name: "上游503",
			upstream: &backoffFailureService{fail: &aistudio.RPCError{
				Method: "GenerateContent", StatusCode: http.StatusServiceUnavailable, Code: 14,
				Message: "The service is currently unavailable.",
			}},
			accounts: twoAccounts, where: "换号退避", calls: 1,
			prior: "AI Studio GenerateContent 返回 HTTP 503、协议错误码 14: The service is currently unavailable.",
			client: backoffClientView{
				status: http.StatusServiceUnavailable, message: "The model is overloaded. Please try again later.", rpc: "UNAVAILABLE",
				openAICode: "null", openAIType: "server_error",
			},
		},
		{
			// 503 以外的 5xx（协议错误码 13 INTERNAL）对外照旧是 500 INTERNAL，不是 503
			name: "上游500",
			upstream: &backoffFailureService{fail: &aistudio.RPCError{
				Method: "GenerateContent", StatusCode: http.StatusInternalServerError, Code: 13,
				Message: "Internal error encountered.",
			}},
			accounts: twoAccounts, where: "换号退避", calls: 1,
			prior: "AI Studio GenerateContent 返回 HTTP 500、协议错误码 13: Internal error encountered.",
			client: backoffClientView{
				status: http.StatusInternalServerError, rpc: "INTERNAL", openAICode: "null", openAIType: "server_error",
				message: "An internal error has occurred. Please retry or report in https://developers.generativeai.google/guide/troubleshooting",
			},
		},
		{
			name:     "首事件前断流",
			upstream: &backoffFailureService{stream: true},
			accounts: twoAccounts, where: "换号退避", calls: 1,
			prior: "AI Studio stream closed before first event",
			client: backoffClientView{
				status: http.StatusInternalServerError, rpc: "INTERNAL", openAICode: "null", openAIType: "server_error",
				message: "An internal error has occurred. Please retry or report in https://developers.generativeai.google/guide/troubleshooting",
			},
		},
		{
			name:       "首事件超时",
			upstream:   &backoffFailureService{hang: true},
			accounts:   twoAccounts,
			firstEvent: 50 * time.Millisecond, where: "换号退避", calls: 1, client: deadline,
			prior: "上游首事件超时：50ms 内没有收到上游事件",
		},
		{
			// 裸限流不退避；第二次限流后、换号前等待 2s（远大于剩余期限），期限在这段等待中到期
			name:     "限流换号前等待",
			upstream: &backoffFailureService{fail: bareRateLimitError()},
			accounts: []string{"alice@example.com", "bob@example.com", "carol@example.com", "dave@example.com"},
			jitter:   2 * time.Second, where: "限流换号前等待", calls: 2,
			prior: "AI Studio GenerateContent 返回 HTTP 429、协议错误码 8: Quota exceeded",
			client: backoffClientView{
				status: http.StatusTooManyRequests, message: "Resource has been exhausted (e.g. check quota).", rpc: "RESOURCE_EXHAUSTED",
				openAICode: `"rate_limit_exceeded"`, openAIType: "requests",
			},
		},
	} {
		for _, endpoint := range []struct {
			name, path, body, want string
		}{
			{
				name: "OpenAI", path: "/v1/chat/completions",
				body: `{"model":"` + testRuntimeModel + `","messages":[{"role":"user","content":"hi"}]}`,
				want: fmt.Sprintf(`{"error":{"code":%s,"message":%q,"param":null,"type":%q}}`, test.client.openAICode, test.client.message, test.client.openAIType),
			},
			{
				name: "Gemini", path: "/v1beta/models/" + testRuntimeModel + ":generateContent",
				body: `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`,
				want: fmt.Sprintf(`{"error":{"code":%d,"message":%q,"status":%q}}`, test.client.status, test.client.message, test.client.rpc),
			},
		} {
			t.Run(test.name+"/"+endpoint.name, func(t *testing.T) {
				upstream := &backoffFailureService{fail: test.upstream.fail, stream: test.upstream.stream, hang: test.upstream.hang}
				service, pool := runningGenerateService(t, upstream, context.Background(), limit, test.accounts...)
				service.firstEventTimeout.Store(int64(test.firstEvent))
				if test.jitter > 0 {
					service.quotaJitter = func() time.Duration { return test.jitter }
				}
				admin := &accessLogCapture{}
				handler := api.NewHandler(service, api.Config{APIKey: "sk-test", Admin: admin})
				request := httptest.NewRequest(http.MethodPost, endpoint.path, strings.NewReader(endpoint.body))
				request.Header.Set("Authorization", "Bearer sk-test")
				request.Header.Set("Content-Type", "application/json")
				recorder := httptest.NewRecorder()
				startedAt := time.Now()
				handler.ServeHTTP(recorder, request)
				elapsed := time.Since(startedAt)

				// 客户端看到的与修复前相同：状态码、正文逐字节一致，不含本服务的中文说明，也没有多出 Retry-After
				if body := strings.TrimSpace(recorder.Body.String()); recorder.Code != test.client.status || body != endpoint.want ||
					recorder.Header().Get("Retry-After") != "" {
					t.Fatalf("客户端收到 %d %s（Retry-After=%q），期望与修复前相同：%d %s",
						recorder.Code, body, recorder.Header().Get("Retry-After"), test.client.status, endpoint.want)
				}
				if elapsed < limit-deadlineEarly || elapsed > limit+deadlineSlack {
					t.Fatalf("用时 %s，期望约首事件前期限 %s", elapsed, limit)
				}
				if calls := upstream.callCount(); calls != test.calls {
					t.Fatalf("上游调用 %d 次，期望 %d 次（失败后在%s中到期、没有下一次尝试）", calls, test.calls, test.where)
				}
				admin.mu.Lock()
				entries := append([]api.AccessLog(nil), admin.entries...)
				admin.mu.Unlock()
				if len(entries) != 1 {
					t.Fatalf("访问日志条数 = %d，期望 1", len(entries))
				}
				// request.error 与用量账本的 error 都取自访问日志的 Error，账本的 status 取自访问日志的 Status
				want := "本服务首事件前期限 180ms 到期（停在 " + test.where + "）: " + test.prior
				if entry := entries[0]; entry.Status != test.client.status || entry.Error != want || entry.Canceled {
					t.Fatalf("访问日志 status=%d canceled=%v error=%q，期望 %d、%q", entry.Status, entry.Canceled, entry.Error, test.client.status, want)
				}
				if !progressLoggedAll(service, "本服务期限截断 | 阶段=首事件前 | 期限=180ms", "停在="+test.where, "原因="+test.prior) {
					t.Fatalf("应写一条本服务首事件前期限截断的进度日志（停在 %s，原因为上一次的错误）", test.where)
				}
				if !waitUntil(5*time.Second, func() bool {
					for _, status := range pool.Status() {
						if status.State == aistudio.AccountBusy {
							return false
						}
					}
					return true
				}) {
					t.Fatal("租约没有释放")
				}
			})
		}
	}
}

// TestBetweenAttemptsDeadlineError 两次尝试之间的等待因首事件前期限到期而结束：上一次的错误（上游 RPC 错误、429、取消、超时类）写在
// 本服务前缀之后；错误链接着上一次的错误（对外照旧按它返回），含 context.Canceled 时只到 context.DeadlineExceeded（504、状态 failed，
// 不会记成 cancelled）；不是首事件前期限到期（还在进行、被取消）或已经标注过时不标注
func TestBetweenAttemptsDeadlineError(t *testing.T) {
	code4 := &aistudio.RPCError{Method: "GenerateContent", StatusCode: http.StatusGatewayTimeout, Code: 4, Message: "Deadline expired before operation could complete."}
	quota := &aistudio.RPCError{Method: "GenerateContent", StatusCode: http.StatusTooManyRequests, Code: 8, Message: "Resource has been exhausted (e.g. check quota)."}
	timeout := &firstEventTimeoutError{timeout: time.Second}
	for _, test := range []struct {
		prior error
		// unwrap 为错误链的下一环；deadline 为最终错误满足 context.DeadlineExceeded
		unwrap   error
		deadline bool
	}{
		{prior: code4, unwrap: code4},
		{prior: quota, unwrap: quota},
		{prior: errors.Join(errors.New("连接被重置"), context.Canceled), unwrap: context.DeadlineExceeded, deadline: true},
		{prior: timeout, unwrap: timeout, deadline: true},
	} {
		gc := newGenerateContext(context.Background(), time.Now(), 20*time.Millisecond, time.Hour)
		if _, ok := gc.betweenAttemptsDeadlineError(test.prior, "换号退避"); ok {
			t.Fatal("期限还没到期时不应标注")
		}
		<-gc.Done()
		cut, ok := gc.betweenAttemptsDeadlineError(test.prior, "换号退避")
		if !ok {
			t.Fatalf("上一次为 %v：期限已到期，应标注", test.prior)
		}
		want := "本服务首事件前期限 20ms 到期（停在 换号退避）: " + test.prior.Error()
		if cut.Error() != want {
			t.Fatalf("错误文本 = %q，期望 %q", cut.Error(), want)
		}
		if next := errors.Unwrap(cut); next != test.unwrap {
			t.Fatalf("上一次为 %v：错误链的下一环 = %v，期望 %v", test.prior, next, test.unwrap)
		}
		if errors.Is(cut, context.DeadlineExceeded) != test.deadline || errors.Is(cut, context.Canceled) ||
			finalRequestState(cut) != "failed" || !gc.startExpired() {
			t.Fatalf("上一次为 %v：DeadlineExceeded=%v（期望 %v），错误链里不能有 context.Canceled、状态应为 failed",
				test.prior, errors.Is(cut, context.DeadlineExceeded), test.deadline)
		}
		if _, ok := gc.betweenAttemptsDeadlineError(cut, "换号退避"); ok {
			t.Fatal("已经标注过的错误不应再标注")
		}
	}
	canceled := newGenerateContext(context.Background(), time.Now(), time.Hour, time.Hour)
	canceled.cancel()
	if _, ok := canceled.betweenAttemptsDeadlineError(quota, "换号退避"); ok {
		t.Fatal("请求被取消（不是期限到期）时不应标注")
	}
	var nilContext *generateContext
	if _, ok := nilContext.betweenAttemptsDeadlineError(quota, "换号退避"); ok {
		t.Fatal("nil 上下文不应标注")
	}
}
