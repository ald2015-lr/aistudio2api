package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// TestPrimaryQueueWaitBoundsStartupSlotQueue 主用模型的请求没有热 Worker 可用、要现场冷启动时，等待冷启动名额的时间计入排队预算：
// 名额被其他按需启动占满、预算用尽后按号池繁忙（429）返回，不再在名额队列里等前面的浏览器启动完成（热 Worker 都成死槽位时
// 这正是每个请求的路径，排队几分钟后 504）；退出时离开名额队列，不留下排队者
func TestPrimaryQueueWaitBoundsStartupSlotQueue(t *testing.T) {
	manager, gate := slotTestManager(t, "camoufox-test", 1, "a@example.com", "b@example.com", "c@example.com")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// 容量 1 加按需保留 1：两个按需启动占满冷启动名额，启动停在 launchGate
	results := make(chan error, 2)
	for _, accountID := range []string{"a@example.com", "b@example.com"} {
		go func(accountID string) {
			_, err := manager.Worker(ctx, accountID, testRuntimeModel)
			results <- err
		}(accountID)
	}
	gate.waitStarted(t, "a@example.com", "第一个按需启动")
	gate.waitStarted(t, "b@example.com", "第二个按需启动")
	waitStartupUsage(t, &manager.startupSlots, 2, 0, 0)

	service := &trackedService{
		lifecycle: ctx, pool: manager.pool, requests: manager.requests, workers: manager,
		forbidden: newForbiddenTracker(), quota: newQuotaSharing("", manager.requests),
	}
	service.setPrimaryModels([]string{testRuntimeModel}, 300*time.Millisecond)
	selection := aistudio.AccountSelection{ModelID: testRuntimeModel, Method: "generateContent", Pool: aistudio.PoolScopeNormal}
	startedAt := time.Now()
	lease, err := service.acquireWarmLease(service.primaryAcquireContext(ctx, selection), selection)
	elapsed := time.Since(startedAt)
	var busy *aistudio.CapacityBusyError
	if lease != nil || !errors.As(err, &busy) {
		t.Fatalf("冷启动名额占满时 lease=%v err=%v 用时 %s，期望排队预算用尽后返回号池繁忙", lease, err, elapsed)
	}
	if elapsed < 300*time.Millisecond || elapsed > 3*time.Second {
		t.Fatalf("排队 %s 后返回，期望约 300ms", elapsed)
	}
	if busy.Waited < 250*time.Millisecond || busy.Limit != 300*time.Millisecond {
		t.Fatalf("号池繁忙错误 = %+v，期望记录等待冷启动名额的时间", busy)
	}
	if gate.isStarted("c@example.com") {
		t.Fatal("预算用尽后不应开始启动备用账号的 Worker")
	}
	// 退出后不再占着名额队列，备用账号的租约已释放
	waitStartupUsage(t, &manager.startupSlots, 2, 0, 0)
	if busy, _ := manager.pool.Activity("c@example.com"); busy {
		t.Fatal("排队超限退出后备用账号的租约应已释放")
	}
	if waiters := dispatchWaiters(service); waiters != 0 {
		t.Fatalf("排队超限退出后仍有 %d 个等待者", waiters)
	}

	gate.open()
	for range 2 {
		select {
		case err := <-results:
			if err != nil {
				t.Fatalf("按需启动失败: %v", err)
			}
		case <-ctx.Done():
			t.Fatal("按需启动没有结束")
		}
	}
	waitStartupUsage(t, &manager.startupSlots, 0, 0, 0)
}
