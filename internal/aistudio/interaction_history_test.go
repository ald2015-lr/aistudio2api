package aistudio

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

// TestInteractionToolHistoryAsText 发往 Omni 的对话历史里有函数调用、函数结果与代码执行（同一段对话先用其他模型调用过 save_memory 等，
// 再切到 Omni 并带图片）：原先编码报 “this part type is not supported”（tool 消息报 “does not accept tool messages”）；改为文本后能编码，
// 调用与结果的文字说明写明名称与 JSON（结果没有名称时按调用 ID 取调用的名称，取不到时写调用 ID），tool 消息改为用户消息并与随后的用户消息合并，
// 图片照常内联。不改动传入的切片；
// 没有工具历史时原样返回，其他相邻的同角色消息不合并
func TestInteractionToolHistoryAsText(t *testing.T) {
	png := omniTestPNG(t)
	contents := []Content{
		{Role: RoleUser, Parts: []Part{{Text: "记住我喜欢猫，再算一下 1+1"}}},
		{Role: RoleAssistant, Parts: []Part{
			{Text: "好的"},
			{FunctionCall: &FunctionCall{ID: "call_1", Name: "save_memory", Arguments: json.RawMessage(`{"content":"喜欢猫"}`), ThoughtSignature: "sig-1"},
				ThoughtSignature: "sig-1"},
			{FunctionCall: &FunctionCall{ID: "call_2", Arguments: json.RawMessage(`null`)}},
			{ExecutableCode: &ExecutableCode{Language: "PYTHON", Code: "print(1+1)"}},
			{CodeExecutionResult: &CodeExecutionResult{Outcome: "OUTCOME_OK", Output: "2\n"}},
		}},
		{Role: RoleTool, Parts: []Part{
			{FunctionResult: &FunctionResult{ID: "call_1", Content: json.RawMessage(` {"result":"saved"} `)}},
			{FunctionResult: &FunctionResult{ID: "call_2", Content: json.RawMessage(`{"error":"unknown"}`)}},
		}},
		{Role: RoleUser, Parts: []Part{{Text: "根据这张图生成视频"}, {InlineData: &Blob{MIME: "image/png", Data: png}}}},
	}
	original := slices.Clone(contents)
	originalAssistant := slices.Clone(contents[1].Parts)
	if _, _, err := EncodeCreateInteractionStreamRequest(GenerateRequest{Model: "gemini-omni-1.1-flash", Contents: contents}, omniTestDefaults()); err == nil ||
		!strings.Contains(err.Error(), "this part type is not supported") {
		t.Fatalf("改写前编码应报不支持的 part，得到 %v", err)
	}
	converted, count := InteractionToolHistoryAsText(contents)
	if count != 6 {
		t.Fatalf("改写的 part 数 = %d，期望 6", count)
	}
	if !slices.EqualFunc(contents, original, func(left, right Content) bool { return left.Role == right.Role && len(left.Parts) == len(right.Parts) }) ||
		contents[1].Parts[1].FunctionCall == nil || contents[2].Role != RoleTool || !slices.Equal(contents[1].Parts, originalAssistant) {
		t.Fatal("不应改动传入的切片")
	}
	want := []struct {
		role  Role
		texts []string
	}{
		{RoleUser, []string{"记住我喜欢猫，再算一下 1+1"}},
		{RoleAssistant, []string{"好的", `Tool call save_memory: {"content":"喜欢猫"}`, "Tool call (call_2): {}",
			"Executable code (PYTHON):\nprint(1+1)", "Code execution result (OUTCOME_OK):\n2"}},
		{RoleUser, []string{`Tool result save_memory: {"result":"saved"}`, `Tool result (call_2): {"error":"unknown"}`, "根据这张图生成视频", ""}},
	}
	if len(converted) != len(want) {
		t.Fatalf("改写后 = %+v", converted)
	}
	for index, content := range converted {
		texts := make([]string, 0, len(content.Parts))
		for _, part := range content.Parts {
			if part.FunctionCall != nil || part.FunctionResult != nil || part.ExecutableCode != nil || part.CodeExecutionResult != nil ||
				part.ThoughtSignature != "" {
				t.Fatalf("contents[%d] 仍有工具 part 或签名: %+v", index, part)
			}
			texts = append(texts, part.Text)
		}
		if content.Role != want[index].role || !slices.Equal(texts, want[index].texts) {
			t.Fatalf("contents[%d] = %s %q，期望 %s %q", index, content.Role, texts, want[index].role, want[index].texts)
		}
	}
	if image := converted[2].Parts[3].InlineData; image == nil || image.MIME != "image/png" {
		t.Fatalf("这一轮的图片应保留: %+v", converted[2].Parts[3])
	}
	body, _, err := EncodeCreateInteractionStreamRequest(GenerateRequest{Model: "gemini-omni-1.1-flash", Contents: converted}, omniTestDefaults())
	if err != nil {
		t.Fatalf("改写后应能编码: %v", err)
	}
	if !strings.Contains(string(body), `Tool call save_memory: {\"content\":\"喜欢猫\"}`) || !strings.Contains(string(body), `[null,[1,"`) {
		t.Fatalf("请求体应带调用的文字说明并内联 PNG: %.600s", body)
	}

	// 没有工具历史：原样返回同一个切片，相邻的两条用户消息不合并
	plain := []Content{{Role: RoleUser, Parts: []Part{{Text: "a"}}}, {Role: RoleUser, Parts: []Part{{Text: "b"}}}}
	if same, count := InteractionToolHistoryAsText(plain); count != 0 || len(same) != 2 || &same[0] != &plain[0] {
		t.Fatalf("没有工具历史时应原样返回: %d %+v", count, same)
	}
	// 只有由 tool 消息改写来的用户消息与前后相邻的用户消息合并：更前面相邻的两条普通用户消息保持分开
	mixed := []Content{
		{Role: RoleUser, Parts: []Part{{Text: "a"}}}, {Role: RoleUser, Parts: []Part{{Text: "b"}}},
		{Role: RoleTool, Parts: []Part{{FunctionResult: &FunctionResult{Name: "f", Content: json.RawMessage(`{}`)}}}},
		{Role: RoleUser, Parts: []Part{{Text: "c"}}},
	}
	converted, count = InteractionToolHistoryAsText(mixed)
	texts := func(content Content) []string {
		var result []string
		for _, part := range content.Parts {
			result = append(result, part.Text)
		}
		return result
	}
	if count != 1 || len(converted) != 2 || converted[0].Role != RoleUser || !slices.Equal(texts(converted[0]), []string{"a"}) ||
		converted[1].Role != RoleUser || !slices.Equal(texts(converted[1]), []string{"b", "Tool result f: {}", "c"}) {
		t.Fatalf("改写后 = %+v", converted)
	}
	if note := InteractionToolHistoryNote(6); !strings.Contains(note, "6 个函数调用、函数结果与代码执行已改为文本发送") {
		t.Fatalf("说明 = %q", note)
	}
}

// TestInteractionToolHistoryKeepsFromSystem Anthropic 请求的 messages 里有 system 角色消息（转成带 FromSystem 标记的用户消息），又有工具历史：
// 改写成文本后标记保留，system 转成的轮次不与相邻的 tool 消息合并，发送前按所选账户目录补做的系统提示去除仍能认出并去掉它
func TestInteractionToolHistoryKeepsFromSystem(t *testing.T) {
	contents := []Content{
		{Role: RoleUser, Parts: []Part{{Text: "记住我喜欢猫"}}},
		{Role: RoleAssistant, Parts: []Part{{FunctionCall: &FunctionCall{ID: "call_1", Name: "save_memory", Arguments: json.RawMessage(`{"content":"喜欢猫"}`)}}}},
		{Role: RoleTool, Parts: []Part{{FunctionResult: &FunctionResult{ID: "call_1", Content: json.RawMessage(`{"result":"saved"}`)}}}},
		{Role: RoleUser, FromSystem: true, Parts: []Part{{Text: "<system-reminder>只回中文</system-reminder>"}}},
		{Role: RoleUser, Parts: []Part{{Text: "生成一个猫的视频"}}},
	}
	converted, count := InteractionToolHistoryAsText(contents)
	if count != 2 {
		t.Fatalf("改写的 part 数 = %d，期望 2", count)
	}
	if len(converted) != 5 {
		t.Fatalf("system 转成的轮次不应与相邻消息合并，改写后 = %+v", converted)
	}
	for index, content := range converted {
		if want := index == 3; content.FromSystem != want {
			t.Fatalf("第 %d 条 FromSystem = %v，期望 %v：%+v", index, content.FromSystem, want, converted)
		}
	}
	if converted[2].Role != RoleUser || len(converted[2].Parts) != 1 || converted[2].Parts[0].Text != `Tool result save_memory: {"result":"saved"}` {
		t.Fatalf("tool 消息改写 = %+v", converted[2])
	}
	if !contents[3].FromSystem || contents[2].Role != RoleTool {
		t.Fatal("不应改动传入的切片")
	}
}
