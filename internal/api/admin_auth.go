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
	// adminAuthGlobalMaxFailures 为全部来源合计的错误上限：经反向代理时来源地址取自可伪造的转发头，
	// 攻击者每次换一个地址就能绕过按来源的计数；合计达到上限后暂停密码登录（管理令牌不受影响）
	adminAuthGlobalMaxFailures = 50
)

// adminLoginLimiter 按来源 IP 与全部来源合计限制管理密码的错误尝试
type adminLoginLimiter struct {
	mu      sync.Mutex
	entries map[string]*adminLoginEntry
	global  adminLoginEntry
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
	if now.Before(limiter.global.blockedUntil) {
		return true
	}
	entry, ok := limiter.entries[ip]
	return ok && now.Before(entry.blockedUntil)
}

// fail 记录一次密码错误，窗口内达到上限后封禁该来源
func (limiter *adminLoginLimiter) fail(ip string, now time.Time) {
	limiter.mu.Lock()
	defer limiter.mu.Unlock()
	limiter.global.record(now, adminAuthGlobalMaxFailures)
	if len(limiter.entries) >= adminAuthMaxEntries {
		for key, entry := range limiter.entries {
			if !now.Before(entry.blockedUntil) && now.Sub(entry.windowStart) > adminAuthWindow {
				delete(limiter.entries, key)
			}
		}
	}
	entry, ok := limiter.entries[ip]
	if !ok {
		if len(limiter.entries) >= adminAuthMaxEntries {
			// 清理后仍然满：不再为新来源建记录（由全部来源合计的上限兜底），避免伪造来源让记录无限增长
			return
		}
		entry = &adminLoginEntry{windowStart: now}
		limiter.entries[ip] = entry
	}
	entry.record(now, sourceFailureLimit(ip))
}

// sourceFailureLimit 返回单个来源的错误上限。回环来源通常是同机反向代理，后面的全部客户端共用这一个计数，
// 按单来源上限计时任何人输错 10 次就会让所有人 15 分钟无法用密码登录，因此改按全部来源合计的上限计
func sourceFailureLimit(ip string) int {
	if parsed := net.ParseIP(ip); parsed != nil && parsed.IsLoopback() {
		return adminAuthGlobalMaxFailures
	}
	return adminAuthMaxFailures
}

// record 在窗口内累计一次错误，达到上限时开始封禁
func (entry *adminLoginEntry) record(now time.Time, limit int) {
	if now.Sub(entry.windowStart) > adminAuthWindow {
		entry.failures = 0
		entry.windowStart = now
	}
	entry.failures++
	if entry.failures >= limit {
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

// requestRemoteIP 返回限速使用的来源地址：只取连接的对端地址，不信任 X-Real-IP / X-Forwarded-For。
// 这些头可以由客户端伪造（反向代理通常把客户端发来的 X-Forwarded-For 原样保留在前面），
// 信任它们时每次换一个伪造地址就能绕过单来源限速。同机反向代理后面的客户端因此共用一个回环来源计数，
// 该计数按全部来源合计的上限封禁（见 sourceFailureLimit）；管理令牌不受密码限速影响
func requestRemoteIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
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

// 管理令牌：启动时生成并保存在 .admin-token。浏览器用带 admin_token 参数的地址打开一次管理页面后，
// 服务写入 HttpOnly、SameSite=Strict 的 Cookie，之后同源的页面、/api/ 请求与事件流自动携带；
// 脚本在 X-Admin-Token 请求头中携带令牌
const (
	AdminTokenCookie = "aistudio2api_admin"
	adminTokenHeader = "X-Admin-Token"
	adminTokenQuery  = "admin_token"
	// adminTokenCookieMaxAge 为浏览器记住登录的时长（浏览器允许的上限约 400 天）
	adminTokenCookieMaxAge = 400 * 24 * 60 * 60
)

// hasAdminToken 判断请求是否携带正确的管理令牌（请求头或 Cookie）
func hasAdminToken(r *http.Request, token string) bool {
	if token == "" {
		return false
	}
	candidates := []string{strings.TrimSpace(r.Header.Get(adminTokenHeader))}
	if cookie, err := r.Cookie(AdminTokenCookie); err == nil {
		candidates = append(candidates, cookie.Value)
	}
	for _, candidate := range candidates {
		if candidate != "" && subtle.ConstantTimeCompare([]byte(candidate), []byte(token)) == 1 {
			return true
		}
	}
	return false
}

// adminAccessMiddleware 控制面访问控制
//
// 携带管理令牌的请求放行；设置 ADMIN_PASSWORD 时也可以用 HTTP Basic 认证。
// 回环来源不再无条件放行：反向代理（例如默认配置的 nginx proxy_pass）转发的外网请求来源同样是本机，
// 且不一定带转发头，按来源判断会把这些请求当作本机请求
func adminAccessMiddleware(password string, token string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hasAdminToken(r, token) {
			next.ServeHTTP(w, r)
			return
		}
		if password != "" {
			if checkAdminPassword(w, r, password) {
				next.ServeHTTP(w, r)
			}
			return
		}
		writeAdminError(w, http.StatusUnauthorized, "admin_token_required",
			"需要管理令牌：用启动日志中带 admin_token 参数的地址打开管理页面，或在请求头 X-Admin-Token 中携带 .admin-token 文件的内容")
	})
}

// AdminPageMiddleware 管理页面静态资源的访问控制
//
// 带正确 admin_token 参数的请求写入登录 Cookie 后跳转到去掉参数的地址；携带令牌的请求直接放行；
// 设置 ADMIN_PASSWORD 时浏览器弹出登录框，认证后同源的 /api/ 请求与事件流自动携带凭据
func AdminPageMiddleware(password string, token string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if value := r.URL.Query().Get(adminTokenQuery); value != "" && token != "" &&
			subtle.ConstantTimeCompare([]byte(strings.TrimSpace(value)), []byte(token)) == 1 {
			http.SetCookie(w, &http.Cookie{
				Name: AdminTokenCookie, Value: token, Path: "/", MaxAge: adminTokenCookieMaxAge,
				HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: r.TLS != nil,
			})
			target := *r.URL
			query := target.Query()
			query.Del(adminTokenQuery)
			target.RawQuery = query.Encode()
			http.Redirect(w, r, target.RequestURI(), http.StatusSeeOther)
			return
		}
		if hasAdminToken(r, token) {
			next.ServeHTTP(w, r)
			return
		}
		if password != "" {
			if checkAdminPassword(w, r, password) {
				next.ServeHTTP(w, r)
			}
			return
		}
		writeAdminLoginPage(w)
	})
}

// writeAdminLoginPage 返回说明如何获取管理令牌的页面
func writeAdminLoginPage(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = w.Write([]byte(adminLoginPage))
}

const adminLoginPage = `<!doctype html>
<html lang="zh-CN">
<head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><title>需要管理令牌</title>
<style>body{margin:0;background:#0d1117;color:#c9d1d9;font:15px/1.7 system-ui,sans-serif}main{max-width:640px;margin:12vh auto;padding:0 16px}
h1{font-size:20px;color:#fff}code{background:#161b22;border:1px solid #30363d;border-radius:4px;padding:1px 6px}</style></head>
<body><main>
<h1>需要管理令牌</h1>
<p>管理页面需要令牌才能打开。服务启动时会在日志中打印带令牌的地址（<code>/?admin_token=…</code>），用它打开一次后浏览器会记住登录。</p>
<p>令牌保存在程序目录的 <code>.admin-token</code> 文件中；Linux 上也可以运行 <code>./start.sh status</code> 查看完整地址。设置 <code>ADMIN_PASSWORD</code> 后也可以用密码登录。</p>
<h1 lang="en">Admin token required</h1>
<p lang="en">Open the address with <code>?admin_token=…</code> printed in the startup log once; the browser then remembers the login. The token is stored in the <code>.admin-token</code> file next to the program.</p>
</main></body></html>
`
