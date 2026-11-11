package aistudio

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// omni12CatalogRow 构造 gemini-omni-1.2-flash 的目录行：视频能力只有 16:9/9:16、360p、4–15 秒、24fps，能力码只有对话（没有思考等级 52
// 与思考摘要 59），没有 field 72 思考设置（官网页面没有 Thinking level）
func omni12CatalogRow() []any {
	row := omniCatalogRow([]any{nil, nil, nil, nil, []int{3, 2}, nil, nil, []int{3}, nil, []int{5}, []int{4, 15, 1, 10}, []int{1}}, []int{1, 2})
	row[0] = "models/gemini-omni-1.2-flash"
	row[3] = "Gemini Omni 1.2 Flash"
	row[64] = []int{1}
	row[71] = nil
	return row
}

// interactionWireConfig 返回 CreateInteractionStream 请求体里的 GenerationConfig（interaction field 18 的第 2 项）
func interactionWireConfig(t *testing.T, body []byte) []any {
	t.Helper()
	var root []any
	if err := json.Unmarshal(body, &root); err != nil {
		t.Fatal(err)
	}
	return root[3].([]any)[17].([]any)[1].([]any)
}

// TestInteractionThinkingLevelWithoutCapability 模型目录没有思考等级（能力码 52）的 Interaction 模型（gemini-omni-1.2-flash）与官网一样
// 不发送 GenerationConfig field 6：不带思考参数、reasoning_effort 为 low/high、带思考预算时都一样，请求结构摘要写“思考等级=不发送”；
// 其余字段与带 52 的模型相同。Omni 1.1（带 52，目录默认等级 3）照旧发送（官网抓包见 TestInteractionMatchesOfficialCapture）
func TestInteractionThinkingLevelWithoutCapability(t *testing.T) {
	raw, _ := json.Marshal([]any{[]any{omni12CatalogRow(), omniCatalogRow([]any{nil, nil, nil, nil, []int{3, 2}}, nil)}})
	catalog, err := parseModelCatalog(strings.NewReader(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	omni12, omni11 := catalog.entries["gemini-omni-1.2-flash"], catalog.entries["gemini-omni-1.1-flash"]
	if omni12.defaults.ThinkingLevel || !omni12.defaults.InteractionStream || !OmniVideoModel(omni12.model) || !omni11.defaults.ThinkingLevel {
		t.Fatalf("1.2 defaults=%+v，1.1 defaults=%+v", omni12.defaults, omni11.defaults)
	}
	budget := int64(128)
	for _, config := range []GenerationConfig{{}, {ReasoningEffort: "low"}, {ReasoningEffort: "high"}, {ThinkingBudget: &budget}} {
		request := GenerateRequest{Model: "gemini-omni-1.2-flash", Contents: []Content{{Role: RoleUser, Parts: []Part{{Text: "做个视频"}}}}, Config: config}
		encoded, err := encodeCreateInteractionStream(request, omni12.defaults, true)
		if err != nil {
			t.Fatal(err)
		}
		wire := interactionWireConfig(t, encoded.body)
		if wire[5] != nil || wire[6] != float64(1) || wire[7] != float64(65536) || len(wire) != 25 || encoded.level != 0 {
			t.Fatalf("config=%+v：GenerationConfig=%v level=%d，期望不带 field 6、其余照旧", config, wire, encoded.level)
		}
		if summary := encoded.summary(request); !strings.Contains(summary, " | 思考等级=不发送 | ") {
			t.Fatalf("请求结构摘要应写“思考等级=不发送”：%s", summary)
		}
		// 同一请求按带 52 的模型编码时照旧发送 field 6：两者只差这一项
		withLevel, err := encodeCreateInteractionStream(request, omni11.defaults, true)
		if err != nil {
			t.Fatal(err)
		}
		levelled := interactionWireConfig(t, withLevel.body)
		if levelled[5] == nil || withLevel.level == 0 || !strings.Contains(withLevel.summary(request), " | 思考等级=") ||
			strings.Contains(withLevel.summary(request), "思考等级=不发送") {
			t.Fatalf("config=%+v：带 52 的模型应发送 field 6：%v", config, levelled)
		}
		levelled[5] = nil
		if got, want := mustMarshal(levelled), mustMarshal(wire); string(got) != string(want) {
			t.Fatalf("两种模型的 GenerationConfig 除 field 6 外应相同：%s 与 %s", got, want)
		}
	}
}

// TestGenerateInteractionThinkingIgnoredNote 经 Client.Generate 发往目录没有思考等级的 Omni：客户端的 reasoning_effort 与思考预算不起作用，
// 请求体不带 field 6，请求日志写一行说明（协议层按名称后缀记过的同一条不重复）；不带思考参数时不写
func TestGenerateInteractionThinkingIgnoredNote(t *testing.T) {
	raw, _ := json.Marshal([]any{[]any{omni12CatalogRow()}})
	catalog, err := parseModelCatalog(strings.NewReader(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	entry := catalog.entries["gemini-omni-1.2-flash"]
	budget := int64(2048)
	for _, test := range []struct {
		name   string
		config GenerationConfig
		note   string
	}{
		{name: "reasoning_effort", config: GenerationConfig{ReasoningEffort: "high"}, note: "客户端的思考参数（reasoning_effort=high）对 gemini-omni-1.2-flash 不生效"},
		{name: "思考预算", config: GenerationConfig{ThinkingBudget: &budget}, note: "客户端的思考参数（思考预算=2048）对 gemini-omni-1.2-flash 不生效"},
		{name: "没有思考参数"},
	} {
		t.Run(test.name, func(t *testing.T) {
			upstream := &regenUpstream{responses: []func() (*RPCResponse, error){
				streamResponse(string(interactionTestStream(
					interactionDeltaEvent([]any{[]any{"ok"}}),
					interactionFinal(3, []any{1, nil, nil, nil, 2, nil, nil, nil, 0, 3}),
				))),
			}}
			client, err := NewClient(ClientOptions{Transport: ceilingTransport{}, Protected: upstream})
			if err != nil {
				t.Fatal(err)
			}
			client.catalogs["alice@example.com"] = modelCatalog{models: []Model{entry.model}, entries: map[string]modelEntry{entry.model.ID: entry}}
			request := regenRequest(entry.model.ID)
			request.Config = test.config
			notes := &noteRecorder{}
			events, err := client.Generate(notes.context(context.Background()), request)
			if err != nil {
				t.Fatal(err)
			}
			collectEvents(t, events)
			if len(upstream.bodies) != 1 {
				t.Fatalf("应发送 1 次，实际 %d", len(upstream.bodies))
			}
			if wire := interactionWireConfig(t, []byte(upstream.bodies[0])); wire[5] != nil {
				t.Fatalf("请求体不应带 field 6：%v", wire)
			}
			if test.note == "" {
				if notes.count("不生效：模型目录没有思考等级") != 0 {
					t.Fatalf("没有思考参数时不应写说明：%v", notes.notes)
				}
				return
			}
			if notes.count(test.note+"：模型目录没有思考等级（能力码 52），与官网一样不发送思考等级") != 1 {
				t.Fatalf("请求说明 %v，期望一行 %q", notes.notes, test.note)
			}
			// 协议层已记过同一条说明（request.Notes）时不重复
			again := &regenUpstream{responses: upstream.responses}
			client.protected = again
			request.Notes = []string{InteractionThinkingIgnoredNote(entry.model.ID, entry.defaults, request.Config)}
			repeated := &noteRecorder{}
			events, err = client.Generate(repeated.context(context.Background()), request)
			if err != nil {
				t.Fatal(err)
			}
			collectEvents(t, events)
			if repeated.count("不生效：模型目录没有思考等级") != 0 {
				t.Fatalf("协议层记过的说明不应重复：%v", repeated.notes)
			}
		})
	}
}

// TestParseModelsCapability59 能力码 59 解码为 thinking_summaries（不再是 interaction_route）：带 59、没有 field 79 的模型是文本对话模型，
// 额度用完改用其他模型时能作为目标、Build 通道索引不排除它；带 field 79 的 Omni 照旧按 Interaction 模型排除
func TestParseModelsCapability59(t *testing.T) {
	text := catalogTestRow("gemini-4-carbon", 0)
	text[64] = []int{1, 10, 25, 52, 59}
	raw, _ := json.Marshal([]any{[]any{text, omniCatalogRow([]any{nil, nil, nil, nil, []int{3, 2}}, nil)}})
	catalog, err := parseModelCatalog(strings.NewReader(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	carbon := catalog.entries["gemini-4-carbon"].model
	if !carbon.Capabilities["thinking_summaries"] || !carbon.Capabilities["capability_code_59"] || carbon.Capabilities["interaction_route"] {
		t.Fatalf("能力 %v：59 应解码为 thinking_summaries", carbon.Capabilities)
	}
	if !TextChatModel(carbon) || buildExcludedPlaygroundModel(carbon) || catalog.entries["gemini-4-carbon"].defaults.InteractionStream {
		t.Fatal("带 59 的文本模型应是文本对话模型、不被 Build 通道索引排除")
	}
	request := GenerateRequest{Model: "gemini-4-argon", Tools: Tools{Functions: []FunctionDeclaration{{Name: "lookup"}}}}
	if why := FallbackTargetUnsupported(carbon, request); why != "" {
		t.Fatalf("带 59 的文本模型应能作为改用目标，实际跳过：%s", why)
	}
	omni := catalog.entries["gemini-omni-1.1-flash"].model
	if TextChatModel(omni) || !buildExcludedPlaygroundModel(omni) || FallbackTargetUnsupported(omni, request) != "是 Interaction（Omni）模型，请求方式不同" {
		t.Fatal("带 field 79 的 Omni 应照旧按 Interaction 模型排除")
	}
}
