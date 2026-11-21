package app

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// 平均调用的驻留 Worker 轮换（ROUTING_STRATEGY=balanced，见 balanced.go）
//
// 只改选号顺序摊不到上千个号上：能接请求的只有在跑的那几个到几十个 Worker，必须让在跑的集合按今日发送数轮换。
// 判断（balancedRotate，挂在 keepWarm 的 balancedPass 里，每 30 秒一次）与执行（retirePass 的 rotation 分支）分开：
//   - 判断：每个分区依次核对暂停条件，用替补的真实挑法（pickReplacement 干跑，不预留任何东西）找出接替的号 S，在能服务的主用、热门模型
//     不比 S 多的驻留 Worker 里挑今日发送最多的（pickRotation，纯函数），差距不小于 balancedRotateGap 才写一条轮换登记
//     （registerRotation，与冷却登记同一张表）
//   - 执行：retirePass 轮询登记。先启后停（rotationSoft）时先写软排空（账号照常接单，让出额度照算），后台启动 S；S 就绪
//     （或让出的槽位被请求路径的扩容占去）后改为普通排空，旧号手上的请求跑完才关闭。没有多余排空名额时只换空闲的号：直接关闭，
//     由替补或预热补上
//
// 不打断任何请求：关闭要取独占租约，独占要求账号上没有进行中的请求；指定账户与文件绑定的请求照常使用排空中的号，
// 一直有这种请求时按排空超时放弃，30 分钟内不再换下它。浏览器总数不超过 峰值 + drainLimit（软排空同样计入让出额度与排空名额）；
// 轮换不走按需替换（replaceVictimFor），每分区每分钟 10 次的按需替换上限不受影响；S 走后台启动，冷启动名额先让给请求现场的启动

const (
	// 暂停原因：写进状态页（paused_reason）与“平均调用暂停”日志
	rotationPauseEgress   = "出口限流中"
	rotationPauseRetiring = "有冷却占位或不可调度的 Worker 等着淘汰"
	rotationPauseBackoff  = "替补没有启动，暂停 5 分钟"
	rotationPauseNoSpare  = "分区内没有可接替的备用号"
	rotationPauseNoRoom   = "没有多余的排空名额，在跑的号都在忙"
	// rotationPauseWeakerSpare 为合格的驻留号能服务的分区主用或热门模型都比接替的号多（见 pickRotation 第 4 步）
	rotationPauseWeakerSpare = "备用号能服务的主用或热门模型比在跑的号少"
	// 不轮换但不算暂停的原因（不写日志、不上状态页），测试按它核对判断走到了哪一步
	rotationSkipOff         = "未开启平均调用"
	rotationSkipStopped     = "生成服务没有运行"
	rotationSkipInProgress  = "已有进行中的轮换"
	rotationSkipRate        = "未到下一次轮换的间隔"
	rotationSkipNoCandidate = "没有可换下的驻留号"
	rotationSkipGap         = "差距未到门槛"
	// 取消原因：写进“平均调用轮换取消”日志（WARN）
	rotationCancelOff    = "已关闭平均调用"
	rotationCancelFailed = "替补没有启动"
	// rotationCancelSpareFailed 为接替的号因自己的原因没有启动（浏览器启动失败、WAA runtime 由其他进程占用）：分区不暂停，下一次判断换下一个备用号
	rotationCancelSpareFailed = "接替的号没有启动起来，下一次换其他备用号"
	rotationCancelTimeout     = "排空超时，30 分钟内不再换下本号"
	rotationCancelStale       = "迟迟没有开始"
	rotationCancelTakenOver   = "冷却登记接管"
	// rotationReplacementReason 为轮换的替补启动、补位日志里的原因
	rotationReplacementReason = "平均调用轮换"
)

// rotationCandidate 为一个驻留 Worker 在轮换判断里的状态（balancedRotate 一次收集）
type rotationCandidate struct {
	accountID string
	// usage 为今日发送数
	usage uint32
	// readyAt 为 Worker 就绪的时间（零值为不知道，不作为候选），lastUsed 为账号最近活动时间
	readyAt  time.Time
	lastUsed time.Time
	// busy 为账号上有进行中的请求或独占操作
	busy bool
	// starting 为正在启动或替换 Worker；reserved 为已被其他淘汰路径选中；registered 为已在排空或有任何淘汰登记；failed 为 Worker 已失败
	// （或账户锁被占着，这一轮看不清）
	starting, reserved, registered, failed bool
	// unschedulable、dead、cooling 为账号不可调度、全局长时间冷却、全局冷却中（交给原来的释放与淘汰路径）
	unschedulable, dead, cooling bool
	// cooloffUntil 为当过被换下一方之后不再被换下的截止时间（零值为没有）
	cooloffUntil time.Time
	// rank 为能服务的分区主用模型与热门模型个数（与预热、替补排序同一口径，见 coldRankModels.rankInto）
	rank coldRank
}

// rotationSpare 为接替的号（替补挑法干跑选出的）：账户 ID（没有时为空）、今日发送数与能服务的主用、热门模型个数
type rotationSpare struct {
	accountID string
	usage     uint32
	rank      coldRank
}

// rotationInput 为一个分区一轮轮换判断的全部输入
type rotationInput struct {
	now time.Time
	// enabled、running、egressLimited 为全局条件：平均调用开启、生成服务在运行、有出口限流判定成立
	enabled, running, egressLimited bool
	// pendingOther 为分区内有马上要处理的非轮换登记（冷却或不可调度，见 retirementBlocksRotation）：死槽位优先
	pendingOther bool
	// inProgress 为分区内有还没开始或处于软排空阶段的轮换：同时最多一个
	inProgress bool
	// lastStart 为分区上次登记轮换的时间，rate 为每分钟最多开始的轮换数（见 balancedRotateRate）；backoffUntil 为替补没有启动后暂停到的时间
	lastStart    time.Time
	rate         int
	backoffUntil time.Time
	// drainSlots 为分区占着排空名额的排空数，drainLimit 为名额上限（也是让出额度的上限）；drainTotal 为分区全部排空数（含超过
	// drainSlotTimeout、不再占名额的），drainTotalLimit 为它的上限。drainSlots+1 < drainLimit 且 drainTotal+1 < drainTotalLimit 时才能
	// 先启后停（两样都给死槽位留一个），另外要求 drainTotal < drainLimit（让出额度没有封顶，软排空确实能多让出一个槽位给接替的号）
	drainSlots, drainLimit      int
	drainTotal, drainTotalLimit int
	// resident 为分区驻留的 Worker 数，warmTarget 为常驻数：超出常驻数、空闲超过 workerIdleTimeout 的号交给空闲回收
	resident, warmTarget int
	candidates           []rotationCandidate
	// spare 返回接替的号（替补挑法干跑，没有时账户 ID 为空）；只在有合格的被换下一方时调用一次
	spare func() rotationSpare
}

// rotationDecision 为一轮轮换判断的结果：accountID 非空时登记轮换（soft 为先启后停，否则空闲直接换）；
// 为空时 reason 为不轮换的原因，pause 为真时它算暂停（写状态页与日志）
type rotationDecision struct {
	accountID string
	soft      bool
	readyAt   time.Time
	// usage、target 为被换下的号与接替的号（spare）的今日发送数
	usage  uint32
	spare  string
	target uint32
	reason string
	pause  bool
}

// balancedRotateRate 返回分区每分钟最多开始的轮换数：max(1, min(WARM_STARTUP_CONCURRENCY, 峰值/10))。默认配置（启动并发 2、峰值 10 与 5）
// 两个分区都是 1 次，峰值调到 50 时 2 次：峰值大时驻留号多，固定每分钟 1 次会让每个号要服务很久才轮得到
func balancedRotateRate(maxActive int, warmConcurrency int) int {
	return max(1, min(warmConcurrency, maxActive/10))
}

// balancedRotateInterval 返回同一分区两次开始轮换之间的最短间隔：1 分钟 / rate
func balancedRotateInterval(rate int) time.Duration {
	return time.Minute / time.Duration(max(rate, 1))
}

// rotationEligible 判断驻留 Worker 能否作为被换下的一方（不看忙闲与路径）：不在启动、没被其他路径选中、没有排空或登记、Worker 没失败；
// 就绪满 balancedMinTenure；不在 30 分钟冷却期内；可调度、没有全局长时间冷却或冷却中（交给原来的路径）；分区超出常驻数时
// 空闲超过 workerIdleTimeout 的号交给空闲回收
func rotationEligible(input rotationInput, candidate rotationCandidate) bool {
	switch {
	case candidate.starting, candidate.reserved, candidate.registered, candidate.failed:
		return false
	case candidate.unschedulable, candidate.dead, candidate.cooling:
		return false
	case candidate.readyAt.IsZero() || input.now.Sub(candidate.readyAt) < balancedMinTenure:
		return false
	case input.now.Before(candidate.cooloffUntil):
		return false
	case !candidate.busy && input.now.Sub(candidate.lastUsed) >= workerIdleTimeout && input.resident > input.warmTarget:
		return false
	}
	return true
}

// rotationBefore 判断 left 是否比 right 更该被换下：今日发送多的先；同数时空闲的先（关得快）；再比就绪早的；最后按账户 ID（结果确定）
func rotationBefore(left rotationCandidate, right rotationCandidate) bool {
	if left.usage != right.usage {
		return left.usage > right.usage
	}
	if left.busy != right.busy {
		return !left.busy
	}
	if !left.readyAt.Equal(right.readyAt) {
		return left.readyAt.Before(right.readyAt)
	}
	return left.accountID < right.accountID
}

// pickRotation 为一个分区的轮换判断（纯函数，外层 balancedRotate 只负责收集输入）。按顺序：
//  1. 全局条件：平均调用开启、生成服务在运行、没有出口限流（限流时冷启动闸门会拒掉接替的号，换下的号反而让容量变少）；
//  2. 分区条件：没有马上要处理的非轮换登记（死槽位优先）、没有还没开始或处于软阶段的轮换、不在替补失败后的退避中、距上次开始已满 1 分钟 / R；
//  3. 合格的被换下一方（rotationEligible）：占着排空名额的排空数加一仍低于 drainLimit、排空总数加一仍低于 drainTotalLimit、
//     并且排空总数低于 drainLimit 时先启后停，忙闲都行；否则只看空闲的号、直接关闭（默认的 Ultra 分区与峰值小于 8 的普通分区，
//     以及轮换的排空一直有请求、排空总数快到上限时：要给死槽位留一个排空名额与一个排空总数；排空总数已到 drainLimit 时让出额度已封顶，
//     软排空不会多让出槽位，峰值已满时接替的号必然因槽位已满起不来，旧号却照样被改为普通排空，分区白白少一个接单的 Worker）；
//  4. 接替的号 S：spare（替补挑法干跑）；没有时暂停（小号池自然不轮换）。被换下的号 W 只在能服务的分区主用、热门模型不比 S 多的号里挑
//     （与预热、替补排序同一口径：主用模型个数先比，同数再比热门模型）：W 更多时，W 关掉后补位的预热或替补按这个排序会先挑回 W 本身，
//     白关一次、白冷启动一次；先启后停时分区也会少一个能服务这些模型的 Worker。在不比 S 多的号里按 rotationBefore 取第一个，
//     合格的号都比 S 多时暂停；
//  5. 差距：usage[W] − usage[S] 不小于 balancedRotateGap 才轮换
func pickRotation(input rotationInput) rotationDecision {
	skip := func(reason string) rotationDecision { return rotationDecision{reason: reason} }
	pause := func(reason string) rotationDecision { return rotationDecision{reason: reason, pause: true} }
	switch {
	case !input.enabled:
		return skip(rotationSkipOff)
	case !input.running:
		return skip(rotationSkipStopped)
	case input.egressLimited:
		return pause(rotationPauseEgress)
	case input.pendingOther:
		return pause(rotationPauseRetiring)
	case input.inProgress:
		return skip(rotationSkipInProgress)
	case input.now.Before(input.backoffUntil):
		return pause(rotationPauseBackoff)
	}
	// 墙钟回拨（now 早于上次开始）时不按间隔挡住
	if elapsed := input.now.Sub(input.lastStart); !input.lastStart.IsZero() && elapsed >= 0 && elapsed < balancedRotateInterval(input.rate) {
		return skip(rotationSkipRate)
	}
	soft := input.drainSlots+1 < input.drainLimit && input.drainTotal+1 < input.drainTotalLimit && input.drainTotal < input.drainLimit
	eligible, busySkipped := make([]int, 0, len(input.candidates)), false
	for index, candidate := range input.candidates {
		if !rotationEligible(input, candidate) {
			continue
		}
		if !soft && candidate.busy {
			busySkipped = true
			continue
		}
		eligible = append(eligible, index)
	}
	if len(eligible) == 0 {
		if busySkipped {
			return pause(rotationPauseNoRoom)
		}
		return skip(rotationSkipNoCandidate)
	}
	var spare rotationSpare
	if input.spare != nil {
		spare = input.spare()
	}
	if spare.accountID == "" {
		return pause(rotationPauseNoSpare)
	}
	best := -1
	for _, index := range eligible {
		candidate := input.candidates[index]
		if compareColdRank(candidate.rank, spare.rank) < 0 {
			continue
		}
		if best < 0 || rotationBefore(candidate, input.candidates[best]) {
			best = index
		}
	}
	if best < 0 {
		return pause(rotationPauseWeakerSpare)
	}
	victim := input.candidates[best]
	if victim.usage < spare.usage || victim.usage-spare.usage < balancedRotateGap {
		return skip(rotationSkipGap)
	}
	return rotationDecision{
		accountID: victim.accountID, soft: soft, readyAt: victim.readyAt, usage: victim.usage, spare: spare.accountID, target: spare.usage,
	}
}

// rotationBook 为轮换的记录，由 mu 保护（叶子锁：持有时不取其他锁）
type rotationBook struct {
	mu sync.Mutex
	// startedAt 为各分区上次登记轮换的时间；backoff 为替补没有启动后该分区暂停到的时间；spareFails 为各分区连续因接替的号自己的原因
	// 没有启动的轮换数（见 noteSpareFailure）
	startedAt  map[aistudio.PoolScope]time.Time
	backoff    map[aistudio.PoolScope]time.Time
	spareFails map[aistudio.PoolScope]int
	// cooloff 为当过被换下一方的号（已关闭，或排空超时放弃）不再被换下的截止时间
	cooloff map[string]time.Time
	// paused 为各分区最近一次判断的暂停原因（不暂停时没有），pauseLogAt 为“平均调用暂停”日志的限频（键为 分区|原因）
	paused     map[aistudio.PoolScope]string
	pauseLogAt map[string]time.Time
	// done 为近 1 小时完成（旧号关闭）的轮换时间；doneDay、doneToday 为本额度日的编号与完成数
	done      []time.Time
	doneDay   int32
	doneToday int
}

// rotationBookSnapshot 为一轮判断用到的记录副本
type rotationBookSnapshot struct {
	startedAt map[aistudio.PoolScope]time.Time
	backoff   map[aistudio.PoolScope]time.Time
	cooloff   map[string]time.Time
}

// snapshot 返回判断用的副本，并清掉已过期的冷却期
func (book *rotationBook) snapshot(now time.Time) rotationBookSnapshot {
	book.mu.Lock()
	defer book.mu.Unlock()
	result := rotationBookSnapshot{
		startedAt: make(map[aistudio.PoolScope]time.Time, len(book.startedAt)),
		backoff:   make(map[aistudio.PoolScope]time.Time, len(book.backoff)),
		cooloff:   make(map[string]time.Time, len(book.cooloff)),
	}
	for partition, at := range book.startedAt {
		result.startedAt[partition] = at
	}
	for partition, until := range book.backoff {
		result.backoff[partition] = until
	}
	for accountID, until := range book.cooloff {
		if !now.Before(until) {
			delete(book.cooloff, accountID)
			continue
		}
		result.cooloff[accountID] = until
	}
	return result
}

// noteStart 记下分区登记了一次轮换
func (book *rotationBook) noteStart(partition aistudio.PoolScope, now time.Time) {
	book.mu.Lock()
	if book.startedAt == nil {
		book.startedAt = make(map[aistudio.PoolScope]time.Time)
	}
	book.startedAt[partition] = now
	book.mu.Unlock()
}

// noteBackoff 让分区暂停轮换 balancedFailBackoff（替补没有启动）
func (book *rotationBook) noteBackoff(partition aistudio.PoolScope, now time.Time) {
	book.mu.Lock()
	if book.backoff == nil {
		book.backoff = make(map[aistudio.PoolScope]time.Time)
	}
	book.backoff[partition] = now.Add(balancedFailBackoff)
	book.mu.Unlock()
}

// noteSpareFailure 记下分区一次因接替的号自己的原因没有启动的轮换，返回是否该暂停：连续 balancedSpareFailStreak 次时暂停（并重新计数），
// 否则不暂停（那个号已按预热失败记录暂时不再被挑中、之后排在冷启动候选最后，下一次判断自然换下一个备用号）
func (book *rotationBook) noteSpareFailure(partition aistudio.PoolScope) bool {
	book.mu.Lock()
	defer book.mu.Unlock()
	if book.spareFails == nil {
		book.spareFails = make(map[aistudio.PoolScope]int)
	}
	book.spareFails[partition]++
	if book.spareFails[partition] < balancedSpareFailStreak {
		return false
	}
	delete(book.spareFails, partition)
	return true
}

// noteSpareStarted 清零分区连续没有启动的接替号计数（接替的号已启动）
func (book *rotationBook) noteSpareStarted(partition aistudio.PoolScope) {
	book.mu.Lock()
	delete(book.spareFails, partition)
	book.mu.Unlock()
}

// noteCooloff 让账号 balancedRotateCooloff 内不再被换下
func (book *rotationBook) noteCooloff(accountID string, now time.Time) {
	book.mu.Lock()
	if book.cooloff == nil {
		book.cooloff = make(map[string]time.Time)
	}
	book.cooloff[accountID] = now.Add(balancedRotateCooloff)
	book.mu.Unlock()
}

// noteDone 记下一次完成的轮换（旧号已关闭）
func (book *rotationBook) noteDone(now time.Time) {
	day := aistudio.QuotaDayKey(now)
	book.mu.Lock()
	book.done = append(pruneRotations(book.done, now), now)
	if day != book.doneDay {
		book.doneDay, book.doneToday = day, 0
	}
	book.doneToday++
	book.mu.Unlock()
}

// pruneRotations 去掉 1 小时以前的轮换时间（原地）
func pruneRotations(done []time.Time, now time.Time) []time.Time {
	kept := done[:0]
	for _, at := range done {
		if now.Sub(at) < time.Hour && !now.Before(at) {
			kept = append(kept, at)
		}
	}
	return kept
}

// notePause 记下分区这一轮的暂停原因（空为不暂停），返回是否该写“平均调用暂停”日志：同一分区同一原因 balancedPauseLogInterval 最多一条
func (book *rotationBook) notePause(partition aistudio.PoolScope, reason string, now time.Time) bool {
	book.mu.Lock()
	defer book.mu.Unlock()
	if reason == "" {
		delete(book.paused, partition)
		return false
	}
	if book.paused == nil {
		book.paused = make(map[aistudio.PoolScope]string)
	}
	book.paused[partition] = reason
	if book.pauseLogAt == nil {
		book.pauseLogAt = make(map[string]time.Time)
	}
	key := partition.String() + "|" + reason
	if last, ok := book.pauseLogAt[key]; ok && now.Sub(last) < balancedPauseLogInterval && !now.Before(last) {
		return false
	}
	book.pauseLogAt[key] = now
	return true
}

// stats 返回暂停原因与近 1 小时、本额度日完成的轮换数
func (book *rotationBook) stats(now time.Time) (map[aistudio.PoolScope]string, int, int) {
	day := aistudio.QuotaDayKey(now)
	book.mu.Lock()
	defer book.mu.Unlock()
	paused := make(map[aistudio.PoolScope]string, len(book.paused))
	for partition, reason := range book.paused {
		paused[partition] = reason
	}
	book.done = pruneRotations(book.done, now)
	today := 0
	if book.doneDay == day {
		today = book.doneToday
	}
	return paused, len(book.done), today
}

// balancedPartitionName 为轮换日志里的分区名
func balancedPartitionName(partition aistudio.PoolScope) string {
	return namesOf(partition).balanced
}

// rotationRegistrations 为一次加锁取得的登记概况：registered 为有任何登记的账号，others 为非轮换登记（冷却或不可调度）的副本与分区，
// rotations 为各分区的轮换登记
type rotationRegistrations struct {
	registered map[string]struct{}
	others     []retirementCandidate
	rotations  map[aistudio.PoolScope][]string
}

// rotationRegistrationsIn 在 retireMu 内取得登记概况（分区按成员快照）
func (manager *accountWorkerManager) rotationRegistrationsIn(members aistudio.PartitionSet) rotationRegistrations {
	manager.retireMu.Lock()
	defer manager.retireMu.Unlock()
	view := rotationRegistrations{
		registered: make(map[string]struct{}, len(manager.retirements)),
		rotations:  make(map[aistudio.PoolScope][]string, len(workerPartitions)),
	}
	for accountID, entry := range manager.retirements {
		view.registered[accountID] = struct{}{}
		partition := partitionOf(accountID, members)
		if entry.rotation {
			view.rotations[partition] = append(view.rotations[partition], accountID)
		} else {
			view.others = append(view.others, retirementCandidate{accountID: accountID, entry: entry.snapshot(), partition: partition})
		}
	}
	return view
}

// retirementBlocksRotation 判断一条非轮换登记（冷却或不可调度）是否让所在分区暂停轮换（死槽位优先）：只有马上要用排空名额或冷启动名额的才让。
//   - 还没到重试时间（没有可接替的账号、关闭失败、排空超时后的推迟）：这段时间它什么也不做，不让；
//   - 账号空闲：淘汰循环这一轮就关闭它，随后由替补或预热补位，让；
//   - 账号忙碌、已在排空：替补还在启动时让（后台冷启动名额先给它），替补已启动或没有启动时不让（只等手上的请求结束）；
//   - 账号忙碌、没有排空：可以排空的冷却登记（全局冷却，或在主用、热门模型上冷却，见 drainable）马上排空并启动替补，让；
//     只在其他模型上冷却的、不可调度的要等账号空闲，不让：高并发下它可能一直有请求，原先整个分区跟着停止轮换，直到它碰巧空闲或冷却到期
func retirementBlocksRotation(entry retirement, unschedulable bool, drain *drainState, busy bool, deadModels []string, now time.Time) bool {
	switch {
	case now.Before(entry.retryAt):
		return false
	case !busy:
		return true
	case drain != nil:
		return drain.replacement.Load() == replacementPending
	case unschedulable:
		return false
	}
	return entry.drainable(deadModels)
}

// retiringIn 返回有马上要处理的非轮换登记的分区（见 retirementBlocksRotation）：登记账号的忙闲与可调度一次加锁取得，
// 能否排空按各分区的死槽位模型判断；账户池已不认识的账号不算（淘汰循环随后放弃它）
func (manager *accountWorkerManager) retiringIn(
	others []retirementCandidate, drains map[string]*drainState, now time.Time,
) map[aistudio.PoolScope]bool {
	retiring := make(map[aistudio.PoolScope]bool, len(workerPartitions))
	if len(others) == 0 {
		return retiring
	}
	accountIDs := make([]string, len(others))
	for index := range others {
		accountIDs[index] = others[index].accountID
	}
	states := manager.pool.WarmWorkerStates(accountIDs, "", rotationMinRemaining)
	deadModels := make(map[aistudio.PoolScope][]string, len(workerPartitions))
	for _, other := range others {
		state, known := states[other.accountID]
		if retiring[other.partition] || !known {
			continue
		}
		models, ok := deadModels[other.partition]
		if !ok {
			models = manager.deadSlotModelsFor(other.partition)
			deadModels[other.partition] = models
		}
		unschedulable := other.entry.unschedulable != "" || state.Unschedulable != ""
		if retirementBlocksRotation(other.entry, unschedulable, drains[other.accountID], state.Busy, models, now) {
			retiring[other.partition] = true
		}
	}
	return retiring
}

// rotationCandidatesIn 收集分区内驻留 Worker（accountIDs，已就绪）的轮换状态：账户池一次加锁取得忙闲、冷却与可调度，再按 models
// 核对能服务的主用、热门模型个数（每份非空列表加锁一次），其余读管理器的无锁状态；账户锁被占着（Worker 正在关闭、发布或核对）时
// 这一轮不作为候选，不排队等锁
func (manager *accountWorkerManager) rotationCandidatesIn(
	accountIDs []string, counts map[string]uint32, registered map[string]struct{}, drains map[string]*drainState, cooloff map[string]time.Time,
	models coldRankModels,
) []rotationCandidate {
	states := manager.pool.WarmWorkerStates(accountIDs, "", rotationMinRemaining)
	ranks := models.rankInto(manager.pool, accountIDs, make(map[string]coldRank, len(accountIDs)))
	candidates := make([]rotationCandidate, 0, len(accountIDs))
	for _, accountID := range accountIDs {
		manager.mu.RLock()
		account := manager.accounts[accountID]
		manager.mu.RUnlock()
		state, known := states[accountID]
		if account == nil || !known {
			continue
		}
		_, hasRegistration := registered[accountID]
		_, draining := drains[accountID]
		candidate := rotationCandidate{
			accountID: accountID, usage: counts[accountID], lastUsed: state.LastUsed, busy: state.Busy,
			starting: manager.workerStarting(account, accountID), reserved: manager.victimReserved(accountID),
			registered: hasRegistration || draining, failed: true,
			unschedulable: state.Unschedulable != "", dead: state.Dead, cooling: state.Cooling,
			cooloffUntil: cooloff[accountID], rank: ranks[accountID],
		}
		if at := account.readyAt.Load(); at > 0 {
			candidate.readyAt = time.Unix(0, at)
		}
		if account.mu.TryLock() {
			candidate.failed = account.worker == nil || account.worker.State().Phase == aistudio.WorkerFailed
			account.mu.Unlock()
		}
		candidates = append(candidates, candidate)
	}
	return candidates
}

// registerRotation 写一条轮换登记并唤醒淘汰循环；账号已没有驻留 Worker、或已有任何登记时不登记，返回是否写入
func (manager *accountWorkerManager) registerRotation(decision rotationDecision) bool {
	manager.mu.RLock()
	account := manager.accounts[decision.accountID]
	manager.mu.RUnlock()
	if account == nil || !account.warm.Load() {
		return false
	}
	manager.retireMu.Lock()
	if manager.retirements == nil {
		manager.retirements = make(map[string]*retirement)
	}
	if manager.retirements[decision.accountID] != nil {
		manager.retireMu.Unlock()
		return false
	}
	manager.retirements[decision.accountID] = &retirement{
		since: time.Now(), rotation: true, rotationSoft: decision.soft, rotationUsage: decision.usage, rotationTarget: decision.target,
	}
	manager.retirePending.Store(int32(len(manager.retirements)))
	manager.retireMu.Unlock()
	manager.wakeRetire()
	return true
}

// balancedRotate 为平均调用一轮的轮换判断（balancedPass 调用，自带它的 30 秒节奏）：收集输入、按分区调用 pickRotation、记下暂停原因
// （同一原因 5 分钟最多一条日志）并写轮换登记。只登记，不关闭浏览器：执行在 retirePass（见 advanceRotation）
func (service *trackedService) balancedRotate(ctx context.Context, now time.Time) {
	workers := service.workers
	if workers == nil || !workers.balancedOn() || ctx.Err() != nil {
		return
	}
	book := &workers.balancedState.rotation
	running := workers.serviceRunning()
	limited := service.egress.anyLimited()
	snapshot := service.pool.QuotaDayUsage()
	members := service.pool.PartitionMembers()
	ready := workers.ReadyWarmAccountIDs()
	allWarm := workers.WarmAccountIDs()
	view := workers.rotationRegistrationsIn(members)
	drains := workers.drainSnapshot()
	retiring := workers.retiringIn(view.others, drains, now)
	records := book.snapshot(now)
	for _, partition := range workerPartitions {
		if ctx.Err() != nil {
			return
		}
		accountIDs := inPartition(ready, members, partition)
		if len(accountIDs) == 0 {
			// 分区里没有在跑的 Worker：没有可换的，也不写暂停（只有普通账号的部署里 Ultra 与 Pro 分区一直如此）
			book.notePause(partition, "", now)
			continue
		}
		inProgress := false
		for _, accountID := range view.rotations[partition] {
			if drain := drains[accountID]; drain == nil || drain.soft.Load() {
				inProgress = true
			}
		}
		input := rotationInput{
			now: now, enabled: true, running: running, egressLimited: limited,
			pendingOther: retiring[partition], inProgress: inProgress,
			lastStart: records.startedAt[partition], backoffUntil: records.backoff[partition],
			rate:       balancedRotateRate(workers.maxActiveFor(partition), workers.warmConcurrencyValue()),
			drainSlots: workers.drainSlotCount(partition), drainLimit: workers.drainLimit(partition),
			drainTotal: workers.drainCount(partition), drainTotalLimit: workers.drainTotalLimit(partition),
			resident: len(inPartition(allWarm, members, partition)), warmTarget: workers.warmTargetFor(partition),
		}
		// 暂停条件不成立时才收集候选与干跑替补挑法
		if running && !limited && !input.pendingOther && !inProgress {
			// 与预热、替补排序同一口径（见 rankColdCandidates）：W 与 S 能服务的主用、热门模型个数按同一份列表核对
			models := workers.coldRankModelsFor(partition)
			input.candidates = workers.rotationCandidatesIn(accountIDs, snapshot.Counts, view.registered, drains, records.cooloff, models)
			input.spare = func() rotationSpare {
				spare := workers.pickReplacement(ctx, partition, nil, allWarm)
				if spare == "" {
					return rotationSpare{}
				}
				rank := models.rankInto(service.pool, []string{spare}, make(map[string]coldRank, 1))[spare]
				return rotationSpare{accountID: spare, usage: snapshot.Counts[spare], rank: rank}
			}
		}
		decision := pickRotation(input)
		reason := ""
		if decision.pause {
			reason = decision.reason
		}
		if book.notePause(partition, reason, now) {
			service.requests.log("service", "INFO", fmt.Sprintf("平均调用暂停 | 分区=%s | 原因=%s", balancedPartitionName(partition), reason))
		}
		if decision.accountID == "" || !workers.registerRotation(decision) {
			continue
		}
		book.noteStart(partition, now)
		path := "空闲直接换"
		if decision.soft {
			path = "先启后停"
		}
		service.requests.log(decision.accountID, "INFO", fmt.Sprintf(
			"WAA Worker 平均调用轮换登记 | 今日发送=%d | 接替今日=%d | 差=%d（门槛 %d） | 路径=%s | 就绪=%s",
			decision.usage, decision.target, decision.usage-decision.target, balancedRotateGap, path,
			now.Sub(decision.readyAt).Round(time.Second),
		))
	}
}

// cancelRotation 放弃一条轮换登记（同时清除排空，软排空时账号本来就在接单）并写“平均调用轮换取消”；
// 登记已被冷却或不可调度登记接管时什么也不做（见 dropRotation）
func (manager *accountWorkerManager) cancelRotation(accountID string, reason string) {
	if manager.dropRotation(accountID) {
		manager.logRotationCancel(accountID, reason)
	}
}

// dropRotation 删除账号的轮换登记并清除排空，返回是否删除。retirePass 按一轮开始时的副本处理，这期间登记可能已被冷却或不可调度登记
// 接管（registerRetirement、registerUnschedulable）：那时不删、也不清除排空，交给原来的流程（软排空由冷却路径改为普通排空）
func (manager *accountWorkerManager) dropRotation(accountID string) bool {
	manager.retireMu.Lock()
	entry := manager.retirements[accountID]
	if entry == nil || !entry.rotation {
		manager.retireMu.Unlock()
		return false
	}
	delete(manager.retirements, accountID)
	manager.retirePending.Store(int32(len(manager.retirements)))
	manager.retireMu.Unlock()
	manager.clearDrain(accountID)
	return true
}

// logRotationCancel 写“平均调用轮换取消”（WARN，来源为被换下的账户）
func (manager *accountWorkerManager) logRotationCancel(accountID string, reason string) {
	manager.requests.log(accountID, "WARN", "平均调用轮换取消 | 原因="+reason)
}

// advanceRotation 为 retirePass 里一条轮换登记的一步，返回是否该关闭（账号空闲、可以加入本轮的关闭列表）。candidate.drain 为本轮读到的排空状态。
//  1. 平均调用已关闭：还没开始、或软排空的替补还在启动或没有启动时放弃；替补已启动（或让出的槽位已被扩容占去）、已在普通排空的照常完成，
//     否则分区会多出一个 Worker；
//  2. 账号正在启动或替换 Worker：下一轮再看；
//  3. 还没开始：登记超过 balancedRegistrationTTL 放弃；先启后停时写软排空（名额不够时下一轮再试）并在后台启动替补；
//     空闲直接换时空闲就关闭，忙碌就等；
//  4. 软排空：替补还在启动时等待（超过 balancedSoftTTL 按没有启动处理）；替补已启动或让出的槽位已被扩容占去时改为普通排空，
//     空闲就关闭、忙碌就等；替补没有启动时放弃（账号一直在接单，不受影响）：接替的号自己起不来（浏览器启动失败等，spareFault）时分区
//     不暂停，那个号已暂时不再被挑中、之后排在冷启动候选最后，下一次判断换下一个备用号；其余情况（没有可挑的号、出口限流、软阶段超时），
//     或连续 balancedSpareFailStreak 个接替的号都起不来时，分区暂停轮换 balancedFailBackoff；
//  5. 普通排空且忙碌：排空超过 drainTimeoutFor 时放弃，不重试，30 分钟内不再换下它（多半是指定账户或文件绑定的请求一直在用）；
//  6. 普通排空且空闲：关闭
func (manager *accountWorkerManager) advanceRotation(now time.Time, candidate retirementCandidate, account *accountWorker, busy bool) bool {
	accountID, entry, drain := candidate.accountID, candidate.entry, candidate.drain
	book := &manager.balancedState.rotation
	if !manager.balancedOn() {
		if drain == nil {
			manager.cancelRotation(accountID, rotationCancelOff)
			return false
		}
		if result := drain.replacement.Load(); drain.soft.Load() && (result == replacementPending || result == replacementFailed) {
			manager.cancelRotation(accountID, rotationCancelOff)
			return false
		}
	}
	if manager.workerStarting(account, accountID) {
		return false
	}
	if drain == nil {
		if now.Sub(entry.since) >= balancedRegistrationTTL {
			manager.cancelRotation(accountID, rotationCancelStale)
			return false
		}
		if !entry.rotationSoft {
			return !busy
		}
		// 账号在取成员快照之后换了分区（ULTRA_INCLUDE_PRO 切换）、排空名额已满或已被其他路径选中时 startDrain 返回 nil，下一轮再试
		if drain = manager.startDrain(accountID, candidate.partition, true); drain != nil {
			manager.startReplacement(candidate.partition, nil, rotationReplacementReason, drain)
		}
		return false
	}
	if drain.soft.Load() {
		result := drain.replacement.Load()
		if result == replacementPending {
			if drain.age(now) < manager.balancedSoftTTL() {
				return false
			}
			result = replacementFailed
		}
		if result == replacementFailed {
			// spareFault 在 replacement 写入失败之前写入；软阶段超时时替补还在启动，spareFault 为假
			if drain.spareFault.Load() && !book.noteSpareFailure(candidate.partition) {
				manager.cancelRotation(accountID, rotationCancelSpareFailed)
				return false
			}
			book.noteBackoff(candidate.partition, now)
			manager.cancelRotation(accountID, rotationCancelFailed)
			return false
		}
		if result == replacementStarted {
			book.noteSpareStarted(candidate.partition)
		}
		if !manager.hardenDrain(accountID, drain) {
			return false
		}
		state := "接替已就绪"
		if result == replacementNoRoom {
			state = "让出的槽位已被请求扩容占用"
		}
		manager.requests.log(accountID, "INFO", fmt.Sprintf(
			"WAA Worker 平均调用 | %s，不再接新请求，进行中的请求结束后关闭 | 进行中=%d",
			state, manager.pool.CandidateStatesForScope([]string{accountID}, "", "")[accountID].Active,
		))
		return !busy
	}
	if busy {
		if drain.age(now) >= manager.drainTimeoutFor(accountID) {
			book.noteCooloff(accountID, now)
			manager.cancelRotation(accountID, rotationCancelTimeout)
		}
		return false
	}
	return true
}

// finishRotation 关闭一个已选中的轮换 Worker（调用方已 tryReserveVictim）：写“WAA Worker 平均调用轮换”，账号 30 分钟内不再被换下，
// 补齐预热，并按 replacementsFor 在分区内启动替补（先启后停的替补已启动时不重复补）。关闭出错时放弃这次轮换，同样 30 分钟内不再换下它
func (manager *accountWorkerManager) finishRotation(victim retirementCandidate) {
	accountID := victim.accountID
	book := &manager.balancedState.rotation
	evicted, err := manager.evictIdleWorker(manager.lifecycle, accountID)
	manager.releaseVictim(accountID)
	switch {
	case err != nil:
		book.noteCooloff(accountID, time.Now())
		manager.dropRotation(accountID)
		manager.requests.log(accountID, "WARN", fmt.Sprintf("WAA Worker 平均调用轮换失败 | 错误=%v | 30 分钟内不再换下本号", err))
	case !evicted:
		// 取独占租约时账号又有了请求（或其他进程持有文件锁），等下一次唤醒；普通排空的继续排空，空闲直接换的到登记时限放弃
	default:
		manager.dropRetirement(accountID)
		now := time.Now()
		book.noteCooloff(accountID, now)
		book.noteDone(now)
		// 替补补在 Worker 关闭时所在的分区（见 finishRetirement）
		victim.partition = partitionOf(accountID, manager.pool.PartitionMembers())
		plan := manager.replacementsFor([]evictedWorker{{partition: victim.partition, drain: victim.drain}})
		manager.requests.log(accountID, "INFO", fmt.Sprintf(
			"WAA Worker 平均调用轮换 | 今日发送=%d | 登记后=%s | %s=%d/%d",
			manager.quotaUsage(now).Counts[accountID], now.Sub(victim.entry.since).Round(time.Millisecond),
			workerSlotName(victim.partition), len(manager.warmAccountIDsIn(victim.partition)), manager.maxActiveFor(victim.partition),
		))
		if refill := manager.afterRetire.Load(); refill != nil {
			(*refill)()
		}
		manager.startReplacements(plan, rotationReplacementReason)
	}
}
