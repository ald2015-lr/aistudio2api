package aistudio

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"
)

// PoolScope 表示一次请求可以使用的账户号池。号池按账户当前权益、PRO_POOL 与 ULTRA_INCLUDE_PRO 划分（唯一的判定见 poolOfTier）：
// 权益为 Ultra 的账户组成 Ultra 号池；PRO_POOL=true（默认）时权益为 Pro 的账户单独组成 Pro 号池，PRO_POOL=false 时
// Pro 账户在 ULTRA_INCLUDE_PRO=true 时并入 Ultra 号池，否则留在普通号池；其余账户（Free、Plus 与权益尚未识别的账户）组成普通号池。
// 号池只决定账户服务哪条路径，账户权益本身不变：权益请求头、模型资格与输入上限仍按真实权益。
// /ultra 前缀的请求只用 Ultra 号池，/pro 前缀的请求只用 Pro 号池；ULTRA_EXCLUSIVE=true（默认）时普通路径只用普通号池，
// 为 false 时普通路径不限号池（PRO_POOL=true 时为普通号池与 Ultra 号池，Pro 账户仍只服务 /pro）。
// 预热、轮换、登录检查等不和请求关联的后台任务不限号池。Worker 分区与号池一致（普通分区、Ultra 分区与 Pro 分区），
// 用 PoolScopeNormal、PoolScopeUltra 与 PoolScopePro 表示。取值只追加不插入：thoughtsig 按数值区分号池，api 按数值下标缓存别名
type PoolScope uint8

const (
	// PoolScopeAll 不限号池
	PoolScopeAll PoolScope = iota
	// PoolScopeNormal 只用普通号池
	PoolScopeNormal
	// PoolScopeUltra 只用 Ultra 号池
	PoolScopeUltra
	// PoolScopePro 只用 Pro 号池（PRO_POOL=true 时权益为 Pro 的账户）
	PoolScopePro
	// PoolScopeNormalUltra 只用普通号池与 Ultra 号池：PRO_POOL=true、ULTRA_EXCLUSIVE=false 时普通路径的范围，只是请求范围，不是 Worker 分区
	PoolScopeNormalUltra
	// PoolScopeCount 为号池取值的个数，按号池下标的数组用它定长
	PoolScopeCount
)

// String 返回号池的稳定名称：ultra、normal、pro、normal_ultra，不限号池为空。名称进调度队列键、刷新节流键、降级记录键、
// 轮换登记键、主用模型供给与账户状态，不认识的取值同样为空
func (scope PoolScope) String() string {
	switch scope {
	case PoolScopeUltra:
		return "ultra"
	case PoolScopeNormal:
		return "normal"
	case PoolScopePro:
		return "pro"
	case PoolScopeNormalUltra:
		return "normal_ultra"
	default:
		return ""
	}
}

// Label 返回号池的中文名称，用于错误原因与运行日志；不限号池为空
func (scope PoolScope) Label() string {
	switch scope {
	case PoolScopeUltra:
		return "Ultra 号池"
	case PoolScopeNormal:
		return "普通号池"
	case PoolScopePro:
		return "Pro 号池"
	case PoolScopeNormalUltra:
		return "普通号池与 Ultra 号池"
	default:
		return ""
	}
}

// IsPartition 判断号池是否是 Worker 分区（普通、Ultra 或 Pro）：账户总是恰好属于其中一个
func (scope PoolScope) IsPartition() bool {
	return scope == PoolScopeNormal || scope == PoolScopeUltra || scope == PoolScopePro
}

// partitionBit 返回分区在号池分区掩码里的位
func partitionBit(partition PoolScope) uint8 {
	return 1 << partition
}

// scopePartitions 为每个号池包含的分区（按 partitionBit 的掩码）；不在表里的取值掩码为 0，什么都不包含（失败时关闭，
// 不会把漏掉的取值当成不限号池）
var scopePartitions = [PoolScopeCount]uint8{
	PoolScopeAll:         partitionBit(PoolScopeNormal) | partitionBit(PoolScopeUltra) | partitionBit(PoolScopePro),
	PoolScopeNormal:      partitionBit(PoolScopeNormal),
	PoolScopeUltra:       partitionBit(PoolScopeUltra),
	PoolScopePro:         partitionBit(PoolScopePro),
	PoolScopeNormalUltra: partitionBit(PoolScopeNormal) | partitionBit(PoolScopeUltra),
}

// Contains 判断号池是否包含分区 partition（Worker 分区）；不认识的号池与非分区取值都为否
func (scope PoolScope) Contains(partition PoolScope) bool {
	return scope < PoolScopeCount && partition.IsPartition() && scopePartitions[scope]&partitionBit(partition) != 0
}

// poolOfTier 返回权益为 tier 的账户按开关所属的号池（Worker 分区），号池成员只由这里决定，权益本身不变：
// Ultra 总是 Ultra 号池；Pro 在 proPool（PRO_POOL）时为 Pro 号池，否则 includePro（ULTRA_INCLUDE_PRO）时并入 Ultra 号池；
// 其余（Free、Plus、尚未识别权益的账户按 Free）为普通号池。proPool 为假时与只有两个号池时的规则相同。
// 不叫 paid：Model.Paid 是模型属性，同名会把两者混淆
func poolOfTier(tier BenefitTier, includePro bool, proPool bool) PoolScope {
	switch {
	case tier == BenefitTierUltra:
		return PoolScopeUltra
	case tier == BenefitTierPro && proPool:
		return PoolScopePro
	case tier == BenefitTierPro && includePro:
		return PoolScopeUltra
	default:
		return PoolScopeNormal
	}
}

// tierSlotCount 为号池规则按权益建表的格数：四个已知权益（取值 0～3）各一格，其余取值（不会出现，按普通号池）共用最后一格
const tierSlotCount = 5

// tierSlot 返回权益在号池规则表里的格
func tierSlot(tier BenefitTier) int {
	if tier >= 0 && tier < tierSlotCount-1 {
		return int(tier)
	}
	return tierSlotCount - 1
}

// poolRules 为号池划分规则：开关原值与按开关预先算好的两张表。候选扫描对每个账户都要判断一次号池（上千账户 × 每秒上百请求），
// 判断只查表，不在每个账户上走分支；表只在规则变化时重算（见 SetPoolRules）。受账户池 mu 保护
type poolRules struct {
	// includePro 为 ULTRA_INCLUDE_PRO 的原值（PRO_POOL=true 时保存但不生效），proPool 为 PRO_POOL
	includePro bool
	proPool    bool
	// partition 为权益格 → 所属分区
	partition [tierSlotCount]PoolScope
	// tiers 为号池 → 包含的权益格（按格的位掩码）
	tiers [PoolScopeCount]uint8
}

// newPoolRules 按开关建立号池划分规则与两张表
func newPoolRules(includePro bool, proPool bool) poolRules {
	rules := poolRules{includePro: includePro, proPool: proPool}
	for slot := range rules.partition {
		rules.partition[slot] = poolOfTier(BenefitTier(slot), includePro, proPool)
	}
	for scope := range PoolScopeCount {
		for slot, partition := range rules.partition {
			if scope.Contains(partition) {
				rules.tiers[scope] |= 1 << slot
			}
		}
	}
	return rules
}

// partitionOf 返回权益为 tier 的账户所属的分区
func (rules *poolRules) partitionOf(tier BenefitTier) PoolScope {
	return rules.partition[tierSlot(tier)]
}

// allowsTier 判断权益为 tier 的账户是否属于号池 scope；不认识的号池为否
func (rules *poolRules) allowsTier(scope PoolScope, tier BenefitTier) bool {
	return scope < PoolScopeCount && rules.tiers[scope]&(1<<tierSlot(tier)) != 0
}

// sameMembership 判断两套规则划分出的号池成员是否相同：PRO_POOL=true 时切换 ULTRA_INCLUDE_PRO 只改原值，成员不变
func (rules *poolRules) sameMembership(other *poolRules) bool {
	return rules.partition == other.partition
}

// allows 判断账户在规则下是否属于号池
func (scope PoolScope) allows(account *Account, rules *poolRules) bool {
	return account != nil && rules.allowsTier(scope, account.BenefitTier)
}

// inPoolLocked 判断账户当前是否属于号池；调用方持有 p.mu。账户池内的号池判断都经过这里或 tierInPoolLocked，
// 开关与它派生的成员集合、候选集在同一把锁下变化
func (p *AccountPool) inPoolLocked(scope PoolScope, account *Account) bool {
	return scope.allows(account, &p.rules)
}

// tierInPoolLocked 判断权益为 tier 的账户当前是否属于号池；调用方持有 p.mu。候选索引按位置只存真实权益，号池在读取时推导
func (p *AccountPool) tierInPoolLocked(scope PoolScope, tier BenefitTier) bool {
	return p.rules.allowsTier(scope, tier)
}

// poolOfTierLocked 返回权益为 tier 的账户当前所属的号池（Worker 分区）；调用方持有 p.mu
func (p *AccountPool) poolOfTierLocked(tier BenefitTier) PoolScope {
	return p.rules.partitionOf(tier)
}

// memberPartitionLocked 返回成员集合里记录的账户分区，没有记录的为普通分区；调用方持有 p.mu
func (p *AccountPool) memberPartitionLocked(accountID string) PoolScope {
	if partition, ok := p.partitionMembers[accountID]; ok {
		return partition
	}
	return PoolScopeNormal
}

// rebuildMembersLocked 按当前规则整体重算成员集合：一次 O(N) 扫描，比逐个账户走 accountChangedLocked 少维护一遍候选集
// （候选索引随后整体作废）；调用方持有 p.mu
func (p *AccountPool) rebuildMembersLocked() {
	clear(p.partitionMembers)
	for _, account := range p.accounts {
		if account == nil || p.byID[account.ID] != account {
			continue
		}
		if partition := p.rules.partitionOf(account.BenefitTier); partition != PoolScopeNormal {
			p.partitionMembers[account.ID] = partition
		}
	}
	p.membersSnapshot = nil
}

// SetPoolRules 切换号池划分规则（ULTRA_INCLUDE_PRO 与 PRO_POOL），返回成员是否发生变化。号池成员规则变化与账户权益变化同类
// （见 syncAccountRuntimeLocked）：递增 eligibilityVersion 让按号池缓存的目录汇总、别名解析与模型通道重建；候选集成员按号池判断，
// 整个候选索引作废，下次调度时重建（与 Remove、SetUpstreamChannels 相同）。进行中的租约不受影响，按旧成员选中、尚未确认的租约由
// refreshAndValidateLease 放弃并重新选号。最后关闭 changed，让等待账户的请求和后台循环立即按新成员重查。
// 是否变化按生效的成员判断：PRO_POOL=true 时 ULTRA_INCLUDE_PRO 只保存原值（关闭 PRO_POOL 后按它划分），成员不变、不作废任何缓存
func (p *AccountPool) SetPoolRules(includePro bool, proPool bool) bool {
	if p == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.setPoolRulesLocked(includePro, proPool)
}

// SetUltraIncludePro 只切换 ULTRA_INCLUDE_PRO（PRO_POOL 保持当前值，在锁内读取），返回成员是否发生变化，见 SetPoolRules
func (p *AccountPool) SetUltraIncludePro(on bool) bool {
	if p == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.setPoolRulesLocked(on, p.rules.proPool)
}

// SetProPool 只切换 PRO_POOL（ULTRA_INCLUDE_PRO 保持当前原值，在锁内读取），返回成员是否发生变化，见 SetPoolRules
func (p *AccountPool) SetProPool(on bool) bool {
	if p == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.setPoolRulesLocked(p.rules.includePro, on)
}

// setPoolRulesLocked 为 SetPoolRules 的实现；调用方持有 p.mu
func (p *AccountPool) setPoolRulesLocked(includePro bool, proPool bool) bool {
	next := newPoolRules(includePro, proPool)
	same := p.rules.sameMembership(&next)
	p.rules = next
	if same {
		return false
	}
	p.rebuildMembersLocked()
	// 只有 Ultra 分区的账户挂在出口的 Ultra 通道上（见 egress_rate.go）：成员整体重算后整体重挂，普通与 Pro 之间的移动不换通道
	p.reattachEgressAllLocked()
	p.eligibilityVersion++
	p.invalidateSchedulingLocked()
	p.invalidateStatusLocked()
	p.poolRuleVersion.Add(1)
	p.proPoolLive.Store(proPool)
	p.notifyLocked()
	return true
}

// UltraIncludePro 返回当前生效的 ULTRA_INCLUDE_PRO：权益为 Pro 的账户是否并入 Ultra 号池。PRO_POOL=true 时为假（原值只保存）
func (p *AccountPool) UltraIncludePro() bool {
	if p == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.rules.includePro && !p.rules.proPool
}

// UltraIncludeProSetting 返回保存的 ULTRA_INCLUDE_PRO 原值（不论 PRO_POOL=true 时是否生效）：升级提示据此判断升级前 Pro 账户在哪个分区
// （见 app 的 proPoolNotice），不用于成员划分
func (p *AccountPool) UltraIncludeProSetting() bool {
	if p == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.rules.includePro
}

// ProPool 返回当前是否启用 Pro 独立号池（PRO_POOL），无锁读取：与成员规则在同一把锁内切换，读到真时 Pro 账户已在 Pro 号池
func (p *AccountPool) ProPool() bool {
	if p == nil {
		return false
	}
	return p.proPoolLive.Load()
}

// PoolRuleVersion 返回号池划分规则的版本：号池成员（PRO_POOL、ULTRA_INCLUDE_PRO）或权益规则每次实际变化时加一，无锁读取。
// app 层按号池成员计算、又在锁外缓存的派生结果（预热概况、主用模型供给）计算前读取并一同保存，版本不同即作废
func (p *AccountPool) PoolRuleVersion() uint64 {
	if p == nil {
		return 0
	}
	return p.poolRuleVersion.Load()
}

type poolScopeContextKey struct{}

// ContextWithPoolScope 把请求可以使用的号池写入 context，请求内的全部选号都只在该号池中进行
func ContextWithPoolScope(ctx context.Context, scope PoolScope) context.Context {
	return context.WithValue(ctx, poolScopeContextKey{}, scope)
}

// LookupPoolScope 返回 context 中的号池以及是否已经写入
func LookupPoolScope(ctx context.Context) (PoolScope, bool) {
	if ctx == nil {
		return PoolScopeAll, false
	}
	scope, ok := ctx.Value(poolScopeContextKey{}).(PoolScope)
	return scope, ok
}

// PoolScopeFromContext 返回 context 中的号池；没有写入时（后台任务）不限号池
func PoolScopeFromContext(ctx context.Context) PoolScope {
	scope, _ := LookupPoolScope(ctx)
	return scope
}

// ScopedSelection 为没有指定号池的选择补上 context 中的号池
func ScopedSelection(ctx context.Context, selection AccountSelection) AccountSelection {
	if selection.Pool == PoolScopeAll {
		selection.Pool = PoolScopeFromContext(ctx)
	}
	return selection
}

// ResourcePoolMismatchError 表示请求引用的文件或视频绑定在请求号池之外的账户上：/ultra 请求不能使用其他号池账户的资源，
// /pro 请求同样只能使用 Pro 号池账户的资源，独占模式下普通路径也不能使用 Ultra 号池账户（含 ULTRA_INCLUDE_PRO=true 时并入的 Pro）
// 的资源，PRO_POOL=true 时普通路径不能使用 Pro 账户的资源。资源不随开关迁移，按所属账户当前的号池判断：切换开关后经原路径引用
// 已换号池的账户上的资源，同样按这个错误返回。属于请求本身的原因，对外按 400 返回，不会改用池外账户。
// 消息是英文：会原样返回给客户端，告诉调用方改用哪个入口
type ResourcePoolMismatchError struct {
	ResourceID string
	// Owner 为资源所属账户当前的号池
	Owner PoolScope
	// Request 为请求限定的号池（决定普通号池资源的提示里去掉哪个前缀）
	Request PoolScope
}

func (e *ResourcePoolMismatchError) Error() string {
	switch {
	case e.Owner == PoolScopeUltra:
		return fmt.Sprintf("resource %s belongs to an account of the Ultra pool; request it through the /ultra endpoints", e.ResourceID)
	case e.Owner == PoolScopePro:
		return fmt.Sprintf("resource %s belongs to an account of the Pro pool; request it through the /pro endpoints", e.ResourceID)
	case e.Request == PoolScopePro:
		return fmt.Sprintf("resource %s belongs to an account of the normal pool; request it without the /pro prefix", e.ResourceID)
	}
	return fmt.Sprintf("resource %s belongs to an account of the normal pool; request it without the /ultra prefix", e.ResourceID)
}

// Unwrap 归为请求参数错误，各协议按 400 返回
func (e *ResourcePoolMismatchError) Unwrap() error {
	return ErrInvalidArgument
}

// HTTPStatus 返回资源号池不符对应的公开状态码
func (e *ResourcePoolMismatchError) HTTPStatus() int {
	return http.StatusBadRequest
}

// ErrorCode 返回 OpenAI 兼容的请求错误代码
func (e *ResourcePoolMismatchError) ErrorCode() string {
	return "invalid_request"
}

// resourcePoolErrorLocked 检查资源所属账户是否在号池中，不在时返回号池不符错误；调用方持有账户池 mu
func (p *AccountPool) resourcePoolErrorLocked(scope PoolScope, resourceID string, ownerID string) error {
	if scope == PoolScopeAll {
		return nil
	}
	owner := p.byID[ownerID]
	if owner == nil || p.inPoolLocked(scope, owner) {
		return nil
	}
	return &ResourcePoolMismatchError{ResourceID: resourceID, Owner: p.poolOfTierLocked(owner.BenefitTier), Request: scope}
}

// PoolOf 返回账户当前所属的号池（Worker 分区），随 PRO_POOL 与 ULTRA_INCLUDE_PRO 变化；账户不存在时按普通号池
func (p *AccountPool) PoolOf(accountID string) PoolScope {
	if p == nil {
		return PoolScopeNormal
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	account := p.byID[strings.TrimSpace(accountID)]
	if account == nil {
		return PoolScopeNormal
	}
	return p.poolOfTierLocked(account.BenefitTier)
}

// TierPoolOf 返回账户按真实权益的号池：Ultra 权益为 PoolScopeUltra，其余为 PoolScopeNormal，不随 PRO_POOL 与 ULTRA_INCLUDE_PRO 变化。
// 供额度共享学习分组（键持久化在 .quota-sharing.json）：Ultra 账户的额度与其他权益不同，并入 Ultra 号池的 Pro 证据混进 Ultra 的判定后，
// 关掉开关也不会消失；Pro 独立号池同样不另分组，切换开关不丢已学到的证据。账户不存在时按普通号池
func (p *AccountPool) TierPoolOf(accountID string) PoolScope {
	if p == nil {
		return PoolScopeNormal
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	account := p.byID[strings.TrimSpace(accountID)]
	if account == nil {
		return PoolScopeNormal
	}
	return poolOfTier(account.BenefitTier, false, false)
}

// PartitionSet 为账户 → 所属 Worker 分区的只读快照：只记普通分区以外的账户（Ultra 与 Pro），没有记录的账户为普通分区
type PartitionSet map[string]PoolScope

// Of 返回账户所属的分区，没有记录的为普通分区
func (set PartitionSet) Of(accountID string) PoolScope {
	if partition, ok := set[accountID]; ok {
		return partition
	}
	return PoolScopeNormal
}

// Count 返回快照里属于分区 partition 的账户数；普通分区不记在快照里，为 0
func (set PartitionSet) Count(partition PoolScope) int {
	count := 0
	for _, member := range set {
		if member == partition {
			count++
		}
	}
	return count
}

// PartitionMembers 一次加锁返回当前属于普通分区以外（Ultra、Pro）的账户，供 Worker 分区计数。返回值是共享的只读快照，调用方不得修改：
// 快照只在成员实际变化时重建（见 accountChangedLocked、Remove、SetPoolRules），Pro 账户可达数千，
// 每次调用都克隆会在账户池锁内的热路径上花去近一毫秒
func (p *AccountPool) PartitionMembers() PartitionSet {
	if p == nil {
		return PartitionSet{}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.membersSnapshot == nil {
		p.membersSnapshot = maps.Clone(p.partitionMembers)
		if p.membersSnapshot == nil {
			p.membersSnapshot = PartitionSet{}
		}
	}
	return p.membersSnapshot
}

// CanonicalModelIDIn 与 CanonicalModelID 相同，但只在号池内的账户目录中解析别名
func (p *AccountPool) CanonicalModelIDIn(scope PoolScope, modelID string) string {
	trimmed := strings.TrimPrefix(strings.TrimSpace(modelID), "models/")
	if p == nil || trimmed == "" {
		return modelID
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	// 号池内任一账户目录有同名正式模型时原样返回，否则取按账户顺序第一个带该别名的模型；解析表按目录版本缓存
	index := p.canonicalIndexLocked(scope)
	if _, exists := index.primary[trimmed]; exists {
		return modelID
	}
	if canonical, exists := index.alias[trimmed]; exists {
		return canonical
	}
	return modelID
}

// EligibleModelsIn 与 EligibleModels 相同，但只计号池内的启用账户：/ultra 的模型列表只列 Ultra 号池账户可用的模型
func (p *AccountPool) EligibleModelsIn(scope PoolScope, models []Model) []Model {
	p.mu.Lock()
	defer p.mu.Unlock()
	eligible := make([]Model, 0, len(models))
	for _, model := range models {
		if channels := p.modelChannelsInLocked(model, scope); len(channels) > 0 {
			model.Channels = channels
			eligible = append(eligible, model)
		}
	}
	return eligible
}

// EnabledAccountsIn 与 EnabledAccounts 相同，但只统计号池内的账户。按候选索引的紧凑数组扫描（每个请求都要算一次换号上限）
func (p *AccountPool) EnabledAccountsIn(scope PoolScope) ([]string, int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	index := p.schedulingLocked()
	nowNanos := time.Now().UnixNano()
	ids := make([]string, 0, len(index.ids))
	schedulable := 0
	for position := range index.ids {
		if !index.enabled[position] || !p.tierInPoolLocked(scope, index.tier[position]) {
			continue
		}
		ids = append(ids, index.ids[position])
		if index.blocked[position] || index.active[position] > 0 ||
			index.state[position] == AccountReady && index.globalUntil[position] <= nowNanos {
			schedulable++
		}
	}
	return ids, schedulable
}

// attemptCandidatesIn 返回号池内启用且就绪或忙碌的账户数，作为不固定账户请求的换号上限（与 Status 的状态推导一致）
func (p *AccountPool) attemptCandidatesIn(scope PoolScope) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	index := p.derivedStatesLocked(now)
	nowNanos := now.UnixNano()
	eligible := 0
	for position := range index.ids {
		if !index.enabled[position] || !p.tierInPoolLocked(scope, index.tier[position]) {
			continue
		}
		if state := index.derivedStateAt(position, nowNanos); state == AccountReady || state == AccountBusy {
			eligible++
		}
	}
	return eligible
}

// ModelInOtherPoolError 包装普通路径上“目录中没有请求模型”或“模型不在号池的模型列表里”的选号错误：请求的号池里没有账户能服务这个模型，
// 但 Pro 或 Ultra 号池里有（Pools，按 Pro、Ultra 的顺序）。对外仍按原错误返回（404 模型不存在），公开说明里加一句改用哪个入口
// （PublicHint）。只在选号失败的错误路径上构造（见 app 的 missingModelError），不在请求热路径上多加锁
type ModelInOtherPoolError struct {
	Err   error
	Model string
	Pools []PoolScope
}

func (e *ModelInOtherPoolError) Error() string {
	labels := make([]string, 0, len(e.Pools))
	prefixes := make([]string, 0, len(e.Pools))
	for _, pool := range e.Pools {
		labels = append(labels, pool.Label())
		prefixes = append(prefixes, "/"+pool.String())
	}
	return fmt.Sprintf("%v（%s有账户能服务模型 %s，改用 %s 前缀）", e.Err, strings.Join(labels, "、"), e.Model, strings.Join(prefixes, " 或 "))
}

// Unwrap 保留原错误：状态码、OpenAI 错误代码与“模型不存在”的判断不变
func (e *ModelInOtherPoolError) Unwrap() error {
	return e.Err
}

// PublicHint 返回追加在对外说明后面的英文提示（不含账户信息），如 "This model is available through the /pro or /ultra endpoints."
func (e *ModelInOtherPoolError) PublicHint() string {
	prefixes := make([]string, 0, len(e.Pools))
	for _, pool := range e.Pools {
		prefixes = append(prefixes, "/"+pool.String())
	}
	if len(prefixes) == 0 {
		return ""
	}
	return "This model is available through the " + strings.Join(prefixes, " or ") + " endpoints."
}

// ModelServableIn 判断号池里是否有启用账户能经某个启用的上游通道调用模型（目录、权益、权益规则与通道都允许），与模型列表的口径相同
// （见 EligibleModelsIn），按目录版本记忆；只在错误路径与后台检查上调用
func (p *AccountPool) ModelServableIn(scope PoolScope, modelID string) bool {
	modelID = strings.TrimPrefix(strings.TrimSpace(modelID), "models/")
	if p == nil || modelID == "" {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.modelServableInLocked(scope, modelID)
}

// modelServableInLocked 为 ModelServableIn 的实现：先按生成请求（generateContent）判断，再按不限方法判断；调用方持有 p.mu
func (p *AccountPool) modelServableInLocked(scope PoolScope, modelID string) bool {
	for _, model := range []Model{{ID: modelID, Methods: []string{"generateContent"}}, {ID: modelID}} {
		if len(p.modelChannelsInLocked(model, scope)) > 0 {
			return true
		}
	}
	return false
}

// OtherPoolsServing 返回选择的号池（selection.Pool）不包含、又能服务这次请求的 Pro 与 Ultra 号池（按 Pro、Ultra 的顺序），供普通路径的
// 模型不存在错误提示改用哪个入口。names 为请求模型名可能对应的目录 ID（按后缀写法逐个剥离，见 api.RequestModelBases），为空时只看
// selection.ModelID；号池里按顺序有一个名称能服务即算。请求的方法与能力（selection.Method、Capability）照选号的口径核对：
// 模型在目录里但不支持这个方法或能力时，/pro 与 /ultra 同样接不了，不提示。一次加锁，只在错误路径上调用
func (p *AccountPool) OtherPoolsServing(selection AccountSelection, names []string) []PoolScope {
	if p == nil {
		return nil
	}
	if len(names) == 0 {
		names = []string{selection.ModelID}
	}
	candidates := make([]string, 0, len(names))
	for _, name := range names {
		if name = strings.TrimPrefix(strings.TrimSpace(name), "models/"); name != "" && !slices.Contains(candidates, name) {
			candidates = append(candidates, name)
		}
	}
	if len(candidates) == 0 {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	var pools []PoolScope
	for _, partition := range [...]PoolScope{PoolScopePro, PoolScopeUltra} {
		if selection.Pool.Contains(partition) || !p.hasModelCatalogLocked(partition) {
			continue
		}
		for _, name := range candidates {
			if p.selectionServableInLocked(partition, name, selection.Method, selection.Capability) {
				pools = append(pools, partition)
				break
			}
		}
	}
	return pools
}

// selectionServableInLocked 判断号池能服务按模型、方法与能力的请求：先按号池目录汇总核对（与选号的 validateSelectionLocked 相同：
// 目录列出模型、支持这个方法与能力），不过时直接返回，不按账户查通道、不写通道记录，不存在的模型名不会让记录随名称增长；
// 通过后按模型列表的口径（modelChannelsInLocked：启用账户、权益与权益规则、启用的通道）确认，生成请求按 generateContent 查，
// 其他方法只看模型、权益与通道（方法与能力已由目录汇总核对）。调用方持有 p.mu
func (p *AccountPool) selectionServableInLocked(scope PoolScope, modelID string, method string, capability string) bool {
	if !p.hasModelLocked(modelID, scope) ||
		method != "" && !p.hasModelMethodLocked(modelID, method, scope) ||
		capability != "" && !p.hasModelCapabilityLocked(modelID, capability, scope) {
		return false
	}
	model := Model{ID: modelID}
	if method == "generateContent" {
		model.Methods = []string{method}
	}
	return len(p.modelChannelsInLocked(model, scope)) > 0
}
