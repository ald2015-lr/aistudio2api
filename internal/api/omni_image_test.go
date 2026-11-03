package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"net/http"
	"strings"
	"testing"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// omniImageFixtures 返回测试里现场生成的小 PNG（4×3）、小 JPEG（image/jpeg 编码，8×6）与 GIF（3×2）
func omniImageFixtures(t *testing.T) (pngData, jpegData, gifData []byte) {
	t.Helper()
	canvas := image.NewNRGBA(image.Rect(0, 0, 8, 6))
	for x := range 8 {
		for y := range 6 {
			canvas.Set(x, y, color.NRGBA{R: uint8(30 * x), G: uint8(40 * y), B: 160, A: 255})
		}
	}
	var buffer bytes.Buffer
	if err := png.Encode(&buffer, canvas.SubImage(image.Rect(0, 0, 4, 3))); err != nil {
		t.Fatal(err)
	}
	pngData = bytes.Clone(buffer.Bytes())
	buffer.Reset()
	if err := jpeg.Encode(&buffer, canvas, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatal(err)
	}
	jpegData = bytes.Clone(buffer.Bytes())
	buffer.Reset()
	frame := image.NewPaletted(image.Rect(0, 0, 3, 2), color.Palette{color.Black, color.White, color.RGBA{G: 255, A: 255}})
	frame.SetColorIndex(1, 1, 2)
	if err := gif.Encode(&buffer, frame, nil); err != nil {
		t.Fatal(err)
	}
	return pngData, jpegData, bytes.Clone(buffer.Bytes())
}

// omniImageCaptureBody 按官网抓包的结构写出 CreateInteractionStream 请求体（第 5 项 WAA proof 为 null，见 aistudio 的同名测试）：
// parts 为唯一一条用户 step 的各个 part，video 为 field 54 视频格式的 field 4
func omniImageCaptureBody(parts string, video string) string {
	return `[1,1,null,[null,null,null,null,null,null,null,null,null,null,null,null,null,null,null,null,null,` +
		`["models/gemini-omni-1.1-flash",[null,null,null,null,null,4,1,65536,null,null,null,null,null,null,null,null,null,null,null,null,null,null,null,null,[]]],` +
		`null,null,null,null,null,null,null,null,[[[[[` + parts + `]]]]],` +
		`null,null,null,null,null,null,null,null,null,null,null,null,null,null,null,null,null,null,null,null,null,null,null,null,null,null,` +
		`[[[null,null,null,` + video + `]]]],null,1]`
}

// TestOmniImageOfficialCapture 经协议入口发送“图片在前、文本在后”的用户消息（Gemini inlineData、OpenAI image_url data URI、
// Anthropic base64 image、Interactions image），整理图片并编码出的 CreateInteractionStream 请求体与官网抓包的结构逐字节相同：
// PNG 为 [null,[1,Base64]]、JPEG 为 [null,[2,Base64]]（原文件字节），GIF 转成 PNG 后按枚举 1；JPEG 那份另设 10 秒时长（模型后缀
// 或 video_config）
func TestOmniImageOfficialCapture(t *testing.T) {
	pngData, jpegData, gifData := omniImageFixtures(t)
	gifPNG, ok := aistudio.GIFFirstFramePNG(gifData)
	if !ok {
		t.Fatal("GIF 应能转换")
	}
	encode := base64.StdEncoding.EncodeToString
	pngBody := omniImageCaptureBody(`[null,[1,"`+encode(pngData)+`"]],[["随机视频"]]`, `[null,null,null,null,null,1]`)
	jpegBody := omniImageCaptureBody(`[null,[2,"`+encode(jpegData)+`"]],[["随机视频"]]`, `[null,null,null,null,[10],1]`)
	gifBody := omniImageCaptureBody(`[null,[1,"`+encode(gifPNG)+`"]],[["随机视频"]]`, `[null,null,null,null,null,1]`)
	defaults := aistudio.GenerationDefaults{
		MaxOutputTokens: 65536, Thinking: true, ThinkingLevel: true, DefaultThinkingLevel: 3,
		ThinkingLevels: []int64{1, 3}, InteractionStream: true, VideoResolution: "360p",
	}
	anthropic := map[string]string{"Anthropic-Version": "2023-06-01"}
	gemini := func(mime string, data []byte) string {
		return `{"contents":[{"role":"user","parts":[{"inlineData":{"mimeType":"` + mime + `","data":"` + encode(data) + `"}},{"text":"随机视频"}]}]}`
	}
	chat := func(mime string, data []byte, extra string) string {
		return `{"model":"gemini-omni-1.1-flash","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:` + mime +
			`;base64,` + encode(data) + `"}},{"type":"text","text":"随机视频"}]}]` + extra + `}`
	}
	for _, test := range []struct {
		name   string
		path   string
		body   string
		header map[string]string
		want   string
	}{
		{name: "Gemini inlineData PNG", path: "/v1beta/models/gemini-omni-1.1-flash:generateContent", body: gemini("image/png", pngData), want: pngBody},
		{name: "Gemini inlineData JPEG 10 秒后缀", path: "/v1beta/models/gemini-omni-1.1-flash-10s:generateContent", body: gemini("image/jpeg", jpegData), want: jpegBody},
		{name: "Gemini inlineData GIF", path: "/v1beta/models/gemini-omni-1.1-flash:streamGenerateContent?alt=sse", body: gemini("image/gif", gifData), want: gifBody},
		{name: "Chat image_url PNG", path: "/v1/chat/completions", body: chat("image/png", pngData, ""), want: pngBody},
		{name: "Chat image_url JPEG video_config", path: "/v1/chat/completions", body: chat("image/jpeg", jpegData, `,"video_config":{"duration_seconds":10}`), want: jpegBody},
		{name: "Chat image_url image/jpg 别名", path: "/v1/chat/completions", body: chat("image/jpg", jpegData, `,"video_config":{"duration_seconds":10}`), want: jpegBody},
		{name: "Anthropic base64 JPEG", path: "/v1/messages", header: anthropic,
			body: `{"model":"gemini-omni-1.1-flash-10s","max_tokens":65536,"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64",` +
				`"media_type":"image/jpeg","data":"` + encode(jpegData) + `"}},{"type":"text","text":"随机视频"}]}]}`, want: jpegBody},
		{name: "Interactions image PNG", path: "/v1beta/interactions",
			body: `{"model":"gemini-omni-1.1-flash","input":[{"type":"image","data":"` + encode(pngData) + `","mime_type":"image/png"},{"type":"text","text":"随机视频"}]}`,
			want: pngBody},
	} {
		t.Run(test.name, func(t *testing.T) {
			handler, service := omniHandler(t)
			recorder := postJSON(t, handler, test.path, test.body, test.header)
			if recorder.Code != http.StatusOK || service.last == nil {
				t.Fatalf("状态 %d: %s", recorder.Code, recorder.Body.String())
			}
			request := *service.last
			prepared, _, err := aistudio.PrepareInteractionImages(context.Background(), request.Contents)
			if err != nil {
				t.Fatal(err)
			}
			request.Contents = prepared
			body, _, err := aistudio.EncodeCreateInteractionStreamRequest(request, defaults)
			if err != nil {
				t.Fatal(err)
			}
			if string(body) != test.want {
				t.Fatalf("请求体与官网抓包结构不一致:\n得到 %.600s\n期望 %.600s", body, test.want)
			}
		})
	}
}

// TestClientGIFKeepsSource 各协议入口（Gemini inlineData、OpenAI image_url data URI、Anthropic base64、Interactions image、工具结果里的图片）
// 把客户端内联的 GIF 转成 PNG 时在 SourceGIF 留下原 GIF（不序列化，发送的 MIME 与数据与原先相同）：发往 Omni 时转出的 PNG 超过
// 单次请求的转换额度就换回原 GIF、按需要 Drive 处理（不再绕过额度直接内联）
func TestClientGIFKeepsSource(t *testing.T) {
	_, _, gifData := omniImageFixtures(t)
	gifPNG, ok := aistudio.GIFFirstFramePNG(gifData)
	if !ok {
		t.Fatal("GIF 应能转换")
	}
	encode := base64.StdEncoding.EncodeToString
	anthropic := map[string]string{"Anthropic-Version": "2023-06-01"}
	for _, test := range []struct {
		name   string
		path   string
		body   string
		header map[string]string
	}{
		{name: "Gemini inlineData", path: "/v1beta/models/gemini-omni-1.1-flash:generateContent",
			body: `{"contents":[{"role":"user","parts":[{"inlineData":{"mimeType":"image/gif","data":"` + encode(gifData) + `"}},{"text":"随机视频"}]}]}`},
		{name: "Chat image_url", path: "/v1/chat/completions",
			body: `{"model":"gemini-omni-1.1-flash","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/gif;base64,` +
				encode(gifData) + `"}},{"type":"text","text":"随机视频"}]}]}`},
		{name: "Anthropic base64", path: "/v1/messages", header: anthropic,
			body: `{"model":"gemini-omni-1.1-flash","max_tokens":1024,"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64",` +
				`"media_type":"image/gif","data":"` + encode(gifData) + `"}},{"type":"text","text":"随机视频"}]}]}`},
		{name: "Interactions image", path: "/v1beta/interactions",
			body: `{"model":"gemini-omni-1.1-flash","input":[{"type":"image","data":"` + encode(gifData) + `","mime_type":"image/gif"},{"type":"text","text":"随机视频"}]}`},
		{name: "Chat 工具结果图片", path: "/v1/chat/completions",
			body: `{"model":"gemini-omni-1.1-flash","messages":[{"role":"user","content":"看图"},` +
				`{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"shot","arguments":"{}"}}]},` +
				`{"role":"tool","tool_call_id":"call_1","content":[{"type":"image_url","image_url":{"url":"data:image/gif;base64,` + encode(gifData) + `"}}]}],` +
				`"tools":[{"type":"function","function":{"name":"shot","parameters":{"type":"object"}}}]}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			handler, service := omniHandler(t)
			recorder := postJSON(t, handler, test.path, test.body, test.header)
			if recorder.Code != http.StatusOK || service.last == nil {
				t.Fatalf("状态 %d: %s", recorder.Code, recorder.Body.String())
			}
			var blob *aistudio.Blob
			for _, content := range service.last.Contents {
				for _, part := range content.Parts {
					if part.InlineData != nil {
						blob = part.InlineData
					}
				}
			}
			if blob == nil || blob.MIME != "image/png" || !bytes.Equal(blob.Data, gifPNG) || !bytes.Equal(blob.SourceGIF, gifData) {
				t.Fatalf("协议层应把 GIF 转成 PNG 并留下原 GIF: %+v", blob)
			}
			if encoded, _ := json.Marshal(blob); bytes.Contains(encoded, []byte(encode(gifData))) {
				t.Fatalf("原 GIF 不应序列化: %s", encoded)
			}
			prepared, report, err := aistudio.PrepareInteractionImages(context.Background(), service.last.Contents)
			if err != nil || report.Converted != 1 || report.SourceBytes != int64(len(gifData)) || report.DriveImages != 0 {
				t.Fatalf("额度内: %+v err=%v", report, err)
			}
			restore := aistudio.SetInteractionConvertLimitForTest(int64(len(gifPNG)) - 1)
			defer restore()
			prepared, report, err = aistudio.PrepareInteractionImages(context.Background(), service.last.Contents)
			if err != nil || report.Converted != 0 || report.DriveImages != 1 || !strings.Contains(report.Hint, "exceeds the 1 MB limit on PNG data converted from GIF") {
				t.Fatalf("超额: %+v err=%v", report, err)
			}
			reverted := false
			for _, content := range prepared {
				for _, part := range content.Parts {
					if part.InlineData != nil && part.InlineData.MIME == "image/gif" && bytes.Equal(part.InlineData.Data, gifData) && aistudio.InteractionDriveMedia(part) {
						reverted = true
					}
				}
			}
			if !reverted {
				t.Fatalf("超额时应换回原 GIF 并按需要 Drive 处理: %+v", prepared)
			}
		})
	}
}

// TestContinuationStateDropsSourceGIF Responses（previous_response_id）与 Interactions（previous_interaction_id）存续接状态时去掉
// 协议层转换 GIF 留下的原 GIF（所有模型都一样，状态里不多常驻一份动图）：续接历史里的图片仍是同一张 PNG（MIME 与数据不变），
// 发往 Omni 时按普通 PNG 内联为枚举 1，不再计入 GIF 转换额度、也不换回原 GIF；本轮请求里的 SourceGIF 照旧
func TestContinuationStateDropsSourceGIF(t *testing.T) {
	_, _, gifData := omniImageFixtures(t)
	gifPNG, ok := aistudio.GIFFirstFramePNG(gifData)
	if !ok {
		t.Fatal("GIF 应能转换")
	}
	encode := base64.StdEncoding.EncodeToString
	firstBlob := func(contents []aistudio.Content) *aistudio.Blob {
		for _, content := range contents {
			for _, part := range content.Parts {
				if part.InlineData != nil {
					return part.InlineData
				}
			}
		}
		return nil
	}
	for _, test := range []struct {
		name  string
		path  string
		first string
		next  string
	}{
		{name: "Responses Omni", path: "/v1/responses",
			first: `{"model":"gemini-omni-1.1-flash","input":[{"role":"user","content":[{"type":"input_image","image_url":"data:image/gif;base64,` +
				encode(gifData) + `"},{"type":"input_text","text":"随机视频"}]}]}`,
			next: `{"model":"gemini-omni-1.1-flash","previous_response_id":%q,"input":"再来一个"}`},
		{name: "Responses 其他模型", path: "/v1/responses",
			first: `{"model":"plain-chat","input":[{"role":"user","content":[{"type":"input_image","image_url":"data:image/gif;base64,` +
				encode(gifData) + `"},{"type":"input_text","text":"看图"}]}]}`,
			next: `{"model":"plain-chat","previous_response_id":%q,"input":"再看一次"}`},
		{name: "Interactions Omni", path: "/v1beta/interactions",
			first: `{"model":"gemini-omni-1.1-flash","input":[{"type":"image","data":"` + encode(gifData) + `","mime_type":"image/gif"},{"type":"text","text":"随机视频"}]}`,
			next:  `{"model":"gemini-omni-1.1-flash","previous_interaction_id":%q,"input":"再来一个"}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			handler, service := omniHandler(t)
			first := decodeInteraction(t, postJSON(t, handler, test.path, test.first, nil))
			current := firstBlob(service.last.Contents)
			if current == nil || current.MIME != "image/png" || !bytes.Equal(current.Data, gifPNG) || !bytes.Equal(current.SourceGIF, gifData) {
				t.Fatalf("本轮请求应带转出的 PNG 与原 GIF: %+v", current)
			}
			decodeInteraction(t, postJSON(t, handler, test.path, fmt.Sprintf(test.next, first["id"]), nil))
			history := firstBlob(service.last.Contents)
			if history == nil || history.MIME != "image/png" || !bytes.Equal(history.Data, gifPNG) || history.SourceGIF != nil {
				t.Fatalf("续接历史里的图片应是同一张 PNG、不带原 GIF: %+v", history)
			}
			if !bytes.Equal(current.SourceGIF, gifData) {
				t.Fatal("存续接状态不应改动本轮请求持有的 Blob")
			}
			if !strings.HasPrefix(test.name, "Responses 其他") {
				// 转换额度小于这张 PNG 时也不换回 GIF：续接历史里的图片按普通 PNG 内联
				restore := aistudio.SetInteractionConvertLimitForTest(int64(len(gifPNG)) - 1)
				defer restore()
				prepared, report, err := aistudio.PrepareInteractionImages(context.Background(), service.last.Contents)
				if err != nil || report.Converted != 0 || report.SourceBytes != 0 || report.DriveImages != 0 {
					t.Fatalf("续接历史里的 PNG 不应计入转换额度: %+v err=%v", report, err)
				}
				request := *service.last
				request.Contents = prepared
				body, _, err := aistudio.EncodeCreateInteractionStreamRequest(request, aistudio.GenerationDefaults{MaxOutputTokens: 8192, DefaultThinkingLevel: 3})
				if err != nil || !strings.Contains(string(body), `[null,[1,"`+encode(gifPNG)+`"]]`) {
					t.Fatalf("续接历史里的 PNG 应按枚举 1 内联: err=%v %.400s", err, body)
				}
			}
		})
	}
}

// TestOmniImageTableFormatsThroughProtocols 经 new-api 等中转常见的两个入口（Gemini inlineData、OpenAI image_url data URI）发来的 WebP、HEIC 与
// MIME 为 application/octet-stream 的图片，整理并编码出的请求体按官网前端的枚举表内联（WebP 3、HEIC 4，octet-stream 按文件头识别），
// 与 PNG、JPEG 的官网抓包同一结构；声明为视频的 PNG 数据不按文件头改判，仍是需要 Drive 的附件
func TestOmniImageTableFormatsThroughProtocols(t *testing.T) {
	pngData, _, _ := omniImageFixtures(t)
	webp := []byte("RIFF\x1a\x00\x00\x00WEBPVP8L\x0d\x00\x00\x00\x2f\x00\x00\x00\x10\x07\x10\x11\x11\x88\x88\xfe\x07\x00")
	heic := []byte("\x00\x00\x00\x18ftypheic\x00\x00\x00\x00mif1heic\x00\x00\x00\x10meta-data-here")
	encode := base64.StdEncoding.EncodeToString
	defaults := aistudio.GenerationDefaults{
		MaxOutputTokens: 65536, Thinking: true, ThinkingLevel: true, DefaultThinkingLevel: 3,
		ThinkingLevels: []int64{1, 3}, InteractionStream: true, VideoResolution: "360p",
	}
	gemini := func(mime string, data []byte) string {
		return `{"contents":[{"role":"user","parts":[{"inlineData":{"mimeType":"` + mime + `","data":"` + encode(data) + `"}},{"text":"随机视频"}]}]}`
	}
	chat := func(mime string, data []byte) string {
		return `{"model":"gemini-omni-1.1-flash","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:` + mime +
			`;base64,` + encode(data) + `"}},{"type":"text","text":"随机视频"}]}]}`
	}
	want := func(enum int, data []byte) string {
		return omniImageCaptureBody(fmt.Sprintf(`[null,[%d,"%s"]],[["随机视频"]]`, enum, encode(data)), `[null,null,null,null,null,1]`)
	}
	const generate = "/v1beta/models/gemini-omni-1.1-flash:generateContent"
	for _, test := range []struct {
		name, path, body, want string
	}{
		{name: "Gemini inlineData WebP", path: generate, body: gemini("image/webp", webp), want: want(3, webp)},
		{name: "Gemini inlineData octet-stream HEIC", path: generate, body: gemini("application/octet-stream", heic), want: want(4, heic)},
		{name: "Gemini inlineData octet-stream PNG", path: generate, body: gemini("application/octet-stream", pngData), want: want(1, pngData)},
		{name: "Chat image_url WebP", path: "/v1/chat/completions", body: chat("image/webp", webp), want: want(3, webp)},
		{name: "Chat image_url octet-stream PNG", path: "/v1/chat/completions", body: chat("application/octet-stream", pngData), want: want(1, pngData)},
	} {
		t.Run(test.name, func(t *testing.T) {
			handler, service := omniHandler(t)
			recorder := postJSON(t, handler, test.path, test.body, nil)
			if recorder.Code != http.StatusOK || service.last == nil {
				t.Fatalf("状态 %d: %s", recorder.Code, recorder.Body.String())
			}
			request := *service.last
			prepared, report, err := aistudio.PrepareInteractionImages(context.Background(), request.Contents)
			if err != nil || report.DriveImages != 0 || report.Inline != 1 {
				t.Fatalf("应按官网内联、不需要 Drive: %+v err=%v", report, err)
			}
			if aistudio.InteractionDriveMedia(prepared[0].Parts[0]) {
				t.Fatal("不应按需要 Drive 的附件处理")
			}
			request.Contents = prepared
			body, _, err := aistudio.EncodeCreateInteractionStreamRequest(request, defaults)
			if err != nil || string(body) != test.want {
				t.Fatalf("请求体 = %.400s\n期望 %.400s\nerr=%v", body, test.want, err)
			}
		})
	}
	// 声明为视频的 PNG 数据：不按文件头改判为图片，仍要先上传 Drive
	handler, service := omniHandler(t)
	if recorder := postJSON(t, handler, generate, gemini("video/mp4", pngData), nil); recorder.Code != http.StatusOK || service.last == nil {
		t.Fatalf("状态 %d: %s", recorder.Code, recorder.Body.String())
	}
	if part := service.last.Contents[0].Parts[0]; !aistudio.InteractionDriveMedia(part) || aistudio.InteractionInlineImage(part) {
		t.Fatalf("声明为视频的附件应仍需 Drive: %+v", part.InlineData.MIME)
	}
}

// TestOmniOctetStreamImageVideoTask 经 new-api 转发、被标成 application/octet-stream 的图片配图生视频（模型名后缀 -i2v，README 示例映射的
// gemini-omni-1.1-flash-i2v-9x16）或显式写 reference_to_video 时：视频任务的输入检查与编码用同一套文件头识别，返回 200，编码出的请求体与
// 官网抓包同一结构，图片按枚举 1 原字节内联、任务为 2（参考生视频为 3）；同样标成 octet-stream 的 MP4 不是图片，图生视频仍在选号前返回 400
func TestOmniOctetStreamImageVideoTask(t *testing.T) {
	pngData, _, _ := omniImageFixtures(t)
	encode := base64.StdEncoding.EncodeToString
	defaults := aistudio.GenerationDefaults{
		MaxOutputTokens: 65536, Thinking: true, ThinkingLevel: true, DefaultThinkingLevel: 3,
		ThinkingLevels: []int64{1, 3}, InteractionStream: true, VideoResolution: "360p",
	}
	chat := func(model, mime string, data []byte, extra string) string {
		return `{"model":"` + model + `","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:` + mime +
			`;base64,` + encode(data) + `"}},{"type":"text","text":"随机视频"}]}]` + extra + `}`
	}
	gemini := func(mime string, data []byte) string {
		return `{"contents":[{"role":"user","parts":[{"inlineData":{"mimeType":"` + mime + `","data":"` + encode(data) + `"}},{"text":"随机视频"}]}]}`
	}
	// want 为带视频任务 task 的请求体：GenerationConfig field 25 为 [task]，video 为 field 54 视频格式的 field 4
	want := func(task int, video string) string {
		body := omniImageCaptureBody(`[null,[1,"`+encode(pngData)+`"]],[["随机视频"]]`, video)
		return strings.Replace(body, `,[]]],`, fmt.Sprintf(`,[%d]]],`, task), 1)
	}
	for _, test := range []struct {
		name, path, body, want string
		task                   string
	}{
		{name: "Chat -i2v-9x16 octet-stream PNG", path: "/v1/chat/completions",
			body: chat("gemini-omni-1.1-flash-i2v-9x16", "application/octet-stream", pngData, ""),
			want: want(2, `[null,null,null,2,null,1]`), task: "image_to_video"},
		{name: "Chat -i2v binary/octet-stream PNG", path: "/v1/chat/completions",
			body: chat("gemini-omni-1.1-flash-i2v", "binary/octet-stream", pngData, ""),
			want: want(2, `[null,null,null,null,null,1]`), task: "image_to_video"},
		{name: "Chat 显式 reference_to_video octet-stream PNG", path: "/v1/chat/completions",
			body: chat("gemini-omni-1.1-flash", "application/octet-stream", pngData, `,"video_config":{"task":"reference_to_video"}`),
			want: want(3, `[null,null,null,null,null,1]`), task: "reference_to_video"},
		{name: "Gemini -i2v octet-stream PNG", path: "/v1beta/models/gemini-omni-1.1-flash-i2v:generateContent",
			body: gemini("application/octet-stream", pngData), want: want(2, `[null,null,null,null,null,1]`), task: "image_to_video"},
	} {
		t.Run(test.name, func(t *testing.T) {
			handler, service := omniHandler(t)
			recorder := postJSON(t, handler, test.path, test.body, nil)
			if recorder.Code != http.StatusOK || service.last == nil {
				t.Fatalf("状态 %d: %s", recorder.Code, recorder.Body.String())
			}
			request := *service.last
			if request.Config.VideoConfig == nil || request.Config.VideoConfig.Task != test.task {
				t.Fatalf("视频设置 = %+v，期望任务 %s", request.Config.VideoConfig, test.task)
			}
			prepared, report, err := aistudio.PrepareInteractionImages(context.Background(), request.Contents)
			if err != nil || report.DriveImages != 0 || report.Inline != 1 || aistudio.InteractionDriveMedia(prepared[0].Parts[0]) {
				t.Fatalf("应按官网内联、不需要 Drive: %+v err=%v", report, err)
			}
			request.Contents = prepared
			body, _, err := aistudio.EncodeCreateInteractionStreamRequest(request, defaults)
			if err != nil || string(body) != test.want {
				t.Fatalf("请求体 = %.500s\n期望 %.500s\nerr=%v", body, test.want, err)
			}
		})
	}
	mp4 := []byte("\x00\x00\x00\x18ftypisom\x00\x00\x00\x00isommp42-video-data")
	for _, test := range []struct{ name, path, body string }{
		{name: "Chat -i2v octet-stream MP4", path: "/v1/chat/completions", body: chat("gemini-omni-1.1-flash-i2v-9x16", "application/octet-stream", mp4, "")},
		{name: "Gemini -i2v octet-stream MP4", path: "/v1beta/models/gemini-omni-1.1-flash-i2v:generateContent", body: gemini("application/octet-stream", mp4)},
	} {
		t.Run(test.name, func(t *testing.T) {
			handler, service := omniHandler(t)
			recorder := postJSON(t, handler, test.path, test.body, nil)
			if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "video task image_to_video requires an input image") {
				t.Fatalf("状态 %d: %s，期望 400 缺少输入图片", recorder.Code, recorder.Body.String())
			}
			if service.last != nil {
				t.Fatal("参数错误应在交给生成服务前返回")
			}
		})
	}
	// 同一段 octet-stream 的 MP4 配编辑任务：按视频（需要 Drive 的文件）通过
	handler, service := omniHandler(t)
	if recorder := postJSON(t, handler, "/v1/chat/completions", chat("gemini-omni-1.1-flash-edit", "application/octet-stream", mp4, ""), nil); recorder.Code != http.StatusOK || service.last == nil {
		t.Fatalf("编辑任务：状态 %d: %s", recorder.Code, recorder.Body.String())
	}
	if part := service.last.Contents[0].Parts[0]; !aistudio.InteractionDriveMedia(part) || service.last.Config.VideoConfig.Task != "edit" {
		t.Fatalf("octet-stream 的 MP4 应按需要 Drive 的视频发送: %+v %+v", part.InlineData.MIME, service.last.Config.VideoConfig)
	}
}
