package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Mag1cFall/AIStudio2API/internal/config"
)

func publicTestHandler(key string) http.Handler {
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	keyFunc := func() string { return key }
	return browserOriginMiddleware(keyFunc, authMiddleware(keyFunc, ok))
}

func publicTestRequest(authorization string, origin string) *http.Request {
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	request.RemoteAddr = "127.0.0.1:5555"
	if authorization != "" {
		request.Header.Set("Authorization", authorization)
	}
	if origin != "" {
		request.Header.Set("Origin", origin)
	}
	return request
}

// TestPublicAPIRequiresDefaultKey 使用默认密钥时调用 API 仍必须携带密钥
func TestPublicAPIRequiresDefaultKey(t *testing.T) {
	handler := publicTestHandler(config.DefaultProxyAPIKey)
	for _, test := range []struct {
		name          string
		authorization string
		origin        string
		want          int
	}{
		{name: "缺少密钥", want: http.StatusUnauthorized},
		{name: "密钥错误", authorization: "Bearer wrong", want: http.StatusUnauthorized},
		{name: "默认密钥", authorization: "Bearer " + config.DefaultProxyAPIKey, want: http.StatusNoContent},
		{name: "本机页面", authorization: "Bearer " + config.DefaultProxyAPIKey, origin: "http://127.0.0.1:2048", want: http.StatusNoContent},
		// 默认密钥公开可知：外部网页即使带上它也不能调用本机接口
		{name: "外部网页", authorization: "Bearer " + config.DefaultProxyAPIKey, origin: "https://evil.example", want: http.StatusUnauthorized},
	} {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, publicTestRequest(test.authorization, test.origin))
			if recorder.Code != test.want {
				t.Fatalf("status = %d，期望 %d", recorder.Code, test.want)
			}
		})
	}
}

// TestPublicAPICustomKeyAllowsWebOrigins 自定义密钥时外部网页可以携带密钥调用（与原行为一致）
func TestPublicAPICustomKeyAllowsWebOrigins(t *testing.T) {
	recorder := httptest.NewRecorder()
	publicTestHandler("sk-custom").ServeHTTP(recorder, publicTestRequest("Bearer sk-custom", "https://app.example"))
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("status = %d", recorder.Code)
	}
}
