package api

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// TestGeminiOutputParts_MergeTextAndReasoning 合并相邻思考与相邻正文，思考后单独到达的签名挂到思考 part 上
func TestGeminiOutputParts_MergeTextAndReasoning(t *testing.T) {
	parts := geminiOutputParts(generationResult{events: []aistudio.Event{
		{Kind: aistudio.EventReasoning, Text: "I think "},
		{Kind: aistudio.EventReasoning, Text: "therefore "},
		{Kind: aistudio.EventReasoning, Text: "I am."},
		{Kind: aistudio.EventThoughtSignature, ThoughtSignature: "sig_abc123"},
		{Kind: aistudio.EventText, Text: "Hello "},
		{Kind: aistudio.EventText, Text: "world!"},
	}})
	want := []map[string]any{
		{"text": "I think therefore I am.", "thought": true, "thoughtSignature": "sig_abc123"},
		{"text": "Hello world!"},
	}
	if !reflect.DeepEqual(parts, want) {
		t.Fatalf("合并结果 %+v，期望 %+v", parts, want)
	}
}

// TestGeminiOutputParts_TextOnly 连续正文合并为一个 part
func TestGeminiOutputParts_TextOnly(t *testing.T) {
	parts := geminiOutputParts(generationResult{events: []aistudio.Event{
		{Kind: aistudio.EventText, Text: "Chunk 1 "},
		{Kind: aistudio.EventText, Text: "Chunk 2 "},
		{Kind: aistudio.EventText, Text: "Chunk 3"},
	}})
	want := []map[string]any{{"text": "Chunk 1 Chunk 2 Chunk 3"}}
	if !reflect.DeepEqual(parts, want) {
		t.Fatalf("合并结果 %+v，期望 %+v", parts, want)
	}
}

// TestGeminiOutputParts_LeadingThoughtSignature 开头单独到达的签名挂到随后第一个未签名的 part 上
func TestGeminiOutputParts_LeadingThoughtSignature(t *testing.T) {
	parts := geminiOutputParts(generationResult{events: []aistudio.Event{
		{Kind: aistudio.EventThoughtSignature, ThoughtSignature: "sig_pre"},
		{Kind: aistudio.EventReasoning, Text: "Thinking step."},
		{Kind: aistudio.EventText, Text: "Final text."},
	}})
	want := []map[string]any{
		{"text": "Thinking step.", "thought": true, "thoughtSignature": "sig_pre"},
		{"text": "Final text."},
	}
	if !reflect.DeepEqual(parts, want) {
		t.Fatalf("前置签名 %+v，期望 %+v", parts, want)
	}
}

// TestGeminiOutputParts_LeadingSignatureTargets 前置签名挂到下一个未签名的 part（正文、转写、工具调用）；
// 下一个 part 自带签名时前置签名单独成 part 排在它前面，不覆盖已有签名
func TestGeminiOutputParts_LeadingSignatureTargets(t *testing.T) {
	call := aistudio.FunctionCall{ID: "call_1", Name: "lookup", Arguments: json.RawMessage(`{}`)}
	signedCall := call
	signedCall.ThoughtSignature = "sig-call"
	for _, test := range []struct {
		name  string
		event aistudio.Event
		want  []map[string]any
	}{
		{
			name:  "正文",
			event: aistudio.Event{Kind: aistudio.EventText, Text: "answer"},
			want:  []map[string]any{{"text": "answer", "thoughtSignature": "sig-a"}},
		},
		{
			name:  "转写",
			event: aistudio.Event{Kind: aistudio.EventText, Text: "hello", Transcript: &aistudio.TranscriptMetadata{Speaker: "A"}},
			want: []map[string]any{{
				"text": "hello", "transcriptionMetadata": map[string]any{"speaker": "A"}, "thoughtSignature": "sig-a",
			}},
		},
		{
			name:  "工具调用",
			event: aistudio.Event{Kind: aistudio.EventToolCall, ToolCall: &call},
			want: []map[string]any{{
				"functionCall":     map[string]any{"id": "call_1", "name": "lookup", "args": json.RawMessage(`{}`)},
				"thoughtSignature": "sig-a",
			}},
		},
		{
			name:  "自带签名的工具调用",
			event: aistudio.Event{Kind: aistudio.EventToolCall, ToolCall: &signedCall, ThoughtSignature: "sig-call"},
			want: []map[string]any{
				{"text": "", "thought": true, "thoughtSignature": "sig-a"},
				{
					"functionCall":     map[string]any{"id": "call_1", "name": "lookup", "args": json.RawMessage(`{}`)},
					"thoughtSignature": "sig-call",
				},
			},
		},
	} {
		parts := geminiOutputParts(generationResult{events: []aistudio.Event{
			{Kind: aistudio.EventThoughtSignature, ThoughtSignature: "sig-a"},
			test.event,
		}})
		if !reflect.DeepEqual(parts, test.want) {
			t.Fatalf("%s: 前置签名 %+v，期望 %+v", test.name, parts, test.want)
		}
	}
}

// TestGeminiOutputParts_StandaloneThoughtSignatureFallback 没有可附着内容的签名用空思考 part 承载
func TestGeminiOutputParts_StandaloneThoughtSignatureFallback(t *testing.T) {
	parts := geminiOutputParts(generationResult{events: []aistudio.Event{
		{Kind: aistudio.EventThoughtSignature, ThoughtSignature: "sig_only"},
	}})
	want := []map[string]any{{"text": "", "thought": true, "thoughtSignature": "sig_only"}}
	if !reflect.DeepEqual(parts, want) {
		t.Fatalf("独立签名 %+v，期望 %+v", parts, want)
	}
}

// TestGeminiOutputParts_WithToolCall 思考后单独到达的签名留在思考 part 上，工具调用保持独立
func TestGeminiOutputParts_WithToolCall(t *testing.T) {
	call := aistudio.FunctionCall{ID: "call_1", Name: "get_weather", Arguments: json.RawMessage(`{"location":"Tokyo"}`)}
	parts := geminiOutputParts(generationResult{events: []aistudio.Event{
		{Kind: aistudio.EventReasoning, Text: "Need weather for Tokyo."},
		{Kind: aistudio.EventThoughtSignature, ThoughtSignature: "sig_call"},
		{Kind: aistudio.EventToolCall, ToolCall: &call},
	}})
	if len(parts) != 2 {
		t.Fatalf("期望 2 个 part，得到 %d: %+v", len(parts), parts)
	}
	if parts[0]["thought"] != true || parts[0]["text"] != "Need weather for Tokyo." || parts[0]["thoughtSignature"] != "sig_call" {
		t.Fatalf("思考 part: %+v", parts[0])
	}
	if parts[1]["functionCall"] == nil || parts[1]["thoughtSignature"] != nil {
		t.Fatalf("工具调用 part: %+v", parts[1])
	}
}

// TestGeminiOutputParts_SignedBoundaries 相邻两段各带签名时保持两个 part，签名不被覆盖
func TestGeminiOutputParts_SignedBoundaries(t *testing.T) {
	for _, kind := range []aistudio.EventKind{aistudio.EventText, aistudio.EventReasoning} {
		parts := geminiOutputParts(generationResult{events: []aistudio.Event{
			{Kind: kind, Text: "first", ThoughtSignature: "sig-a"},
			{Kind: kind, Text: "second", ThoughtSignature: "sig-b"},
		}})
		if len(parts) != 2 || parts[0]["text"] != "first" || parts[1]["text"] != "second" ||
			parts[0]["thoughtSignature"] != "sig-a" || parts[1]["thoughtSignature"] != "sig-b" {
			t.Fatalf("kind=%s: 签名边界 %+v", kind, parts)
		}
	}
}

// TestGeminiOutputParts_SignedPartMerging 未签名的增量照常并入（含已带签名的 part），
// 已带签名的 part 不再追加带签名的增量
func TestGeminiOutputParts_SignedPartMerging(t *testing.T) {
	parts := geminiOutputParts(generationResult{events: []aistudio.Event{
		{Kind: aistudio.EventText, Text: "a"},
		{Kind: aistudio.EventText, Text: "b", ThoughtSignature: "sig-b"},
		{Kind: aistudio.EventText, Text: "c"},
		{Kind: aistudio.EventText, Text: "d", ThoughtSignature: "sig-d"},
	}})
	want := []map[string]any{
		{"text": "abc", "thoughtSignature": "sig-b"},
		{"text": "d", "thoughtSignature": "sig-d"},
	}
	if !reflect.DeepEqual(parts, want) {
		t.Fatalf("合并结果 %+v，期望 %+v", parts, want)
	}
}

// TestGeminiOutputParts_PendingSignature 前置签名遇到自带签名的 part 时单独成 part，两份签名都保留
func TestGeminiOutputParts_PendingSignature(t *testing.T) {
	parts := geminiOutputParts(generationResult{events: []aistudio.Event{
		{Kind: aistudio.EventThoughtSignature, ThoughtSignature: "sig-a"},
		{Kind: aistudio.EventReasoning, Text: "reason", ThoughtSignature: "sig-b"},
		{Kind: aistudio.EventText, Text: "answer"},
	}})
	want := []map[string]any{
		{"text": "", "thought": true, "thoughtSignature": "sig-a"},
		{"text": "reason", "thought": true, "thoughtSignature": "sig-b"},
		{"text": "answer"},
	}
	if !reflect.DeepEqual(parts, want) {
		t.Fatalf("前置签名 %+v，期望 %+v", parts, want)
	}
}

// TestGeminiOutputParts_TrailingSignatures 前一个 part 已带签名时，尾部签名单独成 part，不覆盖已有签名
func TestGeminiOutputParts_TrailingSignatures(t *testing.T) {
	parts := geminiOutputParts(generationResult{events: []aistudio.Event{
		{Kind: aistudio.EventText, Text: "answer", ThoughtSignature: "sig-a"},
		{Kind: aistudio.EventThoughtSignature, ThoughtSignature: "sig-b"},
	}})
	want := []map[string]any{
		{"text": "answer", "thoughtSignature": "sig-a"},
		{"text": "", "thought": true, "thoughtSignature": "sig-b"},
	}
	if !reflect.DeepEqual(parts, want) {
		t.Fatalf("尾部签名 %+v，期望 %+v", parts, want)
	}
}

// TestGeminiOutputParts_LeadingSignatures 连续多个前置签名按原顺序各成一个 part，不挂到随后的内容上
func TestGeminiOutputParts_LeadingSignatures(t *testing.T) {
	parts := geminiOutputParts(generationResult{events: []aistudio.Event{
		{Kind: aistudio.EventThoughtSignature, ThoughtSignature: "sig-a"},
		{Kind: aistudio.EventThoughtSignature, ThoughtSignature: "sig-b"},
		{Kind: aistudio.EventThoughtSignature, ThoughtSignature: "sig-c"},
		{Kind: aistudio.EventText, Text: "answer"},
	}})
	want := []map[string]any{
		{"text": "", "thought": true, "thoughtSignature": "sig-a"},
		{"text": "", "thought": true, "thoughtSignature": "sig-b"},
		{"text": "", "thought": true, "thoughtSignature": "sig-c"},
		{"text": "answer"},
	}
	if !reflect.DeepEqual(parts, want) {
		t.Fatalf("连续前置签名 %+v，期望 %+v", parts, want)
	}
}

// TestGeminiOutputParts_SignedAudio 带签名的 PCM 音频片段不合并，各自保留签名
func TestGeminiOutputParts_SignedAudio(t *testing.T) {
	var result generationResult
	for _, signature := range []string{"audio-a", "audio-b"} {
		if err := result.apply(aistudio.Event{Kind: aistudio.EventMedia, ThoughtSignature: signature, Media: &aistudio.Media{MIME: "audio/l16;rate=24000", Data: []byte{1, 2}}}); err != nil {
			t.Fatal(err)
		}
	}
	parts := geminiOutputParts(result)
	if len(parts) != 2 || parts[0]["thoughtSignature"] != "audio-a" || parts[1]["thoughtSignature"] != "audio-b" {
		t.Fatalf("带签名的音频 %+v", parts)
	}
}

// TestGenerationResultAudioMerge 只有前后都不带签名的 PCM（audio/l16）片段按到达顺序拼接
func TestGenerationResultAudioMerge(t *testing.T) {
	for _, test := range []struct {
		name       string
		mime       string
		signatures [2]string
		merged     bool
	}{
		{name: "PCM", mime: "audio/l16;rate=24000", merged: true},
		{name: "PCM 大写", mime: "audio/L16;codec=pcm;rate=24000", merged: true},
		{name: "MP3", mime: "audio/mpeg"},
		{name: "WAV", mime: "audio/wav"},
		{name: "前一片带签名", mime: "audio/l16;rate=24000", signatures: [2]string{"sig-a", ""}},
		{name: "后一片带签名", mime: "audio/l16;rate=24000", signatures: [2]string{"", "sig-b"}},
	} {
		var result generationResult
		for index, data := range [][]byte{{1, 2}, {3, 4}} {
			event := aistudio.Event{Kind: aistudio.EventMedia, ThoughtSignature: test.signatures[index], Media: &aistudio.Media{MIME: test.mime, Data: data}}
			if err := result.apply(event); err != nil {
				t.Fatal(err)
			}
		}
		if test.merged {
			if len(result.events) != 1 || len(result.media) != 1 ||
				!reflect.DeepEqual(result.events[0].Media.Data, []byte{1, 2, 3, 4}) || !reflect.DeepEqual(result.media[0].Data, []byte{1, 2, 3, 4}) {
				t.Fatalf("%s: 应合并为一片，事件 %+v 媒体 %+v", test.name, result.events, result.media)
			}
			continue
		}
		if len(result.events) != 2 || len(result.media) != 2 ||
			result.events[0].ThoughtSignature != test.signatures[0] || result.events[1].ThoughtSignature != test.signatures[1] {
			t.Fatalf("%s: 应保持两片，事件 %+v 媒体 %+v", test.name, result.events, result.media)
		}
	}
}

// TestGeminiStreamStandaloneSignature 流式响应中的独立签名以空思考 part 发出（SSE 与 JSON 数组两种写法）
func TestGeminiStreamStandaloneSignature(t *testing.T) {
	events := []aistudio.Event{
		{Kind: aistudio.EventThoughtSignature, ThoughtSignature: "sig-a"},
		{Kind: aistudio.EventText, Text: "answer"},
		{Kind: aistudio.EventFinish, FinishReason: "stop"},
	}
	body := `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`
	for _, query := range []string{"?alt=sse", ""} {
		handler := NewHandler(&scriptedService{events: events}, Config{APIKey: "sk-test"})
		recorder := postJSON(t, handler, "/v1beta/models/test-model:streamGenerateContent"+query, body, nil)
		if recorder.Code != http.StatusOK {
			t.Fatalf("query=%q: status=%d body=%s", query, recorder.Code, recorder.Body.String())
		}
		var chunks []map[string]any
		if query == "" {
			if err := json.Unmarshal(recorder.Body.Bytes(), &chunks); err != nil {
				t.Fatalf("JSON 数组无法解析: %v %s", err, recorder.Body.String())
			}
		} else {
			for _, frame := range strings.Split(recorder.Body.String(), "\n\n") {
				data, ok := strings.CutPrefix(strings.TrimSpace(frame), "data: ")
				if !ok {
					continue
				}
				var chunk map[string]any
				if err := json.Unmarshal([]byte(data), &chunk); err != nil {
					t.Fatalf("SSE 帧无法解析: %v %s", err, data)
				}
				chunks = append(chunks, chunk)
			}
		}
		if len(chunks) == 0 {
			t.Fatalf("query=%q: 没有输出帧: %s", query, recorder.Body.String())
		}
		want := map[string]any{"text": "", "thought": true, "thoughtSignature": "sig-a"}
		if part := geminiChunkPart(chunks[0]); !reflect.DeepEqual(part, want) {
			t.Fatalf("query=%q: 独立签名帧 %+v，期望 %+v", query, part, want)
		}
	}
}

// geminiChunkPart 取出流式帧中第一个 candidate 的第一个 part
func geminiChunkPart(chunk map[string]any) map[string]any {
	candidates, _ := chunk["candidates"].([]any)
	if len(candidates) == 0 {
		return nil
	}
	candidate, _ := candidates[0].(map[string]any)
	content, _ := candidate["content"].(map[string]any)
	parts, _ := content["parts"].([]any)
	if len(parts) == 0 {
		return nil
	}
	part, _ := parts[0].(map[string]any)
	return part
}

// TestGeminiSignaturePartRoundTrip 输出的空思考签名 part 回传时还原为纯签名 Part，正文 part 的签名原样保留
func TestGeminiSignaturePartRoundTrip(t *testing.T) {
	output := geminiOutputParts(generationResult{events: []aistudio.Event{
		{Kind: aistudio.EventText, Text: "answer", ThoughtSignature: "sig-a"},
		{Kind: aistudio.EventThoughtSignature, ThoughtSignature: "sig-b"},
	}})
	raw, err := json.Marshal(output)
	if err != nil {
		t.Fatal(err)
	}
	var input []geminiPart
	if err := json.Unmarshal(raw, &input); err != nil {
		t.Fatal(err)
	}
	parts, hasResult, err := mapGeminiParts(input)
	if err != nil {
		t.Fatal(err)
	}
	want := []aistudio.Part{{Text: "answer", ThoughtSignature: "sig-a"}, {ThoughtSignature: "sig-b"}}
	if hasResult || !reflect.DeepEqual(parts, want) {
		t.Fatalf("回传解析 %+v，期望 %+v", parts, want)
	}
}

// TestGeminiEmptyThoughtSignatureInput 请求历史里只带签名的空思考 part 还原为纯签名 Part 交给上游
func TestGeminiEmptyThoughtSignatureInput(t *testing.T) {
	service := &scriptedService{events: []aistudio.Event{{Kind: aistudio.EventText, Text: "ok"}, {Kind: aistudio.EventFinish, FinishReason: "stop"}}}
	handler := NewHandler(service, Config{APIKey: "sk-test"})
	body := `{"contents":[
		{"role":"user","parts":[{"text":"hi"}]},
		{"role":"model","parts":[{"text":"","thought":true,"thoughtSignature":"sig-a"},{"text":"answer"}]},
		{"role":"user","parts":[{"text":"next"}]}
	]}`
	recorder := postJSON(t, handler, "/v1beta/models/test-model:generateContent", body, nil)
	if recorder.Code != http.StatusOK || service.last == nil {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if len(service.last.Contents) != 3 {
		t.Fatalf("对话轮次 %+v", service.last.Contents)
	}
	want := []aistudio.Part{{ThoughtSignature: "sig-a"}, {Text: "answer"}}
	if got := service.last.Contents[1].Parts; !reflect.DeepEqual(got, want) {
		t.Fatalf("模型轮 parts %+v，期望 %+v", got, want)
	}
}
