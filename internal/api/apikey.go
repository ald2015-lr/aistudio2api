package api

import "strings"

// currentAPIKey 返回当前生效的公开 API 密钥；配置了 APIKeyFunc 时每次读取最新值，
// 使管理页面修改密钥后无需重启即可生效
func (config Config) currentAPIKey() string {
	if config.APIKeyFunc != nil {
		return strings.TrimSpace(config.APIKeyFunc())
	}
	return strings.TrimSpace(config.APIKey)
}
