package app

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestEgressGuardNilContract 判定器为 nil（测试直接构造的服务）时判定关闭：各方法返回零值或放行，不 panic
func TestEgressGuardNilContract(t *testing.T) {
	var guard *egressGuard
	now := time.Now()
	if verdict := guard.observeOutcome(egressObservation{Key: "", AccountID: "a", Outcome: egressBare, At: now}); verdict != (egressVerdict{}) {
		t.Fatalf("observeOutcome = %+v", verdict)
	}
	if verdict := guard.verdict("", false, now); verdict != (egressVerdict{}) {
		t.Fatalf("verdict = %+v", verdict)
	}
	if guard.anyLimited() || guard.limitedKeys() != nil || guard.catalogPaused("") {
		t.Fatal("nil 判定器不应报告限流")
	}
	release, ok := guard.admitColdStart("", false, "gemini-flash-latest", coldStartReplace, 2)
	if !ok || release == nil {
		t.Fatal("nil 判定器应放行冷启动")
	}
	release(true)
	leave := guard.enterEgressWait("")
	if leave == nil {
		t.Fatal("enterEgressWait 应返回可调用的离开函数")
	}
	leave()
	guard.noteLocalReject("", now)
	guard.run(context.Background())
	if guard.status() != nil {
		t.Fatal("nil 判定器没有状态")
	}
	guard.forceVerdictForTest("", egressVerdict{Mode: egressLimited, Limited: true})
	if guard.verdict("", false, now).Limited {
		t.Fatal("nil 判定器不保存测试判定")
	}
}

// TestEgressGuardForcedVerdict forceVerdictForTest 写入的判定由 verdict 按出口返回，其他出口不受影响
func TestEgressGuardForcedVerdict(t *testing.T) {
	guard := newEgressGuard(nil, nil)
	now := time.Now()
	forced := egressVerdict{Mode: egressLimited, Limited: true, Throttled: true, Label: "直连", Limit: 24, RetryAt: now.Add(7 * time.Second)}
	guard.forceVerdictForTest("", forced)
	if got := guard.verdict("", true, now); got != forced {
		t.Fatalf("verdict = %+v，期望 %+v", got, forced)
	}
	if got := guard.verdict("http://proxy.example:8080", false, now); got != (egressVerdict{}) {
		t.Fatalf("其他出口 verdict = %+v", got)
	}
	var notFound interface{ HTTPStatus() int }
	if err := guard.withdraw("e3b0c44298fc", now); !errors.As(err, &notFound) || notFound.HTTPStatus() != 404 {
		t.Fatalf("withdraw 未知出口 = %v", err)
	}
	if err := guard.resume("e3b0c44298fc", now); err == nil {
		t.Fatal("resume 未知出口应报错")
	}
}

// TestEgressColdStartPausedIsCapacity 冷启动被出口闸门暂停按容量处理
func TestEgressColdStartPausedIsCapacity(t *testing.T) {
	if !errors.Is(errEgressColdStartPaused, errAccountWorkerCapacity) {
		t.Fatal("errEgressColdStartPaused 应包装 errAccountWorkerCapacity")
	}
}

// TestEgressWaitBudgetShared 出口排队预算整个请求共用：再次挂上时沿用已有的一份；没有预算时只返回本次
func TestEgressWaitBudgetShared(t *testing.T) {
	ctx := context.Background()
	if egressWaitBudgetFrom(ctx) != nil {
		t.Fatal("没有挂上时应为 nil")
	}
	var missing *egressWaitBudget
	if got := missing.add(2 * time.Second); got != 2*time.Second || missing.total() != 0 {
		t.Fatalf("nil 预算 add = %v total = %v", got, missing.total())
	}
	ctx = withEgressWaitBudget(ctx)
	budget := egressWaitBudgetFrom(ctx)
	if budget == nil {
		t.Fatal("应挂上预算")
	}
	if egressWaitBudgetFrom(withEgressWaitBudget(ctx)) != budget {
		t.Fatal("再次挂上应沿用已有预算")
	}
	budget.add(3 * time.Second)
	if got := egressWaitBudgetFrom(ctx).add(4 * time.Second); got != 7*time.Second || budget.total() != 7*time.Second {
		t.Fatalf("累计 = %v total = %v", got, budget.total())
	}

	service := &trackedService{}
	if service.egressQueueWaitFor() != egressQueueWaitMax {
		t.Fatalf("默认上限 = %v", service.egressQueueWaitFor())
	}
	service.egressQueueWait = time.Second
	if service.egressQueueWaitFor() != time.Second {
		t.Fatalf("测试上限 = %v", service.egressQueueWaitFor())
	}
}

// TestWorkerStartSourceContext 冷启动来源经 context 传递；空来源不覆盖已有标记
func TestWorkerStartSourceContext(t *testing.T) {
	ctx := context.Background()
	if workerStartSourceFrom(ctx) != "" {
		t.Fatal("没有标记时应为空")
	}
	ctx = withWorkerStartSource(ctx, workerStartCoverage)
	if got := workerStartSourceFrom(ctx); got != "补覆盖" {
		t.Fatalf("来源 = %q", got)
	}
	if got := workerStartSourceFrom(withWorkerStartSource(ctx, " ")); got != "补覆盖" {
		t.Fatalf("空来源覆盖了已有标记：%q", got)
	}
}

// TestUnschedulableHooksNilSafe 不可调度释放与目录调度的挂点在没有 Worker 管理器、没有调度器时不做任何事
func TestUnschedulableHooksNilSafe(t *testing.T) {
	var manager *accountWorkerManager
	manager.releaseUnschedulable("a@example.com", unschedulableAuthRequired)
	service := &trackedService{}
	if added := service.registerUnschedulableWorkers(); added != 0 {
		t.Fatalf("补登记 = %d", added)
	}
	service.noteAuthRequired("a@example.com", "a@example.com", authFromGeneration, errors.New("401"))
	service.catalogSync.noteAuthRequired("a@example.com", time.Now())
	for source, text := range map[authRequiredSource]string{
		authFromGeneration: "生成", authFromStream: "流内", authFromReplay: "续签重放",
		authFromStartup: "启动", authFromCatalog: "目录同步", authFromVerify: "管理验证",
	} {
		if source.String() != text {
			t.Fatalf("来源 %d = %q，期望 %q", source, source.String(), text)
		}
	}
}
