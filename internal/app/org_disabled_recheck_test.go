package app

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/api"
)

// 组织停用账户的模型目录复查：状态清除后立即按常规同步、复查遇到其他失败按退避顺延、重启后照旧只做长间隔复查，
// 以及登录复核遇到 app is disabled 时进入组织停用状态

// 假上游 GetAiStudioBenefitTier 的几种结果
const (
	orgUpstreamDisabled     int32 = iota // HTTP 403、协议错误码 7、AI Studio app is disabled
	orgUpstreamOK                        // 正常
	orgUpstreamUnavailable               // HTTP 503
	orgUpstreamUnauthorized              // HTTP 401
	orgUpstreamRateLimited               // HTTP 429
)

// orgRecheckTransport 为目录测试的假上游：GetAiStudioBenefitTier 按 mode 返回，ListModels 等交给 base；tiers 为 GetAiStudioBenefitTier 的请求次数
type orgRecheckTransport struct {
	base  *catalogRPCTransport
	mode  atomic.Int32
	tiers atomic.Int32
}

func (transport *orgRecheckTransport) Do(ctx context.Context, rpc aistudio.RPCRequest) (*aistudio.RPCResponse, error) {
	if rpc.Method == "GetAiStudioBenefitTier" {
		transport.tiers.Add(1)
		reply := func(status int, body string) (*aistudio.RPCResponse, error) {
			return &aistudio.RPCResponse{
				StatusCode: status,
				Header:     http.Header{"Content-Type": {aistudio.JSONProtobufContentType}},
				Body:       io.NopCloser(strings.NewReader(body)),
			}, nil
		}
		switch transport.mode.Load() {
		case orgUpstreamDisabled:
			return reply(http.StatusForbidden, `[7,"AI Studio app is disabled.",[]]`)
		case orgUpstreamUnavailable:
			return reply(http.StatusServiceUnavailable, `[14,"backend unavailable",[]]`)
		case orgUpstreamUnauthorized:
			return reply(http.StatusUnauthorized, `[16,"Request had invalid authentication credentials.",[]]`)
		case orgUpstreamRateLimited:
			return reply(http.StatusTooManyRequests, `[8,"Resource has been exhausted (e.g. check quota).",[]]`)
		}
	}
	return transport.base.Do(ctx, rpc)
}

// orgCatalogEnv 为组织停用的目录测试环境：账户池载入 root 下的账户（没有目录），目录调度器用假时钟、各项间隔改小，
// 登记表保存在 directory（复查时间固定为 24 小时整）
type orgCatalogEnv struct {
	service   *trackedService
	syncer    *catalogSyncer
	pool      *aistudio.AccountPool
	clock     *syncTestClock
	registry  *orgDisabledRegistry
	requests  *requestRegistry
	transport *orgRecheckTransport
	ctx       context.Context
}

func newOrgCatalogEnv(t *testing.T, root string, directory string, mode int32) *orgCatalogEnv {
	t.Helper()
	transport := &orgRecheckTransport{base: &catalogRPCTransport{listModels: catalogTestRaw(t, catalogTestRow("alpha", 0.7))}}
	transport.mode.Store(mode)
	accounts, failures := aistudio.NewAccountStore(root).LoadTolerant()
	if len(failures) > 0 {
		t.Fatal(errors.Join(failures...))
	}
	pool := aistudio.NewAccountPool(accounts, 2)
	t.Cleanup(pool.WaitBackground)
	client, err := aistudio.NewClient(aistudio.ClientOptions{
		Transport: transport,
		Protected: aistudio.ProtectedTransportFunc(func(context.Context, aistudio.GenerateRequest, aistudio.RPCRequest) (*aistudio.RPCResponse, error) {
			return nil, errors.New("unused")
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	pooled, err := aistudio.NewPooledService(pool, client)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	requests := newRequestRegistry(ctx)
	service := &trackedService{lifecycle: ctx, catalog: pooled, service: pooled, pool: pool, requests: requests}
	service.timeout.Store(int64(time.Minute))
	syncer := newCatalogSyncer(service)
	service.catalogSync = syncer
	syncer.tick = 5 * time.Millisecond
	syncer.interval, syncer.fillInterval = time.Nanosecond, time.Nanosecond
	syncer.random = func() float64 { return 0.5 }
	clock := &syncTestClock{now: time.Now()}
	syncer.now = clock.Now
	registry := withOrgDisabled(t, service, directory)
	return &orgCatalogEnv{
		service: service, syncer: syncer, pool: pool, clock: clock, registry: registry, requests: requests,
		transport: transport, ctx: ctx,
	}
}

// start 启动目录调度器，测试结束时先停下、等它退出
func (env *orgCatalogEnv) start(t *testing.T) {
	t.Helper()
	ctx, stop := context.WithCancel(env.ctx)
	_, _, done := env.syncer.start(ctx)
	t.Cleanup(func() {
		stop()
		<-done
	})
}

// task 返回账户的任务副本（没有任务时为零值）
func (env *orgCatalogEnv) task(accountID string) catalogTask {
	env.syncer.mu.Lock()
	defer env.syncer.mu.Unlock()
	if task := env.syncer.tasks[accountID]; task != nil {
		return *task
	}
	return catalogTask{}
}

// advance 把假时钟分 steps 次各前进 step，每次唤醒调度器并留出处理时间
func (env *orgCatalogEnv) advance(step time.Duration, steps int) {
	for range steps {
		env.clock.Advance(step)
		env.syncer.kick()
		time.Sleep(15 * time.Millisecond)
	}
}

// enterViaCatalog 启动调度器，等账户经目录同步进入组织停用、任务改排长间隔复查
func (env *orgCatalogEnv) enterViaCatalog(t *testing.T, accountIDs ...string) {
	t.Helper()
	env.start(t)
	waitCatalog(t, "经目录同步进入组织停用并改排长间隔复查", func() bool {
		for _, accountID := range accountIDs {
			task := env.task(accountID)
			if !env.pool.OrgDisabled(accountID) || task.priority != catalogPriorityOrgRecheck || task.running {
				return false
			}
		}
		return true
	})
}

// TestOrgDisabledClearResyncsCatalog 经目录同步进入组织停用的账户没有目录：认证文件更新（外部替换走 forgetPermissionDenied，
// 不经重新登录的目录同步）与管理员清除后，长间隔复查的任务立即改回常规优先级并同步，几个调度周期内拿到目录、能取得租约，
// 不再等原定 24 小时后的复查
func TestOrgDisabledClearResyncsCatalog(t *testing.T) {
	const replaced, cleared = "replaced-org@example.com", "cleared-org@example.com"
	root := t.TempDir()
	catalogTestAccounts(t, root, replaced, cleared)
	env := newOrgCatalogEnv(t, root, t.TempDir(), orgUpstreamDisabled)
	env.enterViaCatalog(t, replaced, cleared)
	for _, accountID := range []string{replaced, cleared} {
		if status := testStatus(t, env.pool, accountID); len(status.Models) != 0 {
			t.Fatalf("经目录同步进入的账户不应有目录: %+v", status)
		}
	}
	before := env.transport.tiers.Load()
	// 组织重新开放（或换了号）之后：一个外部替换认证文件，一个由管理员在管理页清除
	env.transport.mode.Store(orgUpstreamOK)
	env.service.forgetPermissionDenied(replaced, "认证文件更新")
	admin := &runtimeAdmin{pool: env.pool, service: env.service, requests: env.requests}
	if count, err := admin.ClearOrgDisabled(context.Background(), api.PermissionDeniedClearInput{AccountIDs: []string{cleared}}); err != nil || count != 1 {
		t.Fatalf("管理员清除 = %d err=%v", count, err)
	}
	waitCatalog(t, "清除后立即同步目录", func() bool {
		for _, accountID := range []string{replaced, cleared} {
			status, _ := env.pool.StatusOf(accountID)
			task := env.task(accountID)
			if len(status.Models) == 0 || task.priority != catalogPriorityExpired || task.running {
				return false
			}
		}
		return true
	})
	if calls := env.transport.tiers.Load() - before; calls != 2 {
		t.Fatalf("清除后每个账户应同步一次目录，实际请求 %d 次", calls)
	}
	for _, accountID := range []string{replaced, cleared} {
		status := testStatus(t, env.pool, accountID)
		if status.State != aistudio.AccountReady || env.pool.OrgDisabled(accountID) {
			t.Fatalf("清除后应恢复就绪: %+v", status)
		}
		// 组织停用的账户连指定账户的调度租约都取不到（见 aistudio.TestOrgDisabledStopsScheduling）：清除后照常取得
		lease, ok, err := env.pool.TryAcquireFor(context.Background(), aistudio.AccountSelection{AccountID: accountID})
		if err != nil || !ok {
			t.Fatalf("清除并同步目录后应能取得 %s 的租约: ok=%t err=%v", accountID, ok, err)
		}
		_ = lease.Release()
		if task := env.task(accountID); !task.due.After(env.clock.Now().Add(time.Hour)) {
			t.Fatalf("同步成功后应排常规的过期刷新: %+v", task)
		}
	}
	if !hasLogContaining(env.requests, "已清除组织停用标记，恢复调度 | 来源=认证文件更新") ||
		!hasLogContaining(env.requests, "已清除组织停用标记，恢复调度 | 来源=管理员清除") {
		t.Fatalf("日志 = %v", serviceLogMessages(env.requests))
	}
}

// TestOrgDisabledVerifyPassedClears 组织重新开放 AI Studio 后在管理页验证：浏览器停在 AI Studio 输入框、接口复核也通过时，
// 在随后的目录同步之前就清除组织停用（来源=验证通过），立即恢复调度。目录同步遇到上游 429 时账户也不再是组织停用，
// 与“验证通过”的结论一致，目录任务改回常规优先级（不再按约一天后的复查排着）。原先只靠随后的同步成功才清除，
// 同步 429 时提示验证通过、账户却仍是 org_disabled。接口复核出错（不能下结论）时保持组织停用
func TestOrgDisabledVerifyPassedClears(t *testing.T) {
	const passed, probeFailed = "verify-reopen@example.com", "verify-probe-error@example.com"
	root := t.TempDir()
	catalogTestAccounts(t, root, passed, probeFailed)
	env := newOrgCatalogEnv(t, root, t.TempDir(), orgUpstreamDisabled)
	env.enterViaCatalog(t, passed, probeFailed)
	// 组织重新开放之后：浏览器到了输入框，接口复核（ProbeLogin）通过；随后的目录同步遇到上游 429
	env.transport.mode.Store(orgUpstreamRateLimited)
	prober := newProbeTestService(env.service.service)
	prober.set(probeFailed, errors.New("connection reset"))
	env.service.service = prober
	admin := &runtimeAdmin{
		pool: env.pool, service: env.service, requests: env.requests,
		login: &verifyTestLogin{verification: aistudio.LoginVerification{Authenticated: true}},
	}
	before := env.transport.tiers.Load()
	account, err := admin.VerifyAccount(context.Background(), passed)
	if err != nil {
		t.Fatal(err)
	}
	if env.transport.tiers.Load() == before {
		t.Fatal("验证通过后应同步一次目录（这里遇到上游 429）")
	}
	if account.Authenticated == nil || !*account.Authenticated || account.State == string(aistudio.AccountOrgDisabled) || account.OrgDisabled {
		t.Fatalf("验证通过后不应再是组织停用: %+v", account)
	}
	if env.pool.OrgDisabled(passed) {
		t.Fatal("验证通过后应清除组织停用标记")
	}
	if _, listed := env.registry.entryOf(passed); listed {
		t.Fatal("验证通过后应清除登记")
	}
	if !hasLogContaining(env.requests, "已清除组织停用标记，恢复调度 | 来源=验证通过") {
		t.Fatalf("日志 = %v", serviceLogMessages(env.requests))
	}
	if task := env.task(passed); task.priority == catalogPriorityOrgRecheck {
		t.Fatalf("目录任务应改回常规优先级: %+v", task)
	}
	// 接口复核出错：按浏览器结论算验证通过，但不能说明组织已重新开放，保持组织停用（随后的同步成功时才清除）
	if _, err := admin.VerifyAccount(context.Background(), probeFailed); err != nil {
		t.Fatal(err)
	}
	if !env.pool.OrgDisabled(probeFailed) {
		t.Fatal("接口复核出错时不应清除组织停用")
	}
	if !hasLogContaining(env.requests, "账户验证完成 | 已认证=true", "接口复核=出错") {
		t.Fatalf("日志 = %v", serviceLogMessages(env.requests))
	}
}

// TestOrgDisabledRecheckOtherFailureBackoff 复查时间到了以后上游返回的不是 app is disabled：5xx 按退避顺延（1 分钟起翻倍），
// 一小时内只请求个位数次，顺延的时间与次数写回登记表，只在第 1、2、4… 次写“组织停用账户复查未完成，顺延”、不写“模型目录同步失败”；
// 401 直接顺延一个完整的复查间隔。期间账户一直保持组织停用；上游恢复后到期的复查通过即清除
func TestOrgDisabledRecheckOtherFailureBackoff(t *testing.T) {
	for _, test := range []struct {
		name     string
		mode     int32
		maxCalls int32
	}{
		{"503", orgUpstreamUnavailable, 9},
		{"401", orgUpstreamUnauthorized, 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			const accountID = "org-recheck@example.com"
			root := t.TempDir()
			catalogTestAccounts(t, root, accountID)
			env := newOrgCatalogEnv(t, root, t.TempDir(), orgUpstreamDisabled)
			env.enterViaCatalog(t, accountID)
			entered, _ := env.registry.entryOf(accountID)
			// 复查时间到了，上游这时返回其他失败：之后一小时按一分钟一步推进
			env.transport.mode.Store(test.mode)
			before := env.transport.tiers.Load()
			env.clock.Advance(entered.CheckAt.Sub(env.clock.Now()) + time.Minute)
			env.syncer.kick()
			waitCatalog(t, "到期的复查发出", func() bool { return env.transport.tiers.Load() > before })
			env.advance(time.Minute, 60)
			calls := env.transport.tiers.Load() - before
			if calls > test.maxCalls {
				t.Fatalf("复查遇到 %s 后一小时内请求了 %d 次，期望不超过 %d 次（按退避顺延，不每分钟重试）", test.name, calls, test.maxCalls)
			}
			if !env.pool.OrgDisabled(accountID) {
				t.Fatal("复查没有结论时应保持组织停用")
			}
			entry, _ := env.registry.entryOf(accountID)
			if entry.RecheckFailures != int(calls) || !entry.CheckAt.After(env.clock.Now()) {
				t.Fatalf("登记应记下顺延后的复查时间与次数: %+v（请求 %d 次）", entry, calls)
			}
			if task := env.task(accountID); task.priority != catalogPriorityOrgRecheck || !task.due.Equal(entry.CheckAt) {
				t.Fatalf("任务应按登记的复查时间排长间隔复查: %+v，登记 %+v", task, entry)
			}
			if test.mode == orgUpstreamUnauthorized {
				if wait := entry.CheckAt.Sub(env.clock.Now()); wait < orgDisabledRecheck-2*time.Hour {
					t.Fatalf("401 应顺延一个完整的复查间隔，实际还有 %s", wait)
				}
			}
			if hasLogContaining(env.requests, "模型目录同步失败") {
				t.Fatalf("不应写模型目录同步失败: %v", serviceLogMessages(env.requests))
			}
			lines := logCount(env.requests, "组织停用账户复查未完成，顺延 | 错误=")
			expected := 0
			for failures := 1; failures <= int(calls); failures++ {
				if failures&(failures-1) == 0 {
					expected++
				}
			}
			if lines != expected {
				t.Fatalf("顺延日志应只在第 1、2、4… 次写（%d 次失败 → %d 行），实际 %d 行: %v", calls, expected, lines, serviceLogMessages(env.requests))
			}
			if orgDisabledLogs(env.requests, accountID) != 1 {
				t.Fatalf("进入组织停用的日志只写一行: %v", serviceLogMessages(env.requests))
			}
			if test.mode != orgUpstreamUnavailable {
				return
			}
			// 上游恢复：到期的复查通过即清除
			env.transport.mode.Store(orgUpstreamOK)
			env.clock.Advance(entry.CheckAt.Sub(env.clock.Now()) + time.Minute)
			env.syncer.kick()
			waitCatalog(t, "上游恢复后复查通过", func() bool {
				return !env.pool.OrgDisabled(accountID) && env.task(accountID).priority == catalogPriorityExpired
			})
			if _, listed := env.registry.entryOf(accountID); listed {
				t.Fatal("复查通过后应清除登记")
			}
		})
	}
}

// TestOrgDisabledRestartKeepsLongRecheck 重启后（新的账户池与登记表）对照登记表恢复标记，目录调度器只排长间隔复查，到期时间等于登记的
// 复查时间（没有目录的账户不按缺失立即同步）；登记表里记下的复查失败次数也带过来
func TestOrgDisabledRestartKeepsLongRecheck(t *testing.T) {
	const accountID = "org-restart@example.com"
	root := t.TempDir()
	directory := t.TempDir()
	catalogTestAccounts(t, root, accountID)
	first := newOrgCatalogEnv(t, root, directory, orgUpstreamDisabled)
	first.service.enterOrgDisabled(accountID, accountID, aistudio.OrgDisabledFromStartup, serviceNotAllowedStartupError())
	checkAt := first.registry.postpone(accountID, time.Now().Add(30*time.Hour), 3)
	first.registry.flush()

	// 新进程：上游已恢复正常，没有目录的账户如果按缺失排就会立即同步并清除
	second := newOrgCatalogEnv(t, root, t.TempDir(), orgUpstreamOK)
	second.registry = newOrgDisabledRegistry(directory, second.requests)
	second.service.orgDisabled = second.registry
	second.service.reconcileOrgDisabled()
	if !second.pool.OrgDisabled(accountID) {
		t.Fatal("重启后应恢复组织停用标记")
	}
	second.start(t)
	waitCatalog(t, "重启后排长间隔复查", func() bool { return second.task(accountID).priority == catalogPriorityOrgRecheck })
	second.advance(time.Minute, 10)
	task := second.task(accountID)
	if !task.due.Equal(checkAt) || task.failures != 3 {
		t.Fatalf("任务 = %+v，期望到期时间为登记的 %s、失败次数 3", task, checkAt)
	}
	if calls := second.transport.tiers.Load(); calls != 0 {
		t.Fatalf("复查时间之前不应同步，实际请求 %d 次", calls)
	}
	if !second.pool.OrgDisabled(accountID) {
		t.Fatal("复查之前应保持组织停用")
	}
}

// TestOrgDisabledDispatchReschedulesTask 账户已排着常规任务（缺失、立即到期）时被标为组织停用（例如没有经 enterOrgDisabled、
// 只写了账户池的标记）：调度器取出任务时改排长间隔复查，不发目录请求
func TestOrgDisabledDispatchReschedulesTask(t *testing.T) {
	const accountID = "org-dispatch@example.com"
	root := t.TempDir()
	catalogTestAccounts(t, root, accountID)
	env := newOrgCatalogEnv(t, root, t.TempDir(), orgUpstreamOK)
	now := env.clock.Now()
	env.syncer.reconcile(now)
	if task := env.task(accountID); task.priority != catalogPriorityMissing || task.due.After(now) {
		t.Fatalf("没有目录的账户应排缺失、立即到期: %+v", task)
	}
	if _, err := env.pool.MarkOrgDisabled(accountID, ""); err != nil {
		t.Fatal(err)
	}
	var tasks sync.WaitGroup
	env.syncer.dispatch(env.ctx, &tasks)
	tasks.Wait()
	task := env.task(accountID)
	if task.priority != catalogPriorityOrgRecheck || task.due.Before(now.Add(orgDisabledRecheck-time.Minute)) {
		t.Fatalf("取出时应改排长间隔复查: %+v", task)
	}
	if calls := env.transport.tiers.Load(); calls != 0 {
		t.Fatalf("不应发目录请求，实际 %d 次", calls)
	}
}

// TestOrgDisabledLoginRecheck 登录复核（疑似登录失效与需要登录账户的定期复核）的 GetAiStudioBenefitTier 返回 app is disabled：
// 进入组织停用状态（来源=登录复核），疑似记录移出、疑似冷却解除，不标需要登录；定期复核不计入“跳过”、之后不再取它
func TestOrgDisabledLoginRecheck(t *testing.T) {
	t.Run("疑似登录失效", func(t *testing.T) {
		manager, pool, requests := partitionTestManager(t, []string{partitionNormalA}, nil, 1, 1, 0, 1)
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		service, recheck, prober := recheckTestService(t, ctx, manager, pool, requests)
		registry := withOrgDisabled(t, service, t.TempDir())
		prober.set(partitionNormalA, orgDisabledRPCError("GetAiStudioBenefitTier"))
		if suspectAccountByGeneration(t, pool, partitionNormalA) {
			t.Fatal("第一次 401 不应直接标需要登录")
		}
		if !waitUntil(5*time.Second, func() bool { return pool.OrgDisabled(partitionNormalA) && !recheck.suspected(partitionNormalA) }) {
			t.Fatalf("复核返回 app is disabled 应进入组织停用并移出疑似，复核 %d 次", prober.probed(partitionNormalA))
		}
		if !waitUntil(time.Second, func() bool { return suspectCooldown(t, pool, partitionNormalA) == "" }) {
			t.Fatal("疑似冷却应解除")
		}
		if entry, _ := registry.entryOf(partitionNormalA); entry.Source != aistudio.OrgDisabledFromRecheck {
			t.Fatalf("登记 = %+v", entry)
		}
		time.Sleep(200 * time.Millisecond)
		if probed := prober.probed(partitionNormalA); probed != 1 {
			t.Fatalf("进入组织停用后不应再复核，复核了 %d 次", probed)
		}
		if hasLogContaining(requests, "疑似登录失效复核未完成") || hasLogContaining(requests, "账号需要登录") {
			t.Fatalf("不应按复核出错或登录失效处理: %v", serviceLogMessages(requests))
		}
		pool.ClearOrgDisabled(partitionNormalA)
		if state := testStatus(t, pool, partitionNormalA).State; state != aistudio.AccountReady {
			t.Fatalf("底层状态不应改为需要登录: %s", state)
		}
	})
	t.Run("定期复核", func(t *testing.T) {
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
		registry := withOrgDisabled(t, service, t.TempDir())
		for _, accountID := range []string{partitionNormalA, partitionNormalB} {
			if err := pool.MarkAuthRequired(accountID, "HTTP 401"); err != nil {
				t.Fatal(err)
			}
		}
		prober.set(partitionNormalA, orgDisabledRPCError("GetAiStudioBenefitTier"))
		prober.set(partitionNormalB, authFailureError())
		startRecheckLoop(t, ctx, recheck)
		if !waitUntil(5*time.Second, func() bool { return pool.OrgDisabled(partitionNormalA) }) {
			t.Fatal("定期复核返回 app is disabled 应进入组织停用")
		}
		if entry, _ := registry.entryOf(partitionNormalA); entry.Source != aistudio.OrgDisabledFromRecheck {
			t.Fatalf("登记 = %+v", entry)
		}
		if !waitUntil(5*time.Second, func() bool {
			return hasLogContaining(requests, "登录复核 | 恢复=0 | 仍失效=1 | 跳过=0 | 下一轮=")
		}) {
			t.Fatalf("组织停用不应计入跳过，日志 = %v", serviceLogMessages(requests))
		}
		// 再过几轮：组织停用的账户不再取出来复核
		time.Sleep(500 * time.Millisecond)
		if probed := prober.probed(partitionNormalA); probed != 1 {
			t.Fatalf("组织停用的账户不应再复核，复核了 %d 次", probed)
		}
		recheck.mu.Lock()
		_, recorded := recheck.records[partitionNormalA]
		recheck.mu.Unlock()
		if recorded {
			t.Fatal("组织停用的账户应移出定期复核的记录")
		}
		if hasLogContaining(requests, "登录复核恢复") {
			t.Fatalf("不应恢复为就绪: %v", serviceLogMessages(requests))
		}
	})
}
