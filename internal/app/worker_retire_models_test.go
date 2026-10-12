package app

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// TestRetirementTracksEveryCooledModel 同一账号先后在两个模型上都成为死槽位（每个模型的所有启用通道都冷却）：第一个模型的冷却
// 被清除后登记仍要为第二个模型保留，账号的请求释放租约后 Worker 照常淘汰。原先一条登记只记一个模型，第二个模型的登记被丢弃，
// 第一个模型解除冷却时整条登记被放弃，第二个模型上的死槽位只能等 5% 阈值的轮换
func TestRetirementTracksEveryCooledModel(t *testing.T) {
	const otherModel = "other-model"
	manager, pool, requests := partitionTestManager(t, []string{partitionNormalA, partitionNormalB}, nil, 1, 2, 0, 1)
	catalog := []aistudio.Model{
		{ID: testRuntimeModel, Methods: []string{"generateContent"}, Capabilities: map[string]bool{"chat_model": true}},
		{ID: otherModel, Methods: []string{"generateContent"}, Capabilities: map[string]bool{"chat_model": true}},
	}
	for _, accountID := range []string{partitionNormalA, partitionNormalB} {
		if err := pool.SetCatalog(accountID, aistudio.BenefitTierFree, catalog); err != nil {
			t.Fatal(err)
		}
	}
	pool.SetUpstreamChannels([]aistudio.Channel{aistudio.ChannelPlayground, aistudio.ChannelBuild})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	service := retireTestService(ctx, manager, pool, requests)
	// 不在淘汰后补齐预热：A 只在第二个模型上冷却，预热会把它再次启动，干扰对淘汰结果的判断
	manager.afterRetire.Store(nil)
	if _, err := manager.promote(ctx, partitionNormalA, testRuntimeModel); err != nil {
		t.Fatal(err)
	}
	release := holdLease(t, pool, partitionNormalA)
	until := time.Now().Add(2 * time.Hour)
	scopesOf := func(model string) []string {
		return []string{model, aistudio.ChannelCooldownScope(aistudio.ChannelBuild, model)}
	}
	// 两个模型先后在两个通道上都进入每日限额，每次都按 applyQuotaCooldown 的方式登记淘汰
	for _, model := range []string{testRuntimeModel, otherModel} {
		for _, scope := range scopesOf(model) {
			if err := pool.MarkCooldownIfGeneration(partitionNormalA, scope, pool.ModelAccessGeneration(partitionNormalA), time.Now(), until, "每日限额"); err != nil {
				t.Fatal(err)
			}
			service.retireCooledWorker(partitionNormalA, model, aistudio.QuotaCooldown{Until: until, Kind: aistudio.DailyQuotaKind})
		}
	}
	if got := manager.retirementCount(); got != 1 {
		t.Fatalf("同一账号的登记应合并为一条，登记数 = %d", got)
	}
	// 只清除第一个模型的冷却：账号在第二个模型上仍是死槽位
	for _, scope := range scopesOf(testRuntimeModel) {
		if err := pool.ClearCooldownIfGeneration(partitionNormalA, scope, pool.ModelAccessGeneration(partitionNormalA), time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	if !pool.AccountLongCooling(partitionNormalA, otherModel, rotationMinRemaining) {
		t.Fatalf("账号在 %s 上应仍为长时间冷却", otherModel)
	}
	time.Sleep(400 * time.Millisecond)
	if got := manager.retirementCount(); got != 1 {
		t.Fatalf("第一个模型解除冷却后登记应为第二个模型保留，登记数 = %d", got)
	}
	release()
	if !waitUntil(3*time.Second, func() bool { return !slices.Contains(manager.WarmAccountIDs(), partitionNormalA) }) {
		t.Fatalf("第二个模型 %s 上的死槽位应在租约释放后淘汰，登记=%d，驻留=%v", otherModel, manager.retirementCount(), manager.WarmAccountIDs())
	}
	if !waitUntil(3*time.Second, func() bool { return hasLogContaining(requests, "WAA Worker 冷却淘汰", "范围="+otherModel) }) {
		t.Fatalf("淘汰日志应只写明仍在冷却的模型，日志 = %v", serviceLogMessages(requests))
	}
}
