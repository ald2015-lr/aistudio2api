package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Mag1cFall/AIStudio2API/internal/vault"
)

const (
	vaultTestPassword = "Fake-Pass-Api-0001"
	vaultTestNote     = "Fake-Note-Api-0002"
	vaultTestKey      = "JBSWY3DPEHPK3PXP"
)

// vaultAdmin 为账号资料管理端替身：默认把调用交给只在内存的 vault.Store（经过真实的解析、合并与打码），
// 设置了 err 时直接返回它；记录收到的家庭组编号与 rev
type vaultAdmin struct {
	AdminService
	store  *vault.Store
	err    error
	ids    []string
	revs   []*int64
	since  []string
	called int
}

func newVaultAdmin() *vaultAdmin { return &vaultAdmin{store: vault.Open("", vault.Options{})} }

func (admin *vaultAdmin) RecordAccessStart(AccessLog) {}

func (admin *vaultAdmin) RecordAccessLog(AccessLog) {}

func (admin *vaultAdmin) Vault(ctx context.Context, since string) (vault.View, error) {
	admin.called++
	admin.since = append(admin.since, since)
	if admin.err != nil {
		return vault.View{}, admin.err
	}
	return admin.store.View(ctx, since)
}

func (admin *vaultAdmin) ImportProfiles(ctx context.Context, input vault.ProfileImportInput) (vault.ProfileImportResult, error) {
	admin.called++
	if admin.err != nil {
		return vault.ProfileImportResult{}, admin.err
	}
	return admin.store.ImportProfiles(ctx, input)
}

func (admin *vaultAdmin) UpdateProfile(ctx context.Context, input vault.ProfileUpdate) (vault.ProfileUpdateResult, error) {
	admin.called++
	if admin.err != nil {
		return vault.ProfileUpdateResult{}, admin.err
	}
	return admin.store.UpdateProfile(ctx, input)
}

func (admin *vaultAdmin) DeleteProfiles(ctx context.Context, input vault.ProfileDeleteInput) (vault.ProfileDeleteResult, error) {
	admin.called++
	if admin.err != nil {
		return vault.ProfileDeleteResult{}, admin.err
	}
	return admin.store.DeleteProfiles(ctx, input)
}

func (admin *vaultAdmin) RevealProfile(ctx context.Context, input vault.RevealInput) (vault.ProfileSecret, error) {
	admin.called++
	if admin.err != nil {
		return vault.ProfileSecret{}, admin.err
	}
	return admin.store.RevealProfile(ctx, input)
}

func (admin *vaultAdmin) ExportProfiles(ctx context.Context, input vault.ExportInput) (vault.ProfileExport, error) {
	admin.called++
	if admin.err != nil {
		return vault.ProfileExport{}, admin.err
	}
	return admin.store.ExportProfiles(ctx, input)
}

func (admin *vaultAdmin) ImportFamilies(ctx context.Context, input vault.FamilyImportInput) (vault.FamilyImportResult, error) {
	admin.called++
	if admin.err != nil {
		return vault.FamilyImportResult{}, admin.err
	}
	return admin.store.ImportFamilies(ctx, input)
}

func (admin *vaultAdmin) CreateFamily(ctx context.Context, input vault.FamilyCreate) (vault.FamilyBlockResult, error) {
	admin.called++
	if admin.err != nil {
		return vault.FamilyBlockResult{}, admin.err
	}
	return admin.store.CreateFamily(ctx, input)
}

func (admin *vaultAdmin) UpdateFamily(ctx context.Context, id string, input vault.FamilyUpdate) (vault.FamilyBlockResult, error) {
	admin.called++
	admin.ids = append(admin.ids, id)
	if admin.err != nil {
		return vault.FamilyBlockResult{}, admin.err
	}
	return admin.store.UpdateFamily(ctx, id, input)
}

func (admin *vaultAdmin) DissolveFamily(ctx context.Context, id string, rev *int64) (vault.FamilyDissolveResult, error) {
	admin.called++
	admin.ids = append(admin.ids, id)
	admin.revs = append(admin.revs, rev)
	if admin.err != nil {
		return vault.FamilyDissolveResult{}, admin.err
	}
	return admin.store.DissolveFamily(ctx, id, rev)
}

func (admin *vaultAdmin) StartEmpty(ctx context.Context, file string) (vault.StartEmptyResult, error) {
	admin.called++
	if admin.err != nil {
		return vault.StartEmptyResult{}, admin.err
	}
	return admin.store.StartEmpty(ctx, file)
}

func vaultHandler(admin AdminService) http.Handler {
	return NewHandler(modelListService{}, Config{APIKey: "sk-test", AdminToken: testAdminToken, Admin: admin})
}

func vaultRequest(handler http.Handler, method, target, body string) *httptest.ResponseRecorder {
	return studioKeyRequest(handler, method, target, body, true)
}

func vaultErrorCode(t *testing.T, recorder *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("错误体不是 JSON: %s", recorder.Body.String())
	}
	return body.Error.Code
}

func seedVault(t *testing.T, handler http.Handler) {
	t.Helper()
	body, _ := json.Marshal(vault.ProfileImportInput{Sources: []vault.ImportSource{{Text: "a@example.com----" + vaultTestPassword + "----r@example.com----" + vaultTestKey + "----" + vaultTestNote}}})
	if recorder := vaultRequest(handler, http.MethodPost, "/api/vault/profiles/import", string(body)); recorder.Code != http.StatusOK {
		t.Fatalf("导入 status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

var vaultRoutes = []struct{ method, target, body string }{
	{http.MethodGet, "/api/vault", ""},
	{http.MethodPost, "/api/vault/profiles/import", `{"dry_run":true,"sources":[{"text":"a@example.com----x"}]}`},
	{http.MethodPost, "/api/vault/profiles/update", `{"email":"a@example.com"}`},
	{http.MethodPost, "/api/vault/profiles/delete", `{"emails":["a@example.com"]}`},
	{http.MethodPost, "/api/vault/profiles/reveal", `{"email":"a@example.com"}`},
	{http.MethodPost, "/api/vault/profiles/export", `{"all":true}`},
	{http.MethodPost, "/api/vault/families/import", `{"dry_run":true,"text":"a@example.com\nb@example.com"}`},
	{http.MethodPost, "/api/vault/families", `{"dry_run":true,"primary":"a@example.com","members":["b@example.com"]}`},
	{http.MethodPut, "/api/vault/families/fam-aaaaaaaaaaaa", `{"dry_run":true,"name":"x"}`},
	{http.MethodDelete, "/api/vault/families/fam-aaaaaaaaaaaa", ""},
	{http.MethodPost, "/api/vault/start-empty", `{"file":"profiles"}`},
}

// TestVaultRoutesAuthAndCache 每条新路由都要管理令牌；全部 no-store，取出与导出另有 Pragma: no-cache；管理端不支持时 501
func TestVaultRoutesAuthAndCache(t *testing.T) {
	admin := newVaultAdmin()
	handler := vaultHandler(admin)
	for _, route := range vaultRoutes {
		if recorder := studioKeyRequest(handler, route.method, route.target, route.body, false); recorder.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s 没有令牌 status = %d", route.method, route.target, recorder.Code)
		}
	}
	if admin.called != 0 {
		t.Fatal("没有令牌时不应调用管理端")
	}
	seedVault(t, handler)
	for _, route := range vaultRoutes {
		recorder := vaultRequest(handler, route.method, route.target, route.body)
		if recorder.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("%s %s Cache-Control = %q", route.method, route.target, recorder.Header().Get("Cache-Control"))
		}
		secret := strings.HasSuffix(route.target, "/reveal") || strings.HasSuffix(route.target, "/export")
		if secret && recorder.Header().Get("Pragma") != "no-cache" {
			t.Fatalf("%s 应带 Pragma: no-cache", route.target)
		}
	}
	unsupported := vaultHandler(&permissionDeniedAdmin{})
	for _, route := range vaultRoutes {
		if recorder := vaultRequest(unsupported, route.method, route.target, route.body); recorder.Code != http.StatusNotImplemented || vaultErrorCode(t, recorder) != vault.CodeUnsupported {
			t.Fatalf("%s %s 不支持时 status = %d", route.method, route.target, recorder.Code)
		}
	}
}

// TestVaultResponsesMasked 列表、导入预览与结果、编辑结果、家庭组结果都不含密码、2FA、备注；取出与导出才有
func TestVaultResponsesMasked(t *testing.T) {
	handler := vaultHandler(newVaultAdmin())
	seedVault(t, handler)
	update, _ := json.Marshal(vault.ProfileUpdate{Email: "b@example.com", Password: &[]string{vaultTestPassword}[0], Note: &[]string{vaultTestNote}[0]})
	preview, _ := json.Marshal(vault.ProfileImportInput{DryRun: true, Sources: []vault.ImportSource{{Text: "c@example.com----" + vaultTestPassword + "----" + vaultTestNote}}})
	for _, request := range []struct{ method, target, body string }{
		{http.MethodGet, "/api/vault", ""},
		{http.MethodPost, "/api/vault/profiles/import", string(preview)},
		{http.MethodPost, "/api/vault/profiles/update", string(update)},
		{http.MethodPost, "/api/vault/families/import", `{"text":"a@example.com\nb@example.com"}`},
	} {
		recorder := vaultRequest(handler, request.method, request.target, request.body)
		body := recorder.Body.String()
		if recorder.Code != http.StatusOK || strings.Contains(body, vaultTestPassword) || strings.Contains(body, vaultTestNote) || strings.Contains(body, vaultTestKey) {
			t.Fatalf("%s %s status=%d body=%s", request.method, request.target, recorder.Code, body)
		}
	}
	list := vaultRequest(handler, http.MethodGet, "/api/vault", "")
	var view vault.View
	if err := json.Unmarshal(list.Body.Bytes(), &view); err != nil || len(view.Profiles) != 2 || view.Profiles[0].TOTPLast4 != "3PXP" || len(view.Families) != 1 {
		t.Fatalf("列表 err=%v body=%s", err, list.Body.String())
	}
	if unchanged := vaultRequest(handler, http.MethodGet, "/api/vault?since="+view.Revision, ""); unchanged.Code != http.StatusOK ||
		strings.TrimSpace(unchanged.Body.String()) != `{"revision":"`+view.Revision+`","unchanged":true}` {
		t.Fatalf("since 相同 = %s", unchanged.Body.String())
	}
	reveal := vaultRequest(handler, http.MethodPost, "/api/vault/profiles/reveal", `{"email":"A@Example.com"}`)
	var secret vault.ProfileSecret
	if err := json.Unmarshal(reveal.Body.Bytes(), &secret); err != nil || secret.Password != vaultTestPassword || secret.TOTP != vaultTestKey ||
		secret.Note != vaultTestNote || secret.TOTPCode == nil || len(secret.TOTPCode.Code) != 6 || secret.LineFormat != "dash" {
		t.Fatalf("取出 status=%d body=%s", reveal.Code, reveal.Body.String())
	}
	export := vaultRequest(handler, http.MethodPost, "/api/vault/profiles/export", `{"emails":["a@example.com","zz@example.com"],"include_note":true}`)
	if !strings.Contains(export.Body.String(), vaultTestNote) || !strings.Contains(export.Body.String(), `"missing":["zz@example.com"]`) {
		t.Fatalf("导出 = %s", export.Body.String())
	}
}

// TestVaultBadBodies 坏 JSON 400 且不回显内容；超过 8 MiB 413；错误映射；context.Canceled 不写响应
func TestVaultBadBodies(t *testing.T) {
	admin := newVaultAdmin()
	handler := vaultHandler(admin)
	for _, body := range []string{`{"sources":[{"text":"Fake-Pass-9999`, `{"email":"a@example.com","password":Fake-Pass-9999}`, `{"email":1} {"x":"Fake-Pass-9999"}`, `["Fake-Pass-9999"]`, ``} {
		recorder := vaultRequest(handler, http.MethodPost, "/api/vault/profiles/import", body)
		if recorder.Code != http.StatusBadRequest || strings.Contains(recorder.Body.String(), "Fake-Pass-9999") || vaultErrorCode(t, recorder) != vault.CodeInvalidRequest ||
			!strings.Contains(recorder.Body.String(), "请求体不是有效的 JSON") || strings.Contains(recorder.Body.String(), "invalid character") {
			t.Fatalf("坏 JSON %q status=%d body=%s", body, recorder.Code, recorder.Body.String())
		}
	}
	huge := `{"sources":[{"text":"` + strings.Repeat("x", 8<<20) + `"}]}`
	recorder := vaultRequest(handler, http.MethodPost, "/api/vault/profiles/import", huge)
	if recorder.Code != http.StatusRequestEntityTooLarge || vaultErrorCode(t, recorder) != vault.CodeTooLarge || !strings.Contains(recorder.Body.String(), "8 MB") {
		t.Fatalf("超大请求体 status=%d body=%.200s", recorder.Code, recorder.Body.String())
	}
	// 服务端检查：取出不存在 404、只要验证码而没有 2FA 404、mode 不认识 400、导入 4 MiB 以上 413
	seedVault(t, handler)
	if _, err := admin.store.UpdateProfile(context.Background(), vault.ProfileUpdate{Email: "nototp@example.com", Password: &[]string{"x"}[0]}); err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct {
		target, body string
		status       int
		code         string
	}{
		{"/api/vault/profiles/reveal", `{"email":"nobody@example.com"}`, 404, vault.CodeNotFound},
		{"/api/vault/profiles/reveal", `{"email":"nototp@example.com","fields":["totp_code"]}`, 404, vault.CodeTOTPMissing},
		{"/api/vault/profiles/reveal", `{"email":"a@example.com","fields":["cookie"]}`, 400, vault.CodeInvalidRequest},
		{"/api/vault/profiles/reveal", `{"email":"bad"}`, 400, vault.CodeInvalidEmail},
		{"/api/vault/profiles/import", `{"mode":"x","sources":[{"text":"a"}]}`, 400, vault.CodeInvalidRequest},
		{"/api/vault/profiles/import", `{"sources":[{"text":"` + strings.Repeat("y", 4<<20+1) + `"}]}`, 413, vault.CodeTooLarge},
		{"/api/vault/start-empty", `{"file":"other"}`, 400, vault.CodeInvalidRequest},
		{"/api/vault/start-empty", `{"file":"profiles"}`, 409, vault.CodeConflict},
		{"/api/vault/families", `{"primary":"a@example.com","members":[]}`, 400, vault.CodeFamilyEmpty},
	} {
		recorder := vaultRequest(handler, http.MethodPost, item.target, item.body)
		if recorder.Code != item.status || vaultErrorCode(t, recorder) != item.code {
			t.Fatalf("%s %.80s status=%d body=%.300s，期望 %d %s", item.target, item.body, recorder.Code, recorder.Body.String(), item.status, item.code)
		}
	}
	// 缺省字段时 2FA 缺失只给 totp_code_error，其他字段照常
	soft := vaultRequest(handler, http.MethodPost, "/api/vault/profiles/reveal", `{"email":"nototp@example.com"}`)
	if soft.Code != http.StatusOK || !strings.Contains(soft.Body.String(), `"totp_code_error":"missing"`) || !strings.Contains(soft.Body.String(), `"password":"x"`) {
		t.Fatalf("软失败 = %d %s", soft.Code, soft.Body.String())
	}
	if recorder := vaultRequest(handler, http.MethodDelete, "/api/vault/families/fam-aaaaaaaaaaaa?rev=abc", ""); recorder.Code != http.StatusBadRequest {
		t.Fatalf("rev 不是整数 status=%d", recorder.Code)
	}
	admin.err = &vault.Error{Status: http.StatusConflict, Code: vault.CodeUnreadable, Message: "账号资料文件无法读取"}
	if recorder := vaultRequest(handler, http.MethodGet, "/api/vault", ""); recorder.Code != http.StatusConflict || vaultErrorCode(t, recorder) != vault.CodeUnreadable {
		t.Fatalf("错误映射 status=%d", recorder.Code)
	}
	admin.err = &vault.Error{Status: http.StatusConflict, Code: vault.CodeSuspectUnacked, Message: "请核对"}
	if recorder := vaultRequest(handler, http.MethodPost, "/api/vault/profiles/import", `{"sources":[]}`); recorder.Code != http.StatusConflict || vaultErrorCode(t, recorder) != vault.CodeSuspectUnacked {
		t.Fatalf("suspect status=%d", recorder.Code)
	}
	admin.err = context.Canceled
	if recorder := vaultRequest(handler, http.MethodGet, "/api/vault", ""); recorder.Body.Len() != 0 {
		t.Fatalf("取消时不应写响应: %s", recorder.Body.String())
	}
}

// TestVaultConflictPreview 带新预览的冲突：409，error.code 为 vault_conflict，preview 是打码视图
func TestVaultConflictPreview(t *testing.T) {
	admin := newVaultAdmin()
	handler := vaultHandler(admin)
	seedVault(t, handler)
	body, _ := json.Marshal(vault.ProfileImportInput{Sources: []vault.ImportSource{{Text: "a@example.com----Other-Fake-Pass"}}, ExpectOverwrites: &vault.FieldCounts{}})
	recorder := vaultRequest(handler, http.MethodPost, "/api/vault/profiles/import", string(body))
	var response struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
		Preview vault.ProfileImportResult `json:"preview"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil || recorder.Code != http.StatusConflict || response.Error.Code != vault.CodeConflict ||
		!response.Preview.DryRun || response.Preview.Summary.Overwrites.Password != 1 {
		t.Fatalf("冲突 status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if strings.Contains(recorder.Body.String(), "Other-Fake-Pass") || strings.Contains(recorder.Body.String(), vaultTestPassword) {
		t.Fatalf("冲突预览不应含密码: %s", recorder.Body.String())
	}
}

// TestVaultRouteShapes PUT/DELETE 取到路径里的编号与 rev；families/import 不被当成编号；用 GET 访问 POST 路由 405
func TestVaultRouteShapes(t *testing.T) {
	admin := newVaultAdmin()
	handler := vaultHandler(admin)
	vaultRequest(handler, http.MethodPut, "/api/vault/families/fam-0123456789ab", `{"name":"x"}`)
	vaultRequest(handler, http.MethodDelete, "/api/vault/families/fam-0123456789ab?rev=7", "")
	vaultRequest(handler, http.MethodDelete, "/api/vault/families/fam-0123456789ab", "")
	if len(admin.ids) != 3 || admin.ids[0] != "fam-0123456789ab" || admin.revs[0] == nil || *admin.revs[0] != 7 || admin.revs[1] != nil {
		t.Fatalf("编号与 rev = %v %v", admin.ids, admin.revs)
	}
	if recorder := vaultRequest(handler, http.MethodPost, "/api/vault/families/import", `{"dry_run":true,"text":"a@example.com\nb@example.com"}`); recorder.Code != http.StatusOK || len(admin.ids) != 3 {
		t.Fatalf("families/import 不应被当成编号: status=%d ids=%v", recorder.Code, admin.ids)
	}
	for _, target := range []string{"/api/vault/profiles/reveal", "/api/vault/profiles/export", "/api/vault/profiles/import", "/api/vault/start-empty", "/api/vault/families/import"} {
		if recorder := vaultRequest(handler, http.MethodGet, target, ""); recorder.Code != http.StatusMethodNotAllowed {
			t.Fatalf("GET %s status=%d", target, recorder.Code)
		}
	}
	if recorder := vaultRequest(handler, http.MethodGet, "/api/vault?since=abc", ""); recorder.Code != http.StatusOK || admin.since[len(admin.since)-1] != "abc" {
		t.Fatalf("since = %v", admin.since)
	}
}
