package aistudio

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// FrameDecoder 将 field 1 repeated 帧转换为规范事件
type FrameDecoder struct {
	usage     *Usage
	finished  bool
	lastFrame json.RawMessage
	// continuable 表示本轮按官网自动续写编码（long- 请求、模型有能力码 91，见 continuation.go）：读取候选 field 9 的续写 token
	continuable bool
	// continueAtMaxTokens 表示本轮的累计输出上限低于请求的输出总上限：以 max_tokens 结束且同一帧带续写 token 时也转入下一轮
	continueAtMaxTokens bool
	// latestToken 为本轮已出现的最近一个续写 token
	latestToken string
	// continuation 为本轮结束后下一轮使用的续写 token；continuationRequired 表示上游以终止原因 22 或带 token 的 max_tokens
	// 要求续写（本轮不形成终态，不发出用量与结束事件）
	continuation         string
	continuationRequired bool
	// finishCode 为本轮完成帧的终止原因编号（续写循环判断是否转入下一轮）
	finishCode int64
	// tolerance 为本请求的解码容错记录（见 decode_tolerance.go）；没有设置时第一次容错时新建，只在这个解码器内去重
	tolerance *decodeTolerance
	// pendingFinish 表示出现过读不出的结束原因、还没有能识别的结束原因：不在中间帧定终态，流正常结束时由 flushPendingFinish 按
	// unspecified 结束；pendingFinishMessage 为那一帧的结束说明
	pendingFinish        bool
	pendingFinishMessage string
	// producedContent 表示本解码器已产出过内容事件（正文、思考、工具调用、媒体等）：之后读不出的 promptFeedback 不再按输入拦截
	// （正文已经出来，不可能是输入拦截；也不会再重新生成），按未拦截继续
	producedContent bool
	// live 表示解码 Live 会话的 Part（parseBidiContent）：inline media 没有 MIME 时保留为空（Live 客户端按缺省的 PCM 处理），不按文件头判断
	live bool
}

// gcMethod 为 Playground GenerateContent 写进协议证据与容错日志的方法名
const gcMethod = "GenerateContent"

// tol 返回本解码器的容错记录
func (d *FrameDecoder) tol() *decodeTolerance {
	if d.tolerance == nil {
		d.tolerance = newDecodeTolerance(nil, "")
	}
	return d.tolerance
}

// NewFrameDecoder 创建单次 GenerateContent 的有状态解码器
func NewFrameDecoder() *FrameDecoder {
	return &FrameDecoder{}
}

// Decode 解码一条 repeated 流帧。结构性问题（field 1 里的 null 或稀疏元素、空帧、候选为空数组）跳过这一帧或这一项，多个候选取第一个；
// 引用、grounding 与用量解码失败只丢掉这一项（用量走本地估算）；帧、候选、content 或正文不是能读的形状时仍按协议证据错误返回
func (d *FrameDecoder) Decode(raw json.RawMessage) ([]Event, error) {
	d.lastFrame = append(json.RawMessage(nil), raw...)
	if isJSONNull(raw) {
		// field 1 里的 null（[[,<帧>]] 经稀疏补全后同样是 null）：没有内容，跳过
		d.tol().note(gcMethod, "$[0][]", "field 1 的元素为 null，已跳过", raw)
		return nil, nil
	}
	frame, err := rawArray(raw, "$[0][]", raw)
	if err != nil {
		return nil, withMethod(err, gcMethod)
	}
	if len(frame) == 0 {
		d.tol().note(gcMethod, "$[0][]", "空帧，已跳过", raw)
		return nil, nil
	}
	if isJSONNull(frame[0]) {
		if feedbackRaw := rawAt(frame, 1); !isJSONNull(feedbackRaw) {
			if err := d.decodePromptFeedback(feedbackRaw, raw); err != nil {
				return nil, err
			}
		}
		return nil, nil
	}
	candidates, err := rawArray(frame[0], "$[0][][0]", raw)
	if err != nil {
		return nil, withMethod(err, gcMethod)
	}
	if len(candidates) == 0 {
		// 候选为空数组：这一帧没有内容。promptFeedback 与 frame 0 为 null 时一样读取：真正的输入拦截写成 [[],[1,…]] 时照旧按拦截结束，
		// 不跳过（跳过后流里没有完成帧，会按“没有完成帧”在同账号重新生成，最后对外 500 而不是 400）；没有拦截时用量照常读取。
		// 跳过候选的容错日志在判断拦截之后写：真正的拦截不再多写一行“已跳过候选”
		if feedbackRaw := rawAt(frame, 1); !isJSONNull(feedbackRaw) {
			if err := d.decodePromptFeedback(feedbackRaw, raw); err != nil {
				return nil, err
			}
		}
		d.tol().note(gcMethod, "$[0][][0]", "候选为空数组，已跳过候选", frame[0])
		d.decodeFrameUsage(frame, nil, raw)
		return nil, nil
	}
	if len(candidates) > 1 {
		// 其余候选也必须是数组或 null：有标量（如 [content, 1]）说明这一层不是候选列表（嵌套层次变了），取第一个会把错位的数据当正文交出，
		// 仍按协议证据错误返回
		for index, extra := range candidates[1:] {
			if _, err := rawArray(extra, fmt.Sprintf("$[0][][0][%d]", index+1), raw); err != nil {
				return nil, &ProtocolEvidenceError{
					Method: gcMethod, Path: fmt.Sprintf("$[0][][0][%d]", index+1), Detail: "候选列表里有不是数组的元素，嵌套层次与现场确认的不同",
					Raw: cloneRaw(raw),
				}
			}
		}
		// 本服务只请求一个候选（OpenAI n、Gemini candidateCount 不是 1 时按 400 拒绝），客户端也只要一个：取第一个
		d.tol().note(gcMethod, "$[0][][0]", "多个候选，只取第一个", toleranceText("候选数=%d", len(candidates)))
	}
	candidate, err := rawArray(candidates[0], "$[0][][0][0]", raw)
	if err != nil {
		return nil, withMethod(err, gcMethod)
	}
	finishRaw := rawAt(candidate, 1)
	frameToken := ""
	if d.continuable {
		// 候选 field 9（JSON 索引 8）为官网自动续写的续写 token：不是字符串时忽略
		_ = json.Unmarshal(rawAt(candidate, 8), &frameToken)
		if frameToken != "" {
			d.latestToken = frameToken
		}
	}
	events := make([]Event, 0, 4)
	if len(candidate) > 0 && !isJSONNull(candidate[0]) {
		contentEvents, err := d.decodeContent(candidate[0], raw)
		if err != nil {
			return nil, err
		}
		if len(contentEvents) > 0 {
			d.producedContent = true
		}
		events = append(events, contentEvents...)
	}
	if citationsRaw := rawAt(candidate, 6); !isJSONNull(citationsRaw) {
		citations := d.decodeCitations(citationsRaw)
		for index := range citations {
			citation := citations[index]
			events = append(events, Event{Kind: EventCitation, Citation: &citation})
		}
	}
	if groundingRaw := rawAt(candidate, 7); !isJSONNull(groundingRaw) {
		if grounding, ok := decodeGroundingMetadata(groundingRaw, "$[0][][0][0][7]", raw, d.tol()); ok {
			events = append(events, Event{Kind: EventGrounding, Grounding: &grounding})
		}
	}
	d.decodeFrameUsage(frame, events, raw)
	if isJSONNull(finishRaw) {
		return events, nil
	}
	finishCode, finishReason, state := d.decodeFinish(finishRaw)
	// candidate field 4 为上游对结束原因的说明（finishMessage），只是诊断信息：不是字符串时忽略
	var finishMessage string
	_ = json.Unmarshal(rawAt(candidate, 3), &finishMessage)
	switch state {
	case finishBlank:
		// 空白的结束原因等同未设置：这一帧不结束
		return events, nil
	case finishUnreadable:
		// 读不出的结束原因不在这一帧定终态（中间帧提前结束会丢掉后面真正的结束原因与续写判断）：记为待定，后面的帧出现能识别的
		// 结束原因时以后者为准，流正常结束时仍只有待定的才按 unspecified 结束（flushPendingFinish）
		if !d.finished && !d.pendingFinish {
			d.pendingFinish = true
			d.pendingFinishMessage = strings.TrimSpace(finishMessage)
		}
		return events, nil
	}
	if d.continuable && !d.finished && (finishCode == 22 || finishCode == 2 && frameToken != "" && d.continueAtMaxTokens) {
		// 官网自动续写：本轮不形成终态，用量（d.usage）与续写 token 交给续写循环，下一轮带 token 接着生成
		d.continuationRequired = true
		d.continuation = d.latestToken
		d.finishCode = finishCode
		d.finished = true
		return events, nil
	}
	if !d.finished {
		d.finishCode = finishCode
		if d.usage != nil {
			usage := *d.usage
			events = append(events, Event{Kind: EventUsage, Usage: &usage})
		}
		events = append(events, Event{Kind: EventFinish, FinishReason: finishReason, FinishMessage: strings.TrimSpace(finishMessage)})
		d.finished = true
	}
	return events, nil
}

// flushPendingFinish 在流正常结束（流尾不是错误）后调用：出现过读不出的结束原因、之后再没有能识别的结束原因时，按 unspecified 结束，
// 交出用量与结束事件；其他情况返回 nil
func (d *FrameDecoder) flushPendingFinish() []Event {
	if d.finished || !d.pendingFinish {
		return nil
	}
	d.finished = true
	d.finishCode = 0
	events := make([]Event, 0, 2)
	if d.usage != nil {
		usage := *d.usage
		events = append(events, Event{Kind: EventUsage, Usage: &usage})
	}
	return append(events, Event{Kind: EventFinish, FinishReason: "unspecified", FinishMessage: d.pendingFinishMessage})
}

// decodeFrameUsage 读取帧 field 3 的用量：挂到本帧事件上供降级判定，完整时记为本轮用量；解码失败时丢掉已记的用量（走本地估算）
func (d *FrameDecoder) decodeFrameUsage(frame []json.RawMessage, events []Event, evidence json.RawMessage) {
	usageRaw := rawAt(frame, 2)
	if isJSONNull(usageRaw) {
		return
	}
	annotateUsageProgress(events, usageRaw)
	usage, complete, ok := decodeUsage(usageRaw, d.tol())
	if !ok {
		d.usage = nil
		return
	}
	if complete {
		d.usage = &usage
	}
}

// finishReasonCodes 为结束原因名称（Gemini API 枚举名去掉 FINISH_REASON_ 前缀后的小写）到编号的换算，上游把结束原因写成名称时用
var finishReasonCodes = func() map[string]int64 {
	codes := make(map[string]int64)
	for code := int64(0); code <= 22; code++ {
		if name := decodeFinishReason(code); !strings.HasPrefix(name, "provider_") {
			codes[name] = code
		}
	}
	return codes
}()

// finishState 为一帧结束原因的读取结果
type finishState int

const (
	// finishRead 为读出了结束原因
	finishRead finishState = iota
	// finishBlank 为空白字符串：等同这一帧没有结束原因
	finishBlank
	// finishUnreadable 为读不出（对象、数组、不像枚举名的字符串）
	finishUnreadable
)

// finishEnumName 匹配枚举样式的结束原因名称（大写字母开头，只有大写字母、数字与下划线）；"model" 这类小写词不是结束原因
var finishEnumName = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)

// decodeFinish 读取候选 field 2 的结束原因：整数按编号；带小数的数与数字字符串按整数换算；名称（如 "STOP"，可带 FINISH_REASON_ 前缀）
// 按表换算，不在表里的枚举样式名称保留为小写名称（编号 -1）；空白字符串等同没有结束原因（finishBlank）；其他写法读不出（finishUnreadable，
// 由 Decode 记为待定）。只有整数是原本的写法，其余写一行容错日志
func (d *FrameDecoder) decodeFinish(raw json.RawMessage) (int64, string, finishState) {
	path := "$[0][][0][0][1]"
	if code, err := rawInt64(raw, path, raw); err == nil {
		return code, decodeFinishReason(code), finishRead
	}
	if code, ok := lenientInt64(raw); ok {
		d.tol().note(gcMethod, path, "结束原因不是整数，按编号换算", raw)
		return code, decodeFinishReason(code), finishRead
	}
	var name string
	if json.Unmarshal(raw, &name) == nil {
		name = strings.TrimSpace(name)
		if name == "" {
			d.tol().note(gcMethod, path, "结束原因为空串，按这一帧没有结束原因", raw)
			return 0, "", finishBlank
		}
		reason := strings.ToLower(strings.TrimPrefix(strings.ToUpper(name), "FINISH_REASON_"))
		if code, ok := finishReasonCodes[reason]; ok {
			d.tol().note(gcMethod, path, "结束原因写成名称，按名称换算", raw)
			return code, reason, finishRead
		}
		if finishEnumName.MatchString(name) {
			d.tol().note(gcMethod, path, "结束原因名称不在表里，保留为小写可读名", raw)
			return -1, reason, finishRead
		}
	}
	d.tol().note(gcMethod, path, "结束原因无法识别，后面没有能识别的结束原因时按 unspecified 结束", raw)
	return 0, "unspecified", finishUnreadable
}

// decodePromptFeedback 解码 frame 0 为 null 或候选为空数组时的 promptFeedback：有拦截原因时返回输入拦截错误；拦截原因为 null、缺位或
// 空白字符串时不是拦截，跳过这一帧继续读取（原先按协议证据错误中止）。promptFeedback 本身不是数组（如 {"blockReason":1}）时读不出
// 有没有拦截：还没有产出内容时与拦截原因无法识别时一样按 unspecified 拦截（按未拦截跳过会让流里没有完成帧，在同账号重新生成）；
// 已产出内容时正文已经出来，不可能是输入拦截，也不会再重新生成，按未拦截继续
func (d *FrameDecoder) decodePromptFeedback(raw json.RawMessage, evidence json.RawMessage) error {
	feedback, err := rawArray(raw, "$[0][][1]", evidence)
	if err != nil && d.producedContent {
		d.tol().note(gcMethod, "$[0][][1]", "promptFeedback 不是数组，已有正文，按未拦截继续", raw)
		return nil
	}
	if err != nil {
		d.tol().note(gcMethod, "$[0][][1]", "promptFeedback 不是数组，按 unspecified 拦截", raw)
		return &PromptFeedbackError{Reason: "unspecified", Raw: append(json.RawMessage(nil), raw...)}
	}
	reasonRaw := rawAt(feedback, 0)
	var blank string
	if isJSONNull(reasonRaw) || json.Unmarshal(reasonRaw, &blank) == nil && strings.TrimSpace(blank) == "" {
		// 拦截原因为 null、缺位或空白字符串：都是没有拦截原因
		d.tol().note(gcMethod, "$[0][][1][0]", "promptFeedback 没有拦截原因，按未拦截继续", raw)
		return nil
	}
	// 有拦截原因时照旧按拦截处理：整数按编号；带小数的数按编号换算，名称（如 "SAFETY"）按名称，其他写法按 unspecified
	reason := "unspecified"
	var name string
	if code, err := rawInt64(reasonRaw, "$[0][][1][0]", evidence); err == nil {
		reason = decodePromptBlockReason(code)
	} else if code, ok := lenientInt64(reasonRaw); ok {
		d.tol().note(gcMethod, "$[0][][1][0]", "拦截原因不是整数，按编号换算", reasonRaw)
		reason = decodePromptBlockReason(code)
	} else if json.Unmarshal(reasonRaw, &name) == nil && strings.TrimSpace(name) != "" {
		d.tol().note(gcMethod, "$[0][][1][0]", "拦截原因写成名称，按名称处理", reasonRaw)
		reason = buildEnumName(strings.ToUpper(strings.TrimSpace(name)), "BLOCK_REASON_")
	} else {
		d.tol().note(gcMethod, "$[0][][1][0]", "拦截原因无法识别，按 unspecified 拦截", reasonRaw)
	}
	// PromptFeedback field 3 为上游对拦截原因的说明（blockReasonMessage），只是诊断信息：不是字符串时忽略
	var message string
	_ = json.Unmarshal(rawAt(feedback, 2), &message)
	return &PromptFeedbackError{
		Reason: reason, Message: strings.TrimSpace(message), Raw: append(json.RawMessage(nil), raw...),
	}
}

func decodePromptBlockReason(reason int64) string {
	switch reason {
	case 0:
		return "unspecified"
	case 1:
		return "safety"
	case 2:
		return "other"
	case 3:
		return "blocklist"
	case 4:
		return "prohibited_content"
	case 5:
		return "image_safety"
	default:
		return fmt.Sprintf("provider_%d", reason)
	}
}

func decodeFinishReason(code int64) string {
	switch code {
	case 0:
		return "unspecified"
	case 1:
		return "stop"
	case 2:
		return "max_tokens"
	case 3:
		return "safety"
	case 4:
		return "recitation"
	case 5:
		return "other"
	case 6:
		return "language"
	case 7:
		return "blocklist"
	case 8:
		return "prohibited_content"
	case 9:
		return "spii"
	case 10:
		return "malformed_function_call"
	case 11:
		return "image_safety"
	case 12:
		return "unexpected_tool_call"
	case 13:
		return "too_many_tool_calls"
	case 14:
		return "image_prohibited_content"
	case 15:
		return "image_other"
	case 16:
		return "no_image"
	case 17:
		return "image_recitation"
	case 18:
		return "missing_thought_signature"
	case 19:
		// 响应格式异常（Gemini API FINISH_REASON_MALFORMED_RESPONSE，Build 通道经 buildEnumName 得到同一个名字）：AI Studio 页面显示
		// Content blocked，通常没有正文；不算内容拦截，客户端还没收到内容时可以重新生成（见 regenerate.go）。原先为 provider_19
		return "malformed_response"
	case 22:
		// 官网自动续写要求下一轮（只在 long- 请求按续写编码时出现）；续写循环不再续写时按 max_tokens 结束，协议层同样按长度截断映射
		return "continuation"
	default:
		return fmt.Sprintf("provider_%d", code)
	}
}

// End 校验流已经出现正常完成帧
func (d *FrameDecoder) End() error {
	if d.finished {
		return nil
	}
	return &ProtocolEvidenceError{
		Method: "GenerateContent", Path: "$", Detail: "流结束前没有完成帧", Raw: append(json.RawMessage(nil), d.lastFrame...), Err: errMissingFinishFrame,
	}
}

func (d *FrameDecoder) decodeContent(raw json.RawMessage, evidence json.RawMessage) ([]Event, error) {
	content, err := rawArray(raw, "$[0][][0][0][0]", evidence)
	if err != nil {
		return nil, withMethod(err, "GenerateContent")
	}
	if len(content) == 0 {
		return nil, nil
	}
	// 角色只用来核对：缺位、为 null 或不是 "model" 时都按 model 处理（JSON+protobuf 省略末尾未设置的字段时就没有角色）
	var role string
	if roleRaw := rawAt(content, 1); json.Unmarshal(roleRaw, &role) != nil || role != "model" {
		d.tol().note(gcMethod, "$[0][][0][0][0][1]", "响应角色缺失或不是 model，按 model 处理", roleRaw)
	}
	partsRaw := rawAt(content, 0)
	if isJSONNull(partsRaw) {
		return nil, nil
	}
	parts, err := rawArray(partsRaw, "$[0][][0][0][0][0]", evidence)
	if err != nil {
		return nil, withMethod(err, "GenerateContent")
	}
	events := make([]Event, 0, len(parts))
	for index, partRaw := range parts {
		partEvents, err := d.decodePart(partRaw, fmt.Sprintf("$[0][][0][0][0][0][%d]", index), evidence)
		if err != nil {
			return nil, err
		}
		events = append(events, partEvents...)
	}
	return events, nil
}

func (d *FrameDecoder) decodePart(raw json.RawMessage, path string, evidence json.RawMessage) ([]Event, error) {
	part, err := rawArray(raw, path, evidence)
	if err != nil {
		return nil, withMethod(err, "GenerateContent")
	}
	// 思考标记：1/0/true/false 之外的写法按“非 0、非 false 即为真”，读不出时按 false
	thought := false
	if thoughtRaw := rawAt(part, 12); !isJSONNull(thoughtRaw) {
		if thought, err = rawBool(thoughtRaw, path+"[12]", raw); err != nil {
			var ok bool
			if thought, ok = lenientBool(thoughtRaw); ok {
				d.tol().note(gcMethod, path+"[12]", "思考标记的写法不常见，按非 0 即真处理", thoughtRaw)
			} else {
				d.tol().note(gcMethod, path+"[12]", "思考标记无法识别，按 false 处理", thoughtRaw)
			}
		}
	}
	// 思考签名是不透明的字符串：null 为没有签名，其他类型无法回传，忽略
	signature := ""
	if signatureRaw := rawAt(part, 14); !isJSONNull(signatureRaw) {
		if signature, err = rawString(signatureRaw, path+"[14]", raw); err != nil {
			signature = ""
			d.tol().note(gcMethod, path+"[14]", "思考签名不是字符串，已忽略", signatureRaw)
		}
	}
	events := make([]Event, 0, 2)
	emptyText := false
	textEvent := -1
	if textRaw := rawAt(part, 1); !isJSONNull(textRaw) {
		// 正文：数字与布尔值按原文转成文本；对象与数组读不出正文，仍按协议证据错误中止
		text, converted, ok := lenientText(textRaw)
		if !ok {
			return nil, &ProtocolEvidenceError{Method: gcMethod, Path: path + "[1]", Detail: "正文不是字符串，读不出正文", Raw: cloneEvidence(textRaw, raw)}
		}
		if converted {
			d.tol().note(gcMethod, path+"[1]", "正文不是字符串，按原文转成文本", textRaw)
		}
		if text != "" {
			kind := EventText
			if thought {
				kind = EventReasoning
			}
			events = append(events, Event{Kind: kind, Text: text, ThoughtSignature: signature})
			if kind == EventText {
				textEvent = len(events) - 1
			}
		} else {
			emptyText = true
		}
	}
	if inlineRaw := rawAt(part, 2); !isJSONNull(inlineRaw) && !thought {
		media, err := decodeInlineMedia(inlineRaw, path+"[2]", raw, d.tol(), !d.live)
		if err != nil {
			return nil, err
		}
		events = append(events, Event{Kind: EventMedia, Media: &media, ThoughtSignature: signature})
	}
	if codeRaw := rawAt(part, 7); !isJSONNull(codeRaw) {
		code, err := decodeExecutableCode(codeRaw, path+"[7]", raw, d.tol())
		if err != nil {
			return nil, err
		}
		events = append(events, Event{Kind: EventExecutableCode, ExecutableCode: &code, ThoughtSignature: signature})
	}
	if resultRaw := rawAt(part, 8); !isJSONNull(resultRaw) {
		result, err := decodeCodeExecutionResult(resultRaw, path+"[8]", raw, d.tol())
		if err != nil {
			return nil, err
		}
		events = append(events, Event{Kind: EventCodeExecutionResult, CodeExecutionResult: &result, ThoughtSignature: signature})
	}
	if callRaw := rawAt(part, 10); !isJSONNull(callRaw) {
		call, err := decodeFunctionCall(callRaw, path+"[10]", raw, d.tol())
		if err != nil {
			return nil, err
		}
		call.ThoughtSignature = signature
		events = append(events, Event{Kind: EventToolCall, ToolCall: &call, ThoughtSignature: signature})
	}
	if transcriptRaw := rawAt(part, 22); !isJSONNull(transcriptRaw) {
		transcript, err := decodeTranscriptMetadata(transcriptRaw, path+"[22]", raw, d.tol())
		switch {
		case err != nil && textEvent < 0:
			// 转写正文就是这个 Part 的正文：读不出时仍报错
			return nil, err
		case err != nil:
			// 同一 Part 已有正文：转写元数据只是附加信息，读不出时丢掉
			d.tol().note(gcMethod, path+"[22]", "转写元数据读不出，已丢弃（正文照常）", transcriptRaw)
		case textEvent >= 0 && events[textEvent].Text == transcript.Text:
			events[textEvent].Transcript = &transcript
		default:
			events = append(events, Event{
				Kind: EventText, Text: transcript.Text, Transcript: &transcript, ThoughtSignature: signature,
			})
		}
	}
	if len(events) == 0 {
		if signature != "" {
			return []Event{{Kind: EventThoughtSignature, ThoughtSignature: signature}}, nil
		}
		if emptyText {
			return nil, nil
		}
		return nil, nil
	}
	return events, nil
}

// decodeCitations 解码候选 field 7 的 citationMetadata：每项 URI 在索引 2、标题在索引 3。引用是辅助元数据：整段读不出时丢掉，
// 条目缺少 URI（只有起止位置的 [[0,10]] 这种形状）、URI 为 null 或空串时跳过该条目，标题不是字符串时忽略标题
func (d *FrameDecoder) decodeCitations(raw json.RawMessage) []Citation {
	const path = "$[0][][0][0][6]"
	metadata, err := rawArray(raw, path, raw)
	if err != nil {
		d.tol().note(gcMethod, path, "引用元数据不是数组，已丢弃", raw)
		return nil
	}
	entriesRaw := rawAt(metadata, 0)
	if isJSONNull(entriesRaw) {
		return nil
	}
	entries, err := rawArray(entriesRaw, path+"[0]", raw)
	if err != nil {
		d.tol().note(gcMethod, path+"[0]", "引用条目列表不是数组，已丢弃", entriesRaw)
		return nil
	}
	citations := make([]Citation, 0, len(entries))
	for index, entryRaw := range entries {
		entryPath := fmt.Sprintf("%s[0][%d]", path, index)
		entry, err := rawArray(entryRaw, entryPath, raw)
		if err != nil || isJSONNull(entryRaw) {
			d.tol().note(gcMethod, entryPath, "引用条目不是数组，已跳过该条目", entryRaw)
			continue
		}
		var url string
		if json.Unmarshal(rawAt(entry, 2), &url) != nil || strings.TrimSpace(url) == "" {
			d.tol().note(gcMethod, entryPath+"[2]", "引用条目缺少 URI，已跳过该条目", entryRaw)
			continue
		}
		title := ""
		if titleRaw := rawAt(entry, 3); !isJSONNull(titleRaw) && json.Unmarshal(titleRaw, &title) != nil {
			title = ""
			d.tol().note(gcMethod, entryPath+"[3]", "引用标题不是字符串，已忽略", titleRaw)
		}
		citations = append(citations, Citation{URL: url, Title: title})
	}
	return citations
}

// decodeUsage 解码帧 field 3 的用量：输入、正文、总数位于索引 0、1、2，工具与思考位于 7、9。各数按 lenientInt64 读取（整数、带小数的数、
// 数字字符串）；complete 表示输入与总数都在。不是数组或某个数读不出时 ok 为假（写一行容错日志），调用方丢掉用量，走本地估算
func decodeUsage(raw json.RawMessage, tolerance *decodeTolerance) (usage Usage, complete bool, ok bool) {
	const path = "$[0][][2]"
	values, err := rawArray(raw, path, raw)
	if err != nil {
		tolerance.note(gcMethod, path, "用量不是数组，已丢弃，改用本地估算", raw)
		return Usage{}, false, false
	}
	if isJSONNull(rawAt(values, 0)) || isJSONNull(rawAt(values, 2)) {
		return Usage{}, false, true
	}
	number := func(index int, target *int64) bool {
		value := rawAt(values, index)
		if isJSONNull(value) {
			return true
		}
		parsed, err := rawInt64(value, fmt.Sprintf("%s[%d]", path, index), raw)
		if err != nil {
			var ok bool
			if parsed, ok = lenientInt64(value); !ok {
				tolerance.note(gcMethod, fmt.Sprintf("%s[%d]", path, index), "用量不是数字，已丢弃，改用本地估算", raw)
				return false
			}
			tolerance.note(gcMethod, fmt.Sprintf("%s[%d]", path, index), "用量不是整数，按整数换算", value)
		}
		*target = parsed
		return true
	}
	usage.OutputTokensMissing = isJSONNull(rawAt(values, 1))
	for _, field := range []struct {
		index  int
		target *int64
	}{{0, &usage.InputTokens}, {1, &usage.OutputTokens}, {2, &usage.TotalTokens}, {7, &usage.ToolTokens}, {9, &usage.ReasoningTokens}} {
		if !number(field.index, field.target) {
			return Usage{}, false, false
		}
	}
	return usage, true, true
}

// annotateUsageProgress 把这一帧用量数组里的输入数（第 0 项）与累计正文数（第 1 项）挂到本帧的事件上，供降级判定计算出字速度。
// 思考阶段的中间帧只有输入数，正文阶段每帧都带累计正文数；完整用量仍只在完成帧发出
func annotateUsageProgress(events []Event, usageRaw json.RawMessage) {
	if len(events) == 0 {
		return
	}
	var values []json.RawMessage
	if err := json.Unmarshal(usageRaw, &values); err != nil {
		return
	}
	var prompt, output int64
	if len(values) > 0 {
		_ = json.Unmarshal(values[0], &prompt)
	}
	if len(values) > 1 {
		_ = json.Unmarshal(values[1], &output)
	}
	for index := range events {
		events[index].UpstreamPromptTokens = prompt
		events[index].UpstreamOutputTokens = output
	}
}
