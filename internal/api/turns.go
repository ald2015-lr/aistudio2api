package api

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// continuationPrompt 为历史以模型工具调用开头时补在最前面的用户消息
const continuationPrompt = "Continue."

// prefillContinuationPrompt 为历史以模型消息结尾（预填充、续写）时补在最后的用户消息（见 aistudio.PrefillContinuationPrompt）。
// 不接受以模型轮结尾的模型改为请模型从上一条消息结尾处接着写；接受的模型（aistudio.NativePrefillModel）由 prepareGenerate 原样发送
const prefillContinuationPrompt = aistudio.PrefillContinuationPrompt

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
//   - 历史中任何位置关联不到调用的工具结果（客户端截断或压缩上下文时把调用那一轮裁掉了、同一个结果发了两次）：改写成文字说明，
//     放在这组工具结果之后的用户消息里；关联上而没有名称的结果补上调用的名称，见 orphanResultsAsText
//
// 除孤立工具结果与结果名称外只调整分轮，不改动任何内容与思考签名；返回修正后的历史与修正次数
func normalizeTurns(contents []aistudio.Content) ([]aistudio.Content, int) {
	result, fixes := normalizeTurnSplits(contents)
	// 以模型的文字消息结尾时补一条用户消息；以工具调用结尾说明缺少工具结果，属于客户端错误，保持原样
	if aistudio.EndsWithModelText(result) {
		result = aistudio.AppendPrefillContinuation(result)
		fixes++
	}
	return result, fixes
}

// normalizeTurnSplits 同 normalizeTurns，但历史以模型的文字消息结尾时不补用户消息（由调用方按模型决定，见 prepareGenerate）
func normalizeTurnSplits(contents []aistudio.Content) ([]aistudio.Content, int) {
	contents, fixes := orphanResultsAsText(contents)
	result := make([]aistudio.Content, 0, len(contents)+2)
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
	return result, fixes
}

// orphanResultsAsText 在整段历史里为工具结果找对应的调用（改写成文字参照上游 Mag1cFall/AIStudio2API d94bf91，关联顺序与说明位置不同：
// 上游在编码层逐个结果依次按 ID、唯一待返回调用、同名调用关联并原位替换；这里放在协议层做）：按顺序记下
// 待返回的函数调用（普通用户消息之后清空，没有部件的消息编码时整条跳过、不算，与编码时的规则相同）；一组连续的工具结果先全部按调用 ID
// 关联（同 ID 的调用不止一个时先找同名的），剩下的有名称的结果按同名的待返回调用（多个时取最早的）、没有名称的结果按唯一的待返回调用关联。
// 先按 ID、再按名称、最后按唯一：不然一个过时的无名结果会抢走后面按 ID 本该关联上的调用，排在前面的无名结果也会因为同名结果还没取走调用
// 而被判为孤立。关联上的调用不再待返回，结果没有名称时补上调用的名称（编码时不用再关联）。关联不到调用的结果
// （客户端或中转截断、压缩上下文时把调用那一轮裁掉了，new-api 这时发出的 functionResponse 也没有名称；同一个结果发了两次）原样发给上游
// 会被拒绝（函数结果与调用的数量不一致、或缺少名称无法编码），改写成文字说明，写法与 Anthropic 入口渲染服务端工具历史的方式一致：
// "Tool result <名称>: <结果 JSON>"。结果里取出的媒体随文字说明一起留在用户消息里。
//
// 改写的说明不能留在原处：调用与结果之间插进一条用户消息，上游会因为函数调用之后不是函数结果而拒绝。所以先暂存，到这组工具结果结束
// （下一条不含工具结果的消息之前，或历史末尾）时作为一条用户消息放在这组结果之后；整条消息都是孤立结果时整条移走，其余结果原样留下。
// 历史开头的孤立结果同样如此（原先只处理这一种）。在协议层改写：WAA binding（aistudio/service.go）按改写后的历史计算，与实际发送的一致。
// 不改动传入的切片，返回改写后的历史与改写的结果数
func orphanResultsAsText(contents []aistudio.Content) ([]aistudio.Content, int) {
	if !slices.ContainsFunc(contents, hasFunctionResult) {
		return contents, 0
	}
	result := make([]aistudio.Content, 0, len(contents)+1)
	rewritten, changed := 0, false
	var pending []aistudio.FunctionCall
	for index := 0; index < len(contents); {
		content := contents[index]
		if !hasFunctionResult(content) {
			if content.Role == aistudio.RoleUser && len(content.Parts) > 0 {
				// 与编码时相同：普通用户消息之后之前的调用不再待返回；没有部件的消息（Responses 的空用户消息）编码时整条跳过、不清空，
				// 这里同样不清空（随后 normalizeTurnSplits 丢掉它），否则夹在调用与结果之间时本可按 ID 关联的结果被改写成文字
				pending = nil
			}
			for _, part := range content.Parts {
				if part.FunctionCall != nil {
					pending = append(pending, *part.FunctionCall)
				}
			}
			result = append(result, content)
			index++
			continue
		}
		end := index
		for end < len(contents) && hasFunctionResult(contents[end]) {
			end++
		}
		group, orphans, count, groupChanged := linkResultGroup(contents[index:end], &pending)
		result = append(result, group...)
		if len(orphans) > 0 {
			// 这组工具结果之后的用户消息：之后调用不再待返回（编码时同样在普通用户消息处清空）
			result = append(result, aistudio.Content{Role: aistudio.RoleUser, Parts: orphans})
			pending = nil
		}
		rewritten += count
		changed = changed || groupChanged
		index = end
	}
	if !changed {
		return contents, 0
	}
	return result, rewritten
}

// linkResultGroup 关联一组连续的工具结果消息（规则见 orphanResultsAsText），pending 为待返回的调用（关联上的删去）。返回留下的消息
// （关联上的结果与其余内容，整条都是孤立结果的消息不留）、孤立结果改写的文字说明与随它们的媒体、改写的结果数，以及是否有改动
func linkResultGroup(group []aistudio.Content, pending *[]aistudio.FunctionCall) ([]aistudio.Content, []aistudio.Part, int, bool) {
	type location struct{ content, part int }
	var locations []location
	for contentIndex, content := range group {
		for partIndex, part := range content.Parts {
			switch {
			case part.FunctionCall != nil:
				*pending = append(*pending, *part.FunctionCall)
			case part.FunctionResult != nil:
				locations = append(locations, location{contentIndex, partIndex})
			}
		}
	}
	calls := *pending
	taken := make([]bool, len(calls))
	matched := make(map[location]int, len(locations))
	find := func(accept func(aistudio.FunctionCall) bool) int {
		for callIndex, call := range calls {
			if !taken[callIndex] && accept(call) {
				return callIndex
			}
		}
		return -1
	}
	// 第一遍按调用 ID：同 ID 的调用不止一个时先找同名的
	for _, at := range locations {
		result := group[at.content].Parts[at.part].FunctionResult
		if result.ID == "" {
			continue
		}
		callIndex := find(func(call aistudio.FunctionCall) bool {
			return call.ID == result.ID && (result.Name == "" || call.Name == result.Name)
		})
		if callIndex < 0 {
			callIndex = find(func(call aistudio.FunctionCall) bool { return call.ID == result.ID })
		}
		if callIndex >= 0 {
			taken[callIndex], matched[at] = true, callIndex
		}
	}
	// 第二遍把有名称的结果按同名的待返回调用（多个时取最早的）关联，第三遍把没有名称的结果按唯一的待返回调用关联。有名称的先关联：
	// 不然排在前面的无名结果面对还没被同名结果取走的几个调用，判为孤立，有名称的结果随后又按名称关联，留下一个没有结果的调用
	for _, named := range []bool{true, false} {
		for _, at := range locations {
			if _, ok := matched[at]; ok {
				continue
			}
			result := group[at.content].Parts[at.part].FunctionResult
			if (result.Name != "") != named {
				continue
			}
			callIndex := -1
			if named {
				callIndex = find(func(call aistudio.FunctionCall) bool { return call.Name == result.Name })
			} else {
				remaining := 0
				for index := range calls {
					if !taken[index] {
						remaining++
						callIndex = index
					}
				}
				if remaining != 1 {
					callIndex = -1
				}
			}
			if callIndex >= 0 {
				taken[callIndex], matched[at] = true, callIndex
			}
		}
	}
	left := make([]aistudio.FunctionCall, 0, len(calls))
	for index, call := range calls {
		if !taken[index] {
			left = append(left, call)
		}
	}
	*pending = left

	kept := make([]aistudio.Content, 0, len(group))
	var orphans []aistudio.Part
	rewritten, changed := 0, false
	for contentIndex, content := range group {
		parts := make([]aistudio.Part, 0, len(content.Parts))
		// moved 为孤立结果的文字说明与随它们的媒体；whole 为整条消息按原顺序改写成用户消息时的部件（这条消息没有关联上的结果时用它）
		var moved, whole []aistudio.Part
		results := 0
		// orphaned 为当前在孤立结果之后：紧随其后、从工具结果取出的媒体跟着文字说明走
		orphaned := false
		for partIndex, part := range content.Parts {
			if part.FunctionResult == nil {
				plain := part
				plain.FromToolResult = false
				whole = append(whole, plain)
				if orphaned && part.FromToolResult {
					moved = append(moved, plain)
					continue
				}
				orphaned = false
				parts = append(parts, part)
				continue
			}
			callIndex, ok := matched[location{contentIndex, partIndex}]
			if !ok {
				text := aistudio.Part{Text: orphanResultText(*part.FunctionResult)}
				moved, whole = append(moved, text), append(whole, text)
				rewritten++
				orphaned = true
				continue
			}
			orphaned = false
			results++
			if part.FunctionResult.Name == "" {
				named := *part.FunctionResult
				named.Name = calls[callIndex].Name
				part.FunctionResult = &named
				changed = true
			}
			parts = append(parts, part)
		}
		if len(moved) > 0 {
			changed = true
		}
		if results == 0 {
			// 整条都是孤立结果：按原顺序整条改成用户消息里的部件（同一条消息里的文字等一起移走，取出的媒体不再标为工具结果）
			orphans = append(orphans, whole...)
			continue
		}
		orphans = append(orphans, moved...)
		content.Parts = parts
		kept = append(kept, content)
	}
	return kept, orphans, rewritten, changed
}

// orphanResultText 为孤立工具结果的文字说明：名称为空时用调用 ID
func orphanResultText(result aistudio.FunctionResult) string {
	label := result.Name
	if label == "" && result.ID != "" {
		label = "(" + result.ID + ")"
	}
	content := strings.TrimSpace(string(result.Content))
	if label == "" {
		return "Tool result: " + content
	}
	return "Tool result " + label + ": " + content
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

// prepareGenerate 在发往上游前整理请求：先修正对话历史分轮，再解析模型名后缀（Omni 模型另整理视频设置）。
// 各入口在此之前已按 OMNI_IGNORE_SYSTEM_PROMPT 去掉发给 Omni 视频模型的系统提示（见 omni_system.go），预填充判断与分轮合并按去掉后的对话进行。
// 返回是否需要隐藏思维链；Omni 视频设置不成立（例如非第一轮指定视频任务、帧率不是 24fps）时返回参数错误，调用方在选号前按 400 返回
//
// 历史以模型的文字消息结尾（预填充、续写）时按解析后的模型处理：接受以模型轮结尾的模型（aistudio.NativePrefillModel，
// 3.5 以前的模型与 gemma-）原样发送、靠模型原生续写（标记 PrefillTail，上游仍拒绝时生成服务改回补用户消息重试一次），
// 其余模型在末尾补一条请模型从这里接着写的用户消息
func (s *server) prepareGenerate(ctx context.Context, request *aistudio.GenerateRequest) (bool, error) {
	request.Contents, _ = normalizeTurnSplits(request.Contents)
	hide, err := s.applyModelAlias(ctx, request)
	if err != nil || !aistudio.EndsWithModelText(request.Contents) {
		return hide, err
	}
	if aistudio.NativePrefillModel(request.Model) {
		request.PrefillTail = true
		aistudio.TraceFromContext(ctx).Note("客户端最后一条是模型消息（预填充）：模型接受以模型轮结尾的请求，原样发送、由模型原生续写")
		return hide, nil
	}
	request.Contents = aistudio.AppendPrefillContinuation(request.Contents)
	aistudio.TraceFromContext(ctx).Note("客户端最后一条是模型消息（预填充）：已在末尾补一条请模型从这里接着写的用户消息")
	return hide, nil
}
