package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
)

// egressActionAdmin 记录收到的出口管理动作
type egressActionAdmin struct {
	capturingAdmin
	id     string
	action string
	err    error
}

func (admin *egressActionAdmin) EgressAction(_ context.Context, id string, action string) (AdminStatus, error) {
	admin.id, admin.action = id, action
	if admin.err != nil {
		return AdminStatus{}, admin.err
	}
	return AdminStatus{State: "running", Egress: []AdminEgress{{ID: id, Label: "直连", Mode: "rejected"}}}, nil
}

// notFoundError 让管理接口按 404 返回
type notFoundError struct{}

func (notFoundError) Error() string   { return "出口不存在" }
func (notFoundError) HTTPStatus() int { return http.StatusNotFound }

// TestHandleEgressAction POST /api/egress/{id}/withdraw|resume 把出口编号与动作交给管理端并返回最新状态；
// 管理端不支持时返回 404；管理端报出口不存在时按它的状态码返回；其他方法不进入
func TestHandleEgressAction(t *testing.T) {
	admin := &egressActionAdmin{}
	handler := NewHandler(modelListService{}, Config{APIKey: "sk-test", AdminToken: testAdminToken, Admin: admin})
	for _, action := range []string{EgressActionWithdraw, EgressActionResume} {
		recorder := adminPost(handler, "/api/egress/e3b0c44298fc/"+action, "")
		if recorder.Code != http.StatusOK {
			t.Fatalf("%s status = %d: %s", action, recorder.Code, recorder.Body.String())
		}
		if admin.id != "e3b0c44298fc" || admin.action != action {
			t.Fatalf("%s 收到 id=%q action=%q", action, admin.id, admin.action)
		}
		var status AdminStatus
		if err := json.Unmarshal(recorder.Body.Bytes(), &status); err != nil {
			t.Fatal(err)
		}
		if len(status.Egress) != 1 || status.Egress[0].ID != "e3b0c44298fc" {
			t.Fatalf("返回状态 = %+v", status)
		}
	}

	admin.err = errors.Join(errors.New("撤销失败"), notFoundError{})
	if recorder := adminPost(handler, "/api/egress/unknown/withdraw", ""); recorder.Code != http.StatusNotFound {
		t.Fatalf("出口不存在 status = %d，期望 404", recorder.Code)
	}
	if recorder := adminRequest(handler, http.MethodGet, "/api/egress/e3b0c44298fc/withdraw", ""); recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET status = %d，期望 405", recorder.Code)
	}

	unsupported := NewHandler(modelListService{}, Config{APIKey: "sk-test", AdminToken: testAdminToken, Admin: &capturingAdmin{}})
	if recorder := adminPost(unsupported, "/api/egress/e3b0c44298fc/withdraw", ""); recorder.Code != http.StatusNotFound {
		t.Fatalf("不支持出口管理动作 status = %d，期望 404", recorder.Code)
	}
}
