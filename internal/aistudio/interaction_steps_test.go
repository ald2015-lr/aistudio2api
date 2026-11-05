package aistudio

import (
	"encoding/json"
	"slices"
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
	steps, binding, hasModelTurn, err := encodeInteractionSteps(contents, false)
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

	// 以模型消息结尾时附件单独成一条用户消息：前面是模型 step 时单独成一条用户 step
	steps, binding, hasModelTurn, err = encodeInteractionSteps([]Content{
		{Role: RoleUser, Parts: []Part{{Text: "画"}}},
		{Role: RoleAssistant, Parts: []Part{{Text: "好了"}, {File: &FileRef{ID: "drive-last"}}}},
	}, false)
	encoded, _ = json.Marshal(steps)
	if err != nil || !hasModelTurn || string(encoded) != `[[[[[["画"]]]]],[null,[[[["好了"]]]]],[[[`+intro+`,`+file("drive-last")+`]]]]` {
		t.Fatalf("结尾的模型附件应单独成一条用户 step: steps=%s hasModelTurn=%v err=%v", encoded, hasModelTurn, err)
	}
	if encodedBinding, _ = json.Marshal(binding); strings.Count(string(encodedBinding), `"role":"user"`) != 2 {
		t.Fatalf("binding = %s", encodedBinding)
	}
	// 只有附件的模型消息整条省略后，前后两条用户消息相邻，附件那一条并入前一条用户 step（说明与附件接在原文本之后）；binding 不随 step 合并，
	// 说明文字仍单独一条
	steps, binding, hasModelTurn, err = encodeInteractionSteps([]Content{
		{Role: RoleUser, Parts: []Part{{Text: "画"}}},
		{Role: RoleAssistant, Parts: []Part{{File: &FileRef{ID: "drive-last"}}}},
	}, false)
	encoded, _ = json.Marshal(steps)
	if err != nil || hasModelTurn || string(encoded) != `[[[[[["画"]],`+intro+`,`+file("drive-last")+`]]]]` {
		t.Fatalf("结尾的模型附件应并入前一条用户 step: steps=%s hasModelTurn=%v err=%v", encoded, hasModelTurn, err)
	}
	if encodedBinding, _ = json.Marshal(binding); string(encodedBinding) !=
		`[{"role":"user","parts":[{"text":"画"}]},{"role":"user","parts":[{"text":"`+interactionModelMediaIntro+`"}]}]` {
		t.Fatalf("binding = %s", encodedBinding)
	}
}

// TestInteractionMergesAdjacentSteps 相邻同角色的消息合并为一条 step（与上游 encodeInteractionSteps 相同）：连续的用户消息、只有附件的模型消息
// 省略后相邻的用户消息、连续的模型消息都合并，part 按原顺序接在后面；模型历史媒体照旧移到随后那条用户消息的最前面（合并后在这条消息的
// 位置上）；binding 不随 step 合并，仍按消息逐条生成（与合并前、与上游相同），bindingPrompt 的结果与合并前逐字节相同（含 bindingPrompt
// 按消息处理的 YouTube 链接）；只有用户消息时不算多轮
func TestInteractionMergesAdjacentSteps(t *testing.T) {
	file := func(id string) string { return `[null,null,null,null,null,null,null,null,["` + id + `"]]` }
	intro := `[["` + interactionModelMediaIntro + `"]]`
	contents := []Content{
		{Role: RoleUser, Parts: []Part{{Text: "第一句"}}},
		{Role: RoleUser, Parts: []Part{{File: &FileRef{ID: "drive-a"}}, {Text: "第二句"}}},
		{Role: RoleAssistant, Parts: []Part{{Text: "回复一"}}},
		{Role: RoleAssistant, Parts: []Part{{Text: "回复二"}, {File: &FileRef{ID: "drive-model"}}}},
		{Role: RoleUser, Parts: []Part{{Text: "第三句"}}},
		{Role: RoleAssistant, Parts: []Part{{File: &FileRef{ID: "drive-only"}}}},
		{Role: RoleUser, Parts: []Part{{Text: "第四句"}}},
	}
	steps, binding, hasModelTurn, err := encodeInteractionSteps(contents, false)
	if err != nil || !hasModelTurn {
		t.Fatalf("hasModelTurn=%v err=%v", hasModelTurn, err)
	}
	encoded, _ := json.Marshal(steps)
	want := `[[[[[["第一句"]],` + file("drive-a") + `,[["第二句"]]]]],[null,[[[["回复一"]],[["回复二"]]]]],` +
		`[[[` + intro + `,` + file("drive-model") + `,[["第三句"]],` + intro + `,` + file("drive-only") + `,[["第四句"]]]]]]`
	if string(encoded) != want {
		t.Fatalf("steps = %s\n期望 %s", encoded, want)
	}
	encodedBinding, _ := json.Marshal(binding)
	wantBinding := `[{"role":"user","parts":[{"text":"第一句"}]},{"role":"user","parts":[{"text":"第二句"}]},` +
		`{"role":"assistant","parts":[{"text":"回复一"}]},{"role":"assistant","parts":[{"text":"回复二"}]},` +
		`{"role":"user","parts":[{"text":"` + interactionModelMediaIntro + `"},{"text":"第三句"}]},` +
		`{"role":"user","parts":[{"text":"` + interactionModelMediaIntro + `"},{"text":"第四句"}]}]`
	if string(encodedBinding) != wantBinding {
		t.Fatalf("binding = %s\n期望 %s", encodedBinding, wantBinding)
	}
	merged, err := bindingPrompt(GenerateRequest{Contents: binding})
	if err != nil {
		t.Fatal(err)
	}
	if want := "第一句 第二句 回复一 回复二 " + interactionModelMediaIntro + " 第三句 " + interactionModelMediaIntro + " 第四句"; merged != want {
		t.Fatalf("binding 文本 = %q，期望 %q（与合并前逐条拼接相同）", merged, want)
	}

	// 相邻两条用户消息含同一个 YouTube 链接：bindingPrompt 按消息去掉链接文本、各补一个占位（只在同一条消息内去重），binding 若随 step
	// 合并成一条就只剩一个占位、少一个空格；按消息逐条生成时与合并前逐条拼接的结果逐字节相同
	const video = "https://www.youtube.com/watch?v=dQw4w9WgXcQ"
	youtube := []Content{
		{Role: RoleUser, Parts: []Part{{Text: "看 " + video}}},
		{Role: RoleUser, Parts: []Part{{Text: "再看 " + video}}},
	}
	steps, binding, _, err = encodeInteractionSteps(youtube, false)
	if err != nil {
		t.Fatal(err)
	}
	if encoded, _ = json.Marshal(steps); string(encoded) != `[[[[[["看 `+video+`"]],[["再看 `+video+`"]]]]]]` {
		t.Fatalf("两条用户消息应合并为一条 step: %s", encoded)
	}
	got, err := bindingPrompt(GenerateRequest{Contents: binding})
	if err != nil {
		t.Fatal(err)
	}
	separate := make([]string, 0, len(youtube))
	for _, content := range youtube {
		value, err := bindingPrompt(GenerateRequest{Contents: []Content{content}})
		if err != nil {
			t.Fatal(err)
		}
		separate = append(separate, value)
	}
	if want := strings.Join(separate, " "); got != want || got != " 看  再看" {
		t.Fatalf("YouTube 链接的 binding 文本 = %q，期望与逐条拼接的 %q 相同（%q）", got, want, " 看  再看")
	}
	mergedOnce, err := bindingPrompt(GenerateRequest{Contents: []Content{{Role: RoleUser, Parts: append(slices.Clone(youtube[0].Parts), youtube[1].Parts...)}}})
	if err != nil || mergedOnce == got {
		t.Fatalf("前提：合并成一条消息时 bindingPrompt 的结果（%q）应与逐条拼接不同，err=%v", mergedOnce, err)
	}

	// 只有连续的用户消息：合并为一条用户 step，不算多轮（可以带视频任务）
	steps, _, hasModelTurn, err = encodeInteractionSteps([]Content{
		{Role: RoleUser, Parts: []Part{{Text: "参考这张"}}},
		{Role: RoleUser, Parts: []Part{{Text: "生成视频"}}},
	}, false)
	encoded, _ = json.Marshal(steps)
	if err != nil || hasModelTurn || string(encoded) != `[[[[[["参考这张"]],[["生成视频"]]]]]]` {
		t.Fatalf("连续用户消息: steps=%s hasModelTurn=%v err=%v", encoded, hasModelTurn, err)
	}
}

// TestInteractionModelAttachmentErrors 移到用户 step 的模型附件按用户附件的规则编码：没有 Drive 授权、仍是 inline data 的视频与不能内联的
// 图片与用户附件一样返回需要 Drive 授权的错误（指明所在的模型消息），外部地址不接受；不静默丢掉。说明为英文（原样返回客户端），不含中文
func TestInteractionModelAttachmentErrorsEnglish(t *testing.T) {
	for _, contents := range [][]Content{
		{{Role: RoleUser, Parts: []Part{{InlineData: &Blob{MIME: "video/mp4", Data: []byte("video")}}}}},
		{{Role: RoleUser, Parts: []Part{{InlineData: &Blob{MIME: "image/avif", Data: omniTestAVIF}}}}},
		{{Role: RoleUser, Parts: []Part{{InlineData: &Blob{MIME: "image/png", Data: []byte("not a png")}}}}},
		{{Role: RoleUser, Parts: []Part{{ExternalMedia: &ExternalMedia{MIME: "image/png", URL: "https://images.example.net/a.png"}}}}},
		{{Role: RoleUser, Parts: []Part{{FunctionResult: &FunctionResult{Name: "f"}}}}},
		{{Role: Role("system"), Parts: []Part{{Text: "x"}}}},
		{},
	} {
		_, _, _, err := encodeInteractionSteps(contents, false)
		if err == nil {
			t.Fatalf("%+v 应返回错误", contents)
		}
		// 参数错误的哨兵前缀（中文）由 API 层去掉，其后的说明原样返回客户端
		for _, r := range strings.TrimPrefix(err.Error(), ErrInvalidArgument.Error()) {
			if r >= 0x4e00 && r <= 0x9fff {
				t.Fatalf("错误说明应为英文：%v", err)
			}
		}
	}
}

// TestInteractionModelAttachmentErrors 移到用户 step 的模型附件按用户附件的规则编码：没有 Drive 授权、仍是 inline data 的视频与不能内联的
// 图片与用户附件一样返回需要 Drive 授权的错误（指明所在的模型消息），外部地址不接受；不静默丢掉
func TestInteractionModelAttachmentErrors(t *testing.T) {
	video := &Blob{MIME: "video/mp4", Data: []byte("video")}
	avif := &Blob{MIME: "image/avif", Data: omniTestAVIF}
	const videoWant = "video, audio and document input for this model must be uploaded to Google Drive first, which requires an account with Google Drive authorized"
	for _, test := range []struct {
		name     string
		contents []Content
		want     string
	}{
		{
			name: "用户内联视频",
			contents: []Content{
				{Role: RoleUser, Parts: []Part{{Text: "看视频"}, {InlineData: video}}},
			},
			want: "contents[0]: " + videoWant,
		},
		{
			name: "用户 AVIF 图片",
			contents: []Content{
				{Role: RoleUser, Parts: []Part{{Text: "看图"}, {InlineData: avif}}},
			},
			want: "contents[0]: this image/avif image cannot be sent inline to this model (only PNG, JPEG, WebP, HEIC, HEIF, BMP and TIFF images are, " +
				"and GIF is converted to PNG) and must be uploaded to Google Drive first",
		},
		{
			name: "模型内联视频",
			contents: []Content{
				{Role: RoleUser, Parts: []Part{{Text: "画"}}},
				{Role: RoleAssistant, Parts: []Part{{InlineData: video, ThoughtSignature: "sig"}, {Text: "好了"}}},
				{Role: RoleUser, Parts: []Part{{Text: "动起来"}}},
			},
			want: "contents[1]: " + videoWant,
		},
		{
			name: "模型消息结尾的内联视频",
			contents: []Content{
				{Role: RoleUser, Parts: []Part{{Text: "画"}}},
				{Role: RoleAssistant, Parts: []Part{{InlineData: video}}},
			},
			want: "contents[1]: " + videoWant,
		},
		{
			name: "模型外部地址",
			contents: []Content{
				{Role: RoleUser, Parts: []Part{{Text: "看这个"}}},
				{Role: RoleAssistant, Parts: []Part{{ExternalMedia: &ExternalMedia{MIME: "image/png", URL: "https://images.example.net/a.png"}}, {Text: "好"}}},
				{Role: RoleUser, Parts: []Part{{Text: "动起来"}}},
			},
			want: "contents[1]: this model only accepts inline or uploaded image and video input; external URLs are not supported",
		},
	} {
		_, _, _, err := encodeInteractionSteps(test.contents, false)
		if err == nil || !strings.Contains(err.Error(), test.want) {
			t.Fatalf("%s: 期望错误含 %q，实际 %v", test.name, test.want, err)
		}
	}
}
