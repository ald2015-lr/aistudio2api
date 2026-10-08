package api

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// TestFinishReasonMapping 只有内容拦截类结束原因映射为 content_filter / refusal，其他非标准原因按正常结束
func TestFinishReasonMapping(t *testing.T) {
	for _, test := range []struct {
		reason          string
		tools           bool
		openAI          string
		anthropic       string
		providerVisible bool
	}{
		{reason: "stop", openAI: "stop", anthropic: "end_turn"},
		{reason: "stop", tools: true, openAI: "tool_calls", anthropic: "tool_use"},
		{reason: "max_tokens", openAI: "length", anthropic: "max_tokens"},
		{reason: "safety", openAI: "content_filter", anthropic: "refusal", providerVisible: true},
		{reason: "prohibited_content", tools: true, openAI: "content_filter", anthropic: "refusal", providerVisible: true},
		{reason: "malformed_function_call", openAI: "stop", anthropic: "end_turn", providerVisible: true},
		{reason: "unexpected_tool_call", tools: true, openAI: "tool_calls", anthropic: "tool_use", providerVisible: true},
		{reason: "other", openAI: "stop", anthropic: "end_turn", providerVisible: true},
		{reason: "provider_99", openAI: "stop", anthropic: "end_turn", providerVisible: true},
	} {
		if got := openAIFinishReason(test.reason, test.tools); got != test.openAI {
			t.Fatalf("%s tools=%v: OpenAI %q，期望 %q", test.reason, test.tools, got, test.openAI)
		}
		if got, _ := anthropicStop(test.reason, test.tools, ""); got != test.anthropic {
			t.Fatalf("%s tools=%v: Anthropic %q，期望 %q", test.reason, test.tools, got, test.anthropic)
		}
		if visible := providerFinishReason(test.reason) != ""; visible != test.providerVisible {
			t.Fatalf("%s: provider_finish_reason 可见=%v", test.reason, visible)
		}
	}
}

// TestChatFileAndRemoteImageParts Chat 嵌套的 file 对象、不带前缀的 base64、https 图片地址与 file_url 都能解析
func TestChatFileAndRemoteImageParts(t *testing.T) {
	nested, err := openAIContentPart(json.RawMessage(`{"type":"file","file":{"filename":"a.pdf","file_data":"data:application/pdf;base64,JVBERg=="}}`))
	if err != nil || nested.InlineData == nil || nested.InlineData.MIME != "application/pdf" {
		t.Fatalf("嵌套 file: %+v %v", nested, err)
	}
	raw, err := openAIContentPart(json.RawMessage(`{"type":"file","file":{"filename":"notes.txt","file_data":"aGVsbG8="}}`))
	if err != nil || raw.InlineData == nil || raw.InlineData.MIME != "text/plain" || string(raw.InlineData.Data) != "hello" {
		t.Fatalf("base64 file_data: %+v %v", raw, err)
	}
	byID, err := openAIContentPart(json.RawMessage(`{"type":"file","file":{"file_id":"file-123"}}`))
	if err != nil || byID.File == nil || byID.File.ID != "file-123" {
		t.Fatalf("file_id: %+v %v", byID, err)
	}
	image, err := openAIContentPart(json.RawMessage(`{"type":"image_url","image_url":{"url":"https://example.com/cat.png?x=1"}}`))
	if err != nil || image.ExternalMedia == nil || image.ExternalMedia.MIME != "image/png" || image.File != nil {
		t.Fatalf("https 图片: %+v %v", image, err)
	}
	unknown, _ := openAIContentPart(json.RawMessage(`{"type":"input_image","image_url":"https://example.com/render"}`))
	if unknown.ExternalMedia == nil || unknown.ExternalMedia.MIME != "image/*" {
		t.Fatalf("无扩展名图片: %+v", unknown)
	}
	fileURL, err := openAIContentPart(json.RawMessage(`{"type":"input_file","file_url":"https://example.com/report.pdf"}`))
	if err != nil || fileURL.ExternalMedia == nil || fileURL.ExternalMedia.MIME != "application/pdf" {
		t.Fatalf("file_url: %+v %v", fileURL, err)
	}
	if _, err := openAIContentPart(json.RawMessage(`{"type":"file","file":{}}`)); err == nil {
		t.Fatal("空 file part 应返回明确错误")
	}
}

// TestResponsesReplayOwnOutputItems 无状态客户端把上一轮输出（含内置工具调用项）放回 input 时不再返回 400
func TestResponsesReplayOwnOutputItems(t *testing.T) {
	contents, _, err := responsesContents(json.RawMessage(`[
		{"type":"message","role":"user","content":"search it"},
		{"type":"web_search_call","id":"ws_1","status":"completed","action":{"type":"search","query":"go"}},
		{"type":"code_interpreter_call","id":"ci_1","code":"print(1)"},
		{"type":"image_generation_call","id":"ig_1","result":"aGk="},
		{"type":"item_reference","id":"msg_1"},
		{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]},
		{"type":"message","role":"user","content":"thanks"}
	]`))
	if err != nil || len(contents) != 3 {
		t.Fatalf("contents=%+v err=%v", contents, err)
	}
}

// TestGeminiThinkingConfig thinkingBudget=-1 按动态思考（未设置）处理；includeThoughts=false 隐藏思考正文
func TestGeminiThinkingConfig(t *testing.T) {
	var request geminiRequest
	decodeInto(t, `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"thinkingConfig":{"thinkingBudget":-1,"includeThoughts":false}}}`, &request)
	generate, err := request.toGenerateRequest("id", "gemini-x")
	if err != nil || generate.Config.ThinkingBudget != nil || !generate.Config.HideThinking {
		t.Fatalf("config=%+v err=%v", generate.Config, err)
	}
	var budgeted geminiRequest
	decodeInto(t, `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"thinkingConfig":{"thinkingBudget":512}}}`, &budgeted)
	generate, err = budgeted.toGenerateRequest("id", "gemini-x")
	if err != nil || generate.Config.ThinkingBudget == nil || *generate.Config.ThinkingBudget != 512 || generate.Config.HideThinking {
		t.Fatalf("config=%+v err=%v", generate.Config, err)
	}
}

// TestResponsesSearchHoldIsBounded 声明 web_search 时正文最多暂存 300 字节或 1 秒，之后照常流式输出
func TestResponsesSearchHoldIsBounded(t *testing.T) {
	recorder := httptest.NewRecorder()
	writer := &responsesStreamWriter{w: recorder, id: "resp_1", indexes: make(map[string]int), searchProbe: true}
	if err := writer.live(aistudio.Event{Kind: aistudio.EventText, Text: "short"}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(recorder.Body.String(), "output_text.delta") {
		t.Fatal("短正文应先暂存")
	}
	if err := writer.live(aistudio.Event{Kind: aistudio.EventText, Text: strings.Repeat("x", responsesSearchHoldBytes)}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(recorder.Body.String(), "output_text.delta") || writer.searchProbe {
		t.Fatal("超过暂存上限后应开始流式输出")
	}
}

// TestToolResultDocumentsBecomeAttachments 工具结果中的 PDF、MCP blob 与音频作为附件发送，不再按 base64 文本发送
func TestToolResultDocumentsBecomeAttachments(t *testing.T) {
	raw := json.RawMessage(`[
		{"type":"text","text":"read ok"},
		{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"JVBERg=="}},
		{"type":"resource","resource":{"uri":"file:///a.csv","mimeType":"text/csv","blob":"YSxi"}},
		{"type":"audio","data":"UklGRg==","mimeType":"audio/wav"},
		{"type":"document","source":{"type":"text","media_type":"text/plain","data":"plain"}}
	]`)
	cleaned, media, err := splitFunctionResultMedia(raw)
	if err != nil || len(media) != 3 {
		t.Fatalf("media=%d err=%v", len(media), err)
	}
	if media[0].InlineData.MIME != "application/pdf" || media[1].InlineData.MIME != "text/csv" || media[2].InlineData.MIME != "audio/wav" {
		t.Fatalf("类型: %s %s %s", media[0].InlineData.MIME, media[1].InlineData.MIME, media[2].InlineData.MIME)
	}
	if strings.Contains(string(cleaned), "JVBERg") || !strings.Contains(string(cleaned), "文件 1") || !strings.Contains(string(cleaned), `"plain"`) {
		t.Fatalf("cleaned=%s", cleaned)
	}
}
