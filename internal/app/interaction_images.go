package app

import (
	"fmt"
	"slices"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// Interaction 模型（gemini-omni）的图片按官网内联发送（Content field 2：PNG 为 [1, Base64]、JPEG 为 [2, Base64]，原文件字节，不经 Drive，
// 见 aistudio.PrepareInteractionImages）：generateWithRetry 在选号前把 GIF 首帧转成 PNG，只有视频、其他文件与无法转为 PNG 的图片
// （aistudio.InteractionDriveMedia）才按 Drive 状态选号、上传到账号的 Drive。只有图片的 Omni 请求与纯文本请求一样选号，不查 Drive 状态、
// 不换 Drive token、不因账号没有 Drive 授权换号

// interactionDriveMediaInput 返回内容里需要先上传 Drive 的内联附件数与原始字节数（aistudio.InteractionDriveMedia）
func interactionDriveMediaInput(contents []aistudio.Content) (int, int64) {
	count := 0
	var bytes int64
	for _, content := range contents {
		for _, part := range content.Parts {
			if aistudio.InteractionDriveMedia(part) {
				count++
				bytes += int64(len(part.InlineData.Data))
			}
		}
	}
	return count, bytes
}

// heldInlineImage 为上传 Drive 前从内容里拿掉的内联图片：content、part 为它所在的位置
type heldInlineImage struct {
	content int
	part    int
	blob    *aistudio.Blob
}

// holdInlineImages 返回拿掉内联图片（PNG、JPEG，aistudio.InteractionInlineImage）inline data 的内容副本与拿掉的图片（不改动 contents）：
// 交给 UploadInlineMediaToLease 的内容只剩需要 Drive 的附件，图片不上传（上传只改写 inline data 所在的 part，不增删消息与 part，
// 之后按位置放回）
func holdInlineImages(contents []aistudio.Content) ([]aistudio.Content, []heldInlineImage) {
	var held []heldInlineImage
	result := contents
	for contentIndex, content := range contents {
		copied := false
		for partIndex, part := range content.Parts {
			if !aistudio.InteractionInlineImage(part) {
				continue
			}
			if !copied {
				if len(held) == 0 {
					result = slices.Clone(contents)
				}
				result[contentIndex].Parts = slices.Clone(content.Parts)
				copied = true
			}
			held = append(held, heldInlineImage{content: contentIndex, part: partIndex, blob: part.InlineData})
			result[contentIndex].Parts[partIndex].InlineData = nil
		}
	}
	return result, held
}

// restoreInlineImages 把 holdInlineImages 拿掉的图片按位置放回上传后的内容（复制改动的 part 列表，不改动上传返回的切片）；
// 上传改变了消息或 part 的数量、或那个位置已经不空时返回错误（不会发生，兜底不让图片错位）
func restoreInlineImages(contents []aistudio.Content, held []heldInlineImage) ([]aistudio.Content, error) {
	if len(held) == 0 {
		return contents, nil
	}
	result := slices.Clone(contents)
	copied := make(map[int]bool)
	for _, image := range held {
		if image.content >= len(result) || image.part >= len(result[image.content].Parts) {
			return nil, fmt.Errorf("内联附件上传后消息结构变了，无法放回 contents[%d] 的图片", image.content)
		}
		if !copied[image.content] {
			result[image.content].Parts = slices.Clone(result[image.content].Parts)
			copied[image.content] = true
		}
		part := &result[image.content].Parts[image.part]
		if part.InlineData != nil || part.File != nil {
			return nil, fmt.Errorf("内联附件上传后 contents[%d] 的图片位置已被占用", image.content)
		}
		part.InlineData = image.blob
	}
	return result, nil
}

// imageProgressNote 为写进请求进度的一行说明
type imageProgressNote struct {
	level string
	text  string
}

// interactionImageNotes 返回 Interaction 模型图片整理结果写进请求进度的说明：按官网内联发送的图片数、字节数与 GIF 转换情况，
// 以及无法转为 PNG、改走 Drive 的图片
func interactionImageNotes(report aistudio.InteractionImageReport) []imageProgressNote {
	var notes []imageProgressNote
	if report.Inline > 0 {
		text := fmt.Sprintf("Omni 图片按官网内联发送，不经 Drive | 图片=%d | 字节=%dB", report.Inline, report.InlineBytes)
		if report.Converted > 0 {
			text += fmt.Sprintf(" | GIF 转为 PNG=%d | GIF=%dB | PNG=%dB", report.Converted, report.SourceBytes, report.PNGBytes)
		}
		notes = append(notes, imageProgressNote{level: "INFO", text: text})
	}
	if report.DriveImages > 0 {
		notes = append(notes, imageProgressNote{level: "WARN", text: fmt.Sprintf(
			"Omni 图片无法内联，改走 Drive（需要有 Drive 授权的账号） | 张数=%d | 原因=%s", report.DriveImages, report.Reason)})
	}
	return notes
}
