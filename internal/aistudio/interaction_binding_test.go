package aistudio

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

// bindingCapture 为记录交给 DoProtected 的请求与 RPC 的受保护传输（测试上游不返回响应）
type bindingCapture struct {
	requests []GenerateRequest
	rpcs     []RPCRequest
}

func (capture *bindingCapture) DoProtected(_ context.Context, request GenerateRequest, rpc RPCRequest) (*RPCResponse, error) {
	capture.requests = append(capture.requests, request)
	capture.rpcs = append(capture.rpcs, rpc)
	return nil, errors.New("测试上游不返回响应")
}

// interactionBindingRun 经 generateInteraction 发送一次 Omni 请求，返回交给 DoProtected 的请求算出的 bindingPrompt（与 WorkerProtectedTransport
// 计算 proof 的输入相同）、请求体与 binding；enabled 为 nil 时不挂 OMNI_IMAGE_BINDING（按默认开启）
func interactionBindingRun(t *testing.T, contents []Content, enabled *bool) (string, []byte, []Content) {
	t.Helper()
	capture := &bindingCapture{}
	client, err := NewClient(ClientOptions{Transport: ceilingTransport{}, Protected: capture})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if enabled != nil {
		ctx = ContextWithInteractionImageBinding(ctx, *enabled)
	}
	request := GenerateRequest{ID: "req-binding", Model: "gemini-omni-1.1-flash", Contents: contents}
	if _, err := client.generateInteraction(ctx, request, modelEntry{model: omniTestModel(), defaults: omniTestDefaults()}); err == nil ||
		errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("应发往上游（测试上游返回错误），得到 %v", err)
	}
	if len(capture.requests) != 1 {
		t.Fatalf("应调用一次 DoProtected，得到 %d 次", len(capture.requests))
	}
	sent := capture.requests[0]
	if sent.Model != request.Model || sent.ID != request.ID {
		t.Fatalf("交给 DoProtected 的请求只应替换 Contents: %+v", sent)
	}
	prompt, err := bindingPrompt(sent)
	if err != nil {
		t.Fatal(err)
	}
	return prompt, capture.rpcs[0].Body, sent.Contents
}

// TestInteractionTextBindingUnchanged 纯文字的 Interaction 请求（多轮、相邻同角色合并、YouTube 链接、只有 Drive 引用的附件）：
// OMNI_IMAGE_BINDING 开启、关闭与不挂开关时交给 DoProtected 的 binding 与请求体逐字节相同，bindingPrompt 等于 a574e64 的规则
// （每条消息的文本按顺序以空格连接，YouTube 链接按消息去掉链接文本并补一个空占位，Drive 文件 ID 与移过去的说明之外的附件不参与）
func TestInteractionTextBindingUnchanged(t *testing.T) {
	on, off := true, false
	for _, test := range []struct {
		name     string
		contents []Content
		want     string
	}{
		{name: "多轮与合并", contents: []Content{
			{Role: RoleUser, Parts: []Part{{Text: "第一轮 https://youtu.be/abc123 看看"}}},
			{Role: RoleAssistant, Parts: []Part{{Text: "好的"}}},
			{Role: RoleUser, Parts: []Part{{Text: "再来"}}},
			{Role: RoleUser, Parts: []Part{{Text: "继续"}}},
			{Role: RoleAssistant, Parts: []Part{{Text: "第二轮"}, {Text: "还有", Thought: true}}},
			{Role: RoleUser, Parts: []Part{{Text: "最后"}}},
		}, want: " 第一轮  看看 好的 再来 继续 第二轮 最后"},
		{name: "Drive 引用", contents: []Content{
			{Role: RoleUser, Parts: []Part{{File: &FileRef{ID: "drive-text-only", MIME: "video/mp4"}}, {Text: "分析视频"}}},
		}, want: "分析视频"},
		{name: "模型历史视频", contents: []Content{
			{Role: RoleUser, Parts: []Part{{Text: "做个视频"}}},
			{Role: RoleAssistant, Parts: []Part{{Text: "好了"}, {File: &FileRef{ID: "drive-model-video", MIME: "video/mp4"}}}},
			{Role: RoleUser, Parts: []Part{{Text: "更长"}}},
		}, want: "做个视频 好了 " + interactionModelMediaIntro + " 更长"},
	} {
		t.Run(test.name, func(t *testing.T) {
			defaultPrompt, defaultBody, defaultBinding := interactionBindingRun(t, test.contents, nil)
			for _, enabled := range []*bool{&on, &off} {
				prompt, body, binding := interactionBindingRun(t, test.contents, enabled)
				if prompt != test.want || string(body) != string(defaultBody) || !reflect.DeepEqual(binding, defaultBinding) {
					t.Fatalf("开关=%v：bindingPrompt=%q（期望 %q），请求体或 binding 与默认不同", *enabled, prompt, test.want)
				}
			}
			if defaultPrompt != test.want {
				t.Fatalf("默认 bindingPrompt=%q，期望 %q", defaultPrompt, test.want)
			}
			if strings.Contains(defaultPrompt, "drive-") {
				t.Fatalf("Drive 文件 ID 不应参与 binding: %q", defaultPrompt)
			}
		})
	}
}

// TestInteractionImageBindingPrompt OMNI_IMAGE_BINDING 开启（默认）时，请求体里内联发送的图片按 GenerateContent 的写法（base64.StdEncoding
// 的原数据，与请求体里发送的是同一段字节）计入 binding，位置与它在 step 里的位置一致：bindingPrompt 等于“按请求体顺序的文本与图片 base64
// 以空格连接”。单图、多图、图在文本前后、GIF 转成的 PNG（用转换后的字节）、模型历史图片移到用户 step（说明之后）、与 Drive 引用并存
// （文件 ID 不计入）、YouTube 链接（处理不变）。关闭时只含文本，与 a574e64 相同；请求体与开关无关
func TestInteractionImageBindingPrompt(t *testing.T) {
	pngData, jpegData, gifData := omniTestPNG(t), omniTestJPEG(t), omniTestGIF(t)
	encode := base64.StdEncoding.EncodeToString
	png := func() Part { return Part{InlineData: &Blob{MIME: "image/png", Data: pngData}} }
	converted, report, err := PrepareInteractionImages(context.Background(), []Content{
		{Role: RoleUser, Parts: []Part{{InlineData: &Blob{MIME: "image/gif", Data: gifData}}, {Text: "动起来"}}},
	})
	if err != nil || report.Converted != 1 {
		t.Fatalf("GIF 应转成 PNG: %+v err=%v", report, err)
	}
	convertedPNG := converted[0].Parts[0].InlineData.Data
	on, off := true, false
	for _, test := range []struct {
		name     string
		contents []Content
		// on 与 off 为开关开启与关闭时 bindingPrompt 的各项（按请求体顺序，以空格连接）
		on, off []string
	}{
		{name: "单图", contents: []Content{{Role: RoleUser, Parts: []Part{png(), {Text: "随机视频"}}}},
			on: []string{encode(pngData), "随机视频"}, off: []string{"随机视频"}},
		{name: "多图", contents: []Content{{Role: RoleUser, Parts: []Part{
			png(), {InlineData: &Blob{MIME: "image/webp", Data: omniTestWebP}}, {InlineData: &Blob{MIME: "image/jpeg", Data: jpegData}}, {Text: "三张"},
		}}}, on: []string{encode(pngData), encode(omniTestWebP), encode(jpegData), "三张"}, off: []string{"三张"}},
		{name: "图夹在文本之间", contents: []Content{{Role: RoleUser, Parts: []Part{
			{Text: "先看"}, {InlineData: &Blob{MIME: "image/jpeg", Data: jpegData}}, {Text: "再动"},
		}}}, on: []string{"先看", encode(jpegData), "再动"}, off: []string{"先看", "再动"}},
		{name: "图在文本后", contents: []Content{{Role: RoleUser, Parts: []Part{{Text: "随机视频"}, png()}}},
			on: []string{"随机视频", encode(pngData)}, off: []string{"随机视频"}},
		{name: "GIF 转成的 PNG", contents: converted, on: []string{encode(convertedPNG), "动起来"}, off: []string{"动起来"}},
		{name: "模型历史图片移到用户 step", contents: []Content{
			{Role: RoleUser, Parts: []Part{{Text: "画一只猫"}}},
			{Role: RoleAssistant, Parts: []Part{{Text: "好了"}, {InlineData: &Blob{MIME: "image/png", Data: pngData}, RestoredLink: true}}},
			{Role: RoleUser, Parts: []Part{{Text: "让它动起来"}}},
		}, on: []string{"画一只猫", "好了", interactionModelMediaIntro, encode(pngData), "让它动起来"},
			off: []string{"画一只猫", "好了", interactionModelMediaIntro, "让它动起来"}},
		{name: "以模型图片结尾", contents: []Content{
			{Role: RoleUser, Parts: []Part{{Text: "画一只猫"}}},
			{Role: RoleAssistant, Parts: []Part{{Text: "好了"}, {InlineData: &Blob{MIME: "image/jpeg", Data: jpegData}}}},
		}, on: []string{"画一只猫", "好了", interactionModelMediaIntro, encode(jpegData)}, off: []string{"画一只猫", "好了", interactionModelMediaIntro}},
		{name: "与 Drive 引用并存", contents: []Content{{Role: RoleUser, Parts: []Part{
			png(), {File: &FileRef{ID: "drive-video-together", MIME: "video/mp4"}}, {Text: "参考图片和视频"},
		}}}, on: []string{encode(pngData), "参考图片和视频"}, off: []string{"参考图片和视频"}},
		{name: "YouTube 链接", contents: []Content{{Role: RoleUser, Parts: []Part{{Text: "看 https://youtu.be/xyz789 这个"}, png()}}},
			on: []string{"", "看  这个", encode(pngData)}, off: []string{"", "看  这个"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			defaultPrompt, defaultBody, _ := interactionBindingRun(t, test.contents, nil)
			onPrompt, onBody, _ := interactionBindingRun(t, test.contents, &on)
			offPrompt, offBody, offBinding := interactionBindingRun(t, test.contents, &off)
			if want := strings.Join(test.on, " "); onPrompt != want || defaultPrompt != want {
				t.Fatalf("开启时 bindingPrompt =\n%q\n默认 %q\n期望 %q", onPrompt, defaultPrompt, want)
			}
			if want := strings.Join(test.off, " "); offPrompt != want {
				t.Fatalf("关闭时 bindingPrompt = %q，期望 %q", offPrompt, want)
			}
			if string(onBody) != string(offBody) || string(onBody) != string(defaultBody) {
				t.Fatal("请求体应与开关无关")
			}
			for _, content := range offBinding {
				for _, part := range content.Parts {
					if part.InlineData != nil || part.File != nil {
						t.Fatalf("关闭时 binding 只含文本（与 a574e64 相同）: %+v", offBinding)
					}
				}
			}
			if strings.Contains(onPrompt, "drive-") {
				t.Fatalf("Drive 文件 ID 不应参与 binding: %.200q", onPrompt)
			}
		})
	}
	// GIF 转换前的字节不参与：binding 与请求体里发送的是转换后的 PNG
	if prompt, body, _ := interactionBindingRun(t, converted, &on); strings.Contains(prompt, encode(gifData)) ||
		!strings.Contains(string(body), encode(convertedPNG)) {
		t.Fatal("GIF 应按转换后的 PNG 字节计入 binding 与请求体")
	}
	// 历史图片的位置与请求体一致：说明、图片在随后那条用户消息的文本之前
	history := []Content{
		{Role: RoleUser, Parts: []Part{{Text: "画一只猫"}}},
		{Role: RoleAssistant, Parts: []Part{{Text: "好了"}, png()}},
		{Role: RoleUser, Parts: []Part{{Text: "让它动起来"}}},
	}
	_, body, binding := interactionBindingRun(t, history, &on)
	encodedBinding, _ := json.Marshal(binding)
	if !strings.Contains(string(body), `[[["`+interactionModelMediaIntro+`"]],[null,[1,"`+encode(pngData)+`"]],[["让它动起来"]]]`) ||
		!strings.HasSuffix(string(encodedBinding), `{"role":"user","parts":[{"text":"`+interactionModelMediaIntro+`"},{"inline_data":{"mime":"image/png","data":"`+
			encode(pngData)+`"}},{"text":"让它动起来"}]}]`) {
		t.Fatalf("历史图片的 binding 位置应与请求体一致: %s", encodedBinding)
	}
}

// TestGenerateContentAndBuildBindingUnchanged GenerateContent 与 Build 的 binding 与 OMNI_IMAGE_BINDING 无关：带图的 GenerateContent 请求交给
// DoProtected 的请求与开关无关（bindingPrompt 照旧是文本与图片 base64），Build 的代理请求体（Build 的 binding 从它算出）逐字节相同
func TestGenerateContentAndBuildBindingUnchanged(t *testing.T) {
	pngData := omniTestPNG(t)
	request := GenerateRequest{ID: "req-content", Model: "gemini-test", AccountID: "content@example.com", Contents: []Content{
		{Role: RoleUser, Parts: []Part{{Text: "描述"}, {InlineData: &Blob{MIME: "image/png", Data: pngData}}}},
	}}
	entry := modelEntry{model: Model{ID: "gemini-test", Methods: []string{"generateContent"}}, defaults: GenerationDefaults{MaxOutputTokens: 1024}}
	on, off := true, false
	var playground, build []*bindingCapture
	for _, enabled := range []*bool{nil, &on, &off} {
		ctx := context.Background()
		if enabled != nil {
			ctx = ContextWithInteractionImageBinding(ctx, *enabled)
		}
		capture := &bindingCapture{}
		client, err := NewClient(ClientOptions{Transport: ceilingTransport{}, Protected: capture})
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := client.sendPlayground(ctx, request, entry, ctx, &longOutputRounds{}); err == nil {
			t.Fatal("测试上游应返回错误")
		}
		playground = append(playground, capture)
		buildCapture := &bindingCapture{}
		buildClient, err := NewClient(ClientOptions{Transport: ceilingTransport{}, Protected: buildCapture})
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := buildClient.sendBuild(ctx, request, entry); err == nil {
			t.Fatal("测试上游应返回错误")
		}
		build = append(build, buildCapture)
	}
	want := "描述 " + base64.StdEncoding.EncodeToString(pngData)
	for index, capture := range playground {
		if len(capture.requests) != 1 || !reflect.DeepEqual(capture.requests[0], request) {
			t.Fatalf("GenerateContent 交给 DoProtected 的请求应原样: %+v", capture.requests)
		}
		if prompt, err := bindingPrompt(capture.requests[0]); err != nil || prompt != want {
			t.Fatalf("GenerateContent bindingPrompt = %.80q，期望 %.80q（err=%v）", prompt, want, err)
		}
		if string(capture.rpcs[0].Body) != string(playground[0].rpcs[0].Body) {
			t.Fatalf("第 %d 次 GenerateContent 请求体与开关有关", index)
		}
	}
	first, err := buildBindingPrompt(build[0].rpcs[0].Body)
	if err != nil || first == "" {
		t.Fatalf("Build binding: %q err=%v", first, err)
	}
	for _, capture := range build {
		prompt, err := buildBindingPrompt(capture.rpcs[0].Body)
		if len(capture.rpcs) != 1 || string(capture.rpcs[0].Body) != string(build[0].rpcs[0].Body) || err != nil || prompt != first ||
			!reflect.DeepEqual(capture.requests[0], request) {
			t.Fatal("Build 的代理请求体与 binding 应与开关无关")
		}
	}
}

// TestInteractionImageRejectedError 带图 403 结束时对外的说明：400 FAILED_PRECONDITION，英文写明 Omni 拒绝了带图请求、可以去掉图片重试；
// 不含会让中转自动禁用渠道的关键词与上游原文，内部说明（管理日志）带着上游错误与开关取值
func TestInteractionImageRejectedError(t *testing.T) {
	cause := &RPCError{Method: interactionMethod, StatusCode: http.StatusForbidden, Code: 7, Message: "The caller does not have permission"}
	err := &InteractionImageRejectedError{Model: "gemini-omni-1.1-flash", Tried: 2, Images: 1, ImageBinding: true, Cause: cause}
	if err.HTTPStatus() != http.StatusBadRequest || err.RPCStatus() != "FAILED_PRECONDITION" {
		t.Fatalf("状态 = %d %s", err.HTTPStatus(), err.RPCStatus())
	}
	public := err.PublicMessage()
	if public != "The Omni model gemini-omni-1.1-flash rejected this request because it contains image input (tried on 2 accounts). "+
		"Remove the images and send the request again; text-only requests are not affected." {
		t.Fatalf("对外说明 = %q", public)
	}
	lower := strings.ToLower(public)
	for _, keyword := range []string{"permission", "denied", "quota", "insufficient", "api key", "not allowed", "not authorized", "disabled",
		"credit", "balance", "billing", "organization", "forbidden", "caller"} {
		if strings.Contains(lower, keyword) {
			t.Fatalf("对外说明不应含 %q: %s", keyword, public)
		}
	}
	if single := (&InteractionImageRejectedError{Tried: 1}).PublicMessage(); !strings.Contains(single, "The Omni model rejected") ||
		!strings.Contains(single, "(tried on 1 account)") {
		t.Fatalf("单个账户的说明 = %q", single)
	}
	var rpcError *RPCError
	if !errors.As(err, &rpcError) || rpcError != cause || !strings.Contains(err.Error(), "OMNI_IMAGE_BINDING=true") ||
		!strings.Contains(err.Error(), "试过 2 个账户") {
		t.Fatalf("内部说明应带上游错误与开关取值: %v", err)
	}
	if !InteractionImageBindingFromContext(context.Background()) {
		t.Fatal("没有挂开关时应按默认开启")
	}
	if InteractionImageBindingFromContext(ContextWithInteractionImageBinding(context.Background(), false)) {
		t.Fatal("挂了关闭时应为关闭")
	}
	contents := []Content{
		{Role: RoleUser, Parts: []Part{{InlineData: &Blob{MIME: "image/png", Data: omniTestPNG(t)}}, {Text: "看"}}},
		{Role: RoleAssistant, Parts: []Part{{InlineData: &Blob{MIME: "image/jpeg", Data: omniTestJPEG(t)}}, {InlineData: &Blob{MIME: "image/png", Data: omniTestPNG(t)}, Thought: true}}},
		{Role: RoleUser, Parts: []Part{{InlineData: &Blob{MIME: "video/mp4", Data: []byte("\x00\x00\x00\x18ftypmp42")}}, {File: &FileRef{ID: "drive-x"}}}},
	}
	if count := InteractionInlineImageCount(contents); count != 2 {
		t.Fatalf("内联图片数 = %d，期望 2（思考 part、视频与 Drive 引用不算）", count)
	}
}
