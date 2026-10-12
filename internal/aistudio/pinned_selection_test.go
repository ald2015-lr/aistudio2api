package aistudio

import (
	"context"
	"errors"
	"testing"
)

// TestPinnedSelectionUsesIndexPosition 指定账户的选号按账户表与候选索引的下标定位，不扫描全部账户：
// 删除账户让后续账户的下标变化后仍选中正确的账户；账户不在号池、不在限定集合或不存在时按没有候选返回
func TestPinnedSelectionUsesIndexPosition(t *testing.T) {
	catalog := indexTestCatalog()
	ids := []string{"pin-a@example.com", "pin-b@example.com", "pin-c@example.com", "pin-d@example.com", "pin-e@example.com"}
	initial := make([]*Account, 0, len(ids))
	for index, id := range ids {
		tier := BenefitTierFree
		if index == len(ids)-1 {
			tier = BenefitTierUltra
		}
		initial = append(initial, indexTestAccount(id, tier, catalog))
	}
	pool := NewAccountPool(initial, 2)
	ctx := context.Background()
	selection := AccountSelection{ModelID: indexTestModel, Method: "generateContent"}
	if _, err := pool.ClassifyCandidates(ctx, selection, nil); err != nil {
		t.Fatal(err)
	}
	// 删除中间的账户：之后账户的下标都向前移动，候选索引整体重建
	if _, err := pool.Remove(ids[1], func(*Account) error { return nil }); err != nil {
		t.Fatal(err)
	}
	pool.mu.Lock()
	for _, id := range []string{ids[0], ids[2], ids[3], ids[4]} {
		indices, err := pool.selectionIndicesLocked(AccountSelection{AccountID: id})
		if err != nil || len(indices) != 1 || pool.accounts[indices[0]].ID != id {
			pool.mu.Unlock()
			t.Fatalf("指定账户 %s 的下标 = %v（err=%v），期望正好定位到该账户", id, indices, err)
		}
	}
	pool.mu.Unlock()
	pinned := selection
	pinned.AccountID = ids[3]
	lease, _, err := pool.TryAcquireFor(ctx, pinned)
	if err != nil || lease == nil || lease.Account().ID != ids[3] {
		t.Fatalf("删除账户后指定 %s 的租约 = %v（err=%v），期望选中该账户", ids[3], lease, err)
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
	for name, selection := range map[string]AccountSelection{
		"不存在的账户":   {AccountID: "missing@example.com", ModelID: indexTestModel},
		"不在号池":     {AccountID: ids[2], ModelID: indexTestModel, Pool: PoolScopeUltra},
		"不在限定集合":   {AccountID: ids[2], ModelID: indexTestModel, AllowedAccountIDs: []string{ids[0]}},
		"已删除的账户":   {AccountID: ids[1], ModelID: indexTestModel},
		"Ultra 号池": {AccountID: ids[4], ModelID: indexTestModel, Pool: PoolScopeNormal},
	} {
		if _, _, err := pool.TryAcquireFor(ctx, selection); !errors.Is(err, ErrNoEligibleAccount) {
			t.Fatalf("%s：err = %v，期望 %v", name, err, ErrNoEligibleAccount)
		}
	}
	// 新增账户后立即可以按指定账户选中
	added := "pin-added@example.com"
	if err := pool.Add(indexTestAccount(added, BenefitTierFree, catalog)); err != nil {
		t.Fatal(err)
	}
	pinned.AccountID = added
	if lease, _, err = pool.TryAcquireFor(ctx, pinned); err != nil || lease == nil || lease.Account().ID != added {
		t.Fatalf("新增账户后指定 %s 的租约 = %v（err=%v），期望选中该账户", added, lease, err)
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
}
