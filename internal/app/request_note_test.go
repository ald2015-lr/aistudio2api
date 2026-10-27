package app

import (
	"context"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// noteUpstream 在生成前像 aistudio.Client 按所选账户目录再整理请求那样报告一条请求说明，之后照 bareRateUpstream 正常返回
type noteUpstream struct {
	*bareRateUpstream
}

func (upstream noteUpstream) Generate(ctx context.Context, request aistudio.GenerateRequest) (<-chan aistudio.Event, error) {
	aistudio.ReportRequestNote(ctx, "视频时长 10 秒不是模型可选的时长（4–8 秒、步长 1），按最接近的 8 秒发送")
	return upstream.bareRateUpstream.Generate(ctx, request)
}

// TestGenerateLogsAttemptRequestNotes 生成服务按本次账户整理请求时报告的说明写进该请求的管理日志（WARN，来源为本次账户）
func TestGenerateLogsAttemptRequestNotes(t *testing.T) {
	const account = "note-account@example.com"
	service, _ := firstEventTestService(t, noteUpstream{&bareRateUpstream{limited: map[string]bool{}}}, 0, account)
	request := aistudio.GenerateRequest{
		ID: "req-attempt-note", Model: testRuntimeModel,
		Contents: []aistudio.Content{{Role: aistudio.RoleUser, Parts: []aistudio.Part{{Text: "hi"}}}},
	}
	requestCtx, requestCancel := context.WithTimeout(service.lifecycle, 20*time.Second)
	defer requestCancel()
	events := make(chan aistudio.Event, 8)
	go service.generateWithRetry(service.lifecycle, requestCtx, requestCancel, time.Now(), request, "", events, newGenerationDiagnostics(request, "未发送"))
	for event := range events {
		if event.Kind == aistudio.EventError {
			t.Fatalf("生成失败: %v", event.Err)
		}
	}
	service.requests.mu.Lock()
	defer service.requests.mu.Unlock()
	for _, entry := range service.requests.logs {
		if entry.Level == "WARN" && entry.Source == account && entry.Request != nil && entry.Request.ID == "req-attempt-note" &&
			entry.Message == "视频时长 10 秒不是模型可选的时长（4–8 秒、步长 1），按最接近的 8 秒发送" {
			return
		}
	}
	t.Fatalf("请求日志应记下本次账户报告的说明: %+v", service.requests.logs)
}
