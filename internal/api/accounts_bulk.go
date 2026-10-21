package api

import (
	"context"
	"net/http"
	"slices"
	"strings"
)

// BulkEnableInput 为批量启用账户的请求：AccountIDs 为要启用的账户；AllDisabled 为真时启用全部停用账户，忽略 AccountIDs
type BulkEnableInput struct {
	AccountIDs  []string `json:"account_ids"`
	AllDisabled bool     `json:"all_disabled"`
}

// BulkEnableFailure 为一个没有启用的账户：Code 为 account_busy（账户正在验证、登录或处理请求）、
// account_not_found 等错误码，没有对应错误码时为空
type BulkEnableFailure struct {
	ID      string `json:"id"`
	Code    string `json:"code,omitempty"`
	Message string `json:"message"`
}

// BulkEnableResult 为批量启用的结果：Enabled 为本次启用的账户数，Skipped 为原本已启用的账户数，
// Remaining 为超过单次上限、本次没有处理的停用账户数（再次提交即可继续）
type BulkEnableResult struct {
	Enabled   int                 `json:"enabled"`
	Skipped   int                 `json:"skipped"`
	Failed    []BulkEnableFailure `json:"failed"`
	Remaining int                 `json:"remaining"`
}

// bulkEnableService 由支持批量启用账户的管理端实现
type bulkEnableService interface {
	EnableAccounts(context.Context, BulkEnableInput) (BulkEnableResult, error)
}

// handleBulkEnableAccounts 一次请求启用一批账户：原先管理页逐个账户发 PUT，每启用一个都要重算并推送全部账户状态，
// 上万个账户要几个小时
func (s *server) handleBulkEnableAccounts(w http.ResponseWriter, r *http.Request) {
	service, ok := s.config.Admin.(bulkEnableService)
	if !ok {
		writeAdminError(w, http.StatusNotImplemented, "bulk_enable_unsupported", "Bulk enable is not supported")
		return
	}
	var input BulkEnableInput
	if err := decodeJSON(r, &input); err != nil {
		writeAdminError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	ctx := context.WithValue(r.Context(), adminCallerKey{}, adminCallerText(r))
	result, err := service.EnableAccounts(ctx, input)
	if err != nil {
		writeAdminUpstreamError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// BulkDeleteInput 为批量删除账户的请求：AccountIDs 为要删除的账户（去掉首尾空白、去重后不能为空）
type BulkDeleteInput struct {
	AccountIDs []string `json:"account_ids"`
}

// BulkDeleteFailure 为一个没有删除的账户：Code 为 account_busy（等待上限内没有等到账户空闲）等错误码，没有对应错误码时为空
type BulkDeleteFailure struct {
	ID      string `json:"id"`
	Code    string `json:"code,omitempty"`
	Message string `json:"message"`
}

// BulkDeleteResult 为批量删除的结果：Deleted 为本次删除的账户数；Missing 为号池里已经没有的账户数（已被删除或从未载入，
// 同时清除它们的 403 无权限记录，不算失败，也不占单次上限）；Remaining 为号池里还有、超过单次上限本次没有处理的账户数（再次提交即可继续）
type BulkDeleteResult struct {
	Deleted   int                 `json:"deleted"`
	Missing   int                 `json:"missing"`
	Failed    []BulkDeleteFailure `json:"failed"`
	Remaining int                 `json:"remaining"`
}

// bulkDeleteService 由支持批量删除账户的管理端实现
type bulkDeleteService interface {
	DeleteAccounts(context.Context, BulkDeleteInput) (BulkDeleteResult, error)
}

// handleBulkDeleteAccounts 一次请求删除一批账户及其认证目录（管理页“403 无权限账户”的一键删除）：原先只能逐个删除，
// 每删除一个都要重算并推送全部账户状态；account_ids 为空时返回 400
func (s *server) handleBulkDeleteAccounts(w http.ResponseWriter, r *http.Request) {
	service, ok := s.config.Admin.(bulkDeleteService)
	if !ok {
		writeAdminError(w, http.StatusNotImplemented, "bulk_delete_unsupported", "Bulk delete is not supported")
		return
	}
	var input BulkDeleteInput
	if err := decodeJSON(r, &input); err != nil {
		writeAdminError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if !slices.ContainsFunc(input.AccountIDs, func(id string) bool { return strings.TrimSpace(id) != "" }) {
		writeAdminError(w, http.StatusBadRequest, "invalid_request", "account_ids is required")
		return
	}
	ctx := context.WithValue(r.Context(), adminCallerKey{}, adminCallerText(r))
	result, err := service.DeleteAccounts(ctx, input)
	if err != nil {
		writeAdminUpstreamError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}
