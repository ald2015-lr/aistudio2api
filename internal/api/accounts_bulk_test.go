package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// bulkEnableAdmin 记录收到的批量启用请求
type bulkEnableAdmin struct {
	capturingAdmin
	input  BulkEnableInput
	caller string
}

func (admin *bulkEnableAdmin) EnableAccounts(ctx context.Context, input BulkEnableInput) (BulkEnableResult, error) {
	admin.input = input
	admin.caller = AdminCaller(ctx)
	return BulkEnableResult{
		Enabled: 1, Skipped: 1, Failed: []BulkEnableFailure{{ID: "c@example.com", Code: "account_busy", Message: "busy"}},
	}, nil
}

// adminPost 携带管理令牌以 POST 请求控制面
func adminPost(handler http.Handler, target string, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, target, strings.NewReader(body))
	request.RemoteAddr = "127.0.0.1:40000"
	request.Host = "127.0.0.1:2048"
	request.Header.Set("X-Admin-Token", testAdminToken)
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

// TestHandleBulkEnableAccounts POST /api/accounts/bulk-enable 把请求交给管理端并原样返回结果；
// 请求体无效返回 400，管理端不支持时返回 501；与 /api/accounts/{id} 的路由不冲突
func TestHandleBulkEnableAccounts(t *testing.T) {
	admin := &bulkEnableAdmin{}
	handler := NewHandler(modelListService{}, Config{APIKey: "sk-test", AdminToken: testAdminToken, Admin: admin})
	recorder := adminPost(handler, "/api/accounts/bulk-enable", `{"account_ids":["a@example.com","b@example.com"],"all_disabled":false}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", recorder.Code, recorder.Body.String())
	}
	if strings.Join(admin.input.AccountIDs, ",") != "a@example.com,b@example.com" || admin.input.AllDisabled {
		t.Fatalf("收到的请求 = %+v", admin.input)
	}
	if admin.caller == "" {
		t.Fatal("应记录调用来源")
	}
	var result BulkEnableResult
	if err := json.Unmarshal(recorder.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Enabled != 1 || result.Skipped != 1 || len(result.Failed) != 1 || result.Failed[0].Code != "account_busy" {
		t.Fatalf("结果 = %+v", result)
	}

	if recorder := adminPost(handler, "/api/accounts/bulk-enable", `{"account_ids":`); recorder.Code != http.StatusBadRequest {
		t.Fatalf("无效请求体 status = %d，期望 400", recorder.Code)
	}

	unsupported := NewHandler(modelListService{}, Config{APIKey: "sk-test", AdminToken: testAdminToken, Admin: &capturingAdmin{}})
	if recorder := adminPost(unsupported, "/api/accounts/bulk-enable", `{"all_disabled":true}`); recorder.Code != http.StatusNotImplemented {
		t.Fatalf("不支持批量启用 status = %d，期望 501", recorder.Code)
	}
}

// bulkDeleteAdmin 记录收到的批量删除与单个删除请求
type bulkDeleteAdmin struct {
	capturingAdmin
	calls   int
	input   BulkDeleteInput
	caller  string
	deleted []string
}

func (admin *bulkDeleteAdmin) DeleteAccounts(ctx context.Context, input BulkDeleteInput) (BulkDeleteResult, error) {
	admin.calls++
	admin.input = input
	admin.caller = AdminCaller(ctx)
	return BulkDeleteResult{
		Deleted: 2, Missing: 1, Failed: []BulkDeleteFailure{{ID: "c@example.com", Code: "account_busy", Message: "busy"}}, Remaining: 3,
	}, nil
}

func (admin *bulkDeleteAdmin) DeleteAccount(_ context.Context, accountID string) error {
	admin.deleted = append(admin.deleted, accountID)
	return nil
}

// adminRequest 携带管理令牌以指定方法请求控制面
func adminRequest(handler http.Handler, method string, target string, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, target, strings.NewReader(body))
	request.RemoteAddr = "127.0.0.1:40000"
	request.Host = "127.0.0.1:2048"
	request.Header.Set("X-Admin-Token", testAdminToken)
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

// TestHandleBulkDeleteAccounts POST /api/accounts/bulk-delete 把请求交给管理端并原样返回 deleted、missing、failed、remaining；
// account_ids 为空（或全是空白）与请求体无效返回 400、不调用管理端；管理端不支持时返回 501；没有管理令牌时拒绝；
// 其他方法不进入批量删除，DELETE /api/accounts/{id} 的单个删除不受影响
func TestHandleBulkDeleteAccounts(t *testing.T) {
	admin := &bulkDeleteAdmin{}
	handler := NewHandler(modelListService{}, Config{APIKey: "sk-test", AdminToken: testAdminToken, Admin: admin})
	recorder := adminPost(handler, "/api/accounts/bulk-delete", `{"account_ids":["a@example.com","b@example.com"]}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", recorder.Code, recorder.Body.String())
	}
	if strings.Join(admin.input.AccountIDs, ",") != "a@example.com,b@example.com" {
		t.Fatalf("收到的请求 = %+v", admin.input)
	}
	if admin.caller == "" {
		t.Fatal("应记录调用来源")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(recorder.Body.Bytes(), &fields); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"deleted", "missing", "failed", "remaining"} {
		if _, ok := fields[key]; !ok || len(fields) != 4 {
			t.Fatalf("返回字段 = %s，期望 deleted、missing、failed、remaining", recorder.Body.String())
		}
	}
	var result BulkDeleteResult
	if err := json.Unmarshal(recorder.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Deleted != 2 || result.Missing != 1 || result.Remaining != 3 || len(result.Failed) != 1 ||
		result.Failed[0].ID != "c@example.com" || result.Failed[0].Code != "account_busy" || result.Failed[0].Message != "busy" {
		t.Fatalf("结果 = %+v", result)
	}

	for _, body := range []string{`{}`, `{"account_ids":[]}`, `{"account_ids":[" ",""]}`, `{"account_ids":`} {
		if recorder := adminPost(handler, "/api/accounts/bulk-delete", body); recorder.Code != http.StatusBadRequest {
			t.Fatalf("请求体 %s status = %d，期望 400", body, recorder.Code)
		}
	}
	if admin.calls != 1 {
		t.Fatalf("无效请求不应调用管理端，实际调用 %d 次", admin.calls)
	}

	request := httptest.NewRequest(http.MethodPost, "/api/accounts/bulk-delete", strings.NewReader(`{"account_ids":["a@example.com"]}`))
	request.RemoteAddr = "127.0.0.1:40000"
	request.Host = "127.0.0.1:2048"
	unauthorized := httptest.NewRecorder()
	handler.ServeHTTP(unauthorized, request)
	if unauthorized.Code != http.StatusUnauthorized || admin.calls != 1 {
		t.Fatalf("没有管理令牌 status = %d、调用 %d 次，期望 401 且不调用", unauthorized.Code, admin.calls)
	}

	if recorder := adminRequest(handler, http.MethodGet, "/api/accounts/bulk-delete", ""); recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET status = %d，期望 405", recorder.Code)
	}
	if recorder := adminRequest(handler, http.MethodDelete, "/api/accounts/a@example.com", ""); recorder.Code != http.StatusNoContent ||
		strings.Join(admin.deleted, ",") != "a@example.com" || admin.calls != 1 {
		t.Fatalf("单个删除 status = %d deleted=%v calls=%d，期望 204 且只走单个删除", recorder.Code, admin.deleted, admin.calls)
	}

	unsupported := NewHandler(modelListService{}, Config{APIKey: "sk-test", AdminToken: testAdminToken, Admin: &capturingAdmin{}})
	if recorder := adminPost(unsupported, "/api/accounts/bulk-delete", `{"account_ids":["a@example.com"]}`); recorder.Code != http.StatusNotImplemented {
		t.Fatalf("不支持批量删除 status = %d，期望 501", recorder.Code)
	}
}
