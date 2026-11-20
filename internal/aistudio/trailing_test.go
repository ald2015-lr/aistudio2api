package aistudio

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"syscall"
	"testing"
)

// 函数调用之后上游结束生成（trailing.go）的测试：已交给客户端的函数调用之后，上游以流尾错误、流中断、拦截请求、没有完成帧或内容策略类
// 原因结束时，这一轮按 stop 正常结束，原因挂在结束事件的 TrailingError 上

const trailingSafetyFrame = `[[[null,3,null,"blocked after the call"]]]`

// trailingFinish 返回事件里唯一的结束事件；有错误事件、结束事件不止一个或结束之后还有事件时报错
func trailingFinish(t *testing.T, events []Event) Event {
	t.Helper()
	var finish *Event
	for index, event := range events {
		switch event.Kind {
		case EventError:
			t.Fatalf("不应以错误结束: %v | %+v", event.Err, eventKinds(events))
		case EventFinish:
			if finish != nil || index != len(events)-1 {
				t.Fatalf("结束事件应只有一个且在最后: %v", eventKinds(events))
			}
			value := event
			finish = &value
		}
	}
	if finish == nil {
		t.Fatalf("没有结束事件: %v", eventKinds(events))
	}
	return *finish
}

// trailingUsage 返回事件里唯一的用量
func trailingUsage(t *testing.T, events []Event) Usage {
	t.Helper()
	var usages []Usage
	for _, event := range events {
		if event.Kind == EventUsage {
			usages = append(usages, *event.Usage)
		}
	}
	if len(usages) != 1 {
		t.Fatalf("用量事件 %d 个，期望 1 个: %v", len(usages), eventKinds(events))
	}
	return usages[0]
}

// TestTrailingAfterFunctionCall Playground 上已交出函数调用之后：流中断、流尾 429（在完成帧之后）、拦截请求帧、没有完成帧与内容策略类结束原因
// 都按 stop 正常结束，客户端收到完整的函数调用、用量与结束事件；原因留在 TrailingError（拦截类原因可读出原因名，内容策略类原因不算上游错误），
// 结束说明去掉；上游用量没到时按已交出的输出估算；只发送一次（已交出内容，不重新生成）
func TestTrailingAfterFunctionCall(t *testing.T) {
	for _, test := range []struct {
		name     string
		response func() (*RPCResponse, error)
		check    func(t *testing.T, trailing error)
		reason   string
		account  bool
	}{
		{
			name: "流中断", response: interruptedResponse("[[" + guardCallFrame), account: true,
			check: func(t *testing.T, trailing error) {
				if !errors.Is(trailing, syscall.ECONNRESET) {
					t.Fatalf("TrailingError = %v，期望连接重置", trailing)
				}
			},
		},
		{
			name: "流尾 429", response: streamResponse(regenStream(`[8,"Resource has been exhausted"]`, guardCallFrame, regenStopFrame)), account: true,
			check: func(t *testing.T, trailing error) {
				var rpcError *RPCError
				if !errors.As(trailing, &rpcError) || rpcError.StatusCode != http.StatusTooManyRequests {
					t.Fatalf("TrailingError = %v，期望流尾 429", trailing)
				}
			},
		},
		{
			name: "拦截请求", response: streamResponse(regenStream("", guardCallFrame, `[null,[1,null,"input blocked"]]`)), reason: "safety", account: true,
			check: func(t *testing.T, trailing error) {
				var feedback *PromptFeedbackError
				if !errors.As(trailing, &feedback) || feedback.Reason != "safety" {
					t.Fatalf("TrailingError = %v，期望拦截请求", trailing)
				}
			},
		},
		{
			name: "没有完成帧", response: streamResponse(regenStream("", guardCallFrame)), account: true,
			check: func(t *testing.T, trailing error) {
				if !errors.Is(trailing, errMissingFinishFrame) {
					t.Fatalf("TrailingError = %v，期望没有完成帧", trailing)
				}
			},
		},
		{
			name: "内容策略类结束原因", response: streamResponse(regenStream("", guardCallFrame, trailingSafetyFrame)), reason: "safety",
			check: func(t *testing.T, trailing error) {
				var policy *ContentPolicyFinishError
				if !errors.As(trailing, &policy) || policy.Reason != "safety" || policy.Message != "blocked after the call" {
					t.Fatalf("TrailingError = %v，期望内容策略类原因 safety 与上游说明", trailing)
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			upstream := &regenUpstream{responses: []func() (*RPCResponse, error){test.response, streamResponse(regenStream("", regenTextFrame, regenStopFrame))}}
			notes := &noteRecorder{}
			ctx := ContextWithRegenerationBudget(notes.context(context.Background()), NewRegenerationBudget())
			request := regenRequest("test")
			request.Tools = guardTools("auto", "", false)
			events, err := guardClient(t, "test", upstream).Generate(ctx, request)
			if err != nil {
				t.Fatal(err)
			}
			got := collectEvents(t, events)
			if len(got) != 3 || got[0].Kind != EventToolCall || got[0].ToolCall.Name != guardFunction {
				t.Fatalf("事件 = %v，期望函数调用、用量、结束", eventKinds(got))
			}
			finish := trailingFinish(t, got)
			if finish.FinishReason != "stop" || finish.FinishMessage != "" || finish.TrailingError == nil {
				t.Fatalf("结束 = %+v，期望按 stop 结束、去掉结束说明、带 TrailingError", finish)
			}
			test.check(t, finish.TrailingError)
			if reason := TrailingFinishReason(finish.TrailingError); reason != test.reason {
				t.Fatalf("TrailingFinishReason = %q，期望 %q", reason, test.reason)
			}
			if account := TrailingAccountError(finish.TrailingError); account != test.account {
				t.Fatalf("TrailingAccountError = %t，期望 %t", account, test.account)
			}
			if usage := trailingUsage(t, got); usage.OutputTokens <= 0 || usage.InputTokens <= 0 {
				t.Fatalf("用量 = %+v，期望按已交出的函数调用与输入估算", usage)
			}
			if upstream.calls() != 1 || notes.count("同账号重新生成") != 0 {
				t.Fatalf("已交出函数调用，不应重新生成：发送 %d 次，说明 %v", upstream.calls(), notes.notes)
			}
		})
	}
}

// TestTrailingAfterUpstreamFinish 函数调用之后上游已经给出完成帧、之后又以流尾错误结束：结束原因与结束说明以完成帧为准，与不带流尾错误时
// 客户端看到的相同，只挂上 TrailingError。max_tokens、malformed_function_call 照旧交出原因与说明；内容策略类原因照旧按 stop 结束，
// 拦截原因仍可作为终止原因读出，流尾错误挂在 Cause 上、照流内错误处理账号。原先整次请求以错误结束；上游 a68acad 一律改成 stop 并去掉说明，拦截原因丢失，这里不照搬
func TestTrailingAfterUpstreamFinish(t *testing.T) {
	const trailer = `[14,"The service is currently unavailable."]`
	for _, test := range []struct {
		name    string
		frame   string
		reason  string
		message string
		policy  string
	}{
		{name: "max_tokens", frame: `[[[null,2]]]`, reason: "max_tokens"},
		{name: "malformed_function_call", frame: `[[[null,10,null,"bad second call"]]]`, reason: "malformed_function_call", message: "bad second call"},
		{name: "safety", frame: trailingSafetyFrame, reason: "stop", policy: "safety"},
	} {
		t.Run(test.name, func(t *testing.T) {
			finishes := make(map[bool]Event, 2)
			for _, withTrailer := range []bool{false, true} {
				body := regenStream("", guardCallFrame, test.frame)
				if withTrailer {
					body = regenStream(trailer, guardCallFrame, test.frame)
				}
				upstream := &regenUpstream{responses: []func() (*RPCResponse, error){streamResponse(body)}}
				ctx := ContextWithRegenerationBudget(context.Background(), NewRegenerationBudget())
				request := regenRequest("test")
				request.Tools = guardTools("auto", "", false)
				events, err := guardClient(t, "test", upstream).Generate(ctx, request)
				if err != nil {
					t.Fatal(err)
				}
				finishes[withTrailer] = trailingFinish(t, collectEvents(t, events))
			}
			plain, trailed := finishes[false], finishes[true]
			if plain.FinishReason != test.reason || trailed.FinishReason != test.reason || trailed.FinishMessage != plain.FinishMessage ||
				trailed.FinishMessage != test.message {
				t.Fatalf("不带流尾 = %q/%q，带流尾 = %q/%q，期望都为 %q/%q", plain.FinishReason, plain.FinishMessage,
					trailed.FinishReason, trailed.FinishMessage, test.reason, test.message)
			}
			var rpcError *RPCError
			if !errors.As(trailed.TrailingError, &rpcError) || rpcError.StatusCode != http.StatusServiceUnavailable || !TrailingAccountError(trailed.TrailingError) {
				t.Fatalf("TrailingError = %v，期望带流尾 503 且按上游错误处理账号", trailed.TrailingError)
			}
			if reason := TrailingFinishReason(trailed.TrailingError); reason != test.policy || TrailingFinishReason(plain.TrailingError) != test.policy {
				t.Fatalf("终止原因：带流尾 %q，不带流尾 %q，期望都为 %q", reason, TrailingFinishReason(plain.TrailingError), test.policy)
			}
			if test.policy == "" && plain.TrailingError != nil || test.policy != "" && TrailingAccountError(plain.TrailingError) {
				t.Fatalf("不带流尾：TrailingError = %v", plain.TrailingError)
			}
			if test.policy != "" && !strings.Contains(trailed.TrailingError.Error(), "safety") {
				t.Fatalf("TrailingError 文本 = %q，期望带拦截原因", trailed.TrailingError)
			}
		})
	}
}

// TestTrailingNotApplied 没有交出函数调用、或结束原因来自本服务自己时照旧：只有正文时流尾 429 以错误结束、内容策略类原因照常结束；
// 已交出一个调用之后 strict 参数不符（工具约束违约）仍以 *ToolContractError 结束；正常结束不带 TrailingError
func TestTrailingNotApplied(t *testing.T) {
	run := func(t *testing.T, tools Tools, contract bool, response func() (*RPCResponse, error)) []Event {
		t.Helper()
		upstream := &regenUpstream{responses: []func() (*RPCResponse, error){response}}
		ctx := ContextWithRegenerationBudget(context.Background(), NewRegenerationBudget())
		request := regenRequest("test")
		request.Tools = tools
		if contract {
			request = guardRequest(t, tools)
		}
		events, err := guardClient(t, "test", upstream).Generate(ctx, request)
		if err != nil {
			t.Fatal(err)
		}
		return collectEvents(t, events)
	}
	t.Run("只有正文时流尾 429", func(t *testing.T) {
		got := run(t, Tools{}, false, streamResponse(regenStream(`[8,"Resource has been exhausted"]`, regenTextFrame, regenStopFrame)))
		last := got[len(got)-1]
		var rpcError *RPCError
		if last.Kind != EventError || !errors.As(last.Err, &rpcError) || rpcError.StatusCode != http.StatusTooManyRequests {
			t.Fatalf("事件 = %v，期望照旧以 429 错误结束", eventKinds(got))
		}
	})
	t.Run("只有正文时内容策略类原因", func(t *testing.T) {
		got := run(t, Tools{}, false, streamResponse(regenStream("", regenTextFrame, trailingSafetyFrame)))
		finish := trailingFinish(t, got)
		if finish.FinishReason != "safety" || finish.FinishMessage != "blocked after the call" || finish.TrailingError != nil {
			t.Fatalf("结束 = %+v，期望照常以 safety 结束", finish)
		}
	})
	t.Run("调用之后工具约束违约", func(t *testing.T) {
		got := run(t, guardTools("auto", "", true), true, streamResponse(regenStream("", guardCallFrame, guardBadArgsFrame, regenStopFrame)))
		violation := guardContractError(got)
		if violation == nil || violation.Kind != ToolContractSchemaMismatch || got[len(got)-1].Kind != EventError {
			t.Fatalf("事件 = %v 违约 = %+v，期望调用之后的 strict 违约照旧以错误结束", eventKinds(got), violation)
		}
	})
	t.Run("调用之后正常结束", func(t *testing.T) {
		got := run(t, guardTools("auto", "", false), false, streamResponse(regenStream("", guardCallFrame, regenStopFrame)))
		if finish := trailingFinish(t, got); finish.FinishReason != "stop" || finish.TrailingError != nil {
			t.Fatalf("结束 = %+v，期望正常结束、不带 TrailingError", finish)
		}
	})
}

// TestTrailingRequiredToolHeld 必须调用时正文扣住到调用出现：调用放出之后流中断按 stop 结束，扣住的正文随调用一起交出
func TestTrailingRequiredToolHeld(t *testing.T) {
	upstream := &regenUpstream{responses: []func() (*RPCResponse, error){
		interruptedResponse("[[" + guardTextFrame + "," + guardCallFrame),
	}}
	ctx := ContextWithRegenerationBudget(context.Background(), NewRegenerationBudget())
	events, err := guardClient(t, "test", upstream).Generate(ctx, guardRequest(t, guardTools("required", guardFunction, false)))
	if err != nil {
		t.Fatal(err)
	}
	got := collectEvents(t, events)
	if !guardHasText(got, "plain answer") || got[1].Kind != EventToolCall {
		t.Fatalf("事件 = %v，期望扣住的正文与调用一起交出", eventKinds(got))
	}
	if finish := trailingFinish(t, got); finish.FinishReason != "stop" || !errors.Is(finish.TrailingError, syscall.ECONNRESET) {
		t.Fatalf("结束 = %+v，期望按 stop 结束并带上流中断", finish)
	}
}

// TestTrailingBuildChannel Build 通道同样处理：函数调用之后以 SAFETY 结束时按 stop 结束，原因留在 TrailingError；契约照旧在事件流上核对
func TestTrailingBuildChannel(t *testing.T) {
	const (
		callChunk   = `{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"Kei_Workflow","args":{"task":"go"}}}]}}]}`
		safetyChunk = `{"candidates":[{"content":{"role":"model","parts":[{"text":""}]},"finishReason":"SAFETY"}],"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":2,"totalTokenCount":5}}`
	)
	upstream := &regenUpstream{responses: []func() (*RPCResponse, error){streamResponse(guardBuildStream(t, callChunk, safetyChunk))}}
	ctx := ContextWithAccountLease(context.Background(), &AccountLease{channel: ChannelBuild, account: &Account{ID: "alice@example.com"}})
	events, err := guardClient(t, "test", upstream).Generate(ctx, guardRequest(t, guardTools("required", guardFunction, false)))
	if err != nil {
		t.Fatal(err)
	}
	got := collectEvents(t, events)
	if got[0].Kind != EventToolCall {
		t.Fatalf("事件 = %v，期望先交出函数调用", eventKinds(got))
	}
	finish := trailingFinish(t, got)
	var policy *ContentPolicyFinishError
	if finish.FinishReason != "stop" || !errors.As(finish.TrailingError, &policy) || policy.Reason != "safety" {
		t.Fatalf("结束 = %+v，期望按 stop 结束、TrailingError 为 safety", finish)
	}
	if usage := trailingUsage(t, got); usage.InputTokens != 3 || usage.TotalTokens != 5 {
		t.Fatalf("用量 = %+v，期望上游报告的用量", usage)
	}
}

// TestTrailingLongOutput long- 请求：第一轮函数调用之后流中断按 stop 结束；官网续写第一轮交出函数调用后转入第二轮、第二轮发送失败时
// 原本按 max_tokens 体面结束，现在按 stop 结束，TrailingError 为续写失败的原因，ContinuationErr 保留（生成服务据此不重复处理账号状态）
func TestTrailingLongOutput(t *testing.T) {
	model, defaults := longArgonModel()
	model.Capabilities["function_declarations"] = true
	request := longRequest(model.ID, true)
	request.Tools = guardTools("auto", "", false)

	upstream := &longUpstream{bodies: map[int]io.ReadCloser{0: &interruptedBody{data: strings.NewReader("[[" + guardCallFrame)}}}
	events, err := longClient(t, upstream, model, defaults).Generate(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	got := longCollect(t, events)
	if finish := trailingFinish(t, got); finish.FinishReason != "stop" || !errors.Is(finish.TrailingError, syscall.ECONNRESET) || finish.ContinuationErr != nil {
		t.Fatalf("第一轮：结束 = %+v，期望按 stop 结束并带上流中断", finish)
	}

	unavailable := &RPCError{Method: "GenerateContent", StatusCode: http.StatusServiceUnavailable, Code: 14, Message: "unavailable"}
	upstream = &longUpstream{
		streams: []string{longStream(guardCallFrame, longFinishFrame(22, "tok-1", 100, 50, 150))},
		errs:    map[int]error{1: unavailable},
	}
	summary, calls, progress := runLong(t, longClient(t, upstream, model, defaults), upstream, context.Background(), request)
	if len(calls) != 2 || summary.tools != 1 || len(summary.errs) != 0 || len(summary.finishes) != 1 || summary.finishes[0] != "stop" {
		t.Fatalf("续写：调用=%d %+v，期望交出函数调用后按 stop 结束", len(calls), summary)
	}
	failed := false
	for _, item := range progress {
		failed = failed || item.Stage == ContinuationFailed && errors.Is(item.Err, unavailable)
	}
	if !failed {
		t.Fatalf("续写进度仍应报告第 2 轮失败: %+v", progress)
	}
	events, err = longClient(t, &longUpstream{
		streams: []string{longStream(guardCallFrame, longFinishFrame(22, "tok-1", 100, 50, 150))},
		errs:    map[int]error{1: unavailable},
	}, model, defaults).Generate(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	finish := trailingFinish(t, longCollect(t, events))
	if !errors.Is(finish.TrailingError, unavailable) || !errors.Is(finish.ContinuationErr, unavailable) || TrailingAccountError(finish.TrailingError) != true {
		t.Fatalf("续写：结束 = %+v，期望 TrailingError 与 ContinuationErr 都是第 2 轮的失败", finish)
	}
}
