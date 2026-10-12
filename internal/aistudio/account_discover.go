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
			if account.BenefitTier == BenefitTierUltra {
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
		if index.tier[position] == BenefitTierUltra {
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
	// Busy 为账户有进行中的请求、独占操作或认证续签，LastUsed 为最近使用时间
	Busy     bool
	LastUsed time.Time
	// Cooling 为账户在该模型上（或全局）冷却中，不论剩余多久；Dead 为长时间不可用（全局冷却或该模型所有启用通道都冷却），
	// 这样的 Worker 对该模型是死槽位
	Cooling bool
	Dead    bool
}

// WarmWorkerStates 一次加锁返回给定账户的调度状态，供淘汰选号使用：原先每个驻留 Worker 分别查询活动与冷却，
// 一次选号要对账户池加锁上百次，账户池锁争用时持有全局调度锁的替换流程会被拖住很久。modelID 为空时只看全局冷却
func (p *AccountPool) WarmWorkerStates(accountIDs []string, modelID string, minRemaining time.Duration) map[string]WarmWorkerState {
	states := make(map[string]WarmWorkerState, len(accountIDs))
	if p == nil {
		return states
	}
	modelID = strings.TrimPrefix(strings.TrimSpace(modelID), "models/")
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	for _, accountID := range accountIDs {
		account := p.byID[accountID]
		if account == nil {
			continue
		}
		_, cooling := accountCooldown(account, modelID, now)
		states[accountID] = WarmWorkerState{
			Busy:     account.exclusive || account.exclusiveWaiters > 0 || account.authRefreshers > 0 || account.active > 0,
			LastUsed: account.LastUsed,
			Cooling:  cooling,
			Dead:     p.longCoolingLocked(account, modelID, now, minRemaining),
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
// 并且没有在任一指定模型上长时间不可用，一次加锁。数到 limit 个就停止（limit 不大于 0 时数完全部）：
// 轮换与淘汰只需要知道“有没有 N 个可接替的账户”，账户上万时不必在锁内扫完全部。
// 停用、未就绪、不在号池与全局长时间冷却的账户先按候选索引的紧凑数组排除（上万账户、大半处于每日限额时只剩少数要逐个判断）
func (p *AccountPool) SpareAccounts(scope PoolScope, exclude map[string]struct{}, modelIDs []string, minRemaining time.Duration, limit int) int {
	if p == nil {
		return 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	index := p.schedulingLocked()
	nowNanos, minNanos := now.UnixNano(), int64(max(minRemaining, 0))
	spare := 0
	for position, account := range p.accounts {
		if limit > 0 && spare >= limit {
			break
		}
		if account == nil || !index.enabled[position] || index.state[position] != AccountReady || !scope.allowsTier(index.tier[position]) {
			continue
		}
		if global := index.globalUntil[position]; global > nowNanos && global-nowNanos >= minNanos {
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
