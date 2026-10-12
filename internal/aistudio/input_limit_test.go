package aistudio

import (
	"fmt"
	"net/http"
	"testing"
	"time"
)

// TestInputTokenLimitError 按文案识别输入 token 超过上限：400 带计数的写法与部分账号 131072 上限的 429 写法都算，
// 额度文案不算；包装后的错误同样识别
func TestInputTokenLimitError(t *testing.T) {
	for _, test := range []struct {
		message string
		want    bool
	}{
		{"The input token count exceeds the maximum number of tokens allowed 131072", true},
		{"The input token count (2000000) exceeds the maximum number of tokens allowed (1048576).", true},
		{"Input token count exceeds limit", true},
		{"You exceeded your current quota, please check your plan and billing details.", false},
		{"Resource has been exhausted (e.g. check quota).", false},
		{"Too Many Requests", false},
	} {
		err := &RPCError{Method: "GenerateContent", StatusCode: http.StatusTooManyRequests, Code: 8, Message: test.message}
		if got := InputTokenLimitError(err); got != test.want {
			t.Fatalf("%q: InputTokenLimitError = %v，期望 %v", test.message, got, test.want)
		}
		if got := InputTokenLimitError(fmt.Errorf("换号重试: %w", err)); got != test.want {
			t.Fatalf("%q（包装后）: InputTokenLimitError = %v，期望 %v", test.message, got, test.want)
		}
	}
	if InputTokenLimitError(nil) {
		t.Fatal("nil 不应识别为输入超限")
	}
}

// TestInputTokenLimitNotQuota 上游按 429 返回的输入超限不是额度问题：不冷却账号、不换号；普通额度 429 的处理不变
func TestInputTokenLimitNotQuota(t *testing.T) {
	now := time.Now()
	for _, method := range []string{"GenerateContent", "ProxyStreamedCall"} {
		limit := &RPCError{
			Method: method, StatusCode: http.StatusTooManyRequests, Code: 8,
			Message: "The input token count exceeds the maximum number of tokens allowed 131072", RetryDelay: time.Minute,
		}
		if cooldown, ok := QuotaCooldownForError(limit, now); ok {
			t.Fatalf("%s：输入超限不应冷却账号，得到 %+v", method, cooldown)
		}
		if retryableAccountError(limit) {
			t.Fatalf("%s：输入超限不应换号", method)
		}
	}
	quota := &RPCError{Method: "GenerateContent", StatusCode: http.StatusTooManyRequests, Code: 8, Message: "You exceeded your current quota"}
	if _, ok := QuotaCooldownForError(quota, now); !ok {
		t.Fatal("额度 429 仍应冷却账号")
	}
	if !retryableAccountError(quota) {
		t.Fatal("额度 429 仍应换号")
	}
}
