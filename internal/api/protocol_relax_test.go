package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// TestOpenAIIgnoresPenaltyAndLogprobs Chat 的 penalty、logprobs、top_logprobs、logit_bias 被忽略，n 仍只接受 1
func TestOpenAIIgnoresPenaltyAndLogprobs(t *testing.T) {
	var request chatRequest
	decodeInto(t, `{"model":"m","messages":[{"role":"user","content":"hi"}],"frequency_penalty":0.7,"presence_penalty":-1.5,
		"logprobs":true,"top_logprobs":5,"logit_bias":{"50256":-100},"n":1}`, &request)
	if _, err := request.generationConfig(); err != nil {
		t.Fatalf("penalty、logprobs 与 logit_bias 应被忽略: %v", err)
	}
	var malformed chatRequest
	decodeInto(t, `{"model":"m","messages":[{"role":"user","content":"hi"}],"logit_bias":[1,2]}`, &malformed)
	if _, err := malformed.generationConfig(); err != nil {
		t.Fatalf("logit_bias 的写法不影响请求: %v", err)
	}
	var multiple chatRequest
	decodeInto(t, `{"model":"m","messages":[{"role":"user","content":"hi"}],"n":2}`, &multiple)
	if _, err := multiple.generationConfig(); err == nil || !strings.Contains(err.Error(), "n must be 1") {
		t.Fatalf("n=2 仍应返回错误，得到 %v", err)
	}

	// 经完整路由：带非 0 penalty 的请求返回 200，非流式 choice 与官方一致带 logprobs:null
	service := &scriptedService{events: []aistudio.Event{{Kind: aistudio.EventText, Text: "ok"}, {Kind: aistudio.EventFinish, FinishReason: "STOP"}}}
	recorder := postJSON(t, NewHandler(service, Config{APIKey: "sk-test"}), "/v1/chat/completions",
		`{"model":"gemini-2.5-flash","messages":[{"role":"user","content":"hi"}],"frequency_penalty":1,"presence_penalty":1,"logprobs":true}`, nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var body struct {
		Choices []map[string]json.RawMessage `json:"choices"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil || len(body.Choices) != 1 {
		t.Fatalf("body=%s err=%v", recorder.Body.String(), err)
	}
	if logprobs, ok := body.Choices[0]["logprobs"]; !ok || string(logprobs) != "null" {
		t.Fatalf("choice 应带 logprobs:null: %s", recorder.Body.String())
	}
}

// TestGeminiIgnoresPenaltyAndLogprobs Gemini 的 frequencyPenalty、presencePenalty、responseLogprobs、logprobs 被忽略，candidateCount 仍只接受 1
func TestGeminiIgnoresPenaltyAndLogprobs(t *testing.T) {
	var request geminiRequest
	decodeInto(t, `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"frequencyPenalty":0.5,
		"presencePenalty":-0.5,"responseLogprobs":true,"logprobs":5,"candidateCount":1}}`, &request)
	if _, err := request.toGenerateRequest("id", "gemini-x"); err != nil {
		t.Fatalf("penalty 与 logprobs 应被忽略: %v", err)
	}
	var multiple geminiRequest
	decodeInto(t, `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"candidateCount":2}}`, &multiple)
	if _, err := multiple.toGenerateRequest("id", "gemini-x"); err == nil || !strings.Contains(err.Error(), "candidateCount") {
		t.Fatalf("candidateCount=2 仍应返回错误，得到 %v", err)
	}
}

// TestHistoryFunctionArgumentsWrapRaw 历史工具调用的参数不是 JSON object 时包成 _raw，合法 object 原样保留
func TestHistoryFunctionArgumentsWrapRaw(t *testing.T) {
	for input, want := range map[string]string{
		"":                  `{}`,
		"  ":                `{}`,
		`{"path":"a.go"}`:   `{"path":"a.go"}`,
		`{"path":"a.go","n`: `{"_raw":"{\"path\":\"a.go\",\"n"}`,
		`"just text"`:       `{"_raw":"\"just text\""}`,
		`[1,2]`:             `{"_raw":"[1,2]"}`,
		`null`:              `{"_raw":"null"}`,
		`not json`:          `{"_raw":"not json"}`,
	} {
		if got := string(historyFunctionArguments(input)); got != want {
			t.Fatalf("参数 %q 转换为 %s，期望 %s", input, got, want)
		}
	}
}

// TestHistoryToolCallTruncatedArguments Chat tool_calls 与 Responses function_call 回放被截断的参数时不再返回 400，并能编码发送
func TestHistoryToolCallTruncatedArguments(t *testing.T) {
	var chat chatRequest
	decodeInto(t, `{"model":"m","messages":[{"role":"user","content":"edit"},
		{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"write_file","arguments":"{\"path\":\"a.go\",\"content\":\"pack"}}]},
		{"role":"tool","tool_call_id":"call_1","content":"error: invalid arguments"}],
		"tools":[{"type":"function","function":{"name":"write_file","parameters":{"type":"object","properties":{"path":{"type":"string"}}}}}]}`, &chat)
	generate, err := chat.toGenerateRequest("id")
	if err != nil {
		t.Fatalf("Chat 被截断的历史参数应被接受: %v", err)
	}
	call := generate.Contents[1].Parts[0].FunctionCall
	if call == nil || string(call.Arguments) != `{"_raw":"{\"path\":\"a.go\",\"content\":\"pack"}` {
		t.Fatalf("Chat 历史调用=%+v", generate.Contents[1].Parts)
	}
	if _, err := aistudio.EncodeGenerateContentRequest(generate, aistudio.GenerationDefaults{MaxOutputTokens: 1024}, aistudio.RequestContext{}); err != nil {
		t.Fatalf("包装后的参数应能编码: %v", err)
	}

	var responses responsesRequest
	decodeInto(t, `{"model":"m","input":[{"type":"message","role":"user","content":"edit"},
		{"type":"function_call","call_id":"c1","name":"write_file","arguments":"[\"a.go\"]"},
		{"type":"function_call_output","call_id":"c1","output":"ok"}]}`, &responses)
	generate, _, err = responses.toGenerateRequest("id")
	if err != nil {
		t.Fatalf("Responses 不是 object 的历史参数应被接受: %v", err)
	}
	call = generate.Contents[1].Parts[0].FunctionCall
	if call == nil || string(call.Arguments) != `{"_raw":"[\"a.go\"]"}` {
		t.Fatalf("Responses 历史调用=%+v", generate.Contents[1].Parts)
	}
}

// TestAnthropicServerToolVersionsByPrefix 服务端工具的任何日期版本都按前缀识别；tool_search 与 MCP toolset 忽略；web_fetch 的域名限制仍返回 400
func TestAnthropicServerToolVersionsByPrefix(t *testing.T) {
	var request anthropicRequest
	decodeInto(t, `{"model":"m","max_tokens":16,"messages":[{"role":"user","content":"hi"}],"tools":[
		{"type":"web_search_20260209","name":"web_search","max_uses":2},
		{"type":"web_fetch_20260209","name":"web_fetch","max_uses":2},
		{"type":"code_execution_20260120","name":"code_execution"},
		{"type":"tool_search_tool_regex_20251119","name":"tool_search_tool_regex"},
		{"type":"tool_search_tool_bm25_20251119","name":"tool_search_tool_bm25"},
		{"type":"mcp_toolset","mcp_server_name":"docs","default_config":{"enabled":true}},
		{"name":"lookup","input_schema":{"type":"object","properties":{}},"defer_loading":true}]}`, &request)
	generate, err := request.toGenerateRequest("id")
	if err != nil {
		t.Fatalf("新日期版本的服务端工具应被识别: %v", err)
	}
	for _, tool := range []string{"google_search", "url_context", "code_execution"} {
		if !slices.Contains(generate.Tools.Google, tool) {
			t.Fatalf("应启用 %s: %v", tool, generate.Tools.Google)
		}
	}
	if len(generate.Tools.Google) != 3 || len(generate.Tools.Functions) != 1 || generate.Tools.Functions[0].Name != "lookup" {
		t.Fatalf("tool_search 与 toolset 应被忽略: google=%v functions=%+v", generate.Tools.Google, generate.Tools.Functions)
	}
	_, err = anthropicToolsRequest(t, `{"type":"web_fetch_20260209","name":"web_fetch","allowed_domains":["go.dev"]}`)
	if err == nil || !strings.Contains(err.Error(), "allowed_domains") {
		t.Fatalf("新版本 web_fetch 的 allowed_domains 仍应返回 400，得到 %v", err)
	}
	if _, err := anthropicToolsRequest(t, `{"type":"web_search_20260209","name":"search"}`); err == nil {
		t.Fatal("新版本 web_search 的 name 不对仍应返回错误")
	}
}

// anthropicSearchHistoryMessages 构造带 web_search 历史的会话：搜索结果由 issuerKey 签发
func anthropicSearchHistoryMessages(t *testing.T, issuerKey string) []anthropicMessage {
	t.Helper()
	events := []aistudio.Event{{Grounding: &aistudio.GroundingMetadata{
		WebSearchQueries: []string{"go release"},
		Chunks:           []aistudio.GroundingChunk{{URI: "https://go.dev/doc", Title: "Go Docs", Text: "secret snippet"}},
	}}}
	blocks, count := anthropicSearchBlocks(events, issuerKey)
	if count != 1 || len(blocks) != 2 {
		t.Fatalf("搜索块=%+v", blocks)
	}
	content, _ := json.Marshal(append(blocks, anthropicContentBlock{Type: "text", Text: "answer"}))
	return []anthropicMessage{
		{Role: "user", Content: json.RawMessage(`"search"`)},
		{Role: "assistant", Content: content},
		{Role: "user", Content: json.RawMessage(`"thanks"`)},
	}
}

// TestAnthropicSearchHistoryTolerance 搜索历史解不开、配对缺失、结果为 error_code 时转为文本，不再返回 400；只有本服务签发的来源恢复摘要
func TestAnthropicSearchHistoryTolerance(t *testing.T) {
	decode := func(t *testing.T, key string, messages []anthropicMessage) string {
		t.Helper()
		s := &server{config: Config{APIKey: key}}
		request := anthropicRequest{Model: "m", Messages: messages}
		if err := s.decodeAnthropicSearchHistory(&request); err != nil {
			t.Fatalf("搜索历史不应返回错误: %v", err)
		}
		generate, err := request.toGenerateRequest("id")
		if err != nil {
			t.Fatalf("转换失败: %v", err)
		}
		var text strings.Builder
		for _, content := range generate.Contents {
			for _, part := range content.Parts {
				text.WriteString(part.Text + "\n")
			}
		}
		return text.String()
	}
	same := decode(t, "k1", anthropicSearchHistoryMessages(t, "k1"))
	if !strings.Contains(same, "Web search: go release\nGo Docs\nhttps://go.dev/doc\nsecret snippet") {
		t.Fatalf("同一凭证应恢复摘要: %q", same)
	}
	// 换了 API key（或来自其他 Claude 渠道）：保留标题与 URL，摘要不恢复
	other := decode(t, "k2", anthropicSearchHistoryMessages(t, "k1"))
	if !strings.Contains(other, "Web search: go release\nGo Docs\nhttps://go.dev/doc") || strings.Contains(other, "secret snippet") {
		t.Fatalf("其他凭证签发的来源只保留标题与 URL: %q", other)
	}

	unpaired := []anthropicMessage{
		{Role: "user", Content: json.RawMessage(`"search"`)},
		{Role: "assistant", Content: json.RawMessage(`[
			{"type":"server_tool_use","id":"srvtoolu_a","name":"web_search","input":{"query":"only call"}},
			{"type":"web_search_tool_result","tool_use_id":"srvtoolu_missing","content":[{"type":"web_search_result","url":"https://example.com","title":"Example","encrypted_content":"not-base64!"}]},
			{"type":"server_tool_use","id":"srvtoolu_b","name":"web_search","input":{"query":"limited"}},
			{"type":"web_search_tool_result","tool_use_id":"srvtoolu_b","content":{"type":"web_search_tool_result_error","error_code":"max_uses_exceeded"}},
			{"type":"text","text":"done"}]`)},
		{Role: "user", Content: json.RawMessage(`"next"`)},
	}
	text := decode(t, "k1", unpaired)
	for _, want := range []string{"Web search: only call", "Web search results:\nExample\nhttps://example.com", "Web search: limited\nWeb search failed: max_uses_exceeded", "done"} {
		if !strings.Contains(text, want) {
			t.Fatalf("缺少 %q: %q", want, text)
		}
	}

	// 结果内容既不是数组也不是错误对象时仍返回 400
	s := &server{config: Config{APIKey: "k1"}}
	bad := anthropicRequest{Model: "m", Messages: []anthropicMessage{{Role: "assistant", Content: json.RawMessage(`[{"type":"web_search_tool_result","tool_use_id":"x","content":"oops"}]`)}}}
	if err := s.decodeAnthropicSearchHistory(&bad); err == nil {
		t.Fatal("无法识别的搜索结果内容应返回错误")
	}
}

// TestAnthropicServerToolHistoryAsText 其他服务端工具（web_fetch、code_execution、tool_search）的历史调用与结果转为文本
func TestAnthropicServerToolHistoryAsText(t *testing.T) {
	s := &server{config: Config{APIKey: "k1"}}
	request := anthropicRequest{Model: "m", Messages: []anthropicMessage{
		{Role: "user", Content: json.RawMessage(`"fetch"`)},
		{Role: "assistant", Content: json.RawMessage(`[
			{"type":"server_tool_use","id":"srvtoolu_1","name":"web_fetch","input":{"url":"https://go.dev"}},
			{"type":"web_fetch_tool_result","tool_use_id":"srvtoolu_1","content":{"type":"web_fetch_result","url":"https://go.dev",
				"content":{"type":"document","source":{"type":"text","media_type":"text/plain","data":"Go is an open source language"},"title":"Go"}}},
			{"type":"server_tool_use","id":"srvtoolu_2","name":"web_fetch","input":{"url":"https://bad.example"}},
			{"type":"web_fetch_tool_result","tool_use_id":"srvtoolu_2","content":{"type":"web_fetch_tool_error","error_code":"url_not_accessible"}},
			{"type":"server_tool_use","id":"srvtoolu_3","name":"code_execution","input":{"code":"print(1)"}},
			{"type":"code_execution_tool_result","tool_use_id":"srvtoolu_3","content":{"type":"code_execution_result","stdout":"1\n","stderr":"","return_code":0}},
			{"type":"tool_search_tool_result","tool_use_id":"srvtoolu_4","content":{"type":"tool_search_tool_search_result","tool_references":[]}},
			{"type":"text","text":"done"}]`)},
		{Role: "user", Content: json.RawMessage(`[{"type":"search_result","source":"https://kb.example/1","title":"KB","content":[{"type":"text","text":"kb body"}]},{"type":"text","text":"next"}]`)},
	}}
	if err := s.decodeAnthropicSearchHistory(&request); err != nil {
		t.Fatalf("其他服务端工具不应被当作 web_search 拒绝: %v", err)
	}
	generate, err := request.toGenerateRequest("id")
	if err != nil {
		t.Fatalf("服务端工具历史应转为文本: %v", err)
	}
	var text strings.Builder
	for _, content := range generate.Contents {
		for _, part := range content.Parts {
			text.WriteString(part.Text + "\n")
		}
	}
	for _, want := range []string{
		`Server tool call web_fetch: {"url":"https://go.dev"}`, "Web fetch: https://go.dev", "Go is an open source language",
		"Web fetch failed: url_not_accessible", "Server tool call code_execution", "Server tool result code_execution_tool_result",
		"Server tool result tool_search_tool_result", "Search result: KB\nhttps://kb.example/1", "kb body",
	} {
		if !strings.Contains(text.String(), want) {
			t.Fatalf("缺少 %q: %q", want, text.String())
		}
	}
}

// TestAnthropicDocumentSources document 接受 text、content 与 file 来源；image 也接受 file；缺少 file_id 或不支持的来源返回错误
func TestAnthropicDocumentSources(t *testing.T) {
	parts, err := anthropicParts(json.RawMessage(`[
		{"type":"document","source":{"type":"text","media_type":"text/plain","data":"plain doc"},"title":"T"},
		{"type":"document","source":{"type":"content","content":[{"type":"text","text":"chunk one"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"iVBORw0KGgo="}}]}},
		{"type":"document","source":{"type":"content","content":"string content"}},
		{"type":"document","source":{"type":"file","file_id":" file_abc "}},
		{"type":"image","source":{"type":"file","file_id":"file_img"}},
		{"type":"document","source":{"type":"text","media_type":"text/plain","data":""}}]`))
	if err != nil {
		t.Fatalf("document 来源应被接受: %v", err)
	}
	if len(parts) != 6 || parts[0].Text != "plain doc" || parts[1].Text != "chunk one" || parts[2].InlineData == nil ||
		parts[3].Text != "string content" || parts[4].File == nil || parts[4].File.ID != "file_abc" || parts[5].File == nil || parts[5].File.ID != "file_img" {
		t.Fatalf("parts=%+v", parts)
	}
	for name, block := range map[string]string{
		"缺少 file_id":     `[{"type":"document","source":{"type":"file"}}]`,
		"image 不接受 text": `[{"type":"image","source":{"type":"text","data":"x"}}]`,
		"未知来源":           `[{"type":"document","source":{"type":"weird"}}]`,
		"缺少 source":      `[{"type":"document"}]`,
	} {
		if _, err := anthropicParts(json.RawMessage(block)); err == nil {
			t.Fatalf("%s 应返回错误", name)
		}
	}
}

// TestAnthropicStructuredOutput output_config.format 与 beta output_format 映射为 JSON 模式与 responseSchema，前者优先
func TestAnthropicStructuredOutput(t *testing.T) {
	convert := func(t *testing.T, body string) (aistudio.GenerateRequest, error) {
		t.Helper()
		var request anthropicRequest
		decodeInto(t, `{"model":"m","max_tokens":16,"messages":[{"role":"user","content":"hi"}],`+body+`}`, &request)
		return request.toGenerateRequest("id")
	}
	schema := `{"type":"object","properties":{"name":{"type":"string"}},"required":["name"]}`
	generate, err := convert(t, `"output_config":{"effort":"low","format":{"type":"json_schema","schema":`+schema+`}}`)
	if err != nil || generate.Config.ResponseMIMEType != "application/json" || string(generate.Config.ResponseSchema) != schema || generate.Config.ReasoningEffort != "low" {
		t.Fatalf("output_config.format: config=%+v err=%v", generate.Config, err)
	}
	generate, err = convert(t, `"output_format":{"type":"json_schema","schema":`+schema+`}`)
	if err != nil || generate.Config.ResponseMIMEType != "application/json" || string(generate.Config.ResponseSchema) != schema {
		t.Fatalf("output_format: config=%+v err=%v", generate.Config, err)
	}
	generate, err = convert(t, `"output_config":{"format":{"type":"json_schema","schema":{"type":"object","properties":{"a":{"type":"string"}}}}},"output_format":{"type":"json_schema","schema":`+schema+`}`)
	if err != nil || !strings.Contains(string(generate.Config.ResponseSchema), `"a"`) {
		t.Fatalf("两种写法同时出现时应以 output_config.format 为准: %s %v", generate.Config.ResponseSchema, err)
	}
	generate, err = convert(t, `"output_format":{"type":"json_schema","schema":null}`)
	if err != nil || generate.Config.ResponseMIMEType != "application/json" || generate.Config.ResponseSchema != nil {
		t.Fatalf("schema 为 null 时只保留 JSON 模式: %+v %v", generate.Config, err)
	}
	if _, err := convert(t, `"output_config":{"format":{"type":"text"}}`); err == nil {
		t.Fatal("不支持的格式类型应返回错误")
	}
	generate, err = convert(t, `"output_config":{"effort":"high"}`)
	if err != nil || generate.Config.ResponseMIMEType != "" || generate.Config.ResponseSchema != nil {
		t.Fatalf("没有 format 时不启用结构化输出: %+v %v", generate.Config, err)
	}

	// Schema 含无法直接编码的写法时与其他协议一样降级编码，仍按 JSON 模式发送
	fallback := `{"type":"object","$defs":{"tag":{"type":"string"}},"properties":{"tags":{"type":"array","items":{"$ref":"#/$defs/tag"}}}}`
	generate, err = convert(t, `"output_config":{"format":{"type":"json_schema","schema":`+fallback+`}}`)
	if err != nil {
		t.Fatal(err)
	}
	wire, err := aistudio.EncodeGenerateContentRequest(generate, aistudio.GenerationDefaults{MaxOutputTokens: 1024}, aistudio.RequestContext{})
	if err != nil {
		t.Fatalf("降级 Schema 应能编码: %v", err)
	}
	if !strings.Contains(string(wire), `"application/json"`) || !strings.Contains(string(wire), "tags") {
		t.Fatalf("降级后仍应按 JSON 模式发送: %s", wire)
	}
}

// TestAPIKeyAnySourceMatches ?key=、X-Goog-API-Key、X-API-Key、api-key、Bearer 任一与配置的密钥相同即放行
func TestAPIKeyAnySourceMatches(t *testing.T) {
	handler := publicTestHandler("sk-custom")
	for _, test := range []struct {
		name   string
		query  string
		header map[string]string
		want   int
	}{
		{name: "旧 key 参数加正确请求头", query: "?key=sk-stale", header: map[string]string{"X-Goog-API-Key": "sk-custom"}, want: http.StatusNoContent},
		{name: "正确 key 参数加错误 Bearer", query: "?key=sk-custom", header: map[string]string{"Authorization": "Bearer sk-wrong"}, want: http.StatusNoContent},
		{name: "错误 X-API-Key 加正确 Bearer", header: map[string]string{"X-API-Key": "sk-wrong", "Authorization": "Bearer sk-custom"}, want: http.StatusNoContent},
		{name: "api-key 请求头", header: map[string]string{"api-key": "sk-custom"}, want: http.StatusNoContent},
		{name: "X-API-Key 请求头", header: map[string]string{"X-API-Key": " sk-custom "}, want: http.StatusNoContent},
		{name: "全部错误", query: "?key=sk-stale", header: map[string]string{"X-Goog-API-Key": "a", "X-API-Key": "b", "api-key": "c", "Authorization": "Bearer d"}, want: http.StatusUnauthorized},
		{name: "Basic 不算 Bearer", header: map[string]string{"Authorization": "Basic sk-custom"}, want: http.StatusUnauthorized},
		{name: "没有密钥", want: http.StatusUnauthorized},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/v1beta/models/m:generateContent"+test.query, nil)
			request.RemoteAddr = "127.0.0.1:5555"
			for name, value := range test.header {
				request.Header.Set(name, value)
			}
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, request)
			if recorder.Code != test.want {
				t.Fatalf("status = %d，期望 %d", recorder.Code, test.want)
			}
		})
	}
}

// TestGeminiUnspecifiedEnumsAndRetrieval MODALITY_UNSPECIFIED 与 MODE_UNSPECIFIED 按未设置处理；带 dynamicRetrievalConfig 的 googleSearchRetrieval 映射为 Google Search
func TestGeminiUnspecifiedEnumsAndRetrieval(t *testing.T) {
	var request geminiRequest
	decodeInto(t, `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],
		"generationConfig":{"responseModalities":["MODALITY_UNSPECIFIED"]},
		"tools":[{"googleSearchRetrieval":{"dynamicRetrievalConfig":{"mode":"MODE_DYNAMIC","dynamicThreshold":0.7}}}],
		"toolConfig":{"functionCallingConfig":{"mode":"MODE_UNSPECIFIED"}}}`, &request)
	generate, err := request.toGenerateRequest("id", "gemini-x")
	if err != nil {
		t.Fatalf("枚举默认值与 dynamicRetrievalConfig 应被接受: %v", err)
	}
	if generate.Config.ResponseModalities != nil {
		t.Fatalf("只有 MODALITY_UNSPECIFIED 时应按未设置处理: %v", generate.Config.ResponseModalities)
	}
	if generate.Tools.ToolConfig.Mode != "auto" || !slices.Contains(generate.Tools.Google, "google_search") {
		t.Fatalf("tools=%+v", generate.Tools)
	}
	modalities, err := mapGeminiResponseModalities([]string{"TEXT", "MODALITY_UNSPECIFIED", "image"})
	if err != nil || !slices.Equal(modalities, []aistudio.ResponseModality{aistudio.ResponseModalityText, aistudio.ResponseModalityImage}) {
		t.Fatalf("modalities=%v err=%v", modalities, err)
	}
	if _, err := mapGeminiResponseModalities([]string{"VIDEO"}); err == nil {
		t.Fatal("不支持的模态仍应返回错误")
	}
	var invalid geminiRequest
	decodeInto(t, `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"tools":[{"googleSearchRetrieval":"on"}]}`, &invalid)
	if _, err := invalid.toGenerateRequest("id", "gemini-x"); err == nil {
		t.Fatal("googleSearchRetrieval 不是对象时仍应返回错误")
	}
}

// TestGeminiFileDataWithoutMIME fileData 省略 mimeType 时按 displayName 或 fileUri 推断，推断不出时留空；fileUri 仍为必填
func TestGeminiFileDataWithoutMIME(t *testing.T) {
	parts, _, err := mapGeminiParts([]geminiPart{
		{FileData: &geminiFilePart{FileURI: "https://storage.example/report.pdf?sig=abc"}},
		{FileData: &geminiFilePart{FileURI: "files/abc123", DisplayName: "photo.PNG"}},
		{FileData: &geminiFilePart{FileURI: "files/xyz"}},
		{FileDataSnake: &geminiFilePart{FileURISnake: "files/keep", MIMETypeSnake: "audio/wav"}},
		{FileData: &geminiFilePart{FileURI: "https://youtu.be/dQw4w9WgXcQ"}},
	})
	if err != nil {
		t.Fatalf("省略 mimeType 应被接受: %v", err)
	}
	want := []string{"application/pdf", "image/png", "", "audio/wav"}
	for index, mime := range want {
		if parts[index].File == nil || parts[index].File.MIME != mime {
			t.Fatalf("parts[%d]=%+v，期望 MIME %q", index, parts[index].File, mime)
		}
	}
	if parts[4].ExternalMedia == nil {
		t.Fatalf("YouTube 链接仍按外部媒体发送: %+v", parts[4])
	}
	if _, _, err := mapGeminiParts([]geminiPart{{FileData: &geminiFilePart{MIMEType: "image/png"}}}); err == nil || !strings.Contains(err.Error(), "fileUri") {
		t.Fatalf("缺少 fileUri 应返回错误，得到 %v", err)
	}
}

// TestStreamHeartbeatFormats Gemini SSE 心跳为空行（JSON 数组模式同样为换行），Anthropic 为官方 ping 事件，OpenAI 保持 SSE 注释
func TestStreamHeartbeatFormats(t *testing.T) {
	for _, sse := range []bool{true, false} {
		recorder := httptest.NewRecorder()
		writer := &geminiChunkWriter{w: recorder, sse: sse}
		if err := writer.heartbeat(); err != nil {
			t.Fatal(err)
		}
		if recorder.Body.String() != "\n" {
			t.Fatalf("Gemini 心跳(sse=%v)=%q", sse, recorder.Body.String())
		}
	}
	recorder := httptest.NewRecorder()
	if err := (&anthropicStreamWriter{w: recorder}).heartbeat(); err != nil {
		t.Fatal(err)
	}
	if recorder.Body.String() != "event: ping\ndata: {\"type\":\"ping\"}\n\n" {
		t.Fatalf("Anthropic 心跳=%q", recorder.Body.String())
	}
	recorder = httptest.NewRecorder()
	if err := writeSSEHeartbeat(recorder); err != nil {
		t.Fatal(err)
	}
	if recorder.Body.String() != ": ping\n\n" {
		t.Fatalf("OpenAI 心跳=%q", recorder.Body.String())
	}
}
