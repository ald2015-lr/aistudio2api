package aistudio

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestAccountCoolingDownError 发出前已冷却的错误满足哨兵判断（含 errors.Join 包装），按限流 429 对外，Retry-After 取冷却恢复时间，
// 文案以原哨兵开头并写明冷却类型
func TestAccountCoolingDownError(t *testing.T) {
	until := time.Now().Add(40 * time.Second)
	blocked := &AccountCoolingDownError{
		AccountID: "alice@example.com", ModelID: "flash", Until: until,
		Reason: MinuteQuotaKind + ": AI Studio GenerateContent 返回 HTTP 429",
	}
	var err error = blocked
	if !errors.Is(err, ErrAccountCoolingDown) || !errors.Is(errors.Join(err, io.EOF), ErrAccountCoolingDown) {
		t.Fatal("errors.Is(err, ErrAccountCoolingDown) 应成立")
	}
	if errors.Is(err, ErrAccountLeased) {
		t.Fatal("不应匹配其他哨兵")
	}
	var typed *AccountCoolingDownError
	if !errors.As(errors.Join(io.EOF, err), &typed) || typed != blocked {
		t.Fatal("errors.As 应取得带类型的错误")
	}
	if !blocked.RetryAt().Equal(until) || blocked.HTTPStatus() != http.StatusTooManyRequests || blocked.ErrorCode() != "rate_limit_exceeded" {
		t.Fatalf("RetryAt=%s HTTPStatus=%d ErrorCode=%s", blocked.RetryAt(), blocked.HTTPStatus(), blocked.ErrorCode())
	}
	message := blocked.Error()
	if !strings.HasPrefix(message, "账户已进入冷却") || !strings.Contains(message, MinuteQuotaKind) || !strings.Contains(message, "请求没有发到上游") {
		t.Fatalf("Error()=%q", message)
	}
	// 没有冷却原因时只写恢复时间
	if message := (&AccountCoolingDownError{Until: until}).Error(); !strings.HasPrefix(message, "账户已进入冷却（恢复时间 ") {
		t.Fatalf("没有原因时 Error()=%q", message)
	}
}
