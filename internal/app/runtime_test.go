package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// TestGenerateFileBoundWithoutEligibleAccount 文件引用请求在没有可用账号时返回错误事件，不能拿空租约继续执行
func TestGenerateFileBoundWithoutEligibleAccount(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	requests := newRequestRegistry(ctx)
	pool := aistudio.NewAccountPool(nil, 1)
	workers := newAccountWorkerManager(pool, nil, requests, "", "", time.Minute, 1, 1, 1, false)
	defer workers.Close()
	service := &trackedService{
		lifecycle: ctx, pool: pool, requests: requests, workers: workers,
		forbidden: newForbiddenTracker(), quota: newQuotaSharing("", requests),
	}
	request := aistudio.GenerateRequest{
		ID: "req-file", Model: "gemini-test",
		Contents: []aistudio.Content{{Role: aistudio.RoleUser, Parts: []aistudio.Part{{Text: "hi"}}}},
	}
	diag := newGenerationDiagnostics(request, "未发送")
	requestCtx, requestCancel := context.WithTimeout(ctx, 5*time.Second)
	defer requestCancel()
	events := make(chan aistudio.Event, 8)

	finished := make(chan any, 1)
	go func() {
		defer func() { finished <- recover() }()
		service.generateWithRetry(ctx, requestCtx, requestCancel, time.Now(), request, "file-abc", events, diag)
	}()
	select {
	case recovered := <-finished:
		if recovered != nil {
			t.Fatalf("generateWithRetry panic: %v", recovered)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("generateWithRetry 没有结束")
	}
	var sawError bool
	for event := range events {
		if event.Kind == aistudio.EventError {
			sawError = true
			if !errors.Is(event.Err, aistudio.ErrNoEligibleAccount) {
				t.Fatalf("错误类型 = %v，期望没有可用账号", event.Err)
			}
		}
	}
	if !sawError {
		t.Fatal("没有收到错误事件")
	}
}

// TestGenerateRetryBackoff 非额度类失败的换号退避从 200ms 起翻倍，最长 2 秒
func TestGenerateRetryBackoff(t *testing.T) {
	want := []time.Duration{200 * time.Millisecond, 400 * time.Millisecond, 800 * time.Millisecond, 1600 * time.Millisecond, 2 * time.Second, 2 * time.Second}
	for index, expected := range want {
		if got := generateRetryBackoff(index + 1); got != expected {
			t.Fatalf("第 %d 次失败退避 = %s，期望 %s", index+1, got, expected)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := waitRetryBackoff(ctx, 3); !errors.Is(err, context.Canceled) {
		t.Fatalf("请求取消时应立即返回取消错误，得到 %v", err)
	}
}

// TestStartGenerateRejectsEmptyRequest 既没有系统提示也没有对话内容时在选号前返回参数错误
func TestStartGenerateRejectsEmptyRequest(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	requests := newRequestRegistry(ctx)
	pool := aistudio.NewAccountPool(nil, 1)
	service := &trackedService{
		lifecycle: ctx, pool: pool, requests: requests,
		forbidden: newForbiddenTracker(), quota: newQuotaSharing("", requests),
	}
	_, _, err := service.startGenerate(ctx, aistudio.GenerateRequest{ID: "req-empty", Model: "gemini-test"})
	if !errors.Is(err, aistudio.ErrInvalidArgument) {
		t.Fatalf("err = %v，期望参数错误", err)
	}
}
