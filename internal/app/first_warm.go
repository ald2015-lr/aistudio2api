package app

import (
	"sync/atomic"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// 首轮预热（移植自上游 c77dd89，按 Worker 分区改写）：生成服务启动只做必需的步骤就进入运行，首轮预热在后台进行，
// 预热失败只写 WARN、服务保持运行，之后由 keepWarm 每个周期补齐（见 trackedService.Start、warmAfterLaunch）。
// 约束：
//   - 每个 Worker 分区（普通、Ultra、Pro）分别记“首轮预热还没出结果”：Start 开始首轮预热前把全部分区置位（startFirstPrewarm）；
//   - fillWarm 在该分区第一个 Worker 就绪时清除；该分区这一轮里已经没有进行中的启动（达到常驻数、没有可启动的账户、
//     分类失败、槽位已满、冷启动被出口闸门暂停、冷启动名额被排在前面的分区占满）时同样清除；首轮结束时全部清除。
//     清除时唤醒排队的请求（notifyScheduling）；
//   - 不固定账户的选号在请求号池里没有已就绪的 Worker、要冷启动的备用账号有落在首轮预热还没出结果的分区时，
//     等它的第一个 Worker（acquireLease 的 waitReasonFirstWarm），不另起冷启动；备用账号都在已出结果的分区时不等（firstWarmBlocks）。
//     每一轮选号先读首轮状态、再取已就绪 Worker 的快照（firstWarmSnapshotNow），不会看到两者从未同时成立的组合；
//     只有首轮置位，keepWarm 之后的补齐不置位

// firstWarmState 为各分区的首轮预热状态，按号池下标；非分区下标不用
type firstWarmState struct {
	pending [aistudio.PoolScopeCount]atomic.Bool
}

// beginFirstWarm 把全部分区标为首轮预热还没出结果
func (manager *accountWorkerManager) beginFirstWarm() {
	for _, partition := range workerPartitions {
		manager.firstWarm.pending[partition].Store(true)
	}
}

// settleFirstWarm 记分区的首轮预热已出结果并唤醒排队的请求；已经清除时什么也不做
func (manager *accountWorkerManager) settleFirstWarm(partition aistudio.PoolScope) {
	if !partition.IsPartition() || !manager.firstWarm.pending[partition].CompareAndSwap(true, false) {
		return
	}
	manager.notifyScheduling()
}

// settleAllFirstWarm 记全部分区的首轮预热已出结果（首轮结束或没能开始）
func (manager *accountWorkerManager) settleAllFirstWarm() {
	for _, partition := range workerPartitions {
		manager.settleFirstWarm(partition)
	}
}

// firstWarmPendingIn 返回号池 scope 包含的分区里首轮预热还没出结果的分区
func (manager *accountWorkerManager) firstWarmPendingIn(scope aistudio.PoolScope) []aistudio.PoolScope {
	var pending []aistudio.PoolScope
	for _, partition := range workerPartitions {
		if scope.Contains(partition) && manager.firstWarm.pending[partition].Load() {
			pending = append(pending, partition)
		}
	}
	return pending
}

// firstWarmSnapshot 为某一时刻各分区的首轮预热状态，按号池下标，为真表示还没出结果；非分区下标恒为假
type firstWarmSnapshot [aistudio.PoolScopeCount]bool

// firstWarmSnapshotNow 读各分区此刻的首轮预热状态。选号每一轮先读它、再取已就绪 Worker 的快照（见 acquireLease）：
// 分区因第一个 Worker 就绪而清除首轮状态，发生在这个 Worker 发布（activateAccountWorker）之后，所以先读到“已出结果”时，
// 随后取的快照里有这个 Worker，除非它在这之间已被关闭或不再处于 Ready/Busy（那时它本就用不上，照常选号、现场冷启动是对的）。顺序反过来时，两次读取之间 Worker 就绪并清除首轮状态，这一轮看到的是“号池里没有就绪的 Worker、
// 首轮也已出结果”，两者从未同时成立：请求不再等，现场冷启动另一个备用账号（刚就绪的账户已是热 Worker，提升时排在冷账户之后），
// 例如首轮里刚启动失败过的账户，而不是用刚就绪的 Worker。读到“还没出结果”而 Worker 其实已就绪时只是多一次等待复查
// （Worker 就绪与清除首轮状态都发调度信号，排队的请求随即复查）
func (manager *accountWorkerManager) firstWarmSnapshotNow() firstWarmSnapshot {
	var snapshot firstWarmSnapshot
	for _, partition := range workerPartitions {
		snapshot[partition] = manager.firstWarm.pending[partition].Load()
	}
	return snapshot
}

// anyIn 判断号池 scope 包含的分区里有首轮预热还没出结果的
func (snapshot firstWarmSnapshot) anyIn(scope aistudio.PoolScope) bool {
	for _, partition := range workerPartitions {
		if scope.Contains(partition) && snapshot[partition] {
			return true
		}
	}
	return false
}

// firstWarmBlocks 判断号池 scope 的选号是否要等首轮预热：standby（这次要冷启动的备用账号，已去掉坏号、受限出口上与正在启动的账户）
// 里有账户落在首轮预热还没出结果的分区，且 warm（已就绪的 Worker）里没有属于 scope 的。号池跨分区（ULTRA_EXCLUSIVE=false 的普通路径）
// 时只看备用账号所在的分区：备用账号都在已出结果的分区（例如只有 Ultra 账户能服务的模型、Ultra 分区已出结果，普通分区还在首轮）时不等，
// 照常现场冷启动；有备用账号落在待定分区时仍等，不在那个分区另起冷启动。只在号池里有分区待定时才读分区成员。
// pending 为取 warm 之前读的首轮状态（firstWarmSnapshotNow），不在这里现读：现读的状态可能比 warm 新（见 firstWarmSnapshotNow）
func (manager *accountWorkerManager) firstWarmBlocks(scope aistudio.PoolScope, pending firstWarmSnapshot, warm, standby []string) bool {
	if !pending.anyIn(scope) {
		return false
	}
	members := manager.pool.PartitionMembers()
	for _, accountID := range warm {
		if scope.Contains(partitionOf(accountID, members)) {
			return false
		}
	}
	for _, accountID := range standby {
		if partition := partitionOf(accountID, members); scope.Contains(partition) && pending[partition] {
			return true
		}
	}
	return false
}

// firstWarmPendingNames 返回首轮预热还没出结果的分区名（normal、ultra、pro），状态接口显示；都已出结果时为空
func (manager *accountWorkerManager) firstWarmPendingNames() []string {
	var names []string
	for _, partition := range manager.firstWarmPendingIn(aistudio.PoolScopeAll) {
		names = append(names, partition.String())
	}
	return names
}
