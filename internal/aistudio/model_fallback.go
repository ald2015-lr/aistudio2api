package aistudio

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
)

// 额度用完自动改用其他模型（MODEL_FALLBACKS，规则与触发条件见 app/model_fallback.go）用到的请求信息与工具函数

// RequestAlias 为协议层按模型名（后缀与 long- 前缀，见 api 包 applyModelAlias）整理请求之前的客户端设置与名称里的选项：
// 生成服务改用其他模型时按目标模型重新整理思考强度、联网搜索与自动续写（见 api.RetargetGenerateRequest），与客户端直接请求目标模型、
// 带同样的后缀时相同。只在协议层解析出文本对话模型时设置，其他请求为 nil
type RequestAlias struct {
	// ReasoningEffort、ThinkingBudget 为客户端原来的思考参数（按名称后缀改写之前）
	ReasoningEffort string
	ThinkingBudget  *int64
	// Level 为名称后缀指定的思考等级（minimal、low、medium、high），空串表示没有指定
	Level string
	// Online 为名称带 -online；OnlineAppended 为 -online 让协议层在 Tools.Google 里追加了 google_search（客户端原来没有）
	Online         bool
	OnlineAppended bool
	// Long 为名称带 long- 前缀
	Long bool
}

// ModelSwitch 记录生成服务把请求改用了哪个模型（额度用完改用其他模型）：协议层在每个公开 API 请求的 context 上挂一份
// （见 ContextWithModelSwitch），生成服务决定改用时在交出首个事件之前写入，协议层收到首个事件（非流式为读完结果）后据此在响应里
// 报告实际使用的模型、按它登记思考签名。没有改用时 Model 原样返回请求的模型。方法都 nil 安全
type ModelSwitch struct {
	mu     sync.Mutex
	from   string
	target string
}

type modelSwitchContextKey struct{}

// NewModelSwitch 创建一份空的改用记录
func NewModelSwitch() *ModelSwitch {
	return &ModelSwitch{}
}

// ContextWithModelSwitch 把改用记录挂到 context 上
func ContextWithModelSwitch(ctx context.Context, record *ModelSwitch) context.Context {
	if record == nil {
		return ctx
	}
	return context.WithValue(ctx, modelSwitchContextKey{}, record)
}

// ModelSwitchFromContext 返回 context 上的改用记录，没有时为 nil
func ModelSwitchFromContext(ctx context.Context) *ModelSwitch {
	if ctx == nil {
		return nil
	}
	record, _ := ctx.Value(modelSwitchContextKey{}).(*ModelSwitch)
	return record
}

// Switch 记下请求从 from 改用 target；target 为空时清除（改用之后仍按源模型的错误结束）
func (record *ModelSwitch) Switch(from string, target string) {
	if record == nil {
		return
	}
	record.mu.Lock()
	record.from, record.target = from, target
	if target == "" {
		record.from = ""
	}
	record.mu.Unlock()
}

// Model 返回响应里报告的模型：改用过时为目标模型，否则为 requested
func (record *ModelSwitch) Model(requested string) string {
	if record == nil {
		return requested
	}
	record.mu.Lock()
	defer record.mu.Unlock()
	if record.target == "" {
		return requested
	}
	return record.target
}

// From 返回改用前的模型，没有改用时为空
func (record *ModelSwitch) From() string {
	if record == nil {
		return ""
	}
	record.mu.Lock()
	defer record.mu.Unlock()
	return record.from
}

// skipThoughtSignatureValidator 为官方文档给出的跳过思考签名校验的占位值（编码时函数调用缺签名也补它，见 request.go 与 build.go）；
// newAPIThoughtSignature 为 new-api 把 Gemini 回复转成 OpenAI 格式再转回时给函数调用写的占位签名
const (
	skipThoughtSignatureValidator = "skip_thought_signature_validator"
	newAPIThoughtSignature        = "context_engineering_is_the_way_to_go"
)

// PlaceholderThoughtSignature 判断签名为空或是占位值（跳过校验值、new-api 的占位值）：不是哪个模型签发的真实思考签名
func PlaceholderThoughtSignature(signature string) bool {
	switch strings.TrimSpace(signature) {
	case "", skipThoughtSignatureValidator, newAPIThoughtSignature:
		return true
	default:
		return false
	}
}

// ReplaceThoughtSignatures 返回去掉“别的模型签发的”思考签名后的对话副本与处理的签名数：签名是签发模型在那一轮的加密思考上下文，
// 另一个模型校验时可能以 MISSING/INVALID thought signature 拒绝。keep 判断一个签名能不能原样发给这次的模型（为 nil 时都不能），
// 不能的按部分处理：函数调用的签名（Part 与 FunctionCall 上的）换成跳过校验的占位值，与本服务还原不到签名时的做法相同；
// 只有签名、没有其他内容的部分（Anthropic 的 thinking 块、Responses 的 reasoning 项、Gemini 原生只带签名的部分）整个去掉，
// 只清掉签名会留下空部分，编码时报“part 必须且只能设置一种内容”；其余部分（文字、思考、媒体等）只去掉签名（这些部分不要求签名）。
// 去掉部分后没有内容的消息一起去掉。已经是占位值的签名（PlaceholderThoughtSignature）不计数。不改动传入的切片
func ReplaceThoughtSignatures(contents []Content, keep func(signature string) bool) ([]Content, int) {
	replaced := 0
	result := make([]Content, 0, len(contents))
	for _, content := range contents {
		var parts []Part
		changed := false
		for partIndex, part := range content.Parts {
			call := part.FunctionCall
			original := part.ThoughtSignature
			if original == "" && call != nil {
				original = call.ThoughtSignature
			}
			// 没有签名（函数调用在编码时补跳过校验值），或签名可以原样发送
			if original == "" || (keep != nil && keep(original)) {
				if changed {
					parts = append(parts, part)
				}
				continue
			}
			if !changed {
				changed = true
				parts = append(make([]Part, 0, len(content.Parts)), content.Parts[:partIndex]...)
			}
			// 占位值（跳过校验值、new-api 写的占位值）不是哪个模型的思考上下文，照常换成跳过校验值或去掉但不计数：
			// 日志里的个数只算真实签名，经 new-api 时不会把每条 assistant 消息的占位值都算成丢掉的思考上下文
			if !PlaceholderThoughtSignature(original) {
				replaced++
			}
			if call != nil {
				copied := *call
				copied.ThoughtSignature = skipThoughtSignatureValidator
				part.FunctionCall = &copied
				part.ThoughtSignature = skipThoughtSignatureValidator
				parts = append(parts, part)
				continue
			}
			part.ThoughtSignature = ""
			if signatureOnlyPart(part) {
				continue
			}
			parts = append(parts, part)
		}
		if changed {
			if len(parts) == 0 && len(content.Parts) > 0 {
				continue
			}
			content.Parts = parts
		}
		result = append(result, content)
	}
	return result, replaced
}

// signatureOnlyPart 判断去掉签名后的部分没有任何内容
func signatureOnlyPart(part Part) bool {
	return part.Text == "" && part.InlineData == nil && part.ExternalMedia == nil && part.File == nil && part.FunctionCall == nil &&
		part.FunctionResult == nil && part.ExecutableCode == nil && part.CodeExecutionResult == nil
}

// ThoughtSignatureRestore 由协议层挂在每个公开 API 请求的 context 上（见 ContextWithThoughtSignatureRestore）：按 model 的作用域
// （同号池、同 API key，见 api 包 thoughtSignatureScope）为 contents 里签名缺失或为占位值、带调用 ID 的函数调用还原本服务签发过的真实签名，
// 按下标改写 contents，返回还原的个数。生成服务额度用完改用其他模型时按目标模型调用（协议层只按请求的模型还原过一次）
type ThoughtSignatureRestore func(ctx context.Context, model string, contents []Content) int

type thoughtSignatureRestoreKey struct{}

// ContextWithThoughtSignatureRestore 把签名还原函数挂到 context 上；restore 为 nil 时原样返回
func ContextWithThoughtSignatureRestore(ctx context.Context, restore ThoughtSignatureRestore) context.Context {
	if restore == nil {
		return ctx
	}
	return context.WithValue(ctx, thoughtSignatureRestoreKey{}, restore)
}

// ThoughtSignatureRestoreFromContext 返回 context 上的签名还原函数，没有时为 nil
func ThoughtSignatureRestoreFromContext(ctx context.Context) ThoughtSignatureRestore {
	if ctx == nil {
		return nil
	}
	restore, _ := ctx.Value(thoughtSignatureRestoreKey{}).(ThoughtSignatureRestore)
	return restore
}

// TextChatModel 判断模型是文本对话模型：目录标为对话模型，且不是图片、语音、音乐、视频、实时或 Interaction（Omni）模型
func TextChatModel(model Model) bool {
	if !model.Capabilities["chat_model"] || model.Capabilities["interactions_api"] {
		return false
	}
	for _, route := range []string{"image_route", "speech_route", "music_route", "video_route", "live_route", "interaction_route"} {
		if model.Capabilities[route] {
			return false
		}
	}
	return true
}

// TextGenerationRequest 判断请求是文本对话生成：没有图片生成标记、语音或转录设置，输出模态只有文本（或没有指定）
func TextGenerationRequest(request GenerateRequest) bool {
	if request.ImageRoute || request.Config.SpeechConfig != nil || request.Config.TranscriptionConfig != nil ||
		request.Config.ImageConfig != nil || request.Config.VideoConfig != nil {
		return false
	}
	for _, modality := range request.Config.ResponseModalities {
		if !strings.EqualFold(string(modality), string(ResponseModalityText)) {
			return false
		}
	}
	return true
}

// FallbackTargetUnsupported 返回目标模型接不了这次请求的原因（改用其他模型时逐个检查目标，接不了的跳过），能接时返回空串：
// 要支持 generateContent、是文本对话模型（Interaction 即 Omni、图片、语音等模型不接）、支持请求带的工具（函数声明、Google 搜索、
// 代码执行、网址上下文、地图等，与发送前的校验相同）。模型目录没有标明媒体输入与结构化输出能力的字段，文本对话模型照常接收图片、
// 音频、视频、PDF 输入与 responseSchema，这两项不在这里筛选
func FallbackTargetUnsupported(model Model, request GenerateRequest) string {
	if len(model.Methods) > 0 && !slices.Contains(model.Methods, "generateContent") {
		return "不支持 generateContent"
	}
	if model.Capabilities["interactions_api"] || model.Capabilities["interaction_route"] {
		return "是 Interaction（Omni）模型，请求方式不同"
	}
	if !TextChatModel(model) {
		return "不是文本对话模型"
	}
	if err := validateRequestedTools(request.Tools, model); err != nil {
		return fmt.Sprintf("不支持请求的工具（%v）", err)
	}
	return ""
}
