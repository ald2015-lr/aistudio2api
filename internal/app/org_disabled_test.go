package app

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/api"
	"github.com/Mag1cFall/AIStudio2API/internal/camoufoxnative"
)

// orgDisabledRPCError 为上游的组织停用错误（照线上原文）
func orgDisabledRPCError(method string) error {
	return &aistudio.RPCError{Method: method, StatusCode: http.StatusForbidden, Code: 7, Message: "AI Studio app is disabled."}
}

// serviceNotAllowedStartupError 为启动时页面停在组织停用页的错误（照 camoufoxnative 的文案，带一段含邮箱的快照）
func serviceNotAllowedStartupError() error {
	return &camoufoxnative.ServiceNotAllowedError{URL: "access.workspace.google.com/ServiceNotAllowed"}
}

// withOrgDisabled 给测试服务挂上保存在 directory 的组织停用登记表（复查时间固定为 24 小时整），并像 newTrackedService 一样挂上回调
func withOrgDisabled(t *testing.T, service *trackedService, directory string) *orgDisabledRegistry {
	t.Helper()
	registry := newOrgDisabledRegistry(directory, service.requests)
	registry.random = func() float64 { return 0.5 }
	t.Cleanup(registry.flush)
	service.orgDisabled = registry
	service.pool.SetOrgDisabledReporter(service.reportOrgDisabled)
	return registry
}

// orgDisabledLogs 返回“账号所属组织已停用 AI Studio，停止调度”日志的条数
func orgDisabledLogs(requests *requestRegistry, accountID string) int {
	requests.mu.Lock()
	defer requests.mu.Unlock()
	count := 0
	for _, entry := range requests.logs {
		if entry.Source == accountID && strings.HasPrefix(entry.Message, "账号所属组织已停用 AI Studio，停止调度 | 来源=") {
			count++
		}
	}
	return count
}

// countingLaunch 记录每个账户的启动次数：failing 里的账户以 err 失败，其余返回不启动浏览器的测试 Worker
type countingLaunch struct {
	mu       sync.Mutex
	failing  map[string]error
	launches map[string]int
}

func (launch *countingLaunch) launch(_ context.Context, accountID string, options camoufoxnative.Options) (*aistudio.NativeWorker, error) {
	launch.mu.Lock()
	launch.launches[accountID]++
	err := launch.failing[accountID]
	launch.mu.Unlock()
	if options.CacheAttached != nil {
		options.CacheAttached()
	}
	if err != nil {
		return nil, err
	}
	worker, _ := aistudio.NewStubWorker(accountID, 0)
	return worker, nil
}

func (launch *countingLaunch) count(accountID string) int {
	launch.mu.Lock()
	defer launch.mu.Unlock()
	return launch.launches[accountID]
}

// TestOrgDisabledStartupStopsScheduling Worker 启动时页面停在组织停用页：账户进入组织停用状态（来源=启动，只写一行日志），
// 之后不再被预热、按需扩容、替补挑中（预热由 B 补位，A 不再启动浏览器），备用候选与可接替统计不算它；不清浏览器缓存；
// 其他路径再遇到时只累计次数、不再写日志；状态计数里计入 org_disabled
func TestOrgDisabledStartupStopsScheduling(t *testing.T) {
	manager, pool, requests := partitionTestManager(t, []string{partitionNormalA, partitionNormalB}, nil, 1, 2, 0, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	service := unschedulableTestService(ctx, manager, pool, requests)
	registry := withOrgDisabled(t, service, t.TempDir())
	launches := &countingLaunch{failing: map[string]error{partitionNormalA: serviceNotAllowedStartupError()}, launches: map[string]int{}}
	manager.launch = launches.launch
	_, err := manager.promote(ctx, partitionNormalA, testRuntimeModel)
	if !aistudio.OrgDisabledFailure(err) {
		t.Fatalf("启动应以组织停用失败: %v", err)
	}
	if status := testStatus(t, pool, partitionNormalA); status.State != aistudio.AccountOrgDisabled || status.Message != aistudio.OrgDisabledReason {
		t.Fatalf("账户应进入组织停用状态: %+v", status)
	}
	entry, listed := registry.entryOf(partitionNormalA)
	if !listed || entry.Source != aistudio.OrgDisabledFromStartup || entry.Count != 1 || !strings.HasPrefix(entry.Message, camoufoxnative.ServiceNotAllowedPrefix) {
		t.Fatalf("登记 = %+v", entry)
	}
	if wait := time.Until(entry.CheckAt); wait < orgDisabledRecheck-time.Minute || wait > orgDisabledRecheck+time.Minute {
		t.Fatalf("复查时间应约在 %s 后，实际 %s", orgDisabledRecheck, wait)
	}
	if count := orgDisabledLogs(requests, partitionNormalA); count != 1 {
		t.Fatalf("应写一行组织停用日志，实际 %d 行: %v", count, serviceLogMessages(requests))
	}
	if !hasLogContaining(requests, "账号所属组织已停用 AI Studio，停止调度 | 来源=启动 | 下次复查=", "| 原文="+camoufoxnative.ServiceNotAllowedPrefix) {
		t.Fatalf("日志格式不对: %v", serviceLogMessages(requests))
	}
	if hasLogContaining(requests, "清理账户浏览器缓存") {
		t.Fatalf("组织停用不应清浏览器缓存: %v", serviceLogMessages(requests))
	}
	// 预热：B 补位，A 不再启动
	manager.StartPrewarm(ctx)
	if !waitUntil(5*time.Second, func() bool { return slices.Contains(manager.WarmAccountIDs(), partitionNormalB) }) {
		t.Fatalf("预热应由 B 补位，驻留 Worker = %v", manager.WarmAccountIDs())
	}
	// 按需扩容、现场启动、替补都不会再启动它的浏览器（兜底：直接指定也立即失败，不占启动名额）
	if _, err := manager.promote(ctx, partitionNormalA, testRuntimeModel); !aistudio.OrgDisabledFailure(err) {
		t.Fatalf("再次启动应立即失败: %v", err)
	}
	if count := launches.count(partitionNormalA); count != 1 {
		t.Fatalf("A 只应启动过一次浏览器，实际 %d 次", count)
	}
	groups, err := manager.classifyBootstrapCandidates(ctx, nil, aistudio.PoolScopeNormal)
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(groups.StandbyReady, partitionNormalA) || slices.Contains(groups.StandbyBusy, partitionNormalA) {
		t.Fatalf("备用候选不应包含组织停用的账户: %+v", groups)
	}
	if spare := pool.SpareAccounts(aistudio.PoolScopeNormal, nil, []string{testRuntimeModel}, 0, 0); spare != 1 {
		t.Fatalf("可接替统计只应算 B，实际 %d", spare)
	}
	if counts := adminAccountCounts(pool.StateCounts()); counts.OrgDisabled != 1 {
		t.Fatalf("状态计数应计入组织停用: %+v", counts)
	}
	// 其他路径再遇到：只累计次数、记下最近来源，不再写日志
	service.enterOrgDisabled(partitionNormalA, partitionNormalA, aistudio.OrgDisabledFromCatalog, orgDisabledRPCError("GetAiStudioBenefitTier"))
	if entry, _ := registry.entryOf(partitionNormalA); entry.Count != 2 || entry.Source != aistudio.OrgDisabledFromStartup || entry.LastSource != aistudio.OrgDisabledFromCatalog {
		t.Fatalf("再次遇到应累计: %+v", entry)
	}
	if count := orgDisabledLogs(requests, partitionNormalA); count != 1 {
		t.Fatalf("同一账户只写一行日志，实际 %d 行", count)
	}
}

// TestOrgDisabledWarmWorkerYields 驻留 Worker 的账户进入组织停用状态：正在进行的请求不中断（Worker 不关），不再接新请求；
// 租约释放后一轮淘汰内关闭，写“WAA Worker 释放 | 原因=组织已停用”，由预热补位
func TestOrgDisabledWarmWorkerYields(t *testing.T) {
	manager, pool, requests := partitionTestManager(t, []string{partitionNormalA, partitionNormalB}, nil, 1, 2, 0, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	service := unschedulableTestService(ctx, manager, pool, requests)
	withOrgDisabled(t, service, t.TempDir())
	if _, err := manager.promote(ctx, partitionNormalA, testRuntimeModel); err != nil {
		t.Fatal(err)
	}
	lease, err := pool.AcquireFor(ctx, aistudio.AccountSelection{AccountID: partitionNormalA, ModelID: testRuntimeModel})
	if err != nil {
		t.Fatal(err)
	}
	service.enterOrgDisabled(partitionNormalA, partitionNormalA, aistudio.OrgDisabledFromRequest, orgDisabledRPCError("GenerateContent"))
	if got := manager.retirementCount(); got != 1 {
		t.Fatalf("进入组织停用后应立即登记释放，登记数 = %d", got)
	}
	time.Sleep(200 * time.Millisecond)
	if !slices.Contains(manager.WarmAccountIDs(), partitionNormalA) {
		t.Fatal("请求还在进行时不应关闭 Worker")
	}
	if next, ok, _ := pool.TryAcquireFor(ctx, aistudio.AccountSelection{AccountID: partitionNormalA, ModelID: testRuntimeModel}); ok {
		_ = next.Release()
		t.Fatal("组织停用的账户不应再接新请求")
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
	if !waitUntil(3*time.Second, func() bool { return !slices.Contains(manager.WarmAccountIDs(), partitionNormalA) }) {
		t.Fatalf("租约释放后应关闭 Worker，驻留 Worker = %v", manager.WarmAccountIDs())
	}
	if !waitUntil(time.Second, func() bool { return hasLogContaining(requests, "WAA Worker 释放 | 原因=组织已停用") }) {
		t.Fatalf("应记录释放日志: %v", serviceLogMessages(requests))
	}
	if !waitUntil(5*time.Second, func() bool { return slices.Contains(manager.WarmAccountIDs(), partitionNormalB) }) {
		t.Fatalf("释放后应由预热补位，驻留 Worker = %v", manager.WarmAccountIDs())
	}
}

// orgDisabledUpstream 为生成测试的假上游：denied 里的账户返回组织停用的 403，其余返回 hello
type orgDisabledUpstream struct {
	forbiddenUpstream
	disabled map[string]bool
}

func (upstream *orgDisabledUpstream) Generate(ctx context.Context, request aistudio.GenerateRequest) (<-chan aistudio.Event, error) {
	upstream.mu.Lock()
	disabled := upstream.disabled[request.AccountID]
	if disabled {
		upstream.calls = append(upstream.calls, request.AccountID)
	}
	upstream.mu.Unlock()
	if disabled {
		return nil, orgDisabledRPCError("GenerateContent")
	}
	return upstream.forbiddenUpstream.Generate(ctx, request)
}

// TestOrgDisabledGenerateSwitchesAccount 生成请求返回 “AI Studio app is disabled”：换号完成（不退避），账户进入组织停用状态（来源=请求），
// 不按单模型 403 冷却、不登记为 403 无权限账户；之后的请求不再选它
func TestOrgDisabledGenerateSwitchesAccount(t *testing.T) {
	const disabledID, goodID = "a-org@example.com", "b-good@example.com"
	upstream := &orgDisabledUpstream{forbiddenUpstream: forbiddenUpstream{denied: map[string]bool{}}, disabled: map[string]bool{disabledID: true}}
	service, pool := primaryBusyService(t, upstream, disabledID, goodID)
	denied := withPermissionDenied(t, service)
	withOrgDisabled(t, service, t.TempDir())
	preverifyRuntimeModelAccess(t, pool, goodID)
	for index := range 4 {
		text, err, _ := runAccountRequest(t, service, "req-org-"+string(rune('a'+index)), "")
		if err != nil || text != "hello" {
			t.Fatalf("第 %d 个请求应换到正常账户完成: text=%q err=%v", index+1, text, err)
		}
	}
	calls := 0
	for _, accountID := range upstream.called() {
		if accountID == disabledID {
			calls++
		}
	}
	if calls != 1 {
		t.Fatalf("组织停用的账户只应被请求一次，实际 %d 次", calls)
	}
	status := testStatus(t, pool, disabledID)
	if status.State != aistudio.AccountOrgDisabled || len(status.Cooldowns) != 0 {
		t.Fatalf("应进入组织停用、不写冷却: %+v", status)
	}
	if entries := denied.list(); len(entries) != 0 {
		t.Fatalf("不应登记为 403 无权限账户: %+v", entries)
	}
	if !hasLogContaining(service.requests, "账号所属组织已停用 AI Studio，停止调度 | 来源=请求") {
		t.Fatalf("日志 = %v", serviceLogMessages(service.requests))
	}
	if hasLogContaining(service.requests, "账号冷却 | 类型=无权限") || hasLogContaining(service.requests, "账号自动暂停") {
		t.Fatalf("不应按单模型 403 处理: %v", serviceLogMessages(service.requests))
	}
}

// TestOrgDisabledPersistsAcrossRestart 组织停用登记保存在账户目录的 .org-disabled.json：关闭时立即写入；重启后（新的账户池）
// 对照登记表恢复标记，仍不参与调度；服务停止期间替换了认证文件的账户按认证文件更新清除，目录已删除的账户的记录直接清除；
// 同一进程内同一目录共用一份登记表；文件损坏时从空表开始
func TestOrgDisabledPersistsAcrossRestart(t *testing.T) {
	const kept, replaced, removed = "kept@example.com", "replaced@example.com", "removed@example.com"
	root := t.TempDir()
	catalogTestAccounts(t, root, kept, replaced, removed)
	first := newCatalogGeneration(t, root, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	requests := newRequestRegistry(ctx)
	service := &trackedService{lifecycle: ctx, pool: first.pool, requests: requests}
	registry := withOrgDisabled(t, service, root)
	for _, accountID := range []string{kept, replaced, removed} {
		service.enterOrgDisabled(accountID, accountID, aistudio.OrgDisabledFromStartup, serviceNotAllowedStartupError())
	}
	registry.flush()
	if _, err := os.Stat(filepath.Join(root, orgDisabledFileName)); err != nil {
		t.Fatalf("关闭时应写入登记文件: %v", err)
	}
	if shared := openOrgDisabledRegistry(root, requests); shared != openOrgDisabledRegistry(root, requests) {
		t.Fatal("同一目录应共用一份登记表")
	}
	// 服务停止期间：替换 replaced 的认证文件（新的登录签名），删除 removed 的目录
	account, err := first.pool.Account(replaced)
	if err != nil {
		t.Fatal(err)
	}
	if err := aistudio.WriteStorageState(account.StoragePath, authTestState(t, "NEW", false)); err != nil {
		t.Fatal(err)
	}
	removedAccount, err := first.pool.Account(removed)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(removedAccount.Directory); err != nil {
		t.Fatal(err)
	}
	second := newCatalogGeneration(t, root, nil)
	restarted := &trackedService{lifecycle: ctx, pool: second.pool, requests: requests}
	restarted.orgDisabled = newOrgDisabledRegistry(root, requests)
	restarted.reconcileOrgDisabled()
	if !second.pool.OrgDisabled(kept) || testStatus(t, second.pool, kept).State != aistudio.AccountOrgDisabled {
		t.Fatal("重启后应恢复组织停用标记")
	}
	if second.pool.OrgDisabled(replaced) {
		t.Fatal("服务停止期间替换了认证文件的账户应清除")
	}
	if _, listed := restarted.orgDisabled.entryOf(replaced); listed {
		t.Fatal("替换了认证文件的账户的记录应清除")
	}
	if _, listed := restarted.orgDisabled.entryOf(removed); listed {
		t.Fatal("目录已删除的账户的记录应清除")
	}
	if !hasLogContaining(requests, "已清除组织停用标记，恢复调度 | 来源=启动时发现认证文件已更新") ||
		!hasLogContaining(requests, "恢复组织停用账户的标记，不参与调度 | 账户=1") {
		t.Fatalf("日志 = %v", serviceLogMessages(requests))
	}
	// 文件损坏时从空表开始
	broken := t.TempDir()
	if err := os.WriteFile(filepath.Join(broken, orgDisabledFileName), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if count := newOrgDisabledRegistry(broken, requests).count(); count != 0 {
		t.Fatalf("损坏的文件应从空表开始，实际 %d 条", count)
	}
}

// TestOrgDisabledClearedByAuthUpdateAndAdmin 清除条件：认证文件更新（与 403 无权限记录同一处理）与管理员在管理页清除（账户 ID
// 不区分大小写）都立即恢复调度并写日志；管理接口列出来源、次数、时间与打码的原文，不带代理
func TestOrgDisabledClearedByAuthUpdateAndAdmin(t *testing.T) {
	const first, second = "first@example.com", "second@example.com"
	service, pool := primaryBusyService(t, &forbiddenUpstream{denied: map[string]bool{}}, first, second)
	withOrgDisabled(t, service, t.TempDir())
	cause := errors.New("刷新账户 " + second + " 的模型目录: AI Studio GetAiStudioBenefitTier 返回 HTTP 403、协议错误码 7: AI Studio app is disabled.")
	service.enterOrgDisabled(first, first, aistudio.OrgDisabledFromStartup, serviceNotAllowedStartupError())
	service.enterOrgDisabled(second, second, aistudio.OrgDisabledFromCatalog, cause)
	// 两个账户的驻留 Worker 登记释放后异步关闭（关闭时短暂持有独占租约，账户显示为忙碌），等关闭完再核对清除后的状态
	waitFor(t, "两个组织停用账户的驻留 Worker 都已释放", func() bool {
		return logCount(service.requests, "WAA Worker 释放 | 原因="+aistudio.UnschedulableOrgDisabled) == 2
	})
	admin := &runtimeAdmin{pool: pool, service: service, requests: service.requests, workers: service.workers}
	listed, err := admin.OrgDisabled(context.Background())
	if err != nil || len(listed) != 2 {
		t.Fatalf("列表 = %+v err=%v", listed, err)
	}
	for _, entry := range listed {
		if entry.ID == second && (entry.Source != aistudio.OrgDisabledFromCatalog || entry.Count != 1 || entry.CheckAt.IsZero() ||
			strings.Contains(entry.Message, second) || !strings.Contains(entry.Message, "se***@***")) {
			t.Fatalf("条目 = %+v，期望来源、次数、复查时间与打码的原文", entry)
		}
	}
	// 认证文件更新：与 403 无权限记录同一处理（外部替换、重新导入、浏览器登录都经 forgetPermissionDenied）
	service.forgetPermissionDenied(first, "认证文件更新")
	if pool.OrgDisabled(first) || testStatus(t, pool, first).State != aistudio.AccountReady {
		t.Fatal("认证文件更新后应清除组织停用、立即恢复调度")
	}
	if !hasLogContaining(service.requests, "已清除组织停用标记，恢复调度 | 来源=认证文件更新") {
		t.Fatalf("日志 = %v", serviceLogMessages(service.requests))
	}
	// 管理员清除：账户 ID 不区分大小写；缺少参数时 400
	if _, err := admin.ClearOrgDisabled(context.Background(), api.PermissionDeniedClearInput{}); err == nil {
		t.Fatal("没有 account_ids 也没有 all 时应报错")
	}
	cleared, err := admin.ClearOrgDisabled(context.Background(), api.PermissionDeniedClearInput{AccountIDs: []string{" SECOND@example.com "}})
	if err != nil || cleared != 1 {
		t.Fatalf("清除 = %d err=%v", cleared, err)
	}
	if pool.OrgDisabled(second) || testStatus(t, pool, second).State != aistudio.AccountReady {
		t.Fatal("管理员清除后应立即恢复调度")
	}
	lease, ok, err := pool.TryAcquireFor(context.Background(), aistudio.AccountSelection{AccountID: second, ModelID: testRuntimeModel})
	if err != nil || !ok {
		t.Fatalf("清除后应能取得租约: ok=%t err=%v", ok, err)
	}
	_ = lease.Release()
	if listed, _ := admin.OrgDisabled(context.Background()); len(listed) != 0 {
		t.Fatalf("清除后列表应为空: %+v", listed)
	}
	if !hasLogContaining(service.requests, "已清除组织停用标记，恢复调度 | 来源=管理员清除") {
		t.Fatalf("日志 = %v", serviceLogMessages(service.requests))
	}
}

// orgCatalogTransport 为目录测试的假上游：disabled 为真时 GetAiStudioBenefitTier 返回组织停用的 403，否则交给 base
type orgCatalogTransport struct {
	base     *catalogRPCTransport
	disabled atomic.Bool
	tiers    atomic.Int32
}

func (transport *orgCatalogTransport) Do(ctx context.Context, rpc aistudio.RPCRequest) (*aistudio.RPCResponse, error) {
	if rpc.Method == "GetAiStudioBenefitTier" {
		transport.tiers.Add(1)
		if transport.disabled.Load() {
			return &aistudio.RPCResponse{
				StatusCode: http.StatusForbidden,
				Header:     http.Header{"Content-Type": {aistudio.JSONProtobufContentType}},
				Body:       io.NopCloser(strings.NewReader(`[7,"AI Studio app is disabled.",[]]`)),
			}, nil
		}
	}
	return transport.base.Do(ctx, rpc)
}

// TestOrgDisabledCatalogLongRecheck 模型目录同步返回 “AI Studio app is disabled”：账户进入组织停用状态（来源=模型目录），
// 不写“模型目录同步失败”、不按失败每分钟重试，只排 24 小时后的长间隔复查；复查时上游不再返回 app is disabled 即清除、恢复调度
func TestOrgDisabledCatalogLongRecheck(t *testing.T) {
	const accountID = "org-catalog@example.com"
	root := t.TempDir()
	catalogTestAccounts(t, root, accountID)
	transport := &orgCatalogTransport{base: &catalogRPCTransport{listModels: catalogTestRaw(t, catalogTestRow("alpha", 0.7))}}
	transport.disabled.Store(true)
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
	registry := withOrgDisabled(t, service, t.TempDir())
	_, _, done := syncer.start(ctx)
	t.Cleanup(func() {
		cancel()
		<-done
	})
	task := func() catalogTask {
		syncer.mu.Lock()
		defer syncer.mu.Unlock()
		if current := syncer.tasks[accountID]; current != nil {
			return *current
		}
		return catalogTask{}
	}
	waitCatalog(t, "进入组织停用并改排长间隔复查", func() bool {
		current := task()
		return pool.OrgDisabled(accountID) && current.priority == catalogPriorityOrgRecheck && !current.running
	})
	entry, _ := registry.entryOf(accountID)
	if entry.Source != aistudio.OrgDisabledFromCatalog || !task().due.Equal(entry.CheckAt) {
		t.Fatalf("登记 = %+v，任务 = %+v", entry, task())
	}
	// 一小时内（原先每分钟重试一次）不再请求
	for range 6 {
		clock.Advance(10 * time.Minute)
		syncer.kick()
		time.Sleep(20 * time.Millisecond)
	}
	if calls := transport.tiers.Load(); calls != 1 {
		t.Fatalf("长间隔复查之前不应再请求，实际 %d 次", calls)
	}
	if hasLogContaining(requests, "模型目录同步失败") || orgDisabledLogs(requests, accountID) != 1 {
		t.Fatalf("不应写同步失败日志，组织停用日志只写一行: %v", serviceLogMessages(requests))
	}
	if status := syncer.status(); status.Queued != 0 {
		t.Fatalf("复查之前不应算排队: %+v", status)
	}
	// 组织重新开放后的复查：清除状态、恢复调度，改为常规过期刷新
	transport.disabled.Store(false)
	clock.Advance(orgDisabledRecheck + 3*time.Hour)
	syncer.kick()
	waitCatalog(t, "复查通过后清除", func() bool { return !pool.OrgDisabled(accountID) && task().priority == catalogPriorityExpired })
	if status := testStatus(t, pool, accountID); status.State != aistudio.AccountReady || len(status.Models) == 0 {
		t.Fatalf("复查通过后应恢复就绪并有目录: %+v", status)
	}
	if !hasLogContaining(requests, "已清除组织停用标记，恢复调度 | 来源=复查通过") {
		t.Fatalf("日志 = %v", serviceLogMessages(requests))
	}
	if _, listed := registry.entryOf(accountID); listed {
		t.Fatal("复查通过后应清除登记")
	}
}

// TestOrgDisabledVerify 管理验证（新账户自动处理同一路径）遇到组织停用页：验证未通过、原因为组织停用，不标需要登录、
// 不写“账号需要登录”，账户进入组织停用状态（来源=验证）；浏览器已登录但接口返回 app is disabled 时同样如此
func TestOrgDisabledVerify(t *testing.T) {
	isolateDisableNotes(t)
	t.Run("页面停在组织停用页", func(t *testing.T) {
		admin, _, accounts := enableTestAdmin(t, "verify-org@example.com")
		withOrgDisabled(t, admin.service, t.TempDir())
		admin.login = &verifyTestLogin{verification: aistudio.LoginVerification{
			Reason: aistudio.OrgDisabledReason, PageURL: "access.workspace.google.com/ServiceNotAllowed", OrgDisabled: true,
		}}
		admin.service.service = newProbeTestService(nil)
		admin.service.authRecheck = newAuthRecheck(admin.service)
		account, err := admin.VerifyAccount(context.Background(), accounts[0].ID)
		if err != nil {
			t.Fatal(err)
		}
		if account.Authenticated == nil || *account.Authenticated || account.VerifyReason != aistudio.OrgDisabledReason ||
			account.State != string(aistudio.AccountOrgDisabled) {
			t.Fatalf("返回 = %+v，期望未通过、组织停用", account)
		}
		if hasLogContaining(admin.requests, "账号需要登录") || !hasLogContaining(admin.requests, "账号所属组织已停用 AI Studio，停止调度 | 来源=验证") {
			t.Fatalf("日志 = %v", serviceLogMessages(admin.requests))
		}
		if !hasLogContaining(admin.requests, "账户验证完成 | 已认证=false", "页面=access.workspace.google.com/ServiceNotAllowed") {
			t.Fatalf("日志 = %v", serviceLogMessages(admin.requests))
		}
	})
	t.Run("接口返回 app is disabled", func(t *testing.T) {
		admin, _, accounts := enableTestAdmin(t, "verify-org-api@example.com")
		withOrgDisabled(t, admin.service, t.TempDir())
		admin.login = &verifyTestLogin{verification: aistudio.LoginVerification{Authenticated: true}}
		prober := newProbeTestService(nil)
		prober.set(accounts[0].ID, orgDisabledRPCError("GetAiStudioBenefitTier"))
		admin.service.service = prober
		admin.service.authRecheck = newAuthRecheck(admin.service)
		account, err := admin.VerifyAccount(context.Background(), accounts[0].ID)
		if err != nil {
			t.Fatal(err)
		}
		if account.Authenticated == nil || *account.Authenticated || account.State != string(aistudio.AccountOrgDisabled) {
			t.Fatalf("返回 = %+v，期望组织停用", account)
		}
		if !hasLogContaining(admin.requests, "账户验证完成 | 已认证=false", "接口复核=组织停用") {
			t.Fatalf("日志 = %v", serviceLogMessages(admin.requests))
		}
	})
	t.Run("停用账户", func(t *testing.T) {
		// 停用账户（批量导入后待验证、新账户自动处理验证未通过后保持停用）的对外状态始终是 disabled：返回另带 org_disabled，
		// 前端据此提示组织停用、批量验证单独计数，不当成登录失效；账户列表的 message 在停用原因后接上组织停用的原因
		admin, _, accounts := enableTestAdmin(t, "verify-org-off@example.com")
		withOrgDisabled(t, admin.service, t.TempDir())
		accountID := accounts[0].ID
		disableTestAccount(t, admin.pool, accountID)
		admin.login = &verifyTestLogin{verification: aistudio.LoginVerification{
			Reason: aistudio.OrgDisabledReason, PageURL: "access.workspace.google.com/ServiceNotAllowed", OrgDisabled: true,
		}}
		admin.service.service = newProbeTestService(nil)
		admin.service.authRecheck = newAuthRecheck(admin.service)
		account, err := admin.VerifyAccount(context.Background(), accountID)
		if err != nil {
			t.Fatal(err)
		}
		if account.Authenticated == nil || *account.Authenticated || account.State != string(aistudio.AccountDisabled) ||
			!account.OrgDisabled || account.VerifyReason != aistudio.OrgDisabledReason {
			t.Fatalf("返回 = %+v，期望状态仍为停用、org_disabled 为真、原因为组织停用", account)
		}
		if !strings.Contains(mustJSON(t, account), `"org_disabled":true`) {
			t.Fatalf("返回的 JSON 应带 org_disabled: %s", mustJSON(t, account))
		}
		if !admin.pool.OrgDisabled(accountID) || hasLogContaining(admin.requests, "账号需要登录") {
			t.Fatalf("应进入组织停用状态、不标需要登录，日志 = %v", serviceLogMessages(admin.requests))
		}
		listed, err := admin.account(accountID)
		if err != nil {
			t.Fatal(err)
		}
		if listed.State != string(aistudio.AccountDisabled) || !listed.OrgDisabled || !strings.HasSuffix(listed.Message, "；"+aistudio.OrgDisabledReason) {
			t.Fatalf("账户列表 = %+v，期望停用、org_disabled 为真、message 接上组织停用的原因", listed)
		}
		// 新账户自动处理验证未通过的停用原因里已经带着组织停用的原因时不重复
		accountDisableNotes.set(accountID, "新账户自动处理验证未通过（"+aistudio.OrgDisabledReason+"），认证文件更新或重启后按默认启用", true)
		if listed, _ = admin.account(accountID); strings.Count(listed.Message, aistudio.OrgDisabledReason) != 1 {
			t.Fatalf("message 不应重复组织停用的原因: %s", listed.Message)
		}
		// 清除标记后不再带 org_disabled（省略这个字段，旧前端不受影响）
		admin.pool.ClearOrgDisabled(accountID)
		if listed, _ = admin.account(accountID); listed.OrgDisabled || strings.Contains(mustJSON(t, listed), "org_disabled") {
			t.Fatalf("清除后不应带 org_disabled: %s", mustJSON(t, listed))
		}
	})
}

// TestOrgDisabledCauseSummary 原文压成一行、邮箱打码、截到 200 字
func TestOrgDisabledCauseSummary(t *testing.T) {
	long := errors.New("刷新账户 someone@example.com 的模型目录:\n" + strings.Repeat("x", 300))
	summary := orgDisabledCauseSummary(long)
	if strings.Contains(summary, "someone") || strings.Contains(summary, "\n") || !strings.Contains(summary, "so***@***") {
		t.Fatalf("应打码并压成一行: %s", summary)
	}
	if runes := []rune(summary); len(runes) != orgDisabledMessageLimit+1 || !strings.HasSuffix(summary, "…") {
		t.Fatalf("应截到 %d 字: %d", orgDisabledMessageLimit, len(runes))
	}
	if orgDisabledCauseSummary(nil) != "-" {
		t.Fatal("没有错误时为 -")
	}
}

// TestOrgDisabledWatcherAuthUpdate 账户目录扫描对组织停用账户与 403 无权限账户同样处理：第一次看到时就对照（载入账户池之后、
// 第一次扫描之前替换的认证文件），同一会话里重新导出（登录签名 Cookie 不变、内容变了）也算更新；更新后清除组织停用、立即恢复调度。
// 认证文件没变的账户保持组织停用
func TestOrgDisabledWatcherAuthUpdate(t *testing.T) {
	const replaced, reexported, kept = "replaced@example.com", "reexported@example.com", "kept@example.com"
	admin, _, _ := enableTestAdmin(t, replaced, reexported, kept)
	registry := withOrgDisabled(t, admin.service, "")
	pool := admin.pool
	for _, accountID := range []string{replaced, reexported, kept} {
		admin.service.enterOrgDisabled(accountID, accountID, aistudio.OrgDisabledFromStartup, serviceNotAllowedStartupError())
	}
	account, err := pool.Account(replaced)
	if err != nil {
		t.Fatal(err)
	}
	if err := aistudio.WriteStorageState(account.StoragePath, authTestState(t, "NEW", false)); err != nil {
		t.Fatal(err)
	}
	base := time.Now().Add(-time.Minute)
	for _, accountID := range []string{replaced, reexported, kept} {
		settleAuthFile(t, pool, accountID, base)
	}
	watcher := newTestAccountWatcher(admin)
	scanAndWait(t, watcher)
	if pool.OrgDisabled(replaced) || testStatus(t, pool, replaced).State != aistudio.AccountReady {
		t.Fatal("第一次扫描就应发现替换过的认证文件并清除组织停用")
	}
	if !pool.OrgDisabled(reexported) || !pool.OrgDisabled(kept) {
		t.Fatal("认证文件没变的账户应保持组织停用")
	}
	// 同一会话里重新导出：登录签名 Cookie 不变，其他 Cookie 变了
	account, err = pool.Account(reexported)
	if err != nil {
		t.Fatal(err)
	}
	state, err := aistudio.LoadStorageState(account.StoragePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := aistudio.WriteStorageState(account.StoragePath, withCookie(state, "NID", "browser-reexport")); err != nil {
		t.Fatal(err)
	}
	settleAuthFile(t, pool, reexported, base.Add(20*time.Second))
	scanAndWait(t, watcher)
	if pool.OrgDisabled(reexported) {
		t.Fatal("同一会话重新导出的认证文件应清除组织停用")
	}
	if _, listed := registry.entryOf(reexported); listed {
		t.Fatal("应清除登记")
	}
	if !pool.OrgDisabled(kept) {
		t.Fatal("认证文件没变的账户应保持组织停用")
	}
	if !hasLogContaining(admin.requests, "已清除组织停用标记，恢复调度 | 来源=认证文件更新") {
		t.Fatalf("日志 = %v", serviceLogMessages(admin.requests))
	}
}
