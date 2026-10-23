package app

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// 死槽位：驻留 Worker 的账号进入了长时间冷却（如每日限额把整个账号冻结到次日，或请求模型的所有启用通道都冷却），
// 这个 Worker 占着常驻名额却服务不了当前的请求。原先只有每 30 秒一轮的冷却轮换会替换它们，而且要等某个模型的
// 冷却账号达到预热池的 5%、一轮最多替换启动并发个；账号额度耗尽的速度超过这个节奏时，热池被死槽位占满，
// 新请求只能排队等替换。这里分三层处理：
//   - 账号刚被 applyQuotaCooldown 冷却时登记淘汰（retireWorker）：Worker 空闲就立刻关闭，忙碌则等它的请求释放租约后关闭；
//   - keepWarm 每个周期清扫全局长时间冷却的空闲 Worker（sweepDeadWorkers），不受 5% 阈值限制；
//   - 请求选号时分区已满而有死槽位（reclaimDeadWorker）：现场回收一个再启动备用账号，不等轮换。
// 三条路径与 ensureWorker 的替换共用 tryReserveVictim，同一个 Worker 不会被两边同时淘汰；淘汰经 evictIdleWorker 取账号的独占租约，
// 正在服务请求的 Worker 永远不会被关闭。只有分区内有可接替的账户时才淘汰：小规模部署全部账号都冷却时保留 Worker，
// 冷却到期后能立即服务。登记了却一直忙碌的 Worker（饱和时其他模型的请求一直在用它）先排空再淘汰，替补在排空开始时就启动，
// 见 worker_drain.go；keepWarm 每个周期把驻留 Worker 中的死槽位补登记（registerDeadWorkers），重启后与其他进程写入的冷却同样会被淘汰。
// 不可调度的驻留 Worker（账号需要登录、已停用、没有模型目录等，见 worker_unschedulable.go）共用这张登记表与淘汰循环，按释放处理：
// 空闲即关，忙碌等请求结束，不排空、不要求可接替账户、不走排空超时，关闭后只补齐预热

const (
	// retireDebounce 为淘汰循环两轮之间的最短间隔：租约释放很频繁，忙碌中的登记不必每次都重试
	retireDebounce = 250 * time.Millisecond
	// retireRetryInterval 为仍有登记未处理（账号忙碌、淘汰未成功）时下一轮的兜底间隔
	retireRetryInterval = 2 * time.Second
	// retireFailureBackoff 为淘汰出错（浏览器关不掉等）后同一登记再次尝试的间隔
	retireFailureBackoff = 5 * time.Second
	// retireNoSpareBackoff 为分区内没有可接替账户时同一登记再次尝试的间隔
	retireNoSpareBackoff = 30 * time.Second
)

// retirement 为一条待淘汰登记
type retirement struct {
	// models 为导致登记的冷却模型（每个模型的所有启用通道都冷却），按 ID 排序；全局冷却为空。
	// 同一账号可能先后在几个模型上成为死槽位，其中一个解除冷却时登记仍要为其余模型保留
	models []string
	since  time.Time
	// retryAt 为上次淘汰未成功后下一次尝试的时间，零值为随时可试
	retryAt time.Time
	// unschedulable 为不可调度登记的原因（需要登录、已停用、没有模型目录等，见 worker_unschedulable.go），空为冷却登记。
	// 不可调度登记按释放处理（见 retirePass）；账号恢复可调度后清空，原先另有冷却登记的照常按冷却核对，只有不可调度登记的随后放弃
	unschedulable string
}

// global 判断登记是否为全局冷却
func (entry retirement) global() bool {
	return len(entry.models) == 0
}

// drainable 判断忙碌的登记能否排空：全局冷却，或登记的模型中有一个是判断死槽位用的模型（deadSlotModelsFor，热门模型与账号所在分区的主用模型）。
// 模型按降级判定的规则比较（主用模型已整理为小写）
func (entry retirement) drainable(deadModels []string) bool {
	if entry.global() {
		return true
	}
	for _, modelID := range entry.models {
		name := normalizeGuardModel(modelID)
		for _, deadModel := range deadModels {
			if normalizeGuardModel(deadModel) == name {
				return true
			}
		}
	}
	return false
}

// snapshot 返回登记的副本，模型列表另拷一份，淘汰循环读取时不与新的登记互相干扰
func (entry retirement) snapshot() retirement {
	entry.models = slices.Clone(entry.models)
	return entry
}

// scopeLabel 返回登记范围在日志里的写法
func (entry retirement) scopeLabel() string {
	if entry.global() {
		return "全局"
	}
	return strings.Join(entry.models, ",")
}

// stillUnschedulable 按淘汰循环本轮一次加锁取得的账户状态核对不可调度：返回写上当前原因的登记，ok 为账号此刻不可调度
// （冷却登记的账号后来不可调度时同样为真）；账号已恢复可调度时返回清空原因的登记、ok 为假，调用方再按冷却核对
func (entry retirement) stillUnschedulable(state aistudio.WarmWorkerState) (retirement, bool) {
	entry.unschedulable = state.Unschedulable
	return entry, entry.unschedulable != ""
}

// stillCooling 返回登记中账号仍长时间冷却的范围：全局登记按全局冷却判断，按模型的登记只保留仍冷却的模型；
// 返回的 ok 为假表示登记已没有理由保留
func (entry retirement) stillCooling(pool *aistudio.AccountPool, accountID string) (retirement, bool) {
	if entry.global() {
		return entry, pool.AccountLongCooling(accountID, "", rotationMinRemaining)
	}
	entry.models = slices.DeleteFunc(slices.Clone(entry.models), func(modelID string) bool {
		return !pool.AccountLongCooling(accountID, modelID, rotationMinRemaining)
	})
	return entry, len(entry.models) > 0
}

// retireWorker 登记账号的驻留 Worker 待淘汰并唤醒淘汰循环；账号没有驻留 Worker 时不登记。
// modelID 为空表示全局冷却，会覆盖同一账号先前按模型的登记；再按别的模型登记时并入同一条登记
func (manager *accountWorkerManager) retireWorker(accountID string, modelID string) bool {
	registered, _ := manager.registerRetirement(accountID, modelID)
	return registered
}

// registerRetirement 为 retireWorker 的实现：registered 为账号有驻留 Worker、登记已存在或已写入，
// added 为这次新增了登记或登记的范围（新的模型、改为全局）
func (manager *accountWorkerManager) registerRetirement(accountID string, modelID string) (registered bool, added bool) {
	manager.mu.RLock()
	account := manager.accounts[accountID]
	manager.mu.RUnlock()
	if account == nil || !account.warm.Load() {
		return false, false
	}
	manager.retireMu.Lock()
	if manager.retirements == nil {
		manager.retirements = make(map[string]*retirement)
	}
	if existing := manager.retirements[accountID]; existing != nil {
		switch {
		case existing.global():
		case modelID == "":
			existing.models, added = nil, true
		case !slices.Contains(existing.models, modelID):
			existing.models = append(slices.Clone(existing.models), modelID)
			slices.Sort(existing.models)
			added = true
		}
		manager.retireMu.Unlock()
		return true, added
	}
	entry := &retirement{since: time.Now()}
	if modelID != "" {
		entry.models = []string{modelID}
	}
	manager.retirements[accountID] = entry
	manager.retirePending.Store(int32(len(manager.retirements)))
	manager.retireMu.Unlock()
	manager.wakeRetire()
	return true, true
}

// registerUnschedulable 登记释放不可调度账号的驻留 Worker（reason 为原因）并唤醒淘汰循环；账号没有驻留 Worker 时不登记。
// 与冷却登记同锁、同一张表：已有冷却登记时改为不可调度登记（保留冷却的范围，恢复可调度后仍按冷却核对），并清除推迟：
// 排空超时被推迟 1 分钟、没有可接替账户被推迟 30 秒的登记都立即处理。registered、added 的含义同 registerRetirement
// （已是不可调度登记时 added 为假，只更新原因）
func (manager *accountWorkerManager) registerUnschedulable(accountID string, reason string) (registered bool, added bool) {
	manager.mu.RLock()
	account := manager.accounts[accountID]
	manager.mu.RUnlock()
	if account == nil || !account.warm.Load() {
		return false, false
	}
	manager.retireMu.Lock()
	if manager.retirements == nil {
		manager.retirements = make(map[string]*retirement)
	}
	if existing := manager.retirements[accountID]; existing != nil {
		if existing.unschedulable == "" {
			existing.retryAt, added = time.Time{}, true
		}
		existing.unschedulable = reason
		manager.retireMu.Unlock()
		if added {
			manager.wakeRetire()
		}
		return true, added
	}
	manager.retirements[accountID] = &retirement{since: time.Now(), unschedulable: reason}
	manager.retirePending.Store(int32(len(manager.retirements)))
	manager.retireMu.Unlock()
	manager.wakeRetire()
	return true, true
}

// setUnschedulableReason 写回登记的不可调度原因（retirePass 按本轮的账户状态核对后调用；空为已恢复可调度）；没有登记时不做任何事
func (manager *accountWorkerManager) setUnschedulableReason(accountID string, reason string) {
	manager.retireMu.Lock()
	if entry := manager.retirements[accountID]; entry != nil {
		entry.unschedulable = reason
	}
	manager.retireMu.Unlock()
}

// wakeRetire 唤醒淘汰循环；没有登记时不唤醒，重复唤醒合并
func (manager *accountWorkerManager) wakeRetire() {
	if manager.retirePending.Load() == 0 || manager.retireWake == nil {
		return
	}
	select {
	case manager.retireWake <- struct{}{}:
	default:
	}
}

// dropRetirement 删除一条登记，账号在排空时一并清除排空（Worker 已关闭，或冷却已清除、缩短，不再需要淘汰）
func (manager *accountWorkerManager) dropRetirement(accountID string) {
	manager.retireMu.Lock()
	delete(manager.retirements, accountID)
	manager.retirePending.Store(int32(len(manager.retirements)))
	manager.retireMu.Unlock()
	manager.clearDrain(accountID)
}

// deferRetirement 把一条登记推迟到 retryAt 之后再试
func (manager *accountWorkerManager) deferRetirement(accountID string, retryAt time.Time) {
	manager.retireMu.Lock()
	if entry := manager.retirements[accountID]; entry != nil {
		entry.retryAt = retryAt
	}
	manager.retireMu.Unlock()
}

// retirementCount 返回待淘汰登记数
func (manager *accountWorkerManager) retirementCount() int {
	return int(manager.retirePending.Load())
}

// retirementCounts 按分区返回待淘汰登记数（供状态接口显示）
func (manager *accountWorkerManager) retirementCounts(ultra map[string]struct{}) (normal int, ultraCount int) {
	manager.retireMu.Lock()
	defer manager.retireMu.Unlock()
	for accountID := range manager.retirements {
		if partitionOf(accountID, ultra) == aistudio.PoolScopeUltra {
			ultraCount++
		} else {
			normal++
		}
	}
	return normal, ultraCount
}

// retireLoop 处理待淘汰登记：由 retireWorker 与账户池变化（租约释放）唤醒，仍有登记时按兜底间隔重试
func (manager *accountWorkerManager) retireLoop() {
	retry := time.NewTimer(time.Hour)
	retry.Stop()
	defer retry.Stop()
	for {
		select {
		case <-manager.lifecycle.Done():
			return
		case <-manager.retireWake:
		case <-retry.C:
		}
		if manager.retirePass() > 0 {
			retry.Reset(retireRetryInterval)
		}
		debounce := time.NewTimer(retireDebounce)
		select {
		case <-manager.lifecycle.Done():
			debounce.Stop()
			return
		case <-debounce.C:
		}
	}
}

// retirementCandidate 为一轮淘汰中取出的登记
type retirementCandidate struct {
	accountID string
	entry     retirement
	partition aistudio.PoolScope
	// drain 为账号的排空状态，没有排空时为 nil
	drain *drainState
}

// spareKey 返回可接替账户统计的键：分区与登记的模型
func (candidate retirementCandidate) spareKey() string {
	return candidate.partition.String() + "|" + strings.Join(candidate.entry.models, ",")
}

// retirePass 处理一轮登记，返回处理后仍待淘汰的登记数。每条登记先核对账号仍有驻留 Worker、仍在长时间冷却
// （冷却被清除或缩短时放弃淘汰并清除排空），再要求分区内有能服务登记模型的可接替账户（按登记的模型统计，不会换上同一模型的
// 另一个死槽位）；账号正在启动或被其他路径选中时留到下一轮。忙碌的登记（全局冷却，或在主用、热门模型上冷却，见 drainable）
// 在分区排空数低于 drainLimit 时标记排空（最老的登记先排空）并立即启动替补，排空中的账号空闲后关闭，超过排空超时仍忙碌时
// 清除排空、1 分钟后再试；只在其他模型上冷却的忙碌登记不排空，空闲后照常淘汰。关闭并发进行，数量不超过启动并发。
// 账号不可调度（需要登录、已停用、没有模型目录等；冷却登记的账号后来不可调度的同样）时按释放处理：不核对可接替账户、不排空，
// 也不走排空超时（已在排空的账号请求结束后下一轮即关闭，不再推迟 1 分钟）；忙碌时留到租约释放后被再次唤醒，空闲时与冷却登记
// 共用每轮的关闭并发
func (manager *accountWorkerManager) retirePass() int {
	now := time.Now()
	manager.retireMu.Lock()
	candidates := make([]retirementCandidate, 0, len(manager.retirements))
	for accountID, entry := range manager.retirements {
		if now.Before(entry.retryAt) {
			continue
		}
		candidates = append(candidates, retirementCandidate{accountID: accountID, entry: entry.snapshot()})
	}
	manager.retireMu.Unlock()
	if len(candidates) == 0 {
		return manager.retirementCount()
	}
	sort.Slice(candidates, func(left int, right int) bool {
		return candidates[left].entry.since.Before(candidates[right].entry.since)
	})
	ultra := manager.pool.UltraAccountIDs()
	warm := manager.WarmAccountIDs()
	opening := manager.OpeningAccountIDs()
	// 各登记账号是否忙碌一次加锁取得（keepWarm 补登记后登记可能有上百条，逐条查询要对账户池加锁上百次）；
	// 状态稍旧没有关系：误判为空闲时淘汰取独占租约会失败，误判为忙碌时先排空，空闲后下一轮淘汰
	candidateIDs := make([]string, len(candidates))
	for index := range candidates {
		candidateIDs[index] = candidates[index].accountID
	}
	states := manager.pool.WarmWorkerStates(candidateIDs, "", rotationMinRemaining)
	// spare 为可接替的账户数，按分区与登记的模型分别统计（第一次需要时），每启动一次淘汰或排空减一；occupied 为各分区驻留与启动中的账户
	spare := make(map[string]int, len(workerPartitions))
	spareKnown := make(map[string]bool, len(workerPartitions))
	occupied := make(map[aistudio.PoolScope]map[string]struct{}, len(workerPartitions))
	pendingIn := make(map[aistudio.PoolScope]int, len(workerPartitions))
	for index := range candidates {
		candidates[index].partition = partitionOf(candidates[index].accountID, ultra)
		pendingIn[candidates[index].partition]++
	}
	occupiedIn := func(partition aistudio.PoolScope) map[string]struct{} {
		if held, ok := occupied[partition]; ok {
			return held
		}
		held := make(map[string]struct{}, len(warm)+len(opening))
		for _, id := range inPartition(warm, ultra, partition) {
			held[id] = struct{}{}
		}
		for _, id := range inPartition(opening, ultra, partition) {
			held[id] = struct{}{}
		}
		occupied[partition] = held
		return held
	}
	// 判断能否排空用账号所在分区的死槽位模型（Ultra 分区按 Ultra 主用模型），每个分区第一次需要时取一次
	deadModels := make(map[aistudio.PoolScope][]string, len(workerPartitions))
	deadModelsIn := func(partition aistudio.PoolScope) []string {
		models, ok := deadModels[partition]
		if !ok {
			models = manager.deadSlotModelsFor(partition)
			deadModels[partition] = models
		}
		return models
	}
	var victims []retirementCandidate
	for _, candidate := range candidates {
		if manager.lifecycle.Err() != nil {
			break
		}
		accountID := candidate.accountID
		manager.mu.RLock()
		account := manager.accounts[accountID]
		manager.mu.RUnlock()
		if account == nil || !account.warm.Load() {
			manager.dropRetirement(accountID)
			continue
		}
		// 不可调度按本轮一次加锁取得的状态核对（见 stillUnschedulable）：不可调度的账号拿不到不固定账户的新租约，忙碌只是还在跑
		// 旧请求或独占操作，空闲即关；不核对可接替账户、不排空、不走下面的排空超时分支。账号已恢复可调度（验证通过、重新启用、
		// 目录恢复）时撤销不可调度登记，原先另有冷却登记的照常按冷却核对，没有的随后放弃
		release, unschedulable := candidate.entry.stillUnschedulable(states[accountID])
		if release.unschedulable != candidate.entry.unschedulable {
			manager.setUnschedulableReason(accountID, release.unschedulable)
		}
		candidate.entry = release
		if unschedulable {
			if manager.workerStarting(account, accountID) || states[accountID].Busy || !manager.tryReserveVictim(accountID) {
				continue
			}
			// 选中之前其他路径（现场回收、按需替换）可能刚关掉它：持有淘汰标记后再核对一次，免得对已关闭的账号再写一条释放
			if !account.warm.Load() {
				manager.releaseVictim(accountID)
				manager.dropRetirement(accountID)
				continue
			}
			candidate.drain = manager.drainOf(accountID)
			victims = append(victims, candidate)
			if len(victims) >= max(manager.warmConcurrencyValue(), 1) {
				break
			}
			continue
		}
		// 只要登记里还有一个模型（或全局）仍长时间冷却，账号就仍是死槽位；日志只写仍在冷却的范围
		cooling, ok := candidate.entry.stillCooling(manager.pool, accountID)
		if !ok {
			manager.dropRetirement(accountID)
			continue
		}
		candidate.entry = cooling
		if manager.workerStarting(account, accountID) {
			continue
		}
		partition := candidate.partition
		busy := states[accountID].Busy
		candidate.drain = manager.drainOf(accountID)
		if candidate.drain != nil {
			// 已在排空：替补在排空开始时已经启动，不再核对可接替账户；空闲后照常淘汰
			if busy {
				if since := candidate.drain.age(now); since >= manager.drainTimeout() {
					manager.clearDrain(accountID)
					manager.deferRetirement(accountID, now.Add(drainRetryBackoff))
					manager.requests.log(accountID, "WARN", fmt.Sprintf(
						"WAA Worker 排空超时 | 范围=%s | 排空=%s | 仍有进行中的请求，恢复接受新请求，%s 后再试",
						candidate.entry.scopeLabel(), since.Round(time.Second), drainRetryBackoff,
					))
				}
				continue
			}
		} else {
			// 只有全局冷却、或在主用模型与热门模型上冷却的死槽位才排空；只在别的模型上冷却的（少用的模型、单个账号无权限的模型）
			// 还能服务主用与热门模型，忙碌时不排空，空闲后照常淘汰
			if busy && !candidate.entry.drainable(deadModelsIn(partition)) {
				continue
			}
			key := candidate.spareKey()
			if !spareKnown[key] {
				spareKnown[key] = true
				spare[key] = manager.pool.SpareAccounts(partition, occupiedIn(partition), candidate.entry.models, rotationMinRemaining, pendingIn[partition])
			}
			if spare[key] <= 0 {
				manager.deferRetirement(accountID, now.Add(retireNoSpareBackoff))
				continue
			}
			// 账号正在服务请求：不取独占租约，先排空（不固定账户的请求不再分给它），等进行中的请求释放租约后被再次唤醒
			if busy {
				// 账号在取成员快照之后换了分区（ULTRA_INCLUDE_PRO 切换）时 startDrain 同样返回 nil：上面的可接替账户与能否排空
				// 都是按旧分区判断的，不写入排空，登记没有推迟，下一轮按新的成员快照重新判断
				drain := manager.startDrain(accountID, partition)
				if drain == nil {
					continue
				}
				spare[key]--
				manager.requests.log(accountID, "INFO", fmt.Sprintf(
					"WAA Worker 排空 | 范围=%s | 登记后=%s | 排空中=%d（占名额 %d/%d） | 进行中的请求结束后关闭，替补先启动",
					candidate.entry.scopeLabel(), now.Sub(candidate.entry.since).Round(time.Millisecond),
					manager.drainCount(partition), manager.drainSlotCount(partition), manager.drainLimit(partition),
				))
				manager.startReplacement(partition, candidate.entry.models, "排空", drain)
				continue
			}
		}
		if !manager.tryReserveVictim(accountID) {
			continue
		}
		if candidate.drain == nil {
			spare[candidate.spareKey()]--
		}
		victims = append(victims, candidate)
		if len(victims) >= max(manager.warmConcurrencyValue(), 1) {
			break
		}
	}
	if len(victims) == 0 {
		return manager.retirementCount()
	}
	var closes sync.WaitGroup
	for _, victim := range victims {
		closes.Add(1)
		go func(victim retirementCandidate) {
			defer closes.Done()
			manager.finishRetirement(victim)
		}(victim)
	}
	closes.Wait()
	return manager.retirementCount()
}

// finishRetirement 关闭一个已选中的待淘汰 Worker；调用方已 tryReserveVictim。关闭后补齐预热，并按 replacementsFor 在分区内启动替补
// （排空开始时的替补已启动或仍在启动时不重复补）。不可调度登记见 finishRelease
func (manager *accountWorkerManager) finishRetirement(victim retirementCandidate) {
	if victim.entry.unschedulable != "" {
		manager.finishRelease(victim)
		return
	}
	accountID := victim.accountID
	evicted, err := manager.evictIdleWorker(manager.lifecycle, accountID)
	manager.releaseVictim(accountID)
	switch {
	case err != nil:
		manager.clearDrain(accountID)
		manager.requests.log(accountID, "WARN", fmt.Sprintf("WAA Worker 冷却淘汰失败 | 范围=%s | 错误=%v", victim.entry.scopeLabel(), err))
		manager.deferRetirement(accountID, time.Now().Add(retireFailureBackoff))
	case !evicted:
		// 取独占租约时账号又有了请求（或其他进程持有文件锁），等下一次唤醒；排空中的账号继续排空
	default:
		manager.dropRetirement(accountID)
		// 替补补在 Worker 关闭时所在的分区：victim.partition 是选中时的成员快照，ULTRA_INCLUDE_PRO 在选中之后切换时
		// 关闭的 Pro Worker 空出的是新分区的槽位（排空中的已由 poolRuleChanged 改记到新分区），按当前成员重算
		victim.partition = partitionOf(accountID, manager.pool.UltraAccountIDs())
		plan := manager.replacementsFor([]evictedWorker{{partition: victim.partition, models: victim.entry.models, drain: victim.drain}})
		drained := ""
		if victim.drain != nil {
			drained = fmt.Sprintf(" | 排空=%s", victim.drain.age(time.Now()).Round(time.Millisecond))
		}
		manager.requests.log(accountID, "INFO", fmt.Sprintf(
			"WAA Worker 冷却淘汰 | 范围=%s | 登记后=%s%s | %s=%d/%d",
			victim.entry.scopeLabel(), time.Since(victim.entry.since).Round(time.Millisecond), drained,
			workerSlotName(victim.partition), len(manager.warmAccountIDsIn(victim.partition)), manager.maxActiveFor(victim.partition),
		))
		if refill := manager.afterRetire.Load(); refill != nil {
			(*refill)()
		}
		manager.startReplacements(plan, "冷却淘汰")
	}
}

// finishRelease 关闭一个不可调度登记的 Worker（写“WAA Worker 释放”，不写“冷却淘汰”）；调用方已 tryReserveVictim。
// 关闭后只补齐预热（afterRetire），不按 replacementsFor 启替补：替补按冷却模型挑选，这里空出的槽位由预热按偏好补上；
// 出口限流时预热走补位路径，只受补位并发约束
func (manager *accountWorkerManager) finishRelease(victim retirementCandidate) {
	accountID := victim.accountID
	evicted, err := manager.evictIdleWorker(manager.lifecycle, accountID)
	manager.releaseVictim(accountID)
	switch {
	case err != nil:
		manager.clearDrain(accountID)
		manager.requests.log(accountID, "WARN", fmt.Sprintf("WAA Worker 释放失败 | 原因=%s | 错误=%v", victim.entry.unschedulable, err))
		manager.deferRetirement(accountID, time.Now().Add(retireFailureBackoff))
	case !evicted:
		// 取独占租约时账号又有了请求（指定账户的请求、独占操作），等下一次唤醒
	default:
		manager.dropRetirement(accountID)
		partition := partitionOf(accountID, manager.pool.UltraAccountIDs())
		manager.requests.log(accountID, "INFO", fmt.Sprintf(
			"WAA Worker 释放 | 原因=%s | 登记后=%s | %s=%d/%d",
			victim.entry.unschedulable, time.Since(victim.entry.since).Round(time.Millisecond),
			workerSlotName(partition), len(manager.warmAccountIDsIn(partition)), manager.maxActiveFor(partition),
		))
		if refill := manager.afterRetire.Load(); refill != nil {
			(*refill)()
		}
	}
}

// workerStarting 判断账户正在启动或替换 Worker（与 idleWarmVictimFor 相同的口径）：这时不淘汰、不释放，避免与持有 startupMu
// 等 rebalanceMu 的启动流程互相等待，留到下一轮
func (manager *accountWorkerManager) workerStarting(account *accountWorker, accountID string) bool {
	if _, starting := manager.openingSet.Load(accountID); starting {
		return true
	}
	if !account.startupMu.TryLock() {
		return true
	}
	account.startupMu.Unlock()
	return false
}

// retireCooledWorker 在 applyQuotaCooldown 写入冷却后判断该账号的驻留 Worker 是否已成死槽位并登记淘汰：
// 冷却剩余不少于 rotationMinRemaining，且为全局冷却或该模型的所有启用通道都已冷却（另一通道同步冷却后再判断）
func (service *trackedService) retireCooledWorker(accountID string, modelID string, cooldown aistudio.QuotaCooldown) {
	if service.workers == nil || time.Until(cooldown.Until) < rotationMinRemaining {
		return
	}
	scope := modelID
	if cooldown.Global {
		scope = ""
	}
	if !service.pool.AccountLongCooling(accountID, scope, rotationMinRemaining) {
		return
	}
	service.workers.retireWorker(accountID, scope)
}

// registerDeadWorkers 把驻留 Worker 中的死槽位补登记淘汰（keepWarm 每个周期调用）：账号全局长时间冷却，或在热门模型、所在分区的
// 主用模型上长时间不可用（Ultra 分区按 Ultra 主用模型，见 primaryModelListFor）。原先只在本进程 applyQuotaCooldown 写入冷却、
// 账号恰好驻留时登记，而且只在内存里：重启后、其他进程写入的冷却、冷却时账号还没驻留（随后被预热）都没有登记，这些 Worker 永远
// 不会被排空淘汰，状态页的冷却占位也与登记数对不上。每个分区先对驻留 Worker 调用一次 LongCoolingSet，只对其中的死槽位再按全局与
// 各模型分别核对范围，返回新增的登记数
func (service *trackedService) registerDeadWorkers() int {
	workers := service.workers
	if workers == nil {
		return 0
	}
	allWarm := workers.WarmAccountIDs()
	ultra := service.pool.UltraAccountIDs()
	added, deadTotal := 0, 0
	var logged []string
	for _, partition := range workerPartitions {
		warm := inPartition(allWarm, ultra, partition)
		if len(warm) == 0 {
			continue
		}
		models := workers.deadSlotModelsFor(partition)
		partitionAdded, dead := service.registerDeadWorkersIn(warm, models)
		added += partitionAdded
		deadTotal += dead
		logged = mergeModelLists(logged, models)
	}
	if added > 0 {
		service.requests.log("service", "INFO", fmt.Sprintf(
			"WAA Worker 补登记淘汰 | 新增=%d | 驻留 Worker 中的死槽位=%d/%d | 模型=%s",
			added, deadTotal, len(allWarm), strings.Join(logged, ","),
		))
	}
	return added
}

// registerDeadWorkersIn 在一个分区的驻留 Worker 中补登记死槽位（models 为该分区判断死槽位用的模型），返回新增的登记数与死槽位数
func (service *trackedService) registerDeadWorkersIn(warm []string, models []string) (int, int) {
	workers := service.workers
	dead := service.pool.LongCoolingSet(warm, models, rotationMinRemaining, false)
	if len(dead) == 0 {
		return 0, 0
	}
	accountIDs := make([]string, 0, len(dead))
	for accountID := range dead {
		accountIDs = append(accountIDs, accountID)
	}
	slices.Sort(accountIDs)
	added := 0
	global := service.pool.LongCoolingSet(accountIDs, nil, rotationMinRemaining, false)
	for accountID := range global {
		if _, ok := workers.registerRetirement(accountID, ""); ok {
			added++
		}
	}
	scoped := slices.DeleteFunc(accountIDs, func(accountID string) bool {
		_, isGlobal := global[accountID]
		return isGlobal
	})
	for _, modelID := range models {
		if len(scoped) == 0 {
			break
		}
		for accountID := range service.pool.LongCoolingSet(scoped, []string{modelID}, rotationMinRemaining, false) {
			if _, ok := workers.registerRetirement(accountID, modelID); ok {
				added++
			}
		}
	}
	return added, len(dead)
}

// victimMode 为淘汰选号的范围
type victimMode int

const (
	// victimAny 任意空闲 Worker：不可调度的优先（账号需要登录、已停用、没有模型目录，不是任何模型的候选），其次死槽位、冷却中的，
	// 再次最久未用；剩余不到 aistudio.ShortCooldownMax 的短冷却（分钟限额、限流）不算冷却中，与健康的空闲 Worker 同档按最久未用排序
	victimAny victimMode = iota
	// victimCooling 只选冷却中的（剩余不少于 ShortCooldownMax，见 WarmWorkerStates）与不可调度的，不可调度、死槽位优先：
	// 同分区有 Worker 正在启动时按需替换也能换掉僵尸 Worker
	victimCooling
	// victimDead 只选死槽位（账号长时间冷却）与不可调度的，不可调度的优先：请求现场回收也能回收僵尸 Worker
	victimDead
	// victimReplaceable 为同分区有 Worker 正在启动时的按需替换（N3）：除 victimCooling 的范围外，健康的空闲 Worker 对请求的模型不可服务
	// （目录里没有、权益或通道不支持），或空闲超过 victimIdleReplaceAfter 时也可以换掉。原先这时只换冷却中的，而现场提升判断“能不能替换”
	// 用的是任意空闲 Worker（promotable），两边口径不一致，线上 pro 227 行、flash 48 行“Worker 容量已满”
	victimReplaceable
)

const (
	// victimIdleReplaceAfter 为 victimReplaceable 下健康空闲 Worker 可以被换掉的最短空闲时间
	victimIdleReplaceAfter = 2 * time.Minute
)

// pickIdleVictim 在分区内按 mode 选择空闲热 Worker。账户池只加锁一次取得全部候选的活动与冷却状态；
// 正在启动、替换、关闭或被其他路径选中的账户不作为候选（原因见 idleWarmVictimFor 的注释）
func (manager *accountWorkerManager) pickIdleVictim(excludeID string, modelID string, mode victimMode, partition aistudio.PoolScope) string {
	return manager.pickIdleVictimWith(excludeID, modelID, nil, mode, partition, time.Time{}, false)
}

// pickReplaceVictim 为按需替换与扩容的淘汰选号（N6）：与 pickIdleVictim 相同，另外不选就绪不到 rotationWarmupGrace 的 Worker
// （不可调度与死槽位除外）。冷却轮换早就有这条宽限，替换与扩容原先没有：刚冷启动的 Worker 第一个请求被裸限流冷却，
// 马上又被换掉、再冷启动一个，30 分钟 700–1000 次启停里有一部分就是这样来回换
func (manager *accountWorkerManager) pickReplaceVictim(excludeID string, modelID string, mode victimMode, partition aistudio.PoolScope) string {
	return manager.pickIdleVictimWith(excludeID, modelID, nil, mode, partition, time.Time{}, true)
}

// replaceVictimFor 为按需替换（ensureWorker）与现场提升判断“能否替换”（promotable）共用的淘汰选号（N3）：分区没有 Worker 在启动时
// 任意空闲 Worker（victimAny），有 Worker 在启动时按 victimReplaceable；都带启动宽限（见 pickReplaceVictim）。
// 是否有 Worker 在启动读无锁镜像 openingSet，调用方不必持有 rebalanceMu
func (manager *accountWorkerManager) replaceVictimFor(excludeID string, modelID string, partition aistudio.PoolScope) string {
	mode := victimAny
	if manager.openingInSet(partition) {
		mode = victimReplaceable
	}
	return manager.pickReplaceVictim(excludeID, modelID, mode, partition)
}

// openingInSet 返回分区内是否有 Worker 正在启动（读无锁镜像 openingSet）
func (manager *accountWorkerManager) openingInSet(partition aistudio.PoolScope) bool {
	ultra := manager.pool.UltraAccountIDs()
	opening := false
	manager.openingSet.Range(func(key, _ any) bool {
		if accountID, ok := key.(string); ok && partitionOf(accountID, ultra) == partition {
			opening = true
			return false
		}
		return true
	})
	return opening
}

// pickIdleVictimFor 与 pickIdleVictim 相同，另把在 deadModels 任一模型上长时间冷却的账户也算作死槽位（再多加锁一次）：
// 空闲回收不针对某个模型，按热门与主用模型判断死槽位，先关死槽位，留下还能服务主用模型的 Worker。
// idleBefore 非零时只在最近使用与就绪时间都早于它的 Worker 中挑选（空闲回收）
func (manager *accountWorkerManager) pickIdleVictimFor(
	excludeID string, modelID string, deadModels []string, mode victimMode, partition aistudio.PoolScope, idleBefore time.Time,
) string {
	return manager.pickIdleVictimWith(excludeID, modelID, deadModels, mode, partition, idleBefore, false)
}

// pickIdleVictimWith 为淘汰选号的实现；grace 为真时不选就绪不到 rotationWarmupGrace 的 Worker（不可调度与死槽位除外）
func (manager *accountWorkerManager) pickIdleVictimWith(
	excludeID string, modelID string, deadModels []string, mode victimMode, partition aistudio.PoolScope, idleBefore time.Time, grace bool,
) string {
	now := time.Now()
	// 启动宽限只在 newRuntime 打开（replaceGrace 为 rotationWarmupGrace）；测试直接构造的管理器为 0，替换照旧立即可选
	replaceGrace := time.Duration(manager.replaceGrace.Load())
	grace = grace && replaceGrace > 0
	warm := manager.warmAccountIDsIn(partition)
	candidates := make([]string, 0, len(warm))
	readyAt := make(map[string]time.Time, len(warm))
	for _, accountID := range warm {
		if accountID == excludeID || manager.victimReserved(accountID) {
			continue
		}
		manager.mu.RLock()
		account := manager.accounts[accountID]
		manager.mu.RUnlock()
		if account == nil {
			continue
		}
		// idleBefore 非零时（空闲回收）只在空闲超时的 Worker 中挑选：死槽位优先只在这些 Worker 之间生效，
		// 否则刚被其他模型的请求用过的死槽位排在最前，回收整轮放弃，真正空闲超时的 Worker 一直不被关闭
		if !idleBefore.IsZero() && time.Unix(0, account.readyAt.Load()).After(idleBefore) {
			continue
		}
		// 正在启动或替换 Worker 的账户不作为淘汰对象：启动流程持有该账户的 startupMu 之后还要拿 rebalanceMu，
		// 而淘汰在持有 rebalanceMu 时要拿 startupMu，两边互相等待会让全局调度永久停住
		if _, opening := manager.openingSet.Load(accountID); opening {
			continue
		}
		if !account.startupMu.TryLock() {
			continue
		}
		account.startupMu.Unlock()
		// 锁被占用说明该账户的 Worker 正在关闭、发布或核对（WAA proof 在锁外生成，请求占用由下方的租约活动判断），
		// 直接跳过而不是排队等待
		if !account.mu.TryLock() {
			continue
		}
		cleanupPending := accountWorkerCleanupPending(account)
		account.mu.Unlock()
		if cleanupPending {
			continue
		}
		candidates = append(candidates, accountID)
		readyAt[accountID] = time.Unix(0, account.readyAt.Load())
	}
	if len(candidates) == 0 {
		return ""
	}
	states := manager.pool.WarmWorkerStates(candidates, modelID, rotationMinRemaining)
	if len(deadModels) > 0 {
		for accountID := range manager.pool.LongCoolingSet(candidates, deadModels, rotationMinRemaining, false) {
			if state, ok := states[accountID]; ok {
				state.Dead = true
				states[accountID] = state
			}
		}
	}
	rank := func(state aistudio.WarmWorkerState) int {
		switch {
		case state.Unschedulable != "":
			return 4
		case state.Dead:
			return 3
		case state.Cooling:
			return 2
		case state.Unservable:
			return 1
		}
		return 0
	}
	var selected string
	var selectedState aistudio.WarmWorkerState
	for _, accountID := range candidates {
		state := states[accountID]
		unschedulable := state.Unschedulable != ""
		if state.Busy || mode == victimCooling && !state.Cooling && !unschedulable || mode == victimDead && !state.Dead && !unschedulable {
			continue
		}
		if mode == victimReplaceable && !unschedulable && !state.Dead && !state.Cooling && !state.Unservable &&
			now.Sub(state.LastUsed) < victimIdleReplaceAfter {
			continue
		}
		if grace && !unschedulable && !state.Dead && now.Sub(readyAt[accountID]) < replaceGrace {
			continue
		}
		if !idleBefore.IsZero() && state.LastUsed.After(idleBefore) {
			continue
		}
		if selected == "" || rank(state) > rank(selectedState) ||
			rank(state) == rank(selectedState) && state.LastUsed.Before(selectedState.LastUsed) {
			selected, selectedState = accountID, state
		}
	}
	return selected
}

// reclaimDeadWorker 现场回收分区内一个对该模型是死槽位（或账号已不可调度）的空闲 Worker，返回回收的账户；
// 没有死槽位、已被其他路径选中或淘汰时账号又忙碌时返回空。回收后不启动替补：调用的请求随后自己启动备用账号补上空出的槽位，
// 另起替补会与它争同一个槽位，替补先占到时请求按替换流程再淘汰一个空闲 Worker，或只能等替补冷启动、主用模型排队超限返回 429
func (manager *accountWorkerManager) reclaimDeadWorker(ctx context.Context, partition aistudio.PoolScope, modelID string) (string, error) {
	victim := manager.pickIdleVictim("", modelID, victimDead, partition)
	if victim == "" || !manager.tryReserveVictim(victim) {
		return "", nil
	}
	defer manager.releaseVictim(victim)
	evicted, err := manager.evictIdleWorker(ctx, victim)
	if err != nil {
		manager.requests.log(victim, "WARN", fmt.Sprintf("WAA Worker 回收冷却占位失败 | 错误=%v", err))
		return "", err
	}
	if !evicted {
		return "", nil
	}
	manager.dropRetirement(victim)
	manager.requests.log(victim, "INFO", fmt.Sprintf(
		"WAA Worker 回收冷却占位 | 模型=%s | %s=%d/%d",
		modelID, workerSlotName(partition), len(manager.warmAccountIDsIn(partition)), manager.maxActiveFor(partition),
	))
	return victim, nil
}

// deadWarmCounts 统计各分区驻留 Worker 中的死槽位数（账号全局长时间冷却，或在预热池的热门冷却模型、所在分区的主用模型上长时间不可用），
// 供状态页显示；口径与 registerDeadWorkers 的补登记一致
func (manager *accountWorkerManager) deadWarmCounts(warm []string, ultra map[string]struct{}) (normal int, ultraDead int) {
	normal = len(manager.pool.LongCoolingSet(inPartition(warm, ultra, aistudio.PoolScopeNormal),
		manager.deadSlotModelsFor(aistudio.PoolScopeNormal), rotationMinRemaining, false))
	ultraDead = len(manager.pool.LongCoolingSet(inPartition(warm, ultra, aistudio.PoolScopeUltra),
		manager.deadSlotModelsFor(aistudio.PoolScopeUltra), rotationMinRemaining, false))
	return normal, ultraDead
}

// unschedulableWarmCounts 统计各分区驻留 Worker 中不可调度的账号数（需要登录、已停用、不可用、没有模型目录，等待释放），供状态页显示；
// 口径与补登记释放（registerUnschedulableWorkers）一致，与死槽位分开计：死槽位的口径不变，同一账号既长时间冷却又不可调度时两边都计
func (manager *accountWorkerManager) unschedulableWarmCounts(warm []string, ultra map[string]struct{}) (normal int, ultraCount int) {
	for accountID := range manager.pool.UnschedulableSet(warm) {
		if partitionOf(accountID, ultra) == aistudio.PoolScopeUltra {
			ultraCount++
		} else {
			normal++
		}
	}
	return normal, ultraCount
}
