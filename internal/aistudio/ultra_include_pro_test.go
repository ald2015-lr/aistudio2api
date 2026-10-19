package aistudio

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ULTRA_INCLUDE_PRO 的测试：开关开启后权益为 Pro 的账户并入 Ultra 号池，按 Ultra 号池的全部规则调度，
// 账户的真实权益（模型资格、输入上限的权益比较、额度共享分组、状态里的权益）不变

const (
	includeProModel = "pro-model"
	includeProAlias = "pro-latest"
	includeProBuild = "build-only-model"
)

// verifyIndex 在锁内同步派生缓存后核对候选索引与账户状态一致
func verifyIndex(t *testing.T, pool *AccountPool, step string) {
	t.Helper()
	pool.mu.Lock()
	defer pool.mu.Unlock()
	pool.syncDerivedLocked(time.Now())
	if err := pool.verifySchedulingIndexLocked(); err != nil {
		t.Fatalf("%s：候选索引与账户状态不一致: %v", step, err)
	}
}

// sameMap 判断两次返回的是不是同一份 map（写时复制的快照没有重建）
func sameMap(left map[string]struct{}, right map[string]struct{}) bool {
	return reflect.ValueOf(left).UnsafePointer() == reflect.ValueOf(right).UnsafePointer()
}

func modelIDs(models []Model) []string {
	ids := make([]string, 0, len(models))
	for _, model := range models {
		ids = append(ids, model.ID)
	}
	return ids
}

// TestUltraPoolTierMembership 号池成员的唯一判定：Ultra 总是属于 Ultra 号池，开关开启时 Pro 也属于；
// Free、Plus 与未识别的权益始终在普通号池，不限号池时都允许
func TestUltraPoolTierMembership(t *testing.T) {
	unknown := BenefitTier(9)
	for _, test := range []struct {
		tier       BenefitTier
		includePro bool
		ultra      bool
	}{
		{tier: BenefitTierFree, ultra: false},
		{tier: BenefitTierFree, includePro: true, ultra: false},
		{tier: BenefitTierPlus, ultra: false},
		{tier: BenefitTierPlus, includePro: true, ultra: false},
		{tier: BenefitTierPro, ultra: false},
		{tier: BenefitTierPro, includePro: true, ultra: true},
		{tier: BenefitTierUltra, ultra: true},
		{tier: BenefitTierUltra, includePro: true, ultra: true},
		{tier: unknown, ultra: false},
		{tier: unknown, includePro: true, ultra: false},
	} {
		name := fmt.Sprintf("权益 %d 并入=%t", test.tier, test.includePro)
		if got := ultraPoolTier(test.tier, test.includePro); got != test.ultra {
			t.Fatalf("%s：ultraPoolTier = %t，期望 %t", name, got, test.ultra)
		}
		wantPool := PoolScopeNormal
		if test.ultra {
			wantPool = PoolScopeUltra
		}
		if got := poolOfTier(test.tier, test.includePro); got != wantPool {
			t.Fatalf("%s：poolOfTier = %v，期望 %v", name, got, wantPool)
		}
		if !PoolScopeAll.allowsTier(test.tier, test.includePro) {
			t.Fatalf("%s：不限号池应允许任何权益", name)
		}
		if got := PoolScopeUltra.allowsTier(test.tier, test.includePro); got != test.ultra {
			t.Fatalf("%s：Ultra 号池 allowsTier = %t，期望 %t", name, got, test.ultra)
		}
		if got := PoolScopeNormal.allowsTier(test.tier, test.includePro); got == test.ultra {
			t.Fatalf("%s：普通号池 allowsTier = %t，期望 %t", name, got, !test.ultra)
		}
		account := &Account{ID: "a", BenefitTier: test.tier}
		if PoolScopeUltra.allows(account, test.includePro) != test.ultra || PoolScopeNormal.allows(nil, test.includePro) {
			t.Fatalf("%s：allows 与 allowsTier 不一致", name)
		}
	}
}

// TestUltraIncludeProDefaultOff 不调用开关时与原行为一致：Pro 属于普通号池
func TestUltraIncludeProDefaultOff(t *testing.T) {
	pool := scopeTestPool(t, map[string]BenefitTier{scopeNormalA: BenefitTierFree, scopeNormalB: BenefitTierPro, scopeUltraA: BenefitTierUltra})
	if pool.UltraIncludePro() || pool.PoolRuleVersion() != 0 {
		t.Fatalf("默认开关 = %t，规则版本 = %d，期望关闭、版本 0", pool.UltraIncludePro(), pool.PoolRuleVersion())
	}
	if pool.PoolOf(scopeNormalB) != PoolScopeNormal || pool.PoolOf(scopeUltraA) != PoolScopeUltra {
		t.Fatal("默认时 Pro 应在普通号池、Ultra 在 Ultra 号池")
	}
	if ultra := pool.UltraAccountIDs(); len(ultra) != 1 {
		t.Fatalf("默认时 Ultra 号池成员 = %v，期望只有 Ultra 账户", ultra)
	}
	for _, status := range pool.Status() {
		want := "normal"
		if status.ID == scopeUltraA {
			want = "ultra"
		}
		if status.Pool != want {
			t.Fatalf("默认时账户 %s 的号池 = %q，期望 %q", status.ID, status.Pool, want)
		}
	}
	if ids, _ := pool.EnabledAccountsIn(PoolScopeUltra); !slices.Equal(ids, []string{scopeUltraA}) {
		t.Fatalf("默认时 Ultra 号池启用账户 = %v", ids)
	}
	if ids, _ := pool.EnabledAccountsIn(PoolScopeNormal); len(ids) != 2 || !slices.Contains(ids, scopeNormalB) {
		t.Fatalf("默认时普通号池启用账户 = %v，期望含 Pro", ids)
	}
	if nilPool := (*AccountPool)(nil); nilPool.SetUltraIncludePro(true) || nilPool.UltraIncludePro() || nilPool.PoolRuleVersion() != 0 {
		t.Fatal("nil 账户池的开关方法应安全返回零值")
	}
}

// TestUltraIncludeProToggleMovesAccount 开关切换后 Pro 账户立即整体换到 Ultra 号池：取租约、候选、目录、别名、计数、
// 预热概况、可接替统计、资源号池与状态列表都按新成员；持有的 Pro 租约不受影响，释放后索引一致；再关闭全部恢复
func TestUltraIncludeProToggleMovesAccount(t *testing.T) {
	const pro = scopeNormalB
	pool := scopeTestPool(t, map[string]BenefitTier{scopeNormalA: BenefitTierFree, pro: BenefitTierPro, scopeUltraA: BenefitTierUltra})
	if err := pool.SetCatalog(pro, BenefitTierPro, []Model{
		{ID: scopeSharedModel, Methods: []string{"generateContent", "countTokens"}, Capabilities: map[string]bool{"chat_model": true}},
		{
			ID: includeProModel, Methods: []string{"generateContent"}, Capabilities: map[string]bool{"chat_model": true},
			CapabilityOptions: map[string][]string{"aliases": {includeProAlias}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	// 账户超过 statusCacheMinAccounts 时 Status 走缓存，切换必须作废它
	for index := 0; index <= statusCacheMinAccounts; index++ {
		filler := indexTestAccount(fmt.Sprintf("filler-%03d@example.com", index), BenefitTierFree, nil)
		if err := pool.Add(filler); err != nil {
			t.Fatal(err)
		}
	}
	bindTestFile(t, pool, pro, "files/pro")
	catalog := []Model{{ID: scopeSharedModel, Methods: []string{"generateContent"}}, {ID: includeProModel, Methods: []string{"generateContent"}}}
	shared := AccountSelection{ModelID: scopeSharedModel, Method: "generateContent"}
	ultraCtx, normalCtx := scopeContext(PoolScopeUltra), scopeContext(PoolScopeNormal)

	// 切换前先让各缓存都建好
	warmCaches := func() {
		t.Helper()
		pool.EligibleModelsIn(PoolScopeUltra, catalog)
		pool.EligibleModelsIn(PoolScopeNormal, catalog)
		pool.CanonicalModelIDIn(PoolScopeUltra, includeProAlias)
		pool.CanonicalModelIDIn(PoolScopeNormal, includeProAlias)
		for _, scope := range []PoolScope{PoolScopeNormal, PoolScopeUltra} {
			if _, err := pool.ClassifyCandidatesCached(AccountSelection{ModelID: scopeSharedModel, Method: "generateContent", Pool: scope}, nil); err != nil {
				t.Fatalf("预先分类 %v 失败: %v", scope, err)
			}
			pool.SpareAccounts(scope, nil, []string{scopeSharedModel}, 0, 0)
		}
		pool.ServeableCounts([]string{pro, scopeUltraA}, []string{scopeSharedModel}, 0)
		pool.Status()
		pool.UltraAccountIDs()
		for _, ctx := range []context.Context{ultraCtx, normalCtx} {
			lease, err := pool.AcquireFor(ctx, shared)
			if err != nil {
				t.Fatal(err)
			}
			_ = lease.Release()
		}
	}
	type poolView struct {
		includePro bool
	}
	assertView := func(view poolView, step string) {
		t.Helper()
		on := view.includePro
		wantPool := PoolScopeNormal
		if on {
			wantPool = PoolScopeUltra
		}
		if pool.UltraIncludePro() != on {
			t.Fatalf("%s：UltraIncludePro = %t", step, pool.UltraIncludePro())
		}
		if pool.PoolOf(pro) != wantPool {
			t.Fatalf("%s：Pro 的 PoolOf = %v，期望 %v", step, pool.PoolOf(pro), wantPool)
		}
		if _, member := pool.UltraAccountIDs()[pro]; member != on {
			t.Fatalf("%s：UltraAccountIDs 含 Pro = %t", step, member)
		}
		// /ultra 取租约：开启时能取到 Pro，关闭时取不到
		ultraSeen := acquiredAccounts(t, pool, ultraCtx, shared)
		if ultraSeen[pro] != on || !ultraSeen[scopeUltraA] {
			t.Fatalf("%s：/ultra 取到的账户 %v", step, ultraSeen)
		}
		// 独占的普通号池：开启时多次取租约都不是 Pro
		for range 6 {
			lease, err := pool.AcquireFor(normalCtx, AccountSelection{ModelID: includeProModel, Method: "generateContent"})
			if on {
				if !errors.Is(err, ErrModelNotFound) {
					t.Fatalf("%s：普通号池请求只有 Pro 有的模型：err = %v，期望模型不存在", step, err)
				}
				break
			}
			if err != nil || lease.Account().ID != pro {
				t.Fatalf("%s：关闭时普通号池请求 Pro 独有模型应取到 Pro: %v", step, err)
			}
			_ = lease.Release()
		}
		for range 6 {
			lease, err := pool.AcquireFor(normalCtx, shared)
			if err != nil {
				t.Fatal(err)
			}
			if on && lease.Account().ID == pro {
				t.Fatalf("%s：开启后普通号池仍取到了 Pro", step)
			}
			_ = lease.Release()
		}
		// 普通号池固定 Pro 账户：开启后不在号池内
		lease, err := pool.AcquireFor(normalCtx, AccountSelection{AccountID: pro})
		if on {
			if !errors.Is(err, ErrNoEligibleAccount) {
				t.Fatalf("%s：普通号池固定 Pro：err = %v，期望没有符合条件的账户", step, err)
			}
		} else if err != nil {
			t.Fatalf("%s：关闭时普通号池固定 Pro 应可用: %v", step, err)
		} else {
			_ = lease.Release()
		}
		// 绑定在 Pro 上的资源跟着账户换号池
		var mismatch *ResourcePoolMismatchError
		mismatchCtx := normalCtx
		if !on {
			mismatchCtx = ultraCtx
		}
		if _, err := pool.AcquireFor(mismatchCtx, AccountSelection{ResourceID: "files/pro"}); !errors.As(err, &mismatch) || mismatch.Owner != wantPool {
			t.Fatalf("%s：跨号池引用 Pro 的文件：err = %v，期望资源属于 %v", step, err, wantPool)
		}
		ownCtx := ultraCtx
		if !on {
			ownCtx = normalCtx
		}
		lease, err = pool.AcquireFor(ownCtx, AccountSelection{ResourceID: "files/pro"})
		if err != nil || lease.Account().ID != pro {
			t.Fatalf("%s：号池内引用 Pro 的文件失败: %v", step, err)
		}
		_ = lease.Release()
		// 候选与计数
		ultraIDs, _ := pool.EnabledAccountsIn(PoolScopeUltra)
		normalIDs, _ := pool.EnabledAccountsIn(PoolScopeNormal)
		if slices.Contains(ultraIDs, pro) != on || slices.Contains(normalIDs, pro) == on {
			t.Fatalf("%s：EnabledAccountsIn Ultra=%v 普通含 Pro=%t", step, ultraIDs, slices.Contains(normalIDs, pro))
		}
		wantUltra := 1
		if on {
			wantUltra = 2
		}
		if got := pool.attemptCandidatesIn(PoolScopeUltra); got != wantUltra {
			t.Fatalf("%s：Ultra 号池换号上限 = %d，期望 %d", step, got, wantUltra)
		}
		groups, err := pool.ClassifyCandidatesCached(AccountSelection{ModelID: scopeSharedModel, Method: "generateContent", Pool: PoolScopeUltra}, nil)
		if err != nil || slices.Contains(groups.StandbyReady, pro) != on {
			t.Fatalf("%s：Ultra 候选 %v（%v）", step, groups.StandbyReady, err)
		}
		if _, ultra := pool.PoolStateCounts(); ultra.Total != wantUltra {
			t.Fatalf("%s：Ultra 号池状态计数 = %+v，期望 %d 个", step, ultra, wantUltra)
		}
		if summary := pool.BootstrapSummary(); summary.UltraAvailable != wantUltra {
			t.Fatalf("%s：预热概况 UltraAvailable = %d，期望 %d", step, summary.UltraAvailable, wantUltra)
		}
		if got := pool.SpareAccounts(PoolScopeUltra, nil, []string{scopeSharedModel}, 0, 0); got != wantUltra {
			t.Fatalf("%s：Ultra 号池可接替账户 = %d，期望 %d", step, got, wantUltra)
		}
		// 并入的 Pro 按 Ultra 号池的候选集计分；按普通号池计分时独占模式下它不在候选里，会得 0 分
		if counts := pool.ServeableCounts([]string{pro}, []string{includeProModel}, 0); counts[pro] != 1 {
			t.Fatalf("%s：Pro 能服务的模型数 = %d，期望 1", step, counts[pro])
		}
		// 目录与别名
		if got := modelIDs(pool.EligibleModelsIn(PoolScopeUltra, catalog)); slices.Contains(got, includeProModel) != on {
			t.Fatalf("%s：Ultra 号池目录 %v", step, got)
		}
		if got := modelIDs(pool.EligibleModelsIn(PoolScopeNormal, catalog)); slices.Contains(got, includeProModel) == on {
			t.Fatalf("%s：普通号池目录 %v", step, got)
		}
		wantAlias, wantNormalAlias := includeProAlias, includeProModel
		if on {
			wantAlias, wantNormalAlias = includeProModel, includeProAlias
		}
		if got := pool.CanonicalModelIDIn(PoolScopeUltra, includeProAlias); got != wantAlias {
			t.Fatalf("%s：Ultra 号池别名解析为 %q，期望 %q", step, got, wantAlias)
		}
		if got := pool.CanonicalModelIDIn(PoolScopeNormal, includeProAlias); got != wantNormalAlias {
			t.Fatalf("%s：普通号池别名解析为 %q，期望 %q", step, got, wantNormalAlias)
		}
		// 状态列表：号池随开关，权益始终是 Pro
		for _, status := range pool.Status() {
			if status.ID == pro && (status.Pool != wantPool.String() || status.BenefitTier != "Pro") {
				t.Fatalf("%s：Pro 的状态 Pool=%q BenefitTier=%q", step, status.Pool, status.BenefitTier)
			}
		}
		if status, _ := pool.StatusOf(pro); status.Pool != wantPool.String() {
			t.Fatalf("%s：StatusOf 的号池 = %q", step, status.Pool)
		}
		verifyIndex(t, pool, step)
	}

	warmCaches()
	assertView(poolView{}, "切换前")
	// 持有 Pro 的租约时切换：租约不受影响，释放后索引仍一致
	held, err := pool.AcquireFor(normalCtx, AccountSelection{AccountID: pro})
	if err != nil {
		t.Fatal(err)
	}
	warmCaches()
	version := pool.PoolRuleVersion()
	if !pool.SetUltraIncludePro(true) {
		t.Fatal("开启开关应返回发生变化")
	}
	if pool.SetUltraIncludePro(true) {
		t.Fatal("重复开启不应返回发生变化")
	}
	if pool.PoolRuleVersion() != version+1 {
		t.Fatalf("开启后规则版本 = %d，期望 %d", pool.PoolRuleVersion(), version+1)
	}
	verifyIndex(t, pool, "持有租约时切换")
	if err := held.Release(); err != nil {
		t.Fatal(err)
	}
	verifyIndex(t, pool, "切换后释放租约")
	assertView(poolView{includePro: true}, "开启后")

	warmCaches()
	if !pool.SetUltraIncludePro(false) {
		t.Fatal("关闭开关应返回发生变化")
	}
	assertView(poolView{}, "再关闭后")
}

// TestUltraIncludeProResourceDirections 固定文档里写的资源引用方向：资源不迁移，按所属账户当前的号池判断。
// 开启后独占模式的普通路径（PoolScopeNormal）引用 Pro 账户的资源返回号池不符、/ultra 放行；
// ULTRA_EXCLUSIVE=false 时普通路径为 PoolScopeAll，不检查号池，开启不会产生 400；关闭后 /ultra 引用返回号池不符
func TestUltraIncludeProResourceDirections(t *testing.T) {
	const pro = scopeNormalB
	pool := scopeTestPool(t, map[string]BenefitTier{scopeNormalA: BenefitTierFree, pro: BenefitTierPro, scopeUltraA: BenefitTierUltra})
	bindTestFile(t, pool, pro, "files/pro")
	// expect 为各入口引用 Pro 文件的结果：PoolScopeAll 表示放行，其余为号池不符错误里的 Owner
	check := func(step string, expect map[PoolScope]PoolScope) {
		t.Helper()
		for _, scope := range []PoolScope{PoolScopeAll, PoolScopeNormal, PoolScopeUltra} {
			lease, err := pool.AcquireFor(scopeContext(scope), AccountSelection{ResourceID: "files/pro"})
			want := expect[scope]
			if want == PoolScopeAll {
				if err != nil || lease.Account().ID != pro {
					t.Fatalf("%s：%v 入口引用 Pro 的文件应放行: %v", step, scope, err)
				}
				_ = lease.Release()
				continue
			}
			var mismatch *ResourcePoolMismatchError
			if !errors.As(err, &mismatch) || mismatch.Owner != want {
				t.Fatalf("%s：%v 入口引用 Pro 的文件：err = %v，期望资源属于 %v", step, scope, err, want)
			}
		}
	}
	check("关闭时", map[PoolScope]PoolScope{PoolScopeAll: PoolScopeAll, PoolScopeNormal: PoolScopeAll, PoolScopeUltra: PoolScopeNormal})
	pool.SetUltraIncludePro(true)
	check("开启后", map[PoolScope]PoolScope{PoolScopeAll: PoolScopeAll, PoolScopeNormal: PoolScopeUltra, PoolScopeUltra: PoolScopeAll})
	pool.SetUltraIncludePro(false)
	check("再关闭后", map[PoolScope]PoolScope{PoolScopeAll: PoolScopeAll, PoolScopeNormal: PoolScopeAll, PoolScopeUltra: PoolScopeNormal})
}

// TestUltraIncludeProTierChangeWhileOn 开关开启时账户权益变化照常经增量路径换池：Pro 降为 Free 回到普通号池，升为 Ultra 留在 Ultra 号池
func TestUltraIncludeProTierChangeWhileOn(t *testing.T) {
	const account = scopeNormalB
	pool := scopeTestPool(t, map[string]BenefitTier{scopeNormalA: BenefitTierFree, account: BenefitTierPro})
	pool.SetUltraIncludePro(true)
	models := []Model{{ID: scopeSharedModel, Methods: []string{"generateContent"}, Capabilities: map[string]bool{"chat_model": true}}}
	for _, step := range []struct {
		tier BenefitTier
		want PoolScope
	}{
		{tier: BenefitTierFree, want: PoolScopeNormal},
		{tier: BenefitTierPro, want: PoolScopeUltra},
		{tier: BenefitTierUltra, want: PoolScopeUltra},
		{tier: BenefitTierPlus, want: PoolScopeNormal},
	} {
		before := pool.UltraAccountIDs()
		if err := pool.SetCatalog(account, step.tier, models); err != nil {
			t.Fatal(err)
		}
		if got := pool.PoolOf(account); got != step.want {
			t.Fatalf("权益改为 %s 后号池 = %v，期望 %v", step.tier, got, step.want)
		}
		after := pool.UltraAccountIDs()
		if _, member := after[account]; member != (step.want == PoolScopeUltra) {
			t.Fatalf("权益改为 %s 后 UltraAccountIDs = %v", step.tier, after)
		}
		_, wasMember := before[account]
		if changed := wasMember != (step.want == PoolScopeUltra); changed == sameMap(before, after) {
			t.Fatalf("权益改为 %s：成员变化=%t，快照是否重建与之不符", step.tier, changed)
		}
		ids, _ := pool.EnabledAccountsIn(step.want)
		if !slices.Contains(ids, account) {
			t.Fatalf("权益改为 %s 后 %v 启用账户 %v 不含该账户", step.tier, step.want, ids)
		}
		verifyIndex(t, pool, "权益改为 "+step.tier.String())
	}
}

// TestUltraIncludeProKeepsRealTier 开关只改号池成员：只有 Ultra 订阅可用的模型仍只由 Ultra 账户服务，
// 更高权益重试仍按真实权益比较，额度共享分组与状态里的权益不随开关变化
func TestUltraIncludeProKeepsRealTier(t *testing.T) {
	const ultraOnly = "ultra-subscription-model"
	pool := scopeTestPool(t, map[string]BenefitTier{scopeNormalA: BenefitTierFree, scopeNormalB: BenefitTierPro, scopeUltraA: BenefitTierUltra})
	models := []Model{
		{ID: scopeSharedModel, Methods: []string{"generateContent"}, Capabilities: map[string]bool{"chat_model": true}},
		{ID: ultraOnly, Methods: []string{"generateContent"}, Capabilities: map[string]bool{"chat_model": true}, AccessModes: []int64{4}},
	}
	for accountID, tier := range map[string]BenefitTier{scopeNormalA: BenefitTierFree, scopeNormalB: BenefitTierPro, scopeUltraA: BenefitTierUltra} {
		if err := pool.SetCatalog(accountID, tier, models); err != nil {
			t.Fatal(err)
		}
	}
	for _, on := range []bool{false, true} {
		pool.SetUltraIncludePro(on)
		step := fmt.Sprintf("并入=%t", on)
		for accountID := range acquiredAccounts(t, pool, scopeContext(PoolScopeUltra), AccountSelection{ModelID: ultraOnly, Method: "generateContent"}) {
			if accountID != scopeUltraA {
				t.Fatalf("%s：只有 Ultra 订阅可用的模型取到了 %s", step, accountID)
			}
		}
		if got := pool.EnabledAccountsAboveTierOf(PoolScopeUltra, scopeNormalB); !slices.Equal(got, []string{scopeUltraA}) {
			t.Fatalf("%s：Ultra 号池中权益高于 Pro 的账户 = %v，期望只有 Ultra", step, got)
		}
		above := pool.EnabledAccountsAboveTierOf(PoolScopeNormal, scopeNormalA)
		if slices.Contains(above, scopeNormalB) == on || slices.Contains(above, scopeUltraA) {
			t.Fatalf("%s：普通号池中权益高于 Free 的账户 = %v", step, above)
		}
		if pool.TierPoolOf(scopeNormalB) != PoolScopeNormal || pool.TierPoolOf(scopeUltraA) != PoolScopeUltra ||
			pool.TierPoolOf(scopeNormalA) != PoolScopeNormal || pool.TierPoolOf("missing@example.com") != PoolScopeNormal {
			t.Fatalf("%s：TierPoolOf 应按真实权益且不随开关变化", step)
		}
		status, _ := pool.StatusOf(scopeNormalB)
		wantPool := "normal"
		if on {
			wantPool = "ultra"
		}
		if status.BenefitTier != "Pro" || status.Pool != wantPool {
			t.Fatalf("%s：Pro 的状态 BenefitTier=%q Pool=%q", step, status.BenefitTier, status.Pool)
		}
		pool.mu.Lock()
		tier := pool.byID[scopeNormalB].BenefitTier
		pool.mu.Unlock()
		if tier != BenefitTierPro {
			t.Fatalf("%s：账户权益被改成了 %s", step, tier)
		}
	}
}

// TestUltraIncludeProBuildOnlyModel 只在 Build 提供的模型不检查权益：Pro 的 Build 目录有它时，开启后 /ultra 可以由 Pro 服务，
// 关闭后 /ultra 没有账户；任一账户的 Playground 目录把它标成只有 Ultra 订阅可用时，Pro 被排除
func TestUltraIncludeProBuildOnlyModel(t *testing.T) {
	const pro = scopeNormalB
	pool := scopeTestPool(t, map[string]BenefitTier{scopeNormalA: BenefitTierFree, pro: BenefitTierPro})
	pool.SetUpstreamChannels([]Channel{ChannelPlayground, ChannelBuild})
	buildModel := Model{ID: includeProBuild, Methods: []string{"generateContent"}, Capabilities: map[string]bool{"chat_model": true}}
	if err := pool.SetBuildCatalog(pro, []Model{buildModel}); err != nil {
		t.Fatal(err)
	}
	selection := AccountSelection{ModelID: includeProBuild, Method: "generateContent"}
	ultraCtx := scopeContext(PoolScopeUltra)

	if _, err := pool.AcquireFor(ultraCtx, selection); err == nil {
		t.Fatal("关闭时 /ultra 不应有账户服务只在 Build 提供的模型")
	}
	pool.SetUltraIncludePro(true)
	if got := modelIDs(pool.EligibleModelsIn(PoolScopeUltra, []Model{buildModel})); !slices.Equal(got, []string{includeProBuild}) {
		t.Fatalf("开启后 Ultra 号池目录 = %v，期望含只在 Build 提供的模型", got)
	}
	lease, err := pool.AcquireFor(ultraCtx, selection)
	if err != nil {
		t.Fatalf("开启后 /ultra 请求只在 Build 提供的模型失败: %v", err)
	}
	if lease.Account().ID != pro || lease.Channel() != ChannelBuild {
		t.Fatalf("开启后取到 %s 通道 %s，期望 Pro 经 Build", lease.Account().ID, lease.Channel())
	}
	_ = lease.Release()
	if _, err := pool.AcquireFor(scopeContext(PoolScopeNormal), selection); !errors.Is(err, ErrModelNotFound) {
		t.Fatalf("开启后独占的普通路径：err = %v，期望模型不存在", err)
	}
	pool.SetUltraIncludePro(false)
	if _, err := pool.AcquireFor(ultraCtx, selection); err == nil {
		t.Fatal("再关闭后 /ultra 不应有账户服务只在 Build 提供的模型")
	}

	// Ultra 账户的 Playground 目录把它标成只有 Ultra 订阅可用：Build 通道对 Pro 按权益排除，/ultra 只由 Ultra 服务
	pool.SetUltraIncludePro(true)
	if err := pool.Add(indexTestAccount(scopeUltraA, BenefitTierUltra, []Model{{
		ID: includeProBuild, Methods: []string{"generateContent"}, Capabilities: map[string]bool{"chat_model": true}, AccessModes: []int64{4},
	}})); err != nil {
		t.Fatal(err)
	}
	for accountID := range acquiredAccounts(t, pool, ultraCtx, selection) {
		if accountID != scopeUltraA {
			t.Fatalf("Playground 标为只有 Ultra 订阅可用后 /ultra 取到了 %s", accountID)
		}
	}
	verifyIndex(t, pool, "Ultra 账户加入后")
}

// TestUltraIncludeProEmptyPoolReasons 开关开启时号池为空的原因写明 Pro 也属于 Ultra 号池；关闭时与原文完全相同
func TestUltraIncludeProEmptyPoolReasons(t *testing.T) {
	onlyPro := scopeTestPool(t, map[string]BenefitTier{scopeNormalB: BenefitTierPro})
	if err := onlyPro.NoEligibleError(AccountSelection{Pool: PoolScopeUltra}); !strings.Contains(err.Error(), emptyUltraPoolReason) ||
		strings.Contains(err.Error(), emptyUltraPoolWithProReason) {
		t.Fatalf("关闭时只有 Pro 的 Ultra 号池原因 = %v，期望原文", err)
	}
	onlyPro.SetUltraIncludePro(true)
	err := onlyPro.NoEligibleError(AccountSelection{Pool: PoolScopeNormal})
	var notReady *AccountsNotReadyError
	if !errors.As(err, &notReady) || !slices.Equal(notReady.Reasons, []string{emptyNormalPoolWithProReason}) {
		t.Fatalf("开启时只有 Pro 的普通号池：err = %v，期望说明 Pro 属于 Ultra 号池", err)
	}
	onlyPro.SetUltraIncludePro(false)
	if err := onlyPro.NoEligibleError(AccountSelection{Pool: PoolScopeNormal}); errors.As(err, &notReady) {
		t.Fatalf("关闭时只有 Pro 的普通号池不应为空: %v", err)
	}

	onlyFree := scopeTestPool(t, map[string]BenefitTier{scopeNormalA: BenefitTierFree})
	onlyFree.SetUltraIncludePro(true)
	if err := onlyFree.NoEligibleError(AccountSelection{Pool: PoolScopeUltra}); !errors.As(err, &notReady) ||
		!slices.Equal(notReady.Reasons, []string{emptyUltraPoolWithProReason}) {
		t.Fatalf("开启时只有 Free 的 Ultra 号池：err = %v，期望说明没有 Ultra 或 Pro", err)
	}
	onlyFree.SetUltraIncludePro(false)
	if err := onlyFree.NoEligibleError(AccountSelection{Pool: PoolScopeUltra}); !errors.As(err, &notReady) ||
		!slices.Equal(notReady.Reasons, []string{emptyUltraPoolReason}) {
		t.Fatalf("关闭时只有 Free 的 Ultra 号池：err = %v，期望原文", err)
	}

	onlyUltra := scopeTestPool(t, map[string]BenefitTier{scopeUltraA: BenefitTierUltra})
	if err := onlyUltra.NoEligibleError(AccountSelection{Pool: PoolScopeNormal}); !errors.As(err, &notReady) ||
		!slices.Equal(notReady.Reasons, []string{emptyNormalPoolReason}) {
		t.Fatalf("关闭时只有 Ultra 的普通号池：err = %v，期望原文", err)
	}
}

// TestUltraAccountIDsSnapshot UltraAccountIDs 写时复制：成员不变时返回同一份快照，权益变化、增删账户、切换开关时换新；
// 冷却、状态与启用变化不作废快照
func TestUltraAccountIDsSnapshot(t *testing.T) {
	pool := scopeTestPool(t, map[string]BenefitTier{scopeNormalA: BenefitTierFree, scopeNormalB: BenefitTierPro, scopeUltraA: BenefitTierUltra})
	snapshot := pool.UltraAccountIDs()
	expectSame := func(step string) {
		t.Helper()
		if next := pool.UltraAccountIDs(); !sameMap(snapshot, next) {
			t.Fatalf("%s：成员没有变化，快照不应重建", step)
		}
	}
	expectNew := func(step string, want ...string) {
		t.Helper()
		next := pool.UltraAccountIDs()
		if sameMap(snapshot, next) {
			t.Fatalf("%s：成员已变化，快照应重建", step)
		}
		got := make([]string, 0, len(next))
		for accountID := range next {
			got = append(got, accountID)
		}
		slices.Sort(got)
		slices.Sort(want)
		if !slices.Equal(got, want) {
			t.Fatalf("%s：成员 = %v，期望 %v", step, got, want)
		}
		snapshot = next
	}
	expectSame("再次读取")
	if err := pool.MarkCooldownIfGeneration(scopeUltraA, "", pool.ModelAccessGeneration(scopeUltraA), time.Now(), time.Now().Add(time.Hour), "测试冷却"); err != nil {
		t.Fatal(err)
	}
	expectSame("冷却变化")
	if err := pool.MarkUnavailable(scopeUltraA, "测试不可用"); err != nil {
		t.Fatal(err)
	}
	expectSame("状态变化")
	pool.mu.Lock()
	account := pool.byID[scopeUltraA]
	account.Config.Enabled = false
	pool.eligibilityVersion++
	pool.accountChangedLocked(account)
	pool.mu.Unlock()
	expectSame("启用变化")
	if err := pool.Add(indexTestAccount("free-added@example.com", BenefitTierFree, nil)); err != nil {
		t.Fatal(err)
	}
	expectSame("加入 Free 账户")

	models := []Model{{ID: scopeSharedModel, Methods: []string{"generateContent"}}}
	if err := pool.SetCatalog(scopeNormalA, BenefitTierUltra, models); err != nil {
		t.Fatal(err)
	}
	expectNew("权益升为 Ultra", scopeUltraA, scopeNormalA)
	if err := pool.Add(indexTestAccount("ultra-added@example.com", BenefitTierUltra, nil)); err != nil {
		t.Fatal(err)
	}
	expectNew("加入 Ultra 账户", scopeUltraA, scopeNormalA, "ultra-added@example.com")
	if _, err := pool.Remove("ultra-added@example.com", func(*Account) error { return nil }); err != nil {
		t.Fatal(err)
	}
	expectNew("移除 Ultra 账户", scopeUltraA, scopeNormalA)
	pool.SetUltraIncludePro(true)
	expectNew("开启开关", scopeUltraA, scopeNormalA, scopeNormalB)
	pool.SetUltraIncludePro(true)
	expectSame("重复开启")
	pool.SetUltraIncludePro(false)
	expectNew("关闭开关", scopeUltraA, scopeNormalA)
}

// TestPoolRuleVersion 号池规则版本只在开关实际变化时加一
func TestPoolRuleVersion(t *testing.T) {
	pool := NewAccountPool([]*Account{indexTestAccount(scopeNormalB, BenefitTierPro, nil)}, 1)
	for _, step := range []struct {
		on      bool
		changed bool
		version uint64
	}{
		{on: false, changed: false, version: 0},
		{on: true, changed: true, version: 1},
		{on: true, changed: false, version: 1},
		{on: false, changed: true, version: 2},
		{on: false, changed: false, version: 2},
		{on: true, changed: true, version: 3},
	} {
		if changed := pool.SetUltraIncludePro(step.on); changed != step.changed || pool.PoolRuleVersion() != step.version {
			t.Fatalf("SetUltraIncludePro(%t) = %t，版本 %d；期望 %t，版本 %d", step.on, changed, pool.PoolRuleVersion(), step.changed, step.version)
		}
	}
}

// TestUltraIncludeProToggleRace 取租约、读取状态与派生缓存的同时反复切换开关，-race 下没有数据竞争；
// 停止后固定为开启，普通号池不再取到 Pro，候选索引与账户状态一致
func TestUltraIncludeProToggleRace(t *testing.T) {
	tiers := map[string]BenefitTier{}
	for index := range 2 {
		tiers[fmt.Sprintf("free-%d@example.com", index)] = BenefitTierFree
		tiers[fmt.Sprintf("pro-%d@example.com", index)] = BenefitTierPro
		tiers[fmt.Sprintf("ultra-%d@example.com", index)] = BenefitTierUltra
	}
	pool := scopeTestPool(t, tiers)
	shared := AccountSelection{ModelID: scopeSharedModel, Method: "generateContent"}
	catalog := []Model{{ID: scopeSharedModel, Methods: []string{"generateContent"}}}
	var stop atomic.Bool
	var group sync.WaitGroup
	for worker := range 8 {
		group.Add(1)
		go func() {
			defer group.Done()
			scope := []PoolScope{PoolScopeNormal, PoolScopeUltra}[worker%2]
			for !stop.Load() {
				ctx, cancel := context.WithTimeout(scopeContext(scope), 200*time.Millisecond)
				lease, err := pool.AcquireFor(ctx, shared)
				cancel()
				if err == nil {
					_ = lease.Release()
				}
			}
		}()
	}
	readers := []func(){
		func() { pool.Status() },
		func() { pool.UltraAccountIDs() },
		func() { pool.EligibleModelsIn(PoolScopeUltra, catalog) },
		func() { pool.SpareAccounts(PoolScopeUltra, nil, []string{scopeSharedModel}, 0, 0) },
		func() {
			_, _ = pool.ClassifyCandidatesCached(AccountSelection{ModelID: scopeSharedModel, Method: "generateContent", Pool: PoolScopeNormal}, nil)
		},
	}
	for _, read := range readers {
		group.Add(1)
		go func() {
			defer group.Done()
			for !stop.Load() {
				read()
			}
		}()
	}
	for index := range 200 {
		pool.SetUltraIncludePro(index%2 == 0)
		if index%20 == 0 {
			time.Sleep(time.Millisecond)
		}
	}
	stop.Store(true)
	group.Wait()

	pool.SetUltraIncludePro(true)
	for range 20 {
		lease, err := pool.AcquireFor(scopeContext(PoolScopeNormal), shared)
		if err != nil {
			t.Fatal(err)
		}
		if tiers[lease.Account().ID] == BenefitTierPro {
			t.Fatalf("开启后普通号池取到了 Pro 账户 %s", lease.Account().ID)
		}
		_ = lease.Release()
	}
	verifyIndex(t, pool, "并发切换之后")
}
