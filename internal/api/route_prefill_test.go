package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// TestRouteNotFoundByProtocol 未知路由（含方法不对）按路径所属协议返回 404 错误体，消息为 "Invalid URL (方法 路径)"；
// /ultra 前缀共用同一条处理链。原先是 Go 默认的纯文本 "404 page not found"
func TestRouteNotFoundByProtocol(t *testing.T) {
	handler := NewHandler(&scriptedService{}, Config{APIKey: "sk-test"})
	for _, test := range []struct {
		method  string
		path    string
		header  map[string]string
		message string
		check   func(map[string]any) bool
	}{
		{method: http.MethodGet, path: "/v1/unknown", message: "Invalid URL (GET /v1/unknown)", check: func(body map[string]any) bool {
			// OpenAI 官方写法：type 为 invalid_request_error，param 与 code 为 null
			failure, _ := body["error"].(map[string]any)
			_, hasParam := failure["param"]
			return failure["type"] == "invalid_request_error" && hasParam && failure["param"] == nil && failure["code"] == nil
		}},
		{method: http.MethodGet, path: "/v1/chat/completions", message: "Invalid URL (GET /v1/chat/completions)", check: func(body map[string]any) bool {
			failure, _ := body["error"].(map[string]any)
			return failure["type"] == "invalid_request_error"
		}},
		{method: http.MethodPost, path: "/v1beta/unknown/route", message: "Invalid URL (POST /v1beta/unknown/route)", check: func(body map[string]any) bool {
			failure, _ := body["error"].(map[string]any)
			return failure["status"] == "NOT_FOUND" && failure["code"] == float64(http.StatusNotFound)
		}},
		{
			method: http.MethodGet, path: "/v1/messages/unknown", header: map[string]string{"Anthropic-Version": "2023-06-01"},
			message: "Invalid URL (GET /v1/messages/unknown)", check: func(body map[string]any) bool {
				failure, _ := body["error"].(map[string]any)
				return body["type"] == "error" && failure["type"] == "not_found_error"
			},
		},
		{method: http.MethodGet, path: "/ultra/v1/unknown", message: "Invalid URL (GET /v1/unknown)", check: func(body map[string]any) bool {
			failure, _ := body["error"].(map[string]any)
			return failure["type"] == "invalid_request_error"
		}},
	} {
		request := httptest.NewRequest(test.method, test.path, strings.NewReader(`{}`))
		request.Header.Set("Authorization", "Bearer sk-test")
		for name, value := range test.header {
			request.Header.Set(name, value)
		}
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		var body map[string]any
		if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
			t.Fatalf("%s %s：应返回 JSON 错误体，得到 %q", test.method, test.path, recorder.Body.String())
		}
		failure, _ := body["error"].(map[string]any)
		if recorder.Code != http.StatusNotFound || failure["message"] != test.message || !test.check(body) {
			t.Fatalf("%s %s：status=%d body=%s", test.method, test.path, recorder.Code, recorder.Body.String())
		}
	}
	// 已有路由照常处理
	recorder := postJSON(t, handler, "/v1/chat/completions", `{"model":"m","messages":[]}`, nil)
	if recorder.Code == http.StatusNotFound {
		t.Fatalf("已有路由不应返回 404：%s", recorder.Body.String())
	}
}

// TestNativePrefillTail 名单内的模型（3.5 以前与 gemma-）以模型轮结尾时原样交给生成服务（PrefillTail），不补用户消息；
// 名单外的模型照旧在末尾补一条请模型接着写的用户消息
func TestNativePrefillTail(t *testing.T) {
	for _, test := range []struct {
		model  string
		native bool
	}{
		{model: "gemini-3-flash-preview", native: true},
		{model: "gemma-3-27b-it", native: true},
		{model: "gemini-4-argon", native: false},
	} {
		service := &scriptedService{events: []aistudio.Event{
			{Kind: aistudio.EventText, Text: "world"},
			{Kind: aistudio.EventFinish, FinishReason: "stop"},
		}}
		handler := NewHandler(service, Config{APIKey: "sk-test"})
		recorder := postJSON(t, handler, "/v1/chat/completions",
			`{"model":"`+test.model+`","messages":[{"role":"user","content":"say hello world"},{"role":"assistant","content":"hello"}]}`, nil)
		if recorder.Code != http.StatusOK || service.last == nil {
			t.Fatalf("%s：status=%d body=%s", test.model, recorder.Code, recorder.Body.String())
		}
		contents := service.last.Contents
		last := contents[len(contents)-1]
		if test.native {
			if !service.last.PrefillTail || last.Role != aistudio.RoleAssistant || last.Parts[0].Text != "hello" {
				t.Fatalf("%s：应原样以模型轮结尾交给生成服务：PrefillTail=%t 最后一轮=%+v", test.model, service.last.PrefillTail, last)
			}
			continue
		}
		if service.last.PrefillTail || last.Role != aistudio.RoleUser || last.Parts[0].Text != prefillContinuationPrompt {
			t.Fatalf("%s：应补一条请模型接着写的用户消息：PrefillTail=%t 最后一轮=%+v", test.model, service.last.PrefillTail, last)
		}
	}
}

// TestAnthropicSearchBlocksBeforeText 非流式 Anthropic 响应：搜索块（server_tool_use、web_search_tool_result）放在开头的思考之后、
// 正文之前，与官方顺序一致（原先拼在全部内容之后）
func TestAnthropicSearchBlocksBeforeText(t *testing.T) {
	result := generationResult{events: []aistudio.Event{
		{Kind: aistudio.EventReasoning, Text: "先查一下", ThoughtSignature: "sig"},
		{Kind: aistudio.EventText, Text: "Go 1.25 已发布。"},
		{Grounding: &aistudio.GroundingMetadata{
			WebSearchQueries: []string{"go release"},
			Chunks:           []aistudio.GroundingChunk{{URI: "https://go.dev/doc", Title: "Go Docs"}},
		}},
	}, finishReason: "stop"}
	response := buildAnthropicResponse("msg_1", "gemini-3-flash-preview", result, "sk-test", mediaOutput{})
	blocks, _ := response["content"].([]anthropicContentBlock)
	var types []string
	for _, block := range blocks {
		types = append(types, block.Type)
	}
	if strings.Join(types, ",") != "thinking,server_tool_use,web_search_tool_result,text" {
		t.Fatalf("内容块顺序 = %v，期望 思考、搜索、正文", types)
	}
	// 没有思考时搜索块在最前
	result = generationResult{events: []aistudio.Event{
		{Kind: aistudio.EventText, Text: "answer"},
		{Grounding: &aistudio.GroundingMetadata{WebSearchQueries: []string{"q"}, Chunks: []aistudio.GroundingChunk{{URI: "https://example.com", Title: "E"}}}},
	}, finishReason: "stop"}
	blocks, _ = buildAnthropicResponse("msg_2", "m", result, "sk-test", mediaOutput{})["content"].([]anthropicContentBlock)
	if len(blocks) != 3 || blocks[0].Type != "server_tool_use" || blocks[2].Type != "text" {
		t.Fatalf("没有思考时内容块 = %+v", blocks)
	}
}
