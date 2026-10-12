package aistudio

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"strings"
	"time"
)

// PoolScope 表示一次请求可以使用的账户号池。号池按账户当前权益划分：权益为 Ultra 的账户组成 Ultra 号池，
// 其余账户（含权益尚未识别的账户）组成普通号池。/ultra 前缀的请求只用 Ultra 号池；ULTRA_EXCLUSIVE=true（默认）时
// 普通路径只用普通号池，为 false 时普通路径不限号池。预热、轮换、登录检查等不和请求关联的后台任务不限号池。
// Worker 分区同样按账户当前权益划分（普通分区与 Ultra 分区），用 PoolScopeNormal 与 PoolScopeUltra 表示
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

// allowsTier 判断当前权益为 tier 的账户是否属于号池
func (scope PoolScope) allowsTier(tier BenefitTier) bool {
	switch scope {
	case PoolScopeUltra:
		return tier == BenefitTierUltra
	case PoolScopeNormal:
		return tier != BenefitTierUltra
	default:
		return true
	}
}

// allows 判断账户当前是否属于号池；调用方持有账户池 mu
func (scope PoolScope) allows(account *Account) bool {
	return account != nil && scope.allowsTier(account.BenefitTier)
}

// PoolOfTier 返回权益所属的号池：Ultra 权益属于 Ultra 号池，其余（含尚未识别）属于普通号池
func PoolOfTier(tier BenefitTier) PoolScope {
	if tier == BenefitTierUltra {
		return PoolScopeUltra
	}
	return PoolScopeNormal
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
// 独占模式下普通路径也不能使用 Ultra 账户的资源。属于请求本身的原因，对外按 400 返回，不会改用池外账户。
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
	if owner == nil || scope.allows(owner) {
		return nil
	}
	return &ResourcePoolMismatchError{ResourceID: resourceID, Owner: PoolOfTier(owner.BenefitTier)}
}

// PoolOf 返回账户当前所属的号池（Worker 分区）；账户不存在时按普通号池
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
	return PoolOfTier(account.BenefitTier)
}

// UltraAccountIDs 一次加锁返回当前属于 Ultra 号池的账户，供 Worker 分区计数
func (p *AccountPool) UltraAccountIDs() map[string]struct{} {
	if p == nil {
		return make(map[string]struct{})
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	// 账户池随权益变化维护 Ultra 账户集合（见 accountChangedLocked），每次请求的分区判断不再扫描全部账户
	return maps.Clone(p.ultraAccounts)
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
		if !index.enabled[position] || !scope.allowsTier(index.tier[position]) {
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
		if !index.enabled[position] || !scope.allowsTier(index.tier[position]) {
			continue
		}
		if state := index.derivedStateAt(position, nowNanos); state == AccountReady || state == AccountBusy {
			eligible++
		}
	}
	return eligible
}
