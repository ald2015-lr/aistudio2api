package app

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/camoufoxnative"
)

// restrictedLogs 返回以 prefix 开头的“…，停止调度 | 来源=”日志的条数
func restrictedLogs(requests *requestRegistry, accountID string, prefix string) int {
	requests.mu.Lock()
	defer requests.mu.Unlock()
	count := 0
	for _, entry := range requests.logs {
		if entry.Source == accountID && strings.HasPrefix(entry.Message, prefix+"，停止调度 | 来源=") {
			count++
		}
	}
	return count
}

// TestRestrictedStartupPersistsKind Worker 启动时页面停在 servicerestricted 页（按上游 a68acad 移植的识别）：走组织停用的同一套机制
// 停止调度，但原因按种类写：日志“Google 已限制该账号使用 AI Studio，停止调度 | 来源=启动”（不写组织停用的日志），账户状态说明、
// 管理视图的 restriction 与不可调度原因都是服务受限，不当成登录失效；登记表记下种类，重启后按种类恢复；管理页列表带种类与说明。
// 之后接口返回 “Account not supported.” 时改记为年龄或资格、再写一行
func TestRestrictedStartupPersistsKind(t *testing.T) {
	manager, pool, requests := partitionTestManager(t, []string{partitionNormalA, partitionNormalB}, nil, 1, 2, 0, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	service := unschedulableTestService(ctx, manager, pool, requests)
	directory := t.TempDir()
	registry := withOrgDisabled(t, service, directory)
	restricted := &camoufoxnative.ServiceNotAllowedError{URL: "accounts.google.com/info/servicerestricted", Kind: camoufoxnative.RestrictionService}
	launches := &countingLaunch{failing: map[string]error{partitionNormalA: restricted}, launches: map[string]int{}}
	manager.launch = launches.launch
	if _, err := manager.promote(ctx, partitionNormalA, testRuntimeModel); !aistudio.OrgDisabledFailure(err) {
		t.Fatalf("启动应以拒绝账号失败: %v", err)
	}
	status := testStatus(t, pool, partitionNormalA)
	if status.State != aistudio.AccountOrgDisabled || status.Restriction != aistudio.RestrictionService ||
		status.Message != camoufoxnative.ServiceRestrictedPrefix {
		t.Fatalf("账户应按服务受限停止调度: %+v", status)
	}
	if hasLogContaining(requests, "账号需要登录") {
		t.Fatalf("服务受限不是登录失效: %v", serviceLogMessages(requests))
	}
	if restrictedLogs(requests, partitionNormalA, camoufoxnative.ServiceRestrictedPrefix) != 1 || orgDisabledLogs(requests, partitionNormalA) != 0 {
		t.Fatalf("应写一行服务受限的日志、不写组织停用的日志: %v", serviceLogMessages(requests))
	}
	if entry, listed := registry.entryOf(partitionNormalA); !listed || entry.Kind != string(aistudio.RestrictionService) {
		t.Fatalf("登记应记下种类: %+v", entry)
	}
	if reason := pool.UnschedulableSet([]string{partitionNormalA})[partitionNormalA]; reason != aistudio.UnschedulableServiceRestricted {
		t.Fatalf("不可调度原因 = %q", reason)
	}
	dto := adminAccountDTO(status)
	if dto.Restriction != string(aistudio.RestrictionService) || !dto.OrgDisabled {
		t.Fatalf("管理视图 = %+v", dto)
	}
	admin := &runtimeAdmin{service: service, pool: pool, requests: requests}
	listed, err := admin.OrgDisabled(ctx)
	if err != nil || len(listed) != 1 || listed[0].Restriction != string(aistudio.RestrictionService) || listed[0].Reason != camoufoxnative.ServiceRestrictedPrefix {
		t.Fatalf("管理页列表 = %+v %v", listed, err)
	}
	// 接口随后返回 “Account not supported.”：改记为年龄或资格，种类变了再写一行
	service.enterOrgDisabled(partitionNormalA, partitionNormalA, aistudio.OrgDisabledFromRequest,
		&aistudio.RPCError{Method: "GenerateContent", StatusCode: http.StatusForbidden, Code: 7, Message: "Account not supported."})
	if status = testStatus(t, pool, partitionNormalA); status.Restriction != aistudio.RestrictionAge || status.Message != camoufoxnative.AgeRequirementPrefix {
		t.Fatalf("应改记为年龄或资格: %+v", status)
	}
	if restrictedLogs(requests, partitionNormalA, camoufoxnative.AgeRequirementPrefix) != 1 {
		t.Fatalf("种类变了应再写一行: %v", serviceLogMessages(requests))
	}
	// 同一种类再次遇到不再写
	service.enterOrgDisabled(partitionNormalA, partitionNormalA, aistudio.OrgDisabledFromCatalog,
		&aistudio.RPCError{Method: "ListModels", StatusCode: http.StatusForbidden, Code: 7, Message: "Account not supported."})
	if restrictedLogs(requests, partitionNormalA, camoufoxnative.AgeRequirementPrefix) != 1 {
		t.Fatalf("同一种类不应重复写: %v", serviceLogMessages(requests))
	}
	registry.flush()
	// 重启：新的账户池按登记的种类恢复
	pool.ClearOrgDisabled(partitionNormalA)
	restarted := &trackedService{lifecycle: ctx, pool: pool, requests: requests, orgDisabled: newOrgDisabledRegistry(directory, requests)}
	restarted.reconcileOrgDisabled()
	if status = testStatus(t, pool, partitionNormalA); status.State != aistudio.AccountOrgDisabled || status.Restriction != aistudio.RestrictionAge ||
		status.Message != camoufoxnative.AgeRequirementPrefix {
		t.Fatalf("重启后应按登记的种类恢复: %+v", status)
	}
}

// TestRestrictedKindKeptOnStateSentinel 账户已按服务受限停止调度后，“账户已在这个状态、不启动 Worker”的哨兵（errWorkerOrgDisabled：
// 请求在标记之前取得租约、排队等启动名额期间账户被标记；errWorkerOrgDisabledDuringStart：启动期间被标记）经请求尝试（enterOrgDisabled）
// 或 aistudio 的请求失败回调（账户池的 ReportOrgDisabled，即 markRetryableFailure）回来时不再登记：种类、说明、登记的原文、次数与复查时间
// 都不变，不再写日志。原先按认不出种类记成组织停用，种类被改写并持久化，另写一行“账号所属组织已停用 AI Studio，停止调度”
func TestRestrictedKindKeptOnStateSentinel(t *testing.T) {
	manager, pool, requests := partitionTestManager(t, []string{partitionNormalA, partitionNormalB}, nil, 1, 2, 0, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	service := unschedulableTestService(ctx, manager, pool, requests)
	registry := withOrgDisabled(t, service, t.TempDir())
	restricted := &camoufoxnative.ServiceNotAllowedError{URL: "accounts.google.com/info/servicerestricted", Kind: camoufoxnative.RestrictionService}
	launches := &countingLaunch{failing: map[string]error{partitionNormalA: restricted}, launches: map[string]int{}}
	manager.launch = launches.launch
	if _, err := manager.promote(ctx, partitionNormalA, testRuntimeModel); !aistudio.OrgDisabledFailure(err) {
		t.Fatalf("启动应以拒绝账号失败: %v", err)
	}
	before, listed := registry.entryOf(partitionNormalA)
	if status := testStatus(t, pool, partitionNormalA); !listed || status.Restriction != aistudio.RestrictionService {
		t.Fatalf("前提：账户应按服务受限登记: %+v %+v", status, before)
	}
	// 再次启动：账户已标记，返回不带种类的哨兵（与请求尝试里重开 Worker 拿到的相同）
	_, again := manager.startAuthenticatedWorker(ctx, partitionNormalA, testRuntimeModel)
	if !aistudio.OrgDisabledFailure(again) || !errors.Is(again, errWorkerOrgDisabled) {
		t.Fatalf("账户已标记时应返回 errWorkerOrgDisabled: %v", again)
	}
	time.Sleep(2 * time.Millisecond)
	service.enterOrgDisabled(partitionNormalA, partitionNormalA, aistudio.OrgDisabledFromRequest, again)
	pool.ReportOrgDisabled(partitionNormalA, aistudio.OrgDisabledFromRequest, again)
	service.enterOrgDisabled(partitionNormalA, partitionNormalA, aistudio.OrgDisabledFromRequest,
		&accountWorkerInitError{err: errWorkerOrgDisabledDuringStart})
	status := testStatus(t, pool, partitionNormalA)
	if status.State != aistudio.AccountOrgDisabled || status.Restriction != aistudio.RestrictionService ||
		status.Message != camoufoxnative.ServiceRestrictedPrefix {
		t.Fatalf("种类不应被哨兵改写: %+v", status)
	}
	if after, _ := registry.entryOf(partitionNormalA); after != before {
		t.Fatalf("登记不应被哨兵改写:\n前 %+v\n后 %+v", before, after)
	}
	if orgDisabledLogs(requests, partitionNormalA) != 0 || restrictedLogs(requests, partitionNormalA, camoufoxnative.ServiceRestrictedPrefix) != 1 {
		t.Fatalf("不应再写日志: %v", serviceLogMessages(requests))
	}
	if reason := pool.UnschedulableSet([]string{partitionNormalA})[partitionNormalA]; reason != aistudio.UnschedulableServiceRestricted {
		t.Fatalf("不可调度原因 = %q", reason)
	}
	// 带种类的新证据照旧更新（接口返回 Account not supported.）
	service.enterOrgDisabled(partitionNormalA, partitionNormalA, aistudio.OrgDisabledFromRequest,
		&aistudio.RPCError{Method: "GenerateContent", StatusCode: http.StatusForbidden, Code: 7, Message: "Account not supported."})
	if status = testStatus(t, pool, partitionNormalA); status.Restriction != aistudio.RestrictionAge {
		t.Fatalf("带种类的证据应照旧更新: %+v", status)
	}
}

// TestRestrictedVerify 管理页验证：页面停在 Workspace 说明页（浏览器验证返回种类）时按 Workspace 未开放进入状态；浏览器停在 AI Studio、
// 接口复核返回 “Account not supported.” 时按年龄或资格进入状态；都不标需要登录，验证结果的原因为种类的说明
func TestRestrictedVerify(t *testing.T) {
	isolateDisableNotes(t)
	t.Run("页面停在 Workspace 说明页", func(t *testing.T) {
		admin, _, accounts := enableTestAdmin(t, "verify-workspace@example.com")
		withOrgDisabled(t, admin.service, t.TempDir())
		admin.login = &verifyTestLogin{verification: aistudio.LoginVerification{
			Reason: camoufoxnative.WorkspaceDisabledPrefix, PageURL: "aistudio.google.com/docs/workspace", OrgDisabled: true,
			Restriction: aistudio.RestrictionWorkspace,
		}}
		admin.service.service = newProbeTestService(nil)
		admin.service.authRecheck = newAuthRecheck(admin.service)
		account, err := admin.VerifyAccount(context.Background(), accounts[0].ID)
		if err != nil {
			t.Fatal(err)
		}
		if account.Authenticated == nil || *account.Authenticated || account.VerifyReason != camoufoxnative.WorkspaceDisabledPrefix ||
			account.State != string(aistudio.AccountOrgDisabled) || account.Restriction != string(aistudio.RestrictionWorkspace) {
			t.Fatalf("返回 = %+v，期望未通过、Workspace 未开放", account)
		}
		if hasLogContaining(admin.requests, "账号需要登录") ||
			!hasLogContaining(admin.requests, camoufoxnative.WorkspaceDisabledPrefix+"，停止调度 | 来源=验证") ||
			!hasLogContaining(admin.requests, "账户验证完成 | 已认证=false", "原因="+camoufoxnative.WorkspaceDisabledPrefix) {
			t.Fatalf("日志 = %v", serviceLogMessages(admin.requests))
		}
	})
	t.Run("接口返回 Account not supported", func(t *testing.T) {
		admin, _, accounts := enableTestAdmin(t, "verify-age@example.com")
		withOrgDisabled(t, admin.service, t.TempDir())
		admin.login = &verifyTestLogin{verification: aistudio.LoginVerification{Authenticated: true}}
		prober := newProbeTestService(nil)
		prober.set(accounts[0].ID, &aistudio.RPCError{Method: "GetAiStudioBenefitTier", StatusCode: http.StatusForbidden, Code: 7, Message: "Account not supported."})
		admin.service.service = prober
		admin.service.authRecheck = newAuthRecheck(admin.service)
		account, err := admin.VerifyAccount(context.Background(), accounts[0].ID)
		if err != nil {
			t.Fatal(err)
		}
		if account.Authenticated == nil || *account.Authenticated || account.Restriction != string(aistudio.RestrictionAge) ||
			account.VerifyReason != camoufoxnative.AgeRequirementPrefix {
			t.Fatalf("返回 = %+v，期望年龄或资格", account)
		}
		if hasLogContaining(admin.requests, "账号需要登录") || !hasLogContaining(admin.requests, "账户验证完成 | 已认证=false", "接口复核=组织停用") {
			t.Fatalf("日志 = %v", serviceLogMessages(admin.requests))
		}
	})
}

// regionUpstream 为生成测试的假上游：region 里的账户返回 “Region not supported.” 的 403，其余返回 hello
type regionUpstream struct {
	forbiddenUpstream
	regionMu sync.Mutex
	region   map[string]int
}

func (upstream *regionUpstream) Generate(ctx context.Context, request aistudio.GenerateRequest) (<-chan aistudio.Event, error) {
	upstream.regionMu.Lock()
	_, blocked := upstream.region[request.AccountID]
	if blocked {
		upstream.region[request.AccountID]++
	}
	upstream.regionMu.Unlock()
	if blocked {
		return nil, &aistudio.RPCError{Method: "GenerateContent", StatusCode: http.StatusForbidden, Code: 7, Message: "Region not supported."}
	}
	return upstream.forbiddenUpstream.Generate(ctx, request)
}

// TestRegionNotSupportedNotMarked 生成请求返回 “Region not supported.”（AI Studio 不支持当前出口地区）：出口的问题，换号完成；
// 账户不进组织停用状态、不按单模型 403 冷却、不登记为 403 无权限账户、不自动暂停，写一行进度说明该查出口
func TestRegionNotSupportedNotMarked(t *testing.T) {
	const regionID, goodID = "a-region@example.com", "b-good@example.com"
	upstream := &regionUpstream{forbiddenUpstream: forbiddenUpstream{denied: map[string]bool{}}, region: map[string]int{regionID: 0}}
	service, pool := primaryBusyService(t, upstream, regionID, goodID)
	denied := withPermissionDenied(t, service)
	withOrgDisabled(t, service, t.TempDir())
	preverifyRuntimeModelAccess(t, pool, goodID)
	for index := range 4 {
		text, err, _ := runAccountRequest(t, service, "req-region-"+string(rune('a'+index)), "")
		if err != nil || text != "hello" {
			t.Fatalf("第 %d 个请求应换到正常账户完成: text=%q err=%v", index+1, text, err)
		}
	}
	upstream.regionMu.Lock()
	calls := upstream.region[regionID]
	upstream.regionMu.Unlock()
	if calls == 0 {
		t.Fatal("测试前提：地区不受支持的账户至少被请求过一次")
	}
	status := testStatus(t, pool, regionID)
	if pool.OrgDisabled(regionID) || status.State == aistudio.AccountOrgDisabled || len(status.Cooldowns) != 0 {
		t.Fatalf("不应标记账户或写冷却: %+v", status)
	}
	if entries := denied.list(); len(entries) != 0 {
		t.Fatalf("不应登记为 403 无权限账户: %+v", entries)
	}
	if hasLogContaining(service.requests, "账号冷却 | 类型=无权限") || hasLogContaining(service.requests, "账号自动暂停") ||
		hasLogContaining(service.requests, "停止调度") {
		t.Fatalf("不应按 403 或拒绝账号处理: %v", serviceLogMessages(service.requests))
	}
	if !hasLogContaining(service.requests, "AI Studio 不支持当前出口地区（Region not supported），检查该账户或全局代理的出口") {
		t.Fatalf("应写一行出口地区的说明: %v", serviceLogMessages(service.requests))
	}
	if isPermissionDenied(&aistudio.RPCError{Method: "GenerateContent", StatusCode: http.StatusForbidden, Code: 7, Message: "Region not supported."}) ||
		isPermissionDenied(&aistudio.RPCError{Method: "GenerateContent", StatusCode: http.StatusForbidden, Code: 7, Message: "Early Access Apps disabled."}) {
		t.Fatal("地区与 Workspace 未开放的 403 不应算 403 无权限")
	}
}
