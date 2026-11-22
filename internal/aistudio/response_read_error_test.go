package aistudio

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"syscall"
	"testing"
	"time"
)

// brokenErrorBody 为读到一半就失败的错误响应体：先读出 prefix，之后返回 cause（连接被重置等）
type brokenErrorBody struct {
	prefix *strings.Reader
	cause  error
}

func (body *brokenErrorBody) Read(buffer []byte) (int, error) {
	if body.prefix.Len() > 0 {
		return body.prefix.Read(buffer)
	}
	return 0, body.cause
}

func (body *brokenErrorBody) Close() error { return nil }

func brokenErrorResponse(status int, cause error) *RPCResponse {
	return &RPCResponse{
		StatusCode: status, Header: http.Header{"Retry-After": {"60"}},
		Body: &brokenErrorBody{prefix: strings.NewReader(`[8,"You exceeded your cur`), cause: cause},
	}
}

// TestValidateRPCResponseReadError 读非 200 错误响应体时连接中断：返回带方法、状态码与原因的 ResponseReadError（原先是普通错误），
// 不是 RPCError，不据状态码冷却、不判登录失效；数据接口按可换号处理，原因是取消或超时时不换号
func TestValidateRPCResponseReadError(t *testing.T) {
	_, err := validateRPCResponse("CountTokens", brokenErrorResponse(http.StatusTooManyRequests, syscall.ECONNRESET))
	var readError *ResponseReadError
	if !errors.As(err, &readError) || readError.Method != "CountTokens" || readError.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("应返回 ResponseReadError（方法与状态码），得到 %T %v", err, err)
	}
	if !errors.Is(err, syscall.ECONNRESET) || !strings.Contains(err.Error(), "HTTP 429") || !strings.Contains(err.Error(), "CountTokens") {
		t.Fatalf("错误应能识别读取原因，文本写明方法与状态码：%v", err)
	}
	var rpcError *RPCError
	if errors.As(err, &rpcError) {
		t.Fatal("错误体没读到，不应当成 RPCError")
	}
	if cooldown, ok := QuotaCooldownForError(err, time.Now()); ok {
		t.Fatalf("状态码只写进日志，不应据此冷却：%+v", cooldown)
	}
	if !retryableAccountError(err) || !retryableAccountError(fmt.Errorf("包装: %w", err)) {
		t.Fatal("读错误响应体中断应换号")
	}
	_, unauthorized := validateRPCResponse("ListModels", brokenErrorResponse(http.StatusUnauthorized, io.ErrUnexpectedEOF))
	if DefinitiveAuthenticationFailure(unauthorized) {
		t.Fatal("401 的错误体没读到，不应判为登录失效")
	}
	for _, cause := range []error{context.Canceled, context.DeadlineExceeded} {
		_, err := validateRPCResponse("CountTokens", brokenErrorResponse(http.StatusServiceUnavailable, cause))
		if !errors.As(err, &readError) || retryableAccountError(err) {
			t.Fatalf("原因为 %v 时不应换号：%v", cause, err)
		}
	}
}

// readErrorTransport 对每个普通 RPC 返回读到一半就断开的 503 错误响应
type readErrorTransport struct{}

func (readErrorTransport) Do(context.Context, RPCRequest) (*RPCResponse, error) {
	return brokenErrorResponse(http.StatusServiceUnavailable, syscall.ECONNRESET), nil
}

// TestCountTokensSwitchesOnResponseReadError 数据接口（CountTokens）读错误响应体中断时换号：3 个账户都中断时 3 个都试过，
// 按最后一次的 ResponseReadError 结束；原先当场失败、只试 1 个
func TestCountTokensSwitchesOnResponseReadError(t *testing.T) {
	service := ceilingService(t, 3)
	client, err := NewClient(ClientOptions{Transport: readErrorTransport{}, Protected: ceilingProtected{}})
	if err != nil {
		t.Fatal(err)
	}
	client.catalogs = service.client.catalogs
	service.client = client
	attempts, err := runCeilingPath(t, service, context.Background(), "CountTokens")
	var readError *ResponseReadError
	if attempts != 3 || !errors.As(err, &readError) {
		t.Fatalf("尝试 %d 个账户 err=%v，期望 3 个都试过后按 ResponseReadError 结束", attempts, err)
	}
}
