package aistudio

import (
	"testing"
	"time"
)

// TestTryExtendCooldownSameUntilKept 已有仍生效、恢复时间与本次相同的冷却（同账号并发请求都被每日限额拒绝，参照上游 ea74f2f）：
// TryExtendCooldownIfGeneration 按保留处理，返回 CooldownKeptLater 与原恢复时间，原因保留，检查时间照常推进；
// TryMarkCooldownIfGeneration 照旧覆盖
func TestTryExtendCooldownSameUntilKept(t *testing.T) {
	pool := strikePool(t, ChannelPlayground, ChannelBuild)
	generation := pool.ModelAccessGeneration(strikeAccount)
	start := time.Now().Add(-time.Minute)
	until := time.Now().Add(3 * time.Hour).Truncate(time.Second)
	if write, err := pool.TryMarkCooldownIfGeneration(strikeAccount, strikeModel, generation, start, until, DailyQuotaKind+": 第一次"); err != nil || write != CooldownWritten {
		t.Fatalf("第一次写入：write=%s err=%v", write, err)
	}
	checkedAt := start.Add(time.Millisecond)
	write, effective, err := pool.TryExtendCooldownIfGeneration(strikeAccount, strikeModel, generation, checkedAt, until.In(time.FixedZone("PDT", -7*3600)), DailyQuotaKind+": 第二次")
	if err != nil || write != CooldownKeptLater || !effective.Equal(until) {
		t.Fatalf("恢复时间相同：write=%s effective=%s err=%v，期望保留 %s", write, effective, err, until)
	}
	if cooldown, cooling := strikeCooldown(t, pool, strikeModel); !cooling || !cooldown.Until.Equal(until) || cooldown.Reason != DailyQuotaKind+": 第一次" {
		t.Fatalf("原冷却与原因应保留：%+v", cooldown)
	}
	if _, access := runtimeSnapshot(t, pool, strikeAccount); !access[strikeModel].CheckedAt.Equal(checkedAt.UTC()) {
		t.Fatalf("CheckedAt = %s，期望推进到 %s", access[strikeModel].CheckedAt, checkedAt.UTC())
	}
	if write, err := pool.TryMarkCooldownIfGeneration(strikeAccount, strikeModel, generation, checkedAt.Add(time.Millisecond), until, DailyQuotaKind+": 覆盖"); err != nil || write != CooldownWritten {
		t.Fatalf("TryMarkCooldownIfGeneration 应照常覆盖：write=%s err=%v", write, err)
	}
	if CooldownKeptLater.String() != "已有相同或更晚的冷却，保留原冷却" {
		t.Fatalf("日志原因 = %q", CooldownKeptLater.String())
	}
}
