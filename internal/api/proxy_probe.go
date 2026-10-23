package api

import (
	"context"
	"net/http"
)

// ProxyTestInput 为“测试代理”的请求：Proxy 为输入框原文（任何支持的写法），为空时测试当前生成服务的默认代理
type ProxyTestInput struct {
	Proxy string `json:"proxy"`
}

// ProxyTestProbe 为一条路径的测试结果：成功时 IP 为出口 IP，失败时 Error 为中文说明（不含账号密码）
type ProxyTestProbe struct {
	IP        string `json:"ip,omitempty"`
	ElapsedMS int64  `json:"elapsed_ms"`
	Error     string `json:"error,omitempty"`
}

// ProxyTestResult 为测试结果：Browser 为 Camoufox 的路径（带账号密码时经本机中转），Direct 为 Go 后台请求的路径（直连上游代理）；
// Proxy 为去掉账号密码的 scheme://host:port
type ProxyTestResult struct {
	Proxy         string         `json:"proxy"`
	Authenticated bool           `json:"authenticated"`
	Relayed       bool           `json:"relayed"`
	Browser       ProxyTestProbe `json:"browser"`
	Direct        ProxyTestProbe `json:"direct"`
	SameIP        bool           `json:"same_ip"`
	Warnings      []string       `json:"warnings,omitempty"` // rotating_ip、https_plain_browser
}

// proxyTestService 由支持“测试代理”的管理端实现
type proxyTestService interface {
	TestProxy(context.Context, ProxyTestInput) (ProxyTestResult, error)
}

// handleTestProxy 经代理访问出口 IP 服务，检查账号密码与 IP 是否按连接轮换；不访问 Google
func (s *server) handleTestProxy(w http.ResponseWriter, r *http.Request) {
	service, ok := s.config.Admin.(proxyTestService)
	if !ok {
		writeAdminError(w, http.StatusNotImplemented, "proxy_test_unsupported", "Proxy test is not supported")
		return
	}
	var input ProxyTestInput
	if err := decodeJSON(r, &input); err != nil {
		// JSON 解码错误只描述语法与类型（最多带出一个字符），不回显字段值
		writeAdminError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	result, err := service.TestProxy(r.Context(), input)
	if err != nil {
		writeAdminUpstreamError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}
