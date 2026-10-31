package app

import (
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"
	"sync/atomic"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// generationPanicError 为生成请求的后台 goroutine（换号重试、转发上游事件、降级判定）里恢复的 panic：
// 转成错误事件结束本请求，对外按 500 与官方措辞返回（INTERNAL），进程不退出
type generationPanicError struct {
	where string
	value any
}

func (e *generationPanicError) Error() string {
	return fmt.Sprintf("%s时发生 panic（已恢复，栈见运行日志）: %v", e.where, e.value)
}

// HTTPStatus 让协议层按 500 返回
func (e *generationPanicError) HTTPStatus() int {
	return http.StatusInternalServerError
}

// recoveredPanic 记录恢复的 panic 并转成错误：请求进度写一行 ERROR（不含栈），运行日志另记带栈的一行。
// 必须在 recover 所在的 defer 里调用，栈才是 panic 发生处的栈
func (service *trackedService) recoveredPanic(requestID string, source string, where string, value any) error {
	err := &generationPanicError{where: where, value: value}
	slog.Error("生成请求后台 panic，已恢复", "request_id", requestID, "where", where, "panic", fmt.Sprint(value), "stack", string(debug.Stack()))
	if service != nil && service.requests != nil {
		summary := strings.Join(strings.Fields(fmt.Sprint(value)), " ")
		if runes := []rune(summary); len(runes) > 300 {
			summary = string(runes[:300]) + "…"
		}
		service.requests.logRequestProgress(requestID, source, "ERROR", fmt.Sprintf("%s时发生 panic，已恢复并结束本请求 | 错误=%s", where, summary))
	}
	return err
}

// leaseAccountLabel 返回租约的账号标签；租约或账号为空时为 "service"（恢复 panic 时用，不再因为取标签本身 panic）
func leaseAccountLabel(lease *aistudio.AccountLease) string {
	if account := lease.Account(); account != nil && account.Config.Label != "" {
		return account.Config.Label
	}
	return "service"
}

// forwardEventsPrologueHook 非空时 forwardEvents 在注册清理之后、前置步骤之前以请求 ID 调用它，只供测试在前置步骤注入 panic
var forwardEventsPrologueHook atomic.Pointer[func(requestID string)]

// startGeneratePanicHook 非空时 startGenerate 在登记请求之后以请求 ID 调用它，只供测试在这一段注入 panic
var startGeneratePanicHook atomic.Pointer[func(requestID string)]
