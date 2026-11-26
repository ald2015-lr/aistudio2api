package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

// proxyPoolAutoAdmin 记录收到的坏 IP 自动换管理请求
type proxyPoolAutoAdmin struct {
	capturingAdmin
	resumes int
	rotated []string
	err     error
}

func (admin *proxyPoolAutoAdmin) ResumeProxyPoolAuto(context.Context) (ProxyPoolFixedAutoResumeResult, error) {
	admin.resumes++
	if admin.err != nil {
		return ProxyPoolFixedAutoResumeResult{}, admin.err
	}
	return ProxyPoolFixedAutoResumeResult{Resumed: true, HaltedAt: time.Date(2026, 11, 26, 10, 5, 0, 0, time.UTC), Skipped: 3}, nil
}

func (admin *proxyPoolAutoAdmin) RotateProxyPoolSession(_ context.Context, accountID string) (ProxyPoolFixedRotateResult, error) {
	admin.rotated = append(admin.rotated, accountID)
	if admin.err != nil {
		return ProxyPoolFixedRotateResult{}, admin.err
	}
	return ProxyPoolFixedRotateResult{AccountID: accountID, From: "abc", To: "abcr1", Restarted: 1}, nil
}

// TestProxyPoolAutoRoutes 恢复自动换与手动换会话：200 结果 JSON、{id} 原样传给管理端、管理端错误按状态码与错误码返回（带中文说明）、不支持时 501
func TestProxyPoolAutoRoutes(t *testing.T) {
	admin := &proxyPoolAutoAdmin{}
	handler := NewHandler(modelListService{}, Config{APIKey: "sk-test", AdminToken: testAdminToken, Admin: admin})
	recorder := adminPost(handler, "/api/proxy/pool/fixed/auto/resume", ``)
	var resumed ProxyPoolFixedAutoResumeResult
	if recorder.Code != http.StatusOK || json.Unmarshal(recorder.Body.Bytes(), &resumed) != nil || !resumed.Resumed || resumed.Skipped != 3 || admin.resumes != 1 ||
		!strings.Contains(recorder.Body.String(), `"halted_at":"2026-11-26T10:05:00Z"`) {
		t.Fatalf("resume：%d %s", recorder.Code, recorder.Body.String())
	}
	recorder = adminPost(handler, "/api/proxy/pool/fixed/accounts/a@example.com/rotate", ``)
	var rotated ProxyPoolFixedRotateResult
	if recorder.Code != http.StatusOK || json.Unmarshal(recorder.Body.Bytes(), &rotated) != nil || rotated.To != "abcr1" || rotated.Restarted != 1 ||
		len(admin.rotated) != 1 || admin.rotated[0] != "a@example.com" {
		t.Fatalf("rotate：%d %s %v", recorder.Code, recorder.Body.String(), admin.rotated)
	}
	if recorder := adminPost(handler, "/api/proxy/pool/fixed/accounts/b%40example.com/rotate", ``); recorder.Code != http.StatusOK || admin.rotated[1] != "b@example.com" {
		t.Fatalf("编码过的 id 应解码后原样传：%d %v", recorder.Code, admin.rotated)
	}
	for _, test := range []struct {
		status int
		code   string
	}{
		{http.StatusConflict, "proxy_pool_not_fixed"},
		{http.StatusConflict, "proxy_pool_not_fixed_dynamic"},
		{http.StatusConflict, "proxy_pool_account_not_pooled"},
		{http.StatusNotFound, "account_not_found"},
		{http.StatusServiceUnavailable, "proxy_pool_unavailable"},
	} {
		admin.err = proxyTestStatusError{status: test.status, code: test.code}
		for _, path := range []string{"/api/proxy/pool/fixed/auto/resume", "/api/proxy/pool/fixed/accounts/a@example.com/rotate"} {
			if recorder := adminPost(handler, path, ``); recorder.Code != test.status || !strings.Contains(recorder.Body.String(), test.code) {
				t.Fatalf("%s 管理端错误 %s 应原样返回：%d %s", path, test.code, recorder.Code, recorder.Body.String())
			}
		}
	}
	unsupported := NewHandler(modelListService{}, Config{APIKey: "sk-test", AdminToken: testAdminToken, Admin: &proxyPoolFixedAdmin{}})
	for _, path := range []string{"/api/proxy/pool/fixed/auto/resume", "/api/proxy/pool/fixed/accounts/a@example.com/rotate"} {
		if recorder := adminPost(unsupported, path, `{}`); recorder.Code != http.StatusNotImplemented || !strings.Contains(recorder.Body.String(), "proxy_pool_unsupported") {
			t.Fatalf("%s 不支持时应返回 501：%d", path, recorder.Code)
		}
	}
}

// TestProxyPoolAutoJSON proxy_pool_fixed_auto_replace 是顶层可选字段（缺省不输出、0 照样输出）；状态的 auto 与 paused[].auto 缺省不输出
func TestProxyPoolAutoJSON(t *testing.T) {
	if data, _ := json.Marshal(RuntimeConfig{}); strings.Contains(string(data), "proxy_pool_fixed_auto_replace") {
		t.Fatalf("缺省时不应输出：%s", data)
	}
	zero := 0
	if data, _ := json.Marshal(RuntimeConfig{ProxyPoolFixedAutoReplace: &zero}); !strings.Contains(string(data), `"proxy_pool_fixed_auto_replace":0`) {
		t.Fatalf("0 应输出：%s", data)
	}
	var decoded RuntimeConfig
	if err := json.Unmarshal([]byte(`{"proxy_pool_fixed_auto_replace":7}`), &decoded); err != nil || decoded.ProxyPoolFixedAutoReplace == nil || *decoded.ProxyPoolFixedAutoReplace != 7 {
		t.Fatalf("解析 = %v (%v)", decoded.ProxyPoolFixedAutoReplace, err)
	}
	status := AdminProxyPoolFixed{Mode: "static", Paused: []AdminProxyPoolPausedPort{{Port: 10001, Reason: "cooling_upstream", Accounts: 2}}}
	if data, _ := json.Marshal(status); strings.Contains(string(data), `"auto"`) {
		t.Fatalf("上限为 0 时状态不应有 auto：%s", data)
	}
	status.Auto = &AdminProxyPoolFixedAuto{Limit: 10, WindowMinutes: 30}
	status.Paused[0].Auto = "halted"
	data, _ := json.Marshal(status)
	if !strings.Contains(string(data), `"auto":{"limit":10,"window_minutes":30,"recent":0,"halted":false}`) || !strings.Contains(string(data), `"auto":"halted"`) {
		t.Fatalf("JSON：%s", data)
	}
}
