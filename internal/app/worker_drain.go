package app

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/config"
)

// 排空与替补（死槽位先启后停）。
//
// 饱和时死槽位（账号在主用或热门模型上长时间冷却，还能服务其他模型）一直有其他模型的请求在用，永远等不到空闲；
// 原先的淘汰路径都只关闭空闲 Worker，它们就一直占着名额。retirePass 遇到忙碌的登记（全局冷却，或在主用、热门模型上冷却；
// 只在其他模型上冷却的 Worker 还能服务主用模型，不排空，空闲后照常淘汰）时把账号标记排空（aistudio.AccountPool.SetDraining）：
// 不固定账户的请求不再取得它的新租约，进行中的请求照常结束，最后一个租约释放后由原有的唤醒 → retirePass → finishRetirement 关闭；
// 指定账户与文件绑定的请求照常使用。每个分区同时排空的数量不超过 drainLimit，最老的登记先排空，并且要求分区内有按登记模型
// 统计的可接替账户（见 SpareAccounts），不会为换上另一个同样的死槽位而排空。
//
// 先启后停：分区的容量判断把排空中的 Worker 视为已让出槽位（最多 drainLimit 个，见 drainCredit），排空开始时立即在后台启动替补
// （startReplacement），不等旧 Worker 关闭；分区的浏览器总数因此最多为 峰值 + drainLimit。
// 排空在每条退出路径上清除：Worker 关闭（含 ResetAll 与 Close，见 closeCleanupLeftovers）、登记被放弃（冷却被清除或缩短）、
// 淘汰出错、排空超时（REQUEST_TIMEOUT + 30 秒后仍有请求，多半是固定账户或文件绑定的请求一直在用：清除标记，1 分钟后再试）。
//
// 替补：淘汰（排空开始、空闲淘汰、清扫、轮换）之后在同一分区挑一个能服务登记模型、对热门与主用模型都没有长时间冷却的
// 备用账号，经后台冷启动名额启动。WARM_WORKER_LIMIT 低于驻留数时预热不会补齐空出的槽位，驻留数靠替补保持；分区低于预热目标时
// 由预热补齐，替补只补其余部分（在调用预热之前统计，见 replacementsFor；排空开始时除外：旧 Worker 仍驻留，预热不会为它补）。
// 替补只在分区有空槽（含排空让出的）时启动，不替换其他账号的空闲 Worker

const (
	// drainTimeoutGrace 为排空超时在 REQUEST_TIMEOUT 之外的余量
	drainTimeoutGrace = 30 * time.Second
	// drainRetryBackoff 为排空超时后同一登记再次尝试的间隔
	drainRetryBackoff = time.Minute
	// replacementScanBatch 与 replacementScanLimit 为挑替补时每批核对的备用账号数与最多核对的总数：每批只对账户池加锁一次，
	// 上万备用账号时不在锁内扫完全部
	replacementScanBatch = 64
	replacementScanLimit = 1024
	// standbyPreferLimit 为请求路径挑备用账号时按热门与主用模型冷却重新排序的候选数上限（控制锁时间）
	standbyPreferLimit = 64
)

const (
	// replacementPending、replacementStarted、replacementFailed 为排空开始时替补的结果
	replacementPending int32 = iota
	replacementStarted
	replacementFailed
)

// drainState 为一个排空中的账号
type drainState struct {
	partition aistudio.PoolScope
	// startedAt 为排空开始的时间（UnixNano）
	startedAt atomic.Int64
	// replacement 为排空开始时启动的替补的结果：失败时排空的 Worker 关闭后再补一个，成功或仍在启动时不重复补
	replacement atomic.Int32
}

// age 返回排空已持续的时间
func (drain *drainState) age(now time.Time) time.Duration {
	return now.Sub(time.Unix(0, drain.startedAt.Load()))
}

// finishReplacement 记录替补结果；drain 为 nil 时不做任何事
func (drain *drainState) finishReplacement(started bool) {
	if drain == nil {
		return
	}
	if started {
		drain.replacement.Store(replacementStarted)
	} else {
		drain.replacement.Store(replacementFailed)
	}
}

// replacementFailed 判断排空开始时的替补是否没有启动起来
func (drain *drainState) replacementFailed() bool {
	return drain != nil && drain.replacement.Load() == replacementFailed
}

// drainLimit 返回分区同时排空的上限：max(WARM_STARTUP_CONCURRENCY, 1) × 2，且不超过分区峰值的 1/4（至少 1）；峰值为 0 的分区不排空
func (manager *accountWorkerManager) drainLimit(partition aistudio.PoolScope) int {
	maxActive := manager.maxActiveFor(partition)
	if maxActive <= 0 {
		return 0
	}
	return min(max(manager.warmConcurrencyValue(), 1)*2, max(1, maxActive/4))
}

// startDrain 在分区排空数低于上限时把账号标记排空并返回排空状态；已在排空、达到上限、账号已没有驻留 Worker、
// 已被其他路径选中淘汰或已不在账户池时返回 nil
func (manager *accountWorkerManager) startDrain(accountID string, partition aistudio.PoolScope) *drainState {
	manager.drainMu.Lock()
	defer manager.drainMu.Unlock()
	if manager.drains[accountID] != nil || manager.drainCountLocked(partition) >= manager.drainLimit(partition) {
		return nil
	}
	// 在 drainMu 内核对账号仍有驻留 Worker、没有被其他路径选中淘汰：retirePass 核对驻留之后，现场回收或清扫可能已关闭 Worker 并删除登记，
	// 此时写入的排空没有任何路径再清除。closeCleanupLeftovers 先把 warm 置为假再经 drainMu 清除排空，这里看到 warm 为真时随后的关闭一定会清除这次写入
	manager.mu.RLock()
	account := manager.accounts[accountID]
	manager.mu.RUnlock()
	if account == nil || !account.warm.Load() || manager.victimReserved(accountID) {
		return nil
	}
	if !manager.pool.SetDraining(accountID, true) && !manager.pool.Draining(accountID) {
		return nil
	}
	drain := &drainState{partition: partition}
	drain.startedAt.Store(time.Now().UnixNano())
	manager.drains[accountID] = drain
	return drain
}

// clearDrain 清除账号的排空标记，返回清除前的排空状态（不在排空时为 nil）
func (manager *accountWorkerManager) clearDrain(accountID string) *drainState {
	manager.drainMu.Lock()
	defer manager.drainMu.Unlock()
	drain := manager.drains[accountID]
	if drain == nil {
		return nil
	}
	delete(manager.drains, accountID)
	manager.pool.SetDraining(accountID, false)
	return drain
}

// clearAllDrains 清除全部排空标记（ResetAll 与 Close 时）
func (manager *accountWorkerManager) clearAllDrains() {
	manager.drainMu.Lock()
	defer manager.drainMu.Unlock()
	for accountID := range manager.drains {
		delete(manager.drains, accountID)
		manager.pool.SetDraining(accountID, false)
	}
}

// drainOf 返回账号的排空状态，不在排空时为 nil
func (manager *accountWorkerManager) drainOf(accountID string) *drainState {
	manager.drainMu.Lock()
	defer manager.drainMu.Unlock()
	return manager.drains[accountID]
}

// drainCount 返回分区内排空中的账号数
func (manager *accountWorkerManager) drainCount(partition aistudio.PoolScope) int {
	manager.drainMu.Lock()
	defer manager.drainMu.Unlock()
	return manager.drainCountLocked(partition)
}

// drainCounts 按分区返回排空中的账号数（供状态接口显示），一次加锁
func (manager *accountWorkerManager) drainCounts() (normal int, ultra int) {
	manager.drainMu.Lock()
	defer manager.drainMu.Unlock()
	for _, drain := range manager.drains {
		if drain.partition == aistudio.PoolScopeUltra {
			ultra++
		} else {
			normal++
		}
	}
	return normal, ultra
}

// drainingAccounts 返回排空中的账号
func (manager *accountWorkerManager) drainingAccounts() map[string]struct{} {
	manager.drainMu.Lock()
	defer manager.drainMu.Unlock()
	accounts := make(map[string]struct{}, len(manager.drains))
	for accountID := range manager.drains {
		accounts[accountID] = struct{}{}
	}
	return accounts
}

func (manager *accountWorkerManager) drainCountLocked(partition aistudio.PoolScope) int {
	count := 0
	for _, drain := range manager.drains {
		if drain.partition == partition {
			count++
		}
	}
	return count
}

// drainCredit 返回分区容量判断中视为已让出的槽位数：分区内排空中、仍占着槽位（在 occupied 中；occupied 为 nil 时不核对）的 Worker 数，
// 最多 drainLimit 个。所有按峰值判断容量的地方（ensureWorker、standbyCapacity、状态接口）都按 占用 - drainCredit 与峰值比较
func (manager *accountWorkerManager) drainCredit(partition aistudio.PoolScope, occupied map[string]struct{}) int {
	manager.drainMu.Lock()
	count := 0
	for accountID, drain := range manager.drains {
		if drain.partition != partition {
			continue
		}
		if occupied != nil {
			if _, held := occupied[accountID]; !held {
				continue
			}
		}
		count++
	}
	manager.drainMu.Unlock()
	return min(count, manager.drainLimit(partition))
}

// drainTimeout 返回排空超时：REQUEST_TIMEOUT + 30 秒（生成服务没有设置时按默认请求超时）
func (manager *accountWorkerManager) drainTimeout() time.Duration {
	timeout := time.Duration(manager.requestTimeout.Load())
	if timeout <= 0 {
		timeout = config.Default().RequestTimeout
	}
	return timeout + drainTimeoutGrace
}

// setPrimaryModels 记录主用模型（已按降级判定的规则整理并排序）
func (manager *accountWorkerManager) setPrimaryModels(models []string) {
	manager.primaryModels.Store(&models)
}

func (manager *accountWorkerManager) primaryModelList() []string {
	if models := manager.primaryModels.Load(); models != nil {
		return *models
	}
	return nil
}

// deadSlotModels 返回判断死槽位用的模型：预热池的热门冷却模型与主用模型，去重并排序。
// 主用模型不等热门模型的 5% 阈值：重启后热门模型还没学到时同样有效
func (manager *accountWorkerManager) deadSlotModels() []string {
	hot, primary := manager.hotModelList(), manager.primaryModelList()
	if len(primary) == 0 {
		return hot
	}
	if len(hot) == 0 {
		return primary
	}
	models := append(slices.Clone(hot), primary...)
	slices.Sort(models)
	return slices.Compact(models)
}

// serviceRunning 判断生成服务是否在运行；没有设置时（测试直接使用管理器）视为运行
func (manager *accountWorkerManager) serviceRunning() bool {
	if active := manager.serviceActive.Load(); active != nil {
		return (*active)()
	}
	return true
}

type replacementStartupKey struct{}

// withReplacementStartup 标记替补启动：分区没有空槽（含排空让出的）时 ensureWorker 按容量已满返回，不替换其他账号的空闲 Worker
func withReplacementStartup(ctx context.Context) context.Context {
	return context.WithValue(ctx, replacementStartupKey{}, true)
}

func replacementStartup(ctx context.Context) bool {
	replacement, _ := ctx.Value(replacementStartupKey{}).(bool)
	return replacement
}

// evictedWorker 为一个被淘汰的 Worker：所在分区、登记的冷却模型（全局冷却或不针对模型时为空）与淘汰前读取的排空状态
// （关闭 Worker 时排空已被清除）
type evictedWorker struct {
	partition aistudio.PoolScope
	models    []string
	drain     *drainState
}

// replacementsFor 在淘汰之后、调用预热之前返回要启动替补的被淘汰 Worker：排空开始时的替补已启动或仍在启动的不再补；
// 分区驻留与启动中的 Worker 低于预热目标时缺口由随后的预热补齐，替补只补其余部分（WARM_WORKER_LIMIT 低于驻留数时预热一个都不补）。
// 必须在预热开始之前统计，否则预热刚启动的 Worker 会被算进驻留数，替补与预热重复补同一个槽位
func (manager *accountWorkerManager) replacementsFor(evicted []evictedWorker) []evictedWorker {
	pending := make(map[aistudio.PoolScope][]evictedWorker, len(workerPartitions))
	for _, worker := range evicted {
		if worker.drain != nil && !worker.drain.replacementFailed() {
			continue
		}
		pending[worker.partition] = append(pending[worker.partition], worker)
	}
	if len(pending) == 0 {
		return nil
	}
	ultra := manager.pool.UltraAccountIDs()
	resident := make(map[aistudio.PoolScope]map[string]struct{}, len(workerPartitions))
	for _, ids := range [][]string{manager.WarmAccountIDs(), manager.OpeningAccountIDs()} {
		for _, accountID := range ids {
			partition := partitionOf(accountID, ultra)
			if resident[partition] == nil {
				resident[partition] = make(map[string]struct{})
			}
			resident[partition][accountID] = struct{}{}
		}
	}
	var plan []evictedWorker
	for _, partition := range workerPartitions {
		workers := pending[partition]
		fill := max(0, manager.prewarmTargetFor(partition)-len(resident[partition]))
		if fill < len(workers) {
			plan = append(plan, workers[fill:]...)
		}
	}
	return plan
}

// startReplacements 为 replacementsFor 选出的被淘汰 Worker 各启动一个替补
func (manager *accountWorkerManager) startReplacements(plan []evictedWorker, reason string) {
	for _, worker := range plan {
		manager.startReplacement(worker.partition, worker.models, reason, nil)
	}
}

// startReplacement 在后台为分区启动一个替补 Worker（见本文件开头的说明）。models 为被淘汰的 Worker 登记的冷却模型（全局冷却为空），
// reason 写进日志；drain 不为 nil 时为排空开始时的替补，结果记在排空状态里。是否需要替补由调用方判断（排空开始时总是需要，
// 旧 Worker 仍驻留、预热不会为它补；淘汰之后见 replacementsFor）
func (manager *accountWorkerManager) startReplacement(partition aistudio.PoolScope, models []string, reason string, drain *drainState) {
	// 先取 context 再判断运行状态：停止服务时先改状态再取消后台启动，判断通过时取到的 context 一定会被随后的 ResetAll 取消
	ctx := manager.backgroundContext()
	if ctx.Err() != nil || !manager.serviceRunning() {
		drain.finishReplacement(false)
		return
	}
	models = slices.Clone(models)
	go func() {
		drain.finishReplacement(manager.runReplacement(ctx, partition, models, reason))
	}()
}

// runReplacement 挑选并启动替补，返回是否启动成功（替补账号已由其他路径在启动时也算成功）
func (manager *accountWorkerManager) runReplacement(ctx context.Context, partition aistudio.PoolScope, models []string, reason string) bool {
	warm := manager.WarmAccountIDs()
	scope := retirement{models: models}.scopeLabel()
	accountID := manager.pickReplacement(ctx, partition, models, warm)
	if accountID == "" {
		manager.requests.log("service", "INFO", fmt.Sprintf(
			"%s 替补跳过 | 原因=%s | 范围=%s | 分区内没有能服务这些模型、对热门与主用模型都没有长时间冷却的备用账号",
			workerLogName(partition), reason, scope,
		))
		return false
	}
	manager.requests.log(accountID, "INFO", fmt.Sprintf(
		"%s 替补启动 | 原因=%s | 范围=%s | %s=%d/%d",
		workerLogName(partition), reason, scope, workerSlotName(partition), len(manager.warmAccountIDsIn(partition)), manager.maxActiveFor(partition),
	))
	bootstrapModel := ""
	if len(models) > 0 {
		bootstrapModel = models[0]
	}
	taskCtx, cancel := context.WithTimeout(
		withReplacementStartup(withBackgroundStartup(ctx)), time.Duration(manager.initTimeout.Load())+warmTaskGrace,
	)
	defer cancel()
	_, err := manager.ensureWorker(taskCtx, accountID, bootstrapModel, false)
	switch {
	case err == nil:
		manager.clearWarmFailure(accountID)
		return true
	case errors.Is(err, errAccountWorkerOpening):
		return true
	case ctx.Err() != nil:
		return false
	case errors.Is(err, errAccountWorkerCapacity), errors.Is(err, errAccountRuntimeBusy):
		manager.requests.log(accountID, "INFO", fmt.Sprintf(
			"%s 替补未启动 | 原因=%s | %s", workerLogName(partition), reason, strings.TrimSpace(err.Error()),
		))
		return false
	default:
		manager.noteWarmFailure(accountID)
		manager.requests.log(accountID, "WARN", fmt.Sprintf("%s 替补启动失败 | 原因=%s | 错误=%v", workerLogName(partition), reason, err))
		return false
	}
}

// pickReplacement 在分区内挑一个替补账号：能服务 models 的空闲备用账号（按调度索引分类，见 replacementCandidates），
// 去掉预热失败冷却中、正在启动、被其他进程占用与已有 Worker 的账号，并且对热门与主用模型（以及 models）都没有长时间冷却；
// 按批核对，最多 replacementScanLimit 个，没有合适的返回空
func (manager *accountWorkerManager) pickReplacement(ctx context.Context, partition aistudio.PoolScope, models []string, warm []string) string {
	standby := manager.replacementCandidates(ctx, partition, models, warm)
	if len(standby) == 0 {
		return ""
	}
	avoid := manager.deadSlotModels()
	if len(models) > 0 {
		avoid = append(slices.Clone(avoid), models...)
		slices.Sort(avoid)
		avoid = slices.Compact(avoid)
	}
	failures := manager.recentWarmFailures(time.Now())
	for start := 0; start < len(standby) && start < replacementScanLimit; start += replacementScanBatch {
		batch := excludeAccountIDs(standby[start:min(start+replacementScanBatch, len(standby))], failures)
		batch, _ = manager.withoutOpening(batch)
		batch, _ = manager.runtimeAvailable(batch)
		batch = manager.coldAccounts(batch)
		if len(batch) == 0 {
			continue
		}
		cooling := manager.pool.LongCoolingSet(batch, avoid, rotationMinRemaining, false)
		for _, accountID := range batch {
			if _, cooled := cooling[accountID]; !cooled {
				return accountID
			}
		}
	}
	return ""
}

// replacementCandidates 返回分区内能服务 models 的空闲备用账号（多个模型时取交集）。全局冷却的登记没有模型，改按主用模型分类，
// 替补要能服务主用模型；没有主用模型（或主用模型在分区内没有候选）时按预热的候选（全部初始化模型）。
// 登记的模型分类失败（目录变化后没有账号支持）时不挑替补
func (manager *accountWorkerManager) replacementCandidates(
	ctx context.Context, partition aistudio.PoolScope, models []string, warm []string,
) []string {
	targets := models
	if len(targets) == 0 {
		targets = manager.primaryModelList()
	}
	var standby []string
	matched := false
	for _, modelID := range targets {
		groups, err := manager.pool.ClassifyCandidatesCached(aistudio.AccountSelection{
			ModelID: modelID, Method: "generateContent", Pool: partition,
		}, warm)
		if err != nil {
			if len(models) > 0 {
				return nil
			}
			continue
		}
		if !matched {
			standby, matched = groups.StandbyReady, true
			continue
		}
		standby = intersectAccountIDs(standby, groups.StandbyReady)
	}
	if matched {
		return standby
	}
	groups, err := manager.classifyBootstrapCandidates(ctx, warm, partition)
	if err != nil {
		return nil
	}
	return groups.StandbyReady
}

// intersectAccountIDs 返回同时出现在两个列表中的账户，保持 left 的顺序
func intersectAccountIDs(left []string, right []string) []string {
	keep := make(map[string]struct{}, len(right))
	for _, accountID := range right {
		keep[accountID] = struct{}{}
	}
	result := make([]string, 0, min(len(left), len(right)))
	for _, accountID := range left {
		if _, ok := keep[accountID]; ok {
			result = append(result, accountID)
		}
	}
	return result
}
