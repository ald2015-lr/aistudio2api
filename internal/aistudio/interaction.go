package aistudio

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
)

// interactionThinkingLevels 把 GenerateContent thinking level 换算为 Interaction 枚举
var interactionThinkingLevels = map[int64]int64{4: 1, 1: 2, 2: 3, 3: 4}

// interactionMediaKind 为 Content 一类媒体字段的 MIME 枚举表：name 为写进协议证据的中文名称，枚举为 null 或不在表里时取 unlisted
type interactionMediaKind struct {
	name     string
	codes    map[int64]string
	unlisted string
}

// interactionMediaMIME 为 Content 各媒体字段（按数组下标：1 为 field 2 图片、2 为 field 3 音频、3 为 field 4 文档、4 为 field 5 视频）的
// MIME 枚举表，与官网前端相同（按上游 Mag1cFall/AIStudio2API 45e33b4 移植）：解码输出时枚举为 null 或不在表里取该类的缺省 MIME，
// 不按协议证据错误返回（原先视频已经生成、额度已扣，却因 [null,…] 或表外枚举整次 502）。输入编码用同一张表：Omni 的图片按文件头识别格式后
// 取这里的枚举内联发送（见 interaction_image.go 的 interactionImageInputMIME）；两份官网抓包的输入 PNG 为 1、JPEG 为 2，与表一致
var interactionMediaMIME = map[int]interactionMediaKind{
	1: {name: "图片", codes: map[int64]string{
		1: "image/png", 2: "image/jpeg", 3: "image/webp", 4: "image/heic", 5: "image/heif", 7: "image/gif", 8: "image/bmp", 9: "image/tiff",
	}, unlisted: "image/jpeg"},
	2: {name: "音频", codes: map[int64]string{
		1: "audio/wav", 2: "audio/mp3", 3: "audio/aiff", 4: "audio/aac", 5: "audio/ogg", 6: "audio/flac", 7: "audio/mpeg",
		8: "audio/m4a", 9: "audio/l16", 10: "audio/s16le", 11: "audio/opus", 12: "audio/alaw", 13: "audio/mulaw",
	}, unlisted: "audio/wav"},
	3: {name: "文档", codes: map[int64]string{1: "application/pdf"}, unlisted: "application/pdf"},
	4: {name: "视频", codes: map[int64]string{
		1: "video/mp4", 2: "video/mpeg", 3: "video/mpg", 4: "video/mov", 5: "video/avi", 6: "video/x-flv", 7: "video/webm", 8: "video/wmv", 9: "video/3gpp",
	}, unlisted: "video/mp4"},
}

// interactionStatusHTTP 把流尾 google.rpc 状态码换算为 HTTP 状态
var interactionStatusHTTP = map[int64]int{
	3: http.StatusBadRequest, 4: http.StatusGatewayTimeout, 5: http.StatusNotFound, 7: http.StatusForbidden,
	8: http.StatusTooManyRequests, 9: http.StatusBadRequest, 13: http.StatusInternalServerError,
	14: http.StatusServiceUnavailable, 16: http.StatusUnauthorized,
}

// EncodeCreateInteractionStreamRequest 编码官网 CreateInteractionStream 请求并返回参与 WAA 绑定的文本内容
// stop sequences 不发送：没有 CreateInteractionStream 停止序列字段的现场证据（上游 Mag1cFall/AIStudio2API 同样不发送，只在本地匹配正文），
// 不猜字段号；协议层或 generateInteraction 已清空并在该请求的管理日志记一行（原先整次请求返回参数错误）
func EncodeCreateInteractionStreamRequest(request GenerateRequest, defaults GenerationDefaults) ([]byte, []Content, error) {
	encoded, err := encodeCreateInteractionStream(request, defaults)
	if err != nil {
		return nil, nil, err
	}
	return encoded.body, encoded.binding, nil
}

// interactionEncoding 为编码好的 CreateInteractionStream 请求：请求体、参与 WAA binding 的内容，以及写请求结构摘要用的各部分
// （input steps、是否多轮、thinking level、max output tokens、视频格式与任务；编辑任务没有视频格式，video 为 nil）
type interactionEncoding struct {
	body         []byte
	binding      []Content
	steps        []any
	hasModelTurn bool
	level        int64
	maxOutput    int64
	video        []any
	task         int64
}

// encodeCreateInteractionStream 为 EncodeCreateInteractionStreamRequest 的实现，另外返回请求结构摘要要用的各部分
func encodeCreateInteractionStream(request GenerateRequest, defaults GenerationDefaults) (interactionEncoding, error) {
	steps, binding, hasModelTurn, err := encodeInteractionSteps(request.Contents)
	if err != nil {
		return interactionEncoding{}, err
	}
	level, err := interactionThinkingLevel(request.Config, defaults)
	if err != nil {
		return interactionEncoding{}, err
	}
	maxOutput := defaults.MaxOutputTokens
	if request.Config.MaxOutputTokens != nil && *request.Config.MaxOutputTokens > 0 {
		maxOutput = *request.Config.MaxOutputTokens
	}
	// 超过模型上限时按上限发送，不让整次请求失败
	if defaults.MaxOutputTokens > 0 && maxOutput > defaults.MaxOutputTokens {
		maxOutput = defaults.MaxOutputTokens
	}
	if maxOutput <= 0 {
		return interactionEncoding{}, fmt.Errorf("模型目录缺少有效 output token limit")
	}
	video, task, err := interactionVideoFormat(request.Config.VideoConfig, defaults.VideoResolution)
	if err != nil {
		return interactionEncoding{}, err
	}
	// 视频任务只能在第一轮使用（官网 Video task 的提示），协议层已按同一规则返回 400，这里兜底
	if task != 0 && hasModelTurn {
		return interactionEncoding{}, errors.New(videoTaskFirstTurnMessage)
	}
	// GenerationConfig：field 6 thinking level、field 7 thinking summaries、field 8 max output tokens；
	// 历史中没有模型输出时 field 25 为视频任务，未指定任务时为 []
	config := make([]any, 8)
	config[5] = level
	config[6] = int64(1)
	config[7] = maxOutput
	if !hasModelTurn {
		config = append(config, make([]any, 17)...)
		config[24] = []any{}
		if task != 0 {
			config[24] = []any{task}
		}
	}
	interaction := make([]any, 54)
	if system := strings.TrimSpace(request.System); system != "" {
		interaction[6] = system
	}
	interaction[17] = []any{wireModelName(request.Model), config}
	interaction[26] = []any{steps}
	// field 54 输出格式：视频格式 field 4 宽高比、field 5 时长、field 6 分辨率；编辑任务不带输出格式，interaction 只到 field 27
	if task == interactionVideoTasks["edit"] {
		interaction = interaction[:27]
		video = nil
	} else {
		interaction[53] = []any{[]any{[]any{nil, nil, nil, video}}}
	}
	body, err := json.Marshal([]any{int64(1), int64(1), nil, interaction, nil, int64(1)})
	if err != nil {
		return interactionEncoding{}, fmt.Errorf("编码 CreateInteractionStream: %w", err)
	}
	return interactionEncoding{
		body: body, binding: binding, steps: steps, hasModelTurn: hasModelTurn, level: level, maxOutput: maxOutput, video: video, task: task,
	}, nil
}

// interactionModelMediaIntro 放在从模型消息移到用户 step 的附件之前的说明
const interactionModelMediaIntro = "以下是上文模型回复里的图片或视频："

// interactionModelMedia 为模型消息里待移到随后用户 step 的附件；index 为它所在的 contents 下标，报错时指明位置
type interactionModelMedia struct {
	index int
	part  Part
}

// interactionStep 为编码中的一条 input step：model 为模型 step，parts 为 Content 列表
type interactionStep struct {
	model bool
	parts []any
}

// encodeInteractionSteps 把规范消息编码为 Interaction input steps，用户为 step field 1、模型为 field 2；part 按客户端给的顺序编码。
// 相邻同角色的消息合并为一条 step（与上游 Mag1cFall/AIStudio2API 的 encodeInteractionSteps 相同；官网每轮只有一条用户 step 与一条模型
// step），例如连续两条用户消息、只有附件的模型消息省略后前后两条用户消息，以及连续的模型消息。
// 模型 step 只带文本：模型消息里的附件（客户端回传的生成图片、本服务恢复的图片与视频链接、续接历史里模型生成的视频）连同一句说明
// 移到紧随其后的用户消息最前面（与 api.normalizeTurns 移动工具结果图片的做法一致），以模型消息结尾时单独成一条用户消息（前面是用户 step 时
// 按上一条规则并入）；只有附件的模型消息整条省略。移过去的附件按用户附件编码：能内联的图片（PNG、JPEG、WebP、HEIC、HEIF、BMP、TIFF）内联
// （与官网相同），视频等上传后为 Drive 引用；没有 Drive 授权、仍是 inline data 的视频与不能内联的图片与用户附件一样返回需要 Drive 授权的
// 错误，外部地址不接受，不静默丢掉。错误说明为英文，按参数错误原样返回客户端（app 层在发送前已把这类请求交给有 Drive 授权的账号，
// 外部地址在选号前按 400 返回，这里只是兜底）。binding 不随 step 合并：与合并前、与上游相同，每条编码出 part 的消息一条（以模型消息结尾时
// 移过去的说明文字另成一条），只含文本（图片数据与文件 ID 不参与，移过去的说明文字参与，放在随后那条用户消息的开头）。bindingPrompt 按
// 消息处理 YouTube 链接（去掉链接文本、补一个占位，只在同一条消息内去重），合并后两条消息里同一个链接只剩一个占位，参与 WAA proof 的
// 字符串就变了
func encodeInteractionSteps(contents []Content) ([]any, []Content, bool, error) {
	merged := make([]interactionStep, 0, len(contents)+1)
	binding := make([]Content, 0, len(contents)+1)
	hasModelTurn := false
	var pendingMedia []interactionModelMedia
	// appendStep 追加一条 step；与上一条同角色时并入上一条（parts 按顺序接在后面）
	appendStep := func(model bool, parts []any) {
		if last := len(merged) - 1; last >= 0 && merged[last].model == model {
			merged[last].parts = append(merged[last].parts, parts...)
			return
		}
		merged = append(merged, interactionStep{model: model, parts: parts})
	}
	// flushMedia 编码待移动的模型附件，返回放在用户消息最前面的 part 与参与 binding 的说明文字（图片数据与文件 ID 不参与 binding）
	flushMedia := func() ([]any, []Part, error) {
		if len(pendingMedia) == 0 {
			return nil, nil, nil
		}
		parts := []any{[]any{[]any{interactionModelMediaIntro}}}
		for _, media := range pendingMedia {
			encoded, err := encodeInteractionAttachment(media.index, media.part, RoleUser)
			if err != nil {
				return nil, nil, err
			}
			parts = append(parts, encoded)
		}
		pendingMedia = nil
		return parts, []Part{{Text: interactionModelMediaIntro}}, nil
	}
	for index, content := range contents {
		parts := make([]any, 0, len(content.Parts))
		texts := make([]Part, 0, len(content.Parts))
		if content.Role == RoleUser {
			media, intro, err := flushMedia()
			if err != nil {
				return nil, nil, false, err
			}
			parts = append(parts, media...)
			texts = append(texts, intro...)
		}
		for _, part := range content.Parts {
			switch {
			case part.Thought || part.Text == "" && part.ThoughtSignature != "" && part.InlineData == nil && part.File == nil:
				continue
			case part.Text != "" && part.InlineData == nil && part.File == nil && part.FunctionCall == nil && part.FunctionResult == nil:
				parts = append(parts, []any{[]any{part.Text}})
				texts = append(texts, Part{Text: part.Text})
			case content.Role == RoleAssistant && (part.InlineData != nil || part.File != nil || part.ExternalMedia != nil):
				pendingMedia = append(pendingMedia, interactionModelMedia{index: index, part: part})
			default:
				encoded, err := encodeInteractionAttachment(index, part, content.Role)
				if err != nil {
					return nil, nil, false, err
				}
				parts = append(parts, encoded)
			}
		}
		if len(parts) == 0 {
			continue
		}
		switch content.Role {
		case RoleUser:
			appendStep(false, parts)
		case RoleAssistant:
			appendStep(true, parts)
			hasModelTurn = true
		default:
			return nil, nil, false, fmt.Errorf("contents[%d]: this model does not accept %s messages", index, content.Role)
		}
		binding = append(binding, Content{Role: content.Role, Parts: texts})
	}
	media, intro, err := flushMedia()
	if err != nil {
		return nil, nil, false, err
	}
	if len(media) > 0 {
		appendStep(false, media)
		binding = append(binding, Content{Role: RoleUser, Parts: intro})
	}
	if len(merged) == 0 {
		return nil, nil, false, fmt.Errorf("%w: contents must not be empty", ErrInvalidArgument)
	}
	steps := make([]any, 0, len(merged))
	for _, step := range merged {
		if step.model {
			steps = append(steps, []any{nil, []any{step.parts}})
		} else {
			steps = append(steps, []any{[]any{step.parts}})
		}
	}
	return steps, binding, hasModelTurn, nil
}

// encodeInteractionAttachment 编码 step 里的附件 part：能内联的图片按官网内联为 Content field 2 [MIME 枚举, Base64]（枚举取官网前端的表：
// PNG 1、JPEG 2、WebP 3、HEIC 4、HEIF 5、BMP 8、TIFF 9，原文件字节；GIF 已在 app 层转成 PNG，见 PrepareInteractionImages），用户的 Drive
// 文件引用为 Content field 9；其余 inline data（视频、音频、PDF 等文件与不能内联的图片，账户没有 Drive 授权时仍是 inline data）返回需要
// Drive 授权的错误；外部地址与其他 part 不接受。
// 错误说明为英文（原样返回客户端）
func encodeInteractionAttachment(index int, part Part, role Role) ([]any, error) {
	switch {
	case InteractionInlineImage(part):
		// Content field 2 为图片：[MIME 枚举, Base64]
		enum, _ := interactionImageEnum(part.InlineData)
		return []any{nil, []any{enum, base64.StdEncoding.EncodeToString(part.InlineData.Data)}}, nil
	case part.File != nil && strings.TrimSpace(part.File.ID) != "" && part.Text == "" && role == RoleUser:
		// Content field 9 为 Drive 文件引用
		return []any{nil, nil, nil, nil, nil, nil, nil, nil, []any{strings.TrimSpace(part.File.ID)}}, nil
	case part.InlineData != nil:
		return nil, errors.New(interactionDriveMediaMessage(index, part.InlineData))
	case part.ExternalMedia != nil:
		return nil, errors.New(interactionExternalMediaMessage(index))
	default:
		return nil, fmt.Errorf("contents[%d]: this model only accepts text, image and video parts; this part type is not supported", index)
	}
}

// interactionThinkingLevel 按请求的思考强度或预算选择模型支持的 Interaction thinking level
func interactionThinkingLevel(config GenerationConfig, defaults GenerationDefaults) (int64, error) {
	level := defaults.DefaultThinkingLevel
	switch normalizeReasoningEffort(config.ReasoningEffort) {
	case "":
		if config.ThinkingBudget != nil {
			level = thinkingLevelForBudget(*config.ThinkingBudget)
		}
	case "minimal", "none":
		level = 4
	case "low":
		level = 1
	case "medium":
		level = 2
	case "high":
		level = 3
	}
	level = closestSupportedThinkingLevel(level, defaults.ThinkingLevels)
	wire, ok := interactionThinkingLevels[level]
	if !ok {
		return 0, fmt.Errorf("未识别的 thinking level %d", level)
	}
	return wire, nil
}

// generateInteraction 经 CreateInteractionStream 生成并映射为规范事件
func (c *Client) generateInteraction(ctx context.Context, request GenerateRequest, entry modelEntry) (<-chan Event, error) {
	// 视频设置按实际调用的账户目录再整理一次：协议层按合并目录整理过并记下说明（时长范围取各账户的交集），这里通常不会再改；
	// 协议层没有整理（例如启动期间别名目录取不到）或合并目录与所选账户的目录不一致（例如别名目录缓存还没刷新）时，
	// 这一次留下的新说明经 ReportRequestNote 写进该请求的管理日志。
	// 任务是否可用以协议层的合并目录为准，这里不按单个账户的 video_tasks 拒绝（拒绝会直接返回 400，不会换号）
	video, notes, err := normalizeVideoConfig(request.Config.VideoConfig, entry.model, request.Contents, false)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidArgument, err)
	}
	for _, note := range notes {
		if !slices.Contains(request.Notes, note) {
			ReportRequestNote(ctx, note)
		}
	}
	request.Config.VideoConfig = video
	// 协议层对能输出视频的 Omni 模型已清空 stop sequences 并记过说明；协议层没有处理到的请求（目录没有列出视频选项的 Interaction 模型、
	// 别名目录取不到等）在这里补记
	if count := len(request.Config.StopSequences); count > 0 {
		ReportRequestNote(ctx, InteractionStopSequencesNote(count))
		request.Config.StopSequences = nil
	}
	encoded, err := encodeCreateInteractionStream(request, entry.defaults)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidArgument, err)
	}
	// 请求结构摘要（各 step 的 part 种类、图片格式与字节数、Drive 引用、视频设置等，不含提示词正文与图片数据）写进该请求的管理日志，
	// 上游返回笼统的 400 时与官网抓包逐项对照
	ReportRequestInfo(ctx, encoded.summary(request))
	rpc := newRPCRequest("CreateInteractionStream", request.AccountID, request.ID, encoded.body, true)
	c.applyBenefitTier(rpc.Method, request.AccountID, rpc.Header)
	bindingRequest := request
	bindingRequest.Contents = encoded.binding
	// 经排查路由（/trace/）的请求与 GenerateContent 一样记下发往上游的请求体与上游原始响应的开头（含错误响应：在读出错误之前接上），
	// 另记 binding 的字节数与 sha256（只是证据，binding 的组成不变），便于与官网页面算 proof 时的输入对照
	trace := TraceFromContext(ctx)
	trace.RecordRequest(rpc.Method, rpc.URL, encoded.body)
	if trace != nil {
		if prompt, promptErr := bindingPrompt(bindingRequest); promptErr == nil {
			sum := sha256.Sum256([]byte(prompt))
			trace.Note(fmt.Sprintf("CreateInteractionStream binding | 字节=%d | sha256=%s", len(prompt), hex.EncodeToString(sum[:])))
		}
	}
	response, err := c.protected.DoProtected(ctx, bindingRequest, rpc)
	if err != nil {
		return nil, fmt.Errorf("发送 AI Studio CreateInteractionStream: %w", err)
	}
	if response != nil {
		response.Body = trace.TeeUpstream(response.Body)
	}
	response, err = validateRPCResponse("CreateInteractionStream", response)
	if err != nil {
		return nil, err
	}
	events := make(chan Event, 8)
	ready := make(chan error, 1)
	go func() {
		defer close(events)
		stopClose := context.AfterFunc(ctx, func() {
			_ = response.Body.Close()
		})
		defer stopClose()
		committed := false
		// 解码或转发时 panic：关闭上游响应体，还没交出事件流时按错误返回（调用方照旧换号或返回错误），已交出时以错误事件结束，不让进程退出
		defer func() {
			value := recover()
			if value == nil {
				return
			}
			_ = response.Body.Close()
			err := decodePanicError("解码 AI Studio Interaction 事件流", value)
			if !committed {
				ready <- err
				return
			}
			select {
			case events <- Event{Kind: EventError, Err: err}:
			case <-ctx.Done():
			}
		}()
		send := func(event Event) error {
			event = assignLocalCallID(event)
			if request.Config.HideThinking && event.Kind == EventReasoning {
				if event.ThoughtSignature == "" {
					return nil
				}
				event = hiddenThought(event)
			}
			if !committed {
				committed = true
				ready <- nil
			}
			event.ProviderModel = entry.model.ID
			select {
			case events <- event:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		err := DecodeInteractionStream(observeStreamActivity(ctx, response.Body), send)
		if closeErr := response.Body.Close(); err == nil && ctx.Err() == nil {
			err = closeErr
		}
		if err == nil || ctx.Err() != nil {
			if !committed {
				ready <- ctx.Err()
			}
			return
		}
		if !committed {
			ready <- err
			return
		}
		_ = send(Event{Kind: EventError, Err: err})
	}()
	select {
	case err := <-ready:
		if err != nil {
			for range events {
			}
			return nil, err
		}
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return events, nil
}

// DecodeInteractionStream 按网络顺序解码 CreateInteractionStream 事件与流尾状态
func DecodeInteractionStream(source io.Reader, emit func(Event) error) error {
	decoder := json.NewDecoder(newSparseJSONReader(source))
	decoder.UseNumber()
	if token, err := decoder.Token(); err != nil || token != json.Delim('[') {
		return &ProtocolEvidenceError{Method: "CreateInteractionStream", Path: "$", Detail: "根值不是数组"}
	}
	if !decoder.More() {
		return &ProtocolEvidenceError{Method: "CreateInteractionStream", Path: "$", Detail: "根数组缺少事件列表"}
	}
	if token, err := decoder.Token(); err != nil || token != json.Delim('[') {
		return &ProtocolEvidenceError{Method: "CreateInteractionStream", Path: "$[0]", Detail: "事件列表不是数组"}
	}
	finished := false
	for index := 0; decoder.More(); index++ {
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			return &ProtocolEvidenceError{Method: "CreateInteractionStream", Path: fmt.Sprintf("$[0][%d]", index), Detail: err.Error(), Err: err}
		}
		done, err := decodeInteractionEvent(raw, emit)
		if err != nil {
			return err
		}
		finished = finished || done
	}
	if token, err := decoder.Token(); err != nil || token != json.Delim(']') {
		return &ProtocolEvidenceError{Method: "CreateInteractionStream", Path: "$[0]", Detail: "事件列表没有正常结束"}
	}
	trailing := make([]json.RawMessage, 0, 3)
	for decoder.More() {
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			return &ProtocolEvidenceError{Method: "CreateInteractionStream", Path: "$", Detail: err.Error(), Err: err}
		}
		trailing = append(trailing, raw)
	}
	if len(trailing) > 0 {
		// 流尾 field 2 为 google.rpc.Status：[code, message, details]
		status, err := rawArray(trailing[0], "$[1]", trailing[0])
		if err != nil || len(status) == 0 {
			return &ProtocolEvidenceError{Method: "CreateInteractionStream", Path: "$[1]", Detail: "流尾状态无效", Raw: cloneRaw(trailing[0])}
		}
		code, err := rawInt64(status[0], "$[1][0]", trailing[0])
		if err != nil {
			return withMethod(err, "CreateInteractionStream")
		}
		if code != 0 {
			statusCode := http.StatusBadGateway
			if mapped, ok := interactionStatusHTTP[code]; ok {
				statusCode = mapped
			}
			return DecodeRPCError("CreateInteractionStream", statusCode, trailing[0])
		}
	}
	if !finished {
		return &ProtocolEvidenceError{Method: "CreateInteractionStream", Path: "$", Detail: "流结束前没有最终 interaction"}
	}
	return nil
}

// decodeInteractionEvent 解码单个流事件，返回是否为最终 interaction
func decodeInteractionEvent(raw json.RawMessage, emit func(Event) error) (bool, error) {
	event, err := rawArray(raw, "$event", raw)
	if err != nil {
		return false, withMethod(err, "CreateInteractionStream")
	}
	if delta := rawAt(event, 10); !isJSONNull(delta) {
		return false, decodeInteractionDelta(delta, emit)
	}
	final := rawAt(event, 19)
	if isJSONNull(final) {
		final = rawAt(event, 1)
	}
	if isJSONNull(final) {
		return false, nil
	}
	wrapper, err := rawArray(final, "$event[19]", raw)
	if err != nil || len(wrapper) == 0 {
		return false, &ProtocolEvidenceError{Method: "CreateInteractionStream", Path: "$event[19]", Detail: "最终 interaction 为空", Raw: cloneRaw(raw)}
	}
	interaction, err := rawArray(wrapper[0], "$event[19][0]", raw)
	if err != nil {
		return false, withMethod(err, "CreateInteractionStream")
	}
	status, err := optionalIntField(interaction, 1, "$event[19][0]", raw)
	if err != nil {
		return false, withMethod(err, "CreateInteractionStream")
	}
	switch status {
	case 3:
	case 4:
		return false, &RPCError{Method: "CreateInteractionStream", StatusCode: http.StatusBadGateway, Message: "interaction failed"}
	case 5:
		return false, &RPCError{Method: "CreateInteractionStream", StatusCode: http.StatusBadGateway, Message: "interaction cancelled"}
	default:
		return false, &ProtocolEvidenceError{Method: "CreateInteractionStream", Path: "$event[19][0][1]", Detail: fmt.Sprintf("未识别的最终状态 %d", status), Raw: cloneRaw(raw)}
	}
	if usage := rawAt(interaction, 12); !isJSONNull(usage) {
		decoded, err := decodeInteractionUsage(usage, raw)
		if err != nil {
			return false, err
		}
		if err := emit(Event{Kind: EventUsage, Usage: decoded}); err != nil {
			return false, err
		}
	}
	return true, emit(Event{Kind: EventFinish, FinishReason: "stop"})
}

// decodeInteractionDelta 解码 content delta：field 1 文本、field 2 图片、field 3 音频、field 4 文档、field 5 视频、field 6 思考摘要、
// field 7 思考签名、field 10 引用（媒体与引用按上游 Mag1cFall/AIStudio2API 的解码移植：媒体按官网前端的枚举表取 MIME 后作为媒体输出，
// 引用为正文起止位置与来源网址）
func decodeInteractionDelta(raw json.RawMessage, emit func(Event) error) error {
	delta, err := rawArray(raw, "$delta", raw)
	if err != nil || len(delta) < 2 {
		return &ProtocolEvidenceError{Method: "CreateInteractionStream", Path: "$delta", Detail: "content delta 字段不足", Raw: cloneRaw(raw)}
	}
	content, err := rawArray(delta[1], "$delta[1]", raw)
	if err != nil {
		return withMethod(err, "CreateInteractionStream")
	}
	for index, value := range content {
		if isJSONNull(value) {
			continue
		}
		switch index {
		case 0:
			text, err := rawArray(value, "$delta[1][0]", raw)
			if err != nil || len(text) == 0 {
				return &ProtocolEvidenceError{Method: "CreateInteractionStream", Path: "$delta[1][0]", Detail: "文本 delta 无效", Raw: cloneRaw(raw)}
			}
			decoded, err := rawString(text[0], "$delta[1][0][0]", raw)
			if err != nil {
				return withMethod(err, "CreateInteractionStream")
			}
			if err := emit(Event{Kind: EventText, Text: decoded}); err != nil {
				return err
			}
		case 1, 2, 3, 4:
			media, err := decodeInteractionMedia(value, index, raw)
			if err != nil {
				return err
			}
			if err := emit(Event{Kind: EventMedia, Media: media}); err != nil {
				return err
			}
		case 5:
			text := strings.Join(interactionStrings(value), "")
			if text != "" {
				if err := emit(Event{Kind: EventReasoning, Text: text}); err != nil {
					return err
				}
			}
		case 6:
		case 9:
			if err := decodeInteractionCitations(value, raw, emit); err != nil {
				return err
			}
		default:
			return &ProtocolEvidenceError{Method: "CreateInteractionStream", Path: fmt.Sprintf("$delta[1][%d]", index), Detail: "未识别的 content delta 字段", Raw: cloneRaw(raw)}
		}
	}
	return nil
}

// decodeInteractionMedia 解码图片（field 2）、音频（field 3）、文档（field 4）或视频（field 5）content：MIME 枚举与 Base64 数据。
// 枚举为 null 或不在 interactionMediaMIME 表里时按官网前端取该类的缺省 MIME（图片 image/jpeg、音频 audio/wav、文档 application/pdf、
// 视频 video/mp4），不按文件头另行判断；枚举不是整数、数据不是 Base64 时按协议证据错误返回
func decodeInteractionMedia(value json.RawMessage, index int, raw json.RawMessage) (*Media, error) {
	kind := interactionMediaMIME[index]
	path := fmt.Sprintf("$delta[1][%d]", index)
	media, err := rawArray(value, path, raw)
	if err != nil || len(media) < 2 {
		return nil, &ProtocolEvidenceError{Method: "CreateInteractionStream", Path: path, Detail: kind.name + " delta 无效", Raw: cloneRaw(raw)}
	}
	var code int64
	if !isJSONNull(media[0]) {
		if code, err = rawInt64(media[0], path+"[0]", raw); err != nil {
			return nil, withMethod(err, "CreateInteractionStream")
		}
	}
	mimeType, ok := kind.codes[code]
	if !ok {
		mimeType = kind.unlisted
	}
	encoded, err := rawString(media[1], path+"[1]", raw)
	if err != nil {
		return nil, withMethod(err, "CreateInteractionStream")
	}
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, &ProtocolEvidenceError{Method: "CreateInteractionStream", Path: path + "[1]", Detail: kind.name + "数据不是 Base64", Raw: cloneRaw(raw)}
	}
	return &Media{MIME: mimeType, Data: data}, nil
}

// decodeInteractionCitations 解码 content field 10 的引用（按上游的解码移植）：分组里的每项依次为正文中引用标记的起止字符位置与
// field 4 的来源网址，按来源输出引用事件（各协议与 Playground 的引用一样写进 annotations、citationMetadata 等）
func decodeInteractionCitations(value json.RawMessage, raw json.RawMessage, emit func(Event) error) error {
	groups, err := rawArray(value, "$delta[1][9]", raw)
	if err != nil {
		return withMethod(err, "CreateInteractionStream")
	}
	for _, group := range groups {
		items, err := rawArray(group, "$delta[1][9][]", raw)
		if err != nil {
			return withMethod(err, "CreateInteractionStream")
		}
		for _, item := range items {
			fields, err := rawArray(item, "$delta[1][9][][]", raw)
			if err != nil || len(fields) < 4 {
				return &ProtocolEvidenceError{Method: "CreateInteractionStream", Path: "$delta[1][9][][]", Detail: "引用字段不足", Raw: cloneRaw(raw)}
			}
			start, err := optionalIntField(fields, 0, "$delta[1][9][][]", raw)
			if err != nil {
				return withMethod(err, "CreateInteractionStream")
			}
			end, err := optionalIntField(fields, 1, "$delta[1][9][][]", raw)
			if err != nil {
				return withMethod(err, "CreateInteractionStream")
			}
			urls := interactionStrings(fields[3])
			if len(urls) == 0 {
				return &ProtocolEvidenceError{Method: "CreateInteractionStream", Path: "$delta[1][9][][3]", Detail: "引用缺少网址", Raw: cloneRaw(raw)}
			}
			if err := emit(Event{Kind: EventCitation, Citation: &Citation{URL: urls[0], Start: int(start), End: int(end)}}); err != nil {
				return err
			}
		}
	}
	return nil
}

// interactionStrings 按顺序收集嵌套数组中的全部字符串
func interactionStrings(raw json.RawMessage) []string {
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil
	}
	var values []string
	var walk func(any)
	walk = func(item any) {
		switch typed := item.(type) {
		case string:
			values = append(values, typed)
		case []any:
			for _, child := range typed {
				walk(child)
			}
		}
	}
	walk(value)
	return values
}

// decodeInteractionUsage 解码 Usage：输入、输出、思考与总 token 位于索引 0、4、8、9
func decodeInteractionUsage(raw json.RawMessage, evidence json.RawMessage) (*Usage, error) {
	values, err := rawArray(raw, "$usage", evidence)
	if err != nil {
		return nil, withMethod(err, "CreateInteractionStream")
	}
	usage := &Usage{}
	for _, field := range []struct {
		index  int
		target *int64
	}{{0, &usage.InputTokens}, {4, &usage.OutputTokens}, {8, &usage.ReasoningTokens}, {9, &usage.TotalTokens}} {
		value, err := optionalIntField(values, field.index, "$usage", evidence)
		if err != nil {
			return nil, withMethod(err, "CreateInteractionStream")
		}
		*field.target = value
	}
	if usage.TotalTokens == 0 {
		return nil, errors.New("CreateInteractionStream usage 缺少 total tokens")
	}
	return usage, nil
}
