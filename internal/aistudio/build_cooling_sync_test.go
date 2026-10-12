package aistudio

import (
	"fmt"
	"testing"
	"time"
)

// TestDerivedCoolingTracksThrottledPlaygroundIndex 账户多于 playgroundIndexThrottleAccounts 时 Playground 模型索引最多每秒重建一次。
// 另一账户的 Playground 目录变化会改变某账户 Build 通道能否调用模型（Ultra 专用模型出现在 Playground 后，Free 账户的 Build 通道
// 对它不再可用），状态口径的冷却期限（候选索引 coolUntil）与含 Build 通道的候选集按目录版本重算时必须以同一版本的索引为准：
// 把节流中的旧索引烙进缓存后，状态摘要会一直与 StatusOf 不一致，直到下一次目录变化
func TestDerivedCoolingTracksThrottledPlaygroundIndex(t *testing.T) {
	const accounts = playgroundIndexThrottleAccounts + 5
	catalog := indexTestCatalog()
	// 初始目录都没有 Ultra 专用模型
	withoutUltraOnly := catalog[:3]
	initial := make([]*Account, 0, accounts)
	for index := 0; index < accounts; index++ {
		initial = append(initial, indexTestAccount(fmt.Sprintf("cool-%03d@example.com", index), BenefitTierFree, withoutUltraOnly))
	}
	pool := NewAccountPool(initial, 2)
	pool.SetUpstreamChannels([]Channel{ChannelPlayground, ChannelBuild})
	victim, other, another := initial[7].ID, initial[11].ID, initial[13].ID
	// 受害账户的 Build 目录能生成 Ultra 专用模型：Playground 里没有该模型时 Build 通道可用，它在该模型上的 Build 冷却即状态口径的冷却
	buildCatalog := []Model{{ID: indexTestUltraOnly, Methods: []string{"generateContent"}, Capabilities: map[string]bool{"chat_model": true}}}
	if err := pool.SetBuildCatalog(victim, buildCatalog); err != nil {
		t.Fatal(err)
	}
	until := time.Now().Add(time.Hour)
	scope := ChannelCooldownScope(ChannelBuild, indexTestUltraOnly)
	if err := pool.MarkCooldownIfGeneration(victim, scope, pool.ModelAccessGeneration(victim), time.Now(), until, DailyQuotaKind); err != nil {
		t.Fatal(err)
	}
	countsOf := func(step string) AccountStateCounts {
		t.Helper()
		counts, _ := pool.PoolStateCounts()
		if counts.Total != accounts {
			t.Fatalf("%s：状态计数 = %+v，期望 %d 个账户", step, counts, accounts)
		}
		return counts
	}
	stateOf := func(step string) AccountState {
		t.Helper()
		status, ok := pool.StatusOf(victim)
		if !ok {
			t.Fatalf("%s：账户 %s 不存在", step, victim)
		}
		return status.State
	}
	buildSelection := AccountSelection{ModelID: indexTestUltraOnly, Method: "generateContent", BuildOnly: true}
	buildCandidates := func(step string) int {
		t.Helper()
		groups, err := pool.ClassifyCandidatesCached(buildSelection, nil)
		if err != nil {
			t.Fatalf("%s：分类 Build 候选失败: %v", step, err)
		}
		return len(groups.StandbyReady) + len(groups.StandbyBusy)
	}
	if counts := countsOf("初始"); counts.Cooldown != 1 || stateOf("初始") != AccountCooldown {
		t.Fatalf("初始状态计数 = %+v，StatusOf = %s，期望受害账户为 cooldown", counts, stateOf("初始"))
	}
	// 受害账户是 Build 通道上该模型唯一的候选（冷却中）
	if got := buildCandidates("初始"); got != 0 {
		t.Fatalf("初始 Build 候选 = %d，期望冷却中的受害账户不在可用候选里", got)
	}

	// 让节流窗口过去，再让 Playground 索引刚刚重建：另一账户目录变化后 StatusOf 触发按需重建
	time.Sleep(playgroundIndexRebuildInterval + 100*time.Millisecond)
	if err := pool.SetCatalog(other, BenefitTierFree, withoutUltraOnly[:2]); err != nil {
		t.Fatal(err)
	}
	if state := stateOf("索引重建后"); state != AccountCooldown {
		t.Fatalf("索引重建后 StatusOf = %s，期望仍为 cooldown", state)
	}
	// 节流窗口内：第三个账户的 Playground 目录加入 Ultra 专用模型，Free 账户的 Build 通道对该模型不再可用，受害账户不再冷却
	if err := pool.SetCatalog(another, BenefitTierFree, catalog); err != nil {
		t.Fatal(err)
	}
	immediate := countsOf("目录变化后立即")
	immediateCandidates := buildCandidates("目录变化后立即")
	time.Sleep(playgroundIndexRebuildInterval + 100*time.Millisecond)
	if state := stateOf("Playground 索引重建后"); state != AccountReady {
		t.Fatalf("Playground 索引重建后 StatusOf = %s，期望 ready（Build 通道对 Free 账户不再可用，冷却记录不再影响调度）", state)
	}
	// 目录变化后的一秒内允许沿用索引重建前的结论（最多晚一秒生效），之后必须与 StatusOf 一致
	if later := countsOf("Playground 索引重建后"); later.Cooldown != 0 {
		t.Fatalf("状态口径不一致：StatusOf 为 ready，状态计数目录变化后立即为 %+v、Playground 索引重建后仍为 %+v，期望冷却数为 0", immediate, later)
	}
	// 含 Build 通道的候选集同样不能把旧索引烙进去：受害账户的 Build 通道不再支持该模型，它不再是候选
	if later := buildCandidates("Playground 索引重建后"); later != 0 {
		t.Fatalf("Build 候选不一致：目录变化后立即 %d、索引重建后仍为 %d，期望没有候选", immediateCandidates, later)
	}
}
