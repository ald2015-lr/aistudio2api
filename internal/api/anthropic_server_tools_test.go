package api

import (
	"slices"
	"strings"
	"testing"
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
