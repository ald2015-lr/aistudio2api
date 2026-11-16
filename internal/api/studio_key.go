package api

import (
	"context"
	"net/http"
	"time"
)

// AI Studio API Key（账户在 AI Studio API Keys 页面上的 Key）：管理页按账户获取并记录 Key、项目名与项目编号，只提供复制，不做转发。
// 列表接口只返回 Key 末四位；完整 Key 只经 POST /api/studio-keys/{id}/reveal 返回（需要管理令牌，Cache-Control: no-store）

// StudioKeyRecord 为一个账户的 Key 记录（不含完整 Key）
type StudioKeyRecord struct {
	AccountID string `json:"account_id"`
	Label     string `json:"label,omitempty"`
	// Status 为最近一次获取的结论：ok（读到 Key）、none（页面上没有 Key 也不能创建）、failed、skipped（账户未登录、停用等）
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
	// HasKey 表示保存着 Key（最近一次失败时仍保留上一次读到的 Key）；Last4 为它的末四位
	HasKey         bool       `json:"has_key"`
	Last4          string     `json:"last4,omitempty"`
	ProjectName    string     `json:"project_name,omitempty"`
	ProjectNumber  string     `json:"project_number,omitempty"`
	ProjectDisplay string     `json:"project_display,omitempty"`
	Created        bool       `json:"created,omitempty"`
	KeyCount       int        `json:"key_count,omitempty"`
	FetchedAt      *time.Time `json:"fetched_at,omitempty"`
	CreatedAt      *time.Time `json:"created_at,omitempty"`
	AttemptAt      *time.Time `json:"attempt_at,omitempty"`
}

// StudioKeyStatus 为获取任务与全部记录
type StudioKeyStatus struct {
	// DelaySeconds 为相邻两个账户之间的间隔（STUDIO_KEY_DELAY，保存配置后立即生效）
	DelaySeconds int      `json:"delay_seconds"`
	Running      bool     `json:"running"`
	Current      string   `json:"current,omitempty"`
	Queue        []string `json:"queue"`
	// NextAt 为队列里下一个账户开始的时间（在间隔中等待时）
	NextAt  *time.Time        `json:"next_at,omitempty"`
	Records []StudioKeyRecord `json:"records"`
}

// StudioKeyFetchInput 为要获取 Key 的账户；All 为全部账户（按账户列表顺序逐个排队，跳过规则照旧），
// MissingOnly 时跳过已经保存着 Key 的账户（中途停止或失败后再点一次只补没拿到的）
type StudioKeyFetchInput struct {
	AccountIDs  []string `json:"account_ids"`
	All         bool     `json:"all,omitempty"`
	MissingOnly bool     `json:"missing_only,omitempty"`
}

// StudioKeyExport 为全部保存着的完整 Key（只由 export 接口返回，给“复制全部 Key”用）
type StudioKeyExport struct {
	Keys []StudioKeySecret `json:"keys"`
}

// StudioKeySecret 为完整 Key（只由 reveal 接口返回）
type StudioKeySecret struct {
	AccountID     string `json:"account_id"`
	Key           string `json:"key"`
	ProjectName   string `json:"project_name,omitempty"`
	ProjectNumber string `json:"project_number,omitempty"`
}

// StudioKeyService 由支持获取 AI Studio API Key 的管理端实现
type StudioKeyService interface {
	StudioKeys(context.Context) (StudioKeyStatus, error)
	FetchStudioKeys(context.Context, StudioKeyFetchInput) (StudioKeyStatus, error)
	RevealStudioKey(context.Context, string) (StudioKeySecret, error)
	// StopStudioKeys 清空排队中的账户（正在处理的那个照常做完）
	StopStudioKeys(context.Context) (StudioKeyStatus, error)
	ExportStudioKeys(context.Context) (StudioKeyExport, error)
}

func (s *server) studioKeyService(w http.ResponseWriter) (StudioKeyService, bool) {
	service, ok := s.config.Admin.(StudioKeyService)
	if !ok {
		writeAdminError(w, http.StatusNotImplemented, "studio_key_unsupported", "AI Studio API key is not supported")
	}
	return service, ok
}

func (s *server) handleStudioKeys(w http.ResponseWriter, r *http.Request) {
	service, ok := s.studioKeyService(w)
	if !ok {
		return
	}
	status, err := service.StudioKeys(r.Context())
	if err != nil {
		writeAdminUpstreamError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, status)
}

func (s *server) handleFetchStudioKeys(w http.ResponseWriter, r *http.Request) {
	service, ok := s.studioKeyService(w)
	if !ok {
		return
	}
	var input StudioKeyFetchInput
	if err := decodeJSON(r, &input); err != nil {
		writeAdminError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if len(input.AccountIDs) == 0 && !input.All {
		writeAdminError(w, http.StatusBadRequest, "invalid_request", "account_ids is required")
		return
	}
	status, err := service.FetchStudioKeys(r.Context(), input)
	if err != nil {
		writeAdminUpstreamError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, status)
}

func (s *server) handleRevealStudioKey(w http.ResponseWriter, r *http.Request) {
	service, ok := s.studioKeyService(w)
	if !ok {
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	secret, err := service.RevealStudioKey(r.Context(), r.PathValue("id"))
	if err != nil {
		writeAdminUpstreamError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, secret)
}

func (s *server) handleStopStudioKeys(w http.ResponseWriter, r *http.Request) {
	service, ok := s.studioKeyService(w)
	if !ok {
		return
	}
	status, err := service.StopStudioKeys(r.Context())
	if err != nil {
		writeAdminUpstreamError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, status)
}

func (s *server) handleExportStudioKeys(w http.ResponseWriter, r *http.Request) {
	service, ok := s.studioKeyService(w)
	if !ok {
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	export, err := service.ExportStudioKeys(r.Context())
	if err != nil {
		writeAdminUpstreamError(w, err)
		return
	}
	if export.Keys == nil {
		export.Keys = []StudioKeySecret{}
	}
	writeJSON(w, http.StatusOK, export)
}
