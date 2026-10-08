package aistudio

import (
	"fmt"
	"strings"
)

// harmCategories 为 Gemini API 安全类别名与 wire 编号（编号与 Gemini API 枚举相同），前四个是官网运行设置的类别。
// HARM_CATEGORY_UNSPECIFIED（0）与 DEROGATORY 至 DANGEROUS（1–6）上游返回 400，按无法识别处理
var harmCategories = []struct {
	name string
	code int64
}{
	{"HARM_CATEGORY_HARASSMENT", 7},
	{"HARM_CATEGORY_HATE_SPEECH", 8},
	{"HARM_CATEGORY_SEXUALLY_EXPLICIT", 9},
	{"HARM_CATEGORY_DANGEROUS_CONTENT", 10},
	{"HARM_CATEGORY_CIVIC_INTEGRITY", 11},
}

// defaultOffSafetyCategories 为非图片模型默认关闭过滤的类别数：harmCategories 的前四个，与官网 Playground 一致
const defaultOffSafetyCategories = 4

// harmThresholds 为 Gemini API 拦截阈值名与 wire 编号
var harmThresholds = map[string]int64{
	"BLOCK_LOW_AND_ABOVE": 1, "BLOCK_MEDIUM_AND_ABOVE": 2, "BLOCK_ONLY_HIGH": 3, "BLOCK_NONE": 4, "OFF": 5,
}

// unspecifiedHarmThreshold 表示沿用该类别的默认阈值
const unspecifiedHarmThreshold = "HARM_BLOCK_THRESHOLD_UNSPECIFIED"

// NormalizeSafetySettings 统一安全设置写法（去空白、转大写），丢弃无法识别的类别或阈值，并逐条返回说明供调用方记 WARN。
// 未知名称不返回 400：客户端常把别家或旧版类别一并发来，跳过后其余设置照常生效。
// 阈值为 HARM_BLOCK_THRESHOLD_UNSPECIFIED 的条目表示沿用默认值，同样不发送，但不算无法识别
func NormalizeSafetySettings(settings []SafetySetting) ([]SafetySetting, []string) {
	if len(settings) == 0 {
		return nil, nil
	}
	normalized := make([]SafetySetting, 0, len(settings))
	var skipped []string
	for _, setting := range settings {
		category := strings.ToUpper(strings.TrimSpace(setting.Category))
		threshold := strings.ToUpper(strings.TrimSpace(setting.Threshold))
		switch {
		case harmCategoryCode(category) == 0:
			skipped = append(skipped, fmt.Sprintf("未知类别 %q", setting.Category))
		case threshold == unspecifiedHarmThreshold:
		case harmThresholds[threshold] == 0:
			skipped = append(skipped, fmt.Sprintf("类别 %s 的未知阈值 %q", category, setting.Threshold))
		default:
			normalized = append(normalized, SafetySetting{Category: category, Threshold: threshold})
		}
	}
	return normalized, skipped
}

// resolveSafetySettings 按类别编号顺序返回最终阈值：非图片模型未指定的官网四类为 OFF，客户端阈值只覆盖对应类别，
// 客户端列出的其他已知类别按编号追加；图片模型只发送客户端列出的类别。
// 无法识别的条目在这里直接跳过，WARN 已在选号前记录（见 NormalizeSafetySettings）
func resolveSafetySettings(settings []SafetySetting, imageRoute bool) []SafetySetting {
	thresholds := make(map[string]string, len(harmCategories))
	if !imageRoute {
		for _, category := range harmCategories[:defaultOffSafetyCategories] {
			thresholds[category.name] = "OFF"
		}
	}
	normalized, _ := NormalizeSafetySettings(settings)
	for _, setting := range normalized {
		thresholds[setting.Category] = setting.Threshold
	}
	resolved := make([]SafetySetting, 0, len(thresholds))
	for _, category := range harmCategories {
		if threshold, ok := thresholds[category.name]; ok {
			resolved = append(resolved, SafetySetting{Category: category.name, Threshold: threshold})
		}
	}
	return resolved
}

// encodeSafetySettings 编码 GenerateContent 根字段 3 的安全设置，每项为 [null, null, 类别, 阈值]；没有条目时为 nil
func encodeSafetySettings(settings []SafetySetting) []any {
	if len(settings) == 0 {
		return nil
	}
	wire := make([]any, 0, len(settings))
	for _, setting := range settings {
		wire = append(wire, []any{nil, nil, harmCategoryCode(setting.Category), harmThresholds[setting.Threshold]})
	}
	return wire
}

// encodeBuildSafetySettings 编码 Build 请求体的 safetySettings，类别与阈值取 Gemini API 枚举名；没有条目时为 nil
func encodeBuildSafetySettings(settings []SafetySetting) []any {
	if len(settings) == 0 {
		return nil
	}
	wire := make([]any, 0, len(settings))
	for _, setting := range settings {
		wire = append(wire, map[string]any{"category": setting.Category, "threshold": setting.Threshold})
	}
	return wire
}

// harmCategoryCode 返回安全类别名的 wire 编号，无法识别时为 0
func harmCategoryCode(name string) int64 {
	for _, category := range harmCategories {
		if category.name == name {
			return category.code
		}
	}
	return 0
}
