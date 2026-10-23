package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// proxyTestAdmin 记录收到的“测试代理”请求
type proxyTestAdmin struct {
	capturingAdmin
	calls int
	input ProxyTestInput
	err   error
}

func (admin *proxyTestAdmin) TestProxy(_ context.Context, input ProxyTestInput) (ProxyTestResult, error) {
	admin.calls++
	admin.input = input
	if admin.err != nil {
		return ProxyTestResult{}, admin.err
	}
	return ProxyTestResult{
		Proxy: "http://1.2.3.4:8080", Authenticated: true, Relayed: true,
		Browser:  ProxyTestProbe{IP: "203.0.113.7", ElapsedMS: 812},
		Direct:   ProxyTestProbe{IP: "203.0.113.8", ElapsedMS: 640},
		Warnings: []string{"rotating_ip"},
	}, nil
}

// proxyTestStatusError 为带状态码与错误码的管理端错误
type proxyTestStatusError struct {
	status int
	code   string
}

func (err proxyTestStatusError) Error() string     { return "上一次代理测试还没有结束" }
func (err proxyTestStatusError) HTTPStatus() int   { return err.status }
func (err proxyTestStatusError) ErrorCode() string { return err.code }

// TestProxyTestRoute POST /api/proxy/test 带管理令牌时到达管理端并原样返回结果；没有令牌时 401；
// 请求体无效 400；管理端不实现接口时 501；管理端的错误按状态码与错误码返回
func TestProxyTestRoute(t *testing.T) {
	admin := &proxyTestAdmin{}
	handler := NewHandler(modelListService{}, Config{APIKey: "sk-test", AdminToken: testAdminToken, Admin: admin})
	recorder := adminPost(handler, "/api/proxy/test", `{"proxy":"1.2.3.4:8080:user:pass"}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", recorder.Code, recorder.Body.String())
	}
	if admin.calls != 1 || admin.input.Proxy != "1.2.3.4:8080:user:pass" {
		t.Fatalf("收到的请求 = %+v（%d 次）", admin.input, admin.calls)
	}
	var result ProxyTestResult
	if err := json.Unmarshal(recorder.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Proxy != "http://1.2.3.4:8080" || !result.Relayed || result.Browser.IP != "203.0.113.7" ||
		result.Direct.ElapsedMS != 640 || len(result.Warnings) != 1 || result.Warnings[0] != "rotating_ip" {
		t.Fatalf("结果 = %+v", result)
	}
	for _, field := range []string{`"same_ip":false`, `"elapsed_ms":812`, `"authenticated":true`} {
		if !strings.Contains(recorder.Body.String(), field) {
			t.Fatalf("响应缺少 %s: %s", field, recorder.Body.String())
		}
	}

	request := httptest.NewRequest(http.MethodPost, "/api/proxy/test", strings.NewReader(`{"proxy":""}`))
	request.RemoteAddr = "127.0.0.1:40000"
	request.Host = "127.0.0.1:2048"
	unauthorized := httptest.NewRecorder()
	handler.ServeHTTP(unauthorized, request)
	if unauthorized.Code != http.StatusUnauthorized || admin.calls != 1 {
		t.Fatalf("没有管理令牌 status = %d（调用 %d 次），期望 401 且不调用管理端", unauthorized.Code, admin.calls)
	}

	if recorder := adminPost(handler, "/api/proxy/test", `{"proxy":`); recorder.Code != http.StatusBadRequest {
		t.Fatalf("无效请求体 status = %d，期望 400", recorder.Code)
	}

	admin.err = proxyTestStatusError{status: http.StatusConflict, code: "proxy_test_busy"}
	busy := adminPost(handler, "/api/proxy/test", `{"proxy":""}`)
	if busy.Code != http.StatusConflict || !strings.Contains(busy.Body.String(), `"code":"proxy_test_busy"`) {
		t.Fatalf("管理端错误 status = %d: %s", busy.Code, busy.Body.String())
	}

	unsupported := NewHandler(modelListService{}, Config{APIKey: "sk-test", AdminToken: testAdminToken, Admin: &capturingAdmin{}})
	recorder = adminPost(unsupported, "/api/proxy/test", `{"proxy":""}`)
	if recorder.Code != http.StatusNotImplemented || !strings.Contains(recorder.Body.String(), "proxy_test_unsupported") {
		t.Fatalf("不支持测试代理 status = %d: %s", recorder.Code, recorder.Body.String())
	}
}
