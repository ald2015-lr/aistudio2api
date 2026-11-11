package aistudio

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// Playground 通道上的工具约束核对与同账号重新生成
//
// Build 通道按 functionCallingConfig 原生执行必须调用与指定函数；Playground 没有对应字段（官网的 toolConfig 只有 retrieval 与
// includeServerSideToolInvocations），prepareToolRequest 只能在系统指令末尾写明要求，模型有时不照做（直接文字回答、调用没选的函数、
// strict 参数不符）。原先在事件流上核对、到结束才判违约，正文与思考已经先交给了下游，客户端只能收到 502。现在在 Client.Generate 的
// 解码循环里核对（toolGuard），与客户端还没收到内容时的重新生成（regenerate.go）共用同一套规则：
//   - 扣住事件：要求必须调用（tool_choice 为 required/any，或指定了函数）时，正文、思考、签名、引用等事件都先扣住，出现第一个通过核对的
//     工具调用（必须调用且声明了内置工具时，用了内置工具也算）时先放出扣住的事件、再放出这个调用，之后照常转发；到结束都没有调用为违约，
//     扣住的事件不交出。只有 strict Schema 或单次调用的契约（auto 模式）不扣住：普通聊天带 strict 工具时流式不能失去实时性。
//     流式请求扣住期间协议层照常写数据型心跳（STREAM_DATA_HEARTBEAT）；扣住第一个事件时经 FirstEventClock.PauseFirstEvent 暂停
//     首事件超时（FIRST_EVENT_TIMEOUT），app 层同时让请求上下文退出首事件前的固定期限（REQUEST_TIMEOUT）：上游已经在输出，扣住期间
//     改按上游网络活动判空闲（连续 REQUEST_TIMEOUT 没有字节才到期），整段受总时长上限约束（见 app/generate_context.go 的 holdUpstream），
//     思考很长、或 long- 续写几轮之后才调用的请求不会被首事件前期限截成 504。本次 Generate 第一次扣住时写一行 INFO 说明
//   - 违约（调用了未选择的函数、strict 参数不符、要求调用却到结束都没有调用，命中停止序列时同样按结束核对）而还没有向下游交出任何事件时，
//     在同账号、同租约重新生成：与批次 B 共用每请求的次数（RegenerationBudget）、准入（ReserveRegeneration：账号冷却、每分钟名额满或
//     出口限速时不重新生成）、剩余期限与首事件超时够不够再来一次的判断（regenerationTimeShort：剩余期限按整段耗时，首事件超时按
//     到扣住第一个事件的耗时，首事件计时那时已暂停），首事件计时按每次发送重新起算；
//     有随机后缀时换一个新的（ContextWithRegenerationVariant），并把工具要求写得更明确（toolGuard.strengthen，级别见 toolHintsFor）。
//     必须调用时第一次发送就写明“必须调用、参数符合 Schema、不要只回复文字”（第 1 级，见 prepareToolRequest：原先第一次发送的
//     “Use one of these tools …” gemini-4 常不照做），重新生成时再加一句、并在最后一条用户文字消息末尾附上同样的要求（第 2 级，两次重新生成
//     都是这一级）；auto 模式只有 strict Schema 或指定函数的契约第一次发送不写要求，第一次重新生成写明只能调用哪些函数、参数要符合 Schema，
//     第二次再加一句。没有随机后缀的请求同样重新生成；要求已是第 2 级（必须调用时的第二次重新生成）而没有随机后缀可换时，这次发送会与
//     上一次逐字节相同，上游对完全相同的输入返回完全相同的回复（见 regenerate.go），与批次 B 遇到确定性结果时一样不再重新生成，写一行说明
//     后按 502 结束，不占次数与准入（见 toolRegeneration）。WAA binding 的组成规则不变，内容本身的变化照常参与
//   - 已经交出过事件（auto 模式的 strict 违约发生在正文之后等）、long- 续写已决定发第二轮、次数用完或不准入时不重新生成，按 502 结束
//     （*ToolContractError，PublicMessage 写明是哪种违约与共生成了几次），请求说明写明原因。要求必须调用而上游以内容拦截类原因结束
//     （safety、recitation、prohibited_content 等，加强工具要求对它基本没有作用）时同样不重新生成、不占次数，说明写出结束原因；
//     max_tokens 等其他结束原因照常重新生成，请求说明里的原因带着结束原因
//   - 只要求内置工具（如只声明了 google_search 的必须调用）时加强的要求写“回答前必须使用这个工具”，不称为函数、不要求只回复调用，
//     免得模型输出同名的函数调用、被判为调用了未选择的函数
//   - 不带工具约束的请求（contract 为 nil）逐字节、逐事件不变；契约第一次就满足时事件序列不变（必须调用时只是扣住到调用出现）。
//     Build 与 Interaction 通道照旧经 forward 核对，不扣住、不重新生成

// toolGuard 为一次 Generate 在 Playground 通道上的工具约束核对状态：每次发送（含重新生成）重新计（reset），加强的级别在整次 Generate 内保留
type toolGuard struct {
	contract *toolContract
	// calls 为本次发送的函数调用数（单次调用时第二个起丢弃），serverSideUsed 为用过内置工具（搜索来源、代码执行）
	calls          int
	serverSideUsed bool
	// satisfied 为本次发送已出现通过核对的调用（必须调用的契约据此停止扣住）
	satisfied bool
	// held 为扣住、还没交出的事件；paused 为本次发送已暂停首事件计时，heldAfter 为本次发送从开始到扣住第一个事件的耗时
	// （重新生成时据此判断首事件超时够不够，见 firstEventElapsed）；noted 为本次 Generate 已写过扣住的说明
	held      []Event
	paused    bool
	heldAfter time.Duration
	noted     bool
	// level 为工具要求的级别（从第一次发送的级别 contract.firstLevel() 起，每次因违约重新生成加一，最多 2，见 toolHintsFor）；
	// sends 为本次 Generate 已核对的发送次数
	level int
	sends int
}

// newToolGuard 为契约建立核对状态；没有契约时为 nil（各方法对 nil 按没有约束处理）
func newToolGuard(contract *toolContract) *toolGuard {
	if contract == nil {
		return nil
	}
	return &toolGuard{contract: contract, sends: 1, level: contract.firstLevel()}
}

// holds 判断契约要求扣住事件：必须调用时扣住到第一个通过核对的调用
func (guard *toolGuard) holds() bool {
	return guard != nil && guard.contract.required
}

// admit 核对一个要交给下游的事件：返回现在要交出的事件（扣住时为空；出现第一个通过核对的调用时为先前扣住的事件加上它），
// held 为这个事件被扣住，违约时返回 *ToolContractError
func (guard *toolGuard) admit(event Event) (pass []Event, held bool, violation *ToolContractError) {
	if guard == nil {
		return []Event{event}, false, nil
	}
	contract := guard.contract
	switch event.Kind {
	case EventGrounding, EventExecutableCode, EventCodeExecutionResult:
		guard.serverSideUsed = true
	case EventToolCall:
		if event.ToolCall == nil {
			break
		}
		guard.calls++
		if contract.single && guard.calls > 1 {
			// 要求单次调用而上游返回了多个：只保留第一个，其余丢弃
			return nil, false, nil
		}
		if violation := contract.check(*event.ToolCall); violation != nil {
			return nil, false, violation
		}
		if !guard.satisfied {
			guard.satisfied = true
			return guard.release(event), false, nil
		}
	}
	if !guard.satisfied && contract.required && contract.serverSide && guard.serverSideUsed {
		guard.satisfied = true
		return guard.release(event), false, nil
	}
	if guard.holds() && !guard.satisfied {
		guard.held = append(guard.held, event)
		return nil, true, nil
	}
	return []Event{event}, false, nil
}

// release 交出扣住的事件与 event
func (guard *toolGuard) release(event Event) []Event {
	pass := append(guard.held, event)
	guard.held = nil
	return pass
}

// violation 判断本次发送解码结束后的违约：解码中途核对出的违约（admit 返回、经 emit 传回），或正常结束（含命中停止序列）时要求调用
// 却没有调用。finish 为本次发送的结束事件。其他错误（流中断、流尾错误）不算违约，照旧按原错误处理。返回的违约带上共生成的次数
func (guard *toolGuard) violation(err error, finish *Event) *ToolContractError {
	if guard == nil {
		return nil
	}
	var violation *ToolContractError
	switch {
	case errors.As(err, &violation):
	case err != nil && !errors.Is(err, errStopSequenceMatched):
		return nil
	case !guard.contract.required || guard.satisfied:
		return nil
	case err != nil:
		violation = guard.contract.missingCall("命中停止序列")
	case finish != nil && finish.FinishReason != "":
		violation = guard.contract.missingCall("结束原因=" + finish.FinishReason)
		if contentBlockedFinish(finish.FinishReason) {
			// 内容拦截类结束：加强工具要求对它基本没有作用，不重新生成（见 toolRegeneration），对外说明写出结束原因
			violation.Blocked = finish.FinishReason
		}
	default:
		violation = guard.contract.missingCall("")
	}
	violation.Attempts = guard.sends
	return violation
}

// contentBlockedFinish 判断结束原因为内容拦截（安全、版权背诵、屏蔽词、违禁内容、个人敏感信息、图片安全类），与协议层按内容拦截
// 返回的结束原因相同
func contentBlockedFinish(reason string) bool {
	switch reason {
	case "safety", "recitation", "blocklist", "prohibited_content", "spii",
		"image_safety", "image_prohibited_content", "image_recitation":
		return true
	}
	return false
}

// reset 为下一次发送（重新生成、续写回退的重发）清空本次发送的核对状态：扣住的事件属于被放弃的这次，不交出
func (guard *toolGuard) reset() {
	if guard == nil {
		return
	}
	guard.calls, guard.serverSideUsed, guard.satisfied, guard.held, guard.paused, guard.heldAfter = 0, false, false, nil, false, 0
	guard.sends++
}

// pauseOnHold 在本次发送扣住第一个事件时暂停首事件计时（上游已经有了响应；app 层同时退出首事件前的固定期限，改按上游活动判空闲），
// 记下本次发送从开始（attemptStarted）到扣住第一个事件的耗时；本次 Generate 第一次扣住时写一行说明
func (guard *toolGuard) pauseOnHold(ctx context.Context, attemptStarted time.Time) {
	if guard == nil || guard.paused {
		return
	}
	guard.paused = true
	guard.heldAfter = time.Since(attemptStarted)
	if !guard.noted {
		guard.noted = true
		ReportRequestInfo(ctx, fmt.Sprintf("工具约束要求必须调用：正文与思考扣住到调用出现 | 要求=%s | 首个上游事件=%s",
			strings.Join(guard.contract.toolNames, ", "), guard.heldAfter.Round(time.Millisecond)))
	}
	if clock := firstEventClockFrom(ctx); clock != nil {
		clock.PauseFirstEvent()
	}
}

// firstEventElapsed 返回判断首事件超时够不够再来一次时用的耗时：本次发送扣住过事件时为到扣住第一个事件的耗时（首事件计时那时已暂停，
// 重发真正要等的只是这一段），否则为整次发送的耗时 elapsed
func (guard *toolGuard) firstEventElapsed(elapsed time.Duration) time.Duration {
	if guard == nil || !guard.paused {
		return elapsed
	}
	return guard.heldAfter
}

// canStrengthen 判断工具要求还能再加强一级（还没到第 2 级，见 toolHintsFor）：不能时这次重新生成的工具要求与上一次相同
func (guard *toolGuard) canStrengthen() bool {
	return guard != nil && guard.level < 2
}

// strengthen 因违约重新生成前把工具要求写得更明确（最多到第 2 级，见 toolHintsFor），返回级别是否提高了（已是第 2 级时为假，
// 这次重新生成的工具要求与上一次相同）
func (guard *toolGuard) strengthen() bool {
	if !guard.canStrengthen() {
		return false
	}
	guard.level++
	return true
}

// apply 按工具要求的级别重写发往上游的请求（source 为整理成线格式之前的请求，系统指令里是第一次发送的要求）：系统指令里的工具要求换成
// toolHintsFor 的写法，第二级且必须调用时另在最后一条用户文字消息末尾附上同样的要求。级别与第一次发送相同时原样返回（与第一次发送逐字节相同）
func (guard *toolGuard) apply(source GenerateRequest) GenerateRequest {
	if guard == nil || guard.level == guard.contract.firstLevel() {
		return source
	}
	request := source
	system := request.System
	if request.toolHints != "" {
		system = request.clientSystem
	}
	hints, requirement := guard.contract.toolHintsFor(guard.level)
	request.clientSystem, request.toolHints = system, strings.Join(hints, "\n")
	request.System = joinToolHints(system, request.toolHints)
	if guard.level >= 2 && guard.contract.required {
		request.Contents = appendToolRequirement(request.Contents, requirement)
	}
	return request
}

// toolHintsFor 返回第 level 级（至少 1）的工具要求与其中的核心一句：搜索偏好照旧；必须调用时为“必须调用、参数符合 Schema、
// 不要只回复文字”（只要求内置工具时为“回答前必须使用这个工具”，不称为函数；必须调用的第一次发送就是第 1 级，见 prepareToolRequest），
// 只有 strict Schema 或指定函数的 auto 模式写明只能调用哪些函数、参数要符合 Schema；单次调用照旧；第二级另加一句。
// 要求里写发往上游的函数名（超过 63 字节时为截短名，见 wireFunctionName）：与函数声明一致，模型照着要求里的名字调用也对得上
// （Playground 与 Build 通道的声明都用截短名，prepareToolRequest 在选通道之前写要求）；回传时换回原名，核对按原名
func (contract *toolContract) toolHintsFor(level int) ([]string, string) {
	hints := slices.Clone(contract.searchHints)
	toolNames, functionNames := wireFunctionNames(contract.toolNames), wireFunctionNames(contract.functionNames)
	var requirement, extra string
	switch {
	case contract.required && contract.singleFunction():
		requirement = "You must call the function " + toolNames[0] + " in this response with arguments that match its schema. Do not reply with plain text."
		extra = "Reply with the function call only."
	case contract.required && len(contract.functionNames) == 0:
		// 只要求内置工具（如只声明了 google_search）：它不是函数，不写 function、schema，也不要求只回复调用，免得模型输出同名的函数调用
		if len(toolNames) == 1 {
			requirement = "You must use the tool " + toolNames[0] + " in this response before answering. Do not answer from memory."
		} else {
			requirement = "You must use one of these tools in this response before answering: " + strings.Join(toolNames, ", ") + ". Do not answer from memory."
		}
		extra = "Base the answer on the tool results."
	case contract.required:
		requirement = "You must call one of these tools in this response: " + strings.Join(toolNames, ", ") +
			". The arguments must match the tool's schema. Do not reply with plain text."
		extra = "Reply with the tool call only."
	default:
		requirement = "If you call a function in this response, call only these functions: " + strings.Join(functionNames, ", ") +
			", with arguments that match the function's schema exactly."
		extra = "Check the arguments against the function's schema before calling it."
	}
	hints = append(hints, requirement)
	if contract.single {
		hints = append(hints, toolSingleHint)
	}
	if level >= 2 {
		hints = append(hints, extra)
	}
	return hints, requirement
}

// appendToolRequirement 在最后一条用户文字消息末尾附上工具要求（返回副本，不修改调用方的数据）：最后一条不是用户消息或是工具结果时不附
// （与随机后缀的规则相同），末尾是文字时接在后面，否则单独作为一个文本 part
func appendToolRequirement(contents []Content, requirement string) []Content {
	last := len(contents) - 1
	if last < 0 || contents[last].Role != RoleUser || len(contents[last].Parts) == 0 {
		return contents
	}
	content := contents[last]
	if slices.ContainsFunc(content.Parts, func(part Part) bool { return part.FunctionResult != nil }) {
		return contents
	}
	parts := slices.Clone(content.Parts)
	if tail := &parts[len(parts)-1]; plainUserText(*tail) {
		tail.Text += "\n\n" + requirement
	} else {
		parts = append(parts, Part{Text: requirement})
	}
	copied := slices.Clone(contents)
	content.Parts = parts
	copied[last] = content
	return copied
}

// plainUserText 判断 part 为可以直接接上文字的正文
func plainUserText(part Part) bool {
	return part.Text != "" && !part.Thought && part.InlineData == nil && part.ExternalMedia == nil && part.File == nil &&
		part.FunctionCall == nil && part.FunctionResult == nil && part.ExecutableCode == nil && part.CodeExecutionResult == nil &&
		part.SpeechMetadata == nil
}

// toolRegenerationInput 为工具约束违约后判断能否同账号重新生成的依据
type toolRegenerationInput struct {
	violation *ToolContractError
	// finish 为被放弃这次的结束事件（中途违约时为 nil），elapsed 为这次的耗时，firstEvent 为判断首事件超时用的耗时
	// （扣住过事件时为到扣住第一个事件的耗时，见 toolGuard.firstEventElapsed）
	finish            *Event
	elapsed           time.Duration
	firstEventElapsed time.Duration
	// delivered 为已经向下游交出过事件，continued 为 long- 续写已决定发第二轮
	delivered, continued bool
	// unchanged 为这次重新生成的输入会与上一次发送逐字节相同：没有随机后缀可换（调用方已调用过一次换后缀的函数，结果留着重新生成时用），
	// 工具要求也已是第 2 级（toolGuard.canStrengthen 为假）
	unchanged bool
	// budget 为本请求各次尝试共用的重新生成次数
	budget *RegenerationBudget
	// model 为目录模型 ID（准入的冷却范围按它）
	model string
}

// toolRegeneration 判断工具约束违约后能否同账号重新生成（规则与批次 B 相同，见文件开头说明）：已交出事件、续写已发第二轮、上游以内容拦截
// 结束、剩余期限或首事件超时不够再来一次、这次的输入会与上一次相同（没有随机后缀可换、工具要求已是第 2 级：上游对完全相同的输入返回
// 完全相同的回复，与批次 B 的 regenerationNoVariantNote 一样不原样重发）、次数用完或账号不准入时写一行说明并返回假（按 502 结束；
// 次数用完之前的几种不占次数与准入）；能时占用一次次数与准入名额，返回序号（从 1 计）
func toolRegeneration(ctx context.Context, input toolRegenerationInput) (int, bool) {
	reason := "原因=" + input.violation.Detail
	switch {
	case input.delivered:
		ReportRequestNote(ctx, "工具约束未满足，已向客户端交出内容，不重新生成 | "+reason)
		return 0, false
	case input.continued:
		ReportRequestNote(ctx, "工具约束未满足，long- 续写已发第二轮，不重新生成 | "+reason)
		return 0, false
	case input.violation.Blocked != "":
		ReportRequestNote(ctx, "工具约束未满足，上游以内容拦截结束（结束原因="+input.violation.Blocked+
			"），加强工具要求多半没有作用，不重新生成 | "+reason)
		return 0, false
	}
	reason += " | " + regenerationDetail(nil, input.finish, input.elapsed)
	if short := regenerationTimeShort(ctx, input.elapsed, input.firstEventElapsed); short != "" {
		ReportRequestNote(ctx, "工具约束未满足，"+short+" | "+reason)
		return 0, false
	}
	if input.unchanged {
		ReportRequestNote(ctx, toolRegenerationNoVariantNote+" | "+reason)
		return 0, false
	}
	sequence, ok := input.budget.take()
	if !ok {
		ReportRequestNote(ctx, fmt.Sprintf("工具约束未满足，重新生成次数已用完（%d 次），不再重新生成 | %s", regenerationLimit, reason))
		return 0, false
	}
	if lease, leased := AccountLeaseFromContext(ctx); leased {
		if denied := lease.ReserveRegeneration(input.model); denied != "" {
			input.budget.giveBack()
			ReportRequestNote(ctx, "工具约束未满足，但"+denied+"，不重新生成 | "+reason)
			return 0, false
		}
	}
	return sequence, true
}

// toolRegenerationNoVariantNote 为工具约束违约、但这次重新生成的输入会与上一次逐字节相同而不重新生成时的说明（与批次 B 的
// regenerationNoVariantNote 同一个道理）
const toolRegenerationNoVariantNote = "工具约束未满足，但请求没有加随机后缀（客户端指定了 seed、温度为 0、最后一轮是工具结果或 REPEAT_PROMPT_NONCE 关闭）" +
	"且工具要求已是最高的第 2 级，原样重发多半得到同样的结果，不再重新生成"

// toolRegenerationNote 为因工具约束同账号重新生成写进请求说明的一行：原因、第几次、被放弃这次的耗时与上游结束说明、输入怎么换的
func toolRegenerationNote(violation *ToolContractError, finish *Event, elapsed time.Duration, sequence int, input string) string {
	return fmt.Sprintf("工具约束未满足，同账号重新生成 | 原因=%s | 第 %d/%d 次 | %s | 输入=%s",
		violation.Detail, sequence, regenerationLimit, regenerationDetail(nil, finish, elapsed), input)
}
