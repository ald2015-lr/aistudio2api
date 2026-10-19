package aistudio

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// finishEvent 返回事件里的终止事件，没有时测试失败
func finishEvent(t *testing.T, events []Event) Event {
	t.Helper()
	for _, event := range events {
		if event.Kind == EventFinish {
			return event
		}
	}
	t.Fatalf("没有终止事件: %+v", events)
	return Event{}
}

// TestFrameDecoderFinishMessage Playground 候选 field 4 的 finishMessage 写进终止事件；没有或不是字符串时为空
func TestFrameDecoderFinishMessage(t *testing.T) {
	events, err := NewFrameDecoder().Decode(json.RawMessage(`[[[null,10,null,"Malformed function call: print(default_api.lookup(id=))"]]]`))
	if err != nil {
		t.Fatal(err)
	}
	finish := finishEvent(t, events)
	if finish.FinishReason != "malformed_function_call" || finish.FinishMessage != "Malformed function call: print(default_api.lookup(id=))" {
		t.Fatalf("终止事件 = %+v", finish)
	}
	for _, raw := range []string{`[[[null,1]]]`, `[[[null,1,null,null]]]`, `[[[null,1,null,7]]]`} {
		events, err := NewFrameDecoder().Decode(json.RawMessage(raw))
		if err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
		if finish := finishEvent(t, events); finish.FinishReason != "stop" || finish.FinishMessage != "" {
			t.Fatalf("%s: 终止事件 = %+v", raw, finish)
		}
	}
}

// TestFrameDecoderBlockReasonMessage Playground promptFeedback field 3 的拦截说明写进 PromptFeedbackError 的错误信息
func TestFrameDecoderBlockReasonMessage(t *testing.T) {
	_, err := NewFrameDecoder().Decode(json.RawMessage(`[null,[4,null,"The prompt contains prohibited content."]]`))
	var blocked *PromptFeedbackError
	if !errors.As(err, &blocked) {
		t.Fatalf("err = %v，期望 PromptFeedbackError", err)
	}
	if blocked.Reason != "prohibited_content" || blocked.Message != "The prompt contains prohibited content." {
		t.Fatalf("拦截 = %+v", blocked)
	}
	if !strings.Contains(err.Error(), "prohibited_content: The prompt contains prohibited content.") || !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("错误信息 = %q", err.Error())
	}
	_, err = NewFrameDecoder().Decode(json.RawMessage(`[null,[1]]`))
	if !errors.As(err, &blocked) || blocked.Message != "" || err.Error() != "AI Studio 拒绝当前输入: safety" {
		t.Fatalf("没有说明时错误信息保持原样，得到 %v", err)
	}
}

// TestBuildDecoderFinishMessage Build 响应的 finishMessage 与 promptFeedback.blockReasonMessage 同样解码
func TestBuildDecoderFinishMessage(t *testing.T) {
	decoder := &BuildStreamDecoder{}
	events, err := decoder.Decode([]byte(`{"candidates":[{"content":{"parts":[{"text":"hi"}]},"finishReason":"UNEXPECTED_TOOL_CALL","finishMessage":"Unexpected tool call: lookup"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	finish := finishEvent(t, events)
	if finish.FinishReason != "unexpected_tool_call" || finish.FinishMessage != "Unexpected tool call: lookup" {
		t.Fatalf("终止事件 = %+v", finish)
	}
	events, err = (&BuildStreamDecoder{}).Decode([]byte(`{"candidates":[{"finishReason":"STOP"}]}`))
	if err != nil || finishEvent(t, events).FinishMessage != "" {
		t.Fatalf("没有 finishMessage 时应为空: %+v err=%v", events, err)
	}

	_, err = (&BuildStreamDecoder{}).Decode([]byte(`{"promptFeedback":{"blockReason":"SAFETY","blockReasonMessage":"Blocked for safety."}}`))
	var blocked *PromptFeedbackError
	if !errors.As(err, &blocked) || blocked.Reason != "safety" || blocked.Message != "Blocked for safety." {
		t.Fatalf("err = %v，期望带说明的 PromptFeedbackError", err)
	}
	if err.Error() != "AI Studio 拒绝当前输入: safety: Blocked for safety." {
		t.Fatalf("错误信息 = %q", err.Error())
	}
}
