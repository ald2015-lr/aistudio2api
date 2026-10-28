package aistudio

import (
	"slices"
	"sort"
	"strings"
	"time"
)

// Channel 表示生成请求的上游额度来源
type Channel string

const (
	// ChannelPlayground 表示官网 Playground 的 GenerateContent
	ChannelPlayground Channel = "playground"
	// ChannelBuild 表示官网 Build 应用代理的 Gemini API 调用
	ChannelBuild Channel = "build"
)

// ChannelCooldownScope 返回通道在作用域上的冷却键，Build 使用 build:<作用域>
func ChannelCooldownScope(channel Channel, scope string) string {
	scope = strings.TrimSpace(scope)
	if channel == ChannelBuild && scope != "" {
		return ModelAccessKey(string(ChannelBuild), scope)
	}
	return scope
}

// channelCandidate 表示一个账户与通道组合
type channelCandidate struct {
	index   int
	channel Channel
}

// SetUpstreamChannels 设置生成请求按顺序启用的上游通道
func (p *AccountPool) SetUpstreamChannels(channels []Channel) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.channels = append([]Channel(nil), channels...)
	p.channelsNoBuild = nil
	for _, channel := range p.enabledChannelsLocked() {
		if channel != ChannelBuild {
			p.channelsNoBuild = append(p.channelsNoBuild, channel)
		}
	}
	// 通道顺序决定每个候选集的通道与冷却键，整个候选索引重建
	p.eligibilityVersion++
	p.invalidateSchedulingLocked()
	p.notifyLocked()
}

// SetBuildCatalog 保存账户 Build 代理返回的 Gemini API 模型目录
func (p *AccountPool) SetBuildCatalog(accountID string, models []Model) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	account := p.byID[strings.TrimSpace(accountID)]
	if account == nil {
		return ErrAccountNotFound
	}
	account.buildModels = cloneAccountModels(models)
	p.eligibilityVersion++
	p.accountChangedLocked(account)
	p.notifyLocked()
	return nil
}

// AccountsWithoutBuildCatalog 返回还没有 Build 目录（没有同步过，也没有从写盘缓存恢复）的启用账户，按账户顺序
func (p *AccountPool) AccountsWithoutBuildCatalog() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var ids []string
	for _, account := range p.accounts {
		if account != nil && account.Config.Enabled && account.buildModels == nil {
			ids = append(ids, account.ID)
		}
	}
	return ids
}

// BuildEnabled 返回 Build 通道是否启用
func (p *AccountPool) BuildEnabled() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.channelEnabledLocked(ChannelBuild)
}

// PlaygroundEnabled 返回 Playground 通道是否启用
func (p *AccountPool) PlaygroundEnabled() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.channelEnabledLocked(ChannelPlayground)
}

// Channel 返回租约本次使用的上游通道
func (l *AccountLease) Channel() Channel {
	if l == nil || l.channel == "" {
		return ChannelPlayground
	}
	return l.channel
}

// CooldownScope 返回租约通道在模型作用域上的冷却键
func (l *AccountLease) CooldownScope(scope string) string {
	return ChannelCooldownScope(l.Channel(), scope)
}

func (p *AccountPool) channelEnabledLocked(channel Channel) bool {
	if len(p.channels) == 0 {
		return channel == ChannelPlayground
	}
	return slices.Contains(p.channels, channel)
}

func (p *AccountPool) enabledChannelsLocked() []Channel {
	if len(p.channels) == 0 {
		return []Channel{ChannelPlayground}
	}
	return p.channels
}

// generationChannelSelection 判断选择是否属于按通道调度的生成请求
func generationChannelSelection(selection AccountSelection) bool {
	return strings.TrimSpace(selection.ModelID) != "" && selection.Method == "generateContent" &&
		strings.TrimSpace(selection.ModelAccessScope) == "" && strings.TrimSpace(selection.ResourceID) == "" &&
		strings.TrimSpace(selection.Capability) == "" && !selection.PlaygroundOnly
}

// selectionChannelsLocked 返回选择可使用的通道顺序；非生成请求只使用 Playground RPC。
// Build 生成全局暂停时（见 build_pause.go）生成请求把 Build 当作没有启用，试探请求（BuildProbe）除外
func (p *AccountPool) selectionChannelsLocked(selection AccountSelection) []Channel {
	if !generationChannelSelection(selection) {
		return []Channel{ChannelPlayground}
	}
	buildOpen := p.channelEnabledLocked(ChannelBuild) && (selection.BuildProbe || !p.buildPaused)
	if selection.BuildOnly {
		if buildOpen {
			return []Channel{ChannelBuild}
		}
		return nil
	}
	if selection.PlaygroundFirst && p.channelEnabledLocked(ChannelPlayground) {
		return []Channel{ChannelPlayground}
	}
	if !buildOpen && p.channelEnabledLocked(ChannelBuild) {
		return p.channelsNoBuild
	}
	return p.enabledChannelsLocked()
}

// channelSupportsLocked 判断账户的通道目录是否支持选择
func (p *AccountPool) channelSupportsLocked(account *Account, channel Channel, selection AccountSelection) bool {
	if strings.TrimSpace(selection.ModelID) == "" {
		return channel == ChannelPlayground
	}
	switch channel {
	case ChannelPlayground:
		if selection.BuildOnly || generationChannelSelection(selection) && !p.channelEnabledLocked(ChannelPlayground) {
			return false
		}
		return accountSupportsSelection(account, selection)
	case ChannelBuild:
		// Build 生成暂停（全局或该账户，见 build_pause.go）时不经 Build 接生成请求，试探请求除外
		return generationChannelSelection(selection) && p.channelEnabledLocked(ChannelBuild) &&
			!p.buildPauseActiveLocked(account, selection) && p.buildSupportsModelLocked(account, selection.ModelID)
	default:
		return false
	}
}

// buildCatalogSupportsLocked 判断账户的 Build 目录支持这个生成选择，不看 Build 通道是否启用、是否暂停：
// 已经取得的 Build 租约发送前的校验用它（见 validateLeaseSelection）
func (p *AccountPool) buildCatalogSupportsLocked(account *Account, selection AccountSelection) bool {
	return generationChannelSelection(selection) && p.buildSupportsModelLocked(account, selection.ModelID)
}

// accountSupportsAnyChannelLocked 判断账户至少有一个通道支持选择
func (p *AccountPool) accountSupportsAnyChannelLocked(account *Account, selection AccountSelection) bool {
	for _, channel := range p.selectionChannelsLocked(selection) {
		if p.channelSupportsLocked(account, channel, selection) {
			return true
		}
	}
	return false
}

// accountChannelCooldownLocked 返回账户全部支持通道都冷却时的最早恢复时间
func (p *AccountPool) accountChannelCooldownLocked(account *Account, selection AccountSelection, now time.Time) (time.Time, bool) {
	scope := selectionAccessScope(selection)
	var earliest time.Time
	for _, channel := range p.selectionChannelsLocked(selection) {
		if !p.channelSupportsLocked(account, channel, selection) {
			continue
		}
		cooldown, active := accountCooldown(account, ChannelCooldownScope(channel, scope), now)
		if !active {
			return time.Time{}, false
		}
		if earliest.IsZero() || cooldown.Until.Before(earliest) {
			earliest = cooldown.Until
		}
	}
	return earliest, !earliest.IsZero()
}

// channelCandidatesLocked 按账户 ID 与通道顺序展开候选，轮询策略从上次选中组合之后开始；
// PreferPlayground 时每个账户先 Playground，轮询从上次选中账户之后开始，不在同一账户的通道之间轮询
func (p *AccountPool) channelCandidatesLocked(indices []int, selection AccountSelection) []channelCandidate {
	channels := p.selectionChannelsLocked(selection)
	preferPlayground := selection.PreferPlayground && len(channels) > 1 && slices.Contains(channels, ChannelPlayground)
	if preferPlayground && channels[0] != ChannelPlayground {
		// 通道顺序是账户池共用的切片，另建一份
		ordered := make([]Channel, 0, len(channels))
		ordered = append(ordered, ChannelPlayground)
		for _, channel := range channels {
			if channel != ChannelPlayground {
				ordered = append(ordered, channel)
			}
		}
		channels = ordered
	}
	candidates := make([]channelCandidate, 0, len(indices)*len(channels))
	for _, index := range indices {
		for _, channel := range channels {
			candidates = append(candidates, channelCandidate{index: index, channel: channel})
		}
	}
	rank := func(channel Channel) int { return slices.Index(channels, channel) }
	// selectionIndicesLocked 返回的下标已按账户 ID 有序，展开后的候选天然满足排序要求，只在无序时才排序。
	// 原先每次选号都在账户池锁内对"账户数 × 通道数"个候选排序
	if !p.indicesSortedByIDLocked(indices) {
		sort.SliceStable(candidates, func(left, right int) bool {
			leftID, rightID := p.accounts[candidates[left].index].ID, p.accounts[candidates[right].index].ID
			if leftID != rightID {
				return leftID < rightID
			}
			return rank(candidates[left].channel) < rank(candidates[right].channel)
		})
	}
	if p.routingStrategy != "round-robin" || len(indices) <= 1 && len(channels) <= 1 {
		return candidates
	}
	key := selectionAccessScope(selection)
	lastAccount := p.lastPicked[key]
	lastChannel := p.lastPickedChannel[key]
	start := sort.Search(len(candidates), func(position int) bool {
		account := p.accounts[candidates[position].index].ID
		if account != lastAccount {
			return account > lastAccount
		}
		return !preferPlayground && lastChannel != "" && rank(candidates[position].channel) > rank(lastChannel)
	})
	return append(candidates[start:], candidates[:start]...)
}

// buildSupportsModelLocked 判断账户 Build 目录可经生成请求调用模型
func (p *AccountPool) buildSupportsModelLocked(account *Account, modelID string) bool {
	modelID = strings.TrimPrefix(strings.TrimSpace(modelID), "models/")
	if _, found := accountBuildGeneratesLocked(account)[modelID]; !found {
		return false
	}
	if model, exists := p.playgroundModelLocked(modelID); exists {
		return !buildExcludedPlaygroundModel(model) && modelAllowedByTier(model, account.BenefitTier)
	}
	return !buildRequiresSpecialTool(modelID)
}

// playgroundModelLocked 按 ID 或别名查找 Playground 目录中的模型，结果与按账户顺序逐个 modelMatchesID
// 找到的第一个模型相同。原实现每次都扫描全部账户的全部模型；buildOnlyModelsLocked 对每个启用账户、
// 每个 Build 模型都要调用一次，账户上千时是随账户数平方增长的计算，并且全程占着账户池锁
func (p *AccountPool) playgroundModelLocked(modelID string) (Model, bool) {
	p.ensurePlaygroundIndexLocked()
	model, exists := p.playgroundIndex[modelID]
	return model, exists
}

// ensurePlaygroundIndexLocked 在索引缺失，或目录版本变化且节流允许时重建 Playground 模型索引；调用方持有 p.mu
func (p *AccountPool) ensurePlaygroundIndexLocked() {
	if p.playgroundIndex != nil && (p.playgroundIndexVersion == p.catalogVersion || !p.playgroundIndexRebuildDue()) {
		return
	}
	index := make(map[string]Model)
	for _, account := range p.accounts {
		if account == nil {
			continue
		}
		for _, model := range account.Models {
			if _, exists := index[model.ID]; !exists {
				index[model.ID] = model
			}
			for _, alias := range model.CapabilityOptions["aliases"] {
				if _, exists := index[alias]; !exists {
					index[alias] = model
				}
			}
		}
	}
	p.playgroundIndex = index
	p.playgroundIndexVersion = p.catalogVersion
	p.playgroundIndexBuiltAt = time.Now()
}

// playgroundIndexCurrentLocked 返回 Playground 模型索引（节流允许时先重建）是否与当前目录版本一致。Build 通道能否调用模型
// 要查这个索引（见 buildSupportsModelLocked）：按目录版本缓存的派生结果（状态口径的冷却期限、含 Build 通道的候选集）重算时
// 必须以同一版本的索引为准，否则会把节流中的旧索引烙进缓存，要等下一次目录变化才纠正；索引落后时调用方跳过这一轮、
// 节流过后再算，不额外重建索引；调用方持有 p.mu
func (p *AccountPool) playgroundIndexCurrentLocked() bool {
	p.ensurePlaygroundIndexLocked()
	return p.playgroundIndexVersion == p.catalogVersion
}

// buildExcludedPlaygroundModel 判断 Playground 目录模型是否使用 Build 未接入的专用路由
func buildExcludedPlaygroundModel(model Model) bool {
	return model.Capabilities["interactions_api"] || model.Capabilities["interaction_route"] ||
		model.Capabilities["transcription_output"]
}

// buildRequiresSpecialTool 判断 Build 独有模型是否只能配合 Computer Use 等专用工具调用
func buildRequiresSpecialTool(modelID string) bool {
	return strings.Contains(modelID, "computer-use")
}

// buildOnlyModelsLocked 返回启用账户 Build 目录中 Playground 目录没有的可生成模型。
// 对 Playground 没有的模型，buildSupportsModelLocked 的结论只取决于"该账户 Build 目录里这个模型可生成"
// 与"不是只能配合专用工具的模型"，这里直接按此判断，每个账户只遍历一次自己的 Build 目录
func (p *AccountPool) buildOnlyModelsLocked() []Model {
	if !p.channelEnabledLocked(ChannelBuild) {
		return nil
	}
	var models []Model
	seen := make(map[string]struct{})
	for _, account := range p.accounts {
		if account == nil || !account.Config.Enabled || len(account.buildModels) == 0 {
			continue
		}
		generates := accountBuildGeneratesLocked(account)
		for _, model := range account.buildModels {
			if _, exists := seen[model.ID]; exists {
				continue
			}
			if _, exists := p.playgroundModelLocked(model.ID); exists {
				continue
			}
			if _, generate := generates[model.ID]; !generate || buildRequiresSpecialTool(model.ID) {
				continue
			}
			seen[model.ID] = struct{}{}
			models = append(models, cloneAccountModels([]Model{model})[0])
		}
	}
	return models
}

func (p *AccountPool) hasPlaygroundModelLocked(modelID string) bool {
	_, exists := p.playgroundModelLocked(modelID)
	return exists
}

func (p *AccountPool) hasBuildModelLocked(modelID string, method string, scope PoolScope) bool {
	if !p.channelEnabledLocked(ChannelBuild) {
		return false
	}
	for _, account := range p.accounts {
		if account == nil || !p.inPoolLocked(scope, account) {
			continue
		}
		for _, model := range account.buildModels {
			if model.ID == modelID && (method == "" || hasMethod(model, method)) && p.buildSupportsModelLocked(account, modelID) {
				return true
			}
		}
	}
	return false
}

// modelChannelsInLocked 返回号池内至少一个启用账户可以调用模型的通道。结果按目录版本记忆（见 pool_catalog_cache.go）：
// 目录列表每个模型都要算一次，没有账户支持的模型（如 Ultra 号池列普通目录）要扫完全部账户
func (p *AccountPool) modelChannelsInLocked(model Model, scope PoolScope) []string {
	// 模型列表只看目录与通道配置，不看 Build 生成暂停：暂停期间只能经 Build 调用的模型仍然列出，请求时给出清楚的错误
	selection := AccountSelection{ModelID: model.ID, BuildProbe: true}
	if hasMethod(model, "generateContent") {
		selection.Method = "generateContent"
	}
	memo := p.modelChannelsMemoLocked(scope)
	key := model.ID + "\x00" + selection.Method
	if channels, done := memo.channels[key]; done {
		return slices.Clone(channels)
	}
	var channels []string
	for _, channel := range p.selectionChannelsLocked(selection) {
		for _, account := range p.accounts {
			if account != nil && account.Config.Enabled && p.inPoolLocked(scope, account) && p.channelSupportsLocked(account, channel, selection) {
				channels = append(channels, string(channel))
				break
			}
		}
	}
	memo.channels[key] = channels
	return slices.Clone(channels)
}

// AccountChannelAvailable 返回账户是否还有支持选择且未冷却的通道
func (p *AccountPool) AccountChannelAvailable(accountID string, selection AccountSelection) bool {
	if p == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	account := p.byID[strings.TrimSpace(accountID)]
	if account == nil || !account.Config.Enabled || !p.accountSupportsAnyChannelLocked(account, selection) {
		return false
	}
	_, cooling := p.accountChannelCooldownLocked(account, selection, time.Now())
	return !cooling
}

const (
	// 账户数超过 playgroundIndexThrottleAccounts 时，Playground 模型索引最多每 playgroundIndexRebuildInterval 重建一次
	playgroundIndexThrottleAccounts = 200
	playgroundIndexRebuildInterval  = time.Second
)

// playgroundIndexRebuildDue 判断目录变化后是否立即重建索引。启动后逐个同步账户目录期间目录版本几乎每次调度都在变，
// 每次都在账户池锁内全量重建（账户数 × 模型数）会让所有调度排队；诊断快照里抓到的持锁者正是这段重建。
// 账户多时限制为每秒最多一次，模型元数据最多晚一秒生效，不影响路由
func (p *AccountPool) playgroundIndexRebuildDue() bool {
	return len(p.accounts) <= playgroundIndexThrottleAccounts ||
		time.Since(p.playgroundIndexBuiltAt) >= playgroundIndexRebuildInterval
}

// indicesSortedByIDLocked 判断下标是否已按账户 ID 升序
func (p *AccountPool) indicesSortedByIDLocked(indices []int) bool {
	for position := 1; position < len(indices); position++ {
		if p.accounts[indices[position-1]].ID > p.accounts[indices[position]].ID {
			return false
		}
	}
	return true
}

// accountChannelsCooldown 与 accountChannelCooldownLocked 相同，但直接使用已经判断过的支持通道
func accountChannelsCooldown(account *Account, channels []Channel, selection AccountSelection, now time.Time) (time.Time, bool) {
	scope := selectionAccessScope(selection)
	var earliest time.Time
	for _, channel := range channels {
		cooldown, active := accountCooldown(account, ChannelCooldownScope(channel, scope), now)
		if !active {
			return time.Time{}, false
		}
		if earliest.IsZero() || cooldown.Until.Before(earliest) {
			earliest = cooldown.Until
		}
	}
	return earliest, !earliest.IsZero()
}
