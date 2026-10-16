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
