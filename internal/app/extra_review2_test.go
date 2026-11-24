package app

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/api"
)

// 补充池第 2 轮复查的回归测试：快照之后关闭的补充池 Worker 不被家分区当成淘汰对象、原槽位重开期间旧 Worker 被关时新 Worker 仍计入补充池、
// 判断容量之后标记被去掉时重新判断、跨号池借来的账号已不是成员时不重放、重放时已不是成员不盖掉原错误、停服清成员缓存、
// 补充池预热排在主预热之后、count_tokens 没有取得租约时不标补充池。账户只用 @example.com

// TestExtraStaleVictimSkipped 补充池 Worker x 在家分区按需替换选号的快照之后被关闭（标记随之去掉、在叠加视图里改记家分区）：
// 选号在账户锁内复核，不把它当成家分区的淘汰对象；按需替换没有别的对象可换时不发布，家分区不超过峰值。
// 从来没有补充池 Worker 时（标记集合为空指针）不复核：快照之后关闭的家分区 Worker 照旧可以选中（与没有补充池时相同）
func TestExtraStaleVictimSkipped(t *testing.T) {
	t.Run("补充池 Worker 关闭后不当家分区的淘汰对象", func(t *testing.T) {
		n0, n1, n2, x := "stale-n0@example.com", "stale-n1@example.com", "stale-n2@example.com", "stale-x@example.com"
		env := newExtraEnv(t, []string{n0, n1, n2, x}, nil, 2, 2, 0, 1, 1, 2)
		env.member(t, aistudio.BenefitTierFree, x)
		env.pass(t)
		if got := env.in(aistudio.PoolScopeExtra); !slices.Equal(got, []string{x}) {
			t.Fatalf("补充池 Worker = %v", got)
		}
		env.promote(t, n0, n1)
		defer occupy(t, env.pool, n0)()
		defer occupy(t, env.pool, n1)()
		if err := env.manager.ResetFor(x, workerStopIdleReclaim); err != nil {
			t.Fatal(err)
		}
		if env.manager.extraOwnedHas(x) || env.workerOf(x) != nil {
			t.Fatal("x 应已关闭且没有标记")
		}
		// 选号快照那一刻的样子：驻留快照里 x 还 warm，叠加视图里 x 已改记普通分区
		account := env.manager.extraAccount(x)
		account.warm.Store(true)
		victim := env.manager.replaceVictimFor(n2, testRuntimeModel, aistudio.PoolScopeNormal)
		ctx, cancel := context.WithTimeout(env.ctx, 300*time.Millisecond)
		_, err := env.manager.promote(ctx, n2, testRuntimeModel)
		cancel()
		account.warm.Store(false)
		if victim == x {
			t.Fatal("快照之后关闭的补充池 Worker 被选为普通分区的淘汰对象")
		}
		if err == nil {
			t.Fatal("普通分区已满、两个 Worker 都忙：按需替换应以容量已满结束")
		}
		if normal := env.in(aistudio.PoolScopeNormal); len(normal) > env.manager.maxActiveFor(aistudio.PoolScopeNormal) {
			t.Fatalf("普通分区 %v 超过峰值 %d", normal, env.manager.maxActiveFor(aistudio.PoolScopeNormal))
		}
		if slices.Contains(env.launches(), n2) {
			t.Fatal("没有空出槽位，不应启动 n2")
		}
	})
	t.Run("没有过补充池 Worker 时照旧", func(t *testing.T) {
		n0, n1, n2 := "stale-off-n0@example.com", "stale-off-n1@example.com", "stale-off-n2@example.com"
		env := newExtraEnv(t, []string{n0, n1, n2}, nil, 2, 2, 0, 1, 0, 1)
		env.promote(t, n0, n1)
		defer occupy(t, env.pool, n0)()
		if err := env.manager.ResetFor(n1, workerStopIdleReclaim); err != nil {
			t.Fatal(err)
		}
		if env.manager.extraOwned.Load() != nil {
			t.Fatal("前提：本进程没有过补充池 Worker")
		}
		account := env.manager.extraAccount(n1)
		account.warm.Store(true)
		victim := env.manager.replaceVictimFor(n2, testRuntimeModel, aistudio.PoolScopeNormal)
		account.warm.Store(false)
		if victim != n1 {
			t.Fatalf("淘汰对象 = %q，期望与没有补充池时相同，快照里的 %s 照旧可选", victim, n1)
		}
	})
}

// TestExtraSlotMovedChecks 淘汰选号的账户锁内复核（extraSlotMoved）：有 Worker 的账户按补充池标记判断分区，标记与要选的分区不一致的不选；
// 没有 Worker 或不在驻留的不选；本进程没有过补充池 Worker 时恒为假
func TestExtraSlotMovedChecks(t *testing.T) {
	n0, x := "moved-n0@example.com", "moved-x@example.com"
	env := newExtraEnv(t, []string{n0, x}, nil, 1, 2, 0, 1, 1, 1)
	env.promote(t, n0)
	home := env.manager.extraAccount(n0)
	moved := func(account *accountWorker, partition aistudio.PoolScope) bool {
		account.mu.Lock()
		defer account.mu.Unlock()
		return env.manager.extraSlotMoved(account, partition)
	}
	if moved(home, aistudio.PoolScopeExtra) || moved(home, aistudio.PoolScopeNormal) {
		t.Fatal("本进程没有过补充池 Worker：恒为假")
	}
	env.member(t, aistudio.BenefitTierFree, x)
	env.pass(t)
	extra := env.manager.extraAccount(x)
	switch {
	case moved(home, aistudio.PoolScopeNormal), !moved(home, aistudio.PoolScopeExtra):
		t.Fatal("家分区的 Worker 只算家分区的候选")
	case moved(extra, aistudio.PoolScopeExtra), !moved(extra, aistudio.PoolScopeNormal):
		t.Fatal("补充池 Worker 只算补充池的候选")
	}
	if err := env.manager.ResetFor(x, workerStopIdleReclaim); err != nil {
		t.Fatal(err)
	}
	if !moved(extra, aistudio.PoolScopeNormal) || !moved(extra, aistudio.PoolScopeExtra) {
		t.Fatal("没有 Worker 的账户不作为候选")
	}
}

// TestExtraOwnSlotReopenKeepsTag 补充池 Worker 的浏览器异常退出、账户原槽位重开（按补充池计、不判断容量）期间，另一条不取独占租约的路径
// （照 prepareUpdate：持有 startupMu 与账户锁）关掉旧 Worker：标记保留到这次启动结束，新 Worker 发布时仍计入补充池，家分区不超过峰值；
// 重开失败时启动结束后去掉标记（账户已不占任何槽位）
func TestExtraOwnSlotReopenKeepsTag(t *testing.T) {
	for _, failing := range []bool{false, true} {
		name := "重开成功"
		if failing {
			name = "重开失败"
		}
		t.Run(name, func(t *testing.T) {
			n0, n1, x := "ownslot-n0@example.com", "ownslot-n1@example.com", "ownslot-x@example.com"
			env := newExtraEnv(t, []string{n0, n1, x}, nil, 2, 2, 0, 1, 1, 2)
			env.member(t, aistudio.BenefitTierFree, x)
			env.pass(t)
			if got := env.in(aistudio.PoolScopeExtra); !slices.Equal(got, []string{x}) {
				t.Fatalf("补充池 Worker = %v", got)
			}
			env.promote(t, n0, n1)
			account := env.manager.extraAccount(x)
			_ = env.workerOf(x).Close()
			if failing {
				env.mu.Lock()
				env.fail[x] = errors.New("浏览器启动失败（测试）")
				env.mu.Unlock()
			}
			account.startupMu.Lock()
			done := make(chan error, 1)
			go func() {
				ctx, cancel := context.WithTimeout(env.ctx, 5*time.Second)
				defer cancel()
				ctx = withExtraStart(withWorkerStartSource(ctx, workerStartReopen), extraStartOptions{stay: true, fallbackHome: true})
				_, err := env.manager.ensureWorker(ctx, x, testRuntimeModel, true)
				done <- err
			}()
			if !waitUntil(2*time.Second, func() bool { _, opening := env.manager.openingSet.Load(x); return opening }) {
				account.startupMu.Unlock()
				t.Fatal("重开没有登记启动占位")
			}
			account.mu.Lock()
			_ = env.manager.closeAccountWorker(account)
			account.mu.Unlock()
			kept := env.manager.extraOwnedHas(x)
			account.startupMu.Unlock()
			err := <-done
			if !kept {
				t.Fatal("启动占位还在时关闭旧 Worker 不应去掉补充池标记")
			}
			if normal := env.in(aistudio.PoolScopeNormal); len(normal) > env.manager.maxActiveFor(aistudio.PoolScopeNormal) {
				t.Fatalf("普通分区 %v 超过峰值 %d", normal, env.manager.maxActiveFor(aistudio.PoolScopeNormal))
			}
			if failing {
				if err == nil {
					t.Fatal("重开应失败")
				}
				if env.manager.extraOwnedHas(x) || env.workerOf(x) != nil {
					t.Fatal("重开失败、账户已不占槽位：应去掉补充池标记")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := env.in(aistudio.PoolScopeExtra); !slices.Equal(got, []string{x}) || !env.manager.extraOwnedHas(x) {
				t.Fatalf("补充池 Worker = %v，期望重开的 %s 仍在补充池", got, x)
			}
		})
	}
}

// TestExtraSelfLockBusy ensureWorker 判断容量时账户自己的锁被占用：本进程有过补充池 Worker 时回到快路径等锁、不按原槽位绕过容量判断；
// 没有过补充池 Worker 时照旧按原槽位（与没有补充池时相同）；锁没被占用时不受影响
func TestExtraSelfLockBusy(t *testing.T) {
	x, other := "selfbusy-x@example.com", "selfbusy-other@example.com"
	env := newExtraEnv(t, []string{x, other}, nil, 0, 2, 0, 1, 0, 1)
	manager := env.manager
	busy := workerOccupancy{lockBusy: map[string]struct{}{x: {}}}
	if manager.extraSelfLockBusy(x, busy) {
		t.Fatal("没有过补充池 Worker：照旧按原槽位")
	}
	manager.rebalanceMu.Lock()
	manager.setExtraOwnedLocked(other, true)
	manager.setExtraOwnedLocked(other, false)
	manager.rebalanceMu.Unlock()
	if !manager.extraSelfLockBusy(x, busy) {
		t.Fatal("有过补充池 Worker、自己的锁被占用：应回到快路径等锁")
	}
	if manager.extraSelfLockBusy(x, workerOccupancy{lockBusy: map[string]struct{}{other: {}}}) || manager.extraSelfLockBusy(x, workerOccupancy{}) {
		t.Fatal("自己的锁没被占用时不受影响")
	}
}

// TestExtraCloseTagOpening 关闭流程与启动登记在 extraMu 内互斥：账户在启动（openingSet）时关闭不去掉标记；没在启动时去掉；
// 标记已被去掉时 storeExtraOpening 不登记启动占位的镜像
func TestExtraCloseTagOpening(t *testing.T) {
	x := "closetag-x@example.com"
	env := newExtraEnv(t, []string{x}, nil, 0, 1, 0, 1, 1, 1)
	manager := env.manager
	manager.rebalanceMu.Lock()
	manager.setExtraOwnedLocked(x, true)
	ok := manager.storeExtraOpening(x)
	manager.rebalanceMu.Unlock()
	if !ok {
		t.Fatal("带标记时应登记")
	}
	manager.clearExtraTagOnClose(x)
	if !manager.extraOwnedHas(x) {
		t.Fatal("账户在启动时关闭流程不应去掉标记")
	}
	manager.openingSet.Delete(x)
	manager.clearExtraTagOnClose(x)
	if manager.extraOwnedHas(x) {
		t.Fatal("账户不在启动时关闭流程应去掉标记")
	}
	manager.rebalanceMu.Lock()
	ok = manager.storeExtraOpening(x)
	manager.rebalanceMu.Unlock()
	if _, opening := manager.openingSet.Load(x); ok || opening {
		t.Fatal("没有标记时不应登记启动占位的镜像")
	}
}

// TestExtraStaleTagRechecked 带残留补充池标记的账户（没有 Worker）按补充池判断了容量、登记启动占位之前标记被关闭流程去掉（这里由冷启动闸门
// 模拟：闸门在判断容量之后、登记之前调用）：重新按叠加视图判断，账户已改记家分区，家分区已满时按容量已满返回，不在家分区多发布一个。
// 空槽路径与补充池已满的替换路径都核对（替换路径的淘汰对象解除预留、没有被关掉）
func TestExtraStaleTagRechecked(t *testing.T) {
	for _, full := range []bool{false, true} {
		name := "补充池有空槽"
		if full {
			name = "补充池已满走替换"
		}
		t.Run(name, func(t *testing.T) {
			n0, n1, x, y := "staletag-n0@example.com", "staletag-n1@example.com", "staletag-x@example.com", "staletag-y@example.com"
			extraMax := 2
			if full {
				extraMax = 1
			}
			env := newExtraEnv(t, []string{n0, n1, x, y}, nil, 2, 2, 0, 1, 0, extraMax)
			if full {
				env.setExtra(true, 1, 1)
				env.member(t, aistudio.BenefitTierFree, y)
				env.pass(t)
				if got := env.in(aistudio.PoolScopeExtra); !slices.Equal(got, []string{y}) {
					t.Fatalf("补充池 Worker = %v", got)
				}
				env.setExtra(true, 0, 1)
			}
			env.promote(t, n0, n1)
			defer occupy(t, env.pool, n0)()
			defer occupy(t, env.pool, n1)()
			env.manager.rebalanceMu.Lock()
			env.manager.setExtraOwnedLocked(x, true)
			env.manager.rebalanceMu.Unlock()
			calls := 0
			gate := coldStartGateFunc(func(accountID, _ string, _ coldStartPath) (func(bool), bool) {
				if accountID == x {
					calls++
					if calls == 1 {
						// 判断容量之后、登记之前，关闭流程去掉了 x 的标记
						env.manager.clearExtraTagOnClose(x)
					}
				}
				return func(bool) {}, true
			})
			env.manager.coldStartGate.Store(&gate)
			ctx, cancel := context.WithTimeout(env.ctx, 2*time.Second)
			defer cancel()
			_, err := env.manager.promote(ctx, x, testRuntimeModel)
			if !errors.Is(err, errAccountWorkerCapacity) {
				t.Fatalf("err = %v，期望家分区已满时按容量已满返回", err)
			}
			if calls == 0 {
				t.Fatal("前提：冷启动闸门应被调用")
			}
			if slices.Contains(env.launches(), x) {
				t.Fatal("标记已去掉、家分区已满：不应启动 x")
			}
			if normal := env.in(aistudio.PoolScopeNormal); len(normal) > env.manager.maxActiveFor(aistudio.PoolScopeNormal) {
				t.Fatalf("普通分区 %v 超过峰值", normal)
			}
			if full {
				if env.workerOf(y) == nil || env.manager.victimReserved(y) {
					t.Fatal("替换路径的淘汰对象应解除预留、没有被关掉")
				}
			}
		})
	}
}

// TestExtraReplayNotMember 默认地址只能由补充池服务的付费模型借到 Pro 成员，Worker 失败重建、要在原账号重放时账号已恢复（不是成员）：
// 不重放，写一行说明并换号，请求不以内部错误 errExtraMemberGone 结束，保留 Worker 失败的原错误
func TestExtraReplayNotMember(t *testing.T) {
	normal, lent := "replaygone-normal@example.com", "replaygone-pro@example.com"
	env := newBorrowEnv(t, []string{normal}, nil, []string{lent}, 1, 1)
	env.cool(t, lent)
	env.promote(t, normal)
	env.lend(t, lent)
	calls := 0
	env.upstream.beforeAccount(lent, func(context.Context, aistudio.GenerateRequest) error {
		calls++
		if calls == 1 {
			env.recover(t, lent)
			return waaRuntimeFailure("GenerateContent")
		}
		return nil
	})
	_, err := env.generate(t, aistudio.PoolScopeNormal, borrowPaidModel, 10*time.Second)
	if errors.Is(err, errExtraMemberGone) {
		t.Fatalf("请求以内部错误 errExtraMemberGone 结束：%v", err)
	}
	var rpc *aistudio.RPCError
	if !errors.As(err, &rpc) {
		t.Fatalf("err = %v（%T），期望保留 Worker 失败的原错误", err, err)
	}
	if got := env.upstream.accounts(0); !slices.Equal(got, []string{lent}) {
		t.Fatalf("上游调用 = %v，期望只有第一次", got)
	}
	if got := countLogsAt(env.requests, "WARN", extraReplaySwitchMessage); got != 1 {
		t.Fatalf("账号已不是成员：应写一行不在原账号重放的说明（实际 %d 行）", got)
	}
}

// TestSelectionErrorReplaces 选号失败时请求记下的错误：没有可用账号一类不盖掉上一次尝试的错误；在原账号重放时借来的账号已不是补充池成员
// （replayAs 判断之后才恢复）同样不盖掉；其他选号错误照旧替换，没有上一次错误时照旧记下
func TestSelectionErrorReplaces(t *testing.T) {
	previous := waaRuntimeFailure("GenerateContent")
	noEligible := aistudio.NewExtraUnavailableError(borrowPaidModel, time.Now())
	other := errors.New("其他选号错误")
	cases := []struct {
		name              string
		err, acquireErr   error
		replayUnavailable bool
		want              bool
	}{
		{"没有可用账号不盖掉", previous, noEligible, false, false},
		{"没有上一次错误时记下", nil, noEligible, false, true},
		{"重放时已不是成员不盖掉", previous, errExtraMemberGone, true, false},
		{"重放时已不是成员、没有上一次错误时记下", nil, errExtraMemberGone, true, true},
		{"不是重放时照旧替换", previous, errExtraMemberGone, false, true},
		{"其他错误照旧替换", previous, other, true, true},
	}
	for _, tc := range cases {
		if got := selectionErrorReplaces(tc.err, tc.acquireErr, tc.replayUnavailable); got != tc.want {
			t.Errorf("%s：%v，期望 %v", tc.name, got, tc.want)
		}
	}
}

// TestExtraStopClearsView 停服（ResetAll）清空补充池的成员缓存：状态接口不再给出停服前的成员数，账户列表不再标 member；
// 补充池关闭时账户列表不读成员缓存（停服期间关闭补充池时不对账，缓存还是关闭之前的）
func TestExtraStopClearsView(t *testing.T) {
	t.Run("停服", func(t *testing.T) {
		ids := extraIDsOf("stopview", 3)
		env := newExtraEnv(t, ids, nil, 0, 3, 0, 1, 1, 1)
		env.member(t, aistudio.BenefitTierFree, ids...)
		env.pass(t)
		if env.manager.extraView.Load() == nil || len(env.in(aistudio.PoolScopeExtra)) != 1 {
			t.Fatal("前提：有成员缓存与一个补充池 Worker")
		}
		if err := env.manager.ResetAll(); err != nil {
			t.Fatal(err)
		}
		slots := env.manager.slotMembers()
		status := env.manager.extraStatus(slots, env.manager.WarmAccountIDs(), env.manager.OpeningAccountIDs(), env.manager.occupiedSlotsApprox(slots), partitionCounts{})
		if status == nil || status.Members != 0 || status.ColdMembers != 0 || status.Free != 0 {
			t.Fatalf("停服后状态 = %+v，期望补充池开启、成员数清零", status)
		}
		accounts := []api.AdminAccount{{ID: ids[0]}, {ID: ids[1]}, {ID: ids[2]}}
		env.manager.markExtraAccounts(accounts)
		for _, account := range accounts {
			if account.Extra != "" {
				t.Fatalf("停服后 %s 仍标 %q", account.ID, account.Extra)
			}
		}
	})
	t.Run("补充池关闭时不读成员缓存", func(t *testing.T) {
		ids := extraIDsOf("offview", 2)
		env := newExtraEnv(t, ids, nil, 0, 2, 0, 1, 0, 1)
		env.member(t, aistudio.BenefitTierFree, ids...)
		env.pass(t)
		accounts := []api.AdminAccount{{ID: ids[0]}}
		env.manager.markExtraAccounts(accounts)
		if accounts[0].Extra != adminAccountExtraMember {
			t.Fatalf("开启时 %s 应标 member，实际 %q", ids[0], accounts[0].Extra)
		}
		env.setExtra(false, 0, 1)
		accounts = []api.AdminAccount{{ID: ids[0]}}
		env.manager.markExtraAccounts(accounts)
		if accounts[0].Extra != "" {
			t.Fatalf("补充池已关闭（还没对账）：%s 仍标 %q", ids[0], accounts[0].Extra)
		}
	})
}

// TestExtraPrewarmAfterMain keepWarmPass 的补充池预热排在主分区补齐预热之后（I8）：主分区低于常驻数、这一轮发起主预热时，
// 补充池这一轮最多发起 1 个冷启动（按主预热在运行计），不按主预热不在运行给 max(1, WSC−1)
func TestExtraPrewarmAfterMain(t *testing.T) {
	members := []string{"after-main-m1@example.com", "after-main-m2@example.com", "after-main-m3@example.com"}
	spares := []string{"after-main-s1@example.com", "after-main-s2@example.com", "after-main-s3@example.com"}
	env := newExtraEnv(t, append(slices.Clone(members), spares...), nil, 3, 4, 0, 1, 3, 3)
	env.manager.warmConcurrency.Store(3)
	env.manager.startupSlots.resized()
	env.member(t, aistudio.BenefitTierFree, members...)
	gate := make(chan struct{})
	env.mu.Lock()
	env.gate = gate
	env.mu.Unlock()
	if !env.manager.prewarmNeeded() {
		t.Fatal("前提：普通分区低于常驻数")
	}
	env.service.keepWarmPass(env.ctx, time.Now())
	env.manager.extraMu.Lock()
	extraLaunched := len(env.manager.extraStarting)
	env.manager.extraMu.Unlock()
	close(gate)
	env.waitLaunches(t)
	env.manager.waitPrewarm()
	if extraLaunched > 1 {
		t.Fatalf("同一轮补充池发起了 %d 个冷启动，期望主预热先拿名额、补充池最多 1 个", extraLaunched)
	}
	if extraLaunched == 0 {
		t.Fatal("前提：补充池这一轮应发起 1 个冷启动")
	}
	if got := len(env.in(aistudio.PoolScopeNormal)); got == 0 {
		t.Fatal("主分区的补齐预热应照常进行")
	}
}

// TestExtraCountTokensNoLeaseNotMarked 默认地址 count_tokens 借用补充池、L 里只有跑不了付费模型的 Free 成员：返回 503 时没有取得任何租约，
// 访问日志与请求日志都不标补充池；借到 Pro 成员成功时照常标补充池
func TestExtraCountTokensNoLeaseNotMarked(t *testing.T) {
	countThrough := func(env *borrowEnv) func(ctx context.Context, request aistudio.TokenCountRequest) (aistudio.TokenCount, error) {
		return func(ctx context.Context, request aistudio.TokenCountRequest) (aistudio.TokenCount, error) {
			lease, err := env.pool.AcquireFor(ctx, aistudio.AccountSelection{
				ModelID: request.Model, Method: "countTokens", Pool: aistudio.PoolScopeFromContext(ctx),
			})
			if err != nil {
				return aistudio.TokenCount{}, err
			}
			_ = lease.Release()
			return aistudio.TokenCount{InputTokens: 1}, nil
		}
	}
	body := `{"model":"` + borrowPaidModel + `","messages":[{"role":"user","content":"hi"}]}`
	t.Run("没有取得租约", func(t *testing.T) {
		normal, freeMember, pro := "countlease-normal-a@example.com", "countlease-normal-b@example.com", "countlease-pro@example.com"
		env := newBorrowEnv(t, []string{normal, freeMember}, nil, []string{pro}, 1, 2)
		env.upstream.count = countThrough(env)
		env.promote(t, normal)
		env.cool(t, freeMember)
		env.lend(t, freeMember)
		handler, admin := borrowHandler(env)
		recorder, entries := postFallbackTo(t, handler, admin, "/v1/messages/count_tokens", body)
		if recorder.Code != http.StatusServiceUnavailable || len(entries) == 0 {
			t.Fatalf("状态 %d、访问日志 %d 条，响应 %s", recorder.Code, len(entries), strings.TrimSpace(recorder.Body.String()))
		}
		entry := entries[len(entries)-1]
		if entry.Extra || requestLogData(entry).Extra {
			t.Fatal("没有取得任何租约的 503 不应标补充池")
		}
	})
	t.Run("借到成员", func(t *testing.T) {
		normal, pro := "countlease-ok-normal@example.com", "countlease-ok-pro@example.com"
		env := newBorrowEnv(t, []string{normal}, nil, []string{pro}, 1, 1)
		env.upstream.count = countThrough(env)
		env.promote(t, normal)
		env.cool(t, pro)
		env.lend(t, pro)
		handler, admin := borrowHandler(env)
		recorder, entries := postFallbackTo(t, handler, admin, "/v1/messages/count_tokens", body)
		if recorder.Code != http.StatusOK || len(entries) == 0 {
			t.Fatalf("状态 %d、访问日志 %d 条，响应 %s", recorder.Code, len(entries), strings.TrimSpace(recorder.Body.String()))
		}
		if !entries[len(entries)-1].Extra {
			t.Fatal("借到补充池成员的 count_tokens 应标补充池")
		}
	})
}
