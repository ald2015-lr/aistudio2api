package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// long- 前缀与批次 A（Omni 系统提示、媒体恢复）、批次 B（原生续写名单、未知路由、Anthropic 搜索块顺序）合并后的协议层测试

// mergeTestModels 为带原生续写名单内模型的目录
func mergeTestModels() []aistudio.Model {
	return append(aliasTestModels(),
		aistudio.Model{ID: "gemini-3-flash-preview", Name: "Gemini 3 Flash", Capabilities: map[string]bool{"chat_model": true, "thinking": true, "thinking_level": true, "google_search": true}},
		aistudio.Model{ID: "gemma-3-27b-it", Name: "Gemma 3", Capabilities: map[string]bool{"chat_model": true}},
	)
}

// TestLongOutputNativePrefillAfterPrefix 原生续写名单在模型名后缀解析之后判断：带 long- 时按去掉前缀（与思考等级等后缀）后的目录模型判断，
// 名单内的模型原样以模型轮结尾交给生成服务（PrefillTail），名单外的照旧补用户消息；两种情况都带上续写标记
func TestLongOutputNativePrefillAfterPrefix(t *testing.T) {
	for _, test := range []struct {
		model  string
		id     string
		native bool
	}{
		{model: "long-gemini-3-flash-preview", id: "gemini-3-flash-preview", native: true},
		{model: "LONG-gemini-3-flash-preview-low-nothinking", id: "gemini-3-flash-preview", native: true},
		{model: "long-gemma-3-27b-it", id: "gemma-3-27b-it", native: true},
		{model: "long-gemini-2.5-pro-medium", id: "gemini-2.5-pro", native: false},
	} {
		service := &scriptedService{modelListService: modelListService{models: mergeTestModels()}, events: []aistudio.Event{
			{Kind: aistudio.EventText, Text: "world"},
			{Kind: aistudio.EventFinish, FinishReason: "stop"},
		}}
		recorder := postJSON(t, NewHandler(service, Config{APIKey: "sk-test"}), "/v1/chat/completions",
			`{"model":"`+test.model+`","messages":[{"role":"user","content":"say hello world"},{"role":"assistant","content":"hello"}]}`, nil)
		if recorder.Code != http.StatusOK || service.last == nil {
			t.Fatalf("%s：status=%d body=%s", test.model, recorder.Code, recorder.Body.String())
		}
		request := service.last
		last := request.Contents[len(request.Contents)-1]
		if request.Model != test.id || !request.LongOutput {
			t.Fatalf("%s：应去掉 long- 与后缀并带续写标记：model=%s long=%v", test.model, request.Model, request.LongOutput)
		}
		if test.native {
			if !request.PrefillTail || last.Role != aistudio.RoleAssistant {
				t.Fatalf("%s：名单内的模型应原样以模型轮结尾：PrefillTail=%v 最后一轮=%+v", test.model, request.PrefillTail, last)
			}
			continue
		}
		if request.PrefillTail || last.Role != aistudio.RoleUser || last.Parts[0].Text != prefillContinuationPrompt {
			t.Fatalf("%s：名单外的模型应补用户消息：PrefillTail=%v 最后一轮=%+v", test.model, request.PrefillTail, last)
		}
	}
}

// TestLongOutputOmniKeepsOmniRules 带 long- 的 Omni 模型名仍按 Omni 判定：OMNI_IGNORE_SYSTEM_PROMPT 照常去掉系统提示（四套协议加 Interactions），
// 续写对 Omni 按普通请求处理（不带续写标记，留一行说明）
func TestLongOutputOmniKeepsOmniRules(t *testing.T) {
	var enabled atomic.Bool
	enabled.Store(true)
	handler, service := omniSystemHandler(t, &enabled)
	for _, model := range []string{"long-gemini-omni-1.1-flash", "long-gemini-omni-1.1-flash-9x16-720p-8s-low", "Long-omni-video-latest"} {
		for _, test := range omniSystemCases(model) {
			service.last = nil
			recorder := postJSON(t, handler, test.path, test.body, test.header)
			if recorder.Code != http.StatusOK || service.last == nil {
				t.Fatalf("%s %s: 状态 %d: %s", model, test.name, recorder.Code, recorder.Body.String())
			}
			request := service.last
			if request.System != "" || !slices.Equal(describeTurns(t, request.Contents), test.turns) ||
				!slices.Equal(systemNotes(request.Notes), []string{expectedSystemNote(test)}) {
				t.Fatalf("%s %s: 应按 Omni 去掉系统提示: system=%q turns=%q notes=%q", model, test.name, request.System,
					describeTurns(t, request.Contents), request.Notes)
			}
			if request.LongOutput || strings.HasPrefix(strings.ToLower(request.Model), "long-") ||
				!slices.ContainsFunc(request.Notes, func(note string) bool { return strings.Contains(note, "不自动续写") }) {
				t.Fatalf("%s %s: Omni 按普通请求处理并留说明: model=%s long=%v notes=%q", model, test.name, request.Model, request.LongOutput, request.Notes)
			}
		}
	}
}

// TestRouteNotFoundKeepsTraceAndExistingRoutes 未知路由兜底只接住没有注册的路径：/trace/ 前缀下的已有接口（Chat、Interactions、模型列表）
// 照常处理并写排查记录，/trace/ 下的未知路径按所属协议返回 404 错误体
func TestRouteNotFoundKeepsTraceAndExistingRoutes(t *testing.T) {
	service := &scriptedService{modelListService: modelListService{models: mergeTestModels()}, events: []aistudio.Event{
		{Kind: aistudio.EventText, Text: "ok"},
		{Kind: aistudio.EventFinish, FinishReason: "stop"},
	}}
	handler := NewHandler(service, Config{APIKey: "sk-test", TraceDir: t.TempDir()})
	for _, path := range []string{"/trace/v1/chat/completions", "/trace/v1beta/interactions", "/trace/ultra/v1/chat/completions"} {
		body := `{"model":"gemini-2.5-pro","messages":[{"role":"user","content":"hi"}]}`
		if strings.Contains(path, "interactions") {
			body = `{"model":"gemini-2.5-pro","input":"hi","store":false}`
		}
		if recorder := postJSON(t, handler, path, body, nil); recorder.Code != http.StatusOK {
			t.Fatalf("%s：已有接口不应被兜底接住：status=%d body=%s", path, recorder.Code, recorder.Body.String())
		}
	}
	for _, path := range []string{"/trace/v1/models", "/v1/models"} {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		request.Header.Set("Authorization", "Bearer sk-test")
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusOK {
			t.Fatalf("%s：status=%d body=%s", path, recorder.Code, recorder.Body.String())
		}
	}
	request := httptest.NewRequest(http.MethodGet, "/trace/v1/unknown", nil)
	request.Header.Set("Authorization", "Bearer sk-test")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	var body map[string]any
	_ = json.Unmarshal(recorder.Body.Bytes(), &body)
	failure, _ := body["error"].(map[string]any)
	if recorder.Code != http.StatusNotFound || failure["message"] != "Invalid URL (GET /v1/unknown)" {
		t.Fatalf("/trace/ 下的未知路径：status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

// TestAnthropicSearchBlocksWithContinuation long- 请求续写合并后的 Anthropic 非流式响应：各轮正文接成一个正文块，搜索块（续写在出现
// 搜索来源的一轮之后不再续写，合并后的回复里只有一轮的来源）照样放在开头的思考之后、正文之前；续写不再继续时按 max_tokens 结束，
// 模型名报告去掉 long- 的目录模型
func TestAnthropicSearchBlocksWithContinuation(t *testing.T) {
	service := &scriptedService{modelListService: modelListService{models: mergeTestModels()}, events: []aistudio.Event{
		{Kind: aistudio.EventReasoning, Text: "先查一下", ThoughtSignature: "sig"},
		{Kind: aistudio.EventText, Text: "第一轮。"},
		{Kind: aistudio.EventText, Text: "第二轮。"},
		{Kind: aistudio.EventGrounding, Grounding: &aistudio.GroundingMetadata{
			WebSearchQueries: []string{"go release"},
			Chunks:           []aistudio.GroundingChunk{{URI: "https://go.dev/doc", Title: "Go Docs"}},
		}},
		{Kind: aistudio.EventUsage, Usage: &aistudio.Usage{InputTokens: 10, OutputTokens: 130, TotalTokens: 140}},
		{Kind: aistudio.EventFinish, FinishReason: "max_tokens"},
	}}
	recorder := postJSON(t, NewHandler(service, Config{APIKey: "sk-test"}), "/v1/messages",
		`{"model":"long-gemini-3-flash-preview","max_tokens":200000,"messages":[{"role":"user","content":"写长文"}]}`,
		map[string]string{"Anthropic-Version": "2023-06-01"})
	if recorder.Code != http.StatusOK || service.last == nil || !service.last.LongOutput {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		Model      string `json:"model"`
		StopReason string `json:"stop_reason"`
		Content    []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	var types []string
	text := ""
	for _, block := range response.Content {
		types = append(types, block.Type)
		if block.Type == "text" {
			text += block.Text
		}
	}
	if strings.Join(types, ",") != "thinking,server_tool_use,web_search_tool_result,text" || text != "第一轮。第二轮。" ||
		response.StopReason != "max_tokens" || response.Model != "gemini-3-flash-preview" {
		t.Fatalf("内容块=%v 正文=%q stop_reason=%s model=%s", types, text, response.StopReason, response.Model)
	}
}
