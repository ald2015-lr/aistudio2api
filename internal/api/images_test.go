package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// TestOpenAIImageConfigSizes size 与 quality 的映射：原先的三种尺寸照原样只写宽高比；其他 WIDTHxHEIGHT 按目录（没有时按 Gemini 通用的
// 10 种）取最接近的宽高比，按面积取目录里最接近的分辨率档（1K 不写，目录没有分辨率选项时不写），不精确时写说明；quality 优先；
// 写法不对的 size 仍返回参数错误
func TestOpenAIImageConfigSizes(t *testing.T) {
	catalog := map[string][]string{
		"image_aspect_ratios":      {"1:1", "2:3", "3:2", "3:4", "4:3", "4:5", "5:4", "9:16", "16:9", "21:9"},
		"image_output_resolutions": {"512", "1K", "2K", "4K"},
	}
	narrow := map[string][]string{"image_aspect_ratios": {"1:1", "16:9", "9:16"}}
	for _, test := range []struct {
		name, size, quality string
		options             map[string][]string
		ratio, resolution   string
		notes               []string
	}{
		{name: "auto", size: "auto"},
		{name: "空", size: ""},
		{name: "原先的 1024x1024", size: "1024x1024", options: catalog, ratio: "1:1"},
		{name: "原先的 1536x1024 不受目录影响", size: "1536x1024", options: narrow, ratio: "3:2"},
		{name: "原先的 1024x1536", size: "1024X1536", ratio: "2:3"},
		{name: "DALL·E 竖图没有目录", size: "1024x1792", ratio: "9:16",
			notes: []string{"图片 size 1024x1792 按最接近的宽高比 9:16 发送（不缩放、不裁切）"}},
		{name: "DALL·E 横图 1K 不写", size: "1792x1024", options: catalog, ratio: "16:9",
			notes: []string{"图片 size 1792x1024 按最接近的宽高比 16:9 发送（不缩放、不裁切）"}},
		{name: "2K 方图", size: "2048x2048", options: catalog, ratio: "1:1", resolution: "2K",
			notes: []string{"图片 size 2048x2048 按最接近的分辨率 2K 发送"}},
		{name: "4K 横图", size: "3840x2160", options: catalog, ratio: "16:9", resolution: "2K",
			notes: []string{"图片 size 3840x2160 按最接近的分辨率 2K 发送"}},
		{name: "精确的 4:5、面积接近 2K", size: "1600x2000", options: catalog, ratio: "4:5", resolution: "2K",
			notes: []string{"图片 size 1600x2000 按最接近的分辨率 2K 发送"}},
		{name: "超宽图", size: "2560x1080", options: catalog, ratio: "21:9", resolution: "2K",
			notes: []string{"图片 size 2560x1080 按最接近的宽高比 21:9 发送（不缩放、不裁切）", "图片 size 2560x1080 按最接近的分辨率 2K 发送"}},
		{name: "目录没有分辨率选项", size: "2560x1080", ratio: "21:9",
			notes: []string{"图片 size 2560x1080 按最接近的宽高比 21:9 发送（不缩放、不裁切）"}},
		{name: "4K 方图", size: "4096x4096", options: catalog, ratio: "1:1", resolution: "4K",
			notes: []string{"图片 size 4096x4096 按最接近的分辨率 4K 发送"}},
		{name: "512 方图", size: "512x512", options: catalog, ratio: "1:1", resolution: "512",
			notes: []string{"图片 size 512x512 按最接近的分辨率 512 发送"}},
		{name: "quality 优先", size: "2048x2048", quality: "high", options: catalog, ratio: "1:1", resolution: "4K"},
		{name: "目录只有三种宽高比", size: "1200x1000", options: narrow, ratio: "1:1",
			notes: []string{"图片 size 1200x1000 按最接近的宽高比 1:1 发送（不缩放、不裁切）"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			config, notes, err := openAIImageConfig(test.size, test.quality, test.options)
			if err != nil {
				t.Fatal(err)
			}
			ratio, resolution := "", ""
			if config != nil {
				ratio, resolution = config.AspectRatio, config.ImageSize
			}
			if ratio != test.ratio || resolution != test.resolution || strings.Join(notes, "|") != strings.Join(test.notes, "|") {
				t.Fatalf("size=%s quality=%s：宽高比 %q 分辨率 %q 说明 %q；期望 %q %q %q",
					test.size, test.quality, ratio, resolution, notes, test.ratio, test.resolution, test.notes)
			}
		})
	}
	for _, size := range []string{"big", "0x100", "1024", "1024x", "x1024", "99999x10"} {
		if _, _, err := openAIImageConfig(size, "", nil); err == nil {
			t.Errorf("size %q 应返回参数错误", size)
		}
	}
}

// imageResultService 返回一张 PNG、一段改写后的提示词与用量，并记录生成请求
type imageResultService struct {
	modelListService
	last *aistudio.GenerateRequest
}

func (service *imageResultService) Generate(_ context.Context, request aistudio.GenerateRequest) (<-chan aistudio.Event, error) {
	copied := request
	service.last = &copied
	events := make(chan aistudio.Event, 3)
	events <- aistudio.Event{Kind: aistudio.EventMedia, Media: &aistudio.Media{MIME: "image/png", Data: attachmentPNG}}
	events <- aistudio.Event{Kind: aistudio.EventText, Text: "a cat"}
	events <- aistudio.Event{Kind: aistudio.EventFinish, FinishReason: "stop",
		Usage: &aistudio.Usage{InputTokens: 12, OutputTokens: 1290, TotalTokens: 1302}}
	close(events)
	return events, nil
}

// TestOpenAIImagesResponse 默认每张图同时带 b64_json 与 url（data URI，同一份数据），response_format 明确要求时只给那一种；
// 带 usage（gpt-image 写法）；任意 size 不再 400；客户端写了 output_format 时响应写实际格式，不是 png 时请求日志说明不转码
func TestOpenAIImagesResponse(t *testing.T) {
	service := &imageResultService{modelListService: modelListService{models: []aistudio.Model{{
		ID: "gemini-3.1-flash-image", Methods: []string{"generateContent"}, Capabilities: map[string]bool{"chat_model": true},
		CapabilityOptions: map[string][]string{"image_aspect_ratios": {"1:1", "9:16", "16:9"}, "image_output_resolutions": {"1K", "2K"}},
	}}}}
	handler := NewHandler(service, Config{APIKey: "sk-test"})
	encoded := base64.StdEncoding.EncodeToString(attachmentPNG)
	post := func(body string) map[string]any {
		t.Helper()
		recorder := postJSON(t, handler, "/v1/images/generations", body, nil)
		var response map[string]any
		if recorder.Code != http.StatusOK || json.Unmarshal(recorder.Body.Bytes(), &response) != nil {
			t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
		}
		return response
	}
	item := func(response map[string]any) map[string]any {
		data, _ := response["data"].([]any)
		if len(data) != 1 {
			t.Fatalf("data = %v", response["data"])
		}
		return data[0].(map[string]any)
	}

	response := post(`{"model":"gemini-3.1-flash-image","prompt":"cat","size":"1024x1792"}`)
	image := item(response)
	if image["b64_json"] != encoded || image["url"] != "data:image/png;base64,"+encoded || image["revised_prompt"] != "a cat" {
		t.Fatalf("默认应同时带 b64_json 与 url: %v", image)
	}
	usage, _ := response["usage"].(map[string]any)
	details, _ := usage["input_tokens_details"].(map[string]any)
	if usage["input_tokens"] != float64(12) || usage["output_tokens"] != float64(1290) || usage["total_tokens"] != float64(1302) ||
		details["text_tokens"] != float64(12) || details["image_tokens"] != float64(0) {
		t.Fatalf("usage = %v", response["usage"])
	}
	if _, ok := response["output_format"]; ok {
		t.Fatalf("没有写 output_format 时响应不带它: %v", response)
	}
	if config := service.last.Config.ImageConfig; config == nil || config.AspectRatio != "9:16" || config.ImageSize != "" {
		t.Fatalf("1024x1792 应按 9:16 发送: %+v", config)
	}
	if strings.Join(service.last.Notes, "|") != "图片 size 1024x1792 按最接近的宽高比 9:16 发送（不缩放、不裁切）" {
		t.Fatalf("说明 = %q", service.last.Notes)
	}

	if only := item(post(`{"model":"gemini-3.1-flash-image","prompt":"cat","response_format":"b64_json"}`)); only["b64_json"] != encoded || only["url"] != nil {
		t.Fatalf("response_format=b64_json 只给 b64_json: %v", only)
	}
	if only := item(post(`{"model":"gemini-3.1-flash-image","prompt":"cat","response_format":"url"}`)); only["url"] == nil || only["b64_json"] != nil {
		t.Fatalf("response_format=url 只给 url: %v", only)
	}

	response = post(`{"model":"gemini-3.1-flash-image","prompt":"cat","size":"1024x1024","output_format":"jpeg","output_compression":80,"background":"auto","moderation":"low"}`)
	if response["output_format"] != "png" {
		t.Fatalf("写了 output_format 时响应应写实际格式 png: %v", response["output_format"])
	}
	if config := service.last.Config.ImageConfig; config == nil || config.AspectRatio != "1:1" || config.ImageSize != "" {
		t.Fatalf("1024x1024 照原样只写 1:1: %+v", config)
	}
	if len(service.last.Notes) != 1 || !strings.Contains(service.last.Notes[0], "output_format=jpeg") {
		t.Fatalf("output_format 不是 png 时应说明不转码: %q", service.last.Notes)
	}
}
