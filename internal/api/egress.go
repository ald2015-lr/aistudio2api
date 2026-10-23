package api

import (
	"context"
	"net/http"
)

// 出口限流判定的管理动作：撤销判定（30 分钟内不再判定）与恢复判定。只改内存，不落盘，也不是配置项
const (
	EgressActionWithdraw = "withdraw"
	EgressActionResume   = "resume"
)

// egressActionService 由支持出口限流管理动作的管理端实现；id 为状态接口给出的出口编号（AdminEgress.ID），
// action 为 EgressActionWithdraw 或 EgressActionResume，返回动作后的最新状态
type egressActionService interface {
	EgressAction(ctx context.Context, id string, action string) (AdminStatus, error)
}

// registerEgress 注册出口限流判定的管理动作路由
func (s *server) registerEgress(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/egress/{id}/withdraw", s.handleEgressAction(EgressActionWithdraw))
	mux.HandleFunc("POST /api/egress/{id}/resume", s.handleEgressAction(EgressActionResume))
}

// handleEgressAction 执行出口的管理动作；管理端没有实现时返回 404
func (s *server) handleEgressAction(action string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		service, ok := s.config.Admin.(egressActionService)
		if !ok {
			writeAdminError(w, http.StatusNotFound, "not_found", "Egress actions are not supported")
			return
		}
		status, err := service.EgressAction(r.Context(), r.PathValue("id"), action)
		if err != nil {
			writeAdminUpstreamError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, status)
	}
}
