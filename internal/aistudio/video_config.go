package aistudio

import (
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"
)

// interactionVideoAspectRatios 是 CreateInteractionStream 视频输出格式（interaction field 54）的宽高比枚举
var interactionVideoAspectRatios = map[string]int64{"16:9": 1, "9:16": 2}

// interactionVideoResolutions 是视频输出格式的分辨率枚举
var interactionVideoResolutions = map[string]int64{"360p": 1, "720p": 2, "1080p": 3, "4k": 4}

// interactionVideoTasks 是 GenerationConfig field 25 的视频任务枚举，名称取 Interactions API video_config.task
var interactionVideoTasks = map[string]int64{"text_to_video": 1, "image_to_video": 2, "reference_to_video": 3, "edit": 4, "extend": 5}

// interactionDefaultVideoResolution 为模型目录没有列出分辨率时发送的分辨率枚举（官网默认 360p）
const interactionDefaultVideoResolution int64 = 1

// defaultVideoFrameRate 为模型目录没有列出帧率时可用的帧率：官网只有 24fps，且界面上不可选
const defaultVideoFrameRate = 24

// videoTaskFirstTurnMessage 与官网 Video task 的提示一致：有模型轮次的对话不能指定视频任务
const videoTaskFirstTurnMessage = "Video task is only supported on the first turn of a conversation"

// VideoDurationRange 为模型目录的视频时长滑块（video_duration_range）：下限、上限、步长与默认值，单位秒，0 表示目录没有给出
type VideoDurationRange struct {
	Min     int64
	Max     int64
	Step    int64
	Default int64
}

// OmniVideoModel 判断模型是经 CreateInteractionStream 生成视频的 Omni 模型：目录带 Interaction 配置（field 79），
// 且列出视频输出分辨率、时长范围或视频任务。视频设置、视频任务与模型名的视频后缀只对这类模型生效
func OmniVideoModel(model Model) bool {
	if !model.Capabilities["interactions_api"] {
		return false
	}
	options := model.CapabilityOptions
	return len(options["video_output_resolutions"]) > 0 || len(options["video_duration_range"]) > 0 || len(options["video_tasks"]) > 0
}

// ModelVideoDurationRange 读取模型目录的视频时长滑块。多个账户的目录合并后（取值按字符串排序去重，不保留上游顺序）同名项可能出现多次，
// 按数值取各账户都能接受的范围：下限取最大值、上限取最小值、步长取最大值，默认值取落在范围内的最大一个（都不在范围内时换成范围内最接近的）。
// 这样协议层按合并目录调整的时长与留下的说明对每个账户都成立；范围为空（下限大于上限）时 ok 为 false
func ModelVideoDurationRange(model Model) (VideoDurationRange, bool) {
	var bounds VideoDurationRange
	var defaults []int64
	for _, item := range model.CapabilityOptions["video_duration_range"] {
		name, value, ok := strings.Cut(item, "=")
		seconds, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
		if !ok || err != nil || seconds < 0 {
			continue
		}
		switch strings.TrimSpace(name) {
		case "min":
			bounds.Min = max(bounds.Min, seconds)
		case "max":
			// 0 表示目录没有给出上限
			if seconds > 0 && (bounds.Max == 0 || seconds < bounds.Max) {
				bounds.Max = seconds
			}
		case "step":
			bounds.Step = max(bounds.Step, seconds)
		case "default":
			defaults = append(defaults, seconds)
		}
	}
	if bounds.Max > 0 && bounds.Min > bounds.Max {
		return VideoDurationRange{}, false
	}
	slices.Sort(defaults)
	for index := len(defaults) - 1; index >= 0; index-- {
		if defaults[index] >= bounds.Min && (bounds.Max == 0 || defaults[index] <= bounds.Max) {
			bounds.Default = defaults[index]
			break
		}
	}
	if bounds.Default == 0 && len(defaults) > 0 && defaults[len(defaults)-1] > 0 {
		bounds.Default = bounds.fit(defaults[len(defaults)-1])
	}
	return bounds, bounds.Min > 0 || bounds.Max > 0
}

// fit 把时长换成范围内、落在步长上的最接近的秒数（步长从下限算起，距离相同时取较大值）
func (bounds VideoDurationRange) fit(seconds int64) int64 {
	if bounds.Min > 0 && seconds < bounds.Min {
		seconds = bounds.Min
	}
	if bounds.Max > 0 && seconds > bounds.Max {
		seconds = bounds.Max
	}
	if bounds.Step > 1 {
		offset := float64(seconds-bounds.Min) / float64(bounds.Step)
		seconds = bounds.Min + int64(math.Floor(offset+0.5))*bounds.Step
		if bounds.Max > 0 && seconds > bounds.Max {
			seconds -= bounds.Step
		}
	}
	return seconds
}

// describe 返回写进请求日志的范围说明，例如 4–15 秒、步长 1
func (bounds VideoDurationRange) describe() string {
	text := fmt.Sprintf("%d–%d 秒", bounds.Min, bounds.Max)
	if bounds.Max == 0 {
		text = fmt.Sprintf("不少于 %d 秒", bounds.Min)
	}
	if bounds.Step > 0 {
		text += fmt.Sprintf("、步长 %d", bounds.Step)
	}
	return text
}

// NormalizeVideoConfig 按模型目录整理 Omni 视频设置，返回整理后的设置与要写进请求日志的说明：
//   - 宽高比为空、auto 或 ASPECT_RATIO_UNSPECIFIED 时不指定（官网 Auto）；其他取值换成模型选项中比值最接近的 16:9 或 9:16，无法解析时报错
//   - 分辨率换成模型选项中像素高度最接近的 360p、720p、1080p 或 4k，无法解析时报错；为空、auto 或 VIDEO_RESOLUTION_UNSPECIFIED 时
//     由编码取模型默认分辨率
//   - 时长超出模型时长范围或不在步长上时，换成范围内最接近的合法秒数并记一条说明；负数报错
//   - 帧率只校验：不在模型帧率（目录没有给出时为 24fps）中时报错
//   - 视频任务必须是已知任务（unspecified、auto 与 VIDEO_TASK_ 前缀的写法同样接受）且在模型 video_tasks 中，只能在第一轮
//     （对话中没有模型轮次）使用，图片与视频任务需要对应的输入；edit 任务同时设置了宽高比、时长或分辨率时记一条说明（编辑请求不带输出格式）
//
// 错误信息为英文，按参数错误原样返回给客户端
func NormalizeVideoConfig(config *VideoConfig, model Model, contents []Content) (*VideoConfig, []string, error) {
	return normalizeVideoConfig(config, model, contents, true)
}

// normalizeVideoConfig 与 NormalizeVideoConfig 相同；checkTasks 为 false 时不按模型 video_tasks 拒绝任务：
// 发送前按所选账户的目录再整理时使用，任务是否可用以协议层的合并目录为准（单个账户的目录没有列出时交给上游判断，免得只能换号的请求直接 400）
func normalizeVideoConfig(config *VideoConfig, model Model, contents []Content, checkTasks bool) (*VideoConfig, []string, error) {
	if config == nil {
		return nil, nil, nil
	}
	normalized := *config
	options := model.CapabilityOptions
	var notes []string
	if ratio := videoAspectRatioValue(normalized.AspectRatio); ratio == "" {
		normalized.AspectRatio = ""
	} else {
		value, ok := AspectRatioLog(ratio)
		if !ok {
			return nil, nil, fmt.Errorf("video aspect_ratio %q is not valid; use auto, 16:9 or 9:16", config.AspectRatio)
		}
		nearest := NearestOption(value, videoWireChoices(options["video_aspect_ratios"], interactionVideoAspectRatios, AspectRatioLog), AspectRatioLog)
		if nearest != ratio {
			notes = append(notes, fmt.Sprintf("视频宽高比 %s 不是模型选项，按最接近的 %s 发送", strings.TrimSpace(config.AspectRatio), nearest))
		}
		normalized.AspectRatio = nearest
	}
	if resolution := videoResolutionValue(normalized.Resolution); resolution == "" {
		normalized.Resolution = ""
	} else {
		height, ok := videoResolutionHeight(resolution)
		if !ok {
			return nil, nil, fmt.Errorf("video resolution %q is not valid; use 360p, 720p, 1080p or 4k", config.Resolution)
		}
		nearest := NearestOption(height, videoWireChoices(options["video_output_resolutions"], interactionVideoResolutions, videoResolutionMeasure), videoResolutionHeight)
		// 目录列出的 368p 编码为上游的 360p：请求 368p 时只是换成上游名称，不算换成别的分辨率，不记说明
		if nearest != resolution && !videoOptionListed(options["video_output_resolutions"], resolution, videoResolutionValue) {
			notes = append(notes, fmt.Sprintf("视频分辨率 %s 不是模型选项，按最接近的 %s 发送", strings.TrimSpace(config.Resolution), nearest))
		}
		normalized.Resolution = nearest
	}
	if normalized.DurationSeconds < 0 {
		return nil, nil, errors.New("video duration must not be negative")
	}
	if normalized.DurationSeconds > 0 {
		if bounds, ok := ModelVideoDurationRange(model); ok {
			if fitted := bounds.fit(normalized.DurationSeconds); fitted != normalized.DurationSeconds {
				notes = append(notes, fmt.Sprintf("视频时长 %d 秒不是模型可选的时长（%s），按最接近的 %d 秒发送", normalized.DurationSeconds, bounds.describe(), fitted))
				normalized.DurationSeconds = fitted
			}
		} else if listed := options["video_durations_seconds"]; len(listed) > 0 {
			nearest := NearestOption(float64(normalized.DurationSeconds), listed, videoDurationOption)
			if fitted, err := strconv.ParseInt(nearest, 10, 64); err == nil && fitted != normalized.DurationSeconds {
				notes = append(notes, fmt.Sprintf("视频时长 %d 秒不是模型可选的时长（%s 秒），按最接近的 %d 秒发送", normalized.DurationSeconds, strings.Join(listed, "、"), fitted))
				normalized.DurationSeconds = fitted
			}
		}
	}
	if err := checkVideoFrameRate(normalized.FrameRate, options["video_frame_rates"]); err != nil {
		return nil, nil, err
	}
	task := videoTaskValue(normalized.Task)
	normalized.Task = task
	if task != "" {
		if _, ok := interactionVideoTasks[task]; !ok {
			return nil, nil, fmt.Errorf("video task %q is not valid; use text_to_video, image_to_video, reference_to_video, edit or extend", config.Task)
		}
		if listed := options["video_tasks"]; checkTasks && len(listed) > 0 && !slices.Contains(listed, task) {
			return nil, nil, fmt.Errorf("model %s does not support video task %s; supported tasks: %s", model.ID, task, strings.Join(listed, ", "))
		}
		if interactionHasModelTurn(contents) {
			return nil, nil, errors.New(videoTaskFirstTurnMessage)
		}
		if err := checkVideoTaskInputs(task, contents); err != nil {
			return nil, nil, err
		}
		// 编辑任务的请求不带输出格式（与官网一致），宽高比、时长与分辨率不发送
		if task == "edit" && (normalized.AspectRatio != "" || normalized.DurationSeconds > 0 || normalized.Resolution != "") {
			notes = append(notes, "视频任务 edit 不带输出格式，宽高比、时长与分辨率不发送")
		}
	}
	return &normalized, notes, nil
}

// videoWireChoices 把模型目录的选项换成 CreateInteractionStream 能编码的取值：每项按 measure 换成最接近的上游取值再去重
// （与编码默认分辨率的规则相同，目录的 368p 换成 360p）；目录没有列出（或都无法度量）时为全部可编码的取值
func videoWireChoices(listed []string, wire map[string]int64, measure func(string) (float64, bool)) []string {
	all := slices.Sorted(maps.Keys(wire))
	choices := make([]string, 0, len(listed))
	for _, option := range listed {
		value, ok := measure(option)
		if !ok {
			continue
		}
		if nearest := NearestOption(value, all, measure); nearest != "" && !slices.Contains(choices, nearest) {
			choices = append(choices, nearest)
		}
	}
	if len(choices) == 0 {
		choices = all
	}
	return choices
}

// videoOptionListed 判断目录选项里有没有与 value 写法相同的一项（按 normalize 统一写法后比较）
func videoOptionListed(listed []string, value string, normalize func(string) string) bool {
	for _, option := range listed {
		if normalize(option) == value {
			return true
		}
	}
	return false
}

// videoAspectRatioValue 统一宽高比写法（小写，16x9 写成 16:9）；空串、auto 与 ASPECT_RATIO_UNSPECIFIED（官网 Auto）返回空串
func videoAspectRatioValue(value string) string {
	ratio := strings.ToLower(strings.TrimSpace(value))
	switch ratio {
	case "", "auto", "unspecified", "aspect_ratio_unspecified":
		return ""
	}
	return strings.ReplaceAll(ratio, "x", ":")
}

// videoResolutionValue 统一分辨率写法（见 normalizeVideoResolutionName）；空串、auto 与 VIDEO_RESOLUTION_UNSPECIFIED 返回空串，即未设置
func videoResolutionValue(value string) string {
	resolution := normalizeVideoResolutionName(value)
	if resolution == "auto" || resolution == "unspecified" {
		return ""
	}
	return resolution
}

// videoResolutionMeasure 返回任意写法（720P、VIDEO_RESOLUTION_720P 等）的分辨率的像素高度
func videoResolutionMeasure(value string) (float64, bool) {
	return videoResolutionHeight(normalizeVideoResolutionName(value))
}

// videoTaskValue 统一视频任务写法（小写，去掉 VIDEO_TASK_ 前缀）；空串、unspecified 与 auto（官网 Unspecified）返回空串
func videoTaskValue(value string) string {
	task := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(value)), "video_task_")
	if task == "unspecified" || task == "auto" {
		return ""
	}
	return task
}

// normalizeVideoResolutionName 把 720P、VIDEO_RESOLUTION_720P、4K 等写法统一为小写的 720p、4k
func normalizeVideoResolutionName(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	value = strings.TrimPrefix(value, "video_resolution_")
	value = strings.TrimPrefix(value, "resolution_")
	return value
}

// checkVideoFrameRate 校验客户端要求的帧率：0 为未设置；模型目录列出帧率（如 24fps）时只接受其中的取值，没有列出时只接受 24
func checkVideoFrameRate(frameRate float64, listed []string) error {
	if frameRate == 0 {
		return nil
	}
	allowed := make([]float64, 0, len(listed))
	names := make([]string, 0, len(listed))
	for _, option := range listed {
		value, err := strconv.ParseFloat(strings.TrimSuffix(strings.ToLower(strings.TrimSpace(option)), "fps"), 64)
		if err == nil && value > 0 {
			allowed = append(allowed, value)
			names = append(names, option)
		}
	}
	if len(allowed) == 0 {
		allowed, names = []float64{defaultVideoFrameRate}, []string{"24fps"}
	}
	if slices.Contains(allowed, frameRate) {
		return nil
	}
	return fmt.Errorf("video frame_rate %s is not supported; this model only supports %s", strconv.FormatFloat(frameRate, 'f', -1, 64), strings.Join(names, ", "))
}

// InteractionHasModelTurn 判断对话中是否有模型轮次（规则同 interactionHasModelTurn）：有时不能指定视频任务。
// 协议层据此在多轮对话里忽略模型名后缀指定的视频任务（见 api.applyOmniVideoAlias）
func InteractionHasModelTurn(contents []Content) bool {
	return interactionHasModelTurn(contents)
}

// VideoTaskName 返回统一写法的视频任务名（小写，去掉 VIDEO_TASK_ 前缀）；空串、unspecified 与 auto（官网 Unspecified）返回空串
func VideoTaskName(value string) string {
	return videoTaskValue(value)
}

// interactionHasModelTurn 判断对话中是否有模型轮次：会编码为模型 step 的模型消息，与 encodeInteractionSteps 的规则一致，只有附件
// （移到随后的用户 step）或只有思考的模型消息不算；从本服务媒体链接恢复出来、带链接文本的媒体（Part.RestoredLink，RestoredText 非空）
// 按客户端回传的链接文本算，与没能恢复、换回链接文本（app 层没有可用的 Drive 授权账号时）的结果相同。上一轮回复只有视频链接时
// （Chat Completions 与 Messages 的 url 模式输出 "[video](…)"，不带前后换行），恢复后只剩视频，原先判为没有模型轮次：后缀任务不被忽略、
// 显式任务也不返回 400，换回链接文本后生成层又按多轮返回 400，结果随能否恢复与回退变化
func interactionHasModelTurn(contents []Content) bool {
	for _, content := range contents {
		if content.Role != RoleAssistant {
			continue
		}
		for _, part := range content.Parts {
			switch {
			case part.RestoredLink && part.RestoredText != "":
				return true
			case part.Thought || part.Text == "" && part.ThoughtSignature != "" && part.InlineData == nil && part.File == nil:
			case part.InlineData != nil || part.File != nil || part.ExternalMedia != nil:
			default:
				return true
			}
		}
	}
	return false
}

// checkVideoTaskInputs 校验视频任务需要的输入：image_to_video、reference_to_video 需要至少一张图片，edit、extend 需要至少一段视频。
// 模型消息里的附件会移到随后的用户 step，同样计入；类型不明的文件引用两种都算
func checkVideoTaskInputs(task string, contents []Content) error {
	want := ""
	switch task {
	case "image_to_video", "reference_to_video":
		want = "image/"
	case "edit", "extend":
		want = "video/"
	default:
		return nil
	}
	for _, content := range contents {
		for _, part := range content.Parts {
			mime, ok := partMediaMIME(part)
			if ok && (mime == "" || strings.HasPrefix(strings.ToLower(mime), want)) {
				return nil
			}
		}
	}
	if want == "image/" {
		return fmt.Errorf("video task %s requires an input image", task)
	}
	return fmt.Errorf("video task %s requires an input video", task)
}

// partMediaMIME 返回附件 part 的 MIME，不是附件时 ok 为 false
func partMediaMIME(part Part) (string, bool) {
	switch {
	case part.InlineData != nil:
		return part.InlineData.MIME, true
	case part.File != nil:
		return part.File.MIME, true
	case part.ExternalMedia != nil:
		return part.ExternalMedia.MIME, true
	}
	return "", false
}

// interactionVideoFormat 编码视频输出格式并返回任务枚举：格式依次为 field 4 宽高比、field 5 时长 [秒]、field 6 分辨率。
// 宽高比与分辨率取最接近的上游取值（调用方一般已按模型目录整理过），宽高比未设置（含 auto 与 ASPECT_RATIO_UNSPECIFIED）时不带；
// 分辨率未设置时取模型默认分辨率，目录也没有时为官网默认的 360p；时长未设置时不带（与上游相同，由上游按模型默认）
func interactionVideoFormat(config *VideoConfig, defaultResolution string) ([]any, int64, error) {
	format := []any{nil, nil, nil, nil, nil, interactionDefaultVideoResolution}
	if value, ok := nearestVideoResolution(defaultResolution); ok {
		format[5] = value
	}
	if config == nil {
		return format, 0, nil
	}
	if ratio := videoAspectRatioValue(config.AspectRatio); ratio != "" {
		value, ok := AspectRatioLog(ratio)
		if !ok {
			return nil, 0, fmt.Errorf("video aspect_ratio %q is not valid; use auto, 16:9 or 9:16", config.AspectRatio)
		}
		format[3] = interactionVideoAspectRatios[NearestOption(value, slices.Sorted(maps.Keys(interactionVideoAspectRatios)), AspectRatioLog)]
	}
	if config.DurationSeconds < 0 {
		return nil, 0, errors.New("video duration must not be negative")
	}
	if config.DurationSeconds > 0 {
		format[4] = []any{config.DurationSeconds}
	}
	if resolution := videoResolutionValue(config.Resolution); resolution != "" {
		value, ok := nearestVideoResolution(resolution)
		if !ok {
			return nil, 0, fmt.Errorf("video resolution %q is not valid; use 360p, 720p, 1080p or 4k", config.Resolution)
		}
		format[5] = value
	}
	task := videoTaskValue(config.Task)
	if task == "" {
		return format, 0, nil
	}
	value, ok := interactionVideoTasks[task]
	if !ok {
		return nil, 0, fmt.Errorf("video task %q is not valid; use text_to_video, image_to_video, reference_to_video, edit or extend", config.Task)
	}
	return format, value, nil
}

// nearestVideoResolution 返回像素高度最接近的分辨率枚举（368p 换成 360p），无法解析时 ok 为 false
func nearestVideoResolution(value string) (int64, bool) {
	height, ok := videoResolutionHeight(normalizeVideoResolutionName(value))
	if !ok {
		return 0, false
	}
	return interactionVideoResolutions[NearestOption(height, slices.Sorted(maps.Keys(interactionVideoResolutions)), videoResolutionHeight)], true
}

// NearestOption 返回度量值与目标最接近的选项，距离相同时取度量值较大者；没有可度量的选项时返回空串
func NearestOption(target float64, options []string, measure func(string) (float64, bool)) string {
	nearest := ""
	nearestDistance, nearestValue := math.Inf(1), math.Inf(-1)
	for _, option := range options {
		value, ok := measure(option)
		if !ok {
			continue
		}
		distance := math.Abs(value - target)
		if distance < nearestDistance || distance == nearestDistance && value > nearestValue {
			nearest, nearestDistance, nearestValue = option, distance, value
		}
	}
	return nearest
}

// AspectRatioLog 返回 W:H 宽高比的自然对数，按比值比较 16:9 与 4:3 等取值的远近
func AspectRatioLog(value string) (float64, bool) {
	widthText, heightText, ok := strings.Cut(strings.TrimSpace(value), ":")
	width, widthErr := strconv.ParseFloat(strings.TrimSpace(widthText), 64)
	height, heightErr := strconv.ParseFloat(strings.TrimSpace(heightText), 64)
	if !ok || widthErr != nil || heightErr != nil || !(width > 0) || !(height > 0) || math.IsInf(width, 0) || math.IsInf(height, 0) {
		return 0, false
	}
	return math.Log(width) - math.Log(height), true
}

// videoDurationOption 解析视频时长选项（video_durations_seconds）的秒数
func videoDurationOption(value string) (float64, bool) {
	seconds, err := strconv.Atoi(strings.TrimSpace(value))
	return float64(seconds), err == nil
}

// videoResolutionHeight 返回 720p、4k 等视频分辨率的像素高度（Nk 按 N×540 计，4k 为 2160）
func videoResolutionHeight(value string) (float64, bool) {
	value = strings.ToLower(strings.TrimSpace(value))
	scale := 0
	switch {
	case strings.HasSuffix(value, "p"):
		scale = 1
	case strings.HasSuffix(value, "k"):
		scale = 540
	}
	number, err := strconv.Atoi(value[:max(len(value)-1, 0)])
	if scale == 0 || err != nil || number <= 0 || number > 100000 {
		return 0, false
	}
	return float64(number * scale), true
}
