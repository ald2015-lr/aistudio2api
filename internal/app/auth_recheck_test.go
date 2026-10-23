package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/camoufoxnative"
)

// probeTestService 给测试服务加上登录复核（loginProber）：results 为各账号复核的结果（没有时为通过），calls 记录复核的账号；
// 其余方法来自内嵌的服务（复核测试不会调用）
type probeTestService struct {
	aistudio.Service
	mu      sync.Mutex
	results map[string]error
	calls   []string
}

func newProbeTestService(inner aistudio.Service) *probeTestService {
	return &probeTestService{Service: inner, results: make(map[string]error)}
}

func (service *probeTestService) ProbeLogin(_ context.Context, lease *aistudio.AccountLease) error {
	service.mu.Lock()
	defer service.mu.Unlock()
	accountID := lease.Account().ID
	service.calls = append(service.calls, accountID)
	return service.results[accountID]
}

func (service *probeTestService) set(accountID string, err error) {
	service.mu.Lock()
	service.results[accountID] = err
	service.mu.Unlock()
}

// probed 返回复核过 accountID 的次数
func (service *probeTestService) probed(accountID string) int {
	service.mu.Lock()
	defer service.mu.Unlock()
	count := 0
	for _, called := range service.calls {
		if called == accountID {
			count++
		}
	}
	return count
}

// recheckTestService 在 unschedulableTestService 之上挂上登录复核器（各项间隔改小）并启动复核循环，返回服务、复核器与复核服务
func recheckTestService(
	t *testing.T, ctx context.Context, manager *accountWorkerManager, pool *aistudio.AccountPool, requests *requestRegistry,
) (*trackedService, *authRecheck, *probeTestService) {
	t.Helper()
	service := unschedulableTestService(ctx, manager, pool, requests)
	prober := newProbeTestService(service.service)
	service.service = prober
	recheck := newAuthRecheck(service)
	if recheck == nil {
		t.Fatal("实现了 ProbeLogin 的服务应创建复核器")
	}
	recheck.interval = time.Millisecond
	recheck.suspectRe = 20 * time.Millisecond
	service.authRecheck = recheck
	pool.SetAuthFailureGate(recheck.gate)
	t.Cleanup(func() { pool.SetAuthFailureGate(nil) })
	startRecheckLoop(t, ctx, recheck)
	return service, recheck, prober
}

// startRecheckLoop 启动复核循环并等它开始；测试结束时先停下循环、等进行中的复核写完，再删临时目录
// （Cleanup 后进先出，临时目录在更早注册），免得复核写到已删除的账户目录
func startRecheckLoop(t *testing.T, ctx context.Context, recheck *authRecheck) {
	t.Helper()
	runCtx, stop := context.WithCancel(ctx)
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		recheck.run(runCtx)
	}()
	t.Cleanup(func() {
		stop()
		<-stopped
	})
	if !waitUntil(time.Second, func() bool { return recheck.active.Load() > 0 }) {
		t.Fatal("复核循环没有启动")
	}
}

// suspectAccountByGeneration 模拟生成请求的上游 401：经租约路径交给 gate，返回是否被直接标记
func suspectAccountByGeneration(t *testing.T, pool *aistudio.AccountPool, accountID string) bool {
	t.Helper()
	lease, err := pool.AcquireFor(context.Background(), aistudio.AccountSelection{AccountID: accountID, ModelID: testRuntimeModel})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lease.Release() }()
	marked, err := lease.MarkAuthenticationFailure(authFailureError())
	if err != nil {
		t.Fatal(err)
	}
	return marked
}

// suspectCooldown 返回账号全局冷却的原因（没有冷却时为空）
func suspectCooldown(t *testing.T, pool *aistudio.AccountPool, accountID string) string {
	t.Helper()
	for _, cooldown := range testStatus(t, pool, accountID).Cooldowns {
		if strings.HasPrefix(cooldown.Reason, authSuspectReasonPrefix) {
			return cooldown.Reason
		}
	}
	return ""
}

// TestAuthSuspectRecheckClears 上游 401 只记疑似登录失效、写短冷却，不标需要登录；复核通过后清掉疑似冷却，账号照常可调度
func TestAuthSuspectRecheckClears(t *testing.T) {
	manager, pool, requests := partitionTestManager(t, []string{partitionNormalA}, nil, 1, 1, 0, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, recheck, prober := recheckTestService(t, ctx, manager, pool, requests)
	if suspectAccountByGeneration(t, pool, partitionNormalA) {
		t.Fatal("第一次 401 不应直接标需要登录")
	}
	if status := testStatus(t, pool, partitionNormalA); status.State == aistudio.AccountAuthRequired {
		t.Fatal("疑似登录失效不应改账户状态")
	}
	if !hasLogContaining(requests, "账号疑似登录失效 | 来源=生成 | 出口=直连 | 冷却=5m0s | 复核=排队 | 错误=") {
		t.Fatalf("应记录疑似登录失效，日志 = %v", serviceLogMessages(requests))
	}
	if !waitUntil(5*time.Second, func() bool { return prober.probed(partitionNormalA) > 0 && !recheck.suspected(partitionNormalA) }) {
		t.Fatalf("应复核并排除嫌疑，复核次数 = %d", prober.probed(partitionNormalA))
	}
	// 嫌疑先在复核器里排除，随后才清账户池里的冷却
	if !waitUntil(5*time.Second, func() bool { return suspectCooldown(t, pool, partitionNormalA) == "" }) {
		t.Fatalf("复核通过后疑似冷却应清除，仍有 %q", suspectCooldown(t, pool, partitionNormalA))
	}
	if status := testStatus(t, pool, partitionNormalA); status.State == aistudio.AccountAuthRequired {
		t.Fatal("复核通过不应标需要登录")
	}
	if !waitUntil(time.Second, func() bool {
		return hasLogContaining(requests, "疑似登录失效复核通过 | 来源=生成 | 疑似=")
	}) {
		t.Fatalf("应记录复核通过，日志 = %v", serviceLogMessages(requests))
	}
	if status := recheck.status(); status.Cleared1h != 1 || status.Suspects != 0 {
		t.Fatalf("复核状态 = %+v", status)
	}
}

// TestAuthSuspectRecheckConfirmsAndReleasesWorker 复核仍是 401 才标需要登录：写“账号需要登录 | 来源=生成 | … | 复核=仍失效”，
// 驻留 Worker 登记释放并关闭（与 WP-D 的不可调度释放配合）
func TestAuthSuspectRecheckConfirmsAndReleasesWorker(t *testing.T) {
	manager, pool, requests := partitionTestManager(t, []string{partitionNormalA}, nil, 1, 1, 0, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, recheck, prober := recheckTestService(t, ctx, manager, pool, requests)
	if _, err := manager.promote(ctx, partitionNormalA, testRuntimeModel); err != nil {
		t.Fatal(err)
	}
	prober.set(partitionNormalA, authFailureError())
	if suspectAccountByGeneration(t, pool, partitionNormalA) {
		t.Fatal("第一次 401 不应直接标需要登录")
	}
	if !waitUntil(5*time.Second, func() bool {
		return testStatus(t, pool, partitionNormalA).State == aistudio.AccountAuthRequired
	}) {
		t.Fatalf("复核仍是 401 应标需要登录，复核次数 = %d", prober.probed(partitionNormalA))
	}
	if !waitUntil(3*time.Second, func() bool { return !slices.Contains(manager.WarmAccountIDs(), partitionNormalA) }) {
		t.Fatalf("标记后驻留 Worker 应被释放，驻留 Worker = %v", manager.WarmAccountIDs())
	}
	if !waitUntil(3*time.Second, func() bool {
		return hasLogContaining(requests, "账号需要登录 | 来源=生成 | 出口=直连 | 出口限流中=否 | 错误=AI Studio GenerateContent 返回 HTTP 401", "| 复核=仍失效")
	}) {
		t.Fatalf("应记录复核确认的需要登录，日志 = %v", serviceLogMessages(requests))
	}
	if !waitUntil(time.Second, func() bool { return hasLogContaining(requests, "WAA Worker 释放 | 原因=需要登录") }) {
		t.Fatalf("应记录释放，日志 = %v", serviceLogMessages(requests))
	}
	if status := recheck.status(); status.Confirmed1h != 1 || status.Suspects != 0 {
		t.Fatalf("复核状态 = %+v", status)
	}
}

// TestAuthRecheckPausedDuringEgressLimited 出口限流判定成立期间不复核（疑似账号保持短冷却），判定解除后照常复核
func TestAuthRecheckPausedDuringEgressLimited(t *testing.T) {
	manager, pool, requests := partitionTestManager(t, []string{partitionNormalA}, nil, 1, 1, 0, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	service := unschedulableTestService(ctx, manager, pool, requests)
	prober := newProbeTestService(service.service)
	service.service = prober
	recheck := newAuthRecheck(service)
	recheck.interval = time.Millisecond
	recheck.suspectRe = 20 * time.Millisecond
	var mu sync.Mutex
	paused := true
	recheck.egressPaused = func(string) bool {
		mu.Lock()
		defer mu.Unlock()
		return paused
	}
	service.authRecheck = recheck
	pool.SetAuthFailureGate(recheck.gate)
	t.Cleanup(func() { pool.SetAuthFailureGate(nil) })
	startRecheckLoop(t, ctx, recheck)
	if suspectAccountByGeneration(t, pool, partitionNormalA) {
		t.Fatal("第一次 401 不应直接标需要登录")
	}
	time.Sleep(300 * time.Millisecond)
	if prober.probed(partitionNormalA) != 0 {
		t.Fatal("出口限流期间不应复核")
	}
	if suspectCooldown(t, pool, partitionNormalA) == "" || !recheck.suspected(partitionNormalA) {
		t.Fatal("出口限流期间应保持疑似状态与短冷却")
	}
	if status := recheck.status(); status.PauseReason != authRecheckPauseEgress {
		t.Fatalf("状态应写暂停原因，实际 %+v", status)
	}
	mu.Lock()
	paused = false
	mu.Unlock()
	if !waitUntil(5*time.Second, func() bool { return prober.probed(partitionNormalA) > 0 && !recheck.suspected(partitionNormalA) }) {
		t.Fatal("判定解除后应复核")
	}
}

// TestAuthRecheckPeriodicRecoveryAndBackoff 定期复核需要登录的账号：复核通过的恢复就绪并写“登录复核恢复”，仍是 401 的按账户退避
// （下一轮不再复核它），每轮写一条持久汇总“登录复核 | 恢复=… | 仍失效=… | 跳过=… | 下一轮=…”
func TestAuthRecheckPeriodicRecoveryAndBackoff(t *testing.T) {
	manager, pool, requests := partitionTestManager(t, []string{partitionNormalA, partitionNormalB}, nil, 1, 2, 0, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	service := unschedulableTestService(ctx, manager, pool, requests)
	prober := newProbeTestService(service.service)
	service.service = prober
	recheck := newAuthRecheck(service)
	recheck.interval = time.Millisecond
	recheck.round0 = 100 * time.Millisecond
	service.authRecheck = recheck
	for _, accountID := range []string{partitionNormalA, partitionNormalB} {
		if err := pool.MarkAuthRequired(accountID, "HTTP 401"); err != nil {
			t.Fatal(err)
		}
	}
	prober.set(partitionNormalB, authFailureError())
	startRecheckLoop(t, ctx, recheck)
	if !waitUntil(5*time.Second, func() bool { return testStatus(t, pool, partitionNormalA).State == aistudio.AccountReady }) {
		t.Fatal("复核通过的需要登录账号应恢复就绪")
	}
	// 账号在复核的租约内恢复就绪，随后才写日志
	if !waitUntil(5*time.Second, func() bool { return hasLogContaining(requests, "登录复核恢复 | 复核次数=1 | 耗时=") }) {
		t.Fatalf("应记录登录复核恢复，日志 = %v", serviceLogMessages(requests))
	}
	if !waitUntil(5*time.Second, func() bool {
		return hasLogContaining(requests, "登录复核 | 恢复=1 | 仍失效=1 | 跳过=0 | 下一轮=")
	}) {
		t.Fatalf("应写一轮的汇总，日志 = %v", serviceLogMessages(requests))
	}
	if status := testStatus(t, pool, partitionNormalB); status.State != aistudio.AccountAuthRequired {
		t.Fatalf("复核仍是 401 的账号应保持需要登录，实际 %s", status.State)
	}
	// 再过几轮：B 在 15 分钟退避内不再复核
	time.Sleep(500 * time.Millisecond)
	if probed := prober.probed(partitionNormalB); probed != 1 {
		t.Fatalf("仍失效的账号应按退避推后，复核了 %d 次", probed)
	}
	recheck.mu.Lock()
	record := recheck.records[partitionNormalB]
	recheck.mu.Unlock()
	if record == nil || record.failures != 1 || time.Until(record.nextAt) < authRecheckBackoffBase-time.Minute {
		t.Fatalf("退避记录 = %+v，期望失败 1 次、约 15 分钟后再复核", record)
	}
	if got := authRecheckBackoff(10); got != authRecheckBackoffMax {
		t.Fatalf("退避上限 = %s，期望 %s", got, authRecheckBackoffMax)
	}
	if got := authRecheckBackoff(2); got != 2*authRecheckBackoffBase {
		t.Fatalf("第 2 次退避 = %s", got)
	}
}

// TestAuthRecheckGatePassThroughWhenStopped 复核循环没有在跑（服务停止）时 gate 放行，401 照常直接标记
func TestAuthRecheckGatePassThroughWhenStopped(t *testing.T) {
	manager, pool, requests := partitionTestManager(t, []string{partitionNormalA}, nil, 1, 1, 0, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	service := unschedulableTestService(ctx, manager, pool, requests)
	service.service = newProbeTestService(service.service)
	recheck := newAuthRecheck(service)
	pool.SetAuthFailureGate(recheck.gate)
	if !suspectAccountByGeneration(t, pool, partitionNormalA) {
		t.Fatal("复核循环没有在跑时应直接标记")
	}
	if newAuthRecheck(&trackedService{pool: pool, service: newSwitchUpstream()}) != nil {
		t.Fatal("服务不支持复核时不应创建复核器")
	}
}

// TestAuthRenewSkippedOffWindows 非 Windows 平台视为不能续签：有续签材料也不续签，401 直接走标记（经 gate 时记疑似）
func TestAuthRenewSkippedOffWindows(t *testing.T) {
	if got, want := authRenewPlatformSupported(), runtime.GOOS == "windows"; got != want {
		t.Fatalf("authRenewPlatformSupported = %t，期望 %t", got, want)
	}
	pool, _ := authTestPool(t, true)
	counts := &authTestCounts{}
	refresher := newAuthTestRefresher(t, counts)
	refresher.renewSupported = func() bool { return false }
	_, ctx := authTestLease(t, pool)
	if refresher.Available(ctx) {
		t.Fatal("不支持续签的平台上 Available 应为 false")
	}
	if err := refresher.Recover(ctx, authFailureError()); err == nil {
		t.Fatal("不能续签时恢复应失败")
	}
	if counts.refreshes.Load() != 0 {
		t.Fatal("不支持续签的平台上不应续签")
	}
	if status := authTestStatus(t, pool); status.State != aistudio.AccountAuthRequired {
		t.Fatalf("没有 gate 时应直接标需要登录，实际 %s", status.State)
	}
}

// TestGenerationAuthFailureSwitchLimit 一批账号同时 401：单个请求因登录失效最多换号 authFailureSwitchLimit 次，之后按原错误结束
func TestGenerationAuthFailureSwitchLimit(t *testing.T) {
	accounts := switchTestIDs(6)
	upstream := newSwitchUpstream().failAll(authFailureError, accounts...)
	service, _ := primaryBusyService(t, upstream, accounts...)
	_, err, _ := runModelRequest(t, service, "req-auth-limit", "", testRuntimeModel)
	if !aistudio.DefinitiveAuthenticationFailure(err) {
		t.Fatalf("应以登录失效结束，实际 %v", err)
	}
	if calls := upstream.called(); len(calls) != authFailureSwitchLimit+1 {
		t.Fatalf("上游调用 = %v，期望 %d 次", calls, authFailureSwitchLimit+1)
	}
	if !progressLogged(service, "登录失效换号已达上限，不再换号 | 登录失效=3/3") {
		t.Fatal("请求进度应写明登录失效换号已达上限")
	}
}

// TestStartupCookieWriteback Worker 启动完成后立即把浏览器 Cookie 合并写回认证文件（N1）：Go 通道随后用的是浏览器轮换过的 Cookie
func TestStartupCookieWriteback(t *testing.T) {
	manager, pool, _ := partitionTestManager(t, []string{partitionNormalA}, nil, 1, 1, 0, 1)
	fresh := []aistudio.StateCookie{
		{Name: "SAPISID", Value: "FRESH", Domain: ".google.com", Path: "/", Expires: -1, Secure: true, SameSite: "None"},
		{Name: "__Secure-1PSIDTS", Value: "ROTATED", Domain: ".google.com", Path: "/", Expires: -1, Secure: true, SameSite: "None"},
	}
	encoded, err := json.Marshal(fresh)
	if err != nil {
		t.Fatal(err)
	}
	manager.launch = func(_ context.Context, accountID string, _ camoufoxnative.Options) (*aistudio.NativeWorker, error) {
		worker, _ := aistudio.NewStubWorkerWithHooks(accountID, aistudio.StubWorkerHooks{
			StorageCookies: func(context.Context) ([]byte, error) { return encoded, nil },
		})
		return worker, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := manager.promote(ctx, partitionNormalA, testRuntimeModel); err != nil {
		t.Fatal(err)
	}
	account, err := pool.Account(partitionNormalA)
	if err != nil {
		t.Fatal(err)
	}
	state, err := aistudio.LoadStorageState(account.StoragePath)
	if err != nil {
		t.Fatal(err)
	}
	values := make(map[string]string, len(state.Cookies))
	for _, cookie := range state.Cookies {
		values[cookie.Name] = cookie.Value
	}
	if values["SAPISID"] != "FRESH" || values["__Secure-1PSIDTS"] != "ROTATED" || values["__Secure-3PAPISID"] != "test" {
		t.Fatalf("启动后认证文件的 Cookie = %v，期望浏览器的新值合并进来、没导出的旧 Cookie 保留", values)
	}
}

// TestAuthRequiredRecentCount 状态里的“最近 10 分钟新增需要登录”按“账号需要登录”日志计数
func TestAuthRequiredRecentCount(t *testing.T) {
	var events recentEvents
	now := time.Now()
	events.add(now.Add(-11 * time.Minute))
	events.add(now.Add(-2 * time.Minute))
	events.add(now)
	if got := events.count(now, authRecentWindow); got != 2 {
		t.Fatalf("最近 10 分钟 = %d，期望 2", got)
	}
	pool, _ := testAccountPool(t, "recent@example.com")
	service := &trackedService{pool: pool, requests: newRequestRegistry(context.Background())}
	if err := pool.MarkAuthRequired("recent@example.com", "HTTP 401"); err != nil {
		t.Fatal(err)
	}
	service.noteAuthRequired("recent@example.com", "", authFromGeneration, errors.New("401"))
	if got := service.authMarks.count(time.Now(), authRecentWindow); got != 1 {
		t.Fatalf("写一条需要登录后计数 = %d，期望 1", got)
	}
	if authSourceOf(&aistudio.RPCError{Method: "ListModels", StatusCode: http.StatusUnauthorized}) != authFromCatalog ||
		authSourceOf(&aistudio.RPCError{Method: "CountTokens", StatusCode: http.StatusUnauthorized}) != authFromOther {
		t.Fatal("疑似来源按方法推断")
	}
}

// TestAuthRecheckSkipsBrowserEvidence 定期复核只复核按接口证据（上游 401）标记的账号：Worker 启动跳到登录页、管理验证判定未登录
// 是浏览器证据，接口复核通过也不改回就绪（不复核），汇总写“浏览器判定失效不复核=”；之后按接口证据再被标记时照常复核
func TestAuthRecheckSkipsBrowserEvidence(t *testing.T) {
	manager, pool, requests := partitionTestManager(t, []string{partitionNormalA, partitionNormalB}, nil, 1, 2, 0, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	service := unschedulableTestService(ctx, manager, pool, requests)
	prober := newProbeTestService(service.service)
	service.service = prober
	recheck := newAuthRecheck(service)
	recheck.interval = time.Millisecond
	recheck.round0 = 100 * time.Millisecond
	service.authRecheck = recheck
	for _, accountID := range []string{partitionNormalA, partitionNormalB} {
		if err := pool.MarkAuthRequired(accountID, "HTTP 401"); err != nil {
			t.Fatal(err)
		}
	}
	// A：Worker 启动跳到登录页（浏览器证据）；B：生成 401（接口证据）
	service.noteAuthRequired(partitionNormalA, "", authFromStartup, errors.New("AI Studio 登录页跳转"))
	service.noteAuthRequired(partitionNormalB, "", authFromGeneration, authFailureError())
	startRecheckLoop(t, ctx, recheck)
	if !waitUntil(5*time.Second, func() bool { return testStatus(t, pool, partitionNormalB).State == aistudio.AccountReady }) {
		t.Fatal("接口证据的账号复核通过应恢复就绪")
	}
	if !waitUntil(5*time.Second, func() bool {
		return hasLogContaining(requests, "登录复核 | 恢复=1 | 仍失效=0 | 跳过=0 | 下一轮=", "| 浏览器判定失效不复核=1")
	}) {
		t.Fatalf("汇总应写浏览器判定失效不复核，日志 = %v", serviceLogMessages(requests))
	}
	time.Sleep(300 * time.Millisecond)
	if prober.probed(partitionNormalA) != 0 || testStatus(t, pool, partitionNormalA).State != aistudio.AccountAuthRequired {
		t.Fatalf("浏览器证据的账号不应复核（复核 %d 次）也不应恢复", prober.probed(partitionNormalA))
	}
	// 之后按接口证据再被标记：恢复复核
	service.noteAuthRequired(partitionNormalA, "", authFromGeneration, authFailureError())
	if !waitUntil(5*time.Second, func() bool { return testStatus(t, pool, partitionNormalA).State == aistudio.AccountReady }) {
		t.Fatal("按接口证据再被标记后应照常复核恢复")
	}
}

// TestAuthRecheckSummaryPerRound 疑似账户的复核结果累计到轮次边界才写汇总，不按每个疑似账户写一条持久行
func TestAuthRecheckSummaryPerRound(t *testing.T) {
	ids := []string{partitionNormalA, partitionNormalB}
	manager, pool, requests := partitionTestManager(t, ids, nil, 1, 2, 0, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, recheck, prober := recheckTestService(t, ctx, manager, pool, requests)
	for _, accountID := range ids {
		if suspectAccountByGeneration(t, pool, accountID) {
			t.Fatal("第一次 401 不应直接标需要登录")
		}
		if !waitUntil(5*time.Second, func() bool { return prober.probed(accountID) > 0 && !recheck.suspected(accountID) }) {
			t.Fatalf("应复核并排除 %s 的嫌疑", accountID)
		}
	}
	time.Sleep(200 * time.Millisecond)
	if hasLogContaining(requests, "登录复核 | ") {
		t.Fatalf("轮次边界之前不应写汇总，日志 = %v", serviceLogMessages(requests))
	}
	// 到轮次边界：写一条汇总，带上期间疑似账户的结果
	recheck.mu.Lock()
	recheck.nextRound = time.Now().Add(-time.Second)
	recheck.mu.Unlock()
	recheck.kick()
	if !waitUntil(5*time.Second, func() bool {
		return hasLogContaining(requests, "登录复核 | 恢复=0 | 仍失效=0 | 跳过=0 | 下一轮=", "| 疑似排除=2 | 疑似确认=0 | 待复核疑似=0")
	}) {
		t.Fatalf("轮次边界应写一条汇总，日志 = %v", serviceLogMessages(requests))
	}
	count := 0
	for _, message := range serviceLogMessages(requests) {
		if strings.HasPrefix(message, "登录复核 | ") {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("汇总写了 %d 条，期望 1 条", count)
	}
}

// TestNoteLoginRestoredClears 人工恢复登录（浏览器登录、认证文件更新、管理验证通过）统一清掉登录复核的疑似记录与退避、
// 疑似登录失效冷却；正在复核的疑似账户被清掉后，复核结论不再生效
func TestNoteLoginRestoredClears(t *testing.T) {
	manager, pool, requests := partitionTestManager(t, []string{partitionNormalA}, nil, 1, 1, 0, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	service := unschedulableTestService(ctx, manager, pool, requests)
	prober := newProbeTestService(service.service)
	service.service = prober
	recheck := newAuthRecheck(service)
	recheck.interval = time.Millisecond
	service.authRecheck = recheck
	pool.SetAuthFailureGate(recheck.gate)
	t.Cleanup(func() { pool.SetAuthFailureGate(nil) })
	// 复核循环在跑（gate 只记疑似），但出口限流暂停复核：疑似状态与短冷却一直保持
	recheck.egressPaused = func(string) bool { return true }
	startRecheckLoop(t, ctx, recheck)
	if suspectAccountByGeneration(t, pool, partitionNormalA) {
		t.Fatal("第一次 401 不应直接标需要登录")
	}
	if suspectCooldown(t, pool, partitionNormalA) == "" || !recheck.suspected(partitionNormalA) {
		t.Fatal("应记疑似登录失效并写短冷却")
	}
	recheck.mu.Lock()
	recheck.recordLocked(partitionNormalA).failures = 3
	recheck.mu.Unlock()
	service.noteLoginRestored(partitionNormalA)
	if recheck.suspected(partitionNormalA) || suspectCooldown(t, pool, partitionNormalA) != "" {
		t.Fatal("人工恢复登录后应清掉疑似记录与疑似登录失效冷却")
	}
	recheck.mu.Lock()
	_, kept := recheck.records[partitionNormalA]
	recheck.mu.Unlock()
	if kept {
		t.Fatal("人工恢复登录后应清掉复核退避")
	}
	// 正在复核的疑似账户被清掉：复核结论（仍是 401）不再标记
	suspect := &authSuspect{accountID: partitionNormalA, cause: authFailureError(), generation: pool.AuthGeneration(partitionNormalA)}
	recheck.mu.Lock()
	recheck.running++
	recheck.mu.Unlock()
	recheck.finishSuspect(suspect, suspect.generation, time.Now(), authFailureError())
	if status := testStatus(t, pool, partitionNormalA); status.State == aistudio.AccountAuthRequired {
		t.Fatal("已清掉的疑似账户的复核结论不应生效")
	}
	recheck.mu.Lock()
	running := recheck.running
	recheck.mu.Unlock()
	if running != 0 {
		t.Fatalf("进行中计数 = %d，期望归零", running)
	}
}
