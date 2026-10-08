package app

import (
	"context"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/api"
	"github.com/Mag1cFall/AIStudio2API/internal/config"
)

func cleanupTestManager(t *testing.T) *accountWorkerManager {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return &accountWorkerManager{
		accounts: make(map[string]*accountWorker), requests: newRequestRegistry(ctx),
		signal: make(chan struct{}), dispatch: newDispatchQueue(), victims: make(map[string]struct{}),
		openings: make(map[string]chan struct{}),
	}
}

// 主 Worker 关闭失败（浏览器关不掉，停在 WorkerClosing）后，后台清理重试应重新关闭并让账户恢复可用
func TestRetryPendingCleanupRetriesStuckMainWorker(t *testing.T) {
	manager := cleanupTestManager(t)
	worker, closed := aistudio.NewStubWorker("a@example.com", 1)
	account := &accountWorker{id: "a@example.com", label: "a", worker: worker}
	account.warm.Store(true)
	manager.accounts[account.id] = account

	if err := manager.Reset(account.id); err == nil {
		t.Fatal("第一次关闭应失败")
	}
	if _, _, _, err := manager.checkReadyWorker(account.id, "", true); !errors.Is(err, errAccountWorkerCleanupPending) {
		t.Fatalf("关闭失败后应处于待清理状态，实际 %v", err)
	}
	manager.retryPendingCleanup()
	if closed() != 1 || account.worker != nil || account.warm.Load() {
		t.Fatalf("清理重试未重新关闭卡住的主 Worker: closed=%d worker=%v warm=%v", closed(), account.worker != nil, account.warm.Load())
	}
	if _, _, _, err := manager.checkReadyWorker(account.id, "", true); errors.Is(err, errAccountWorkerCleanupPending) {
		t.Fatal("重试成功后账户不应再待清理")
	}
}

// 只有替换 Worker 残留时，清理重试不能把正在服务请求的健康主 Worker 一并关闭
func TestRetryPendingCleanupKeepsHealthyMainWorker(t *testing.T) {
	manager := cleanupTestManager(t)
	primary, primaryClosed := aistudio.NewStubWorker("c@example.com", 0)
	leftover, leftoverClosed := aistudio.NewStubWorker("c@example.com", 0)
	account := &accountWorker{id: "c@example.com", label: "c", worker: primary, cleanupWorker: leftover}
	account.warm.Store(true)
	manager.accounts[account.id] = account

	manager.retryPendingCleanup()

	if primaryClosed() != 0 || account.worker != primary {
		t.Fatalf("健康主 Worker 被清理重试关闭: closed=%d", primaryClosed())
	}
	if leftoverClosed() != 1 || account.cleanupWorker != nil {
		t.Fatalf("残留的替换 Worker 未被清理: closed=%d", leftoverClosed())
	}
	if !account.warm.Load() {
		t.Fatal("主 Worker 仍在时不应清除 warm")
	}
}

type restartTestService struct {
	aistudio.Service
	state *atomic.Value
}

func (service restartTestService) State() string { return service.state.Load().(string) }

type restartTestAdmin struct {
	api.AdminService
	state *atomic.Value
}

func (admin restartTestAdmin) Status(context.Context) (api.AdminStatus, error) {
	return api.AdminStatus{State: admin.state.Load().(string)}, nil
}

func (admin restartTestAdmin) StopService(context.Context) (api.AdminStatus, error) {
	admin.state.Store("STOPPED")
	return api.AdminStatus{State: "STOPPED"}, nil
}

func (admin restartTestAdmin) StartService(context.Context) (api.AdminStatus, error) {
	admin.state.Store("RUNNING")
	return api.AdminStatus{State: "RUNNING"}, nil
}

func restartTestGeneration(state string) *runtimeGeneration {
	value := &atomic.Value{}
	value.Store(state)
	return &runtimeGeneration{
		service: restartTestService{state: value}, admin: restartTestAdmin{state: value},
		lifecycleCancel: func() {}, closeRuntime: func() error { return nil },
	}
}

// 应用配置的重启在等待进行中请求期间用户按了停止：请求结束后不应再把服务拉起来
func TestRestartServiceRespectsStopDuringDrain(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var launched atomic.Int32
	manager := &runtimeManager{
		lifecycle: ctx, requests: newRequestRegistry(ctx), intent: &serviceIntent{},
		current:    restartTestGeneration("RUNNING"),
		configPath: filepath.Join(t.TempDir(), ".env"),
		factory: func(context.Context, context.Context, config.Config, *requestRegistry) (*runtimeGeneration, error) {
			launched.Add(1)
			return restartTestGeneration("STOPPED"), nil
		},
	}
	manager.intent.running.Store(true)
	manager.requests.start(aistudio.GenerateRequest{ID: "r1"}, func() {})

	done := make(chan error, 1)
	go func() {
		_, err := manager.RestartService(ctx)
		done <- err
	}()
	time.Sleep(200 * time.Millisecond)
	if _, err := manager.StopService(ctx); err != nil {
		t.Fatal(err)
	}
	manager.requests.finish("r1", "completed", nil)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if launched.Load() != 0 || manager.intent.running.Load() {
		t.Fatalf("用户停止被等待中的重启覆盖: launched=%d running=%v", launched.Load(), manager.intent.running.Load())
	}
}
