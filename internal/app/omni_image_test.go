package app

import (
	"bytes"
	"encoding/base64"
	"errors"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/api"
)

// omniImageIntro 与 aistudio 里移到用户 step 的模型附件前的说明一致
const omniImageIntro = "以下是上文模型回复里的图片或视频："

// omniTestImages 返回测试里现场生成的小 PNG（4×3）、小 JPEG（image/jpeg 编码，8×6）与 GIF（3×2）
func omniTestImages(t *testing.T) (pngData, jpegData, gifData []byte) {
	t.Helper()
	canvas := image.NewNRGBA(image.Rect(0, 0, 8, 6))
	for x := range 8 {
		for y := range 6 {
			canvas.Set(x, y, color.NRGBA{R: uint8(25 * x), G: 90, B: uint8(35 * y), A: 255})
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
	frame := image.NewPaletted(image.Rect(0, 0, 3, 2), color.Palette{color.Black, color.White, color.RGBA{R: 255, A: 255}})
	frame.SetColorIndex(2, 1, 2)
	if err := gif.Encode(&buffer, frame, nil); err != nil {
		t.Fatal(err)
	}
	return pngData, jpegData, bytes.Clone(buffer.Bytes())
}

// omniTestCaptureBody 按官网抓包的结构写出 CreateInteractionStream 请求体（第 5 项 WAA proof 为 null，见 aistudio 的同名测试）：
// parts 为唯一一条用户 step 的各个 part，video 为 field 54 视频格式的 field 4
func omniTestCaptureBody(parts string, video string) string {
	return `[1,1,null,[null,null,null,null,null,null,null,null,null,null,null,null,null,null,null,null,null,` +
		`["models/gemini-omni-1.1-flash",[null,null,null,null,null,4,1,65536,null,null,null,null,null,null,null,null,null,null,null,null,null,null,null,null,[]]],` +
		`null,null,null,null,null,null,null,null,[[[[[` + parts + `]]]]],` +
		`null,null,null,null,null,null,null,null,null,null,null,null,null,null,null,null,null,null,null,null,null,null,null,null,null,null,` +
		`[[[null,null,null,` + video + `]]]],null,1]`
}

// omniTestWebP 为文件头是 WebP 的图片数据（标准库解不了）
var omniTestWebP = []byte("RIFF\x1a\x00\x00\x00WEBPVP8L\x0d\x00\x00\x00\x2f\x00\x00\x00\x10\x07\x10\x11\x11\x88\x88\xfe\x07\x00")

// omniTestHugeGIF 为头部声明 65535×65535 画布、首帧 1×1 的最小 GIF
var omniTestHugeGIF = []byte{'G', 'I', 'F', '8', '9', 'a', 0xff, 0xff, 0xff, 0xff, 0x80, 0, 0,
	0, 0, 0, 0xff, 0xff, 0xff, ',', 0, 0, 0, 0, 1, 0, 1, 0, 0, 2, 2, 0x44, 0x01, 0, ';'}

// lastBody 返回最近一次 Generate 编码出的请求体
func (upstream *omniDriveUpstream) lastBody(t *testing.T) string {
	t.Helper()
	upstream.mu.Lock()
	defer upstream.mu.Unlock()
	if len(upstream.bodies) == 0 {
		t.Fatal("没有调用 Generate")
	}
	return string(upstream.bodies[len(upstream.bodies)-1])
}

// TestOmniImageOnlySkipsDrive 只有图片（用户的 JPEG、模型历史里从本服务链接恢复的 PNG）的 Omni 请求与纯文本请求一样选号：号池里全部账号
// 都没有 Drive 授权（记着不可用或状态未知）时也在第一个选到的账号上成功，不查询 Drive 状态、不调用上传（不换 Drive token）、不换号、
// 不冷却；图片按官网内联发送（历史图片连同说明移到用户 step 最前面）；指定账号同样如此
func TestOmniImageOnlySkipsDrive(t *testing.T) {
	pngData, jpegData, _ := omniTestImages(t)
	encode := base64.StdEncoding.EncodeToString
	accounts := []string{"a-nodrive@example.com", "b-nodrive@example.com", "c-nodrive@example.com"}
	request := func(id string) aistudio.GenerateRequest {
		return aistudio.GenerateRequest{
			ID: id, Model: omniDriveModel,
			Contents: []aistudio.Content{
				{Role: aistudio.RoleUser, Parts: []aistudio.Part{{Text: "画一只猫"}}},
				{Role: aistudio.RoleAssistant, Parts: []aistudio.Part{
					{Text: "好了"},
					{InlineData: &aistudio.Blob{MIME: "image/png", Data: pngData}, RestoredLink: true, RestoredText: restoredMediaLink("image/png")},
				}},
				{Role: aistudio.RoleUser, Parts: []aistudio.Part{{InlineData: &aistudio.Blob{MIME: "image/jpeg", Data: jpegData}}, {Text: "随机视频"}}},
			},
		}
	}
	wantStep := `[[[[["` + omniImageIntro + `"]],[null,[1,"` + encode(pngData) + `"]],[null,[2,"` + encode(jpegData) + `"]],[["随机视频"]]]]]`
	for _, test := range []struct {
		name    string
		known   bool
		account string
	}{
		{name: "全部记着不可用", known: true},
		{name: "状态未知", known: false},
		{name: "指定没有 Drive 的账号", known: true, account: "b-nodrive@example.com"},
	} {
		t.Run(test.name, func(t *testing.T) {
			upstream := newOmniDriveUpstream(accounts)
			if test.known {
				for _, accountID := range accounts {
					upstream.remember(accountID, aistudio.DriveStateUnavailable)
				}
			}
			service, pool := omniDriveService(t, upstream)
			sent := request("req-image-only")
			sent.AccountID = test.account
			text, err, _ := runOmniDriveRequest(t, service, sent)
			if err != nil || text != "ok" {
				t.Fatalf("只有图片的 Omni 请求应成功：text=%q err=%v", text, err)
			}
			calls, uploads, candidates := upstream.snapshot()
			if len(calls) != 1 || !strings.HasSuffix(calls[0], "/inline") || len(uploads) != 0 || candidates != 0 {
				t.Fatalf("Generate=%v 上传=%v 候选查询=%d，期望第一个账号直接内联发送、不上传、不查 Drive 状态", calls, uploads, candidates)
			}
			if test.account != "" && calls[0] != test.account+"/inline" {
				t.Fatalf("Generate=%v，期望在指定账号上发送", calls)
			}
			if body := upstream.lastBody(t); !strings.Contains(body, `]]]]],`+wantStep+`]],null,null`) || strings.Contains(body, `[null,null,null,null,null,null,null,null,["`) {
				t.Fatalf("图片应按官网内联发送（历史图片移到用户 step 最前面），没有 Drive 引用: %.400s", body)
			}
			if !progressLogged(service, "Omni 图片按官网内联发送，不经 Drive | 图片=2 | 字节="+strconv.Itoa(len(pngData)+len(jpegData))+"B") {
				t.Fatal("应在请求进度里记下按官网内联发送的图片")
			}
			if progressLogged(service, "Drive 授权") || progressLogged(service, "换号") {
				t.Fatal("只有图片的请求不应按 Drive 授权选号或换号")
			}
			assertNoAccountCooldown(t, pool)
		})
	}
}

// TestOmniImageGoldenThroughService 经 generateWithRetry 发送“图片（或视频）在前、文本在后”的 Omni 请求，编码出的 CreateInteractionStream
// 请求体与官网抓包的结构逐字节相同：PNG、JPEG 在没有 Drive 授权的账号上内联发送（JPEG 枚举 2、原字节，另设 10 秒时长），GIF 选号前
// 转成 PNG 后按枚举 1；视频在有 Drive 授权的账号上先上传、以 Drive 文件引用发送（官网发送视频同样如此）
func TestOmniImageGoldenThroughService(t *testing.T) {
	pngData, jpegData, gifData := omniTestImages(t)
	gifPNG, ok := aistudio.GIFFirstFramePNG(gifData)
	if !ok {
		t.Fatal("GIF 应能转换")
	}
	encode := base64.StdEncoding.EncodeToString
	const defaultVideo = `[null,null,null,null,null,1]`
	for _, test := range []struct {
		name   string
		blob   *aistudio.Blob
		prompt string
		video  *aistudio.VideoConfig
		drive  bool
		want   string
	}{
		{name: "PNG", blob: &aistudio.Blob{MIME: "image/png", Data: pngData}, prompt: "随机视频",
			want: omniTestCaptureBody(`[null,[1,"`+encode(pngData)+`"]],[["随机视频"]]`, defaultVideo)},
		{name: "JPEG 10 秒", blob: &aistudio.Blob{MIME: "image/jpeg", Data: jpegData}, prompt: "随机视频", video: &aistudio.VideoConfig{DurationSeconds: 10},
			want: omniTestCaptureBody(`[null,[2,"`+encode(jpegData)+`"]],[["随机视频"]]`, `[null,null,null,null,[10],1]`)},
		{name: "GIF 转 PNG", blob: &aistudio.Blob{MIME: "image/gif", Data: gifData}, prompt: "随机视频",
			want: omniTestCaptureBody(`[null,[1,"`+encode(gifPNG)+`"]],[["随机视频"]]`, defaultVideo)},
		{name: "视频走 Drive", blob: &aistudio.Blob{MIME: "video/mp4", Data: []byte("\x00\x00\x00\x18ftypmp42video")}, prompt: "分析视频", drive: true,
			want: omniTestCaptureBody(`[null,null,null,null,null,null,null,null,["drive-z-drive@example.com"]],[["分析视频"]]`, defaultVideo)},
	} {
		t.Run(test.name, func(t *testing.T) {
			accounts := []string{"a-nodrive@example.com"}
			upstream := newOmniDriveUpstream(accounts)
			upstream.remember("a-nodrive@example.com", aistudio.DriveStateUnavailable)
			if test.drive {
				accounts = append(accounts, "z-drive@example.com")
				upstream = newOmniDriveUpstream(accounts, "z-drive@example.com")
				upstream.remember("a-nodrive@example.com", aistudio.DriveStateUnavailable)
			}
			service, _ := omniDriveService(t, upstream)
			text, err, _ := runOmniDriveRequest(t, service, aistudio.GenerateRequest{
				ID: "req-golden", Model: omniDriveModel, Config: aistudio.GenerationConfig{VideoConfig: test.video},
				Contents: []aistudio.Content{{Role: aistudio.RoleUser, Parts: []aistudio.Part{{InlineData: test.blob}, {Text: test.prompt}}}},
			})
			if err != nil || text != "ok" {
				t.Fatalf("text=%q err=%v", text, err)
			}
			if body := upstream.lastBody(t); body != test.want {
				t.Fatalf("请求体与官网抓包结构不一致:\n得到 %.600s\n期望 %.600s", body, test.want)
			}
			upstream.mu.Lock()
			uploadMIMEs := slices.Clone(upstream.uploadMIMEs)
			upstream.mu.Unlock()
			if test.drive != (len(uploadMIMEs) == 1) || test.drive && !slices.Equal(uploadMIMEs[0], []string{"video/mp4"}) {
				t.Fatalf("上传 = %v，只有视频应上传 Drive", uploadMIMEs)
			}
			if test.blob.MIME == "image/gif" && !progressLogged(service, "GIF 转为 PNG=1 | GIF="+strconv.Itoa(len(gifData))+"B | PNG="+strconv.Itoa(len(gifPNG))+"B") {
				t.Fatal("应在请求进度里记下 GIF 转换")
			}
		})
	}
}

// TestOmniUndecodableImagesNeedDrive 不能内联的图片（WebP、头部声明超过 4096×4096 画布的 GIF）与视频一样要先上传 Drive：有 Drive 授权的
// 账号时只上传这张图（同一请求的 PNG 拿出来不上传、在原位置内联发送），号池里没有 Drive 授权的账号时不调用 Generate，返回 400
// FAILED_PRECONDITION 与英文说明（请改用 PNG 或 JPEG），请求进度写明原因
func TestOmniUndecodableImagesNeedDrive(t *testing.T) {
	pngData, _, _ := omniTestImages(t)
	encode := base64.StdEncoding.EncodeToString
	for _, test := range []struct {
		name   string
		blob   aistudio.Blob
		hint   string
		reason string
	}{
		{name: "WebP", blob: aistudio.Blob{MIME: "image/webp", Data: omniTestWebP},
			hint: "contents[0]: image/webp images cannot be sent inline to this model", reason: "image/webp 图片不是 PNG、JPEG 或 GIF"},
		{name: "超大 GIF", blob: aistudio.Blob{MIME: "image/gif", Data: omniTestHugeGIF},
			hint: "contents[0]: the GIF image is 65535x65535 pixels, larger than the 4096x4096 pixels", reason: "超过转 PNG 的上限 4096×4096"},
	} {
		request := func(id string) (aistudio.GenerateRequest, *aistudio.Blob) {
			pngBlob := &aistudio.Blob{MIME: "image/png", Data: pngData}
			blob := test.blob
			return aistudio.GenerateRequest{ID: id, Model: omniDriveModel, Contents: []aistudio.Content{{Role: aistudio.RoleUser, Parts: []aistudio.Part{
				{InlineData: &blob}, {InlineData: pngBlob}, {Text: "随机视频"},
			}}}}, pngBlob
		}
		t.Run(test.name+"/有 Drive 时上传", func(t *testing.T) {
			accounts := []string{"a-nodrive@example.com", "z-drive@example.com"}
			upstream := newOmniDriveUpstream(accounts, "z-drive@example.com")
			upstream.remember("a-nodrive@example.com", aistudio.DriveStateUnavailable)
			upstream.remember("z-drive@example.com", aistudio.DriveStateAvailable)
			service, _ := omniDriveService(t, upstream)
			sent, pngBlob := request("req-undecodable-drive")
			text, err, _ := runOmniDriveRequest(t, service, sent)
			if err != nil || text != "ok" {
				t.Fatalf("text=%q err=%v", text, err)
			}
			calls, uploads, _ := upstream.snapshot()
			upstream.mu.Lock()
			uploadMIMEs, parts := slices.Clone(upstream.uploadMIMEs), upstream.contents[0][0].Parts
			upstream.mu.Unlock()
			if !slices.Equal(calls, []string{"z-drive@example.com/inline"}) || !slices.Equal(uploads, []string{"z-drive@example.com"}) ||
				len(uploadMIMEs) != 1 || !slices.Equal(uploadMIMEs[0], []string{test.blob.MIME}) {
				t.Fatalf("Generate=%v 上传=%v（%v），期望只把这张图上传到有 Drive 授权的账号", calls, uploads, uploadMIMEs)
			}
			if parts[0].File == nil || parts[0].InlineData != nil || parts[1].InlineData != pngBlob || parts[1].File != nil {
				t.Fatalf("发送的内容 = %+v，期望这张图为 Drive 引用、PNG 在原位置内联", parts)
			}
			want := `[null,null,null,null,null,null,null,null,["drive-z-drive@example.com"]],[null,[1,"` + encode(pngData) + `"]],[["随机视频"]]`
			if body := upstream.lastBody(t); !strings.Contains(body, `[[[[[`+want+`]]]]]`) {
				t.Fatalf("请求体 = %.400s，期望含 %s", body, want)
			}
			if !progressLogged(service, "Omni 图片无法内联，改走 Drive（需要有 Drive 授权的账号） | 张数=1 | 原因=contents[0] ") ||
				!progressLogged(service, test.reason) {
				t.Fatal("应在请求进度里写明这张图改走 Drive 的原因")
			}
		})
		t.Run(test.name+"/没有 Drive 时 400", func(t *testing.T) {
			accounts := []string{"a-nodrive@example.com", "b-nodrive@example.com"}
			upstream := newOmniDriveUpstream(accounts)
			for _, accountID := range accounts {
				upstream.remember(accountID, aistudio.DriveStateUnavailable)
			}
			service, pool := omniDriveService(t, upstream)
			sent, _ := request("req-undecodable-none")
			_, err, _ := runOmniDriveRequest(t, service, sent)
			var driveRequired *aistudio.DriveRequiredError
			if !errors.As(err, &driveRequired) || driveRequired.HTTPStatus() != http.StatusBadRequest || driveRequired.RPCStatus() != "FAILED_PRECONDITION" {
				t.Fatalf("err = %#v，期望 400 FAILED_PRECONDITION", err)
			}
			message := driveRequired.PublicMessage()
			if !strings.Contains(message, "requires an account with Google Drive authorized, and no account in the pool has Google Drive authorization.") ||
				!strings.Contains(message, test.hint) || !strings.HasSuffix(message, "as PNG or JPEG instead.") {
				t.Fatalf("对外说明 = %q", message)
			}
			for _, r := range message {
				if r >= 0x4e00 && r <= 0x9fff {
					t.Fatalf("对外说明应为英文: %q", message)
				}
			}
			if calls, uploads, _ := upstream.snapshot(); len(calls)+len(uploads) != 0 {
				t.Fatalf("Generate=%v 上传=%v，不应发送", calls, uploads)
			}
			if !progressLogged(service, "号池里没有已知或可能有 Drive 授权的账号，Omni 的视频（及无法转为 PNG 的图片）无法发送，按 400 返回") {
				t.Fatal("应写明需要 Drive 的进度")
			}
			assertNoAccountCooldown(t, pool)
		})
	}
}

// TestOmniVideoWithJPEGUploadsOnlyVideo 同一请求里有 JPEG 与视频：照常按 Drive 状态选号（先试记着可用的账号），只上传视频，JPEG 在原位置
// 按枚举 2 原字节内联发送
func TestOmniVideoWithJPEGUploadsOnlyVideo(t *testing.T) {
	_, jpegData, _ := omniTestImages(t)
	accounts := []string{"a-nodrive@example.com", "z-drive@example.com"}
	upstream := newOmniDriveUpstream(accounts, "z-drive@example.com")
	upstream.remember("a-nodrive@example.com", aistudio.DriveStateUnavailable)
	upstream.remember("z-drive@example.com", aistudio.DriveStateAvailable)
	service, _ := omniDriveService(t, upstream)
	text, err, _ := runOmniDriveRequest(t, service, aistudio.GenerateRequest{
		ID: "req-video-jpeg", Model: omniDriveModel,
		Contents: []aistudio.Content{{Role: aistudio.RoleUser, Parts: []aistudio.Part{
			{InlineData: &aistudio.Blob{MIME: "image/jpeg", Data: jpegData}},
			{InlineData: &aistudio.Blob{MIME: "video/mp4", Data: []byte("video")}},
			{Text: "分析视频"},
		}}},
	})
	if err != nil || text != "ok" {
		t.Fatalf("text=%q err=%v", text, err)
	}
	_, _, candidates := upstream.snapshot()
	upstream.mu.Lock()
	uploadMIMEs := slices.Clone(upstream.uploadMIMEs)
	upstream.mu.Unlock()
	if candidates == 0 || len(uploadMIMEs) != 1 || !slices.Equal(uploadMIMEs[0], []string{"video/mp4"}) {
		t.Fatalf("候选查询 %d 次，上传 %v，期望按 Drive 状态选号、只上传视频", candidates, uploadMIMEs)
	}
	want := `[[[[[[null,[2,"` + base64.StdEncoding.EncodeToString(jpegData) + `"]],[null,null,null,null,null,null,null,null,["drive-z-drive@example.com"]],[["分析视频"]]]]]]]`
	if body := upstream.lastBody(t); !strings.Contains(body, want) {
		t.Fatalf("请求体 = %.400s，期望含 %s", body, want)
	}
}

// TestOmniImageOtherModelsUnchanged 其他模型（GenerateContent）的附件处理不变：PNG、JPEG、GIF 都照旧交给上传（没有 Drive 授权时内联发送），
// GIF 不在这里转换，不查 Drive 状态
func TestOmniImageOtherModelsUnchanged(t *testing.T) {
	pngData, jpegData, gifData := omniTestImages(t)
	upstream := newOmniDriveUpstream([]string{"a-nodrive@example.com"})
	upstream.remember("a-nodrive@example.com", aistudio.DriveStateUnavailable)
	service, _ := omniDriveService(t, upstream)
	gifBlob := &aistudio.Blob{MIME: "image/gif", Data: gifData}
	text, err, _ := runOmniDriveRequest(t, service, aistudio.GenerateRequest{
		ID: "req-other-images", Model: testRuntimeModel,
		Contents: []aistudio.Content{{Role: aistudio.RoleUser, Parts: []aistudio.Part{
			{InlineData: &aistudio.Blob{MIME: "image/png", Data: pngData}},
			{InlineData: &aistudio.Blob{MIME: "image/jpeg", Data: jpegData}},
			{InlineData: gifBlob},
			{Text: "描述图片"},
		}}},
	})
	if err != nil || text != "ok" {
		t.Fatalf("text=%q err=%v", text, err)
	}
	calls, uploads, candidates := upstream.snapshot()
	upstream.mu.Lock()
	uploadMIMEs, parts := slices.Clone(upstream.uploadMIMEs), upstream.contents[0][0].Parts
	upstream.mu.Unlock()
	if !slices.Equal(calls, []string{"a-nodrive@example.com/inline"}) || !slices.Equal(uploads, []string{"a-nodrive@example.com"}) || candidates != 0 ||
		len(uploadMIMEs) != 1 || !slices.Equal(uploadMIMEs[0], []string{"image/png", "image/jpeg", "image/gif"}) {
		t.Fatalf("Generate=%v 上传=%v（%v）候选查询=%d，期望全部附件照旧交给上传", calls, uploads, uploadMIMEs, candidates)
	}
	if parts[2].InlineData != gifBlob {
		t.Fatalf("其他模型的 GIF 不应在这里转换: %+v", parts[2].InlineData)
	}
	if progressLogged(service, "Omni 图片") {
		t.Fatal("其他模型不应整理 Omni 图片")
	}
}

// TestHoldRestoreInlineImages 上传前拿掉的 PNG、JPEG 按位置放回（视频与 WebP 留给上传）；不改动原内容；上传改了消息结构时报错，不让图片错位
func TestHoldRestoreInlineImages(t *testing.T) {
	pngData, jpegData, _ := omniTestImages(t)
	pngBlob := &aistudio.Blob{MIME: "image/png", Data: pngData}
	jpegBlob := &aistudio.Blob{MIME: "image/jpeg", Data: jpegData}
	video := &aistudio.Blob{MIME: "video/mp4", Data: []byte("video")}
	webp := &aistudio.Blob{MIME: "image/webp", Data: omniTestWebP}
	contents := []aistudio.Content{
		{Role: aistudio.RoleUser, Parts: []aistudio.Part{{Text: "a"}}},
		{Role: aistudio.RoleAssistant, Parts: []aistudio.Part{{InlineData: pngBlob, RestoredLink: true}, {Text: "b"}}},
		{Role: aistudio.RoleUser, Parts: []aistudio.Part{{InlineData: jpegBlob}, {InlineData: video}, {InlineData: webp}, {Text: "c"}}},
	}
	held, images := holdInlineImages(contents)
	if len(images) != 2 || held[1].Parts[0].InlineData != nil || !held[1].Parts[0].RestoredLink || held[2].Parts[0].InlineData != nil ||
		held[2].Parts[1].InlineData != video || held[2].Parts[2].InlineData != webp {
		t.Fatalf("拿掉的图片 = %+v，内容 = %+v", images, held)
	}
	if contents[1].Parts[0].InlineData != pngBlob || contents[2].Parts[0].InlineData != jpegBlob {
		t.Fatal("不应改动原内容")
	}
	if &held[0] == &contents[0] {
		t.Fatal("应复制内容")
	}
	uploaded := slices.Clone(held)
	uploaded[2].Parts = slices.Clone(held[2].Parts)
	uploaded[2].Parts[1] = aistudio.Part{File: &aistudio.FileRef{ID: "drive-video"}}
	restored, err := restoreInlineImages(uploaded, images)
	if err != nil || restored[1].Parts[0].InlineData != pngBlob || restored[2].Parts[0].InlineData != jpegBlob ||
		restored[2].Parts[1].File == nil || restored[2].Parts[2].InlineData != webp {
		t.Fatalf("放回后 = %+v err=%v", restored, err)
	}
	if uploaded[2].Parts[0].InlineData != nil {
		t.Fatal("不应改动上传返回的内容")
	}
	if _, err := restoreInlineImages(uploaded[:2], images); err == nil {
		t.Fatal("消息数变了应报错")
	}
	if _, err := restoreInlineImages(restored, images); err == nil {
		t.Fatal("位置已被占用应报错")
	}
	if same, none := holdInlineImages(contents[:1]); &same[0] != &contents[0] || none != nil {
		t.Fatal("没有图片时应原样返回")
	}
}

// TestOmniClientGIFThroughProtocol 客户端经协议入口（Gemini inlineData、OpenAI image_url data URI）发来的 GIF 在协议层已转成 PNG：
// 发往 Omni 时照样计入单次请求的 GIF 转换额度并在请求进度里记下 GIF 转换；转出的 PNG 超过额度时换回原 GIF，有 Drive 授权的账号时
// 只把原 GIF（原字节）上传 Drive、以文件引用发送，号池里没有 Drive 授权的账号时不发送，返回 400 与英文说明（请改用 PNG 或 JPEG）
func TestOmniClientGIFThroughProtocol(t *testing.T) {
	_, _, gifData := omniTestImages(t)
	gifPNG, ok := aistudio.GIFFirstFramePNG(gifData)
	if !ok {
		t.Fatal("GIF 应能转换")
	}
	encode := base64.StdEncoding.EncodeToString
	entries := []struct {
		name string
		path string
		body string
	}{
		{name: "Gemini inlineData", path: "/v1beta/models/" + omniDriveModel + ":generateContent",
			body: `{"contents":[{"role":"user","parts":[{"inlineData":{"mimeType":"image/gif","data":"` + encode(gifData) + `"}},{"text":"随机视频"}]}]}`},
		{name: "Chat image_url", path: "/v1/chat/completions",
			body: `{"model":"` + omniDriveModel + `","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/gif;base64,` +
				encode(gifData) + `"}},{"type":"text","text":"随机视频"}]}]}`},
	}
	post := func(t *testing.T, service *trackedService, path string, body string) *httptest.ResponseRecorder {
		t.Helper()
		handler := api.NewHandler(service, api.Config{APIKey: "sk-test"})
		request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer sk-test")
		request.Header.Set("Content-Type", "application/json")
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		return recorder
	}
	conversion := "GIF 转为 PNG=1 | GIF=" + strconv.Itoa(len(gifData)) + "B | PNG=" + strconv.Itoa(len(gifPNG)) + "B"
	for _, entry := range entries {
		t.Run(entry.name+"/额度内内联", func(t *testing.T) {
			upstream := newOmniDriveUpstream([]string{"a-nodrive@example.com"})
			upstream.remember("a-nodrive@example.com", aistudio.DriveStateUnavailable)
			service, _ := omniDriveService(t, upstream)
			if recorder := post(t, service, entry.path, entry.body); recorder.Code != http.StatusOK {
				t.Fatalf("状态 %d: %s", recorder.Code, recorder.Body.String())
			}
			want := `[[[[[[null,[1,"` + encode(gifPNG) + `"]],[["随机视频"]]]]]]`
			if body := upstream.lastBody(t); !strings.Contains(body, want) {
				t.Fatalf("请求体 = %.400s，期望 GIF 转成的 PNG 按枚举 1 内联", body)
			}
			if _, uploads, _ := upstream.snapshot(); len(uploads) != 0 {
				t.Fatalf("上传 = %v，额度内的 GIF 不应上传 Drive", uploads)
			}
			if !progressLogged(service, conversion) {
				t.Fatal("应在请求进度里记下协议层转换的 GIF")
			}
		})
		t.Run(entry.name+"/超额上传原 GIF", func(t *testing.T) {
			t.Cleanup(aistudio.SetInteractionConvertLimitForTest(int64(len(gifPNG)) - 1))
			accounts := []string{"a-nodrive@example.com", "z-drive@example.com"}
			upstream := newOmniDriveUpstream(accounts, "z-drive@example.com")
			upstream.remember("a-nodrive@example.com", aistudio.DriveStateUnavailable)
			upstream.remember("z-drive@example.com", aistudio.DriveStateAvailable)
			service, _ := omniDriveService(t, upstream)
			if recorder := post(t, service, entry.path, entry.body); recorder.Code != http.StatusOK {
				t.Fatalf("状态 %d: %s", recorder.Code, recorder.Body.String())
			}
			calls, uploads, _ := upstream.snapshot()
			upstream.mu.Lock()
			uploadMIMEs, uploadData := slices.Clone(upstream.uploadMIMEs), slices.Clone(upstream.uploadData)
			upstream.mu.Unlock()
			if !slices.Equal(calls, []string{"z-drive@example.com"}) || !slices.Equal(uploads, []string{"z-drive@example.com"}) ||
				len(uploadMIMEs) != 1 || !slices.Equal(uploadMIMEs[0], []string{"image/gif"}) || !bytes.Equal(uploadData[0][0], gifData) {
				t.Fatalf("Generate=%v 上传=%v（%v），期望把原 GIF 上传到有 Drive 授权的账号", calls, uploads, uploadMIMEs)
			}
			want := `[[[[[[null,null,null,null,null,null,null,null,["drive-z-drive@example.com"]],[["随机视频"]]]]]]`
			if body := upstream.lastBody(t); !strings.Contains(body, want) {
				t.Fatalf("请求体 = %.400s，期望原 GIF 以 Drive 引用发送", body)
			}
			if !progressLoggedAll(service, "Omni 图片无法内联，改走 Drive", "超过单次请求的转换上限") || progressLogged(service, "GIF 转为 PNG=") {
				t.Fatal("应在请求进度里写明这张 GIF 超出转换额度、改走 Drive")
			}
		})
		t.Run(entry.name+"/超额没有 Drive 时 400", func(t *testing.T) {
			t.Cleanup(aistudio.SetInteractionConvertLimitForTest(int64(len(gifPNG)) - 1))
			accounts := []string{"a-nodrive@example.com", "b-nodrive@example.com"}
			upstream := newOmniDriveUpstream(accounts)
			for _, accountID := range accounts {
				upstream.remember(accountID, aistudio.DriveStateUnavailable)
			}
			service, pool := omniDriveService(t, upstream)
			recorder := post(t, service, entry.path, entry.body)
			if recorder.Code != http.StatusBadRequest ||
				!strings.Contains(recorder.Body.String(), "contents[0]: the GIF image exceeds the 1 MB limit on PNG data converted from GIF images in one request") ||
				!strings.Contains(recorder.Body.String(), "as PNG or JPEG instead.") {
				t.Fatalf("状态 %d: %s，期望 400 与英文说明", recorder.Code, recorder.Body.String())
			}
			if calls, uploads, _ := upstream.snapshot(); len(calls)+len(uploads) != 0 {
				t.Fatalf("Generate=%v 上传=%v，不应发送", calls, uploads)
			}
			assertNoAccountCooldown(t, pool)
		})
	}
}
