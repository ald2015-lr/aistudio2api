package aistudio

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// requiredUndefinedSchema 的 required 列出了 properties 没有定义的属性 b，嵌套对象里的 inner.y 同样没有定义
const requiredUndefinedSchema = `{"type":"object","properties":{"a":{"type":"integer","minimum":1.50},"inner":{"type":"object","properties":{"x":{"type":"string"}},"required":["x","y"]}},"required":["a","b"],"additionalProperties":false}`

// TestDefineRequiredProperties 为 required 中未定义的属性补上 {}，已定义的属性与数值写法保持原样
func TestDefineRequiredProperties(t *testing.T) {
	patched := defineRequiredProperties(json.RawMessage(requiredUndefinedSchema))
	var schema map[string]any
	if err := json.Unmarshal(patched, &schema); err != nil {
		t.Fatal(err)
	}
	properties := schema["properties"].(map[string]any)
	if definition, ok := properties["b"].(map[string]any); !ok || len(definition) != 0 {
		t.Fatalf("properties.b = %#v，期望补上 {}", properties["b"])
	}
	inner := properties["inner"].(map[string]any)["properties"].(map[string]any)
	if definition, ok := inner["y"].(map[string]any); !ok || len(definition) != 0 {
		t.Fatalf("嵌套 properties.y = %#v，期望补上 {}", inner["y"])
	}
	if !bytes.Contains(patched, []byte(`"minimum":1.50`)) {
		t.Fatalf("数值写法被改变: %s", patched)
	}

	// 只有 required 没有 properties 时补出 properties
	patched = defineRequiredProperties(json.RawMessage(`{"type":"object","required":["q"]}`))
	if !bytes.Contains(patched, []byte(`"properties":{"q":{}}`)) {
		t.Fatalf("没有 properties 时应补出: %s", patched)
	}

	// 组合与数组里的节点同样补齐
	patched = defineRequiredProperties(json.RawMessage(`{"type":"array","items":{"anyOf":[{"type":"object","required":["k"]}]}}`))
	if !bytes.Contains(patched, []byte(`"properties":{"k":{}}`)) {
		t.Fatalf("items.anyOf 中的节点应补齐: %s", patched)
	}

	// 不需要补齐、properties 写法错误、不是 JSON object 时原样返回（同一段字节，不重新编码）
	for _, raw := range []string{
		`{"type":"object", "properties":{"a":{"type":"string"}}, "required":["a"]}`,
		`{"type":"object","properties":"bad","required":["a"]}`,
		`{"type":"object","properties":{}}`,
		`[1,2]`,
	} {
		if got := defineRequiredProperties(json.RawMessage(raw)); string(got) != raw {
			t.Fatalf("%s 应原样返回，得到 %s", raw, got)
		}
	}
}

// TestDefineRequiredPropertiesRespectsLimits 大小与嵌套检查在前：超过大小上限的 Schema 不解析、原样返回；
// 位于最大层数的节点不补（补上的定义会超过嵌套上限），更浅的节点照常补齐
func TestDefineRequiredPropertiesRespectsLimits(t *testing.T) {
	oversized := json.RawMessage(`{"type":"object","description":"` + strings.Repeat("a", maxSchemaBytes) + `","required":["x"]}`)
	if got := defineRequiredProperties(oversized); !bytes.Equal(got, oversized) {
		t.Fatal("超过大小上限的 Schema 应原样返回")
	}
	if _, err := encodeFunctionDeclaration(FunctionDeclaration{Name: "big", Parameters: oversized}); err == nil || !strings.Contains(err.Error(), "字节上限") {
		t.Fatalf("超过大小上限仍应报大小错误，得到 %v", err)
	}

	nested := func(depth int) json.RawMessage {
		return json.RawMessage(strings.Repeat(`{"type":"object","properties":{"n":`, depth) +
			`{"type":"object","required":["x"]}` + strings.Repeat(`}}`, depth))
	}
	atLimit := nested(maxSchemaDepth)
	if got := defineRequiredProperties(atLimit); !bytes.Equal(got, atLimit) {
		t.Fatal("位于最大层数的节点不应补齐")
	}
	if _, err := encodeJSONSchema(atLimit); err != nil {
		t.Fatalf("最大层数内的 Schema 应能编码: %v", err)
	}
	belowLimit := defineRequiredProperties(nested(maxSchemaDepth - 1))
	if !bytes.Contains(belowLimit, []byte(`"properties":{"x":{}}`)) {
		t.Fatal("最大层数以内的节点应补齐")
	}
	if _, err := encodeJSONSchema(belowLimit); err != nil {
		t.Fatalf("补齐后不应超过嵌套上限: %v", err)
	}
	if _, err := encodeJSONSchema(defineRequiredProperties(nested(maxSchemaDepth + 1))); err == nil || !isSchemaLimitError(err) {
		t.Fatalf("超过嵌套上限仍应报嵌套错误，得到 %v", err)
	}
}

// TestRequiredUndefinedPropertyPlayground Playground 函数声明为未定义的必填属性编码出属性定义，不再降级编码
func TestRequiredUndefinedPropertyPlayground(t *testing.T) {
	wire, err := encodeFunctionDeclaration(FunctionDeclaration{Name: "lookup", Parameters: json.RawMessage(requiredUndefinedSchema)})
	if err != nil {
		t.Fatal(err)
	}
	parameters := wire[2].([]any)
	entries, _ := parameters[6].([]any)
	names := map[string]bool{}
	for _, entry := range entries {
		names[entry.([]any)[0].(string)] = true
	}
	if !names["a"] || !names["b"] || !names["inner"] {
		t.Fatalf("properties 应包含 a、b、inner，得到 %v", names)
	}
	if required, _ := parameters[7].([]string); len(required) != 2 {
		t.Fatalf("required 应保留两项，得到 %#v", parameters[7])
	}
}

// TestRequiredUndefinedPropertyBuild Build 的 parametersJsonSchema 为未定义的必填属性补上 {}；不需要补齐的 Schema 原样发送
func TestRequiredUndefinedPropertyBuild(t *testing.T) {
	request := safetyTestRequest(nil)
	request.Tools = Tools{Functions: []FunctionDeclaration{
		{Name: "lookup", Parameters: json.RawMessage(requiredUndefinedSchema)},
		{Name: "ping", Parameters: json.RawMessage(`{"type":"object","properties":{"host":{"type":"string","maxLength":5.0}},"required":["host"]}`)},
	}}
	body := encodeBuildBody(t, request, GenerationDefaults{MaxOutputTokens: 1024}, false)
	declarations := body["tools"].([]any)[0].(map[string]any)["functionDeclarations"].([]any)
	lookup := declarations[0].(map[string]any)["parametersJsonSchema"].(map[string]any)
	if definition, ok := lookup["properties"].(map[string]any)["b"].(map[string]any); !ok || len(definition) != 0 {
		t.Fatalf("Build properties.b = %#v，期望补上 {}", lookup["properties"])
	}
	_, raw, err := EncodeBuildGenerateRequest(request, GenerationDefaults{MaxOutputTokens: 1024}, false, false)
	if err != nil {
		t.Fatal(err)
	}
	// 不需要补齐的 Schema 不重新编码：整数字段的写法（5.0）也照原样发送，Build 请求形态不变
	if !bytes.Contains(raw, []byte(`{"type":"object","properties":{"host":{"type":"string","maxLength":5.0}},"required":["host"]}`)) {
		t.Fatalf("不需要补齐的 Schema 应原样发送: %s", raw)
	}
}

// TestRequiredUndefinedPropertyStrictContract strict 参数核对使用补齐后的 Schema：
// additionalProperties:false 时未定义的必填属性原先永远无法满足，补齐后带上它的参数能通过，缺少它仍判违约
func TestRequiredUndefinedPropertyStrictContract(t *testing.T) {
	tools := Tools{Functions: []FunctionDeclaration{{Name: "lookup", Strict: true, Parameters: json.RawMessage(requiredUndefinedSchema)}}}
	_, contract, err := prepareToolRequest(GenerateRequest{Tools: tools})
	if err != nil || contract == nil || contract.schemas["lookup"] == nil {
		t.Fatalf("contract=%v err=%v，期望编译出 strict Schema", contract, err)
	}
	valid := toolCall("lookup", `{"a":2,"b":"anything","inner":{"x":"1","y":[1]}}`)
	if events := runContract(t, contract, valid, Event{Kind: EventFinish}); contractViolation(events) {
		t.Fatalf("带上补齐属性的参数应通过: %+v", events)
	}
	missing := toolCall("lookup", `{"a":2,"inner":{"x":"1","y":1}}`)
	if events := runContract(t, contract, missing, Event{Kind: EventFinish}); !contractViolation(events) {
		t.Fatal("缺少必填属性 b 仍应判违约")
	}
}
