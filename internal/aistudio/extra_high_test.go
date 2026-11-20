package aistudio

import (
	"encoding/json"
	"strings"
	"testing"
)

// extraHighDefaults 为目录列出 Extra High 的思考等级模型的默认值（等级列表 [1,2,3,4,5]、默认等级 default）
func extraHighDefaults(defaultLevel int64, levels ...int64) GenerationDefaults {
	return GenerationDefaults{
		MaxOutputTokens: 65536, Thinking: true, ThinkingLevel: true, DefaultThinkingLevel: defaultLevel, ThinkingLevels: levels,
	}
}

// generationThinkingWire 返回 GenerationConfig field 17（下标 16）的思考等级（第 4 项）；没有时为 -1
func generationThinkingWire(t *testing.T, config GenerationConfig, defaults GenerationDefaults) int64 {
	t.Helper()
	wire, err := encodeGenerationConfig(config, defaults)
	if err != nil {
		t.Fatal(err)
	}
	thinking, _ := wire[16].([]any)
	if len(thinking) < 4 || thinking[3] == nil {
		return -1
	}
	return thinking[3].(int64)
}

// TestNormalizeReasoningEffortExtraHigh xhigh、extra_high（Gemini 枚举 EXTRA_HIGH）、extra-high、extrahigh、max（Anthropic 最高档）、maximum、
// highest 都归为 xhigh；原先都归为 high。其他写法不变
func TestNormalizeReasoningEffortExtraHigh(t *testing.T) {
	for _, value := range []string{"xhigh", "XHIGH", " extra_high ", "EXTRA_HIGH", "extra-high", "ExtraHigh", "max", "maximum", "Highest"} {
		if got := normalizeReasoningEffort(value); got != "xhigh" {
			t.Fatalf("%q 归为 %q，期望 xhigh", value, got)
		}
	}
	for value, want := range map[string]string{"high": "high", "HIGH": "high", "medium": "medium", "min": "minimal", "off": "none", "auto": "", "extra": ""} {
		if got := normalizeReasoningEffort(value); got != want {
			t.Fatalf("%q 归为 %q，期望 %q", value, got, want)
		}
	}
}

// TestExtraHighGenerateContentWire Extra High 在 ListModels 中为 5，GenerateContent 请求的 GenerationConfig field 17 第 4 项写 6（官网编号）：
// 请求 xhigh 等写法、目录默认等级为 5 且没有思考参数时都发 6；其他等级照旧
func TestExtraHighGenerateContentWire(t *testing.T) {
	defaults := extraHighDefaults(3, 4, 1, 2, 3, 5)
	for _, effort := range []string{"xhigh", "extra_high", "max", "highest"} {
		if got := generationThinkingWire(t, GenerationConfig{ReasoningEffort: effort}, defaults); got != 6 {
			t.Fatalf("reasoning_effort=%s 发出等级 %d，期望 6", effort, got)
		}
	}
	for effort, want := range map[string]int64{"high": 3, "medium": 2, "low": 1, "minimal": 4, "": 3} {
		if got := generationThinkingWire(t, GenerationConfig{ReasoningEffort: effort}, defaults); got != want {
			t.Fatalf("reasoning_effort=%q 发出等级 %d，期望 %d", effort, got, want)
		}
	}
	// 目录默认等级为 Extra High：不带思考参数时按默认等级发送，同样换成 6（原先原样发 5）
	if got := generationThinkingWire(t, GenerationConfig{}, extraHighDefaults(5, 1, 3, 5)); got != 6 {
		t.Fatalf("目录默认等级 5 发出 %d，期望 6", got)
	}
	// 完整请求体里同样是 6
	request := GenerateRequest{Model: "gemini-test", Contents: []Content{{Role: RoleUser, Parts: []Part{{Text: "hi"}}}}, Config: GenerationConfig{ReasoningEffort: "xhigh"}}
	body, err := EncodeGenerateContentRequest(request, defaults, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	var root []any
	if err := json.Unmarshal(body, &root); err != nil {
		t.Fatal(err)
	}
	if thinking := root[3].([]any)[16].([]any); thinking[3] != float64(6) {
		t.Fatalf("请求体的思考设置 = %v，期望第 4 项为 6", thinking)
	}
}

// TestExtraHighFallsBackToHigh 模型目录的等级列表没有列出 Extra High（包括没有等级列表）时 xhigh 按 HIGH（3）发送，不给不支持的模型发 6；
// 列表连 HIGH 也没有时取最接近 HIGH 的等级。只支持思考预算的模型按最大预算（与 high 相同），预算换算不会得到 Extra High
func TestExtraHighFallsBackToHigh(t *testing.T) {
	for _, test := range []struct {
		levels []int64
		want   int64
	}{
		{levels: nil, want: 3},
		{levels: []int64{1, 2, 3}, want: 3},
		{levels: []int64{4, 1, 2, 3}, want: 3},
		{levels: []int64{1, 2}, want: 2},
		{levels: []int64{1, 3, 5}, want: 6},
	} {
		got := generationThinkingWire(t, GenerationConfig{ReasoningEffort: "xhigh"}, extraHighDefaults(3, test.levels...))
		if got != test.want {
			t.Fatalf("等级列表 %v 请求 xhigh 发出 %d，期望 %d", test.levels, got, test.want)
		}
	}
	for _, test := range []struct {
		requested int64
		supported []int64
		want      int64
	}{
		{5, nil, 3}, {5, []int64{1, 3}, 3}, {5, []int64{1, 2}, 2}, {5, []int64{5}, 5}, {3, []int64{1, 2, 3, 5}, 3}, {3, nil, 3},
	} {
		if got := closestSupportedThinkingLevel(test.requested, test.supported); got != test.want {
			t.Fatalf("closestSupportedThinkingLevel(%d, %v) = %d，期望 %d", test.requested, test.supported, got, test.want)
		}
	}
	budgetOnly := GenerationDefaults{MaxOutputTokens: 65536, Thinking: true, ThinkingBudget: true}
	wire, err := encodeGenerationConfig(GenerationConfig{ReasoningEffort: "xhigh"}, budgetOnly)
	if err != nil {
		t.Fatal(err)
	}
	if thinking := wire[16].([]any); len(thinking) < 2 || thinking[1] != int64(24576) {
		t.Fatalf("只支持预算的模型请求 xhigh：%v，期望预算 24576", thinking)
	}
	for _, budget := range []int64{0, 1024, 8192, 24576, 1 << 20} {
		if level := thinkingLevelForBudget(budget); level == extraHighThinkingLevel {
			t.Fatalf("预算 %d 换算为 Extra High", budget)
		}
	}
}

// buildThinkingLevelName 返回 Build 请求体 generationConfig.thinkingConfig.thinkingLevel
func buildThinkingLevelName(t *testing.T, config GenerationConfig, defaults GenerationDefaults) string {
	t.Helper()
	request := GenerateRequest{Model: "gemini-test", Contents: []Content{{Role: RoleUser, Parts: []Part{{Text: "hi"}}}}, Config: config}
	_, body, err := EncodeBuildGenerateRequest(request, defaults, false, false)
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		GenerationConfig struct {
			ThinkingConfig struct {
				ThinkingLevel string `json:"thinkingLevel"`
			} `json:"thinkingConfig"`
		} `json:"generationConfig"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatal(err)
	}
	return decoded.GenerationConfig.ThinkingConfig.ThinkingLevel
}

// TestExtraHighBuild Build 通道按 Gemini API 名称发送：目录列出 Extra High 的模型请求 xhigh 与目录默认等级 5 都发 EXTRA_HIGH
// （Build 编码读的是已换算成 6 的 GenerationConfig，ListModels 编号 5 也认）；只在 Build 可用的模型（没有等级列表）按 HIGH
func TestExtraHighBuild(t *testing.T) {
	defaults := extraHighDefaults(3, 1, 2, 3, 5)
	if got := buildThinkingLevelName(t, GenerationConfig{ReasoningEffort: "xhigh"}, defaults); got != "EXTRA_HIGH" {
		t.Fatalf("Build xhigh 发出 %q，期望 EXTRA_HIGH", got)
	}
	if got := buildThinkingLevelName(t, GenerationConfig{}, extraHighDefaults(5, 3, 5)); got != "EXTRA_HIGH" {
		t.Fatalf("Build 目录默认等级 5 发出 %q，期望 EXTRA_HIGH", got)
	}
	if got := buildThinkingLevelName(t, GenerationConfig{ReasoningEffort: "high"}, defaults); got != "HIGH" {
		t.Fatalf("Build high 发出 %q", got)
	}
	buildOnly := buildModelDefaults(Model{ID: "gemini-4-argon", OutputTokenLimit: 65536, Capabilities: map[string]bool{"chat_model": true, "thinking": true}})
	if got := buildThinkingLevelName(t, GenerationConfig{ReasoningEffort: "xhigh"}, buildOnly); got != "HIGH" {
		t.Fatalf("Build 独有模型 xhigh 发出 %q，期望 HIGH", got)
	}
	for _, level := range []int64{extraHighThinkingLevel, extraHighThinkingWire} {
		if buildThinkingLevels[level] != "EXTRA_HIGH" {
			t.Fatalf("Build 等级换算表缺少 %d", level)
		}
	}
}

// TestExtraHighInteraction Interaction 的 thinking level 枚举没有 Extra High：xhigh 与目录默认等级 5 都按 HIGH（Interaction 编号 4）发送，
// 原先目录默认等级为 5 时以“未识别的 thinking level 5”失败
func TestExtraHighInteraction(t *testing.T) {
	request := func(effort string) GenerateRequest {
		return GenerateRequest{Model: "gemini-omni-test", Contents: []Content{{Role: RoleUser, Parts: []Part{{Text: "做个视频"}}}}, Config: GenerationConfig{ReasoningEffort: effort}}
	}
	for _, test := range []struct {
		effort   string
		defaults GenerationDefaults
	}{
		{"xhigh", extraHighDefaults(3, 1, 3, 5)},
		{"max", extraHighDefaults(3, 1, 3, 5)},
		{"", extraHighDefaults(5, 1, 3, 5)},
		{"", extraHighDefaults(5)},
		{"xhigh", extraHighDefaults(3, 5)},
	} {
		encoded, err := encodeCreateInteractionStream(request(test.effort), test.defaults, true)
		if err != nil {
			t.Fatalf("effort=%q defaults=%+v：%v", test.effort, test.defaults, err)
		}
		if wire := interactionWireConfig(t, encoded.body); wire[5] != float64(4) || encoded.level != 4 {
			t.Fatalf("effort=%q defaults=%+v：thinking level = %v，期望 HIGH（4）", test.effort, test.defaults, wire[5])
		}
	}
}

// TestExtraHighLiveDefault Live setup 的 GenerationConfig 与 GenerateContent 相同：目录默认等级为 Extra High（5）时按 6 发送，其他等级照旧
func TestExtraHighLiveDefault(t *testing.T) {
	for defaultLevel, want := range map[int64]float64{5: 6, 3: 3, 2: 2} {
		body, _, err := EncodeBidiSetupRequest(BidiRequest{
			Model: "models/gemini-live", Mode: BidiModeLive, generationDefaults: extraHighDefaults(defaultLevel, 1, 2, 3, 5),
		}, RequestContext{})
		if err != nil {
			t.Fatal(err)
		}
		var root []any
		if err := json.Unmarshal(body, &root); err != nil {
			t.Fatal(err)
		}
		thinking := root[6].([]any)[1].([]any)[16].([]any)
		if thinking[3] != want {
			t.Fatalf("默认等级 %d：Live setup 思考设置 = %v，期望第 4 项为 %v", defaultLevel, thinking, want)
		}
	}
}

// extraHighCatalogRow 构造文本模型的目录行：field 72（下标 71）子索引 5 为默认等级、6 为等级列表
func extraHighCatalogRow(id string, thinking []any) []any {
	row := make([]any, 80)
	row[0] = "models/" + id
	row[3] = strings.ToUpper(id)
	row[5] = 1048576
	row[6] = 65536
	row[7] = []string{"countTokens", "generateContent"}
	row[64] = []int{1, 25, 52}
	row[71] = thinking
	return row
}

// TestExtraHighCatalogFlag 目录的等级列表有 5（或默认等级为 5）时模型标出 ExtraHighThinking；这一项不写进 JSON，账户目录指纹不变
// （写进 Capabilities 会让升级后全部账户的目录指纹变化、按过期重新同步）；复制与多账户合并时保留（合并取或）
func TestExtraHighCatalogFlag(t *testing.T) {
	raw, _ := json.Marshal([]any{[]any{
		extraHighCatalogRow("gemini-with-xhigh", []any{nil, nil, nil, nil, nil, 3, []int{4, 1, 2, 3, 5}}),
		extraHighCatalogRow("gemini-default-xhigh", []any{nil, nil, nil, nil, nil, 5, []int{}}),
		extraHighCatalogRow("gemini-high-only", []any{nil, nil, nil, nil, nil, 3, []int{1, 2, 3}}),
		extraHighCatalogRow("gemini-no-levels", nil),
	}})
	catalog, err := parseModelCatalog(strings.NewReader(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]bool{"gemini-with-xhigh": true, "gemini-default-xhigh": true, "gemini-high-only": false, "gemini-no-levels": false} {
		if got := catalog.entries[id].model.ExtraHighThinking; got != want {
			t.Fatalf("%s ExtraHighThinking = %v，期望 %v", id, got, want)
		}
	}
	models := cloneModels(catalog.models)
	if !models[0].ExtraHighThinking {
		t.Fatal("cloneModels 丢了 ExtraHighThinking")
	}
	if encoded, _ := json.Marshal(models[0]); strings.Contains(strings.ToLower(string(encoded)), "extra") {
		t.Fatalf("ExtraHighThinking 不应写进 JSON：%s", encoded)
	}
	plain := cloneModels(models)
	for index := range plain {
		plain[index].ExtraHighThinking = false
	}
	withFlag, err := accountCatalogFingerprint(BenefitTierPro, models)
	if err != nil {
		t.Fatal(err)
	}
	withoutFlag, err := accountCatalogFingerprint(BenefitTierPro, plain)
	if err != nil {
		t.Fatal(err)
	}
	if withFlag != withoutFlag {
		t.Fatal("ExtraHighThinking 改变了账户目录指纹")
	}
	merged := mergeModelSets([][]Model{plain, models, plain})
	for _, model := range merged {
		if want := model.ID == "gemini-with-xhigh" || model.ID == "gemini-default-xhigh"; model.ExtraHighThinking != want {
			t.Fatalf("合并后 %s ExtraHighThinking = %v，期望 %v（任一账户列出即算）", model.ID, model.ExtraHighThinking, want)
		}
	}
	for _, model := range mergeModelSets([][]Model{plain, plain}) {
		if model.ExtraHighThinking {
			t.Fatalf("都没有列出时 %s 不应标出", model.ID)
		}
	}
}

// TestExtraHighListingMatchesWire 模型列表是否列出 -xhigh（ExtraHighThinking）与请求 xhigh 是否真按 Extra High（6）发送口径相同：
// 目录默认等级就是 5、等级列表为空或没有列出 5 的模型同样列出 -xhigh，xhigh 按 6 发送，不会比不带思考参数时（默认 5 发 6）更低
func TestExtraHighListingMatchesWire(t *testing.T) {
	raw, _ := json.Marshal([]any{[]any{
		extraHighCatalogRow("gemini-with-xhigh", []any{nil, nil, nil, nil, nil, 3, []int{4, 1, 2, 3, 5}}),
		extraHighCatalogRow("gemini-default-xhigh", []any{nil, nil, nil, nil, nil, 5, []int{}}),
		extraHighCatalogRow("gemini-default-xhigh-unlisted", []any{nil, nil, nil, nil, nil, 5, []int{1, 3}}),
		extraHighCatalogRow("gemini-high-only", []any{nil, nil, nil, nil, nil, 3, []int{1, 2, 3}}),
		extraHighCatalogRow("gemini-high-default", []any{nil, nil, nil, nil, nil, 3, []int{}}),
	}})
	catalog, err := parseModelCatalog(strings.NewReader(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	for id, entry := range catalog.entries {
		xhigh := generationThinkingWire(t, GenerationConfig{ReasoningEffort: "xhigh"}, entry.defaults)
		if listed := entry.model.ExtraHighThinking; listed != (xhigh == extraHighThinkingWire) {
			t.Fatalf("%s：列出 -xhigh = %v，请求 xhigh 却发出 %d", id, listed, xhigh)
		}
		if fallback := generationThinkingWire(t, GenerationConfig{}, entry.defaults); xhigh < fallback {
			t.Fatalf("%s：请求 xhigh 发出 %d，比不带思考参数时的 %d 还低", id, xhigh, fallback)
		}
	}
	for id, want := range map[string]int64{"gemini-default-xhigh": 6, "gemini-default-xhigh-unlisted": 6, "gemini-high-only": 3, "gemini-high-default": 3} {
		if got := generationThinkingWire(t, GenerationConfig{ReasoningEffort: "max"}, catalog.entries[id].defaults); got != want {
			t.Fatalf("%s 请求 max 发出 %d，期望 %d", id, got, want)
		}
	}
	// 其他等级的取法不变：默认等级 5、列表 [1, 3] 的模型请求 medium 仍取最接近的 1（距离相同取较低的）
	if got := generationThinkingWire(t, GenerationConfig{ReasoningEffort: "medium"}, catalog.entries["gemini-default-xhigh-unlisted"].defaults); got != 1 {
		t.Fatalf("默认 5、列表 [1, 3] 请求 medium 发出 %d，期望 1", got)
	}
}
