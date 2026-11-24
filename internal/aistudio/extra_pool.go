package aistudio

import (
	"context"
	"slices"
	"strings"
	"time"
)

// 补充池（额外浏览器号池，EXTRA_POOL）在账户池内的部分
//
// 账户属于哪个号池（家分区）完全不变：补充池只多出一个号池取值 PoolScopeExtra，供借用选号与补充池 Worker 槽位使用。
// 它包含的权益由补充池规则给出（Free、Plus、权益尚未识别的账户、Pro；EXTRA_POOL_INCLUDE_ULTRA 时再加 Ultra），关闭时什么都不包含。
// 权益规则（ULTRA_ONLY_MODELS、PAID_ONLY_MODELS、目录访问模式）照常在 channelSupportsLocked 里按账户真实权益执行，补充池不绕过。
//
// 成员（ExtraMembers、ExtraMemberStates）只描述账户此刻的状态：整户可调度、没有长的全局冷却、至少一个模型在账户可用的每个通道上都在冷却
// （与管理页显示冷却同一口径，accountModelCoolingUntilLocked），同时还有初始化模型能用。完全没有冷却的账户留在主号池，
// 整户不可用（需要登录、停用、组织停用、长的全局冷却）的账户不进补充池

const (
	// extraGlobalExit 为补充池成员留存时容忍的全局冷却：剩余少于它（分钟限额一类的短冷却）不离开，不少于它按整户不可用离开
	extraGlobalExit = 15 * time.Minute
)

// ExtraPoolRules 为补充池在账户池内的规则：Enabled 为 EXTRA_POOL，IncludeUltra 为 EXTRA_POOL_INCLUDE_ULTRA，DefaultPaid 为
// EXTRA_POOL_DEFAULT_PAID（默认地址能否借补充池服务本路由列表之外的模型）。Enabled 为假时规范化为零值
type ExtraPoolRules struct {
	Enabled      bool
	IncludeUltra bool
	DefaultPaid  bool
}

// normalized 返回规范化的规则：关闭时为零值（关闭时切换其余两项不算变化）
func (rules ExtraPoolRules) normalized() ExtraPoolRules {
	if !rules.Enabled {
		return ExtraPoolRules{}
	}
	return rules
}

// tierMask 返回补充池包含的权益格（按 tierSlot 的位掩码）：开启时为全部权益格，不含 Ultra（IncludeUltra 时含）；关闭时为 0
func (rules ExtraPoolRules) tierMask() uint8 {
	if !rules.Enabled {
		return 0
	}
	var mask uint8
	for slot := range tierSlotCount {
		if slot == tierSlot(BenefitTierUltra) && !rules.IncludeUltra {
			continue
		}
		mask |= 1 << slot
	}
	return mask
}

// SetExtraPoolRules 切换补充池规则，返回生效的规则是否变化（规范化后与当前相同时什么也不做）。权益掩码变化（开关或 IncludeUltra）时
// 与号池成员规则变化同类：递增 eligibilityVersion，作废候选索引与状态缓存；任何生效值变化（含 DefaultPaid）都递增 poolRuleVersion
// 并关闭 changed，让按版本缓存的模型列表与别名重建、等待账户的请求立即重查。进行中的租约不受影响，按旧掩码选中、尚未确认的租约由
// refreshAndValidateLease 放弃并重新选号
func (p *AccountPool) SetExtraPoolRules(rules ExtraPoolRules) bool {
	if p == nil {
		return false
	}
	rules = rules.normalized()
	p.mu.Lock()
	defer p.mu.Unlock()
	if rules == p.extraRules {
		return false
	}
	maskChanged := rules.tierMask() != p.extraRules.tierMask()
	p.extraRules = rules
	live := rules
	p.extraLive.Store(&live)
	if maskChanged {
		p.rules.tiers[PoolScopeExtra] = rules.tierMask()
		p.eligibilityVersion++
		p.invalidateSchedulingLocked()
		p.invalidateStatusLocked()
	}
	p.poolRuleVersion.Add(1)
	p.notifyLocked()
	return true
}

// ExtraPoolLive 返回当前生效的补充池规则（无锁读取的副本）；从没设置过时为零值（关闭）
func (p *AccountPool) ExtraPoolLive() ExtraPoolRules {
	if p == nil {
		return ExtraPoolRules{}
	}
	if live := p.extraLive.Load(); live != nil {
		return *live
	}
	return ExtraPoolRules{}
}

// ExtraMember 为补充池的一个成员：Home 为账户的家分区（不变），Until 为它按状态口径冷却到的最晚时刻
type ExtraMember struct {
	ID    string
	Tier  BenefitTier
	Home  PoolScope
	Until time.Time
}

// ExtraExit 为账户是否仍是补充池成员：ExtraStay 为仍是成员，其余为不是成员的原因
type ExtraExit uint8

const (
	// ExtraStay 仍是成员
	ExtraStay ExtraExit = iota
	// ExtraRecovered 模型冷却已结束（或剩余不够进入门槛），账户完全可用，回主号池
	ExtraRecovered
	// ExtraRuleOff 补充池已关闭，或账户的权益不在补充池范围（如关闭 EXTRA_POOL_INCLUDE_ULTRA 后的 Ultra 账户）
	ExtraRuleOff
	// ExtraUnschedulable 账户整户不可调度：停用、需要登录、不可用、组织停用或服务受限
	ExtraUnschedulable
	// ExtraGlobalCooling 全局冷却：进入时有生效的全局冷却，留存时全局冷却剩余不少于 extraGlobalExit
	ExtraGlobalCooling
	// ExtraNoUsableModel 没有能用的初始化模型
	ExtraNoUsableModel
	// ExtraMissing 账户已不在账户池
	ExtraMissing
)

// String 返回离开原因的中文说明（运行日志用）
func (exit ExtraExit) String() string {
	switch exit {
	case ExtraStay:
		return "仍是成员"
	case ExtraRecovered:
		return "冷却已结束"
	case ExtraRuleOff:
		return "补充池已关闭或权益不在补充池范围"
	case ExtraUnschedulable:
		return "账户不可调度"
	case ExtraGlobalCooling:
		return "全局冷却"
	case ExtraNoUsableModel:
		return "没有能用的模型"
	case ExtraMissing:
		return "账户已删除"
	default:
		return ""
	}
}

// ExtraMembers 按进入口径（门槛 entryMin）列出补充池成员，按 Until 从晚到早、ID 升序排列；补充池关闭时直接返回 nil。
// 不在锁内逐个账户深查：前 4 条（权益、整户可调度、没有生效的全局冷却、状态口径冷却剩余不少于门槛）只读候选索引的紧凑数组——
// 全局冷却已过期时 coolUntil 恰好是最晚的按模型状态口径冷却时刻；第 5 条（还有初始化模型能用）读 usable 中各模型在补充池的
// 生成候选集（最多 len(usable) 个，由已有机制增量维护）。只在 keepWarm 周期里调用
func (p *AccountPool) ExtraMembers(now time.Time, entryMin time.Duration, usable []string) []ExtraMember {
	if p == nil {
		return nil
	}
	members := p.extraMembersLocked(now, entryMin, usable)
	// 排序在锁外进行：成员可达上万个
	slices.SortFunc(members, func(a, b ExtraMember) int {
		if c := b.Until.Compare(a.Until); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
	return members
}

// extraMembersLocked 为 ExtraMembers 在账户池锁内的扫描：先只记下标与期限（不含指针，扫描上万个账户时不给 GC 添负担），
// 再按确切的个数构造成员
func (p *AccountPool) extraMembersLocked(now time.Time, entryMin time.Duration, usable []string) []ExtraMember {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.rules.tiers[PoolScopeExtra] == 0 {
		return nil
	}
	index := p.derivedStatesLocked(now)
	sets := p.spareSetsLocked(PoolScopeExtra, usable, now)
	nowNanos, minNanos, shortNanos := now.UnixNano(), int64(entryMin), int64(ShortCooldownMax)
	type hit struct {
		position int
		until    int64
	}
	var hits []hit
	for position := range index.ids {
		if position >= len(p.accounts) || p.accounts[position] == nil || !index.enabled[position] ||
			index.state[position] != AccountReady || !p.tierInPoolLocked(PoolScopeExtra, index.tier[position]) ||
			index.globalUntil[position] > nowNanos {
			continue
		}
		until := index.coolUntil[position]
		if until <= nowNanos || until-nowNanos < minNanos || !anyServeable(sets, position, nowNanos, shortNanos) {
			continue
		}
		hits = append(hits, hit{position: position, until: until})
	}
	if len(hits) == 0 {
		return nil
	}
	members := make([]ExtraMember, len(hits))
	for i, hit := range hits {
		tier := index.tier[hit.position]
		members[i] = ExtraMember{ID: index.ids[hit.position], Tier: tier, Home: p.rules.partitionOf(tier), Until: time.Unix(0, hit.until)}
	}
	return members
}

// anyServeable 判断下标处的账户在任一候选集中是候选，且恢复时间距 now 不足 minNanos
func anyServeable(sets []*candidateSet, position int, nowNanos int64, minNanos int64) bool {
	for _, set := range sets {
		if serveableIn(set, position, nowNanos, minNanos) {
			return true
		}
	}
	return false
}

// ExtraMemberStates 一次加锁，逐个账户精确判断 ids 在门槛 min 下是否仍是补充池成员，不是时给出原因。min 大于 0 为进入口径
// （不能有生效的全局冷却，状态口径冷却剩余不少于 min）；min 为 0 为留存口径（全局冷却剩余少于 extraGlobalExit 不离开，
// 冷却未到期即仍是成员）。“还有初始化模型能用”不计全局冷却：留存时短的全局冷却不让成员离开。
// 只对补充池 Worker（不超过峰值加少量余量）或单个账户调用
func (p *AccountPool) ExtraMemberStates(ids []string, now time.Time, min time.Duration, usable []string) map[string]ExtraExit {
	states := make(map[string]ExtraExit, len(ids))
	if p == nil {
		for _, id := range ids {
			states[id] = ExtraMissing
		}
		return states
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	var sets []*candidateSet
	if p.rules.tiers[PoolScopeExtra] != 0 && len(ids) > 0 {
		sets = p.spareSetsLocked(PoolScopeExtra, usable, now)
	}
	for _, id := range ids {
		states[id] = p.extraExitLocked(p.byID[strings.TrimSpace(id)], now, min, sets)
	}
	return states
}

// extraExitLocked 为 ExtraMemberStates 对一个账户的判断；调用方持有 p.mu
func (p *AccountPool) extraExitLocked(account *Account, now time.Time, min time.Duration, sets []*candidateSet) ExtraExit {
	if account == nil {
		return ExtraMissing
	}
	if p.rules.tiers[PoolScopeExtra] == 0 || !p.tierInPoolLocked(PoolScopeExtra, account.BenefitTier) {
		return ExtraRuleOff
	}
	if !account.Config.Enabled || account.schedulingState() != AccountReady {
		return ExtraUnschedulable
	}
	nowNanos := now.UnixNano()
	if global := cooldownNanos(account.runtime.Cooldowns[globalCooldownKey]); global > nowNanos &&
		(min > 0 || global-nowNanos >= int64(extraGlobalExit)) {
		return ExtraGlobalCooling
	}
	var modelUntil int64
	for key := range account.runtime.Cooldowns {
		if key != globalCooldownKey {
			modelUntil = max(modelUntil, p.accountModelCoolingUntilLocked(account, cooldownModelID(key)))
		}
	}
	if modelUntil <= nowNanos || modelUntil-nowNanos < int64(min) {
		return ExtraRecovered
	}
	shortNanos := int64(ShortCooldownMax)
	for _, set := range sets {
		if until := p.extraModelUntilLocked(set, account); until != 0 && !(until > nowNanos && until-nowNanos >= shortNanos) {
			return ExtraStay
		}
	}
	return ExtraNoUsableModel
}

// extraModelUntilLocked 返回账户在候选集上不计全局冷却的恢复时刻：0 为不是候选，-1 为有支持的通道没有冷却，正数为各支持通道中
// 最早的恢复时刻。与 candidateStateLocked 的通道循环相同，只是不取全局冷却（候选集里的期限含全局冷却）；调用方持有 p.mu
func (p *AccountPool) extraModelUntilLocked(set *candidateSet, account *Account) int64 {
	position := account.position
	if position < 0 || position >= len(set.until) || position >= len(p.accounts) || p.accounts[position] != account ||
		set.until[position] == 0 {
		return 0
	}
	until := int64(-1)
	for _, channel := range set.channels {
		if !p.channelSupportsLocked(account, channel, set.selection) {
			continue
		}
		var channelUntil int64
		if scopeKey := ChannelCooldownScope(channel, set.scope); scopeKey != "" {
			channelUntil = cooldownNanos(account.runtime.Cooldowns[scopeKey])
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

// NewExtraUnavailableError 返回只能由补充池服务的请求此刻借不到补充池 Worker 时的错误：模型在默认地址的模型列表里（Listed），
// 按 503 UNAVAILABLE 与 Retry-After（noServingRetryAfter 之后）返回，对外说明沿用官方英文措辞，不带账户信息
func NewExtraUnavailableError(model string, now time.Time) *NoServingAccountError {
	return &NoServingAccountError{
		Model: strings.TrimPrefix(strings.TrimSpace(model), "models/"), Summary: "补充池此刻没有可借出的 Worker",
		Listed: true, Until: now.Add(noServingRetryAfter),
	}
}

// selectionOverride 为补充池的选择覆盖：号池与可选的账户
type selectionOverride struct {
	pool    PoolScope
	allowed []string
}

type selectionOverrideKey struct{}

// WithSelectionOverride 让 ctx 内不固定账户与资源的选号改在号池 pool 的 allowed 账户里进行（AcquireFor 与换号上限读取）。
// 只供 countTokens 借用补充池使用：默认地址上只能由补充池服务的模型改用补充池里可借出的账户；其他调用方都不读它
func WithSelectionOverride(ctx context.Context, pool PoolScope, allowed []string) context.Context {
	return context.WithValue(ctx, selectionOverrideKey{}, selectionOverride{pool: pool, allowed: slices.Clone(allowed)})
}

// selectionOverrideFrom 返回 ctx 上的选择覆盖
func selectionOverrideFrom(ctx context.Context) (selectionOverride, bool) {
	if ctx == nil {
		return selectionOverride{}, false
	}
	override, ok := ctx.Value(selectionOverrideKey{}).(selectionOverride)
	return override, ok
}

// RoutingFillFirst 判断账户选择策略为优先填满（ROUTING_STRATEGY=fill-first；账户池里不是 round-robin 的都按优先填满选号）：
// 补充池“一起分担”时按两边空闲数轮取，优先填满时固定主号池在前（见 app 的 extra_select.go）。只在要借用补充池时读取
func (p *AccountPool) RoutingFillFirst() bool {
	if p == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.routingStrategy != "round-robin"
}

// HasAccountsIn 判断号池 scope 里有账户（按账户池的号池规则，不看状态与目录）：补充池判断默认地址的号池是不是一个账户都没有
// （例如只有 Pro 与 Ultra 账户、PRO_POOL=true 时），那时只能由补充池服务的请求照样借用（见 app 的 extra_select.go）。只在主分类出错时读取
func (p *AccountPool) HasAccountsIn(scope PoolScope) bool {
	if p == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.hasAccountLocked(scope)
}
