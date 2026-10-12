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
// 冷却到期后能立即服务

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
}

// global 判断登记是否为全局冷却
func (entry retirement) global() bool {
	return len(entry.models) == 0
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
	manager.mu.RLock()
	account := manager.accounts[accountID]
	manager.mu.RUnlock()
	if account == nil || !account.warm.Load() {
		return false
	}
	manager.retireMu.Lock()
	if manager.retirements == nil {
		manager.retirements = make(map[string]*retirement)
	}
	if existing := manager.retirements[accountID]; existing != nil {
		switch {
		case modelID == "":
			existing.models = nil
		case existing.global():
		case !slices.Contains(existing.models, modelID):
			existing.models = append(slices.Clone(existing.models), modelID)
			slices.Sort(existing.models)
		}
		manager.retireMu.Unlock()
		return true
	}
	entry := &retirement{since: time.Now()}
	if modelID != "" {
		entry.models = []string{modelID}
	}
	manager.retirements[accountID] = entry
	manager.retirePending.Store(int32(len(manager.retirements)))
	manager.retireMu.Unlock()
	manager.wakeRetire()
	return true
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

// dropRetirement 删除一条登记
func (manager *accountWorkerManager) dropRetirement(accountID string) {
	manager.retireMu.Lock()
	delete(manager.retirements, accountID)
	manager.retirePending.Store(int32(len(manager.retirements)))
	manager.retireMu.Unlock()
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
}

// retirePass 处理一轮登记，返回处理后仍待淘汰的登记数。每条登记先核对账号仍有驻留 Worker、仍在长时间冷却
// （冷却被清除或缩短时放弃淘汰），再要求分区内有可接替的账户；账号忙碌、正在启动或被其他路径选中时留到下一轮。
// 关闭并发进行，数量不超过启动并发
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
	// spare 为各分区可接替的账户数，第一次需要时统计，每启动一次淘汰减一
	spare := make(map[aistudio.PoolScope]int, len(workerPartitions))
	spareKnown := make(map[aistudio.PoolScope]bool, len(workerPartitions))
	pendingIn := make(map[aistudio.PoolScope]int, len(workerPartitions))
	for index := range candidates {
		candidates[index].partition = partitionOf(candidates[index].accountID, ultra)
		pendingIn[candidates[index].partition]++
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
		// 只要登记里还有一个模型（或全局）仍长时间冷却，账号就仍是死槽位；日志只写仍在冷却的范围
		cooling, ok := candidate.entry.stillCooling(manager.pool, accountID)
		if !ok {
			manager.dropRetirement(accountID)
			continue
		}
		candidate.entry = cooling
		if _, starting := manager.openingSet.Load(accountID); starting {
			continue
		}
		// 与 idleWarmVictimFor 相同：正在启动或替换的账户不淘汰，避免与持有 startupMu 等 rebalanceMu 的启动流程互相等待
		if !account.startupMu.TryLock() {
			continue
		}
		account.startupMu.Unlock()
		partition := candidate.partition
		if !spareKnown[partition] {
			spareKnown[partition] = true
			occupied := make(map[string]struct{}, len(warm)+len(opening))
			for _, id := range inPartition(warm, ultra, partition) {
				occupied[id] = struct{}{}
			}
			for _, id := range inPartition(opening, ultra, partition) {
				occupied[id] = struct{}{}
			}
			spare[partition] = manager.pool.SpareAccounts(partition, occupied, nil, rotationMinRemaining, pendingIn[partition])
		}
		if spare[partition] <= 0 {
			manager.deferRetirement(accountID, now.Add(retireNoSpareBackoff))
			continue
		}
		// 便宜的预检：账号正在服务请求时不取独占租约，等它的请求释放租约后被再次唤醒
		if busy, _ := manager.pool.Activity(accountID); busy {
			continue
		}
		if !manager.tryReserveVictim(accountID) {
			continue
		}
		spare[partition]--
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

// finishRetirement 关闭一个已选中的待淘汰 Worker；调用方已 tryReserveVictim
func (manager *accountWorkerManager) finishRetirement(victim retirementCandidate) {
	accountID := victim.accountID
	evicted, err := manager.evictIdleWorker(manager.lifecycle, accountID)
	manager.releaseVictim(accountID)
	switch {
	case err != nil:
		manager.requests.log(accountID, "WARN", fmt.Sprintf("WAA Worker 冷却淘汰失败 | 范围=%s | 错误=%v", victim.entry.scopeLabel(), err))
		manager.deferRetirement(accountID, time.Now().Add(retireFailureBackoff))
	case !evicted:
		// 取独占租约时账号又有了请求（或其他进程持有文件锁），等下一次唤醒
	default:
		manager.dropRetirement(accountID)
		manager.requests.log(accountID, "INFO", fmt.Sprintf(
			"WAA Worker 冷却淘汰 | 范围=%s | 登记后=%s | %s=%d/%d",
			victim.entry.scopeLabel(), time.Since(victim.entry.since).Round(time.Millisecond),
			workerSlotName(victim.partition), len(manager.warmAccountIDsIn(victim.partition)), manager.maxActiveFor(victim.partition),
		))
		if refill := manager.afterRetire.Load(); refill != nil {
			(*refill)()
		}
	}
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

// victimMode 为淘汰选号的范围
type victimMode int

const (
	// victimAny 任意空闲 Worker：死槽位优先，其次冷却中的，再次最久未用
	victimAny victimMode = iota
	// victimCooling 只选冷却中的（不论剩余多久），死槽位优先
	victimCooling
	// victimDead 只选死槽位（账号长时间冷却）
	victimDead
)

// pickIdleVictim 在分区内按 mode 选择空闲热 Worker。账户池只加锁一次取得全部候选的活动与冷却状态；
// 正在启动、替换、关闭或被其他路径选中的账户不作为候选（原因见 idleWarmVictimFor 的注释）
func (manager *accountWorkerManager) pickIdleVictim(excludeID string, modelID string, mode victimMode, partition aistudio.PoolScope) string {
	warm := manager.warmAccountIDsIn(partition)
	candidates := make([]string, 0, len(warm))
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
	}
	if len(candidates) == 0 {
		return ""
	}
	states := manager.pool.WarmWorkerStates(candidates, modelID, rotationMinRemaining)
	rank := func(state aistudio.WarmWorkerState) int {
		switch {
		case state.Dead:
			return 2
		case state.Cooling:
			return 1
		}
		return 0
	}
	var selected string
	var selectedState aistudio.WarmWorkerState
	for _, accountID := range candidates {
		state := states[accountID]
		if state.Busy || mode == victimCooling && !state.Cooling || mode == victimDead && !state.Dead {
			continue
		}
		if selected == "" || rank(state) > rank(selectedState) ||
			rank(state) == rank(selectedState) && state.LastUsed.Before(selectedState.LastUsed) {
			selected, selectedState = accountID, state
		}
	}
	return selected
}

// reclaimDeadWorker 现场回收分区内一个对该模型是死槽位的空闲 Worker，返回回收的账户；
// 没有死槽位、已被其他路径选中或淘汰时账号又忙碌时返回空
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

// deadWarmCounts 统计各分区驻留 Worker 中的死槽位数（账号全局长时间冷却，或在预热池的热门冷却模型上长时间不可用），供状态页显示
func (manager *accountWorkerManager) deadWarmCounts(warm []string, ultra map[string]struct{}) (normal int, ultraDead int) {
	for accountID := range manager.pool.LongCoolingSet(warm, manager.hotModelList(), rotationMinRemaining, false) {
		if partitionOf(accountID, ultra) == aistudio.PoolScopeUltra {
			ultraDead++
		} else {
			normal++
		}
	}
	return normal, ultraDead
}
