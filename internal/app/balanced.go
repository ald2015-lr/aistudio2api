package app

import (
	"cmp"
	"context"
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/api"
)

// 平均调用（ROUTING_STRATEGY=balanced，界面上叫“平均调用”）
//
// 目标：高并发时流量尽量平均落在每个账号上，所有账号在一天里差不多同时用到上限，便于成批补号。账户池内的选号与 round-robin 完全相同
// （aistudio.RoutingBalanced 存成 round-robin），这里在 app 层按“今日发送数”（每账号、每个太平洋时间额度日真正发往上游的生成请求数，
// 见 aistudio/quota_usage.go）调整四处选号，都只在开关打开时生效，关闭时与原来逐字节一致：
//  1. 驻留 Worker 之间空闲的优先（acquireLease）：不固定账户的选号先在没有进行中请求的热 Worker（WarmReady）里选，取不到再用全部热 Worker；
//     死槽位那一层同样先空闲、后全部。每一层内部仍按轮询。原先轮询只按账号 ID 轮转、不看进行中的请求数，会把新请求叠到已经有请求的号上
//  2. 预热与替补（rankColdCandidates）：去坏号、近期成功整组前移之后，先按今日发送数从少到多稳定排序，再按能服务的主用、热门模型个数排序。
//     最终顺序为 能服务的主用模型数↓、热门模型数↓、今日发送↑、近期成功优先、原顺序；balancedColdFailHold 内启动失败过的号整组排在最后
//  3. 请求路径的扩容与现场提升（standbyCapacity.preferred）：前段为今日发送最少的 standbyPreferLimit 个（部分选择，同分保持原顺序）
//     加原列表的前 standbyPreferLimit 个（见 balancedStandbyOrder），按能服务的主用、热门模型个数排序、同档今日发送少的先，其余保持原顺序
//  4. 淘汰同档时（pickIdleVictimWith）：今日发送多的先关，再比最久未用，排空中的号不选。空闲回收、按需替换、现场回收多出来的槽位
//     优先从用得多的号身上收回
//
// 另外按 balancedCheckInterval 在 keepWarm 里判断驻留 Worker 的轮换（用得多的驻留号先启后停地换成用得少的备用号，见 balanced_rotation.go）、
// 统计状态页概况（各分区可调度账号的今日发送数分布与轮换计数）、写 quota-usage.json，每 balancedSummaryInterval 写一条概况日志。
//
// 请求路径不为今日发送数对账户池加锁：读 1 秒缓存的副本（quotaUsage）。计数只统计本进程，多个进程共用运行目录时文件以最后写入为准

const (
	// balancedCheckInterval 为平均调用的检查节奏（状态页概况、写盘与轮换判断），挂在 keepWarm 上
	balancedCheckInterval = 30 * time.Second
	// balancedRotateGap 为轮换门槛（次发送）：驻留号的今日发送数比接替它的备用号多出这么多才换下。约等于 ACCOUNT_RPM_LIMIT 10 × 5 分钟，
	// 满速服务时恰好在最短服务时长到期时达到门槛
	balancedRotateGap = 50
	// balancedMinTenure 为驻留 Worker 就绪满这么久才可能被换下
	balancedMinTenure = 5 * time.Minute
	// balancedRotateCooloff 为当过被换下一方的账号在这段时间内不再被换下（固定账户与文件绑定的请求可能马上把它拉回来）
	balancedRotateCooloff = 30 * time.Minute
	// balancedRegistrationTTL 为轮换登记后一直没有开始（没开始排空、也没关闭）就放弃的时长
	balancedRegistrationTTL = 2 * time.Minute
	// balancedFailBackoff 为替补没有启动后该分区暂停轮换的时长：没有可挑的号、出口限流、软阶段超时等，或连续 balancedSpareFailStreak 个
	// 接替的号都因自己的原因起不来（多半是系统性故障）时。单个接替的号自己起不来（浏览器启动失败等）时不暂停，下一次判断换下一个备用号
	balancedFailBackoff = 5 * time.Minute
	// balancedSpareFailStreak 为分区连续多少次轮换的接替号都因自己的原因没有启动时才暂停 balancedFailBackoff（替补启动成功时清零）
	balancedSpareFailStreak = 3
	// balancedColdFailHold 为启动失败过（之后没有成功）的号排在冷启动候选最后的时长（见 rankColdCandidates）：预热失败记录只挡 5 分钟，
	// 今日发送为 0、每次都起不来的号到期后又排第一，接替、预热、死槽位替补一直先挑它
	balancedColdFailHold = 2 * time.Hour
	// balancedUsageCacheTTL 为今日发送数副本的缓存时长（请求路径只读缓存）
	balancedUsageCacheTTL = time.Second
	// balancedSummaryInterval 为“平均调用概况”日志的间隔
	balancedSummaryInterval = 10 * time.Minute
	// balancedPauseLogInterval 为同一分区同一暂停原因的“平均调用暂停”日志的最短间隔
	balancedPauseLogInterval = 5 * time.Minute
)

// balancedSoftTTL 返回轮换先启后停时“替补还没就绪”阶段的上限：一次冷启动的初始化超时加预热任务余量，再多留 30 秒
func (manager *accountWorkerManager) balancedSoftTTL() time.Duration {
	return time.Duration(manager.initTimeout.Load()) + warmTaskGrace + 30*time.Second
}

// balancedState 为平均调用在 Worker 管理器上的状态（开关本身是 accountWorkerManager.balanced）
type balancedState struct {
	// usage 为今日发送数的缓存副本，usageAt 为取得时间（见 quotaUsage）；由 usageMu 保护，副本取得后只读
	usageMu sync.Mutex
	usage   aistudio.QuotaUsageSnapshot
	usageAt time.Time
	// checkAt 为上次检查的时间（UnixNano，0 为开启后还没检查过），summaryAt 为上次写概况日志的时间；都只由 keepWarm 一个 goroutine 读写，
	// 开关切换时清零（原子量）
	checkAt   atomic.Int64
	summaryAt atomic.Int64
	// status 为状态页概况的缓存（见 balancedStatus），关闭时为 nil
	status atomic.Pointer[api.AdminBalanced]
	// store 为今日发送数的写盘记录（见 quota_usage_store.go）；只在 newRuntime 挂上，测试构造的管理器为空（不写盘）
	store atomic.Pointer[quotaUsageStore]
	// rotation 为驻留 Worker 轮换的记录（各分区上次开始的时间、替补失败后的退避、被换下的号的冷却期、暂停原因与完成数，见 balanced_rotation.go）；
	// 开关切换时不清空：重新打开后冷却期与退避照样有效
	rotation rotationBook
}

// setBalanced 切换平均调用开关，返回是否发生变化。启动时（newRuntime）与热更新（applyLiveSettings）调用；
// 打开时下一次 keepWarm 立即统计概况，关闭时清掉状态页概况
func (manager *accountWorkerManager) setBalanced(on bool) bool {
	if manager == nil || manager.balanced.Swap(on) == on {
		return false
	}
	manager.balancedState.checkAt.Store(0)
	manager.balancedState.summaryAt.Store(0)
	if !on {
		manager.balancedState.status.Store(nil)
	}
	return true
}

// balancedOn 返回平均调用是否开启（manager 为 nil 时为否）
func (manager *accountWorkerManager) balancedOn() bool {
	return manager != nil && manager.balanced.Load()
}

// quotaUsage 返回今日发送数的副本，最多 balancedUsageCacheTTL 取一次（一次加锁复制，1300 个账号约几十微秒）；
// 副本的 Counts 由多个调用方共用，只读
func (manager *accountWorkerManager) quotaUsage(now time.Time) aistudio.QuotaUsageSnapshot {
	state := &manager.balancedState
	state.usageMu.Lock()
	defer state.usageMu.Unlock()
	if state.usage.Counts == nil || now.Sub(state.usageAt) >= balancedUsageCacheTTL || now.Before(state.usageAt) {
		state.usage = manager.pool.QuotaDayUsage()
		state.usageAt = now
	}
	return state.usage
}

// balancedUsageCounts 返回平均调用开启时的今日发送数（缓存副本），未开启时为 nil：调用方按 nil 走原来的顺序
func (manager *accountWorkerManager) balancedUsageCounts() map[string]uint32 {
	if !manager.balancedOn() {
		return nil
	}
	return manager.quotaUsage(time.Now()).Counts
}

// sortByUsage 返回按今日发送数从少到多稳定排序的副本（同分保持原顺序）
func sortByUsage(accountIDs []string, counts map[string]uint32) []string {
	result := slices.Clone(accountIDs)
	slices.SortStableFunc(result, func(left string, right string) int {
		return cmp.Compare(counts[left], counts[right])
	})
	return result
}

// leastUsedFirst 返回把今日发送最少的 limit 个（按发送数从少到多，同分保持原顺序）放在最前、其余保持原顺序的副本：
// 部分选择，O(n log limit)，备用账号上千个时不做全排序
func leastUsedFirst(accountIDs []string, counts map[string]uint32, limit int) []string {
	if len(accountIDs) <= limit {
		return sortByUsage(accountIDs, counts)
	}
	// top 为目前最少的 limit 个下标，按（发送数，下标）升序；新元素比最后一个小才插入
	top := make([]int, 0, limit)
	less := func(left int, right int) bool {
		leftCount, rightCount := counts[accountIDs[left]], counts[accountIDs[right]]
		return leftCount < rightCount || leftCount == rightCount && left < right
	}
	for index := range accountIDs {
		if len(top) == limit && !less(index, top[limit-1]) {
			continue
		}
		at := sort.Search(len(top), func(i int) bool { return less(index, top[i]) })
		if len(top) < limit {
			top = append(top, 0)
		}
		copy(top[at+1:], top[at:len(top)-1])
		top[at] = index
	}
	picked := make([]bool, len(accountIDs))
	result := make([]string, 0, len(accountIDs))
	for _, index := range top {
		picked[index] = true
		result = append(result, accountIDs[index])
	}
	for index, accountID := range accountIDs {
		if !picked[index] {
			result = append(result, accountID)
		}
	}
	return result
}

// victimTieBefore 判断淘汰选号同档时 candidate 是否排在已选的 selected 前面：平均调用开启（counts 非 nil）时今日发送多的先关，
// 同数再比最久未用；未开启时只比最久未用（与原来一致）
func victimTieBefore(
	counts map[string]uint32, candidate string, candidateState aistudio.WarmWorkerState, selected string, selectedState aistudio.WarmWorkerState,
) bool {
	if counts != nil {
		if left, right := counts[candidate], counts[selected]; left != right {
			return left > right
		}
	}
	return candidateState.LastUsed.Before(selectedState.LastUsed)
}

// routingStrategySummary 为热更新日志里的账户选择策略：平均调用写成 balanced（平均调用），其余原样
func routingStrategySummary(strategy string) string {
	if strategy == aistudio.RoutingBalanced {
		return strategy + "（平均调用）"
	}
	return strategy
}

// balancedRotationStats 为状态页与概况日志里轮换的部分：各分区进行中的轮换数（轮换登记数，还没开始的、软排空的与普通排空的都算）
// 与最近一次判断的暂停原因、近 1 小时与本额度日完成（旧号已关闭）的轮换数
type balancedRotationStats struct {
	rotating map[aistudio.PoolScope]int
	paused   map[aistudio.PoolScope]string
	lastHour int
	today    int
}

// balancedRotationStats 返回轮换的计数（见 balancedRotationStats 类型的说明）：登记数在 retireMu 内数，其余读轮换记录
func (manager *accountWorkerManager) balancedRotationStats(now time.Time) balancedRotationStats {
	stats := balancedRotationStats{rotating: make(map[aistudio.PoolScope]int, len(workerPartitions))}
	members := manager.slotMembers()
	manager.retireMu.Lock()
	for accountID, entry := range manager.retirements {
		if entry.rotation {
			stats.rotating[partitionOf(accountID, members)]++
		}
	}
	manager.retireMu.Unlock()
	stats.paused, stats.lastHour, stats.today = manager.balancedState.rotation.stats(now)
	return stats
}

// balancedPass 为 keepWarm 里平均调用的一轮（放在冷却轮换之后、清扫失败 Worker 之前），自带 balancedCheckInterval 的节奏：
// 版本有变化时写 quota-usage.json、判断驻留 Worker 的轮换（balancedRotate）、统计状态页概况、每 balancedSummaryInterval 写一条概况日志。
// 未开启时什么也不做
func (service *trackedService) balancedPass(ctx context.Context, now time.Time) {
	workers := service.workers
	if !workers.balancedOn() {
		return
	}
	state := &workers.balancedState
	if last := state.checkAt.Load(); last != 0 && now.Sub(time.Unix(0, last)) < balancedCheckInterval && !now.Before(time.Unix(0, last)) {
		return
	}
	state.checkAt.Store(now.UnixNano())
	// 写盘与概况用这一刻的计数，不用请求路径的缓存副本
	snapshot := service.pool.QuotaDayUsage()
	if store := state.store.Load(); store != nil {
		if err := store.save(snapshot); err != nil {
			service.requests.log("service", "WARN", "今日发送数写入失败 | 错误="+err.Error())
		}
	}
	service.balancedRotate(ctx, now)
	status := service.balancedOverview(snapshot, workers.balancedRotationStats(now))
	// 统计期间被关掉时不写回概况（setBalanced(false) 已清掉）
	if workers.balancedOn() {
		state.status.Store(status)
	}
	if last := state.summaryAt.Load(); last == 0 || now.Sub(time.Unix(0, last)) >= balancedSummaryInterval || now.Before(time.Unix(0, last)) {
		state.summaryAt.Store(now.UnixNano())
		service.requests.log("service", "INFO", balancedSummaryLine(status))
	}
}

// balancedOverview 统计各分区可调度账号（号池内启用、且不是不可调度的：需要验证账号的整户暂停 30 天、需要登录等不算，否则“最少”一直是 0）
// 的今日发送数分布，与轮换计数一起组成状态页概况。每个分区对账户池加锁两次
func (service *trackedService) balancedOverview(snapshot aistudio.QuotaUsageSnapshot, rotation balancedRotationStats) *api.AdminBalanced {
	status := &api.AdminBalanced{
		QuotaDay: snapshot.Day, Gap: balancedRotateGap, Rotations1h: rotation.lastHour, RotationsToday: rotation.today,
	}
	for _, partition := range workerPartitions {
		accountIDs, _ := service.pool.EnabledAccountsIn(partition)
		unschedulable := service.pool.UnschedulableSet(accountIDs)
		counts := make([]uint32, 0, len(accountIDs))
		for _, accountID := range accountIDs {
			if _, skip := unschedulable[accountID]; !skip {
				counts = append(counts, snapshot.Counts[accountID])
			}
		}
		spread := usageSpread(counts)
		spread.Rotating = rotation.rotating[partition]
		spread.PausedReason = rotation.paused[partition]
		switch partition {
		case aistudio.PoolScopeNormal:
			status.Normal = spread
		case aistudio.PoolScopeUltra:
			status.Ultra = spread
		case aistudio.PoolScopePro:
			// 只有 Pro 号池有可调度账号时列出（PRO_POOL=false 时 Pro 号池为空，状态与概况日志与原先相同）
			if spread.Accounts > 0 {
				status.Pro = &spread
			}
		}
	}
	return status
}

// usageSpread 返回一组今日发送数的分布：账号数、发送过的账号数、最少、中位、p90 与最多（就近取秩，中位取下中位）
func usageSpread(counts []uint32) api.AdminBalancedPartition {
	spread := api.AdminBalancedPartition{Accounts: len(counts)}
	if len(counts) == 0 {
		return spread
	}
	sorted := slices.Clone(counts)
	slices.Sort(sorted)
	rank := func(fraction float64) int {
		index := int(math.Ceil(fraction*float64(len(sorted)))) - 1
		return int(sorted[min(max(index, 0), len(sorted)-1)])
	}
	for _, count := range sorted {
		if count > 0 {
			spread.Used++
		}
	}
	spread.Min, spread.Median, spread.P90, spread.Max = int(sorted[0]), rank(0.5), rank(0.9), int(sorted[len(sorted)-1])
	return spread
}

// balancedSummaryLine 为“平均调用概况”日志（不列账户）
func balancedSummaryLine(status *api.AdminBalanced) string {
	part := func(name string, spread api.AdminBalancedPartition) string {
		line := fmt.Sprintf("%s 可调度=%d 今日用过=%d 最少/中位/p90/最多=%d/%d/%d/%d",
			name, spread.Accounts, spread.Used, spread.Min, spread.Median, spread.P90, spread.Max)
		if spread.Rotating > 0 {
			line += fmt.Sprintf(" 轮换中=%d", spread.Rotating)
		}
		if spread.PausedReason != "" {
			line += " 暂停=" + spread.PausedReason
		}
		return line
	}
	parts := []string{"平均调用概况", "额度日=" + status.QuotaDay, part("普通", status.Normal), part("Ultra", status.Ultra)}
	if status.Pro != nil {
		parts = append(parts, part("Pro", *status.Pro))
	}
	parts = append(parts, fmt.Sprintf("近 1 小时轮换=%d", status.Rotations1h))
	return strings.Join(parts, " | ")
}

// balancedStatus 返回状态页的平均调用概况：开启、服务在运行且已统计过时才有；只读缓存，不对账户池加锁
func (manager *accountWorkerManager) balancedStatus(running bool) *api.AdminBalanced {
	if !running || !manager.balancedOn() {
		return nil
	}
	return manager.balancedState.status.Load()
}
