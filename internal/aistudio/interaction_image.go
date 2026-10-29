package aistudio

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/gif"
	"image/png"
	"mime"
	"strings"
	"sync/atomic"
)

// Interaction 模型（gemini-omni）的图片输入按官网内联发送。官网给 gemini-omni-1.1-flash 附一张图片、输入文字后 Run 的两份抓包里，
// 用户 step 的 parts 依次为图片与文本，图片 part 为 Content field 2 = [MIME 枚举, Base64]：PNG 为枚举 1（与解码输出时 interactionImageMIME
// 的 1:image/png 一致）、JPEG 为枚举 2，Base64 是原文件字节（JPEG 的 JFIF、EXIF 与 XMP 段原样保留，没有转码、没有去掉元数据），
// 没有 Drive 文件引用，账号不需要 Drive 授权。视频的官网抓包则是先上传到 Drive、以 Content field 9 的文件引用发送，视频与其他文件照旧。
// 其他图片格式的枚举值没有证据，不猜：GIF 取首帧转成 PNG（与各协议整理内联 GIF 的做法相同；客户端内联的 GIF 已由协议层转好，
// 原 GIF 留在 Blob.SourceGIF）后按枚举 1 发送；标准库解不了的格式（WebP、HEIC、HEIF、AVIF 等）、超过像素或体积上限的 GIF 与视频一样
// 要先上传到账号的 Drive（Content field 9；协议层转好的 GIF 换回原 GIF 上传）

const (
	// interactionPNGEnum 为 Content field 2 图片的 MIME 枚举 1：image/png
	interactionPNGEnum int64 = 1
	// interactionJPEGEnum 为 Content field 2 图片的 MIME 枚举 2：image/jpeg（官网发送 JPEG 照片时的取值）
	interactionJPEGEnum int64 = 2
)

// maxImageConvertPixels 为 GIF 转 PNG 时允许的逻辑画布像素上限（4096×4096，约 64MB RGBA）：先只读头部（DecodeConfig）检查再解码。
// 头部声明的画布尺寸不受文件大小约束（35 字节的 GIF 就能声明 65535×65535），按声明尺寸分配会耗尽内存
const maxImageConvertPixels = 4096 * 4096

// defaultInteractionConvertLimit 为一次请求里 GIF 转出的 PNG 合计上限（64MB，与 api 包单次请求从媒体目录恢复的总量上限
// maxRestoredMediaBytes 相同，同为随请求展开的内联媒体）：PNG 无损，动图首帧转出来常比原 GIF 大得多。协议层已转成 PNG 的客户端 GIF
// （Blob.SourceGIF）与本服务层转换的 GIF 按出现顺序共用这份额度：超过剩余额度时本服务层放弃转换（编码到超出即停，不留下更大的缓冲），
// 协议层转好的换回原 GIF，都按需要 Drive 处理
const defaultInteractionConvertLimit int64 = 64 << 20

// interactionConvertLimit 为测试改小的合计上限（经 SetInteractionConvertLimitForTest），为 0 时取 defaultInteractionConvertLimit
var interactionConvertLimit atomic.Int64

// convertLimit 返回一次请求里 GIF 转出的 PNG 合计上限
func convertLimit() int64 {
	if limit := interactionConvertLimit.Load(); limit > 0 {
		return limit
	}
	return defaultInteractionConvertLimit
}

// SetInteractionConvertLimitForTest 把一次请求里 GIF 转出的 PNG 合计上限改为 limit（不大于 0 时恢复为 64MB），只供其他包的测试用小图
// 触发超额，返回恢复原值的函数
func SetInteractionConvertLimitForTest(limit int64) (restore func()) {
	previous := interactionConvertLimit.Swap(max(limit, 0))
	return func() { interactionConvertLimit.Store(previous) }
}

// interactionConvertSlots 限制同时进行的 GIF 转换数：一张 4096×4096 的 GIF 解码与转换时调色板帧、RGBA 画布与 PNG 缓冲合计可达
// 两百 MB，许多带 GIF 的请求同时到达时不让内存随并发无界增长
var interactionConvertSlots = make(chan struct{}, 2)

var (
	// pngSignature 为 PNG 文件签名
	pngSignature = []byte("\x89PNG\r\n\x1a\n")
	// jpegSignature 为 JPEG 文件开头（SOI 标记与随后的段标记前缀）
	jpegSignature = []byte{0xff, 0xd8, 0xff}
)

// errConvertedTooLarge 表示转出的 PNG 超过剩余额度
var errConvertedTooLarge = errors.New("转出的 PNG 超过上限")

// interactionImageFormat 为发往 Interaction 模型的内联附件的图片格式
type interactionImageFormat int

const (
	// interactionNotImage 为不是图片的附件（视频、音频、PDF 等，MIME 不是 image/*）
	interactionNotImage interactionImageFormat = iota
	interactionPNG
	interactionJPEG
	interactionGIF
	// interactionOtherImage 为文件头不是 PNG、JPEG、GIF 的图片（WebP、HEIC、HEIF、AVIF 等，或数据与声明的格式不符）
	interactionOtherImage
)

// imageMediaType 返回 MIME 的类型名（小写、去掉参数）；写法不合规时按去空白转小写的原文
func imageMediaType(value string) string {
	mediaType, _, err := mime.ParseMediaType(value)
	if err != nil {
		return strings.ToLower(strings.TrimSpace(value))
	}
	return mediaType
}

// declaredInlineImage 判断 MIME（已转小写、去掉参数）声明的是能内联或转换的图片格式：PNG、JPEG（含 image/jpg、image/pjpeg 等别名）与 GIF
func declaredInlineImage(mediaType string) bool {
	switch mediaType {
	case "image/png", "image/jpeg", "image/jpg", "image/pjpeg", "image/gif":
		return true
	}
	return false
}

// imageFormatOf 返回内联附件的图片格式：MIME（不分大小写）不是 image/* 时不是图片；是图片时以文件头为准（PNG 签名、JPEG 的 SOI 标记、
// GIF87a/GIF89a），客户端标错 MIME 的图片按实际格式处理，文件头不是这三种的为其他图片（声明为 PNG、JPEG 而数据不是时同样如此）
func imageFormatOf(blob *Blob) interactionImageFormat {
	if !strings.HasPrefix(imageMediaType(blob.MIME), "image/") {
		return interactionNotImage
	}
	switch data := blob.Data; {
	case bytes.HasPrefix(data, pngSignature):
		return interactionPNG
	case bytes.HasPrefix(data, jpegSignature):
		return interactionJPEG
	case bytes.HasPrefix(data, []byte("GIF87a")) || bytes.HasPrefix(data, []byte("GIF89a")):
		return interactionGIF
	}
	return interactionOtherImage
}

// interactionImageEnum 返回内联图片按官网发送的 Content field 2 MIME 枚举：PNG 为 1、JPEG 为 2；其他格式没有可用的枚举，返回 false
func interactionImageEnum(blob *Blob) (int64, bool) {
	switch imageFormatOf(blob) {
	case interactionPNG:
		return interactionPNGEnum, true
	case interactionJPEG:
		return interactionJPEGEnum, true
	}
	return 0, false
}

// InteractionInlineImage 判断 part 是 Interaction 模型按官网直接内联发送的图片（Content field 2）：PNG 或 JPEG 的 inline data，
// 原字节发送；GIF 经 PrepareInteractionImages 转成 PNG 后也是
func InteractionInlineImage(part Part) bool {
	if part.InlineData == nil {
		return false
	}
	_, ok := interactionImageEnum(part.InlineData)
	return ok
}

// InteractionDriveMedia 判断 part 是 Interaction 模型要先上传到账号 Drive 再以文件引用发送的内联附件：视频、其他非图片文件，
// 以及不能内联的图片（标准库解不了的格式、超过像素或体积上限或解码失败而没能转成 PNG 的 GIF）
func InteractionDriveMedia(part Part) bool {
	return part.InlineData != nil && !InteractionInlineImage(part)
}

// InteractionImageReport 为 PrepareInteractionImages 的整理结果，写进请求进度与需要 Drive 的错误说明
type InteractionImageReport struct {
	// Inline 为按官网直接内联发送的图片数（PNG、JPEG 与转成 PNG 的 GIF），InlineBytes 为它们发送的字节数合计
	Inline      int
	InlineBytes int64
	// Converted 为其中由 GIF 首帧转成 PNG 的张数（含协议层已转好的客户端 GIF），SourceBytes、PNGBytes 为这些转换前后的合计字节数
	Converted             int
	SourceBytes, PNGBytes int64
	// DriveImages 为不能内联、要先上传 Drive 的图片数；Hint 为第一张的英文说明（号池里没有 Drive 授权的账号时随错误返回客户端，
	// 见 DriveRequiredError.ImageHint），Reason 为第一张的中文原因（写请求进度）
	DriveImages  int
	Hint, Reason string
}

// imageIssue 为一张图片不能内联的原因：hint 为英文（返回客户端），reason 为中文（写请求进度）
type imageIssue struct {
	hint, reason string
}

// PrepareInteractionImages 整理发往 Interaction 模型的内联图片（用户附件、会移到用户 step 的模型历史图片、从本服务链接恢复的图片都算）：
// PNG、JPEG 原样保留（按官网原字节内联发送）；GIF 取首帧转成 PNG 后改写为 image/png（解码前用 DecodeConfig 检查画布不超过
// 4096×4096，一次请求转出的 PNG 合计不超过 64MB，同时进行的转换数有限）；其他格式与没能转换的 GIF 原样保留，按需要 Drive 处理
// （InteractionDriveMedia）。协议层已把客户端内联的 GIF 转成 PNG 的（带 SourceGIF）同样计入转换张数与合计额度，超过剩余额度时换回原 GIF
// （image/gif）、按需要 Drive 处理，与本服务层转换超额时相同。视频与其他附件不动。返回新的内容（改动过的消息复制 part 列表，
// 不改动 contents）；只在等待转换名额时 ctx 结束才返回错误
func PrepareInteractionImages(ctx context.Context, contents []Content) ([]Content, InteractionImageReport, error) {
	var report InteractionImageReport
	result := contents
	cloned := make(map[int]bool)
	remaining := convertLimit()
	// replace 把 contents[contentIndex].Parts[partIndex] 的 inline data 换成 blob（第一次改动时复制 contents 与那条消息的 part 列表）
	replace := func(contentIndex, partIndex int, blob *Blob) {
		if !cloned[contentIndex] {
			if len(cloned) == 0 {
				result = append([]Content(nil), contents...)
			}
			cloned[contentIndex] = true
			result[contentIndex].Parts = append([]Part(nil), contents[contentIndex].Parts...)
		}
		result[contentIndex].Parts[partIndex].InlineData = blob
	}
	// convertedInline 记下一张由 GIF 转成、按枚举 1 内联发送的 PNG
	convertedInline := func(source, converted int) {
		remaining -= int64(converted)
		report.Inline++
		report.InlineBytes += int64(converted)
		report.Converted++
		report.SourceBytes += int64(source)
		report.PNGBytes += int64(converted)
	}
	needsDrive := func(issue *imageIssue) {
		report.DriveImages++
		if report.Hint == "" {
			report.Hint, report.Reason = issue.hint, issue.reason
		}
	}
	for contentIndex, content := range contents {
		for partIndex, part := range content.Parts {
			if part.InlineData == nil {
				continue
			}
			format := imageFormatOf(part.InlineData)
			switch format {
			case interactionNotImage:
				continue
			case interactionPNG, interactionJPEG:
				if source := part.InlineData.SourceGIF; format == interactionPNG && len(source) > 0 {
					// 协议层已把客户端的 GIF 转成 PNG：与本服务层的转换共用额度，超出时换回原 GIF 上传 Drive
					if size := int64(len(part.InlineData.Data)); size > remaining {
						needsDrive(convertLimitIssue(contentIndex))
						replace(contentIndex, partIndex, &Blob{MIME: "image/gif", Data: source})
						continue
					}
					convertedInline(len(source), len(part.InlineData.Data))
					continue
				}
				report.Inline++
				report.InlineBytes += int64(len(part.InlineData.Data))
				continue
			}
			converted, issue, err := convertGIFForInteraction(ctx, contentIndex, part.InlineData, format, remaining)
			if err != nil {
				return nil, InteractionImageReport{}, err
			}
			if issue != nil {
				needsDrive(issue)
				continue
			}
			convertedInline(len(part.InlineData.Data), len(converted))
			replace(contentIndex, partIndex, &Blob{MIME: "image/png", Data: converted})
		}
	}
	return result, report, nil
}

// convertLimitIssue 为 GIF 转出的 PNG 超过单次请求剩余转换额度时的说明
func convertLimitIssue(index int) *imageIssue {
	// 按 MB 向上取整（测试改小的上限不足 1MB 时写 1 MB）
	limitMB := (convertLimit() + 1<<20 - 1) >> 20
	return &imageIssue{
		hint: fmt.Sprintf("contents[%d]: the GIF image exceeds the %d MB limit on PNG data converted from GIF images in one request, "+
			"so it cannot be sent inline to this model; send the image as PNG or JPEG instead", index, limitMB),
		reason: fmt.Sprintf("contents[%d] 的 GIF 转为 PNG 后超过单次请求的转换上限 %dMB（一次请求转出的 PNG 合计）", index, limitMB),
	}
}

// convertGIFForInteraction 把 GIF 首帧转成 PNG，转出的 PNG 不超过 limit；其他格式与不能转换的 GIF 返回原因。index 为所在的 contents
// 下标（写进说明）。只在等待转换名额时 ctx 结束才返回错误
func convertGIFForInteraction(ctx context.Context, index int, blob *Blob, format interactionImageFormat, limit int64) ([]byte, *imageIssue, error) {
	mediaType := imageMediaType(blob.MIME)
	if format != interactionGIF {
		if declaredInlineImage(mediaType) {
			return nil, &imageIssue{
				hint: fmt.Sprintf("contents[%d]: the %s image data is not a valid PNG, JPEG or GIF file and cannot be sent inline to this model; "+
					"send a valid PNG or JPEG image instead", index, mediaType),
				reason: fmt.Sprintf("contents[%d] 声明为 %s，数据的文件头却不是 PNG、JPEG 或 GIF，不能内联", index, mediaType),
			}, nil
		}
		return nil, &imageIssue{
			hint: fmt.Sprintf("contents[%d]: %s images cannot be sent inline to this model (only PNG and JPEG images are, and GIF is converted to PNG); "+
				"send the image as PNG or JPEG instead", index, mediaType),
			reason: fmt.Sprintf("contents[%d] 的 %s 图片不是 PNG、JPEG 或 GIF，不能内联、也不能转为 PNG", index, mediaType),
		}, nil
	}
	decodeIssue := func(err error) *imageIssue {
		return &imageIssue{
			hint:   fmt.Sprintf("contents[%d]: the GIF image could not be decoded for converting to PNG; send the image as PNG or JPEG instead", index),
			reason: fmt.Sprintf("contents[%d] 的 GIF 解码失败（%v），不能转为 PNG", index, err),
		}
	}
	config, err := gif.DecodeConfig(bytes.NewReader(blob.Data))
	if err != nil {
		return nil, decodeIssue(err), nil
	}
	if config.Width <= 0 || config.Height <= 0 || int64(config.Width)*int64(config.Height) > maxImageConvertPixels {
		return nil, &imageIssue{
			hint: fmt.Sprintf("contents[%d]: the GIF image is %dx%d pixels, larger than the 4096x4096 pixels that can be converted to PNG "+
				"for inline input; send a smaller image as PNG or JPEG instead", index, config.Width, config.Height),
			reason: fmt.Sprintf("contents[%d] 的 GIF 画布为 %d×%d 像素，超过转 PNG 的上限 4096×4096", index, config.Width, config.Height),
		}, nil
	}
	select {
	case interactionConvertSlots <- struct{}{}:
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	}
	defer func() { <-interactionConvertSlots }()
	frame, err := gifFirstFrame(blob.Data, config)
	if err != nil {
		return nil, decodeIssue(err), nil
	}
	encoded, err := encodePNGWithin(frame, limit)
	if errors.Is(err, errConvertedTooLarge) {
		return nil, convertLimitIssue(index), nil
	}
	if err != nil {
		return nil, decodeIssue(err), nil
	}
	return encoded, nil, nil
}

// gifFirstFrame 把 GIF 首帧按逻辑画布画出：保留帧位置；首帧不透明时画布先铺全局色表的背景色，透明首帧保留透明画布。
// config 为 gif.DecodeConfig 的结果，调用方已检查画布像素数（解码器按帧尺寸分配，帧又必须落在画布内）
func gifFirstFrame(data []byte, config image.Config) (image.Image, error) {
	decoded, err := gif.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	frame, ok := decoded.(*image.Paletted)
	if !ok {
		return nil, errors.New("GIF 首帧不是调色板图像")
	}
	canvas := image.NewNRGBA(image.Rect(0, 0, config.Width, config.Height))
	transparent := false
	for _, entry := range frame.Palette {
		_, _, _, alpha := entry.RGBA()
		transparent = transparent || alpha == 0
	}
	// GIF 背景色来自全局色表（逻辑屏幕描述符第 11 字节为背景色下标）
	if palette, ok := config.ColorModel.(color.Palette); ok && !transparent && len(data) > 11 && int(data[11]) < len(palette) {
		draw.Draw(canvas, canvas.Bounds(), image.NewUniform(palette[data[11]]), image.Point{}, draw.Src)
	}
	draw.Draw(canvas, frame.Bounds(), frame, frame.Bounds().Min, draw.Over)
	return canvas, nil
}

// limitedPNGBuffer 为不超过 limit 字节的 PNG 输出缓冲：超出时写入失败，编码随之停止
type limitedPNGBuffer struct {
	bytes.Buffer
	limit int64
}

func (buffer *limitedPNGBuffer) Write(data []byte) (int, error) {
	if int64(buffer.Len())+int64(len(data)) > buffer.limit {
		return 0, errConvertedTooLarge
	}
	return buffer.Buffer.Write(data)
}

// encodePNGWithin 把图像编码为 PNG，输出超过 limit 字节时返回 errConvertedTooLarge
func encodePNGWithin(source image.Image, limit int64) ([]byte, error) {
	buffer := &limitedPNGBuffer{limit: limit}
	if err := png.Encode(buffer, source); err != nil {
		if errors.Is(err, errConvertedTooLarge) {
			return nil, errConvertedTooLarge
		}
		return nil, err
	}
	return buffer.Bytes(), nil
}

// GIFFirstFramePNG 把 GIF 首帧按逻辑画布转成 PNG（保留帧位置与透明背景），供各协议整理内联 GIF（api 包 normalizeImagePayload，所有模型
// 相同，不限转出大小、不占转换名额；发往 Interaction 模型的由 PrepareInteractionImages 按 SourceGIF 计入单次请求的合计额度）；
// 不是 GIF、画布超过 4096×4096 或解码失败时返回 false，由调用方原样发送
func GIFFirstFramePNG(data []byte) ([]byte, bool) {
	config, err := gif.DecodeConfig(bytes.NewReader(data))
	if err != nil || config.Width <= 0 || config.Height <= 0 || int64(config.Width)*int64(config.Height) > maxImageConvertPixels {
		return nil, false
	}
	frame, err := gifFirstFrame(data, config)
	if err != nil {
		return nil, false
	}
	var buffer bytes.Buffer
	if err := png.Encode(&buffer, frame); err != nil {
		return nil, false
	}
	return buffer.Bytes(), true
}

// interactionDriveMediaMessage 为 Interaction 模型的内联附件仍需上传 Drive、却还是 inline data 时的英文说明（原样返回客户端）：
// app 层在发送前已把这类请求交给有 Drive 授权的账号，这里只是兜底
func interactionDriveMediaMessage(index int, blob *Blob) string {
	if imageFormatOf(blob) != interactionNotImage {
		return fmt.Sprintf("contents[%d]: this %s image cannot be sent inline to this model (only PNG and JPEG images are, and GIF is converted to PNG) "+
			"and must be uploaded to Google Drive first, which requires an account with Google Drive authorized; send the image as PNG or JPEG instead",
			index, imageMediaType(blob.MIME))
	}
	return fmt.Sprintf("contents[%d]: video and file input for this model must be uploaded to Google Drive first, "+
		"which requires an account with Google Drive authorized", index)
}
