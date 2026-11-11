package aistudio

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// long- 自动续写（continuation.go）的测试：假上游按调用顺序返回预先写好的 GenerateContent 流，记下每次收到的请求

const longTestAccount = "a@example.com"

// forgetPrefillRejection 清除该模型在 Playground 上被拒绝原生续写的记忆（prefill.go 的进程级记忆，测试之间共用），测试结束时再清一次
func forgetPrefillRejection(t *testing.T, model string) {
	t.Helper()
	prefillRejections.Delete(prefillRejectionKey(ChannelPlayground, model))
	t.Cleanup(func() { prefillRejections.Delete(prefillRejectionKey(ChannelPlayground, model)) })
}

// longPart 为一个 Part：thought 为思考
func longPart(text string, thought bool) string {
	quoted, _ := json.Marshal(text)
	if thought {
		return `[null,` + string(quoted) + `,null,null,null,null,null,null,null,null,null,null,true]`
	}
	return `[null,` + string(quoted) + `]`
}

// longTextFrame 为只带一段正文（或思考）的流帧
func longTextFrame(text string, thought bool) string {
	return `[[[[[` + longPart(text, thought) + `],"model"]]]]`
}

// longToolFrame 为带一个函数调用的流帧
func longToolFrame(name string) string {
	return `[[[[[[null,null,null,null,null,null,null,null,null,null,["` + name + `"]]],"model"]]]]`
}

// longFinishFrame 为完成帧：code 为终止原因，token 为候选 field 9 的续写 token（空为不带），usage 为 [输入, 正文, 总量, 思考]
func longFinishFrame(code int, token string, usage ...int64) string {
	candidate := fmt.Sprintf(`[null,%d`, code)
	if token != "" {
		candidate += `,null,null,null,null,null,null,"` + token + `"`
	}
	candidate += `]`
	frame := `[[` + candidate + `]`
	if len(usage) >= 3 {
		values := fmt.Sprintf(`[%d,%d,%d`, usage[0], usage[1], usage[2])
		if len(usage) >= 4 {
			values += fmt.Sprintf(`,null,null,null,null,null,null,%d`, usage[3])
		}
		frame += `,null,` + values + `]`
	}
	return frame + `]`
}

func longStream(frames ...string) string {
	return `[[` + strings.Join(frames, ",") + `]]`
}

// longCall 为假上游收到的一次请求
type longCall struct {
	request GenerateRequest
	body    []any
	account string
}

// longUpstream 按调用顺序返回 streams（errs 中同一下标非空时返回该错误，bodies 中同一下标非空时以它为响应体），send 不为 nil 时
// 在返回前调用（记发送、模拟延迟），gate 不为 nil 时同样在返回前调用，返回错误时这次发送以该错误失败（模拟发出前被冷却拦下）
type longUpstream struct {
	mu      sync.Mutex
	streams []string
	errs    map[int]error
	bodies  map[int]io.ReadCloser
	send    func(ctx context.Context)
	gate    func(ctx context.Context) error
	calls   []longCall
}

func (upstream *longUpstream) DoProtected(ctx context.Context, request GenerateRequest, rpc RPCRequest) (*RPCResponse, error) {
	var body []any
	if err := json.Unmarshal(rpc.Body, &body); err != nil {
		return nil, err
	}
	account := request.AccountID
	if lease, ok := AccountLeaseFromContext(ctx); ok {
		account = lease.Account().ID
	}
	upstream.mu.Lock()
	index := len(upstream.calls)
	upstream.calls = append(upstream.calls, longCall{request: request, body: body, account: account})
	upstream.mu.Unlock()
	if upstream.send != nil {
		upstream.send(ctx)
	}
	if upstream.gate != nil {
		if err := upstream.gate(ctx); err != nil {
			return nil, err
		}
	}
	if err := upstream.errs[index]; err != nil {
		return nil, err
	}
	if body := upstream.bodies[index]; body != nil {
		return &RPCResponse{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {JSONProtobufContentType}}, Body: body}, nil
	}
	if index >= len(upstream.streams) {
		return nil, errors.New("测试上游没有更多的流")
	}
	return &RPCResponse{
		StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {JSONProtobufContentType}},
		Body: io.NopCloser(strings.NewReader(upstream.streams[index])),
	}, nil
}

func (upstream *longUpstream) snapshot() []longCall {
	upstream.mu.Lock()
	defer upstream.mu.Unlock()
	return append([]longCall(nil), upstream.calls...)
}

// longClient 返回目录里只有 model 的 Client
func longClient(t *testing.T, upstream *longUpstream, model Model, defaults GenerationDefaults) *Client {
	t.Helper()
	client, err := NewClient(ClientOptions{Transport: ceilingTransport{}, Protected: upstream})
	if err != nil {
		t.Fatal(err)
	}
	client.catalogs[longTestAccount] = modelCatalog{models: []Model{model}, entries: map[string]modelEntry{model.ID: {model: model, defaults: defaults}}}
	return client
}

// longPlainModel 为没有官网续写能力、不能原生预填充的文本模型，单轮上限 100
func longPlainModel(id string) (Model, GenerationDefaults) {
	model := Model{ID: id, Methods: []string{"generateContent"}, InputTokenLimit: 1048576, OutputTokenLimit: 100, Capabilities: map[string]bool{"chat_model": true}}
	return model, GenerationDefaults{MaxOutputTokens: 100}
}

// longArgonModel 为有官网续写能力（能力码 91）的模型
func longArgonModel() (Model, GenerationDefaults) {
	model := Model{
		ID: "argon-test", Methods: []string{"generateContent"}, InputTokenLimit: 1048576, OutputTokenLimit: 65536,
		Capabilities: map[string]bool{"chat_model": true, "output_continuation": true},
	}
	return model, GenerationDefaults{MaxOutputTokens: 65536, ContinuedOutputTokens: 1048576}
}

func longRequest(model string, long bool) GenerateRequest {
	return GenerateRequest{
		ID: "req-long", Model: model, AccountID: longTestAccount, LongOutput: long,
		Contents: []Content{{Role: RoleUser, Parts: []Part{{Text: "写一篇很长的文章"}}}},
	}
}

// longCollect 读完事件流
func longCollect(t *testing.T, events <-chan Event) []Event {
	t.Helper()
	var got []Event
	timeout := time.After(10 * time.Second)
	for {
		select {
		case event, ok := <-events:
			if !ok {
				return got
			}
			got = append(got, event)
		case <-timeout:
			t.Fatal("事件流没有结束")
			return nil
		}
	}
}

// longSummary 汇总事件：正文、思考、用量事件、结束事件与错误
type longSummary struct {
	text, reasoning string
	usages          []Usage
	finishes        []string
	errs            []error
	tools           int
}

func summarizeLong(events []Event) longSummary {
	var summary longSummary
	for _, event := range events {
		switch event.Kind {
		case EventText:
			summary.text += event.Text
		case EventReasoning:
			summary.reasoning += event.Text
		case EventUsage:
			summary.usages = append(summary.usages, *event.Usage)
		case EventFinish:
			summary.finishes = append(summary.finishes, event.FinishReason)
		case EventError:
			summary.errs = append(summary.errs, event.Err)
		case EventToolCall:
			summary.tools++
		}
	}
	return summary
}

// runLong 发送请求并返回汇总、假上游收到的请求与续写进度
func runLong(t *testing.T, client *Client, upstream *longUpstream, ctx context.Context, request GenerateRequest) (longSummary, []longCall, []ContinuationProgress) {
	t.Helper()
	var mu sync.Mutex
	var progress []ContinuationProgress
	ctx = ContextWithContinuationObserver(ctx, func(item ContinuationProgress) {
		mu.Lock()
		progress = append(progress, item)
		mu.Unlock()
	})
	events, err := client.Generate(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	summary := summarizeLong(longCollect(t, events))
	mu.Lock()
	defer mu.Unlock()
	return summary, upstream.snapshot(), append([]ContinuationProgress(nil), progress...)
}

// roundEnds 返回进度中各轮结束的记录
func roundEnds(progress []ContinuationProgress) []ContinuationProgress {
	var ends []ContinuationProgress
	for _, item := range progress {
		if item.Stage == ContinuationRoundEnd {
			ends = append(ends, item)
		}
	}
	return ends
}

// wireConfig 返回请求体的 generation config（根 field 4）
func wireConfig(t *testing.T, body []any) []any {
	t.Helper()
	config, ok := body[3].([]any)
	if !ok {
		t.Fatalf("请求体没有 generation config: %v", body)
	}
	return config
}

// TestOfficialContinuationEncoding 官网续写的编码：generation config field 46 为 1、根 field 11 省略，第 N 轮的输出上限为
// min(总上限, (N+1)×32768)，续写轮带根 field 17 的续写 token；不续写的请求仍是 11 项、field 11 为 1、没有 field 46
func TestOfficialContinuationEncoding(t *testing.T) {
	_, defaults := longArgonModel()
	request := longRequest("argon-test", false)
	plain, err := EncodeGenerateContentRequest(request, defaults, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	var wire []any
	if err := json.Unmarshal(plain, &wire); err != nil {
		t.Fatal(err)
	}
	if len(wire) != 11 || wire[10] != float64(1) || len(wireConfig(t, wire)) >= 46 || wireConfig(t, wire)[3] != float64(65536) {
		t.Fatalf("普通请求应保持 11 项、field 11 为 1、没有续写字段: %s", plain)
	}
	for _, test := range []struct {
		round  int
		token  string
		total  int64
		length int
		limit  float64
	}{
		{round: 0, total: 1048576, length: 11, limit: 32768},
		{round: 2, token: "tok-2", total: 1048576, length: 17, limit: 98304},
		{round: 1, token: "tok-1", total: 40000, length: 17, limit: 40000},
	} {
		request.continuation = &officialContinuation{round: test.round, token: test.token, total: test.total}
		raw, err := EncodeGenerateContentRequest(request, defaults, RequestContext{})
		if err != nil {
			t.Fatal(err)
		}
		var body []any
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatal(err)
		}
		config := wireConfig(t, body)
		if len(body) != test.length || body[10] != nil || len(config) != 46 || config[45] != float64(1) || config[3] != test.limit {
			t.Fatalf("第 %d 轮编码不对: %s", test.round, raw)
		}
		if test.token != "" && body[16] != test.token {
			t.Fatalf("第 %d 轮应在根 field 17 带续写 token: %s", test.round, raw)
		}
	}
}

// TestFrameDecoderContinuation 续写编码时终止原因 22、或以 max_tokens 结束且同一帧带 token 而本轮上限低于总上限时本轮不形成终态；
// 不续写的解码器把 22 记为 continuation（不再是 provider_22）
func TestFrameDecoderContinuation(t *testing.T) {
	decode := func(decoder *FrameDecoder, frames ...string) []Event {
		t.Helper()
		var events []Event
		if err := DecodeGenerateStream(strings.NewReader(longStream(frames...)), decoder, func(event Event) error {
			events = append(events, event)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if err := decoder.End(); err != nil {
			t.Fatal(err)
		}
		return events
	}
	decoder := &FrameDecoder{continuable: true}
	events := decode(decoder, longTextFrame("段落", false), longFinishFrame(22, "tok-a", 10, 5, 15))
	if summary := summarizeLong(events); len(summary.finishes) != 0 || len(summary.usages) != 0 || summary.text != "段落" ||
		!decoder.continuationRequired || decoder.continuation != "tok-a" || decoder.usage == nil || decoder.usage.OutputTokens != 5 {
		t.Fatalf("终止原因 22 应转入下一轮: %+v decoder=%+v", summary, decoder)
	}
	decoder = &FrameDecoder{continuable: true, continueAtMaxTokens: true}
	decode(decoder, longFinishFrame(2, "tok-b", 10, 5, 15))
	if !decoder.continuationRequired || decoder.continuation != "tok-b" {
		t.Fatalf("max_tokens 带 token 且本轮上限低于总上限时应续写: %+v", decoder)
	}
	decoder = &FrameDecoder{continuable: true}
	if summary := summarizeLong(decode(decoder, longFinishFrame(2, "tok-c", 10, 5, 15))); decoder.continuationRequired || len(summary.finishes) != 1 || summary.finishes[0] != "max_tokens" {
		t.Fatalf("本轮上限已到总上限时 max_tokens 应照常结束: %+v", summary)
	}
	if summary := summarizeLong(decode(NewFrameDecoder(), longFinishFrame(22, "tok-d", 10, 5, 15))); len(summary.finishes) != 1 || summary.finishes[0] != "continuation" {
		t.Fatalf("不续写时终止原因 22 应为 continuation: %+v", summary)
	}
}

// TestOfficialContinuationRounds 官网续写在同一账号上连发三轮：第二、三轮带上一轮最近的续写 token、累计上限递增、省略根 field 11；
// 各轮正文接成一条流，用量与结束事件只在最后发一次（输入取最后一轮，正文累加，总量为最后一轮总量加之前各轮的输出）
func TestOfficialContinuationRounds(t *testing.T) {
	model, defaults := longArgonModel()
	upstream := &longUpstream{streams: []string{
		longStream(`[[[[[`+longPart("第一段", false)+`],"model"],null,null,null,null,null,null,null,"tok-early"]]]`, longFinishFrame(22, "tok-1", 100, 32000, 32100)),
		longStream(longTextFrame("第二段", false), longFinishFrame(2, "tok-2", 100, 32768, 32868)),
		longStream(longTextFrame("第三段", false), longFinishFrame(1, "", 100, 100, 200)),
	}}
	client := longClient(t, upstream, model, defaults)
	summary, calls, progress := runLong(t, client, upstream, context.Background(), longRequest(model.ID, true))
	if len(calls) != 3 || summary.text != "第一段第二段第三段" || len(summary.errs) != 0 {
		t.Fatalf("调用=%d 正文=%q 错误=%v，期望三轮接成一条", len(calls), summary.text, summary.errs)
	}
	for index, want := range []struct {
		token string
		limit float64
	}{{"", 32768}, {"tok-1", 65536}, {"tok-2", 98304}} {
		body := calls[index].body
		config := wireConfig(t, body)
		if body[10] != nil || config[45] != float64(1) || config[3] != want.limit || calls[index].account != longTestAccount {
			t.Fatalf("第 %d 轮请求不对: %v", index+1, body)
		}
		if want.token == "" && len(body) != 11 || want.token != "" && (len(body) != 17 || body[16] != want.token) {
			t.Fatalf("第 %d 轮续写 token 不对: %v", index+1, body)
		}
	}
	if len(summary.usages) != 1 || len(summary.finishes) != 1 || summary.finishes[0] != "stop" {
		t.Fatalf("用量=%v 结束=%v，期望各一次且结束为 stop", summary.usages, summary.finishes)
	}
	if usage := summary.usages[0]; usage.InputTokens != 100 || usage.OutputTokens != 32000+32768+100 || usage.TotalTokens != 200+32000+32768 {
		t.Fatalf("合并用量 = %+v", usage)
	}
	ends := roundEnds(progress)
	if len(ends) != 3 || !ends[0].Next || !ends[1].Next || ends[2].Next || ends[0].Mode != ContinuationOfficial ||
		ends[0].RoundFinish != "continuation" || ends[1].RoundFinish != "max_tokens" || ends[2].Output != 32000+32768+100 {
		t.Fatalf("每轮进度 = %+v", ends)
	}
}

// TestOfficialContinuationStops 官网续写到客户端给出的总上限（40000）时第二轮按 max_tokens 照常结束；终止原因 22 没有续写 token 时
// 不报错，按 max_tokens 结束；总上限不超过 32768 时按普通请求发送（与不带 long- 逐字节相同）
func TestOfficialContinuationStops(t *testing.T) {
	model, defaults := longArgonModel()
	limit := int64(40000)
	upstream := &longUpstream{streams: []string{
		longStream(longTextFrame("前", false), longFinishFrame(2, "tok-1", 10, 32768, 32778)),
		longStream(longTextFrame("后", false), longFinishFrame(2, "tok-2", 10, 7232, 7242)),
	}}
	request := longRequest(model.ID, true)
	request.Config.MaxOutputTokens = &limit
	summary, calls, _ := runLong(t, longClient(t, upstream, model, defaults), upstream, context.Background(), request)
	if len(calls) != 2 || wireConfig(t, calls[1].body)[3] != float64(40000) || summary.text != "前后" ||
		len(summary.finishes) != 1 || summary.finishes[0] != "max_tokens" {
		t.Fatalf("调用=%d 正文=%q 结束=%v，期望第二轮按总上限 40000 结束", len(calls), summary.text, summary.finishes)
	}

	upstream = &longUpstream{streams: []string{longStream(longTextFrame("只有一轮", false), longFinishFrame(22, "", 10, 5, 15))}}
	summary, calls, progress := runLong(t, longClient(t, upstream, model, defaults), upstream, context.Background(), longRequest(model.ID, true))
	if len(calls) != 1 || len(summary.errs) != 0 || len(summary.finishes) != 1 || summary.finishes[0] != "max_tokens" ||
		len(roundEnds(progress)) != 1 || !strings.Contains(roundEnds(progress)[0].Stop, "没有续写 token") {
		t.Fatalf("22 没有 token: 调用=%d %+v %+v", len(calls), summary, progress)
	}

	small := int64(20000)
	for _, long := range []bool{false, true} {
		upstream = &longUpstream{streams: []string{longStream(longTextFrame("短", false), longFinishFrame(2, "tok", 10, 5, 15))}}
		request := longRequest(model.ID, long)
		request.Config.MaxOutputTokens = &small
		_, calls, _ := runLong(t, longClient(t, upstream, model, defaults), upstream, context.Background(), request)
		if len(calls) != 1 || len(calls[0].body) != 11 || calls[0].body[10] != float64(1) || wireConfig(t, calls[0].body)[3] != float64(20000) {
			t.Fatalf("long=%v 总上限不超过 32768 时应按普通请求发送: %v", long, calls)
		}
	}
}

// TestSimulatedContinuationSeam 模拟续写：第一轮以 max_tokens 结束且有正文时把已输出的正文作为模型轮接在历史末尾、补一条请模型接着写的
// 用户消息再发一轮（输出上限为剩余总量与单轮上限中较小的，思考按最低）；新一轮开头与已输出正文末尾重复的部分去掉，新一轮的思考不输出；
// 合并用量的输入取第一轮
func TestSimulatedContinuationSeam(t *testing.T) {
	model, defaults := longPlainModel("plain-test")
	first := "开头的一段。the quick brown fox jumps over"
	upstream := &longUpstream{streams: []string{
		longStream(longTextFrame("先想想", true), longTextFrame(first, false), longFinishFrame(2, "", 50, 90, 160, 20)),
		longStream(longTextFrame("再想一遍", true), longTextFrame("the quick brown ", false), longTextFrame("fox jumps over the lazy dog.", false),
			longFinishFrame(1, "", 400, 20, 430, 10)),
	}}
	request := longRequest(model.ID, true)
	request.LongOutputDefaultTokens = 300
	summary, calls, progress := runLong(t, longClient(t, upstream, model, defaults), upstream, context.Background(), request)
	if len(calls) != 2 {
		t.Fatalf("调用 %d 次，期望 2", len(calls))
	}
	if want := first + " the lazy dog."; summary.text != want || summary.reasoning != "先想想" {
		t.Fatalf("正文=%q 思考=%q，期望去掉接缝处重复、只保留第一轮的思考", summary.text, summary.reasoning)
	}
	second := calls[1].request
	contents := second.Contents
	if len(contents) != 3 || contents[1].Role != RoleAssistant || contents[1].Parts[0].Text != first ||
		contents[2].Role != RoleUser || contents[2].Parts[0].Text != PrefillContinuationPrompt {
		t.Fatalf("第二轮历史 = %+v", contents)
	}
	if second.Config.MaxOutputTokens == nil || *second.Config.MaxOutputTokens != 100 || second.Config.ReasoningEffort != "minimal" || calls[1].account != longTestAccount {
		t.Fatalf("第二轮设置 = %+v", second.Config)
	}
	if len(summary.usages) != 1 || len(summary.finishes) != 1 || summary.finishes[0] != "stop" {
		t.Fatalf("用量=%v 结束=%v", summary.usages, summary.finishes)
	}
	if usage := summary.usages[0]; usage.InputTokens != 50 || usage.OutputTokens != 110 || usage.ReasoningTokens != 30 || usage.TotalTokens != 160+30 {
		t.Fatalf("合并用量 = %+v", usage)
	}
	ends := roundEnds(progress)
	if len(ends) != 2 || !ends[0].Next || ends[0].Prefill != ContinuationPrefillPrompt || ends[0].RoundOutput != 110 || ends[1].Output != 140 || ends[0].Mode != ContinuationSimulated {
		t.Fatalf("每轮进度 = %+v", ends)
	}
}

// TestSimulatedContinuationNativePrefill 能原生预填充的模型原样以模型轮结尾发送；上游仍以 400 拒绝时改为补用户消息重发这一轮
func TestSimulatedContinuationNativePrefill(t *testing.T) {
	model, defaults := longPlainModel("gemini-3-flash-preview")
	forgetPrefillRejection(t, model.ID)
	upstream := &longUpstream{
		streams: []string{
			longStream(longTextFrame("第一段", false), longFinishFrame(2, "", 10, 100, 110)),
			"",
			longStream(longTextFrame("第二段", false), longFinishFrame(1, "", 10, 5, 15)),
		},
		errs: map[int]error{1: &RPCError{Method: "GenerateContent", StatusCode: http.StatusBadRequest, Code: 3, Message: "Requests ending with a model turn are not supported"}},
	}
	request := longRequest(model.ID, true)
	request.LongOutputDefaultTokens = 300
	summary, calls, progress := runLong(t, longClient(t, upstream, model, defaults), upstream, context.Background(), request)
	if len(calls) != 3 || summary.text != "第一段第二段" || len(summary.errs) != 0 {
		t.Fatalf("调用=%d 正文=%q 错误=%v", len(calls), summary.text, summary.errs)
	}
	if contents := calls[1].request.Contents; len(contents) != 2 || contents[1].Role != RoleAssistant {
		t.Fatalf("原生预填充应以模型轮结尾: %+v", contents)
	}
	if contents := calls[2].request.Contents; len(contents) != 3 || contents[2].Role != RoleUser || contents[2].Parts[0].Text != PrefillContinuationPrompt {
		t.Fatalf("被拒绝后应补用户消息重发: %+v", contents)
	}
	sending := 0
	for _, item := range progress {
		if item.Stage == ContinuationSending && item.Round == 2 {
			sending++
		}
	}
	if ends := roundEnds(progress); len(ends) != 2 || ends[0].Prefill != ContinuationPrefillNative || sending != 2 {
		t.Fatalf("进度 = %+v", progress)
	}
}

// TestSimulatedContinuationStopRules 模拟续写不续写的情形：自然结束、本轮有工具调用、本轮只有思考、本轮上限已由总上限决定、
// 客户端给出的上限不超过单轮上限（与不带 long- 的请求逐字节相同）、不带 long-（以 max_tokens 结束也只发一次）
func TestSimulatedContinuationStopRules(t *testing.T) {
	model, defaults := longPlainModel("plain-test")
	type outcome struct {
		calls  int
		finish string
	}
	for _, test := range []struct {
		name    string
		long    bool
		total   int64
		limit   *int64
		streams []string
		want    outcome
		stop    string
	}{
		{name: "自然结束", long: true, total: 300, streams: []string{longStream(longTextFrame("完", false), longFinishFrame(1, "", 10, 5, 15))},
			want: outcome{1, "stop"}, stop: "上游结束"},
		{name: "工具调用", long: true, total: 300, streams: []string{longStream(longTextFrame("查一下", false), longToolFrame("lookup"), longFinishFrame(2, "", 10, 100, 110))},
			want: outcome{1, "max_tokens"}, stop: "工具调用"},
		{name: "只有思考", long: true, total: 300, streams: []string{longStream(longTextFrame("一直在想", true), longFinishFrame(2, "", 10, 0, 110, 100))},
			want: outcome{1, "max_tokens"}, stop: "没有新的正文"},
		{name: "到总上限", long: true, total: 150, streams: []string{
			longStream(longTextFrame("一", false), longFinishFrame(2, "", 10, 100, 110)),
			// 第二轮的上限已由总上限决定（50）：以 max_tokens 结束就不再续写，即使上游报告的输出（40）还没到总上限
			longStream(longTextFrame("二", false), longFinishFrame(2, "", 10, 40, 50)),
		}, want: outcome{2, "max_tokens"}, stop: "总上限"},
		{name: "不带 long-", long: false, total: 300, streams: []string{longStream(longTextFrame("一", false), longFinishFrame(2, "", 10, 100, 110))},
			want: outcome{1, "max_tokens"}},
	} {
		upstream := &longUpstream{streams: test.streams}
		request := longRequest(model.ID, test.long)
		request.LongOutputDefaultTokens = test.total
		summary, calls, progress := runLong(t, longClient(t, upstream, model, defaults), upstream, context.Background(), request)
		if len(calls) != test.want.calls || len(summary.finishes) != 1 || summary.finishes[0] != test.want.finish || len(summary.usages) != 1 {
			t.Fatalf("%s: 调用=%d 结束=%v 用量=%v，期望 %+v", test.name, len(calls), summary.finishes, summary.usages, test.want)
		}
		ends := roundEnds(progress)
		if test.stop == "" && len(ends) != 0 || test.stop != "" && (len(ends) != test.want.calls || !strings.Contains(ends[len(ends)-1].Stop, test.stop)) {
			t.Fatalf("%s: 进度 = %+v，期望不续写原因含 %q", test.name, ends, test.stop)
		}
		if test.name == "到总上限" && *calls[1].request.Config.MaxOutputTokens != 50 {
			t.Fatalf("第二轮输出上限 = %d，期望剩余的 50", *calls[1].request.Config.MaxOutputTokens)
		}
	}
	// 客户端给出的上限不超过单轮上限：按普通请求发送，请求体与不带 long- 的相同
	limit := int64(80)
	var bodies [2][]any
	for index, long := range []bool{false, true} {
		upstream := &longUpstream{streams: []string{longStream(longTextFrame("一", false), longFinishFrame(2, "", 10, 80, 90))}}
		request := longRequest(model.ID, long)
		request.Config.MaxOutputTokens = &limit
		request.LongOutputDefaultTokens = 300
		_, calls, _ := runLong(t, longClient(t, upstream, model, defaults), upstream, context.Background(), request)
		if len(calls) != 1 {
			t.Fatalf("long=%v: 调用 %d 次", long, len(calls))
		}
		bodies[index] = calls[0].body
	}
	if left, right := fmt.Sprint(bodies[0]), fmt.Sprint(bodies[1]); left != right {
		t.Fatalf("不续写的 long- 请求应与普通请求逐字节相同:\n%s\n%s", left, right)
	}
}

// TestLongOutputRoundFailure 第二轮起发送失败：不报错，已输出的正文完整保留，按 max_tokens 结束并带第一轮的用量（非流式由此
// 拿到全部已输出的内容，流式有用量）；第一轮失败与普通请求相同，以该错误结束
func TestLongOutputRoundFailure(t *testing.T) {
	model, defaults := longPlainModel("plain-test")
	unavailable := &RPCError{Method: "GenerateContent", StatusCode: http.StatusServiceUnavailable, Code: 14, Message: "unavailable"}
	upstream := &longUpstream{
		streams: []string{longStream(longTextFrame("已经输出", false), longFinishFrame(2, "", 10, 100, 110))},
		errs:    map[int]error{1: unavailable},
	}
	request := longRequest(model.ID, true)
	request.LongOutputDefaultTokens = 300
	summary, calls, progress := runLong(t, longClient(t, upstream, model, defaults), upstream, context.Background(), request)
	if len(calls) != 2 || summary.text != "已经输出" || len(summary.errs) != 0 || len(summary.finishes) != 1 || summary.finishes[0] != "max_tokens" {
		t.Fatalf("调用=%d %+v，期望保留已输出正文、不报错、按 max_tokens 结束", len(calls), summary)
	}
	if len(summary.usages) != 1 || summary.usages[0].InputTokens != 10 || summary.usages[0].OutputTokens != 100 || summary.usages[0].TotalTokens != 110 {
		t.Fatalf("用量 = %+v，期望第一轮的用量一次", summary.usages)
	}
	failed := false
	for _, item := range progress {
		failed = failed || item.Stage == ContinuationFailed && item.Round == 2 && item.Sending && errors.Is(item.Err, unavailable) &&
			strings.Contains(item.Stop, "发送失败，按 max_tokens 结束")
	}
	if !failed {
		t.Fatalf("进度应报告第 2 轮发送失败（带原错误）: %+v", progress)
	}

	// 第一轮失败：与不带 long- 的请求相同，以该错误结束
	upstream = &longUpstream{errs: map[int]error{0: unavailable}}
	client := longClient(t, upstream, model, defaults)
	if _, err := client.Generate(context.Background(), request); !errors.Is(err, unavailable) {
		t.Fatalf("第一轮发送失败应原样返回错误，得到 %v", err)
	}
	upstream = &longUpstream{streams: []string{`[[` + longTextFrame("第一轮", false)}}
	summary, _, _ = runLong(t, longClient(t, upstream, model, defaults), upstream, context.Background(), request)
	if summary.text != "第一轮" || len(summary.errs) != 1 || len(summary.finishes) != 0 {
		t.Fatalf("第一轮输出中断应以错误结束: %+v", summary)
	}
}

// TestLongOutputRoundStreamError 模拟续写后续轮次开头的正文先扣住比对接缝：这一轮输出中途出错（帧解析失败）时，扣住的正文照样交给
// 客户端，接着按 max_tokens 结束、带合并用量（这一轮按已收到的输出估算），不报错
func TestLongOutputRoundStreamError(t *testing.T) {
	model, defaults := longPlainModel("plain-test")
	upstream := &longUpstream{streams: []string{
		longStream(longTextFrame("第一轮的正文。", false), longFinishFrame(2, "", 10, 100, 110)),
		`[[` + longTextFrame("第二轮已收到的正文", false) + `,{"broken"`,
	}}
	request := longRequest(model.ID, true)
	request.LongOutputDefaultTokens = 300
	summary, calls, progress := runLong(t, longClient(t, upstream, model, defaults), upstream, context.Background(), request)
	if len(calls) != 2 || summary.text != "第一轮的正文。第二轮已收到的正文" || len(summary.errs) != 0 ||
		len(summary.finishes) != 1 || summary.finishes[0] != "max_tokens" {
		t.Fatalf("调用=%d %+v，期望第二轮已收到的正文照样输出并按 max_tokens 结束", len(calls), summary)
	}
	if len(summary.usages) != 1 || summary.usages[0].InputTokens != 10 || summary.usages[0].OutputTokens <= 100 {
		t.Fatalf("用量 = %+v，期望第一轮用量加上第二轮按已收到的输出估算的正文", summary.usages)
	}
	failed := false
	for _, item := range progress {
		failed = failed || item.Stage == ContinuationFailed && item.Round == 2 && !item.Sending && item.Err != nil &&
			strings.Contains(item.Stop, "输出中断")
	}
	if !failed {
		t.Fatalf("进度应报告第 2 轮输出中断: %+v", progress)
	}
}

// TestLongOutputInputLimitStops 模拟续写把已输出的正文发回上游：部分账号的输入上限（131072，按 429 返回）在续写轮被超过时
// 不报错，按 max_tokens 结束并带合并用量
func TestLongOutputInputLimitStops(t *testing.T) {
	model, defaults := longPlainModel("plain-test")
	capErr := &RPCError{
		Method: "GenerateContent", StatusCode: http.StatusTooManyRequests, Code: 8,
		Message: "The input token count exceeds the maximum number of tokens allowed 131072",
	}
	upstream := &longUpstream{
		streams: []string{
			longStream(longTextFrame("一", false), longFinishFrame(2, "", 10, 100, 110)),
			longStream(longTextFrame("二", false), longFinishFrame(2, "", 150, 100, 250)),
		},
		errs: map[int]error{2: capErr},
	}
	request := longRequest(model.ID, true)
	request.LongOutputDefaultTokens = 1000
	summary, calls, progress := runLong(t, longClient(t, upstream, model, defaults), upstream, context.Background(), request)
	if len(calls) != 3 || summary.text != "一二" || len(summary.errs) != 0 || len(summary.finishes) != 1 || summary.finishes[0] != "max_tokens" {
		t.Fatalf("调用=%d %+v，期望第 3 轮输入超限时按 max_tokens 结束", len(calls), summary)
	}
	if len(summary.usages) != 1 || summary.usages[0].OutputTokens != 200 || summary.usages[0].InputTokens != 10 {
		t.Fatalf("用量 = %+v，期望前两轮合并", summary.usages)
	}
	stopped := false
	for _, item := range progress {
		stopped = stopped || item.Stage == ContinuationFailed && item.Round == 3 && strings.Contains(item.Stop, "输入超过账号输入上限")
	}
	if !stopped {
		t.Fatalf("进度应报告输入超限停止: %+v", progress)
	}
}

// TestLongOutputSameLeaseAndRateWindow 续写的每一轮都在同一个租约上发送（不换号），各计入该账号的每分钟上限；窗口已满时
// 先等窗口释放（报告等待进度）再发下一轮
func TestLongOutputSameLeaseAndRateWindow(t *testing.T) {
	const window = 600 * time.Millisecond
	pool := newRateTestPool(t, window, 2, 2,
		indexTestAccount(longTestAccount, BenefitTierFree, indexTestCatalog()), indexTestAccount("b@example.com", BenefitTierFree, indexTestCatalog()))
	lease := mustTryAcquire(t, pool, AccountSelection{ModelID: indexTestModel, Method: "generateContent", AccountID: longTestAccount})
	defer func() { _ = lease.Release() }()
	model, defaults := longPlainModel("plain-test")
	// lives 为每轮发送（与 WorkerProtectedTransport 一样经租约记一次发送）之后账号窗口内的发送数
	var lives []int
	upstream := &longUpstream{
		streams: []string{
			longStream(longTextFrame("一", false), longFinishFrame(2, "", 10, 100, 110)),
			longStream(longTextFrame("二", false), longFinishFrame(2, "", 10, 100, 110)),
			longStream(longTextFrame("三", false), longFinishFrame(1, "", 10, 5, 15)),
		},
		send: func(ctx context.Context) {
			if current, ok := AccountLeaseFromContext(ctx); ok {
				if err := current.beginGenerationSend(indexTestModel); err != nil {
					t.Errorf("记发送: %v", err)
				}
				live, _, _, _ := rateSnapshot(pool, current.Account().ID)
				lives = append(lives, live)
			}
		},
	}
	request := longRequest(model.ID, true)
	request.LongOutputDefaultTokens = 1000
	startedAt := time.Now()
	summary, calls, progress := runLong(t, longClient(t, upstream, model, defaults), upstream, ContextWithAccountLease(context.Background(), lease), request)
	if len(calls) != 3 || summary.text != "一二三" {
		t.Fatalf("调用=%d 正文=%q", len(calls), summary.text)
	}
	for index, call := range calls {
		if call.account != longTestAccount {
			t.Fatalf("第 %d 轮在 %s 上发送，期望同一账号", index+1, call.account)
		}
	}
	// 第 3 轮等到第 1 轮的发送过期才发：窗口内始终不超过上限 2；没有等待时第 3 轮发送后窗口内会有 3 次
	if len(lives) != 3 || lives[0] != 1 || lives[1] != 2 || lives[2] > 2 {
		t.Fatalf("每轮发送后窗口内的发送数 = %v，期望依次计入且第 3 轮发送后不超过上限 2", lives)
	}
	waited := false
	for _, item := range progress {
		waited = waited || item.Stage == ContinuationWaiting && item.Round == 3 && item.Wait > 0
	}
	if !waited || time.Since(startedAt) < window/2 {
		t.Fatalf("第 3 轮应等账号每分钟窗口释放: 等待=%v 用时=%s 进度=%+v", waited, time.Since(startedAt), progress)
	}
	if admission := lease.continuationAdmission(indexTestModel, time.Now().Add(window)); admission.wait != 0 {
		t.Fatalf("窗口过去后不应再等，得到 %s", admission.wait)
	}
	pool.SetAccountRateLimit(0)
	if admission := lease.continuationAdmission(indexTestModel, time.Now()); admission.wait != 0 {
		t.Fatalf("关闭每分钟上限后不应等待，得到 %s", admission.wait)
	}
	if _, _, pending, _ := rateSnapshot(pool, longTestAccount); pending != 0 {
		t.Fatalf("各轮发出后预占 = %d，期望 0（续写的预占在发送时抵掉）", pending)
	}
}

// TestNewLongOutputPlan 续写计划：官网续写的总上限取客户端上限或目录的续写总量，模拟续写取客户端上限或 LONG_OUTPUT_MAX_TOKENS；
// 客户端上限先套用最大输出下限；总上限不超过单轮上限、Interaction 模型、图片与语音请求、非文本输出不续写
func TestNewLongOutputPlan(t *testing.T) {
	_, argon := longArgonModel()
	_, plain := longPlainModel("plain-test")
	value := func(v int64) *int64 { return &v }
	for _, test := range []struct {
		name     string
		request  GenerateRequest
		defaults GenerationDefaults
		official bool
		total    int64
	}{
		{name: "官网续写默认总量", request: GenerateRequest{LongOutput: true}, defaults: argon, official: true, total: 1048576},
		{name: "官网续写客户端上限", request: GenerateRequest{LongOutput: true, Config: GenerationConfig{MaxOutputTokens: value(500000)}}, defaults: argon, official: true, total: 500000},
		{name: "官网续写上限按续写总量封顶", request: GenerateRequest{LongOutput: true, Config: GenerationConfig{MaxOutputTokens: value(5000000)}}, defaults: argon, official: true, total: 1048576},
		{name: "下限提高后作为总上限", request: GenerateRequest{LongOutput: true, MinOutputTokens: 60000, Config: GenerationConfig{MaxOutputTokens: value(1000)}}, defaults: argon, official: true, total: 60000},
		{name: "模拟续写默认总量", request: GenerateRequest{LongOutput: true, LongOutputDefaultTokens: 262144}, defaults: plain, total: 262144},
		{name: "模拟续写客户端上限", request: GenerateRequest{LongOutput: true, LongOutputDefaultTokens: 262144, Config: GenerationConfig{MaxOutputTokens: value(250)}}, defaults: plain, total: 250},
	} {
		plan, reason := newLongOutputPlan(test.request, test.defaults, "plan-test")
		if plan == nil || plan.official != test.official || plan.total != test.total || reason != "" {
			t.Fatalf("%s: 计划 = %+v（%s），期望 official=%v total=%d", test.name, plan, reason, test.official, test.total)
		}
	}
	interaction := plain
	interaction.InteractionStream = true
	image := plain
	image.ImageRoute = true
	for name, test := range map[string]struct {
		request  GenerateRequest
		defaults GenerationDefaults
		reason   string
	}{
		"不带 long-":       {GenerateRequest{LongOutputDefaultTokens: 262144}, plain, ""},
		"默认总上限为 0":       {GenerateRequest{LongOutput: true}, plain, "LONG_OUTPUT_MAX_TOKENS=0"},
		"客户端上限不超过单轮":     {GenerateRequest{LongOutput: true, LongOutputDefaultTokens: 262144, Config: GenerationConfig{MaxOutputTokens: value(100)}}, plain, "总上限 100 不超过模型单轮上限 100"},
		"官网续写总量不超过一轮":    {GenerateRequest{LongOutput: true, Config: GenerationConfig{MaxOutputTokens: value(32768)}}, argon, "总上限 32768 不超过官网续写的单轮上限 32768"},
		"Interaction 模型": {GenerateRequest{LongOutput: true, LongOutputDefaultTokens: 262144}, interaction, "Interaction"},
		"图片模型":           {GenerateRequest{LongOutput: true, LongOutputDefaultTokens: 262144}, image, "图片模型"},
		"语音请求":           {GenerateRequest{LongOutput: true, LongOutputDefaultTokens: 262144, Config: GenerationConfig{SpeechConfig: &SpeechConfig{VoiceName: "Zephyr"}}}, plain, "语音"},
		"非文本输出":          {GenerateRequest{LongOutput: true, LongOutputDefaultTokens: 262144, Config: GenerationConfig{ResponseModalities: []ResponseModality{ResponseModalityImage}}}, plain, "非文本输出"},
	} {
		plan, reason := newLongOutputPlan(test.request, test.defaults, "plan-test")
		if plan != nil || test.reason == "" && reason != "" || test.reason != "" && !strings.Contains(reason, test.reason) {
			t.Fatalf("%s: 不应续写，得到 %+v，原因 %q（期望含 %q）", name, plan, reason, test.reason)
		}
	}
	// 原样以模型轮结尾与客户端预填充共用 prefill.go 的名单与拒绝记忆：名单内、近期没被拒过的模型原生预填充，被拒过的直接补用户消息
	forgetPrefillRejection(t, "gemini-3.1-pro-preview")
	native := GenerateRequest{LongOutput: true, LongOutputDefaultTokens: 262144}
	if plan, _ := newLongOutputPlan(native, plain, "gemini-3.1-pro-preview"); plan == nil || !plan.prefill || plan.model != "gemini-3.1-pro-preview" {
		t.Fatalf("名单内的模型应原生预填充: %+v", plan)
	}
	if plan, _ := newLongOutputPlan(native, plain, "gemini-2.5-pro"); plan == nil || plan.prefill {
		t.Fatalf("名单外的模型应补用户消息: %+v", plan)
	}
	rememberPrefillRejected(ChannelPlayground, "gemini-3.1-pro-preview", time.Now())
	if plan, _ := newLongOutputPlan(native, plain, "gemini-3.1-pro-preview"); plan == nil || plan.prefill {
		t.Fatalf("近期被拒过的模型应直接补用户消息（与客户端预填充共用记忆）: %+v", plan)
	}
}

// TestSeamOverlapAndUsageMerge 接缝去重至少 20 字节才认定重复（中文按字节、去掉后仍是合法 UTF-8）；两种续写方式的用量合并
func TestSeamOverlapAndUsageMerge(t *testing.T) {
	if got := seamOverlap("前文……the quick brown foxes", "the quick brown foxes jump"); got != len("the quick brown foxes") {
		t.Fatalf("重复 20 字节以上应去掉，得到 %d", got)
	}
	if got := seamOverlap("前文……the quick brown fox", "the quick brown fox jumps"); got != 0 {
		t.Fatalf("19 字节的相同前后缀按巧合保留，得到 %d", got)
	}
	if got := seamOverlap("前文 brown fox", "brown fox jumps"); got != 0 {
		t.Fatalf("不到 20 字节的相同前后缀按巧合保留，得到 %d", got)
	}
	tail := "这是已经输出的一段中文正文，写到这里"
	next := "一段中文正文，写到这里然后接着写"
	if got := seamOverlap(tail, next); got != len("一段中文正文，写到这里") || !strings.HasPrefix(next[got:], "然后") {
		t.Fatalf("中文接缝 = %d", got)
	}
	previous := Usage{InputTokens: 100, OutputTokens: 30, ReasoningTokens: 20, ToolTokens: 5, TotalTokens: 155}
	next2 := Usage{InputTokens: 900, OutputTokens: 40, ReasoningTokens: 10, TotalTokens: 950}
	if got := mergeContinuedUsage(previous, next2, true); got.InputTokens != 900 || got.OutputTokens != 70 || got.ReasoningTokens != 30 || got.ToolTokens != 5 || got.TotalTokens != 950+55 {
		t.Fatalf("官网续写合并 = %+v", got)
	}
	if got := mergeContinuedUsage(previous, next2, false); got.InputTokens != 100 || got.OutputTokens != 70 || got.TotalTokens != 155+50 {
		t.Fatalf("模拟续写合并 = %+v", got)
	}
	missing := Usage{InputTokens: 10, TotalTokens: 40, ReasoningTokens: 5, OutputTokensMissing: true}
	if got := mergeContinuedUsage(missing, missing, false); got.OutputTokens != 50 || got.OutputTokensMissing {
		t.Fatalf("缺正文 token 时按总量补上: %+v", got)
	}
}

// longSignedPart 为带思考签名（part field 15）的正文 Part
func longSignedPart(text string, signature string) string {
	quoted, _ := json.Marshal(text)
	return `[null,` + string(quoted) + strings.Repeat(`,null`, 12) + `,"` + signature + `"]`
}

// longGroundingFrame 为只带搜索来源（候选 field 8）的流帧：一个正文片段 [start, end)
func longGroundingFrame(start, end int) string {
	return fmt.Sprintf(`[[[null,null,null,null,null,null,null,[null,null,[[[0,%d,%d]]]]]]]`, start, end)
}

// runLongEvents 与 runLong 相同，返回原始事件
func runLongEvents(t *testing.T, client *Client, ctx context.Context, request GenerateRequest, observer func(ContinuationProgress)) []Event {
	t.Helper()
	if observer != nil {
		ctx = ContextWithContinuationObserver(ctx, observer)
	}
	events, err := client.Generate(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	return longCollect(t, events)
}

// TestLongOutputPlanStructuredAndTools 模拟续写不用于结构化输出（response schema，或 application/json、text/x.enum 这类不是普通文本的
// 输出类型）与带 Google 内置工具（联网搜索、URL Context 等）的请求：按普通请求发送并给出原因。官网续写由上游接着同一次生成往下写，
// 不受影响；text/plain 与只有函数工具的请求照常模拟续写（有函数调用的一轮另按停止规则不续写）
func TestLongOutputPlanStructuredAndTools(t *testing.T) {
	_, argon := longArgonModel()
	_, plain := longPlainModel("plain-test")
	base := GenerateRequest{LongOutput: true, LongOutputDefaultTokens: 262144}
	withConfig := func(config GenerationConfig) GenerateRequest {
		request := base
		request.Config = config
		return request
	}
	withTools := func(tools Tools) GenerateRequest {
		request := base
		request.Tools = tools
		return request
	}
	for name, test := range map[string]struct {
		request GenerateRequest
		reason  string
	}{
		"response schema": {withConfig(GenerationConfig{ResponseSchema: json.RawMessage(`{"type":"object"}`)}), "结构化输出（JSON Schema 或 JSON 模式）不能模拟续写"},
		"JSON 模式":         {withConfig(GenerationConfig{ResponseMIMEType: "application/json"}), "结构化输出"},
		"枚举":              {withConfig(GenerationConfig{ResponseMIMEType: " Text/X.Enum "}), "结构化输出"},
		"联网搜索":            {withTools(Tools{GoogleSearch: &GoogleSearchOptions{WebSearch: true}}), "Google 内置工具"},
		"URL Context":     {withTools(Tools{Google: []string{"url_context"}}), "Google 内置工具"},
	} {
		if plan, reason := newLongOutputPlan(test.request, plain, "plain-test"); plan != nil || !strings.Contains(reason, test.reason) {
			t.Fatalf("%s: 模拟续写不应续写，得到 %+v，原因 %q（期望含 %q）", name, plan, reason, test.reason)
		}
		if plan, reason := newLongOutputPlan(test.request, argon, "argon-test"); plan == nil || !plan.official || reason != "" {
			t.Fatalf("%s: 官网续写不受影响，得到 %+v（%s）", name, plan, reason)
		}
	}
	for name, request := range map[string]GenerateRequest{
		"text/plain": withConfig(GenerationConfig{ResponseMIMEType: "text/plain"}),
		"只有函数工具":     withTools(Tools{Functions: []FunctionDeclaration{{Name: "lookup"}}}),
	} {
		if plan, reason := newLongOutputPlan(request, plain, "plain-test"); plan == nil || plan.official || reason != "" {
			t.Fatalf("%s: 应照常模拟续写，得到 %+v（%s）", name, plan, reason)
		}
	}
}

// TestSimulatedContinuationStructuredOutput 带 response schema 的 long- 请求在没有官网续写能力的模型上只发一轮：第一轮以 max_tokens
// 截在 JSON 中间也不续写（第二轮带着同一个 schema 会从头输出一个新的 JSON，拼起来不合法），报告一次不续写的原因、按 max_tokens 结束；
// 有官网续写能力的模型照常官网续写，合并后是一个合法的 JSON
func TestSimulatedContinuationStructuredOutput(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{"items":{"type":"array","items":{"type":"string"}}}}`)
	model, defaults := longPlainModel("plain-test")
	upstream := &longUpstream{streams: []string{
		longStream(longTextFrame(`{"items":["a","b",`, false), longFinishFrame(2, "", 10, 100, 110)),
		longStream(longTextFrame(`{"items":["c"]}`, false), longFinishFrame(1, "", 10, 5, 15)),
	}}
	request := longRequest(model.ID, true)
	request.LongOutputDefaultTokens = 300
	request.Config.ResponseMIMEType, request.Config.ResponseSchema = "application/json", schema
	summary, calls, progress := runLong(t, longClient(t, upstream, model, defaults), upstream, context.Background(), request)
	if len(calls) != 1 || summary.text != `{"items":["a","b",` || len(summary.finishes) != 1 || summary.finishes[0] != "max_tokens" {
		t.Fatalf("调用=%d %+v，期望只发一轮、按 max_tokens 结束", len(calls), summary)
	}
	skipped := 0
	for _, item := range progress {
		if item.Stage == ContinuationSkipped && strings.Contains(item.Stop, "结构化输出") {
			skipped++
		}
	}
	if skipped != 1 || len(roundEnds(progress)) != 0 {
		t.Fatalf("进度 = %+v，期望报告一次不续写的原因", progress)
	}

	argon, argonDefaults := longArgonModel()
	upstream = &longUpstream{streams: []string{
		longStream(longTextFrame(`{"items":["a",`, false), longFinishFrame(22, "tok-1", 10, 32768, 32778)),
		longStream(longTextFrame(`"b"]}`, false), longFinishFrame(1, "", 10, 5, 15)),
	}}
	request = longRequest(argon.ID, true)
	request.Config.ResponseMIMEType, request.Config.ResponseSchema = "application/json", schema
	summary, calls, _ = runLong(t, longClient(t, upstream, argon, argonDefaults), upstream, context.Background(), request)
	if len(calls) != 2 || summary.text != `{"items":["a","b"]}` || !json.Valid([]byte(summary.text)) {
		t.Fatalf("官网续写调用=%d 正文=%q，期望照常续写成一个合法的 JSON", len(calls), summary.text)
	}
}

// TestSimulatedContinuationGrounding 模拟续写：有搜索来源（groundingMetadata）的一轮之后不再续写；只在后续轮次出现的搜索来源，
// 正文片段的位置换算到合并后的正文（加上之前各轮放行的正文字节数、减去接缝去掉的重复字节数）
func TestSimulatedContinuationGrounding(t *testing.T) {
	model, defaults := longPlainModel("plain-test")
	upstream := &longUpstream{streams: []string{
		longStream(longTextFrame("第一轮", false), longGroundingFrame(0, 9), longFinishFrame(2, "", 10, 100, 110)),
		longStream(longTextFrame("不应发送", false), longFinishFrame(1, "", 10, 5, 15)),
	}}
	request := longRequest(model.ID, true)
	request.LongOutputDefaultTokens = 300
	_, calls, progress := runLong(t, longClient(t, upstream, model, defaults), upstream, context.Background(), request)
	if ends := roundEnds(progress); len(calls) != 1 || len(ends) != 1 || !strings.Contains(ends[0].Stop, "本轮有搜索来源") {
		t.Fatalf("调用=%d 进度=%+v，期望有搜索来源的一轮之后不续写", len(calls), ends)
	}

	first := "开头的一段。the quick brown fox jumps over"
	repeated := "the quick brown fox jumps over"
	tail := " the lazy dog."
	upstream = &longUpstream{streams: []string{
		longStream(longTextFrame(first, false), longFinishFrame(2, "", 10, 100, 110)),
		longStream(longTextFrame(repeated+tail, false), longGroundingFrame(len(repeated), len(repeated)+len(tail)), longFinishFrame(1, "", 10, 5, 15)),
	}}
	events := runLongEvents(t, longClient(t, upstream, model, defaults), context.Background(), request, nil)
	text := ""
	var grounding *GroundingMetadata
	for _, event := range events {
		switch event.Kind {
		case EventText:
			text += event.Text
		case EventGrounding:
			grounding = event.Grounding
		}
	}
	if text != first+tail || grounding == nil || len(grounding.Supports) != 1 {
		t.Fatalf("正文=%q 来源=%+v", text, grounding)
	}
	segment := grounding.Supports[0].Segment
	if segment.StartIndex != len(first) || segment.EndIndex != len(first)+len(tail) || text[segment.StartIndex:segment.EndIndex] != tail {
		t.Fatalf("片段位置 = [%d, %d)，期望换算到合并正文的 [%d, %d)", segment.StartIndex, segment.EndIndex, len(first), len(first)+len(tail))
	}
}

// TestSimulatedContinuationFollowUpSignatures 模拟续写后续轮次的正文不带思考签名（思考只保留在第一轮），函数调用的签名保留；
// 第一轮正文的签名原样保留
func TestSimulatedContinuationFollowUpSignatures(t *testing.T) {
	model, defaults := longPlainModel("plain-test")
	toolPart := `[null,null,null,null,null,null,null,null,null,null,["lookup"],null,null,null,"sig-tool"]`
	upstream := &longUpstream{streams: []string{
		longStream(`[[[[[`+longSignedPart("第一轮的正文", "sig-1")+`],"model"]]]]`, longFinishFrame(2, "", 10, 100, 110)),
		longStream(`[[[[[`+longSignedPart("第二轮的正文", "sig-2")+`,`+toolPart+`],"model"]]]]`, longFinishFrame(2, "", 10, 100, 110)),
	}}
	request := longRequest(model.ID, true)
	request.LongOutputDefaultTokens = 1000
	events := runLongEvents(t, longClient(t, upstream, model, defaults), context.Background(), request, nil)
	var texts, signatures []string
	toolSignature := ""
	for _, event := range events {
		switch event.Kind {
		case EventText:
			texts = append(texts, event.Text)
			signatures = append(signatures, event.ThoughtSignature)
		case EventToolCall:
			toolSignature = event.ToolCall.ThoughtSignature + "|" + event.ThoughtSignature
		}
	}
	if strings.Join(texts, "") != "第一轮的正文第二轮的正文" || len(signatures) != 2 || signatures[0] != "sig-1" || signatures[1] != "" {
		t.Fatalf("正文=%q 签名=%q，期望第二轮的正文不带签名", texts, signatures)
	}
	if toolSignature != "sig-tool|sig-tool" {
		t.Fatalf("函数调用签名 = %q，期望保留", toolSignature)
	}
}

// TestSimulatedContinuationOtherOutputStops 模拟续写：以 max_tokens 结束的一轮有代码执行（或媒体输出）时不续写
func TestSimulatedContinuationOtherOutputStops(t *testing.T) {
	model, defaults := longPlainModel("plain-test")
	codePart := `[null,null,null,null,null,null,null,[1,"print(1)"]]`
	upstream := &longUpstream{streams: []string{
		longStream(longTextFrame("先算一下", false), `[[[[[`+codePart+`],"model"]]]]`, longFinishFrame(2, "", 10, 100, 110)),
		longStream(longTextFrame("不应发送", false), longFinishFrame(1, "", 10, 5, 15)),
	}}
	request := longRequest(model.ID, true)
	request.LongOutputDefaultTokens = 300
	summary, calls, progress := runLong(t, longClient(t, upstream, model, defaults), upstream, context.Background(), request)
	ends := roundEnds(progress)
	if len(calls) != 1 || len(ends) != 1 || ends[0].Next || ends[0].Stop != "本轮有代码执行或媒体输出，不续写" ||
		len(summary.finishes) != 1 || summary.finishes[0] != "max_tokens" {
		t.Fatalf("调用=%d 进度=%+v 结束=%v，期望有代码执行的一轮之后不续写", len(calls), ends, summary.finishes)
	}
}

// TestLongOutputRoundInterrupted 生成服务经 Interrupt 结束第二轮（本服务的期限到期）：发送前结束时按发送失败、输出中途结束时按输出中断
// 体面结束，已输出的内容（含第二轮扣住待比对接缝的正文）完整返回、按 max_tokens 结束并带合并用量，不报错；Err 为结束的原因
func TestLongOutputRoundInterrupted(t *testing.T) {
	model, defaults := longPlainModel("plain-test")
	reason := errors.New("测试结束这一轮")
	// 发送阶段：第二轮的发送一直等到这一轮的上下文结束（模拟准备 WAA proof、等响应头），开始等之后才结束这一轮
	sending := make(chan struct{}, 1)
	var upstream *longUpstream
	upstream = &longUpstream{
		streams: []string{longStream(longTextFrame("第一轮的正文。", false), longFinishFrame(2, "", 10, 100, 110))},
		gate: func(ctx context.Context) error {
			if len(upstream.snapshot()) < 2 {
				return nil
			}
			sending <- struct{}{}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(5 * time.Second):
				return errors.New("发送没有随这一轮结束")
			}
		},
	}
	request := longRequest(model.ID, true)
	request.LongOutputDefaultTokens = 300
	var mu sync.Mutex
	var progress []ContinuationProgress
	interrupt := make(chan func(error), 1)
	observer := func(item ContinuationProgress) {
		mu.Lock()
		progress = append(progress, item)
		mu.Unlock()
		if item.Stage == ContinuationRoundEnd && item.Next {
			interrupt <- item.Interrupt
		}
	}
	go func() {
		stop := <-interrupt
		<-sending
		stop(reason)
	}()
	startedAt := time.Now()
	summary := summarizeLong(runLongEvents(t, longClient(t, upstream, model, defaults), context.Background(), request, observer))
	if elapsed := time.Since(startedAt); elapsed > 2*time.Second {
		t.Fatalf("用时 %s，期望结束这一轮时正在进行的发送随之结束", elapsed)
	}
	if len(upstream.snapshot()) != 2 || summary.text != "第一轮的正文。" || len(summary.errs) != 0 ||
		len(summary.finishes) != 1 || summary.finishes[0] != "max_tokens" || len(summary.usages) != 1 || summary.usages[0].OutputTokens != 100 {
		t.Fatalf("%+v，期望第二轮发送前被结束时体面结束", summary)
	}
	assertInterrupted := func(sending bool) {
		t.Helper()
		mu.Lock()
		defer mu.Unlock()
		for _, item := range progress {
			var interrupted *ContinuationInterruptedError
			if item.Stage == ContinuationFailed && item.Round == 2 && item.Sending == sending && errors.As(item.Err, &interrupted) &&
				errors.Is(item.Err, reason) && strings.Contains(item.Stop, "续写第 2 轮被本服务结束（测试结束这一轮），按 max_tokens 结束") {
				return
			}
		}
		t.Fatalf("进度 = %+v，期望报告第 2 轮被本服务结束（发送阶段=%v）", progress, sending)
	}
	assertInterrupted(true)

	// 输出中途：第二轮的响应体写出一段正文后停住，结束这一轮时关闭响应体，扣住的正文照样交给客户端
	reader, writer := io.Pipe()
	defer func() { _ = writer.Close() }()
	written := make(chan struct{})
	go func() {
		_, _ = writer.Write([]byte(`[[` + longTextFrame("第二轮写出的一段", false) + `,`))
		close(written)
	}()
	upstream = &longUpstream{
		streams: []string{longStream(longTextFrame("第一轮的正文。", false), longFinishFrame(2, "", 10, 100, 110))},
		bodies:  map[int]io.ReadCloser{1: reader},
	}
	progress = nil
	interrupt = make(chan func(error), 1)
	observer = func(item ContinuationProgress) {
		mu.Lock()
		progress = append(progress, item)
		mu.Unlock()
		if item.Stage == ContinuationRoundEnd && item.Next {
			interrupt <- item.Interrupt
		}
	}
	go func() {
		stop := <-interrupt
		<-written
		time.Sleep(50 * time.Millisecond)
		stop(reason)
	}()
	summary = summarizeLong(runLongEvents(t, longClient(t, upstream, model, defaults), context.Background(), request, observer))
	if summary.text != "第一轮的正文。第二轮写出的一段" || len(summary.errs) != 0 || len(summary.finishes) != 1 || summary.finishes[0] != "max_tokens" ||
		len(summary.usages) != 1 {
		t.Fatalf("%+v，期望第二轮输出中途被结束时交出已收到的正文并体面结束", summary)
	}
	assertInterrupted(false)

	// 决定续写时就结束这一轮（离绝对总时长上限不到一个空闲期限）：不再发送第二轮，同样体面结束
	upstream = &longUpstream{streams: []string{longStream(longTextFrame("第一轮的正文。", false), longFinishFrame(2, "", 10, 100, 110))}}
	progress = nil
	observer = func(item ContinuationProgress) {
		mu.Lock()
		progress = append(progress, item)
		mu.Unlock()
		if item.Stage == ContinuationRoundEnd && item.Next {
			item.Interrupt(reason)
		}
	}
	summary = summarizeLong(runLongEvents(t, longClient(t, upstream, model, defaults), context.Background(), request, observer))
	if calls := upstream.snapshot(); len(calls) != 1 || summary.text != "第一轮的正文。" || len(summary.finishes) != 1 || summary.finishes[0] != "max_tokens" {
		t.Fatalf("调用=%d %+v，期望不再发送第二轮、按 max_tokens 结束", len(calls), summary)
	}
	assertInterrupted(true)
}

// trackedBody 为记下关闭次数的响应体。底层响应体只应关一次：WAA_BACKEND=go 的 HTTP/2 响应体第二次 Close 会对已结束的流再发一个
// RST_STREAM(CANCEL)（见 onceCloseBody）
type trackedBody struct {
	io.Reader
	closes atomic.Int32
}

func (body *trackedBody) Close() error {
	body.closes.Add(1)
	return nil
}

// TestLongOutputClosesPreviousRound 续写每发送一轮之前先关闭上一轮已读完的响应体：发第二轮时第一轮（Generate 持有的响应）已关闭，
// 发第三轮时第二轮已关闭；模拟续写的原生预填充在流尾被拒、在同一轮内补用户消息重发时，被拒的那一次同样先关闭。原先第一轮的响应体
// 挂到整个请求结束，第二轮起要等下一轮的上下文取消后在另一个 goroutine 里关。请求结束时全部关闭，输出与不改时相同。
// 每个响应体的底层恰好关闭一次：第一轮由 decodeLongRounds 关过之后 Generate 结束时不再关，第二轮起取消这一轮的回调与 closePrevious
// 不重复关闭（底层 Close 不一定能重复，见 onceCloseBody）
func TestLongOutputClosesPreviousRound(t *testing.T) {
	model, defaults := longPlainModel("gemini-3-flash-preview")
	forgetPrefillRejection(t, model.ID)
	streams := []string{
		longStream(longTextFrame("第一段", false), longFinishFrame(2, "", 10, 100, 110)),
		// 第二轮以模型轮原样发送：流尾 400 拒绝，在这一轮内改补用户消息重发
		`[[],[3,"Requests ending with a model turn are not supported"]]`,
		longStream(longTextFrame("第二段", false), longFinishFrame(2, "", 10, 100, 110)),
		longStream(longTextFrame("第三段", false), longFinishFrame(1, "", 10, 5, 15)),
	}
	tracked := make([]*trackedBody, len(streams))
	bodies := make(map[int]io.ReadCloser, len(streams))
	for index, stream := range streams {
		tracked[index] = &trackedBody{Reader: strings.NewReader(stream)}
		bodies[index] = tracked[index]
	}
	var mu sync.Mutex
	var open []string
	upstream := &longUpstream{bodies: bodies}
	upstream.send = func(context.Context) {
		upstream.mu.Lock()
		index := len(upstream.calls) - 1
		upstream.mu.Unlock()
		mu.Lock()
		defer mu.Unlock()
		for previous := range index {
			if closes := tracked[previous].closes.Load(); closes != 1 {
				open = append(open, fmt.Sprintf("发送第 %d 次时第 %d 次的响应体关闭了 %d 次", index+1, previous+1, closes))
			}
		}
	}
	request := longRequest(model.ID, true)
	request.LongOutputDefaultTokens = 1000
	summary, calls, _ := runLong(t, longClient(t, upstream, model, defaults), upstream, context.Background(), request)
	if len(calls) != len(streams) || summary.text != "第一段第二段第三段" || len(summary.errs) != 0 || summary.finishes[len(summary.finishes)-1] != "stop" {
		t.Fatalf("调用=%d 正文=%q 结束=%v 错误=%v", len(calls), summary.text, summary.finishes, summary.errs)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(open) != 0 {
		t.Fatalf("续写发下一轮之前应先关闭上一轮的响应体（恰好一次）：%v", open)
	}
	for index, body := range tracked {
		if closes := body.closes.Load(); closes != 1 {
			t.Fatalf("请求结束后第 %d 次的响应体关闭了 %d 次，期望恰好 1 次", index+1, closes)
		}
	}
}

// onceCloseRecorder 为不能重复关闭的底层响应体：关闭次数用普通字段记（并发关闭时 -race 报数据竞争），第二次关闭记为错误
type onceCloseRecorder struct {
	io.Reader
	closes int
}

func (body *onceCloseRecorder) Close() error {
	body.closes++
	if body.closes > 1 {
		return errors.New("重复关闭")
	}
	return nil
}

// TestOnceCloseBody 只关一次的包装：读取原样转发；多次关闭（含并发）只关底层一次，之后都返回第一次的结果
func TestOnceCloseBody(t *testing.T) {
	recorder := &onceCloseRecorder{Reader: strings.NewReader("data")}
	body := newOnceCloseBody(recorder)
	if data, err := io.ReadAll(body); err != nil || string(data) != "data" {
		t.Fatalf("读取=%q err=%v", data, err)
	}
	var wait sync.WaitGroup
	for range 8 {
		wait.Go(func() {
			if err := body.Close(); err != nil {
				t.Errorf("Close 返回 %v", err)
			}
		})
	}
	wait.Wait()
	if err := body.Close(); err != nil || recorder.closes != 1 {
		t.Fatalf("底层关闭 %d 次（期望 1 次），err=%v", recorder.closes, err)
	}
	failing := newOnceCloseBody(&failingCloser{})
	if first, second := failing.Close(), failing.Close(); first == nil || second != first {
		t.Fatalf("底层关闭失败时每次都应返回第一次的错误：%v %v", first, second)
	}
}

// failingCloser 为关闭失败的底层响应体
type failingCloser struct{ calls int }

func (closer *failingCloser) Read([]byte) (int, error) { return 0, io.EOF }

func (closer *failingCloser) Close() error {
	closer.calls++
	return fmt.Errorf("关闭失败（第 %d 次）", closer.calls)
}
