package api

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// 模型名后缀（可以组合，顺序不限，例如 gemini-3.1-pro-preview-128-nothinking-online、gemini-4-argon-low-nothinking）：
//
//	不带后缀     思考强度设为最高（客户端显式传了思考参数时以客户端为准）
//	-128        最低思考：支持思考等级的模型用最低等级，只支持预算的模型用 128 token（与 -minimal 相同）
//	-minimal、-low、-medium、-high
//	            按名称指定思考等级，优先于客户端传入的思考参数；模型不支持该等级时取最接近的等级，
//	            只支持预算的模型换算为 128、1024、8192 token 与最大预算
//	-nothinking 模型照常思考，但不返回思维链
//	-online     开启 Google 搜索
//
// 目录里本身就以等级结尾的模型 ID（例如 xxx-low）不再强制最高思考，按 ID 里的等级设置
const (
	suffixMinThinking = "-128"
	suffixHideThought = "-nothinking"
	suffixOnline      = "-online"
	minThinkingBudget = 128
)

// thinkingLevelSuffixes 为按名称指定思考等级的后缀与对应的等级（reasoning_effort 写法）
var thinkingLevelSuffixes = []struct {
	suffix string
	level  string
}{
	{"-minimal", "minimal"},
	{"-low", "low"},
	{"-medium", "medium"},
	{"-high", "high"},
}

// modelAlias 表示从模型名解析出的后缀；level 为名称指定的思考等级（minimal、low、medium、high），空串表示没有指定
type modelAlias struct {
	base        string
	level       string
	hideThought bool
	online      bool
}

// trailingThinkingLevel 返回名称末尾的思考等级后缀（-minimal、-low、-medium、-high）对应的等级，没有时返回空串
func trailingThinkingLevel(name string) (string, string) {
	lower := strings.ToLower(name)
	for _, entry := range thinkingLevelSuffixes {
		if strings.HasSuffix(lower, entry.suffix) {
			return entry.suffix, entry.level
		}
	}
	return "", ""
}

// aliasVariant 表示模型列表中展示的一个后缀组合
type aliasVariant struct {
	suffix string
	label  string
}

// stripModelSuffix 从 alias.base 末尾剥离一个已知后缀并记到 alias 上，没有可剥离的后缀时返回 false。
// 思考等级后缀出现多个时以最靠后的为准（先剥离的先记）
func stripModelSuffix(alias *modelAlias) bool {
	lower := strings.ToLower(alias.base)
	setLevel := func(level string) {
		if alias.level == "" {
			alias.level = level
		}
	}
	if suffix, level := trailingThinkingLevel(alias.base); suffix != "" {
		setLevel(level)
		alias.base = alias.base[:len(alias.base)-len(suffix)]
		return true
	}
	switch {
	case strings.HasSuffix(lower, suffixHideThought):
		alias.hideThought = true
		alias.base = alias.base[:len(alias.base)-len(suffixHideThought)]
	case strings.HasSuffix(lower, suffixOnline):
		alias.online = true
		alias.base = alias.base[:len(alias.base)-len(suffixOnline)]
	case strings.HasSuffix(lower, suffixMinThinking):
		setLevel("minimal")
		alias.base = alias.base[:len(alias.base)-len(suffixMinThinking)]
	default:
		return false
	}
	return true
}

// parseModelAlias 从名称末尾依次剥离全部已知后缀
func parseModelAlias(name string) modelAlias {
	alias := modelAlias{base: strings.TrimSpace(name)}
	for stripModelSuffix(&alias) {
	}
	return alias
}

// resolveModelAlias 返回请求名称对应的目录模型：名称本身就是目录模型时不解析后缀；否则从末尾一个一个剥离后缀，
// 剥到第一个目录里有的名称为止（目录 ID 本身以 -low、-online 之类结尾时不会被多剥一层）。
// 目录 ID 以思考等级结尾（例如 xxx-low）且后缀没有另外指定等级时按 ID 里的等级设置，不强制最高思考
func resolveModelAlias(models []aistudio.Model, name string) (aistudio.Model, modelAlias, bool) {
	byID := func(id string) (aistudio.Model, bool) {
		for _, model := range models {
			if model.ID == id {
				return model, true
			}
		}
		return aistudio.Model{}, false
	}
	// 客户端配置里复制粘贴的模型名常带首尾空白：先去掉再解析，改写后的 request.Model 也是干净的目录 ID
	name = strings.TrimSpace(name)
	alias := modelAlias{base: name}
	for {
		if model, ok := byID(alias.base); ok {
			if alias.level == "" {
				_, alias.level = trailingThinkingLevel(alias.base)
			}
			return model, alias, true
		}
		if !stripModelSuffix(&alias) || alias.base == "" {
			return aistudio.Model{}, alias, false
		}
	}
}

// isTextChatModel 判断模型是否为文本对话模型；图片、语音、音乐、视频、实时等模型不适用后缀
func isTextChatModel(model aistudio.Model) bool {
	capabilities := model.Capabilities
	if !capabilities["chat_model"] {
		return false
	}
	for _, route := range []string{
		"image_route", "speech_route", "music_route", "video_route", "live_route", "interaction_route",
	} {
		if capabilities[route] {
			return false
		}
	}
	return true
}

func thinkingControllable(model aistudio.Model) bool {
	return thinkingLevelControllable(model) || model.Capabilities["thinking_budget"]
}

// thinkingLevelControllable 判断模型能按思考等级调节：Playground 目录标明支持思考等级，或是只在 Build 通道可用的 Gemini 3 及以后的
// 思考模型（Build 目录只有“会思考”一个字段，没有思考等级与预算；Build 编码按思考等级发送，见 aistudio.buildModelDefaults）。
// 只在 Build 可用的 2.5 等旧模型只接受思考预算，不按等级处理，免得发出它们不认识的 thinkingLevel
func thinkingLevelControllable(model aistudio.Model) bool {
	return model.Capabilities["thinking_level"] || buildOnlyLevelModel(model)
}

// buildOnlyLevelModel 判断模型只在 Build 通道可用、会思考且为 Gemini 3 及以后（这些模型都支持 thinkingLevel 的 LOW 与 HIGH）
func buildOnlyLevelModel(model aistudio.Model) bool {
	if model.Capabilities["thinking_level"] || model.Capabilities["thinking_budget"] || !model.Capabilities["thinking"] {
		return false
	}
	if len(model.Channels) == 0 || slices.Contains(model.Channels, string(aistudio.ChannelPlayground)) {
		return false
	}
	// Build 目录没有图片、语音、实时等路由字段，按 ID 排除这类模型，不给它们发思考等级
	lower := strings.ToLower(model.ID)
	for _, marker := range []string{"image", "tts", "audio", "live", "embedding", "veo", "lyria"} {
		if strings.Contains(lower, marker) {
			return false
		}
	}
	return geminiMajorVersion(model.ID) >= 3
}

// geminiMajorVersion 返回 gemini-<主版本>[.<次版本>]-... 形式模型 ID 的主版本号，不是这种形式时返回 0
func geminiMajorVersion(modelID string) int {
	rest, ok := strings.CutPrefix(strings.ToLower(strings.TrimPrefix(modelID, "models/")), "gemini-")
	if !ok {
		return 0
	}
	digits := 0
	for digits < len(rest) && rest[digits] >= '0' && rest[digits] <= '9' {
		digits++
	}
	if digits == 0 || digits > 3 {
		return 0
	}
	major, err := strconv.Atoi(rest[:digits])
	if err != nil {
		return 0
	}
	return major
}

func modelThinks(model aistudio.Model) bool {
	return model.Capabilities["thinking"] || thinkingControllable(model)
}

// maxThinkingBudget 只支持思考预算的模型（Gemini 2.5 系列）的最大预算
func maxThinkingBudget(modelID string) int64 {
	if strings.Contains(strings.ToLower(modelID), "pro") {
		return 32768
	}
	return 24576
}

// applyThinkingAlias 设置思考强度：requested 为名称指定的等级（minimal、low、medium、high），优先于客户端传入的思考参数；
// 为空时设为最高，客户端显式传了思考参数时以客户端为准。支持思考等级的模型写 reasoning_effort（编码时按模型支持的等级取最接近的），
// 只支持思考预算的模型换算为预算
func applyThinkingAlias(config *aistudio.GenerationConfig, model aistudio.Model, requested string) {
	level := thinkingLevelControllable(model)
	if !level && !model.Capabilities["thinking_budget"] {
		return
	}
	explicit := strings.TrimSpace(config.ReasoningEffort) != "" || config.ThinkingBudget != nil
	if explicit && requested == "" {
		return
	}
	if requested == "" {
		requested = "high"
	}
	config.ReasoningEffort = ""
	config.ThinkingBudget = nil
	if level {
		// 只在 Build 可用的模型没有目录里的等级列表，编码时无法换成最接近的等级；Gemini 3 Pro、4 Argon 等不支持 MINIMAL，按 LOW 发送
		if requested == "minimal" && buildOnlyLevelModel(model) {
			requested = "low"
		}
		config.ReasoningEffort = requested
		return
	}
	value := thinkingBudgetForAlias(model.ID, requested)
	config.ThinkingBudget = &value
}

// thinkingBudgetForAlias 把名称指定的思考等级换算为只支持预算的模型的预算：最低 128、低 1024、中 8192、最高为模型的最大预算
func thinkingBudgetForAlias(modelID string, level string) int64 {
	switch level {
	case "minimal":
		return minThinkingBudget
	case "low":
		return 1024
	case "medium":
		return 8192
	default:
		return maxThinkingBudget(modelID)
	}
}

// aliasModelCacheTTL 为后缀解析使用的模型目录缓存时长；目录变化最多延迟这么久被后缀解析看到
// aliasModelCacheTTL 为别名目录快照的有效期。计算一次要在账户池锁内检查全部账户的全部模型（账户多时几百毫秒），
// 模型目录很少变化，过期后先继续用旧快照，由后台刷新
const aliasModelCacheTTL = 30 * time.Second

type aliasModelSnapshot struct {
	models  []aistudio.Model
	expires time.Time
}

// aliasCatalogCache 为一个号池的别名目录快照：refreshMu 保证没有快照时只有一个请求计算目录，refreshing 保证后台同时只有一个刷新
type aliasCatalogCache struct {
	models     atomic.Pointer[aliasModelSnapshot]
	refreshMu  sync.Mutex
	refreshing atomic.Bool
}

// aliasCache 返回请求号池的别名目录缓存：各号池的模型目录不同，不能共用快照
func (s *server) aliasCache(ctx context.Context) (*aliasCatalogCache, aistudio.PoolScope) {
	scope := aistudio.PoolScopeFromContext(ctx)
	if int(scope) >= len(s.aliasCaches) {
		scope = aistudio.PoolScopeAll
	}
	return &s.aliasCaches[scope], scope
}

// aliasCatalog 返回后缀解析用的模型目录。Models 需要在账户池锁内按账户计算各模型可用通道，
// 账户多、请求多时逐请求计算会加剧锁竞争；后缀解析只需要模型 ID 与能力，短时缓存即可
// aliasCatalog 返回别名解析用的模型目录。每个生成请求都会调用：原先快照 3 秒就过期，且过期那一刻到达的每个请求
// 都各自在账户池锁内重算一遍全部账户的模型，高并发时把账户池锁占满（诊断中锁占用率超过 100%，状态接口卡死）。
// 现在已有快照时一律立即返回（过期的在后台只刷新一次），只有还没有快照时才同步计算，且同一时间只算一次。
// 快照按请求号池分别缓存（见 aliasCache）
func (s *server) aliasCatalog(ctx context.Context) ([]aistudio.Model, error) {
	cache, scope := s.aliasCache(ctx)
	if cached := cache.models.Load(); cached != nil {
		if time.Now().After(cached.expires) {
			s.refreshAliasCatalogAsync(cache, scope)
		}
		return cached.models, nil
	}
	cache.refreshMu.Lock()
	defer cache.refreshMu.Unlock()
	if cached := cache.models.Load(); cached != nil {
		return cached.models, nil
	}
	models, err := s.service.Models(ctx)
	if err != nil {
		return nil, err
	}
	cache.models.Store(&aliasModelSnapshot{models: models, expires: time.Now().Add(aliasModelCacheTTL)})
	return models, nil
}

// refreshAliasCatalogAsync 在后台刷新号池的别名目录，同一号池同一时间最多一个刷新；失败时保留旧快照，下次过期再试
func (s *server) refreshAliasCatalogAsync(cache *aliasCatalogCache, scope aistudio.PoolScope) {
	if !cache.refreshing.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer cache.refreshing.Store(false)
		ctx, cancel := context.WithTimeout(aistudio.ContextWithPoolScope(context.Background(), scope), 2*time.Minute)
		defer cancel()
		models, err := s.service.Models(ctx)
		if err != nil {
			return
		}
		cache.models.Store(&aliasModelSnapshot{models: models, expires: time.Now().Add(aliasModelCacheTTL)})
	}()
}

// applyModelAlias 解析模型后缀并改写请求：换成目录模型名，按后缀设置思考强度与联网搜索。
// 返回是否需要隐藏思维链。未知模型保持原样，由上游返回原有错误。
func (s *server) applyModelAlias(ctx context.Context, request *aistudio.GenerateRequest) bool {
	models, err := s.aliasCatalog(ctx)
	if err != nil {
		return false
	}
	model, alias, ok := resolveModelAlias(models, request.Model)
	if !ok || !isTextChatModel(model) {
		return false
	}
	request.Model = model.ID
	applyThinkingAlias(&request.Config, model, alias.level)
	if alias.online && model.Capabilities["google_search"] && request.Tools.ToolConfig.Mode != "none" {
		request.Tools.Google = appendUnique(request.Tools.Google, "google_search")
	}
	return alias.hideThought && modelThinks(model)
}

// resolveModelName 把带后缀的名称换成目录模型名，供计数等只需要模型名的接口使用
func (s *server) resolveModelName(ctx context.Context, name string) string {
	models, err := s.aliasCatalog(ctx)
	if err != nil {
		return name
	}
	if model, _, ok := resolveModelAlias(models, name); ok {
		return model.ID
	}
	return name
}

// aliasVariants 返回模型列表中为该模型展示的后缀组合
func aliasVariants(model aistudio.Model) []aliasVariant {
	if !isTextChatModel(model) {
		return nil
	}
	var variants []aliasVariant
	// 目录 ID 本身以思考等级结尾时不再追加等级后缀（xxx-low-medium 没有意义）
	levelled, _ := trailingThinkingLevel(model.ID)
	if thinkingControllable(model) && levelled == "" {
		variants = append(variants,
			aliasVariant{suffix: suffixMinThinking, label: "最低思考"},
			aliasVariant{suffix: "-low", label: "低思考"},
			aliasVariant{suffix: "-medium", label: "中等思考"},
		)
	}
	if modelThinks(model) {
		variants = append(variants, aliasVariant{suffix: suffixHideThought, label: "隐藏思维链"})
	}
	if thinkingControllable(model) && levelled == "" {
		variants = append(variants, aliasVariant{
			suffix: suffixMinThinking + suffixHideThought, label: "最低思考 · 隐藏思维链",
		})
	}
	if model.Capabilities["google_search"] {
		variants = append(variants, aliasVariant{suffix: suffixOnline, label: "联网搜索"})
	}
	return variants
}

// expandModelAliases 在公开模型列表中为文本对话模型追加后缀别名，客户端可直接选择
func expandModelAliases(models []aistudio.Model) []aistudio.Model {
	expanded := make([]aistudio.Model, 0, len(models)*3)
	for _, model := range models {
		expanded = append(expanded, model)
		for _, variant := range aliasVariants(model) {
			alias := model
			alias.ID = model.ID + variant.suffix
			alias.Name = model.Name + " · " + variant.label
			expanded = append(expanded, alias)
		}
	}
	return expanded
}

// lookupPublicModel 按单模型查询的名称查找模型：先匹配列表中的正式 ID 与后缀别名，再匹配目录别名，
// 最后解析列表里没有展开的后缀组合（如 -nothinking-online）。目录别名返回对应的正式模型，后缀组合返回请求的名称
func lookupPublicModel(models []aistudio.Model, name string) (aistudio.Model, bool) {
	for _, model := range expandModelAliases(models) {
		if model.ID == name {
			return model, true
		}
	}
	for _, model := range models {
		for _, alias := range model.CapabilityOptions["aliases"] {
			if alias == name {
				return model, true
			}
		}
	}
	if model, alias, ok := resolveModelAlias(models, name); ok && alias.base != name && isTextChatModel(model) {
		model.ID = name
		return model, true
	}
	return aistudio.Model{}, false
}

// withHiddenReasoning 在 -nothinking 时丢弃思维链文本，保留思考签名与用量，
// 多轮工具调用所需的签名不会丢失
func withHiddenReasoning(ctx context.Context, events <-chan aistudio.Event, hide bool) <-chan aistudio.Event {
	if !hide {
		return events
	}
	out := make(chan aistudio.Event, 16)
	go func() {
		defer close(out)
		for event := range events {
			if event.Kind == aistudio.EventReasoning {
				switch {
				case event.ThoughtSignature != "":
					event = aistudio.Event{
						Kind: aistudio.EventThoughtSignature, ThoughtSignature: event.ThoughtSignature,
						Usage: event.Usage, ProviderModel: event.ProviderModel,
					}
				case event.Usage != nil:
					event = aistudio.Event{Kind: aistudio.EventUsage, Usage: event.Usage, ProviderModel: event.ProviderModel}
				default:
					continue
				}
			}
			select {
			case out <- event:
			case <-ctx.Done():
				// 客户端已断开：继续读完上游，让生产方正常结束
				for range events {
				}
				return
			}
		}
	}()
	return out
}
