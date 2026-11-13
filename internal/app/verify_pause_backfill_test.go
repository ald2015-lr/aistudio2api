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

// TestBackfillVerifyRequiredPauses 启动时为登记里有账户级 403（需要验证账号）、却没有整户暂停的账户补写暂停：期限从最近一次 403 起算
// 30 天；超过 30 天的不补；403 之后全局作用域有更晚记录（任一模型成功）的不补；普通单模型 403 不补；已暂停的不重复写；
// 最近一次 403 晚于现在（时钟回拨）的期限截到 30 天后、检查时间截到现在。补写后账户不可调度（需要验证账号），不是预热候选
func TestBackfillVerifyRequiredPauses(t *testing.T) {
	const (
		recent    = "backfill-recent@example.com"
		old       = "backfill-old@example.com"
		succeeded = "backfill-succeeded@example.com"
		plain     = "backfill-plain@example.com"
		paused    = "backfill-paused@example.com"
		future    = "backfill-future@example.com"
	)
	pool, _ := testAccountPool(t, recent, old, succeeded, plain, paused, future)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	requests := newRequestRegistry(ctx)
	service := &trackedService{lifecycle: ctx, pool: pool, requests: requests, forbidden: newForbiddenTracker()}
	registry := withPermissionDenied(t, service)
	now := time.Now()
	verify := "AI Studio GenerateContent 返回 HTTP 403、协议错误码 7: " + verifyRequiredMessage
	twoDaysAgo := now.Add(-48 * time.Hour)
	registry.record(recent, "model-a", aistudio.ChannelPlayground, verify, "", now.Add(-72*time.Hour))
	// 同一账户另一模型更晚的一次：期限按最近一次算
	registry.record(recent, "model-b", aistudio.ChannelBuild, verify, "", twoDaysAgo)
	registry.record(old, "model-a", aistudio.ChannelPlayground, verify, "", now.Add(-31*24*time.Hour))
	registry.record(succeeded, "model-a", aistudio.ChannelPlayground, verify, "", twoDaysAgo)
	registry.record(plain, "model-a", aistudio.ChannelPlayground, "The caller does not have permission", "", twoDaysAgo)
	registry.record(paused, "model-a", aistudio.ChannelPlayground, verify, "", twoDaysAgo)
	registry.record(future, "model-a", aistudio.ChannelPlayground, verify, "", now.Add(24*time.Hour))
	// succeeded：403 之后（重启前）有一次成功推进了全局作用域的检查时间（同一进程里登记会被清除，这里模拟登记没来得及清除）
	if _, err := pool.MarkModelAccessVerifiedIfGeneration(succeeded, testRuntimeModel, pool.ModelAccessGeneration(succeeded), now.Add(-24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	pauseVerifyRequired(t, pool, paused)
	pausedBefore, _ := globalPauseOf(t, pool, paused)

	if written := service.backfillVerifyRequiredPauses(now); written != 2 {
		t.Fatalf("应补写 2 个账户（recent、future），实际 %d，日志 = %v", written, serviceLogMessages(requests))
	}
	cooldown, ok := globalPauseOf(t, pool, recent)
	if !ok || cooldown.Reason != verifyRequiredPauseReason || !cooldown.Until.Equal(twoDaysAgo.Add(verifyRequiredPause).UTC()) {
		t.Fatalf("recent 应补写到最近一次 403 后 30 天: %+v（存在=%t），期望到期 %s", cooldown, ok, twoDaysAgo.Add(verifyRequiredPause))
	}
	if cooldown, ok = globalPauseOf(t, pool, future); !ok || cooldown.Reason != verifyRequiredPauseReason ||
		!cooldown.Until.Equal(now.Add(verifyRequiredPause).UTC()) {
		t.Fatalf("future 的期限应截到 30 天后: %+v（存在=%t）", cooldown, ok)
	}
	for _, accountID := range []string{old, succeeded, plain} {
		if cooldown, ok := globalPauseOf(t, pool, accountID); ok {
			t.Fatalf("%s 不应补写暂停，实际 %+v", accountID, cooldown)
		}
	}
	if pausedAfter, _ := globalPauseOf(t, pool, paused); !pausedAfter.Until.Equal(pausedBefore.Until) {
		t.Fatalf("已暂停的不应重写: 之前 %s 之后 %s", pausedBefore.Until, pausedAfter.Until)
	}
	if !hasLogContaining(requests, "启动时补写需要验证账号的整户暂停 | 补写=2 | 已暂停=1 | 最近一次 403 超过 30 天=1 | 之后有更新的记录=1 | 失败=0") {
		t.Fatalf("应记录补写汇总，日志 = %v", serviceLogMessages(requests))
	}
	reasons := pool.UnschedulableSet([]string{recent, future, old, succeeded, plain})
	if reasons[recent] != aistudio.UnschedulableVerifyRequired || reasons[future] != aistudio.UnschedulableVerifyRequired || len(reasons) != 2 {
		t.Fatalf("补写的账户应不可调度（需要验证账号），其余照常: %v", reasons)
	}
	groups, err := pool.ClassifyCandidates(ctx, aistudio.AccountSelection{ModelID: testRuntimeModel, Method: "generateContent"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, accountID := range groups.StandbyReady {
		if accountID == recent || accountID == future {
			t.Fatalf("补写暂停的账户不应是预热候选: %v", groups.StandbyReady)
		}
	}
	// 再次启动：已暂停的只查内存，不重复写
	if written := service.backfillVerifyRequiredPauses(time.Now()); written != 0 {
		t.Fatalf("再次补写不应写入，实际 %d", written)
	}
	// future 的检查时间截到补写时的现在，没有写到未来：清除 403 记录解除暂停后（账户仍未验证），之后按现在的租约时间写入的
	// 整户暂停与每日限额仍能写入，不被当作陈旧跳过
	if lifted := service.liftVerifyRequiredPauses([]string{future}, false, time.Now()); lifted != 1 {
		t.Fatalf("清除 403 记录应解除 future 的暂停，实际 %d", lifted)
	}
	for _, reason := range []string{verifyRequiredPauseReason, "每日限额"} {
		write, err := pool.TryMarkCooldownIfGeneration(
			future, "", pool.ModelAccessGeneration(future), time.Now(), time.Now().Add(time.Hour), reason,
		)
		if err != nil || write != aistudio.CooldownWritten {
			t.Fatalf("解除后再写全局冷却（%s）应写入: 结果=%v err=%v", reason, write, err)
		}
	}
}
