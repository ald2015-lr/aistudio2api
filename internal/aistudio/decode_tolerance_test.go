package aistudio

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

// 解码容错的测试：每一项都是“原先按协议证据错误中止、现在继续”的形状（去掉容错时解码返回错误，断言失败），另有正文读不出时仍报错的用例

// toleranceLog 截获运行日志里的解码容错行
type toleranceLog struct {
	buffer bytes.Buffer
}

// captureToleranceLog 把默认 slog 换成写进缓冲区的 JSON 日志，测试结束时换回
func captureToleranceLog(t *testing.T) *toleranceLog {
	t.Helper()
	log := &toleranceLog{}
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&log.buffer, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return log
}

// lines 返回解码容错的日志行（种类包含 kind 的；kind 为空时全部）
func (log *toleranceLog) lines(kind string) []map[string]any {
	var result []map[string]any
	for _, line := range strings.Split(log.buffer.String(), "\n") {
		var entry map[string]any
		if json.Unmarshal([]byte(line), &entry) != nil || entry["msg"] != "上游响应解码容错，已继续" {
			continue
		}
		if value, _ := entry["kind"].(string); kind == "" || strings.Contains(value, kind) {
			result = append(result, entry)
		}
	}
	return result
}

// expectOne 断言种类包含 kind 的容错日志恰好一行，返回这一行
func (log *toleranceLog) expectOne(t *testing.T, kind string) map[string]any {
	t.Helper()
	lines := log.lines(kind)
	if len(lines) != 1 {
		t.Fatalf("容错日志“%s”有 %d 行，期望 1 行；全部日志：\n%s", kind, len(lines), log.buffer.String())
	}
	return lines[0]
}

// tpart 按 JSON 索引组装一个数组（Part、候选等），没有给出的位置为 null
func tpart(fields map[int]any) []any {
	size := 0
	for index := range fields {
		size = max(size, index+1)
	}
	values := make([]any, size)
	for index, value := range fields {
		values[index] = value
	}
	return values
}

// ttext 为一个正文 Part
func ttext(text string) []any { return tpart(map[int]any{1: text}) }

// tcandidate 组装候选：content 为 [parts, "model"]，finish 不为 nil 时写进 field 2，extra 为其他字段
func tcandidate(parts []any, finish any, extra map[int]any) []any {
	fields := map[int]any{0: []any{parts, "model"}}
	if finish != nil {
		fields[1] = finish
	}
	for index, value := range extra {
		fields[index] = value
	}
	return tpart(fields)
}

// tframe 组装只有一个候选的帧，usage 不为 nil 时写进帧 field 3
func tframe(candidate []any, usage any) []any {
	frame := []any{[]any{candidate}}
	if usage != nil {
		frame = append(frame, nil, usage)
	}
	return frame
}

// tstream 把帧拼成 GenerateContent 响应，trailer 非空时作为流尾
func tstream(trailer string, frames ...any) string {
	encoded, _ := json.Marshal(frames)
	if trailer == "" {
		return "[" + string(encoded) + "]"
	}
	return "[" + string(encoded) + "," + trailer + "]"
}

// traw 为一段 JSON 原文，组装帧时原样嵌入（保留 1.0 这类写法）
func traw(text string) json.RawMessage {
	if !json.Valid([]byte(text)) {
		panic("不是合法 JSON: " + text)
	}
	return json.RawMessage(text)
}

// decodeTestStream 按 sendPlayground 的顺序解码整个响应（含完成帧校验），容错记录用 tolerance
func decodeTestStream(body string, tolerance *decodeTolerance) ([]Event, error) {
	decoder := NewFrameDecoder()
	decoder.tolerance = tolerance
	var events []Event
	err := DecodeGenerateStream(strings.NewReader(body), decoder, func(event Event) error {
		events = append(events, event)
		return nil
	})
	if err == nil {
		err = decoder.End()
	}
	return events, err
}

func eventsOfKind(events []Event, kind EventKind) []Event {
	var result []Event
	for _, event := range events {
		if event.Kind == kind {
			result = append(result, event)
		}
	}
	return result
}

func joinedText(events []Event) string {
	var text strings.Builder
	for _, event := range eventsOfKind(events, EventText) {
		text.WriteString(event.Text)
	}
	return text.String()
}

// TestToleranceCitations 引用条目缺少 URI（只有起止位置的 [[[0,10]]]）、URI 为 null 或空串时跳过该条目，标题不是字符串时忽略标题，
// 引用元数据整段不是数组时丢掉；正文与其他引用照常，同一种容错只写一行
func TestToleranceCitations(t *testing.T) {
	log := captureToleranceLog(t)
	citations := traw(`[[[0,10],[0,4,null,"mit"],[0,3,"https://a.example","A"],[1,2,""],[0,1,"https://b.example",7]]]`)
	body := tstream("", tframe(tcandidate([]any{ttext("cited")}, 1, map[int]any{6: citations}), nil))
	events, err := decodeTestStream(body, newDecodeTolerance(nil, "req-cite"))
	if err != nil {
		t.Fatalf("缺少 URI 的引用不应中止：%v", err)
	}
	got := eventsOfKind(events, EventCitation)
	want := []Citation{{URL: "https://a.example", Title: "A"}, {URL: "https://b.example"}}
	if len(got) != len(want) || *got[0].Citation != want[0] || *got[1].Citation != want[1] || joinedText(events) != "cited" {
		t.Fatalf("引用 = %+v，正文 = %q", got, joinedText(events))
	}
	if events[len(events)-1].Kind != EventFinish {
		t.Fatalf("最后一个事件 = %v，期望结束事件", events[len(events)-1].Kind)
	}
	line := log.expectOne(t, "引用条目缺少 URI")
	if line["request_id"] != "req-cite" || line["method"] != "GenerateContent" || line["path"] != "$[0][][0][0][6][0][0][2]" || line["raw"] != "[0,10]" {
		t.Fatalf("容错日志 = %v", line)
	}
	log.expectOne(t, "引用标题不是字符串")

	// 整段不是数组：丢掉引用，正文照常
	events, err = decodeTestStream(tstream("", tframe(tcandidate([]any{ttext("x")}, 1, map[int]any{6: "broken"}), nil)), nil)
	if err != nil || len(eventsOfKind(events, EventCitation)) != 0 || joinedText(events) != "x" {
		t.Fatalf("引用元数据不是数组：事件 = %v，错误 = %v", eventKinds(events), err)
	}
}

// TestToleranceGrounding grounding chunk：下标 1 为图片来源（与上游 bd6c40f 一致），未知来源（下标 3 及以上）、空 chunk、不是数组的
// chunk 换成占位项保持索引对齐，多个来源取第一个，非字符串字段忽略；support 的 chunk 索引为 null 时跳过该索引（同位置的置信度一并去掉）；
// 整段 grounding 不是数组时只写日志、不发这个事件
func TestToleranceGrounding(t *testing.T) {
	log := captureToleranceLog(t)
	grounding := traw(`[
		null,
		[
			[["https://w.example", 5, "snippet"]],
			[null, ["https://page.example/cat", "page.example"]],
			[null, null, ["https://maps.example", "Shop", "", "PLACE"]],
			[null, null, null, ["https://new.example"]],
			[],
			[["https://first.example", "First"], null, ["https://second.example"]],
			"broken"
		],
		[[[0, 0, 4, "text"], [0, null, 5, 1.0], [0.9, 0.8, 0.7, 0.6]], [null, [null]], "bad"],
		null,
		["q1", 5, "q2"]
	]`)
	body := tstream("", tframe(tcandidate([]any{ttext("searched")}, 1, map[int]any{7: grounding}), nil))
	events, err := decodeTestStream(body, nil)
	if err != nil {
		t.Fatalf("grounding 不应中止：%v", err)
	}
	found := eventsOfKind(events, EventGrounding)
	if len(found) != 1 || joinedText(events) != "searched" {
		t.Fatalf("事件 = %v", eventKinds(events))
	}
	metadata := found[0].Grounding
	wantChunks := []GroundingChunk{
		{Source: "web", URI: "https://w.example", Text: "snippet"},
		{Source: "image", URI: "https://page.example/cat", Domain: "page.example"},
		{Source: "maps", URI: "https://maps.example", Title: "Shop", PlaceID: "PLACE"},
		{Source: "unknown"}, {Source: "unknown"},
		{Source: "web", URI: "https://first.example", Title: "First"},
		{Source: "unknown"},
	}
	if !reflect.DeepEqual(metadata.Chunks, wantChunks) {
		t.Fatalf("chunks = %+v", metadata.Chunks)
	}
	if len(metadata.Supports) != 2 {
		t.Fatalf("supports = %+v，期望跳过不是数组的那一项", metadata.Supports)
	}
	first := metadata.Supports[0]
	if !reflect.DeepEqual(first.ChunkIndices, []int{0, 5, 1}) || !reflect.DeepEqual(first.ConfidenceScores, []float64{0.9, 0.7, 0.6}) ||
		first.Segment != (GroundingSegment{EndIndex: 4, Text: "text"}) {
		t.Fatalf("第一个 support = %+v", first)
	}
	if second := metadata.Supports[1]; len(second.ChunkIndices) != 0 {
		t.Fatalf("第二个 support = %+v，期望 null 索引被跳过", second)
	}
	if !reflect.DeepEqual(metadata.WebSearchQueries, []string{"q1", "q2"}) {
		t.Fatalf("查询词 = %v", metadata.WebSearchQueries)
	}
	for _, kind := range []string{
		"来源类型未知", "grounding chunk 为空", "多个来源", "grounding chunk 不是数组", "字段不是字符串",
		"chunk 索引为 null", "grounding support 不是数组", "查询词不是字符串",
	} {
		log.expectOne(t, kind)
	}

	// 整段不是数组：不发 grounding 事件，正文与结束照常
	events, err = decodeTestStream(tstream("", tframe(tcandidate([]any{ttext("x")}, 1, map[int]any{7: 42}), nil)), nil)
	if err != nil || len(eventsOfKind(events, EventGrounding)) != 0 || joinedText(events) != "x" || events[len(events)-1].Kind != EventFinish {
		t.Fatalf("grounding 不是数组：事件 = %v，错误 = %v", eventKinds(events), err)
	}
	log.expectOne(t, "已丢弃这次 grounding")
}

// TestToleranceCodeExecution 代码执行 part：language/outcome 为 null、缺位或不在表中时取缺省值或保留为 *_<n>，code 缺位按空串，
// outcome 不确定时文本同时放进 Output 与 Error（各协议按 outcome 取其一，文本不丢）；表内整数的结果与原先相同
func TestToleranceCodeExecution(t *testing.T) {
	log := captureToleranceLog(t)
	parts := []any{
		tpart(map[int]any{7: traw(`[null,"print(1)"]`)}),
		tpart(map[int]any{7: traw(`[2,"x = 1"]`)}),
		tpart(map[int]any{7: traw(`[1]`)}),
		tpart(map[int]any{8: traw(`[]`)}),
		tpart(map[int]any{8: traw(`[null,"out"]`)}),
		tpart(map[int]any{8: traw(`[4,"later"]`)}),
		tpart(map[int]any{8: traw(`["OUTCOME_OK","named"]`)}),
		tpart(map[int]any{8: traw(`[1,"ok"]`)}),
		tpart(map[int]any{8: traw(`[2,"failed"]`)}),
	}
	events, err := decodeTestStream(tstream("", tframe(tcandidate(append(parts, ttext("done")), 1, nil), nil)), nil)
	if err != nil {
		t.Fatalf("代码执行的枚举不应中止：%v", err)
	}
	codes := eventsOfKind(events, EventExecutableCode)
	wantCodes := []ExecutableCode{{"LANGUAGE_UNSPECIFIED", "print(1)"}, {"LANGUAGE_2", "x = 1"}, {"PYTHON", ""}}
	for index, want := range wantCodes {
		if index >= len(codes) || *codes[index].ExecutableCode != want {
			t.Fatalf("executable code = %v，期望 %v", codes, wantCodes)
		}
	}
	results := eventsOfKind(events, EventCodeExecutionResult)
	wantResults := []CodeExecutionResult{
		{Outcome: "OUTCOME_UNSPECIFIED"},
		{Outcome: "OUTCOME_UNSPECIFIED", Output: "out", Error: "out"},
		{Outcome: "OUTCOME_4", Output: "later", Error: "later"},
		{Outcome: "OUTCOME_OK", Output: "named"},
		{Outcome: "OUTCOME_OK", Output: "ok"},
		{Outcome: "OUTCOME_FAILED", Error: "failed"},
	}
	if len(results) != len(wantResults) {
		t.Fatalf("results = %v", results)
	}
	for index, want := range wantResults {
		if *results[index].CodeExecutionResult != want {
			t.Fatalf("第 %d 个结果 = %+v，期望 %+v", index, *results[index].CodeExecutionResult, want)
		}
	}
	if joinedText(events) != "done" {
		t.Fatalf("正文 = %q", joinedText(events))
	}
	log.expectOne(t, "language 缺位或为 null")
	log.expectOne(t, "language 不在枚举表里")
	log.expectOne(t, "代码缺位")
	log.expectOne(t, "outcome 缺位或为 null")
	log.expectOne(t, "outcome 不在枚举表里")
}

// TestToleranceFunctionArguments 函数调用参数的 Struct/Value 尽量按最合理的值解码，单个参数异常时该参数按 null，不丢掉整次工具调用；
// 函数名缺失仍是错误（带 ErrFunctionCallMissingName）
func TestToleranceFunctionArguments(t *testing.T) {
	log := captureToleranceLog(t)
	call := traw(`["lookup",[[
		["only_key"],
		["three",[null,null,"v"],"extra"],
		["empty",[]],
		["nulls",[null]],
		["many",[null,1,"s"]],
		["null5",[5]],
		["nan",[null,"NaN"]],
		["inf",[null,"-Infinity"]],
		["numtext",[null,"1.5"]],
		["strnum",[null,null,7]],
		["booltext",[null,null,null,"true"]],
		["notvalue","x"],
		[5,[null,1]],
		[],
		["list",[null,null,null,null,null,[[[null,1],[0]]]]],
		["badlist",[null,null,null,null,null,"x"]],
		["ok",[null,null,"fine"]]
	]],5]`)
	body := tstream("", tframe(tcandidate([]any{tpart(map[int]any{10: call})}, 1, nil), nil))
	events, err := decodeTestStream(body, nil)
	if err != nil {
		t.Fatalf("参数异常不应中止：%v", err)
	}
	calls := eventsOfKind(events, EventToolCall)
	if len(calls) != 1 || calls[0].ToolCall.Name != "lookup" || calls[0].ToolCall.ID != "" {
		t.Fatalf("工具调用 = %+v", calls)
	}
	var arguments map[string]any
	if err := json.Unmarshal(calls[0].ToolCall.Arguments, &arguments); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"only_key": nil, "three": "v", "empty": nil, "nulls": nil, "many": 1.0, "null5": nil, "nan": nil, "inf": nil,
		"numtext": 1.5, "strnum": "7", "booltext": true, "notvalue": nil, "list": []any{1.0, nil}, "badlist": []any{}, "ok": "fine",
	}
	if !reflect.DeepEqual(arguments, want) {
		t.Fatalf("参数 = %v\n期望 %v", arguments, want)
	}
	for _, kind := range []string{
		"只有键没有值", "多于两项", "没有设置任何字段", "同时设置多个字段", "null_value 写法不常见", "NaN", "数字写成字符串",
		"字符串写成其他类型", "布尔值写法不常见", "函数参数 Value 不是数组", "键不是字符串", "不是数组或为空", "ListValue 不是数组", "调用 ID 不是字符串",
	} {
		log.expectOne(t, kind)
	}

	// 函数名缺失、为 null、不是字符串或为空串：无法调用，仍是错误，说明写清楚
	for _, shape := range []string{`[]`, `[null,[]]`, `[5]`, `["",[]]`} {
		body := tstream("", tframe(tcandidate([]any{tpart(map[int]any{10: traw(shape)})}, 1, nil), nil))
		_, err := decodeTestStream(body, nil)
		var evidence *ProtocolEvidenceError
		if !errors.Is(err, ErrFunctionCallMissingName) || !errors.As(err, &evidence) || !strings.Contains(err.Error(), "缺少函数名") {
			t.Fatalf("%s：错误 = %v，期望函数名缺失的协议证据错误", shape, err)
		}
		if StreamInterrupted(err) {
			t.Fatalf("%s：函数名缺失不是流中断，不应重新生成", shape)
		}
	}
}

// TestToleranceRoleAndFeedback 响应角色缺失、为 null 或不是 "model" 时按 model 处理（多帧只写一行）；promptFeedback 没有拦截原因时
// 不是拦截，有拦截原因时照旧按拦截处理
func TestToleranceRoleAndFeedback(t *testing.T) {
	log := captureToleranceLog(t)
	body := tstream("",
		[]any{[]any{[]any{[]any{[]any{ttext("a")}}}}},
		[]any{[]any{[]any{[]any{[]any{ttext("b")}, nil}}}},
		[]any{[]any{[]any{[]any{[]any{ttext("c")}, "assistant"}}}},
		[]any{[]any{[]any{[]any{[]any{ttext("d")}, 5}, 1}}},
	)
	events, err := decodeTestStream(body, newDecodeTolerance(nil, "req-role"))
	if err != nil || joinedText(events) != "abcd" || events[len(events)-1].FinishReason != "stop" {
		t.Fatalf("角色异常：事件 = %v，正文 = %q，错误 = %v", eventKinds(events), joinedText(events), err)
	}
	log.expectOne(t, "响应角色缺失或不是 model")

	// promptFeedback 只有安全评级、没有拦截原因：跳过这一帧，后面的正文照常
	body = tstream("", []any{nil, traw(`[null,[[1,1],[2,1]]]`)}, tframe(tcandidate([]any{ttext("fine")}, 1, nil), nil))
	events, err = decodeTestStream(body, nil)
	if err != nil || joinedText(events) != "fine" {
		t.Fatalf("没有拦截原因的 promptFeedback：事件 = %v，错误 = %v", eventKinds(events), err)
	}
	log.expectOne(t, "promptFeedback 没有拦截原因")
	for shape, reason := range map[string]string{`[1,null,"why"]`: "safety", `["PROHIBITED_CONTENT"]`: "prohibited_content", `[42]`: "provider_42"} {
		_, err := decodeTestStream(tstream("", []any{nil, traw(shape)}), nil)
		var blocked *PromptFeedbackError
		if !errors.As(err, &blocked) || blocked.Reason != reason {
			t.Fatalf("%s：错误 = %v，期望按 %s 拦截", shape, err, reason)
		}
	}
}

// TestToleranceStructural 结构性问题：field 1 的 null 与稀疏元素、空帧、候选为空数组（用量照常读取）跳过；多个候选取第一个；
// field 1 为 null 时照常读取流尾（错误流尾交出上游错误）
func TestToleranceStructural(t *testing.T) {
	log := captureToleranceLog(t)
	frame := tframe(tcandidate([]any{ttext("x")}, nil, nil), nil)
	finish := tframe(tcandidate([]any{ttext("y")}, 1, nil), nil)
	encodedFrame, _ := json.Marshal(frame)
	encodedFinish, _ := json.Marshal(finish)
	body := `[[null,,` + string(encodedFrame) + `,[],[[],null,[3,1,4]],[[` + `[[[[null,"z"]],"model"]],[[[[null,"other"]],"model"]]` + `]],` + string(encodedFinish) + `]]`
	events, err := decodeTestStream(body, nil)
	if err != nil {
		t.Fatalf("结构性问题不应中止：%v", err)
	}
	if joinedText(events) != "xzy" {
		t.Fatalf("正文 = %q，期望第二个候选被丢掉", joinedText(events))
	}
	usage := eventsOfKind(events, EventUsage)
	if len(usage) != 1 || usage[0].Usage.InputTokens != 3 || usage[0].Usage.TotalTokens != 4 {
		t.Fatalf("用量 = %+v，期望候选为空的那一帧的用量照常读取", usage)
	}
	log.expectOne(t, "field 1 的元素为 null")
	log.expectOne(t, "空帧")
	log.expectOne(t, "候选为空数组")
	if line := log.expectOne(t, "多个候选"); line["raw"] != `"候选数=2"` {
		t.Fatalf("多个候选的日志 = %v", line)
	}

	// field 1 为 null：流尾错误照常交出，正常流尾按没有完成帧报错
	_, err = decodeTestStream(`[null,[13,"Internal error encountered."]]`, nil)
	var rpcError *RPCError
	if !errors.As(err, &rpcError) || rpcError.StatusCode != http.StatusInternalServerError {
		t.Fatalf("field 1 为 null 的流尾错误 = %v", err)
	}
	if _, err := decodeTestStream(`[null]`, nil); !errors.Is(err, errMissingFinishFrame) {
		t.Fatalf("field 1 为 null 且没有流尾：错误 = %v，期望没有完成帧", err)
	}
}

// TestToleranceScalars 用量各数的类型变化、finishReason 写成名称或带小数、thought 的其他写法、签名与正文为 null 或其他标量：能转的转，
// 转不了的取缺省（用量走本地估算，finishReason 未知时保留可读名）；正文是对象或数组时仍报错
func TestToleranceScalars(t *testing.T) {
	log := captureToleranceLog(t)
	parts := []any{
		tpart(map[int]any{1: "think", 12: 2}),
		tpart(map[int]any{1: "think2", 12: "true"}),
		tpart(map[int]any{1: "visible", 12: map[string]any{}}),
		tpart(map[int]any{1: nil, 14: 5}),
		tpart(map[int]any{1: 42}),
		tpart(map[int]any{1: "end", 14: "SIG"}),
	}
	body := tstream("", tframe(tcandidate(parts, "STOP", nil), traw(`[10.0,"3",13.0,null,null,null,null,1.0,null,2]`)))
	events, err := decodeTestStream(body, nil)
	if err != nil {
		t.Fatalf("标量类型变化不应中止：%v", err)
	}
	reasoning := eventsOfKind(events, EventReasoning)
	if len(reasoning) != 2 || reasoning[0].Text != "think" || reasoning[1].Text != "think2" {
		t.Fatalf("思考 = %+v", reasoning)
	}
	if joinedText(events) != "visible42end" {
		t.Fatalf("正文 = %q", joinedText(events))
	}
	usage := eventsOfKind(events, EventUsage)
	if len(usage) != 1 || *usage[0].Usage != (Usage{InputTokens: 10, OutputTokens: 3, TotalTokens: 13, ToolTokens: 1, ReasoningTokens: 2}) {
		t.Fatalf("用量 = %+v", usage)
	}
	if finish := events[len(events)-1]; finish.Kind != EventFinish || finish.FinishReason != "stop" {
		t.Fatalf("结束 = %+v", finish)
	}
	for _, kind := range []string{"非 0 即真", "思考标记无法识别", "思考签名不是字符串", "正文不是字符串，按原文", "用量不是整数", "结束原因写成名称"} {
		log.expectOne(t, kind)
	}

	// 用量读不出：丢掉用量（Generate 改用本地估算），结束照常；未知的结束原因名称保留为可读名；结束原因是对象时按 unspecified
	for _, test := range []struct {
		finish any
		usage  any
		reason string
	}{
		{"FINISH_REASON_NEW_THING", traw(`[10,"abc",13]`), "new_thing"},
		{traw(`1.0`), "broken", "stop"},
		{map[string]any{}, nil, "unspecified"},
	} {
		events, err := decodeTestStream(tstream("", tframe(tcandidate([]any{ttext("t")}, test.finish, nil), test.usage)), nil)
		if err != nil || len(eventsOfKind(events, EventUsage)) != 0 || events[len(events)-1].FinishReason != test.reason {
			t.Fatalf("%v：事件 = %+v，错误 = %v", test.finish, events, err)
		}
	}
	log.expectOne(t, "用量不是数字")
	log.expectOne(t, "用量不是数组")
	log.expectOne(t, "结束原因不是整数")
	log.expectOne(t, "结束原因无法识别")

	// 正文是对象或数组：读不出正文，仍按协议证据错误中止
	for _, text := range []any{map[string]any{"text": "x"}, []any{"x"}} {
		_, err := decodeTestStream(tstream("", tframe(tcandidate([]any{tpart(map[int]any{1: text})}, 1, nil), nil)), nil)
		var evidence *ProtocolEvidenceError
		if !errors.As(err, &evidence) || !strings.Contains(evidence.Detail, "读不出正文") {
			t.Fatalf("正文 %v：错误 = %v，期望读不出正文", text, err)
		}
	}
	// 帧、候选与 content 不是数组：同样读不出正文
	for _, body := range []string{`[["frame"]]`, `[[["candidates"]]]`, `[[[[["content"]]]]]`} {
		var evidence *ProtocolEvidenceError
		if _, err := decodeTestStream(body, nil); !errors.As(err, &evidence) {
			t.Fatalf("%s：错误 = %v，期望协议证据错误", body, err)
		}
	}
}

// TestToleranceInlineMediaBase64 inline media 的 Base64 接受标准、URL 安全、带或不带填充的写法（原先只接受标准带填充），MIME 为空时按
// 文件头判断；不是 Base64 时仍报错
func TestToleranceInlineMediaBase64(t *testing.T) {
	log := captureToleranceLog(t)
	png := []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIH\xfb\xff\xfe")
	parts := []any{
		tpart(map[int]any{2: []any{"image/png", base64.StdEncoding.EncodeToString(png)}}),
		tpart(map[int]any{2: []any{"image/png", base64.RawStdEncoding.EncodeToString(png)}}),
		tpart(map[int]any{2: []any{"image/png", base64.URLEncoding.EncodeToString(png)}}),
		tpart(map[int]any{2: []any{"image/png", base64.RawURLEncoding.EncodeToString(png)}}),
		tpart(map[int]any{2: []any{nil, base64.StdEncoding.EncodeToString(png)}}),
	}
	events, err := decodeTestStream(tstream("", tframe(tcandidate(parts, 1, nil), nil)), nil)
	if err != nil {
		t.Fatalf("URL 安全或不带填充的 Base64 不应中止：%v", err)
	}
	media := eventsOfKind(events, EventMedia)
	if len(media) != len(parts) {
		t.Fatalf("媒体 = %d 个", len(media))
	}
	for index, event := range media {
		if !bytes.Equal(event.Media.Data, png) || event.Media.MIME != "image/png" {
			t.Fatalf("第 %d 个媒体 = %s %x", index, event.Media.MIME, event.Media.Data)
		}
	}
	line := log.expectOne(t, "没有 MIME")
	if raw, _ := line["raw"].(string); strings.Contains(raw, base64.StdEncoding.EncodeToString(png)) {
		t.Fatalf("容错日志不应写出媒体数据：%v", line)
	}
	var evidence *ProtocolEvidenceError
	if _, err := decodeTestStream(tstream("", tframe(tcandidate([]any{tpart(map[int]any{2: []any{"image/png", "%%%"}})}, 1, nil), nil)), nil); !errors.As(err, &evidence) {
		t.Fatalf("不是 Base64：错误 = %v，期望协议证据错误", err)
	}
}

// TestToleranceTrailers Build 代理与 Interaction 的流尾为 null、[] 或状态码为 null 时按 0（成功）；错误状态照常交出；Build 多个候选取第一个
func TestToleranceTrailers(t *testing.T) {
	log := captureToleranceLog(t)
	// 同一请求的容错记录：各次解码共用，同一种容错只写一行
	tolerance := newDecodeTolerance(nil, "req-trailer")
	chunk := `[["{\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"first\"}]},\"finishReason\":\"STOP\"},{\"content\":{\"parts\":[{\"text\":\"second\"}]}}]}"]]`
	for _, trailer := range []string{`null`, `[]`, `[null]`, `[null,"OK"]`, `[0]`} {
		var text strings.Builder
		decoder := &BuildStreamDecoder{tolerance: tolerance}
		err := DecodeBuildStream(strings.NewReader("["+chunk+","+trailer+"]"), decoder, func(event Event) error {
			text.WriteString(event.Text)
			return nil
		})
		if err != nil || text.String() != "first" {
			t.Fatalf("Build 流尾 %s：正文 = %q，错误 = %v", trailer, text.String(), err)
		}
	}
	var rpcError *RPCError
	if err := DecodeBuildStream(strings.NewReader("["+chunk+`,[14,"unavailable"]]`), &BuildStreamDecoder{tolerance: tolerance}, func(Event) error { return nil }); !errors.As(err, &rpcError) || rpcError.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("Build 错误流尾 = %v", err)
	}
	if err := DecodeBuildStream(strings.NewReader("["+chunk+`,"weird"]`), &BuildStreamDecoder{tolerance: tolerance}, func(Event) error { return nil }); err == nil {
		t.Fatal("Build 流尾不是数组时仍应报错")
	}
	log.expectOne(t, "多个候选")

	textEvent := make([]any, 11)
	textEvent[10] = []any{nil, []any{[]any{"Hi"}}}
	final := make([]any, 20)
	final[19] = []any{[]any{nil, 3}}
	events, _ := json.Marshal([]any{textEvent, final})
	for _, trailer := range []string{`null`, `[]`, `[null,""]`} {
		var kinds []EventKind
		err := decodeInteractionStream(strings.NewReader("["+string(events)+","+trailer+"]"), func(event Event) error {
			kinds = append(kinds, event.Kind)
			return nil
		}, tolerance, nil)
		if err != nil || !reflect.DeepEqual(kinds, []EventKind{EventText, EventFinish}) {
			t.Fatalf("Interaction 流尾 %s：事件 = %v，错误 = %v", trailer, kinds, err)
		}
	}
	if err := DecodeInteractionStream(strings.NewReader("["+string(events)+`,[8,"exhausted"]]`), func(Event) error { return nil }); !errors.As(err, &rpcError) || rpcError.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("Interaction 错误流尾 = %v", err)
	}
	// Build 与 Interaction 各写一行
	for _, kind := range []string{"流尾状态为 null", "流尾状态码省略或为 null"} {
		lines := log.lines(kind)
		if len(lines) != 2 || lines[0]["method"] != "ProxyStreamedCall" || lines[1]["method"] != "CreateInteractionStream" {
			t.Fatalf("容错日志“%s” = %v", kind, lines)
		}
	}
}

// TestToleranceBuildGrounding Build 的 grounding chunk 没有能读的来源时换成占位项（原先跳过，后面的索引整体错位），多个来源取第一个，
// image 来源按 sourceUri
func TestToleranceBuildGrounding(t *testing.T) {
	captureToleranceLog(t)
	chunk := `{"candidates":[{"content":{"parts":[{"text":"g"}]},"finishReason":"STOP","groundingMetadata":{"groundingChunks":[` +
		`{"web":{"uri":"https://w.example"}},{"somethingNew":{"uri":"https://n.example"}},{"image":{"sourceUri":"https://p.example","domain":"p.example"}},` +
		`{"web":{"uri":"https://a.example"},"maps":{"uri":"https://b.example"}}]}}]}`
	encoded, _ := json.Marshal(chunk)
	var grounding *GroundingMetadata
	err := DecodeBuildStream(strings.NewReader(`[[[`+string(encoded)+`]],[0]]`), &BuildStreamDecoder{}, func(event Event) error {
		if event.Grounding != nil {
			grounding = event.Grounding
		}
		return nil
	})
	want := []GroundingChunk{
		{Source: "web", URI: "https://w.example"}, {Source: "unknown"},
		{Source: "image", URI: "https://p.example", Domain: "p.example"}, {Source: "web", URI: "https://a.example"},
	}
	if err != nil || grounding == nil || !reflect.DeepEqual(grounding.Chunks, want) {
		t.Fatalf("Build grounding = %+v，错误 = %v", grounding, err)
	}
}

// TestToleranceInteractionDelta Interaction 的 content delta：没见过的字段跳过并写日志（正文与媒体照常），没有 content 的 delta 跳过，
// 用量读不出时丢掉用量（结束照常），媒体 Base64 接受 URL 安全与不带填充的写法
func TestToleranceInteractionDelta(t *testing.T) {
	log := captureToleranceLog(t)
	delta := func(content []any) []any {
		event := make([]any, 11)
		event[10] = []any{nil, content}
		return event
	}
	empty := make([]any, 11)
	empty[10] = []any{"meta"}
	final := make([]any, 20)
	final[19] = []any{[]any{nil, 3, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, "broken-usage"}}
	data := []byte{0xfb, 0xff, 0xfe}
	stream := []any{
		delta([]any{[]any{"Hi"}, nil, nil, nil, nil, nil, nil, []any{"new kind"}, nil, nil, []any{"another"}}),
		empty,
		delta([]any{nil, nil, nil, nil, []any{1, base64.RawURLEncoding.EncodeToString(data)}}),
		final,
	}
	encoded, _ := json.Marshal([]any{stream, []any{0}})
	var events []Event
	if err := DecodeInteractionStream(bytes.NewReader(encoded), func(event Event) error {
		events = append(events, event)
		return nil
	}); err != nil {
		t.Fatalf("未识别的 content delta 字段不应中止：%v", err)
	}
	if !reflect.DeepEqual(eventKinds(events), []EventKind{EventText, EventMedia, EventFinish}) || events[0].Text != "Hi" ||
		!bytes.Equal(events[1].Media.Data, data) || events[1].Media.MIME != "video/mp4" {
		t.Fatalf("事件 = %+v", events)
	}
	if len(log.lines("未识别的 content delta 字段")) != 2 {
		t.Fatalf("两个不同位置的未知字段各写一行：\n%s", log.buffer.String())
	}
	log.expectOne(t, "没有 content")
	log.expectOne(t, "用量无法解码")
	log.expectOne(t, "不是标准带填充的 Base64")
}

// TestToleranceGenerateEndToEnd 经 Client.Generate 的完整解码：正文完整交出，只丢掉坏掉的引用与 grounding、用量读不出时改用本地估算；
// 同一种容错在多帧、重新生成与同一请求的多次 Generate（共用 ContextWithDecodeTolerance）里都只写一行运行日志与一行排查记录
func TestToleranceGenerateEndToEnd(t *testing.T) {
	log := captureToleranceLog(t)
	frame := func(text string, finish any, usage any) []any {
		return tframe(tcandidate([]any{ttext(text)}, finish, map[int]any{6: traw(`[[[0,10]]]`), 7: 5}), usage)
	}
	body := tstream("", frame("Hello", nil, nil), frame(", ", nil, nil), frame("world", 1, "broken"))
	upstream := &regenUpstream{responses: []func() (*RPCResponse, error){streamResponse(body)}}
	client := regenClient(t, "test", upstream)
	trace := NewTrace()
	ctx := ContextWithDecodeTolerance(ContextWithTrace(context.Background(), trace), "req-e2e")
	for attempt := 0; attempt < 2; attempt++ {
		source, err := client.Generate(ctx, regenRequest("test"))
		if err != nil {
			t.Fatal(err)
		}
		events := collectEvents(t, source)
		if joinedText(events) != "Hello, world" {
			t.Fatalf("正文 = %q，事件 = %v", joinedText(events), eventKinds(events))
		}
		if len(eventsOfKind(events, EventCitation)) != 0 || len(eventsOfKind(events, EventGrounding)) != 0 || len(eventsOfKind(events, EventError)) != 0 {
			t.Fatalf("事件 = %v，期望只丢掉坏掉的引用与 grounding", eventKinds(events))
		}
		usage := eventsOfKind(events, EventUsage)
		if len(usage) != 1 || usage[0].Usage.InputTokens <= 0 || usage[0].Usage.OutputTokens <= 0 {
			t.Fatalf("用量 = %+v，期望本地估算", usage)
		}
		if last := events[len(events)-1]; last.Kind != EventFinish || last.FinishReason != "stop" {
			t.Fatalf("最后一个事件 = %+v", last)
		}
	}
	for _, kind := range []string{"引用条目缺少 URI", "已丢弃这次 grounding", "用量不是数组"} {
		if line := log.expectOne(t, kind); line["request_id"] != "req-e2e" {
			t.Fatalf("日志 = %v", line)
		}
	}
	var notes []string
	for _, note := range trace.data.Notes {
		if strings.Contains(note, "解码容错 | GenerateContent") {
			notes = append(notes, note)
		}
	}
	if len(notes) != 3 || !strings.Contains(strings.Join(notes, "\n"), "位置=$[0][][0][0][6][0][0][2] | 引用条目缺少 URI，已跳过该条目 | 原文=[0,10]") {
		t.Fatalf("排查记录 = %v", notes)
	}
}

// TestToleranceGenerateStillFailsWithoutBody 正文读不出时经 Generate 仍以错误结束（不把所有错误吞掉）
func TestToleranceGenerateStillFailsWithoutBody(t *testing.T) {
	captureToleranceLog(t)
	body := tstream("", tframe(tcandidate([]any{tpart(map[int]any{1: map[string]any{"text": "x"}})}, 1, nil), nil))
	upstream := &regenUpstream{responses: []func() (*RPCResponse, error){streamResponse(body)}}
	source, err := regenClient(t, "test", upstream).Generate(context.Background(), regenRequest("test"))
	if err != nil {
		t.Fatal(err)
	}
	events := collectEvents(t, source)
	var evidence *ProtocolEvidenceError
	if len(events) != 1 || events[0].Kind != EventError || !errors.As(events[0].Err, &evidence) {
		t.Fatalf("事件 = %v，期望读不出正文的错误", eventKinds(events))
	}
}

// TestToleranceSnippet 容错日志的原文：长 Base64 只写长度，超过上限截断（不切断 UTF-8 字符），缺位写“缺位”
func TestToleranceSnippet(t *testing.T) {
	media := strings.Repeat("QUJD", 64)
	if got := toleranceSnippet(json.RawMessage(`["image/png","` + media + `"]`)); got != `["image/png","<256 字符>"]` {
		t.Fatalf("Base64 原文 = %s", got)
	}
	long := json.RawMessage(`"` + strings.Repeat("中", 200) + `"`)
	got := toleranceSnippet(long)
	if !strings.Contains(got, "…（共 602 字节）") || len(got) > toleranceSnippetLimit+40 || !strings.HasPrefix(got, `"中`) {
		t.Fatalf("截断 = %s", got)
	}
	if toleranceSnippet(nil) != "缺位" {
		t.Fatal("缺位")
	}
}

// TestToleranceBlankAndPendingFinish 结束原因为空串时等同这一帧没有结束原因（原先按 unspecified 提前结束、丢掉后面真正的 max_tokens 与
// 续写判断）；读不出的结束原因（对象、"model" 这类不像枚举名的字符串）不在中间帧定终态，后面出现能识别的结束原因时以后者为准，
// 流结束时仍只有读不出的才按 unspecified 结束（排在之后各帧的事件后面）
func TestToleranceBlankAndPendingFinish(t *testing.T) {
	log := captureToleranceLog(t)
	body := tstream("",
		tframe(tcandidate([]any{ttext("a")}, "", nil), nil),
		tframe(tcandidate([]any{ttext("b")}, 2, nil), traw(`[10,5,15]`)),
	)
	events, err := decodeTestStream(body, nil)
	if err != nil {
		t.Fatal(err)
	}
	finishes := eventsOfKind(events, EventFinish)
	if joinedText(events) != "ab" || len(finishes) != 1 || finishes[0].FinishReason != "max_tokens" || events[len(events)-1].Kind != EventFinish {
		t.Fatalf("空串结束原因：事件 = %v，结束 = %+v，期望只在第二帧按 max_tokens 结束", eventKinds(events), finishes)
	}
	if usage := eventsOfKind(events, EventUsage); len(usage) != 1 || usage[0].Usage.OutputTokens != 5 {
		t.Fatalf("用量 = %+v", usage)
	}
	log.expectOne(t, "结束原因为空串")

	// long- 官网续写：空串的中间帧不结束本轮，真正的完成帧（max_tokens 带续写 token）照常转入下一轮
	decoder := &FrameDecoder{continuable: true, continueAtMaxTokens: true}
	continuation := `[[` + `[[[[[[null,"a"]],"model"],"  "]]],` + longFinishFrame(2, "tok-x", 10, 5, 15) + `]]`
	if err := DecodeGenerateStream(strings.NewReader(continuation), decoder, func(Event) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := decoder.End(); err != nil || !decoder.continuationRequired || decoder.continuation != "tok-x" || decoder.finishCode != 2 {
		t.Fatalf("空串中间帧之后应照常续写：%+v，错误 = %v", decoder, err)
	}

	// 读不出的结束原因在中间帧：后面能识别的结束原因为准
	for _, unreadable := range []any{map[string]any{}, []any{1}, "model"} {
		body := tstream("",
			tframe(tcandidate([]any{ttext("x")}, unreadable, map[int]any{3: "ignored"}), nil),
			tframe(tcandidate([]any{ttext("y")}, 1, nil), nil),
		)
		events, err := decodeTestStream(body, nil)
		if finishes := eventsOfKind(events, EventFinish); err != nil || joinedText(events) != "xy" || len(finishes) != 1 || finishes[0].FinishReason != "stop" {
			t.Fatalf("%v：事件 = %v，结束 = %+v，错误 = %v", unreadable, eventKinds(events), finishes, err)
		}
	}
	// 流结束时仍只有读不出的：按 unspecified 结束，排在之后各帧的事件后面，带那一帧的结束说明与最后的用量
	body = tstream("",
		tframe(tcandidate([]any{ttext("x")}, map[string]any{}, map[int]any{3: " why "}), nil),
		tframe(tcandidate([]any{ttext("y")}, nil, nil), traw(`[7,2,9]`)),
	)
	events, err = decodeTestStream(body, nil)
	if err != nil || !reflect.DeepEqual(eventKinds(events), []EventKind{EventText, EventText, EventUsage, EventFinish}) {
		t.Fatalf("只有读不出的结束原因：事件 = %v，错误 = %v", eventKinds(events), err)
	}
	if finish := events[3]; finish.FinishReason != "unspecified" || finish.FinishMessage != "why" || events[2].Usage.TotalTokens != 9 {
		t.Fatalf("结束 = %+v，用量 = %+v", finish, events[2].Usage)
	}
	// 流尾是错误时照旧交出上游错误，不按 unspecified 结束
	var rpcError *RPCError
	if _, err := decodeTestStream(tstream(`[13,"Internal error encountered."]`, tframe(tcandidate([]any{ttext("x")}, map[string]any{}, nil), nil)), nil); !errors.As(err, &rpcError) {
		t.Fatalf("流尾错误 = %v", err)
	}
	if len(log.lines("结束原因无法识别")) == 0 {
		t.Fatal("读不出的结束原因应写容错日志")
	}

	// 拦截原因为空串：不是拦截，后面的正文照常
	body = tstream("", []any{nil, []any{""}}, tframe(tcandidate([]any{ttext("fine")}, 1, nil), nil))
	events, err = decodeTestStream(body, nil)
	if err != nil || joinedText(events) != "fine" {
		t.Fatalf("空串拦截原因：事件 = %v，错误 = %v", eventKinds(events), err)
	}
}

// TestToleranceMisalignedCandidates 多个候选时其余候选也必须是数组或 null：嵌套少一层（[content, 1] 被当成候选列表）时仍按协议证据错误
// 返回，不把媒体的 Base64 当正文、把 "model" 当结束原因交出
func TestToleranceMisalignedCandidates(t *testing.T) {
	captureToleranceLog(t)
	png := base64.StdEncoding.EncodeToString([]byte("\x89PNG\r\n\x1a\n0000"))
	content := []any{[]any{[]any{nil, nil, []any{"image/png", png}}}, "model"}
	body := tstream("", []any{[]any{content, 1}})
	events, err := decodeTestStream(body, nil)
	var evidence *ProtocolEvidenceError
	if !errors.As(err, &evidence) || evidence.Path != "$[0][][0][1]" || len(eventsOfKind(events, EventText)) != 0 {
		t.Fatalf("嵌套错位：事件 = %+v，错误 = %v，期望协议证据错误", events, err)
	}
	// 其余候选为 null 或数组：取第一个
	body = tstream("", []any{[]any{tcandidate([]any{ttext("first")}, 1, nil), nil, []any{}}})
	if events, err := decodeTestStream(body, nil); err != nil || joinedText(events) != "first" {
		t.Fatalf("其余候选为 null 或空数组：事件 = %v，错误 = %v", eventKinds(events), err)
	}
}

// TestToleranceTranscriptMetadata 转写元数据（Part field 23）的 speaker 不是字符串时忽略，时间戳列表不是数组时丢掉、某个时间戳读不出时
// 丢掉该项（原先都按协议证据错误中止整帧）；同一 Part 已有正文时整段读不出只丢掉元数据；转写正文就是正文且读不出时仍报错
func TestToleranceTranscriptMetadata(t *testing.T) {
	log := captureToleranceLog(t)
	parts := []any{
		tpart(map[int]any{1: "hello", 22: traw(`["hello",5,["bad",[null,[1,5],[2]],[null,"x"]]]`)}),
		tpart(map[int]any{22: traw(`["world",null,"broken"]`)}),
		tpart(map[int]any{1: "kept", 22: traw(`"broken"`)}),
	}
	events, err := decodeTestStream(tstream("", tframe(tcandidate(parts, 1, nil), nil)), nil)
	if err != nil {
		t.Fatalf("转写元数据异常不应中止：%v", err)
	}
	texts := eventsOfKind(events, EventText)
	if joinedText(events) != "helloworldkept" || len(texts) != 3 {
		t.Fatalf("正文 = %q", joinedText(events))
	}
	first := texts[0].Transcript
	if first == nil || first.Speaker != "" || !reflect.DeepEqual(first.Timestamps, []TranscriptTimestamp{{Start: TranscriptDuration{Seconds: 1, Nanos: 5}, End: TranscriptDuration{Seconds: 2}}}) {
		t.Fatalf("第一段转写 = %+v", first)
	}
	if second := texts[1].Transcript; second == nil || second.Text != "world" || second.Timestamps != nil {
		t.Fatalf("第二段转写 = %+v", second)
	}
	if texts[2].Transcript != nil {
		t.Fatalf("读不出的转写元数据应丢掉：%+v", texts[2])
	}
	for _, kind := range []string{"说话人不是字符串", "转写时间戳读不出", "时间戳列表不是数组", "转写元数据读不出"} {
		log.expectOne(t, kind)
	}
	// 转写正文是对象、Part 也没有别的正文：读不出正文，仍报错
	var evidence *ProtocolEvidenceError
	if _, err := decodeTestStream(tstream("", tframe(tcandidate([]any{tpart(map[int]any{22: traw(`[{"t":1}]`)})}, 1, nil), nil)), nil); !errors.As(err, &evidence) || !strings.Contains(evidence.Detail, "读不出正文") {
		t.Fatalf("转写正文读不出：错误 = %v", err)
	}
}

// TestToleranceLiveSessionDedupe Live 会话按会话去重：同一会话的多条 serverContent 与 tool call 里同一种容错只写一行（原先每条消息新建记录，
// 音频分片逐条刷屏）；Live 的 inline media 没有 MIME 时保留为空（客户端按缺省 PCM 处理，与原先相同），不按文件头判断、不写日志
func TestToleranceLiveSessionDedupe(t *testing.T) {
	log := captureToleranceLog(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	session := &BidiSession{ctx: ctx, wireEvents: make(chan BidiEvent, 64), tolerance: newDecodeTolerance(nil, "")}
	audio := base64.StdEncoding.EncodeToString([]byte{0, 1, 2, 3})
	message := func(index int) json.RawMessage {
		content := []any{[]any{[]any{[]any{nil, 42}, []any{nil, nil, []any{nil, audio}}}}}
		call := []any{"lookup", []any{[]any{[]any{"only_key"}}}, "call-" + string(rune('a'+index))}
		frame, _ := json.Marshal([]any{[]any{index + 1, []any{[]any{nil, nil, content}, []any{nil, nil, nil, []any{nil, []any{call}}}}}})
		return frame
	}
	for index := 0; index < 3; index++ {
		if _, err := session.consumeBackchannelFrame(message(index)); err != nil {
			t.Fatal(err)
		}
	}
	close(session.wireEvents)
	var texts, media, calls int
	for event := range session.wireEvents {
		switch event.Kind {
		case BidiEventText:
			texts++
		case BidiEventMedia:
			media++
			if event.Media.MIME != "" {
				t.Fatalf("Live 媒体的 MIME = %q，期望保留为空", event.Media.MIME)
			}
		case BidiEventToolCall:
			calls++
		}
	}
	if texts != 3 || media != 3 || calls != 3 {
		t.Fatalf("事件：正文 %d、媒体 %d、调用 %d", texts, media, calls)
	}
	log.expectOne(t, "正文不是字符串，按原文转成文本")
	log.expectOne(t, "只有键没有值")
	if lines := log.lines("没有 MIME"); len(lines) != 0 {
		t.Fatalf("Live 媒体不应按文件头判断：%v", lines)
	}
}

// TestToleranceSnippetEscapedBase64 容错日志的脱敏认得出带 JSON 转义（\n 换行、\/）的 Base64：只写长度，不写出媒体数据
func TestToleranceSnippetEscapedBase64(t *testing.T) {
	data := make([]byte, 600)
	for index := range data {
		data[index] = byte(index*7 + 3)
	}
	encoded := base64.StdEncoding.EncodeToString(data)
	var wrapped strings.Builder
	for start := 0; start < len(encoded); start += 76 {
		wrapped.WriteString(encoded[start:min(start+76, len(encoded))])
		wrapped.WriteString("\n")
	}
	lines, _ := json.Marshal([]any{5, wrapped.String()})
	slashes := json.RawMessage(strings.ReplaceAll(`[5,"`+encoded+`"]`, "/", `\/`))
	if !strings.Contains(string(lines), `\n`) || !strings.Contains(string(slashes), `\/`) {
		t.Fatal("测试数据应带转义")
	}
	for _, raw := range []json.RawMessage{lines, slashes} {
		got := toleranceSnippet(raw)
		if !strings.HasPrefix(got, `[5,"<`) || strings.Contains(got, encoded[:24]) || strings.Contains(got, encoded[100:124]) {
			t.Fatalf("脱敏 = %s", got)
		}
	}
	// 普通的短字符串与带标点的长文本原样保留
	if got := toleranceSnippet(json.RawMessage(`["image/png","short"]`)); got != `["image/png","short"]` {
		t.Fatalf("短字符串 = %s", got)
	}
	sentence := strings.Repeat("hello, world. ", 10)
	if got := toleranceSnippet(json.RawMessage(`"` + sentence + `"`)); got != `"`+sentence+`"` {
		t.Fatalf("带标点的长文本 = %s", got)
	}
}

// TestToleranceInteractionFinalStatus Omni 最终 interaction 状态不在表里或为 null：已经交出视频或正文时写一行容错日志、照常读取用量、
// 按 other 结束（原先视频已交出仍以协议错误结束，非流式整份作废）；还没交出内容时仍按协议错误返回；失败与取消照旧
func TestToleranceInteractionFinalStatus(t *testing.T) {
	log := captureToleranceLog(t)
	video := make([]any, 11)
	video[10] = []any{nil, []any{nil, nil, nil, nil, []any{1, base64.StdEncoding.EncodeToString([]byte("mp4-data"))}}}
	final := func(status any) []any {
		event := make([]any, 20)
		event[19] = []any{[]any{nil, status, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, []any{4, nil, nil, nil, 6, nil, nil, nil, nil, 10}}}
		return event
	}
	for _, status := range []any{6, nil, "COMPLETED"} {
		encoded, _ := json.Marshal([]any{[]any{video, final(status)}, []any{0}})
		var events []Event
		err := DecodeInteractionStream(bytes.NewReader(encoded), func(event Event) error {
			events = append(events, event)
			return nil
		})
		if err != nil || !reflect.DeepEqual(eventKinds(events), []EventKind{EventMedia, EventUsage, EventFinish}) ||
			events[2].FinishReason != "other" || events[1].Usage.TotalTokens != 10 {
			t.Fatalf("状态 %v：事件 = %+v，错误 = %v", status, events, err)
		}
		// 没有交出内容：仍报错
		encoded, _ = json.Marshal([]any{[]any{final(status)}, []any{0}})
		var evidence *ProtocolEvidenceError
		if err := DecodeInteractionStream(bytes.NewReader(encoded), func(Event) error { return nil }); !errors.As(err, &evidence) {
			t.Fatalf("状态 %v 且没有内容：错误 = %v，期望协议证据错误", status, err)
		}
	}
	if len(log.lines("最终状态不在表里")) != 3 {
		t.Fatalf("三次解码各写一行：\n%s", log.buffer.String())
	}
	encoded, _ := json.Marshal([]any{[]any{video, final(4)}, []any{0}})
	var rpcError *RPCError
	if err := DecodeInteractionStream(bytes.NewReader(encoded), func(Event) error { return nil }); !errors.As(err, &rpcError) {
		t.Fatalf("失败状态 = %v", err)
	}
}

// TestToleranceUsageDroppedAfterEarlierFrame 前一帧用量完整、完成帧的用量读不出时丢掉已记的用量，经 Generate 改用本地估算
// （不交出前一帧偏小的数）
func TestToleranceUsageDroppedAfterEarlierFrame(t *testing.T) {
	log := captureToleranceLog(t)
	body := tstream("",
		tframe(tcandidate([]any{ttext("Hello")}, nil, nil), traw(`[30000,20000,50000]`)),
		tframe(tcandidate([]any{ttext(" world")}, 1, nil), "broken"),
	)
	upstream := &regenUpstream{responses: []func() (*RPCResponse, error){streamResponse(body)}}
	source, err := regenClient(t, "test", upstream).Generate(context.Background(), regenRequest("test"))
	if err != nil {
		t.Fatal(err)
	}
	events := collectEvents(t, source)
	usage := eventsOfKind(events, EventUsage)
	if joinedText(events) != "Hello world" || len(usage) != 1 || usage[0].Usage.InputTokens <= 0 || usage[0].Usage.InputTokens == 30000 || usage[0].Usage.TotalTokens == 50000 {
		t.Fatalf("用量 = %+v，期望本地估算", usage)
	}
	log.expectOne(t, "用量不是数组")
}

// TestToleranceFinishNameContinuation 结束原因写成名称时按名称换算编号，long- 官网续写照常判断："CONTINUATION" 为 22、"MAX_TOKENS" 带
// 续写 token 为 2，都转入下一轮
func TestToleranceFinishNameContinuation(t *testing.T) {
	captureToleranceLog(t)
	frame := func(name string, token string) string {
		return `[[[null,"` + name + `",null,null,null,null,null,null,"` + token + `"]],null,[100,32000,32100]]`
	}
	for _, test := range []struct {
		name string
		code int64
	}{{"CONTINUATION", 22}, {"FINISH_REASON_MAX_TOKENS", 2}} {
		decoder := &FrameDecoder{continuable: true, continueAtMaxTokens: true}
		if err := DecodeGenerateStream(strings.NewReader(longStream(longTextFrame("段", false), frame(test.name, "tok"))), decoder, func(Event) error { return nil }); err != nil {
			t.Fatal(err)
		}
		if !decoder.continuationRequired || decoder.finishCode != test.code || decoder.continuation != "tok" {
			t.Fatalf("%s：decoder = %+v，期望编号 %d 并转入下一轮", test.name, decoder, test.code)
		}
	}

	model, defaults := longArgonModel()
	upstream := &longUpstream{streams: []string{
		longStream(longTextFrame("第一段", false), frame("CONTINUATION", "tok-1")),
		longStream(longTextFrame("第二段", false), frame("MAX_TOKENS", "tok-2")),
		longStream(longTextFrame("第三段", false), `[[[null,"STOP"]],null,[100,100,200]]`),
	}}
	summary, calls, _ := runLong(t, longClient(t, upstream, model, defaults), upstream, context.Background(), longRequest(model.ID, true))
	if len(calls) != 3 || summary.text != "第一段第二段第三段" || len(summary.finishes) != 1 || summary.finishes[0] != "stop" || len(summary.errs) != 0 {
		t.Fatalf("调用 = %d，汇总 = %+v，期望写成名称的结束原因照常续写三轮", len(calls), summary)
	}
	if calls[1].body[16] != "tok-1" || calls[2].body[16] != "tok-2" {
		t.Fatalf("续写 token：%v %v", calls[1].body[16], calls[2].body[16])
	}
}

// TestToleranceCodeExecutionReplay 解码出的不在表里的代码执行枚举（LANGUAGE_2、OUTCOME_4）与写成名称的（JAVASCRIPT）回放给上游时照常编码：
// <prefix>_<n> 按 n 发回，其他名称按 0；按 outcome 选中的字段为空时用另一个（原先报“未识别的 language/outcome”，下一轮 500）
func TestToleranceCodeExecutionReplay(t *testing.T) {
	captureToleranceLog(t)
	body := tstream("", tframe(tcandidate([]any{
		tpart(map[int]any{7: traw(`[2,"x = 1"]`)}),
		tpart(map[int]any{8: traw(`[4,"later"]`)}),
		tpart(map[int]any{7: traw(`["JAVASCRIPT","y"]`)}),
	}, 1, nil), nil))
	events, err := decodeTestStream(body, nil)
	if err != nil {
		t.Fatal(err)
	}
	var parts []Part
	for _, event := range events {
		switch event.Kind {
		case EventExecutableCode:
			parts = append(parts, Part{ExecutableCode: event.ExecutableCode})
		case EventCodeExecutionResult:
			// Gemini 客户端只回传标准的 output 字段
			parts = append(parts, Part{CodeExecutionResult: &CodeExecutionResult{Outcome: event.CodeExecutionResult.Outcome, Output: event.CodeExecutionResult.Output}})
		}
	}
	parts = append(parts, Part{CodeExecutionResult: &CodeExecutionResult{Outcome: "OUTCOME_FAILED", Error: "boom", Output: "ignored"}})
	request := GenerateRequest{Model: "test", Contents: []Content{
		{Role: RoleUser, Parts: []Part{{Text: "run"}}}, {Role: RoleAssistant, Parts: parts}, {Role: RoleUser, Parts: []Part{{Text: "again"}}},
	}}
	encoded, err := EncodeGenerateContentRequest(request, GenerationDefaults{MaxOutputTokens: 1024}, RequestContext{})
	if err != nil {
		t.Fatalf("回放解码出的代码执行枚举应能编码：%v", err)
	}
	var wire []any
	if err := json.Unmarshal(encoded, &wire); err != nil {
		t.Fatal(err)
	}
	model := wire[1].([]any)[1].([]any)[0].([]any)
	want := [][2]any{{7, []any{2.0, "x = 1"}}, {8, []any{4.0, "later"}}, {7, []any{0.0, "y"}}, {8, []any{2.0, "boom"}}}
	for index, item := range want {
		part := model[index].([]any)
		if !reflect.DeepEqual(part[item[0].(int)], item[1]) {
			t.Fatalf("第 %d 个 Part = %v，期望 [%d] = %v", index, part, item[0], item[1])
		}
	}
}

// generateFailure 返回 Generate 的错误：发送前就失败时为返回的错误，交出事件流后为错误事件里的错误
func generateFailure(t *testing.T, client *Client, ctx context.Context, request GenerateRequest) error {
	t.Helper()
	source, err := client.Generate(ctx, request)
	if err != nil {
		return err
	}
	for _, event := range collectEvents(t, source) {
		if event.Kind == EventError {
			return event.Err
		}
	}
	return nil
}

// TestTolerancePromptFeedbackWithEmptyCandidates 真正的输入拦截写成候选为空数组（[[],[1,…]]）、或 promptFeedback 本身不是数组时，
// 照旧按输入拦截结束、不重新生成。跳过这一帧时流里没有完成帧，原先会按“没有完成帧”在同账号重新生成三次、最后对外 500 而不是 400；
// 候选为空数组而 promptFeedback 没有拦截原因时照常跳过这一帧
func TestTolerancePromptFeedbackWithEmptyCandidates(t *testing.T) {
	log := captureToleranceLog(t)
	for _, test := range []struct {
		frame  string
		reason string
	}{
		{`[[],[1,null,"blocked"]]`, "safety"},
		{`[[],[1]]`, "safety"},
		{`[[],{"blockReason":1}]`, "unspecified"},
		{`[null,{"blockReason":1}]`, "unspecified"},
	} {
		_, err := decodeTestStream(`[[`+test.frame+`]]`, nil)
		var blocked *PromptFeedbackError
		if !errors.As(err, &blocked) || blocked.Reason != test.reason {
			t.Fatalf("%s：错误 = %v，期望按 %s 拦截", test.frame, err, test.reason)
		}
		upstream := &regenUpstream{responses: []func() (*RPCResponse, error){streamResponse(`[[` + test.frame + `]]`)}}
		err = generateFailure(t, regenClient(t, "test", upstream), context.Background(), regenRequest("test"))
		if !errors.As(err, &blocked) || blocked.Reason != test.reason || upstream.calls() != 1 {
			t.Fatalf("%s 经 Generate：错误 = %v，上游调用 %d 次，期望按 %s 拦截且不重新生成", test.frame, err, upstream.calls(), test.reason)
		}
	}
	// 两种不是数组的写法各解码两次（直接解码与经 Generate），每次各一份容错记录、各写一行
	if lines := log.lines("promptFeedback 不是数组，按 unspecified 拦截"); len(lines) != 4 {
		t.Fatalf("promptFeedback 不是数组的日志 %d 行，期望 4 行：\n%s", len(lines), log.buffer.String())
	}

	// 候选为空数组、promptFeedback 只有安全评级：不是拦截，跳过这一帧，后面的正文照常
	body := `[[[[],[null,[[1,1]]]],` + string(mustMarshal(tframe(tcandidate([]any{ttext("fine")}, 1, nil), nil))) + `]]`
	events, err := decodeTestStream(body, nil)
	if err != nil || joinedText(events) != "fine" {
		t.Fatalf("候选为空数组且没有拦截原因：事件 = %v，错误 = %v", eventKinds(events), err)
	}
}

func mustMarshal(value any) []byte {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return encoded
}

// TestToleranceCodeExecutionUnreadable 代码执行读不出代码或输出时仍按协议证据错误返回（与正文、inline media 一致），不静默丢掉这一项或
// 交出空代码块：executable code 或 code execution result 本身不是数组，代码或输出是对象或数组。枚举与缺位的容错不变
func TestToleranceCodeExecutionUnreadable(t *testing.T) {
	captureToleranceLog(t)
	for name, part := range map[string][]any{
		"executable code 不是数组":       tpart(map[int]any{7: "print(1)"}),
		"代码是对象":                      tpart(map[int]any{7: traw(`[1,{"src":"print(1)"}]`)}),
		"代码是数组":                      tpart(map[int]any{7: traw(`[1,["print(1)"]]`)}),
		"code execution result 不是数组": tpart(map[int]any{8: "42"}),
		"输出是数组":                      tpart(map[int]any{8: traw(`[1,["42"]]`)}),
		"输出是对象":                      tpart(map[int]any{8: traw(`[null,{"text":"42"}]`)}),
	} {
		_, err := decodeTestStream(tstream("", tframe(tcandidate([]any{ttext("before"), part}, 1, nil), nil)), nil)
		var evidence *ProtocolEvidenceError
		if !errors.As(err, &evidence) {
			t.Fatalf("%s：错误 = %v，期望协议证据错误", name, err)
		}
	}
	// 经 Generate 同样以错误结束（非流式不会拿到 finish=stop 而缺了代码块的结果）
	body := tstream("", tframe(tcandidate([]any{tpart(map[int]any{7: traw(`[1,{"src":"print(1)"}]`)})}, 1, nil), nil))
	upstream := &regenUpstream{responses: []func() (*RPCResponse, error){streamResponse(body)}}
	var evidence *ProtocolEvidenceError
	if err := generateFailure(t, regenClient(t, "test", upstream), context.Background(), regenRequest("test")); !errors.As(err, &evidence) ||
		!strings.Contains(evidence.Detail, "executable code 的代码是对象或数组") {
		t.Fatalf("经 Generate：错误 = %v，期望读不出代码的协议证据错误", err)
	}
	// 枚举与缺位照旧容错
	events, err := decodeTestStream(tstream("", tframe(tcandidate([]any{
		tpart(map[int]any{7: traw(`[null]`)}), tpart(map[int]any{8: traw(`[7,42]`)}),
	}, 1, nil), nil)), nil)
	codes, results := eventsOfKind(events, EventExecutableCode), eventsOfKind(events, EventCodeExecutionResult)
	if err != nil || len(codes) != 1 || *codes[0].ExecutableCode != (ExecutableCode{Language: "LANGUAGE_UNSPECIFIED"}) ||
		len(results) != 1 || *results[0].CodeExecutionResult != (CodeExecutionResult{Outcome: "OUTCOME_7", Output: "42", Error: "42"}) {
		t.Fatalf("枚举与缺位：事件 = %+v，错误 = %v", events, err)
	}
}

// interactionTestStream 把 Interaction 流事件拼成响应（流尾为 [0]）
func interactionTestStream(events ...any) []byte {
	return mustMarshal([]any{events, []any{0}})
}

// interactionFinal 为最终 interaction 事件：status 为最终状态，usage 不为 nil 时写进 interaction field 13
func interactionFinal(status any, usage any) []any {
	interaction := make([]any, 13)
	interaction[1] = status
	interaction[12] = usage
	event := make([]any, 20)
	event[19] = []any{interaction}
	return event
}

// decodeInteractionTest 解码一段 Interaction 响应，返回事件与错误
func decodeInteractionTest(body []byte) ([]Event, error) {
	var events []Event
	err := DecodeInteractionStream(bytes.NewReader(body), func(event Event) error {
		events = append(events, event)
		return nil
	})
	return events, err
}

// TestToleranceInteractionMediaMIME Omni 媒体的 MIME 枚举：null 与 0 取该类的缺省 MIME（官网现有的视频即为 [null,<数据>]，不写日志）；
// 不在表里的整数取缺省 MIME，带小数的数与数字字符串按编号，写成表里的 MIME 字符串时按字符串，写成名称等其他写法取缺省 MIME，各写一行
// 容错日志（原先不是整数时按协议证据错误中止整次生成，媒体已经生成、额度已扣；表外整数不写日志，官网改了枚举看不出来）
func TestToleranceInteractionMediaMIME(t *testing.T) {
	log := captureToleranceLog(t)
	data := []byte("\x00\x00\x00\x18ftypmp42video")
	encoded := base64.StdEncoding.EncodeToString(data)
	video := func(code any) []any { return interactionDeltaEvent([]any{nil, nil, nil, nil, []any{code, encoded}}) }
	image := func(code any) []any { return interactionDeltaEvent([]any{nil, []any{code, encoded}}) }
	for _, test := range []struct {
		event []any
		mime  string
	}{
		{video(nil), "video/mp4"}, {video(0), "video/mp4"}, {video(7), "video/webm"},
		{video(42), "video/mp4"}, {video(43), "video/mp4"},
		{video("VIDEO_WEBM"), "video/mp4"}, {video(map[string]any{"mime": 7}), "video/mp4"},
		{video(traw(`7.0`)), "video/webm"}, {video("7"), "video/webm"}, {video(traw(`7.4`)), "video/webm"},
		{image("image/png"), "image/png"}, {image(" IMAGE/WEBP "), "image/webp"},
	} {
		events, err := decodeInteractionTest(interactionTestStream(test.event, interactionFinal(3, []any{1, nil, nil, nil, 2, nil, nil, nil, 0, 3})))
		if err != nil || !reflect.DeepEqual(eventKinds(events), []EventKind{EventMedia, EventUsage, EventFinish}) ||
			events[0].Media.MIME != test.mime || !bytes.Equal(events[0].Media.Data, data) || events[2].FinishReason != "stop" {
			t.Fatalf("%v：事件 = %+v，错误 = %v，期望 %s", test.event[10], events, err, test.mime)
		}
	}
	// 每次解码各有一份容错记录：表外整数两次、不是整数的两种写法、按编号两次（数字字符串 "7" 与原先一样直接按整数读，不写日志）、
	// MIME 字符串两次
	for kind, count := range map[string]int{
		"视频 MIME 枚举不在表里，取缺省 MIME video/mp4": 2, "视频 MIME 枚举不是整数，取缺省 MIME video/mp4": 2,
		"视频 MIME 枚举不是整数，按编号换算": 2, "图片 MIME 枚举写成 MIME 字符串": 2,
	} {
		if lines := log.lines(kind); len(lines) != count {
			t.Fatalf("容错日志“%s”有 %d 行，期望 %d 行：\n%s", kind, len(lines), count, log.buffer.String())
		}
	}
	if total := len(log.lines("")); total != 8 {
		t.Fatalf("null、0 与表内整数不应写日志，共 %d 行：\n%s", total, log.buffer.String())
	}
	// 同一请求里同一种只写一行
	tolerance := newDecodeTolerance(nil, "req-mime")
	if err := decodeInteractionStream(bytes.NewReader(interactionTestStream(video(42), video(44), interactionFinal(3, nil))), func(Event) error { return nil }, tolerance, nil); err != nil {
		t.Fatal(err)
	}
	if lines := log.lines("视频 MIME 枚举不在表里"); len(lines) != 3 || lines[2]["request_id"] != "req-mime" {
		t.Fatalf("同一请求两个表外枚举应只写一行：\n%s", log.buffer.String())
	}
	// 数据不是 Base64 仍报错
	var evidence *ProtocolEvidenceError
	if _, err := decodeInteractionTest(interactionTestStream(interactionDeltaEvent([]any{nil, nil, nil, nil, []any{"VIDEO_MP4", "%%%"}}), interactionFinal(3, nil))); !errors.As(err, &evidence) {
		t.Fatalf("数据不是 Base64：错误 = %v，期望协议证据错误", err)
	}
}

// TestToleranceInteractionUsage Omni 用量各数带小数或写成数字字符串时按整数换算、总数缺位或为 0 时按输入、输出、思考之和（原先报“期望整数”
// “缺少 total tokens”，整次生成中止）；整份读不出时经 Generate 改用本地估算（原先只丢掉用量，账本记 0）
func TestToleranceInteractionUsage(t *testing.T) {
	log := captureToleranceLog(t)
	text := interactionDeltaEvent([]any{[]any{"海浪"}})
	for _, test := range []struct {
		usage any
		want  Usage
		kind  string
	}{
		{traw(`[10,null,null,null,5.0,null,null,null,1,16]`), Usage{InputTokens: 10, OutputTokens: 5, ReasoningTokens: 1, TotalTokens: 16}, "用量不是整数，按整数换算"},
		{traw(`["10",null,null,null,5.4,null,null,null,1,"16.0"]`), Usage{InputTokens: 10, OutputTokens: 5, ReasoningTokens: 1, TotalTokens: 16}, "用量不是整数，按整数换算"},
		{traw(`[10,null,null,null,5,null,null,null,1]`), Usage{InputTokens: 10, OutputTokens: 5, ReasoningTokens: 1, TotalTokens: 16}, "用量缺少总数"},
		{traw(`[10,null,null,null,5,null,null,null,1,0]`), Usage{InputTokens: 10, OutputTokens: 5, ReasoningTokens: 1, TotalTokens: 16}, "用量缺少总数"},
	} {
		before := len(log.lines(test.kind))
		events, err := decodeInteractionTest(interactionTestStream(text, interactionFinal(3, test.usage)))
		if err != nil || !reflect.DeepEqual(eventKinds(events), []EventKind{EventText, EventUsage, EventFinish}) || *events[1].Usage != test.want {
			t.Fatalf("%s：事件 = %+v，错误 = %v", test.usage, events, err)
		}
		if after := len(log.lines(test.kind)); after != before+1 {
			t.Fatalf("%s：容错日志“%s”应多一行：\n%s", test.usage, test.kind, log.buffer.String())
		}
	}
	// 表内整数与原先相同，不写日志
	before := len(log.lines(""))
	events, err := decodeInteractionTest(interactionTestStream(text, interactionFinal(3, []any{3, nil, nil, nil, 19310, nil, nil, nil, 589, 19902})))
	if err != nil || len(events) != 3 || *events[1].Usage != (Usage{InputTokens: 3, OutputTokens: 19310, ReasoningTokens: 589, TotalTokens: 19902}) || len(log.lines("")) != before {
		t.Fatalf("官网用量：事件 = %+v，错误 = %v", events, err)
	}

	// 经 Generate：用量读不出（不是数组、某个数读不出、全为 0）时交出本地估算，正文与结束照常，日志每请求一行
	for _, usage := range []any{"broken", traw(`[10,null,null,null,{"n":5}]`), traw(`[]`)} {
		body := interactionTestStream(text, interactionDeltaEvent([]any{nil, nil, nil, nil, []any{nil, base64.StdEncoding.EncodeToString([]byte("mp4"))}}), interactionFinal(3, usage))
		events := collectEvents(t, generateInteractionTest(t, body))
		usageEvents := eventsOfKind(events, EventUsage)
		if joinedText(events) != "海浪" || len(eventsOfKind(events, EventMedia)) != 1 || len(usageEvents) != 1 ||
			usageEvents[0].Usage.InputTokens <= 0 || usageEvents[0].Usage.OutputTokens <= 0 || events[len(events)-1].FinishReason != "stop" {
			t.Fatalf("用量 %v：事件 = %+v", usage, events)
		}
	}
	if lines := log.lines("用量无法解码，改用本地估算"); len(lines) != 3 {
		t.Fatalf("三次请求各写一行：\n%s", log.buffer.String())
	}
}

// generateInteractionTest 经 Client.Generate 走 CreateInteractionStream，上游返回 body，返回事件流
func generateInteractionTest(t *testing.T, body []byte) <-chan Event {
	t.Helper()
	upstream := &regenUpstream{responses: []func() (*RPCResponse, error){streamResponse(string(body))}}
	client := regenClient(t, "test", upstream)
	catalog := client.catalogs["alice@example.com"]
	entry := catalog.entries["test"]
	entry.defaults = omniTestDefaults()
	catalog.entries["test"] = entry
	source, err := client.Generate(context.Background(), regenRequest("test"))
	if err != nil {
		t.Fatal(err)
	}
	return source
}

// TestToleranceInteractionEmptyShapes Interaction 的空文本 delta（content field 1 为 []：TextContent 全为缺省值）跳过；最终 interaction
// 为 [] 而已经交出过内容时按 other 结束（经 Generate 时交出本地估算的用量）。原先分别报“文本 delta 无效”“最终 interaction 为空”，
// 正文或视频已交出也整次 500。还没交出内容时最终 interaction 为 [] 仍报错，正文是对象或数组时仍报错
func TestToleranceInteractionEmptyShapes(t *testing.T) {
	log := captureToleranceLog(t)
	emptyFinal := make([]any, 20)
	emptyFinal[19] = []any{}
	text := interactionDeltaEvent([]any{[]any{"hi"}})
	blank := interactionDeltaEvent([]any{[]any{}})
	number := interactionDeltaEvent([]any{[]any{5}})
	events, err := decodeInteractionTest(interactionTestStream(text, blank, number, interactionFinal(3, nil)))
	if err != nil || joinedText(events) != "hi5" || events[len(events)-1].FinishReason != "stop" {
		t.Fatalf("空文本 delta：事件 = %+v，错误 = %v", events, err)
	}
	log.expectOne(t, "文本 delta 为空，已跳过")
	log.expectOne(t, "正文不是字符串，按原文转成文本")

	events, err = decodeInteractionTest(interactionTestStream(text, emptyFinal))
	if err != nil || !reflect.DeepEqual(eventKinds(events), []EventKind{EventText, EventFinish}) || events[1].FinishReason != "other" {
		t.Fatalf("已交出正文、最终 interaction 为空：事件 = %+v，错误 = %v", events, err)
	}
	log.expectOne(t, "最终 interaction 为空，已交出内容")
	generated := collectEvents(t, generateInteractionTest(t, interactionTestStream(text, emptyFinal)))
	if usage := eventsOfKind(generated, EventUsage); joinedText(generated) != "hi" || len(usage) != 1 || usage[0].Usage.InputTokens <= 0 ||
		generated[len(generated)-1].FinishReason != "other" {
		t.Fatalf("经 Generate：事件 = %+v", generated)
	}

	var evidence *ProtocolEvidenceError
	for name, body := range map[string][]byte{
		"没有交出内容、最终 interaction 为空": interactionTestStream(blank, emptyFinal),
		"正文是对象":         interactionTestStream(interactionDeltaEvent([]any{[]any{map[string]any{"text": "hi"}}}), interactionFinal(3, nil)),
		"文本 delta 不是数组": interactionTestStream(interactionDeltaEvent([]any{"hi"}), interactionFinal(3, nil)),
	} {
		if _, err := decodeInteractionTest(body); !errors.As(err, &evidence) {
			t.Fatalf("%s：错误 = %v，期望协议证据错误", name, err)
		}
	}
}

// TestToleranceSnippetEmbeddedBase64 容错日志的脱敏不要求整个字符串都是 Base64：data URL、前后带文字的 Base64、带 \n 转义的分行 Base64
// 只替换 Base64 那一段（写成 <N 字符>），字符串其余部分保留
func TestToleranceSnippetEmbeddedBase64(t *testing.T) {
	data := make([]byte, 360)
	for index := range data {
		data[index] = byte(index*13 + 5)
	}
	encoded := base64.StdEncoding.EncodeToString(data)
	var wrapped strings.Builder
	for start := 0; start < len(encoded); start += 76 {
		wrapped.WriteString(encoded[start:min(start+76, len(encoded))])
		wrapped.WriteString("\n")
	}
	for _, test := range []struct {
		value any
		keep  []string
	}{
		{[]any{nil, nil, "data:image/png;base64," + encoded}, []string{`"data:image/png;base64,<480 字符>"`}},
		{map[string]any{"note": "see: " + encoded + " (attached)"}, []string{`"see: <480 字符> (attached)"`}},
		// 分行 Base64 后面紧跟换行的短单词会并进这一段（宁可多遮一点）
		{[]any{"file:\n" + wrapped.String() + "end."}, []string{`"file:\n<`, ` 字符>."`}},
		{[]any{"data:image/png;base64," + base64.RawURLEncoding.EncodeToString(data)}, []string{`"data:image/png;base64,<480 字符>"`}},
	} {
		raw := mustMarshal(test.value)
		got := toleranceSnippet(raw)
		for _, segment := range []string{encoded[:24], encoded[200:224], encoded[400:424], base64.RawURLEncoding.EncodeToString(data)[200:224]} {
			if strings.Contains(got, segment) {
				t.Fatalf("%s：脱敏后仍有 Base64 原文：%s", raw[:40], got)
			}
		}
		for _, keep := range test.keep {
			if !strings.Contains(got, keep) {
				t.Fatalf("脱敏 = %s，期望保留 %s", got, keep)
			}
		}
	}
	// 普通文本（含长单词与 URL）不受影响
	for _, value := range []string{
		strings.Repeat("hello world, ", 12),
		"call get_weather_for_city_with_a_very_long_function_name(city='Paris') and then summarize_the_forecast_for_the_next_days.",
		"https://example.com/some/path?query=value&other=1 and more text here to make it long enough for the check",
	} {
		raw := mustMarshal(value)
		if got := toleranceSnippet(raw); got != string(raw) {
			t.Fatalf("普通文本 = %s", got)
		}
	}
}

// TestTolerancePromptFeedbackAfterContent 已经交出正文后才出现读不出的 promptFeedback（不是数组）：正文已经出来，不可能是输入拦截，
// 按未拦截继续、照常读到完成帧（原先按 unspecified 拦截，流式中途 400、非流式整份作废）；真正的拦截 [[],[1]] 不再多写一行“已跳过候选”
func TestTolerancePromptFeedbackAfterContent(t *testing.T) {
	log := captureToleranceLog(t)
	content := string(mustMarshal(tframe(tcandidate([]any{ttext("fine")}, nil, nil), nil)))
	finish := string(mustMarshal(tframe(tcandidate([]any{ttext("!")}, 1, nil), nil)))
	for _, feedback := range []string{`[[],{"x":1}]`, `[null,{"x":1}]`} {
		events, err := decodeTestStream(`[[`+content+`,`+feedback+`,`+finish+`]]`, nil)
		finishes := eventsOfKind(events, EventFinish)
		if err != nil || joinedText(events) != "fine!" || len(finishes) != 1 || finishes[0].FinishReason != "stop" {
			t.Fatalf("%s：事件 = %+v，错误 = %v，期望正文完整、按 stop 结束", feedback, events, err)
		}
	}
	if lines := log.lines("promptFeedback 不是数组，已有正文，按未拦截继续"); len(lines) != 2 {
		t.Fatalf("已有正文时 promptFeedback 不是数组的日志 %d 行，期望 2 行：\n%s", len(lines), log.buffer.String())
	}
	if lines := log.lines("promptFeedback 不是数组，按 unspecified 拦截"); len(lines) != 0 {
		t.Fatalf("已有正文时不应按拦截记日志：\n%s", log.buffer.String())
	}

	log = captureToleranceLog(t)
	_, err := decodeTestStream(`[[[[],[1]]]]`, nil)
	var blocked *PromptFeedbackError
	if !errors.As(err, &blocked) || blocked.Reason != "safety" {
		t.Fatalf("[[],[1]]：错误 = %v，期望按 safety 拦截", err)
	}
	if lines := log.lines("候选为空数组，已跳过候选"); len(lines) != 0 {
		t.Fatalf("真正的拦截不应写“已跳过候选”：\n%s", log.buffer.String())
	}
}

// TestToleranceInteractionEmptyInteraction 最终 interaction 写成 [[]] 或 [null]（Interaction 的字段全为缺省值）而已经交出过内容：
// 与 wrapper 为空（[]）时一样按 other 结束、经 Generate 交出本地估算的用量（原先走“最终状态不在表里”分支，没有用量，账本记 0）；
// 还没交出内容时仍报错
func TestToleranceInteractionEmptyInteraction(t *testing.T) {
	captureToleranceLog(t)
	text := interactionDeltaEvent([]any{[]any{"hi"}})
	for name, inner := range map[string]any{"[[]]": []any{}, "[null]": nil} {
		final := make([]any, 20)
		final[19] = []any{inner}
		events, err := decodeInteractionTest(interactionTestStream(text, final))
		if err != nil || !reflect.DeepEqual(eventKinds(events), []EventKind{EventText, EventFinish}) || events[1].FinishReason != "other" {
			t.Fatalf("%s：事件 = %+v，错误 = %v", name, events, err)
		}
		generated := collectEvents(t, generateInteractionTest(t, interactionTestStream(text, final)))
		if usage := eventsOfKind(generated, EventUsage); joinedText(generated) != "hi" || len(usage) != 1 || usage[0].Usage.InputTokens <= 0 ||
			generated[len(generated)-1].FinishReason != "other" {
			t.Fatalf("%s 经 Generate：事件 = %+v，期望本地估算的用量与 other 结束", name, generated)
		}
		blank := interactionDeltaEvent([]any{[]any{}})
		if _, err := decodeInteractionTest(interactionTestStream(blank, final)); err == nil {
			t.Fatalf("%s：还没交出内容时应报错", name)
		}
	}
}
