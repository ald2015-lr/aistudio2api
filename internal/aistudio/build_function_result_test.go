package aistudio

import (
	"encoding/json"
	"testing"
)

// parallelCallContents 返回一轮两个并行函数调用，以及给定的两个函数结果
func parallelCallContents(first FunctionResult, second FunctionResult) []Content {
	return []Content{
		{Role: RoleUser, Parts: []Part{{Text: "weather and time"}}},
		{Role: RoleAssistant, Parts: []Part{
			{FunctionCall: &FunctionCall{ID: "c1", Name: "get_weather", Arguments: json.RawMessage(`{"city":"Paris"}`)}},
			{FunctionCall: &FunctionCall{ID: "c2", Name: "get_time", Arguments: json.RawMessage(`{}`)}},
		}},
		{Role: RoleTool, Parts: []Part{{FunctionResult: &first}, {FunctionResult: &second}}},
	}
}

// buildResultNames 返回 Build 编码后最后一条内容里各函数结果的名称
func buildResultNames(t *testing.T, contents []Content) []string {
	t.Helper()
	wire, err := encodeBuildContents(contents)
	if err != nil {
		t.Fatalf("Build 编码失败: %v", err)
	}
	parts := wire[len(wire)-1].(map[string]any)["parts"].([]any)
	names := make([]string, 0, len(parts))
	for _, part := range parts {
		names = append(names, part.(map[string]any)["functionResponse"].(map[string]any)["name"].(string))
	}
	return names
}

// TestBuildFunctionResultRemovedFromPending Build 函数结果关联上调用后从待返回列表删除（与 Playground 一致）：
// 第一个结果按 ID 关联后，第二个不带 ID、不带名称的结果对应剩下的唯一调用，原先因仍有两个待返回调用而报错
func TestBuildFunctionResultRemovedFromPending(t *testing.T) {
	for _, first := range []FunctionResult{
		{ID: "c1", Content: json.RawMessage(`{"temp":20}`)},
		{ID: "c1", Name: "get_weather", Content: json.RawMessage(`{"temp":20}`)},
	} {
		names := buildResultNames(t, parallelCallContents(first, FunctionResult{Content: json.RawMessage(`{"time":"12:00"}`)}))
		if len(names) != 2 || names[0] != "get_weather" || names[1] != "get_time" {
			t.Fatalf("第一个结果 %+v：名称 = %v，期望 [get_weather get_time]", first, names)
		}
		// Playground 同样能关联，两条通道一致
		if _, err := encodeContents(parallelCallContents(first, FunctionResult{Content: json.RawMessage(`{"time":"12:00"}`)})); err != nil {
			t.Fatalf("Playground 编码失败: %v", err)
		}
	}

	// 两个结果都不带 ID 与名称时无法确定对应关系，仍报错
	if _, err := encodeBuildContents(parallelCallContents(
		FunctionResult{Content: json.RawMessage(`{}`)}, FunctionResult{Content: json.RawMessage(`{}`)},
	)); err == nil {
		t.Fatal("无法关联的函数结果应返回错误")
	}
}
