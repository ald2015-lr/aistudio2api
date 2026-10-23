package api

import (
	"context"
	"net/http"
)

// ProxyTestInput 为“测试代理”的请求：Proxy 为输入框原文（任何支持的写法），为空时测试当前生成服务的默认代理。
// Pool 不为空时抽测 IP 池（输入框里当前的设置，不必先保存）：静态均匀抽若干端口（含首尾），动态抽测若干临时会话号；
// Samples 为抽测的出口数（默认 3，最多 10）。AccountID 不为空时测这个账户实际使用的出口（账户代理、IP 池绑定或默认代理），
// 结果的 Slot 为端口号或会话号
type ProxyTestInput struct {
	Proxy     string           `json:"proxy"`
	Pool      *ProxyPoolConfig `json:"pool,omitempty"`
	Samples   int              `json:"samples,omitempty"`
	AccountID string           `json:"account_id,omitempty"`
}

// ProxyTestProbe 为一条路径的测试结果：成功时 IP 为出口 IP，失败时 Error 为中文说明（不含账号密码）
type ProxyTestProbe struct {
	IP        string `json:"ip,omitempty"`
	ElapsedMS int64  `json:"elapsed_ms"`
	Error     string `json:"error,omitempty"`
}

// ProxyTestSample 为抽测 IP 池时一个出口的结果：Slot 为端口号或临时会话号，其余同 ProxyTestResult
type ProxyTestSample struct {
	Slot    string         `json:"slot"`
	Browser ProxyTestProbe `json:"browser"`
	Direct  ProxyTestProbe `json:"direct"`
	SameIP  bool           `json:"same_ip"`
}

// ProxyTestResult 为测试结果：Browser 为 Camoufox 的路径（带账号密码时经本机中转），Direct 为 Go 后台请求的路径（直连上游代理）；
// Proxy 为去掉账号密码的 scheme://host:port（抽测 IP 池时为 scheme://host）。抽测 IP 池时 Samples 为每个出口的结果、
// Browser/Direct 为第一个出口的结果、DistinctIPs 为不同出口 IP 数；测试账户实际出口时 Slot 为它当前的端口号或会话号
type ProxyTestResult struct {
	Proxy         string            `json:"proxy"`
	Authenticated bool              `json:"authenticated"`
	Relayed       bool              `json:"relayed"`
	Browser       ProxyTestProbe    `json:"browser"`
	Direct        ProxyTestProbe    `json:"direct"`
	SameIP        bool              `json:"same_ip"`
	Warnings      []string          `json:"warnings,omitempty"` // rotating_ip、https_plain_browser、duplicate_ip、pool_partial
	Samples       []ProxyTestSample `json:"samples,omitempty"`
	DistinctIPs   *int              `json:"distinct_ips,omitempty"`
	Slot          string            `json:"slot,omitempty"`
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
