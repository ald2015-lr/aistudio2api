package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// TestUpstreamTimeoutProtocolShapes 上游超时（含每次尝试的首事件超时，满足 DeadlineExceeded）三种协议都按官方的 504 结构返回
func TestUpstreamTimeoutProtocolShapes(t *testing.T) {
	err := fmt.Errorf("上游首事件超时: %w", context.DeadlineExceeded)
	for _, test := range []struct {
		name  string
		write func(http.ResponseWriter, error)
		shape string
	}{
		{name: "openai", write: writeOpenAIRequestError, shape: `"server_error"`},
		{name: "gemini", write: writeGeminiRequestError, shape: `"DEADLINE_EXCEEDED"`},
		{name: "anthropic", write: writeAnthropicRequestError, shape: `"api_error"`},
	} {
		recorder := httptest.NewRecorder()
		test.write(recorder, err)
		body := recorder.Body.String()
		if recorder.Code != http.StatusGatewayTimeout || !strings.Contains(body, officialDeadlineMessage) || !strings.Contains(body, test.shape) {
			t.Fatalf("%s 响应 = %d %s，期望 504 与 %s", test.name, recorder.Code, body, test.shape)
		}
	}
}

// serviceDeadlineTestError 模拟生成服务自己的期限到期（app 层的 serviceDeadlineError）：只解包为 DeadlineExceeded，内部文本带“本服务”
type serviceDeadlineTestError struct{}

func (serviceDeadlineTestError) Error() string {
	return "本服务首事件后空闲期限 5m0s 到期（已输出 推理=3 正文=1）: context deadline exceeded"
}

func (serviceDeadlineTestError) Unwrap() error { return context.DeadlineExceeded }

// deadlineFrames 返回响应体里带官方 504 文案的行（流内的错误帧）
func deadlineFrames(body string) []string {
	var frames []string
	for _, line := range strings.Split(body, "\n") {
		if strings.Contains(line, officialDeadlineMessage) {
			frames = append(frames, line)
		}
	}
	return frames
}

// TestServiceDeadlinePublicShape 本服务自己的期限到期（首事件前、首事件后空闲、总时长上限）对外与普通的 DeadlineExceeded 完全相同：
// 流开始前为 HTTP 504 与官方文案，开始推流后流内错误帧相同，响应里没有内部文本；访问日志记 504、不是取消，error 字段为内部文本
func TestServiceDeadlinePublicShape(t *testing.T) {
	internal := serviceDeadlineTestError{}
	plain := fmt.Errorf("%w", context.DeadlineExceeded)
	anthropicHeader := map[string]string{"Anthropic-Version": "2023-06-01"}
	for _, test := range []struct {
		name   string
		stream string
		unary  string
		body   func(stream bool) string
		header map[string]string
	}{
		{
			name: "openai", stream: "/v1/chat/completions", unary: "/v1/chat/completions",
			body: func(stream bool) string {
				return fmt.Sprintf(`{"model":"gemini-2.5-flash","stream":%t,"messages":[{"role":"user","content":"hi"}]}`, stream)
			},
		},
		{
			name: "anthropic", stream: "/v1/messages", unary: "/v1/messages", header: anthropicHeader,
			body: func(stream bool) string {
				return fmt.Sprintf(`{"model":"gemini-2.5-flash","stream":%t,"max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`, stream)
			},
		},
		{
			name: "gemini", stream: "/v1beta/models/gemini-2.5-flash:streamGenerateContent?alt=sse", unary: "/v1beta/models/gemini-2.5-flash:generateContent",
			body: func(bool) string { return `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}` },
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			run := func(path string, stream bool, events []aistudio.Event) (*httptest.ResponseRecorder, AccessLog) {
				admin := &capturingAdmin{}
				handler := NewHandler(&scriptedService{events: events}, Config{APIKey: "sk-test", Admin: admin})
				recorder := postJSON(t, handler, path, test.body(stream), test.header)
				entries := admin.finished()
				if len(entries) != 1 {
					t.Fatalf("访问日志条数 = %d，期望 1", len(entries))
				}
				return recorder, entries[0]
			}
			failure := []aistudio.Event{{Kind: aistudio.EventError, Err: internal}}
			for _, request := range []struct {
				path   string
				stream bool
			}{{test.stream, true}, {test.unary, false}} {
				recorder, entry := run(request.path, request.stream, failure)
				body := recorder.Body.String()
				if recorder.Code != http.StatusGatewayTimeout || !strings.Contains(body, officialDeadlineMessage) || strings.Contains(body, "本服务") {
					t.Fatalf("流式=%v 流开始前：%d %s，期望 HTTP 504、官方文案、不含内部文本", request.stream, recorder.Code, body)
				}
				if entry.Status != http.StatusGatewayTimeout || entry.Canceled || entry.Error != internal.Error() {
					t.Fatalf("流式=%v 访问日志 status=%d canceled=%v error=%q", request.stream, entry.Status, entry.Canceled, entry.Error)
				}
			}
			started := func(err error) []aistudio.Event {
				return []aistudio.Event{{Kind: aistudio.EventText, Text: "hello"}, {Kind: aistudio.EventError, Err: err}}
			}
			recorder, entry := run(test.stream, true, started(internal))
			reference, _ := run(test.stream, true, started(plain))
			body := recorder.Body.String()
			if recorder.Code != http.StatusOK || strings.Contains(body, "本服务") {
				t.Fatalf("开始推流后：%d %s，期望流内错误、不含内部文本", recorder.Code, body)
			}
			frames, want := deadlineFrames(body), deadlineFrames(reference.Body.String())
			if len(frames) == 0 || strings.Join(frames, "\n") != strings.Join(want, "\n") {
				t.Fatalf("流内错误帧 = %q，期望与普通 DeadlineExceeded 相同 %q", frames, want)
			}
			if entry.Status != http.StatusGatewayTimeout || entry.Canceled || entry.Error != internal.Error() {
				t.Fatalf("开始推流后访问日志 status=%d canceled=%v error=%q", entry.Status, entry.Canceled, entry.Error)
			}
		})
	}
}
