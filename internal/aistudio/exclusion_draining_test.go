package aistudio

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"slices"
	"sort"
	"testing"
	"time"
)

// 排除列表、排空标记与可接替账户统计的测试：排除列表在候选索引上跳过账户下标，结果与限定账户集合（原实现）一致；
// 排空中的账户不接不固定账户的新租约，指定账户与文件绑定的请求照常使用

// leaseDiffPool 按种子构造账户池：两次用同一种子得到完全相同的账户、目录与 Build 目录
func leaseDiffPool(t *testing.T, seed int64) (*AccountPool, []string) {
	t.Helper()
	random := rand.New(rand.NewSource(seed))
	catalog := indexTestCatalog()
	randomModels := func() []Model {
		var models []Model
		for _, model := range catalog {
			if model.ID == indexTestUltraOnly && random.Intn(6) != 0 {
				continue
			}
			if random.Intn(4) != 0 {
				models = append(models, model)
			}
		}
		return models
	}
	var accounts []*Account
	var ids []string
	for index := 0; index < 24; index++ {
		id := fmt.Sprintf("lease-%02d@example.com", index)
		tier := []BenefitTier{BenefitTierFree, BenefitTierFree, BenefitTierPro, BenefitTierUltra}[random.Intn(4)]
		accounts = append(accounts, indexTestAccount(id, tier, randomModels()))
		ids = append(ids, id)
	}
	pool := NewAccountPool(accounts, 2)
	pool.SetUpstreamChannels([]Channel{ChannelPlayground, ChannelBuild})
	for _, id := range ids {
		if random.Intn(2) == 0 {
			if err := pool.SetBuildCatalog(id, randomModels()); err != nil {
				t.Fatal(err)
			}
		}
	}
	return pool, ids
}

// TestExclusionLeaseMatchesAllowList 排除列表走候选索引选号，与把排除换成“其余全部账户”的限定集合（逐个账户判断的原路径）
// 在随机租约、释放、冷却与排空之后取得的账户、通道与是否可等待完全一致
func TestExclusionLeaseMatchesAllowList(t *testing.T) {
	seed := time.Now().UnixNano()
	t.Logf("随机种子 %d", seed)
	indexed, ids := leaseDiffPool(t, seed)
	reference, _ := leaseDiffPool(t, seed)
	random := rand.New(rand.NewSource(seed + 1))
	var indexedLeases, referenceLeases []*AccountLease
	defer func() {
		for _, lease := range append(indexedLeases, referenceLeases...) {
			_ = lease.Release()
		}
	}()
	describe := func(lease *AccountLease) string {
		if lease == nil {
			return leaseAccountID(lease)
		}
		return lease.Account().ID + "/" + string(lease.Channel())
	}
	rounds := 3000
	if testing.Short() {
		rounds = 600
	}
	acquired := 0
	for round := 0; round < rounds; round++ {
		now := time.Now()
		switch op := random.Intn(10); {
		case op < 4:
			if len(indexedLeases) == 0 {
				continue
			}
			at := random.Intn(len(indexedLeases))
			if err := errors.Join(indexedLeases[at].Release(), referenceLeases[at].Release()); err != nil {
				t.Fatal(err)
			}
			indexedLeases = slices.Delete(indexedLeases, at, at+1)
			referenceLeases = slices.Delete(referenceLeases, at, at+1)
		case op == 4:
			accountID := ids[random.Intn(len(ids))]
			scope := []string{"", indexTestModel, indexTestFlash, ChannelCooldownScope(ChannelBuild, indexTestModel)}[random.Intn(4)]
			until := now.Add(time.Hour)
			clear := random.Intn(3) == 0
			for _, pool := range []*AccountPool{indexed, reference} {
				var err error
				if clear {
					err = pool.ClearCooldownIfGeneration(accountID, scope, pool.ModelAccessGeneration(accountID), now)
				} else {
					err = pool.MarkCooldownIfGeneration(accountID, scope, pool.ModelAccessGeneration(accountID), now, until, "测试冷却")
				}
				if err != nil {
					t.Fatal(err)
				}
			}
		case op == 5:
			accountID := ids[random.Intn(len(ids))]
			on := random.Intn(2) == 0
			if indexed.SetDraining(accountID, on) != reference.SetDraining(accountID, on) {
				t.Fatalf("第 %d 轮：两个账户池的排空变化不一致", round)
			}
		default:
			selection := AccountSelection{
				ModelID: []string{indexTestModel, indexTestFlash, indexTestFlashAlias, indexTestUltraOnly}[random.Intn(4)],
				Method:  []string{"generateContent", "generateContent", "countTokens"}[random.Intn(3)],
				Pool:    []PoolScope{PoolScopeAll, PoolScopeNormal, PoolScopeUltra}[random.Intn(3)],
			}
			switch random.Intn(5) {
			case 0:
				selection.PreferPlayground = true
			case 1:
				selection.PlaygroundFirst = true
			case 2:
				selection.BuildOnly = true
			}
			var excluded []string
			for count := random.Intn(5); count > 0; count-- {
				excluded = append(excluded, ids[random.Intn(len(ids))])
			}
			if random.Intn(4) == 0 {
				excluded = append(excluded, "missing@example.com")
			}
			allowed := make([]string, 0, len(ids))
			for _, id := range ids {
				if !slices.Contains(excluded, id) {
					allowed = append(allowed, id)
				}
			}
			withExclusion, withAllowList := selection, selection
			withExclusion.ExcludedAccountIDs = excluded
			withAllowList.AllowedAccountIDs = allowed
			indexed.mu.Lock()
			indexedLease, _, indexedWaitable, indexedErr := indexed.tryAcquireLocked(withExclusion, now)
			indexed.mu.Unlock()
			reference.mu.Lock()
			referenceLease, _, referenceWaitable, referenceErr := reference.tryAcquireLocked(withAllowList, now)
			reference.mu.Unlock()
			if !sameError(indexedErr, referenceErr) || indexedWaitable != referenceWaitable || describe(indexedLease) != describe(referenceLease) {
				t.Fatalf("第 %d 轮：选择 %+v 排除 %v：索引=(%s, 可等待=%v, %v) 参考=(%s, 可等待=%v, %v)", round, selection, excluded,
					describe(indexedLease), indexedWaitable, indexedErr, describe(referenceLease), referenceWaitable, referenceErr)
			}
			if indexedLease != nil {
				if slices.Contains(excluded, indexedLease.Account().ID) {
					t.Fatalf("第 %d 轮：租约落在被排除的账户 %s", round, indexedLease.Account().ID)
				}
				if indexed.Draining(indexedLease.Account().ID) {
					t.Fatalf("第 %d 轮：不固定账户的租约落在排空中的账户 %s", round, indexedLease.Account().ID)
				}
				indexedLeases = append(indexedLeases, indexedLease)
				referenceLeases = append(referenceLeases, referenceLease)
				acquired++
			}
		}
	}
	if acquired == 0 {
		t.Fatal("随机过程中没有取得任何租约，测试没有覆盖选号")
	}
	t.Logf("%d 轮中取得 %d 个租约", rounds, acquired)
	indexed.mu.Lock()
	err := indexed.verifySchedulingIndexLocked()
	indexed.mu.Unlock()
	if err != nil {
		t.Fatalf("随机过程后索引不一致: %v", err)
	}
}

// TestExcludedAccountsSkipped 排除列表不参与资格、最早恢复时间与可等待判断；最早恢复时间跳过被排除账户后冷却堆保持完整；
// 指定账户被排除时没有可用账户；context 中的租约账户被排除时同样拒绝
func TestExcludedAccountsSkipped(t *testing.T) {
	catalog := indexTestCatalog()
	pool := NewAccountPool([]*Account{
		indexTestAccount("a@example.com", BenefitTierFree, catalog),
		indexTestAccount("b@example.com", BenefitTierFree, catalog),
		indexTestAccount("c@example.com", BenefitTierFree, catalog),
	}, 2)
	ctx := context.Background()
	now := time.Now()
	earlier, later := now.Add(time.Hour), now.Add(2*time.Hour)
	for accountID, until := range map[string]time.Time{"a@example.com": earlier, "b@example.com": later} {
		if err := pool.MarkCooldownIfGeneration(accountID, "", pool.ModelAccessGeneration(accountID), now, until, "测试"); err != nil {
			t.Fatal(err)
		}
	}
	selection := AccountSelection{ModelID: indexTestModel, Method: "generateContent"}
	excluding := selection
	excluding.ExcludedAccountIDs = []string{"a@example.com", " c@example.com ", "missing@example.com"}
	groups, err := pool.ClassifyCandidates(ctx, excluding, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !groups.Eligible || len(groups.StandbyReady) != 0 || !groups.EarliestCooldown.Equal(later.UTC()) {
		t.Fatalf("排除 a、c 后分类 = %+v，期望只剩冷却中的 b，最早恢复时间为 b 的 %s", groups, later)
	}
	// 跳过被排除账户时弹出的堆项已放回：不排除时最早恢复时间仍是 a 的
	groups, err = pool.ClassifyCandidates(ctx, selection, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(groups.StandbyReady, []string{"c@example.com"}) || !groups.EarliestCooldown.Equal(earlier.UTC()) {
		t.Fatalf("不排除时分类 = %+v，期望 c 可用、最早恢复时间为 a 的 %s", groups, earlier)
	}
	pool.mu.Lock()
	err = pool.verifySchedulingIndexLocked()
	lease, earliest, waitable, acquireErr := pool.tryAcquireLocked(excluding, time.Now())
	pool.mu.Unlock()
	if err != nil {
		t.Fatalf("排除后索引不一致: %v", err)
	}
	if acquireErr != nil || lease != nil || !waitable || !earliest.Equal(later.UTC()) {
		t.Fatalf("排除 a、c 时 tryAcquireLocked = (%s, %s, %v, %v)，期望可等待、最早恢复时间为 %s", leaseAccountID(lease), earliest, waitable, acquireErr, later)
	}
	excludeAll := selection
	excludeAll.ExcludedAccountIDs = []string{"a@example.com", "b@example.com", "c@example.com"}
	if groups, err = pool.ClassifyCandidates(ctx, excludeAll, nil); err != nil {
		t.Fatal(err)
	}
	if groups.Eligible || !groups.EarliestCooldown.IsZero() {
		t.Fatalf("全部排除时分类 = %+v，期望没有候选", groups)
	}
	pool.mu.Lock()
	lease, _, waitable, acquireErr = pool.tryAcquireLocked(excludeAll, time.Now())
	pool.mu.Unlock()
	if acquireErr != nil || lease != nil || waitable {
		t.Fatalf("全部排除时 tryAcquireLocked = (%s, %v, %v)，期望不可等待", leaseAccountID(lease), waitable, acquireErr)
	}
	// 指定账户被排除
	pinned := AccountSelection{ModelID: indexTestModel, Method: "generateContent", AccountID: "c@example.com", ExcludedAccountIDs: []string{"c@example.com"}}
	if _, err := pool.ClassifyCandidates(ctx, pinned, nil); !errors.Is(err, ErrNoEligibleAccount) {
		t.Fatalf("指定账户被排除时分类错误 = %v，期望 ErrNoEligibleAccount", err)
	}
	if _, _, err := pool.TryAcquireFor(ctx, pinned); !errors.Is(err, ErrNoEligibleAccount) {
		t.Fatalf("指定账户被排除时选号错误 = %v，期望 ErrNoEligibleAccount", err)
	}
	// 不排除 c 时取到 c；context 租约的账户被排除时拒绝
	lease, _, err = pool.TryAcquireFor(ctx, withExcluded(excluding, "a@example.com"))
	if err != nil || lease == nil || lease.Account().ID != "c@example.com" {
		t.Fatalf("只排除 a 时租约 = %s %v，期望 c", leaseAccountID(lease), err)
	}
	defer lease.Release()
	if err := validateLeaseSelection(lease, AccountSelection{ExcludedAccountIDs: []string{"c@example.com"}}); !errors.Is(err, ErrNoEligibleAccount) {
		t.Fatalf("context 租约账户被排除时校验 = %v，期望 ErrNoEligibleAccount", err)
	}
	if err := validateLeaseSelection(lease, AccountSelection{ExcludedAccountIDs: []string{"a@example.com"}}); err != nil {
		t.Fatalf("context 租约账户未被排除时校验失败: %v", err)
	}
}

// leaseAccountID 返回租约的账户 ID，没有租约时为“无”
func leaseAccountID(lease *AccountLease) string {
	if lease == nil {
		return "无"
	}
	return lease.Account().ID
}

// withExcluded 返回只排除给定账户的选择
func withExcluded(selection AccountSelection, accountIDs ...string) AccountSelection {
	selection.ExcludedAccountIDs = accountIDs
	return selection
}

// TestExclusionStaysOnIndexedPath 合成 15k 账户号池上排除几个账户的分类仍走候选索引：结果等于不排除的结果去掉这些账户，
// 耗时与不排除时同一量级，远低于把排除换成“其余全部账户”的限定集合（原换号重试的做法）
func TestExclusionStaysOnIndexedPath(t *testing.T) {
	if testing.Short() {
		t.Skip("合成号池压测在 -short 下跳过")
	}
	pool, warm := scalePool(t)
	ctx := context.Background()
	selection := AccountSelection{ModelID: scaleModel, Method: "generateContent", Pool: PoolScopeNormal}
	base, err := pool.ClassifyCandidates(ctx, selection, warm)
	if err != nil {
		t.Fatal(err)
	}
	// 两个热账户、一个备用账户、一个冷却账户与一个不存在的账户
	excluded := []string{warm[0], warm[1], base.StandbyReady[0], "free-00141@example.com", "missing@example.com"}
	withExclusion := withExcluded(selection, excluded...)
	got, err := pool.ClassifyCandidates(ctx, withExclusion, warm)
	if err != nil {
		t.Fatal(err)
	}
	drop := func(values []string) []string {
		return slices.DeleteFunc(slices.Clone(values), func(id string) bool { return slices.Contains(excluded, id) })
	}
	want := base
	want.WarmReady, want.WarmAvailable, want.WarmBusy = drop(base.WarmReady), drop(base.WarmAvailable), drop(base.WarmBusy)
	want.StandbyReady, want.StandbyBusy = drop(base.StandbyReady), drop(base.StandbyBusy)
	if !sameGroups(got, want) {
		t.Fatalf("排除后的分类与不排除的结果去掉排除账户不一致：WarmReady %d/%d StandbyReady %d/%d",
			len(got.WarmReady), len(want.WarmReady), len(got.StandbyReady), len(want.StandbyReady))
	}
	enabled, _ := pool.EnabledAccountsIn(PoolScopeNormal)
	withAllowList := selection
	withAllowList.AllowedAccountIDs = drop(enabled)
	allowGroups, err := pool.ClassifyCandidates(ctx, withAllowList, warm)
	if err != nil {
		t.Fatal(err)
	}
	if !sameGroups(got, allowGroups) {
		t.Fatal("排除列表与等价的限定账户集合分类结果不一致")
	}
	median := func(selection AccountSelection, runs int) time.Duration {
		samples := make([]time.Duration, 0, runs)
		for run := 0; run < runs; run++ {
			begin := time.Now()
			if _, err := pool.ClassifyCandidates(ctx, selection, warm); err != nil {
				t.Fatal(err)
			}
			samples = append(samples, time.Since(begin))
		}
		sort.Slice(samples, func(left, right int) bool { return samples[left] < samples[right] })
		return samples[len(samples)/2]
	}
	baseCost := median(selection, 41)
	exclusionCost := median(withExclusion, 41)
	allowCost := median(withAllowList, 9)
	t.Logf("分类耗时中位数 | 不排除=%s 排除 %d 个=%s 等价限定集合（%d 个账户）=%s",
		baseCost, len(excluded), exclusionCost, len(withAllowList.AllowedAccountIDs), allowCost)
	if exclusionCost > 4*baseCost+time.Millisecond {
		t.Fatalf("排除几个账户的分类耗时 %s，远高于不排除的 %s：没有走候选索引", exclusionCost, baseCost)
	}
	if exclusionCost*4 > allowCost {
		t.Fatalf("排除几个账户的分类耗时 %s 接近限定账户集合路径的 %s：没有走候选索引", exclusionCost, allowCost)
	}
	lease, _, err := pool.TryAcquireFor(ctx, withExclusion)
	if err != nil || lease == nil {
		t.Fatalf("排除后选号 = %s %v，期望取得租约", leaseAccountID(lease), err)
	}
	if slices.Contains(excluded, lease.Account().ID) {
		t.Fatalf("租约落在被排除的账户 %s", lease.Account().ID)
	}
	_ = lease.Release()
}

// TestDrainingRefusesSharedLeases 排空中的账户：不固定账户的分类按忙碌处理（热账户 WarmBusy、备用账户 StandbyBusy）、
// 不接新租约；指定账户与文件绑定的请求照常取得租约；对外状态、Activity 与预热概况不变；标记变化唤醒等待方；移出号池时清除
func TestDrainingRefusesSharedLeases(t *testing.T) {
	catalog := indexTestCatalog()
	drained := indexTestAccount("a@example.com", BenefitTierFree, catalog)
	drained.runtime.Resources["files/a"] = ResourceBinding{}
	other := indexTestAccount("b@example.com", BenefitTierFree, catalog)
	pool := NewAccountPool([]*Account{drained, other}, 2)
	ctx := context.Background()
	selection := AccountSelection{ModelID: indexTestModel, Method: "generateContent"}
	both := []string{"a@example.com", "b@example.com"}
	statusBefore, _ := pool.StatusOf("a@example.com")
	countsBefore := pool.StateCounts()
	summaryBefore := pool.BootstrapSummary()

	changed := pool.Changed()
	if !pool.SetDraining("a@example.com", true) {
		t.Fatal("首次排空应报告变化")
	}
	select {
	case <-changed:
	default:
		t.Fatal("排空标记变化应唤醒等待槽位的请求")
	}
	if pool.SetDraining("a@example.com", true) || pool.SetDraining("missing@example.com", true) {
		t.Fatal("重复排空或账户不存在时不应报告变化")
	}
	if !pool.Draining("a@example.com") || pool.Draining("b@example.com") {
		t.Fatal("Draining 与设置不一致")
	}
	status, _ := pool.StatusOf("a@example.com")
	if status.State != statusBefore.State || pool.StateCounts() != countsBefore ||
		!slices.Equal(pool.BootstrapSummary().ModelIDs, summaryBefore.ModelIDs) || pool.BootstrapSummary().Available != summaryBefore.Available {
		t.Fatalf("排空改变了对外状态：%s → %s", statusBefore.State, status.State)
	}
	if busy, _ := pool.Activity("a@example.com"); busy {
		t.Fatal("排空不应计入 Activity")
	}
	groups, err := pool.ClassifyCandidates(ctx, selection, both)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(groups.WarmBusy, []string{"a@example.com"}) || !slices.Equal(groups.WarmReady, []string{"b@example.com"}) {
		t.Fatalf("排空的热账户分类 = %+v，期望 a 在 WarmBusy", groups)
	}
	if groups, err = pool.ClassifyCandidates(ctx, selection, nil); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(groups.StandbyBusy, []string{"a@example.com"}) || !slices.Equal(groups.StandbyReady, []string{"b@example.com"}) {
		t.Fatalf("排空的备用账户分类 = %+v，期望 a 在 StandbyBusy", groups)
	}
	allowA := selection
	allowA.AllowedAccountIDs = []string{"a@example.com"}
	if groups, err = pool.ClassifyCandidates(ctx, allowA, both); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(groups.WarmBusy, []string{"a@example.com"}) {
		t.Fatalf("限定集合内排空的热账户分类 = %+v，期望 a 在 WarmBusy", groups)
	}
	pinned := selection
	pinned.AccountID = "a@example.com"
	if groups, err = pool.ClassifyCandidates(ctx, pinned, both); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(groups.WarmReady, []string{"a@example.com"}) {
		t.Fatalf("指定账户的分类 = %+v，期望不看排空", groups)
	}
	// 不固定账户的租约都落在 b，b 满后没有租约
	var leases []*AccountLease
	defer func() {
		for _, lease := range leases {
			_ = lease.Release()
		}
	}()
	for attempt := 0; attempt < 3; attempt++ {
		lease, waitable, err := pool.TryAcquireFor(ctx, selection)
		if err != nil {
			t.Fatal(err)
		}
		if attempt < 2 {
			if lease == nil || lease.Account().ID != "b@example.com" {
				t.Fatalf("第 %d 个不固定账户的租约 = %s，期望 b", attempt+1, leaseAccountID(lease))
			}
			leases = append(leases, lease)
			continue
		}
		if lease != nil || !waitable {
			t.Fatalf("b 满后租约 = %s（可等待=%v），期望没有租约且可等待", leaseAccountID(lease), waitable)
		}
	}
	if lease, _, err := pool.TryAcquireFor(ctx, allowA); err != nil || lease != nil {
		t.Fatalf("限定集合只有排空账户时租约 = %s %v，期望没有", leaseAccountID(lease), err)
	}
	lease, _, err := pool.TryAcquireFor(ctx, pinned)
	if err != nil || lease == nil || lease.Account().ID != "a@example.com" {
		t.Fatalf("指定排空账户的租约 = %s %v，期望照常取得", leaseAccountID(lease), err)
	}
	leases = append(leases, lease)
	pool.mu.Lock()
	lease, _, _, err = pool.tryAcquireLocked(AccountSelection{ResourceID: "files/a"}, time.Now())
	pool.mu.Unlock()
	if err != nil || lease == nil || lease.Account().ID != "a@example.com" {
		t.Fatalf("文件绑定排空账户的租约 = %s %v，期望照常取得", leaseAccountID(lease), err)
	}
	leases = append(leases, lease)
	for _, lease := range leases {
		if err := lease.Release(); err != nil {
			t.Fatal(err)
		}
	}
	leases = nil
	// 结束排空后不固定账户的租约可以落在 a
	if !pool.SetDraining("a@example.com", false) || pool.Draining("a@example.com") {
		t.Fatal("结束排空应报告变化")
	}
	lease, _, err = pool.TryAcquireFor(ctx, allowA)
	if err != nil || lease == nil || lease.Account().ID != "a@example.com" {
		t.Fatalf("结束排空后租约 = %s %v，期望 a", leaseAccountID(lease), err)
	}
	leases = append(leases, lease)
	// 移出号池时清除排空标记
	pool.SetDraining("b@example.com", true)
	if _, err := pool.Remove("b@example.com", func(*Account) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if other.draining || pool.Draining("b@example.com") {
		t.Fatal("移出号池的账户仍处于排空")
	}
	pool.mu.Lock()
	err = pool.verifySchedulingIndexLocked()
	pool.mu.Unlock()
	if err != nil {
		t.Fatalf("排空后索引不一致: %v", err)
	}
}

// TestSpareAccountsRequiresModelSupport 指定模型时可接替账户只算支持这些模型、且支持的通道没有长时间冷却的账户：
// 目录里没有该模型的账户、只有一个通道支持而该通道长时间冷却的账户都不算（原先只看冷却键，前者会被当成可接替）
func TestSpareAccountsRequiresModelSupport(t *testing.T) {
	catalog := indexTestCatalog()
	withoutModel := slices.DeleteFunc(slices.Clone(catalog), func(model Model) bool { return model.ID == indexTestModel })
	now := time.Now()
	long, short := CooldownState{Until: now.Add(2 * time.Hour)}, CooldownState{Until: now.Add(time.Minute)}
	accounts := map[string]*Account{
		"a": indexTestAccount("a@example.com", BenefitTierFree, catalog),
		"b": indexTestAccount("b@example.com", BenefitTierFree, withoutModel),
		"c": indexTestAccount("c@example.com", BenefitTierFree, catalog),
		"d": indexTestAccount("d@example.com", BenefitTierFree, catalog),
		"e": indexTestAccount("e@example.com", BenefitTierFree, catalog),
		"f": indexTestAccount("f@example.com", BenefitTierFree, catalog),
	}
	// c 在两个通道上都长时间冷却；d 只有 Playground 支持该模型（没有 Build 目录），Playground 长时间冷却；e 只冷却一分钟
	accounts["c"].runtime.Cooldowns[indexTestModel] = long
	accounts["c"].runtime.Cooldowns[ChannelCooldownScope(ChannelBuild, indexTestModel)] = long
	accounts["d"].runtime.Cooldowns[indexTestModel] = long
	accounts["e"].runtime.Cooldowns[indexTestModel] = short
	accounts["e"].runtime.Cooldowns[ChannelCooldownScope(ChannelBuild, indexTestModel)] = short
	pool := NewAccountPool([]*Account{accounts["a"], accounts["b"], accounts["c"], accounts["d"], accounts["e"], accounts["f"]}, 2)
	pool.SetUpstreamChannels([]Channel{ChannelPlayground, ChannelBuild})
	exclude := map[string]struct{}{"f@example.com": {}}
	minRemaining := 15 * time.Minute
	for _, check := range []struct {
		models []string
		limit  int
		want   int
	}{
		{nil, 0, 5},
		{[]string{indexTestModel}, 0, 2},
		{[]string{indexTestFlash}, 0, 5},
		{[]string{indexTestModel, indexTestFlash}, 0, 2},
		{[]string{indexTestModel}, 1, 1},
		{[]string{"unknown-model"}, 0, 0},
	} {
		if got := pool.SpareAccounts(PoolScopeNormal, exclude, check.models, minRemaining, check.limit); got != check.want {
			t.Fatalf("模型 %v（limit=%d）的可接替账户 %d，期望 %d", check.models, check.limit, got, check.want)
		}
	}
	// d 的 Build 目录支持该模型后，Build 通道没有冷却，可以接替
	if err := pool.SetBuildCatalog("d@example.com", catalog); err != nil {
		t.Fatal(err)
	}
	if got := pool.SpareAccounts(PoolScopeNormal, exclude, []string{indexTestModel}, minRemaining, 0); got != 3 {
		t.Fatalf("d 的 Build 通道可用后可接替账户 %d，期望 3", got)
	}
}

// TestServeableCounts 按候选集成员统计账户能服务几个模型：目录里没有该模型的账户计 0（不能因为“没冷却”就算能服务），
// 所有支持它的通道都长时间冷却计 0，短冷却仍计入，全局长时间冷却的账户对所有模型计 0，号池不认识的模型对谁都计 0
func TestServeableCounts(t *testing.T) {
	catalog := indexTestCatalog()
	withoutModel := slices.DeleteFunc(slices.Clone(catalog), func(model Model) bool { return model.ID == indexTestModel })
	now := time.Now()
	long, short := CooldownState{Until: now.Add(2 * time.Hour)}, CooldownState{Until: now.Add(time.Minute)}
	accounts := []*Account{
		indexTestAccount("a@example.com", BenefitTierFree, catalog),
		indexTestAccount("b@example.com", BenefitTierFree, withoutModel),
		indexTestAccount("c@example.com", BenefitTierFree, catalog),
		indexTestAccount("e@example.com", BenefitTierFree, catalog),
		indexTestAccount("g@example.com", BenefitTierFree, catalog),
	}
	// c 在两个通道上都长时间冷却该模型；e 只冷却一分钟；g 全局长时间冷却
	accounts[2].runtime.Cooldowns[indexTestModel] = long
	accounts[2].runtime.Cooldowns[ChannelCooldownScope(ChannelBuild, indexTestModel)] = long
	accounts[3].runtime.Cooldowns[indexTestModel] = short
	accounts[3].runtime.Cooldowns[ChannelCooldownScope(ChannelBuild, indexTestModel)] = short
	accounts[4].runtime.Cooldowns[globalCooldownKey] = long
	pool := NewAccountPool(accounts, 2)
	pool.SetUpstreamChannels([]Channel{ChannelPlayground, ChannelBuild})
	ids := []string{"a@example.com", "b@example.com", "c@example.com", "e@example.com", "g@example.com", "missing@example.com"}
	minRemaining := 15 * time.Minute

	counts := pool.ServeableCounts(ids, []string{indexTestModel}, minRemaining)
	want := map[string]int{"a@example.com": 1, "b@example.com": 0, "c@example.com": 0, "e@example.com": 1, "g@example.com": 0}
	for id, expected := range want {
		if counts[id] != expected {
			t.Fatalf("%s 能服务 %d 个模型，期望 %d（全部：%v）", id, counts[id], expected, counts)
		}
	}
	if _, present := counts["missing@example.com"]; present {
		t.Fatalf("不存在的账户不应出现在结果里：%v", counts)
	}

	counts = pool.ServeableCounts(ids, []string{indexTestModel, indexTestFlash, "unknown-model"}, minRemaining)
	want = map[string]int{"a@example.com": 2, "b@example.com": 1, "c@example.com": 1, "e@example.com": 2, "g@example.com": 0}
	for id, expected := range want {
		if counts[id] != expected {
			t.Fatalf("多个模型时 %s 能服务 %d 个，期望 %d（全部：%v）", id, counts[id], expected, counts)
		}
	}
	if got := pool.ServeableCounts(ids, nil, minRemaining); len(got) != 0 {
		t.Fatalf("没有模型时应返回空结果：%v", got)
	}
}
