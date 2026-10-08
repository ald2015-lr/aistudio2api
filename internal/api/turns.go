package api

import (
	"context"
	"fmt"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// continuationPrompt 为历史以模型工具调用开头时补在最前面的用户消息
const continuationPrompt = "Continue."

// prefillContinuationPrompt 为历史以模型消息结尾（预填充、续写）时补在最后的用户消息。
// Gemini 3 等模型不接受以模型轮结尾的请求，改为请模型从上一条消息结尾处接着写；
// 客户端会把输出接在原消息后面，所以要求模型不要重复已有内容
const prefillContinuationPrompt = "Continue exactly from where your previous message ended. " +
	"Do not repeat any of it; output only the continuation."

// normalizeTurns 修正客户端常见的对话历史分轮问题，使其满足 Gemini 的要求：
//
//   - 连续多条模型消息合并为一轮：常见于客户端把"说明文字"和"工具调用"存成两条 assistant 消息，
//     或把并行工具调用拆成多条各含一个调用的 assistant 消息。否则上游返回 400
//     "function call turn comes immediately after a user turn or after a function response turn"
//   - 连续多条纯工具结果合并为一轮：并行调用的结果应在同一轮返回
//   - 历史以模型的工具调用开头（客户端截断上下文时把最早的用户消息裁掉了）：在前面补一条用户消息
//   - 工具结果中取出的图片（见 splitFunctionResultMedia）：移到这组工具结果之后的用户消息里，
//     保证工具结果那一轮只有工具结果、数量与调用一致
//   - 历史以模型的文字消息结尾（SillyTavern 等的预填充、"继续"续写）：在最后补一条请模型接着写的用户消息。
//     否则上游返回 400 "Requests ending with a model turn are not supported"
//
// 只调整分轮，不改动任何内容与思考签名；返回修正后的历史与修正次数
func normalizeTurns(contents []aistudio.Content) ([]aistudio.Content, int) {
	result := make([]aistudio.Content, 0, len(contents)+2)
	fixes := 0
	// pendingMedia 为当前这组工具结果中取出的图片，这组工具结果结束后放进紧随其后的用户消息
	var pendingMedia []aistudio.Part
	flushMedia := func(next *aistudio.Content) {
		if len(pendingMedia) == 0 {
			return
		}
		media := append([]aistudio.Part{{Text: toolMediaIntro}}, pendingMedia...)
		pendingMedia = nil
		fixes++
		// 紧接着就是普通用户消息时并入其中，避免连续两条用户消息
		if next != nil && next.Role == aistudio.RoleUser && !hasFunctionResult(*next) {
			next.Parts = append(media, next.Parts...)
			return
		}
		result = append(result, aistudio.Content{Role: aistudio.RoleUser, Parts: media})
	}
	for _, content := range contents {
		content, media := takeToolResultMedia(content)
		if len(content.Parts) == 0 {
			pendingMedia = append(pendingMedia, media...)
			continue
		}
		merged := false
		if count := len(result); count > 0 {
			last := &result[count-1]
			mergeModel := content.Role == aistudio.RoleAssistant && last.Role == aistudio.RoleAssistant
			mergeResults := functionResponseTurn(content) && functionResponseTurn(*last)
			if mergeModel || mergeResults {
				combined := make([]aistudio.Part, 0, len(last.Parts)+len(content.Parts))
				combined = append(combined, last.Parts...)
				last.Parts = append(combined, content.Parts...)
				fixes++
				merged = true
			}
		}
		if !merged {
			flushMedia(&content)
			result = append(result, content)
		}
		pendingMedia = append(pendingMedia, media...)
	}
	flushMedia(nil)
	if len(result) > 0 && result[0].Role == aistudio.RoleAssistant && hasFunctionCall(result[0]) {
		result = append([]aistudio.Content{{
			Role: aistudio.RoleUser, Parts: []aistudio.Part{{Text: continuationPrompt}},
		}}, result...)
		fixes++
	}
	// 以模型的文字消息结尾时补一条用户消息；以工具调用结尾说明缺少工具结果，属于客户端错误，保持原样
	if count := len(result); count > 0 && result[count-1].Role == aistudio.RoleAssistant && !hasFunctionCall(result[count-1]) {
		result = append(result, aistudio.Content{
			Role: aistudio.RoleUser, Parts: []aistudio.Part{{Text: prefillContinuationPrompt}},
		})
		fixes++
	}
	return result, fixes
}

// toolMediaIntro 放在工具结果图片与文件之前的说明
const toolMediaIntro = "以下是上面工具调用返回的图片或文件："

// takeToolResultMedia 从一轮消息中取出工具结果里的图片，并在每张图片前标注来自哪次工具调用
func takeToolResultMedia(content aistudio.Content) (aistudio.Content, []aistudio.Part) {
	var media []aistudio.Part
	kept := make([]aistudio.Part, 0, len(content.Parts))
	label := ""
	index := 0
	for _, part := range content.Parts {
		if part.FunctionResult != nil {
			label = part.FunctionResult.Name
			if label == "" {
				label = part.FunctionResult.ID
			}
			index = 0
		}
		if !part.FromToolResult {
			kept = append(kept, part)
			continue
		}
		index++
		part.FromToolResult = false
		media = append(media, aistudio.Part{Text: fmt.Sprintf("[工具 %s 返回的%s %d]", label, mediaKind(part), index)}, part)
	}
	content.Parts = kept
	return content, media
}

func hasFunctionResult(content aistudio.Content) bool {
	for _, part := range content.Parts {
		if part.FunctionResult != nil {
			return true
		}
	}
	return false
}

// functionResponseTurn 判断是否为只包含工具结果的一轮
func functionResponseTurn(content aistudio.Content) bool {
	if content.Role != aistudio.RoleTool && content.Role != aistudio.RoleUser || len(content.Parts) == 0 {
		return false
	}
	for _, part := range content.Parts {
		if part.FunctionResult == nil {
			return false
		}
	}
	return true
}

func hasFunctionCall(content aistudio.Content) bool {
	for _, part := range content.Parts {
		if part.FunctionCall != nil {
			return true
		}
	}
	return false
}

// prepareGenerate 在发往上游前整理请求：先修正对话历史分轮，再解析模型名后缀。
// 返回是否需要隐藏思维链
func (s *server) prepareGenerate(ctx context.Context, request *aistudio.GenerateRequest) bool {
	if count := len(request.Contents); count > 0 && request.Contents[count-1].Role == aistudio.RoleAssistant &&
		!hasFunctionCall(request.Contents[count-1]) {
		aistudio.TraceFromContext(ctx).Note("客户端最后一条是模型消息（预填充）：已在末尾补一条请模型从这里接着写的用户消息")
	}
	request.Contents, _ = normalizeTurns(request.Contents)
	return s.applyModelAlias(ctx, request)
}
