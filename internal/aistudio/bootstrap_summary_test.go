package aistudio

import (
	"slices"
	"testing"
)

// TestBootstrapSummaryIgnoresDisabledModels 预热概况的模型只来自启用账户（上游 c77dd89）：停用账户独有的模型不进 ModelIDs，
// 预热按模型分类候选时不再为它多跑一轮；停用账户同样不计入可预热数。重新启用后它的模型回到概况里
func TestBootstrapSummaryIgnoresDisabledModels(t *testing.T) {
	store := NewAccountStore(t.TempDir())
	pool := NewAccountPool(nil, 2)
	t.Cleanup(pool.WaitBackground)
	catalogs := map[string][]string{
		"alice@example.com": {"shared-model", "alice-model"},
		"bob@example.com":   {"shared-model", "bob-model"},
	}
	for _, accountID := range []string{"alice@example.com", "bob@example.com"} {
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
		models := make([]Model, 0, len(catalogs[accountID]))
		for _, modelID := range catalogs[accountID] {
			models = append(models, Model{ID: modelID, Methods: []string{"generateContent"}, Capabilities: map[string]bool{"chat_model": true}})
		}
		if err := pool.SetCatalog(accountID, BenefitTierFree, models); err != nil {
			t.Fatal(err)
		}
	}
	if summary := pool.BootstrapSummary(); !slices.Equal(summary.ModelIDs, []string{"shared-model", "alice-model", "bob-model"}) || summary.Available != 2 {
		t.Fatalf("两个账户都启用时概况 = %+v", summary)
	}
	if _, _, err := pool.SetEnabled("bob@example.com", false); err != nil {
		t.Fatal(err)
	}
	if summary := pool.BootstrapSummary(); !slices.Equal(summary.ModelIDs, []string{"shared-model", "alice-model"}) || summary.Available != 1 {
		t.Fatalf("停用 bob 后概况 = %+v，期望不含 bob-model、可预热 1 个", summary)
	}
	if _, _, err := pool.SetEnabled("bob@example.com", true); err != nil {
		t.Fatal(err)
	}
	if summary := pool.BootstrapSummary(); !slices.Contains(summary.ModelIDs, "bob-model") {
		t.Fatalf("重新启用 bob 后概况 = %+v，期望含 bob-model", summary)
	}
}
