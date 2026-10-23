package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"time"
)

// ProxyPoolConfig 为 IP 池设置（PROXY_POOL_*）。URL 可能带账号密码：只在 /api/config 返回给管理页编辑（与 proxy 相同），
// 页面按掩码显示；PortStart、PortEnd 为静态 IP 的端口段（含两端），任何模式下都校验
type ProxyPoolConfig struct {
	Mode      string `json:"mode"`
	URL       string `json:"url"`
	PortStart int    `json:"port_start"`
	PortEnd   int    `json:"port_end"`
}

// AdminProxyPool 为 IP 池的实时状态：PROXY_POOL_MODE 不为 off，或关闭后仍有浏览器在用旧出口时返回；不含账号密码。
// Total 为静态 IP 的端口数（动态为 0），InUse 为有浏览器占用的端口（动态为有浏览器在用的会话），Leases 为池分配的活动租约
// （Worker 与登录、验证），Capacity 为可能同时运行的浏览器数 Σ(峰值 + 排空上限)（不含登录与验证）；Shared 为被两个及以上账号
// 同时占用的端口，External 为账户固定代理或 PROXY 指向、或被其他进程锁住、不再分配的端口，Stale 为改配置前分出、浏览器还没关闭的
// 池出口，Unpooled 为 IP 池开启时仍持有直通租约（走 PROXY）的 Worker，Orphaned 为账户已删除、浏览器关闭失败仍占着的租约，
// PinnedDefault 为自己的代理与 PROXY 相同、因此不使用 IP 池的账户；AffinityHits / AffinityMisses 为本次运行回到 / 没能回到上次端口的次数
type AdminProxyPool struct {
	Mode           string                     `json:"mode"`
	Endpoint       string                     `json:"endpoint,omitempty"`
	PortStart      int                        `json:"port_start,omitempty"`
	PortEnd        int                        `json:"port_end,omitempty"`
	Total          int                        `json:"total"`
	InUse          int                        `json:"in_use"`
	Leases         int                        `json:"leases"`
	Capacity       int                        `json:"capacity"`
	Shared         int                        `json:"shared"`
	Cooling        int                        `json:"cooling"`
	External       int                        `json:"external"`
	Stale          int                        `json:"stale"`
	Unpooled       int                        `json:"unpooled"`
	Orphaned       int                        `json:"orphaned"`
	PinnedDefault  int                        `json:"pinned_default"`
	Rotations      int                        `json:"rotations,omitempty"`
	AffinityHits   int                        `json:"affinity_hits"`
	AffinityMisses int                        `json:"affinity_misses"`
	CoolingPorts   []AdminProxyPoolPort       `json:"cooling_ports,omitempty"` // 最多 20 个，按恢复时间
	HotPorts       []AdminProxyPoolHotPort    `json:"hot_ports,omitempty"`     // 最近 10 分钟裸 429 最多的 5 个端口
	Assignments    []AdminProxyPoolAssignment `json:"assignments,omitempty"`   // 活动的池租约
	Check          *AdminProxyPoolCheck       `json:"check,omitempty"`         // 最近一次“检查全部端口”
	RelayLimit     int                        `json:"relay_limit,omitempty"`   // 本机代理中转的进程连接上限
}

// AdminProxyPoolPort 为冷却中的端口：Level 为冷却档位（第 n 档 15 分钟 × 2^(n-1)，最长 2 小时）
type AdminProxyPoolPort struct {
	Port  int       `json:"port"`
	Until time.Time `json:"until"`
	Level int       `json:"level"`
}

// AdminProxyPoolHotPort 为最近 10 分钟裸 429 最多的端口：RateLimited 为次数，Accounts 为返回过裸 429 的不同账号数
type AdminProxyPoolHotPort struct {
	Port        int `json:"port"`
	RateLimited int `json:"rate_limited"`
	Accounts    int `json:"accounts"`
}

// AdminProxyPoolAssignment 为一个活动的池租约：Slot 为端口号或会话号；Kind 为 worker 或 login（登录、验证、导入）；
// 登录中还没有账户 ID 的新账户 AccountID 为空
type AdminProxyPoolAssignment struct {
	AccountID string `json:"account_id"`
	Slot      string `json:"slot"`
	Kind      string `json:"kind"`
}

// AdminProxyPoolCheck 为最近一次“检查全部端口”（只走后台请求路径）：Done 为已检查的端口数，OK 为拿到出口 IP 的端口数，
// DistinctIPs 为其中不同 IP 的个数；Duplicates 为出口 IP 相同的端口组（最多 20 组），Failed 为失败的端口（最多 50 个，错误已脱敏）
type AdminProxyPoolCheck struct {
	Running     bool                      `json:"running"`
	StartedAt   time.Time                 `json:"started_at"`
	FinishedAt  time.Time                 `json:"finished_at,omitzero"`
	Total       int                       `json:"total"`
	Done        int                       `json:"done"`
	OK          int                       `json:"ok"`
	DistinctIPs int                       `json:"distinct_ips"`
	Duplicates  []AdminProxyPoolDuplicate `json:"duplicates,omitempty"`
	Failed      []AdminProxyPoolFailure   `json:"failed,omitempty"`
}

// AdminProxyPoolDuplicate 为出口 IP 相同的一组端口
type AdminProxyPoolDuplicate struct {
	IP    string `json:"ip"`
	Ports []int  `json:"ports"`
}

// AdminProxyPoolFailure 为检查失败的端口（Error 已去掉账号密码）
type AdminProxyPoolFailure struct {
	Port  int    `json:"port"`
	Error string `json:"error"`
}

// ProxyPoolCheckInput 为“检查全部端口”的请求：Pool 为空（或请求体为空）时用当前生效的设置；只支持静态 IP
type ProxyPoolCheckInput struct {
	Pool *ProxyPoolConfig `json:"pool,omitempty"`
}

// FollowDefaultProxyResult 为“改为跟随默认代理”的结果：Changed 为清空了账户代理的账户数，Running 为其中仍有运行中 Worker
// （关闭后才改用 IP 池）的账户数，Skipped 为正在被其他操作使用、本次跳过的账户数
type FollowDefaultProxyResult struct {
	Changed int `json:"changed"`
	Running int `json:"running"`
	Skipped int `json:"skipped"`
}

// proxyPoolService 由支持 IP 池的管理端实现
type proxyPoolService interface {
	CheckProxyPool(context.Context, ProxyPoolCheckInput) error
	FollowDefaultProxy(context.Context) (FollowDefaultProxyResult, error)
}

// handleCheckProxyPool 在后台逐个检查静态 IP 端口段里每个端口的出口 IP：开始后返回 202（不带响应体），已在进行时 409；
// 进度与结果在状态接口的 proxy_pool.check
func (s *server) handleCheckProxyPool(w http.ResponseWriter, r *http.Request) {
	service, ok := s.config.Admin.(proxyPoolService)
	if !ok {
		writeAdminError(w, http.StatusNotImplemented, "proxy_pool_unsupported", "Proxy pool is not supported")
		return
	}
	var input ProxyPoolCheckInput
	// 请求体为空按 {} 处理（用当前生效的设置）
	if err := decodeJSON(r, &input); err != nil && !errors.Is(err, io.EOF) {
		// JSON 解码错误只描述语法与类型（最多带出一个字符），不回显字段值
		writeAdminError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if err := service.CheckProxyPool(r.Context(), input); err != nil {
		writeAdminUpstreamError(w, err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

// handleFollowDefaultProxy 把账户代理与默认代理相同的账户改为跟随默认代理（清空账户代理）；运行中的 Worker 不重启
func (s *server) handleFollowDefaultProxy(w http.ResponseWriter, r *http.Request) {
	service, ok := s.config.Admin.(proxyPoolService)
	if !ok {
		writeAdminError(w, http.StatusNotImplemented, "proxy_pool_unsupported", "Proxy pool is not supported")
		return
	}
	result, err := service.FollowDefaultProxy(r.Context())
	if err != nil {
		writeAdminUpstreamError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}
