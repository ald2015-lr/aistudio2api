package app

import (
	"cmp"
	"context"
	"fmt"
	"slices"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/api"
	"github.com/Mag1cFall/AIStudio2API/internal/config"
)

// Worker 分区：每个 Worker 计入其账户当前所在号池对应的分区（aistudio.PoolScopeNormal、aistudio.PoolScopeUltra 或 aistudio.PoolScopePro），
// 号池由账户权益与 PRO_POOL、ULTRA_INCLUDE_PRO 共同决定（见 aistudio.AccountPool.PartitionMembers）。
// WARM_WORKER_LIMIT、MAX_ACTIVE_WORKERS（warmTarget、maxActive）只约束普通分区，ULTRA_WARM_WORKER_LIMIT、
// ULTRA_MAX_ACTIVE_WORKERS（ultraWarmTarget、ultraMaxActive）约束 Ultra 分区，PRO_WARM_WORKER_LIMIT、PRO_MAX_ACTIVE_WORKERS
// （proWarmTarget、proMaxActive）约束 Pro 分区，浏览器总数最多为各分区峰值之和（PRO_POOL=false 时 Pro 分区为空）。
// 预热补齐、空闲回收、容量判断、淘汰选号（idleWarmVictimFor 与冷却轮换的 tryReserveVictim）都只在一个分区内进行：
// 一个分区的请求不会淘汰其他分区的 Worker。冷启动名额（WARM_STARTUP_CONCURRENCY）与单账户并发仍是全局共用的，
// 预热时按 workerPartitions 的顺序（普通、Ultra、Pro）分给各分区。
// 账户权益在持有 Worker 期间变化、或 PRO_POOL、ULTRA_INCLUDE_PRO 切换时，该 Worker 从此按新分区计数，不强制重启：
// 分区判断每次调用都重新读取成员，超出峰值的空闲 Worker 由空闲回收逐步收敛（见 poolRuleChanged）

// workerPartitions 为全部 Worker 分区，普通分区在前；Pro 分区追加在最后，预热时不推后 Ultra 分区
var workerPartitions = [...]aistudio.PoolScope{aistudio.PoolScopeNormal, aistudio.PoolScopeUltra, aistudio.PoolScopePro}

// partitionCounts 为按分区（PoolScope 下标）的计数；非分区下标不用
type partitionCounts [aistudio.PoolScopeCount]int

// partitionNames 为分区在日志与状态里的名称（见 partitionLabel、workerLogName、workerSlotName、balancedPartitionName），
// 普通与 Ultra 分区沿用原有写法；不是分区的取值没有名称
type partitionNames struct {
	label, worker, slot, balanced string
}

var partitionNameTable = [aistudio.PoolScopeCount]partitionNames{
	aistudio.PoolScopeNormal: {label: "普通分区", worker: "WAA Worker", slot: "Worker", balanced: "普通"},
	aistudio.PoolScopeUltra:  {label: "Ultra 分区", worker: "Ultra WAA Worker", slot: "Ultra Worker", balanced: "Ultra"},
	aistudio.PoolScopePro:    {label: "Pro 分区", worker: "Pro WAA Worker", slot: "Pro Worker", balanced: "Pro"},
}

// namesOf 返回分区的名称；不是分区的取值按普通分区（与原先 if Ultra 否则普通的写法相同）
func namesOf(partition aistudio.PoolScope) partitionNames {
	return partitionNameTable[namesPartition(partition)]
}

// namesPartition 把不是分区的取值归到普通分区（计数与名称沿用原先 if Ultra 否则普通的口径）
func namesPartition(partition aistudio.PoolScope) aistudio.PoolScope {
	if partition.IsPartition() {
		return partition
	}
	return aistudio.PoolScopeNormal
}

// defaultUltraCapacity 返回 Ultra 分区的默认常驻数与峰值数（与 config.Default 一致）
func defaultUltraCapacity() (int, int) {
	defaults := config.Default()
	return defaults.UltraWarmWorkerLimit, defaults.UltraMaxActiveWorkers
}

// defaultProCapacity 返回 Pro 分区的默认常驻数与峰值数（与 config.Default 一致）
func defaultProCapacity() (int, int) {
	defaults := config.Default()
	return defaults.ProWarmWorkerLimit, defaults.ProMaxActiveWorkers
}

// setUltraCapacity 更新 Ultra 分区的常驻数与峰值数
func (manager *accountWorkerManager) setUltraCapacity(warm int, maxActive int) {
	manager.ultraWarmTarget.Store(int64(warm))
	manager.ultraMaxActive.Store(int64(maxActive))
}

// setProCapacity 更新 Pro 分区的常驻数与峰值数
func (manager *accountWorkerManager) setProCapacity(warm int, maxActive int) {
	manager.proWarmTarget.Store(int64(warm))
	manager.proMaxActive.Store(int64(maxActive))
}

// warmTargetFor 返回分区的常驻 Worker 数；不是分区的取值为 0（调用方都传分区，漏掉时不按普通分区的上限放行）
func (manager *accountWorkerManager) warmTargetFor(partition aistudio.PoolScope) int {
	switch partition {
	case aistudio.PoolScopeNormal:
		return manager.warmTargetValue()
	case aistudio.PoolScopeUltra:
		return int(manager.ultraWarmTarget.Load())
	case aistudio.PoolScopePro:
		return int(manager.proWarmTarget.Load())
	default:
		return 0
	}
}

// maxActiveFor 返回分区的峰值 Worker 数；不是分区的取值为 0
func (manager *accountWorkerManager) maxActiveFor(partition aistudio.PoolScope) int {
	switch partition {
	case aistudio.PoolScopeNormal:
		return manager.maxActiveValue()
	case aistudio.PoolScopeUltra:
		return int(manager.ultraMaxActive.Load())
	case aistudio.PoolScopePro:
		return int(manager.proMaxActive.Load())
	default:
		return 0
	}
}

// partitionOf 按 PartitionMembers 的快照返回账户所属的分区
func partitionOf(accountID string, members aistudio.PartitionSet) aistudio.PoolScope {
	return members.Of(accountID)
}

// inPartition 返回属于分区的账户，保持原有顺序
func inPartition(accountIDs []string, members aistudio.PartitionSet, partition aistudio.PoolScope) []string {
	result := make([]string, 0, len(accountIDs))
	for _, accountID := range accountIDs {
		if partitionOf(accountID, members) == partition {
			result = append(result, accountID)
		}
	}
	return result
}

// countByPartition 按分区统计账户数
func countByPartition(accountIDs []string, members aistudio.PartitionSet) partitionCounts {
	var counts partitionCounts
	for _, accountID := range accountIDs {
		counts[partitionOf(accountID, members)]++
	}
	return counts
}

// openingIn 判断分区内是否有正在启动的 Worker；调用方持有 rebalanceMu。
// 其他分区的启动不影响本分区的淘汰选号与空闲回收
func (manager *accountWorkerManager) openingIn(partition aistudio.PoolScope, members aistudio.PartitionSet) bool {
	for accountID := range manager.openings {
		if partitionOf(accountID, members) == partition {
			return true
		}
	}
	return false
}

// warmAccountIDsIn 返回分区内驻留的 Worker
func (manager *accountWorkerManager) warmAccountIDsIn(partition aistudio.PoolScope) []string {
	return inPartition(manager.WarmAccountIDs(), manager.pool.PartitionMembers(), partition)
}

// prewarmTargetFor 返回分区需要预热的账户数：分区常驻数与分区内可预热的账户数取小
func (manager *accountWorkerManager) prewarmTargetFor(partition aistudio.PoolScope) int {
	return manager.prewarmTargetFrom(partition, manager.bootstrapSummary())
}

func (manager *accountWorkerManager) prewarmTargetFrom(partition aistudio.PoolScope, summary aistudio.BootstrapSummary) int {
	return min(manager.warmTargetFor(partition), summary.AvailableIn(partition))
}

// prewarmNeeded 判断是否有分区的驻留与启动中 Worker 少于该分区的预热目标
func (manager *accountWorkerManager) prewarmNeeded() bool {
	_, short := manager.shortPartition()
	return short
}

// shortPartition 返回第一个驻留与启动中 Worker 少于预热目标的分区。按分区判断：
// 一个分区按需扩容超出常驻数时，不能掩盖另一个分区预热不足
func (manager *accountWorkerManager) shortPartition() (aistudio.PoolScope, bool) {
	members := manager.pool.PartitionMembers()
	warm, opening := countByPartition(manager.WarmAccountIDs(), members), countByPartition(manager.OpeningAccountIDs(), members)
	summary := manager.bootstrapSummary()
	for _, partition := range workerPartitions {
		if warm[partition]+opening[partition] < manager.prewarmTargetFrom(partition, summary) {
			return partition, true
		}
	}
	return aistudio.PoolScopeAll, false
}

// partitionLabel 返回分区的中文名称，用于运行日志
func partitionLabel(partition aistudio.PoolScope) string {
	return namesOf(partition).label
}

// poolRuleChange 为一次号池划分规则切换里变化的部分：includePro 为生效的 ULTRA_INCLUDE_PRO（PRO_POOL=true 时为假）变化了，
// proPool 为 PRO_POOL 变化了
type poolRuleChange struct {
	includePro bool
	proPool    bool
}

// poolRuleChanged 在号池成员规则（PRO_POOL、ULTRA_INCLUDE_PRO）实际切换之后调用：Pro 账户换了号池，它们的 Worker 从此计入另一个分区。
// 分区计数、预热目标与容量判断每次调用都重新读取成员，不需要迁移；这里只处理按分区固定下来的状态并唤醒等待者：
//  1. 排空状态在排空开始时记下了分区，按当前成员改标，否则排空中的 Pro Worker 会在最长排空超时内
//     继续给旧分区记让出额度；
//  2. 唤醒等待容量的请求，按新成员重新判断（账户池的变化通知已唤醒等待账户的请求与调度队首）；
//  3. 记录生效日志，分区超过峰值时提示空闲 Worker 将逐步回收。
//
// 所有输入都读当前状态，可以重复执行；exclusive 为调用时生效的 ULTRA_EXCLUSIVE，change 为这次变化的开关（决定写哪几行日志）。
// 只切换 ULTRA_INCLUDE_PRO（PRO_POOL=false）时日志与只有两个号池时逐字相同
func (manager *accountWorkerManager) poolRuleChanged(exclusive bool, change poolRuleChange) {
	manager.drainMu.Lock()
	// 在 drainMu 内取成员快照（锁顺序 drainMu 再到账户池锁，与 SetDraining 相同）：startDrain 同样在 drainMu 内按当前成员算分区，
	// 两边不会用不同时刻的成员覆盖对方
	members := manager.pool.PartitionMembers()
	for accountID, drain := range manager.drains {
		drain.partition = partitionOf(accountID, members)
	}
	manager.drainMu.Unlock()
	manager.notifyScheduling()

	members = manager.pool.PartitionMembers()
	warm, opening := countByPartition(manager.WarmAccountIDs(), members), countByPartition(manager.OpeningAccountIDs(), members)
	var current partitionCounts
	for _, partition := range workerPartitions {
		current[partition] = warm[partition] + opening[partition]
	}
	includePro, proPool := manager.pool.UltraIncludePro(), manager.pool.ProPool()
	if change.includePro && !proPool {
		manager.requests.log("service", "INFO", fmt.Sprintf(
			"Pro 账户并入 Ultra 号池设置已更新并立即生效 | 并入=%t | Ultra 号池账户=%d | Ultra 分区 Worker=%d/%d | 普通分区 Worker=%d/%d",
			includePro, members.Count(aistudio.PoolScopeUltra),
			current[aistudio.PoolScopeUltra], manager.maxActiveFor(aistudio.PoolScopeUltra),
			current[aistudio.PoolScopeNormal], manager.maxActiveFor(aistudio.PoolScopeNormal),
		))
	}
	if change.proPool {
		manager.requests.log("service", "INFO", fmt.Sprintf(
			"Pro 独立号池设置已更新并立即生效 | 开启=%t | Pro 号池账户=%d | Pro 分区 Worker=%d/%d | Ultra 分区 Worker=%d/%d | 普通分区 Worker=%d/%d",
			proPool, members.Count(aistudio.PoolScopePro),
			current[aistudio.PoolScopePro], manager.maxActiveFor(aistudio.PoolScopePro),
			current[aistudio.PoolScopeUltra], manager.maxActiveFor(aistudio.PoolScopeUltra),
			current[aistudio.PoolScopeNormal], manager.maxActiveFor(aistudio.PoolScopeNormal),
		))
		if proPool {
			manager.requests.log("service", "WARN", "PRO_POOL=true 时 Pro 账户只服务 /pro 前缀，不再服务默认地址与 /ultra；Pro 账户上的文件与视频改从 /pro 引用")
		} else {
			manager.requests.log("service", "WARN", "PRO_POOL=false 时 /pro 前缀返回 404；Pro 账户回到普通号池（ULTRA_INCLUDE_PRO=true 时并入 Ultra 号池），Pro 账户上的文件与视频改从对应路径引用")
		}
	}
	for _, partition := range workerPartitions {
		if peak := manager.maxActiveFor(partition); current[partition] > peak {
			manager.requests.log("service", "WARN", fmt.Sprintf(
				"%s Worker 超过峰值 | 当前=%d | 峰值=%d | 空闲 Worker 将逐步回收，可调大对应峰值",
				partitionLabel(partition), current[partition], peak,
			))
		}
	}
	if includePro && exclusive && change.includePro {
		manager.requests.log("service", "WARN", "ULTRA_EXCLUSIVE=true 时 Pro 账户不再服务普通路径；Pro 账户上的文件与视频改从 /ultra 引用")
	}
}

// workerLogName 返回分区在运行日志里的 Worker 名称：普通分区沿用原有写法，Ultra 与 Pro 分区加前缀
func workerLogName(partition aistudio.PoolScope) string {
	return namesOf(partition).worker
}

// workerSlotName 返回分区在槽位说明里的名称
func workerSlotName(partition aistudio.PoolScope) string {
	return namesOf(partition).slot
}

// fillTargetFor 返回预热循环中分区要补齐到的常驻数：普通分区按 WARM_WORKER_LIMIT，Ultra 分区按 ULTRA_WARM_WORKER_LIMIT，
// Pro 分区按 PRO_WARM_WORKER_LIMIT。Ultra（Pro）分区没有可预热的账户时为 0，没有这类账户的部署不会为空分区反复分类候选
// （UltraAvailable 按号池成员计数，PRO_POOL=false、ULTRA_INCLUDE_PRO=true 时含 Pro，只有 Pro 账户的部署同样会预热 Ultra 分区；
// PRO_POOL=false 时 ProAvailable 为 0，Pro 分区不预热）。
// 可预热的账户都属于 Ultra 与 Pro 号池时普通分区同样为 0：只有付费账户（或普通账户都不可用）的部署按付费分区完成预热，
// 常驻数为 0 时直接完成首轮预热、Worker 按需启动；没有可预热账户时仍按原来的方式报错
func (manager *accountWorkerManager) fillTargetFor(partition aistudio.PoolScope, summary aistudio.BootstrapSummary) int {
	switch partition {
	case aistudio.PoolScopeUltra:
		if summary.UltraAvailable == 0 {
			return 0
		}
	case aistudio.PoolScopePro:
		if summary.ProAvailable == 0 {
			return 0
		}
	case aistudio.PoolScopeNormal:
		if paid := summary.UltraAvailable + summary.ProAvailable; paid > 0 && summary.Available == paid {
			return 0
		}
	default:
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
	members  aistudio.PartitionSet
	active   map[aistudio.PoolScope]int
	cooling  map[aistudio.PoolScope]bool
	idle     map[aistudio.PoolScope]bool
	// ranks 为已核对过的备用账户能服务几个主用模型与热门模型（见 preferred），一次调度内每个账户最多核对一次；
	// rankModels 为各分区排序用的主用与热门模型列表（见 coldRankModelsFor），一次调度内第一次需要时取一次
	ranks      map[string]coldRank
	rankModels map[aistudio.PoolScope]coldRankModels
}

// standbyCapacity 按当前驻留的 Worker 创建分区容量判断
func (manager *accountWorkerManager) standbyCapacity(active []string, modelID string) *standbyCapacity {
	return &standbyCapacity{manager: manager, modelID: modelID, resident: active}
}

// partition 返回账户所在的分区；第一次调用时读取分区成员并按分区统计占用的 Worker（驻留与正在启动，减去排空让出的）
func (capacity *standbyCapacity) partition(accountID string) aistudio.PoolScope {
	if capacity.members == nil {
		capacity.members = capacity.manager.pool.PartitionMembers()
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
			capacity.active[partitionOf(accountID, capacity.members)]++
		}
		for _, partition := range workerPartitions {
			capacity.active[partition] -= capacity.manager.drainCredit(partition, occupied)
		}
	}
	return partitionOf(accountID, capacity.members)
}

// preferred 返回按偏好排列的候选，分为两段：前 standbyPreferLimit 个与预热、替补同一口径（见 preferUncooled 与 compareColdRank）：
// 按能服务的分区主用模型个数从多到少，同数再按热门模型（不含主用模型）个数，仍相同时保持原顺序；第二段为其余候选，
// 保持原顺序且不复制（备用账户可能有上万个）。为其他模型的请求启动的备用账号也优先选还能服务主用模型的，
// 不直接启动一个服务不了主用模型的 Worker（目录里没有、权益不够或冷却剩余不少于 coldRankMinRemaining，按候选集成员判断，
// 见 aistudio.ServeableCounts）；原先按主用与热门合计个数、15 分钟阈值计分，对主用模型 403 冷却、能服务热门模型的账号
// 与能服务主用模型、在热门模型上冷却的账号同分，按账号 ID 先启动前者。只核对前 standbyPreferLimit 个（控制账户池锁内的工作量），
// 都服务不了时仍按原顺序使用。
// 主用模型按候选账户所在的分区取（Ultra 与 Pro 分区按 Ultra 主用模型，见 primaryModelListFor）：不限号池的选号候选跨多个分区，
// 前段按分区分组各核对一次（每个分区每份非空列表最多一次加锁，分区归属在调度内已缓存），不用各列表的并集，免得普通号池的主用模型
// 左右 Ultra 备用账号的先后
func (capacity *standbyCapacity) preferred(list []string) [2][]string {
	if len(list) <= 1 {
		return [2][]string{list, nil}
	}
	// 平均调用（见 balanced.go）：前段为今日发送最少的 standbyPreferLimit 个（同分保持原顺序）加上原列表的前 standbyPreferLimit 个
	// （去重，最多核对 2 × standbyPreferLimit 个），其余保持原顺序；之后照旧按能服务的主用、热门模型个数排前段，同档再按今日发送从少到多。
	// 原先的列表按 ID 从上次选中的账号往后轮转，上千个备用账号里用得最少的多半不在前 64 个里；只看最少的 64 个又不行：能服务主用模型的号
	// 还要接主用请求，今日发送天然更多，最少的 64 个往往全是服务不了主用模型的号，为其他模型扩容时启动的 Worker 接不了主用请求，
	// 而原列表的前段在混合号池里几乎总有能服务的号
	counts := capacity.manager.balancedUsageCounts()
	headSize := standbyPreferLimit
	if counts != nil {
		list, headSize = balancedStandbyOrder(list, counts, standbyPreferLimit)
	}
	if capacity.rankModels == nil {
		capacity.rankModels = make(map[aistudio.PoolScope]coldRankModels, len(workerPartitions))
		for _, partition := range workerPartitions {
			capacity.rankModels[partition] = capacity.manager.coldRankModelsFor(partition)
		}
	}
	models := capacity.rankModels
	if allRankModelsEmpty(models) {
		return [2][]string{list, nil}
	}
	head := list[:min(headSize, len(list))]
	unknown := make(map[aistudio.PoolScope][]string, len(workerPartitions))
	for _, accountID := range head {
		if _, checked := capacity.ranks[accountID]; !checked {
			partition := capacity.partition(accountID)
			unknown[partition] = append(unknown[partition], accountID)
		}
	}
	for _, partition := range workerPartitions {
		accountIDs := unknown[partition]
		if len(accountIDs) == 0 {
			continue
		}
		if capacity.ranks == nil {
			capacity.ranks = make(map[string]coldRank, len(head))
		}
		models[partition].rankInto(capacity.manager.pool, accountIDs, capacity.ranks)
	}
	ordered := slices.Clone(head)
	slices.SortStableFunc(ordered, func(left string, right string) int {
		if order := compareColdRank(capacity.ranks[left], capacity.ranks[right]); order != 0 || counts == nil {
			return order
		}
		return cmp.Compare(counts[left], counts[right])
	})
	return [2][]string{ordered, list[len(head):]}
}

// balancedStandbyOrder 为平均调用下请求路径挑备用账号的候选顺序（见 standbyCapacity.preferred）：返回重排后的副本与前段长度。
// 前段为今日发送最少的 limit 个（按发送数从少到多，同分保持原顺序）加上原列表前 limit 个里不在其中的（保持原顺序），其余保持原顺序；
// 不重复、不丢
func balancedStandbyOrder(list []string, counts map[string]uint32, limit int) ([]string, int) {
	if len(list) <= limit {
		return sortByUsage(list, counts), len(list)
	}
	least := leastUsedFirst(list, counts, limit)
	picked := make(map[string]struct{}, limit)
	for _, accountID := range least[:limit] {
		picked[accountID] = struct{}{}
	}
	result := make([]string, 0, len(list))
	result = append(result, least[:limit]...)
	for _, accountID := range list[:limit] {
		if _, ok := picked[accountID]; !ok {
			result = append(result, accountID)
			picked[accountID] = struct{}{}
		}
	}
	head := len(result)
	for _, accountID := range list[limit:] {
		if _, ok := picked[accountID]; !ok {
			result = append(result, accountID)
		}
	}
	return result, head
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
		// 扩容替换同样不换刚就绪不到 rotationWarmupGrace 的 Worker（N6，见 pickReplaceVictim）
		found = capacity.manager.pickReplaceVictim("", capacity.modelID, victimCooling, partition) != ""
		capacity.cooling[partition] = found
	}
	return found
}

// idleVictim 判断分区内是否有按需替换能淘汰的空闲 Worker：与 ensureWorker 的替换同一个函数（replaceVictimFor，N3），
// 原先这里看任意空闲 Worker、那边有 Worker 在启动时只换冷却中的，选了备用账号去提升却拿不到槽位，返回“Worker 容量已满”；
// 本分区每分钟的按需替换已达上限时同样为否
func (capacity *standbyCapacity) idleVictim(partition aistudio.PoolScope) bool {
	found, checked := capacity.idle[partition]
	if !checked {
		found = capacity.manager.replacementAllowed(context.Background(), partition) &&
			capacity.manager.replaceVictimFor("", capacity.modelID, partition) != ""
		capacity.idle[partition] = found
	}
	return found
}

// expandable 返回可以在后台扩容的备用账户：所在分区未满，或分区已满但有冷却中（剩余不少于 ShortCooldownMax）的空闲 Worker 可以替换；
// 优先没有 Worker 的账户，其次按能服务的主用模型个数、再按热门模型个数从多到少（见 preferred）
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
// allowVictim 时（没有热候选）分区已满但同分区有可淘汰的空闲 Worker 也可以。同样按能服务的主用模型个数、再按热门模型个数从多到少（见 preferred）
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

// allRankModelsEmpty 判断各分区排序用的主用与热门模型列表都为空
func allRankModelsEmpty(models map[aistudio.PoolScope]coldRankModels) bool {
	for _, partition := range workerPartitions {
		if !models[partition].empty() {
			return false
		}
	}
	return true
}

// partitionWorkerCounts 返回 Ultra 或 Pro 分区的 Worker 计数，供管理页状态显示；dead 为其中账号长时间冷却的死槽位数
func (manager *accountWorkerManager) partitionWorkerCounts(
	partition aistudio.PoolScope, members aistudio.PartitionSet, warm []string, starting []string, occupied int, dead int,
) api.AdminWorkerPartition {
	return api.AdminWorkerPartition{
		Warm: len(inPartition(warm, members, partition)), Starting: len(inPartition(starting, members, partition)),
		Occupied: occupied, Dead: dead, Target: manager.prewarmTargetFor(partition),
		WarmLimit: manager.warmTargetFor(partition), Max: manager.maxActiveFor(partition),
	}
}
