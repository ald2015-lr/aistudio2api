package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// TestGeminiSystemRoleContents Gemini 原生 contents 里 role 为 system、developer 的消息：对话消息之前的并入系统指令（另起一行接在
// systemInstruction 之后），对话中途的留在原位，以 <system-reminder> 包成单独一轮用户消息并标记 FromSystem（与 Anthropic 的 system 角色
// 消息相同）；空的忽略，带媒体或函数的返回参数错误
func TestGeminiSystemRoleContents(t *testing.T) {
	var request geminiRequest
	decodeInto(t, `{"systemInstruction":{"parts":[{"text":"base rules"}]},"contents":[
		{"role":"system","parts":[{"text":"be brief"}]},
		{"role":"developer","parts":[{"text":"use "},{"text":"English"}]},
		{"role":"system","parts":[{"text":"  "}]},
		{"role":"user","parts":[{"text":"hi"}]},
		{"role":"model","parts":[{"text":"hello"}]},
		{"role":"system","parts":[{"text":"now answer in one word"}]},
		{"role":"user","parts":[{"text":"why"}]}]}`, &request)
	generate, err := request.toGenerateRequest("resp_test", "gemini-2.5-flash")
	if err != nil {
		t.Fatal(err)
	}
	if generate.System != "base rules\nbe brief\nuse English" {
		t.Fatalf("系统指令 = %q", generate.System)
	}
	var shape []string
	for _, content := range generate.Contents {
		texts := make([]string, 0, len(content.Parts))
		for _, part := range content.Parts {
			texts = append(texts, part.Text)
		}
		marker := ""
		if content.FromSystem {
			marker = "*"
		}
		shape = append(shape, string(content.Role)+marker+":"+strings.Join(texts, "|"))
	}
	want := []string{"user:hi", "assistant:hello", "user*:" + aistudio.SystemReminderOpen + "now answer in one word" + aistudio.SystemReminderClose, "user:why"}
	if strings.Join(shape, "\n") != strings.Join(want, "\n") {
		t.Fatalf("对话 = %q，期望 %q", shape, want)
	}

	// 没有 systemInstruction 时开头的 system 消息就是系统指令
	request = geminiRequest{}
	decodeInto(t, `{"contents":[{"role":"developer","parts":[{"text":"rules"}]},{"role":"user","parts":[{"text":"hi"}]}]}`, &request)
	if generate, err = request.toGenerateRequest("resp_test", "gemini-2.5-flash"); err != nil || generate.System != "rules" || len(generate.Contents) != 1 {
		t.Fatalf("system=%q contents=%+v err=%v", generate.System, generate.Contents, err)
	}

	for _, body := range []string{
		`{"contents":[{"role":"system","parts":[{"inlineData":{"mimeType":"image/png","data":"AAAA"}}]},{"role":"user","parts":[{"text":"hi"}]}]}`,
		`{"contents":[{"role":"user","parts":[{"text":"hi"}]},{"role":"developer","parts":[{"functionCall":{"name":"f","args":{}}}]}]}`,
	} {
		request = geminiRequest{}
		decodeInto(t, body, &request)
		if _, err := request.toGenerateRequest("resp_test", "gemini-2.5-flash"); err == nil || !strings.Contains(err.Error(), "must contain text") {
			t.Fatalf("%s 应返回参数错误，得到 %v", body, err)
		}
	}
}

// TestGeminiSystemRoleAccepted 带 role:system 的 Gemini 原生请求不再在选号前返回 400，系统指令与对话中途的系统消息交给生成服务
func TestGeminiSystemRoleAccepted(t *testing.T) {
	service := &recordingService{}
	recorder := postJSON(t, NewHandler(service, Config{APIKey: "sk-test"}), "/v1beta/models/gemini-2.5-flash:generateContent",
		`{"contents":[{"role":"system","parts":[{"text":"be brief"}]},{"role":"user","parts":[{"text":"hi"}]},{"role":"model","parts":[{"text":"hello"}]},{"role":"system","parts":[{"text":"one word"}]},{"role":"user","parts":[{"text":"why"}]}]}`, nil)
	if recorder.Code != http.StatusOK || service.last == nil {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if service.last.System != "be brief" || len(service.last.Contents) != 4 || !service.last.Contents[2].FromSystem {
		t.Fatalf("生成请求 system=%q contents=%+v", service.last.System, service.last.Contents)
	}
}
