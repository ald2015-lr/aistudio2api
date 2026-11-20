package aistudio

import (
	"encoding/json"
	"strings"
)

type generatedOutputParts struct {
	visible   []Part
	reasoning []Part
	// visibleText 与 reasoningText 为两个列表最后一个文字 part 的累加缓冲：逐块 last.Text += text 每次都复制已有的全部文字，
	// 几万块的长回复总耗时与长度平方成正比（几百 KB 的回复要近 1 秒、分配几 GB）。缓冲按倍数扩容，part 的 Text 每次换成缓冲的
	// String()（不复制），读到的 Text 与原先逐块拼接的结果相同。结构体按值复制（续写各轮的 parts、重新生成时清空）后各自继续累加时，
	// 缓冲长度与 Text 对不上的一方从 Text 另起缓冲，不会混进另一方的文字
	visibleText, reasoningText *strings.Builder
}

func (output *generatedOutputParts) observe(event Event) {
	switch event.Kind {
	case EventText:
		appendOutputText(&output.visible, &output.visibleText, event.Text, false, event.ThoughtSignature)
	case EventReasoning:
		appendOutputText(&output.reasoning, &output.reasoningText, event.Text, true, event.ThoughtSignature)
	case EventToolCall:
		if event.ToolCall != nil {
			call := *event.ToolCall
			call.Arguments = append(json.RawMessage(nil), call.Arguments...)
			output.visible = append(output.visible, Part{FunctionCall: &call, ThoughtSignature: event.ThoughtSignature})
		}
	case EventExecutableCode:
		if event.ExecutableCode != nil {
			code := *event.ExecutableCode
			output.visible = append(output.visible, Part{ExecutableCode: &code, ThoughtSignature: event.ThoughtSignature})
		}
	case EventCodeExecutionResult:
		if event.CodeExecutionResult != nil {
			result := *event.CodeExecutionResult
			output.visible = append(output.visible, Part{CodeExecutionResult: &result, ThoughtSignature: event.ThoughtSignature})
		}
	case EventMedia:
		output.appendMedia(event)
	}
}

// appendOutputText 把一段文字并入 parts 最后一个同类（思考与否、签名相同）的文字 part，没有时另起一个；buffer 为这个列表的累加缓冲
func appendOutputText(parts *[]Part, buffer **strings.Builder, text string, thought bool, signature string) {
	if text == "" {
		return
	}
	if len(*parts) > 0 {
		last := &(*parts)[len(*parts)-1]
		if last.Text != "" && last.Thought == thought && last.ThoughtSignature == signature {
			// 缓冲只追加：长度与 Text 相同说明之后没有别人往里写过，内容就是 Text；否则（新的 part、结构体复制后对方先写了）从 Text 另起
			if *buffer == nil || (*buffer).Len() != len(last.Text) {
				*buffer = &strings.Builder{}
				(*buffer).Grow(2 * (len(last.Text) + len(text)))
				(*buffer).WriteString(last.Text)
			}
			(*buffer).WriteString(text)
			last.Text = (*buffer).String()
			return
		}
	}
	*parts = append(*parts, Part{Text: text, Thought: thought, ThoughtSignature: signature})
	*buffer = nil
}

func (output *generatedOutputParts) appendMedia(event Event) {
	if event.Media == nil {
		return
	}
	if len(event.Media.Data) == 0 {
		if event.Media.URL != "" {
			output.visible = append(output.visible, Part{
				File:             &FileRef{ID: event.Media.URL, Name: event.Media.Name, MIME: event.Media.MIME},
				ThoughtSignature: event.ThoughtSignature,
			})
		}
		return
	}
	if len(output.visible) > 0 {
		last := &output.visible[len(output.visible)-1]
		if last.InlineData != nil && last.InlineData.MIME == event.Media.MIME && last.ThoughtSignature == event.ThoughtSignature {
			last.InlineData.Data = append(last.InlineData.Data, event.Media.Data...)
			return
		}
	}
	data := append([]byte(nil), event.Media.Data...)
	output.visible = append(output.visible, Part{
		InlineData: &Blob{MIME: event.Media.MIME, Data: data}, ThoughtSignature: event.ThoughtSignature,
	})
}

func localCompleteUsage(request GenerateRequest, output generatedOutputParts) *Usage {
	inputTokens := localContentsTokens(request.Contents)
	if request.System != "" {
		inputTokens += localTextTokens(request.System) + 1
	}
	toolTokens := localToolTokens(request.Tools)
	reasoningTokens := localPartsTokens(output.reasoning)
	outputTokens := localPartsTokens(output.visible)
	return &Usage{
		InputTokens: inputTokens, ToolTokens: toolTokens, ReasoningTokens: reasoningTokens,
		OutputTokens: outputTokens, TotalTokens: inputTokens + toolTokens + reasoningTokens + outputTokens,
	}
}

// countedCompleteUsage 使用权威输入总数补全本地停止用量
func countedCompleteUsage(request GenerateRequest, output generatedOutputParts, count TokenCount) *Usage {
	toolTokens := localToolTokens(request.Tools)
	if toolTokens > count.InputTokens {
		toolTokens = count.InputTokens
	}
	inputTokens := count.InputTokens - toolTokens
	reasoningTokens := localPartsTokens(output.reasoning)
	outputTokens := localPartsTokens(output.visible)
	return &Usage{
		InputTokens: inputTokens, ToolTokens: toolTokens, ReasoningTokens: reasoningTokens,
		OutputTokens: outputTokens, TotalTokens: count.InputTokens + reasoningTokens + outputTokens,
	}
}

// EstimatedInputTokens 返回文本、工具和引用元数据的本地输入 Token 估算
func EstimatedInputTokens(request GenerateRequest) int64 {
	inputTokens := localContentsTokens(request.Contents)
	if request.System != "" {
		inputTokens += localTextTokens(request.System) + 1
	}
	return inputTokens + localToolTokens(request.Tools)
}

func localContentsTokens(contents []Content) int64 {
	var total int64
	for _, content := range contents {
		total++
		total += localPartsTokens(content.Parts)
	}
	return total
}

func localPartsTokens(parts []Part) int64 {
	var total int64
	for _, part := range parts {
		total += localPartTokens(part)
	}
	return total
}

func localPartTokens(part Part) int64 {
	switch {
	case part.Text != "":
		return localTextTokens(part.Text)
	case part.ExternalMedia != nil:
		return localTextTokens(part.ExternalMedia.URL)
	case part.File != nil:
		return localTextTokens(part.File.ID) + localTextTokens(part.File.Name)
	case part.FunctionCall != nil:
		return localTextTokens(part.FunctionCall.Name) + localTextTokens(string(part.FunctionCall.Arguments))
	case part.FunctionResult != nil:
		return localTextTokens(part.FunctionResult.Name) + localTextTokens(string(part.FunctionResult.Content))
	case part.ExecutableCode != nil:
		return localTextTokens(part.ExecutableCode.Language) + localTextTokens(part.ExecutableCode.Code)
	case part.CodeExecutionResult != nil:
		return localTextTokens(part.CodeExecutionResult.Outcome) + localTextTokens(part.CodeExecutionResult.Output)
	default:
		return 0
	}
}

func localToolTokens(tools Tools) int64 {
	if tools.ToolConfig.Mode == "none" ||
		(len(tools.Functions) == 0 && len(tools.Google) == 0 && tools.GoogleSearch == nil) {
		return 0
	}
	encoded, _ := json.Marshal(tools)
	return localTextTokens(string(encoded))
}
