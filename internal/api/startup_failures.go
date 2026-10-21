package api

import (
	"context"
	"net/http"
	"slices"
	"strings"
)

// AdminStartupFailure 表示一个启动 Worker 失败的账户。Kind 为 auth（登录失效）、files（认证文件无法用于启动浏览器）、
// page_blocked（页面受阻：页面打开后填不进提示词或 Run 按钮一直不可用，多半是账户在官网有需要处理的提示）、
// fingerprint（账户 Camoufox 指纹文件损坏，与登录无关）或 ambiguous（启动反复失败、原因不确定）。Confirmed 为真时确定失效，可一键删除：
// 来自确定失效的持久表（auth、files 且账户当前处于需要重新登录状态；page_blocked 为号池健康时连续 3 次页面受阻，账户状态不变），
// 或账户当前处于需要重新登录状态；为假时只读，不能据此删号：内存里的启动失败暂停（fingerprint、ambiguous，以及尚未确认的 page_blocked），
// 或账户当前不是需要重新登录状态的持久记录。Stage 为失败时的启动阶段 1–7（未知为 0），URL 为登录跳转的地址（只用于展示），
// Count 为连续失败次数；FirstAt、LastAt 与 Until（暂停到期，内存里的暂停与确认的 page_blocked 有）为 RFC3339，可为空
type AdminStartupFailure struct {
	ID        string `json:"id"`
	Label     string `json:"label"`
	Kind      string `json:"kind"`
	Confirmed bool   `json:"confirmed"`
	Reason    string `json:"reason,omitempty"`
	Message   string `json:"message,omitempty"`
	Stage     int    `json:"stage"`
	URL       string `json:"url,omitempty"`
	Count     int    `json:"count"`
	FirstAt   string `json:"first_at,omitempty"`
	LastAt    string `json:"last_at,omitempty"`
	Until     string `json:"until,omitempty"`
}

// StartupFailureClearInput 表示要清除启动失败记录的账户；All 为真时清除全部
type StartupFailureClearInput struct {
	AccountIDs []string `json:"account_ids"`
	All        bool     `json:"all"`
}

// startupFailureService 由支持启动失败列表的管理端实现
type startupFailureService interface {
	StartupFailures(context.Context) ([]AdminStartupFailure, error)
	ClearStartupFailures(context.Context, StartupFailureClearInput) (int, error)
}

// handleStartupFailures 返回启动 Worker 失败的账户（管理页“失效账户”卡片）：确定失效的在前，原因不确定的暂停在后
func (s *server) handleStartupFailures(w http.ResponseWriter, r *http.Request) {
	service, ok := s.config.Admin.(startupFailureService)
	if !ok {
		writeAdminError(w, http.StatusNotImplemented, "startup_failures_unsupported", "Startup failures are not supported")
		return
	}
	accounts, err := service.StartupFailures(r.Context())
	if err != nil {
		writeAdminUpstreamError(w, err)
		return
	}
	if accounts == nil {
		accounts = []AdminStartupFailure{}
	}
	writeJSON(w, http.StatusOK, map[string][]AdminStartupFailure{"accounts": accounts})
}

// handleClearStartupFailures 清除启动失败记录（持久表与内存暂停），不改变账户状态；account_ids 为空且 all 为假时返回 400
func (s *server) handleClearStartupFailures(w http.ResponseWriter, r *http.Request) {
	service, ok := s.config.Admin.(startupFailureService)
	if !ok {
		writeAdminError(w, http.StatusNotImplemented, "startup_failures_unsupported", "Startup failures are not supported")
		return
	}
	var input StartupFailureClearInput
	if err := decodeJSON(r, &input); err != nil {
		writeAdminError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if !input.All && !slices.ContainsFunc(input.AccountIDs, func(id string) bool { return strings.TrimSpace(id) != "" }) {
		writeAdminError(w, http.StatusBadRequest, "invalid_request", "account_ids or all is required")
		return
	}
	cleared, err := service.ClearStartupFailures(r.Context(), input)
	if err != nil {
		writeAdminUpstreamError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"cleared": cleared})
}
