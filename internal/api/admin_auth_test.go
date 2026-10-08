package api

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const testAdminToken = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
}

// localRequest 模拟本机发出、或经默认配置的反向代理转发（来源与 Host 都是回环、没有转发头）的请求
func localRequest(target string) *http.Request {
	request := httptest.NewRequest(http.MethodGet, target, nil)
	request.RemoteAddr = "127.0.0.1:40000"
	request.Host = "127.0.0.1:2048"
	return request
}

// TestAdminAccessRequiresToken 控制面不再按回环来源放行，必须携带管理令牌
func TestAdminAccessRequiresToken(t *testing.T) {
	handler := adminAccessMiddleware("", testAdminToken, okHandler())
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, localRequest("/api/status"))
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("没有令牌的回环请求 status = %d，期望 401", recorder.Code)
	}

	withHeader := localRequest("/api/status")
	withHeader.Header.Set("X-Admin-Token", testAdminToken)
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, withHeader)
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("携带令牌请求头 status = %d", recorder.Code)
	}

	withCookie := localRequest("/api/status")
	withCookie.AddCookie(&http.Cookie{Name: AdminTokenCookie, Value: testAdminToken})
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, withCookie)
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("携带令牌 Cookie status = %d", recorder.Code)
	}

	wrong := localRequest("/api/status")
	wrong.Header.Set("X-Admin-Token", strings.Repeat("0", len(testAdminToken)))
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, wrong)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("错误令牌 status = %d", recorder.Code)
	}
}

// TestAdminAccessPasswordStillWorks 设置 ADMIN_PASSWORD 时仍可用 HTTP Basic 认证
func TestAdminAccessPasswordStillWorks(t *testing.T) {
	handler := adminAccessMiddleware("secret-password", testAdminToken, okHandler())
	request := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	request.RemoteAddr = "203.0.113.9:40000"
	request.SetBasicAuth("any", "secret-password")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("密码认证 status = %d", recorder.Code)
	}
}

// TestAdminPageTokenLogin 带令牌参数打开管理页面时写入登录 Cookie 并跳转到去掉参数的地址
func TestAdminPageTokenLogin(t *testing.T) {
	handler := AdminPageMiddleware("", testAdminToken, okHandler())
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, localRequest("/?admin_token="+testAdminToken+"&tab=logs"))
	if recorder.Code != http.StatusSeeOther {
		t.Fatalf("status = %d，期望 303", recorder.Code)
	}
	if location := recorder.Header().Get("Location"); location != "/?tab=logs" {
		t.Fatalf("跳转地址 = %q", location)
	}
	cookies := recorder.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != AdminTokenCookie || cookies[0].Value != testAdminToken ||
		!cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode {
		t.Fatalf("登录 Cookie = %#v", cookies)
	}

	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, localRequest("/"))
	if recorder.Code != http.StatusUnauthorized || !strings.Contains(recorder.Body.String(), "admin_token") {
		t.Fatalf("没有令牌的页面请求 status = %d", recorder.Code)
	}

	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, localRequest("/?admin_token=wrong"))
	if recorder.Code != http.StatusUnauthorized || len(recorder.Result().Cookies()) != 0 {
		t.Fatalf("错误令牌参数 status = %d", recorder.Code)
	}
}

// TestAdminLimiterGlobalCap 伪造来源轮换地址时，全部来源合计的错误上限仍会暂停密码登录
func TestAdminLimiterGlobalCap(t *testing.T) {
	limiter := &adminLoginLimiter{entries: make(map[string]*adminLoginEntry)}
	now := time.Now()
	for index := range adminAuthGlobalMaxFailures {
		ip := fmt.Sprintf("198.51.100.%d", index%250)
		if limiter.blocked(ip, now) {
			t.Fatalf("第 %d 次之前不应封禁", index)
		}
		limiter.fail(ip, now)
	}
	if !limiter.blocked("203.0.113.77", now) {
		t.Fatal("合计错误达到上限后应暂停所有来源的密码登录")
	}
	if limiter.blocked("203.0.113.77", now.Add(adminAuthBlock+time.Second)) {
		t.Fatal("封禁到期后应恢复")
	}
}

// TestSameOriginRejectsCrossSite 跨站浏览器请求、非 http(s) 或带用户信息的 Origin 被拒绝，非浏览器请求放行
func TestSameOriginRejectsCrossSite(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	handler := sameOriginMiddleware(ok)
	for _, test := range []struct {
		name   string
		header map[string]string
		want   int
	}{
		{name: "命令行", want: http.StatusNoContent},
		{name: "同源页面", header: map[string]string{"Origin": "http://panel.example", "Sec-Fetch-Site": "same-origin"}, want: http.StatusNoContent},
		{name: "跨站", header: map[string]string{"Sec-Fetch-Site": "cross-site"}, want: http.StatusForbidden},
		{name: "其他主机", header: map[string]string{"Origin": "https://evil.example"}, want: http.StatusForbidden},
		{name: "非 http 协议", header: map[string]string{"Origin": "chrome-extension://panel.example"}, want: http.StatusForbidden},
		{name: "带用户信息", header: map[string]string{"Origin": "http://user@panel.example"}, want: http.StatusForbidden},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "http://panel.example/api/config", nil)
			for name, value := range test.header {
				request.Header.Set(name, value)
			}
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, request)
			if recorder.Code != test.want {
				t.Fatalf("status = %d，期望 %d", recorder.Code, test.want)
			}
		})
	}
}

// TestRequestRemoteIPIgnoresForwardedHeaders 限速来源只取连接对端地址，伪造的转发头不能绕过单来源限速
func TestRequestRemoteIPIgnoresForwardedHeaders(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	request.RemoteAddr = "127.0.0.1:41000"
	request.Header.Set("X-Real-IP", "203.0.113.9")
	request.Header.Set("X-Forwarded-For", "198.51.100.7, 127.0.0.1")
	if got := requestRemoteIP(request); got != "127.0.0.1" {
		t.Fatalf("来源 = %q，期望连接对端地址", got)
	}
}

// TestControlPlaneLimitsBody 管理接口限制请求体大小并禁止缓存
func TestControlPlaneLimitsBody(t *testing.T) {
	handler := controlPlaneMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.ReadAll(r.Body); err != nil {
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPut, "/api/config", strings.NewReader(strings.Repeat("a", maxControlBodyBytes+1))))
	if recorder.Code != http.StatusRequestEntityTooLarge || recorder.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("status=%d cache-control=%q", recorder.Code, recorder.Header().Get("Cache-Control"))
	}
}
