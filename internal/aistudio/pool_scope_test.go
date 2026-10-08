package aistudio

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

const (
	scopeSharedModel = "shared-model"
	scopeUltraModel  = "ultra-model"
	scopeUltraAlias  = "ultra-latest"
	scopeNormalA     = "normal-a@example.com"
	scopeNormalB     = "normal-b@example.com"
	scopeUltraA      = "ultra-a@example.com"
	scopeUltraB      = "ultra-b@example.com"
)

// scopeTestPool 按权益创建账户：所有账户都有共享模型，Ultra 账户另有只在 Ultra 号池出现的模型（带目录别名）
func scopeTestPool(t *testing.T, tiers map[string]BenefitTier) *AccountPool {
	t.Helper()
	store := NewAccountStore(t.TempDir())
	pool := NewAccountPool(nil, 2)
	for accountID, tier := range tiers {
		account, publish, err := store.Create(DefaultAccountConfig(accountID), testStorageState("OLD"))
		if err != nil {
			t.Fatal(err)
		}
		if err := pool.Add(account); err != nil {
			t.Fatal(err)
		}
		if err := publish.Release(); err != nil {
			t.Fatal(err)
		}
		models := []Model{{
			ID: scopeSharedModel, Methods: []string{"generateContent", "countTokens", "bidiGenerateContent"},
			Capabilities: map[string]bool{"chat_model": true},
		}}
		if tier == BenefitTierUltra {
			models = append(models, Model{
				ID: scopeUltraModel, Methods: []string{"generateContent", "countTokens"},
				Capabilities:      map[string]bool{"chat_model": true},
				CapabilityOptions: map[string][]string{"aliases": {scopeUltraAlias}},
			})
		}
		if err := pool.SetCatalog(account.ID, tier, models); err != nil {
			t.Fatal(err)
		}
	}
	return pool
}

func scopeContext(scope PoolScope) context.Context {
	return ContextWithPoolScope(context.Background(), scope)
}

// acquiredAccounts 连续取两次租约（不释放），返回取到的账户
func acquiredAccounts(t *testing.T, pool *AccountPool, ctx context.Context, selection AccountSelection) map[string]bool {
	t.Helper()
	seen := make(map[string]bool)
	for range 4 {
		lease, err := pool.AcquireFor(ctx, selection)
		if err != nil {
			t.Fatalf("取租约失败: %v", err)
		}
		seen[lease.Account().ID] = true
		if err := lease.Release(); err != nil {
			t.Fatal(err)
		}
	}
	return seen
}

// TestPoolScopeIsolation /ultra 请求只用 Ultra 号池，独占模式的普通请求只用普通号池，不限号池时两边都用
func TestPoolScopeIsolation(t *testing.T) {
	pool := scopeTestPool(t, map[string]BenefitTier{
		scopeNormalA: BenefitTierFree, scopeNormalB: BenefitTierPro, scopeUltraA: BenefitTierUltra, scopeUltraB: BenefitTierUltra,
	})
	selection := AccountSelection{ModelID: scopeSharedModel, Method: "generateContent"}
	for accountID := range acquiredAccounts(t, pool, scopeContext(PoolScopeUltra), selection) {
		if pool.PoolOf(accountID) != PoolScopeUltra {
			t.Fatalf("Ultra 请求用到了普通号池账户 %s", accountID)
		}
	}
	for accountID := range acquiredAccounts(t, pool, scopeContext(PoolScopeNormal), selection) {
		if pool.PoolOf(accountID) != PoolScopeNormal {
			t.Fatalf("独占模式的普通请求用到了 Ultra 账户 %s", accountID)
		}
	}
	all := acquiredAccounts(t, pool, context.Background(), selection)
	if len(all) != 4 {
		t.Fatalf("不限号池时应轮询全部账户，实际 %v", all)
	}

	// 分类候选与立即取租约同样只看号池内的账户
	groups, err := pool.ClassifyCandidates(scopeContext(PoolScopeUltra), selection, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, accountID := range append(groups.StandbyReady, groups.StandbyBusy...) {
		if pool.PoolOf(accountID) != PoolScopeUltra {
			t.Fatalf("Ultra 候选里出现普通号池账户 %s", accountID)
		}
	}
	if len(groups.StandbyReady) != 2 {
		t.Fatalf("Ultra 候选数 = %d，期望 2", len(groups.StandbyReady))
	}
	lease, _, err := pool.TryAcquireFor(scopeContext(PoolScopeNormal), selection)
	if err != nil || lease == nil {
		t.Fatalf("TryAcquireFor 失败: %v", err)
	}
	if pool.PoolOf(lease.Account().ID) != PoolScopeNormal {
		t.Fatalf("TryAcquireFor 用到了号池之外的账户 %s", lease.Account().ID)
	}
	_ = lease.Release()

	// 选择条件里显式写明的号池优先于 context
	explicit := selection
	explicit.Pool = PoolScopeUltra
	lease, err = pool.AcquireFor(scopeContext(PoolScopeNormal), explicit)
	if err != nil {
		t.Fatal(err)
	}
	if pool.PoolOf(lease.Account().ID) != PoolScopeUltra {
		t.Fatalf("显式 Ultra 选择用到了 %s", lease.Account().ID)
	}
	_ = lease.Release()

	// 指定账户（换号恢复、上传）不在号池内时不可用
	pinned := AccountSelection{AccountID: scopeNormalA}
	if _, err := pool.AcquireFor(scopeContext(PoolScopeUltra), pinned); !errors.Is(err, ErrNoEligibleAccount) {
		t.Fatalf("Ultra 请求指定普通账户：err = %v，期望没有符合条件的账户", err)
	}
}

// TestPoolScopeWithoutUltraAccounts 没有 Ultra 账户或 Ultra 账户都不可用时按号池暂时不可调度（503），错误写明 Ultra 号池；
// 全部冷却按 429
func TestPoolScopeWithoutUltraAccounts(t *testing.T) {
	pool := scopeTestPool(t, map[string]BenefitTier{scopeNormalA: BenefitTierFree})
	ctx := scopeContext(PoolScopeUltra)
	selection := AccountSelection{ModelID: scopeSharedModel, Method: "generateContent"}
	assertUltraNotReady := func(err error, what string) {
		t.Helper()
		var notReady *AccountsNotReadyError
		if !errors.As(err, &notReady) || notReady.HTTPStatus() != http.StatusServiceUnavailable {
			t.Fatalf("%s：err = %v，期望号池暂时不可调度（503）", what, err)
		}
		if notReady.Pool != PoolScopeUltra || !strings.Contains(err.Error(), "Ultra 号池") {
			t.Fatalf("%s：错误没有写明 Ultra 号池: %v", what, err)
		}
	}
	_, err := pool.AcquireFor(ctx, selection)
	assertUltraNotReady(err, "没有 Ultra 账户时取租约")
	_, err = pool.ClassifyCandidates(ctx, selection, nil)
	assertUltraNotReady(err, "没有 Ultra 账户时分类候选")
	assertUltraNotReady(pool.NoEligibleError(AccountSelection{Pool: PoolScopeUltra}), "没有 Ultra 账户时的无账户错误")
	if !strings.Contains(pool.NoEligibleError(AccountSelection{Pool: PoolScopeUltra}).Error(), emptyUltraPoolReason) {
		t.Fatal("Ultra 号池为空时应说明没有权益为 Ultra 的账户")
	}
	// 普通请求不受影响
	lease, err := pool.AcquireFor(scopeContext(PoolScopeNormal), selection)
	if err != nil {
		t.Fatalf("普通请求取租约失败: %v", err)
	}
	_ = lease.Release()

	// Ultra 账户都需要重新登录：同样按 503，并列出账户
	pool = scopeTestPool(t, map[string]BenefitTier{scopeNormalA: BenefitTierFree, scopeUltraA: BenefitTierUltra})
	if err := pool.MarkAuthRequired(scopeUltraA, "Cookie 失效"); err != nil {
		t.Fatal(err)
	}
	_, err = pool.AcquireFor(ctx, selection)
	assertUltraNotReady(err, "Ultra 账户需要重新登录")
	if !strings.Contains(err.Error(), scopeUltraA) || strings.Contains(err.Error(), scopeNormalA) {
		t.Fatalf("错误应只列出 Ultra 号池的账户: %v", err)
	}

	// Ultra 账户全部冷却：候选为空时按全部冷却（429）
	pool = scopeTestPool(t, map[string]BenefitTier{scopeNormalA: BenefitTierFree, scopeUltraA: BenefitTierUltra})
	until := time.Now().Add(2 * time.Hour)
	if err := pool.MarkCooldownIfGeneration(scopeUltraA, "", pool.ModelAccessGeneration(scopeUltraA), time.Now(), until, "每日限额"); err != nil {
		t.Fatal(err)
	}
	groups, err := pool.ClassifyCandidates(ctx, selection, nil)
	if err != nil {
		t.Fatal(err)
	}
	var cooling *AllCoolingError
	if err := pool.NoCandidateError(AccountSelection{ModelID: scopeSharedModel, Pool: PoolScopeUltra}, groups); !errors.As(err, &cooling) ||
		cooling.HTTPStatus() != http.StatusTooManyRequests {
		t.Fatalf("Ultra 账户全部冷却：err = %v，期望全部冷却（429）", err)
	}
}

// TestPoolScopeModels 模型目录与别名按号池：只在 Ultra 账户出现的模型只出现在 Ultra 号池的目录里，
// 普通号池请求按模型不存在处理
func TestPoolScopeModels(t *testing.T) {
	pool := scopeTestPool(t, map[string]BenefitTier{scopeNormalA: BenefitTierFree, scopeUltraA: BenefitTierUltra})
	catalog := []Model{{ID: scopeSharedModel, Methods: []string{"generateContent"}}, {ID: scopeUltraModel, Methods: []string{"generateContent"}}}
	ids := func(models []Model) []string {
		result := make([]string, 0, len(models))
		for _, model := range models {
			result = append(result, model.ID)
		}
		return result
	}
	if got := ids(pool.EligibleModelsIn(PoolScopeUltra, catalog)); len(got) != 2 {
		t.Fatalf("Ultra 号池目录 = %v，期望两个模型", got)
	}
	if got := ids(pool.EligibleModelsIn(PoolScopeNormal, catalog)); len(got) != 1 || got[0] != scopeSharedModel {
		t.Fatalf("普通号池目录 = %v，期望只有共享模型", got)
	}
	if got := ids(pool.EligibleModels(catalog)); len(got) != 2 {
		t.Fatalf("不限号池目录 = %v，期望两个模型", got)
	}
	if got := pool.CanonicalModelIDIn(PoolScopeUltra, scopeUltraAlias); got != scopeUltraModel {
		t.Fatalf("Ultra 号池别名解析为 %q，期望 %q", got, scopeUltraModel)
	}
	if got := pool.CanonicalModelIDIn(PoolScopeNormal, scopeUltraAlias); got != scopeUltraAlias {
		t.Fatalf("普通号池不应解析 Ultra 账户目录里的别名，实际 %q", got)
	}
	selection := AccountSelection{ModelID: scopeUltraModel, Method: "generateContent"}
	if _, err := pool.AcquireFor(scopeContext(PoolScopeNormal), selection); !errors.Is(err, ErrModelNotFound) {
		t.Fatalf("普通号池请求 Ultra 独有模型：err = %v，期望模型不存在", err)
	}
	lease, err := pool.AcquireFor(scopeContext(PoolScopeUltra), selection)
	if err != nil {
		t.Fatalf("Ultra 号池请求 Ultra 独有模型失败: %v", err)
	}
	_ = lease.Release()
}

// bindTestFile 在指定账户上绑定一个已上传文件
func bindTestFile(t *testing.T, pool *AccountPool, accountID string, fileID string) {
	t.Helper()
	lease, err := pool.AcquireFor(context.Background(), AccountSelection{AccountID: accountID})
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	if err := lease.BindFileResource(context.Background(), FileRef{ID: fileID, Name: "a.txt", MIME: "text/plain"}, 3, "user_data"); err != nil {
		t.Fatal(err)
	}
}

// TestResourcePoolMismatch 文件绑定在号池之外的账户上时按 400 拒绝并说明属于哪个号池，不会改用池外账户
func TestResourcePoolMismatch(t *testing.T) {
	pool := scopeTestPool(t, map[string]BenefitTier{scopeNormalA: BenefitTierFree, scopeUltraA: BenefitTierUltra})
	bindTestFile(t, pool, scopeNormalA, "files/normal")
	bindTestFile(t, pool, scopeUltraA, "files/ultra")
	assertMismatch := func(err error, owner PoolScope, what string) {
		t.Helper()
		var mismatch *ResourcePoolMismatchError
		if !errors.As(err, &mismatch) || mismatch.Owner != owner {
			t.Fatalf("%s：err = %v，期望资源属于%s的错误", what, err, owner.Label())
		}
		if mismatch.HTTPStatus() != http.StatusBadRequest || !errors.Is(err, ErrInvalidArgument) {
			t.Fatalf("%s：资源号池不符应按请求参数错误（400）返回", what)
		}
		if errors.Is(err, ErrResourceNotFound) || errors.Is(err, ErrNoEligibleAccount) {
			t.Fatalf("%s：资源号池不符不能按资源不存在或没有账户处理", what)
		}
	}
	ultra, normal := scopeContext(PoolScopeUltra), scopeContext(PoolScopeNormal)
	_, err := pool.AcquireFor(ultra, AccountSelection{ResourceID: "files/normal"})
	assertMismatch(err, PoolScopeNormal, "Ultra 请求取普通账户的文件")
	if !strings.Contains(err.Error(), "normal pool") {
		t.Fatalf("错误消息应说明文件属于普通号池: %v", err)
	}
	_, err = pool.AcquireFor(normal, AccountSelection{ResourceID: "files/ultra"})
	assertMismatch(err, PoolScopeUltra, "普通请求取 Ultra 账户的文件")
	if !strings.Contains(err.Error(), "Ultra pool") {
		t.Fatalf("错误消息应说明文件属于 Ultra 号池: %v", err)
	}
	_, err = pool.ClassifyCandidates(ultra, AccountSelection{ResourceID: "files/normal"}, nil)
	assertMismatch(err, PoolScopeNormal, "Ultra 请求分类普通账户文件的候选")
	_, err = pool.FileMetadata(ultra, "files/normal")
	assertMismatch(err, PoolScopeNormal, "Ultra 请求读取普通账户文件的元数据")
	contents := []Content{{Role: RoleUser, Parts: []Part{{File: &FileRef{ID: "files/ultra"}}, {File: &FileRef{ID: "files/normal"}}}}}
	_, err = pool.ResourceIDForContents(ultra, contents)
	assertMismatch(err, PoolScopeNormal, "Ultra 生成请求引用普通账户的文件")

	// 号池内的文件照常可用；不限号池时两边都可用
	if _, err := pool.FileMetadata(ultra, "files/ultra"); err != nil {
		t.Fatalf("Ultra 请求读取 Ultra 文件失败: %v", err)
	}
	lease, err := pool.AcquireFor(ultra, AccountSelection{ResourceID: "files/ultra"})
	if err != nil || lease.Account().ID != scopeUltraA {
		t.Fatalf("Ultra 请求取 Ultra 文件的账户失败: %v", err)
	}
	_ = lease.Release()
	if resourceID, err := pool.ResourceIDForContents(context.Background(), contents); err != nil || resourceID != "files/ultra" {
		t.Fatalf("不限号池时文件引用应可用: %q %v", resourceID, err)
	}
}

// TestPoolScopeCountsAndUploads 上传候选、换号上限与状态计数按号池统计
func TestPoolScopeCountsAndUploads(t *testing.T) {
	pool := scopeTestPool(t, map[string]BenefitTier{
		scopeNormalA: BenefitTierFree, scopeNormalB: BenefitTierPro, scopeUltraA: BenefitTierUltra,
	})
	if got := pool.fileUploadAccountIDs(PoolScopeUltra); len(got) != 1 || got[0] != scopeUltraA {
		t.Fatalf("Ultra 上传候选 = %v", got)
	}
	if got := pool.fileUploadAccountIDs(PoolScopeNormal); len(got) != 2 {
		t.Fatalf("普通上传候选 = %v", got)
	}
	if got := accountAttemptLimit(scopeContext(PoolScopeUltra), pool, false); got != 1 {
		t.Fatalf("Ultra 换号上限 = %d，期望 1", got)
	}
	if got := accountAttemptLimit(context.Background(), pool, false); got != 3 {
		t.Fatalf("不限号池换号上限 = %d，期望 3", got)
	}
	if _, eligible := pool.EnabledAccountsIn(PoolScopeNormal); eligible != 2 {
		t.Fatalf("普通号池可调度账户 = %d，期望 2", eligible)
	}
	if err := pool.MarkAuthRequired(scopeUltraA, "Cookie 失效"); err != nil {
		t.Fatal(err)
	}
	all, ultra := pool.PoolStateCounts()
	if all.Total != 3 || all.Ready != 2 || all.AuthRequired != 1 {
		t.Fatalf("全部账户计数 = %+v", all)
	}
	if ultra.Total != 1 || ultra.Ready != 0 || ultra.AuthRequired != 1 {
		t.Fatalf("Ultra 账户计数 = %+v", ultra)
	}
	summary := pool.BootstrapSummary()
	if summary.Available != 2 || summary.UltraAvailable != 0 {
		t.Fatalf("预热概况 = %+v，期望可预热 2 个、其中 Ultra 0 个", summary)
	}
	statuses := pool.Status()
	for _, status := range statuses {
		if !status.BenefitTierKnown {
			t.Fatalf("账户 %s 已同步目录，权益应为已读取", status.ID)
		}
		want := "normal"
		if status.ID == scopeUltraA {
			want = "ultra"
		}
		if status.Pool != want {
			t.Fatalf("账户 %s 的号池 = %q，期望 %q", status.ID, status.Pool, want)
		}
	}
}

// TestPoolScopeTierChangeMovesAccount 账户权益变化后立即按新权益归入号池
func TestPoolScopeTierChangeMovesAccount(t *testing.T) {
	pool := scopeTestPool(t, map[string]BenefitTier{scopeNormalA: BenefitTierPro})
	if _, err := pool.AcquireFor(scopeContext(PoolScopeUltra), AccountSelection{ModelID: scopeSharedModel, Method: "generateContent"}); err == nil {
		t.Fatal("Pro 账户不应进入 Ultra 号池")
	}
	if err := pool.SetCatalog(scopeNormalA, BenefitTierUltra, []Model{{ID: scopeSharedModel, Methods: []string{"generateContent"}}}); err != nil {
		t.Fatal(err)
	}
	lease, err := pool.AcquireFor(scopeContext(PoolScopeUltra), AccountSelection{ModelID: scopeSharedModel, Method: "generateContent"})
	if err != nil {
		t.Fatalf("升级为 Ultra 后应进入 Ultra 号池: %v", err)
	}
	_ = lease.Release()
	if pool.PoolOf(scopeNormalA) != PoolScopeUltra {
		t.Fatal("PoolOf 应按新权益返回 Ultra")
	}
	if _, ok := pool.UltraAccountIDs()[scopeNormalA]; !ok {
		t.Fatal("UltraAccountIDs 应包含升级后的账户")
	}
}

// TestNormalPoolErrors 没有 Ultra 账户时普通号池的错误与不分号池时完全相同；账户都是 Ultra 账户时说明独占模式不能使用
func TestNormalPoolErrors(t *testing.T) {
	selection := AccountSelection{ModelID: scopeSharedModel, Method: "generateContent"}
	pool := scopeTestPool(t, map[string]BenefitTier{scopeNormalA: BenefitTierFree})
	if err := pool.MarkAuthRequired(scopeNormalA, "Cookie 失效"); err != nil {
		t.Fatal(err)
	}
	_, scoped := pool.AcquireFor(scopeContext(PoolScopeNormal), selection)
	_, unscoped := pool.AcquireFor(context.Background(), selection)
	if scoped == nil || unscoped == nil || scoped.Error() != unscoped.Error() {
		t.Fatalf("普通号池的错误 %v 应与不分号池时 %v 相同", scoped, unscoped)
	}
	empty := NewAccountPool(nil, 1)
	if got, want := empty.NoEligibleError(AccountSelection{Pool: PoolScopeNormal}).Error(), empty.NoEligibleError(AccountSelection{}).Error(); got != want {
		t.Fatalf("空号池的错误 %q 应与不分号池时 %q 相同", got, want)
	}

	onlyUltra := scopeTestPool(t, map[string]BenefitTier{scopeUltraA: BenefitTierUltra})
	_, err := onlyUltra.AcquireFor(scopeContext(PoolScopeNormal), selection)
	var notReady *AccountsNotReadyError
	if !errors.As(err, &notReady) || !strings.Contains(err.Error(), emptyNormalPoolReason) || strings.Contains(err.Error(), "Ultra 号池：") {
		t.Fatalf("账户都是 Ultra 账户时普通路径：err = %v，期望说明独占模式的 503", err)
	}
}

// TestTranscribeExhaustedCandidatesNamesPool 转录候选在尝试前就已耗尽时的 503 写明请求的号池：/ultra 请求写明 Ultra 号池，
// 普通路径与不分号池时相同
func TestTranscribeExhaustedCandidatesNamesPool(t *testing.T) {
	pool := scopeTestPool(t, map[string]BenefitTier{scopeUltraA: BenefitTierUltra, scopeNormalA: BenefitTierFree})
	service := &PooledService{pool: pool}
	request := TranscriptionRequest{Name: "a.wav", MIME: "audio/wav", Size: 1, Reader: strings.NewReader("x"), CandidateAccountIDs: []string{}}
	for _, test := range []struct {
		scope PoolScope
		ultra bool
	}{
		{scope: PoolScopeUltra, ultra: true},
		{scope: PoolScopeNormal},
		{scope: PoolScopeAll},
	} {
		_, err := service.Transcribe(scopeContext(test.scope), request)
		var notReady *AccountsNotReadyError
		if !errors.As(err, &notReady) || notReady.Pool != test.scope || strings.Contains(err.Error(), "Ultra 号池") != test.ultra {
			t.Fatalf("号池 %q 转录候选耗尽：err = %v，期望号池为该号池的 503", test.scope.String(), err)
		}
	}
}
