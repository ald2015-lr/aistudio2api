package api

import (
	"context"
	"strings"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// 模型名后缀（可以组合，顺序不限，例如 gemini-3.1-pro-preview-128-nothinking-online）：
//
//	不带后缀     思考强度设为最高（客户端显式传了思考参数时以客户端为准）
//	-128        最低思考：支持思考等级的模型用最低等级，只支持预算的模型用 128 token
//	-nothinking 模型照常思考，但不返回思维链
//	-online     开启 Google 搜索
const (
	suffixMinThinking = "-128"
	suffixHideThought = "-nothinking"
	suffixOnline      = "-online"
	minThinkingBudget = 128
)

// modelAlias 表示从模型名解析出的后缀
type modelAlias struct {
	base        string
	minThinking bool
	hideThought bool
	online      bool
}

// aliasVariant 表示模型列表中展示的一个后缀组合
type aliasVariant struct {
	suffix string
	label  string
}

// parseModelAlias 从名称末尾依次剥离已知后缀
func parseModelAlias(name string) modelAlias {
	alias := modelAlias{base: strings.TrimSpace(name)}
	for {
		lower := strings.ToLower(alias.base)
		switch {
		case strings.HasSuffix(lower, suffixHideThought):
			alias.hideThought = true
			alias.base = alias.base[:len(alias.base)-len(suffixHideThought)]
		case strings.HasSuffix(lower, suffixOnline):
			alias.online = true
			alias.base = alias.base[:len(alias.base)-len(suffixOnline)]
		case strings.HasSuffix(lower, suffixMinThinking):
			alias.minThinking = true
			alias.base = alias.base[:len(alias.base)-len(suffixMinThinking)]
		default:
			return alias
		}
	}
}

// resolveModelAlias 返回请求名称对应的目录模型；名称本身就是目录模型时不解析后缀
func resolveModelAlias(models []aistudio.Model, name string) (aistudio.Model, modelAlias, bool) {
	for _, model := range models {
		if model.ID == name {
			return model, modelAlias{base: name}, true
		}
	}
	alias := parseModelAlias(name)
	if alias.base == name || alias.base == "" {
		return aistudio.Model{}, alias, false
	}
	for _, model := range models {
		if model.ID == alias.base {
			return model, alias, true
		}
	}
	return aistudio.Model{}, alias, false
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
	return model.Capabilities["thinking_level"] || model.Capabilities["thinking_budget"]
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

// applyThinkingAlias 设置思考强度：minimum 为最低，否则为最高
func applyThinkingAlias(config *aistudio.GenerationConfig, model aistudio.Model, minimum bool) {
	level := model.Capabilities["thinking_level"]
	if !level && !model.Capabilities["thinking_budget"] {
		return
	}
	explicit := strings.TrimSpace(config.ReasoningEffort) != "" || config.ThinkingBudget != nil
	if explicit && !minimum {
		return
	}
	config.ReasoningEffort = ""
	config.ThinkingBudget = nil
	if level {
		if minimum {
			config.ReasoningEffort = "minimal"
		} else {
			config.ReasoningEffort = "high"
		}
		return
	}
	value := maxThinkingBudget(model.ID)
	if minimum {
		value = minThinkingBudget
	}
	config.ThinkingBudget = &value
}

// aliasModelCacheTTL 为后缀解析使用的模型目录缓存时长；目录变化最多延迟这么久被后缀解析看到
// aliasModelCacheTTL 为别名目录快照的有效期。计算一次要在账户池锁内检查全部账户的全部模型（账户多时几百毫秒），
// 模型目录很少变化，过期后先继续用旧快照，由后台刷新
const aliasModelCacheTTL = 30 * time.Second

type aliasModelSnapshot struct {
	models  []aistudio.Model
	expires time.Time
}

// aliasCatalog 返回后缀解析用的模型目录。Models 需要在账户池锁内按账户计算各模型可用通道，
// 账户多、请求多时逐请求计算会加剧锁竞争；后缀解析只需要模型 ID 与能力，短时缓存即可
// aliasCatalog 返回别名解析用的模型目录。每个生成请求都会调用：原先快照 3 秒就过期，且过期那一刻到达的每个请求
// 都各自在账户池锁内重算一遍全部账户的模型，高并发时把账户池锁占满（诊断中锁占用率超过 100%，状态接口卡死）。
// 现在已有快照时一律立即返回（过期的在后台只刷新一次），只有还没有快照时才同步计算，且同一时间只算一次
func (s *server) aliasCatalog(ctx context.Context) ([]aistudio.Model, error) {
	if cached := s.aliasModels.Load(); cached != nil {
		if time.Now().After(cached.expires) {
			s.refreshAliasCatalogAsync()
		}
		return cached.models, nil
	}
	s.aliasRefreshMu.Lock()
	defer s.aliasRefreshMu.Unlock()
	if cached := s.aliasModels.Load(); cached != nil {
		return cached.models, nil
	}
	models, err := s.service.Models(ctx)
	if err != nil {
		return nil, err
	}
	s.aliasModels.Store(&aliasModelSnapshot{models: models, expires: time.Now().Add(aliasModelCacheTTL)})
	return models, nil
}

// refreshAliasCatalogAsync 在后台刷新别名目录，同一时间最多一个刷新；失败时保留旧快照，下次过期再试
func (s *server) refreshAliasCatalogAsync() {
	if !s.aliasRefreshing.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer s.aliasRefreshing.Store(false)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		models, err := s.service.Models(ctx)
		if err != nil {
			return
		}
		s.aliasModels.Store(&aliasModelSnapshot{models: models, expires: time.Now().Add(aliasModelCacheTTL)})
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
	applyThinkingAlias(&request.Config, model, alias.minThinking)
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
	if thinkingControllable(model) {
		variants = append(variants, aliasVariant{suffix: suffixMinThinking, label: "最低思考"})
	}
	if modelThinks(model) {
		variants = append(variants, aliasVariant{suffix: suffixHideThought, label: "隐藏思维链"})
	}
	if thinkingControllable(model) {
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
