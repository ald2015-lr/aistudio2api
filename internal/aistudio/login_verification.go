package aistudio

import (
	"net/url"
	"strings"
)

// VerificationPagePath 返回页面地址的 host+path（不含 scheme、查询参数与片段，host 小写）：登录页的查询参数里有账号、
// 回跳地址与会话标识，不写进日志与账户原因。解析失败或没有 host 时为空。
// 单独放在这里（不放 login_native.go）：login_native.go 与并行的代理改动共同修改，这里只在它的 Verify 返回值里加一行
func VerificationPagePath(pageURL string) string {
	parsed, err := url.Parse(strings.TrimSpace(pageURL))
	if err != nil || parsed.Host == "" {
		return ""
	}
	path := parsed.EscapedPath()
	if path == "/" {
		path = ""
	}
	return strings.ToLower(parsed.Host) + path
}
