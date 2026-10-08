package aistudio

import "context"

// SentInput 为一次尝试实际发往上游的输入：工具约束写进系统指令、只保留指定函数、truncation=auto 删除较早轮次之后的内容。
// 降级判定用它校准"字数 → token"的比例，上游返回的输入 token 数按的就是这份内容
type SentInput struct {
	Channel  Channel
	System   string
	Contents []Content
	Tools    Tools
	// SchemaHintChars 为编码时附在函数说明或结构化输出根说明里的完整 Schema 字数（只有 Playground 会附，Build 原样发送 Schema）
	SchemaHintChars int64
}

type sentInputContextKey struct{}

// ContextWithSentInputObserver 在每次尝试编码发送前报告实际发往上游的输入
func ContextWithSentInputObserver(ctx context.Context, observer func(SentInput)) context.Context {
	if observer == nil {
		return ctx
	}
	return context.WithValue(ctx, sentInputContextKey{}, observer)
}

// reportSentInput 报告本次尝试的实际输入；没有观察者时不做任何计算（附 Schema 的字数需要再编码一遍 Schema）
func reportSentInput(ctx context.Context, channel Channel, request GenerateRequest) {
	observer, _ := ctx.Value(sentInputContextKey{}).(func(SentInput))
	if observer == nil {
		return
	}
	input := SentInput{Channel: channel, System: request.System, Contents: request.Contents, Tools: request.Tools}
	if channel != ChannelBuild {
		input.SchemaHintChars = playgroundSchemaHintChars(request)
	}
	observer(input)
}

// playgroundSchemaHintChars 返回 Playground 编码时附在函数说明（降级或 strict）与结构化输出根说明（降级）里的完整 Schema 字数
func playgroundSchemaHintChars(request GenerateRequest) int64 {
	var chars int64
	if request.Tools.ToolConfig.Mode != "none" {
		for _, declaration := range request.Tools.Functions {
			if _, hint, err := encodeFunctionDeclarationHint(declaration); err == nil {
				chars += hint
			}
		}
	}
	if len(request.Config.ResponseSchema) > 0 {
		if _, hint, err := encodeResponseSchemaHint(request.Config.ResponseSchema); err == nil {
			chars += hint
		}
	}
	return chars
}
