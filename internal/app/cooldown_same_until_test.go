package app

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// perModelDailyQuotaError 构造按模型的每日限额 429（不带重试建议：恢复时间就是太平洋时间次日零点，同一天里每次都相同）
func perModelDailyQuotaError() error {
	return &aistudio.RPCError{
		Method: "GenerateContent", StatusCode: http.StatusTooManyRequests, Code: 8, Message: "You exceeded your current quota",
		Metadata: map[string]string{"quota_limit": "GenerateRequestsPerDayPerProjectPerModel-FreeTier"},
	}
}

// TestDailyCooldownSameUntilRetained 同账号两个并发请求都被每日限额拒绝（恢复时间都是太平洋时间次日零点，参照上游 ea74f2f）：
// 第一次写入冷却并记一行 WARN“账号冷却”；第二次恢复时间相同，按保留处理，只记一行 INFO“账号冷却未写入 … 原因=已有相同或更晚的冷却”，
// 不再写 WARN、不改原冷却；检查时间照常推进到第二个租约。原先第二次照样重写冷却并再写一行 WARN
func TestDailyCooldownSameUntilRetained(t *testing.T) {
	manager, pool, requests := partitionTestManager(t, []string{partitionNormalA}, nil, 1, 1, 0, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	service := retireTestService(ctx, manager, pool, requests)
	pool.SetPerAccountConcurrency(2)
	selection := aistudio.AccountSelection{AccountID: partitionNormalA, ModelID: testRuntimeModel}
	first, err := pool.AcquireFor(ctx, selection)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = first.Release() }()
	time.Sleep(2 * time.Millisecond)
	second, err := pool.AcquireFor(ctx, selection)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = second.Release() }()
	if !second.CheckedAt().After(first.CheckedAt()) {
		t.Fatal("前提：第二个租约晚于第一个")
	}
	for _, lease := range []*aistudio.AccountLease{first, second} {
		cooldown, ok, stateErr := service.applyQuotaCooldown(lease, partitionNormalA, testRuntimeModel, perModelDailyQuotaError(), "", aistudio.PoolScopeNormal, true, true)
		if !ok || stateErr != nil || cooldown.Kind != aistudio.DailyQuotaKind {
			t.Fatalf("应识别为每日限额：%+v ok=%v err=%v", cooldown, ok, stateErr)
		}
	}
	if got := countSourceLogs(requests, partitionNormalA, "账号冷却 | 类型=每日限额"); got != 1 {
		t.Fatalf("恢复时间相同的第二次不应再写 WARN，写了 %d 行：%v", got, serviceLogMessages(requests))
	}
	if got := countSourceLogs(requests, partitionNormalA, "账号冷却未写入 | 类型=每日限额", "原因=已有相同或更晚的冷却，保留原冷却"); got != 1 {
		t.Fatalf("第二次应记一行未写入：%v", serviceLogMessages(requests))
	}
	status, _ := pool.StatusOf(partitionNormalA)
	if cooldown, ok := status.Cooldowns[first.CooldownScope(testRuntimeModel)]; !ok || !cooldown.Active(time.Now()) {
		t.Fatalf("冷却应保留：%+v", status.Cooldowns)
	}
}
