package app

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestRootHandlerRoutesUltraToPublicAPI /ultra/ 与 /v1、/trace/ 一样交给公开 API，不需要管理令牌；管理页面仍需要令牌
func TestRootHandlerRoutesUltraToPublicAPI(t *testing.T) {
	var reached []string
	apiHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = append(reached, r.URL.Path)
		w.WriteHeader(http.StatusTeapot)
	})
	handler := rootHandler(apiHandler, "", "secret-token")
	for _, path := range []string{"/ultra/v1/models", "/ultra/v1beta/models", "/ultra/unknown", "/trace/v1/models", "/v1/models"} {
		reached = nil
		request := httptest.NewRequest(http.MethodGet, path, nil)
		request.RemoteAddr = "127.0.0.1:5555"
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusTeapot || len(reached) != 1 || reached[0] != path {
			t.Fatalf("%s 应交给公开 API 处理，得到状态 %d、到达 %v", path, recorder.Code, reached)
		}
	}

	reached = nil
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.RemoteAddr = "127.0.0.1:5555"
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized || len(reached) != 0 {
		t.Fatalf("没有令牌打开管理页面应返回 401，得到状态 %d、到达 %v", recorder.Code, reached)
	}
}
