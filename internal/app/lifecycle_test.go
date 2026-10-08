package app

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
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

func (stoppedAdmin) Status(context.Context) (api.AdminStatus, error)      { return api.AdminStatus{}, nil }
func (stoppedAdmin) StopService(context.Context) (api.AdminStatus, error) { return api.AdminStatus{}, nil }

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
