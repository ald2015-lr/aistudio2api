package api

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// TestResponsesReasoningWithoutEncryptedContentKeepsSignature 没有 encrypted_content 的 reasoning 项不清掉之前待用的签名：
// 随后的函数调用仍带上前一个 reasoning 的签名；有 encrypted_content 的 reasoning 照旧换成它的签名，message 照旧清掉
func TestResponsesReasoningWithoutEncryptedContentKeepsSignature(t *testing.T) {
	contents, _, err := responsesContents(json.RawMessage(`[
		{"type":"message","role":"user","content":"hi"},
		{"type":"reasoning","encrypted_content":"sig-a","summary":[]},
		{"type":"reasoning","summary":[{"type":"summary_text","text":"thinking"}]},
		{"type":"function_call","call_id":"c1","name":"lookup","arguments":"{}"},
		{"type":"function_call_output","call_id":"c1","output":"ok"},
		{"type":"reasoning","encrypted_content":"sig-b"},
		{"type":"reasoning","encrypted_content":"sig-c"},
		{"type":"function_call","call_id":"c2","name":"lookup","arguments":"{}"},
		{"type":"function_call_output","call_id":"c2","output":"ok"},
		{"type":"reasoning","encrypted_content":"sig-d"},
		{"type":"message","role":"user","content":"more"},
		{"type":"reasoning"},
		{"type":"function_call","call_id":"c3","name":"lookup","arguments":"{}"}]`))
	if err != nil {
		t.Fatal(err)
	}
	var signatures []string
	for _, content := range contents {
		for _, part := range content.Parts {
			if part.FunctionCall != nil {
				signatures = append(signatures, part.FunctionCall.ThoughtSignature)
			}
		}
	}
	if strings.Join(signatures, ",") != "sig-a,sig-c," {
		t.Fatalf("函数调用的签名 = %q，期望 sig-a、sig-c 与空（message 清掉了 sig-d）", signatures)
	}
}

// TestResponsesIncompleteFunctionOutput status 为 incomplete 的 function_call_output 包成 {"status":"incomplete","output":…}，
// 让模型知道结果不完整；completed 与没有 status 的照旧
func TestResponsesIncompleteFunctionOutput(t *testing.T) {
	results := func(raw string) []string {
		t.Helper()
		contents, _, err := responsesContents(json.RawMessage(raw))
		if err != nil {
			t.Fatal(err)
		}
		var outputs []string
		for _, content := range contents {
			for _, part := range content.Parts {
				if part.FunctionResult != nil {
					outputs = append(outputs, string(part.FunctionResult.Content))
				}
			}
		}
		return outputs
	}
	got := results(`[
		{"type":"function_call","call_id":"c1","name":"shell","arguments":"{}"},
		{"type":"function_call_output","call_id":"c1","output":"partial log","status":"incomplete"},
		{"type":"function_call","call_id":"c2","name":"shell","arguments":"{}"},
		{"type":"function_call_output","call_id":"c2","output":{"exit":0},"status":"completed"},
		{"type":"function_call","call_id":"c3","name":"shell","arguments":"{}"},
		{"type":"function_call_output","call_id":"c3","output":"done"}]`)
	want := []string{`{"status":"incomplete","output":{"result":"partial log"}}`, `{"exit":0}`, `{"result":"done"}`}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("函数结果 = %q，期望 %q", got, want)
	}
}

// TestSingleMediaBlockFunctionResult 工具结果本身就是单个媒体内容块时按只含它的块数组处理：媒体取出来随后作为附件发送，原位置换成文字说明；
// 普通对象、网址图片与解码失败的单个块与原先一样原样发送
func TestSingleMediaBlockFunctionResult(t *testing.T) {
	for _, raw := range []string{
		`{"type":"image","data":"iVBORw0KGgo=","mimeType":"image/png"}`,
		`{"type":"input_image","image_url":"data:image/png;base64,iVBORw0KGgo="}`,
		`{"type":"image_url","image_url":{"url":"data:image/png;base64,iVBORw0KGgo="}}`,
		`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"iVBORw0KGgo="}}`,
	} {
		cleaned, media, err := splitFunctionResultMedia(json.RawMessage(raw))
		if err != nil || len(media) != 1 || media[0].InlineData == nil || media[0].InlineData.MIME != "image/png" || !media[0].FromToolResult {
			t.Fatalf("%s：media=%+v err=%v，期望取出一张 PNG", raw, media, err)
		}
		if string(cleaned) != `{"text":"[图片 1：已作为图片附在随后的消息中]","type":"text"}` {
			t.Fatalf("%s：原位置 = %s", raw, cleaned)
		}
	}
	for _, raw := range []string{
		`{"ok":true,"type":"image"}`,
		`{"type":"input_image","image_url":"https://example.com/a.png"}`,
		`{"type":"image","data":"%%%","mimeType":"image/png"}`,
		`{"type":"text","text":"hello"}`,
	} {
		cleaned, media, err := splitFunctionResultMedia(json.RawMessage(raw))
		if err != nil || len(media) != 0 || string(cleaned) != raw {
			t.Fatalf("%s：cleaned=%s media=%+v err=%v，期望原样", raw, cleaned, media, err)
		}
	}
	// Responses 的 function_call_output 输出为单个 input_image 对象时，图片随后作为用户消息里的附件发送
	contents, _, err := responsesContents(json.RawMessage(`[
		{"type":"function_call","call_id":"c1","name":"screenshot","arguments":"{}"},
		{"type":"function_call_output","call_id":"c1","output":{"type":"input_image","image_url":"data:image/png;base64,iVBORw0KGgo="}}]`))
	if err != nil {
		t.Fatal(err)
	}
	var result *aistudio.FunctionResult
	var attached *aistudio.Blob
	for _, content := range contents {
		for _, part := range content.Parts {
			if part.FunctionResult != nil {
				result = part.FunctionResult
			}
			if part.FromToolResult && part.InlineData != nil {
				attached = part.InlineData
			}
		}
	}
	if result == nil || strings.Contains(string(result.Content), "base64") || attached == nil || attached.MIME != "image/png" {
		t.Fatalf("函数结果=%+v 附件=%+v，期望图片取出、结果里不带 base64", result, attached)
	}
}
