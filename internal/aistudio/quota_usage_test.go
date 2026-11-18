package aistudio

import (
	"context"
	"fmt"
	"testing"
	"time"
	"unsafe"
)

// quotaUsageTestPool 返回 n 个 Free 账户、单账户并发 concurrency 的内存账户池与账户 ID（@example.com）
func quotaUsageTestPool(n int, concurrency int) (*AccountPool, []string) {
	catalog := indexTestCatalog()
	accounts := make([]*Account, 0, n)
	ids := make([]string, 0, n)
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("usage-%04d@example.com", i)
		accounts = append(accounts, indexTestAccount(id, BenefitTierFree, catalog))
		ids = append(ids, id)
	}
	return NewAccountPool(accounts, concurrency), ids
}

// quotaCount 返回当前额度日里账号的发送数
func quotaCount(pool *AccountPool, accountID string) uint32 {
	return pool.QuotaDayUsage().Counts[accountID]
}

// TestQuotaDayUsageCounts 今日发送数只按真正发往上游的生成请求计：beginGenerationSend 与 RecordGenerationSent 每次加 1，
// 同一租约上 401 续签后的重放算两次、long- 续写每一轮都算；取得租约没有发出就释放的不算，countTokens 的租约不算
func TestQuotaDayUsageCounts(t *testing.T) {
	pool, ids := quotaUsageTestPool(2, 2)
	ctx := context.Background()
	generate := AccountSelection{ModelID: indexTestModel, Method: "generateContent", AccountID: ids[0]}
	lease, _, err := pool.TryAcquireFor(ctx, generate)
	if err != nil || lease == nil {
		t.Fatalf("取生成租约: %v %v", lease, err)
	}
	if got := quotaCount(pool, ids[0]); got != 0 {
		t.Fatalf("只取得租约、没有发出时今日发送数 = %d，期望 0", got)
	}
	if err := lease.beginGenerationSend(indexTestModel); err != nil {
		t.Fatal(err)
	}
	if got := quotaCount(pool, ids[0]); got != 1 {
		t.Fatalf("发出一次后今日发送数 = %d，期望 1", got)
	}
	// 401 续签后在同一租约上重放：再发一次，算两次
	if err := lease.beginGenerationSend(indexTestModel); err != nil {
		t.Fatal(err)
	}
	if got := quotaCount(pool, ids[0]); got != 2 {
		t.Fatalf("401 重放后今日发送数 = %d，期望 2", got)
	}
	// long- 续写：每一轮重新预占名额再发送，每轮都算
	for round := 0; round < 3; round++ {
		lease.ReserveContinuation()
		if err := lease.beginGenerationSend(indexTestModel); err != nil {
			t.Fatal(err)
		}
	}
	if got := quotaCount(pool, ids[0]); got != 5 {
		t.Fatalf("续写 3 轮后今日发送数 = %d，期望 5", got)
	}
	lease.RecordGenerationSent()
	if got := quotaCount(pool, ids[0]); got != 6 {
		t.Fatalf("RecordGenerationSent 后今日发送数 = %d，期望 6", got)
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
	// 没有发出就释放的租约不算
	unsent, _, err := pool.TryAcquireFor(ctx, AccountSelection{ModelID: indexTestModel, Method: "generateContent", AccountID: ids[1]})
	if err != nil || unsent == nil {
		t.Fatalf("取生成租约: %v %v", unsent, err)
	}
	if err := unsent.Release(); err != nil {
		t.Fatal(err)
	}
	// countTokens 的租约不经过发送记账
	tokens, _, err := pool.TryAcquireFor(ctx, AccountSelection{ModelID: indexTestModel, Method: "countTokens", AccountID: ids[1]})
	if err != nil || tokens == nil {
		t.Fatalf("取 countTokens 租约: %v %v", tokens, err)
	}
	if err := tokens.Release(); err != nil {
		t.Fatal(err)
	}
	snapshot := pool.QuotaDayUsage()
	if got := snapshot.Counts[ids[1]]; got != 0 {
		t.Fatalf("没有发出与 countTokens 的租约不应计入，今日发送数 = %d", got)
	}
	if _, ok := snapshot.Counts[ids[1]]; ok {
		t.Fatalf("没有发送过的账号不应出现在副本里: %v", snapshot.Counts)
	}
	if snapshot.DayKey != QuotaDayKey(time.Now()) || snapshot.Day == "" {
		t.Fatalf("副本的额度日 = %d %q，期望 %d", snapshot.DayKey, snapshot.Day, QuotaDayKey(time.Now()))
	}
	// 副本与账户池互不影响
	snapshot.Counts[ids[0]] = 100
	if got := quotaCount(pool, ids[0]); got != 6 {
		t.Fatalf("改副本不应影响账户池，今日发送数 = %d", got)
	}
}

// TestQuotaDayUsageRollover 太平洋时间零点跨日清零（含 2026-03-08、2026-11-01 两个夏令时切换日，分别只有 23、25 小时），
// 同一额度日内墙钟回拨不清零，回拨到前一个额度日也不清零；版本在每次变化时加一
func TestQuotaDayUsageRollover(t *testing.T) {
	location, err := time.LoadLocation(quotaResetTimezone)
	if err != nil {
		t.Fatal(err)
	}
	pacific := func(year int, month time.Month, day, hour, minute int) time.Time {
		return time.Date(year, month, day, hour, minute, 0, 0, location)
	}
	pool, ids := quotaUsageTestPool(1, 2)
	account := ids[0]
	pool.mu.Lock()
	defer pool.mu.Unlock()
	note := func(now time.Time, times int) {
		for i := 0; i < times; i++ {
			pool.noteQuotaUsageLocked(account, now)
		}
	}
	expect := func(now time.Time, want uint32, wantDay string) QuotaUsageSnapshot {
		t.Helper()
		snapshot := pool.quotaDayUsageLocked(now)
		if snapshot.Counts[account] != want || snapshot.Day != wantDay {
			t.Fatalf("%s：今日发送数 = %d、额度日 = %s，期望 %d、%s", now.Format(time.RFC3339), snapshot.Counts[account], snapshot.Day, want, wantDay)
		}
		return snapshot
	}

	// 夏令时开始（2026-03-08，只有 23 小时）：当天 00:30（PST）记的到 23:30（PDT）仍在同一额度日，次日零点清零
	note(pacific(2026, time.March, 8, 0, 30), 5)
	march := expect(pacific(2026, time.March, 8, 23, 30), 5, "2026-03-08")
	if end := pool.quotaUsage.dayEnd; !end.Equal(pacific(2026, time.March, 9, 0, 0)) || end.Sub(pacific(2026, time.March, 8, 0, 0)) != 23*time.Hour {
		t.Fatalf("2026-03-08 额度日结束于 %s，期望太平洋时间 3 月 9 日零点、全天 23 小时", end)
	}
	next := expect(pacific(2026, time.March, 9, 0, 1), 0, "2026-03-09")
	if next.Version <= march.Version || next.DayKey <= march.DayKey {
		t.Fatalf("跨日清零后版本与额度日编号应变大: %+v → %+v", march, next)
	}

	// 夏令时结束（2026-11-01，有 25 小时）：当天 00:30（PDT）记的到 23:30（PST）仍在同一额度日，次日零点清零
	note(pacific(2026, time.November, 1, 0, 30), 7)
	expect(pacific(2026, time.November, 1, 23, 30), 7, "2026-11-01")
	if end := pool.quotaUsage.dayEnd; end.Sub(pacific(2026, time.November, 1, 0, 0)) != 25*time.Hour {
		t.Fatalf("2026-11-01 额度日应有 25 小时，结束于 %s", end)
	}
	expect(pacific(2026, time.November, 2, 0, 1), 0, "2026-11-02")

	// 23:59 → 00:01 跨日清零
	note(pacific(2026, time.November, 17, 23, 59), 3)
	before := expect(pacific(2026, time.November, 17, 23, 59), 3, "2026-11-17")
	after := expect(pacific(2026, time.November, 18, 0, 1), 0, "2026-11-18")
	if after.Version <= before.Version || after.DayKey <= before.DayKey {
		t.Fatalf("跨日清零后版本与额度日编号应变大: %+v → %+v", before, after)
	}
	if after.DayKey != QuotaDayKey(pacific(2026, time.November, 18, 12, 0)) {
		t.Fatalf("额度日编号 = %d，期望与 QuotaDayKey 一致", after.DayKey)
	}
	note(pacific(2026, time.November, 18, 0, 1), 2)
	expect(pacific(2026, time.November, 18, 0, 2), 2, "2026-11-18")

	// 同一额度日内墙钟回拨：不清零
	note(pacific(2026, time.November, 18, 10, 0), 1)
	expect(pacific(2026, time.November, 18, 9, 0), 3, "2026-11-18")
	// 回拨到前一个额度日：不清零、额度日不倒退；时间回到原额度日后照常累加，过了零点照常清零
	expect(pacific(2026, time.November, 17, 23, 58), 3, "2026-11-18")
	note(pacific(2026, time.November, 18, 10, 30), 1)
	expect(pacific(2026, time.November, 18, 10, 31), 4, "2026-11-18")
	expect(pacific(2026, time.November, 19, 0, 0), 0, "2026-11-19")
}

// TestQuotaDayUsageClockCorrection 墙钟先往前跳到两天后、随后被校正回来（开机时钟不准、NTP 校正等）：往前跳时照常清零；
// 校正回来时额度日跟着回到当天、计数保留（错误时钟期间的发送是刚刚真实发出的）、版本加一（写盘时改成正确的额度日），
// 之后照常在当天的零点清零；原先额度日停在未来那天，之后几个真实额度日都不清零
func TestQuotaDayUsageClockCorrection(t *testing.T) {
	location, err := time.LoadLocation(quotaResetTimezone)
	if err != nil {
		t.Fatal(err)
	}
	pacific := func(day, hour int) time.Time { return time.Date(2026, time.November, day, hour, 0, 0, 0, location) }
	pool, ids := quotaUsageTestPool(1, 2)
	account := ids[0]
	pool.mu.Lock()
	defer pool.mu.Unlock()
	note := func(now time.Time, times int) {
		for i := 0; i < times; i++ {
			pool.noteQuotaUsageLocked(account, now)
		}
	}
	expect := func(now time.Time, want uint32, wantDay string) QuotaUsageSnapshot {
		t.Helper()
		snapshot := pool.quotaDayUsageLocked(now)
		if snapshot.Counts[account] != want || snapshot.Day != wantDay || snapshot.DayKey != QuotaDayKey(now) {
			t.Fatalf("%s：今日发送数 = %d、额度日 = %s（编号与 QuotaDayKey 一致：%t），期望 %d、%s",
				now.Format(time.RFC3339), snapshot.Counts[account], snapshot.Day, snapshot.DayKey == QuotaDayKey(now), want, wantDay)
		}
		return snapshot
	}
	note(pacific(18, 8), 3)
	expect(pacific(18, 8), 3, "2026-11-18")
	// 墙钟跳到两天后：照常按跨日清零
	note(pacific(20, 9), 1)
	jumped := expect(pacific(20, 9), 1, "2026-11-20")
	// 校正回当天：额度日跟着回来，计数保留，版本加一
	corrected := expect(pacific(18, 9), 1, "2026-11-18")
	if corrected.Version <= jumped.Version {
		t.Fatalf("时钟校正改了额度日，版本应变大：%d → %d", jumped.Version, corrected.Version)
	}
	note(pacific(18, 10), 11)
	expect(pacific(18, 23), 12, "2026-11-18")
	// 当天零点照常清零，第二天只算第二天的
	expect(pacific(19, 0).Add(time.Minute), 0, "2026-11-19")
	note(pacific(19, 10), 10)
	expect(pacific(19, 23), 10, "2026-11-19")
	// 回拨不超过一个额度日（到前一天）时额度日不倒退、不清零，回来后照常累加
	if back := pool.quotaDayUsageLocked(pacific(18, 22)); back.Counts[account] != 10 || back.Day != "2026-11-19" {
		t.Fatalf("回拨到前一天：今日发送数 = %d、额度日 = %s，期望 10、2026-11-19", back.Counts[account], back.Day)
	}
	note(pacific(19, 23), 1)
	expect(pacific(19, 23), 11, "2026-11-19")
}

// timeLayout 与 time.Time 的内存布局相同（wall、ext、loc），只用来在测试里造出“墙钟被改过、单调时钟照常往前”的时刻
type timeLayout struct {
	wall uint64
	ext  int64
	loc  *time.Location
}

// shiftWall 返回墙钟加 d（整秒）、单调时钟读数不变的时刻，与系统时钟被调整后 time.Now() 的返回值形状相同；t 必须来自 time.Now()。
// time.Time 的布局与预期不符（单调读数没有保留、墙钟没有按 d 变化）时返回 false，调用方跳过
func shiftWall(t time.Time, d time.Duration) (time.Time, bool) {
	if unsafe.Sizeof(t) != unsafe.Sizeof(timeLayout{}) {
		return t, false
	}
	shifted := t
	layout := (*timeLayout)(unsafe.Pointer(&shifted))
	const hasMonotonic = 1 << 63
	if layout.wall&hasMonotonic == 0 {
		return t, false
	}
	layout.wall += uint64(d/time.Second) << 30
	// 单调时钟比较：与原时刻相同；墙钟比较：差 d
	if shifted.Sub(t) != 0 || shifted.Round(0).Sub(t.Round(0)) != d {
		return t, false
	}
	return shifted, true
}

// TestQuotaDayUsageClockCorrectionMonotonic 与生产相同，时刻都来自 time.Now()（带单调时钟读数）：墙钟先往前跳 3 天、随后被校正回来，
// 额度日跟着回到当天（编号与 QuotaDayKey 一致）。原先两个时刻都带单调读数时 Before 只比单调时钟，校正回来时判断不出墙钟回拨，
// 额度日一直停在 3 天后
func TestQuotaDayUsageClockCorrectionMonotonic(t *testing.T) {
	pool, ids := quotaUsageTestPool(1, 2)
	account := ids[0]
	pool.mu.Lock()
	defer pool.mu.Unlock()
	pool.noteQuotaUsageLocked(account, time.Now())
	jumped, ok := shiftWall(time.Now(), 72*time.Hour)
	if !ok {
		t.Skip("time.Time 的内存布局与预期不符，造不出墙钟被调整过的时刻")
	}
	pool.noteQuotaUsageLocked(account, jumped)
	if got := pool.quotaUsage.dayKey; got != QuotaDayKey(jumped) {
		t.Fatalf("墙钟跳到 3 天后时额度日编号 = %d，期望 %d", got, QuotaDayKey(jumped))
	}
	// 校正回来：墙钟回到真实时间，单调时钟照常往前
	for i := 0; i < 10; i++ {
		pool.noteQuotaUsageLocked(account, time.Now())
	}
	now := time.Now()
	snapshot := pool.quotaDayUsageLocked(now)
	if snapshot.DayKey != QuotaDayKey(now) || snapshot.Day != nextQuotaDay(now).AddDate(0, 0, -1).Format(time.DateOnly) {
		t.Fatalf("校正后额度日 = %s（编号 %d），期望当天（编号 %d）", snapshot.Day, snapshot.DayKey, QuotaDayKey(now))
	}
	if got := snapshot.Counts[account]; got != 11 {
		t.Fatalf("校正后计数 = %d，期望保留跳回之后的 11 次", got)
	}
}

// TestQuotaDayUsageSurvivesReimport 计数按账户 ID 存在账户池里：运行中重新导入账号（先 Remove 再 Add 一个新对象）后计数仍在，
// 新对象上的发送接着累加
func TestQuotaDayUsageSurvivesReimport(t *testing.T) {
	pool, ids := quotaUsageTestPool(2, 2)
	ctx := context.Background()
	send := func(accountID string) {
		t.Helper()
		lease, _, err := pool.TryAcquireFor(ctx, AccountSelection{ModelID: indexTestModel, Method: "generateContent", AccountID: accountID})
		if err != nil || lease == nil {
			t.Fatalf("取生成租约: %v %v", lease, err)
		}
		if err := lease.beginGenerationSend(indexTestModel); err != nil {
			t.Fatal(err)
		}
		if err := lease.Release(); err != nil {
			t.Fatal(err)
		}
	}
	send(ids[0])
	send(ids[0])
	if _, err := pool.Remove(ids[0], func(*Account) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if got := quotaCount(pool, ids[0]); got != 2 {
		t.Fatalf("移除账号后计数 = %d，期望保留 2", got)
	}
	if err := pool.Add(indexTestAccount(ids[0], BenefitTierFree, indexTestCatalog())); err != nil {
		t.Fatal(err)
	}
	send(ids[0])
	if got := quotaCount(pool, ids[0]); got != 3 {
		t.Fatalf("重新导入后再发一次，计数 = %d，期望 3", got)
	}
}

// TestSeedQuotaDayUsage 并回重启前保存的计数：同一额度日按 max 合并、可重复调用，不同额度日忽略
func TestSeedQuotaDayUsage(t *testing.T) {
	pool, ids := quotaUsageTestPool(3, 2)
	pool.mu.Lock()
	now := time.Now()
	pool.noteQuotaUsageLocked(ids[0], now)
	pool.noteQuotaUsageLocked(ids[0], now)
	pool.noteQuotaUsageLocked(ids[0], now)
	pool.noteQuotaUsageLocked(ids[1], now)
	pool.mu.Unlock()
	today := QuotaDayKey(now)
	before := pool.QuotaDayUsage()
	saved := map[string]uint32{ids[0]: 2, ids[1]: 9, "gone@example.com": 4}
	if !pool.SeedQuotaDayUsage(today, saved) {
		t.Fatal("同一额度日的计数应并回")
	}
	after := pool.QuotaDayUsage()
	want := map[string]uint32{ids[0]: 3, ids[1]: 9, "gone@example.com": 4}
	for accountID, count := range want {
		if after.Counts[accountID] != count {
			t.Fatalf("并回后 %s = %d，期望 %d（取两边较大的）；全部 %v", accountID, after.Counts[accountID], count, after.Counts)
		}
	}
	if after.Version <= before.Version {
		t.Fatalf("并回改变了计数，版本应变大: %d → %d", before.Version, after.Version)
	}
	// 重复并回同一份不累加、版本不变
	if !pool.SeedQuotaDayUsage(today, saved) {
		t.Fatal("重复并回同一额度日应返回 true")
	}
	again := pool.QuotaDayUsage()
	if again.Version != after.Version || again.Counts[ids[1]] != 9 {
		t.Fatalf("重复并回不应改变计数: %v（版本 %d → %d）", again.Counts, after.Version, again.Version)
	}
	// 前一个额度日的计数忽略
	if pool.SeedQuotaDayUsage(QuotaDayKey(now.Add(-36*time.Hour)), map[string]uint32{ids[2]: 50}) {
		t.Fatal("不同额度日的计数不应并回")
	}
	if got := quotaCount(pool, ids[2]); got != 0 {
		t.Fatalf("不同额度日的计数被并回了: %d", got)
	}
}
