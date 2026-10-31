package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// sequenceService 按调用顺序返回 replies 里的事件（超出时重复最后一组），并记录每次收到的生成请求
type sequenceService struct {
	modelListService
	replies [][]aistudio.Event

	mu       sync.Mutex
	requests []aistudio.GenerateRequest
}

func (service *sequenceService) Generate(_ context.Context, request aistudio.GenerateRequest) (<-chan aistudio.Event, error) {
	service.mu.Lock()
	index := len(service.requests)
	service.requests = append(service.requests, request)
	service.mu.Unlock()
	reply := service.replies[min(index, len(service.replies)-1)]
	events := make(chan aistudio.Event, len(reply))
	for _, event := range reply {
		events <- event
	}
	close(events)
	return events, nil
}

func (service *sequenceService) request(t *testing.T, index int) aistudio.GenerateRequest {
	t.Helper()
	service.mu.Lock()
	defer service.mu.Unlock()
	if index >= len(service.requests) {
		t.Fatalf("生成服务只收到 %d 个请求，期望第 %d 个", len(service.requests), index+1)
	}
	return service.requests[index]
}

func toolCallEvent(id string, name string, arguments string, signature string) aistudio.Event {
	return aistudio.Event{
		Kind: aistudio.EventToolCall, ThoughtSignature: signature,
		ToolCall: &aistudio.FunctionCall{ID: id, Name: name, Arguments: json.RawMessage(arguments), ThoughtSignature: signature},
	}
}

func signatureOf(part aistudio.Part) (string, string) {
	if part.FunctionCall == nil {
		return part.ThoughtSignature, ""
	}
	return part.ThoughtSignature, part.FunctionCall.ThoughtSignature
}

// testSignatureScope 为签名测试用的作用域（模型、号池、API key 摘要）
const testSignatureScope = "gemini-3.8-flash\x000\x00key"

// TestThoughtSignatureRestoreRules 签名还原规则：签名为空或为两种占位值的调用按调用 ID 加函数名还原（本服务补发的 call_local_* 参数被改写也认，
// 其他 ID 要求参数一致）；带 ID 却查不到、或登记为“已签发、没有签名”的调用保持原样；不带 ID 的调用（即使同名同参）不还原；
// Part 与 FunctionCall 两处都改写（按下标改原切片）；客户端带回的真实签名不动
func TestThoughtSignatureRestoreRules(t *testing.T) {
	store := newThoughtSignatureStore()
	store.Remember(testSignatureScope, []aistudio.FunctionCall{
		{ID: "call_local_a", Name: "search_web", Arguments: json.RawMessage(`{"query":"a","limit":3}`), ThoughtSignature: "sig-a"},
		{ID: "call_local_b", Name: "search_web", Arguments: json.RawMessage(`{"query":"b"}`), ThoughtSignature: "sig-b"},
		// 签名为空或占位值的调用登记为“已签发、没有签名”
		{ID: "call_local_c", Name: "search_web", Arguments: json.RawMessage(`{"query":"c"}`), ThoughtSignature: placeholderSignatureNewAPI},
		{ID: "call_local_d", Name: "search_web", Arguments: json.RawMessage(`{"query":"d"}`)},
		// 上游给的调用 ID（不是本服务补发的格式）
		{ID: "fc_upstream_e", Name: "get_time", Arguments: json.RawMessage(`{}`), ThoughtSignature: "sig-e"},
	})
	// 同一个 ID 之后再以没有签名的形式登记（例如另一条路径只看到调用本身）不覆盖已有签名
	store.Remember(testSignatureScope, []aistudio.FunctionCall{{ID: "call_local_a", Name: "search_web", Arguments: json.RawMessage(`{"query":"a","limit":3}`)}})
	call := func(id string, name string, arguments string, partSignature string, callSignature string) aistudio.Part {
		return aistudio.Part{
			FunctionCall:     &aistudio.FunctionCall{ID: id, Name: name, Arguments: json.RawMessage(arguments), ThoughtSignature: callSignature},
			ThoughtSignature: partSignature,
		}
	}
	contents := []aistudio.Content{
		{Role: aistudio.RoleUser, Parts: []aistudio.Part{{Text: "hi"}}},
		{Role: aistudio.RoleAssistant, Parts: []aistudio.Part{
			call("call_local_a", "search_web", `{"query":"a","limit":3}`, placeholderSignatureNewAPI, placeholderSignatureNewAPI), // 0 按 ID，占位值
			call("call_other", "search_web", `{ "limit": 3, "query": "a" }`, "", ""),                                              // 1 ID 查不到：保持原样
			call("call_local_b", "search_web", `{"query":"changed"}`, skipThoughtSignature, ""),                                   // 2 本服务 ID（参数被改写也认）
			call("call_local_b", "search_web", `{"query":"b"}`, "sig-from-client", "sig-from-client"),                             // 3 客户端带回的真签名
			call("call_local_c", "search_web", `{"query":"c"}`, placeholderSignatureNewAPI, placeholderSignatureNewAPI),           // 4 已签发、没有签名
			call("call_local_d", "search_web", `{"query":"d"}`, "", ""),                                                           // 5 已签发、没有签名
			{Text: "说明", ThoughtSignature: placeholderSignatureNewAPI},                                                            // 6 非调用 part 不动
			call("fc_upstream_e", "get_time", `null`, "", ""),                                                                     // 7 上游 ID，参数一致（null 与 {} 相同）
			call("fc_upstream_e", "get_time", `{"zone":"UTC"}`, "", ""),                                                           // 8 上游 ID，参数不一致
		}},
		{Role: aistudio.RoleUser, Parts: []aistudio.Part{{Text: "继续"}}},
		{Role: aistudio.RoleAssistant, Parts: []aistudio.Part{
			call("", "search_web", `{"limit":3,"query":"a"}`, "", ""), // 0 不带 ID、与 call_local_a 同名同参：不还原
			call("", "get_time", `{}`, "", ""),                        // 1 不带 ID、与 fc_upstream_e 同名同参：不还原
		}},
		{Role: aistudio.RoleAssistant, Parts: []aistudio.Part{
			{Text: "先说明再调用"},
			call("", "get_time", `{}`, placeholderSignatureNewAPI, ""), // 0 不带 ID、占位值：保持原样
		}},
	}
	original := contents[1].Parts[0].FunctionCall
	if restored := store.Restore(testSignatureScope, contents); restored != 3 {
		t.Fatalf("还原个数 = %d，期望 3", restored)
	}
	for _, check := range []struct {
		content, part int
		want          string
	}{
		{1, 0, "sig-a"}, {1, 1, ""}, {1, 2, "sig-b"}, {1, 3, "sig-from-client"}, {1, 4, placeholderSignatureNewAPI}, {1, 5, ""},
		{1, 7, "sig-e"}, {1, 8, ""}, {3, 0, ""}, {3, 1, ""},
	} {
		partSignature, callSignature := signatureOf(contents[check.content].Parts[check.part])
		if partSignature != check.want || callSignature != check.want {
			t.Fatalf("contents[%d].parts[%d] 签名 = %q/%q，期望 Part 与 FunctionCall 都是 %q", check.content, check.part, partSignature, callSignature, check.want)
		}
	}
	if partSignature, callSignature := signatureOf(contents[4].Parts[1]); partSignature != placeholderSignatureNewAPI || callSignature != "" {
		t.Fatalf("不带 ID 的调用应保持原样: %q/%q", partSignature, callSignature)
	}
	if contents[1].Parts[6].ThoughtSignature != placeholderSignatureNewAPI {
		t.Fatalf("非调用 part 的签名不应改动: %q", contents[1].Parts[6].ThoughtSignature)
	}
	if original.ThoughtSignature != placeholderSignatureNewAPI {
		t.Fatal("还原改写的是请求自己的副本，不应改动调用方持有的 FunctionCall")
	}
}

// TestThoughtSignatureNotBorrowed 签名不能套到别的调用上：并行调用的第二个（Gemini 只给第一个签名）与更早一次带签名的调用同名同参时
// 仍保持缺失（编码补跳过校验值）；另一个模型、另一个号池或换了 API key 的请求不还原
func TestThoughtSignatureNotBorrowed(t *testing.T) {
	store := newThoughtSignatureStore()
	// 第 1 轮（或另一段对话）：search_web{q:x} 是带签名的第一个调用
	store.Remember(testSignatureScope, []aistudio.FunctionCall{{ID: "call_local_1", Name: "search_web", Arguments: json.RawMessage(`{"q":"x"}`), ThoughtSignature: "S1"}})
	// 第 3 轮：并行调用，第二个与第 1 轮同名同参、没有签名
	store.Remember(testSignatureScope, []aistudio.FunctionCall{
		{ID: "call_local_8", Name: "search_web", Arguments: json.RawMessage(`{"q":"y"}`), ThoughtSignature: "S3"},
		{ID: "call_local_9", Name: "search_web", Arguments: json.RawMessage(`{"q":"x"}`)},
	})
	round4 := func() []aistudio.Content {
		return []aistudio.Content{
			{Role: aistudio.RoleUser, Parts: []aistudio.Part{{Text: "hi"}}},
			{Role: aistudio.RoleAssistant, Parts: []aistudio.Part{
				{FunctionCall: &aistudio.FunctionCall{ID: "call_local_8", Name: "search_web", Arguments: json.RawMessage(`{"q":"y"}`)}, ThoughtSignature: placeholderSignatureNewAPI},
				{FunctionCall: &aistudio.FunctionCall{ID: "call_local_9", Name: "search_web", Arguments: json.RawMessage(`{"q":"x"}`)}},
			}},
		}
	}
	contents := round4()
	if restored := store.Restore(testSignatureScope, contents); restored != 1 {
		t.Fatalf("还原个数 = %d，期望只还原第一个调用", restored)
	}
	if part, call := signatureOf(contents[1].Parts[0]); part != "S3" || call != "S3" {
		t.Fatalf("第一个调用 = %q/%q，期望 S3", part, call)
	}
	if part, call := signatureOf(contents[1].Parts[1]); part != "" || call != "" {
		t.Fatalf("第二个并行调用不应被套上别的轮次的签名: %q/%q", part, call)
	}
	// 编码时第二个调用补跳过校验值
	wire, err := aistudio.EncodeCountTokensRequest(aistudio.TokenCountRequest{Model: "gemini-3.8-flash", Contents: contents})
	if err != nil {
		t.Fatal(err)
	}
	if text := string(wire); strings.Count(text, `"skip_thought_signature_validator"`) != 1 || strings.Contains(text, `"S1"`) || !strings.Contains(text, `"S3"`) {
		t.Fatalf("编码应为 S3 与一个跳过校验值、没有 S1: %s", text)
	}
	// 作用域不同：另一个模型、号池或 API key
	for _, scope := range []string{"gemini-3.1-pro-preview\x000\x00key", "gemini-3.8-flash\x003\x00key", "gemini-3.8-flash\x000\x00other"} {
		contents := round4()
		if restored := store.Restore(scope, contents); restored != 0 {
			t.Fatalf("作用域 %q 不应还原，得到 %d 个", scope, restored)
		}
	}
	// 经服务入口：号池与 API key 计入作用域，带后缀的别名与目录模型名同一作用域
	catalog := modelListService{models: []aistudio.Model{{ID: "gemini-3.8-flash", Methods: []string{"generateContent"}, Capabilities: map[string]bool{"chat_model": true, "thinking_level": true}}}}
	s := &server{service: &scriptedService{modelListService: catalog}, config: Config{APIKey: "sk-a"}}
	plain := s.thoughtSignatureScope(context.Background(), "models/gemini-3.8-flash")
	if alias := s.thoughtSignatureScope(context.Background(), "gemini-3.8-flash-high"); alias != plain {
		t.Fatalf("带后缀的别名应与目录模型名同一作用域: %q / %q", alias, plain)
	}
	if ultra := s.thoughtSignatureScope(aistudio.ContextWithPoolScope(context.Background(), aistudio.PoolScopeUltra), "gemini-3.8-flash"); ultra == plain {
		t.Fatal("Ultra 号池应是另一个作用域")
	}
	rotated := &server{service: s.service, config: Config{APIKey: "sk-b"}}
	if rotated.thoughtSignatureScope(context.Background(), "gemini-3.8-flash") == plain {
		t.Fatal("换了 API key 应是另一个作用域")
	}
}

// TestThoughtSignatureNotSharedAcrossConversations 经中转时所有终端用户共用本服务的同一把 API key，作用域分不出对话：
// 两段首条用户消息不同的会话调用同名同参的函数，另一段会话的历史（客户端去掉了调用 ID 与签名，或带着本会话自己的 ID）不会被套上
// 这一段签发的签名；同一会话两个步骤同名同参时，各步骤只拿回自己那一步的签名，去掉 ID 后都不还原、不串用最后一次的签名
func TestThoughtSignatureNotSharedAcrossConversations(t *testing.T) {
	service := &sequenceService{replies: [][]aistudio.Event{
		// 会话 A：get_time{}，签名 S_A
		{toolCallEvent("call_local_ta", "get_time", `{}`, "S_A"), {Kind: aistudio.EventFinish, FinishReason: "stop"}},
		// 会话 C 的两个步骤：都是 search_web{q:天气}，签名分别为 S_C1、S_C2
		{toolCallEvent("call_local_c1", "search_web", `{"q":"天气"}`, "S_C1"), {Kind: aistudio.EventFinish, FinishReason: "stop"}},
		{toolCallEvent("call_local_c2", "search_web", `{"q":"天气"}`, "S_C2"), {Kind: aistudio.EventFinish, FinishReason: "stop"}},
		{{Kind: aistudio.EventText, Text: "好的"}, {Kind: aistudio.EventFinish, FinishReason: "stop"}},
	}}
	handler := NewHandler(service, Config{APIKey: "sk-test"})
	stream := "/v1beta/models/gemini-3.8-flash:streamGenerateContent?alt=sse"
	for index, body := range []string{
		`{"contents":[{"role":"user","parts":[{"text":"会话 A：现在几点"}]}]}`,
		`{"contents":[{"role":"user","parts":[{"text":"会话 C：查天气"}]}]}`,
		`{"contents":[{"role":"user","parts":[{"text":"会话 C：查天气"}]},{"role":"model","parts":[{"functionCall":{"id":"call_local_c1","name":"search_web","args":{"q":"天气"}}}]},{"role":"user","parts":[{"functionResponse":{"id":"call_local_c1","name":"search_web","response":{"content":"暂无"}}}]}]}`,
	} {
		if recorder := postJSON(t, handler, stream, body, nil); recorder.Code != http.StatusOK {
			t.Fatalf("第 %d 个请求 status=%d body=%s", index+1, recorder.Code, recorder.Body.String())
		}
	}
	signatures := func(contents []aistudio.Content) []string {
		var result []string
		for _, content := range contents {
			for _, part := range content.Parts {
				if part.FunctionCall != nil {
					partSignature, callSignature := signatureOf(part)
					result = append(result, partSignature+"/"+callSignature)
				}
			}
		}
		return result
	}
	cases := []struct {
		name string
		body string
		want []string
	}{
		{
			// 会话 B：首条用户消息不同，历史里同名同参的 get_time{} 不带 ID 与签名
			name: "另一段会话不带 ID",
			body: `{"contents":[{"role":"user","parts":[{"text":"会话 B：报一下时间"}]},{"role":"model","parts":[{"functionCall":{"name":"get_time","args":{}}}]},{"role":"user","parts":[{"functionResponse":{"name":"get_time","response":{"content":"12:00"}}}]}]}`,
			want: []string{"/"},
		},
		{
			// 会话 B：带着 new-api 的占位签名与一个不是本服务签发的 ID
			name: "另一段会话的 ID",
			body: `{"contents":[{"role":"user","parts":[{"text":"会话 B：报一下时间"}]},{"role":"model","parts":[{"functionCall":{"id":"call_b_1","name":"get_time","args":{}},"thoughtSignature":"context_engineering_is_the_way_to_go"}]},{"role":"user","parts":[{"functionResponse":{"id":"call_b_1","name":"get_time","response":{"content":"12:00"}}}]}]}`,
			want: []string{placeholderSignatureNewAPI + "/" + placeholderSignatureNewAPI},
		},
		{
			// 会话 C 第 3 轮：两个步骤按各自的 ID 还原，不串用
			name: "同一会话按 ID",
			body: `{"contents":[{"role":"user","parts":[{"text":"会话 C：查天气"}]},{"role":"model","parts":[{"functionCall":{"id":"call_local_c1","name":"search_web","args":{"q":"天气"}},"thoughtSignature":"context_engineering_is_the_way_to_go"}]},{"role":"user","parts":[{"functionResponse":{"id":"call_local_c1","name":"search_web","response":{"content":"暂无"}}}]},{"role":"model","parts":[{"functionCall":{"id":"call_local_c2","name":"search_web","args":{"q":"天气"}},"thoughtSignature":"context_engineering_is_the_way_to_go"}]},{"role":"user","parts":[{"functionResponse":{"id":"call_local_c2","name":"search_web","response":{"content":"晴"}}}]}]}`,
			want: []string{"S_C1/S_C1", "S_C2/S_C2"},
		},
		{
			// 会话 C 第 3 轮，客户端去掉了调用 ID：两个步骤都不还原，第一步不会拿到最后一次签发的 S_C2
			name: "同一会话不带 ID",
			body: `{"contents":[{"role":"user","parts":[{"text":"会话 C：查天气"}]},{"role":"model","parts":[{"functionCall":{"name":"search_web","args":{"q":"天气"}}}]},{"role":"user","parts":[{"functionResponse":{"name":"search_web","response":{"content":"暂无"}}}]},{"role":"model","parts":[{"functionCall":{"name":"search_web","args":{"q":"天气"}}}]},{"role":"user","parts":[{"functionResponse":{"name":"search_web","response":{"content":"晴"}}}]}]}`,
			want: []string{"/", "/"},
		},
	}
	for index, check := range cases {
		if recorder := postJSON(t, handler, stream, check.body, nil); recorder.Code != http.StatusOK {
			t.Fatalf("%s: status=%d body=%s", check.name, recorder.Code, recorder.Body.String())
		}
		got := signatures(service.request(t, 3+index).Contents)
		if strings.Join(got, ",") != strings.Join(check.want, ",") {
			t.Fatalf("%s: 签名 = %v，期望 %v", check.name, got, check.want)
		}
	}
}

// TestOpenAIRestoresPlaceholderSignature OpenAI 入口同样把两种占位值当作缺失
func TestOpenAIRestoresPlaceholderSignature(t *testing.T) {
	store := newThoughtSignatureStore()
	store.Remember(testSignatureScope, []aistudio.FunctionCall{{ID: "call_sig_1", Name: "get_weather", Arguments: json.RawMessage(`{"city":"Berlin"}`), ThoughtSignature: "sig-abc"}})
	for _, placeholder := range []string{placeholderSignatureNewAPI, skipThoughtSignature} {
		var request chatRequest
		raw := fmt.Sprintf(`{"model":"m","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":null,"tool_calls":[{"id":"call_sig_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Berlin\"}"},"extra_content":{"google":{"thought_signature":%q}}}]}]}`, placeholder)
		if err := json.Unmarshal([]byte(raw), &request); err != nil {
			t.Fatal(err)
		}
		generate, err := request.toGenerateRequest("id")
		if err != nil {
			t.Fatal(err)
		}
		store.Restore(testSignatureScope, generate.Contents)
		if part, call := signatureOf(generate.Contents[1].Parts[0]); part != "sig-abc" || call != "sig-abc" {
			t.Fatalf("占位值 %q 未还原: %q/%q", placeholder, part, call)
		}
	}
}

// newAPIRound4Body 按 new-api 的 OpenAI → Gemini 转换规则（relaykit/relayconvert/internal/oai_chat/to_gemini_chat_req.go）手写的
// “3 轮工具调用后的第 4 轮”请求：每条 assistant 消息只有第一个 functionCall 带占位签名，调用 ID 原样带回本服务输出的 call_local_*；
// 第 2 轮两个并行调用（第二个参数为空 {}，结果没有名称）；第 3 轮的参数为 null；结果分别包成 {result:[…]} 与 {content:"…"}，
// 最后一个约 20KB；tools 为 OpenAPI 方言（类型大写、nullable）
func newAPIRound4Body(t *testing.T) string {
	t.Helper()
	citations := make([]map[string]any, 0, 10)
	for index := range 10 {
		citations = append(citations, map[string]any{
			"title": fmt.Sprintf("结果 %d", index), "url": fmt.Sprintf("https://example.com/%d", index),
			"snippet": strings.Repeat("检索摘要", 20),
		})
	}
	page := strings.Repeat("网页正文段落。", 1000)
	body := map[string]any{
		"contents": []any{
			map[string]any{"role": "user", "parts": []any{map[string]any{"text": "帮我查一下今天的新闻并总结"}}},
			map[string]any{"role": "model", "parts": []any{map[string]any{
				"functionCall":     map[string]any{"id": "call_local_r1", "name": "search_web", "args": map[string]any{"query": "今天 新闻"}},
				"thoughtSignature": placeholderSignatureNewAPI,
			}}},
			map[string]any{"role": "user", "parts": []any{map[string]any{
				"functionResponse": map[string]any{"id": "call_local_r1", "name": "search_web", "response": map[string]any{"result": citations}},
			}}},
			map[string]any{"role": "model", "parts": []any{
				map[string]any{
					"functionCall":     map[string]any{"id": "call_local_r2a", "name": "search_web", "args": map[string]any{"query": "科技 新闻"}},
					"thoughtSignature": placeholderSignatureNewAPI,
				},
				map[string]any{"functionCall": map[string]any{"id": "call_local_r2b", "name": "search_web", "args": map[string]any{}}},
			}},
			map[string]any{"role": "user", "parts": []any{
				map[string]any{"functionResponse": map[string]any{"id": "call_local_r2a", "name": "search_web", "response": map[string]any{"content": "科技新闻列表"}}},
				map[string]any{"functionResponse": map[string]any{"id": "call_local_r2b", "name": "", "response": map[string]any{"content": "默认新闻列表"}}},
			}},
			map[string]any{"role": "model", "parts": []any{map[string]any{
				"functionCall":     map[string]any{"id": "call_local_r3", "name": "fetch_page", "args": nil},
				"thoughtSignature": placeholderSignatureNewAPI,
			}}},
			map[string]any{"role": "user", "parts": []any{map[string]any{
				"functionResponse": map[string]any{"id": "call_local_r3", "name": "fetch_page", "response": map[string]any{"content": page}},
			}}},
		},
		"tools": []any{map[string]any{"functionDeclarations": []any{
			map[string]any{"name": "search_web", "description": "联网检索", "parameters": map[string]any{
				"type": "OBJECT", "required": []any{"query"},
				"properties": map[string]any{"query": map[string]any{"type": "STRING"}, "limit": map[string]any{"type": "INTEGER", "nullable": true}},
			}},
			map[string]any{"name": "fetch_page", "parameters": map[string]any{
				"type": "OBJECT", "properties": map[string]any{"url": map[string]any{"type": "STRING"}},
			}},
		}}},
		"generationConfig": map[string]any{"temperature": 0.7, "thinkingConfig": map[string]any{"includeThoughts": true}},
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(page) < 20000 {
		t.Fatalf("网页正文只有 %d 字节", len(page))
	}
	return string(encoded)
}

// TestGeminiRound4AfterThreeToolRounds 完整服务流程：前 3 轮经 Gemini 原生入口输出带签名的函数调用（流式两轮、非流式一轮），
// 第 4 轮按 new-api 的规则带回占位签名、并行调用、空参数、null 参数、无名结果与 20KB 结果：请求成功；各轮第一个调用还原成真签名，
// 第二个并行调用保持缺失（编码补跳过校验值），即使另一段对话里签发过同名同参（search_web{}）的带签名调用；发往上游的编码里没有占位值、
// 结果名称已补上；首个事件很快到达，响应流里没有心跳块，按 new-api 的规则每块都被转成增量
func TestGeminiRound4AfterThreeToolRounds(t *testing.T) {
	service := &sequenceService{replies: [][]aistudio.Event{
		// 另一段对话：第一个调用为带签名的 search_web{}，与第 2 轮的第二个并行调用同名同参
		{toolCallEvent("call_local_x0", "search_web", `{}`, "sig-other"), {Kind: aistudio.EventFinish, FinishReason: "stop"}},
		{{Kind: aistudio.EventReasoning, Text: "先检索"}, toolCallEvent("call_local_r1", "search_web", `{"query":"今天 新闻"}`, "sig-r1"), {Kind: aistudio.EventFinish, FinishReason: "stop"}},
		{toolCallEvent("call_local_r2a", "search_web", `{"query":"科技 新闻"}`, "sig-r2"), toolCallEvent("call_local_r2b", "search_web", `{}`, ""), {Kind: aistudio.EventFinish, FinishReason: "stop"}},
		{toolCallEvent("call_local_r3", "fetch_page", `{}`, "sig-r3"), {Kind: aistudio.EventFinish, FinishReason: "stop"}},
		{{Kind: aistudio.EventReasoning, Text: "综合 30 个引用"}, {Kind: aistudio.EventText, Text: "今天的新闻总结如下"}, {Kind: aistudio.EventFinish, FinishReason: "stop"},
			{Kind: aistudio.EventUsage, Usage: &aistudio.Usage{InputTokens: 16000, OutputTokens: 300, TotalTokens: 16300}}},
	}}
	handler := NewHandler(service, Config{APIKey: "sk-test"})
	stream := "/v1beta/models/gemini-3.8-flash:streamGenerateContent?alt=sse"
	simple := `{"contents":[{"role":"user","parts":[{"text":"帮我查一下今天的新闻并总结"}]}]}`
	if recorder := postJSON(t, handler, stream, `{"contents":[{"role":"user","parts":[{"text":"另一段对话"}]}]}`, nil); recorder.Code != http.StatusOK {
		t.Fatalf("另一段对话 status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	for round, path := range []string{stream, stream, "/v1beta/models/gemini-3.8-flash:generateContent"} {
		if recorder := postJSON(t, handler, path, simple, nil); recorder.Code != http.StatusOK {
			t.Fatalf("第 %d 轮 status=%d body=%s", round+1, recorder.Code, recorder.Body.String())
		}
	}
	recorder := postJSON(t, handler, stream, newAPIRound4Body(t), nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("第 4 轮 status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	request := service.request(t, 4)
	contents := request.Contents
	if len(contents) != 7 {
		t.Fatalf("第 4 轮历史 %d 轮，期望 7 轮: %+v", len(contents), contents)
	}
	for _, check := range []struct {
		content, part int
		want          string
	}{{1, 0, "sig-r1"}, {3, 0, "sig-r2"}, {3, 1, ""}, {5, 0, "sig-r3"}} {
		partSignature, callSignature := signatureOf(contents[check.content].Parts[check.part])
		if partSignature != check.want || callSignature != check.want {
			t.Fatalf("contents[%d].parts[%d] 签名 = %q/%q，期望 %q", check.content, check.part, partSignature, callSignature, check.want)
		}
	}
	if args := string(contents[5].Parts[0].FunctionCall.Arguments); args != `{}` {
		t.Fatalf("null 参数应按 {} 处理: %s", args)
	}
	if result := contents[4].Parts[1].FunctionResult; result == nil || result.Name != "" || contents[4].Role != aistudio.RoleTool {
		t.Fatalf("无名结果应原样交给编码层补名: %+v", contents[4])
	}
	if result := contents[6].Parts[0].FunctionResult; result == nil || len(result.Content) < 20000 {
		t.Fatalf("20KB 结果应完整保留: %+v", contents[6])
	}
	if len(request.Tools.Functions) != 2 || !strings.Contains(string(request.Tools.Functions[0].Parameters), `"OBJECT"`) {
		t.Fatalf("Playground 通道的函数声明应原样保留 OpenAPI 写法（由编码处理）: %+v", request.Tools.Functions)
	}

	// 发往上游的编码（Playground）：没有占位值，三个真签名都在，第二个并行调用为跳过校验值，无名结果补上了函数名
	wire, err := aistudio.EncodeCountTokensRequest(aistudio.TokenCountRequest{Model: request.Model, Contents: contents, Tools: request.Tools})
	if err != nil {
		t.Fatalf("编码失败: %v", err)
	}
	text := string(wire)
	if strings.Contains(text, placeholderSignatureNewAPI) || strings.Contains(text, `"sig-other"`) {
		t.Fatal("编码里不应再有 new-api 的占位签名，也不应有另一段对话的签名")
	}
	for _, want := range []string{`"sig-r1"`, `"sig-r2"`, `"sig-r3"`, `"skip_thought_signature_validator"`} {
		if !strings.Contains(text, want) {
			t.Fatalf("编码里缺少 %s", want)
		}
	}
	if strings.Count(text, `["search_web",`) < 5 {
		t.Fatalf("三个 search_web 调用与三个结果都应带函数名（无名结果按调用 ID 补名）: %s", text[:min(len(text), 2000)])
	}

	// 响应：首个事件很快到达，没有心跳块，依次为思考、正文、结束；按 new-api 的规则每块都会写给它的客户端
	kinds := geminiStreamKinds(t, recorder.Body.String())
	if strings.Join(kinds, ",") != "thought,text,finish" {
		t.Fatalf("第 4 轮响应块 = %v", kinds)
	}
	for _, line := range strings.Split(recorder.Body.String(), "\n") {
		if strings.HasPrefix(line, "data: ") {
			if _, forwarded := newAPIForward(t, line); !forwarded {
				t.Fatalf("new-api 应转发每一块: %s", line)
			}
		}
	}
}

// TestGeminiToolValidationRelaxed 放宽的校验：functionResponse 没有名称时不再 400（编码按调用 ID 补名）；functionCall.args 为 null 按 {}；
// 函数结果为 null 按 {}；参数不是对象时仍按 400 返回
func TestGeminiToolValidationRelaxed(t *testing.T) {
	path := "/v1beta/models/gemini-3.8-flash:generateContent"
	reply := []aistudio.Event{{Kind: aistudio.EventText, Text: "ok"}, {Kind: aistudio.EventFinish, FinishReason: "stop"}}
	service := &sequenceService{replies: [][]aistudio.Event{reply}}
	handler := NewHandler(service, Config{APIKey: "sk-test"})
	body := `{"contents":[{"role":"user","parts":[{"text":"hi"}]},` +
		`{"role":"model","parts":[{"functionCall":{"id":"c1","name":"lookup","args":null}}]},` +
		`{"role":"user","parts":[{"functionResponse":{"id":"c1","response":null}}]}]}`
	if recorder := postJSON(t, handler, path, body, nil); recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	request := service.request(t, 0)
	call, result := request.Contents[1].Parts[0].FunctionCall, request.Contents[2].Parts[0].FunctionResult
	if string(call.Arguments) != `{}` || result == nil || string(result.Content) != `{}` || result.Name != "" {
		t.Fatalf("call=%+v result=%+v", call, result)
	}
	wire, err := aistudio.EncodeCountTokensRequest(aistudio.TokenCountRequest{Model: "gemini-3.8-flash", Contents: request.Contents})
	if err != nil || strings.Count(string(wire), `["lookup",`) != 2 {
		t.Fatalf("编码应为结果补上调用的名称: err=%v %s", err, wire)
	}

	for _, invalid := range []string{
		`{"contents":[{"role":"model","parts":[{"functionCall":{"name":"lookup","args":"x"}}]}]}`,
		`{"contents":[{"role":"model","parts":[{"functionCall":{"name":"","args":{}}}]}]}`,
	} {
		if recorder := postJSON(t, handler, path, invalid, nil); recorder.Code != http.StatusBadRequest {
			t.Fatalf("应返回 400: status=%d body=%s", recorder.Code, recorder.Body.String())
		}
	}
}

// TestLeadingOrphanToolResultsBecomeText 历史开头（任何函数调用之前）的工具结果改写成文字说明并改为用户消息，Gemini 与 OpenAI 入口一致；
// 后面有对应调用的结果不动
func TestLeadingOrphanToolResultsBecomeText(t *testing.T) {
	reply := []aistudio.Event{{Kind: aistudio.EventText, Text: "ok"}, {Kind: aistudio.EventFinish, FinishReason: "stop"}}
	service := &sequenceService{replies: [][]aistudio.Event{reply}}
	handler := NewHandler(service, Config{APIKey: "sk-test"})
	gemini := `{"contents":[` +
		`{"role":"user","parts":[{"functionResponse":{"id":"call_local_old","name":"","response":{"content":"旧结果"}}}]},` +
		`{"role":"user","parts":[{"text":"继续"}]},` +
		`{"role":"model","parts":[{"functionCall":{"id":"c2","name":"lookup","args":{}}}]},` +
		`{"role":"user","parts":[{"functionResponse":{"id":"c2","name":"lookup","response":{"ok":true}}}]}]}`
	if recorder := postJSON(t, handler, "/v1beta/models/gemini-3.8-flash:generateContent", gemini, nil); recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	contents := service.request(t, 0).Contents
	first := contents[0]
	if first.Role != aistudio.RoleUser || len(first.Parts) != 1 || first.Parts[0].FunctionResult != nil ||
		first.Parts[0].Text != `Tool result (call_local_old): {"content":"旧结果"}` {
		t.Fatalf("开头的孤立结果应改写成文字说明: %+v", first)
	}
	if last := contents[len(contents)-1]; last.Parts[0].FunctionResult == nil || last.Parts[0].FunctionResult.Name != "lookup" {
		t.Fatalf("有对应调用的结果不应改动: %+v", last)
	}

	openai := `{"model":"gemini-3.8-flash","messages":[{"role":"tool","tool_call_id":"call_x","name":"search_web","content":"{\"hits\":3}"},{"role":"user","content":"继续"}]}`
	if recorder := postJSON(t, handler, "/v1/chat/completions", openai, nil); recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	first = service.request(t, 1).Contents[0]
	if first.Role != aistudio.RoleUser || first.Parts[0].FunctionResult != nil || first.Parts[0].Text != `Tool result search_web: {"result":"{\"hits\":3}"}` {
		t.Fatalf("OpenAI 入口开头的孤立结果同样改写: %+v", first)
	}
}

// TestGeminiFunctionResponseMediaSplit Gemini 入口对函数结果做与 OpenAI 入口一致的媒体拆分：{"result":[…]}（new-api 的数组包装）、
// {"content":[…]}（MCP）里的 base64 图片与 functionResponse.parts 的 inlineData 都作为真正的图片附在随后的用户消息里，
// 结果原位置换成文字说明，不再按 base64 文本发送
func TestGeminiFunctionResponseMediaSplit(t *testing.T) {
	const png = "iVBORw0KGgo="
	reply := []aistudio.Event{{Kind: aistudio.EventText, Text: "ok"}, {Kind: aistudio.EventFinish, FinishReason: "stop"}}
	for _, test := range []struct {
		name     string
		response string
	}{
		{name: "result 包装", response: `{"id":"c1","name":"shot","response":{"result":[{"type":"text","text":"截图"},{"type":"image_url","image_url":{"url":"data:image/png;base64,` + png + `"}}]}}`},
		{name: "MCP content", response: `{"id":"c1","name":"shot","response":{"content":[{"type":"image","data":"` + png + `","mimeType":"image/png"}]}}`},
		{name: "parts", response: `{"id":"c1","name":"shot","response":{"ok":true},"parts":[{"inlineData":{"mimeType":"image/png","data":"` + png + `"}}]}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			service := &sequenceService{replies: [][]aistudio.Event{reply}}
			handler := NewHandler(service, Config{APIKey: "sk-test"})
			body := `{"contents":[{"role":"user","parts":[{"text":"截个图"}]},` +
				`{"role":"model","parts":[{"functionCall":{"id":"c1","name":"shot","args":{}}}]},` +
				`{"role":"user","parts":[{"functionResponse":` + test.response + `}]}]}`
			if recorder := postJSON(t, handler, "/v1beta/models/gemini-3.8-flash:generateContent", body, nil); recorder.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
			}
			contents := service.request(t, 0).Contents
			if len(contents) != 4 {
				t.Fatalf("期望工具结果之后多一条带图片的用户消息: %+v", contents)
			}
			result := contents[2]
			if len(result.Parts) != 1 || result.Parts[0].FunctionResult == nil || strings.Contains(string(result.Parts[0].FunctionResult.Content), png) {
				t.Fatalf("工具结果那一轮应只有结果、且不再带 base64: %+v", result)
			}
			media := contents[3]
			if media.Role != aistudio.RoleUser || media.Parts[0].Text != toolMediaIntro {
				t.Fatalf("图片应附在随后的用户消息里: %+v", media)
			}
			found := false
			for _, part := range media.Parts {
				found = found || part.InlineData != nil && part.InlineData.MIME == "image/png"
			}
			if !found {
				t.Fatalf("随后的用户消息里没有图片: %+v", media)
			}
		})
	}
	// parts 里不是媒体时按 400 返回
	service := &sequenceService{replies: [][]aistudio.Event{reply}}
	handler := NewHandler(service, Config{APIKey: "sk-test"})
	body := `{"contents":[{"role":"user","parts":[{"functionResponse":{"name":"shot","response":{},"parts":[{"text":"x"}]}}]}]}`
	if recorder := postJSON(t, handler, "/v1beta/models/gemini-3.8-flash:generateContent", body, nil); recorder.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

// TestGeminiMissingThoughtSignatureFinishReason 缺少思考签名的结束原因按 Gemini 官方枚举名 MISSING_THOUGHT_SIGNATURE 输出（原先为 OTHER），
// 工具调用过多为 TOO_MANY_TOOL_CALLS
func TestGeminiMissingThoughtSignatureFinishReason(t *testing.T) {
	for reason, want := range map[string]string{"missing_thought_signature": "MISSING_THOUGHT_SIGNATURE", "too_many_tool_calls": "TOO_MANY_TOOL_CALLS"} {
		if got := geminiFinishReason(reason); got != want {
			t.Fatalf("%s → %s，期望 %s", reason, got, want)
		}
	}
}
