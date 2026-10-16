package app

import (
	"context"
	"errors"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/api"
)

// queueProgressLines 返回该请求时间线里包含 fragment 的请求进度日志
func queueProgressLines(service *trackedService, requestID string, fragment string) []api.AdminLog {
	service.requests.mu.Lock()
	defer service.requests.mu.Unlock()
	var lines []api.AdminLog
	for _, entry := range service.requests.logs {
		if entry.Event == "request.progress" && entry.Request != nil && entry.Request.ID == requestID &&
			strings.Contains(entry.Message, fragment) {
			lines = append(lines, entry)
		}
	}
	return lines
}

// queueProgressCount 返回全部请求里包含 fragment、级别为 level 的请求进度日志条数
func queueProgressCount(service *trackedService, level string, fragment string) int {
	service.requests.mu.Lock()
	defer service.requests.mu.Unlock()
	count := 0
	for _, entry := range service.requests.logs {
		if entry.Event == "request.progress" && entry.Level == level && strings.Contains(entry.Message, fragment) {
			count++
		}
	}
	return count
}

type queuedResult struct {
	text string
	err  error
}

// startQueuedRequest 在后台执行一次生成请求（主路由，没有排查记录），请求上下文 timeout 后超时；返回结果通道
func startQueuedRequest(service *trackedService, requestID string, timeout time.Duration) (<-chan queuedResult, context.CancelFunc) {
	request := aistudio.GenerateRequest{
		ID: requestID, Model: testRuntimeModel,
		Contents: []aistudio.Content{{Role: aistudio.RoleUser, Parts: []aistudio.Part{{Text: "hi"}}}},
	}
	requestCtx, requestCancel := context.WithTimeout(service.lifecycle, timeout)
	events := make(chan aistudio.Event, 8)
	result := make(chan queuedResult, 1)
	go service.generateWithRetry(service.lifecycle, requestCtx, requestCancel, time.Now(), request, "", events, newGenerationDiagnostics(request, "未发送"))
	go func() {
		defer requestCancel()
		var got queuedResult
		for event := range events {
			switch event.Kind {
			case aistudio.EventText:
				got.text += event.Text
			case aistudio.EventError:
				got.err = event.Err
			}
		}
		result <- got
	}()
	return result, requestCancel
}

// queueProgressContext 返回带排队进度（宽限与间隔为给定值）的选号上下文
func queueProgressContext(ctx context.Context, requestID string, firstDelay, interval time.Duration) (context.Context, *leaseWaitProgress) {
	progress := newLeaseWaitProgress(requestID)
	progress.firstDelay, progress.interval = firstDelay, interval
	progress.beginAttempt()
	return withLeaseWaitProgress(ctx, progress), progress
}

// TestLeaseWaitProgressThrottle 排队进度的节奏：每次尝试排队满宽限才写第一条，之后每个间隔最多一条；已等待累计各次选号排队的
// 墙钟时间（换号重试接着累加，两次选号之间不算），按毫秒取整；原因每次等待都更新；所有方法对 nil 安全
func TestLeaseWaitProgressThrottle(t *testing.T) {
	progress := newLeaseWaitProgress(" req-x ")
	if progress == nil || progress.requestID != "req-x" || progress.firstDelay != queueProgressFirstDelay || progress.interval != queueProgressInterval {
		t.Fatalf("进度 = %+v，期望去掉空白的请求 ID 与默认宽限、间隔", progress)
	}
	progress.beginAttempt()
	t0 := time.Unix(1_700_000_000, 0)
	at := func(d time.Duration) time.Time { return t0.Add(d) }
	progress.track(at(0), "Worker 正在扩容")
	if _, ok := progress.due(at(0)); ok {
		t.Fatal("宽限内不应写进度")
	}
	progress.track(at(time.Second), "账号全部忙碌")
	if _, ok := progress.due(at(time.Second)); ok {
		t.Fatal("排队 1s 不应写进度")
	}
	progress.track(at(2234567*time.Microsecond), "没有空闲的 Worker 槽位")
	first, ok := progress.due(at(2234567 * time.Microsecond))
	if !first || !ok {
		t.Fatalf("排队满 2s 后应写第一条：first=%v ok=%v", first, ok)
	}
	if got := progress.message(at(2234567*time.Microsecond), " | 模型=m"); got != "排队等待 | 原因=没有空闲的 Worker 槽位 | 已等待=2.235s | 尝试=1 | 模型=m" {
		t.Fatalf("进度文案 = %q", got)
	}
	for second := 3; second < 17; second++ {
		progress.track(at(time.Duration(second)*time.Second), "账号全部忙碌")
		if _, ok := progress.due(at(time.Duration(second) * time.Second)); ok {
			t.Fatalf("第 %ds 不应重复写（间隔 15s）", second)
		}
	}
	progress.track(at(17300*time.Millisecond), "账号冷却中")
	if first, ok := progress.due(at(17300 * time.Millisecond)); first || !ok {
		t.Fatalf("距上一条满 15s 应重复写：first=%v ok=%v", first, ok)
	}
	if got := progress.message(at(17300*time.Millisecond), ""); !strings.Contains(got, "原因=账号冷却中 | 已等待=17.3s") {
		t.Fatalf("重复的进度应带当前原因与墙钟已等待：%q", got)
	}

	// 18s 时取得账号，之后到 40s 是上游处理与退避：不算排队
	progress.acquireEnded(at(18 * time.Second))
	if got := progress.waited(at(30 * time.Second)); got != 18*time.Second {
		t.Fatalf("两次选号之间已等待 = %s，期望停在 18s", got)
	}
	progress.acquireEnded(at(35 * time.Second))
	if got := progress.waited(at(35 * time.Second)); got != 18*time.Second {
		t.Fatalf("没有排队的选号结束后已等待 = %s，期望不变", got)
	}

	// 换号重试：宽限重新计，已等待接着累加
	progress.beginAttempt()
	progress.track(at(40*time.Second), "账号全部忙碌")
	if _, ok := progress.due(at(41 * time.Second)); ok {
		t.Fatal("新一次尝试同样有宽限")
	}
	if first, ok := progress.due(at(42 * time.Second)); !first || !ok {
		t.Fatal("新一次尝试排队满宽限后应写第一条")
	}
	if got := progress.message(at(42*time.Second), ""); !strings.Contains(got, "已等待=20s | 尝试=2") {
		t.Fatalf("已等待应为两次选号排队之和（18s + 2s），不含中间上游处理的 22s：%q", got)
	}

	// nil 安全
	var none *leaseWaitProgress
	none.beginAttempt()
	none.track(t0, "账号全部忙碌")
	none.acquireEnded(t0)
	if first, ok := none.due(t0); first || ok || none.message(t0, "x") != "" || none.waited(t0) != 0 {
		t.Fatal("nil 进度不应写任何东西")
	}
	if err := none.queueEnded(context.DeadlineExceeded); err != context.DeadlineExceeded {
		t.Fatalf("nil 进度应原样返回错误，得到 %v", err)
	}
	if newLeaseWaitProgress("  ") != nil {
		t.Fatal("请求 ID 为空时不应创建进度")
	}
	base := context.Background()
	if withLeaseWaitProgress(base, nil) != base || leaseWaitProgressFromContext(base) != nil || leaseRequestID(base) != "" {
		t.Fatal("没有进度时上下文应原样返回")
	}
	trace := aistudio.NewTrace()
	trace.SetRequestID("req-trace")
	traced := aistudio.ContextWithTrace(base, trace)
	if got := leaseRequestID(traced); got != "req-trace" {
		t.Fatalf("只有排查记录时请求 ID = %q，期望 req-trace", got)
	}
	if got := leaseRequestID(withLeaseWaitProgress(traced, newLeaseWaitProgress("req-main"))); got != "req-main" {
		t.Fatalf("有排队进度时请求 ID = %q，期望 req-main", got)
	}
}

// TestQueueProgressLimiterBoundsRepeats 重复的排队进度全进程限流：一开始最多放行 30 条，之后每秒补 2 条；放行时返回此前被省略的条数
func TestQueueProgressLimiterBoundsRepeats(t *testing.T) {
	var limiter queueProgressLimiter
	now := time.Unix(1_700_000_000, 0)
	allowed := 0
	for range 1000 {
		if ok, _ := limiter.allow(now); ok {
			allowed++
		}
	}
	if allowed != 30 {
		t.Fatalf("同一时刻放行 %d 条，期望 30", allowed)
	}
	if ok, suppressed := limiter.allow(now.Add(time.Second)); !ok || suppressed != 970 {
		t.Fatalf("1s 后第一条 ok=%v 省略=%d，期望放行并报告省略 970 条", ok, suppressed)
	}
	if ok, suppressed := limiter.allow(now.Add(time.Second)); !ok || suppressed != 0 {
		t.Fatalf("1s 后第二条 ok=%v 省略=%d，期望放行且不再报告", ok, suppressed)
	}
	if ok, _ := limiter.allow(now.Add(time.Second)); ok {
		t.Fatal("1s 只补 2 条，第三条应被省略")
	}
	// 时钟回退时不补令牌
	if ok, _ := limiter.allow(now); ok {
		t.Fatal("时钟回退时不应补令牌")
	}
	var none *queueProgressLimiter
	if ok, _ := none.allow(now); !ok {
		t.Fatal("nil 限流器不限流")
	}
}

// TestNoteQueueWaitSuppressedCountIsServiceLine 被全局限流省略的条数在下一次放行时另写一条服务日志（不带请求 ID），
// 不写进放行的那个请求的时间线；第一条（INFO）不受全局限流
func TestNoteQueueWaitSuppressedCountIsServiceLine(t *testing.T) {
	service, _ := primaryBusyService(t, nil, primaryTestA)
	t0 := time.Now()
	service.queueProgress.last = t0
	service.queueProgress.tokens = 0
	progress := newLeaseWaitProgress("req-limited")
	progress.firstDelay, progress.interval = 0, 100*time.Millisecond
	progress.beginAttempt()
	detail := func(time.Time) string { return " | 模型=m" }
	service.noteQueueWait(progress, nil, t0, "账号全部忙碌", detail)
	if lines := queueProgressLines(service, "req-limited", "排队等待"); len(lines) != 1 || lines[0].Level != "INFO" {
		t.Fatalf("第一条应不受全局限流照常写 INFO：%v", lines)
	}
	// 令牌为 0：100ms 后只补 0.2 条，被省略
	service.noteQueueWait(progress, nil, t0.Add(150*time.Millisecond), "账号全部忙碌", detail)
	if lines := queueProgressLines(service, "req-limited", "排队等待"); len(lines) != 1 {
		t.Fatalf("令牌用尽时重复的进度应被省略：%v", lines)
	}
	// 再过 500ms 补满 1 条：放行，并先写一条省略 1 条的服务日志
	service.noteQueueWait(progress, nil, t0.Add(700*time.Millisecond), "没有空闲的 Worker 槽位", detail)
	lines := queueProgressLines(service, "req-limited", "排队等待")
	if len(lines) != 2 || lines[1].Level != "WARN" || !strings.Contains(lines[1].Message, "原因=没有空闲的 Worker 槽位") ||
		strings.Contains(lines[1].Message, "省略") {
		t.Fatalf("放行的重复进度应为 WARN、带当前原因、不带省略数：%v", lines)
	}
	service.requests.mu.Lock()
	defer service.requests.mu.Unlock()
	for _, entry := range service.requests.logs {
		if strings.Contains(entry.Message, "排队进度日志已省略 1 条") {
			if entry.Request != nil || entry.Event != "runtime.message" || entry.Source != "service" || entry.Level != "WARN" {
				t.Fatalf("省略数应为不带请求的服务日志：%+v", entry)
			}
			return
		}
	}
	t.Fatal("应写一条省略数的服务日志")
}

// TestNonPrimaryQueueWaitLogsReasonAfterGrace 非主用模型的请求在账号全部忙碌时排队：宽限（2s）内不写，满宽限后在请求时间线写一条
// 带原因与请求 ID 的 INFO（主路由，没有排查记录），之后 15s 内不重复；账号空闲后请求照常完成
func TestNonPrimaryQueueWaitLogsReasonAfterGrace(t *testing.T) {
	service, pool := primaryBusyService(t, nil, primaryTestA)
	release := holdLease(t, pool, primaryTestA)
	defer release()
	result, cancel := startQueuedRequest(service, "req-queue-wait", 20*time.Second)
	defer cancel()
	if !waitUntil(3*time.Second, func() bool { return dispatchWaiters(service) > 0 }) {
		t.Fatal("请求应进入调度等待队列")
	}
	time.Sleep(1200 * time.Millisecond)
	if lines := queueProgressLines(service, "req-queue-wait", "排队等待"); len(lines) != 0 {
		t.Fatalf("宽限内不应写排队进度：%v", lines)
	}
	if !waitUntil(3*time.Second, func() bool { return len(queueProgressLines(service, "req-queue-wait", "排队等待")) > 0 }) {
		t.Fatal("排队满宽限后应写排队进度")
	}
	lines := queueProgressLines(service, "req-queue-wait", "排队等待 | 原因=账号全部忙碌")
	if len(lines) != 1 || lines[0].Level != "INFO" || lines[0].Source != "service" {
		t.Fatalf("排队进度 = %v，期望一条 INFO 写明账号全部忙碌", lines)
	}
	message := lines[0].Message
	if !regexp.MustCompile(`已等待=[23](\.\d+)?s \| 尝试=1 \| 模型=` + testRuntimeModel + ` \| 同条件队列=1/1 \| 热空闲=0 \| 热忙碌=1`).MatchString(message) {
		t.Fatalf("排队进度文案 = %q", message)
	}
	time.Sleep(1200 * time.Millisecond)
	if got := len(queueProgressLines(service, "req-queue-wait", "排队等待")); got != 1 {
		t.Fatalf("15s 内不应重复写，共 %d 条", got)
	}
	release()
	select {
	case got := <-result:
		if got.err != nil || got.text != "hello" {
			t.Fatalf("账号空闲后请求应完成：text=%q err=%v", got.text, got.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("账号空闲后请求没有完成")
	}
	if waiters := dispatchWaiters(service); waiters != 0 {
		t.Fatalf("请求结束后仍有 %d 个等待者", waiters)
	}
	assertLeasesReleased(t, pool)
}

// TestQueueWaitQuickLeaseLogsNothing 排队不满宽限就取得账号的请求不写排队进度
func TestQueueWaitQuickLeaseLogsNothing(t *testing.T) {
	service, pool := primaryBusyService(t, nil, primaryTestA)
	release := holdLease(t, pool, primaryTestA)
	result, cancel := startQueuedRequest(service, "req-queue-quick", 20*time.Second)
	defer cancel()
	if !waitUntil(3*time.Second, func() bool { return dispatchWaiters(service) > 0 }) {
		t.Fatal("请求应进入调度等待队列")
	}
	time.Sleep(500 * time.Millisecond)
	release()
	select {
	case got := <-result:
		if got.err != nil || got.text != "hello" {
			t.Fatalf("请求应完成：text=%q err=%v", got.text, got.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("请求没有完成")
	}
	if lines := queueProgressLines(service, "req-queue-quick", "排队等待"); len(lines) != 0 {
		t.Fatalf("很快取得账号的请求不应写排队进度：%v", lines)
	}
}

// TestQueueWaitTimeoutKeepsErrorsIs 请求在排队中超时：错误带上最后的等待原因与已等待时间，仍满足 errors.Is(context.DeadlineExceeded)
// （对外 504 不变），不留下等待者；请求被取消时同样带原因并满足 errors.Is(context.Canceled)
func TestQueueWaitTimeoutKeepsErrorsIs(t *testing.T) {
	service, pool := primaryBusyService(t, nil, primaryTestA)
	release := holdLease(t, pool, primaryTestA)
	defer release()
	result, cancel := startQueuedRequest(service, "req-queue-timeout", 700*time.Millisecond)
	defer cancel()
	var got queuedResult
	select {
	case got = <-result:
	case <-time.After(5 * time.Second):
		t.Fatal("请求超时后没有结束")
	}
	if !errors.Is(got.err, context.DeadlineExceeded) || finalRequestState(got.err) != "failed" {
		t.Fatalf("err = %v，期望仍满足 context.DeadlineExceeded", got.err)
	}
	if !regexp.MustCompile(`^排队 \d+(\.\d+)?m?s 后请求超时（最后原因=账号全部忙碌）: context deadline exceeded$`).MatchString(got.err.Error()) {
		t.Fatalf("超时错误文案 = %q，期望带最后原因与已等待", got.err.Error())
	}
	if waiters := dispatchWaiters(service); waiters != 0 {
		t.Fatalf("超时退出后仍有 %d 个等待者", waiters)
	}

	// 请求被取消
	ctx, cancelAcquire := context.WithCancel(service.lifecycle)
	ctx, _ = queueProgressContext(ctx, "req-queue-cancel", time.Hour, time.Hour)
	time.AfterFunc(300*time.Millisecond, cancelAcquire)
	lease, err := service.acquireWarmLease(ctx, aistudio.AccountSelection{ModelID: testRuntimeModel, Method: "generateContent"})
	if lease != nil || !errors.Is(err, context.Canceled) || finalRequestState(err) != "cancelled" ||
		!strings.Contains(err.Error(), "后请求已取消（最后原因=账号全部忙碌）") {
		t.Fatalf("取消时 lease=%v err=%v，期望带最后原因且仍满足 context.Canceled", lease, err)
	}

	// 没有排队过（立即取消）时原样返回 ctx 错误
	canceled, cancelNow := context.WithCancel(service.lifecycle)
	cancelNow()
	canceled, _ = queueProgressContext(canceled, "req-queue-none", 0, time.Hour)
	if _, err := service.acquireWarmLease(canceled, aistudio.AccountSelection{ModelID: testRuntimeModel, Method: "generateContent"}); err != context.Canceled {
		t.Fatalf("没有排队过时 err = %v，期望原样的 context.Canceled", err)
	}
}

// TestQueueWaitExcludesUpstreamTime 换号重试排队中超时：错误里的"排队 X"只算各次选号排队的时间，不含上一次尝试在上游处理的时间。
// 原先从第一次排队一直按墙钟算，上游处理了几分钟、换号排队十几秒就超时的请求报成"排队 5m0s 后请求超时"
func TestQueueWaitExcludesUpstreamTime(t *testing.T) {
	const (
		timeout      = 4 * time.Second
		firstQueue   = time.Second
		upstreamTime = 1500 * time.Millisecond
	)
	// primary-a 收到请求后一直不发事件，首事件超时（upstreamTime）后换号；primary-b 一直满着，换号后排队到请求超时
	upstream := newStallingService(primaryTestA)
	service, pool := primaryBusyService(t, upstream, primaryTestA, primaryTestB)
	service.firstEventTimeout.Store(int64(upstreamTime))
	releaseA := holdLease(t, pool, primaryTestA)
	releaseB := holdLease(t, pool, primaryTestB)
	defer releaseB()
	startedAt := time.Now()
	result, cancel := startQueuedRequest(service, "req-queue-upstream", timeout)
	defer cancel()
	if !waitUntil(3*time.Second, func() bool { return dispatchWaiters(service) > 0 }) {
		releaseA()
		t.Fatal("请求应进入调度等待队列")
	}
	time.Sleep(firstQueue - time.Since(startedAt))
	releaseA()
	var got queuedResult
	select {
	case got = <-result:
	case <-time.After(timeout + 5*time.Second):
		t.Fatal("请求超时后没有结束")
	}
	elapsed := time.Since(startedAt)
	if !errors.Is(got.err, context.DeadlineExceeded) {
		t.Fatalf("err = %v，期望仍满足 context.DeadlineExceeded", got.err)
	}
	match := regexp.MustCompile(`^排队 (\S+) 后请求超时（最后原因=账号全部忙碌）: context deadline exceeded$`).FindStringSubmatch(got.err.Error())
	if match == nil {
		t.Fatalf("超时错误文案 = %q", got.err.Error())
	}
	waited, err := time.ParseDuration(match[1])
	if err != nil {
		t.Fatal(err)
	}
	upstream.mu.Lock()
	calls := slices.Clone(upstream.calls)
	upstream.mu.Unlock()
	if !slices.Equal(calls, []string{primaryTestA}) {
		t.Fatalf("上游调用 = %v，期望只在 primary-a 上首事件超时一次", calls)
	}
	t.Logf("报告排队 %s，总时长 %s", waited, elapsed)
	// 排队 = 第一次排队（约 1s）+ 换号后排队（总时长减去上游处理与退避）：不含上游处理的 1.5s，也不只算第二次排队
	if limit := elapsed - upstreamTime + 300*time.Millisecond; waited > limit {
		t.Fatalf("报告排队 %s，超过总时长 %s 减去上游处理 %s：上游处理的时间被算成了排队", waited, elapsed, upstreamTime)
	}
	if waited < firstQueue+500*time.Millisecond {
		t.Fatalf("报告排队 %s，期望累计两次选号的排队（第一次约 %s）", waited, firstQueue)
	}
}

// TestPrimaryCapacityWaitWritesNoQueueLine 主用模型容量原因的排队不写排队进度（超限时已有"主用模型排队超过…"一句）；
// 冷却等待不受排队上限限制，照常写
func TestPrimaryCapacityWaitWritesNoQueueLine(t *testing.T) {
	service, pool := primaryBusyService(t, nil, primaryTestA)
	release := holdLease(t, pool, primaryTestA)
	service.setPrimaryModels([]string{testRuntimeModel}, 2500*time.Millisecond)
	text, err, _ := runPrimaryRequest(t, service, "req-primary-noline")
	var busy *aistudio.CapacityBusyError
	if text != "" || !errors.As(err, &busy) {
		t.Fatalf("text=%q err=%v，期望号池繁忙", text, err)
	}
	if !progressLogged(service, "主用模型排队超过 2.5s") {
		t.Fatal("应写主用模型排队超限的一句")
	}
	if lines := queueProgressLines(service, "req-primary-noline", "排队等待"); len(lines) != 0 {
		t.Fatalf("主用模型容量原因的排队不应写排队进度：%v", lines)
	}
	release()

	// 冷却等待：宽限为 0 时第一次等待就写，原因为账号冷却中
	until := time.Now().Add(400 * time.Millisecond)
	if err := pool.MarkCooldownIfGeneration(primaryTestA, testRuntimeModel, pool.ModelAccessGeneration(primaryTestA), time.Now(), until, "分钟限额"); err != nil {
		t.Fatal(err)
	}
	ctx, _ := queueProgressContext(service.lifecycle, "req-primary-cooling", 0, time.Hour)
	lease, err, _ := acquirePrimary(ctx, service, aistudio.PoolScopeNormal)
	if err != nil {
		t.Fatalf("冷却结束后应取得租约: %v", err)
	}
	_ = lease.Release()
	if lines := queueProgressLines(service, "req-primary-cooling", "排队等待 | 原因="+waitReasonCooling); len(lines) != 1 {
		t.Fatalf("主用模型的冷却等待应写排队进度：%v", lines)
	} else if !strings.Contains(lines[0].Message, "最早恢复=") {
		t.Fatalf("冷却等待的排队进度应带最早恢复时间：%q", lines[0].Message)
	}
}

// TestImmediateAcquireLogsNoQueueLine 不等待的选号（流式优先 Playground 的第一步）没有立即可用的账号时直接返回，不写排队进度、不记等待原因
func TestImmediateAcquireLogsNoQueueLine(t *testing.T) {
	service, pool := primaryBusyService(t, nil, primaryTestA)
	release := holdLease(t, pool, primaryTestA)
	defer release()
	ctx, progress := queueProgressContext(service.lifecycle, "req-immediate", 0, 0)
	lease, err := service.acquireWarmLeaseNow(ctx, aistudio.AccountSelection{ModelID: testRuntimeModel, Method: "generateContent"}, nil)
	var busy *noImmediateLeaseError
	if lease != nil || !errors.As(err, &busy) {
		t.Fatalf("lease=%v err=%v，期望没有立即可用的账号", lease, err)
	}
	if lines := queueProgressLines(service, "req-immediate", "排队等待"); len(lines) != 0 || progress.reason != "" {
		t.Fatalf("不等待的选号不应写排队进度：%v reason=%q", lines, progress.reason)
	}
}

// TestWaitQueuedKeepsWakeChannel waitQueued 只加日志，等待本身不变：用调用方给的通道唤醒（"Worker 容量已满"用 Worker 状态变化的通道，
// 不是调度队列的唤醒），写明原因
func TestWaitQueuedKeepsWakeChannel(t *testing.T) {
	service, _ := primaryBusyService(t, nil, primaryTestA)
	ctx, progress := queueProgressContext(service.lifecycle, "req-capacity", 0, time.Hour)
	changed := make(chan struct{}, 1)
	time.AfterFunc(100*time.Millisecond, func() { changed <- struct{}{} })
	startedAt := time.Now()
	if err := service.waitQueued(ctx, progress, nil, changed, 5*time.Second, "Worker 容量已满", nil); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(startedAt); elapsed >= time.Second {
		t.Fatalf("等待 %s 才返回，期望被通道唤醒", elapsed)
	}
	if lines := queueProgressLines(service, "req-capacity", "排队等待 | 原因=Worker 容量已满"); len(lines) != 1 {
		t.Fatalf("应写明容量已满：%v", lines)
	}
}

// TestQueueWaitRepeatsAsWarn 仍在排队的请求按间隔重复写 WARN，带当前的已等待时间
func TestQueueWaitRepeatsAsWarn(t *testing.T) {
	service, pool := primaryBusyService(t, nil, primaryTestA)
	release := holdLease(t, pool, primaryTestA)
	ctx, _ := queueProgressContext(service.lifecycle, "req-repeat", 0, 500*time.Millisecond)
	time.AfterFunc(2500*time.Millisecond, release)
	lease, err := service.acquireWarmLease(ctx, aistudio.AccountSelection{ModelID: testRuntimeModel, Method: "generateContent"})
	if err != nil {
		t.Fatal(err)
	}
	_ = lease.Release()
	lines := queueProgressLines(service, "req-repeat", "排队等待 | 原因=账号全部忙碌")
	if len(lines) < 3 || lines[0].Level != "INFO" {
		t.Fatalf("排队进度 = %v，期望第一条 INFO 后每次复查重复", lines)
	}
	for index, line := range lines[1:] {
		if line.Level != "WARN" || !regexp.MustCompile(`已等待=[1-3](\.\d+)?s`).MatchString(line.Message) {
			t.Fatalf("第 %d 条重复进度 = %+v，期望 WARN 带墙钟已等待", index+2, line)
		}
	}
}

// TestQueueProgressManyWaitersBounded 大量请求同时排队：每个请求第一条照常写，重复的经全局限流（初始 30 条、之后每秒 2 条），
// 省略数另写服务日志；账号空闲后全部取得租约，不留下等待者
func TestQueueProgressManyWaitersBounded(t *testing.T) {
	const waiters = 100
	service, pool := primaryBusyService(t, nil, primaryTestA)
	release := holdLease(t, pool, primaryTestA)
	var group sync.WaitGroup
	errs := make(chan error, waiters)
	for index := range waiters {
		group.Add(1)
		go func() {
			defer group.Done()
			ctx, cancel := context.WithTimeout(service.lifecycle, 20*time.Second)
			defer cancel()
			ctx, _ = queueProgressContext(ctx, "req-many-"+strconv.Itoa(index), 0, 100*time.Millisecond)
			lease, err := service.acquireWarmLease(ctx, aistudio.AccountSelection{ModelID: testRuntimeModel, Method: "generateContent"})
			if err != nil {
				errs <- err
				return
			}
			_ = lease.Release()
		}()
	}
	if !waitUntil(5*time.Second, func() bool { return dispatchWaiters(service) == waiters }) {
		t.Fatalf("等待者 = %d，期望 %d", dispatchWaiters(service), waiters)
	}
	time.Sleep(2500 * time.Millisecond)
	release()
	group.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("账号空闲后选号失败: %v", err)
	}
	if got := queueProgressCount(service, "INFO", "排队等待"); got != waiters {
		t.Fatalf("第一条排队进度 %d 条，期望每个请求一条（%d）", got, waiters)
	}
	if got := queueProgressCount(service, "WARN", "排队等待"); got < 1 || got > 30+2*4+5 {
		t.Fatalf("重复的排队进度 %d 条，期望受全局限流", got)
	}
	if !hasLogContaining(service.requests, "排队进度日志已省略") {
		t.Fatal("应写被省略条数的服务日志")
	}
	if left := dispatchWaiters(service); left != 0 {
		t.Fatalf("选号结束后仍有 %d 个等待者", left)
	}
	assertLeasesReleased(t, pool)
}

// TestAcquireLeaseReclaimLogsWithoutTrace 主路由（没有排查记录）的请求现场回收死槽位时，回收记录同样写进请求时间线
func TestAcquireLeaseReclaimLogsWithoutTrace(t *testing.T) {
	manager, pool, requests := partitionTestManager(t, []string{partitionNormalA, partitionNormalB, partitionNormalC}, nil, 2, 2, 0, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	service := retireTestService(ctx, manager, pool, requests)
	for _, accountID := range []string{partitionNormalA, partitionNormalB} {
		if _, err := manager.promote(ctx, accountID, testRuntimeModel); err != nil {
			t.Fatal(err)
		}
		coolGlobally(t, pool, accountID)
	}
	acquireCtx, _ := queueProgressContext(ctx, "req-reclaim-main", queueProgressFirstDelay, queueProgressInterval)
	lease, err := service.acquireWarmLease(acquireCtx, aistudio.AccountSelection{ModelID: testRuntimeModel, Method: "generateContent"})
	if err != nil {
		t.Fatalf("选号失败: %v", err)
	}
	defer lease.Release()
	if lease.Account().ID != partitionNormalC {
		t.Fatalf("应启动备用账号 C，实际 %s", lease.Account().ID)
	}
	if lines := queueProgressLines(service, "req-reclaim-main", "回收冷却占位"); len(lines) != 1 {
		t.Fatalf("主路由的请求时间线应有回收记录：%v", serviceLogMessages(requests))
	}
}
