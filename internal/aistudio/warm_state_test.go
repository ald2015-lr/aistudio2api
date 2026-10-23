package aistudio

import (
	"testing"
	"time"
)

// TestWarmWorkerStatesUnschedulable 淘汰选号与补登记的“不可调度”口径：停用、需要登录、不可用、其他非就绪状态、目录加载过却没有
// 可初始化模型（目录被同步成空、只剩不能初始化 WAA 的模型）都给出原因；就绪且有目录的账户、手工标热从没设过目录的账户为空。
// UnschedulableSet 与 WarmWorkerStates 口径一致，只返回不可调度的账户
func TestWarmWorkerStatesUnschedulable(t *testing.T) {
	chat := warmCoolingModel(strikeModel)
	video := Model{ID: "video-model", Methods: []string{"predictLongRunning"}}
	accounts := map[string]*Account{
		"ready@example.com":       indexTestAccount("ready@example.com", BenefitTierFree, []Model{chat}),
		"disabled@example.com":    indexTestAccount("disabled@example.com", BenefitTierFree, []Model{chat}),
		"auth@example.com":        indexTestAccount("auth@example.com", BenefitTierFree, []Model{chat}),
		"unavailable@example.com": indexTestAccount("unavailable@example.com", BenefitTierFree, []Model{chat}),
		"cleared@example.com":     indexTestAccount("cleared@example.com", BenefitTierFree, []Model{chat}),
		"video@example.com":       indexTestAccount("video@example.com", BenefitTierFree, []Model{video}),
		// 手工标热、从没设过目录（initializedAt 为零）：测试里常见，不算不可调度
		"manual@example.com": indexTestAccount("manual@example.com", BenefitTierFree, nil),
	}
	accounts["disabled@example.com"].Config.Enabled = false
	accounts["auth@example.com"].State = AccountAuthRequired
	accounts["unavailable@example.com"].State = AccountUnavailable
	accounts["manual@example.com"].initializedAt = time.Time{}
	list := make([]*Account, 0, len(accounts))
	ids := make([]string, 0, len(accounts))
	for id, account := range accounts {
		list = append(list, account)
		ids = append(ids, id)
	}
	pool := NewAccountPool(list, 2)
	// 目录被同步成空：SetCatalog 写入空目录后 initializedAt 非零、没有可初始化模型
	if err := pool.SetCatalog("cleared@example.com", BenefitTierFree, nil); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"ready@example.com":       "",
		"disabled@example.com":    UnschedulableDisabled,
		"auth@example.com":        UnschedulableAuthRequired,
		"unavailable@example.com": UnschedulableUnavailable,
		"cleared@example.com":     UnschedulableNoCatalog,
		"video@example.com":       UnschedulableNoCatalog,
		"manual@example.com":      "",
	}
	states := pool.WarmWorkerStates(ids, "", 15*time.Minute)
	for id, reason := range want {
		state, ok := states[id]
		if !ok {
			t.Fatalf("%s：没有返回状态", id)
		}
		if state.Unschedulable != reason {
			t.Fatalf("%s：Unschedulable=%q，期望 %q", id, state.Unschedulable, reason)
		}
	}
	set := pool.UnschedulableSet(append(ids, "missing@example.com"))
	for id, reason := range want {
		got, listed := set[id]
		if reason == "" && listed || reason != "" && got != reason {
			t.Fatalf("%s：UnschedulableSet 给出 %q（列出=%t），期望 %q", id, got, listed, reason)
		}
	}
	if _, listed := set["missing@example.com"]; listed || len(set) != 5 {
		t.Fatalf("UnschedulableSet 应只列出 5 个不可调度的账户，实际 %v", set)
	}
	// 恢复可调度：重新就绪、重新设上有聊天模型的目录后原因消失
	if err := pool.MarkReady("auth@example.com"); err != nil {
		t.Fatal(err)
	}
	if err := pool.SetCatalog("cleared@example.com", BenefitTierFree, []Model{chat}); err != nil {
		t.Fatal(err)
	}
	if set := pool.UnschedulableSet([]string{"auth@example.com", "cleared@example.com"}); len(set) != 0 {
		t.Fatalf("恢复后不应再不可调度，实际 %v", set)
	}
	// 停用账户的状态写回也保持停用（setAccountState 对停用账户写 disabled），原因仍是已停用
	if err := pool.MarkReady("disabled@example.com"); err != nil {
		t.Fatal(err)
	}
	if got := pool.UnschedulableSet([]string{"disabled@example.com"})["disabled@example.com"]; got != UnschedulableDisabled {
		t.Fatalf("停用账户 MarkReady 后原因 = %q，期望已停用", got)
	}
}
