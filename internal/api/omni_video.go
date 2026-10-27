package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// Omni 视频设置（官网 Run settings 的 Aspect ratio、Video duration、Resolution、Frame rate 与 Advanced 的 Video task）在各协议中的写法：
//
//	Gemini         generationConfig.responseFormat[].video {aspectRatio, duration, resolution, frameRate（也接受 fps）}
//	               generationConfig.videoConfig {task, aspectRatio, durationSeconds, resolution, frameRate（也接受 fps）}（同一项以后者为准）
//	OpenAI Chat、Responses、Anthropic Messages
//	               顶层 video_config {aspect_ratio, duration_seconds（也接受 duration）, resolution, frame_rate（也接受 fps）, task}
//	Interactions   response_format {type:"video", aspect_ratio, duration, resolution, frame_rate（也接受 fps）}
//	               generation_config.video_config {task, aspect_ratio, duration_seconds, resolution, frame_rate}（同一项以后者为准）
//	模型名后缀     -16x9、-9x16、-360p、-720p、-1080p、-4k、-<N>s、-t2v、-i2v、-r2v、-edit、-extend，优先于以上参数
//
// 只对能输出视频的 Omni 模型生效（见 aistudio.OmniVideoModel），其他模型忽略这些字段；模型名后缀对目录 ID 与 Omni 的目录别名都生效

// videoSeconds 为视频时长：JSON 数字或 "8s"、"8" 形式的字符串，四舍五入为整秒；null 与空字符串为未设置
type videoSeconds struct {
	seconds int64
	set     bool
}

// UnmarshalJSON 读取数字或字符串写法的时长，负数与无法解析的取值返回参数错误
func (value *videoSeconds) UnmarshalJSON(data []byte) error {
	text, err := jsonScalarText(data)
	if err != nil {
		return fmt.Errorf("video duration must be a number of seconds such as 8 or \"8s\"")
	}
	seconds, set, err := parseVideoSeconds(text)
	if err != nil {
		return err
	}
	*value = videoSeconds{seconds: seconds, set: set}
	return nil
}

// parseVideoSeconds 把 "8s"、"8"、"7.6" 形式的时长四舍五入为整秒，空串为未设置
func parseVideoSeconds(text string) (int64, bool, error) {
	text = strings.ToLower(strings.TrimSpace(text))
	if text == "" {
		return 0, false, nil
	}
	number, err := strconv.ParseFloat(strings.TrimSpace(strings.TrimSuffix(text, "s")), 64)
	if err != nil || math.IsNaN(number) || math.IsInf(number, 0) || number < 0 || number > 86400 {
		return 0, false, fmt.Errorf("video duration %q must be a non-negative number of seconds such as 8 or \"8s\"", text)
	}
	return int64(math.Round(number)), true, nil
}

// videoFrameRate 为视频帧率：JSON 数字或 "24"、"24fps" 形式的字符串；null 与空字符串为未设置
type videoFrameRate struct {
	value float64
}

// UnmarshalJSON 读取数字或字符串写法的帧率，是否支持在按模型目录整理时校验（官网只有 24fps）
func (rate *videoFrameRate) UnmarshalJSON(data []byte) error {
	text, err := jsonScalarText(data)
	if err != nil {
		return fmt.Errorf("video frame_rate must be a number of frames per second such as 24")
	}
	text = strings.TrimSpace(strings.TrimSuffix(strings.ToLower(strings.TrimSpace(text)), "fps"))
	if text == "" {
		*rate = videoFrameRate{}
		return nil
	}
	value, parseErr := strconv.ParseFloat(text, 64)
	if parseErr != nil || math.IsNaN(value) || math.IsInf(value, 0) || value <= 0 {
		return fmt.Errorf("video frame_rate %q must be a number of frames per second such as 24", text)
	}
	*rate = videoFrameRate{value: value}
	return nil
}

// jsonScalarText 返回 JSON 字符串的内容或数字的原文，null 为空串，其他类型返回错误
func jsonScalarText(data []byte) (string, error) {
	trimmed := bytes.TrimSpace(data)
	switch {
	case len(trimmed) == 0 || string(trimmed) == "null":
		return "", nil
	case trimmed[0] == '"':
		var text string
		err := json.Unmarshal(trimmed, &text)
		return text, err
	case trimmed[0] == '-' || trimmed[0] >= '0' && trimmed[0] <= '9':
		var number json.Number
		err := json.Unmarshal(trimmed, &number)
		return number.String(), err
	}
	return "", fmt.Errorf("expected a number or string")
}

// videoConfigRequest 为 snake_case 写法的 video_config：OpenAI Chat Completions、Responses 与 Anthropic Messages 的顶层字段
// （OpenAI SDK 的 extra_body 会合并到顶层），以及 Interactions 的 generation_config.video_config
type videoConfigRequest struct {
	AspectRatio     string         `json:"aspect_ratio"`
	DurationSeconds videoSeconds   `json:"duration_seconds"`
	Duration        videoSeconds   `json:"duration"`
	Resolution      string         `json:"resolution"`
	FrameRate       videoFrameRate `json:"frame_rate"`
	FPS             videoFrameRate `json:"fps"`
	Task            string         `json:"task"`
}

// videoConfig 返回规范视频设置，全部未设置时为 nil；duration_seconds 与 duration 同时出现时以 duration_seconds 为准
func (request *videoConfigRequest) videoConfig() *aistudio.VideoConfig {
	if request == nil {
		return nil
	}
	seconds := request.DurationSeconds
	if !seconds.set {
		seconds = request.Duration
	}
	return compactVideoConfig(aistudio.VideoConfig{
		AspectRatio: request.AspectRatio, DurationSeconds: seconds.seconds, Resolution: request.Resolution,
		FrameRate: firstFrameRate(request.FrameRate, request.FPS), Task: request.Task,
	})
}

// firstFrameRate 返回 frame_rate（frameRate）写法的帧率，没有设置时取 fps 写法的
func firstFrameRate(frameRate videoFrameRate, fps videoFrameRate) float64 {
	if frameRate.value != 0 {
		return frameRate.value
	}
	return fps.value
}

// compactVideoConfig 去掉首尾空白，全部未设置时返回 nil
func compactVideoConfig(config aistudio.VideoConfig) *aistudio.VideoConfig {
	config.AspectRatio = strings.TrimSpace(config.AspectRatio)
	config.Resolution = strings.TrimSpace(config.Resolution)
	config.Task = strings.TrimSpace(config.Task)
	if config == (aistudio.VideoConfig{}) {
		return nil
	}
	return &config
}

// overlayVideoConfig 返回 base 被 override 中设置了的项覆盖后的视频设置，两者都没有设置任何项时为 nil
func overlayVideoConfig(base *aistudio.VideoConfig, override *aistudio.VideoConfig) *aistudio.VideoConfig {
	var merged aistudio.VideoConfig
	if base != nil {
		merged = *base
	}
	if override != nil {
		if override.AspectRatio != "" {
			merged.AspectRatio = override.AspectRatio
		}
		if override.DurationSeconds != 0 {
			merged.DurationSeconds = override.DurationSeconds
		}
		if override.Resolution != "" {
			merged.Resolution = override.Resolution
		}
		if override.FrameRate != 0 {
			merged.FrameRate = override.FrameRate
		}
		if override.Task != "" {
			merged.Task = override.Task
		}
	}
	return compactVideoConfig(merged)
}

// videoTaskSuffixes 为模型名的视频任务后缀
var videoTaskSuffixes = map[string]string{
	"t2v": "text_to_video", "i2v": "image_to_video", "r2v": "reference_to_video", "edit": "edit", "extend": "extend",
}

// videoAspectSuffixes 为模型名的宽高比后缀
var videoAspectSuffixes = map[string]string{"16x9": "16:9", "9x16": "9:16"}

// videoResolutionSuffixes 为模型名的分辨率后缀
var videoResolutionSuffixes = map[string]string{"360p": "360p", "720p": "720p", "1080p": "1080p", "4k": "4k"}

// stripVideoSuffix 从 alias.base 末尾剥离一个 Omni 视频后缀（-16x9、-9x16、-360p、-720p、-1080p、-4k、-<N>s、-t2v、-i2v、-r2v、
// -edit、-extend，不分大小写）并记到 alias.video；同一项出现多次时以最靠后的为准（先剥离的先记）。没有可剥离的后缀时返回 false
func stripVideoSuffix(alias *modelAlias) bool {
	index := strings.LastIndex(alias.base, "-")
	if index <= 0 {
		return false
	}
	token := strings.ToLower(alias.base[index+1:])
	video := &alias.video
	switch {
	case videoAspectSuffixes[token] != "":
		if video.AspectRatio == "" {
			video.AspectRatio = videoAspectSuffixes[token]
		}
	case videoResolutionSuffixes[token] != "":
		if video.Resolution == "" {
			video.Resolution = videoResolutionSuffixes[token]
		}
	case videoTaskSuffixes[token] != "":
		if video.Task == "" {
			video.Task = videoTaskSuffixes[token]
		}
	case videoDurationSuffix(token) > 0:
		if video.DurationSeconds == 0 {
			video.DurationSeconds = videoDurationSuffix(token)
		}
	default:
		return false
	}
	alias.base = alias.base[:index]
	return true
}

// videoDurationSuffix 解析 8s、10s 形式的时长后缀（1–3 位数字加 s），不是这种形式时返回 0
func videoDurationSuffix(token string) int64 {
	digits, ok := strings.CutSuffix(token, "s")
	if !ok || digits == "" || len(digits) > 3 {
		return 0
	}
	for _, char := range digits {
		if char < '0' || char > '9' {
			return 0
		}
	}
	seconds, err := strconv.ParseInt(digits, 10, 64)
	if err != nil {
		return 0
	}
	return seconds
}

// applyOmniVideoAlias 处理能输出视频的 Omni 模型：换成去掉后缀的模型名（目录 ID，或按目录别名调用时的别名，调度前再换成目录 ID）；
// 名称里的思考等级后缀优先于请求参数（不带时沿用模型默认等级，不像文本模型那样强制最高）；视频后缀优先于请求里的视频设置，
// 再按模型目录整理（见 aistudio.NormalizeVideoConfig），说明写进请求日志；没有确认 CreateInteractionStream 的停止序列字段，
// stop sequences 清空并记一行请求日志（见 aistudio.InteractionStopSequencesNote）。返回是否隐藏思维链，视频设置不成立时返回参数错误
func applyOmniVideoAlias(request *aistudio.GenerateRequest, model aistudio.Model, alias modelAlias) (bool, error) {
	request.Model = alias.base
	if alias.level != "" {
		request.Config.ReasoningEffort = alias.level
		request.Config.ThinkingBudget = nil
	}
	if count := len(request.Config.StopSequences); count > 0 {
		request.Notes = append(request.Notes, aistudio.InteractionStopSequencesNote(count))
		request.Config.StopSequences = nil
	}
	video, notes, err := aistudio.NormalizeVideoConfig(overlayVideoConfig(request.Config.VideoConfig, &alias.video), model, request.Contents)
	if err != nil {
		return false, err
	}
	request.Config.VideoConfig = video
	request.Notes = append(request.Notes, notes...)
	return alias.hideThought && modelThinks(model), nil
}
