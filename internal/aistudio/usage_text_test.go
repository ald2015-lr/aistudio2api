package aistudio

import (
	"strings"
	"testing"
)

// benchmarkTextEvents 为一段长回复的流式事件：events 个正文增量，每个 size 字节，中间夹思考增量
func benchmarkTextEvents(events, size int) []Event {
	chunk := strings.Repeat("字", size/3)
	list := make([]Event, 0, events)
	for index := range events {
		kind := EventText
		if index%10 == 0 {
			kind = EventReasoning
		}
		list = append(list, Event{Kind: kind, Text: chunk})
	}
	return list
}

// BenchmarkGeneratedOutputObserve 长回复逐块累计本地输出（用量估算用）：原先最后一个正文 part 每块 += 一次，总耗时与长度平方成正比
func BenchmarkGeneratedOutputObserve(b *testing.B) {
	events := benchmarkTextEvents(20000, 30)
	b.ReportAllocs()
	for b.Loop() {
		var output generatedOutputParts
		for _, event := range events {
			output.observe(event)
		}
		if len(output.visible) != 1 || len(output.reasoning) != 1 {
			b.Fatalf("parts = %d/%d", len(output.visible), len(output.reasoning))
		}
	}
}

// naiveOutputText 为原先逐块拼接的写法，供对照
func naiveOutputText(parts *[]Part, text string, thought bool, signature string) {
	if text == "" {
		return
	}
	if len(*parts) > 0 {
		last := &(*parts)[len(*parts)-1]
		if last.Text != "" && last.Thought == thought && last.ThoughtSignature == signature {
			last.Text += text
			return
		}
	}
	*parts = append(*parts, Part{Text: text, Thought: thought, ThoughtSignature: signature})
}

// TestGeneratedOutputTextMatchesConcat 累加缓冲得到的 parts 与原先逐块拼接完全相同：正文与思考交错、签名变化、中间夹函数调用与媒体、
// 重新生成时清空；结构体按值复制后两方各自继续累加时互不混入
func TestGeneratedOutputTextMatchesConcat(t *testing.T) {
	call := &FunctionCall{ID: "call-1", Name: "lookup", Arguments: []byte(`{"q":1}`)}
	events := []Event{
		{Kind: EventReasoning, Text: "想"}, {Kind: EventReasoning, Text: "一想"}, {Kind: EventReasoning, Text: "。", ThoughtSignature: "sig-a"},
		{Kind: EventText, Text: "你"}, {Kind: EventText, Text: ""}, {Kind: EventText, Text: "好"}, {Kind: EventText, Text: "，", ThoughtSignature: "sig-b"},
		{Kind: EventText, Text: "世界", ThoughtSignature: "sig-b"}, {Kind: EventToolCall, ToolCall: call}, {Kind: EventText, Text: "之后"},
		{Kind: EventMedia, Media: &Media{MIME: "image/png", Data: []byte{1, 2}}}, {Kind: EventText, Text: "再"}, {Kind: EventText, Text: "说"},
		{Kind: EventReasoning, Text: "又想"}, {Kind: EventReasoning, Text: "了"},
	}
	var output generatedOutputParts
	var visible, reasoning []Part
	for _, event := range events {
		output.observe(event)
		switch event.Kind {
		case EventText:
			naiveOutputText(&visible, event.Text, false, event.ThoughtSignature)
		case EventReasoning:
			naiveOutputText(&reasoning, event.Text, true, event.ThoughtSignature)
		case EventToolCall:
			copied := *event.ToolCall
			visible = append(visible, Part{FunctionCall: &copied})
		case EventMedia:
			visible = append(visible, Part{InlineData: &Blob{MIME: event.Media.MIME, Data: event.Media.Data}})
		}
	}
	if describe(output.visible) != describe(visible) || describe(output.reasoning) != describe(reasoning) {
		t.Fatalf("visible=%s\nwant=%s\nreasoning=%s\nwant=%s", describe(output.visible), describe(visible), describe(output.reasoning), describe(reasoning))
	}
	if localPartsTokens(output.visible) != localPartsTokens(visible) {
		t.Fatal("本地 token 估算应相同")
	}

	// 按值复制后各自累加：parts 与原先一样共用底层数组（最后一个 part 的 Text 以最后写入的一方为准），缓冲不把另一方的文字带进来
	var left generatedOutputParts
	left.observe(Event{Kind: EventText, Text: "ab"})
	left.observe(Event{Kind: EventText, Text: "c"})
	right := left
	right.visible = append([]Part(nil), left.visible...)
	right.observe(Event{Kind: EventText, Text: "XYZ"})
	left.observe(Event{Kind: EventText, Text: "d"})
	if left.visible[0].Text != "abcd" || right.visible[0].Text != "abcXYZ" {
		t.Fatalf("复制后各自累加: left=%q right=%q", left.visible[0].Text, right.visible[0].Text)
	}
	right.observe(Event{Kind: EventText, Text: "!"})
	if right.visible[0].Text != "abcXYZ!" || left.visible[0].Text != "abcd" {
		t.Fatalf("复制后各自累加: left=%q right=%q", left.visible[0].Text, right.visible[0].Text)
	}
	// 新起的 part 与上一个 part 等长时不能沿用上一个 part 的缓冲
	var equal generatedOutputParts
	equal.observe(Event{Kind: EventText, Text: "a", ThoughtSignature: "sig-a"})
	equal.observe(Event{Kind: EventText, Text: "b", ThoughtSignature: "sig-a"})
	equal.observe(Event{Kind: EventText, Text: "cd", ThoughtSignature: "sig-b"})
	equal.observe(Event{Kind: EventText, Text: "e", ThoughtSignature: "sig-b"})
	if len(equal.visible) != 2 || equal.visible[0].Text != "ab" || equal.visible[1].Text != "cde" {
		t.Fatalf("等长的新 part: %s", describe(equal.visible))
	}
	left = generatedOutputParts{}
	left.observe(Event{Kind: EventText, Text: "新"})
	if len(left.visible) != 1 || left.visible[0].Text != "新" {
		t.Fatalf("清空后 = %+v", left.visible)
	}
}

func describe(parts []Part) string {
	var builder strings.Builder
	for _, part := range parts {
		switch {
		case part.FunctionCall != nil:
			builder.WriteString("[call " + part.FunctionCall.Name + "]")
		case part.InlineData != nil:
			builder.WriteString("[media " + part.InlineData.MIME + "]")
		default:
			builder.WriteString("[" + part.Text + "|" + part.ThoughtSignature + "]")
		}
	}
	return builder.String()
}
