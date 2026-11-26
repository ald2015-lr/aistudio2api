package api

import (
	"context"
	"net/http"
	"time"
)

// OnboardingPolicy 为新账户自动处理设置
type OnboardingPolicy struct {
	AutoVerify  bool `json:"auto_verify"`
	AutoEnable  bool `json:"auto_enable"`
	BatchSize   int  `json:"batch_size"`
	Concurrency int  `json:"concurrency"`
}

// OnboardingEvent 为一条新账户处理结果；Source 为 reauth 时是已有账户重新上传认证文件后的处理，Outcome 为重新上传后的结果：
// restored（已恢复）、auth_required（仍需登录）、verify_required（仍需验证）、forbidden（仍有 403）、restricted（仍受限）、
// manual_disabled（跳过，手动停用）、verify_off（已重新载入，自动验证已关闭）、error（验证出错）、skipped（跳过）
type OnboardingEvent struct {
	AccountID string    `json:"account_id"`
	Result    string    `json:"result"`
	Detail    string    `json:"detail,omitempty"`
	At        time.Time `json:"at"`
	Source    string    `json:"source,omitempty"`
	Outcome   string    `json:"outcome,omitempty"`
}

// OnboardingStatus 为新账户自动处理的队列状态
type OnboardingStatus struct {
	Policy     OnboardingPolicy `json:"policy"`
	Pending    int              `json:"pending"`
	Processing []string         `json:"processing"`
	// NextBatchSeconds 为下一批开始前的秒数：0 表示即将开始，-1 表示队列为空
	NextBatchSeconds int               `json:"next_batch_seconds"`
	Counts           map[string]int    `json:"counts"`
	Recent           []OnboardingEvent `json:"recent"`
	// Reauth 为待处理与处理中里换上新认证文件后重新验证的账户数（已计入 Pending 与 Processing）
	Reauth int `json:"reauth"`
	// Unchanged 为本次运行中健康账户换上了内容相同的认证文件、没有重新认证的次数
	Unchanged int `json:"unchanged"`
	// SameLogin 为本次运行中状态正常的启用账户换上了内容有变化、登录签名没变的认证文件，按原口径没有重新载入的次数
	// （同一账户每小时最多计一次）
	SameLogin int `json:"same_login"`
	// NotReuploaded 为本次运行中失效账户换上的认证文件被当成本服务自己的文件（保留了原文件的修改时间）或共用账户目录的其他进程回写
	// （只差轮换 Cookie 或 AI Studio 的 localStorage）、没有按重新上传处理的次数（其他进程回写同一账户每小时最多计一次）
	NotReuploaded int `json:"not_reuploaded"`
	// Loading 为账户目录扫描检测到认证文件更新、还没载入完的账户数（批量重新上传时每轮扫描最多载入 40 个，载入前账户列表仍显示原来的状态）
	Loading int `json:"loading"`
	// ReauthOutcomes 为处理记录里各账户最近一次重新上传后的结果计数（键同 OnboardingEvent.Outcome），没有时省略；
	// 这些记录不计入 Counts
	ReauthOutcomes map[string]int `json:"reauth_outcomes,omitempty"`
}

// onboardingService 由支持新账户自动处理的管理端实现
type onboardingService interface {
	Onboarding(context.Context) (OnboardingStatus, error)
	UpdateOnboarding(context.Context, OnboardingPolicy) (OnboardingStatus, error)
	QueueDisabledAccounts(context.Context) (OnboardingStatus, error)
}

func (s *server) handleGetOnboarding(w http.ResponseWriter, r *http.Request) {
	service, ok := s.config.Admin.(onboardingService)
	if !ok {
		writeAdminError(w, http.StatusNotImplemented, "onboarding_unsupported", "Onboarding is not supported")
		return
	}
	status, err := service.Onboarding(r.Context())
	if err != nil {
		writeAdminUpstreamError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, status)
}

func (s *server) handleUpdateOnboarding(w http.ResponseWriter, r *http.Request) {
	service, ok := s.config.Admin.(onboardingService)
	if !ok {
		writeAdminError(w, http.StatusNotImplemented, "onboarding_unsupported", "Onboarding is not supported")
		return
	}
	var policy OnboardingPolicy
	if err := decodeJSON(r, &policy); err != nil {
		writeAdminError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	status, err := service.UpdateOnboarding(r.Context(), policy)
	if err != nil {
		writeAdminUpstreamError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, status)
}

func (s *server) handleQueueDisabledOnboarding(w http.ResponseWriter, r *http.Request) {
	service, ok := s.config.Admin.(onboardingService)
	if !ok {
		writeAdminError(w, http.StatusNotImplemented, "onboarding_unsupported", "Onboarding is not supported")
		return
	}
	status, err := service.QueueDisabledAccounts(r.Context())
	if err != nil {
		writeAdminUpstreamError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, status)
}
