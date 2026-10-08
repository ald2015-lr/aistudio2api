package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// TestNoEligibleAccountPublicStatus 没有可用账户按原因区分：号池一侧的暂时性原因按 503，全部冷却按 429，
// 只有没有任何账户能处理该请求时才按请求本身的原因返回 4xx
func TestNoEligibleAccountPublicStatus(t *testing.T) {
	busy := aistudio.PoolNotReady("候选账户被其他进程占用", errors.New("runtime 被占用"))
	for _, test := range []struct {
		name   string
		err    error
		model  string
		status int
		rpc    string
	}{
		{name: "账户需要重新登录", err: &aistudio.AccountsNotReadyError{Reasons: []string{"a 需要重新登录"}}, model: "test-model", status: http.StatusServiceUnavailable, rpc: "UNAVAILABLE"},
		{name: "号池为空", err: aistudio.PoolNotReady("账户池中没有账户", nil), model: "test-model", status: http.StatusServiceUnavailable, rpc: "UNAVAILABLE"},
		{name: "账户被占用", err: fmt.Errorf("获取账号: %w", busy), model: "test-model", status: http.StatusServiceUnavailable, rpc: "UNAVAILABLE"},
		{name: "全部冷却", err: &aistudio.AllCoolingError{ModelID: "test-model", Until: time.Now().Add(time.Hour)}, model: "test-model", status: http.StatusTooManyRequests, rpc: "RESOURCE_EXHAUSTED"},
		{name: "没有账户支持该模型", err: fmt.Errorf("%w: 模型不支持", aistudio.ErrNoEligibleAccount), model: "test-model", status: http.StatusNotFound, rpc: "NOT_FOUND"},
		{name: "没有账户能处理且未指明模型", err: aistudio.ErrNoEligibleAccount, status: http.StatusBadRequest, rpc: "INVALID_ARGUMENT"},
	} {
		t.Run(test.name, func(t *testing.T) {
			public := publicErrorFor(test.err, test.model)
			if public.Status != test.status || public.RPC != test.rpc {
				t.Fatalf("得到 %+v，期望 %d %s", public, test.status, test.rpc)
			}
			wantStatus := test.status
			if test.status == http.StatusNotFound {
				// statusFromError 不知道模型名，没有账户支持时按请求错误 400
				wantStatus = http.StatusBadRequest
			}
			if got := statusFromError(test.err); got != wantStatus {
				t.Fatalf("statusFromError = %d，期望 %d", got, wantStatus)
			}
		})
	}
}

// TestRequestErrorRetryAfter 全部冷却且已知最早恢复时间时，三种协议的错误响应都带 Retry-After；其他错误不带
func TestRequestErrorRetryAfter(t *testing.T) {
	cooling := &aistudio.AllCoolingError{ModelID: "test-model", Until: time.Now().Add(90 * time.Second)}
	for name, write := range map[string]func(http.ResponseWriter, error){
		"openai": writeOpenAIRequestError, "gemini": writeGeminiRequestError, "anthropic": writeAnthropicRequestError,
	} {
		recorder := httptest.NewRecorder()
		write(recorder, cooling)
		if recorder.Code != http.StatusTooManyRequests {
			t.Fatalf("%s 状态码 = %d，期望 429", name, recorder.Code)
		}
		seconds, err := strconv.Atoi(recorder.Header().Get("Retry-After"))
		if err != nil || seconds < 89 || seconds > 91 {
			t.Fatalf("%s Retry-After = %q，期望约 90 秒", name, recorder.Header().Get("Retry-After"))
		}
		unavailable := httptest.NewRecorder()
		write(unavailable, &aistudio.AccountsNotReadyError{Reasons: []string{"a 需要重新登录"}})
		if unavailable.Code != http.StatusServiceUnavailable || unavailable.Header().Get("Retry-After") != "" {
			t.Fatalf("%s 号池不可调度：状态码 = %d，Retry-After = %q，期望 503 且不带 Retry-After",
				name, unavailable.Code, unavailable.Header().Get("Retry-After"))
		}
	}
	expired := httptest.NewRecorder()
	writeOpenAIRequestError(expired, &aistudio.AllCoolingError{ModelID: "test-model", Until: time.Now().Add(-time.Second)})
	if expired.Header().Get("Retry-After") != "1" {
		t.Fatalf("恢复时间已过时 Retry-After = %q，期望至少 1 秒", expired.Header().Get("Retry-After"))
	}
}

// TestAccountsNotReadyProtocolShapes 号池暂时不可调度时三种协议都按官方的服务不可用结构返回，不带出账户信息
func TestAccountsNotReadyProtocolShapes(t *testing.T) {
	err := &aistudio.AccountsNotReadyError{Reasons: []string{"alice@example.com 需要重新登录"}}
	openai := httptest.NewRecorder()
	writeOpenAIRequestError(openai, err)
	var openaiBody struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if decodeErr := json.Unmarshal(openai.Body.Bytes(), &openaiBody); decodeErr != nil || openai.Code != http.StatusServiceUnavailable ||
		openaiBody.Error.Type != "server_error" || openaiBody.Error.Message != officialUnavailableMessage {
		t.Fatalf("OpenAI 响应 = %d %s", openai.Code, openai.Body.String())
	}
	gemini := httptest.NewRecorder()
	writeGeminiRequestError(gemini, err)
	var geminiBody struct {
		Error struct {
			Status string `json:"status"`
		} `json:"error"`
	}
	if decodeErr := json.Unmarshal(gemini.Body.Bytes(), &geminiBody); decodeErr != nil || gemini.Code != http.StatusServiceUnavailable ||
		geminiBody.Error.Status != "UNAVAILABLE" {
		t.Fatalf("Gemini 响应 = %d %s", gemini.Code, gemini.Body.String())
	}
	anthropic := httptest.NewRecorder()
	writeAnthropicRequestError(anthropic, err)
	var anthropicBody struct {
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	if decodeErr := json.Unmarshal(anthropic.Body.Bytes(), &anthropicBody); decodeErr != nil || anthropic.Code != http.StatusServiceUnavailable ||
		anthropicBody.Error.Type != "api_error" {
		t.Fatalf("Anthropic 响应 = %d %s", anthropic.Code, anthropic.Body.String())
	}
	for _, body := range []string{openai.Body.String(), gemini.Body.String(), anthropic.Body.String()} {
		if strings.Contains(body, "alice@example.com") {
			t.Fatalf("账户邮箱不能出现在返回给客户端的错误里: %s", body)
		}
	}
}
