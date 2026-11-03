package app

import (
	"strings"
	"testing"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// TestApplyPromptNonceKeepsTrailingURLIntact 末尾是链接时随机后缀单独成段，不污染链接
func TestApplyPromptNonceKeepsTrailingURLIntact(t *testing.T) {
	nonce := newPromptNonce()
	for _, text := range []string{
		"总结这个视频 https://youtu.be/dQw4w9WgXcQ",
		"总结 https://www.youtube.com/watch?v=dQw4w9WgXcQ\n",
	} {
		request := aistudio.GenerateRequest{Contents: []aistudio.Content{
			{Role: aistudio.RoleUser, Parts: []aistudio.Part{{Text: text}}},
		}}
		modified, ok := applyPromptNonce(request, nonce)
		if !ok {
			t.Fatalf("%q: 没有加入后缀", text)
		}
		parts := modified.Contents[0].Parts
		if len(parts) != 2 || parts[0].Text != text || parts[1].Text != nonce {
			t.Fatalf("%q: 后缀应单独成段，得到 %#v", text, parts)
		}
		fields := strings.Fields(parts[0].Text)
		media, ok := aistudio.ExternalMediaForURL(fields[len(fields)-1])
		if !ok || media.URL != "https://www.youtube.com/watch?v=dQw4w9WgXcQ" {
			t.Fatalf("%q: 链接被改写为 %#v", text, media)
		}
		if request.Contents[0].Parts[0].Text != text || len(request.Contents[0].Parts) != 1 {
			t.Fatal("调用方的原始内容被修改")
		}
	}
}

// TestApplyPromptNonceAppendsToPlainText 普通文本仍把后缀接在最后一段末尾
func TestApplyPromptNonceAppendsToPlainText(t *testing.T) {
	nonce := newPromptNonce()
	request := aistudio.GenerateRequest{Contents: []aistudio.Content{
		{Role: aistudio.RoleUser, Parts: []aistudio.Part{{Text: "你好"}}},
	}}
	modified, ok := applyPromptNonce(request, nonce)
	if !ok || len(modified.Contents[0].Parts) != 1 || modified.Contents[0].Parts[0].Text != "你好"+nonce {
		t.Fatalf("得到 %#v", modified.Contents[0].Parts)
	}
}

// TestApplyPromptNonceKeepsFromSystem 最后一条用户消息是 system 角色消息转成的轮次（Anthropic messages 以 system 消息结尾）时，
// 加后缀后 FromSystem 标记保留：Omni 发送前补做系统提示去除要靠它认出这一轮
func TestApplyPromptNonceKeepsFromSystem(t *testing.T) {
	nonce := newPromptNonce()
	request := aistudio.GenerateRequest{Contents: []aistudio.Content{
		{Role: aistudio.RoleUser, Parts: []aistudio.Part{{Text: "生成一个猫的视频"}}},
		{Role: aistudio.RoleUser, FromSystem: true, Parts: []aistudio.Part{{Text: "<system-reminder>只回中文</system-reminder>"}}},
	}}
	modified, ok := applyPromptNonce(request, nonce)
	if !ok || !modified.Contents[1].FromSystem || modified.Contents[1].Parts[0].Text != "<system-reminder>只回中文</system-reminder>"+nonce {
		t.Fatalf("得到 %+v", modified.Contents)
	}
	if request.Contents[1].Parts[0].Text != "<system-reminder>只回中文</system-reminder>" {
		t.Fatal("调用方的原始内容被修改")
	}
}
