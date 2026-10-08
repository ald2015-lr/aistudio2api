package app

import (
	"context"
	"encoding/json"
	"testing"
	"unicode/utf8"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/config"
)

// sentInputGuardService 返回只拦截测试模型的生成服务
func sentInputGuardService(t *testing.T) *trackedService {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	service := &trackedService{lifecycle: ctx, requests: newRequestRegistry(ctx)}
	guard := config.DefaultDowngradeGuard()
	guard.Models = []string{testRuntimeModel}
	service.setDowngradeGuard(guard)
	return service
}

// TestGuardInputSizeFollowsSentInput 降级判定的输入字数按每次尝试实际发送的内容校准：普通请求与准备判定时完全相同，
// 写进系统指令的工具约束、附在说明里的 Schema 计入，truncation=auto 删除的轮次不再计入
func TestGuardInputSizeFollowsSentInput(t *testing.T) {
	service := sentInputGuardService(t)
	request := aistudio.GenerateRequest{
		ID: "guard-sent", Model: testRuntimeModel, System: "be brief",
		Contents: guardContents("first question", "first answer", "second question"),
		Tools:    aistudio.Tools{Functions: []aistudio.FunctionDeclaration{{Name: "get_weather", Parameters: json.RawMessage(`{"type":"object"}`)}}},
	}
	gate, rejected := service.prepareDowngradeGate(context.Background(), request, request.Contents)
	if rejected != nil || gate == nil {
		t.Fatalf("gate=%v rejected=%v", gate, rejected)
	}
	prepared, _ := gate.inputSize()
	preparedRatio, preparedUpper := gate.tokenRatio(1000)

	gate.observeSentInput(aistudio.SentInput{
		Channel: aistudio.ChannelPlayground, System: request.System, Contents: request.Contents, Tools: request.Tools,
	})
	if chars, _ := gate.inputSize(); chars != prepared {
		t.Fatalf("普通请求的实际输入字数 = %d，应与准备判定时的 %d 相同", chars, prepared)
	}
	if ratio, upper := gate.tokenRatio(1000); ratio != preparedRatio || upper != preparedUpper {
		t.Fatalf("普通请求的比例 = %v/%v，应与准备判定时的 %v/%v 相同", ratio, upper, preparedRatio, preparedUpper)
	}

	hint := "\nUse one of these tools in this response: get_weather. Use the tool before answering."
	gate.observeSentInput(aistudio.SentInput{
		Channel: aistudio.ChannelPlayground, System: request.System + hint, Contents: request.Contents,
		Tools: request.Tools, SchemaHintChars: 120,
	})
	rewritten, _ := gate.inputSize()
	if want := prepared + int64(utf8.RuneCountInString(hint)) + 120; rewritten != want {
		t.Fatalf("改写后的实际输入字数 = %d，期望 %d（含系统指令提示与附上的 Schema）", rewritten, want)
	}
	if ratio, _ := gate.tokenRatio(1000); ratio >= preparedRatio {
		t.Fatalf("实际输入更长时比例应变小：%v ≥ %v", ratio, preparedRatio)
	}

	gate.observeSentInput(aistudio.SentInput{
		Channel: aistudio.ChannelPlayground, System: request.System, Contents: request.Contents[2:],
		Tools: request.Tools,
	})
	truncated, _ := gate.inputSize()
	if want := prepared - int64(utf8.RuneCountInString("first question")+utf8.RuneCountInString("first answer")); truncated != want {
		t.Fatalf("截断后的实际输入字数 = %d，期望 %d（不含删除的轮次）", truncated, want)
	}
}

// TestGuardSentInputMedia 实际输入含附件时比例仍标为上限
func TestGuardSentInputMedia(t *testing.T) {
	service := sentInputGuardService(t)
	request := aistudio.GenerateRequest{ID: "guard-media", Model: testRuntimeModel, Contents: guardContents("describe")}
	gate, _ := service.prepareDowngradeGate(context.Background(), request, request.Contents)
	contents := []aistudio.Content{{Role: aistudio.RoleUser, Parts: []aistudio.Part{
		{Text: "describe"}, {File: &aistudio.FileRef{ID: "files/abc", MIME: "image/png"}},
	}}}
	gate.observeSentInput(aistudio.SentInput{Channel: aistudio.ChannelPlayground, Contents: contents})
	if _, upper := gate.tokenRatio(1000); !upper {
		t.Fatal("实际输入含附件时比例应标为上限")
	}
}
