package app

import (
	"math/rand/v2"
	"sync"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// 出口风暴场景（方案 §14 WP-E）：真实的出口判定器（进入、AIMD、限速有效性检查、解除、冷启动闸门）在假时钟上逐秒驱动，
// 账户池侧的出口令牌桶（主用模型优先、非主用保底 25%）与上游按场景模拟。判定器的窗口与节拍都是分钟级常量（2 分钟窗口、3 分钟有效性检查），
// 走真实的 generateWithRetry 要跑十几分钟墙钟时间，这里把同一套判定逻辑放进离散时间模拟里，几十毫秒跑完。
// 两种假上游：固定容量（每 60 秒最多接受 C 次发送，其余裸 QE），与按概率拒绝（每次发送 85% 裸 QE，与速率无关）

const (
	// stormAccounts 为同一出口（直连）上的账号数
	stormAccounts = 20
	// stormArrivalEvery 为请求到达的间隔（每分钟 60 个，主用与非主用 2:1 混合）
	stormArrivalEvery = time.Second
	// stormPrimaryWait 与 stormOtherWait 为主用模型的排队上限（PRIMARY_QUEUE_WAIT 默认 10s）与非主用的出口排队上限（egressQueueWaitMax）
	stormPrimaryWait = 10 * time.Second
	stormOtherWait   = egressQueueWaitMax
	// stormRefillLimit 为冷启动闸门的补位并发（WARM_STARTUP_CONCURRENCY=16 → 4）
	stormRefillLimit = 4
	// egressShareForTest 为非主用模型在出口令牌桶里的保底份额（与 aistudio 的出口令牌桶一致）
	egressShareForTest = 0.25
)

// stormUpstream 为假上游：accept 返回这次发送是否被接受（否则为裸 QE）
type stormUpstream interface {
	accept(now time.Time) bool
}

// fixedCapacityUpstream 每 60 秒最多接受 capacity 次发送；lifted 之后不再限制
type fixedCapacityUpstream struct {
	capacity int
	lifted   time.Time
	accepted []time.Time
}

func (upstream *fixedCapacityUpstream) accept(now time.Time) bool {
	if !upstream.lifted.IsZero() && !now.Before(upstream.lifted) {
		return true
	}
	kept := upstream.accepted[:0]
	for _, at := range upstream.accepted {
		if now.Sub(at) < time.Minute {
			kept = append(kept, at)
		}
	}
	upstream.accepted = kept
	if len(upstream.accepted) >= upstream.capacity {
		return false
	}
	upstream.accepted = append(upstream.accepted, now)
	return true
}

// randomRejectUpstream 每次发送按 reject 的概率裸 QE，与发送速率无关
type randomRejectUpstream struct {
	reject float64
	random *rand.Rand
}

func (upstream *randomRejectUpstream) accept(time.Time) bool {
	return upstream.random.Float64() >= upstream.reject
}

// stormSend 为一次发往上游的生成请求
type stormSend struct {
	at      time.Time
	primary bool
}

// stormPool 为判定器看到的账户池：限速由判定器设下，发送计数来自模拟
type stormPool struct {
	mu    sync.Mutex
	now   func() time.Time
	limit [2]int
	sends []stormSend
}

func (pool *stormPool) EgressSnapshot() []aistudio.EgressStatus {
	pool.mu.Lock()
	defer pool.mu.Unlock()
	now := pool.now()
	status := aistudio.EgressStatus{Key: "", ID: aistudio.EgressID(""), Label: aistudio.EgressLabel("")}
	normal := &status.Lanes[aistudio.EgressLaneNormal]
	normal.Lane, normal.Accounts, normal.Limit = aistudio.EgressLaneNormal, stormAccounts, pool.limit[aistudio.EgressLaneNormal]
	for _, send := range pool.sends {
		age := now.Sub(send.at)
		if age < time.Minute {
			normal.Sent1m++
			if send.primary {
				normal.PrimarySent1m++
			}
		}
		if age < 30*time.Second {
			normal.Sent30s++
		}
	}
	status.Lanes[aistudio.EgressLaneUltra].Lane = aistudio.EgressLaneUltra
	return []aistudio.EgressStatus{status}
}

func (pool *stormPool) EgressSnapshotFor(keys []string) ([]aistudio.EgressStatus, int) {
	return egressSnapshotFor(pool.EgressSnapshot(), keys)
}

func (pool *stormPool) SetEgressRateLimit(_ string, lane aistudio.EgressLane, limit int) {
	pool.mu.Lock()
	pool.limit[lane] = limit
	pool.mu.Unlock()
}

func (pool *stormPool) RateLadderAccounts(string, int) []string { return nil }

func (pool *stormPool) RelaxRateLimitCooldowns(string, time.Time, int) (int, error) { return 0, nil }

func (pool *stormPool) currentLimit() int {
	pool.mu.Lock()
	defer pool.mu.Unlock()
	return pool.limit[aistudio.EgressLaneNormal]
}

func (pool *stormPool) record(send stormSend) {
	pool.mu.Lock()
	pool.sends = append(pool.sends, send)
	if len(pool.sends) > 4096 {
		pool.sends = append([]stormSend(nil), pool.sends[len(pool.sends)-2048:]...)
	}
	pool.mu.Unlock()
}

// stormRequest 为一个排队中的请求
type stormRequest struct {
	primary   bool
	arrived   time.Time
	rateFails int
}

// stormResult 为一次模拟的结果
type stormResult struct {
	successes, primarySuccesses, otherSuccesses int
	upstream429, local429                       int
	sends, coldStarts                           int
	everLimited, everSuspended                  bool
	limitAtLift, maxLimitAfterLift              int
	releasedAfterLift                           time.Duration
}

// runEgressStorm 逐秒模拟 duration：withGuard 为假时关闭判定器（原行为：不限速、每次裸 QE 都按冷却登记淘汰并冷启动替补）。
// lift 非零时在这个时刻之后记录限速与解除（上游在 lift 解除限制）
func runEgressStorm(t *testing.T, upstream stormUpstream, duration time.Duration, withGuard bool, lift time.Duration) stormResult {
	t.Helper()
	start := time.Date(2026, 3, 10, 18, 0, 0, 0, time.UTC)
	now := start
	clock := func() time.Time { return now }
	pool := &stormPool{now: clock}
	var guard *egressGuard
	if withGuard {
		guard = newEgressGuard(nil, nil)
		guard.pool = pool
		guard.now = clock
		guard.logf = func(egressLogEntry) {}
	}
	var result stormResult
	var queue []*stormRequest
	demoted := make(map[string]time.Time)
	next := 0
	pickAccount := func() string {
		for range stormAccounts {
			accountID := coldAccount(next % stormAccounts)
			next++
			if until, ok := demoted[accountID]; !ok || !now.Before(until) {
				return accountID
			}
		}
		return coldAccount(next % stormAccounts)
	}
	var tokens float64
	arrivals := 0
	primaryIssued, otherIssued := 0, 0
	for elapsed := time.Duration(0); elapsed < duration; elapsed += time.Second {
		now = start.Add(elapsed)
		if elapsed%stormArrivalEvery == 0 {
			queue = append(queue, &stormRequest{primary: arrivals%3 != 2, arrived: now})
			arrivals++
		}
		limit := pool.currentLimit()
		throttled := withGuard && limit > 0
		if throttled {
			tokens = min(tokens+float64(limit)/60, max(float64(limit)/6, 1))
		}
		// 主用模型先取令牌，非主用保底 25%（与账户池的出口令牌桶一致）：非主用已取得的令牌不到总数的 1/4 时下一个令牌给非主用
		var primaries, others []*stormRequest
		for _, request := range queue {
			if request.primary {
				primaries = append(primaries, request)
			} else {
				others = append(others, request)
			}
		}
		ordered := make([]*stormRequest, 0, len(queue))
		for len(primaries) > 0 || len(others) > 0 {
			takeOther := len(primaries) == 0 ||
				len(others) > 0 && float64(otherIssued) < egressShareForTest*float64(otherIssued+primaryIssued+1)
			if takeOther {
				ordered = append(ordered, others[0])
				others = others[1:]
			} else {
				ordered = append(ordered, primaries[0])
				primaries = primaries[1:]
			}
		}
		var waiting []*stormRequest
		waitedOnEgress := false
		for _, request := range ordered {
			if throttled && tokens < 1 {
				waiting = append(waiting, request)
				waitedOnEgress = true
				continue
			}
			if throttled {
				tokens--
			}
			if request.primary {
				primaryIssued++
			} else {
				otherIssued++
			}
			accountID := pickAccount()
			accepted := upstream.accept(now)
			pool.record(stormSend{at: now, primary: request.primary})
			result.sends++
			outcome := egressBare
			if accepted {
				outcome = egressSuccess
			}
			verdict := guard.observeOutcome(egressObservation{Key: "", AccountID: accountID, Outcome: outcome, At: now})
			if accepted {
				result.successes++
				if request.primary {
					result.primarySuccesses++
				} else {
					result.otherSuccesses++
				}
				continue
			}
			if verdict.Demote {
				demoted[accountID] = now.Add(egressDemoteCooldown)
			}
			// 裸 QE 后的冷启动：原行为逐级冷却登记淘汰、冷启动替补；判定成立时经冷启动闸门（替换暂停，只有补位与补覆盖按配额）
			if withGuard {
				if release, ok := guard.admitColdStart("", false, testRuntimeModel, coldStartReplace, stormRefillLimit); ok {
					if guard.verdict("", false, now).Limited {
						result.coldStarts++
					}
					release(true)
				}
			} else {
				result.coldStarts++
			}
			request.rateFails++
			switchLimit := normalRateLimitSwitchLimit
			if verdict.Suspended {
				switchLimit = egressSuspendedRateSwitches
			}
			if request.rateFails > switchLimit {
				result.upstream429++
				continue
			}
			waiting = append(waiting, request)
		}
		// 排队超限：主用模型按排队上限，非主用按出口排队上限，本地返回 429
		queue = queue[:0]
		for _, request := range waiting {
			limit := stormOtherWait
			if request.primary {
				limit = stormPrimaryWait
			}
			if now.Sub(request.arrived) >= limit {
				result.local429++
				guard.noteLocalReject("", now)
				continue
			}
			queue = append(queue, request)
		}
		if waitedOnEgress && withGuard {
			guard.enterEgressWait("")()
		}
		if withGuard && elapsed%(5*time.Second) == 0 {
			guard.tick(now)
		}
		if withGuard {
			verdict := guard.verdict("", false, now)
			result.everLimited = result.everLimited || verdict.Limited
			result.everSuspended = result.everSuspended || verdict.Suspended
			if lift > 0 && elapsed >= lift {
				if elapsed == lift {
					result.limitAtLift = pool.currentLimit()
				}
				result.maxLimitAfterLift = max(result.maxLimitAfterLift, pool.currentLimit())
				if result.releasedAfterLift == 0 && !verdict.Limited {
					result.releasedAfterLift = elapsed - lift
				}
			}
		}
	}
	return result
}

// TestEgressStormScenario 出口风暴两种假上游：
//   - 固定容量（20 个账号同出口，每 60 秒最多接受 20 次，其余裸 QE）：判定成立、限速生效；成功数不低于关闭判定器时的 90%；
//     非主用模型拿到 ≥20% 的成功；冷启动有界（判定成立后替换暂停）；上游解除限制后限速提升，并在设定时间内解除判定；
//   - 按概率拒绝（每次发送 85% 裸 QE）：判定成立，限速在测试时间内判为无效并停止（Suspended）；成功数不低于关闭判定器时的 90%
func TestEgressStormScenario(t *testing.T) {
	t.Run("固定容量", func(t *testing.T) {
		const duration = 20 * time.Minute
		const lift = 12 * time.Minute
		baseline := runEgressStorm(t, &fixedCapacityUpstream{capacity: 20, lifted: time.Date(2026, 3, 10, 18, 0, 0, 0, time.UTC).Add(lift)}, duration, false, 0)
		guarded := runEgressStorm(t, &fixedCapacityUpstream{capacity: 20, lifted: time.Date(2026, 3, 10, 18, 0, 0, 0, time.UTC).Add(lift)}, duration, true, lift)
		t.Logf("关闭判定器：%+v", baseline)
		t.Logf("打开判定器：%+v", guarded)
		if !guarded.everLimited {
			t.Fatal("固定容量的出口应判定为限流")
		}
		if float64(guarded.successes) < 0.9*float64(baseline.successes) {
			t.Fatalf("成功 %d，低于关闭判定器时 %d 的 90%%", guarded.successes, baseline.successes)
		}
		if float64(guarded.otherSuccesses) < 0.2*float64(guarded.successes) {
			t.Fatalf("非主用成功 %d / %d，期望 ≥20%%", guarded.otherSuccesses, guarded.successes)
		}
		if guarded.sends >= baseline.sends {
			t.Fatalf("判定成立后应少发：发送 %d，关闭判定器时 %d", guarded.sends, baseline.sends)
		}
		if guarded.coldStarts > 0 || baseline.coldStarts < 100 {
			t.Fatalf("判定期间的冷启动应有界（替换暂停）：打开 %d，关闭 %d", guarded.coldStarts, baseline.coldStarts)
		}
		if guarded.maxLimitAfterLift <= guarded.limitAtLift {
			t.Fatalf("上游解除限制后限速应提升：解除时 %d，之后最高 %d", guarded.limitAtLift, guarded.maxLimitAfterLift)
		}
		if guarded.releasedAfterLift == 0 || guarded.releasedAfterLift > 8*time.Minute {
			t.Fatalf("上游解除限制后应在 8 分钟内解除判定，实际 %s", guarded.releasedAfterLift)
		}
	})
	t.Run("按概率拒绝", func(t *testing.T) {
		const duration = 30 * time.Minute
		baseline := runEgressStorm(t, &randomRejectUpstream{reject: 0.85, random: rand.New(rand.NewPCG(7, 11))}, duration, false, 0)
		guarded := runEgressStorm(t, &randomRejectUpstream{reject: 0.85, random: rand.New(rand.NewPCG(7, 11))}, duration, true, 0)
		t.Logf("关闭判定器：%+v", baseline)
		t.Logf("打开判定器：%+v", guarded)
		if !guarded.everLimited {
			t.Fatal("按概率拒绝的出口同样应判定为限流")
		}
		if !guarded.everSuspended {
			t.Fatal("拒绝与发送速率无关时限速应在测试时间内判为无效并停止")
		}
		if float64(guarded.successes) < 0.9*float64(baseline.successes) {
			t.Fatalf("成功 %d，低于关闭判定器时 %d 的 90%%", guarded.successes, baseline.successes)
		}
	})
}
