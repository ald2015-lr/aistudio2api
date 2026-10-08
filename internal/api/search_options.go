package api

import (
	"encoding/json"
	"fmt"

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
		var object struct {
			AllowedDomains []string `json:"allowed_domains"`
		}
		if err := json.Unmarshal(filters, &object); err != nil {
			return nil, fmt.Errorf("web_search filters must be an object")
		}
		options.AllowedDomains = object.AllowedDomains
	}
	return options, nil
}
