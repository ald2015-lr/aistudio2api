package aistudio

import (
	"slices"
	"sort"
	"strings"
)

// 按配置收窄模型可用的权益（ULTRA_ONLY_MODELS、PAID_ONLY_MODELS）
//
// 账户目录 Playground 行的访问模式（AccessModes，见 modelAllowedByTier）原先是唯一按权益挡模型的依据，但它不总是可靠：
// Google 不一定给只限付费或只限 Ultra 的模型标注访问模式，Build 独有的模型（只在 Build 目录里）根本没有这一列；目录约每天才刷新一次，
// 新加的限制要很久才传到每个账户。ULTRA_INCLUDE_PRO=true 把 Pro 并入 Ultra 号池后，目录没有标注 [4] 的 Ultra 专用模型（如 gemini-4-carbon）
// 会选中 Pro 账户、占 Ultra 分区的槽位然后失败。规则按配置补上这一层：
//   - 只收窄、不放宽：账户能服务模型 = 目录按权益允许（modelAllowedByTier）且规则允许账户的真实权益；同一模型在两份列表里时取交集（只有 Ultra）；
//   - 按真实权益：ULTRA_INCLUDE_PRO 并入 Ultra 号池的 Pro 仍按 Pro 判断，不能服务 Ultra 专用模型；权益尚未读取的账户按 Free 处理，
//     两份列表里的模型都不分给它，读到权益后自动恢复；
//   - 匹配：规则里的名称与目录项的模型 ID 或任一别名相同即命中（与 modelMatchesID 相同，只是不区分大小写；同一 ID 在一份目录里出现多次时取交集）；
//     两个通道都先按账户自己 Playground 目录里的目录项判断，自己没有时 Build 按 Playground 索引里的目录项，
//     Build 独有的模型没有 Playground 行，按选择的模型 ID 原样匹配；
//   - 只在候选判断的一处生效（channelSupportsLocked 的 Playground 与 Build 两个分支，以及 WarmWorkerStates 用的
//     accountSupportsSelectionLocked），候选集、热 Worker 选择、预热排序（ServeableCounts）、备用账号与替补、主用模型供给、
//     改用其他模型（MODEL_FALLBACKS）的目标与模型列表都经候选集成员或同一判断得到它；
//   - 刻意不看规则的地方：已取得租约发送前的校验（Account.SupportsModel、SupportsMethod、buildCatalogSupportsLocked）——热更新不让
//     进行中的请求失败；状态口径的冷却（accountModelCoolingLocked）；判断模型是否存在（hasBuildModelLocked 等）——规则排除的模型仍按
//     “号池里有这个模型、权益不符”处理，不触发目录缺模型的补拉；Build 一次性返回的判断（buildUsesUnary）、权益请求头与 WAA 初始化模型。
//
// 规则受 p.mu 保护，SetModelTierRules 修改后与 SetUltraIncludePro 一样作废候选索引与按版本缓存的派生结果

// tierMask 为允许的权益集合：第 n 位对应权益值 n。权益值不能按大小比较（Plus=3 大于 Ultra=2），只能按集合判断
type tierMask uint8

// tierBit 返回权益在集合里的位；未知的权益值不在任何集合里
func tierBit(tier BenefitTier) tierMask {
	if tier < BenefitTierFree || tier > BenefitTierPlus {
		return 0
	}
	return 1 << uint(tier)
}

const (
	// TierRuleUltraOnlyKey 与 TierRulePaidOnlyKey 为两份规则的配置键
	TierRuleUltraOnlyKey = "ULTRA_ONLY_MODELS"
	TierRulePaidOnlyKey  = "PAID_ONLY_MODELS"
)

var (
	// ultraOnlyTiers 为 ULTRA_ONLY_MODELS 允许的权益：只有 Ultra
	ultraOnlyTiers = tierBit(BenefitTierUltra)
	// paidOnlyTiers 为 PAID_ONLY_MODELS 允许的权益：Pro 与 Ultra（与目录访问模式 3 相同，不含 Plus）
	paidOnlyTiers = tierBit(BenefitTierPro) | tierBit(BenefitTierUltra)
	// allTiers 为全部已知权益
	allTiers = tierBit(BenefitTierFree) | tierBit(BenefitTierPro) | tierBit(BenefitTierUltra) | tierBit(BenefitTierPlus)
	// ruleTierOrder 为提示里按权益排列的顺序
	ruleTierOrder = []BenefitTier{BenefitTierFree, BenefitTierPlus, BenefitTierPro, BenefitTierUltra}
)

// tierRuleEntry 为规则里的一项：Key 为所在配置键，Name 为写的名称（已去 models/ 前缀），mask 为允许的权益
type tierRuleEntry struct {
	key  string
	name string
	mask tierMask
}

// modelTierRules 为生效的规则：masks 按小写名称索引允许的权益（同一名称在两份列表里时取交集），entries 按配置顺序列出每一项
type modelTierRules struct {
	masks   map[string]tierMask
	entries []tierRuleEntry
}

// newModelTierRules 按两份列表建立规则；名称去空白与 models/ 前缀，不区分大小写去重
func newModelTierRules(ultraOnly []string, paidOnly []string) modelTierRules {
	rules := modelTierRules{masks: make(map[string]tierMask)}
	add := func(key string, names []string, mask tierMask) {
		seen := make(map[string]struct{}, len(names))
		for _, name := range names {
			name = strings.TrimPrefix(strings.TrimSpace(name), "models/")
			lower := strings.ToLower(name)
			if lower == "" {
				continue
			}
			if _, done := seen[lower]; done {
				continue
			}
			seen[lower] = struct{}{}
			current, exists := rules.masks[lower]
			if !exists {
				current = allTiers
			}
			rules.masks[lower] = current & mask
			rules.entries = append(rules.entries, tierRuleEntry{key: key, name: name, mask: mask})
		}
	}
	add(TierRuleUltraOnlyKey, ultraOnly, ultraOnlyTiers)
	add(TierRulePaidOnlyKey, paidOnly, paidOnlyTiers)
	return rules
}

// equal 判断两份规则相同（按配置写的名称与顺序比较，名称大小写不同也算变化：日志与提示里显示的是写的名称）
func (rules modelTierRules) equal(other modelTierRules) bool {
	return slices.Equal(rules.entries, other.entries)
}

// names 返回一份配置键下写的名称
func (rules modelTierRules) names(key string) []string {
	var names []string
	for _, entry := range rules.entries {
		if entry.key == key {
			names = append(names, entry.name)
		}
	}
	return names
}

// maskFor 返回规则对目录项允许的权益：模型 ID 与每个别名命中的规则取交集；没有命中时为全部权益
func (rules modelTierRules) maskFor(model Model) tierMask {
	mask := allTiers
	if len(rules.masks) == 0 {
		return mask
	}
	if allowed, exists := rules.masks[strings.ToLower(model.ID)]; exists {
		mask &= allowed
	}
	for _, alias := range model.CapabilityOptions["aliases"] {
		if allowed, exists := rules.masks[strings.ToLower(alias)]; exists {
			mask &= allowed
		}
	}
	return mask
}

// SetModelTierRules 设置 ULTRA_ONLY_MODELS（只有 Ultra 能用）与 PAID_ONLY_MODELS（Pro 与 Ultra 能用）的模型，返回规则是否发生变化。
// 与 SetUltraIncludePro 一样：递增 eligibilityVersion 让按版本缓存的目录派生结果（模型通道、模型列表、核对结果、规则提示）重建，
// 候选索引整体作废、下次调度时按新规则重建，状态缓存作废，号池规则版本加一（app 层按它作废预热概况与主用模型供给的缓存），
// 最后关闭 changed 让等待账户的请求立即按新规则重查。进行中的租约不受影响（发送前的校验不看规则），按旧规则选中、尚未确认的租约
// 由 refreshAndValidateLease 放弃并重新选号
func (p *AccountPool) SetModelTierRules(ultraOnly []string, paidOnly []string) bool {
	if p == nil {
		return false
	}
	rules := newModelTierRules(ultraOnly, paidOnly)
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.tierRules.equal(rules) {
		return false
	}
	p.tierRules = rules
	p.tierRulesVersion++
	p.eligibilityVersion++
	p.invalidateSchedulingLocked()
	p.invalidateStatusLocked()
	p.poolRuleVersion.Add(1)
	p.notifyLocked()
	return true
}

// ModelTierRules 返回生效的两份规则（写的名称，按配置顺序）
func (p *AccountPool) ModelTierRules() (ultraOnly []string, paidOnly []string) {
	if p == nil {
		return nil, nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.tierRules.names(TierRuleUltraOnlyKey), p.tierRules.names(TierRulePaidOnlyKey)
}

// tierMaskCacheLimit 为按目录内容共享的规则结果（tierMasksByCatalog）的条数上限，超过时清空重算（目录内容随时间变化，
// 旧内容的条目不再用到）
const tierMaskCacheLimit = 1024

// catalogMasks 返回目录里受规则限制的模型（按模型 ID）允许的权益，没有受限的模型时为 nil。同一 ID 在目录里出现多次时取交集
func (rules modelTierRules) catalogMasks(models []Model) map[string]tierMask {
	var masks map[string]tierMask
	for _, model := range models {
		mask := rules.maskFor(model)
		if mask == allTiers {
			continue
		}
		if masks == nil {
			masks = make(map[string]tierMask)
		}
		if current, exists := masks[model.ID]; exists {
			mask &= current
		}
		masks[model.ID] = mask
	}
	return masks
}

// accountTierMasksLocked 返回账户 Playground 目录里受规则限制的模型允许的权益（见 catalogMasks）。候选判断对每个账户、每个模型都要
// 查一次，逐次按别名与小写匹配（maskFor）要读每个目录项的别名，账户的目录各自独立、几乎都不在缓存里，在账户池锁内的开销约为
// 候选判断本身的一倍；所以按目录切片与规则版本缓存在账户上，同一份目录内容（accountCatalogKeyLocked）在账户之间共享，
// 规则变化后每种目录只算一次。调用方持有 p.mu
func (p *AccountPool) accountTierMasksLocked(account *Account) map[string]tierMask {
	if account.tierMasksSource != nil && account.tierMasksRules == p.tierRulesVersion && sameModelSlice(account.tierMasksSource, account.Models) {
		return account.tierMasks
	}
	if p.tierMasksByCatalog == nil || p.tierMasksByCatalogRules != p.tierRulesVersion || len(p.tierMasksByCatalog) >= tierMaskCacheLimit {
		p.tierMasksByCatalog = make(map[uint64]map[string]tierMask)
		p.tierMasksByCatalogRules = p.tierRulesVersion
	}
	key := accountCatalogKeyLocked(account)
	masks, done := p.tierMasksByCatalog[key]
	if !done {
		masks = p.tierRules.catalogMasks(account.Models)
		p.tierMasksByCatalog[key] = masks
	}
	account.tierMasks = masks
	account.tierMasksSource = account.Models
	if account.tierMasksSource == nil {
		account.tierMasksSource = []Model{}
	}
	account.tierMasksRules = p.tierRulesVersion
	return masks
}

// tierRuleAllowsAccountModelLocked 判断规则允许账户使用它 Playground 目录里的目录项 model（取自 account.Models）；调用方持有 p.mu
func (p *AccountPool) tierRuleAllowsAccountModelLocked(account *Account, model Model) bool {
	if len(p.tierRules.masks) == 0 {
		return true
	}
	mask, ruled := p.accountTierMasksLocked(account)[model.ID]
	return !ruled || mask&tierBit(account.BenefitTier) != 0
}

// tierRuleAllowsBuildLocked 判断规则允许账户经 Build 使用模型。按哪个目录项匹配（ID 与别名，规则写别名时同样命中）：
//   - 账户自己的 Playground 目录有这个模型时按它自己的目录项，与 Playground 分支（accountSupportsSelectionLocked）判断同一行。
//     各账户目录里同一模型的别名可能不同（Google 逐步更新目录期间），按账户池索引里排在前面的目录项判断会让写别名的规则在两个分支上
//     结论不同：Playground 分支已排除的 Pro 仍经 Build 被选中；
//   - 自己的 Playground 目录没有时按 Playground 索引里的目录项（与 buildSupportsModelLocked 用同一个）；
//   - Build 独有的模型（任何 Playground 目录都没有）按选择的模型 ID 原样匹配。
//
// 调用方持有 p.mu
func (p *AccountPool) tierRuleAllowsBuildLocked(account *Account, modelID string) bool {
	if len(p.tierRules.masks) == 0 {
		return true
	}
	modelID = strings.TrimPrefix(strings.TrimSpace(modelID), "models/")
	if own, exists := accountModelLocked(account, modelID); exists {
		return p.tierRuleAllowsAccountModelLocked(account, own)
	}
	model, exists := p.playgroundModelLocked(modelID)
	if !exists {
		model = Model{ID: modelID}
	}
	return p.tierRules.maskFor(model)&tierBit(account.BenefitTier) != 0
}

// tierRuleExcludesLocked 判断规则把账户排除在模型之外（核对摘要的“权益不符”计数用），口径与 Build 分支相同（先看账户自己的
// Playground 目录项）；调用方持有 p.mu
func (p *AccountPool) tierRuleExcludesLocked(account *Account, modelID string) bool {
	if len(p.tierRules.masks) == 0 || strings.TrimSpace(modelID) == "" {
		return false
	}
	return !p.tierRuleAllowsBuildLocked(account, modelID)
}

// TierRuleHint 为一条比账户目录更严的规则：配置键 Key 里的模型 Model 把权益 Tier 排除在外，但 Accounts 个这一权益的启用账户的
// Playground 目录显示它能用这个模型。Marked 区分两种账户，同一权益两种都有时分成两条、各自计数：
//   - 为真：这些账户的目录项明确标注了允许（访问模式不为空且允许这一权益）。Google 给原先只限 Ultra 的模型开放 Pro 后目录会这样显示，
//     提示运营者确认后从列表里移除；Accounts 只数这样的账户，不把同权益里只是没有标注的算进去；
//   - 为假：这些账户的目录项没有标注访问模式。规则本来就是为这种目录设的，只作说明、不建议移除（见 app 的 checkTierRules）。
//
// Build 目录没有访问模式，不参与对照
type TierRuleHint struct {
	Key      string
	Model    string
	Tier     BenefitTier
	Marked   bool
	Accounts int
}

// TierRuleReport 为规则与账户目录的对照（见 AccountPool.TierRuleReport）。Checked 为号池里已有账户读到目录（没有时无法对照，
// Hints 与 Unmatched 都为空）；Unmatched 为不在任何账户 Playground 或 Build 目录里的规则名称（写错、带了后缀，或模型还没上线）
type TierRuleReport struct {
	Checked   bool
	Hints     []TierRuleHint
	Unmatched []TierRuleName
}

// TierRuleName 为规则里的一个名称及其配置键
type TierRuleName struct {
	Key  string
	Name string
}

// tierRuleReportCache 为按 eligibilityVersion 缓存的对照结果
type tierRuleReportCache struct {
	version uint64
	report  TierRuleReport
}

// TierRuleReport 对照规则与账户目录：哪些规则比目录更严（TierRuleHint）、哪些规则名称不在任何目录里。结果按 eligibilityVersion
// 缓存（目录、权益、启用状态与规则变化时才重算），重算时一次扫描全部账户、按目录内容与权益去重。只读内存，不发上游请求；
// 由后台周期与配置热更新调用，不在请求路径上
func (p *AccountPool) TierRuleReport() TierRuleReport {
	if p == nil {
		return TierRuleReport{}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if cached := p.tierReport; cached != nil && cached.version == p.eligibilityVersion {
		return cloneTierRuleReport(cached.report)
	}
	report := p.tierRuleReportLocked()
	p.tierReport = &tierRuleReportCache{version: p.eligibilityVersion, report: report}
	return cloneTierRuleReport(report)
}

func cloneTierRuleReport(report TierRuleReport) TierRuleReport {
	report.Hints = slices.Clone(report.Hints)
	report.Unmatched = slices.Clone(report.Unmatched)
	return report
}

// tierHintKey 为提示的分组键：规则项、权益与目录是否明确标注了允许
type tierHintKey struct {
	entry  int
	tier   BenefitTier
	marked bool
}

// tierRuleReportLocked 计算 TierRuleReport；调用方持有 p.mu
func (p *AccountPool) tierRuleReportLocked() TierRuleReport {
	var report TierRuleReport
	entries := p.tierRules.entries
	byName := make(map[string][]int, len(entries))
	for index, entry := range entries {
		lower := strings.ToLower(entry.name)
		byName[lower] = append(byName[lower], index)
	}
	present := make(map[string]struct{}, len(byName))
	markPresent := func(name string) {
		if len(byName) == 0 {
			return
		}
		lower := strings.ToLower(name)
		if _, ruled := byName[lower]; ruled {
			present[lower] = struct{}{}
		}
	}
	type catalogTier struct {
		key  uint64
		tier BenefitTier
	}
	// 同一份目录与同一权益的账户结论相同：每种只算一次，记下命中的提示（含这份目录是否明确标注了允许）
	type catalogHints struct {
		keys []tierHintKey
	}
	computed := make(map[catalogTier]*catalogHints)
	counts := make(map[tierHintKey]int)
	for _, account := range p.accounts {
		if account == nil || len(account.Models) == 0 {
			continue
		}
		report.Checked = true
		key := catalogTier{key: accountCatalogKeyLocked(account), tier: account.BenefitTier}
		hints, done := computed[key]
		if !done {
			hints = &catalogHints{}
			computed[key] = hints
			for _, model := range account.Models {
				names := append([]string{model.ID}, model.CapabilityOptions["aliases"]...)
				for _, name := range names {
					markPresent(name)
					for _, index := range byName[strings.ToLower(name)] {
						entry := entries[index]
						if entry.mask&tierBit(account.BenefitTier) != 0 || !modelAllowedByTier(model, account.BenefitTier) {
							continue
						}
						// 同一规则项在这份目录里命中多行（ID 与别名分别命中）时只算一次：任一行明确标注了允许就算标注
						marked := len(model.AccessModes) > 0
						position := slices.IndexFunc(hints.keys, func(key tierHintKey) bool {
							return key.entry == index && key.tier == account.BenefitTier
						})
						if position < 0 {
							hints.keys = append(hints.keys, tierHintKey{entry: index, tier: account.BenefitTier, marked: marked})
						} else if marked {
							hints.keys[position].marked = true
						}
					}
				}
			}
		}
		if !account.Config.Enabled {
			continue
		}
		for _, hint := range hints.keys {
			counts[hint]++
		}
	}
	if !report.Checked {
		return TierRuleReport{}
	}
	// Build 目录只用来判断规则名称在不在某个目录里：Playground 目录里都已找到时不再看；同一份内容只扫描一次（目录同步每次都换
	// 一份切片、内容多半不变，原先每次重算都逐个账户、逐个模型扫描，几千个账户时在账户池锁内要约 10ms）
	buildSeen := make(map[uint64]struct{})
	for _, account := range p.accounts {
		if len(present) == len(byName) {
			break
		}
		if account == nil || len(account.buildModels) == 0 {
			continue
		}
		key := accountBuildKeyLocked(account)
		if _, done := buildSeen[key]; done {
			continue
		}
		buildSeen[key] = struct{}{}
		for _, model := range account.buildModels {
			markPresent(model.ID)
		}
	}
	for hint, accounts := range counts {
		entry := entries[hint.entry]
		report.Hints = append(report.Hints, TierRuleHint{
			Key: entry.key, Model: entry.name, Tier: hint.tier, Marked: hint.marked, Accounts: accounts,
		})
	}
	sort.Slice(report.Hints, func(left int, right int) bool {
		a, b := report.Hints[left], report.Hints[right]
		if a.Key != b.Key {
			return a.Key > b.Key // ULTRA_ONLY_MODELS 在前
		}
		if a.Model != b.Model {
			return a.Model < b.Model
		}
		if a.Tier != b.Tier {
			return slices.Index(ruleTierOrder, a.Tier) < slices.Index(ruleTierOrder, b.Tier)
		}
		return a.Marked && !b.Marked
	})
	for _, entry := range entries {
		if _, found := present[strings.ToLower(entry.name)]; !found {
			report.Unmatched = append(report.Unmatched, TierRuleName{Key: entry.key, Name: entry.name})
		}
	}
	return report
}
