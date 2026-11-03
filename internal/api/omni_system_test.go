package api

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"unicode/utf8"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/mediastore"
)

// chatboxSystemPrompt 为 Chatbox 一类客户端自动加的系统提示（线上实测 Omni 带上它只回文字、说无法生成视频）
const chatboxSystemPrompt = "You are a helpful assistant."

// omniSystemHandler 返回带 Omni 目录（另加 Lyria）、记录最后一次生成请求的公开 API；enabled 为 OMNI_IGNORE_SYSTEM_PROMPT 的当前值，
// 每个请求读取一次，测试中途改它即为热更新
func omniSystemHandler(t *testing.T, enabled *atomic.Bool) (http.Handler, *recordingService) {
	t.Helper()
	models := append(omniAliasModels(), aistudio.Model{
		ID: "lyria-3-pro-preview", Name: "Lyria 3 Pro", Capabilities: map[string]bool{"chat_model": true, "music_route": true},
	})
	service := &recordingService{modelListService: modelListService{models: models}}
	return NewHandler(service, Config{APIKey: "sk-test", OmniIgnoreSystemPrompt: enabled.Load}), service
}

// describeTurns 把对话写成便于比较的文本：每轮为“角色 文本|文本”，同时检查没有留下 system 角色消息转成的轮次
func describeTurns(t *testing.T, contents []aistudio.Content) []string {
	t.Helper()
	described := make([]string, 0, len(contents))
	for _, content := range contents {
		described = append(described, string(content.Role)+" "+strings.Join(describeParts(content.Parts), "|"))
	}
	return described
}

// systemNotes 返回请求日志说明里视频模型忽略系统提示的几行
func systemNotes(notes []string) []string {
	var matched []string
	for _, note := range notes {
		if strings.HasPrefix(note, "视频模型忽略系统提示") {
			matched = append(matched, note)
		}
	}
	return matched
}

// omniSystemCase 为一个协议入口带系统提示的请求：system 为该请求会落到 GenerateRequest.System 的文本（多段时按协议的写法拼接），
// reminders 为 messages 里 system 角色消息的文本（Anthropic 转成 <system-reminder> 用户消息），turns 为去掉系统提示后应发送的对话
type omniSystemCase struct {
	name      string
	path      string
	body      string
	header    map[string]string
	system    string
	reminders []string
	turns     []string
}

// omniSystemCases 覆盖四套生成接口加 Interactions 的系统提示写法；model 为请求的模型名（Gemini 原生接口写在路径里）
func omniSystemCases(model string) []omniSystemCase {
	anthropic := map[string]string{"Anthropic-Version": "2023-06-01"}
	runtime := "Current model: " + model + "\nCurrent date: 2026-11-03"
	user := []string{"user text:随机生成一个视频"}
	return []omniSystemCase{
		{
			name: "Chat 第一条 system 消息", path: "/v1/chat/completions", system: chatboxSystemPrompt, turns: user,
			body: `{"model":"` + model + `","messages":[{"role":"system","content":"` + chatboxSystemPrompt + `"},{"role":"user","content":"随机生成一个视频"}]}`,
		},
		{
			name: "Chat system 与 developer 消息分散在对话里", path: "/v1/chat/completions", system: chatboxSystemPrompt + "\n" + runtime,
			turns: []string{"user text:你好", "assistant text:你好！", "user text:随机生成一个视频"},
			body: `{"model":"` + model + `","messages":[{"role":"system","content":[{"type":"text","text":"` + chatboxSystemPrompt + `"}]},
				{"role":"user","content":"你好"},{"role":"assistant","content":"你好！"},{"role":"developer","content":` + jsonString(runtime) + `},
				{"role":"user","content":"随机生成一个视频"}]}`,
		},
		{
			name: "Responses instructions", path: "/v1/responses", system: chatboxSystemPrompt, turns: user,
			body: `{"model":"` + model + `","instructions":"` + chatboxSystemPrompt + `","input":"随机生成一个视频"}`,
		},
		{
			name: "Responses input 里的 system 消息", path: "/v1/responses", system: chatboxSystemPrompt + "\n" + runtime, turns: user,
			body: `{"model":"` + model + `","instructions":"` + chatboxSystemPrompt + `","input":[{"role":"system","content":` + jsonString(runtime) + `},
				{"role":"user","content":[{"type":"input_text","text":"随机生成一个视频"}]}]}`,
		},
		{
			name: "Anthropic 顶层 system 文本块", path: "/v1/messages", header: anthropic, system: chatboxSystemPrompt, turns: user,
			body: `{"model":"` + model + `","max_tokens":64,"system":[{"type":"text","text":"` + chatboxSystemPrompt + `","cache_control":{"type":"ephemeral"}}],
				"messages":[{"role":"user","content":"随机生成一个视频"}]}`,
		},
		{
			name: "Anthropic messages 里的 system 角色消息", path: "/v1/messages", header: anthropic, system: chatboxSystemPrompt,
			reminders: []string{runtime, "以中文回答"}, turns: []string{"user text:你好", "assistant text:你好！", "user text:随机生成一个视频"},
			body: `{"model":"` + model + `","max_tokens":64,"system":"` + chatboxSystemPrompt + `","messages":[{"role":"system","content":` + jsonString(runtime) + `},
				{"role":"user","content":"你好"},{"role":"assistant","content":"你好！"},{"role":"system","content":[{"type":"text","text":"以中文回答"}]},
				{"role":"user","content":"随机生成一个视频"}]}`,
		},
		{
			name: "Gemini systemInstruction", path: "/v1beta/models/" + model + ":generateContent", system: chatboxSystemPrompt + runtime, turns: user,
			body: `{"systemInstruction":{"parts":[{"text":"` + chatboxSystemPrompt + `"},{"text":` + jsonString(runtime) + `}]},
				"contents":[{"role":"user","parts":[{"text":"随机生成一个视频"}]}]}`,
		},
		{
			name: "Gemini system_instruction 流式", path: "/v1beta/models/" + model + ":streamGenerateContent?alt=sse", system: chatboxSystemPrompt, turns: user,
			body: `{"system_instruction":{"parts":[{"text":"` + chatboxSystemPrompt + `"}]},"contents":[{"role":"user","parts":[{"text":"随机生成一个视频"}]}]}`,
		},
		{
			name: "Interactions system_instruction", path: "/v1beta/interactions", system: chatboxSystemPrompt, turns: user,
			body: `{"model":"` + model + `","system_instruction":"` + chatboxSystemPrompt + `","input":"随机生成一个视频"}`,
		},
	}
}

// jsonString 返回字符串的 JSON 写法
func jsonString(value string) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

// expectedSystemNote 返回 test 去掉系统提示时应写进请求日志的说明（字数为系统提示加 system 角色消息正文的字数，不含 <system-reminder> 包裹）
func expectedSystemNote(test omniSystemCase) string {
	chars := utf8.RuneCountInString(test.system)
	for _, reminder := range test.reminders {
		chars += utf8.RuneCountInString(reminder)
	}
	return aistudio.OmniSystemPromptNote(aistudio.SystemPromptDrop{Chars: chars, Turns: len(test.reminders)})
}

// TestOmniIgnoreSystemPromptProtocols 开启时（默认）四套生成接口加 Interactions 发给 Omni 的请求去掉系统提示：GenerateRequest.System 为空，
// Anthropic messages 里的 system 角色消息整轮去掉，不留空轮次，其余消息原样；请求日志记一行去掉的字数（不含提示原文）。
// 关闭时系统提示与 system 角色消息照常发送，不记说明，除系统提示外与开启时完全相同
func TestOmniIgnoreSystemPromptProtocols(t *testing.T) {
	var enabled atomic.Bool
	enabled.Store(true)
	handler, service := omniSystemHandler(t, &enabled)
	for _, test := range omniSystemCases("gemini-omni-1.1-flash") {
		t.Run(test.name, func(t *testing.T) {
			enabled.Store(true)
			service.last = nil
			recorder := postJSON(t, handler, test.path, test.body, test.header)
			if recorder.Code != http.StatusOK || service.last == nil {
				t.Fatalf("状态 %d: %s", recorder.Code, recorder.Body.String())
			}
			on := *service.last
			if on.System != "" || on.Model != "gemini-omni-1.1-flash" {
				t.Fatalf("开启时系统提示应去掉: model=%s system=%q", on.Model, on.System)
			}
			if on.IgnoreOmniSystemPrompt {
				t.Fatal("协议层已去掉，不应再交给生成服务在发送前补做")
			}
			if got := describeTurns(t, on.Contents); !slices.Equal(got, test.turns) {
				t.Fatalf("去掉系统提示后的对话 %q，期望 %q", got, test.turns)
			}
			for _, content := range on.Contents {
				if content.FromSystem || len(content.Parts) == 0 {
					t.Fatalf("不应留下 system 角色消息转成的轮次或空轮次: %+v", on.Contents)
				}
			}
			if notes := systemNotes(on.Notes); !slices.Equal(notes, []string{expectedSystemNote(test)}) {
				t.Fatalf("请求日志说明 %q，期望 %q", notes, expectedSystemNote(test))
			}
			for _, note := range on.Notes {
				if strings.Contains(note, "helpful") || strings.Contains(note, "Current date") || strings.Contains(note, "以中文回答") {
					t.Fatalf("请求日志不应写系统提示原文: %q", note)
				}
			}

			enabled.Store(false)
			service.last = nil
			recorder = postJSON(t, handler, test.path, test.body, test.header)
			if recorder.Code != http.StatusOK || service.last == nil {
				t.Fatalf("关闭时状态 %d: %s", recorder.Code, recorder.Body.String())
			}
			off := *service.last
			if off.System != test.system || len(systemNotes(off.Notes)) != 0 || off.IgnoreOmniSystemPrompt {
				t.Fatalf("关闭时系统提示应照常发送、不交给生成服务补做: system=%q notes=%q 补做=%v", off.System, off.Notes, off.IgnoreOmniSystemPrompt)
			}
			reminders := 0
			var kept []aistudio.Content
			for _, content := range off.Contents {
				if content.FromSystem {
					want := aistudio.SystemReminderOpen + test.reminders[reminders] + aistudio.SystemReminderClose
					if content.Role != aistudio.RoleUser || len(content.Parts) != 1 || content.Parts[0].Text != want {
						t.Fatalf("关闭时 system 角色消息应照原样转成用户消息: %+v", content)
					}
					reminders++
					continue
				}
				kept = append(kept, content)
			}
			if reminders != len(test.reminders) {
				t.Fatalf("关闭时 system 角色消息 %d 条，期望 %d 条", reminders, len(test.reminders))
			}
			// 除系统提示与说明外，关闭时与开启时逐字节相同
			off.System, off.Contents, off.Notes, off.ID = "", kept, nil, ""
			on.Notes, on.ID = nil, ""
			offJSON, _ := json.Marshal(off)
			onJSON, _ := json.Marshal(on)
			if string(offJSON) != string(onJSON) {
				t.Fatalf("关闭与开启除系统提示外应相同:\n关闭 %s\n开启 %s", offJSON, onJSON)
			}
		})
	}
}

// TestOmniIgnoreSystemPromptOfficialCapture 开启时带 Chatbox 系统提示的请求编码出的 CreateInteractionStream 请求体与官网不带系统提示的抓包
// 逐字节一致（系统提示没有发给上游）；关闭时与原先一样，系统提示写在 interaction field 7（下标 6），其余字节与抓包相同
func TestOmniIgnoreSystemPromptOfficialCapture(t *testing.T) {
	const capturePrefix = `[1,1,null,[null,null,null,null,null,null,`
	const captureRest = `null,null,null,null,null,null,null,null,null,null,` +
		`["models/gemini-omni-1.1-flash",[null,null,null,null,null,4,1,65536,null,null,null,null,null,null,null,null,null,null,null,null,null,null,null,null,[]]],` +
		`null,null,null,null,null,null,null,null,[[[[[[["随机\n"]]]]]]],null,null,null,null,null,null,null,null,null,null,null,null,null,null,null,null,null,` +
		`null,null,null,null,null,null,null,null,null,[[[null,null,null,[null,null,null,1,[10],4]]]]],null,1]`
	capture := capturePrefix + `null,` + captureRest
	withSystem := capturePrefix + jsonString(chatboxSystemPrompt) + `,` + captureRest
	defaults := aistudio.GenerationDefaults{
		MaxOutputTokens: 65536, Thinking: true, ThinkingLevel: true, DefaultThinkingLevel: 3,
		ThinkingLevels: []int64{1, 3}, InteractionStream: true, VideoResolution: "360p",
	}
	model := "gemini-omni-1.1-flash-high-16x9-10s-4k"
	anthropic := map[string]string{"Anthropic-Version": "2023-06-01"}
	for _, test := range []struct {
		name   string
		path   string
		body   string
		header map[string]string
	}{
		{name: "Chat", path: "/v1/chat/completions",
			body: `{"model":"` + model + `","messages":[{"role":"system","content":"` + chatboxSystemPrompt + `"},{"role":"user","content":"随机\n"}]}`},
		{name: "Responses", path: "/v1/responses",
			body: `{"model":"` + model + `","instructions":"` + chatboxSystemPrompt + `","input":"随机\n"}`},
		{name: "Anthropic", path: "/v1/messages", header: anthropic,
			body: `{"model":"` + model + `","max_tokens":65536,"system":"` + chatboxSystemPrompt + `","messages":[{"role":"user","content":"随机\n"}]}`},
		{name: "Gemini", path: "/v1beta/models/" + model + ":generateContent",
			body: `{"systemInstruction":{"parts":[{"text":"` + chatboxSystemPrompt + `"}]},"contents":[{"role":"user","parts":[{"text":"随机\n"}]}]}`},
		{name: "Interactions", path: "/v1beta/interactions",
			body: `{"model":"` + model + `","system_instruction":"` + chatboxSystemPrompt + `","input":"随机\n"}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, on := range []bool{true, false} {
				var enabled atomic.Bool
				enabled.Store(on)
				handler, service := omniSystemHandler(t, &enabled)
				recorder := postJSON(t, handler, test.path, test.body, test.header)
				if recorder.Code != http.StatusOK || service.last == nil {
					t.Fatalf("开启=%v 状态 %d: %s", on, recorder.Code, recorder.Body.String())
				}
				body, _, err := aistudio.EncodeCreateInteractionStreamRequest(*service.last, defaults)
				if err != nil {
					t.Fatal(err)
				}
				want := capture
				if !on {
					want = withSystem
				}
				if string(body) != want {
					t.Fatalf("开启=%v 请求体不一致:\n得到 %s\n期望 %s", on, body, want)
				}
			}
		})
	}
	// Anthropic messages 里的 system 角色消息同样不发给上游
	var enabled atomic.Bool
	enabled.Store(true)
	handler, service := omniSystemHandler(t, &enabled)
	recorder := postJSON(t, handler, "/v1/messages", `{"model":"`+model+`","max_tokens":65536,"messages":[{"role":"system","content":"`+
		chatboxSystemPrompt+`"},{"role":"user","content":"随机\n"}]}`, anthropic)
	if recorder.Code != http.StatusOK || service.last == nil {
		t.Fatalf("状态 %d: %s", recorder.Code, recorder.Body.String())
	}
	if body, _, err := aistudio.EncodeCreateInteractionStreamRequest(*service.last, defaults); err != nil || string(body) != capture {
		t.Fatalf("system 角色消息应整轮去掉: %v\n得到 %s\n期望 %s", err, body, capture)
	}
}

// TestOmniIgnoreSystemPromptModelNames 带视频后缀、思考后缀、models/ 前缀与目录别名的 Omni 模型名同样去掉系统提示
func TestOmniIgnoreSystemPromptModelNames(t *testing.T) {
	var enabled atomic.Bool
	enabled.Store(true)
	handler, service := omniSystemHandler(t, &enabled)
	for _, model := range []string{
		"gemini-omni-1.1-flash-9x16-720p-8s-low", "gemini-omni-1.1-flash-nothinking", "gemini-omni-1.1-flash-16x9-4K-nothinking",
		"models/gemini-omni-1.1-flash", "omni-video-latest", "omni-video-latest-9x16-8s-high",
	} {
		for _, test := range omniSystemCases(model) {
			if strings.HasPrefix(model, "models/") && strings.HasPrefix(test.path, "/v1beta/models/") {
				continue
			}
			service.last = nil
			recorder := postJSON(t, handler, test.path, test.body, test.header)
			if recorder.Code != http.StatusOK || service.last == nil {
				t.Fatalf("%s %s: 状态 %d: %s", model, test.name, recorder.Code, recorder.Body.String())
			}
			if service.last.System != "" || !slices.Equal(describeTurns(t, service.last.Contents), test.turns) ||
				!slices.Equal(systemNotes(service.last.Notes), []string{expectedSystemNote(test)}) {
				t.Fatalf("%s %s: 应去掉系统提示: system=%q turns=%q notes=%q", model, test.name, service.last.System,
					describeTurns(t, service.last.Contents), service.last.Notes)
			}
		}
	}
}

// TestOmniIgnoreSystemPromptOtherModels 开启时其他模型不受影响：文本模型（含带视频字样的名称）、图片模型、Veo、Lyria 的系统提示与
// system 角色消息照常发送，不记说明
func TestOmniIgnoreSystemPromptOtherModels(t *testing.T) {
	var enabled atomic.Bool
	enabled.Store(true)
	handler, service := omniSystemHandler(t, &enabled)
	for _, model := range []string{
		"gemini-4-argon", "gemini-4-argon-low", "gemini-4-argon-720p", "image-model", "veo-3.1-generate-preview",
		"veo-3.1-generate-preview-8s", "lyria-3-pro-preview", "unknown-model",
	} {
		for _, test := range omniSystemCases(model) {
			service.last = nil
			recorder := postJSON(t, handler, test.path, test.body, test.header)
			if recorder.Code != http.StatusOK || service.last == nil {
				t.Fatalf("%s %s: 状态 %d: %s", model, test.name, recorder.Code, recorder.Body.String())
			}
			reminders := 0
			for _, content := range service.last.Contents {
				if content.FromSystem {
					reminders++
				}
			}
			if service.last.System != test.system || reminders != len(test.reminders) || len(systemNotes(service.last.Notes)) != 0 {
				t.Fatalf("%s %s: 不应去掉系统提示: system=%q reminders=%d notes=%q", model, test.name, service.last.System, reminders, service.last.Notes)
			}
			// 协议层没有认出 Omni：只标记交给生成服务按所选账户的目录再判定（这些模型在那里同样不是 Omni，不受影响，见 aistudio 的测试）
			if !service.last.IgnoreOmniSystemPrompt {
				t.Fatalf("%s %s: 应标记交给生成服务在发送前再判定", model, test.name)
			}
		}
	}
}

// TestOmniIgnoreSystemPromptHotReload 开关每个请求读取一次：中途关闭、再打开，下一个请求立即按新值
func TestOmniIgnoreSystemPromptHotReload(t *testing.T) {
	var enabled atomic.Bool
	enabled.Store(true)
	handler, service := omniSystemHandler(t, &enabled)
	body := `{"model":"gemini-omni-1.1-flash","messages":[{"role":"system","content":"` + chatboxSystemPrompt + `"},{"role":"user","content":"随机生成一个视频"}]}`
	for index, on := range []bool{true, false, true, false} {
		enabled.Store(on)
		recorder := postJSON(t, handler, "/v1/chat/completions", body, nil)
		if recorder.Code != http.StatusOK || service.last == nil {
			t.Fatalf("第 %d 次状态 %d: %s", index+1, recorder.Code, recorder.Body.String())
		}
		if stripped := service.last.System == ""; stripped != on || (len(systemNotes(service.last.Notes)) == 1) != on || service.last.IgnoreOmniSystemPrompt {
			t.Fatalf("第 %d 次开启=%v: system=%q notes=%q 补做=%v", index+1, on, service.last.System, service.last.Notes, service.last.IgnoreOmniSystemPrompt)
		}
	}
	// 没有配置读取函数时按默认开启
	handler, service = omniHandler(t)
	if recorder := postJSON(t, handler, "/v1/chat/completions", body, nil); recorder.Code != http.StatusOK || service.last.System != "" {
		t.Fatalf("未配置时应按默认开启: %d system=%q", recorder.Code, service.last.System)
	}
}

// TestOmniIgnoreSystemPromptAliasCatalogMissing 别名目录没有 Omni（启动期间目录取不到、空快照被缓存、缓存还没刷新）时协议层认不出 Omni：
// 系统提示原样交给生成服务、不记说明，只标记 IgnoreOmniSystemPrompt，由生成服务按所选账户的目录在发送前补做（见 aistudio 的
// TestOmniSystemPromptBeforeSend）。关闭时与没有系统提示时不标记，请求与原先完全相同
func TestOmniIgnoreSystemPromptAliasCatalogMissing(t *testing.T) {
	var enabled atomic.Bool
	service := &recordingService{}
	handler := NewHandler(service, Config{APIKey: "sk-test", OmniIgnoreSystemPrompt: enabled.Load})
	for _, test := range omniSystemCases("gemini-omni-1.1-flash") {
		for _, on := range []bool{true, false} {
			enabled.Store(on)
			service.last = nil
			recorder := postJSON(t, handler, test.path, test.body, test.header)
			if recorder.Code != http.StatusOK || service.last == nil {
				t.Fatalf("%s 开启=%v: 状态 %d: %s", test.name, on, recorder.Code, recorder.Body.String())
			}
			reminders := 0
			for _, content := range service.last.Contents {
				if content.FromSystem {
					reminders++
				}
			}
			if service.last.System != test.system || reminders != len(test.reminders) || len(systemNotes(service.last.Notes)) != 0 {
				t.Fatalf("%s 开启=%v: 协议层认不出 Omni 时不应去掉: system=%q reminders=%d notes=%q", test.name, on,
					service.last.System, reminders, service.last.Notes)
			}
			if service.last.IgnoreOmniSystemPrompt != on {
				t.Fatalf("%s 开启=%v: 交给生成服务补做=%v", test.name, on, service.last.IgnoreOmniSystemPrompt)
			}
		}
	}
	enabled.Store(true)
	service.last = nil
	body := `{"model":"gemini-omni-1.1-flash","messages":[{"role":"user","content":"随机生成一个视频"}]}`
	if recorder := postJSON(t, handler, "/v1/chat/completions", body, nil); recorder.Code != http.StatusOK || service.last == nil ||
		service.last.IgnoreOmniSystemPrompt {
		t.Fatalf("没有系统提示时不应标记: %d %+v", recorder.Code, service.last)
	}
}

// TestOmniIgnoreSystemPromptOnlySystem 除系统提示外没有对话消息时：只有系统提示就保留（生成服务把它作为用户消息发送，去掉后请求就空了）；
// 还有 system 角色消息转成的轮次时只去掉系统提示，这些轮次照常按用户消息发送（系统提示不能留下来作为真正的系统指令发给 Omni）。都记一行说明
func TestOmniIgnoreSystemPromptOnlySystem(t *testing.T) {
	var enabled atomic.Bool
	enabled.Store(true)
	handler, service := omniSystemHandler(t, &enabled)
	anthropic := map[string]string{"Anthropic-Version": "2023-06-01"}
	prompt := "生成一段海浪拍岸的视频"
	note := aistudio.OmniSystemPromptNote(aistudio.SystemPromptDrop{KeptChars: utf8.RuneCountInString(prompt)})
	reminder := []string{"user text:" + aistudio.SystemReminderOpen + prompt + aistudio.SystemReminderClose}
	for _, test := range []struct {
		name   string
		path   string
		body   string
		header map[string]string
		system string
		turns  []string
		note   string
	}{
		{name: "Gemini 只有 systemInstruction", path: "/v1beta/models/gemini-omni-1.1-flash:generateContent", system: prompt, note: note,
			body: `{"systemInstruction":{"parts":[{"text":"` + prompt + `"}]}}`},
		{name: "Chat 只有 system 消息", path: "/v1/chat/completions", system: prompt, note: note,
			body: `{"model":"gemini-omni-1.1-flash","messages":[{"role":"system","content":"` + prompt + `"}]}`},
		{name: "Interactions 只有 system_instruction", path: "/v1beta/interactions", system: prompt, note: note,
			body: `{"model":"gemini-omni-1.1-flash","system_instruction":"` + prompt + `"}`},
		{name: "Anthropic 只有 system 角色消息", path: "/v1/messages", header: anthropic, turns: reminder,
			note: aistudio.OmniSystemPromptNote(aistudio.SystemPromptDrop{KeptChars: utf8.RuneCountInString(prompt), KeptTurns: 1}),
			body: `{"model":"gemini-omni-1.1-flash","max_tokens":64,"messages":[{"role":"system","content":"` + prompt + `"}]}`},
		{name: "Anthropic 顶层 system 加上只有 system 角色消息", path: "/v1/messages", header: anthropic, turns: reminder,
			note: aistudio.OmniSystemPromptNote(aistudio.SystemPromptDrop{
				Chars: utf8.RuneCountInString(chatboxSystemPrompt), KeptChars: utf8.RuneCountInString(prompt), KeptTurns: 1,
			}),
			body: `{"model":"gemini-omni-1.1-flash","max_tokens":64,"system":"` + chatboxSystemPrompt + `","messages":[{"role":"system","content":"` + prompt + `"}]}`},
	} {
		service.last = nil
		recorder := postJSON(t, handler, test.path, test.body, test.header)
		if recorder.Code != http.StatusOK || service.last == nil {
			t.Fatalf("%s: 状态 %d: %s", test.name, recorder.Code, recorder.Body.String())
		}
		if service.last.System != test.system || !slices.Equal(describeTurns(t, service.last.Contents), test.turns) ||
			!slices.Equal(systemNotes(service.last.Notes), []string{test.note}) {
			t.Fatalf("%s: system=%q contents=%q notes=%q，期望 system=%q contents=%q note=%q", test.name, service.last.System,
				describeTurns(t, service.last.Contents), service.last.Notes, test.system, test.turns, test.note)
		}
	}
	// 顶层 system 加上只有 system 角色消息：编码出的请求体里没有系统指令（interaction field 7 为 null），system 角色消息作为用户消息发送
	service.last = nil
	body := `{"model":"gemini-omni-1.1-flash","max_tokens":64,"system":"` + chatboxSystemPrompt + `","messages":[{"role":"system","content":"随机\n"}]}`
	if recorder := postJSON(t, handler, "/v1/messages", body, anthropic); recorder.Code != http.StatusOK || service.last == nil {
		t.Fatalf("状态 %d: %s", recorder.Code, recorder.Body.String())
	}
	encoded, _, err := aistudio.EncodeCreateInteractionStreamRequest(*service.last, aistudio.GenerationDefaults{
		MaxOutputTokens: 65536, Thinking: true, ThinkingLevel: true, DefaultThinkingLevel: 3,
		ThinkingLevels: []int64{1, 3}, InteractionStream: true, VideoResolution: "360p",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(encoded), `[1,1,null,[null,null,null,null,null,null,null,`) || strings.Contains(string(encoded), "helpful") ||
		!strings.Contains(string(encoded), jsonString(aistudio.SystemReminderOpen+"随机\n"+aistudio.SystemReminderClose)) {
		t.Fatalf("系统提示不应作为系统指令发送，system 角色消息按用户消息发送: %s", encoded)
	}
	if notes := service.last.Notes; len(systemNotes(notes)) != 1 || strings.Contains(strings.Join(notes, ""), "随机") {
		t.Fatalf("请求日志说明 %q", notes)
	}
}

// TestOmniIgnoreSystemPromptCountTokens Gemini countTokens 与 Anthropic count_tokens 按同一规则计数：开启时 Omni 的系统提示与
// system 角色消息不计入，关闭时与其他模型照常计入
func TestOmniIgnoreSystemPromptCountTokens(t *testing.T) {
	var enabled atomic.Bool
	service := &countingOmniService{modelListService: modelListService{models: omniAliasModels()}}
	handler := NewHandler(service, Config{APIKey: "sk-test", OmniIgnoreSystemPrompt: enabled.Load})
	anthropic := map[string]string{"Anthropic-Version": "2023-06-01"}
	gemini := `{"systemInstruction":{"parts":[{"text":"` + chatboxSystemPrompt + `"}]},"contents":[{"role":"user","parts":[{"text":"做个视频"}]}]}`
	messages := func(model string) string {
		return `{"model":"` + model + `","system":"` + chatboxSystemPrompt + `","messages":[{"role":"system","content":"以中文回答"},{"role":"user","content":"做个视频"}]}`
	}
	for _, test := range []struct {
		path     string
		body     string
		header   map[string]string
		on       bool
		stripped bool
	}{
		{path: "/v1beta/models/gemini-omni-1.1-flash-9x16:countTokens", body: gemini, on: true, stripped: true},
		{path: "/v1beta/models/omni-video-latest:countTokens", body: gemini, on: true, stripped: true},
		{path: "/v1beta/models/gemini-omni-1.1-flash:countTokens", body: gemini, on: false},
		{path: "/v1beta/models/gemini-4-argon:countTokens", body: gemini, on: true},
		{path: "/v1/messages/count_tokens", body: messages("gemini-omni-1.1-flash-low"), header: anthropic, on: true, stripped: true},
		{path: "/v1/messages/count_tokens", body: messages("gemini-omni-1.1-flash"), header: anthropic, on: false},
		{path: "/v1/messages/count_tokens", body: messages("gemini-4-argon"), header: anthropic, on: true},
	} {
		enabled.Store(test.on)
		service.last = nil
		recorder := postJSON(t, handler, test.path, test.body, test.header)
		if recorder.Code != http.StatusOK || service.last == nil {
			t.Fatalf("%s: %d %s", test.path, recorder.Code, recorder.Body.String())
		}
		reminders := 0
		for _, content := range service.last.Contents {
			if content.FromSystem {
				reminders++
			}
		}
		if (service.last.System == "") != test.stripped || test.stripped && reminders != 0 ||
			!test.stripped && strings.Contains(test.path, "count_tokens") && reminders != 1 {
			t.Fatalf("%s 开启=%v: system=%q reminders=%d，期望去掉=%v", test.path, test.on, service.last.System, reminders, test.stripped)
		}
		if len(service.last.Contents) == 0 || describeParts(service.last.Contents[len(service.last.Contents)-1].Parts)[0] != "text:做个视频" {
			t.Fatalf("%s: 对话消息应保留: %+v", test.path, service.last.Contents)
		}
	}
}

// TestDropSystemPromptKeepsOtherTurns 去掉 system 角色消息转成的轮次时不改动传入的切片，其余消息原样（包括相邻的模型消息，之后由分轮整理合并）
func TestDropSystemPromptKeepsOtherTurns(t *testing.T) {
	user := aistudio.Content{Role: aistudio.RoleUser, Parts: []aistudio.Part{{Text: "你好"}}}
	model := aistudio.Content{Role: aistudio.RoleAssistant, Parts: []aistudio.Part{{Text: "你好！", ThoughtSignature: "sig"}}}
	reminder := aistudio.Content{Role: aistudio.RoleUser, Parts: []aistudio.Part{{Text: aistudio.SystemReminderOpen + "规则" + aistudio.SystemReminderClose}}, FromSystem: true}
	original := []aistudio.Content{reminder, user, model, reminder, model}
	contents := original
	system := "  "
	if drop := aistudio.DropClientSystemPrompt(&system, &contents); drop != (aistudio.SystemPromptDrop{Chars: 2 + 2 + 2, Turns: 2}) || system != "" {
		t.Fatalf("drop=%+v system=%q", drop, system)
	}
	if len(contents) != 3 || contents[0].Parts[0].Text != "你好" || contents[1].Parts[0].ThoughtSignature != "sig" || contents[2].Role != aistudio.RoleAssistant {
		t.Fatalf("去掉后的对话: %+v", contents)
	}
	if !original[0].FromSystem || !original[3].FromSystem || len(original) != 5 {
		t.Fatalf("不应改动传入的切片: %+v", original)
	}
	merged, _ := normalizeTurns(contents)
	if len(merged) != 3 || merged[1].Role != aistudio.RoleAssistant || len(merged[1].Parts) != 2 {
		t.Fatalf("相邻的模型消息由分轮整理合并，末尾补续写提示: %+v", merged)
	}

	// 除系统提示外只有 system 角色消息：只去掉系统提示，这些轮次原样保留（按用户消息发送）
	contents = []aistudio.Content{reminder, reminder}
	system = chatboxSystemPrompt
	drop := aistudio.DropClientSystemPrompt(&system, &contents)
	if drop != (aistudio.SystemPromptDrop{Chars: utf8.RuneCountInString(chatboxSystemPrompt), KeptChars: 4, KeptTurns: 2}) || system != "" ||
		len(contents) != 2 || !slices.Equal(describeParts(contents[1].Parts), []string{"text:" + reminder.Parts[0].Text}) {
		t.Fatalf("只有 system 角色消息: drop=%+v system=%q contents=%+v", drop, system, contents)
	}
	// 只有系统提示：什么也不改
	contents = nil
	system = chatboxSystemPrompt
	if drop := aistudio.DropClientSystemPrompt(&system, &contents); drop != (aistudio.SystemPromptDrop{KeptChars: utf8.RuneCountInString(chatboxSystemPrompt)}) ||
		system != chatboxSystemPrompt || contents != nil {
		t.Fatalf("只有系统提示: drop=%+v system=%q contents=%+v", drop, system, contents)
	}
}

// TestOmniIgnoreSystemPromptBeforeMediaRestore 开启时先去掉系统提示再恢复回传的媒体链接：Anthropic messages 末尾的 system 角色消息去掉后，
// 最后一条用户消息才是这一轮，其中粘贴的本服务视频链接与客户端直接带的附件一样不带 RestoredLink（没有可用的 Drive 授权账号时按需要
// Drive 授权返回，不换回链接文本），与不带 system 角色消息的请求完全相同。关闭时与原先一样：system 角色消息是最后一条，视频带标记
func TestOmniIgnoreSystemPromptBeforeMediaRestore(t *testing.T) {
	var enabled atomic.Bool
	service := &recordingService{modelListService: modelListService{models: omniAliasModels()}}
	dir := filepath.Join(t.TempDir(), "media")
	settings := &mediaSettingsStub{enabled: true, base: "https://ai.example.com"}
	handler := NewHandler(service, Config{APIKey: "sk-test", MediaDir: dir, MediaLinks: settings.links, OmniIgnoreSystemPrompt: enabled.Load})
	name, _ := mediastore.New(dir).Save(testVideo, "video/mp4")
	link := "https://ai.example.com/media/" + name
	anthropic := map[string]string{"Anthropic-Version": "2023-06-01"}
	user := `{"role":"user","content":"把这个视频延长 5 秒 ` + link + `"}`
	system := `{"role":"system","content":"Current date: 2026-11-03"}`
	send := func(on bool, messages string) aistudio.GenerateRequest {
		t.Helper()
		enabled.Store(on)
		service.last = nil
		body := `{"model":"gemini-omni-1.1-flash","max_tokens":64,"messages":[` + messages + `]}`
		if recorder := postJSON(t, handler, "/v1/messages", body, anthropic); recorder.Code != http.StatusOK || service.last == nil {
			t.Fatalf("开启=%v 状态 %d: %s", on, recorder.Code, recorder.Body.String())
		}
		return *service.last
	}
	// restoredVideo 返回请求里恢复出来的视频是否带 RestoredLink
	restoredVideo := func(request aistudio.GenerateRequest) bool {
		t.Helper()
		for _, content := range request.Contents {
			for _, part := range content.Parts {
				if part.InlineData != nil && part.InlineData.MIME == "video/mp4" {
					return part.RestoredLink
				}
			}
		}
		t.Fatalf("视频链接应恢复为视频: %+v", request.Contents)
		return false
	}
	baseline := send(true, user)
	if restoredVideo(baseline) {
		t.Fatal("这一轮用户消息里粘贴的视频不应带 RestoredLink")
	}
	on := send(true, user+","+system)
	if restoredVideo(on) || !slices.Equal(describeTurns(t, on.Contents), describeTurns(t, baseline.Contents)) {
		t.Fatalf("去掉末尾的 system 角色消息后应与不带它的请求相同: RestoredLink=%v 对话 %q，期望 %q",
			restoredVideo(on), describeTurns(t, on.Contents), describeTurns(t, baseline.Contents))
	}
	if !slices.Equal(systemNotes(on.Notes), []string{aistudio.OmniSystemPromptNote(aistudio.SystemPromptDrop{Chars: utf8.RuneCountInString("Current date: 2026-11-03"), Turns: 1})}) {
		t.Fatalf("请求日志说明 %q", on.Notes)
	}
	if off := send(false, user+","+system); !restoredVideo(off) || len(systemNotes(off.Notes)) != 0 {
		t.Fatalf("关闭时应与原先一样（末尾的 system 角色消息算这一轮）: RestoredLink=%v notes=%q", restoredVideo(off), off.Notes)
	}
}
