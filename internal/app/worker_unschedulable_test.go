package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/api"
	"github.com/Mag1cFall/AIStudio2API/internal/camoufoxnative"
)

// authFailureError 构造上游确认登录失效的错误（HTTP 401、协议错误码 16，用户报告的原文）。用 GenerateContent：
// GenerateAccessToken 的 401 只让附件尝试失败并短冷却，不算登录失效（见 aistudio.AccessTokenAuthFailure）
func authFailureError() error {
	return &aistudio.RPCError{
		Method: "GenerateContent", StatusCode: http.StatusUnauthorized, Code: 16,
		Message: "Request is missing required authentication credential.\nExpected OAuth 2 access token, login cookie or other valid authentication credential.",
	}
}

// unschedulableTestService 在 retireTestService 之上像 newTrackedService 一样挂上“账号需要登录”回调
func unschedulableTestService(ctx context.Context, manager *accountWorkerManager, pool *aistudio.AccountPool, requests *requestRegistry) *trackedService {
	service := retireTestService(ctx, manager, pool, requests)
	note := authRequiredHook(service.noteAuthRequired)
	manager.authRequired.Store(&note)
	return service
}

// markAuthRequiredViaTransport 让持有 lease 的一次请求经认证续签传输（生成请求的 DoProtected 与普通 RPC 共用）得到 401：
// 账户没有续签材料，续签器直接把它标为需要登录，与线上生成请求 401 的路径相同
func markAuthRequiredViaTransport(t *testing.T, refresher *authRuntimeRefresher, lease *aistudio.AccountLease) {
	t.Helper()
	ctx := aistudio.ContextWithAccountLease(context.Background(), lease)
	_, err := refresher.do(ctx, "GenerateContent", func() (*aistudio.RPCResponse, error) {
		return unauthorizedResponse(loginExpiredBody), nil
	})
	if !aistudio.DefinitiveAuthenticationFailure(err) {
		t.Errorf("续签传输应返回登录失效，实际 %v", err)
	}
}

// durableMessages 取出没有写入协程的注册表（newConsoleTestRegistry）里经 logDurable 排进独立队列的日志
func durableMessages(registry *requestRegistry) []api.AdminLog {
	var entries []api.AdminLog
	for {
		select {
		case entry := <-registry.consoleDurable:
			entries = append(entries, entry)
		default:
			return entries
		}
	}
}

// sourceLogCount 统计运行日志中来源为 source、以 prefix 开头的记录数
func sourceLogCount(requests *requestRegistry, source string, prefix string) int {
	requests.mu.Lock()
	defer requests.mu.Unlock()
	count := 0
	for _, entry := range requests.logs {
		if entry.Source == source && strings.HasPrefix(entry.Message, prefix) {
			count++
		}
	}
	return count
}

// TestAuthRequiredWorkerReleasedImmediately 生成请求 401（经认证续签传输确认需要登录）：请求还在进行时 Worker 不关，
// 租约释放后一轮淘汰内关闭，写“WAA Worker 释放 | 原因=需要登录”（不是冷却淘汰）与“账号需要登录”，并由预热补位
func TestAuthRequiredWorkerReleasedImmediately(t *testing.T) {
	manager, pool, requests := partitionTestManager(t, []string{partitionNormalA, partitionNormalB}, nil, 1, 2, 0, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	unschedulableTestService(ctx, manager, pool, requests)
	if _, err := manager.promote(ctx, partitionNormalA, testRuntimeModel); err != nil {
		t.Fatal(err)
	}
	refresher := &authRuntimeRefresher{requests: requests, workers: manager}
	lease, err := pool.AcquireFor(ctx, aistudio.AccountSelection{AccountID: partitionNormalA, ModelID: testRuntimeModel})
	if err != nil {
		t.Fatal(err)
	}
	markAuthRequiredViaTransport(t, refresher, lease)
	if got := manager.retirementCount(); got != 1 {
		t.Fatalf("确认需要登录后应立即登记释放，登记数 = %d", got)
	}
	time.Sleep(300 * time.Millisecond)
	if !slices.Contains(manager.WarmAccountIDs(), partitionNormalA) {
		t.Fatal("请求还在进行时不应关闭 Worker")
	}
	releasedAt := time.Now()
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
	if !waitUntil(3*time.Second, func() bool { return !slices.Contains(manager.WarmAccountIDs(), partitionNormalA) }) {
		t.Fatalf("租约释放后应关闭需要登录账号的 Worker，驻留 Worker = %v", manager.WarmAccountIDs())
	}
	if waited := time.Since(releasedAt); waited >= 2*time.Second {
		t.Fatalf("租约释放后 %s 才关闭，期望一轮淘汰内关闭", waited)
	}
	if !waitUntil(time.Second, func() bool { return hasLogContaining(requests, "WAA Worker 释放 | 原因=需要登录 | 登记后=") }) {
		t.Fatalf("应记录释放日志，日志 = %v", serviceLogMessages(requests))
	}
	if hasLogContaining(requests, "冷却淘汰") || hasLogContaining(requests, "替补") {
		t.Fatalf("不可调度释放不写冷却淘汰、不启替补，日志 = %v", serviceLogMessages(requests))
	}
	if !hasLogContaining(requests, "账号需要登录 | 来源=续签重放 | 出口=直连 | 出口限流中=否 | 错误=") {
		t.Fatalf("应记录账号需要登录，日志 = %v", serviceLogMessages(requests))
	}
	// 预热补位：B 接替（A 需要登录，不再预热）
	if !waitUntil(5*time.Second, func() bool { return slices.Contains(manager.WarmAccountIDs(), partitionNormalB) }) {
		t.Fatalf("释放后应由预热补位，驻留 Worker = %v", manager.WarmAccountIDs())
	}
	if !waitUntil(time.Second, func() bool { return manager.retirementCount() == 0 }) {
		t.Fatalf("关闭后登记应清除，登记数 = %d", manager.retirementCount())
	}
}

// TestAuthRequiredBusyWorkerReleasedAfterRequest 账号还有其他进行中的请求：不排空、不要求可接替账户（分区里只有这一个账户，
// 冷却登记会因为没有可接替账户推迟），等最后一个租约释放后关闭
func TestAuthRequiredBusyWorkerReleasedAfterRequest(t *testing.T) {
	manager, pool, requests := partitionTestManager(t, []string{partitionNormalA}, nil, 1, 1, 0, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	unschedulableTestService(ctx, manager, pool, requests)
	if _, err := manager.promote(ctx, partitionNormalA, testRuntimeModel); err != nil {
		t.Fatal(err)
	}
	releaseOther := holdLease(t, pool, partitionNormalA)
	defer releaseOther()
	lease, err := pool.AcquireFor(ctx, aistudio.AccountSelection{AccountID: partitionNormalA, ModelID: testRuntimeModel})
	if err != nil {
		t.Fatal(err)
	}
	markAuthRequiredViaTransport(t, &authRuntimeRefresher{requests: requests, workers: manager}, lease)
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(600 * time.Millisecond)
	if !slices.Contains(manager.WarmAccountIDs(), partitionNormalA) {
		t.Fatal("账号仍在服务其他请求时不应关闭 Worker")
	}
	if pool.Draining(partitionNormalA) || manager.drainCount(aistudio.PoolScopeNormal) != 0 || hasLogContaining(requests, "排空") {
		t.Fatal("不可调度登记不排空")
	}
	if got := manager.retirementCount(); got != 1 {
		t.Fatalf("忙碌期间登记应保留，登记数 = %d", got)
	}
	releaseOther()
	if !waitUntil(3*time.Second, func() bool { return !slices.Contains(manager.WarmAccountIDs(), partitionNormalA) }) {
		t.Fatalf("最后一个租约释放后应关闭 Worker（没有可接替账户也关），驻留 Worker = %v", manager.WarmAccountIDs())
	}
	if !waitUntil(time.Second, func() bool { return hasLogContaining(requests, "WAA Worker 释放 | 原因=需要登录") }) {
		t.Fatalf("应记录释放日志，日志 = %v", serviceLogMessages(requests))
	}
}

// TestUnschedulableSkipsDrainTimeout 已在排空（冷却登记）的账号变成需要登录：排空超时分支不再处理它（不清除排空、不推迟 1 分钟），
// 请求结束后下一轮即按释放关闭。补登记与淘汰循环按本轮状态识别两条路径都成立
func TestUnschedulableSkipsDrainTimeout(t *testing.T) {
	accounts := []string{"drain-a@example.com", "drain-b@example.com", "drain-c@example.com"}
	for _, register := range []bool{true, false} {
		t.Run(fmt.Sprintf("补登记=%t", register), func(t *testing.T) {
			env := newDrainEnv(t, accounts, 2, 2)
			env.manager.requestTimeout.Store(int64(time.Second))
			env.promote(t, accounts[:2]...)
			release := env.startBusyDrain(t, accounts[0])
			defer release()
			drain := env.manager.drainOf(accounts[0])
			if drain == nil {
				t.Fatal("应有排空状态")
			}
			if err := env.pool.MarkAuthRequired(accounts[0], "HTTP 401"); err != nil {
				t.Fatal(err)
			}
			if register {
				// 淘汰循环可能已按本轮状态先改了登记（这时补登记不再计新增），下面按登记本身核对
				if added := env.service.registerUnschedulableWorkers(); added > 1 {
					t.Fatalf("只有一个账号变成不可调度，新增 = %d", added)
				}
			}
			drain.startedAt.Store(time.Now().Add(-env.manager.drainTimeout() - time.Second).UnixNano())
			env.manager.wakeRetire()
			time.Sleep(600 * time.Millisecond)
			if hasLogContaining(env.requests, "排空超时") {
				t.Fatalf("不可调度的账号不走排空超时，日志 = %v", serviceLogMessages(env.requests))
			}
			env.manager.retireMu.Lock()
			entry := env.manager.retirements[accounts[0]]
			var retryAt time.Time
			var reason string
			if entry != nil {
				retryAt, reason = entry.retryAt, entry.unschedulable
			}
			env.manager.retireMu.Unlock()
			if entry == nil || !retryAt.IsZero() || reason != unschedulableAuthRequired {
				t.Fatalf("登记应改为不可调度且不推迟: 存在=%v 重试时间=%v 原因=%q", entry != nil, retryAt, reason)
			}
			if !slices.Contains(env.manager.WarmAccountIDs(), accounts[0]) {
				t.Fatal("请求还在进行时不应关闭")
			}
			releasedAt := time.Now()
			release()
			if !waitUntil(3*time.Second, func() bool { return !slices.Contains(env.manager.WarmAccountIDs(), accounts[0]) }) {
				t.Fatalf("请求结束后应关闭，驻留 Worker = %v", sortedWarm(env.manager))
			}
			if waited := time.Since(releasedAt); waited >= 2*time.Second {
				t.Fatalf("请求结束 %s 后才关闭，期望下一轮即关闭", waited)
			}
			if !waitUntil(time.Second, func() bool { return hasLogContaining(env.requests, "WAA Worker 释放 | 原因=需要登录") }) {
				t.Fatalf("应记录释放日志，日志 = %v", serviceLogMessages(env.requests))
			}
			if hasLogContaining(env.requests, "WAA Worker 冷却淘汰") || env.pool.Draining(accounts[0]) {
				t.Fatal("应按释放关闭（不写冷却淘汰），关闭后清除排空")
			}
		})
	}
}

// TestUnschedulableSweepCatchesOtherPaths 其他路径造成的不可调度（aistudio 内部的 401 直接在账户池标需要登录、管理页停用、
// 目录被同步成空）不经调度层：keepWarm 的补登记按原因汇总写持久行“WAA Worker 补登记释放”，随后释放；已登记的不重复计
func TestUnschedulableSweepCatchesOtherPaths(t *testing.T) {
	a, b, c := "sweep-a@example.com", "sweep-b@example.com", "sweep-c@example.com"
	manager, pool, requests := partitionTestManager(t, []string{a, b, c}, nil, 3, 3, 0, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	service := unschedulableTestService(ctx, manager, pool, requests)
	durable, _ := newConsoleTestRegistry(consoleQueueSize, consoleDurableQueueSize)
	service.requests = durable
	for _, accountID := range []string{a, b, c} {
		if _, err := manager.promote(ctx, accountID, testRuntimeModel); err != nil {
			t.Fatal(err)
		}
	}
	// 释放过程中三个账号都不能空闲，补登记前不关
	releases := []func(){holdLease(t, pool, a), holdLease(t, pool, b), holdLease(t, pool, c)}
	if err := pool.MarkAuthRequired(a, "HTTP 401"); err != nil {
		t.Fatal(err)
	}
	// 停用要取独占租约：先放开 B（停用后它空闲，补登记后立即释放）
	releases[1]()
	disableTestAccount(t, pool, b)
	if err := pool.SetCatalog(c, aistudio.BenefitTierFree, nil); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	if got := manager.retirementCount(); got != 0 || len(manager.WarmAccountIDs()) != 3 {
		t.Fatalf("这些路径不经调度层，补登记前不应有登记: 登记数 = %d，驻留 Worker = %v", got, manager.WarmAccountIDs())
	}
	service.keepWarmPass(ctx, time.Now())
	var found bool
	for _, entry := range durableMessages(durable) {
		if entry.Message == "WAA Worker 补登记释放 | 新增=3 | 驻留中不可调度=3/3 | 原因=需要登录 1、已停用 1、没有模型目录 1" {
			found = true
		}
	}
	if !found {
		t.Fatalf("补登记应写持久行，日志 = %v", serviceLogMessages(durable))
	}
	if added := service.registerUnschedulableWorkers(); added != 0 {
		t.Fatalf("已登记的不应重复计，新增 = %d", added)
	}
	releases[0]()
	releases[2]()
	if !waitUntil(6*time.Second, func() bool { return len(manager.WarmAccountIDs()) == 0 }) {
		t.Fatalf("补登记后应释放全部不可调度的 Worker，驻留 Worker = %v", manager.WarmAccountIDs())
	}
	for accountID, reason := range map[string]string{a: "需要登录", b: "已停用", c: "没有模型目录"} {
		if !waitUntil(time.Second, func() bool {
			return sourceLogCount(requests, accountID, "WAA Worker 释放 | 原因="+reason+" |") == 1
		}) {
			t.Fatalf("%s 应按原因 %s 释放一次，日志 = %v", accountID, reason, serviceLogMessages(requests))
		}
	}
}

// TestOnDemandReplaceEvictsUnschedulableWhileOpening 分区已满、另有 Worker 正在启动时按需替换只淘汰冷却中的空闲 Worker：
// 需要登录的僵尸 Worker 同样可选（诊断里 88–99% 的时间有 Worker 在启动，按需替换原先换不掉它），健康的不动
func TestOnDemandReplaceEvictsUnschedulableWhileOpening(t *testing.T) {
	a, b, c, d := "open-a@example.com", "open-b@example.com", "open-c@example.com", "open-d@example.com"
	manager, pool, _ := partitionTestManager(t, []string{a, b, c, d}, nil, 2, 3, 0, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for _, accountID := range []string{a, b} {
		if _, err := manager.promote(ctx, accountID, testRuntimeModel); err != nil {
			t.Fatal(err)
		}
	}
	launch := manager.launch
	started, unblock := make(chan struct{}), make(chan struct{})
	manager.launch = func(launchCtx context.Context, accountID string, options camoufoxnative.Options) (*aistudio.NativeWorker, error) {
		if accountID == c {
			close(started)
			select {
			case <-unblock:
			case <-launchCtx.Done():
				return nil, launchCtx.Err()
			}
		}
		return launch(launchCtx, accountID, options)
	}
	opened := make(chan error, 1)
	go func() {
		_, err := manager.promote(ctx, c, testRuntimeModel)
		opened <- err
	}()
	<-started
	defer func() {
		close(unblock)
		if err := <-opened; err != nil {
			t.Errorf("C 的启动应成功: %v", err)
		}
	}()
	if err := pool.MarkAuthRequired(a, "HTTP 401"); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.promote(ctx, d, testRuntimeModel); err != nil {
		t.Fatalf("同分区有 Worker 启动中时按需替换应能淘汰需要登录的 Worker: %v", err)
	}
	warm := manager.WarmAccountIDs()
	if slices.Contains(warm, a) || !slices.Contains(warm, b) || !slices.Contains(warm, d) {
		t.Fatalf("应淘汰需要登录的 A、保留健康的 B，驻留 Worker = %v", warm)
	}
}

// TestReclaimDeadIncludesUnschedulable 请求现场回收（victimDead）与空闲回收也接受不可调度的 Worker，并排在死槽位之前；健康的不回收
func TestReclaimDeadIncludesUnschedulable(t *testing.T) {
	a, b, c := "reclaim-a@example.com", "reclaim-b@example.com", "reclaim-c@example.com"
	t.Run("现场回收", func(t *testing.T) {
		manager, pool, _ := partitionTestManager(t, []string{a, b, c}, nil, 3, 3, 0, 1)
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		for _, accountID := range []string{a, b, c} {
			if _, err := manager.promote(ctx, accountID, testRuntimeModel); err != nil {
				t.Fatal(err)
			}
		}
		coolGlobally(t, pool, a)
		if err := pool.MarkAuthRequired(b, "HTTP 401"); err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{b, a, ""} {
			victim, err := manager.reclaimDeadWorker(ctx, aistudio.PoolScopeNormal, testRuntimeModel)
			if err != nil || victim != want {
				t.Fatalf("回收顺序应为不可调度、死槽位、不回收健康的：victim=%q err=%v，期望 %q", victim, err, want)
			}
		}
		if got := sortedWarm(manager); !slices.Equal(got, []string{c}) {
			t.Fatalf("应只留下健康的 C，驻留 Worker = %v", got)
		}
	})
	t.Run("空闲回收", func(t *testing.T) {
		manager, pool, _ := partitionTestManager(t, []string{a, b, c}, nil, 1, 3, 0, 1)
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		for _, accountID := range []string{a, b, c} {
			if _, err := manager.promote(ctx, accountID, testRuntimeModel); err != nil {
				t.Fatal(err)
			}
		}
		coolGlobally(t, pool, a)
		if err := pool.SetCatalog(b, aistudio.BenefitTierFree, nil); err != nil {
			t.Fatal(err)
		}
		if !manager.reapIdleWorker(time.Now().Add(time.Second)) {
			t.Fatal("超出常驻数时应回收一个空闲 Worker")
		}
		if got := sortedWarm(manager); slices.Contains(got, b) || !slices.Contains(got, a) {
			t.Fatalf("空闲回收应先关没有模型目录的 B，驻留 Worker = %v", got)
		}
	})
}

// TestUnschedulableRecoveredDropsRetirement 账号恢复可调度（验证通过）：只有不可调度登记的放弃，Worker 保留；
// 原先另有冷却登记的改回冷却登记，照常按冷却核对（这里没有可接替账户，保留登记、推迟再试）
func TestUnschedulableRecoveredDropsRetirement(t *testing.T) {
	t.Run("只有不可调度登记", func(t *testing.T) {
		manager, pool, requests := partitionTestManager(t, []string{partitionNormalA, partitionNormalB}, nil, 1, 2, 0, 1)
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		unschedulableTestService(ctx, manager, pool, requests)
		if _, err := manager.promote(ctx, partitionNormalA, testRuntimeModel); err != nil {
			t.Fatal(err)
		}
		release := holdLease(t, pool, partitionNormalA)
		defer release()
		if err := pool.MarkAuthRequired(partitionNormalA, "HTTP 401"); err != nil {
			t.Fatal(err)
		}
		manager.releaseUnschedulable(partitionNormalA, unschedulableAuthRequired)
		if got := manager.retirementCount(); got != 1 {
			t.Fatalf("应登记释放，登记数 = %d", got)
		}
		if err := pool.MarkReady(partitionNormalA); err != nil {
			t.Fatal(err)
		}
		if !waitUntil(3*time.Second, func() bool { return manager.retirementCount() == 0 }) {
			t.Fatalf("恢复可调度后登记应放弃，登记数 = %d", manager.retirementCount())
		}
		release()
		time.Sleep(300 * time.Millisecond)
		if !slices.Contains(manager.WarmAccountIDs(), partitionNormalA) || hasLogContaining(requests, "WAA Worker 释放") {
			t.Fatal("恢复可调度后不应关闭 Worker")
		}
	})
	t.Run("原先另有冷却登记", func(t *testing.T) {
		manager, pool, requests := partitionTestManager(t, []string{partitionNormalA}, nil, 1, 1, 0, 1)
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		unschedulableTestService(ctx, manager, pool, requests)
		if _, err := manager.promote(ctx, partitionNormalA, testRuntimeModel); err != nil {
			t.Fatal(err)
		}
		release := holdLease(t, pool, partitionNormalA)
		defer release()
		coolGlobally(t, pool, partitionNormalA)
		if !manager.retireWorker(partitionNormalA, "") {
			t.Fatal("全局冷却应登记淘汰")
		}
		deferred := func() bool {
			manager.retireMu.Lock()
			defer manager.retireMu.Unlock()
			entry := manager.retirements[partitionNormalA]
			return entry != nil && !entry.retryAt.IsZero()
		}
		// 先等冷却登记因没有可接替账户被推迟：推迟期间淘汰循环不处理它，改为不可调度登记只经下面的登记
		if !waitUntil(3*time.Second, deferred) {
			t.Fatal("没有可接替账户时冷却登记应推迟再试")
		}
		if err := pool.MarkAuthRequired(partitionNormalA, "HTTP 401"); err != nil {
			t.Fatal(err)
		}
		if registered, added := manager.registerUnschedulable(partitionNormalA, unschedulableAuthRequired); !registered || !added {
			t.Fatalf("冷却登记应改为不可调度登记: registered=%t added=%t", registered, added)
		}
		if err := pool.MarkReady(partitionNormalA); err != nil {
			t.Fatal(err)
		}
		if !waitUntil(3*time.Second, func() bool {
			manager.retireMu.Lock()
			defer manager.retireMu.Unlock()
			entry := manager.retirements[partitionNormalA]
			return entry != nil && entry.unschedulable == "" && entry.global() && !entry.retryAt.IsZero()
		}) {
			t.Fatal("恢复可调度后应改回全局冷却登记，并因没有可接替账户推迟再试")
		}
		release()
		time.Sleep(300 * time.Millisecond)
		if !slices.Contains(manager.WarmAccountIDs(), partitionNormalA) {
			t.Fatal("没有可接替账户时冷却登记不关闭 Worker")
		}
	})
}

// TestUnschedulableCountsInStatus 状态接口按分区给出驻留 Worker 中不可调度的数量，与死槽位分开计；没有时为 0（JSON 照常带字段）
func TestUnschedulableCountsInStatus(t *testing.T) {
	manager, pool, requests := partitionTestManager(t, []string{partitionNormalA, partitionNormalB}, []string{partitionUltraA}, 2, 4, 1, 3)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for _, accountID := range []string{partitionNormalA, partitionNormalB, partitionUltraA} {
		if _, err := manager.promote(ctx, accountID, testRuntimeModel); err != nil {
			t.Fatal(err)
		}
	}
	service := &trackedService{lifecycle: ctx, pool: pool, requests: requests, workers: manager}
	admin := &runtimeAdmin{lifecycle: ctx, pool: pool, service: service, requests: requests, workers: manager}
	status, err := admin.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if status.Workers.Unschedulable != 0 || status.UltraWorkers.Unschedulable != 0 {
		t.Fatalf("没有不可调度时应为 0: %+v / %+v", status.Workers, status.UltraWorkers)
	}
	encoded, err := json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"unschedulable":0`) {
		t.Fatalf("状态 JSON 应带 unschedulable 字段: %s", encoded)
	}
	if err := pool.MarkAuthRequired(partitionNormalA, "HTTP 401"); err != nil {
		t.Fatal(err)
	}
	if err := pool.SetCatalog(partitionNormalB, aistudio.BenefitTierFree, nil); err != nil {
		t.Fatal(err)
	}
	if err := pool.MarkAuthRequired(partitionUltraA, "HTTP 401"); err != nil {
		t.Fatal(err)
	}
	if status, err = admin.Status(ctx); err != nil {
		t.Fatal(err)
	}
	if status.Workers.Unschedulable != 2 || status.Workers.Dead != 0 || status.Workers.Warm != 2 {
		t.Fatalf("普通分区应有 2 个不可调度、死槽位不变: %+v", status.Workers)
	}
	if status.UltraWorkers.Unschedulable != 1 || status.UltraWorkers.Dead != 0 {
		t.Fatalf("Ultra 分区应有 1 个不可调度: %+v", status.UltraWorkers)
	}
}

// failingCatalog 为每次刷新都返回上游 429 的模型目录服务
type failingCatalog struct {
	mu    sync.Mutex
	calls int
}

func (catalog *failingCatalog) RefreshAccountModels(context.Context, string) ([]aistudio.Model, error) {
	catalog.mu.Lock()
	catalog.calls++
	catalog.mu.Unlock()
	return nil, &aistudio.RPCError{Method: "ListModels", StatusCode: http.StatusTooManyRequests, Code: 8, Message: "Quota exceeded"}
}

func (catalog *failingCatalog) CachedModels() []aistudio.Model { return nil }

func (catalog *failingCatalog) callCount() int {
	catalog.mu.Lock()
	defer catalog.mu.Unlock()
	return catalog.calls
}

// keepCatalogAdmin 创建 enableTestAdmin 的管理端，Worker 用不启动浏览器的实现，目录同步一律失败（上游 429）
func keepCatalogAdmin(t *testing.T, accountIDs ...string) (*runtimeAdmin, *failingCatalog, []*aistudio.Account) {
	t.Helper()
	admin, _, accounts := enableTestAdmin(t, accountIDs...)
	admin.workers.launch = func(_ context.Context, accountID string, _ camoufoxnative.Options) (*aistudio.NativeWorker, error) {
		worker, _ := aistudio.NewStubWorker(accountID, 0)
		return worker, nil
	}
	catalog := &failingCatalog{}
	admin.service.catalog = catalog
	return admin, catalog, accounts
}

// assertCatalogCandidate 断言账户就绪、目录保留，是不固定账户生成请求的候选，且不算不可调度
func assertCatalogCandidate(t *testing.T, admin *runtimeAdmin, accountID string) {
	t.Helper()
	status := testStatus(t, admin.pool, accountID)
	if status.State != aistudio.AccountReady || len(status.Models) == 0 {
		t.Fatalf("账户应就绪且保留目录: state=%s 模型=%v", status.State, status.Models)
	}
	if reasons := admin.pool.UnschedulableSet([]string{accountID}); len(reasons) != 0 {
		t.Fatalf("账户不应不可调度: %v", reasons)
	}
	lease, _, err := admin.pool.TryAcquireFor(context.Background(), aistudio.AccountSelection{
		ModelID: testRuntimeModel, Method: "generateContent", AllowedAccountIDs: []string{accountID},
	})
	if err != nil || lease == nil {
		t.Fatalf("账户应是不固定账户请求的候选: lease=%v err=%v", lease, err)
	}
	_ = lease.Release()
}

// TestVerifyLoginKeepsCatalog 管理页验证通过：保留目录与驻留 Worker，账号立刻是候选；随后的目录同步 429 失败时目录仍在。
// 验证判定登录失效时登记释放 Worker，写“账号需要登录 | 来源=管理验证”
func TestVerifyLoginKeepsCatalog(t *testing.T) {
	t.Run("验证通过", func(t *testing.T) {
		admin, catalog, accounts := keepCatalogAdmin(t, "verify-a@example.com")
		accountID := accounts[0].ID
		admin.login = &verifyTestLogin{verification: aistudio.LoginVerification{Authenticated: true}}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := admin.workers.promote(ctx, accountID, testRuntimeModel); err != nil {
			t.Fatal(err)
		}
		// 验证前账号被生成 401 标成需要登录（“验证成功仍显示需要登录”的场景）
		if err := admin.pool.MarkAuthRequired(accountID, "HTTP 401"); err != nil {
			t.Fatal(err)
		}
		if _, err := admin.VerifyAccount(ctx, accountID); err != nil {
			t.Fatal(err)
		}
		if catalog.callCount() != 1 {
			t.Fatalf("验证通过后应同步一次目录，实际 %d 次", catalog.callCount())
		}
		assertCatalogCandidate(t, admin, accountID)
		time.Sleep(300 * time.Millisecond)
		if !slices.Contains(admin.workers.WarmAccountIDs(), accountID) {
			t.Fatal("验证通过后应保留驻留 Worker")
		}
	})
	t.Run("验证失败", func(t *testing.T) {
		admin, _, accounts := keepCatalogAdmin(t, "verify-b@example.com")
		accountID := accounts[0].ID
		admin.login = &verifyTestLogin{verification: aistudio.LoginVerification{Reason: "跳转到登录页"}}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := admin.workers.promote(ctx, accountID, testRuntimeModel); err != nil {
			t.Fatal(err)
		}
		if _, err := admin.VerifyAccount(ctx, accountID); err != nil {
			t.Fatal(err)
		}
		if !waitUntil(3*time.Second, func() bool { return !slices.Contains(admin.workers.WarmAccountIDs(), accountID) }) {
			t.Fatalf("验证判定登录失效后应释放 Worker，驻留 Worker = %v", admin.workers.WarmAccountIDs())
		}
		if !hasLogContaining(admin.requests, "账号需要登录 | 来源=管理验证 | 出口=直连 | 出口限流中=否 | 错误=跳转到登录页") {
			t.Fatalf("应记录账号需要登录，日志 = %v", serviceLogMessages(admin.requests))
		}
	})
}

// loginResultLogin 为测试用隔离浏览器：登录返回同一邮箱与可签名的认证状态
type loginResultLogin struct {
	email string
	state aistudio.StorageState
}

func (login *loginResultLogin) Login(context.Context, aistudio.IsolatedLoginRequest) (aistudio.IsolatedLoginResult, error) {
	return aistudio.IsolatedLoginResult{StorageState: login.state, Email: login.email, VerifiedAt: time.Now()}, nil
}

func (login *loginResultLogin) Verify(context.Context, aistudio.IsolatedLoginRequest, aistudio.StorageState) (aistudio.LoginVerification, error) {
	return aistudio.LoginVerification{Authenticated: true, VerifiedAt: time.Now()}, nil
}

// TestLoginKeepsCatalogUntilSync 浏览器重新登录不再清空目录：随后的同步 429 失败时旧目录仍在，账号登录后立刻可调度
func TestLoginKeepsCatalogUntilSync(t *testing.T) {
	admin, catalog, accounts := keepCatalogAdmin(t, "login-a@example.com")
	accountID := accounts[0].ID
	admin.login = &loginResultLogin{email: accountID, state: authTestState(t, "NEW", false)}
	if err := admin.pool.MarkAuthRequired(accountID, "HTTP 401"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := admin.LoginAccount(ctx, accountID); err != nil {
		t.Fatal(err)
	}
	if catalog.callCount() != 1 {
		t.Fatalf("登录后应同步一次目录，实际 %d 次", catalog.callCount())
	}
	assertCatalogCandidate(t, admin, accountID)
}

// TestAuthFileReloadKeepsCatalog 需要登录的账户换了认证文件：载入后不清空目录，同步失败时旧目录仍在、账号照常可调度
func TestAuthFileReloadKeepsCatalog(t *testing.T) {
	admin, catalog, accounts := keepCatalogAdmin(t, "reload-a@example.com")
	accountID := accounts[0].ID
	if err := admin.pool.MarkAuthRequired(accountID, "HTTP 401"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	reloaded, err := admin.reloadAccountCredentials(ctx, accountID, accounts[0].StoragePath)
	if err != nil || !reloaded {
		t.Fatalf("应载入新认证文件: reloaded=%t err=%v", reloaded, err)
	}
	if catalog.callCount() != 1 {
		t.Fatalf("载入后应同步一次目录，实际 %d 次", catalog.callCount())
	}
	assertCatalogCandidate(t, admin, accountID)
}

// TestAuthRequiredLogFields “账号需要登录”为持久行，带来源、出口标签与出口是否判定限流；同账号 1 分钟最多一条（按账号去重），
// 标记没有写入（账号仍可调度）时不写；错误压成一行、过长截断
func TestAuthRequiredLogFields(t *testing.T) {
	a, b := "note-a@example.com", "note-b@example.com"
	pool, _ := testAccountPool(t, a, b)
	registry, _ := newConsoleTestRegistry(consoleQueueSize, consoleDurableQueueSize)
	service := &trackedService{pool: pool, requests: registry}
	service.noteAuthRequired(a, "label-a", authFromGeneration, authFailureError())
	if entries := durableMessages(registry); len(entries) != 0 {
		t.Fatalf("账号仍可调度时不应写，日志 = %v", entries)
	}
	if err := pool.MarkAuthRequired(a, "HTTP 401"); err != nil {
		t.Fatal(err)
	}
	service.noteAuthRequired(a, "label-a", authFromGeneration, authFailureError())
	entries := durableMessages(registry)
	want := "账号需要登录 | 来源=生成 | 出口=直连 | 出口限流中=否 | 错误=AI Studio GenerateContent 返回 HTTP 401"
	if len(entries) != 1 || entries[0].Source != "label-a" || entries[0].Level != "WARN" || !strings.HasPrefix(entries[0].Message, want) ||
		strings.Contains(entries[0].Message, "\n") {
		t.Fatalf("应写一条持久行 %q（错误压成一行），实际 %+v", want, entries)
	}
	// 同账号 1 分钟内不再写（续签重放、生成、流内可能先后确认同一次失效）
	service.noteAuthRequired(a, "label-a", authFromStream, authFailureError())
	if entries := durableMessages(registry); len(entries) != 0 {
		t.Fatalf("同账号 1 分钟内应只写一条，日志 = %v", entries)
	}
	// 另一个账号照常写；出口判定成立时“出口限流中=是”；没有显示名时用账号 ID，没有错误时写 -
	service.egress = newEgressGuard(pool, registry)
	service.egress.forceVerdictForTest("", egressVerdict{Mode: egressLimited, Limited: true, Label: "直连"})
	if err := pool.MarkAuthRequired(b, "HTTP 401"); err != nil {
		t.Fatal(err)
	}
	service.noteAuthRequired(b, "", authFromVerify, nil)
	entries = durableMessages(registry)
	if len(entries) != 1 || entries[0].Source != b ||
		entries[0].Message != "账号需要登录 | 来源=管理验证 | 出口=直连 | 出口限流中=是 | 错误=-" {
		t.Fatalf("另一个账号应照常写，实际 %+v", entries)
	}
	// 过了 1 分钟同账号可以再写；过长的错误截断
	service.authNotes.mu.Lock()
	service.authNotes.logged[a] = time.Now().Add(-authRequiredNoteInterval - time.Second)
	service.authNotes.mu.Unlock()
	service.noteAuthRequired(a, "label-a", authFromStream, errors.New(strings.Repeat("长", 500)))
	entries = durableMessages(registry)
	if len(entries) != 1 || !strings.Contains(entries[0].Message, "来源=流内") ||
		!strings.HasSuffix(entries[0].Message, strings.Repeat("长", authRequiredCauseLimit)+"…") {
		t.Fatalf("1 分钟后应可再写并截断错误，实际 %+v", entries)
	}
}

// TestAuthRequiredSourceLabels Worker 启动阶段确认的登录失效记为“启动”：预热（没有租约）直接标记，按需启动（有租约）经认证续签器标记
func TestAuthRequiredSourceLabels(t *testing.T) {
	for _, leased := range []bool{false, true} {
		t.Run(fmt.Sprintf("租约=%t", leased), func(t *testing.T) {
			pool, account := authTestPool(t, false)
			manager, _ := authTestManager(t, pool, account, func(int32) (*aistudio.NativeWorker, error) {
				return nil, loginRedirectError()
			})
			refresher := newAuthTestRefresher(t, &authTestCounts{})
			refresher.workers = manager
			manager.refresher = refresher
			service := &trackedService{pool: pool, workers: manager, requests: manager.requests}
			note := authRequiredHook(service.noteAuthRequired)
			manager.authRequired.Store(&note)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if leased {
				if _, err := service.acquireWarmLease(ctx, aistudio.AccountSelection{ModelID: "gemini-test", Method: "generateContent"}); err == nil {
					t.Fatal("登录失效时不应取得租约")
				}
			} else if _, err := manager.ensureWorker(ctx, authTestAccount, "", false); !errors.Is(err, aistudio.ErrAuthenticationRequired) {
				t.Fatalf("应返回登录失效，实际 %v", err)
			}
			if status := authTestStatus(t, pool); status.State != aistudio.AccountAuthRequired {
				t.Fatalf("应标为需要登录，实际 %s", status.State)
			}
			if !hasLogContaining(manager.requests, "账号需要登录 | 来源=启动 | 出口=直连") || hasLogContaining(manager.requests, "来源=续签重放") {
				t.Fatalf("启动阶段的登录失效应记为启动，日志 = %v", serviceLogMessages(manager.requests))
			}
		})
	}
}

// streamAuthUpstream 让 accountID 的生成先输出一段正文、随后在流内返回登录失效
type streamAuthUpstream struct {
	*forbiddenUpstream
	accountID string
}

func (upstream *streamAuthUpstream) Generate(ctx context.Context, request aistudio.GenerateRequest) (<-chan aistudio.Event, error) {
	if request.AccountID != upstream.accountID {
		return upstream.forbiddenUpstream.Generate(ctx, request)
	}
	events := make(chan aistudio.Event, 2)
	events <- aistudio.Event{Kind: aistudio.EventText, Text: "partial"}
	events <- aistudio.Event{Kind: aistudio.EventError, Err: authFailureError()}
	close(events)
	return events, nil
}

// TestStreamAuthRequiredReleasesWorker 流式输出中途确认登录失效：账号标需要登录，请求结束后释放 Worker，记为“流内”
func TestStreamAuthRequiredReleasesWorker(t *testing.T) {
	manager, pool, requests := partitionTestManager(t, []string{partitionNormalA, partitionNormalB}, nil, 1, 2, 0, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	service := unschedulableTestService(ctx, manager, pool, requests)
	service.service = &streamAuthUpstream{forbiddenUpstream: &forbiddenUpstream{}, accountID: partitionNormalA}
	service.timeout.Store(int64(time.Minute))
	if _, err := manager.promote(ctx, partitionNormalA, testRuntimeModel); err != nil {
		t.Fatal(err)
	}
	text, err, _ := runModelRequest(t, service, "req-stream-401", partitionNormalA, testRuntimeModel)
	if text != "partial" || !aistudio.DefinitiveAuthenticationFailure(err) {
		t.Fatalf("应先输出正文再返回登录失效: text=%q err=%v", text, err)
	}
	if status := testStatus(t, pool, partitionNormalA); status.State != aistudio.AccountAuthRequired {
		t.Fatalf("应标为需要登录，实际 %s", status.State)
	}
	if !waitUntil(3*time.Second, func() bool { return !slices.Contains(manager.WarmAccountIDs(), partitionNormalA) }) {
		t.Fatalf("请求结束后应释放 Worker，驻留 Worker = %v", manager.WarmAccountIDs())
	}
	if !hasLogContaining(requests, "账号需要登录 | 来源=流内 | 出口=直连 | 出口限流中=否") ||
		!waitUntil(time.Second, func() bool { return hasLogContaining(requests, "WAA Worker 释放 | 原因=需要登录") }) {
		t.Fatalf("应记录流内需要登录与释放，日志 = %v", serviceLogMessages(requests))
	}
}

// TestGenerationAuthRequiredReleasesWorker 生成请求首事件前确认登录失效（上游直接返回 401，不经认证续签传输）：generateWithRetry
// 把账号标为需要登录，租约释放后写“账号需要登录 | 来源=生成”，并登记释放它的驻留 Worker，一轮淘汰内关闭
func TestGenerationAuthRequiredReleasesWorker(t *testing.T) {
	manager, pool, requests := partitionTestManager(t, []string{partitionNormalA, partitionNormalB}, nil, 1, 2, 0, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	service := unschedulableTestService(ctx, manager, pool, requests)
	service.service = newSwitchUpstream().failAll(authFailureError, partitionNormalA)
	service.timeout.Store(int64(time.Minute))
	if _, err := manager.promote(ctx, partitionNormalA, testRuntimeModel); err != nil {
		t.Fatal(err)
	}
	_, err, _ := runModelRequest(t, service, "req-generate-401", partitionNormalA, testRuntimeModel)
	if !aistudio.DefinitiveAuthenticationFailure(err) {
		t.Fatalf("指定账号的请求应以登录失效结束，实际 %v", err)
	}
	if status := testStatus(t, pool, partitionNormalA); status.State != aistudio.AccountAuthRequired {
		t.Fatalf("应标为需要登录，实际 %s", status.State)
	}
	if !waitUntil(3*time.Second, func() bool { return !slices.Contains(manager.WarmAccountIDs(), partitionNormalA) }) {
		t.Fatalf("请求结束后应释放 Worker，驻留 Worker = %v", manager.WarmAccountIDs())
	}
	if !hasLogContaining(requests, "账号需要登录 | 来源=生成 | 出口=直连 | 出口限流中=否") ||
		!waitUntil(time.Second, func() bool { return hasLogContaining(requests, "WAA Worker 释放 | 原因=需要登录") }) {
		t.Fatalf("应记录生成路径的需要登录与释放，日志 = %v", serviceLogMessages(requests))
	}
}

// TestUnschedulableConcurrentRelease 大量账户在请求进行中先后被确认需要登录，同时补登记、冷却登记与现场回收并发进行：
// 每个 Worker 只关闭一次、不死锁，租约全部释放，登记清空
func TestUnschedulableConcurrentRelease(t *testing.T) {
	const count = 12
	ids := make([]string, count)
	for index := range ids {
		ids[index] = fmt.Sprintf("conc-%02d@example.com", index)
	}
	manager, pool, requests := partitionTestManager(t, ids, nil, count, count, 0, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	service := unschedulableTestService(ctx, manager, pool, requests)
	for _, accountID := range ids {
		if _, err := manager.promote(ctx, accountID, testRuntimeModel); err != nil {
			t.Fatal(err)
		}
	}
	refresher := &authRuntimeRefresher{requests: requests, workers: manager}
	background := make([]func(), count)
	for index, accountID := range ids {
		background[index] = holdLease(t, pool, accountID)
	}
	var workers sync.WaitGroup
	for index, accountID := range ids {
		workers.Add(1)
		go func(index int, accountID string) {
			defer workers.Done()
			lease, err := pool.AcquireFor(ctx, aistudio.AccountSelection{AccountID: accountID, ModelID: testRuntimeModel})
			if err != nil {
				t.Error(err)
				return
			}
			if index%2 == 0 {
				// 一半账号先有冷却登记（没有可接替账户，推迟再试），随后改为不可调度登记
				until := time.Now().Add(2 * time.Hour)
				if err := pool.MarkCooldownIfGeneration(accountID, "", pool.ModelAccessGeneration(accountID), time.Now(), until, "每日限额"); err != nil {
					t.Error(err)
				}
				manager.retireWorker(accountID, "")
			}
			markAuthRequiredViaTransport(t, refresher, lease)
			manager.releaseUnschedulable(accountID, unschedulableAuthRequired)
			time.Sleep(time.Duration(index%4) * 10 * time.Millisecond)
			_ = lease.Release()
		}(index, accountID)
	}
	stop := make(chan struct{})
	var sweepers sync.WaitGroup
	sweepers.Add(2)
	go func() {
		defer sweepers.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			service.registerUnschedulableWorkers()
			time.Sleep(5 * time.Millisecond)
		}
	}()
	go func() {
		defer sweepers.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			_, _ = manager.reclaimDeadWorker(ctx, aistudio.PoolScopeNormal, testRuntimeModel)
			time.Sleep(5 * time.Millisecond)
		}
	}()
	workers.Wait()
	for _, release := range background {
		release()
	}
	if !waitUntil(15*time.Second, func() bool { return len(manager.WarmAccountIDs()) == 0 }) {
		t.Fatalf("全部 Worker 应被释放，驻留 Worker = %v", manager.WarmAccountIDs())
	}
	close(stop)
	sweepers.Wait()
	if !waitUntil(3*time.Second, func() bool { return manager.retirementCount() == 0 }) {
		t.Fatalf("登记应清空，登记数 = %d", manager.retirementCount())
	}
	for _, accountID := range ids {
		closed := sourceLogCount(requests, accountID, "WAA Worker 释放 |") + sourceLogCount(requests, accountID, "WAA Worker 回收冷却占位 |") +
			sourceLogCount(requests, accountID, "WAA Worker 冷却淘汰 |")
		if closed != 1 {
			t.Fatalf("%s 应只关闭一次，实际 %d 次，日志 = %v", accountID, closed, serviceLogMessages(requests))
		}
		if busy, _ := pool.Activity(accountID); busy {
			t.Fatalf("%s 的租约应全部释放", accountID)
		}
	}
	if hasLogContaining(requests, "WAA Worker 释放失败") {
		t.Fatalf("不应有释放失败，日志 = %v", serviceLogMessages(requests))
	}
}
