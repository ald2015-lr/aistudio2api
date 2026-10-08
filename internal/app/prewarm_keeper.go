package app

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

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
		if time.Since(lastRotation) >= rotationInterval {
			lastRotation = time.Now()
			service.rotateCooledWorkers(dataContext)
		}
		workers := service.workers
		workers.sweepFailedWorkers(dataContext)
		workers.retryPendingCleanup()
		if len(workers.WarmAccountIDs())+len(workers.OpeningAccountIDs()) >= workers.PrewarmTarget() {
			continue
		}
		workers.StartPrewarm(dataContext)
	}
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
	// max(下限, 预热数×比例) 时才进行轮换，避免为少用的模型反复重启浏览器
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

// preferUncooled 把候选账户中没有在热门模型上长时间冷却的排在前面，预热时优先使用。
// 全部都在冷却时仍然保留，它们还能服务其他模型
func (manager *accountWorkerManager) preferUncooled(accountIDs []string) []string {
	hot := manager.hotModelList()
	if len(hot) == 0 || len(accountIDs) == 0 {
		return accountIDs
	}
	cooling := manager.pool.LongCoolingSet(accountIDs, hot, rotationMinRemaining, false)
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

// rotateCooledWorkers 把预热池中被长时间冷却占住的空闲 Worker（如当天某模型额度已用完）
// 换成没有冷却的账户。原先只有请求到来、找不到可用的预热 Worker 时才现场启动或替换浏览器，
// 这几秒到十几秒都算在请求的等待时间里；这里在后台提前完成
func (service *trackedService) rotateCooledWorkers(ctx context.Context) {
	workers := service.workers
	warm := workers.WarmAccountIDs()
	if len(warm) == 0 {
		workers.setHotModels(nil)
		return
	}
	threshold := max(rotationMinAccounts, int(float64(len(warm))*rotationMinShare))
	counts := service.pool.LongCooldownModels(warm, rotationMinRemaining)
	hot := make([]string, 0)
	for modelID, count := range counts {
		if count >= threshold {
			hot = append(hot, modelID)
		}
	}
	sort.Strings(hot)
	workers.setHotModels(hot)
	if len(hot) == 0 {
		return
	}
	occupied := make(map[string]struct{}, len(warm))
	for _, accountID := range warm {
		occupied[accountID] = struct{}{}
	}
	for _, accountID := range workers.OpeningAccountIDs() {
		occupied[accountID] = struct{}{}
	}
	limit := min(workers.warmConcurrencyValue(), service.pool.SpareAccounts(occupied, hot, rotationMinRemaining))
	if limit <= 0 {
		return
	}
	victims := service.pool.LongCoolingSet(warm, hot, rotationMinRemaining, true)
	rotated := 0
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
		evicted, err := workers.evictIdleWorker(ctx, accountID)
		workers.releaseVictim(accountID)
		if err != nil {
			service.requests.log(accountID, "WARN", fmt.Sprintf("WAA Worker 冷却轮换失败 | 错误=%v", err))
			continue
		}
		if evicted {
			rotated++
		}
	}
	if rotated == 0 {
		return
	}
	details := make([]string, 0, len(hot))
	for _, modelID := range hot {
		details = append(details, fmt.Sprintf("%s %d/%d", modelID, counts[modelID], len(warm)))
	}
	service.requests.log("service", "INFO", fmt.Sprintf(
		"WAA Worker 冷却轮换 | 替换=%d | 预热池中长时间冷却=%s", rotated, strings.Join(details, "，"),
	))
	workers.StartPrewarm(ctx)
}

// retryPendingCleanup 重试清理失败的账户：关闭失败的浏览器或释放失败的运行时租约会让账户停在“待清理”状态，
// 之后每次 ensureWorker 都失败，替换与回收也跳过它，原先要等停止再启动服务才会恢复。
// 按账户退避重试（30 秒起翻倍，最长 10 分钟）；账户正被使用时跳过，下一轮再试
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
		// 只重试明确记录下来的清理失败；正在正常关闭（WorkerClosing）的 Worker 不在这里重复关闭
		failedCleanup := account.cleanupWorker != nil || account.cleanupLease != nil || account.worker == nil && account.runtimeLease != nil
		if failedCleanup && !now.Before(account.cleanupRetryAt) {
			if err := manager.closeAccountWorker(account); err != nil {
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
