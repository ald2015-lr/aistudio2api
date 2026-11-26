package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"
)

// IP 池固定分配（PROXY_POOL_FIXED）的管理操作：重新平均分配、给端口换 IP（把这个端口上的账户挪到其他账户最少的端口、停用这个端口）、
// 恢复停用的端口。由支持的管理端按可选接口实现，不改 AdminService

// ProxyPoolFixedInput 为重新平均分配与换 IP 的请求：DryRun 为真时只计算、不改分配（管理页先显示挪动数再确认）
type ProxyPoolFixedInput struct {
	DryRun bool `json:"dry_run,omitempty"`
}

// ProxyPoolFixedRebalanceResult 为重新平均分配的结果：Accounts 为分配表里的账户数（清理之后），Ports 为参与分配的端口数，Moved 为换了端口的
// 账户数，Pruned 为清理掉的已不在账户池的记录（服务停止时不清理），Running 为被挪的账户里有运行中 Worker 的（关闭后改用新端口），
// MinPerPort / MaxPerPort 为之后每个端口的最少 / 最多账户数
type ProxyPoolFixedRebalanceResult struct {
	DryRun     bool `json:"dry_run"`
	Accounts   int  `json:"accounts"`
	Ports      int  `json:"ports"`
	Moved      int  `json:"moved"`
	Pruned     int  `json:"pruned"`
	Running    int  `json:"running"`
	MinPerPort int  `json:"min_per_port"`
	MaxPerPort int  `json:"max_per_port"`
}

// ProxyPoolFixedReassignResult 为给端口换 IP 的结果：Moved 为挪走的账户数，Targets 为挪到的端口（去重升序，最多 20 个），Running 为被挪账户里
// 有运行中 Worker 的（不论在哪个端口），Closing 为此刻用这个端口服务、服务运行中时登记后台关闭的 Worker 数（含固定端口已在别处的过渡 Worker；
// 预演与执行同一口径），Restarted 为执行时真正登记了后台关闭的 Worker（等进行中的请求结束，下次启动用各自的固定端口），Excluded 为这个端口已停用
// （之后不再分配）
type ProxyPoolFixedReassignResult struct {
	DryRun    bool  `json:"dry_run"`
	Port      int   `json:"port"`
	Moved     int   `json:"moved"`
	Targets   []int `json:"targets,omitempty"`
	Running   int   `json:"running"`
	Closing   int   `json:"closing"`
	Restarted int   `json:"restarted"`
	Excluded  bool  `json:"excluded"`
}

// ProxyPoolFixedRestoreResult 为恢复停用端口的结果：Restored 为假时这个端口本来就没有停用
type ProxyPoolFixedRestoreResult struct {
	Port     int  `json:"port"`
	Restored bool `json:"restored"`
}

// ProxyPoolFixedAutoResumeResult 为“恢复自动换 IP（会话）”的结果：Resumed 为假时本来就没有停止；HaltedAt 为停止的时刻，Skipped 为停止之后又坏、
// 仍按暂停处理的个数（不含触发停止的那一个；需要时手动换）
type ProxyPoolFixedAutoResumeResult struct {
	Resumed  bool      `json:"resumed"`
	HaltedAt time.Time `json:"halted_at,omitzero"`
	Skipped  int       `json:"skipped"`
}

// ProxyPoolFixedRotateResult 为动态固定会话下给账户手动换会话的结果：From、To 为换前与换后的会话号，Restarted 为登记了后台关闭的 Worker 数
// （0 或 1：用旧会话的 Worker 在进行中的请求结束后关闭，下次启动用新会话）
type ProxyPoolFixedRotateResult struct {
	AccountID string `json:"account_id"`
	From      string `json:"from"`
	To        string `json:"to"`
	Restarted int    `json:"restarted"`
}

// proxyPoolAutoService 由支持坏 IP 自动换的管理端实现（与 proxyPoolFixedService 分开：不支持的管理端照旧只实现那三个方法）
type proxyPoolAutoService interface {
	ResumeProxyPoolAuto(context.Context) (ProxyPoolFixedAutoResumeResult, error)
	RotateProxyPoolSession(context.Context, string) (ProxyPoolFixedRotateResult, error)
}

// proxyPoolFixedService 由支持 IP 池固定分配的管理端实现
type proxyPoolFixedService interface {
	RebalanceProxyPool(context.Context, ProxyPoolFixedInput) (ProxyPoolFixedRebalanceResult, error)
	ReassignProxyPoolPort(context.Context, int, ProxyPoolFixedInput) (ProxyPoolFixedReassignResult, error)
	RestoreProxyPoolPort(context.Context, int) (ProxyPoolFixedRestoreResult, error)
}

// decodeProxyPoolFixedInput 解码请求体：为空按 {} 处理
func decodeProxyPoolFixedInput(w http.ResponseWriter, r *http.Request) (ProxyPoolFixedInput, bool) {
	var input ProxyPoolFixedInput
	if err := decodeJSON(r, &input); err != nil && !errors.Is(err, io.EOF) {
		// JSON 解码错误只描述语法与类型（最多带出一个字符），不回显字段值
		writeAdminError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return ProxyPoolFixedInput{}, false
	}
	return input, true
}

// proxyPoolFixedPort 读取路径里的端口号：不是 1 到 65535 的整数时返回 400
func proxyPoolFixedPort(w http.ResponseWriter, r *http.Request) (int, bool) {
	port, err := strconv.Atoi(r.PathValue("port"))
	if err != nil || port < 1 || port > 65535 {
		writeAdminError(w, http.StatusBadRequest, "invalid_request", "Port must be an integer between 1 and 65535")
		return 0, false
	}
	return port, true
}

// handleRebalanceProxyPool 重新平均分配固定端口（最少挪动）；dry_run 时只计算
func (s *server) handleRebalanceProxyPool(w http.ResponseWriter, r *http.Request) {
	service, ok := s.config.Admin.(proxyPoolFixedService)
	if !ok {
		writeAdminError(w, http.StatusNotImplemented, "proxy_pool_unsupported", "Proxy pool is not supported")
		return
	}
	input, ok := decodeProxyPoolFixedInput(w, r)
	if !ok {
		return
	}
	result, err := service.RebalanceProxyPool(r.Context(), input)
	if err != nil {
		writeAdminUpstreamError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// handleReassignProxyPoolPort 给端口换 IP：这个端口上的账户挪到其他账户最少的端口，这个端口停用；dry_run 时只计算
func (s *server) handleReassignProxyPoolPort(w http.ResponseWriter, r *http.Request) {
	service, ok := s.config.Admin.(proxyPoolFixedService)
	if !ok {
		writeAdminError(w, http.StatusNotImplemented, "proxy_pool_unsupported", "Proxy pool is not supported")
		return
	}
	port, ok := proxyPoolFixedPort(w, r)
	if !ok {
		return
	}
	input, ok := decodeProxyPoolFixedInput(w, r)
	if !ok {
		return
	}
	result, err := service.ReassignProxyPoolPort(r.Context(), port, input)
	if err != nil {
		writeAdminUpstreamError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// handleRestoreProxyPoolPort 恢复“换 IP”后停用的端口（之后新增账户与重新平均分配会用到它，不挪任何账户）
func (s *server) handleRestoreProxyPoolPort(w http.ResponseWriter, r *http.Request) {
	service, ok := s.config.Admin.(proxyPoolFixedService)
	if !ok {
		writeAdminError(w, http.StatusNotImplemented, "proxy_pool_unsupported", "Proxy pool is not supported")
		return
	}
	port, ok := proxyPoolFixedPort(w, r)
	if !ok {
		return
	}
	result, err := service.RestoreProxyPoolPort(r.Context(), port)
	if err != nil {
		writeAdminUpstreamError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// handleResumeProxyPoolAuto 恢复自动换 IP（固定分配）或自动换会话（固定会话）：清除“坏得太多已停止”与 30 分钟计数；此刻暂停的端口不补做
func (s *server) handleResumeProxyPoolAuto(w http.ResponseWriter, r *http.Request) {
	service, ok := s.config.Admin.(proxyPoolAutoService)
	if !ok {
		writeAdminError(w, http.StatusNotImplemented, "proxy_pool_unsupported", "Proxy pool is not supported")
		return
	}
	result, err := service.ResumeProxyPoolAuto(r.Context())
	if err != nil {
		writeAdminUpstreamError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// handleRotateProxyPoolSession 动态固定会话下给账户换会话（之后用新的会话号；用旧会话的 Worker 在进行中的请求结束后关闭）
func (s *server) handleRotateProxyPoolSession(w http.ResponseWriter, r *http.Request) {
	service, ok := s.config.Admin.(proxyPoolAutoService)
	if !ok {
		writeAdminError(w, http.StatusNotImplemented, "proxy_pool_unsupported", "Proxy pool is not supported")
		return
	}
	result, err := service.RotateProxyPoolSession(r.Context(), r.PathValue("id"))
	if err != nil {
		writeAdminUpstreamError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}
