package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/camoufoxnative"
)

// blockWorkerClose 让账户当前 Worker 的关闭停在关闭回调里（浏览器关闭要几秒），返回“已开始关闭”的通道与放行函数
func blockWorkerClose(t *testing.T, manager *accountWorkerManager, accountID string) (<-chan struct{}, func()) {
	t.Helper()
	manager.mu.RLock()
	account := manager.accounts[accountID]
	manager.mu.RUnlock()
	if account == nil {
		t.Fatalf("账户 %s 不存在", accountID)
	}
	account.mu.Lock()
	worker := account.worker
	account.mu.Unlock()
	if worker == nil {
		t.Fatalf("账户 %s 没有 Worker", accountID)
	}
	closing := make(chan struct{})
	release := make(chan struct{})
	worker.OnClosed(func() {
		close(closing)
		<-release
	})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	return closing, unblock
}

// promoteWithin 在 limit 内现场提升账户，超时（例如卡在 rebalanceMu 上）时放行 unblock 后报错
func promoteWithin(
	t *testing.T, manager *accountWorkerManager, accountID string, limit time.Duration, unblock func(),
) error {
	t.Helper()
	result := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_, err := manager.promote(ctx, accountID, testRuntimeModel)
		result <- err
	}()
	select {
	case err := <-result:
		return err
	case <-time.After(limit):
		unblock()
		<-result
		t.Fatalf("另一个浏览器关闭期间 %s 的调度在等待（rebalanceMu 被关闭流程占着）", accountID)
		return nil
	}
}

// TestReapClosesBrowserOutsideRebalanceLock 空闲回收在 rebalanceMu 外关闭浏览器：关闭期间其他账户的调度不排队；
// 被回收的 Worker 关完之前仍占着槽位，分区不会多启动一个
func TestReapClosesBrowserOutsideRebalanceLock(t *testing.T) {
	manager, pool, _ := partitionTestManager(t,
		[]string{partitionNormalA, partitionNormalB, partitionNormalC}, nil, 1, 2, 1, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for _, accountID := range []string{partitionNormalA, partitionNormalB} {
		if _, err := manager.promote(ctx, accountID, testRuntimeModel); err != nil {
			t.Fatal(err)
		}
	}
	// A 忙碌：回收只能选 B，替换也没有可淘汰的空闲 Worker
	releaseA := holdLease(t, pool, partitionNormalA)
	defer releaseA()
	closing, unblock := blockWorkerClose(t, manager, partitionNormalB)
	reaped := make(chan bool, 1)
	go func() { reaped <- manager.reapIdleWorker(time.Now().Add(time.Hour)) }()
	select {
	case <-closing:
	case <-ctx.Done():
		t.Fatal("空闲回收没有开始关闭 B")
	}
	if !manager.victimReserved(partitionNormalB) {
		t.Fatal("关闭期间 B 应保持预留，其他淘汰路径不再选它")
	}
	// 关闭期间：调度不等关闭（不持有 rebalanceMu）；B 还占着槽位（A + B = 峰值 2），C 不能启动
	if err := promoteWithin(t, manager, partitionNormalC, 2*time.Second, unblock); !errors.Is(err, errAccountWorkerCapacity) {
		t.Fatalf("B 关完之前分区应仍是满的：err = %v", err)
	}
	if warm := sortedWarm(manager); slices.Contains(warm, partitionNormalC) {
		t.Fatalf("B 关完之前不应多启动 C，驻留 %v", warm)
	}
	unblock()
	select {
	case ok := <-reaped:
		if !ok {
			t.Fatal("应回收 B")
		}
	case <-ctx.Done():
		t.Fatal("空闲回收没有结束")
	}
	if manager.victimReserved(partitionNormalB) {
		t.Fatal("回收结束后应解除预留")
	}
	if warm := sortedWarm(manager); !slices.Equal(warm, []string{partitionNormalA}) {
		t.Fatalf("回收后驻留 %v，期望只剩 A", warm)
	}
	// B 关完、槽位让出后 C 可以启动
	if _, err := manager.promote(ctx, partitionNormalC, testRuntimeModel); err != nil {
		t.Fatalf("B 关完后 C 应能启动: %v", err)
	}
}

// TestReplacementEvictsOutsideRebalanceLock 按需替换在 rebalanceMu 外关闭被替换的浏览器：关闭期间其他账户的调度不排队；
// 新 Worker 的启动占位与被替换的 Worker 都计入占用，不会再为第三个账户多放出槽位；关完后新 Worker 发布
func TestReplacementEvictsOutsideRebalanceLock(t *testing.T) {
	manager, _, _ := partitionTestManager(t,
		[]string{partitionNormalA, partitionNormalB, partitionNormalC}, nil, 1, 1, 1, 1)
	var launches atomic.Int32
	launch := manager.launch
	manager.launch = func(ctx context.Context, accountID string, options camoufoxnative.Options) (*aistudio.NativeWorker, error) {
		launches.Add(1)
		return launch(ctx, accountID, options)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := manager.promote(ctx, partitionNormalA, testRuntimeModel); err != nil {
		t.Fatal(err)
	}
	closing, unblock := blockWorkerClose(t, manager, partitionNormalA)
	replaced := make(chan error, 1)
	go func() {
		_, err := manager.promote(ctx, partitionNormalB, testRuntimeModel)
		replaced <- err
	}()
	select {
	case <-closing:
	case <-ctx.Done():
		t.Fatal("按需替换没有开始关闭 A")
	}
	if err := promoteWithin(t, manager, partitionNormalC, 2*time.Second, unblock); !errors.Is(err, errAccountWorkerCapacity) {
		t.Fatalf("A 关完之前分区应仍是满的：err = %v", err)
	}
	if got := launches.Load(); got != 2 {
		t.Fatalf("A 关闭期间只应启动 A、B 两个浏览器，实际启动 %d 次", got)
	}
	unblock()
	select {
	case err := <-replaced:
		if err != nil {
			t.Fatalf("替换应成功: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("按需替换没有结束")
	}
	if warm := sortedWarm(manager); !slices.Equal(warm, []string{partitionNormalB}) {
		t.Fatalf("替换后驻留 %v，期望只有 B", warm)
	}
	if manager.victimReserved(partitionNormalA) {
		t.Fatal("替换结束后应解除 A 的预留")
	}
}

// TestEvictReservedVictimAfterRemove 锁外淘汰的对象在淘汰期间被删除时按已淘汰处理，不让按需替换失败、也不写回收失败：
// 按真实的删除顺序构造（账户池取得独占、回调里关闭 Worker、移出账户池，最后才从管理器移除），两个中间状态都要覆盖：
// 账户池已移出、管理器还在（取租约得到 ErrAccountNotFound），以及两边都已移除
func TestEvictReservedVictimAfterRemove(t *testing.T) {
	manager, pool, _ := partitionTestManager(t, []string{partitionNormalA, partitionNormalB}, nil, 1, 2, 1, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for _, accountID := range []string{partitionNormalA, partitionNormalB} {
		if _, err := manager.promote(ctx, accountID, testRuntimeModel); err != nil {
			t.Fatal(err)
		}
	}
	remove := func(accountID string) {
		t.Helper()
		if _, err := pool.Remove(accountID, func(account *aistudio.Account) error {
			return manager.Reset(account.ID)
		}); err != nil {
			t.Fatal(err)
		}
	}
	// 账户池已移出（Worker 已由删除流程关闭），管理器还没移除
	remove(partitionNormalA)
	if evicted, err := manager.evictIdleWorker(ctx, partitionNormalA); !errors.Is(err, aistudio.ErrAccountNotFound) || evicted {
		t.Fatalf("evictIdleWorker 对账户池已移出的账户应照旧报错：evicted=%v err=%v", evicted, err)
	}
	if evicted, err := manager.evictReservedVictim(ctx, partitionNormalA); err != nil || !evicted {
		t.Fatalf("账户池已移出的淘汰对象应按已淘汰处理：evicted=%v err=%v", evicted, err)
	}
	// 两边都已移除
	remove(partitionNormalB)
	if err := manager.Remove(partitionNormalB); err != nil {
		t.Fatal(err)
	}
	if evicted, err := manager.evictReservedVictim(ctx, partitionNormalB); err != nil || !evicted {
		t.Fatalf("已删除的淘汰对象应按已淘汰处理：evicted=%v err=%v", evicted, err)
	}
	if warm := manager.WarmAccountIDs(); len(warm) != 0 {
		t.Fatalf("删除流程应已关闭两个 Worker，驻留 %v", warm)
	}
}

// TestReplacementSurvivesVictimDeletion 按需替换选中的淘汰对象在新浏览器启动期间被删除：新 Worker 照常发布（删除流程已关闭被替换的
// Worker、空出槽位），请求不失败，新浏览器不被关掉
func TestReplacementSurvivesVictimDeletion(t *testing.T) {
	manager, pool, _ := partitionTestManager(t, []string{partitionNormalA, partitionNormalB}, nil, 1, 1, 1, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := manager.promote(ctx, partitionNormalA, testRuntimeModel); err != nil {
		t.Fatal(err)
	}
	launching := make(chan struct{})
	proceed := make(chan struct{})
	launch := manager.launch
	manager.launch = func(ctx context.Context, accountID string, options camoufoxnative.Options) (*aistudio.NativeWorker, error) {
		if accountID == partitionNormalB {
			close(launching)
			<-proceed
		}
		return launch(ctx, accountID, options)
	}
	replaced := make(chan error, 1)
	go func() {
		_, err := manager.promote(ctx, partitionNormalB, testRuntimeModel)
		replaced <- err
	}()
	select {
	case <-launching:
	case <-ctx.Done():
		t.Fatal("按需替换没有开始启动 B")
	}
	if !manager.victimReserved(partitionNormalA) {
		t.Fatal("前提：A 应已被选为淘汰对象")
	}
	// 新浏览器启动期间删除 A：账户池取得独占、关闭 Worker、移出，随后从管理器移除
	if _, err := pool.Remove(partitionNormalA, func(account *aistudio.Account) error {
		return manager.Reset(account.ID)
	}); err != nil {
		t.Fatal(err)
	}
	close(proceed)
	select {
	case err := <-replaced:
		if err != nil {
			t.Fatalf("淘汰对象被删除后替换应照常完成：%v", err)
		}
	case <-ctx.Done():
		t.Fatal("按需替换没有结束")
	}
	if err := manager.Remove(partitionNormalA); err != nil {
		t.Fatal(err)
	}
	if warm := sortedWarm(manager); !slices.Equal(warm, []string{partitionNormalB}) {
		t.Fatalf("替换后驻留 %v，期望只有 B", warm)
	}
}

// TestReapDoesNotTriggerReplacement 空闲回收在锁外关闭 A 期间，同分区需要冷启动的 C 不走按需替换（不再淘汰同样空闲的 B、不占每分钟
// 替换配额）：等 A 关完、空出槽位后按需扩容，最后驻留 B 与 C。原先回收全程持有 rebalanceMu，C 等回收结束后扩容，结果相同
func TestReapDoesNotTriggerReplacement(t *testing.T) {
	manager, _, requests := partitionTestManager(t,
		[]string{partitionNormalA, partitionNormalB, partitionNormalC}, nil, 1, 2, 1, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for _, accountID := range []string{partitionNormalA, partitionNormalB} {
		if _, err := manager.promote(ctx, accountID, testRuntimeModel); err != nil {
			t.Fatal(err)
		}
	}
	// A、B 都空闲：回收选中其中一个，另一个应保留
	closingA, unblockA := blockWorkerClose(t, manager, partitionNormalA)
	closingB, unblockB := blockWorkerClose(t, manager, partitionNormalB)
	unblock := func() { unblockA(); unblockB() }
	reaped := make(chan bool, 1)
	go func() { reaped <- manager.reapIdleWorker(time.Now().Add(time.Hour)) }()
	var reapedID, survivor string
	select {
	case <-closingA:
		reapedID, survivor = partitionNormalA, partitionNormalB
	case <-closingB:
		reapedID, survivor = partitionNormalB, partitionNormalA
	case <-ctx.Done():
		t.Fatal("空闲回收没有开始关闭")
	}
	// 不等待的选号：回收关完之前分区仍是满的，按容量已满返回，不淘汰另一个空闲 Worker
	if err := promoteWithin(t, manager, partitionNormalC, 2*time.Second, unblock); !errors.Is(err, errAccountWorkerCapacity) {
		t.Fatalf("回收关完之前不等待的选号应按容量已满返回：err = %v", err)
	}
	// 等待的选号（请求路径）：等回收结束
	started := make(chan error, 1)
	go func() {
		_, err := manager.ensureWorker(withWorkerStartSource(ctx, workerStartOnDemand), partitionNormalC, testRuntimeModel, true)
		started <- err
	}()
	time.Sleep(300 * time.Millisecond)
	if !manager.victimReserved(reapedID) || manager.victimReserved(survivor) {
		t.Fatalf("回收期间只应预留被回收的 %s，不应为 C 再预留 %s", reapedID, survivor)
	}
	select {
	case err := <-started:
		t.Fatalf("回收关完之前 C 不应启动：%v", err)
	default:
	}
	unblock()
	select {
	case ok := <-reaped:
		if !ok {
			t.Fatal("应回收一个空闲 Worker")
		}
	case <-ctx.Done():
		t.Fatal("空闲回收没有结束")
	}
	select {
	case err := <-started:
		if err != nil {
			t.Fatalf("回收关完后 C 应按空槽启动：%v", err)
		}
	case <-ctx.Done():
		t.Fatal("C 没有在回收结束后启动")
	}
	want := []string{survivor, partitionNormalC}
	slices.Sort(want)
	if warm := sortedWarm(manager); !slices.Equal(warm, want) {
		t.Fatalf("驻留 %v，期望 %v（另一个空闲 Worker 不应被替换掉）", warm, want)
	}
	if hasLogContaining(requests, "按需替换") || !hasLogContaining(requests, "按需扩容") {
		t.Fatalf("C 应按需扩容而不是按需替换：%v", serviceLogMessages(requests))
	}
	manager.rebalanceMu.Lock()
	reaping := len(manager.reaping)
	manager.rebalanceMu.Unlock()
	if reaping != 0 {
		t.Fatalf("回收结束后应撤销进行中的回收计数：%d", reaping)
	}
}

// TestUnlockedEvictionConcurrency 空闲回收、按需替换、删除账户并发进行：没有死锁与数据竞争，结束后驻留不超过峰值、
// 每个启动过的浏览器要么驻留要么已关闭
func TestUnlockedEvictionConcurrency(t *testing.T) {
	accountIDs := make([]string, 8)
	for index := range accountIDs {
		accountIDs[index] = fmt.Sprintf("evict-%d@example.com", index)
	}
	manager, _, _ := partitionTestManager(t, accountIDs, nil, 1, 3, 1, 1)
	var mu sync.Mutex
	closedFuncs := map[*aistudio.NativeWorker]func() int{}
	manager.launch = func(_ context.Context, accountID string, _ camoufoxnative.Options) (*aistudio.NativeWorker, error) {
		worker, closed := aistudio.NewStubWorker(accountID, 0)
		mu.Lock()
		closedFuncs[worker] = closed
		mu.Unlock()
		return worker, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	deadline := time.Now().Add(time.Second)
	var group sync.WaitGroup
	for index, accountID := range accountIDs[:6] {
		group.Add(1)
		go func(index int, accountID string) {
			defer group.Done()
			for time.Now().Before(deadline) {
				_, _ = manager.promote(ctx, accountID, testRuntimeModel)
				time.Sleep(time.Duration(index) * time.Millisecond)
			}
		}(index, accountID)
	}
	group.Add(1)
	go func() {
		defer group.Done()
		for time.Now().Before(deadline) {
			manager.reapIdleWorker(time.Now().Add(time.Hour))
			time.Sleep(time.Millisecond)
		}
	}()
	group.Add(1)
	go func() {
		defer group.Done()
		time.Sleep(200 * time.Millisecond)
		_ = manager.Remove(accountIDs[0])
	}()
	group.Wait()
	if err := ctx.Err(); err != nil {
		t.Fatalf("并发淘汰没有在期限内结束: %v", err)
	}
	// 驻留数不超过峰值（替换是先启后停，过程中短暂多 1 个，结束后应回到峰值以内）
	if warm := manager.warmAccountIDsIn(aistudio.PoolScopeNormal); len(warm) > 3 {
		t.Fatalf("驻留 %d 个，超过峰值 3：%v", len(warm), warm)
	}
	manager.victimMu.Lock()
	reserved := len(manager.victims)
	manager.victimMu.Unlock()
	if reserved != 0 {
		t.Fatalf("结束后仍有 %d 个淘汰预留没有解除", reserved)
	}
	manager.rebalanceMu.Lock()
	openings := len(manager.openings)
	manager.rebalanceMu.Unlock()
	if openings != 0 {
		t.Fatalf("结束后仍有 %d 个启动占位", openings)
	}
	warm := map[string]struct{}{}
	for _, accountID := range manager.WarmAccountIDs() {
		warm[accountID] = struct{}{}
	}
	mu.Lock()
	defer mu.Unlock()
	live := 0
	for worker, closed := range closedFuncs {
		if closed() > 0 {
			continue
		}
		live++
		if _, ok := warm[worker.State().AccountID]; !ok {
			t.Fatalf("账户 %s 的浏览器没有关闭也不在驻留里（泄漏）", worker.State().AccountID)
		}
	}
	if live > 3 {
		t.Fatalf("仍在运行的浏览器 %d 个，超过峰值 3", live)
	}
}

// removeAccountConfig 删掉账户的 account.json（账户目录被外部移走、目录扫描还没把它移出账户池时的样子），并让账户池按它取一次租约：
// 取租约发现文件不在，把账户标为不可调度（它随之成为首选淘汰对象），Worker 还活着
func removeAccountConfig(t *testing.T, pool *aistudio.AccountPool, accountID string) {
	t.Helper()
	account, err := pool.Account(accountID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(account.ConfigPath); err != nil {
		t.Fatal(err)
	}
	if lease, err := pool.AcquireAccount(context.Background(), accountID); !errors.Is(err, aistudio.ErrAccountNotFound) {
		if lease != nil {
			_ = lease.Release()
		}
		t.Fatalf("前提：账户文件不在时取租约应得到 ErrAccountNotFound，实际 %v", err)
	}
}

// TestReapStopsOnVictimWithMissingFiles 空闲回收的淘汰对象账户文件被外部移走、还在账户池里（Worker 还活着）：不算已回收，
// 写一条失败日志后结束本轮，不在它身上原地空转、不刷“空闲回收”日志；Worker 照常驻留，等目录扫描移出时由移出流程关闭
func TestReapStopsOnVictimWithMissingFiles(t *testing.T) {
	manager, pool, requests := partitionTestManager(t, []string{partitionNormalA, partitionNormalB}, nil, 1, 2, 1, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for _, accountID := range []string{partitionNormalA, partitionNormalB} {
		if _, err := manager.promote(ctx, accountID, testRuntimeModel); err != nil {
			t.Fatal(err)
		}
	}
	// A 忙碌：回收只能选 B
	releaseA := holdLease(t, pool, partitionNormalA)
	defer releaseA()
	removeAccountConfig(t, pool, partitionNormalB)
	for round := range 3 {
		if manager.reapIdleWorker(time.Now().Add(time.Hour)) {
			t.Fatalf("第 %d 轮：账户文件不在、Worker 还活着的淘汰对象不应算已回收（回收循环会原地空转）", round+1)
		}
	}
	if got := countLogs(requests, "空闲回收 | Worker="); got != 0 {
		t.Fatalf("没有关闭任何 Worker，不应写“空闲回收”：%d 条", got)
	}
	if got := countLogs(requests, "WAA Worker 空闲回收失败", "账户文件已不在"); got != 3 {
		t.Fatalf("每轮应写一条回收失败并结束本轮，实际 %d 条：%v", got, serviceLogMessages(requests))
	}
	if warm := sortedWarm(manager); !slices.Contains(warm, partitionNormalB) {
		t.Fatalf("B 的 Worker 还没关闭，应仍驻留：%v", warm)
	}
	if manager.victimReserved(partitionNormalB) {
		t.Fatal("回收结束后应解除 B 的预留")
	}
	// 目录扫描移出账户：移出流程关闭 Worker 之后，淘汰按已淘汰处理（删除路径不变）
	if _, err := pool.Remove(partitionNormalB, func(account *aistudio.Account) error {
		return manager.Reset(account.ID)
	}); err != nil {
		t.Fatal(err)
	}
	if evicted, err := manager.evictReservedVictim(ctx, partitionNormalB); err != nil || !evicted {
		t.Fatalf("移出账户池后的淘汰对象应按已淘汰处理：evicted=%v err=%v", evicted, err)
	}
}

// TestReplacementSkipsVictimWithMissingFiles 按需替换选中的淘汰对象账户文件被外部移走、还在账户池里（Worker 还活着）：不按已淘汰
// 发布新 Worker，跳过它换下一个淘汰对象，驻留数不超过峰值；没有别的淘汰对象时新浏览器等到请求结束后丢弃，不多驻留
func TestReplacementSkipsVictimWithMissingFiles(t *testing.T) {
	t.Run("换下一个淘汰对象", func(t *testing.T) {
		manager, pool, requests := partitionTestManager(t,
			[]string{partitionNormalA, partitionNormalB, partitionNormalC}, nil, 1, 2, 1, 1)
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		for _, accountID := range []string{partitionNormalA, partitionNormalC} {
			if _, err := manager.promote(ctx, accountID, testRuntimeModel); err != nil {
				t.Fatal(err)
			}
		}
		// A 的文件不在、已不可调度：排在淘汰选号的最前
		removeAccountConfig(t, pool, partitionNormalA)
		if _, err := manager.promote(ctx, partitionNormalB, testRuntimeModel); err != nil {
			t.Fatalf("跳过 A 后应换掉 C 完成替换：%v", err)
		}
		want := []string{partitionNormalA, partitionNormalB}
		if warm := sortedWarm(manager); !slices.Equal(warm, want) {
			t.Fatalf("驻留 %v，期望 %v（A 的 Worker 还活着，不能按已淘汰再多驻留一个）", warm, want)
		}
		if !hasLogContaining(requests, "按需替换跳过淘汰对象", "账户文件已不在") {
			t.Fatalf("应写跳过淘汰对象的日志：%v", serviceLogMessages(requests))
		}
		if manager.victimReserved(partitionNormalA) || manager.victimReserved(partitionNormalC) {
			t.Fatal("替换结束后应解除全部预留")
		}
	})
	t.Run("没有别的淘汰对象", func(t *testing.T) {
		manager, pool, _ := partitionTestManager(t,
			[]string{partitionNormalA, partitionNormalB, partitionNormalC}, nil, 1, 1, 1, 1)
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if _, err := manager.promote(ctx, partitionNormalA, testRuntimeModel); err != nil {
			t.Fatal(err)
		}
		removeAccountConfig(t, pool, partitionNormalA)
		for _, accountID := range []string{partitionNormalB, partitionNormalC} {
			requestCtx, requestCancel := context.WithTimeout(ctx, 300*time.Millisecond)
			_, err := manager.promote(requestCtx, accountID, testRuntimeModel)
			requestCancel()
			if err == nil {
				t.Fatalf("%s：A 的 Worker 还活着、没有别的淘汰对象，替换不应成功", accountID)
			}
			if warm := sortedWarm(manager); !slices.Equal(warm, []string{partitionNormalA}) {
				t.Fatalf("%s 之后驻留 %v，不应超过峰值 1", accountID, warm)
			}
		}
		manager.rebalanceMu.Lock()
		openings := len(manager.openings)
		manager.rebalanceMu.Unlock()
		if openings != 0 || manager.victimReserved(partitionNormalA) {
			t.Fatalf("结束后不应留下启动占位（%d）或 A 的预留", openings)
		}
	})
}

// TestReplacementAbandonKeepsOpeningUntilClosed 按需替换放弃时（等淘汰对象期间请求取消），先关闭还没发布的新浏览器、关完才撤销启动占位：
// 新浏览器关闭期间占位仍在，同分区第三个账户不能启动；关完后占位撤销，新浏览器只关一次
func TestReplacementAbandonKeepsOpeningUntilClosed(t *testing.T) {
	manager, pool, _ := partitionTestManager(t,
		[]string{partitionNormalA, partitionNormalB, partitionNormalC}, nil, 1, 1, 1, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := manager.promote(ctx, partitionNormalA, testRuntimeModel); err != nil {
		t.Fatal(err)
	}
	launching, proceed := make(chan struct{}), make(chan struct{})
	closingB, releaseB := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() {
		releaseOnce.Do(func() {
			close(releaseB)
		})
	}
	t.Cleanup(unblock)
	var closedB func() int
	launch := manager.launch
	manager.launch = func(launchCtx context.Context, accountID string, options camoufoxnative.Options) (*aistudio.NativeWorker, error) {
		if accountID != partitionNormalB {
			return launch(launchCtx, accountID, options)
		}
		close(launching)
		<-proceed
		worker, closed := aistudio.NewStubWorker(accountID, 0)
		closedB = closed
		worker.OnClosed(func() {
			close(closingB)
			<-releaseB
		})
		return worker, nil
	}
	requestCtx, requestCancel := context.WithCancel(ctx)
	defer requestCancel()
	replaced := make(chan error, 1)
	go func() {
		_, err := manager.promote(requestCtx, partitionNormalB, testRuntimeModel)
		replaced <- err
	}()
	select {
	case <-launching:
	case <-ctx.Done():
		t.Fatal("按需替换没有开始启动 B")
	}
	// 新浏览器启动期间 A 开始服务请求：淘汰等不到 A 空闲，请求随后取消，替换放弃
	releaseA := holdLease(t, pool, partitionNormalA)
	defer releaseA()
	close(proceed)
	time.Sleep(150 * time.Millisecond)
	requestCancel()
	select {
	case <-closingB:
	case <-ctx.Done():
		t.Fatal("替换放弃后没有关闭还没发布的新浏览器")
	}
	manager.rebalanceMu.Lock()
	opening := manager.openings[partitionNormalB]
	manager.rebalanceMu.Unlock()
	if opening == nil {
		unblock()
		t.Fatal("新浏览器关完之前应保留 B 的启动占位（照常计入占用）")
	}
	if err := promoteWithin(t, manager, partitionNormalC, 2*time.Second, unblock); !errors.Is(err, errAccountWorkerCapacity) {
		t.Fatalf("新浏览器关完之前分区应仍是满的：err = %v", err)
	}
	unblock()
	select {
	case err := <-replaced:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("放弃的替换应按请求取消返回：%v", err)
		}
	case <-ctx.Done():
		t.Fatal("按需替换没有结束")
	}
	manager.rebalanceMu.Lock()
	openings := len(manager.openings)
	manager.rebalanceMu.Unlock()
	if openings != 0 || manager.victimReserved(partitionNormalA) {
		t.Fatalf("放弃后不应留下启动占位（%d）或 A 的预留", openings)
	}
	if got := closedB(); got != 1 {
		t.Fatalf("没发布的新浏览器应恰好关闭一次：%d", got)
	}
	if warm := sortedWarm(manager); !slices.Equal(warm, []string{partitionNormalA}) {
		t.Fatalf("放弃后驻留 %v，期望只有 A", warm)
	}
}

// detachStaleAccount 按目录扫描移出账户的顺序（detachAccount）移出：账户池取得独占、回调里关闭它的 Worker、移出账户池，最后从管理器移除
func detachStaleAccount(t *testing.T, manager *accountWorkerManager, pool *aistudio.AccountPool, accountID string) {
	t.Helper()
	if _, err := pool.Remove(accountID, func(account *aistudio.Account) error {
		return manager.Reset(account.ID)
	}); err != nil {
		t.Fatal(err)
	}
	if err := manager.Remove(accountID); err != nil {
		t.Fatal(err)
	}
}

// waitStaleVictimSkip 等按需替换写出“跳过淘汰对象”日志（新浏览器已启动好、正在等下一个淘汰对象）
func waitStaleVictimSkip(t *testing.T, requests *requestRegistry) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !hasLogContaining(requests, "按需替换跳过淘汰对象") {
		if time.Now().After(deadline) {
			t.Fatalf("按需替换没有跳过账户文件已不在的淘汰对象：%v", serviceLogMessages(requests))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// replacementLeftovers 返回替换结束后残留的启动占位数，以及 accountIDs 里仍被预留的账户
func replacementLeftovers(manager *accountWorkerManager, accountIDs ...string) (int, []string) {
	manager.rebalanceMu.Lock()
	openings := len(manager.openings)
	manager.rebalanceMu.Unlock()
	var reserved []string
	for _, accountID := range accountIDs {
		if manager.victimReserved(accountID) {
			reserved = append(reserved, accountID)
		}
	}
	return openings, reserved
}

// TestReplacementPublishesAfterStaleVictimDetached 按需替换跳过账户文件已不在的淘汰对象 A、没有别的淘汰对象时，A 被目录扫描移出
// （移出流程关闭它的 Worker、空出槽位）后立即发布新 Worker：请求路径不等到请求超时、关掉已启动好的新浏览器，后台扩容（不带期限）
// 不一直挂住、不一直占着启动占位。原先跳过 A 后只等下一个淘汰对象、不再看分区是否已空出槽位
func TestReplacementPublishesAfterStaleVictimDetached(t *testing.T) {
	t.Run("请求路径", func(t *testing.T) {
		manager, pool, requests := partitionTestManager(t, []string{partitionNormalA, partitionNormalB}, nil, 1, 1, 1, 1)
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if _, err := manager.promote(ctx, partitionNormalA, testRuntimeModel); err != nil {
			t.Fatal(err)
		}
		removeAccountConfig(t, pool, partitionNormalA)
		replaced := make(chan error, 1)
		go func() {
			requestCtx, requestCancel := context.WithTimeout(ctx, 5*time.Second)
			defer requestCancel()
			_, err := manager.promote(requestCtx, partitionNormalB, testRuntimeModel)
			replaced <- err
		}()
		waitStaleVictimSkip(t, requests)
		detachStaleAccount(t, manager, pool, partitionNormalA)
		select {
		case err := <-replaced:
			if err != nil {
				t.Fatalf("A 移出、槽位空出后替换应完成：%v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("A 移出、槽位空出后 1 秒内替换应完成，仍在等下一个淘汰对象")
		}
		if warm := sortedWarm(manager); !slices.Equal(warm, []string{partitionNormalB}) {
			t.Fatalf("驻留 %v，期望只有 B", warm)
		}
		if openings, reserved := replacementLeftovers(manager, partitionNormalA, partitionNormalB); openings != 0 || len(reserved) != 0 {
			t.Fatalf("替换结束后不应留下启动占位（%d）或预留 %v", openings, reserved)
		}
	})
	t.Run("后台扩容", func(t *testing.T) {
		manager, pool, requests := partitionTestManager(t, []string{partitionNormalA, partitionNormalB}, nil, 1, 1, 1, 1)
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if _, err := manager.promote(ctx, partitionNormalA, testRuntimeModel); err != nil {
			t.Fatal(err)
		}
		removeAccountConfig(t, pool, partitionNormalA)
		done := manager.expandInBackground(partitionNormalB, testRuntimeModel)
		waitStaleVictimSkip(t, requests)
		detachStaleAccount(t, manager, pool, partitionNormalA)
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("A 移出、槽位空出后 1 秒内后台扩容应结束，仍挂着启动占位")
		}
		if warm := sortedWarm(manager); !slices.Equal(warm, []string{partitionNormalB}) {
			t.Fatalf("驻留 %v，期望只有 B", warm)
		}
		if openings, reserved := replacementLeftovers(manager, partitionNormalA, partitionNormalB); openings != 0 || len(reserved) != 0 {
			t.Fatalf("后台扩容结束后不应留下启动占位（%d）或预留 %v", openings, reserved)
		}
	})
	t.Run("峰值大于 1 时不再淘汰健康 Worker", func(t *testing.T) {
		manager, pool, requests := partitionTestManager(t,
			[]string{partitionNormalA, partitionNormalB, partitionNormalC}, nil, 1, 2, 1, 1)
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		for _, accountID := range []string{partitionNormalA, partitionNormalC} {
			if _, err := manager.promote(ctx, accountID, testRuntimeModel); err != nil {
				t.Fatal(err)
			}
		}
		// C 正在服务请求，跳过 A 之后没有可淘汰的 Worker
		releaseC := holdLease(t, pool, partitionNormalC)
		defer releaseC()
		removeAccountConfig(t, pool, partitionNormalA)
		replaced := make(chan error, 1)
		go func() {
			requestCtx, requestCancel := context.WithTimeout(ctx, 5*time.Second)
			defer requestCancel()
			_, err := manager.promote(requestCtx, partitionNormalB, testRuntimeModel)
			replaced <- err
		}()
		waitStaleVictimSkip(t, requests)
		detachStaleAccount(t, manager, pool, partitionNormalA)
		select {
		case err := <-replaced:
			if err != nil {
				t.Fatalf("A 移出、槽位空出后替换应完成：%v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("A 移出、槽位空出后 1 秒内替换应完成，不应等 C 空闲后再淘汰它")
		}
		releaseC()
		if warm := sortedWarm(manager); !slices.Equal(warm, []string{partitionNormalB, partitionNormalC}) {
			t.Fatalf("驻留 %v，期望 B 与 C（C 是健康 Worker，分区不应少于峰值）", warm)
		}
	})
}

// TestReplacementRetriesStaleVictimAfterFilesRestored 跳过的淘汰对象 A 的账户文件放回后解除预留、重新参与选号：按需替换换掉 A 完成，
// 不一直等到请求超时（原先 A 保持预留到替换结束，选号跳过它）
func TestReplacementRetriesStaleVictimAfterFilesRestored(t *testing.T) {
	manager, pool, requests := partitionTestManager(t, []string{partitionNormalA, partitionNormalB}, nil, 1, 1, 1, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := manager.promote(ctx, partitionNormalA, testRuntimeModel); err != nil {
		t.Fatal(err)
	}
	account, err := pool.Account(partitionNormalA)
	if err != nil {
		t.Fatal(err)
	}
	config, err := os.ReadFile(account.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	removeAccountConfig(t, pool, partitionNormalA)
	replaced := make(chan error, 1)
	go func() {
		requestCtx, requestCancel := context.WithTimeout(ctx, 5*time.Second)
		defer requestCancel()
		_, err := manager.promote(requestCtx, partitionNormalB, testRuntimeModel)
		replaced <- err
	}()
	waitStaleVictimSkip(t, requests)
	if err := os.WriteFile(account.ConfigPath, config, 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-replaced:
		if err != nil {
			t.Fatalf("A 的文件放回后应换掉 A 完成替换：%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("A 的文件放回后 1 秒内替换应完成，A 仍被预留、选号跳过它")
	}
	if warm := sortedWarm(manager); !slices.Equal(warm, []string{partitionNormalB}) {
		t.Fatalf("驻留 %v，期望只有 B", warm)
	}
	if openings, reserved := replacementLeftovers(manager, partitionNormalA, partitionNormalB); openings != 0 || len(reserved) != 0 {
		t.Fatalf("替换结束后不应留下启动占位（%d）或预留 %v", openings, reserved)
	}
}

// TestReplacementGivesUpOnStaleVictims 只剩账户文件已不在的淘汰对象、分区一直没有空出槽位（账户目录还在，目录扫描不会移出它）时，
// 按需替换等满 staleVictimWait 后放弃：关掉没发布的新浏览器、撤销启动占位与预留，不带期限的后台扩容不一直挂住
func TestReplacementGivesUpOnStaleVictims(t *testing.T) {
	previous := staleVictimWait
	staleVictimWait = 300 * time.Millisecond
	t.Cleanup(func() { staleVictimWait = previous })
	manager, pool, requests := partitionTestManager(t, []string{partitionNormalA, partitionNormalB}, nil, 1, 1, 1, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := manager.promote(ctx, partitionNormalA, testRuntimeModel); err != nil {
		t.Fatal(err)
	}
	removeAccountConfig(t, pool, partitionNormalA)
	done := manager.expandInBackground(partitionNormalB, testRuntimeModel)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("只剩账户文件已不在的淘汰对象时，后台扩容应在等满上限后放弃，不一直挂住")
	}
	if !hasLogContaining(requests, "后台扩容失败", "账户文件已不在") {
		t.Fatalf("放弃时应写后台扩容失败及原因：%v", serviceLogMessages(requests))
	}
	if warm := sortedWarm(manager); !slices.Equal(warm, []string{partitionNormalA}) {
		t.Fatalf("驻留 %v，期望只有 A（新浏览器没发布）", warm)
	}
	if openings, reserved := replacementLeftovers(manager, partitionNormalA, partitionNormalB); openings != 0 || len(reserved) != 0 {
		t.Fatalf("放弃后不应留下启动占位（%d）或预留 %v", openings, reserved)
	}
}
