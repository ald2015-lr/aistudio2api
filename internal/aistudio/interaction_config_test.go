package aistudio

import (
	"bytes"
	"encoding/json"
	"testing"
)

// interactionConfigRow 构造带 field 79（下标 78）Interaction 配置的模型目录行；config 为 nil 时没有这项配置
func interactionConfigRow(id string, config any) []any {
	row := make([]any, 85)
	row[0] = "models/" + id
	row[3] = id
	row[6] = 65536
	row[7] = []string{"countTokens", "generateContent"}
	// 1 对话模型、25 会思考、52 思考等级、91 官网自动续写（与现场 gemini-4-argon、gemini-4-neon 的能力码一致的部分）
	row[64] = []int{1, 25, 52, 91}
	row[78] = config
	return row
}

// TestInteractionConfigClassification 模型目录 field 79 的分类：2026-11 起 Google 逐个账户给 gemini-4-argon、gemini-4-neon 等普通文本模型加上
// [null,null,1,2]（类型为模型、field 3 为 1），它们照常经 GenerateContent 生成；原先只要带 field 79 就标成 interactions_api，
// 不算文本对话模型，带后缀的名称（gemini-4-neon-medium）不再解析、原名转发后以模型不存在 404。Omni（field 3 为 false）与 agent（类型 1）照旧
func TestInteractionConfigClassification(t *testing.T) {
	cases := []struct {
		name            string
		config          any
		interactionsAPI bool
		stream          bool
	}{
		{name: "没有 field 79", config: nil},
		{name: "Gemini 4 普通模型 [null,null,1,2]", config: []any{nil, nil, 1, 2}},
		{name: "类型为模型、field 3 为 true", config: []any{nil, nil, true, 2}},
		{name: "Omni [null,null,false,2]", config: []any{nil, nil, false, 2}, interactionsAPI: true, stream: true},
		{name: "Omni field 3 为 0", config: []any{nil, nil, 0, 2}, interactionsAPI: true, stream: true},
		{name: "类型为模型、没有 field 3", config: []any{nil, nil, nil, 2}, interactionsAPI: true, stream: true},
		{name: "agent 后台任务", config: []any{nil, nil, true, 1}, interactionsAPI: true},
		{name: "agent", config: []any{nil, nil, nil, 1}, interactionsAPI: true},
		{name: "空配置", config: []any{}, interactionsAPI: true},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			raw, err := json.Marshal([]any{[]any{interactionConfigRow("gemini-test", test.config)}})
			if err != nil {
				t.Fatal(err)
			}
			catalog, err := parseModelCatalog(bytes.NewReader(raw))
			if err != nil {
				t.Fatal(err)
			}
			entry, ok := catalog.entries["gemini-test"]
			if !ok {
				t.Fatal("目录里没有模型")
			}
			if got := entry.model.Capabilities["interactions_api"]; got != test.interactionsAPI {
				t.Fatalf("interactions_api = %t，期望 %t", got, test.interactionsAPI)
			}
			if entry.defaults.InteractionStream != test.stream {
				t.Fatalf("InteractionStream = %t，期望 %t", entry.defaults.InteractionStream, test.stream)
			}
			if got := TextChatModel(entry.model); got != !test.interactionsAPI {
				t.Fatalf("TextChatModel = %t，期望 %t", got, !test.interactionsAPI)
			}
		})
	}
}

// TestInteractionConfigMalformed field 79 写法不对时与原先一样整份目录解析失败（不静默当作没有配置）
func TestInteractionConfigMalformed(t *testing.T) {
	for _, config := range []any{"oops", []any{nil, nil, "yes", 2}, []any{nil, nil, false, "two"}} {
		raw, err := json.Marshal([]any{[]any{interactionConfigRow("gemini-test", config)}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := parseModelCatalog(bytes.NewReader(raw)); err == nil {
			t.Fatalf("field 79 = %v 时应解析失败", config)
		}
	}
}
