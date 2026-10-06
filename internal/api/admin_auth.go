package api

import (
	"crypto/sha256"
	"crypto/subtle"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	adminAuthChallenge   = `Basic realm="AIStudio2API", charset="UTF-8"`
	adminAuthMaxFailures = 10
	adminAuthWindow      = 10 * time.Minute
	adminAuthBlock       = 15 * time.Minute
	adminAuthMaxEntries  = 10000
)

// adminLoginLimiter 按来源 IP 限制管理密码的错误尝试
type adminLoginLimiter struct {
	mu      sync.Mutex
	entries map[string]*adminLoginEntry
}

type adminLoginEntry struct {
	failures     int
	windowStart  time.Time
	blockedUntil time.Time
}

var adminLimiter = &adminLoginLimiter{entries: make(map[string]*adminLoginEntry)}

// blocked 判断来源 IP 是否处于封禁期
func (limiter *adminLoginLimiter) blocked(ip string, now time.Time) bool {
	limiter.mu.Lock()
	defer limiter.mu.Unlock()
	entry, ok := limiter.entries[ip]
	return ok && now.Before(entry.blockedUntil)
}

// fail 记录一次密码错误，窗口内达到上限后封禁该 IP
func (limiter *adminLoginLimiter) fail(ip string, now time.Time) {
	limiter.mu.Lock()
	defer limiter.mu.Unlock()
	if len(limiter.entries) >= adminAuthMaxEntries {
		for key, entry := range limiter.entries {
			if !now.Before(entry.blockedUntil) && now.Sub(entry.windowStart) > adminAuthWindow {
				delete(limiter.entries, key)
			}
		}
	}
	entry, ok := limiter.entries[ip]
	if !ok {
		entry = &adminLoginEntry{windowStart: now}
		limiter.entries[ip] = entry
	}
	if now.Sub(entry.windowStart) > adminAuthWindow {
		entry.failures = 0
		entry.windowStart = now
	}
	entry.failures++
	if entry.failures >= adminAuthMaxFailures {
		entry.blockedUntil = now.Add(adminAuthBlock)
		entry.failures = 0
		entry.windowStart = now
	}
}

// success 清除来源 IP 的错误记录
func (limiter *adminLoginLimiter) success(ip string) {
	limiter.mu.Lock()
	delete(limiter.entries, ip)
	limiter.mu.Unlock()
}

// forwardedHeaders 为反向代理转发时会附带的来源头
var forwardedHeaders = []string{"X-Forwarded-For", "X-Real-IP", "Forwarded", "X-Forwarded-Host"}

// proxied 判断请求是否经过反向代理转发
func proxied(r *http.Request) bool {
	for _, name := range forwardedHeaders {
		if r.Header.Get(name) != "" {
			return true
		}
	}
	return false
}

// isLoopbackRequest 判断请求确实由本机直接发出：来源与 Host 都是回环地址，且没有反向代理转发头。
// 经本机 nginx 等反代转发的外网请求来源同样是 127.0.0.1，若反代没有改写 Host，
// 只看来源和 Host 会被当作本机请求而绕过管理密码，所以带转发头的请求一律按远程处理
func isLoopbackRequest(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	return err == nil && net.ParseIP(host).IsLoopback() && loopbackHost(r.Host) && !proxied(r)
}

// requestRemoteIP 返回请求来源 IP；直连来源是本机反代时取反代传来的真实客户端地址，
// 避免所有外网请求共用 127.0.0.1 的错误计数，被他人故意输错密码连带封禁
func requestRemoteIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	if net.ParseIP(host).IsLoopback() {
		if clientIP := strings.TrimSpace(r.Header.Get("X-Real-IP")); clientIP != "" {
			return clientIP
		}
		if forwarded := r.Header.Get("X-Forwarded-For"); forwarded != "" {
			if first := strings.TrimSpace(strings.Split(forwarded, ",")[0]); first != "" {
				return first
			}
		}
	}
	return host
}

// checkAdminPassword 校验 HTTP Basic 认证中的管理密码，用户名不参与校验
//
// 返回 true 表示放行；返回 false 时已写入 401 或 429 响应。
// 浏览器首次请求不带凭据，只返回质询，不计为错误尝试。
func checkAdminPassword(w http.ResponseWriter, r *http.Request, password string) bool {
	ip := requestRemoteIP(r)
	now := time.Now()
	if adminLimiter.blocked(ip, now) {
		writeAdminError(w, http.StatusTooManyRequests, "admin_login_blocked", "管理密码错误次数过多，请 15 分钟后再试")
		return false
	}
	if _, provided, ok := r.BasicAuth(); ok {
		providedSum := sha256.Sum256([]byte(provided))
		expectedSum := sha256.Sum256([]byte(password))
		if subtle.ConstantTimeCompare(providedSum[:], expectedSum[:]) == 1 {
			adminLimiter.success(ip)
			return true
		}
		adminLimiter.fail(ip, now)
	}
	w.Header().Set("WWW-Authenticate", adminAuthChallenge)
	writeAdminError(w, http.StatusUnauthorized, "admin_auth_required", "需要管理密码")
	return false
}

// adminAccessMiddleware 控制面访问控制
//
// 本机回环请求直接放行；设置 ADMIN_PASSWORD 时，远程请求通过 HTTP Basic 认证后放行；
// 未设置时拒绝远程请求，保持原有行为。
func adminAccessMiddleware(password string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isLoopbackRequest(r) {
			next.ServeHTTP(w, r)
			return
		}
		if password == "" {
			writeAdminError(w, http.StatusForbidden, "control_plane_forbidden",
				"Control plane is only available from loopback; set ADMIN_PASSWORD to enable remote management")
			return
		}
		if checkAdminPassword(w, r, password) {
			next.ServeHTTP(w, r)
		}
	})
}

// AdminPageMiddleware 管理页面静态资源的访问控制
//
// 设置 ADMIN_PASSWORD 时远程打开页面先要求认证，浏览器弹出登录框后会为同源的
// /api/ 请求与事件流自动携带凭据；未设置时保持原行为。
func AdminPageMiddleware(password string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if password == "" || isLoopbackRequest(r) {
			next.ServeHTTP(w, r)
			return
		}
		if checkAdminPassword(w, r, password) {
			next.ServeHTTP(w, r)
		}
	})
}
