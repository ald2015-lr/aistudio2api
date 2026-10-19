package aistudio

import (
	"context"
	"fmt"
	"maps"
	"math/rand"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"
)

// 候选索引的差分测试：在随机变更（冷却、状态、启用、目录、权益、通道、增删、租约、排空）之后，
// 用索引分类的结果必须与逐个账户判断的参考实现（classifyCandidatesBruteLocked）完全一致，
// 目录派生缓存与状态统计也要与按原实现逐个扫描的结果一致

const (
	indexTestModel      = "index-model"
	indexTestFlash      = "index-flash"
	indexTestFlashAlias = "index-flash-latest"
	indexTestImage      = "index-image"
	indexTestUltraOnly  = "index-ultra-only"
)

// indexTestCatalog 返回测试目录：带别名、带能力、带访问模式限制（只有 Ultra 可用）的模型各一个
func indexTestCatalog() []Model {
	return []Model{
		{ID: indexTestModel, Methods: []string{"generateContent", "countTokens"}, Capabilities: map[string]bool{"chat_model": true}},
		{
			ID: indexTestFlash, Methods: []string{"generateContent", "countTokens", "bidiGenerateContent"},
			Capabilities:      map[string]bool{"chat_model": true},
			CapabilityOptions: map[string][]string{"aliases": {indexTestFlashAlias}},
		},
		{ID: indexTestImage, Methods: []string{"predict"}, Capabilities: map[string]bool{"image_generation": true}},
		{
			ID: indexTestUltraOnly, Methods: []string{"generateContent"}, Capabilities: map[string]bool{"chat_model": true},
			AccessModes: []int64{int64(BenefitTierUltra)},
		},
	}
}

// indexTestAccount 构造只在内存中的账户
func indexTestAccount(id string, tier BenefitTier, models []Model) *Account {
	return &Account{
		ID: id, Config: AccountConfig{Label: id, Enabled: true}, State: AccountReady, BenefitTier: tier,
		Models: cloneAccountModels(models), initializedAt: time.Now(),
		runtime: accountRuntimeState{
			Cooldowns: make(map[string]CooldownState), Resources: make(map[string]ResourceBinding),
			ModelAccess: make(map[string]ModelAccess), BenefitTier: tier,
		},
	}
}

// normalizeGroups 把空切片统一为 nil，便于比较（索引分类会预先分配备用列表）
func normalizeGroups(groups AccountCandidateGroups) AccountCandidateGroups {
	normalize := func(values []string) []string {
		if len(values) == 0 {
			return nil
		}
		return values
	}
	groups.WarmReady = normalize(groups.WarmReady)
	groups.WarmAvailable = normalize(groups.WarmAvailable)
	groups.WarmBusy = normalize(groups.WarmBusy)
	groups.StandbyReady = normalize(groups.StandbyReady)
	groups.StandbyBusy = normalize(groups.StandbyBusy)
	groups.EarliestCooldown = groups.EarliestCooldown.UTC()
	return groups
}

func sameGroups(left AccountCandidateGroups, right AccountCandidateGroups) bool {
	left, right = normalizeGroups(left), normalizeGroups(right)
	if !left.EarliestCooldown.Equal(right.EarliestCooldown) {
		return false
	}
	left.EarliestCooldown, right.EarliestCooldown = time.Time{}, time.Time{}
	return reflect.DeepEqual(left, right)
}

func sameError(left error, right error) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return left.Error() == right.Error()
}

// 以下为原实现的拷贝，作为目录派生缓存与状态统计的参考

func bruteHasModel(p *AccountPool, modelID string, method string, capability string, scope PoolScope) bool {
	for _, account := range p.accounts {
		if account == nil || !scope.allows(account, p.ultraIncludePro) {
			continue
		}
		for _, model := range account.Models {
			if !modelMatchesID(model, modelID) {
				continue
			}
			if method != "" && !hasMethod(model, method) {
				continue
			}
			if capability != "" && !model.Capabilities[capability] {
				continue
			}
			return true
		}
	}
	return capability == "" && p.hasBuildModelLocked(modelID, method, scope)
}

func bruteCanonical(p *AccountPool, scope PoolScope, modelID string) string {
	trimmed := strings.TrimPrefix(strings.TrimSpace(modelID), "models/")
	canonical := ""
	for _, account := range p.accounts {
		if account == nil || !scope.allows(account, p.ultraIncludePro) {
			continue
		}
		for _, model := range account.Models {
			if model.ID == trimmed {
				return modelID
			}
			if canonical == "" && modelMatchesID(model, trimmed) {
				canonical = model.ID
			}
		}
	}
	if canonical == "" {
		return modelID
	}
	return canonical
}

func bruteModelChannels(p *AccountPool, model Model, scope PoolScope) []string {
	selection := AccountSelection{ModelID: model.ID}
	if hasMethod(model, "generateContent") {
		selection.Method = "generateContent"
	}
	var channels []string
	for _, channel := range p.selectionChannelsLocked(selection) {
		for _, account := range p.accounts {
			if account != nil && account.Config.Enabled && scope.allows(account, p.ultraIncludePro) && p.channelSupportsLocked(account, channel, selection) {
				channels = append(channels, string(channel))
				break
			}
		}
	}
	return channels
}

func bruteStateCounts(p *AccountPool, now time.Time) (AccountStateCounts, AccountStateCounts) {
	var counts, ultra AccountStateCounts
	for _, account := range p.accounts {
		if account == nil {
			continue
		}
		state := p.accountStateLocked(account, now)
		counts.add(state)
		if ultraPoolTier(account.BenefitTier, p.ultraIncludePro) {
			ultra.add(state)
		}
	}
	return counts, ultra
}

func bruteEnabledAccountsIn(p *AccountPool, scope PoolScope, now time.Time) ([]string, int) {
	ids := make([]string, 0, len(p.accounts))
	schedulable := 0
	for _, account := range p.accounts {
		if account == nil || !account.Config.Enabled || !scope.allows(account, p.ultraIncludePro) {
			continue
		}
		ids = append(ids, account.ID)
		_, cooling := accountCooldown(account, "", now)
		if account.exclusive || account.exclusiveWaiters > 0 || account.authRefreshers > 0 || account.active > 0 || account.State == AccountReady && !cooling {
			schedulable++
		}
	}
	return ids, schedulable
}

func bruteAttemptCandidates(p *AccountPool, scope PoolScope, now time.Time) int {
	eligible := 0
	for _, account := range p.accounts {
		if account == nil || !account.Config.Enabled || !scope.allows(account, p.ultraIncludePro) {
			continue
		}
		if state := p.accountStateLocked(account, now); state == AccountReady || state == AccountBusy {
			eligible++
		}
	}
	return eligible
}

func bruteBootstrapModels(account *Account) []string {
	eligible := func(modelID string) bool {
		for _, model := range account.Models {
			if !modelMatchesID(model, modelID) || !hasMethod(model, "generateContent") ||
				!modelAllowedByTier(model, account.BenefitTier) || !model.Capabilities["chat_model"] {
				continue
			}
			return true
		}
		return false
	}
	models := make([]string, 0, len(account.Models))
	seen := make(map[string]struct{}, len(account.Models))
	appendModel := func(modelID string) {
		if _, exists := seen[modelID]; exists || !eligible(modelID) {
			return
		}
		seen[modelID] = struct{}{}
		models = append(models, modelID)
	}
	appendModel(preferredBootstrapModelID)
	for _, model := range account.Models {
		appendModel(model.ID)
	}
	return models
}

func bruteWarmCoverage(account *Account) int {
	coverage := 0
	for _, model := range account.Models {
		if account.SupportsMethod(model.ID, "generateContent") {
			coverage++
		}
	}
	return coverage
}

// indexFuzzer 驱动随机变更并核对结果
type indexFuzzer struct {
	t       *testing.T
	random  *rand.Rand
	pool    *AccountPool
	catalog []Model
	ids     []string
	leases  []*AccountLease
	ends    []func()
	nextID  int
}

func (fuzzer *indexFuzzer) randomID() string {
	return fuzzer.ids[fuzzer.random.Intn(len(fuzzer.ids))]
}

// randomModels 随机抽取目录；Ultra 专用模型很少出现，让“Playground 里有没有它”在变更中来回翻转：
// 它出现在任一账户的 Playground 目录后，Free 账户的 Build 通道对它不再可用（跨账户依赖，见 buildSupportsModelLocked）
func (fuzzer *indexFuzzer) randomModels() []Model {
	var models []Model
	for _, model := range fuzzer.catalog {
		if model.ID == indexTestUltraOnly {
			if fuzzer.random.Intn(50) == 0 {
				models = append(models, model)
			}
			continue
		}
		if fuzzer.random.Intn(4) != 0 {
			models = append(models, model)
		}
	}
	return models
}

func (fuzzer *indexFuzzer) randomTier() BenefitTier {
	return []BenefitTier{BenefitTierFree, BenefitTierFree, BenefitTierPro, BenefitTierUltra}[fuzzer.random.Intn(4)]
}

func (fuzzer *indexFuzzer) randomScope() string {
	return []string{
		"", indexTestModel, indexTestFlash, "build:" + indexTestModel, "count-tokens:" + indexTestModel, "build:" + indexTestUltraOnly,
	}[fuzzer.random.Intn(6)]
}

// syncDerivedLocked 跳过节流，把目录变化对其他账户 Build 通道的影响同步到候选索引：状态口径的冷却期限重算、含 Build 通道的候选集重建。
// 生产路径按 schedulingBuildRebuildInterval 节流（最多晚一秒），核对前先同步再与逐个账户现算的参考实现比较
func (p *AccountPool) syncDerivedLocked(now time.Time) {
	index := p.schedulingLocked()
	index.coolSyncedAt = time.Time{}
	index.refreshCoolingLocked(p, now)
	for _, set := range index.sets {
		if (set.eligibilityVersion != p.eligibilityVersion || set.playgroundStale) && slices.Contains(set.channels, ChannelBuild) {
			p.rebuildCandidateSetLocked(set, now)
		}
	}
}

// mutate 执行一次随机变更
func (fuzzer *indexFuzzer) mutate() {
	pool, random := fuzzer.pool, fuzzer.random
	now := time.Now()
	switch random.Intn(18) {
	case 0, 1, 2:
		accountID := fuzzer.randomID()
		until := now.Add(time.Duration(5+random.Intn(60)) * time.Millisecond)
		if random.Intn(5) == 0 {
			until = now.Add(time.Hour)
		}
		err := pool.MarkCooldownIfGeneration(accountID, fuzzer.randomScope(), pool.ModelAccessGeneration(accountID), now, until, "测试冷却")
		if err != nil && !strings.Contains(err.Error(), "账户不存在") {
			fuzzer.t.Fatalf("MarkCooldownIfGeneration: %v", err)
		}
	case 3:
		accountID := fuzzer.randomID()
		err := pool.ClearCooldownIfGeneration(accountID, fuzzer.randomScope(), pool.ModelAccessGeneration(accountID), now)
		if err != nil && !strings.Contains(err.Error(), "账户不存在") {
			fuzzer.t.Fatalf("ClearCooldownIfGeneration: %v", err)
		}
	case 4:
		accountID := fuzzer.randomID()
		var err error
		switch random.Intn(3) {
		case 0:
			err = pool.MarkReady(accountID)
		case 1:
			err = pool.MarkUnavailable(accountID, "测试不可用")
		default:
			err = pool.MarkAuthRequired(accountID, "测试需要登录")
		}
		if err != nil && !strings.Contains(err.Error(), "账户不存在") {
			fuzzer.t.Fatalf("setAccountState: %v", err)
		}
	case 5:
		// 启用状态：与 SaveConfig 相同的锁内更新
		accountID := fuzzer.randomID()
		pool.mu.Lock()
		if account := pool.byID[accountID]; account != nil {
			account.Config.Enabled = random.Intn(3) != 0
			if !account.Config.Enabled {
				account.State = AccountDisabled
			} else if account.State == AccountDisabled {
				account.State = AccountReady
			}
			pool.eligibilityVersion++
			pool.accountChangedLocked(account)
		}
		pool.mu.Unlock()
	case 6, 7:
		accountID := fuzzer.randomID()
		err := pool.SetCatalog(accountID, fuzzer.randomTier(), fuzzer.randomModels())
		if err != nil && !strings.Contains(err.Error(), "账户不存在") {
			fuzzer.t.Fatalf("SetCatalog: %v", err)
		}
	case 8:
		accountID := fuzzer.randomID()
		err := pool.SetBuildCatalog(accountID, fuzzer.randomModels())
		if err != nil && !strings.Contains(err.Error(), "账户不存在") {
			fuzzer.t.Fatalf("SetBuildCatalog: %v", err)
		}
	case 9:
		if random.Intn(4) == 0 {
			channels := [][]Channel{{ChannelPlayground}, {ChannelBuild}, {ChannelPlayground, ChannelBuild}, {ChannelBuild, ChannelPlayground}}
			pool.SetUpstreamChannels(channels[random.Intn(len(channels))])
		}
	case 10:
		if random.Intn(3) == 0 {
			fuzzer.nextID++
			id := fmt.Sprintf("added-%03d@example.com", fuzzer.nextID)
			if err := pool.Add(indexTestAccount(id, fuzzer.randomTier(), fuzzer.randomModels())); err != nil {
				fuzzer.t.Fatalf("Add: %v", err)
			}
			fuzzer.ids = append(fuzzer.ids, id)
		} else if len(fuzzer.ids) > 4 {
			accountID := fuzzer.randomID()
			if _, err := pool.Remove(accountID, func(*Account) error { return nil }); err == nil {
				fuzzer.ids = slices.DeleteFunc(fuzzer.ids, func(id string) bool { return id == accountID })
			}
		}
	case 11, 12:
		selection := fuzzer.randomSelection()
		selection.AccountID, selection.ResourceID = "", ""
		if random.Intn(2) == 0 {
			selection.AllowedAccountIDs = nil
		}
		lease, _, err := pool.TryAcquireFor(context.Background(), selection)
		if err == nil && lease != nil {
			fuzzer.leases = append(fuzzer.leases, lease)
		}
	case 13:
		if len(fuzzer.leases) > 0 {
			at := random.Intn(len(fuzzer.leases))
			if err := fuzzer.leases[at].Release(); err != nil {
				fuzzer.t.Fatalf("Release: %v", err)
			}
			fuzzer.leases = slices.Delete(fuzzer.leases, at, at+1)
		}
	case 14:
		if random.Intn(2) == 0 && len(fuzzer.leases) > 0 {
			if end, ok := fuzzer.leases[random.Intn(len(fuzzer.leases))].BeginAuthRefresh(); ok {
				fuzzer.ends = append(fuzzer.ends, end)
			}
		} else if len(fuzzer.ends) > 0 {
			at := random.Intn(len(fuzzer.ends))
			fuzzer.ends[at]()
			fuzzer.ends = slices.Delete(fuzzer.ends, at, at+1)
		} else {
			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			lease, err := pool.AcquireAccount(ctx, fuzzer.randomID())
			cancel()
			if err == nil {
				fuzzer.leases = append(fuzzer.leases, lease)
			}
		}
	case 15:
		if random.Intn(4) == 0 {
			pool.SetPerAccountConcurrency(1 + random.Intn(3))
		} else {
			time.Sleep(time.Duration(1+random.Intn(8)) * time.Millisecond)
		}
	case 16:
		// 排空：约三分之一的时间处于排空，覆盖热账户与备用账户
		accountID := fuzzer.randomID()
		on := random.Intn(3) != 0
		before := pool.Draining(accountID)
		changed := pool.SetDraining(accountID, on)
		if changed != (before != on) || pool.Draining(accountID) != on {
			fuzzer.t.Fatalf("SetDraining(%s, %v) = %v，之前 %v，之后 %v", accountID, on, changed, before, pool.Draining(accountID))
		}
	case 17:
		// ULTRA_INCLUDE_PRO 热切换：Pro 账户在两个号池之间整体移动，候选索引作废后按新成员重建；
		// 先取一次快照，核对切换后快照被作废
		if random.Intn(3) == 0 {
			pool.UltraAccountIDs()
			on := random.Intn(2) == 0
			before := pool.UltraIncludePro()
			version := pool.PoolRuleVersion()
			changed := pool.SetUltraIncludePro(on)
			if changed != (before != on) || pool.UltraIncludePro() != on {
				fuzzer.t.Fatalf("SetUltraIncludePro(%v) = %v，之前 %v", on, changed, before)
			}
			wantVersion := version
			if changed {
				wantVersion++
			}
			if pool.PoolRuleVersion() != wantVersion {
				fuzzer.t.Fatalf("SetUltraIncludePro(%v) 后号池规则版本 %d，期望 %d", on, pool.PoolRuleVersion(), wantVersion)
			}
		}
	}
}

// randomSelection 生成随机选择条件
func (fuzzer *indexFuzzer) randomSelection() AccountSelection {
	random := fuzzer.random
	selection := AccountSelection{
		ModelID:    []string{indexTestModel, indexTestFlash, indexTestFlashAlias, indexTestImage, indexTestUltraOnly, "models/" + indexTestModel, "", "unknown-model"}[random.Intn(8)],
		Method:     []string{"", "generateContent", "generateContent", "countTokens", "predict"}[random.Intn(5)],
		Capability: []string{"", "", "chat_model", "image_generation"}[random.Intn(4)],
		Pool:       []PoolScope{PoolScopeAll, PoolScopeNormal, PoolScopeUltra}[random.Intn(3)],
	}
	if random.Intn(5) == 0 {
		selection.ModelAccessScope = "count-tokens:" + strings.TrimPrefix(selection.ModelID, "models/")
	}
	switch random.Intn(6) {
	case 0:
		selection.PlaygroundOnly = true
	case 1:
		selection.BuildOnly = true
	case 2:
		selection.PlaygroundFirst = true
	case 3:
		selection.PreferPlayground = true
	}
	switch random.Intn(5) {
	case 0:
		selection.AllowedAccountIDs = []string{}
	case 1, 2:
		for _, id := range fuzzer.ids {
			if random.Intn(2) == 0 {
				selection.AllowedAccountIDs = append(selection.AllowedAccountIDs, id)
			}
		}
		selection.AllowedAccountIDs = append(selection.AllowedAccountIDs, "missing@example.com", " ")
	}
	// 排除列表：多数选择带 0-5 个排除账户（含重复、不存在与空白 ID），偶尔超过逐个比较的上限
	switch random.Intn(6) {
	case 0, 1:
	case 2:
		for _, id := range fuzzer.ids {
			if random.Intn(3) == 0 {
				selection.ExcludedAccountIDs = append(selection.ExcludedAccountIDs, id)
			}
		}
	default:
		for count := 1 + random.Intn(5); count > 0; count-- {
			selection.ExcludedAccountIDs = append(selection.ExcludedAccountIDs, fuzzer.randomID())
		}
		if random.Intn(3) == 0 {
			selection.ExcludedAccountIDs = append(selection.ExcludedAccountIDs, "missing@example.com", " ", selection.ExcludedAccountIDs[0])
		}
	}
	if random.Intn(8) == 0 {
		selection.AccountID = fuzzer.randomID()
	}
	return selection
}

func (fuzzer *indexFuzzer) randomWarm() []string {
	var warm []string
	for _, id := range fuzzer.ids {
		if fuzzer.random.Intn(3) == 0 {
			warm = append(warm, id)
		}
	}
	return warm
}

// check 在锁内核对索引一致性、分类结果与派生缓存
func (fuzzer *indexFuzzer) check(round int) {
	pool, t := fuzzer.pool, fuzzer.t
	pool.mu.Lock()
	defer pool.mu.Unlock()
	pool.syncDerivedLocked(time.Now())
	if err := pool.verifySchedulingIndexLocked(); err != nil {
		t.Fatalf("第 %d 轮：候选索引与账户状态不一致: %v", round, err)
	}
	for attempt := 0; attempt < 12; attempt++ {
		selection := fuzzer.randomSelection()
		selection.ModelID = strings.TrimPrefix(strings.TrimSpace(selection.ModelID), "models/")
		warm := fuzzer.randomWarm()
		now := time.Now()
		indexed, indexedErr := pool.classifyCandidatesAtLocked(selection, warm, now)
		brute, bruteErr := pool.classifyCandidatesBruteLocked(selection, warm, now)
		if !sameError(indexedErr, bruteErr) {
			t.Fatalf("第 %d 轮：选择 %+v 的错误不一致：索引=%v 参考=%v", round, selection, indexedErr, bruteErr)
		}
		if !sameGroups(indexed, brute) {
			t.Fatalf("第 %d 轮：选择 %+v 的分类不一致\n索引=%+v\n参考=%+v", round, selection, normalizeGroups(indexed), normalizeGroups(brute))
		}
	}
	now := time.Now()
	for attempt := 0; attempt < 4; attempt++ {
		scope := []PoolScope{PoolScopeAll, PoolScopeNormal, PoolScopeUltra}[fuzzer.random.Intn(3)]
		models := [][]string{nil, {indexTestModel}, {indexTestFlash}, {indexTestModel, indexTestFlash}, {indexTestUltraOnly}}[fuzzer.random.Intn(5)]
		exclude := make(map[string]struct{})
		for _, id := range fuzzer.randomWarm() {
			exclude[id] = struct{}{}
		}
		minRemaining := []time.Duration{0, 30 * time.Millisecond, time.Minute}[fuzzer.random.Intn(3)]
		got := pool.spareAccountsAtLocked(scope, exclude, models, minRemaining, 0, now)
		if want := bruteSpareAccounts(pool, scope, exclude, models, minRemaining, now); got != want {
			t.Fatalf("第 %d 轮：号池 %v 模型 %v 排除 %d 个 的可接替账户 %d，期望 %d", round, scope, models, len(exclude), got, want)
		}
		if limited := pool.spareAccountsAtLocked(scope, exclude, models, minRemaining, 2, now); limited != min(got, 2) {
			t.Fatalf("第 %d 轮：号池 %v 模型 %v 限 2 个时可接替账户 %d，期望 %d", round, scope, models, limited, min(got, 2))
		}
	}
	now = time.Now()
	for _, scope := range []PoolScope{PoolScopeAll, PoolScopeNormal, PoolScopeUltra} {
		for _, modelID := range []string{indexTestModel, indexTestFlash, indexTestFlashAlias, indexTestImage, indexTestUltraOnly, "unknown-model"} {
			for _, method := range []string{"", "generateContent", "predict"} {
				var got bool
				switch method {
				case "":
					got = pool.hasModelLocked(modelID, scope)
				default:
					got = pool.hasModelMethodLocked(modelID, method, scope)
				}
				if want := bruteHasModel(pool, modelID, method, "", scope); got != want {
					t.Fatalf("第 %d 轮：号池 %v 模型 %s 方法 %q 的存在性 %v，期望 %v", round, scope, modelID, method, got, want)
				}
			}
			for _, capability := range []string{"chat_model", "image_generation"} {
				if got, want := pool.hasModelCapabilityLocked(modelID, capability, scope), bruteHasModel(pool, modelID, "", capability, scope); got != want {
					t.Fatalf("第 %d 轮：号池 %v 模型 %s 能力 %s 的存在性 %v，期望 %v", round, scope, modelID, capability, got, want)
				}
			}
			index := pool.canonicalIndexLocked(scope)
			got := modelID
			if _, exists := index.primary[modelID]; !exists {
				if canonical, exists := index.alias[modelID]; exists {
					got = canonical
				}
			}
			if want := bruteCanonical(pool, scope, modelID); got != want {
				t.Fatalf("第 %d 轮：号池 %v 模型 %s 的正式 ID %q，期望 %q", round, scope, modelID, got, want)
			}
		}
		for _, model := range fuzzer.catalog {
			if got, want := pool.modelChannelsInLocked(model, scope), bruteModelChannels(pool, model, scope); !slices.Equal(got, want) {
				t.Fatalf("第 %d 轮：号池 %v 模型 %s 的通道 %v，期望 %v", round, scope, model.ID, got, want)
			}
		}
		if got, want := pool.hasModelCatalogLocked(scope), bruteHasCatalog(pool, scope); got != want {
			t.Fatalf("第 %d 轮：号池 %v 的目录存在性 %v，期望 %v", round, scope, got, want)
		}
		// 状态统计与换号上限用同一时刻比较
		nowNanos := now.UnixNano()
		index := pool.derivedStatesLocked(now)
		ids, schedulable := make([]string, 0), 0
		eligible := 0
		for position := range index.ids {
			if !index.enabled[position] || !scope.allowsTier(index.tier[position], pool.ultraIncludePro) {
				continue
			}
			ids = append(ids, index.ids[position])
			if index.blocked[position] || index.active[position] > 0 || index.state[position] == AccountReady && index.globalUntil[position] <= nowNanos {
				schedulable++
			}
			if state := index.derivedStateAt(position, nowNanos); state == AccountReady || state == AccountBusy {
				eligible++
			}
		}
		wantIDs, wantSchedulable := bruteEnabledAccountsIn(pool, scope, now)
		if !slices.Equal(ids, wantIDs) || schedulable != wantSchedulable {
			t.Fatalf("第 %d 轮：号池 %v 的启用账户 (%d, %d)，期望 (%d, %d)", round, scope, len(ids), schedulable, len(wantIDs), wantSchedulable)
		}
		if want := bruteAttemptCandidates(pool, scope, now); eligible != want {
			t.Fatalf("第 %d 轮：号池 %v 的换号上限 %d，期望 %d", round, scope, eligible, want)
		}
	}
	index := pool.derivedStatesLocked(now)
	var counts, ultra AccountStateCounts
	for position := range index.ids {
		if pool.accounts[position] == nil {
			continue
		}
		state := index.derivedStateAt(position, now.UnixNano())
		counts.add(state)
		if ultraPoolTier(index.tier[position], pool.ultraIncludePro) {
			ultra.add(state)
		}
	}
	if wantCounts, wantUltra := bruteStateCounts(pool, now); counts != wantCounts || ultra != wantUltra {
		t.Fatalf("第 %d 轮：状态统计 %+v / %+v，期望 %+v / %+v", round, counts, ultra, wantCounts, wantUltra)
	}
	ultraIDs := make([]string, 0)
	for id := range pool.ultraAccounts {
		ultraIDs = append(ultraIDs, id)
	}
	sort.Strings(ultraIDs)
	wantUltraIDs := make([]string, 0)
	for _, account := range pool.accounts {
		if account != nil && ultraPoolTier(account.BenefitTier, pool.ultraIncludePro) {
			wantUltraIDs = append(wantUltraIDs, account.ID)
		}
	}
	sort.Strings(wantUltraIDs)
	if !slices.Equal(ultraIDs, wantUltraIDs) {
		t.Fatalf("第 %d 轮：Ultra 号池成员集合 %v，期望 %v（ULTRA_INCLUDE_PRO=%v）", round, ultraIDs, wantUltraIDs, pool.ultraIncludePro)
	}
	// 写时复制的快照已建好时必须与成员集合一致：成员变化的每条路径都要作废它
	if snapshot := pool.ultraSnapshot; snapshot != nil {
		snapshotIDs := slices.Sorted(maps.Keys(snapshot))
		if !slices.Equal(snapshotIDs, wantUltraIDs) {
			t.Fatalf("第 %d 轮：UltraAccountIDs 快照 %v，期望 %v", round, snapshotIDs, wantUltraIDs)
		}
	}
	for _, account := range pool.accounts {
		if got, want := accountBootstrapModelsLocked(account), bruteBootstrapModels(account); !slices.Equal(got, want) {
			t.Fatalf("第 %d 轮：账户 %s 的初始化模型 %v，期望 %v", round, account.ID, got, want)
		}
		if got, want := accountWarmCoverageLocked(account), bruteWarmCoverage(account); got != want {
			t.Fatalf("第 %d 轮：账户 %s 的可生成模型数 %d，期望 %d", round, account.ID, got, want)
		}
	}
}

// bruteSpareAccounts 为 SpareAccounts 的参考实现：逐个账户按通道判断是否为各模型生成请求的候选，
// 支持的通道中是否有一个没有长时间冷却；调用方持有 p.mu
func bruteSpareAccounts(p *AccountPool, scope PoolScope, exclude map[string]struct{}, modelIDs []string, minRemaining time.Duration, now time.Time) int {
	spare := 0
	for _, account := range p.accounts {
		if account == nil || !account.Config.Enabled || account.State != AccountReady || !scope.allows(account, p.ultraIncludePro) {
			continue
		}
		if _, excluded := exclude[account.ID]; excluded || len(bruteBootstrapModels(account)) == 0 {
			continue
		}
		if p.longCoolingLocked(account, "", now, minRemaining) {
			continue
		}
		ok := true
		for _, modelID := range modelIDs {
			selection := AccountSelection{ModelID: modelID, Method: "generateContent", Pool: scope}
			usable := false
			for _, channel := range p.selectionChannelsLocked(selection) {
				if !p.channelSupportsLocked(account, channel, selection) {
					continue
				}
				cooldown, active := accountCooldown(account, ChannelCooldownScope(channel, modelID), now)
				if !active || cooldown.Until.Sub(now) < minRemaining {
					usable = true
				}
			}
			if !usable || p.longCoolingLocked(account, modelID, now, minRemaining) {
				ok = false
				break
			}
		}
		if ok {
			spare++
		}
	}
	return spare
}

func bruteHasCatalog(p *AccountPool, scope PoolScope) bool {
	for _, account := range p.accounts {
		if account != nil && len(account.Models) > 0 && scope.allows(account, p.ultraIncludePro) {
			return true
		}
	}
	return false
}

// TestSchedulingIndexMatchesBruteForce 随机变更账户池后，索引分类与参考实现一致，派生缓存与原实现一致
func TestSchedulingIndexMatchesBruteForce(t *testing.T) {
	seed := time.Now().UnixNano()
	t.Logf("随机种子 %d", seed)
	random := rand.New(rand.NewSource(seed))
	catalog := indexTestCatalog()
	accounts := 48
	rounds := 150
	if testing.Short() {
		accounts, rounds = 16, 40
	}
	fuzzer := &indexFuzzer{t: t, random: random, catalog: catalog}
	var initial []*Account
	for index := 0; index < accounts; index++ {
		id := fmt.Sprintf("fuzz-%03d@example.com", index)
		account := indexTestAccount(id, fuzzer.randomTier(), fuzzer.randomModels())
		if random.Intn(3) == 0 {
			account.runtime.Cooldowns[globalCooldownKey] = CooldownState{Until: time.Now().Add(time.Duration(random.Intn(80)) * time.Millisecond)}
		}
		initial = append(initial, account)
		fuzzer.ids = append(fuzzer.ids, id)
	}
	fuzzer.pool = NewAccountPool(initial, 2)
	fuzzer.pool.SetUpstreamChannels([]Channel{ChannelPlayground, ChannelBuild})
	for _, id := range fuzzer.ids[:accounts/2] {
		if err := fuzzer.pool.SetBuildCatalog(id, fuzzer.randomModels()); err != nil {
			t.Fatal(err)
		}
	}
	defer func() {
		for _, end := range fuzzer.ends {
			end()
		}
		for _, lease := range fuzzer.leases {
			_ = lease.Release()
		}
	}()
	fuzzer.check(0)
	for round := 1; round <= rounds; round++ {
		for step := 0; step < 1+random.Intn(6); step++ {
			fuzzer.mutate()
		}
		fuzzer.check(round)
	}
}

// TestSchedulingIndexCooldownExpiry 冷却到期后候选自动回到可用列表，最早恢复时间来自冷却堆
func TestSchedulingIndexCooldownExpiry(t *testing.T) {
	catalog := indexTestCatalog()
	pool := NewAccountPool([]*Account{
		indexTestAccount("a@example.com", BenefitTierFree, catalog),
		indexTestAccount("b@example.com", BenefitTierFree, catalog),
	}, 2)
	ctx := context.Background()
	selection := AccountSelection{ModelID: indexTestModel, Method: "generateContent"}
	now := time.Now()
	until := now.Add(80 * time.Millisecond)
	if err := pool.MarkCooldownIfGeneration("a@example.com", "", pool.ModelAccessGeneration("a@example.com"), now, until, "测试"); err != nil {
		t.Fatal(err)
	}
	later := now.Add(time.Hour)
	if err := pool.MarkCooldownIfGeneration("b@example.com", indexTestModel, pool.ModelAccessGeneration("b@example.com"), now, later, "测试"); err != nil {
		t.Fatal(err)
	}
	groups, err := pool.ClassifyCandidates(ctx, selection, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(groups.StandbyReady) != 0 || !groups.Eligible || !groups.EarliestCooldown.Equal(until.UTC()) {
		t.Fatalf("全部冷却时分类 = %+v，期望没有候选且最早恢复时间为 %s", groups, until)
	}
	time.Sleep(time.Until(until) + 20*time.Millisecond)
	groups, err = pool.ClassifyCandidates(ctx, selection, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(groups.StandbyReady, []string{"a@example.com"}) || !groups.EarliestCooldown.Equal(later.UTC()) {
		t.Fatalf("冷却到期后分类 = %+v，期望 a 可用且最早恢复时间为 %s", groups, later)
	}
	pool.mu.Lock()
	err = pool.verifySchedulingIndexLocked()
	pool.mu.Unlock()
	if err != nil {
		t.Fatalf("冷却到期后索引不一致: %v", err)
	}
	// 清除冷却后 b 立即回到可用列表
	if err := pool.ClearCooldownIfGeneration("b@example.com", indexTestModel, pool.ModelAccessGeneration("b@example.com"), time.Now()); err != nil {
		t.Fatal(err)
	}
	groups, err = pool.ClassifyCandidates(ctx, selection, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(groups.StandbyReady) != 2 || !groups.EarliestCooldown.IsZero() {
		t.Fatalf("清除冷却后分类 = %+v，期望两个账户都可用", groups)
	}
}

// TestSchedulingIndexSaveConfig 账户经 SaveConfig 停用后立即退出候选，重新启用后回来
func TestSchedulingIndexSaveConfig(t *testing.T) {
	pool, accountID := testCatalogPool(t)
	ctx := context.Background()
	selection := AccountSelection{ModelID: testNoEligibleModel, Method: "generateContent"}
	groups, err := pool.ClassifyCandidates(ctx, selection, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(groups.StandbyReady, []string{accountID}) {
		t.Fatalf("初始分类 = %+v，期望账户可用", groups)
	}
	lease, err := pool.AcquireFor(ctx, AccountSelection{AccountID: accountID})
	if err != nil {
		t.Fatal(err)
	}
	config := lease.Account().Config
	config.Enabled = false
	if err := lease.SaveConfig(config); err != nil {
		t.Fatal(err)
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
	if groups, err = pool.ClassifyCandidates(ctx, selection, nil); err != nil {
		t.Fatal(err)
	}
	if groups.Eligible || len(groups.StandbyReady) != 0 {
		t.Fatalf("停用后分类 = %+v，期望没有候选", groups)
	}
	if lease, err = pool.AcquireAccount(ctx, accountID); err != nil {
		t.Fatal(err)
	}
	config.Enabled = true
	if err := lease.SaveConfig(config); err != nil {
		t.Fatal(err)
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
	if groups, err = pool.ClassifyCandidates(ctx, selection, nil); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(groups.StandbyReady, []string{accountID}) {
		t.Fatalf("重新启用后分类 = %+v，期望账户可用", groups)
	}
}

// TestTryAcquireIndexedRotation 不限账户的选号在账户与通道之间按原顺序轮询：同一账户先用完上次通道之后的通道，
// 再轮到下一个账户；PreferPlayground 时每个账户先 Playground，只在账户之间轮询
func TestTryAcquireIndexedRotation(t *testing.T) {
	catalog := indexTestCatalog()
	pool := NewAccountPool([]*Account{
		indexTestAccount("a@example.com", BenefitTierFree, catalog),
		indexTestAccount("b@example.com", BenefitTierFree, catalog),
	}, 4)
	pool.SetUpstreamChannels([]Channel{ChannelPlayground, ChannelBuild})
	for _, id := range []string{"a@example.com", "b@example.com"} {
		if err := pool.SetBuildCatalog(id, catalog); err != nil {
			t.Fatal(err)
		}
	}
	acquire := func(selection AccountSelection, count int) []string {
		t.Helper()
		var used []string
		for index := 0; index < count; index++ {
			lease, _, err := pool.TryAcquireFor(context.Background(), selection)
			if err != nil {
				t.Fatal(err)
			}
			if lease == nil {
				t.Fatal("没有取得租约")
			}
			used = append(used, lease.Account().ID+"/"+string(lease.Channel()))
			if err := lease.Release(); err != nil {
				t.Fatal(err)
			}
		}
		return used
	}
	selection := AccountSelection{ModelID: indexTestModel, Method: "generateContent"}
	want := []string{
		"a@example.com/playground", "a@example.com/build", "b@example.com/playground", "b@example.com/build",
		"a@example.com/playground", "a@example.com/build",
	}
	if got := acquire(selection, len(want)); !slices.Equal(got, want) {
		t.Fatalf("轮询顺序 %v，期望 %v", got, want)
	}
	selection.PreferPlayground = true
	want = []string{"b@example.com/playground", "a@example.com/playground", "b@example.com/playground"}
	if got := acquire(selection, len(want)); !slices.Equal(got, want) {
		t.Fatalf("PreferPlayground 轮询顺序 %v，期望 %v", got, want)
	}
	// Playground 冷却时同一账号改走 Build，全部冷却时返回最早恢复时间
	now := time.Now()
	for _, id := range []string{"a@example.com", "b@example.com"} {
		if err := pool.MarkCooldownIfGeneration(id, indexTestModel, pool.ModelAccessGeneration(id), now, now.Add(time.Hour), "测试"); err != nil {
			t.Fatal(err)
		}
	}
	if got := acquire(selection, 2); !slices.Equal(got, []string{"a@example.com/build", "b@example.com/build"}) {
		t.Fatalf("Playground 冷却后顺序 %v，期望都走 Build", got)
	}
	until := now.Add(2 * time.Hour)
	for _, id := range []string{"a@example.com", "b@example.com"} {
		if err := pool.MarkCooldownIfGeneration(id, "", pool.ModelAccessGeneration(id), now, until, "测试"); err != nil {
			t.Fatal(err)
		}
	}
	pool.mu.Lock()
	lease, earliest, waitable, err := pool.tryAcquireLocked(selection, time.Now())
	pool.mu.Unlock()
	if err != nil || lease != nil || !waitable || !earliest.Equal(until.UTC()) {
		t.Fatalf("全部冷却时 tryAcquireLocked = (%v, %s, %v, %v)，期望可等待且最早恢复时间为 %s", lease, earliest, waitable, err, until)
	}
}

// TestSchedulingIndexAddRemove 索引建立后加入的账户参与调度，移除的账户退出调度
func TestSchedulingIndexAddRemove(t *testing.T) {
	catalog := indexTestCatalog()
	pool := NewAccountPool([]*Account{indexTestAccount("a@example.com", BenefitTierFree, catalog)}, 2)
	ctx := context.Background()
	selection := AccountSelection{ModelID: indexTestModel, Method: "generateContent"}
	if _, err := pool.ClassifyCandidates(ctx, selection, nil); err != nil {
		t.Fatal(err)
	}
	added := indexTestAccount("b@example.com", BenefitTierUltra, catalog)
	if err := pool.Add(added); err != nil {
		t.Fatal(err)
	}
	groups, err := pool.ClassifyCandidates(ctx, selection, []string{"b@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(groups.WarmReady, []string{"b@example.com"}) || !slices.Equal(groups.StandbyReady, []string{"a@example.com"}) {
		t.Fatalf("加入账户后分类 = %+v", groups)
	}
	if ultra := pool.UltraAccountIDs(); len(ultra) != 1 {
		t.Fatalf("Ultra 账户集合 = %v，期望只有 b", ultra)
	}
	if _, err := pool.Remove("a@example.com", func(*Account) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if groups, err = pool.ClassifyCandidates(ctx, selection, nil); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(groups.StandbyReady, []string{"b@example.com"}) {
		t.Fatalf("移除账户后分类 = %+v", groups)
	}
	pool.mu.Lock()
	err = pool.verifySchedulingIndexLocked()
	pool.mu.Unlock()
	if err != nil {
		t.Fatalf("增删后索引不一致: %v", err)
	}
}

// TestSelectionRefreshIntervalScales 刷新间隔随账户数放大并有上限
func TestSelectionRefreshIntervalScales(t *testing.T) {
	for accounts, want := range map[int]time.Duration{
		0: 3 * time.Second, 999: 3 * time.Second, 1000: 6 * time.Second, 5000: 18 * time.Second, 15000: 30 * time.Second,
	} {
		if got := selectionRefreshIntervalFor(accounts); got != want {
			t.Fatalf("%d 个账户的刷新间隔 %s，期望 %s", accounts, got, want)
		}
	}
}
