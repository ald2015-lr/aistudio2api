package app

import (
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"
)

const (
	// supervisorTick 为检查生成服务状态的周期
	supervisorTick = 2 * time.Second
	// supervisorFirstDelay 为发现服务停止后首次重新启动前的等待
	supervisorFirstDelay = 5 * time.Second
	// supervisorMaxDelay 为一般失败的重试间隔上限
	supervisorMaxDelay = time.Minute
	// supervisorNoAccountMaxDelay 为没有可用账户时的重试间隔上限
	supervisorNoAccountMaxDelay = 5 * time.Minute
)

// serviceIntent 记录用户期望的运行状态：启动（含自动启动、应用配置）置为运行，手动停止置为停止
type serviceIntent struct {
	running atomic.Bool
	// stops 为手动停止的次数：启动过程中用户按了停止时，启动据此放弃，不会在停止之后又把服务拉起来
	stops atomic.Uint64
}

// superviseService 期望运行、但生成服务停在 STOPPED（启动失败或意外停止）时自动重新启动，
// 失败按指数退避重试；手动点击"停止服务"后不会再自动启动
func (manager *runtimeManager) superviseService() {
	ctx := manager.lifecycle
	ticker := time.NewTicker(supervisorTick)
	defer ticker.Stop()
	delay := supervisorFirstDelay
	attempt := 1
	var retryAt time.Time
	reset := func() {
		delay = supervisorFirstDelay
		attempt = 1
		retryAt = time.Time{}
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if !manager.intent.running.Load() {
			reset()
			continue
		}
		// 启动正在进行时不打扰
		if !manager.startMu.TryLock() {
			continue
		}
		manager.startMu.Unlock()
		// 只读取服务状态字段；完整的 Status 要统计全部账户并占用账户池锁，不适合每 2 秒调用
		state := manager.serviceState()
		if state == "" {
			continue
		}
		if state != "STOPPED" {
			if state == "RUNNING" {
				reset()
			}
			continue
		}
		now := time.Now()
		if retryAt.IsZero() {
			retryAt = now.Add(delay)
			continue
		}
		if now.Before(retryAt) {
			continue
		}
		manager.requests.log("service", "WARN", fmt.Sprintf("生成服务未在运行，自动重新启动 | 第 %d 次", attempt))
		_, startErr := manager.startService(ctx, false)
		if ctx.Err() != nil {
			return
		}
		if startErr == nil {
			reset()
			continue
		}
		limit := supervisorMaxDelay
		var operationErr *adminOperationError
		if errors.As(startErr, &operationErr) && operationErr.code == "account_required" {
			limit = supervisorNoAccountMaxDelay
		}
		delay = min(delay*2, limit)
		retryAt = time.Now().Add(delay)
		attempt++
		manager.requests.log("service", "WARN", fmt.Sprintf(
			"自动重新启动失败 | %s 后重试 | 错误=%s", delay, strings.TrimSpace(startErr.Error()),
		))
	}
}

// serviceState 返回当前生成服务的状态（STOPPED、LAUNCHING、RUNNING），不构建账户统计
func (manager *runtimeManager) serviceState() string {
	manager.mu.RLock()
	defer manager.mu.RUnlock()
	if manager.current == nil {
		return ""
	}
	admin, ok := manager.current.admin.(*runtimeAdmin)
	if !ok || admin.service == nil {
		return ""
	}
	return admin.service.State()
}
