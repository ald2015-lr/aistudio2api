package app

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/api"
	"github.com/Mag1cFall/AIStudio2API/internal/config"
)

// blockingStartService 在 startGenerate 内暂停，模拟写访问日志之前有写锁排队
type blockingStartService struct {
	aistudio.Service
	entered chan struct{}
	proceed chan struct{}
}

func (service *blockingStartService) State() string { return "RUNNING" }

func (service *blockingStartService) Models(context.Context) ([]aistudio.Model, error) {
	return []aistudio.Model{{ID: "gemini-test", Name: "gemini-test", Methods: []string{"generateContent"}}}, nil
}

// startGenerate 与 trackedService.startGenerate 一样在调用链内写访问日志
func (service *blockingStartService) startGenerate(ctx context.Context, _ aistudio.GenerateRequest) (<-chan aistudio.Event, func() error, error) {
	close(service.entered)
	<-service.proceed
	api.StartAccessLog(ctx)
	events := make(chan aistudio.Event, 1)
	events <- aistudio.Event{Kind: aistudio.EventFinish}
	close(events)
	return events, nil, nil
}

type recordingAdmin struct{ api.AdminService }

func (recordingAdmin) RecordAccessStart(api.AccessLog) {}
func (recordingAdmin) RecordAccessLog(api.AccessLog)   {}

// TestGenerateDoesNotDeadlockWithQueuedWriter 生成请求进行中保存配置（写锁排队）时请求仍能完成
func TestGenerateDoesNotDeadlockWithQueuedWriter(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	service := &blockingStartService{entered: make(chan struct{}), proceed: make(chan struct{})}
	manager := &runtimeManager{
		lifecycle: ctx, requests: newRequestRegistry(ctx), apiKey: newAPIKeyHolder("old"),
		intent:  &serviceIntent{},
		current: &runtimeGeneration{service: service, admin: recordingAdmin{}},
	}
	handler := api.NewHandler(manager, api.Config{APIKeyFunc: manager.activeAPIKey, Admin: manager})

	done := make(chan int, 1)
	go func() {
		body := `{"model":"gemini-test","messages":[{"role":"user","content":"hi"}]}`
		request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer old")
		request.Header.Set("Content-Type", "application/json")
		request.RemoteAddr = "127.0.0.1:5555"
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		done <- recorder.Code
	}()
	select {
	case <-service.entered:
	case code := <-done:
		t.Fatalf("请求在进入 startGenerate 之前结束: status %d", code)
	case <-time.After(5 * time.Second):
		t.Fatal("请求没有进入 startGenerate")
	}

	writerDone := make(chan struct{})
	go func() {
		manager.applyAPIKey("new")
		close(writerDone)
	}()
	time.Sleep(100 * time.Millisecond)
	close(service.proceed)

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("死锁：请求在访问日志回调里等待读锁，写锁在等待请求释放读锁")
	}
	select {
	case <-writerDone:
	case <-time.After(3 * time.Second):
		t.Fatal("写锁没有拿到")
	}
}

// stoppedService 为已停止的生成服务
type stoppedService struct{ aistudio.Service }

func (stoppedService) State() string { return "STOPPED" }

type stoppedAdmin struct{ api.AdminService }

func (stoppedAdmin) Status(context.Context) (api.AdminStatus, error) { return api.AdminStatus{}, nil }
func (stoppedAdmin) StopService(context.Context) (api.AdminStatus, error) {
	return api.AdminStatus{}, nil
}

// TestUserStopIsNotOverridden 用户停止之后，监督器的自动重启与停止之前排队的启动都不会再把服务拉起来
func TestUserStopIsNotOverridden(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	launched := make(chan struct{}, 2)
	manager := &runtimeManager{
		lifecycle: ctx, requests: newRequestRegistry(ctx), intent: &serviceIntent{},
		current: &runtimeGeneration{service: stoppedService{}, admin: stoppedAdmin{}},
		factory: func(context.Context, context.Context, config.Config, *requestRegistry) (*runtimeGeneration, error) {
			launched <- struct{}{}
			return nil, errors.New("不应启动")
		},
		configPath: filepath.Join(t.TempDir(), ".env"),
	}
	// 监督器：用户期望停止时不启动
	if _, err := manager.startService(ctx, false); err != nil {
		t.Fatal(err)
	}
	// 用户启动排在另一次启动之后，期间用户按了停止
	manager.startMu.Lock()
	done := make(chan error, 1)
	go func() {
		_, err := manager.StartService(ctx)
		done <- err
	}()
	time.Sleep(50 * time.Millisecond)
	if _, err := manager.StopService(ctx); err != nil {
		t.Fatal(err)
	}
	manager.startMu.Unlock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	select {
	case <-launched:
		t.Fatal("停止之后服务仍被启动")
	default:
	}
	if manager.intent.running.Load() {
		t.Fatal("停止之后期望状态仍为运行")
	}
}

// TestNoStartAfterShutdownBegins 进程开始退出后，启动（含监督器自动重启）直接放弃
func TestNoStartAfterShutdownBegins(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	manager := &runtimeManager{
		lifecycle: ctx, requests: newRequestRegistry(ctx), intent: &serviceIntent{},
		current: &runtimeGeneration{service: stoppedService{}, admin: stoppedAdmin{}},
		factory: func(context.Context, context.Context, config.Config, *requestRegistry) (*runtimeGeneration, error) {
			t.Fatal("退出期间不应再启动")
			return nil, nil
		},
	}
	manager.beginShutdown()
	if _, err := manager.StartService(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.startService(ctx, false); err != nil {
		t.Fatal(err)
	}
}

// stoppingTestAdmin 的 StopService 等 release 关闭后才返回，模拟停止步骤（cancel_requests 到 flush_writes）进行中
type stoppingTestAdmin struct {
	restartTestAdmin
	entered chan struct{}
	release chan struct{}
}

func (admin stoppingTestAdmin) StopService(ctx context.Context) (api.AdminStatus, error) {
	close(admin.entered)
	<-admin.release
	return admin.restartTestAdmin.StopService(ctx)
}

type startCallResult struct {
	status api.AdminStatus
	err    error
}

// TestStartWaitsForInProgressTransition 启动或停止进行中调用启动（POST /api/control/start）时，接口等进行中的启动或停止结束后再处理，
// 不返回“正在切换状态”之类的错误（README 的 service_stopped 处理办法）：另一次启动（监督器自动重新启动、保存设置后的重启）进行中时
// 排在它之后，结束后拿到 RUNNING、不报错，也不再启动一次；停止步骤进行中时先等停止结束，再启动新一代
func TestStartWaitsForInProgressTransition(t *testing.T) {
	t.Run("启动进行中", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		var launched atomic.Int32
		current := restartTestGeneration("LAUNCHING")
		manager := &runtimeManager{
			lifecycle: ctx, requests: newRequestRegistry(ctx), intent: &serviceIntent{},
			current: current, configPath: filepath.Join(t.TempDir(), ".env"),
			factory: func(context.Context, context.Context, config.Config, *requestRegistry) (*runtimeGeneration, error) {
				launched.Add(1)
				return nil, errors.New("另一次启动进行中时不应再启动一次")
			},
		}
		manager.intent.running.Store(true)
		// 另一次启动在整个启动期间持有 startMu
		manager.startMu.Lock()
		done := make(chan startCallResult, 1)
		go func() {
			status, err := manager.StartService(ctx)
			done <- startCallResult{status, err}
		}()
		select {
		case result := <-done:
			manager.startMu.Unlock()
			t.Fatalf("另一次启动进行中时启动立即返回: state=%q err=%v", result.status.State, result.err)
		case <-time.After(200 * time.Millisecond):
		}
		current.service.(restartTestService).state.Store("RUNNING")
		manager.startMu.Unlock()
		select {
		case result := <-done:
			if result.err != nil || result.status.State != "RUNNING" || launched.Load() != 0 {
				t.Fatalf("另一次启动结束后应拿到 RUNNING、不报错、不再启动: state=%q err=%v launched=%d",
					result.status.State, result.err, launched.Load())
			}
		case <-time.After(5 * time.Second):
			t.Fatal("另一次启动结束后启动没有返回")
		}
	})
	t.Run("停止步骤进行中", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		path := filepath.Join(t.TempDir(), ".env")
		if err := config.Default().Save(path); err != nil {
			t.Fatal(err)
		}
		state := &atomic.Value{}
		state.Store("STOPPED")
		admin := stoppingTestAdmin{
			restartTestAdmin: restartTestAdmin{state: state}, entered: make(chan struct{}), release: make(chan struct{}),
		}
		errLaunched := errors.New("新一代已开始创建")
		var launched atomic.Int32
		manager := &runtimeManager{
			lifecycle: ctx, requests: newRequestRegistry(ctx), intent: &serviceIntent{}, configPath: path,
			current: &runtimeGeneration{
				service: restartTestService{state: state}, admin: admin,
				lifecycleCancel: func() {}, closeRuntime: func() error { return nil },
			},
			factory: func(context.Context, context.Context, config.Config, *requestRegistry) (*runtimeGeneration, error) {
				launched.Add(1)
				return nil, errLaunched
			},
		}
		done := make(chan startCallResult, 1)
		go func() {
			status, err := manager.StartService(ctx)
			done <- startCallResult{status, err}
		}()
		select {
		case <-admin.entered:
		case <-time.After(5 * time.Second):
			t.Fatal("停止步骤进行中时启动没有先等停止结束")
		}
		select {
		case result := <-done:
			close(admin.release)
			t.Fatalf("停止步骤进行中时启动立即返回: state=%q err=%v", result.status.State, result.err)
		case <-time.After(200 * time.Millisecond):
		}
		if launched.Load() != 0 {
			close(admin.release)
			t.Fatal("停止还没结束就开始创建新一代")
		}
		close(admin.release)
		select {
		case result := <-done:
			if !errors.Is(result.err, errLaunched) || launched.Load() != 1 {
				t.Fatalf("停止结束后应创建新一代: err=%v launched=%d", result.err, launched.Load())
			}
		case <-time.After(5 * time.Second):
			t.Fatal("停止结束后启动没有返回")
		}
	})
}
