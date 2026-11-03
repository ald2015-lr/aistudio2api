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
		// 不固定账户的生成选号没有候选：模型在列按 503，不在号池的模型列表里（权益、通道接不了）按 404
		{name: "模型在列但没有账户能接", err: &aistudio.NoServingAccountError{Model: "test-model", Summary: "可用通道=playground", Listed: true}, model: "test-model",
			status: http.StatusServiceUnavailable, rpc: "UNAVAILABLE"},
		{name: "模型不在号池的模型列表里", err: &aistudio.NoServingAccountError{Model: "test-model", Summary: "可用通道=build", Reason: "x"}, model: "test-model",
			status: http.StatusNotFound, rpc: "NOT_FOUND"},
	} {
		t.Run(test.name, func(t *testing.T) {
			public := publicErrorFor(test.err, test.model)
			if public.Status != test.status || public.RPC != test.rpc {
				t.Fatalf("得到 %+v，期望 %d %s", public, test.status, test.rpc)
			}
			wantStatus := test.status
			var noServing *aistudio.NoServingAccountError
			if test.status == http.StatusNotFound && !errors.As(test.err, &noServing) {
				// statusFromError 不知道模型名，没有账户支持时按请求错误 400
				wantStatus = http.StatusBadRequest
			}
			if got := statusFromError(test.err); got != wantStatus {
				t.Fatalf("statusFromError = %d，期望 %d", got, wantStatus)
			}
		})
	}
}

// TestRequestErrorRetryAfter 全部冷却且已知最早恢复时间时，三种协议的错误响应都带 Retry-After（较长的间隔带随机抖动）；其他错误不带
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
		// 90 秒的建议间隔另加 0–3 秒的随机抖动（见 retryAfterSeconds）
		seconds, err := strconv.Atoi(recorder.Header().Get("Retry-After"))
		if err != nil || seconds < 89 || seconds > 94 {
			t.Fatalf("%s Retry-After = %q，期望约 90 秒（含 0–3 秒抖动）", name, recorder.Header().Get("Retry-After"))
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

// TestNoServingAccountProtocolShapes 不固定账户的生成选号没有候选时三种协议的对外形状：模型在列为 503 UNAVAILABLE 与写明模型在列的英文说明
// （OpenAI code 为 model_unavailable），带 Retry-After；模型不在号池的模型列表里为 404 NOT_FOUND 与写明原因类别的英文说明（OpenAI code 为
// model_not_found），不带 Retry-After、不说稍后重试。都不带出内部核对摘要与账户
func TestNoServingAccountProtocolShapes(t *testing.T) {
	now := time.Now()
	listed := aistudio.NewNoServingAccountError(aistudio.NoCandidateReport{
		Model: "gemini-omni-1.1-flash", ModelListed: true, Listed: 3, Servable: 3, Channels: []string{"playground"},
	}, now)
	unlisted := aistudio.NewNoServingAccountError(aistudio.NoCandidateReport{
		Model: "gemini-omni-1.1-flash", Listed: 3, Unsupported: 3, Channels: []string{"build"}, EnabledChannels: []string{"build"},
	}, now)
	for _, test := range []struct {
		name   string
		err    *aistudio.NoServingAccountError
		status int
		want   string
		code   string
		retry  bool
	}{
		{name: "模型在列", err: listed, status: http.StatusServiceUnavailable, code: "model_unavailable", retry: true,
			want: "Model gemini-omni-1.1-flash is listed, but no account in the pool can serve this request right now. Please try again later."},
		{name: "模型不在列", err: unlisted, status: http.StatusNotFound, code: "model_not_found",
			want: "models/gemini-omni-1.1-flash is not available to the accounts in this pool: the enabled upstream channels (build) cannot serve this model. " +
				"Call ListModels to see the list of available models."},
	} {
		if !strings.Contains(test.err.Summary, "目录列出=3") {
			t.Fatalf("%s: 摘要 %q", test.name, test.err.Summary)
		}
		for name, write := range map[string]func(http.ResponseWriter, error){
			"openai": writeOpenAIRequestError, "gemini": writeGeminiRequestError, "anthropic": writeAnthropicRequestError,
		} {
			recorder := httptest.NewRecorder()
			write(recorder, test.err)
			body := recorder.Body.String()
			if recorder.Code != test.status || !strings.Contains(body, test.want) || strings.Contains(body, "目录列出") || strings.Contains(body, "可用通道") {
				t.Fatalf("%s %s 响应 = %d %s", test.name, name, recorder.Code, body)
			}
			if retry := recorder.Header().Get("Retry-After"); (retry != "") != test.retry {
				t.Fatalf("%s %s Retry-After = %q", test.name, name, retry)
			}
			if !test.retry && strings.Contains(body, "try again later") {
				t.Fatalf("%s %s 长期状态不应说稍后重试: %s", test.name, name, body)
			}
			if name == "openai" && !strings.Contains(body, `"code":"`+test.code+`"`) {
				t.Fatalf("%s OpenAI code 应为 %s: %s", test.name, test.code, body)
			}
			if name == "gemini" && !strings.Contains(body, `"status":"`+test.err.RPCStatus()+`"`) {
				t.Fatalf("%s Gemini status 应为 %s: %s", test.name, test.err.RPCStatus(), body)
			}
		}
		if !errors.Is(test.err, aistudio.ErrNoEligibleAccount) || openAIErrorCode(test.err) != test.code || statusFromError(test.err) != test.status {
			t.Fatalf("%s: 应保留无账户哨兵，OpenAI 错误码 %q，状态 %d", test.name, openAIErrorCode(test.err), statusFromError(test.err))
		}
	}
}
