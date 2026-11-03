package aistudio

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/gif"
	"image/png"
	"maps"
	"mime"
	"slices"
	"strings"
	"sync/atomic"
)

// Interaction 模型（gemini-omni）的图片输入按官网内联发送。官网给 gemini-omni-1.1-flash 附一张图片、输入文字后 Run 的两份抓包里，
// 用户 step 的 parts 依次为图片与文本，图片 part 为 Content field 2 = [MIME 枚举, Base64]：PNG 为枚举 1、JPEG 为枚举 2，Base64 是原文件字节
// （JPEG 的 JFIF、EXIF 与 XMP 段原样保留，没有转码、没有去掉元数据），没有 Drive 文件引用，账号不需要 Drive 授权。视频的官网抓包则是先上传到
// Drive、以 Content field 9 的文件引用发送，视频与其他文件照旧。
// 枚举取官网前端的媒体 MIME 枚举表（interactionMediaMIME，输入 step 与输出 delta 是同一种 Content 结构，两份抓包的 PNG 1、JPEG 2 与表一致）：
// 文件头识别为 PNG、JPEG、WebP（3）、HEIC（4）、HEIF（5）、BMP（8）、TIFF（9）的图片原字节内联；GIF 取首帧转成 PNG（与各协议整理内联 GIF 的
// 做法相同；客户端内联的 GIF 已由协议层转好，原 GIF 留在 Blob.SourceGIF）后按枚举 1 发送。表里没有的格式（AVIF、SVG、ICO 等）、数据与声明的
// 格式不符、超过像素或体积上限的 GIF 与视频一样要先上传到账号的 Drive（Content field 9；协议层转好的 GIF 换回原 GIF 上传）。
// MIME 为空或 application/octet-stream 一类、文件头却是上面这些图片格式的附件按图片处理（有些下载器与中转把图片标成 octet-stream）；
// 明确声明为 video/*、audio/* 等其他类型的不按文件头改判

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

const (
	// inlineImageFormatsText 为能内联发送的图片格式（英文说明里用），inlineImageFormatsZH 为中文说明里用的同一份列表
	inlineImageFormatsText = "PNG, JPEG, WebP, HEIC, HEIF, BMP, TIFF or GIF"
	inlineImageFormatsZH   = "PNG、JPEG、WebP、HEIC、HEIF、BMP、TIFF 与 GIF"
	// inlineImageFormatsNote 为英文说明里括注的能内联的图片格式
	inlineImageFormatsNote = "only PNG, JPEG, WebP, HEIC, HEIF, BMP and TIFF images are, and GIF is converted to PNG"
)

// errConvertedTooLarge 表示转出的 PNG 超过剩余额度
var errConvertedTooLarge = errors.New("转出的 PNG 超过上限")

// interactionImageFormat 为发往 Interaction 模型的内联附件的图片格式
type interactionImageFormat int

const (
	// interactionNotImage 为不是图片的附件（视频、音频、PDF 等：MIME 不是 image/*，或 MIME 为空、octet-stream 而文件头不是已知图片格式）
	interactionNotImage interactionImageFormat = iota
	interactionPNG
	interactionJPEG
	interactionWebP
	interactionHEIC
	interactionHEIF
	interactionBMP
	interactionTIFF
	interactionGIF
	// interactionOtherImage 为声明为 image/*、文件头却不是上面这些格式的图片（AVIF、SVG、ICO 等，或数据与声明的格式不符）
	interactionOtherImage
)

// interactionImageInput 为一种能原字节内联发送的图片格式：name 为写进日志与说明的名称，mime 为官网枚举表里对应的 MIME
type interactionImageInput struct {
	name string
	mime string
}

// interactionImageInputMIME 为能原字节内联发送的图片格式（GIF 不在其中：转成 PNG 后按 PNG 发送）
var interactionImageInputMIME = map[interactionImageFormat]interactionImageInput{
	interactionPNG:  {name: "PNG", mime: "image/png"},
	interactionJPEG: {name: "JPEG", mime: "image/jpeg"},
	interactionWebP: {name: "WebP", mime: "image/webp"},
	interactionHEIC: {name: "HEIC", mime: "image/heic"},
	interactionHEIF: {name: "HEIF", mime: "image/heif"},
	interactionBMP:  {name: "BMP", mime: "image/bmp"},
	interactionTIFF: {name: "TIFF", mime: "image/tiff"},
}

// interactionImageInputEnum 为各内联图片格式在官网枚举表（interactionMediaMIME 的图片一类）里的枚举：PNG 1、JPEG 2、WebP 3、HEIC 4、
// HEIF 5、BMP 8、TIFF 9，按 MIME 从表里反查，不另写一份
var interactionImageInputEnum = func() map[interactionImageFormat]int64 {
	enums := make(map[interactionImageFormat]int64, len(interactionImageInputMIME))
	for format, input := range interactionImageInputMIME {
		for code, mimeType := range interactionMediaMIME[1].codes {
			if mimeType == input.mime {
				enums[format] = code
			}
		}
	}
	return enums
}()

// interactionImageEnumName 返回图片枚举的格式名（写进请求结构摘要）：内联格式用 interactionImageInputMIME 的名称，表里的其他枚举用
// MIME 子类型的大写，不在表里时为“枚举 N”
func interactionImageEnumName(code int64) string {
	for format, enum := range interactionImageInputEnum {
		if enum == code {
			return interactionImageInputMIME[format].name
		}
	}
	if mimeType, ok := interactionMediaMIME[1].codes[code]; ok {
		return strings.ToUpper(strings.TrimPrefix(mimeType, "image/"))
	}
	return fmt.Sprintf("枚举 %d", code)
}

var (
	// heicBrands 为 HEIF 容器里表示 HEVC 编码图片（HEIC）的 ftyp 品牌
	heicBrands = []string{"heic", "heix", "heim", "heis", "hevc", "hevx", "hevm", "hevs"}
	// heifBrands 为通用 HEIF 品牌
	heifBrands = []string{"mif1", "msf1", "mif2"}
	// avifBrands 为 AVIF 的品牌：AVIF 同样是 HEIF 容器（兼容品牌常带 mif1），但官网枚举表里没有 AVIF
	avifBrands = []string{"avif", "avis"}
)

// isoBrands 返回 ISO 基本媒体文件（HEIF、MP4 等）开头 ftyp 盒的主品牌与兼容品牌（最多读到盒尾或前 256 字节）；不是 ftyp 开头时 ok 为 false
func isoBrands(data []byte) (major string, compatible []string, ok bool) {
	if len(data) < 16 || string(data[4:8]) != "ftyp" {
		return "", nil, false
	}
	size := int(binary.BigEndian.Uint32(data[:4]))
	if size < 16 || size > len(data) {
		size = len(data)
	}
	size = min(size, 256)
	for offset := 16; offset+4 <= size; offset += 4 {
		compatible = append(compatible, string(data[offset:offset+4]))
	}
	return string(data[8:12]), compatible, true
}

// sniffHEIF 按 ftyp 品牌判断 HEIF 容器里的图片：主品牌或兼容品牌里有 HEVC 品牌的为 HEIC；主品牌为通用 HEIF 品牌、兼容品牌里没有 AVIF
// 品牌的为 HEIF；AVIF（主品牌为 avif、avis，或通用品牌加 AVIF 兼容品牌）与视频（mp4、mov 等其他品牌）不算。声明的 MIME 为 image/heic 或
// image/heif 时按声明区分两者（官网前端按文件的 MIME 取枚举）
func sniffHEIF(data []byte, mediaType string) interactionImageFormat {
	major, compatible, ok := isoBrands(data)
	if !ok || slices.Contains(avifBrands, major) {
		return interactionNotImage
	}
	brand := func(set []string) func(string) bool {
		return func(value string) bool { return slices.Contains(set, value) }
	}
	heic := slices.Contains(heicBrands, major) || slices.ContainsFunc(compatible, brand(heicBrands))
	heif := slices.Contains(heifBrands, major) && !slices.ContainsFunc(compatible, brand(avifBrands))
	if !heic && !heif {
		return interactionNotImage
	}
	switch mediaType {
	case "image/heic", "image/heic-sequence":
		return interactionHEIC
	case "image/heif", "image/heif-sequence":
		return interactionHEIF
	}
	if heic {
		return interactionHEIC
	}
	return interactionHEIF
}

// bmpHeaderSizes 为 BMP 的 DIB 头长度（BITMAPCOREHEADER 12 到 BITMAPV5HEADER 124）：文件头 “BM” 只有两个字节，再看 DIB 头长度，
// 免得把恰好以 “BM” 开头的其他数据当成 BMP
var bmpHeaderSizes = []uint32{12, 16, 40, 52, 56, 64, 108, 124}

// sniffImageFormat 按文件头识别图片格式：PNG 签名、JPEG 的 SOI 标记、GIF87a/GIF89a、RIFF…WEBP、BMP 的 “BM” 与 DIB 头长度、TIFF 的
// II*\0 与 MM\0*、HEIF 容器的 ftyp 品牌；都不是时返回 interactionNotImage。mediaType 为声明的 MIME，只用来区分 HEIC 与 HEIF
func sniffImageFormat(data []byte, mediaType string) interactionImageFormat {
	switch {
	case bytes.HasPrefix(data, pngSignature):
		return interactionPNG
	case bytes.HasPrefix(data, jpegSignature):
		return interactionJPEG
	case bytes.HasPrefix(data, []byte("GIF87a")) || bytes.HasPrefix(data, []byte("GIF89a")):
		return interactionGIF
	case len(data) >= 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WEBP":
		return interactionWebP
	case len(data) >= 18 && string(data[:2]) == "BM" && slices.Contains(bmpHeaderSizes, binary.LittleEndian.Uint32(data[14:18])):
		return interactionBMP
	case len(data) >= 8 && (bytes.HasPrefix(data, []byte("II*\x00")) || bytes.HasPrefix(data, []byte("MM\x00*"))):
		return interactionTIFF
	}
	return sniffHEIF(data, mediaType)
}

// undeclaredMediaType 判断 MIME（已转小写、去掉参数）没有说明类型：为空或 application/octet-stream 一类，这时按文件头判断是不是图片
func undeclaredMediaType(mediaType string) bool {
	switch mediaType {
	case "", "application/octet-stream", "binary/octet-stream", "application/x-octet-stream", "application/unknown":
		return true
	}
	return false
}

// imageMediaType 返回 MIME 的类型名（小写、去掉参数）；写法不合规时按去空白转小写的原文
func imageMediaType(value string) string {
	mediaType, _, err := mime.ParseMediaType(value)
	if err != nil {
		return strings.ToLower(strings.TrimSpace(value))
	}
	return mediaType
}

// declaredInlineImage 判断 MIME（已转小写、去掉参数）声明的是能内联或转换的图片格式：PNG、JPEG（含 image/jpg、image/pjpeg 等别名）、
// WebP、HEIC、HEIF、BMP、TIFF 与 GIF
func declaredInlineImage(mediaType string) bool {
	switch mediaType {
	case "image/png", "image/jpeg", "image/jpg", "image/pjpeg", "image/gif", "image/webp", "image/heic", "image/heif",
		"image/heic-sequence", "image/heif-sequence", "image/bmp", "image/x-bmp", "image/x-ms-bmp", "image/tiff", "image/tif":
		return true
	}
	return false
}

// imageFormatOf 返回内联附件的图片格式，以文件头为准（见 sniffImageFormat），客户端标错 MIME 的图片按实际格式处理：
// MIME（不分大小写）为 image/* 时文件头不是已知格式的为其他图片（声明为 PNG、JPEG 等而数据不是时同样如此）；MIME 为空或 octet-stream 一类时
// 文件头是已知图片格式的按该格式，否则不是图片；其他 MIME（video/*、audio/*、application/pdf 等）不是图片
func imageFormatOf(blob *Blob) interactionImageFormat {
	mediaType := imageMediaType(blob.MIME)
	declared := strings.HasPrefix(mediaType, "image/")
	if !declared && !undeclaredMediaType(mediaType) {
		return interactionNotImage
	}
	format := sniffImageFormat(blob.Data, mediaType)
	if format == interactionNotImage && declared {
		return interactionOtherImage
	}
	return format
}

// interactionImageEnum 返回内联图片按官网发送的 Content field 2 MIME 枚举（见 interactionImageInputEnum）：PNG 1、JPEG 2、WebP 3、
// HEIC 4、HEIF 5、BMP 8、TIFF 9；GIF（要先转成 PNG）与其他格式返回 false
func interactionImageEnum(blob *Blob) (int64, bool) {
	enum, ok := interactionImageInputEnum[imageFormatOf(blob)]
	return enum, ok
}

// InteractionInlineImage 判断 part 是 Interaction 模型按官网直接内联发送的图片（Content field 2）：PNG、JPEG、WebP、HEIC、HEIF、BMP 或 TIFF
// 的 inline data（按文件头识别，MIME 为空或 octet-stream 的也算），原字节发送；GIF 经 PrepareInteractionImages 转成 PNG 后也是
func InteractionInlineImage(part Part) bool {
	if part.InlineData == nil {
		return false
	}
	_, ok := interactionImageEnum(part.InlineData)
	return ok
}

// InteractionDriveMedia 判断 part 是 Interaction 模型要先上传到账号 Drive 再以文件引用发送的内联附件：视频、音频、PDF 等其他文件，
// 以及不能内联的图片（官网枚举表里没有的格式如 AVIF、数据与声明的格式不符、超过像素或体积上限或解码失败而没能转成 PNG 的 GIF）
func InteractionDriveMedia(part Part) bool {
	return part.InlineData != nil && !InteractionInlineImage(part)
}

// InteractionImageReport 为 PrepareInteractionImages 的整理结果，写进请求进度与需要 Drive 的错误说明
type InteractionImageReport struct {
	// Inline 为按官网直接内联发送的图片数（PNG、JPEG、WebP、HEIC、HEIF、BMP、TIFF 与转成 PNG 的 GIF），InlineBytes 为它们发送的字节数合计；
	// Formats 为其中各格式的张数，按格式名排序写成“JPEG 1、PNG 2”（格式名见 interactionImageInputMIME，转成 PNG 的 GIF 计为 PNG；
	// 没有内联图片时为空）
	Inline      int
	InlineBytes int64
	Formats     string
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
// PNG、JPEG、WebP、HEIC、HEIF、BMP、TIFF 原样保留（按官网原字节内联发送，MIME 为空或 octet-stream 的按文件头识别）；GIF 取首帧转成 PNG 后
// 改写为 image/png（解码前用 DecodeConfig 检查画布不超过 4096×4096，一次请求转出的 PNG 合计不超过 64MB，同时进行的转换数有限）；官网枚举表
// 里没有的格式与没能转换的 GIF 原样保留，按需要 Drive 处理（InteractionDriveMedia）。协议层已把客户端内联的 GIF 转成 PNG 的（带 SourceGIF）同样计入转换张数与合计额度，超过剩余额度时换回原 GIF
// （image/gif）、按需要 Drive 处理，与本服务层转换超额时相同。视频与其他附件不动。返回新的内容（改动过的消息复制 part 列表，
// 不改动 contents）；只在等待转换名额时 ctx 结束才返回错误
func PrepareInteractionImages(ctx context.Context, contents []Content) ([]Content, InteractionImageReport, error) {
	var report InteractionImageReport
	formats := make(map[string]int)
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
	// inline 记下一张按官网原字节内联发送的图片
	inline := func(format interactionImageFormat, size int) {
		report.Inline++
		report.InlineBytes += int64(size)
		formats[interactionImageInputMIME[format].name]++
	}
	// convertedInline 记下一张由 GIF 转成、按枚举 1 内联发送的 PNG
	convertedInline := func(source, converted int) {
		remaining -= int64(converted)
		inline(interactionPNG, converted)
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
			if format == interactionNotImage {
				continue
			}
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
			if _, ok := interactionImageInputMIME[format]; ok {
				inline(format, len(part.InlineData.Data))
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
	names := slices.Sorted(maps.Keys(formats))
	counts := make([]string, 0, len(names))
	for _, name := range names {
		counts = append(counts, fmt.Sprintf("%s %d", name, formats[name]))
	}
	report.Formats = strings.Join(counts, "、")
	return result, report, nil
}

// convertLimitIssue 为 GIF 转出的 PNG 超过单次请求剩余转换额度时的说明
func convertLimitIssue(index int) *imageIssue {
	// 按 MB 向上取整（测试改小的上限不足 1MB 时写 1 MB）
	limitMB := (convertLimit() + 1<<20 - 1) >> 20
	return &imageIssue{
		hint: fmt.Sprintf("contents[%d]: the GIF image exceeds the %d MB limit on PNG data converted from GIF images in one request, "+
			"so it cannot be sent inline to this model; send the image as PNG, JPEG or WebP instead", index, limitMB),
		reason: fmt.Sprintf("contents[%d] 的 GIF 转为 PNG 后超过单次请求的转换上限 %dMB（一次请求转出的 PNG 合计）", index, limitMB),
	}
}

// convertGIFForInteraction 把 GIF 首帧转成 PNG，转出的 PNG 不超过 limit；不能内联的其他格式与不能转换的 GIF 返回原因。index 为所在的 contents
// 下标（写进说明）。只在等待转换名额时 ctx 结束才返回错误
func convertGIFForInteraction(ctx context.Context, index int, blob *Blob, format interactionImageFormat, limit int64) ([]byte, *imageIssue, error) {
	mediaType := imageMediaType(blob.MIME)
	if format != interactionGIF {
		if declaredInlineImage(mediaType) {
			return nil, &imageIssue{
				hint: fmt.Sprintf("contents[%d]: the %s image data is not a valid %s file and cannot be sent inline to this model; "+
					"send a valid image in one of these formats instead", index, mediaType, inlineImageFormatsText),
				reason: fmt.Sprintf("contents[%d] 声明为 %s，数据的文件头却不是 %s，不能内联", index, mediaType, inlineImageFormatsZH),
			}, nil
		}
		return nil, &imageIssue{
			hint: fmt.Sprintf("contents[%d]: %s images cannot be sent inline to this model (%s); "+
				"send the image as PNG, JPEG or WebP instead", index, mediaType, inlineImageFormatsNote),
			reason: fmt.Sprintf("contents[%d] 的 %s 图片不是能内联的格式（%s），不能内联", index, mediaType, inlineImageFormatsZH),
		}, nil
	}
	decodeIssue := func(err error) *imageIssue {
		return &imageIssue{
			hint:   fmt.Sprintf("contents[%d]: the GIF image could not be decoded for converting to PNG; send the image as PNG, JPEG or WebP instead", index),
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
				"for inline input; send a smaller image or send it as PNG, JPEG or WebP instead", index, config.Width, config.Height),
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
		return fmt.Sprintf("contents[%d]: this %s image cannot be sent inline to this model (%s) "+
			"and must be uploaded to Google Drive first, which requires an account with Google Drive authorized; send the image as PNG, JPEG or WebP instead",
			index, imageMediaType(blob.MIME), inlineImageFormatsNote)
	}
	return fmt.Sprintf("contents[%d]: video, audio and document input for this model must be uploaded to Google Drive first, "+
		"which requires an account with Google Drive authorized", index)
}
