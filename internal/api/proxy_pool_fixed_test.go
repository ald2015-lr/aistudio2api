package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// proxyPoolFixedAdmin 记录收到的固定分配管理请求
type proxyPoolFixedAdmin struct {
	capturingAdmin
	rebalances []ProxyPoolFixedInput
	reassigns  []int
	dryRuns    []bool
	restores   []int
	err        error
}

func (admin *proxyPoolFixedAdmin) RebalanceProxyPool(_ context.Context, input ProxyPoolFixedInput) (ProxyPoolFixedRebalanceResult, error) {
	admin.rebalances = append(admin.rebalances, input)
	if admin.err != nil {
		return ProxyPoolFixedRebalanceResult{}, admin.err
	}
	return ProxyPoolFixedRebalanceResult{DryRun: input.DryRun, Accounts: 2000, Ports: 500, Moved: 7, MinPerPort: 4, MaxPerPort: 4}, nil
}

func (admin *proxyPoolFixedAdmin) ReassignProxyPoolPort(_ context.Context, port int, input ProxyPoolFixedInput) (ProxyPoolFixedReassignResult, error) {
	admin.reassigns = append(admin.reassigns, port)
	admin.dryRuns = append(admin.dryRuns, input.DryRun)
	if admin.err != nil {
		return ProxyPoolFixedReassignResult{}, admin.err
	}
	return ProxyPoolFixedReassignResult{DryRun: input.DryRun, Port: port, Moved: 4, Targets: []int{10077, 10102}, Excluded: !input.DryRun}, nil
}

func (admin *proxyPoolFixedAdmin) RestoreProxyPoolPort(_ context.Context, port int) (ProxyPoolFixedRestoreResult, error) {
	admin.restores = append(admin.restores, port)
	if admin.err != nil {
		return ProxyPoolFixedRestoreResult{}, admin.err
	}
	return ProxyPoolFixedRestoreResult{Port: port, Restored: true}, nil
}

// TestProxyPoolFixedRoutes 三个固定分配接口：200 结果 JSON、dry_run 透传、请求体为空按 {}、非法端口 400、管理端错误按状态码与错误码返回、
// 不支持时 501
func TestProxyPoolFixedRoutes(t *testing.T) {
	admin := &proxyPoolFixedAdmin{}
	handler := NewHandler(modelListService{}, Config{APIKey: "sk-test", AdminToken: testAdminToken, Admin: admin})
	recorder := adminPost(handler, "/api/proxy/pool/fixed/rebalance", `{"dry_run":true}`)
	var rebalance ProxyPoolFixedRebalanceResult
	if recorder.Code != http.StatusOK || json.Unmarshal(recorder.Body.Bytes(), &rebalance) != nil || !rebalance.DryRun || rebalance.Moved != 7 ||
		len(admin.rebalances) != 1 || !admin.rebalances[0].DryRun {
		t.Fatalf("rebalance：%d %s %+v", recorder.Code, recorder.Body.String(), admin.rebalances)
	}
	if recorder := adminPost(handler, "/api/proxy/pool/fixed/rebalance", ``); recorder.Code != http.StatusOK || admin.rebalances[1].DryRun {
		t.Fatalf("空请求体应按 {}：%d %+v", recorder.Code, admin.rebalances)
	}
	recorder = adminPost(handler, "/api/proxy/pool/fixed/ports/10023/reassign", ``)
	var reassign ProxyPoolFixedReassignResult
	if recorder.Code != http.StatusOK || json.Unmarshal(recorder.Body.Bytes(), &reassign) != nil || reassign.Port != 10023 || !reassign.Excluded ||
		len(admin.reassigns) != 1 || admin.reassigns[0] != 10023 || admin.dryRuns[0] {
		t.Fatalf("reassign：%d %s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), `"targets":[10077,10102]`) {
		t.Fatalf("结果 JSON：%s", recorder.Body.String())
	}
	if recorder := adminPost(handler, "/api/proxy/pool/fixed/ports/10023/reassign", `{"dry_run":true}`); recorder.Code != http.StatusOK || !admin.dryRuns[1] {
		t.Fatalf("dry_run 应透传：%d", recorder.Code)
	}
	recorder = adminPost(handler, "/api/proxy/pool/fixed/ports/10023/restore", ``)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"restored":true`) || admin.restores[0] != 10023 {
		t.Fatalf("restore：%d %s", recorder.Code, recorder.Body.String())
	}
	for _, path := range []string{"/api/proxy/pool/fixed/ports/abc/reassign", "/api/proxy/pool/fixed/ports/0/restore", "/api/proxy/pool/fixed/ports/70000/reassign"} {
		if recorder := adminPost(handler, path, ``); recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "invalid_request") {
			t.Fatalf("%s 应返回 400：%d %s", path, recorder.Code, recorder.Body.String())
		}
	}
	if recorder := adminPost(handler, "/api/proxy/pool/fixed/rebalance", `{"dry_run":`); recorder.Code != http.StatusBadRequest {
		t.Fatalf("无效请求体应返回 400：%d", recorder.Code)
	}
	admin.err = proxyTestStatusError{status: http.StatusConflict, code: "proxy_pool_not_fixed"}
	for _, path := range []string{"/api/proxy/pool/fixed/rebalance", "/api/proxy/pool/fixed/ports/10023/reassign", "/api/proxy/pool/fixed/ports/10023/restore"} {
		if recorder := adminPost(handler, path, ``); recorder.Code != http.StatusConflict || !strings.Contains(recorder.Body.String(), "proxy_pool_not_fixed") {
			t.Fatalf("%s 管理端错误应原样返回：%d %s", path, recorder.Code, recorder.Body.String())
		}
	}
	unsupported := NewHandler(modelListService{}, Config{APIKey: "sk-test", AdminToken: testAdminToken, Admin: &capturingAdmin{}})
	for _, path := range []string{"/api/proxy/pool/fixed/rebalance", "/api/proxy/pool/fixed/ports/10023/reassign", "/api/proxy/pool/fixed/ports/10023/restore"} {
		if recorder := adminPost(unsupported, path, `{}`); recorder.Code != http.StatusNotImplemented || !strings.Contains(recorder.Body.String(), "proxy_pool_unsupported") {
			t.Fatalf("%s 不支持时应返回 501：%d", path, recorder.Code)
		}
	}
}

// TestRuntimeConfigProxyPoolFixed proxy_pool_fixed 是顶层可选字段：缺省时不输出、读写往返；不放进 proxy_pool
func TestRuntimeConfigProxyPoolFixed(t *testing.T) {
	data, err := json.Marshal(RuntimeConfig{})
	if err != nil || strings.Contains(string(data), "proxy_pool_fixed") {
		t.Fatalf("缺省时不应输出：%s (%v)", data, err)
	}
	enabled := true
	data, _ = json.Marshal(RuntimeConfig{ProxyPoolFixed: &enabled, ProxyPool: &ProxyPoolConfig{Mode: "static"}})
	if !strings.Contains(string(data), `"proxy_pool_fixed":true`) || strings.Contains(string(data), `"fixed"`) {
		t.Fatalf("JSON：%s", data)
	}
	var decoded RuntimeConfig
	if err := json.Unmarshal([]byte(`{"proxy_pool_fixed":false}`), &decoded); err != nil || decoded.ProxyPoolFixed == nil || *decoded.ProxyPoolFixed {
		t.Fatalf("解析 = %v (%v)", decoded.ProxyPoolFixed, err)
	}
	if data, _ := json.Marshal(AdminAccount{ID: "a@example.com"}); strings.Contains(string(data), "proxy_pool_fixed_port") {
		t.Fatalf("没有固定端口时不应输出：%s", data)
	}
	if data, _ := json.Marshal(AdminProxyPool{Mode: "static"}); strings.Contains(string(data), `"fixed"`) {
		t.Fatalf("固定分配关闭时状态不应有 fixed：%s", data)
	}
}
