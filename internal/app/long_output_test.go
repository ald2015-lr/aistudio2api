package app

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// long- 自动续写在生成服务里的测试：真实的 aistudio.Client 接在假上游上，经 generateWithRetry 与 forwardEvents 跑完整个请求

// longFrames 为一轮 GenerateContent 流：一段正文，以 code 结束，用量为 [输入, 正文, 总量]
func longFrames(text string, code int, usage [3]int64) string {
	quoted, _ := json.Marshal(text)
	textFrame := `[[[[[[null,` + string(quoted) + `]],"model"]]]]`
	finishFrame := `[[[null,` + itoa(int64(code)) + `]],null,[` + itoa(usage[0]) + `,` + itoa(usage[1]) + `,` + itoa(usage[2]) + `]]`
	return `[[` + textFrame + `,` + finishFrame + `]]`
}

func itoa(value int64) string {
	data, _ := json.Marshal(value)
	return string(data)
}

// longRuntimeUpstream 按调用顺序返回 streams：每次发送像 WorkerProtectedTransport 一样报告准备 WAA proof、等响应头与开始流式三个阶段，
// 经租约记一次发送（计入每账号每分钟上限），第 index 次（从 0 计）在返回前等 delays[index]（模拟准备 proof 与等响应头），
// errs[index] 非空时以它失败，bodies[index] 非空时以它返回的响应体代替 streams[index]；记下每次的账号，pool 非空时另记发送时
// 该账号上是否有服务 long- 请求的租约
type longRuntimeUpstream struct {
	streams []string
	delays  map[int]time.Duration
	errs    map[int]error
	bodies  map[int]func() io.ReadCloser
	pool    *aistudio.AccountPool
	mu      sync.Mutex
	calls   []string
	active  []bool
}

func (upstream *longRuntimeUpstream) protected() aistudio.ProtectedTransport {
	return aistudio.ProtectedTransportFunc(func(ctx context.Context, request aistudio.GenerateRequest, _ aistudio.RPCRequest) (*aistudio.RPCResponse, error) {
		lease, ok := aistudio.AccountLeaseFromContext(ctx)
		if !ok {
			return nil, io.ErrUnexpectedEOF
		}
		aistudio.ReportRequestPhaseForTest(ctx, aistudio.RequestPhasePreparingWAA)
		lease.RecordGenerationSent()
		aistudio.ReportRequestPhaseForTest(ctx, aistudio.RequestPhaseSendingUpstream)
		upstream.mu.Lock()
		index := len(upstream.calls)
		upstream.calls = append(upstream.calls, lease.Account().ID)
		if upstream.pool != nil {
			upstream.active = append(upstream.active, upstream.pool.LongOutputActive(lease.Account().ID))
		}
		upstream.mu.Unlock()
		if delay := upstream.delays[index]; delay > 0 {
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		if err := upstream.errs[index]; err != nil {
			return nil, err
		}
		var body io.ReadCloser
		switch {
		case upstream.bodies[index] != nil:
			body = upstream.bodies[index]()
		case index < len(upstream.streams):
			body = io.NopCloser(strings.NewReader(upstream.streams[index]))
		default:
			return nil, io.ErrUnexpectedEOF
		}
		aistudio.ReportRequestPhaseForTest(ctx, aistudio.RequestPhaseStreaming)
		return &aistudio.RPCResponse{
			StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {aistudio.JSONProtobufContentType}}, Body: body,
		}, nil
	})
}

// longOutputActive 返回各次发送时该账号上是否有服务 long- 请求的租约
func (upstream *longRuntimeUpstream) longOutputActive() []bool {
	upstream.mu.Lock()
	defer upstream.mu.Unlock()
	return append([]bool(nil), upstream.active...)
}

// stallingBody 返回先写出 prefix、之后一直不再有数据的响应体（关闭时读取结束）
func stallingBody(prefix string) func() io.ReadCloser {
	return func() io.ReadCloser {
		reader, writer := io.Pipe()
		go func() {
			_, _ = writer.Write([]byte(prefix))
		}()
		return reader
	}
}

func (upstream *longRuntimeUpstream) accounts() []string {
	upstream.mu.Lock()
	defer upstream.mu.Unlock()
	return append([]string(nil), upstream.calls...)
}

// longRuntimeService 返回生成服务：上游为真实 Client（目录里的 testRuntimeModel 单轮上限 100），号池里有两个账号
func longRuntimeService(t *testing.T, upstream *longRuntimeUpstream, firstEventTimeout time.Duration) (*trackedService, *aistudio.AccountPool) {
	t.Helper()
	client, err := aistudio.NewClient(aistudio.ClientOptions{Transport: &catalogRPCTransport{}, Protected: upstream.protected()})
	if err != nil {
		t.Fatal(err)
	}
	row := catalogTestRow(testRuntimeModel, 0.7)
	row[6] = 100
	row[64] = []int{1}
	data, err := aistudio.ParseModelCatalogRaw([]byte(catalogTestRaw(t, row)))
	if err != nil {
		t.Fatal(err)
	}
	accounts := []string{"alice@example.com", "bob@example.com"}
	for _, account := range accounts {
		client.PrimeAccountCatalog(account, aistudio.BenefitTierFree, data)
	}
	service, pool := firstEventTestService(t, client, firstEventTimeout, accounts...)
	service.setLongOutput(true, 300)
	return service, pool
}

// progressCount 返回包含 text 的请求进度条数
func progressCount(service *trackedService, text string) int {
	service.requests.mu.Lock()
	defer service.requests.mu.Unlock()
	count := 0
	for _, entry := range service.requests.logs {
		if entry.Event == "request.progress" && strings.Contains(entry.Message, text) {
			count++
		}
	}
	return count
}

// TestLongOutputRuntimeRounds 模拟续写跑完两轮：同一账号、每轮计入每分钟上限（第二轮等窗口释放）；两轮之间等窗口（比首事件后空闲期限长）
// 不触发空闲期限，下一轮准备与等响应头（比首事件超时长）不触发首事件超时；开始新一轮时总时长上限重新计算（整段比总时长上限长仍完整结束）；
// 客户端收到一条连续的流，用量与结束事件各一次，每轮一行进度，请求只登记一次完成
func TestLongOutputRuntimeRounds(t *testing.T) {
	upstream := &longRuntimeUpstream{
		streams: []string{
			longFrames("第一轮的正文。", 2, [3]int64{20, 100, 120}),
			longFrames("第二轮接着写完。", 1, [3]int64{150, 30, 180}),
		},
		delays: map[int]time.Duration{1: 200 * time.Millisecond},
	}
	service, pool := longRuntimeService(t, upstream, 150*time.Millisecond)
	upstream.pool = pool
	const window = 700 * time.Millisecond
	pool.SetRateWindowForTest(window)
	pool.SetAccountRateLimit(1)
	states := watchRequestStates(t, service)
	// 首事件前期限与首事件后空闲期限 300ms，总时长上限 800ms：整段约 1 秒
	gc := newGenerateContext(service.lifecycle, time.Now(), 300*time.Millisecond, 800*time.Millisecond)
	request := deadlineRequest("req-long-runtime")
	request.LongOutput, request.LongOutputDefaultTokens = true, 300
	result := runDeadlineRequest(t, service, states, request, gc)
	if result.err != nil || result.state != "completed" {
		t.Fatalf("err=%v state=%s，期望两轮完整结束", result.err, result.state)
	}
	if elapsed := result.endedAt.Sub(result.startedAt); elapsed < 850*time.Millisecond {
		t.Fatalf("用时 %s，测试要求整段长于总时长上限 800ms", elapsed)
	}
	text, usages, finishes := "", 0, 0
	var usage *aistudio.Usage
	for _, event := range result.events {
		switch event.Kind {
		case aistudio.EventText:
			text += event.Text
		case aistudio.EventUsage:
			usages++
			usage = event.Usage
		case aistudio.EventFinish:
			finishes++
		}
	}
	if text != "第一轮的正文。第二轮接着写完。" || usages != 1 || finishes != 1 || usage.InputTokens != 20 || usage.OutputTokens != 130 {
		t.Fatalf("正文=%q 用量=%d 次 %+v 结束=%d 次，期望一条连续的流、合并用量各一次", text, usages, usage, finishes)
	}
	accounts := upstream.accounts()
	if len(accounts) != 2 || accounts[0] != accounts[1] {
		t.Fatalf("各轮账号 = %v，期望同一账号", accounts)
	}
	if !progressLogged(service, "自动续写第 2 轮等待账号每分钟窗口") {
		t.Fatalf("第二轮应等账号每分钟窗口: %v", serviceLogMessages(service.requests))
	}
	// 后续轮次的准备 WAA proof、等响应头只算上游有进展，不再逐阶段写（第一轮照常各写一行）
	for _, line := range []string{
		"自动续写 | 第 1 轮结束 | 方式=模拟续写 | 本轮结束原因=max_tokens | 本轮输入=20 | 本轮输出=100 | 累计输出=100 | 总上限=300 | 下一步=第 2 轮（补用户消息）",
		"自动续写 | 第 2 轮结束 | 方式=模拟续写 | 本轮结束原因=stop | 本轮输入=150 | 本轮输出=30 | 累计输出=130 | 总上限=300 | 下一步=结束（上游结束）",
		"收到上游首个事件", "WAA proof 完成", "上游已返回响应头",
	} {
		if count := progressCount(service, line); count != 1 {
			t.Fatalf("进度 %q 写了 %d 次，期望 1 次: %v", line, count, serviceLogMessages(service.requests))
		}
	}
	if progressLogged(service, "本服务期限截断") || progressLogged(service, "上游首事件超时") || progressLogged(service, "被本服务结束") {
		t.Fatalf("两轮之间不应触发期限: %v", serviceLogMessages(service.requests))
	}
	// 租约在整段输出期间标记为服务 long- 请求（排空超时与后台关闭按续写的绝对总时长上限），释放后清除
	if active := upstream.longOutputActive(); len(active) != 2 || !active[0] || !active[1] || pool.LongOutputActive(accounts[0]) {
		t.Fatalf("各轮发送时的续写租约标记 = %v，结束后 = %v，期望进行中为真、结束后为假", active, pool.LongOutputActive(accounts[0]))
	}
	assertLeasesReleased(t, pool)
}

// TestPrepareLongOutput 开关关闭时清除续写标记并写一行说明（long- 前缀照常解析、按普通请求处理）；Interaction 模型同样不续写；
// 开启时填上默认总上限（热更新后新请求立即按新值）
func TestPrepareLongOutput(t *testing.T) {
	service, pool := firstEventTestService(t, &pacedService{}, 0, "alice@example.com")
	if err := pool.SetCatalog("alice@example.com", aistudio.BenefitTierFree, []aistudio.Model{
		{ID: testRuntimeModel, Methods: []string{"generateContent"}, Capabilities: map[string]bool{"chat_model": true}},
		{ID: "omni-test", Methods: []string{"generateContent"}, Capabilities: map[string]bool{"chat_model": true, "interactions_api": true}},
	}); err != nil {
		t.Fatal(err)
	}
	request := deadlineRequest("req-long-off")
	request.LongOutput = true
	service.setLongOutput(false, 300)
	service.prepareLongOutput(&request)
	if request.LongOutput || !progressLogged(service, "自动续写已关闭（LONG_OUTPUT_ENABLED=false），按普通请求处理") {
		t.Fatalf("关闭时应按普通请求处理: %+v", request)
	}
	service.setLongOutput(true, 4096)
	request = deadlineRequest("req-long-omni")
	request.Model, request.LongOutput = "omni-test", true
	service.prepareLongOutput(&request)
	if request.LongOutput || !progressLogged(service, "经 Interaction 通道生成") {
		t.Fatalf("Interaction 模型不应续写: %+v", request)
	}
	request = deadlineRequest("req-long-on")
	request.LongOutput = true
	service.prepareLongOutput(&request)
	if !request.LongOutput || request.LongOutputDefaultTokens != 4096 || !progressLogged(service, "模型名带 long- 前缀已识别，发送时按模型目录决定是否自动续写") ||
		progressLogged(service, "开启自动续写") {
		t.Fatalf("开启时应填上默认总上限: %+v", request)
	}
	plain := deadlineRequest("req-plain")
	service.prepareLongOutput(&plain)
	if plain.LongOutput || plain.LongOutputDefaultTokens != 0 || progressLogged(service, "req-plain") {
		t.Fatalf("不带 long- 的请求不应改动: %+v", plain)
	}
}

// TestLongOutputRuntimeRoundFailure 续写的第二轮发送遇到额度 429（该模型 2 小时的额度冷却）：请求按 max_tokens 正常结束，
// 第一轮的正文与用量完整交给客户端（不是错误）；账号照流内错误的规则写回冷却，这次结束不算模型资格验证成功（否则冷却被清掉）；
// 请求日志写一行原因，用量账本与请求结果各一次
func TestLongOutputRuntimeRoundFailure(t *testing.T) {
	upstream := &longRuntimeUpstream{
		streams: []string{longFrames("第一轮的正文。", 2, [3]int64{20, 100, 120})},
		errs: map[int]error{1: &aistudio.RPCError{
			Method: "GenerateContent", StatusCode: http.StatusTooManyRequests, Code: 8, Message: "You exceeded your current quota", RetryDelay: 2 * time.Hour,
		}},
	}
	service, pool := longRuntimeService(t, upstream, 0)
	states := watchRequestStates(t, service)
	gc := newGenerateContext(service.lifecycle, time.Now(), 5*time.Second, time.Minute)
	request := deadlineRequest("req-long-failure")
	request.LongOutput, request.LongOutputDefaultTokens = true, 300
	result := runDeadlineRequest(t, service, states, request, gc)
	if result.err != nil || result.errEvents != 0 || result.state != "completed" {
		t.Fatalf("err=%v 错误事件=%d state=%s，期望按 max_tokens 正常结束", result.err, result.errEvents, result.state)
	}
	text, finish := "", ""
	usages := 0
	for _, event := range result.events {
		switch event.Kind {
		case aistudio.EventText:
			text += event.Text
		case aistudio.EventUsage:
			usages++
			if event.Usage.OutputTokens != 100 {
				t.Fatalf("用量 = %+v，期望第一轮的用量", event.Usage)
			}
		case aistudio.EventFinish:
			finish = event.FinishReason
		}
	}
	if text != "第一轮的正文。" || usages != 1 || finish != "max_tokens" {
		t.Fatalf("正文=%q 用量=%d 次 结束=%q", text, usages, finish)
	}
	accounts := upstream.accounts()
	if len(accounts) != 2 || accounts[0] != accounts[1] {
		t.Fatalf("各轮账号 = %v，期望同一账号、不换号重试", accounts)
	}
	if !progressLoggedAll(service, "自动续写 | 续写第 2 轮发送失败，按 max_tokens 结束（已输出的内容完整返回）", "方式=模拟续写") {
		t.Fatalf("应写一行续写失败的原因: %v", serviceLogMessages(service.requests))
	}
	pool.WaitBackground()
	if !pool.AccountCoolingDown(accounts[0], testRuntimeModel) || pool.AccountCoolingDown(accounts[0], "") {
		t.Fatal("续写轮的额度 429 应照流内错误写回该模型的冷却，且结束后不被模型资格验证清掉")
	}
	if pool.LongOutputActive(accounts[0]) {
		t.Fatal("请求结束后不应还有续写租约")
	}
}

// TestLongOutputRuntimeRoundClientError 续写第二轮发送时或流内（流尾状态）收到带 google.rpc.BadRequest details 的上游 400：请求照样按
// max_tokens 正常结束（已输出的内容完整返回），并照流内错误的规则在执行账号名下记一行“上游返回 4xx”证据（details 原文不丢），
// 末尾带“自动续写第 2 轮”，只记一行（不再另记“已输出内容后”）
func TestLongOutputRuntimeRoundClientError(t *testing.T) {
	const badRequest = `[3,"Request contains an invalid argument.",[["type.googleapis.com/google.rpc.BadRequest",[[["contents[1]","bad"]]]]]]`
	const want = `上游返回 4xx | 方法=GenerateContent | HTTP=400 | 协议错误码=3 | 文案=Request contains an invalid argument. | ` +
		`详情类型=google.rpc.BadRequest | 详情=[["type.googleapis.com/google.rpc.BadRequest",[[["contents[1]","bad"]]]]] | 自动续写第 2 轮`
	first := longFrames("第一轮的正文。", 2, [3]int64{20, 100, 120})
	for _, test := range []struct {
		name     string
		upstream *longRuntimeUpstream
		text     string
	}{
		{name: "发送时", text: "第一轮的正文。", upstream: &longRuntimeUpstream{
			streams: []string{first},
			errs:    map[int]error{1: aistudio.DecodeRPCError("GenerateContent", http.StatusBadRequest, []byte(badRequest))},
		}},
		{name: "流内", text: "第一轮的正文。第二轮的一部分", upstream: &longRuntimeUpstream{
			streams: []string{first, `[[[[[[[[null,"第二轮的一部分"]],"model"]]]]],` + badRequest + `]`},
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			service, _ := longRuntimeService(t, test.upstream, 0)
			states := watchRequestStates(t, service)
			gc := newGenerateContext(service.lifecycle, time.Now(), 5*time.Second, time.Minute)
			request := deadlineRequest("req-long-client-error")
			request.LongOutput, request.LongOutputDefaultTokens = true, 300
			result := runDeadlineRequest(t, service, states, request, gc)
			if result.err != nil || result.errEvents != 0 || result.state != "completed" {
				t.Fatalf("err=%v 错误事件=%d state=%s，期望按 max_tokens 正常结束", result.err, result.errEvents, result.state)
			}
			text, finish := "", ""
			for _, event := range result.events {
				switch event.Kind {
				case aistudio.EventText:
					text += event.Text
				case aistudio.EventFinish:
					finish = event.FinishReason
				}
			}
			accounts := test.upstream.accounts()
			if text != test.text || finish != "max_tokens" || len(accounts) != 2 || accounts[0] != accounts[1] {
				t.Fatalf("正文=%q 结束=%q 各轮账号=%v，期望 %q、max_tokens、同一账号两轮", text, finish, accounts, test.text)
			}
			service.requests.mu.Lock()
			var evidence []string
			for _, entry := range service.requests.logs {
				if entry.Event == "request.progress" && strings.HasPrefix(entry.Message, "上游返回 4xx") {
					evidence = append(evidence, entry.Source+" "+entry.Level+" "+entry.Message)
				}
			}
			service.requests.mu.Unlock()
			if len(evidence) != 1 || evidence[0] != accounts[0]+" WARN "+want {
				t.Fatalf("证据行 = %q\n期望 %q", evidence, accounts[0]+" WARN "+want)
			}
			if !progressLoggedAll(service, "自动续写 | 续写第 2 轮", "按 max_tokens 结束（已输出的内容完整返回）") {
				t.Fatalf("应写一行续写失败的原因: %v", serviceLogMessages(service.requests))
			}
		})
	}
}

// TestLongOutputRuntimeSkipped long- 请求按模型目录不续写时（客户端给出的上限不超过单轮上限）写一行原因，一个请求最多一次
func TestLongOutputRuntimeSkipped(t *testing.T) {
	upstream := &longRuntimeUpstream{streams: []string{longFrames("一轮就够。", 2, [3]int64{20, 80, 100})}}
	service, _ := longRuntimeService(t, upstream, 0)
	states := watchRequestStates(t, service)
	gc := newGenerateContext(service.lifecycle, time.Now(), 5*time.Second, time.Minute)
	request := deadlineRequest("req-long-skipped")
	limit := int64(80)
	request.Config.MaxOutputTokens = &limit
	request.LongOutput, request.LongOutputDefaultTokens = true, 300
	result := runDeadlineRequest(t, service, states, request, gc)
	if result.err != nil || result.state != "completed" || len(upstream.accounts()) != 1 {
		t.Fatalf("err=%v state=%s 调用=%v", result.err, result.state, upstream.accounts())
	}
	if count := progressCount(service, "模型名带 long- 前缀，本次不自动续写：总上限 80 不超过模型单轮上限 100，按普通请求处理"); count != 1 {
		t.Fatalf("不续写的原因写了 %d 次，期望 1 次: %v", count, serviceLogMessages(service.requests))
	}
}

// TestLongOutputDrainAndResetLimits 账号上有服务 long- 请求的租约时，排空超时与后台关闭的等待上限按续写的绝对总时长上限计算；
// 租约释放后恢复为普通请求的总时长上限
func TestLongOutputDrainAndResetLimits(t *testing.T) {
	service, pool := firstEventTestService(t, &pacedService{}, 0, "alice@example.com")
	manager := service.workers
	manager.requestTimeout.Store(int64(time.Second))
	normal := generateMaxDuration + drainTimeoutGrace
	long := generateMaxDuration*longOutputDurationFactor + longRoundGrace + drainTimeoutGrace
	if got := manager.drainTimeoutFor("alice@example.com"); got != normal {
		t.Fatalf("没有续写租约时排空超时 = %s，期望 %s", got, normal)
	}
	lease, err := pool.AcquireFor(context.Background(), aistudio.AccountSelection{AccountID: "alice@example.com", ModelID: testRuntimeModel, Method: "generateContent"})
	if err != nil {
		t.Fatal(err)
	}
	lease.MarkLongOutput()
	if got := manager.drainTimeoutFor("alice@example.com"); got != long {
		t.Fatalf("有续写租约时排空超时 = %s，期望 %s", got, long)
	}
	if got := manager.workerResetLimitFor("alice@example.com"); got != long {
		t.Fatalf("有续写租约时后台关闭上限 = %s，期望 %s", got, long)
	}
	if got := manager.drainTimeout(); got != normal {
		t.Fatalf("不按账号的排空超时 = %s，期望不变 %s", got, normal)
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
	if got := manager.workerResetLimitFor("alice@example.com"); got != normal {
		t.Fatalf("租约释放后后台关闭上限 = %s，期望 %s", got, normal)
	}
	manager.resetLimit.Store(int64(100 * time.Millisecond))
	if got := manager.workerResetLimitFor("alice@example.com"); got != 100*time.Millisecond {
		t.Fatalf("设置 resetLimit 后上限 = %s，期望 100ms", got)
	}
}

// TestGenerateContextRenewTotalCapped 续写每轮重新计算的总时长上限不超过绝对总时长上限（总时长上限的 longOutputDurationFactor 倍），
// 到期原因里的期限为绝对总时长上限
func TestGenerateContextRenewTotalCapped(t *testing.T) {
	const maxDuration = 100 * time.Millisecond
	gc := newGenerateContext(context.Background(), time.Now(), 50*time.Millisecond, maxDuration)
	defer gc.cancel()
	if !gc.beginStreaming() {
		t.Fatal("切到首事件后失败")
	}
	deadline := time.Now().Add(maxDuration * longOutputDurationFactor)
	for time.Now().Before(deadline.Add(-maxDuration / 2)) {
		if !gc.renewTotal() {
			t.Fatalf("还没到绝对总时长上限时续期失败: %v", gc.Err())
		}
		time.Sleep(maxDuration / 4)
	}
	gc.renewTotal()
	awaitDone(t, gc, 2*maxDuration, "绝对总时长上限")
	if elapsed := time.Since(gc.startedAt); elapsed > maxDuration*longOutputDurationFactor+maxDuration/2 {
		t.Fatalf("用时 %s，期望在绝对总时长上限 %s 截断", elapsed, maxDuration*longOutputDurationFactor)
	}
	if cause := gc.deadlineCause(); cause == nil || cause.stage != serviceDeadlineTotal || cause.limit != maxDuration*longOutputDurationFactor {
		t.Fatalf("到期原因 = %+v，期望总时长上限且期限为绝对总时长上限", cause)
	}
}

// TestGenerateContextRenewTotal 续写开始新一轮时总时长上限从此刻重新计算；首事件前不改期限
func TestGenerateContextRenewTotal(t *testing.T) {
	gc := newGenerateContext(context.Background(), time.Now(), 50*time.Millisecond, 300*time.Millisecond)
	defer gc.cancel()
	if gc.renewTotal() {
		t.Fatal("首事件前不应重新计算总时长上限")
	}
	if !gc.beginStreaming() {
		t.Fatal("切到首事件后失败")
	}
	time.Sleep(200 * time.Millisecond)
	if !gc.renewTotal() {
		t.Fatal("首事件后应重新计算总时长上限")
	}
	time.Sleep(200 * time.Millisecond)
	if err := gc.Err(); err != nil {
		t.Fatalf("重新计算后 400ms 时不应到期（原期限 300ms），得到 %v", err)
	}
	awaitDone(t, gc, time.Second, "重新计算后的总时长上限")
	if cause := gc.deadlineCause(); cause == nil || cause.stage != serviceDeadlineTotal {
		t.Fatalf("到期原因 = %+v，期望总时长上限", cause)
	}
	var nilContext *generateContext
	if nilContext.renewTotal() {
		t.Fatal("nil 上下文应返回 false")
	}
}

// TestUpstreamActivityPause 暂停时空闲时间为 0；恢复时从此刻重新计
func TestUpstreamActivityPause(t *testing.T) {
	activity := &upstreamActivity{}
	progress := time.Now().Add(-time.Hour)
	activity.pause()
	if quiet := activity.quietFor(time.Now(), progress); quiet != 0 {
		t.Fatalf("暂停时空闲 = %s", quiet)
	}
	activity.resume()
	if quiet := activity.quietFor(time.Now(), progress); quiet > time.Second {
		t.Fatalf("恢复后应从此刻重新计，空闲 = %s", quiet)
	}
	if activity.bytes.Load() != 0 {
		t.Fatal("恢复计时不应计入网络字节")
	}
}
