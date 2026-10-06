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

// OnboardingEvent 为一条新账户处理结果
type OnboardingEvent struct {
	AccountID string    `json:"account_id"`
	Result    string    `json:"result"`
	Detail    string    `json:"detail,omitempty"`
	At        time.Time `json:"at"`
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
