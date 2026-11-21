package app

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Mag1cFall/AIStudio2API/internal/api"
)

// TestProRouteFollowsAdminSave 经管理页保存切换 PRO_POOL：公开 API 的 /pro（按 app 的挂载与 publicAPIConfig 接线）关闭时按协议返回 404，
// 开启后立即可用，再关闭又是 404；普通路径不受影响
func TestProRouteFollowsAdminSave(t *testing.T) {
	env := newIncludeProRuntime(t)
	handler := rootHandler(api.NewHandler(env.manager, publicAPIConfig(env.manager, env.cfg, "token", nil)), "", "token")
	get := func(path string) *httptest.ResponseRecorder {
		t.Helper()
		request := httptest.NewRequest(http.MethodGet, path, nil)
		request.Header.Set("Authorization", "Bearer "+env.cfg.ProxyAPIKey)
		request.RemoteAddr = "127.0.0.1:5555"
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		return recorder
	}
	disabled := func(stage string) {
		t.Helper()
		recorder := get("/pro/v1/models")
		if recorder.Code != http.StatusNotFound ||
			!strings.Contains(recorder.Body.String(), "Invalid URL (GET /pro/v1/models): the /pro endpoints are disabled on this server.") {
			t.Fatalf("%s：/pro/v1/models = %d %s，期望 /pro 关闭的 404", stage, recorder.Code, recorder.Body.String())
		}
	}
	disabled("PRO_POOL=false")
	if recorder := get("/v1/models"); recorder.Code != http.StatusOK {
		t.Fatalf("普通路径：%d %s", recorder.Code, recorder.Body.String())
	}

	env.proPoolSave(t, func(value *api.RuntimeConfig) {
		on := true
		value.ProPool = &on
	})
	if recorder := get("/pro/v1/models"); recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"object":"list"`) {
		t.Fatalf("开启后 /pro/v1/models = %d %s", recorder.Code, recorder.Body.String())
	}
	if recorder := get("/pro/v1beta/models"); recorder.Code != http.StatusOK {
		t.Fatalf("开启后 /pro/v1beta/models = %d %s", recorder.Code, recorder.Body.String())
	}

	env.proPoolSave(t, func(value *api.RuntimeConfig) {
		off := false
		value.ProPool = &off
	})
	disabled("再关闭后")
}
