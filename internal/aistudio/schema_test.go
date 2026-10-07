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
