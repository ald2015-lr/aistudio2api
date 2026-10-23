package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/api"
)

// 出口判定器的单元测试：假时钟（直接驱动 observeOutcome 与 tick），假账户池（记录设下的限速，快照按测试设定），截获日志

// fakeEgressPool 为判定器测试的假账户池
type fakeEgressPool struct {
	mu     sync.Mutex
	limits map[egressLaneRef]int
	sets   int
	lanes  map[string][2]aistudio.EgressLaneStatus
	ladder map[string][]string
	relax  []string
	until  time.Time
	failed map[string]bool
}

func newFakeEgressPool() *fakeEgressPool {
	return &fakeEgressPool{
		limits: make(map[egressLaneRef]int), lanes: make(map[string][2]aistudio.EgressLaneStatus),
		ladder: make(map[string][]string), failed: make(map[string]bool),
	}
}

func (pool *fakeEgressPool) EgressSnapshot() []aistudio.EgressStatus {
	pool.mu.Lock()
	defer pool.mu.Unlock()
	result := make([]aistudio.EgressStatus, 0, len(pool.lanes))
	for key, lanes := range pool.lanes {
		status := aistudio.EgressStatus{Key: key, ID: aistudio.EgressID(key), Label: aistudio.EgressLabel(key), Lanes: lanes}
		for lane := range status.Lanes {
			status.Lanes[lane].Lane = aistudio.EgressLane(lane)
			status.Lanes[lane].Limit = pool.limits[egressLaneRef{key: key, lane: lane}]
		}
		result = append(result, status)
	}
	return result
}

func (pool *fakeEgressPool) EgressSnapshotFor(keys []string) ([]aistudio.EgressStatus, int) {
	return egressSnapshotFor(pool.EgressSnapshot(), keys)
}

// egressSnapshotFor 为假账户池的 EgressSnapshotFor：从全量快照里按 keys 的顺序挑出，总数为全量快照的条数
func egressSnapshotFor(all []aistudio.EgressStatus, keys []string) ([]aistudio.EgressStatus, int) {
	byKey := egressStatusIndex(all)
	var result []aistudio.EgressStatus
	for _, key := range keys {
		if status, ok := byKey[key]; ok {
			result = append(result, status)
		}
	}
	return result, len(all)
}

func (pool *fakeEgressPool) SetEgressRateLimit(key string, lane aistudio.EgressLane, limit int) {
	pool.mu.Lock()
	defer pool.mu.Unlock()
	pool.limits[egressLaneRef{key: key, lane: int(lane)}] = limit
	pool.sets++
}

func (pool *fakeEgressPool) RateLadderAccounts(key string, minLevel int) []string {
	pool.mu.Lock()
	defer pool.mu.Unlock()
	if minLevel != egressRelaxMinLevel {
		return nil
	}
	return append([]string(nil), pool.ladder[key]...)
}

func (pool *fakeEgressPool) RelaxRateLimitCooldowns(accountID string, until time.Time, minLevel int) (int, error) {
	pool.mu.Lock()
	defer pool.mu.Unlock()
	if pool.failed[accountID] {
		return 0, errors.New("模拟写盘失败")
	}
	pool.relax = append(pool.relax, accountID)
	pool.until = until
	return 1, nil
}

// limit 返回设到账户池的限速
func (pool *fakeEgressPool) limit(key string, lane int) int {
	pool.mu.Lock()
	defer pool.mu.Unlock()
	return pool.limits[egressLaneRef{key: key, lane: lane}]
}

// setLane 设置出口通道的账户池侧计数（发送数等）
func (pool *fakeEgressPool) setLane(key string, lane int, status aistudio.EgressLaneStatus) {
	pool.mu.Lock()
	defer pool.mu.Unlock()
	lanes := pool.lanes[key]
	lanes[lane] = status
	pool.lanes[key] = lanes
}

// guardHarness 为判定器测试的环境：假时钟从太平洋时间中午开始（离额度日零点远），10 秒桶对齐
type guardHarness struct {
	t     *testing.T
	guard *egressGuard
	pool  *fakeEgressPool
	mu    sync.Mutex
	now   time.Time
	logs  []egressLogEntry
}

var guardHarnessStart = time.Date(2026, 10, 20, 19, 0, 0, 0, time.UTC)

func newGuardHarness(t *testing.T) *guardHarness {
	t.Helper()
	return newGuardHarnessAt(t, guardHarnessStart)
}

func newGuardHarnessAt(t *testing.T, start time.Time) *guardHarness {
	t.Helper()
	harness := &guardHarness{t: t, pool: newFakeEgressPool(), now: start}
	harness.guard = &egressGuard{pool: harness.pool, now: harness.clock, logf: harness.capture}
	harness.pool.setLane("", egressLaneNormal, aistudio.EgressLaneStatus{Accounts: 100})
	t.Cleanup(harness.guard.relaxWG.Wait)
	return harness
}

func (harness *guardHarness) clock() time.Time {
	harness.mu.Lock()
	defer harness.mu.Unlock()
	return harness.now
}

func (harness *guardHarness) advance(d time.Duration) {
	harness.mu.Lock()
	harness.now = harness.now.Add(d)
	harness.mu.Unlock()
}

func (harness *guardHarness) capture(entry egressLogEntry) {
	harness.mu.Lock()
	harness.logs = append(harness.logs, entry)
	harness.mu.Unlock()
}

// observe 在直连出口的普通通道上记一次结果
func (harness *guardHarness) observe(accountID string, outcome egressOutcome) egressVerdict {
	return harness.observeOn("", accountID, false, outcome)
}

func (harness *guardHarness) observeOn(key string, accountID string, ultra bool, outcome egressOutcome) egressVerdict {
	return harness.guard.observeOutcome(egressObservation{Key: key, AccountID: accountID, Ultra: ultra, Outcome: outcome, At: harness.clock()})
}

func (harness *guardHarness) tick() {
	harness.guard.tick(harness.clock())
}

// logged 返回首段为 prefix、包含全部片段的日志
func (harness *guardHarness) logged(prefix string, fragments ...string) []egressLogEntry {
	harness.mu.Lock()
	defer harness.mu.Unlock()
	var result []egressLogEntry
	for _, entry := range harness.logs {
		if !strings.HasPrefix(entry.message, prefix) {
			continue
		}
		matched := true
		for _, fragment := range fragments {
			matched = matched && strings.Contains(entry.message, fragment)
		}
		if matched {
			result = append(result, entry)
		}
	}
	return result
}

// mustLogged 断言恰有 count 条匹配的日志
func (harness *guardHarness) mustLogged(count int, prefix string, fragments ...string) {
	harness.t.Helper()
	if got := harness.logged(prefix, fragments...); len(got) != count {
		harness.mu.Lock()
		all := make([]string, 0, len(harness.logs))
		for _, entry := range harness.logs {
			all = append(all, entry.message)
		}
		harness.mu.Unlock()
		harness.t.Fatalf("日志 %q（%v）有 %d 条，期望 %d 条；全部日志：\n%s", prefix, fragments, len(got), count, strings.Join(all, "\n"))
	}
}

func (harness *guardHarness) mode(key string) egressMode {
	harness.guard.mu.Lock()
	defer harness.guard.mu.Unlock()
	if state := harness.guard.states[key]; state != nil {
		return state.mode
	}
	return egressNormal
}

func (harness *guardHarness) lane(key string, lane int) egressLaneState {
	harness.guard.mu.Lock()
	defer harness.guard.mu.Unlock()
	if state := harness.guard.states[key]; state != nil {
		return state.lanes[lane]
	}
	return egressLaneState{}
}

func (harness *guardHarness) status(key string) egressStatusView {
	harness.t.Helper()
	for _, entry := range harness.guard.status() {
		if entry.ID == aistudio.EgressID(key) {
			return entry
		}
	}
	harness.t.Fatalf("状态里没有出口 %q", key)
	return egressStatusView{}
}

// enterWithFresh 让直连出口进入判定：先记 successes 次成功，再由冷号逐个裸 429 直到成立，返回用掉的冷号数
func (harness *guardHarness) enterWithFresh(successes int) int {
	harness.t.Helper()
	for index := range successes {
		harness.observe(fmt.Sprintf("ok-%d@example.com", index), egressSuccess)
	}
	for index := range 200 {
		if harness.observe(fmt.Sprintf("cold-%d@example.com", index), egressBare).Limited {
			return index + 1
		}
	}
	harness.t.Fatal("判定没有成立")
	return 0
}

func coldAccount(index int) string {
	return fmt.Sprintf("acct-%03d@example.com", index)
}

// TestEgressGuardEnterBranches 进入的两条分支各自成立：比例 ≥0.5 且冷号首发被拒 ≥3；比例 ≥0.8 且 ≥20 个账号。
// 各缺一个条件（比例、冷号、次数、账号数）都不成立
func TestEgressGuardEnterBranches(t *testing.T) {
	t.Run("冷号首发", func(t *testing.T) {
		harness := newGuardHarness(t)
		for index := range 9 {
			harness.observe(fmt.Sprintf("ok-%d@example.com", index), egressSuccess)
		}
		for index := range 9 {
			if harness.observe(coldAccount(index), egressBare).Limited {
				t.Fatalf("第 %d 次裸 429 就成立了（不足 10 次）", index+1)
			}
		}
		verdict := harness.observe(coldAccount(9), egressBare)
		if !verdict.Limited || verdict.Mode != egressLimited || verdict.Label != "直连" || !verdict.Throttled {
			t.Fatalf("10 次裸 429（比例 10/19、冷号 10 个）后判定 = %+v，期望成立", verdict)
		}
		harness.mustLogged(1, "出口限流判定成立 | 出口=直连 | 窗口=2m | 账号=10 | 裸限流=10 | 尝试=19 | 比例=53% | 冷号首发被拒=10 | 条件=冷号首发")
		if got := harness.guard.limitedKeys(); len(got) != 1 || !harness.guard.anyLimited() || !harness.guard.catalogPaused("") {
			t.Fatalf("判定成立的出口 = %v", got)
		}
	})
	t.Run("高比例", func(t *testing.T) {
		harness := newGuardHarness(t)
		for index := range 20 {
			harness.observe(coldAccount(index), egressSuccess)
		}
		// 先成功过的账号不是冷号；移出 2 分钟窗口
		harness.advance(3 * time.Minute)
		for index := range 4 {
			harness.observe(fmt.Sprintf("ok-%d@example.com", index), egressSuccess)
		}
		for index := range 19 {
			if harness.observe(coldAccount(index), egressBare).Limited {
				t.Fatalf("第 %d 个账号就成立了（不足 20 个账号）", index+1)
			}
		}
		if !harness.observe(coldAccount(19), egressBare).Limited {
			t.Fatal("20 个账号、比例 20/24 时应按高比例成立")
		}
		harness.mustLogged(1, "出口限流判定成立", "冷号首发被拒=0", "条件=高比例", "账号=20")
	})
	for _, tc := range []struct {
		name      string
		successes int
		bare      int
		accounts  int
		warm      bool
	}{
		{name: "比例不足", successes: 11, bare: 10, accounts: 10},
		{name: "没有冷号", successes: 9, bare: 10, accounts: 10, warm: true},
		{name: "次数不足", successes: 0, bare: 9, accounts: 9},
		{name: "账号不足", successes: 0, bare: 12, accounts: 4},
		{name: "高比例但账号不足 20", successes: 4, bare: 19, accounts: 19, warm: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			harness := newGuardHarness(t)
			if tc.warm {
				for index := range tc.accounts {
					harness.observe(coldAccount(index), egressSuccess)
				}
				harness.advance(3 * time.Minute)
			}
			for index := range tc.successes {
				harness.observe(fmt.Sprintf("ok-%d@example.com", index), egressSuccess)
			}
			for index := range tc.bare {
				if verdict := harness.observe(coldAccount(index%tc.accounts), egressBare); verdict.Limited {
					t.Fatalf("第 %d 次裸 429 后成立：%+v", index+1, verdict)
				}
			}
			if harness.mode("") != egressNormal || harness.guard.anyLimited() {
				t.Fatal("条件不足时不应成立")
			}
		})
	}
}

// TestEgressGuardEntersWithSuccesses 成功夹杂在裸 429 之间（比例约 85%）仍成立；原号池级判定要求零成功，同样的序列从不成立
func TestEgressGuardEntersWithSuccesses(t *testing.T) {
	harness := newGuardHarness(t)
	systemic := newRateLimitGuard()
	entered := false
	for index := range 20 {
		now := harness.clock()
		if index%7 == 3 {
			harness.observe(fmt.Sprintf("ok-%d@example.com", index), egressSuccess)
			systemic.succeeded(aistudio.PoolScopeNormal, testRuntimeModel, now)
		} else {
			entered = harness.observe(coldAccount(index), egressBare).Limited || entered
			if hit, _ := systemic.observe(aistudio.PoolScopeNormal, coldAccount(index), testRuntimeModel, now); hit {
				t.Fatal("原号池级判定在有成功时不应成立（对照）")
			}
		}
		harness.advance(time.Second)
	}
	if !entered || harness.mode("") != egressLimited {
		t.Fatal("成功夹杂、比例约 85% 时出口判定应成立")
	}
	if entry := harness.status(""); entry.Mode != "limited" || entry.RatioEntry < 0.5 || entry.Limiter != "active" {
		t.Fatalf("状态 = %+v", entry)
	}
}

// TestEgressGuardInitialLimitAndFloor 初值为普通通道最近 60 秒成功 ×1.25（至少 6），下限为初值的一半（至少 6），从空桶开始；
// Retry-After 按排队长度估计，限定在 5s–60s
func TestEgressGuardInitialLimitAndFloor(t *testing.T) {
	harness := newGuardHarness(t)
	harness.enterWithFresh(20)
	lane := harness.lane("", egressLaneNormal)
	if lane.initial != 25 || lane.limit != 25 || lane.floor != 13 || !lane.active {
		t.Fatalf("通道 = %+v，期望初值 25、下限 13", lane)
	}
	if got := harness.pool.limit("", egressLaneNormal); got != 25 {
		t.Fatalf("账户池普通通道限速 = %d", got)
	}
	if got := harness.pool.limit("", egressLaneUltra); got != 0 {
		t.Fatalf("Ultra 通道不应限速，实际 %d", got)
	}
	harness.mustLogged(1, "出口限流判定成立", "初始限速=25/分钟 | 下限=13/分钟")
	now := harness.clock()
	if verdict := harness.guard.verdict("", false, now); verdict.Limit != 25 || !verdict.RetryAt.Equal(now.Add(egressRetryMin)) {
		t.Fatalf("判定 = %+v，期望限速 25、建议 5s 后重试（2.4s 不足 5s）", verdict)
	}
	leaves := make([]func(), 0, 4)
	for range 4 {
		leaves = append(leaves, harness.guard.enterEgressWait(""))
	}
	// 排队 4 个：60s/25 × 5 = 12s
	if verdict := harness.guard.verdict("", false, now); !verdict.RetryAt.Equal(now.Add(12 * time.Second)) {
		t.Fatalf("排队 4 个时建议重试 = %s 后", verdict.RetryAt.Sub(now))
	}
	for _, leave := range leaves {
		leave()
		leave()
	}
	if entry := harness.status(""); entry.Queued != 0 {
		t.Fatalf("离开后排队数 = %d（重复调用离开函数只算一次）", entry.Queued)
	}

	empty := newGuardHarness(t)
	empty.enterWithFresh(0)
	if lane := empty.lane("", egressLaneNormal); lane.initial != egressMinLimit || lane.floor != egressMinLimit {
		t.Fatalf("没有成功时通道 = %+v，期望初值与下限都是 6", lane)
	}
}

// TestEgressGuardDecreaseQuiet 降速要距上次调整 30 秒（约等于在途请求的首事件时间）：启用后 10 秒不降，30 秒才降；降速后 10 秒不再降
func TestEgressGuardDecreaseQuiet(t *testing.T) {
	harness := newGuardHarness(t)
	harness.enterWithFresh(20)
	bare := func(count int) {
		for index := range count {
			harness.observe(fmt.Sprintf("later-%s-%d@example.com", harness.clock().Format("150405"), index), egressBare)
		}
	}
	harness.advance(10 * time.Second)
	bare(3)
	harness.tick()
	if limit := harness.lane("", egressLaneNormal).limit; limit != 25 {
		t.Fatalf("启用后 10 秒降速到 %d，期望不降", limit)
	}
	harness.advance(20 * time.Second)
	bare(3)
	harness.tick()
	if limit := harness.lane("", egressLaneNormal).limit; limit != 17 {
		t.Fatalf("启用后 30 秒限速 = %d，期望 floor(25×0.7) = 17", limit)
	}
	harness.mustLogged(1, "出口限速调整 | 出口=直连 | 通道=普通 | 限速=25→17/分钟 | 原因=降速 | 30秒裸限流=")
	if got := harness.pool.limit("", egressLaneNormal); got != 17 {
		t.Fatalf("账户池限速 = %d", got)
	}
	harness.advance(10 * time.Second)
	bare(3)
	harness.tick()
	if limit := harness.lane("", egressLaneNormal).limit; limit != 17 {
		t.Fatalf("降速后 10 秒又降到 %d", limit)
	}
	harness.advance(20 * time.Second)
	bare(3)
	harness.tick()
	if limit := harness.lane("", egressLaneNormal).limit; limit != 13 {
		t.Fatalf("再过 30 秒限速 = %d，期望降到下限 13", limit)
	}
	harness.advance(30 * time.Second)
	bare(3)
	harness.tick()
	if limit := harness.lane("", egressLaneNormal).limit; limit != 13 {
		t.Fatalf("已在下限时限速 = %d", limit)
	}
}

// TestEgressGuardDecreaseRevertsOnSuccessDrop 降速满 60 秒后成功数比降速前少 20% 以上就回到原值，并锁定 10 分钟不再降速；
// 成功数不变时保留降速
func TestEgressGuardDecreaseRevertsOnSuccessDrop(t *testing.T) {
	setup := func(t *testing.T) *guardHarness {
		harness := newGuardHarness(t)
		harness.enterWithFresh(20)
		harness.advance(25 * time.Second)
		for index := range 3 {
			harness.observe(fmt.Sprintf("bare-%d@example.com", index), egressBare)
		}
		harness.advance(5 * time.Second)
		harness.tick()
		if limit := harness.lane("", egressLaneNormal).limit; limit != 17 {
			t.Fatalf("降速后限速 = %d", limit)
		}
		return harness
	}
	t.Run("成功下降则回退", func(t *testing.T) {
		harness := setup(t)
		harness.advance(30 * time.Second)
		for index := range 10 {
			harness.observe(fmt.Sprintf("after-%d@example.com", index), egressSuccess)
		}
		harness.advance(30 * time.Second)
		harness.tick()
		if limit := harness.lane("", egressLaneNormal).limit; limit != 25 {
			t.Fatalf("成功从 20 降到 10 后限速 = %d，期望回到 25", limit)
		}
		harness.mustLogged(1, "出口限速调整", "限速=17→25/分钟 | 原因=降速回退")
		// 锁定期内裸 429 再多也不降速
		harness.advance(35 * time.Second)
		for index := range 5 {
			harness.observe(fmt.Sprintf("locked-%d@example.com", index), egressBare)
		}
		harness.tick()
		if limit := harness.lane("", egressLaneNormal).limit; limit != 25 {
			t.Fatalf("降速锁定期内限速 = %d", limit)
		}
		harness.advance(egressDecreaseLock)
		for index := range 5 {
			harness.observe(fmt.Sprintf("unlocked-%d@example.com", index), egressBare)
		}
		harness.tick()
		if limit := harness.lane("", egressLaneNormal).limit; limit >= 25 {
			t.Fatalf("锁定期过后限速 = %d，期望可以再降", limit)
		}
	})
	t.Run("成功不变则保留", func(t *testing.T) {
		harness := setup(t)
		harness.advance(30 * time.Second)
		for index := range 20 {
			harness.observe(fmt.Sprintf("after-%d@example.com", index), egressSuccess)
		}
		harness.advance(30 * time.Second)
		harness.tick()
		if limit := harness.lane("", egressLaneNormal).limit; limit != 17 {
			t.Fatalf("成功没有下降时限速 = %d，期望保留 17", limit)
		}
		harness.mustLogged(0, "出口限速调整", "原因=降速回退")
	})
}

// TestEgressGuardIncreaseNeedsSends 30 秒内实际发送不足 max(3, 限速/4) 时不提速：没有发送就没有证据
func TestEgressGuardIncreaseNeedsSends(t *testing.T) {
	harness := newGuardHarness(t)
	harness.enterWithFresh(20)
	leave := harness.guard.enterEgressWait("")
	defer leave()
	harness.advance(time.Minute)
	harness.pool.setLane("", egressLaneNormal, aistudio.EgressLaneStatus{Accounts: 100, Sent1m: 25, Sent30s: 5})
	harness.tick()
	if limit := harness.lane("", egressLaneNormal).limit; limit != 25 {
		t.Fatalf("30 秒只发送 5 次（< 25/4）时提速到 %d", limit)
	}
	harness.advance(15 * time.Second)
	harness.pool.setLane("", egressLaneNormal, aistudio.EgressLaneStatus{Accounts: 100, Sent1m: 25, Sent30s: 6})
	harness.tick()
	if limit := harness.lane("", egressLaneNormal).limit; limit != 38 {
		t.Fatalf("有发送、没有裸 429、限速在起作用时限速 = %d，期望 ceil(25×1.5) = 38", limit)
	}
	harness.mustLogged(1, "出口限速调整", "限速=25→38/分钟 | 原因=快速提速 | 30秒裸限流=0", "30秒发送=6", "排队30秒=")
}

// TestEgressGuardIncreaseOnlyWhenBinding 限速不起作用（发送不到限速的 80%，最近 30 秒也没有出口限速排队）时不提速
func TestEgressGuardIncreaseOnlyWhenBinding(t *testing.T) {
	harness := newGuardHarness(t)
	harness.enterWithFresh(20)
	harness.advance(time.Minute)
	harness.pool.setLane("", egressLaneNormal, aistudio.EgressLaneStatus{Accounts: 100, Sent1m: 19, Sent30s: 10})
	harness.tick()
	if limit := harness.lane("", egressLaneNormal).limit; limit != 25 {
		t.Fatalf("发送 19 < 25×0.8 时提速到 %d", limit)
	}
	harness.advance(15 * time.Second)
	harness.pool.setLane("", egressLaneNormal, aistudio.EgressLaneStatus{Accounts: 100, Sent1m: 20, Sent30s: 10})
	harness.tick()
	if limit := harness.lane("", egressLaneNormal).limit; limit != 38 {
		t.Fatalf("发送达到限速的 80%% 时限速 = %d，期望 38", limit)
	}
}

// TestEgressGuardFastRecovery 没有裸 429、限速在起作用、有发送时每 15 秒 ×1.5，6 步约 11 倍；比例 <0.1 时慢提速 +max(1, 10%)
func TestEgressGuardFastRecovery(t *testing.T) {
	harness := newGuardHarness(t)
	harness.enterWithFresh(0)
	harness.advance(time.Minute)
	want := []int{9, 14, 21, 32, 48, 72}
	for step, expected := range want {
		limit := harness.lane("", egressLaneNormal).limit
		harness.pool.setLane("", egressLaneNormal, aistudio.EgressLaneStatus{Accounts: 100, Sent1m: limit, Sent30s: limit})
		harness.tick()
		if got := harness.lane("", egressLaneNormal).limit; got != expected {
			t.Fatalf("第 %d 步限速 = %d，期望 %d", step+1, got, expected)
		}
		harness.advance(egressIncreaseEvery)
	}
	if harness.mode("") != egressLimited {
		t.Fatal("提速期间不应解除（解除另有条件）")
	}

	slow := newGuardHarness(t)
	slow.enterWithFresh(20)
	slow.advance(time.Minute)
	for index := range 20 {
		slow.observe(fmt.Sprintf("ok-late-%d@example.com", index), egressSuccess)
	}
	slow.observe("one-bare@example.com", egressBare)
	slow.pool.setLane("", egressLaneNormal, aistudio.EgressLaneStatus{Accounts: 100, Sent1m: 25, Sent30s: 21})
	slow.tick()
	if limit := slow.lane("", egressLaneNormal).limit; limit != 28 {
		t.Fatalf("比例 1/21 时限速 = %d，期望 25 + ceil(2.5) = 28", limit)
	}
	slow.mustLogged(1, "出口限速调整", "限速=25→28/分钟 | 原因=提速")
}

// TestEgressGuardExitWithoutMultiple 初值 6、需求持续大于能力（一直在排队、比例约 0.2）时不解除也不反复进出；需求降到限速以下、
// 限速连续 2 分钟不起作用且比例 <5% 时解除，两条通道限速归零
func TestEgressGuardExitWithoutMultiple(t *testing.T) {
	harness := newGuardHarness(t)
	harness.enterWithFresh(0)
	leave := harness.guard.enterEgressWait("")
	serial := 0
	for range 60 {
		harness.advance(10 * time.Second)
		for range 8 {
			harness.observe("ok@example.com", egressSuccess)
		}
		for range 2 {
			serial++
			harness.observe(fmt.Sprintf("busy-%d@example.com", serial), egressBare)
		}
		harness.pool.setLane("", egressLaneNormal, aistudio.EgressLaneStatus{Accounts: 100, Sent1m: 6, Sent30s: 3})
		harness.tick()
		if harness.mode("") != egressLimited {
			t.Fatalf("需求大于能力时第 %s 解除了", harness.clock().Sub(guardHarnessStart))
		}
	}
	harness.mustLogged(1, "出口限流判定成立")
	harness.mustLogged(0, "出口限流解除")
	// 需求降下来：不再排队，发送不到限速的 80%，只有成功
	leave()
	exitedAt := time.Duration(0)
	for range 60 {
		harness.advance(5 * time.Second)
		harness.observe("ok@example.com", egressSuccess)
		harness.pool.setLane("", egressLaneNormal, aistudio.EgressLaneStatus{Accounts: 100, Sent1m: 2, Sent30s: 1})
		harness.tick()
		if harness.mode("") == egressNormal {
			exitedAt = harness.clock().Sub(guardHarnessStart) - 10*time.Minute
			break
		}
	}
	if exitedAt == 0 || exitedAt > 3*time.Minute {
		t.Fatalf("需求降下来后 %s 才解除（0 为没有解除），期望 2–3 分钟内", exitedAt)
	}
	harness.mustLogged(1, "出口限流解除 | 出口=直连 | 持续=", "最终限速=6/分钟")
	if got := harness.pool.limit("", egressLaneNormal); got != 0 {
		t.Fatalf("解除后账户池限速 = %d", got)
	}
	if harness.guard.anyLimited() {
		t.Fatal("解除后不应还有判定成立的出口")
	}
	for range 5 {
		harness.advance(5 * time.Second)
		harness.observe("ok@example.com", egressSuccess)
		harness.tick()
	}
	harness.mustLogged(1, "出口限流判定成立")
}

// TestEgressGuardSuspendWhenIneffective 限速 3 分钟、最近 2 分钟一直在起作用，比例仍没有比进入时降 0.2 时停止限速：判定保持，
// verdict.Suspended、不再限速；比例降了 0.2 以上时不停止
func TestEgressGuardSuspendWhenIneffective(t *testing.T) {
	run := func(t *testing.T, success, bare int) *guardHarness {
		harness := newGuardHarness(t)
		harness.observe("ok@example.com", egressSuccess)
		harness.observe("ok@example.com", egressSuccess)
		harness.enterWithFresh(0)
		leave := harness.guard.enterEgressWait("")
		t.Cleanup(leave)
		serial := 0
		for range 30 {
			harness.advance(10 * time.Second)
			for range success {
				harness.observe("ok@example.com", egressSuccess)
			}
			for range bare {
				serial++
				harness.observe(fmt.Sprintf("bare-%d@example.com", serial), egressBare)
			}
			harness.tick()
		}
		return harness
	}
	t.Run("比例不改善", func(t *testing.T) {
		harness := run(t, 1, 6)
		harness.mustLogged(1, "出口限速无效，停止限速 | 出口=直连 | 通道=普通 | 进入时比例=83% | 限速后比例=86%", "限流类换号放宽到 4 次")
		verdict := harness.guard.verdict("", false, harness.clock())
		if !verdict.Limited || !verdict.Suspended || verdict.Throttled || verdict.Limit != 0 {
			t.Fatalf("停止限速后判定 = %+v", verdict)
		}
		if got := harness.pool.limit("", egressLaneNormal); got != 0 {
			t.Fatalf("账户池限速 = %d", got)
		}
		if entry := harness.status(""); entry.Mode != "limited" || entry.Limiter != "suspended" {
			t.Fatalf("状态 = %+v", entry)
		}
	})
	t.Run("比例改善", func(t *testing.T) {
		harness := run(t, 6, 4)
		harness.mustLogged(0, "出口限速无效")
		if verdict := harness.guard.verdict("", false, harness.clock()); verdict.Suspended || !verdict.Throttled {
			t.Fatalf("比例从 83%% 降到 40%% 时判定 = %+v，期望照常限速", verdict)
		}
	})
}

// TestEgressGuardFloorWarnsOnly 限速停在下限 10 分钟、比例仍 ≥0.9 时只写“出口几乎完全被拒”（10 分钟一条），判定保持，不撤销
func TestEgressGuardFloorWarnsOnly(t *testing.T) {
	harness := newGuardHarness(t)
	harness.enterWithFresh(0)
	leave := harness.guard.enterEgressWait("")
	defer leave()
	serial := 0
	for step := range 90 {
		harness.advance(10 * time.Second)
		if step%6 == 0 {
			harness.observe("ok@example.com", egressSuccess)
		}
		for range 10 {
			serial++
			harness.observe(fmt.Sprintf("bare-%d@example.com", serial), egressBare)
		}
		harness.tick()
	}
	harness.mustLogged(1, "出口几乎完全被拒 | 出口=直连 | 限速=6/分钟（下限） | 10分钟比例=", "判定保持，不交回逐级冷却")
	harness.mustLogged(0, "出口限速无效")
	harness.mustLogged(0, "出口限流判定撤销")
	if harness.mode("") != egressLimited {
		t.Fatal("几乎完全被拒时判定应保持")
	}
}

// TestEgressGuardValveRetriedZeroSuccess 判定满 10 分钟、≥30 个复试账号零成功、出口 10 分钟内仍有 ≥10 次成功时撤销 30 分钟；
// 复试账号不足 30、有一个恢复或出口零成功时不撤销
func TestEgressGuardValveRetriedZeroSuccess(t *testing.T) {
	run := func(t *testing.T, retried int, recover bool, successes int) *guardHarness {
		harness := newGuardHarness(t)
		harness.enterWithFresh(0)
		harness.advance(time.Minute)
		for index := range retried {
			harness.observe(fmt.Sprintf("retry-%d@example.com", index), egressBare)
		}
		harness.advance(time.Minute)
		for index := range retried {
			harness.observe(fmt.Sprintf("retry-%d@example.com", index), egressBare)
		}
		harness.advance(3 * time.Minute)
		for index := range successes {
			harness.observe(fmt.Sprintf("healthy-%d@example.com", index), egressSuccess)
		}
		if recover {
			harness.observe("retry-0@example.com", egressSuccess)
		}
		// 最近 2 分钟仍有裸 429（其他账号），不满足解除条件
		harness.advance(4 * time.Minute)
		for index := range 5 {
			harness.observe(fmt.Sprintf("tail-%d@example.com", index), egressBare)
		}
		harness.advance(time.Minute + 5*time.Second)
		harness.tick()
		return harness
	}
	harness := run(t, 30, false, 10)
	if harness.mode("") != egressRejected {
		t.Fatalf("30 个复试账号零成功时模式 = %v，期望撤销", harness.mode(""))
	}
	harness.mustLogged(1, "出口限流判定撤销 | 出口=直连 | 原因=复试账号零成功 | 持续=", "30分钟内不再判定")
	if entry := harness.status(""); entry.Mode != "rejected" || entry.RejectedBy != "安全阀" || entry.RejectedUntil.IsZero() {
		t.Fatalf("状态 = %+v", entry)
	}
	if got := harness.pool.limit("", egressLaneNormal); got != 0 {
		t.Fatalf("撤销后账户池限速 = %d", got)
	}
	for _, tc := range []struct {
		name      string
		retried   int
		recover   bool
		successes int
	}{
		{name: "复试账号不足", retried: 29, successes: 10},
		{name: "有一个恢复", retried: 30, recover: true, successes: 10},
		{name: "出口零成功", retried: 30, successes: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			harness := run(t, tc.retried, tc.recover, tc.successes)
			if harness.mode("") != egressLimited {
				t.Fatalf("模式 = %v，期望保持判定", harness.mode(""))
			}
		})
	}
}

// TestEgressGuardDemoteDynamic 比例 r 约 0.2 时连续 3 次裸 429 即按账号降级（k(r) = ceil(ln0.01/ln r)）；r 约 0.7 时不按动态门槛降级
func TestEgressGuardDemoteDynamic(t *testing.T) {
	harness := newGuardHarness(t)
	harness.enterWithFresh(0)
	harness.advance(egressJudgeWindow + 10*time.Second)
	for index := range 40 {
		harness.observe(fmt.Sprintf("ok-%d@example.com", index), egressSuccess)
	}
	for index := range 7 {
		harness.observe(fmt.Sprintf("other-%d@example.com", index), egressBare)
	}
	for attempt := 1; attempt <= 2; attempt++ {
		if verdict := harness.observe("victim@example.com", egressBare); verdict.Demote {
			t.Fatalf("第 %d 次就降级了", attempt)
		}
	}
	verdict := harness.observe("victim@example.com", egressBare)
	if !verdict.Demote || verdict.DemoteStreak != 3 || !verdict.Limited {
		t.Fatalf("r≈0.2 时第 3 次判定 = %+v，期望降级、连续 3 次", verdict)
	}
	if entry := harness.status(""); entry.Demoted != 1 {
		t.Fatalf("降级账号数 = %d", entry.Demoted)
	}
	harness.tick()
	harness.mustLogged(1, "出口限流：账号按账号级失败冷却 | 出口=直连 | 新增=1 | 合计=1 | 门槛=3次 | 兜底=0")

	high := newGuardHarness(t)
	high.enterWithFresh(3)
	for attempt := 1; attempt <= 8; attempt++ {
		if verdict := high.observe("victim@example.com", egressBare); verdict.Demote {
			t.Fatalf("r≈0.7 以上时第 %d 次降级了（个别账号无法与出口区分）", attempt)
		}
	}
	for _, tc := range []struct {
		ratio float64
		want  int
		ok    bool
	}{
		{ratio: 0.1, want: 3, ok: true}, {ratio: 0.2, want: 3, ok: true}, {ratio: 0.5, want: 7, ok: true},
		{ratio: 0.56, want: 8, ok: true}, {ratio: 0.6, ok: false}, {ratio: 0.85, ok: false},
	} {
		if got, ok := egressDemoteThreshold(tc.ratio); ok != tc.ok || ok && got != tc.want {
			t.Fatalf("k(%.2f) = %d, %v，期望 %d, %v", tc.ratio, got, ok, tc.want, tc.ok)
		}
	}
}

// TestEgressGuardDemoteBackstop 本次判定期间零成功、连续 6 次、跨 10 分钟的账号不论比例都降级；判定期间成功过的不按兜底降级
func TestEgressGuardDemoteBackstop(t *testing.T) {
	harness := newGuardHarness(t)
	harness.enterWithFresh(0)
	for attempt := 1; attempt <= 5; attempt++ {
		harness.advance(2 * time.Minute)
		if verdict := harness.observe("spent@example.com", egressBare); verdict.Demote {
			t.Fatalf("第 %d 次就降级了", attempt)
		}
	}
	harness.advance(2 * time.Minute)
	verdict := harness.observe("spent@example.com", egressBare)
	if !verdict.Demote || verdict.DemoteStreak != 6 {
		t.Fatalf("零成功、连续 6 次、跨 10 分钟时判定 = %+v，期望降级", verdict)
	}
	harness.tick()
	harness.mustLogged(1, "出口限流：账号按账号级失败冷却", "新增=1 | 合计=1 | 门槛=无 | 兜底=1")

	succeeded := newGuardHarness(t)
	succeeded.enterWithFresh(0)
	succeeded.observe("mixed@example.com", egressSuccess)
	for attempt := 1; attempt <= 6; attempt++ {
		succeeded.advance(2 * time.Minute)
		if verdict := succeeded.observe("mixed@example.com", egressBare); verdict.Demote {
			t.Fatalf("判定期间成功过的账号第 %d 次按兜底降级了", attempt)
		}
	}
}

// TestEgressGuardDemotedExcludedFromRatio 降级账号此后的结果不计入出口统计（含比例），也不再返回降级
func TestEgressGuardDemotedExcludedFromRatio(t *testing.T) {
	harness := newGuardHarness(t)
	harness.enterWithFresh(0)
	harness.advance(egressJudgeWindow + 10*time.Second)
	for index := range 40 {
		harness.observe(fmt.Sprintf("ok-%d@example.com", index), egressSuccess)
	}
	for index := range 7 {
		harness.observe(fmt.Sprintf("other-%d@example.com", index), egressBare)
	}
	for range 3 {
		harness.observe("victim@example.com", egressBare)
	}
	before := harness.status("")
	for range 5 {
		if verdict := harness.observe("victim@example.com", egressBare); verdict.Demote {
			t.Fatal("已降级的账号不应再返回降级")
		}
	}
	after := harness.status("")
	if after.Bare1m != before.Bare1m || after.Ratio2m != before.Ratio2m || after.Demoted != 1 {
		t.Fatalf("降级后状态 %+v → %+v，期望降级账号的结果不计入", before, after)
	}
}

// TestEgressGuardUltraLaneActivation Ultra 通道只在判定期间 Ultra 账户自己有 ≥3 次裸 429（比例 ≥0.5）时才启用限速，按 Ultra 自己的成功数
func TestEgressGuardUltraLaneActivation(t *testing.T) {
	harness := newGuardHarness(t)
	harness.enterWithFresh(0)
	if got := harness.pool.limit("", egressLaneUltra); got != 0 {
		t.Fatalf("进入时 Ultra 通道限速 = %d", got)
	}
	for index := range 2 {
		harness.observeOn("", fmt.Sprintf("ultra-%d@example.com", index), true, egressBare)
	}
	if harness.lane("", egressLaneUltra).active || harness.guard.verdict("", true, harness.clock()).Throttled {
		t.Fatal("Ultra 裸 429 不足 3 次时不应限速")
	}
	verdict := harness.observeOn("", "ultra-2@example.com", true, egressBare)
	if !verdict.Throttled || verdict.Limit != egressMinLimit {
		t.Fatalf("Ultra 第 3 次裸 429 后判定 = %+v，期望 Ultra 通道限速 6", verdict)
	}
	if got := harness.pool.limit("", egressLaneUltra); got != egressMinLimit {
		t.Fatalf("账户池 Ultra 通道限速 = %d", got)
	}
	harness.mustLogged(1, "出口限速调整 | 出口=直连 | 通道=Ultra | 限速=0→6/分钟 | 原因=启用")
	if normal := harness.guard.verdict("", false, harness.clock()); normal.Limit != egressMinLimit || !normal.Throttled {
		t.Fatalf("普通通道判定 = %+v", normal)
	}
}

// TestEgressGuardAdminWithdrawResume withdraw 立即撤销 30 分钟、两条通道限速归零，期间不再进入；resume 回到 normal，可以重新进入；
// 撤销到期自动恢复；未知编号 404
func TestEgressGuardAdminWithdrawResume(t *testing.T) {
	harness := newGuardHarness(t)
	harness.enterWithFresh(0)
	id := aistudio.EgressID("")
	if err := harness.guard.withdraw(id, harness.clock()); err != nil {
		t.Fatal(err)
	}
	if harness.mode("") != egressRejected || harness.pool.limit("", egressLaneNormal) != 0 || harness.guard.anyLimited() {
		t.Fatalf("撤销后模式 = %v、限速 = %d", harness.mode(""), harness.pool.limit("", egressLaneNormal))
	}
	harness.mustLogged(1, "出口限流判定撤销 | 出口=直连 | 原因=管理操作 | 持续=0s | 30分钟内不再判定")
	entry := harness.status("")
	if entry.RejectedBy != "管理操作" || !entry.RejectedUntil.Equal(harness.clock().Add(egressRejectFor)) {
		t.Fatalf("状态 = %+v", entry)
	}
	for index := range 30 {
		if harness.observe(fmt.Sprintf("again-%d@example.com", index), egressBare).Limited {
			t.Fatal("撤销期间不应再进入")
		}
	}
	if err := harness.guard.resume(id, harness.clock()); err != nil {
		t.Fatal(err)
	}
	harness.mustLogged(1, "出口限流判定恢复 | 出口=直连 | 原因=管理操作")
	if harness.mode("") != egressNormal {
		t.Fatalf("恢复后模式 = %v", harness.mode(""))
	}
	harness.advance(egressJudgeWindow + 10*time.Second)
	harness.enterWithFresh(0)
	if !harness.guard.anyLimited() {
		t.Fatal("恢复后应能重新进入")
	}

	if err := harness.guard.withdraw(id, harness.clock()); err != nil {
		t.Fatal(err)
	}
	harness.advance(egressRejectFor)
	harness.tick()
	if harness.mode("") != egressNormal {
		t.Fatalf("撤销 30 分钟到期后模式 = %v", harness.mode(""))
	}
	harness.mustLogged(1, "出口限流判定恢复 | 出口=直连 | 原因=撤销到期")

	var notFound interface{ HTTPStatus() int }
	if err := harness.guard.withdraw("000000000000", harness.clock()); !errors.As(err, &notFound) || notFound.HTTPStatus() != 404 {
		t.Fatalf("未知编号 withdraw = %v", err)
	}
	// 账户池里有、判定器还没见过的出口同样可以撤销
	proxy := "http://gw.example:8080"
	harness.pool.setLane(proxy, egressLaneNormal, aistudio.EgressLaneStatus{Accounts: 3})
	if err := harness.guard.withdraw(aistudio.EgressID(proxy), harness.clock()); err != nil || harness.mode(proxy) != egressRejected {
		t.Fatalf("撤销账户池里的出口：err=%v 模式=%v", err, harness.mode(proxy))
	}
}

// TestEgressGuardIndependentTicker 判定器用自己的计时器（run），不依赖 keepWarmPass：Worker 管理器的预热循环卡住时计时照常，
// 撤销到期照样恢复；ctx 结束时 run 返回
func TestEgressGuardIndependentTicker(t *testing.T) {
	harness := newGuardHarness(t)
	harness.guard.tickEvery = 5 * time.Millisecond
	harness.enterWithFresh(0)
	if err := harness.guard.withdraw(aistudio.EgressID(""), harness.clock()); err != nil {
		t.Fatal(err)
	}
	harness.advance(egressRejectFor)
	// 模拟 keepWarmPass 卡住（清扫死槽位同步等浏览器关闭）：持有预热循环的锁
	manager := &accountWorkerManager{}
	manager.fillMu.Lock()
	defer manager.fillMu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		harness.guard.run(ctx)
		close(done)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for harness.guard.ticks.Load() < 3 || harness.mode("") != egressNormal {
		if time.Now().After(deadline) {
			t.Fatalf("计时 %d 次、模式 %v，期望独立计时器照常推进", harness.guard.ticks.Load(), harness.mode(""))
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("ctx 结束后 run 没有返回")
	}
}

// TestEgressGuardIndependentEgresses 各出口独立判定、限速、暂停目录与冷启动闸门
func TestEgressGuardIndependentEgresses(t *testing.T) {
	harness := newGuardHarness(t)
	proxy := aistudio.EgressKey("socks5h://User:Pass@GW.example:1080")
	for index := range 5 {
		harness.observeOn(proxy, fmt.Sprintf("proxy-%d@example.com", index), false, egressSuccess)
	}
	harness.enterWithFresh(0)
	limited := harness.guard.limitedKeys()
	if _, ok := limited[""]; !ok || len(limited) != 1 {
		t.Fatalf("判定成立的出口 = %v，期望只有直连", limited)
	}
	if harness.guard.catalogPaused(proxy) || !harness.guard.catalogPaused("") {
		t.Fatal("目录暂停应只作用于判定成立的出口")
	}
	if harness.pool.limit(proxy, egressLaneNormal) != 0 || harness.pool.limit("", egressLaneNormal) == 0 {
		t.Fatal("限速应只设在判定成立的出口上")
	}
	if _, ok := harness.guard.admitColdStart(proxy, false, testRuntimeModel, coldStartReplace, 2); !ok {
		t.Fatal("未受限出口上的冷启动应照常放行")
	}
	if _, ok := harness.guard.admitColdStart("", false, testRuntimeModel, coldStartReplace, 2); ok {
		t.Fatal("受限出口上的替换应暂停")
	}
	statuses := harness.guard.status()
	if len(statuses) != 2 || statuses[0].Mode != "limited" || statuses[0].Label != "直连" ||
		statuses[1].Mode != "normal" || !strings.HasPrefix(statuses[1].Label, "socks5://gw.example:1080#") {
		t.Fatalf("状态 = %+v，期望受限的直连排在前面、代理标签脱敏", statuses)
	}
	for _, entry := range statuses {
		if strings.Contains(entry.Label, "User") || strings.Contains(entry.Label, "Pass") {
			t.Fatalf("标签泄露了凭据：%s", entry.Label)
		}
	}
}

// TestEgressGuardNilSafe 判定器为 nil 或零值时各方法不 panic，判定关闭
func TestEgressGuardNilSafe(t *testing.T) {
	var guard *egressGuard
	now := time.Now()
	guard.tick(now)
	if guard.limitedCooldown() != egressLimitedCooldown || guard.status() != nil || guard.anyLimited() {
		t.Fatal("nil 判定器不应有状态")
	}
	if err := guard.withdraw("x", now); err == nil {
		t.Fatal("nil 判定器 withdraw 应报错")
	}
	if err := guard.resume("x", now); err == nil {
		t.Fatal("nil 判定器 resume 应报错")
	}
	zero := &egressGuard{}
	if verdict := zero.observeOutcome(egressObservation{AccountID: "a", Outcome: egressBare, At: now}); verdict.Limited {
		t.Fatal("零值判定器一次裸 429 不应成立")
	}
	zero.observeOutcome(egressObservation{Outcome: egressSuccess})
	zero.tick(now)
	zero.noteLocalReject("", time.Time{})
	zero.enterEgressWait("")()
	if release, ok := zero.admitColdStart("", false, "m", coldStartRefill, 0); !ok {
		t.Fatal("零值判定器应放行冷启动")
	} else {
		release(true)
	}
	if statuses := zero.status(); len(statuses) != 1 || statuses[0].Mode != "normal" || statuses[0].LocalRejects1m != 1 {
		t.Fatalf("零值判定器状态 = %+v", statuses)
	}
	zero.syncLimits()
}

// TestEgressGuardFreshPerQuotaDay 冷号按太平洋时间额度日计：当天第一次结果是裸 429 才算冷号首发，次日零点后同一账号重新算冷号
func TestEgressGuardFreshPerQuotaDay(t *testing.T) {
	location, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Fatal(err)
	}
	harness := newGuardHarnessAt(t, time.Date(2026, 10, 20, 23, 58, 0, 0, location))
	harness.observe("a@example.com", egressBare)
	harness.observe("a@example.com", egressBare)
	harness.observe("b@example.com", egressSuccess)
	harness.observe("b@example.com", egressBare)
	if fresh := harness.status("").FreshRejects2m; fresh != 1 {
		t.Fatalf("同一额度日内冷号首发 = %d，期望只有 a 的第一次", fresh)
	}
	harness.advance(egressJudgeWindow + 10*time.Second)
	harness.tick()
	harness.observe("a@example.com", egressBare)
	harness.observe("b@example.com", egressBare)
	if fresh := harness.status("").FreshRejects2m; fresh != 2 {
		t.Fatalf("次日零点后冷号首发 = %d，期望 a、b 都重新算冷号", fresh)
	}
}

// TestEgressColdStartPaths 冷启动闸门：补位只受并发约束、不计每分钟配额；补覆盖与补位共用并发、每分钟 ≤6 且同模型一个在途；
// 替换与超出常驻暂停；配额按（出口，通道）分开；没有判定的出口不计数。暂停的路径同出口同路径 10 分钟写一条
func TestEgressColdStartPaths(t *testing.T) {
	harness := newGuardHarness(t)
	harness.enterWithFresh(0)
	guard := harness.guard
	admit := func(path coldStartPath, model string, ultra bool) (func(bool), bool) {
		return guard.admitColdStart("", ultra, model, path, 2)
	}
	first, ok1 := admit(coldStartRefill, "", false)
	second, ok2 := admit(coldStartRefill, "", false)
	if !ok1 || !ok2 {
		t.Fatal("并发 2 以内的补位应放行")
	}
	if _, ok := admit(coldStartRefill, "", false); ok {
		t.Fatal("补位并发已满时应推迟")
	}
	if _, ok := admit(coldStartRefill, "", false); ok {
		t.Fatal("补位并发已满时应推迟")
	}
	harness.mustLogged(1, "出口限流暂停冷启动 | 出口=直连 | 通道=普通 | 路径=补位并发已满 | 10分钟内跳过=1")
	lane := harness.status("").Lanes[0]
	if lane.ColdStarting != 2 || lane.RefillDeferred1m != 2 || lane.ColdStarted1m != 2 {
		t.Fatalf("普通通道冷启动计数 = %+v", lane)
	}
	if release, ok := admit(coldStartRefill, "", true); !ok {
		t.Fatal("Ultra 通道的补位不受普通通道并发影响")
	} else {
		release(true)
	}
	first(true)
	first(true)
	if release, ok := admit(coldStartRefill, "", false); !ok {
		t.Fatal("退还一个名额后补位应放行（重复退还只算一次）")
	} else {
		release(false)
	}
	second(true)
	for index := range 20 {
		release, ok := admit(coldStartRefill, "", false)
		if !ok {
			t.Fatalf("第 %d 个依次完成的补位被拦下：补位不计每分钟配额", index+1)
		}
		release(true)
	}
	harness.advance(time.Minute + time.Second)
	coverageX, ok := admit(coldStartCoverage, "model-x", false)
	if !ok {
		t.Fatal("补覆盖应放行")
	}
	if _, ok := admit(coldStartCoverage, "model-x", false); ok {
		t.Fatal("同模型已在补覆盖时不应再启动")
	}
	harness.mustLogged(1, "出口限流暂停冷启动", "路径=补覆盖配额已满")
	coverageY, ok := admit(coldStartCoverage, "model-y", false)
	if !ok {
		t.Fatal("另一个模型的补覆盖应放行")
	}
	if _, ok := admit(coldStartCoverage, "model-z", false); ok {
		t.Fatal("补覆盖与补位共用并发，已满时应暂停")
	}
	coverageX(true)
	coverageY(true)
	for index := 2; index < egressCoveragePerMinute; index++ {
		release, ok := admit(coldStartCoverage, fmt.Sprintf("model-%d", index), false)
		if !ok {
			t.Fatalf("一分钟内第 %d 个补覆盖被拦下", index+1)
		}
		release(true)
	}
	if _, ok := admit(coldStartCoverage, "model-late", false); ok {
		t.Fatal("每分钟补覆盖超过 6 个时应暂停")
	}
	harness.advance(time.Minute + time.Second)
	if release, ok := admit(coldStartCoverage, "model-late", false); !ok {
		t.Fatal("一分钟后补覆盖配额应恢复")
	} else {
		release(true)
	}
	if _, ok := admit(coldStartReplace, "", false); ok {
		t.Fatal("出口限流时替换空闲 Worker 应暂停")
	}
	if _, ok := admit(coldStartExpand, "", false); ok {
		t.Fatal("出口限流时超出常驻的扩容应暂停")
	}
	harness.mustLogged(1, "出口限流暂停冷启动", "路径=替换")
	harness.mustLogged(1, "出口限流暂停冷启动", "路径=超出常驻")
	if release, ok := guard.admitColdStart("http://other.example:3128", false, "", coldStartReplace, 2); !ok {
		t.Fatal("没有判定的出口不经配额")
	} else {
		release(true)
	}
	if got := egressRefillLimit(16); got != 4 {
		t.Fatalf("WARM_STARTUP_CONCURRENCY=16 时补位并发 = %d，期望 4", got)
	}
	if got := egressRefillLimit(3); got != egressRefillConcurrencyMin {
		t.Fatalf("WARM_STARTUP_CONCURRENCY=3 时补位并发 = %d，期望 2", got)
	}
}

// TestEgressGuardRelaxLadder 进入判定时后台撤回出口上第 2 档以上的逐级冷却（缩到现在 + 10 秒），完成后写一条持久日志并计入状态
func TestEgressGuardRelaxLadder(t *testing.T) {
	harness := newGuardHarness(t)
	harness.pool.ladder[""] = []string{"l1@example.com", "l2@example.com", "l3@example.com", "bad@example.com"}
	harness.pool.failed["bad@example.com"] = true
	startedAt := time.Now()
	harness.enterWithFresh(0)
	harness.guard.relaxWG.Wait()
	harness.pool.mu.Lock()
	relaxed := len(harness.pool.relax)
	until := harness.pool.until
	harness.pool.mu.Unlock()
	if relaxed != 3 || until.Before(startedAt.Add(egressLimitedCooldown)) || until.After(time.Now().Add(egressLimitedCooldown)) {
		t.Fatalf("撤回 %d 个、缩到 %s，期望 3 个、约现在 + 10 秒", relaxed, until)
	}
	entries := harness.logged("出口限流：撤回逐级冷却 | 出口=直连 | 账号=3 | 失败=1 | 耗时=")
	if len(entries) != 1 || !entries[0].durable {
		t.Fatalf("撤回日志 = %+v，期望一条持久日志", entries)
	}
	if entry := harness.status(""); entry.Relaxed != 3 {
		t.Fatalf("状态里撤回数 = %d", entry.Relaxed)
	}
}

// TestEgressStatusQueuedAndLocalRejects 状态里的排队数随进入、离开“出口限速”排队变化；本地 429 按最近一分钟计
func TestEgressStatusQueuedAndLocalRejects(t *testing.T) {
	harness := newGuardHarness(t)
	harness.enterWithFresh(20)
	leaveA := harness.guard.enterEgressWait("")
	leaveB := harness.guard.enterEgressWait("")
	harness.guard.noteLocalReject("", harness.clock())
	harness.guard.noteLocalReject("", harness.clock())
	entry := harness.status("")
	if entry.Queued != 2 || entry.LocalRejects1m != 2 || entry.Success1m != 20 || entry.Bare1m == 0 || entry.Accounts != 100 {
		t.Fatalf("状态 = %+v", entry)
	}
	if len(entry.Lanes) != 1 || entry.Lanes[0].Lane != "normal" || entry.Lanes[0].LimitRPM != 25 {
		t.Fatalf("通道 = %+v", entry.Lanes)
	}
	if entry.RetryAt.Sub(harness.clock()) != 7200*time.Millisecond {
		t.Fatalf("建议重试 = %s 后，期望 60s/25 × 3 = 7.2s", entry.RetryAt.Sub(harness.clock()))
	}
	harness.mustLogged(1, "出口每分钟请求已满 | 出口=直连 | 通道=普通 | 限速=25/分钟 | 令牌释放前新请求按“出口限速”排队")
	leaveA()
	leaveB()
	harness.advance(time.Minute + time.Second)
	if entry := harness.status(""); entry.Queued != 0 || entry.LocalRejects1m != 0 {
		t.Fatalf("一分钟后状态 = %+v", entry)
	}
}

// TestEgressDurableLogs 判定器的关键行（成立、调整、停止限速、几乎完全被拒、撤回、降级、暂停冷启动、每分钟已满、解除、撤销、恢复）
// 一律走 logDurable；默认写进运行日志
func TestEgressDurableLogs(t *testing.T) {
	harness := newGuardHarness(t)
	harness.enterWithFresh(0)
	harness.guard.enterEgressWait("")()
	harness.guard.admitColdStart("", false, "", coldStartReplace, 2)
	harness.guard.withdraw(aistudio.EgressID(""), harness.clock())
	harness.guard.resume(aistudio.EgressID(""), harness.clock())
	harness.guard.relaxWG.Wait()
	harness.mu.Lock()
	entries := append([]egressLogEntry(nil), harness.logs...)
	harness.mu.Unlock()
	prefixes := map[string]bool{}
	for _, entry := range entries {
		if !entry.durable {
			t.Fatalf("日志不是持久行：%s", entry.message)
		}
		prefixes[strings.SplitN(entry.message, " | ", 2)[0]] = true
	}
	for _, prefix := range []string{"出口限流判定成立", "出口每分钟请求已满", "出口限流暂停冷启动", "出口限流判定撤销", "出口限流判定恢复", "出口限流：撤回逐级冷却"} {
		if !prefixes[prefix] {
			t.Fatalf("缺少持久日志 %q：%v", prefix, prefixes)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	requests := newRequestRegistry(ctx)
	guard := newEgressGuard(nil, requests)
	for index := range 10 {
		guard.observeOutcome(egressObservation{AccountID: coldAccount(index), Outcome: egressBare, At: time.Now()})
	}
	if countSourceLogs(requests, "service", "出口限流判定成立 | 出口=直连") != 1 {
		t.Fatal("默认应写进运行日志")
	}
}

// egressStatusView 为测试读取的出口状态（与 api.AdminEgress 相同）
type egressStatusView = api.AdminEgress

// TestEgressQueueSnapshotText 排队快照在候选里有因出口令牌桶已满按忙碌分组的时追加出口限速、出口已满与下一个令牌
func TestEgressQueueSnapshotText(t *testing.T) {
	now := time.Now()
	snapshot := queueSnapshot{warmBusy: 3, egressLimited: 3, egressLimit: 6, egressNext: now.Add(2400 * time.Millisecond)}
	if text := snapshot.text(now); !strings.HasSuffix(text, " | 出口限速=6/分钟 | 出口已满=3 | 下一个令牌=2.4s") {
		t.Fatalf("排队快照 = %q", text)
	}
	if text := (queueSnapshot{warmBusy: 1}).text(now); strings.Contains(text, "出口") {
		t.Fatalf("没有出口限速时不应写出口字段：%q", text)
	}
}
