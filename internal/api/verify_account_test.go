package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

// verifyAdmin 按账户返回固定的验证结论
type verifyAdmin struct {
	capturingAdmin
	id string
}

func (admin *verifyAdmin) VerifyAccount(_ context.Context, id string) (AdminAccount, error) {
	admin.id = id
	authenticated := false
	return AdminAccount{ID: id, State: "auth_required", Authenticated: &authenticated, VerifyReason: "AI Studio 登录已失效（跳转到 accounts.google.com/v3/signin）"}, nil
}

// TestHandleVerifyAccountShape POST /api/accounts/{id}/verify 返回 {"account": …}，验证结论在 account.authenticated 与
// account.verify_reason 里（前端按它计“有效 / 登录失效 / 出错”，HTTP 200 只表示验证做完了）
func TestHandleVerifyAccountShape(t *testing.T) {
	admin := &verifyAdmin{}
	handler := NewHandler(modelListService{}, Config{APIKey: "sk-test", AdminToken: testAdminToken, Admin: admin})
	recorder := adminPost(handler, "/api/accounts/a@example.com/verify", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", recorder.Code, recorder.Body.String())
	}
	if admin.id != "a@example.com" {
		t.Fatalf("验证的账户 = %q", admin.id)
	}
	var response struct {
		Account map[string]any `json:"account"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Account == nil {
		t.Fatalf("返回里没有 account 外壳：%s", recorder.Body.String())
	}
	if authenticated, ok := response.Account["authenticated"].(bool); !ok || authenticated {
		t.Fatalf("account.authenticated = %v，期望 false：%s", response.Account["authenticated"], recorder.Body.String())
	}
	if reason, _ := response.Account["verify_reason"].(string); reason == "" {
		t.Fatalf("account.verify_reason 为空：%s", recorder.Body.String())
	}
}
