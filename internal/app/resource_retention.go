package app

import (
	"context"
	"fmt"
	"strings"
	"time"
)

const (
	// resourceReclaimFirstDelay 为生成服务启动后第一次清理到期资源绑定前的等待（不和启动时的目录同步、预热挤在一起）
	resourceReclaimFirstDelay = 2 * time.Minute
	// resourceReclaimInterval 为两轮清理的间隔；上一轮达到账户上限、还有没处理的账户时按 resourceReclaimBacklogInterval 接着清
	resourceReclaimInterval        = 10 * time.Minute
	resourceReclaimBacklogInterval = time.Minute
	// resourceReclaimBatch 为每轮最多写回 runtime-state.json 的账户数：升级后第一次清理积压时不集中写盘
	resourceReclaimBatch = 200
)

// reclaimResourceBindings 在生成服务运行期间定期清理过了保留期的本地资源绑定（Live 续接凭证、视频任务与结果文件，
// 见 aistudio.ReclaimExpiredResourceBindings；不删 Drive 上的文件），ctx 取消时返回
func (service *trackedService) reclaimResourceBindings(ctx context.Context) {
	if service == nil || service.pool == nil {
		return
	}
	timer := time.NewTimer(resourceReclaimFirstDelay)
	defer timer.Stop()
	cursor := ""
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		next, backlog := service.reclaimResourceBindingsOnce(ctx, time.Now(), cursor)
		cursor = next
		if backlog {
			timer.Reset(resourceReclaimBacklogInterval)
		} else {
			timer.Reset(resourceReclaimInterval)
		}
	}
}

// reclaimResourceBindingsOnce 执行一轮清理并写日志（清理了绑定或有失败时才写），返回下一轮的起点与是否还有积压
func (service *trackedService) reclaimResourceBindingsOnce(ctx context.Context, now time.Time, cursor string) (string, bool) {
	result := service.pool.ReclaimExpiredResourceBindings(ctx, now, resourceReclaimBatch, cursor)
	if result.Bindings > 0 {
		kinds := make([]string, 0, 3)
		for _, item := range []struct{ kind, name string }{
			{"bidi-session", "Live 续接"}, {"video-operation", "视频任务"}, {"video-file", "视频文件"},
		} {
			if count := result.ByKind[item.kind]; count > 0 {
				kinds = append(kinds, fmt.Sprintf("%s %d", item.name, count))
			}
		}
		service.requests.log("service", "INFO", fmt.Sprintf(
			"资源绑定到期清理 | 账户=%d | 绑定=%d（%s） | 账户忙推后=%d | 待处理=%d",
			result.Accounts, result.Bindings, strings.Join(kinds, "、"), result.Busy, result.Remaining,
		))
	}
	if result.Failed > 0 {
		service.requests.log("service", "WARN", fmt.Sprintf(
			"资源绑定到期清理失败 | 账户=%d | 错误=%v", result.Failed, result.Err,
		))
	}
	return result.Next, result.Remaining > 0
}
