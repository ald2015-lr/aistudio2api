package app

import (
	"context"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/config"
)

func guardTestService(t *testing.T) *trackedService {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	service := &trackedService{lifecycle: ctx, requests: newRequestRegistry(ctx)}
	guard := config.DefaultDowngradeGuard()
	guard.Models = []string{"gemini-guard-test"}
	service.setDowngradeGuard(guard)
	return service
}

func guardContents(texts ...string) []aistudio.Content {
	contents := make([]aistudio.Content, 0, len(texts))
	for index, text := range texts {
		role := aistudio.RoleUser
		if index%2 == 1 {
			role = aistudio.RoleAssistant
		}
		contents = append(contents, aistudio.Content{Role: role, Parts: []aistudio.Part{{Text: text}}})
	}
	return contents
}

// TestGuardInputCharsIncludePromptNonce 估算比例的分母按实际发往上游的内容计算（含随机后缀）
func TestGuardInputCharsIncludePromptNonce(t *testing.T) {
	service := guardTestService(t)
	original := guardContents("Explain quantum computing in detail.")
	request := aistudio.GenerateRequest{ID: "guard-chars", Model: "gemini-guard-test", Contents: original}
	nonce := newPromptNonce()
	modified, ok := applyPromptNonce(request, nonce)
	if !ok {
		t.Fatal("没有加入随机后缀")
	}
	gate, rejected := service.prepareDowngradeGate(context.Background(), modified, original)
	if rejected != nil || gate == nil {
		t.Fatalf("gate=%v rejected=%v", gate, rejected)
	}
	want := int64(len([]rune("Explain quantum computing in detail.")) + promptNonceBits)
	if gate.inputChars != want {
		t.Fatalf("inputChars = %d，期望 %d（含 %d 个零宽字符）", gate.inputChars, want, promptNonceBits)
	}
}

// TestGuardDoesNotRememberSingleMessage 单条消息的对话判定为降级后不记录，避免拒绝所有同一开场白的对话
func TestGuardDoesNotRememberSingleMessage(t *testing.T) {
	service := guardTestService(t)
	single := aistudio.GenerateRequest{ID: "guard-single", Model: "gemini-guard-test", System: "guard-single-system", Contents: guardContents("hi")}
	gate, _ := service.prepareDowngradeGate(context.Background(), single, single.Contents)
	gate.remember()
	if _, rejected := service.prepareDowngradeGate(context.Background(), single, single.Contents); rejected != nil {
		t.Fatal("单条消息的对话不应被记录")
	}

	multi := aistudio.GenerateRequest{ID: "guard-multi", Model: "gemini-guard-test", System: "guard-multi-system", Contents: guardContents("hi", "hello", "continue")}
	gate, _ = service.prepareDowngradeGate(context.Background(), multi, multi.Contents)
	gate.remember()
	if _, rejected := service.prepareDowngradeGate(context.Background(), multi, multi.Contents); rejected == nil {
		t.Fatal("多条消息的对话应被记录并在发送前拒绝")
	}
}

// TestGuardFinalWindowIgnoresNonTextUsage 回复包含函数调用时结束复核不用包含函数调用的总输出 token
func TestGuardFinalWindowIgnoresNonTextUsage(t *testing.T) {
	var meter downgradeMeter
	started := time.Now()
	meter.observe(aistudio.Event{Kind: aistudio.EventText, Text: "a", UpstreamOutputTokens: 10}, started, true)
	meter.observe(aistudio.Event{Kind: aistudio.EventText, Text: "b", UpstreamOutputTokens: 410}, started.Add(3*time.Second), true)
	meter.observe(aistudio.Event{Kind: aistudio.EventToolCall}, started.Add(4*time.Second), true)
	meter.observe(aistudio.Event{Kind: aistudio.EventUsage, Usage: &aistudio.Usage{OutputTokens: 2410}}, started.Add(4*time.Second), true)
	tokens, window, ok := meter.finalWindow()
	if !ok || tokens != 400 || window != 3*time.Second {
		t.Fatalf("finalWindow = %d, %s, %v；期望只按正文累计数 400 token / 3 秒", tokens, window, ok)
	}
}

// TestGuardClientDisconnectNotJudged 客户端断开导致上游提前结束时不判定、不记录对话
func TestGuardClientDisconnectNotJudged(t *testing.T) {
	service := guardTestService(t)
	request := aistudio.GenerateRequest{ID: "guard-gone", Model: "gemini-guard-test", System: "guard-gone-system", Contents: guardContents("q1", "a1", "q2")}
	clientCtx, clientCancel := context.WithCancel(context.Background())
	gate, rejected := service.prepareDowngradeGate(clientCtx, request, request.Contents)
	if rejected != nil || gate == nil {
		t.Fatal("准备判定失败")
	}
	upstream := make(chan aistudio.Event, 4)
	out, _ := gate.start(clientCtx, func() {}, upstream)
	upstream <- aistudio.Event{Kind: aistudio.EventText, Text: "开头", UpstreamOutputTokens: 10}
	time.Sleep(900 * time.Millisecond)
	// 0.9 秒内新增 390 token：结束时按整段速度复核会判为降级（约 430 tok/s）
	upstream <- aistudio.Event{Kind: aistudio.EventText, Text: "很长的一段正文", UpstreamOutputTokens: 400}
	time.Sleep(50 * time.Millisecond)
	clientCancel()
	close(upstream)
	for range out {
	}
	if gate.rejection() != nil {
		t.Fatalf("客户端断开后不应判定为降级: %v", gate.rejection())
	}
	if _, remembered := service.prepareDowngradeGate(context.Background(), request, request.Contents); remembered != nil {
		t.Fatal("客户端断开时的截断数据不应记录对话")
	}
}
