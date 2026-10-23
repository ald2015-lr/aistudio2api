package aistudio

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// DiscoverDirectories 列出账户路径下当前存在的账户目录，返回排序后的绝对路径
//
// 识别规则与 Load 相同；与 Load 不同的是这里只列目录，不解析内容，
// 单个目录损坏或正在复制时不影响其他目录。
func (s *AccountStore) DiscoverDirectories() ([]string, error) {
	if s == nil || len(s.paths) == 0 {
		return nil, fmt.Errorf("账户路径为空")
	}
	directories := make([]string, 0)
	for _, source := range s.paths {
		absolute, err := filepath.Abs(source)
		if err != nil {
			return nil, fmt.Errorf("解析账户路径 %q: %w", source, err)
		}
		info, err := os.Stat(absolute)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("读取账户路径 %q: %w", source, err)
		}
		if !info.IsDir() {
			if filepath.Base(absolute) == storageStateName {
				directories = append(directories, filepath.Dir(absolute))
			}
			continue
		}
		if fileExists(filepath.Join(absolute, storageStateName)) || fileExists(filepath.Join(absolute, accountConfigName)) {
			directories = append(directories, absolute)
			continue
		}
		entries, err := os.ReadDir(absolute)
		if err != nil {
			return nil, fmt.Errorf("扫描账户目录 %q: %w", source, err)
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			name := entry.Name()
			if strings.HasPrefix(name, ".") {
				continue
			}
			directory := filepath.Join(absolute, name)
			if fileExists(filepath.Join(directory, storageStateName)) || fileExists(filepath.Join(directory, accountConfigName)) {
				directories = append(directories, directory)
			}
		}
	}
	sort.Strings(directories)
	return directories, nil
}

// LoadDirectory 从已有账户目录载入单个账户
func (s *AccountStore) LoadDirectory(directory string) (*Account, error) {
	return loadAccount(directory)
}

// LoadTolerant 与 Load 相同，但跳过无法读取、ID 重复或资源冲突的目录并返回这些错误，
// 单个目录损坏或正在复制时不影响其余账户启动
func (s *AccountStore) LoadTolerant() ([]*Account, []error) {
	directories, err := s.DiscoverDirectories()
	if err != nil {
		return nil, []error{err}
	}
	accounts := make([]*Account, 0, len(directories))
	ids := make(map[string]struct{}, len(directories))
	resources := make(map[string]string)
	var failures []error
	for _, directory := range directories {
		account, err := loadAccount(directory)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		if _, exists := ids[account.ID]; exists {
			failures = append(failures, fmt.Errorf("账户 ID 重复: %s", account.ID))
			continue
		}
		conflict := ""
		for resourceID := range account.runtime.Resources {
			if owner, exists := resources[resourceID]; exists {
				conflict = fmt.Sprintf("资源 %s 同时绑定账户 %s 和 %s", resourceID, owner, account.ID)
				break
			}
		}
		if conflict != "" {
			failures = append(failures, errors.New(conflict))
			continue
		}
		ids[account.ID] = struct{}{}
		for resourceID := range account.runtime.Resources {
			resources[resourceID] = account.ID
		}
		accounts = append(accounts, account)
	}
	return accounts, failures
}

// SetPerAccountConcurrency 运行中调整单账户并发上限，并唤醒等待槽位的请求
func (p *AccountPool) SetPerAccountConcurrency(limit int) {
	if limit < 1 {
		limit = 1
	}
	p.mu.Lock()
	p.perAccountConcurrency = limit
	p.notifyLocked()
	p.mu.Unlock()
}

// ClassifyCandidatesCached 只按内存中的账户状态分类候选，不读磁盘、不等待运行态文件锁。
// 供预热这类可以容忍状态稍旧的场景使用：启动阶段大量账户正在同步目录并持有锁，
// 带刷新的 ClassifyCandidates 会被逐个拖住，导致预热长时间停滞
func (p *AccountPool) ClassifyCandidatesCached(
	selection AccountSelection,
	warmAccountIDs []string,
) (AccountCandidateGroups, error) {
	if p == nil {
		return AccountCandidateGroups{}, ErrNoEligibleAccount
	}
	selection.ModelID = strings.TrimPrefix(strings.TrimSpace(selection.ModelID), "models/")
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.classifyCandidatesLocked(selection, warmAccountIDs)
}

// BootstrapSummary 为一次加锁得到的预热概况
type BootstrapSummary struct {
	// Available 为启用、就绪或忙碌且有 WAA 初始化模型的账户数（与 Status 的状态推导一致）
	Available int
	// UltraAvailable 为 Available 中属于 Ultra 号池的账户数（Ultra Worker 分区的预热目标上限）
	UltraAvailable int
	// ModelIDs 为全部账户出现过的初始化模型，去重并按首次出现顺序排列
	ModelIDs []string
}

// BootstrapSummary 在一次加锁内统计全部账户的预热概况。
// 逐个账户调用 BootstrapModels 需要对几百个账户分别加锁，启动阶段账户池锁竞争激烈时
// 每次加锁都要排队，整体会被拖到几十秒，导致预热循环与状态接口长时间卡住
func (p *AccountPool) BootstrapSummary() BootstrapSummary {
	if p == nil {
		return BootstrapSummary{}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	index := p.derivedStatesLocked(now)
	nowNanos := now.UnixNano()
	summary := BootstrapSummary{}
	seen := make(map[string]struct{})
	for position, account := range p.accounts {
		if account == nil {
			continue
		}
		models := accountBootstrapModelsLocked(account)
		for _, modelID := range models {
			if _, exists := seen[modelID]; !exists {
				seen[modelID] = struct{}{}
				summary.ModelIDs = append(summary.ModelIDs, modelID)
			}
		}
		if len(models) == 0 || !account.Config.Enabled {
			continue
		}
		// 与 Status 共用状态推导（按候选索引的紧凑数组）：需要登录的账户即使还有未结束的租约也不计入可预热
		if state := index.derivedStateAt(position, nowNanos); state == AccountReady || state == AccountBusy {
			summary.Available++
			if ultraPoolTier(account.BenefitTier, p.ultraIncludePro) {
				summary.UltraAvailable++
			}
		}
	}
	return summary
}

// PrimaryDirectory 返回第一个账户路径对应的目录（绝对路径），用于保存账户集合级别的状态文件
func (s *AccountStore) PrimaryDirectory() string {
	if s == nil || len(s.paths) == 0 {
		return ""
	}
	absolute, err := filepath.Abs(s.paths[0])
	if err != nil {
		return ""
	}
	if info, err := os.Stat(absolute); err == nil && !info.IsDir() {
		return filepath.Dir(absolute)
	}
	return absolute
}

const (
	// selectionRefreshInterval 为同一模型两次从磁盘刷新全部账户运行态的最短间隔
	selectionRefreshInterval = 3 * time.Second
	// 账户数每超过 selectionRefreshScaleAccounts 一档，刷新间隔多加一个 selectionRefreshInterval，最长 selectionRefreshMaxInterval：
	// 上万账户一次刷新就是上万次文件锁与读取，全部冷却时每个排队请求都会触发
	selectionRefreshScaleAccounts = 1000
	selectionRefreshMaxInterval   = 30 * time.Second
)

// selectionRefreshIntervalFor 返回账户数对应的刷新间隔
func selectionRefreshIntervalFor(accounts int) time.Duration {
	interval := selectionRefreshInterval * time.Duration(1+accounts/selectionRefreshScaleAccounts)
	return min(interval, selectionRefreshMaxInterval)
}

// refreshSelectionRuntimesThrottled 限频的运行态刷新。
// 所有候选都没有空位且有账户在冷却时，调度会从磁盘重读该模型全部账户的运行态（每个账户一次文件锁与读取），
// 用于感知其他进程改动的冷却。高并发时每个排队请求每秒都会触发一次，几百个账户就是每秒成千上万次文件操作，
// 并与账户池锁激烈竞争；单进程部署时内存状态本就是最新的，因此同一模型最多每 3 秒刷新一次，账户上万时间隔再按账户数放大
func (p *AccountPool) refreshSelectionRuntimesThrottled(ctx context.Context, selection AccountSelection) error {
	// 各号池只刷新自己的账户，限频按号池分开，避免 Ultra 请求的刷新挡住普通号池的刷新
	key := strings.TrimPrefix(strings.TrimSpace(selection.ModelID), "models/") + "|" + strings.TrimSpace(selection.AccountID) +
		"|" + selection.Pool.String()
	now := time.Now()
	p.mu.Lock()
	interval := selectionRefreshIntervalFor(len(p.accounts))
	p.mu.Unlock()
	p.selectionRefreshMu.Lock()
	if last, ok := p.selectionRefreshAt[key]; ok && now.Sub(last) < interval {
		p.selectionRefreshMu.Unlock()
		return nil
	}
	if p.selectionRefreshAt == nil {
		p.selectionRefreshAt = make(map[string]time.Time)
	}
	p.selectionRefreshAt[key] = now
	p.selectionRefreshMu.Unlock()
	return p.refreshSelectionRuntimes(ctx, selection)
}

// AccountStateCounts 为各状态的账户数量
type AccountStateCounts struct {
	Total, Ready, Busy, Cooldown, AuthRequired int
}

// StateCounts 一次加锁统计各状态账户数，不构建完整状态。
// 状态接口每几秒被管理页调用一次；账户上千时构建完整状态（逐个排序模型、拷贝冷却表）会长时间占用账户池锁
func (p *AccountPool) StateCounts() AccountStateCounts {
	counts, _ := p.PoolStateCounts()
	return counts
}

// PoolStateCounts 一次加锁统计全部账户与其中 Ultra 号池账户的各状态数量
func (p *AccountPool) PoolStateCounts() (AccountStateCounts, AccountStateCounts) {
	var counts, ultra AccountStateCounts
	if p == nil {
		return counts, ultra
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	// 按候选索引的紧凑数组推导状态（与 accountStateLocked 相同），状态接口每几秒调用一次，不再逐个访问账户结构
	now := time.Now()
	index := p.derivedStatesLocked(now)
	nowNanos := now.UnixNano()
	for position := range index.ids {
		if p.accounts[position] == nil {
			continue
		}
		state := index.derivedStateAt(position, nowNanos)
		counts.add(state)
		if ultraPoolTier(index.tier[position], p.ultraIncludePro) {
			ultra.add(state)
		}
	}
	return counts, ultra
}

// add 计入一个账户的对外状态
func (counts *AccountStateCounts) add(state AccountState) {
	counts.Total++
	switch state {
	case AccountReady:
		counts.Ready++
	case AccountBusy:
		counts.Busy++
	case AccountCooldown:
		counts.Cooldown++
	case AccountAuthRequired:
		counts.AuthRequired++
	}
}

// StatusOf 返回单个账户的状态，只构建这一个账户
func (p *AccountPool) StatusOf(accountID string) (AccountStatus, bool) {
	if p == nil {
		return AccountStatus{}, false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	account := p.byID[strings.TrimSpace(accountID)]
	if account == nil {
		return AccountStatus{}, false
	}
	return p.accountStatusLocked(account, time.Now()), true
}

// Labels 一次加锁返回这些账户的显示名称，不存在的账户不返回。管理页列出一批账户时用它，不为每个账户构建一次完整状态
func (p *AccountPool) Labels(accountIDs []string) map[string]string {
	labels := make(map[string]string, len(accountIDs))
	if p == nil {
		return labels
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, accountID := range accountIDs {
		if account := p.byID[accountID]; account != nil {
			labels[accountID] = account.Config.Label
		}
	}
	return labels
}

// longCoolingLocked 判断账户在该模型上是否长时间不可用：全局冷却，或所有启用通道都在冷却，
// 且剩余时间不少于 minRemaining（排除一分钟内就恢复的分钟限额）；modelID 为空时只看全局冷却。调用方持有 p.mu。
// 这是 Worker 淘汰与轮换的口径（针对指定模型、不看通道是否支持该模型、有剩余时间门槛）；状态页与状态摘要的 cooldown
// 另按 accountCoolingLocked 判断（任一模型、只看账户可用的通道、不设门槛），两者有意不同
func (p *AccountPool) longCoolingLocked(account *Account, modelID string, now time.Time, minRemaining time.Duration) bool {
	if longCooldownActive(account, globalCooldownKey, now, minRemaining) {
		return true
	}
	if modelID == "" {
		return false
	}
	playground := !p.channelEnabledLocked(ChannelPlayground) || longCooldownActive(account, modelID, now, minRemaining)
	build := !p.channelEnabledLocked(ChannelBuild) ||
		longCooldownActive(account, ChannelCooldownScope(ChannelBuild, modelID), now, minRemaining)
	return playground && build
}

// longCooldownActive 判断账户某个冷却键是否生效且剩余不少于 minRemaining
func longCooldownActive(account *Account, key string, now time.Time, minRemaining time.Duration) bool {
	cooldown, exists := account.runtime.Cooldowns[key]
	return exists && cooldown.Active(now) && cooldown.Until.Sub(now) >= minRemaining
}

// AccountLongCooling 判断单个账户当前是否长时间不可用：全局冷却，或在该模型上所有启用通道都冷却，剩余不少于 minRemaining；
// modelID 为空时只看全局冷却。模型按账户目录归一（与 MarkCooldownIfGeneration 写入的键一致）。
// 账号刚进入每日限额时调度层用它判断该账号的驻留 Worker 是否已成死槽位
func (p *AccountPool) AccountLongCooling(accountID string, modelID string, minRemaining time.Duration) bool {
	if p == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	account := p.byID[strings.TrimSpace(accountID)]
	if account == nil {
		return false
	}
	modelID = strings.TrimPrefix(strings.TrimSpace(modelID), "models/")
	if modelID != "" {
		modelID = canonicalAccountModelID(account, modelID)
	}
	return p.longCoolingLocked(account, modelID, time.Now(), minRemaining)
}

// WarmWorkerState 为淘汰选号用的一个驻留 Worker 账户的调度状态
type WarmWorkerState struct {
	// Busy 为账户有进行中的请求、独占操作或认证续签，LastUsed 为最近活动时间：最近一次取得租约与最近一次请求结束（普通租约全部释放）
	// 中较晚的一个，刚结束长流的账户不算空闲了整条流的时长
	Busy     bool
	LastUsed time.Time
	// Cooling 为账户冷却中、且剩余不少于 ShortCooldownMax（分钟限额、限流第一档这类一分钟内就恢复的短冷却不算）：全局冷却，
	// 或该模型生成请求所有支持的启用通道都在冷却（按 Build 键 build:<模型> 与 Playground 键分别判断，取最早恢复的通道）；
	// 只有一个通道冷却、另一支持的通道可用时不算（它还能服务该模型），账户对该模型没有任何支持的通道时也不算。
	// 账户目录里有该模型、但它不支持 generateContent（视频、Live 等）时没有生成通道可判断，按 Playground 的模型键与全局冷却判断。
	// Dead 为长时间不可用（全局冷却或该模型所有启用通道都冷却，不看通道是否支持该模型，与淘汰、补登记、轮换的死槽位口径一致），
	// 这样的 Worker 对该模型是死槽位
	Cooling bool
	Dead    bool
	// Unschedulable 为账户已不能服务任何模型的原因（见 unschedulableReasonLocked），空为可调度：这样的驻留 Worker
	// 不是任何模型的候选，却占着驻留槽位，淘汰选号排在死槽位之前
	Unschedulable string
	// Unservable 为账户对该模型没有任何可用的目录与通道（目录里没有它，或权益、通道不支持）：modelID 为空时为假。
	// 同分区有 Worker 在启动时，按需替换也可以换掉这样的健康空闲 Worker（它服务不了这个请求，见 app 的 victimReplaceable）
	Unservable bool
}

// 账户不可调度的原因（WarmWorkerState.Unschedulable、UnschedulableSet），调度层写进“WAA Worker 释放 | 原因=”
const (
	UnschedulableDisabled     = "已停用"
	UnschedulableAuthRequired = "需要登录"
	UnschedulableUnavailable  = "不可用"
	UnschedulableNotReady     = "不可调度"
	UnschedulableNoCatalog    = "没有模型目录"
)

// unschedulableReasonLocked 返回账户不能服务任何模型的原因，空为可调度；调用方持有 p.mu。
// 停用、需要登录、不可用与其他非就绪状态的账户拿不到不固定账户的租约；目录加载过（initializedAt 非零）却没有可初始化的模型时
// 同样不是任何模型的候选（目录被同步成空、权益变化后没有可用模型）。只看“目录加载过”：测试里手工标热、从没设过目录的账户不算
func unschedulableReasonLocked(account *Account) string {
	switch {
	case !account.Config.Enabled:
		return UnschedulableDisabled
	case account.State == AccountAuthRequired:
		return UnschedulableAuthRequired
	case account.State == AccountUnavailable:
		return UnschedulableUnavailable
	case account.State != AccountReady:
		return UnschedulableNotReady
	case !account.initializedAt.IsZero() && len(accountBootstrapModelsLocked(account)) == 0:
		return UnschedulableNoCatalog
	}
	return ""
}

// UnschedulableSet 一次加锁返回给定账户中不可调度的账户与原因（可调度与不存在的账户不返回），
// 供驻留 Worker 的补登记释放与状态计数使用
func (p *AccountPool) UnschedulableSet(accountIDs []string) map[string]string {
	result := make(map[string]string)
	if p == nil {
		return result
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, accountID := range accountIDs {
		account := p.byID[accountID]
		if account == nil {
			continue
		}
		if reason := unschedulableReasonLocked(account); reason != "" {
			result[accountID] = reason
		}
	}
	return result
}

// WarmWorkerStates 一次加锁返回给定账户的调度状态，供淘汰选号使用：原先每个驻留 Worker 分别查询活动与冷却，
// 一次选号要对账户池加锁上百次，账户池锁争用时持有全局调度锁的替换流程会被拖住很久。modelID 为空时只看全局冷却
func (p *AccountPool) WarmWorkerStates(accountIDs []string, modelID string, minRemaining time.Duration) map[string]WarmWorkerState {
	states := make(map[string]WarmWorkerState, len(accountIDs))
	if p == nil {
		return states
	}
	modelID = strings.TrimPrefix(strings.TrimSpace(modelID), "models/")
	// 按该模型生成请求的通道判断冷却；modelID 为空时只走 Playground、作用域为空，只看全局冷却
	selection := AccountSelection{ModelID: modelID, Method: "generateContent"}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	for _, accountID := range accountIDs {
		account := p.byID[accountID]
		if account == nil {
			continue
		}
		until, cooling := p.accountChannelCooldownLocked(account, selection, now)
		if !cooling && modelID != "" && !p.accountSupportsAnyChannelLocked(account, selection) &&
			accountSupportsSelection(account, AccountSelection{ModelID: modelID}) {
			// 账户能服务该模型、只是它不走 generateContent（视频、Live 等，请求按模型取 Worker）：退回原先按模型键的口径，
			// 否则在该模型上冷却的空闲 Worker 不算冷却中，分区满时不被替换
			if cooldown, active := accountCooldown(account, canonicalAccountModelID(account, modelID), now); active {
				until, cooling = cooldown.Until, true
			}
		}
		states[accountID] = WarmWorkerState{
			Busy:     account.exclusive || account.exclusiveWaiters > 0 || account.authRefreshers > 0 || account.active > 0,
			LastUsed: account.lastActivityLocked(),
			// 剩余不到 ShortCooldownMax 的冷却（分钟限额、限流第一档）不算冷却中：一分钟内就恢复，关掉它换一个要冷启动 10–30 秒的账号得不偿失
			Cooling: longCooldownActive(account, globalCooldownKey, now, ShortCooldownMax) ||
				cooling && until.Sub(now) >= ShortCooldownMax,
			Dead:          p.longCoolingLocked(account, modelID, now, minRemaining),
			Unschedulable: unschedulableReasonLocked(account),
			Unservable: modelID != "" && !p.accountSupportsAnyChannelLocked(account, selection) &&
				!accountSupportsSelection(account, AccountSelection{ModelID: modelID}),
		}
	}
	return states
}

// cooldownModelID 从冷却键中取出模型：Playground 键即模型名，Build 键为 build:<模型>
func cooldownModelID(key string) string {
	if key == globalCooldownKey {
		return ""
	}
	return strings.TrimPrefix(key, string(ChannelBuild)+":")
}

// LongCooldownModels 统计给定账户在各模型上长时间不可用的账户数，一次加锁
func (p *AccountPool) LongCooldownModels(accountIDs []string, minRemaining time.Duration) map[string]int {
	counts := make(map[string]int)
	if p == nil {
		return counts
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	for _, accountID := range accountIDs {
		account := p.byID[accountID]
		if account == nil {
			continue
		}
		seen := make(map[string]struct{})
		for key := range account.runtime.Cooldowns {
			modelID := cooldownModelID(key)
			if modelID == "" {
				continue
			}
			if _, done := seen[modelID]; done {
				continue
			}
			seen[modelID] = struct{}{}
			if p.longCoolingLocked(account, modelID, now, minRemaining) {
				counts[modelID]++
			}
		}
	}
	return counts
}

// LongCoolingSet 返回给定账户中长时间不可用的账户，一次加锁：全局长时间冷却的账户总是计入，
// 另外计入在任一指定模型上长时间不可用的账户（modelIDs 为空时只看全局冷却）；idleOnly 时只返回没有进行中请求的
func (p *AccountPool) LongCoolingSet(accountIDs []string, modelIDs []string, minRemaining time.Duration, idleOnly bool) map[string]struct{} {
	result := make(map[string]struct{})
	if p == nil {
		return result
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	for _, accountID := range accountIDs {
		account := p.byID[accountID]
		if account == nil {
			continue
		}
		if idleOnly && (account.active > 0 || account.exclusive || account.exclusiveWaiters > 0) {
			continue
		}
		if p.longCoolingLocked(account, "", now, minRemaining) {
			result[accountID] = struct{}{}
			continue
		}
		for _, modelID := range modelIDs {
			if p.longCoolingLocked(account, modelID, now, minRemaining) {
				result[accountID] = struct{}{}
				break
			}
		}
	}
	return result
}

// SpareAccounts 统计号池内可以接替的账户数：不在 exclude 中、启用且就绪、有 WAA 初始化模型、没有全局长时间冷却，
// 并且对每个指定模型都能接替：是该模型生成请求的候选（属于号池、至少一个启用通道的目录支持它），支持的通道中至少有一个
// 没有长时间冷却（按候选集的恢复时间），也不按淘汰口径在该模型上长时间不可用（见 longCoolingLocked）。
// 不支持指定模型的账户不算可接替：换上它同样服务不了该模型，等于换上另一个死槽位。一次加锁，数到 limit 个就停止
// （limit 不大于 0 时数完全部）：轮换与淘汰只需要知道“有没有 N 个可接替的账户”，账户上万时不必在锁内扫完全部。
// 停用、未就绪、不在号池与全局长时间冷却的账户先按候选索引的紧凑数组排除，指定模型的支持与冷却按候选集的下标数组判断
// （上万账户、大半处于每日限额时只剩少数要逐个判断）
func (p *AccountPool) SpareAccounts(scope PoolScope, exclude map[string]struct{}, modelIDs []string, minRemaining time.Duration, limit int) int {
	if p == nil {
		return 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.spareAccountsAtLocked(scope, exclude, modelIDs, minRemaining, limit, time.Now())
}

// spareAccountsAtLocked 按 now 时刻统计可接替的账户数（见 SpareAccounts）；调用方持有 p.mu
func (p *AccountPool) spareAccountsAtLocked(
	scope PoolScope, exclude map[string]struct{}, modelIDs []string, minRemaining time.Duration, limit int, now time.Time,
) int {
	index := p.schedulingLocked()
	nowNanos, minNanos := now.UnixNano(), int64(max(minRemaining, 0))
	sets := p.spareSetsLocked(scope, modelIDs, now)
	spare := 0
	for position, account := range p.accounts {
		if limit > 0 && spare >= limit {
			break
		}
		if account == nil || !index.enabled[position] || index.state[position] != AccountReady || !p.tierInPoolLocked(scope, index.tier[position]) {
			continue
		}
		if global := index.globalUntil[position]; global > nowNanos && global-nowNanos >= minNanos {
			continue
		}
		if !spareForSets(sets, position, nowNanos, minNanos) {
			continue
		}
		if _, excluded := exclude[account.ID]; excluded || len(accountBootstrapModelsLocked(account)) == 0 {
			continue
		}
		cooling := p.longCoolingLocked(account, "", now, minRemaining)
		for _, modelID := range modelIDs {
			if cooling {
				break
			}
			cooling = p.longCoolingLocked(account, modelID, now, minRemaining)
		}
		if !cooling {
			spare++
		}
	}
	return spare
}

// ServeableCounts 返回 accountIDs 中每个账户能服务 modelIDs 里几个模型：账户在该模型生成请求的候选集里（目录、权益、启用通道都支持，
// 按账户所属号池判断），且至少一个支持它的通道没有长时间冷却（剩余不少于 minRemaining 的冷却视为不能服务），全局长时间冷却的账户计 0。
// 只看冷却键的 LongCoolingSet 会把目录里根本没有该模型的账户当成“没冷却”，预热与备用账号排序据此会启动一个服务不了主用模型的 Worker；
// 这里按候选集成员判断。号池不认识的模型对所有账户都计 0，不影响排序。单次加锁，候选集已缓存时每个账户是常数时间
func (p *AccountPool) ServeableCounts(accountIDs []string, modelIDs []string, minRemaining time.Duration) map[string]int {
	counts := make(map[string]int, len(accountIDs))
	if p == nil || len(accountIDs) == 0 || len(modelIDs) == 0 {
		return counts
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	index := p.schedulingLocked()
	nowNanos, minNanos := now.UnixNano(), int64(max(minRemaining, 0))
	sets := make(map[PoolScope][]*candidateSet, 2)
	for _, accountID := range accountIDs {
		account := p.byID[strings.TrimSpace(accountID)]
		if account == nil || !index.positionValid(p, account) {
			continue
		}
		position := account.position
		if global := index.globalUntil[position]; global > nowNanos && global-nowNanos >= minNanos {
			counts[account.ID] = 0
			continue
		}
		// 索引只存真实权益，号池由权益加 ULTRA_INCLUDE_PRO 推导：并入的 Pro 按 Ultra 号池的候选集计分，
		// 否则独占模式下它在普通候选集里得 0 分，永远不会为 Ultra 主用模型预热
		scope := p.poolOfTierLocked(index.tier[position])
		scoped, built := sets[scope]
		if !built {
			scoped = p.spareSetsLocked(scope, modelIDs, now)
			sets[scope] = scoped
		}
		served := 0
		for _, set := range scoped {
			if serveableIn(set, position, nowNanos, minNanos) {
				served++
			}
		}
		counts[account.ID] = served
	}
	return counts
}

// CoolingCandidateCount 返回 accountIDs 中有几个账户是选择条件的候选、且所有支持的启用通道都在冷却（含剩余不到 ShortCooldownMax 的
// 短冷却），口径与调度分类一致（候选集的恢复时间，见 candidateStateLocked）：分类时这些账户不进任何分组，主用模型供给的 warm 与
// standby 都不计它们。不是候选（不在号池、目录或启用通道不支持、停用、未就绪）的账户不计。单次加锁，候选集已缓存时每个账户是常数时间
func (p *AccountPool) CoolingCandidateCount(selection AccountSelection, accountIDs []string) int {
	if p == nil || len(accountIDs) == 0 {
		return 0
	}
	selection.ModelID = strings.TrimPrefix(strings.TrimSpace(selection.ModelID), "models/")
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	nowNanos := now.UnixNano()
	set := p.candidateSetLocked(selection, now)
	set.expireLocked(p, nowNanos)
	count := 0
	for _, accountID := range accountIDs {
		account := p.byID[strings.TrimSpace(accountID)]
		if account == nil {
			continue
		}
		// 候选集里 0 为非候选、-1 为可用、正数为冷却到期时刻
		if position := account.position; position >= 0 && position < len(set.until) && p.accounts[position] == account &&
			set.until[position] > nowNanos {
			count++
		}
	}
	return count
}

// spareSetsLocked 返回各指定模型生成请求在号池内的候选集，供判断账户能否接替；调用方持有 p.mu
func (p *AccountPool) spareSetsLocked(scope PoolScope, modelIDs []string, now time.Time) []*candidateSet {
	sets := make([]*candidateSet, 0, len(modelIDs))
	for _, modelID := range modelIDs {
		modelID = strings.TrimPrefix(strings.TrimSpace(modelID), "models/")
		if modelID == "" {
			continue
		}
		sets = append(sets, p.candidateSetLocked(AccountSelection{ModelID: modelID, Method: "generateContent", Pool: scope}, now))
	}
	return sets
}

// spareForSets 判断下标处的账户在每个候选集中都是候选，且恢复时间距 now 不足 minNanos（未冷却、已到期或很快恢复）
func spareForSets(sets []*candidateSet, position int, nowNanos int64, minNanos int64) bool {
	for _, set := range sets {
		if !serveableIn(set, position, nowNanos, minNanos) {
			return false
		}
	}
	return true
}

// serveableIn 判断下标处的账户是候选集的成员，且恢复时间距 now 不足 minNanos（候选集里 0 为非成员、-1 为可用、正数为冷却到期时刻）
func serveableIn(set *candidateSet, position int, nowNanos int64, minNanos int64) bool {
	if position < 0 || position >= len(set.until) {
		return false
	}
	until := set.until[position]
	return until != 0 && !(until > nowNanos && until-nowNanos >= minNanos)
}
