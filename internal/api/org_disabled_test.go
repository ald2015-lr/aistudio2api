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

// orgDisabledAdmin 在 403 无权限账户列表之外另实现组织停用账户能力（OrgDisabledService）
type orgDisabledAdmin struct {
	permissionDeniedAdmin
	orgDisabled []AdminOrgDisabled
	orgCleared  []PermissionDeniedClearInput
}

func (admin *orgDisabledAdmin) OrgDisabled(context.Context) ([]AdminOrgDisabled, error) {
	return admin.orgDisabled, nil
}

func (admin *orgDisabledAdmin) ClearOrgDisabled(_ context.Context, input PermissionDeniedClearInput) (int, error) {
	admin.orgCleared = append(admin.orgCleared, input)
	return len(input.AccountIDs), nil
}

// postAdminJSON 携带管理令牌提交 JSON 请求
func postAdminJSON(handler http.Handler, target string, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, target, strings.NewReader(body))
	request.RemoteAddr = "127.0.0.1:40000"
	request.Host = "127.0.0.1:2048"
	request.Header.Set("X-Admin-Token", testAdminToken)
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

// TestOrgDisabledRoutes 组织停用账户：/api/accounts/permission-denied 的返回在 accounts 之外另带 org_disabled（来源、次数、时间、
// 复查时间、原文），没有时为空数组；/api/accounts/org-disabled/clear 把 account_ids / all 交给管理服务；管理服务没有实现时
// 返回里没有 org_disabled、清除接口 404（旧的管理服务不受影响）
func TestOrgDisabledRoutes(t *testing.T) {
	at := time.Date(2026, 10, 27, 8, 0, 0, 0, time.UTC)
	admin := &orgDisabledAdmin{orgDisabled: []AdminOrgDisabled{{
		ID: "org@example.com", Label: "Org@example.com", Source: "启动", LastSource: "模型目录", Count: 2,
		FirstAt: at, LastAt: at.Add(time.Minute), CheckAt: at.Add(24 * time.Hour), Message: "AI Studio 已被账号所属组织停用 url=access.workspace.google.com/ServiceNotAllowed",
	}}}
	handler := NewHandler(modelListService{}, Config{APIKey: "sk-test", AdminToken: testAdminToken, Admin: admin})
	recorder := adminGet(handler, "/api/accounts/permission-denied")
	if recorder.Code != http.StatusOK {
		t.Fatalf("列表 status = %d: %s", recorder.Code, recorder.Body.String())
	}
	var body struct {
		Accounts    []map[string]any `json:"accounts"`
		OrgDisabled []map[string]any `json:"org_disabled"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil || !strings.Contains(recorder.Body.String(), `"accounts":`) || len(body.OrgDisabled) != 1 {
		t.Fatalf("列表 body = %s err=%v", recorder.Body.String(), err)
	}
	entry := body.OrgDisabled[0]
	for _, field := range []string{"id", "label", "source", "last_source", "count", "first_at", "last_at", "check_at", "message"} {
		if _, ok := entry[field]; !ok {
			t.Fatalf("条目缺少 %s：%v", field, entry)
		}
	}
	if entry["source"] != "启动" || entry["check_at"] != "2026-10-28T08:00:00Z" {
		t.Fatalf("条目内容 = %v", entry)
	}
	recorder = postAdminJSON(handler, "/api/accounts/org-disabled/clear", `{"account_ids":["org@example.com"]}`)
	if recorder.Code != http.StatusOK || strings.TrimSpace(recorder.Body.String()) != `{"cleared":1}` {
		t.Fatalf("清除 status = %d body = %s", recorder.Code, recorder.Body.String())
	}
	if len(admin.orgCleared) != 1 || !slices.Equal(admin.orgCleared[0].AccountIDs, []string{"org@example.com"}) || len(admin.cleared) != 0 {
		t.Fatalf("清除请求 = %+v（403 记录不应被动）", admin.orgCleared)
	}
	if recorder := postAdminJSON(handler, "/api/accounts/org-disabled/clear", `{"all":`); recorder.Code != http.StatusBadRequest {
		t.Fatalf("请求体无效 status = %d，期望 400", recorder.Code)
	}
	// 没有组织停用账户时为空数组
	admin.orgDisabled = nil
	recorder = adminGet(handler, "/api/accounts/permission-denied")
	if !strings.Contains(recorder.Body.String(), `"org_disabled":[]`) {
		t.Fatalf("没有组织停用账户时应为空数组: %s", recorder.Body.String())
	}
	// 旧的管理服务：没有 org_disabled，清除接口 404
	legacy := NewHandler(modelListService{}, Config{APIKey: "sk-test", AdminToken: testAdminToken, Admin: &permissionDeniedAdmin{}})
	if recorder := adminGet(legacy, "/api/accounts/permission-denied"); strings.Contains(recorder.Body.String(), "org_disabled") {
		t.Fatalf("没有实现时不应返回 org_disabled: %s", recorder.Body.String())
	}
	if recorder := postAdminJSON(legacy, "/api/accounts/org-disabled/clear", `{"all":true}`); recorder.Code != http.StatusNotFound {
		t.Fatalf("没有实现时清除接口 status = %d，期望 404", recorder.Code)
	}
}

// TestAdminAccountOrgDisabledJSON 账户的 org_disabled：所属组织停用了 AI Studio 时为 true（停用账户的 state 仍为 disabled），
// 没有时省略（旧前端与旧调用方看到的字段不变）
func TestAdminAccountOrgDisabledJSON(t *testing.T) {
	authenticated := false
	data, err := json.Marshal(AdminAccount{
		ID: "org@example.com", State: "disabled", Authenticated: &authenticated,
		VerifyReason: "AI Studio 已被账号所属组织停用", OrgDisabled: true,
	})
	if err != nil || !strings.Contains(string(data), `"org_disabled":true`) || !strings.Contains(string(data), `"state":"disabled"`) {
		t.Fatalf("组织停用的停用账户 = %s err=%v", data, err)
	}
	data, err = json.Marshal(AdminAccount{ID: "ok@example.com", State: "ready"})
	if err != nil || strings.Contains(string(data), "org_disabled") {
		t.Fatalf("没有组织停用时应省略 org_disabled: %s err=%v", data, err)
	}
}
