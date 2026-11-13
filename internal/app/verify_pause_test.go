package app

import (
	"context"
	"net/http"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// verifyRequiredMessage 为上游要求先验证账号时 403 的原文
const verifyRequiredMessage = "Your account must be verified before it can continue using the Gemini API. Verify your account at https://aistudio.google.com/verify-account."

// verifyRequiredError 构造生成请求要求先验证账号的 403（协议错误码 7）
func verifyRequiredError() error {
	return &aistudio.RPCError{Method: "GenerateContent", StatusCode: http.StatusForbidden, Code: 7, Message: verifyRequiredMessage}
}

// pauseVerifyRequired 像生成路径一样给账户写入需要验证账号的整户暂停（全局冷却 verifyRequiredPause，原因 verifyRequiredPauseReason），
// 代表其他路径写入、重启后读回的暂停
func pauseVerifyRequired(t *testing.T, pool *aistudio.AccountPool, accountID string) {
	t.Helper()
	until := time.Now().Add(verifyRequiredPause)
	if err := pool.MarkCooldownIfGeneration(
		accountID, "", pool.ModelAccessGeneration(accountID), time.Now(), until, verifyRequiredPauseReason,
	); err != nil {
		t.Fatal(err)
	}
}

// assertNoCooldownRetire 断言没有按冷却淘汰处理（冷却淘汰、冷却清扫、补登记淘汰、排空、替补都不写）
func assertNoCooldownRetire(t *testing.T, requests *requestRegistry) {
	t.Helper()
	for _, fragment := range []string{"冷却淘汰", "冷却清扫", "补登记淘汰", "排空", "替补"} {
		if hasLogContaining(requests, fragment) {
			t.Fatalf("需要验证账号的 Worker 应按不可调度释放，不应出现“%s”，日志 = %v", fragment, serviceLogMessages(requests))
		}
	}
}

// TestVerifyRequiredPauseReasonStable 整户暂停的原因不能改动文字：线上已持久化的暂停靠逐字相同识别为不可调度，
// 认证文件更新、验证通过时靠后缀按 403 冷却清除，管理页清除 403 记录时靠原因只解除这一种暂停
func TestVerifyRequiredPauseReasonStable(t *testing.T) {
	want := "需要验证账号（verify-account），在官网验证后到管理页点验证；已 1 " + forbiddenPauseReasonSuffix
	if verifyRequiredPauseReason != want {
		t.Fatalf("整户暂停原因 = %q，期望 %q", verifyRequiredPauseReason, want)
	}
	cooldown := aistudio.CooldownState{Until: time.Now().Add(time.Hour), Reason: verifyRequiredPauseReason}
	if !forbiddenCooldown("*", cooldown) || !verifyRequiredPauseCooldown("*", cooldown) {
		t.Fatal("整户暂停应能按 403 冷却清除，也应能被清除 403 记录单独解除")
	}
}

// TestVerifyPauseReleasedWithoutSpare 需要验证账号的整户暂停是 30 天的全局冷却，原先按冷却淘汰要等分区内有可接替账户：
// 分区只有这一个账户、或全部账户都暂停时 Worker 一直驻留。现在 keepWarm 补登记释放，空闲即关，写“WAA Worker 释放 | 原因=需要验证账号”，
// 不写冷却淘汰、冷却清扫与替补；之后 keepWarm 也不再为它启动 Worker
func TestVerifyPauseReleasedWithoutSpare(t *testing.T) {
	cases := []struct {
		name     string
		accounts []string
		paused   []string
	}{
		{name: "只有一个账户", accounts: []string{partitionNormalA}, paused: []string{partitionNormalA}},
		{name: "全部账户都暂停", accounts: []string{partitionNormalA, partitionNormalB}, paused: []string{partitionNormalA, partitionNormalB}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			manager, pool, requests := partitionTestManager(t, testCase.accounts, nil, len(testCase.accounts), len(testCase.accounts), 0, 1)
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			service := unschedulableTestService(ctx, manager, pool, requests)
			if _, err := manager.promote(ctx, partitionNormalA, testRuntimeModel); err != nil {
				t.Fatal(err)
			}
			for _, accountID := range testCase.paused {
				pauseVerifyRequired(t, pool, accountID)
			}
			service.keepWarmPass(ctx, time.Now())
			if !waitUntil(3*time.Second, func() bool { return !slices.Contains(manager.WarmAccountIDs(), partitionNormalA) }) {
				t.Fatalf("没有可接替账户也应释放需要验证账号的 Worker，驻留 Worker = %v，日志 = %v",
					manager.WarmAccountIDs(), serviceLogMessages(requests))
			}
			if !waitUntil(time.Second, func() bool {
				return sourceLogCount(requests, partitionNormalA, "WAA Worker 释放 | 原因=需要验证账号 | 登记后=") == 1
			}) {
				t.Fatalf("应按需要验证账号释放一次，日志 = %v", serviceLogMessages(requests))
			}
			if !hasLogContaining(requests, "WAA Worker 补登记释放 | 新增=1 | 驻留中不可调度=1/1 | 原因=需要验证账号 1") {
				t.Fatalf("keepWarm 补登记应按需要验证账号计数，日志 = %v", serviceLogMessages(requests))
			}
			// 之后的 keepWarm 不为暂停中的账户启动 Worker（预热目标不计冷却中的账户）
			for range 3 {
				service.keepWarmPass(ctx, time.Now())
				time.Sleep(100 * time.Millisecond)
			}
			if warm, opening := manager.WarmAccountIDs(), manager.OpeningAccountIDs(); len(warm) != 0 || len(opening) != 0 {
				t.Fatalf("暂停中的账户不应再启动 Worker: 驻留=%v 启动中=%v", warm, opening)
			}
			assertNoCooldownRetire(t, requests)
			if got := manager.retirementCount(); got != 0 {
				t.Fatalf("关闭后登记应清除，登记数 = %d", got)
			}
		})
	}
}

// TestVerifyPauseBusyReleasedAfterLease 暂停写入时账号还有进行中的请求：不排空、不要求可接替账户（分区里只有这一个账户），
// 请求释放租约后一轮淘汰内关闭
func TestVerifyPauseBusyReleasedAfterLease(t *testing.T) {
	manager, pool, requests := partitionTestManager(t, []string{partitionNormalA}, nil, 1, 1, 0, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	service := unschedulableTestService(ctx, manager, pool, requests)
	if _, err := manager.promote(ctx, partitionNormalA, testRuntimeModel); err != nil {
		t.Fatal(err)
	}
	release := holdLease(t, pool, partitionNormalA)
	defer release()
	pauseVerifyRequired(t, pool, partitionNormalA)
	service.keepWarmPass(ctx, time.Now())
	time.Sleep(600 * time.Millisecond)
	if !slices.Contains(manager.WarmAccountIDs(), partitionNormalA) {
		t.Fatal("账号仍在服务请求时不应关闭 Worker")
	}
	if pool.Draining(partitionNormalA) || manager.drainCount(aistudio.PoolScopeNormal) != 0 {
		t.Fatal("需要验证账号的登记不排空")
	}
	if got := manager.retirementCount(); got != 1 {
		t.Fatalf("忙碌期间登记应保留，登记数 = %d", got)
	}
	releasedAt := time.Now()
	release()
	if !waitUntil(3*time.Second, func() bool { return !slices.Contains(manager.WarmAccountIDs(), partitionNormalA) }) {
		t.Fatalf("租约释放后应关闭 Worker（没有可接替账户也关），驻留 Worker = %v，日志 = %v",
			manager.WarmAccountIDs(), serviceLogMessages(requests))
	}
	if waited := time.Since(releasedAt); waited >= 2*time.Second {
		t.Fatalf("租约释放 %s 后才关闭，期望一轮淘汰内关闭", waited)
	}
	if !waitUntil(time.Second, func() bool { return hasLogContaining(requests, "WAA Worker 释放 | 原因=需要验证账号") }) {
		t.Fatalf("应记录释放日志，日志 = %v", serviceLogMessages(requests))
	}
	assertNoCooldownRetire(t, requests)
}

// TestVerifyPauseGenerationReleasesWorker 生成请求遇到 403 verify-account：写入整户暂停后立即登记释放（不等 keepWarm），
// 请求换到其他账户完成，结束后关闭这个 Worker，写“WAA Worker 释放 | 原因=需要验证账号”，不按冷却淘汰、不启替补。
// “Build 仍可用”：同一账号另一通道还能服务该模型，单模型冷却后 retireCooledWorker 不登记冷却淘汰，只有写入暂停后的
// releaseUnschedulable 能做到请求结束即关（测试不跑 keepWarm）；“只有 Playground”：单模型冷却已先登记冷却淘汰，
// 淘汰循环按不可调度转成释放
func TestVerifyPauseGenerationReleasesWorker(t *testing.T) {
	for _, testCase := range []struct {
		name  string
		build bool
	}{
		{name: "只有 Playground"},
		{name: "Build 仍可用", build: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			manager, pool, requests := partitionTestManager(t, []string{partitionNormalA, partitionNormalB}, nil, 1, 2, 0, 1)
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			service := unschedulableTestService(ctx, manager, pool, requests)
			withPermissionDenied(t, service)
			upstream := newSwitchUpstream().failAll(verifyRequiredError, partitionNormalA)
			service.service = upstream
			service.timeout.Store(int64(time.Minute))
			if testCase.build {
				pool.SetUpstreamChannels([]aistudio.Channel{aistudio.ChannelPlayground, aistudio.ChannelBuild})
				target := aistudio.Model{ID: testRuntimeModel, Methods: []string{"generateContent"}, Capabilities: map[string]bool{"chat_model": true}}
				if err := pool.SetBuildCatalog(partitionNormalA, []aistudio.Model{target}); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := manager.promote(ctx, partitionNormalA, testRuntimeModel); err != nil {
				t.Fatal(err)
			}
			text, err, _ := runModelRequest(t, service, "req-verify-release", "", testRuntimeModel)
			if err != nil || text != "hello" {
				t.Fatalf("应换到其他账户完成: text=%q err=%v，日志 = %v", text, err, serviceLogMessages(requests))
			}
			if !hasLogContaining(requests, "账号暂停调度 | 原因="+verifyRequiredPauseReason) {
				t.Fatalf("应暂停整个账户，日志 = %v", serviceLogMessages(requests))
			}
			if testCase.build {
				// 确认用例走到了“另一通道仍可用”：单模型冷却只写在返回 403 的那个通道上，retireCooledWorker 因此不登记冷却淘汰
				status, _ := pool.StatusOf(partitionNormalA)
				_, playground := status.Cooldowns[testRuntimeModel]
				_, build := status.Cooldowns[aistudio.ChannelCooldownScope(aistudio.ChannelBuild, testRuntimeModel)]
				if playground == build {
					t.Fatalf("单模型冷却应只写在一个通道上，冷却 = %v，调用 = %v", status.Cooldowns, upstream.called())
				}
			}
			if !waitUntil(3*time.Second, func() bool { return !slices.Contains(manager.WarmAccountIDs(), partitionNormalA) }) {
				t.Fatalf("请求结束后应释放需要验证账号的 Worker，驻留 Worker = %v，日志 = %v",
					manager.WarmAccountIDs(), serviceLogMessages(requests))
			}
			if !waitUntil(time.Second, func() bool {
				return sourceLogCount(requests, partitionNormalA, "WAA Worker 释放 | 原因=需要验证账号 | 登记后=") == 1
			}) {
				t.Fatalf("应按需要验证账号释放一次，日志 = %v", serviceLogMessages(requests))
			}
			assertNoCooldownRetire(t, requests)
			if !slices.Contains(manager.WarmAccountIDs(), partitionNormalB) {
				t.Fatalf("换号后的账户应保留 Worker，驻留 Worker = %v", manager.WarmAccountIDs())
			}
		})
	}
}

// TestVerifyPauseSkippedByRotation 冷却轮换不处理需要验证账号的 Worker：分区里有可接替账户、存在热门冷却模型（第一次 verify 403
// 还写了 30 分钟的单模型冷却），暂停中的空闲 Worker 已过 rotationWarmupGrace，rotateCooledWorkers 也不关闭它、不写“冷却轮换”与
// “替补”（同一个 keepWarm 周期里补登记释放只是异步唤醒淘汰循环，轮换先抢到时会按冷却轮换关闭并多冷启动一个替补）；
// 随后仍由释放关闭
func TestVerifyPauseSkippedByRotation(t *testing.T) {
	const (
		busyE  = "normal-e@example.com"
		spareD = "normal-d@example.com"
	)
	warm := []string{partitionNormalA, partitionNormalB, partitionNormalC, busyE}
	manager, pool, requests := partitionTestManager(t, append(slices.Clone(warm), spareD), nil, len(warm), len(warm)+1, 0, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	service := unschedulableTestService(ctx, manager, pool, requests)
	until := time.Now().Add(2 * time.Hour)
	for _, accountID := range warm {
		if _, err := manager.promote(ctx, accountID, testRuntimeModel); err != nil {
			t.Fatal(err)
		}
		manager.accounts[accountID].readyAt.Store(time.Now().Add(-time.Hour).UnixNano())
		if err := pool.MarkCooldownIfGeneration(accountID, testRuntimeModel, pool.ModelAccessGeneration(accountID), time.Now(), until, "无权限"); err != nil {
			t.Fatal(err)
		}
	}
	pauseVerifyRequired(t, pool, partitionNormalA)
	// 其余冷却的 Worker 都在服务请求：轮换只可能选中暂停中的 A
	for _, accountID := range warm[1:] {
		release := holdExclusive(t, pool, accountID)
		defer release()
	}
	if hot, counts := service.hotModelsIn(warm); !slices.Equal(hot, []string{testRuntimeModel}) {
		t.Fatalf("应有热门冷却模型 %s，实际 %v（%v）", testRuntimeModel, hot, counts)
	}
	service.rotateCooledWorkers(ctx)
	manager.waitPrewarm()
	if !slices.Contains(manager.WarmAccountIDs(), partitionNormalA) {
		t.Fatalf("冷却轮换不应关闭需要验证账号的 Worker，日志 = %v", serviceLogMessages(requests))
	}
	for _, fragment := range []string{"冷却轮换", "替补"} {
		if hasLogContaining(requests, fragment) {
			t.Fatalf("不应出现“%s”，日志 = %v", fragment, serviceLogMessages(requests))
		}
	}
	if added := service.registerUnschedulableWorkers(); added != 1 {
		t.Fatalf("暂停中的驻留 Worker 应补登记释放，新增 = %d", added)
	}
	if !waitUntil(3*time.Second, func() bool {
		return sourceLogCount(requests, partitionNormalA, "WAA Worker 释放 | 原因=需要验证账号 | 登记后=") == 1
	}) {
		t.Fatalf("应按需要验证账号释放，日志 = %v", serviceLogMessages(requests))
	}
	assertNoCooldownRetire(t, requests)
}

// TestVerifyPauseSkippedBySweep 冷却清扫不处理需要验证账号的 Worker：整户暂停也是全局长时间冷却，分区里有可接替账户时
// sweepDeadWorkers 原先会按“冷却清扫”关闭它并启动替补；现在跳过，只由释放关闭
func TestVerifyPauseSkippedBySweep(t *testing.T) {
	manager, pool, requests := partitionTestManager(t, []string{partitionNormalA, partitionNormalB, partitionNormalC}, nil, 2, 3, 0, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	service := unschedulableTestService(ctx, manager, pool, requests)
	for _, accountID := range []string{partitionNormalA, partitionNormalB} {
		if _, err := manager.promote(ctx, accountID, testRuntimeModel); err != nil {
			t.Fatal(err)
		}
	}
	pauseVerifyRequired(t, pool, partitionNormalA)
	if spare := pool.SpareAccounts(aistudio.PoolScopeNormal, map[string]struct{}{partitionNormalA: {}, partitionNormalB: {}}, nil, rotationMinRemaining, 1); spare != 1 {
		t.Fatalf("分区里应有可接替账户，实际 %d", spare)
	}
	if got := service.sweepDeadWorkers(ctx); got != 0 {
		t.Fatalf("冷却清扫不应关闭需要验证账号的 Worker，实际关闭 %d 个，日志 = %v", got, serviceLogMessages(requests))
	}
	manager.waitPrewarm()
	if !slices.Contains(manager.WarmAccountIDs(), partitionNormalA) {
		t.Fatalf("冷却清扫不应关闭需要验证账号的 Worker，驻留 Worker = %v", manager.WarmAccountIDs())
	}
	if added := service.registerUnschedulableWorkers(); added != 1 {
		t.Fatalf("暂停中的驻留 Worker 应补登记释放，新增 = %d", added)
	}
	if !waitUntil(3*time.Second, func() bool {
		return sourceLogCount(requests, partitionNormalA, "WAA Worker 释放 | 原因=需要验证账号 | 登记后=") == 1
	}) {
		t.Fatalf("应按需要验证账号释放，日志 = %v", serviceLogMessages(requests))
	}
	assertNoCooldownRetire(t, requests)
}

// TestVerifyPauseNotPrewarmedAndRecovers 暂停中的账户（含重启后从运行态读回的暂停）不预热；暂停解除（验证通过清除 403 状态、
// 清除 403 记录）后恢复可调度，keepWarm 重新预热它
func TestVerifyPauseNotPrewarmedAndRecovers(t *testing.T) {
	manager, pool, requests := partitionTestManager(t, []string{partitionNormalA, partitionNormalB, partitionNormalC}, nil, 3, 3, 0, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	service := unschedulableTestService(ctx, manager, pool, requests)
	pauseVerifyRequired(t, pool, partitionNormalA)
	pauseVerifyRequired(t, pool, partitionNormalC)
	if reasons := pool.UnschedulableSet([]string{partitionNormalA, partitionNormalB}); reasons[partitionNormalA] != "需要验证账号" || len(reasons) != 1 {
		t.Fatalf("暂停中的账户应不可调度（需要验证账号），实际 %v", reasons)
	}
	// 重启：从账户目录重新载入，暂停仍在运行态里，读回后同样不可调度
	loaded, failures := aistudio.NewAccountStore(filepath.Dir(testAccountDirectory(t, pool, partitionNormalA))).LoadTolerant()
	if len(failures) != 0 {
		t.Fatal(failures)
	}
	reloaded := aistudio.NewAccountPool(loaded, 2)
	if reason := reloaded.UnschedulableSet([]string{partitionNormalA})[partitionNormalA]; reason != "需要验证账号" {
		t.Fatalf("重启后读回的暂停应不可调度（需要验证账号），实际 %q", reason)
	}
	service.keepWarmPass(ctx, time.Now())
	if !waitUntil(5*time.Second, func() bool { return slices.Contains(manager.WarmAccountIDs(), partitionNormalB) }) {
		t.Fatalf("应预热未暂停的账户，驻留 Worker = %v", manager.WarmAccountIDs())
	}
	for range 3 {
		service.keepWarmPass(ctx, time.Now())
		time.Sleep(100 * time.Millisecond)
	}
	for _, accountID := range []string{partitionNormalA, partitionNormalC} {
		if slices.Contains(manager.WarmAccountIDs(), accountID) || slices.Contains(manager.OpeningAccountIDs(), accountID) {
			t.Fatalf("暂停中的账户 %s 不应预热: 驻留=%v 启动中=%v", accountID, manager.WarmAccountIDs(), manager.OpeningAccountIDs())
		}
	}
	// 验证通过（与认证文件更新相同）清除 403 状态：暂停解除，恢复可调度，keepWarm 重新预热它
	service.clearPermissionDeniedState(partitionNormalA, "账户验证")
	// 清除 403 记录：只解除需要验证账号的整户暂停
	if lifted := service.liftVerifyRequiredPauses([]string{partitionNormalC}, false, time.Now()); lifted != 1 {
		t.Fatalf("清除 403 记录应解除 1 个暂停，实际 %d", lifted)
	}
	if reasons := pool.UnschedulableSet([]string{partitionNormalA, partitionNormalC}); len(reasons) != 0 {
		t.Fatalf("暂停解除后应恢复可调度，实际 %v", reasons)
	}
	service.keepWarmPass(ctx, time.Now())
	if !waitUntil(5*time.Second, func() bool {
		warm := manager.WarmAccountIDs()
		return slices.Contains(warm, partitionNormalA) && slices.Contains(warm, partitionNormalC)
	}) {
		t.Fatalf("暂停解除后应重新预热，驻留 Worker = %v", manager.WarmAccountIDs())
	}
}

// testAccountDirectory 返回账户的目录
func testAccountDirectory(t *testing.T, pool *aistudio.AccountPool, accountID string) string {
	t.Helper()
	account, err := pool.Account(accountID)
	if err != nil {
		t.Fatal(err)
	}
	return account.Directory
}

// TestVerifyPauseLiftedDropsRelease 暂停写入时账号忙碌、释放已登记：请求结束前暂停被解除（验证通过、认证文件更新、任一模型成功、
// 清除 403 记录、到期），账户恢复可调度，释放登记作废，不误关刚恢复的账户的 Worker
func TestVerifyPauseLiftedDropsRelease(t *testing.T) {
	lifts := []struct {
		name string
		// short 为真时暂停只写 800ms（代表到期）
		short bool
		lift  func(t *testing.T, service *trackedService, accountID string)
	}{
		{name: "验证通过", lift: func(_ *testing.T, service *trackedService, accountID string) {
			service.clearPermissionDeniedState(accountID, "账户验证")
		}},
		{name: "认证文件更新", lift: func(_ *testing.T, service *trackedService, accountID string) {
			service.forgetPermissionDenied(accountID, "外部更新")
		}},
		{name: "任一模型成功", lift: func(_ *testing.T, service *trackedService, accountID string) {
			service.permissionDeniedSucceeded(accountID, "other-model", aistudio.ChannelPlayground)
		}},
		{name: "清除403记录", lift: func(t *testing.T, service *trackedService, accountID string) {
			if lifted := service.liftVerifyRequiredPauses([]string{accountID}, false, time.Now()); lifted != 1 {
				t.Fatalf("应解除 1 个暂停，实际 %d", lifted)
			}
		}},
		{name: "到期", short: true, lift: func(*testing.T, *trackedService, string) {
			time.Sleep(900 * time.Millisecond)
		}},
	}
	for _, testCase := range lifts {
		t.Run(testCase.name, func(t *testing.T) {
			manager, pool, requests := partitionTestManager(t, []string{partitionNormalA}, nil, 1, 1, 0, 1)
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			service := unschedulableTestService(ctx, manager, pool, requests)
			withPermissionDenied(t, service)
			if _, err := manager.promote(ctx, partitionNormalA, testRuntimeModel); err != nil {
				t.Fatal(err)
			}
			release := holdLease(t, pool, partitionNormalA)
			defer release()
			service.recordPermissionDenied(partitionNormalA, testRuntimeModel, aistudio.ChannelPlayground,
				"AI Studio GenerateContent 返回 HTTP 403、协议错误码 7: "+verifyRequiredMessage, time.Now(), nil)
			if testCase.short {
				until := time.Now().Add(800 * time.Millisecond)
				if err := pool.MarkCooldownIfGeneration(
					partitionNormalA, "", pool.ModelAccessGeneration(partitionNormalA), time.Now(), until, verifyRequiredPauseReason,
				); err != nil {
					t.Fatal(err)
				}
			} else {
				pauseVerifyRequired(t, pool, partitionNormalA)
			}
			if added := service.registerUnschedulableWorkers(); added != 1 {
				t.Fatalf("暂停中的驻留 Worker 应补登记释放，新增 = %d", added)
			}
			testCase.lift(t, service, partitionNormalA)
			if reasons := pool.UnschedulableSet([]string{partitionNormalA}); len(reasons) != 0 {
				t.Fatalf("暂停解除后应恢复可调度，实际 %v", reasons)
			}
			release()
			time.Sleep(600 * time.Millisecond)
			if !slices.Contains(manager.WarmAccountIDs(), partitionNormalA) {
				t.Fatalf("暂停解除后不应关闭刚恢复的账户的 Worker，日志 = %v", serviceLogMessages(requests))
			}
			if !waitUntil(time.Second, func() bool { return manager.retirementCount() == 0 }) {
				t.Fatalf("暂停解除后释放登记应作废，登记数 = %d", manager.retirementCount())
			}
			if hasLogContaining(requests, "WAA Worker 释放") {
				t.Fatalf("不应释放刚恢复的账户的 Worker，日志 = %v", serviceLogMessages(requests))
			}
		})
	}
}

// TestVerifyPauseAdminVerifyUsesBrowser 用户主动“验证”：暂停中的账户（Worker 已释放）照常用隔离浏览器验证（独占租约不受冷却限制），
// 验证通过后暂停解除，账户恢复预热
func TestVerifyPauseAdminVerifyUsesBrowser(t *testing.T) {
	admin, _, accounts := keepCatalogAdmin(t, "verify-pause@example.com")
	accountID := accounts[0].ID
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := admin.workers.promote(ctx, accountID, testRuntimeModel); err != nil {
		t.Fatal(err)
	}
	pauseVerifyRequired(t, admin.pool, accountID)
	if added := admin.service.registerUnschedulableWorkers(); added != 1 {
		t.Fatalf("暂停中的驻留 Worker 应补登记释放，新增 = %d", added)
	}
	if !waitUntil(3*time.Second, func() bool { return !slices.Contains(admin.workers.WarmAccountIDs(), accountID) }) {
		t.Fatalf("暂停中的 Worker 应释放，驻留 Worker = %v", admin.workers.WarmAccountIDs())
	}
	login := &verifyTestLogin{verification: aistudio.LoginVerification{Authenticated: true}}
	admin.login = login
	account, err := admin.VerifyAccount(ctx, accountID)
	if err != nil {
		t.Fatal(err)
	}
	if login.calls.Load() != 1 || account.Authenticated == nil || !*account.Authenticated {
		t.Fatalf("暂停中的账户应能用浏览器验证: 调用=%d 结论=%v", login.calls.Load(), account.Authenticated)
	}
	if reasons := admin.pool.UnschedulableSet([]string{accountID}); len(reasons) != 0 || admin.pool.AccountCoolingDown(accountID, "") {
		t.Fatalf("验证通过后应解除暂停、恢复可调度，实际 %v", reasons)
	}
	admin.workers.StartPrewarm(ctx)
	if !waitUntil(5*time.Second, func() bool { return slices.Contains(admin.workers.WarmAccountIDs(), accountID) }) {
		t.Fatalf("验证通过后应重新预热，驻留 Worker = %v", admin.workers.WarmAccountIDs())
	}
}

// TestVerifyPauseCountsInStatus 状态接口里驻留 Worker 的账户暂停中时同时计入不可调度与死槽位（两边都计是既有口径）
func TestVerifyPauseCountsInStatus(t *testing.T) {
	manager, pool, requests := partitionTestManager(t, []string{partitionNormalA, partitionNormalB}, nil, 2, 2, 0, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for _, accountID := range []string{partitionNormalA, partitionNormalB} {
		if _, err := manager.promote(ctx, accountID, testRuntimeModel); err != nil {
			t.Fatal(err)
		}
	}
	pauseVerifyRequired(t, pool, partitionNormalA)
	service := &trackedService{lifecycle: ctx, pool: pool, requests: requests, workers: manager}
	admin := &runtimeAdmin{lifecycle: ctx, pool: pool, service: service, requests: requests, workers: manager}
	status, err := admin.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if status.Workers.Unschedulable != 1 || status.Workers.Dead != 1 || status.Workers.Warm != 2 {
		t.Fatalf("暂停中的驻留 Worker 应计入不可调度与死槽位: %+v", status.Workers)
	}
}
