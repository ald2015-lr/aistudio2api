package aistudio

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// 视频模型忽略系统提示（api 层的 OMNI_IGNORE_SYSTEM_PROMPT）的共用规则：协议层按别名目录认出能输出视频的 Omni 模型时去掉
// 客户端的系统提示并记下说明；协议层没有认出（启动期间别名目录取不到、别名目录缓存还没刷新、新加入的账户是唯一带 Omni 的账户）时
// 只标记 GenerateRequest.IgnoreOmniSystemPrompt，生成服务按所选账户的目录在发送前按同一规则补做一次（见 ignoreOmniSystemPromptBeforeSend），
// 说明经 ReportRequestNote 写进该请求的管理日志。两处去掉的内容与说明完全相同

// SystemReminderOpen 与 SystemReminderClose 包住客户端 messages 里 system 角色消息的文本（Anthropic Messages 入口），
// 这一轮作为用户消息发送并标记为 Content.FromSystem
const (
	SystemReminderOpen  = "<system-reminder>\n"
	SystemReminderClose = "\n</system-reminder>"
)

// HasClientSystemPrompt 判断请求带了客户端的系统提示：系统提示不全是空白，或有 system 角色消息转成的一轮
func HasClientSystemPrompt(system string, contents []Content) bool {
	if strings.TrimSpace(system) != "" {
		return true
	}
	for _, content := range contents {
		if content.FromSystem {
			return true
		}
	}
	return false
}

// SystemPromptDrop 为一次去掉系统提示的结果；字数不含提示原文，system 角色消息不计 <system-reminder> 包裹
type SystemPromptDrop struct {
	// Chars 与 Turns 为去掉的字数与 system 角色消息条数
	Chars, Turns int
	// KeptChars 与 KeptTurns 为除系统提示外没有对话消息时保留下来（按用户消息发送）的字数与 system 角色消息条数
	KeptChars, KeptTurns int
}

// DropClientSystemPrompt 去掉系统提示与 system 角色消息转成的轮次。除系统提示外没有对话消息时：有 system 角色消息转成的轮次就只去掉系统提示，
// 这些轮次原样保留（本来就按用户消息发送）；只有系统提示时什么也不改（生成服务把只有系统提示的请求按用户消息发送，去掉后整个请求就空了）。
// 不改动传入的 contents 切片，有轮次要去掉时换成新切片
func DropClientSystemPrompt(system *string, contents *[]Content) SystemPromptDrop {
	systemChars := utf8.RuneCountInString(*system)
	remaining, turns, turnChars := 0, 0, 0
	for _, content := range *contents {
		if !content.FromSystem {
			remaining++
			continue
		}
		turns++
		for _, part := range content.Parts {
			turnChars += utf8.RuneCountInString(strings.TrimSuffix(strings.TrimPrefix(part.Text, SystemReminderOpen), SystemReminderClose))
		}
	}
	if remaining == 0 && turns == 0 {
		return SystemPromptDrop{KeptChars: systemChars}
	}
	*system = ""
	if remaining == 0 {
		return SystemPromptDrop{Chars: systemChars, KeptChars: turnChars, KeptTurns: turns}
	}
	if turns > 0 {
		filtered := make([]Content, 0, remaining)
		for _, content := range *contents {
			if !content.FromSystem {
				filtered = append(filtered, content)
			}
		}
		*contents = filtered
	}
	return SystemPromptDrop{Chars: systemChars + turnChars, Turns: turns}
}

// OmniSystemPromptNote 为去掉（或因为没有其他消息而保留）系统提示时写进请求日志的说明，不含提示原文
func OmniSystemPromptNote(drop SystemPromptDrop) string {
	switch {
	case drop.KeptTurns > 0 && drop.Chars > 0:
		return fmt.Sprintf("视频模型忽略系统提示：已去掉 %d 字的系统提示，不发给上游；请求除系统提示外没有对话消息，保留 %d 条 system 角色消息"+
			"（%d 字，按用户消息发送）（OMNI_IGNORE_SYSTEM_PROMPT）", drop.Chars, drop.KeptTurns, drop.KeptChars)
	case drop.KeptTurns > 0:
		return fmt.Sprintf("视频模型忽略系统提示：请求除系统提示外没有对话消息，保留 %d 条 system 角色消息（%d 字，按用户消息发送）（OMNI_IGNORE_SYSTEM_PROMPT）",
			drop.KeptTurns, drop.KeptChars)
	case drop.KeptChars > 0:
		return fmt.Sprintf("视频模型忽略系统提示：请求除系统提示外没有对话消息，保留 %d 字的系统提示（按用户消息发送）（OMNI_IGNORE_SYSTEM_PROMPT）", drop.KeptChars)
	case drop.Turns > 0:
		return fmt.Sprintf("视频模型忽略系统提示：已去掉 %d 字的系统提示（其中 %d 条为 system 角色消息），不发给上游（OMNI_IGNORE_SYSTEM_PROMPT）", drop.Chars, drop.Turns)
	}
	return fmt.Sprintf("视频模型忽略系统提示：已去掉 %d 字的系统提示，不发给上游（OMNI_IGNORE_SYSTEM_PROMPT）", drop.Chars)
}

// ignoreOmniSystemPromptBeforeSend 为协议层没有认出 Omni 的请求（IgnoreOmniSystemPrompt 为真）在发送前按所选账户的目录补做视频模型忽略系统提示：
// model 是能输出视频的 Omni 且请求仍带客户端的系统提示时按 DropClientSystemPrompt 去掉，返回要写进请求日志的说明（与协议层相同）。
// 本服务按工具设置追加在系统提示之后的要求（见 prepareToolRequest）不是客户端的系统提示，保留。其他模型、没有标记或没有系统提示时不改动请求
func ignoreOmniSystemPromptBeforeSend(request *GenerateRequest, model Model) (string, bool) {
	if !request.IgnoreOmniSystemPrompt || !OmniVideoModel(model) {
		return "", false
	}
	system, hints := request.System, ""
	if request.toolHints != "" {
		system, hints = request.clientSystem, request.toolHints
	}
	if !HasClientSystemPrompt(system, request.Contents) {
		return "", false
	}
	drop := DropClientSystemPrompt(&system, &request.Contents)
	if hints != "" {
		// 客户端的系统提示同样记下去掉之后的样子：同账号重新生成时按它重写工具要求（见 tool_guard.go），不能把去掉的系统提示又加回来
		request.clientSystem = system
		system = joinToolHints(system, hints)
	}
	request.System = system
	request.IgnoreOmniSystemPrompt = false
	return OmniSystemPromptNote(drop), true
}
