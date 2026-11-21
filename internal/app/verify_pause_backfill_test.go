package app

import (
	"context"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// globalPauseOf 返回账户全局作用域的冷却；没有时 ok 为假
func globalPauseOf(t *testing.T, pool *aistudio.AccountPool, accountID string) (aistudio.CooldownState, bool) {
	t.Helper()
	status, exists := pool.StatusOf(accountID)
	if !exists {
		t.Fatalf("账号 %s 不存在", accountID)
	}
	cooldown, ok := status.Cooldowns["*"]
	return cooldown, ok
}

// modelVerifyOf 返回账户在模型 Playground 键与 Build 键上的冷却
func modelVerifyOf(t *testing.T, pool *aistudio.AccountPool, accountID string, modelID string) (aistudio.CooldownState, aistudio.CooldownState) {
	t.Helper()
	status, exists := pool.StatusOf(accountID)
	if !exists {
		t.Fatalf("账号 %s 不存在", accountID)
	}
	return status.Cooldowns[modelID], status.Cooldowns[aistudio.ChannelCooldownScope(aistudio.ChannelBuild, modelID)]
}

// TestReconcileVerifyPauses 启动时按登记整理需要验证账号的暂停：两个及以上模型的整户（暂停生效的保留，没有的补写：期限从最近一次 403
// 起算 30 天、超过 30 天的不补、之后全局有更晚记录的不补）；只有一个模型的只暂停这个模型（整户暂停生效的改为单个模型，恢复时间沿用、
// 全局暂停删除；没有暂停的补写两个键；已有单个模型冷却的不动；最近一次晚于现在的截到 30 天后、检查时间截到现在）；同一模型的
// Playground 与 Build、大小写不同的写法算一个；没有登记的整户暂停不动；普通 403 不管。汇总行只有计数；再次运行不再写入
func TestReconcileVerifyPauses(t *testing.T) {
	const (
		twoModels = "reconcile-two@example.com"
		oneModel  = "reconcile-one@example.com"
		old       = "reconcile-old@example.com"
		succeeded = "reconcile-succeeded@example.com"
		plain     = "reconcile-plain@example.com"
		pausedTwo = "reconcile-paused-two@example.com"
		pausedOne = "reconcile-paused-one@example.com"
		orphan    = "reconcile-orphan@example.com"
		existing  = "reconcile-existing@example.com"
		future    = "reconcile-future@example.com"
		channels  = "reconcile-channels@example.com"
		casing    = "reconcile-casing@example.com"
	)
	ids := []string{twoModels, oneModel, old, succeeded, plain, pausedTwo, pausedOne, orphan, existing, future, channels, casing}
	pool, _ := testAccountPool(t, ids...)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	requests := newRequestRegistry(ctx)
	service := &trackedService{lifecycle: ctx, pool: pool, requests: requests, forbidden: newForbiddenTracker()}
	registry := withPermissionDenied(t, service)
	now := time.Now()
	verify := "AI Studio GenerateContent 返回 HTTP 403、协议错误码 7: " + verifyRequiredMessage
	twoDaysAgo := now.Add(-48 * time.Hour)
	registry.record(twoModels, "model-a", aistudio.ChannelPlayground, verify, "", now.Add(-72*time.Hour))
	// 同一账户另一模型更晚的一次：整户期限按最近一次算
	registry.record(twoModels, "model-b", aistudio.ChannelBuild, verify, "", twoDaysAgo)
	registry.record(oneModel, "model-a", aistudio.ChannelPlayground, verify, "", twoDaysAgo)
	registry.record(old, "model-a", aistudio.ChannelPlayground, verify, "", now.Add(-31*24*time.Hour))
	registry.record(succeeded, "model-a", aistudio.ChannelPlayground, verify, "", twoDaysAgo)
	registry.record(succeeded, "model-b", aistudio.ChannelPlayground, verify, "", twoDaysAgo)
	registry.record(plain, "model-a", aistudio.ChannelPlayground, "The caller does not have permission", "", twoDaysAgo)
	registry.record(pausedTwo, "model-a", aistudio.ChannelPlayground, verify, "", twoDaysAgo)
	registry.record(pausedTwo, "model-b", aistudio.ChannelPlayground, verify, "", twoDaysAgo)
	registry.record(pausedOne, "model-a", aistudio.ChannelPlayground, verify, "", twoDaysAgo)
	registry.record(existing, "model-a", aistudio.ChannelPlayground, verify, "", twoDaysAgo)
	registry.record(future, "model-a", aistudio.ChannelPlayground, verify, "", now.Add(24*time.Hour))
	registry.record(channels, "model-a", aistudio.ChannelPlayground, verify, "", twoDaysAgo)
	registry.record(channels, "model-a", aistudio.ChannelBuild, verify, "", twoDaysAgo)
	registry.record(casing, "model-a", aistudio.ChannelPlayground, verify, "", twoDaysAgo)
	registry.record(casing, "MODEL-A", aistudio.ChannelBuild, verify, "", twoDaysAgo)
	// succeeded：403 之后（重启前）有一次成功推进了全局作用域的检查时间（同一进程里登记会被清除，这里模拟登记没来得及清除）
	if _, err := pool.MarkModelAccessVerifiedIfGeneration(succeeded, testRuntimeModel, pool.ModelAccessGeneration(succeeded), now.Add(-24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	pauseVerifyRequired(t, pool, pausedTwo)
	pauseVerifyRequired(t, pool, pausedOne)
	pauseVerifyRequired(t, pool, orphan)
	pausedTwoBefore, _ := globalPauseOf(t, pool, pausedTwo)
	pausedOneBefore, _ := globalPauseOf(t, pool, pausedOne)
	existingUntil := now.Add(10 * 24 * time.Hour)
	if written, _, err := pool.MarkModelVerifyCooldownIfGeneration(existing, "model-a", pool.ModelAccessGeneration(existing), now.Add(-time.Hour), existingUntil); err != nil || written != 2 {
		t.Fatalf("预置单个模型冷却: written=%d err=%v", written, err)
	}

	counts := service.reconcileVerifyRequiredPauses(now)
	want := verifyReconcileCounts{moved: 1, kept: 1, accountWritten: 1, modelWritten: 4, modelExisting: 1, orphan: 1, expired: 1, stale: 1}
	if counts != want {
		t.Fatalf("计数 = %+v，期望 %+v，日志 = %v", counts, want, serviceLogMessages(requests))
	}
	if !hasLogContaining(requests, "启动时整理需要验证账号的暂停 | 整户暂停改为单个模型=1 | 整户暂停保留=1 | 补写整户暂停=1 | 补写单个模型=4 | 已有单个模型=1"+
		" | 没有登记的整户暂停=1 | 最近一次 403 超过 30 天=1 | 之后有更新的记录=1 | 失败=0") {
		t.Fatalf("应记录汇总，日志 = %v", serviceLogMessages(requests))
	}
	// 两个模型：补写整户暂停，期限按最近一次
	if cooldown, ok := globalPauseOf(t, pool, twoModels); !ok || cooldown.Reason != verifyRequiredPauseReason ||
		!cooldown.Until.Equal(twoDaysAgo.Add(verifyRequiredPause).UTC()) {
		t.Fatalf("两个模型的账户应补写整户暂停到最近一次 403 后 30 天: %+v（存在=%t）", cooldown, ok)
	}
	// 一个模型：只暂停这个模型（两个键），不暂停整户
	for _, item := range []struct {
		account string
		until   time.Time
	}{
		{oneModel, twoDaysAgo.Add(verifyRequiredPause)}, {future, now.Add(verifyRequiredPause)},
		{channels, twoDaysAgo.Add(verifyRequiredPause)}, {casing, twoDaysAgo.Add(verifyRequiredPause)},
	} {
		if cooldown, ok := globalPauseOf(t, pool, item.account); ok {
			t.Fatalf("%s 只在一个模型上要求验证，不应整户暂停: %+v", item.account, cooldown)
		}
		playground, build := modelVerifyOf(t, pool, item.account, "model-a")
		if item.account == casing {
			// 登记键原样保留：大小写不同的写法取较晚的一个
			if playground.Reason != modelVerifyRequiredReason {
				playground, build = modelVerifyOf(t, pool, item.account, "MODEL-A")
			}
		}
		if playground.Reason != modelVerifyRequiredReason || build.Reason != modelVerifyRequiredReason ||
			!playground.Until.Equal(item.until.UTC()) || !build.Until.Equal(item.until.UTC()) {
			t.Fatalf("%s 应在模型的两个键上补写单个模型冷却到 %s: playground=%+v build=%+v", item.account, item.until, playground, build)
		}
	}
	// 整户暂停 + 一个模型：改为单个模型，恢复时间沿用原暂停，全局暂停删除
	if cooldown, ok := globalPauseOf(t, pool, pausedOne); ok {
		t.Fatalf("pausedOne 的整户暂停应删除: %+v", cooldown)
	}
	if playground, build := modelVerifyOf(t, pool, pausedOne, "model-a"); playground.Reason != modelVerifyRequiredReason ||
		build.Reason != modelVerifyRequiredReason || !playground.Until.Equal(pausedOneBefore.Until) || !build.Until.Equal(pausedOneBefore.Until) {
		t.Fatalf("pausedOne 应改为 model-a 上的单个模型冷却、恢复时间沿用 %s: %+v %+v", pausedOneBefore.Until, playground, build)
	}
	// 整户暂停 + 两个模型、没有登记的整户暂停：保留，不改
	if cooldown, ok := globalPauseOf(t, pool, pausedTwo); !ok || !cooldown.Until.Equal(pausedTwoBefore.Until) {
		t.Fatalf("pausedTwo 的整户暂停应保留: %+v", cooldown)
	}
	if cooldown, ok := globalPauseOf(t, pool, orphan); !ok || cooldown.Reason != verifyRequiredPauseReason {
		t.Fatalf("没有登记的整户暂停应保留: %+v", cooldown)
	}
	// 已有单个模型冷却：不动
	if playground, _ := modelVerifyOf(t, pool, existing, "model-a"); !playground.Until.Equal(existingUntil.UTC()) {
		t.Fatalf("已有的单个模型冷却不应重写: %+v", playground)
	}
	for _, accountID := range []string{old, succeeded, plain} {
		if cooldown, ok := globalPauseOf(t, pool, accountID); ok {
			t.Fatalf("%s 不应补写暂停，实际 %+v", accountID, cooldown)
		}
		if playground, _ := modelVerifyOf(t, pool, accountID, "model-a"); playground.Reason == modelVerifyRequiredReason {
			t.Fatalf("%s 不应补写单个模型冷却，实际 %+v", accountID, playground)
		}
	}
	reasons := pool.UnschedulableSet(ids)
	for _, accountID := range []string{twoModels, pausedTwo, orphan} {
		if reasons[accountID] != aistudio.UnschedulableVerifyRequired {
			t.Fatalf("%s 应不可调度（需要验证账号）: %v", accountID, reasons)
		}
	}
	if len(reasons) != 3 {
		t.Fatalf("只在一个模型上要求验证的账户照常可调度: %v", reasons)
	}
	groups, err := pool.ClassifyCandidates(ctx, aistudio.AccountSelection{ModelID: testRuntimeModel, Method: "generateContent"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	standby := map[string]bool{}
	for _, accountID := range groups.StandbyReady {
		standby[accountID] = true
	}
	if standby[twoModels] || !standby[oneModel] || !standby[pausedOne] {
		t.Fatalf("整户暂停的不是候选，单个模型的对其他模型仍是候选: %v", groups.StandbyReady)
	}
	// 再次启动：只查内存，不再写入
	again := service.reconcileVerifyRequiredPauses(time.Now())
	if again.moved+again.accountWritten+again.modelWritten != 0 || again.modelExisting != 6 || again.kept != 2 {
		t.Fatalf("再次运行不应写入: %+v", again)
	}
	// future 的检查时间截到现在：清除 403 记录解除单个模型暂停后，之后按现在的租约时间写入的冷却仍能写入
	if lifted := service.liftVerifyRequiredPauses([]string{future}, false, time.Now()); lifted != 1 {
		t.Fatalf("清除 403 记录应解除 future 的单个模型暂停，实际 %d", lifted)
	}
	if !hasLogContaining(requests, "解除单个模型需要验证的暂停，该模型重新参与调度") {
		t.Fatalf("应写单个模型的解除行，日志 = %v", serviceLogMessages(requests))
	}
	write, err := pool.TryMarkCooldownIfGeneration(future, "model-a", pool.ModelAccessGeneration(future), time.Now(), time.Now().Add(time.Hour), "每日限额")
	if err != nil || write != aistudio.CooldownWritten {
		t.Fatalf("解除后再写模型冷却应写入: 结果=%v err=%v", write, err)
	}
}
