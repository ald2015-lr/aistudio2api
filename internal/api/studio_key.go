package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// AI Studio API Key（账户在 AI Studio API Keys 页面上的 Key）：管理页按账户获取并记录 Key、项目名、项目编号、显示名与项目 ID，
// 只提供复制，不做转发。列表接口只返回 Key 末四位；完整 Key 只经 POST /api/studio-keys/{id}/reveal 与 export 返回（需要管理令牌，
// Cache-Control: no-store）。POST /api/studio-keys/clear 清除记录：没有 Key 的记录删掉，保存着 Key 的只清掉失败或跳过的信息，不删除账户的 Key；
// 例外是作废的 Key（与其他账户保存的相同，多半是页面自己的公共 Key，不是这个账户的）：连同记录删掉。作废的 Key 在列表里是 invalid、
// 不算有 Key，reveal 返回 409 studio_key_invalid，export 不含它

// StudioKeyClearLimit 为一次清除最多的账户数
const StudioKeyClearLimit = 5000

// StudioKeyRecord 为一个账户的 Key 记录（不含完整 Key）
type StudioKeyRecord struct {
	AccountID string `json:"account_id"`
	Label     string `json:"label,omitempty"`
	// Status 为最近一次获取的结论：ok（读到 Key）、none（页面上没有 Key 也不能创建）、failed、skipped（账户未登录、停用等），
	// 或 invalid（保存着的 Key 与其他账户相同、已作废：has_key 为假，不给末四位与项目，Reason 写明原因）
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
	// HasKey 表示保存着 Key（最近一次失败时仍保留上一次读到的 Key）；Last4 为它的末四位；
	// ProjectCreated 表示 Key 所在的项目是获取时经页面新建的（账户原先没有 Google Cloud 项目）
	HasKey         bool   `json:"has_key"`
	Last4          string `json:"last4,omitempty"`
	ProjectName    string `json:"project_name,omitempty"`
	ProjectNumber  string `json:"project_number,omitempty"`
	ProjectDisplay string `json:"project_display,omitempty"`
	// ProjectID 为项目 ID（如 gen-lang-client-0123456789）
	ProjectID      string     `json:"project_id,omitempty"`
	Created        bool       `json:"created,omitempty"`
	ProjectCreated bool       `json:"project_created,omitempty"`
	KeyCount       int        `json:"key_count,omitempty"`
	FetchedAt      *time.Time `json:"fetched_at,omitempty"`
	CreatedAt      *time.Time `json:"created_at,omitempty"`
	AttemptAt      *time.Time `json:"attempt_at,omitempty"`
}

// StudioKeyStatus 为获取任务与全部记录
type StudioKeyStatus struct {
	// DelaySeconds 为相邻两个账户之间的间隔（STUDIO_KEY_DELAY，保存配置后立即生效）
	DelaySeconds int    `json:"delay_seconds"`
	Running      bool   `json:"running"`
	Current      string `json:"current,omitempty"`
	// CurrentFill 表示正在处理的账户是补项目信息（只读，不创建 Key 与项目）
	CurrentFill bool     `json:"current_fill,omitempty"`
	Queue       []string `json:"queue"`
	// NextAt 为队列里下一个账户开始的时间（在间隔中等待时）
	NextAt  *time.Time        `json:"next_at,omitempty"`
	Records []StudioKeyRecord `json:"records"`
}

// StudioKeyFetchInput 为要获取 Key 的账户；All 为全部账户（按账户列表顺序逐个排队，跳过规则照旧），
// MissingOnly 时跳过已经保存着 Key 的账户（中途停止或失败后再点一次只补没拿到的）。
// FillProject 为补项目信息：只排保存着 Key 的账户，只读打开页面找这条已保存的 Key 读它的项目，从不创建 Key 与项目；
// MissingProject 同样是补项目信息，并且只排保存着 Key 却没有项目编号的账户（与 All 一起用）。同一账户既按普通方式又按补项目信息排队时按普通方式
type StudioKeyFetchInput struct {
	AccountIDs     []string `json:"account_ids"`
	All            bool     `json:"all,omitempty"`
	MissingOnly    bool     `json:"missing_only,omitempty"`
	FillProject    bool     `json:"fill_project,omitempty"`
	MissingProject bool     `json:"missing_project,omitempty"`
}

// StudioKeyClearInput 为要清除记录的账户（最多 StudioKeyClearLimit 个）
type StudioKeyClearInput struct {
	AccountIDs []string `json:"account_ids"`
}

// StudioKeyClearCounts 为清除的结果
type StudioKeyClearCounts struct {
	// Deleted 为删掉的记录数（没有 Key 的记录：失败、没有 Key、跳过）
	Deleted int `json:"deleted"`
	// Reset 为清掉失败或跳过信息的记录数（保存着 Key 的记录：Key、项目与时间不动，状态回到成功）
	Reset int `json:"reset"`
	// Busy 为正在处理、没有动的账户数
	Busy int `json:"busy"`
	// Invalid 为删掉的作废记录数（保存着作废的 Key：与其他账户相同，不是这个账户的 Key）
	Invalid int `json:"invalid"`
	// Unknown 为不认识的账户数
	Unknown int `json:"unknown"`
	// Unchanged 为不用清的账户数（没有记录，或保存着 Key 且本来就是成功）
	Unchanged int `json:"unchanged"`
	// Errors 为记录读不出来、没有动的账户数（免得删掉 Key）
	Errors int `json:"errors"`
}

// StudioKeyClearResult 为清除之后的状态与计数
type StudioKeyClearResult struct {
	StudioKeyStatus
	Cleared StudioKeyClearCounts `json:"cleared"`
}

// StudioKeyExport 为全部保存着的完整 Key（只由 export 接口返回，给“复制全部 Key”用）
type StudioKeyExport struct {
	Keys []StudioKeySecret `json:"keys"`
}

// StudioKeySecret 为完整 Key（只由 reveal 接口返回）
type StudioKeySecret struct {
	AccountID      string `json:"account_id"`
	Key            string `json:"key"`
	ProjectName    string `json:"project_name,omitempty"`
	ProjectNumber  string `json:"project_number,omitempty"`
	ProjectDisplay string `json:"project_display,omitempty"`
	ProjectID      string `json:"project_id,omitempty"`
}

// StudioKeyService 由支持获取 AI Studio API Key 的管理端实现
type StudioKeyService interface {
	StudioKeys(context.Context) (StudioKeyStatus, error)
	FetchStudioKeys(context.Context, StudioKeyFetchInput) (StudioKeyStatus, error)
	RevealStudioKey(context.Context, string) (StudioKeySecret, error)
	// StopStudioKeys 清空排队中的账户（正在处理的那个照常做完）
	StopStudioKeys(context.Context) (StudioKeyStatus, error)
	ExportStudioKeys(context.Context) (StudioKeyExport, error)
	// ClearStudioKeys 清除账户的记录：没有 Key 的删掉，保存着 Key 的只清掉失败或跳过的信息（不删除账户的 Key；作废的 Key 连同记录删掉）；
	// 正在处理的账户不动
	ClearStudioKeys(context.Context, StudioKeyClearInput) (StudioKeyClearResult, error)
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
	if input.MissingOnly && (input.FillProject || input.MissingProject) {
		writeAdminError(w, http.StatusBadRequest, "invalid_request", "missing_only cannot be combined with fill_project or missing_project")
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

func (s *server) handleClearStudioKeys(w http.ResponseWriter, r *http.Request) {
	service, ok := s.studioKeyService(w)
	if !ok {
		return
	}
	var input StudioKeyClearInput
	if err := decodeJSON(r, &input); err != nil {
		writeAdminError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	// 先按原始个数拒绝（留出重复与空白的余量），再用 map 去重：去重不随请求体变大成平方增长
	tooMany := func() {
		writeAdminError(w, http.StatusBadRequest, "invalid_request", fmt.Sprintf("too many account_ids (at most %d)", StudioKeyClearLimit))
	}
	if len(input.AccountIDs) > 2*StudioKeyClearLimit {
		tooMany()
		return
	}
	ids := make([]string, 0, min(len(input.AccountIDs), StudioKeyClearLimit))
	seen := make(map[string]bool, cap(ids))
	for _, id := range input.AccountIDs {
		if id = strings.TrimSpace(id); id != "" && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		writeAdminError(w, http.StatusBadRequest, "invalid_request", "account_ids is required")
		return
	}
	if len(ids) > StudioKeyClearLimit {
		tooMany()
		return
	}
	result, err := service.ClearStudioKeys(r.Context(), StudioKeyClearInput{AccountIDs: ids})
	if err != nil {
		writeAdminUpstreamError(w, err)
		return
	}
	if result.Queue == nil {
		result.Queue = []string{}
	}
	if result.Records == nil {
		result.Records = []StudioKeyRecord{}
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, result)
}
