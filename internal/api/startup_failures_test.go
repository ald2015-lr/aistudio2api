package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// startupFailureTestAdmin 返回固定的启动失败列表并记录收到的清除请求
type startupFailureTestAdmin struct {
	capturingAdmin
	clears int
	input  StartupFailureClearInput
}

func (admin *startupFailureTestAdmin) StartupFailures(context.Context) ([]AdminStartupFailure, error) {
	return []AdminStartupFailure{
		{ID: "dead@example.com", Label: "dead@example.com", Kind: "auth", Confirmed: true, Reason: "启动 Worker 时被跳转到 Google 登录页",
			Stage: 5, URL: "https://accounts.google.com/x", Count: 2, FirstAt: "2026-10-01T08:00:00Z", LastAt: "2026-10-01T08:05:00Z"},
		{ID: "slow@example.com", Label: "slow@example.com", Kind: "ambiguous", Confirmed: false, Stage: 5, Count: 1,
			Until: "2026-10-01T08:07:00Z"},
	}, nil
}

func (admin *startupFailureTestAdmin) ClearStartupFailures(_ context.Context, input StartupFailureClearInput) (int, error) {
	admin.clears++
	admin.input = input
	return len(input.AccountIDs), nil
}

// TestHandleStartupFailures GET /api/accounts/startup-failures 返回 {"accounts":[...]}，每项带约定的字段；
// POST /api/accounts/startup-failures/clear 把请求交给管理端并返回 {"cleared": n}，account_ids 为空且 all 为假时返回 400、不调用管理端；
// 两个接口都要管理令牌；管理端不支持时返回 501
func TestHandleStartupFailures(t *testing.T) {
	admin := &startupFailureTestAdmin{}
	handler := NewHandler(modelListService{}, Config{APIKey: "sk-test", AdminToken: testAdminToken, Admin: admin})
	recorder := adminRequest(handler, http.MethodGet, "/api/accounts/startup-failures", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", recorder.Code, recorder.Body.String())
	}
	var listed struct {
		Accounts []map[string]json.RawMessage `json:"accounts"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Accounts) != 2 {
		t.Fatalf("列表 = %s", recorder.Body.String())
	}
	for _, key := range []string{"id", "label", "kind", "confirmed", "reason", "stage", "url", "count", "first_at", "last_at"} {
		if _, ok := listed.Accounts[0][key]; !ok {
			t.Fatalf("确定失效条目缺少 %s：%s", key, recorder.Body.String())
		}
	}
	if _, ok := listed.Accounts[1]["until"]; !ok || string(listed.Accounts[1]["confirmed"]) != "false" {
		t.Fatalf("未确认条目 = %s", recorder.Body.String())
	}

	recorder = adminPost(handler, "/api/accounts/startup-failures/clear", `{"account_ids":["dead@example.com"],"all":false}`)
	if recorder.Code != http.StatusOK || strings.TrimSpace(recorder.Body.String()) != `{"cleared":1}` {
		t.Fatalf("清除 status = %d: %s", recorder.Code, recorder.Body.String())
	}
	if admin.clears != 1 || strings.Join(admin.input.AccountIDs, ",") != "dead@example.com" || admin.input.All {
		t.Fatalf("收到的清除请求 = %+v", admin.input)
	}
	if recorder := adminPost(handler, "/api/accounts/startup-failures/clear", `{"all":true}`); recorder.Code != http.StatusOK || !admin.input.All {
		t.Fatalf("清除全部 status = %d", recorder.Code)
	}
	for _, body := range []string{`{}`, `{"account_ids":[],"all":false}`, `{"account_ids":[" "]}`, `{"account_ids":`} {
		if recorder := adminPost(handler, "/api/accounts/startup-failures/clear", body); recorder.Code != http.StatusBadRequest {
			t.Fatalf("请求体 %s status = %d，期望 400", body, recorder.Code)
		}
	}
	if admin.clears != 2 {
		t.Fatalf("无效请求不应调用管理端，实际调用 %d 次", admin.clears)
	}

	for _, method := range []string{http.MethodGet, http.MethodPost} {
		target := "/api/accounts/startup-failures"
		if method == http.MethodPost {
			target += "/clear"
		}
		request := httptest.NewRequest(method, target, strings.NewReader(`{"all":true}`))
		request.RemoteAddr = "127.0.0.1:40000"
		request.Host = "127.0.0.1:2048"
		unauthorized := httptest.NewRecorder()
		handler.ServeHTTP(unauthorized, request)
		if unauthorized.Code != http.StatusUnauthorized {
			t.Fatalf("%s 没有管理令牌 status = %d，期望 401", method, unauthorized.Code)
		}
	}
	if admin.clears != 2 {
		t.Fatal("没有管理令牌时不应调用管理端")
	}

	unsupported := NewHandler(modelListService{}, Config{APIKey: "sk-test", AdminToken: testAdminToken, Admin: &capturingAdmin{}})
	if recorder := adminRequest(unsupported, http.MethodGet, "/api/accounts/startup-failures", ""); recorder.Code != http.StatusNotImplemented {
		t.Fatalf("不支持时 GET status = %d，期望 501", recorder.Code)
	}
	if recorder := adminPost(unsupported, "/api/accounts/startup-failures/clear", `{"all":true}`); recorder.Code != http.StatusNotImplemented {
		t.Fatalf("不支持时 POST status = %d，期望 501", recorder.Code)
	}
}

// TestAdminStatusStartupFields 状态接口的 startup 对象带约定的字段，没有最近一次成功时省略 last_success
func TestAdminStatusStartupFields(t *testing.T) {
	data, err := json.Marshal(AdminStatus{Startup: AdminStartupStatus{Launching: 3, Limit: 17, QueuedOnDemand: 2, QueuedBackground: 120, Paused: 4, Dead: 5, EnvFailures10m: 6}})
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Startup map[string]json.RawMessage `json:"startup"`
	}
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"launching", "limit", "queued_on_demand", "queued_background", "paused", "dead", "env_failures_10m"} {
		if _, ok := decoded.Startup[key]; !ok {
			t.Fatalf("startup 缺少 %s：%s", key, data)
		}
	}
	if _, ok := decoded.Startup["last_success"]; ok {
		t.Fatalf("没有成功时间时应省略 last_success：%s", data)
	}
}
