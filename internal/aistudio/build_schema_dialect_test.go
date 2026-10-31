package aistudio

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// newAPIOpenAPISchema 为 new-api 把 OpenAI 的 parameters 转成 Gemini functionDeclarations[].parameters 后的写法：
// 类型名大写，["integer","null"] 改成 nullable，字符串枚举带 format: enum
const newAPIOpenAPISchema = `{"type":"OBJECT","properties":{"query":{"type":"STRING","description":"关键词"},` +
	`"limit":{"type":"INTEGER","nullable":true,"format":"int32","minimum":1},` +
	`"tags":{"type":"ARRAY","items":{"type":"STRING","format":"enum","enum":["news","tech"]}},` +
	`"filter":{"anyOf":[{"type":"STRING"},{"type":"NUMBER"}],"nullable":true},` +
	`"meta":{"type":"OBJECT","properties":{"lang":{"type":"TYPE_UNSPECIFIED"}}},` +
	`"unit":{"type":"STRING","enum":["c","f"],"nullable":true},` +
	`"mode":{"type":"STRING","format":"enum","enum":["a",null],"nullable":true}},"required":["query"]}`

// TestOpenAPISchemaAsJSONSchema OpenAPI 方言转成 JSON Schema：类型名改小写，nullable 改为类型数组加 null（没有 type 时给 anyOf 加 null 分支，
// 带 enum 时给 enum 补 null、已有时不重复），format: enum 去掉、其余 format 保留，TYPE_UNSPECIFIED 去掉，数值写法不变；本来就是 JSON Schema 时原样返回（逐字节）
func TestOpenAPISchemaAsJSONSchema(t *testing.T) {
	converted := openAPISchemaAsJSONSchema(json.RawMessage(newAPIOpenAPISchema))
	var got, want any
	if err := json.Unmarshal(converted, &got); err != nil {
		t.Fatal(err)
	}
	_ = json.Unmarshal([]byte(`{"type":"object","properties":{"query":{"type":"string","description":"关键词"},`+
		`"limit":{"type":["integer","null"],"format":"int32","minimum":1},`+
		`"tags":{"type":"array","items":{"type":"string","enum":["news","tech"]}},`+
		`"filter":{"anyOf":[{"type":"string"},{"type":"number"},{"type":"null"}]},`+
		`"meta":{"type":"object","properties":{"lang":{}}},`+
		`"unit":{"type":["string","null"],"enum":["c","f",null]},`+
		`"mode":{"type":["string","null"],"enum":["a",null]}},"required":["query"]}`), &want)
	gotJSON, _ := json.Marshal(got)
	wantJSON, _ := json.Marshal(want)
	if !bytes.Equal(gotJSON, wantJSON) {
		t.Fatalf("转换结果 = %s\n期望 %s", gotJSON, wantJSON)
	}
	for _, raw := range []string{
		`{"type":"object","properties":{"host":{"type":"string","maxLength":5.0}},"required":["host"]}`,
		`{"type":"object","properties":{"n":{"type":["integer","null"]}}}`,
		`{"type":"object"}`,
		`not json`,
	} {
		if result := openAPISchemaAsJSONSchema(json.RawMessage(raw)); string(result) != raw {
			t.Fatalf("本来就是 JSON Schema 时应原样返回: %s → %s", raw, result)
		}
	}
}

// TestBuildFunctionDeclarationsUseJSONSchema Build 通道发送的 parametersJsonSchema 是转换后的 JSON Schema（不再带大写类型与 nullable）；
// Playground 通道照旧由 Schema 编码处理 OpenAPI 写法
func TestBuildFunctionDeclarationsUseJSONSchema(t *testing.T) {
	request := safetyTestRequest(nil)
	request.Tools = Tools{Functions: []FunctionDeclaration{{Name: "search_web", Parameters: json.RawMessage(newAPIOpenAPISchema)}}}
	_, raw, err := EncodeBuildGenerateRequest(request, GenerationDefaults{MaxOutputTokens: 1024}, false, false)
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)
	for _, unwanted := range []string{`"OBJECT"`, `"STRING"`, `"INTEGER"`, `"nullable"`, `"TYPE_UNSPECIFIED"`, `"format":"enum"`} {
		if strings.Contains(body, unwanted) {
			t.Fatalf("Build 请求里不应再有 %s: %s", unwanted, body)
		}
	}
	if !strings.Contains(body, `"parametersJsonSchema":{`) || !strings.Contains(body, `"type":["integer","null"]`) {
		t.Fatalf("Build 请求应发送转换后的 parametersJsonSchema: %s", body)
	}
	// Playground：大写类型与 nullable 照旧由 encodeJSONSchema 编码（类型码 6 为 object）
	declaration, err := encodeFunctionDeclaration(FunctionDeclaration{Name: "search_web", Parameters: json.RawMessage(newAPIOpenAPISchema)})
	if err != nil {
		t.Fatalf("Playground 编码失败: %v", err)
	}
	encoded, _ := json.Marshal(declaration)
	if !strings.Contains(string(encoded), `"search_web"`) {
		t.Fatalf("Playground 声明 = %s", encoded)
	}
}
