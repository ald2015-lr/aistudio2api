package app

import (
	"context"
	"errors"
	"strings"
)

// runAutoStart 在管理监听就绪后发起一次启动；失败后的重试由 superviseService 负责
func runAutoStart(ctx context.Context, manager *runtimeManager) {
	manager.requests.log("service", "INFO", "自动启动生成服务 | AUTO_START=true")
	_, err := manager.StartService(ctx)
	if err == nil || ctx.Err() != nil {
		return
	}
	var operationErr *adminOperationError
	if errors.As(err, &operationErr) && operationErr.code == "account_required" {
		// start.sh 通过"没有可用账户"识别这种情况并给出排查信息
		manager.requests.log("service", "WARN", "自动启动未成功 | 没有可用账户 | 稍后自动重试")
		return
	}
	manager.requests.log("service", "WARN", "自动启动未成功，稍后自动重试 | 错误="+strings.TrimSpace(err.Error()))
}
