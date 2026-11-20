package app

import (
	"fmt"
	"strings"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// 重新生成用完仍异常结束且没有输出时按错误结束（ABNORMAL_FINISH_ERROR，默认关闭，保存后立即生效；按上游 Mag1cFall/AIStudio2API cd3d4ab 移植）
//
// 上游以 MALFORMED_FUNCTION_CALL、UNEXPECTED_TOOL_CALL 或 MALFORMED_RESPONSE 结束、客户端还没收到内容时，生成服务先同账号重新生成
// （见 aistudio/regenerate.go）；用完（或不能重新生成）仍这样结束时原先交出一个空回复。开关打开时 forwardEvents 把这样的结束换成
// *aistudio.AbnormalFinishError：
//   - 判定在转发处：还没交出内容时，不算内容的事件（用量、思考签名、来源、引用、空正文的思考）先扣住，到第一个内容事件或结束事件
//     为止（Playground 的用量只在结束前交出，签名一般紧挨着随后的正文，扣住不改变内容事件的时序）；结束事件是异常原因时整个换成错误
//     事件、扣住的事件丢弃（协议层收到的第一个事件就是错误时按 HTTP 502 返回，见 api/official_errors.go），否则把扣住的事件照原顺序
//     先放出。原先只扣住用量：思考签名先到时当场交给协议层，流式请求随之提交 200，之后换成的错误只能作为流内错误
//   - 内容指正文、思考正文、函数调用、代码执行与媒体；思考签名、来源与引用不算
//   - 不算账户的问题：转发已在首事件之后，不冷却、不换号、不触发 MODEL_FALLBACKS，账号与出口的成功侧记账、模型资格验证照旧；
//     用量照旧写进请求日志与用量账本，结束原因照旧写“终止原因”
//   - 每个请求在开始时读一次开关（generateWithRetry），保存后下一个请求立即按新值；Gemini 原生接口（KeepAbnormalFinish）不受影响
type abnormalFinishGate struct {
	enabled bool
	// delivered 为已经向下游交出过内容
	delivered bool
	// held 为扣住的不算内容的事件（按到达顺序）
	held []aistudio.Event
	// generations 返回交出结果的那次尝试（一个账号的一次租约）共生成的次数（含同账号重新生成），nil 时不写次数
	generations func() int
}

// setAbnormalFinishError 设置 ABNORMAL_FINISH_ERROR（启动时与热更新时调用），返回值是否有变化
func (service *trackedService) setAbnormalFinishError(enabled bool) bool {
	return service.abnormalFinishError.Swap(enabled) != enabled
}

// abnormalFinishGateFor 返回这次转发用的判定；diag 为 nil（测试直接调用 forwardEvents）或请求开始时开关关闭时不扣住任何事件
func (diag *generationDiagnostics) abnormalFinishGateFor() *abnormalFinishGate {
	gate := &abnormalFinishGate{}
	if diag == nil || !diag.abnormalFinish {
		return gate
	}
	gate.enabled = true
	if budget := diag.regenerations; budget != nil {
		// 重新生成次数按请求共用（换号后的尝试共用同一份），减去这次尝试开始前已用的，只计交出结果的这次尝试
		base := diag.regenerationsBefore
		gate.generations = func() int { return budget.Used() - base + 1 }
	}
	return gate
}

// admit 按事件更新判定：hold 为这个事件先扣住（还没交出内容时不算内容的事件）；abnormal 非 nil 时这个结束事件要换成错误事件
// （扣住的事件随之丢弃，用量已在转发处记下）。错误事件不经这里：扣住的事件在它之前照原顺序放出
func (gate *abnormalFinishGate) admit(event aistudio.Event) (hold bool, abnormal *aistudio.AbnormalFinishError) {
	if !gate.enabled || gate.delivered {
		return false, nil
	}
	switch {
	case event.Kind == aistudio.EventFinish:
		if !aistudio.AbnormalFinishReason(event.FinishReason) {
			return false, nil
		}
		abnormal = &aistudio.AbnormalFinishError{Reason: event.FinishReason, Message: event.FinishMessage}
		for index := len(gate.held) - 1; index >= 0; index-- {
			if gate.held[index].Kind == aistudio.EventUsage {
				abnormal.Usage = gate.held[index].Usage
				break
			}
		}
		if gate.generations != nil {
			abnormal.Generations = gate.generations()
		}
		gate.held = nil
		return false, abnormal
	case deliversContent(event):
		gate.delivered = true
		return false, nil
	}
	gate.held = append(gate.held, event)
	return true, nil
}

// release 返回要在当前事件之前放出的扣住事件并清空
func (gate *abnormalFinishGate) release() []aistudio.Event {
	held := gate.held
	gate.held = nil
	return held
}

// deliversContent 判断事件是否向客户端交出内容：正文、思考正文、函数调用、代码执行与媒体；思考签名、来源、引用与用量不算
func deliversContent(event aistudio.Event) bool {
	switch event.Kind {
	case aistudio.EventText, aistudio.EventReasoning:
		return event.Text != ""
	case aistudio.EventToolCall, aistudio.EventExecutableCode, aistudio.EventCodeExecutionResult, aistudio.EventMedia:
		return true
	}
	return false
}

// abnormalFinishNote 为换成错误时写进请求进度与排查记录的一行
func abnormalFinishNote(err *aistudio.AbnormalFinishError) string {
	note := "上游异常结束且没有交出内容，按错误结束（ABNORMAL_FINISH_ERROR，对外 502） | 结束原因=" + strings.ToUpper(err.Reason)
	if err.Generations > 0 {
		note += fmt.Sprintf(" | 共生成=%d 次", err.Generations)
	}
	return note
}
