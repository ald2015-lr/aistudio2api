package app

import (
	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/api"
	"github.com/Mag1cFall/AIStudio2API/internal/config"
)

// Worker 分区：每个 Worker 计入其账户当前权益所属的分区（aistudio.PoolScopeNormal 或 aistudio.PoolScopeUltra）。
// WARM_WORKER_LIMIT、MAX_ACTIVE_WORKERS（warmTarget、maxActive）只约束普通分区，ULTRA_WARM_WORKER_LIMIT、
// ULTRA_MAX_ACTIVE_WORKERS（ultraWarmTarget、ultraMaxActive）约束 Ultra 分区，浏览器总数最多为两个分区峰值之和。
// 预热补齐、空闲回收、容量判断、淘汰选号（idleWarmVictimFor 与冷却轮换的 tryReserveVictim）都只在一个分区内进行：
// Ultra 请求不会淘汰普通 Worker，反之亦然。冷启动名额（WARM_STARTUP_CONCURRENCY）与单账户并发仍是全局共用的。
// 账户权益在持有 Worker 期间变化时，该 Worker 从此按新分区计数，不强制重启

// workerPartitions 为全部 Worker 分区，普通分区在前
var workerPartitions = [...]aistudio.PoolScope{aistudio.PoolScopeNormal, aistudio.PoolScopeUltra}

// defaultUltraCapacity 返回 Ultra 分区的默认常驻数与峰值数（与 config.Default 一致）
func defaultUltraCapacity() (int, int) {
	defaults := config.Default()
	return defaults.UltraWarmWorkerLimit, defaults.UltraMaxActiveWorkers
}

// setUltraCapacity 更新 Ultra 分区的常驻数与峰值数
func (manager *accountWorkerManager) setUltraCapacity(warm int, maxActive int) {
	manager.ultraWarmTarget.Store(int64(warm))
	manager.ultraMaxActive.Store(int64(maxActive))
}

// warmTargetFor 返回分区的常驻 Worker 数
func (manager *accountWorkerManager) warmTargetFor(partition aistudio.PoolScope) int {
	if partition == aistudio.PoolScopeUltra {
		return int(manager.ultraWarmTarget.Load())
	}
	return manager.warmTargetValue()
}

// maxActiveFor 返回分区的峰值 Worker 数
func (manager *accountWorkerManager) maxActiveFor(partition aistudio.PoolScope) int {
	if partition == aistudio.PoolScopeUltra {
		return int(manager.ultraMaxActive.Load())
	}
	return manager.maxActiveValue()
}

// partitionOf 按 UltraAccountIDs 的结果返回账户所属的分区
func partitionOf(accountID string, ultra map[string]struct{}) aistudio.PoolScope {
	if _, exists := ultra[accountID]; exists {
		return aistudio.PoolScopeUltra
	}
	return aistudio.PoolScopeNormal
}

// inPartition 返回属于分区的账户，保持原有顺序
func inPartition(accountIDs []string, ultra map[string]struct{}, partition aistudio.PoolScope) []string {
	result := make([]string, 0, len(accountIDs))
	for _, accountID := range accountIDs {
		if partitionOf(accountID, ultra) == partition {
			result = append(result, accountID)
		}
	}
	return result
}

// openingIn 判断分区内是否有正在启动的 Worker；调用方持有 rebalanceMu。
// 另一个分区的启动不影响本分区的淘汰选号与空闲回收
func (manager *accountWorkerManager) openingIn(partition aistudio.PoolScope, ultra map[string]struct{}) bool {
	for accountID := range manager.openings {
		if partitionOf(accountID, ultra) == partition {
			return true
		}
	}
	return false
}

// warmAccountIDsIn 返回分区内驻留的 Worker
func (manager *accountWorkerManager) warmAccountIDsIn(partition aistudio.PoolScope) []string {
	return inPartition(manager.WarmAccountIDs(), manager.pool.UltraAccountIDs(), partition)
}

// prewarmTargetFor 返回分区需要预热的账户数：分区常驻数与分区内可预热的账户数取小
func (manager *accountWorkerManager) prewarmTargetFor(partition aistudio.PoolScope) int {
	return manager.prewarmTargetFrom(partition, manager.bootstrapSummary())
}

func (manager *accountWorkerManager) prewarmTargetFrom(partition aistudio.PoolScope, summary aistudio.BootstrapSummary) int {
	available := summary.Available - summary.UltraAvailable
	if partition == aistudio.PoolScopeUltra {
		available = summary.UltraAvailable
	}
	return min(manager.warmTargetFor(partition), available)
}

// prewarmNeeded 判断是否有分区的驻留与启动中 Worker 少于该分区的预热目标
func (manager *accountWorkerManager) prewarmNeeded() bool {
	_, short := manager.shortPartition()
	return short
}

// shortPartition 返回第一个驻留与启动中 Worker 少于预热目标的分区。按分区判断：
// 一个分区按需扩容超出常驻数时，不能掩盖另一个分区预热不足
func (manager *accountWorkerManager) shortPartition() (aistudio.PoolScope, bool) {
	ultra := manager.pool.UltraAccountIDs()
	warm, opening := manager.WarmAccountIDs(), manager.OpeningAccountIDs()
	summary := manager.bootstrapSummary()
	for _, partition := range workerPartitions {
		if len(inPartition(warm, ultra, partition))+len(inPartition(opening, ultra, partition)) < manager.prewarmTargetFrom(partition, summary) {
			return partition, true
		}
	}
	return aistudio.PoolScopeAll, false
}

// workerLogName 返回分区在运行日志里的 Worker 名称：普通分区沿用原有写法，Ultra 分区加前缀
func workerLogName(partition aistudio.PoolScope) string {
	if partition == aistudio.PoolScopeUltra {
		return "Ultra WAA Worker"
	}
	return "WAA Worker"
}

// workerSlotName 返回分区在槽位说明里的名称
func workerSlotName(partition aistudio.PoolScope) string {
	if partition == aistudio.PoolScopeUltra {
		return "Ultra Worker"
	}
	return "Worker"
}

// fillTargetFor 返回预热循环中分区要补齐到的常驻数：普通分区按 WARM_WORKER_LIMIT；Ultra 分区按 ULTRA_WARM_WORKER_LIMIT，
// 没有可预热的 Ultra 账户时为 0，没有 Ultra 账户的部署不会为空分区反复分类候选。
// 可预热的账户都是 Ultra 时普通分区同样为 0：只有 Ultra 账户（或普通账户都不可用）的部署按 Ultra 分区完成预热，
// Ultra 常驻数为 0 时直接完成首轮预热、Ultra Worker 按需启动；没有可预热账户时仍按原来的方式报错
func (manager *accountWorkerManager) fillTargetFor(partition aistudio.PoolScope, summary aistudio.BootstrapSummary) int {
	if partition == aistudio.PoolScopeUltra && summary.UltraAvailable == 0 {
		return 0
	}
	if partition == aistudio.PoolScopeNormal && summary.UltraAvailable > 0 && summary.Available == summary.UltraAvailable {
		return 0
	}
	return manager.warmTargetFor(partition)
}

// fullPartition 返回第一个 Worker 槽位已满的分区
func fullPartition(capacityFull map[aistudio.PoolScope]bool) (aistudio.PoolScope, bool) {
	for _, partition := range workerPartitions {
		if capacityFull[partition] {
			return partition, true
		}
	}
	return aistudio.PoolScopeAll, false
}

// standbyCapacity 在一次调度中按 Worker 分区判断备用账户能否启动 Worker：分区未满，或分区已满但有可淘汰的空闲 Worker。
// 分区归属在第一次需要时才读取（没有备用账户时不扫描账户池），每个分区的淘汰检查只做一次（要逐个尝试账户锁）
type standbyCapacity struct {
	manager  *accountWorkerManager
	modelID  string
	resident []string
	ultra    map[string]struct{}
	active   map[aistudio.PoolScope]int
	cooling  map[aistudio.PoolScope]bool
	idle     map[aistudio.PoolScope]bool
}

// standbyCapacity 按当前驻留的 Worker 创建分区容量判断
func (manager *accountWorkerManager) standbyCapacity(active []string, modelID string) *standbyCapacity {
	return &standbyCapacity{manager: manager, modelID: modelID, resident: active}
}

// partition 返回账户所在的分区；第一次调用时读取 Ultra 账户并按分区统计驻留的 Worker
func (capacity *standbyCapacity) partition(accountID string) aistudio.PoolScope {
	if capacity.ultra == nil {
		capacity.ultra = capacity.manager.pool.UltraAccountIDs()
		capacity.active = make(map[aistudio.PoolScope]int, len(workerPartitions))
		capacity.cooling = make(map[aistudio.PoolScope]bool, len(workerPartitions))
		capacity.idle = make(map[aistudio.PoolScope]bool, len(workerPartitions))
		for _, resident := range capacity.resident {
			capacity.active[partitionOf(resident, capacity.ultra)]++
		}
	}
	return partitionOf(accountID, capacity.ultra)
}

func (capacity *standbyCapacity) room(partition aistudio.PoolScope) bool {
	return capacity.active[partition] < capacity.manager.maxActiveFor(partition)
}

// coolingVictim 判断已满的分区内是否有在该模型上冷却的空闲 Worker 可以替换
func (capacity *standbyCapacity) coolingVictim(partition aistudio.PoolScope) bool {
	if capacity.room(partition) {
		return false
	}
	found, checked := capacity.cooling[partition]
	if !checked {
		found = capacity.manager.idleWarmVictimFor("", capacity.modelID, true, partition) != ""
		capacity.cooling[partition] = found
	}
	return found
}

// idleVictim 判断分区内是否有可淘汰的空闲 Worker
func (capacity *standbyCapacity) idleVictim(partition aistudio.PoolScope) bool {
	found, checked := capacity.idle[partition]
	if !checked {
		found = capacity.manager.idleWarmVictimFor("", "", false, partition) != ""
		capacity.idle[partition] = found
	}
	return found
}

// expandable 返回可以在后台扩容的备用账户：所在分区未满，或分区已满但有冷却中的空闲 Worker 可以替换；优先没有 Worker 的账户
func (capacity *standbyCapacity) expandable(standby []string) string {
	for _, candidates := range [][]string{capacity.manager.coldAccounts(standby), standby} {
		for _, accountID := range candidates {
			partition := capacity.partition(accountID)
			if capacity.room(partition) || capacity.coolingVictim(partition) {
				return accountID
			}
		}
	}
	return ""
}

// promotable 返回现场启动 Worker 的备用账户：所在分区未满时优先没有 Worker 的账户；
// allowVictim 时（没有热候选）分区已满但同分区有可淘汰的空闲 Worker 也可以
func (capacity *standbyCapacity) promotable(standby []string, allowVictim bool) string {
	for _, candidates := range [][]string{capacity.manager.coldAccounts(standby), standby} {
		for _, accountID := range candidates {
			if capacity.room(capacity.partition(accountID)) {
				return accountID
			}
		}
	}
	if !allowVictim {
		return ""
	}
	for _, accountID := range standby {
		if capacity.idleVictim(capacity.partition(accountID)) {
			return accountID
		}
	}
	return ""
}

// ultraWorkerCounts 返回 Ultra 分区的 Worker 计数，供管理页状态显示
func (manager *accountWorkerManager) ultraWorkerCounts(ultra map[string]struct{}, warm []string, starting []string, occupied int) api.AdminWorkerPartition {
	partition := aistudio.PoolScopeUltra
	return api.AdminWorkerPartition{
		Warm: len(inPartition(warm, ultra, partition)), Starting: len(inPartition(starting, ultra, partition)),
		Occupied: occupied, Target: manager.prewarmTargetFor(partition),
		WarmLimit: manager.warmTargetFor(partition), Max: manager.maxActiveFor(partition),
	}
}
