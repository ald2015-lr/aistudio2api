package aistudio

import (
	"fmt"
	"regexp"
	"strings"
)

// BillingHeaderPrefix 为 Claude Code 写在系统提示开头的计费归属行的开头（形如 x-anthropic-billing-header: cc_version=…; cc_entrypoint=…;）
const BillingHeaderPrefix = "x-anthropic-billing-header:"

// billingHeader 匹配行首的计费行头部：BillingHeaderPrefix 加若干以分号结尾的 key=value 字段（Claude Code 的写法，值里没有空白），
// 最后一个字段没有分号时只在它一直到行尾时才算；字段之后的空格、Tab 一起匹配。只匹配这些字段，不包括同一行里其后的其他文本
var billingHeader = regexp.MustCompile(`(?m)^x-anthropic-billing-header:(?:[ \t]*[A-Za-z0-9_.-]+=[^;\s]*;)*(?:[ \t]*[A-Za-z0-9_.-]+=[^;\s]*[ \t]*\r?$)?[ \t]*`)

// DropClientBillingHeader 删除系统提示里 Claude Code 的计费行，返回删除后的系统提示与删除的处数。
// 这一行随客户端版本（可能也随会话）变化，对 Gemini 没有意义，又在系统提示最前面：每次不同就让整段系统提示前缀无法复用上游缓存，
// 还把客户端信息带给上游。只按行首前缀匹配（与上游 Mag1cFall/AIStudio2API 相同，行首有空白的不算），只删前缀与其后的 key=value 字段：
// 这一行没有别的内容时整行删除（与按行切分、去掉这一行再以换行拼接相同）；同一行在字段之后还有文本时保留这些文本——OpenAI Chat、
// Responses 与 Gemini 把系统提示的多段首尾相连，经 claude-code-router、LiteLLM 等转换器转来的计费块（不以换行结尾）会和下一段
// （身份句或正文）粘成一行，整行删除会连带删掉它们（上游逐段处理，不会这样）。其余内容逐字节不变；删除后只剩空白时返回空串
// （这段系统提示不再发送）。没有计费行时原样返回、删除处数为 0
func DropClientBillingHeader(system string) (string, int) {
	if !strings.Contains(system, BillingHeaderPrefix) {
		return system, 0
	}
	matches := billingHeader.FindAllStringIndex(system, -1)
	if len(matches) == 0 {
		return system, 0
	}
	result := make([]byte, 0, len(system))
	last := 0
	for _, match := range matches {
		start, end := match[0], match[1]
		lineEnd := len(system)
		if newline := strings.IndexByte(system[end:], '\n'); newline >= 0 {
			lineEnd = end + newline
		}
		result = append(result, system[last:start]...)
		switch {
		case strings.TrimSpace(system[end:lineEnd]) != "":
			// 同一行在字段之后还有文本（粘在一起的下一段）：只删字段，保留其后的文本
			last = end
		case lineEnd < len(system):
			// 这一行只有计费行：连同行尾的换行删除
			last = lineEnd + 1
		default:
			// 最后一行只有计费行：连同它前面的换行删除
			if len(result) > 0 && result[len(result)-1] == '\n' {
				result = result[:len(result)-1]
			}
			last = lineEnd
		}
	}
	result = append(result, system[last:]...)
	cleaned := string(result)
	if strings.TrimSpace(cleaned) == "" {
		cleaned = ""
	}
	return cleaned, len(matches)
}

// BillingHeaderNote 为删除计费行时写进请求日志的说明（INFO，见 GenerateRequest.InfoNotes），不含提示原文
func BillingHeaderNote(removed int, emptied bool) string {
	note := fmt.Sprintf("已删除系统提示里 Claude Code 的计费行（%s，%d 处）：随客户端版本变化、对 Gemini 没有意义，删去后系统提示前缀可复用上游缓存",
		strings.TrimSuffix(BillingHeaderPrefix, ":"), removed)
	if emptied {
		note += "；删除后系统提示为空，不再发送系统提示"
	}
	return note
}
