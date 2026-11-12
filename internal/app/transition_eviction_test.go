package app

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/api"
	"github.com/Mag1cFall/AIStudio2API/internal/camoufoxnative"
)

// 管理页启停立即生效（收尾在后台，account_transition.go）与 rebalanceMu 外的淘汰（空闲回收与按需替换，runtime.go）的衔接：
// 两边都会关闭同一账户的 Worker。停用收尾与锁外淘汰都在账户的独占租约内经 Reset 关闭，彼此串行；锁外淘汰的浏览器关完之前
// 照常占着槽位。这里确认合在一起时浏览器只关一次、不漏关、不多放出槽位，同一账户重新启用后的启动要等旧浏览器关完；
// 停用收尾自己关闭 Worker 时（等独占操作、正在关闭两个阶段）再启用，同样要等关完，阶段不被旧收尾清掉

// trackedLaunches 记录测试管理器启动的每个 stub 浏览器与它的关闭次数
type trackedLaunches struct {
	mu        sync.Mutex
	byAccount map[string][]func() int
}

// trackLaunches 换上记录启动的 stub 启动函数
func trackLaunches(manager *accountWorkerManager) *trackedLaunches {
	launches := &trackedLaunches{byAccount: make(map[string][]func() int)}
	manager.launch = func(_ context.Context, accountID string, _ camoufoxnative.Options) (*aistudio.NativeWorker, error) {
		worker, closed := aistudio.NewStubWorker(accountID, 0)
		launches.mu.Lock()
		launches.byAccount[accountID] = append(launches.byAccount[accountID], closed)
		launches.mu.Unlock()
		return worker, nil
	}
	return launches
}

// closes 返回账户每个浏览器的关闭次数（按启动顺序）
func (launches *trackedLaunches) closes(accountID string) []int {
	launches.mu.Lock()
	defer launches.mu.Unlock()
	counts := make([]int, 0, len(launches.byAccount[accountID]))
	for _, closed := range launches.byAccount[accountID] {
		counts = append(counts, closed())
	}
	return counts
}

// total 返回启动过的浏览器总数
func (launches *trackedLaunches) total() int {
	launches.mu.Lock()
	defer launches.mu.Unlock()
	count := 0
	for _, closed := range launches.byAccount {
		count += len(closed)
	}
	return count
}

// assertNoEvictionLeftovers 断言没有残留的淘汰预留、启动占位与启停收尾
func assertNoEvictionLeftovers(t *testing.T, admin *runtimeAdmin, accountIDs ...string) {
	t.Helper()
	manager := admin.workers
	manager.victimMu.Lock()
	reserved := len(manager.victims)
	manager.victimMu.Unlock()
	manager.rebalanceMu.Lock()
	openings := len(manager.openings)
	manager.rebalanceMu.Unlock()
	if reserved != 0 || openings != 0 {
		t.Fatalf("结束后仍有淘汰预留 %d 个、启动占位 %d 个", reserved, openings)
	}
	for _, accountID := range accountIDs {
		if admin.transitions.active(accountID) || transitionOf(t, admin, accountID) != "" {
			t.Fatalf("账户 %s 仍有启停收尾", accountID)
		}
	}
}

// TestToggleDuringUnlockedReap 空闲回收在 rebalanceMu 外关闭账户的浏览器期间，管理页停用（以及停用后再启用）同一账户：
// 启停都立即返回；停用收尾排在回收的独占租约之后，回收关完后不再重复关闭；再启用后同一账户的启动要等旧浏览器关完，
// 不会和它撞车；结束后浏览器只关一次，没有残留的预留、占位与收尾
func TestToggleDuringUnlockedReap(t *testing.T) {
	for _, reenable := range []bool{false, true} {
		name := "停用"
		if reenable {
			name = "停用后再启用"
		}
		t.Run(name, func(t *testing.T) {
			isolateDisableNotes(t)
			admin, _, accounts := keepCatalogAdmin(t, "reap-a@example.com", "reap-b@example.com")
			admin.operationWait = 5 * time.Second
			manager := admin.workers
			manager.maxActive.Store(2)
			launches := trackLaunches(manager)
			target, busy := accounts[0].ID, accounts[1].ID
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			for _, accountID := range []string{target, busy} {
				if _, err := manager.promote(ctx, accountID, testRuntimeModel); err != nil {
					t.Fatal(err)
				}
			}
			// 另一个账户忙碌：回收只能选 target（驻留 2 个、常驻 1 个）
			releaseBusy := holdLease(t, admin.pool, busy)
			defer releaseBusy()
			closing, unblock := blockWorkerClose(t, manager, target)
			reaped := make(chan bool, 1)
			go func() { reaped <- manager.reapIdleWorker(time.Now().Add(time.Hour)) }()
			select {
			case <-closing:
			case <-ctx.Done():
				t.Fatal("空闲回收没有开始关闭浏览器")
			}

			// 回收在锁外关闭期间停用：立即返回，停用收尾等回收的独占租约
			view := toggleWithin(t, admin, target, false, time.Second)
			if view.Enabled || view.Transition != string(aistudio.AccountDraining) {
				t.Fatalf("停用应立即生效并显示 draining：%+v", view)
			}
			restarted := make(chan error, 1)
			if reenable {
				// 收尾还在等回收时再启用：取消收尾、立即返回
				if view := toggleWithin(t, admin, target, true, 2*time.Second); !view.Enabled {
					t.Fatalf("再启用应立即生效：%+v", view)
				}
				// 同一账户重新启动 Worker：旧浏览器关完之前不能启动新的
				go func() {
					_, err := manager.promote(ctx, target, testRuntimeModel)
					restarted <- err
				}()
				select {
				case err := <-restarted:
					unblock()
					t.Fatalf("旧浏览器还没关完，同一账户的启动就返回了：%v", err)
				case <-time.After(200 * time.Millisecond):
				}
				if got := len(launches.closes(target)); got != 1 {
					unblock()
					t.Fatalf("旧浏览器关完之前启动了新的浏览器：共 %d 个", got)
				}
			}
			if !manager.victimReserved(target) {
				t.Fatal("锁外关闭期间应保持淘汰预留")
			}

			unblock()
			select {
			case ok := <-reaped:
				if !ok {
					t.Fatal("空闲回收应关闭 target")
				}
			case <-ctx.Done():
				t.Fatal("空闲回收没有结束")
			}
			if reenable {
				select {
				case err := <-restarted:
					if err != nil {
						t.Fatalf("旧浏览器关完后重新启用的账户应能启动：%v", err)
					}
				case <-ctx.Done():
					t.Fatal("重新启用的账户没有启动")
				}
			}
			waitFor(t, "启停收尾结束", func() bool {
				return !admin.transitions.active(target) && transitionOf(t, admin, target) == ""
			})
			closes := launches.closes(target)
			if len(closes) == 0 || closes[0] != 1 {
				t.Fatalf("回收的浏览器应恰好关闭一次（不重复、不漏关）：%v", closes)
			}
			warm := manager.WarmAccountIDs()
			if reenable {
				if !slices.Equal(closes, []int{1, 0}) || !slices.Contains(warm, target) {
					t.Fatalf("再启用后应只启动一个新浏览器并驻留：关闭次数 %v，驻留 %v", closes, warm)
				}
			} else if len(closes) != 1 || slices.Contains(warm, target) {
				t.Fatalf("停用的账户不应再有浏览器：关闭次数 %v，驻留 %v", closes, warm)
			}
			if hasLogContaining(admin.requests, "账户停用收尾失败") {
				t.Fatal("回收先关闭了 Worker，停用收尾不应失败")
			}
			assertNoEvictionLeftovers(t, admin, target)
		})
	}
}

// TestDisableDuringUnlockedReplacement 按需替换在 rebalanceMu 外关闭被替换账户的浏览器期间，管理页停用被替换的账户：
// 停用立即返回；关闭期间被替换的浏览器与新 Worker 的启动占位都计入占用，第三个账户不能启动（不多放出槽位）；
// 关完后新 Worker 发布，停用收尾不重复关闭，被替换的浏览器只关一次
func TestDisableDuringUnlockedReplacement(t *testing.T) {
	isolateDisableNotes(t)
	admin, _, accounts := keepCatalogAdmin(t, "swap-a@example.com", "swap-b@example.com", "swap-c@example.com")
	admin.operationWait = 5 * time.Second
	manager := admin.workers
	launches := trackLaunches(manager)
	victim, incoming, third := accounts[0].ID, accounts[1].ID, accounts[2].ID
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := manager.promote(ctx, victim, testRuntimeModel); err != nil {
		t.Fatal(err)
	}
	closing, unblock := blockWorkerClose(t, manager, victim)
	replaced := make(chan error, 1)
	go func() {
		_, err := manager.promote(ctx, incoming, testRuntimeModel)
		replaced <- err
	}()
	select {
	case <-closing:
	case <-ctx.Done():
		t.Fatal("按需替换没有开始关闭被替换的浏览器")
	}

	view := toggleWithin(t, admin, victim, false, time.Second)
	if view.Enabled || view.Transition != string(aistudio.AccountDraining) {
		t.Fatalf("停用应立即生效并显示 draining：%+v", view)
	}
	if err := promoteWithin(t, manager, third, 2*time.Second, unblock); !errors.Is(err, errAccountWorkerCapacity) {
		t.Fatalf("被替换的浏览器关完之前分区应仍是满的：err = %v", err)
	}
	if got := launches.total(); got != 2 {
		t.Fatalf("关闭期间只应启动被替换与新来的两个浏览器，实际 %d 个", got)
	}

	unblock()
	select {
	case err := <-replaced:
		if err != nil {
			t.Fatalf("替换应成功：%v", err)
		}
	case <-ctx.Done():
		t.Fatal("按需替换没有结束")
	}
	waitFor(t, "停用收尾结束", func() bool {
		return !admin.transitions.active(victim) && transitionOf(t, admin, victim) == ""
	})
	if closes := launches.closes(victim); !slices.Equal(closes, []int{1}) {
		t.Fatalf("被替换的浏览器应恰好关闭一次：%v", closes)
	}
	if closes := launches.closes(incoming); !slices.Equal(closes, []int{0}) {
		t.Fatalf("新 Worker 不应被停用收尾关闭：%v", closes)
	}
	if warm := sortedWarm(manager); !slices.Equal(warm, []string{incoming}) {
		t.Fatalf("替换后驻留 %v，期望只有 %s", warm, incoming)
	}
	if hasLogContaining(admin.requests, "账户停用收尾失败") {
		t.Fatal("替换先关闭了 Worker，停用收尾不应失败")
	}
	assertNoEvictionLeftovers(t, admin, victim)
}

// TestDisableStagesWhileAccountOperationRuns 停用时账户上正有独占操作（可见重新登录、验证、目录同步这类，不计入请求数）：
// 收尾等这些操作结束时阶段保持 draining、进行中的请求数为 0（管理页显示“等账户上的其他操作结束”，不显示“正在关闭 Worker”）；
// 操作结束、收尾取得独占租约开始关闭 Worker 后阶段为 closing，关完清除
func TestDisableStagesWhileAccountOperationRuns(t *testing.T) {
	admin, account := transitionTestAdmin(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	operation, err := admin.pool.AcquireAccount(ctx, account.ID)
	if err != nil {
		t.Fatal(err)
	}
	released := false
	defer func() {
		if !released {
			_ = operation.Release()
		}
	}()
	closing, unblock := blockWorkerClose(t, admin.workers, account.ID)
	view := toggleWithin(t, admin, account.ID, false, time.Second)
	if view.Enabled || view.Transition != string(aistudio.AccountDraining) || view.ActiveRequests != 0 {
		t.Fatalf("停用应立即生效，阶段 draining、没有进行中的请求：%+v", view)
	}
	time.Sleep(100 * time.Millisecond)
	if stage := transitionOf(t, admin, account.ID); stage != aistudio.AccountDraining {
		t.Fatalf("独占操作还没结束时阶段应保持 draining，实际 %q", stage)
	}
	released = true
	if err := operation.Release(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-closing:
	case <-ctx.Done():
		t.Fatal("独占操作结束后收尾没有开始关闭 Worker")
	}
	if view, _ := admin.account(account.ID); view.Transition != string(aistudio.AccountClosing) || view.ActiveRequests != 0 {
		unblock()
		t.Fatalf("开始关闭 Worker 后阶段应为 closing：%+v", view)
	}
	unblock()
	waitFor(t, "停用收尾结束", func() bool { return transitionOf(t, admin, account.ID) == "" && !admin.transitions.active(account.ID) })
	if slices.Contains(admin.workers.WarmAccountIDs(), account.ID) {
		t.Fatal("收尾应关闭 Worker")
	}
}

// TestEnableWaitsForDisableClose 停用收尾正在关闭 Worker（Reset 卡在关闭浏览器）时启用：启用先取消并等这次收尾退出（关完浏览器），
// 才写入启用状态，不和旧浏览器撞车；返回后阶段为 syncing，不会被旧收尾结束时清掉；旧收尾之后不再关闭任何浏览器
func TestEnableWaitsForDisableClose(t *testing.T) {
	isolateDisableNotes(t)
	admin, catalog, accounts := enableTestAdmin(t, "close@example.com")
	admin.operationWait = 5 * time.Second
	launches := trackLaunches(admin.workers)
	accountID := accounts[0].ID
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := admin.workers.promote(ctx, accountID, testRuntimeModel); err != nil {
		t.Fatal(err)
	}
	closing, unblock := blockWorkerClose(t, admin.workers, accountID)
	toggleWithin(t, admin, accountID, false, time.Second)
	select {
	case <-closing:
	case <-ctx.Done():
		t.Fatal("停用收尾没有开始关闭 Worker")
	}
	// 启用后的收尾要同步一次目录（停用账户没有目录），同步卡住时阶段保持 syncing
	if err := admin.pool.SetCatalog(accountID, aistudio.BenefitTierFree, nil); err != nil {
		unblock()
		t.Fatal(err)
	}
	catalog.gate = make(chan struct{})
	type enableResult struct {
		view api.AdminAccount
		err  error
	}
	enabled := make(chan enableResult, 1)
	go func() {
		view, err := admin.UpdateAccount(context.Background(), accountID, manualDisableInput(accountID, true))
		enabled <- enableResult{view: view, err: err}
	}()
	select {
	case result := <-enabled:
		unblock()
		t.Fatalf("停用收尾还在关闭 Worker 时启用不应先写入：%+v（%v）", result.view, result.err)
	case <-time.After(200 * time.Millisecond):
	}
	if testStatus(t, admin.pool, accountID).Enabled {
		unblock()
		t.Fatal("停用收尾关完 Worker 之前不应写入启用状态")
	}
	unblock()
	var result enableResult
	select {
	case result = <-enabled:
	case <-ctx.Done():
		t.Fatal("停用收尾关完 Worker 后启用没有返回")
	}
	if result.err != nil || !result.view.Enabled || result.view.Transition != string(aistudio.AccountSyncing) {
		t.Fatalf("启用应在旧收尾退出后写入并显示 syncing：%+v（%v）", result.view, result.err)
	}
	time.Sleep(100 * time.Millisecond)
	if stage := transitionOf(t, admin, accountID); stage != aistudio.AccountSyncing || admin.transitions.stage(accountID) != aistudio.AccountSyncing {
		t.Fatalf("旧收尾结束时不应清掉启用收尾的阶段：%q / %q", stage, admin.transitions.stage(accountID))
	}
	close(catalog.gate)
	waitFor(t, "启用收尾结束", func() bool { return !admin.transitions.active(accountID) && transitionOf(t, admin, accountID) == "" })
	if _, err := admin.workers.promote(ctx, accountID, testRuntimeModel); err != nil {
		t.Fatalf("重新启用的账户应能启动 Worker：%v", err)
	}
	time.Sleep(100 * time.Millisecond)
	if closes := launches.closes(accountID); !slices.Equal(closes, []int{1, 0}) {
		t.Fatalf("旧浏览器应恰好关闭一次，重新启用后启动的不被关闭：%v", closes)
	}
	if hasLogContaining(admin.requests, "账户停用收尾失败") {
		t.Fatal("停用收尾不应失败")
	}
}
