package app

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/camoufoxnative"
	"github.com/Mag1cFall/AIStudio2API/internal/proxyrelay"
)

// startupLoginPageURL 为 Camoufox Worker 启动停在 Google 确认身份页的地址（照线上形态，参数为示例值）
const startupLoginPageURL = "https://accounts.google.com/v3/signin/confirmidentifier?continue=https://aistudio.google.com/prompts/new_chat&flowName=GlifWebSignIn&flowEntry=ServiceLogin"

// camoufoxLoginPageError 返回与 camoufoxnative 启动停在登录页时相同形态的错误（哨兵 + " url=<地址>"，文案“隔离登录态失效 url=…”）
func camoufoxLoginPageError() error {
	return fmt.Errorf("%w url=%s", camoufoxnative.ErrLoginPageRedirect, startupLoginPageURL)
}

// camoufoxLoginPageRelayError 为同一错误经本机代理中转包装、且这次启动期间中转记到了连上游代理的失败（命中 proxyrelay.ErrUpstream）
func camoufoxLoginPageRelayError() error {
	return fmt.Errorf("%w；本机代理中转：上游代理拒绝了隧道：503 Service Unavailable（目标 accounts.google.com）%w",
		camoufoxLoginPageError(), proxyrelay.ErrUpstream)
}

// TestStartupLoginPageMarksAuthRequired 预热、后台扩容等没有租约的启动停在 Google 登录 / 确认身份页（Camoufox 的“隔离登录态失效”）：
// 不能续签的平台（Linux）上不论有没有续签材料都直接标需要登录，原因保留原文，写持久行“账号需要登录 | 来源=启动”，不续签、不取租约；
// 能续签且有材料（Windows）时照旧留给带租约的按需启动恢复。中转记到上游失败的同一错误按普通启动失败处理、不标
func TestStartupLoginPageMarksAuthRequired(t *testing.T) {
	cases := []struct {
		name       string
		oauth      bool
		renew      bool
		err        error
		wantMarked bool
	}{
		{name: "不能续签·没有材料", err: camoufoxLoginPageError(), wantMarked: true},
		{name: "不能续签·有材料", oauth: true, err: camoufoxLoginPageError(), wantMarked: true},
		{name: "能续签·没有材料", renew: true, err: camoufoxLoginPageError(), wantMarked: true},
		{name: "能续签·有材料", oauth: true, renew: true, err: camoufoxLoginPageError()},
		{name: "中转记到上游失败", err: camoufoxLoginPageRelayError()},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			pool, account := authTestPool(t, item.oauth)
			manager, launches := authTestManager(t, pool, account, func(int32) (*aistudio.NativeWorker, error) {
				return nil, item.err
			})
			counts := &authTestCounts{}
			manager.refresher = newAuthTestRefresher(t, counts)
			renew := item.renew
			manager.refresher.renewSupported = func() bool { return renew }
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			service := unschedulableTestService(ctx, manager, pool, manager.requests)
			_, err := manager.ensureWorker(withWorkerStartSource(ctx, workerStartPrewarm), authTestAccount, "", false)
			if !errors.Is(err, aistudio.ErrLoginPageRedirect) {
				t.Fatalf("应返回登录页错误，实际 %v", err)
			}
			status := authTestStatus(t, pool)
			if status.LastUsed != nil || counts.refreshes.Load() != 0 || launches.Load() != 1 {
				t.Fatalf("没有租约的启动不应取租约、续签或重复启动: LastUsed=%v 续签=%d 启动=%d",
					status.LastUsed, counts.refreshes.Load(), launches.Load())
			}
			logged := hasLogContaining(service.requests, "账号需要登录 | 来源=启动 | 出口=直连 | 出口限流中=否 | 错误=隔离登录态失效 url=https://accounts.google.com/v3/signin/confirmidentifier")
			if !item.wantMarked {
				if status.State != aistudio.AccountReady || logged {
					t.Fatalf("不应标需要登录: state=%s 日志=%v", status.State, serviceLogMessages(service.requests))
				}
				return
			}
			if status.State != aistudio.AccountAuthRequired || status.Message != "隔离登录态失效 url="+startupLoginPageURL {
				t.Fatalf("应标需要登录并保留原文: %+v", status)
			}
			if !logged {
				t.Fatalf("应写“账号需要登录 | 来源=启动”，日志 = %v", serviceLogMessages(service.requests))
			}
			if reasons := pool.UnschedulableSet([]string{authTestAccount}); reasons[authTestAccount] != unschedulableAuthRequired {
				t.Fatalf("账户应不可调度（需要登录）: %v", reasons)
			}
		})
	}
}

// TestStartupLoginPageLeasedMarks 按需启动（带请求租约）停在登录页：不能续签时不续签、只启动一次，账户标需要登录，
// 请求按账户需要重新登录返回（503 列出原因）并保留登录页原因，日志来源为启动
func TestStartupLoginPageLeasedMarks(t *testing.T) {
	pool, account := authTestPool(t, true)
	manager, launches := authTestManager(t, pool, account, func(int32) (*aistudio.NativeWorker, error) {
		return nil, camoufoxLoginPageError()
	})
	counts := &authTestCounts{}
	manager.refresher = newAuthTestRefresher(t, counts)
	manager.refresher.renewSupported = func() bool { return false }
	manager.refresher.workers = manager
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	service := unschedulableTestService(ctx, manager, pool, manager.requests)
	_, err := service.acquireWarmLease(ctx, aistudio.AccountSelection{ModelID: "gemini-test", Method: "generateContent"})
	var notReady *aistudio.AccountsNotReadyError
	if !errors.As(err, &notReady) || !errors.Is(err, aistudio.ErrLoginPageRedirect) {
		t.Fatalf("应返回账户需要重新登录并保留登录页原因，实际 %v", err)
	}
	if launches.Load() != 1 || counts.refreshes.Load() != 0 {
		t.Fatalf("不能续签时不应续签或重复启动: 启动=%d 续签=%d", launches.Load(), counts.refreshes.Load())
	}
	if status := authTestStatus(t, pool); status.State != aistudio.AccountAuthRequired {
		t.Fatalf("应标为需要登录，实际 %s", status.State)
	}
	if !hasLogContaining(service.requests, "账号需要登录 | 来源=启动 |", "隔离登录态失效 url=") {
		t.Fatalf("应写“账号需要登录 | 来源=启动”，日志 = %v", serviceLogMessages(service.requests))
	}
}

// TestStartupLoginPageNotRestarted 预热时 A 的 Worker 启动停在登录页、B 正常：A 标需要登录后，再次预热、替补选号、备用候选、
// 可接替统计、按需选号（B 忙碌时）都不再挑 A，A 的浏览器只启动过一次；B 照常驻留
func TestStartupLoginPageNotRestarted(t *testing.T) {
	manager, pool, requests := partitionTestManager(t, []string{partitionNormalA, partitionNormalB}, nil, 2, 2, 0, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	service := unschedulableTestService(ctx, manager, pool, requests)
	launches := &countingLaunch{failing: map[string]error{partitionNormalA: camoufoxLoginPageError()}, launches: map[string]int{}}
	manager.launch = launches.launch
	manager.StartPrewarm(ctx)
	if !waitUntil(5*time.Second, func() bool {
		return testStatus(t, pool, partitionNormalA).State == aistudio.AccountAuthRequired &&
			slices.Contains(manager.WarmAccountIDs(), partitionNormalB)
	}) {
		t.Fatalf("预热后 A 应为需要登录、B 驻留: A=%s 驻留=%v 日志=%v",
			testStatus(t, pool, partitionNormalA).State, manager.WarmAccountIDs(), serviceLogMessages(requests))
	}
	if !hasLogContaining(requests, "账号需要登录 | 来源=启动") {
		t.Fatalf("应写“账号需要登录 | 来源=启动”，日志 = %v", serviceLogMessages(requests))
	}
	// 再预热几轮（A 仍在目标之内）：不再启动 A
	for range 3 {
		<-manager.StartPrewarm(ctx)
		time.Sleep(50 * time.Millisecond)
	}
	groups, err := manager.classifyBootstrapCandidates(ctx, nil, aistudio.PoolScopeNormal)
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(groups.StandbyReady, partitionNormalA) || slices.Contains(groups.StandbyBusy, partitionNormalA) {
		t.Fatalf("备用候选不应包含需要登录的账户: %+v", groups)
	}
	if standby := manager.replacementCandidates(ctx, aistudio.PoolScopeNormal, []string{testRuntimeModel}, nil); slices.Contains(standby, partitionNormalA) {
		t.Fatalf("替补候选不应包含需要登录的账户: %v", standby)
	}
	if picked := manager.pickReplacement(ctx, aistudio.PoolScopeNormal, []string{testRuntimeModel}, manager.WarmAccountIDs()); picked == partitionNormalA {
		t.Fatal("替补不应挑需要登录的账户")
	}
	if spare := pool.SpareAccounts(aistudio.PoolScopeNormal, nil, []string{testRuntimeModel}, 0, 0); spare != 1 {
		t.Fatalf("可接替统计只应算 B，实际 %d", spare)
	}
	// 按需选号：B 忙碌（独占租约）时请求也不会冷启动 A，等到期限结束
	release := holdExclusive(t, pool, partitionNormalB)
	shortCtx, shortCancel := context.WithTimeout(ctx, 300*time.Millisecond)
	lease, err := service.acquireWarmLease(shortCtx, aistudio.AccountSelection{ModelID: testRuntimeModel, Method: "generateContent"})
	shortCancel()
	if err == nil {
		_ = lease.Release()
		if lease.Account().ID == partitionNormalA {
			t.Fatal("按需选号不应选中需要登录的账户")
		}
	}
	release()
	time.Sleep(100 * time.Millisecond)
	if count := launches.count(partitionNormalA); count != 1 {
		t.Fatalf("A 只应启动过一次浏览器，实际 %d 次，日志 = %v", count, serviceLogMessages(requests))
	}
	if testStatus(t, pool, partitionNormalA).State != aistudio.AccountAuthRequired {
		t.Fatal("A 应保持需要登录")
	}
}

// TestStartupLoginPageSkippedByAuthRecheck 经真实启动路径（预热停在登录页）标记的需要登录是浏览器证据：定期登录复核不复核它，
// 接口复核会通过也不改回就绪（不会被预热再选中、冷启动、再标回）
func TestStartupLoginPageSkippedByAuthRecheck(t *testing.T) {
	manager, pool, requests := partitionTestManager(t, []string{partitionNormalA}, nil, 1, 1, 0, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	service := unschedulableTestService(ctx, manager, pool, requests)
	prober := newProbeTestService(service.service)
	service.service = prober
	recheck := newAuthRecheck(service)
	recheck.interval = time.Millisecond
	recheck.round0 = 100 * time.Millisecond
	service.authRecheck = recheck
	launches := &countingLaunch{failing: map[string]error{partitionNormalA: camoufoxLoginPageError()}, launches: map[string]int{}}
	manager.launch = launches.launch
	if _, err := manager.ensureWorker(withWorkerStartSource(ctx, workerStartPrewarm), partitionNormalA, "", false); !errors.Is(err, aistudio.ErrLoginPageRedirect) {
		t.Fatalf("应返回登录页错误，实际 %v", err)
	}
	if testStatus(t, pool, partitionNormalA).State != aistudio.AccountAuthRequired {
		t.Fatal("启动停在登录页应标需要登录")
	}
	startRecheckLoop(t, ctx, recheck)
	// 只有浏览器证据的账户时轮次不写汇总（没有结果），改看本轮因浏览器证据不复核的计数
	if !waitUntil(5*time.Second, func() bool {
		recheck.mu.Lock()
		defer recheck.mu.Unlock()
		return recheck.round.browser >= 1
	}) {
		t.Fatal("定期复核应已开始一轮并把它记为浏览器判定失效不复核")
	}
	time.Sleep(300 * time.Millisecond)
	if prober.probed(partitionNormalA) != 0 || testStatus(t, pool, partitionNormalA).State != aistudio.AccountAuthRequired {
		t.Fatalf("启动停在登录页的账号不应复核（复核 %d 次）也不应恢复", prober.probed(partitionNormalA))
	}
}

// TestStartupLoginPageMarkSurvivesLaterLease 启动期间取得的另一个租约（如 Go 通道的 CountTokens）在标记之后才确认认证有效：
// 标记按启动失败的时刻写，这个更早取得的租约改不回就绪（原先按启动开始时刻写，会被它改回就绪、又被预热选中冷启动一次）
func TestStartupLoginPageMarkSurvivesLaterLease(t *testing.T) {
	manager, pool, requests := partitionTestManager(t, []string{partitionNormalA}, nil, 1, 1, 0, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	unschedulableTestService(ctx, manager, pool, requests)
	var during *aistudio.AccountLease
	manager.launch = func(_ context.Context, accountID string, options camoufoxnative.Options) (*aistudio.NativeWorker, error) {
		if options.CacheAttached != nil {
			options.CacheAttached()
		}
		time.Sleep(10 * time.Millisecond)
		lease, ok, err := pool.TryAcquireFor(context.Background(), aistudio.AccountSelection{AccountID: accountID, ModelID: testRuntimeModel})
		if err != nil || !ok {
			t.Errorf("启动期间应能取得租约: ok=%t err=%v", ok, err)
		} else {
			during = lease
		}
		time.Sleep(10 * time.Millisecond)
		return nil, camoufoxLoginPageError()
	}
	if _, err := manager.ensureWorker(withWorkerStartSource(ctx, workerStartPrewarm), partitionNormalA, "", false); !errors.Is(err, aistudio.ErrLoginPageRedirect) {
		t.Fatalf("应返回登录页错误，实际 %v", err)
	}
	if during == nil {
		t.Fatal("没有取得启动期间的租约")
	}
	if testStatus(t, pool, partitionNormalA).State != aistudio.AccountAuthRequired {
		t.Fatal("启动停在登录页应标需要登录")
	}
	if err := during.MarkAuthenticationValid(); err != nil {
		t.Fatal(err)
	}
	_ = during.Release()
	if state := testStatus(t, pool, partitionNormalA).State; state != aistudio.AccountAuthRequired {
		t.Fatalf("启动期间取得的租约在标记之后确认有效，不应改回就绪，实际 %s", state)
	}
}

// TestStartupLoginPageRestored 启动停在登录页标了需要登录的账户，认证文件更新、管理页验证通过、浏览器重新登录都恢复就绪，
// 恢复后重新成为候选，下一次启动照常启动浏览器
func TestStartupLoginPageRestored(t *testing.T) {
	setup := func(t *testing.T, accountID string) (*runtimeAdmin, *countingLaunch) {
		t.Helper()
		admin, _, accounts := keepCatalogAdmin(t, accountID)
		note := authRequiredHook(admin.service.noteAuthRequired)
		admin.workers.authRequired.Store(&note)
		launches := &countingLaunch{failing: map[string]error{accountID: camoufoxLoginPageError()}, launches: map[string]int{}}
		admin.workers.launch = launches.launch
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := admin.workers.ensureWorker(withWorkerStartSource(ctx, workerStartPrewarm), accounts[0].ID, "", false); !errors.Is(err, aistudio.ErrLoginPageRedirect) {
			t.Fatalf("应返回登录页错误，实际 %v", err)
		}
		if testStatus(t, admin.pool, accountID).State != aistudio.AccountAuthRequired {
			t.Fatal("启动停在登录页应标需要登录")
		}
		if !hasLogContaining(admin.requests, "账号需要登录 | 来源=启动") {
			t.Fatalf("日志 = %v", serviceLogMessages(admin.requests))
		}
		return admin, launches
	}
	restartable := func(t *testing.T, admin *runtimeAdmin, launches *countingLaunch, accountID string) {
		t.Helper()
		assertCatalogCandidate(t, admin, accountID)
		launches.mu.Lock()
		delete(launches.failing, accountID)
		launches.mu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := admin.workers.promote(ctx, accountID, testRuntimeModel); err != nil {
			t.Fatalf("恢复后应能再启动: %v", err)
		}
		if count := launches.count(accountID); count != 2 {
			t.Fatalf("恢复后应再启动一次浏览器，实际共 %d 次", count)
		}
	}
	t.Run("认证文件更新", func(t *testing.T) {
		const accountID = "login-page-reload@example.com"
		admin, launches := setup(t, accountID)
		account, err := admin.pool.Account(accountID)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if reloaded, err := admin.reloadAccountCredentials(ctx, accountID, account.StoragePath); err != nil || !reloaded {
			t.Fatalf("应载入新认证文件: reloaded=%t err=%v", reloaded, err)
		}
		restartable(t, admin, launches, accountID)
	})
	t.Run("管理页验证通过", func(t *testing.T) {
		const accountID = "login-page-verify@example.com"
		admin, launches := setup(t, accountID)
		admin.login = &verifyTestLogin{verification: aistudio.LoginVerification{Authenticated: true}}
		admin.service.service = newProbeTestService(nil)
		admin.service.authRecheck = newAuthRecheck(admin.service)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		account, err := admin.VerifyAccount(ctx, accountID)
		if err != nil {
			t.Fatal(err)
		}
		if account.Authenticated == nil || !*account.Authenticated {
			t.Fatalf("验证应通过: %+v", account)
		}
		restartable(t, admin, launches, accountID)
	})
	t.Run("浏览器重新登录", func(t *testing.T) {
		const accountID = "login-page-login@example.com"
		admin, launches := setup(t, accountID)
		admin.login = &loginResultLogin{email: accountID, state: authTestState(t, "NEW", false)}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := admin.LoginAccount(ctx, accountID); err != nil {
			t.Fatal(err)
		}
		restartable(t, admin, launches, accountID)
	})
	t.Run("管理页验证仍落登录页", func(t *testing.T) {
		const accountID = "login-page-verify-fail@example.com"
		admin, _ := setup(t, accountID)
		admin.login = &verifyTestLogin{verification: aistudio.LoginVerification{
			Reason: "跳转到登录页", PageURL: "accounts.google.com/v3/signin/confirmidentifier",
		}}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := admin.VerifyAccount(ctx, accountID); err != nil {
			t.Fatal(err)
		}
		if status := testStatus(t, admin.pool, accountID); status.State != aistudio.AccountAuthRequired ||
			!strings.Contains(status.Message, "跳转到登录页") {
			t.Fatalf("验证仍落登录页时应保持需要登录: %+v", status)
		}
	})
}

// loginRestartUpstream 为测试上游：restartAccount 的调用先关掉它的 Worker，再像受保护传输那样经 Worker 管理器取 Worker（带请求租约，
// 现场冷启动），取不到时返回那个错误；其他调用交给 workerFailureService。模拟发送时 Worker 已不在、在生成尝试内重启 Worker
type loginRestartUpstream struct {
	*workerFailureService
	workers        *accountWorkerManager
	restartAccount string
}

func (upstream *loginRestartUpstream) Generate(ctx context.Context, request aistudio.GenerateRequest) (<-chan aistudio.Event, error) {
	if request.AccountID == upstream.restartAccount {
		if err := upstream.workers.Reset(request.AccountID); err != nil {
			return nil, err
		}
		if _, err := upstream.workers.Worker(ctx, request.AccountID, request.Model); err != nil {
			return nil, err
		}
	}
	return upstream.workerFailureService.Generate(ctx, request)
}

// TestStartupLoginPageInGenerationKeepsBrowserEvidence 生成尝试内重启 Worker 停在登录页：启动阶段（带租约、不能续签）已按“来源=启动”
// 标需要登录，请求换号成功；生成循环不再按生成标记、不写“来源=生成”，登录复核记下的仍是浏览器证据，定期复核不复核、不把账号改回就绪
// （原先生成循环再标一次并按接口证据记下来源，复核用磁盘 Cookie 探测通过就恢复就绪，账号再被冷启动、再落到登录页）
func TestStartupLoginPageInGenerationKeepsBrowserEvidence(t *testing.T) {
	upstream := &loginRestartUpstream{workerFailureService: newWorkerFailureService(map[string]int{}), restartAccount: resetTestAlice}
	service, _ := workerResetTestService(t, upstream, resetTestAlice, resetTestBob)
	manager := service.workers
	upstream.workers = manager
	launches := &countingLaunch{failing: map[string]error{resetTestAlice: camoufoxLoginPageError()}, launches: map[string]int{}}
	manager.launch = launches.launch
	counts := &authTestCounts{}
	manager.refresher = newAuthTestRefresher(t, counts)
	manager.refresher.renewSupported = func() bool { return false }
	manager.refresher.workers = manager
	note := authRequiredHook(service.noteAuthRequired)
	manager.authRequired.Store(&note)
	prober := newProbeTestService(service.service)
	service.service = prober
	recheck := newAuthRecheck(service)
	recheck.interval = time.Millisecond
	recheck.round0 = 100 * time.Millisecond
	service.authRecheck = recheck

	result := awaitResetTestResult(t, startResetTestRequest(service.lifecycle, service, "req-login-restart", ""), 15*time.Second)
	if result.err != nil || result.text != "hello" {
		t.Fatalf("应换到 bob 成功: text=%q err=%v", result.text, result.err)
	}
	if calls := upstream.called(); !slices.Equal(calls, []string{resetTestBob}) {
		t.Fatalf("上游调用 = %v，alice 应在取 Worker 时失败、换到 bob", calls)
	}
	if launches.count(resetTestAlice) != 1 || counts.refreshes.Load() != 0 {
		t.Fatalf("alice 应只冷启动一次、不续签: 启动=%d 续签=%d", launches.count(resetTestAlice), counts.refreshes.Load())
	}
	if testStatus(t, service.pool, resetTestAlice).State != aistudio.AccountAuthRequired {
		t.Fatal("alice 应标需要登录")
	}
	if !hasLogContaining(service.requests, "账号需要登录 | 来源=启动 |", "隔离登录态失效 url=") ||
		hasLogContaining(service.requests, "账号需要登录 | 来源=生成") {
		t.Fatalf("应只写“来源=启动”，日志 = %v", serviceLogMessages(service.requests))
	}
	recheck.mu.Lock()
	record := recheck.records[resetTestAlice]
	browser := record != nil && record.browserEvidence
	recheck.mu.Unlock()
	if !browser {
		t.Fatal("登录复核应记为浏览器证据")
	}
	ctx, cancel := context.WithTimeout(service.lifecycle, 10*time.Second)
	defer cancel()
	startRecheckLoop(t, ctx, recheck)
	if !waitUntil(5*time.Second, func() bool {
		recheck.mu.Lock()
		defer recheck.mu.Unlock()
		return recheck.round.browser >= 1
	}) {
		t.Fatal("定期复核应已开始一轮并把它记为浏览器判定失效不复核")
	}
	time.Sleep(300 * time.Millisecond)
	if prober.probed(resetTestAlice) != 0 || testStatus(t, service.pool, resetTestAlice).State != aistudio.AccountAuthRequired {
		t.Fatalf("不应复核（复核 %d 次）也不应恢复", prober.probed(resetTestAlice))
	}
}
