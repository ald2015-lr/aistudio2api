package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestTraceHidesEveryAPIKeyHeader 鉴权接受的每个密钥请求头在排查记录里都被隐藏，包括 Azure 风格的 api-key
func TestTraceHidesEveryAPIKeyHeader(t *testing.T) {
	names := append([]string{"api-key", "x-api-key", "x-goog-api-key"}, apiKeyHeaders...)
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			request.Header.Set(name, "sk-secret")
			if !requestAPIKeyMatches(request, "sk-secret") {
				t.Fatalf("%s 应是有效凭据", name)
			}
			// 原样写入非规范化的键名，覆盖不经 Header.Set 的情况
			header := http.Header{name: {"sk-secret"}}
			headers := traceHeaders(header)
			if got := headers[http.CanonicalHeaderKey(name)]; got != "<已隐藏>" {
				t.Fatalf("%s 记录为 %q，期望 <已隐藏>", name, got)
			}
		})
	}
	headers := traceHeaders(http.Header{"Authorization": {"Bearer sk-secret"}, "Content-Type": {"application/json"}})
	if headers["Authorization"] != "<已隐藏>" || headers["Content-Type"] != "application/json" {
		t.Fatalf("headers=%v", headers)
	}
}

// TestTraceRouteRedactsAzureAPIKey 只用 api-key 请求头通过鉴权的排查路由请求，记录文件里不出现明文密钥
func TestTraceRouteRedactsAzureAPIKey(t *testing.T) {
	dir := t.TempDir()
	s := &server{traces: newTraceStore(dir)}
	keyFunc := func() string { return "sk-secret" }
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	handler := s.traceEntry(authMiddleware(keyFunc, traceCaptureMiddleware(ok)))
	request := httptest.NewRequest(http.MethodPost, "/trace/v1/chat/completions", strings.NewReader(`{"model":"m"}`))
	request.RemoteAddr = "127.0.0.1:5555"
	request.Header.Set("api-key", "sk-secret")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("status = %d", recorder.Code)
	}
	files, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil || len(files) != 1 {
		t.Fatalf("排查记录文件 %v err=%v", files, err)
	}
	data, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "sk-secret") {
		t.Fatalf("排查记录含明文密钥: %s", data)
	}
	var record struct {
		Client struct {
			Headers map[string]string `json:"headers"`
		} `json:"client"`
	}
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatal(err)
	}
	if got := record.Client.Headers["Api-Key"]; got != "<已隐藏>" {
		t.Fatalf("api-key 请求头记录为 %q，期望 <已隐藏>", got)
	}
}
