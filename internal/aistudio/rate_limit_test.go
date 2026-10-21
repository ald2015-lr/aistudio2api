package aistudio

import (
	"context"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// rateTestSelection 为计入每分钟上限的生成选择
var rateTestSelection = AccountSelection{ModelID: indexTestModel, Method: "generateContent"}

// newRateTestPool 返回只在内存中的账户池：每分钟窗口为 window、上限为 limit，单账户并发为 concurrency
func newRateTestPool(t *testing.T, window time.Duration, limit int, concurrency int, accounts ...*Account) *AccountPool {
	t.Helper()
	pool := NewAccountPool(accounts, concurrency)
	pool.mu.Lock()
	pool.rateWindow = window
	pool.mu.Unlock()
	pool.SetAccountRateLimit(limit)
	return pool
}

// rateSnapshot 返回账户窗口内的发送数、记录的发送总数、预占数与缓存的满窗截止时刻
func rateSnapshot(pool *AccountPool, id string) (live int, total int, pending int, until int64) {
	pool.mu.Lock()
	defer pool.mu.Unlock()
	account := pool.byID[id]
	if account == nil || account.rate == nil {
		return 0, 0, 0, 0
	}
	cutoff := time.Now().UnixNano() - int64(pool.rateWindowLocked())
	for _, stamp := range account.rate.sent {
		if stamp > cutoff {
			live++
		}
	}
	return live, len(account.rate.sent), account.rate.pending, account.rate.until
}

// mustTryAcquire 立即取得一个租约，取不到时失败
func mustTryAcquire(t *testing.T, pool *AccountPool, selection AccountSelection) *AccountLease {
	t.Helper()
	lease, _, err := pool.TryAcquireFor(context.Background(), selection)
	if err != nil || lease == nil {
		t.Fatalf("选择 %+v 应立即取得租约: lease=%v err=%v", selection, lease != nil, err)
	}
	return lease
}

// sendAndRelease 取得生成租约、记一次发送后释放
func sendAndRelease(t *testing.T, pool *AccountPool, selection AccountSelection) {
	t.Helper()
	lease := mustTryAcquire(t, pool, selection)
	lease.RecordGenerationSent()
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
}

// TestAccountRateLimitBlocksAfterSends 窗口内发满上限后不再给生成租约，最早可用时间为第一次发送加窗口；窗口释放后
// AcquireFor 按这个时间自行醒来取得租约，不需要别的唤醒
func TestAccountRateLimitBlocksAfterSends(t *testing.T) {
	const window = 300 * time.Millisecond
	pool := newRateTestPool(t, window, 3, 2, indexTestAccount("a@example.com", BenefitTierFree, indexTestCatalog()))
	for range 3 {
		sendAndRelease(t, pool, rateTestSelection)
	}
	if lease, waitable, err := pool.TryAcquireFor(context.Background(), rateTestSelection); lease != nil || !waitable || err != nil {
		t.Fatalf("发满 3 次后第 4 个租约 = %v（可等待=%v）%v，期望取不到且可等待", lease != nil, waitable, err)
	}
	pool.mu.Lock()
	first := pool.byID["a@example.com"].rate.sent[0]
	lease, earliest, waitable, err := pool.tryAcquireLocked(rateTestSelection, time.Now())
	pool.mu.Unlock()
	if want := time.Unix(0, first+int64(window)).UTC(); lease != nil || !waitable || err != nil || !earliest.Equal(want) {
		t.Fatalf("满窗时 tryAcquireLocked = %v %s %v %v，期望最早 %s", lease != nil, earliest, waitable, err, want)
	}
	if _, _, pending, _ := rateSnapshot(pool, "a@example.com"); pending != 0 {
		t.Fatalf("取不到租约时不应预占，pending=%d", pending)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	acquired, err := pool.AcquireFor(ctx, rateTestSelection)
	if err != nil {
		t.Fatalf("窗口释放后应取得租约: %v", err)
	}
	if now := time.Now(); now.Before(earliest) {
		t.Fatalf("在窗口释放（%s）之前就取得了租约（%s）", earliest, now)
	}
	if err := acquired.Release(); err != nil {
		t.Fatal(err)
	}
}

// TestAccountRateReservationRefundedWhenNotSent 取得但没有发出的生成租约占着名额；释放时退还预占并唤醒等待方
func TestAccountRateReservationRefundedWhenNotSent(t *testing.T) {
	pool := newRateTestPool(t, 10*time.Second, 2, 4, indexTestAccount("a@example.com", BenefitTierFree, indexTestCatalog()))
	first := mustTryAcquire(t, pool, rateTestSelection)
	second := mustTryAcquire(t, pool, rateTestSelection)
	if lease, waitable, _ := pool.TryAcquireFor(context.Background(), rateTestSelection); lease != nil || !waitable {
		t.Fatalf("两个预占占满上限 2 时第 3 个租约 = %v（可等待=%v），期望取不到", lease != nil, waitable)
	}
	if _, _, pending, until := rateSnapshot(pool, "a@example.com"); pending != 2 || until != rateUntilPending {
		t.Fatalf("pending=%d until=%d，期望 2 与只因预占而满", pending, until)
	}
	// 只因预占而满时不设最早可用时间，等租约释放的唤醒
	pool.mu.Lock()
	_, earliest, _, _ := pool.tryAcquireLocked(rateTestSelection, time.Now())
	pool.mu.Unlock()
	if !earliest.IsZero() {
		t.Fatalf("只因预占而满时最早可用时间应为空，得到 %s", earliest)
	}
	changed := pool.Changed()
	if err := first.Release(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-changed:
	default:
		t.Fatal("释放预占后应唤醒等待方")
	}
	third := mustTryAcquire(t, pool, rateTestSelection)
	for _, lease := range []*AccountLease{second, third} {
		if err := lease.Release(); err != nil {
			t.Fatal(err)
		}
	}
	if _, total, pending, until := rateSnapshot(pool, "a@example.com"); pending != 0 || total != 0 || until != 0 {
		t.Fatalf("全部释放后 pending=%d 发送=%d until=%d，期望都为 0", pending, total, until)
	}
}

// TestAccountRateConcurrentLeasesCannotOversubscribe 并发取租约、发送、释放时，任何时刻窗口内已发出数加预占数不超过上限
func TestAccountRateConcurrentLeasesCannotOversubscribe(t *testing.T) {
	const limit = 5
	pool := newRateTestPool(t, 150*time.Millisecond, limit, 16, indexTestAccount("a@example.com", BenefitTierFree, indexTestCatalog()))
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	var sends atomic.Int64
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				lease, err := pool.AcquireFor(ctx, rateTestSelection)
				if err != nil {
					return
				}
				if live, _, pending, _ := rateSnapshot(pool, "a@example.com"); live+pending > limit {
					t.Errorf("窗口内已发出 %d + 预占 %d 超过上限 %d", live, pending, limit)
				}
				lease.RecordGenerationSent()
				sends.Add(1)
				if err := lease.Release(); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	wg.Wait()
	// 500ms 内窗口至少释放两次：发送数不少于两个窗口的上限，也不超过四个窗口的上限
	if got := sends.Load(); got < 2*limit || got > 4*limit {
		t.Fatalf("500ms 内发送 %d 次，期望在 %d 到 %d 之间", got, 2*limit, 4*limit)
	}
	if _, _, pending, _ := rateSnapshot(pool, "a@example.com"); pending != 0 {
		t.Fatalf("全部释放后 pending=%d", pending)
	}
}

// TestAccountRateMultipleSendsPerLease 同一租约发送两次（如 401 续签后的重放）记两条，预占只抵一次
func TestAccountRateMultipleSendsPerLease(t *testing.T) {
	pool := newRateTestPool(t, 10*time.Second, 10, 2, indexTestAccount("a@example.com", BenefitTierFree, indexTestCatalog()))
	lease := mustTryAcquire(t, pool, rateTestSelection)
	lease.RecordGenerationSent()
	lease.RecordGenerationSent()
	if live, _, pending, _ := rateSnapshot(pool, "a@example.com"); live != 2 || pending != 0 {
		t.Fatalf("发送两次后 live=%d pending=%d，期望 2 与 0", live, pending)
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
	if live, _, pending, _ := rateSnapshot(pool, "a@example.com"); live != 2 || pending != 0 {
		t.Fatalf("释放后 live=%d pending=%d，期望 2 与 0", live, pending)
	}
}

// TestAccountRateOnlyGenerationSelections 只拦生成选择：窗口已满时 countTokens、方法为空、Live、Veo 与独占租约照常取得且不预占；
// 文件绑定的生成选号不拦，但照常预占
func TestAccountRateOnlyGenerationSelections(t *testing.T) {
	const id = "a@example.com"
	account := indexTestAccount(id, BenefitTierFree, append(indexTestCatalog(), Model{ID: "index-veo", Methods: []string{"predictLongRunning"}}))
	account.runtime.Resources["files/a"] = ResourceBinding{}
	pool := newRateTestPool(t, 10*time.Second, 1, 4, account)
	sendAndRelease(t, pool, rateTestSelection)
	if lease, _, _ := pool.TryAcquireFor(context.Background(), rateTestSelection); lease != nil {
		t.Fatal("窗口已满时生成选择不应取得租约")
	}
	for _, selection := range []AccountSelection{
		{ModelID: indexTestModel, Method: "countTokens"},
		{ModelID: indexTestModel},
		{ModelID: indexTestFlash, Method: "bidiGenerateContent"},
		{ModelID: "index-veo", Method: "predictLongRunning"},
		{AccountID: id},
	} {
		lease := mustTryAcquire(t, pool, selection)
		if _, _, pending, _ := rateSnapshot(pool, id); pending != 0 || lease.ratePending {
			t.Fatalf("选择 %+v 不应预占，pending=%d", selection, pending)
		}
		if err := lease.Release(); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	exclusive, err := pool.AcquireAccount(ctx, id)
	if err != nil {
		t.Fatalf("窗口已满时独占租约应照常取得: %v", err)
	}
	if _, _, pending, _ := rateSnapshot(pool, id); pending != 0 || exclusive.ratePending {
		t.Fatalf("独占租约不应预占，pending=%d", pending)
	}
	if err := exclusive.Release(); err != nil {
		t.Fatal(err)
	}

	bound, err := pool.AcquireFor(ctx, AccountSelection{ModelID: indexTestModel, Method: "generateContent", ResourceID: "files/a"})
	if err != nil {
		t.Fatalf("窗口已满时文件绑定的生成选号应照常取得: %v", err)
	}
	if _, _, pending, _ := rateSnapshot(pool, id); pending != 1 || !bound.ratePending {
		t.Fatalf("文件绑定的生成租约应预占，pending=%d", pending)
	}
	if err := bound.Release(); err != nil {
		t.Fatal(err)
	}
	if _, _, pending, _ := rateSnapshot(pool, id); pending != 0 {
		t.Fatalf("释放文件绑定的租约后 pending=%d", pending)
	}
}

// TestAccountRateHotUpdate 运行中调大上限立即可用并唤醒等待方，调小后变满，设为 0 不拦；关闭期间的发送照常记账，重新打开后仍满
func TestAccountRateHotUpdate(t *testing.T) {
	const id = "a@example.com"
	pool := newRateTestPool(t, 10*time.Second, 1, 4, indexTestAccount(id, BenefitTierFree, indexTestCatalog()))
	sendAndRelease(t, pool, rateTestSelection)
	if lease, _, _ := pool.TryAcquireFor(context.Background(), rateTestSelection); lease != nil {
		t.Fatal("上限 1 发送 1 次后应满")
	}
	changed := pool.Changed()
	pool.SetAccountRateLimit(2)
	select {
	case <-changed:
	default:
		t.Fatal("调大上限后应唤醒等待方")
	}
	if pool.AccountRateLimit() != 2 {
		t.Fatalf("AccountRateLimit=%d，期望 2", pool.AccountRateLimit())
	}
	sendAndRelease(t, pool, rateTestSelection)
	pool.SetAccountRateLimit(1)
	if lease, _, _ := pool.TryAcquireFor(context.Background(), rateTestSelection); lease != nil {
		t.Fatal("调小上限后应满")
	}
	// 值不变时不唤醒
	changed = pool.Changed()
	pool.SetAccountRateLimit(1)
	select {
	case <-changed:
		t.Fatal("上限不变时不应唤醒")
	default:
	}
	pool.SetAccountRateLimit(0)
	sendAndRelease(t, pool, rateTestSelection)
	if live, _, _, until := rateSnapshot(pool, id); live != 3 || until != 0 {
		t.Fatalf("关闭期间 live=%d until=%d，期望照常记账、不满", live, until)
	}
	pool.SetAccountRateLimit(3)
	if lease, _, _ := pool.TryAcquireFor(context.Background(), rateTestSelection); lease != nil {
		t.Fatal("重新打开上限 3 时窗口内已有 3 次，应满")
	}
	pool.SetAccountRateLimit(-5)
	if pool.AccountRateLimit() != 0 {
		t.Fatalf("负数上限应按 0 处理，得到 %d", pool.AccountRateLimit())
	}
}

// TestBeginGenerationSend 发出前没有冷却时记一次发送并抵掉预占；租约账户已进入冷却时返回带恢复时间与原因的
// AccountCoolingDownError、不记发送；释放后预占退还
func TestBeginGenerationSend(t *testing.T) {
	const id = "a@example.com"
	pool := newRateTestPool(t, 10*time.Second, 5, 4, indexTestAccount(id, BenefitTierFree, indexTestCatalog()))
	lease := mustTryAcquire(t, pool, rateTestSelection)
	if err := lease.beginGenerationSend(indexTestModel); err != nil {
		t.Fatalf("没有冷却时 beginGenerationSend = %v", err)
	}
	if live, _, pending, _ := rateSnapshot(pool, id); live != 1 || pending != 0 {
		t.Fatalf("发送后 live=%d pending=%d，期望 1 与 0", live, pending)
	}
	blocked := mustTryAcquire(t, pool, rateTestSelection)
	until := time.Now().Add(30 * time.Second)
	reason := MinuteQuotaKind + ": 测试"
	if err := pool.MarkCooldownIfGeneration(id, "", pool.ModelAccessGeneration(id), time.Now(), until, reason); err != nil {
		t.Fatal(err)
	}
	err := blocked.beginGenerationSend("models/" + indexTestModel)
	var cooling *AccountCoolingDownError
	if !errors.As(err, &cooling) || !errors.Is(err, ErrAccountCoolingDown) {
		t.Fatalf("冷却中 beginGenerationSend = %v，期望 AccountCoolingDownError", err)
	}
	if cooling.AccountID != id || !cooling.Until.Equal(until.UTC()) || cooling.Reason != reason {
		t.Fatalf("AccountCoolingDownError=%+v，期望恢复时间 %s 与原因 %q", cooling, until.UTC(), reason)
	}
	if live, _, pending, _ := rateSnapshot(pool, id); live != 1 || pending != 1 {
		t.Fatalf("拦下后 live=%d pending=%d，期望不记发送、预占仍在", live, pending)
	}
	for _, held := range []*AccountLease{lease, blocked} {
		if err := held.Release(); err != nil {
			t.Fatal(err)
		}
	}
	if live, _, pending, _ := rateSnapshot(pool, id); live != 1 || pending != 0 {
		t.Fatalf("释放后 live=%d pending=%d，期望 1 与 0", live, pending)
	}
	if err := (*AccountLease)(nil).beginGenerationSend(indexTestModel); err != nil {
		t.Fatalf("空租约 beginGenerationSend = %v", err)
	}
}

// TestAccountRateClassification 满窗的热账户分进 WarmBusy、备用账户分进 StandbyBusy 并计入 RateLimited，最早冷却时间不变；
// 固定账户、限定账户、候选索引与逐个判断四条路径结果一致；只因预占而满时同样按忙碌处理
func TestAccountRateClassification(t *testing.T) {
	catalog := indexTestCatalog()
	pool := newRateTestPool(t, 10*time.Second, 1, 2,
		indexTestAccount("s@example.com", BenefitTierFree, catalog), indexTestAccount("w@example.com", BenefitTierFree, catalog))
	warm := []string{"w@example.com"}
	ctx := context.Background()
	for _, id := range []string{"w@example.com", "s@example.com"} {
		pinned := rateTestSelection
		pinned.AccountID = id
		sendAndRelease(t, pool, pinned)
	}
	groups, err := pool.ClassifyCandidates(ctx, rateTestSelection, warm)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(groups.WarmBusy, []string{"w@example.com"}) || !slices.Equal(groups.StandbyBusy, []string{"s@example.com"}) ||
		groups.RateLimited != 2 || !groups.EarliestCooldown.IsZero() || !groups.Eligible ||
		len(groups.WarmReady)+len(groups.WarmAvailable)+len(groups.StandbyReady) != 0 {
		t.Fatalf("满窗分类 = %+v，期望 w 在 WarmBusy、s 在 StandbyBusy、RateLimited=2、没有冷却", groups)
	}
	pool.mu.Lock()
	now := time.Now()
	brute, bruteErr := pool.classifyCandidatesBruteLocked(rateTestSelection, warm, now)
	allowed := rateTestSelection
	allowed.AllowedAccountIDs = []string{"w@example.com", "s@example.com"}
	limited, limitedErr := pool.classifyCandidatesAtLocked(allowed, warm, now)
	limitedBrute, _ := pool.classifyCandidatesBruteLocked(allowed, warm, now)
	pinned := rateTestSelection
	pinned.AccountID = "w@example.com"
	fixed, fixedErr := pool.classifyCandidatesAtLocked(pinned, warm, now)
	fixedBrute, _ := pool.classifyCandidatesBruteLocked(pinned, warm, now)
	pool.mu.Unlock()
	if bruteErr != nil || limitedErr != nil || fixedErr != nil {
		t.Fatalf("分类出错: %v %v %v", bruteErr, limitedErr, fixedErr)
	}
	if !sameGroups(groups, brute) || !sameGroups(limited, limitedBrute) || !sameGroups(limited, groups) {
		t.Fatalf("索引、限定账户与逐个判断不一致\n索引=%+v\n限定=%+v\n参考=%+v", groups, limited, brute)
	}
	if !sameGroups(fixed, fixedBrute) || !slices.Equal(fixed.WarmBusy, []string{"w@example.com"}) || fixed.RateLimited != 1 {
		t.Fatalf("固定账户分类 = %+v，参考 %+v", fixed, fixedBrute)
	}
	// 非生成选择不看每分钟窗口
	if counts, err := pool.ClassifyCandidates(ctx, AccountSelection{ModelID: indexTestModel, Method: "countTokens"}, warm); err != nil ||
		counts.RateLimited != 0 || !slices.Equal(counts.WarmReady, []string{"w@example.com"}) {
		t.Fatalf("countTokens 分类 = %+v %v，期望不受每分钟窗口影响", counts, err)
	}

	// 只因预占而满：上限 1、持有一个没有发出的租约，账户并发还有空位，仍按忙碌处理
	pending := newRateTestPool(t, 10*time.Second, 1, 2, indexTestAccount("w@example.com", BenefitTierFree, catalog))
	held := mustTryAcquire(t, pending, rateTestSelection)
	defer held.Release()
	groups, err = pending.ClassifyCandidates(ctx, rateTestSelection, warm)
	if err != nil || !slices.Equal(groups.WarmBusy, []string{"w@example.com"}) || groups.RateLimited != 1 || !groups.EarliestCooldown.IsZero() {
		t.Fatalf("只因预占而满的分类 = %+v %v，期望 WarmBusy 且 RateLimited=1", groups, err)
	}
	pending.mu.Lock()
	err = pending.verifySchedulingIndexLocked()
	pending.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
}

// TestAccountRateRemoveAndReAdd 账户移除后窗口随账户对象丢弃；同 ID 重新加入的账户从空窗口开始
func TestAccountRateRemoveAndReAdd(t *testing.T) {
	const id = "a@example.com"
	catalog := indexTestCatalog()
	pool := newRateTestPool(t, 10*time.Second, 1, 2, indexTestAccount(id, BenefitTierFree, catalog))
	lease := mustTryAcquire(t, pool, rateTestSelection)
	lease.RecordGenerationSent()
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Remove(id, func(*Account) error { return nil }); err != nil {
		t.Fatal(err)
	}
	// 已移除账户的租约再记发送不 panic、也不记到任何账户上
	lease.RecordGenerationSent()
	if err := lease.beginGenerationSend(indexTestModel); err != nil {
		t.Fatalf("已移除账户的 beginGenerationSend = %v", err)
	}
	if err := pool.Add(indexTestAccount(id, BenefitTierFree, catalog)); err != nil {
		t.Fatal(err)
	}
	fresh := mustTryAcquire(t, pool, rateTestSelection)
	defer fresh.Release()
	if live, total, pending, _ := rateSnapshot(pool, id); live != 0 || total != 0 || pending != 1 {
		t.Fatalf("重新加入后 live=%d 发送=%d pending=%d，期望空窗口加一个预占", live, total, pending)
	}
	pool.mu.Lock()
	err := pool.verifySchedulingIndexLocked()
	pool.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
}

// TestComputeRateUntil 截止时刻：未满为 0；满窗为第 excess+1 条发送加窗口；预占不少于上限为 rateUntilPending
func TestComputeRateUntil(t *testing.T) {
	pool := NewAccountPool(nil, 2)
	pool.rateWindow = time.Second
	pool.accountRateLimit = 3
	window := int64(time.Second)
	now := int64(10 * time.Second)
	sent := []int64{now - 1500*int64(time.Millisecond), now - 900*int64(time.Millisecond), now - 500*int64(time.Millisecond), now - 100*int64(time.Millisecond)}
	for _, test := range []struct {
		pending int
		want    int64
	}{
		{0, sent[1] + window},
		{1, sent[2] + window},
		{2, sent[3] + window},
		{3, rateUntilPending},
	} {
		if got := pool.computeRateUntilLocked(&accountRate{sent: sent, pending: test.pending}, now); got != test.want {
			t.Fatalf("pending=%d 截止时刻 %d，期望 %d", test.pending, got, test.want)
		}
	}
	if got := pool.computeRateUntilLocked(&accountRate{sent: sent[2:]}, now); got != 0 {
		t.Fatalf("窗口内 2 次、上限 3 时应未满，得到 %d", got)
	}
	pool.accountRateLimit = 0
	if got := pool.computeRateUntilLocked(&accountRate{sent: sent, pending: 5}, now); got != 0 {
		t.Fatalf("上限为 0 时应未满，得到 %d", got)
	}
}

// TestAccountRatePendingToFiniteWakesWaiter 上限 1、并发 2：A 的预占占满窗口（只因预占而满），B 用 AcquireFor 等待调度信号；
// A 发出后截止时刻转为有限值，这一刻唤醒等待方（普通发送不唤醒），B 在窗口释放后取得租约，不必等 A 释放
func TestAccountRatePendingToFiniteWakesWaiter(t *testing.T) {
	const window = 300 * time.Millisecond
	const id = "a@example.com"
	// 普通发送（窗口没满、也不是从只因预占而满转出）不唤醒
	roomy := newRateTestPool(t, 10*time.Second, 3, 2, indexTestAccount(id, BenefitTierFree, indexTestCatalog()))
	lease := mustTryAcquire(t, roomy, rateTestSelection)
	changed := roomy.Changed()
	lease.RecordGenerationSent()
	select {
	case <-changed:
		t.Fatal("普通发送不应唤醒等待方")
	default:
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}

	pool := newRateTestPool(t, window, 1, 2, indexTestAccount(id, BenefitTierFree, indexTestCatalog()))
	held := mustTryAcquire(t, pool, rateTestSelection)
	defer held.Release()
	if _, _, pending, until := rateSnapshot(pool, id); pending != 1 || until != rateUntilPending {
		t.Fatalf("pending=%d until=%d，期望只因预占而满", pending, until)
	}
	type result struct {
		lease *AccountLease
		at    time.Time
		err   error
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan result, 1)
	go func() {
		lease, err := pool.AcquireFor(ctx, rateTestSelection)
		done <- result{lease, time.Now(), err}
	}()
	// 等 B 进入等待（只因预占而满时没有定时，只等调度信号）
	time.Sleep(100 * time.Millisecond)
	changed = pool.Changed()
	held.RecordGenerationSent()
	select {
	case <-changed:
	default:
		t.Fatal("最后一个预占发出、截止时刻转为有限值时应唤醒等待方")
	}
	pool.mu.Lock()
	releaseAt := time.Unix(0, pool.byID[id].rate.until)
	pool.mu.Unlock()
	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("B 应取得租约: %v", got.err)
		}
		if got.at.Before(releaseAt) {
			t.Fatalf("B 在窗口释放（%s）之前就取得了租约（%s）", releaseAt, got.at)
		}
		if late := got.at.Sub(releaseAt); late > window {
			t.Fatalf("B 在窗口释放后 %s 才取得租约，期望约一个窗口内", late)
		}
		if err := got.lease.Release(); err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Until(releaseAt) + 2*time.Second):
		t.Fatal("A 没有释放时 B 一直没有取得租约")
	}
}

// TestAccountRateWindowReleaseNotifies 窗口按时间释放时由号池的计时器发调度信号（app 层经 forwardPoolChanges 唤醒各队首，
// 不只靠等待方各自的计时器）：两个账户先后满窗，各自的窗口释放时各唤醒一次，不提前；之后没有满窗账户，计时器不再设。
// 断言不依赖测试协程的时序：窗口到期后把账户移出 rateFull 的只有计时器回调 rateWindowReleased，而且它在同一临界区里 notifyLocked，
// 所以每轮先取 Changed() 再在锁内看账户是否还在 rateFull——还在就等这个通道（移出时一定会关闭它），已移出就说明信号已经发出
// （测试协程停顿时可能在订阅之前），只核对移出不早于窗口释放时刻
func TestAccountRateWindowReleaseNotifies(t *testing.T) {
	const window = 300 * time.Millisecond
	ids := []string{"a@example.com", "b@example.com"}
	pool := newRateTestPool(t, window, 1, 2,
		indexTestAccount(ids[0], BenefitTierFree, indexTestCatalog()), indexTestAccount(ids[1], BenefitTierFree, indexTestCatalog()))
	for index, id := range ids {
		if index > 0 {
			time.Sleep(150 * time.Millisecond)
		}
		pinned := rateTestSelection
		pinned.AccountID = id
		sendAndRelease(t, pool, pinned)
	}
	for _, id := range ids {
		pool.mu.Lock()
		account := pool.byID[id]
		releaseAt := time.Unix(0, account.rate.until)
		pool.mu.Unlock()
		for {
			changed := pool.Changed()
			pool.mu.Lock()
			_, full := pool.rateFull[account]
			pool.mu.Unlock()
			if !full {
				if now := time.Now(); now.Before(releaseAt) {
					t.Fatalf("%s 的窗口在 %s 释放，%s 就已移出满窗集合（并发出调度信号）", id, releaseAt, now)
				}
				break
			}
			// 还在 rateFull：之后的移出一定关闭这个通道；其他原因的唤醒再看一次
			select {
			case <-changed:
			case <-time.After(time.Until(releaseAt) + 2*time.Second):
				t.Fatalf("%s 的窗口释放后没有调度信号", id)
			}
		}
		pinned := rateTestSelection
		pinned.AccountID = id
		if err := mustTryAcquire(t, pool, pinned).Release(); err != nil {
			t.Fatal(err)
		}
	}
	pool.mu.Lock()
	defer pool.mu.Unlock()
	if len(pool.rateFull) != 0 || pool.rateWakeAt != 0 {
		t.Fatalf("没有满窗账户后 rateFull=%d rateWakeAt=%d，期望都为空", len(pool.rateFull), pool.rateWakeAt)
	}
}

// TestRateWindowNoteClaimedByLogger “已满”日志的 10 分钟名额只由写日志的一侧占用：不写日志的租约（如转写经 AcquireFor 自取的租约）
// 占满窗口后释放，下一个占满窗口的租约仍可以写；写过之后 10 分钟内同一账户不再写；没有占满窗口的租约不写
func TestRateWindowNoteClaimedByLogger(t *testing.T) {
	const id = "a@example.com"
	pool := newRateTestPool(t, 10*time.Second, 1, 2, indexTestAccount(id, BenefitTierFree, indexTestCatalog()))
	silent, err := pool.AcquireFor(context.Background(), rateTestSelection)
	if err != nil {
		t.Fatal(err)
	}
	if !silent.rateFilled {
		t.Fatal("占满窗口的租约应标记 rateFilled")
	}
	if err := silent.Release(); err != nil {
		t.Fatal(err)
	}
	logged := mustTryAcquire(t, pool, rateTestSelection)
	if !logged.ClaimRateWindowNote() {
		t.Fatal("不写日志的租约占满过窗口后，下一个占满窗口的租约仍应写日志")
	}
	if logged.ClaimRateWindowNote() {
		t.Fatal("同一账户 10 分钟内只写一次")
	}
	if err := logged.Release(); err != nil {
		t.Fatal(err)
	}
	again := mustTryAcquire(t, pool, rateTestSelection)
	if !again.rateFilled || again.ClaimRateWindowNote() {
		t.Fatalf("10 分钟内再次占满：rateFilled=%t，期望占满但不再写日志", again.rateFilled)
	}
	if err := again.Release(); err != nil {
		t.Fatal(err)
	}
	pool.SetAccountRateLimit(3)
	roomy := mustTryAcquire(t, pool, rateTestSelection)
	if roomy.rateFilled || roomy.ClaimRateWindowNote() {
		t.Fatal("没有占满窗口的租约不写日志")
	}
	if err := roomy.Release(); err != nil {
		t.Fatal(err)
	}
	if (*AccountLease)(nil).ClaimRateWindowNote() {
		t.Fatal("空租约不写日志")
	}
}
