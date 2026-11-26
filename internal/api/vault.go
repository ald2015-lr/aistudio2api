package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/Mag1cFall/AIStudio2API/internal/vault"
)

// 账号资料（密码、辅助邮箱、2FA、备注）与家庭组：只用于管理页的查看、复制与分类显示，不参与登录与调度。
// 全部在 /api/ 链上（同源检查、管理令牌、8 MiB 请求体上限、Cache-Control: no-store，请求体与响应体都不记录）。
// 列表与预览只有打码视图；明文只经 POST /api/vault/profiles/reveal 与 export 返回（另设 Pragma: no-cache）。
// 邮箱只放在 JSON 请求体里，不进 URL（反向代理的访问日志只记 URL）；家庭组编号可以进路径。
// 错误文案固定（不回显 err.Error()、请求内容与文件内容）

// VaultService 由支持账号资料与家庭组的管理端实现
type VaultService interface {
	Vault(ctx context.Context, since string) (vault.View, error)
	ImportProfiles(ctx context.Context, input vault.ProfileImportInput) (vault.ProfileImportResult, error)
	UpdateProfile(ctx context.Context, input vault.ProfileUpdate) (vault.ProfileUpdateResult, error)
	DeleteProfiles(ctx context.Context, input vault.ProfileDeleteInput) (vault.ProfileDeleteResult, error)
	RevealProfile(ctx context.Context, input vault.RevealInput) (vault.ProfileSecret, error)
	ExportProfiles(ctx context.Context, input vault.ExportInput) (vault.ProfileExport, error)
	ImportFamilies(ctx context.Context, input vault.FamilyImportInput) (vault.FamilyImportResult, error)
	CreateFamily(ctx context.Context, input vault.FamilyCreate) (vault.FamilyBlockResult, error)
	UpdateFamily(ctx context.Context, id string, input vault.FamilyUpdate) (vault.FamilyBlockResult, error)
	DissolveFamily(ctx context.Context, id string, rev *int64) (vault.FamilyDissolveResult, error)
	StartEmpty(ctx context.Context, file string) (vault.StartEmptyResult, error)
}

func (s *server) registerVault(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/vault", s.handleVault)
	mux.HandleFunc("POST /api/vault/profiles/import", s.handleVaultImportProfiles)
	mux.HandleFunc("POST /api/vault/profiles/update", s.handleVaultUpdateProfile)
	mux.HandleFunc("POST /api/vault/profiles/delete", s.handleVaultDeleteProfiles)
	mux.HandleFunc("POST /api/vault/profiles/reveal", s.handleVaultRevealProfile)
	mux.HandleFunc("POST /api/vault/profiles/export", s.handleVaultExportProfiles)
	mux.HandleFunc("POST /api/vault/families/import", s.handleVaultImportFamilies)
	mux.HandleFunc("POST /api/vault/families", s.handleVaultCreateFamily)
	mux.HandleFunc("PUT /api/vault/families/{id}", s.handleVaultUpdateFamily)
	mux.HandleFunc("DELETE /api/vault/families/{id}", s.handleVaultDissolveFamily)
	mux.HandleFunc("POST /api/vault/start-empty", s.handleVaultStartEmpty)
}

func (s *server) vaultService(w http.ResponseWriter) (VaultService, bool) {
	service, ok := s.config.Admin.(VaultService)
	if !ok {
		writeAdminError(w, http.StatusNotImplemented, vault.CodeUnsupported, "账号资料与家庭组不可用（管理端不支持）")
	}
	return service, ok
}

var errVaultTrailing = errors.New("trailing data")

// decodeVaultBody 解析请求体：超过上限 413、其余错误 400，文案固定，从不回显解析错误的原文（语法错误会带出请求里的字符）
func decodeVaultBody(w http.ResponseWriter, r *http.Request, target any) bool {
	decoder := json.NewDecoder(r.Body)
	err := decoder.Decode(target)
	if err == nil {
		var trailing json.RawMessage
		if extra := decoder.Decode(&trailing); !errors.Is(extra, io.EOF) {
			err = extra
			if err == nil {
				err = errVaultTrailing
			}
		}
	}
	if err == nil {
		return true
	}
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		writeAdminError(w, http.StatusRequestEntityTooLarge, vault.CodeTooLarge, "请求体超过 8 MB，请分批导入")
		return false
	}
	writeAdminError(w, http.StatusBadRequest, vault.CodeInvalidRequest, "请求体不是有效的 JSON")
	return false
}

// writeVaultError 写错误：带新预览的冲突写 409 {"error":{…},"preview":{…}}，其余照管理接口的通用写法（文案是固定的中文说明）
func writeVaultError(w http.ResponseWriter, err error) {
	var conflict *vault.ConflictError
	if errors.As(err, &conflict) {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error":   map[string]string{"code": conflict.ErrorCode(), "message": conflict.Error()},
			"preview": conflict.Preview,
		})
		return
	}
	writeAdminUpstreamError(w, err)
}

func noStoreSecret(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
}

func (s *server) handleVault(w http.ResponseWriter, r *http.Request) {
	service, ok := s.vaultService(w)
	if !ok {
		return
	}
	view, err := service.Vault(r.Context(), r.URL.Query().Get("since"))
	if err != nil {
		writeVaultError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	if view.Unchanged {
		writeJSON(w, http.StatusOK, map[string]any{"revision": view.Revision, "unchanged": true})
		return
	}
	if view.Profiles == nil {
		view.Profiles = []vault.ProfileView{}
	}
	if view.Families == nil {
		view.Families = []vault.FamilyGroup{}
	}
	writeJSON(w, http.StatusOK, view)
}

func (s *server) handleVaultImportProfiles(w http.ResponseWriter, r *http.Request) {
	service, ok := s.vaultService(w)
	if !ok {
		return
	}
	var input vault.ProfileImportInput
	if !decodeVaultBody(w, r, &input) {
		return
	}
	result, err := service.ImportProfiles(r.Context(), input)
	if err != nil {
		writeVaultError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *server) handleVaultUpdateProfile(w http.ResponseWriter, r *http.Request) {
	service, ok := s.vaultService(w)
	if !ok {
		return
	}
	var input vault.ProfileUpdate
	if !decodeVaultBody(w, r, &input) {
		return
	}
	result, err := service.UpdateProfile(r.Context(), input)
	if err != nil {
		writeVaultError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *server) handleVaultDeleteProfiles(w http.ResponseWriter, r *http.Request) {
	service, ok := s.vaultService(w)
	if !ok {
		return
	}
	var input vault.ProfileDeleteInput
	if !decodeVaultBody(w, r, &input) {
		return
	}
	result, err := service.DeleteProfiles(r.Context(), input)
	if err != nil {
		writeVaultError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *server) handleVaultRevealProfile(w http.ResponseWriter, r *http.Request) {
	service, ok := s.vaultService(w)
	if !ok {
		return
	}
	noStoreSecret(w)
	var input vault.RevealInput
	if !decodeVaultBody(w, r, &input) {
		return
	}
	secret, err := service.RevealProfile(r.Context(), input)
	if err != nil {
		writeVaultError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, secret)
}

func (s *server) handleVaultExportProfiles(w http.ResponseWriter, r *http.Request) {
	service, ok := s.vaultService(w)
	if !ok {
		return
	}
	noStoreSecret(w)
	var input vault.ExportInput
	if !decodeVaultBody(w, r, &input) {
		return
	}
	export, err := service.ExportProfiles(r.Context(), input)
	if err != nil {
		writeVaultError(w, err)
		return
	}
	if export.Lines == nil {
		export.Lines = []string{}
	}
	if export.Missing == nil {
		export.Missing = []string{}
	}
	if export.Lossy == nil {
		export.Lossy = []vault.LossyLine{}
	}
	writeJSON(w, http.StatusOK, export)
}

func (s *server) handleVaultImportFamilies(w http.ResponseWriter, r *http.Request) {
	service, ok := s.vaultService(w)
	if !ok {
		return
	}
	var input vault.FamilyImportInput
	if !decodeVaultBody(w, r, &input) {
		return
	}
	result, err := service.ImportFamilies(r.Context(), input)
	if err != nil {
		writeVaultError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *server) handleVaultCreateFamily(w http.ResponseWriter, r *http.Request) {
	service, ok := s.vaultService(w)
	if !ok {
		return
	}
	var input vault.FamilyCreate
	if !decodeVaultBody(w, r, &input) {
		return
	}
	result, err := service.CreateFamily(r.Context(), input)
	if err != nil {
		writeVaultError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *server) handleVaultUpdateFamily(w http.ResponseWriter, r *http.Request) {
	service, ok := s.vaultService(w)
	if !ok {
		return
	}
	var input vault.FamilyUpdate
	if !decodeVaultBody(w, r, &input) {
		return
	}
	result, err := service.UpdateFamily(r.Context(), r.PathValue("id"), input)
	if err != nil {
		writeVaultError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *server) handleVaultDissolveFamily(w http.ResponseWriter, r *http.Request) {
	service, ok := s.vaultService(w)
	if !ok {
		return
	}
	var rev *int64
	if text := r.URL.Query().Get("rev"); text != "" {
		value, err := strconv.ParseInt(text, 10, 64)
		if err != nil {
			writeAdminError(w, http.StatusBadRequest, vault.CodeInvalidRequest, "rev 应为整数")
			return
		}
		rev = &value
	}
	result, err := service.DissolveFamily(r.Context(), r.PathValue("id"), rev)
	if err != nil {
		writeVaultError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *server) handleVaultStartEmpty(w http.ResponseWriter, r *http.Request) {
	service, ok := s.vaultService(w)
	if !ok {
		return
	}
	var input struct {
		File string `json:"file"`
	}
	if !decodeVaultBody(w, r, &input) {
		return
	}
	result, err := service.StartEmpty(r.Context(), input.File)
	if err != nil {
		writeVaultError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}
