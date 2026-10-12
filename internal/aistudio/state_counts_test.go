package aistudio

import (
	"context"
	"testing"
	"time"
)

// TestStateCountsIncludeModelCooldowns 状态摘要与账户状态按调度口径计入冷却：全局 "*" 冷却，或某个模型在账户可用的每个启用通道上
// 都冷却时为 cooldown；只有一个通道冷却、另一个通道还能用时仍为就绪；忙碌优先于冷却；过期冷却不计。
// 原先只看全局冷却，每日限额按模型与通道记录时状态页的冷却计数一直为 0
func TestStateCountsIncludeModelCooldowns(t *testing.T) {
	const normalC = "normal-c@example.com"
	pool := scopeTestPool(t, map[string]BenefitTier{scopeNormalA: BenefitTierFree, scopeNormalB: BenefitTierFree, normalC: BenefitTierFree})
	pool.SetUpstreamChannels([]Channel{ChannelPlayground, ChannelBuild})
	// B 的 Build 目录也支持共享模型；A 只能经 Playground 调用
	if err := pool.SetBuildCatalog(scopeNormalB, []Model{{ID: scopeSharedModel, Methods: []string{"generateContent"}}}); err != nil {
		t.Fatal(err)
	}
	mark := func(accountID string, scope string, until time.Time) {
		t.Helper()
		if err := pool.MarkCooldownIfGeneration(accountID, scope, pool.ModelAccessGeneration(accountID), time.Now(), until, DailyQuotaKind); err != nil {
			t.Fatal(err)
		}
	}
	stateOf := func(accountID string) AccountState {
		t.Helper()
		status, ok := pool.StatusOf(accountID)
		if !ok {
			t.Fatalf("账户 %s 不存在", accountID)
		}
		return status.State
	}
	expectCounts := func(step string, ready int, busy int, cooldown int) {
		t.Helper()
		counts := pool.StateCounts()
		if counts.Total != 3 || counts.Ready != ready || counts.Busy != busy || counts.Cooldown != cooldown {
			t.Fatalf("%s：状态计数 = %+v，期望就绪 %d、忙碌 %d、冷却 %d", step, counts, ready, busy, cooldown)
		}
	}
	expectCounts("初始", 3, 0, 0)

	until := time.Now().Add(2 * time.Hour)
	// A 只能经 Playground 调用：Playground 冷却即整体冷却
	mark(scopeNormalA, scopeSharedModel, until)
	if state := stateOf(scopeNormalA); state != AccountCooldown {
		t.Fatalf("A 唯一可用通道冷却后状态 = %s，期望 cooldown", state)
	}
	expectCounts("A 冷却", 2, 0, 1)

	// B 两个通道都能用：只冷却 Playground 时 Build 还能继续，仍为就绪；只启用 Playground 时则为冷却
	mark(scopeNormalB, scopeSharedModel, until)
	if state := stateOf(scopeNormalB); state != AccountReady {
		t.Fatalf("B 只有 Playground 冷却时状态 = %s，期望 ready", state)
	}
	pool.SetUpstreamChannels([]Channel{ChannelPlayground})
	if state := stateOf(scopeNormalB); state != AccountCooldown {
		t.Fatalf("只启用 Playground 时 B 状态 = %s，期望 cooldown", state)
	}
	pool.SetUpstreamChannels([]Channel{ChannelPlayground, ChannelBuild})
	expectCounts("B 单通道冷却", 2, 0, 1)
	mark(scopeNormalB, ChannelCooldownScope(ChannelBuild, scopeSharedModel), until)
	if state := stateOf(scopeNormalB); state != AccountCooldown {
		t.Fatalf("B 两个通道都冷却后状态 = %s，期望 cooldown", state)
	}
	expectCounts("B 双通道冷却", 1, 0, 2)

	// 忙碌优先于冷却：A 有活动租约时显示忙碌，释放后恢复冷却
	lease, err := pool.AcquireFor(context.Background(), AccountSelection{AccountID: scopeNormalA})
	if err != nil {
		t.Fatal(err)
	}
	if state := stateOf(scopeNormalA); state != AccountBusy {
		t.Fatalf("A 有活动租约时状态 = %s，期望 busy", state)
	}
	expectCounts("A 忙碌", 1, 1, 1)
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
	expectCounts("A 释放", 1, 0, 2)

	// C 的全局冷却很快过期：期间为冷却，过期后恢复就绪
	mark(normalC, "", time.Now().Add(80*time.Millisecond))
	if state := stateOf(normalC); state != AccountCooldown {
		t.Fatalf("C 全局冷却时状态 = %s，期望 cooldown", state)
	}
	expectCounts("C 全局冷却", 0, 0, 3)
	time.Sleep(120 * time.Millisecond)
	if state := stateOf(normalC); state != AccountReady {
		t.Fatalf("C 全局冷却过期后状态 = %s，期望 ready", state)
	}
	expectCounts("C 冷却过期", 1, 0, 2)
}
