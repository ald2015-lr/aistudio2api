package app

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// coolModelFor 只在 model 上给账户写入 d 的每日限额冷却（测试账户只启用 Playground 通道）
func coolModelFor(t *testing.T, pool *aistudio.AccountPool, accountID string, model string, d time.Duration) {
	t.Helper()
	if err := pool.MarkCooldownIfGeneration(accountID, model, pool.ModelAccessGeneration(accountID), time.Now(), time.Now().Add(d), "每日限额"); err != nil {
		t.Fatal(err)
	}
}

// holdOtherModelLease 让账户正在服务其他模型（drainProModel）的请求，返回释放函数
func holdOtherModelLease(t *testing.T, pool *aistudio.AccountPool, accountID string) func() {
	t.Helper()
	lease, err := pool.AcquireFor(context.Background(), aistudio.AccountSelection{AccountID: accountID, ModelID: drainProModel, Method: "generateContent"})
	if err != nil {
		t.Fatal(err)
	}
	return func() { _ = lease.Release() }
}

// fullPartitionCooledWarm 准备分区已满的场景：a、b 是驻留 Worker，都在 testRuntimeModel 上冷却 cooldown、又都在服务其他模型的请求
// （不能回收或替换），备用账号 c 没有冷却；返回 a、b 的释放函数
func fullPartitionCooledWarm(t *testing.T, env *drainEnv, accounts []string, cooldown time.Duration) (func(), func()) {
	t.Helper()
	env.promote(t, accounts[0], accounts[1])
	for _, accountID := range accounts[:2] {
		coolModelFor(t, env.pool, accountID, testRuntimeModel, cooldown)
	}
	releaseA := holdOtherModelLease(t, env.pool, accounts[0])
	releaseB := holdOtherModelLease(t, env.pool, accounts[1])
	t.Cleanup(releaseA)
	t.Cleanup(releaseB)
	return releaseA, releaseB
}

var slotWaitAccounts = []string{"slot-a@example.com", "slot-b@example.com", "slot-c@example.com"}

// TestNoSlotWaitsInsteadOfAllCooling 分区已满、热 Worker 都在该模型上冷却且都在服务其他模型，而还有没冷却的备用账号：
// 这是没有空闲的 Worker 槽位，不是全部冷却：请求排队等待（排队进度写明原因），不返回全部冷却的 429；热 Worker 空闲后立即被唤醒，
// 替换空闲的冷却 Worker 启动备用账号取得租约
func TestNoSlotWaitsInsteadOfAllCooling(t *testing.T) {
	env := newDrainEnv(t, slotWaitAccounts, 2, 2)
	releaseA, _ := fullPartitionCooledWarm(t, env, slotWaitAccounts, 5*time.Minute)
	type acquired struct {
		lease *aistudio.AccountLease
		err   error
	}
	result := make(chan acquired, 1)
	ctx, _ := queueProgressContext(env.ctx, "req-no-slot", 0, time.Hour)
	go func() {
		lease, err := env.service.acquireWarmLease(ctx, aistudio.AccountSelection{ModelID: testRuntimeModel, Method: "generateContent"})
		result <- acquired{lease: lease, err: err}
	}()
	select {
	case got := <-result:
		t.Fatalf("有没冷却的备用账号时不应提前返回：lease=%v err=%v", got.lease, got.err)
	case <-time.After(500 * time.Millisecond):
	}
	if waiters := dispatchWaiters(env.service); waiters != 1 {
		t.Fatalf("请求应在调度等待队列中排队，等待者 = %d", waiters)
	}
	if lines := queueProgressLines(env.service, "req-no-slot", "排队等待 | 原因="+waitReasonNoSlot); len(lines) != 1 ||
		!strings.Contains(lines[0].Message, "热忙碌=0 | 备用=1 | 备用忙碌=0") {
		t.Fatalf("排队进度应写明没有空闲的 Worker 槽位与备用账号数：%v", lines)
	}
	releasedAt := time.Now()
	releaseA()
	select {
	case got := <-result:
		if got.err != nil {
			t.Fatalf("热 Worker 空闲后选号失败: %v", got.err)
		}
		defer got.lease.Release()
		if got.lease.Account().ID != slotWaitAccounts[2] {
			t.Fatalf("应启动备用账号 c，实际 %s", got.lease.Account().ID)
		}
		if waited := time.Since(releasedAt); waited >= schedulingRecheck {
			t.Fatalf("热 Worker 空闲后 %s 才取得租约，期望被立即唤醒", waited)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("热 Worker 空闲后没有取得租约")
	}
	if got := sortedWarm(env.manager); !slices.Equal(got, slotWaitAccounts[1:]) {
		t.Fatalf("应替换空闲的冷却 Worker a，驻留 Worker = %v", got)
	}
	if waiters := dispatchWaiters(env.service); waiters != 0 {
		t.Fatalf("选号结束后仍有 %d 个等待者", waiters)
	}
}

// TestPrimaryNoSlotBoundedByBudget 同样的场景下主用模型受排队上限限制：超过 PRIMARY_QUEUE_WAIT 返回号池繁忙（原因为没有空闲的
// Worker 槽位），不返回全部冷却；热 Worker 只剩不到 1 分钟的冷却时也不按冷却等待绕过排队上限
func TestPrimaryNoSlotBoundedByBudget(t *testing.T) {
	for _, test := range []struct {
		name     string
		cooldown time.Duration
	}{
		{name: "冷却超过 1 分钟", cooldown: 5 * time.Minute},
		{name: "冷却不到 1 分钟", cooldown: 30 * time.Second},
	} {
		t.Run(test.name, func(t *testing.T) {
			env := newDrainEnv(t, slotWaitAccounts, 2, 2)
			fullPartitionCooledWarm(t, env, slotWaitAccounts, test.cooldown)
			env.service.setPrimaryModels([]string{testRuntimeModel}, 300*time.Millisecond)
			ctx, cancel := context.WithTimeout(env.ctx, 3*time.Second)
			defer cancel()
			lease, err, elapsed := acquirePrimary(ctx, env.service, aistudio.PoolScopeNormal)
			if lease != nil {
				_ = lease.Release()
			}
			var busy *aistudio.CapacityBusyError
			var cooling *aistudio.AllCoolingError
			if !errors.As(err, &busy) || errors.As(err, &cooling) {
				t.Fatalf("err = %v，期望号池繁忙而不是全部冷却", err)
			}
			if busy.Reason != waitReasonNoSlot {
				t.Fatalf("排队原因 = %q，期望 %q", busy.Reason, waitReasonNoSlot)
			}
			if elapsed < 300*time.Millisecond || elapsed > 2*time.Second {
				t.Fatalf("排队 %s 后返回，期望约 300ms", elapsed)
			}
			if retry := time.Until(busy.RetryAt()); retry < 500*time.Millisecond || retry > 1500*time.Millisecond {
				t.Fatalf("建议重试间隔 = %s，期望约 1s（不是冷却恢复时间）", retry)
			}
			if waiters := dispatchWaiters(env.service); waiters != 0 {
				t.Fatalf("排队超限退出后仍有 %d 个等待者", waiters)
			}
		})
	}
}

// TestAllCoolingStillReturnedWithoutStandby 候选真的全部冷却（没有备用账号，或备用账号也在冷却）时仍立即返回全部冷却，
// 恢复时间为最早恢复的冷却；降级判定的 Playground 优先选号在有备用账号时同样返回全部冷却，调用方随即改为不限通道选号
func TestAllCoolingStillReturnedWithoutStandby(t *testing.T) {
	for _, test := range []struct {
		name            string
		accounts        []string
		coolStandby     bool
		playgroundFirst bool
	}{
		{name: "没有备用账号", accounts: slotWaitAccounts[:2]},
		{name: "备用账号也在冷却", accounts: slotWaitAccounts, coolStandby: true},
		{name: "Playground 优先", accounts: slotWaitAccounts, playgroundFirst: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			env := newDrainEnv(t, test.accounts, 2, 2)
			fullPartitionCooledWarm(t, env, test.accounts, 2*time.Hour)
			if test.coolStandby {
				coolModelFor(t, env.pool, test.accounts[2], testRuntimeModel, 3*time.Hour)
			}
			ctx, cancel := context.WithTimeout(env.ctx, 3*time.Second)
			defer cancel()
			startedAt := time.Now()
			lease, err := env.service.acquireWarmLease(ctx, aistudio.AccountSelection{
				ModelID: testRuntimeModel, Method: "generateContent", PlaygroundFirst: test.playgroundFirst,
			})
			if lease != nil {
				_ = lease.Release()
			}
			var cooling *aistudio.AllCoolingError
			if !errors.As(err, &cooling) {
				t.Fatalf("err = %v，期望全部冷却", err)
			}
			if elapsed := time.Since(startedAt); elapsed >= schedulingRecheck {
				t.Fatalf("全部冷却 %s 后才返回，期望立即返回", elapsed)
			}
			if until := time.Until(cooling.Until); until < 110*time.Minute || until > 2*time.Hour {
				t.Fatalf("最早恢复还剩 %s，期望约 2 小时", until)
			}
		})
	}
}

// TestLostWarmRaceNeverReportsAllCooling 热候选取租约时被其他请求抢先（分类时还有空位）按账号全部忙碌等待：
// 另一个驻留 Worker 在冷却、没有备用账号时，也不会因此误报全部冷却
func TestLostWarmRaceNeverReportsAllCooling(t *testing.T) {
	env := newDrainEnv(t, slotWaitAccounts[:2], 2, 2)
	env.promote(t, slotWaitAccounts[0], slotWaitAccounts[1])
	coolModelFor(t, env.pool, slotWaitAccounts[1], testRuntimeModel, 2*time.Hour)
	ctx, cancel := context.WithTimeout(env.ctx, 1500*time.Millisecond)
	defer cancel()
	var falseCooling, leases atomic.Int64
	var loops sync.WaitGroup
	for range 16 {
		loops.Add(1)
		go func() {
			defer loops.Done()
			for ctx.Err() == nil {
				lease, err := env.service.acquireWarmLease(ctx, aistudio.AccountSelection{ModelID: testRuntimeModel, Method: "generateContent"})
				var cooling *aistudio.AllCoolingError
				if errors.As(err, &cooling) {
					falseCooling.Add(1)
				}
				if lease != nil {
					leases.Add(1)
					_ = lease.Release()
				}
			}
		}()
	}
	loops.Wait()
	if leases.Load() == 0 {
		t.Fatal("负载期间应有请求取得租约")
	}
	if got := falseCooling.Load(); got != 0 {
		t.Fatalf("还有未冷却的热 Worker 时误报了 %d 次全部冷却（共 %d 个租约）", got, leases.Load())
	}
}
