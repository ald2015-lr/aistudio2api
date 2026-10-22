package app

import (
	"context"
	"fmt"
	"slices"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/api"
	"github.com/Mag1cFall/AIStudio2API/internal/config"
)

// Worker 分区：每个 Worker 计入其账户当前所在号池对应的分区（aistudio.PoolScopeNormal 或 aistudio.PoolScopeUltra），
// 号池由账户权益与 ULTRA_INCLUDE_PRO 共同决定（见 aistudio.AccountPool.UltraAccountIDs）。
// WARM_WORKER_LIMIT、MAX_ACTIVE_WORKERS（warmTarget、maxActive）只约束普通分区，ULTRA_WARM_WORKER_LIMIT、
// ULTRA_MAX_ACTIVE_WORKERS（ultraWarmTarget、ultraMaxActive）约束 Ultra 分区，浏览器总数最多为两个分区峰值之和。
// 预热补齐、空闲回收、容量判断、淘汰选号（idleWarmVictimFor 与冷却轮换的 tryReserveVictim）都只在一个分区内进行：
// Ultra 请求不会淘汰普通 Worker，反之亦然。冷启动名额（WARM_STARTUP_CONCURRENCY）与单账户并发仍是全局共用的。
// 账户权益在持有 Worker 期间变化、或 ULTRA_INCLUDE_PRO 切换时，该 Worker 从此按新分区计数，不强制重启：
// 分区判断每次调用都重新读取成员，超出峰值的空闲 Worker 由空闲回收逐步收敛（见 poolRuleChanged）

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

// partitionLabel 返回分区的中文名称，用于运行日志
func partitionLabel(partition aistudio.PoolScope) string {
	if partition == aistudio.PoolScopeUltra {
		return "Ultra 分区"
	}
	return "普通分区"
}

// poolRuleChanged 在 ULTRA_INCLUDE_PRO 实际切换之后调用：Pro 账户换了号池，它们的 Worker 从此计入另一个分区。
// 分区计数、预热目标与容量判断每次调用都重新读取成员，不需要迁移；这里只处理按分区固定下来的状态并唤醒等待者：
//  1. 排空状态在排空开始时记下了分区，按当前成员改标，否则排空中的 Pro Worker 会在最长排空超时内
//     继续给旧分区记让出额度；
//  2. 唤醒等待容量的请求，按新成员重新判断（账户池的变化通知已唤醒等待账户的请求与调度队首）；
//  3. 记录生效日志，分区超过峰值时提示空闲 Worker 将逐步回收。
//
// 所有输入都读当前状态，可以重复执行；exclusive 为调用时生效的 ULTRA_EXCLUSIVE
func (manager *accountWorkerManager) poolRuleChanged(exclusive bool) {
	manager.drainMu.Lock()
	// 在 drainMu 内取成员快照（锁顺序 drainMu 再到账户池锁，与 SetDraining 相同）：startDrain 同样在 drainMu 内按当前成员算分区，
	// 两边不会用不同时刻的成员覆盖对方
	ultra := manager.pool.UltraAccountIDs()
	for accountID, drain := range manager.drains {
		drain.partition = partitionOf(accountID, ultra)
	}
	manager.drainMu.Unlock()
	manager.notifyScheduling()

	ultra = manager.pool.UltraAccountIDs()
	warm, opening := manager.WarmAccountIDs(), manager.OpeningAccountIDs()
	current := make(map[aistudio.PoolScope]int, len(workerPartitions))
	for _, partition := range workerPartitions {
		current[partition] = len(inPartition(warm, ultra, partition)) + len(inPartition(opening, ultra, partition))
	}
	includePro := manager.pool.UltraIncludePro()
	manager.requests.log("service", "INFO", fmt.Sprintf(
		"Pro 账户并入 Ultra 号池设置已更新并立即生效 | 并入=%t | Ultra 号池账户=%d | Ultra 分区 Worker=%d/%d | 普通分区 Worker=%d/%d",
		includePro, len(ultra),
		current[aistudio.PoolScopeUltra], manager.maxActiveFor(aistudio.PoolScopeUltra),
		current[aistudio.PoolScopeNormal], manager.maxActiveFor(aistudio.PoolScopeNormal),
	))
	for _, partition := range workerPartitions {
		if peak := manager.maxActiveFor(partition); current[partition] > peak {
			manager.requests.log("service", "WARN", fmt.Sprintf(
				"%s Worker 超过峰值 | 当前=%d | 峰值=%d | 空闲 Worker 将逐步回收，可调大对应峰值",
				partitionLabel(partition), current[partition], peak,
			))
		}
	}
	if includePro && exclusive {
		manager.requests.log("service", "WARN", "ULTRA_EXCLUSIVE=true 时 Pro 账户不再服务普通路径；Pro 账户上的文件与视频改从 /ultra 引用")
	}
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
// 没有可预热的 Ultra 号池账户时为 0，没有 Ultra 号池账户的部署不会为空分区反复分类候选（UltraAvailable 按号池成员计数，
// ULTRA_INCLUDE_PRO=true 时含 Pro，只有 Pro 账户的部署同样会预热 Ultra 分区）。
// 可预热的账户都属于 Ultra 号池时普通分区同样为 0：只有 Ultra 号池账户（或普通账户都不可用）的部署按 Ultra 分区完成预热，
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
// 分区归属在第一次需要时才读取（没有备用账户时不扫描账户池），每个分区的淘汰检查只做一次（要逐个尝试账户锁）。
// 占用与 ensureWorker 口径一致：驻留与正在启动的 Worker 都占槽位，排空中的 Worker 按已让出计（最多 drainLimit 个）；
// 原先只数驻留的 Worker，分区被启动中的 Worker 占满时排队的请求每秒都会发起一次注定按容量已满结束的后台扩容
type standbyCapacity struct {
	manager  *accountWorkerManager
	modelID  string
	resident []string
	ultra    map[string]struct{}
	active   map[aistudio.PoolScope]int
	cooling  map[aistudio.PoolScope]bool
	idle     map[aistudio.PoolScope]bool
	// serveable 为已核对过的备用账户能服务几个热门或主用模型（见 preferred），一次调度内每个账户最多核对一次；
	// deadModels 为各分区判断死槽位用的模型，一次调度内第一次需要时取一次
	serveable  map[string]int
	deadModels map[aistudio.PoolScope][]string
}

// standbyCapacity 按当前驻留的 Worker 创建分区容量判断
func (manager *accountWorkerManager) standbyCapacity(active []string, modelID string) *standbyCapacity {
	return &standbyCapacity{manager: manager, modelID: modelID, resident: active}
}

// partition 返回账户所在的分区；第一次调用时读取 Ultra 账户并按分区统计占用的 Worker（驻留与正在启动，减去排空让出的）
func (capacity *standbyCapacity) partition(accountID string) aistudio.PoolScope {
	if capacity.ultra == nil {
		capacity.ultra = capacity.manager.pool.UltraAccountIDs()
		capacity.active = make(map[aistudio.PoolScope]int, len(workerPartitions))
		capacity.cooling = make(map[aistudio.PoolScope]bool, len(workerPartitions))
		capacity.idle = make(map[aistudio.PoolScope]bool, len(workerPartitions))
		occupied := make(map[string]struct{}, len(capacity.resident))
		for _, resident := range capacity.resident {
			occupied[resident] = struct{}{}
		}
		for _, opening := range capacity.manager.OpeningAccountIDs() {
			occupied[opening] = struct{}{}
		}
		for accountID := range occupied {
			capacity.active[partitionOf(accountID, capacity.ultra)]++
		}
		for _, partition := range workerPartitions {
			capacity.active[partition] -= capacity.manager.drainCredit(partition, occupied)
		}
	}
	return partitionOf(accountID, capacity.ultra)
}

// preferred 返回按偏好排列的候选，分为两段：前 standbyPreferLimit 个按能服务的热门与主用模型个数从多到少排列（同数保持原顺序），
// 第二段为其余候选，保持原顺序且不复制（备用账户可能有上万个）。为其他模型的请求启动的备用账号也优先选还能服务主用模型的，
// 不直接启动一个服务不了主用模型的 Worker（目录里没有、权益不够或已长时间冷却，按候选集成员判断，见 aistudio.ServeableCounts）；
// 只核对前 standbyPreferLimit 个（控制账户池锁内的工作量），都服务不了时仍按原顺序使用。
// 主用模型按候选账户所在的分区取（Ultra 分区按 Ultra 主用模型，见 primaryModelListFor）：不限号池的选号候选跨两个分区，
// 前段按分区分组各核对一次（每个分区最多一次加锁，分区归属在调度内已缓存），不用两个列表的并集，免得普通号池的主用模型
// 左右 Ultra 备用账号的先后
func (capacity *standbyCapacity) preferred(list []string) [2][]string {
	if len(list) <= 1 {
		return [2][]string{list, nil}
	}
	if capacity.deadModels == nil {
		capacity.deadModels = make(map[aistudio.PoolScope][]string, len(workerPartitions))
		for _, partition := range workerPartitions {
			capacity.deadModels[partition] = capacity.manager.deadSlotModelsFor(partition)
		}
	}
	models := capacity.deadModels
	if len(models[aistudio.PoolScopeNormal]) == 0 && len(models[aistudio.PoolScopeUltra]) == 0 {
		return [2][]string{list, nil}
	}
	head := list[:min(standbyPreferLimit, len(list))]
	unknown := make(map[aistudio.PoolScope][]string, len(workerPartitions))
	for _, accountID := range head {
		if _, checked := capacity.serveable[accountID]; !checked {
			partition := capacity.partition(accountID)
			unknown[partition] = append(unknown[partition], accountID)
		}
	}
	for _, partition := range workerPartitions {
		accountIDs := unknown[partition]
		if len(accountIDs) == 0 {
			continue
		}
		if capacity.serveable == nil {
			capacity.serveable = make(map[string]int, len(head))
		}
		counts := capacity.manager.pool.ServeableCounts(accountIDs, models[partition], rotationMinRemaining)
		for _, accountID := range accountIDs {
			capacity.serveable[accountID] = counts[accountID]
		}
	}
	ordered := slices.Clone(head)
	slices.SortStableFunc(ordered, func(left string, right string) int {
		return capacity.serveable[right] - capacity.serveable[left]
	})
	return [2][]string{ordered, list[len(head):]}
}

func (capacity *standbyCapacity) room(partition aistudio.PoolScope) bool {
	return capacity.active[partition] < capacity.manager.maxActiveFor(partition)
}

// coolingVictim 判断已满的分区内是否有在该模型上冷却的空闲 Worker 可以替换；剩余不到 aistudio.ShortCooldownMax 的短冷却
// （分钟限额、限流）不算：一分钟内就恢复，为扩容替换它得不偿失
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

// expandable 返回可以在后台扩容的备用账户：所在分区未满，或分区已满但有冷却中（剩余不少于 ShortCooldownMax）的空闲 Worker 可以替换；
// 优先没有 Worker 的账户，其次优先没有在热门或主用模型上长时间冷却的账户（见 preferred）
func (capacity *standbyCapacity) expandable(standby []string) string {
	for _, candidates := range [][]string{capacity.manager.coldAccounts(standby), standby} {
		for _, segment := range capacity.preferred(candidates) {
			for _, accountID := range segment {
				partition := capacity.partition(accountID)
				if capacity.room(partition) || capacity.coolingVictim(partition) {
					return accountID
				}
			}
		}
	}
	return ""
}

// reclaimDead 在备用账户所在的分区已满时现场回收一个对该模型是死槽位（账号长时间冷却）的空闲 Worker，
// 返回回收的账户与分区；每次调度最多回收一个，每个分区只尝试一次，没有死槽位时返回空，由随后的扩容或替换按原流程处理
func (capacity *standbyCapacity) reclaimDead(ctx context.Context, standby []string) (string, aistudio.PoolScope) {
	checked := make(map[aistudio.PoolScope]bool, len(workerPartitions))
	for _, accountID := range standby {
		partition := capacity.partition(accountID)
		if checked[partition] || capacity.room(partition) {
			continue
		}
		checked[partition] = true
		if victim, _ := capacity.manager.reclaimDeadWorker(ctx, partition, capacity.modelID); victim != "" {
			return victim, partition
		}
	}
	return "", aistudio.PoolScopeAll
}

// promotable 返回现场启动 Worker 的备用账户：所在分区未满时优先没有 Worker 的账户；
// allowVictim 时（没有热候选）分区已满但同分区有可淘汰的空闲 Worker 也可以。同样优先没有在热门或主用模型上长时间冷却的账户
func (capacity *standbyCapacity) promotable(standby []string, allowVictim bool) string {
	for _, candidates := range [][]string{capacity.manager.coldAccounts(standby), standby} {
		for _, segment := range capacity.preferred(candidates) {
			for _, accountID := range segment {
				if capacity.room(capacity.partition(accountID)) {
					return accountID
				}
			}
		}
	}
	if !allowVictim {
		return ""
	}
	for _, segment := range capacity.preferred(standby) {
		for _, accountID := range segment {
			if capacity.idleVictim(capacity.partition(accountID)) {
				return accountID
			}
		}
	}
	return ""
}

// ultraWorkerCounts 返回 Ultra 分区的 Worker 计数，供管理页状态显示；dead 为其中账号长时间冷却的死槽位数
func (manager *accountWorkerManager) ultraWorkerCounts(
	ultra map[string]struct{}, warm []string, starting []string, occupied int, dead int,
) api.AdminWorkerPartition {
	partition := aistudio.PoolScopeUltra
	return api.AdminWorkerPartition{
		Warm: len(inPartition(warm, ultra, partition)), Starting: len(inPartition(starting, ultra, partition)),
		Occupied: occupied, Dead: dead, Target: manager.prewarmTargetFor(partition),
		WarmLimit: manager.warmTargetFor(partition), Max: manager.maxActiveFor(partition),
	}
}
