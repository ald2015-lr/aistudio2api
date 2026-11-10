package api

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// TestRetargetGenerateRequest 改用其他模型时按目标模型重新整理协议层按模型名做过的设置：结果与客户端带同样的后缀直接请求目标模型相同
// （思考等级或预算、-online、long-）；按原生续写原样发送的预填充在目标不接受时改为补用户消息
func TestRetargetGenerateRequest(t *testing.T) {
	models := longTestModels()
	s := &server{service: modelListService{models: models}}
	byID := func(id string) aistudio.Model {
		for _, model := range models {
			if model.ID == id {
				return model
			}
		}
		t.Fatalf("目录里没有 %s", id)
		return aistudio.Model{}
	}
	prepare := func(name string, config aistudio.GenerationConfig) aistudio.GenerateRequest {
		t.Helper()
		request := aistudio.GenerateRequest{
			Model: name, Config: config,
			Contents: []aistudio.Content{{Role: aistudio.RoleUser, Parts: []aistudio.Part{{Text: "hi"}}}},
		}
		if _, err := s.prepareGenerate(context.Background(), &request); err != nil {
			t.Fatal(err)
		}
		return request
	}
	medium := "medium"
	for _, test := range []struct {
		name, source, direct, target string
		config                       aistudio.GenerationConfig
	}{
		{name: "等级后缀到预算模型", source: "gemini-4-argon-low", direct: "gemini-2.5-pro-low", target: "gemini-2.5-pro"},
		{name: "不带后缀到预算模型", source: "gemini-4-argon", direct: "gemini-2.5-pro", target: "gemini-2.5-pro"},
		{name: "客户端显式思考参数", source: "gemini-4-argon", direct: "gemini-2.5-pro", target: "gemini-2.5-pro", config: aistudio.GenerationConfig{ReasoningEffort: medium}},
		{name: "-online 到不支持搜索的模型", source: "gemini-4-argon-online-low", direct: "plain-chat-online-low", target: "plain-chat"},
		{name: "-online 到支持搜索的模型", source: "gemini-2.5-pro-online", direct: "gemini-3.8-flash-online", target: "gemini-3.8-flash"},
		{name: "long- 到能续写的模型", source: "long-gemini-4-argon-low", direct: "long-gemini-3.8-flash-low", target: "gemini-3.8-flash"},
		{name: "long- 到只在 Build 的模型", source: "long-gemini-4-argon", direct: "long-build-only-chat", target: "build-only-chat"},
	} {
		retargeted := prepare(test.source, test.config)
		notes := RetargetGenerateRequest(&retargeted, byID(test.target))
		direct := prepare(test.direct, test.config)
		if retargeted.Model != test.target || !reflect.DeepEqual(retargeted.Config, direct.Config) ||
			!reflect.DeepEqual(retargeted.Tools, direct.Tools) || retargeted.LongOutput != direct.LongOutput {
			t.Fatalf("%s：改用后 model=%s config=%+v tools=%+v long=%v，直接请求 %s 为 config=%+v tools=%+v long=%v",
				test.name, retargeted.Model, retargeted.Config, retargeted.Tools, retargeted.LongOutput,
				test.direct, direct.Config, direct.Tools, direct.LongOutput)
		}
		if retargeted.Alias == nil || retargeted.Alias.OnlineAppended != direct.Alias.OnlineAppended {
			t.Fatalf("%s：Alias = %+v，期望与直接请求一致 %+v", test.name, retargeted.Alias, direct.Alias)
		}
		if long := len(direct.Notes) > 0; long != (len(notes) > 0) {
			t.Fatalf("%s：说明 = %v，直接请求的说明 = %v", test.name, notes, direct.Notes)
		}
	}
	// 预填充：源模型在原生续写名单里、按模型轮原样发送；目标不在名单里时补一条请模型接着写的用户消息并写说明，在名单里时不动
	prefill := aistudio.GenerateRequest{
		Model: "gemini-3.1-pro-preview", PrefillTail: true,
		Contents: []aistudio.Content{
			{Role: aistudio.RoleUser, Parts: []aistudio.Part{{Text: "写一首诗"}}},
			{Role: aistudio.RoleAssistant, Parts: []aistudio.Part{{Text: "床前"}}},
		},
	}
	native := prefill
	if notes := RetargetGenerateRequest(&native, aistudio.Model{ID: "gemini-3-flash-preview"}); len(notes) != 0 || !native.PrefillTail || len(native.Contents) != 2 {
		t.Fatalf("目标在原生续写名单里时不应改动：%+v %v", native, notes)
	}
	notes := RetargetGenerateRequest(&prefill, byID("gemini-4-argon"))
	if prefill.PrefillTail || len(prefill.Contents) != 3 || prefill.Contents[2].Parts[0].Text != aistudio.PrefillContinuationPrompt ||
		len(notes) != 1 || !strings.Contains(notes[0], "不接受以模型轮结尾的请求") {
		t.Fatalf("目标不接受原生续写时应补用户消息：%+v %v", prefill, notes)
	}
}

// TestServedModel 公开 API 的请求上挂着改用记录：没有改用时报告请求的模型，生成服务改用后报告目标模型
func TestServedModel(t *testing.T) {
	record := aistudio.NewModelSwitch()
	ctx := aistudio.ContextWithModelSwitch(context.Background(), record)
	if servedModel(ctx, "gemini-4-argon") != "gemini-4-argon" || servedModel(context.Background(), "x") != "x" {
		t.Fatal("没有改用时应报告请求的模型")
	}
	record.Switch("gemini-4-argon", "gemini-4-carbon")
	if servedModel(ctx, "gemini-4-argon") != "gemini-4-carbon" {
		t.Fatal("改用后应报告目标模型")
	}
}

// switchingService 模拟生成服务在提交响应头之后才额度用完、改用其他模型：delay 后在请求的改用记录上写入改用，再隔 after 交出正文与结束
type switchingService struct {
	modelListService
	delay, after time.Duration
}

func (service switchingService) Generate(ctx context.Context, request aistudio.GenerateRequest) (<-chan aistudio.Event, error) {
	events := make(chan aistudio.Event)
	go func() {
		defer close(events)
		for index, wait := range []time.Duration{service.delay, service.after} {
			select {
			case <-time.After(wait):
			case <-ctx.Done():
				return
			}
			if index == 0 {
				aistudio.ModelSwitchFromContext(ctx).Switch(request.Model, "gemini-4-carbon")
			}
		}
		for _, event := range []aistudio.Event{{Kind: aistudio.EventText, Text: "hi"}, {Kind: aistudio.EventFinish, FinishReason: "stop"}} {
			select {
			case events <- event:
			case <-ctx.Done():
				return
			}
		}
	}()
	return events, nil
}

// TestModelFallbackStreamAfterHeaders 改用发生在流式响应头提交之后（源模型很晚才额度用完）：已写出的开头保留源模型，之后的块（含心跳块）、
// Responses 的 response.completed 与 Interactions 的 interaction.completed 报告实际使用的模型。原先 Responses 整段报告源模型，
// OpenAI 与 Gemini 的心跳块一直是源模型
func TestModelFallbackStreamAfterHeaders(t *testing.T) {
	withStreamStartHold(t, 40*time.Millisecond)
	withHeartbeatInterval(t, 30*time.Millisecond)
	const source, target = "gemini-4-argon", "gemini-4-carbon"
	handler := NewHandler(switchingService{modelListService: modelListService{models: longTestModels()}, delay: 150 * time.Millisecond, after: 150 * time.Millisecond},
		Config{APIKey: "sk-test"})
	// OpenAI：role 块为源模型；改用之后的心跳块、正文与结束块为目标模型
	chunks := sseData(t, postJSON(t, handler, "/v1/chat/completions", `{"model":"`+source+`","stream":true,"messages":[{"role":"user","content":"hi"}]}`, nil).Body.String())
	if len(chunks) < 3 || chunks[0]["model"] != source {
		t.Fatalf("OpenAI 的 role 块应保留源模型：%v", chunks)
	}
	heartbeatTarget := false
	for _, chunk := range chunks {
		choices, _ := chunk["choices"].([]any)
		if len(choices) == 0 {
			continue
		}
		choice, _ := choices[0].(map[string]any)
		delta, _ := choice["delta"].(map[string]any)
		if delta["content"] == "hi" || choice["finish_reason"] != nil {
			if chunk["model"] != target {
				t.Fatalf("OpenAI 改用之后的块应报告 %s：%v", target, chunk)
			}
		}
		if len(delta) == 0 && choice["finish_reason"] == nil && chunk["model"] == target {
			heartbeatTarget = true
		}
	}
	if !heartbeatTarget {
		t.Fatalf("OpenAI 改用之后的心跳块应报告 %s：%v", target, chunks)
	}
	// Gemini：改用之后的心跳块与正文块的 modelVersion 为目标模型，之后不再出现源模型
	chunks = sseData(t, postJSON(t, handler, "/v1beta/models/"+source+":streamGenerateContent?alt=sse", `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`, nil).Body.String())
	switched, heartbeatTarget := false, false
	for _, chunk := range chunks {
		if chunk["modelVersion"] == target {
			switched = true
			heartbeatTarget = heartbeatTarget || geminiHeartbeatChunk(chunk)
		} else if switched {
			t.Fatalf("Gemini 改用之后的块不应再报告源模型：%v", chunks)
		}
	}
	if chunks[0]["modelVersion"] != source || !heartbeatTarget || chunks[len(chunks)-1]["modelVersion"] != target {
		t.Fatalf("Gemini 的块 = %v，期望开头为源模型、改用之后（含心跳块）为目标模型", chunks)
	}
	// Responses：response.created 保留源模型，response.completed 为目标模型
	events := sseData(t, postJSON(t, handler, "/v1/responses", `{"model":"`+source+`","stream":true,"input":"hi"}`, nil).Body.String())
	models := map[string]any{}
	for _, event := range events {
		if response, ok := event["response"].(map[string]any); ok {
			models[event["type"].(string)] = response["model"]
		}
	}
	if models["response.created"] != source || models["response.completed"] != target {
		t.Fatalf("Responses 的模型 = %v，期望 created 为 %s、completed 为 %s", models, source, target)
	}
	// Interactions：interaction.completed 为目标模型
	events = sseData(t, postJSON(t, handler, "/v1beta/interactions", `{"model":"`+source+`","stream":true,"input":"hi"}`, nil).Body.String())
	completed := ""
	for _, event := range events {
		if interaction, ok := event["interaction"].(map[string]any); ok && interaction["status"] != "in_progress" {
			completed, _ = interaction["model"].(string)
		}
	}
	if completed != target {
		t.Fatalf("Interactions 结束时的模型 = %q，期望 %s：%v", completed, target, events)
	}
	// Anthropic：message_start 已按源模型写出，之后的事件不带模型（README 写明）
	events = sseData(t, postJSON(t, handler, "/v1/messages", `{"model":"`+source+`","stream":true,"max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`, nil).Body.String())
	if message, _ := events[0]["message"].(map[string]any); events[0]["type"] != "message_start" || message["model"] != source {
		t.Fatalf("Anthropic 的 message_start = %v", events[0])
	}
}

// TestAwaitStreamStartRestartWait 生成服务在首个事件之前改用其他模型（StreamStartHold.RestartWait）：写出响应头前的等待从改用时刻起按目标模型
// 重新计，目标模型的首个事件在原来的等待之后才到时仍先收到它（错误照常按 HTTP 状态返回）；合计不超过上限
func TestAwaitStreamStartRestartWait(t *testing.T) {
	run := func(restart time.Duration, holdMax time.Duration, first aistudio.Event) (bool, time.Duration, error) {
		hold := aistudio.NewStreamStartHold()
		ctx := aistudio.WithStreamStartHold(context.Background(), hold)
		events := make(chan aistudio.Event, 1)
		go func() {
			time.Sleep(50 * time.Millisecond)
			if restart > 0 {
				hold.RestartWait(restart)
			}
			time.Sleep(250 * time.Millisecond)
			events <- first
			close(events)
		}()
		startedAt := time.Now()
		stream, ready, err := awaitStreamStartSince(ctx, events, 100*time.Millisecond, holdMax, startedAt)
		elapsed := time.Since(startedAt)
		if stream != nil {
			go drainEvents(stream)
		}
		return ready, elapsed, err
	}
	text := aistudio.Event{Kind: aistudio.EventText, Text: "hi"}
	if ready, _, err := run(0, 2*time.Second, text); ready || err != nil {
		t.Fatalf("没有改用时 100ms 到期就先提交响应头：ready=%v err=%v", ready, err)
	}
	if ready, _, err := run(time.Second, 2*time.Second, text); !ready || err != nil {
		t.Fatalf("改用后应等到目标模型的首个事件：ready=%v err=%v", ready, err)
	}
	failure := aistudio.Event{Kind: aistudio.EventError, Err: aistudio.ErrInvalidArgument}
	if _, _, err := run(time.Second, 2*time.Second, failure); err == nil {
		t.Fatal("改用后目标模型首事件前的错误应按 HTTP 状态返回")
	}
	if ready, elapsed, _ := run(10*time.Second, 200*time.Millisecond, text); ready || elapsed > time.Second {
		t.Fatalf("合计不超过上限：ready=%v 用时 %s", ready, elapsed)
	}
}
