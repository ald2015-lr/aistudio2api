package app

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/api"
	"github.com/Mag1cFall/AIStudio2API/internal/camoufoxnative"
	"github.com/gofrs/flock"
)

// transitionTestAdmin 创建带测试 Worker 的管理端：账户 a 有一个驻留 Worker；独占等待上限调到 5 秒（原路径会等满）
func transitionTestAdmin(t *testing.T) (*runtimeAdmin, *aistudio.Account) {
	t.Helper()
	isolateDisableNotes(t)
	admin, _, accounts := keepCatalogAdmin(t, "a@example.com")
	admin.operationWait = 5 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := admin.workers.promote(ctx, accounts[0].ID, testRuntimeModel); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(admin.workers.WarmAccountIDs(), accounts[0].ID) {
		t.Fatal("前提：账户应有驻留 Worker")
	}
	return admin, accounts[0]
}

// toggleWithin 在 limit 内完成一次管理页启停（只改启用状态），返回账户视图
func toggleWithin(t *testing.T, admin *runtimeAdmin, accountID string, enabled bool, limit time.Duration) api.AdminAccount {
	t.Helper()
	startedAt := time.Now()
	account, err := admin.UpdateAccount(context.Background(), accountID, manualDisableInput(accountID, enabled))
	if err != nil {
		t.Fatalf("启停 enabled=%t: %v", enabled, err)
	}
	if elapsed := time.Since(startedAt); elapsed > limit {
		t.Fatalf("启停 enabled=%t 用了 %s，应立即返回", enabled, elapsed)
	}
	return account
}

// transitionOf 返回账户当前的收尾阶段
func transitionOf(t *testing.T, admin *runtimeAdmin, accountID string) aistudio.AccountTransition {
	t.Helper()
	return testStatus(t, admin.pool, accountID).Transition
}

// TestDisableTakesEffectImmediately 停用一个正在处理请求的账户（D4）：立即写回 account.json 与手动停用记录并返回，账户行显示
// draining 与进行中的请求数，Worker 等请求结束后才在后台关闭；原先要等独占租约（这里等满 5 秒后报 account_busy）
func TestDisableTakesEffectImmediately(t *testing.T) {
	admin, account := transitionTestAdmin(t)
	release := holdLease(t, admin.pool, account.ID)
	defer release()
	view := toggleWithin(t, admin, account.ID, false, time.Second)
	if view.Enabled || view.Transition != string(aistudio.AccountDraining) || view.ActiveRequests != 1 {
		t.Fatalf("返回的账户应已停用、显示 draining 与 1 个进行中的请求：%+v", view)
	}
	if diskEnabled(t, account.Directory) || !accountDisableNotes.manual(account.ID) {
		t.Fatal("停用应立即写回 account.json 并记为手动停用")
	}
	if listed, _ := admin.Accounts(context.Background()); listed[0].Transition != string(aistudio.AccountDraining) {
		t.Fatalf("账户列表应显示收尾阶段：%+v", listed[0])
	}
	if !slices.Contains(admin.workers.WarmAccountIDs(), account.ID) {
		t.Fatal("请求还没结束时不应关闭 Worker")
	}
	if !hasLogContaining(admin.requests, "账户已停用 | 通过管理页或管理接口停用") {
		t.Fatal("缺少账户已停用的日志")
	}
	release()
	waitFor(t, "停用收尾结束", func() bool { return transitionOf(t, admin, account.ID) == "" && !admin.transitions.active(account.ID) })
	if slices.Contains(admin.workers.WarmAccountIDs(), account.ID) {
		t.Fatal("请求结束后应关闭 Worker")
	}
	if view, _ := admin.account(account.ID); view.Transition != "" || view.ActiveRequests != 0 {
		t.Fatalf("收尾结束后不应再显示阶段：%+v", view)
	}
}

// TestReenableDuringDrain 停用收尾还在等请求时再次启用：取消收尾、立即启用（不关闭仍在服务的 Worker），清除手动停用记录；
// 进行中的请求照常结束
func TestReenableDuringDrain(t *testing.T) {
	admin, account := transitionTestAdmin(t)
	release := holdLease(t, admin.pool, account.ID)
	toggleWithin(t, admin, account.ID, false, time.Second)
	view := toggleWithin(t, admin, account.ID, true, 2*time.Second)
	if !view.Enabled || !diskEnabled(t, account.Directory) || accountDisableNotes.manual(account.ID) {
		t.Fatalf("再次启用应立即生效并清除手动停用记录：%+v", view)
	}
	waitFor(t, "启用收尾结束", func() bool { return !admin.transitions.active(account.ID) })
	release()
	time.Sleep(100 * time.Millisecond)
	if !slices.Contains(admin.workers.WarmAccountIDs(), account.ID) {
		t.Fatal("取消的停用收尾不应在请求结束后关闭已重新启用的账户的 Worker")
	}
	if transitionOf(t, admin, account.ID) != "" {
		t.Fatal("收尾结束后不应再显示阶段")
	}
}

// TestDeleteDuringDrain 停用收尾期间删除：请求还在进行时照旧返回 account_busy，收尾恢复、继续显示 draining；请求结束后删除成功，
// 不留下收尾
func TestDeleteDuringDrain(t *testing.T) {
	admin, account := transitionTestAdmin(t)
	release := holdLease(t, admin.pool, account.ID)
	toggleWithin(t, admin, account.ID, false, time.Second)
	err := admin.DeleteAccount(context.Background(), account.ID)
	var operation *adminOperationError
	if !errors.As(err, &operation) || operation.code != "account_busy" {
		t.Fatalf("请求还在进行时删除应返回 account_busy，得到 %v", err)
	}
	if transitionOf(t, admin, account.ID) != aistudio.AccountDraining || !admin.transitions.active(account.ID) {
		t.Fatal("删除失败后停用收尾应继续")
	}
	release()
	waitFor(t, "停用收尾结束", func() bool { return !admin.transitions.active(account.ID) })
	if err := admin.DeleteAccount(context.Background(), account.ID); err != nil {
		t.Fatalf("请求结束后应能删除：%v", err)
	}
	if _, ok := admin.pool.StatusOf(account.ID); ok || admin.transitions.active(account.ID) {
		t.Fatal("删除后不应留下账户或收尾")
	}
}

// TestTransitionsCloseOnShutdown 生成服务实例关闭时取消全部收尾并等它们退出（请求还没结束也不等），阶段清除；之后的启停照常写入，
// 不再开始收尾
func TestTransitionsCloseOnShutdown(t *testing.T) {
	admin, account := transitionTestAdmin(t)
	release := holdLease(t, admin.pool, account.ID)
	defer release()
	toggleWithin(t, admin, account.ID, false, time.Second)
	done := make(chan struct{})
	go func() {
		admin.transitions.close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("关闭时应取消收尾，不等进行中的请求")
	}
	if transitionOf(t, admin, account.ID) != "" || admin.transitions.active(account.ID) {
		t.Fatal("关闭后不应留下收尾")
	}
	view := toggleWithin(t, admin, account.ID, true, time.Second)
	if !view.Enabled || view.Transition != "" || admin.transitions.active(account.ID) {
		t.Fatalf("关闭后的启停应照常写入、不再开始收尾：%+v", view)
	}
}

// TestConcurrentTogglesStayConsistent 同一账户被并发反复启停（账户上一直有请求）：不死锁，结束后 account.json、账户池与手动停用记录一致
// （停用 ⇔ 有手动停用记录），不留下收尾
func TestConcurrentTogglesStayConsistent(t *testing.T) {
	admin, account := transitionTestAdmin(t)
	release := holdLease(t, admin.pool, account.ID)
	var workers sync.WaitGroup
	for worker := range 8 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			random := rand.New(rand.NewPCG(uint64(worker), 7))
			for range 15 {
				enabled := random.IntN(2) == 0
				if _, err := admin.UpdateAccount(context.Background(), account.ID, manualDisableInput(account.ID, enabled)); err != nil {
					t.Errorf("启停失败：%v", err)
					return
				}
			}
		}()
	}
	finished := make(chan struct{})
	go func() {
		workers.Wait()
		close(finished)
	}()
	select {
	case <-finished:
	case <-time.After(30 * time.Second):
		t.Fatal("并发启停应在限时内完成（疑似死锁）")
	}
	release()
	waitFor(t, "收尾结束", func() bool { return !admin.transitions.active(account.ID) })
	status := testStatus(t, admin.pool, account.ID)
	if status.Enabled != diskEnabled(t, account.Directory) {
		t.Fatalf("账户池 %t 与 account.json %t 不一致", status.Enabled, diskEnabled(t, account.Directory))
	}
	if accountDisableNotes.manual(account.ID) == status.Enabled {
		t.Fatalf("手动停用记录应与启用状态一致：enabled=%t manual=%t", status.Enabled, accountDisableNotes.manual(account.ID))
	}
	if status.Transition != "" {
		t.Fatalf("不应留下收尾阶段：%s", status.Transition)
	}
}

// TestEnableOnlyChange 代理、语言、时区都不变时（启用状态变不变都算）走立即生效的路径，改了其中任何一个走原路径
func TestEnableOnlyChange(t *testing.T) {
	current := aistudio.AccountStatus{Enabled: true, Proxy: "", Locale: "en-US", Timezone: "UTC"}
	next := aistudio.AccountConfig{Enabled: false, Proxy: "", Locale: "en-US", Timezone: "UTC"}
	if !enableOnlyChange(current, next) {
		t.Fatal("只停用应走立即生效的路径")
	}
	next.Enabled = true
	if !enableOnlyChange(current, next) {
		t.Fatal("原样保存（页面上的状态已过时）应走立即生效的路径")
	}
	for _, change := range []func(*aistudio.AccountConfig){
		func(value *aistudio.AccountConfig) { value.Proxy = "http://203.0.113.7:3128" },
		func(value *aistudio.AccountConfig) { value.Locale = "ja-JP" },
		func(value *aistudio.AccountConfig) { value.Timezone = "Asia/Tokyo" },
	} {
		changed := next
		change(&changed)
		if enableOnlyChange(current, changed) {
			t.Fatalf("改了代理、语言或时区应走原路径：%+v", changed)
		}
	}
}

// headerStateOf 返回账户当前的固定出口（每次重建都是新的对象）
func headerStateOf(admin *runtimeAdmin, accountID string) *accountHeaderState {
	admin.headers.mu.RLock()
	defer admin.headers.mu.RUnlock()
	return admin.headers.accounts[accountID]
}

// assertDrainResumes 断言停用收尾仍在进行（阶段 draining），请求结束后关闭 Worker、重建固定出口，收尾结束
func assertDrainResumes(t *testing.T, admin *runtimeAdmin, accountID string, release func()) {
	t.Helper()
	if transitionOf(t, admin, accountID) != aistudio.AccountDraining || admin.transitions.stage(accountID) != aistudio.AccountDraining {
		t.Fatalf("停用收尾应仍在进行：阶段 %q / %q", transitionOf(t, admin, accountID), admin.transitions.stage(accountID))
	}
	if !slices.Contains(admin.workers.WarmAccountIDs(), accountID) {
		t.Fatal("请求还没结束时不应关闭 Worker")
	}
	header := headerStateOf(admin, accountID)
	release()
	waitFor(t, "停用收尾结束", func() bool { return transitionOf(t, admin, accountID) == "" && !admin.transitions.active(accountID) })
	if slices.Contains(admin.workers.WarmAccountIDs(), accountID) {
		t.Fatal("请求结束后收尾应关闭 Worker")
	}
	if headerStateOf(admin, accountID) == header {
		t.Fatal("收尾应重建账户的固定出口（关闭旧出口的空闲连接）")
	}
}

// TestRepeatedDisableKeepsDrain 停用收尾期间再次停用同一账户（编辑框原样保存、批量停用里包含正在收尾的账户、重复提交）：
// 不取消进行中的收尾，阶段与进行中的请求数照常显示，手动停用记录更新；请求结束后关闭 Worker、重建固定出口
func TestRepeatedDisableKeepsDrain(t *testing.T) {
	admin, account := transitionTestAdmin(t)
	release := holdLease(t, admin.pool, account.ID)
	defer release()
	toggleWithin(t, admin, account.ID, false, time.Second)
	view := toggleWithin(t, admin, account.ID, false, time.Second)
	if view.Enabled || view.Transition != string(aistudio.AccountDraining) || view.ActiveRequests != 1 {
		t.Fatalf("再次停用后应仍显示 draining 与 1 个进行中的请求：%+v", view)
	}
	if !accountDisableNotes.manual(account.ID) || !hasLogContaining(admin.requests, "停用账户记为手动停用") {
		t.Fatal("再次停用应照旧记为手动停用")
	}
	assertDrainResumes(t, admin, account.ID, release)
}

// TestRepeatedEnableKeepsSync 启用收尾（没有目录的账户同步一次目录）期间再次启用：不取消，目录照常同步完，阶段随之清除
func TestRepeatedEnableKeepsSync(t *testing.T) {
	isolateDisableNotes(t)
	admin, catalog, accounts := enableTestAdmin(t, "a@example.com")
	accountID := accounts[0].ID
	disableTestAccount(t, admin.pool, accountID)
	catalog.gate = make(chan struct{})
	view := toggleWithin(t, admin, accountID, true, time.Second)
	if view.Transition != string(aistudio.AccountSyncing) {
		t.Fatalf("启用没有目录的账户应显示 syncing：%+v", view)
	}
	view = toggleWithin(t, admin, accountID, true, time.Second)
	if view.Transition != string(aistudio.AccountSyncing) || admin.transitions.stage(accountID) != aistudio.AccountSyncing {
		t.Fatalf("再次启用不应取消目录同步：%+v", view)
	}
	close(catalog.gate)
	waitFor(t, "启用收尾结束", func() bool { return !admin.transitions.active(accountID) && transitionOf(t, admin, accountID) == "" })
	if refreshed := catalog.refreshedIDs(); !slices.Equal(refreshed, []string{accountID}) {
		t.Fatalf("启用后的目录同步应照常完成一次：%v", refreshed)
	}
}

// TestUpdateDuringDrainResumesOnBusy 停用收尾期间改语言区域（走取独占租约的原路径），账户仍有请求、等不到租约返回 account_busy：
// 收尾按原阶段重新开始，账户行照常显示 draining，请求结束后关闭 Worker、重建固定出口
func TestUpdateDuringDrainResumesOnBusy(t *testing.T) {
	admin, account := transitionTestAdmin(t)
	admin.operationWait = 200 * time.Millisecond
	release := holdLease(t, admin.pool, account.ID)
	defer release()
	toggleWithin(t, admin, account.ID, false, time.Second)
	input := manualDisableInput(account.ID, false)
	input.Locale = "ja-JP"
	_, err := admin.UpdateAccount(context.Background(), account.ID, input)
	var operation *adminOperationError
	if !errors.As(err, &operation) || operation.code != "account_busy" {
		t.Fatalf("账户仍有请求时改语言区域应返回 account_busy，得到 %v", err)
	}
	assertDrainResumes(t, admin, account.ID, release)
}

// TestBulkDeleteFailureResumesDrain 停用收尾期间批量删除，账户仍有请求、等满上限报 account_busy：账户留在号池，收尾重新开始，
// 阶段照常显示；请求结束后关闭 Worker、重建固定出口
func TestBulkDeleteFailureResumesDrain(t *testing.T) {
	previousWait := bulkDeleteBusyWait
	bulkDeleteBusyWait = 200 * time.Millisecond
	t.Cleanup(func() { bulkDeleteBusyWait = previousWait })
	admin, account := transitionTestAdmin(t)
	release := holdLease(t, admin.pool, account.ID)
	defer release()
	toggleWithin(t, admin, account.ID, false, time.Second)
	result, err := admin.DeleteAccounts(context.Background(), api.BulkDeleteInput{AccountIDs: []string{account.ID}})
	if err != nil || result.Deleted != 0 || len(result.Failed) != 1 || result.Failed[0].Code != "account_busy" {
		t.Fatalf("账户仍有请求时批量删除应报 account_busy：%+v（%v）", result, err)
	}
	assertDrainResumes(t, admin, account.ID, release)
}

// holdExternalAccountLock 模拟另一个进程（例如热重建期间还在收尾请求的上一代生成服务）持有账户文件锁（.leases 下的锁文件），返回释放函数
func holdExternalAccountLock(t *testing.T, account *aistudio.Account) func() {
	t.Helper()
	accountDirectory := filepath.Dir(account.StoragePath)
	lock := flock.New(filepath.Join(filepath.Dir(accountDirectory), ".leases", filepath.Base(accountDirectory)+".lock"))
	locked, err := lock.TryLock()
	if err != nil || !locked {
		t.Fatalf("前提：应能取得账户文件锁（locked=%t）：%v", locked, err)
	}
	var once sync.Once
	release := func() { once.Do(func() { _ = lock.Unlock() }) }
	t.Cleanup(release)
	return release
}

// drainHeldByOtherProcess 让账户进入“已停用、进程内没有请求、停用收尾在等另一个进程释放账户文件锁”的状态，返回释放文件锁的函数：
// 进程内还有请求时停用（不取租约的写入沿用请求租约持有的文件锁），取消收尾、请求结束后由另一个进程取得文件锁，再按原阶段恢复收尾
func drainHeldByOtherProcess(t *testing.T, admin *runtimeAdmin, account *aistudio.Account) func() {
	t.Helper()
	release := holdLease(t, admin.pool, account.ID)
	toggleWithin(t, admin, account.ID, false, time.Second)
	stage := admin.cancelTransition(account.ID)
	release()
	releaseExternal := holdExternalAccountLock(t, account)
	admin.resumeTransition(account.ID, stage)
	if got := admin.transitions.stage(account.ID); got != aistudio.AccountDraining {
		t.Fatalf("前提：停用收尾应在等账户文件锁，阶段 %q", got)
	}
	return releaseExternal
}

// TestEnableFailureResumesDrain 停用收尾在等另一个进程释放账户文件锁时再次启用（逐个启用与批量启用）：写入取不到文件锁、返回
// account_busy，账户仍是停用，被取消的停用收尾按原阶段重新开始、账户行照常显示阶段；文件锁释放后关闭 Worker、重建固定出口。
// 原先启用先取消收尾、写入失败后不恢复，账户停在没有收尾的停用状态：Worker 只能等淘汰循环关闭，旧出口的空闲连接不关闭
func TestEnableFailureResumesDrain(t *testing.T) {
	for _, bulk := range []bool{false, true} {
		name := "逐个启用"
		if bulk {
			name = "批量启用"
		}
		t.Run(name, func(t *testing.T) {
			admin, account := transitionTestAdmin(t)
			releaseExternal := drainHeldByOtherProcess(t, admin, account)
			if bulk {
				result, err := admin.EnableAccounts(context.Background(), api.BulkEnableInput{AccountIDs: []string{account.ID}})
				if err != nil || result.Enabled != 0 || len(result.Failed) != 1 || result.Failed[0].Code != "account_busy" {
					t.Fatalf("另一个进程持有账户文件锁时批量启用应报 account_busy：%+v（%v）", result, err)
				}
			} else {
				_, err := admin.UpdateAccount(context.Background(), account.ID, manualDisableInput(account.ID, true))
				var operation *adminOperationError
				if !errors.As(err, &operation) || operation.code != "account_busy" {
					t.Fatalf("另一个进程持有账户文件锁时启用应返回 account_busy，得到 %v", err)
				}
			}
			if testStatus(t, admin.pool, account.ID).Enabled || diskEnabled(t, account.Directory) {
				t.Fatal("写入失败后账户应仍是停用")
			}
			if stage := admin.transitions.stage(account.ID); stage != aistudio.AccountDraining {
				t.Fatalf("被取消的停用收尾应按原阶段重新开始，实际 %q", stage)
			}
			// 进程内没有请求与独占操作时账户行显示为正在关闭（见 AccountStatus 的阶段推断）
			if stage := transitionOf(t, admin, account.ID); stage != aistudio.AccountDraining && stage != aistudio.AccountClosing {
				t.Fatalf("账户行应仍显示收尾阶段，实际 %q", stage)
			}
			if !slices.Contains(admin.workers.WarmAccountIDs(), account.ID) {
				t.Fatal("取得账户文件锁之前不应关闭 Worker")
			}
			header := headerStateOf(admin, account.ID)
			releaseExternal()
			waitFor(t, "停用收尾结束", func() bool {
				return transitionOf(t, admin, account.ID) == "" && !admin.transitions.active(account.ID)
			})
			if slices.Contains(admin.workers.WarmAccountIDs(), account.ID) {
				t.Fatal("文件锁释放后收尾应关闭 Worker")
			}
			if headerStateOf(admin, account.ID) == header {
				t.Fatal("收尾应重建账户的固定出口（关闭旧出口的空闲连接）")
			}
		})
	}
}

// TestBulkEnableDuringDrain 停用收尾期间（账户还有请求在跑）批量启用：与逐个启用一样取消收尾、立即启用并清除手动停用记录，
// 不再等独占租约后报 account_busy；仍在服务的 Worker 不关闭，阶段清除
func TestBulkEnableDuringDrain(t *testing.T) {
	admin, account := transitionTestAdmin(t)
	release := holdLease(t, admin.pool, account.ID)
	defer release()
	toggleWithin(t, admin, account.ID, false, time.Second)
	startedAt := time.Now()
	result, err := admin.EnableAccounts(context.Background(), api.BulkEnableInput{AccountIDs: []string{account.ID}})
	if err != nil || result.Enabled != 1 || len(result.Failed) != 0 {
		t.Fatalf("停用收尾期间批量启用应立即成功：%+v（%v）", result, err)
	}
	if elapsed := time.Since(startedAt); elapsed > time.Second {
		t.Fatalf("批量启用不应等进行中的请求，用了 %s", elapsed)
	}
	status := testStatus(t, admin.pool, account.ID)
	if !status.Enabled || !diskEnabled(t, account.Directory) || accountDisableNotes.manual(account.ID) {
		t.Fatalf("应启用、写回 account.json 并清除手动停用记录：%+v", status)
	}
	if status.Transition != "" || admin.transitions.active(account.ID) {
		t.Fatalf("批量启用应取消停用收尾：阶段 %q", status.Transition)
	}
	release()
	time.Sleep(100 * time.Millisecond)
	if !slices.Contains(admin.workers.WarmAccountIDs(), account.ID) {
		t.Fatal("取消的停用收尾不应在请求结束后关闭已重新启用的账户的 Worker")
	}
}

// TestToggleRefreshesIPPool 只切换启用状态时 IP 池照旧重算账户的出口（与原先保存账户相同）：IP 池里账户的会话号已经变了、
// 还没推送时，启停后账户拿到新的出口
func TestToggleRefreshesIPPool(t *testing.T) {
	isolateDisableNotes(t)
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	admin, _, accounts := keepCatalogAdmin(t, "a@example.com")
	account := accounts[0]
	ipPool, _ := newTestProxyPool(t, newProxyPoolTestClock(), "", "")
	ipPool.configure(dynamicProxyPool(), "", 2)
	ipPool.attach(admin.pool)
	admin.workers.proxyPool = ipPool
	for index, enabled := range []bool{false, true} {
		session := fmt.Sprintf("toggle%d", index)
		ipPool.mu.Lock()
		ipPool.sessions[account.ID] = &proxyPoolSession{base: session}
		ipPool.mu.Unlock()
		if binding, ok := account.CurrentProxyBinding(); ok && binding.Slot == session {
			t.Fatal("前提：会话号变化还没推送给账户")
		}
		toggleWithin(t, admin, account.ID, enabled, time.Second)
		if binding, ok := account.CurrentProxyBinding(); !ok || binding.Slot != session {
			t.Fatalf("enabled=%t：启停后应重算出口，得到 %+v", enabled, binding)
		}
		waitFor(t, "收尾结束", func() bool { return !admin.transitions.active(account.ID) })
	}
}

// TestDisableDuringUnleasedStartup 停用恰好碰上不持有账户租约的 Worker 启动（预热、无租约的按需提升）：停用收尾取得独占租约后
// 可取消地等这次启动结束。这期间再启用立即返回（原先收尾卡在 Reset 的 startupMu 上，启用要等到启动结束），启动好的 Worker
// 留给重新启用的账户；只停用时等启动结束后关闭刚启动的 Worker
func TestDisableDuringUnleasedStartup(t *testing.T) {
	for _, reenable := range []bool{true, false} {
		name := "停用"
		if reenable {
			name = "停用后再启用"
		}
		t.Run(name, func(t *testing.T) {
			isolateDisableNotes(t)
			admin, _, accounts := keepCatalogAdmin(t, "startup@example.com")
			admin.operationWait = 5 * time.Second
			manager := admin.workers
			accountID := accounts[0].ID
			launching, proceed := make(chan struct{}), make(chan struct{})
			var proceedOnce sync.Once
			release := func() { proceedOnce.Do(func() { close(proceed) }) }
			t.Cleanup(release)
			var mu sync.Mutex
			var closes []func() int
			var launchingOnce sync.Once
			manager.launch = func(launchCtx context.Context, launched string, _ camoufoxnative.Options) (*aistudio.NativeWorker, error) {
				launchingOnce.Do(func() { close(launching) })
				select {
				case <-proceed:
				case <-launchCtx.Done():
					return nil, launchCtx.Err()
				}
				worker, closed := aistudio.NewStubWorker(launched, 0)
				mu.Lock()
				closes = append(closes, closed)
				mu.Unlock()
				return worker, nil
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			started := make(chan error, 1)
			go func() {
				_, err := manager.promote(ctx, accountID, testRuntimeModel)
				started <- err
			}()
			select {
			case <-launching:
			case <-ctx.Done():
				t.Fatal("Worker 没有开始启动")
			}
			// 返回时收尾可能还没取得独占租约（账户上没有请求与独占操作，显示为 closing）
			view := toggleWithin(t, admin, accountID, false, time.Second)
			if view.Enabled || view.Transition == "" {
				t.Fatalf("停用应立即生效并显示收尾阶段：%+v", view)
			}
			// 收尾已取得独占租约（账户上没有请求），在等启动结束：阶段为 draining，不是正在关闭
			time.Sleep(200 * time.Millisecond)
			if stage := transitionOf(t, admin, accountID); stage != aistudio.AccountDraining {
				t.Fatalf("等启动结束期间阶段应为 draining，实际 %q", stage)
			}
			if reenable {
				if view := toggleWithin(t, admin, accountID, true, time.Second); !view.Enabled {
					t.Fatalf("再启用应立即生效：%+v", view)
				}
			}
			release()
			select {
			case err := <-started:
				if err != nil {
					t.Fatalf("启动应完成：%v", err)
				}
			case <-ctx.Done():
				t.Fatal("启动没有结束")
			}
			waitFor(t, "启停收尾结束", func() bool {
				return !admin.transitions.active(accountID) && transitionOf(t, admin, accountID) == ""
			})
			mu.Lock()
			counts := make([]int, 0, len(closes))
			for _, closed := range closes {
				counts = append(counts, closed())
			}
			mu.Unlock()
			warm := slices.Contains(manager.WarmAccountIDs(), accountID)
			if reenable {
				if !slices.Equal(counts, []int{0}) || !warm {
					t.Fatalf("重新启用的账户应留着刚启动的 Worker：关闭次数 %v，驻留 %t", counts, warm)
				}
			} else if !slices.Equal(counts, []int{1}) || warm {
				t.Fatalf("停用的账户应在启动结束后关闭刚启动的 Worker：关闭次数 %v，驻留 %t", counts, warm)
			}
			if hasLogContaining(admin.requests, "账户停用收尾失败") {
				t.Fatal("停用收尾不应失败")
			}
		})
	}
}
