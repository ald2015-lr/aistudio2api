package api

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// panicError 为处理请求时恢复的 panic：对外按 500 与官方措辞返回（INTERNAL），管理日志记下在哪一步与 panic 的值，栈写进运行日志
type panicError struct {
	where string
	value any
}

func (e *panicError) Error() string {
	return fmt.Sprintf("%s时发生 panic（已恢复，栈见运行日志）: %v", e.where, e.value)
}

// HTTPStatus 让 statusFromError 与 publicErrorFor 按 500 处理
func (e *panicError) HTTPStatus() int {
	return http.StatusInternalServerError
}

// recoveredPanic 把恢复的 panic 写进运行日志（带请求 ID 与栈）并转成错误。必须在 recover 所在的 defer 里调用，栈才是 panic 发生处的栈
func recoveredPanic(ctx context.Context, where string, value any) error {
	requestID := ""
	if metadata, ok := ctx.Value(accessLogContextKey{}).(*accessLogMetadata); ok {
		requestID = metadata.id()
	}
	slog.Error("请求处理 panic，已恢复", "request_id", requestID, "where", where, "panic", fmt.Sprint(value), "stack", string(debug.Stack()))
	return &panicError{where: where, value: value}
}

// serveRecovering 调用处理函数并恢复其中的 panic：写运行日志与请求日志（状态 500），响应头还没提交时按请求的协议返回官方结构的 500。
// 响应头已提交时返回 aborted：调用方写完请求日志后以 http.ErrAbortHandler 交给 net/http 中止连接（HTTP/1.1 不写 chunked 结尾，
// HTTP/2 发 RST_STREAM），客户端照旧读到意外断开、知道回复不完整可以重试。不能正常收尾：Gemini SSE 没有结束标记，
// 正常结束的截断流会被 SDK 当成完整回复，JSON 数组模式还会被收尾的 "]" 补成合法而残缺的 JSON。
// 处理函数自己以 http.ErrAbortHandler 中止连接时同样返回 aborted
func serveRecovering(next http.Handler, writer *accessLogResponseWriter, r *http.Request) (aborted bool) {
	defer func() {
		value := recover()
		if value == nil {
			return
		}
		if value == http.ErrAbortHandler {
			aborted = true
			return
		}
		err := recoveredPanic(r.Context(), "处理请求 "+r.Method+" "+r.URL.Path, value)
		writer.metadata.setPanic(err)
		if writer.committed() {
			aborted = true
			return
		}
		writePanicResponse(writer, r, err)
	}()
	next.ServeHTTP(writer, r)
	return false
}

// writePanicResponse 按请求的协议写出 500 官方错误结构（消息为官方的内部错误措辞）
func writePanicResponse(w http.ResponseWriter, r *http.Request, err error) {
	switch protocolForRequest(r) {
	case "gemini":
		writeGeminiRequestError(w, err)
	case "anthropic":
		writeAnthropicRequestError(w, err)
	case "admin":
		writeAdminError(w, http.StatusInternalServerError, "internal_error", "Internal error")
	default:
		writeOpenAIRequestError(w, err)
	}
}

// panicSummary 为管理日志里 panic 的一行说明：去掉换行，过长时截断
func panicSummary(err error) string {
	text := strings.Join(strings.Fields(err.Error()), " ")
	if runes := []rune(text); len(runes) > 500 {
		text = string(runes[:500]) + "…"
	}
	return text
}

// recoverEventFilter 恢复协议层事件过滤 goroutine 里的 panic：带栈写进运行日志，往 out 写一个错误事件结束本请求（协议层按 500 返回），
// 并在后台读完上游让生产方正常结束，不让进程退出。必须直接作为 defer 调用，且在 close(out) 之后注册（先于关闭执行）
func recoverEventFilter(ctx context.Context, out chan<- aistudio.Event, events <-chan aistudio.Event, where string) {
	value := recover()
	if value == nil {
		return
	}
	err := recoveredPanic(ctx, where, value)
	select {
	case out <- aistudio.Event{Kind: aistudio.EventError, Err: err}:
	case <-ctx.Done():
	}
	go drainEvents(events)
}
