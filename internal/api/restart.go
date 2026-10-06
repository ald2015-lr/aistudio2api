package api

import (
	"context"
	"net/http"
)

// serviceRestarter 由支持平滑重建生成服务的管理端实现
type serviceRestarter interface {
	RestartService(context.Context) (AdminStatus, error)
}

// handleRestartService 等待进行中的请求结束后重建生成服务，使已保存的配置生效
func (s *server) handleRestartService(w http.ResponseWriter, r *http.Request) {
	restarter, ok := s.config.Admin.(serviceRestarter)
	if !ok {
		writeAdminError(w, http.StatusNotImplemented, "restart_unsupported", "Service restart is not supported")
		return
	}
	status, err := restarter.RestartService(r.Context())
	if err != nil {
		writeAdminUpstreamError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, status)
}
