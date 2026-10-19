package aistudio

import (
	"encoding/binary"
	"hash/maphash"
	"slices"
	"sort"
	"time"
)

// 账户池的目录派生缓存
//
// 模型存在性检查、别名解析、模型可用通道与预热模型原先每次都在账户池锁内扫描全部账户的全部模型。
// 这些结果只随目录相关的变化改变（账户增删、目录或权益变化、启用状态、Build 目录、上游通道），
// 账户池用 eligibilityVersion 记录这类变化，派生结果按版本缓存，版本不变时直接返回。
// 重建时按目录内容去重：上万个账户通常只有几种目录，每种只处理一次。全部结构都受账户池 mu 保护

// catalogKeySeed 为目录内容键的散列种子，进程内固定
var catalogKeySeed = maphash.MakeSeed()

// setCatalogKeyLocked 用 SetCatalog 已在锁外算好的目录指纹（含权益与全部模型字段，比内容键更细）直接作为账户目录的内容键，
// 省去首次按目录计算散列；调用方持有 p.mu，account.Models 已替换为新目录
func setCatalogKeyLocked(account *Account, fingerprint string) {
	account.catalogKey = maphash.String(catalogKeySeed, fingerprint)
	account.catalogKeySource = account.Models
	if account.catalogKeySource == nil {
		account.catalogKeySource = []Model{}
	}
}

// accountCatalogKeyLocked 返回账户 Playground 目录的内容键：模型 ID、别名、方法、能力与访问模式的散列，
// 目录切片被替换时才重算。只用于按目录去重（上万账户通常只有几种目录），不拼接字符串以免每个账户分配几 KB
func accountCatalogKeyLocked(account *Account) uint64 {
	if account.catalogKeySource != nil && sameModelSlice(account.catalogKeySource, account.Models) {
		return account.catalogKey
	}
	var hash maphash.Hash
	hash.SetSeed(catalogKeySeed)
	var number [8]byte
	writeField := func(value string, tag byte) {
		_, _ = hash.WriteString(value)
		_ = hash.WriteByte(tag)
	}
	for _, model := range account.Models {
		writeField(model.ID, 0)
		for _, alias := range model.CapabilityOptions["aliases"] {
			writeField(alias, 1)
		}
		for _, method := range model.Methods {
			writeField(method, 2)
		}
		capabilities := make([]string, 0, len(model.Capabilities))
		for capability, enabled := range model.Capabilities {
			if enabled {
				capabilities = append(capabilities, capability)
			}
		}
		sort.Strings(capabilities)
		for _, capability := range capabilities {
			writeField(capability, 3)
		}
		for _, mode := range model.AccessModes {
			binary.LittleEndian.PutUint64(number[:], uint64(mode))
			_, _ = hash.Write(number[:])
			_ = hash.WriteByte(4)
		}
		_ = hash.WriteByte('\n')
	}
	account.catalogKey = hash.Sum64()
	account.catalogKeySource = account.Models
	if account.catalogKeySource == nil {
		account.catalogKeySource = []Model{}
	}
	return account.catalogKey
}

// catalogSummary 为一个号池内全部账户 Playground 目录的汇总（不看启用与权益，与原 hasModelLocked 等的口径相同）
type catalogSummary struct {
	version uint64
	// accounts 为有目录的账户数
	accounts int
	// models 按模型 ID 与别名索引目录中出现过的方法与能力
	models map[string]*catalogModelSummary
}

type catalogModelSummary struct {
	methods      map[string]struct{}
	capabilities map[string]struct{}
}

// catalogSummaryLocked 返回号池的目录汇总，目录版本变化后重建；调用方持有 p.mu
func (p *AccountPool) catalogSummaryLocked(scope PoolScope) *catalogSummary {
	summary := p.catalogSummaries[scope]
	if summary != nil && summary.version == p.eligibilityVersion {
		return summary
	}
	summary = &catalogSummary{version: p.eligibilityVersion, models: make(map[string]*catalogModelSummary)}
	seen := make(map[uint64]struct{})
	for _, account := range p.accounts {
		if account == nil || len(account.Models) == 0 || !p.inPoolLocked(scope, account) {
			continue
		}
		summary.accounts++
		key := accountCatalogKeyLocked(account)
		if _, done := seen[key]; done {
			continue
		}
		seen[key] = struct{}{}
		for _, model := range account.Models {
			summary.add(model, model.ID)
			for _, alias := range model.CapabilityOptions["aliases"] {
				summary.add(model, alias)
			}
		}
	}
	p.catalogSummaries[scope] = summary
	return summary
}

func (summary *catalogSummary) add(model Model, key string) {
	entry := summary.models[key]
	if entry == nil {
		entry = &catalogModelSummary{methods: make(map[string]struct{}), capabilities: make(map[string]struct{})}
		summary.models[key] = entry
	}
	for _, method := range model.Methods {
		entry.methods[method] = struct{}{}
	}
	for capability, enabled := range model.Capabilities {
		if enabled {
			entry.capabilities[capability] = struct{}{}
		}
	}
}

// canonicalIndex 为一个号池内目录别名到正式模型 ID 的解析表
type canonicalIndex struct {
	version uint64
	// primary 为目录中出现过的正式模型 ID
	primary map[string]struct{}
	// alias 为别名到按账户顺序第一个带该别名的模型 ID
	alias map[string]string
}

// canonicalIndexLocked 返回号池的别名解析表，目录版本变化后重建；调用方持有 p.mu
func (p *AccountPool) canonicalIndexLocked(scope PoolScope) *canonicalIndex {
	index := p.canonicalIndexes[scope]
	if index != nil && index.version == p.eligibilityVersion {
		return index
	}
	index = &canonicalIndex{version: p.eligibilityVersion, primary: make(map[string]struct{}), alias: make(map[string]string)}
	seen := make(map[uint64]struct{})
	for _, account := range p.accounts {
		if account == nil || len(account.Models) == 0 || !p.inPoolLocked(scope, account) {
			continue
		}
		key := accountCatalogKeyLocked(account)
		if _, done := seen[key]; done {
			continue
		}
		seen[key] = struct{}{}
		for _, model := range account.Models {
			index.primary[model.ID] = struct{}{}
			for _, alias := range model.CapabilityOptions["aliases"] {
				if _, exists := index.alias[alias]; !exists {
					index.alias[alias] = model.ID
				}
			}
		}
	}
	p.canonicalIndexes[scope] = index
	return index
}

// modelChannelsMemo 记录号池内各模型的可用通道（modelChannelsInLocked 的结果），目录版本变化后作废
type modelChannelsMemo struct {
	version  uint64
	channels map[string][]string
}

// modelChannelsMemoLocked 返回号池的模型通道记录；调用方持有 p.mu
func (p *AccountPool) modelChannelsMemoLocked(scope PoolScope) *modelChannelsMemo {
	memo := p.modelChannelsMemos[scope]
	if memo == nil || memo.version != p.eligibilityVersion {
		memo = &modelChannelsMemo{version: p.eligibilityVersion, channels: make(map[string][]string)}
		p.modelChannelsMemos[scope] = memo
	}
	return memo
}

// accountBootstrapModelsLocked 返回账户的 WAA 初始化模型，目录或权益变化时才重算；调用方持有 p.mu，不得修改返回值。
// 原实现对每个模型再扫一遍目录判断资格，目录几十个模型时是平方量级，预热概况与候补统计对上万账户各算一次要几百毫秒
func accountBootstrapModelsLocked(account *Account) []string {
	if account.bootstrapSource != nil && sameModelSlice(account.bootstrapSource, account.Models) && account.bootstrapTier == account.BenefitTier {
		return account.bootstrapModels
	}
	// bootstrapEligible 判断目录模型本身可作为初始化模型；某个 ID 只要有一个按 ID 或别名匹配的目录模型满足条件即可
	bootstrapEligible := func(model Model) bool {
		return hasMethod(model, "generateContent") && modelAllowedByTier(model, account.BenefitTier) && model.Capabilities["chat_model"]
	}
	eligibleID := func(modelID string) bool {
		for _, model := range account.Models {
			if modelMatchesID(model, modelID) && bootstrapEligible(model) {
				return true
			}
		}
		return false
	}
	models := make([]string, 0, len(account.Models))
	seen := make(map[string]struct{}, len(account.Models))
	if eligibleID(preferredBootstrapModelID) {
		seen[preferredBootstrapModelID] = struct{}{}
		models = append(models, preferredBootstrapModelID)
	}
	for _, model := range account.Models {
		if _, exists := seen[model.ID]; exists {
			continue
		}
		// 多数模型按自身判断即可，不满足时才查有没有别的目录模型以别名匹配
		if !bootstrapEligible(model) && !eligibleID(model.ID) {
			continue
		}
		seen[model.ID] = struct{}{}
		models = append(models, model.ID)
	}
	account.bootstrapModels = models
	account.bootstrapSource = account.Models
	if account.bootstrapSource == nil {
		account.bootstrapSource = []Model{}
	}
	account.bootstrapTier = account.BenefitTier
	return models
}

// accountWarmCoverageLocked 返回账户目录中可生成的模型数（PreferWarmPool 的排序依据），目录或权益变化时才重算；调用方持有 p.mu
func accountWarmCoverageLocked(account *Account) int {
	if account.warmCoverageSource != nil && sameModelSlice(account.warmCoverageSource, account.Models) && account.warmCoverageTier == account.BenefitTier {
		return account.warmCoverage
	}
	coverage := 0
	for _, model := range account.Models {
		if account.SupportsMethod(model.ID, "generateContent") {
			coverage++
		}
	}
	account.warmCoverage = coverage
	account.warmCoverageSource = account.Models
	if account.warmCoverageSource == nil {
		account.warmCoverageSource = []Model{}
	}
	account.warmCoverageTier = account.BenefitTier
	return coverage
}

// accountStatusModelsLocked 返回账户目录按 ID 排序的模型列表，目录变化时才重算；调用方持有 p.mu，不得修改返回值
func accountStatusModelsLocked(account *Account) []string {
	if account.statusModelsSource != nil && sameModelSlice(account.statusModelsSource, account.Models) {
		return account.statusModels
	}
	models := make([]string, 0, len(account.Models))
	for _, model := range account.Models {
		models = append(models, model.ID)
	}
	sort.Strings(models)
	account.statusModels = models
	account.statusModelsSource = account.Models
	if account.statusModelsSource == nil {
		account.statusModelsSource = []Model{}
	}
	return models
}

// cachedStatusLocked 返回缓存的账户状态列表（账户多时最多 statusCacheTTL 旧）；调用方持有 p.mu
func (p *AccountPool) cachedStatusLocked(now time.Time) ([]AccountStatus, bool) {
	if len(p.accounts) <= statusCacheMinAccounts || p.statusCache == nil || now.Sub(p.statusCacheAt) >= statusCacheTTL {
		return nil, false
	}
	return p.statusCache, true
}

// storeStatusLocked 保存账户状态列表供随后 statusCacheTTL 内复用；调用方持有 p.mu
func (p *AccountPool) storeStatusLocked(statuses []AccountStatus, now time.Time) {
	if len(p.accounts) <= statusCacheMinAccounts {
		return
	}
	p.statusCache = statuses
	p.statusCacheAt = now
}

// invalidateStatusLocked 丢弃缓存的账户状态列表（账户增删后）；调用方持有 p.mu
func (p *AccountPool) invalidateStatusLocked() {
	p.statusCache = nil
}

// refreshStatusEntryLocked 在账户的启用、状态、目录、权益或冷却变化后就地更新缓存列表中该账户的一项：管理操作的返回值与账户列表
// 都来自 Status()，变化要在缓存期内立刻可见，又不必因为一个账户的变化丢掉整份列表（账户上万时重建要几十毫秒，冷却与目录同步
// 每秒都有几次）。缓存已过期时直接丢弃，下次调用重建；下标与缓存项对不上（账户增删后尚未重编号）时整份作废；调用方持有 p.mu
func (p *AccountPool) refreshStatusEntryLocked(account *Account) {
	if p.statusCache == nil {
		return
	}
	now := time.Now()
	if now.Sub(p.statusCacheAt) >= statusCacheTTL {
		p.invalidateStatusLocked()
		return
	}
	position := account.position
	if position < 0 || position >= len(p.statusCache) || p.statusCache[position].ID != account.ID {
		p.invalidateStatusLocked()
		return
	}
	p.statusCache[position] = p.accountStatusLocked(account, now)
}

// cloneStatuses 拷贝状态列表，调用方各自持有一份
func cloneStatuses(statuses []AccountStatus) []AccountStatus {
	return slices.Clone(statuses)
}
