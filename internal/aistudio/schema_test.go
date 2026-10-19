package aistudio

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func nestedAnyOfSchema(depth int) json.RawMessage {
	return json.RawMessage(strings.Repeat(`{"anyOf":[`, depth) + `{"type":"string","const":"x"}` + strings.Repeat(`]}`, depth))
}

// TestEncodeJSONSchemaNestedAnyOfIsFast 嵌套 anyOf 的转换耗时不再随层数按立方增长
func TestEncodeJSONSchemaNestedAnyOfIsFast(t *testing.T) {
	started := time.Now()
	wire, err := encodeJSONSchema(nestedAnyOfSchema(maxSchemaDepth))
	if err != nil {
		t.Fatal(err)
	}
	if len(wire) == 0 {
		t.Fatal("转换结果为空")
	}
	// 旧实现在 64 层时约 0.1 秒、300 层时 2.4 秒；单层规范化后应在毫秒级
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("嵌套 %d 层耗时 %s", maxSchemaDepth, elapsed)
	}
}

// TestEncodeJSONSchemaRejectsExcessiveDepth 超过嵌套上限时返回明确错误
func TestEncodeJSONSchemaRejectsExcessiveDepth(t *testing.T) {
	_, err := encodeJSONSchema(nestedAnyOfSchema(maxSchemaDepth + 1))
	if err == nil || !strings.Contains(err.Error(), "嵌套超过") {
		t.Fatalf("期望嵌套层数错误，得到 %v", err)
	}
	deepItems := json.RawMessage(strings.Repeat(`{"type":"array","items":`, maxSchemaDepth+1) + `{"type":"string"}` + strings.Repeat(`}`, maxSchemaDepth+1))
	if _, err := encodeJSONSchema(deepItems); err == nil {
		t.Fatal("items 嵌套超过上限时应返回错误")
	}
}

// TestEncodeJSONSchemaRejectsOversizedSchema 超过大小上限时直接拒绝
func TestEncodeJSONSchemaRejectsOversizedSchema(t *testing.T) {
	description := strings.Repeat("a", maxSchemaBytes)
	_, err := encodeJSONSchema(json.RawMessage(`{"type":"string","description":"` + description + `"}`))
	if err == nil || !strings.Contains(err.Error(), "字节上限") {
		t.Fatalf("期望大小上限错误，得到 %v", err)
	}
}

// TestEncodeJSONSchemaNormalizesNestedConst 嵌套组合中的 const 与说明字段仍被规范化
func TestEncodeJSONSchemaNormalizesNestedConst(t *testing.T) {
	wire, err := encodeJSONSchema(json.RawMessage(`{"anyOf":[{"anyOf":[{"const":"a","title":"A"}]},{"type":"null"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(wire)
	if !strings.Contains(string(encoded), `["a"]`) {
		t.Fatalf("嵌套 const 没有转换为 enum: %s", encoded)
	}
}

func mustEncodeSchema(t *testing.T, raw string) string {
	t.Helper()
	wire, err := encodeJSONSchema(json.RawMessage(raw))
	if err != nil {
		t.Fatalf("编码 %s 失败: %v", raw, err)
	}
	encoded, _ := json.Marshal(wire)
	return string(encoded)
}

// TestEncodeJSONSchemaIgnoresNullFields 可选字段写成显式 null 时按缺省处理
func TestEncodeJSONSchemaIgnoresNullFields(t *testing.T) {
	for _, field := range []string{
		"format", "description", "nullable", "enum", "items", "properties", "required",
		"minItems", "maxItems", "minProperties", "maxProperties", "minimum", "maximum",
		"minLength", "maxLength", "pattern", "example", "oneOf", "anyOf", "allOf", "not",
		"default", "$defs", "examples",
	} {
		if got := mustEncodeSchema(t, `{"type":"string","`+field+`":null}`); got != `[1]` {
			t.Fatalf("%s:null 编码为 %s", field, got)
		}
	}
	// 数组 items:null 视为缺省，补默认元素类型
	if got := mustEncodeSchema(t, `{"type":"array","items":null}`); got != `[5,null,null,null,null,[1]]` {
		t.Fatalf("items:null 编码为 %s", got)
	}
	// const:null 仍按不支持的常量报错
	if _, err := encodeJSONSchema(json.RawMessage(`{"const":null}`)); err == nil {
		t.Fatal("const:null 应返回错误")
	}
}

// TestEncodeJSONSchemaTopLevelNull 顶层为空或 null 时按没有参数的对象编码
func TestEncodeJSONSchemaTopLevelNull(t *testing.T) {
	for _, raw := range []string{``, `null`, "  \n"} {
		if got := mustEncodeSchema(t, raw); got != `[6]` {
			t.Fatalf("%q 编码为 %s", raw, got)
		}
	}
}

// TestEncodeJSONSchemaNotVariants not 的各种写法被规范化为可编码形式
func TestEncodeJSONSchemaNotVariants(t *testing.T) {
	for _, not := range []string{`null`, `false`, `true`, `{}`, `{"description":null}`, `""`} {
		if got := mustEncodeSchema(t, `{"type":"string","not":`+not+`}`); got != `[1]` {
			t.Fatalf("not:%s 编码为 %s", not, got)
		}
	}
	if got := mustEncodeSchema(t, `{"type":"string","not":{"type":"null"}}`); got != `[1,null,null,false]` {
		t.Fatalf("not:{type:null} 编码为 %s", got)
	}
	excluded := `[1,null,null,null,null,null,null,null,null,null,null,null,null,null,null,null,null,null,null,[1,null,null,null,["a","b"]]]`
	if got := mustEncodeSchema(t, `{"type":"string","not":["a","b"]}`); got != excluded {
		t.Fatalf("not 数组编码为 %s", got)
	}
	single := `[1,null,null,null,null,null,null,null,null,null,null,null,null,null,null,null,null,null,null,[1,null,null,null,["a"]]]`
	if got := mustEncodeSchema(t, `{"type":"string","not":"a"}`); got != single {
		t.Fatalf("not 字符串编码为 %s", got)
	}
	if got := mustEncodeSchema(t, `{"type":"string","not":{"enum":["a"]}}`); got != single {
		t.Fatalf("not 对象编码为 %s", got)
	}
}

// TestEncodeJSONSchemaBooleanSubschemas items 与 properties 中的布尔 schema 被接受
func TestEncodeJSONSchemaBooleanSubschemas(t *testing.T) {
	// items:true 不限制元素，按默认类型编码
	if got := mustEncodeSchema(t, `{"type":"array","items":true}`); got != `[5,null,null,null,null,[1]]` {
		t.Fatalf("items:true 编码为 %s", got)
	}
	// items:false 不允许元素：仍带元素定义，并限制 maxItems=0
	got := mustEncodeSchema(t, `{"type":"array","items":false}`)
	if got != `[5,null,null,null,null,[1],null,null,null,null,null,null,null,null,null,null,null,null,null,null,0]` {
		t.Fatalf("items:false 编码为 %s", got)
	}
	// 封闭元组：items:false 只禁止 prefixItems 之外的元素
	got = mustEncodeSchema(t, `{"type":"array","prefixItems":[{"type":"string"},{"type":"integer"}],"items":false}`)
	if !strings.HasSuffix(got, `,2]`) || !strings.Contains(got, `[1]`) || !strings.Contains(got, `[3]`) {
		t.Fatalf("封闭元组编码为 %s", got)
	}
	// 客户端给出的 maxItems 更小时保留，不被 prefixItems 长度放宽
	got = mustEncodeSchema(t, `{"type":"array","prefixItems":[{"type":"string"},{"type":"integer"},{"type":"boolean"}],"items":false,"maxItems":1}`)
	if !strings.HasSuffix(got, `,1]`) {
		t.Fatalf("封闭元组 maxItems=1 编码为 %s", got)
	}
	if _, err := encodeJSONSchema(json.RawMessage(`{"type":"array","items":false,"minItems":1}`)); err == nil {
		t.Fatal("items:false 且 minItems>0 应返回错误")
	}
	// properties 中 true/null 按开放节点处理，false 删除并同步 required
	got = mustEncodeSchema(t, `{"type":"object","properties":{"allow":true,"empty":null,"deny":false,"name":{"type":"string"}},"required":["deny","name"]}`)
	want := `[6,null,null,null,null,null,[["allow",[1]],["empty",[1]],["name",[1]]],["name"]]`
	if got != want {
		t.Fatalf("布尔属性编码为 %s，期望 %s", got, want)
	}
}

// TestEncodeJSONSchemaBooleanItemsRespectDepth 布尔替换后的子节点仍受嵌套上限约束
func TestEncodeJSONSchemaBooleanItemsRespectDepth(t *testing.T) {
	deep := strings.Repeat(`{"type":"array","items":`, maxSchemaDepth+1) + `true` + strings.Repeat(`}`, maxSchemaDepth+1)
	if _, err := encodeJSONSchema(json.RawMessage(deep)); err == nil || !strings.Contains(err.Error(), "嵌套超过") {
		t.Fatalf("期望嵌套层数错误，得到 %v", err)
	}
}

// TestEncodeJSONSchemaCanonicalIntegers 整数字段的字符串写法（"5"）与零小数写法（5.0）按整数编码，
// 不再因解析失败让整个 Schema 降级；带小数或不是数字的写法仍报错
func TestEncodeJSONSchemaCanonicalIntegers(t *testing.T) {
	wire, err := encodeJSONSchema(json.RawMessage(`{"type":"array","items":{"type":"string","minLength":"2","maxLength":10.0},"minItems":"1","maxItems":5.0}`))
	if err != nil {
		t.Fatal(err)
	}
	if wire[21] != int64(1) || wire[20] != int64(5) {
		t.Fatalf("minItems=%#v maxItems=%#v，期望 1 与 5", wire[21], wire[20])
	}
	items := wire[5].([]any)
	if items[12] != int64(2) || items[13] != int64(10) {
		t.Fatalf("items minLength=%#v maxLength=%#v，期望 2 与 10", items[12], items[13])
	}
	wire, err = encodeJSONSchema(json.RawMessage(`{"type":"object","properties":{"a":{"type":"string"}},"minProperties":"0","maxProperties":"3"}`))
	if err != nil {
		t.Fatal(err)
	}
	if wire[8] != int64(0) || wire[9] != int64(3) {
		t.Fatalf("minProperties=%#v maxProperties=%#v，期望 0 与 3", wire[8], wire[9])
	}
	for _, raw := range []string{
		`{"type":"string","maxLength":"5.5"}`,
		`{"type":"string","maxLength":2.5}`,
		`{"type":"string","maxLength":"abc"}`,
		`{"type":"string","maxLength":"-1"}`,
		`{"type":"string","maxLength":1e300}`,
	} {
		if _, err := encodeJSONSchema(json.RawMessage(raw)); err == nil {
			t.Fatalf("%s 应返回错误", raw)
		}
	}
	// 函数参数里的字符串整数不再降级编码（降级时说明里会附上完整 Schema）
	declaration, err := encodeFunctionDeclaration(FunctionDeclaration{Name: "list", Parameters: json.RawMessage(`{"type":"object","properties":{"tags":{"type":"array","items":{"type":"string"},"maxItems":"3"}}}`)})
	if err != nil || declaration[1] != nil {
		t.Fatalf("字符串整数不应触发降级: wire=%v err=%v", declaration, err)
	}
}

// TestEncodeJSONSchemaTypeUnspecified type 为 TYPE_UNSPECIFIED 时按没有声明 type 处理，由结构推断类型
func TestEncodeJSONSchemaTypeUnspecified(t *testing.T) {
	for _, test := range []struct {
		raw  string
		code int64
	}{
		{raw: `{"type":"TYPE_UNSPECIFIED","properties":{"a":{"type":"string"}}}`, code: 6},
		{raw: `{"type":"TYPE_UNSPECIFIED","items":{"type":"STRING"}}`, code: 5},
		{raw: `{"type":"type_unspecified"}`, code: 1},
		{raw: `{"type":"TYPE_UNSPECIFIED","const":"x"}`, code: 1},
		{raw: `{"anyOf":[{"type":"TYPE_UNSPECIFIED"},{"type":"integer"}]}`, code: 3},
	} {
		wire, err := encodeJSONSchema(json.RawMessage(test.raw))
		if err != nil {
			t.Fatalf("%s: %v", test.raw, err)
		}
		if wire[0] != test.code {
			t.Fatalf("%s: type=%#v，期望 %d", test.raw, wire[0], test.code)
		}
	}
	wire, err := encodeJSONSchema(json.RawMessage(`{"type":"object","properties":{"a":{"type":"TYPE_UNSPECIFIED"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	property := wire[6].([]any)[0].([]any)[1].([]any)
	if property[0] != int64(1) {
		t.Fatalf("属性 type=%#v，期望按未声明推断为 string", property[0])
	}
}
