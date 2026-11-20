package api

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// 对外 429、503 带重试时间时消息末尾另起一句 " Please try again in Ns."（上游 Mag1cFall/AIStudio2API cd3d4ab 1F-B4）：
// 秒数与 Retry-After 相同；流内错误事件（没有响应头）同样带上

// retryHintSeconds 取消息末尾重试提示里的秒数，没有时为 -1
func retryHintSeconds(message string) int {
	match := regexp.MustCompile(` Please try again in ([0-9]+)s\.$`).FindStringSubmatch(message)
	if match == nil {
		return -1
	}
	seconds, _ := strconv.Atoi(match[1])
	return seconds
}

// TestRetryHintMatchesRetryAfter 非流式与写出响应头前失败的流式请求：429 消息末尾的秒数与 Retry-After 完全相同（抖动只算一次）
func TestRetryHintMatchesRetryAfter(t *testing.T) {
	anthropic := map[string]string{"Anthropic-Version": "2023-06-01"}
	for _, test := range []struct {
		name   string
		path   string
		body   string
		header map[string]string
		field  string
	}{
		{name: "Chat", path: "/v1/chat/completions", body: `{"model":"test-model","messages":[{"role":"user","content":"hi"}]}`},
		{name: "Responses 流式", path: "/v1/responses", body: `{"model":"test-model","stream":true,"input":"hi"}`},
		{name: "Anthropic", path: "/v1/messages", body: `{"model":"test-model","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`, header: anthropic},
		{name: "Gemini", path: "/v1beta/models/test-model:generateContent", body: `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`},
		{name: "Interactions 流式", path: "/v1beta/interactions", body: `{"model":"test-model","stream":true,"input":"hi"}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			// 20 秒以上才有 0–3 秒的随机抖动（见 retryAfterSeconds）：多次请求确认消息与响应头取的是同一次计算
			cooling := &aistudio.AllCoolingError{ModelID: "test-model", Until: time.Now().Add(40 * time.Second)}
			handler := NewHandler(&scriptedService{events: []aistudio.Event{{Kind: aistudio.EventError, Err: cooling}}}, Config{APIKey: "sk-test"})
			for range 12 {
				recorder := postJSON(t, handler, test.path, test.body, test.header)
				header, err := strconv.Atoi(recorder.Header().Get("Retry-After"))
				if recorder.Code != http.StatusTooManyRequests || err != nil {
					t.Fatalf("status=%d Retry-After=%q", recorder.Code, recorder.Header().Get("Retry-After"))
				}
				raw := recorder.Body.String()
				match := regexp.MustCompile(`Resource has been exhausted \(e\.g\. check quota\)\. Please try again in ([0-9]+)s\.`).FindStringSubmatch(raw)
				if match == nil {
					t.Fatalf("消息应带重试提示: %s", raw)
				}
				if seconds, _ := strconv.Atoi(match[1]); seconds != header {
					t.Fatalf("消息里的 %d 秒与 Retry-After %d 秒不同", seconds, header)
				}
			}
		})
	}
}

// TestRetryHintInStreamErrors 写出响应头之后的流内错误（已收到正文）：OpenAI、Responses、Anthropic、Gemini、Interactions 的错误事件
// 消息末尾都带重试提示（秒数约为距恢复的时间）
func TestRetryHintInStreamErrors(t *testing.T) {
	anthropic := map[string]string{"Anthropic-Version": "2023-06-01"}
	for _, test := range []struct {
		name   string
		path   string
		body   string
		header map[string]string
	}{
		{name: "Chat", path: "/v1/chat/completions", body: `{"model":"test-model","stream":true,"messages":[{"role":"user","content":"hi"}]}`},
		{name: "Responses", path: "/v1/responses", body: `{"model":"test-model","stream":true,"input":"hi"}`},
		{name: "Anthropic", path: "/v1/messages", body: `{"model":"test-model","stream":true,"max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`, header: anthropic},
		{name: "Gemini", path: "/v1beta/models/test-model:streamGenerateContent?alt=sse", body: `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`},
		{name: "Interactions", path: "/v1beta/interactions", body: `{"model":"test-model","stream":true,"input":"hi"}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			cooling := &aistudio.AllCoolingError{ModelID: "test-model", Until: time.Now().Add(9 * time.Second)}
			events := []aistudio.Event{{Kind: aistudio.EventText, Text: "partial"}, {Kind: aistudio.EventError, Err: cooling}}
			handler := NewHandler(&scriptedService{events: events}, Config{APIKey: "sk-test"})
			recorder := postJSON(t, handler, test.path, test.body, test.header)
			raw := recorder.Body.String()
			if recorder.Code != http.StatusOK || !strings.Contains(raw, "partial") {
				t.Fatalf("应先写出正文再在流内报错：status=%d body=%s", recorder.Code, raw)
			}
			match := regexp.MustCompile(`Please try again in ([0-9]+)s\.`).FindStringSubmatch(raw)
			if match == nil {
				t.Fatalf("流内错误应带重试提示: %s", raw)
			}
			if seconds, _ := strconv.Atoi(match[1]); seconds < 9 || seconds > 10 {
				t.Fatalf("流内错误的重试提示为 %d 秒，期望约 9 秒", seconds)
			}
		})
	}
}

// TestRetryHintOnlyWithRetryTime 没有最早恢复时间的 429（上游限流原文）、503 与 4xx 不带重试提示；带提示的消息不含 new-api 的自动禁用关键词
func TestRetryHintOnlyWithRetryTime(t *testing.T) {
	upstream := &aistudio.RPCError{Method: "GenerateContent", StatusCode: http.StatusTooManyRequests, Code: 8, Message: "You exceeded your current quota"}
	for _, err := range []error{upstream, aistudio.PoolNotReady("候选账户被其他进程占用", nil), aistudio.ErrInvalidArgument, errors.New("内部错误")} {
		public, seconds := publicErrorWithRetry(err, "test-model")
		if seconds != 0 || retryHintSeconds(public.Message) >= 0 {
			t.Fatalf("%v：不应带重试提示，得到 %q（%d 秒）", err, public.Message, seconds)
		}
	}
	// 错误链里有恢复时间、对外却不是 429 或 503（输入被拦截的 400 在前）：不带提示，与 Retry-After 只写在 429、503 上一致
	blocked := errors.Join(&aistudio.PromptFeedbackError{Reason: "SAFETY"}, &aistudio.AllCoolingError{ModelID: "test-model", Until: time.Now().Add(time.Minute)})
	if public, seconds := publicErrorWithRetry(blocked, "test-model"); public.Status != http.StatusBadRequest || seconds != 0 || retryHintSeconds(public.Message) >= 0 {
		t.Fatalf("400 不应带重试提示：%+v（%d 秒）", public, seconds)
	}
	public, seconds := publicErrorWithRetry(&aistudio.AllCoolingError{ModelID: "test-model", Until: time.Now().Add(2500 * time.Millisecond)}, "test-model")
	if seconds != 3 || public.Message != officialExhaustedMessage+" Please try again in 3s." {
		t.Fatalf("2.5 秒后恢复应向上取整为 3 秒：%q（%d）", public.Message, seconds)
	}
	for _, keyword := range []string{"You exceeded your current quota", "Your credit balance is too low", "Permission denied",
		"This organization has been disabled", "Operation not allowed", "Your account is not authorized", "billing"} {
		if strings.Contains(strings.ToLower(public.Message), strings.ToLower(keyword)) {
			t.Fatalf("重试提示不应含 new-api 的自动禁用关键词 %q: %s", keyword, public.Message)
		}
	}
	if got := retryHintMessage("No trailing period", 5); got != "No trailing period. Please try again in 5s." {
		t.Fatalf("没有句号的消息 = %q", got)
	}
	recorder := httptest.NewRecorder()
	setRetryAfter(recorder, 0)
	if recorder.Header().Get("Retry-After") != "" {
		t.Fatal("没有秒数时不应写 Retry-After")
	}
}

// TestRetryHintLiveErrors Live（WebSocket）的 error 消息同样带重试提示：建立会话失败（bidiErrorMessage）与会话中的错误事件（mapBidiServerEvent）
func TestRetryHintLiveErrors(t *testing.T) {
	cooling := &aistudio.AllCoolingError{ModelID: "test-model", Until: time.Now().Add(4 * time.Second)}
	for name, message := range map[string]string{
		"建立会话失败": bidiErrorMessage(cooling).Error,
		"会话中的错误": mapBidiServerEvent(aistudio.BidiEvent{Kind: aistudio.BidiEventError, Err: cooling}).Error,
	} {
		if seconds := retryHintSeconds(message); seconds < 4 || seconds > 5 {
			t.Fatalf("%s：消息 %q 应带约 4 秒的重试提示", name, message)
		}
	}
}

// TestRetryHint503 带恢复时间的 503（模型在列、暂时没有账户能接）：消息保留原文，末尾另带与 Retry-After 相同秒数的重试提示
func TestRetryHint503(t *testing.T) {
	listed := aistudio.NewNoServingAccountError(aistudio.NoCandidateReport{
		Model: "gemini-test-model", ModelListed: true, Listed: 2, Servable: 2, Channels: []string{"playground"},
	}, time.Now())
	for name, write := range map[string]func(http.ResponseWriter, error){
		"openai": writeOpenAIRequestError, "gemini": writeGeminiRequestError, "anthropic": writeAnthropicRequestError,
	} {
		recorder := httptest.NewRecorder()
		write(recorder, listed)
		header, err := strconv.Atoi(recorder.Header().Get("Retry-After"))
		if recorder.Code != http.StatusServiceUnavailable || err != nil {
			t.Fatalf("%s：status=%d Retry-After=%q", name, recorder.Code, recorder.Header().Get("Retry-After"))
		}
		want := "Model gemini-test-model is listed, but no account in the pool can serve this request right now. Please try again later." +
			" Please try again in " + strconv.Itoa(header) + "s."
		if body := recorder.Body.String(); !strings.Contains(body, want) {
			t.Fatalf("%s：消息应为原文加重试提示 %q：%s", name, want, body)
		}
	}
}
