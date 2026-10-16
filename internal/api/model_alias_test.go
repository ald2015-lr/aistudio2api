package api

import (
	"slices"
	"testing"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// TestParseModelAliasThinkingLevels 思考等级后缀（-minimal、-low、-medium、-high、-128）与其他后缀可以组合、顺序不限、不分大小写；
// 出现多个等级时以最靠后的为准
func TestParseModelAliasThinkingLevels(t *testing.T) {
	for _, test := range []struct {
		name   string
		base   string
		level  string
		hide   bool
		online bool
	}{
		{name: "gemini-4-argon", base: "gemini-4-argon"},
		{name: "gemini-4-argon-low", base: "gemini-4-argon", level: "low"},
		{name: "gemini-4-argon-medium", base: "gemini-4-argon", level: "medium"},
		{name: "gemini-4-argon-HIGH", base: "gemini-4-argon", level: "high"},
		{name: "gemini-4-argon-minimal", base: "gemini-4-argon", level: "minimal"},
		{name: "gemini-4-argon-medium-nothinking", base: "gemini-4-argon", level: "medium", hide: true},
		{name: "gemini-4-argon-nothinking-low-online", base: "gemini-4-argon", level: "low", hide: true, online: true},
		{name: "gemini-3.1-pro-preview-128", base: "gemini-3.1-pro-preview", level: "minimal"},
		{name: "gemini-3.1-pro-preview-128-nothinking-online", base: "gemini-3.1-pro-preview", level: "minimal", hide: true, online: true},
		{name: "gemini-4-argon-low-128", base: "gemini-4-argon", level: "minimal"},
		{name: "gemini-4-argon-128-medium", base: "gemini-4-argon", level: "medium"},
	} {
		alias := parseModelAlias(test.name)
		if alias.base != test.base || alias.level != test.level || alias.hideThought != test.hide || alias.online != test.online {
			t.Fatalf("%s 解析为 %+v，期望 base=%s level=%q hide=%v online=%v", test.name, alias, test.base, test.level, test.hide, test.online)
		}
	}
}

func aliasTestModels() []aistudio.Model {
	return []aistudio.Model{
		{ID: "gemini-4-argon", Name: "Gemini 4 Argon", Capabilities: map[string]bool{"chat_model": true, "thinking": true, "thinking_level": true, "google_search": true}},
		{ID: "gemini-2.5-pro", Name: "Gemini 2.5 Pro", Capabilities: map[string]bool{"chat_model": true, "thinking": true, "thinking_budget": true}},
		{ID: "vendor-model-low", Name: "Vendor Low", Capabilities: map[string]bool{"chat_model": true, "thinking": true, "thinking_level": true}},
		{ID: "plain-chat", Name: "Plain", Capabilities: map[string]bool{"chat_model": true}},
		{ID: "image-model", Name: "Image", Capabilities: map[string]bool{"chat_model": true, "image_route": true, "thinking_level": true}},
	}
}

// TestResolveModelAliasThinkingLevel 带等级后缀的名称解析到目录里的基础模型并带上等级；目录 ID 本身以等级结尾时按 ID 里的等级，
// 不再当成“不带后缀”强制最高思考；不认识的基础模型不解析
func TestResolveModelAliasThinkingLevel(t *testing.T) {
	models := aliasTestModels()
	for _, test := range []struct {
		name  string
		id    string
		level string
		ok    bool
	}{
		{name: "gemini-4-argon", id: "gemini-4-argon", ok: true},
		{name: "gemini-4-argon-low", id: "gemini-4-argon", level: "low", ok: true},
		{name: "gemini-4-argon-medium-nothinking", id: "gemini-4-argon", level: "medium", ok: true},
		{name: "vendor-model-low", id: "vendor-model-low", level: "low", ok: true},
		{name: "vendor-model-low-high", id: "vendor-model-low", level: "high", ok: true},
		{name: "unknown-model-low", ok: false},
		{name: " gemini-4-argon-low ", id: "gemini-4-argon", level: "low", ok: true},
		{name: "gemini-4-argon-medium-nothinking\n", id: "gemini-4-argon", level: "medium", ok: true},
		{name: "gemini-4-argon ", id: "gemini-4-argon", ok: true},
	} {
		model, alias, ok := resolveModelAlias(models, test.name)
		if ok != test.ok || ok && (model.ID != test.id || alias.level != test.level) {
			t.Fatalf("%s 解析为 model=%s level=%q ok=%v，期望 model=%s level=%q ok=%v", test.name, model.ID, alias.level, ok, test.id, test.level, test.ok)
		}
	}
}

// TestApplyThinkingAliasLevels 名称指定的等级优先于客户端参数；没有指定时设为最高、客户端显式参数优先；
// 只支持预算的模型按等级换算预算；不支持调节思考的模型不改
func TestApplyThinkingAliasLevels(t *testing.T) {
	models := aliasTestModels()
	level, budget, plain := models[0], models[1], models[3]
	ptr := func(value int64) *int64 { return &value }

	config := aistudio.GenerationConfig{}
	applyThinkingAlias(&config, level, "")
	if config.ReasoningEffort != "high" || config.ThinkingBudget != nil {
		t.Fatalf("不带等级：%+v，期望最高", config)
	}
	config = aistudio.GenerationConfig{ReasoningEffort: "low"}
	applyThinkingAlias(&config, level, "")
	if config.ReasoningEffort != "low" {
		t.Fatalf("不带等级时客户端显式参数优先：%+v", config)
	}
	for _, requested := range []string{"minimal", "low", "medium", "high"} {
		config = aistudio.GenerationConfig{ReasoningEffort: "high", ThinkingBudget: ptr(32768)}
		applyThinkingAlias(&config, level, requested)
		if config.ReasoningEffort != requested || config.ThinkingBudget != nil {
			t.Fatalf("名称指定 %s：%+v，期望覆盖客户端参数", requested, config)
		}
	}
	for requested, want := range map[string]int64{"minimal": 128, "low": 1024, "medium": 8192, "high": 32768, "": 32768} {
		config = aistudio.GenerationConfig{}
		applyThinkingAlias(&config, budget, requested)
		if config.ReasoningEffort != "" || config.ThinkingBudget == nil || *config.ThinkingBudget != want {
			t.Fatalf("只支持预算的模型 %q：%+v，期望预算 %d", requested, config, want)
		}
	}
	config = aistudio.GenerationConfig{}
	applyThinkingAlias(&config, plain, "low")
	if config.ReasoningEffort != "" || config.ThinkingBudget != nil {
		t.Fatalf("不支持调节思考的模型不应改动：%+v", config)
	}
}

// TestExpandModelAliasesThinkingLevels 模型列表为可调节思考的文本模型列出 -low、-medium；目录 ID 本身以等级结尾的模型不再追加等级后缀；
// 非文本模型不列后缀；单模型查询能找到组合后缀
func TestExpandModelAliasesThinkingLevels(t *testing.T) {
	ids := make([]string, 0)
	for _, model := range expandModelAliases(aliasTestModels()) {
		ids = append(ids, model.ID)
	}
	for _, want := range []string{"gemini-4-argon-low", "gemini-4-argon-medium", "gemini-4-argon-128", "gemini-2.5-pro-low", "gemini-2.5-pro-medium", "vendor-model-low-nothinking"} {
		if !slices.Contains(ids, want) {
			t.Fatalf("模型列表缺少 %s：%v", want, ids)
		}
	}
	for _, unwanted := range []string{"vendor-model-low-low", "vendor-model-low-medium", "vendor-model-low-128", "plain-chat-low", "image-model-low"} {
		if slices.Contains(ids, unwanted) {
			t.Fatalf("模型列表不应有 %s：%v", unwanted, ids)
		}
	}
	if model, ok := lookupPublicModel(aliasTestModels(), "gemini-4-argon-medium-nothinking-online"); !ok || model.ID != "gemini-4-argon-medium-nothinking-online" {
		t.Fatalf("单模型查询组合后缀：%+v ok=%v", model, ok)
	}
}

// TestBuildOnlyThinkingModels 只在 Build 通道可用的 Gemini 3 及以后的思考模型（Build 目录只有“会思考”字段）按思考等级处理：
// 不带后缀设为最高，-low、-medium 生效，-128/-minimal 按 LOW 发送；同样只在 Build 可用的 2.5 等旧模型与两个通道都有的模型不按此处理
func TestBuildOnlyThinkingModels(t *testing.T) {
	build := []string{"build"}
	argon := aistudio.Model{ID: "gemini-4-argon", Channels: build, Capabilities: map[string]bool{"chat_model": true, "thinking": true}}
	legacy := aistudio.Model{ID: "gemini-2.5-flash", Channels: build, Capabilities: map[string]bool{"chat_model": true, "thinking": true}}
	both := aistudio.Model{ID: "gemini-3.1-pro-preview", Channels: []string{"playground", "build"}, Capabilities: map[string]bool{"chat_model": true, "thinking": true}}
	gemma := aistudio.Model{ID: "gemma-4-27b-it", Channels: build, Capabilities: map[string]bool{"chat_model": true, "thinking": true}}
	image := aistudio.Model{ID: "gemini-3-pro-image-preview", Channels: build, Capabilities: map[string]bool{"chat_model": true, "thinking": true}}
	if thinkingLevelControllable(image) {
		t.Fatal("Build 独有的图片模型不应按思考等级处理")
	}
	if !thinkingLevelControllable(argon) || thinkingLevelControllable(legacy) || thinkingLevelControllable(both) || thinkingLevelControllable(gemma) {
		t.Fatalf("Build 独有判断不对：argon=%v legacy=%v both=%v gemma=%v",
			thinkingLevelControllable(argon), thinkingLevelControllable(legacy), thinkingLevelControllable(both), thinkingLevelControllable(gemma))
	}
	for requested, want := range map[string]string{"": "high", "low": "low", "medium": "medium", "high": "high", "minimal": "low"} {
		config := aistudio.GenerationConfig{}
		applyThinkingAlias(&config, argon, requested)
		if config.ReasoningEffort != want || config.ThinkingBudget != nil {
			t.Fatalf("Build 独有的 gemini-4-argon 指定 %q：%+v，期望 %s", requested, config, want)
		}
	}
	for _, model := range []aistudio.Model{legacy, gemma} {
		config := aistudio.GenerationConfig{}
		applyThinkingAlias(&config, model, "low")
		if config.ReasoningEffort != "" || config.ThinkingBudget != nil {
			t.Fatalf("%s 不应按思考等级处理：%+v", model.ID, config)
		}
	}
	ids := make([]string, 0)
	for _, model := range expandModelAliases([]aistudio.Model{argon, legacy}) {
		ids = append(ids, model.ID)
	}
	if !slices.Contains(ids, "gemini-4-argon-low") || !slices.Contains(ids, "gemini-4-argon-medium") || slices.Contains(ids, "gemini-2.5-flash-low") {
		t.Fatalf("模型列表：%v", ids)
	}
	for id, want := range map[string]int{"gemini-4-argon": 4, "gemini-3.1-pro-preview": 3, "models/gemini-2.5-flash": 2, "gemini-pro-latest": 0, "gemma-4": 0, "gemini-": 0} {
		if got := geminiMajorVersion(id); got != want {
			t.Fatalf("geminiMajorVersion(%s) = %d，期望 %d", id, got, want)
		}
	}
}
