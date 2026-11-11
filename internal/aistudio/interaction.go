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

// EncodeCreateInteractionStreamRequest 编码官网 CreateInteractionStream 请求并返回参与 WAA 绑定的内容，binding 按 OMNI_IMAGE_BINDING 的默认值
// （开启：请求体里的内联图片计入，见 EncodeCreateInteractionStreamRequestBinding）。
// stop sequences 不发送：没有 CreateInteractionStream 停止序列字段的现场证据（上游 Mag1cFall/AIStudio2API 同样不发送，只在本地匹配正文），
// 不猜字段号；协议层或 generateInteraction 已清空并在该请求的管理日志记一行（原先整次请求返回参数错误）
func EncodeCreateInteractionStreamRequest(request GenerateRequest, defaults GenerationDefaults) ([]byte, []Content, error) {
	return EncodeCreateInteractionStreamRequestBinding(request, defaults, true)
}

// EncodeCreateInteractionStreamRequestBinding 与 EncodeCreateInteractionStreamRequest 相同，imageBinding 为 OMNI_IMAGE_BINDING：
// 为真时请求体里的内联图片按 GenerateContent 的写法计入 binding，为假时 binding 只含文本（与 a574e64 相同，见 encodeInteractionSteps）。
// 请求体与开关无关，逐字节相同
func EncodeCreateInteractionStreamRequestBinding(request GenerateRequest, defaults GenerationDefaults, imageBinding bool) ([]byte, []Content, error) {
	encoded, err := encodeCreateInteractionStream(request, defaults, imageBinding)
	if err != nil {
		return nil, nil, err
	}
	return encoded.body, encoded.binding, nil
}

// interactionEncoding 为编码好的 CreateInteractionStream 请求：请求体、参与 WAA binding 的内容，以及写请求结构摘要用的各部分
// （input steps、是否多轮、thinking level、max output tokens、视频格式与任务；编辑任务没有视频格式，video 为 nil；
// imageBinding 为这次编码时 OMNI_IMAGE_BINDING 的值，bindingImages 为计入 binding 的图片数）
type interactionEncoding struct {
	body          []byte
	binding       []Content
	steps         []any
	hasModelTurn  bool
	level         int64
	maxOutput     int64
	video         []any
	task          int64
	imageBinding  bool
	bindingImages int
}

// encodeCreateInteractionStream 为 EncodeCreateInteractionStreamRequestBinding 的实现，另外返回请求结构摘要要用的各部分
func encodeCreateInteractionStream(request GenerateRequest, defaults GenerationDefaults, imageBinding bool) (interactionEncoding, error) {
	steps, binding, hasModelTurn, err := encodeInteractionSteps(request.Contents, imageBinding)
	if err != nil {
		return interactionEncoding{}, err
	}
	bindingImages := 0
	for _, content := range binding {
		for _, part := range content.Parts {
			if part.InlineData != nil {
				bindingImages++
			}
		}
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
	// 历史中没有模型输出时 field 25 为视频任务，未指定任务时为 []。模型目录没有思考等级（能力码 52，如 gemini-omni-1.2-flash）时
	// level 为 0，与官网一样不发送 field 6（见 interactionThinkingLevel）
	config := make([]any, 8)
	if level != 0 {
		config[5] = level
	}
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
		imageBinding: imageBinding, bindingImages: bindingImages,
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
// 移过去的说明文字另成一条），含文本（移过去的说明文字参与，放在随后那条用户消息的开头），Drive 文件 ID 不参与。bindingPrompt 按
// 消息处理 YouTube 链接（去掉链接文本、补一个占位，只在同一条消息内去重），合并后两条消息里同一个链接只剩一个占位，参与 WAA proof 的
// 字符串就变了。
// imageBinding 为 OMNI_IMAGE_BINDING（用户特许的唯一例外，只改 Interaction 交给 DoProtected 的 binding 内容）：为真时请求体里内联发送的图片
// （Content field 2，含 GIF 转成的 PNG、从模型历史移到用户 step 的图片、从本服务链接恢复的图片）按它在 step 里的位置计入 binding，写法与
// GenerateContent 的 bindingPrompt 相同（InlineData 按 base64.StdEncoding 写出，与请求体里发送的是同一段原字节）；为假时只含文本，与 a574e64
// 逐字节相同。没有内联图片的请求两者相同。上游 Mag1cFall/AIStudio2API 的 Interaction 从不内联图片（一律要 Drive），只含文本的规则没有在
// 内联图片时验证过：线上带图请求在多个账户上都被 403 PERMISSION_DENIED 拒绝、纯文字正常，推断官网页面算 proof 时计入了图片
func encodeInteractionSteps(contents []Content, imageBinding bool) ([]any, []Content, bool, error) {
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
	// bindingImage 返回内联图片参与 binding 的 part（imageBinding 为假或 part 不是内联图片时为 false）：只带 inline data，
	// bindingPrompt 按 base64.StdEncoding 写出，与 encodeInteractionAttachment 发送的是同一段原字节
	bindingImage := func(part Part) (Part, bool) {
		if !imageBinding || !InteractionInlineImage(part) {
			return Part{}, false
		}
		return Part{InlineData: part.InlineData}, true
	}
	// flushMedia 编码待移动的模型附件，返回放在用户消息最前面的 part 与参与 binding 的说明文字（imageBinding 时还有其中的内联图片，
	// 按发送顺序；文件 ID 不参与 binding）
	flushMedia := func() ([]any, []Part, error) {
		if len(pendingMedia) == 0 {
			return nil, nil, nil
		}
		parts := []any{[]any{[]any{interactionModelMediaIntro}}}
		bound := []Part{{Text: interactionModelMediaIntro}}
		for _, media := range pendingMedia {
			encoded, err := encodeInteractionAttachment(media.index, media.part, RoleUser)
			if err != nil {
				return nil, nil, err
			}
			parts = append(parts, encoded)
			if image, ok := bindingImage(media.part); ok {
				bound = append(bound, image)
			}
		}
		pendingMedia = nil
		return parts, bound, nil
	}
	for index, content := range contents {
		parts := make([]any, 0, len(content.Parts))
		// bound 为这条消息参与 binding 的 part：文本，imageBinding 时还有内联图片，按发送顺序
		bound := make([]Part, 0, len(content.Parts))
		if content.Role == RoleUser {
			media, intro, err := flushMedia()
			if err != nil {
				return nil, nil, false, err
			}
			parts = append(parts, media...)
			bound = append(bound, intro...)
		}
		for _, part := range content.Parts {
			switch {
			case part.Thought || part.Text == "" && part.ThoughtSignature != "" && part.InlineData == nil && part.File == nil:
				continue
			case part.Text != "" && part.InlineData == nil && part.File == nil && part.FunctionCall == nil && part.FunctionResult == nil:
				parts = append(parts, []any{[]any{part.Text}})
				bound = append(bound, Part{Text: part.Text})
			case content.Role == RoleAssistant && (part.InlineData != nil || part.File != nil || part.ExternalMedia != nil):
				pendingMedia = append(pendingMedia, interactionModelMedia{index: index, part: part})
			default:
				encoded, err := encodeInteractionAttachment(index, part, content.Role)
				if err != nil {
					return nil, nil, false, err
				}
				parts = append(parts, encoded)
				if image, ok := bindingImage(part); ok {
					bound = append(bound, image)
				}
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
		binding = append(binding, Content{Role: content.Role, Parts: bound})
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

// interactionThinkingLevel 按请求的思考强度或预算选择模型支持的 Interaction thinking level。模型目录没有思考等级能力（能力码 52，
// 如 gemini-omni-1.2-flash，官网页面没有 Thinking level）时与官网一样不发送，返回 0：请求的思考强度、预算与模型名的等级后缀都不起作用
// （请求日志的说明见 InteractionThinkingIgnoredNote 与 InteractionThinkingSuffixIgnoredNote）
func interactionThinkingLevel(config GenerationConfig, defaults GenerationDefaults) (int64, error) {
	if !defaults.ThinkingLevel {
		return 0, nil
	}
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
	// 模型目录没有思考等级（能力码 52）时请求体不带思考等级，客户端的思考参数不起作用：写一行说明（模型名的等级后缀协议层没有写进思考参数，
	// 另记了一行；同一条说明已在 request.Notes 里时不重复）
	if note := InteractionThinkingIgnoredNote(entry.model.ID, entry.defaults, request.Config); note != "" && !slices.Contains(request.Notes, note) {
		ReportRequestNote(ctx, note)
	}
	// 协议层对能输出视频的 Omni 模型已清空 stop sequences 并记过说明；协议层没有处理到的请求（目录没有列出视频选项的 Interaction 模型、
	// 别名目录取不到等）在这里补记
	if count := len(request.Config.StopSequences); count > 0 {
		ReportRequestNote(ctx, InteractionStopSequencesNote(count))
		request.Config.StopSequences = nil
	}
	// OMNI_IMAGE_BINDING：app 层每次尝试按当前设置挂在 ctx 上（保存后下一个请求立即按新值），没有挂时按默认开启；只改交给 DoProtected 的
	// binding 内容，请求体不变
	encoded, err := encodeCreateInteractionStream(request, entry.defaults, InteractionImageBindingFromContext(ctx))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidArgument, err)
	}
	// 请求结构摘要（各 step 的 part 种类、图片格式与字节数、Drive 引用、binding 是否计入图片、视频设置等，不含提示词正文与图片数据）写进
	// 该请求的管理日志，上游返回笼统的 400 或带图请求返回 403 时与官网抓包逐项对照
	ReportRequestInfo(ctx, encoded.summary(request))
	rpc := newRPCRequest("CreateInteractionStream", request.AccountID, request.ID, encoded.body, true)
	c.applyBenefitTier(rpc.Method, request.AccountID, rpc.Header)
	bindingRequest := request
	bindingRequest.Contents = encoded.binding
	// 经排查路由（/trace/）的请求与 GenerateContent 一样记下发往上游的请求体与上游原始响应的开头（含错误响应：在读出错误之前接上），
	// 另记 binding 的字节数与 sha256（只是证据，不改 binding 的组成；是否计入图片见请求结构摘要），便于与官网页面算 proof 时的输入对照
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
		// output 收集交出的正文与思考：上游用量读不出时按它做本地估算（与 Playground 相同；媒体不计 token，不收集，免得多拷一份视频）
		var output generatedOutputParts
		send := func(event Event) error {
			event = assignLocalCallID(event)
			if event.Kind != EventMedia {
				output.observe(event)
			}
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
		err := decodeInteractionStream(observeStreamActivity(ctx, response.Body), send, decodeToleranceFrom(ctx), func() *Usage {
			return localCompleteUsage(request, output)
		})
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

// interactionMethod 为 CreateInteractionStream 写进协议证据与容错日志的方法名
const interactionMethod = "CreateInteractionStream"

// DecodeInteractionStream 按网络顺序解码 CreateInteractionStream 事件与流尾状态（容错只在这次解码内去重）
func DecodeInteractionStream(source io.Reader, emit func(Event) error) error {
	return decodeInteractionStream(source, emit, newDecodeTolerance(nil, ""), nil)
}

// decodeInteractionStream 为 DecodeInteractionStream 的实现，容错写进 tolerance（见 decode_tolerance.go）。estimate 返回本地估算的用量：
// 最终 interaction 带着用量却读不出、或最终 interaction 为空时用它代替（generateInteraction 按已交出的正文与思考估算）；为 nil 时丢掉用量
func decodeInteractionStream(source io.Reader, emit func(Event) error, tolerance *decodeTolerance, estimate func() *Usage) error {
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
	// delivered 表示已经交出过正文或媒体：最终状态读不出时据此决定按 other 结束还是报错
	delivered := false
	track := func(event Event) error {
		if event.Kind == EventMedia || event.Kind == EventText && event.Text != "" {
			delivered = true
		}
		return emit(event)
	}
	for index := 0; decoder.More(); index++ {
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			return &ProtocolEvidenceError{Method: "CreateInteractionStream", Path: fmt.Sprintf("$[0][%d]", index), Detail: err.Error(), Err: err}
		}
		done, err := decodeInteractionEvent(raw, track, tolerance, delivered, estimate)
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
		// 流尾 field 2 为 google.rpc.Status：[code, message, details]；为 null、[] 或状态码为 null 时按 0（成功），与 Build 代理共用 trailerError
		if err := trailerError(interactionMethod, trailing[0], tolerance); err != nil {
			return err
		}
	}
	if !finished {
		return &ProtocolEvidenceError{Method: "CreateInteractionStream", Path: "$", Detail: "流结束前没有最终 interaction"}
	}
	return nil
}

// decodeInteractionEvent 解码单个流事件，返回是否为最终 interaction。最终状态 3 为完成，4、5 为失败与取消；不在表里、为 null 或读不出时，
// 已经交出过正文或媒体（delivered）就写一行容错日志、照常读取用量并按 other 结束（视频已生成、额度已扣，不让整份作废），否则仍按协议证据
// 错误返回。最终 interaction 为 [] 时同样处理（没有用量可读，有 estimate 时交出本地估算）。用量读不出时有 estimate 就交出本地估算
func decodeInteractionEvent(raw json.RawMessage, emit func(Event) error, tolerance *decodeTolerance, delivered bool, estimate func() *Usage) (bool, error) {
	event, err := rawArray(raw, "$event", raw)
	if err != nil {
		return false, withMethod(err, "CreateInteractionStream")
	}
	if delta := rawAt(event, 10); !isJSONNull(delta) {
		return false, decodeInteractionDelta(delta, emit, tolerance)
	}
	final := rawAt(event, 19)
	if isJSONNull(final) {
		final = rawAt(event, 1)
	}
	if isJSONNull(final) {
		return false, nil
	}
	wrapper, err := rawArray(final, "$event[19]", raw)
	if err != nil || len(wrapper) == 0 && !delivered {
		return false, &ProtocolEvidenceError{Method: "CreateInteractionStream", Path: "$event[19]", Detail: "最终 interaction 为空", Raw: cloneRaw(raw)}
	}
	// finishEmpty 处理已经交出过内容、最终 interaction 却没有任何字段的情形：与状态读不出时一样按 other 结束（没有用量可读，有 estimate
	// 时交出本地估算；原先报“最终 interaction 为空”或“未识别的最终状态”，正文或视频已交出也整次 500 或用量记 0）
	finishEmpty := func(path string, value json.RawMessage) (bool, error) {
		tolerance.note(interactionMethod, path, "最终 interaction 为空，已交出内容，按 other 结束", value)
		if estimate != nil {
			if usage := estimate(); usage != nil {
				if err := emit(Event{Kind: EventUsage, Usage: usage}); err != nil {
					return false, err
				}
			}
		}
		return true, emit(Event{Kind: EventFinish, FinishReason: "other"})
	}
	if len(wrapper) == 0 {
		// event[19] 为外层 wrapper，Interaction 本身是 wrapper[0]：[] 是 wrapper 为空（没有 Interaction）
		return finishEmpty("$event[19]", final)
	}
	interaction, err := rawArray(wrapper[0], "$event[19][0]", raw)
	if err != nil {
		return false, withMethod(err, "CreateInteractionStream")
	}
	if len(interaction) == 0 && delivered {
		// [[]] 或 [null]：Interaction 的字段全为缺省值（JSON+protobuf 省略全部字段），与 wrapper 为空时一样处理
		return finishEmpty("$event[19][0]", final)
	}
	status, statusErr := optionalIntField(interaction, 1, "$event[19][0]", raw)
	finishReason := "stop"
	switch {
	case statusErr == nil && status == 3:
	case statusErr == nil && status == 4:
		return false, &RPCError{Method: "CreateInteractionStream", StatusCode: http.StatusBadGateway, Message: "interaction failed"}
	case statusErr == nil && status == 5:
		return false, &RPCError{Method: "CreateInteractionStream", StatusCode: http.StatusBadGateway, Message: "interaction cancelled"}
	case !delivered && statusErr != nil:
		return false, withMethod(statusErr, "CreateInteractionStream")
	case !delivered:
		return false, &ProtocolEvidenceError{Method: "CreateInteractionStream", Path: "$event[19][0][1]", Detail: fmt.Sprintf("未识别的最终状态 %d", status), Raw: cloneRaw(raw)}
	default:
		tolerance.note(interactionMethod, "$event[19][0][1]", "最终状态不在表里，已交出内容，按 other 结束", rawAt(interaction, 1))
		finishReason = "other"
	}
	if usage := rawAt(interaction, 12); !isJSONNull(usage) {
		// 用量是辅助元数据：读不出时改用本地估算（没有 estimate 时丢掉），都写容错日志，结束事件照常发出
		decoded, ok := decodeInteractionUsage(usage, tolerance)
		switch {
		case !ok && estimate != nil:
			tolerance.note(interactionMethod, "$event[19][0][12]", "用量无法解码，改用本地估算", usage)
			decoded = estimate()
		case !ok:
			tolerance.note(interactionMethod, "$event[19][0][12]", "用量无法解码，已丢弃", usage)
		}
		if decoded != nil {
			if err := emit(Event{Kind: EventUsage, Usage: decoded}); err != nil {
				return false, err
			}
		}
	}
	return true, emit(Event{Kind: EventFinish, FinishReason: finishReason})
}

// decodeInteractionDelta 解码 content delta：field 1 文本、field 2 图片、field 3 音频、field 4 文档、field 5 视频、field 6 思考摘要、
// field 7 思考签名、field 10 引用（媒体与引用按上游 Mag1cFall/AIStudio2API 的解码移植：媒体按官网前端的枚举表取 MIME 后作为媒体输出，
// 引用为正文起止位置与来源网址）。没有 content 的 delta 跳过，没见过的 content 字段跳过并写容错日志（原先按协议证据错误中止）
func decodeInteractionDelta(raw json.RawMessage, emit func(Event) error, tolerance *decodeTolerance) error {
	delta, err := rawArray(raw, "$delta", raw)
	if err != nil {
		return &ProtocolEvidenceError{Method: "CreateInteractionStream", Path: "$delta", Detail: "content delta 不是数组", Raw: cloneRaw(raw)}
	}
	if isJSONNull(rawAt(delta, 1)) {
		tolerance.note(interactionMethod, "$delta[1]", "content delta 没有 content，已跳过", raw)
		return nil
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
			if err != nil {
				return &ProtocolEvidenceError{Method: "CreateInteractionStream", Path: "$delta[1][0]", Detail: "文本 delta 无效", Raw: cloneRaw(raw)}
			}
			if len(text) == 0 {
				// TextContent 全为缺省值（JSON+protobuf 省略了空串 text）：没有正文，跳过（原先按“文本 delta 无效”中止）
				tolerance.note(interactionMethod, "$delta[1][0]", "文本 delta 为空，已跳过", value)
				continue
			}
			// 正文：null 为空串，数字与布尔值按原文转成文本；对象与数组读不出正文，仍按协议证据错误中止
			decoded, converted, ok := lenientText(text[0])
			if !ok {
				return &ProtocolEvidenceError{Method: "CreateInteractionStream", Path: "$delta[1][0][0]", Detail: "正文不是字符串，读不出正文", Raw: cloneRaw(raw)}
			}
			if converted {
				tolerance.note(interactionMethod, "$delta[1][0][0]", "正文不是字符串，按原文转成文本", text[0])
			}
			if err := emit(Event{Kind: EventText, Text: decoded}); err != nil {
				return err
			}
		case 1, 2, 3, 4:
			media, err := decodeInteractionMedia(value, index, raw, tolerance)
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
			if err := decodeInteractionCitations(value, raw, emit, tolerance); err != nil {
				return err
			}
		default:
			// 没见过的 content 字段（官网新增的内容类型）：跳过这一项，正文与媒体照常
			tolerance.note(interactionMethod, fmt.Sprintf("$delta[1][%d]", index), fmt.Sprintf("未识别的 content delta 字段（下标 %d），已跳过", index), value)
		}
	}
	return nil
}

// decodeInteractionMedia 解码图片（field 2）、音频（field 3）、文档（field 4）或视频（field 5）content：MIME 枚举与 Base64 数据。
// MIME 枚举按 decodeInteractionMediaMIME 取值，读不出时取该类的缺省 MIME，不中止（媒体已经生成、额度已扣）；数据不是 Base64 时按协议
// 证据错误返回。Base64 接受标准与 URL 安全两种字母表、带或不带填充（标准带填充的写法最先尝试，官网现有的数据结果不变）
func decodeInteractionMedia(value json.RawMessage, index int, raw json.RawMessage, tolerance *decodeTolerance) (*Media, error) {
	kind := interactionMediaMIME[index]
	path := fmt.Sprintf("$delta[1][%d]", index)
	media, err := rawArray(value, path, raw)
	if err != nil || len(media) < 2 {
		return nil, &ProtocolEvidenceError{Method: "CreateInteractionStream", Path: path, Detail: kind.name + " delta 无效", Raw: cloneRaw(raw)}
	}
	mimeType := decodeInteractionMediaMIME(kind, media[0], path+"[0]", tolerance)
	encoded, err := rawString(media[1], path+"[1]", raw)
	if err != nil {
		return nil, withMethod(err, "CreateInteractionStream")
	}
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		var ok bool
		if data, ok = decodeLenientBase64(encoded); !ok {
			return nil, &ProtocolEvidenceError{Method: "CreateInteractionStream", Path: path + "[1]", Detail: kind.name + "数据不是 Base64", Raw: cloneRaw(raw)}
		}
		tolerance.note(interactionMethod, path+"[1]", kind.name+"数据不是标准带填充的 Base64，按 URL 安全或不带填充的写法解码", toleranceText("长度=%d", len(encoded)))
	}
	return &Media{MIME: mimeType, Data: data}, nil
}

// decodeInteractionMediaMIME 按 interactionMediaMIME 读取媒体的 MIME 枚举：表内整数按表；null 与 0（未设置，官网现有的视频即为
// [null,<数据>]）取该类的缺省 MIME（图片 image/jpeg、音频 audio/wav、文档 application/pdf、视频 video/mp4，与官网前端相同，不按文件头
// 另行判断），不写日志。其余写法不中止、各写一行容错日志：不在表里的整数取缺省 MIME；带小数的数与非整数写法的数字字符串按编号换算（整数写法的数字字符串
// 如 "7" 按整数直接读取，不写日志）；写成表里的 MIME
// 字符串（如 "image/png"）时按字符串；其他写法（枚举名称、对象等）取缺省 MIME（原先按协议证据错误中止整次生成，媒体已经生成、额度已扣）
func decodeInteractionMediaMIME(kind interactionMediaKind, raw json.RawMessage, path string, tolerance *decodeTolerance) string {
	if isJSONNull(raw) {
		return kind.unlisted
	}
	code, err := rawInt64(raw, path, raw)
	if err != nil {
		var ok bool
		if code, ok = lenientInt64(raw); ok {
			tolerance.note(interactionMethod, path, kind.name+" MIME 枚举不是整数，按编号换算", raw)
		} else {
			var name string
			if json.Unmarshal(raw, &name) == nil {
				name = strings.ToLower(strings.TrimSpace(name))
				for _, known := range kind.codes {
					if known == name {
						tolerance.note(interactionMethod, path, kind.name+" MIME 枚举写成 MIME 字符串，按字符串", raw)
						return known
					}
				}
			}
			tolerance.note(interactionMethod, path, kind.name+" MIME 枚举不是整数，取缺省 MIME "+kind.unlisted, raw)
			return kind.unlisted
		}
	}
	if mimeType, ok := kind.codes[code]; ok {
		return mimeType
	}
	if code != 0 {
		tolerance.note(interactionMethod, path, kind.name+" MIME 枚举不在表里，取缺省 MIME "+kind.unlisted, raw)
	}
	return kind.unlisted
}

// decodeInteractionCitations 解码 content field 10 的引用（按上游的解码移植）：分组里的每项依次为正文中引用标记的起止字符位置与
// field 4 的来源网址，按来源输出引用事件（各协议与 Playground 的引用一样写进 annotations、citationMetadata 等）。引用是辅助元数据：
// 分组读不出、条目字段不足或缺少网址时跳过（写容错日志），起止位置读不出时按 0
func decodeInteractionCitations(value json.RawMessage, raw json.RawMessage, emit func(Event) error, tolerance *decodeTolerance) error {
	groups, err := rawArray(value, "$delta[1][9]", raw)
	if err != nil {
		tolerance.note(interactionMethod, "$delta[1][9]", "引用列表不是数组，已丢弃", value)
		return nil
	}
	for _, group := range groups {
		items, err := rawArray(group, "$delta[1][9][]", raw)
		if err != nil {
			tolerance.note(interactionMethod, "$delta[1][9][]", "引用分组不是数组，已跳过", group)
			continue
		}
		for _, item := range items {
			fields, err := rawArray(item, "$delta[1][9][][]", raw)
			if err != nil || len(fields) < 4 {
				tolerance.note(interactionMethod, "$delta[1][9][][]", "引用字段不足，已跳过该条目", item)
				continue
			}
			start, ok := lenientInt64(fields[0])
			if !ok && !isJSONNull(fields[0]) {
				tolerance.note(interactionMethod, "$delta[1][9][][0]", "引用起始位置不是整数，按 0", fields[0])
			}
			end, ok := lenientInt64(fields[1])
			if !ok && !isJSONNull(fields[1]) {
				tolerance.note(interactionMethod, "$delta[1][9][][1]", "引用结束位置不是整数，按 0", fields[1])
			}
			urls := interactionStrings(fields[3])
			if len(urls) == 0 {
				tolerance.note(interactionMethod, "$delta[1][9][][3]", "引用条目缺少网址，已跳过该条目", item)
				continue
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

// decodeInteractionUsage 解码 Usage：输入、输出、思考与总 token 位于索引 0、4、8、9。各数为整数时与原先相同（整数写法的数字字符串
// 如 "7" 同样按整数直接读取，不写日志）；带小数的数与非整数写法的数字字符串按整数换算（写一行容错日志）；总数缺位或为 0 时按输入、输出、思考三项之和（写一行容错日志，原先按缺少总数中止整次生成）。用量不是
// 数组、某个数读不出、各数全为 0 时读不出用量（ok 为假），由调用方改用本地估算
func decodeInteractionUsage(raw json.RawMessage, tolerance *decodeTolerance) (*Usage, bool) {
	values, err := rawArray(raw, "$usage", raw)
	if err != nil {
		return nil, false
	}
	usage := &Usage{}
	for _, field := range []struct {
		index  int
		target *int64
	}{{0, &usage.InputTokens}, {4, &usage.OutputTokens}, {8, &usage.ReasoningTokens}, {9, &usage.TotalTokens}} {
		value := rawAt(values, field.index)
		if isJSONNull(value) {
			continue
		}
		number, err := rawInt64(value, "$usage", raw)
		if err != nil {
			var ok bool
			if number, ok = lenientInt64(value); !ok {
				return nil, false
			}
			tolerance.note(interactionMethod, fmt.Sprintf("$event[19][0][12][%d]", field.index), "用量不是整数，按整数换算", value)
		}
		*field.target = number
	}
	if usage.TotalTokens == 0 {
		usage.TotalTokens = usage.InputTokens + usage.OutputTokens + usage.ReasoningTokens
		if usage.TotalTokens == 0 {
			return nil, false
		}
		tolerance.note(interactionMethod, "$event[19][0][12][9]", "用量缺少总数，按输入、输出、思考之和", raw)
	}
	return usage, true
}
