package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/api"
	"github.com/Mag1cFall/AIStudio2API/internal/config"
)

// scriptedEventsService 是测试用的上游：每次 Generate 按 events 交出同样的事件序列
type scriptedEventsService struct {
	*stallingService
	events []aistudio.Event
}

func (service *scriptedEventsService) Generate(_ context.Context, request aistudio.GenerateRequest) (<-chan aistudio.Event, error) {
	service.mu.Lock()
	service.calls = append(service.calls, request.AccountID)
	service.mu.Unlock()
	events := make(chan aistudio.Event, len(service.events))
	for _, event := range service.events {
		event.ProviderModel = testRuntimeModel
		events <- event
	}
	close(events)
	return events, nil
}

// runScriptedRequest 执行一次生成，返回转发给协议层的全部事件
func runScriptedRequest(t *testing.T, service *trackedService, request aistudio.GenerateRequest) []aistudio.Event {
	t.Helper()
	requestCtx, requestCancel := context.WithTimeout(service.lifecycle, 20*time.Second)
	defer requestCancel()
	events := make(chan aistudio.Event, 16)
	done := make(chan struct{})
	go func() {
		defer close(done)
		service.generateWithRetry(service.lifecycle, requestCtx, requestCancel, time.Now(), request, "", events, newGenerationDiagnostics(request, "未发送"))
	}()
	var received []aistudio.Event
	for event := range events {
		received = append(received, event)
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("generateWithRetry 没有结束")
	}
	return received
}

func abnormalTestRequest(id string) aistudio.GenerateRequest {
	return aistudio.GenerateRequest{
		ID: id, Model: testRuntimeModel,
		Contents: []aistudio.Content{{Role: aistudio.RoleUser, Parts: []aistudio.Part{{Text: "hi"}}}},
	}
}

func eventKinds(events []aistudio.Event) string {
	kinds := make([]string, 0, len(events))
	for _, event := range events {
		kinds = append(kinds, string(event.Kind))
	}
	return strings.Join(kinds, ",")
}

// abnormalFinishEvents 为重新生成用完后上游交出的结果：没有内容，用量之后以 MALFORMED_FUNCTION_CALL 结束
func abnormalFinishEvents(prefix ...aistudio.Event) []aistudio.Event {
	return append(prefix,
		aistudio.Event{Kind: aistudio.EventUsage, Usage: &aistudio.Usage{InputTokens: 120, OutputTokens: 7, TotalTokens: 127}},
		aistudio.Event{Kind: aistudio.EventFinish, FinishReason: "malformed_function_call", FinishMessage: "Malformed function call: lookup(id=)"},
	)
}

// TestAbnormalFinishErrorOff ABNORMAL_FINISH_ERROR 关闭（默认）时与原先相同：用量与异常结束原样转发，请求按完成结束
func TestAbnormalFinishErrorOff(t *testing.T) {
	upstream := &scriptedEventsService{stallingService: newStallingService(), events: abnormalFinishEvents()}
	service, pool := firstEventTestService(t, upstream, 0, "alice@example.com")
	events := runScriptedRequest(t, service, abnormalTestRequest("req-abnormal-off"))
	if eventKinds(events) != "usage,finish" || events[1].FinishReason != "malformed_function_call" {
		t.Fatalf("关闭时应原样转发: %s", eventKinds(events))
	}
	if lines := queueProgressLines(service, "req-abnormal-off", "ABNORMAL_FINISH_ERROR"); len(lines) != 0 {
		t.Fatalf("关闭时不应写说明: %+v", lines)
	}
	assertLeasesReleased(t, pool)
}

// TestAbnormalFinishErrorOn 开关打开时没有交出内容就以异常原因结束的回复换成 *aistudio.AbnormalFinishError：带上游用量与共生成次数，
// 只请求一次上游（不换号）、账户不冷却，写一行说明；扣住的用量不再单独交出
func TestAbnormalFinishErrorOn(t *testing.T) {
	for _, reason := range []string{"malformed_function_call", "unexpected_tool_call", "malformed_response"} {
		events := abnormalFinishEvents()
		events[1].FinishReason = reason
		upstream := &scriptedEventsService{stallingService: newStallingService(), events: events}
		service, pool := firstEventTestService(t, upstream, 0, "alice@example.com", "bob@example.com")
		service.setAbnormalFinishError(true)
		received := runScriptedRequest(t, service, abnormalTestRequest("req-abnormal-on"))
		if eventKinds(received) != "error" {
			t.Fatalf("%s：应只交出一个错误事件: %s", reason, eventKinds(received))
		}
		var abnormal *aistudio.AbnormalFinishError
		if !errors.As(received[0].Err, &abnormal) || abnormal.Reason != reason || abnormal.Usage == nil || abnormal.Usage.TotalTokens != 127 ||
			abnormal.Generations != 1 {
			t.Fatalf("%s：错误 = %#v", reason, received[0].Err)
		}
		upstream.mu.Lock()
		calls := len(upstream.calls)
		upstream.mu.Unlock()
		if calls != 1 {
			t.Fatalf("%s：不算账户的问题，不应换号: 请求了 %d 次", reason, calls)
		}
		lines := queueProgressLines(service, "req-abnormal-on", "ABNORMAL_FINISH_ERROR")
		if len(lines) != 1 || lines[0].Level != "WARN" || !strings.Contains(lines[0].Message, "结束原因="+strings.ToUpper(reason)) {
			t.Fatalf("%s：说明 = %+v", reason, lines)
		}
		// 上游结束说明照旧写
		if reason == "malformed_function_call" && len(queueProgressLines(service, "req-abnormal-on", "上游结束说明")) != 1 {
			t.Fatal("上游结束说明应照旧写")
		}
		assertLeasesReleased(t, pool)
	}
}

// TestAbnormalFinishKeepsContent 已经交出内容（正文、思考正文、函数调用）时照旧以结束原因结束；扣住的用量按原顺序放出
func TestAbnormalFinishKeepsContent(t *testing.T) {
	for name, content := range map[string]aistudio.Event{
		"正文":   {Kind: aistudio.EventText, Text: "partial"},
		"思考正文": {Kind: aistudio.EventReasoning, Text: "thinking"},
		"函数调用": {Kind: aistudio.EventToolCall, ToolCall: &aistudio.FunctionCall{ID: "call-1", Name: "lookup", Arguments: []byte(`{}`)}},
	} {
		upstream := &scriptedEventsService{stallingService: newStallingService(), events: abnormalFinishEvents(content)}
		service, pool := firstEventTestService(t, upstream, 0, "alice@example.com")
		service.setAbnormalFinishError(true)
		received := runScriptedRequest(t, service, abnormalTestRequest("req-abnormal-content"))
		if got := eventKinds(received); got != string(content.Kind)+",usage,finish" {
			t.Fatalf("%s：已交出内容时应照旧结束: %s", name, got)
		}
		assertLeasesReleased(t, pool)
	}
	// 用量在内容之前到达（其他通道）：先扣住，下一个事件到达时按原顺序放出
	upstream := &scriptedEventsService{stallingService: newStallingService(), events: []aistudio.Event{
		{Kind: aistudio.EventUsage, Usage: &aistudio.Usage{InputTokens: 3}},
		{Kind: aistudio.EventText, Text: "hello"},
		{Kind: aistudio.EventFinish, FinishReason: "stop"},
	}}
	service, _ := firstEventTestService(t, upstream, 0, "alice@example.com")
	service.setAbnormalFinishError(true)
	if got := eventKinds(runScriptedRequest(t, service, abnormalTestRequest("req-abnormal-order"))); got != "usage,text,finish" {
		t.Fatalf("扣住的用量应按原顺序放出: %s", got)
	}
}

// TestAbnormalFinishSignatureNotContent 思考签名、来源、引用与空正文的思考不算内容：没有交出内容时先扣住，异常结束时连同用量一起丢弃、
// 只交出错误（原先签名当场交出，协议层随之提交 200，错误只能作为流内错误）；之后出现内容或正常结束时照原顺序先放出
func TestAbnormalFinishSignatureNotContent(t *testing.T) {
	nonContent := []aistudio.Event{
		{Kind: aistudio.EventThoughtSignature, ThoughtSignature: "sig-fake"},
		{Kind: aistudio.EventGrounding, Grounding: &aistudio.GroundingMetadata{WebSearchQueries: []string{"weather"}}},
		{Kind: aistudio.EventCitation, Citation: &aistudio.Citation{URL: "https://example.com/a"}},
		{Kind: aistudio.EventReasoning, ThoughtSignature: "sig-empty-thought"},
	}
	upstream := &scriptedEventsService{stallingService: newStallingService(), events: abnormalFinishEvents(nonContent...)}
	service, pool := firstEventTestService(t, upstream, 0, "alice@example.com")
	service.setAbnormalFinishError(true)
	received := runScriptedRequest(t, service, abnormalTestRequest("req-abnormal-signature"))
	var abnormal *aistudio.AbnormalFinishError
	if eventKinds(received) != "error" || !errors.As(received[0].Err, &abnormal) || abnormal.Usage == nil || abnormal.Usage.TotalTokens != 127 {
		t.Fatalf("只交出过不算内容的事件时应只交出错误: %s", eventKinds(received))
	}
	assertLeasesReleased(t, pool)
	// 之后出现内容：扣住的事件照原顺序在内容之前放出
	upstream.events = append(append([]aistudio.Event(nil), nonContent...),
		aistudio.Event{Kind: aistudio.EventText, Text: "hello"},
		aistudio.Event{Kind: aistudio.EventUsage, Usage: &aistudio.Usage{TotalTokens: 9}},
		aistudio.Event{Kind: aistudio.EventFinish, FinishReason: "malformed_function_call"},
	)
	if got := eventKinds(runScriptedRequest(t, service, abnormalTestRequest("req-abnormal-signature-text"))); got !=
		"thought_signature,grounding,citation,reasoning,text,usage,finish" {
		t.Fatalf("出现内容后应照原顺序放出: %s", got)
	}
	// 没有内容的正常结束：扣住的事件照原顺序在结束之前放出
	upstream.events = append(append([]aistudio.Event(nil), nonContent...),
		aistudio.Event{Kind: aistudio.EventUsage, Usage: &aistudio.Usage{TotalTokens: 9}},
		aistudio.Event{Kind: aistudio.EventFinish, FinishReason: "stop"},
	)
	if got := eventKinds(runScriptedRequest(t, service, abnormalTestRequest("req-abnormal-signature-stop"))); got !=
		"thought_signature,grounding,citation,reasoning,usage,finish" {
		t.Fatalf("正常结束时应照原顺序放出: %s", got)
	}
	// 流内错误：扣住的事件照原顺序在错误之前放出（与开关关闭时相同）
	upstream.events = append(append([]aistudio.Event(nil), nonContent[0]),
		aistudio.Event{Kind: aistudio.EventError, Err: errors.New("upstream broke")},
	)
	if got := eventKinds(runScriptedRequest(t, service, abnormalTestRequest("req-abnormal-signature-error"))); got != "thought_signature,error" {
		t.Fatalf("流内错误之前应放出扣住的事件: %s", got)
	}
}

// TestAbnormalFinishAfterSignatureStreamIsBadGateway 经公开 API 的流式请求（OpenAI Chat、Anthropic thinking.disabled）：上游先交出只带签名的
// 事件（Gemini 3/4 思考模型常见；Anthropic 不返回思考时带签名的思考换成签名事件）再以 MALFORMED_FUNCTION_CALL 结束，开关打开时仍按 HTTP 502
// 返回，没有提交 200 与流内错误
func TestAbnormalFinishAfterSignatureStreamIsBadGateway(t *testing.T) {
	upstream := &scriptedEventsService{stallingService: newStallingService(), events: abnormalFinishEvents(
		aistudio.Event{Kind: aistudio.EventReasoning, Text: "", ThoughtSignature: "sig-fake"},
		aistudio.Event{Kind: aistudio.EventThoughtSignature, ThoughtSignature: "sig-fake-2"},
	)}
	service, _ := runningGenerateService(t, upstream, context.Background(), time.Minute, "alice@example.com")
	service.setAbnormalFinishError(true)
	handler := api.NewHandler(service, api.Config{APIKey: "sk-test"})
	for _, test := range []struct {
		name, path, body string
	}{
		{"ChatStream", "/v1/chat/completions", `{"model":"` + testRuntimeModel + `","stream":true,"messages":[{"role":"user","content":"hi"}]}`},
		{"AnthropicStream", "/v1/messages", `{"model":"` + testRuntimeModel + `","stream":true,"max_tokens":16,"thinking":{"type":"disabled"},` +
			`"messages":[{"role":"user","content":"hi"}]}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, test.path, strings.NewReader(test.body))
			request.Header.Set("Authorization", "Bearer sk-test")
			request.Header.Set("X-Api-Key", "sk-test")
			request.Header.Set("Anthropic-Version", "2023-06-01")
			request.Header.Set("Content-Type", "application/json")
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, request)
			body := recorder.Body.String()
			if recorder.Code != http.StatusBadGateway || !strings.Contains(body, "MALFORMED_FUNCTION_CALL") ||
				strings.Contains(body, "data:") || strings.Contains(body, "sig-fake") {
				t.Fatalf("status = %d，响应 %s，期望在提交响应头之前按 502 返回、不带签名", recorder.Code, body)
			}
		})
	}
}

// TestAbnormalFinishHeldSignatureRemembered 额度用完改用其他模型时，扣住后放出的思考签名照样记为目标模型签发（之后发给别的模型时换掉）
func TestAbnormalFinishHeldSignatureRemembered(t *testing.T) {
	const heldSignature = "REAL-SIG-held-before-text"
	accounts := ultraTestIDs(2)
	upstream := &fallbackUpstream{leading: map[string][]aistudio.Event{
		fallbackTarget: {{Kind: aistudio.EventThoughtSignature, ThoughtSignature: heldSignature}},
	}}
	service, pool := fallbackService(t, upstream, aistudio.BenefitTierUltra, config.ModelFallbackScopeUltra, accounts...)
	service.setAbnormalFinishError(true)
	coolAll(t, pool, fallbackSource, 3*time.Hour, accounts...)
	admin := &accessLogCapture{}
	handler := api.NewHandler(service, api.Config{APIKey: "sk-test", Admin: admin})
	recorder, _ := postFallbackTo(t, handler, admin, "/ultra/v1/chat/completions",
		`{"model":"`+fallbackSource+`","messages":[{"role":"user","content":"q"}]}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("状态 %d，响应 %s", recorder.Code, recorder.Body.String())
	}
	if issuer := service.fallbackSignatures.issuer(heldSignature); issuer != fallbackTarget {
		t.Fatalf("扣住后放出的签名应登记为 %s 签发，实际 %q", fallbackTarget, issuer)
	}
}

// TestAbnormalFinishGeminiKeeps Gemini 原生接口（KeepAbnormalFinish）照旧以结束原因结束；正常结束不受开关影响
func TestAbnormalFinishGeminiKeeps(t *testing.T) {
	upstream := &scriptedEventsService{stallingService: newStallingService(), events: abnormalFinishEvents()}
	service, _ := firstEventTestService(t, upstream, 0, "alice@example.com")
	service.setAbnormalFinishError(true)
	request := abnormalTestRequest("req-abnormal-gemini")
	request.KeepAbnormalFinish = true
	if got := eventKinds(runScriptedRequest(t, service, request)); got != "usage,finish" {
		t.Fatalf("Gemini 原生接口应保留结束原因: %s", got)
	}
	upstream.events = []aistudio.Event{{Kind: aistudio.EventUsage, Usage: &aistudio.Usage{}}, {Kind: aistudio.EventFinish, FinishReason: "stop"}}
	if got := eventKinds(runScriptedRequest(t, service, abnormalTestRequest("req-abnormal-stop"))); got != "usage,finish" {
		t.Fatalf("没有内容的正常结束不受影响: %s", got)
	}
}

// TestAbnormalFinishGenerations 共生成次数按这次运行的重新生成次数加 1
func TestAbnormalFinishGenerations(t *testing.T) {
	budget := aistudio.NewRegenerationBudget()
	diag := &generationDiagnostics{abnormalFinish: true, regenerations: budget}
	gate := diag.abnormalFinishGateFor()
	if hold, _ := gate.admit(aistudio.Event{Kind: aistudio.EventUsage, Usage: &aistudio.Usage{TotalTokens: 1}}); !hold {
		t.Fatal("用量应扣住")
	}
	_, abnormal := gate.admit(aistudio.Event{Kind: aistudio.EventFinish, FinishReason: "MALFORMED_RESPONSE"})
	if abnormal == nil || abnormal.Generations != 1 || abnormal.Usage == nil || len(gate.release()) != 0 {
		t.Fatalf("结果 = %+v", abnormal)
	}
	if disabled := (*generationDiagnostics)(nil).abnormalFinishGateFor(); disabled.enabled {
		t.Fatal("没有 diag 时不应启用")
	}
	if note := abnormalFinishNote(&aistudio.AbnormalFinishError{Reason: "malformed_function_call", Generations: 3}); !strings.Contains(note, "共生成=3 次") ||
		!strings.Contains(note, "MALFORMED_FUNCTION_CALL") {
		t.Fatalf("说明 = %q", note)
	}
}

// TestAbnormalFinishGenerationsPerAttempt 共生成次数只计交出结果的那次尝试（账号）：第一个账号流尾 503、同账号重新生成 2 次仍是 503 后换号，
// 第二个账号以 MALFORMED_FUNCTION_CALL 结束、次数已用完不再重新生成，错误写共生成 1 次（对外不写次数）。原先按请求共用的重新生成次数加 1，
// 写成 3 次，既不是这个账号的次数（1 次），也不是全请求的发送次数（4 次）
func TestAbnormalFinishGenerationsPerAttempt(t *testing.T) {
	const unavailable = `[[],[14,"The service is currently unavailable."]]`
	var mu sync.Mutex
	failing := ""
	transport := newScriptedTransport(func(accountID string, _ int, _ string) *aistudio.RPCResponse {
		mu.Lock()
		if failing == "" {
			failing = accountID
		}
		body := regenTestMalformed
		if accountID == failing {
			body = unavailable
		}
		mu.Unlock()
		return playgroundResponse(http.StatusOK, io.NopCloser(strings.NewReader(body)))
	})
	service := regenerationTestService(t, transport, switchTestIDs(2), testRuntimeModel)
	service.repeatNonce.Store(true)
	service.setAbnormalFinishError(true)
	request := abnormalTestRequest("req-abnormal-per-attempt")
	diag := newGenerationDiagnostics(request, "未发送")
	if !service.maybeAddPromptNonce(&request, diag, false) {
		t.Fatal("应加上随机后缀")
	}
	requestCtx, cancel := context.WithTimeout(service.lifecycle, 20*time.Second)
	defer cancel()
	events := make(chan aistudio.Event, 8)
	go service.generateWithRetry(service.lifecycle, requestCtx, cancel, time.Now(), request, "", events, diag)
	var last aistudio.Event
	for event := range events {
		last = event
	}
	_, order := transport.sent()
	if len(order) != 4 || order[0] != order[1] || order[1] != order[2] || order[3] == order[0] {
		t.Fatalf("应在第一个账号共发送 3 次后换号再发 1 次，实际发送顺序 %v", order)
	}
	var abnormal *aistudio.AbnormalFinishError
	if last.Kind != aistudio.EventError || !errors.As(last.Err, &abnormal) || abnormal.Generations != 1 {
		t.Fatalf("最后事件 = %+v，期望只计第二个账号的 1 次", last)
	}
	if message := abnormal.PublicMessage(); strings.Contains(message, "tried") {
		t.Fatalf("只生成一次时不写次数: %q", message)
	}
}

// TestAbnormalFinishStillVerifiesModelAccess 异常结束换成错误时上游已正常生成了回复：账号在这个模型上的资格照旧记为已验证
// （forwardEvents 的 finished 在换成错误事件之前算出；挪到之后或按替换后的事件判断时这里不再验证）
func TestAbnormalFinishStillVerifiesModelAccess(t *testing.T) {
	upstream := &scriptedEventsService{stallingService: newStallingService(), events: abnormalFinishEvents()}
	service, pool := firstEventTestService(t, upstream, 0, "alice@example.com")
	service.setAbnormalFinishError(true)
	account, err := pool.Account("alice@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(account.RuntimePath); err == nil && strings.Contains(string(data), `"`+testRuntimeModel+`"`) {
		t.Fatalf("前提：请求之前不应已有这个模型的资格: %s", data)
	}
	if got := eventKinds(runScriptedRequest(t, service, abnormalTestRequest("req-abnormal-verify"))); got != "error" {
		t.Fatalf("应换成错误: %s", got)
	}
	pool.WaitBackground()
	if access := diskModelAccess(t, account.RuntimePath, testRuntimeModel); access.State != aistudio.ModelAccessVerified {
		t.Fatalf("异常结束换成错误时模型资格应照旧记为已验证: %+v", access)
	}
	// 对照：上游以错误结束（没有结束事件）时不验证
	upstream.events = []aistudio.Event{{Kind: aistudio.EventError, Err: errors.New("upstream broke")}}
	other, otherPool := firstEventTestService(t, upstream, 0, "bob@example.com")
	other.setAbnormalFinishError(true)
	runScriptedRequest(t, other, abnormalTestRequest("req-abnormal-verify-error"))
	otherPool.WaitBackground()
	otherAccount, _ := otherPool.Account("bob@example.com")
	if data, err := os.ReadFile(otherAccount.RuntimePath); err == nil && strings.Contains(string(data), `"verified"`) {
		t.Fatalf("以错误结束时不应验证: %s", data)
	}
}

// TestAbnormalFinishErrorHotConfig ABNORMAL_FINISH_ERROR 保存后立即生效、不提示需要重启：写进 .env，读取时总是返回，旧版页面不带该字段时沿用现值；
// 是热更新字段（liveFieldsEqual 为假、requiresRebuild 为假），生效配置比较包含它
func TestAbnormalFinishErrorHotConfig(t *testing.T) {
	clearAppConfigEnv(t)
	t.Setenv("ABNORMAL_FINISH_ERROR", "")
	os.Unsetenv("ABNORMAL_FINISH_ERROR")
	upstream := &scriptedEventsService{stallingService: newStallingService(), events: abnormalFinishEvents()}
	service, pool := runningGenerateService(t, upstream, context.Background(), time.Minute, "alice@example.com")
	manager, path := newMediaHotManager(t)
	generation := manager.current
	admin := generation.admin.(*runtimeAdmin)
	admin.service, admin.pool, generation.service = service, pool, service
	ctx := context.Background()

	if value := runtimeConfigDTO(config.Default()); value.AbnormalFinishError == nil || *value.AbnormalFinishError {
		t.Fatalf("读取的默认值 = %v", value.AbnormalFinishError)
	}
	value := runtimeConfigDTO(config.Default())
	on := true
	value.AbnormalFinishError = &on
	updated, err := manager.UpdateRuntimeConfig(ctx, value)
	if err != nil {
		t.Fatal(err)
	}
	if updated.AbnormalFinishError == nil || !*updated.AbnormalFinishError || updated.ServiceRestartRequired || updated.ManagementRestartRequired {
		t.Fatalf("保存后返回 = %v，需要重启 = %v/%v", updated.AbnormalFinishError, updated.ServiceRestartRequired, updated.ManagementRestartRequired)
	}
	if !service.abnormalFinishError.Load() {
		t.Fatal("保存后应立即生效")
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "ABNORMAL_FINISH_ERROR=true\n") {
		t.Fatalf(".env 缺少 ABNORMAL_FINISH_ERROR=true:\n%s", data)
	}
	if !hasLogContaining(manager.requests, "异常结束按错误返回设置已更新并立即生效", "开启=true") {
		t.Fatal("缺少生效日志")
	}
	if got := eventKinds(runScriptedRequest(t, service, abnormalTestRequest("req-abnormal-hot"))); got != "error" {
		t.Fatalf("保存后下一个请求应按打开处理: %s", got)
	}
	read, err := manager.RuntimeConfig(ctx)
	if err != nil || read.AbnormalFinishError == nil || !*read.AbnormalFinishError || read.ServiceRestartRequired {
		t.Fatalf("读取配置: %v %v", read.AbnormalFinishError, err)
	}
	legacy := read
	legacy.AbnormalFinishError = nil
	if _, err := manager.UpdateRuntimeConfig(ctx, legacy); err != nil || !service.abnormalFinishError.Load() {
		t.Fatalf("旧版页面保存后应沿用现值（打开）: %v", err)
	}
	off := false
	read.AbnormalFinishError = &off
	if _, err := manager.UpdateRuntimeConfig(ctx, read); err != nil || service.abnormalFinishError.Load() {
		t.Fatalf("关闭应立即生效: %v", err)
	}
	if got := eventKinds(runScriptedRequest(t, service, abnormalTestRequest("req-abnormal-hot-off"))); got != "usage,finish" {
		t.Fatalf("关闭后下一个请求应原样结束: %s", got)
	}

	active := config.Default()
	changed := active
	changed.AbnormalFinishError = true
	if liveFieldsEqual(changed, active) || requiresRebuild(changed, active) {
		t.Fatal("ABNORMAL_FINISH_ERROR 变化应按热更新处理")
	}
	dto := runtimeConfigDTO(changed)
	if sameDataConfig(dto, active, dataConfigOverrides{}) || !sameDataConfig(dto, changed, dataConfigOverrides{}) {
		t.Fatal("生效配置比较应包含 ABNORMAL_FINISH_ERROR")
	}
	dto.AbnormalFinishError = nil
	if !sameDataConfig(dto, active, dataConfigOverrides{}) {
		t.Fatal("旧版页面不带这个字段时按生效值比较")
	}
}

// TestAbnormalFinishErrorAccessLog 经公开 API：开关打开时按 HTTP 502 返回，访问日志（请求日志与用量账本的来源）照旧记下这次的用量与结束原因，
// 错误写明异常结束
func TestAbnormalFinishErrorAccessLog(t *testing.T) {
	upstream := &scriptedEventsService{stallingService: newStallingService(), events: abnormalFinishEvents()}
	service, _ := runningGenerateService(t, upstream, context.Background(), time.Minute, "alice@example.com")
	service.setAbnormalFinishError(true)
	status, entry := serveAccessLog(t, service)
	if status != http.StatusBadGateway {
		t.Fatalf("status = %d，期望 502", status)
	}
	if entry.Usage == nil || entry.Usage.TotalTokens != 127 || entry.FinishReason != "malformed_function_call" ||
		!strings.Contains(entry.Error, "异常结束生成且没有输出") {
		t.Fatalf("访问日志 usage=%+v finish=%q error=%q", entry.Usage, entry.FinishReason, entry.Error)
	}
	if row := requestRow(entry, time.Now()); row.TotalTokens != 127 || row.State != "failed" {
		t.Fatalf("用量账本 = %+v，期望照旧记下用量、按失败记", row)
	}
}

// TestAbnormalFinishRegenerationHoldsStreamStart ABNORMAL_FINISH_ERROR 打开时，首个事件之前的每次同账号重新生成让 API 层写出流式响应头前的
// 等待从重新生成时刻起重新计（aistudio.StreamStartHold.RestartWait，合计仍受 API 层 50 秒上限约束）：三次生成加起来超过 10 秒时，判定仍在
// 提交响应头之前完成。开关关闭时不延长（与原先相同）
func TestAbnormalFinishRegenerationHoldsStreamStart(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		t.Run(fmt.Sprintf("开关=%t", enabled), func(t *testing.T) {
			transport := newScriptedTransport(func(string, int, string) *aistudio.RPCResponse {
				return playgroundResponse(http.StatusOK, &delayedBody{delay: 300 * time.Millisecond, data: strings.NewReader(regenTestMalformed)})
			})
			service := regenerationTestService(t, transport, switchTestIDs(1), testRuntimeModel)
			service.repeatNonce.Store(true)
			service.setAbnormalFinishError(enabled)
			request := aistudio.GenerateRequest{
				ID: "req-abnormal-hold", Model: testRuntimeModel,
				Contents: []aistudio.Content{{Role: aistudio.RoleUser, Parts: []aistudio.Part{{Text: "查一下天气"}}}},
			}
			diag := newGenerationDiagnostics(request, "未发送")
			if !service.maybeAddPromptNonce(&request, diag, false) {
				t.Fatal("应加上随机后缀")
			}
			hold := aistudio.NewStreamStartHold()
			requestCtx, cancel := context.WithTimeout(aistudio.WithStreamStartHold(service.lifecycle, hold), 20*time.Second)
			defer cancel()
			startedAt := time.Now()
			events := make(chan aistudio.Event, 8)
			go service.generateWithRetry(service.lifecycle, requestCtx, cancel, startedAt, request, "", events, diag)
			var last aistudio.Event
			for event := range events {
				last = event
			}
			if _, order := transport.sent(); len(order) != 3 {
				t.Fatalf("应在同一账号共生成 3 次，实际发送顺序 %v", order)
			}
			const wait = 10 * time.Second
			limit := hold.Limit(startedAt, wait)
			if !enabled {
				if limit != wait || last.Kind != aistudio.EventFinish {
					t.Fatalf("开关关闭：写出响应头前的等待 = %s、最后事件 = %s，期望不延长、原样结束", limit, last.Kind)
				}
				return
			}
			var abnormal *aistudio.AbnormalFinishError
			if last.Kind != aistudio.EventError || !errors.As(last.Err, &abnormal) || abnormal.Generations != 3 {
				t.Fatalf("最后事件 = %+v，期望共生成 3 次的异常结束错误", last)
			}
			if limit < wait+600*time.Millisecond {
				t.Fatalf("写出响应头前的等待 = %s，期望从第二次重新生成（约 600ms 后）起再等 %s", limit, wait)
			}
		})
	}
}

// TestAbnormalFinishSlowStreamIsBadGateway 经公开 API 的流式请求：三次生成共约 10.5 秒（超过写出流式响应头前默认的 10 秒等待）才判定
// 异常结束，仍按 HTTP 502 返回（原先第 10 秒先提交 200 与心跳，判定之后只能作为流内错误，中转不会重试）
func TestAbnormalFinishSlowStreamIsBadGateway(t *testing.T) {
	transport := newScriptedTransport(func(string, int, string) *aistudio.RPCResponse {
		return playgroundResponse(http.StatusOK, &delayedBody{delay: 3500 * time.Millisecond, data: strings.NewReader(regenTestMalformed)})
	})
	service := toolRegenerationTestService(t, transport, switchTestIDs(1)...)
	service.repeatNonce.Store(true)
	service.setAbnormalFinishError(true)
	handler := api.NewHandler(service, api.Config{APIKey: "sk-test"})
	body := `{"model":"` + testRuntimeModel + `","stream":true,"messages":[{"role":"user","content":"查一下天气"}]}`
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer sk-test")
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	startedAt := time.Now()
	handler.ServeHTTP(recorder, request)
	if elapsed := time.Since(startedAt); elapsed < 10*time.Second {
		t.Fatalf("用时 %s，期望三次生成共超过 10 秒（status=%d body=%s）", elapsed, recorder.Code, recorder.Body.String())
	}
	if _, order := transport.sent(); len(order) != 3 {
		t.Fatalf("应在同一账号共生成 3 次，实际发送顺序 %v", order)
	}
	if recorder.Code != http.StatusBadGateway || !strings.Contains(recorder.Body.String(), "MALFORMED_FUNCTION_CALL") ||
		strings.Contains(recorder.Body.String(), "data:") {
		t.Fatalf("status = %d，响应 %s，期望在提交响应头之前按 502 返回", recorder.Code, recorder.Body.String())
	}
}
