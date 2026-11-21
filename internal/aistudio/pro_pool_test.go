package aistudio

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"
)

const (
	proTestFree    = "pro-free@example.com"
	proTestPlus    = "pro-plus@example.com"
	proTestUnknown = "pro-unknown@example.com"
	proTestPro     = "pro-pro@example.com"
	proTestPro2    = "pro-pro2@example.com"
	proTestUltra   = "pro-ultra@example.com"
	proTestShared  = "pro-shared-model"
	proTestCarbon  = "gemini-4-carbon"
	proTestArgon   = "gemini-4-argon"
	proTestPaid    = "gemini-3.1-pro-preview"
)

// proTestCatalog 为 Pro 号池测试的目录：共享模型与付费专用模型所有账户都列出，carbon 与 argon 只有 Pro 与 Ultra 列出（目录不标注访问模式）
func proTestCatalog(tier BenefitTier) []Model {
	models := []Model{
		{ID: proTestShared, Methods: []string{"generateContent", "countTokens"}, Capabilities: map[string]bool{"chat_model": true}},
		{ID: proTestPaid, Methods: []string{"generateContent"}, Capabilities: map[string]bool{"chat_model": true}},
	}
	if tier == BenefitTierPro || tier == BenefitTierUltra {
		models = append(models,
			Model{ID: proTestCarbon, Methods: []string{"generateContent"}, Capabilities: map[string]bool{"chat_model": true}},
			Model{ID: proTestArgon, Methods: []string{"generateContent"}, Capabilities: map[string]bool{"chat_model": true}},
		)
	}
	return models
}

// proTestPool 建立 Free、Plus、权益尚未读取（按 Free，没有目录指纹）、Pro 与 Ultra 账户各一个（pros 为额外的 Pro 账户），
// 权益规则为默认的 carbon 只限 Ultra、3.1 Pro 预览只限付费；proPool 为 PRO_POOL
func proTestPool(t *testing.T, proPool bool, pros ...string) *AccountPool {
	t.Helper()
	accounts := []*Account{
		indexTestAccount(proTestFree, BenefitTierFree, nil),
		indexTestAccount(proTestPlus, BenefitTierPlus, nil),
		indexTestAccount(proTestUnknown, BenefitTierFree, proTestCatalog(BenefitTierFree)),
		indexTestAccount(proTestPro, BenefitTierPro, nil),
		indexTestAccount(proTestUltra, BenefitTierUltra, nil),
	}
	for _, accountID := range pros {
		accounts = append(accounts, indexTestAccount(accountID, BenefitTierPro, nil))
	}
	pool := NewAccountPool(accounts, 4)
	t.Cleanup(pool.WaitBackground)
	pool.SetModelTierRules([]string{proTestCarbon}, []string{proTestPaid})
	for _, account := range accounts {
		if account.ID == proTestUnknown {
			continue
		}
		if err := pool.SetCatalog(account.ID, account.BenefitTier, proTestCatalog(account.BenefitTier)); err != nil {
			t.Fatal(err)
		}
	}
	pool.SetPoolRules(false, proPool)
	return pool
}

// TestPoolRulesMatrix 成员规则：全部权益 × ULTRA_INCLUDE_PRO × PRO_POOL。PRO_POOL=false 时与 56e69d9 的 ultraPoolTier 逐项相同；
// PRO_POOL=true 时 Pro 总在 Pro 号池（ULTRA_INCLUDE_PRO 不生效），其余与关闭时相同。权益尚未读取的账户就是 Free（没有第五种权益输入），
// 不认识的权益值按普通号池。预先算好的两张表与规则函数一致
func TestPoolRulesMatrix(t *testing.T) {
	tiers := []BenefitTier{BenefitTierFree, BenefitTierPlus, BenefitTierPro, BenefitTierUltra, BenefitTier(9), BenefitTier(-1)}
	for _, proPool := range []bool{false, true} {
		for _, includePro := range []bool{false, true} {
			rules := newPoolRules(includePro, proPool)
			for _, tier := range tiers {
				name := fmt.Sprintf("权益 %d · INCLUDE_PRO=%t · PRO_POOL=%t", tier, includePro, proPool)
				got := poolOfTier(tier, includePro, proPool)
				var want PoolScope
				switch {
				case proPool && tier == BenefitTierPro:
					want = PoolScopePro
				case baseUltraPoolTier(tier, includePro && !proPool):
					want = PoolScopeUltra
				default:
					want = PoolScopeNormal
				}
				if got != want {
					t.Fatalf("%s：poolOfTier = %v，期望 %v", name, got, want)
				}
				if !proPool && (got == PoolScopeUltra) != baseUltraPoolTier(tier, includePro) {
					t.Fatalf("%s：PRO_POOL=false 时与原规则不同", name)
				}
				if rules.partitionOf(tier) != got {
					t.Fatalf("%s：规则表 = %v，期望 %v", name, rules.partitionOf(tier), got)
				}
				for scope := range PoolScopeCount + 2 {
					if rules.allowsTier(scope, tier) != scope.Contains(got) {
						t.Fatalf("%s：号池 %d 的权益掩码与 Contains 不一致", name, scope)
					}
				}
			}
		}
	}
	// 生效的成员相同即视为没有变化：PRO_POOL=true 时 ULTRA_INCLUDE_PRO 的两个值成员相同
	on, onInclude := newPoolRules(false, true), newPoolRules(true, true)
	if !on.sameMembership(&onInclude) {
		t.Fatal("PRO_POOL=true 时切换 ULTRA_INCLUDE_PRO 不应改变成员")
	}
	off, offInclude := newPoolRules(false, false), newPoolRules(true, false)
	if off.sameMembership(&offInclude) || off.sameMembership(&on) {
		t.Fatal("PRO_POOL=false 时切换 ULTRA_INCLUDE_PRO、或切换 PRO_POOL 都应改变成员")
	}
}

// TestPoolScopeExhaustive 每个号池取值都有唯一的名称与中文名（不限号池为空），分区只有普通、Ultra 与 Pro；
// 不认识的取值什么都不包含（失败时关闭，不当成不限号池）
func TestPoolScopeExhaustive(t *testing.T) {
	names, labels := make(map[string]PoolScope), make(map[string]PoolScope)
	for scope := range PoolScopeCount {
		name, label := scope.String(), scope.Label()
		if scope == PoolScopeAll {
			if name != "" || label != "" {
				t.Fatalf("不限号池的名称应为空：%q / %q", name, label)
			}
		} else if name == "" || label == "" {
			t.Fatalf("号池 %d 缺少名称：%q / %q", scope, name, label)
		}
		if previous, exists := names[name]; exists {
			t.Fatalf("号池 %d 与 %d 同名 %q", scope, previous, name)
		}
		if previous, exists := labels[label]; exists {
			t.Fatalf("号池 %d 与 %d 中文名相同 %q", scope, previous, label)
		}
		names[name], labels[label] = scope, scope
	}
	if PoolScopePro.String() != "pro" || PoolScopePro.Label() != "Pro 号池" || PoolScopeNormalUltra.String() != "normal_ultra" {
		t.Fatalf("新号池的名称 = %q / %q / %q", PoolScopePro, PoolScopePro.Label(), PoolScopeNormalUltra)
	}
	// 原有取值的数值与名称不变：thoughtsig 按数值、api 按下标、账本按名称
	if PoolScopeAll != 0 || PoolScopeNormal != 1 || PoolScopeUltra != 2 || PoolScopePro != 3 || PoolScopeNormalUltra != 4 ||
		PoolScopeNormal.String() != "normal" || PoolScopeUltra.String() != "ultra" || PoolScopeUltra.Label() != "Ultra 号池" ||
		PoolScopeNormal.Label() != "普通号池" {
		t.Fatal("原有号池的数值或名称变了")
	}
	partitions := []PoolScope{PoolScopeNormal, PoolScopeUltra, PoolScopePro}
	for scope := range PoolScopeCount + 3 {
		if scope.IsPartition() != slices.Contains(partitions, scope) {
			t.Fatalf("号池 %d 的 IsPartition = %t", scope, scope.IsPartition())
		}
	}
	want := map[PoolScope][]PoolScope{
		PoolScopeAll: partitions, PoolScopeNormal: {PoolScopeNormal}, PoolScopeUltra: {PoolScopeUltra}, PoolScopePro: {PoolScopePro},
		PoolScopeNormalUltra: {PoolScopeNormal, PoolScopeUltra},
	}
	for scope := range PoolScopeCount + 3 {
		for partition := range PoolScopeCount + 3 {
			if got := scope.Contains(partition); got != slices.Contains(want[scope], partition) {
				t.Fatalf("号池 %d Contains(%d) = %t", scope, partition, got)
			}
		}
	}
	rules := newPoolRules(true, true)
	for _, tier := range []BenefitTier{BenefitTierFree, BenefitTierPro, BenefitTierUltra} {
		if rules.allowsTier(PoolScopeCount, tier) || rules.allowsTier(PoolScope(200), tier) {
			t.Fatalf("不认识的号池不应允许权益 %d", tier)
		}
	}
}

// TestProScopeSelection PRO_POOL=true：/pro（Pro 号池）只用 Pro；独占的普通路径只用 Free、Plus 与权益尚未读取的账户；/ultra 只用 Ultra；
// 普通号池与 Ultra 号池（ULTRA_EXCLUSIVE=false 的普通路径）不用 Pro；不限号池（后台、指定账户）用全部账户
func TestProScopeSelection(t *testing.T) {
	pool := proTestPool(t, true)
	if status, _ := pool.StatusOf(proTestUnknown); status.BenefitTierKnown || status.Pool != "normal" {
		t.Fatalf("权益尚未读取的账户：known=%t pool=%q，期望按 Free 在普通号池", status.BenefitTierKnown, status.Pool)
	}
	want := map[PoolScope][]string{
		PoolScopeAll:         {proTestFree, proTestPlus, proTestUnknown, proTestPro, proTestUltra},
		PoolScopeNormal:      {proTestFree, proTestPlus, proTestUnknown},
		PoolScopeUltra:       {proTestUltra},
		PoolScopePro:         {proTestPro},
		PoolScopeNormalUltra: {proTestFree, proTestPlus, proTestUnknown, proTestUltra},
	}
	selection := AccountSelection{ModelID: proTestShared, Method: "generateContent"}
	for scope, accounts := range want {
		ids, _ := pool.EnabledAccountsIn(scope)
		if slices.Sort(ids); !slices.Equal(ids, slices.Sorted(slices.Values(accounts))) {
			t.Fatalf("号池 %v 的启用账户 = %v，期望 %v", scope, ids, accounts)
		}
		for accountID := range acquiredAccounts(t, pool, scopeContext(scope), selection) {
			if !slices.Contains(accounts, accountID) {
				t.Fatalf("号池 %v 的请求取到了号池之外的账户 %s", scope, accountID)
			}
		}
	}
	if pool.PoolOf(proTestPro) != PoolScopePro || pool.PartitionMembers().Of(proTestPro) != PoolScopePro ||
		pool.PartitionMembers().Of(proTestFree) != PoolScopeNormal {
		t.Fatalf("Pro 账户的号池 = %v / %v", pool.PoolOf(proTestPro), pool.PartitionMembers().Of(proTestPro))
	}
	if status, _ := pool.StatusOf(proTestPro); status.Pool != "pro" || status.BenefitTier != "Pro" {
		t.Fatalf("Pro 账户状态 pool=%q tier=%q", status.Pool, status.BenefitTier)
	}
	// 普通路径固定 Pro 账户：不在号池内；不限号池（后台任务）可以用
	if _, err := pool.AcquireFor(scopeContext(PoolScopeNormal), AccountSelection{AccountID: proTestPro}); !errors.Is(err, ErrNoEligibleAccount) {
		t.Fatalf("普通路径固定 Pro：err = %v，期望没有符合条件的账户", err)
	}
	lease, err := pool.AcquireFor(context.Background(), AccountSelection{AccountID: proTestPro})
	if err != nil {
		t.Fatalf("不限号池固定 Pro 应可用：%v", err)
	}
	_ = lease.Release()
}

// TestProNeverBorrowsUltra /pro 不借用其他号池：Pro 候选全部冷却时按全部冷却（429，带最早恢复时间）返回，不取 Ultra 或 Free；
// 没有 Pro 账户时按 Pro 号池暂时不可调度（503）返回，原因写明没有 Pro 账户；PRO_POOL 关闭后仍在进行的 /pro 选号写明开关关闭
func TestProNeverBorrowsUltra(t *testing.T) {
	pool := proTestPool(t, true)
	now := time.Now()
	until := now.Add(time.Hour).Truncate(time.Second)
	if err := pool.MarkCooldownIfGeneration(proTestPro, "", pool.ModelAccessGeneration(proTestPro), now, until, "测试冷却"); err != nil {
		t.Fatal(err)
	}
	proCtx := scopeContext(PoolScopePro)
	selection := AccountSelection{ModelID: proTestShared, Method: "generateContent"}
	lease, err := pool.AcquireFor(proCtx, selection)
	var cooling *AllCoolingError
	if !errors.As(err, &cooling) || cooling.Until.IsZero() {
		if lease != nil {
			t.Fatalf("Pro 全部冷却时取到了 %s（应按全部冷却返回）", lease.Account().ID)
		}
		t.Fatalf("Pro 全部冷却：err = %v，期望 AllCoolingError", err)
	}
	if lease, _, err := pool.TryAcquireFor(proCtx, selection); err == nil && lease != nil {
		t.Fatalf("Pro 全部冷却时 TryAcquireFor 取到了 %s", lease.Account().ID)
	}

	empty := NewAccountPool([]*Account{
		indexTestAccount(proTestFree, BenefitTierFree, proTestCatalog(BenefitTierFree)),
		indexTestAccount(proTestUltra, BenefitTierUltra, proTestCatalog(BenefitTierUltra)),
	}, 2)
	t.Cleanup(empty.WaitBackground)
	empty.SetPoolRules(false, true)
	_, err = empty.AcquireFor(proCtx, selection)
	var notReady *AccountsNotReadyError
	if !errors.As(err, &notReady) || notReady.Pool != PoolScopePro || !slices.Equal(notReady.Reasons, []string{emptyProPoolReason}) ||
		notReady.HTTPStatus() != http.StatusServiceUnavailable || !strings.Contains(err.Error(), "Pro 号池：") {
		t.Fatalf("没有 Pro 账户：err = %v，期望 Pro 号池暂时不可调度（%s）", err, emptyProPoolReason)
	}
	// PRO_POOL 关闭：Pro 号池总是为空，进行中的 /pro 选号写明开关关闭（新的 /pro 请求由 api 层按 404 返回）
	pool.SetProPool(false)
	_, err = pool.AcquireFor(proCtx, AccountSelection{ModelID: proTestShared, Method: "generateContent"})
	if !errors.As(err, &notReady) || !slices.Equal(notReady.Reasons, []string{proPoolDisabledReason}) || notReady.HTTPStatus() != http.StatusServiceUnavailable {
		t.Fatalf("PRO_POOL 关闭后的 /pro 选号：err = %v，期望 %s", err, proPoolDisabledReason)
	}
}

// TestProCatalogExcludesCarbon /pro 的模型列表与选号：carbon 按 ULTRA_ONLY_MODELS 不分给 Pro（不列出、选号没有候选且模型不在列，对外 404），
// argon 与 3.1 Pro 预览可用；/ultra 照常有 carbon。普通路径只有 Pro 与 Ultra 号池能服务的模型给出改用哪个入口
func TestProCatalogExcludesCarbon(t *testing.T) {
	pool := proTestPool(t, true)
	catalog := proTestCatalog(BenefitTierUltra)
	ids := func(scope PoolScope) []string {
		var result []string
		for _, model := range pool.EligibleModelsIn(scope, catalog) {
			result = append(result, model.ID)
		}
		slices.Sort(result)
		return result
	}
	if got := ids(PoolScopePro); !slices.Equal(got, []string{proTestPaid, proTestArgon, proTestShared}) {
		t.Fatalf("/pro 模型列表 = %v，期望不含 carbon", got)
	}
	if got := ids(PoolScopeUltra); !slices.Contains(got, proTestCarbon) || !slices.Contains(got, proTestArgon) {
		t.Fatalf("/ultra 模型列表 = %v，应含 carbon 与 argon", got)
	}
	if got := ids(PoolScopeNormal); !slices.Equal(got, []string{proTestShared}) {
		t.Fatalf("普通路径模型列表 = %v，期望只有共享模型（付费模型只在 /pro 与 /ultra）", got)
	}
	carbon := AccountSelection{ModelID: proTestCarbon, Method: "generateContent", Pool: PoolScopePro}
	if _, err := pool.AcquireFor(scopeContext(PoolScopePro), carbon); !NoCandidateFailure(err) {
		t.Fatalf("/pro 请求 carbon：err = %v，期望没有候选", err)
	}
	report := pool.NoCandidateReport(carbon)
	if noServing := NewNoServingAccountError(report, time.Now()); noServing.Listed || noServing.HTTPStatus() != http.StatusNotFound {
		t.Fatalf("/pro 请求 carbon：listed=%t status=%d，期望 404 模型不在列", noServing.Listed, noServing.HTTPStatus())
	}
	if pool.ModelServableIn(PoolScopePro, proTestCarbon) || !pool.ModelServableIn(PoolScopeUltra, proTestCarbon) ||
		!pool.ModelServableIn(PoolScopePro, proTestArgon) {
		t.Fatal("ModelServableIn 与模型列表口径不一致")
	}
	generate := func(scope PoolScope, modelID string) AccountSelection {
		return AccountSelection{ModelID: modelID, Method: "generateContent", Pool: scope}
	}
	if got := pool.OtherPoolsServing(generate(PoolScopeNormal, proTestArgon), nil); !slices.Equal(got, []PoolScope{PoolScopePro, PoolScopeUltra}) {
		t.Fatalf("普通路径请求 argon 的其他号池 = %v", got)
	}
	if got := pool.OtherPoolsServing(generate(PoolScopeNormal, proTestCarbon), nil); !slices.Equal(got, []PoolScope{PoolScopeUltra}) {
		t.Fatalf("普通路径请求 carbon 的其他号池 = %v，期望只有 Ultra", got)
	}
	if got := pool.OtherPoolsServing(generate(PoolScopeNormalUltra, proTestArgon), nil); !slices.Equal(got, []PoolScope{PoolScopePro}) {
		t.Fatalf("普通号池与 Ultra 号池请求 argon 的其他号池 = %v，期望只有 Pro", got)
	}
	// 只在错误路径上调用（请求的号池服务不了），不限号池已包含全部分区，没有“其他号池”
	if got := pool.OtherPoolsServing(generate(PoolScopeAll, proTestArgon), nil); len(got) != 0 {
		t.Fatalf("不限号池不应有其他号池：%v", got)
	}
	// 后缀写法按调用方给的候选名逐个找（见 api.RequestModelBases）：原名不在目录、去掉后缀的名称在
	if got := pool.OtherPoolsServing(generate(PoolScopeNormal, proTestArgon+"-medium"), []string{proTestArgon + "-medium", proTestArgon}); !slices.Equal(got, []PoolScope{PoolScopePro, PoolScopeUltra}) {
		t.Fatalf("普通路径请求 argon-medium 的其他号池 = %v", got)
	}
	if got := pool.OtherPoolsServing(generate(PoolScopeNormal, proTestArgon+"-medium"), nil); len(got) != 0 {
		t.Fatalf("没有给去掉后缀的候选名时不应找到：%v", got)
	}
	hint := &ModelInOtherPoolError{Err: ErrModelNotFound, Model: proTestArgon, Pools: []PoolScope{PoolScopePro, PoolScopeUltra}}
	if !errors.Is(hint, ErrModelNotFound) || hint.PublicHint() != "This model is available through the /pro or /ultra endpoints." ||
		!strings.Contains(hint.Error(), "改用 /pro 或 /ultra 前缀") {
		t.Fatalf("改用提示 = %q / %q", hint.PublicHint(), hint.Error())
	}
	if only := (&ModelInOtherPoolError{Err: ErrModelNotFound, Pools: []PoolScope{PoolScopeUltra}}); only.PublicHint() != "This model is available through the /ultra endpoints." {
		t.Fatalf("只有 Ultra 时的提示 = %q", only.PublicHint())
	}
	if !pool.TierRulesAllow(proTestArgon, BenefitTierPro) || pool.TierRulesAllow(proTestCarbon, BenefitTierPro) ||
		!pool.TierRulesAllow("models/GEMINI-4-CARBON", BenefitTierUltra) || pool.TierRulesAllow(proTestPaid, BenefitTierFree) {
		t.Fatal("TierRulesAllow 与权益规则不一致")
	}
}

// TestSetPoolRulesHotSwitch 热切换 PRO_POOL：每次实际切换规则版本只加一，成员、按号池的目录缓存与出口通道随之重算（Pro 与普通分区之间的移动
// 不换出口通道，只有 Ultra 用 Ultra 通道）；重复设置、以及 PRO_POOL=true 时只切换 ULTRA_INCLUDE_PRO 都返回没有变化、不作废缓存
func TestSetPoolRulesHotSwitch(t *testing.T) {
	pool := proTestPool(t, false)
	catalog := proTestCatalog(BenefitTierUltra)
	has := func(scope PoolScope, modelID string) bool {
		return slices.ContainsFunc(pool.EligibleModelsIn(scope, catalog), func(model Model) bool { return model.ID == modelID })
	}
	lane := func(accountID string) EgressLane {
		_, lane := pool.EgressKeyOf(accountID)
		return lane
	}
	checkEgress := func(step string) {
		t.Helper()
		pool.mu.Lock()
		err := pool.verifyEgressLocked()
		pool.mu.Unlock()
		if err != nil {
			t.Fatalf("%s：出口通道不一致：%v", step, err)
		}
		if lane(proTestUltra) != EgressLaneUltra || lane(proTestFree) != EgressLaneNormal {
			t.Fatalf("%s：Ultra 应在 Ultra 通道、Free 在普通通道", step)
		}
	}
	type step struct {
		name                string
		includePro, proPool bool
		changed             bool
		proPartition        PoolScope
		proLane             EgressLane
		argonNormal         bool
	}
	steps := []step{
		{name: "开启 PRO_POOL", proPool: true, changed: true, proPartition: PoolScopePro, proLane: EgressLaneNormal},
		{name: "重复开启", proPool: true, proPartition: PoolScopePro, proLane: EgressLaneNormal},
		{name: "PRO_POOL=true 时开启 ULTRA_INCLUDE_PRO", includePro: true, proPool: true, proPartition: PoolScopePro, proLane: EgressLaneNormal},
		{name: "关闭 PRO_POOL（并入 Ultra）", includePro: true, changed: true, proPartition: PoolScopeUltra, proLane: EgressLaneUltra},
		{name: "再开启 PRO_POOL", includePro: true, proPool: true, changed: true, proPartition: PoolScopePro, proLane: EgressLaneNormal},
		{name: "两个都关闭", changed: true, proPartition: PoolScopeNormal, proLane: EgressLaneNormal, argonNormal: true},
	}
	// 关闭时 argon 只有 Pro 与 Ultra 列出，Pro 在普通号池时普通路径能服务它
	if !has(PoolScopeNormal, proTestArgon) {
		t.Fatal("初始（两个开关都关闭）普通路径应能服务 argon")
	}
	checkEgress("初始")
	for _, step := range steps {
		version := pool.PoolRuleVersion()
		snapshot := pool.PartitionMembers()
		has(PoolScopeNormal, proTestArgon) // 先建好缓存
		if got := pool.SetPoolRules(step.includePro, step.proPool); got != step.changed {
			t.Fatalf("%s：SetPoolRules = %t，期望 %t", step.name, got, step.changed)
		}
		wantVersion := version
		if step.changed {
			wantVersion++
		}
		if pool.PoolRuleVersion() != wantVersion {
			t.Fatalf("%s：规则版本 = %d，期望 %d", step.name, pool.PoolRuleVersion(), wantVersion)
		}
		if next := pool.PartitionMembers(); sameMap(snapshot, next) == step.changed {
			t.Fatalf("%s：成员快照是否重建与是否变化不符", step.name)
		}
		if got := pool.PartitionMembers().Of(proTestPro); got != step.proPartition {
			t.Fatalf("%s：Pro 账户分区 = %v，期望 %v", step.name, got, step.proPartition)
		}
		if pool.ProPool() != step.proPool || pool.UltraIncludePro() != (step.includePro && !step.proPool) {
			t.Fatalf("%s：ProPool=%t UltraIncludePro=%t", step.name, pool.ProPool(), pool.UltraIncludePro())
		}
		if got := has(PoolScopeNormal, proTestArgon); got != step.argonNormal {
			t.Fatalf("%s：普通路径能服务 argon = %t，期望 %t（按号池缓存的模型通道应重算）", step.name, got, step.argonNormal)
		}
		if got := has(PoolScopePro, proTestArgon); got != step.proPool {
			t.Fatalf("%s：/pro 能服务 argon = %t", step.name, got)
		}
		if lane(proTestPro) != step.proLane {
			t.Fatalf("%s：Pro 账户出口通道 = %d，期望 %d", step.name, lane(proTestPro), step.proLane)
		}
		checkEgress(step.name)
		verifyIndex(t, pool, step.name)
	}
	// SetUltraIncludePro 在锁内读当前的 PRO_POOL：开启时只保存原值
	pool.SetProPool(true)
	if pool.SetUltraIncludePro(true) || pool.PartitionMembers().Of(proTestPro) != PoolScopePro {
		t.Fatal("PRO_POOL=true 时 SetUltraIncludePro 不应改变成员")
	}
	if !pool.SetProPool(false) || pool.PartitionMembers().Of(proTestPro) != PoolScopeUltra {
		t.Fatal("关闭 PRO_POOL 后应按保存的 ULTRA_INCLUDE_PRO=true 并入 Ultra")
	}
}

// TestUnknownTierMovesToPro 权益尚未读取的账户按 Free 在普通分区，读到 Pro 后换到 Pro 分区：只在分区实际变化时重建成员快照，
// 冷却等无关变化不重建；普通与 Pro 之间的移动不换出口通道
func TestUnknownTierMovesToPro(t *testing.T) {
	pool := proTestPool(t, true)
	if pool.PartitionMembers().Of(proTestUnknown) != PoolScopeNormal {
		t.Fatal("权益尚未读取的账户应在普通分区")
	}
	snapshot := pool.PartitionMembers()
	now := time.Now()
	if err := pool.MarkCooldownIfGeneration(proTestUnknown, "", pool.ModelAccessGeneration(proTestUnknown), now, now.Add(time.Hour), "测试冷却"); err != nil {
		t.Fatal(err)
	}
	if !sameMap(snapshot, pool.PartitionMembers()) {
		t.Fatal("冷却变化不应重建成员快照")
	}
	pool.mu.Lock()
	egressBefore := pool.byID[proTestUnknown].egress
	pool.mu.Unlock()
	if err := pool.SetCatalog(proTestUnknown, BenefitTierPro, proTestCatalog(BenefitTierPro)); err != nil {
		t.Fatal(err)
	}
	next := pool.PartitionMembers()
	if sameMap(snapshot, next) || next.Of(proTestUnknown) != PoolScopePro {
		t.Fatalf("读到 Pro 后分区 = %v，快照应重建", next.Of(proTestUnknown))
	}
	pool.mu.Lock()
	egressAfter := pool.byID[proTestUnknown].egress
	pool.mu.Unlock()
	if egressBefore == nil || egressAfter != egressBefore {
		t.Fatal("普通分区到 Pro 分区的移动不应换出口通道")
	}
	if status, _ := pool.StatusOf(proTestUnknown); !status.BenefitTierKnown || status.Pool != "pro" {
		t.Fatalf("读到权益后状态 known=%t pool=%q", status.BenefitTierKnown, status.Pool)
	}
	ids, _ := pool.EnabledAccountsIn(PoolScopePro)
	if slices.Sort(ids); !slices.Equal(ids, []string{proTestPro, proTestUnknown}) {
		t.Fatalf("Pro 号池账户 = %v", ids)
	}
	verifyIndex(t, pool, "读到 Pro 后")
	if _, err := pool.Remove(proTestUnknown, func(*Account) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if _, member := pool.PartitionMembers()[proTestUnknown]; member {
		t.Fatal("移除后不应留在成员集合")
	}
}

// TestResourcePoolMismatchProTexts 资源号池不符的四种文案：Pro 账户的资源指向 /pro；/pro 请求普通号池的资源去掉 /pro 前缀；
// Ultra 与 /ultra 的原有文案逐字不变
func TestResourcePoolMismatchProTexts(t *testing.T) {
	for _, test := range []struct {
		owner, request PoolScope
		want           string
	}{
		{PoolScopePro, PoolScopeNormal, "resource f belongs to an account of the Pro pool; request it through the /pro endpoints"},
		{PoolScopePro, PoolScopeUltra, "resource f belongs to an account of the Pro pool; request it through the /pro endpoints"},
		{PoolScopeUltra, PoolScopePro, "resource f belongs to an account of the Ultra pool; request it through the /ultra endpoints"},
		{PoolScopeUltra, PoolScopeNormal, "resource f belongs to an account of the Ultra pool; request it through the /ultra endpoints"},
		{PoolScopeNormal, PoolScopePro, "resource f belongs to an account of the normal pool; request it without the /pro prefix"},
		{PoolScopeNormal, PoolScopeUltra, "resource f belongs to an account of the normal pool; request it without the /ultra prefix"},
	} {
		err := &ResourcePoolMismatchError{ResourceID: "f", Owner: test.owner, Request: test.request}
		if err.Error() != test.want {
			t.Fatalf("Owner=%v Request=%v：%q，期望 %q", test.owner, test.request, err.Error(), test.want)
		}
	}
	pool := proTestPool(t, true)
	bindTestFile(t, pool, proTestPro, "files/pro")
	bindTestFile(t, pool, proTestFree, "files/free")
	var mismatch *ResourcePoolMismatchError
	if _, err := pool.AcquireFor(scopeContext(PoolScopeNormal), AccountSelection{ResourceID: "files/pro"}); !errors.As(err, &mismatch) ||
		mismatch.Owner != PoolScopePro || !strings.Contains(err.Error(), "/pro endpoints") {
		t.Fatalf("普通路径引用 Pro 账户的文件：err = %v", err)
	}
	if _, err := pool.AcquireFor(scopeContext(PoolScopePro), AccountSelection{ResourceID: "files/free"}); !errors.As(err, &mismatch) ||
		mismatch.Request != PoolScopePro || !strings.Contains(err.Error(), "without the /pro prefix") {
		t.Fatalf("/pro 引用普通号池的文件：err = %v", err)
	}
	lease, err := pool.AcquireFor(scopeContext(PoolScopePro), AccountSelection{ResourceID: "files/pro"})
	if err != nil || lease.Account().ID != proTestPro {
		t.Fatalf("/pro 引用 Pro 账户的文件应可用：%v", err)
	}
	_ = lease.Release()
}

// TestTierPoolOfIgnoresProPool 额度共享学习按真实权益分组，不随 PRO_POOL 变化：Pro 仍与 Free 同组（持久化的键不变）
func TestTierPoolOfIgnoresProPool(t *testing.T) {
	pool := proTestPool(t, true)
	if pool.TierPoolOf(proTestPro) != PoolScopeNormal || pool.TierPoolOf(proTestUltra) != PoolScopeUltra || pool.TierPoolOf(proTestFree) != PoolScopeNormal {
		t.Fatal("TierPoolOf 应只按真实权益")
	}
	pool.SetPoolRules(true, false)
	if pool.TierPoolOf(proTestPro) != PoolScopeNormal {
		t.Fatal("ULTRA_INCLUDE_PRO 并入后 TierPoolOf 仍按真实权益")
	}
}

// TestBootstrapAndStateCountsPro 预热概况与状态计数按三个分区：Pro 可预热数、Pro 状态计数，普通分区的数量不含 Pro
func TestBootstrapAndStateCountsPro(t *testing.T) {
	pool := proTestPool(t, true, proTestPro2)
	if err := pool.MarkAuthRequired(proTestPro2, "Cookie 失效"); err != nil {
		t.Fatal(err)
	}
	summary := pool.BootstrapSummary()
	// indexTestAccount 的账户都有初始化模型；需要登录的 Pro 不可预热
	if summary.Available != 5 || summary.UltraAvailable != 1 || summary.ProAvailable != 1 ||
		summary.AvailableIn(PoolScopeNormal) != 3 || summary.AvailableIn(PoolScopePro) != 1 || summary.AvailableIn(PoolScopeAll) != 0 {
		t.Fatalf("预热概况 = %+v", summary)
	}
	all, partitions := pool.PoolStateCounts()
	if all.Total != 6 || partitions[PoolScopePro].Total != 2 || partitions[PoolScopePro].AuthRequired != 1 ||
		partitions[PoolScopeUltra].Total != 1 || partitions[PoolScopeNormal].Total != 3 || partitions[PoolScopeAll].Total != 0 {
		t.Fatalf("状态计数 = %+v / %+v", all, partitions)
	}
	pool.SetProPool(false)
	if summary := pool.BootstrapSummary(); summary.ProAvailable != 0 || summary.AvailableIn(PoolScopeNormal) != 4 {
		t.Fatalf("PRO_POOL 关闭后预热概况 = %+v", summary)
	}
}

// TestEmptyPoolReasonsPro 号池为空的原因：PRO_POOL=true 且账户都属于 Pro 或 Ultra 号池时普通路径写明 Pro 只服务 /pro；
// 普通号池与 Ultra 号池为空（只有 Pro 账户）同样写明；开关都关闭时原因逐字不变；Ultra 与 Pro 号池的错误写明号池
func TestEmptyPoolReasonsPro(t *testing.T) {
	newPool := func(proPool bool, tiers ...BenefitTier) *AccountPool {
		accounts := make([]*Account, 0, len(tiers))
		for index, tier := range tiers {
			accounts = append(accounts, indexTestAccount(fmt.Sprintf("empty-%d@example.com", index), tier, proTestCatalog(tier)))
		}
		pool := NewAccountPool(accounts, 2)
		t.Cleanup(pool.WaitBackground)
		pool.SetPoolRules(false, proPool)
		return pool
	}
	reason := func(pool *AccountPool, scope PoolScope) string {
		var notReady *AccountsNotReadyError
		if err := pool.NoEligibleError(AccountSelection{Pool: scope}); errors.As(err, &notReady) && len(notReady.Reasons) == 1 {
			return notReady.Reasons[0]
		}
		return ""
	}
	for _, test := range []struct {
		name    string
		pool    *AccountPool
		scope   PoolScope
		want    string
		labeled bool
	}{
		{"开启、Pro 与 Ultra", newPool(true, BenefitTierPro, BenefitTierUltra), PoolScopeNormal, emptyNormalPoolProReason, false},
		{"开启、只有 Ultra", newPool(true, BenefitTierUltra), PoolScopeNormal, emptyNormalPoolReason, false},
		{"开启、只有 Pro（非独占）", newPool(true, BenefitTierPro), PoolScopeNormalUltra, emptyNormalUltraPoolReason, false},
		{"开启、没有 Pro", newPool(true, BenefitTierFree), PoolScopePro, emptyProPoolReason, true},
		{"关闭、只有 Ultra", newPool(false, BenefitTierUltra, BenefitTierUltra), PoolScopeNormal, "账户都是 Ultra 账户，ULTRA_EXCLUSIVE=true 时只服务 /ultra 前缀的请求", false},
		{"关闭、没有 Ultra", newPool(false, BenefitTierPro), PoolScopeUltra, "没有权益为 Ultra 的账户", true},
		{"关闭、/pro", newPool(false, BenefitTierPro), PoolScopePro, proPoolDisabledReason, true},
	} {
		if got := reason(test.pool, test.scope); got != test.want {
			t.Fatalf("%s：原因 = %q，期望 %q", test.name, got, test.want)
		}
		err := test.pool.NoEligibleError(AccountSelection{Pool: test.scope})
		if labeled := strings.Contains(err.Error(), test.scope.Label()+"："); labeled != test.labeled {
			t.Fatalf("%s：错误 %q 是否写明号池 = %t，期望 %t", test.name, err.Error(), labeled, test.labeled)
		}
	}
	busy := &CapacityBusyError{ModelID: "m", Pool: PoolScopePro, Reason: "测试"}
	if !strings.Contains(busy.Error(), "Pro 号池，已排队") {
		t.Fatalf("Pro 号池繁忙的文案 = %q", busy.Error())
	}
	if normal := (&CapacityBusyError{ModelID: "m", Pool: PoolScopeNormalUltra, Reason: "测试"}); strings.Contains(normal.Error(), "号池，已排队") {
		t.Fatalf("普通路径繁忙的文案不应写明号池：%q", normal.Error())
	}
	quota := &PrimaryQuotaError{ModelID: proTestArgon, Pool: PoolScopePro, Reason: "测试"}
	if !strings.HasPrefix(quota.Error(), "额度不足：Pro 号池的 Ultra 主用模型 "+proTestArgon) {
		t.Fatalf("Pro 号池主用模型额度不足的文案 = %q（诊断脚本按“额度不足：”与“主用模型”识别）", quota.Error())
	}
}

// TestProPoolOffParity PRO_POOL=false 时与只有两个号池时（56e69d9）逐项相同：随机的权益与 ULTRA_INCLUDE_PRO 下，各号池的成员、
// 账户的号池与状态名、出口通道、预热概况与状态计数都按原来的 ultraPoolTier 推出，Pro 号池为空
func TestProPoolOffParity(t *testing.T) {
	random := rand.New(rand.NewPCG(7, 11))
	tiers := []BenefitTier{BenefitTierFree, BenefitTierPlus, BenefitTierPro, BenefitTierUltra}
	for round := range 40 {
		includePro := round%2 == 1
		accounts := make([]*Account, 0, 12)
		for index := range 12 {
			tier := tiers[random.IntN(len(tiers))]
			accounts = append(accounts, indexTestAccount(fmt.Sprintf("parity-%d-%d@example.com", round, index), tier, indexTestCatalog()))
		}
		pool := NewAccountPool(accounts, 2)
		pool.SetPoolRules(includePro, false)
		baseOf := func(account *Account) PoolScope {
			if baseUltraPoolTier(account.BenefitTier, includePro) {
				return PoolScopeUltra
			}
			return PoolScopeNormal
		}
		wantIDs := map[PoolScope][]string{}
		wantUltra := 0
		for _, account := range accounts {
			partition := baseOf(account)
			wantIDs[partition] = append(wantIDs[partition], account.ID)
			wantIDs[PoolScopeAll] = append(wantIDs[PoolScopeAll], account.ID)
			if partition == PoolScopeUltra {
				wantUltra++
			}
			if pool.PoolOf(account.ID) != partition || pool.PartitionMembers().Of(account.ID) != partition {
				t.Fatalf("第 %d 轮：%s 的号池与原规则不同", round, account.ID)
			}
			if status, _ := pool.StatusOf(account.ID); status.Pool != partition.String() {
				t.Fatalf("第 %d 轮：%s 的状态号池 = %q", round, account.ID, status.Pool)
			}
			wantLane := EgressLaneNormal
			if partition == PoolScopeUltra {
				wantLane = EgressLaneUltra
			}
			if _, lane := pool.EgressKeyOf(account.ID); lane != wantLane {
				t.Fatalf("第 %d 轮：%s 的出口通道与原规则不同", round, account.ID)
			}
		}
		for _, scope := range []PoolScope{PoolScopeAll, PoolScopeNormal, PoolScopeUltra, PoolScopePro} {
			ids, _ := pool.EnabledAccountsIn(scope)
			slices.Sort(ids)
			want := slices.Sorted(slices.Values(wantIDs[scope]))
			if !slices.Equal(ids, want) {
				t.Fatalf("第 %d 轮：号池 %v 的账户 = %v，期望 %v", round, scope, ids, want)
			}
		}
		summary := pool.BootstrapSummary()
		_, partitions := pool.PoolStateCounts()
		if summary.UltraAvailable != wantUltra || summary.ProAvailable != 0 || partitions[PoolScopeUltra].Total != wantUltra ||
			partitions[PoolScopePro].Total != 0 || pool.ProPool() {
			t.Fatalf("第 %d 轮：预热概况 %+v，状态计数 %+v，期望 Ultra %d、Pro 0", round, summary, partitions, wantUltra)
		}
		if members := pool.PartitionMembers(); members.Count(PoolScopePro) != 0 || members.Count(PoolScopeUltra) != wantUltra ||
			len(members) != wantUltra {
			t.Fatalf("第 %d 轮：成员集合 = %v", round, members)
		}
		pool.WaitBackground()
	}
}

// TestTierRuleReportUltraMarked 规则对照列出目录明确标注只有 Ultra 能用的模型（供 PRO_POOL=true 时核对 Pro 号池能否服务）
func TestTierRuleReportUltraMarked(t *testing.T) {
	marked := Model{ID: "ultra-marked", Methods: []string{"generateContent"}, AccessModes: []int64{4}}
	paid := Model{ID: "paid-marked", Methods: []string{"generateContent"}, AccessModes: []int64{3}}
	pool := NewAccountPool([]*Account{indexTestAccount(proTestUltra, BenefitTierUltra, []Model{marked, paid})}, 2)
	t.Cleanup(pool.WaitBackground)
	if report := pool.TierRuleReport(); !report.Checked || !slices.Equal(report.UltraMarked, []string{"ultra-marked"}) {
		t.Fatalf("UltraMarked = %v", report.UltraMarked)
	}
}

// TestOtherPoolsServingMethodAndCapability 改用入口的判断按请求的方法与能力：模型在 Pro 号池目录里但不支持请求的方法或能力时
// （普通号池同样列出、同样不支持），/pro 同样接不了，不提示；只有 Pro 号池支持这个方法时才提示
func TestOtherPoolsServingMethodAndCapability(t *testing.T) {
	embed := Model{ID: "embed-x", Methods: []string{"embedContent"}}
	chat := Model{ID: "chat-x", Methods: []string{"generateContent"}, Capabilities: map[string]bool{"chat_model": true}}
	live := Model{ID: "live-x", Methods: []string{"generateContent"}, Capabilities: map[string]bool{"chat_model": true}}
	proLive := Model{ID: "live-x", Methods: []string{"generateContent", "bidiGenerateContent"}, Capabilities: map[string]bool{"chat_model": true}}
	accounts := []*Account{
		indexTestAccount(proTestFree, BenefitTierFree, nil),
		indexTestAccount(proTestPro, BenefitTierPro, nil),
	}
	pool := NewAccountPool(accounts, 4)
	t.Cleanup(pool.WaitBackground)
	if err := pool.SetCatalog(proTestFree, BenefitTierFree, []Model{embed, chat, live}); err != nil {
		t.Fatal(err)
	}
	if err := pool.SetCatalog(proTestPro, BenefitTierPro, []Model{embed, chat, proLive}); err != nil {
		t.Fatal(err)
	}
	pool.SetPoolRules(false, true)
	for _, selection := range []AccountSelection{
		{ModelID: "embed-x", Method: "generateContent", Pool: PoolScopeNormal},
		{ModelID: "chat-x", Method: "bidiGenerateContent", Pool: PoolScopeNormal},
		{ModelID: "chat-x", Method: "generateContent", Capability: "transcription_output", Pool: PoolScopeNormal},
	} {
		if got := pool.OtherPoolsServing(selection, nil); len(got) != 0 {
			t.Errorf("%s/%s/%s：/pro 同样接不了，不应提示改用 %v", selection.ModelID, selection.Method, selection.Capability, got)
		}
	}
	if got := pool.OtherPoolsServing(AccountSelection{ModelID: "live-x", Method: "bidiGenerateContent", Pool: PoolScopeNormal}, nil); !slices.Equal(got, []PoolScope{PoolScopePro}) {
		t.Fatalf("只有 Pro 号池支持 Live 的模型应提示 /pro：%v", got)
	}
	if got := pool.OtherPoolsServing(AccountSelection{ModelID: "embed-x", Method: "embedContent", Pool: PoolScopeNormal}, nil); !slices.Equal(got, []PoolScope{PoolScopePro}) {
		t.Fatalf("方法支持时按模型判断：%v", got)
	}
}

// TestOtherPoolsServingSkipsUnlistedNames 不存在的模型名先按号池目录汇总排除：不按账户查通道，Pro 与 Ultra 号池的通道记录不随名称增长
func TestOtherPoolsServingSkipsUnlistedNames(t *testing.T) {
	pool := proTestPool(t, true)
	for index := range 50 {
		name := fmt.Sprintf("bogus-%d", index)
		if got := pool.OtherPoolsServing(AccountSelection{ModelID: name, Method: "generateContent", Pool: PoolScopeNormal}, []string{name, name + "-x"}); len(got) != 0 {
			t.Fatalf("不存在的模型 %s = %v", name, got)
		}
	}
	pool.mu.Lock()
	defer pool.mu.Unlock()
	for _, scope := range []PoolScope{PoolScopePro, PoolScopeUltra} {
		if memo := pool.modelChannelsMemos[scope]; memo != nil && len(memo.channels) != 0 {
			t.Fatalf("号池 %v 的通道记录不应因不存在的模型名增长：%d 条", scope, len(memo.channels))
		}
	}
}

// TestBuildModelLookupSkipsUnlistedNames 开启 Build 通道时，任何账户 Build 目录都没有的模型名（含后缀写法剥出的多个候选名）在改用入口
// 提示与选号校验里都不逐个账户扫描 Build 目录；只在 Build 目录列出的模型照常按账户核对，新同步的 Build 目录立即生效
func TestBuildModelLookupSkipsUnlistedNames(t *testing.T) {
	pool := proTestPool(t, true)
	pool.SetUpstreamChannels([]Channel{ChannelPlayground, ChannelBuild})
	buildOnly := Model{ID: "build-only-x", Methods: []string{"generateContent"}}
	if err := pool.SetBuildCatalog(proTestPro, []Model{buildOnly}); err != nil {
		t.Fatal(err)
	}
	before := buildModelScans.Load()
	for index := range 50 {
		name := fmt.Sprintf("long-bogus-%d-high", index)
		names := []string{name, fmt.Sprintf("long-bogus-%d", index), fmt.Sprintf("bogus-%d-high", index), fmt.Sprintf("bogus-%d", index)}
		if got := pool.OtherPoolsServing(AccountSelection{ModelID: name, Method: "generateContent", Pool: PoolScopeNormal}, names); len(got) != 0 {
			t.Fatalf("不存在的模型 %s = %v", name, got)
		}
		if pool.ModelListedIn(PoolScopeNormal, names[3]) || pool.ModelListedIn(PoolScopePro, names[0]) {
			t.Fatalf("不存在的模型 %s 不应列出", names[3])
		}
	}
	if scans := buildModelScans.Load() - before; scans != 0 {
		t.Fatalf("不存在的模型名不应逐个账户扫描 Build 目录，实际扫描 %d 次", scans)
	}
	if got := pool.OtherPoolsServing(AccountSelection{ModelID: buildOnly.ID, Method: "generateContent", Pool: PoolScopeNormal}, nil); !slices.Equal(got, []PoolScope{PoolScopePro}) {
		t.Fatalf("只有 Pro 账户 Build 目录列出的模型应提示 /pro：%v", got)
	}
	if buildModelScans.Load() == before {
		t.Fatal("Build 目录列出的模型应照常按账户核对")
	}
	if !pool.ModelListedIn(PoolScopePro, buildOnly.ID) || pool.ModelListedIn(PoolScopeNormal, buildOnly.ID) {
		t.Fatal("Build 目录列出的模型按号池判断")
	}
	fresh := Model{ID: "build-fresh-y", Methods: []string{"generateContent"}}
	if got := pool.OtherPoolsServing(AccountSelection{ModelID: fresh.ID, Method: "generateContent", Pool: PoolScopeNormal}, nil); len(got) != 0 {
		t.Fatalf("还没有账户列出的模型 = %v", got)
	}
	if err := pool.SetBuildCatalog(proTestUltra, []Model{fresh}); err != nil {
		t.Fatal(err)
	}
	if got := pool.OtherPoolsServing(AccountSelection{ModelID: fresh.ID, Method: "generateContent", Pool: PoolScopeNormal}, nil); !slices.Equal(got, []PoolScope{PoolScopeUltra}) {
		t.Fatalf("新同步的 Build 目录应立即生效：%v", got)
	}
}
