package api

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// mapSearchOptions 把公开协议的搜索偏好（搜索深度、近似位置、域名过滤）保留到共同工具配置。
// AI Studio 没有对应的原生参数，由生成链路以系统指令提示的方式传给模型，属于软约束
func mapSearchOptions(size string, location json.RawMessage, filters json.RawMessage) (*aistudio.GoogleSearchOptions, error) {
	switch size {
	case "", "low", "medium", "high":
	default:
		return nil, fmt.Errorf("search_context_size must be low, medium or high")
	}
	options := &aistudio.GoogleSearchOptions{WebSearch: true, ContextSize: size}
	if rawJSONConfigured(location) {
		var object map[string]json.RawMessage
		if err := json.Unmarshal(location, &object); err != nil || object == nil {
			return nil, fmt.Errorf("user_location must be an object")
		}
		options.UserLocation = location
	}
	if rawJSONConfigured(filters) {
		var object map[string]json.RawMessage
		if err := json.Unmarshal(filters, &object); err != nil || object == nil {
			return nil, fmt.Errorf("web_search filters must be an object")
		}
		var err error
		if options.AllowedDomains, err = searchDomainList(object["allowed_domains"], "allowed_domains"); err != nil {
			return nil, err
		}
		if options.BlockedDomains, err = searchDomainList(object["blocked_domains"], "blocked_domains"); err != nil {
			return nil, err
		}
		// 与 Anthropic 一致：允许列表与排除列表不能同时使用
		if len(options.AllowedDomains) > 0 && len(options.BlockedDomains) > 0 {
			return nil, fmt.Errorf("allowed_domains and blocked_domains cannot be used together")
		}
	}
	return options, nil
}

// searchDomainList 解析域名列表：省略或 null 为空，去掉空白项
func searchDomainList(raw json.RawMessage, field string) ([]string, error) {
	if !rawJSONConfigured(raw) {
		return nil, nil
	}
	var values []string
	if err := json.Unmarshal(raw, &values); err != nil {
		return nil, fmt.Errorf("%s must be an array of strings", field)
	}
	domains := make([]string, 0, len(values))
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			domains = append(domains, value)
		}
	}
	if len(domains) == 0 {
		return nil, nil
	}
	return domains, nil
}
