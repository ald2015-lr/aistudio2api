package api

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// omniAliasModels 为带 Omni 视频模型的目录：Omni 列出视频选项与目录别名 omni-video-latest，另有文本模型、ID 本身以 -4k 结尾的文本模型、
// 图片模型与 Veo（同样列出视频选项，但没有 Interaction 配置，不是 Omni）
func omniAliasModels() []aistudio.Model {
	return append(aliasTestModels(),
		aistudio.Model{
			ID: "gemini-omni-1.1-flash", Name: "Gemini Omni 1.1 Flash", Methods: []string{"countTokens", "generateContent"},
			Capabilities: map[string]bool{"chat_model": true, "thinking": true, "thinking_level": true, "interaction_route": true, "interactions_api": true},
			CapabilityOptions: map[string][]string{
				"video_aspect_ratios":      {"16:9", "9:16"},
				"video_output_resolutions": {"360p", "720p", "1080p", "4k"},
				"video_duration_range":     {"min=4", "max=15", "step=1", "default=10"},
				"video_frame_rates":        {"24fps"},
				"video_tasks":              {"text_to_video", "image_to_video", "reference_to_video", "edit", "extend"},
				"aliases":                  {"omni-video-latest"},
			},
		},
		aistudio.Model{ID: "vendor-chat-4k", Name: "Vendor 4K", Capabilities: map[string]bool{"chat_model": true, "thinking": true, "thinking_level": true}},
		aistudio.Model{
			ID: "veo-3.1-generate-preview", Name: "Veo 3.1", Methods: []string{"predictLongRunning"},
			Capabilities: map[string]bool{"video_route": true},
			CapabilityOptions: map[string][]string{
				"video_aspect_ratios":      {"16:9", "9:16"},
				"video_durations_seconds":  {"4", "6", "8"},
				"video_output_resolutions": {"720p", "1080p"},
			},
		},
	)
}

// TestResolveOmniVideoSuffixes 视频后缀只对能输出视频的 Omni 模型解析：可与思考等级、-nothinking 组合，顺序不限、不分大小写，
// 同一项出现多次时以最靠后的为准；其他模型带这类字样时与原先一样解析不到，目录 ID 本身带这类字样时按目录 ID
func TestResolveOmniVideoSuffixes(t *testing.T) {
	models := omniAliasModels()
	for _, test := range []struct {
		name  string
		id    string
		level string
		hide  bool
		video aistudio.VideoConfig
		ok    bool
	}{
		{name: "gemini-omni-1.1-flash", id: "gemini-omni-1.1-flash", ok: true},
		{name: "gemini-omni-1.1-flash-9x16-720p-8s-low", id: "gemini-omni-1.1-flash", level: "low", video: aistudio.VideoConfig{AspectRatio: "9:16", Resolution: "720p", DurationSeconds: 8}, ok: true},
		{name: "gemini-omni-1.1-flash-low-8s-720p-9x16", id: "gemini-omni-1.1-flash", level: "low", video: aistudio.VideoConfig{AspectRatio: "9:16", Resolution: "720p", DurationSeconds: 8}, ok: true},
		{name: "gemini-omni-1.1-flash-I2V-4K-nothinking", id: "gemini-omni-1.1-flash", hide: true, video: aistudio.VideoConfig{Task: "image_to_video", Resolution: "4k"}, ok: true},
		{name: "gemini-omni-1.1-flash-16x9-9x16", id: "gemini-omni-1.1-flash", video: aistudio.VideoConfig{AspectRatio: "9:16"}, ok: true},
		{name: "gemini-omni-1.1-flash-edit-t2v-10S", id: "gemini-omni-1.1-flash", video: aistudio.VideoConfig{Task: "text_to_video", DurationSeconds: 10}, ok: true},
		{name: "gemini-omni-1.1-flash-r2v-extend", id: "gemini-omni-1.1-flash", video: aistudio.VideoConfig{Task: "extend"}, ok: true},
		{name: "gemini-omni-1.1-flash-high", id: "gemini-omni-1.1-flash", level: "high", ok: true},
		{name: "gemini-omni-1.1-flash-128-360p", id: "gemini-omni-1.1-flash", level: "minimal", video: aistudio.VideoConfig{Resolution: "360p"}, ok: true},
		{name: " models/gemini-omni-1.1-flash-1080p ", id: "gemini-omni-1.1-flash", video: aistudio.VideoConfig{Resolution: "1080p"}, ok: true},
		{name: "gemini-omni-1.1-flash-1000s", ok: false},
		{name: "gemini-omni-1.1-flash-21x9", ok: false},
		{name: "gemini-4-argon-720p", ok: false},
		{name: "gemini-4-argon-9x16-low", ok: false},
		{name: "image-model-4k", ok: false},
		{name: "gemini-4-argon-low", id: "gemini-4-argon", level: "low", ok: true},
		{name: "vendor-chat-4k", id: "vendor-chat-4k", ok: true},
		{name: "vendor-chat-4k-medium", id: "vendor-chat-4k", level: "medium", ok: true},
		{name: "veo-3.1-generate-preview", id: "veo-3.1-generate-preview", ok: true},
		{name: "veo-3.1-generate-preview-8s", ok: false},
		{name: "veo-3.1-generate-preview-720p", ok: false},
		{name: "veo-3.1-generate-preview-9x16-t2v", ok: false},
	} {
		model, alias, ok := resolveRequestModel(models, test.name)
		if ok != test.ok || ok && (model.ID != test.id || alias.level != test.level || alias.hideThought != test.hide || alias.video != test.video) {
			t.Fatalf("%q 解析为 model=%s alias=%+v ok=%v，期望 model=%s level=%q hide=%v video=%+v ok=%v",
				test.name, model.ID, alias, ok, test.id, test.level, test.hide, test.video, test.ok)
		}
	}
	// 视频后缀同样对 Omni 的目录别名生效，alias.base 为别名（调度前再换成目录 ID）
	for name, want := range map[string]aistudio.VideoConfig{
		"omni-video-latest":              {},
		"omni-video-latest-9x16-8s-high": {AspectRatio: "9:16", DurationSeconds: 8},
	} {
		model, alias, ok := resolveRequestModel(models, name)
		if !ok || model.ID != "gemini-omni-1.1-flash" || alias.base != "omni-video-latest" || alias.video != want {
			t.Fatalf("%s 解析为 model=%s alias=%+v ok=%v", name, model.ID, alias, ok)
		}
	}
	if _, _, ok := resolveModelAlias(models, "omni-video-latest-low"); ok {
		t.Fatal("原有的后缀解析不按目录别名匹配")
	}
	// 原有的后缀解析不变：不带视频后缀时与原先完全相同
	if _, alias, ok := resolveModelAlias(models, "gemini-omni-1.1-flash-720p"); ok || alias.video != (aistudio.VideoConfig{}) {
		t.Fatal("resolveModelAlias 不应解析视频后缀")
	}
	if model, ok := lookupPublicModel(models, "gemini-omni-1.1-flash-9x16-720p-low"); !ok || model.ID != "gemini-omni-1.1-flash-9x16-720p-low" {
		t.Fatalf("单模型查询应能找到 Omni 后缀组合: %+v ok=%v", model, ok)
	}
	for _, name := range []string{"gemini-4-argon-720p", "veo-3.1-generate-preview-8s"} {
		if _, ok := lookupPublicModel(models, name); ok {
			t.Fatalf("%s: 非 Omni 模型带视频后缀不应查到", name)
		}
	}
	for _, model := range expandModelAliases(models) {
		if strings.HasPrefix(model.ID, "gemini-omni-1.1-flash-") {
			t.Fatalf("模型列表不展开 Omni 后缀组合: %s", model.ID)
		}
	}
}

// omniHandler 返回带 Omni 目录、记录最后一次生成请求的公开 API
func omniHandler(t *testing.T) (http.Handler, *recordingService) {
	t.Helper()
	service := &recordingService{modelListService: modelListService{models: omniAliasModels()}}
	handler, _ := newMediaHandler(t, service, &mediaSettingsStub{})
	return handler, service
}

// TestOmniVideoProtocolParameters 四套协议（加 Responses）的视频设置写法映射为同一份规范设置：模型后缀优先于请求参数，
// Gemini 的 videoConfig 覆盖 responseFormat.video，Interactions 的 generation_config.video_config 覆盖 response_format；
// stop sequences 清空并留下请求日志说明，响应里的模型名去掉后缀
func TestOmniVideoProtocolParameters(t *testing.T) {
	anthropic := map[string]string{"Anthropic-Version": "2023-06-01"}
	for _, test := range []struct {
		name   string
		path   string
		body   string
		header map[string]string
		want   aistudio.VideoConfig
		model  string
		stop   bool
	}{
		{
			name: "Chat 顶层 video_config", path: "/v1/chat/completions",
			body: `{"model":"gemini-omni-1.1-flash","stop":["END"],"messages":[{"role":"user","content":"做个视频"}],
				"video_config":{"aspect_ratio":"9:16","duration_seconds":"8s","resolution":"720P","frame_rate":24,"task":"text_to_video"}}`,
			want: aistudio.VideoConfig{AspectRatio: "9:16", DurationSeconds: 8, Resolution: "720p", FrameRate: 24, Task: "text_to_video"}, stop: true,
		},
		{
			name: "Chat 后缀优先于参数", path: "/v1/chat/completions",
			body: `{"model":"gemini-omni-1.1-flash-16x9-1080p-6s-low","messages":[{"role":"user","content":"做个视频"}],
				"video_config":{"aspect_ratio":"9:16","duration":10,"resolution":"360p","frame_rate":"24fps"}}`,
			want: aistudio.VideoConfig{AspectRatio: "16:9", DurationSeconds: 6, Resolution: "1080p", FrameRate: 24}, model: "gemini-omni-1.1-flash",
		},
		{
			name: "Responses 顶层 video_config", path: "/v1/responses",
			body: `{"model":"gemini-omni-1.1-flash","input":"做个视频","video_config":{"aspect_ratio":"16:9","duration":"12","fps":24}}`,
			want: aistudio.VideoConfig{AspectRatio: "16:9", DurationSeconds: 12, FrameRate: 24},
		},
		{
			name: "Anthropic 顶层 video_config", path: "/v1/messages", header: anthropic,
			body: `{"model":"gemini-omni-1.1-flash","max_tokens":64,"stop_sequences":["###"],"messages":[{"role":"user","content":"做个视频"}],
				"video_config":{"aspect_ratio":"9:16","duration_seconds":7.6,"resolution":"4k"}}`,
			want: aistudio.VideoConfig{AspectRatio: "9:16", DurationSeconds: 8, Resolution: "4k"}, stop: true,
		},
		{
			name: "Gemini responseFormat 与 videoConfig", path: "/v1beta/models/gemini-omni-1.1-flash:generateContent",
			body: `{"contents":[{"role":"user","parts":[{"text":"做个视频"}]}],"generationConfig":{"stopSequences":["END"],
				"responseFormat":{"video":{"aspectRatio":"ASPECT_RATIO_NINE_BY_SIXTEEN","duration":"8s","resolution":"720p"}},
				"videoConfig":{"task":"text_to_video","resolution":"1080p"}}}`,
			want: aistudio.VideoConfig{AspectRatio: "9:16", DurationSeconds: 8, Resolution: "1080p", Task: "text_to_video"}, stop: true,
		},
		{
			name: "Gemini snake_case 与数组写法", path: "/v1beta/models/gemini-omni-1.1-flash-4k:generateContent",
			body: `{"contents":[{"role":"user","parts":[{"text":"做个视频"}]}],"generation_config":{
				"response_format":[{"video":{"aspect_ratio":"16:9","duration":6}}],"video_config":{"duration_seconds":"9s","frame_rate":24}}}`,
			want: aistudio.VideoConfig{AspectRatio: "16:9", DurationSeconds: 9, Resolution: "4k", FrameRate: 24}, model: "gemini-omni-1.1-flash",
		},
		{
			name: "Interactions response_format 与 video_config", path: "/v1beta/interactions",
			body: `{"model":"gemini-omni-1.1-flash","input":"做个视频","response_format":{"type":"video","aspect_ratio":"9:16","duration":"8s","resolution":"720p"},
				"generation_config":{"stop_sequences":["END"],"video_config":{"task":"text_to_video","duration_seconds":10}}}`,
			want: aistudio.VideoConfig{AspectRatio: "9:16", DurationSeconds: 10, Resolution: "720p", Task: "text_to_video"}, stop: true,
		},
		{
			name: "Gemini fps 与 UNSPECIFIED 枚举名", path: "/v1beta/models/gemini-omni-1.1-flash:generateContent",
			body: `{"contents":[{"role":"user","parts":[{"text":"做个视频"}]}],"generationConfig":{
				"responseFormat":{"video":{"aspectRatio":"ASPECT_RATIO_UNSPECIFIED","fps":24,"duration":"6s"}},
				"videoConfig":{"resolution":"VIDEO_RESOLUTION_UNSPECIFIED","task":"VIDEO_TASK_UNSPECIFIED"}}}`,
			want: aistudio.VideoConfig{DurationSeconds: 6, FrameRate: 24},
		},
		{
			name: "Interactions response_format.fps", path: "/v1beta/interactions",
			body: `{"model":"gemini-omni-1.1-flash","input":"做个视频","response_format":{"type":"video","fps":"24fps","aspect_ratio":"ASPECT_RATIO_UNSPECIFIED","resolution":"720p"}}`,
			want: aistudio.VideoConfig{Resolution: "720p", FrameRate: 24},
		},
		{
			name: "Interactions 模型后缀", path: "/v1beta/interactions",
			body: `{"model":"gemini-omni-1.1-flash-9x16-5s","input":"做个视频","response_format":[{"type":"text"},{"type":"video","aspect_ratio":"16:9"}]}`,
			want: aistudio.VideoConfig{AspectRatio: "9:16", DurationSeconds: 5}, model: "gemini-omni-1.1-flash",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			handler, service := omniHandler(t)
			recorder := postJSON(t, handler, test.path, test.body, test.header)
			if recorder.Code != http.StatusOK || service.last == nil {
				t.Fatalf("状态 %d: %s", recorder.Code, recorder.Body.String())
			}
			request := service.last
			if request.Model != "gemini-omni-1.1-flash" || request.Config.VideoConfig == nil || *request.Config.VideoConfig != test.want {
				t.Fatalf("model=%s video=%+v，期望 %+v", request.Model, request.Config.VideoConfig, test.want)
			}
			if len(request.Config.StopSequences) != 0 {
				t.Fatalf("Omni 的 stop sequences 应清空: %v", request.Config.StopSequences)
			}
			hasStopNote := false
			for _, note := range request.Notes {
				hasStopNote = hasStopNote || note == aistudio.InteractionStopSequencesNote(1)
			}
			if hasStopNote != test.stop {
				t.Fatalf("请求日志说明 %v，期望 stop 说明=%v", request.Notes, test.stop)
			}
			if test.model != "" && !strings.Contains(recorder.Body.String(), `"`+test.model+`"`) {
				t.Fatalf("响应应报告去掉后缀的模型名 %s: %.300s", test.model, recorder.Body.String())
			}
			if strings.Contains(recorder.Body.String(), "gemini-omni-1.1-flash-") {
				t.Fatalf("响应里不应出现带后缀的模型名: %.300s", recorder.Body.String())
			}
		})
	}
}

// TestOmniVideoOfficialCapture 用模型后缀或各协议参数写出官网同样的设置（16:9、10 秒、4K、High），编码出的 CreateInteractionStream
// 请求体与官网抓包逐字节一致（第 5 项 WAA proof 换成 null）
func TestOmniVideoOfficialCapture(t *testing.T) {
	const capture = `[1,1,null,[null,null,null,null,null,null,null,null,null,null,null,null,null,null,null,null,null,` +
		`["models/gemini-omni-1.1-flash",[null,null,null,null,null,4,1,65536,null,null,null,null,null,null,null,null,null,null,null,null,null,null,null,null,[]]],` +
		`null,null,null,null,null,null,null,null,[[[[[[["随机\n"]]]]]]],null,null,null,null,null,null,null,null,null,null,null,null,null,null,null,null,null,` +
		`null,null,null,null,null,null,null,null,null,[[[null,null,null,[null,null,null,1,[10],4]]]]],null,1]`
	defaults := aistudio.GenerationDefaults{
		MaxOutputTokens: 65536, Thinking: true, ThinkingLevel: true, DefaultThinkingLevel: 3,
		ThinkingLevels: []int64{1, 3}, InteractionStream: true, VideoResolution: "360p",
	}
	anthropic := map[string]string{"Anthropic-Version": "2023-06-01"}
	for _, test := range []struct {
		name   string
		path   string
		body   string
		header map[string]string
	}{
		{name: "Gemini 模型后缀", path: "/v1beta/models/gemini-omni-1.1-flash-16x9-4k-10s-high:generateContent",
			body: `{"contents":[{"role":"user","parts":[{"text":"随机\n"}]}]}`},
		{name: "Gemini videoConfig", path: "/v1beta/models/gemini-omni-1.1-flash:generateContent",
			body: `{"contents":[{"role":"user","parts":[{"text":"随机\n"}]}],"generationConfig":{"thinkingConfig":{"thinkingLevel":"HIGH"},
				"videoConfig":{"aspectRatio":"16:9","durationSeconds":10,"resolution":"4k"}}}`},
		{name: "Chat video_config", path: "/v1/chat/completions",
			body: `{"model":"gemini-omni-1.1-flash","reasoning_effort":"high","messages":[{"role":"user","content":"随机\n"}],
				"video_config":{"aspect_ratio":"16:9","duration_seconds":10,"resolution":"4K"}}`},
		{name: "Anthropic 模型后缀", path: "/v1/messages", header: anthropic,
			body: `{"model":"gemini-omni-1.1-flash-high-16x9-10s-4k","max_tokens":65536,"messages":[{"role":"user","content":"随机\n"}]}`},
		{name: "Interactions response_format", path: "/v1beta/interactions",
			body: `{"model":"gemini-omni-1.1-flash-high","input":"随机\n","response_format":{"type":"video","aspect_ratio":"16:9","duration":"10s","resolution":"4k"}}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			handler, service := omniHandler(t)
			recorder := postJSON(t, handler, test.path, test.body, test.header)
			if recorder.Code != http.StatusOK || service.last == nil {
				t.Fatalf("状态 %d: %s", recorder.Code, recorder.Body.String())
			}
			body, _, err := aistudio.EncodeCreateInteractionStreamRequest(*service.last, defaults)
			if err != nil {
				t.Fatal(err)
			}
			if string(body) != capture {
				t.Fatalf("请求体与官网抓包不一致:\n得到 %s\n期望 %s", body, capture)
			}
		})
	}
}

// TestOmniVideoProtocolErrors 视频设置不成立时在选号前返回 400 与清楚的英文说明：帧率不是 24fps、非第一轮指定任务（与官网提示一致）、
// 任务缺少需要的输入、时长写法不对、未知任务
func TestOmniVideoProtocolErrors(t *testing.T) {
	for _, test := range []struct {
		name string
		path string
		body string
		want string
	}{
		{name: "帧率 30", path: "/v1/chat/completions", body: `{"model":"gemini-omni-1.1-flash","messages":[{"role":"user","content":"hi"}],"video_config":{"frame_rate":30}}`, want: "only supports 24fps"},
		{name: "非第一轮任务", path: "/v1/chat/completions", body: `{"model":"gemini-omni-1.1-flash","messages":[{"role":"user","content":"做个视频"},{"role":"assistant","content":"好的"},{"role":"user","content":"再来"}],"video_config":{"task":"text_to_video"}}`, want: "Video task is only supported on the first turn of a conversation"},
		{name: "后缀任务非第一轮", path: "/v1beta/models/gemini-omni-1.1-flash-t2v:generateContent", body: `{"contents":[{"role":"user","parts":[{"text":"a"}]},{"role":"model","parts":[{"text":"b"}]},{"role":"user","parts":[{"text":"c"}]}]}`, want: "first turn"},
		{name: "编辑任务没有视频", path: "/v1/messages", body: `{"model":"gemini-omni-1.1-flash-edit","max_tokens":16,"messages":[{"role":"user","content":"改一下"}]}`, want: "requires an input video"},
		{name: "图片任务没有图片", path: "/v1/responses", body: `{"model":"gemini-omni-1.1-flash","input":"动起来","video_config":{"task":"image_to_video"}}`, want: "requires an input image"},
		{name: "时长写法", path: "/v1/chat/completions", body: `{"model":"gemini-omni-1.1-flash","messages":[{"role":"user","content":"hi"}],"video_config":{"duration":"eight"}}`, want: "video duration"},
		{name: "负数时长", path: "/v1beta/interactions", body: `{"model":"gemini-omni-1.1-flash","input":"hi","response_format":{"type":"video","duration":-2}}`, want: "non-negative"},
		{name: "未知任务", path: "/v1beta/interactions", body: `{"model":"gemini-omni-1.1-flash","input":"hi","generation_config":{"video_config":{"task":"remix"}}}`, want: "is not valid; use text_to_video"},
		{name: "宽高比", path: "/v1beta/models/gemini-omni-1.1-flash:generateContent", body: `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"videoConfig":{"aspectRatio":"portrait"}}}`, want: "aspect_ratio"},
		{name: "Gemini videoConfig.fps 30", path: "/v1beta/models/gemini-omni-1.1-flash:generateContent", body: `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"videoConfig":{"fps":30}}}`, want: "only supports 24fps"},
		{name: "Gemini responseFormat.video.fps 30", path: "/v1beta/models/gemini-omni-1.1-flash:generateContent", body: `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generation_config":{"response_format":{"video":{"fps":"30fps"}}}}`, want: "only supports 24fps"},
		{name: "Interactions response_format.fps 30", path: "/v1beta/interactions", body: `{"model":"gemini-omni-1.1-flash","input":"hi","response_format":{"type":"video","fps":30}}`, want: "only supports 24fps"},
		{name: "Responses video_config.fps 25", path: "/v1/responses", body: `{"model":"gemini-omni-1.1-flash","input":"hi","video_config":{"fps":25}}`, want: "only supports 24fps"},
		{name: "目录别名的后缀任务非第一轮", path: "/v1/chat/completions", body: `{"model":"omni-video-latest-t2v","messages":[{"role":"user","content":"a"},{"role":"assistant","content":"b"},{"role":"user","content":"c"}]}`, want: "first turn"},
	} {
		t.Run(test.name, func(t *testing.T) {
			handler, service := omniHandler(t)
			var header map[string]string
			if strings.HasPrefix(test.path, "/v1/messages") {
				header = map[string]string{"Anthropic-Version": "2023-06-01"}
			}
			recorder := postJSON(t, handler, test.path, test.body, header)
			if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), test.want) {
				t.Fatalf("状态 %d: %s，期望 400 且含 %q", recorder.Code, recorder.Body.String(), test.want)
			}
			if service.last != nil {
				t.Fatal("参数错误应在交给生成服务前返回")
			}
		})
	}
}

// TestOmniVideoOtherModelsUnaffected 其他模型不受视频设置与视频后缀影响：文本模型的 stop sequences 照常保留（本地匹配），不留说明；
// Veo 同样列出视频选项，但不是 Omni：模型名不改写、stop sequences 保留、视频设置原样交给生成服务（不整理也不校验）；
// 时长超出 Omni 范围时按范围取最接近的合法值并留下说明
func TestOmniVideoOtherModelsUnaffected(t *testing.T) {
	handler, service := omniHandler(t)
	recorder := postJSON(t, handler, "/v1/chat/completions", `{"model":"gemini-4-argon-low","stop":["END"],"messages":[{"role":"user","content":"hi"}]}`, nil)
	if recorder.Code != http.StatusOK || service.last.Model != "gemini-4-argon" || len(service.last.Config.StopSequences) != 1 || len(service.last.Notes) != 0 {
		t.Fatalf("文本模型: %d %+v", recorder.Code, service.last)
	}
	recorder = postJSON(t, handler, "/v1/chat/completions", `{"model":"gemini-4-argon-720p","messages":[{"role":"user","content":"hi"}]}`, nil)
	if recorder.Code != http.StatusOK || service.last.Model != "gemini-4-argon-720p" {
		t.Fatalf("文本模型带视频后缀应保持原名: %d %s", recorder.Code, service.last.Model)
	}
	for _, test := range []struct{ path, body, model string }{
		{path: "/v1/chat/completions", model: "veo-3.1-generate-preview",
			body: `{"model":"veo-3.1-generate-preview","stop":["END"],"messages":[{"role":"user","content":"hi"}],"video_config":{"aspect_ratio":"9x16","duration":40,"frame_rate":30}}`},
		{path: "/v1/chat/completions", model: "veo-3.1-generate-preview-8s",
			body: `{"model":"veo-3.1-generate-preview-8s","stop":["END"],"messages":[{"role":"user","content":"hi"}],"video_config":{"aspect_ratio":"9x16","duration":40,"frame_rate":30}}`},
		{path: "/v1beta/models/veo-3.1-generate-preview-720p:generateContent", model: "veo-3.1-generate-preview-720p",
			body: `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"stopSequences":["END"],"videoConfig":{"aspectRatio":"9x16","durationSeconds":40,"fps":30}}}`},
	} {
		service.last = nil
		recorder = postJSON(t, handler, test.path, test.body, nil)
		if recorder.Code != http.StatusOK || service.last == nil {
			t.Fatalf("Veo %s: %d %s", test.model, recorder.Code, recorder.Body.String())
		}
		want := aistudio.VideoConfig{AspectRatio: "9x16", DurationSeconds: 40, FrameRate: 30}
		if service.last.Model != test.model || !slices.Equal(service.last.Config.StopSequences, []string{"END"}) || len(service.last.Notes) != 0 ||
			service.last.Config.VideoConfig == nil || *service.last.Config.VideoConfig != want {
			t.Fatalf("Veo %s 不应按 Omni 处理: model=%s stop=%v notes=%v video=%+v", test.model, service.last.Model,
				service.last.Config.StopSequences, service.last.Notes, service.last.Config.VideoConfig)
		}
	}
	recorder = postJSON(t, handler, "/v1/chat/completions", `{"model":"gemini-omni-1.1-flash-30s","messages":[{"role":"user","content":"hi"}]}`, nil)
	if recorder.Code != http.StatusOK || service.last.Config.VideoConfig == nil || service.last.Config.VideoConfig.DurationSeconds != 15 ||
		len(service.last.Notes) != 1 || !strings.Contains(service.last.Notes[0], "按最接近的 15 秒发送") {
		t.Fatalf("超出范围的时长: %d %+v %v", recorder.Code, service.last.Config.VideoConfig, service.last.Notes)
	}
	// Omni 不带等级后缀时沿用模型默认等级与客户端参数，不强制最高
	recorder = postJSON(t, handler, "/v1/chat/completions", `{"model":"gemini-omni-1.1-flash","reasoning_effort":"low","messages":[{"role":"user","content":"hi"}]}`, nil)
	if recorder.Code != http.StatusOK || service.last.Config.ReasoningEffort != "low" || service.last.Config.VideoConfig != nil {
		t.Fatalf("Omni 不带后缀: %+v", service.last.Config)
	}
	recorder = postJSON(t, handler, "/v1/chat/completions", `{"model":"gemini-omni-1.1-flash-high","reasoning_effort":"low","messages":[{"role":"user","content":"hi"}]}`, nil)
	if recorder.Code != http.StatusOK || service.last.Config.ReasoningEffort != "high" {
		t.Fatalf("Omni 等级后缀应优先于参数: %+v", service.last.Config)
	}
}

// TestVideoSecondsParsing 时长接受数字与 "8s"、"8" 字符串并四舍五入，帧率接受 24 与 "24fps"
func TestVideoSecondsParsing(t *testing.T) {
	for raw, want := range map[string]int64{`8`: 8, `"8s"`: 8, `" 10 "`: 10, `7.5`: 8, `"6.4s"`: 6, `null`: 0, `""`: 0} {
		var value videoSeconds
		if err := json.Unmarshal([]byte(raw), &value); err != nil || value.seconds != want {
			t.Fatalf("%s 解析为 %+v err=%v，期望 %d", raw, value, err, want)
		}
	}
	for _, raw := range []string{`-1`, `"abc"`, `true`, `[8]`, `"1e9"`} {
		var value videoSeconds
		if err := json.Unmarshal([]byte(raw), &value); err == nil {
			t.Fatalf("%s 应返回错误", raw)
		}
	}
	for raw, want := range map[string]float64{`24`: 24, `"24fps"`: 24, `"24"`: 24, `23.976`: 23.976, `null`: 0} {
		var rate videoFrameRate
		if err := json.Unmarshal([]byte(raw), &rate); err != nil || rate.value != want {
			t.Fatalf("%s 解析为 %+v err=%v", raw, rate, err)
		}
	}
	for _, raw := range []string{`0`, `"fast"`, `{}`} {
		var rate videoFrameRate
		if err := json.Unmarshal([]byte(raw), &rate); err == nil {
			t.Fatalf("%s 应返回错误", raw)
		}
	}
}

// TestGeminiResponseFormatText responseFormat 的文本与图片格式与 responseMimeType、responseSchema、imageConfig 等价（枚举名换成上游取值），
// snake_case 写法里用户 Schema 的属性名原样保留；responseFormat 为 null 时按未设置处理
func TestGeminiResponseFormatText(t *testing.T) {
	handler, service := omniHandler(t)
	body := `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generation_config":{"response_format":[
		{"text":{"mime_type":"APPLICATION_JSON","schema":{"type":"object","properties":{"user_name":{"type":"string"}}}}},
		{"image":{"aspect_ratio":"ASPECT_RATIO_SIXTEEN_BY_NINE","image_size":"IMAGE_SIZE_TWO_K"}}]}}`
	recorder := postJSON(t, handler, "/v1beta/models/gemini-4-argon:generateContent", body, nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("状态 %d: %s", recorder.Code, recorder.Body.String())
	}
	config := service.last.Config
	if config.ResponseMIMEType != "application/json" || !strings.Contains(string(config.ResponseSchema), `"user_name"`) ||
		config.ImageConfig == nil || *config.ImageConfig != (aistudio.ImageConfig{AspectRatio: "16:9", ImageSize: "2K"}) || config.VideoConfig != nil {
		t.Fatalf("responseFormat 映射: %+v schema=%s image=%+v", config, config.ResponseSchema, config.ImageConfig)
	}
	recorder = postJSON(t, handler, "/v1beta/models/gemini-4-argon:generateContent", `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"responseFormat":null}}`, nil)
	if recorder.Code != http.StatusOK || service.last.Config.ResponseMIMEType != "" {
		t.Fatalf("responseFormat=null: %d %s", recorder.Code, recorder.Body.String())
	}
}

// TestOmniVideoCatalogAlias 按 Omni 的目录别名调用时视频后缀同样生效：请求模型为去掉后缀的别名（调度前再换成目录 ID），
// 视频设置与 stop sequences 在协议层整理并留下说明，响应报告去掉后缀的别名
func TestOmniVideoCatalogAlias(t *testing.T) {
	handler, service := omniHandler(t)
	recorder := postJSON(t, handler, "/v1/chat/completions",
		`{"model":"omni-video-latest-9x16-720p","stop":["END"],"messages":[{"role":"user","content":"做个视频"}],"video_config":{"duration":40}}`, nil)
	if recorder.Code != http.StatusOK || service.last == nil {
		t.Fatalf("状态 %d: %s", recorder.Code, recorder.Body.String())
	}
	request := service.last
	if want := (aistudio.VideoConfig{AspectRatio: "9:16", Resolution: "720p", DurationSeconds: 15}); request.Model != "omni-video-latest" ||
		request.Config.VideoConfig == nil || *request.Config.VideoConfig != want || len(request.Config.StopSequences) != 0 {
		t.Fatalf("model=%s video=%+v stop=%v", request.Model, request.Config.VideoConfig, request.Config.StopSequences)
	}
	if !slices.Contains(request.Notes, aistudio.InteractionStopSequencesNote(1)) || len(request.Notes) != 2 {
		t.Fatalf("请求日志说明 %v", request.Notes)
	}
	if body := recorder.Body.String(); !strings.Contains(body, `"omni-video-latest"`) || strings.Contains(body, "omni-video-latest-") {
		t.Fatalf("响应应报告去掉后缀的别名: %.300s", body)
	}
}

// countingOmniService 带 Omni 目录、记录最后一次计数请求
type countingOmniService struct {
	modelListService
	last *aistudio.TokenCountRequest
}

func (service *countingOmniService) CountTokens(_ context.Context, request aistudio.TokenCountRequest) (aistudio.TokenCount, error) {
	copied := request
	service.last = &copied
	return aistudio.TokenCount{InputTokens: 7}, nil
}

// TestOmniVideoCountTokensSuffix Gemini countTokens 与 Anthropic count_tokens 同样去掉 Omni 的视频与思考后缀、把目录别名换成目录 ID，
// 交给计数服务的是目录模型名；非 Omni 模型带视频字样时保持原名
func TestOmniVideoCountTokensSuffix(t *testing.T) {
	service := &countingOmniService{modelListService: modelListService{models: omniAliasModels()}}
	handler, _ := newMediaHandler(t, service, &mediaSettingsStub{})
	anthropic := map[string]string{"Anthropic-Version": "2023-06-01"}
	gemini := `{"contents":[{"role":"user","parts":[{"text":"做个视频"}]}]}`
	for _, test := range []struct {
		path   string
		body   string
		header map[string]string
		want   string
	}{
		{path: "/v1beta/models/gemini-omni-1.1-flash-9x16-720p:countTokens", body: gemini, want: "gemini-omni-1.1-flash"},
		{path: "/v1beta/models/omni-video-latest-8s-low:countTokens", body: gemini, want: "gemini-omni-1.1-flash"},
		{path: "/v1beta/models/veo-3.1-generate-preview-8s:countTokens", body: gemini, want: "veo-3.1-generate-preview-8s"},
		{path: "/v1/messages/count_tokens", header: anthropic, want: "gemini-omni-1.1-flash",
			body: `{"model":"gemini-omni-1.1-flash-16x9-1080p-6s-high","messages":[{"role":"user","content":"做个视频"}]}`},
		{path: "/v1/messages/count_tokens", header: anthropic, want: "gemini-4-argon-720p",
			body: `{"model":"gemini-4-argon-720p","messages":[{"role":"user","content":"hi"}]}`},
	} {
		service.last = nil
		recorder := postJSON(t, handler, test.path, test.body, test.header)
		if recorder.Code != http.StatusOK || service.last == nil {
			t.Fatalf("%s: %d %s", test.path, recorder.Code, recorder.Body.String())
		}
		if service.last.Model != test.want {
			t.Fatalf("%s %s: 计数模型 %q，期望 %q", test.path, test.body, service.last.Model, test.want)
		}
	}
}
