package aistudio

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func weatherTools(mode string) Tools {
	return Tools{
		Functions: []FunctionDeclaration{
			{Name: "get_weather", Parameters: json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}},"required":["city"],"additionalProperties":false}`)},
			{Name: "get_time", Parameters: json.RawMessage(`{"type":"object","properties":{}}`)},
		},
		ToolConfig: ToolConfig{Mode: mode},
	}
}

func runContract(t *testing.T, contract *toolContract, events ...Event) []Event {
	t.Helper()
	source := make(chan Event, len(events))
	for _, event := range events {
		source <- event
	}
	close(source)
	var result []Event
	for event := range contract.forward(context.Background(), source) {
		result = append(result, event)
	}
	return result
}

func toolCall(name string, arguments string) Event {
	return Event{Kind: EventToolCall, ToolCall: &FunctionCall{ID: name, Name: name, Arguments: json.RawMessage(arguments)}}
}

func contractViolation(events []Event) bool {
	for _, event := range events {
		var contract *ToolContractError
		if event.Kind == EventError && errors.As(event.Err, &contract) {
			return true
		}
	}
	return false
}

// TestPrepareToolRequestModes 必须调用、指定函数与单次调用写进系统指令，指定函数时只发送选中的函数
func TestPrepareToolRequestModes(t *testing.T) {
	request, contract, err := prepareToolRequest(GenerateRequest{System: "be brief", Tools: weatherTools("required")})
	if err != nil || contract == nil || !contract.required {
		t.Fatalf("required: contract=%+v err=%v", contract, err)
	}
	if !strings.HasPrefix(request.System, "be brief\n") || !strings.Contains(request.System, "get_weather, get_time") {
		t.Fatalf("required 提示: %q", request.System)
	}

	named := weatherTools("required")
	named.ToolConfig.AllowedFunctionNames = []string{"get_time"}
	request, _, err = prepareToolRequest(GenerateRequest{Tools: named})
	if err != nil || len(request.Tools.Functions) != 1 || request.Tools.Functions[0].Name != "get_time" {
		t.Fatalf("指定函数: functions=%+v err=%v", request.Tools.Functions, err)
	}

	unknown := weatherTools("required")
	unknown.ToolConfig.AllowedFunctionNames = []string{"missing"}
	if _, _, err := prepareToolRequest(GenerateRequest{Tools: unknown}); err == nil {
		t.Fatal("指定未声明的函数应返回错误")
	}
	if _, _, err := prepareToolRequest(GenerateRequest{Tools: Tools{ToolConfig: ToolConfig{Mode: "required"}}}); err == nil {
		t.Fatal("required 没有任何工具应返回错误")
	}

	single := weatherTools("auto")
	parallel := false
	single.ToolConfig.ParallelCalls = &parallel
	request, contract, err = prepareToolRequest(GenerateRequest{Tools: single})
	if err != nil || contract == nil || !contract.single || !strings.Contains(request.System, "at most one function") {
		t.Fatalf("单次调用: contract=%+v system=%q err=%v", contract, request.System, err)
	}

	// 默认 auto、没有 strict 时不需要契约
	if _, contract, err := prepareToolRequest(GenerateRequest{Tools: weatherTools("auto")}); err != nil || contract != nil {
		t.Fatalf("auto: contract=%+v err=%v", contract, err)
	}
	if _, contract, _ := prepareToolRequest(GenerateRequest{Tools: weatherTools("none")}); contract != nil {
		t.Fatal("none 不需要契约")
	}
}

// TestToolContractForward 核对上游返回的调用：未选择的函数、strict 参数不符、必须调用却没有调用时报错，单次调用只保留第一个
func TestToolContractForward(t *testing.T) {
	named := weatherTools("required")
	named.ToolConfig.AllowedFunctionNames = []string{"get_weather"}
	_, contract, _ := prepareToolRequest(GenerateRequest{Tools: named})
	if events := runContract(t, contract, toolCall("get_time", `{}`), Event{Kind: EventFinish}); !contractViolation(events) {
		t.Fatalf("调用未选择的函数应报错: %+v", events)
	}
	if events := runContract(t, contract, Event{Kind: EventText, Text: "sunny"}, Event{Kind: EventFinish}); !contractViolation(events) {
		t.Fatalf("没有调用工具应报错: %+v", events)
	}
	if events := runContract(t, contract, toolCall("get_weather", `{"city":"Paris"}`), Event{Kind: EventFinish}); contractViolation(events) || len(events) != 2 {
		t.Fatalf("满足约束的调用被拦截: %+v", events)
	}

	strict := weatherTools("auto")
	strict.Functions[0].Strict = true
	_, contract, _ = prepareToolRequest(GenerateRequest{Tools: strict})
	if events := runContract(t, contract, toolCall("get_weather", `{"town":"Paris"}`)); !contractViolation(events) {
		t.Fatalf("strict 参数不符应报错: %+v", events)
	}
	if events := runContract(t, contract, toolCall("get_weather", `{"city":"Paris"}`)); contractViolation(events) {
		t.Fatalf("strict 参数正确被拦截: %+v", events)
	}

	single := weatherTools("auto")
	parallel := false
	single.ToolConfig.ParallelCalls = &parallel
	_, contract, _ = prepareToolRequest(GenerateRequest{Tools: single})
	events := runContract(t, contract, toolCall("get_weather", `{"city":"Paris"}`), toolCall("get_time", `{}`), Event{Kind: EventFinish})
	if contractViolation(events) || len(events) != 2 || events[0].ToolCall.Name != "get_weather" {
		t.Fatalf("单次调用应只保留第一个: %+v", events)
	}

	// 必须调用且声明了搜索：上游用了搜索也算满足
	search := weatherTools("required")
	search.Google = []string{"google_search"}
	_, contract, _ = prepareToolRequest(GenerateRequest{Tools: search})
	if events := runContract(t, contract, Event{Kind: EventGrounding}, Event{Kind: EventFinish}); contractViolation(events) {
		t.Fatalf("使用搜索应满足 required: %+v", events)
	}
}

// TestValidatedModeChecksAllFunctions Gemini VALIDATED 对所有函数做参数校验；无法编译的 Schema 不阻止请求
func TestValidatedModeChecksAllFunctions(t *testing.T) {
	tools := weatherTools("validated")
	tools.Functions = append(tools.Functions, FunctionDeclaration{Name: "broken", Parameters: json.RawMessage(`{"$ref":"https://example.com/x.json"}`)})
	request, contract, err := prepareToolRequest(GenerateRequest{Tools: tools})
	if err != nil || contract == nil || contract.schemas["get_weather"] == nil || contract.schemas["broken"] != nil {
		t.Fatalf("contract=%+v err=%v", contract, err)
	}
	for _, declaration := range request.Tools.Functions {
		if !declaration.Strict {
			t.Fatalf("validated 时 %s 应为 strict", declaration.Name)
		}
	}
}

// TestSearchOptionsBecomeHints 搜索偏好以系统指令提示传给上游
func TestSearchOptionsBecomeHints(t *testing.T) {
	request, _, err := prepareToolRequest(GenerateRequest{Tools: Tools{
		Google:       []string{"google_search"},
		GoogleSearch: &GoogleSearchOptions{WebSearch: true, ContextSize: "high", AllowedDomains: []string{"go.dev"}},
	}})
	if err != nil || !strings.Contains(request.System, "high search context") || !strings.Contains(request.System, "go.dev") {
		t.Fatalf("system=%q err=%v", request.System, err)
	}
}

// TestFunctionDeclarationSchemaFallback Playground 无法编码的参数 Schema 降级为层级与类型，完整 Schema 附在说明里
func TestFunctionDeclarationSchemaFallback(t *testing.T) {
	raw := `{"type":"object","$defs":{"id":{"type":"string"}},"properties":{"id":{"$ref":"#/$defs/id"},"tags":{"type":"array","uniqueItems":true,"items":{"type":"string"}},"count":{"const":3},"level":{"enum":[1,2,3]}},"required":["id"]}`
	wire, err := encodeFunctionDeclaration(FunctionDeclaration{Name: "lookup", Description: "Find a record", Parameters: json.RawMessage(raw)})
	if err != nil {
		t.Fatalf("降级编码失败: %v", err)
	}
	description, _ := wire[1].(string)
	if !strings.HasPrefix(description, "Find a record\nArguments must follow this JSON Schema: ") || !strings.Contains(description, `"$defs"`) {
		t.Fatalf("说明没有附上完整 Schema: %q", description)
	}
	encoded, _ := json.Marshal(wire[2])
	if !strings.Contains(string(encoded), `"count"`) || !strings.Contains(string(encoded), `"tags"`) {
		t.Fatalf("降级后的参数结构缺少属性: %s", encoded)
	}

	// 可直接编码的 Schema 不附说明；strict 时附上
	plain := FunctionDeclaration{Name: "ping", Parameters: json.RawMessage(`{"type":"object","properties":{"host":{"type":"string"}}}`)}
	wire, err = encodeFunctionDeclaration(plain)
	if err != nil || wire[1] != nil {
		t.Fatalf("普通 Schema: wire=%v err=%v", wire, err)
	}
	plain.Strict = true
	wire, _ = encodeFunctionDeclaration(plain)
	if description, _ := wire[1].(string); !strings.HasPrefix(description, "Arguments must follow this JSON Schema: ") {
		t.Fatalf("strict 说明: %q", description)
	}

	// 超过嵌套上限仍直接报错
	deep := strings.Repeat(`{"type":"array","items":`, maxSchemaDepth+2) + `{"type":"string"}` + strings.Repeat(`}`, maxSchemaDepth+2)
	if _, err := encodeFunctionDeclaration(FunctionDeclaration{Name: "deep", Parameters: json.RawMessage(deep)}); err == nil {
		t.Fatal("超过嵌套上限应返回错误")
	}
}

// TestBuildFunctionCallingConfig Build 通道按原生 functionCallingConfig 执行必须调用、指定函数与 strict
func TestBuildFunctionCallingConfig(t *testing.T) {
	if calling := buildFunctionCallingConfig(weatherTools("auto")); calling != nil {
		t.Fatalf("auto 不应发送: %v", calling)
	}
	named := weatherTools("required")
	named.ToolConfig.AllowedFunctionNames = []string{"get_weather"}
	calling := buildFunctionCallingConfig(named)
	if calling["mode"] != "ANY" || len(calling["allowedFunctionNames"].([]string)) != 1 {
		t.Fatalf("required: %v", calling)
	}
	strict := weatherTools("auto")
	strict.Functions[1].Strict = true
	if calling := buildFunctionCallingConfig(strict); calling["mode"] != "VALIDATED" {
		t.Fatalf("strict: %v", calling)
	}
	if calling := buildFunctionCallingConfig(weatherTools("none")); calling != nil {
		t.Fatalf("none 不应发送: %v", calling)
	}
}

// TestNextConversationTurn 截断只在用户轮边界进行，工具结果不与调用拆开
func TestNextConversationTurn(t *testing.T) {
	contents := []Content{
		{Role: RoleUser, Parts: []Part{{Text: "q1"}}},
		{Role: RoleAssistant, Parts: []Part{{FunctionCall: &FunctionCall{Name: "f"}}}},
		{Role: RoleUser, Parts: []Part{{FunctionResult: &FunctionResult{Name: "f"}}}},
		{Role: RoleAssistant, Parts: []Part{{Text: "a1"}}},
		{Role: RoleUser, Parts: []Part{{Text: "q2"}}},
	}
	if got := nextConversationTurn(contents); got != 4 {
		t.Fatalf("下一轮位置 = %d，期望 4", got)
	}
	if got := nextConversationTurn(contents[4:]); got != -1 {
		t.Fatalf("只剩最后一轮时应返回 -1，得到 %d", got)
	}
}
