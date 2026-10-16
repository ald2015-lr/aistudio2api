package api

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// responseModelService 返回带上游实际服务模型的事件，用于确认响应里的模型名不被它替换
func responseModelService() *scriptedService {
	return &scriptedService{
		modelListService: modelListService{models: []aistudio.Model{{
			ID: "gemini-3.1-pro-preview", Methods: []string{"generateContent"},
			Capabilities: map[string]bool{"chat_model": true, "thinking_level": true},
		}}},
		events: []aistudio.Event{
			{Kind: aistudio.EventText, Text: "ok", ProviderModel: "gemini-3.1-pro-preview-upstream-0612"},
			{Kind: aistudio.EventFinish, FinishReason: "stop", ProviderModel: "gemini-3.1-pro-preview-upstream-0612"},
		},
	}
}

// sseModels 返回 SSE 正文里所有 data 帧中指定字段的值（嵌套字段用 . 分隔）
func sseModels(t *testing.T, body string, path ...string) []string {
	t.Helper()
	var models []string
	for _, line := range strings.Split(body, "\n") {
		data, ok := strings.CutPrefix(line, "data: ")
		if !ok || data == "[DONE]" {
			continue
		}
		var value any
		if err := json.Unmarshal([]byte(data), &value); err != nil {
			continue
		}
		for _, key := range path {
			object, isObject := value.(map[string]any)
			if !isObject {
				value = nil
				break
			}
			value = object[key]
		}
		if model, isString := value.(string); isString {
			models = append(models, model)
		}
	}
	return models
}

// TestResponseModelReportsRequestedBaseName 各协议响应里的模型名为请求的模型名去掉后缀：
// gemini-3.1-pro-preview、gemini-3.1-pro-preview-128、gemini-3.1-pro-preview-low 都报告为 gemini-3.1-pro-preview，
// 不换成上游报告的实际服务模型版本；目录里没有的名称原样报告
func TestResponseModelReportsRequestedBaseName(t *testing.T) {
	const base = "gemini-3.1-pro-preview"
	for _, test := range []struct {
		requested string
		want      string
	}{
		{requested: base, want: base},
		{requested: base + "-128", want: base},
		{requested: base + "-low", want: base},
		{requested: base + "-medium-nothinking-online", want: base},
		{requested: " " + base + "-high ", want: base},
		{requested: "unknown-model-low", want: "unknown-model-low"},
	} {
		name, err := json.Marshal(test.requested)
		if err != nil {
			t.Fatal(err)
		}
		model := string(name)
		check := func(protocol string, got []string) {
			t.Helper()
			if len(got) == 0 {
				t.Fatalf("%s %q: 响应里没有模型名", protocol, test.requested)
			}
			for _, value := range got {
				if value != test.want {
					t.Fatalf("%s %q: 响应模型名 %q，期望 %q（全部：%v）", protocol, test.requested, value, test.want, got)
				}
			}
		}
		decode := func(protocol string, body string) map[string]any {
			t.Helper()
			var object map[string]any
			if err := json.Unmarshal([]byte(body), &object); err != nil {
				t.Fatalf("%s %q: 响应不是 JSON: %s", protocol, test.requested, body)
			}
			return object
		}

		handler := NewHandler(responseModelService(), Config{APIKey: "sk-test"})
		chat := `{"model":` + model + `,"messages":[{"role":"user","content":"hi"}]`
		plain := postJSON(t, handler, "/v1/chat/completions", chat+`}`, nil)
		check("OpenAI", []string{decode("OpenAI", plain.Body.String())["model"].(string)})
		stream := postJSON(t, handler, "/v1/chat/completions", chat+`,"stream":true}`, nil)
		check("OpenAI 流式", sseModels(t, stream.Body.String(), "model"))

		messages := `{"model":` + model + `,"max_tokens":16,"messages":[{"role":"user","content":"hi"}]`
		anthropic := postJSON(t, handler, "/v1/messages", messages+`}`, nil)
		check("Anthropic", []string{decode("Anthropic", anthropic.Body.String())["model"].(string)})
		anthropicStream := postJSON(t, handler, "/v1/messages", messages+`,"stream":true}`, nil)
		check("Anthropic 流式", sseModels(t, anthropicStream.Body.String(), "message", "model"))

		responses := `{"model":` + model + `,"input":"hi"`
		plainResponses := postJSON(t, handler, "/v1/responses", responses+`}`, nil)
		check("Responses", []string{decode("Responses", plainResponses.Body.String())["model"].(string)})
		streamResponses := postJSON(t, handler, "/v1/responses", responses+`,"stream":true}`, nil)
		check("Responses 流式", sseModels(t, streamResponses.Body.String(), "response", "model"))

		interaction := postJSON(t, handler, "/v1beta/interactions", `{"model":`+model+`,"input":"hi","store":false}`, nil)
		check("Interactions", []string{decode("Interactions", interaction.Body.String())["model"].(string)})

		if strings.TrimSpace(test.requested) != test.requested {
			continue // Gemini 的模型名在路径里，不带空白
		}
		gemini := postJSON(t, handler, "/v1beta/models/"+test.requested+":generateContent", `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`, nil)
		check("Gemini", []string{decode("Gemini", gemini.Body.String())["modelVersion"].(string)})
		geminiStream := postJSON(t, handler, "/v1beta/models/"+test.requested+":streamGenerateContent?alt=sse", `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`, nil)
		check("Gemini 流式", sseModels(t, geminiStream.Body.String(), "modelVersion"))
	}
}
