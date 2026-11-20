package aistudio

import (
	"context"
	"errors"
	"slices"
	"sort"
	"testing"
	"time"
)

// 权益规则（ULTRA_ONLY_MODELS、PAID_ONLY_MODELS，见 tier_rules.go）的测试

const (
	tierFree    = "tier-free@example.com"
	tierPlus    = "tier-plus@example.com"
	tierPro     = "tier-pro@example.com"
	tierUltra   = "tier-ultra@example.com"
	tierCarbon  = "gemini-4-carbon"
	tierArgon   = "gemini-4-argon"
	tierPreview = "gemini-3.1-pro-preview"
)

// tierModel 返回可生成的目录项；modes 为目录的访问模式（为空表示没有标注权益限制）
func tierModel(id string, modes ...int64) Model {
	return Model{ID: id, Methods: []string{"generateContent"}, Capabilities: map[string]bool{"chat_model": true}, AccessModes: modes}
}

// tierPool 按 tiers 建账户池，每个账户的 Playground 目录为 catalog(权益)
func tierPool(t *testing.T, tiers map[string]BenefitTier, catalog func(BenefitTier) []Model) *AccountPool {
	t.Helper()
	pool := scopeTestPool(t, tiers)
	for id, tier := range tiers {
		if err := pool.SetCatalog(id, tier, catalog(tier)); err != nil {
			t.Fatal(err)
		}
	}
	return pool
}

// tierAcquired 连续取 rounds 次租约（每次立即释放），返回取到的账户（与通道）；取不到时返回错误
func tierAcquired(t *testing.T, pool *AccountPool, scope PoolScope, modelID string, rounds int) (map[string]bool, error) {
	t.Helper()
	seen := make(map[string]bool)
	for range rounds {
		lease, err := pool.AcquireFor(scopeContext(scope), AccountSelection{ModelID: modelID, Method: "generateContent"})
		if err != nil {
			return seen, err
		}
		seen[lease.Account().ID] = true
		if err := lease.Release(); err != nil {
			t.Fatal(err)
		}
	}
	return seen, nil
}

func tierSorted(set map[string]bool) []string {
	keys := make([]string, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// tierListed 返回号池模型列表（与 GET /v1/models、/ultra/v1/models 同口径）里的模型 ID
func tierListed(pool *AccountPool, scope PoolScope, models ...Model) []string {
	var ids []string
	for _, model := range pool.EligibleModelsIn(scope, models) {
		ids = append(ids, model.ID)
	}
	return ids
}

// TestTierRuleUltraOnlyExcludesPro ULTRA_INCLUDE_PRO=true、目录没有给 carbon 标注 [4]：Ultra 专用规则下 /ultra 的 carbon 只取 Ultra 账户，
// argon 照常两个账户都取；预热排序（ServeableCounts）给 Pro 计 1（只有 argon）、Ultra 计 2；只有 Pro 的 Ultra 号池不列出 carbon
func TestTierRuleUltraOnlyExcludesPro(t *testing.T) {
	tiers := map[string]BenefitTier{tierPro: BenefitTierPro, tierUltra: BenefitTierUltra}
	catalog := func(BenefitTier) []Model { return []Model{tierModel(tierArgon), tierModel(tierCarbon)} }
	pool := tierPool(t, tiers, catalog)
	pool.SetUltraIncludePro(true)
	if !pool.SetModelTierRules([]string{tierCarbon}, nil) {
		t.Fatal("首次设置规则应返回已变化")
	}
	if seen, err := tierAcquired(t, pool, PoolScopeUltra, tierCarbon, 6); err != nil || !slices.Equal(tierSorted(seen), []string{tierUltra}) {
		t.Fatalf("carbon 取到 %v（%v），期望只有 Ultra 账户", tierSorted(seen), err)
	}
	if seen, err := tierAcquired(t, pool, PoolScopeUltra, tierArgon, 6); err != nil || len(seen) != 2 {
		t.Fatalf("argon 取到 %v（%v），期望 Pro 与 Ultra 都参与", tierSorted(seen), err)
	}
	counts := pool.ServeableCounts([]string{tierPro, tierUltra}, []string{tierArgon, tierCarbon}, 0)
	if counts[tierPro] != 1 || counts[tierUltra] != 2 {
		t.Fatalf("ServeableCounts = %v，期望 Pro 1、Ultra 2", counts)
	}
	if spare := pool.SpareAccounts(PoolScopeUltra, map[string]struct{}{tierUltra: {}}, []string{tierCarbon}, 0, 10); spare != 0 {
		t.Fatalf("排除 Ultra 账户后 carbon 的备用账号 = %d，期望 0（Pro 不能服务 carbon）", spare)
	}
	if listed := tierListed(pool, PoolScopeUltra, catalog(BenefitTierUltra)...); !slices.Equal(listed, []string{tierArgon, tierCarbon}) {
		t.Fatalf("有 Ultra 账户时 /ultra 模型列表 = %v", listed)
	}

	onlyPro := tierPool(t, map[string]BenefitTier{tierPro: BenefitTierPro}, catalog)
	onlyPro.SetUltraIncludePro(true)
	onlyPro.SetModelTierRules([]string{tierCarbon}, nil)
	if listed := tierListed(onlyPro, PoolScopeUltra, catalog(BenefitTierPro)...); !slices.Equal(listed, []string{tierArgon}) {
		t.Fatalf("只有 Pro 的 Ultra 号池模型列表 = %v，期望只有 argon", listed)
	}
	if seen, err := tierAcquired(t, onlyPro, PoolScopeUltra, tierCarbon, 1); err == nil || len(seen) != 0 || !errors.Is(err, ErrNoEligibleAccount) {
		t.Fatalf("只有 Pro 时 carbon 取到 %v（%v），期望没有符合条件的账户", tierSorted(seen), err)
	}
}

// TestTierRuleBuildOnly 只在 Build 目录里的模型（没有 Playground 行，目录也就没有访问模式）：规则按选择的模型 ID 原样匹配，
// Build 分支同样只取 Ultra 账户；只有 Pro 的 Ultra 号池不列出它，Playground 目录里的模型不受影响
func TestTierRuleBuildOnly(t *testing.T) {
	tiers := map[string]BenefitTier{tierPro: BenefitTierPro, tierUltra: BenefitTierUltra}
	setup := func(tiers map[string]BenefitTier) *AccountPool {
		pool := tierPool(t, tiers, func(BenefitTier) []Model { return []Model{tierModel(scopeSharedModel)} })
		pool.SetUpstreamChannels([]Channel{ChannelPlayground, ChannelBuild})
		for id := range tiers {
			if err := pool.SetBuildCatalog(id, []Model{tierModel(tierArgon), tierModel(tierCarbon)}); err != nil {
				t.Fatal(err)
			}
		}
		pool.SetUltraIncludePro(true)
		pool.SetModelTierRules([]string{tierCarbon}, nil)
		return pool
	}
	pool := setup(tiers)
	if seen, err := tierAcquired(t, pool, PoolScopeUltra, tierCarbon, 6); err != nil || !slices.Equal(tierSorted(seen), []string{tierUltra}) {
		t.Fatalf("Build 独有的 carbon 取到 %v（%v），期望只有 Ultra 账户", tierSorted(seen), err)
	}
	if seen, err := tierAcquired(t, pool, PoolScopeUltra, tierArgon, 6); err != nil || len(seen) != 2 {
		t.Fatalf("Build 独有的 argon 取到 %v（%v），期望两个账户都参与", tierSorted(seen), err)
	}
	if counts := pool.ServeableCounts([]string{tierPro, tierUltra}, []string{tierArgon, tierCarbon}, 0); counts[tierPro] != 1 || counts[tierUltra] != 2 {
		t.Fatalf("ServeableCounts = %v，期望 Pro 1、Ultra 2", counts)
	}
	onlyPro := setup(map[string]BenefitTier{tierPro: BenefitTierPro})
	listed := tierListed(onlyPro, PoolScopeUltra, tierModel(tierArgon), tierModel(tierCarbon), tierModel(scopeSharedModel))
	if !slices.Equal(listed, []string{tierArgon, scopeSharedModel}) {
		t.Fatalf("只有 Pro 的 Ultra 号池模型列表 = %v，期望不含 carbon", listed)
	}
	// 规则排除的 Build 独有模型仍按“号池里有这个模型、权益不符”处理，不是目录里没有（不触发目录缺模型的补拉）
	_, err := onlyPro.ClassifyCandidates(scopeContext(PoolScopeUltra), AccountSelection{ModelID: tierCarbon, Method: "generateContent"}, nil)
	if errors.Is(err, ErrModelNotFound) || !onlyPro.ModelListedIn(PoolScopeUltra, tierCarbon) {
		t.Fatalf("规则排除的 Build 独有模型不应按目录中没有处理：err=%v listed=%t", err, onlyPro.ModelListedIn(PoolScopeUltra, tierCarbon))
	}
}

// TestTierRuleHotReloadInvalidates 候选集与模型列表已按旧规则建好后设置、清除规则：下一次选号与列表立即按新规则，
// 号池规则版本每次实际变化加一，相同规则不算变化
func TestTierRuleHotReloadInvalidates(t *testing.T) {
	tiers := map[string]BenefitTier{tierPro: BenefitTierPro, tierUltra: BenefitTierUltra}
	catalog := func(BenefitTier) []Model { return []Model{tierModel(tierArgon), tierModel(tierCarbon)} }
	pool := tierPool(t, tiers, catalog)
	pool.SetUltraIncludePro(true)
	onlyPro := tierPool(t, map[string]BenefitTier{tierPro: BenefitTierPro}, catalog)
	onlyPro.SetUltraIncludePro(true)
	check := func(stage string, wantAccounts []string, wantListed []string) {
		t.Helper()
		seen, err := tierAcquired(t, pool, PoolScopeUltra, tierCarbon, 6)
		if err != nil || !slices.Equal(tierSorted(seen), wantAccounts) {
			t.Fatalf("%s：carbon 取到 %v（%v），期望 %v", stage, tierSorted(seen), err, wantAccounts)
		}
		if listed := tierListed(onlyPro, PoolScopeUltra, catalog(BenefitTierPro)...); !slices.Equal(listed, wantListed) {
			t.Fatalf("%s：只有 Pro 的 Ultra 号池模型列表 = %v，期望 %v", stage, listed, wantListed)
		}
		if counts := pool.ServeableCounts([]string{tierPro}, []string{tierCarbon}, 0); counts[tierPro] != len(wantListed)-1 {
			t.Fatalf("%s：Pro 的 ServeableCounts = %d", stage, counts[tierPro])
		}
	}
	check("没有规则", []string{tierPro, tierUltra}, []string{tierArgon, tierCarbon})
	version := pool.PoolRuleVersion()
	if !pool.SetModelTierRules([]string{tierCarbon}, nil) || !onlyPro.SetModelTierRules([]string{tierCarbon}, nil) {
		t.Fatal("设置规则应返回已变化")
	}
	if pool.PoolRuleVersion() != version+1 {
		t.Fatalf("设置规则后号池规则版本 = %d，期望 %d", pool.PoolRuleVersion(), version+1)
	}
	check("设置规则后", []string{tierUltra}, []string{tierArgon})
	if pool.SetModelTierRules([]string{tierCarbon}, nil) || pool.PoolRuleVersion() != version+1 {
		t.Fatal("相同规则不应算变化")
	}
	if !pool.SetModelTierRules(nil, nil) || !onlyPro.SetModelTierRules(nil, nil) || pool.PoolRuleVersion() != version+2 {
		t.Fatal("清除规则应返回已变化并递增号池规则版本")
	}
	check("清除规则后", []string{tierPro, tierUltra}, []string{tierArgon, tierCarbon})
}

// TestTierRuleNeverWidens 规则只收窄：目录标注 [4] 的模型写进 PAID_ONLY_MODELS（允许 Pro）后仍只有 Ultra 能用；
// 目录标注 [3] 的模型不在任何列表里时 Free 照旧不能用
func TestTierRuleNeverWidens(t *testing.T) {
	tiers := map[string]BenefitTier{tierFree: BenefitTierFree, tierPro: BenefitTierPro, tierUltra: BenefitTierUltra}
	pool := tierPool(t, tiers, func(BenefitTier) []Model {
		return []Model{tierModel(tierCarbon, 4), tierModel(tierPreview, 3)}
	})
	pool.SetModelTierRules(nil, []string{tierCarbon})
	if seen, err := tierAcquired(t, pool, PoolScopeAll, tierCarbon, 6); err != nil || !slices.Equal(tierSorted(seen), []string{tierUltra}) {
		t.Fatalf("目录 [4] + 付费规则：carbon 取到 %v（%v），期望只有 Ultra", tierSorted(seen), err)
	}
	if seen, err := tierAcquired(t, pool, PoolScopeAll, tierPreview, 6); err != nil || !slices.Equal(tierSorted(seen), []string{tierPro, tierUltra}) {
		t.Fatalf("目录 [3]：pro-preview 取到 %v（%v），期望 Pro 与 Ultra", tierSorted(seen), err)
	}
}

// TestTierRuleAliasAndCase 规则里的名称与目录项的别名相同即命中，不区分大小写，models/ 前缀忽略；按别名请求同样排除
func TestTierRuleAliasAndCase(t *testing.T) {
	tiers := map[string]BenefitTier{tierPro: BenefitTierPro, tierUltra: BenefitTierUltra}
	carbon := tierModel(tierCarbon)
	carbon.CapabilityOptions = map[string][]string{"aliases": {"gemini-carbon-latest"}}
	pool := tierPool(t, tiers, func(BenefitTier) []Model { return []Model{carbon, tierModel(tierArgon)} })
	for _, rule := range []string{"GEMINI-CARBON-LATEST", "models/Gemini-4-Carbon"} {
		pool.SetModelTierRules([]string{rule}, nil)
		for _, requested := range []string{tierCarbon, "gemini-carbon-latest"} {
			if seen, err := tierAcquired(t, pool, PoolScopeAll, requested, 6); err != nil || !slices.Equal(tierSorted(seen), []string{tierUltra}) {
				t.Fatalf("规则 %q、请求 %s：取到 %v（%v），期望只有 Ultra", rule, requested, tierSorted(seen), err)
			}
		}
		if seen, err := tierAcquired(t, pool, PoolScopeAll, tierArgon, 6); err != nil || len(seen) != 2 {
			t.Fatalf("规则 %q 不应影响 argon：%v（%v）", rule, tierSorted(seen), err)
		}
	}
	if ultraOnly, _ := pool.ModelTierRules(); !slices.Equal(ultraOnly, []string{"Gemini-4-Carbon"}) {
		t.Fatalf("ModelTierRules = %v，期望写的名称（去掉 models/）", ultraOnly)
	}
}

// TestTierRuleBuildMatchesPlaygroundAlias 规则写 Playground 目录项的别名、只开 Build 通道：经 Build 的选号同样只取 Ultra
// （Build 分支按 Playground 索引里的目录项匹配别名，不是只按选择的模型 ID 原样匹配）；规则写别名之外的名称时 Pro 照常参与
func TestTierRuleBuildMatchesPlaygroundAlias(t *testing.T) {
	tiers := map[string]BenefitTier{tierPro: BenefitTierPro, tierUltra: BenefitTierUltra}
	carbon := tierModel(tierCarbon)
	carbon.CapabilityOptions = map[string][]string{"aliases": {"gemini-carbon-latest"}}
	pool := tierPool(t, tiers, func(BenefitTier) []Model { return []Model{carbon} })
	pool.SetUpstreamChannels([]Channel{ChannelBuild})
	for id := range tiers {
		if err := pool.SetBuildCatalog(id, []Model{tierModel(tierCarbon)}); err != nil {
			t.Fatal(err)
		}
	}
	pool.SetModelTierRules([]string{"gemini-carbon-other"}, nil)
	if seen, err := tierAcquired(t, pool, PoolScopeAll, tierCarbon, 6); err != nil || len(seen) != 2 {
		t.Fatalf("规则不涉及 carbon 时经 Build 取到 %v（%v），期望两个账户都参与", tierSorted(seen), err)
	}
	pool.SetModelTierRules([]string{"Gemini-Carbon-Latest"}, nil)
	lease, err := pool.AcquireFor(scopeContext(PoolScopeAll), AccountSelection{ModelID: tierCarbon, Method: "generateContent"})
	if err != nil || lease.Channel() != ChannelBuild {
		t.Fatalf("应经 Build 取到租约：%v", err)
	}
	_ = lease.Release()
	if seen, err := tierAcquired(t, pool, PoolScopeAll, tierCarbon, 6); err != nil || !slices.Equal(tierSorted(seen), []string{tierUltra}) {
		t.Fatalf("规则写 Playground 别名、经 Build 选号：取到 %v（%v），期望只有 Ultra", tierSorted(seen), err)
	}
}

// TestTierRuleBuildUsesOwnCatalogRow 各账户目录里同一模型的别名不同（Google 逐步更新目录期间）、规则写别名时，Build 分支先按账户自己的
// Playground 目录项判断，与 Playground 分支结论相同：账户池索引里排在前面的目录项（第一个账户的，没有别名）不能让 Pro 经 Build 被选中。
// 账户自己的 Playground 目录没有这个模型时，Build 分支按索引里的目录项（带别名）判断
func TestTierRuleBuildUsesOwnCatalogRow(t *testing.T) {
	const firstFree = "a-free@example.com"
	aliased := tierModel(tierCarbon)
	aliased.CapabilityOptions = map[string][]string{"aliases": {"gemini-carbon-latest"}}
	selection := AccountSelection{ModelID: tierCarbon, Method: "generateContent"}

	t.Run("自己的目录项带别名", func(t *testing.T) {
		// 账户顺序固定：Free 排第一，它的目录项没有别名，账户池索引里 carbon 取的是这一行
		pool := NewAccountPool([]*Account{
			indexTestAccount(firstFree, BenefitTierFree, []Model{tierModel(tierCarbon)}),
			indexTestAccount(tierPro, BenefitTierPro, []Model{aliased}),
			indexTestAccount(tierUltra, BenefitTierUltra, []Model{aliased}),
		}, 2)
		pool.SetUpstreamChannels([]Channel{ChannelPlayground, ChannelBuild})
		for _, id := range []string{tierPro, tierUltra} {
			if err := pool.SetBuildCatalog(id, []Model{tierModel(tierCarbon)}); err != nil {
				t.Fatal(err)
			}
		}
		pool.SetUltraIncludePro(true)
		pool.SetModelTierRules([]string{"Gemini-Carbon-Latest"}, nil)
		pool.mu.Lock()
		indexed, _ := pool.playgroundModelLocked(tierCarbon)
		pro := pool.byID[tierPro]
		playground := pool.channelSupportsLocked(pro, ChannelPlayground, selection)
		build := pool.channelSupportsLocked(pro, ChannelBuild, selection)
		pool.mu.Unlock()
		if len(indexed.CapabilityOptions["aliases"]) != 0 {
			t.Fatalf("前提：索引里的目录项应为第一个账户的、没有别名：%v", indexed.CapabilityOptions)
		}
		if playground || build {
			t.Fatalf("Pro 自己的目录项带规则里的别名：Playground 分支 %v、Build 分支 %v，期望都不支持", playground, build)
		}
		if seen, err := tierAcquired(t, pool, PoolScopeUltra, tierCarbon, 8); err != nil || !slices.Equal(tierSorted(seen), []string{tierUltra}) {
			t.Fatalf("/ultra 的 carbon 取到 %v（%v），期望只有 Ultra（Pro 不能经 Build 被选中）", tierSorted(seen), err)
		}
	})

	t.Run("自己的目录没有这个模型", func(t *testing.T) {
		// Pro 只经 Build 提供 carbon（Playground 目录里没有），Ultra 的 Playground 目录项带别名：Build 分支按索引里的目录项命中规则
		pool := NewAccountPool([]*Account{
			indexTestAccount(tierPro, BenefitTierPro, []Model{tierModel(tierArgon)}),
			indexTestAccount(tierUltra, BenefitTierUltra, []Model{aliased}),
		}, 2)
		pool.SetUpstreamChannels([]Channel{ChannelPlayground, ChannelBuild})
		for _, id := range []string{tierPro, tierUltra} {
			if err := pool.SetBuildCatalog(id, []Model{tierModel(tierCarbon)}); err != nil {
				t.Fatal(err)
			}
		}
		pool.SetUltraIncludePro(true)
		pool.SetModelTierRules([]string{"gemini-carbon-latest"}, nil)
		if seen, err := tierAcquired(t, pool, PoolScopeUltra, tierCarbon, 8); err != nil || !slices.Equal(tierSorted(seen), []string{tierUltra}) {
			t.Fatalf("/ultra 的 carbon 取到 %v（%v），期望只有 Ultra（Pro 的 Build 按索引里带别名的目录项判断）", tierSorted(seen), err)
		}
		pool.SetModelTierRules([]string{"gemini-carbon-other"}, nil)
		if seen, err := tierAcquired(t, pool, PoolScopeUltra, tierCarbon, 8); err != nil || len(seen) != 2 {
			t.Fatalf("规则不涉及 carbon 时取到 %v（%v），期望 Pro 与 Ultra 都参与", tierSorted(seen), err)
		}
	})
}

// TestTierRuleChangeWakesWaiter 规则变化后，正在等待账户的请求立即按新规则重查（SetModelTierRules 唤醒等待者）：
// 每账户并发 1，Ultra 被占住时 carbon 请求在等（候选只有忙碌的 Ultra，没有冷却期限，会一直等到账户池变化）；清除规则后立即取到 Pro
func TestTierRuleChangeWakesWaiter(t *testing.T) {
	tiers := map[string]BenefitTier{tierPro: BenefitTierPro, tierUltra: BenefitTierUltra}
	pool := tierPool(t, tiers, func(BenefitTier) []Model { return []Model{tierModel(tierCarbon)} })
	pool.SetPerAccountConcurrency(1)
	pool.SetModelTierRules([]string{tierCarbon}, nil)
	selection := AccountSelection{ModelID: tierCarbon, Method: "generateContent"}
	held, err := pool.AcquireFor(context.Background(), selection)
	if err != nil || held.Account().ID != tierUltra {
		t.Fatalf("占住 Ultra：%v", err)
	}
	defer func() { _ = held.Release() }()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	type result struct {
		account string
		err     error
	}
	got := make(chan result, 1)
	go func() {
		lease, err := pool.AcquireFor(ctx, selection)
		if err != nil {
			got <- result{err: err}
			return
		}
		got <- result{account: lease.Account().ID}
		_ = lease.Release()
	}()
	select {
	case early := <-got:
		t.Fatalf("Ultra 被占住、规则排除 Pro 时请求应等待，却得到 %+v", early)
	case <-time.After(150 * time.Millisecond):
	}
	start := time.Now()
	pool.SetModelTierRules(nil, nil)
	select {
	case outcome := <-got:
		if outcome.err != nil || outcome.account != tierPro {
			t.Fatalf("清除规则后等待中的请求：%+v，期望取到 Pro", outcome)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("清除规则 %s 后等待中的请求仍没有重查（没有唤醒等待者）", time.Since(start))
	}
}

// TestTierRuleAccountMaskCache 规则按账户目录缓存（accountTierMasksLocked）：候选判断读缓存，不逐次按别名匹配；目录替换（新增别名）与
// 规则变化后重算；目录内容相同的账户共享同一份结果
func TestTierRuleAccountMaskCache(t *testing.T) {
	const otherPro = "tier-pro-2@example.com"
	tiers := map[string]BenefitTier{tierPro: BenefitTierPro, otherPro: BenefitTierPro, tierUltra: BenefitTierUltra, tierFree: BenefitTierFree}
	pool := tierPool(t, tiers, func(BenefitTier) []Model { return []Model{tierModel(tierCarbon), tierModel(tierArgon)} })
	pool.SetModelTierRules([]string{"gemini-carbon-latest"}, nil)
	if seen, err := tierAcquired(t, pool, PoolScopeAll, tierCarbon, 12); err != nil || len(seen) != 4 {
		t.Fatalf("目录里还没有这个别名时规则不命中：取到 %v（%v）", tierSorted(seen), err)
	}
	carbon := tierModel(tierCarbon)
	carbon.CapabilityOptions = map[string][]string{"aliases": {"gemini-carbon-latest"}}
	for id, tier := range tiers {
		if err := pool.SetCatalog(id, tier, []Model{carbon, tierModel(tierArgon)}); err != nil {
			t.Fatal(err)
		}
	}
	if seen, err := tierAcquired(t, pool, PoolScopeAll, tierCarbon, 12); err != nil || !slices.Equal(tierSorted(seen), []string{tierUltra}) {
		t.Fatalf("目录新增别名后：取到 %v（%v），期望只有 Ultra", tierSorted(seen), err)
	}
	pool.mu.Lock()
	cached := pool.byID[tierPro].tierMasks
	ok := len(cached) == 1 && cached[tierCarbon] == ultraOnlyTiers
	if ok {
		// 改动 Pro 账户上的缓存：目录内容相同的另一个 Pro 账户共享同一份，候选判断读的是缓存（逐次按别名匹配时不受影响）
		cached[tierArgon] = ultraOnlyTiers
	}
	pool.mu.Unlock()
	if !ok {
		t.Fatalf("Pro 账户上应缓存 carbon 的规则结果：%v", cached)
	}
	if seen, err := tierAcquired(t, pool, PoolScopeAll, tierArgon, 12); err != nil || !slices.Equal(tierSorted(seen), []string{tierFree, tierUltra}) {
		t.Fatalf("候选判断应读共享的缓存：argon 取到 %v（%v），期望只有 Free 与 Ultra", tierSorted(seen), err)
	}
	pool.SetModelTierRules([]string{tierCarbon}, nil)
	if seen, err := tierAcquired(t, pool, PoolScopeAll, tierArgon, 12); err != nil || len(seen) != 4 {
		t.Fatalf("规则变化后缓存应重算：argon 取到 %v（%v）", tierSorted(seen), err)
	}
}

// TestTierRuleIntersection 同一模型在两份列表里取交集（只有 Ultra），与列表顺序无关；PAID_ONLY_MODELS 允许 Pro 与 Ultra，不含 Plus 与 Free
func TestTierRuleIntersection(t *testing.T) {
	tiers := map[string]BenefitTier{
		tierFree: BenefitTierFree, tierPlus: BenefitTierPlus, tierPro: BenefitTierPro, tierUltra: BenefitTierUltra,
	}
	pool := tierPool(t, tiers, func(BenefitTier) []Model { return []Model{tierModel(tierCarbon), tierModel(tierPreview)} })
	pool.SetModelTierRules([]string{tierCarbon}, []string{tierCarbon, tierPreview})
	if seen, err := tierAcquired(t, pool, PoolScopeAll, tierCarbon, 8); err != nil || !slices.Equal(tierSorted(seen), []string{tierUltra}) {
		t.Fatalf("两份列表都有 carbon：取到 %v（%v），期望只有 Ultra", tierSorted(seen), err)
	}
	if seen, err := tierAcquired(t, pool, PoolScopeAll, tierPreview, 8); err != nil || !slices.Equal(tierSorted(seen), []string{tierPro, tierUltra}) {
		t.Fatalf("付费规则：pro-preview 取到 %v（%v），期望 Pro 与 Ultra（不含 Plus、Free）", tierSorted(seen), err)
	}
}

// TestTierRuleKeepsBuildUnary 规则不改变 Build 是否一次性返回（BuildUsesUnary 只看目录的访问模式）与目录项本身的权益判断
func TestTierRuleKeepsBuildUnary(t *testing.T) {
	tiers := map[string]BenefitTier{tierFree: BenefitTierFree, tierUltra: BenefitTierUltra}
	unmarked, marked := tierModel(tierPreview), tierModel(tierCarbon, 4)
	pool := tierPool(t, tiers, func(BenefitTier) []Model { return []Model{unmarked, marked} })
	before := []bool{BuildUsesUnary(unmarked), BuildUsesUnary(marked), modelAllowedByTier(unmarked, BenefitTierFree)}
	pool.SetModelTierRules([]string{tierCarbon}, []string{tierPreview})
	after := []bool{BuildUsesUnary(unmarked), BuildUsesUnary(marked), modelAllowedByTier(unmarked, BenefitTierFree)}
	if !slices.Equal(before, []bool{false, true, true}) || !slices.Equal(after, before) {
		t.Fatalf("BuildUsesUnary 与目录权益判断：设置规则前 %v，后 %v，期望都是 [false true true]", before, after)
	}
}

// TestWarmWorkerStatesUnservableByRule 规则排除的模型上 Pro Worker 算作不能服务（分区满时可被替换），Ultra Worker 与不在规则里的模型不受影响
func TestWarmWorkerStatesUnservableByRule(t *testing.T) {
	tiers := map[string]BenefitTier{tierPro: BenefitTierPro, tierUltra: BenefitTierUltra}
	pool := tierPool(t, tiers, func(BenefitTier) []Model { return []Model{tierModel(tierArgon), tierModel(tierCarbon)} })
	pool.SetUltraIncludePro(true)
	ids := []string{tierPro, tierUltra}
	if states := pool.WarmWorkerStates(ids, tierCarbon, time.Minute); states[tierPro].Unservable || states[tierUltra].Unservable {
		t.Fatalf("没有规则时都能服务 carbon：%+v", states)
	}
	pool.SetModelTierRules([]string{tierCarbon}, nil)
	states := pool.WarmWorkerStates(ids, tierCarbon, time.Minute)
	if !states[tierPro].Unservable || states[tierUltra].Unservable {
		t.Fatalf("carbon：Pro Unservable=%t、Ultra Unservable=%t，期望 Pro 不能服务", states[tierPro].Unservable, states[tierUltra].Unservable)
	}
	if states := pool.WarmWorkerStates(ids, tierArgon, time.Minute); states[tierPro].Unservable || states[tierUltra].Unservable {
		t.Fatalf("argon 不在规则里：%+v", states)
	}
}

// TestTierRuleLeaseSurvivesHotReload 规则设置之前取得的租约（Build 与 Playground）在规则排除它之后仍能通过发送前的校验：
// 热更新不让进行中的请求失败
func TestTierRuleLeaseSurvivesHotReload(t *testing.T) {
	tiers := map[string]BenefitTier{tierPro: BenefitTierPro}
	for _, channel := range []Channel{ChannelBuild, ChannelPlayground} {
		t.Run(string(channel), func(t *testing.T) {
			pool := tierPool(t, tiers, func(BenefitTier) []Model {
				if channel == ChannelBuild {
					return []Model{tierModel(scopeSharedModel)}
				}
				return []Model{tierModel(tierCarbon)}
			})
			if channel == ChannelBuild {
				pool.SetUpstreamChannels([]Channel{ChannelBuild})
				if err := pool.SetBuildCatalog(tierPro, []Model{tierModel(tierCarbon)}); err != nil {
					t.Fatal(err)
				}
			}
			selection := AccountSelection{ModelID: tierCarbon, Method: "generateContent"}
			lease, err := pool.AcquireFor(scopeContext(PoolScopeAll), selection)
			if err != nil || lease.Channel() != channel {
				t.Fatalf("取租约：%v（通道 %s）", err, lease.Channel())
			}
			defer func() { _ = lease.Release() }()
			pool.SetModelTierRules([]string{tierCarbon}, nil)
			if err := validateLeaseSelection(lease, selection); err != nil {
				t.Fatalf("规则设置前取得的 %s 租约发送前校验失败：%v", channel, err)
			}
			if _, err := tierAcquired(t, pool, PoolScopeAll, tierCarbon, 1); !errors.Is(err, ErrNoEligibleAccount) {
				t.Fatalf("新的选号应按规则没有候选：%v", err)
			}
		})
	}
}

// TestTierRuleStatusCoolingUnchanged 状态口径的冷却不看规则：Pro 账户在规则排除的模型上冷却时，设置规则前后的状态相同
func TestTierRuleStatusCoolingUnchanged(t *testing.T) {
	tiers := map[string]BenefitTier{tierPro: BenefitTierPro}
	pool := tierPool(t, tiers, func(BenefitTier) []Model { return []Model{tierModel(tierCarbon)} })
	now := time.Now()
	if err := pool.MarkCooldownIfGeneration(tierPro, tierCarbon, pool.ModelAccessGeneration(tierPro), now, now.Add(time.Hour), "测试冷却"); err != nil {
		t.Fatal(err)
	}
	state := func() AccountState {
		status, _ := pool.StatusOf(tierPro)
		return status.State
	}
	before := state()
	pool.SetModelTierRules([]string{tierCarbon}, nil)
	if after := state(); before != AccountCooldown || after != before {
		t.Fatalf("状态：设置规则前 %s，后 %s，期望都是冷却", before, after)
	}
}

// TestTierRuleNoCandidateCountsTier 没有候选时的核对把规则排除计入“权益不符”（含 Build 独有模型），模型不在列表里时对外说明权益原因
func TestTierRuleNoCandidateCountsTier(t *testing.T) {
	pool := tierPool(t, map[string]BenefitTier{tierPro: BenefitTierPro}, func(BenefitTier) []Model {
		return []Model{tierModel(tierCarbon)}
	})
	pool.SetUpstreamChannels([]Channel{ChannelPlayground, ChannelBuild})
	if err := pool.SetBuildCatalog(tierPro, []Model{tierModel(tierArgon)}); err != nil {
		t.Fatal(err)
	}
	pool.SetModelTierRules([]string{tierCarbon, tierArgon}, nil)
	for _, modelID := range []string{tierCarbon, tierArgon} {
		report := pool.NoCandidateReport(AccountSelection{ModelID: modelID, Method: "generateContent"})
		if report.Listed != 1 || report.Tier != 1 || report.Unsupported != 0 || report.ModelListed {
			t.Fatalf("%s：核对 %+v，期望权益不符 1、不在模型列表里", modelID, report)
		}
		if reason := report.UnlistedReason(); reason != "no account in this pool has a subscription tier that allows this model" {
			t.Fatalf("%s：对外原因 %q", modelID, reason)
		}
	}
}

// TestTierRuleWarmCoverage 预热排序用的可生成模型数不计规则排除的模型，规则变化后重算
func TestTierRuleWarmCoverage(t *testing.T) {
	pool := tierPool(t, map[string]BenefitTier{tierPro: BenefitTierPro}, func(BenefitTier) []Model {
		return []Model{tierModel(tierArgon), tierModel(tierCarbon)}
	})
	coverage := func() int {
		pool.mu.Lock()
		defer pool.mu.Unlock()
		return pool.accountWarmCoverageLocked(pool.byID[tierPro])
	}
	if got := coverage(); got != 2 {
		t.Fatalf("没有规则时可生成模型数 = %d", got)
	}
	pool.SetModelTierRules([]string{tierCarbon}, nil)
	if got := coverage(); got != 1 {
		t.Fatalf("规则排除 carbon 后可生成模型数 = %d，期望 1", got)
	}
	pool.SetModelTierRules(nil, nil)
	if got := coverage(); got != 2 {
		t.Fatalf("清除规则后可生成模型数 = %d，期望 2", got)
	}
}

// TestTierRuleReport 规则与目录对照：Pro 目录标注 carbon [3]（Pro 可用）或没有标注时提示比目录更严，标注 [4] 时不提示；
// 规则名称不在任何 Playground 或 Build 目录里时列为不匹配；停用账户不计数；还没有账户读到目录时不对照
func TestTierRuleReport(t *testing.T) {
	empty := NewAccountPool(nil, 1)
	empty.SetModelTierRules([]string{tierCarbon}, nil)
	if report := empty.TierRuleReport(); report.Checked || len(report.Unmatched) != 0 {
		t.Fatalf("没有目录时不应对照：%+v", report)
	}
	tiers := map[string]BenefitTier{tierFree: BenefitTierFree, tierPro: BenefitTierPro, tierUltra: BenefitTierUltra}
	modes := map[BenefitTier][]int64{BenefitTierPro: {3}, BenefitTierFree: {4}, BenefitTierUltra: {4}}
	pool := tierPool(t, tiers, func(tier BenefitTier) []Model {
		return []Model{tierModel(tierCarbon, modes[tier]...), tierModel(tierPreview)}
	})
	pool.SetUpstreamChannels([]Channel{ChannelPlayground, ChannelBuild})
	if err := pool.SetBuildCatalog(tierUltra, []Model{tierModel(tierArgon)}); err != nil {
		t.Fatal(err)
	}
	pool.SetModelTierRules([]string{tierCarbon, tierArgon, "gemini-4-carbon-high"}, []string{tierPreview})
	report := pool.TierRuleReport()
	want := []TierRuleHint{
		{Key: TierRuleUltraOnlyKey, Model: tierCarbon, Tier: BenefitTierPro, Marked: true, Accounts: 1},
		{Key: TierRulePaidOnlyKey, Model: tierPreview, Tier: BenefitTierFree, Marked: false, Accounts: 1},
	}
	if !report.Checked || !slices.Equal(report.Hints, want) {
		t.Fatalf("提示 = %+v，期望 %+v", report.Hints, want)
	}
	if !slices.Equal(report.Unmatched, []TierRuleName{{Key: TierRuleUltraOnlyKey, Name: "gemini-4-carbon-high"}}) {
		t.Fatalf("不匹配 = %+v，期望只有带后缀的名称（argon 在 Build 目录里）", report.Unmatched)
	}
	if _, _, err := pool.SetEnabled(tierPro, false); err != nil {
		t.Fatal(err)
	}
	if report := pool.TierRuleReport(); !slices.Equal(report.Hints, want[1:]) {
		t.Fatalf("停用 Pro 账户后提示 = %+v，期望只剩 Free 的", report.Hints)
	}
	pool.SetModelTierRules(nil, nil)
	if report := pool.TierRuleReport(); len(report.Hints) != 0 || len(report.Unmatched) != 0 {
		t.Fatalf("清除规则后不应再提示：%+v", report)
	}
}

// TestTierRuleReportSplitsMarked 同一权益里有的账户目录明确标注了允许、有的只是没有标注（Google 逐步给账户开放、目录陆续刷新期间）：
// 分成两条提示、各自计数，醒目提示（Marked）的账户数只数标注了的账户；同一目录里 ID 与别名分别命中同一规则项时只算一次
func TestTierRuleReportSplitsMarked(t *testing.T) {
	const (
		otherPro = "tier-pro-2@example.com"
		thirdPro = "tier-pro-3@example.com"
	)
	tiers := map[string]BenefitTier{tierPro: BenefitTierPro, otherPro: BenefitTierPro, thirdPro: BenefitTierPro, tierUltra: BenefitTierUltra}
	pool := tierPool(t, tiers, func(BenefitTier) []Model { return []Model{tierModel(tierCarbon)} })
	if err := pool.SetCatalog(tierPro, BenefitTierPro, []Model{tierModel(tierCarbon, 3)}); err != nil {
		t.Fatal(err)
	}
	pool.SetModelTierRules([]string{tierCarbon}, nil)
	want := []TierRuleHint{
		{Key: TierRuleUltraOnlyKey, Model: tierCarbon, Tier: BenefitTierPro, Marked: true, Accounts: 1},
		{Key: TierRuleUltraOnlyKey, Model: tierCarbon, Tier: BenefitTierPro, Marked: false, Accounts: 2},
	}
	if report := pool.TierRuleReport(); !slices.Equal(report.Hints, want) {
		t.Fatalf("部分标注：提示 = %+v，期望 %+v", report.Hints, want)
	}
	// 一个目录里两行分别按 ID 与别名命中同一规则项，其中一行标注了 [3]：这份目录算标注了，账户只计一次
	aliasRow := tierModel("gemini-carbon-preview", 3)
	aliasRow.CapabilityOptions = map[string][]string{"aliases": {tierCarbon}}
	if err := pool.SetCatalog(otherPro, BenefitTierPro, []Model{tierModel(tierCarbon), aliasRow}); err != nil {
		t.Fatal(err)
	}
	want = []TierRuleHint{
		{Key: TierRuleUltraOnlyKey, Model: tierCarbon, Tier: BenefitTierPro, Marked: true, Accounts: 2},
		{Key: TierRuleUltraOnlyKey, Model: tierCarbon, Tier: BenefitTierPro, Marked: false, Accounts: 1},
	}
	if report := pool.TierRuleReport(); !slices.Equal(report.Hints, want) {
		t.Fatalf("目录里两行命中同一规则项：提示 = %+v，期望 %+v", report.Hints, want)
	}
}

// TestTierRuleReportAliasRule 规则写目录别名（大小写混合）：对照按目录项的 ID 与别名找规则名称，不列为不匹配，并照常给出提示
func TestTierRuleReportAliasRule(t *testing.T) {
	tiers := map[string]BenefitTier{tierPro: BenefitTierPro, tierUltra: BenefitTierUltra}
	carbon := tierModel(tierCarbon)
	carbon.CapabilityOptions = map[string][]string{"aliases": {"gemini-carbon-latest"}}
	pool := tierPool(t, tiers, func(BenefitTier) []Model { return []Model{carbon} })
	pool.SetModelTierRules([]string{"Gemini-Carbon-Latest"}, nil)
	report := pool.TierRuleReport()
	if len(report.Unmatched) != 0 {
		t.Fatalf("别名在 Playground 目录里，不应列为不匹配：%+v", report.Unmatched)
	}
	want := []TierRuleHint{{Key: TierRuleUltraOnlyKey, Model: "Gemini-Carbon-Latest", Tier: BenefitTierPro, Marked: false, Accounts: 1}}
	if !slices.Equal(report.Hints, want) {
		t.Fatalf("规则写别名：提示 = %+v，期望 %+v", report.Hints, want)
	}
}

// TestTierRuleReportBuildScan 对照判断规则名称在不在目录里时：名称都在 Playground 目录里找到后不再看 Build 目录；
// 只在 Build 目录里的名称按每个账户的 Build 目录内容找（内容相同的只扫描一次，内容不同的都要看到），Build 目录内容变化后按新内容找，
// 同内容的 Build 目录同步后结果不变
func TestTierRuleReportBuildScan(t *testing.T) {
	tiers := map[string]BenefitTier{tierPro: BenefitTierPro, tierUltra: BenefitTierUltra}
	pool := tierPool(t, tiers, func(BenefitTier) []Model { return []Model{tierModel(tierCarbon, 4)} })
	pool.SetUpstreamChannels([]Channel{ChannelPlayground, ChannelBuild})
	setBuild := func(id string, models ...Model) {
		t.Helper()
		if err := pool.SetBuildCatalog(id, models); err != nil {
			t.Fatal(err)
		}
	}
	setBuild(tierPro, tierModel(tierArgon))
	setBuild(tierUltra, tierModel(tierArgon))
	pool.SetModelTierRules([]string{tierCarbon}, nil)
	if report := pool.TierRuleReport(); !report.Checked || len(report.Unmatched) != 0 {
		t.Fatalf("carbon 在 Playground 目录里：%+v", report)
	}
	pool.mu.Lock()
	scanned := pool.byID[tierPro].buildKeySource != nil || pool.byID[tierUltra].buildKeySource != nil
	pool.mu.Unlock()
	if scanned {
		t.Fatal("规则名称都在 Playground 目录里找到时不应再扫描 Build 目录")
	}
	pool.SetModelTierRules([]string{tierCarbon, "Gemini-Build-Only", "gemini-missing"}, nil)
	unmatched := func(stage string, names ...string) {
		t.Helper()
		var want []TierRuleName
		for _, name := range names {
			want = append(want, TierRuleName{Key: TierRuleUltraOnlyKey, Name: name})
		}
		if report := pool.TierRuleReport(); !slices.Equal(report.Unmatched, want) {
			t.Fatalf("%s：不匹配 = %+v，期望 %+v", stage, report.Unmatched, want)
		}
	}
	unmatched("两个账户的 Build 目录相同", "Gemini-Build-Only", "gemini-missing")
	// Ultra 的 Build 目录内容变了（原先与 Pro 相同）：按新内容找到只在 Build 目录里的名称
	setBuild(tierUltra, tierModel(tierArgon), tierModel("gemini-build-only"))
	unmatched("Ultra 的 Build 目录新增模型后", "gemini-missing")
	// 目录同步：内容不变、换一份切片，对照结果按版本重算后相同
	setBuild(tierPro, tierModel(tierArgon))
	setBuild(tierUltra, tierModel(tierArgon), tierModel("gemini-build-only"))
	unmatched("同内容的目录同步后", "gemini-missing")
}
