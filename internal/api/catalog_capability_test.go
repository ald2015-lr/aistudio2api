package api

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// 按官网 ListModels 的位置解码出的目录（aistudio.ParseModels）上的协议层判定：能力码 59（thinking_summaries）不再让文本模型被当成
// Interaction（Omni）模型；没有思考等级（能力码 52）的 Omni（gemini-omni-1.2-flash）忽略思考等级后缀

// wireCatalogRow 按官网 ListModels 的位置构造一行模型：codes 为 field 65（下标 64）能力码；omni 为真时带 field 79（下标 78）
// Interaction 配置与 field 71（下标 70）视频能力（宽高比 16:9/9:16、分辨率 360p、时长 4–15 秒、帧率 24fps）
func wireCatalogRow(id string, codes []int, omni bool) []any {
	row := make([]any, 85)
	row[0] = "models/" + id
	row[3] = strings.ToUpper(id)
	row[5] = 1048576
	row[6] = 65536
	row[7] = []string{"countTokens", "generateContent"}
	row[64] = codes
	if omni {
		row[70] = []any{nil, nil, nil, nil, []int{3, 2}, nil, nil, []int{3}, nil, []int{5}, []int{4, 15, 1, 10}, []int{1}}
		row[78] = []any{nil, nil, false, 2}
	}
	return row
}

// parseWireCatalog 用 aistudio.ParseModels 解码 rows
func parseWireCatalog(t *testing.T, rows ...[]any) []aistudio.Model {
	t.Helper()
	raw, err := json.Marshal([]any{rows})
	if err != nil {
		t.Fatal(err)
	}
	models, err := aistudio.ParseModels(strings.NewReader(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	return models
}

// capabilityTestCatalog 为 argon（对话、函数、搜索、思考、思考等级、思考摘要 59）、Omni 1.1（{1,25,52,59}，线上形状）与
// Omni 1.2（只有对话能力码，没有 52 与 59）
func capabilityTestCatalog(t *testing.T) []aistudio.Model {
	t.Helper()
	return parseWireCatalog(t,
		wireCatalogRow("gemini-4-argon", []int{1, 10, 12, 25, 52, 59}, false),
		wireCatalogRow("gemini-omni-1.1-flash", []int{1, 25, 52, 59}, true),
		wireCatalogRow("gemini-omni-1.2-flash", []int{1}, true),
	)
}

// TestCapability59TextModelAliases 能力码 59 解码为 thinking_summaries：带它的文本模型照常是文本对话模型（后缀别名、long- 与
// 名称后缀生效）；Omni 1.1 与 1.2 只因 field 79 不是文本对话模型，公开列表不为它们列 long- 名称（原先 1.2 没有 59，多列出
// long-gemini-omni-1.2-flash；argon 若带 59 则后缀与 long- 都失效）
func TestCapability59TextModelAliases(t *testing.T) {
	models := capabilityTestCatalog(t)
	byID := map[string]aistudio.Model{}
	for _, model := range models {
		byID[model.ID] = model
	}
	argon := byID["gemini-4-argon"]
	if !argon.Capabilities["thinking_summaries"] || argon.Capabilities["interaction_route"] || argon.Capabilities["interactions_api"] {
		t.Fatalf("argon 的能力 %v：59 应解码为 thinking_summaries", argon.Capabilities)
	}
	if !isTextChatModel(argon) || !longOutputModel(argon) || !aistudio.TextChatModel(argon) {
		t.Fatal("带能力码 59 的 argon 应是文本对话模型")
	}
	for _, id := range []string{"gemini-omni-1.1-flash", "gemini-omni-1.2-flash"} {
		if isTextChatModel(byID[id]) || longOutputModel(byID[id]) || !aistudio.OmniVideoModel(byID[id]) {
			t.Fatalf("%s 应是 Omni、不是文本对话模型", id)
		}
	}
	var listed []string
	for _, model := range expandModelAliases(models) {
		listed = append(listed, model.ID)
	}
	for _, id := range []string{"gemini-4-argon-low", "gemini-4-argon-nothinking", "gemini-4-argon-online", "long-gemini-4-argon"} {
		if !slices.Contains(listed, id) {
			t.Fatalf("公开列表 %v 应有 %s", listed, id)
		}
	}
	for _, id := range listed {
		if strings.HasPrefix(id, "long-gemini-omni") || strings.HasPrefix(id, "gemini-omni-1.2-flash-") || strings.HasPrefix(id, "gemini-omni-1.1-flash-") {
			t.Fatalf("公开列表不应为 Omni 列 %s", id)
		}
	}
	s := &server{service: modelListService{models: models}}
	request := aistudio.GenerateRequest{Model: "long-gemini-4-argon-low", Contents: []aistudio.Content{{Role: aistudio.RoleUser, Parts: []aistudio.Part{{Text: "hi"}}}}}
	if _, err := s.applyModelAlias(context.Background(), &request); err != nil {
		t.Fatal(err)
	}
	if request.Model != "gemini-4-argon" || request.Config.ReasoningEffort != "low" || !request.LongOutput || request.Alias == nil {
		t.Fatalf("long-gemini-4-argon-low 整理为 model=%s effort=%q long=%v alias=%+v", request.Model, request.Config.ReasoningEffort, request.LongOutput, request.Alias)
	}
}

// TestOmniThinkingSuffixWithoutLevel 模型目录没有思考等级（能力码 52）的 Omni（gemini-omni-1.2-flash）：名称里的思考等级后缀不改
// 请求的思考参数，留一行请求日志说明；视频后缀照常生效。Omni 1.1 带 52，后缀照旧写进 reasoning_effort
func TestOmniThinkingSuffixWithoutLevel(t *testing.T) {
	s := &server{service: modelListService{models: capabilityTestCatalog(t)}}
	for _, test := range []struct {
		name   string
		model  string
		effort string
		note   bool
	}{
		{name: "gemini-omni-1.2-flash-9x16-5s-low", model: "gemini-omni-1.2-flash", note: true},
		{name: "gemini-omni-1.2-flash-128", model: "gemini-omni-1.2-flash", note: true},
		{name: "gemini-omni-1.2-flash-9x16", model: "gemini-omni-1.2-flash"},
		{name: "gemini-omni-1.1-flash-9x16-5s-low", model: "gemini-omni-1.1-flash", effort: "low"},
	} {
		request := aistudio.GenerateRequest{Model: test.name, Contents: []aistudio.Content{{Role: aistudio.RoleUser, Parts: []aistudio.Part{{Text: "做个视频"}}}}}
		if _, err := s.applyModelAlias(context.Background(), &request); err != nil {
			t.Fatalf("%s: %v", test.name, err)
		}
		suffixVideo := strings.Contains(test.name, "-9x16")
		if request.Model != test.model || request.Config.ReasoningEffort != test.effort ||
			suffixVideo && (request.Config.VideoConfig == nil || request.Config.VideoConfig.AspectRatio != "9:16") {
			t.Fatalf("%s 整理为 model=%s effort=%q video=%+v", test.name, request.Model, request.Config.ReasoningEffort, request.Config.VideoConfig)
		}
		noted := false
		for _, note := range request.Notes {
			noted = noted || strings.Contains(note, "对 "+test.model+" 不生效：模型目录没有思考等级（能力码 52）")
		}
		if noted != test.note {
			t.Fatalf("%s 的请求说明 %v，期望写明后缀不生效=%v", test.name, request.Notes, test.note)
		}
	}
}
