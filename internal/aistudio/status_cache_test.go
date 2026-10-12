package aistudio

import (
	"fmt"
	"testing"
	"time"
)

// statusCacheTestPool 构造账户数超过 statusCacheMinAccounts 的号池（Status 按缓存返回），返回号池与一个中间位置的账户 ID
func statusCacheTestPool(t *testing.T) (*AccountPool, string) {
	t.Helper()
	catalog := indexTestCatalog()
	accounts := make([]*Account, 0, statusCacheMinAccounts+1)
	for index := 0; index <= statusCacheMinAccounts; index++ {
		accounts = append(accounts, indexTestAccount(fmt.Sprintf("status-%03d@example.com", index), BenefitTierFree, catalog))
	}
	return NewAccountPool(accounts, 2), accounts[statusCacheMinAccounts/2].ID
}

// listedStatus 在 Status() 列表中找到账户
func listedStatus(t *testing.T, pool *AccountPool, accountID string) AccountStatus {
	t.Helper()
	for _, status := range pool.Status() {
		if status.ID == accountID {
			return status
		}
	}
	t.Fatalf("Status() 列表中没有账户 %s", accountID)
	return AccountStatus{}
}

// TestStatusCacheReflectsAccountChanges 账户多时 Status() 按缓存返回，但账户的状态、冷却、目录与权益变化后缓存期内的列表
// 也要立刻反映：管理操作（启停、重新登录、标记不可用）的返回值与账户列表都来自它，不能等缓存过期才与 StatusOf 一致
func TestStatusCacheReflectsAccountChanges(t *testing.T) {
	pool, victim := statusCacheTestPool(t)
	if state := listedStatus(t, pool, victim).State; state != AccountReady {
		t.Fatalf("初始状态 = %s，期望 ready", state)
	}
	expectSame := func(step string) AccountStatus {
		t.Helper()
		direct, ok := pool.StatusOf(victim)
		if !ok {
			t.Fatalf("%s：账户 %s 不存在", step, victim)
		}
		listed := listedStatus(t, pool, victim)
		if listed.State != direct.State || listed.Enabled != direct.Enabled || listed.BenefitTier != direct.BenefitTier ||
			len(listed.Models) != len(direct.Models) || len(listed.Cooldowns) != len(direct.Cooldowns) {
			t.Fatalf("%s：Status() 列表中的账户 %+v 与 StatusOf 的 %+v 不一致", step, listed, direct)
		}
		return listed
	}

	if err := pool.MarkUnavailable(victim, "测试不可用"); err != nil {
		t.Fatal(err)
	}
	if listed := expectSame("MarkUnavailable 之后"); listed.State != AccountUnavailable || listed.Message != "测试不可用" {
		t.Fatalf("MarkUnavailable 之后 Status() 列表中的状态 = %s（%s），期望 unavailable", listed.State, listed.Message)
	}
	if err := pool.MarkReady(victim); err != nil {
		t.Fatal(err)
	}
	if listed := expectSame("MarkReady 之后"); listed.State != AccountReady {
		t.Fatalf("MarkReady 之后 Status() 列表中的状态 = %s，期望 ready", listed.State)
	}

	until := time.Now().Add(time.Hour)
	if err := pool.MarkCooldownIfGeneration(victim, "", pool.ModelAccessGeneration(victim), time.Now(), until, DailyQuotaKind); err != nil {
		t.Fatal(err)
	}
	if listed := expectSame("全局冷却之后"); listed.State != AccountCooldown || len(listed.Cooldowns) != 1 {
		t.Fatalf("全局冷却之后 Status() 列表中的状态 = %s，冷却 %d 条，期望 cooldown 与 1 条", listed.State, len(listed.Cooldowns))
	}
	if err := pool.ClearCooldownIfGeneration(victim, "", pool.ModelAccessGeneration(victim), time.Now()); err != nil {
		t.Fatal(err)
	}
	if listed := expectSame("清除冷却之后"); listed.State != AccountReady || len(listed.Cooldowns) != 0 {
		t.Fatalf("清除冷却之后 Status() 列表中的状态 = %s，仍有 %d 条冷却", listed.State, len(listed.Cooldowns))
	}
	// 变化只更新缓存中的一项，不丢掉整份列表：账户上万时重建要几十毫秒，冷却与目录同步每秒都有几次
	pool.mu.Lock()
	cached := pool.statusCache != nil
	pool.mu.Unlock()
	if !cached {
		t.Fatal("账户变化后缓存的状态列表应就地更新，而不是整份作废")
	}

	if err := pool.SetCatalog(victim, BenefitTierUltra, indexTestCatalog()[:1]); err != nil {
		t.Fatal(err)
	}
	if listed := expectSame("目录与权益变化之后"); listed.BenefitTier != BenefitTierUltra.String() || len(listed.Models) != 1 {
		t.Fatalf("目录与权益变化之后 Status() 列表中的权益 = %s，模型 %v，期望 Ultra 与 1 个模型", listed.BenefitTier, listed.Models)
	}

	// 增删账户后列表长度也要立即变化
	if err := pool.Add(indexTestAccount("status-added@example.com", BenefitTierFree, indexTestCatalog())); err != nil {
		t.Fatal(err)
	}
	if got := len(pool.Status()); got != statusCacheMinAccounts+2 {
		t.Fatalf("新增账户后 Status() 返回 %d 个账户，期望 %d", got, statusCacheMinAccounts+2)
	}
}
