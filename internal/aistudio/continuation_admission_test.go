package aistudio

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// long- 自动续写下一轮的号池准入（resend_admission.go，与重新生成共用）与续写轮的其他细节的测试

// waitedFor 判断进度里第 round 轮报告过因 reason 等待
func waitedFor(progress []ContinuationProgress, round int, reason ContinuationWaitReason) bool {
	for _, item := range progress {
		if item.Stage == ContinuationWaiting && item.Round == round && item.WaitReason == reason && item.Wait > 0 {
			return true
		}
	}
	return false
}

// sendRecorder 为假上游的发送钩子：像 WorkerProtectedTransport 一样经租约记一次发送（beginGenerationSend），记下发送后账号窗口内的发送数
type sendRecorder struct {
	t     *testing.T
	pool  *AccountPool
	mu    sync.Mutex
	lives []int
	times []time.Time
	errs  []error
}

func (recorder *sendRecorder) send(ctx context.Context) error {
	lease, ok := AccountLeaseFromContext(ctx)
	if !ok {
		return nil
	}
	err := lease.beginGenerationSend(indexTestModel)
	defer func() {
		recorder.mu.Lock()
		defer recorder.mu.Unlock()
		recorder.errs = append(recorder.errs, err)
	}()
	live, _, _, _ := rateSnapshot(recorder.pool, lease.Account().ID)
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	recorder.lives = append(recorder.lives, live)
	recorder.times = append(recorder.times, time.Now())
	return err
}

// record 记一次不经续写的发送（同账号其他租约）
func (recorder *sendRecorder) record(lease *AccountLease) {
	lease.RecordGenerationSent()
	live, _, _, _ := rateSnapshot(recorder.pool, lease.Account().ID)
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	recorder.lives = append(recorder.lives, live)
	recorder.times = append(recorder.times, time.Now())
	recorder.errs = append(recorder.errs, nil)
}

func (recorder *sendRecorder) snapshot() ([]int, []time.Time, []error) {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	return slices.Clone(recorder.lives), slices.Clone(recorder.times), slices.Clone(recorder.errs)
}

// observeProgress 在 ctx 上登记进度接收方：每条进度先交给 hook（可为 nil）再记下
func observeProgress(ctx context.Context, hook func(ContinuationProgress)) (context.Context, func() []ContinuationProgress) {
	var mu sync.Mutex
	var progress []ContinuationProgress
	ctx = ContextWithContinuationObserver(ctx, func(item ContinuationProgress) {
		if hook != nil {
			hook(item)
		}
		mu.Lock()
		progress = append(progress, item)
		mu.Unlock()
	})
	return ctx, func() []ContinuationProgress {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(progress)
	}
}

// runLongWith 发送请求并汇总事件（进度由调用方经 observeProgress 登记）
func runLongWith(t *testing.T, client *Client, ctx context.Context, request GenerateRequest) longSummary {
	t.Helper()
	events, err := client.Generate(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	return summarizeLong(longCollect(t, events))
}

// TestContinuationCountsOtherLeasePending 续写的下一轮按“已发出 + 其他租约的预占”判断每分钟窗口：上限 2，long 请求 A 第一轮发出后
// 同账号的租约 B 还没发出（预占 1），A 的第二轮要等 A 第一轮的发送过期，窗口内的发送数始终不超过 2；A 等待期间重新预占的名额让
// 新的选号看到窗口已满。原先只数已发出的请求，A 立即发出第二轮，B 随后发出，窗口内 3 次
func TestContinuationCountsOtherLeasePending(t *testing.T) {
	const window = 600 * time.Millisecond
	pool := newRateTestPool(t, window, 2, 3, indexTestAccount(longTestAccount, BenefitTierFree, indexTestCatalog()))
	selection := AccountSelection{ModelID: indexTestModel, Method: "generateContent"}
	leaseA := mustTryAcquire(t, pool, selection)
	defer func() { _ = leaseA.Release() }()
	leaseB := mustTryAcquire(t, pool, selection)
	defer func() { _ = leaseB.Release() }()
	recorder := &sendRecorder{t: t, pool: pool}
	model, defaults := longPlainModel("plain-test")
	upstream := &longUpstream{
		streams: []string{
			longStream(longTextFrame("一", false), longFinishFrame(2, "", 10, 100, 110)),
			longStream(longTextFrame("二", false), longFinishFrame(1, "", 10, 5, 15)),
		},
		gate: recorder.send,
	}
	var bSent sync.Once
	blockedNew := true
	ctx, progress := observeProgress(ContextWithAccountLease(context.Background(), leaseA), func(item ContinuationProgress) {
		if item.Stage != ContinuationWaiting {
			return
		}
		bSent.Do(func() {
			// A 等待中：重新预占的名额让窗口对新的选号已满；随后 B 发出
			if lease, _, _ := pool.TryAcquireFor(context.Background(), selection); lease != nil {
				blockedNew = false
				_ = lease.Release()
			}
			recorder.record(leaseB)
		})
	})
	request := longRequest(model.ID, true)
	request.LongOutputDefaultTokens = 1000
	summary := runLongWith(t, longClient(t, upstream, model, defaults), ctx, request)
	bSent.Do(func() { recorder.record(leaseB) })
	if summary.text != "一二" || len(summary.errs) != 0 {
		t.Fatalf("正文=%q 错误=%v", summary.text, summary.errs)
	}
	lives, _, _ := recorder.snapshot()
	if len(lives) != 3 || slices.Max(lives) > 2 {
		t.Fatalf("每次发送后窗口内的发送数 = %v，期望 3 次且始终不超过上限 2", lives)
	}
	if !waitedFor(progress(), 2, ContinuationWaitRate) || !blockedNew {
		t.Fatalf("第 2 轮应因其他租约的预占等窗口（等待中新选号取不到租约=%v）: %+v", blockedNew, progress())
	}
	if _, _, pending, _ := rateSnapshot(pool, longTestAccount); pending != 0 {
		t.Fatalf("全部发出后预占 = %d，期望 0", pending)
	}
}

// TestContinuationWaitHoldsReservation 续写的下一轮在等号池准入期间为租约占着一个每分钟名额（waitNextRound 的 ReserveContinuation）：
// 上限 1，第一轮以 max_tokens 结束后第二轮等第一轮的发送过期；等待期间账号的预占为 1、续写登记 1 项，第一轮的发送过期的那一刻
// （测试把窗口缩到 1ms）同账号新的选号仍取不到这个账号，不会与这一轮同时被准入、窗口内超过上限；第二轮发出后预占与登记都清空。
// 原先没有这次预占时，窗口一过期新的租约与第二轮会同时准入，上游返回分钟限额 429，这一轮失败、账号被冷却
func TestContinuationWaitHoldsReservation(t *testing.T) {
	const window = 400 * time.Millisecond
	pool := newRateTestPool(t, window, 1, 2, indexTestAccount(longTestAccount, BenefitTierFree, indexTestCatalog()))
	selection := AccountSelection{ModelID: indexTestModel, Method: "generateContent"}
	lease := mustTryAcquire(t, pool, selection)
	defer func() { _ = lease.Release() }()
	recorder := &sendRecorder{t: t, pool: pool}
	model, defaults := longPlainModel("plain-test")
	upstream := &longUpstream{
		streams: []string{
			longStream(longTextFrame("一", false), longFinishFrame(2, "", 10, 100, 110)),
			longStream(longTextFrame("二", false), longFinishFrame(1, "", 10, 5, 15)),
		},
		gate: recorder.send,
	}
	var once sync.Once
	var waiting struct {
		live, pending, continuing int
		admitted                  bool
	}
	ctx, progress := observeProgress(ContextWithAccountLease(context.Background(), lease), func(item ContinuationProgress) {
		if item.Stage != ContinuationWaiting || item.Round != 2 {
			return
		}
		once.Do(func() {
			// 第二轮等待中：第一轮的发送过期（窗口缩到 1ms），这一轮还没重新检查准入
			time.Sleep(5 * time.Millisecond)
			pool.SetRateWindowForTest(time.Millisecond)
			waiting.live, _, waiting.pending, _ = rateSnapshot(pool, longTestAccount)
			pool.mu.Lock()
			waiting.continuing = len(pool.byID[longTestAccount].rate.continuing)
			pool.mu.Unlock()
			if other, _, _ := pool.TryAcquireFor(context.Background(), selection); other != nil {
				waiting.admitted = true
				_ = other.Release()
			}
		})
	})
	request := longRequest(model.ID, true)
	request.LongOutputDefaultTokens = 1000
	summary := runLongWith(t, longClient(t, upstream, model, defaults), ctx, request)
	if summary.text != "一二" || len(summary.errs) != 0 || len(upstream.snapshot()) != 2 {
		t.Fatalf("正文=%q 错误=%v 调用=%d，期望两轮完整结束", summary.text, summary.errs, len(upstream.snapshot()))
	}
	if !waitedFor(progress(), 2, ContinuationWaitRate) {
		t.Fatalf("第 2 轮应等账号每分钟窗口: %+v", progress())
	}
	if waiting.live != 0 || waiting.pending != 1 || waiting.continuing != 1 || waiting.admitted {
		t.Fatalf("第 2 轮等待期间：窗口内发送=%d 预占=%d 续写登记=%d 新选号取得租约=%v，期望 0、1、1、取不到",
			waiting.live, waiting.pending, waiting.continuing, waiting.admitted)
	}
	pool.mu.Lock()
	pending, continuing := pool.byID[longTestAccount].rate.pending, len(pool.byID[longTestAccount].rate.continuing)
	pool.mu.Unlock()
	if pending != 0 || continuing != 0 {
		t.Fatalf("第 2 轮发出后预占=%d 续写登记=%d，期望都为 0", pending, continuing)
	}
}

// TestContinuationReservationOrder 同账号两个续写预占按登记先后发出：上限 1 时先登记的不等后登记的（否则两个续写互相等对方的预占），
// 后登记的等先登记的发出、再等它的发送过期；释放后登记清空
func TestContinuationReservationOrder(t *testing.T) {
	const window = 400 * time.Millisecond
	pool := newRateTestPool(t, time.Millisecond, 0, 2, indexTestAccount(longTestAccount, BenefitTierFree, indexTestCatalog()))
	leaseA := mustTryAcquire(t, pool, rateTestSelection)
	leaseB := mustTryAcquire(t, pool, rateTestSelection)
	leaseA.RecordGenerationSent()
	leaseB.RecordGenerationSent()
	// 两个租约的第一轮都已发出并过了窗口
	time.Sleep(5 * time.Millisecond)
	pool.SetRateWindowForTest(time.Millisecond)
	pool.SetRateWindowForTest(window)
	if live, _, _, _ := rateSnapshot(pool, longTestAccount); live != 0 {
		t.Fatalf("窗口内还有 %d 次发送", live)
	}
	pool.SetAccountRateLimit(1)
	leaseA.ReserveContinuation()
	leaseB.ReserveContinuation()
	if admission := leaseA.continuationAdmission(indexTestModel, time.Now()); admission.wait != 0 {
		t.Fatalf("先登记的续写等了 %s，期望不等后登记的续写预占", admission.wait)
	}
	if admission := leaseB.continuationAdmission(indexTestModel, time.Now()); admission.wait <= 0 || admission.reason != ContinuationWaitRate {
		t.Fatalf("后登记的续写应等先登记的发出，得到 %s %q", admission.wait, admission.reason)
	}
	leaseA.RecordGenerationSent()
	if admission := leaseB.continuationAdmission(indexTestModel, time.Now()); admission.wait < window/2 || admission.reason != ContinuationWaitRate {
		t.Fatalf("先登记的发出后，后登记的应等它的发送过期，得到 %s %q", admission.wait, admission.reason)
	}
	if err := leaseB.Release(); err != nil {
		t.Fatal(err)
	}
	if err := leaseA.Release(); err != nil {
		t.Fatal(err)
	}
	pool.mu.Lock()
	rate := pool.byID[longTestAccount].rate
	pending, continuing := rate.pending, len(rate.continuing)
	pool.mu.Unlock()
	if pending != 0 || continuing != 0 {
		t.Fatalf("释放后预占=%d 续写登记=%d，期望都为 0", pending, continuing)
	}
	// 已释放的租约不再预占
	leaseA.ReserveContinuation()
	if _, _, pending, _ := rateSnapshot(pool, longTestAccount); pending != 0 {
		t.Fatalf("已释放的租约预占了 %d 个名额", pending)
	}
}

// TestPrefillRejectedResendWaitsWindow 原生预填充被 400 拒绝后的重发同样等账号每分钟窗口（被拒的那次已计入窗口）：上限 2 时三次发送后
// 窗口内不超过 2；并在进程内记住这个模型，之后的请求直接补用户消息，不再白试一次
func TestPrefillRejectedResendWaitsWindow(t *testing.T) {
	const window = 600 * time.Millisecond
	pool := newRateTestPool(t, window, 2, 2, indexTestAccount(longTestAccount, BenefitTierFree, indexTestCatalog()))
	lease := mustTryAcquire(t, pool, AccountSelection{ModelID: indexTestModel, Method: "generateContent", AccountID: longTestAccount})
	defer func() { _ = lease.Release() }()
	recorder := &sendRecorder{t: t, pool: pool}
	model, defaults := longPlainModel("gemini-3-flash-preview")
	forgetPrefillRejection(t, model.ID)
	rejected := &RPCError{Method: "GenerateContent", StatusCode: http.StatusBadRequest, Code: 3, Message: "Requests ending with a model turn are not supported"}
	upstream := &longUpstream{
		streams: []string{
			longStream(longTextFrame("第一段", false), longFinishFrame(2, "", 10, 100, 110)),
			"",
			longStream(longTextFrame("第二段", false), longFinishFrame(1, "", 10, 5, 15)),
			longStream(longTextFrame("再一段", false), longFinishFrame(2, "", 10, 100, 110)),
			longStream(longTextFrame("又一段", false), longFinishFrame(1, "", 10, 5, 15)),
		},
		errs: map[int]error{1: rejected},
		gate: recorder.send,
	}
	client := longClient(t, upstream, model, defaults)
	ctx, progress := observeProgress(ContextWithAccountLease(context.Background(), lease), nil)
	request := longRequest(model.ID, true)
	request.LongOutputDefaultTokens = 300
	summary := runLongWith(t, client, ctx, request)
	if summary.text != "第一段第二段" || len(summary.errs) != 0 {
		t.Fatalf("正文=%q 错误=%v", summary.text, summary.errs)
	}
	lives, _, _ := recorder.snapshot()
	if len(lives) != 3 || lives[0] != 1 || lives[1] != 2 || lives[2] > 2 {
		t.Fatalf("每次发送后窗口内的发送数 = %v，期望重发等窗口释放、不超过上限 2", lives)
	}
	if !waitedFor(progress(), 2, ContinuationWaitRate) {
		t.Fatalf("重发前应等账号每分钟窗口: %+v", progress())
	}
	// 之后的请求：直接补用户消息，只发两次
	summary = runLongWith(t, client, context.Background(), request)
	calls := upstream.snapshot()
	if summary.text != "再一段又一段" || len(calls) != 5 {
		t.Fatalf("正文=%q 调用=%d，期望第二个请求只发两次", summary.text, len(calls))
	}
	if contents := calls[4].request.Contents; len(contents) != 3 || contents[2].Role != RoleUser || contents[2].Parts[0].Text != PrefillContinuationPrompt {
		t.Fatalf("记住拒绝后应直接补用户消息: %+v", contents)
	}
	// 与客户端预填充共用 prefill.go 的记忆：按“模型 + 通道”（不分大小写、去掉 models/）保留 prefillRejectionTTL
	if !prefillRecentlyRejected(ChannelPlayground, "models/GEMINI-3-flash-preview", time.Now()) ||
		prefillRecentlyRejected(ChannelBuild, model.ID, time.Now()) ||
		prefillRecentlyRejected(ChannelPlayground, model.ID, time.Now().Add(prefillRejectionTTL+time.Second)) {
		t.Fatal("拒绝记录应按模型与通道（不分大小写、去掉 models/）保留 prefillRejectionTTL")
	}
}

// TestContinuationEgressToken 出口被判定限流（令牌桶限速）时续写的下一轮同样消耗一个令牌：令牌桶已空时等到令牌可用再发，
// 消耗的令牌让之后的选择排在它后面；没有限速时不等
func TestContinuationEgressToken(t *testing.T) {
	pool := newEgressTestPool(2, longTestAccount)
	pool.mu.Lock()
	pool.egressWindow = 600 * time.Millisecond
	pool.mu.Unlock()
	pool.SetEgressRateLimit("", EgressLaneNormal, 6)
	rate := egressLane(pool, "", EgressLaneNormal)
	// 指定账户的选号不受出口门控，照常预占
	lease := mustTryAcquire(t, pool, AccountSelection{ModelID: indexTestModel, Method: "generateContent", AccountID: longTestAccount})
	defer func() { _ = lease.Release() }()
	recorder := &sendRecorder{t: t, pool: pool}
	model, defaults := longPlainModel("plain-test")
	upstream := &longUpstream{
		streams: []string{
			longStream(longTextFrame("一", false), longFinishFrame(2, "", 10, 100, 110)),
			longStream(longTextFrame("二", false), longFinishFrame(1, "", 10, 5, 15)),
		},
		gate: recorder.send,
	}
	const hold = 300 * time.Millisecond
	var drainedAt time.Time
	ctx, progress := observeProgress(ContextWithAccountLease(context.Background(), lease), func(item ContinuationProgress) {
		if item.Stage == ContinuationRoundEnd && item.Next {
			// 第一轮结束时出口令牌桶已空 hold
			drainEgress(pool, rate, hold)
			drainedAt = time.Now()
		}
	})
	request := longRequest(model.ID, true)
	request.LongOutputDefaultTokens = 1000
	summary := runLongWith(t, longClient(t, upstream, model, defaults), ctx, request)
	if summary.text != "一二" || len(summary.errs) != 0 {
		t.Fatalf("正文=%q 错误=%v", summary.text, summary.errs)
	}
	_, times, _ := recorder.snapshot()
	if len(times) != 2 || times[1].Sub(drainedAt) < hold*3/4 {
		t.Fatalf("第 2 轮在令牌桶清空后 %s 发出，期望等令牌（约 %s）", times[len(times)-1].Sub(drainedAt), hold)
	}
	if !waitedFor(progress(), 2, ContinuationWaitEgress) {
		t.Fatalf("第 2 轮应报告等出口令牌: %+v", progress())
	}
	if status, _ := egressLaneSnapshot(pool, "", EgressLaneNormal); status.Pending != 0 || status.Sent1m != 2 {
		t.Fatalf("出口通道 预占=%d 发送=%d，期望 0 与 2", status.Pending, status.Sent1m)
	}
	// 关闭限速后续写不等出口
	pool.SetEgressRateLimit("", EgressLaneNormal, 0)
	lease.ReserveContinuation()
	if admission := lease.continuationAdmission(indexTestModel, time.Now()); admission.wait != 0 {
		t.Fatalf("没有出口限速时等了 %s（%s）", admission.wait, admission.reason)
	}
}

// TestContinuationShortCooldownWait 轮与轮交界时账号刚进入分钟限额这类短冷却：续写的下一轮等冷却结束再发（不直接发送后失败）；
// 长冷却（每日限额）不等，发送被冷却拦下后按 max_tokens 结束、已输出内容完整返回
func TestContinuationShortCooldownWait(t *testing.T) {
	model, defaults := longPlainModel("plain-test")
	for _, test := range []struct {
		name   string
		hold   time.Duration
		reason string
		short  bool
	}{
		{name: "分钟限额", hold: 300 * time.Millisecond, reason: MinuteQuotaKind + ": test", short: true},
		{name: "每日限额", hold: 10 * time.Hour, reason: DailyQuotaKind + ": test"},
	} {
		pool := newRateTestPool(t, time.Minute, 0, 2, indexTestAccount(longTestAccount, BenefitTierFree, indexTestCatalog()))
		lease := mustTryAcquire(t, pool, AccountSelection{ModelID: indexTestModel, Method: "generateContent", AccountID: longTestAccount})
		recorder := &sendRecorder{t: t, pool: pool}
		upstream := &longUpstream{
			streams: []string{
				longStream(longTextFrame("一", false), longFinishFrame(2, "", 10, 100, 110)),
				longStream(longTextFrame("二", false), longFinishFrame(1, "", 10, 5, 15)),
			},
			gate: recorder.send,
		}
		var until time.Time
		ctx, progress := observeProgress(ContextWithAccountLease(context.Background(), lease), func(item ContinuationProgress) {
			if item.Stage == ContinuationRoundEnd && item.Next {
				until = time.Now().Add(test.hold)
				if err := pool.MarkCooldownIfGeneration(longTestAccount, "", pool.ModelAccessGeneration(longTestAccount), time.Now(), until, test.reason); err != nil {
					t.Errorf("%s: 写冷却: %v", test.name, err)
				}
			}
		})
		request := longRequest(model.ID, true)
		request.LongOutputDefaultTokens = 1000
		summary := runLongWith(t, longClient(t, upstream, model, defaults), ctx, request)
		_, times, errs := recorder.snapshot()
		if test.short {
			if summary.text != "一二" || len(times) != 2 || errs[1] != nil || times[1].Before(until) {
				t.Fatalf("%s: 正文=%q 发送=%v 错误=%v，期望等冷却结束后发出第 2 轮", test.name, summary.text, times, errs)
			}
			if !waitedFor(progress(), 2, ContinuationWaitCooldown) {
				t.Fatalf("%s: 应报告等账号短冷却: %+v", test.name, progress())
			}
		} else {
			var cooling *AccountCoolingDownError
			if summary.text != "一" || len(errs) != 2 || !errors.As(errs[1], &cooling) || len(summary.errs) != 0 ||
				len(summary.finishes) != 1 || summary.finishes[0] != "max_tokens" {
				t.Fatalf("%s: 正文=%q 发送错误=%v 汇总=%+v，期望不等、被冷却拦下后按 max_tokens 结束", test.name, summary.text, errs, summary)
			}
			if waitedFor(progress(), 2, ContinuationWaitCooldown) {
				t.Fatalf("%s: 长冷却不应等待", test.name)
			}
		}
		_ = lease.Release()
	}
}

// TestLowerContinuationThinking 模拟续写后续轮次的思考强度只降不升：客户端关闭思考（预算 0）时只支持预算的模型第二轮仍按 0 发送
// （原先改成 minimal，换算为预算 128）；客户端要求较高的思考时降为 minimal
func TestLowerContinuationThinking(t *testing.T) {
	budget := func(v int64) *int64 { return &v }
	for _, test := range []struct {
		config GenerationConfig
		effort string
		budget *int64
	}{
		{config: GenerationConfig{ThinkingBudget: budget(0)}, budget: budget(0)},
		{config: GenerationConfig{ThinkingBudget: budget(64)}, budget: budget(64)},
		{config: GenerationConfig{ReasoningEffort: "none"}, effort: "none"},
		{config: GenerationConfig{ReasoningEffort: "disabled"}, effort: "disabled"},
		{config: GenerationConfig{ReasoningEffort: "MINIMAL"}, effort: "MINIMAL"},
		{config: GenerationConfig{ReasoningEffort: "high"}, effort: "minimal"},
		{config: GenerationConfig{ThinkingBudget: budget(8192)}, effort: "minimal"},
		{config: GenerationConfig{ThinkingBudget: budget(-1)}, effort: "minimal"},
		{config: GenerationConfig{}, effort: "minimal"},
	} {
		got := lowerContinuationThinking(test.config)
		if got.ReasoningEffort != test.effort || (got.ThinkingBudget == nil) != (test.budget == nil) ||
			got.ThinkingBudget != nil && *got.ThinkingBudget != *test.budget {
			t.Fatalf("%+v → effort=%q budget=%v，期望 %q %v", test.config, got.ReasoningEffort, got.ThinkingBudget, test.effort, test.budget)
		}
	}
	model, defaults := longPlainModel("budget-test")
	defaults.ThinkingBudget = true
	upstream := &longUpstream{streams: []string{
		longStream(longTextFrame("一", false), longFinishFrame(2, "", 10, 100, 110)),
		longStream(longTextFrame("二", false), longFinishFrame(1, "", 10, 5, 15)),
	}}
	request := longRequest(model.ID, true)
	request.LongOutputDefaultTokens = 1000
	request.Config.ThinkingBudget = budget(0)
	_, calls, _ := runLong(t, longClient(t, upstream, model, defaults), upstream, context.Background(), request)
	if len(calls) != 2 {
		t.Fatalf("调用 %d 次", len(calls))
	}
	for index, call := range calls {
		thinking, ok := wireConfig(t, call.body)[16].([]any)
		if !ok || len(thinking) < 2 || thinking[1] != float64(0) {
			t.Fatalf("第 %d 轮思考设置 = %v，期望保留客户端的预算 0", index+1, wireConfig(t, call.body)[16])
		}
	}
}

// TestLongOutputSkippedReason long- 请求不续写时报告一次原因（按普通请求发送）；续写的请求不报告
func TestLongOutputSkippedReason(t *testing.T) {
	model, defaults := longPlainModel("plain-test")
	limit := int64(80)
	upstream := &longUpstream{streams: []string{longStream(longTextFrame("一", false), longFinishFrame(2, "", 10, 80, 90))}}
	request := longRequest(model.ID, true)
	request.Config.MaxOutputTokens = &limit
	request.LongOutputDefaultTokens = 300
	_, calls, progress := runLong(t, longClient(t, upstream, model, defaults), upstream, context.Background(), request)
	if len(calls) != 1 || len(progress) != 1 || progress[0].Stage != ContinuationSkipped ||
		!strings.Contains(progress[0].Stop, "总上限 80 不超过模型单轮上限 100") {
		t.Fatalf("调用=%d 进度=%+v，期望报告一次不续写的原因", len(calls), progress)
	}
	upstream = &longUpstream{streams: []string{longStream(longTextFrame("一", false), longFinishFrame(1, "", 10, 5, 15))}}
	request = longRequest(model.ID, false)
	if _, _, progress := runLong(t, longClient(t, upstream, model, defaults), upstream, context.Background(), request); len(progress) != 0 {
		t.Fatalf("不带 long- 的请求不应报告进度: %+v", progress)
	}
}

// TestMarkLongOutput 服务 long- 请求的租约计入账户的续写租约数，释放时退出；续写预占同样标记；已释放的租约不再标记
func TestMarkLongOutput(t *testing.T) {
	pool := newRateTestPool(t, time.Minute, 0, 2, indexTestAccount(longTestAccount, BenefitTierFree, indexTestCatalog()))
	first := mustTryAcquire(t, pool, rateTestSelection)
	second := mustTryAcquire(t, pool, rateTestSelection)
	if pool.LongOutputActive(longTestAccount) {
		t.Fatal("没有续写租约时不应为真")
	}
	first.MarkLongOutput()
	first.MarkLongOutput()
	second.ReserveContinuation()
	if !pool.LongOutputActive(longTestAccount) {
		t.Fatal("标记后应为真")
	}
	if err := first.Release(); err != nil {
		t.Fatal(err)
	}
	if !pool.LongOutputActive(longTestAccount) {
		t.Fatal("还有一个续写租约时应为真")
	}
	if err := second.Release(); err != nil {
		t.Fatal(err)
	}
	first.MarkLongOutput()
	if pool.LongOutputActive(longTestAccount) || pool.LongOutputActive("missing@example.com") {
		t.Fatal("全部释放后（已释放的租约再标记也不算）应为假")
	}
}
