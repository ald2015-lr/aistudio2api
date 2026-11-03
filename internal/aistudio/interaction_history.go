package aistudio

import (
	"fmt"
	"strings"
)

// InteractionToolHistoryAsText 把发往 Interaction 模型（gemini-omni）的对话里的函数调用、函数结果与代码执行 part 改为文本，保留上下文：
// CreateInteractionStream 的 step 只接受文本、图片与视频（见 encodeInteractionSteps），请求也不带工具（见 InteractionToolsNote）。
// 同一段对话先用其他模型调用过函数（new-api、Chatbox 自动附带的 save_memory 等），再切到 Omni 时历史里就有这些 part（OpenAI 格式为
// tool_calls 与 role=tool 的消息，经 new-api 的 Gemini 渠道为 functionCall 与 functionResponse），原先选到账号后编码报
// “this part type is not supported”（函数结果所在的 tool 消息报 “does not accept tool messages”），按 400 结束。
// 写法与协议层改写孤立工具结果、Anthropic 入口渲染服务端工具历史的文字说明一致：“Tool call <名称>: <参数 JSON>”、
// “Tool result <名称>: <结果 JSON>”，代码执行为 “Executable code (<语言>):” 与 “Code execution result (<结果>):”。
// 结果没有名称时（OpenAI 的 tool 消息只带 tool_call_id）按调用 ID 取前面调用的名称，取不到时写调用 ID。
// 函数结果所在的 tool 消息改为用户消息，与相邻的同角色消息合并（不出现连续两条用户消息）；思考签名随调用丢掉（Omni 用不上）。
// 不改动传入的切片；没有这类 part 时原样返回。返回改写后的对话与改写的 part 数
func InteractionToolHistoryAsText(contents []Content) ([]Content, int) {
	if !interactionToolHistory(contents) {
		return contents, 0
	}
	// names 为对话里各函数调用的 ID 与名称，给没有名称的结果补名
	names := make(map[string]string)
	for _, content := range contents {
		for _, part := range content.Parts {
			if call := part.FunctionCall; call != nil && strings.TrimSpace(call.ID) != "" && strings.TrimSpace(call.Name) != "" {
				names[strings.TrimSpace(call.ID)] = call.Name
			}
		}
	}
	converted := 0
	result := make([]Content, 0, len(contents))
	// merged 标记 result 中由 tool 消息改写来、或并入过改写内容的消息：只有它们与相邻的同角色消息合并，其余消息的分轮不变
	merged := make([]bool, 0, len(contents))
	for _, content := range contents {
		role, fromTool := content.Role, content.Role == RoleTool
		if fromTool {
			role = RoleUser
		}
		parts := make([]Part, 0, len(content.Parts))
		for _, part := range content.Parts {
			if text, ok := interactionToolPartText(part, names); ok {
				parts = append(parts, Part{Text: text})
				converted++
				continue
			}
			parts = append(parts, part)
		}
		// 只合并来源相同的消息：system 角色消息转成的轮次（FromSystem）与普通消息不合并，标记随消息保留，
		// 发送前按所选账户目录补做的系统提示去除（见 generate.go）仍能认出这些轮次
		if last := len(result) - 1; last >= 0 && result[last].Role == role && result[last].FromSystem == content.FromSystem && (fromTool || merged[last]) {
			result[last].Parts = append(result[last].Parts, parts...)
			merged[last] = merged[last] || fromTool
			continue
		}
		// 复制原消息再换角色与 part，保留 FromSystem 等不参与编码的标记
		content.Role, content.Parts = role, parts
		result = append(result, content)
		merged = append(merged, fromTool)
	}
	return result, converted
}

// interactionToolHistory 判断对话里有要改为文本的工具历史：tool 消息，或函数调用、函数结果、代码执行 part
func interactionToolHistory(contents []Content) bool {
	for _, content := range contents {
		if content.Role == RoleTool {
			return true
		}
		for _, part := range content.Parts {
			if _, ok := interactionToolPartText(part, nil); ok {
				return true
			}
		}
	}
	return false
}

// interactionToolPartText 返回工具历史 part 的文字说明；不是函数调用、函数结果或代码执行时返回 false。结果没有名称时按调用 ID 从 names
// 取调用的名称，仍为空时用调用 ID
func interactionToolPartText(part Part, names map[string]string) (string, bool) {
	label := func(name, id string) string {
		if name = strings.TrimSpace(name); name != "" {
			return " " + name
		}
		if id = strings.TrimSpace(id); id != "" {
			return " (" + id + ")"
		}
		return ""
	}
	switch {
	case part.FunctionCall != nil:
		call := part.FunctionCall
		arguments := strings.TrimSpace(string(call.Arguments))
		if arguments == "" || arguments == "null" {
			arguments = "{}"
		}
		return "Tool call" + label(call.Name, call.ID) + ": " + arguments, true
	case part.FunctionResult != nil:
		result := part.FunctionResult
		name := result.Name
		if strings.TrimSpace(name) == "" {
			name = names[strings.TrimSpace(result.ID)]
		}
		return "Tool result" + label(name, result.ID) + ": " + strings.TrimSpace(string(result.Content)), true
	case part.ExecutableCode != nil:
		code := part.ExecutableCode
		language := strings.TrimSpace(code.Language)
		if language == "" {
			language = "unspecified"
		}
		return "Executable code (" + language + "):\n" + code.Code, true
	case part.CodeExecutionResult != nil:
		execution := part.CodeExecutionResult
		outcome := strings.TrimSpace(execution.Outcome)
		if outcome == "" {
			outcome = "unspecified"
		}
		text := "Code execution result (" + outcome + ")"
		if output := strings.TrimSpace(execution.Output); output != "" {
			text += ":\n" + output
		}
		if failure := strings.TrimSpace(execution.Error); failure != "" {
			text += "\nError: " + failure
		}
		return text, true
	}
	return "", false
}

// InteractionToolHistoryNote 为 Interaction 模型（gemini-omni）的对话历史里有工具调用、结果与代码执行、已改为文本时写进请求日志的说明
func InteractionToolHistoryNote(count int) string {
	return fmt.Sprintf("Omni 模型不调用工具：对话历史里的 %d 个函数调用、函数结果与代码执行已改为文本发送（CreateInteractionStream 只接受文本、图片与视频）", count)
}
