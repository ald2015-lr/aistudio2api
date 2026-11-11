package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// warmupService 记录生成调用次数；目录里只有 gemini-2.5-flash
type warmupService struct {
	modelListService
	calls int
}

func (service *warmupService) Generate(context.Context, aistudio.GenerateRequest) (<-chan aistudio.Event, error) {
	service.calls++
	events := make(chan aistudio.Event, 2)
	events <- aistudio.Event{Kind: aistudio.EventText, Text: "full answer"}
	events <- aistudio.Event{Kind: aistudio.EventFinish, FinishReason: "stop"}
	close(events)
	return events, nil
}

// TestAnthropicMaxTokensZeroWarmup max_tokens 为 0 只做预热：不调用生成服务，返回空 content、stop_reason 为 max_tokens、
// usage 带本地估算的 input_tokens 与 0 个输出；流式为 message_start → message_delta → message_stop，没有内容块；
// 模型不在目录里时与生成请求一样返回 404；参数错误照常 400；max_tokens 为 1 时照常生成
func TestAnthropicMaxTokensZeroWarmup(t *testing.T) {
	service := &warmupService{modelListService: modelListService{models: []aistudio.Model{{
		ID: "gemini-2.5-flash", Methods: []string{"generateContent"}, Capabilities: map[string]bool{"chat_model": true},
	}}}}
	handler := NewHandler(service, Config{APIKey: "sk-test"})
	header := map[string]string{"Anthropic-Version": "2023-06-01"}
	body := func(maxTokens int, stream bool, model string) string {
		encoded, _ := json.Marshal(map[string]any{
			"model": model, "max_tokens": maxTokens, "stream": stream, "system": "Cache me",
			"messages": []map[string]any{{"role": "user", "content": "a long prompt to cache"}},
		})
		return string(encoded)
	}

	recorder := postJSON(t, handler, "/v1/messages", body(0, false, "gemini-2.5-flash"), header)
	var message struct {
		Type       string            `json:"type"`
		Model      string            `json:"model"`
		Content    []json.RawMessage `json:"content"`
		StopReason string            `json:"stop_reason"`
		Usage      map[string]int64  `json:"usage"`
	}
	if recorder.Code != http.StatusOK || json.Unmarshal(recorder.Body.Bytes(), &message) != nil {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if message.Type != "message" || message.Model != "gemini-2.5-flash" || message.Content == nil || len(message.Content) != 0 ||
		message.StopReason != "max_tokens" || message.Usage["input_tokens"] <= 0 || message.Usage["output_tokens"] != 0 {
		t.Fatalf("预热响应不对: %s", recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), `"content":[]`) {
		t.Fatalf("content 应为空数组: %s", recorder.Body.String())
	}

	stream := postJSON(t, handler, "/v1/messages", body(0, true, "gemini-2.5-flash"), header)
	var names []string
	for _, line := range strings.Split(stream.Body.String(), "\n") {
		if name, ok := strings.CutPrefix(line, "event: "); ok {
			names = append(names, name)
		}
	}
	if stream.Code != http.StatusOK || strings.Join(names, ",") != "message_start,message_delta,message_stop" ||
		!strings.Contains(stream.Body.String(), `"stop_reason":"max_tokens"`) || !strings.Contains(stream.Body.String(), `"output_tokens":0`) {
		t.Fatalf("流式预热 = %v: %s", names, stream.Body.String())
	}
	if service.calls != 0 {
		t.Fatalf("预热不应调用生成服务，实际 %d 次", service.calls)
	}

	if missing := postJSON(t, handler, "/v1/messages", body(0, false, "no-such-model"), header); missing.Code != http.StatusNotFound {
		t.Fatalf("目录里没有的模型应返回 404: status=%d body=%s", missing.Code, missing.Body.String())
	}
	invalid := postJSON(t, handler, "/v1/messages",
		`{"model":"gemini-2.5-flash","max_tokens":0,"thinking":{"type":"bogus"},"messages":[{"role":"user","content":"hi"}]}`, header)
	if invalid.Code != http.StatusBadRequest || service.calls != 0 {
		t.Fatalf("参数错误应照常 400: status=%d calls=%d", invalid.Code, service.calls)
	}

	normal := postJSON(t, handler, "/v1/messages", body(1, false, "gemini-2.5-flash"), header)
	if normal.Code != http.StatusOK || service.calls != 1 || !strings.Contains(normal.Body.String(), "full answer") {
		t.Fatalf("max_tokens=1 应照常生成: calls=%d body=%s", service.calls, normal.Body.String())
	}
}

// TestAnthropicWarmupSkipsLedger max_tokens 为 0 的预热请求照写请求日志（带模型、标为预热），不进用量账本：访问日志带 Warmup，
// 开启正文记录时也不保存正文；模型不存在的 404 同样标为预热；max_tokens 为正数的请求不带标记，正文照常保存
func TestAnthropicWarmupSkipsLedger(t *testing.T) {
	service := &warmupService{modelListService: modelListService{models: []aistudio.Model{{
		ID: "gemini-2.5-flash", Methods: []string{"generateContent"}, Capabilities: map[string]bool{"chat_model": true},
	}}}}
	admin := &capturingAdmin{}
	ledger := &fakeLedger{capture: true}
	handler := NewHandler(service, Config{APIKey: "sk-test", Admin: admin, Ledger: ledger})
	header := map[string]string{"Anthropic-Version": "2023-06-01"}
	body := func(maxTokens int, stream bool) string {
		encoded, _ := json.Marshal(map[string]any{
			"model": "gemini-2.5-flash", "max_tokens": maxTokens, "stream": stream,
			"messages": []map[string]any{{"role": "user", "content": "a long prompt to cache"}},
		})
		return string(encoded)
	}
	for _, stream := range []bool{false, true} {
		if recorder := postJSON(t, handler, "/v1/messages", body(0, stream), header); recorder.Code != http.StatusOK {
			t.Fatalf("预热 stream=%v status=%d body=%s", stream, recorder.Code, recorder.Body.String())
		}
	}
	missing, _ := json.Marshal(map[string]any{
		"model": "no-such-model", "max_tokens": 0, "messages": []map[string]any{{"role": "user", "content": "hi"}},
	})
	if recorder := postJSON(t, handler, "/v1/messages", string(missing), header); recorder.Code != http.StatusNotFound {
		t.Fatalf("目录里没有的模型应返回 404: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if normal := postJSON(t, handler, "/v1/messages", body(1, false), header); normal.Code != http.StatusOK {
		t.Fatalf("max_tokens=1 status=%d body=%s", normal.Code, normal.Body.String())
	}

	entries := admin.finished()
	if len(entries) != 4 {
		t.Fatalf("请求日志条数=%d，期望 4", len(entries))
	}
	// 模型不存在的预热请求同样标为预热（不进账本），请求日志带模型、按 404 失败记录
	if notFound := entries[2]; !notFound.Warmup || notFound.Model != "no-such-model" || notFound.Status != http.StatusNotFound {
		t.Fatalf("模型不存在的预热请求日志不对: %+v", notFound)
	}
	entries = append(entries[:2], entries[3])
	for index, entry := range entries[:2] {
		if !entry.Warmup || !entry.Authorized || entry.Model != "gemini-2.5-flash" || entry.Account != "" || entry.Status != http.StatusOK {
			t.Fatalf("第 %d 条预热请求日志不对: %+v", index+1, entry)
		}
	}
	if entries[2].Warmup {
		t.Fatalf("max_tokens=1 的请求不应标为预热: %+v", entries[2])
	}
	if len(ledger.bodies) != 1 || ledger.bodies[0].ID != entries[2].RequestID {
		t.Fatalf("只应保存 max_tokens=1 请求的正文，实际 %+v", ledger.bodies)
	}
	if service.calls != 1 {
		t.Fatalf("只有 max_tokens=1 应调用生成服务，实际 %d 次", service.calls)
	}
}
