package api

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

func decodeInto(t *testing.T, raw string, target any) {
	t.Helper()
	if err := json.Unmarshal([]byte(raw), target); err != nil {
		t.Fatal(err)
	}
}

// TestOpenAIToolChoiceCompatibility Chat 的 required、指定函数、strict、parallel_tool_calls=false、web_search_options 不再返回 400
func TestOpenAIToolChoiceCompatibility(t *testing.T) {
	var request chatRequest
	decodeInto(t, `{"model":"m","messages":[{"role":"user","content":"hi"}],
		"tools":[{"type":"function","function":{"name":"get_weather","strict":true,"parameters":{"type":"object","properties":{"city":{"type":"string"}}}}}],
		"tool_choice":{"type":"function","function":{"name":"get_weather"}},"parallel_tool_calls":false,
		"web_search_options":{"search_context_size":"low","user_location":{"type":"approximate"}}}`, &request)
	generate, err := request.toGenerateRequest("id")
	if err != nil {
		t.Fatal(err)
	}
	config := generate.Tools.ToolConfig
	if config.Mode != "required" || !slices.Equal(config.AllowedFunctionNames, []string{"get_weather"}) ||
		config.ParallelCalls == nil || *config.ParallelCalls || !generate.Tools.Functions[0].Strict {
		t.Fatalf("tool config=%+v functions=%+v", config, generate.Tools.Functions)
	}
	if search := generate.Tools.GoogleSearch; search == nil || search.ContextSize != "low" || len(search.UserLocation) == 0 {
		t.Fatalf("搜索偏好=%+v", generate.Tools.GoogleSearch)
	}
	if choice, err := openAIToolChoice(json.RawMessage(`"required"`)); err != nil || choice.Mode != "required" {
		t.Fatalf("required: %+v %v", choice, err)
	}
	// Responses 写法：name 与 namespace 在顶层
	if choice, err := openAIToolChoice(json.RawMessage(`{"type":"function","name":"search","namespace":"docs"}`)); err != nil || choice.AllowedFunctionNames[0] != "docs.search" {
		t.Fatalf("Responses 指定函数: %+v %v", choice, err)
	}
	if _, err := openAIToolChoice(json.RawMessage(`{"type":"function"}`)); err == nil {
		t.Fatal("缺少函数名应返回错误")
	}
}

// TestResponsesNamespaceAndOptions Responses 的命名空间函数以全名发送，输出还原命名空间；truncation=auto 被接受
func TestResponsesNamespaceAndOptions(t *testing.T) {
	var request responsesRequest
	decodeInto(t, `{"model":"m","truncation":"auto","parallel_tool_calls":false,
		"input":[{"type":"message","role":"user","content":"hi"},{"type":"function_call","call_id":"c1","namespace":"docs","name":"search","arguments":"{}"},
			{"type":"function_call_output","call_id":"c1","output":"ok"}],
		"tools":[{"type":"namespace","name":"docs","tools":[{"type":"function","name":"search","strict":true}]},
			{"type":"namespace","name":"code","tools":[{"type":"function","name":"search"}]},
			{"type":"web_search","search_context_size":"high","filters":{"allowed_domains":["go.dev"]}}]}`, &request)
	generate, _, err := request.toGenerateRequest("id")
	if err != nil {
		t.Fatal(err)
	}
	if !generate.Truncate || generate.Tools.ToolConfig.ParallelCalls == nil || *generate.Tools.ToolConfig.ParallelCalls {
		t.Fatalf("truncate=%v tool config=%+v", generate.Truncate, generate.Tools.ToolConfig)
	}
	names := []string{generate.Tools.Functions[0].Name, generate.Tools.Functions[1].Name}
	if !slices.Equal(names, []string{"docs.search", "code.search"}) || !generate.Tools.Functions[0].Strict {
		t.Fatalf("函数=%+v", generate.Tools.Functions)
	}
	if call := generate.Contents[1].Parts[0].FunctionCall; call == nil || call.Name != "docs.search" {
		t.Fatalf("function_call 输入=%+v", generate.Contents[1])
	}
	if search := generate.Tools.GoogleSearch; search == nil || search.ContextSize != "high" || search.AllowedDomains[0] != "go.dev" {
		t.Fatalf("搜索偏好=%+v", generate.Tools.GoogleSearch)
	}
	item := responseFunctionCall(aistudio.FunctionCall{ID: "c2", Name: "docs.search", Arguments: json.RawMessage(`{}`)}, request.Tools)
	if item["namespace"] != "docs" || item["name"] != "search" {
		t.Fatalf("输出项=%v", item)
	}
}

// TestGeminiFunctionCallingModes Gemini 的 ANY、VALIDATED 与 allowedFunctionNames 被接受
func TestGeminiFunctionCallingModes(t *testing.T) {
	groups := []geminiToolGroup{}
	decodeInto(t, `[{"functionDeclarations":[{"name":"a"},{"name":"b"}]}]`, &groups)
	for mode, want := range map[string]string{"ANY": "required", "VALIDATED": "validated", "": "auto", "NONE": "none"} {
		var config geminiToolConfig
		config.FunctionCallingConfig.Mode = mode
		config.FunctionCallingConfig.AllowedFunctionNames = []string{"b"}
		tools, err := mapGeminiTools(groups, config)
		if err != nil || tools.ToolConfig.Mode != want || tools.ToolConfig.AllowedFunctionNames[0] != "b" {
			t.Fatalf("mode %q: %+v %v", mode, tools.ToolConfig, err)
		}
	}
}

// TestAnthropicToolCompatibility Anthropic 的 any、指定工具、disable_parallel_tool_use、cache_control、strict 与 thinking disabled
func TestAnthropicToolCompatibility(t *testing.T) {
	var request anthropicRequest
	decodeInto(t, `{"model":"m","max_tokens":16,"thinking":{"type":"disabled"},
		"messages":[{"role":"user","content":"hi"}],
		"tools":[{"name":"get_weather","input_schema":{"type":"object"},"cache_control":{"type":"ephemeral"},"strict":true,"defer_loading":false},
			{"type":"web_search_20250305","name":"web_search","max_uses":3,"cache_control":{"type":"ephemeral"},"allowed_domains":["go.dev"],"user_location":{"type":"approximate","city":"Paris"}}],
		"tool_choice":{"type":"tool","name":"get_weather","disable_parallel_tool_use":true}}`, &request)
	generate, err := request.toGenerateRequest("id")
	if err != nil {
		t.Fatal(err)
	}
	config := generate.Tools.ToolConfig
	if config.Mode != "required" || config.AllowedFunctionNames[0] != "get_weather" || config.ParallelCalls == nil || *config.ParallelCalls {
		t.Fatalf("tool config=%+v", config)
	}
	if !generate.Tools.Functions[0].Strict || !generate.Config.HideThinking || generate.Config.ReasoningEffort != "none" {
		t.Fatalf("functions=%+v config=%+v", generate.Tools.Functions, generate.Config)
	}
	if search := generate.Tools.GoogleSearch; search == nil || search.AllowedDomains[0] != "go.dev" || !strings.Contains(string(search.UserLocation), "Paris") {
		t.Fatalf("搜索偏好=%+v", generate.Tools.GoogleSearch)
	}
	if choice, err := anthropicToolChoice(json.RawMessage(`{"type":"any"}`)); err != nil || choice.Mode != "required" {
		t.Fatalf("any: %+v %v", choice, err)
	}
}

// TestToolContractErrorIsBadGateway 工具约束违约按 502 返回
func TestToolContractErrorIsBadGateway(t *testing.T) {
	err := &aistudio.ToolContractError{Detail: "x"}
	if got := publicErrorFor(err, "m"); got.Status != http.StatusBadGateway {
		t.Fatalf("public error=%+v", got)
	}
	if statusFromError(err) != http.StatusBadGateway {
		t.Fatal("statusFromError 应为 502")
	}
}
