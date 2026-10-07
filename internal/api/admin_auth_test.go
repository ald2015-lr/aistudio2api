package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
