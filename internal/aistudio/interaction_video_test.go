package aistudio

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
)

// omniTestDefaults 为 Omni 模型目录解析出的生成默认值：思考等级 High/Low、输出上限 65536、默认分辨率 360p
func omniTestDefaults() GenerationDefaults {
	return GenerationDefaults{
		MaxOutputTokens: 65536, Thinking: true, ThinkingLevel: true, DefaultThinkingLevel: 3,
		ThinkingLevels: []int64{1, 3}, InteractionStream: true, VideoResolution: "360p",
	}
}

// encodeOmniTest 编码一次 CreateInteractionStream 请求，返回 interaction、GenerationConfig 与 field 54 输出格式的 JSON
func encodeOmniTest(t *testing.T, contents []Content, video *VideoConfig, defaults GenerationDefaults) ([]any, string, string, error) {
	t.Helper()
	request := GenerateRequest{Model: "gemini-omni-1.1-flash", Contents: contents, Config: GenerationConfig{VideoConfig: video}}
	body, _, err := EncodeCreateInteractionStreamRequest(request, defaults)
	if err != nil {
		return nil, "", "", err
	}
	var root []any
	if err := json.Unmarshal(body, &root); err != nil {
		t.Fatal(err)
	}
	interaction := root[3].([]any)
	config, _ := json.Marshal(interaction[17].([]any)[1])
	format := "absent"
	if len(interaction) > 53 {
		encoded, _ := json.Marshal(interaction[53])
		format = string(encoded)
	}
	return interaction, string(config), format, nil
}

// TestInteractionVideoFormatEncoding 视频设置编码到 field 54 视频格式（field 4 宽高比 16:9=1、9:16=2，field 5 [秒]，field 6 分辨率
// 360p=1、720p=2、1080p=3、4k=4，取最接近的取值）与 GenerationConfig field 25 任务（text_to_video=1 … extend=5）。在上游能编码的取值上
// 与上游 Mag1cFall/AIStudio2API 的 interactionVideoFormat 一致（另在临时程序里对 2 万多种取值组合与上游逐一比对过），只有默认分辨率为空或
// 368p 时不同：本服务发 1（官网默认 360p），上游不带；auto、16x9、VIDEO_TASK_ 前缀等写法上游会报错。未设置分辨率时取模型目录的默认分辨率，
// 宽高比为空或 auto 时不带
func TestInteractionVideoFormatEncoding(t *testing.T) {
	user := []Content{{Role: RoleUser, Parts: []Part{{Text: "一只猫在跳舞"}}}}
	for _, test := range []struct {
		name       string
		video      *VideoConfig
		resolution string
		format     string
		config     string
	}{
		{name: "未设置用目录默认 360p", format: `[[[null,null,null,[null,null,null,null,null,1]]]]`, config: `,[]]`},
		{name: "目录默认 720p", resolution: "720p", format: `[[[null,null,null,[null,null,null,null,null,2]]]]`, config: `,[]]`},
		{name: "目录没有分辨率时按官网默认 360p", resolution: "-", format: `[[[null,null,null,[null,null,null,null,null,1]]]]`, config: `,[]]`},
		{name: "目录默认 368p 换成 360p", resolution: "368p", format: `[[[null,null,null,[null,null,null,null,null,1]]]]`, config: `,[]]`},
		{name: "16:9 8 秒 1080p", video: &VideoConfig{AspectRatio: "16:9", DurationSeconds: 8, Resolution: "1080p"}, format: `[[[null,null,null,[null,null,null,1,[8],3]]]]`, config: `,[]]`},
		{name: "9:16 4k", video: &VideoConfig{AspectRatio: "9:16", Resolution: "4K"}, format: `[[[null,null,null,[null,null,null,2,null,4]]]]`, config: `,[]]`},
		{name: "720p", video: &VideoConfig{Resolution: "720p"}, format: `[[[null,null,null,[null,null,null,null,null,2]]]]`, config: `,[]]`},
		{name: "4:3 取最接近的 16:9", video: &VideoConfig{AspectRatio: "4:3"}, format: `[[[null,null,null,[null,null,null,1,null,1]]]]`, config: `,[]]`},
		{name: "3:4 取最接近的 9:16", video: &VideoConfig{AspectRatio: "3:4"}, format: `[[[null,null,null,[null,null,null,2,null,1]]]]`, config: `,[]]`},
		{name: "1:1 距离相同取较大的 16:9", video: &VideoConfig{AspectRatio: "1:1"}, format: `[[[null,null,null,[null,null,null,1,null,1]]]]`, config: `,[]]`},
		{name: "auto 不带宽高比", video: &VideoConfig{AspectRatio: "auto", DurationSeconds: 10}, format: `[[[null,null,null,[null,null,null,null,[10],1]]]]`, config: `,[]]`},
		{name: "16x9 写法", video: &VideoConfig{AspectRatio: "16x9"}, format: `[[[null,null,null,[null,null,null,1,null,1]]]]`, config: `,[]]`},
		{name: "1440p 取最接近的 1080p", video: &VideoConfig{Resolution: "1440p"}, format: `[[[null,null,null,[null,null,null,null,null,3]]]]`, config: `,[]]`},
		{name: "文字生成视频", video: &VideoConfig{Task: "text_to_video"}, format: `[[[null,null,null,[null,null,null,null,null,1]]]]`, config: `,[1]]`},
		{name: "图片生成视频", video: &VideoConfig{Task: "image_to_video", AspectRatio: "9:16"}, format: `[[[null,null,null,[null,null,null,2,null,1]]]]`, config: `,[2]]`},
		{name: "参考图生成视频", video: &VideoConfig{Task: "reference_to_video"}, format: `[[[null,null,null,[null,null,null,null,null,1]]]]`, config: `,[3]]`},
		{name: "延长视频", video: &VideoConfig{Task: "extend", DurationSeconds: 6}, format: `[[[null,null,null,[null,null,null,null,[6],1]]]]`, config: `,[5]]`},
		{name: "任务 unspecified 等于不指定", video: &VideoConfig{Task: "Unspecified"}, format: `[[[null,null,null,[null,null,null,null,null,1]]]]`, config: `,[]]`},
		{name: "UNSPECIFIED 枚举名等于不指定", video: &VideoConfig{AspectRatio: "ASPECT_RATIO_UNSPECIFIED", Resolution: "VIDEO_RESOLUTION_UNSPECIFIED", Task: "VIDEO_TASK_UNSPECIFIED"},
			resolution: "720p", format: `[[[null,null,null,[null,null,null,null,null,2]]]]`, config: `,[]]`},
		{name: "VIDEO_TASK_ 前缀的任务", video: &VideoConfig{Task: "VIDEO_TASK_TEXT_TO_VIDEO"}, format: `[[[null,null,null,[null,null,null,null,null,1]]]]`, config: `,[1]]`},
	} {
		defaults := omniTestDefaults()
		switch test.resolution {
		case "":
		case "-":
			defaults.VideoResolution = ""
		default:
			defaults.VideoResolution = test.resolution
		}
		interaction, config, format, err := encodeOmniTest(t, user, test.video, defaults)
		if err != nil {
			t.Fatalf("%s: %v", test.name, err)
		}
		if format != test.format || !strings.HasSuffix(config, test.config) || len(interaction) != 54 {
			t.Fatalf("%s: field 54=%s config=%s len=%d，期望 field 54=%s config 以 %s 结尾", test.name, format, config, len(interaction), test.format, test.config)
		}
		if want := `[null,null,null,null,null,4,1,65536,`; !strings.HasPrefix(config, want) {
			t.Fatalf("%s: config=%s，期望以 %s 开头（High 为 4）", test.name, config, want)
		}
	}
}

// officialOmniCapture 为官网一次 Omni 请求（16:9、10 秒、4K、思考等级 High，提示词“随机”）抓到的 CreateInteractionStream 请求体，
// 第 5 项 WAA proof 已换成 null
const officialOmniCapture = `[1,1,null,[null,null,null,null,null,null,null,null,null,null,null,null,null,null,null,null,null,` +
	`["models/gemini-omni-1.1-flash",[null,null,null,null,null,4,1,65536,null,null,null,null,null,null,null,null,null,null,null,null,null,null,null,null,[]]],` +
	`null,null,null,null,null,null,null,null,[[[[[[["随机\n"]]]]]]],null,null,null,null,null,null,null,null,null,null,null,null,null,null,null,null,null,` +
	`null,null,null,null,null,null,null,null,null,[[[null,null,null,[null,null,null,1,[10],4]]]]],null,1]`

// TestInteractionMatchesOfficialCapture 同样的设置编码出的请求体与官网抓包逐字节一致（思考等级取目录默认 High 或显式 high 都一样）
func TestInteractionMatchesOfficialCapture(t *testing.T) {
	for _, effort := range []string{"", "high"} {
		request := GenerateRequest{
			Model:    "gemini-omni-1.1-flash",
			Contents: []Content{{Role: RoleUser, Parts: []Part{{Text: "随机\n"}}}},
			Config: GenerationConfig{
				ReasoningEffort: effort,
				VideoConfig:     &VideoConfig{AspectRatio: "16:9", DurationSeconds: 10, Resolution: "4k"},
			},
		}
		body, _, err := EncodeCreateInteractionStreamRequest(request, omniTestDefaults())
		if err != nil {
			t.Fatal(err)
		}
		if string(body) != officialOmniCapture {
			t.Fatalf("effort=%q 请求体与官网抓包不一致:\n得到 %s\n期望 %s", effort, body, officialOmniCapture)
		}
	}
}

// TestInteractionVideoTaskTurns 视频任务只在第一轮发送：编辑任务 interaction 只到 field 27（不带输出格式）；有模型轮次时 GenerationConfig 只到 field 8、
// 不带 field 25，指定任务返回与官网一致的提示；只有附件的模型消息不算模型轮次
func TestInteractionVideoTaskTurns(t *testing.T) {
	video := []Content{{Role: RoleUser, Parts: []Part{{Text: "把天空换成晚霞"}, {File: &FileRef{ID: "drive-video", MIME: "video/mp4"}}}}}
	interaction, config, format, err := encodeOmniTest(t, video, &VideoConfig{Task: "edit", AspectRatio: "16:9"}, omniTestDefaults())
	if err != nil || len(interaction) != 27 || format != "absent" || !strings.HasSuffix(config, `,[4]]`) {
		t.Fatalf("编辑任务: len=%d field 54=%s config=%s err=%v", len(interaction), format, config, err)
	}

	history := []Content{
		{Role: RoleUser, Parts: []Part{{Text: "做个视频"}}},
		{Role: RoleAssistant, Parts: []Part{{Text: "好的"}}},
		{Role: RoleUser, Parts: []Part{{Text: "再来一个竖屏的"}}},
	}
	interaction, config, format, err = encodeOmniTest(t, history, &VideoConfig{AspectRatio: "9:16"}, omniTestDefaults())
	if err != nil || config != `[null,null,null,null,null,4,1,65536]` || format != `[[[null,null,null,[null,null,null,2,null,1]]]]` || len(interaction) != 54 {
		t.Fatalf("非第一轮: config=%s field 54=%s err=%v", config, format, err)
	}
	if _, _, _, err = encodeOmniTest(t, history, &VideoConfig{Task: "text_to_video"}, omniTestDefaults()); err == nil || err.Error() != videoTaskFirstTurnMessage {
		t.Fatalf("非第一轮指定任务应返回官网提示，得到 %v", err)
	}

	mediaOnly := []Content{
		{Role: RoleUser, Parts: []Part{{Text: "做个视频"}}},
		{Role: RoleAssistant, Parts: []Part{{File: &FileRef{ID: "drive-generated", MIME: "video/mp4"}}}},
		{Role: RoleUser, Parts: []Part{{Text: "延长 5 秒"}}},
	}
	if _, config, _, err = encodeOmniTest(t, mediaOnly, &VideoConfig{Task: "extend"}, omniTestDefaults()); err != nil || !strings.HasSuffix(config, `,[5]]`) {
		t.Fatalf("只有附件的模型消息不算模型轮次: config=%s err=%v", config, err)
	}
	for _, contents := range [][]Content{video, history, mediaOnly, {
		{Role: RoleUser, Parts: []Part{{Text: "a"}}},
		{Role: RoleAssistant, Parts: []Part{{Text: "想", Thought: true}, {ThoughtSignature: "sig"}}},
		{Role: RoleUser, Parts: []Part{{Text: "b"}}},
	}} {
		_, _, hasModelTurn, err := encodeInteractionSteps(contents)
		if err != nil || hasModelTurn != interactionHasModelTurn(contents) {
			t.Fatalf("interactionHasModelTurn 与编码规则不一致: %v err=%v", hasModelTurn, err)
		}
	}
}

// TestInteractionVideoFormatErrors 无法解析的宽高比、分辨率、未知任务与负数时长返回英文参数错误（原样返回给客户端）
func TestInteractionVideoFormatErrors(t *testing.T) {
	user := []Content{{Role: RoleUser, Parts: []Part{{Text: "hi"}}}}
	for _, video := range []*VideoConfig{
		{AspectRatio: "wide"}, {Resolution: "ultra"}, {Task: "remix"}, {DurationSeconds: -1},
	} {
		if _, _, _, err := encodeOmniTest(t, user, video, omniTestDefaults()); err == nil || containsHan(err.Error()) {
			t.Fatalf("%+v 应返回英文参数错误，得到 %v", *video, err)
		}
	}
}

// TestInteractionIgnoresStopSequences stop sequences 不再让整次请求失败，也不发送（说明由协议层或 generateInteraction 记下）
func TestInteractionIgnoresStopSequences(t *testing.T) {
	request := GenerateRequest{
		Model: "gemini-omni-1.1-flash", Contents: []Content{{Role: RoleUser, Parts: []Part{{Text: "hi"}}}},
		Config: GenerationConfig{StopSequences: []string{"END"}},
	}
	body, _, err := EncodeCreateInteractionStreamRequest(request, omniTestDefaults())
	if err != nil || strings.Contains(string(body), "END") {
		t.Fatalf("stop sequences 应忽略: body=%s err=%v", body, err)
	}
}

func containsHan(text string) bool {
	for _, char := range text {
		if char >= 0x4e00 && char <= 0x9fff {
			return true
		}
	}
	return false
}

// omniTestModel 为目录解析后的 Omni 模型：宽高比 16:9、9:16，分辨率 360p–4k，时长 4–15 秒步长 1 默认 10，帧率 24fps，五种任务
func omniTestModel() Model {
	return Model{
		ID: "gemini-omni-1.1-flash", Methods: []string{"generateContent"},
		Capabilities: map[string]bool{"interactions_api": true, "thinking_level": true},
		CapabilityOptions: map[string][]string{
			"video_aspect_ratios":      {"16:9", "9:16"},
			"video_output_resolutions": {"360p", "720p", "1080p", "4k"},
			"video_duration_range":     {"min=4", "max=15", "step=1", "default=10"},
			"video_frame_rates":        {"24fps"},
			"video_tasks":              {"text_to_video", "image_to_video", "reference_to_video", "edit", "extend"},
		},
	}
}

// TestNormalizeVideoConfig 按模型目录整理视频设置：时长超出范围或不在步长上时取最接近的合法值并记说明；帧率只接受 24fps；
// 任务必须在目录里、只在第一轮、需要的输入齐全；宽高比与分辨率换成最接近的模型选项并记说明
func TestNormalizeVideoConfig(t *testing.T) {
	model := omniTestModel()
	text := []Content{{Role: RoleUser, Parts: []Part{{Text: "一只猫"}}}}
	image := []Content{{Role: RoleUser, Parts: []Part{{Text: "动起来"}, {File: &FileRef{ID: "drive-image", MIME: "image/png"}}}}}
	unknown := []Content{{Role: RoleUser, Parts: []Part{{Text: "改"}, {File: &FileRef{ID: "drive-file"}}}}}
	for _, test := range []struct {
		name     string
		config   VideoConfig
		model    *Model
		contents []Content
		want     VideoConfig
		note     string
		err      string
	}{
		{name: "合法取值不改", config: VideoConfig{AspectRatio: "9:16", DurationSeconds: 8, Resolution: "720p", FrameRate: 24}, want: VideoConfig{AspectRatio: "9:16", DurationSeconds: 8, Resolution: "720p", FrameRate: 24}},
		{name: "超过上限", config: VideoConfig{DurationSeconds: 30}, want: VideoConfig{DurationSeconds: 15}, note: "视频时长 30 秒不是模型可选的时长（4–15 秒、步长 1），按最接近的 15 秒发送"},
		{name: "低于下限", config: VideoConfig{DurationSeconds: 2}, want: VideoConfig{DurationSeconds: 4}, note: "按最接近的 4 秒发送"},
		{name: "步长 2", config: VideoConfig{DurationSeconds: 7}, model: withDurationRange(model, "min=4", "max=15", "step=2"), want: VideoConfig{DurationSeconds: 8}, note: "步长 2"},
		{name: "步长 2 上限附近", config: VideoConfig{DurationSeconds: 15}, model: withDurationRange(model, "min=4", "max=15", "step=2"), want: VideoConfig{DurationSeconds: 14}, note: "按最接近的 14 秒发送"},
		{name: "合并目录取各账户都能接受的上限", config: VideoConfig{DurationSeconds: 20}, model: withDurationRange(model, "max=15", "max=20", "min=4"), want: VideoConfig{DurationSeconds: 15}, note: "4–15 秒"},
		{name: "合并目录下限取最大、步长取最大", config: VideoConfig{DurationSeconds: 20}, model: withDurationRange(model, "max=15", "max=20", "min=4", "min=6", "step=1", "step=2"), want: VideoConfig{DurationSeconds: 14}, note: "6–15 秒、步长 2"},
		{name: "宽高比 auto", config: VideoConfig{AspectRatio: " Auto "}, want: VideoConfig{}},
		{name: "宽高比 4:3", config: VideoConfig{AspectRatio: "4:3"}, want: VideoConfig{AspectRatio: "16:9"}, note: "视频宽高比 4:3 不是模型选项，按最接近的 16:9 发送"},
		{name: "分辨率大写", config: VideoConfig{Resolution: "1080P"}, want: VideoConfig{Resolution: "1080p"}},
		{name: "分辨率 2160p", config: VideoConfig{Resolution: "2160p"}, want: VideoConfig{Resolution: "4k"}, note: "按最接近的 4k 发送"},
		{name: "目录只有 720p", config: VideoConfig{Resolution: "4k"}, model: withOption(model, "video_output_resolutions", "720p"), want: VideoConfig{Resolution: "720p"}, note: "按最接近的 720p 发送"},
		{name: "目录列 368p 时请求 360p", config: VideoConfig{Resolution: "360p"}, model: withOption(model, "video_output_resolutions", "368p", "720p", "1080p", "4k"), want: VideoConfig{Resolution: "360p"}},
		{name: "目录列 368p 时请求 368p", config: VideoConfig{Resolution: "368P"}, model: withOption(model, "video_output_resolutions", "368p", "720p", "1080p", "4k"), want: VideoConfig{Resolution: "360p"}},
		{name: "目录没有 368p 时请求 368p", config: VideoConfig{Resolution: "368p"}, want: VideoConfig{Resolution: "360p"}, note: "视频分辨率 368p 不是模型选项，按最接近的 360p 发送"},
		{name: "UNSPECIFIED 枚举名按未设置", config: VideoConfig{AspectRatio: "ASPECT_RATIO_UNSPECIFIED", Resolution: "VIDEO_RESOLUTION_UNSPECIFIED", Task: "VIDEO_TASK_UNSPECIFIED", DurationSeconds: 8}, want: VideoConfig{DurationSeconds: 8}},
		{name: "unspecified 写法按未设置", config: VideoConfig{AspectRatio: "unspecified", Resolution: "Unspecified"}, want: VideoConfig{}},
		{name: "VIDEO_TASK_ 前缀", config: VideoConfig{Task: "VIDEO_TASK_TEXT_TO_VIDEO"}, want: VideoConfig{Task: "text_to_video"}},
		{name: "帧率 30", config: VideoConfig{FrameRate: 30}, err: "this model only supports 24fps"},
		{name: "目录没有帧率时只接受 24", config: VideoConfig{FrameRate: 25}, model: withOption(model, "video_frame_rates"), err: "only supports 24fps"},
		{name: "负数时长", config: VideoConfig{DurationSeconds: -3}, err: "must not be negative"},
		{name: "未知任务", config: VideoConfig{Task: "remix"}, err: "video task \"remix\" is not valid"},
		{name: "目录没有的任务", config: VideoConfig{Task: "edit"}, model: withOption(model, "video_tasks", "text_to_video", "image_to_video"), contents: unknown, err: "does not support video task edit"},
		{name: "图片任务没有图片", config: VideoConfig{Task: "image_to_video"}, err: "requires an input image"},
		{name: "编辑任务只有图片", config: VideoConfig{Task: "edit"}, contents: image, err: "requires an input video"},
		{name: "图片任务", config: VideoConfig{Task: "Image_To_Video"}, contents: image, want: VideoConfig{Task: "image_to_video"}},
		{name: "类型不明的文件可作视频", config: VideoConfig{Task: "extend"}, contents: unknown, want: VideoConfig{Task: "extend"}},
		{name: "编辑任务的输出格式不发送", config: VideoConfig{Task: "edit", AspectRatio: "9:16"}, contents: unknown, want: VideoConfig{Task: "edit", AspectRatio: "9:16"}, note: "视频任务 edit 不带输出格式"},
		{name: "非第一轮", config: VideoConfig{Task: "text_to_video"}, contents: []Content{
			{Role: RoleUser, Parts: []Part{{Text: "a"}}}, {Role: RoleAssistant, Parts: []Part{{Text: "b"}}}, {Role: RoleUser, Parts: []Part{{Text: "c"}}},
		}, err: videoTaskFirstTurnMessage},
		{name: "无法解析的宽高比", config: VideoConfig{AspectRatio: "portrait"}, err: "aspect_ratio \"portrait\" is not valid"},
	} {
		target := model
		if test.model != nil {
			target = *test.model
		}
		contents := test.contents
		if contents == nil {
			contents = text
		}
		config := test.config
		got, notes, err := NormalizeVideoConfig(&config, target, contents)
		if test.err != "" {
			if err == nil || !strings.Contains(err.Error(), test.err) || containsHan(err.Error()) {
				t.Fatalf("%s: 期望英文错误含 %q，得到 %v", test.name, test.err, err)
			}
			continue
		}
		if err != nil || got == nil || *got != test.want {
			t.Fatalf("%s: 得到 %+v err=%v，期望 %+v", test.name, got, err, test.want)
		}
		if test.note == "" && len(notes) > 0 || test.note != "" && (len(notes) != 1 || !strings.Contains(notes[0], test.note)) {
			t.Fatalf("%s: 说明 %v，期望含 %q", test.name, notes, test.note)
		}
		if config != test.config {
			t.Fatalf("%s: 不应修改传入的设置", test.name)
		}
	}
	if got, notes, err := NormalizeVideoConfig(nil, model, text); got != nil || notes != nil || err != nil {
		t.Fatalf("没有视频设置: %+v %v %v", got, notes, err)
	}
}

func withOption(model Model, key string, values ...string) *Model {
	options := make(map[string][]string, len(model.CapabilityOptions))
	for name, current := range model.CapabilityOptions {
		options[name] = current
	}
	if len(values) == 0 {
		delete(options, key)
	} else {
		options[key] = values
	}
	model.CapabilityOptions = options
	return &model
}

func withDurationRange(model Model, values ...string) *Model {
	return withOption(model, "video_duration_range", values...)
}

// omniCatalogRow 按官网 ListModels 的位置构造 Omni 模型行：field 71（下标 70）子索引 4 宽高比、7 输入方式、9 分辨率、
// 10 时长滑块、11 帧率，field 79（下标 78）Interaction 配置，field 85（下标 84）视频任务
func omniCatalogRow(video []any, tasks any) []any {
	row := make([]any, 85)
	row[0] = "models/gemini-omni-1.1-flash"
	row[3] = "Gemini Omni 1.1 Flash"
	row[5] = 131072
	row[6] = 65536
	row[7] = []string{"countTokens", "generateContent"}
	row[64] = []int{1, 25, 52, 59}
	row[70] = video
	row[71] = []any{nil, nil, nil, nil, nil, 3, []int{1, 3}}
	row[78] = []any{nil, nil, false, 2}
	row[84] = tasks
	return row
}

// veoCatalogRow 构造 Veo 模型行：field 71（下标 70）为视频能力，没有 field 79 Interaction 配置，经 predictLongRunning 生成
func veoCatalogRow(id string, video []any) []any {
	row := make([]any, 85)
	row[0] = "models/" + id
	row[3] = strings.ToUpper(id)
	row[7] = []string{"predictLongRunning"}
	row[64] = []int{20}
	row[70] = video
	return row
}

// TestParseModelsOmniVideoOptions 目录解析补齐 Omni 视频选项：宽高比、分辨率（第一项为默认分辨率）、时长范围 min/max/step/default、
// 帧率 24fps、视频输入方式与任务，旧字段不变；新增的几项取值形状不对时只省略该项，不让目录解析失败；Veo 列出视频选项也不算 Omni
func TestParseModelsOmniVideoOptions(t *testing.T) {
	video := []any{nil, nil, nil, nil, []int{3, 2}, nil, nil, []int{3, 4, 5}, nil, []int{5, 1, 2, 3}, []int{4, 15, 1, 10}, []int{1}}
	raw, _ := json.Marshal([]any{[]any{omniCatalogRow(video, []int{1, 2, 3, 4, 5}), catalogTestRow("gemini-2.5-flash", 1)}})
	catalog, err := parseModelCatalog(strings.NewReader(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	entry := catalog.entries["gemini-omni-1.1-flash"]
	want := map[string][]string{
		"video_aspect_ratios":      {"16:9", "9:16"},
		"video_output_resolutions": {"360p", "720p", "1080p", "4k"},
		"video_inputs":             {"frames", "reference_images"},
		"video_duration_range":     {"min=4", "max=15", "step=1", "default=10"},
		"video_frame_rates":        {"24fps"},
		"video_tasks":              {"text_to_video", "image_to_video", "reference_to_video", "edit", "extend"},
	}
	for key, values := range want {
		if !slices.Equal(entry.model.CapabilityOptions[key], values) {
			t.Fatalf("%s = %v，期望 %v", key, entry.model.CapabilityOptions[key], values)
		}
	}
	if entry.defaults.VideoResolution != "360p" || !entry.defaults.InteractionStream || !OmniVideoModel(entry.model) {
		t.Fatalf("defaults=%+v omni=%v", entry.defaults, OmniVideoModel(entry.model))
	}
	bounds, ok := ModelVideoDurationRange(entry.model)
	if !ok || bounds != (VideoDurationRange{Min: 4, Max: 15, Step: 1, Default: 10}) {
		t.Fatalf("时长范围 %+v ok=%v", bounds, ok)
	}
	if text := catalog.entries["gemini-2.5-flash"]; text.model.CapabilityOptions != nil || OmniVideoModel(text.model) || text.defaults.VideoResolution != "" {
		t.Fatalf("文本模型不应有视频选项: %+v", text.model.CapabilityOptions)
	}

	// Veo 同样列出视频选项，但没有 Interaction 配置（field 79），不是 Omni；子索引 7 含 3 而子索引 9 没有列出分辨率时，
	// 与上游相同取官网提供的 720p、1080p、4k，第一项 720p 为默认分辨率；子索引 9 列出时以子索引 9 为准
	raw, _ = json.Marshal([]any{[]any{
		veoCatalogRow("veo-3.1-generate-preview", []any{nil, nil, nil, nil, []int{3, 2}, []int{5, 2, 4}, nil, []int{3, 4, 5}, nil, []int{1, 2}}),
		veoCatalogRow("veo-3.1-fast-generate-preview", []any{nil, nil, nil, nil, []int{3, 2}, []int{4}, nil, []int{3}, nil, nil}),
		veoCatalogRow("veo-2.0-generate-001", []any{nil, nil, nil, nil, []int{3}, []int{1}, nil, []int{4}, nil, nil}),
	}})
	catalog, err = parseModelCatalog(strings.NewReader(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	for id, resolutions := range map[string][]string{
		"veo-3.1-generate-preview":      {"720p", "1080p"},
		"veo-3.1-fast-generate-preview": {"720p", "1080p", "4k"},
		"veo-2.0-generate-001":          nil,
	} {
		veo := catalog.entries[id]
		if OmniVideoModel(veo.model) || veo.model.Capabilities["interactions_api"] || !veo.model.Capabilities["video_route"] {
			t.Fatalf("%s 不是 Omni: %+v", id, veo.model.Capabilities)
		}
		if got := veo.model.CapabilityOptions["video_output_resolutions"]; !slices.Equal(got, resolutions) {
			t.Fatalf("%s video_output_resolutions = %v，期望 %v", id, got, resolutions)
		}
		want := ""
		if len(resolutions) > 0 {
			want = resolutions[0]
		}
		if veo.defaults.VideoResolution != want {
			t.Fatalf("%s 默认分辨率 %q，期望 %q", id, veo.defaults.VideoResolution, want)
		}
	}

	odd := []any{nil, nil, nil, nil, []int{3}, nil, nil, "frames", nil, []int{1}, []any{true, []int{15}}, map[string]any{"fps": 24}}
	raw, _ = json.Marshal([]any{[]any{omniCatalogRow(odd, "all")}})
	catalog, err = parseModelCatalog(strings.NewReader(string(raw)))
	if err != nil {
		t.Fatalf("新增视频选项形状不对时不应让目录解析失败: %v", err)
	}
	options := catalog.entries["gemini-omni-1.1-flash"].model.CapabilityOptions
	for _, key := range []string{"video_inputs", "video_duration_range", "video_frame_rates", "video_tasks"} {
		if _, exists := options[key]; exists {
			t.Fatalf("%s 形状不对时应省略: %v", key, options)
		}
	}
	if !slices.Equal(options["video_output_resolutions"], []string{"720p"}) || catalog.entries["gemini-omni-1.1-flash"].defaults.VideoResolution != "720p" {
		t.Fatalf("旧字段应照常解析: %v", options)
	}
}

// TestMergedVideoDurationRange 多个账户的目录合并后（取值按字符串排序去重）时长范围取各账户都能接受的交集：账户 A 为 4–8 秒、默认 8，
// 账户 B 为 4–10 秒、默认 10，合并后为 4–8 秒、默认 8；协议层按合并目录把 10 秒调成 8 秒并记说明，这一说明对两个账户都成立
func TestMergedVideoDurationRange(t *testing.T) {
	accountA := omniTestModel()
	accountA.CapabilityOptions = withDurationRange(accountA, "min=4", "max=8", "step=1", "default=8").CapabilityOptions
	accountB := omniTestModel()
	accountB.CapabilityOptions = withDurationRange(accountB, "min=4", "max=10", "step=1", "default=10").CapabilityOptions
	merged := mergeModelSets([][]Model{{accountA}, {accountB}})
	if len(merged) != 1 {
		t.Fatalf("合并结果 %+v", merged)
	}
	if got := merged[0].CapabilityOptions["video_duration_range"]; !slices.Equal(got, []string{"default=10", "default=8", "max=10", "max=8", "min=4", "step=1"}) {
		t.Fatalf("合并后的取值按字符串排序去重，得到 %v", got)
	}
	bounds, ok := ModelVideoDurationRange(merged[0])
	if !ok || bounds != (VideoDurationRange{Min: 4, Max: 8, Step: 1, Default: 8}) {
		t.Fatalf("合并目录的时长范围 %+v ok=%v，期望 4–8 秒、默认 8", bounds, ok)
	}
	text := []Content{{Role: RoleUser, Parts: []Part{{Text: "一只猫"}}}}
	got, notes, err := NormalizeVideoConfig(&VideoConfig{DurationSeconds: 10}, merged[0], text)
	if err != nil || got.DurationSeconds != 8 || len(notes) != 1 || !strings.Contains(notes[0], "按最接近的 8 秒发送") {
		t.Fatalf("协议层整理: %+v %v %v", got, notes, err)
	}
	for _, account := range []Model{accountA, accountB} {
		if again, more, err := normalizeVideoConfig(got, account, text, false); err != nil || *again != *got || len(more) != 0 {
			t.Fatalf("%v: 按账户目录再整理不应再改: %+v %v %v", account.CapabilityOptions["video_duration_range"], again, more, err)
		}
	}
	// 各账户的范围不相交时不按合并目录调整（交给发送前按账户目录整理）；默认值都不在交集内时换成交集内最接近的
	if _, ok := ModelVideoDurationRange(*withDurationRange(accountA, "min=4", "min=10", "max=8", "max=15")); ok {
		t.Fatal("范围不相交时应返回 ok=false")
	}
	if bounds, _ := ModelVideoDurationRange(*withDurationRange(accountA, "min=4", "min=6", "max=8", "max=15", "default=4")); bounds.Default != 6 {
		t.Fatalf("默认值不在交集内时应换成范围内最接近的，得到 %+v", bounds)
	}
}

// interactionCapture 记录 generateInteraction 发出的 CreateInteractionStream 请求体，并以错误结束（不解析响应）
type interactionCapture struct {
	bodies [][]byte
}

func (capture *interactionCapture) DoProtected(_ context.Context, _ GenerateRequest, rpc RPCRequest) (*RPCResponse, error) {
	capture.bodies = append(capture.bodies, append([]byte(nil), rpc.Body...))
	return nil, errors.New("测试上游不返回响应")
}

// TestGenerateInteractionNotes 发送前按所选账户的目录再整理视频设置：留下的新说明（协议层没有记过的）经请求说明观察者报告，
// 已在协议层记过的不重复；协议层没有清空的 stop sequences 不发送，补记一行；单个账户目录没有列出的视频任务不再直接拒绝（交给上游判断）
func TestGenerateInteractionNotes(t *testing.T) {
	capture := &interactionCapture{}
	client, err := NewClient(ClientOptions{Transport: ceilingTransport{}, Protected: capture})
	if err != nil {
		t.Fatal(err)
	}
	account := omniTestModel()
	account.CapabilityOptions = withOption(*withDurationRange(account, "min=4", "max=8", "step=1", "default=8"), "video_tasks", "text_to_video").CapabilityOptions
	entry := modelEntry{model: account, defaults: omniTestDefaults()}
	run := func(request GenerateRequest) []string {
		t.Helper()
		var notes []string
		ctx := ContextWithRequestNoteObserver(context.Background(), func(note string) { notes = append(notes, note) })
		if _, err := client.generateInteraction(ctx, request, entry); err == nil || errors.Is(err, ErrInvalidArgument) {
			t.Fatalf("应发往上游（测试上游返回错误），得到 %v", err)
		}
		return notes
	}
	text := []Content{{Role: RoleUser, Parts: []Part{{Text: "一只猫"}}}}
	durationNote := "视频时长 10 秒不是模型可选的时长（4–8 秒、步长 1），按最接近的 8 秒发送"

	notes := run(GenerateRequest{Model: account.ID, Contents: text, Config: GenerationConfig{VideoConfig: &VideoConfig{DurationSeconds: 10}, StopSequences: []string{"END", "###"}}})
	if !slices.Equal(notes, []string{durationNote, InteractionStopSequencesNote(2)}) {
		t.Fatalf("协议层没有整理时应报告按账户调整的时长与忽略的停止序列: %v", notes)
	}
	body := string(capture.bodies[len(capture.bodies)-1])
	if !strings.Contains(body, `[null,null,null,null,[8],1]`) || strings.Contains(body, "END") {
		t.Fatalf("应按账户范围发送 8 秒且不发送停止序列: %s", body)
	}

	if notes := run(GenerateRequest{Model: account.ID, Contents: text, Notes: []string{durationNote}, Config: GenerationConfig{VideoConfig: &VideoConfig{DurationSeconds: 10}}}); len(notes) != 0 {
		t.Fatalf("协议层已记过的说明不应重复报告: %v", notes)
	}

	video := []Content{{Role: RoleUser, Parts: []Part{{Text: "把天空换成晚霞"}, {File: &FileRef{ID: "drive-video", MIME: "video/mp4"}}}}}
	if notes := run(GenerateRequest{Model: account.ID, Contents: video, Config: GenerationConfig{VideoConfig: &VideoConfig{Task: "edit"}}}); len(notes) != 0 {
		t.Fatalf("编辑任务不应留下说明: %v", notes)
	}
	if body := string(capture.bodies[len(capture.bodies)-1]); !strings.Contains(body, `,[4]]`) {
		t.Fatalf("账户目录没有列出 edit 时也应照发任务: %s", body)
	}
	// 第一轮与任务输入的检查与账户无关，照常返回参数错误
	if _, err := client.generateInteraction(context.Background(), GenerateRequest{Model: account.ID, Contents: text, Config: GenerationConfig{VideoConfig: &VideoConfig{Task: "edit"}}}, entry); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("编辑任务没有视频应返回参数错误，得到 %v", err)
	}
}
