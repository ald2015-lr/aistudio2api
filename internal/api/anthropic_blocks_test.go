package api

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// BenchmarkAnthropicBlocks 非流式 Messages 响应按事件拼内容块：原先文本块与思考块每个事件 += 一次，总耗时与长度平方成正比
func BenchmarkAnthropicBlocks(b *testing.B) {
	chunk := strings.Repeat("字", 10)
	var result generationResult
	for range 2000 {
		result.events = append(result.events, aistudio.Event{Kind: aistudio.EventReasoning, Text: chunk})
	}
	for range 20000 {
		result.events = append(result.events, aistudio.Event{Kind: aistudio.EventText, Text: chunk})
	}
	b.ReportAllocs()
	for b.Loop() {
		blocks := anthropicBlocks(result, mediaOutput{})
		if len(blocks) != 2 {
			b.Fatalf("blocks = %d", len(blocks))
		}
	}
}

// TestAnthropicBlocksAccumulate 文本块与思考块按缓冲累加，结果与逐个拼接相同：思考增量合并成一块并带最后的签名，正文、代码执行合并进同一个文本块，
// 工具调用之后另起文本块，引用来源接在最后的文本块后面
func TestAnthropicBlocksAccumulate(t *testing.T) {
	events := []aistudio.Event{
		{Kind: aistudio.EventReasoning, Text: "想"}, {Kind: aistudio.EventReasoning, Text: "一想"},
		{Kind: aistudio.EventReasoning, Text: "。", ThoughtSignature: "sig-fake"},
		{Kind: aistudio.EventText, Text: "你"}, {Kind: aistudio.EventText, Text: "好"},
		{Kind: aistudio.EventExecutableCode, ExecutableCode: &aistudio.ExecutableCode{Language: "PYTHON", Code: "print(1)"}},
		{Kind: aistudio.EventText, Text: "完"},
		{Kind: aistudio.EventToolCall, ToolCall: &aistudio.FunctionCall{ID: "toolu_1", Name: "lookup", Arguments: []byte(`{}`)}},
		{Kind: aistudio.EventText, Text: "之"}, {Kind: aistudio.EventText, Text: "后"},
	}
	code := renderCodeExecution(events[5])
	blocks := anthropicBlocks(generationResult{events: events, citations: []aistudio.Citation{{URL: "https://docs.example.com/a", Title: "A"}}}, mediaOutput{})
	sources := renderCitationsMarkdown([]aistudio.Citation{{URL: "https://docs.example.com/a", Title: "A"}})
	if len(blocks) != 4 || blocks[0].Type != "thinking" || *blocks[0].Thinking != "想一想。" || blocks[0].Signature != "sig-fake" ||
		blocks[1].Type != "text" || blocks[1].Text != "你好\n"+code+"\n完" || blocks[2].Type != "tool_use" ||
		blocks[3].Type != "text" || blocks[3].Text != "之后\n\n"+sources {
		encoded, _ := json.Marshal(blocks)
		t.Fatalf("blocks = %s", encoded)
	}
}
