package api

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// TestResponsesAgentMessage Codex 多智能体会话的 agent_message 输入项按 user 轮发送：content 里 input_text 取 text、
// encrypted_content 取原文，依次作为文本 Part；前面 reasoning 的签名不挂到后面的函数调用上；其他 content 类型返回 400
// （原先整个请求以 unsupported input item type 返回 400）
func TestResponsesAgentMessage(t *testing.T) {
	contents, instructions, err := responsesContents(json.RawMessage(`[
		{"type":"message","role":"user","content":"plan the task"},
		{"type":"reasoning","encrypted_content":"sig-1"},
		{"type":"agent_message","author":"worker-1","content":[
			{"type":"input_text","text":"worker report"},
			{"type":"encrypted_content","encrypted_content":"model wrote this"}
		]},
		{"type":"function_call","call_id":"call_1","name":"lookup","arguments":"{}"}
	]`))
	if err != nil || len(instructions) != 0 {
		t.Fatalf("contents=%+v instructions=%v err=%v", contents, instructions, err)
	}
	if len(contents) != 3 || contents[1].Role != aistudio.RoleUser {
		t.Fatalf("contents=%+v，期望第二轮为 agent_message 转成的 user 轮", contents)
	}
	if want := []aistudio.Part{{Text: "worker report"}, {Text: "model wrote this"}}; !reflect.DeepEqual(contents[1].Parts, want) {
		t.Fatalf("agent_message parts=%+v，期望 %+v", contents[1].Parts, want)
	}
	if call := contents[2].Parts[0].FunctionCall; call == nil || call.ThoughtSignature != "" {
		t.Fatalf("agent_message 之后的函数调用不应挂上前面 reasoning 的签名：%+v", contents[2].Parts[0])
	}

	// 空文本与普通 message 一样省略：原先生成空的文本 Part，编码时按“part 必须且只能设置一种内容”拒绝，整个请求以 400 结束；
	// 全部为空的 agent_message 这一轮没有 Part，发送时跳过
	contents, _, err = responsesContents(json.RawMessage(`[
		{"type":"message","role":"user","content":"plan the task"},
		{"type":"agent_message","content":[
			{"type":"input_text","text":""},
			{"type":"input_text","text":"worker report"},
			{"type":"encrypted_content","encrypted_content":""}
		]},
		{"type":"agent_message","content":[{"type":"input_text","text":""}]}
	]`))
	if err != nil || len(contents) != 3 {
		t.Fatalf("contents=%+v err=%v", contents, err)
	}
	if want := []aistudio.Part{{Text: "worker report"}}; !reflect.DeepEqual(contents[1].Parts, want) || len(contents[2].Parts) != 0 {
		t.Fatalf("agent_message parts=%+v / %+v，期望只发送非空文本 %+v", contents[1].Parts, contents[2].Parts, want)
	}
	if _, err := aistudio.EncodeGenerateContentRequest(aistudio.GenerateRequest{Model: "gemini-3.8-flash", Contents: contents},
		aistudio.GenerationDefaults{MaxOutputTokens: 1024}, aistudio.RequestContext{}); err != nil {
		t.Fatalf("带空文本的 agent_message 应能编码：%v", err)
	}

	for _, input := range []string{
		`[{"type":"agent_message","content":[{"type":"input_image","image_url":"https://example.com/a.png"}]}]`,
		`[{"type":"agent_message","content":"plain string"}]`,
	} {
		if _, _, err := responsesContents(json.RawMessage(input)); err == nil || !strings.HasPrefix(err.Error(), "agent_message: ") {
			t.Fatalf("%s：err=%v，期望 agent_message 的英文错误", input, err)
		}
	}

	// 经公开 API：agent_message 历史不再让整个请求返回 400，发往生成服务的是 user 轮
	service := &sequenceService{replies: [][]aistudio.Event{{{Kind: aistudio.EventText, Text: "ok"}, {Kind: aistudio.EventFinish, FinishReason: "stop"}}}}
	handler := NewHandler(service, Config{APIKey: "sk-test"})
	body := `{"model":"gemini-3.8-flash","input":[{"type":"message","role":"user","content":"hi"},` +
		`{"type":"agent_message","content":[{"type":"input_text","text":"from another agent"}]}]}`
	recorder := postJSON(t, handler, "/v1/responses", body, nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	request := service.request(t, 0)
	last := request.Contents[len(request.Contents)-1]
	if last.Role != aistudio.RoleUser || !strings.Contains(last.Parts[len(last.Parts)-1].Text, "from another agent") {
		t.Fatalf("发往生成服务的最后一轮=%+v，期望含 agent_message 的 user 轮", last)
	}
	recorder = postJSON(t, handler, "/v1/responses", `{"model":"gemini-3.8-flash","input":[{"type":"agent_message","content":[{"type":"refusal"}]}]}`, nil)
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), `unsupported content type \"refusal\"`) {
		t.Fatalf("status=%d body=%s，期望 400 写明不支持的 content 类型", recorder.Code, recorder.Body.String())
	}
}
