package app

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/api"
)

// 出口限流判定（egress_guard.go）与生成服务、Worker 管理器、管理接口的接线：冷启动闸门、选号排队归属的出口、
// 非主用模型出口排队超限的错误，以及管理页的撤销与恢复判定

// egressCoverageRetry 为一次选号里补覆盖被冷启动闸门暂停后再次尝试的间隔：期间按驻留账号该模型额度已用完排队，
// 不在每次复查时都去取一次租约、问一次闸门
const egressCoverageRetry = 5 * time.Second

// egressColdStartGate 返回挂到 Worker 管理器上的冷启动闸门：没有出口判定成立时直接放行（不查出口键）；
// 成立时按账户取出口键与分区通道（EgressKeyOf，在 rebalanceMu 内调用，与 ensureWorker 里其他账户池读取的锁顺序相同），
// 补位并发按当前的 WARM_STARTUP_CONCURRENCY 计算（热更新即生效）。
// IP 池开启且账户没有自己的代理时直接放行：闸门看的是账户现在的默认出口，真正启动时 IP 池另分出口（静态独占一个端口、
// 动态用账户自己的会话），每个出口只有这一个账号（锁顺序 rebalanceMu → proxyPool.mu，IP 池不反向取 rebalanceMu）
func (service *trackedService) egressColdStartGate() coldStartGateFunc {
	return func(accountID, modelID string, path coldStartPath) (func(bool), bool) {
		guard := service.egress
		if !guard.anyLimited() || service.pool == nil || service.workers.ipPool().poolsAccount(accountID) {
			return func(bool) {}, true
		}
		key, lane := service.pool.EgressKeyOf(accountID)
		refill := egressRefillConcurrencyMin
		if service.workers != nil {
			refill = egressRefillLimit(service.workers.warmConcurrencyValue())
		}
		return guard.admitColdStart(key, lane == aistudio.EgressLaneUltra, modelID, path, refill)
	}
}

// egressWaitKey 返回选号按出口排队时归属的出口：判定成立的出口只有一个时就是它（单出口直连时为空串），多个时取建议重试时间最早的
func (service *trackedService) egressWaitKey(limited map[string]struct{}, ultra bool) string {
	best, bestAt, first := "", time.Time{}, true
	now := time.Now()
	for key := range limited {
		if len(limited) == 1 {
			return key
		}
		retryAt := service.egress.verdict(key, ultra, now).RetryAt
		if first || retryAt.Before(bestAt) || retryAt.Equal(bestAt) && key < best {
			best, bestAt, first = key, retryAt, false
		}
	}
	return best
}

// egressQueueState 返回出口通道当前的限速与下一个令牌的时刻（排队进度的“出口限速”“下一个令牌”）：限速取判定器的，
// 下一个令牌取账户池这个出口的计数（一次加锁，只取这一个出口）；只在真正写排队进度时调用
func (service *trackedService) egressQueueState(key string, ultra bool) (int, time.Time) {
	limit := service.egress.verdict(key, ultra, time.Now()).Limit
	if service.pool == nil {
		return limit, time.Time{}
	}
	lane := aistudio.EgressLaneNormal
	if ultra {
		lane = aistudio.EgressLaneUltra
	}
	statuses, _ := service.pool.EgressSnapshotFor([]string{key})
	for _, status := range statuses {
		if status.Key == key {
			if limit == 0 {
				limit = status.Lanes[lane].Limit
			}
			return limit, status.Lanes[lane].NextAt
		}
	}
	return limit, time.Time{}
}

// egressQueueExceeded 为非主用模型按出口排队（出口限速、驻留账号该模型额度已用完）累计超过上限时的错误：对外 429，
// 建议重试时间按出口的排队长度估计（判定器已限定在 5s–60s），并计入状态里的本地 429
func (service *trackedService) egressQueueExceeded(
	selection aistudio.AccountSelection, reason string, key string, ultra bool, waited time.Duration, limit time.Duration,
) error {
	now := time.Now()
	retryAt := service.egress.verdict(key, ultra, now).RetryAt
	if retryAt.IsZero() {
		retryAt = now.Add(egressRetryMin)
	}
	service.egress.noteLocalReject(key, now)
	return &aistudio.CapacityBusyError{
		ModelID: selection.ModelID, Pool: selection.Pool, Reason: reason,
		Waited: waited, Limit: limit, Attempt: 1, Until: retryAt,
	}
}

// EgressAction 为管理页对出口限流判定的动作（撤销判定 30 分钟、恢复判定），返回动作后的最新状态。只改内存，不落盘，也不是配置项
func (admin *runtimeAdmin) EgressAction(ctx context.Context, id string, action string) (api.AdminStatus, error) {
	id = strings.TrimSpace(id)
	var guard *egressGuard
	if admin.service != nil {
		guard = admin.service.egress
	}
	var err error
	switch action {
	case api.EgressActionWithdraw:
		err = guard.withdraw(id, time.Now())
	case api.EgressActionResume:
		err = guard.resume(id, time.Now())
	default:
		err = fmt.Errorf("未知的出口管理动作: %s", action)
	}
	if err != nil {
		return api.AdminStatus{}, err
	}
	return admin.Status(ctx)
}

// egressActionAdmin 为支持出口管理动作的管理端（当前生成服务的 runtimeAdmin）
type egressActionAdmin interface {
	EgressAction(ctx context.Context, id string, action string) (api.AdminStatus, error)
}

// EgressAction 由当前生成服务执行出口限流判定的管理动作
func (manager *runtimeManager) EgressAction(ctx context.Context, id string, action string) (api.AdminStatus, error) {
	admin, ok := manager.generation().admin.(egressActionAdmin)
	if !ok {
		return api.AdminStatus{}, egressNotFoundError{id: id}
	}
	return admin.EgressAction(ctx, id, action)
}
