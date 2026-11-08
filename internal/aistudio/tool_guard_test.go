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
	"syscall"
	"testing"
	"time"
)

// Playground 通道上工具约束的扣住与同账号重新生成（tool_guard.go）的测试

const (
	guardFunction = "Kei_Workflow"
	// guardStrongHint 为必须调用 Kei_Workflow 时系统指令里的工具要求（第一次发送就是这句，第 1 级）；guardExtraHint 为重新生成时
	// （第 2 级）再加的一句，同时在最后一条用户文字消息末尾附上 guardStrongHint
	guardStrongHint = "You must call the function Kei_Workflow in this response with arguments that match its schema. Do not reply with plain text."
	guardExtraHint  = "Reply with the function call only."
	// guardUserHint 为附在最后一条用户文字消息末尾的要求在请求体里的写法（接在空行之后、在这段文本的末尾）
	guardUserHint = "\\n\\n" + guardStrongHint + `"]],"user"`
	// guardOldHint 为原先第一次发送的写法（gemini-4 常不照做），现在不再发送
	guardOldHint = "Use one of these tools in this response"
)

// guardTools 返回 Kei_Workflow 与 Kei_Search 两个函数；mode 为工具模式，pick 非空时指定这个函数，strict 时 Kei_Workflow 为 strict
func guardTools(mode string, pick string, strict bool) Tools {
	tools := Tools{
		Functions: []FunctionDeclaration{
			{
				Name: guardFunction, Strict: strict,
				Parameters: json.RawMessage(`{"type":"object","properties":{"task":{"type":"string"}},"required":["task"],"additionalProperties":false}`),
			},
			{Name: "Kei_Search", Parameters: json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}}}`)},
		},
		ToolConfig: ToolConfig{Mode: mode},
	}
	if pick != "" {
		tools.ToolConfig.AllowedFunctionNames = []string{pick}
	}
	return tools
}

// guardClient 返回支持函数调用的 Playground 模型 modelID 的 Client，上游按 responses 依次返回
func guardClient(t *testing.T, modelID string, upstream *regenUpstream) *Client {
	t.Helper()
	client := regenClient(t, modelID, upstream)
	catalog := client.catalogs["alice@example.com"]
	entry := catalog.entries[modelID]
	entry.model.Capabilities = map[string]bool{"chat_model": true, "function_declarations": true}
	catalog.models = []Model{entry.model}
	catalog.entries = map[string]modelEntry{modelID: entry}
	client.catalogs["alice@example.com"] = catalog
	return client
}

// guardRequest 按 PooledService.Generate 的步骤整理带工具约束的请求（prepareToolRequest 把契约挂到请求上）
func guardRequest(t *testing.T, tools Tools) GenerateRequest {
	t.Helper()
	request := regenRequest("test")
	request.System = "be helpful"
	request.Tools = tools
	prepared, contract, err := prepareToolRequest(request)
	if err != nil || contract == nil || prepared.toolContract != contract {
		t.Fatalf("prepareToolRequest: contract=%v err=%v", contract, err)
	}
	return prepared
}

// 流帧：正文、思考、函数调用（参数为 Struct 编码）与正常结束
var (
	guardTextFrame     = `[[[[[[null,"plain answer"]],"model"]]]]`
	guardThoughtFrame  = `[[[[[[null,"thinking",null,null,null,null,null,null,null,null,null,null,true]],"model"]]]]`
	guardCallFrame     = guardFunctionFrame(guardFunction, `[[["task",[null,null,"go"]]]]`)
	guardBadArgsFrame  = guardFunctionFrame(guardFunction, `[[["town",[null,null,"Paris"]]]]`)
	guardOtherCallFrom = guardFunctionFrame("Kei_Search", `[[["q",[null,null,"x"]]]]`)
	guardAfterText     = `[[[[[[null,"after call"]],"model"]]]]`
)

func guardFunctionFrame(name string, arguments string) string {
	return `[[[[[[null,null,null,null,null,null,null,null,null,null,["` + name + `",` + arguments + `]]],"model"]]]]`
}

// guardContractError 返回事件里的工具约束错误
func guardContractError(events []Event) *ToolContractError {
	for _, event := range events {
		var contract *ToolContractError
		if event.Kind == EventError && errors.As(event.Err, &contract) {
			return contract
		}
	}
	return nil
}

func guardHasText(events []Event, text string) bool {
	for _, event := range events {
		if (event.Kind == EventText || event.Kind == EventReasoning) && strings.Contains(event.Text, text) {
			return true
		}
	}
	return false
}

// TestToolGuardRequiredRegenerates 指定函数（必须调用）时第一次只回复文字：扣住的思考与正文不交出，同账号重新生成，客户端只收到第二次的
// 调用、用量与结束。第一次发送就写明必须调用（第 1 级，不再是原来 gemini-4 常不照做的 “Use one of these tools …”），重新生成的那次
// 再加一句并在最后一条用户文字消息末尾附上同样的要求（第 2 级）。有随机后缀时换一个，没有时同样重新生成
func TestToolGuardRequiredRegenerates(t *testing.T) {
	for _, variant := range []bool{true, false} {
		t.Run(fmt.Sprintf("随机后缀=%v", variant), func(t *testing.T) {
			upstream := &regenUpstream{responses: []func() (*RPCResponse, error){
				streamResponse(regenStream("", guardThoughtFrame, guardTextFrame, regenStopFrame)),
				streamResponse(regenStream("", guardCallFrame, regenStopFrame)),
			}}
			notes := &noteRecorder{}
			ctx := ContextWithRegenerationBudget(notes.context(context.Background()), NewRegenerationBudget())
			if variant {
				ctx = variantContext(ctx)
			}
			events, err := guardClient(t, "test", upstream).Generate(ctx, guardRequest(t, guardTools("required", guardFunction, false)))
			if err != nil {
				t.Fatal(err)
			}
			got := collectEvents(t, events)
			kinds := eventKinds(got)
			if len(got) != 3 || kinds[0] != EventToolCall || got[0].ToolCall.Name != guardFunction || kinds[1] != EventUsage || kinds[2] != EventFinish {
				t.Fatalf("事件 = %v，期望只有第二次的 调用/用量/结束", kinds)
			}
			if guardHasText(got, "plain answer") || guardHasText(got, "thinking") {
				t.Fatal("第一次扣住的正文与思考不应交出")
			}
			if upstream.calls() != 2 {
				t.Fatalf("应发送 2 次，实际 %d", upstream.calls())
			}
			first, second := upstream.bodies[0], upstream.bodies[1]
			if !strings.Contains(first, "be helpful\\n"+guardStrongHint) || strings.Contains(first, guardExtraHint) ||
				strings.Contains(first, guardUserHint) || strings.Contains(first, guardOldHint) {
				t.Fatalf("第一次发送应在客户端系统提示后写明必须调用（第 1 级），不附到用户消息：%s", first)
			}
			if !strings.Contains(second, "be helpful\\n"+guardStrongHint+"\\n"+guardExtraHint) || !strings.Contains(second, guardUserHint) ||
				strings.Contains(second, guardOldHint) {
				t.Fatalf("重新生成应保留客户端系统提示、再加一句并附到用户消息（第 2 级）：%s", second)
			}
			input := "输入=已加强工具要求"
			if variant {
				input = "输入=已换随机后缀，已加强工具要求"
			}
			want := "工具约束未满足，同账号重新生成 | 原因=要求调用工具 Kei_Workflow，上游没有返回工具调用（结束原因=stop） | 第 1/2 次 | 本次耗时="
			if notes.count(want) != 1 || notes.count(input) != 1 {
				t.Fatalf("请求说明 = %v，期望一行重新生成说明（%s）", notes.notes, input)
			}
			if variant != strings.Contains(second, variantMarker+"1") {
				t.Fatalf("有随机后缀=%v 时第二次发送换后缀应为 %v", variant, variant)
			}
		})
	}
}

// TestToolGuardSecondLevelHint 每次都只回复文字：两次重新生成都用第 2 级的要求（再加一句、附到用户消息），第二次重新生成时要求已是最高级。
// 有随机后缀时换一个新的照常重发，共生成 3 次、请求说明写明次数已用完；没有随机后缀时第三次发送会与第二次逐字节相同（上游对相同输入
// 返回相同回复），与批次 B 一样不再重新生成：只发 2 次、只占 1 次重新生成，请求说明写明原因。都按 502 结束、不交出任何正文
func TestToolGuardSecondLevelHint(t *testing.T) {
	for _, variant := range []bool{true, false} {
		t.Run(fmt.Sprintf("随机后缀=%v", variant), func(t *testing.T) {
			upstream := &regenUpstream{responses: []func() (*RPCResponse, error){streamResponse(regenStream("", guardTextFrame, regenStopFrame))}}
			notes := &noteRecorder{}
			budget := NewRegenerationBudget()
			ctx := ContextWithRegenerationBudget(notes.context(context.Background()), budget)
			if variant {
				ctx = variantContext(ctx)
			}
			events, err := guardClient(t, "test", upstream).Generate(ctx, guardRequest(t, guardTools("required", guardFunction, false)))
			if err != nil {
				t.Fatal(err)
			}
			attempts, used := 2, 1
			if variant {
				attempts, used = 3, regenerationLimit
			}
			got := collectEvents(t, events)
			violation := guardContractError(got)
			if len(got) != 1 || violation == nil || violation.Kind != ToolContractMissingCall || violation.Attempts != attempts {
				t.Fatalf("事件 = %v 违约=%+v，期望只有一个写明共生成 %d 次的违约", eventKinds(got), violation, attempts)
			}
			if message, want := violation.PublicMessage(), fmt.Sprintf("The model did not call the required function Kei_Workflow (tried %d times). Please retry.", attempts); message != want {
				t.Fatalf("对外说明 = %q，期望 %q", message, want)
			}
			if !strings.Contains(violation.Error(), fmt.Sprintf("要求调用工具 Kei_Workflow，上游没有返回工具调用（结束原因=stop）（共生成 %d 次）", attempts)) {
				t.Fatalf("日志原因 = %q", violation.Error())
			}
			if upstream.calls() != attempts || budget.Used() != used {
				t.Fatalf("应发送 %d 次、用掉 %d 次重新生成，实际 %d 次、%d 次", attempts, used, upstream.calls(), budget.Used())
			}
			first, second := upstream.bodies[0], upstream.bodies[1]
			if !strings.Contains(first, guardStrongHint) || strings.Contains(first, guardExtraHint) || strings.Contains(first, guardUserHint) {
				t.Fatalf("第一次发送应为第 1 级：%s", first)
			}
			for index, body := range upstream.bodies[1:] {
				if !strings.Contains(body, guardStrongHint+"\\n"+guardExtraHint) || strings.Count(body, guardUserHint) != 1 {
					t.Fatalf("第 %d 次重新生成应为第 2 级（再加一句、用户消息末尾附一次要求）：%s", index+1, body)
				}
			}
			const unchanged = "工具约束未满足，但请求没有加随机后缀（客户端指定了 seed、温度为 0、最后一轮是工具结果或 REPEAT_PROMPT_NONCE 关闭）" +
				"且工具要求已是最高的第 2 级，原样重发多半得到同样的结果，不再重新生成 | 原因=要求调用工具 Kei_Workflow，上游没有返回工具调用（结束原因=stop） | 本次耗时="
			exhausted := "工具约束未满足，重新生成次数已用完（2 次），不再重新生成 | 原因=要求调用工具 Kei_Workflow"
			if !variant {
				if notes.count("工具约束未满足，同账号重新生成") != 1 || notes.count("第 1/2 次") != 1 || notes.count("输入=已加强工具要求") != 1 ||
					notes.count(unchanged) != 1 || notes.count(exhausted) != 0 || notes.count("原样重发 |") != 0 {
					t.Fatalf("请求说明 = %v，期望第一次重新生成写已加强、之后写明输入不会变而不再重新生成", notes.notes)
				}
				return
			}
			third := upstream.bodies[2]
			if third == second || !strings.Contains(second, variantMarker+"1") || !strings.Contains(third, variantMarker+"2") {
				t.Fatal("有随机后缀时第二次重新生成应换一个新的后缀")
			}
			if notes.count("工具约束未满足，同账号重新生成") != 2 || notes.count(exhausted) != 1 || notes.count(unchanged) != 0 ||
				notes.count("第 1/2 次") != 1 || notes.count("输入=已换随机后缀，已加强工具要求") != 1 || notes.count("第 2/2 次") != 1 ||
				notes.count("| 输入=已换随机后缀") != 2 {
				t.Fatalf("请求说明 = %v，期望第一次重新生成写换后缀并加强、第二次只换后缀、之后次数已用完", notes.notes)
			}
		})
	}
}

// TestToolGuardStrictRegenerates strict 参数不符、调用了未选择的函数而还没交出事件时同账号重新生成，第二次正确调用后照常交出；
// 次数用完时对外说明写明违约种类（未选择的函数名来自上游输出，不写进说明）。一直违约时有随机后缀共生成 3 次；没有随机后缀时
// auto 模式从第 0 级起，两次重新生成前都还能升级（0→1→2），每次输入都有变化，同样 3 次；必须调用的从第 1 级起，第二次重新生成时
// 输入不会变，只生成 2 次
func TestToolGuardStrictRegenerates(t *testing.T) {
	for _, test := range []struct {
		name   string
		tools  Tools
		first  string
		reason string
		public string
		// plain 为没有随机后缀时一直违约的发送次数
		plain int
	}{
		{
			name: "auto 模式 strict 参数不符", tools: guardTools("auto", "", true), first: guardBadArgsFrame,
			reason: `原因=函数 "Kei_Workflow" 的参数不符合 strict Schema`,
			public: "The model's arguments for Kei_Workflow did not match the strict schema (tried %d times). Please retry.", plain: 3,
		},
		{
			name: "必须调用时 strict 参数不符", tools: guardTools("required", guardFunction, true), first: guardBadArgsFrame,
			reason: `原因=函数 "Kei_Workflow" 的参数不符合 strict Schema`,
			public: "The model's arguments for Kei_Workflow did not match the strict schema (tried %d times). Please retry.", plain: 2,
		},
		{
			name: "调用了未选择的函数", tools: guardTools("required", guardFunction, false), first: guardOtherCallFrom,
			reason: `原因=上游调用了未选择的函数 "Kei_Search"`,
			public: "The model called a function that was not selected (tried %d times). Please retry.", plain: 2,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			upstream := &regenUpstream{responses: []func() (*RPCResponse, error){
				streamResponse(regenStream("", test.first, regenStopFrame)),
				streamResponse(regenStream("", guardCallFrame, regenStopFrame)),
			}}
			notes := &noteRecorder{}
			ctx := ContextWithRegenerationBudget(notes.context(context.Background()), NewRegenerationBudget())
			events, err := guardClient(t, "test", upstream).Generate(ctx, guardRequest(t, test.tools))
			if err != nil {
				t.Fatal(err)
			}
			got := collectEvents(t, events)
			if kinds := eventKinds(got); len(got) != 3 || kinds[0] != EventToolCall || string(got[0].ToolCall.Arguments) != `{"task":"go"}` {
				t.Fatalf("事件 = %v，期望第二次正确的调用", kinds)
			}
			if upstream.calls() != 2 || notes.count("工具约束未满足，同账号重新生成 | "+test.reason) != 1 {
				t.Fatalf("发送 %d 次，请求说明 = %v", upstream.calls(), notes.notes)
			}
			// 一直违约：次数用完或输入不会再变之后按 502 结束，说明写明违约种类与共生成的次数
			for _, variant := range []bool{true, false} {
				always := &regenUpstream{responses: []func() (*RPCResponse, error){streamResponse(regenStream("", test.first, regenStopFrame))}}
				ctx, want := context.Background(), test.plain
				if variant {
					ctx, want = variantContext(ctx), 3
				}
				events, err = guardClient(t, "test", always).Generate(ctx, guardRequest(t, test.tools))
				if err != nil {
					t.Fatal(err)
				}
				violation := guardContractError(collectEvents(t, events))
				if public := fmt.Sprintf(test.public, want); violation == nil || violation.PublicMessage() != public || always.calls() != want {
					t.Fatalf("随机后缀=%v：违约=%+v，发送 %d 次，期望发送 %d 次、对外说明 %q", variant, violation, always.calls(), want, public)
				}
			}
		})
	}
}

// TestToolGuardNoRegenerationAfterContent auto 模式只有 strict 的契约不扣住事件：正文先交出，之后参数不符时照旧以违约结束、不重新生成，
// 请求说明写明已交出内容
func TestToolGuardNoRegenerationAfterContent(t *testing.T) {
	upstream := &regenUpstream{responses: []func() (*RPCResponse, error){
		streamResponse(regenStream("", guardTextFrame, guardBadArgsFrame, regenStopFrame)),
		streamResponse(regenStream("", guardCallFrame, regenStopFrame)),
	}}
	notes := &noteRecorder{}
	events, err := guardClient(t, "test", upstream).Generate(notes.context(context.Background()), guardRequest(t, guardTools("auto", "", true)))
	if err != nil {
		t.Fatal(err)
	}
	got := collectEvents(t, events)
	violation := guardContractError(got)
	if len(got) != 2 || got[0].Kind != EventText || violation == nil || violation.Kind != ToolContractSchemaMismatch || violation.Attempts != 1 {
		t.Fatalf("事件 = %v 违约=%+v，期望正文之后以 strict 违约结束", eventKinds(got), violation)
	}
	if violation.PublicMessage() != "The model's arguments for Kei_Workflow did not match the strict schema. Please retry." {
		t.Fatalf("对外说明 = %q", violation.PublicMessage())
	}
	if upstream.calls() != 1 || notes.count("工具约束未满足，已向客户端交出内容，不重新生成") != 1 {
		t.Fatalf("发送 %d 次，请求说明 = %v，期望不重新生成", upstream.calls(), notes.notes)
	}
}

// TestToolGuardAdmissionDenied 重新生成与批次 B 共用准入：每分钟名额已满时不重新生成，按违约结束，请求说明写明是名额
func TestToolGuardAdmissionDenied(t *testing.T) {
	pool := newRateTestPool(t, time.Minute, 1, 2, indexTestAccount("alice@example.com", BenefitTierFree, indexTestCatalog()))
	lease := mustTryAcquire(t, pool, rateTestSelection)
	defer func() { _ = lease.Release() }()
	upstream := &regenUpstream{lease: lease, responses: []func() (*RPCResponse, error){
		streamResponse(regenStream("", guardTextFrame, regenStopFrame)),
		streamResponse(regenStream("", guardCallFrame, regenStopFrame)),
	}}
	notes := &noteRecorder{}
	budget := NewRegenerationBudget()
	ctx := ContextWithRegenerationBudget(ContextWithAccountLease(notes.context(context.Background()), lease), budget)
	request := guardRequest(t, guardTools("required", guardFunction, false))
	request.Model = indexTestModel
	events, err := guardClient(t, indexTestModel, upstream).Generate(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	got := collectEvents(t, events)
	if violation := guardContractError(got); len(got) != 1 || violation == nil || violation.Attempts != 1 {
		t.Fatalf("事件 = %v，期望只有违约", eventKinds(got))
	}
	if upstream.calls() != 1 || budget.Used() != 0 {
		t.Fatalf("准入不足时不应重新生成：发送 %d 次，用掉 %d 次", upstream.calls(), budget.Used())
	}
	if notes.count("工具约束未满足，但每分钟名额 1/1（已发出 1、预占 0，ACCOUNT_RPM_LIMIT），不重新生成 | 原因=") != 1 {
		t.Fatalf("请求说明 = %v", notes.notes)
	}
}

// TestToolGuardUnchangedInputSkipsAdmission 没有随机后缀、要求已是第 2 级时不再重新生成，也不占次数与准入：每分钟名额 3 个，
// 第一次发送与第一次重新生成各用 1 个，结束时租约上没有留下预占（不再重新生成的那次没有经 ReserveRegeneration 预占）
func TestToolGuardUnchangedInputSkipsAdmission(t *testing.T) {
	pool := newRateTestPool(t, time.Minute, 3, 2, indexTestAccount("alice@example.com", BenefitTierFree, indexTestCatalog()))
	lease := mustTryAcquire(t, pool, rateTestSelection)
	defer func() { _ = lease.Release() }()
	upstream := &regenUpstream{lease: lease, responses: []func() (*RPCResponse, error){streamResponse(regenStream("", guardTextFrame, regenStopFrame))}}
	notes := &noteRecorder{}
	budget := NewRegenerationBudget()
	ctx := ContextWithRegenerationBudget(ContextWithAccountLease(notes.context(context.Background()), lease), budget)
	request := guardRequest(t, guardTools("required", guardFunction, false))
	request.Model = indexTestModel
	events, err := guardClient(t, indexTestModel, upstream).Generate(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if violation := guardContractError(collectEvents(t, events)); violation == nil || violation.Attempts != 2 {
		t.Fatalf("违约=%+v，期望共生成 2 次", violation)
	}
	if upstream.calls() != 2 || budget.Used() != 1 || notes.count("且工具要求已是最高的第 2 级，原样重发多半得到同样的结果，不再重新生成") != 1 {
		t.Fatalf("发送 %d 次、用掉 %d 次，请求说明 = %v", upstream.calls(), budget.Used(), notes.notes)
	}
	pool.mu.Lock()
	pending := lease.ratePending
	pool.mu.Unlock()
	if pending {
		t.Fatal("不再重新生成的那次不应预占每分钟名额")
	}
}

// gatedBody 先读出 head，再等 release 关闭后读出 tail（上游先返回一段，过一会儿才出后面的帧）
type gatedBody struct {
	head, tail *strings.Reader
	release    chan struct{}
	once       sync.Once
}

func (body *gatedBody) Read(buffer []byte) (int, error) {
	if body.head.Len() > 0 {
		return body.head.Read(buffer)
	}
	<-body.release
	return body.tail.Read(buffer)
}

func (body *gatedBody) Close() error {
	body.once.Do(func() {
		select {
		case <-body.release:
		default:
			close(body.release)
		}
	})
	return nil
}

func gatedResponse(body *gatedBody) func() (*RPCResponse, error) {
	return func() (*RPCResponse, error) {
		return &RPCResponse{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {JSONProtobufContentType}}, Body: body}, nil
	}
}

// TestToolGuardHoldsUntilCall 必须调用时正文与思考先扣住：调用出现之前事件流上没有任何事件（首事件计时暂停一次），调用到达后按原顺序
// 先交出扣住的思考与正文、再交出调用，之后的事件照常转发；auto 模式只有 strict 的契约不扣住，正文立即交出
func TestToolGuardHoldsUntilCall(t *testing.T) {
	for _, test := range []struct {
		name  string
		tools Tools
		hold  bool
	}{
		{name: "必须调用扣住", tools: guardTools("required", guardFunction, false), hold: true},
		{name: "strict 不扣住", tools: guardTools("auto", "", true)},
	} {
		t.Run(test.name, func(t *testing.T) {
			body := &gatedBody{
				head:    strings.NewReader("[[" + guardThoughtFrame + "," + guardTextFrame + ","),
				tail:    strings.NewReader(guardCallFrame + "," + guardAfterText + "," + regenStopFrame + "]]"),
				release: make(chan struct{}),
			}
			upstream := &regenUpstream{responses: []func() (*RPCResponse, error){gatedResponse(body)}}
			clock := &fakeFirstEventClock{window: time.Minute, restartOK: true}
			ctx := ContextWithFirstEventClock(context.Background(), clock)
			events, err := guardClient(t, "test", upstream).Generate(ctx, guardRequest(t, test.tools))
			if err != nil {
				t.Fatal(err)
			}
			select {
			case event := <-events:
				if test.hold {
					t.Fatalf("调用出现之前不应交出事件，收到 %s", event.Kind)
				}
				if event.Kind != EventReasoning {
					t.Fatalf("不扣住时首个事件应为思考，收到 %s", event.Kind)
				}
			case <-time.After(300 * time.Millisecond):
				if !test.hold {
					t.Fatal("不扣住时思考应立即交出")
				}
			}
			if pauses := clock.pauses.Load(); test.hold && pauses != 1 || !test.hold && pauses != 0 {
				t.Fatalf("首事件计时暂停 %d 次，扣住=%v", pauses, test.hold)
			}
			close(body.release)
			got := collectEvents(t, events)
			if test.hold {
				want := []EventKind{EventReasoning, EventText, EventToolCall, EventText, EventUsage, EventFinish}
				if fmt.Sprint(eventKinds(got)) != fmt.Sprint(want) || got[1].Text != "plain answer" || got[3].Text != "after call" {
					t.Fatalf("事件 = %v，期望 %v（扣住的事件按原顺序在调用之前交出）", eventKinds(got), want)
				}
			}
			if upstream.calls() != 1 {
				t.Fatalf("满足约束时只发送 1 次，实际 %d", upstream.calls())
			}
		})
	}
}

// TestToolGuardSatisfiedSequenceUnchanged 第一次就正确调用时事件序列与不带工具约束的请求相同（必须调用时只是扣住到调用出现）；
// 不带约束的请求的请求体与原来相同（没有工具要求）
func TestToolGuardSatisfiedSequenceUnchanged(t *testing.T) {
	stream := regenStream("", guardThoughtFrame, guardTextFrame, guardCallFrame, guardAfterText, regenStopFrame)
	run := func(request GenerateRequest) ([]Event, string) {
		upstream := &regenUpstream{responses: []func() (*RPCResponse, error){streamResponse(stream)}}
		events, err := guardClient(t, "test", upstream).Generate(context.Background(), request)
		if err != nil {
			t.Fatal(err)
		}
		got := collectEvents(t, events)
		if upstream.calls() != 1 {
			t.Fatalf("应只发送 1 次，实际 %d", upstream.calls())
		}
		return got, upstream.bodies[0]
	}
	plain, plainBody := run(regenRequest("test"))
	for _, tools := range []Tools{guardTools("required", guardFunction, false), guardTools("auto", "", true), guardTools("required", "", true)} {
		guarded, _ := run(guardRequest(t, tools))
		if fmt.Sprint(eventKinds(guarded)) != fmt.Sprint(eventKinds(plain)) {
			t.Fatalf("满足约束时事件 = %v，期望与不带约束相同 %v", eventKinds(guarded), eventKinds(plain))
		}
		for index := range plain {
			if plain[index].Text != guarded[index].Text {
				t.Fatalf("第 %d 个事件正文 %q，期望 %q", index, guarded[index].Text, plain[index].Text)
			}
		}
	}
	if strings.Contains(plainBody, "Use one of these tools") || strings.Contains(plainBody, "You must call") {
		t.Fatal("不带工具约束的请求不应带工具要求")
	}
}

// TestToolGuardHoldEnablesStreamRegeneration 必须调用时扣住的正文没有交出，之后流中断仍按批次 B 在同账号重新生成（原样重发）
func TestToolGuardHoldEnablesStreamRegeneration(t *testing.T) {
	upstream := &regenUpstream{responses: []func() (*RPCResponse, error){
		interruptedResponse(`[[` + guardTextFrame + `,`),
		streamResponse(regenStream("", guardCallFrame, regenStopFrame)),
	}}
	notes := &noteRecorder{}
	events, err := guardClient(t, "test", upstream).Generate(notes.context(context.Background()), guardRequest(t, guardTools("required", guardFunction, false)))
	if err != nil {
		t.Fatal(err)
	}
	got := collectEvents(t, events)
	if kinds := eventKinds(got); len(got) != 3 || kinds[0] != EventToolCall || guardHasText(got, "plain answer") {
		t.Fatalf("事件 = %v，期望只交出重新生成后的调用", kinds)
	}
	if upstream.calls() != 2 || notes.count("客户端还没收到内容，同账号重新生成 | 第 1/2 次 | 流中断") != 1 || upstream.bodies[0] != upstream.bodies[1] {
		t.Fatalf("发送 %d 次，请求说明 = %v，期望按流中断原样重发", upstream.calls(), notes.notes)
	}
}

// TestToolGuardStopSequenceBeforeCall 命中停止序列时同样按结束核对：必须调用却还没调用为违约，重新生成
func TestToolGuardStopSequenceBeforeCall(t *testing.T) {
	upstream := &regenUpstream{responses: []func() (*RPCResponse, error){
		streamResponse(regenStream("", guardTextFrame, regenStopFrame)),
		streamResponse(regenStream("", guardCallFrame, regenStopFrame)),
	}}
	notes := &noteRecorder{}
	request := guardRequest(t, guardTools("required", guardFunction, false))
	request.Config.StopSequences = []string{"answer"}
	events, err := guardClient(t, "test", upstream).Generate(notes.context(context.Background()), request)
	if err != nil {
		t.Fatal(err)
	}
	got := collectEvents(t, events)
	if kinds := eventKinds(got); len(got) != 3 || kinds[0] != EventToolCall || guardHasText(got, "plain") {
		t.Fatalf("事件 = %v，期望重新生成后的调用", kinds)
	}
	if notes.count("（命中停止序列） | 第 1/2 次") != 1 {
		t.Fatalf("请求说明 = %v", notes.notes)
	}
}

// guardLongRequest 为 long- 请求加上必须调用 Kei_Workflow 的工具约束
func guardLongRequest(t *testing.T, model string) GenerateRequest {
	t.Helper()
	request := longRequest(model, true)
	request.Tools = guardTools("required", guardFunction, false)
	prepared, _, err := prepareToolRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	return prepared
}

// TestToolGuardLongOutput long- 自动续写与必须调用：第一轮没调用就结束时同账号重新生成（被放弃的第一轮不报告结束，重发仍按续写计划编码）；
// 第一轮要求续写时扣住的正文跟到第二轮，第二轮调用后按顺序交出；续写发了第二轮之后仍没调用时不重新生成，按违约结束
func TestToolGuardLongOutput(t *testing.T) {
	model, defaults := longArgonModel()
	model.Capabilities["function_declarations"] = true
	stop := longFinishFrame(1, "", 10, 5, 15)
	call := `[[[[[[null,null,null,null,null,null,null,null,null,null,["` + guardFunction + `",[[["task",[null,null,"go"]]]]]]],"model"]]]]`
	t.Run("第一轮重新生成", func(t *testing.T) {
		upstream := &longUpstream{streams: []string{longStream(longTextFrame("只回复文字", false), stop), longStream(call, stop)}}
		recorder := &noteRecorder{}
		ctx, budget := regenerationTestBudget(recorder.context(context.Background()))
		summary, calls, progress := runLong(t, longClient(t, upstream, model, defaults), upstream, ctx, guardLongRequest(t, model.ID))
		if len(calls) != 2 || summary.tools != 1 || summary.text != "" || len(summary.errs) != 0 || budget.Used() != 1 {
			t.Fatalf("调用=%d 工具=%d 正文=%q 错误=%v 重新生成=%d", len(calls), summary.tools, summary.text, summary.errs, budget.Used())
		}
		if ends := roundEnds(progress); len(ends) != 1 || ends[0].Next {
			t.Fatalf("报告的各轮结束 = %+v，期望只报告重新生成后的那一轮", ends)
		}
		for _, sent := range calls {
			if config, _ := sent.body[3].([]any); len(config) < 46 || config[45] != float64(1) {
				t.Fatal("重发的第一轮仍应按官网续写编码")
			}
		}
		if recorder.count("工具约束未满足，同账号重新生成 | 原因=要求调用工具 Kei_Workflow") != 1 {
			t.Fatalf("请求说明 = %v", recorder.notes)
		}
	})
	t.Run("续写到第二轮调用", func(t *testing.T) {
		upstream := &longUpstream{streams: []string{
			longStream(longTextFrame("第一轮", false), longFinishFrame(22, "tok-1", 10, 100, 110)),
			longStream(longTextFrame("第二轮", false), call, stop),
		}}
		ctx, budget := regenerationTestBudget(context.Background())
		summary, calls, _ := runLong(t, longClient(t, upstream, model, defaults), upstream, ctx, guardLongRequest(t, model.ID))
		if len(calls) != 2 || summary.text != "第一轮第二轮" || summary.tools != 1 || len(summary.errs) != 0 || budget.Used() != 0 {
			t.Fatalf("调用=%d 正文=%q 工具=%d 错误=%v 重新生成=%d", len(calls), summary.text, summary.tools, summary.errs, budget.Used())
		}
	})
	t.Run("第二轮仍没调用", func(t *testing.T) {
		upstream := &longUpstream{streams: []string{
			longStream(longTextFrame("第一轮", false), longFinishFrame(22, "tok-1", 10, 100, 110)),
			longStream(longTextFrame("第二轮", false), stop),
			longStream(call, stop),
		}}
		recorder := &noteRecorder{}
		ctx, budget := regenerationTestBudget(recorder.context(context.Background()))
		summary, calls, _ := runLong(t, longClient(t, upstream, model, defaults), upstream, ctx, guardLongRequest(t, model.ID))
		var violation *ToolContractError
		if len(calls) != 2 || summary.text != "" || len(summary.errs) != 1 || !errors.As(summary.errs[0], &violation) || budget.Used() != 0 {
			t.Fatalf("调用=%d 正文=%q 错误=%v 重新生成=%d，期望第二轮之后按违约结束", len(calls), summary.text, summary.errs, budget.Used())
		}
		if recorder.count("工具约束未满足，long- 续写已发第二轮，不重新生成") != 1 {
			t.Fatalf("请求说明 = %v", recorder.notes)
		}
	})
}

// TestToolGuardBuildForward Build 通道与 Interaction 通道照旧经 forward 核对：契约不扣住、不重新生成（核对本身见 TestToolContractForward），
// 这里确认 Playground 之外的通道没有建立扣住状态
func TestToolGuardBuildForward(t *testing.T) {
	if guard := newToolGuard(nil); guard != nil || guard.holds() {
		t.Fatal("没有契约时不应扣住")
	}
	request := guardRequest(t, guardTools("required", guardFunction, false))
	source := make(chan Event, 3)
	source <- Event{Kind: EventText, Text: "plain answer"}
	source <- Event{Kind: EventFinish, FinishReason: "stop"}
	close(source)
	var got []Event
	for event := range request.toolContract.forward(context.Background(), source) {
		got = append(got, event)
	}
	if len(got) != 2 || got[0].Kind != EventText || guardContractError(got) == nil || guardContractError(got).Kind != ToolContractMissingCall {
		t.Fatalf("forward 应照旧先转发正文、结束时报违约: %v", eventKinds(got))
	}
}

// guardBuildStream 把 GenerateContentResponse JSON 拼成 Build 代理的流式响应（ProxyStreamedCall：每项 field 1 为一段响应 JSON）
func guardBuildStream(t *testing.T, chunks ...string) string {
	t.Helper()
	items := make([]string, 0, len(chunks))
	for _, chunk := range chunks {
		encoded, err := json.Marshal(chunk)
		if err != nil {
			t.Fatal(err)
		}
		items = append(items, "["+string(encoded)+"]")
	}
	return "[[" + strings.Join(items, ",") + "]]"
}

// guardSingleSend 确认这次 Generate 只发送 1 次、没有扣住也没有重新生成
func guardSingleSend(t *testing.T, upstream *regenUpstream, notes *noteRecorder) {
	t.Helper()
	if upstream.calls() != 1 {
		t.Fatalf("应只发送 1 次，实际 %d 次", upstream.calls())
	}
	if notes.count("扣住到调用出现") != 0 || notes.count("同账号重新生成") != 0 || notes.count("工具约束未满足") != 0 {
		t.Fatalf("不应扣住或重新生成：%v", notes.notes)
	}
}

// TestToolGuardBuildChannelGenerate 经 Client.Generate 走 Build 通道（租约为 ChannelBuild）：契约照旧在事件流上核对（forward），
// 正文照常先交出，只回复文字时以 *ToolContractError 结束，strict 参数不符时在调用处结束；只发送 1 次，不扣住、不重新生成
func TestToolGuardBuildChannelGenerate(t *testing.T) {
	const (
		textChunk = `{"candidates":[{"content":{"role":"model","parts":[{"text":"plain answer"}]}}]}`
		stopChunk = `{"candidates":[{"content":{"role":"model","parts":[{"text":""}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":2,"totalTokenCount":5}}`
		badCall   = `{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"Kei_Workflow","args":{"town":"Paris"}}}]}}]}`
	)
	for _, test := range []struct {
		name   string
		tools  Tools
		chunks []string
		kind   ToolContractViolation
	}{
		{name: "必须调用却只回复文字", tools: guardTools("required", guardFunction, false), chunks: []string{textChunk, stopChunk}, kind: ToolContractMissingCall},
		{name: "strict 参数不符", tools: guardTools("auto", "", true), chunks: []string{textChunk, badCall, stopChunk}, kind: ToolContractSchemaMismatch},
	} {
		t.Run(test.name, func(t *testing.T) {
			upstream := &regenUpstream{responses: []func() (*RPCResponse, error){
				streamResponse(guardBuildStream(t, test.chunks...)),
				streamResponse(guardBuildStream(t, textChunk, stopChunk)),
			}}
			notes := &noteRecorder{}
			ctx := ContextWithRegenerationBudget(notes.context(context.Background()), NewRegenerationBudget())
			ctx = ContextWithAccountLease(ctx, &AccountLease{channel: ChannelBuild, account: &Account{ID: "alice@example.com"}})
			events, err := guardClient(t, "test", upstream).Generate(ctx, guardRequest(t, test.tools))
			if err != nil {
				t.Fatal(err)
			}
			got := collectEvents(t, events)
			violation := guardContractError(got)
			if len(got) < 2 || got[0].Kind != EventText || got[0].Text != "plain answer" || violation == nil || violation.Kind != test.kind ||
				got[len(got)-1].Kind != EventError {
				t.Fatalf("事件 = %v 违约 = %+v 内容 = %+v，期望先交出正文、以 %s 违约结束", eventKinds(got), violation, got, test.kind)
			}
			guardSingleSend(t, upstream, notes)
			// 经 Build 代理发送，按 functionCallingConfig 原生执行（必须调用为 ANY，strict 为 VALIDATED）
			if !strings.HasPrefix(upstream.bodies[0], `["/v1beta/models/test:streamGenerateContent"`) ||
				!strings.Contains(upstream.bodies[0], `\"functionCallingConfig\":{`) {
				t.Fatalf("应经 Build 代理发送：%.300s", upstream.bodies[0])
			}
		})
	}
}

// TestToolGuardInteractionChannelGenerate 经 Client.Generate 走 Interaction 通道（CreateInteractionStream）：契约照旧在事件流上核对，
// 只要求内置工具（google_search）而上游只回复文字时正文照常先交出、结束时以 *ToolContractError 结束；只发送 1 次，不扣住、不重新生成
func TestToolGuardInteractionChannelGenerate(t *testing.T) {
	upstream := &regenUpstream{responses: []func() (*RPCResponse, error){
		streamResponse(string(interactionTestStream(
			interactionDeltaEvent([]any{[]any{"plain answer"}}),
			interactionFinal(3, []any{1, nil, nil, nil, 2, nil, nil, nil, 0, 3}),
		))),
	}}
	client, err := NewClient(ClientOptions{Transport: ceilingTransport{}, Protected: upstream})
	if err != nil {
		t.Fatal(err)
	}
	model := Model{
		ID: "gemini-interaction-text", Methods: []string{"generateContent"},
		Capabilities: map[string]bool{"interactions_api": true, "thinking_level": true, "google_search": true},
	}
	client.catalogs["alice@example.com"] = modelCatalog{models: []Model{model}, entries: map[string]modelEntry{model.ID: {model: model, defaults: omniTestDefaults()}}}
	request := regenRequest(model.ID)
	request.Tools = Tools{GoogleSearch: &GoogleSearchOptions{}, ToolConfig: ToolConfig{Mode: "required"}}
	prepared, contract, err := prepareToolRequest(request)
	if err != nil || contract == nil || !contract.required {
		t.Fatalf("prepareToolRequest: contract=%+v err=%v", contract, err)
	}
	notes := &noteRecorder{}
	ctx := ContextWithRegenerationBudget(notes.context(context.Background()), NewRegenerationBudget())
	events, err := client.Generate(ctx, prepared)
	if err != nil {
		t.Fatal(err)
	}
	got := collectEvents(t, events)
	violation := guardContractError(got)
	if len(got) < 2 || got[0].Kind != EventText || got[0].Text != "plain answer" || violation == nil || violation.Kind != ToolContractMissingCall ||
		got[len(got)-1].Kind != EventError {
		t.Fatalf("事件 = %v 违约 = %+v，期望先交出正文、结束时报没有调用", eventKinds(got), violation)
	}
	guardSingleSend(t, upstream, notes)
}

// TestToolContractPublicMessages 对外说明写明违约种类，不含会让中转自动禁用渠道的字样
func TestToolContractPublicMessages(t *testing.T) {
	for _, err := range []*ToolContractError{
		{Kind: ToolContractMissingCall, Required: []string{"A", "google_search"}, Attempts: 2},
		{Kind: ToolContractMissingCall, Required: []string{"A"}, SingleFunction: true},
		{Kind: ToolContractUnselectedFunction},
		{Kind: ToolContractSchemaMismatch, Function: "A", Attempts: 3},
		{Detail: "x"},
	} {
		message := strings.ToLower(err.PublicMessage())
		for _, word := range []string{"permission", "denied", "quota", "insufficient", "unauthorized", "forbidden", "api key"} {
			if strings.Contains(message, word) {
				t.Fatalf("说明 %q 含 %q", message, word)
			}
		}
		if !strings.HasSuffix(message, "please retry.") {
			t.Fatalf("说明 %q", message)
		}
	}
	for _, test := range []struct {
		err  *ToolContractError
		want string
	}{
		{&ToolContractError{Kind: ToolContractMissingCall, Required: []string{"A"}, SingleFunction: true, Blocked: "safety"},
			"The model stopped (safety) without calling the required function A."},
		{&ToolContractError{Kind: ToolContractMissingCall, Required: []string{"A", "google_search"}, Blocked: "recitation", Attempts: 2},
			"The model stopped (recitation) without calling any of the required tools (A, google_search) (tried 2 times)."},
		{&ToolContractError{Kind: ToolContractMissingCall, Blocked: "prohibited_content"},
			"The model stopped (prohibited_content) without calling a required tool."},
	} {
		if got := test.err.PublicMessage(); got != test.want {
			t.Fatalf("内容拦截的说明 = %q，期望 %q", got, test.want)
		}
	}
	if got := (&ToolContractError{Kind: ToolContractMissingCall, Required: []string{"A", "google_search"}, Attempts: 2}).PublicMessage(); got !=
		"The model did not call any of the required tools (A, google_search) (tried 2 times). Please retry." {
		t.Fatalf("说明 = %q", got)
	}
	if (&ToolContractError{Detail: "x"}).PublicMessage() != ToolContractPublicMessage {
		t.Fatal("没有违约种类时用笼统的说明")
	}
	// 客户端声明的函数名含会让中转禁用渠道的单词（check_quota、get_balance 等）：说明里不写名字，改用不带名字的写法
	for _, test := range []struct {
		err  *ToolContractError
		want string
	}{
		{&ToolContractError{Kind: ToolContractMissingCall, Required: []string{"check_quota"}, SingleFunction: true, Attempts: 3},
			"The model did not call the required function (tried 3 times). Please retry."},
		{&ToolContractError{Kind: ToolContractMissingCall, Required: []string{"Kei_Workflow", "getBalance"}, Attempts: 2},
			"The model did not call any of the required tools (tried 2 times). Please retry."},
		{&ToolContractError{Kind: ToolContractMissingCall, Required: []string{"get-credit-score"}, SingleFunction: true, Blocked: "safety"},
			"The model stopped (safety) without calling the required function."},
		{&ToolContractError{Kind: ToolContractMissingCall, Required: []string{"insufficient_stock_alert", "google_search"}, Blocked: "spii"},
			"The model stopped (spii) without calling any of the required tools."},
		{&ToolContractError{Kind: ToolContractSchemaMismatch, Function: "Permission_Denied_Report", Attempts: 3},
			"The model's function arguments did not match the strict schema (tried 3 times). Please retry."},
		{&ToolContractError{Kind: ToolContractSchemaMismatch, Function: "billing.lookup"},
			"The model's function arguments did not match the strict schema. Please retry."},
		{&ToolContractError{Kind: ToolContractSchemaMismatch, Function: "rotate_API_KEY"},
			"The model's function arguments did not match the strict schema. Please retry."},
	} {
		got := test.err.PublicMessage()
		if got != test.want {
			t.Fatalf("说明 = %q，期望 %q", got, test.want)
		}
		lower := strings.ToLower(got)
		for _, word := range []string{"permission", "denied", "quota", "insufficient", "credit", "balance", "billing", "api_key", "api key"} {
			if strings.Contains(lower, word) {
				t.Fatalf("说明 %q 含 %q", got, word)
			}
		}
	}
	// 普通函数名照常写出
	for _, name := range []string{"Kei_Workflow", "get_weather", "search_docs", "edit_file"} {
		if publicToolName(name) != name {
			t.Fatalf("%q 应照常写进说明", name)
		}
	}
}

// delayedGuardResponse 等 delay 之后才返回响应头与整段流（上游先排队、过一会儿才出首个事件）
func delayedGuardResponse(delay time.Duration, body string) func() (*RPCResponse, error) {
	return func() (*RPCResponse, error) {
		time.Sleep(delay)
		return streamResponse(body)()
	}
}

// TestToolGuardFirstEventWindowUsesHeldDelay 开启首事件超时时，扣住过事件的这次按“到扣住第一个事件的耗时”判断首事件超时够不够：
// 首个事件立即到达并被扣住、整段较长（超过首事件超时）却只回复文字时照常重新生成；到扣住第一个事件就已超过首事件超时的 1/1.2 时
// 不重新生成，说明写明是到扣住首个事件的耗时
func TestToolGuardFirstEventWindowUsesHeldDelay(t *testing.T) {
	t.Run("首事件很快、整段较长", func(t *testing.T) {
		body := &gatedBody{
			head:    strings.NewReader("[[" + guardThoughtFrame + ","),
			tail:    strings.NewReader(guardTextFrame + "," + regenStopFrame + "]]"),
			release: make(chan struct{}),
		}
		time.AfterFunc(250*time.Millisecond, func() { _ = body.Close() })
		upstream := &regenUpstream{responses: []func() (*RPCResponse, error){
			gatedResponse(body),
			streamResponse(regenStream("", guardCallFrame, regenStopFrame)),
		}}
		notes := &noteRecorder{}
		clock := &fakeFirstEventClock{window: 150 * time.Millisecond, restartOK: true}
		budget := NewRegenerationBudget()
		ctx := ContextWithFirstEventClock(ContextWithRegenerationBudget(notes.context(context.Background()), budget), clock)
		events, err := guardClient(t, "test", upstream).Generate(ctx, guardRequest(t, guardTools("required", guardFunction, false)))
		if err != nil {
			t.Fatal(err)
		}
		got := collectEvents(t, events)
		if kinds := eventKinds(got); len(got) != 3 || kinds[0] != EventToolCall || guardHasText(got, "plain answer") {
			t.Fatalf("事件 = %v，期望重新生成后的调用", kinds)
		}
		if upstream.calls() != 2 || budget.Used() != 1 || clock.pauses.Load() != 1 || clock.restarts.Load() != 1 {
			t.Fatalf("发送 %d 次、重新生成 %d 次、暂停 %d 次、重新起算 %d 次，请求说明 = %v",
				upstream.calls(), budget.Used(), clock.pauses.Load(), clock.restarts.Load(), notes.notes)
		}
		if notes.count("首事件超时不到") != 0 || notes.count("工具约束未满足，同账号重新生成 | 原因=要求调用工具 Kei_Workflow") != 1 {
			t.Fatalf("请求说明 = %v", notes.notes)
		}
	})
	t.Run("等首事件就已不够", func(t *testing.T) {
		upstream := &regenUpstream{responses: []func() (*RPCResponse, error){
			delayedGuardResponse(200*time.Millisecond, regenStream("", guardTextFrame, regenStopFrame)),
			streamResponse(regenStream("", guardCallFrame, regenStopFrame)),
		}}
		notes := &noteRecorder{}
		clock := &fakeFirstEventClock{window: 200 * time.Millisecond, restartOK: true}
		budget := NewRegenerationBudget()
		ctx := ContextWithFirstEventClock(ContextWithRegenerationBudget(notes.context(context.Background()), budget), clock)
		events, err := guardClient(t, "test", upstream).Generate(ctx, guardRequest(t, guardTools("required", guardFunction, false)))
		if err != nil {
			t.Fatal(err)
		}
		got := collectEvents(t, events)
		if violation := guardContractError(got); len(got) != 1 || violation == nil || violation.Attempts != 1 {
			t.Fatalf("事件 = %v，期望只有违约", eventKinds(got))
		}
		if upstream.calls() != 1 || budget.Used() != 0 ||
			notes.count("工具约束未满足，首事件超时不到本次到扣住首个事件的耗时（") != 1 || notes.count("的 1.2 倍（重发多半被首事件计时打断），不重新生成 | FIRST_EVENT_TIMEOUT=200ms") != 1 {
			t.Fatalf("发送 %d 次、重新生成 %d 次，请求说明 = %v", upstream.calls(), budget.Used(), notes.notes)
		}
	})
}

// slowInterruptedBody 先交出 head，过 delay 之后以连接被重置结束（扣住的正文之后流中断）
type slowInterruptedBody struct {
	head   *strings.Reader
	delay  time.Duration
	waited bool
}

func (body *slowInterruptedBody) Read(buffer []byte) (int, error) {
	if body.head.Len() > 0 {
		return body.head.Read(buffer)
	}
	if !body.waited {
		body.waited = true
		time.Sleep(body.delay)
	}
	return 0, syscall.ECONNRESET
}

func (body *slowInterruptedBody) Close() error { return nil }

// TestToolGuardHeldStreamRegenerationFirstEventWindow 必须调用时扣住的正文之后流中断（批次 B 的重新生成）：首事件超时同样按到扣住
// 第一个事件的耗时判断，整段超过首事件超时、首个事件却很快时照常原样重发
func TestToolGuardHeldStreamRegenerationFirstEventWindow(t *testing.T) {
	upstream := &regenUpstream{responses: []func() (*RPCResponse, error){
		func() (*RPCResponse, error) {
			return &RPCResponse{
				StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {JSONProtobufContentType}},
				Body: &slowInterruptedBody{head: strings.NewReader("[[" + guardTextFrame + ","), delay: 250 * time.Millisecond},
			}, nil
		},
		streamResponse(regenStream("", guardCallFrame, regenStopFrame)),
	}}
	notes := &noteRecorder{}
	clock := &fakeFirstEventClock{window: 150 * time.Millisecond, restartOK: true}
	ctx := ContextWithFirstEventClock(notes.context(context.Background()), clock)
	events, err := guardClient(t, "test", upstream).Generate(ctx, guardRequest(t, guardTools("required", guardFunction, false)))
	if err != nil {
		t.Fatal(err)
	}
	got := collectEvents(t, events)
	if kinds := eventKinds(got); len(got) != 3 || kinds[0] != EventToolCall || guardHasText(got, "plain answer") {
		t.Fatalf("事件 = %v，期望重新生成后的调用", kinds)
	}
	if upstream.calls() != 2 || notes.count("客户端还没收到内容，同账号重新生成 | 第 1/2 次 | 流中断") != 1 || notes.count("首事件超时不到") != 0 {
		t.Fatalf("发送 %d 次，请求说明 = %v", upstream.calls(), notes.notes)
	}
}

// TestToolGuardRemainingDeadlineShort 请求期限的剩余不到本次整段耗时的 1.2 倍时不重新生成：只发送 1 次、以违约结束，
// 请求说明写明剩余期限不足，重新生成次数没有被占用
func TestToolGuardRemainingDeadlineShort(t *testing.T) {
	upstream := &regenUpstream{responses: []func() (*RPCResponse, error){
		delayedGuardResponse(300*time.Millisecond, regenStream("", guardTextFrame, regenStopFrame)),
		streamResponse(regenStream("", guardCallFrame, regenStopFrame)),
	}}
	notes := &noteRecorder{}
	budget := NewRegenerationBudget()
	ctx, cancel := context.WithTimeout(ContextWithRegenerationBudget(notes.context(context.Background()), budget), 500*time.Millisecond)
	defer cancel()
	events, err := guardClient(t, "test", upstream).Generate(ctx, guardRequest(t, guardTools("required", guardFunction, false)))
	if err != nil {
		t.Fatal(err)
	}
	got := collectEvents(t, events)
	if violation := guardContractError(got); len(got) != 1 || violation == nil || violation.Kind != ToolContractMissingCall || violation.Attempts != 1 {
		t.Fatalf("事件 = %v，期望只有违约", eventKinds(got))
	}
	if upstream.calls() != 1 || budget.Used() != 0 ||
		notes.count("工具约束未满足，剩余期限不足（不到本次耗时的 1.2 倍），不重新生成 | 剩余=") != 1 {
		t.Fatalf("发送 %d 次、重新生成 %d 次，请求说明 = %v", upstream.calls(), budget.Used(), notes.notes)
	}
}

// TestToolGuardContentBlockedFinish 必须调用时上游以内容拦截类原因结束（safety 等）：加强工具要求对它基本没有作用，不重新生成、
// 不占次数，对外说明写出结束原因（不写 Please retry）；max_tokens 等其他结束原因照常重新生成
func TestToolGuardContentBlockedFinish(t *testing.T) {
	safetyFrame := `[[[null,3]]]`
	upstream := &regenUpstream{responses: []func() (*RPCResponse, error){
		streamResponse(regenStream("", guardTextFrame, safetyFrame)),
		streamResponse(regenStream("", guardCallFrame, regenStopFrame)),
	}}
	notes := &noteRecorder{}
	budget := NewRegenerationBudget()
	ctx := ContextWithRegenerationBudget(notes.context(context.Background()), budget)
	events, err := guardClient(t, "test", upstream).Generate(ctx, guardRequest(t, guardTools("required", guardFunction, false)))
	if err != nil {
		t.Fatal(err)
	}
	got := collectEvents(t, events)
	violation := guardContractError(got)
	if len(got) != 1 || violation == nil || violation.Blocked != "safety" || guardHasText(got, "plain answer") {
		t.Fatalf("事件 = %v 违约=%+v，期望只有写明内容拦截的违约", eventKinds(got), violation)
	}
	if message := violation.PublicMessage(); message != "The model stopped (safety) without calling the required function Kei_Workflow." {
		t.Fatalf("对外说明 = %q", message)
	}
	if upstream.calls() != 1 || budget.Used() != 0 ||
		notes.count("工具约束未满足，上游以内容拦截结束（结束原因=safety），加强工具要求多半没有作用，不重新生成 | 原因=要求调用工具 Kei_Workflow") != 1 {
		t.Fatalf("发送 %d 次、重新生成 %d 次，请求说明 = %v", upstream.calls(), budget.Used(), notes.notes)
	}
	maxTokens := &regenUpstream{responses: []func() (*RPCResponse, error){
		streamResponse(regenStream("", guardTextFrame, `[[[null,2]]]`)),
		streamResponse(regenStream("", guardCallFrame, regenStopFrame)),
	}}
	notes = &noteRecorder{}
	events, err = guardClient(t, "test", maxTokens).Generate(notes.context(context.Background()), guardRequest(t, guardTools("required", guardFunction, false)))
	if err != nil {
		t.Fatal(err)
	}
	if got := collectEvents(t, events); len(got) != 3 || got[0].Kind != EventToolCall || maxTokens.calls() != 2 ||
		notes.count("工具约束未满足，同账号重新生成 | 原因=要求调用工具 Kei_Workflow，上游没有返回工具调用（结束原因=max_tokens）") != 1 {
		t.Fatalf("max_tokens 时应照常重新生成：事件 %v，发送 %d 次，请求说明 = %v", eventKinds(got), maxTokens.calls(), notes.notes)
	}
}

// TestToolHintsBuiltinOnly 只要求内置工具（只声明了 google_search 的必须调用）时，加强的要求不把它称为函数、不要求只回复调用
// （模型照做会输出同名的函数调用、被判为调用了未选择的函数）；唯一要求的是声明的函数时照旧按函数写
func TestToolHintsBuiltinOnly(t *testing.T) {
	request := regenRequest("test")
	request.Tools = Tools{GoogleSearch: &GoogleSearchOptions{}, ToolConfig: ToolConfig{Mode: "required"}}
	prepared, contract, err := prepareToolRequest(request)
	if err != nil || contract == nil || !contract.required || len(contract.functionNames) != 0 {
		t.Fatalf("prepareToolRequest: contract=%+v err=%v", contract, err)
	}
	for level := 1; level <= 2; level++ {
		hints, requirement := contract.toolHintsFor(level)
		joined := strings.Join(hints, "\n")
		if requirement != "You must use the tool google_search in this response before answering. Do not answer from memory." {
			t.Fatalf("第 %d 级要求 = %q", level, requirement)
		}
		if strings.Contains(joined, "function") || strings.Contains(joined, "schema") || strings.Contains(joined, "call only") {
			t.Fatalf("第 %d 级只有内置工具时不应写成函数：%q", level, joined)
		}
	}
	guard := newToolGuard(contract)
	guard.strengthen()
	guard.strengthen()
	rewritten := guard.apply(prepared)
	if !strings.Contains(rewritten.System, "Base the answer on the tool results.") ||
		!strings.Contains(rewritten.Contents[len(rewritten.Contents)-1].Parts[0].Text, "You must use the tool google_search") {
		t.Fatalf("第二级应加一句并附到用户消息：system=%q", rewritten.System)
	}
	if violation := contract.missingCall(""); violation.SingleFunction ||
		violation.PublicMessage() != "The model did not call any of the required tools (google_search). Please retry." {
		t.Fatalf("只有内置工具时的违约 = %+v %q", violation, violation.PublicMessage())
	}
	single := guardRequest(t, guardTools("required", guardFunction, false)).toolContract
	if _, requirement := single.toolHintsFor(1); requirement != guardStrongHint || !single.singleFunction() {
		t.Fatalf("唯一要求的是声明的函数时要求 = %q", requirement)
	}
}

var _ io.ReadCloser = (*gatedBody)(nil)
