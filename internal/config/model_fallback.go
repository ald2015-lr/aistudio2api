package config

import (
	"fmt"
	"strings"
	"unicode"
)

// 额度用完自动改用其他模型（MODEL_FALLBACKS 与 MODEL_FALLBACK_SCOPE，保存后立即生效，见 app/model_fallback.go）
//
// MODEL_FALLBACKS 为逗号分隔的规则，每条写成 源模型=目标1|目标2：请求的模型（按协议层解析后的目录模型 ID，后缀与 long- 前缀照常解析）
// 在请求的号池里没有可用额度时按顺序改用目标模型。模型名写目录 ID（也接受目录别名，按请求号池的目录解析），去掉首尾空白与 models/ 前缀，
// 比较不分大小写；目标不递归套用它自己的规则。空值表示没有规则（关闭）。写法无效（源与目标相同、源模型或目标重复、空名、含空白等）时
// 加载按默认值并记入无效项，管理页保存时拒绝。
// MODEL_FALLBACK_SCOPE 为生效范围：ultra（默认，只对 /ultra 路径的请求）、all（普通路径与 /ultra）、off（关闭）

const (
	// DefaultModelFallbacks 为 MODEL_FALLBACKS 的默认值：/ultra 上 gemini-4-argon 每个账号每天上限 50 次，额度按模型分开计，
	// argon 用完时 gemini-4-carbon 往往还能用
	DefaultModelFallbacks = "gemini-4-argon=gemini-4-carbon"
	// ModelFallbackScopeUltra 为只对 /ultra 路径的请求改用（默认）
	ModelFallbackScopeUltra = "ultra"
	// ModelFallbackScopeAll 为普通路径与 /ultra 都改用
	ModelFallbackScopeAll = "all"
	// ModelFallbackScopeOff 为关闭
	ModelFallbackScopeOff = "off"
)

// ModelFallbackRule 为一条额度用完改用其他模型的规则：Source 在号池里没有可用额度时按顺序改用 Targets
type ModelFallbackRule struct {
	Source  string
	Targets []string
}

// ParseModelFallbacks 解析 MODEL_FALLBACKS：规则以逗号（或全角逗号、换行）分隔，每条为 源模型=目标1|目标2；空白与空规则跳过。
// 模型名去掉首尾空白与 models/ 前缀；源与目标相同、源模型重复、同一条规则里目标重复、空名、名字里有空白或分隔符时返回错误
func ParseModelFallbacks(value string) ([]ModelFallbackRule, error) {
	separators := func(r rune) bool { return r == ',' || r == '，' || r == '\n' || r == '\r' }
	var rules []ModelFallbackRule
	for _, item := range strings.FieldsFunc(value, separators) {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		source, targets, found := strings.Cut(item, "=")
		if !found {
			return nil, fmt.Errorf("MODEL_FALLBACKS 的规则 %q 无效：应写成 源模型=目标模型，多个目标用 | 分隔", item)
		}
		source, err := fallbackModelName(source)
		if err != nil {
			return nil, fmt.Errorf("MODEL_FALLBACKS 的规则 %q 无效：源模型%s", item, err)
		}
		rule := ModelFallbackRule{Source: source}
		for _, target := range strings.Split(targets, "|") {
			target, err := fallbackModelName(target)
			if err != nil {
				return nil, fmt.Errorf("MODEL_FALLBACKS 的规则 %q 无效：目标模型%s", item, err)
			}
			if strings.EqualFold(target, source) {
				return nil, fmt.Errorf("MODEL_FALLBACKS 的规则 %q 无效：目标模型 %s 与源模型相同", item, target)
			}
			for _, existing := range rule.Targets {
				if strings.EqualFold(existing, target) {
					return nil, fmt.Errorf("MODEL_FALLBACKS 的规则 %q 无效：目标模型 %s 重复", item, target)
				}
			}
			rule.Targets = append(rule.Targets, target)
		}
		for _, existing := range rules {
			if strings.EqualFold(existing.Source, source) {
				return nil, fmt.Errorf("MODEL_FALLBACKS 的源模型 %s 重复：同一个模型只能写一条规则", source)
			}
		}
		rules = append(rules, rule)
	}
	return rules, nil
}

// fallbackModelName 整理规则里的一个模型名：去掉首尾空白与 models/ 前缀；为空、含空白或 = | 等分隔符时返回原因
func fallbackModelName(name string) (string, error) {
	name = strings.TrimPrefix(strings.TrimSpace(name), "models/")
	if name == "" {
		return "", fmt.Errorf("不能为空")
	}
	if strings.IndexFunc(name, unicode.IsSpace) >= 0 || strings.ContainsAny(name, "=|，,") {
		return "", fmt.Errorf(" %q 不能包含空白、= 或 |", name)
	}
	return name, nil
}

// FormatModelFallbacks 把规则写回 MODEL_FALLBACKS 的规范写法（源模型=目标1|目标2，规则之间以逗号分隔）
func FormatModelFallbacks(rules []ModelFallbackRule) string {
	items := make([]string, 0, len(rules))
	for _, rule := range rules {
		items = append(items, rule.Source+"="+strings.Join(rule.Targets, "|"))
	}
	return strings.Join(items, ",")
}

// NormalizeModelFallbacks 解析后按规范写法返回 MODEL_FALLBACKS；写法无效时返回原值（去首尾空白）与错误
func NormalizeModelFallbacks(value string) (string, error) {
	rules, err := ParseModelFallbacks(value)
	if err != nil {
		return strings.TrimSpace(value), err
	}
	return FormatModelFallbacks(rules), nil
}

// NormalizeModelFallbackScope 整理 MODEL_FALLBACK_SCOPE：去空白、转小写
func NormalizeModelFallbackScope(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

// validateModelFallbacks 校验 MODEL_FALLBACKS（Validate 与加载后的联动校验共用）
func validateModelFallbacks(value string) error {
	_, err := ParseModelFallbacks(value)
	return err
}

// validateModelFallbackScope 校验 MODEL_FALLBACK_SCOPE
func validateModelFallbackScope(scope string) error {
	switch scope {
	case ModelFallbackScopeUltra, ModelFallbackScopeAll, ModelFallbackScopeOff:
		return nil
	}
	return fmt.Errorf("MODEL_FALLBACK_SCOPE 必须是 ultra、all 或 off")
}
