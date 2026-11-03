package aistudio

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync/atomic"
	"time"
)

// 模型名带 long- 前缀的请求的自动续写（long- 前缀的解析见 api 包 model_alias.go，开关与默认总上限见 LONG_OUTPUT_*）。
//
// 只在 Playground GenerateContent 通道进行（Build 与 Interaction 通道按普通请求处理），整个过程在同一个租约（同一账号、同一 Worker）上完成，
// 不换号；每一轮都是一次新的 GenerateContent，经 doProtected 照常取新的 WAA proof、计入每账号每分钟上限，发送前遵守号池的准入
// （每分钟窗口含其他租约的预占、出口令牌桶、账号短冷却，与重新生成共用同一套准入，见 resend_admission.go）。按模型能力二选一：
//
//   - 官网续写：模型目录有能力码 91（output_continuation，如 gemini-4-argon）时照官网发送：generation config field 46 为 1、根 field 11 省略，
//     第 N 轮（从 0 计）的输出上限为 min(总上限, (N+1)×32768)；上游以终止原因 22 结束，或以 max_tokens 结束且同一帧带续写 token
//     （候选 field 9）而本轮上限低于总上限时，下一轮复制原请求、根 field 17 写入最近的续写 token；
//   - 模拟续写：其他模型本轮以 max_tokens 结束（达到单轮上限、总输出还没到总上限）、有新的正文且没有工具调用、代码执行、媒体与搜索来源时，
//     把已输出的正文作为模型轮放在历史末尾再发一轮。是否原样以模型轮结尾与客户端预填充共用 prefill.go 的同一套规则：名单与判断函数
//     （NativePrefillModel）、拒绝的识别（prefillRejectedError）与按“模型 + 通道”记 prefillRejectionTTL 的记忆。能原生预填充、近期没被拒过的
//     模型原样以模型轮结尾；上游以 400 拒绝时（发送时或流尾）记住这个模型，在这一轮内改为补用户消息重发（不换号，重发前同样预占、
//     等号池准入，不经 PrefillRetryError 换号：那只用于普通请求与 long- 请求的第一轮），之后各轮与之后的请求直接补用户消息；
//     其余模型在末尾补一条请模型从上一条消息结尾处接着写的用户消息（PrefillContinuationPrompt，与协议层处理客户端预填充的做法相同）。新一轮开头与已输出正文末尾重复的部分去掉（至少 seamMinOverlap 字节才认定为重复），
//     后续轮次的思考与思考签名不输出（工具调用的签名保留），思考强度降到最低（只降不升，见 lowerContinuationThinking），
//     搜索来源里正文片段的位置换算到合并后的正文（见 shiftGrounding）。结构化输出与带 Google 内置工具的请求不模拟续写（见 newLongOutputPlan）。
//
// 与客户端还没收到内容时的重新生成（regenerate.go）的嵌套：重新生成只在还没向下游交出任何事件时发生，只可能落在第一轮；每次重发都经
// sendPlayground 按同一份请求重新生成续写计划，官网续写的编码（config[45]、省略根 field 11）照旧，各轮的合并用量在每次发送的解码里
// 从头计，被放弃那次的用量不会并进来；第一轮还没交出事件就以可重新生成的原因结束（regenerableFinish）时不报告这一轮结束（这次结果
// 可能被放弃，见 longOutputRounds.mayRegenerate）。续写决定发第二轮之后（longOutputRounds）不再重新生成，失败按下面的续写规则结束；
// 重新生成次数与续写轮数各算各的。续写的后续轮次在 Generate 的原始上下文上发送（不用重发的上下文）：首事件计时不因续写的各轮重新起算。
//
// 流尾：每一轮的流尾 google.rpc 状态（见 decoder.go 的 playgroundTrailerError）在这一轮的帧之后、续写判断之前处理：
// 同一个响应里既以终止原因 22 要求续写、流尾又是错误时按错误处理，不再续写——第一轮与普通请求相同（错误照常交出），第二轮起按
// 这一轮失败体面结束（Err 为流尾错误，账号状态照常处理）；流尾状态码为 0 时照常续写。
//
// 对客户端无感：各轮的正文、思考与工具事件按顺序接成一条流，用量与结束事件只在最后发一次（各轮用量合并，见 mergeContinuedUsage），
// 不再续写时上游的 22 按 max_tokens 结束。第一轮失败与普通请求相同（以该错误结束）；第二轮起发送失败或输出中途失败时不报错，
// 先交出已收到的正文，再按 max_tokens 结束、带合并后的用量（流式与非流式都完整返回已输出的内容，见 finishFailedRound），
// 续写后的输入超过账号输入上限（模拟续写把已输出的正文发回上游）同样按 max_tokens 结束；第二轮起本服务的期限到期时生成服务经
// ContinuationProgress.Interrupt 只结束这一轮，同样按 max_tokens 体面结束。每一轮经 ContextWithContinuationObserver
// 报告进度：生成服务写一行请求日志，并在开始下一轮时重新计算总时长上限、刷新空闲期限的计时；不续写的 long- 请求报告一次原因

const (
	// continuationRoundTokens 为官网自动续写每轮增加的输出上限
	continuationRoundTokens = 32768
	// longOutputMaxRounds 为一次 long- 请求最多的轮数（含第一轮），防止上游每轮只输出很少时无限续写
	longOutputMaxRounds = 64
	// seamWindow 为模拟续写在新一轮开头最多扣住、与已输出正文末尾比对的字节数
	seamWindow = 1024
	// seamMinOverlap 为认定新一轮开头重复了已输出正文末尾的最少字节数：更短的相同前后缀按巧合处理，原样保留
	seamMinOverlap = 20
	// continuationWaitStep 为等待号池准入时报告进度的间隔（生成服务据此刷新空闲期限的计时）
	continuationWaitStep = 5 * time.Second
)

// 自动续写方式（写进请求日志）
const (
	ContinuationOfficial  = "官网续写"
	ContinuationSimulated = "模拟续写"
)

// 模拟续写下一轮的接续方式（写进请求日志）
const (
	ContinuationPrefillNative = "原生预填充"
	ContinuationPrefillPrompt = "补用户消息"
)

// ContinuationStage 为自动续写进度的阶段
type ContinuationStage int

const (
	// ContinuationRoundEnd 为一轮结束：Next 为真时接着发下一轮，否则整个请求在这一轮结束（Stop 为原因）
	ContinuationRoundEnd ContinuationStage = iota + 1
	// ContinuationWaiting 为下一轮在等号池准入（WaitReason：账号每分钟窗口、出口令牌或账号短冷却），Wait 为还要等的时间；
	// 等待中每 5 秒报告一次
	ContinuationWaiting
	// ContinuationSending 为下一轮开始发送（准备 WAA proof 之前）
	ContinuationSending
	// ContinuationFailed 为第二轮起发送失败（Sending 为真）或输出中途失败：不报错，已输出的内容完整返回，按 max_tokens 结束；
	// Err 为原因（生成服务据此照常处理账号状态：额度冷却、登录失效），Stop 为写进日志的说明
	ContinuationFailed
	// ContinuationSkipped 为 long- 请求这次不续写（Stop 为原因，例如总上限不超过单轮上限），按普通请求发送
	ContinuationSkipped
)

// ContinuationProgress 为自动续写的进度
type ContinuationProgress struct {
	Stage ContinuationStage
	// Mode 为续写方式（ContinuationOfficial、ContinuationSimulated）
	Mode string
	// Round 为刚结束（RoundEnd）或即将发送（Waiting、Sending、Failed）的轮次，从 1 计
	Round int
	// RoundFinish 为刚结束这一轮的上游结束原因（22 为 continuation）
	RoundFinish string
	// RoundInput 为这一轮上游报告的输入 token（模拟续写的后续轮次含发回上游的已输出正文）；RoundOutput 为这一轮的输出 token
	// （正文与思考），Output 为到这一轮为止各轮累计，Total 为输出总上限
	RoundInput, RoundOutput, Output, Total int64
	// Next 为接着发下一轮；Stop 为不再续写的原因（Next 为假时）
	Next bool
	Stop string
	// Prefill 为模拟续写下一轮的接续方式（ContinuationPrefillNative、ContinuationPrefillPrompt）
	Prefill    string
	Wait       time.Duration
	WaitReason ContinuationWaitReason
	// WaitDetail 为等待的具体原因（每分钟名额、出口令牌或冷却截止，写法与重新生成不准入时的说明相同，见 resend_admission.go）
	WaitDetail string
	// Sending 为 ContinuationFailed 发生在发送阶段（这一轮还没有任何输出）
	Sending bool
	Err     error
	// Interrupt 只在 RoundEnd 且 Next 为真时给出：结束即将开始的这一轮（等号池准入、发送与输出都算），续写按这一轮失败处理
	// （不报错、已输出的内容完整返回、按 max_tokens 结束，Err 为 *ContinuationInterruptedError）。生成服务在本服务的首事件后空闲期限、
	// 总时长上限或绝对总时长上限到期时调用它，不结束整个请求；这一轮结束（下一次 RoundEnd 或 Failed）后调用不再有作用
	Interrupt func(cause error)
}

// ContinuationInterruptedError 为生成服务经 ContinuationProgress.Interrupt 结束了续写的某一轮（第二轮起），Cause 为原因
// （本服务的首事件后空闲期限、总时长上限到期等）
type ContinuationInterruptedError struct {
	Cause error
}

func (e *ContinuationInterruptedError) Error() string {
	if e.Cause == nil {
		return "本服务结束了自动续写的这一轮"
	}
	return e.Cause.Error()
}

func (e *ContinuationInterruptedError) Unwrap() error {
	return e.Cause
}

type continuationContextKey struct{}

// ContextWithContinuationObserver 登记自动续写进度的接收方（生成服务写请求日志、续期请求期限）
func ContextWithContinuationObserver(ctx context.Context, observer func(ContinuationProgress)) context.Context {
	if observer == nil {
		return ctx
	}
	return context.WithValue(ctx, continuationContextKey{}, observer)
}

func reportContinuation(ctx context.Context, progress ContinuationProgress) {
	if observer, ok := ctx.Value(continuationContextKey{}).(func(ContinuationProgress)); ok {
		observer(progress)
	}
}

// officialContinuation 为官网自动续写本轮的编码设置：round 从 0 计，token 为上一轮给出的续写 token（第一轮为空），total 为输出总上限
type officialContinuation struct {
	round int
	token string
	total int64
}

// continueGenerationConfig 为官网自动续写开启续写（generation config field 46 为 1），并把输出上限换算为第 round 轮的累计上限
func continueGenerationConfig(config []any, round int, total int64) []any {
	for len(config) < 46 {
		config = append(config, nil)
	}
	config[45] = int64(1)
	if _, ok := config[3].(int64); ok {
		config[3] = min(total, int64(round+1)*continuationRoundTokens)
	}
	return config
}

// longOutputPlan 为一次 long- 请求的续写计划
type longOutputPlan struct {
	official bool
	// model 为目录模型 ID（原生续写的名单、拒绝记忆与准入的冷却范围都按它）
	model string
	// total 为输出总上限（正文与思考合计）；roundLimit 为单轮上限（官网续写为 32768，模拟续写为模型目录的输出上限）
	total, roundLimit int64
	// prefill 为模拟续写下一轮原样以模型轮结尾（能原生预填充的模型；上游拒绝后改为假）
	prefill bool
}

// newLongOutputPlan 按请求与模型目录生成续写计划（modelID 为目录模型 ID）；不续写时返回 nil 与原因（按普通请求发送，与不带 long- 的请求逐字节相同）：
// 不是 long- 请求（原因为空），模型经 Interaction 生成，图片、语音、转写请求或要求非文本输出，以及总上限不超过单轮上限
// （客户端给出的输出上限本来一轮就够，或模拟续写没有可用的默认总上限）。模拟续写另外不用于结构化输出（JSON 模式或 response schema：
// 上游按约束解码要求回复从头就是一个完整的 JSON，第二轮不会接着写半截的 JSON，拼起来是“半个 JSON + 一个新的 JSON”）与
// 带 Google 内置工具的请求（联网搜索、URL Context、地图、代码执行：来源列表、引用位置与工具结果按单轮给出，拼接后的位置与来源都不对）；
// 官网续写由上游接着同一次生成往下写，不受这两条影响。
//
// 官网续写按官网的分段方式：总上限超过 32768 就分轮（官网 Output length 超过 32768 时同样分多段续写），不按目录的单轮上限
// （如 65536）先发一个普通请求：普通请求一轮能否写到 32768 以上没有证实，客户端给出的上限被 MIN_OUTPUT_TOKENS 提高到 60000 时
// 改发普通请求，单轮实际写不到 32768 以上的话续写就失去作用；代价只在输出确实超过 32768 时多一轮
func newLongOutputPlan(request GenerateRequest, defaults GenerationDefaults, modelID string) (*longOutputPlan, string) {
	if !request.LongOutput {
		return nil, ""
	}
	switch {
	case defaults.InteractionStream:
		return nil, "该模型经 Interaction 通道生成"
	case defaults.ImageRoute:
		return nil, "该模型是图片模型"
	case defaults.MaxOutputTokens <= 0:
		return nil, "模型目录没有单轮输出上限"
	}
	config := request.Config
	if config.SpeechConfig != nil || config.TranscriptionConfig != nil || config.ImageConfig != nil {
		return nil, "语音、转写或图片请求"
	}
	for _, modality := range config.ResponseModalities {
		if modality != ResponseModalityText {
			return nil, "请求要求非文本输出"
		}
	}
	// 客户端给出的输出上限按普通请求的规则先套用最大输出下限（MIN_OUTPUT_TOKENS，不超过模型单轮上限），再作为总上限
	config = applyOutputFloor(config, request.MinOutputTokens, defaults.MaxOutputTokens)
	clientLimit := config.MaxOutputTokens != nil && *config.MaxOutputTokens > 0
	if defaults.ContinuedOutputTokens > 0 {
		total := defaults.ContinuedOutputTokens
		if clientLimit {
			total = min(*config.MaxOutputTokens, max(defaults.MaxOutputTokens, defaults.ContinuedOutputTokens))
		}
		if total <= continuationRoundTokens {
			return nil, fmt.Sprintf("总上限 %d 不超过官网续写的单轮上限 %d", total, continuationRoundTokens)
		}
		return &longOutputPlan{official: true, model: modelID, total: total, roundLimit: continuationRoundTokens}, ""
	}
	if structuredOutput(config) {
		return nil, "结构化输出（JSON Schema 或 JSON 模式）不能模拟续写"
	}
	if len(request.Tools.Google) > 0 || request.Tools.GoogleSearch != nil {
		return nil, "请求带 Google 内置工具（联网搜索、URL Context、地图、代码执行），来源与引用位置按单轮给出，不能模拟续写"
	}
	total := request.LongOutputDefaultTokens
	if clientLimit {
		total = *config.MaxOutputTokens
	} else if total <= 0 {
		return nil, "客户端没有给出最大输出且 LONG_OUTPUT_MAX_TOKENS=0（不模拟续写）"
	}
	if total <= defaults.MaxOutputTokens {
		return nil, fmt.Sprintf("总上限 %d 不超过模型单轮上限 %d", total, defaults.MaxOutputTokens)
	}
	// 原样以模型轮结尾与客户端预填充共用名单与拒绝记忆（prefill.go）：近期在 Playground 被拒过的模型直接补用户消息
	prefill := NativePrefillModel(modelID) && !prefillRecentlyRejected(ChannelPlayground, modelID, time.Now())
	return &longOutputPlan{model: modelID, total: total, roundLimit: defaults.MaxOutputTokens, prefill: prefill}, ""
}

// structuredOutput 判断请求要求结构化输出：带 response schema，或输出 MIME 类型不是普通文本（application/json、text/x.enum 等）
func structuredOutput(config GenerationConfig) bool {
	if len(config.ResponseSchema) > 0 {
		return true
	}
	mime := strings.ToLower(strings.TrimSpace(config.ResponseMIMEType))
	return mime != "" && mime != "text/plain"
}

// shiftGrounding 把模拟续写后续轮次的搜索来源（groundingMetadata）里正文片段的位置换算到合并后的正文：上游按本轮的正文给出位置
// （字节），offset 为本轮正文第 0 字节在合并正文中的位置（之前各轮放行的正文字节数减去接缝去掉的重复字节数）。没有位置（EndIndex 为 0）
// 的片段原样保留。Playground 的引用（citationMetadata）只有地址与标题、不带位置，不需要换算
func shiftGrounding(grounding GroundingMetadata, offset int) *GroundingMetadata {
	if offset != 0 && len(grounding.Supports) > 0 {
		supports := slices.Clone(grounding.Supports)
		for index := range supports {
			if segment := &supports[index].Segment; segment.EndIndex > 0 {
				segment.StartIndex = max(0, segment.StartIndex+offset)
				segment.EndIndex = max(0, segment.EndIndex+offset)
			}
		}
		grounding.Supports = supports
	}
	return &grounding
}

// mode 返回续写方式
func (plan *longOutputPlan) mode() string {
	if plan.official {
		return ContinuationOfficial
	}
	return ContinuationSimulated
}

// prefillMode 返回模拟续写下一轮的接续方式
func (plan *longOutputPlan) prefillMode() string {
	if plan.official {
		return ""
	}
	if plan.prefill {
		return ContinuationPrefillNative
	}
	return ContinuationPrefillPrompt
}

// firstRequest 返回第一轮的请求：官网续写按第 0 轮编码；模拟续写第一轮的输出上限为 min(单轮上限, 总上限)（与普通请求按模型上限发送相同）
func (plan *longOutputPlan) firstRequest(request GenerateRequest) GenerateRequest {
	if plan.official {
		request.continuation = &officialContinuation{total: plan.total}
		return request
	}
	limit := min(plan.roundLimit, plan.total)
	request.Config.MaxOutputTokens = &limit
	request.MinOutputTokens = 0
	return request
}

// officialRound 返回官网续写第 round 轮（从 0 计）的请求
func (plan *longOutputPlan) officialRound(request GenerateRequest, round int, token string) GenerateRequest {
	request.continuation = &officialContinuation{round: round, token: token, total: plan.total}
	return request
}

// simulatedRound 返回模拟续写下一轮的请求：已输出的正文作为模型轮接在历史末尾（历史本来以模型轮结尾时并进那一轮），
// 不能原生预填充时再补一条请模型接着写的用户消息；输出上限为 limit，思考强度降到最低（续写只需要接着写，思考只保留在第一轮）
func (plan *longOutputPlan) simulatedRound(request GenerateRequest, text string, limit int64) GenerateRequest {
	contents := slices.Clone(request.Contents)
	if count := len(contents); count > 0 && contents[count-1].Role == RoleAssistant {
		last := contents[count-1]
		last.Parts = append(slices.Clone(last.Parts), Part{Text: text})
		contents[count-1] = last
	} else {
		contents = append(contents, Content{Role: RoleAssistant, Parts: []Part{{Text: text}}})
	}
	if !plan.prefill {
		contents = append(contents, Content{Role: RoleUser, Parts: []Part{{Text: PrefillContinuationPrompt}}})
	}
	request.Contents = contents
	request.Config.MaxOutputTokens = &limit
	request.MinOutputTokens = 0
	request.Config = lowerContinuationThinking(request.Config)
	return request
}

// lowerContinuationThinking 把模拟续写后续轮次的思考强度降到最低，只降不升：客户端已经要求不思考或最低思考（思考强度 none、minimal，
// 或思考预算 0 到 128，即 minimal 换算的预算）时保留客户端的设置；否则按 minimal 发送（只支持预算的模型换算为 128，见 encodeGenerationConfig）。
// 原先一律改成 minimal，客户端关闭思考（预算 0）时只支持预算的模型反而从第二轮起按 128 思考
func lowerContinuationThinking(config GenerationConfig) GenerationConfig {
	switch normalizeReasoningEffort(config.ReasoningEffort) {
	case "none", "minimal":
		return config
	}
	if budget := config.ThinkingBudget; budget != nil && *budget >= 0 && *budget <= thinkingBudgetForLevel(4) {
		return config
	}
	config.ReasoningEffort = "minimal"
	config.ThinkingBudget = nil
	return config
}

// completeOutputTokens 在上游没有给出正文 token 时按总量减去输入、工具与思考补上
func completeOutputTokens(usage Usage) Usage {
	if usage.OutputTokensMissing {
		usage.OutputTokens = max(0, usage.TotalTokens-usage.InputTokens-usage.ToolTokens-usage.ReasoningTokens)
		usage.OutputTokensMissing = false
	}
	return usage
}

// mergeContinuedUsage 合并续写前后两轮的用量：正文、思考与工具用量逐轮累加。官网续写以新一轮为基础（与上游一致：输入取新一轮，
// 总量为新一轮总量加之前各轮的这三项）；模拟续写以第一轮为基础：后续轮次的输入含已输出的正文（作为模型轮发回上游），
// 不是客户端的输入，输入取第一轮，总量为之前的总量加新一轮的这三项（各轮实际的输入写在请求日志的每轮进度里）
func mergeContinuedUsage(previous Usage, next Usage, official bool) Usage {
	previous = completeOutputTokens(previous)
	next = completeOutputTokens(next)
	merged := Usage{
		OutputTokens:    previous.OutputTokens + next.OutputTokens,
		ReasoningTokens: previous.ReasoningTokens + next.ReasoningTokens,
		ToolTokens:      previous.ToolTokens + next.ToolTokens,
	}
	if official {
		merged.InputTokens = next.InputTokens
		merged.TotalTokens = next.TotalTokens + previous.OutputTokens + previous.ReasoningTokens + previous.ToolTokens
	} else {
		merged.InputTokens = previous.InputTokens
		merged.TotalTokens = previous.TotalTokens + next.OutputTokens + next.ReasoningTokens + next.ToolTokens
	}
	return merged
}

// seamGuard 为模拟续写新一轮开头的接缝处理：扣住开头最多 seamWindow 字节的正文，与已输出正文的末尾比对，去掉重复的部分后放行
type seamGuard struct {
	tail    string
	pending strings.Builder
	// held 为最后一个被扣住的正文事件（放行时沿用它的模型名与累计正文数）
	held   Event
	active bool
	// trimmed 为比对后去掉的重复字节数（本轮正文开头与已输出正文末尾相同的部分）
	trimmed int
}

// newSeamGuard 以已输出正文的末尾（最多 seamWindow 字节）建立接缝处理
func newSeamGuard(text string) *seamGuard {
	if len(text) > seamWindow {
		text = text[len(text)-seamWindow:]
	}
	return &seamGuard{tail: text, active: text != ""}
}

// hold 扣住一个正文事件，返回是否已凑够比对窗口
func (seam *seamGuard) hold(event Event) bool {
	seam.pending.WriteString(event.Text)
	seam.held = event
	return seam.pending.Len() >= seamWindow
}

// release 比对并返回放行的正文事件（没有可放行的正文时 ok 为假）；之后不再扣住
func (seam *seamGuard) release() (Event, bool) {
	if !seam.active {
		return Event{}, false
	}
	seam.active = false
	text := seam.pending.String()
	seam.pending.Reset()
	if text == "" {
		return Event{}, false
	}
	seam.trimmed = seamOverlap(seam.tail, text)
	text = text[seam.trimmed:]
	if text == "" {
		return Event{}, false
	}
	event := seam.held
	event.Text = text
	return event, true
}

// seamOverlap 返回 next 开头与 tail 末尾重复的最长字节数（不少于 seamMinOverlap，否则为 0）。tail 与 next 都是合法 UTF-8：
// 相同的字节序列在 next 中从字符边界开始、以完整字符结束，去掉后剩下的仍是合法 UTF-8
func seamOverlap(tail string, next string) int {
	for length := min(len(tail), len(next)); length >= seamMinOverlap; length-- {
		if strings.HasSuffix(tail, next[:length]) {
			return length
		}
	}
	return 0
}

// longRound 为续写循环中一轮的观察结果
type longRound struct {
	usage  *Usage
	finish *Event
	parts  generatedOutputParts
	// textBytes 为这一轮放行的正文字节数；tools 为有工具调用；otherOutput 为有代码执行或媒体输出；grounding 为有搜索来源
	// （groundingMetadata：来源列表与片段位置按单轮给出，合并不了）
	textBytes   int
	tools       bool
	otherOutput bool
	grounding   bool
	// received 为这一轮收到过用量与结束之外的事件（流尾拒绝原生续写时据此判断这一轮还没有任何输出）
	received bool
}

// usageOrEstimate 返回这一轮的用量：上游没有给出时按本地估算（第一轮估算输入，之后各轮只估算输出）
func (round *longRound) usageOrEstimate(request GenerateRequest, first bool) Usage {
	if round.usage != nil {
		return completeOutputTokens(*round.usage)
	}
	estimate := localCompleteUsage(request, round.parts)
	if !first {
		estimate.InputTokens, estimate.ToolTokens = 0, 0
		estimate.TotalTokens = estimate.OutputTokens + estimate.ReasoningTokens
	}
	return *estimate
}

// longOutputRounds 记录一次 Generate 里 long- 请求的续写是否已决定发第二轮：之后 Generate 不再重新生成（重新生成只在第一轮、
// 还没向下游交出任何事件时发生），也不再按原生续写被拒改回补用户消息（续写轮内自己处理，见 decodeLongRounds）。
// delivered 为 Generate 已经向下游交出过事件（与它判断能否重新生成的标记相同：隐藏的思考、停止序列暂存的正文不算）。
// 每次 Generate 一个，重新生成的各次发送共用
type longOutputRounds struct {
	continued atomic.Bool
	delivered atomic.Bool
}

// started 判断续写已决定发第二轮
func (rounds *longOutputRounds) started() bool {
	return rounds != nil && rounds.continued.Load()
}

// markDelivered 记下 Generate 已经向下游交出过事件
func (rounds *longOutputRounds) markDelivered() {
	if rounds != nil {
		rounds.delivered.Store(true)
	}
}

// mayRegenerate 判断只有一轮就结束的第一轮可能被 Generate 重新生成放弃：还没向下游交出任何事件，结束原因又是 Generate 因之重新生成的
// （regenerableFinish：函数调用格式错误、意外的工具调用）
func (rounds *longOutputRounds) mayRegenerate(finishReason string) bool {
	return rounds != nil && !rounds.delivered.Load() && regenerableFinish(finishReason)
}

// decodeLongRounds 解码 long- 请求的第一轮并按计划续写（见文件开头说明），各轮输出依次交给 emit；用量与结束事件只在最后发出一次。
// ctx 为 Generate 的原始上下文（续写的后续轮次在它上面发送，不用重发的上下文，见文件开头说明），rounds 为本次 Generate 的续写记录
func (c *Client) decodeLongRounds(
	ctx context.Context, plan *longOutputPlan, request GenerateRequest, defaults GenerationDefaults, runtime RequestContext,
	source io.Reader, emit func(Event) error, rounds *longOutputRounds,
) error {
	var bodies []io.Closer
	defer func() {
		for _, body := range bodies {
			_ = body.Close()
		}
	}()
	var merged *Usage
	var text strings.Builder
	// output 为之前各轮的输出 token（正文与思考），计入总上限
	var output int64
	roundLimit := min(plan.roundLimit, plan.total)
	// roundCtx 为第二轮起当前这一轮的上下文（等号池准入、发送与关闭响应体都用它）：生成服务经 ContinuationProgress.Interrupt
	// 结束这一轮时取消它，续写按这一轮失败体面结束；ctx 本身结束（客户端断开、取消）时随之结束
	roundCtx, cancelRound := ctx, context.CancelCauseFunc(func(error) {})
	defer func() { cancelRound(nil) }()
	// roundError 在这一轮被生成服务结束时把错误换成结束的原因（*ContinuationInterruptedError），其他情形原样返回
	roundError := func(err error) error {
		var interrupted *ContinuationInterruptedError
		if roundCtx.Err() != nil && errors.As(context.Cause(roundCtx), &interrupted) {
			return interrupted
		}
		return err
	}
	// mergeRound 把一轮的用量并入合并用量与累计输出，返回这一轮的用量
	mergeRound := func(state *longRound, decoder *FrameDecoder, first bool) Usage {
		if state.usage == nil && decoder.usage != nil {
			// 官网续写转入下一轮的这一轮不发用量事件，用量留在解码器里
			usage := *decoder.usage
			state.usage = &usage
		}
		usage := state.usageOrEstimate(request, first)
		output += usage.OutputTokens + usage.ReasoningTokens
		switch {
		case merged != nil:
			value := mergeContinuedUsage(*merged, usage, plan.official)
			merged = &value
		case state.usage != nil:
			value := *state.usage
			merged = &value
		default:
			merged = &usage
		}
		return usage
	}
	// sendRound 发送续写的第 round 轮（从 1 计，至少第 2 轮）：先为租约预占、等号池准入，再发送
	sendRound := func(round int, nextRequest GenerateRequest) (*RPCResponse, error) {
		if err := waitNextRound(roundCtx, plan, round); err != nil {
			return nil, err
		}
		reportContinuation(ctx, ContinuationProgress{Stage: ContinuationSending, Mode: plan.mode(), Round: round, Prefill: plan.prefillMode()})
		return c.sendLongRound(roundCtx, nextRequest, defaults, runtime)
	}
	// prefillRejected 处理模拟续写的这一轮原样以模型轮结尾被上游拒绝（发送时的 HTTP 400 或流尾 400，判断与客户端预填充相同，见 prefill.go）：
	// 记住这个模型与通道，之后各轮与之后的请求直接补用户消息；在这一轮内改为补用户消息重发（不换号，重发前照样重新预占、等号池准入，
	// 被拒的那次已经计入账号每分钟窗口）。返回重发的响应
	prefillRejected := func(round int, cause error) (*RPCResponse, error) {
		plan.prefill = false
		rememberPrefillRejected(ChannelPlayground, plan.model, time.Now())
		ReportRequestNote(ctx, fmt.Sprintf("自动续写第 %d 轮：", round)+prefillFallbackNote(plan.model, ChannelPlayground, cause))
		return sendRound(round, plan.simulatedRound(request, text.String(), roundLimit))
	}
	// attach 把续写一轮的响应接上：取消这一轮时关闭它的响应体，整个请求结束时一并关闭
	attach := func(response *RPCResponse) {
		body := response.Body
		stopClose := context.AfterFunc(roundCtx, func() {
			_ = body.Close()
		})
		bodies = append(bodies, closerFunc(func() error {
			stopClose()
			return body.Close()
		}))
		source = observeStreamActivity(roundCtx, body)
	}
	for round := 0; ; round++ {
		decoder := NewFrameDecoder()
		if plan.official {
			decoder.continuable = true
			decoder.continueAtMaxTokens = int64(round+1)*continuationRoundTokens < plan.total
		}
		simulatedFollowUp := !plan.official && round > 0
		var seam *seamGuard
		// roundBase 为之前各轮放行的正文字节数（模拟续写的后续轮次据此换算搜索来源里正文片段的位置）
		roundBase := text.Len()
		if simulatedFollowUp {
			seam = newSeamGuard(text.String())
		}
		state := &longRound{}
		// emitFailed 为 emit 本身返回了错误（客户端已断开、命中停止序列）：这时原样返回，不按续写失败处理
		emitFailed := false
		pass := func(event Event) error {
			state.parts.observe(event)
			switch event.Kind {
			case EventText:
				state.textBytes += len(event.Text)
				if !plan.official {
					text.WriteString(event.Text)
				}
			case EventToolCall:
				state.tools = true
			case EventMedia, EventExecutableCode, EventCodeExecutionResult:
				state.otherOutput = true
			case EventGrounding:
				state.grounding = true
			}
			if err := emit(event); err != nil {
				emitFailed = true
				return err
			}
			return nil
		}
		releaseSeam := func() error {
			if seam == nil {
				return nil
			}
			event, ok := seam.release()
			if !ok {
				return nil
			}
			return pass(event)
		}
		forward := func(event Event) error {
			switch event.Kind {
			case EventUsage:
				if event.Usage != nil {
					usage := *event.Usage
					state.usage = &usage
				}
				return nil
			case EventFinish:
				finish := event
				state.finish = &finish
				return nil
			}
			state.received = true
			// 各轮上游报告的累计正文数（UpstreamOutputTokens）原样交给降级判定：判定窗口在第一轮内就结束，上游结束时的复核取合并后的用量
			if simulatedFollowUp {
				switch event.Kind {
				case EventReasoning, EventThoughtSignature:
					// 后续轮次的思考与独立签名不输出：思考只保留在第一轮
					return nil
				case EventToolCall:
					// 工具调用的签名保留（下一轮对话发回上游时需要）
				default:
					event.ThoughtSignature = ""
				}
				if event.Kind == EventText && seam.active {
					if seam.hold(event) {
						return releaseSeam()
					}
					return nil
				}
				if err := releaseSeam(); err != nil {
					return err
				}
				if event.Kind == EventGrounding && event.Grounding != nil {
					// 正文片段的位置按本轮正文给出：换算到合并后的正文（接缝去掉的重复部分在比对放行后才知道，非正文事件之前已经放行）。
					// 有搜索来源的一轮之后不再续写，合并后的回复里最多只有一轮的来源
					event.Grounding = shiftGrounding(*event.Grounding, roundBase-seam.trimmed)
				}
			}
			return pass(event)
		}
		// 流尾 google.rpc 状态在这一轮的帧之后、续写判断之前（DecodeGenerateStream 内）处理：流尾是错误时不论帧里是否要求续写都按错误处理
		err := DecodeGenerateStream(source, decoder, forward)
		if err == nil {
			err = releaseSeam()
		}
		if err == nil {
			err = decoder.End()
		}
		if err != nil {
			if round == 0 || emitFailed || ctx.Err() != nil {
				// 第一轮失败与普通请求相同；客户端断开、命中停止序列或请求已结束时原样返回
				return err
			}
			if !plan.official && plan.prefill && !state.received && roundCtx.Err() == nil && prefillRejectedError(err) {
				// 流尾 400：上游不接受以模型轮结尾的请求（这一轮还没有任何输出），与发送时被拒一样在这一轮内改补用户消息重发
				response, sendErr := prefillRejected(round+1, err)
				if sendErr != nil {
					if ctx.Err() != nil {
						return sendErr
					}
					return finishFailedRound(ctx, plan, round+1, true, roundError(sendErr), merged, output, emit)
				}
				attach(response)
				round--
				continue
			}
			// 生成服务结束了这一轮（本服务的期限到期）时按结束的原因报告
			err = roundError(err)
			// 续写的后续轮次输出中途失败（连接重置、帧解析错误、流内错误帧或流尾错误、没有完成帧）：先交出扣住待比对接缝的正文，
			// 再按 max_tokens 结束，这一轮的用量按已收到的输出估算
			if releaseErr := releaseSeam(); releaseErr != nil {
				return releaseErr
			}
			mergeRound(state, decoder, false)
			return finishFailedRound(ctx, plan, round+1, false, err, merged, output, emit)
		}
		usage := mergeRound(state, decoder, round == 0)
		roundOutput := usage.OutputTokens + usage.ReasoningTokens
		finishReason := ""
		if state.finish != nil {
			finishReason = state.finish.FinishReason
		} else if decoder.continuationRequired {
			finishReason = decodeFinishReason(decoder.finishCode)
		}
		next, stop := false, ""
		switch {
		case plan.official && !decoder.continuationRequired:
			stop = "上游结束"
		case plan.official && decoder.continuation == "":
			stop = "上游要求续写但没有续写 token，按 max_tokens 结束"
		case !plan.official && finishReason != "max_tokens":
			stop = "上游结束"
		case !plan.official && state.tools:
			stop = "本轮有工具调用，不续写"
		case !plan.official && state.otherOutput:
			stop = "本轮有代码执行或媒体输出，不续写"
		case !plan.official && state.grounding:
			stop = "本轮有搜索来源（groundingMetadata），不续写"
		case !plan.official && state.textBytes == 0:
			stop = "本轮没有新的正文（思考占满单轮上限），不续写"
		case !plan.official && roundLimit < plan.roundLimit, output >= plan.total:
			stop = "达到输出总上限"
		case round+1 >= longOutputMaxRounds:
			stop = "达到最多轮数"
		default:
			next = true
		}
		progress := ContinuationProgress{
			Stage: ContinuationRoundEnd, Mode: plan.mode(), Round: round + 1, RoundFinish: finishReason,
			RoundInput: usage.InputTokens, RoundOutput: roundOutput, Output: output, Total: plan.total, Next: next, Stop: stop,
		}
		if next {
			// 决定发第二轮起：本次 Generate 不再重新生成（见 longOutputRounds）
			rounds.continued.Store(true)
			progress.Prefill = plan.prefillMode()
			// 下一轮的上下文：上一轮已经读完，结束它的上下文（关闭它的响应体）
			cancelRound(nil)
			var cancel context.CancelCauseFunc
			roundCtx, cancel = context.WithCancelCause(ctx)
			cancelRound = cancel
			progress.Interrupt = func(cause error) {
				cancel(&ContinuationInterruptedError{Cause: cause})
			}
		}
		// 第一轮就结束、还可能被重新生成放弃时不报告这一轮结束：先写“下一步=结束”、重新生成后又从第 1 轮写起会让同一请求的时间线
		// 前后矛盾；不重新生成时只有一轮，等同普通请求，最终结果与不重新生成的原因由 Generate 的请求说明与 app 层写清（与第一轮失败时
		// 不报告相同）
		if next || round > 0 || !rounds.mayRegenerate(finishReason) {
			reportContinuation(ctx, progress)
		}
		if !next {
			if merged != nil && (round > 0 || state.usage != nil) {
				if err := emit(Event{Kind: EventUsage, Usage: merged}); err != nil {
					return err
				}
			}
			finish := Event{Kind: EventFinish, FinishReason: "max_tokens"}
			if state.finish != nil {
				finish = *state.finish
			}
			if finish.FinishReason == "continuation" {
				finish.FinishReason = "max_tokens"
			}
			return emit(finish)
		}
		var nextRequest GenerateRequest
		if plan.official {
			nextRequest = plan.officialRound(request, round+1, decoder.continuation)
		} else {
			roundLimit = min(plan.roundLimit, plan.total-output)
			nextRequest = plan.simulatedRound(request, text.String(), roundLimit)
		}
		response, err := sendRound(round+2, nextRequest)
		if err != nil && !plan.official && plan.prefill && roundCtx.Err() == nil && prefillRejectedError(err) {
			// 发送时上游不接受以模型轮结尾的请求：在这一轮内改为补用户消息重发
			response, err = prefillRejected(round+2, err)
		}
		if err != nil {
			if ctx.Err() != nil {
				return err
			}
			// 等号池准入或发送时失败，或这一轮被生成服务结束（本服务的期限到期）
			return finishFailedRound(ctx, plan, round+2, true, roundError(err), merged, output, emit)
		}
		attach(response)
	}
}

// finishFailedRound 在续写的第 round 轮（从 1 计，至少第 2 轮）发送失败（sending）或输出中途失败时结束整个请求：不报错，
// 报告 ContinuationFailed（生成服务写日志、照常处理账号状态），再发合并后的用量与 max_tokens 结束。已输出的内容（含前面各轮）
// 由此完整交给客户端：按错误结束时非流式会丢掉全部已生成的内容、流式没有用量，比不带 long- 的请求更差。
// 续写后的输入超过账号输入上限（模拟续写把已输出的正文发回上游，部分账号的输入上限为 131072）按停止续写处理
func finishFailedRound(
	ctx context.Context, plan *longOutputPlan, round int, sending bool, err error, merged *Usage, output int64, emit func(Event) error,
) error {
	stop := fmt.Sprintf("续写第 %d 轮输出中断，按 max_tokens 结束（已输出的内容完整返回）", round)
	if sending {
		stop = fmt.Sprintf("续写第 %d 轮发送失败，按 max_tokens 结束（已输出的内容完整返回）", round)
	}
	var interrupted *ContinuationInterruptedError
	switch {
	case errors.As(err, &interrupted):
		stop = fmt.Sprintf("续写第 %d 轮被本服务结束（%s），按 max_tokens 结束（已输出的内容完整返回）", round, interrupted.Error())
	case InputTokenLimitError(err):
		stop = "续写后的输入超过账号输入上限，不再续写，按 max_tokens 结束"
	}
	reportContinuation(ctx, ContinuationProgress{
		Stage: ContinuationFailed, Mode: plan.mode(), Round: round, Output: output, Total: plan.total, Stop: stop, Sending: sending, Err: err,
	})
	if merged != nil {
		if err := emit(Event{Kind: EventUsage, Usage: merged}); err != nil {
			return err
		}
	}
	return emit(Event{Kind: EventFinish, FinishReason: "max_tokens", ContinuationErr: err})
}

// sendLongRound 编码并发送续写的下一轮（同一租约，经 doProtected 照常取新的 WAA proof、计入每账号每分钟上限）
func (c *Client) sendLongRound(ctx context.Context, request GenerateRequest, defaults GenerationDefaults, runtime RequestContext) (*RPCResponse, error) {
	body, err := EncodeGenerateContentRequest(request, defaults, runtime)
	if err != nil {
		return nil, err
	}
	return c.doProtected(ctx, request, body)
}

// waitNextRound 在发送续写的第 round 轮之前为租约重新预占并等号池准入（与重新生成共用的准入，见 resend_admission.go）：这一轮同样计入
// 账号每分钟上限与出口令牌桶，不等就发多半换来 429；等待中每 continuationWaitStep 报告一次进度（含与重新生成不准入时写法相同的原因）。
// 没有租约（测试替身）时不等。ctx（这一轮的上下文）已经结束时返回它的错误，不再发送：生成服务在决定续写时就结束了这一轮
// （离绝对总时长上限不到一个空闲期限）
func waitNextRound(ctx context.Context, plan *longOutputPlan, round int) error {
	lease, ok := AccountLeaseFromContext(ctx)
	if !ok {
		return ctx.Err()
	}
	lease.ReserveContinuation()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		admission := lease.continuationAdmission(plan.model, time.Now())
		if admission.wait <= 0 {
			return nil
		}
		reportContinuation(ctx, ContinuationProgress{
			Stage: ContinuationWaiting, Mode: plan.mode(), Round: round, Wait: admission.wait, WaitReason: admission.reason, WaitDetail: admission.detail,
		})
		timer := time.NewTimer(min(admission.wait, continuationWaitStep))
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// closerFunc 把函数包装成 io.Closer
type closerFunc func() error

func (f closerFunc) Close() error {
	return f()
}
