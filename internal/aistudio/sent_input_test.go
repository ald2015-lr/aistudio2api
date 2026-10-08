package aistudio

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

const strictSchemaHintPrefix = "Arguments must follow this JSON Schema: "

// TestPlaygroundSchemaHintChars 附在函数说明与结构化输出根说明里的完整 Schema 计入实际输入；能直接编码的普通 Schema 不附
func TestPlaygroundSchemaHintChars(t *testing.T) {
	parameters := json.RawMessage(`{"type": "object", "properties": {"city": {"type": "string"}}}`)
	compact := `{"type":"object","properties":{"city":{"type":"string"}}}`
	plain := GenerateRequest{Tools: Tools{Functions: []FunctionDeclaration{{Name: "get_weather", Parameters: parameters}}}}
	if chars := playgroundSchemaHintChars(plain); chars != 0 {
		t.Fatalf("普通函数不附 Schema，字数应为 0，得到 %d", chars)
	}
	strict := GenerateRequest{Tools: Tools{Functions: []FunctionDeclaration{{Name: "get_weather", Description: "天气", Parameters: parameters, Strict: true}}}}
	want := int64(utf8.RuneCountInString("\n" + strictSchemaHintPrefix + compact))
	if chars := playgroundSchemaHintChars(strict); chars != want {
		t.Fatalf("strict 函数附上的 Schema 字数 = %d，期望 %d", chars, want)
	}
	degraded := GenerateRequest{Tools: Tools{Functions: []FunctionDeclaration{{
		Name: "lookup", Parameters: json.RawMessage(`{"type":"object","properties":{"ids":{"type":"array","uniqueItems":true,"items":{"type":"string"}}}}`),
	}}}}
	if chars := playgroundSchemaHintChars(degraded); chars <= 0 {
		t.Fatalf("降级编码的函数应计入附上的 Schema，得到 %d", chars)
	}
	none := strict
	none.Tools.ToolConfig.Mode = "none"
	if chars := playgroundSchemaHintChars(none); chars != 0 {
		t.Fatalf("tool choice none 不发送函数，字数应为 0，得到 %d", chars)
	}
	response := GenerateRequest{Config: GenerationConfig{ResponseSchema: json.RawMessage(`{"type":"object","properties":{"tags":{"type":"array","uniqueItems":true,"items":{"type":"string"}}}}`)}}
	if chars := playgroundSchemaHintChars(response); chars <= 0 {
		t.Fatalf("降级的结构化输出 Schema 应计入附上的 Schema，得到 %d", chars)
	}
	encodable := GenerateRequest{Config: GenerationConfig{ResponseSchema: parameters}}
	if chars := playgroundSchemaHintChars(encodable); chars != 0 {
		t.Fatalf("能直接编码的结构化输出 Schema 不附，字数应为 0，得到 %d", chars)
	}
}

// TestReportSentInputBuildWithoutHints Build 原样发送 Schema，不计附加字数；没有观察者时不做任何事
func TestReportSentInputBuildWithoutHints(t *testing.T) {
	request := GenerateRequest{System: "s", Tools: Tools{Functions: []FunctionDeclaration{{
		Name: "get_weather", Parameters: json.RawMessage(`{"type":"object"}`), Strict: true,
	}}}}
	reportSentInput(context.Background(), ChannelPlayground, request)
	var observed []SentInput
	ctx := ContextWithSentInputObserver(context.Background(), func(input SentInput) { observed = append(observed, input) })
	reportSentInput(ctx, ChannelBuild, request)
	reportSentInput(ctx, ChannelPlayground, request)
	if len(observed) != 2 || observed[0].SchemaHintChars != 0 || observed[1].SchemaHintChars <= 0 {
		t.Fatalf("Build 不应计附加 Schema、Playground 应计入，得到 %+v", observed)
	}
}

// TestSentInputReflectsRewrites 生成请求编码前报告的输入已包含工具约束提示、只保留指定函数、截断后的对话
func TestSentInputReflectsRewrites(t *testing.T) {
	const accountID = "alice@example.com"
	model := Model{
		ID: "test", Methods: []string{"generateContent", "countTokens"}, InputTokenLimit: 100,
		Capabilities: map[string]bool{"function_declarations": true},
	}
	pool := testPoolWithAccount(t, accountID)
	if err := pool.SetCatalog(accountID, BenefitTierFree, []Model{model}); err != nil {
		t.Fatal(err)
	}
	client, _ := truncateClient(t, 5000, 10)
	client.catalogs[accountID] = modelCatalog{models: []Model{model}, entries: map[string]modelEntry{"test": {model: model}}}
	service, err := NewPooledService(pool, client)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := pool.AcquireFor(context.Background(), AccountSelection{AccountID: accountID})
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	var observed []SentInput
	ctx := ContextWithSentInputObserver(ContextWithAccountLease(context.Background(), lease), func(input SentInput) {
		observed = append(observed, input)
	})
	long := strings.Repeat("hello world ", 40)
	request := GenerateRequest{
		Model: "test", Truncate: true, System: "be brief",
		Contents: []Content{
			{Role: RoleUser, Parts: []Part{{Text: long}}},
			{Role: RoleAssistant, Parts: []Part{{Text: long}}},
			{Role: RoleUser, Parts: []Part{{Text: "latest"}}},
		},
		Tools: Tools{
			Functions: []FunctionDeclaration{
				{Name: "get_weather", Parameters: json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}}}`), Strict: true},
				{Name: "unused", Parameters: json.RawMessage(`{"type":"object"}`)},
			},
			ToolConfig: ToolConfig{Mode: "required", AllowedFunctionNames: []string{"get_weather"}},
		},
	}
	// 发送由测试替身拒绝：只关心编码前报告的输入
	_, _ = service.Generate(ctx, request)
	if len(observed) != 1 {
		t.Fatalf("应报告一次实际输入，得到 %d 次", len(observed))
	}
	input := observed[0]
	if input.Channel != ChannelPlayground || len(input.Contents) != 1 || input.Contents[0].Parts[0].Text != "latest" {
		t.Fatalf("实际输入应为截断后的最新一轮，得到 %d 条消息", len(input.Contents))
	}
	if !strings.HasPrefix(input.System, "be brief\n") || !strings.Contains(input.System, "Use one of these tools in this response: get_weather") {
		t.Fatalf("实际系统指令应包含工具约束提示: %q", input.System)
	}
	if len(input.Tools.Functions) != 1 || input.Tools.Functions[0].Name != "get_weather" {
		t.Fatalf("实际输入应只保留指定函数: %+v", input.Tools.Functions)
	}
	want := int64(utf8.RuneCountInString(strictSchemaHintPrefix + `{"type":"object","properties":{"city":{"type":"string"}}}`))
	if input.SchemaHintChars != want {
		t.Fatalf("附上的 Schema 字数 = %d，期望 %d", input.SchemaHintChars, want)
	}
}
