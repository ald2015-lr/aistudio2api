package aistudio

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

// 超过 63 字节的函数名（wireFunctionName / restoreFunctionNames）的测试：发往 Playground、CountTokens 与 Build 时截短加哈希，
// 回传的调用在交给 Generate 之前换回原名；Live 照旧按原名

// longFunctionPrefix 为 Codex apps 工具拼出的长名前缀（与上游 Mag1cFall/AIStudio2API 的测试相同）
const longFunctionPrefix = "mcp__codex_apps__codex_document_control___execute_document_"

// TestWireFunctionNameEncoding 声明、历史里的函数调用与函数结果用同一个截短名（原名前 50 个字节、"_" 与 SHA-256 前 6 字节的十六进制，
// 共 63 字节），不超过 63 字节的名字原样；回传的截短名换回原名，其他名字与其他事件不变；CountTokens 与 Build 通道（声明、
// allowedFunctionNames、历史）用同一个截短名；Live（bidi）照旧发原名
func TestWireFunctionNameEncoding(t *testing.T) {
	names := []string{longFunctionPrefix + "command", longFunctionPrefix + "commands", "Read", strings.Repeat("x", 63)}
	functions := make([]FunctionDeclaration, 0, len(names))
	for _, name := range names {
		functions = append(functions, FunctionDeclaration{Name: name, Parameters: json.RawMessage(`{"type":"object"}`)})
	}
	wire, _, err := encodeRequestedTools(Tools{Functions: functions})
	if err != nil {
		t.Fatal(err)
	}
	sent := make([]string, 0, len(names))
	for _, declaration := range wire[0].([]any)[1].([]any) {
		sent = append(sent, declaration.([]any)[0].(string))
	}
	for index := range 2 {
		name := sent[index]
		suffix := name[len(name)-13:]
		if len(name) != maxWireFunctionNameLength || name[:50] != names[index][:50] || suffix[0] != '_' {
			t.Fatalf("截短名 %q 应为原名前 50 个字节加 _ 与 12 位十六进制，共 63 字节", name)
		}
		if _, err := hex.DecodeString(suffix[1:]); err != nil {
			t.Fatalf("截短名 %q 的结尾不是十六进制", name)
		}
	}
	if sent[0] == sent[1] || sent[2] != "Read" || sent[3] != names[3] {
		t.Fatalf("声明的函数名 %q", sent)
	}
	contents, err := encodeContents([]Content{
		{Role: RoleUser, Parts: []Part{{Text: "run"}}},
		{Role: RoleAssistant, Parts: []Part{{FunctionCall: &FunctionCall{ID: "call-1", Name: names[0], Arguments: json.RawMessage(`{}`)}}}},
		{Role: RoleUser, Parts: []Part{{FunctionResult: &FunctionResult{ID: "call-1", Name: names[0], Content: json.RawMessage(`{"ok":true}`)}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	history, _ := json.Marshal(contents)
	if strings.Count(string(history), `"`+sent[0]+`"`) != 2 || strings.Contains(string(history), names[0]) {
		t.Fatalf("历史中的函数名与声明不一致: %s", history)
	}

	var received []Event
	emit := restoreFunctionNames(functions, func(event Event) error {
		received = append(received, event)
		return nil
	})
	original := &FunctionCall{ID: "u1", Name: sent[0], Arguments: json.RawMessage(`{"a":1}`)}
	for _, event := range []Event{
		{Kind: EventToolCall, ToolCall: original},
		{Kind: EventToolCall, ToolCall: &FunctionCall{Name: sent[1]}},
		{Kind: EventToolCall, ToolCall: &FunctionCall{Name: "Read"}},
		{Kind: EventToolCall, ToolCall: &FunctionCall{Name: names[0]}},
		{Kind: EventText, Text: sent[0]},
	} {
		if err := emit(event); err != nil {
			t.Fatal(err)
		}
	}
	got := []string{received[0].ToolCall.Name, received[1].ToolCall.Name, received[2].ToolCall.Name, received[3].ToolCall.Name, received[4].Text}
	if want := []string{names[0], names[1], "Read", names[0], sent[0]}; !reflect.DeepEqual(got, want) {
		t.Fatalf("回传的函数名 %q，期望 %q", got, want)
	}
	if original.Name != sent[0] || received[0].ToolCall.ID != "u1" || string(received[0].ToolCall.Arguments) != `{"a":1}` {
		t.Fatalf("还原应复制调用、不改上游事件：%+v %+v", original, received[0].ToolCall)
	}

	count, err := EncodeCountTokensRequest(TokenCountRequest{
		Model: "test", Contents: []Content{{Role: RoleUser, Parts: []Part{{Text: "run"}}}}, Tools: Tools{Functions: functions},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(count), `"`+sent[0]+`"`) || strings.Contains(string(count), names[0]) {
		t.Fatalf("CountTokens 的声明应为截短名: %s", count)
	}

	build := GenerateRequest{
		Model: "test", Tools: Tools{Functions: functions[:2], ToolConfig: ToolConfig{Mode: "required", AllowedFunctionNames: []string{names[1]}}},
		Contents: []Content{
			{Role: RoleUser, Parts: []Part{{Text: "run"}}},
			{Role: RoleAssistant, Parts: []Part{{FunctionCall: &FunctionCall{ID: "call-1", Name: names[0], Arguments: json.RawMessage(`{}`)}}}},
			{Role: RoleUser, Parts: []Part{{FunctionResult: &FunctionResult{ID: "call-1", Name: names[0], Content: json.RawMessage(`{"ok":true}`)}}}},
		},
	}
	_, body, err := EncodeBuildGenerateRequest(build, GenerationDefaults{MaxOutputTokens: 8192}, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(body), `"`+sent[0]+`"`) != 3 ||
		!strings.Contains(string(body), `"allowedFunctionNames":["`+sent[1]+`"]`) || strings.Contains(string(body), names[0]) || strings.Contains(string(body), names[1]) {
		t.Fatalf("Build 请求的声明、allowedFunctionNames 与历史应为截短名: %s", body)
	}

	live, _, err := EncodeBidiSetupRequest(BidiRequest{Model: "models/gemini-live", Mode: BidiModeLive, Tools: functions[:1]}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(live), `"`+names[0]+`"`) {
		t.Fatalf("Live 会话应照旧按原名声明: %s", live)
	}
}

// TestWireFunctionNameUnicode 名字含多字节字符时截取退到字符边界：截短名是合法 UTF-8、不超过 63 字节，经 JSON 编码发往上游、上游按
// 请求体里的名字调用（再经 JSON 解码）时仍能换回原名。原先按字节在字符中间截断，非法字节在请求体里变成 U+FFFD，回传时对不上。
// ASCII 名字与上游逐字节相同（前 50 个字节）
func TestWireFunctionNameUnicode(t *testing.T) {
	for _, name := range []string{
		strings.Repeat("工", 30),                                 // 90 字节：第 50 字节落在第 17 个字的中间
		"mcp__docs__" + strings.Repeat("检索", 10) + "_tool",      // 前缀 ASCII，后面是多字节字符
		strings.Repeat("a", 49) + "é" + strings.Repeat("b", 20), // é 占第 50、51 个字节，截取处落在它中间
		strings.Repeat("😀", 20),                                 // 4 字节字符
	} {
		wire := wireFunctionName(name)
		if !utf8.ValidString(wire) || len(wire) > maxWireFunctionNameLength || wire == name {
			t.Fatalf("%q 的截短名 %q（%d 字节）应为不超过 63 字节的合法 UTF-8", name, wire, len(wire))
		}
		prefix := strings.TrimSuffix(wire, wire[len(wire)-13:])
		if !strings.HasPrefix(name, prefix) || len(prefix) > 50 || len(prefix) < 47 {
			t.Fatalf("%q 的截短名 %q 应取原名前 50 个字节内完整的字符", name, wire)
		}
		encoded, err := json.Marshal([]any{wire})
		if err != nil {
			t.Fatal(err)
		}
		var decoded []string
		if err := json.Unmarshal(encoded, &decoded); err != nil || decoded[0] != wire {
			t.Fatalf("截短名经 JSON 编码后变了：%q → %q", wire, decoded)
		}
		var restored string
		emit := restoreFunctionNames([]FunctionDeclaration{{Name: name}}, func(event Event) error {
			restored = event.ToolCall.Name
			return nil
		})
		if err := emit(Event{Kind: EventToolCall, ToolCall: &FunctionCall{Name: decoded[0]}}); err != nil || restored != name {
			t.Fatalf("上游按请求体里的名字 %q 调用，换回 %q，期望 %q", decoded[0], restored, name)
		}
	}
	ascii := longFunctionPrefix + "command"
	if wire := wireFunctionName(ascii); wire[:50] != ascii[:50] || len(wire) != maxWireFunctionNameLength {
		t.Fatalf("ASCII 名字应取前 50 个字节：%q", wire)
	}
}

// TestWireFunctionNameCountsBytes 长度按字节算（与 README、protocol.md 的说明一致）：21 个汉字正好 63 字节，原样发送；22 个汉字只有
// 22 个字符、却有 66 字节，按截短名发送；40 个汉字（120 字节）取前 16 个字（48 字节）加 "_" 与 12 位十六进制，共 61 字节
func TestWireFunctionNameCountsBytes(t *testing.T) {
	if name := strings.Repeat("工", 21); wireFunctionName(name) != name {
		t.Fatalf("63 字节的名字应原样发送：%q", wireFunctionName(name))
	}
	if name := strings.Repeat("工", 22); wireFunctionName(name) == name {
		t.Fatalf("22 个字符、66 字节的名字应按截短名发送")
	}
	name := strings.Repeat("工", 40)
	wire := wireFunctionName(name)
	if len(wire) != 61 || !strings.HasPrefix(wire, strings.Repeat("工", 16)+"_") {
		t.Fatalf("40 个汉字的截短名应为前 16 个字加 _ 与 12 位十六进制，共 61 字节：%q（%d 字节）", wire, len(wire))
	}
}

// TestWireFunctionNamesInfo 请求日志的截短名对照：按声明顺序列出被截短的函数（最多 5 个，其余写数量），没有超长函数名时为空串
func TestWireFunctionNamesInfo(t *testing.T) {
	if info := wireFunctionNamesInfo([]FunctionDeclaration{{Name: "Read"}, {Name: strings.Repeat("x", 63)}}); info != "" {
		t.Fatalf("没有超长函数名时不写对照：%q", info)
	}
	var functions []FunctionDeclaration
	var pairs []string
	for index := range 7 {
		name := fmt.Sprintf("%s%02d_command", longFunctionPrefix, index)
		functions = append(functions, FunctionDeclaration{Name: "Read"}, FunctionDeclaration{Name: name})
		if index < 5 {
			pairs = append(pairs, wireFunctionName(name)+"="+name)
		}
	}
	want := "函数名超过 63 字节，按截短名发送（上游返回的调用换回原名） | 函数=7 个 | " + strings.Join(pairs, "；") + "；…另有 2 个"
	if info := wireFunctionNamesInfo(functions); info != want {
		t.Fatalf("对照=%q\n期望=%q", info, want)
	}
}

// longFunctionTools 为一个超长名函数与一个普通函数；mode 为工具模式，pick 非空时指定这个函数
func longFunctionTools(mode string, pick string) Tools {
	tools := Tools{
		Functions: []FunctionDeclaration{
			{Name: longFunctionPrefix + "command", Parameters: json.RawMessage(`{"type":"object","properties":{"task":{"type":"string"}}}`)},
			{Name: "Read", Parameters: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}}}`)},
		},
		ToolConfig: ToolConfig{Mode: mode},
	}
	if pick != "" {
		tools.ToolConfig.AllowedFunctionNames = []string{pick}
	}
	return tools
}

// TestGenerateLongFunctionNamePlayground 经 Client.Generate 走 Playground：请求体里只有截短名（声明与必须调用时系统指令、重新生成时
// 用户消息里的工具要求都写截短名），上游以截短名调用时交出的调用为原名、补上本地调用 ID；必须调用指定的长名函数时工具约束按原名核对通过
// （扣住的正文随调用一起交出，不判违约、不重新生成）；上游只回复文字时同账号重新生成，加强的要求同样写截短名
func TestGenerateLongFunctionNamePlayground(t *testing.T) {
	name := longFunctionPrefix + "command"
	wire := wireFunctionName(name)
	for _, test := range []struct {
		name      string
		tools     Tools
		responses []string
		sends     int
	}{
		{name: "auto", tools: longFunctionTools("auto", ""), responses: []string{regenStream("", guardFunctionFrame(wire, `[[["task",[null,null,"go"]]]]`), regenStopFrame)}, sends: 1},
		{
			name: "必须调用指定的长名函数", tools: longFunctionTools("required", name), sends: 1,
			responses: []string{regenStream("", guardTextFrame, guardFunctionFrame(wire, `[[["task",[null,null,"go"]]]]`), regenStopFrame)},
		},
		{
			name: "只回复文字后重新生成", tools: longFunctionTools("required", name), sends: 2,
			responses: []string{
				regenStream("", guardTextFrame, regenStopFrame),
				regenStream("", guardFunctionFrame(wire, `[[["task",[null,null,"go"]]]]`), regenStopFrame),
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			responses := make([]func() (*RPCResponse, error), 0, len(test.responses))
			for _, body := range test.responses {
				responses = append(responses, streamResponse(body))
			}
			upstream := &regenUpstream{responses: responses}
			notes := &noteRecorder{}
			ctx := variantContext(ContextWithRegenerationBudget(notes.context(context.Background()), NewRegenerationBudget()))
			request := regenRequest("test")
			request.System = "be helpful"
			request.Tools = test.tools
			prepared, _, err := prepareToolRequest(request)
			if err != nil {
				t.Fatal(err)
			}
			events, err := guardClient(t, "test", upstream).Generate(ctx, prepared)
			if err != nil {
				t.Fatal(err)
			}
			got := collectEvents(t, events)
			var call *FunctionCall
			for _, event := range got {
				if event.Kind == EventToolCall {
					call = event.ToolCall
				}
				if event.Kind == EventError {
					t.Fatalf("不应以错误结束：%v", event.Err)
				}
			}
			if call == nil || call.Name != name || !LocalCallID(call.ID) || string(call.Arguments) != `{"task":"go"}` {
				t.Fatalf("交出的调用 %+v，期望原名 %s 与本地调用 ID", call, name)
			}
			if upstream.calls() != test.sends {
				t.Fatalf("发送 %d 次，期望 %d：%v", upstream.calls(), test.sends, notes.notes)
			}
			for index, body := range upstream.bodies {
				if strings.Contains(body, name) || !strings.Contains(body, `"`+wire+`"`) {
					t.Fatalf("第 %d 次请求体应只有截短名: %.600s", index+1, body)
				}
			}
			if test.tools.ToolConfig.Mode == "required" {
				hint := "You must call the function " + wire + " in this response"
				if !strings.Contains(upstream.bodies[0], hint) {
					t.Fatalf("系统指令里的工具要求应写截短名: %.900s", upstream.bodies[0])
				}
				if test.sends == 2 && strings.Count(upstream.bodies[1], hint) != 2 {
					t.Fatalf("重新生成时系统指令与用户消息里的工具要求都应写截短名: %.900s", upstream.bodies[1])
				}
			}
		})
	}
}

// TestGenerateLongFunctionNameContinuation long- 续写：第一轮正文写满单轮上限，第二轮（续写请求同样只发截短名）以截短名调用，交出的调用为原名
func TestGenerateLongFunctionNameContinuation(t *testing.T) {
	name := longFunctionPrefix + "command"
	wire := wireFunctionName(name)
	model, defaults := longPlainModel("plain-long")
	model.Capabilities["function_declarations"] = true
	forgetPrefillRejection(t, model.ID)
	upstream := &longUpstream{streams: []string{
		longStream(longTextFrame("第一段", false), longFinishFrame(2, "", 10, 100, 110)),
		longStream(longToolFrame(wire), longFinishFrame(1, "", 20, 5, 25)),
	}}
	client := longClient(t, upstream, model, defaults)
	request := longRequest(model.ID, true)
	request.LongOutputDefaultTokens = 1000
	request.Tools = longFunctionTools("auto", "")
	events := runLongEvents(t, client, context.Background(), request, nil)
	calls := upstream.snapshot()
	summary := summarizeLong(events)
	if len(calls) != 2 || summary.tools != 1 || len(summary.errs) != 0 || summary.text != "第一段" {
		t.Fatalf("calls=%d summary=%+v，期望续写一轮、交出一个调用", len(calls), summary)
	}
	for _, event := range events {
		if event.Kind == EventToolCall && (event.ToolCall == nil || event.ToolCall.Name != name || !LocalCallID(event.ToolCall.ID)) {
			t.Fatalf("续写轮交出的调用 %+v，期望原名 %s 与本地调用 ID", event.ToolCall, name)
		}
	}
	for index, call := range calls {
		body, _ := json.Marshal(call.body)
		if strings.Contains(string(body), name) || !strings.Contains(string(body), `"`+wire+`"`) {
			t.Fatalf("第 %d 轮请求体应只有截短名: %.400s", index+1, body)
		}
	}
}

// TestGenerateLongFunctionNameBuild 经 Client.Generate 走 Build 通道：声明与 allowedFunctionNames 为截短名，上游以截短名调用时交出的调用为原名，
// 工具约束（forward）按原名核对通过。流式与单次（只限付费权益的模型经 ProxyUnaryCall 一次返回，见 buildUsesUnary）两条解码路径都换回原名
func TestGenerateLongFunctionNameBuild(t *testing.T) {
	name := longFunctionPrefix + "command"
	wire := wireFunctionName(name)
	call := `{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"` + wire + `","args":{"task":"go"}}}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":2,"totalTokenCount":5}}`
	for _, test := range []struct {
		name  string
		unary bool
	}{{name: "流式"}, {name: "单次", unary: true}} {
		t.Run(test.name, func(t *testing.T) {
			response := guardBuildStream(t, call)
			if test.unary {
				// ProxyUnaryCall 的响应为 ProxyResponse：field 1 为 generateContent 的 JSON 字符串
				encoded, err := json.Marshal(call)
				if err != nil {
					t.Fatal(err)
				}
				response = "[" + string(encoded) + "]"
			}
			upstream := &regenUpstream{responses: []func() (*RPCResponse, error){streamResponse(response)}}
			client := guardClient(t, "test", upstream)
			if test.unary {
				catalog := client.catalogs["alice@example.com"]
				entry := catalog.entries["test"]
				entry.model.AccessModes = []int64{int64(BenefitTierUltra)}
				catalog.models = []Model{entry.model}
				catalog.entries = map[string]modelEntry{"test": entry}
				client.catalogs["alice@example.com"] = catalog
				if !buildUsesUnary(entry.model) {
					t.Fatal("只限付费权益的模型在 Build 通道应走单次调用")
				}
			}
			notes := &noteRecorder{}
			var infos []string
			ctx := ContextWithRegenerationBudget(notes.context(context.Background()), NewRegenerationBudget())
			ctx = ContextWithRequestInfoObserver(ctx, func(message string) { infos = append(infos, message) })
			ctx = ContextWithAccountLease(ctx, &AccountLease{channel: ChannelBuild, account: &Account{ID: "alice@example.com"}})
			events, err := client.Generate(ctx, guardRequest(t, longFunctionTools("required", name)))
			if err != nil {
				t.Fatal(err)
			}
			got := collectEvents(t, events)
			var received *FunctionCall
			for _, event := range got {
				if event.Kind == EventToolCall {
					received = event.ToolCall
				}
				if event.Kind == EventError {
					t.Fatalf("不应以错误结束：%v", event.Err)
				}
			}
			if received == nil || received.Name != name {
				t.Fatalf("交出的调用 %+v，期望原名 %s", received, name)
			}
			body := upstream.bodies[0]
			if strings.Contains(body, name) || !strings.Contains(body, `\"allowedFunctionNames\":[\"`+wire+`\"]`) || !strings.Contains(body, `\"name\":\"`+wire+`\"`) {
				t.Fatalf("Build 请求体应只有截短名: %.900s", body)
			}
			if unary := strings.Contains(body, ":generateContent"); unary != test.unary {
				t.Fatalf("单次调用=%v，期望 %v: %.300s", unary, test.unary, body)
			}
			if want := "函数名超过 63 字节，按截短名发送（上游返回的调用换回原名） | 函数=1 个 | " + wire + "=" + name; !reflect.DeepEqual(infos, []string{want}) {
				t.Fatalf("请求日志应有一行截短名对照：%q", infos)
			}
		})
	}
}
