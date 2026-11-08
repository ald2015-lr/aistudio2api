package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/api"
)

// 工具约束未满足时的同账号重新生成（aistudio/tool_guard.go）经完整链路（HTTP → API 层 → 生成服务 → PooledService → Client）的测试

const (
	toolTestText = `[[[[[[[[null,"plain answer"]],"model"]]]],[[[null,1]]]]]`
	toolTestCall = `[[[[[[[[null,null,null,null,null,null,null,null,null,null,["Kei_Workflow",[[["task",[null,null,"go"]]]]]]],"model"]]]],[[[null,1]]]]]`
)

// toolRegenerationTestService 返回接真实 PooledService 的生成服务：accounts 个账号，模型目录里 testRuntimeModel 支持函数调用，上游为 transport
func toolRegenerationTestService(t *testing.T, transport *scriptedTransport, accounts ...string) *trackedService {
	t.Helper()
	client, err := aistudio.NewClient(aistudio.ClientOptions{Transport: &catalogRPCTransport{}, Protected: transport})
	if err != nil {
		t.Fatal(err)
	}
	row := catalogTestRow(testRuntimeModel, 0.7)
	row[64] = []int{1, 10, 25}
	catalog, err := aistudio.ParseModelCatalogRaw([]byte(catalogTestRaw(t, row)))
	if err != nil {
		t.Fatal(err)
	}
	for _, accountID := range accounts {
		client.PrimeAccountCatalog(accountID, aistudio.BenefitTierFree, catalog)
	}
	service, pool := runningGenerateService(t, nil, context.Background(), time.Minute, accounts...)
	pooled, err := aistudio.NewPooledService(pool, client)
	if err != nil {
		t.Fatal(err)
	}
	service.service = pooled
	return service
}

// toolChatBody 为强制调用 Kei_Workflow 的 Chat Completions 请求（与报告里的 Agent 框架相同的写法）
func toolChatBody(stream bool) string {
	body := map[string]any{
		"model": testRuntimeModel, "stream": stream,
		"messages": []any{map[string]any{"role": "user", "content": "run the workflow"}},
		"tools": []any{
			map[string]any{"type": "function", "function": map[string]any{
				"name": "Kei_Workflow", "parameters": map[string]any{
					"type": "object", "properties": map[string]any{"task": map[string]any{"type": "string"}}, "required": []string{"task"},
				},
			}},
			map[string]any{"type": "function", "function": map[string]any{
				"name": "Kei_Search", "parameters": map[string]any{"type": "object", "properties": map[string]any{"q": map[string]any{"type": "string"}}},
			}},
		},
		"tool_choice": map[string]any{"type": "function", "function": map[string]any{"name": "Kei_Workflow"}},
	}
	data, _ := json.Marshal(body)
	return string(data)
}

func postToolChat(t *testing.T, service *trackedService, stream bool) *httptest.ResponseRecorder {
	t.Helper()
	handler := api.NewHandler(service, api.Config{APIKey: "sk-test"})
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(toolChatBody(stream)))
	request.Header.Set("Authorization", "Bearer sk-test")
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

// TestToolChoiceRegeneratesEndToEnd 强制调用指定函数、第一次只回复文字：流式与非流式的客户端都只收到第二次的函数调用（没有第一次的正文），
// 上游在同一账号发送 2 次（第一次发送就写明必须调用，重新生成的那次再加强）；请求日志写一行“工具约束未满足，同账号重新生成”
func TestToolChoiceRegeneratesEndToEnd(t *testing.T) {
	for _, stream := range []bool{false, true} {
		name := "非流式"
		if stream {
			name = "流式"
		}
		t.Run(name, func(t *testing.T) {
			accounts := switchTestIDs(2)
			transport := newScriptedTransport(func(_ string, call int, _ string) *aistudio.RPCResponse {
				body := toolTestCall
				if call == 1 {
					body = toolTestText
				}
				return playgroundResponse(http.StatusOK, io.NopCloser(strings.NewReader(body)))
			})
			service := toolRegenerationTestService(t, transport, accounts...)
			recorder := postToolChat(t, service, stream)
			body := recorder.Body.String()
			if recorder.Code != http.StatusOK || !strings.Contains(body, `"Kei_Workflow"`) || !strings.Contains(body, `"tool_calls"`) {
				t.Fatalf("状态 %d，响应 %s，期望 200 与函数调用", recorder.Code, body)
			}
			if strings.Contains(body, "plain answer") {
				t.Fatalf("第一次的正文不应交给客户端：%s", body)
			}
			if !strings.Contains(body, `\"task\":\"go\"`) && !strings.Contains(body, `"{\"task\":\"go\"}"`) {
				t.Fatalf("调用参数不对：%s", body)
			}
			bodies, order := transport.sent()
			if len(order) != 2 || order[0] != order[1] {
				t.Fatalf("应在同一账号发送 2 次，实际发送顺序 %v", order)
			}
			sent := bodies[order[0]]
			const required = "You must call the function Kei_Workflow in this response with arguments that match its schema. Do not reply with plain text."
			if !strings.Contains(sent[0], required) || strings.Contains(sent[0], "Use one of these tools") ||
				strings.Contains(sent[0], "Reply with the function call only.") {
				t.Fatal("第一次发送就应写明必须调用（第 1 级）")
			}
			if !strings.Contains(sent[1], required+`\nReply with the function call only.`) || !strings.Contains(sent[1], `\n\n`+required) {
				t.Fatal("重新生成的那次应把工具要求写得更明确（再加一句并附到用户消息）")
			}
			expectProgress(t, service, "工具约束未满足，同账号重新生成 | 原因=要求调用工具 Kei_Workflow，上游没有返回工具调用（结束原因=stop） | 第 1/2 次")
		})
	}
}

// TestToolChoiceExhaustedEndToEnd 每次都只回复文字：按 502 结束，说明写明没有调用要求的函数与共生成的次数（不含会让中转禁用渠道的字样），
// 不换号（违约与账号无关），账号照收到首事件记成功侧。加了随机后缀（REPEAT_PROMPT_NONCE 开启）时共生成 3 次，请求日志写两行重新生成、
// 一行次数已用完；没加时第二次重新生成的输入会与上一次逐字节相同，只生成 2 次，请求日志写一行重新生成、一行不再重新生成的原因
func TestToolChoiceExhaustedEndToEnd(t *testing.T) {
	for _, nonce := range []bool{true, false} {
		t.Run(fmt.Sprintf("随机后缀=%v", nonce), func(t *testing.T) {
			accounts := switchTestIDs(2)
			transport := newScriptedTransport(func(string, int, string) *aistudio.RPCResponse {
				return playgroundResponse(http.StatusOK, io.NopCloser(strings.NewReader(toolTestText)))
			})
			service := toolRegenerationTestService(t, transport, accounts...)
			service.repeatNonce.Store(nonce)
			service.egress = newEgressGuard(service.pool, service.requests)
			recorder := postToolChat(t, service, false)
			body := recorder.Body.String()
			attempts := 2
			if nonce {
				attempts = 3
			}
			if want := fmt.Sprintf("The model did not call the required function Kei_Workflow (tried %d times). Please retry.", attempts); recorder.Code != http.StatusBadGateway ||
				!strings.Contains(body, want) {
				t.Fatalf("状态 %d，响应 %s，期望 502 与写明原因的说明", recorder.Code, body)
			}
			if strings.Contains(body, "plain answer") {
				t.Fatal("违约时不应交出扣住的正文")
			}
			bodies, order := transport.sent()
			if len(order) != attempts || order[0] != order[len(order)-1] || order[0] != order[1] {
				t.Fatalf("应在同一账号发送 %d 次，实际发送顺序 %v", attempts, order)
			}
			if sent := bodies[order[0]]; sent[len(sent)-1] == sent[len(sent)-2] {
				t.Fatal("每次重新生成的请求体都应与上一次不同")
			}
			// 上游账号与出口这次都正常返回了回复：出口判定照收到首事件记一次成功，不记其他失败
			service.egress.mu.Lock()
			var counts egressCounts
			for _, state := range service.egress.states {
				counts.add(state.history.sum(time.Now(), time.Hour, egressLaneOf(false)))
			}
			service.egress.mu.Unlock()
			if counts.success != 1 || counts.other != 0 {
				t.Fatalf("出口判定 = %+v，期望一次成功、没有其他失败", counts)
			}
			if nonce {
				expectProgress(t, service,
					"工具约束未满足，同账号重新生成 | 原因=要求调用工具 Kei_Workflow，上游没有返回工具调用（结束原因=stop） | 第 2/2 次",
					"工具约束未满足，重新生成次数已用完（2 次），不再重新生成 | 原因=要求调用工具 Kei_Workflow")
				if !progressLoggedAll(service, "第 1/2 次", "输入=已换随机后缀，已加强工具要求") || !progressLoggedAll(service, "第 2/2 次", "输入=已换随机后缀") {
					t.Fatal("有随机后缀时两次重新生成都应换后缀")
				}
				return
			}
			expectProgress(t, service,
				"工具约束未满足，同账号重新生成 | 原因=要求调用工具 Kei_Workflow，上游没有返回工具调用（结束原因=stop） | 第 1/2 次",
				"且工具要求已是最高的第 2 级，原样重发多半得到同样的结果，不再重新生成 | 原因=要求调用工具 Kei_Workflow")
			if progressLogged(service, "第 2/2 次") || progressLogged(service, "重新生成次数已用完") {
				t.Fatal("输入不会变时不应再重新生成")
			}
		})
	}
}

// TestFirstEventDeadlinePause 首事件计时暂停：生成服务扣住上游事件时不再到期，之后不被 start 重新启动；同一尝试内的重发照常重新起算；
// 已停止或已触发时暂停不改变结果
func TestFirstEventDeadlinePause(t *testing.T) {
	fired := make(chan error, 1)
	deadline := newFirstEventDeadline(80*time.Millisecond, func(err error) { fired <- err })
	deadline.start()
	deadline.PauseFirstEvent()
	deadline.start()
	select {
	case err := <-fired:
		t.Fatalf("暂停后不应到期：%v", err)
	case <-time.After(200 * time.Millisecond):
	}
	if !deadline.RestartFirstEvent() {
		t.Fatal("暂停后的重发应能重新起算")
	}
	select {
	case <-fired:
	case <-time.After(2 * time.Second):
		t.Fatal("重新起算后应照常到期")
	}
	if deadline.stop() == nil {
		t.Fatal("到期后 stop 应返回首事件超时")
	}
	deadline.PauseFirstEvent()
	if deadline.RestartFirstEvent() {
		t.Fatal("已触发时不应重新起算")
	}
	// 关闭时暂停不改变首事件计时，但照常通知请求上下文退出首事件前的固定期限（回调），重发时照常重新起算空闲计时；停止后不再回调
	off := newFirstEventDeadline(0, func(error) { t.Fatal("关闭时不应取消") })
	holds, restarts := 0, 0
	off.observeHold(func() { holds++ }, func() { restarts++ })
	if off.holding() {
		t.Fatal("还没扣住时不应标为扣住")
	}
	off.PauseFirstEvent()
	off.start()
	if !off.RestartFirstEvent() || holds != 1 || restarts != 1 || !off.holding() {
		t.Fatalf("关闭时扣住与重发应照常回调：扣住 %d 次、重发 %d 次", holds, restarts)
	}
	if off.stop() != nil || !off.RestartFirstEvent() {
		t.Fatal("关闭时暂停不改变结果")
	}
	off.PauseFirstEvent()
	if holds != 1 || restarts != 1 {
		t.Fatalf("停止后不应再回调：扣住 %d 次、重发 %d 次", holds, restarts)
	}
	// 开启时同样回调
	on := newFirstEventDeadline(time.Hour, func(error) {})
	onHolds := 0
	on.observeHold(func() { onHolds++ }, nil)
	on.start()
	on.PauseFirstEvent()
	if onHolds != 1 || !on.holding() || on.stop() != nil {
		t.Fatalf("开启时扣住应回调一次：%d", onHolds)
	}
}

// pacedBody 每隔 interval 读出 chunks 的下一段（第一段立即读出）：上游持续输出、过一会儿才出后面的帧；关闭后读取立即结束
type pacedBody struct {
	chunks   []string
	interval time.Duration
	current  *strings.Reader
	closed   chan struct{}
	once     sync.Once
}

func newPacedBody(interval time.Duration, chunks ...string) *pacedBody {
	return &pacedBody{chunks: chunks, interval: interval, closed: make(chan struct{})}
}

func (body *pacedBody) Read(buffer []byte) (int, error) {
	for body.current == nil || body.current.Len() == 0 {
		if len(body.chunks) == 0 {
			return 0, io.EOF
		}
		if body.current != nil {
			select {
			case <-time.After(body.interval):
			case <-body.closed:
				return 0, io.ErrClosedPipe
			}
		}
		body.current, body.chunks = strings.NewReader(body.chunks[0]), body.chunks[1:]
	}
	return body.current.Read(buffer)
}

func (body *pacedBody) Close() error {
	body.once.Do(func() { close(body.closed) })
	return nil
}

// toolTestTextFrame 为一块正文流帧（不带外层数组）
func toolTestTextFrame(text string) string {
	return `[[[[[[null,"` + text + `"]],"model"]]]]`
}

const (
	toolTestCallFrame = `[[[[[[null,null,null,null,null,null,null,null,null,null,["Kei_Workflow",[[["task",[null,null,"go"]]]]]]],"model"]]]]`
	toolTestStopFrame = `[[[null,1]]]`
)

// TestToolHoldOutlivesRequestTimeout 强制调用指定函数、上游持续输出正文、过了比 REQUEST_TIMEOUT 更久才出调用：扣住期间请求上下文退出
// 首事件前的固定期限、改按上游活动判空闲，流式与非流式都正常返回 200 与调用（扣住的正文在调用之前交出），只发送 1 次；
// 原先首事件前期限照常计，到期按 504 结束
func TestToolHoldOutlivesRequestTimeout(t *testing.T) {
	for _, stream := range []bool{false, true} {
		name := "非流式"
		if stream {
			name = "流式"
		}
		t.Run(name, func(t *testing.T) {
			accounts := switchTestIDs(1)
			transport := newScriptedTransport(func(string, int, string) *aistudio.RPCResponse {
				chunks := []string{"[[" + toolTestTextFrame("part0")}
				for index := 1; index < 6; index++ {
					chunks = append(chunks, ","+toolTestTextFrame(fmt.Sprintf("part%d", index)))
				}
				chunks = append(chunks, ","+toolTestCallFrame+","+toolTestStopFrame+"]]")
				return playgroundResponse(http.StatusOK, newPacedBody(200*time.Millisecond, chunks...))
			})
			service := toolRegenerationTestService(t, transport, accounts...)
			service.timeout.Store(int64(500 * time.Millisecond))
			startedAt := time.Now()
			recorder := postToolChat(t, service, stream)
			body := recorder.Body.String()
			if recorder.Code != http.StatusOK || !strings.Contains(body, `"Kei_Workflow"`) || !strings.Contains(body, "part5") {
				t.Fatalf("状态 %d，响应 %s，期望 200、扣住的正文与函数调用", recorder.Code, body)
			}
			if elapsed := time.Since(startedAt); elapsed < time.Second {
				t.Fatalf("用时 %s，期望上游持续输出约 1 秒（超过 REQUEST_TIMEOUT）", elapsed)
			}
			if _, order := transport.sent(); len(order) != 1 {
				t.Fatalf("满足约束时只发送 1 次，实际 %v", order)
			}
			expectProgress(t, service, "工具约束要求必须调用：正文与思考扣住到调用出现 | 要求=Kei_Workflow | 首个上游事件=")
		})
	}
}

// TestToolHoldIdleExpires 扣住期间上游连续 REQUEST_TIMEOUT 没有活动：按扣住期间空闲到期，504，请求日志写明阶段；扣住的正文不交出
func TestToolHoldIdleExpires(t *testing.T) {
	accounts := switchTestIDs(1)
	transport := newScriptedTransport(func(string, int, string) *aistudio.RPCResponse {
		return playgroundResponse(http.StatusOK, newPacedBody(5*time.Second, "[["+toolTestTextFrame("part0"), ","+toolTestCallFrame+"]]"))
	})
	service := toolRegenerationTestService(t, transport, accounts...)
	service.timeout.Store(int64(300 * time.Millisecond))
	startedAt := time.Now()
	recorder := postToolChat(t, service, false)
	elapsed := time.Since(startedAt)
	if recorder.Code != http.StatusGatewayTimeout || strings.Contains(recorder.Body.String(), "part0") {
		t.Fatalf("状态 %d，响应 %s，期望 504 且不交出扣住的正文", recorder.Code, recorder.Body.String())
	}
	if elapsed < 300*time.Millisecond || elapsed > 3*time.Second {
		t.Fatalf("用时 %s，期望约一个空闲期限后结束", elapsed)
	}
	expectProgress(t, service, "本服务期限截断 | 阶段=扣住期间空闲 | 期限=300ms", "停在=扣住上游事件")
}

// TestToolContractNotEgressFailure 工具约束违约与出口无关：不计入出口的其他失败（包一层同样）
func TestToolContractNotEgressFailure(t *testing.T) {
	for _, err := range []error{
		&aistudio.ToolContractError{Kind: aistudio.ToolContractMissingCall},
		fmt.Errorf("尝试失败: %w", &aistudio.ToolContractError{Kind: aistudio.ToolContractSchemaMismatch}),
	} {
		if egressCountsOther(err) || egressOtherOutcome(context.Background(), true, false, false, err) {
			t.Fatalf("%v 不应计入出口的其他失败", err)
		}
	}
	if !egressCountsOther(errors.New("其他错误")) {
		t.Fatal("对照：其他非 RPC 错误照常计入")
	}
}

// TestToolHoldEndsBeforeAccountSwitch 扣住过上游事件的尝试以可换号的错误结束（账号 A 持续输出正文约 450ms 后只回复文字、重新生成那次
// 503）：换到账号 B 前退出扣住（endHold），B 一直没有首个事件时按首事件前期限到期（504），期限顺延了扣住的时长（错误写明不计入的扣住时长），
// 不是按 A 留下的扣住状态以“扣住期间空闲”到期；扣住的正文不交出。原先没有 endHold 时 B 的等待仍按 A 的上游活动判扣住期间空闲，
// 也不再受首事件前期限约束
func TestToolHoldEndsBeforeAccountSwitch(t *testing.T) {
	accounts := switchTestIDs(2)
	var mu sync.Mutex
	first := ""
	transport := newScriptedTransport(func(accountID string, call int, _ string) *aistudio.RPCResponse {
		mu.Lock()
		if first == "" {
			first = accountID
		}
		isFirst := accountID == first
		mu.Unlock()
		switch {
		case isFirst && call == 1:
			return playgroundResponse(http.StatusOK, newPacedBody(150*time.Millisecond,
				"[["+toolTestTextFrame("part0"), ","+toolTestTextFrame("part1"), ","+toolTestTextFrame("part2"), ","+toolTestStopFrame+"]]"))
		case isFirst:
			return playgroundResponse(http.StatusServiceUnavailable, io.NopCloser(strings.NewReader(`[14,"The service is currently unavailable."]`)))
		default:
			// 账号 B：返回响应头后一直没有首个事件
			return playgroundResponse(http.StatusOK, newPacedBody(time.Minute, "[[", "]]"))
		}
	})
	service := toolRegenerationTestService(t, transport, accounts...)
	const requestTimeout = 400 * time.Millisecond
	service.timeout.Store(int64(requestTimeout))
	request := aistudio.GenerateRequest{
		ID: "req-hold-switch", Model: testRuntimeModel,
		Contents: []aistudio.Content{{Role: aistudio.RoleUser, Parts: []aistudio.Part{{Text: "run the workflow"}}}},
		Tools: aistudio.Tools{
			Functions: []aistudio.FunctionDeclaration{{
				Name: "Kei_Workflow", Parameters: json.RawMessage(`{"type":"object","properties":{"task":{"type":"string"}},"required":["task"]}`),
			}},
			ToolConfig: aistudio.ToolConfig{Mode: "required", AllowedFunctionNames: []string{"Kei_Workflow"}},
		},
	}
	var result deadlineResult
	select {
	case result = <-startGenerated(t, service, request, nil):
	case <-time.After(10 * time.Second):
		t.Fatal("请求没有结束")
	}
	if result.nonError() != 0 || result.err == nil {
		t.Fatalf("事件 = %+v，期望不交出扣住的正文、以错误结束", result.events)
	}
	var cut *serviceDeadlineError
	if !errors.As(result.err, &cut) || cut.stage != serviceDeadlineStart || cut.held || cut.heldFor < 300*time.Millisecond ||
		!strings.Contains(result.err.Error(), "本服务首事件前期限 400ms 到期（扣住上游事件的 ") || !strings.Contains(result.err.Error(), " 不计入）") {
		t.Fatalf("err = %v（%+v），期望换号后按首事件前期限到期、写明扣住的时长不计入", result.err, cut)
	}
	if !errors.Is(result.err, context.DeadlineExceeded) {
		t.Fatalf("err = %v，期望满足 DeadlineExceeded（对外 504）", result.err)
	}
	// 期限顺延了扣住的时长：结束时间不早于 REQUEST_TIMEOUT 加上扣住的时长
	if elapsed := result.endedAt.Sub(result.startedAt); elapsed < requestTimeout+cut.heldFor-50*time.Millisecond {
		t.Fatalf("用时 %s，期望不早于首事件前期限 %s 加扣住的 %s", elapsed, requestTimeout, cut.heldFor)
	}
	_, order := transport.sent()
	if len(order) != 3 || order[0] != order[1] || order[2] == order[0] {
		t.Fatalf("发送顺序 = %v，期望 A 发送 2 次（第二次 503）后换到 B", order)
	}
	expectProgress(t, service, "工具约束未满足，同账号重新生成", "本服务期限截断 | 阶段=首事件前 | 期限=400ms")
	if progressLogged(service, "阶段=扣住期间空闲") || progressLogged(service, "停在=扣住上游事件") {
		t.Fatal("换号后不应再按扣住状态判期限")
	}
}
