package aistudio

import (
	"context"
	"slices"
	"testing"
)

// routingPicks 在新建的 4 个账户（单账户并发 2）的账户池上按策略各取放 4 次：先走不限账户的索引快路径，再走限定账户（app 层传热账号列表）的路径，
// 返回依次选中的账户
func routingPicks(t *testing.T, strategy string) []string {
	t.Helper()
	pool, ids := quotaUsageTestPool(4, 2)
	pool.SetRoutingStrategy(strategy)
	var picks []string
	for _, selection := range []AccountSelection{
		{ModelID: indexTestModel, Method: "generateContent"},
		{ModelID: indexTestModel, Method: "generateContent", AllowedAccountIDs: ids},
	} {
		for i := 0; i < 4; i++ {
			lease, _, err := pool.TryAcquireFor(context.Background(), selection)
			if err != nil || lease == nil {
				t.Fatalf("%s: 第 %d 次取租约失败: %v", strategy, i, err)
			}
			picks = append(picks, lease.Account().ID)
			if err := lease.Release(); err != nil {
				t.Fatal(err)
			}
		}
	}
	return picks
}

// TestBalancedStrategyActsAsRoundRobin 平均调用在账户池内与 round-robin 完全相同：SetRoutingStrategy 把 balanced 存成 round-robin，
// 两条选号路径取到的顺序一致；原样存下会被五处按 "round-robin" 比较的判断当成 fill-first（每次都取第一个账户）
func TestBalancedStrategyActsAsRoundRobin(t *testing.T) {
	roundRobin := routingPicks(t, "round-robin")
	balanced := routingPicks(t, RoutingBalanced)
	fillFirst := routingPicks(t, "fill-first")
	if !slices.Equal(balanced, roundRobin) {
		t.Fatalf("balanced 的选号 = %v，应与 round-robin 相同 %v", balanced, roundRobin)
	}
	if slices.Equal(balanced, fillFirst) {
		t.Fatalf("balanced 的选号与 fill-first 相同 %v，没有规范成 round-robin", fillFirst)
	}
	pool, _ := quotaUsageTestPool(1, 2)
	pool.SetRoutingStrategy(RoutingBalanced)
	pool.mu.Lock()
	stored := pool.routingStrategy
	pool.mu.Unlock()
	if stored != "round-robin" {
		t.Fatalf("账户池存下的策略 = %q，期望 round-robin", stored)
	}
}
