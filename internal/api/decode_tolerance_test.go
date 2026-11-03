package api

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// rawUpstreamService 用 aistudio 的解码器（与 sendPlayground 相同的 DecodeGenerateStream 与完成帧校验）解码一段 Playground
// GenerateContent 原始响应并交给协议层：从上游原始字节到客户端响应的端到端
type rawUpstreamService struct {
	modelListService
	body string
}

func (service *rawUpstreamService) Generate(ctx context.Context, _ aistudio.GenerateRequest) (<-chan aistudio.Event, error) {
	events := make(chan aistudio.Event, 64)
	go func() {
		defer close(events)
		send := func(event aistudio.Event) error {
			select {
			case events <- event:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		decoder := aistudio.NewFrameDecoder()
		err := aistudio.DecodeGenerateStream(strings.NewReader(service.body), decoder, send)
		if err == nil {
			err = decoder.End()
		}
		if err != nil {
			_ = send(aistudio.Event{Kind: aistudio.EventError, Err: err})
		}
	}()
	return events, nil
}

// toleranceLogBuffer 截获运行日志里的解码容错行，测试结束时换回默认日志
func toleranceLogBuffer(t *testing.T) *bytes.Buffer {
	t.Helper()
	buffer := &bytes.Buffer{}
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(buffer, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return buffer
}

// toleranceKinds 统计缓冲区里各种解码容错的行数
func toleranceKinds(buffer *bytes.Buffer) map[string]int {
	counts := make(map[string]int)
	for _, line := range strings.Split(buffer.String(), "\n") {
		var entry map[string]any
		if json.Unmarshal([]byte(line), &entry) == nil && entry["msg"] == "上游响应解码容错，已继续" {
			kind, _ := entry["kind"].(string)
			counts[kind]++
		}
	}
	return counts
}

// toleranceUpstreamBody 为一段需要容错的官网响应：正文分两帧；第一帧的引用里有一条只有起止位置的条目（缺少 URI）和一条正常的；
// 第二帧的 grounding 有一个来源类型未知的 chunk（夹在 web 与图片来源之间）；完成帧的用量不是数组
const toleranceUpstreamBody = `[[` +
	`[[[[[[null,"Hello"]],"model"],null,null,null,null,null,[[[0,10],[0,5,"https://a.example","A"]]]]]],` +
	`[[[[[[null,", "]],"model"],null,null,null,null,null,[[[0,3]]]]]],` +
	`[[[[[[null,"world"]],"model"],1,null,null,null,null,null,` +
	`[null,[[["https://w.example","W"]],[null,null,null,["https://new.example"]],[null,["https://img.example/page","img.example"]]],[[[0,0,5],[0,1,2]]]]` +
	`]],null,"broken"]` +
	`]]`

// checkToleranceLogs 断言一个请求里三种容错各写了一行
func checkToleranceLogs(t *testing.T, name string, buffer *bytes.Buffer) {
	t.Helper()
	counts := toleranceKinds(buffer)
	want := map[string]int{
		"引用条目缺少 URI，已跳过该条目": 1, "grounding chunk 的来源类型未知（下标 3），换成占位项": 1, "用量不是数组，已丢弃，改用本地估算": 1,
	}
	if !reflect.DeepEqual(counts, want) {
		t.Fatalf("%s：容错日志 = %v，期望 %v", name, counts, want)
	}
	buffer.Reset()
}

// TestDecodeToleranceChatEndToEnd 原先这段响应在解码引用时按协议证据错误中止（客户端拿到 500）：现在流式与非流式都完整交出正文，
// 只丢掉缺少 URI 的那条引用与读不出的用量，grounding 的占位项不产生引用；每个请求每种容错只写一行日志
func TestDecodeToleranceChatEndToEnd(t *testing.T) {
	buffer := toleranceLogBuffer(t)
	handler := NewHandler(&rawUpstreamService{body: toleranceUpstreamBody}, Config{APIKey: "sk-test"})

	recorder := postJSON(t, handler, "/v1/chat/completions", `{"model":"gemini-3.8-flash","messages":[{"role":"user","content":"hi"}]}`, nil)
	if recorder.Code != 200 {
		t.Fatalf("非流式状态 = %d：%s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		Choices []struct {
			Message struct {
				Content     string `json:"content"`
				Annotations []struct {
					URLCitation struct {
						URL string `json:"url"`
					} `json:"url_citation"`
				} `json:"annotations"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil || len(response.Choices) != 1 {
		t.Fatalf("非流式响应 = %s", recorder.Body.String())
	}
	choice := response.Choices[0]
	var urls []string
	for _, annotation := range choice.Message.Annotations {
		urls = append(urls, annotation.URLCitation.URL)
	}
	if choice.Message.Content != "Hello, world" || choice.FinishReason != "stop" {
		t.Fatalf("非流式正文 = %q，结束 = %q", choice.Message.Content, choice.FinishReason)
	}
	if !reflect.DeepEqual(urls, []string{"https://a.example", "https://w.example", "https://img.example/page"}) {
		t.Fatalf("非流式引用 = %v，期望只丢掉缺少 URI 的条目与占位 chunk", urls)
	}
	checkToleranceLogs(t, "非流式", buffer)

	stream := postJSON(t, handler, "/v1/chat/completions", `{"model":"gemini-3.8-flash","stream":true,"messages":[{"role":"user","content":"hi"}]}`, nil)
	var content strings.Builder
	finish := ""
	for _, chunk := range sseData(t, stream.Body.String()) {
		if _, failed := chunk["error"]; failed {
			t.Fatalf("流式出现错误块：%v", chunk)
		}
		choices, _ := chunk["choices"].([]any)
		if len(choices) == 0 {
			continue
		}
		choice, _ := choices[0].(map[string]any)
		if delta, _ := choice["delta"].(map[string]any); delta != nil {
			text, _ := delta["content"].(string)
			content.WriteString(text)
		}
		if reason, _ := choice["finish_reason"].(string); reason != "" {
			finish = reason
		}
	}
	if content.String() != "Hello, world" || finish != "stop" || !strings.Contains(stream.Body.String(), "data: [DONE]") {
		t.Fatalf("流式正文 = %q，结束 = %q：%s", content.String(), finish, stream.Body.String())
	}
	// 流式同样只丢掉坏掉的那条引用：annotations 块里是与非流式相同的三个网址
	var streamURLs []string
	for _, chunk := range sseData(t, stream.Body.String()) {
		choices, _ := chunk["choices"].([]any)
		if len(choices) == 0 {
			continue
		}
		choice, _ := choices[0].(map[string]any)
		delta, _ := choice["delta"].(map[string]any)
		annotations, _ := delta["annotations"].([]any)
		for _, annotation := range annotations {
			citation, _ := annotation.(map[string]any)["url_citation"].(map[string]any)
			url, _ := citation["url"].(string)
			streamURLs = append(streamURLs, url)
		}
	}
	if !reflect.DeepEqual(streamURLs, urls) {
		t.Fatalf("流式引用 = %v，期望与非流式相同的 %v", streamURLs, urls)
	}
	checkToleranceLogs(t, "流式", buffer)
}

// TestDecodeToleranceGeminiGroundingAlignment Gemini 输出的 groundingChunks 按原位置保留占位项（空对象）与图片来源，groundingChunkIndices
// 照常对齐：web 在 0、占位项在 1、图片来源在 2
func TestDecodeToleranceGeminiGroundingAlignment(t *testing.T) {
	toleranceLogBuffer(t)
	handler := NewHandler(&rawUpstreamService{body: toleranceUpstreamBody}, Config{APIKey: "sk-test"})
	recorder := postJSON(t, handler, "/v1beta/models/gemini-3.8-flash:generateContent", `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`, nil)
	var response struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"content"`
			GroundingMetadata struct {
				GroundingChunks   []map[string]map[string]any `json:"groundingChunks"`
				GroundingSupports []struct {
					GroundingChunkIndices []int `json:"groundingChunkIndices"`
				} `json:"groundingSupports"`
			} `json:"groundingMetadata"`
		} `json:"candidates"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil || len(response.Candidates) != 1 {
		t.Fatalf("响应 = %s", recorder.Body.String())
	}
	candidate := response.Candidates[0]
	var text strings.Builder
	for _, part := range candidate.Content.Parts {
		text.WriteString(part.Text)
	}
	if text.String() != "Hello, world" {
		t.Fatalf("正文 = %q", text.String())
	}
	chunks := candidate.GroundingMetadata.GroundingChunks
	want := []map[string]map[string]any{
		{"web": {"uri": "https://w.example", "title": "W"}},
		{},
		{"image": {"sourceUri": "https://img.example/page", "domain": "img.example"}},
	}
	if !reflect.DeepEqual(chunks, want) {
		t.Fatalf("groundingChunks = %v", chunks)
	}
	if supports := candidate.GroundingMetadata.GroundingSupports; len(supports) != 1 || !reflect.DeepEqual(supports[0].GroundingChunkIndices, []int{0, 1, 2}) {
		t.Fatalf("groundingSupports = %+v", supports)
	}
}

// TestUnnamedFunctionCallPublicError 上游返回的函数调用没有函数名：仍按错误结束，对外为 502 与写明原因的英文说明（原先为 500 通用措辞）
func TestUnnamedFunctionCallPublicError(t *testing.T) {
	toleranceLogBuffer(t)
	body := `[[[[[[[[null,null,null,null,null,null,null,null,null,null,[null,[[["q",[null,null,"x"]]]]]]],"model"],1]]]]]`
	handler := NewHandler(&rawUpstreamService{body: body}, Config{APIKey: "sk-test"})
	recorder := postJSON(t, handler, "/v1/chat/completions", `{"model":"gemini-3.8-flash","messages":[{"role":"user","content":"hi"}]}`, nil)
	if recorder.Code != 502 || !strings.Contains(recorder.Body.String(), "function call without a function name") {
		t.Fatalf("状态 = %d：%s", recorder.Code, recorder.Body.String())
	}
}

// replayCaptureService 记下收到的请求并按 aistudio 的线格式编码（与 sendPlayground 相同的 EncodeGenerateContentRequest），回复一段 ok
type replayCaptureService struct {
	modelListService
	mu      sync.Mutex
	encoded []byte
	err     error
}

func (service *replayCaptureService) Generate(_ context.Context, request aistudio.GenerateRequest) (<-chan aistudio.Event, error) {
	encoded, err := aistudio.EncodeGenerateContentRequest(request, aistudio.GenerationDefaults{MaxOutputTokens: 1024}, aistudio.RequestContext{})
	service.mu.Lock()
	service.encoded, service.err = encoded, err
	service.mu.Unlock()
	events := make(chan aistudio.Event, 2)
	events <- aistudio.Event{Kind: aistudio.EventText, Text: "ok"}
	events <- aistudio.Event{Kind: aistudio.EventFinish, FinishReason: "stop"}
	close(events)
	return events, nil
}

// TestDecodeToleranceGeminiCodeExecutionRoundTrip outcome 不确定（null、不在表里）的代码执行结果：Gemini 输出把文本写进标准的 output 字段
// （原先只写非标准的 error，new-api 的 Gemini 渠道与 google-genai SDK 读不到）；客户端把这一轮原样回放时，LANGUAGE_2、OUTCOME_4 与
// 只带 output 的结果照常编码（原先报“未识别的 language/outcome”，下一轮 500），表内 outcome 的输出不变
func TestDecodeToleranceGeminiCodeExecutionRoundTrip(t *testing.T) {
	toleranceLogBuffer(t)
	body := `[[[[[[[` +
		`[null,null,null,null,null,null,null,[2,"print(1)"]],` +
		`[null,null,null,null,null,null,null,null,[null,"1\n"]],` +
		`[null,null,null,null,null,null,null,null,[4,"later"]],` +
		`[null,null,null,null,null,null,null,null,[2,"boom"]]` +
		`],"model"],1]]]]]`
	handler := NewHandler(&rawUpstreamService{body: body}, Config{APIKey: "sk-test"})
	recorder := postJSON(t, handler, "/v1beta/models/gemini-3.8-flash:generateContent", `{"contents":[{"role":"user","parts":[{"text":"run"}]}]}`, nil)
	var response struct {
		Candidates []struct {
			Content struct {
				Parts []map[string]json.RawMessage `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil || len(response.Candidates) != 1 {
		t.Fatalf("响应 = %s", recorder.Body.String())
	}
	parts := response.Candidates[0].Content.Parts
	var results []map[string]string
	var standard []map[string]json.RawMessage
	for _, part := range parts {
		if raw, ok := part["codeExecutionResult"]; ok {
			var result map[string]string
			if err := json.Unmarshal(raw, &result); err != nil {
				t.Fatal(err)
			}
			results = append(results, result)
			// 按标准字段读取的客户端：只保留 outcome 与 output
			standard = append(standard, map[string]json.RawMessage{"codeExecutionResult": mustJSON(t, map[string]string{"outcome": result["outcome"], "output": result["output"]})})
		} else {
			standard = append(standard, part)
		}
	}
	want := []map[string]string{
		{"outcome": "OUTCOME_UNSPECIFIED", "output": "1\n", "error": "1\n"},
		{"outcome": "OUTCOME_4", "output": "later", "error": "later"},
		{"outcome": "OUTCOME_FAILED", "error": "boom"},
	}
	if !reflect.DeepEqual(results, want) {
		t.Fatalf("codeExecutionResult = %v，期望 %v", results, want)
	}

	capture := &replayCaptureService{}
	replay := NewHandler(capture, Config{APIKey: "sk-test"})
	history, _ := json.Marshal(map[string]any{"contents": []any{
		map[string]any{"role": "user", "parts": []any{map[string]any{"text": "run"}}},
		map[string]any{"role": "model", "parts": standard},
		map[string]any{"role": "user", "parts": []any{map[string]any{"text": "again"}}},
	}})
	recorder = postJSON(t, replay, "/v1beta/models/gemini-3.8-flash:generateContent", string(history), nil)
	capture.mu.Lock()
	encoded, err := capture.encoded, capture.err
	capture.mu.Unlock()
	if recorder.Code != 200 || err != nil {
		t.Fatalf("回放：状态 = %d，编码错误 = %v：%s", recorder.Code, err, recorder.Body.String())
	}
	var wire []any
	if err := json.Unmarshal(encoded, &wire); err != nil {
		t.Fatal(err)
	}
	modelParts := wire[1].([]any)[1].([]any)[0].([]any)
	if len(modelParts) != 4 {
		t.Fatalf("模型轮 Part = %v", modelParts)
	}
	for index, item := range []struct {
		field int
		value []any
	}{{7, []any{2.0, "print(1)"}}, {8, []any{0.0, "1\n"}}, {8, []any{4.0, "later"}}, {8, []any{2.0}}} {
		part := modelParts[index].([]any)
		if len(part) <= item.field || !reflect.DeepEqual(part[item.field], item.value) {
			t.Fatalf("第 %d 个 Part = %v，期望 [%d] = %v", index, part, item.field, item.value)
		}
	}
}

func mustJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}
