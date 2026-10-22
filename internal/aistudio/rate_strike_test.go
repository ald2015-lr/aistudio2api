package aistudio

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// 连续裸限流档位的测试：档位时长直接传分钟级（不等待），“冷却到期后再失败”用 CheckedAt = 上次 Result.Until + 1ms 模拟
// （见 nextAfter：临近太平洋时间零点时各档都被截到零点，Until 相同，CheckedAt 仍须严格递增）

const (
	strikeAccount = "strike@example.com"
	strikeModel   = "strike-model"
)

// strikeLadder 为测试用的四档时长：各档不同，便于按到期时间区分档位
var strikeLadder = []time.Duration{time.Minute, 5 * time.Minute, 30 * time.Minute, 2 * time.Hour}

// nextAfter 返回“上一档冷却到期后取得的租约”的检查时间：until + 1ms，且严格晚于上一次失败的检查时间 previous。
// 临近太平洋时间零点时各档都被截到零点，相邻两次的 until 相同，直接用 until + 1ms 会与上一次的 CheckedAt 相同而被判为过旧
func nextAfter(previous time.Time, until time.Time) time.Time {
	if until.After(previous) {
		return until.Add(time.Millisecond)
	}
	return previous.Add(time.Millisecond)
}

// strikePool 返回只在内存中的单账户号池：Playground 与 Build 目录都支持 strikeModel，按 channels 启用上游通道
func strikePool(t *testing.T, channels ...Channel) *AccountPool {
	t.Helper()
	model := Model{ID: strikeModel, Methods: []string{"generateContent"}, Capabilities: map[string]bool{"chat_model": true}}
	pool := NewAccountPool([]*Account{indexTestAccount(strikeAccount, BenefitTierFree, []Model{model})}, 2)
	t.Cleanup(pool.WaitBackground)
	if err := pool.SetBuildCatalog(strikeAccount, []Model{model}); err != nil {
		t.Fatal(err)
	}
	pool.SetUpstreamChannels(channels)
	return pool
}

// strikeMark 构造一次裸限流写入
func strikeMark(pool *AccountPool, channel Channel, checkedAt time.Time, shared bool, escalate bool) RateLimitMark {
	return RateLimitMark{
		AccountID: strikeAccount, ModelID: strikeModel, Channel: channel,
		Generation: pool.ModelAccessGeneration(strikeAccount), CheckedAt: checkedAt,
		Cooldown: QuotaCooldown{
			Kind: RateLimitKind, Reason: RateLimitKind + ": AI Studio GenerateContent 返回 HTTP 429: Quota exceeded", Escalate: true,
		},
		Ladder: strikeLadder, Shared: shared, Escalate: escalate,
	}
}

// markStrike 写入一次裸限流，必须真正写入；返回结果与调用前后的时刻（用于按档位核对到期时间）
func markStrike(t *testing.T, pool *AccountPool, mark RateLimitMark) (RateLimitMarkResult, time.Time, time.Time) {
	t.Helper()
	before := time.Now()
	result, err := pool.MarkRateLimitCooldown(mark)
	after := time.Now()
	if err != nil || result.Write != CooldownWritten {
		t.Fatalf("写入裸限流冷却：result=%+v err=%v", result, err)
	}
	return result, before, after
}

// assertStepUntil 断言到期时间为第 level 档在 [before, after] 之间某一时刻的计算结果（含次日零点截短）
func assertStepUntil(t *testing.T, label string, got time.Time, level int, before time.Time, after time.Time) {
	t.Helper()
	low, high := rateLimitStepUntil(strikeLadder, level, before), rateLimitStepUntil(strikeLadder, level, after)
	if got.Before(low) || got.After(high) {
		t.Fatalf("%s：到期 %s，期望第 %d 档的 %s 到 %s", label, got.Format(time.RFC3339Nano), level,
			low.Format(time.RFC3339Nano), high.Format(time.RFC3339Nano))
	}
}

// strikeCooldown 返回账户在冷却键上的冷却；没有或已到期时 ok 为假
func strikeCooldown(t *testing.T, pool *AccountPool, scope string) (CooldownState, bool) {
	t.Helper()
	cooldowns, _ := runtimeSnapshot(t, pool, strikeAccount)
	cooldown, exists := cooldowns[scope]
	return cooldown, exists && cooldown.Active(time.Now())
}

// strikeRecord 返回账户内存中的档位记录
func strikeRecord(t *testing.T, pool *AccountPool, group Channel) (rateStrike, bool) {
	t.Helper()
	pool.mu.Lock()
	defer pool.mu.Unlock()
	record, exists := pool.byID[strikeAccount].rateStrikes[rateStrikeKey{model: strikeModel, group: group}]
	return record, exists
}

var strikeBuildKey = ChannelCooldownScope(ChannelBuild, strikeModel)

// TestRateStrikeLevels 冷却到期后再次裸限流逐档升级：1→2→3→4→5（次日零点），第 5 档之后不再升；共用时两个通道都写入，
// 原因保留“限流: ”前缀并写明档位
func TestRateStrikeLevels(t *testing.T) {
	pool := strikePool(t, ChannelPlayground, ChannelBuild)
	checkedAt := time.Now().Add(-time.Second)
	for level := 1; level <= 6; level++ {
		result, before, after := markStrike(t, pool, strikeMark(pool, ChannelPlayground, checkedAt, true, true))
		want := min(level, len(strikeLadder)+1)
		if result.Level != want || !result.Synced {
			t.Fatalf("第 %d 次：result=%+v，期望第 %d 档且同步另一通道", level, result, want)
		}
		assertStepUntil(t, fmt.Sprintf("第 %d 次", level), result.Until, want, before, after)
		for _, scope := range []string{strikeModel, strikeBuildKey} {
			cooldown, cooling := strikeCooldown(t, pool, scope)
			if !cooling || !cooldown.Until.Equal(result.Until) {
				t.Fatalf("第 %d 次：%s 冷却=%+v，期望到 %s", level, scope, cooldown, result.Until)
			}
			if !rateCooldownReason(cooldown.Reason) || !strings.HasPrefix(cooldown.Reason, "限流: 连续限流第 ") ||
				!strings.Contains(cooldown.Reason, "Quota exceeded") || strings.Contains(cooldown.Reason, "限流: 限流: ") {
				t.Fatalf("第 %d 次：原因 %q 应为“限流: 连续限流第 N 档，<原文>”", level, cooldown.Reason)
			}
		}
		// 冷却到期后取得的租约再次失败（第 5 档冷却到次日零点，之后仍为第 5 档）
		checkedAt = nextAfter(checkedAt, result.Until)
	}
}

// TestRateLimitStepUntil 各档时长按太平洋时间次日零点截短，超出各档后为次日零点
func TestRateLimitStepUntil(t *testing.T) {
	location, err := time.LoadLocation(quotaResetTimezone)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 22, 23, 50, 0, 0, location)
	midnight := time.Date(2026, 10, 23, 0, 0, 0, 0, location)
	for level, want := range map[int]time.Time{
		1: now.Add(time.Minute), 2: now.Add(5 * time.Minute), 3: midnight, 4: midnight, 5: midnight, 9: midnight,
	} {
		if got := rateLimitStepUntil(strikeLadder, level, now); !got.Equal(want) {
			t.Fatalf("23:50 的第 %d 档 = %s，期望 %s", level, got, want)
		}
	}
	noon := time.Date(2026, 10, 22, 12, 0, 0, 0, location)
	if got := rateLimitStepUntil(strikeLadder, 4, noon); !got.Equal(noon.Add(2 * time.Hour)) {
		t.Fatalf("中午的第 4 档 = %s，期望 2 小时后", got)
	}
}

// TestRateStrikeQuotaDayReset 跨额度日（记录写入时的次日零点已过）后档位作废，从第一档起；作废的记录在下次写入时一并清理
func TestRateStrikeQuotaDayReset(t *testing.T) {
	pool := strikePool(t, ChannelPlayground, ChannelBuild)
	firstAt := time.Now().Add(-time.Second)
	first, _, _ := markStrike(t, pool, strikeMark(pool, ChannelPlayground, firstAt, true, true))
	secondAt := nextAfter(firstAt, first.Until)
	second, _, _ := markStrike(t, pool, strikeMark(pool, ChannelPlayground, secondAt, true, true))
	if second.Level != 2 {
		t.Fatalf("第二次应升到第 2 档：%+v", second)
	}
	pool.mu.Lock()
	account := pool.byID[strikeAccount]
	record := account.rateStrikes[rateStrikeKey{model: strikeModel}]
	record.reset = time.Now().Add(-time.Second)
	account.rateStrikes[rateStrikeKey{model: strikeModel}] = record
	// 另一条已作废的记录（不共用时 Build 的一份）
	account.rateStrikes[rateStrikeKey{model: strikeModel, group: ChannelBuild}] = rateStrike{level: 3, reset: time.Now().Add(-time.Second)}
	pool.mu.Unlock()
	third, _, _ := markStrike(t, pool, strikeMark(pool, ChannelPlayground, nextAfter(secondAt, second.Until), true, true))
	if third.Level != 1 {
		t.Fatalf("跨额度日后应从第 1 档起：%+v", third)
	}
	if record, exists := strikeRecord(t, pool, ""); !exists || record.level != 1 || !record.reset.After(time.Now()) {
		t.Fatalf("跨额度日后的档位记录 = %+v（存在=%t），期望第 1 档并按新的额度日作废", record, exists)
	}
	if _, exists := strikeRecord(t, pool, ChannelBuild); exists {
		t.Fatal("已作废的记录应在写入时清理")
	}
}

// TestRateStrikeConcurrentBurst 冷却写入前已经发出的并发请求（取得租约早于本档到期时间）随后也失败、同一请求换通道再试也失败时
// 不升档，冷却不缩短；本档到期后取得的租约再失败才升档
func TestRateStrikeConcurrentBurst(t *testing.T) {
	pool := strikePool(t, ChannelPlayground, ChannelBuild)
	start := time.Now().Add(-time.Second)
	first, _, _ := markStrike(t, pool, strikeMark(pool, ChannelPlayground, start, true, true))
	// 同一时刻前后取得租约的并发请求：同通道键的 CheckedAt 早于它时按过旧跳过，晚于它时写入但不升档
	if stale, err := pool.MarkRateLimitCooldown(strikeMark(pool, ChannelPlayground, start.Add(-time.Millisecond), true, true)); err != nil ||
		stale.Write != CooldownSkippedStale {
		t.Fatalf("取得租约更早的失败：result=%+v err=%v，期望按过旧跳过", stale, err)
	}
	burst, _, _ := markStrike(t, pool, strikeMark(pool, ChannelPlayground, start.Add(time.Millisecond), true, true))
	if burst.Level != 1 || burst.Until.Before(first.Until) {
		t.Fatalf("并发失败：%+v，期望仍为第 1 档且不早于 %s", burst, first.Until)
	}
	// 同一请求换到 Build 通道再试（租约晚于第一次、早于本档到期）
	switchedAt := start.Add(2 * time.Millisecond)
	switched, _, _ := markStrike(t, pool, strikeMark(pool, ChannelBuild, switchedAt, true, true))
	if switched.Level != 1 {
		t.Fatalf("换通道再试：%+v，期望仍为第 1 档", switched)
	}
	next, _, _ := markStrike(t, pool, strikeMark(pool, ChannelPlayground, nextAfter(switchedAt, switched.Until), true, true))
	if next.Level != 2 {
		t.Fatalf("本档到期后再失败：%+v，期望升到第 2 档", next)
	}
}

// TestRateStrikeNoEscalate 不允许升档（号池级限流、已输出内容后）时档位不变，只按第一档续冷却；之后允许升档时从原档位继续
func TestRateStrikeNoEscalate(t *testing.T) {
	pool := strikePool(t, ChannelPlayground, ChannelBuild)
	firstAt := time.Now().Add(-time.Second)
	first, before, after := markStrike(t, pool, strikeMark(pool, ChannelPlayground, firstAt, true, false))
	if first.Level != 1 {
		t.Fatalf("没有记录时不允许升档也为第 1 档：%+v", first)
	}
	assertStepUntil(t, "没有记录时不允许升档", first.Until, 1, before, after)
	secondAt := nextAfter(firstAt, first.Until)
	second, _, _ := markStrike(t, pool, strikeMark(pool, ChannelPlayground, secondAt, true, true))
	if second.Level != 2 {
		t.Fatalf("应升到第 2 档：%+v", second)
	}
	heldAt := nextAfter(secondAt, second.Until)
	held, _, _ := markStrike(t, pool, strikeMark(pool, ChannelPlayground, heldAt, true, false))
	if held.Level != 2 {
		t.Fatalf("不允许升档时档位应不变：%+v", held)
	}
	// 续到原到期与第一档时长中较晚的一个（CheckedAt 只是模拟到期，按真实时刻第 2 档的 5 分钟冷却仍生效，原到期更晚）
	if !held.Until.Equal(second.Until) {
		t.Fatalf("不允许升档的续冷却 = %s，期望保持原到期 %s", held.Until, second.Until)
	}
	// 上一次失败的租约就是 heldAt（held.Until + 1ms），再晚一点取得的租约才不算过旧
	third, _, _ := markStrike(t, pool, strikeMark(pool, ChannelPlayground, nextAfter(heldAt, held.Until), true, true))
	if third.Level != 3 {
		t.Fatalf("允许升档后应从第 2 档升到第 3 档：%+v", third)
	}
}

// TestRateStrikeSharedKeepsLaterCooldown 共用时另一通道已有更晚的冷却（如每日限额）保留不缩短、原因不变，只推进 CheckedAt；
// 另一通道已有不早于本次租约的记录时不写，Synced 为假
func TestRateStrikeSharedKeepsLaterCooldown(t *testing.T) {
	pool := strikePool(t, ChannelPlayground, ChannelBuild)
	generation := pool.ModelAccessGeneration(strikeAccount)
	daily := time.Now().Add(3 * time.Hour)
	start := time.Now().Add(-time.Minute)
	if write, err := pool.TryMarkCooldownIfGeneration(strikeAccount, strikeBuildKey, generation, start, daily, DailyQuotaKind+": 测试"); err != nil || write != CooldownWritten {
		t.Fatalf("写入 Build 每日限额：write=%s err=%v", write, err)
	}
	checkedAt := start.Add(time.Second)
	result, _, _ := markStrike(t, pool, strikeMark(pool, ChannelPlayground, checkedAt, true, true))
	if !result.Synced {
		t.Fatalf("另一通道应同步处理：%+v", result)
	}
	build, cooling := strikeCooldown(t, pool, strikeBuildKey)
	if !cooling || !build.Until.Equal(daily.UTC()) || build.Reason != DailyQuotaKind+": 测试" {
		t.Fatalf("Build 更晚的冷却应保留：%+v", build)
	}
	if _, access := runtimeSnapshot(t, pool, strikeAccount); !access[strikeBuildKey].CheckedAt.Equal(checkedAt.UTC()) {
		t.Fatalf("Build 键的 CheckedAt = %s，期望推进到 %s", access[strikeBuildKey].CheckedAt, checkedAt.UTC())
	}
	if playground, cooling := strikeCooldown(t, pool, strikeModel); !cooling || !playground.Until.Equal(result.Until) {
		t.Fatalf("Playground 应写入本档冷却：%+v", playground)
	}
	// 本通道键已有更晚的冷却时同样保留，Result.Until 为实际生效的恢复时间
	if write, err := pool.TryMarkCooldownIfGeneration(strikeAccount, strikeModel, generation, checkedAt.Add(time.Second), daily, DailyQuotaKind+": 测试"); err != nil || write != CooldownWritten {
		t.Fatalf("写入 Playground 每日限额：write=%s err=%v", write, err)
	}
	kept, _, _ := markStrike(t, pool, strikeMark(pool, ChannelPlayground, checkedAt.Add(2*time.Second), true, true))
	if !kept.Until.Equal(daily.UTC()) {
		t.Fatalf("本通道已有更晚的冷却时 Until = %s，期望 %s", kept.Until, daily.UTC())
	}

	// 另一通道的记录不早于本次租约：不写，Synced 为假
	other := strikePool(t, ChannelPlayground, ChannelBuild)
	later := time.Now().Add(time.Hour)
	if _, err := other.MarkModelAccessVerifiedIfGeneration(strikeAccount, strikeBuildKey, other.ModelAccessGeneration(strikeAccount), later); err != nil {
		t.Fatal(err)
	}
	skipped, _, _ := markStrike(t, other, strikeMark(other, ChannelPlayground, time.Now(), true, true))
	if skipped.Synced {
		t.Fatalf("另一通道有更晚的记录时不应同步：%+v", skipped)
	}
	if _, cooling := strikeCooldown(t, other, strikeBuildKey); cooling {
		t.Fatal("另一通道有更晚的记录时不应写入冷却")
	}
}

// TestTryExtendCooldownKeepsLater 额度冷却的单键写入不缩短：已有仍生效且更晚的冷却时保留它（原因也保留），只推进 CheckedAt，
// 返回 CooldownKeptLater 与实际生效的恢复时间；新冷却更晚时照常写入。TryMarkCooldownIfGeneration 仍无条件覆盖
func TestTryExtendCooldownKeepsLater(t *testing.T) {
	pool := strikePool(t, ChannelPlayground, ChannelBuild)
	generation := pool.ModelAccessGeneration(strikeAccount)
	start := time.Now().Add(-time.Minute)
	long := time.Now().Add(30 * time.Minute)
	if write, err := pool.TryMarkCooldownIfGeneration(strikeAccount, strikeModel, generation, start, long, RateLimitKind+": 连续限流第 3 档，测试"); err != nil || write != CooldownWritten {
		t.Fatalf("写入第 3 档：write=%s err=%v", write, err)
	}
	checkedAt := start.Add(time.Millisecond)
	write, effective, err := pool.TryExtendCooldownIfGeneration(strikeAccount, strikeModel, generation, checkedAt, time.Now().Add(30*time.Second), RateLimitKind+": 短")
	if err != nil || write != CooldownKeptLater || !effective.Equal(long.UTC()) {
		t.Fatalf("较短的冷却：write=%s effective=%s err=%v，期望保留 %s", write, effective, err, long.UTC())
	}
	if cooldown, cooling := strikeCooldown(t, pool, strikeModel); !cooling || !cooldown.Until.Equal(long.UTC()) || cooldown.Reason != RateLimitKind+": 连续限流第 3 档，测试" {
		t.Fatalf("更晚的冷却与原因应保留：%+v", cooldown)
	}
	if _, access := runtimeSnapshot(t, pool, strikeAccount); !access[strikeModel].CheckedAt.Equal(checkedAt.UTC()) {
		t.Fatalf("CheckedAt = %s，期望推进到 %s", access[strikeModel].CheckedAt, checkedAt.UTC())
	}
	if write, _, err := pool.TryExtendCooldownIfGeneration(strikeAccount, strikeModel, generation, checkedAt, time.Now().Add(time.Hour), RateLimitKind+": 过旧"); err != nil || write != CooldownSkippedStale {
		t.Fatalf("检查时间不晚于已推进的记录：write=%s err=%v，期望过旧", write, err)
	}
	longer := time.Now().Add(time.Hour)
	write, effective, err = pool.TryExtendCooldownIfGeneration(strikeAccount, strikeModel, generation, checkedAt.Add(time.Millisecond), longer, DailyQuotaKind+": 更晚")
	if err != nil || write != CooldownWritten || !effective.Equal(longer.UTC()) {
		t.Fatalf("更晚的冷却应写入：write=%s effective=%s err=%v", write, effective, err)
	}
	if cooldown, cooling := strikeCooldown(t, pool, strikeModel); !cooling || !cooldown.Until.Equal(longer.UTC()) || cooldown.Reason != DailyQuotaKind+": 更晚" {
		t.Fatalf("更晚的冷却应写入：%+v", cooldown)
	}
	// 其他调用方（单模型 403 等）用的 TryMarkCooldownIfGeneration 语义不变：照常覆盖
	short := time.Now().Add(30 * time.Second)
	if write, err := pool.TryMarkCooldownIfGeneration(strikeAccount, strikeModel, generation, checkedAt.Add(2*time.Millisecond), short, RateLimitKind+": 覆盖"); err != nil || write != CooldownWritten {
		t.Fatalf("TryMarkCooldownIfGeneration：write=%s err=%v", write, err)
	}
	if cooldown, cooling := strikeCooldown(t, pool, strikeModel); !cooling || !cooldown.Until.Equal(short.UTC()) {
		t.Fatalf("TryMarkCooldownIfGeneration 应照常覆盖：%+v", cooldown)
	}
}

// TestRateStrikeIndependentChannels 不共用时只写本通道的键，两个通道各记一份档位
func TestRateStrikeIndependentChannels(t *testing.T) {
	pool := strikePool(t, ChannelPlayground, ChannelBuild)
	firstAt := time.Now().Add(-time.Second)
	first, _, _ := markStrike(t, pool, strikeMark(pool, ChannelPlayground, firstAt, false, true))
	secondAt := nextAfter(firstAt, first.Until)
	second, _, _ := markStrike(t, pool, strikeMark(pool, ChannelPlayground, secondAt, false, true))
	if second.Level != 2 || second.Synced {
		t.Fatalf("Playground 第二次：%+v，期望第 2 档且不同步", second)
	}
	if _, cooling := strikeCooldown(t, pool, strikeBuildKey); cooling {
		t.Fatal("不共用时不应冷却 Build")
	}
	build, _, _ := markStrike(t, pool, strikeMark(pool, ChannelBuild, nextAfter(secondAt, second.Until), false, true))
	if build.Level != 1 {
		t.Fatalf("Build 的档位与 Playground 分开计：%+v，期望第 1 档", build)
	}
	if _, exists := strikeRecord(t, pool, ""); exists {
		t.Fatal("不共用时不应写共用的档位")
	}
	if record, exists := strikeRecord(t, pool, ChannelPlayground); !exists || record.level != 2 {
		t.Fatalf("Playground 档位 = %+v（存在=%t），期望第 2 档", record, exists)
	}
}

// TestRateStrikeSingleChannel 只启用一个通道时共用不影响写入：只写启用的那个键
func TestRateStrikeSingleChannel(t *testing.T) {
	pool := strikePool(t, ChannelPlayground)
	result, _, _ := markStrike(t, pool, strikeMark(pool, ChannelPlayground, time.Now().Add(-time.Second), true, true))
	if result.Synced {
		t.Fatalf("只启用一个通道时不应同步：%+v", result)
	}
	if _, cooling := strikeCooldown(t, pool, strikeBuildKey); cooling {
		t.Fatal("没有启用的 Build 通道不应写入冷却")
	}
	if !pool.AccountLongCooling(strikeAccount, strikeModel, 0) {
		t.Fatal("唯一启用的通道冷却后该模型应不可用")
	}
}

// TestRateStrikeSkippedKeepsLevel 目录代际不符或本通道键已有不早于本次租约的记录时不写入，档位不变
func TestRateStrikeSkippedKeepsLevel(t *testing.T) {
	pool := strikePool(t, ChannelPlayground, ChannelBuild)
	firstAt := time.Now().Add(-time.Second)
	first, _, _ := markStrike(t, pool, strikeMark(pool, ChannelPlayground, firstAt, true, true))
	before, _ := strikeRecord(t, pool, "")
	mark := strikeMark(pool, ChannelPlayground, nextAfter(firstAt, first.Until), true, true)
	mark.Generation++
	if result, err := pool.MarkRateLimitCooldown(mark); err != nil || result.Write != CooldownSkippedGeneration {
		t.Fatalf("目录代际不符：result=%+v err=%v", result, err)
	}
	if result, err := pool.MarkRateLimitCooldown(strikeMark(pool, ChannelPlayground, time.Now().Add(-time.Hour), true, true)); err != nil ||
		result.Write != CooldownSkippedStale {
		t.Fatalf("过旧的租约：result=%+v err=%v", result, err)
	}
	if after, _ := strikeRecord(t, pool, ""); after != before {
		t.Fatalf("跳过写入后档位 = %+v，期望不变 %+v", after, before)
	}
	for _, invalid := range []RateLimitMark{
		{AccountID: strikeAccount, ModelID: strikeModel, Ladder: strikeLadder},
		{AccountID: strikeAccount, CheckedAt: time.Now(), Ladder: strikeLadder},
		{AccountID: strikeAccount, ModelID: strikeModel, CheckedAt: time.Now()},
		{AccountID: strikeAccount, ModelID: strikeModel, CheckedAt: time.Now(), Ladder: []time.Duration{0}},
	} {
		if _, err := pool.MarkRateLimitCooldown(invalid); err == nil {
			t.Fatalf("参数无效时应返回错误：%+v", invalid)
		}
	}
}

// TestRateStrikeClearedBySuccess 该通道成功一次清零档位：成功的租约晚于最近一次失败才清，返回此前的档位，之后再失败从第一档起；
// 快路径（模型资格已验证、该键没有冷却且 CheckedAt 不早于本次）也能清；不共用时只清成功所在通道的一份
func TestRateStrikeClearedBySuccess(t *testing.T) {
	pool := strikePool(t, ChannelPlayground, ChannelBuild)
	generation := pool.ModelAccessGeneration(strikeAccount)
	firstAt := time.Now().Add(-time.Second)
	first, _, _ := markStrike(t, pool, strikeMark(pool, ChannelPlayground, firstAt, true, true))
	failedAt := nextAfter(firstAt, first.Until)
	second, _, _ := markStrike(t, pool, strikeMark(pool, ChannelPlayground, failedAt, true, true))
	// 第 2 档冷却到期后取得租约的成功
	successAt := nextAfter(failedAt, second.Until)
	// 取得租约早于最近一次失败的成功不清
	if _, cleared, err := pool.markModelAccessVerifiedClearing(strikeAccount, strikeBuildKey, generation, failedAt.Add(-time.Millisecond)); err != nil || cleared != 0 {
		t.Fatalf("早于失败的成功：cleared=%d err=%v，期望不清", cleared, err)
	}
	if record, exists := strikeRecord(t, pool, ""); !exists || record.level != 2 {
		t.Fatalf("早于失败的成功后档位 = %+v（存在=%t），期望仍为第 2 档", record, exists)
	}
	// 共用时任一通道成功都清共用的一份（后台写入，回调带此前档位）
	done := make(chan int, 1)
	pool.MarkGenerationVerifiedAsync(strikeAccount, strikeBuildKey, generation, successAt, func(_ bool, cleared int, err error) {
		if err != nil {
			t.Error(err)
		}
		done <- cleared
	})
	pool.WaitBackground()
	if cleared := <-done; cleared != 2 {
		t.Fatalf("成功清零返回的此前档位 = %d，期望 2", cleared)
	}
	if _, exists := strikeRecord(t, pool, ""); exists {
		t.Fatal("成功后档位应清零")
	}
	again, _, _ := markStrike(t, pool, strikeMark(pool, ChannelPlayground, successAt.Add(time.Second), true, true))
	if again.Level != 1 {
		t.Fatalf("清零后再失败应从第 1 档起：%+v", again)
	}

	// 快路径：Build 的模型资格已在未来时刻验证（没有冷却、CheckedAt 不早于本次），Playground 失败时 Build 键过旧不同步
	fast := strikePool(t, ChannelPlayground, ChannelBuild)
	fastGeneration := fast.ModelAccessGeneration(strikeAccount)
	future := time.Now().Add(time.Hour)
	if _, err := fast.MarkModelAccessVerifiedIfGeneration(strikeAccount, strikeBuildKey, fastGeneration, future); err != nil {
		t.Fatal(err)
	}
	if result, _, _ := markStrike(t, fast, strikeMark(fast, ChannelPlayground, time.Now(), true, true)); result.Synced {
		t.Fatalf("Build 键更晚时不应同步：%+v", result)
	}
	if changed, cleared, err := fast.markModelAccessVerifiedClearing(strikeAccount, strikeBuildKey, fastGeneration, future); err != nil || changed || cleared != 1 {
		t.Fatalf("快路径条件下的成功：changed=%t cleared=%d err=%v，期望清零第 1 档", changed, cleared, err)
	}
	if _, exists := strikeRecord(t, fast, ""); exists {
		t.Fatal("快路径条件下的成功也应清零档位")
	}
	if changed, cleared, err := fast.markModelAccessVerifiedClearing(strikeAccount, strikeBuildKey, fastGeneration, future); err != nil || changed || cleared != 0 {
		t.Fatalf("没有档位时重复成功：changed=%t cleared=%d err=%v", changed, cleared, err)
	}

	// 不共用：Build 成功只清 Build 的一份，Playground 的保留
	split := strikePool(t, ChannelPlayground, ChannelBuild)
	splitGeneration := split.ModelAccessGeneration(strikeAccount)
	playground, _, _ := markStrike(t, split, strikeMark(split, ChannelPlayground, time.Now().Add(-time.Second), false, true))
	build, _, _ := markStrike(t, split, strikeMark(split, ChannelBuild, time.Now().Add(-time.Second), false, true))
	splitAt := playground.Until.Add(time.Millisecond)
	if build.Until.After(splitAt) {
		splitAt = build.Until.Add(time.Millisecond)
	}
	if _, cleared, err := split.markModelAccessVerifiedClearing(strikeAccount, strikeBuildKey, splitGeneration, splitAt); err != nil || cleared != 1 {
		t.Fatalf("不共用时 Build 成功：cleared=%d err=%v", cleared, err)
	}
	if _, exists := strikeRecord(t, split, ChannelBuild); exists {
		t.Fatal("Build 成功应清 Build 的档位")
	}
	if _, exists := strikeRecord(t, split, ChannelPlayground); !exists {
		t.Fatal("Build 成功不应清 Playground 的档位")
	}
	// 目录代际不符时不清
	if _, cleared, err := split.markModelAccessVerifiedClearing(strikeAccount, strikeModel, splitGeneration+1, splitAt); err != nil || cleared != 0 {
		t.Fatalf("目录代际不符：cleared=%d err=%v，期望不清", cleared, err)
	}
	if _, exists := strikeRecord(t, split, ChannelPlayground); !exists {
		t.Fatal("目录代际不符时不应清 Playground 的档位")
	}
}
