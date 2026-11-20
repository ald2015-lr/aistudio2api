package app

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/camoufoxnative"
)

// rotationBase 返回一个会轮换的输入：普通分区（排空名额 2、排空总数上限 4，都没有占用，可以先启后停），两个就绪满 10 分钟的驻留号，
// W 今日发送 120、另一个 30，接替的号 0；三个号能服务的主用、热门模型个数相同
func rotationBase(now time.Time) rotationInput {
	return rotationInput{
		now: now, enabled: true, running: true, rate: 1, drainSlots: 0, drainLimit: 2, drainTotal: 0, drainTotalLimit: 4,
		resident: 2, warmTarget: 2,
		candidates: []rotationCandidate{
			{accountID: "rot-w@example.com", usage: 120, readyAt: now.Add(-10 * time.Minute), lastUsed: now.Add(-time.Second), busy: true},
			{accountID: "rot-x@example.com", usage: 30, readyAt: now.Add(-10 * time.Minute), lastUsed: now.Add(-time.Second)},
		},
		spare: func() rotationSpare { return rotationSpare{accountID: "rot-s@example.com"} },
	}
}

// TestPickRotationTable 轮换判断的纯函数：每一种不该登记的情况都不登记（并给出对应原因，暂停的另标 pause），
// 普通分区忙碌的 W 先启后停；排空名额不够时忙碌的 W 跳过、空闲的 W 空闲直接换；同分时空闲的先、再比就绪早的
func TestPickRotationTable(t *testing.T) {
	now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name   string
		mutate func(input *rotationInput)
		want   string
		pause  bool
		soft   bool
		victim string
	}{
		{name: "会轮换：忙碌的 W 先启后停", mutate: func(*rotationInput) {}, victim: "rot-w@example.com", soft: true},
		{name: "差值小于 50", mutate: func(input *rotationInput) {
			input.spare = func() rotationSpare { return rotationSpare{accountID: "rot-s@example.com", usage: 71} }
		}, want: rotationSkipGap},
		{name: "差值恰好 50", mutate: func(input *rotationInput) {
			input.spare = func() rotationSpare { return rotationSpare{accountID: "rot-s@example.com", usage: 70} }
		}, victim: "rot-w@example.com", soft: true},
		{name: "接替的号用得更多", mutate: func(input *rotationInput) {
			input.spare = func() rotationSpare { return rotationSpare{accountID: "rot-s@example.com", usage: 200} }
		}, want: rotationSkipGap},
		{name: "服务不足 5 分钟", mutate: func(input *rotationInput) {
			input.candidates[0].readyAt = now.Add(-4 * time.Minute)
			input.candidates = input.candidates[:1]
		}, want: rotationSkipNoCandidate},
		{name: "就绪时间未知", mutate: func(input *rotationInput) {
			input.candidates[0].readyAt = time.Time{}
			input.candidates = input.candidates[:1]
		}, want: rotationSkipNoCandidate},
		{name: "30 分钟冷却期内", mutate: func(input *rotationInput) {
			input.candidates[0].cooloffUntil = now.Add(time.Minute)
		}, want: rotationSkipGap},
		{name: "冷却期刚过", mutate: func(input *rotationInput) {
			input.candidates[0].cooloffUntil = now
		}, victim: "rot-w@example.com", soft: true},
		{name: "不可调度", mutate: func(input *rotationInput) { input.candidates[0].unschedulable = true }, want: rotationSkipGap},
		{name: "死槽位", mutate: func(input *rotationInput) { input.candidates[0].dead = true }, want: rotationSkipGap},
		{name: "冷却中", mutate: func(input *rotationInput) { input.candidates[0].cooling = true }, want: rotationSkipGap},
		{name: "正在启动", mutate: func(input *rotationInput) { input.candidates[0].starting = true }, want: rotationSkipGap},
		{name: "被其他路径选中", mutate: func(input *rotationInput) { input.candidates[0].reserved = true }, want: rotationSkipGap},
		{name: "已在排空或有登记", mutate: func(input *rotationInput) { input.candidates[0].registered = true }, want: rotationSkipGap},
		{name: "Worker 已失败", mutate: func(input *rotationInput) { input.candidates[0].failed = true }, want: rotationSkipGap},
		{name: "速率 R 未到", mutate: func(input *rotationInput) { input.lastStart = now.Add(-59 * time.Second) }, want: rotationSkipRate},
		{name: "速率 R 已到", mutate: func(input *rotationInput) { input.lastStart = now.Add(-time.Minute) }, victim: "rot-w@example.com", soft: true},
		{name: "R=2 时 30 秒即可", mutate: func(input *rotationInput) {
			input.rate, input.lastStart = 2, now.Add(-30*time.Second)
		}, victim: "rot-w@example.com", soft: true},
		{name: "R=2 时 29 秒不行", mutate: func(input *rotationInput) {
			input.rate, input.lastStart = 2, now.Add(-29*time.Second)
		}, want: rotationSkipRate},
		{name: "墙钟回拨不按间隔挡住", mutate: func(input *rotationInput) { input.lastStart = now.Add(time.Minute) }, victim: "rot-w@example.com", soft: true},
		{name: "已有一个进行中", mutate: func(input *rotationInput) { input.inProgress = true }, want: rotationSkipInProgress},
		{name: "失败退避中", mutate: func(input *rotationInput) { input.backoffUntil = now.Add(time.Second) }, want: rotationPauseBackoff, pause: true},
		{name: "失败退避已过", mutate: func(input *rotationInput) { input.backoffUntil = now }, victim: "rot-w@example.com", soft: true},
		{name: "出口限流", mutate: func(input *rotationInput) { input.egressLimited = true }, want: rotationPauseEgress, pause: true},
		{name: "分区内有非轮换登记", mutate: func(input *rotationInput) { input.pendingOther = true }, want: rotationPauseRetiring, pause: true},
		{name: "没有 S", mutate: func(input *rotationInput) {
			input.spare = func() rotationSpare { return rotationSpare{} }
		}, want: rotationPauseNoSpare, pause: true},
		{name: "没有 spare 函数", mutate: func(input *rotationInput) { input.spare = nil }, want: rotationPauseNoSpare, pause: true},
		{name: "balanced 关闭", mutate: func(input *rotationInput) { input.enabled = false }, want: rotationSkipOff},
		{name: "服务没有运行", mutate: func(input *rotationInput) { input.running = false }, want: rotationSkipStopped},
		{name: "Ultra 分区（drainLimit=1）忙碌的 W 跳过", mutate: func(input *rotationInput) {
			input.drainLimit = 1
			input.candidates[1].busy = true
		}, want: rotationPauseNoRoom, pause: true},
		{name: "Ultra 分区空闲的 W 空闲直接换", mutate: func(input *rotationInput) {
			input.drainLimit = 1
			input.candidates[0].busy = false
		}, victim: "rot-w@example.com"},
		{name: "排空名额只剩一个：给死槽位留着，只换空闲的", mutate: func(input *rotationInput) {
			input.drainLimit, input.drainSlots = 4, 3
			input.candidates[1].usage = 100
		}, victim: "rot-x@example.com"},
		{name: "排空名额够：忙碌的 W 先启后停", mutate: func(input *rotationInput) {
			input.drainLimit, input.drainSlots = 4, 2
		}, victim: "rot-w@example.com", soft: true},
		{name: "排空总数只剩一个（轮换的排空一直有请求、不再占名额）：给死槽位留着，忙碌的 W 跳过", mutate: func(input *rotationInput) {
			input.drainSlots, input.drainTotal = 0, 3
			input.candidates[1].busy = true
		}, want: rotationPauseNoRoom, pause: true},
		{name: "排空总数只剩一个：只换空闲的", mutate: func(input *rotationInput) {
			input.drainSlots, input.drainTotal = 0, 3
			input.candidates[1].usage = 100
		}, victim: "rot-x@example.com"},
		{name: "排空总数已到 drainLimit（让出额度封顶，软排空不会多让出槽位）：忙碌的 W 跳过", mutate: func(input *rotationInput) {
			input.drainSlots, input.drainTotal = 0, 2
			input.candidates[1].busy = true
		}, want: rotationPauseNoRoom, pause: true},
		{name: "让出额度封顶：只换空闲的", mutate: func(input *rotationInput) {
			input.drainSlots, input.drainTotal = 0, 2
			input.candidates[1].usage = 100
		}, victim: "rot-x@example.com"},
		{name: "排空总数低于 drainLimit、名额与总数都够：忙碌的 W 先启后停", mutate: func(input *rotationInput) {
			input.drainLimit, input.drainTotalLimit, input.drainSlots, input.drainTotal = 3, 6, 1, 2
		}, victim: "rot-w@example.com", soft: true},
		{name: "排空总数恰好到 drainLimit：不再先启后停", mutate: func(input *rotationInput) {
			input.drainLimit, input.drainTotalLimit, input.drainSlots, input.drainTotal = 3, 6, 1, 3
			input.candidates[1].busy = true
		}, want: rotationPauseNoRoom, pause: true},
		{name: "在跑的号能服务的热门模型都比接替的号多：暂停", mutate: func(input *rotationInput) {
			input.candidates[0].rank, input.candidates[1].rank = coldRank{primary: 1, hot: 1}, coldRank{primary: 1, hot: 2}
			input.spare = func() rotationSpare { return rotationSpare{accountID: "rot-s@example.com", rank: coldRank{primary: 1}} }
		}, want: rotationPauseWeakerSpare, pause: true},
		{name: "在跑的号能服务的主用模型比接替的号多：暂停（主用先比，接替的号热门模型更多也不行）", mutate: func(input *rotationInput) {
			input.candidates[0].rank, input.candidates[1].rank = coldRank{primary: 2}, coldRank{primary: 2}
			input.spare = func() rotationSpare {
				return rotationSpare{accountID: "rot-s@example.com", rank: coldRank{primary: 1, hot: 3}}
			}
		}, want: rotationPauseWeakerSpare, pause: true},
		{name: "用得最多的号能服务的模型比接替的号多：换不比它多的下一个", mutate: func(input *rotationInput) {
			input.candidates[0].rank = coldRank{primary: 1, hot: 1}
			input.candidates[1].rank, input.candidates[1].usage = coldRank{primary: 1}, 100
			input.spare = func() rotationSpare { return rotationSpare{accountID: "rot-s@example.com", rank: coldRank{primary: 1}} }
		}, victim: "rot-x@example.com", soft: true},
		{name: "接替的号能服务的模型更多：照常换", mutate: func(input *rotationInput) {
			input.spare = func() rotationSpare {
				return rotationSpare{accountID: "rot-s@example.com", rank: coldRank{primary: 1, hot: 1}}
			}
		}, victim: "rot-w@example.com", soft: true},
		{name: "换不比它多的下一个，差距按它算", mutate: func(input *rotationInput) {
			input.candidates[0].rank = coldRank{hot: 1}
			input.spare = func() rotationSpare { return rotationSpare{accountID: "rot-s@example.com"} }
		}, want: rotationSkipGap},
		{name: "超出常驻数时空闲超过 5 分钟的交给空闲回收", mutate: func(input *rotationInput) {
			input.resident, input.warmTarget = 3, 2
			input.candidates[0].busy = false
			input.candidates[0].lastUsed = now.Add(-workerIdleTimeout)
		}, want: rotationSkipGap},
		{name: "未超出常驻数时空闲久的照样轮换", mutate: func(input *rotationInput) {
			input.candidates[0].busy = false
			input.candidates[0].lastUsed = now.Add(-time.Hour)
		}, victim: "rot-w@example.com", soft: true},
		{name: "同分时空闲的先", mutate: func(input *rotationInput) {
			input.candidates[1].usage = 120
		}, victim: "rot-x@example.com", soft: true},
		{name: "同分同忙闲时就绪早的先", mutate: func(input *rotationInput) {
			input.candidates[1].usage, input.candidates[1].busy = 120, true
			input.candidates[1].readyAt = now.Add(-time.Hour)
		}, victim: "rot-x@example.com", soft: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			input := rotationBase(now)
			test.mutate(&input)
			got := pickRotation(input)
			if test.victim != "" {
				if got.accountID != test.victim || got.soft != test.soft || got.spare == "" || got.reason != "" || got.pause {
					t.Fatalf("应登记 %s（先启后停=%t），实际 %+v", test.victim, test.soft, got)
				}
				return
			}
			if got.accountID != "" || got.reason != test.want || got.pause != test.pause {
				t.Fatalf("不应登记，原因应为 %q（暂停=%t），实际 %+v", test.want, test.pause, got)
			}
		})
	}
}

// TestPickRotationSpareOnce 接替的号（替补挑法干跑）只在选出被换下的号之后调用，且只调用一次；暂停或没有候选时不调用
func TestPickRotationSpareOnce(t *testing.T) {
	now := time.Now()
	calls := 0
	input := rotationBase(now)
	input.spare = func() rotationSpare { calls++; return rotationSpare{accountID: "rot-s@example.com"} }
	if got := pickRotation(input); got.accountID == "" || calls != 1 {
		t.Fatalf("应登记且只干跑一次替补挑法：%+v，调用 %d 次", got, calls)
	}
	for _, mutate := range []func(*rotationInput){
		func(input *rotationInput) { input.pendingOther = true },
		func(input *rotationInput) { input.inProgress = true },
		func(input *rotationInput) { input.candidates = nil },
	} {
		calls = 0
		input := rotationBase(now)
		input.spare = func() rotationSpare { calls++; return rotationSpare{accountID: "rot-s@example.com"} }
		mutate(&input)
		if got := pickRotation(input); got.accountID != "" || calls != 0 {
			t.Fatalf("不轮换时不应干跑替补挑法：%+v，调用 %d 次", got, calls)
		}
	}
}

// pickRotationReference 为 pickRotation 的 O(n²) 参考实现：逐条写出资格条件（不复用 rotationEligible、compareColdRank），
// 被换下的号是能服务的主用、热门模型不比接替的号多的合格候选里，没有任何其他这样的候选“更该被换下”的那一个
func pickRotationReference(input rotationInput) rotationDecision {
	if !input.enabled || !input.running || input.egressLimited || input.pendingOther || input.inProgress || input.now.Before(input.backoffUntil) {
		return rotationDecision{}
	}
	if !input.lastStart.IsZero() && !input.now.Before(input.lastStart) && input.now.Sub(input.lastStart)*time.Duration(max(input.rate, 1)) < time.Minute {
		return rotationDecision{}
	}
	soft := input.drainLimit-input.drainSlots >= 2 && input.drainTotalLimit-input.drainTotal >= 2 && input.drainLimit-input.drainTotal >= 1
	eligible := func(candidate rotationCandidate) bool {
		if candidate.starting || candidate.reserved || candidate.registered || candidate.failed ||
			candidate.unschedulable || candidate.dead || candidate.cooling || candidate.readyAt.IsZero() {
			return false
		}
		if input.now.Sub(candidate.readyAt) < 5*time.Minute || candidate.cooloffUntil.After(input.now) {
			return false
		}
		if !candidate.busy && input.resident > input.warmTarget && input.now.Sub(candidate.lastUsed) >= 5*time.Minute {
			return false
		}
		return soft || !candidate.busy
	}
	if !slices.ContainsFunc(input.candidates, eligible) {
		return rotationDecision{}
	}
	spare := input.spare()
	if spare.accountID == "" {
		return rotationDecision{}
	}
	// stronger 判断候选能服务的主用模型比接替的号多，或主用一样多、热门模型更多
	stronger := func(candidate rotationCandidate) bool {
		return candidate.rank.primary > spare.rank.primary || candidate.rank.primary == spare.rank.primary && candidate.rank.hot > spare.rank.hot
	}
	// ahead 判断 b 是否严格排在 a 前面（更该被换下）
	ahead := func(b rotationCandidate, a rotationCandidate) bool {
		switch {
		case b.usage != a.usage:
			return b.usage > a.usage
		case b.busy != a.busy:
			return a.busy
		case !b.readyAt.Equal(a.readyAt):
			return b.readyAt.Before(a.readyAt)
		}
		return b.accountID < a.accountID
	}
	for _, candidate := range input.candidates {
		if !eligible(candidate) || stronger(candidate) {
			continue
		}
		beaten := false
		for _, other := range input.candidates {
			if other.accountID != candidate.accountID && eligible(other) && !stronger(other) && ahead(other, candidate) {
				beaten = true
				break
			}
		}
		if beaten {
			continue
		}
		if int64(candidate.usage)-int64(spare.usage) < balancedRotateGap {
			return rotationDecision{}
		}
		return rotationDecision{accountID: candidate.accountID, soft: soft}
	}
	return rotationDecision{}
}

// TestPickRotationMatchesReference 随机输入上 pickRotation 选出的被换下的号与路径和 O(n²) 参考实现一致
func TestPickRotationMatchesReference(t *testing.T) {
	seed := time.Now().UnixNano()
	t.Logf("随机种子 %d", seed)
	random := rand.New(rand.NewSource(seed))
	now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	chance := func(n int) bool { return random.Intn(n) == 0 }
	registered := 0
	for round := 0; round < 3000; round++ {
		input := rotationInput{
			now: now, enabled: !chance(20), running: !chance(20), egressLimited: chance(20), pendingOther: chance(15), inProgress: chance(15),
			rate: random.Intn(4), drainLimit: random.Intn(5), resident: random.Intn(12), warmTarget: random.Intn(12),
		}
		input.drainSlots = random.Intn(input.drainLimit + 1)
		input.drainTotalLimit = 2 * input.drainLimit
		input.drainTotal = input.drainSlots + random.Intn(input.drainTotalLimit-input.drainSlots+1)
		if chance(4) {
			input.lastStart = now.Add(-time.Duration(random.Intn(90)) * time.Second)
		}
		if chance(10) {
			input.backoffUntil = now.Add(time.Duration(random.Intn(600)-300) * time.Second)
		}
		size := random.Intn(12)
		for index := 0; index < size; index++ {
			candidate := rotationCandidate{
				accountID: fmt.Sprintf("rot-%02d@example.com", random.Intn(40)),
				usage:     uint32(random.Intn(6) * 30),
				readyAt:   now.Add(-time.Duration(random.Intn(4)*3) * time.Minute),
				lastUsed:  now.Add(-time.Duration(random.Intn(4)*3) * time.Minute),
				busy:      chance(2),
				starting:  chance(15), reserved: chance(15), registered: chance(15), failed: chance(15),
				unschedulable: chance(15), dead: chance(15), cooling: chance(15),
				rank: coldRank{primary: random.Intn(3), hot: random.Intn(3)},
			}
			if chance(8) {
				candidate.readyAt = time.Time{}
			}
			if chance(6) {
				candidate.cooloffUntil = now.Add(time.Duration(random.Intn(60)-30) * time.Minute)
			}
			// 账户 ID 不重复（同一个号只有一个 Worker）
			if slices.ContainsFunc(input.candidates, func(other rotationCandidate) bool { return other.accountID == candidate.accountID }) {
				continue
			}
			input.candidates = append(input.candidates, candidate)
		}
		var spare rotationSpare
		if !chance(6) {
			spare = rotationSpare{
				accountID: "rot-spare@example.com", usage: uint32(random.Intn(5) * 30),
				rank: coldRank{primary: random.Intn(3), hot: random.Intn(3)},
			}
		}
		input.spare = func() rotationSpare { return spare }
		got, want := pickRotation(input), pickRotationReference(input)
		if got.accountID != want.accountID || got.accountID != "" && got.soft != want.soft {
			t.Fatalf("第 %d 轮：得到 %+v，参考实现 %+v，输入 %+v", round, got, want, input)
		}
		if got.accountID != "" {
			registered++
		}
	}
	if registered == 0 {
		t.Fatal("随机输入里应有登记的情况")
	}
}

// TestBalancedRotateRate 每分钟最多开始的轮换数 R = max(1, min(WARM_STARTUP_CONCURRENCY, 峰值/10))
func TestBalancedRotateRate(t *testing.T) {
	for _, test := range []struct{ maxActive, concurrency, want int }{
		{10, 2, 1}, {5, 2, 1}, {50, 2, 2}, {50, 8, 5}, {200, 8, 8}, {0, 2, 1}, {30, 0, 1},
	} {
		if got := balancedRotateRate(test.maxActive, test.concurrency); got != test.want {
			t.Fatalf("峰值 %d 启动并发 %d：R = %d，期望 %d", test.maxActive, test.concurrency, got, test.want)
		}
	}
	if balancedRotateInterval(2) != 30*time.Second || balancedRotateInterval(0) != time.Minute {
		t.Fatal("间隔应为 1 分钟 / R")
	}
}

// rotationGate 让测试控制新账号的冷启动：hold 为真时启动等 release 关闭（或 Worker 启动的 context 结束），fail 非空时启动返回它；
// waiting 为正在等待的启动数
type rotationGate struct {
	hold    atomic.Bool
	release chan struct{}
	fail    atomic.Pointer[error]
	waiting atomic.Int32
}

// gateLaunches 换掉 env 的启动函数（在任何后台启动之前调用）
func gateLaunches(env *drainEnv) *rotationGate {
	gate := &rotationGate{release: make(chan struct{})}
	env.manager.launch = func(ctx context.Context, accountID string, _ camoufoxnative.Options) (*aistudio.NativeWorker, error) {
		if failure := gate.fail.Load(); failure != nil {
			return nil, *failure
		}
		if gate.hold.Load() {
			gate.waiting.Add(1)
			defer gate.waiting.Add(-1)
			select {
			case <-gate.release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		worker, _ := aistudio.NewStubWorker(accountID, 0)
		return worker, nil
	}
	return gate
}

// rotationEnv 为轮换的集成测试环境：residents 为启动好、就绪满 10 分钟的驻留号，其余为备用号；平均调用开启，单账号并发 2
type rotationEnv struct {
	*drainEnv
	gate      *rotationGate
	residents []string
	spares    []string
}

func newRotationEnv(t *testing.T, residents int, spares int, warmTarget int, maxActive int) *rotationEnv {
	t.Helper()
	accounts := make([]string, 0, residents+spares)
	for index := 0; index < residents; index++ {
		accounts = append(accounts, fmt.Sprintf("rot-r%d@example.com", index))
	}
	for index := 0; index < spares; index++ {
		accounts = append(accounts, fmt.Sprintf("rot-s%d@example.com", index))
	}
	env := newDrainEnv(t, accounts, warmTarget, maxActive)
	gate := gateLaunches(env)
	env.pool.SetPerAccountConcurrency(2)
	env.promote(t, accounts[:residents]...)
	aged := time.Now().Add(-10 * time.Minute).UnixNano()
	for _, accountID := range accounts[:residents] {
		env.manager.mu.RLock()
		env.manager.accounts[accountID].readyAt.Store(aged)
		env.manager.mu.RUnlock()
	}
	env.manager.setBalanced(true)
	return &rotationEnv{drainEnv: env, gate: gate, residents: accounts[:residents], spares: accounts[residents:]}
}

// seedResidents 给 W（第一个驻留号）记 usage 次、其余驻留号 30 次今日发送，备用号为 0
func (env *rotationEnv) seedResidents(t *testing.T, usage uint32) {
	t.Helper()
	counts := map[string]uint32{env.residents[0]: usage}
	for _, accountID := range env.residents[1:] {
		counts[accountID] = 30
	}
	seedBalancedUsage(t, env.manager, counts)
}

// entry 返回账号登记的副本（没有时 ok 为假）
func (env *rotationEnv) entry(accountID string) (retirement, bool) {
	env.manager.retireMu.Lock()
	defer env.manager.retireMu.Unlock()
	entry := env.manager.retirements[accountID]
	if entry == nil {
		return retirement{}, false
	}
	return entry.snapshot(), true
}

// unpinnedLease 尝试只在 accountID 上取一个不固定账户的租约，返回是否取得（取得后立即释放）
func (env *rotationEnv) unpinnedLease(t *testing.T, accountID string) bool {
	t.Helper()
	lease, _, err := env.pool.TryAcquireFor(env.ctx, aistudio.AccountSelection{
		ModelID: testRuntimeModel, Method: "generateContent", AllowedAccountIDs: []string{accountID},
	})
	if err != nil || lease == nil {
		return false
	}
	_ = lease.Release()
	return true
}

// logLevelContaining 判断是否有指定级别、包含全部片段的运行日志
func logLevelContaining(requests *requestRegistry, level string, fragments ...string) bool {
	requests.mu.Lock()
	defer requests.mu.Unlock()
	for _, entry := range requests.logs {
		if entry.Level != level {
			continue
		}
		matched := true
		for _, fragment := range fragments {
			matched = matched && strings.Contains(entry.Message, fragment)
		}
		if matched {
			return true
		}
	}
	return false
}

// TestBalancedRotationSoftPath 先启后停的完整过程（普通分区峰值 8、排空名额 2）：W 忙碌时登记先启后停；软阶段 W 照常接不固定账户的请求、
// 不计入状态页的排空数但计入让出额度；接替就绪后改为普通排空，不固定账户的租约取不到、指定账户的照常能取；W 持有租约期间不关，释放后关闭；
// 驻留数回到 8、不重复补；整个过程浏览器数不超过 峰值 + drainLimit；关闭后计入轮换数、W 进入 30 分钟冷却期
func TestBalancedRotationSoftPath(t *testing.T) {
	env := newRotationEnv(t, 8, 2, 8, 8)
	limit := env.manager.drainLimit(aistudio.PoolScopeNormal)
	if limit != 2 {
		t.Fatalf("峰值 8 时 drainLimit 应为 2，实际 %d", limit)
	}
	w := env.residents[0]
	env.seedResidents(t, 120)
	release := holdLease(t, env.pool, w)
	defer release()
	env.gate.hold.Store(true)
	stopPeak := env.watchBrowsers()
	env.service.balancedRotate(env.ctx, time.Now())
	if !hasLogContaining(env.requests, "WAA Worker 平均调用轮换登记", "今日发送=120", "接替今日=0", "差=120（门槛 50）", "路径=先启后停", "就绪=10m") {
		t.Fatalf("应登记先启后停的轮换，日志 = %v", serviceLogMessages(env.requests))
	}
	if !waitUntil(5*time.Second, func() bool { return env.gate.waiting.Load() == 1 }) {
		t.Fatalf("接替的号应开始冷启动，日志 = %v", serviceLogMessages(env.requests))
	}
	drain := env.manager.drainOf(w)
	if drain == nil || !drain.soft.Load() {
		t.Fatal("W 应处于软排空")
	}
	if env.pool.Draining(w) || !env.unpinnedLease(t, w) {
		t.Fatal("软阶段 W 应照常接不固定账户的请求")
	}
	if normal, _ := env.manager.drainCounts(); normal != 0 || len(env.manager.drainingAccounts()) != 0 {
		t.Fatalf("软排空不计入状态页的排空数与主用供给：%d、%v", normal, env.manager.drainingAccounts())
	}
	if credit := env.manager.drainCredit(aistudio.PoolScopeNormal, nil); credit != 1 || env.manager.drainSlotCount(aistudio.PoolScopeNormal) != 1 {
		t.Fatalf("软排空应计入让出额度与排空名额：让出 %d，名额 %d", credit, env.manager.drainSlotCount(aistudio.PoolScopeNormal))
	}
	if stats := env.manager.balancedRotationStats(time.Now()); stats.rotating[aistudio.PoolScopeNormal] != 1 {
		t.Fatalf("进行中的轮换应为 1，实际 %v", stats.rotating)
	}
	if !hasLogContaining(env.requests, "替补启动", "原因=平均调用轮换") {
		t.Fatalf("应记录接替的号的替补启动，日志 = %v", serviceLogMessages(env.requests))
	}
	// 软阶段同一分区不再开始第二个轮换
	env.service.balancedRotate(env.ctx, time.Now().Add(2*time.Minute))
	if count := strings.Count(strings.Join(serviceLogMessages(env.requests), "\n"), "平均调用轮换登记"); count != 1 {
		t.Fatalf("软阶段不应再登记轮换，登记 %d 次", count)
	}
	close(env.gate.release)
	if !waitUntil(5*time.Second, func() bool { return env.pool.Draining(w) }) {
		t.Fatalf("接替就绪后 W 应改为普通排空，日志 = %v", serviceLogMessages(env.requests))
	}
	if !waitUntil(time.Second, func() bool {
		return hasLogContaining(env.requests, "WAA Worker 平均调用 | 接替已就绪，不再接新请求，进行中的请求结束后关闭 | 进行中=1")
	}) {
		t.Fatalf("应记录接替就绪，日志 = %v", serviceLogMessages(env.requests))
	}
	if env.unpinnedLease(t, w) {
		t.Fatal("普通排空后 W 不应再接不固定账户的请求")
	}
	pinned, err := env.pool.AcquireFor(env.ctx, aistudio.AccountSelection{AccountID: w, ModelID: testRuntimeModel})
	if err != nil {
		t.Fatalf("指定账户的请求应能用排空中的 W：%v", err)
	}
	_ = pinned.Release()
	env.manager.wakeRetire()
	time.Sleep(600 * time.Millisecond)
	if !slices.Contains(env.manager.WarmAccountIDs(), w) {
		t.Fatal("W 持有租约期间不应关闭")
	}
	release()
	if !waitUntil(5*time.Second, func() bool { return !slices.Contains(env.manager.WarmAccountIDs(), w) }) {
		t.Fatalf("W 的租约释放后应关闭，日志 = %v", serviceLogMessages(env.requests))
	}
	if !waitUntil(time.Second, func() bool {
		return hasLogContaining(env.requests, "WAA Worker 平均调用轮换 | 今日发送=120 | 登记后=", "Worker=8/8")
	}) {
		t.Fatalf("应记录轮换关闭，日志 = %v", serviceLogMessages(env.requests))
	}
	time.Sleep(500 * time.Millisecond)
	if got := sortedWarm(env.manager); len(got) != 8 || slices.Contains(got, w) {
		t.Fatalf("驻留数应回到 8、不重复补，实际 %v", got)
	}
	if peak := stopPeak(); peak > 8+limit {
		t.Fatalf("浏览器总数最多为 峰值 + drainLimit = %d，实际 %d", 8+limit, peak)
	}
	if env.pool.Draining(w) || env.manager.drainOf(w) != nil || env.manager.retirementCount() != 0 {
		t.Fatal("关闭后不应留下排空或登记")
	}
	stats := env.manager.balancedRotationStats(time.Now())
	if stats.lastHour != 1 || stats.today != 1 || stats.rotating[aistudio.PoolScopeNormal] != 0 {
		t.Fatalf("轮换计数不对：%+v", stats)
	}
	if until, ok := env.manager.balancedState.rotation.snapshot(time.Now()).cooloff[w]; !ok || time.Until(until) < 29*time.Minute {
		t.Fatalf("被换下的号应进入 30 分钟冷却期：%v %v", ok, until)
	}
	if normal, _ := env.manager.retirementCounts(env.pool.UltraAccountIDs()); normal != 0 {
		t.Fatalf("轮换登记不计入状态页的待淘汰数，实际 %d", normal)
	}
}

// TestBalancedRotationReplacementFails 接替的号启动失败（非容量原因）：取消轮换，W 从头到尾没有被排空、照常接单；单个接替的号起不来时
// 分区不暂停、下一次判断换下一个备用号，连续 3 个接替的号都起不来（系统性故障）时分区暂停 5 分钟（状态页与日志写暂停原因，
// 同一原因 5 分钟最多一条），暂停结束后换一个没失败过的备用号再登记
func TestBalancedRotationReplacementFails(t *testing.T) {
	env := newRotationEnv(t, 8, 4, 8, 8)
	w := env.residents[0]
	env.seedResidents(t, 120)
	failure := errors.New("测试：浏览器启动失败")
	env.gate.fail.Store(&failure)
	var hardened atomic.Bool
	stop := make(chan struct{})
	watched := make(chan struct{})
	go func() {
		defer close(watched)
		for {
			if env.pool.Draining(w) {
				hardened.Store(true)
			}
			select {
			case <-stop:
				return
			case <-time.After(time.Millisecond):
			}
		}
	}()
	now := time.Now()
	for round := 0; round < balancedSpareFailStreak; round++ {
		env.service.balancedRotate(env.ctx, now.Add(time.Duration(round)*time.Minute))
		if _, registered := env.entry(w); !registered {
			t.Fatalf("第 %d 次判断应登记（前面的失败不让分区暂停），日志 = %v", round+1, serviceLogMessages(env.requests))
		}
		reason := rotationCancelSpareFailed
		if round == balancedSpareFailStreak-1 {
			reason = rotationCancelFailed
		}
		if !waitUntil(5*time.Second, func() bool {
			_, registered := env.entry(w)
			return !registered && logLevelContaining(env.requests, "WARN", "平均调用轮换取消 | 原因="+reason)
		}) {
			t.Fatalf("第 %d 个接替的号启动失败后应以 %q 取消轮换，日志 = %v", round+1, reason, serviceLogMessages(env.requests))
		}
		if stats := env.manager.balancedRotationStats(now); round < balancedSpareFailStreak-1 && stats.paused[aistudio.PoolScopeNormal] != "" {
			t.Fatalf("单个接替的号起不来时分区不应暂停：%v", stats.paused)
		}
	}
	if count := strings.Count(strings.Join(serviceLogMessages(env.requests), "\n"), "替补启动失败 | 原因=平均调用轮换"); count != balancedSpareFailStreak {
		t.Fatalf("应依次换 %d 个不同的接替号，替补启动失败 %d 次", balancedSpareFailStreak, count)
	}
	for _, spare := range env.spares[:balancedSpareFailStreak] {
		if !logSourceContaining(env.requests, spare, "替补启动失败 | 原因=平均调用轮换") {
			t.Fatalf("接替的号 %s 应各试过一次，日志 = %v", spare, serviceLogMessages(env.requests))
		}
	}
	close(stop)
	<-watched
	if hardened.Load() || env.pool.Draining(w) || env.manager.drainOf(w) != nil {
		t.Fatal("替补没有启动时 W 不应被排空")
	}
	if !env.unpinnedLease(t, w) || !slices.Contains(env.manager.WarmAccountIDs(), w) {
		t.Fatal("W 应照常接单")
	}
	env.service.balancedRotate(env.ctx, now.Add(3*time.Minute))
	if stats := env.manager.balancedRotationStats(now); stats.paused[aistudio.PoolScopeNormal] != rotationPauseBackoff {
		t.Fatalf("分区应暂停：%v", stats.paused)
	}
	env.service.balancedRotate(env.ctx, now.Add(4*time.Minute))
	if count := strings.Count(strings.Join(serviceLogMessages(env.requests), "\n"), "平均调用暂停 | 分区=普通 | 原因="+rotationPauseBackoff); count != 1 {
		t.Fatalf("同一原因 5 分钟内只写一条暂停日志，实际 %d", count)
	}
	if _, registered := env.entry(w); registered {
		t.Fatal("退避期间不应再登记")
	}
	env.gate.fail.Store(nil)
	env.service.balancedRotate(env.ctx, now.Add(8*time.Minute))
	if _, registered := env.entry(w); !registered {
		t.Fatalf("退避结束后应再次登记，日志 = %v", serviceLogMessages(env.requests))
	}
	if !waitUntil(5*time.Second, func() bool {
		return logSourceContaining(env.requests, env.spares[balancedSpareFailStreak], "替补启动")
	}) {
		t.Fatalf("暂停结束后应换没失败过的备用号接替，日志 = %v", serviceLogMessages(env.requests))
	}
	if stats := env.manager.balancedRotationStats(now); stats.paused[aistudio.PoolScopeNormal] != "" {
		t.Fatalf("不再暂停时应清掉暂停原因：%v", stats.paused)
	}
}

// TestBalancedRotationRuntimeBusyNotHardened 接替的号的 WAA runtime 被其他进程占用（errAccountRuntimeBusy）：只说明这个号用不了，
// 槽位并没有被别人占去，按接替的号没有启动起来取消（分区不暂停），W 不改为普通排空（改了的话 W 关闭后分区少一个 Worker 却没人补）
func TestBalancedRotationRuntimeBusyNotHardened(t *testing.T) {
	env := newRotationEnv(t, 8, 2, 8, 8)
	w := env.residents[0]
	env.seedResidents(t, 120)
	for _, spare := range env.spares {
		lease, err := aistudio.AcquireAccountRuntimeLease(spare)
		if err != nil {
			t.Fatal(err)
		}
		defer lease.Release()
	}
	env.service.balancedRotate(env.ctx, time.Now())
	if !waitUntil(5*time.Second, func() bool {
		_, registered := env.entry(w)
		return !registered && logLevelContaining(env.requests, "WARN", "平均调用轮换取消 | 原因="+rotationCancelSpareFailed)
	}) {
		t.Fatalf("runtime 被占用时应按接替的号没有启动起来取消（这个号自己的原因，分区不暂停），日志 = %v", serviceLogMessages(env.requests))
	}
	if !hasLogContaining(env.requests, "替补未启动", "原因=平均调用轮换", errAccountRuntimeBusy.Error()) {
		t.Fatalf("应记录替补因 runtime 被占用没有启动，日志 = %v", serviceLogMessages(env.requests))
	}
	if hasLogContaining(env.requests, "接替已就绪") || hasLogContaining(env.requests, "让出的槽位已被请求扩容占用") || env.pool.Draining(w) {
		t.Fatal("runtime 被占用时不应把 W 改为普通排空")
	}
	if !slices.Contains(env.manager.WarmAccountIDs(), w) {
		t.Fatal("W 应保留")
	}
}

// TestBalancedRotationNoRoomHardens 软排空让出的槽位被请求路径的扩容占去（替补按容量已满没有启动，replacementNoRoom）：
// 照样改为普通排空并关闭 W，驻留数回到峰值，不超峰值 + drainLimit
func TestBalancedRotationNoRoomHardens(t *testing.T) {
	env := newRotationEnv(t, 8, 2, 8, 8)
	w := env.residents[0]
	stopPeak := env.watchBrowsers()
	drain := env.manager.startDrain(w, aistudio.PoolScopeNormal, true)
	if drain == nil {
		t.Fatal("应能开始软排空")
	}
	// 请求路径的扩容用掉让出的槽位。容量判断把账户锁正被占用的账号按占用 1 个槽位估计（occupiedWorkersIn），而 watchBrowsers
	// 每 2 毫秒逐个锁一遍全部账号（含备用号）：两者恰好重叠时会多估一个槽位、按容量已满返回（慢机器、-race、少核时偶发，
	// CI 曾因此失败）。这是设计上的暂时性估计，短时间内重试即可
	var expandErr error
	if !waitUntil(5*time.Second, func() bool {
		_, expandErr = env.manager.ensureWorker(withImmediateStartup(env.ctx), env.spares[1], testRuntimeModel, false)
		return expandErr == nil
	}) {
		t.Fatalf("软排空让出槽位后扩容应能启动：%v", expandErr)
	}
	result, spareFault := env.manager.runReplacement(env.ctx, aistudio.PoolScopeNormal, nil, rotationReplacementReason)
	if result != replacementNoRoom || spareFault {
		t.Fatalf("槽位被占去时替补应为 NoRoom、不算挑中的号自己的原因，实际 %d %t", result, spareFault)
	}
	drain.finishReplacement(result)
	if !drain.replacementFailed() {
		t.Fatal("NoRoom 对旧语义仍算替补没有启动")
	}
	if !env.manager.registerRotation(rotationDecision{accountID: w, soft: true, usage: 120}) {
		t.Fatal("应能登记")
	}
	if !waitUntil(5*time.Second, func() bool { return !slices.Contains(env.manager.WarmAccountIDs(), w) }) {
		t.Fatalf("NoRoom 时应改为普通排空并关闭 W，日志 = %v", serviceLogMessages(env.requests))
	}
	if !hasLogContaining(env.requests, "WAA Worker 平均调用 | 让出的槽位已被请求扩容占用，不再接新请求") {
		t.Fatalf("应记录槽位被占去，日志 = %v", serviceLogMessages(env.requests))
	}
	time.Sleep(500 * time.Millisecond)
	if got := len(env.manager.WarmAccountIDs()); got != 8 {
		t.Fatalf("驻留数应回到峰值 8，实际 %d", got)
	}
	if peak := stopPeak(); peak > 8+env.manager.drainLimit(aistudio.PoolScopeNormal) {
		t.Fatalf("浏览器数 %d 超过 峰值 + drainLimit", peak)
	}
}

// TestBalancedRotationDrainTimeout 普通排空后 W 一直有指定账户的请求：排空超过排空超时时放弃，不重试，W 恢复接单并进入 30 分钟冷却期，
// 之后的判断不再选它
func TestBalancedRotationDrainTimeout(t *testing.T) {
	env := newRotationEnv(t, 8, 2, 8, 8)
	w := env.residents[0]
	env.seedResidents(t, 120)
	release := holdLease(t, env.pool, w)
	defer release()
	now := time.Now()
	env.service.balancedRotate(env.ctx, now)
	if !waitUntil(5*time.Second, func() bool { return env.pool.Draining(w) }) {
		t.Fatalf("接替就绪后应改为普通排空，日志 = %v", serviceLogMessages(env.requests))
	}
	drain := env.manager.drainOf(w)
	drain.startedAt.Store(time.Now().Add(-env.manager.drainTimeoutFor(w) - time.Second).UnixNano())
	env.manager.wakeRetire()
	if !waitUntil(5*time.Second, func() bool {
		_, registered := env.entry(w)
		return !registered && !env.pool.Draining(w)
	}) {
		t.Fatal("排空超时后应放弃轮换并恢复接单")
	}
	if !waitUntil(time.Second, func() bool {
		return logLevelContaining(env.requests, "WARN", "平均调用轮换取消 | 原因=排空超时，30 分钟内不再换下本号")
	}) {
		t.Fatalf("应记录排空超时取消，日志 = %v", serviceLogMessages(env.requests))
	}
	if _, ok := env.manager.balancedState.rotation.snapshot(time.Now()).cooloff[w]; !ok {
		t.Fatal("W 应进入冷却期")
	}
	env.service.balancedRotate(env.ctx, now.Add(5*time.Minute))
	time.Sleep(300 * time.Millisecond)
	if _, registered := env.entry(w); registered || env.pool.Draining(w) {
		t.Fatal("冷却期内不应再换下 W")
	}
	if !slices.Contains(env.manager.WarmAccountIDs(), w) {
		t.Fatal("W 应保留")
	}
}

// TestBalancedRotationHotOff 运行中关闭平均调用：还没开始的（空闲直接换的登记、软排空中替补还在启动的）撤销，W 恢复如初；
// 接替已就绪、已在普通排空的照常完成（否则分区多出一个 Worker）
func TestBalancedRotationHotOff(t *testing.T) {
	t.Run("软排空替补还在启动", func(t *testing.T) {
		env := newRotationEnv(t, 8, 2, 8, 8)
		w := env.residents[0]
		env.seedResidents(t, 120)
		env.gate.hold.Store(true)
		env.service.balancedRotate(env.ctx, time.Now())
		if !waitUntil(5*time.Second, func() bool { return env.gate.waiting.Load() == 1 }) {
			t.Fatal("接替的号应开始冷启动")
		}
		env.manager.setBalanced(false)
		env.manager.wakeRetire()
		if !waitUntil(5*time.Second, func() bool {
			_, registered := env.entry(w)
			return !registered && env.manager.drainOf(w) == nil
		}) {
			t.Fatal("关闭后还在软阶段的轮换应撤销")
		}
		// 日志在撤销之后写
		if !waitUntil(time.Second, func() bool {
			return logLevelContaining(env.requests, "WARN", "平均调用轮换取消 | 原因=已关闭平均调用")
		}) || env.pool.Draining(w) {
			t.Fatalf("应记录取消且 W 没有被排空，日志 = %v", serviceLogMessages(env.requests))
		}
		close(env.gate.release)
		time.Sleep(300 * time.Millisecond)
		if !slices.Contains(env.manager.WarmAccountIDs(), w) {
			t.Fatal("撤销后 W 应保留")
		}
	})
	t.Run("空闲直接换还没开始", func(t *testing.T) {
		env := newRotationEnv(t, 4, 2, 4, 4)
		w := env.residents[0]
		release := holdLease(t, env.pool, w)
		defer release()
		if !env.manager.registerRotation(rotationDecision{accountID: w, usage: 120}) {
			t.Fatal("应能登记")
		}
		time.Sleep(300 * time.Millisecond)
		if _, registered := env.entry(w); !registered {
			t.Fatal("忙碌时空闲直接换的登记应等待")
		}
		env.manager.setBalanced(false)
		env.manager.wakeRetire()
		if !waitUntil(5*time.Second, func() bool { _, registered := env.entry(w); return !registered }) {
			t.Fatal("关闭后还没开始的轮换应撤销")
		}
		release()
		time.Sleep(300 * time.Millisecond)
		if !slices.Contains(env.manager.WarmAccountIDs(), w) {
			t.Fatal("撤销后 W 应保留")
		}
	})
	t.Run("已在普通排空", func(t *testing.T) {
		env := newRotationEnv(t, 8, 2, 8, 8)
		w := env.residents[0]
		env.seedResidents(t, 120)
		release := holdLease(t, env.pool, w)
		defer release()
		env.service.balancedRotate(env.ctx, time.Now())
		if !waitUntil(5*time.Second, func() bool { return env.pool.Draining(w) }) {
			t.Fatal("接替就绪后应改为普通排空")
		}
		env.manager.setBalanced(false)
		env.manager.wakeRetire()
		time.Sleep(600 * time.Millisecond)
		if _, registered := env.entry(w); !registered || !env.pool.Draining(w) {
			t.Fatal("已在普通排空的轮换关闭平均调用后应照常完成")
		}
		release()
		if !waitUntil(5*time.Second, func() bool { return !slices.Contains(env.manager.WarmAccountIDs(), w) }) {
			t.Fatal("W 的请求结束后应关闭")
		}
		time.Sleep(300 * time.Millisecond)
		if got := len(env.manager.WarmAccountIDs()); got != 8 {
			t.Fatalf("驻留数应为 8，实际 %d", got)
		}
	})
}

// TestBalancedRotationTakenOver 轮换中途账号变成死槽位或不可调度：registerRetirement 转成冷却登记（软排空立即改为普通排空，
// 之后按冷却淘汰），registerUnschedulable 转成不可调度登记（恢复可调度后不会再继续一次过时的轮换），都写“冷却登记接管”
func TestBalancedRotationTakenOver(t *testing.T) {
	t.Run("冷却登记", func(t *testing.T) {
		env := newRotationEnv(t, 8, 2, 8, 8)
		w := env.residents[0]
		env.seedResidents(t, 120)
		release := holdLease(t, env.pool, w)
		defer release()
		env.gate.hold.Store(true)
		env.service.balancedRotate(env.ctx, time.Now())
		if !waitUntil(5*time.Second, func() bool { return env.gate.waiting.Load() == 1 }) {
			t.Fatal("接替的号应开始冷启动")
		}
		env.retirePro(t, w)
		entry, ok := env.entry(w)
		if !ok || entry.rotation || entry.rotationSoft || !slices.Equal(entry.models, []string{drainProModel}) {
			t.Fatalf("应转成主用模型的冷却登记：%+v", entry)
		}
		if !logLevelContaining(env.requests, "WARN", "平均调用轮换取消 | 原因=冷却登记接管") {
			t.Fatalf("应记录接管，日志 = %v", serviceLogMessages(env.requests))
		}
		if !waitUntil(5*time.Second, func() bool { return env.pool.Draining(w) }) {
			t.Fatal("冷却登记接管后软排空应立即改为普通排空")
		}
		if normal, _ := env.manager.retirementCounts(env.pool.UltraAccountIDs()); normal != 1 {
			t.Fatalf("接管后计入状态页的待淘汰数，实际 %d", normal)
		}
		close(env.gate.release)
		release()
		if !waitUntil(5*time.Second, func() bool { return !slices.Contains(env.manager.WarmAccountIDs(), w) }) {
			t.Fatal("W 应按冷却淘汰关闭")
		}
		if !waitUntil(time.Second, func() bool { return hasLogContaining(env.requests, "冷却淘汰", "范围="+drainProModel) }) {
			t.Fatalf("应按冷却淘汰记录，日志 = %v", serviceLogMessages(env.requests))
		}
	})
	t.Run("不可调度登记", func(t *testing.T) {
		env := newRotationEnv(t, 4, 2, 4, 4)
		w := env.residents[0]
		release := holdLease(t, env.pool, w)
		defer release()
		if !env.manager.registerRotation(rotationDecision{accountID: w, usage: 120}) {
			t.Fatal("应能登记")
		}
		if registered, added := env.manager.registerUnschedulable(w, "测试原因"); !registered || !added {
			t.Fatal("应改为不可调度登记")
		}
		entry, ok := env.entry(w)
		if !ok || entry.rotation || entry.unschedulable == "" {
			t.Fatalf("应转成不可调度登记：%+v", entry)
		}
		if !logLevelContaining(env.requests, "WARN", "平均调用轮换取消 | 原因=冷却登记接管") {
			t.Fatalf("应记录接管，日志 = %v", serviceLogMessages(env.requests))
		}
		// 账号其实可调度：淘汰循环撤销不可调度登记，没有冷却，放弃，不会继续轮换
		env.manager.wakeRetire()
		if !waitUntil(5*time.Second, func() bool { _, registered := env.entry(w); return !registered }) {
			t.Fatal("恢复可调度后应放弃登记")
		}
		release()
		time.Sleep(300 * time.Millisecond)
		if !slices.Contains(env.manager.WarmAccountIDs(), w) {
			t.Fatal("W 不应被关闭")
		}
	})
	t.Run("取消不误删接管后的登记", func(t *testing.T) {
		// 淘汰循环按一轮开始时的副本处理轮换登记，这期间被冷却登记接管时，按副本的取消不能把冷却登记删掉
		env := newRotationEnv(t, 4, 2, 4, 4)
		w := env.residents[0]
		release := holdLease(t, env.pool, w)
		defer release()
		if !env.manager.registerRotation(rotationDecision{accountID: w, usage: 120}) {
			t.Fatal("应能登记")
		}
		env.coolPro(t, w)
		if registered, added := env.manager.registerRetirement(w, drainProModel); !registered || !added {
			t.Fatal("应转成冷却登记")
		}
		env.manager.cancelRotation(w, rotationCancelStale)
		if entry, ok := env.entry(w); !ok || entry.rotation || !slices.Equal(entry.models, []string{drainProModel}) {
			t.Fatalf("接管后的冷却登记应保留：%v %+v", ok, entry)
		}
		if hasLogContaining(env.requests, "原因="+rotationCancelStale) {
			t.Fatal("没有取消时不应写取消日志")
		}
		if env.manager.dropRotation(w) {
			t.Fatal("不是轮换登记时 dropRotation 不应删除")
		}
	})
	t.Run("setUnschedulableReason", func(t *testing.T) {
		env := newRotationEnv(t, 4, 2, 4, 4)
		w := env.residents[0]
		release := holdLease(t, env.pool, w)
		defer release()
		if !env.manager.registerRotation(rotationDecision{accountID: w, usage: 120}) {
			t.Fatal("应能登记")
		}
		if env.manager.setUnschedulableReason(w, "") {
			t.Fatal("可调度时不应转换")
		}
		if !env.manager.setUnschedulableReason(w, "测试原因") {
			t.Fatal("不可调度时应转换")
		}
		if entry, _ := env.entry(w); entry.rotation {
			t.Fatal("转换后不应再是轮换登记")
		}
	})
}

// TestBalancedRotationStale 登记后一直没有开始（空闲直接换的号一直忙）：超过 balancedRegistrationTTL 放弃；
// 软排空中替补超过 balancedSoftTTL 仍没有结果时按没有启动处理
func TestBalancedRotationStale(t *testing.T) {
	t.Run("迟迟没有开始", func(t *testing.T) {
		env := newRotationEnv(t, 4, 2, 4, 4)
		w := env.residents[0]
		release := holdLease(t, env.pool, w)
		defer release()
		if !env.manager.registerRotation(rotationDecision{accountID: w, usage: 120}) {
			t.Fatal("应能登记")
		}
		env.manager.retireMu.Lock()
		env.manager.retirements[w].since = time.Now().Add(-balancedRegistrationTTL - time.Second)
		env.manager.retireMu.Unlock()
		env.manager.wakeRetire()
		if !waitUntil(5*time.Second, func() bool { _, registered := env.entry(w); return !registered }) {
			t.Fatal("超过登记时限应放弃")
		}
		if !waitUntil(time.Second, func() bool {
			return logLevelContaining(env.requests, "WARN", "平均调用轮换取消 | 原因=迟迟没有开始")
		}) {
			t.Fatalf("应记录取消，日志 = %v", serviceLogMessages(env.requests))
		}
	})
	t.Run("软阶段超时", func(t *testing.T) {
		env := newRotationEnv(t, 8, 2, 8, 8)
		w := env.residents[0]
		env.seedResidents(t, 120)
		env.gate.hold.Store(true)
		defer close(env.gate.release)
		env.service.balancedRotate(env.ctx, time.Now())
		if !waitUntil(5*time.Second, func() bool { return env.gate.waiting.Load() == 1 }) {
			t.Fatal("接替的号应开始冷启动")
		}
		env.manager.drainOf(w).startedAt.Store(time.Now().Add(-env.manager.balancedSoftTTL() - time.Second).UnixNano())
		env.manager.wakeRetire()
		if !waitUntil(5*time.Second, func() bool { _, registered := env.entry(w); return !registered }) {
			t.Fatal("软阶段超时应按替补没有启动处理")
		}
		if !waitUntil(time.Second, func() bool {
			return logLevelContaining(env.requests, "WARN", "平均调用轮换取消 | 原因=替补没有启动")
		}) || env.pool.Draining(w) {
			t.Fatalf("应取消且 W 没有被排空，日志 = %v", serviceLogMessages(env.requests))
		}
		if stats := env.manager.balancedRotationStats(time.Now()); len(stats.paused) != 0 {
			t.Fatalf("暂停原因只在判断时写：%v", stats.paused)
		}
		env.service.balancedRotate(env.ctx, time.Now().Add(2*time.Minute))
		if stats := env.manager.balancedRotationStats(time.Now()); stats.paused[aistudio.PoolScopeNormal] != rotationPauseBackoff {
			t.Fatalf("软阶段超时后分区应退避：%v", stats.paused)
		}
	})
}

// TestBalancedRotationDirectPath 排空名额不够（峰值 4、drainLimit 1，同默认的 Ultra 分区）时只换空闲的号：空闲的 W 直接关闭，
// 由替补（常驻数低于驻留数时）或预热（低于常驻数时）补上今天用得最少的号；忙碌的 W 不登记
func TestBalancedRotationDirectPath(t *testing.T) {
	for _, test := range []struct {
		name       string
		warmTarget int
		reason     string
	}{
		{name: "替补补位", warmTarget: 2, reason: "替补启动"},
		{name: "预热补位", warmTarget: 4, reason: "预热"},
	} {
		t.Run(test.name, func(t *testing.T) {
			env := newRotationEnv(t, 4, 2, test.warmTarget, 4)
			w := env.residents[0]
			if env.manager.drainLimit(aistudio.PoolScopeNormal) != 1 {
				t.Fatal("峰值 4 时 drainLimit 应为 1")
			}
			env.seedResidents(t, 120)
			seedBalancedUsage(t, env.manager, map[string]uint32{env.spares[0]: 20})
			stopPeak := env.watchBrowsers()
			release := holdLease(t, env.pool, w)
			now := time.Now()
			env.service.balancedRotate(env.ctx, now)
			if _, registered := env.entry(w); registered {
				t.Fatal("排空名额不够时忙碌的 W 不应登记")
			}
			release()
			env.service.balancedRotate(env.ctx, now.Add(time.Minute))
			if !hasLogContaining(env.requests, "WAA Worker 平均调用轮换登记", "路径=空闲直接换", "接替今日=0") {
				t.Fatalf("空闲的 W 应登记空闲直接换，日志 = %v", serviceLogMessages(env.requests))
			}
			if !waitUntil(5*time.Second, func() bool {
				warm := env.manager.WarmAccountIDs()
				return !slices.Contains(warm, w) && len(warm) == 4
			}) {
				t.Fatalf("W 关闭后应补齐到 4，驻留 Worker = %v，日志 = %v", sortedWarm(env.manager), serviceLogMessages(env.requests))
			}
			if !slices.Contains(env.manager.WarmAccountIDs(), env.spares[1]) {
				t.Fatalf("应补今天用得最少的 %s，驻留 Worker = %v", env.spares[1], sortedWarm(env.manager))
			}
			if test.reason == "替补启动" && !hasLogContaining(env.requests, "替补启动", "原因=平均调用轮换") {
				t.Fatalf("应由替补补位，日志 = %v", serviceLogMessages(env.requests))
			}
			if test.reason == "预热" && hasLogContaining(env.requests, "替补启动", "原因=平均调用轮换") {
				t.Fatalf("低于常驻数时应由预热补位、不启替补，日志 = %v", serviceLogMessages(env.requests))
			}
			time.Sleep(300 * time.Millisecond)
			if got := len(env.manager.WarmAccountIDs()); got != 4 {
				t.Fatalf("驻留数应为 4，实际 %d", got)
			}
			if peak := stopPeak(); peak > 4+1 {
				t.Fatalf("浏览器数 %d 超过 峰值 + drainLimit", peak)
			}
		})
	}
}

// TestBalancedRotationSkipsStrongerResident 被换下的号只在能服务的主用、热门模型不比接替的号多的驻留号里挑（峰值 4、drainLimit 1，空闲直接换）：
// 驻留号都能服务一个热门模型、备用号都不能时不轮换（暂停原因写明），否则用得最多的号关掉后，补位的替补按“能服务的模型多的优先”又把它拉回来，
// 白关一次、白冷启动一次；有一个驻留号同样不能服务热门模型时换它，补位的是今天用得少的备用号，用得最多的号留着
func TestBalancedRotationSkipsStrongerResident(t *testing.T) {
	env := newRotationEnv(t, 4, 2, 2, 4)
	// 驻留号都刚用过：驻留数超出常驻数，空闲超过 5 分钟的号交给空闲回收、不作为候选
	for _, accountID := range env.residents {
		holdLease(t, env.pool, accountID)()
	}
	const hot = "rot-hot-model"
	withHot := []aistudio.Model{
		{ID: testRuntimeModel, Methods: []string{"generateContent"}, Capabilities: map[string]bool{"chat_model": true}},
		{ID: drainProModel, Methods: []string{"generateContent"}, Capabilities: map[string]bool{"chat_model": true}},
		{ID: hot, Methods: []string{"generateContent"}, Capabilities: map[string]bool{"chat_model": true}},
	}
	for _, accountID := range env.residents {
		if err := env.pool.SetCatalog(accountID, aistudio.BenefitTierFree, withHot); err != nil {
			t.Fatal(err)
		}
	}
	env.manager.setHotModels([]string{hot})
	w := env.residents[0]
	env.seedResidents(t, 120)
	now := time.Now()
	env.service.balancedRotate(env.ctx, now)
	if _, registered := env.entry(w); registered {
		t.Fatalf("备用号都服务不了热门模型时不应换下 %s，日志 = %v", w, serviceLogMessages(env.requests))
	}
	if stats := env.manager.balancedRotationStats(now); stats.paused[aistudio.PoolScopeNormal] != rotationPauseWeakerSpare {
		t.Fatalf("暂停原因应为 %q，实际 %v", rotationPauseWeakerSpare, stats.paused)
	}
	// 第二个驻留号的目录里没有热门模型（与备用号同档），今日发送 100：换它
	second := env.residents[1]
	if err := env.pool.SetCatalog(second, aistudio.BenefitTierFree, withHot[:2]); err != nil {
		t.Fatal(err)
	}
	seedBalancedUsage(t, env.manager, map[string]uint32{second: 100})
	env.service.balancedRotate(env.ctx, now.Add(time.Minute))
	if !hasLogContaining(env.requests, "WAA Worker 平均调用轮换登记", "今日发送=100", "路径=空闲直接换") {
		t.Fatalf("应换下与备用号同档的 %s，日志 = %v", second, serviceLogMessages(env.requests))
	}
	if !waitUntil(5*time.Second, func() bool {
		warm := env.manager.WarmAccountIDs()
		return !slices.Contains(warm, second) && len(warm) == 4
	}) {
		t.Fatalf("%s 关闭后应补齐到 4，驻留 Worker = %v，日志 = %v", second, sortedWarm(env.manager), serviceLogMessages(env.requests))
	}
	time.Sleep(200 * time.Millisecond)
	warm := env.manager.WarmAccountIDs()
	if slices.Contains(warm, second) || !slices.Contains(warm, w) || !slices.ContainsFunc(env.spares, func(spare string) bool { return slices.Contains(warm, spare) }) {
		t.Fatalf("补位的应是备用号、被换下的号不应被拉回来、用得最多的号留着，驻留 Worker = %v", sortedWarm(env.manager))
	}
}

// TestBalancedRotationLeavesDrainTotalForDeadSlots 轮换的旧号转为普通排空后一直有指定账户或文件绑定的请求：超过 drainSlotTimeout 不再占名额，
// 但仍计入排空总数与让出额度。先启后停要求排空总数低于 drainLimit（让出额度没有封顶）：轮换的排空最多 drainLimit 个，之后忙碌的号
// 不再先启后停（都忙时暂停），随后在主用模型上冷却的忙碌驻留号照样能排空。原先轮换的排空能占满总数，死槽位排空不了、替补也启动不了，
// 最长约 1 小时；之后又只看名额与总数，让出额度封顶时照样先启后停，接替的号必然因槽位已满起不来，旧号却被改为普通排空
func TestBalancedRotationLeavesDrainTotalForDeadSlots(t *testing.T) {
	env := newRotationEnv(t, 8, 8, 8, 8)
	total := env.manager.drainTotalLimit(aistudio.PoolScopeNormal)
	if env.manager.drainLimit(aistudio.PoolScopeNormal) != 2 || total != 4 {
		t.Fatal("峰值 8 时 drainLimit 应为 2、排空总数上限为 4")
	}
	var releases []func()
	defer func() {
		for _, release := range releases {
			release()
		}
	}()
	held := map[string]bool{}
	fake := time.Now().Add(time.Hour)
	for round := 0; round < total+1; round++ {
		counts := map[string]uint32{}
		for index, accountID := range sortedWarm(env.manager) {
			counts[accountID] = uint32(500 + 100*round - index)
		}
		seedBalancedUsage(t, env.manager, counts)
		before := env.manager.drainSnapshot()
		// 每个驻留号都持有一个指定账户的租约（文件绑定的对话一直落在它上面），就绪时间挪到判断时刻的 10 分钟前
		for _, accountID := range env.manager.ReadyWarmAccountIDs() {
			if _, draining := before[accountID]; !draining && !held[accountID] {
				held[accountID] = true
				releases = append(releases, holdLease(t, env.pool, accountID))
			}
		}
		fake = fake.Add(10 * time.Minute)
		for _, accountID := range env.manager.WarmAccountIDs() {
			env.manager.mu.RLock()
			env.manager.accounts[accountID].readyAt.Store(fake.Add(-10 * time.Minute).UnixNano())
			env.manager.mu.RUnlock()
		}
		env.service.balancedRotate(env.ctx, fake)
		// 这一轮新登记的轮换（之前几轮的旧号仍在普通排空、登记还在）
		rotating := ""
		for _, accountID := range env.manager.WarmAccountIDs() {
			if _, draining := before[accountID]; draining {
				continue
			}
			if entry, ok := env.entry(accountID); ok && entry.rotation {
				rotating = accountID
			}
		}
		if rotating == "" {
			break
		}
		// 等它转为普通排空（接替就绪或让出的槽位被占去），再把所有排空的开始时间挪到 drainSlotTimeout 之前：不再占名额，仍在排空
		if !waitUntil(5*time.Second, func() bool {
			env.manager.wakeRetire()
			drain := env.manager.drainOf(rotating)
			return drain != nil && !drain.soft.Load()
		}) {
			t.Fatalf("第 %d 轮：%s 应转为普通排空，日志 = %v", round, rotating, serviceLogMessages(env.requests))
		}
		for _, drain := range env.manager.drainSnapshot() {
			drain.startedAt.Store(time.Now().Add(-env.manager.drainSlotTimeout() - time.Second).UnixNano())
		}
	}
	drains, limit := env.manager.drainCount(aistudio.PoolScopeNormal), env.manager.drainLimit(aistudio.PoolScopeNormal)
	if drains != limit || env.manager.drainSlotCount(aistudio.PoolScopeNormal) != 0 {
		t.Fatalf("轮换的排空应停在 drainLimit = %d 个（让出额度封顶，再软排空不会多让出槽位），实际 %d", limit, drains)
	}
	if hasLogContaining(env.requests, "让出的槽位已被请求扩容占用") || hasLogContaining(env.requests, "替补未启动") {
		t.Fatalf("让出额度封顶时不应再先启后停（接替的号必然因槽位已满起不来），日志 = %v", serviceLogMessages(env.requests))
	}
	if stats := env.manager.balancedRotationStats(fake); stats.paused[aistudio.PoolScopeNormal] != rotationPauseNoRoom {
		t.Fatalf("驻留号都在忙、让出额度封顶时应暂停 %q，实际 %v", rotationPauseNoRoom, stats.paused)
	}
	// 一个忙碌的驻留号在主用模型上冷却：照样排空
	var dead string
	current := env.manager.drainSnapshot()
	for _, accountID := range env.manager.ReadyWarmAccountIDs() {
		if _, draining := current[accountID]; !draining && held[accountID] {
			dead = accountID
			break
		}
	}
	if dead == "" {
		t.Fatal("应还有忙碌、没在排空的驻留号")
	}
	env.retirePro(t, dead)
	if !waitUntil(5*time.Second, func() bool { return env.manager.drainOf(dead) != nil }) {
		t.Fatalf("死槽位 %s 应能排空（排空 %d/%d），日志 = %v", dead, env.manager.drainCount(aistudio.PoolScopeNormal), total,
			serviceLogMessages(env.requests))
	}
}

// TestRetirementBlocksRotation 非轮换登记只有马上要用排空名额或冷启动名额时才让分区暂停轮换：没到重试时间的不让；空闲的让（这一轮就关闭、
// 随后补位）；忙碌且已在排空的只在替补还在启动时让；忙碌、没有排空的只有可以排空的冷却登记让，只在其他模型上冷却的、不可调度的不让
func TestRetirementBlocksRotation(t *testing.T) {
	now := time.Now()
	deadModels := []string{drainProModel}
	drainWith := func(result int32) *drainState {
		drain := &drainState{}
		drain.replacement.Store(result)
		return drain
	}
	other := retirement{models: []string{testRuntimeModel}}
	primary := retirement{models: []string{drainProModel}}
	for _, test := range []struct {
		name          string
		entry         retirement
		unschedulable bool
		drain         *drainState
		busy          bool
		want          bool
	}{
		{name: "空闲的冷却登记", entry: other, want: true},
		{name: "空闲的不可调度登记", entry: retirement{unschedulable: "需要登录"}, unschedulable: true, want: true},
		{name: "没有可接替账号被推迟", entry: retirement{models: []string{drainProModel}, retryAt: now.Add(retireNoSpareBackoff)}, want: false},
		{name: "推迟到期后空闲", entry: retirement{models: []string{drainProModel}, retryAt: now}, want: true},
		{name: "忙碌、在主用模型上冷却（马上排空）", entry: primary, busy: true, want: true},
		{name: "忙碌、全局冷却（马上排空）", entry: retirement{}, busy: true, want: true},
		{name: "忙碌、只在其他模型上冷却（等它空闲）", entry: other, busy: true, want: false},
		{name: "忙碌、不可调度（等它空闲）", entry: retirement{unschedulable: "需要登录"}, unschedulable: true, busy: true, want: false},
		{name: "忙碌、排空中、替补还在启动", entry: primary, busy: true, drain: drainWith(replacementPending), want: true},
		{name: "忙碌、排空中、替补已启动", entry: primary, busy: true, drain: drainWith(replacementStarted), want: false},
		{name: "忙碌、排空中、替补没有启动", entry: primary, busy: true, drain: drainWith(replacementFailed), want: false},
		{name: "空闲、排空中、替补已启动（这一轮就关闭）", entry: primary, drain: drainWith(replacementStarted), want: true},
	} {
		if got := retirementBlocksRotation(test.entry, test.unschedulable, test.drain, test.busy, deadModels, now); got != test.want {
			t.Fatalf("%s：让行 = %t，期望 %t", test.name, got, test.want)
		}
	}
}

// TestBalancedRotateNotStalledByParkedRetirement 一条处理不动的冷却登记（账号一直忙、只在既不是主用也不是热门的模型上冷却：按规则不排空，
// 只能等它空闲）不挡轮换：模拟 30 分钟、每 30 秒一次判断、淘汰循环照常跑，用得最多的驻留号照样登记轮换，不写“等着淘汰”的暂停。
// 原先任何非轮换登记都让整个分区暂停，直到那个号碰巧空闲或冷却到期（每日限额要等到太平洋时间零点）
func TestBalancedRotateNotStalledByParkedRetirement(t *testing.T) {
	env := newRotationEnv(t, 8, 2, 8, 8)
	env.seedResidents(t, 400)
	parked := env.residents[1]
	release := holdLease(t, env.pool, parked)
	defer release()
	until := time.Now().Add(6 * time.Hour)
	if err := env.pool.MarkCooldownIfGeneration(parked, testRuntimeModel, env.pool.ModelAccessGeneration(parked), time.Now(), until, "每日限额"); err != nil {
		t.Fatal(err)
	}
	env.service.retireCooledWorker(parked, testRuntimeModel, aistudio.QuotaCooldown{Until: until, Kind: aistudio.DailyQuotaKind})
	if _, registered := env.entry(parked); !registered {
		t.Fatal("应写下冷却登记")
	}
	now := time.Now()
	registered := false
	for step := 0; step < 60 && !registered; step++ {
		env.manager.retirePass()
		env.service.balancedRotate(env.ctx, now.Add(time.Duration(step)*30*time.Second))
		_, registered = env.entry(env.residents[0])
	}
	if !registered {
		t.Fatalf("处理不动的冷却登记不应挡住轮换，暂停原因 = %v", env.manager.balancedRotationStats(now).paused)
	}
	if entry, ok := env.entry(parked); !ok || entry.rotation {
		t.Fatal("那条冷却登记应原样保留")
	}
	if hasLogContaining(env.requests, "平均调用暂停", rotationPauseRetiring) {
		t.Fatalf("不应写“等着淘汰”的暂停，日志 = %v", serviceLogMessages(env.requests))
	}
}

// TestBalancedRotatePausesForRetirements 分区内有马上要处理的冷却登记时轮换让行：死槽位已在排空、替补还在启动时写暂停原因（状态页）与
// 暂停日志（同一原因 5 分钟最多一条），另一个分区不受影响；替补启动之后不再让行（死槽位只等手上的请求结束），空闲的号照常空闲直接换
func TestBalancedRotatePausesForRetirements(t *testing.T) {
	env := newRotationEnv(t, 8, 2, 8, 8)
	env.seedResidents(t, 120)
	env.gate.hold.Store(true)
	dead := env.residents[1]
	release := holdLease(t, env.pool, dead)
	defer release()
	// 在主用模型上冷却、忙碌：淘汰循环马上排空并启动替补，替补的启动被挡住，一直在启动
	env.retirePro(t, dead)
	if !waitUntil(5*time.Second, func() bool {
		drain := env.manager.drainOf(dead)
		return drain != nil && drain.replacement.Load() == replacementPending && env.gate.waiting.Load() == 1
	}) {
		t.Fatalf("死槽位应在排空、替补在启动，日志 = %v", serviceLogMessages(env.requests))
	}
	now := time.Now()
	env.service.balancedRotate(env.ctx, now)
	env.service.balancedRotate(env.ctx, now.Add(time.Minute))
	if _, registered := env.entry(env.residents[0]); registered {
		t.Fatal("死槽位的替补还在启动时不应轮换")
	}
	if stats := env.manager.balancedRotationStats(now); stats.paused[aistudio.PoolScopeNormal] != rotationPauseRetiring {
		t.Fatalf("暂停原因应为 %q，实际 %v", rotationPauseRetiring, stats.paused)
	}
	message := "平均调用暂停 | 分区=普通 | 原因=" + rotationPauseRetiring
	if count := strings.Count(strings.Join(serviceLogMessages(env.requests), "\n"), message); count != 1 {
		t.Fatalf("5 分钟内只写一条暂停日志，实际 %d", count)
	}
	env.service.balancedRotate(env.ctx, now.Add(5*time.Minute))
	if count := strings.Count(strings.Join(serviceLogMessages(env.requests), "\n"), message); count != 2 {
		t.Fatalf("5 分钟后再写一条暂停日志，实际 %d", count)
	}
	if hasLogContaining(env.requests, "分区=Ultra") {
		t.Fatal("没有 Ultra Worker 时不写 Ultra 分区的暂停")
	}
	status := env.service.balancedOverview(env.pool.QuotaDayUsage(), env.manager.balancedRotationStats(now))
	if status.Normal.PausedReason != rotationPauseRetiring {
		t.Fatalf("状态页应带暂停原因：%+v", status.Normal)
	}
	// 替补启动之后：死槽位只等手上的请求结束，不再让行；排空名额已被它占去一个，只换空闲的号
	env.gate.hold.Store(false)
	close(env.gate.release)
	if !waitUntil(5*time.Second, func() bool { return env.manager.drainOf(dead).replacement.Load() == replacementStarted }) {
		t.Fatal("死槽位的替补应启动")
	}
	// 死槽位的替补启动后分区超出常驻数，空闲超过 5 分钟的号交给空闲回收、不作为候选：W 刚用过（取一个租约再释放），判断用真实时间
	holdLease(t, env.pool, env.residents[0])()
	env.service.balancedRotate(env.ctx, time.Now())
	if !hasLogContaining(env.requests, "WAA Worker 平均调用轮换登记", "路径=空闲直接换") {
		t.Fatalf("替补启动后应照常轮换（空闲直接换），暂停原因 = %v，日志 = %v",
			env.manager.balancedRotationStats(now).paused, serviceLogMessages(env.requests))
	}
}

// TestBalancedSoftDrainFollowsIncludePro 软阶段切换 ULTRA_INCLUDE_PRO：软排空跟着改记到新分区（让出额度、排空名额随之移动，账号仍不标记排空），
// 按排空开始时的分区启动的替补改在新分区挑号
func TestBalancedSoftDrainFollowsIncludePro(t *testing.T) {
	manager, pool, _ := partitionTestManager(t, []string{partitionNormalA, includeProAccount}, []string{partitionUltraA}, 0, 8, 0, 8)
	if err := pool.SetCatalog(includeProAccount, aistudio.BenefitTierPro, []aistudio.Model{{
		ID: testRuntimeModel, Methods: []string{"generateContent"}, Capabilities: map[string]bool{"chat_model": true},
	}}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := manager.promote(ctx, includeProAccount, testRuntimeModel); err != nil {
		t.Fatal(err)
	}
	drain := manager.startDrain(includeProAccount, aistudio.PoolScopeNormal, true)
	if drain == nil {
		t.Fatal("应能开始软排空")
	}
	defer manager.clearDrain(includeProAccount)
	if manager.drainCredit(aistudio.PoolScopeNormal, nil) != 1 || pool.Draining(includeProAccount) {
		t.Fatal("软排空计入普通分区的让出额度，账号不标记排空")
	}
	pool.SetUltraIncludePro(true)
	manager.poolRuleChanged(true)
	if manager.drainPartition(drain) != aistudio.PoolScopeUltra || manager.drainCredit(aistudio.PoolScopeUltra, nil) != 1 ||
		manager.drainCredit(aistudio.PoolScopeNormal, nil) != 0 || manager.drainSlotCount(aistudio.PoolScopeUltra) != 1 {
		t.Fatal("软排空应改记到 Ultra 分区")
	}
	if !drain.soft.Load() || pool.Draining(includeProAccount) {
		t.Fatal("改记分区不改变软排空")
	}
	if normal, ultra := manager.drainCounts(); normal != 0 || ultra != 0 {
		t.Fatalf("软排空不计入状态页的排空数：%d · %d", normal, ultra)
	}
	manager.startReplacement(aistudio.PoolScopeNormal, nil, rotationReplacementReason, drain)
	if !waitUntil(5*time.Second, func() bool { return drain.replacement.Load() != replacementPending }) {
		t.Fatal("替补应结束")
	}
	if drain.replacement.Load() != replacementStarted || !slices.Contains(manager.WarmAccountIDs(), partitionUltraA) ||
		slices.Contains(manager.WarmAccountIDs(), partitionNormalA) {
		t.Fatalf("替补应在 Ultra 分区启动，驻留 Worker = %v", sortedWarm(manager))
	}
	if !manager.hardenDrain(includeProAccount, drain) || !pool.Draining(includeProAccount) || drain.soft.Load() {
		t.Fatal("hardenDrain 应改为普通排空")
	}
	if _, ultra := manager.drainCounts(); ultra != 1 {
		t.Fatal("普通排空计入状态页的排空数")
	}
	if manager.hardenDrain(includeProAccount, &drainState{}) {
		t.Fatal("不是这次的排空时 hardenDrain 应返回假")
	}
}

// TestSoftDrainLeavesSlotForDeadSlots 软排空要给死槽位留一个排空名额与一个排空总数：drainLimit 为 1 时不能软排空，为 2 时只能有一个；
// 排空超过 drainSlotTimeout 不再占名额、仍计入总数与让出额度：全部排空数已到 drainLimit（让出额度封顶）时名额空着、总数 2/4 也不能软排空
// （软排空不会多让出槽位，峰值已满时接替的号必然起不来）；普通排空照常用满名额与总数
func TestSoftDrainLeavesSlotForDeadSlots(t *testing.T) {
	env := newRotationEnv(t, 8, 0, 8, 8)
	if env.manager.startDrain(env.residents[0], aistudio.PoolScopeNormal, true) == nil {
		t.Fatal("名额 2、没有占用时应能软排空")
	}
	if env.manager.startDrain(env.residents[1], aistudio.PoolScopeNormal, true) != nil {
		t.Fatal("软排空不能占最后一个名额")
	}
	if env.manager.startDrain(env.residents[1], aistudio.PoolScopeNormal, false) == nil {
		t.Fatal("死槽位的排空照常能用最后一个名额")
	}
	// 两个排空都超过 drainSlotTimeout：不再占名额，总数 2/4，但让出额度已是 drainLimit 个
	aged := time.Now().Add(-env.manager.drainSlotTimeout() - time.Second).UnixNano()
	for _, drain := range env.manager.drainSnapshot() {
		drain.startedAt.Store(aged)
	}
	if env.manager.drainSlotCount(aistudio.PoolScopeNormal) != 0 || env.manager.drainCredit(aistudio.PoolScopeNormal, nil) != 2 {
		t.Fatal("两个排空都不占名额、让出额度为 2")
	}
	if env.manager.startDrain(env.residents[2], aistudio.PoolScopeNormal, true) != nil {
		t.Fatal("让出额度封顶（排空数已到 drainLimit）时名额空着也不能软排空")
	}
	if env.manager.startDrain(env.residents[2], aistudio.PoolScopeNormal, false) == nil {
		t.Fatal("死槽位的排空照常能排空（总数 3/4）")
	}
	env.manager.drainOf(env.residents[2]).startedAt.Store(aged)
	if env.manager.startDrain(env.residents[3], aistudio.PoolScopeNormal, false) == nil {
		t.Fatal("死槽位的排空照常能用最后一个总数")
	}
	if env.manager.startDrain(env.residents[4], aistudio.PoolScopeNormal, false) != nil {
		t.Fatal("总数 4/4 时不能再排空")
	}
	// 只剩一个不占名额的排空：排空数低于 drainLimit，又能软排空
	for _, accountID := range env.residents[1:4] {
		env.manager.clearDrain(accountID)
	}
	if env.manager.startDrain(env.residents[1], aistudio.PoolScopeNormal, true) == nil {
		t.Fatal("排空数低于 drainLimit、名额空着时应能软排空")
	}
	for _, accountID := range env.residents[:4] {
		env.manager.clearDrain(accountID)
	}
	env.manager.maxActive.Store(4)
	if env.manager.startDrain(env.residents[0], aistudio.PoolScopeNormal, true) != nil {
		t.Fatal("drainLimit 为 1 时不能软排空")
	}
}

// TestBalancedRotationNoSoftPathWhenCreditCapped 分区里已有 drainLimit 个超过 drainSlotTimeout、仍在排空的号（前两次轮换的旧号一直有
// 指定账户或文件绑定的请求，替补都已在跑，浏览器 10 个 = 峰值 + drainLimit）：让出额度已封顶，忙碌的 W 不再先启后停（原先照样先启后停，
// 接替的号必然因槽位已满起不来，W 却被改为普通排空、日志写“让出的槽位已被请求扩容占用”，分区白白少一个接单的 Worker）；
// W 空闲后按空闲直接换登记
func TestBalancedRotationNoSoftPathWhenCreditCapped(t *testing.T) {
	env := newRotationEnv(t, 8, 3, 8, 8)
	w := env.residents[0]
	env.seedResidents(t, 120)
	for _, accountID := range env.residents[1:3] {
		release := holdLease(t, env.pool, accountID)
		defer release()
		drain := env.manager.startDrain(accountID, aistudio.PoolScopeNormal, false)
		if drain == nil {
			t.Fatal("应能排空")
		}
		drain.startedAt.Store(time.Now().Add(-env.manager.drainSlotTimeout() - time.Minute).UnixNano())
		drain.finishReplacement(replacementStarted)
	}
	env.promote(t, env.spares[:2]...)
	if got, credit := len(env.manager.WarmAccountIDs()), env.manager.drainCredit(aistudio.PoolScopeNormal, nil); got != 10 || credit != 2 ||
		env.manager.drainSlotCount(aistudio.PoolScopeNormal) != 0 {
		t.Fatalf("驻留应为 10、让出额度 2、排空名额 0，实际 %d、%d、%d", got, credit, env.manager.drainSlotCount(aistudio.PoolScopeNormal))
	}
	release := holdLease(t, env.pool, w)
	now := time.Now()
	env.service.balancedRotate(env.ctx, now)
	time.Sleep(300 * time.Millisecond)
	if _, registered := env.entry(w); registered || hasLogContaining(env.requests, "路径=先启后停") {
		t.Fatalf("让出额度封顶时忙碌的 W 不应先启后停，日志 = %v", serviceLogMessages(env.requests))
	}
	if env.pool.Draining(w) || env.manager.drainOf(w) != nil || !env.unpinnedLease(t, w) {
		t.Fatal("W 应照常接不固定账户的请求")
	}
	if hasLogContaining(env.requests, "替补未启动") || hasLogContaining(env.requests, "让出的槽位已被请求扩容占用") {
		t.Fatalf("不应启动注定起不来的接替号，日志 = %v", serviceLogMessages(env.requests))
	}
	release()
	env.service.balancedRotate(env.ctx, now.Add(time.Minute))
	if !hasLogContaining(env.requests, "WAA Worker 平均调用轮换登记", "今日发送=120", "路径=空闲直接换") {
		t.Fatalf("W 空闲后应按空闲直接换登记，日志 = %v", serviceLogMessages(env.requests))
	}
}

// TestBalancedSoftDrainNotReplacedOnDemand 软排空期间（峰值 8 已满，W 的槽位让给了正在冷启动的接替号 S），一个请求要在备用号 x 上启动
// 驻留号都服务不了的主用模型、走按需替换：不选软排空中的 W（原先平均调用下同档时今日发送多的先关，正好选中 W，W 的槽位被用了两次：
// S 就绪后驻留 9 个、没有排空可以收回，轮换登记被静默删掉）。S 就绪后 W 照常由轮换关闭，驻留数回到峰值 8，浏览器数不超过 峰值 + drainLimit
func TestBalancedSoftDrainNotReplacedOnDemand(t *testing.T) {
	env := newRotationEnv(t, 8, 2, 8, 8)
	w := env.residents[0]
	// 驻留号的目录只有 testRuntimeModel：请求主用模型时驻留号都服务不了、同档，平均调用下按今日发送数挑淘汰对象
	catalog := []aistudio.Model{{ID: testRuntimeModel, Methods: []string{"generateContent"}, Capabilities: map[string]bool{"chat_model": true}}}
	for _, accountID := range env.residents {
		if err := env.pool.SetCatalog(accountID, aistudio.BenefitTierFree, catalog); err != nil {
			t.Fatal(err)
		}
	}
	env.seedResidents(t, 120)
	// 只扣住 S 的冷启动，x 的冷启动照常完成
	free := map[string]bool{}
	var freeMu sync.Mutex
	release := make(chan struct{})
	var waiting atomic.Int32
	env.manager.launch = func(ctx context.Context, accountID string, _ camoufoxnative.Options) (*aistudio.NativeWorker, error) {
		freeMu.Lock()
		skip := free[accountID]
		freeMu.Unlock()
		if !skip {
			waiting.Add(1)
			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		worker, _ := aistudio.NewStubWorker(accountID, 0)
		return worker, nil
	}
	stopPeak := env.watchBrowsers()
	if !env.manager.registerRotation(rotationDecision{accountID: w, soft: true, usage: 120}) {
		t.Fatal("应能登记")
	}
	if !waitUntil(5*time.Second, func() bool { return waiting.Load() == 1 }) {
		t.Fatalf("S 应开始冷启动，日志 = %v", serviceLogMessages(env.requests))
	}
	if drain := env.manager.drainOf(w); drain == nil || !drain.soft.Load() {
		t.Fatal("W 应在软排空")
	}
	opening := env.manager.OpeningAccountIDs()
	if len(opening) != 1 {
		t.Fatalf("应只有 S 在启动，实际 %v", opening)
	}
	x := env.spares[0]
	if x == opening[0] {
		x = env.spares[1]
	}
	freeMu.Lock()
	free[x] = true
	freeMu.Unlock()
	if _, err := env.manager.ensureWorker(env.ctx, x, drainProModel, false); err != nil {
		t.Fatalf("x 应能按需替换一个服务不了主用模型的空闲 Worker：%v", err)
	}
	if !slices.Contains(env.manager.WarmAccountIDs(), w) {
		t.Fatal("按需替换不应关掉软排空中的 W")
	}
	if drain := env.manager.drainOf(w); drain == nil || !drain.soft.Load() {
		t.Fatal("W 应仍在软排空")
	}
	close(release)
	if !waitUntil(5*time.Second, func() bool {
		env.manager.wakeRetire()
		return !slices.Contains(env.manager.WarmAccountIDs(), w)
	}) {
		t.Fatalf("S 就绪后 W 应由轮换关闭，日志 = %v", serviceLogMessages(env.requests))
	}
	if !waitUntil(time.Second, func() bool { return hasLogContaining(env.requests, "WAA Worker 平均调用轮换 | 今日发送=120") }) {
		t.Fatalf("应记录轮换关闭，日志 = %v", serviceLogMessages(env.requests))
	}
	time.Sleep(300 * time.Millisecond)
	if got := sortedWarm(env.manager); len(got) != 8 || slices.Contains(got, w) || !slices.Contains(got, x) {
		t.Fatalf("驻留数应回到峰值 8（含 x、不含 W），实际 %v", got)
	}
	if peak := stopPeak(); peak > 8+env.manager.drainLimit(aistudio.PoolScopeNormal) {
		t.Fatalf("浏览器数 %d 超过 峰值 + drainLimit", peak)
	}
	if env.manager.retirementCount() != 0 || env.manager.drainCount(aistudio.PoolScopeNormal) != 0 {
		t.Fatal("关闭后不应留下排空或登记")
	}
}

// logSourceContaining 判断是否有来源为 source、包含 fragment 的运行日志
func logSourceContaining(requests *requestRegistry, source string, fragment string) bool {
	requests.mu.Lock()
	defer requests.mu.Unlock()
	for _, entry := range requests.logs {
		if entry.Source == source && strings.Contains(entry.Message, fragment) {
			return true
		}
	}
	return false
}

// TestBalancedRotationBrokenSpareSkipped 一个每次冷启动都失败、今天一次没发过的备用号与一个健康的备用号（今日 10 次）：第一次判断挑中坏号、
// 起不来，轮换取消但分区不暂停；第二次判断时即使预热失败记录的 5 分钟已过，坏号也排在冷启动候选最后，由健康的号完成接替。
// 原先坏号今日发送最少、一直排第一，替补失败后分区退避 5 分钟、与预热失败记录同时到期，每次都选中它，当天整个分区不再轮换
func TestBalancedRotationBrokenSpareSkipped(t *testing.T) {
	env := newRotationEnv(t, 8, 2, 8, 8)
	w := env.residents[0]
	broken, healthy := env.spares[0], env.spares[1]
	var launchMu sync.Mutex
	launches := map[string]int{}
	env.manager.launch = func(ctx context.Context, accountID string, _ camoufoxnative.Options) (*aistudio.NativeWorker, error) {
		launchMu.Lock()
		launches[accountID]++
		launchMu.Unlock()
		if accountID == broken {
			return nil, errors.New("测试：这个号的浏览器每次都起不来")
		}
		worker, _ := aistudio.NewStubWorker(accountID, 0)
		return worker, nil
	}
	launched := func(accountID string) int {
		launchMu.Lock()
		defer launchMu.Unlock()
		return launches[accountID]
	}
	counts := map[string]uint32{w: 200, healthy: 10}
	for _, accountID := range env.residents[1:] {
		counts[accountID] = 150
	}
	seedBalancedUsage(t, env.manager, counts)
	release := holdLease(t, env.pool, w)
	defer release()
	now := time.Now()
	env.service.balancedRotate(env.ctx, now)
	if !waitUntil(5*time.Second, func() bool {
		_, registered := env.entry(w)
		return !registered && logLevelContaining(env.requests, "WARN", "平均调用轮换取消 | 原因="+rotationCancelSpareFailed)
	}) {
		t.Fatalf("坏号起不来时应取消这次轮换，日志 = %v", serviceLogMessages(env.requests))
	}
	if launched(broken) != 1 || launched(healthy) != 0 {
		t.Fatalf("第一次应试坏号：坏号 %d 次，健康号 %d 次", launched(broken), launched(healthy))
	}
	if until := env.manager.balancedState.rotation.snapshot(now).backoff[aistudio.PoolScopeNormal]; until.After(now) {
		t.Fatalf("单个接替的号起不来时分区不应暂停，暂停到 %s", until)
	}
	// 预热失败记录的 5 分钟过去：坏号不再被排除，只靠排序排到最后
	env.manager.warmFailMu.Lock()
	for accountID, at := range env.manager.warmFailures {
		env.manager.warmFailures[accountID] = at.Add(-warmFailureBackoff - 30*time.Second)
	}
	env.manager.warmFailMu.Unlock()
	if len(env.manager.recentWarmFailures(time.Now())) != 0 {
		t.Fatal("预热失败记录应已不再排除坏号")
	}
	env.service.balancedRotate(env.ctx, now.Add(time.Minute))
	if !waitUntil(5*time.Second, func() bool { return env.pool.Draining(w) }) {
		t.Fatalf("第二次判断应由健康的号接替、W 改为普通排空，日志 = %v", serviceLogMessages(env.requests))
	}
	if launched(broken) != 1 || launched(healthy) != 1 || !slices.Contains(env.manager.WarmAccountIDs(), healthy) {
		t.Fatalf("第二次应启动健康的号：坏号 %d 次，健康号 %d 次", launched(broken), launched(healthy))
	}
	if stats := env.manager.balancedRotationStats(now); stats.paused[aistudio.PoolScopeNormal] != "" {
		t.Fatalf("分区不应暂停：%v", stats.paused)
	}
	release()
	if !waitUntil(5*time.Second, func() bool { return !slices.Contains(env.manager.WarmAccountIDs(), w) }) {
		t.Fatalf("W 的租约释放后应关闭，日志 = %v", serviceLogMessages(env.requests))
	}
	if stats := env.manager.balancedRotationStats(time.Now()); stats.today != 1 {
		t.Fatalf("应完成一次轮换：%+v", stats)
	}
}
