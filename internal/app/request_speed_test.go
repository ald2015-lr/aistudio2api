package app

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/api"
)

// speedEvent 为测试上游交出的一个事件与交出前的等待
type speedEvent struct {
	wait  time.Duration
	event aistudio.Event
}

// speedService 是测试用的上游：按 events 的等待依次交出事件
type speedService struct {
	*stallingService
	events []speedEvent
}

func (service *speedService) Generate(ctx context.Context, request aistudio.GenerateRequest) (<-chan aistudio.Event, error) {
	service.mu.Lock()
	service.calls = append(service.calls, request.AccountID)
	service.mu.Unlock()
	events := make(chan aistudio.Event)
	go func() {
		defer close(events)
		for _, paced := range service.events {
			select {
			case <-time.After(paced.wait):
			case <-ctx.Done():
				return
			}
			event := paced.event
			event.ProviderModel = testRuntimeModel
			select {
			case events <- event:
			case <-ctx.Done():
				return
			}
		}
	}()
	return events, nil
}

// serveAccessLog 经公开 API 发一次非流式 Chat 请求，返回 HTTP 状态与写出的访问日志
func serveAccessLog(t *testing.T, service *trackedService) (int, api.AccessLog) {
	t.Helper()
	admin := &accessLogCapture{}
	handler := api.NewHandler(service, api.Config{APIKey: "sk-test", Admin: admin})
	body := `{"model":"` + testRuntimeModel + `","messages":[{"role":"user","content":"hi"}]}`
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer sk-test")
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	admin.mu.Lock()
	defer admin.mu.Unlock()
	if len(admin.entries) != 1 {
		t.Fatalf("访问日志条数 = %d，期望 1", len(admin.entries))
	}
	return recorder.Code, admin.entries[0]
}

// TestGenerationSpeedAfterFirstChunk 请求日志的生成速度只计首个上游分块之后：首个分块之前的等待（排队、准备、同账号重新生成都在这之前）
// 不算进时长；原来的平均速度（总耗时）照旧写出；只有一个分块时测不出，不写生成速度
func TestGenerationSpeedAfterFirstChunk(t *testing.T) {
	usage := aistudio.Event{Kind: aistudio.EventUsage, Usage: &aistudio.Usage{InputTokens: 10, OutputTokens: 100, TotalTokens: 110}}
	upstream := &speedService{stallingService: newStallingService(), events: []speedEvent{
		{wait: 400 * time.Millisecond, event: aistudio.Event{Kind: aistudio.EventText, Text: "a"}},
		{wait: 200 * time.Millisecond, event: aistudio.Event{Kind: aistudio.EventText, Text: "b"}},
		{event: usage}, {event: aistudio.Event{Kind: aistudio.EventFinish, FinishReason: "stop"}},
	}}
	service, _ := runningGenerateService(t, upstream, context.Background(), time.Minute, "alice@example.com")
	status, entry := serveAccessLog(t, service)
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	if entry.FirstOutput < 400*time.Millisecond || entry.FirstOutput >= entry.Latency {
		t.Fatalf("首个分块 = %s、总耗时 = %s，期望首个分块在 400ms 之后", entry.FirstOutput, entry.Latency)
	}
	data := requestLogData(entry)
	window := (entry.Latency - entry.FirstOutput).Seconds()
	if data.Usage == nil || data.Usage.GenerationTokensPerSecond != 100/window ||
		data.Usage.AverageTokensPerSecond != 100/entry.Latency.Seconds() ||
		data.Usage.GenerationTokensPerSecond <= 2*data.Usage.AverageTokensPerSecond {
		t.Fatalf("速度 = %+v，期望生成速度只计首个分块之后（约为平均速度的 3 倍）", data.Usage)
	}

	// 只有一个分块：测不出生成速度，不写；原来的平均速度照旧
	single := &speedService{stallingService: newStallingService(), events: []speedEvent{
		{wait: 100 * time.Millisecond, event: aistudio.Event{Kind: aistudio.EventText, Text: "ab"}},
		{event: usage}, {event: aistudio.Event{Kind: aistudio.EventFinish, FinishReason: "stop"}},
	}}
	service, _ = runningGenerateService(t, single, context.Background(), time.Minute, "alice@example.com")
	_, entry = serveAccessLog(t, service)
	if data := requestLogData(entry); entry.FirstOutput != 0 || data.Usage == nil || data.Usage.GenerationTokensPerSecond != 0 ||
		data.Usage.AverageTokensPerSecond <= 0 {
		t.Fatalf("只有一个分块：first_output=%s usage=%+v", entry.FirstOutput, data.Usage)
	}
}

// postToolChatLogged 经公开 API 发一次强制调用 Kei_Workflow 的非流式 Chat 请求，返回 HTTP 状态与写出的访问日志
func postToolChatLogged(t *testing.T, service *trackedService) (int, api.AccessLog) {
	t.Helper()
	admin := &accessLogCapture{}
	handler := api.NewHandler(service, api.Config{APIKey: "sk-test", Admin: admin})
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(toolChatBody(false)))
	request.Header.Set("Authorization", "Bearer sk-test")
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	admin.mu.Lock()
	defer admin.mu.Unlock()
	if len(admin.entries) != 1 {
		t.Fatalf("访问日志条数 = %d，期望 1", len(admin.entries))
	}
	return recorder.Code, admin.entries[0]
}

// pacedToolBody 为一次发送的流：parts 块正文每隔 interval 到一块，withCall 时最后接函数调用，再以 stop 结束
func pacedToolBody(interval time.Duration, withCall bool, parts ...string) io.ReadCloser {
	chunks := make([]string, 0, len(parts)+1)
	for index, part := range parts {
		prefix := ","
		if index == 0 {
			prefix = "[["
		}
		chunks = append(chunks, prefix+toolTestTextFrame(part))
	}
	tail := "," + toolTestStopFrame + "]]"
	if withCall {
		tail = "," + toolTestCallFrame + tail
	}
	chunks = append(chunks, tail)
	return newPacedBody(interval, chunks...)
}

// TestGenerationSpeedHeldEvents 工具约束要求必须调用：生成服务把正文与思考扣住到调用出现才一起放出（aistudio/tool_guard.go），请求日志的
// 生成速度按扣住第一个上游事件的时刻起算，不按放出的时刻（原先首个分块取放出时刻，扣住期间生成的 token 全算进调用之后的几毫秒，速度被高估
// 几十倍）；同一尝试内重新生成时按交出回复的那次发送扣住的时刻，被放弃的那次不算
func TestGenerationSpeedHeldEvents(t *testing.T) {
	t.Run("扣住到调用出现", func(t *testing.T) {
		transport := newScriptedTransport(func(string, int, string) *aistudio.RPCResponse {
			return playgroundResponse(http.StatusOK, pacedToolBody(200*time.Millisecond, true, "p0", "p1", "p2", "p3", "p4"))
		})
		service := toolRegenerationTestService(t, transport, switchTestIDs(1)...)
		status, entry := postToolChatLogged(t, service)
		if status != http.StatusOK {
			t.Fatalf("status = %d", status)
		}
		if entry.FirstOutput <= 0 || entry.Latency-entry.FirstOutput < 800*time.Millisecond {
			t.Fatalf("首个分块 = %s、总耗时 = %s，期望从扣住第一块正文起算（之后约 1 秒）", entry.FirstOutput, entry.Latency)
		}
	})
	// 重新生成：FIRST_EVENT_TIMEOUT 关闭与开启（重发时首事件计时各走一条路径）都按重新生成那次扣住的时刻
	for _, firstEventTimeout := range []time.Duration{0, 10 * time.Second} {
		t.Run(fmt.Sprintf("重新生成（首事件超时=%s）", firstEventTimeout), func(t *testing.T) {
			transport := newScriptedTransport(func(_ string, call int, _ string) *aistudio.RPCResponse {
				if call == 1 {
					// 第一次只回复文字（违约）：扣住约 1 秒后被放弃
					return playgroundResponse(http.StatusOK, pacedToolBody(200*time.Millisecond, false, "a0", "a1", "a2", "a3", "a4"))
				}
				return playgroundResponse(http.StatusOK, pacedToolBody(300*time.Millisecond, true, "b0", "b1"))
			})
			service := toolRegenerationTestService(t, transport, switchTestIDs(1)...)
			service.firstEventTimeout.Store(int64(firstEventTimeout))
			status, entry := postToolChatLogged(t, service)
			if status != http.StatusOK {
				t.Fatalf("status = %d", status)
			}
			if _, order := transport.sent(); len(order) != 2 {
				t.Fatalf("应在同一账号发送 2 次，实际 %v", order)
			}
			if entry.FirstOutput < 800*time.Millisecond || entry.Latency-entry.FirstOutput < 400*time.Millisecond {
				t.Fatalf("首个分块 = %s、总耗时 = %s，期望从重新生成那次扣住第一块正文起算", entry.FirstOutput, entry.Latency)
			}
		})
	}
}

// TestGenerationSpeedHiddenThinking 思考正文不返回（HideThinking）而用量带思考 token 时不写生成速度：没有返回的思考事件不经转发，
// 首个分块晚于思考开始，按它算会把思考 token 算进很短的时长；不带思考 token 时照常写
func TestGenerationSpeedHiddenThinking(t *testing.T) {
	diag := &generationDiagnostics{hiddenThinking: true}
	if !diag.hiddenThinkingTokens(&aistudio.Usage{OutputTokens: 10, ReasoningTokens: 90}) {
		t.Fatal("不返回思考正文、用量带思考 token 时测不出生成速度")
	}
	if diag.hiddenThinkingTokens(&aistudio.Usage{OutputTokens: 10}) || diag.hiddenThinkingTokens(nil) {
		t.Fatal("没有思考 token 时照常写")
	}
	if (&generationDiagnostics{}).hiddenThinkingTokens(&aistudio.Usage{ReasoningTokens: 90}) || (*generationDiagnostics)(nil).hiddenThinkingTokens(&aistudio.Usage{ReasoningTokens: 90}) {
		t.Fatal("返回思考正文时照常写")
	}
	request := aistudio.GenerateRequest{Config: aistudio.GenerationConfig{HideThinking: true}}
	if !newGenerationDiagnostics(request, "未发送").hiddenThinking {
		t.Fatal("诊断应记下请求不返回思考正文")
	}

	usage := aistudio.Event{Kind: aistudio.EventUsage, Usage: &aistudio.Usage{InputTokens: 10, OutputTokens: 20, ReasoningTokens: 80, TotalTokens: 110}}
	upstream := &speedService{stallingService: newStallingService(), events: []speedEvent{
		{wait: 300 * time.Millisecond, event: aistudio.Event{Kind: aistudio.EventText, Text: "a"}},
		{wait: 100 * time.Millisecond, event: aistudio.Event{Kind: aistudio.EventText, Text: "b"}},
		{event: usage}, {event: aistudio.Event{Kind: aistudio.EventFinish, FinishReason: "stop"}},
	}}
	service, _ := runningGenerateService(t, upstream, context.Background(), time.Minute, "alice@example.com")
	admin := &accessLogCapture{}
	handler := api.NewHandler(service, api.Config{APIKey: "sk-test", Admin: admin})
	body := `{"model":"` + testRuntimeModel + `","max_tokens":64,"thinking":{"type":"disabled"},"messages":[{"role":"user","content":"hi"}]}`
	httpRequest := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	httpRequest.Header.Set("x-api-key", "sk-test")
	httpRequest.Header.Set("anthropic-version", "2023-06-01")
	httpRequest.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httpRequest)
	admin.mu.Lock()
	entries := append([]api.AccessLog(nil), admin.entries...)
	admin.mu.Unlock()
	if recorder.Code != http.StatusOK || len(entries) != 1 {
		t.Fatalf("status = %d，访问日志 %d 条: %s", recorder.Code, len(entries), recorder.Body.String())
	}
	if data := requestLogData(entries[0]); entries[0].FirstOutput != 0 || data.Usage == nil || data.Usage.GenerationTokensPerSecond != 0 ||
		data.Usage.AverageTokensPerSecond <= 0 {
		t.Fatalf("不返回思考正文：first_output=%s usage=%+v，期望不写生成速度、照写平均速度", entries[0].FirstOutput, data.Usage)
	}
}
