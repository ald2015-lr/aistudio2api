package app

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

const (
	slowPrepareAccount = "slow@example.com"
	fastPrepareAccount = "fast@example.com"
)

// blockingStep 让 stub Worker 的 proof 或 Cookie 导出停在执行中，直到测试放行
type blockingStep struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func newBlockingStep(t *testing.T) *blockingStep {
	step := &blockingStep{started: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(step.unblock)
	return step
}

func (step *blockingStep) run(ctx context.Context) error {
	close(step.started)
	select {
	case <-step.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (step *blockingStep) unblock() { step.once.Do(func() { close(step.release) }) }

func (step *blockingStep) waitStarted(t *testing.T) {
	t.Helper()
	select {
	case <-step.started:
	case <-time.After(5 * time.Second):
		t.Fatal("stub Worker 没有开始执行")
	}
}

// prepareTestManager 创建两个账户都已有就绪 Worker 的管理器；slow 账户使用 hooks，fast 账户的 proof 与 Cookie 导出立即完成
func prepareTestManager(t *testing.T, hooks aistudio.StubWorkerHooks) (*accountWorkerManager, func() int) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	pool, accounts := testAccountPool(t, slowPrepareAccount, fastPrepareAccount)
	manager := newAccountWorkerManager(pool, accounts, newRequestRegistry(ctx), "", "", time.Minute, 2, 2, 1, false)
	t.Cleanup(func() { _ = manager.Close() })
	slow, slowClosed := aistudio.NewStubWorkerWithHooks(slowPrepareAccount, hooks)
	fast, _ := aistudio.NewStubWorkerWithHooks(fastPrepareAccount, aistudio.StubWorkerHooks{
		Proof:          func(context.Context) (string, error) { return "!fast", nil },
		StorageCookies: func(context.Context) ([]byte, error) { return []byte("[]"), nil },
	})
	for accountID, worker := range map[string]*aistudio.NativeWorker{slowPrepareAccount: slow, fastPrepareAccount: fast} {
		bootstrapModel, err := pool.BootstrapModel(accountID)
		if err != nil {
			t.Fatal(err)
		}
		account := manager.accounts[accountID]
		account.worker = worker
		account.bootstrapModel = bootstrapModel
		account.warm.Store(true)
	}
	return manager, slowClosed
}

func protectedTestRequest() aistudio.ProtectedRequest {
	return aistudio.ProtectedRequest{Body: []byte(`[null]`), ProofField: 1}
}

// finishWithin 在 limit 内完成 check，超时说明 check 被慢账户的 proof 卡住
func finishWithin(t *testing.T, limit time.Duration, what string, check func() error) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- check() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	case <-time.After(limit):
		t.Fatalf("%s 被同账户进行中的 WAA 准备阻塞超过 %s", what, limit)
	}
}

// 慢账户生成 proof 期间不持有账户锁：同账户的状态读取、预热巡检与取 Worker，以及其他账户的完整准备都立即完成
func TestPrepareDoesNotHoldAccountLock(t *testing.T) {
	proof := newBlockingStep(t)
	manager, _ := prepareTestManager(t, aistudio.StubWorkerHooks{Proof: func(ctx context.Context) (string, error) {
		return "!slow", proof.run(ctx)
	}})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	preparer, err := manager.Worker(ctx, slowPrepareAccount, testRuntimeModel)
	if err != nil {
		t.Fatal(err)
	}
	prepared := make(chan error, 1)
	go func() {
		_, err := preparer.Prepare(ctx, protectedTestRequest())
		prepared <- err
	}()
	proof.waitStarted(t)

	finishWithin(t, time.Second, "同账户的状态读取", func() error {
		if manager.WorkerFailed(slowPrepareAccount) {
			return errors.New("生成 proof 中的 Worker 不应判为失败")
		}
		if available, _ := manager.runtimeAvailable([]string{slowPrepareAccount}); len(available) != 1 {
			return errors.New("生成 proof 中的账户不应判为被其他进程占用")
		}
		manager.sweepFailedWorkers(ctx)
		if occupied := manager.occupiedWorkers(); occupied.slots != 2 {
			return errors.New("占用槽位统计错误")
		}
		return nil
	})
	finishWithin(t, time.Second, "同账户的其他请求取 Worker", func() error {
		_, err := manager.Worker(ctx, slowPrepareAccount, testRuntimeModel)
		return err
	})
	finishWithin(t, time.Second, "其他账户的请求", func() error {
		other, err := manager.Worker(ctx, fastPrepareAccount, testRuntimeModel)
		if err != nil {
			return err
		}
		if _, err := other.Prepare(ctx, protectedTestRequest()); err != nil {
			return err
		}
		_, err = other.BrowserStorageState(ctx)
		return err
	})

	proof.unblock()
	if err := <-prepared; err != nil {
		t.Fatalf("Worker 未被替换时 Prepare 应成功，实际 %v", err)
	}
}

// proof 生成期间 Worker 被替换（发布了新 Worker）：proof 来自已不再发布的 Worker，按 Worker 已更新返回，由上层重放
func TestPrepareDetectsWorkerReplacedDuringProof(t *testing.T) {
	proof := newBlockingStep(t)
	manager, _ := prepareTestManager(t, aistudio.StubWorkerHooks{Proof: func(ctx context.Context) (string, error) {
		return "!slow", proof.run(ctx)
	}})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	preparer, err := manager.Worker(ctx, slowPrepareAccount, testRuntimeModel)
	if err != nil {
		t.Fatal(err)
	}
	prepared := make(chan error, 1)
	go func() {
		_, err := preparer.Prepare(ctx, protectedTestRequest())
		prepared <- err
	}()
	proof.waitStarted(t)

	account := manager.accounts[slowPrepareAccount]
	replacement, _ := aistudio.NewStubWorker(slowPrepareAccount, 0)
	finishWithin(t, time.Second, "替换账户 Worker", func() error {
		account.mu.Lock()
		account.worker = replacement
		account.generation.Add(1)
		account.mu.Unlock()
		return nil
	})

	proof.unblock()
	if err := <-prepared; !errors.Is(err, errAccountWorkerReplaced) {
		t.Fatalf("proof 期间 Worker 被替换应返回 Worker 已更新，实际 %v", err)
	}
	if _, err := preparer.SendProtected(ctx, protectedTestRequest()); !errors.Is(err, errAccountWorkerReplaced) {
		t.Fatalf("旧 Worker 不应再发送请求，实际 %v", err)
	}
}

// proof 生成期间账户被重置：关闭要等进行中的 proof 结束，等待期间其他账户的请求不受影响；
// proof 结束后关闭完成，本次准备按 Worker 已更新返回
func TestPrepareDetectsResetDuringProof(t *testing.T) {
	proof := newBlockingStep(t)
	manager, slowClosed := prepareTestManager(t, aistudio.StubWorkerHooks{Proof: func(ctx context.Context) (string, error) {
		return "!slow", proof.run(ctx)
	}})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	preparer, err := manager.Worker(ctx, slowPrepareAccount, testRuntimeModel)
	if err != nil {
		t.Fatal(err)
	}
	prepared := make(chan error, 1)
	go func() {
		_, err := preparer.Prepare(ctx, protectedTestRequest())
		prepared <- err
	}()
	proof.waitStarted(t)

	reset := make(chan error, 1)
	go func() { reset <- manager.Reset(slowPrepareAccount) }()
	account := manager.accounts[slowPrepareAccount]
	deadline := time.Now().Add(5 * time.Second)
	for account.mu.TryLock() {
		account.mu.Unlock()
		if time.Now().After(deadline) {
			t.Fatal("Reset 没有开始关闭 Worker")
		}
		time.Sleep(time.Millisecond)
	}
	finishWithin(t, time.Second, "重置等待期间其他账户的请求", func() error {
		other, err := manager.Worker(ctx, fastPrepareAccount, testRuntimeModel)
		if err != nil {
			return err
		}
		_, err = other.Prepare(ctx, protectedTestRequest())
		return err
	})
	finishWithin(t, time.Second, "重置等待期间的状态统计", func() error {
		manager.occupiedSlotsApprox(nil)
		manager.ReadyWarmAccountIDs()
		return nil
	})

	proof.unblock()
	if err := <-prepared; !errors.Is(err, errAccountWorkerReplaced) {
		t.Fatalf("proof 期间账户被重置应返回 Worker 已更新，实际 %v", err)
	}
	if err := <-reset; err != nil {
		t.Fatalf("Reset: %v", err)
	}
	if slowClosed() != 1 || account.worker != nil {
		t.Fatalf("重置应关闭旧 Worker: closed=%d worker=%v", slowClosed(), account.worker != nil)
	}
}

// Cookie 导出同样在账户锁外执行：导出期间 Worker 被替换时不返回旧浏览器的 Cookie，避免写回账户状态
func TestBrowserStorageStateDetectsWorkerReplaced(t *testing.T) {
	export := newBlockingStep(t)
	manager, _ := prepareTestManager(t, aistudio.StubWorkerHooks{StorageCookies: func(ctx context.Context) ([]byte, error) {
		return []byte("[]"), export.run(ctx)
	}})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	preparer, err := manager.Worker(ctx, slowPrepareAccount, testRuntimeModel)
	if err != nil {
		t.Fatal(err)
	}
	exported := make(chan error, 1)
	go func() {
		_, err := preparer.BrowserStorageState(ctx)
		exported <- err
	}()
	export.waitStarted(t)

	finishWithin(t, time.Second, "导出 Cookie 期间的状态读取", func() error {
		if manager.WorkerFailed(slowPrepareAccount) {
			return errors.New("导出 Cookie 中的 Worker 不应判为失败")
		}
		return nil
	})
	account := manager.accounts[slowPrepareAccount]
	replacement, _ := aistudio.NewStubWorker(slowPrepareAccount, 0)
	account.mu.Lock()
	account.worker = replacement
	account.generation.Add(1)
	account.mu.Unlock()

	export.unblock()
	if err := <-exported; !errors.Is(err, errAccountWorkerReplaced) {
		t.Fatalf("导出期间 Worker 被替换应返回 Worker 已更新，实际 %v", err)
	}
}

// 关闭失败的 Worker 仍挂在账户上（WorkerClosing）：之前拿到的 preparer 不能再用它准备或发送请求，
// 状态保持 WorkerClosing，后台清理重试照常重新关闭
func TestPreparerRejectsWorkerStuckClosing(t *testing.T) {
	manager := cleanupTestManager(t)
	worker, closed := aistudio.NewStubWorker("d@example.com", 1)
	account := &accountWorker{id: "d@example.com", label: "d", worker: worker}
	account.warm.Store(true)
	manager.accounts[account.id] = account
	preparer, ready, _, err := manager.checkReadyWorker(account.id, "", true)
	if err != nil || !ready {
		t.Fatalf("应取得就绪 Worker: ready=%v err=%v", ready, err)
	}

	if err := manager.Reset(account.id); err == nil {
		t.Fatal("第一次关闭应失败")
	}
	ctx := context.Background()
	if _, err := preparer.Prepare(ctx, protectedTestRequest()); !errors.Is(err, errAccountWorkerReplaced) {
		t.Fatalf("关闭中的 Worker 不应再生成 proof，实际 %v", err)
	}
	if _, err := preparer.SendProtected(ctx, protectedTestRequest()); !errors.Is(err, errAccountWorkerReplaced) {
		t.Fatalf("关闭中的 Worker 不应再发送请求，实际 %v", err)
	}
	if _, err := preparer.BrowserStorageState(ctx); !errors.Is(err, errAccountWorkerReplaced) {
		t.Fatalf("关闭中的 Worker 不应再导出 Cookie，实际 %v", err)
	}
	if phase := worker.State().Phase; phase != aistudio.WorkerClosing {
		t.Fatalf("关闭失败的 Worker 应停在 WorkerClosing，实际 %s", phase)
	}
	manager.retryPendingCleanup()
	if closed() != 1 || account.worker != nil {
		t.Fatalf("清理重试应重新关闭卡住的 Worker: closed=%d worker=%v", closed(), account.worker != nil)
	}
}
