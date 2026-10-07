package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/api"
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
