package aistudio

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// TestReplaceThoughtSignatures 改用其他模型时：函数调用的签名（Part 与 FunctionCall 上的）换成跳过校验的占位值，其余部分的签名去掉，
// 没有签名的部分保持原样；返回替换的签名数（已经是占位值的不计）；不改动传入的对话
func TestReplaceThoughtSignatures(t *testing.T) {
	original := []Content{
		{Role: RoleUser, Parts: []Part{{Text: "hi"}}},
		{Role: RoleAssistant, Parts: []Part{
			{Text: "thinking", Thought: true, ThoughtSignature: "sig-thought"},
			{FunctionCall: &FunctionCall{ID: "call-1", Name: "lookup", Arguments: json.RawMessage(`{}`), ThoughtSignature: "sig-call"}},
			{FunctionCall: &FunctionCall{ID: "call-2", Name: "lookup", Arguments: json.RawMessage(`{}`)}},
			{FunctionCall: &FunctionCall{ID: "call-3", Name: "lookup"}, ThoughtSignature: skipThoughtSignatureValidator},
			// new-api 把 Gemini 回复转成 OpenAI 格式再转回时写的占位签名：照常换成跳过校验值，但不是真实签名，不计数
			{FunctionCall: &FunctionCall{ID: "call-4", Name: "lookup", ThoughtSignature: newAPIThoughtSignature}},
		}},
		{Role: RoleUser, Parts: []Part{{FunctionResult: &FunctionResult{ID: "call-1", Name: "lookup"}}}},
	}
	replaced, count := ReplaceThoughtSignatures(original, nil)
	if count != 2 {
		t.Fatalf("替换数 = %d，期望 2（思考与 call-1；没有签名与已是占位值、new-api 占位值的不计）", count)
	}
	parts := replaced[1].Parts
	if parts[4].ThoughtSignature != skipThoughtSignatureValidator || parts[4].FunctionCall.ThoughtSignature != skipThoughtSignatureValidator {
		t.Fatalf("new-api 的占位签名应换成跳过校验值：%+v %+v", parts[4], parts[4].FunctionCall)
	}
	if parts[0].ThoughtSignature != "" || parts[0].Text != "thinking" || !parts[0].Thought {
		t.Fatalf("思考部分的签名应去掉：%+v", parts[0])
	}
	if parts[1].ThoughtSignature != skipThoughtSignatureValidator || parts[1].FunctionCall.ThoughtSignature != skipThoughtSignatureValidator {
		t.Fatalf("函数调用的签名应换成占位值：%+v %+v", parts[1], parts[1].FunctionCall)
	}
	if parts[2].ThoughtSignature != "" || parts[2].FunctionCall.ThoughtSignature != "" {
		t.Fatalf("没有签名的函数调用保持原样（编码时补占位值）：%+v", parts[2])
	}
	if original[1].Parts[0].ThoughtSignature != "sig-thought" || original[1].Parts[1].FunctionCall.ThoughtSignature != "sig-call" {
		t.Fatal("不应改动传入的对话")
	}
	if replaced[0].Parts[0].Text != "hi" || replaced[2].Parts[0].FunctionResult == nil {
		t.Fatal("其他内容应原样保留")
	}
	// keep 认定可以原样发送的签名不动（目标模型在之前改用时签发的）
	kept, count := ReplaceThoughtSignatures(original, func(signature string) bool { return signature == "sig-call" })
	if count != 1 || kept[1].Parts[1].FunctionCall.ThoughtSignature != "sig-call" || kept[1].Parts[0].ThoughtSignature != "" {
		t.Fatalf("keep 认定的签名应原样保留：count=%d parts=%+v", count, kept[1].Parts)
	}
}

// TestReplaceThoughtSignaturesSignatureOnlyParts 只有签名的部分（Anthropic 的 thinking 与 redacted_thinking 块、Responses 的 reasoning 项、
// Gemini 原生只带签名的部分）整个去掉，去掉后没有内容的消息一起去掉；处理后的对话在 Playground 与 Build 两种编码下都能编码。
// 原先只清掉签名，留下空部分，编码报“part 必须且只能设置一种内容”，改用的目标模型因此以不可用结束
func TestReplaceThoughtSignaturesSignatureOnlyParts(t *testing.T) {
	contents := []Content{
		{Role: RoleUser, Parts: []Part{{Text: "question"}}},
		// Anthropic：[thinking(签名), text] 与 [redacted_thinking, tool_use(签名挂在调用上)]
		{Role: RoleAssistant, Parts: []Part{{ThoughtSignature: "c2lnLWFyZ29u"}, {Text: "answer"}}},
		{Role: RoleUser, Parts: []Part{{Text: "again"}}},
		{Role: RoleAssistant, Parts: []Part{
			{ThoughtSignature: "redacted-data"},
			{FunctionCall: &FunctionCall{ID: "toolu_1", Name: "lookup", Arguments: json.RawMessage(`{}`), ThoughtSignature: "sig-call"}},
		}},
		{Role: RoleTool, Parts: []Part{{FunctionResult: &FunctionResult{ID: "toolu_1", Name: "lookup", Content: json.RawMessage(`{"ok":true}`)}}}},
		// Responses：只有 reasoning 项（encrypted_content）的一条模型消息
		{Role: RoleAssistant, Parts: []Part{{ThoughtSignature: "encrypted-reasoning"}}},
		{Role: RoleUser, Parts: []Part{{Text: "last"}}},
	}
	if _, err := encodeBuildContents(contents); err != nil {
		t.Fatalf("原对话应能编码：%v", err)
	}
	replaced, count := ReplaceThoughtSignatures(contents, nil)
	if count != 4 {
		t.Fatalf("替换数 = %d，期望 4", count)
	}
	if len(replaced) != len(contents)-1 {
		t.Fatalf("只有签名的消息应去掉：%d 条，期望 %d 条", len(replaced), len(contents)-1)
	}
	if parts := replaced[1].Parts; len(parts) != 1 || parts[0].Text != "answer" {
		t.Fatalf("thinking 块应去掉、正文保留：%+v", parts)
	}
	if parts := replaced[3].Parts; len(parts) != 1 || parts[0].FunctionCall == nil || parts[0].ThoughtSignature != skipThoughtSignatureValidator {
		t.Fatalf("redacted_thinking 应去掉、调用的签名换成占位值：%+v", parts)
	}
	for _, content := range replaced {
		for _, part := range content.Parts {
			if part.FunctionCall == nil && part.ThoughtSignature != "" {
				t.Fatalf("非函数调用部分不应再带签名：%+v", part)
			}
		}
	}
	if _, err := encodeBuildContents(replaced); err != nil {
		t.Fatalf("Build 编码失败：%v", err)
	}
	if _, err := encodeContents(replaced); err != nil {
		t.Fatalf("Playground 编码失败：%v", err)
	}
	if len(contents) != 7 || contents[1].Parts[0].ThoughtSignature != "c2lnLWFyZ29u" || len(contents[5].Parts) != 1 {
		t.Fatal("不应改动传入的对话")
	}
}

// TestPlaceholderThoughtSignature 空值、跳过校验值与 new-api 的占位值都不是真实签名
func TestPlaceholderThoughtSignature(t *testing.T) {
	for _, signature := range []string{"", " ", skipThoughtSignatureValidator, newAPIThoughtSignature} {
		if !PlaceholderThoughtSignature(signature) {
			t.Fatalf("%q 应是占位值", signature)
		}
	}
	if PlaceholderThoughtSignature("c2ln") {
		t.Fatal("真实签名不是占位值")
	}
}

// TestStreamStartHoldRestartWait 改用其他模型时写出响应头前的等待从改用时刻起重新计：至少再等目标模型的等待与原来的等待中较长的一个，
// 改用之前的现场冷启动延长不重复计入；没有改用时与原来相同；nil 安全
func TestStreamStartHoldRestartWait(t *testing.T) {
	var none *StreamStartHold
	none.RestartWait(time.Second)
	if none.Limit(time.Now(), 3*time.Second) != 3*time.Second {
		t.Fatal("nil 上应原样返回等待")
	}
	startedAt := time.Now().Add(-4 * time.Second)
	hold := NewStreamStartHold()
	hold.BeginColdStart(2 * time.Second)
	hold.EndColdStart(2 * time.Second)
	if limit := hold.Limit(startedAt, 3*time.Second); limit != 5*time.Second {
		t.Fatalf("没有改用时 = %s，期望等待加冷启动延长 5s", limit)
	}
	hold.RestartWait(10 * time.Second)
	select {
	case <-hold.Changed():
	default:
		t.Fatal("改用应唤醒等待方")
	}
	// 改用在开始后约 4s：之后再等 max(3s, 10s)，改用前的 2s 冷启动延长已经用掉，不再计入
	if limit := hold.Limit(startedAt, 3*time.Second); limit < 14*time.Second || limit > 14*time.Second+time.Second {
		t.Fatalf("改用后 = %s，期望约 14s", limit)
	}
	hold.BeginColdStart(5 * time.Second)
	if limit := hold.Limit(startedAt, 3*time.Second); limit < 19*time.Second || limit > 19*time.Second+time.Second {
		t.Fatalf("改用后的冷启动进行中 = %s，期望约 19s", limit)
	}
	hold.EndColdStart(time.Second)
	// 目标模型的等待更短时按原来的等待
	short := NewStreamStartHold()
	short.RestartWait(0)
	if limit := short.Limit(startedAt, 3*time.Second); limit < 7*time.Second || limit > 7*time.Second+time.Second {
		t.Fatalf("目标等待为 0 时 = %s，期望改用时刻起再等原来的 3s（约 7s）", limit)
	}
}

// TestFallbackTargetUnsupported 目标模型接不了请求时返回原因：不支持 generateContent、Interaction（Omni）或非文本对话模型、不支持请求的工具
func TestFallbackTargetUnsupported(t *testing.T) {
	chat := Model{ID: "chat", Methods: []string{"generateContent"}, Capabilities: map[string]bool{"chat_model": true, "function_declarations": true}}
	request := GenerateRequest{Tools: Tools{Functions: []FunctionDeclaration{{Name: "lookup"}}}}
	if why := FallbackTargetUnsupported(chat, request); why != "" {
		t.Fatalf("能接的目标返回 %q", why)
	}
	for _, test := range []struct {
		model  Model
		reason string
	}{
		{model: Model{ID: "embed", Methods: []string{"embedContent"}, Capabilities: map[string]bool{"chat_model": true}}, reason: "generateContent"},
		{model: Model{ID: "omni", Capabilities: map[string]bool{"chat_model": true, "interactions_api": true}}, reason: "Interaction"},
		{model: Model{ID: "image", Capabilities: map[string]bool{"chat_model": true, "image_route": true}}, reason: "不是文本对话模型"},
		{model: Model{ID: "plain", Capabilities: map[string]bool{"chat_model": true}}, reason: "不支持请求的工具"},
	} {
		if why := FallbackTargetUnsupported(test.model, request); !strings.Contains(why, test.reason) {
			t.Fatalf("%s：原因 = %q，期望含 %q", test.model.ID, why, test.reason)
		}
	}
	if TextGenerationRequest(GenerateRequest{Config: GenerationConfig{ResponseModalities: []ResponseModality{ResponseModalityImage}}}) {
		t.Fatal("图片输出不是文本对话生成")
	}
	if !TextGenerationRequest(GenerateRequest{Config: GenerationConfig{ResponseModalities: []ResponseModality{ResponseModalityText}}}) {
		t.Fatal("只要文本输出是文本对话生成")
	}
}

// TestModelSwitch 改用记录：没有改用时原样返回请求的模型，改用后返回目标模型，清除后恢复；nil 安全
func TestModelSwitch(t *testing.T) {
	var none *ModelSwitch
	none.Switch("a", "b")
	if none.Model("a") != "a" || none.From() != "" || ModelSwitchFromContext(context.Background()) != nil {
		t.Fatal("nil 记录应原样返回")
	}
	record := NewModelSwitch()
	ctx := ContextWithModelSwitch(context.Background(), record)
	if ModelSwitchFromContext(ctx) != record || record.Model("a") != "a" {
		t.Fatal("没有改用时应原样返回")
	}
	record.Switch("a", "b")
	if record.Model("a") != "b" || record.From() != "a" {
		t.Fatalf("改用后 = %q / %q", record.Model("a"), record.From())
	}
	record.Switch("a", "")
	if record.Model("a") != "a" || record.From() != "" {
		t.Fatal("清除后应恢复")
	}
}
