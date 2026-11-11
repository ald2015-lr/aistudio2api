package aistudio

import (
	"context"
	"fmt"
	"strings"
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

// InteractionThinkingSuffixIgnoredNote 为模型名的思考等级后缀（-minimal、-low、-medium、-high、-128）对经 CreateInteractionStream
// 生成、模型目录却没有思考等级（能力码 52，如 gemini-omni-1.2-flash）的模型不生效时写进请求日志的说明（协议层整理模型名时留下）。
// level 为后缀对应的等级（minimal、low、medium、high）
func InteractionThinkingSuffixIgnoredNote(modelID string, level string) string {
	return fmt.Sprintf("模型名后缀指定的思考等级 %s 对 %s 不生效：模型目录没有思考等级（能力码 52），与官网一样不发送思考等级，已忽略后缀", level, modelID)
}

// InteractionThinkingIgnoredNote 为客户端的思考参数（reasoning_effort、思考预算）对模型目录没有思考等级（能力码 52）的 Interaction 模型
// 不生效时写进请求日志的说明；模型有思考等级或请求没有思考参数时返回空串。请求体与官网一样不带 GenerationConfig field 6
// （见 interactionThinkingLevel）
func InteractionThinkingIgnoredNote(modelID string, defaults GenerationDefaults, config GenerationConfig) string {
	if defaults.ThinkingLevel {
		return ""
	}
	var params []string
	if effort := strings.TrimSpace(config.ReasoningEffort); normalizeReasoningEffort(effort) != "" {
		params = append(params, "reasoning_effort="+effort)
	}
	if config.ThinkingBudget != nil {
		params = append(params, fmt.Sprintf("思考预算=%d", *config.ThinkingBudget))
	}
	if len(params) == 0 {
		return ""
	}
	return fmt.Sprintf("客户端的思考参数（%s）对 %s 不生效：模型目录没有思考等级（能力码 52），与官网一样不发送思考等级", strings.Join(params, "、"), modelID)
}

type requestInfoContextKey struct{}

// ContextWithRequestInfoObserver 接收生成服务发送前留下的排查信息（例如 Interaction 请求的结构摘要），调用方以 INFO 写进该请求的管理日志；
// 与 ContextWithRequestNoteObserver 分开：那些是改动了请求的说明（WARN），这些只是证据
func ContextWithRequestInfoObserver(ctx context.Context, observer func(string)) context.Context {
	if observer == nil {
		return ctx
	}
	return context.WithValue(ctx, requestInfoContextKey{}, observer)
}

// ReportRequestInfo 报告一条排查信息；没有观察者时写进排查记录（主路由请求没有排查记录时什么也不做）
func ReportRequestInfo(ctx context.Context, message string) {
	if observer, _ := ctx.Value(requestInfoContextKey{}).(func(string)); observer != nil {
		observer(message)
		return
	}
	TraceFromContext(ctx).Note(message)
}

// InteractionToolsNote 为经 CreateInteractionStream 生成的请求（gemini-omni）带了工具时写进请求日志的说明；没有工具时为空串。
// Interaction 请求体不带工具（见 EncodeCreateInteractionStreamRequest），Omni 的模型目录也没有 function_declarations：中转与客户端自动附带的
// 函数（如 new-api 的记忆函数 save_memory、delete_memory）与 Google 工具不发送，原先函数声明在发送前按模型目录校验，按通用措辞的 400 结束
func InteractionToolsNote(tools Tools) string {
	google := len(tools.Google)
	if tools.GoogleSearch != nil && google == 0 {
		google = 1
	}
	if len(tools.Functions) == 0 && google == 0 {
		return ""
	}
	return fmt.Sprintf("CreateInteractionStream 不带工具，已忽略 %d 个函数声明与 %d 个 Google 工具（Omni 模型不调用工具）", len(tools.Functions), google)
}
