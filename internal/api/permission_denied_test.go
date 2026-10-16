package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"
)

// permissionDeniedAdmin 返回固定的 403 无权限账户列表，并记录清除请求
type permissionDeniedAdmin struct {
	AdminService
	accounts []AdminPermissionDenied
	cleared  []PermissionDeniedClearInput
}

func (admin *permissionDeniedAdmin) PermissionDenied(context.Context) ([]AdminPermissionDenied, error) {
	return admin.accounts, nil
}

func (admin *permissionDeniedAdmin) ClearPermissionDenied(_ context.Context, input PermissionDeniedClearInput) (int, error) {
	admin.cleared = append(admin.cleared, input)
	return len(input.AccountIDs), nil
}

func (admin *permissionDeniedAdmin) RecordAccessStart(AccessLog) {}

func (admin *permissionDeniedAdmin) RecordAccessLog(AccessLog) {}

// postPermissionDeniedClear 携带管理令牌提交清除请求
func postPermissionDeniedClear(handler http.Handler, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, "/api/accounts/permission-denied/clear", strings.NewReader(body))
	request.RemoteAddr = "127.0.0.1:40000"
	request.Host = "127.0.0.1:2048"
	request.Header.Set("X-Admin-Token", testAdminToken)
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

// TestPermissionDeniedRoutes 403 无权限账户列表与清除接口：需要管理令牌；列表按 {"accounts":[...]} 返回邮箱、各模型的通道、次数、
// 首次与最近时间、上游原文；清除接口把 account_ids / all 交给管理服务并返回清除数，请求体无效时返回 400
func TestPermissionDeniedRoutes(t *testing.T) {
	at := time.Date(2026, 10, 16, 8, 0, 0, 0, time.UTC)
	admin := &permissionDeniedAdmin{accounts: []AdminPermissionDenied{{
		ID: "a@example.com", Label: "A@example.com", Count: 3, FirstAt: at, LastAt: at.Add(time.Minute),
		Models: []AdminPermissionDeniedModel{{
			Model: "pro", Channels: []string{"build", "playground"}, Count: 3, FirstAt: at, LastAt: at.Add(time.Minute),
			Message: "The caller does not have permission",
		}},
	}}}
	handler := NewHandler(modelListService{}, Config{APIKey: "sk-test", AdminToken: testAdminToken, Admin: admin})

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, localRequest("/api/accounts/permission-denied"))
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("没有管理令牌 status = %d，期望 401", recorder.Code)
	}
	recorder = adminGet(handler, "/api/accounts/permission-denied")
	if recorder.Code != http.StatusOK {
		t.Fatalf("列表 status = %d: %s", recorder.Code, recorder.Body.String())
	}
	var body struct {
		Accounts []map[string]any `json:"accounts"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil || len(body.Accounts) != 1 {
		t.Fatalf("列表 body = %s err=%v", recorder.Body.String(), err)
	}
	account := body.Accounts[0]
	for _, field := range []string{"id", "label", "models", "count", "first_at", "last_at"} {
		if _, ok := account[field]; !ok {
			t.Fatalf("账户条目缺少 %s：%v", field, account)
		}
	}
	models, _ := account["models"].([]any)
	if len(models) != 1 {
		t.Fatalf("模型条目 = %v", account["models"])
	}
	model, _ := models[0].(map[string]any)
	for _, field := range []string{"model", "channels", "count", "first_at", "last_at", "message"} {
		if _, ok := model[field]; !ok {
			t.Fatalf("模型条目缺少 %s：%v", field, model)
		}
	}
	if account["id"] != "a@example.com" || account["label"] != "A@example.com" || model["last_at"] != "2026-10-16T08:01:00Z" {
		t.Fatalf("列表内容 = %v", account)
	}

	recorder = postPermissionDeniedClear(handler, `{"account_ids":["a@example.com","b@example.com"]}`)
	if recorder.Code != http.StatusOK || strings.TrimSpace(recorder.Body.String()) != `{"cleared":2}` {
		t.Fatalf("清除 status = %d body = %s", recorder.Code, recorder.Body.String())
	}
	if len(admin.cleared) != 1 || !slices.Equal(admin.cleared[0].AccountIDs, []string{"a@example.com", "b@example.com"}) || admin.cleared[0].All {
		t.Fatalf("交给管理服务的清除请求 = %+v", admin.cleared)
	}
	if recorder := postPermissionDeniedClear(handler, `{"all":true}`); recorder.Code != http.StatusOK || len(admin.cleared) != 2 || !admin.cleared[1].All {
		t.Fatalf("全部清除 status = %d cleared = %+v", recorder.Code, admin.cleared)
	}
	if recorder := postPermissionDeniedClear(handler, `{"all":`); recorder.Code != http.StatusBadRequest {
		t.Fatalf("请求体无效 status = %d，期望 400", recorder.Code)
	}
}
