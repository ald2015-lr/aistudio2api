package aistudio

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// markFrame 给流帧（JSON 原文）补上帧索引 4 的生成标记 marker（JSON 原文），中间缺的位置补 null
func markFrame(frame string, marker string) string {
	var values []json.RawMessage
	if err := json.Unmarshal([]byte(frame), &values); err != nil {
		panic(err)
	}
	for len(values) < 5 {
		values = append(values, json.RawMessage("null"))
	}
	values[4] = traw(marker)
	encoded, _ := json.Marshal(values)
	return string(encoded)
}

// interleavedHead 为交错生成的流首帧：frame 0 为 null，索引 5 为 [id, 0]
func interleavedHead(id string) string {
	return `[null,null,null,null,null,["` + id + `",0]]`
}

// interleavedTail 为某条生成的流尾帧：frame 0 为 null，索引 4 为生成标记
func interleavedTail(id string) string {
	return markFrame(`[null]`, `["`+id+`",99]`)
}

// TestInterleavedGenerationsKeepFirst 上游在同一个 GenerateContent 流里交错发出两条生成（内容帧索引 4 为 [id, 序号]）：只保留第一条出现
// 内容帧的生成，正文、函数调用、用量与结束原因各只有一份，结束之后没有事件；另一条生成的帧整帧丢弃，容错日志只写一行。
// 原先两条都解码：正文成了“Hello Hello worldworld”，函数调用重复，结束之后还有正文，用量被后一条覆盖
func TestInterleavedGenerationsKeepFirst(t *testing.T) {
	log := captureToleranceLog(t)
	body := longStream(
		interleavedHead("genA"),
		markFrame(longTextFrame("Hello ", false), `["genA",1]`),
		markFrame(longTextFrame("Hello ", false), `["genB",1]`),
		markFrame(longTextFrame("world", false), `["genA",2]`),
		markFrame(longTextFrame("world", false), `["genB",2]`),
		markFrame(longToolFrame("lookup"), `["genA",3]`),
		markFrame(longToolFrame("lookup"), `["genB",3]`),
		markFrame(longFinishFrame(1, "", 10, 5, 15), `["genA",4]`),
		markFrame(longFinishFrame(1, "", 20, 7, 27), `["genB",4]`),
		interleavedTail("genA"),
		interleavedTail("genB"),
	)
	events, err := decodeTestStream(body, nil)
	if err != nil {
		t.Fatal(err)
	}
	summary := summarizeLong(events)
	if summary.text != "Hello world" || summary.tools != 1 || len(summary.finishes) != 1 || len(summary.usages) != 1 {
		t.Fatalf("汇总 = %+v，期望只保留第一条生成", summary)
	}
	if usage := summary.usages[0]; usage.InputTokens != 10 || usage.OutputTokens != 5 || usage.TotalTokens != 15 {
		t.Fatalf("用量 = %+v，期望第一条生成的 10/5/15", usage)
	}
	if kinds := eventKinds(events); kinds[len(kinds)-1] != EventFinish {
		t.Fatalf("结束之后还有事件：%v", kinds)
	}
	line := log.expectOne(t, "交错多条生成")
	if line["path"] != "$[0][][4]" || !strings.Contains(line["raw"].(string), "genB") {
		t.Fatalf("容错日志 = %v", line)
	}

	// 第二条生成的内容帧先到时保留第二条：以第一条出现内容帧的生成为准，不看流首帧索引 5
	body = longStream(
		interleavedHead("genA"),
		markFrame(longTextFrame("B 的正文", false), `["genB",1]`),
		markFrame(longTextFrame("A 的正文", false), `["genA",1]`),
		markFrame(longFinishFrame(1, "", 20, 7, 27), `["genB",2]`),
		markFrame(longFinishFrame(1, "", 10, 5, 15), `["genA",2]`),
	)
	events, err = decodeTestStream(body, nil)
	if err != nil {
		t.Fatal(err)
	}
	if summary := summarizeLong(events); summary.text != "B 的正文" || len(summary.finishes) != 1 || summary.usages[0].TotalTokens != 27 {
		t.Fatalf("汇总 = %+v，期望保留先出现内容帧的 genB", summary)
	}

	// 另一条生成的流尾帧（frame 0 为 null）带输入拦截也整帧丢弃，不会让保留的那条生成按拦截结束
	body = longStream(
		markFrame(longTextFrame("正文", false), `["genA",1]`),
		markFrame(`[null,[1]]`, `["genB",1]`),
		markFrame(longFinishFrame(1, "", 10, 5, 15), `["genA",2]`),
	)
	events, err = decodeTestStream(body, nil)
	if err != nil {
		t.Fatalf("另一条生成的拦截帧不应影响保留的生成：%v", err)
	}
	if summary := summarizeLong(events); summary.text != "正文" || len(summary.finishes) != 1 || summary.finishes[0] != "stop" {
		t.Fatalf("汇总 = %+v", summary)
	}
}

// TestInterleavedGenerationsEmptyCandidatesDoNotLock 另一条生成只带用量的空候选帧（[[],null,[用量],null,[id, 序号]]）先到时不锁定：
// 按随后第一条出现候选帧的生成保留，用量以保留那条为准。上游 bd7f202 遇到空候选帧也锁定，这时真正出内容的那条被整条丢弃，流里没有完成帧
func TestInterleavedGenerationsEmptyCandidatesDoNotLock(t *testing.T) {
	body := longStream(
		interleavedHead("genA"),
		markFrame(`[[],null,[5,1,6]]`, `["genB",1]`),
		markFrame(longTextFrame("A 的正文", false), `["genA",1]`),
		markFrame(longTextFrame("B 的正文", false), `["genB",2]`),
		markFrame(longFinishFrame(1, "", 10, 5, 15), `["genA",2]`),
		markFrame(longFinishFrame(1, "", 20, 7, 27), `["genB",3]`),
	)
	events, err := decodeTestStream(body, nil)
	if err != nil {
		t.Fatalf("另一条生成的空候选帧先到时不应丢掉出内容的生成：%v", err)
	}
	summary := summarizeLong(events)
	if summary.text != "A 的正文" || len(summary.finishes) != 1 || summary.finishes[0] != "stop" || len(summary.usages) != 1 {
		t.Fatalf("汇总 = %+v，期望保留先出现候选帧的 genA", summary)
	}
	if usage := summary.usages[0]; usage.InputTokens != 10 || usage.OutputTokens != 5 || usage.TotalTokens != 15 {
		t.Fatalf("用量 = %+v，期望 genA 的 10/5/15", usage)
	}
}

// TestInterleavedGenerationsMarkerShapes 没有生成标记的流与原先完全相同；帧索引 4 只有形如 [字符串或数字, 数字] 时才按生成标记过滤，
// 其他形状（项数不是 2、id 为 null 或布尔或数组、序号不是数字、不是数组）不过滤，两条都照常解码；数字 id 同样认
func TestInterleavedGenerationsMarkerShapes(t *testing.T) {
	log := captureToleranceLog(t)
	plain := longStream(longTextFrame("Hello ", false), longTextFrame("world", false), longFinishFrame(1, "", 10, 5, 15))
	events, err := decodeTestStream(plain, nil)
	if err != nil {
		t.Fatal(err)
	}
	if summary := summarizeLong(events); summary.text != "Hello world" || len(summary.finishes) != 1 || len(summary.usages) != 1 {
		t.Fatalf("没有生成标记的流：%+v", summary)
	}
	for _, shapes := range [][2]string{
		{`["genA"]`, `["genB"]`},
		{`["genA",1,2]`, `["genB",1,2]`},
		{`[null,1]`, `[true,1]`},
		{`["genA","1"]`, `["genB","1"]`},
		{`[["genA"],1]`, `[["genB"],1]`},
		{`{"id":"genA"}`, `{"id":"genB"}`},
		{`"genA"`, `"genB"`},
		{`7`, `8`},
		{`[]`, `[]`},
	} {
		body := longStream(
			markFrame(longTextFrame("one ", false), shapes[0]),
			markFrame(longTextFrame("two", false), shapes[1]),
			markFrame(longFinishFrame(1, "", 10, 5, 15), shapes[0]),
		)
		events, err := decodeTestStream(body, nil)
		if err != nil {
			t.Fatalf("%v：%v", shapes, err)
		}
		if summary := summarizeLong(events); summary.text != "one two" || len(summary.finishes) != 1 {
			t.Fatalf("索引 4 为 %v 时不应过滤：%+v", shapes, summary)
		}
	}
	if lines := log.lines("交错多条生成"); len(lines) != 0 {
		t.Fatalf("不应写交错生成的容错日志：%v", lines)
	}
	body := longStream(
		markFrame(longTextFrame("one ", false), `[7, 1]`),
		markFrame(longTextFrame("two", false), `[8, 1]`),
		markFrame(longFinishFrame(1, "", 10, 5, 15), `[7,2]`),
	)
	events, err = decodeTestStream(body, nil)
	if err != nil {
		t.Fatal(err)
	}
	if summary := summarizeLong(events); summary.text != "one " || len(summary.finishes) != 1 {
		t.Fatalf("数字 id 应按生成标记过滤：%+v", summary)
	}
	for marker, want := range map[string]string{
		`["genA",1]`: `"genA"`, `[ 7 , 1.5 ]`: `7`, `["",0]`: `""`, `[-1,2]`: `-1`,
		`["genA",null]`: ``, `[1.5e,1]`: ``, `["genA",-]`: ``, `null`: ``,
	} {
		var frame []json.RawMessage
		_ = json.Unmarshal([]byte(`[null,null,null,null,null]`), &frame)
		frame[4] = json.RawMessage(marker)
		if got := generationMarker(frame); got != want {
			t.Fatalf("generationMarker(%s) = %q，期望 %q", marker, got, want)
		}
	}
}

// TestInterleavedGenerationsPerContinuationRound long- 自动续写每一轮新建解码器：各轮的生成标记互不影响（第二轮是新的一次 GenerateContent，
// 生成 id 与第一轮不同，不能按第一轮的 id 丢掉），每一轮里交错的另一条生成照样丢弃
func TestInterleavedGenerationsPerContinuationRound(t *testing.T) {
	captureToleranceLog(t)
	model, defaults := longArgonModel()
	upstream := &longUpstream{streams: []string{
		longStream(
			markFrame(longTextFrame("第一段", false), `["round1-a",1]`),
			markFrame(longTextFrame("第一段", false), `["round1-b",1]`),
			markFrame(longFinishFrame(22, "tok-1", 100, 32000, 32100), `["round1-a",2]`),
			markFrame(longFinishFrame(22, "tok-1b", 100, 32000, 32100), `["round1-b",2]`),
		),
		longStream(
			markFrame(longTextFrame("第二段", false), `["round2-a",1]`),
			markFrame(longTextFrame("第二段", false), `["round2-b",1]`),
			markFrame(longFinishFrame(1, "", 100, 100, 200), `["round2-a",2]`),
			markFrame(longFinishFrame(1, "", 100, 100, 200), `["round2-b",2]`),
		),
	}}
	summary, calls, _ := runLong(t, longClient(t, upstream, model, defaults), upstream, context.Background(), longRequest(model.ID, true))
	if len(calls) != 2 || summary.text != "第一段第二段" || len(summary.finishes) != 1 || summary.finishes[0] != "stop" || len(summary.errs) != 0 {
		t.Fatalf("调用 = %d，汇总 = %+v，期望两轮各保留第一条生成", len(calls), summary)
	}
	if calls[1].body[16] != "tok-1" {
		t.Fatalf("第二轮的续写 token = %v，期望第一条生成的 tok-1", calls[1].body[16])
	}
}
