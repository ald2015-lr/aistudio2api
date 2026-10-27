package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/camoufoxnative"
)

// 组织停用的几条进入与让位路径：启动期间被其他路径确认、排队等启动名额期间被确认、生成请求按已冷却换号、
// 新账户自动处理跳过、浏览器登录遇到组织停用页（用已保存的认证文件复核接口确认后才进入）

// TestOrgDisabledDuringStartupDiscardsWorker 浏览器启动期间账户被其他路径（这里是模型目录）确认所属组织停用了 AI Studio：
// 刚启动的 Worker 不发布、立即关闭，启动返回组织停用；不按启动来源再登记一次（次数与来源不变，日志只有一行）
func TestOrgDisabledDuringStartupDiscardsWorker(t *testing.T) {
	manager, pool, requests := partitionTestManager(t, []string{partitionNormalA, partitionNormalB}, nil, 1, 2, 0, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	service := unschedulableTestService(ctx, manager, pool, requests)
	registry := withOrgDisabled(t, service, t.TempDir())
	var closed func() int
	manager.launch = func(_ context.Context, accountID string, _ camoufoxnative.Options) (*aistudio.NativeWorker, error) {
		worker, closes := aistudio.NewStubWorker(accountID, 0)
		if accountID == partitionNormalA {
			closed = closes
			service.enterOrgDisabled(accountID, accountID, aistudio.OrgDisabledFromCatalog, orgDisabledRPCError("GetAiStudioBenefitTier"))
		}
		return worker, nil
	}
	_, err := manager.promote(ctx, partitionNormalA, testRuntimeModel)
	if !aistudio.OrgDisabledFailure(err) || !errors.Is(err, errWorkerOrgDisabledDuringStart) {
		t.Fatalf("启动期间被确认组织停用时应返回组织停用: %v", err)
	}
	if slices.Contains(manager.WarmAccountIDs(), partitionNormalA) {
		t.Fatalf("刚启动的 Worker 不应发布，驻留 Worker = %v", manager.WarmAccountIDs())
	}
	if closed == nil || closed() != 1 {
		t.Fatal("刚启动的浏览器应立即关闭")
	}
	entry, _ := registry.entryOf(partitionNormalA)
	if entry.Count != 1 || entry.Source != aistudio.OrgDisabledFromCatalog || entry.LastSource != aistudio.OrgDisabledFromCatalog {
		t.Fatalf("不应按启动再登记一次: %+v", entry)
	}
	if count := orgDisabledLogs(requests, partitionNormalA); count != 1 {
		t.Fatalf("组织停用日志只写一行，实际 %d 行", count)
	}
	// 另一个账户照常启动
	if _, err := manager.promote(ctx, partitionNormalB, testRuntimeModel); err != nil {
		t.Fatal(err)
	}
}

// TestOrgDisabledWhileQueuedForStartupSlot 排队等冷启动名额期间账户被确认所属组织停用了 AI Studio：名额到手后立即让出，
// 不申请出口、不启动浏览器，也不按启动再登记一次；之后的排队者照常拿到名额
func TestOrgDisabledWhileQueuedForStartupSlot(t *testing.T) {
	manager, gate := slotTestManager(t, "camoufox-test", 1, "a@example.com", "b@example.com", "c@example.com")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	service := &trackedService{lifecycle: ctx, pool: manager.pool, requests: manager.requests, workers: manager}
	registry := withOrgDisabled(t, service, t.TempDir())
	background := withBackgroundStartup(ctx)
	results := make(map[string]chan error)
	for _, accountID := range []string{"a@example.com", "b@example.com", "c@example.com"} {
		results[accountID] = make(chan error, 1)
	}
	start := func(accountID string) {
		go func() {
			_, err := manager.ensureWorker(background, accountID, "", false)
			results[accountID] <- err
		}()
	}
	// 容量 1：a 占着名额停在启动里，b、c 依次排队
	start("a@example.com")
	gate.waitStarted(t, "a@example.com", "容量内的预热")
	start("b@example.com")
	waitStartupUsage(t, &manager.startupSlots, 1, 0, 1)
	start("c@example.com")
	waitStartupUsage(t, &manager.startupSlots, 1, 0, 2)
	service.enterOrgDisabled("b@example.com", "b@example.com", aistudio.OrgDisabledFromRequest, orgDisabledRPCError("GenerateContent"))
	gate.open()
	for accountID, result := range results {
		select {
		case err := <-result:
			if accountID == "b@example.com" {
				if !aistudio.OrgDisabledFailure(err) || !errors.Is(err, errWorkerOrgDisabled) {
					t.Fatalf("排队期间被确认组织停用的启动应返回组织停用: %v", err)
				}
			} else if err != nil {
				t.Fatalf("%s 应照常启动: %v", accountID, err)
			}
		case <-ctx.Done():
			t.Fatalf("%s 的启动没有结束", accountID)
		}
	}
	if gate.isStarted("b@example.com") {
		t.Fatal("组织停用的账户不应启动浏览器")
	}
	if entry, _ := registry.entryOf("b@example.com"); entry.Count != 1 || entry.LastSource != aistudio.OrgDisabledFromRequest {
		t.Fatalf("不应按启动再登记一次: %+v", entry)
	}
	waitStartupUsage(t, &manager.startupSlots, 0, 0, 0)
}

// TestOrgDisabledGenerateSkipsFailureLimit 生成请求连续遇到多个组织停用的账户（超过连续失败上限 generateFailureLimit）：
// 按已冷却换号，不计连续失败、不退避，仍在每请求换号硬上限内换到正常账户完成；之后的请求不再选这些账户
func TestOrgDisabledGenerateSkipsFailureLimit(t *testing.T) {
	disabled := make(map[string]bool)
	accountIDs := make([]string, 0, generateFailureLimit+2)
	for index := range generateFailureLimit + 1 {
		accountID := fmt.Sprintf("a%d-org@example.com", index)
		disabled[accountID] = true
		accountIDs = append(accountIDs, accountID)
	}
	const goodID = "z-good@example.com"
	accountIDs = append(accountIDs, goodID)
	if len(accountIDs) > generateSwitchCeiling {
		t.Fatalf("账户数 %d 超过每请求换号硬上限 %d", len(accountIDs), generateSwitchCeiling)
	}
	upstream := &orgDisabledUpstream{forbiddenUpstream: forbiddenUpstream{denied: map[string]bool{}}, disabled: disabled}
	service, pool := primaryBusyService(t, upstream, accountIDs...)
	withOrgDisabled(t, service, t.TempDir())
	text, err, elapsed := runAccountRequest(t, service, "req-org-limit", "")
	if err != nil || text != "hello" {
		t.Fatalf("应换到正常账户完成: text=%q err=%v 已请求=%v", text, err, upstream.called())
	}
	calls := 0
	for _, accountID := range upstream.called() {
		if disabled[accountID] {
			calls++
		}
	}
	if calls <= generateFailureLimit {
		t.Fatalf("应先依次遇到超过 %d 个组织停用的账户（覆盖连续失败上限），实际 %d 个: %v", generateFailureLimit, calls, upstream.called())
	}
	// 不退避：按一般失败换号时 200ms 起翻倍，6 次合计约 6 秒
	if elapsed > 1500*time.Millisecond {
		t.Fatalf("组织停用换号不应退避，用时 %s", elapsed)
	}
	for accountID := range disabled {
		if !pool.OrgDisabled(accountID) {
			t.Fatalf("%s 应进入组织停用", accountID)
		}
	}
	if text, err, _ := runAccountRequest(t, service, "req-org-limit-2", ""); err != nil || text != "hello" {
		t.Fatalf("之后的请求应直接用正常账户: text=%q err=%v", text, err)
	}
	if after := len(upstream.called()); after != calls+2 {
		t.Fatalf("之后的请求不应再选组织停用的账户: %v", upstream.called())
	}
}

// TestOrgDisabledOnboardingSkipped 新账户自动处理遇到已列为组织停用的账户：记为跳过、原因为组织停用，不启动浏览器验证、不启用
func TestOrgDisabledOnboardingSkipped(t *testing.T) {
	isolateDisableNotes(t)
	admin, _, accounts := enableTestAdmin(t, "onboard-org@example.com")
	withOrgDisabled(t, admin.service, t.TempDir())
	login := &verifyTestLogin{verification: aistudio.LoginVerification{Authenticated: true}}
	admin.login = login
	accountID := accounts[0].ID
	disableTestAccount(t, admin.pool, accountID)
	admin.service.enterOrgDisabled(accountID, accountID, aistudio.OrgDisabledFromStartup, serviceNotAllowedStartupError())
	onboarder := newTestOnboarder(admin, t.TempDir())
	admin.onboard = onboarder
	result, detail, retry := onboarder.process(context.Background(), accountID, defaultOnboardingPolicy())
	if result != onboardSkipped || detail != aistudio.OrgDisabledReason || retry {
		t.Fatalf("结果 = %s %q %t，期望跳过、原因为组织停用", result, detail, retry)
	}
	if login.calls.Load() != 0 {
		t.Fatal("不应启动浏览器验证")
	}
	if testStatus(t, admin.pool, accountID).Enabled {
		t.Fatal("不应启用组织停用的账户")
	}
	if !hasLogContaining(admin.requests, "新账户自动处理 | 跳过 | 原因="+aistudio.OrgDisabledReason) {
		t.Fatalf("日志 = %v", serviceLogMessages(admin.requests))
	}
}

// orgDisabledLogin 为测试用隔离浏览器：登录后页面停在组织停用页
type orgDisabledLogin struct{}

func (orgDisabledLogin) Login(context.Context, aistudio.IsolatedLoginRequest) (aistudio.IsolatedLoginResult, error) {
	return aistudio.IsolatedLoginResult{}, &camoufoxnative.ServiceNotAllowedError{URL: "access.workspace.google.com/ServiceNotAllowed"}
}

func (orgDisabledLogin) Verify(context.Context, aistudio.IsolatedLoginRequest, aistudio.StorageState) (aistudio.LoginVerification, error) {
	return aistudio.LoginVerification{}, errors.New("测试中不验证")
}

// TestOrgDisabledLoginAccount 管理页对已有账户浏览器登录，登录后页面停在组织停用页：登录失败、错误为组织停用，不标需要登录。
// 落到这一页时还读不到登录邮箱（可见浏览器里登录的可能是别的账号），用账户已保存的认证文件复核接口：同样返回 app is disabled
// 才进入组织停用状态（来源=登录）；复核通过、401、出错或服务不支持复核时只报告登录失败、写一行警告，账户状态不变、照常调度
func TestOrgDisabledLoginAccount(t *testing.T) {
	isolateDisableNotes(t)
	for _, test := range []struct {
		name      string
		probe     error
		noProber  bool
		confirmed bool
		outcome   string
	}{
		{name: "接口复核同样返回 app is disabled", probe: orgDisabledRPCError("GetAiStudioBenefitTier"), confirmed: true},
		{name: "接口复核通过（登录的是别的账号）", outcome: "通过"},
		{name: "接口复核 401", probe: &aistudio.RPCError{Method: "GetAiStudioBenefitTier", StatusCode: http.StatusUnauthorized, Code: 16, Message: "invalid authentication credentials"}, outcome: "401"},
		{name: "接口复核出错", probe: errors.New("connection reset"), outcome: "出错"},
		{name: "服务不支持复核", noProber: true, outcome: "跳过"},
	} {
		t.Run(test.name, func(t *testing.T) {
			admin, _, accounts := enableTestAdmin(t, "login-org@example.com")
			registry := withOrgDisabled(t, admin.service, t.TempDir())
			admin.login = orgDisabledLogin{}
			accountID := accounts[0].ID
			prober := newProbeTestService(nil)
			prober.set(accountID, test.probe)
			if !test.noProber {
				admin.service.service = prober
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if _, err := admin.LoginAccount(ctx, accountID); !aistudio.OrgDisabledFailure(err) {
				t.Fatalf("登录应以组织停用失败: %v", err)
			}
			if !hasLogContaining(admin.requests, "账户登录失败 | 耗时=", "| 错误="+aistudio.OrgDisabledReason+" url=access.workspace.google.com/ServiceNotAllowed") {
				t.Fatalf("日志 = %v", serviceLogMessages(admin.requests))
			}
			if !test.noProber && prober.probed(accountID) != 1 {
				t.Fatalf("应用已保存的认证文件复核一次接口，实际 %d 次", prober.probed(accountID))
			}
			entry, listed := registry.entryOf(accountID)
			if test.confirmed {
				if !admin.pool.OrgDisabled(accountID) || !listed || entry.Source != aistudio.OrgDisabledFromLogin {
					t.Fatalf("接口复核确认后应进入组织停用状态（来源=登录）: 登记=%+v", entry)
				}
				if !hasLogContaining(admin.requests, "账号所属组织已停用 AI Studio，停止调度 | 来源=登录") {
					t.Fatalf("日志 = %v", serviceLogMessages(admin.requests))
				}
				admin.pool.ClearOrgDisabled(accountID)
			} else {
				if admin.pool.OrgDisabled(accountID) || listed || orgDisabledLogs(admin.requests, accountID) != 0 {
					t.Fatalf("接口复核没有确认时账户状态不应改变: 登记=%+v", entry)
				}
				if !hasLogContaining(admin.requests, "账户登录 | 登录后停在组织停用页，但已保存的认证文件接口复核没有返回 app is disabled", "接口复核="+test.outcome) {
					t.Fatalf("应写一行警告，日志 = %v", serviceLogMessages(admin.requests))
				}
				// 照常调度：取得指定账户的调度租约（组织停用的账户取不到）
				lease, ok, err := admin.pool.TryAcquireFor(context.Background(), aistudio.AccountSelection{AccountID: accountID})
				if err != nil || !ok {
					t.Fatalf("账户应照常调度: ok=%t err=%v", ok, err)
				}
				_ = lease.Release()
			}
			if state := testStatus(t, admin.pool, accountID).State; state == aistudio.AccountAuthRequired {
				t.Fatal("组织停用页不应标需要登录")
			}
		})
	}
}

// TestOrgDisabledAfterStartupRenewal 按需启动先跳到登录页、续签后重新启动时落到组织停用页：这次就进入组织停用状态（来源=启动），
// 不标需要登录，不等下次启动才识别
func TestOrgDisabledAfterStartupRenewal(t *testing.T) {
	pool, account := authTestPool(t, true)
	manager, launches := authTestManager(t, pool, account, func(call int32) (*aistudio.NativeWorker, error) {
		if call == 1 {
			return nil, loginRedirectError()
		}
		return nil, serviceNotAllowedStartupError()
	})
	counts := &authTestCounts{}
	refresher := newAuthTestRefresher(t, counts)
	refresher.reset = func(accountID string) error {
		counts.resets.Add(1)
		return manager.Reset(accountID)
	}
	manager.refresher = refresher
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	service := &trackedService{lifecycle: ctx, pool: pool, workers: manager, requests: manager.requests}
	registry := withOrgDisabled(t, service, t.TempDir())
	if _, err := manager.Worker(aistudio.ContextWithAccountLease(ctx, mustLease(t, pool)), authTestAccount, "gemini-test"); !aistudio.OrgDisabledFailure(err) {
		t.Fatalf("续签后落到组织停用页应以组织停用失败: %v", err)
	}
	if launches.Load() != 2 || counts.refreshes.Load() != 1 {
		t.Fatalf("应续签一次后重新启动: 启动=%d 续签=%d", launches.Load(), counts.refreshes.Load())
	}
	if !pool.OrgDisabled(authTestAccount) {
		t.Fatal("续签后的第二次启动落到组织停用页时应进入组织停用状态")
	}
	if entry, _ := registry.entryOf(authTestAccount); entry.Source != aistudio.OrgDisabledFromStartup {
		t.Fatalf("登记 = %+v", entry)
	}
	pool.ClearOrgDisabled(authTestAccount)
	if status := authTestStatus(t, pool); status.State == aistudio.AccountAuthRequired {
		t.Fatal("组织停用不应标需要登录")
	}
}

// mustLease 取得账户的调度租约，测试结束时释放
func mustLease(t *testing.T, pool *aistudio.AccountPool) *aistudio.AccountLease {
	t.Helper()
	lease, err := pool.AcquireFor(context.Background(), aistudio.AccountSelection{AccountID: authTestAccount})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lease.Release() })
	return lease
}
