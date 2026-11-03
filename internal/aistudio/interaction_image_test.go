package aistudio

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"strings"
	"testing"
)

// omniTestPNG 返回测试里现场生成的小 PNG（4×3 的 RGB 渐变）
func omniTestPNG(t *testing.T) []byte {
	t.Helper()
	canvas := image.NewNRGBA(image.Rect(0, 0, 4, 3))
	for x := range 4 {
		for y := range 3 {
			canvas.Set(x, y, color.NRGBA{R: uint8(60 * x), G: uint8(80 * y), B: 200, A: 255})
		}
	}
	var buffer bytes.Buffer
	if err := png.Encode(&buffer, canvas); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

// omniTestJPEG 返回测试里现场用 image/jpeg 编码的小 JPEG（8×6）
func omniTestJPEG(t *testing.T) []byte {
	t.Helper()
	canvas := image.NewRGBA(image.Rect(0, 0, 8, 6))
	for x := range 8 {
		for y := range 6 {
			canvas.Set(x, y, color.RGBA{R: uint8(30 * x), G: 120, B: uint8(40 * y), A: 255})
		}
	}
	var buffer bytes.Buffer
	if err := jpeg.Encode(&buffer, canvas, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

// omniTestGIF 返回测试里现场生成的两帧 GIF（3×2，四色调色板）
func omniTestGIF(t *testing.T) []byte {
	t.Helper()
	palette := color.Palette{color.Black, color.White, color.RGBA{R: 255, A: 255}, color.RGBA{B: 255, A: 255}}
	first := image.NewPaletted(image.Rect(0, 0, 3, 2), palette)
	second := image.NewPaletted(image.Rect(0, 0, 3, 2), palette)
	for x := range 3 {
		first.SetColorIndex(x, 0, uint8(x+1))
		second.SetColorIndex(x, 1, 2)
	}
	var buffer bytes.Buffer
	if err := gif.EncodeAll(&buffer, &gif.GIF{Image: []*image.Paletted{first, second}, Delay: []int{10, 10}}); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

// omniTestWebP 为文件头是 WebP 的图片数据（标准库解不了）
var omniTestWebP = []byte("RIFF\x1a\x00\x00\x00WEBPVP8L\x0d\x00\x00\x00\x2f\x00\x00\x00\x10\x07\x10\x11\x11\x88\x88\xfe\x07\x00")

// omniTestFtyp 返回以 ftyp 盒开头的 ISO 基本媒体文件头：主品牌 major、兼容品牌 compatible，后面跟一段假数据
func omniTestFtyp(major string, compatible ...string) []byte {
	box := []byte{0, 0, 0, byte(16 + 4*len(compatible))}
	box = append(box, "ftyp"+major+"\x00\x00\x00\x00"...)
	for _, brand := range compatible {
		box = append(box, brand...)
	}
	return append(box, "\x00\x00\x00\x10meta-data-here"...)
}

var (
	// omniTestHEIC 为 iPhone 照片那样主品牌 heic 的 HEIF 文件头，omniTestHEIF 为通用品牌 mif1 的 HEIF 文件头
	omniTestHEIC = omniTestFtyp("heic", "mif1", "heic")
	omniTestHEIF = omniTestFtyp("mif1", "mif1", "miaf")
	// omniTestAVIF 为 AVIF 文件头（同样是 HEIF 容器，官网枚举表里没有），omniTestMIF1AVIF 为通用品牌加 AVIF 兼容品牌的 AVIF
	omniTestAVIF     = omniTestFtyp("avif", "mif1", "miaf", "avif")
	omniTestMIF1AVIF = omniTestFtyp("mif1", "avif", "miaf")
	// omniTestMP4 为 MP4 视频文件头（ftyp 品牌 isom），不是图片
	omniTestMP4 = omniTestFtyp("isom", "isom", "mp42")
	// omniTestTIFF 为小端与大端 TIFF 文件头
	omniTestTIFF   = []byte("II*\x00\x08\x00\x00\x00tiff-data")
	omniTestTIFFBE = []byte("MM\x00*\x00\x00\x00\x08tiff-data")
)

// omniTestBMP 返回 1×1 的 24 位 BMP（BITMAPFILEHEADER 加 40 字节的 BITMAPINFOHEADER 与一个像素）
func omniTestBMP() []byte {
	data := []byte{'B', 'M', 58, 0, 0, 0, 0, 0, 0, 0, 54, 0, 0, 0, 40, 0, 0, 0, 1, 0, 0, 0, 1, 0, 0, 0, 1, 0, 24, 0}
	data = append(data, make([]byte, 24)...)
	return append(data, 0xff, 0x00, 0x00, 0x00)
}

// omniCaptureBody 按官网抓包的结构写出 CreateInteractionStream 请求体（第 5 项 WAA proof 为 null）：interaction 共 54 项，field 18 为
// 模型与 GenerationConfig（思考 High=4、summaries 1、max output 65536、field 25 任务 []），field 27 为只有一条用户 step 的输入，
// parts 为这条 step 的各个 part；field 54 为视频格式，video 为其中的 field 4
func omniCaptureBody(parts string, video string) string {
	return `[1,1,null,[null,null,null,null,null,null,null,null,null,null,null,null,null,null,null,null,null,` +
		`["models/gemini-omni-1.1-flash",[null,null,null,null,null,4,1,65536,null,null,null,null,null,null,null,null,null,null,null,null,null,null,null,null,[]]],` +
		`null,null,null,null,null,null,null,null,[[[[[` + parts + `]]]]],` +
		`null,null,null,null,null,null,null,null,null,null,null,null,null,null,null,null,null,null,null,null,null,null,null,null,null,null,` +
		`[[[null,null,null,` + video + `]]]],null,1]`
}

// omniDefaultVideo 为官网默认的视频格式（不带宽高比与时长、360p）
const omniDefaultVideo = `[null,null,null,null,null,1]`

// TestInteractionImageOfficialCapture 按官网抓包的结构逐字节核对：用户附一张图片、输入“随机视频”（图片在前、文本在后）时，PNG 内联为
// Content field 2 [1, Base64]、JPEG 为 [2, Base64]（原文件字节，image/jpg、image/pjpeg 等别名与大小写不同的 MIME 相同），没有 Drive 引用；
// JPEG 那份另设了 10 秒时长。视频为 Drive 文件引用（Content field 9），文本“分析视频”。图片数据与文件 ID 不参与 binding；
// PrepareInteractionImages 不改动 PNG、JPEG
func TestInteractionImageOfficialCapture(t *testing.T) {
	pngData, jpegData := omniTestPNG(t), omniTestJPEG(t)
	encode := base64.StdEncoding.EncodeToString
	text := Part{Text: "随机视频"}
	for _, test := range []struct {
		name  string
		parts []Part
		video *VideoConfig
		want  string
	}{
		{name: "PNG", parts: []Part{{InlineData: &Blob{MIME: "image/png", Data: pngData}}, text},
			want: omniCaptureBody(`[null,[1,"`+encode(pngData)+`"]],[["随机视频"]]`, omniDefaultVideo)},
		{name: "JPEG 10 秒", parts: []Part{{InlineData: &Blob{MIME: "image/jpeg", Data: jpegData}}, text}, video: &VideoConfig{DurationSeconds: 10},
			want: omniCaptureBody(`[null,[2,"`+encode(jpegData)+`"]],[["随机视频"]]`, `[null,null,null,null,[10],1]`)},
		{name: "image/jpg 别名", parts: []Part{{InlineData: &Blob{MIME: "image/jpg", Data: jpegData}}, text},
			want: omniCaptureBody(`[null,[2,"`+encode(jpegData)+`"]],[["随机视频"]]`, omniDefaultVideo)},
		{name: "IMAGE/PJPEG 大写别名", parts: []Part{{InlineData: &Blob{MIME: "IMAGE/PJPEG", Data: jpegData}}, text},
			want: omniCaptureBody(`[null,[2,"`+encode(jpegData)+`"]],[["随机视频"]]`, omniDefaultVideo)},
		{name: "视频 Drive 引用", parts: []Part{{File: &FileRef{ID: "drive-video-0001", MIME: "video/mp4"}}, {Text: "分析视频"}},
			want: omniCaptureBody(`[null,null,null,null,null,null,null,null,["drive-video-0001"]],[["分析视频"]]`, omniDefaultVideo)},
		// 官网前端枚举表里的其他图片格式与 PNG、JPEG 同一结构，原字节内联（没有官网抓包，按同一张表编码）
		{name: "WebP 枚举 3", parts: []Part{{InlineData: &Blob{MIME: "image/webp", Data: omniTestWebP}}, text},
			want: omniCaptureBody(`[null,[3,"`+encode(omniTestWebP)+`"]],[["随机视频"]]`, omniDefaultVideo)},
		{name: "HEIC 枚举 4", parts: []Part{{InlineData: &Blob{MIME: "image/heic", Data: omniTestHEIC}}, text},
			want: omniCaptureBody(`[null,[4,"`+encode(omniTestHEIC)+`"]],[["随机视频"]]`, omniDefaultVideo)},
		{name: "HEIF 枚举 5", parts: []Part{{InlineData: &Blob{MIME: "image/heif", Data: omniTestHEIF}}, text},
			want: omniCaptureBody(`[null,[5,"`+encode(omniTestHEIF)+`"]],[["随机视频"]]`, omniDefaultVideo)},
		{name: "BMP 枚举 8", parts: []Part{{InlineData: &Blob{MIME: "image/bmp", Data: omniTestBMP()}}, text},
			want: omniCaptureBody(`[null,[8,"`+encode(omniTestBMP())+`"]],[["随机视频"]]`, omniDefaultVideo)},
		{name: "TIFF 枚举 9", parts: []Part{{InlineData: &Blob{MIME: "image/tiff", Data: omniTestTIFF}}, text},
			want: omniCaptureBody(`[null,[9,"`+encode(omniTestTIFF)+`"]],[["随机视频"]]`, omniDefaultVideo)},
		{name: "octet-stream 的 PNG", parts: []Part{{InlineData: &Blob{MIME: "application/octet-stream", Data: pngData}}, text},
			want: omniCaptureBody(`[null,[1,"`+encode(pngData)+`"]],[["随机视频"]]`, omniDefaultVideo)},
		{name: "空 MIME 的 JPEG", parts: []Part{{InlineData: &Blob{Data: jpegData}}, text},
			want: omniCaptureBody(`[null,[2,"`+encode(jpegData)+`"]],[["随机视频"]]`, omniDefaultVideo)},
		{name: "octet-stream 的 HEIC", parts: []Part{{InlineData: &Blob{MIME: "application/octet-stream", Data: omniTestHEIC}}, text},
			want: omniCaptureBody(`[null,[4,"`+encode(omniTestHEIC)+`"]],[["随机视频"]]`, omniDefaultVideo)},
	} {
		t.Run(test.name, func(t *testing.T) {
			contents := []Content{{Role: RoleUser, Parts: test.parts}}
			prepared, report, err := PrepareInteractionImages(context.Background(), contents)
			if err != nil || report.Converted != 0 || report.DriveImages != 0 {
				t.Fatalf("PNG、JPEG 不应转换或改走 Drive: %+v err=%v", report, err)
			}
			if &prepared[0] != &contents[0] {
				t.Fatal("没有要转换的图片时应原样返回内容")
			}
			request := GenerateRequest{Model: "gemini-omni-1.1-flash", Contents: prepared, Config: GenerationConfig{VideoConfig: test.video}}
			body, binding, err := EncodeCreateInteractionStreamRequest(request, omniTestDefaults())
			if err != nil {
				t.Fatal(err)
			}
			if string(body) != test.want {
				t.Fatalf("请求体与官网抓包结构不一致:\n得到 %s\n期望 %s", body, test.want)
			}
			encodedBinding, _ := json.Marshal(binding)
			if want := `[{"role":"user","parts":[{"text":"` + test.parts[1].Text + `"}]}]`; string(encodedBinding) != want {
				t.Fatalf("binding = %s，期望 %s", encodedBinding, want)
			}
		})
	}
}

// TestInteractionImageFormatByMagic 图片格式以文件头为准，按官网前端的枚举表取枚举：PNG 1、JPEG 2、WebP 3、HEIC 4、HEIF 5、BMP 8、TIFF 9
// 原字节内联；MIME 不分大小写、带参数与 JPEG 别名都按文件头判断，标错 MIME 的图片按实际格式发送；HEIC 与 HEIF 按 ftyp 品牌区分，声明了
// image/heic 或 image/heif 时按声明；MIME 为空或 octet-stream 一类的附件文件头是这些图片格式时按图片内联。官网枚举表里没有的格式（AVIF）、
// 未转换的 GIF、数据与声明不符的图片，以及视频、MIME 明确不是图片的文件（即使数据是 PNG）都要上传 Drive
func TestInteractionImageFormatByMagic(t *testing.T) {
	pngData, jpegData, gifData := omniTestPNG(t), omniTestJPEG(t), omniTestGIF(t)
	bmpData := omniTestBMP()
	for _, test := range []struct {
		name  string
		blob  Blob
		enum  int64
		drive bool
	}{
		{name: "PNG", blob: Blob{MIME: "image/png", Data: pngData}, enum: 1},
		{name: "JPEG", blob: Blob{MIME: "image/jpeg", Data: jpegData}, enum: 2},
		{name: "JPEG 带参数", blob: Blob{MIME: "Image/JPEG; charset=binary", Data: jpegData}, enum: 2},
		{name: "image/pjpeg", blob: Blob{MIME: "image/pjpeg", Data: jpegData}, enum: 2},
		{name: "标成 PNG 的 JPEG", blob: Blob{MIME: "image/png", Data: jpegData}, enum: 2},
		{name: "标成 JPEG 的 PNG", blob: Blob{MIME: "image/jpeg", Data: pngData}, enum: 1},
		{name: "WebP", blob: Blob{MIME: "image/webp", Data: omniTestWebP}, enum: 3},
		{name: "HEIC", blob: Blob{MIME: "image/heic", Data: omniTestHEIC}, enum: 4},
		{name: "HEIF", blob: Blob{MIME: "image/heif", Data: omniTestHEIF}, enum: 5},
		{name: "声明 HEIF 的 heic 品牌按声明", blob: Blob{MIME: "image/heif", Data: omniTestHEIC}, enum: 5},
		{name: "声明 HEIC 的 mif1 品牌按声明", blob: Blob{MIME: "image/heic", Data: omniTestHEIF}, enum: 4},
		{name: "标成 JPEG 的 HEIC 按品牌", blob: Blob{MIME: "image/jpeg", Data: omniTestHEIC}, enum: 4},
		{name: "BMP", blob: Blob{MIME: "image/bmp", Data: bmpData}, enum: 8},
		{name: "image/x-ms-bmp 别名", blob: Blob{MIME: "image/x-ms-bmp", Data: bmpData}, enum: 8},
		{name: "TIFF 小端", blob: Blob{MIME: "image/tiff", Data: omniTestTIFF}, enum: 9},
		{name: "TIFF 大端", blob: Blob{MIME: "image/tiff", Data: omniTestTIFFBE}, enum: 9},
		{name: "octet-stream 的 PNG", blob: Blob{MIME: "application/octet-stream", Data: pngData}, enum: 1},
		{name: "空 MIME 的 JPEG", blob: Blob{MIME: "", Data: jpegData}, enum: 2},
		{name: "binary/octet-stream 的 WebP", blob: Blob{MIME: "binary/octet-stream", Data: omniTestWebP}, enum: 3},
		{name: "octet-stream 的 HEIC", blob: Blob{MIME: "Application/Octet-Stream", Data: omniTestHEIC}, enum: 4},
		{name: "octet-stream 的 BMP", blob: Blob{MIME: "application/octet-stream", Data: bmpData}, enum: 8},
		{name: "octet-stream 的 TIFF", blob: Blob{MIME: "application/octet-stream", Data: omniTestTIFF}, enum: 9},
		{name: "AVIF", blob: Blob{MIME: "image/avif", Data: omniTestAVIF}, drive: true},
		{name: "mif1 加 AVIF 兼容品牌", blob: Blob{MIME: "image/avif", Data: omniTestMIF1AVIF}, drive: true},
		{name: "octet-stream 的 AVIF", blob: Blob{MIME: "application/octet-stream", Data: omniTestAVIF}, drive: true},
		{name: "未转换的 GIF", blob: Blob{MIME: "image/gif", Data: gifData}, drive: true},
		{name: "octet-stream 的 GIF", blob: Blob{MIME: "application/octet-stream", Data: gifData}, drive: true},
		{name: "声明 PNG 的坏数据", blob: Blob{MIME: "image/png", Data: []byte("\x89PNG")}, drive: true},
		{name: "只有 BM 两字节", blob: Blob{MIME: "image/bmp", Data: []byte("BM-not-a-bitmap-header")}, drive: true},
		{name: "视频", blob: Blob{MIME: "video/mp4", Data: omniTestMP4}, drive: true},
		{name: "octet-stream 的 MP4", blob: Blob{MIME: "application/octet-stream", Data: omniTestMP4}, drive: true},
		{name: "声明为视频的 PNG 数据", blob: Blob{MIME: "video/mp4", Data: pngData}, drive: true},
		{name: "声明为 PDF 的 JPEG 数据", blob: Blob{MIME: "application/pdf", Data: jpegData}, drive: true},
		{name: "octet-stream 的未知数据", blob: Blob{MIME: "application/octet-stream", Data: []byte("plain bytes")}, drive: true},
	} {
		part := Part{InlineData: &test.blob}
		enum, ok := interactionImageEnum(&test.blob)
		if enum != test.enum || ok != (test.enum != 0) || InteractionInlineImage(part) != (test.enum != 0) || InteractionDriveMedia(part) != test.drive {
			t.Fatalf("%s: 枚举=%d/%v 内联=%v Drive=%v", test.name, enum, ok, InteractionInlineImage(part), InteractionDriveMedia(part))
		}
	}
	if InteractionInlineImage(Part{Text: "x"}) || InteractionDriveMedia(Part{File: &FileRef{ID: "f"}}) {
		t.Fatal("没有 inline data 的 part 既不是内联图片也不需要上传")
	}
}

// TestPrepareInteractionImages GIF（用户附件与模型历史里的）取首帧转成 PNG，与各协议整理 GIF 的结果相同；PNG、JPEG、WebP、octet-stream 的
// HEIC 原样保留（同一份数据，按官网内联发送，各格式计数）；AVIF 与视频不动，AVIF 计为要上传 Drive 的图片并给出英文说明；原内容不改
func TestPrepareInteractionImages(t *testing.T) {
	pngBlob := &Blob{MIME: "image/png", Data: omniTestPNG(t)}
	jpegBlob := &Blob{MIME: "image/jpeg", Data: omniTestJPEG(t)}
	gifData := omniTestGIF(t)
	gifBlob := &Blob{MIME: "image/gif", Data: gifData}
	webpBlob := &Blob{MIME: "image/webp", Data: omniTestWebP}
	avifBlob := &Blob{MIME: "image/avif", Data: omniTestAVIF}
	heicBlob := &Blob{MIME: "application/octet-stream", Data: omniTestHEIC}
	videoBlob := &Blob{MIME: "video/mp4", Data: []byte("video")}
	contents := []Content{
		{Role: RoleUser, Parts: []Part{{Text: "画一只猫"}}},
		{Role: RoleAssistant, Parts: []Part{{Text: "好了"}, {InlineData: gifBlob, RestoredLink: true, RestoredText: "![image](/media/a.gif)"}}},
		{Role: RoleUser, Parts: []Part{
			{InlineData: pngBlob}, {InlineData: jpegBlob}, {InlineData: gifBlob}, {InlineData: webpBlob}, {InlineData: videoBlob},
			{InlineData: avifBlob}, {InlineData: heicBlob}, {Text: "动起来"},
		}},
	}
	prepared, report, err := PrepareInteractionImages(context.Background(), contents)
	if err != nil {
		t.Fatal(err)
	}
	wantPNG, ok := GIFFirstFramePNG(gifData)
	if !ok {
		t.Fatal("GIF 应能转换")
	}
	converted := prepared[1].Parts[1]
	if converted.InlineData.MIME != "image/png" || !bytes.Equal(converted.InlineData.Data, wantPNG) || !converted.RestoredLink ||
		converted.RestoredText != "![image](/media/a.gif)" {
		t.Fatalf("模型历史里的 GIF 应转成 PNG、保留恢复标记: %+v", converted)
	}
	if config, err := png.DecodeConfig(bytes.NewReader(converted.InlineData.Data)); err != nil || config.Width != 3 || config.Height != 2 {
		t.Fatalf("转出的 PNG = %+v err=%v，期望 3×2", config, err)
	}
	user := prepared[2].Parts
	if user[0].InlineData != pngBlob || user[1].InlineData != jpegBlob || user[3].InlineData != webpBlob || user[4].InlineData != videoBlob ||
		user[5].InlineData != avifBlob || user[6].InlineData != heicBlob {
		t.Fatalf("PNG、JPEG、WebP、HEIC、AVIF 与视频应原样保留: %+v", user)
	}
	if user[2].InlineData.MIME != "image/png" || !bytes.Equal(user[2].InlineData.Data, wantPNG) {
		t.Fatalf("用户的 GIF 应转成 PNG: %+v", user[2].InlineData)
	}
	if contents[1].Parts[1].InlineData != gifBlob || contents[2].Parts[2].InlineData != gifBlob {
		t.Fatal("不应改动原内容")
	}
	wantInline := int64(len(pngBlob.Data) + len(jpegBlob.Data) + 2*len(wantPNG) + len(omniTestWebP) + len(omniTestHEIC))
	if report.Inline != 6 || report.InlineBytes != wantInline || report.Formats != "HEIC 1、JPEG 1、PNG 3、WebP 1" || report.Converted != 2 ||
		report.SourceBytes != int64(2*len(gifData)) || report.PNGBytes != int64(2*len(wantPNG)) || report.DriveImages != 1 {
		t.Fatalf("整理结果 = %+v", report)
	}
	if !strings.Contains(report.Hint, "contents[2]: image/avif images cannot be sent inline to this model "+
		"(only PNG, JPEG, WebP, HEIC, HEIF, BMP and TIFF images are, and GIF is converted to PNG)") ||
		!strings.Contains(report.Hint, "send the image as PNG, JPEG or WebP instead") || !strings.Contains(report.Reason, "image/avif 图片不是能内联的格式") {
		t.Fatalf("说明 = %q / %q", report.Hint, report.Reason)
	}
	for _, r := range report.Hint {
		if r >= 0x4e00 && r <= 0x9fff {
			t.Fatalf("对外说明应为英文: %q", report.Hint)
		}
	}
	if InteractionDriveMedia(user[2]) || InteractionDriveMedia(user[3]) || InteractionDriveMedia(user[6]) ||
		!InteractionDriveMedia(user[4]) || !InteractionDriveMedia(user[5]) {
		t.Fatal("转成 PNG 的 GIF、WebP 与 HEIC 不需要 Drive，视频与 AVIF 需要")
	}

	// 没有要转换的图片时原样返回（不复制）
	plain := []Content{{Role: RoleUser, Parts: []Part{{InlineData: pngBlob}, {Text: "x"}}}}
	if got, report, err := PrepareInteractionImages(context.Background(), plain); err != nil || &got[0] != &plain[0] || report.Inline != 1 {
		t.Fatalf("got=%p report=%+v err=%v", got, report, err)
	}
}

// TestPrepareInteractionImagesLimits 超大图片：头部声明超过 4096×4096 画布的 GIF 不解码，一次请求转出的 PNG 合计超过上限时后面的 GIF
// 不转换，都按要上传 Drive 处理并给出英文说明；坏 GIF 同样如此；声明为 PNG 而数据不是的图片说明数据无效；等转换名额时请求结束返回 ctx 错误
func TestPrepareInteractionImagesLimits(t *testing.T) {
	huge := &Blob{MIME: "image/gif", Data: []byte{'G', 'I', 'F', '8', '9', 'a', 0xff, 0xff, 0xff, 0xff, 0x80, 0, 0,
		0, 0, 0, 0xff, 0xff, 0xff, ',', 0, 0, 0, 0, 1, 0, 1, 0, 0, 2, 2, 0x44, 0x01, 0, ';'}}
	_, report, err := PrepareInteractionImages(context.Background(), []Content{{Role: RoleUser, Parts: []Part{{InlineData: huge}}}})
	if err != nil || report.DriveImages != 1 || report.Converted != 0 ||
		!strings.Contains(report.Hint, "contents[0]: the GIF image is 65535x65535 pixels, larger than the 4096x4096 pixels") {
		t.Fatalf("超大画布: %+v err=%v", report, err)
	}

	gifData := omniTestGIF(t)
	single, _ := GIFFirstFramePNG(gifData)
	restore := SetInteractionConvertLimitForTest(int64(len(single)) + 8)
	t.Cleanup(restore)
	contents := []Content{{Role: RoleUser, Parts: []Part{{InlineData: &Blob{MIME: "image/gif", Data: gifData}}, {InlineData: &Blob{MIME: "image/gif", Data: gifData}}}}}
	prepared, report, err := PrepareInteractionImages(context.Background(), contents)
	if err != nil || report.Converted != 1 || report.DriveImages != 1 || prepared[0].Parts[0].InlineData.MIME != "image/png" ||
		prepared[0].Parts[1].InlineData.MIME != "image/gif" ||
		!strings.Contains(report.Hint, "contents[0]: the GIF image exceeds the 1 MB limit on PNG data converted from GIF images in one request") ||
		!strings.Contains(report.Reason, "超过单次请求的转换上限") {
		t.Fatalf("合计上限: %+v err=%v", report, err)
	}
	restore()

	broken := []Content{{Role: RoleUser, Parts: []Part{{InlineData: &Blob{MIME: "image/gif", Data: []byte("GIF89a\x01")}}, {InlineData: &Blob{MIME: "image/png", Data: []byte("png?")}}}}}
	if _, report, err = PrepareInteractionImages(context.Background(), broken); err != nil || report.DriveImages != 2 ||
		!strings.Contains(report.Hint, "the GIF image could not be decoded") {
		t.Fatalf("坏 GIF: %+v err=%v", report, err)
	}
	if _, report, _ = PrepareInteractionImages(context.Background(), broken[:0:0]); report != (InteractionImageReport{}) {
		t.Fatalf("空内容: %+v", report)
	}
	if _, report, _ = PrepareInteractionImages(context.Background(), []Content{{Role: RoleUser, Parts: broken[0].Parts[1:]}}); !strings.Contains(report.Hint,
		"the image/png image data is not a valid PNG, JPEG, WebP, HEIC, HEIF, BMP, TIFF or GIF file") {
		t.Fatalf("声明 PNG 的坏数据: %q", report.Hint)
	}

	// 转换名额占满、请求已结束：返回 ctx 错误，不等
	for range cap(interactionConvertSlots) {
		interactionConvertSlots <- struct{}{}
	}
	defer func() {
		for range cap(interactionConvertSlots) {
			<-interactionConvertSlots
		}
	}()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := PrepareInteractionImages(ctx, contents); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v，期望 context.Canceled", err)
	}
}

// TestPrepareInteractionImagesProtocolGIF 协议层已把客户端的 GIF 转成 PNG（Blob.SourceGIF 为原 GIF）：与本服务层转换的 GIF 按出现顺序
// 共用单次请求的转换额度、计入转换张数与字节数；额度内原样内联（同一份数据），超过剩余额度时换回原 GIF（image/gif、原字节）按需要 Drive
// 处理并给出英文说明；原内容不改
func TestPrepareInteractionImagesProtocolGIF(t *testing.T) {
	gifData := omniTestGIF(t)
	single, ok := GIFFirstFramePNG(gifData)
	if !ok {
		t.Fatal("GIF 应能转换")
	}
	first := &Blob{MIME: "image/png", Data: single, SourceGIF: gifData}
	contents := []Content{{Role: RoleUser, Parts: []Part{{InlineData: first}, {Text: "随机视频"}}}}
	prepared, report, err := PrepareInteractionImages(context.Background(), contents)
	if err != nil || prepared[0].Parts[0].InlineData != first || report.Inline != 1 || report.InlineBytes != int64(len(single)) ||
		report.Converted != 1 || report.SourceBytes != int64(len(gifData)) || report.PNGBytes != int64(len(single)) || report.DriveImages != 0 {
		t.Fatalf("额度内: %+v err=%v", report, err)
	}

	restore := SetInteractionConvertLimitForTest(int64(len(single)) + 8)
	t.Cleanup(restore)
	second := &Blob{MIME: "image/png", Data: single, SourceGIF: gifData}
	contents = []Content{
		{Role: RoleUser, Parts: []Part{{InlineData: first}, {InlineData: &Blob{MIME: "image/gif", Data: gifData}}}},
		{Role: RoleAssistant, Parts: []Part{{Text: "好了"}}},
		{Role: RoleUser, Parts: []Part{{Text: "再来"}, {InlineData: second}}},
	}
	prepared, report, err = PrepareInteractionImages(context.Background(), contents)
	if err != nil {
		t.Fatal(err)
	}
	if prepared[0].Parts[0].InlineData != first || prepared[0].Parts[1].InlineData.MIME != "image/gif" {
		t.Fatalf("第一张在额度内应原样内联，第二张（本服务层转换）超出剩余额度应保留 GIF: %+v", prepared[0].Parts)
	}
	reverted := prepared[2].Parts[1].InlineData
	if reverted.MIME != "image/gif" || !bytes.Equal(reverted.Data, gifData) || reverted.SourceGIF != nil || !InteractionDriveMedia(prepared[2].Parts[1]) {
		t.Fatalf("协议层转好的 PNG 超出剩余额度应换回原 GIF、按需要 Drive 处理: %+v", reverted)
	}
	if contents[2].Parts[1].InlineData != second || prepared[2].Parts[0].Text != "再来" {
		t.Fatal("不应改动原内容，其他 part 保持原位")
	}
	if report.Inline != 1 || report.Converted != 1 || report.DriveImages != 2 || report.PNGBytes != int64(len(single)) ||
		!strings.HasPrefix(report.Hint, "contents[0]: the GIF image exceeds the 1 MB limit on PNG data converted from GIF images in one request") ||
		!strings.HasSuffix(report.Hint, "send the image as PNG, JPEG or WebP instead") {
		t.Fatalf("超额: %+v", report)
	}
	if hint := interactionDriveMediaMessage(2, reverted); !strings.Contains(hint, "this image/gif image cannot be sent inline to this model") {
		t.Fatalf("兜底说明 = %q", hint)
	}
}

// TestInteractionHistoryImagesInline 模型历史里的图片（回传的生成图片、从本服务链接恢复的图片）连同说明移到随后的用户 step 最前面，
// PNG、JPEG 与用户附件一样内联为 Content field 2（不需要 Drive）；用户 step 里的 part 按客户端给的顺序编码；图片数据不参与 binding
func TestInteractionHistoryImagesInline(t *testing.T) {
	pngData, jpegData := omniTestPNG(t), omniTestJPEG(t)
	encode := base64.StdEncoding.EncodeToString
	contents := []Content{
		{Role: RoleUser, Parts: []Part{{Text: "画两张"}}},
		{Role: RoleAssistant, Parts: []Part{
			{Text: "好了"},
			{InlineData: &Blob{MIME: "image/png", Data: pngData}, RestoredLink: true, RestoredText: "![image](/media/a.png)"},
			{InlineData: &Blob{MIME: "image/jpeg", Data: jpegData}, ThoughtSignature: "sig"},
		}},
		{Role: RoleUser, Parts: []Part{{Text: "先看这张"}, {InlineData: &Blob{MIME: "image/jpeg", Data: jpegData}}, {Text: "再动起来"}}},
	}
	steps, binding, hasModelTurn, err := encodeInteractionSteps(contents)
	if err != nil || !hasModelTurn {
		t.Fatalf("hasModelTurn=%v err=%v", hasModelTurn, err)
	}
	encoded, _ := json.Marshal(steps)
	want := `[[[[[["画两张"]]]]],[null,[[[["好了"]]]]],[[[[["` + interactionModelMediaIntro + `"]],[null,[1,"` + encode(pngData) + `"]],` +
		`[null,[2,"` + encode(jpegData) + `"]],[["先看这张"]],[null,[2,"` + encode(jpegData) + `"]],[["再动起来"]]]]]]`
	if string(encoded) != want {
		t.Fatalf("steps = %s\n期望 %s", encoded, want)
	}
	encodedBinding, _ := json.Marshal(binding)
	wantBinding := `[{"role":"user","parts":[{"text":"画两张"}]},{"role":"assistant","parts":[{"text":"好了"}]},` +
		`{"role":"user","parts":[{"text":"` + interactionModelMediaIntro + `"},{"text":"先看这张"},{"text":"再动起来"}]}]`
	if string(encodedBinding) != wantBinding {
		t.Fatalf("binding = %s\n期望 %s", encodedBinding, wantBinding)
	}
}

// TestDecodeInteractionJPEGImage 输出的 content delta field 2 带 MIME 枚举 2 时按 image/jpeg 解码（与输入的枚举相同）
func TestDecodeInteractionJPEGImage(t *testing.T) {
	jpegData := omniTestJPEG(t)
	interaction := make([]any, 13)
	interaction[1] = 3
	final := make([]any, 20)
	final[19] = []any{interaction}
	body, err := json.Marshal([]any{[]any{
		interactionDeltaEvent([]any{nil, []any{2, base64.StdEncoding.EncodeToString(jpegData)}}),
		final,
	}, []any{0}})
	if err != nil {
		t.Fatal(err)
	}
	var media *Media
	if err := DecodeInteractionStream(bytes.NewReader(body), func(event Event) error {
		if event.Kind == EventMedia {
			media = event.Media
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if media == nil || media.MIME != "image/jpeg" || !bytes.Equal(media.Data, jpegData) {
		t.Fatalf("field 2 枚举 2 = %+v", media)
	}
}
