package aistudio

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"unicode/utf8"
)

// omniSystemTestPrompt 为 Chatbox 一类客户端自动加的系统提示
const omniSystemTestPrompt = "You are a helpful assistant."

// interactionSystemField 返回 CreateInteractionStream 请求体里 interaction field 7（系统指令）的值，没有时为 nil
func interactionSystemField(t *testing.T, body []byte) any {
	t.Helper()
	var root []any
	if err := json.Unmarshal(body, &root); err != nil || len(root) < 4 {
		t.Fatalf("请求体 %s: %v", body, err)
	}
	interaction, ok := root[3].([]any)
	if !ok || len(interaction) < 7 {
		t.Fatalf("interaction 结构 %s", body)
	}
	return interaction[6]
}

// TestOmniSystemPromptBeforeSend 协议层没有认出 Omni（别名目录取不到或缓存还没刷新）、只标记了 IgnoreOmniSystemPrompt 的请求，
// 生成服务按所选账户的目录判定为能输出视频的 Omni 时在发送前去掉系统提示：编码出的请求体 field 7 为 null，与不带系统提示的请求逐字节相同，
// 并经请求说明观察者记同一句说明；system 角色消息转成的轮次整轮去掉。没有标记（协议层已处理或开关关闭）时与原先完全一致；
// 不输出视频的 Interaction 模型不受影响；本服务按工具设置追加的要求保留
func TestOmniSystemPromptBeforeSend(t *testing.T) {
	const accountID = "omni-system@example.com"
	capture := &interactionCapture{}
	client, err := NewClient(ClientOptions{Transport: ceilingTransport{}, Protected: capture})
	if err != nil {
		t.Fatal(err)
	}
	omni := omniTestModel()
	omni.Capabilities["google_search"] = true
	// plain 经 CreateInteractionStream 生成、但目录没有列出视频选项，不是能输出视频的 Omni
	plain := Model{ID: "gemini-interaction-text", Methods: []string{"generateContent"}, Capabilities: map[string]bool{"interactions_api": true, "thinking_level": true}}
	client.catalogs[accountID] = modelCatalog{models: []Model{omni, plain}, entries: map[string]modelEntry{
		omni.ID: {model: omni, defaults: omniTestDefaults()}, plain.ID: {model: plain, defaults: omniTestDefaults()},
	}}
	user := []Content{{Role: RoleUser, Parts: []Part{{Text: "随机\n"}}}}
	run := func(request GenerateRequest) ([]byte, []string) {
		t.Helper()
		var notes []string
		ctx := ContextWithRequestNoteObserver(context.Background(), func(note string) { notes = append(notes, note) })
		before := len(capture.bodies)
		request.AccountID = accountID
		if _, err := client.Generate(ctx, request); err == nil {
			t.Fatal("测试上游应返回错误")
		}
		if len(capture.bodies) != before+1 {
			t.Fatalf("应发出一次 CreateInteractionStream，得到 %d 次", len(capture.bodies)-before)
		}
		return capture.bodies[len(capture.bodies)-1], notes
	}
	baseline, notes := run(GenerateRequest{Model: omni.ID, Contents: user})
	if interactionSystemField(t, baseline) != nil || len(notes) != 0 {
		t.Fatalf("不带系统提示的请求: field 7=%v notes=%q", interactionSystemField(t, baseline), notes)
	}
	note := OmniSystemPromptNote(SystemPromptDrop{Chars: utf8.RuneCountInString(omniSystemTestPrompt)})

	// 标记了且所选账户的目录为 Omni：去掉，与不带系统提示的请求逐字节相同，记说明
	body, notes := run(GenerateRequest{Model: omni.ID, System: omniSystemTestPrompt, Contents: user, IgnoreOmniSystemPrompt: true})
	if string(body) != string(baseline) || !slices.Equal(notes, []string{note}) {
		t.Fatalf("发送前应去掉系统提示并记说明:\n得到 %s\n期望 %s\nnotes=%q", body, baseline, notes)
	}
	// system 角色消息转成的轮次同样整轮去掉
	reminder := Content{Role: RoleUser, Parts: []Part{{Text: SystemReminderOpen + "以中文回答" + SystemReminderClose}}, FromSystem: true}
	body, notes = run(GenerateRequest{
		Model: omni.ID, System: omniSystemTestPrompt, Contents: append([]Content{reminder}, user...), IgnoreOmniSystemPrompt: true,
	})
	turnNote := OmniSystemPromptNote(SystemPromptDrop{Chars: utf8.RuneCountInString(omniSystemTestPrompt) + 5, Turns: 1})
	if string(body) != string(baseline) || !slices.Equal(notes, []string{turnNote}) {
		t.Fatalf("system 角色消息应整轮去掉:\n得到 %s\n期望 %s\nnotes=%q", body, baseline, notes)
	}

	// 没有标记（协议层已处理或开关关闭）：与原先一致，系统提示写在 field 7，不记说明
	body, notes = run(GenerateRequest{Model: omni.ID, System: omniSystemTestPrompt, Contents: user})
	if interactionSystemField(t, body) != omniSystemTestPrompt || len(notes) != 0 {
		t.Fatalf("没有标记时应照常发送: field 7=%v notes=%q", interactionSystemField(t, body), notes)
	}
	// 不输出视频的 Interaction 模型：标记了也不受影响
	body, notes = run(GenerateRequest{Model: plain.ID, System: omniSystemTestPrompt, Contents: user, IgnoreOmniSystemPrompt: true})
	if interactionSystemField(t, body) != omniSystemTestPrompt || len(notes) != 0 {
		t.Fatalf("其他模型不应去掉: field 7=%v notes=%q", interactionSystemField(t, body), notes)
	}

	// 经号池生成（先追加工具要求再选号发送）：只去掉客户端的系统提示，追加的工具要求保留
	pool := testPoolWithAccount(t, accountID)
	if err := pool.SetCatalog(accountID, BenefitTierFree, []Model{omni}); err != nil {
		t.Fatal(err)
	}
	service, err := NewPooledService(pool, client)
	if err != nil {
		t.Fatal(err)
	}
	const hint = "Use high search context depth."
	pooled := func(flag bool) (any, []string) {
		t.Helper()
		lease, err := pool.AcquireFor(context.Background(), AccountSelection{AccountID: accountID})
		if err != nil {
			t.Fatal(err)
		}
		defer lease.Release()
		var notes []string
		ctx := ContextWithRequestNoteObserver(ContextWithAccountLease(context.Background(), lease), func(note string) { notes = append(notes, note) })
		before := len(capture.bodies)
		_, _ = service.Generate(ctx, GenerateRequest{
			Model: omni.ID, System: omniSystemTestPrompt, Contents: user, IgnoreOmniSystemPrompt: flag,
			Tools: Tools{GoogleSearch: &GoogleSearchOptions{WebSearch: true, ContextSize: "high"}},
		})
		if len(capture.bodies) != before+1 {
			t.Fatalf("应发出一次 CreateInteractionStream，得到 %d 次", len(capture.bodies)-before)
		}
		return interactionSystemField(t, capture.bodies[len(capture.bodies)-1]), notes
	}
	if system, notes := pooled(true); system != hint || !slices.Equal(notes, []string{note}) {
		t.Fatalf("应只保留工具要求: field 7=%v notes=%q", system, notes)
	}
	if system, notes := pooled(false); system != omniSystemTestPrompt+"\n"+hint || len(notes) != 0 {
		t.Fatalf("没有标记时系统提示与工具要求照常发送: field 7=%v notes=%q", system, notes)
	}
}

// TestOmniSystemPromptBeforeSendRules 发送前补做与协议层同一规则：只有 system 角色消息时只去掉系统提示、保留这些轮次；
// 没有客户端系统提示（只有工具要求）时不改动；只有系统提示时保留并记同一句说明；不改动调用方的 contents
func TestOmniSystemPromptBeforeSendRules(t *testing.T) {
	omni := omniTestModel()
	reminder := Content{Role: RoleUser, Parts: []Part{{Text: SystemReminderOpen + "规则" + SystemReminderClose}}, FromSystem: true}
	user := Content{Role: RoleUser, Parts: []Part{{Text: "做个视频"}}}

	original := []Content{reminder, user}
	request := GenerateRequest{System: "  ", Contents: original, IgnoreOmniSystemPrompt: true}
	note, ok := ignoreOmniSystemPromptBeforeSend(&request, omni)
	if !ok || note != OmniSystemPromptNote(SystemPromptDrop{Chars: 2 + 2, Turns: 1}) || request.System != "" || len(request.Contents) != 1 ||
		request.Contents[0].FromSystem || request.IgnoreOmniSystemPrompt || !original[0].FromSystem {
		t.Fatalf("去掉 system 角色消息: note=%q ok=%v request=%+v original=%+v", note, ok, request, original)
	}

	request = GenerateRequest{System: omniSystemTestPrompt, Contents: []Content{reminder}, IgnoreOmniSystemPrompt: true}
	note, ok = ignoreOmniSystemPromptBeforeSend(&request, omni)
	if !ok || note != OmniSystemPromptNote(SystemPromptDrop{Chars: utf8.RuneCountInString(omniSystemTestPrompt), KeptChars: 2, KeptTurns: 1}) ||
		request.System != "" || len(request.Contents) != 1 {
		t.Fatalf("只有 system 角色消息: note=%q ok=%v request=%+v", note, ok, request)
	}

	request = GenerateRequest{System: omniSystemTestPrompt, IgnoreOmniSystemPrompt: true}
	note, ok = ignoreOmniSystemPromptBeforeSend(&request, omni)
	if !ok || note != OmniSystemPromptNote(SystemPromptDrop{KeptChars: utf8.RuneCountInString(omniSystemTestPrompt)}) || request.System != omniSystemTestPrompt {
		t.Fatalf("只有系统提示: note=%q ok=%v request=%+v", note, ok, request)
	}

	// 只有工具要求、没有客户端的系统提示：不改动
	request = GenerateRequest{Contents: []Content{user}, IgnoreOmniSystemPrompt: true, Tools: Tools{GoogleSearch: &GoogleSearchOptions{ContextSize: "low"}}}
	prepared, _, err := prepareToolRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	system := prepared.System
	if note, ok := ignoreOmniSystemPromptBeforeSend(&prepared, omni); ok || note != "" || prepared.System != system || system == "" {
		t.Fatalf("只有工具要求: note=%q ok=%v system=%q", note, ok, prepared.System)
	}
}
