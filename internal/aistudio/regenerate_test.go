package aistudio

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// Playground GenerateContent 的流帧：正文 "hi"、正常结束、函数调用格式错误结束
const (
	regenTextFrame      = `[[[[[[null,"hi"]],"model"]]]]`
	regenStopFrame      = `[[[null,1]]]`
	regenMalformedFrame = `[[[null,10,null,"Malformed function call: print(default_api.lookup(id=))"]]]`
)

// regenStream 拼出 GenerateContent 响应：frames 为流帧列表，trailer 非空时作为流尾 google.rpc 状态
func regenStream(trailer string, frames ...string) string {
	body := "[[" + strings.Join(frames, ",") + "]"
	if trailer != "" {
		body += "," + trailer
	}
	return body + "]"
}

// interruptedBody 先交出一段响应，再以连接被重置结束
type interruptedBody struct {
	data   *strings.Reader
	closed bool
}

func (body *interruptedBody) Read(buffer []byte) (int, error) {
	if body.data.Len() == 0 {
		return 0, syscall.ECONNRESET
	}
	return body.data.Read(buffer)
}

func (body *interruptedBody) Close() error {
	body.closed = true
	return nil
}

// regenUpstream 按顺序返回预设的响应，记下每次发送的请求体
type regenUpstream struct {
	mu        sync.Mutex
	responses []func() (*RPCResponse, error)
	bodies    []string
	lease     *AccountLease
}

func (upstream *regenUpstream) DoProtected(_ context.Context, _ GenerateRequest, rpc RPCRequest) (*RPCResponse, error) {
	upstream.mu.Lock()
	defer upstream.mu.Unlock()
	upstream.bodies = append(upstream.bodies, string(rpc.Body))
	if upstream.lease != nil {
		// 与 WorkerProtectedTransport.beginGenerationSend 一样每次发送记一次
		upstream.lease.RecordGenerationSent()
	}
	index := len(upstream.bodies) - 1
	if index >= len(upstream.responses) {
		index = len(upstream.responses) - 1
	}
	return upstream.responses[index]()
}

func (upstream *regenUpstream) calls() int {
	upstream.mu.Lock()
	defer upstream.mu.Unlock()
	return len(upstream.bodies)
}

func streamResponse(body string) func() (*RPCResponse, error) {
	return func() (*RPCResponse, error) {
		return &RPCResponse{
			StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {JSONProtobufContentType}},
			Body: io.NopCloser(strings.NewReader(body)),
		}, nil
	}
}

func interruptedResponse(body string) func() (*RPCResponse, error) {
	return func() (*RPCResponse, error) {
		return &RPCResponse{
			StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {JSONProtobufContentType}},
			Body: &interruptedBody{data: strings.NewReader(body)},
		}, nil
	}
}

func errorResponse(status int, body string) func() (*RPCResponse, error) {
	return func() (*RPCResponse, error) {
		return &RPCResponse{
			StatusCode: status, Header: http.Header{"Content-Type": {JSONProtobufContentType}},
			Body: io.NopCloser(strings.NewReader(body)),
		}, nil
	}
}

// regenClient 返回 Playground 模型 modelID 的 Client，上游按 responses 依次返回
func regenClient(t *testing.T, modelID string, upstream *regenUpstream) *Client {
	t.Helper()
	client, err := NewClient(ClientOptions{Transport: ceilingTransport{}, Protected: upstream})
	if err != nil {
		t.Fatal(err)
	}
	model := Model{ID: modelID, Methods: []string{"generateContent"}, OutputTokenLimit: 65536, Capabilities: map[string]bool{"chat_model": true}}
	entry := modelEntry{model: model, defaults: GenerationDefaults{MaxOutputTokens: 65536}}
	client.catalogs["alice@example.com"] = modelCatalog{models: []Model{model}, entries: map[string]modelEntry{modelID: entry}}
	return client
}

func regenRequest(model string, contents ...Content) GenerateRequest {
	if len(contents) == 0 {
		contents = []Content{{Role: RoleUser, Parts: []Part{{Text: "hello"}}}}
	}
	return GenerateRequest{ID: "req-regen", Model: model, AccountID: "alice@example.com", Contents: contents}
}

// collectEvents 读完事件流
func collectEvents(t *testing.T, events <-chan Event) []Event {
	t.Helper()
	var got []Event
	timeout := time.After(5 * time.Second)
	for {
		select {
		case event, ok := <-events:
			if !ok {
				return got
			}
			got = append(got, event)
		case <-timeout:
			t.Fatal("事件流没有关闭")
		}
	}
}

// noteRecorder 收集请求说明（app 层写进请求进度日志的那些）
type noteRecorder struct {
	mu    sync.Mutex
	notes []string
}

func (recorder *noteRecorder) context(ctx context.Context) context.Context {
	return ContextWithRequestNoteObserver(ctx, func(note string) {
		recorder.mu.Lock()
		recorder.notes = append(recorder.notes, note)
		recorder.mu.Unlock()
	})
}

func (recorder *noteRecorder) count(marker string) int {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	count := 0
	for _, note := range recorder.notes {
		if strings.Contains(note, marker) {
			count++
		}
	}
	return count
}

// variantMarker 为测试用换输入函数加在用户消息末尾的序号前缀
const variantMarker = " #variant-"

// variantContext 挂上测试用的换输入函数（模拟 app 层换随机后缀，见 app/duplicates.go 的 promptNonceVariant）：每次把最后一条用户消息
// 末尾的序号换成新的（第 n 次调用为 " #variant-n"）
func variantContext(ctx context.Context) context.Context {
	var calls atomic.Int32
	return ContextWithRegenerationVariant(ctx, func(request GenerateRequest) (GenerateRequest, bool) {
		sequence := calls.Add(1)
		contents := append([]Content(nil), request.Contents...)
		for index := len(contents) - 1; index >= 0; index-- {
			if contents[index].Role != RoleUser || len(contents[index].Parts) == 0 {
				continue
			}
			parts := append([]Part(nil), contents[index].Parts...)
			text := parts[len(parts)-1].Text
			if cut := strings.Index(text, variantMarker); cut >= 0 {
				text = text[:cut]
			}
			parts[len(parts)-1].Text = fmt.Sprintf("%s%s%d", text, variantMarker, sequence)
			contents[index].Parts = parts
			request.Contents = contents
			return request, true
		}
		return request, false
	})
}

func eventKinds(events []Event) []EventKind {
	kinds := make([]EventKind, 0, len(events))
	for _, event := range events {
		kinds = append(kinds, event.Kind)
	}
	return kinds
}

// TestPlaygroundTrailerStatus Playground 流尾 google.rpc 状态按状态码返回上游错误：8 为 429、13 为 500、3 为 400，
// 完成帧之后的流尾错误同样返回；状态码 0、空数组、null 与不是状态的形状照旧正常结束。原先一律报“流结束前没有完成帧”
func TestPlaygroundTrailerStatus(t *testing.T) {
	for _, test := range []struct {
		name   string
		body   string
		status int
		code   int64
	}{
		{name: "429", body: regenStream(`[8,"Resource has been exhausted (e.g. check quota)."]`), status: http.StatusTooManyRequests, code: 8},
		{name: "500", body: regenStream(`[13,"Internal error encountered."]`, regenTextFrame), status: http.StatusInternalServerError, code: 13},
		{name: "400", body: regenStream(`[3,"Request contains an invalid argument."]`, regenTextFrame, regenStopFrame), status: http.StatusBadRequest, code: 3},
	} {
		decoder := NewFrameDecoder()
		err := DecodeGenerateStream(strings.NewReader(test.body), decoder, func(Event) error { return nil })
		var rpcError *RPCError
		if !errors.As(err, &rpcError) || rpcError.StatusCode != test.status || rpcError.Code != test.code {
			t.Fatalf("%s：流尾错误 = %v，期望 HTTP %d、协议错误码 %d", test.name, err, test.status, test.code)
		}
		if StreamInterrupted(err) || strings.Contains(err.Error(), "流结束前没有完成帧") {
			t.Fatalf("%s：流尾错误不应再报成没有完成帧：%v", test.name, err)
		}
	}
	for _, trailer := range []string{`[0]`, `[]`, `null`, `"unknown"`, `[[1,2]]`} {
		decoder := NewFrameDecoder()
		if err := DecodeGenerateStream(strings.NewReader(regenStream(trailer, regenTextFrame, regenStopFrame)), decoder, func(Event) error { return nil }); err != nil {
			t.Fatalf("流尾 %s 应正常解码：%v", trailer, err)
		}
		if err := decoder.End(); err != nil {
			t.Fatalf("流尾 %s 应正常结束：%v", trailer, err)
		}
	}
	// 没有完成帧的正常结束仍是“流中断”（可重新生成）
	decoder := NewFrameDecoder()
	if err := DecodeGenerateStream(strings.NewReader(regenStream("", regenTextFrame)), decoder, func(Event) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := decoder.End(); !StreamInterrupted(err) {
		t.Fatalf("没有完成帧应判为流中断：%v", err)
	}
}

// TestGenerateTrailerQuotaSurfaces 经 Client.Generate：流尾 429 以 RPCError 交出（app 层据此冷却、换号），不重新生成
func TestGenerateTrailerQuotaSurfaces(t *testing.T) {
	upstream := &regenUpstream{responses: []func() (*RPCResponse, error){
		streamResponse(regenStream(`[8,"You exceeded your current quota."]`)),
		streamResponse(regenStream("", regenTextFrame, regenStopFrame)),
	}}
	events, err := regenClient(t, "test", upstream).Generate(context.Background(), regenRequest("test"))
	if err != nil {
		t.Fatal(err)
	}
	got := collectEvents(t, events)
	var rpcError *RPCError
	if len(got) != 1 || got[0].Kind != EventError || !errors.As(got[0].Err, &rpcError) || rpcError.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("事件 = %v，期望一个 429 错误事件", eventKinds(got))
	}
	if upstream.calls() != 1 {
		t.Fatalf("流尾 429 不应重新生成，发送了 %d 次", upstream.calls())
	}
}

// TestGenerateRegeneratesBeforeContent 客户端还没收到内容时，函数调用格式错误、流尾 5xx 与流中断都在同一账号重新生成，
// 客户端只看到最后一次的正文、用量与结束原因；每次重新生成写一行请求说明。因结束原因重新生成的那次换了输入（第二次的请求体
// 带新的后缀），流尾 5xx 与流中断原样重发（两次请求体相同）
func TestGenerateRegeneratesBeforeContent(t *testing.T) {
	for _, test := range []struct {
		name   string
		first  func() (*RPCResponse, error)
		reason string
		varied bool
	}{
		{name: "malformed", first: streamResponse(regenStream("", regenMalformedFrame)), reason: "结束原因=malformed_function_call", varied: true},
		{name: "unexpected", first: streamResponse(regenStream("", `[[[null,12]]]`)), reason: "结束原因=unexpected_tool_call", varied: true},
		{name: "trailer5xx", first: streamResponse(regenStream(`[14,"The service is currently unavailable."]`)), reason: "流尾错误=HTTP 503"},
		{name: "interrupted", first: interruptedResponse(`[[` + regenStopFrame[:5]), reason: "流中断"},
		{name: "missingFinish", first: streamResponse(regenStream("")), reason: "流中断"},
	} {
		upstream := &regenUpstream{responses: []func() (*RPCResponse, error){
			test.first, streamResponse(regenStream("", regenTextFrame, regenStopFrame)),
		}}
		notes := &noteRecorder{}
		events, err := regenClient(t, "test", upstream).Generate(variantContext(notes.context(context.Background())), regenRequest("test"))
		if err != nil {
			t.Fatalf("%s：%v", test.name, err)
		}
		got := collectEvents(t, events)
		kinds := eventKinds(got)
		if len(got) != 3 || kinds[0] != EventText || got[0].Text != "hi" || kinds[1] != EventUsage || kinds[2] != EventFinish || got[2].FinishReason != "stop" {
			t.Fatalf("%s：事件 = %v，期望重新生成后的 正文/用量/结束", test.name, kinds)
		}
		if upstream.calls() != 2 {
			t.Fatalf("%s：应发送 2 次（重新生成 1 次），实际 %d", test.name, upstream.calls())
		}
		input := "输入=原样重发"
		if test.varied {
			input = "输入=已换随机后缀"
		}
		if notes.count("同账号重新生成 | 第 1/2 次 | "+test.reason) != 1 || notes.count(input) != 1 {
			t.Fatalf("%s：请求说明 = %v，期望一行重新生成说明（%s，%s）", test.name, notes.notes, test.reason, input)
		}
		if strings.Contains(upstream.bodies[0], variantMarker) {
			t.Fatalf("%s：第一次发送不应换输入", test.name)
		}
		if varied := strings.Contains(upstream.bodies[1], variantMarker+"1"); varied != test.varied || !test.varied && upstream.bodies[1] != upstream.bodies[0] {
			t.Fatalf("%s：第二次发送换了输入=%v，期望 %v（原样重发时两次请求体应相同）", test.name, varied, test.varied)
		}
	}
}

// TestGenerateNoRegenerateAfterContent 已经交出正文后，流尾 5xx、流中断与函数调用格式错误都照旧结束，不重新生成
func TestGenerateNoRegenerateAfterContent(t *testing.T) {
	for _, test := range []struct {
		name  string
		first func() (*RPCResponse, error)
		last  EventKind
	}{
		{name: "trailer5xx", first: streamResponse(regenStream(`[13,"Internal error encountered."]`, regenTextFrame)), last: EventError},
		{name: "interrupted", first: interruptedResponse(`[[` + regenTextFrame), last: EventError},
		{name: "malformed", first: streamResponse(regenStream("", regenTextFrame, regenMalformedFrame)), last: EventFinish},
	} {
		upstream := &regenUpstream{responses: []func() (*RPCResponse, error){
			test.first, streamResponse(regenStream("", regenTextFrame, regenStopFrame)),
		}}
		events, err := regenClient(t, "test", upstream).Generate(context.Background(), regenRequest("test"))
		if err != nil {
			t.Fatal(err)
		}
		got := collectEvents(t, events)
		if len(got) < 2 || got[0].Kind != EventText || got[len(got)-1].Kind != test.last {
			t.Fatalf("%s：事件 = %v，期望正文之后以 %s 结束", test.name, eventKinds(got), test.last)
		}
		if upstream.calls() != 1 {
			t.Fatalf("%s：已经交出正文后不应重新生成，发送了 %d 次", test.name, upstream.calls())
		}
	}
	// 400 等其他错误不重新生成
	upstream := &regenUpstream{responses: []func() (*RPCResponse, error){
		streamResponse(regenStream(`[3,"Request contains an invalid argument."]`)),
	}}
	events, err := regenClient(t, "test", upstream).Generate(context.Background(), regenRequest("test"))
	if err != nil {
		t.Fatal(err)
	}
	if got := collectEvents(t, events); len(got) != 1 || got[0].Kind != EventError || upstream.calls() != 1 {
		t.Fatalf("流尾 400 不应重新生成：事件 %v，发送 %d 次", eventKinds(got), upstream.calls())
	}
}

// TestGenerateRegenerationLimit 每个请求最多重新生成 regenerationLimit 次，换号的各次尝试共用次数；用完后照旧交出上游结果
func TestGenerateRegenerationLimit(t *testing.T) {
	upstream := &regenUpstream{responses: []func() (*RPCResponse, error){streamResponse(regenStream("", regenMalformedFrame))}}
	notes := &noteRecorder{}
	budget := NewRegenerationBudget()
	ctx := variantContext(ContextWithRegenerationBudget(notes.context(context.Background()), budget))
	events, err := regenClient(t, "test", upstream).Generate(ctx, regenRequest("test"))
	if err != nil {
		t.Fatal(err)
	}
	got := collectEvents(t, events)
	if last := got[len(got)-1]; last.Kind != EventFinish || last.FinishReason != "malformed_function_call" {
		t.Fatalf("次数用完后应交出函数调用格式错误的结束事件，得到 %v", eventKinds(got))
	}
	// 每次重新生成各换一个输入：三次请求体的后缀互不相同
	if !strings.Contains(upstream.bodies[1], variantMarker+"1") || !strings.Contains(upstream.bodies[2], variantMarker+"2") ||
		strings.Contains(upstream.bodies[2], variantMarker+"1") {
		t.Fatal("每次重新生成应换一个新的输入")
	}
	if upstream.calls() != 1+regenerationLimit || budget.Used() != regenerationLimit {
		t.Fatalf("应发送 %d 次、用掉 %d 次，实际 %d 次、%d 次", 1+regenerationLimit, regenerationLimit, upstream.calls(), budget.Used())
	}
	if notes.count("重新生成次数已用完") != 1 || notes.count("同账号重新生成") != regenerationLimit {
		t.Fatalf("请求说明 = %v", notes.notes)
	}
	// 同一请求换号后的下一次尝试共用次数：不再重新生成
	second := &regenUpstream{responses: []func() (*RPCResponse, error){streamResponse(regenStream("", regenMalformedFrame))}}
	events, err = regenClient(t, "test", second).Generate(ctx, regenRequest("test"))
	if err != nil {
		t.Fatal(err)
	}
	collectEvents(t, events)
	if second.calls() != 1 {
		t.Fatalf("换号后的尝试应共用重新生成次数，发送了 %d 次", second.calls())
	}
}

// TestGenerateRegenerationRespectsAccountRate 重新生成计入每账号每分钟上限：名额用满时不再重新生成，已发出的都记在账户上
func TestGenerateRegenerationRespectsAccountRate(t *testing.T) {
	pool := newRateTestPool(t, time.Minute, 2, 2, indexTestAccount("alice@example.com", BenefitTierFree, indexTestCatalog()))
	lease := mustTryAcquire(t, pool, rateTestSelection)
	defer func() { _ = lease.Release() }()
	upstream := &regenUpstream{lease: lease, responses: []func() (*RPCResponse, error){streamResponse(regenStream("", regenMalformedFrame))}}
	notes := &noteRecorder{}
	ctx := variantContext(ContextWithAccountLease(notes.context(context.Background()), lease))
	events, err := regenClient(t, indexTestModel, upstream).Generate(ctx, regenRequest(indexTestModel))
	if err != nil {
		t.Fatal(err)
	}
	collectEvents(t, events)
	// 上限 2：第一次发送 + 一次重新生成后窗口已满，第二次重新生成被拒
	if upstream.calls() != 2 {
		t.Fatalf("每分钟上限 2 时应只发送 2 次，实际 %d", upstream.calls())
	}
	if live, _, pending, _ := rateSnapshot(pool, "alice@example.com"); live != 2 || pending != 0 {
		t.Fatalf("窗口内发送 %d、预占 %d，期望 2 与 0", live, pending)
	}
	// 说明写明是每分钟名额满了（不是冷却或出口限速）
	if notes.count("客户端还没收到内容，但每分钟名额 2/2（已发出 2、预占 0，ACCOUNT_RPM_LIMIT），不重新生成") != 1 {
		t.Fatalf("请求说明 = %v，期望一行名额已满不重新生成", notes.notes)
	}
}

// TestNativePrefillFallback 名单内的模型以模型轮结尾原样发送；上游返回 400 “Requests ending with a model turn are not supported”
// 时改回在末尾补用户消息重试一次、写一行请求说明，并记住该模型与通道，之后同模型直接补用户消息
func TestNativePrefillFallback(t *testing.T) {
	const model = "gemma-prefill-test"
	prefillRejections.Delete(prefillRejectionKey(ChannelPlayground, model))
	t.Cleanup(func() { prefillRejections.Delete(prefillRejectionKey(ChannelPlayground, model)) })
	contents := []Content{
		{Role: RoleUser, Parts: []Part{{Text: "写一首诗"}}},
		{Role: RoleAssistant, Parts: []Part{{Text: "春眠不觉晓，"}}},
	}
	if !NativePrefillModel(model) || !NativePrefillModel("models/gemini-3-flash-preview") || NativePrefillModel("gemini-omni-1.1-flash") ||
		NativePrefillModel("gemini-4-argon") {
		t.Fatal("原生续写名单判断不对")
	}
	upstream := &regenUpstream{responses: []func() (*RPCResponse, error){
		errorResponse(http.StatusBadRequest, `[3,"Requests ending with a model turn are not supported."]`),
		streamResponse(regenStream("", regenTextFrame, regenStopFrame)),
	}}
	notes := &noteRecorder{}
	request := regenRequest(model, contents...)
	request.PrefillTail = true
	client := regenClient(t, model, upstream)
	events, err := client.Generate(notes.context(context.Background()), request)
	if err != nil {
		t.Fatalf("原生续写被拒后应改回补用户消息重试：%v", err)
	}
	if got := collectEvents(t, events); len(got) != 3 || got[0].Text != "hi" {
		t.Fatalf("事件 = %v", eventKinds(got))
	}
	if upstream.calls() != 2 || strings.Contains(upstream.bodies[0], "Continue exactly") || !strings.Contains(upstream.bodies[1], "Continue exactly") {
		t.Fatalf("第一次应原样发送、第二次补用户消息：%d 次", upstream.calls())
	}
	if notes.count("上游不接受以模型轮结尾的请求，改回在末尾补用户消息重试一次") != 1 {
		t.Fatalf("请求说明 = %v", notes.notes)
	}
	// 记住之后同模型同通道直接补用户消息，只发送一次
	again := &regenUpstream{responses: []func() (*RPCResponse, error){streamResponse(regenStream("", regenTextFrame, regenStopFrame))}}
	request = regenRequest(model, contents...)
	request.PrefillTail = true
	events, err = regenClient(t, model, again).Generate(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	collectEvents(t, events)
	if again.calls() != 1 || !strings.Contains(again.bodies[0], "Continue exactly") {
		t.Fatalf("近期被拒过的模型应直接补用户消息：%d 次", again.calls())
	}
}

// TestNativePrefillSentAsIs 名单内的模型以模型轮结尾时原样发送，上游接受时不补用户消息、只发送一次
func TestNativePrefillSentAsIs(t *testing.T) {
	const model = "gemma-prefill-accepted"
	upstream := &regenUpstream{responses: []func() (*RPCResponse, error){streamResponse(regenStream("", regenTextFrame, regenStopFrame))}}
	request := regenRequest(model,
		Content{Role: RoleUser, Parts: []Part{{Text: "写一首诗"}}},
		Content{Role: RoleAssistant, Parts: []Part{{Text: "春眠不觉晓，"}}},
	)
	request.PrefillTail = true
	events, err := regenClient(t, model, upstream).Generate(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	collectEvents(t, events)
	if upstream.calls() != 1 || strings.Contains(upstream.bodies[0], "Continue exactly") || !strings.Contains(upstream.bodies[0], "春眠不觉晓") {
		t.Fatalf("应原样以模型轮结尾发送一次：%d 次", upstream.calls())
	}
}

// TestStreamInterruptedOnlyTransport 只有真正的传输中断算流中断：没有完成帧、响应在 JSON 结构中途结束（io.ErrUnexpectedEOF、io.EOF）；
// 上游字段类型变了（json.UnmarshalTypeError）、函数调用缺少名称、JSON 语法错误这类确定性的格式不符不算
func TestStreamInterruptedOnlyTransport(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
		want bool
	}{
		{name: "没有完成帧", err: NewFrameDecoder().End(), want: true},
		{name: "结构中途结束", err: &ProtocolEvidenceError{Method: "GenerateContent", Path: "$[0][0]", Detail: "unexpected EOF", Err: io.ErrUnexpectedEOF}, want: true},
		{name: "读到 EOF", err: &ProtocolEvidenceError{Method: "GenerateContent", Path: "$[0]", Detail: "EOF", Err: io.EOF}, want: true},
		{name: "字段类型不符", err: &ProtocolEvidenceError{Method: buildProxyStreamedMethod, Path: "$chunk", Detail: "type", Err: &json.UnmarshalTypeError{Value: "number", Type: reflect.TypeOf("")}}},
		{name: "函数调用缺少名称", err: &ProtocolEvidenceError{Method: "GenerateContent", Path: "$[10]", Detail: "function call 缺少名称", Err: validateFunctionCall(&FunctionCall{})}},
		{name: "语法错误", err: &ProtocolEvidenceError{Method: "GenerateContent", Path: "$", Detail: "syntax", Err: &json.SyntaxError{}}},
		{name: "取消", err: &ProtocolEvidenceError{Method: "GenerateContent", Path: "$", Detail: "canceled", Err: context.Canceled}},
		{name: "无 Err", err: &ProtocolEvidenceError{Method: "GenerateContent", Path: "$", Detail: "根值不是数组"}},
	} {
		if got := StreamInterrupted(test.err); got != test.want {
			t.Fatalf("%s：StreamInterrupted = %v，期望 %v", test.name, got, test.want)
		}
	}
}

// TestGenerateNoRegenerateOnDeterministicProtocolError 函数调用缺少名称这类确定性的协议错误不在同账号重新生成（重发只会得到同样的结果），
// 原样交出；流尾 504（DEADLINE_EXCEEDED）也不在同账号重来，交给 app 层换号
func TestGenerateNoRegenerateOnDeterministicProtocolError(t *testing.T) {
	for _, test := range []struct {
		name  string
		first func() (*RPCResponse, error)
		check func(error) bool
	}{
		{
			name:  "函数调用缺少名称",
			first: streamResponse(regenStream("", `[[[[[[null,null,null,null,null,null,null,null,null,null,["",null]]],"model"]]]]`)),
			check: func(err error) bool { return strings.Contains(err.Error(), "function call 缺少名称") },
		},
		{
			name:  "流尾 504",
			first: streamResponse(regenStream(`[4,"Deadline expired before operation could complete."]`)),
			check: func(err error) bool {
				var rpcError *RPCError
				return errors.As(err, &rpcError) && rpcError.StatusCode == http.StatusGatewayTimeout
			},
		},
	} {
		upstream := &regenUpstream{responses: []func() (*RPCResponse, error){
			test.first, streamResponse(regenStream("", regenTextFrame, regenStopFrame)),
		}}
		notes := &noteRecorder{}
		events, err := regenClient(t, "test", upstream).Generate(notes.context(context.Background()), regenRequest("test"))
		if err != nil {
			t.Fatal(err)
		}
		got := collectEvents(t, events)
		if len(got) != 1 || got[0].Kind != EventError || !test.check(got[0].Err) {
			t.Fatalf("%s：事件 = %v，期望原样交出这个错误", test.name, eventKinds(got))
		}
		if upstream.calls() != 1 || notes.count("重新生成") != 0 {
			t.Fatalf("%s：不应重新生成，发送了 %d 次，请求说明 = %v", test.name, upstream.calls(), notes.notes)
		}
	}
}

// TestGenerateRegenerationNoteDetail 重新生成的说明附上被放弃那次的耗时与原文（结束说明、流尾错误说明），app 层只给最终交出的
// 结束事件写上游结束说明，前几次的原文只在这里
func TestGenerateRegenerationNoteDetail(t *testing.T) {
	upstream := &regenUpstream{responses: []func() (*RPCResponse, error){
		streamResponse(regenStream("", regenMalformedFrame)),
		streamResponse(regenStream(`[13,"Internal error encountered."]`)),
		streamResponse(regenStream("", regenTextFrame, regenStopFrame)),
	}}
	notes := &noteRecorder{}
	events, err := regenClient(t, "test", upstream).Generate(variantContext(notes.context(context.Background())), regenRequest("test"))
	if err != nil {
		t.Fatal(err)
	}
	collectEvents(t, events)
	if notes.count("第 1/2 次 | 结束原因=malformed_function_call | 本次耗时=") != 1 ||
		notes.count("上游说明=Malformed function call: print(default_api.lookup(id=))") != 1 ||
		notes.count("第 2/2 次 | 流尾错误=HTTP 500 | 本次耗时=") != 1 || notes.count("上游说明=Internal error encountered.") != 1 {
		t.Fatalf("请求说明 = %v", notes.notes)
	}
	// 过长的原文截断
	long := strings.Repeat("调", regenerationNoteLimit+50)
	if detail := regenerationDetail(nil, &Event{FinishMessage: long}, time.Second); !strings.HasSuffix(detail, "…") ||
		!strings.HasPrefix(detail, "本次耗时=1s | 上游说明=") || strings.Count(detail, "调") != regenerationNoteLimit {
		t.Fatalf("截断后的说明 = %q", detail)
	}
}

// TestGenerateRegenerationResetsUsage 重新生成时清空被放弃那次的用量、结束事件与输出：第一次带用量帧并以函数调用格式错误结束，
// 第二次没有用量帧，交出的用量按第二次本地估算，不是第一次的
func TestGenerateRegenerationResetsUsage(t *testing.T) {
	malformedWithUsage := `[[[null,10,null,"Malformed function call: x(=)"]],null,[777,1,778]]`
	upstream := &regenUpstream{responses: []func() (*RPCResponse, error){
		streamResponse(regenStream("", malformedWithUsage)),
		streamResponse(regenStream("", regenTextFrame, regenStopFrame)),
	}}
	events, err := regenClient(t, "test", upstream).Generate(variantContext(context.Background()), regenRequest("test"))
	if err != nil {
		t.Fatal(err)
	}
	got := collectEvents(t, events)
	if upstream.calls() != 2 || len(got) != 3 || got[1].Kind != EventUsage || got[2].FinishReason != "stop" {
		t.Fatalf("事件 = %v，发送 %d 次", eventKinds(got), upstream.calls())
	}
	if usage := got[1].Usage; usage == nil || usage.InputTokens == 777 || usage.TotalTokens == 778 || usage.OutputTokens <= 0 {
		t.Fatalf("交出的用量 = %+v，不应是被放弃那次的", got[1].Usage)
	}
}

// TestGenerateRegenerationRespectsDeadline 三次生成共用首事件前期限：剩余期限不到本次耗时的 1.2 倍时不重新生成，按这次的结果结束；
// 期限充裕时照常重新生成
func TestGenerateRegenerationRespectsDeadline(t *testing.T) {
	slowMalformed := func(delay time.Duration) func() (*RPCResponse, error) {
		return func() (*RPCResponse, error) {
			time.Sleep(delay)
			return streamResponse(regenStream("", regenMalformedFrame))()
		}
	}
	upstream := &regenUpstream{responses: []func() (*RPCResponse, error){
		slowMalformed(600 * time.Millisecond), streamResponse(regenStream("", regenTextFrame, regenStopFrame)),
	}}
	notes := &noteRecorder{}
	ctx, cancel := context.WithTimeout(variantContext(notes.context(context.Background())), time.Second)
	defer cancel()
	events, err := regenClient(t, "test", upstream).Generate(ctx, regenRequest("test"))
	if err != nil {
		t.Fatal(err)
	}
	got := collectEvents(t, events)
	if last := got[len(got)-1]; last.Kind != EventFinish || last.FinishReason != "malformed_function_call" {
		t.Fatalf("剩余期限不足时应按这次的结果结束，得到 %v", eventKinds(got))
	}
	if upstream.calls() != 1 || notes.count("剩余期限不足（不到本次耗时的 1.2 倍），不重新生成 | 剩余=") != 1 || notes.count("| 本次耗时=") != 1 {
		t.Fatalf("剩余期限不足时只应发送 1 次：%d 次，请求说明 = %v", upstream.calls(), notes.notes)
	}
	// 期限充裕：照常重新生成
	roomy := &regenUpstream{responses: []func() (*RPCResponse, error){
		slowMalformed(100 * time.Millisecond), streamResponse(regenStream("", regenTextFrame, regenStopFrame)),
	}}
	ctx, cancel = context.WithTimeout(variantContext(context.Background()), 20*time.Second)
	defer cancel()
	events, err = regenClient(t, "test", roomy).Generate(ctx, regenRequest("test"))
	if err != nil {
		t.Fatal(err)
	}
	if got := collectEvents(t, events); roomy.calls() != 2 || got[len(got)-1].FinishReason != "stop" {
		t.Fatalf("期限充裕时应重新生成：%d 次，事件 %v", roomy.calls(), eventKinds(got))
	}
}

// TestNativePrefillFallbackFromTrailer 原生续写在流里被拒（流尾 [3,"Requests ending with a model turn are not supported"]）：
// 改回补用户消息重发一次，第二次发送带续写提示，共发送 2 次，不占重新生成次数
func TestNativePrefillFallbackFromTrailer(t *testing.T) {
	const model = "gemma-prefill-trailer"
	prefillRejections.Delete(prefillRejectionKey(ChannelPlayground, model))
	t.Cleanup(func() { prefillRejections.Delete(prefillRejectionKey(ChannelPlayground, model)) })
	upstream := &regenUpstream{responses: []func() (*RPCResponse, error){
		streamResponse(regenStream(`[3,"Requests ending with a model turn are not supported."]`)),
		streamResponse(regenStream("", regenTextFrame, regenStopFrame)),
	}}
	request := regenRequest(model,
		Content{Role: RoleUser, Parts: []Part{{Text: "写一首诗"}}},
		Content{Role: RoleAssistant, Parts: []Part{{Text: "春眠不觉晓，"}}},
	)
	request.PrefillTail = true
	notes := &noteRecorder{}
	budget := NewRegenerationBudget()
	ctx := ContextWithRegenerationBudget(notes.context(context.Background()), budget)
	events, err := regenClient(t, model, upstream).Generate(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if got := collectEvents(t, events); len(got) != 3 || got[0].Text != "hi" || got[2].FinishReason != "stop" {
		t.Fatalf("事件 = %v", eventKinds(got))
	}
	if upstream.calls() != 2 || strings.Contains(upstream.bodies[0], "Continue exactly") || !strings.Contains(upstream.bodies[1], "Continue exactly") {
		t.Fatalf("第一次应原样发送、第二次补用户消息：%d 次", upstream.calls())
	}
	if budget.Used() != 0 || notes.count("改回在末尾补用户消息重试一次") != 1 {
		t.Fatalf("续写回退不应占重新生成次数（已用 %d），请求说明 = %v", budget.Used(), notes.notes)
	}
}

// TestNativePrefillFallbackRespectsAccountRate 改回补用户消息的重发同样占每分钟名额：名额已满时不在本次尝试重发，交出
// *PrefillRetryError（Unwrap 为上游的 400，app 层据此换号再试，不把 400 交给客户端），写一行说明（写明是每分钟名额满了）；
// 流尾 400 的情况同样把它作为首个事件交出
func TestNativePrefillFallbackRespectsAccountRate(t *testing.T) {
	const model = "gemma-prefill-rate"
	prefillRejections.Delete(prefillRejectionKey(ChannelPlayground, model))
	t.Cleanup(func() { prefillRejections.Delete(prefillRejectionKey(ChannelPlayground, model)) })
	for _, trailer := range []bool{false, true} {
		prefillRejections.Delete(prefillRejectionKey(ChannelPlayground, model))
		pool := newRateTestPool(t, time.Minute, 1, 2, indexTestAccount("alice@example.com", BenefitTierFree, indexTestCatalog()))
		lease := mustTryAcquire(t, pool, rateTestSelection)
		first := errorResponse(http.StatusBadRequest, `[3,"Requests ending with a model turn are not supported."]`)
		if trailer {
			first = streamResponse(regenStream(`[3,"Requests ending with a model turn are not supported."]`))
		}
		upstream := &regenUpstream{lease: lease, responses: []func() (*RPCResponse, error){
			first, streamResponse(regenStream("", regenTextFrame, regenStopFrame)),
		}}
		request := regenRequest(model,
			Content{Role: RoleUser, Parts: []Part{{Text: "写一首诗"}}},
			Content{Role: RoleAssistant, Parts: []Part{{Text: "春眠不觉晓，"}}},
		)
		request.PrefillTail = true
		notes := &noteRecorder{}
		events, err := regenClient(t, model, upstream).Generate(ContextWithAccountLease(notes.context(context.Background()), lease), request)
		if trailer {
			if err != nil {
				t.Fatal(err)
			}
			got := collectEvents(t, events)
			if len(got) != 1 || got[0].Kind != EventError {
				t.Fatalf("流尾 400：事件 = %v，期望一个错误事件", eventKinds(got))
			}
			err = got[0].Err
		}
		var retry *PrefillRetryError
		var rpcError *RPCError
		if !errors.As(err, &retry) || !errors.As(err, &rpcError) || rpcError.StatusCode != http.StatusBadRequest ||
			!strings.Contains(retry.Denied, "每分钟名额 1/1") || err.Error() != rpcError.Error() {
			t.Fatalf("流尾=%v：名额已满时应交出可换号的 PrefillRetryError（Unwrap 为上游的 400）：%v", trailer, err)
		}
		if upstream.calls() != 1 || notes.count("上游不接受以模型轮结尾的请求，但每分钟名额 1/1（已发出 1、预占 0，ACCOUNT_RPM_LIMIT），不在本次尝试重发，换号再试") != 1 {
			t.Fatalf("流尾=%v：名额已满时不应重发：%d 次，请求说明 = %v", trailer, upstream.calls(), notes.notes)
		}
		if live, _, pending, _ := rateSnapshot(pool, "alice@example.com"); live != 1 || pending != 0 {
			t.Fatalf("流尾=%v：窗口内发送 %d、预占 %d，期望 1 与 0", trailer, live, pending)
		}
		// 之后同模型直接补用户消息（拒绝已记住）：app 层换号后的下一次尝试补用户消息
		if !prefillRecentlyRejected(ChannelPlayground, model, time.Now()) {
			t.Fatalf("流尾=%v：不重发时也应记住该模型拒绝原生续写", trailer)
		}
		_ = lease.Release()
	}
}

// TestRegenerationNoteNotDuplicatedInTrace 重新生成与续写回退的说明按 ReportRequestNote 的约定：有请求说明观察者时只交给观察者
// （app 层写进进度日志与排查记录的时间线），不再另写一份 notes；没有观察者时写进排查记录的 notes
func TestRegenerationNoteNotDuplicatedInTrace(t *testing.T) {
	for _, observed := range []bool{true, false} {
		upstream := &regenUpstream{responses: []func() (*RPCResponse, error){
			streamResponse(regenStream("", regenMalformedFrame)), streamResponse(regenStream("", regenTextFrame, regenStopFrame)),
		}}
		trace := NewTrace()
		trace.Activate()
		ctx := variantContext(ContextWithTrace(context.Background(), trace))
		notes := &noteRecorder{}
		if observed {
			ctx = notes.context(ctx)
		}
		events, err := regenClient(t, "test", upstream).Generate(ctx, regenRequest("test"))
		if err != nil {
			t.Fatal(err)
		}
		collectEvents(t, events)
		data, err := trace.Finish()
		if err != nil {
			t.Fatal(err)
		}
		var record struct {
			Notes []string `json:"notes"`
		}
		if err := json.Unmarshal(data, &record); err != nil {
			t.Fatal(err)
		}
		inTrace := 0
		for _, note := range record.Notes {
			if strings.Contains(note, "同账号重新生成") {
				inTrace++
			}
		}
		want, wantObserved := 0, 1
		if !observed {
			want, wantObserved = 1, 0
		}
		if inTrace != want || notes.count("同账号重新生成") != wantObserved {
			t.Fatalf("有观察者=%v：排查记录 notes 里 %d 行、观察者 %d 行，期望 %d 与 %d", observed, inTrace, notes.count("同账号重新生成"), want, wantObserved)
		}
	}
}

// TestTraceFindingPrefillKind 排查结论按预填充的实际处理给出：原生续写原样发送、补用户消息、原生续写被拒后改回补用户消息
func TestTraceFindingPrefillKind(t *testing.T) {
	for _, test := range []struct {
		name     string
		notes    []string
		timeline []string
		want     string
	}{
		{name: "原生续写", notes: []string{"客户端最后一条是模型消息（预填充）：模型接受以模型轮结尾的请求，原样发送、由模型原生续写"}, want: "已按原生续写原样以模型轮结尾发送"},
		{name: "补用户消息", notes: []string{"客户端最后一条是模型消息（预填充）：已在末尾补一条请模型从这里接着写的用户消息"}, want: "已转成\"从上一条结尾接着写\"的用户消息"},
		{
			name:     "被拒后改回",
			notes:    []string{"客户端最后一条是模型消息（预填充）：模型接受以模型轮结尾的请求，原样发送、由模型原生续写"},
			timeline: []string{prefillFallbackNote("gemma-x", ChannelPlayground, nil)},
			want:     "上游不接受，已改回补一条",
		},
		{
			// long- 续写的进度与续写轮被拒的改回说明不是客户端预填充：客户端预填充原样发送成功，续写的第二轮被拒改回补用户消息
			name:  "续写轮被拒不改变客户端预填充的结论",
			notes: []string{"客户端最后一条是模型消息（预填充）：模型接受以模型轮结尾的请求，原样发送、由模型原生续写"},
			timeline: []string{
				"自动续写 | 第 1 轮结束 | 方式=模拟续写 | 本轮结束原因=max_tokens | 本轮输入=10 | 本轮输出=100 | 累计输出=100 | 总上限=300 | 下一步=第 2 轮（原生预填充）",
				"自动续写第 2 轮：" + prefillFallbackNote("gemma-x", ChannelPlayground, nil),
			},
			want: "已按原生续写原样以模型轮结尾发送",
		},
		{
			name:  "近期被拒过",
			notes: []string{"客户端最后一条是模型消息（预填充）：模型接受以模型轮结尾的请求，原样发送、由模型原生续写", "该模型近期在 playground 通道拒绝过以模型轮结尾的请求：直接在末尾补用户消息"},
			want:  "上游不接受，已改回补一条",
		},
	} {
		trace := NewTrace()
		trace.Activate()
		for _, note := range test.notes {
			trace.Note(note)
		}
		for _, message := range test.timeline {
			trace.Timeline("WARN", "service", message)
		}
		data, err := trace.Finish()
		if err != nil {
			t.Fatal(err)
		}
		var record struct {
			Findings []TraceFinding `json:"findings"`
		}
		if err := json.Unmarshal(data, &record); err != nil {
			t.Fatal(err)
		}
		found := ""
		for _, finding := range record.Findings {
			if strings.Contains(finding.Title, "预填充") {
				found = finding.Detail
			}
		}
		if !strings.Contains(found, test.want) {
			t.Fatalf("%s：预填充结论 = %q，期望含 %q", test.name, found, test.want)
		}
	}
}

// 思考摘要帧（没有签名）与函数调用帧
const (
	regenThoughtFrame  = `[[[[[[null,"先想一想",null,null,null,null,null,null,null,null,null,null,true]],"model"]]]]`
	regenFunctionFrame = `[[[[[[null,null,null,null,null,null,null,null,null,null,["lookup",null]]],"model"]]]]`
)

// TestGenerateNoRegenerateAfterThoughtOrToolCall 思考与工具调用同样算已交出的内容：先交出思考（HideThinking 关闭）再以函数调用格式错误
// 或流尾 503 结束、先交出函数调用再断流时都不重新生成，只发送一次（客户端已经看到这段思考或调用，重来会看到两段）；
// 隐藏思考（HideThinking）且思考没有签名时这段思考不交出，仍然重新生成
func TestGenerateNoRegenerateAfterThoughtOrToolCall(t *testing.T) {
	for _, test := range []struct {
		name  string
		first func() (*RPCResponse, error)
		kind  EventKind
		last  EventKind
	}{
		{name: "思考后函数调用格式错误", first: streamResponse(regenStream("", regenThoughtFrame, regenMalformedFrame)), kind: EventReasoning, last: EventFinish},
		{name: "思考后流尾 503", first: streamResponse(regenStream(`[14,"The service is currently unavailable."]`, regenThoughtFrame)), kind: EventReasoning, last: EventError},
		{name: "函数调用后断流", first: interruptedResponse(`[[` + regenFunctionFrame), kind: EventToolCall, last: EventError},
	} {
		upstream := &regenUpstream{responses: []func() (*RPCResponse, error){
			test.first, streamResponse(regenStream("", regenTextFrame, regenStopFrame)),
		}}
		notes := &noteRecorder{}
		events, err := regenClient(t, "test", upstream).Generate(variantContext(notes.context(context.Background())), regenRequest("test"))
		if err != nil {
			t.Fatalf("%s：%v", test.name, err)
		}
		got := collectEvents(t, events)
		if len(got) < 2 || got[0].Kind != test.kind || got[len(got)-1].Kind != test.last {
			t.Fatalf("%s：事件 = %v，期望先 %s、以 %s 结束", test.name, eventKinds(got), test.kind, test.last)
		}
		if upstream.calls() != 1 || notes.count("重新生成") != 0 {
			t.Fatalf("%s：已经交出思考或工具调用后不应重新生成，发送了 %d 次，请求说明 = %v", test.name, upstream.calls(), notes.notes)
		}
	}
	// 隐藏思考：没有签名的思考不交出，客户端什么也没收到，照常重新生成
	for _, first := range []func() (*RPCResponse, error){
		streamResponse(regenStream("", regenThoughtFrame, regenMalformedFrame)),
		streamResponse(regenStream(`[14,"The service is currently unavailable."]`, regenThoughtFrame)),
	} {
		upstream := &regenUpstream{responses: []func() (*RPCResponse, error){
			first, streamResponse(regenStream("", regenTextFrame, regenStopFrame)),
		}}
		request := regenRequest("test")
		request.Config.HideThinking = true
		events, err := regenClient(t, "test", upstream).Generate(variantContext(context.Background()), request)
		if err != nil {
			t.Fatal(err)
		}
		got := collectEvents(t, events)
		if upstream.calls() != 2 || len(got) != 3 || got[0].Kind != EventText || got[2].FinishReason != "stop" {
			t.Fatalf("隐藏思考且没有签名时应重新生成：发送 %d 次，事件 %v", upstream.calls(), eventKinds(got))
		}
	}
}

// TestGenerateRegenerationNeedsVariant 因结束原因重新生成要换一个输入：没有挂换输入函数（app 层没加随机后缀的请求）或换不了时不重新生成，
// 写一行说明、按这次的结果结束；流尾 5xx 不是确定性的，照旧原样重发
func TestGenerateRegenerationNeedsVariant(t *testing.T) {
	noVariant := func(ctx context.Context) context.Context {
		return ContextWithRegenerationVariant(ctx, func(request GenerateRequest) (GenerateRequest, bool) { return request, false })
	}
	for name, wrap := range map[string]func(context.Context) context.Context{
		"没有挂": func(ctx context.Context) context.Context { return ctx },
		"换不了": noVariant,
	} {
		upstream := &regenUpstream{responses: []func() (*RPCResponse, error){
			streamResponse(regenStream("", regenMalformedFrame)), streamResponse(regenStream("", regenTextFrame, regenStopFrame)),
		}}
		notes := &noteRecorder{}
		budget := NewRegenerationBudget()
		ctx := wrap(ContextWithRegenerationBudget(notes.context(context.Background()), budget))
		events, err := regenClient(t, "test", upstream).Generate(ctx, regenRequest("test"))
		if err != nil {
			t.Fatal(err)
		}
		got := collectEvents(t, events)
		if last := got[len(got)-1]; last.Kind != EventFinish || last.FinishReason != "malformed_function_call" {
			t.Fatalf("%s：应按这次的结果结束，得到 %v", name, eventKinds(got))
		}
		if upstream.calls() != 1 || budget.Used() != 0 || notes.count(regenerationNoVariantNote+" | 结束原因=malformed_function_call") != 1 {
			t.Fatalf("%s：不应原样重发：发送 %d 次、用掉 %d 次，请求说明 = %v", name, upstream.calls(), budget.Used(), notes.notes)
		}
		// 流尾 5xx：没有可换的输入也原样重发
		trailer := &regenUpstream{responses: []func() (*RPCResponse, error){
			streamResponse(regenStream(`[13,"Internal error encountered."]`)), streamResponse(regenStream("", regenTextFrame, regenStopFrame)),
		}}
		events, err = regenClient(t, "test", trailer).Generate(wrap(context.Background()), regenRequest("test"))
		if err != nil {
			t.Fatal(err)
		}
		if got := collectEvents(t, events); trailer.calls() != 2 || got[len(got)-1].FinishReason != "stop" || trailer.bodies[0] != trailer.bodies[1] {
			t.Fatalf("%s：流尾 5xx 应原样重发：发送 %d 次，事件 %v", name, trailer.calls(), eventKinds(got))
		}
	}
}

// fakeFirstEventClock 为测试用的首事件计时：window 为首事件超时，restartOK 为重新起算的结果，记下重新起算的次数
type fakeFirstEventClock struct {
	window    time.Duration
	restartOK bool
	restarts  atomic.Int32
}

func (clock *fakeFirstEventClock) FirstEventWindow() time.Duration { return clock.window }

func (clock *fakeFirstEventClock) RestartFirstEvent() bool {
	clock.restarts.Add(1)
	return clock.restartOK
}

// TestGenerateRegenerationFirstEventClock 首事件超时按每次发送计：重发前重新起算（每次重发一次）；首事件超时不到本次耗时的 1.2 倍时
// 不重新生成（重发多半被首事件计时打断，原本能交出的结果反倒成了首事件超时），写一行说明、按这次的结果结束；计时已到（尝试正被取消）时
// 不再重发；关闭（0）时不限制
func TestGenerateRegenerationFirstEventClock(t *testing.T) {
	slowMalformed := func(delay time.Duration) func() (*RPCResponse, error) {
		return func() (*RPCResponse, error) {
			time.Sleep(delay)
			return streamResponse(regenStream("", regenMalformedFrame))()
		}
	}
	run := func(clock *fakeFirstEventClock, first func() (*RPCResponse, error)) (*regenUpstream, *noteRecorder, []Event) {
		t.Helper()
		upstream := &regenUpstream{responses: []func() (*RPCResponse, error){first, streamResponse(regenStream("", regenTextFrame, regenStopFrame))}}
		notes := &noteRecorder{}
		ctx := ContextWithFirstEventClock(variantContext(notes.context(context.Background())), clock)
		events, err := regenClient(t, "test", upstream).Generate(ctx, regenRequest("test"))
		if err != nil {
			t.Fatal(err)
		}
		return upstream, notes, collectEvents(t, events)
	}
	// 首事件超时 10s、第一次 100ms：重新生成，重发前重新起算一次
	clock := &fakeFirstEventClock{window: 10 * time.Second, restartOK: true}
	upstream, _, got := run(clock, slowMalformed(100*time.Millisecond))
	if upstream.calls() != 2 || got[len(got)-1].FinishReason != "stop" || clock.restarts.Load() != 1 {
		t.Fatalf("首事件超时充裕时应重新生成并重新起算一次：发送 %d 次，重新起算 %d 次，事件 %v", upstream.calls(), clock.restarts.Load(), eventKinds(got))
	}
	// 首事件超时 300ms、第一次 300ms：不够 1.2 倍，不重新生成
	clock = &fakeFirstEventClock{window: 300 * time.Millisecond, restartOK: true}
	upstream, notes, got := run(clock, slowMalformed(300*time.Millisecond))
	if upstream.calls() != 1 || got[len(got)-1].FinishReason != "malformed_function_call" || clock.restarts.Load() != 0 ||
		notes.count("首事件超时不到本次耗时的 1.2 倍（重发多半被首事件计时打断），不重新生成 | FIRST_EVENT_TIMEOUT=300ms | 结束原因=malformed_function_call") != 1 {
		t.Fatalf("首事件超时不够时不应重新生成：发送 %d 次，请求说明 = %v", upstream.calls(), notes.notes)
	}
	// 计时已到：不再重发，交出取消（app 层按首事件超时换号），被放弃的那次不再交出
	clock = &fakeFirstEventClock{window: 10 * time.Second, restartOK: false}
	upstream, _, got = run(clock, streamResponse(regenStream("", regenMalformedFrame)))
	if upstream.calls() != 1 || clock.restarts.Load() != 1 || len(got) != 1 || got[0].Kind != EventError || !errors.Is(got[0].Err, context.Canceled) {
		t.Fatalf("首事件计时已到时不应重发：发送 %d 次，重新起算 %d 次，事件 %v", upstream.calls(), clock.restarts.Load(), eventKinds(got))
	}
	// 关闭（0）：不按首事件超时限制
	clock = &fakeFirstEventClock{restartOK: true}
	upstream, _, _ = run(clock, slowMalformed(100*time.Millisecond))
	if upstream.calls() != 2 {
		t.Fatalf("首事件超时关闭时应照常重新生成：发送 %d 次", upstream.calls())
	}
}

// TestReserveRegenerationReasons 重新生成与续写重发不准入时说明具体是哪一项：账号冷却（截止时刻与原因）、每分钟名额（已发出 + 预占 / 上限）、
// 出口令牌桶（出口的脱敏显示、通道与上限）；准入时返回空串并预占名额
func TestReserveRegenerationReasons(t *testing.T) {
	// 冷却
	pool := newRateTestPool(t, time.Minute, 0, 2, indexTestAccount("alice@example.com", BenefitTierFree, indexTestCatalog()))
	lease := mustTryAcquire(t, pool, rateTestSelection)
	until := time.Now().Add(time.Minute)
	pool.mu.Lock()
	pool.byID["alice@example.com"].runtime.Cooldowns[globalCooldownKey] = CooldownState{Until: until, Reason: "分钟限额 429：Resource has been exhausted"}
	pool.mu.Unlock()
	if denied := lease.ReserveRegeneration(indexTestModel); denied != "账号冷却至 "+until.Local().Format("15:04:05")+"（原因=分钟限额 429：Resource has been exhausted）" {
		t.Fatalf("冷却时的说明 = %q", denied)
	}
	_ = lease.Release()
	// 每分钟名额
	pool = newRateTestPool(t, time.Minute, 2, 2, indexTestAccount("alice@example.com", BenefitTierFree, indexTestCatalog()))
	lease = mustTryAcquire(t, pool, rateTestSelection)
	lease.RecordGenerationSent()
	if denied := lease.ReserveRegeneration(indexTestModel); denied != "" {
		t.Fatalf("名额还有余量时应准入：%q", denied)
	}
	// 本租约已有没发出的预占：不被自己的预占挡住，也不重复预占（与续写的下一轮同一口径，见 resend_admission.go）
	if denied := lease.ReserveRegeneration(indexTestModel); denied != "" {
		t.Fatalf("自己的预占不应挡住自己：%q", denied)
	}
	if _, _, pending, _ := rateSnapshot(pool, "alice@example.com"); pending != 1 {
		t.Fatalf("预占 = %d，期望不重复预占", pending)
	}
	lease.RecordGenerationSent()
	if denied := lease.ReserveRegeneration(indexTestModel); denied != "每分钟名额 2/2（已发出 2、预占 0，ACCOUNT_RPM_LIMIT）" {
		t.Fatalf("名额已满时的说明 = %q", denied)
	}
	_ = lease.Release()
	// 其他租约的预占同样计入
	pool = newRateTestPool(t, time.Minute, 2, 2, indexTestAccount("alice@example.com", BenefitTierFree, indexTestCatalog()))
	lease = mustTryAcquire(t, pool, rateTestSelection)
	lease.RecordGenerationSent()
	other := mustTryAcquire(t, pool, rateTestSelection)
	if denied := lease.ReserveRegeneration(indexTestModel); denied != "每分钟名额 2/2（已发出 1、预占 1，ACCOUNT_RPM_LIMIT）" {
		t.Fatalf("其他租约预占占满时的说明 = %q", denied)
	}
	_ = other.Release()
	_ = lease.Release()
	// 出口限速：取得租约用掉令牌、发出后令牌未到
	pool = newEgressTestPool(4, "alice@example.com")
	pool.SetEgressRateLimit("", EgressLaneNormal, 6)
	lease = mustTryAcquire(t, pool, egressPrimarySelection)
	lease.RecordGenerationSent()
	if denied := lease.ReserveRegeneration(indexTestModel); denied != "出口 "+EgressLabel("")+"（普通通道）限速 6 次/分钟，令牌未到" {
		t.Fatalf("出口限速时的说明 = %q", denied)
	}
	_ = lease.Release()
}

// TestTraceFindingIgnoresContinuationProgress long- 请求的客户端没有预填充时，续写进度里的“原生预填充”与续写轮被拒的改回说明不让排查结论
// 误报“最后一条消息是模型消息（预填充）”
func TestTraceFindingIgnoresContinuationProgress(t *testing.T) {
	trace := NewTrace()
	trace.Activate()
	trace.Timeline("INFO", "service", "自动续写 | 第 1 轮结束 | 方式=模拟续写 | 本轮结束原因=max_tokens | 本轮输入=10 | 本轮输出=100 | 累计输出=100 | 总上限=300 | 下一步=第 2 轮（原生预填充）")
	trace.Timeline("WARN", "service", "自动续写第 2 轮："+prefillFallbackNote("gemma-x", ChannelPlayground, nil))
	data, err := trace.Finish()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "最后一条消息是模型消息（预填充）") {
		t.Fatalf("续写进度不应让排查结论报预填充：%s", data)
	}
}
