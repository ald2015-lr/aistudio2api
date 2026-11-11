package api

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// pacedEvent 为按间隔送出的一个事件
type pacedEvent struct {
	delay time.Duration
	event aistudio.Event
}

// pacedService 按预设间隔送出事件：模拟强制调用扣住、重新生成这类长时间没有事件，或隐藏思考期间只有签名、用量事件
type pacedService struct {
	modelListService
	events []pacedEvent
}

func (service *pacedService) Generate(ctx context.Context, _ aistudio.GenerateRequest) (<-chan aistudio.Event, error) {
	out := make(chan aistudio.Event)
	go func() {
		defer close(out)
		for _, item := range service.events {
			select {
			case <-time.After(item.delay):
			case <-ctx.Done():
				return
			}
			select {
			case out <- item.event:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}

// responsesStreamEvents 解析 Responses 流：返回事件名与 sequence_number，以及 SSE 注释心跳的个数
func responsesStreamEvents(t *testing.T, body string) (names []string, sequences []float64, pings int) {
	t.Helper()
	for _, block := range strings.Split(body, "\n\n") {
		for _, line := range strings.Split(block, "\n") {
			switch {
			case strings.HasPrefix(line, ":"):
				pings++
			case strings.HasPrefix(line, "data: "):
				var payload map[string]any
				if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &payload); err != nil {
					t.Fatalf("事件不是 JSON: %q", line)
				}
				names = append(names, payload["type"].(string))
				sequences = append(sequences, payload["sequence_number"].(float64))
				if payload["type"] == "response.in_progress" {
					response := payload["response"].(map[string]any)
					if response["status"] != "in_progress" || response["id"] == "" || response["model"] != "gemini-2.5-flash" {
						t.Fatalf("response.in_progress 应带当前的响应对象: %v", response)
					}
				}
			}
		}
	}
	return names, sequences, pings
}

// countName 返回 names 中 name 出现的次数
func countName(names []string, name string) int {
	count := 0
	for _, value := range names {
		if value == name {
			count++
		}
	}
	return count
}

// TestResponsesProgressDuringSilence Responses 流连续静默满阈值时改发 response.in_progress：没有事件时由心跳发出（平时仍是注释），
// 只有签名、用量这类不写出事件的上游事件时由事件处理发出；序号接着递增；事件不断的流只有开头那一条 response.in_progress
func TestResponsesProgressDuringSilence(t *testing.T) {
	withHeartbeatInterval(t, 20*time.Millisecond)
	responsesProgressAfterOverride.Store(int64(100 * time.Millisecond))
	t.Cleanup(func() { responsesProgressAfterOverride.Store(0) })
	finish := aistudio.Event{Kind: aistudio.EventFinish, FinishReason: "stop"}
	text := func(value string) aistudio.Event { return aistudio.Event{Kind: aistudio.EventText, Text: value} }
	signature := aistudio.Event{Kind: aistudio.EventThoughtSignature, ThoughtSignature: "sig"}
	signatures := []pacedEvent{{event: text("a")}}
	for range 30 {
		signatures = append(signatures, pacedEvent{delay: 10 * time.Millisecond, event: signature})
	}
	for _, test := range []struct {
		name        string
		events      []pacedEvent
		minProgress int
		maxProgress int
		wantPings   bool
	}{
		{name: "没有事件的长静默", events: []pacedEvent{{event: text("a")}, {delay: 450 * time.Millisecond, event: text("b")}, {event: finish}},
			minProgress: 3, maxProgress: 10, wantPings: true},
		{name: "只有签名事件", events: append(signatures, pacedEvent{event: finish}), minProgress: 2, maxProgress: 30},
		{name: "事件不断", events: []pacedEvent{{event: text("a")}, {delay: 5 * time.Millisecond, event: text("b")}, {event: finish}},
			minProgress: 1, maxProgress: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			handler := NewHandler(&pacedService{events: test.events}, Config{APIKey: "sk-test"})
			recorder := postJSON(t, handler, "/v1/responses", `{"model":"gemini-2.5-flash","stream":true,"input":"hi"}`, nil)
			names, sequences, pings := responsesStreamEvents(t, recorder.Body.String())
			if len(names) < 3 || names[0] != "response.created" || names[1] != "response.in_progress" || names[len(names)-1] != "response.completed" {
				t.Fatalf("事件顺序不对: %v", names)
			}
			for index := range sequences {
				if sequences[index] != float64(index) {
					t.Fatalf("sequence_number 应从 0 连续递增: %v", sequences)
				}
			}
			progress := countName(names, "response.in_progress")
			if progress < test.minProgress || progress > test.maxProgress {
				t.Fatalf("response.in_progress 共 %d 条，期望 %d–%d 条: %v", progress, test.minProgress, test.maxProgress, names)
			}
			if test.wantPings && pings == 0 {
				t.Fatalf("静默未满阈值时应仍写注释心跳: %s", recorder.Body.String())
			}
			if strings.Join(names[2:], ",") == "" {
				t.Fatalf("没有输出事件: %v", names)
			}
		})
	}
}
