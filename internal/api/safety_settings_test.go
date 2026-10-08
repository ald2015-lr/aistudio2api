package api

import (
	"net/http"
	"reflect"
	"testing"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// TestGeminiSafetySettingsAndMediaResolution Gemini 请求的 safetySettings 与 mediaResolution 原样进入生成请求，camelCase 与 snake_case 两种写法都接受
func TestGeminiSafetySettingsAndMediaResolution(t *testing.T) {
	want := []aistudio.SafetySetting{{Category: "HARM_CATEGORY_HARASSMENT", Threshold: "BLOCK_ONLY_HIGH"}}
	for name, raw := range map[string]string{
		"camelCase": `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],
			"safetySettings":[{"category":"HARM_CATEGORY_HARASSMENT","threshold":"BLOCK_ONLY_HIGH"}],
			"generationConfig":{"mediaResolution":"MEDIA_RESOLUTION_LOW"}}`,
		"snake_case": `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],
			"safety_settings":[{"category":"HARM_CATEGORY_HARASSMENT","threshold":"BLOCK_ONLY_HIGH"}],
			"generation_config":{"media_resolution":"MEDIA_RESOLUTION_LOW"}}`,
		"generationConfig 内 snake_case": `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],
			"safety_settings":[{"category":"HARM_CATEGORY_HARASSMENT","threshold":"BLOCK_ONLY_HIGH"}],
			"generationConfig":{"media_resolution":"MEDIA_RESOLUTION_LOW"}}`,
		"countTokens 包装": `{"generateContentRequest":{"contents":[{"role":"user","parts":[{"text":"hi"}]}],
			"safetySettings":[{"category":"HARM_CATEGORY_HARASSMENT","threshold":"BLOCK_ONLY_HIGH"}],
			"generationConfig":{"mediaResolution":"MEDIA_RESOLUTION_LOW"}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			var request geminiRequest
			decodeInto(t, raw, &request)
			if err := request.normalizeVariants(); err != nil {
				t.Fatal(err)
			}
			generate, err := request.toGenerateRequest("id", "test-model")
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(generate.SafetySettings, want) {
				t.Fatalf("安全设置=%+v，期望 %+v", generate.SafetySettings, want)
			}
			if generate.Config.MediaResolution != "MEDIA_RESOLUTION_LOW" {
				t.Fatalf("媒体分辨率=%q", generate.Config.MediaResolution)
			}
		})
	}
}

// TestGeminiInvalidSafetyAndMediaResolution 媒体分辨率写错或安全设置类型不对时在调用生成服务前返回 400；未知类别照常进入生成服务
func TestGeminiInvalidSafetyAndMediaResolution(t *testing.T) {
	const path = "/v1beta/models/test-model:generateContent"
	for name, body := range map[string]string{
		"媒体分辨率写错":                 `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"mediaResolution":"MEDIA_RESOLUTION_ULTRA"}}`,
		"snake_case 媒体分辨率写错":      `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generation_config":{"media_resolution":"high"}}`,
		"safetySettings 不是数组":     `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"safetySettings":{"category":"HARM_CATEGORY_HARASSMENT"}}`,
		"safety_settings 不是数组":    `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"safety_settings":"OFF"}`,
		"threshold 不是字符串":         `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"safetySettings":[{"category":"HARM_CATEGORY_HARASSMENT","threshold":5}]}`,
		"mediaResolution 不是字符串":   `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"mediaResolution":2}}`,
		"media_resolution 不是字符串":  `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"media_resolution":["LOW"]}}`,
		"generation_config 内类型不对": `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generation_config":{"media_resolution":1}}`,
	} {
		t.Run(name, func(t *testing.T) {
			service := &scriptedService{events: []aistudio.Event{{Kind: aistudio.EventFinish, FinishReason: "STOP"}}}
			recorder := postJSON(t, NewHandler(service, Config{APIKey: "sk-test"}), path, body, nil)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
			}
			if service.last != nil {
				t.Fatal("参数错误不应进入生成服务")
			}
		})
	}
	service := &scriptedService{events: []aistudio.Event{{Kind: aistudio.EventText, Text: "ok"}, {Kind: aistudio.EventFinish, FinishReason: "STOP"}}}
	recorder := postJSON(t, NewHandler(service, Config{APIKey: "sk-test"}), path,
		`{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"safetySettings":[{"category":"HARM_CATEGORY_TOXICITY","threshold":"BLOCK_NONE"}]}`, nil)
	if recorder.Code != http.StatusOK || service.last == nil || len(service.last.SafetySettings) != 1 {
		t.Fatalf("未知类别不应返回 400: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}
