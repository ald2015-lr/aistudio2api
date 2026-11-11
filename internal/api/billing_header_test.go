package api

import (
	"context"
	"strings"
	"testing"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// billingLine 为 Claude Code 写在系统提示开头的计费行（随客户端版本变化）
const billingLine = "x-anthropic-billing-header: cc_version=2.1.30.abc; cc_entrypoint=cli; cch=00000;"

// billingCountService 记录最后一次生成与计数请求
type billingCountService struct {
	recordingService
	counted *aistudio.TokenCountRequest
}

func (service *billingCountService) CountTokens(_ context.Context, request aistudio.TokenCountRequest) (aistudio.TokenCount, error) {
	copied := request
	service.counted = &copied
	return aistudio.TokenCount{InputTokens: 7}, nil
}

// billingNotes 返回请求说明里删除计费行的几行（InfoNotes，按 INFO 写进请求日志）
func billingNotes(notes []string) []string {
	var matched []string
	for _, note := range notes {
		if strings.HasPrefix(note, "已删除系统提示里 Claude Code 的计费行") {
			matched = append(matched, note)
		}
	}
	return matched
}

// TestDropClientBillingHeader 只删行首为 x-anthropic-billing-header: 的计费字段：这一行没有别的内容时整行删除（与按行切分后去掉这一行
// 再拼接相同），其余行逐字节不变；计费块与下一段粘成一行时（多段系统提示首尾相连）只删字段，身份句与正文保留；删空时为空串；
// 没有这一行时原样返回
func TestDropClientBillingHeader(t *testing.T) {
	for _, test := range []struct {
		system, want string
		removed      int
	}{
		{billingLine + "\nYou are Claude Code.\nBe concise.", "You are Claude Code.\nBe concise.", 1},
		{"Intro\n" + billingLine + "\r\nRest", "Intro\nRest", 1},
		{billingLine, "", 1},
		{billingLine + "\n\n  \n", "", 1},
		{"Intro\n" + billingLine, "Intro", 1},
		{"Intro\n" + billingLine + "\n" + billingLine, "Intro", 2},
		{"Intro\n\n" + billingLine, "Intro\n", 1},
		{billingLine + "  \nRest", "Rest", 1},
		{"x-anthropic-billing-header: cc_version=2.1.30.abc; cc_entrypoint=cli\nRest", "Rest", 1},
		// 粘成一行：计费块之后直接是身份句或正文
		{billingLine + "You are Claude Code, Anthropic's official CLI for Claude.\nRule 2",
			"You are Claude Code, Anthropic's official CLI for Claude.\nRule 2", 1},
		{billingLine + " You are Claude Code.", "You are Claude Code.", 1},
		{billingLine + "Rule 1; keep=this;\nRule 2", "Rule 1; keep=this;\nRule 2", 1},
		// 最后一个字段没有分号又粘着正文：只删到能确定的字段，正文不丢
		{"x-anthropic-billing-header: cc_version=1; cc_entrypoint=cliYou are Claude Code; be brief.",
			"cc_entrypoint=cliYou are Claude Code; be brief.", 1},
		{"x-anthropic-billing-header:Rest", "Rest", 1},
		{"  " + billingLine + "\nkeep", "  " + billingLine + "\nkeep", 0},
		{"Mentions x-anthropic-billing-header: inline\nok", "Mentions x-anthropic-billing-header: inline\nok", 0},
		{"plain system\n", "plain system\n", 0},
	} {
		got, removed := aistudio.DropClientBillingHeader(test.system)
		if got != test.want || removed != test.removed {
			t.Errorf("DropClientBillingHeader(%q) = %q, %d；期望 %q, %d", test.system, got, removed, test.want, test.removed)
		}
	}
}

// TestBillingHeaderDroppedAcrossEntries 各生成入口与计数接口在协议转换之后删掉计费行：Anthropic 的 system 块（计费行单独一块，
// 删后与其余块照常拼接）、OpenAI Chat 的 system 消息、Gemini systemInstruction、Responses instructions、Interactions system_instruction、
// Anthropic count_tokens 与 Gemini countTokens；生成请求在请求日志留一行说明（不含原文）；只有计费行时不发送系统提示；
// 不带计费行的请求系统提示逐字节不变、没有说明
func TestBillingHeaderDroppedAcrossEntries(t *testing.T) {
	anthropic := map[string]string{"Anthropic-Version": "2023-06-01"}
	for _, test := range []struct {
		name, path, body string
		header           map[string]string
		count            bool
		want             string
		notes            int
	}{
		{name: "Anthropic system 块", path: "/v1/messages", header: anthropic,
			body: `{"model":"gemini-2.5-flash","max_tokens":16,"system":[{"type":"text","text":"` + billingLine + `"},` +
				`{"type":"text","text":"You are Claude Code."},{"type":"text","text":"Be concise."}],"messages":[{"role":"user","content":"hi"}]}`,
			want: "You are Claude Code.\nBe concise.", notes: 1},
		{name: "Anthropic 只有计费行", path: "/v1/messages", header: anthropic,
			body: `{"model":"gemini-2.5-flash","max_tokens":16,"system":[{"type":"text","text":"` + billingLine + `"}],"messages":[{"role":"user","content":"hi"}]}`,
			want: "", notes: 1},
		{name: "Anthropic 没有计费行", path: "/v1/messages", header: anthropic,
			body: `{"model":"gemini-2.5-flash","max_tokens":16,"system":"Keep  spacing\n\nexact.","messages":[{"role":"user","content":"hi"}]}`,
			want: "Keep  spacing\n\nexact.", notes: 0},
		{name: "OpenAI Chat content 数组（转换器把各块首尾相连）", path: "/v1/chat/completions",
			body: `{"model":"gemini-2.5-flash","messages":[{"role":"system","content":[{"type":"text","text":"` + billingLine + `"},` +
				`{"type":"text","text":"You are Claude Code, Anthropic's official CLI for Claude."},{"type":"text","text":"\nYou are an interactive CLI tool.\nRule 2"}]},` +
				`{"role":"user","content":"hi"}]}`,
			want: "You are Claude Code, Anthropic's official CLI for Claude.\nYou are an interactive CLI tool.\nRule 2", notes: 1},
		{name: "OpenAI Chat 已粘成一行的字符串", path: "/v1/chat/completions",
			body: `{"model":"gemini-2.5-flash","messages":[{"role":"system","content":"` + billingLine + `You are Claude Code.\nRule 1"},{"role":"user","content":"hi"}]}`,
			want: "You are Claude Code.\nRule 1", notes: 1},
		{name: "Responses developer content 数组", path: "/v1/responses",
			body: `{"model":"gemini-2.5-flash","input":[{"role":"developer","content":[{"type":"input_text","text":"` + billingLine + `"},` +
				`{"type":"input_text","text":"You are Claude Code."},{"type":"input_text","text":"Rule 1"}]},{"role":"user","content":"hi"}]}`,
			want: "You are Claude Code.Rule 1", notes: 1},
		{name: "Gemini 多个 part", path: "/v1beta/models/gemini-2.5-flash:generateContent",
			body: `{"systemInstruction":{"parts":[{"text":"` + billingLine + `"},{"text":"You are Claude Code."},{"text":"\nRule 1"}]},` +
				`"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`,
			want: "You are Claude Code.\nRule 1", notes: 1},
		{name: "OpenAI Chat", path: "/v1/chat/completions",
			body: `{"model":"gemini-2.5-flash","messages":[{"role":"system","content":"` + billingLine + `\nSystem rules"},{"role":"user","content":"hi"}]}`,
			want: "System rules", notes: 1},
		{name: "Gemini", path: "/v1beta/models/gemini-2.5-flash:generateContent",
			body: `{"systemInstruction":{"parts":[{"text":"` + billingLine + `\nGemini rules"}]},"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`,
			want: "Gemini rules", notes: 1},
		{name: "Responses", path: "/v1/responses",
			body: `{"model":"gemini-2.5-flash","instructions":"` + billingLine + `\nCodex rules","input":"hi"}`,
			want: "Codex rules", notes: 1},
		{name: "Interactions", path: "/v1beta/interactions",
			body: `{"model":"gemini-2.5-flash","system_instruction":"` + billingLine + `\nInteraction rules","input":"hi"}`,
			want: "Interaction rules", notes: 1},
		{name: "Anthropic count_tokens", path: "/v1/messages/count_tokens", header: anthropic, count: true,
			body: `{"model":"gemini-2.5-flash","system":[{"type":"text","text":"` + billingLine + `"},{"type":"text","text":"Counted"}],"messages":[{"role":"user","content":"hi"}]}`,
			want: "Counted"},
		{name: "Gemini countTokens", path: "/v1beta/models/gemini-2.5-flash:countTokens", count: true,
			body: `{"systemInstruction":{"parts":[{"text":"` + billingLine + `\nCounted"}]},"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`,
			want: "Counted"},
	} {
		t.Run(test.name, func(t *testing.T) {
			service := &billingCountService{}
			recorder := postJSON(t, NewHandler(service, Config{APIKey: "sk-test"}), test.path, test.body, test.header)
			if recorder.Code != 200 {
				t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
			}
			if test.count {
				if service.counted == nil || service.counted.System != test.want {
					t.Fatalf("计数请求的系统提示 = %+v，期望 %q", service.counted, test.want)
				}
				return
			}
			if service.last == nil {
				t.Fatal("没有生成请求")
			}
			if service.last.System != test.want {
				t.Fatalf("系统提示 = %q，期望 %q", service.last.System, test.want)
			}
			notes := billingNotes(service.last.InfoNotes)
			if len(notes) != test.notes {
				t.Fatalf("计费行说明 = %q，期望 %d 行", notes, test.notes)
			}
			if warned := billingNotes(service.last.Notes); len(warned) != 0 {
				t.Fatalf("计费行说明是常规整理，不应按 WARN 写: %q", warned)
			}
			for _, note := range notes {
				if strings.Contains(note, "cc_version") || strings.Contains(note, "cch=") {
					t.Fatalf("说明不应包含计费行原文: %q", note)
				}
				if test.want == "" && !strings.Contains(note, "不再发送系统提示") {
					t.Fatalf("删空时说明应写明不再发送系统提示: %q", note)
				}
			}
		})
	}
}
