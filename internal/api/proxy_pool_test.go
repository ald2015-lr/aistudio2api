package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

// proxyPoolAdmin 记录收到的 IP 池请求
type proxyPoolAdmin struct {
	capturingAdmin
	checks  []ProxyPoolCheckInput
	follows int
	err     error
}

func (admin *proxyPoolAdmin) CheckProxyPool(_ context.Context, input ProxyPoolCheckInput) error {
	admin.checks = append(admin.checks, input)
	return admin.err
}

func (admin *proxyPoolAdmin) FollowDefaultProxy(context.Context) (FollowDefaultProxyResult, error) {
	admin.follows++
	if admin.err != nil {
		return FollowDefaultProxyResult{}, admin.err
	}
	return FollowDefaultProxyResult{Changed: 3, Running: 1, Skipped: 2}, nil
}

// TestProxyPoolRoutes POST /api/proxy/pool/check 开始后返回 202 且没有响应体，管理端的错误（已在进行 409）按状态码返回；
// 请求体为空按 {} 处理；POST /api/accounts/proxy/follow-default 返回 {changed, running, skipped}；管理端不实现接口时 501；请求体无效 400
func TestProxyPoolRoutes(t *testing.T) {
	admin := &proxyPoolAdmin{}
	handler := NewHandler(modelListService{}, Config{APIKey: "sk-test", AdminToken: testAdminToken, Admin: admin})
	recorder := adminPost(handler, "/api/proxy/pool/check", `{"pool":{"mode":"static","url":"isp.example.com:10000:u:p","port_start":10001,"port_end":10002}}`)
	if recorder.Code != http.StatusAccepted || recorder.Body.Len() != 0 {
		t.Fatalf("status = %d body = %q", recorder.Code, recorder.Body.String())
	}
	if len(admin.checks) != 1 || admin.checks[0].Pool == nil || admin.checks[0].Pool.PortEnd != 10002 {
		t.Fatalf("收到的请求 = %+v", admin.checks)
	}
	if recorder := adminPost(handler, "/api/proxy/pool/check", `{}`); recorder.Code != http.StatusAccepted || admin.checks[1].Pool != nil {
		t.Fatalf("缺省 pool：status = %d，收到 %+v", recorder.Code, admin.checks)
	}
	// 请求体为空按 {} 处理
	if recorder := adminPost(handler, "/api/proxy/pool/check", ``); recorder.Code != http.StatusAccepted || len(admin.checks) != 3 || admin.checks[2].Pool != nil {
		t.Fatalf("空请求体：status = %d body = %q，收到 %+v", recorder.Code, recorder.Body.String(), admin.checks)
	}
	admin.err = proxyTestStatusError{status: http.StatusConflict, code: "proxy_pool_check_busy"}
	if recorder := adminPost(handler, "/api/proxy/pool/check", `{}`); recorder.Code != http.StatusConflict {
		t.Fatalf("已在进行 status = %d，期望 409", recorder.Code)
	}
	if recorder := adminPost(handler, "/api/proxy/pool/check", `{"pool":`); recorder.Code != http.StatusBadRequest {
		t.Fatalf("无效请求体 status = %d，期望 400", recorder.Code)
	}

	admin.err = nil
	recorder = adminPost(handler, "/api/accounts/proxy/follow-default", ``)
	if recorder.Code != http.StatusOK {
		t.Fatalf("follow-default status = %d: %s", recorder.Code, recorder.Body.String())
	}
	var result map[string]int
	if err := json.Unmarshal(recorder.Body.Bytes(), &result); err != nil || result["changed"] != 3 || result["running"] != 1 || result["skipped"] != 2 {
		t.Fatalf("结果 = %s（%v）", recorder.Body.String(), err)
	}

	unsupported := NewHandler(modelListService{}, Config{APIKey: "sk-test", AdminToken: testAdminToken, Admin: &capturingAdmin{}})
	for _, path := range []string{"/api/proxy/pool/check", "/api/accounts/proxy/follow-default"} {
		if recorder := adminPost(unsupported, path, `{}`); recorder.Code != http.StatusNotImplemented {
			t.Fatalf("%s 不支持时 status = %d，期望 501", path, recorder.Code)
		}
	}
}
