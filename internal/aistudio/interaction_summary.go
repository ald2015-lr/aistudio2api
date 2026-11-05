package aistudio

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// Interaction 请求结构摘要：上游对 CreateInteractionStream 返回笼统的 400（Request contains an invalid argument.）时，管理日志里只有这一句，
// 看不出发出去的请求长什么样。每次发送前在该请求的管理日志记一行结构摘要（见 interactionEncoding.summary），与官网抓包逐项对照：
// 各 step 的角色与 part 种类、数量（按发送顺序，连续同类合并计数），图片格式与字节数，Drive 引用数，WAA binding 是否计入了图片
// （OMNI_IMAGE_BINDING，见 encodeInteractionSteps），视频设置与任务，是否多轮。
// 只写种类、数量、字节数与枚举，不写提示词正文、系统指令正文、图片数据与 Drive 文件 ID

// interactionPartRun 为一条 step 里连续同类的 part：kind 为种类（说明、图片、Drive、文本、其他），count 为连续的个数
type interactionPartRun struct {
	kind  string
	count int
}

// interactionStepParts 为一条 step 里按发送顺序排列的 part 种类，连续同类合并计数。按实际顺序记：官网抓包是图片在前、文本在后，
// new-api 从 OpenAI 格式转换时多半是文本在前，固定顺序的计数看不出两者的差别
type interactionStepParts []interactionPartRun

// add 追加一个 part 的种类：与上一段同类时累加
func (parts *interactionStepParts) add(kind string) {
	if last := len(*parts) - 1; last >= 0 && (*parts)[last].kind == kind {
		(*parts)[last].count++
		return
	}
	*parts = append(*parts, interactionPartRun{kind: kind, count: 1})
}

// describe 返回 step 的写法（按发送顺序，连续同类合并计数），如“用户[说明 1、图片 2、文本 1]”“用户[文本 1、图片 1]”
func (parts interactionStepParts) describe(model bool) string {
	role := "用户"
	if model {
		role = "模型"
	}
	kinds := make([]string, 0, len(parts))
	for _, run := range parts {
		kinds = append(kinds, fmt.Sprintf("%s %d", run.kind, run.count))
	}
	return role + "[" + strings.Join(kinds, "、") + "]"
}

// interactionImageTotal 为一种图片格式的张数与字节数
type interactionImageTotal struct {
	count int
	bytes int64
}

// base64DecodedLen 返回标准 Base64 字符串解码后的字节数（不解码）
func base64DecodedLen(encoded string) int64 {
	size := int64(len(encoded)) / 4 * 3
	switch {
	case strings.HasSuffix(encoded, "=="):
		size -= 2
	case strings.HasSuffix(encoded, "="):
		size--
	}
	return size
}

// summary 返回写进管理日志的请求结构摘要（见文件开头），按编码出的 input steps 统计，即发往上游的结构（相邻同角色已合并、模型历史媒体已移到
// 用户 step；每条 step 的 part 种类按发送顺序列出，连续同类合并计数）。例如
// Interaction 请求结构 | 方法=CreateInteractionStream | 模型=models/gemini-omni-1.1-flash | 多轮=否 | step=1（用户[图片 1、文本 1]） |
// 图片=1 张（PNG 1 张 1234B） | Drive 引用=0 | 校验值含图片=是（1 张） | 文本=1 段 12B | 系统指令=无 | 思考等级=4 | 输出上限=65536 |
// 视频设置=时长 10s | 视频格式=[null,null,null,null,[10],1] | 任务=未指定 | 请求体=2345B
func (encoded interactionEncoding) summary(request GenerateRequest) string {
	descriptions := make([]string, 0, len(encoded.steps))
	images := make(map[string]*interactionImageTotal)
	imageCount, driveCount, textCount := 0, 0, 0
	var textBytes int64
	for _, raw := range encoded.steps {
		step, _ := raw.([]any)
		model := len(step) > 1 && step[0] == nil
		var content []any
		if model {
			wrapper, _ := step[1].([]any)
			if len(wrapper) > 0 {
				content, _ = wrapper[0].([]any)
			}
		} else if len(step) > 0 {
			wrapper, _ := step[0].([]any)
			if len(wrapper) > 0 {
				content, _ = wrapper[0].([]any)
			}
		}
		var counts interactionStepParts
		for _, item := range content {
			part, _ := item.([]any)
			switch {
			case len(part) == 1:
				text, _ := part[0].([]any)
				value := ""
				if len(text) > 0 {
					value, _ = text[0].(string)
				}
				if value == interactionModelMediaIntro {
					counts.add("说明")
				} else {
					counts.add("文本")
					textCount++
					textBytes += int64(len(value))
				}
			case len(part) >= 2 && part[1] != nil:
				counts.add("图片")
				imageCount++
				media, _ := part[1].([]any)
				name := "未知"
				var size int64
				if len(media) >= 2 {
					if enum, ok := media[0].(int64); ok {
						name = interactionImageEnumName(enum)
					}
					if data, ok := media[1].(string); ok {
						size = base64DecodedLen(data)
					}
				}
				total := images[name]
				if total == nil {
					total = &interactionImageTotal{}
					images[name] = total
				}
				total.count++
				total.bytes += size
			case len(part) >= 9 && part[8] != nil:
				counts.add("Drive")
				driveCount++
			default:
				counts.add("其他")
			}
		}
		descriptions = append(descriptions, counts.describe(model))
	}
	multiTurn := "否"
	if encoded.hasModelTurn {
		multiTurn = "是"
	}
	imageText := "0"
	if imageCount > 0 {
		names := make([]string, 0, len(images))
		for name := range images {
			names = append(names, name)
		}
		slices.Sort(names)
		totals := make([]string, 0, len(names))
		for _, name := range names {
			totals = append(totals, fmt.Sprintf("%s %d 张 %dB", name, images[name].count, images[name].bytes))
		}
		imageText = fmt.Sprintf("%d 张（%s）", imageCount, strings.Join(totals, "、"))
	}
	system := "无"
	if strings.TrimSpace(request.System) != "" {
		system = "有"
	}
	videoFormat := "无（编辑任务不带输出格式）"
	if encoded.video != nil {
		if formatted, err := json.Marshal(encoded.video); err == nil {
			videoFormat = string(formatted)
		}
	}
	task := "未指定"
	for name, value := range interactionVideoTasks {
		if value == encoded.task && encoded.task != 0 {
			task = fmt.Sprintf("%s（%d）", name, value)
		}
	}
	return fmt.Sprintf("Interaction 请求结构 | 方法=CreateInteractionStream | 模型=%s | 多轮=%s | step=%d（%s） | 图片=%s | Drive 引用=%d | "+
		"校验值含图片=%s | 文本=%d 段 %dB | 系统指令=%s | 思考等级=%d | 输出上限=%d | 视频设置=%s | 视频格式=%s | 任务=%s | 请求体=%dB",
		wireModelName(request.Model), multiTurn, len(encoded.steps), strings.Join(descriptions, " "), imageText, driveCount,
		encoded.bindingImageText(), textCount, textBytes, system, encoded.level, encoded.maxOutput, videoSettingsText(request.Config.VideoConfig),
		videoFormat, task, len(encoded.body))
}

// bindingImageText 为请求结构摘要里 binding 是否计入了图片：OMNI_IMAGE_BINDING 关闭时为“关闭”（binding 只含文本，与 a574e64 相同），
// 开启时计入了图片为“是（N 张）”，请求体里没有内联图片为“否”
func (encoded interactionEncoding) bindingImageText() string {
	switch {
	case !encoded.imageBinding:
		return "关闭"
	case encoded.bindingImages > 0:
		return fmt.Sprintf("是（%d 张）", encoded.bindingImages)
	default:
		return "否"
	}
}

// videoSettingsText 返回请求里的视频设置（宽高比、时长、分辨率、帧率与任务，按客户端写的取值）；都没有设置时为“未设置”
func videoSettingsText(config *VideoConfig) string {
	if config == nil {
		return "未设置"
	}
	var fields []string
	if value := strings.TrimSpace(config.AspectRatio); value != "" {
		fields = append(fields, "宽高比 "+value)
	}
	if config.DurationSeconds > 0 {
		fields = append(fields, fmt.Sprintf("时长 %ds", config.DurationSeconds))
	}
	if value := strings.TrimSpace(config.Resolution); value != "" {
		fields = append(fields, "分辨率 "+value)
	}
	if config.FrameRate > 0 {
		fields = append(fields, fmt.Sprintf("帧率 %gfps", config.FrameRate))
	}
	if value := strings.TrimSpace(config.Task); value != "" {
		fields = append(fields, "任务 "+value)
	}
	if len(fields) == 0 {
		return "未设置"
	}
	return strings.Join(fields, "、")
}
