package app

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/api"
)

const (
	// keepWarmInterval 为检查常驻 Worker 是否达标的周期
	keepWarmInterval = 5 * time.Second
	// warmFailureBackoff 为预热失败的账户在此期间内不再被自动预热
	warmFailureBackoff = 5 * time.Minute
	// failedWorkerSweepWait 为后台关闭失败 Worker 时等待该账户进行中请求结束的上限；超时则下一轮再试
	failedWorkerSweepWait = 3 * time.Second
	// cleanupRetryFirst、cleanupRetryMax 为清理失败（浏览器关不掉、运行时租约释放失败）的账户后台重试间隔
	cleanupRetryFirst = 30 * time.Second
	cleanupRetryMax   = 10 * time.Minute
)

// keepWarm 服务运行期间持续把常驻 Worker 补齐到目标数：
// 预热一轮暂时找不到可启动账户（模型目录仍在同步、账户刚启用或刚导入）时，
// 过几秒自动再补，不依赖其他事件触发；已在预热时 StartPrewarm 会直接返回
func (service *trackedService) keepWarm(dataContext context.Context) {
	ticker := time.NewTicker(keepWarmInterval)
	defer ticker.Stop()
	lastRotation := time.Now()
	for {
		select {
		case <-dataContext.Done():
			return
		case <-ticker.C:
		}
		service.lifecycleMu.Lock()
		running := service.state.Load() == serviceRunning && service.dataContext == dataContext
		service.lifecycleMu.Unlock()
		if !running {
			return
		}
		lastRotation = service.keepWarmPass(dataContext, lastRotation)
	}
}

// keepWarmPass 为 keepWarm 的一个周期，返回最近一次冷却轮换的时间
func (service *trackedService) keepWarmPass(dataContext context.Context, lastRotation time.Time) time.Time {
	// 驻留 Worker 中的死槽位每个周期补登记淘汰（忙碌的由淘汰循环排空），全局长时间冷却的空闲 Worker 每个周期清扫，
	// 不等 30 秒一轮的按模型轮换
	service.registerDeadWorkers()
	service.sweepDeadWorkers(dataContext)
	if time.Since(lastRotation) >= rotationInterval {
		lastRotation = time.Now()
		service.rotateCooledWorkers(dataContext)
	}
	workers := service.workers
	workers.sweepFailedWorkers(dataContext)
	workers.retryPendingCleanup()
	// 主用模型供给约每 30 秒统计一次（主用模型列表变化时立即重新统计），状态接口只读缓存
	workers.refreshPrimarySupply(time.Now())
	// 普通与 Ultra 分区各自按自己的预热目标判断
	if workers.prewarmNeeded() {
		workers.StartPrewarm(dataContext)
	}
	return lastRotation
}

// noteWarmFailure 记录一次预热失败
func (manager *accountWorkerManager) noteWarmFailure(accountID string) {
	manager.warmFailMu.Lock()
	if manager.warmFailures == nil {
		manager.warmFailures = make(map[string]time.Time)
	}
	manager.warmFailures[accountID] = time.Now()
	manager.warmFailMu.Unlock()
}

// clearWarmFailure 账户预热成功后清除失败记录
func (manager *accountWorkerManager) clearWarmFailure(accountID string) {
	manager.warmFailMu.Lock()
	delete(manager.warmFailures, accountID)
	manager.warmFailMu.Unlock()
}

// recentWarmFailures 返回冷却期内预热失败过的账户，并顺带清理过期记录
func (manager *accountWorkerManager) recentWarmFailures(now time.Time) map[string]struct{} {
	manager.warmFailMu.Lock()
	defer manager.warmFailMu.Unlock()
	recent := make(map[string]struct{}, len(manager.warmFailures))
	for accountID, failedAt := range manager.warmFailures {
		if now.Sub(failedAt) < warmFailureBackoff {
			recent[accountID] = struct{}{}
		} else {
			delete(manager.warmFailures, accountID)
		}
	}
	return recent
}

// warmIdleLogInterval 为同一预热等待原因重复记录的最短间隔
const warmIdleLogInterval = 5 * time.Minute

// noteWarmIdle 记录预热无法继续的原因；原因不变时每 5 分钟最多记录一次
func (manager *accountWorkerManager) noteWarmIdle(level string, reason string, message string) {
	manager.warmFailMu.Lock()
	now := time.Now()
	if reason == manager.warmIdleReason && now.Sub(manager.warmIdleAt) < warmIdleLogInterval {
		manager.warmFailMu.Unlock()
		return
	}
	manager.warmIdleReason = reason
	manager.warmIdleAt = now
	manager.warmFailMu.Unlock()
	manager.requests.log("service", level, message)
}

// prewarmState 返回预热循环的实时状态：是否在跑、进行中的任务数、本轮已启动数、
// 本轮时长、循环距上次推进的秒数（持续变大说明循环被卡住），以及最近一次暂停原因
func (manager *accountWorkerManager) prewarmState() api.AdminPrewarmState {
	state := api.AdminPrewarmState{Active: manager.fillActive.Load()}
	manager.warmFailMu.Lock()
	state.Reason = manager.warmIdleReason
	manager.warmFailMu.Unlock()
	if !state.Active {
		return state
	}
	now := time.Now()
	state.Inflight = int(manager.fillInflight.Load())
	state.Launched = int(manager.fillLaunched.Load())
	if startedAt := manager.fillStartedAt.Load(); startedAt > 0 {
		state.RoundSeconds = int64(now.Sub(time.Unix(0, startedAt)) / time.Second)
	}
	if loopAt := manager.fillLoopAt.Load(); loopAt > 0 {
		state.LoopAgeSeconds = int64(now.Sub(time.Unix(0, loopAt)) / time.Second)
	}
	return state
}

const (
	// rotationInterval 为检查预热池冷却轮换的周期
	rotationInterval = 30 * time.Second
	// rotationMinRemaining 冷却剩余不少于该时长才视为长时间不可用（排除一分钟内恢复的分钟限额）
	rotationMinRemaining = 15 * time.Minute
	// rotationMinShare 与 rotationMinAccounts：一个模型在预热池中长时间冷却的账户达到
	// max(下限, 预热数×比例) 时才进行轮换，避免为少用的模型反复重启浏览器。
	// 只约束按模型的冷却：全局冷却的账号对任何模型都是死槽位，由 sweepDeadWorkers 与冷却登记不设阈值地淘汰
	rotationMinShare    = 0.05
	rotationMinAccounts = 3
	// rotationWarmupGrace 刚就绪不久的 Worker 不参与轮换
	rotationWarmupGrace = 2 * time.Minute
)

// setHotModels 记录预热池中被大量长时间冷却的模型
func (manager *accountWorkerManager) setHotModels(models []string) {
	manager.hotModels.Store(&models)
}

func (manager *accountWorkerManager) hotModelList() []string {
	if models := manager.hotModels.Load(); models != nil {
		return *models
	}
	return nil
}

// preferUncooled 把候选账户中没有在热门模型与主用模型上长时间冷却的排在前面，预热时优先使用（主用模型在重启后热门模型
// 还没学到时同样有效）。全部都在冷却时仍然保留，它们还能服务其他模型
func (manager *accountWorkerManager) preferUncooled(accountIDs []string) []string {
	models := manager.deadSlotModels()
	if len(models) == 0 || len(accountIDs) == 0 {
		return accountIDs
	}
	cooling := manager.pool.LongCoolingSet(accountIDs, models, rotationMinRemaining, false)
	if len(cooling) == 0 {
		return accountIDs
	}
	result := make([]string, 0, len(accountIDs))
	for _, accountID := range accountIDs {
		if _, cooled := cooling[accountID]; !cooled {
			result = append(result, accountID)
		}
	}
	for _, accountID := range accountIDs {
		if _, cooled := cooling[accountID]; cooled {
			result = append(result, accountID)
		}
	}
	return result
}

// sweepDeadWorkers 淘汰预热池中全局长时间冷却（每日限额冻结整个账号）的空闲 Worker。这样的 Worker 对任何模型都是死槽位，
// 不受 hotModelsIn 按模型的 5% 阈值限制：本进程写入的冷却在 applyQuotaCooldown 时就已登记淘汰（见 worker_retire.go），
// 这里兜底处理其他进程写入的冷却与登记后仍在重试的 Worker。每个 keepWarm 周期两个分区合计最多关闭 WARM_STARTUP_CONCURRENCY 个，
// 关闭并发进行；接替的 Worker 由预热经冷启动名额补齐，不会超过启动并发。只在分区内有可接替的账户时淘汰
func (service *trackedService) sweepDeadWorkers(ctx context.Context) int {
	workers := service.workers
	allWarm := workers.WarmAccountIDs()
	dead := service.pool.LongCoolingSet(allWarm, nil, rotationMinRemaining, true)
	if len(dead) == 0 || ctx.Err() != nil {
		return 0
	}
	budget := max(workers.warmConcurrencyValue(), 1)
	ultra := service.pool.UltraAccountIDs()
	opening := workers.OpeningAccountIDs()
	type victim struct {
		accountID string
		partition aistudio.PoolScope
	}
	var victims []victim
	for _, partition := range workerPartitions {
		if len(victims) >= budget {
			break
		}
		warm := inPartition(allWarm, ultra, partition)
		candidates := make([]string, 0, len(warm))
		for _, accountID := range warm {
			if _, cooled := dead[accountID]; !cooled {
				continue
			}
			if _, starting := workers.openingSet.Load(accountID); starting {
				continue
			}
			workers.mu.RLock()
			account := workers.accounts[accountID]
			workers.mu.RUnlock()
			if account == nil || !account.startupMu.TryLock() {
				continue
			}
			account.startupMu.Unlock()
			candidates = append(candidates, accountID)
		}
		if len(candidates) == 0 {
			continue
		}
		occupied := make(map[string]struct{}, len(warm)+len(opening))
		for _, accountID := range warm {
			occupied[accountID] = struct{}{}
		}
		for _, accountID := range inPartition(opening, ultra, partition) {
			occupied[accountID] = struct{}{}
		}
		limit := min(budget-len(victims), len(candidates))
		limit = min(limit, service.pool.SpareAccounts(partition, occupied, nil, rotationMinRemaining, limit))
		for _, accountID := range candidates {
			if limit <= 0 {
				break
			}
			if !workers.tryReserveVictim(accountID) {
				continue
			}
			victims = append(victims, victim{accountID: accountID, partition: partition})
			limit--
		}
	}
	if len(victims) == 0 {
		return 0
	}
	var closes sync.WaitGroup
	var retired atomic.Int32
	closed := make([]bool, len(victims))
	drains := make([]*drainState, len(victims))
	for index, target := range victims {
		closes.Add(1)
		drains[index] = workers.drainOf(target.accountID)
		go func(index int, target victim) {
			defer closes.Done()
			evicted, err := workers.evictIdleWorker(ctx, target.accountID)
			workers.releaseVictim(target.accountID)
			if err != nil {
				if ctx.Err() == nil {
					service.requests.log(target.accountID, "WARN", fmt.Sprintf("WAA Worker 冷却清扫失败 | 错误=%v", err))
				}
				return
			}
			if evicted {
				workers.dropRetirement(target.accountID)
				closed[index] = true
				retired.Add(1)
			}
		}(index, target)
	}
	closes.Wait()
	count := int(retired.Load())
	if count == 0 {
		return 0
	}
	service.requests.log("service", "INFO", fmt.Sprintf(
		"WAA Worker 冷却清扫 | 关闭全局冷却的空闲 Worker=%d | 预热池中全局冷却=%d/%d", count, len(dead), len(allWarm),
	))
	// 低于预热目标的部分由预热补齐，其余（WARM_WORKER_LIMIT 低于驻留数时预热不补）由替补保持驻留数；在预热开始前统计
	evicted := make([]evictedWorker, 0, count)
	for index, target := range victims {
		if closed[index] {
			evicted = append(evicted, evictedWorker{partition: target.partition, drain: drains[index]})
		}
	}
	plan := workers.replacementsFor(evicted)
	workers.StartPrewarm(ctx)
	workers.startReplacements(plan, "冷却清扫")
	return count
}

// rotateCooledWorkers 把预热池中被长时间冷却占住的空闲 Worker（如当天某模型额度已用完）
// 换成没有冷却的账户。原先只有请求到来、找不到可用的预热 Worker 时才现场启动或替换浏览器，
// 这几秒到十几秒都算在请求的等待时间里；这里在后台提前完成。
// 普通与 Ultra 分区分别统计与轮换：只淘汰本分区的 Worker，接替的账户也按本分区统计，随后由预热按本分区补齐；
// 一轮最多替换的数量两个分区共用启动预热并发。按模型的轮换有阈值；全局冷却的账号见 sweepDeadWorkers
func (service *trackedService) rotateCooledWorkers(ctx context.Context) {
	workers := service.workers
	ultra := service.pool.UltraAccountIDs()
	allWarm := workers.WarmAccountIDs()
	opening := workers.OpeningAccountIDs()
	var allHot []string
	var details []string
	// evicted 为本轮被淘汰的 Worker，随后按 replacementsFor 启动替补
	var evicted []evictedWorker
	rotated := 0
	budget := workers.warmConcurrencyValue()
	for _, partition := range workerPartitions {
		warm := inPartition(allWarm, ultra, partition)
		if len(warm) == 0 {
			continue
		}
		hot, counts := service.hotModelsIn(warm)
		allHot = append(allHot, hot...)
		if len(hot) == 0 || budget-rotated <= 0 || ctx.Err() != nil {
			continue
		}
		count, drains := service.rotatePartition(ctx, partition, warm, opening, hot, budget-rotated)
		if count == 0 {
			continue
		}
		for _, drain := range drains {
			evicted = append(evicted, evictedWorker{partition: partition, models: hot, drain: drain})
		}
		rotated += count
		prefix := ""
		if partition == aistudio.PoolScopeUltra {
			prefix = "Ultra "
		}
		for _, modelID := range hot {
			details = append(details, fmt.Sprintf("%s%s %d/%d", prefix, modelID, counts[modelID], len(warm)))
		}
	}
	sort.Strings(allHot)
	workers.setHotModels(slices.Compact(allHot))
	if rotated == 0 {
		return
	}
	service.requests.log("service", "INFO", fmt.Sprintf(
		"WAA Worker 冷却轮换 | 替换=%d | 预热池中长时间冷却=%s", rotated, strings.Join(details, "，"),
	))
	// 低于预热目标的部分由预热补齐，其余（WARM_WORKER_LIMIT 低于驻留数时预热不补）由替补保持驻留数；在预热开始前统计
	plan := workers.replacementsFor(evicted)
	workers.StartPrewarm(ctx)
	workers.startReplacements(plan, "冷却轮换")
}

// hotModelsIn 返回分区预热池中被大量 Worker 长时间冷却的模型与各模型的冷却数
func (service *trackedService) hotModelsIn(warm []string) ([]string, map[string]int) {
	threshold := max(rotationMinAccounts, int(float64(len(warm))*rotationMinShare))
	counts := service.pool.LongCooldownModels(warm, rotationMinRemaining)
	hot := make([]string, 0)
	for modelID, count := range counts {
		if count >= threshold {
			hot = append(hot, modelID)
		}
	}
	sort.Strings(hot)
	return hot, counts
}

// rotatePartition 在分区内淘汰最多 limit 个在热门模型上长时间冷却的空闲 Worker，返回淘汰数与被淘汰账号淘汰前的排空状态
// （每个被淘汰的 Worker 一项，不在排空时为 nil，供随后启动替补）
func (service *trackedService) rotatePartition(
	ctx context.Context, partition aistudio.PoolScope, warm []string, openingIDs []string, hot []string, limit int,
) (int, []*drainState) {
	workers := service.workers
	occupied := make(map[string]struct{}, len(warm)+len(openingIDs))
	for _, accountID := range warm {
		occupied[accountID] = struct{}{}
	}
	for _, accountID := range openingIDs {
		occupied[accountID] = struct{}{}
	}
	limit = min(limit, service.pool.SpareAccounts(partition, occupied, hot, rotationMinRemaining, limit))
	if limit <= 0 {
		return 0, nil
	}
	victims := service.pool.LongCoolingSet(warm, hot, rotationMinRemaining, true)
	rotated := 0
	var drains []*drainState
	now := time.Now()
	for _, accountID := range warm {
		if rotated >= limit || ctx.Err() != nil {
			break
		}
		if _, victim := victims[accountID]; !victim {
			continue
		}
		if _, opening := workers.openingSet.Load(accountID); opening {
			continue
		}
		workers.mu.RLock()
		account := workers.accounts[accountID]
		workers.mu.RUnlock()
		if account == nil || now.Sub(time.Unix(0, account.readyAt.Load())) < rotationWarmupGrace {
			continue
		}
		// 与 ensureWorker 的替换共用淘汰标记：已被选中的不再重复淘汰，淘汰期间替换也不会选中它
		if !workers.tryReserveVictim(accountID) {
			continue
		}
		drain := workers.drainOf(accountID)
		evicted, err := workers.evictIdleWorker(ctx, accountID)
		workers.releaseVictim(accountID)
		if err != nil {
			service.requests.log(accountID, "WARN", fmt.Sprintf("WAA Worker 冷却轮换失败 | 错误=%v", err))
			continue
		}
		if evicted {
			rotated++
			drains = append(drains, drain)
		}
	}
	return rotated, drains
}

// retryPendingCleanup 重试清理失败的账户：关闭失败的浏览器或释放失败的运行时租约会让账户停在“待清理”状态，
// 之后每次 ensureWorker 都失败，替换与回收也跳过它，原先要等停止再启动服务才会恢复。
// 按账户退避重试（30 秒起翻倍，最长 10 分钟）；账户正在启动或持有锁时跳过，下一轮再试
func (manager *accountWorkerManager) retryPendingCleanup() {
	manager.mu.RLock()
	accounts := make([]*accountWorker, 0, len(manager.accounts))
	for _, account := range manager.accounts {
		accounts = append(accounts, account)
	}
	manager.mu.RUnlock()
	now := time.Now()
	for _, account := range accounts {
		if !account.startupMu.TryLock() {
			continue
		}
		if !account.mu.TryLock() {
			account.startupMu.Unlock()
			continue
		}
		// 主 Worker 只在持有 account.mu 时关闭，这里看到 WorkerClosing 只能是之前的 Close 失败（浏览器关不掉），需要重关；
		// 否则只清理残留的替换 Worker 与租约，健康的主 Worker 可能正被请求使用，不能一并关闭
		stuckMain := account.worker != nil && account.worker.State().Phase == aistudio.WorkerClosing
		leftovers := account.cleanupWorker != nil || account.cleanupLease != nil || account.worker == nil && account.runtimeLease != nil
		if (stuckMain || leftovers) && !now.Before(account.cleanupRetryAt) {
			retry := manager.closeCleanupLeftovers
			if stuckMain {
				retry = manager.closeAccountWorker
			}
			if err := retry(account); err != nil {
				account.cleanupBackoff = min(max(account.cleanupBackoff*2, cleanupRetryFirst), cleanupRetryMax)
				account.cleanupRetryAt = now.Add(account.cleanupBackoff)
				manager.requests.log(account.label, "WARN", fmt.Sprintf(
					"WAA Worker 清理重试失败 | %s 后再试 | 错误=%s", account.cleanupBackoff, strings.TrimSpace(err.Error()),
				))
			} else {
				account.cleanupBackoff, account.cleanupRetryAt = 0, time.Time{}
				manager.requests.log(account.label, "INFO", "WAA Worker 清理重试成功，账户恢复可用")
			}
		}
		account.mu.Unlock()
		account.startupMu.Unlock()
	}
}

// sweepFailedWorkers 关闭已失败的常驻 Worker（浏览器退出、控制连接断开等），释放常驻名额，随后由 keepWarm 补齐。
// 原先失败的 Worker 只看 warm 标志仍计入常驻数，也没有巡检，可用容量会悄悄缩水，只能等请求到来时现场重建
func (manager *accountWorkerManager) sweepFailedWorkers(ctx context.Context) {
	for _, accountID := range manager.WarmAccountIDs() {
		if ctx.Err() != nil {
			return
		}
		if !manager.WorkerFailed(accountID) {
			continue
		}
		generation := manager.WorkerGeneration(accountID)
		sweepCtx, cancel := context.WithTimeout(ctx, failedWorkerSweepWait)
		reset, err := manager.ResetIfGeneration(sweepCtx, accountID, generation)
		cancel()
		switch {
		case reset:
			manager.requests.log(accountID, "WARN", "常驻 Worker 已失败，后台关闭以便重新预热")
		case err != nil && ctx.Err() == nil && !errors.Is(err, context.DeadlineExceeded):
			manager.requests.log(accountID, "WARN", "关闭失败的常驻 Worker 出错 | "+strings.TrimSpace(err.Error()))
		}
	}
}
