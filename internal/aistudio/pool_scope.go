package aistudio

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"strings"
	"time"
)

// PoolScope 表示一次请求可以使用的账户号池。号池按账户当前权益与 ULTRA_INCLUDE_PRO 划分（唯一的判定见 ultraPoolTier）：
// 权益为 Ultra 的账户，以及 ULTRA_INCLUDE_PRO=true 时权益为 Pro 的账户，组成 Ultra 号池（付费号池）；其余账户（含权益尚未识别的账户）
// 组成普通号池。号池只决定账户服务哪条路径，账户权益本身不变：权益请求头、模型资格与输入上限仍按真实权益。
// /ultra 前缀的请求只用 Ultra 号池；ULTRA_EXCLUSIVE=true（默认）时普通路径只用普通号池，为 false 时普通路径不限号池。
// 预热、轮换、登录检查等不和请求关联的后台任务不限号池。Worker 分区与号池一致（普通分区与 Ultra 分区），
// 用 PoolScopeNormal 与 PoolScopeUltra 表示
type PoolScope uint8

const (
	// PoolScopeAll 不限号池
	PoolScopeAll PoolScope = iota
	// PoolScopeNormal 只用普通号池
	PoolScopeNormal
	// PoolScopeUltra 只用 Ultra 号池
	PoolScopeUltra
)

// String 返回号池的稳定名称：ultra、normal，不限号池为空
func (scope PoolScope) String() string {
	switch scope {
	case PoolScopeUltra:
		return "ultra"
	case PoolScopeNormal:
		return "normal"
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
	default:
		return ""
	}
}

// ultraPoolTier 判断权益为 tier 的账户是否属于 Ultra 号池：Ultra 总是属于，includePro（ULTRA_INCLUDE_PRO）为真时 Pro 也属于。
// 号池成员只由这里决定，权益本身不变。不叫 paid：Model.Paid 是模型属性，同名会把两者混淆
func ultraPoolTier(tier BenefitTier, includePro bool) bool {
	return tier == BenefitTierUltra || includePro && tier == BenefitTierPro
}

// poolOfTier 返回权益为 tier 的账户所属的号池（Worker 分区）：属于 Ultra 号池的为 PoolScopeUltra，其余（含尚未识别）为 PoolScopeNormal
func poolOfTier(tier BenefitTier, includePro bool) PoolScope {
	if ultraPoolTier(tier, includePro) {
		return PoolScopeUltra
	}
	return PoolScopeNormal
}

// allowsTier 判断权益为 tier 的账户在开关 includePro 下是否属于号池
func (scope PoolScope) allowsTier(tier BenefitTier, includePro bool) bool {
	switch scope {
	case PoolScopeUltra:
		return ultraPoolTier(tier, includePro)
	case PoolScopeNormal:
		return !ultraPoolTier(tier, includePro)
	default:
		return true
	}
}

// allows 判断账户在开关 includePro 下是否属于号池
func (scope PoolScope) allows(account *Account, includePro bool) bool {
	return account != nil && scope.allowsTier(account.BenefitTier, includePro)
}

// inPoolLocked 判断账户当前是否属于号池；调用方持有 p.mu。账户池内的号池判断都经过这里或 tierInPoolLocked，
// 开关与它派生的成员集合、候选集在同一把锁下变化
func (p *AccountPool) inPoolLocked(scope PoolScope, account *Account) bool {
	return scope.allows(account, p.ultraIncludePro)
}

// tierInPoolLocked 判断权益为 tier 的账户当前是否属于号池；调用方持有 p.mu。候选索引按位置只存真实权益，号池在读取时推导
func (p *AccountPool) tierInPoolLocked(scope PoolScope, tier BenefitTier) bool {
	return scope.allowsTier(tier, p.ultraIncludePro)
}

// poolOfTierLocked 返回权益为 tier 的账户当前所属的号池；调用方持有 p.mu
func (p *AccountPool) poolOfTierLocked(tier BenefitTier) PoolScope {
	return poolOfTier(tier, p.ultraIncludePro)
}

// SetUltraIncludePro 切换 ULTRA_INCLUDE_PRO，返回是否发生变化。号池成员规则变化与账户权益变化同类（见 syncAccountRuntimeLocked）：
// 递增 eligibilityVersion 让按号池缓存的目录汇总、别名解析与模型通道重建；候选集成员按号池判断，整个候选索引作废，
// 下次调度时重建（与 Remove、SetUpstreamChannels 相同）。进行中的租约不受影响，按旧成员选中、尚未确认的租约由
// refreshAndValidateLease 放弃并重新选号。最后关闭 changed，让等待账户的请求和后台循环立即按新成员重查
func (p *AccountPool) SetUltraIncludePro(on bool) bool {
	if p == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ultraIncludePro == on {
		return false
	}
	p.ultraIncludePro = on
	// 成员集合按新规则整体重算：一次 O(N) 扫描，比逐个账户走 accountChangedLocked 少维护一遍候选集（候选索引随后整体作废）
	clear(p.ultraAccounts)
	for _, account := range p.accounts {
		if account != nil && p.byID[account.ID] == account && ultraPoolTier(account.BenefitTier, on) {
			p.ultraAccounts[account.ID] = struct{}{}
		}
	}
	p.ultraSnapshot = nil
	p.eligibilityVersion++
	p.invalidateSchedulingLocked()
	p.invalidateStatusLocked()
	p.poolRuleVersion.Add(1)
	p.notifyLocked()
	return true
}

// UltraIncludePro 返回当前是否把权益为 Pro 的账户并入 Ultra 号池（ULTRA_INCLUDE_PRO）
func (p *AccountPool) UltraIncludePro() bool {
	if p == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.ultraIncludePro
}

// PoolRuleVersion 返回号池划分规则的版本：ULTRA_INCLUDE_PRO 每次实际切换时加一，无锁读取。
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

// ResourcePoolMismatchError 表示请求引用的文件或视频绑定在请求号池之外的账户上：/ultra 请求不能使用普通号池账户的资源，
// 独占模式下普通路径也不能使用 Ultra 号池账户（含 ULTRA_INCLUDE_PRO=true 时并入的 Pro）的资源。资源不随开关迁移，按所属账户当前的
// 号池判断：开启后独占模式下经普通路径、关闭后经 /ultra 引用 Pro 账户上已有的资源，同样按这个错误返回。
// 属于请求本身的原因，对外按 400 返回，不会改用池外账户。
// 消息是英文：会原样返回给客户端，告诉调用方改用哪个入口
type ResourcePoolMismatchError struct {
	ResourceID string
	// Owner 为资源所属账户当前的号池
	Owner PoolScope
}

func (e *ResourcePoolMismatchError) Error() string {
	if e.Owner == PoolScopeUltra {
		return fmt.Sprintf("resource %s belongs to an account of the Ultra pool; request it through the /ultra endpoints", e.ResourceID)
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
	return &ResourcePoolMismatchError{ResourceID: resourceID, Owner: p.poolOfTierLocked(owner.BenefitTier)}
}

// PoolOf 返回账户当前所属的号池（Worker 分区），随 ULTRA_INCLUDE_PRO 变化；账户不存在时按普通号池
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

// TierPoolOf 返回账户按真实权益的号池：Ultra 权益为 PoolScopeUltra，其余为 PoolScopeNormal，不随 ULTRA_INCLUDE_PRO 变化。
// 供额度共享学习分组：Ultra 账户的额度与其他权益不同，并入 Ultra 号池的 Pro 证据混进 Ultra 的判定后，关掉开关也不会消失。
// 账户不存在时按普通号池
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
	return poolOfTier(account.BenefitTier, false)
}

// UltraAccountIDs 一次加锁返回当前属于 Ultra 号池的账户，供 Worker 分区计数。返回值是共享的只读快照，调用方不得修改：
// 快照只在成员实际变化时重建（见 accountChangedLocked、Remove、SetUltraIncludePro），Pro 并入后成员可达数千，
// 每次调用都克隆会在账户池锁内的热路径上花去近一毫秒
func (p *AccountPool) UltraAccountIDs() map[string]struct{} {
	if p == nil {
		return make(map[string]struct{})
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ultraSnapshot == nil {
		p.ultraSnapshot = maps.Clone(p.ultraAccounts)
	}
	return p.ultraSnapshot
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
