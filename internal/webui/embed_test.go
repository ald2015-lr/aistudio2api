package webui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

// testFiles 模拟一次前端构建产物：入口、带哈希的资源与一个非 assets 的静态文件
func testFiles() fstest.MapFS {
	return fstest.MapFS{
		"index.html":                  {Data: []byte(`<!doctype html><script type="module" src="/assets/index-AbC123.js"></script>`)},
		"assets/index-AbC123.js":      {Data: []byte("console.log('app')")},
		"assets/UsagePanel-XyZ789.js": {Data: []byte("export default {}")},
		"assets/index-AbC123.css":     {Data: []byte("body{}")},
		"robots.txt":                  {Data: []byte("User-agent: *")},
	}
}

// serve 以指定方法请求处理器
func serve(handler http.Handler, method, target string) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(method, target, nil))
	return recorder
}

// TestAssetsCachedImmutable /assets/ 下带哈希的文件长期缓存且不重新验证，内容与类型照常
func TestAssetsCachedImmutable(t *testing.T) {
	handler := newHandler(testFiles())
	for _, test := range []struct {
		target      string
		body        string
		contentType string
	}{
		{target: "/assets/index-AbC123.js", body: "console.log('app')", contentType: "javascript"},
		{target: "/assets/UsagePanel-XyZ789.js", body: "export default {}", contentType: "javascript"},
		{target: "/assets/index-AbC123.css", body: "body{}", contentType: "text/css"},
	} {
		recorder := serve(handler, http.MethodGet, test.target)
		if recorder.Code != http.StatusOK || recorder.Body.String() != test.body {
			t.Fatalf("%s status=%d body=%q", test.target, recorder.Code, recorder.Body.String())
		}
		if got := recorder.Header().Get("Cache-Control"); got != "private, max-age=31536000, immutable" {
			t.Fatalf("%s Cache-Control=%q", test.target, got)
		}
		if got := recorder.Header().Get("Content-Type"); !strings.Contains(got, test.contentType) {
			t.Fatalf("%s Content-Type=%q", test.target, got)
		}
	}
	if recorder := serve(handler, http.MethodHead, "/assets/index-AbC123.js"); recorder.Code != http.StatusOK ||
		recorder.Header().Get("Cache-Control") != "private, max-age=31536000, immutable" {
		t.Fatalf("HEAD 资源 status=%d Cache-Control=%q", recorder.Code, recorder.Header().Get("Cache-Control"))
	}
}

// TestMissingAssetReturns404 升级后旧页面请求已不存在的懒加载块时返回 404，而不是回落为 200 的 index.html
// （那样浏览器只报 MIME 类型错误，前端无法按载入失败恢复）；assets 目录本身不列出内容
func TestMissingAssetReturns404(t *testing.T) {
	handler := newHandler(testFiles())
	for _, target := range []string{
		"/assets/UsagePanel-Old000.js", "/assets/missing.css", "/assets/nested/index.html", "/assets/", "/assets",
		"/assets/../assets/gone.js",
	} {
		for _, method := range []string{http.MethodGet, http.MethodHead} {
			recorder := serve(handler, method, target)
			if recorder.Code != http.StatusNotFound {
				t.Fatalf("%s %s status=%d，期望 404", method, target, recorder.Code)
			}
			if strings.Contains(recorder.Body.String(), "<!doctype html>") || strings.Contains(recorder.Body.String(), "index-AbC123") {
				t.Fatalf("%s %s 回落成了 SPA 入口: %q", method, target, recorder.Body.String())
			}
			if got := recorder.Header().Get("Cache-Control"); got != "no-store" {
				t.Fatalf("%s %s 的 404 Cache-Control=%q，期望 no-store", method, target, got)
			}
		}
	}
}

// TestSPAFallbackAndEntryNoStore 入口、非 assets 的静态文件与前端路由回落都是 no-store；前端路由照旧回落为 index.html，
// 服务 API 前缀照旧 404
func TestSPAFallbackAndEntryNoStore(t *testing.T) {
	handler := newHandler(testFiles())
	index := string(testFiles()["index.html"].Data)
	for _, target := range []string{"/", "/usage", "/settings/advanced", "/assetsx/app.js", "/assets-old.js"} {
		recorder := serve(handler, http.MethodGet, target)
		if recorder.Code != http.StatusOK || recorder.Body.String() != index {
			t.Fatalf("%s status=%d body=%q，期望回落为入口", target, recorder.Code, recorder.Body.String())
		}
		if got := recorder.Header().Get("Content-Type"); got != "text/html; charset=utf-8" {
			t.Fatalf("%s Content-Type=%q", target, got)
		}
		if got := recorder.Header().Get("Cache-Control"); got != "no-store" {
			t.Fatalf("%s Cache-Control=%q，期望 no-store", target, got)
		}
	}
	recorder := serve(handler, http.MethodGet, "/robots.txt")
	if recorder.Code != http.StatusOK || recorder.Body.String() != "User-agent: *" || recorder.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("非 assets 静态文件 status=%d body=%q Cache-Control=%q", recorder.Code, recorder.Body.String(), recorder.Header().Get("Cache-Control"))
	}
	for _, target := range []string{"/api/status", "/v1/models", "/v1beta/models", "/health"} {
		if recorder := serve(handler, http.MethodGet, target); recorder.Code != http.StatusNotFound {
			t.Fatalf("%s status=%d，期望 404", target, recorder.Code)
		}
	}
	if recorder := serve(handler, http.MethodPost, "/usage"); recorder.Code != http.StatusNotFound {
		t.Fatalf("POST 前端路由 status=%d，期望 404", recorder.Code)
	}
}

// TestEmbeddedHandlerServesBuild 内嵌的构建产物可以正常提供：入口 no-store，入口引用的第一个资源可读且长期缓存
func TestEmbeddedHandlerServesBuild(t *testing.T) {
	handler := Handler()
	recorder := serve(handler, http.MethodGet, "/")
	if recorder.Code != http.StatusOK || recorder.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("入口 status=%d Cache-Control=%q", recorder.Code, recorder.Header().Get("Cache-Control"))
	}
	body := recorder.Body.String()
	start := strings.Index(body, "/assets/")
	if start < 0 {
		t.Skip("构建产物没有引用 /assets/ 下的文件（未执行 npm run build）")
	}
	end := strings.IndexByte(body[start:], '"')
	if end < 0 {
		t.Fatalf("入口里的资源路径不完整: %q", body[start:])
	}
	asset := serve(handler, http.MethodGet, body[start:start+end])
	if asset.Code != http.StatusOK || asset.Header().Get("Cache-Control") != "private, max-age=31536000, immutable" {
		t.Fatalf("%s status=%d Cache-Control=%q", body[start:start+end], asset.Code, asset.Header().Get("Cache-Control"))
	}
}
