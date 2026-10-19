package app

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// finishMessageService 是测试用的上游：返回一段正文，以带 finishMessage 的终止事件结束
type finishMessageService struct {
	*stallingService
	reason  string
	message string
}

func (service *finishMessageService) Generate(_ context.Context, request aistudio.GenerateRequest) (<-chan aistudio.Event, error) {
	service.mu.Lock()
	service.calls = append(service.calls, request.AccountID)
	service.mu.Unlock()
	events := make(chan aistudio.Event, 2)
	events <- aistudio.Event{Kind: aistudio.EventText, Text: "hello", ProviderModel: testRuntimeModel}
	events <- aistudio.Event{Kind: aistudio.EventFinish, FinishReason: service.reason, FinishMessage: service.message, ProviderModel: testRuntimeModel}
	close(events)
	return events, nil
}

// TestFinishMessageWrittenToRequestLog 上游的 finishMessage 写进这次请求的管理日志：非正常结束为 WARN
func TestFinishMessageWrittenToRequestLog(t *testing.T) {
	upstream := &finishMessageService{
		stallingService: newStallingService(), reason: "malformed_function_call",
		message: "Malformed function call: print(default_api.lookup(id=))",
	}
	service, pool := firstEventTestService(t, upstream, 0, "alice@example.com")
	if text, err := runFirstEventRequest(t, service); err != nil || text != "hello" {
		t.Fatalf("text=%q err=%v", text, err)
	}
	lines := queueProgressLines(service, "req-first-event", "上游结束说明")
	if len(lines) != 1 {
		t.Fatalf("上游结束说明日志 = %+v，期望 1 条", lines)
	}
	if lines[0].Level != "WARN" || !strings.Contains(lines[0].Message, "结束原因=malformed_function_call") ||
		!strings.Contains(lines[0].Message, "说明=Malformed function call: print(default_api.lookup(id=))") {
		t.Fatalf("日志 = %+v", lines[0])
	}
	assertLeasesReleased(t, pool)

	// 上游没有给出说明时不写这条日志
	silent := &finishMessageService{stallingService: newStallingService(), reason: "stop"}
	service, _ = firstEventTestService(t, silent, 0, "alice@example.com")
	if _, err := runFirstEventRequest(t, service); err != nil {
		t.Fatal(err)
	}
	if lines := queueProgressLines(service, "req-first-event", "上游结束说明"); len(lines) != 0 {
		t.Fatalf("没有说明时不应写日志: %+v", lines)
	}
}

// TestUpstreamFinishLog 正常结束为 INFO、其余为 WARN；过长的说明按字数截断
func TestUpstreamFinishLog(t *testing.T) {
	if level, _ := upstreamFinishLog("stop", "done"); level != "INFO" {
		t.Fatalf("stop 级别 = %s", level)
	}
	if level, _ := upstreamFinishLog("safety", "blocked"); level != "WARN" {
		t.Fatalf("safety 级别 = %s", level)
	}
	_, message := upstreamFinishLog("malformed_function_call", strings.Repeat("调", upstreamFinishMessageLimit+10))
	if !strings.HasSuffix(message, "…") || utf8.RuneCountInString(message) > upstreamFinishMessageLimit+64 {
		t.Fatalf("过长的说明没有截断: %d 字", utf8.RuneCountInString(message))
	}
}
