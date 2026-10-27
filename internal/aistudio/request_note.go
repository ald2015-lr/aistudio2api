package aistudio

import (
	"context"
	"fmt"
)

type requestNoteContextKey struct{}

// ContextWithRequestNoteObserver 接收生成服务按本次所选账户整理请求时留下的说明（例如按该账户的时长范围调整的视频时长、
// Interaction 模型忽略的 stop sequences），调用方把它们写进该请求的管理日志
func ContextWithRequestNoteObserver(ctx context.Context, observer func(string)) context.Context {
	if observer == nil {
		return ctx
	}
	return context.WithValue(ctx, requestNoteContextKey{}, observer)
}

// ReportRequestNote 报告一条请求说明；没有观察者时写进排查记录（主路由请求没有排查记录时什么也不做）
func ReportRequestNote(ctx context.Context, note string) {
	if observer, _ := ctx.Value(requestNoteContextKey{}).(func(string)); observer != nil {
		observer(note)
		return
	}
	TraceFromContext(ctx).Note(note)
}

// InteractionStopSequencesNote 为经 CreateInteractionStream 生成的请求忽略 stop sequences 时写进请求日志的说明：
// 没有确认 CreateInteractionStream 的停止序列字段，不猜字段号，也不在本地匹配（见 EncodeCreateInteractionStreamRequest）
func InteractionStopSequencesNote(count int) string {
	return fmt.Sprintf("未确认 CreateInteractionStream 的停止序列字段，已忽略 %d 个停止序列", count)
}
