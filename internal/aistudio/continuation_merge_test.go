package aistudio

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// long- 自动续写与批次 B（原生续写、重新生成、同租约重发的准入、首事件计时、流尾错误）合并后的共用规则测试

// longEndsWithUser 判断请求以补上的那条“接着写”用户消息结尾
func longEndsWithUser(request GenerateRequest) bool {
	count := len(request.Contents)
	return count > 0 && request.Contents[count-1].Role == RoleUser && len(request.Contents[count-1].Parts) > 0 &&
		request.Contents[count-1].Parts[0].Text == PrefillContinuationPrompt
}

// longEndsWithModel 判断请求以模型轮结尾（原生预填充）
func longEndsWithModel(request GenerateRequest) bool {
	count := len(request.Contents)
	return count > 0 && request.Contents[count-1].Role == RoleAssistant
}

// clientPrefillRequest 为客户端预填充的普通请求：历史以模型文字消息结尾、协议层按原生续写原样发送（PrefillTail）
func clientPrefillRequest(model string) GenerateRequest {
	return GenerateRequest{
		ID: "req-prefill", Model: model, AccountID: longTestAccount, PrefillTail: true,
		Contents: []Content{
			{Role: RoleUser, Parts: []Part{{Text: "写一首诗"}}},
			{Role: RoleAssistant, Parts: []Part{{Text: "床前明月光，"}}},
		},
	}
}

// TestContinuationPrefillSharedWithClientPrefill 模拟续写的原生预填充与客户端预填充共用 prefill.go 的名单、拒绝判断与按“模型 + 通道”的记忆：
// 续写第二轮被上游以 400 拒绝（说明用的是另一种写法 last turn，也按同一个判断识别）时在这一轮内改补用户消息重发、写一行与客户端预填充
// 相同写法的说明，之后同模型的普通客户端预填充请求直接补用户消息；反过来，客户端预填充被拒后，同模型的 long- 请求续写时直接补用户消息，
// 不再白试一次原生预填充
func TestContinuationPrefillSharedWithClientPrefill(t *testing.T) {
	model, defaults := longPlainModel("gemini-3.1-pro-preview")
	forgetPrefillRejection(t, model.ID)
	rejected := &RPCError{Method: "GenerateContent", StatusCode: http.StatusBadRequest, Code: 3, Message: "Please ensure that the last turn is a user turn"}
	upstream := &longUpstream{
		streams: []string{
			longStream(longTextFrame("第一段", false), longFinishFrame(2, "", 10, 100, 110)),
			"",
			longStream(longTextFrame("第二段", false), longFinishFrame(1, "", 10, 5, 15)),
			longStream(longTextFrame("疑是地上霜。", false), longFinishFrame(1, "", 10, 5, 15)),
		},
		errs: map[int]error{1: rejected},
	}
	client := longClient(t, upstream, model, defaults)
	notes := &noteRecorder{}
	request := longRequest(model.ID, true)
	request.LongOutputDefaultTokens = 300
	summary, calls, progress := runLong(t, client, upstream, notes.context(context.Background()), request)
	if len(calls) != 3 || summary.text != "第一段第二段" || len(summary.errs) != 0 {
		t.Fatalf("调用=%d 正文=%q 错误=%v", len(calls), summary.text, summary.errs)
	}
	if !longEndsWithModel(calls[1].request) || !longEndsWithUser(calls[2].request) {
		t.Fatalf("第二轮应先原生预填充、被拒后在这一轮内补用户消息重发: %+v / %+v", calls[1].request.Contents, calls[2].request.Contents)
	}
	for _, item := range progress {
		if item.Stage == ContinuationFailed {
			t.Fatalf("被拒后改补用户消息重发，不应按这一轮失败结束: %+v", item)
		}
	}
	if notes.count("自动续写第 2 轮：上游不接受以模型轮结尾的请求，改回在末尾补用户消息重试一次 | 模型=gemini-3.1-pro-preview | 通道=playground") != 1 {
		t.Fatalf("应写一行与客户端预填充被拒时相同写法的说明: %v", notes.notes)
	}
	if !prefillRecentlyRejected(ChannelPlayground, model.ID, time.Now()) {
		t.Fatal("续写轮被拒应记进与客户端预填充共用的记忆")
	}
	// 之后同模型的普通客户端预填充请求：直接补用户消息，只发一次
	summary, calls, _ = runLong(t, client, upstream, context.Background(), clientPrefillRequest(model.ID))
	if len(calls) != 4 || summary.text != "疑是地上霜。" || !longEndsWithUser(calls[3].request) {
		t.Fatalf("记住拒绝后客户端预填充应直接补用户消息: 调用=%d 正文=%q 内容=%+v", len(calls), summary.text, calls[len(calls)-1].request.Contents)
	}

	// 反过来：客户端预填充（long- 请求的第一轮）被拒、改回补用户消息后，同一请求的续写各轮直接补用户消息
	forgetPrefillRejection(t, model.ID)
	upstream = &longUpstream{
		streams: []string{
			"",
			longStream(longTextFrame("第一段", false), longFinishFrame(2, "", 10, 100, 110)),
			longStream(longTextFrame("第二段", false), longFinishFrame(1, "", 10, 5, 15)),
		},
		errs: map[int]error{0: &RPCError{Method: "GenerateContent", StatusCode: http.StatusBadRequest, Code: 3, Message: "Requests ending with a model turn are not supported"}},
	}
	client = longClient(t, upstream, model, defaults)
	request = clientPrefillRequest(model.ID)
	request.LongOutput, request.LongOutputDefaultTokens = true, 300
	summary, calls, progress = runLong(t, client, upstream, context.Background(), request)
	if len(calls) != 3 || summary.text != "第一段第二段" || len(summary.errs) != 0 {
		t.Fatalf("调用=%d 正文=%q 错误=%v", len(calls), summary.text, summary.errs)
	}
	if !longEndsWithModel(calls[0].request) || !longEndsWithUser(calls[1].request) || !longEndsWithUser(calls[2].request) {
		t.Fatalf("第一轮被拒改回补用户消息后，续写轮也应直接补用户消息: %+v", calls[2].request.Contents)
	}
	if ends := roundEnds(progress); len(ends) != 2 || ends[0].Prefill != ContinuationPrefillPrompt {
		t.Fatalf("续写计划应按共用的记忆补用户消息: %+v", ends)
	}
}

// TestContinuationPrefillTrailerRejection 续写第二轮原样以模型轮结尾、上游在流尾以 400 拒绝（这一轮没有任何输出）时，与发送时被拒一样
// 在这一轮内改补用户消息重发（不按这一轮失败结束、不丢后面的续写），并记进共用的记忆
func TestContinuationPrefillTrailerRejection(t *testing.T) {
	model, defaults := longPlainModel("gemini-3-flash-preview")
	forgetPrefillRejection(t, model.ID)
	upstream := &longUpstream{
		streams: []string{
			longStream(longTextFrame("第一段", false), longFinishFrame(2, "", 10, 100, 110)),
			regenStream(`[3,"Requests ending with a model turn are not supported"]`),
			longStream(longTextFrame("第二段", false), longFinishFrame(1, "", 10, 5, 15)),
		},
	}
	request := longRequest(model.ID, true)
	request.LongOutputDefaultTokens = 300
	summary, calls, progress := runLong(t, longClient(t, upstream, model, defaults), upstream, context.Background(), request)
	if len(calls) != 3 || summary.text != "第一段第二段" || len(summary.errs) != 0 || len(summary.finishes) != 1 || summary.finishes[0] != "stop" {
		t.Fatalf("调用=%d 正文=%q 错误=%v 结束=%v", len(calls), summary.text, summary.errs, summary.finishes)
	}
	if !longEndsWithModel(calls[1].request) || !longEndsWithUser(calls[2].request) {
		t.Fatalf("流尾被拒后应在这一轮内补用户消息重发: %+v", calls[2].request.Contents)
	}
	for _, item := range progress {
		if item.Stage == ContinuationFailed {
			t.Fatalf("流尾被拒不应按这一轮失败结束: %+v", item)
		}
	}
	if !prefillRecentlyRejected(ChannelPlayground, model.ID, time.Now()) {
		t.Fatal("流尾被拒应记进共用的记忆")
	}
}

// TestLongOutputFirstRoundPrefillRetryError long- 请求的第一轮是客户端预填充、被上游拒绝且本账号名额已满时，与普通请求一样交出
// *PrefillRetryError 由 app 层换号再试（续写的第二轮起被拒时在本轮内等号池准入后重发，见 TestPrefillRejectedResendWaitsWindow）
func TestLongOutputFirstRoundPrefillRetryError(t *testing.T) {
	pool := newRateTestPool(t, time.Minute, 1, 2, indexTestAccount(longTestAccount, BenefitTierFree, indexTestCatalog()))
	lease := mustTryAcquire(t, pool, AccountSelection{ModelID: indexTestModel, Method: "generateContent", AccountID: longTestAccount})
	defer func() { _ = lease.Release() }()
	recorder := &sendRecorder{t: t, pool: pool}
	model, defaults := longPlainModel("gemini-3-flash-preview")
	forgetPrefillRejection(t, model.ID)
	upstream := &longUpstream{
		errs: map[int]error{0: &RPCError{Method: "GenerateContent", StatusCode: http.StatusBadRequest, Code: 3, Message: "Requests ending with a model turn are not supported"}},
		gate: recorder.send,
	}
	request := clientPrefillRequest(model.ID)
	request.LongOutput, request.LongOutputDefaultTokens = true, 300
	_, err := longClient(t, upstream, model, defaults).Generate(ContextWithAccountLease(context.Background(), lease), request)
	var retry *PrefillRetryError
	if !errors.As(err, &retry) || !strings.Contains(retry.Denied, "每分钟名额 1/1") {
		t.Fatalf("第一轮被拒且名额已满时应交出 PrefillRetryError：%v", err)
	}
	if calls := upstream.snapshot(); len(calls) != 1 {
		t.Fatalf("不应在本账号重发：调用=%d", len(calls))
	}
}

// regenerationTestBudget 返回挂了重新生成次数与测试用换输入函数的上下文
func regenerationTestBudget(ctx context.Context) (context.Context, *RegenerationBudget) {
	budget := NewRegenerationBudget()
	return ContextWithRegenerationBudget(variantContext(ctx), budget), budget
}

// TestLongOutputRegenerationKeepsOfficialPlan 官网续写的第一轮还没交出任何事件就以函数调用格式错误结束时照常重新生成（换随机后缀）：
// 重发的第一轮仍按续写计划编码（generation config field 46 为 1、省略根 field 11），之后照常续写；合并用量只含交出的这几轮
// （被放弃那次的 777 不并进来），重新生成只占一次次数，续写轮数不占重新生成次数；被放弃那次不报告“第 1 轮结束”，
// 进度只有交出的三轮（从重新生成的那次算起第 1 轮）
func TestLongOutputRegenerationKeepsOfficialPlan(t *testing.T) {
	model, defaults := longArgonModel()
	upstream := &longUpstream{
		streams: []string{
			longStream(`[[[null,10,null,"Malformed function call: print(default_api.lookup(id=))"]],null,[5,777,782]]`),
			longStream(longTextFrame("第一轮", false), longFinishFrame(22, "tok-1", 10, 100, 110)),
			longStream(longTextFrame("第二轮", false), longFinishFrame(22, "tok-2", 20, 30, 50)),
			longStream(longTextFrame("第三轮", false), longFinishFrame(1, "", 30, 7, 37)),
		},
	}
	ctx, budget := regenerationTestBudget(context.Background())
	summary, calls, progress := runLong(t, longClient(t, upstream, model, defaults), upstream, ctx, longRequest(model.ID, true))
	if len(calls) != 4 || summary.text != "第一轮第二轮第三轮" || len(summary.errs) != 0 || len(summary.finishes) != 1 || summary.finishes[0] != "stop" {
		t.Fatalf("调用=%d 正文=%q 错误=%v 结束=%v", len(calls), summary.text, summary.errs, summary.finishes)
	}
	for index, call := range calls[:2] {
		config := wireConfig(t, call.body)
		if len(config) != 46 || config[45] != float64(1) || len(call.body) != 11 || call.body[10] != nil {
			t.Fatalf("第 %d 次发送（重新生成前后的第一轮）应按官网续写编码: config=%v body=%v", index+1, config, call.body)
		}
	}
	last := func(request GenerateRequest) string {
		parts := request.Contents[len(request.Contents)-1].Parts
		return parts[len(parts)-1].Text
	}
	if strings.Contains(last(calls[0].request), variantMarker) || !strings.Contains(last(calls[1].request), variantMarker) {
		t.Fatalf("因结束原因重新生成应换输入: %q / %q", last(calls[0].request), last(calls[1].request))
	}
	if len(calls[2].body) != 17 || calls[2].body[16] != "tok-1" || !strings.Contains(last(calls[2].request), variantMarker) {
		t.Fatalf("续写轮应接在重新生成的那次之后（换过的输入、续写 token）: body=%v", calls[2].body)
	}
	if len(summary.usages) != 1 || summary.usages[0].OutputTokens != 137 || summary.usages[0].InputTokens != 30 {
		t.Fatalf("合并用量 = %+v，期望只含交出的三轮（正文 100+30+7）", summary.usages)
	}
	if budget.Used() != 1 {
		t.Fatalf("重新生成次数 = %d，期望 1（续写轮数不占重新生成次数）", budget.Used())
	}
	if ends := roundEnds(progress); len(ends) != 3 || !ends[0].Next || ends[0].Round != 1 || !ends[1].Next || ends[2].Next || ends[2].Round != 3 {
		t.Fatalf("每轮进度 = %+v，期望被放弃那次不报告、交出的三轮各一条", ends)
	}
}

// TestLongOutputFirstRoundRegenerableEnd 第一轮就结束时“第 1 轮结束”进度只在这次结果不会被重新生成放弃时报告：还没交出任何事件
// （隐藏的思考不算）、又以函数调用格式错误结束时不报告（不论随后重新生成与否，不重新生成的原因另有请求说明）；已交出正文、
// 或没有内容但以其他原因结束时照常报告
func TestLongOutputFirstRoundRegenerableEnd(t *testing.T) {
	malformed := `[[[null,10,null,"Malformed function call"]],null,[5,0,5]]`
	stop := longFinishFrame(1, "", 10, 5, 15)
	for _, test := range []struct {
		name    string
		streams []string
		variant bool
		hide    bool
		// calls 为期望的发送次数，ends 为期望报告的各轮结束的结束原因，finish 为交给下游的结束原因，note 为期望的请求说明片段
		calls  int
		ends   []string
		finish string
		note   string
	}{
		{
			name: "没有可换的输入不重新生成", streams: []string{longStream(malformed)},
			calls: 1, finish: "malformed_function_call", note: "不因结束原因重新生成",
		},
		{
			name: "重新生成后按新结果报告", streams: []string{longStream(malformed), longStream(longTextFrame("正文", false), stop)}, variant: true,
			calls: 2, ends: []string{"stop"}, finish: "stop", note: "同账号重新生成",
		},
		{
			name: "只有隐藏的思考仍算没交出", streams: []string{longStream(longTextFrame("想一想", true), malformed), longStream(longTextFrame("正文", false), stop)},
			variant: true, hide: true, calls: 2, ends: []string{"stop"}, finish: "stop", note: "同账号重新生成",
		},
		{
			name: "已交出正文照常报告", streams: []string{longStream(longTextFrame("前半段", false), malformed)}, variant: true,
			calls: 1, ends: []string{"malformed_function_call"}, finish: "malformed_function_call",
		},
		{
			name: "没有内容但不能重新生成照常报告", streams: []string{longStream(stop)}, variant: true,
			calls: 1, ends: []string{"stop"}, finish: "stop",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			model, defaults := longArgonModel()
			upstream := &longUpstream{streams: test.streams}
			recorder := &noteRecorder{}
			ctx := ContextWithRegenerationBudget(recorder.context(context.Background()), NewRegenerationBudget())
			if test.variant {
				ctx = variantContext(ctx)
			}
			request := longRequest(model.ID, true)
			request.Config.HideThinking = test.hide
			summary, calls, progress := runLong(t, longClient(t, upstream, model, defaults), upstream, ctx, request)
			if len(calls) != test.calls || len(summary.errs) != 0 || len(summary.finishes) != 1 || summary.finishes[0] != test.finish {
				t.Fatalf("调用=%d 错误=%v 结束=%v，期望调用 %d 次、以 %s 结束", len(calls), summary.errs, summary.finishes, test.calls, test.finish)
			}
			ends := roundEnds(progress)
			var finishes []string
			for _, end := range ends {
				if end.Next || end.Round != 1 {
					t.Fatalf("只有一轮时不应续写: %+v", end)
				}
				finishes = append(finishes, end.RoundFinish)
			}
			if fmt.Sprint(finishes) != fmt.Sprint(test.ends) {
				t.Fatalf("报告的各轮结束 = %v，期望 %v", finishes, test.ends)
			}
			if test.note != "" && recorder.count(test.note) != 1 {
				t.Fatalf("请求说明 = %v，期望一条含 %q", recorder.notes, test.note)
			}
		})
	}
}

// TestLongOutputNoRegenerationAfterSecondRound 续写决定发第二轮之后不再重新生成：第一轮只有隐藏的思考（没有交出任何事件）就要求续写，
// 第二轮以函数调用格式错误结束时按上游结果结束，不从第一轮重来
func TestLongOutputNoRegenerationAfterSecondRound(t *testing.T) {
	model, defaults := longArgonModel()
	upstream := &longUpstream{
		streams: []string{
			longStream(longTextFrame("想一想", true), longFinishFrame(22, "tok-1", 10, 0, 60, 50)),
			longStream(`[[[null,10,null,"Malformed function call"]],null,[20,0,20]]`),
			longStream(longTextFrame("不应发出", false), longFinishFrame(1, "", 10, 5, 15)),
		},
	}
	request := longRequest(model.ID, true)
	request.Config.HideThinking = true
	ctx, budget := regenerationTestBudget(context.Background())
	summary, calls, _ := runLong(t, longClient(t, upstream, model, defaults), upstream, ctx, request)
	if len(calls) != 2 || len(summary.finishes) != 1 || summary.finishes[0] != "malformed_function_call" || budget.Used() != 0 {
		t.Fatalf("调用=%d 结束=%v 重新生成=%d，期望第二轮之后不再重新生成", len(calls), summary.finishes, budget.Used())
	}
}

// TestLongOutputRoundsKeepFirstEventClock 首事件计时：第一轮里的重新生成照批次 B 每次重发重新起算（重发前一次、开始向上游发送时一次），
// 续写的后续轮次不重新起算，且在原始上下文上发送（请求阶段照常报给生成服务的阶段观察者）
func TestLongOutputRoundsKeepFirstEventClock(t *testing.T) {
	model, defaults := longArgonModel()
	var sending atomic.Int32
	upstream := &longUpstream{
		streams: []string{
			longStream(`[[[null,10,null,"Malformed function call"]]]`),
			longStream(longTextFrame("第一轮", false), longFinishFrame(22, "tok-1", 10, 100, 110)),
			longStream(longTextFrame("第二轮", false), longFinishFrame(1, "", 20, 30, 50)),
		},
		send: func(ctx context.Context) { reportRequestPhase(ctx, RequestPhaseSendingUpstream) },
	}
	clock := &fakeFirstEventClock{window: 10 * time.Second, restartOK: true}
	ctx := ContextWithRequestPhaseObserver(context.Background(), func(phase RequestPhase) {
		if phase == RequestPhaseSendingUpstream {
			sending.Add(1)
		}
	})
	ctx, _ = regenerationTestBudget(ContextWithFirstEventClock(ctx, clock))
	summary, calls, _ := runLong(t, longClient(t, upstream, model, defaults), upstream, ctx, longRequest(model.ID, true))
	if len(calls) != 3 || summary.text != "第一轮第二轮" || len(summary.errs) != 0 {
		t.Fatalf("调用=%d 正文=%q 错误=%v", len(calls), summary.text, summary.errs)
	}
	if restarts := clock.restarts.Load(); restarts != 2 {
		t.Fatalf("首事件计时重新起算 %d 次，期望只有第一轮的重新生成起算 2 次（续写轮不起算）", restarts)
	}
	if count := sending.Load(); count != 2 {
		t.Fatalf("原始阶段观察者收到 %d 次开始发送，期望第一次发送与续写的第二轮各一次（重新生成的重发不报告）", count)
	}
}

// TestContinuationTrailerError 同一个响应里既以终止原因 22 要求续写、流尾又是 google.rpc 错误时按错误处理、不再续写：第一轮与普通请求相同，
// 已交出的正文之后以流尾错误（429）结束；第二轮起按这一轮失败体面结束（已输出内容完整返回、按 max_tokens 结束、带合并用量，
// 失败原因为流尾错误，账号状态照常处理）；流尾状态码为 0 时照常续写
func TestContinuationTrailerError(t *testing.T) {
	model, defaults := longArgonModel()
	// 第一轮：22 + 续写 token，流尾 429
	upstream := &longUpstream{
		streams: []string{
			regenStream(`[8,"Resource has been exhausted (e.g. check quota)."]`, longTextFrame("第一轮", false), longFinishFrame(22, "tok-1", 10, 100, 110)),
			longStream(longTextFrame("不应发出", false), longFinishFrame(1, "", 10, 5, 15)),
		},
	}
	summary, calls, progress := runLong(t, longClient(t, upstream, model, defaults), upstream, context.Background(), longRequest(model.ID, true))
	var rpcError *RPCError
	if len(calls) != 1 || summary.text != "第一轮" || len(summary.errs) != 1 || !errors.As(summary.errs[0], &rpcError) ||
		rpcError.StatusCode != http.StatusTooManyRequests || len(summary.finishes) != 0 {
		t.Fatalf("第一轮流尾错误：调用=%d 正文=%q 错误=%v 结束=%v", len(calls), summary.text, summary.errs, summary.finishes)
	}
	if len(roundEnds(progress)) != 0 {
		t.Fatalf("第一轮流尾错误不应报告续写进度: %+v", progress)
	}
	// 第二轮：22 + 续写 token，流尾 503
	upstream = &longUpstream{
		streams: []string{
			longStream(longTextFrame("第一轮", false), longFinishFrame(22, "tok-1", 10, 100, 110)),
			regenStream(`[14,"The service is currently unavailable."]`, longTextFrame("第二轮", false), longFinishFrame(22, "tok-2", 20, 30, 50)),
			longStream(longTextFrame("不应发出", false), longFinishFrame(1, "", 10, 5, 15)),
		},
	}
	summary, calls, progress = runLong(t, longClient(t, upstream, model, defaults), upstream, context.Background(), longRequest(model.ID, true))
	if len(calls) != 2 || summary.text != "第一轮第二轮" || len(summary.errs) != 0 || len(summary.finishes) != 1 || summary.finishes[0] != "max_tokens" ||
		len(summary.usages) != 1 {
		t.Fatalf("第二轮流尾错误：调用=%d 正文=%q 错误=%v 结束=%v 用量=%v", len(calls), summary.text, summary.errs, summary.finishes, summary.usages)
	}
	failed := 0
	for _, item := range progress {
		if item.Stage == ContinuationFailed {
			failed++
			if item.Round != 2 || item.Sending || !errors.As(item.Err, &rpcError) || rpcError.StatusCode != http.StatusServiceUnavailable {
				t.Fatalf("第二轮失败进度 = %+v", item)
			}
		}
	}
	if failed != 1 {
		t.Fatalf("应报告一次第二轮失败: %+v", progress)
	}
	// 流尾状态码为 0：照常续写
	upstream = &longUpstream{
		streams: []string{
			regenStream(`[0,""]`, longTextFrame("第一轮", false), longFinishFrame(22, "tok-1", 10, 100, 110)),
			longStream(longTextFrame("第二轮", false), longFinishFrame(1, "", 20, 30, 50)),
		},
	}
	summary, calls, _ = runLong(t, longClient(t, upstream, model, defaults), upstream, context.Background(), longRequest(model.ID, true))
	if len(calls) != 2 || summary.text != "第一轮第二轮" || len(summary.errs) != 0 || calls[1].body[16] != "tok-1" {
		t.Fatalf("流尾状态码 0：调用=%d 正文=%q 错误=%v", len(calls), summary.text, summary.errs)
	}
}

// TestResendAdmissionSharedByRegenerationAndContinuation 重新生成（含续写回退的重发）与续写的下一轮共用同一套准入：同样计入其他租约的预占
// （续写的预占挡住别的租约的重新生成），自己的预占不挡自己，同一租约不重复预占，不准入的说明写法相同，释放租约时退还没发出的预占
func TestResendAdmissionSharedByRegenerationAndContinuation(t *testing.T) {
	pool := newRateTestPool(t, time.Minute, 3, 3, indexTestAccount(longTestAccount, BenefitTierFree, indexTestCatalog()))
	selection := AccountSelection{ModelID: indexTestModel, Method: "generateContent"}
	leaseA := mustTryAcquire(t, pool, selection)
	leaseB := mustTryAcquire(t, pool, selection)
	defer func() { _ = leaseB.Release() }()
	leaseA.RecordGenerationSent()
	leaseB.RecordGenerationSent()
	// A 为续写的下一轮预占：自己的预占不挡自己
	leaseA.ReserveContinuation()
	if admission := leaseA.continuationAdmission(indexTestModel, time.Now()); admission.wait != 0 {
		t.Fatalf("续写自己的预占不应挡住自己: %+v", admission)
	}
	// B 重新生成：A 的续写预占同样计入
	if denied := leaseB.ReserveRegeneration(indexTestModel); denied != "每分钟名额 3/3（已发出 2、预占 1，ACCOUNT_RPM_LIMIT）" {
		t.Fatalf("续写的预占应挡住其他租约的重新生成：%q", denied)
	}
	// A 发出后再为下一轮预占：窗口已满，等待的说明与重新生成不准入的说明写法相同；同一租约再要重新生成时不重复预占
	leaseA.RecordGenerationSent()
	leaseA.ReserveContinuation()
	admission := leaseA.continuationAdmission(indexTestModel, time.Now())
	if admission.wait <= 0 || admission.reason != ContinuationWaitRate || admission.detail != "每分钟名额 3/3（已发出 3、预占 0，ACCOUNT_RPM_LIMIT）" {
		t.Fatalf("续写等待 = %+v", admission)
	}
	if denied := leaseA.ReserveRegeneration(indexTestModel); denied != admission.detail {
		t.Fatalf("同一状态下重新生成的不准入说明 = %q，续写等待的说明 = %q，写法应相同", denied, admission.detail)
	}
	if _, _, pending, _ := rateSnapshot(pool, longTestAccount); pending != 1 {
		t.Fatalf("预占 = %d，期望同一租约不重复预占", pending)
	}
	_ = leaseA.Release()
	pool.mu.Lock()
	continuing := len(pool.byID[longTestAccount].rate.continuing)
	pool.mu.Unlock()
	if _, _, pending, _ := rateSnapshot(pool, longTestAccount); pending != 0 || continuing != 0 {
		t.Fatalf("释放租约应退还没发出的预占：预占=%d 续写登记=%d", pending, continuing)
	}
}

// TestResendAdmissionSharedEgress 出口令牌桶：重新生成不准入时不预占、不消耗令牌；续写的下一轮预占时消耗一个令牌并等到它可用，
// 等待的说明与重新生成不准入的说明写法相同；同一租约不重复消耗；释放租约时退还消耗的令牌
func TestResendAdmissionSharedEgress(t *testing.T) {
	pool := newEgressTestPool(4, longTestAccount)
	pool.SetEgressRateLimit("", EgressLaneNormal, 6)
	lease := mustTryAcquire(t, pool, egressPrimarySelection)
	lease.RecordGenerationSent()
	tat := func() int64 {
		pool.mu.Lock()
		defer pool.mu.Unlock()
		return lease.egress.tat
	}
	before := tat()
	denied := lease.ReserveRegeneration(indexTestModel)
	if denied != "出口 "+EgressLabel("")+"（普通通道）限速 6 次/分钟，令牌未到" || tat() != before {
		t.Fatalf("重新生成不准入时不应消耗令牌：%q tat %d → %d", denied, before, tat())
	}
	lease.ReserveContinuation()
	charged := tat()
	admission := lease.continuationAdmission(indexTestModel, time.Now())
	if charged <= before || admission.wait <= 0 || admission.reason != ContinuationWaitEgress || admission.detail != denied {
		t.Fatalf("续写预占应消耗令牌并等待，说明与重新生成相同：tat %d → %d，等待 %+v", before, charged, admission)
	}
	if again := lease.ReserveRegeneration(indexTestModel); again != denied || tat() != charged {
		t.Fatalf("同一租约不应重复消耗令牌：%q tat %d → %d", again, charged, tat())
	}
	_ = lease.Release()
	if refunded := tat(); refunded >= charged {
		t.Fatalf("释放租约应退还续写预占消耗的令牌：tat %d → %d", charged, refunded)
	}
}

// TestContinuationRoundsUseDeferredCookies long- 续写在同一租约上连发几轮时，Cookie 写盘节流照常：每轮返回后上游轮换的 SIDCC 暂存、
// 30 秒内不写盘，下一轮发送前 Go 通道读取（ReloadStorageState）拿到的是上一轮写回的最新值，与每次都写盘时发出的 Cookie 相同；
// 整段结束后写盘时写出最后一轮的值
func TestContinuationRoundsUseDeferredCookies(t *testing.T) {
	pool := testPoolWithAccount(t, "alice@example.com")
	lease, err := pool.AcquireFor(context.Background(), AccountSelection{AccountID: "alice@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	path := lease.Account().StoragePath
	if err := lease.ReplaceCookies(rotatingTestCookies("OLD", "s0")); err != nil {
		t.Fatal(err)
	}
	before, _ := os.Stat(path)
	var sent []string
	var round int
	model, defaults := longArgonModel()
	upstream := &longUpstream{
		streams: []string{
			longStream(longTextFrame("第一轮", false), longFinishFrame(22, "tok-1", 10, 100, 110)),
			longStream(longTextFrame("第二轮", false), longFinishFrame(22, "tok-2", 20, 30, 50)),
			longStream(longTextFrame("第三轮", false), longFinishFrame(1, "", 30, 7, 37)),
		},
		send: func(ctx context.Context) {
			// 像 Go 通道一样发送前读取认证状态，发出后按上游的 Set-Cookie 写回轮换的 SIDCC
			current, _ := AccountLeaseFromContext(ctx)
			state, err := current.ReloadStorageState()
			if err != nil {
				t.Error(err)
				return
			}
			value, _ := state.CookieValue("SIDCC", aiStudioOrigin+"/", time.Now())
			sent = append(sent, value)
			round++
			if err := current.ReplaceCookies(rotatingTestCookies("OLD", fmt.Sprintf("s%d", round))); err != nil {
				t.Error(err)
			}
		},
	}
	client := longClient(t, upstream, model, defaults)
	client.catalogs["alice@example.com"] = client.catalogs[longTestAccount]
	request := longRequest(model.ID, true)
	request.AccountID = "alice@example.com"
	summary, calls, _ := runLong(t, client, upstream, ContextWithAccountLease(context.Background(), lease), request)
	if len(calls) != 3 || summary.text != "第一轮第二轮第三轮" || len(summary.errs) != 0 {
		t.Fatalf("调用=%d 正文=%q 错误=%v", len(calls), summary.text, summary.errs)
	}
	if strings.Join(sent, ",") != "s0,s1,s2" {
		t.Fatalf("各轮发送前读到的 SIDCC = %v，期望每轮都是上一轮写回的最新值", sent)
	}
	if after, _ := os.Stat(path); !after.ModTime().Equal(before.ModTime()) || diskCookie(t, path, "SIDCC") != "s0" {
		t.Fatal("30 秒内只有轮换 Cookie 变化时续写各轮不应写盘")
	}
	if err := pool.FlushDeferredStorage("alice@example.com"); err != nil {
		t.Fatal(err)
	}
	if got := diskCookie(t, path, "SIDCC"); got != "s3" {
		t.Fatalf("写盘时应写出最后一轮的 SIDCC，得到 %q", got)
	}
}
