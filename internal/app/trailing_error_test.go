package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/api"
)

// trailingUpstream 交出一个函数调用、用量，再以带 TrailingError 的 stop 结束（模拟 aistudio 层把函数调用之后的上游错误改成正常结束）
type trailingUpstream struct {
	trailing error
}

func (upstream *trailingUpstream) Models(context.Context) ([]aistudio.Model, error) { return nil, nil }

func (upstream *trailingUpstream) CountTokens(context.Context, aistudio.TokenCountRequest) (aistudio.TokenCount, error) {
	return aistudio.TokenCount{}, nil
}

func (upstream *trailingUpstream) Generate(context.Context, aistudio.GenerateRequest) (<-chan aistudio.Event, error) {
	events := make(chan aistudio.Event, 3)
	events <- aistudio.Event{
		Kind: aistudio.EventToolCall, ProviderModel: testRuntimeModel,
		ToolCall: &aistudio.FunctionCall{ID: "call-1", Name: "lookup", Arguments: json.RawMessage(`{"q":"x"}`)},
	}
	events <- aistudio.Event{Kind: aistudio.EventUsage, ProviderModel: testRuntimeModel, Usage: &aistudio.Usage{InputTokens: 7, OutputTokens: 3, TotalTokens: 10}}
	events <- aistudio.Event{Kind: aistudio.EventFinish, ProviderModel: testRuntimeModel, FinishReason: "stop", TrailingError: upstream.trailing}
	close(events)
	return events, nil
}

// TestTrailingErrorAfterFunctionCall 上游在函数调用之后结束生成（aistudio 层已改成带 TrailingError 的 stop）：客户端收到正常的 tool_calls 结束；
// 请求日志写一行 WARN，访问日志的错误为空、TrailingError 写明原因，用量账本按工具调用完成记录、错误详情带原因；上游的额度 429 照流内错误的
// 规则写回该模型的冷却，且这次结束不算模型资格验证成功（否则冷却被清掉）。内容策略类原因作为终止原因记录，不涉及账号状态；
// 内容策略类完成帧之后又有上游错误时两者都照做
func TestTrailingErrorAfterFunctionCall(t *testing.T) {
	const account = "trailing@example.com"
	quota := &aistudio.RPCError{Method: "GenerateContent", StatusCode: http.StatusTooManyRequests, Code: 8, Message: "Quota exceeded", RetryDelay: 2 * time.Hour}
	for _, test := range []struct {
		name     string
		trailing error
		finish   string
		state    string
		cooling  bool
	}{
		{name: "额度 429", trailing: quota, finish: "stop", state: "tool_calls", cooling: true},
		{name: "内容策略类原因", trailing: &aistudio.ContentPolicyFinishError{Reason: "safety", Message: "blocked"}, finish: "safety", state: "blocked"},
		// safety 完成帧之后又以额度 429 结束：拦截原因照旧记为终止原因，429 照流内错误写回冷却
		{
			name:     "内容策略类原因之后额度 429",
			trailing: &aistudio.ContentPolicyFinishError{Reason: "safety", Message: "blocked", Cause: quota}, finish: "safety", state: "blocked", cooling: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			service, pool := runningGenerateService(t, &trailingUpstream{trailing: test.trailing}, context.Background(), time.Minute, account)
			admin := &accessLogCapture{}
			handler := api.NewHandler(service, api.Config{APIKey: "sk-test", Admin: admin})
			body := `{"model":"` + testRuntimeModel + `","messages":[{"role":"user","content":"hi"}]}`
			request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
			request.Header.Set("Authorization", "Bearer sk-test")
			request.Header.Set("Content-Type", "application/json")
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, request)
			var response struct {
				Choices []struct {
					FinishReason string `json:"finish_reason"`
					Message      struct {
						ToolCalls []any `json:"tool_calls"`
					} `json:"message"`
				} `json:"choices"`
			}
			if recorder.Code != http.StatusOK || json.Unmarshal(recorder.Body.Bytes(), &response) != nil || len(response.Choices) != 1 ||
				response.Choices[0].FinishReason != "tool_calls" || len(response.Choices[0].Message.ToolCalls) != 1 {
				t.Fatalf("status=%d body=%s，期望正常的 tool_calls 结束", recorder.Code, recorder.Body.String())
			}
			admin.mu.Lock()
			entries := append([]api.AccessLog(nil), admin.entries...)
			admin.mu.Unlock()
			if len(entries) != 1 {
				t.Fatalf("访问日志条数 = %d，期望 1", len(entries))
			}
			entry := entries[0]
			if entry.Status != http.StatusOK || entry.Error != "" || entry.FinishReason != test.finish || !strings.Contains(entry.TrailingError, test.trailing.Error()) {
				t.Fatalf("访问日志 status=%d error=%q finish=%q trailing=%q", entry.Status, entry.Error, entry.FinishReason, entry.TrailingError)
			}
			row := requestRow(entry, time.Now())
			if row.State != test.state || !strings.HasPrefix(row.Error, trailingErrorPrefix) || !strings.Contains(row.Error, test.trailing.Error()) {
				t.Fatalf("账本 state=%q error=%q，期望 %s 并带原因", row.State, row.Error, test.state)
			}
			if countSourceLogs(service.requests, "", "函数调用之后上游结束生成，客户端已收到完整的函数调用，按正常完成结束", test.trailing.Error()) != 1 {
				t.Fatalf("应写一行 WARN：%v", serviceLogMessages(service.requests))
			}
			pool.WaitBackground()
			if cooling := pool.AccountCoolingDown(account, testRuntimeModel); cooling != test.cooling {
				t.Fatalf("冷却 = %t，期望 %t：%v", cooling, test.cooling, serviceLogMessages(service.requests))
			}
			if test.cooling && countSourceLogs(service.requests, "", trailingCallNote) < 2 {
				t.Fatalf("冷却日志应写明发生在函数调用之后：%v", serviceLogMessages(service.requests))
			}
		})
	}
}

// TestSettleTrailingErrorSkipsContinuation long- 续写后续轮次失败（结束事件带 ContinuationErr）已在续写进度里处理过账号状态：
// 只写 WARN，不再按流内错误重复冷却
func TestSettleTrailingErrorSkipsContinuation(t *testing.T) {
	const account = "trailing-long@example.com"
	quota := &aistudio.RPCError{Method: "GenerateContent", StatusCode: http.StatusTooManyRequests, Code: 8, Message: "Quota exceeded", RetryDelay: 2 * time.Hour}
	service, pool := firstEventTestService(t, &trailingUpstream{}, 0, account)
	lease, err := pool.AcquireAccount(context.Background(), account)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lease.Release() }()
	service.settleTrailingError(context.Background(), "req-long", account, lease, testRuntimeModel, aistudio.Event{
		Kind: aistudio.EventFinish, FinishReason: "stop", TrailingError: quota, ContinuationErr: quota,
	})
	pool.WaitBackground()
	if pool.AccountCoolingDown(account, testRuntimeModel) || countSourceLogs(service.requests, "", "函数调用之后上游结束生成") != 1 {
		t.Fatalf("续写失败不应重复写回冷却，WARN 应有一行：%v", serviceLogMessages(service.requests))
	}
}
