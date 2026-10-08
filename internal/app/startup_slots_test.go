package app

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/camoufoxnative"
	"github.com/Mag1cFall/AIStudio2API/internal/config"
)

func fixedStartupCapacity(capacity int) func() int {
	return func() int { return capacity }
}

// tryStartupSlot 在 limit 内申请冷启动名额
func tryStartupSlot(slots *startupSlots, capacity func() int, background bool, limit time.Duration) (func(), error) {
	ctx, cancel := context.WithTimeout(context.Background(), limit)
	defer cancel()
	return slots.acquire(ctx, capacity, background, nil)
}

// waitStartupUsage 等待名额占用与排队数达到预期
func waitStartupUsage(t *testing.T, slots *startupSlots, active int, queued int, deferred int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		gotActive, gotQueued, gotDeferred := slots.usage()
		if gotActive == active && gotQueued == queued && gotDeferred == deferred {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("冷启动名额状态不符: 占用=%d 按需排队=%d 预热排队=%d，期望 %d/%d/%d",
				gotActive, gotQueued, gotDeferred, active, queued, deferred)
		}
		time.Sleep(time.Millisecond)
	}
}

// 预热占满容量后按需启动仍有 1 个保留名额；保留名额用完后按需启动排队，释放的名额先给按需启动
func TestStartupSlotsReserveOneForOnDemand(t *testing.T) {
	var slots startupSlots
	capacity := fixedStartupCapacity(2)
	var background []func()
	for range 2 {
		release, err := tryStartupSlot(&slots, capacity, true, time.Second)
		if err != nil {
			t.Fatalf("容量内的预热应立即取得名额: %v", err)
		}
		background = append(background, release)
	}
	if _, err := tryStartupSlot(&slots, capacity, true, 50*time.Millisecond); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("预热占满容量后不应再取得名额，实际 %v", err)
	}
	if _, err := tryStartupSlot(&slots, capacity, false, time.Second); err != nil {
		t.Fatalf("预热占满容量时按需启动应取得保留名额: %v", err)
	}
	if _, err := tryStartupSlot(&slots, capacity, false, 50*time.Millisecond); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("保留名额用完后按需启动应排队，实际 %v", err)
	}
	background[0]()
	background[0]()
	if _, err := tryStartupSlot(&slots, capacity, true, 50*time.Millisecond); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("预热释放的名额不应让预热占用最后一个名额，实际 %v", err)
	}
	if _, err := tryStartupSlot(&slots, capacity, false, time.Second); err != nil {
		t.Fatalf("释放的名额应给按需启动: %v", err)
	}
	waitStartupUsage(t, &slots, 3, 0, 0)
}

// 有按需启动正在排队时，即使占用数还没到容量，预热也让行；按需启动离开队列后预热继续
func TestStartupSlotsBackgroundYieldsToQueuedOnDemand(t *testing.T) {
	var slots startupSlots
	capacity := fixedStartupCapacity(2)
	// 模拟一个正在排队、尚未被唤醒的按需启动（名额刚释放、它还没来得及重新检查）
	slots.mu.Lock()
	slots.queued = 1
	slots.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	acquired := make(chan error, 1)
	go func() {
		release, err := slots.acquire(ctx, capacity, true, nil)
		if err == nil {
			release()
		}
		acquired <- err
	}()
	waitStartupUsage(t, &slots, 0, 1, 1)
	select {
	case err := <-acquired:
		t.Fatalf("有按需启动排队时预热不应取得名额，实际 %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	slots.mu.Lock()
	slots.leaveLocked(false)
	slots.mu.Unlock()
	select {
	case err := <-acquired:
		if err != nil {
			t.Fatalf("按需启动离开队列后预热应取得名额: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("按需启动离开队列后预热没有被唤醒")
	}
}

// 容量随 WARM_STARTUP_CONCURRENCY 热更新：调大后排队的预热立即放行，调小时已占用的名额释放后才按新容量收敛
func TestStartupSlotsFollowCapacityHotUpdate(t *testing.T) {
	var slots startupSlots
	var capacity atomic.Int64
	capacity.Store(1)
	current := func() int { return int(capacity.Load()) }
	first, err := tryStartupSlot(&slots, current, true, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	acquired := make(chan func(), 1)
	go func() {
		release, err := slots.acquire(ctx, current, true, nil)
		if err != nil {
			release = nil
		}
		acquired <- release
	}()
	waitStartupUsage(t, &slots, 1, 0, 1)
	capacity.Store(2)
	slots.resized()
	var second func()
	select {
	case second = <-acquired:
		if second == nil {
			t.Fatal("调大容量后排队的预热应取得名额")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("调大容量后排队的预热没有被唤醒")
	}
	capacity.Store(1)
	slots.resized()
	first()
	if _, err := tryStartupSlot(&slots, current, true, 50*time.Millisecond); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("调小容量后已占用的名额释放前不应放行新的预热，实际 %v", err)
	}
	second()
	if _, err := tryStartupSlot(&slots, current, true, time.Second); err != nil {
		t.Fatalf("占用降到新容量以下后预热应取得名额: %v", err)
	}
}

// launchGate 记录每个账户的 Worker 启动并让启动停住，直到测试放行
type launchGate struct {
	mu         sync.Mutex
	started    map[string]chan struct{}
	background map[string]bool
	release    chan struct{}
	once       sync.Once
}

func newLaunchGate(t *testing.T, accountIDs ...string) *launchGate {
	gate := &launchGate{
		started: make(map[string]chan struct{}), background: make(map[string]bool), release: make(chan struct{}),
	}
	for _, accountID := range accountIDs {
		gate.started[accountID] = make(chan struct{})
	}
	t.Cleanup(gate.open)
	return gate
}

func (gate *launchGate) launch(ctx context.Context, accountID string, _ camoufoxnative.Options) (*aistudio.NativeWorker, error) {
	gate.mu.Lock()
	gate.background[accountID] = backgroundStartup(ctx)
	close(gate.started[accountID])
	gate.mu.Unlock()
	select {
	case <-gate.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	worker, _ := aistudio.NewStubWorker(accountID, 0)
	return worker, nil
}

func (gate *launchGate) open() { gate.once.Do(func() { close(gate.release) }) }

func (gate *launchGate) waitStarted(t *testing.T, accountID string, what string) {
	t.Helper()
	select {
	case <-gate.started[accountID]:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s: 账户 %s 的 Worker 没有开始启动", what, accountID)
	}
}

func (gate *launchGate) isStarted(accountID string) bool {
	select {
	case <-gate.started[accountID]:
		return true
	default:
		return false
	}
}

func (gate *launchGate) launchedInBackground(accountID string) bool {
	gate.mu.Lock()
	defer gate.mu.Unlock()
	return gate.background[accountID]
}

// slotTestManager 创建 Camoufox 后端（启动替换为 launchGate）的管理器，启动预热并发为 concurrency
func slotTestManager(t *testing.T, camoufoxPath string, concurrency int, accountIDs ...string) (*accountWorkerManager, *launchGate) {
	t.Helper()
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	pool, accounts := testAccountPool(t, accountIDs...)
	manager := newAccountWorkerManager(
		pool, accounts, newRequestRegistry(ctx), camoufoxPath, "", time.Minute,
		len(accounts), len(accounts), concurrency, false,
	)
	gate := newLaunchGate(t, accountIDs...)
	manager.launch = gate.launch
	t.Cleanup(func() {
		gate.open()
		_ = manager.Close()
	})
	return manager, gate
}

// 预热占满冷启动容量时，请求现场的冷启动使用保留名额立即开始；第二个预热排队，启动预热并发热更新后立即开始
func TestColdStartReservesSlotForOnDemandWhilePrewarming(t *testing.T) {
	manager, gate := slotTestManager(t, "camoufox-test", 1, "a@example.com", "b@example.com", "c@example.com")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	results := make(chan error, 3)
	background := withBackgroundStartup(ctx)
	go func() {
		_, err := manager.ensureWorker(background, "a@example.com", "", false)
		results <- err
	}()
	gate.waitStarted(t, "a@example.com", "容量内的预热")
	go func() {
		_, err := manager.ensureWorker(background, "b@example.com", "", false)
		results <- err
	}()
	waitStartupUsage(t, &manager.startupSlots, 1, 0, 1)
	if gate.isStarted("b@example.com") {
		t.Fatal("冷启动容量已满时第二个预热不应开始启动")
	}
	go func() {
		_, err := manager.Worker(ctx, "c@example.com", testRuntimeModel)
		results <- err
	}()
	gate.waitStarted(t, "c@example.com", "预热占满容量时请求的冷启动")
	if gate.isStarted("b@example.com") {
		t.Fatal("保留名额只给按需启动，排队的预热不应借用")
	}
	if !gate.launchedInBackground("a@example.com") || gate.launchedInBackground("c@example.com") {
		t.Fatal("预热与请求现场的启动应分别按后台与按需计")
	}

	manager.applyLiveSettings(config.Config{
		WarmWorkerLimit: 3, MaxActiveWorkers: 3, WarmStartupConcurrency: 3, InitTimeout: time.Minute,
	})
	gate.waitStarted(t, "b@example.com", "启动预热并发热更新后排队的预热")

	gate.open()
	for range 3 {
		select {
		case err := <-results:
			if err != nil {
				t.Fatalf("Worker 启动失败: %v", err)
			}
		case <-ctx.Done():
			t.Fatal("Worker 启动没有结束")
		}
	}
	waitStartupUsage(t, &manager.startupSlots, 0, 0, 0)
	if warm := len(manager.WarmAccountIDs()); warm != 3 {
		t.Fatalf("三个 Worker 都应发布，实际 %d", warm)
	}
}

// 预热循环发起的启动按后台计；请求排队时的后台扩容有请求在等，按按需计
func TestPrewarmLaunchesCountAsBackground(t *testing.T) {
	manager, gate := slotTestManager(t, "camoufox-test", 1, "a@example.com", "b@example.com")
	manager.warmTarget.Store(1)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	first := manager.StartPrewarm(ctx)
	deadline := time.Now().Add(5 * time.Second)
	prewarmed := ""
	for prewarmed == "" {
		for _, accountID := range []string{"a@example.com", "b@example.com"} {
			if gate.isStarted(accountID) {
				prewarmed = accountID
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("预热没有启动 Worker")
		}
		time.Sleep(time.Millisecond)
	}
	if !gate.launchedInBackground(prewarmed) {
		t.Fatal("预热循环发起的启动应按后台计")
	}
	expanded := "a@example.com"
	if prewarmed == expanded {
		expanded = "b@example.com"
	}
	manager.expandInBackground(expanded, testRuntimeModel)
	gate.waitStarted(t, expanded, "请求排队时的后台扩容使用保留名额")
	if gate.launchedInBackground(expanded) {
		t.Fatal("请求排队时的后台扩容应按按需计")
	}
	gate.open()
	select {
	case err := <-first:
		if err != nil {
			t.Fatalf("预热失败: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("预热没有结束")
	}
}

// 纯 Go 后端不启动浏览器，冷启动不占名额，只受活动 Worker 上限约束
func TestColdStartSlotsSkipGoBackend(t *testing.T) {
	manager, gate := slotTestManager(t, "", 1, "a@example.com", "b@example.com")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	results := make(chan error, 2)
	background := withBackgroundStartup(ctx)
	for _, accountID := range []string{"a@example.com", "b@example.com"} {
		go func() {
			_, err := manager.ensureWorker(background, accountID, "", false)
			results <- err
		}()
	}
	gate.waitStarted(t, "a@example.com", "纯 Go 后端的预热")
	gate.waitStarted(t, "b@example.com", "纯 Go 后端的预热")
	if active, _, _ := manager.startupSlots.usage(); active != 0 {
		t.Fatalf("纯 Go 后端不应占用冷启动名额，实际占用 %d", active)
	}
	gate.open()
	for range 2 {
		if err := <-results; err != nil {
			t.Fatalf("Worker 启动失败: %v", err)
		}
	}
}
