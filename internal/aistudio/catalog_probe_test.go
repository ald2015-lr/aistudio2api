package aistudio

import (
	"context"
	"slices"
	"testing"
)

// TestCatalogProbeCandidates 缺少模型时读目录的候选：只取号池内就绪、空闲、已有目录的账户，权益从高到低（Ultra、Pro、Plus、Free）
func TestCatalogProbeCandidates(t *testing.T) {
	tiers := map[string]BenefitTier{
		"free@example.com": BenefitTierFree, "plus@example.com": BenefitTierPlus, "pro@example.com": BenefitTierPro,
		"ultra@example.com": BenefitTierUltra, "busy-ultra@example.com": BenefitTierUltra, "empty@example.com": BenefitTierUltra,
	}
	ids := make([]string, 0, len(tiers))
	for id := range tiers {
		ids = append(ids, id)
	}
	pool := retentionTestPool(t, ids...)
	models := []Model{{ID: "alpha", Methods: []string{"generateContent"}, Capabilities: map[string]bool{"chat_model": true}}}
	for id, tier := range tiers {
		catalog := models
		if id == "empty@example.com" {
			catalog = nil
		}
		if err := pool.SetCatalog(id, tier, catalog); err != nil {
			t.Fatal(err)
		}
	}
	lease, err := pool.AcquireFor(context.Background(), AccountSelection{AccountID: "busy-ultra@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()

	got := pool.CatalogProbeCandidates(PoolScopeAll, 10)
	want := []string{"ultra@example.com", "pro@example.com", "plus@example.com", "free@example.com"}
	if !slices.Equal(got, want) {
		t.Fatalf("候选 = %v，期望 %v（有租约、没有目录的不选）", got, want)
	}
	if got := pool.CatalogProbeCandidates(PoolScopeAll, 2); !slices.Equal(got, want[:2]) {
		t.Fatalf("上限 2 时候选 = %v", got)
	}
	if got := pool.CatalogProbeCandidates(PoolScopeNormal, 10); !slices.Equal(got, want[1:]) {
		t.Fatalf("普通号池的候选 = %v，期望 %v（Ultra 账户不在普通号池）", got, want[1:])
	}
	if !pool.ModelListedIn(PoolScopeAll, "models/alpha") || pool.ModelListedIn(PoolScopeAll, "beta") {
		t.Fatal("ModelListedIn 应按目录判断（去掉 models/ 前缀）")
	}
}
