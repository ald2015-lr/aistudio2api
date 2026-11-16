package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

var testStudioSecret = "AQ.TESTKEY" + strings.Repeat("s", 24)

// studioKeyAdmin 为获取 AI Studio API Key 的管理服务替身
type studioKeyAdmin struct {
	AdminService
	fetched [][]string
}

func (admin *studioKeyAdmin) StudioKeys(context.Context) (StudioKeyStatus, error) {
	return StudioKeyStatus{DelaySeconds: 30, Queue: []string{}, Records: []StudioKeyRecord{{
		AccountID: "a@example.com", Status: "ok", HasKey: true, Last4: "ssss", ProjectName: "projects/1", ProjectNumber: "1",
	}}}, nil
}

func (admin *studioKeyAdmin) FetchStudioKeys(_ context.Context, input StudioKeyFetchInput) (StudioKeyStatus, error) {
	admin.fetched = append(admin.fetched, input.AccountIDs)
	return admin.StudioKeys(context.Background())
}

func (admin *studioKeyAdmin) RevealStudioKey(_ context.Context, id string) (StudioKeySecret, error) {
	return StudioKeySecret{AccountID: id, Key: testStudioSecret}, nil
}

func (admin *studioKeyAdmin) RecordAccessStart(AccessLog) {}

func (admin *studioKeyAdmin) RecordAccessLog(AccessLog) {}

func studioKeyRequest(handler http.Handler, method, target, body string, token bool) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, target, strings.NewReader(body))
	request.RemoteAddr = "127.0.0.1:40000"
	request.Host = "127.0.0.1:2048"
	if token {
		request.Header.Set("X-Admin-Token", testAdminToken)
	}
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

// TestStudioKeyRoutes 三个接口都要管理令牌；列表不含完整 Key；reveal 返回完整 Key 且不缓存；批量获取把账户交给管理服务
func TestStudioKeyRoutes(t *testing.T) {
	admin := &studioKeyAdmin{}
	handler := NewHandler(modelListService{}, Config{APIKey: "sk-test", AdminToken: testAdminToken, Admin: admin})
	for _, item := range []struct{ method, target string }{
		{http.MethodGet, "/api/studio-keys"},
		{http.MethodPost, "/api/studio-keys/fetch"},
		{http.MethodPost, "/api/studio-keys/a@example.com/reveal"},
	} {
		if recorder := studioKeyRequest(handler, item.method, item.target, `{"account_ids":["a@example.com"]}`, false); recorder.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s 没有令牌 status = %d", item.method, item.target, recorder.Code)
		}
	}
	list := studioKeyRequest(handler, http.MethodGet, "/api/studio-keys", "", true)
	if list.Code != http.StatusOK || strings.Contains(list.Body.String(), testStudioSecret) || !strings.Contains(list.Body.String(), `"last4":"ssss"`) {
		t.Fatalf("列表 status=%d body=%s", list.Code, list.Body.String())
	}
	reveal := studioKeyRequest(handler, http.MethodPost, "/api/studio-keys/a@example.com/reveal", "", true)
	if reveal.Code != http.StatusOK || reveal.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("reveal status=%d cache=%q", reveal.Code, reveal.Header().Get("Cache-Control"))
	}
	var secret StudioKeySecret
	if err := json.Unmarshal(reveal.Body.Bytes(), &secret); err != nil || secret.Key != testStudioSecret || secret.AccountID != "a@example.com" {
		t.Fatalf("reveal body err=%v", err)
	}
	fetch := studioKeyRequest(handler, http.MethodPost, "/api/studio-keys/fetch", `{"account_ids":["a@example.com","b@example.com"]}`, true)
	if fetch.Code != http.StatusOK || len(admin.fetched) != 1 || len(admin.fetched[0]) != 2 {
		t.Fatalf("fetch status=%d fetched=%v", fetch.Code, admin.fetched)
	}
	if empty := studioKeyRequest(handler, http.MethodPost, "/api/studio-keys/fetch", `{"account_ids":[]}`, true); empty.Code != http.StatusBadRequest {
		t.Fatalf("空列表 status=%d", empty.Code)
	}
}

// TestStudioKeyRoutesUnsupported 管理服务不支持时返回 501
func TestStudioKeyRoutesUnsupported(t *testing.T) {
	handler := NewHandler(modelListService{}, Config{APIKey: "sk-test", AdminToken: testAdminToken, Admin: &permissionDeniedAdmin{}})
	if recorder := studioKeyRequest(handler, http.MethodGet, "/api/studio-keys", "", true); recorder.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d", recorder.Code)
	}
}
