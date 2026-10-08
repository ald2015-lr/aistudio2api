package aistudio

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// safetyTestRequest 返回只有一条用户消息的最小生成请求
func safetyTestRequest(settings []SafetySetting) GenerateRequest {
	return GenerateRequest{
		Model:          "test-model",
		Contents:       []Content{{Role: RoleUser, Parts: []Part{{Text: "hi"}}}},
		SafetySettings: settings,
	}
}

// encodePlaygroundRoot 编码 Playground 请求并解出根数组
func encodePlaygroundRoot(t *testing.T, request GenerateRequest, defaults GenerationDefaults) []any {
	t.Helper()
	body, err := EncodeGenerateContentRequest(request, defaults, RequestContext{})
	if err != nil {
		t.Fatalf("Playground 编码失败: %v", err)
	}
	var root []any
	if err := json.Unmarshal(body, &root); err != nil {
		t.Fatal(err)
	}
	return root
}

// encodeBuildBody 编码 Build 请求并解出请求体
func encodeBuildBody(t *testing.T, request GenerateRequest, defaults GenerationDefaults, imageRoute bool) map[string]any {
	t.Helper()
	_, body, err := EncodeBuildGenerateRequest(request, defaults, imageRoute, false)
	if err != nil {
		t.Fatalf("Build 编码失败: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatal(err)
	}
	return decoded
}

// playgroundSafety 把根字段 3 换成 [类别, 阈值] 编号对，便于比较
func playgroundSafety(t *testing.T, root []any) [][2]float64 {
	t.Helper()
	if root[2] == nil {
		return nil
	}
	var pairs [][2]float64
	for _, item := range root[2].([]any) {
		entry := item.([]any)
		if len(entry) != 4 || entry[0] != nil || entry[1] != nil {
			t.Fatalf("安全设置条目形状不对: %#v", entry)
		}
		pairs = append(pairs, [2]float64{entry[2].(float64), entry[3].(float64)})
	}
	return pairs
}

// buildSafety 把 Build 请求体的 safetySettings 换成“类别=阈值”列表
func buildSafety(body map[string]any) []string {
	raw, ok := body["safetySettings"].([]any)
	if !ok {
		return nil
	}
	var pairs []string
	for _, item := range raw {
		setting := item.(map[string]any)
		pairs = append(pairs, setting["category"].(string)+"="+setting["threshold"].(string))
	}
	return pairs
}

// TestSafetySettingsDefaultOff 客户端没有传安全设置时，两个通道都按官网发送四类 OFF，Playground 普通请求仍为 11 项
func TestSafetySettingsDefaultOff(t *testing.T) {
	defaults := GenerationDefaults{MaxOutputTokens: 1024}
	root := encodePlaygroundRoot(t, safetyTestRequest(nil), defaults)
	if len(root) != 11 {
		t.Fatalf("普通请求根数组应为 11 项，得到 %d", len(root))
	}
	if got, want := playgroundSafety(t, root), [][2]float64{{7, 5}, {8, 5}, {9, 5}, {10, 5}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Playground 默认安全设置=%v，期望 %v", got, want)
	}
	want := []string{
		"HARM_CATEGORY_HARASSMENT=OFF", "HARM_CATEGORY_HATE_SPEECH=OFF",
		"HARM_CATEGORY_SEXUALLY_EXPLICIT=OFF", "HARM_CATEGORY_DANGEROUS_CONTENT=OFF",
	}
	if got := buildSafety(encodeBuildBody(t, safetyTestRequest(nil), defaults, false)); !reflect.DeepEqual(got, want) {
		t.Fatalf("Build 默认安全设置=%v，期望 %v", got, want)
	}
	// 图片模型不带默认类别
	image := GenerationDefaults{MaxOutputTokens: 1024, ImageRoute: true}
	if root := encodePlaygroundRoot(t, safetyTestRequest(nil), image); root[2] != nil {
		t.Fatalf("图片模型不应发送默认安全设置: %#v", root[2])
	}
	if body := encodeBuildBody(t, safetyTestRequest(nil), defaults, true); body["safetySettings"] != nil {
		t.Fatalf("图片路由不应发送默认安全设置: %#v", body["safetySettings"])
	}
}

// TestSafetySettingsClientOverride 客户端阈值只覆盖对应类别，其余三类仍为 OFF；其他已知类别按编号追加，名称不区分大小写
func TestSafetySettingsClientOverride(t *testing.T) {
	defaults := GenerationDefaults{MaxOutputTokens: 1024}
	settings := []SafetySetting{
		{Category: "HARM_CATEGORY_CIVIC_INTEGRITY", Threshold: "BLOCK_NONE"},
		{Category: " harm_category_harassment ", Threshold: "block_only_high"},
	}
	request := safetyTestRequest(settings)
	root := encodePlaygroundRoot(t, request, defaults)
	if got, want := playgroundSafety(t, root), [][2]float64{{7, 3}, {8, 5}, {9, 5}, {10, 5}, {11, 4}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Playground 安全设置=%v，期望 %v", got, want)
	}
	want := []string{
		"HARM_CATEGORY_HARASSMENT=BLOCK_ONLY_HIGH", "HARM_CATEGORY_HATE_SPEECH=OFF",
		"HARM_CATEGORY_SEXUALLY_EXPLICIT=OFF", "HARM_CATEGORY_DANGEROUS_CONTENT=OFF",
		"HARM_CATEGORY_CIVIC_INTEGRITY=BLOCK_NONE",
	}
	if got := buildSafety(encodeBuildBody(t, request, defaults, false)); !reflect.DeepEqual(got, want) {
		t.Fatalf("Build 安全设置=%v，期望 %v", got, want)
	}
	if settings[1].Category != " harm_category_harassment " {
		t.Fatal("编码不应改写调用方的安全设置切片")
	}
	// 图片模型只发送客户端列出的类别
	image := GenerationDefaults{MaxOutputTokens: 1024, ImageRoute: true}
	if got, want := playgroundSafety(t, encodePlaygroundRoot(t, request, image)), [][2]float64{{7, 3}, {11, 4}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("图片模型安全设置=%v，期望 %v", got, want)
	}
}

// TestSafetySettingsUnknownSkipped 未知类别或阈值跳过并给出说明，不导致编码失败；HARM_BLOCK_THRESHOLD_UNSPECIFIED 沿用默认值
func TestSafetySettingsUnknownSkipped(t *testing.T) {
	settings := []SafetySetting{
		{Category: "HARM_CATEGORY_TOXICITY", Threshold: "BLOCK_NONE"},
		{Category: "HARM_CATEGORY_HATE_SPEECH", Threshold: "BLOCK_SOMETIMES"},
		{Category: "HARM_CATEGORY_SEXUALLY_EXPLICIT", Threshold: "HARM_BLOCK_THRESHOLD_UNSPECIFIED"},
		{Category: "HARM_CATEGORY_DANGEROUS_CONTENT", Threshold: "BLOCK_LOW_AND_ABOVE"},
	}
	normalized, skipped := NormalizeSafetySettings(settings)
	if len(skipped) != 2 || !strings.Contains(skipped[0], "HARM_CATEGORY_TOXICITY") || !strings.Contains(skipped[1], "BLOCK_SOMETIMES") {
		t.Fatalf("跳过说明=%q", skipped)
	}
	if want := []SafetySetting{{Category: "HARM_CATEGORY_DANGEROUS_CONTENT", Threshold: "BLOCK_LOW_AND_ABOVE"}}; !reflect.DeepEqual(normalized, want) {
		t.Fatalf("保留的安全设置=%+v，期望 %+v", normalized, want)
	}
	// 编码层直接收到未清理的设置时同样跳过，不返回错误
	defaults := GenerationDefaults{MaxOutputTokens: 1024}
	root := encodePlaygroundRoot(t, safetyTestRequest(settings), defaults)
	if got, want := playgroundSafety(t, root), [][2]float64{{7, 5}, {8, 5}, {9, 5}, {10, 1}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Playground 安全设置=%v，期望 %v", got, want)
	}
	got := buildSafety(encodeBuildBody(t, safetyTestRequest(settings), defaults, false))
	if len(got) != 4 || got[3] != "HARM_CATEGORY_DANGEROUS_CONTENT=BLOCK_LOW_AND_ABOVE" || got[1] != "HARM_CATEGORY_HATE_SPEECH=OFF" {
		t.Fatalf("Build 安全设置=%v", got)
	}
}

// TestMediaResolutionEncoding 输入媒体分辨率在 Playground 写 generation config 字段 18 的编号，在 Build 写 Gemini API 枚举名
func TestMediaResolutionEncoding(t *testing.T) {
	defaults := GenerationDefaults{MaxOutputTokens: 1024}
	request := safetyTestRequest(nil)
	request.Config.MediaResolution = " media_resolution_low "
	config := encodePlaygroundRoot(t, request, defaults)[3].([]any)
	if len(config) != 18 || config[17] != float64(1) {
		t.Fatalf("Playground generation config 应为 18 项且字段 18 为 1，得到 %v", config)
	}
	seed := int64(7)
	request.Config.Seed = &seed
	request.Config.MediaResolution = "MEDIA_RESOLUTION_HIGH"
	config = encodePlaygroundRoot(t, request, defaults)[3].([]any)
	if len(config) != 19 || config[17] != float64(3) || config[18] != float64(7) {
		t.Fatalf("带 seed 时 generation config=%v", config)
	}
	generation := encodeBuildBody(t, request, defaults, false)["generationConfig"].(map[string]any)
	if generation["mediaResolution"] != "MEDIA_RESOLUTION_HIGH" {
		t.Fatalf("Build mediaResolution=%v", generation["mediaResolution"])
	}
	// 未设置与 MEDIA_RESOLUTION_UNSPECIFIED 都不发送
	for _, value := range []string{"", "MEDIA_RESOLUTION_UNSPECIFIED"} {
		request := safetyTestRequest(nil)
		request.Config.MediaResolution = value
		if config := encodePlaygroundRoot(t, request, defaults)[3].([]any); len(config) > 17 {
			t.Fatalf("%q 不应发送字段 18: %v", value, config)
		}
		generation := encodeBuildBody(t, request, defaults, false)["generationConfig"].(map[string]any)
		if _, ok := generation["mediaResolution"]; ok {
			t.Fatalf("%q 不应发送 Build mediaResolution", value)
		}
	}
	// 不可用的值两个通道都返回错误
	request.Config.MediaResolution = "MEDIA_RESOLUTION_ULTRA"
	if ValidateMediaResolution(request.Config.MediaResolution) == nil {
		t.Fatal("不可用的媒体分辨率应返回错误")
	}
	if _, err := EncodeGenerateContentRequest(request, defaults, RequestContext{}); err == nil {
		t.Fatal("Playground 编码不可用的媒体分辨率应返回错误")
	}
	if _, _, err := EncodeBuildGenerateRequest(request, defaults, false, false); err == nil {
		t.Fatal("Build 编码不可用的媒体分辨率应返回错误")
	}
}
