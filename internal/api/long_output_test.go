package api

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// long- 前缀（自动续写）的协议层测试：与各后缀组合、不分大小写、目录 ID 冲突、未知模型、不带前缀时逐字节不变、列表与单模型查询、
// 计数解析与结束原因映射

func longTestModels() []aistudio.Model {
	return append(aliasTestModels(),
		aistudio.Model{ID: "gemini-3.8-flash", Name: "Gemini 3.8 Flash", Channels: []string{"playground", "build"},
			Capabilities: map[string]bool{"chat_model": true, "thinking": true, "thinking_level": true, "google_search": true}},
		aistudio.Model{ID: "long-context-model", Name: "Long Context", Capabilities: map[string]bool{"chat_model": true, "thinking": true, "thinking_level": true}},
		aistudio.Model{ID: "build-only-chat", Name: "Build Only", Channels: []string{"build"}, Capabilities: map[string]bool{"chat_model": true}},
		aistudio.Model{ID: "gemini-omni-1.1-flash", Name: "Omni", Capabilities: map[string]bool{"chat_model": true, "interaction_route": true}},
	)
}

// TestResolveLongRequestModel long- 前缀与各后缀组合、不分大小写；整个名称本身能解析（目录 ID 以 long- 开头）时按目录 ID；
// 只去掉一次前缀；未知模型与只有前缀的名称不解析
func TestResolveLongRequestModel(t *testing.T) {
	models := longTestModels()
	for _, test := range []struct {
		name   string
		id     string
		long   bool
		level  string
		hide   bool
		online bool
		ok     bool
	}{
		{name: "long-gemini-4-argon", id: "gemini-4-argon", long: true, ok: true},
		{name: "LONG-gemini-4-argon-low", id: "gemini-4-argon", long: true, level: "low", ok: true},
		{name: "Long-gemini-3.8-flash-nothinking", id: "gemini-3.8-flash", long: true, hide: true, ok: true},
		{name: "long-gemini-3.8-flash-128-nothinking-online", id: "gemini-3.8-flash", long: true, level: "minimal", hide: true, online: true, ok: true},
		{name: " models/long-gemini-4-argon-high ", id: "gemini-4-argon", long: true, level: "high", ok: true},
		{name: "long-vendor-model-low", id: "vendor-model-low", long: true, level: "low", ok: true},
		{name: "gemini-4-argon-low", id: "gemini-4-argon", level: "low", ok: true},
		{name: "long-context-model", id: "long-context-model", ok: true},
		{name: "long-context-model-low", id: "long-context-model", level: "low", ok: true},
		{name: "long-long-context-model", id: "long-context-model", long: true, ok: true},
		{name: "long-unknown-model", ok: false},
		{name: "long-", ok: false},
		{name: "gemini-4-argon-long", ok: false},
	} {
		model, alias, rest, long, ok := resolveLongRequestModel(models, test.name)
		if ok != test.ok || !ok {
			if ok != test.ok {
				t.Fatalf("%q: ok=%v，期望 %v", test.name, ok, test.ok)
			}
			continue
		}
		if model.ID != test.id || long != test.long || alias.level != test.level || alias.hideThought != test.hide || alias.online != test.online {
			t.Fatalf("%q 解析为 model=%s long=%v alias=%+v，期望 model=%s long=%v level=%q hide=%v online=%v",
				test.name, model.ID, long, alias, test.id, test.long, test.level, test.hide, test.online)
		}
		if long && strings.HasPrefix(strings.ToLower(rest), "long-") && test.id != "long-context-model" {
			t.Fatalf("%q: 去掉前缀后的名称 = %q", test.name, rest)
		}
	}
}

// TestApplyModelAliasLongOutput 带 long- 的请求与去掉前缀的同名请求整理结果完全相同，只多一个续写标记；不带前缀时不设标记；
// 不是能经 Playground 调用的文本对话模型时按普通请求处理并留一行说明；未知模型保持原名
func TestApplyModelAliasLongOutput(t *testing.T) {
	s := &server{service: modelListService{models: longTestModels()}}
	budget := int64(2048)
	base := func(model string) aistudio.GenerateRequest {
		return aistudio.GenerateRequest{
			Model:    model,
			Contents: []aistudio.Content{{Role: aistudio.RoleUser, Parts: []aistudio.Part{{Text: "写长文"}}}},
			Config:   aistudio.GenerationConfig{ThinkingBudget: &budget},
		}
	}
	for _, name := range []string{
		"gemini-4-argon", "gemini-4-argon-low", "gemini-4-argon-nothinking-online", "gemini-3.8-flash-128",
		"gemini-2.5-pro-medium", "vendor-model-low", "plain-chat", "gemini-3.8-flash-high-nothinking",
	} {
		plain := base(name)
		plainHide, err := s.applyModelAlias(context.Background(), &plain)
		if err != nil || plain.LongOutput {
			t.Fatalf("%s: 不带 long- 不应续写: %+v %v", name, plain, err)
		}
		for _, prefix := range []string{"long-", "LONG-", "Long-"} {
			long := base(prefix + name)
			longHide, err := s.applyModelAlias(context.Background(), &long)
			if err != nil || !long.LongOutput || longHide != plainHide {
				t.Fatalf("%s%s: 应开启续写: %+v hide=%v %v", prefix, name, long, longHide, err)
			}
			long.LongOutput = false
			// 名称里的选项另记在 Alias 上（额度用完改用其他模型时按目标模型重新整理），long- 只多一个 Long
			if long.Alias != nil {
				if !long.Alias.Long || plain.Alias == nil || plain.Alias.Long {
					t.Fatalf("%s%s: Alias 应只在带 long- 时记 Long: long=%+v plain=%+v", prefix, name, long.Alias, plain.Alias)
				}
				long.Alias.Long = false
			}
			if !reflect.DeepEqual(long, plain) {
				t.Fatalf("%s%s 去掉续写标记后应与不带前缀的请求相同:\n%+v\n%+v", prefix, name, long, plain)
			}
		}
	}
	for _, name := range []string{"long-image-model", "long-build-only-chat", "long-gemini-omni-1.1-flash"} {
		request := base(name)
		if _, err := s.applyModelAlias(context.Background(), &request); err != nil {
			t.Fatal(err)
		}
		want := strings.TrimPrefix(name, "long-")
		if request.LongOutput || request.Model != want || len(request.Notes) != 1 || !strings.Contains(request.Notes[0], "不自动续写") {
			t.Fatalf("%s: 应按普通请求处理并留一行说明: model=%s long=%v notes=%v", name, request.Model, request.LongOutput, request.Notes)
		}
	}
	for _, name := range []string{"long-unknown-model", "long-context-model"} {
		request := base(name)
		if _, err := s.applyModelAlias(context.Background(), &request); err != nil {
			t.Fatal(err)
		}
		if request.LongOutput || request.Model != name {
			t.Fatalf("%s: 未知模型或目录 ID 本身以 long- 开头时不续写、保持原名: %+v", name, request)
		}
	}
	if got := s.resolveModelName(context.Background(), "long-gemini-4-argon-low-online"); got != "gemini-4-argon" {
		t.Fatalf("计数与签名缓存解析 long- 名称 = %q", got)
	}
}

// TestLongOutputModelListing 模型列表只为能经 Playground 调用的文本对话模型多列一个 long-<模型>（不列 long- 与后缀的组合），
// 单模型查询能查到 long- 与后缀的组合（返回请求的名称），查不到的仍返回 404
func TestLongOutputModelListing(t *testing.T) {
	models := longTestModels()
	expanded := expandModelAliases(models)
	listed := map[string]aistudio.Model{}
	for _, model := range expanded {
		listed[model.ID] = model
	}
	for _, id := range []string{"long-gemini-4-argon", "long-gemini-3.8-flash", "long-plain-chat", "long-long-context-model"} {
		if model, ok := listed[id]; !ok || !strings.HasSuffix(model.Name, "· 自动续写") {
			t.Fatalf("列表应包含 %s: %+v", id, model)
		}
	}
	for _, id := range []string{"long-image-model", "long-build-only-chat", "long-gemini-omni-1.1-flash", "long-gemini-4-argon-low"} {
		if _, ok := listed[id]; ok {
			t.Fatalf("列表不应包含 %s", id)
		}
	}
	textModels := 0
	for _, model := range models {
		if longOutputModel(model) {
			textModels++
		}
	}
	withoutLong := 0
	for _, model := range models {
		withoutLong += 1 + len(aliasVariants(model))
	}
	if len(expanded) != withoutLong+textModels {
		t.Fatalf("列表 %d 项，期望原有 %d 项加每个可续写模型一项（%d）", len(expanded), withoutLong, textModels)
	}
	handler := NewHandler(modelListService{models: models}, Config{APIKey: "sk-test"})
	for path, wantID := range map[string]string{
		"/v1/models/long-gemini-4-argon":                     "long-gemini-4-argon",
		"/v1/models/long-gemini-4-argon-low":                 "long-gemini-4-argon-low",
		"/v1/models/LONG-gemini-3.8-flash-nothinking-online": "LONG-gemini-3.8-flash-nothinking-online",
	} {
		status, body := getModel(t, handler, path, false)
		if status != http.StatusOK || body["id"] != wantID {
			t.Fatalf("%s: status=%d body=%v", path, status, body)
		}
	}
	if status, _ := getModel(t, handler, "/v1/models/long-gpt-4o", false); status != http.StatusNotFound {
		t.Fatalf("未知模型带 long- 应返回 404，得到 %d", status)
	}
	if status, body := getModel(t, handler, "/v1beta/models/long-gemini-4-argon-low", false); status != http.StatusOK || body["name"] != "models/long-gemini-4-argon-low" {
		t.Fatalf("Gemini 单模型查询: status=%d body=%v", status, body)
	}
}

// TestContinuationFinishReason 结束码 22（continuation）按长度截断映射，不当作上游特殊原因
func TestContinuationFinishReason(t *testing.T) {
	if got := openAIFinishReason("continuation", false); got != "length" {
		t.Fatalf("OpenAI = %q", got)
	}
	if got, _ := anthropicStop("continuation", false, ""); got != "max_tokens" {
		t.Fatalf("Anthropic = %q", got)
	}
	if got := geminiFinishReason("continuation"); got != "MAX_TOKENS" {
		t.Fatalf("Gemini = %q", got)
	}
	if got := providerFinishReason("continuation"); got != "" {
		t.Fatalf("provider_finish_reason = %q，期望不单独报告", got)
	}
}

// longRecordingService 记录生成请求，返回两段正文与一次合并后的用量（与生成服务对 long- 请求的输出一致）
type longRecordingService struct {
	modelListService
	requests []aistudio.GenerateRequest
}

func (service *longRecordingService) Generate(_ context.Context, request aistudio.GenerateRequest) (<-chan aistudio.Event, error) {
	service.requests = append(service.requests, request)
	events := make(chan aistudio.Event, 4)
	events <- aistudio.Event{Kind: aistudio.EventText, Text: "第一轮"}
	events <- aistudio.Event{Kind: aistudio.EventText, Text: "第二轮"}
	events <- aistudio.Event{Kind: aistudio.EventUsage, Usage: &aistudio.Usage{InputTokens: 10, OutputTokens: 200, TotalTokens: 210}}
	events <- aistudio.Event{Kind: aistudio.EventFinish, FinishReason: "stop"}
	close(events)
	return events, nil
}

// TestLongOutputProtocolEntry 四套协议入口都把 long- 名称解析为目录模型并设续写标记，响应报告目录模型名（与后缀同口径），
// 用量为合并后的一份，请求日志每个请求一条（用量账本的用量由生成服务在转发结束时写一次，见 app 包的测试）
func TestLongOutputProtocolEntry(t *testing.T) {
	service := &longRecordingService{modelListService: modelListService{models: longTestModels()}}
	admin := &capturingAdmin{}
	handler := NewHandler(service, Config{APIKey: "sk-test", Admin: admin})
	for _, test := range []struct {
		path string
		body string
		key  string
	}{
		{path: "/v1/chat/completions", body: `{"model":"long-gemini-4-argon-low","messages":[{"role":"user","content":"hi"}]}`, key: "model"},
		{path: "/v1/messages", body: `{"model":"long-gemini-4-argon-low","max_tokens":100000,"messages":[{"role":"user","content":"hi"}]}`, key: "model"},
		{path: "/v1/responses", body: `{"model":"long-gemini-4-argon-low","input":"hi"}`, key: "model"},
		{path: "/v1beta/models/long-gemini-4-argon-low:generateContent", body: `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`, key: "modelVersion"},
	} {
		before := len(service.requests)
		recorder := postJSON(t, handler, test.path, test.body, map[string]string{"anthropic-version": "2023-06-01"})
		if recorder.Code != http.StatusOK || len(service.requests) != before+1 {
			t.Fatalf("%s: %d %s", test.path, recorder.Code, recorder.Body.String())
		}
		request := service.requests[len(service.requests)-1]
		if request.Model != "gemini-4-argon" || !request.LongOutput || request.Config.ReasoningEffort != "low" {
			t.Fatalf("%s: 生成请求 model=%s long=%v effort=%q", test.path, request.Model, request.LongOutput, request.Config.ReasoningEffort)
		}
		var body map[string]any
		if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil || body[test.key] != "gemini-4-argon" {
			t.Fatalf("%s: 响应模型名应为目录模型: %v %s", test.path, err, recorder.Body.String())
		}
		if !strings.Contains(recorder.Body.String(), "第一轮第二轮") {
			t.Fatalf("%s: 响应正文应接成一条: %s", test.path, recorder.Body.String())
		}
		if !strings.Contains(recorder.Body.String(), "200") {
			t.Fatalf("%s: 响应用量应为合并后的 200: %s", test.path, recorder.Body.String())
		}
	}
	if anthropic := service.requests[1]; anthropic.Config.MaxOutputTokens == nil || *anthropic.Config.MaxOutputTokens != 100000 {
		t.Fatalf("Anthropic max_tokens 应原样作为总上限交给生成服务: %+v", anthropic.Config.MaxOutputTokens)
	}
	if entries := admin.finished(); len(entries) != 4 {
		t.Fatalf("请求日志 %d 条，期望每个请求一条", len(entries))
	}
}

// TestLongOutputHeartbeatBetweenRounds 续写两轮之间没有事件（准备下一轮、等账号每分钟窗口）时数据型心跳照常写出，保持连接；
// 两轮的正文接在同一条流里，只有一个结束块
func TestLongOutputHeartbeatBetweenRounds(t *testing.T) {
	withHeartbeatInterval(t, 30*time.Millisecond)
	steps := []pacedStep{
		{delay: 0, event: aistudio.Event{Kind: aistudio.EventText, Text: "第一轮"}},
		{delay: 150 * time.Millisecond, event: aistudio.Event{Kind: aistudio.EventText, Text: "第二轮"}},
		{delay: 0, event: aistudio.Event{Kind: aistudio.EventFinish, FinishReason: "stop"}},
	}
	service := &pacedEventsService{modelListService: modelListService{models: longTestModels()}, steps: steps}
	handler := NewHandler(service, Config{APIKey: "sk-test"})
	recorder := postJSON(t, handler, "/v1beta/models/long-gemini-3.8-flash:streamGenerateContent?alt=sse",
		`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`, nil)
	got := collapseRuns(geminiStreamKinds(t, recorder.Body.String()))
	if want := []string{"text", "hb-text", "text", "finish"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("流 = %v，期望两轮之间有心跳: %s", got, recorder.Body.String())
	}
}
