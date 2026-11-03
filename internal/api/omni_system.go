package api

import (
	"context"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// 视频模型忽略系统提示（OMNI_IGNORE_SYSTEM_PROMPT，默认开启，保存后立即生效）：
//
// 聊天客户端（如 Chatbox）每次都自动加系统提示（“You are a helpful assistant.” 加一段运行时信息）。线上实测，同样的“随机生成一个视频”，
// 不带系统提示时 Omni 正常返回视频，带上这句时 Omni 只回一段文字、说自己是文本助手、无法生成视频。开启时发给能输出视频的 Omni 模型
// （aistudio.OmniVideoModel；模型名可带视频、思考后缀与目录别名）的请求去掉客户端的系统提示，不发给上游：
//
//	OpenAI Chat Completions  system、developer 角色消息（不论在第几条）
//	Responses                instructions、input 里的 system、developer 消息，以及 previous_response_id 续接带上的系统指令
//	Anthropic Messages       顶层 system，与 messages 里的 system 角色消息（转成的 <system-reminder> 用户消息整轮去掉，见 aistudio.Content.FromSystem）
//	Gemini 原生接口          systemInstruction（system_instruction）
//	Interactions             system_instruction
//
// 去掉的字数写进该请求的管理日志（不写提示原文）；其他消息原样保留，去掉 system 角色消息不留下空轮次。除系统提示外没有任何对话消息时：
// 有 system 角色消息转成的轮次就只去掉系统提示，保留这些轮次（它们本来就按用户消息发送）；只有系统提示时不去掉（生成服务把只有系统提示的
// 请求按用户消息发送，去掉后整个请求就空了），同样记一行。本服务按工具设置在发送前追加的提示（tool_choice 等，见 aistudio 的工具约束）
// 不是客户端的系统提示，不在这里处理；Omni 请求的工具在生成服务选号前已整个去掉（见 app 的 startGenerate），不会追加这类提示。
// 关闭时与原先完全一致；其他模型（文本、图片、Veo、Lyria 等）不受影响。
//
// 各入口把请求转成 GenerateRequest（Responses、Interactions 接上续接历史）后先调用 ignoreOmniSystemPrompt，再恢复回传的媒体链接
// （restoreMediaLinks）与整理分轮（prepareGenerate）：之后的整理都按去掉后的对话进行，与不带系统提示的请求一致（例如末尾的 system 角色消息
// 去掉后，最后一条用户消息才是“这一轮”）。Gemini countTokens 与 Anthropic count_tokens 同样先去掉再计数，计数与实际发送的内容一致。
//
// 是否为 Omni 在这里按别名目录判定（omniVideoTarget）。别名目录没有认出时（启动期间取不到、缓存还没刷新、新加入的账户是唯一带 Omni 的账户），
// 只标记 GenerateRequest.IgnoreOmniSystemPrompt，生成服务按所选账户的目录在发送前按同一规则补做（去掉的规则与说明见 aistudio 的 omni_system.go）

// omniIgnoreSystemPrompt 返回是否对能输出视频的 Omni 模型去掉系统提示（OMNI_IGNORE_SYSTEM_PROMPT）；没有配置读取函数时按默认开启。
// 每个请求整理时读一次
func (config Config) omniIgnoreSystemPrompt() bool {
	if config.OmniIgnoreSystemPrompt == nil {
		return true
	}
	return config.OmniIgnoreSystemPrompt()
}

// ignoreOmniSystemPrompt 开启 OMNI_IGNORE_SYSTEM_PROMPT 时去掉发给能输出视频的 Omni 模型的系统提示，并在该请求的管理日志记一行（见本文件开头）。
// 开关关闭或没有系统提示时不查模型目录、不改动请求；别名目录没有认出 Omni 时标记交给生成服务在发送前再判定。
// 每个请求只调用一次，在恢复媒体链接与整理分轮之前
func (s *server) ignoreOmniSystemPrompt(ctx context.Context, request *aistudio.GenerateRequest) {
	if !s.config.omniIgnoreSystemPrompt() || !aistudio.HasClientSystemPrompt(request.System, request.Contents) {
		return
	}
	if !s.omniVideoTarget(ctx, request.Model) {
		request.IgnoreOmniSystemPrompt = true
		return
	}
	request.Notes = append(request.Notes, aistudio.OmniSystemPromptNote(aistudio.DropClientSystemPrompt(&request.System, &request.Contents)))
}
