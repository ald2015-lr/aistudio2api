package aistudio

import (
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"
	"time"
)

// 候选索引
//
// 调度一次请求原先要在账户池锁内遍历全部账户，逐个判断启用、就绪、目录支持与冷却（见 classifyCandidatesBruteLocked）。
// 账户上万时每次要十几毫秒，几十个请求排队就是秒级等待：用户服务器 14,933 个账户、12,000 个处于每日限额冷却时，
// ClassifyCandidates 单次持锁最长 826ms，锁等待累计 48.9 小时。现在按选择条件（模型、方法、能力、通道限定、号池、
// 资格范围）为键维护候选集：
//   - until 按账户下标记录成员状态：0 非候选；-1 候选且未冷却；正数为候选且冷却到该时刻（UnixNano）
//   - available 为未冷却候选的下标，按账户 ID 升序；轮询时从上次选中账户之后开始，与原先遍历有序账户的顺序一致
//   - cooling 为冷却候选按恢复时间排列的最小堆：到期时弹出并移回 available，堆顶即最早恢复时间；
//     账户变化时旧堆项不删除，浮到堆顶时发现与 until 不符即丢弃
//
// 成员资格（启用、就绪、号池、通道支持、冷却期限）只在账户变化时由 accountChangedLocked 增量更新，
// 每条变更路径（目录、权益、状态、启用、运行态同步、Build 目录）都要调用它；忙闲（租约、独占、认证刷新）
// 由 busyChangedLocked 在每次变化时写进按下标的紧凑数组，分类时顺序扫描数组，不再逐个访问散落在堆上的账户结构
// （上万账户时缓存未命中占了大半时间）。账户增删或上游通道变化时整个索引作废重建；候选集另按期全量重建一次，
// 兜底任何遗漏的增量更新。全部结构都受账户池 mu 保护
const (
	// schedulingSetMaxAge 为候选集两次全量重建的间隔
	schedulingSetMaxAge = time.Minute
	// schedulingSetMaxCount 为候选集数量上限，超过时淘汰最久未用的
	schedulingSetMaxCount = 256
	// schedulingBuildRebuildInterval 为 Build 通道候选集在目录版本变化后重建的最短间隔：Build 通道能否调用模型
	// 还取决于其他账户 Playground 目录中的同名模型（见 buildSupportsModelLocked），这个跨账户依赖按节流重建
	schedulingBuildRebuildInterval = time.Second
	// statusCacheMinAccounts 以上的账户池缓存 Status 结果 statusCacheTTL：构建全部账户状态要拷贝每个账户的冷却表，
	// 账户上万时每次几十毫秒，管理页、目录推送与失败日志都会调用
	statusCacheMinAccounts = 200
	statusCacheTTL         = time.Second
)

// selectionKey 为候选集的键：影响"账户是否支持选择、按哪些冷却键判断"的全部选择条件。
// 方法、能力与资格范围按原值参与（原实现用原值匹配目录、按资格范围是否为空区分生成请求），
// 账户与资源限定不进索引，PreferPlayground 只影响顺序
type selectionKey struct {
	modelID         string
	accessScope     string
	method          string
	capability      string
	pool            PoolScope
	playgroundOnly  bool
	buildOnly       bool
	playgroundFirst bool
}

func selectionKeyOf(selection AccountSelection) selectionKey {
	return selectionKey{
		modelID: strings.TrimPrefix(strings.TrimSpace(selection.ModelID), "models/"), accessScope: selection.ModelAccessScope,
		method: selection.Method, capability: selection.Capability, pool: selection.Pool,
		playgroundOnly: selection.PlaygroundOnly, buildOnly: selection.BuildOnly, playgroundFirst: selection.PlaygroundFirst,
	}
}

// selection 还原为判断通道支持所需的选择条件
func (key selectionKey) selection() AccountSelection {
	return AccountSelection{
		ModelID: key.modelID, ModelAccessScope: key.accessScope, Method: key.method, Capability: key.capability, Pool: key.pool,
		PlaygroundOnly: key.playgroundOnly, BuildOnly: key.buildOnly, PlaygroundFirst: key.playgroundFirst,
	}
}

// coolingEntry 为冷却堆中的一项：账户下标与登记时的恢复时间
type coolingEntry struct {
	until    int64
	position int
}

// coolingHeap 为按恢复时间排列的最小堆；不用 container/heap 以免每次弹出都装箱分配
type coolingHeap []coolingEntry

func (h *coolingHeap) push(entry coolingEntry) {
	*h = append(*h, entry)
	entries := *h
	child := len(entries) - 1
	for child > 0 {
		parent := (child - 1) / 2
		if entries[parent].until <= entries[child].until {
			break
		}
		entries[parent], entries[child] = entries[child], entries[parent]
		child = parent
	}
}

func (h *coolingHeap) pop() coolingEntry {
	entries := *h
	top := entries[0]
	last := len(entries) - 1
	entries[0] = entries[last]
	entries = entries[:last]
	parent := 0
	for {
		child := 2*parent + 1
		if child >= len(entries) {
			break
		}
		if child+1 < len(entries) && entries[child+1].until < entries[child].until {
			child++
		}
		if entries[parent].until <= entries[child].until {
			break
		}
		entries[parent], entries[child] = entries[child], entries[parent]
		parent = child
	}
	*h = entries
	return top
}

// candidateSet 为一个选择条件下的候选集
type candidateSet struct {
	key       selectionKey
	selection AccountSelection
	// scope 为冷却键的资格范围（selectionAccessScope），channels 为建立时的可用通道顺序（上游通道变化时整个索引作废）
	scope     string
	channels  []Channel
	until     []int64
	available []int
	cooling   coolingHeap
	members   int
	builtAt   time.Time
	lastUsed  time.Time
	// eligibilityVersion 为上次重建时的目录版本，只有含 Build 通道的候选集按它节流重建；playgroundStale 记录重建时
	// Playground 模型索引仍在节流中（落后于目录版本），节流过后要再重建一次，不把旧索引的结论留到下一次目录变化
	eligibilityVersion uint64
	playgroundStale    bool
}

// schedulingIndex 为账户池的候选索引
type schedulingIndex struct {
	accountsVersion uint64
	sets            map[selectionKey]*candidateSet
	// 按账户下标的紧凑数组：ids 为账户 ID；enabled、state、tier、globalUntil 为启用、状态、权益与全局冷却期限
	// （UnixNano，0 为没有）；coolUntil 为账户按状态口径冷却到的时刻（全局冷却，或某个模型在账户可用的每个启用通道上都冷却，
	// 见 accountCoolingUntilLocked），由 accountChangedLocked 更新；blocked 为独占、等待独占或认证刷新中，active 为进行中的租约数，
	// 由 busyChangedLocked 更新。状态统计与换号上限按这些数组顺序扫描，不再逐个访问账户结构
	ids         []string
	enabled     []bool
	state       []AccountState
	tier        []BenefitTier
	globalUntil []int64
	coolUntil   []int64
	blocked     []bool
	active      []int32
	// coolVersion 为上次按目录版本核对 coolUntil 时的 eligibilityVersion，coolSyncedAt 为核对时刻：Build 通道是否可用于某模型
	// 还取决于其他账户的 Playground 目录（见 buildSupportsModelLocked），这个跨账户依赖按节流全量重算（见 refreshCoolingLocked）
	coolVersion  uint64
	coolSyncedAt time.Time
	// warmStamp 记录最近一次分类把该账户标为热账户时的代数；lastWarm 为上次标记用的热账户列表，列表不变时直接沿用代数：
	// 每次分类都按 ID 查一遍上万条目的账户表要几十微秒，而热账户列表只在 Worker 启停时变化
	warmStamp      []uint32
	warmGeneration uint32
	lastWarm       []string
}

// schedulingLocked 返回候选索引，账户增删后重建并重新给账户编号；调用方持有 p.mu
func (p *AccountPool) schedulingLocked() *schedulingIndex {
	if p.sched == nil || p.sched.accountsVersion != p.accountsVersion {
		size := len(p.accounts)
		index := &schedulingIndex{
			accountsVersion: p.accountsVersion, sets: make(map[selectionKey]*candidateSet),
			ids: make([]string, size), enabled: make([]bool, size), state: make([]AccountState, size),
			tier: make([]BenefitTier, size), globalUntil: make([]int64, size), coolUntil: make([]int64, size),
			blocked: make([]bool, size), active: make([]int32, size), warmStamp: make([]uint32, size),
			coolVersion: p.eligibilityVersion, coolSyncedAt: time.Now(),
		}
		for position, account := range p.accounts {
			if account == nil {
				continue
			}
			account.position = position
			index.ids[position] = account.ID
			index.syncAccount(p, account)
			index.blocked[position] = accountBlocked(account)
			index.active[position] = int32(account.active)
		}
		p.sched = index
	}
	return p.sched
}

// derivedStatesLocked 返回用于推导账户对外状态的候选索引：先按目录版本核对 coolUntil；调用方持有 p.mu
func (p *AccountPool) derivedStatesLocked(now time.Time) *schedulingIndex {
	index := p.schedulingLocked()
	index.refreshCoolingLocked(p, now)
	return index
}

// syncAccount 把账户的启用、状态、权益、全局冷却期限与状态口径的冷却期限写进数组；调用方已确认下标有效
func (index *schedulingIndex) syncAccount(p *AccountPool, account *Account) {
	position := account.position
	index.enabled[position] = account.Config.Enabled
	index.state[position] = account.State
	index.tier[position] = account.BenefitTier
	index.globalUntil[position] = cooldownNanos(account.runtime.Cooldowns[globalCooldownKey])
	index.coolUntil[position] = p.accountCoolingUntilLocked(account)
}

// refreshCoolingLocked 在目录版本变化后按节流重算全部账户的 coolUntil：账户自己的冷却表、目录与权益变化时 accountChangedLocked
// 已逐个同步，这里只兜底 Build 通道对其他账户 Playground 目录的依赖，因此只启用 Playground 时不需要重算；调用方持有 p.mu
func (index *schedulingIndex) refreshCoolingLocked(p *AccountPool, now time.Time) {
	if index.coolVersion == p.eligibilityVersion {
		return
	}
	if !p.channelEnabledLocked(ChannelBuild) {
		index.coolVersion = p.eligibilityVersion
		return
	}
	if now.Sub(index.coolSyncedAt) < schedulingBuildRebuildInterval {
		return
	}
	// Playground 模型索引还在节流中时这一轮不重算：用旧索引算出的期限会一直留到下一次目录变化，与 StatusOf 的现算结果不一致；
	// 下一次调用在索引重建后补上（账户多时最多晚一秒）
	if !p.playgroundIndexCurrentLocked() {
		return
	}
	for position, account := range p.accounts {
		if account != nil && position < len(index.coolUntil) {
			index.coolUntil[position] = p.accountCoolingUntilLocked(account)
		}
	}
	index.coolVersion = p.eligibilityVersion
	index.coolSyncedAt = now
}

// accountBlocked 判断账户是否因独占、等待独占或认证刷新而不能接请求
func accountBlocked(account *Account) bool {
	return account.exclusive || account.exclusiveWaiters > 0 || account.authRefreshers > 0
}

// invalidateSchedulingLocked 作废候选索引，下次调度时重建；调用方持有 p.mu
func (p *AccountPool) invalidateSchedulingLocked() {
	p.sched = nil
}

// indexPositionValidLocked 判断账户的下标在索引中有效
func (index *schedulingIndex) positionValid(p *AccountPool, account *Account) bool {
	position := account.position
	return position >= 0 && position < len(index.ids) && position < len(p.accounts) && p.accounts[position] == account
}

// busyChangedLocked 在账户的租约数、独占或认证刷新状态变化后更新忙闲数组；调用方持有 p.mu。
// 每条修改这些字段的路径都必须调用
func (p *AccountPool) busyChangedLocked(account *Account) {
	if p.sched == nil || account == nil || !p.sched.positionValid(p, account) {
		return
	}
	p.sched.blocked[account.position] = accountBlocked(account)
	p.sched.active[account.position] = int32(account.active)
}

// stampWarmLocked 把热账户标上新的代数，返回该代数；分类时按代数判断账户是否为热账户。列表与上次相同时沿用上次的代数
func (index *schedulingIndex) stampWarmLocked(p *AccountPool, warmAccountIDs []string) uint32 {
	if index.lastWarm != nil && slices.Equal(index.lastWarm, warmAccountIDs) {
		return index.warmGeneration
	}
	index.warmGeneration++
	if index.warmGeneration == 0 {
		clear(index.warmStamp)
		index.warmGeneration = 1
	}
	for _, accountID := range warmAccountIDs {
		if account := p.byID[strings.TrimSpace(accountID)]; account != nil && index.positionValid(p, account) {
			index.warmStamp[account.position] = index.warmGeneration
		}
	}
	index.lastWarm = append(index.lastWarm[:0], warmAccountIDs...)
	if index.lastWarm == nil {
		index.lastWarm = []string{}
	}
	return index.warmGeneration
}

// candidateSetLocked 返回选择条件对应的候选集，没有时建立，过期或下标数不符时重建；调用方持有 p.mu
func (p *AccountPool) candidateSetLocked(selection AccountSelection, now time.Time) *candidateSet {
	index := p.schedulingLocked()
	key := selectionKeyOf(selection)
	set := index.sets[key]
	if set == nil {
		if len(index.sets) >= schedulingSetMaxCount {
			index.evictLocked()
		}
		set = &candidateSet{key: key, selection: key.selection()}
		set.scope = selectionAccessScope(set.selection)
		index.sets[key] = set
		p.rebuildCandidateSetLocked(set, now)
	} else if len(set.until) != len(p.accounts) || now.Sub(set.builtAt) >= schedulingSetMaxAge ||
		(set.eligibilityVersion != p.eligibilityVersion || set.playgroundStale) && slices.Contains(set.channels, ChannelBuild) &&
			now.Sub(set.builtAt) >= schedulingBuildRebuildInterval {
		p.rebuildCandidateSetLocked(set, now)
	}
	set.lastUsed = now
	return set
}

// evictLocked 淘汰最久未用的候选集
func (index *schedulingIndex) evictLocked() {
	var victim selectionKey
	var oldest time.Time
	for key, set := range index.sets {
		if oldest.IsZero() || set.lastUsed.Before(oldest) {
			victim, oldest = key, set.lastUsed
		}
	}
	delete(index.sets, victim)
}

// rebuildCandidateSetLocked 按当前账户状态全量重建候选集；调用方持有 p.mu
func (p *AccountPool) rebuildCandidateSetLocked(set *candidateSet, now time.Time) {
	// selectionChannelsLocked 可能直接返回账户池共用的通道切片，另存一份
	set.channels = append(set.channels[:0], p.selectionChannelsLocked(set.selection)...)
	// 含 Build 通道的候选集按 Playground 模型索引判断通道支持：索引仍在节流中时记下，节流过后再重建一次
	set.playgroundStale = slices.Contains(set.channels, ChannelBuild) && !p.playgroundIndexCurrentLocked()
	if cap(set.until) >= len(p.accounts) {
		set.until = set.until[:len(p.accounts)]
		clear(set.until)
	} else {
		set.until = make([]int64, len(p.accounts))
	}
	set.available = set.available[:0]
	set.cooling = set.cooling[:0]
	set.members = 0
	// 按账户 ID 顺序登记，available 天然有序
	for _, position := range p.sortedAccountIndicesLocked() {
		state := p.candidateStateLocked(set, p.accounts[position])
		if state == 0 {
			continue
		}
		set.until[position] = state
		set.members++
		if state < 0 {
			set.available = append(set.available, position)
		} else {
			set.cooling.push(coolingEntry{until: state, position: position})
		}
	}
	set.builtAt = now
	set.eligibilityVersion = p.eligibilityVersion
}

// candidateStateLocked 计算账户在候选集中的成员状态：0 非候选，-1 候选且没有冷却，正数为候选且冷却到该时刻。
// 与 classifyCandidatesBruteLocked 的判断一致：启用、就绪、属于号池、至少一个通道支持选择；
// 冷却按每个支持通道取全局与通道作用域中较晚的期限，账户的恢复时间为各通道中最早的（见 accountChannelsCooldown），
// 期限只在冷却表变化时改变，是否已到期由调度时的时刻决定
func (p *AccountPool) candidateStateLocked(set *candidateSet, account *Account) int64 {
	if account == nil || !account.Config.Enabled || account.State != AccountReady || !set.key.pool.allows(account) {
		return 0
	}
	var supportedBuffer [4]Channel
	supported := supportedBuffer[:0]
	for _, channel := range set.channels {
		if p.channelSupportsLocked(account, channel, set.selection) {
			supported = append(supported, channel)
		}
	}
	if set.key.modelID != "" && len(supported) == 0 {
		return 0
	}
	until := int64(-1)
	global := cooldownNanos(account.runtime.Cooldowns[globalCooldownKey])
	for _, channel := range supported {
		channelUntil := global
		if scopeKey := ChannelCooldownScope(channel, set.scope); scopeKey != "" {
			channelUntil = max(channelUntil, cooldownNanos(account.runtime.Cooldowns[scopeKey]))
		}
		if channelUntil <= 0 {
			return -1
		}
		if until < 0 || channelUntil < until {
			until = channelUntil
		}
	}
	return until
}

// cooldownNanos 返回冷却期限的 UnixNano；没有期限为 0。超出 UnixNano 表示范围的期限按最大值处理
func cooldownNanos(cooldown CooldownState) int64 {
	if cooldown.Until.IsZero() {
		return 0
	}
	if cooldown.Until.Year() > 2200 {
		return math.MaxInt64
	}
	return max(cooldown.Until.UnixNano(), 0)
}

// indexAccountAddedLocked 在新账户加入后给它编号并登记到索引与各候选集；调用方持有 p.mu，账户已追加到 accounts 末尾
func (p *AccountPool) indexAccountAddedLocked(account *Account) {
	account.position = len(p.accounts) - 1
	if index := p.sched; index != nil {
		index.ids = append(index.ids, account.ID)
		index.enabled = append(index.enabled, false)
		index.state = append(index.state, "")
		index.tier = append(index.tier, 0)
		index.globalUntil = append(index.globalUntil, 0)
		index.coolUntil = append(index.coolUntil, 0)
		index.blocked = append(index.blocked, accountBlocked(account))
		index.active = append(index.active, int32(account.active))
		index.warmStamp = append(index.warmStamp, 0)
		// 新账户可能已在热账户列表里，下次分类重新标记
		index.lastWarm = nil
		for _, set := range index.sets {
			set.until = append(set.until, 0)
		}
		index.accountsVersion = p.accountsVersion
	}
	p.accountChangedLocked(account)
}

// accountChangedLocked 在账户的目录、权益、状态、启用或运行态变化后更新各候选集、Ultra 账户集合与缓存的状态列表；调用方持有 p.mu。
// 每条修改这些字段的路径都必须调用，否则候选集要等下次全量重建才能反映变化，Status() 要等缓存过期才与 StatusOf 一致
func (p *AccountPool) accountChangedLocked(account *Account) {
	if account == nil {
		return
	}
	if account.BenefitTier == BenefitTierUltra && p.byID[account.ID] == account {
		p.ultraAccounts[account.ID] = struct{}{}
	} else {
		delete(p.ultraAccounts, account.ID)
	}
	if p.sched != nil {
		if p.sched.positionValid(p, account) {
			p.sched.syncAccount(p, account)
		}
		for _, set := range p.sched.sets {
			p.updateCandidateLocked(set, account)
		}
	}
	p.refreshStatusEntryLocked(account)
}

// derivedStateAt 按数组推导账户对外状态，与 accountStateLocked 相同：停用、忙碌、冷却或原始状态；
// 冷却按状态口径的期限 coolUntil 判断（全局冷却，或某个模型在每个可用通道上都冷却）
func (index *schedulingIndex) derivedStateAt(position int, nowNanos int64) AccountState {
	state := index.state[position]
	if !index.enabled[position] {
		return AccountDisabled
	}
	if state == AccountReady && (index.blocked[position] || index.active[position] > 0) {
		return AccountBusy
	}
	if state == AccountReady && index.coolUntil[position] > nowNanos {
		return AccountCooldown
	}
	return state
}

// updateCandidateLocked 按账户当前状态更新它在候选集中的成员状态
func (p *AccountPool) updateCandidateLocked(set *candidateSet, account *Account) {
	position := account.position
	if position < 0 || position >= len(set.until) || p.accounts[position] != account {
		// 下标与账户不符：候选集已失效，下次使用时重建
		set.builtAt = time.Time{}
		return
	}
	next := p.candidateStateLocked(set, account)
	current := set.until[position]
	if next == current {
		return
	}
	if current == -1 {
		set.removeAvailableLocked(p, position)
	}
	if current == 0 && next != 0 {
		set.members++
	} else if current != 0 && next == 0 {
		set.members--
	}
	set.until[position] = next
	if next == -1 {
		set.insertAvailableLocked(p, position)
	} else if next > 0 {
		set.cooling.push(coolingEntry{until: next, position: position})
	}
}

// availableSearchLocked 返回账户在 available 中的位置（或应插入的位置）
func (set *candidateSet) availableSearchLocked(p *AccountPool, accountID string) int {
	return sort.Search(len(set.available), func(index int) bool {
		return p.accounts[set.available[index]].ID >= accountID
	})
}

func (set *candidateSet) insertAvailableLocked(p *AccountPool, position int) {
	accountID := p.accounts[position].ID
	at := set.availableSearchLocked(p, accountID)
	if at < len(set.available) && set.available[at] == position {
		return
	}
	set.available = slices.Insert(set.available, at, position)
}

func (set *candidateSet) removeAvailableLocked(p *AccountPool, position int) {
	accountID := p.accounts[position].ID
	at := set.availableSearchLocked(p, accountID)
	if at < len(set.available) && set.available[at] == position {
		set.available = slices.Delete(set.available, at, at+1)
	}
}

// expireLocked 把冷却已到期的候选移回 available
func (set *candidateSet) expireLocked(p *AccountPool, nowNanos int64) {
	for len(set.cooling) > 0 && set.cooling[0].until <= nowNanos {
		entry := set.cooling.pop()
		if set.until[entry.position] == entry.until {
			set.until[entry.position] = -1
			set.insertAvailableLocked(p, entry.position)
		}
	}
}

// earliestCoolingLocked 返回仍在冷却的候选中最早的恢复时间；调用方已先执行 expireLocked
func (set *candidateSet) earliestCoolingLocked() (int64, bool) {
	for len(set.cooling) > 0 {
		top := set.cooling[0]
		if set.until[top.position] == top.until {
			return top.until, true
		}
		set.cooling.pop()
	}
	return 0, false
}

// rotationStartLocked 返回轮询起点：available 中第一个 ID 大于上次选中账户的位置；非轮询策略从头开始
func (set *candidateSet) rotationStartLocked(p *AccountPool) int {
	if p.routingStrategy != "round-robin" {
		return 0
	}
	last := p.lastPicked[set.scope]
	return sort.Search(len(set.available), func(index int) bool {
		return p.accounts[set.available[index]].ID > last
	})
}

// classifyIndexedLocked 用候选集完成不限账户的分类（AllowedAccountIDs 为 nil）；调用方持有 p.mu
func (p *AccountPool) classifyIndexedLocked(
	set *candidateSet, warmAccountIDs []string, now time.Time,
) AccountCandidateGroups {
	index := p.sched
	set.expireLocked(p, now.UnixNano())
	groups := AccountCandidateGroups{Eligible: set.members > 0}
	if until, cooling := set.earliestCoolingLocked(); cooling {
		groups.EarliestCooldown = time.Unix(0, until).UTC()
	}
	generation := index.stampWarmLocked(p, warmAccountIDs)
	available := set.available
	start := set.rotationStartLocked(p)
	limit := int32(p.perAccountConcurrency)
	// 备用账户通常占候选的绝大多数，一次分配够
	groups.StandbyReady = make([]string, 0, len(available))
	for _, position := range available[start:] {
		classifyIndexedCandidate(&groups, index, position, generation, limit)
	}
	for _, position := range available[:start] {
		classifyIndexedCandidate(&groups, index, position, generation, limit)
	}
	return groups
}

// classifyIndexedCandidate 按忙闲数组与热账户代数把候选分进对应分组
func classifyIndexedCandidate(groups *AccountCandidateGroups, index *schedulingIndex, position int, generation uint32, limit int32) {
	isWarm := index.warmStamp[position] == generation
	blocked := index.blocked[position]
	active := index.active[position]
	accountID := index.ids[position]
	switch {
	case isWarm && !blocked && active == 0:
		groups.WarmReady = append(groups.WarmReady, accountID)
	case isWarm && !blocked && active < limit:
		groups.WarmAvailable = append(groups.WarmAvailable, accountID)
	case isWarm:
		groups.WarmBusy = append(groups.WarmBusy, accountID)
	case blocked || active > 0:
		groups.StandbyBusy = append(groups.StandbyBusy, accountID)
	default:
		groups.StandbyReady = append(groups.StandbyReady, accountID)
	}
}

// classifyAllowedLocked 用候选集完成限定账户集合的分类：只看 AllowedAccountIDs 中的账户，顺序与成员判断同不限账户时；
// 调用方持有 p.mu
func (p *AccountPool) classifyAllowedLocked(
	set *candidateSet, allowed map[string]struct{}, warmAccountIDs []string, now time.Time,
) AccountCandidateGroups {
	index := p.sched
	nowNanos := now.UnixNano()
	set.expireLocked(p, nowNanos)
	positions := make([]int, 0, len(allowed))
	for accountID := range allowed {
		if account := p.byID[accountID]; account != nil && set.until[account.position] != 0 {
			positions = append(positions, account.position)
		}
	}
	sort.Slice(positions, func(left, right int) bool { return index.ids[positions[left]] < index.ids[positions[right]] })
	start := 0
	if p.routingStrategy == "round-robin" {
		last := p.lastPicked[set.scope]
		start = sort.Search(len(positions), func(at int) bool { return index.ids[positions[at]] > last })
	}
	generation := index.stampWarmLocked(p, warmAccountIDs)
	limit := int32(p.perAccountConcurrency)
	groups := AccountCandidateGroups{}
	for offset := range positions {
		position := positions[(start+offset)%len(positions)]
		groups.Eligible = true
		if until := set.until[position]; until > nowNanos {
			if candidate := time.Unix(0, until).UTC(); groups.EarliestCooldown.IsZero() || candidate.Before(groups.EarliestCooldown) {
				groups.EarliestCooldown = candidate
			}
			continue
		}
		classifyIndexedCandidate(&groups, index, position, generation, limit)
	}
	return groups
}

// classifyCandidate 按账户字段的忙闲与是否为热账户把候选分进对应分组（逐个账户判断的路径使用）
func classifyCandidate(groups *AccountCandidateGroups, account *Account, warm map[string]struct{}, perAccountConcurrency int) {
	_, isWarm := warm[account.ID]
	blocked := accountBlocked(account)
	switch {
	case isWarm && !blocked && account.active == 0:
		groups.WarmReady = append(groups.WarmReady, account.ID)
	case isWarm && !blocked && account.active < perAccountConcurrency:
		groups.WarmAvailable = append(groups.WarmAvailable, account.ID)
	case isWarm:
		groups.WarmBusy = append(groups.WarmBusy, account.ID)
	case blocked || account.active > 0:
		groups.StandbyBusy = append(groups.StandbyBusy, account.ID)
	default:
		groups.StandbyReady = append(groups.StandbyReady, account.ID)
	}
}

// candidatePositionsLocked 返回候选集的全部成员下标（含冷却中的），按账户 ID 排序；供刷新运行态等批量操作使用
func (p *AccountPool) candidatePositionsLocked(set *candidateSet) []int {
	positions := make([]int, 0, set.members)
	for _, position := range p.sortedAccountIndicesLocked() {
		if position < len(set.until) && set.until[position] != 0 {
			positions = append(positions, position)
		}
	}
	return positions
}

// verifySchedulingIndexLocked 核对候选索引与按账户逐个计算的结果是否一致，供测试在随机变更后检查增量更新；调用方持有 p.mu
func (p *AccountPool) verifySchedulingIndexLocked() error {
	index := p.sched
	if index == nil {
		return nil
	}
	if index.accountsVersion != p.accountsVersion {
		return fmt.Errorf("候选索引账户版本 %d 与账户池 %d 不符", index.accountsVersion, p.accountsVersion)
	}
	if len(index.ids) != len(p.accounts) || len(index.blocked) != len(p.accounts) || len(index.active) != len(p.accounts) {
		return fmt.Errorf("候选索引数组长度与账户数 %d 不符", len(p.accounts))
	}
	for position, account := range p.accounts {
		if account == nil {
			continue
		}
		if account.position != position {
			return fmt.Errorf("账户 %s 的下标 %d 与实际位置 %d 不符", account.ID, account.position, position)
		}
		if index.ids[position] != account.ID {
			return fmt.Errorf("下标 %d 的账户 ID %q，期望 %q", position, index.ids[position], account.ID)
		}
		if index.blocked[position] != accountBlocked(account) || index.active[position] != int32(account.active) {
			return fmt.Errorf("账户 %s 的忙闲数组（阻塞=%v 活动=%d）与账户字段（阻塞=%v 活动=%d）不符",
				account.ID, index.blocked[position], index.active[position], accountBlocked(account), account.active)
		}
		if index.enabled[position] != account.Config.Enabled || index.state[position] != account.State ||
			index.tier[position] != account.BenefitTier ||
			index.globalUntil[position] != cooldownNanos(account.runtime.Cooldowns[globalCooldownKey]) ||
			index.coolUntil[position] != p.accountCoolingUntilLocked(account) {
			return fmt.Errorf("账户 %s 的状态数组（启用=%v 状态=%s 权益=%d 全局冷却=%d 状态冷却=%d）与账户字段不符",
				account.ID, index.enabled[position], index.state[position], index.tier[position],
				index.globalUntil[position], index.coolUntil[position])
		}
	}
	nowNanos := time.Now().UnixNano()
	for key, set := range index.sets {
		if len(set.until) != len(p.accounts) {
			return fmt.Errorf("候选集 %+v 的下标数 %d 与账户数 %d 不符", key, len(set.until), len(p.accounts))
		}
		members := 0
		var available []int
		coolingUntil := make(map[int]int64)
		for position, account := range p.accounts {
			expected := p.candidateStateLocked(set, account)
			actual := set.until[position]
			// 已到期的冷却在下次调度时才移回可用列表：到期前后两种表示都算一致
			if expected != actual && !(expected > 0 && expected <= nowNanos && actual == -1) {
				return fmt.Errorf("候选集 %+v 中账户 %s 的状态 %d，期望 %d", key, account.ID, actual, expected)
			}
			if actual != 0 {
				members++
			}
			if actual == -1 {
				available = append(available, position)
			}
			if actual > 0 {
				coolingUntil[position] = actual
			}
		}
		if members != set.members {
			return fmt.Errorf("候选集 %+v 的成员数 %d，期望 %d", key, set.members, members)
		}
		sort.Slice(available, func(left, right int) bool {
			return p.accounts[available[left]].ID < p.accounts[available[right]].ID
		})
		if !slices.Equal(available, set.available) {
			return fmt.Errorf("候选集 %+v 的可用列表 %v，期望 %v", key, set.available, available)
		}
		for _, entry := range set.cooling {
			if coolingUntil[entry.position] == entry.until {
				delete(coolingUntil, entry.position)
			}
		}
		if len(coolingUntil) > 0 {
			return fmt.Errorf("候选集 %+v 有 %d 个冷却成员不在冷却堆中", key, len(coolingUntil))
		}
	}
	return nil
}
