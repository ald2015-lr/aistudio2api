package api

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// TestTrimOneMillionContext 模型名末尾的 [1m]（Claude Code 1M 上下文模式）不分大小写去掉，其他位置或只有 [1m] 时原样
func TestTrimOneMillionContext(t *testing.T) {
	for name, want := range map[string]string{
		"gemini-4-argon[1m]":          "gemini-4-argon",
		"gemini-4-argon[1M]":          "gemini-4-argon",
		" gemini-4-argon-xhigh[1m] ":  "gemini-4-argon-xhigh",
		"gemini-4-argon [1m]":         "gemini-4-argon",
		"long-gemini-4-argon-low[1m]": "long-gemini-4-argon-low",
		"gemini-4-argon":              "gemini-4-argon",
		"gemini-4-argon[1m]-low":      "gemini-4-argon[1m]-low",
		"[1m]":                        "[1m]",
	} {
		if got := trimOneMillionContext(name); got != want {
			t.Fatalf("trimOneMillionContext(%q) = %q，期望 %q", name, got, want)
		}
	}
}

// TestOneMillionContextModelResolution 带 [1m] 的名称与不带时解析结果相同：思考等级后缀、long- 前缀、models/ 前缀照常组合，
// 换号池时按其他号池找模型用的候选名同样去掉
func TestOneMillionContextModelResolution(t *testing.T) {
	models := aliasTestModels()
	for name, want := range map[string]struct {
		level string
		long  bool
	}{
		"gemini-4-argon[1m]":                   {},
		"models/gemini-4-argon[1m]":            {},
		"gemini-4-argon-xhigh[1m]":             {level: "xhigh"},
		"gemini-4-argon-low-nothinking[1M]":    {level: "low"},
		"long-gemini-4-argon-medium[1m]":       {level: "medium", long: true},
		"LONG-gemini-4-argon-xhigh-online[1m]": {level: "xhigh", long: true},
	} {
		model, alias, _, long, ok := resolveLongRequestModel(models, name)
		if !ok || model.ID != "gemini-4-argon" || alias.level != want.level || long != want.long {
			t.Fatalf("%s 解析为 model=%s level=%q long=%v ok=%v，期望 %+v", name, model.ID, alias.level, long, ok, want)
		}
	}
	if bases := RequestModelBases("long-gemini-4-argon-low[1m]"); !slices.Contains(bases, "gemini-4-argon") || slices.ContainsFunc(bases, func(base string) bool {
		return strings.Contains(base, "[1m]")
	}) {
		t.Fatalf("RequestModelBases = %v，期望含 gemini-4-argon、不含 [1m]", bases)
	}
	if model, ok := lookupLongPublicModel(models, "gemini-4-argon-low[1m]"); !ok || model.ID != "gemini-4-argon-low[1m]" {
		t.Fatalf("单模型查询 = %+v %v，期望按请求的名称查到", model, ok)
	}
}

// TestOneMillionContextReachesServiceCanonical Anthropic 请求的模型名带 [1m] 时，交给生成服务、写进请求日志（InfoNotes 记一行说明）
// 与响应里的都是去掉后的目录模型名（之后的改用其他模型、权益规则与选号按它进行）；目录取不到或不是文本对话模型时同样去掉。
// 计数接口按同一个模型
func TestOneMillionContextReachesServiceCanonical(t *testing.T) {
	header := map[string]string{"Anthropic-Version": "2023-06-01"}
	catalog := []aistudio.Model{{ID: "gemini-4-argon", Methods: []string{"generateContent"}, Capabilities: map[string]bool{"chat_model": true, "thinking_level": true}}}
	for _, test := range []struct {
		name, model, want, effort string
		models                    []aistudio.Model
	}{
		{name: "目录模型", model: "gemini-4-argon[1m]", want: "gemini-4-argon", effort: "high", models: catalog},
		{name: "等级后缀", model: "gemini-4-argon-low[1m]", want: "gemini-4-argon", effort: "low", models: catalog},
		{name: "目录为空", model: "gemini-4-argon[1m]", want: "gemini-4-argon"},
	} {
		t.Run(test.name, func(t *testing.T) {
			service := &billingCountService{recordingService: recordingService{modelListService: modelListService{models: test.models}}}
			handler := NewHandler(service, Config{APIKey: "sk-test"})
			recorder := postJSON(t, handler, "/v1/messages", `{"model":"`+test.model+`","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`, header)
			var response struct {
				Model string `json:"model"`
			}
			if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil || recorder.Code != 200 {
				t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
			}
			last := service.last
			if last == nil || last.Model != test.want || response.Model != test.want {
				t.Fatalf("生成服务收到 %+v，响应 model=%q，期望都为 %s", last, response.Model, test.want)
			}
			if test.models != nil && last.Config.ReasoningEffort != test.effort {
				t.Fatalf("思考强度 = %q，期望 %s", last.Config.ReasoningEffort, test.effort)
			}
			if !slices.ContainsFunc(last.InfoNotes, func(note string) bool { return strings.Contains(note, "[1m]") }) {
				t.Fatalf("InfoNotes = %v，期望记一行去掉 [1m] 的说明", last.InfoNotes)
			}
			recorder = postJSON(t, handler, "/v1/messages/count_tokens", `{"model":"`+test.model+`","messages":[{"role":"user","content":"hi"}]}`, header)
			if recorder.Code != 200 || service.counted == nil || service.counted.Model != test.want {
				t.Fatalf("计数：status=%d 请求=%+v，期望模型 %s", recorder.Code, service.counted, test.want)
			}
		})
	}
}
