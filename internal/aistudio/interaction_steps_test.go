package aistudio

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestInteractionMovesModelAttachments 模型消息里的附件（回传的生成图片、恢复的媒体链接，上传后为 Drive 引用）连同说明文字移到
// 紧随其后的用户 step 最前面，模型 step 只带文本，只有附件的模型消息整条省略；binding 含移过去的说明文字，不含文件 ID；
// 用户的 Drive 附件照常发送
func TestInteractionMovesModelAttachments(t *testing.T) {
	contents := []Content{
		{Role: RoleUser, Parts: []Part{{Text: "画一只猫"}, {File: &FileRef{ID: "drive-user"}}}},
		{Role: RoleAssistant, Parts: []Part{{File: &FileRef{ID: "drive-model-1"}, ThoughtSignature: "sig"}, {Text: "猫画好了"}}},
		{Role: RoleUser, Parts: []Part{{Text: "再来"}}},
		{Role: RoleAssistant, Parts: []Part{{File: &FileRef{ID: "drive-model-2"}}}},
		{Role: RoleAssistant, Parts: []Part{{Text: "第二张"}, {File: &FileRef{ID: "drive-model-3"}}}},
		{Role: RoleUser, Parts: []Part{{Text: "让它动起来"}}},
	}
	steps, binding, hasModelTurn, err := encodeInteractionSteps(contents)
	if err != nil {
		t.Fatalf("模型消息里的 Drive 附件不应让请求失败: %v", err)
	}
	encoded, _ := json.Marshal(steps)
	file := func(id string) string { return `[null,null,null,null,null,null,null,null,["` + id + `"]]` }
	intro := `[["` + interactionModelMediaIntro + `"]]`
	want := `[[[[[["画一只猫"]],` + file("drive-user") + `]]],[null,[[[["猫画好了"]]]]],` +
		`[[[` + intro + `,` + file("drive-model-1") + `,[["再来"]]]]],[null,[[[["第二张"]]]]],` +
		`[[[` + intro + `,` + file("drive-model-2") + `,` + file("drive-model-3") + `,[["让它动起来"]]]]]]`
	if string(encoded) != want {
		t.Fatalf("steps = %s\n期望 %s", encoded, want)
	}
	if !hasModelTurn {
		t.Fatal("带文本的模型消息仍是模型 step")
	}
	encodedBinding, _ := json.Marshal(binding)
	wantBinding := `[{"role":"user","parts":[{"text":"画一只猫"}]},{"role":"assistant","parts":[{"text":"猫画好了"}]},` +
		`{"role":"user","parts":[{"text":"` + interactionModelMediaIntro + `"},{"text":"再来"}]},{"role":"assistant","parts":[{"text":"第二张"}]},` +
		`{"role":"user","parts":[{"text":"` + interactionModelMediaIntro + `"},{"text":"让它动起来"}]}]`
	if string(encodedBinding) != wantBinding {
		t.Fatalf("binding = %s\n期望 %s", encodedBinding, wantBinding)
	}

	// 以模型消息结尾时附件单独成一条用户 step
	steps, binding, hasModelTurn, err = encodeInteractionSteps([]Content{
		{Role: RoleUser, Parts: []Part{{Text: "画"}}},
		{Role: RoleAssistant, Parts: []Part{{File: &FileRef{ID: "drive-last"}}}},
	})
	encoded, _ = json.Marshal(steps)
	if err != nil || hasModelTurn || string(encoded) != `[[[[[["画"]]]]],[[[`+intro+`,`+file("drive-last")+`]]]]` {
		t.Fatalf("结尾的模型附件应单独成一条用户 step: steps=%s hasModelTurn=%v err=%v", encoded, hasModelTurn, err)
	}
	if encodedBinding, _ = json.Marshal(binding); strings.Count(string(encodedBinding), `"role":"user"`) != 2 {
		t.Fatalf("binding = %s", encodedBinding)
	}
}

// TestInteractionModelAttachmentErrors 移到用户 step 的模型附件按用户附件的规则编码：没有 Drive 授权、仍是 inline data 时与用户附件
// 一样返回需要 Drive 授权的错误（指明所在的模型消息），外部地址不接受；不静默丢掉
func TestInteractionModelAttachmentErrors(t *testing.T) {
	image := &Blob{MIME: "image/png", Data: []byte("\x89PNG")}
	for _, test := range []struct {
		name     string
		contents []Content
		want     string
	}{
		{
			name: "用户内联附件",
			contents: []Content{
				{Role: RoleUser, Parts: []Part{{Text: "看图"}, {InlineData: image}}},
			},
			want: "contents[0] 附件需要账户 Drive 授权",
		},
		{
			name: "模型内联附件",
			contents: []Content{
				{Role: RoleUser, Parts: []Part{{Text: "画"}}},
				{Role: RoleAssistant, Parts: []Part{{InlineData: image, ThoughtSignature: "sig"}, {Text: "好了"}}},
				{Role: RoleUser, Parts: []Part{{Text: "动起来"}}},
			},
			want: "contents[1] 附件需要账户 Drive 授权",
		},
		{
			name: "模型消息结尾的内联附件",
			contents: []Content{
				{Role: RoleUser, Parts: []Part{{Text: "画"}}},
				{Role: RoleAssistant, Parts: []Part{{InlineData: image}}},
			},
			want: "contents[1] 附件需要账户 Drive 授权",
		},
		{
			name: "模型外部地址",
			contents: []Content{
				{Role: RoleUser, Parts: []Part{{Text: "看这个"}}},
				{Role: RoleAssistant, Parts: []Part{{ExternalMedia: &ExternalMedia{MIME: "image/png", URL: "https://images.example.net/a.png"}}, {Text: "好"}}},
				{Role: RoleUser, Parts: []Part{{Text: "动起来"}}},
			},
			want: "contents[1] 只接受文本与用户附件 part",
		},
	} {
		_, _, _, err := encodeInteractionSteps(test.contents)
		if err == nil || !strings.Contains(err.Error(), test.want) {
			t.Fatalf("%s: 期望错误含 %q，实际 %v", test.name, test.want, err)
		}
	}
}
