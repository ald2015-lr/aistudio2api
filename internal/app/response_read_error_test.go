package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// readBrokenBody 为读到一半就失败的错误响应体：先读出 prefix，之后返回 cause
type readBrokenBody struct {
	prefix *strings.Reader
	cause  error
}

func (body *readBrokenBody) Read(buffer []byte) (int, error) {
	if body.prefix.Len() > 0 {
		return body.prefix.Read(buffer)
	}
	return 0, body.cause
}

func (body *readBrokenBody) Close() error { return nil }

func readBrokenResponse(status int) *aistudio.RPCResponse {
	response := playgroundResponse(status, &readBrokenBody{
		prefix: strings.NewReader(`[8,"You exceeded your current quota, please check your plan`), cause: syscall.ECONNRESET,
	})
	response.Header.Set("Retry-After", "3600")
	return response
}

// TestGenerateSwitchesOnResponseReadError 第一个账号返回 429 响应头、读错误体时连接被重置（529043e 的 ResponseReadError）：
// 换号到第二个账号成功。按一般失败计入连续失败上限（换号前退避），不按状态码冷却第一个账号、不当成额度失败。
// 原先读体失败是普通错误，不换号，请求当场以对外 500（官方 INTERNAL 文案）结束
func TestGenerateSwitchesOnResponseReadError(t *testing.T) {
	accounts := switchTestIDs(2)
	var mu sync.Mutex
	var sentAt []time.Time
	transport := newScriptedTransport(func(string, int, string) *aistudio.RPCResponse {
		mu.Lock()
		defer mu.Unlock()
		sentAt = append(sentAt, time.Now())
		if len(sentAt) == 1 {
			return readBrokenResponse(http.StatusTooManyRequests)
		}
		return playgroundResponse(http.StatusOK, io.NopCloser(strings.NewReader(regenTestText)))
	})
	service := regenerationTestService(t, transport, accounts, testRuntimeModel)
	request := aistudio.GenerateRequest{
		ID: "req-read-error", Model: testRuntimeModel,
		Contents: []aistudio.Content{{Role: aistudio.RoleUser, Parts: []aistudio.Part{{Text: "hi"}}}},
	}
	text, finish, err := runRegenerationRequest(t, service, request, newGenerationDiagnostics(request, "未发送"))
	if err != nil || text != "hi" || finish != "stop" {
		t.Fatalf("读错误体中断后应换号成功：text=%q finish=%q err=%v", text, finish, err)
	}
	_, order := transport.sent()
	if len(order) != 2 || order[0] == order[1] {
		t.Fatalf("应换到另一个账号，实际发送顺序 %v", order)
	}
	if status, _ := service.pool.StatusOf(order[0]); len(status.Cooldowns) != 0 {
		t.Fatalf("错误体没读到，不应按 429 冷却：%+v", status.Cooldowns)
	}
	mu.Lock()
	gap := sentAt[1].Sub(sentAt[0])
	mu.Unlock()
	// 计入连续失败上限时第一次换号前退避 200ms（generateRetryBackoff）；额度类失败不退避
	if gap < generateRetryBackoff(1)*3/4 {
		t.Fatalf("两次发送间隔 %s，应按一般失败退避后再换号", gap)
	}
	if !progressLogged(service, "读取 AI Studio GenerateContent 错误响应（HTTP 429）") {
		t.Fatal("请求日志应写明读错误响应体失败与上游状态码")
	}
	if hasLogContaining(service.requests, "账号冷却") {
		t.Fatal("不应写账号冷却")
	}
}

// TestRetryableGenerateResponseReadError ResponseReadError 只在请求未结束、原因不是取消时换号；原因为超时（首事件超时取消本次尝试）
// 时照期限规则换号；请求已取消时不换号
func TestRetryableGenerateResponseReadError(t *testing.T) {
	reset := &aistudio.ResponseReadError{Method: "GenerateContent", StatusCode: http.StatusTooManyRequests, Err: syscall.ECONNRESET}
	if !retryableGenerateAccountError(context.Background(), reset) || !retryableGenerateAccountError(context.Background(), fmt.Errorf("包装: %w", reset)) {
		t.Fatal("请求未结束时读错误体中断应换号")
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if retryableGenerateAccountError(canceled, reset) {
		t.Fatal("请求已取消时不应换号")
	}
	if retryableGenerateAccountError(context.Background(), &aistudio.ResponseReadError{Method: "GenerateContent", StatusCode: 503, Err: context.Canceled}) {
		t.Fatal("读取因取消失败时不应换号")
	}
	timeout := &aistudio.ResponseReadError{Method: "GenerateContent", StatusCode: 503, Err: &firstEventTimeoutError{timeout: time.Second}}
	if !retryableGenerateAccountError(context.Background(), timeout) {
		t.Fatal("读取因首事件超时失败时应照首事件超时换号")
	}
	if inputTokenLimitError(reset) || isPermissionDenied(reset) || aistudio.DefinitiveAuthenticationFailure(reset) {
		t.Fatal("读错误体中断不应按输入超限、403 或登录失效处理")
	}
}

// TestReadAuthenticationFailureReadError 读 401 响应体失败时返回 ResponseReadError（带方法与状态码），不当成登录失效
func TestReadAuthenticationFailureReadError(t *testing.T) {
	_, _, err := readAuthenticationFailure("GenerateContent", readBrokenResponse(http.StatusUnauthorized))
	var readError *aistudio.ResponseReadError
	if !errors.As(err, &readError) || readError.Method != "GenerateContent" || readError.StatusCode != http.StatusUnauthorized ||
		!errors.Is(err, syscall.ECONNRESET) {
		t.Fatalf("应返回 ResponseReadError，得到 %T %v", err, err)
	}
	if aistudio.DefinitiveAuthenticationFailure(err) || !retryableGenerateAccountError(context.Background(), err) {
		t.Fatal("401 的响应体没读到：不判登录失效，按一般失败换号")
	}
}
