package aistudio

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestPlaygroundSchemaEmptyEnum 官网拒收 enum 里的空串：Playground 线格式去掉空串（去完为空时不写 enum），嵌套属性同样处理；
// 函数声明与结构化输出把完整 Schema 附在说明里交给模型遵守
func TestPlaygroundSchemaEmptyEnum(t *testing.T) {
	for _, test := range []struct {
		raw  string
		want string
	}{
		{raw: `{"type":"string","enum":["","low","high"]}`, want: `[1,null,null,null,["low","high"]]`},
		{raw: `{"type":"string","enum":[""]}`, want: `[1]`},
		{
			raw:  `{"type":"object","properties":{"level":{"type":"string","enum":["","a"]}}}`,
			want: `[6,null,null,null,null,null,[["level",[1,null,null,null,["a"]]]]]`,
		},
	} {
		if got := mustEncodeSchema(t, test.raw); got != test.want {
			t.Fatalf("%s 编码为 %s，期望 %s", test.raw, got, test.want)
		}
	}
	parameters := `{"type":"object","properties":{"level":{"type":"string","enum":["","a"]}}}`
	wire, err := encodeFunctionDeclaration(FunctionDeclaration{Name: "set_level", Description: "设置级别", Parameters: json.RawMessage(parameters)})
	if err != nil {
		t.Fatal(err)
	}
	description, _ := wire[1].(string)
	if !strings.HasPrefix(description, "设置级别\nArguments must follow this JSON Schema: ") || !strings.Contains(description, `"enum":["","a"]`) {
		t.Fatalf("函数说明应附完整 Schema：%q", description)
	}
	if encoded, _ := json.Marshal(wire[2]); strings.Contains(string(encoded), `""`) {
		t.Fatalf("函数参数线格式不应含空串：%s", encoded)
	}
	// 没有空串的 Schema 不附提示
	plain, _ := encodeFunctionDeclaration(FunctionDeclaration{Name: "f", Description: "d", Parameters: json.RawMessage(`{"type":"object","properties":{"a":{"type":"string","enum":["x"]}}}`)})
	if plain[1] != "d" {
		t.Fatalf("没有空串时不应附 Schema：%v", plain[1])
	}
	response, err := encodeResponseSchema(json.RawMessage(`{"type":"string","enum":["","a"]}`))
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(response)
	if string(encoded) != `[1,null,"The response must follow this JSON Schema: {\"type\":\"string\",\"enum\":[\"\",\"a\"]}",null,["a"]]` {
		t.Fatalf("结构化输出编码为 %s", encoded)
	}
	if !schemaHasEmptyEnum(json.RawMessage(`{"type":"object","properties":{"x":{"const":""}}}`)) ||
		!schemaHasEmptyEnum(json.RawMessage(`{"type":"string","not":{"enum":["","a"],"pattern":"^a$"}}`)) ||
		schemaHasEmptyEnum(json.RawMessage(`{"type":"string","enum":["a"],"default":""}`)) {
		t.Fatal("schemaHasEmptyEnum 判断不对")
	}
}

// TestPlaygroundSchemaNotEmptyString not 排除空串：纯枚举排除且节点为 string 时改写为 minLength 1（已有更大的 minLength 不动），
// 其余排除值照旧；联合类型与带其他约束的排除只去掉空串，不收紧其余取值
func TestPlaygroundSchemaNotEmptyString(t *testing.T) {
	minLengthOne := `[1,null,null,null,null,null,null,null,null,null,null,null,1]`
	for _, raw := range []string{
		`{"type":"string","not":""}`,
		`{"type":"string","not":{"enum":[""]}}`,
		`{"type":"string","not":{"type":"string","enum":[""]}}`,
	} {
		if got := mustEncodeSchema(t, raw); got != minLengthOne {
			t.Fatalf("%s 编码为 %s，期望只剩 minLength 1", raw, got)
		}
	}
	if got := mustEncodeSchema(t, `{"type":"string","not":["","a","b"]}`); got !=
		`[1,null,null,null,null,null,null,null,null,null,null,null,1,null,null,null,null,null,null,[1,null,null,null,["a","b"]]]` {
		t.Fatalf("not 数组含空串编码为 %s", got)
	}
	if got := mustEncodeSchema(t, `{"type":"string","minLength":3,"not":""}`); got != `[1,null,null,null,null,null,null,null,null,null,null,null,3]` {
		t.Fatalf("已有 minLength 3 时不应改小：%s", got)
	}
	// 联合类型：不加 minLength（整数分支照旧可用），只把空串从排除值里去掉
	union := mustEncodeSchema(t, `{"type":["integer","string"],"not":{"enum":["","reserved"]}}`)
	if union != `[3,null,null,null,null,null,null,null,null,null,null,null,null,null,null,null,null,[[3],[1]],null,[1,null,null,null,["reserved"]]]` {
		t.Fatalf("联合类型编码为 %s", union)
	}
	// 带 pattern 的排除：排除的是同时满足 enum 与 pattern 的值，去掉空串只会少排除，不改写为 minLength
	pattern := mustEncodeSchema(t, `{"type":"string","not":{"enum":["","reserved"],"pattern":"^reserved$"}}`)
	if pattern != `[1,null,null,null,null,null,null,null,null,null,null,null,null,null,null,null,null,null,null,[1,null,null,null,["reserved"],null,null,null,null,null,null,null,null,null,"^reserved$"]]` {
		t.Fatalf("带 pattern 的排除编码为 %s", pattern)
	}
	// 非字符串节点排除空串：没有可加的 minLength，删除 not
	if got := mustEncodeSchema(t, `{"type":"integer","not":""}`); got != `[3]` {
		t.Fatalf("整数节点 not:\"\" 编码为 %s", got)
	}
}

// TestPlaygroundSchemaNotEmptyStringNotInverted not 里用 const 或组合写出的空串排除：const:"" 与 enum:[""] 同样改写为 minLength 1；
// 改写不了的（anyOf、oneOf、allOf、对象属性嵌套）去掉空串后子 Schema 被放宽，删除这个 not 或其中被放宽的分支，
// 不能发出“字符串但不能是字符串”这样无法满足的约束
func TestPlaygroundSchemaNotEmptyStringNotInverted(t *testing.T) {
	minLengthOne := `[1,null,null,null,null,null,null,null,null,null,null,null,1]`
	for _, test := range []struct {
		raw  string
		want string
	}{
		{raw: `{"type":"string","not":{"const":""}}`, want: minLengthOne},
		{raw: `{"type":"string","not":{"type":"string","const":""}}`, want: minLengthOne},
		{raw: `{"type":"string","not":{"description":"不能为空","enum":[""]}}`, want: minLengthOne},
		// 组合里只排除空串：删除 not，宁可放宽
		{raw: `{"type":"string","not":{"anyOf":[{"const":""}]}}`, want: `[1]`},
		{raw: `{"type":"string","not":{"oneOf":[{"enum":[""]}]}}`, want: `[1]`},
		{raw: `{"type":"string","not":{"allOf":[{"enum":[""]},{"pattern":"^$"}]}}`, want: `[1]`},
		{raw: `{"type":"object","not":{"type":"object","properties":{"a":{"const":""}}}}`, want: `[6]`},
		{raw: `{"type":"array","items":{"type":"string"},"not":{"type":"array","items":{"const":""}}}`, want: `[5,null,null,null,null,[1]]`},
		// anyOf 里其余分支照旧排除，只去掉被放宽的空串分支
		{
			raw: `{"type":"string","not":{"anyOf":[{"const":""},{"const":"x"}]}}`,
			want: `[1,null,null,null,null,null,null,null,null,null,null,null,null,null,null,null,null,null,null,` +
				`[1,null,null,null,null,null,null,null,null,null,null,null,null,null,null,null,null,[[1,null,null,null,["x"]]]]]`,
		},
		// not 之外的 anyOf 分支被放宽照常保留（放宽不会反转）
		{
			raw:  `{"type":"object","properties":{"a":{"anyOf":[{"const":""},{"type":"integer"}]}}}`,
			want: `[6,null,null,null,null,null,[["a",[1,null,null,null,null,null,null,null,null,null,null,null,null,null,null,null,null,[[1],[3]]]]]]`,
		},
	} {
		got := mustEncodeSchema(t, test.raw)
		if got != test.want {
			t.Fatalf("%s 编码为 %s，期望 %s", test.raw, got, test.want)
		}
		// 不论哪种写法，not 的子 Schema 都不能只剩类型（[1]、[6] 这样没有任何约束的子 Schema 取反后无法满足）
		var wire []any
		if err := json.Unmarshal([]byte(got), &wire); err != nil {
			t.Fatal(err)
		}
		if len(wire) > 19 {
			if sub, _ := json.Marshal(wire[19]); string(sub) == `[1]` || string(sub) == `[6]` || string(sub) == `[5]` {
				t.Fatalf("%s 的 not 只剩类型 %s，取反后无法满足", test.raw, sub)
			}
		}
	}
}
