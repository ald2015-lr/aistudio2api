package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

var testStudioSecret = "AQ.TESTKEY" + strings.Repeat("s", 24)

// studioKeyAdmin 为获取 AI Studio API Key 的管理服务替身
type studioKeyAdmin struct {
	AdminService
	fetched [][]string
	all     []bool
	inputs  []StudioKeyFetchInput
	cleared [][]string
	stopped int
}

func (admin *studioKeyAdmin) StudioKeys(context.Context) (StudioKeyStatus, error) {
	return StudioKeyStatus{DelaySeconds: 30, Queue: []string{}, Records: []StudioKeyRecord{{
		AccountID: "a@example.com", Status: "ok", HasKey: true, Last4: "ssss", ProjectName: "projects/1", ProjectNumber: "1",
	}}}, nil
}

func (admin *studioKeyAdmin) FetchStudioKeys(_ context.Context, input StudioKeyFetchInput) (StudioKeyStatus, error) {
	admin.fetched = append(admin.fetched, input.AccountIDs)
	admin.all = append(admin.all, input.All && input.MissingOnly)
	admin.inputs = append(admin.inputs, input)
	return admin.StudioKeys(context.Background())
}

func (admin *studioKeyAdmin) ClearStudioKeys(_ context.Context, input StudioKeyClearInput) (StudioKeyClearResult, error) {
	admin.cleared = append(admin.cleared, input.AccountIDs)
	status, _ := admin.StudioKeys(context.Background())
	return StudioKeyClearResult{StudioKeyStatus: status, Cleared: StudioKeyClearCounts{Deleted: len(input.AccountIDs)}}, nil
}

func (admin *studioKeyAdmin) RevealStudioKey(_ context.Context, id string) (StudioKeySecret, error) {
	return StudioKeySecret{AccountID: id, Key: testStudioSecret}, nil
}

func (admin *studioKeyAdmin) StopStudioKeys(context.Context) (StudioKeyStatus, error) {
	admin.stopped++
	return admin.StudioKeys(context.Background())
}

func (admin *studioKeyAdmin) ExportStudioKeys(context.Context) (StudioKeyExport, error) {
	return StudioKeyExport{Keys: []StudioKeySecret{{AccountID: "a@example.com", Key: testStudioSecret}}}, nil
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
		{http.MethodPost, "/api/studio-keys/stop"},
		{http.MethodPost, "/api/studio-keys/export"},
		{http.MethodPost, "/api/studio-keys/clear"},
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
	// 全部获取：不带账户也接受，交给管理服务展开
	if all := studioKeyRequest(handler, http.MethodPost, "/api/studio-keys/fetch", `{"all":true,"missing_only":true}`, true); all.Code != http.StatusOK || len(admin.all) != 2 || !admin.all[1] {
		t.Fatalf("全部获取 status=%d all=%v", all.Code, admin.all)
	}
	if stop := studioKeyRequest(handler, http.MethodPost, "/api/studio-keys/stop", "", true); stop.Code != http.StatusOK || admin.stopped != 1 {
		t.Fatalf("停止 status=%d stopped=%d", stop.Code, admin.stopped)
	}
	export := studioKeyRequest(handler, http.MethodPost, "/api/studio-keys/export", "", true)
	if export.Code != http.StatusOK || export.Header().Get("Cache-Control") != "no-store" || !strings.Contains(export.Body.String(), testStudioSecret) {
		t.Fatalf("导出 status=%d cache=%q", export.Code, export.Header().Get("Cache-Control"))
	}
}

// TestStudioKeyRoutesUnsupported 管理服务不支持时返回 501
func TestStudioKeyRoutesUnsupported(t *testing.T) {
	handler := NewHandler(modelListService{}, Config{APIKey: "sk-test", AdminToken: testAdminToken, Admin: &permissionDeniedAdmin{}})
	if recorder := studioKeyRequest(handler, http.MethodGet, "/api/studio-keys", "", true); recorder.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d", recorder.Code)
	}
}

// TestStudioKeyClearRoute 清除记录：账户去掉空白与重复后交给管理服务；空列表、超过上限（原始列表超过上限的两倍）时 400；返回最新状态（列表不含完整 Key）与计数，不缓存
func TestStudioKeyClearRoute(t *testing.T) {
	admin := &studioKeyAdmin{}
	handler := NewHandler(modelListService{}, Config{APIKey: "sk-test", AdminToken: testAdminToken, Admin: admin})
	for _, body := range []string{`{"account_ids":[]}`, `{"account_ids":[" ",""]}`, `{}`, `not json`} {
		if recorder := studioKeyRequest(handler, http.MethodPost, "/api/studio-keys/clear", body, true); recorder.Code != http.StatusBadRequest {
			t.Fatalf("%s status = %d", body, recorder.Code)
		}
	}
	ids := make([]string, StudioKeyClearLimit+1)
	for index := range ids {
		ids[index] = fmt.Sprintf("u%d@example.com", index)
	}
	tooMany, _ := json.Marshal(map[string]any{"account_ids": ids})
	if recorder := studioKeyRequest(handler, http.MethodPost, "/api/studio-keys/clear", string(tooMany), true); recorder.Code != http.StatusBadRequest {
		t.Fatalf("超过上限 status = %d", recorder.Code)
	}
	// 原始列表超过上限的两倍时不去重、直接 400（去重用 map，不随请求体平方增长）
	huge := make([]string, 2*StudioKeyClearLimit+1)
	for index := range huge {
		huge[index] = "same@example.com"
	}
	hugeBody, _ := json.Marshal(map[string]any{"account_ids": huge})
	if recorder := studioKeyRequest(handler, http.MethodPost, "/api/studio-keys/clear", string(hugeBody), true); recorder.Code != http.StatusBadRequest {
		t.Fatalf("原始列表过长 status = %d", recorder.Code)
	}
	if len(admin.cleared) != 0 {
		t.Fatalf("不合法的请求不应交给管理服务: %v", admin.cleared)
	}
	// 上限之内的重复（原始个数不超过两倍）去重后照常处理
	repeated := append(slices.Clone(ids[:StudioKeyClearLimit]), ids[:StudioKeyClearLimit]...)
	repeatedBody, _ := json.Marshal(map[string]any{"account_ids": repeated})
	if recorder := studioKeyRequest(handler, http.MethodPost, "/api/studio-keys/clear", string(repeatedBody), true); recorder.Code != http.StatusOK ||
		len(admin.cleared) != 1 || len(admin.cleared[0]) != StudioKeyClearLimit {
		t.Fatalf("重复的账户去重后应照常处理: status=%d", recorder.Code)
	}
	admin.cleared = nil
	recorder := studioKeyRequest(handler, http.MethodPost, "/api/studio-keys/clear", `{"account_ids":[" a@example.com ","b@example.com","a@example.com"]}`, true)
	if recorder.Code != http.StatusOK || recorder.Header().Get("Cache-Control") != "no-store" || len(admin.cleared) != 1 ||
		!slices.Equal(admin.cleared[0], []string{"a@example.com", "b@example.com"}) {
		t.Fatalf("status=%d cleared=%v", recorder.Code, admin.cleared)
	}
	var result struct {
		Records []StudioKeyRecord    `json:"records"`
		Queue   []string             `json:"queue"`
		Cleared StudioKeyClearCounts `json:"cleared"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &result); err != nil || result.Cleared.Deleted != 2 || len(result.Records) != 1 || result.Queue == nil ||
		strings.Contains(recorder.Body.String(), testStudioSecret) {
		t.Fatalf("body err=%v %s", err, recorder.Body.String())
	}
}

// TestStudioKeyFetchFillRoute 补项目信息：fill_project 与 missing_project 原样交给管理服务；与 missing_only 同用时 400
func TestStudioKeyFetchFillRoute(t *testing.T) {
	admin := &studioKeyAdmin{}
	handler := NewHandler(modelListService{}, Config{APIKey: "sk-test", AdminToken: testAdminToken, Admin: admin})
	if recorder := studioKeyRequest(handler, http.MethodPost, "/api/studio-keys/fetch", `{"account_ids":["a@example.com"],"fill_project":true}`, true); recorder.Code != http.StatusOK {
		t.Fatalf("fill_project status = %d", recorder.Code)
	}
	if recorder := studioKeyRequest(handler, http.MethodPost, "/api/studio-keys/fetch", `{"all":true,"missing_project":true}`, true); recorder.Code != http.StatusOK {
		t.Fatalf("missing_project status = %d", recorder.Code)
	}
	if len(admin.inputs) != 2 || !admin.inputs[0].FillProject || admin.inputs[0].All || !admin.inputs[1].All || !admin.inputs[1].MissingProject {
		t.Fatalf("inputs = %+v", admin.inputs)
	}
	for _, body := range []string{`{"all":true,"missing_only":true,"missing_project":true}`, `{"account_ids":["a@example.com"],"missing_only":true,"fill_project":true}`,
		`{"fill_project":true}`} {
		if recorder := studioKeyRequest(handler, http.MethodPost, "/api/studio-keys/fetch", body, true); recorder.Code != http.StatusBadRequest {
			t.Fatalf("%s status = %d", body, recorder.Code)
		}
	}
	if len(admin.inputs) != 2 {
		t.Fatalf("不合法的请求不应交给管理服务: %+v", admin.inputs)
	}
}

// studioKeyInvalidAdmin 为列表里有作废记录、reveal 返回 409 的管理服务替身
type studioKeyInvalidAdmin struct {
	studioKeyAdmin
}

type studioKeyConflict struct{}

func (studioKeyConflict) Error() string {
	return "这条 Key 与另外 2 个账户保存的相同，已作废"
}
func (studioKeyConflict) HTTPStatus() int   { return http.StatusConflict }
func (studioKeyConflict) ErrorCode() string { return "studio_key_invalid" }

func (admin *studioKeyInvalidAdmin) StudioKeys(context.Context) (StudioKeyStatus, error) {
	return StudioKeyStatus{Queue: []string{}, Records: []StudioKeyRecord{{AccountID: "a@example.com", Status: "invalid", Reason: "已作废"}}}, nil
}

func (admin *studioKeyInvalidAdmin) RevealStudioKey(context.Context, string) (StudioKeySecret, error) {
	return StudioKeySecret{}, studioKeyConflict{}
}

func (admin *studioKeyInvalidAdmin) ClearStudioKeys(_ context.Context, input StudioKeyClearInput) (StudioKeyClearResult, error) {
	status, _ := admin.StudioKeys(context.Background())
	return StudioKeyClearResult{StudioKeyStatus: status, Cleared: StudioKeyClearCounts{Invalid: len(input.AccountIDs)}}, nil
}

// TestStudioKeyInvalidRoute 作废的记录：列表里 status 为 invalid、has_key 为 false、没有末四位；reveal 原样返回 409 studio_key_invalid；
// 清除的计数带 invalid
func TestStudioKeyInvalidRoute(t *testing.T) {
	handler := NewHandler(modelListService{}, Config{APIKey: "sk-test", AdminToken: testAdminToken, Admin: &studioKeyInvalidAdmin{}})
	list := studioKeyRequest(handler, http.MethodGet, "/api/studio-keys", "", true)
	if list.Code != http.StatusOK || !strings.Contains(list.Body.String(), `"status":"invalid"`) || !strings.Contains(list.Body.String(), `"has_key":false`) ||
		strings.Contains(list.Body.String(), "last4") {
		t.Fatalf("列表 status=%d body=%s", list.Code, list.Body.String())
	}
	reveal := studioKeyRequest(handler, http.MethodPost, "/api/studio-keys/a@example.com/reveal", "", true)
	if reveal.Code != http.StatusConflict || !strings.Contains(reveal.Body.String(), "studio_key_invalid") {
		t.Fatalf("reveal status=%d body=%s", reveal.Code, reveal.Body.String())
	}
	clear := studioKeyRequest(handler, http.MethodPost, "/api/studio-keys/clear", `{"account_ids":["a@example.com"]}`, true)
	if clear.Code != http.StatusOK || !strings.Contains(clear.Body.String(), `"invalid":1`) {
		t.Fatalf("清除 status=%d body=%s", clear.Code, clear.Body.String())
	}
}
