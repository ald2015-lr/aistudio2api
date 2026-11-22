package api

import (
	"slices"
	"strings"
	"testing"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

func anthropicToolsRequest(t *testing.T, tools string) (anthropicRequest, error) {
	t.Helper()
	var request anthropicRequest
	decodeInto(t, `{"model":"m","max_tokens":16,"messages":[{"role":"user","content":"hi"}],"tools":[`+tools+`]}`, &request)
	_, err := request.toGenerateRequest("id")
	return request, err
}

// TestAnthropicWebSearchDocumentedFields web_search 的文档化字段都被接受：域名过滤与位置映射为搜索偏好，max_uses 与未知字段忽略
func TestAnthropicWebSearchDocumentedFields(t *testing.T) {
	var request anthropicRequest
	decodeInto(t, `{"model":"m","max_tokens":16,"messages":[{"role":"user","content":"hi"}],"tools":[
		{"type":"web_search_20250305","name":"web_search","max_uses":5,"cache_control":{"type":"ephemeral"},
			"blocked_domains":["spam.example"," ",""],"user_location":{"type":"approximate","city":"Paris","timezone":"Europe/Paris"},
			"defer_loading":false,"future_option":{"x":1}}]}`, &request)
	generate, err := request.toGenerateRequest("id")
	if err != nil {
		t.Fatalf("web_search 的文档化字段与未知字段应被接受: %v", err)
	}
	search := generate.Tools.GoogleSearch
	if search == nil || !search.WebSearch || !slices.Equal(search.BlockedDomains, []string{"spam.example"}) || len(search.AllowedDomains) != 0 ||
		!strings.Contains(string(search.UserLocation), "Paris") {
		t.Fatalf("搜索偏好=%+v", search)
	}
	if !slices.Contains(generate.Tools.Google, "google_search") {
		t.Fatalf("应启用 Google Search: %v", generate.Tools.Google)
	}
}

// TestAnthropicWebSearchDomainConflicts allowed_domains 与 blocked_domains 同时使用、或类型不对时与 Anthropic 一样返回 400
func TestAnthropicWebSearchDomainConflicts(t *testing.T) {
	for name, tool := range map[string]string{
		"同时使用":   `{"type":"web_search_20250305","name":"web_search","allowed_domains":["go.dev"],"blocked_domains":["spam.example"]}`,
		"域名不是数组": `{"type":"web_search_20250305","name":"web_search","allowed_domains":"go.dev"}`,
		"位置不是对象": `{"type":"web_search_20250305","name":"web_search","user_location":"Paris"}`,
	} {
		if _, err := anthropicToolsRequest(t, tool); err == nil {
			t.Fatalf("%s 应返回错误", name)
		}
	}
	// 一侧为空列表时不算同时使用
	if _, err := anthropicToolsRequest(t, `{"type":"web_search_20250305","name":"web_search","allowed_domains":["go.dev"],"blocked_domains":[]}`); err != nil {
		t.Fatalf("空的 blocked_domains 不应与 allowed_domains 冲突: %v", err)
	}
}

// TestAnthropicServerToolsIgnoreExtraFields 其他 server tool 的文档化字段与未知字段忽略；只有无法遵守的抓取范围限制返回 400
func TestAnthropicServerToolsIgnoreExtraFields(t *testing.T) {
	var request anthropicRequest
	decodeInto(t, `{"model":"m","max_tokens":16,"messages":[{"role":"user","content":"hi"}],"tools":[
		{"type":"web_fetch_20250910","name":"web_fetch","max_uses":3,"citations":{"enabled":true},"max_content_tokens":4096,"allowed_domains":[],"cache_control":{"type":"ephemeral"}},
		{"type":"code_execution_20250825","name":"code_execution","future_option":true}]}`, &request)
	generate, err := request.toGenerateRequest("id")
	if err != nil {
		t.Fatalf("server tool 的多余字段应忽略: %v", err)
	}
	if !slices.Contains(generate.Tools.Google, "url_context") || !slices.Contains(generate.Tools.Google, "code_execution") {
		t.Fatalf("工具=%v", generate.Tools.Google)
	}
	for _, field := range []string{"allowed_domains", "blocked_domains"} {
		_, err := anthropicToolsRequest(t, `{"type":"web_fetch_20250910","name":"web_fetch","`+field+`":["go.dev"]}`)
		if err == nil || !strings.Contains(err.Error(), field) {
			t.Fatalf("web_fetch 的 %s 无法遵守，应返回 400，得到 %v", field, err)
		}
	}
	// name 与 custom tool 字段仍按原规则校验
	if _, err := anthropicToolsRequest(t, `{"type":"web_search_20250305","name":"search"}`); err == nil {
		t.Fatal("server tool 的 name 不对应返回错误")
	}
	if _, err := anthropicToolsRequest(t, `{"type":"code_execution_20250825","name":"code_execution","description":"run"}`); err == nil {
		t.Fatal("server tool 带 description 应返回错误")
	}
}

// TestAnthropicServerToolChoice tool_choice 指定 server tool 时仍为必须调用，只开启该工具对应的 Google 工具（web_search 保留它的搜索偏好），
// 不声明函数，生成服务的工具选择校验通过（原先按“引用了未声明函数”在选号前返回 400）；指定客户端函数时仍只允许该函数（与上游
// Mag1cFall/AIStudio2API 886e0c0 的测试相同，去掉我们不支持的 web_fetch 域名限制）
func TestAnthropicServerToolChoice(t *testing.T) {
	const search = `{"type":"web_search_20250305","name":"web_search","max_uses":1,"user_location":{"type":"approximate","city":"Paris"}}`
	const tools = search + `,{"type":"web_fetch_20250910","name":"web_fetch"},{"type":"code_execution_20250825","name":"code_execution"},` +
		`{"type":"image_search","name":"image_search"},{"type":"google_maps","name":"google_maps"},{"name":"Read","input_schema":{"type":"object"}}`
	for _, test := range []struct {
		name, tools, choice string
		google, functions   []string
		allowed             []string
		search              bool
	}{
		{"web_search", search, "web_search", []string{"google_search"}, nil, nil, true},
		{"web_search with functions", tools, "web_search", []string{"google_search"}, nil, nil, true},
		{"web_fetch with functions", tools, "web_fetch", []string{"url_context"}, nil, nil, false},
		{"code_execution with functions", tools, "code_execution", []string{"code_execution"}, nil, nil, false},
		{"image_search with functions", tools, "image_search", []string{"image_search"}, nil, nil, false},
		{"google_maps with functions", tools, "google_maps", []string{"google_maps"}, nil, nil, false},
		{"function", tools, "Read", []string{"google_search", "url_context", "code_execution", "image_search", "google_maps"}, []string{"Read"}, []string{"Read"}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var request anthropicRequest
			decodeInto(t, `{"model":"m","max_tokens":400,"messages":[{"role":"user","content":"x"}],"tools":[`+test.tools+`],"tool_choice":{"type":"tool","name":"`+test.choice+`","disable_parallel_tool_use":true}}`, &request)
			got, err := request.toGenerateRequest("id")
			if err != nil {
				t.Fatal(err)
			}
			var functions []string
			for _, declaration := range got.Tools.Functions {
				functions = append(functions, declaration.Name)
			}
			config := got.Tools.ToolConfig
			if config.Mode != "required" || !slices.Equal(config.AllowedFunctionNames, test.allowed) || !slices.Equal(got.Tools.Google, test.google) ||
				!slices.Equal(functions, test.functions) || (got.Tools.GoogleSearch != nil) != test.search {
				t.Fatalf("tools=%+v functions=%v", got.Tools, functions)
			}
			if config.ParallelCalls == nil || *config.ParallelCalls {
				t.Fatalf("disable_parallel_tool_use 丢了：%+v", config)
			}
			if err := aistudio.ValidateToolChoice(got.Tools); err != nil {
				t.Fatalf("生成服务的工具选择校验不通过（会在选号前返回 400）: %v", err)
			}
		})
	}
	// 指定的名称既不是 server tool 也不是声明的函数：照旧由工具选择校验拒绝
	var request anthropicRequest
	decodeInto(t, `{"model":"m","max_tokens":16,"messages":[{"role":"user","content":"x"}],"tools":[`+search+`],"tool_choice":{"type":"tool","name":"web_fetch"}}`, &request)
	got, err := request.toGenerateRequest("id")
	if err != nil {
		t.Fatal(err)
	}
	if err := aistudio.ValidateToolChoice(got.Tools); err == nil {
		t.Fatalf("没有声明的 web_fetch 不应通过校验：%+v", got.Tools)
	}
}
