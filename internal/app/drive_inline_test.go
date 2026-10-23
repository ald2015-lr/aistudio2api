package app

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// driveInlineUpstream 模拟 aistudio.PooledService 的内联附件处理：driveOK 为假时像账号没有 Drive 授权一样原样返回 inline data
// （附件改为内联发送），为真时改写为 Drive 文件引用；生成照 bareRateUpstream 正常返回。记录 ForgetDriveUnavailable 的调用
type driveInlineUpstream struct {
	*bareRateUpstream
	driveMu   sync.Mutex
	driveOK   bool
	forgotten []string
}

func (upstream *driveInlineUpstream) UploadInlineMediaToLease(
	_ context.Context, _ *aistudio.AccountLease, contents []aistudio.Content, temporary *aistudio.TemporaryFileCopies,
) ([]aistudio.Content, *aistudio.TemporaryFileCopies, error) {
	upstream.driveMu.Lock()
	driveOK := upstream.driveOK
	upstream.driveMu.Unlock()
	if !driveOK {
		return contents, temporary, nil
	}
	rewritten := make([]aistudio.Content, len(contents))
	for index, content := range contents {
		rewritten[index] = content
		rewritten[index].Parts = append([]aistudio.Part(nil), content.Parts...)
		for partIndex := range rewritten[index].Parts {
			part := &rewritten[index].Parts[partIndex]
			if part.InlineData != nil {
				part.File = &aistudio.FileRef{ID: "drive-file", Name: "inline-media-1", MIME: part.InlineData.MIME}
				part.InlineData = nil
			}
		}
	}
	return rewritten, aistudio.NewTemporaryFileCopiesForTest(nil), nil
}

func (upstream *driveInlineUpstream) ForgetDriveUnavailable(accountID string) {
	upstream.driveMu.Lock()
	defer upstream.driveMu.Unlock()
	upstream.forgotten = append(upstream.forgotten, accountID)
}

// runInlineImageRequest 在指定账号上执行一次带内联图片的生成，返回正文与错误
func runInlineImageRequest(t *testing.T, service *trackedService, requestID string, accountID string) (string, error) {
	t.Helper()
	request := aistudio.GenerateRequest{
		ID: requestID, Model: testRuntimeModel, AccountID: accountID,
		Contents: []aistudio.Content{{Role: aistudio.RoleUser, Parts: []aistudio.Part{
			{Text: "看图"}, {InlineData: &aistudio.Blob{MIME: "image/png", Data: []byte("png")}},
		}}},
	}
	requestCtx, requestCancel := context.WithTimeout(service.lifecycle, 20*time.Second)
	defer requestCancel()
	events := make(chan aistudio.Event, 8)
	go service.generateWithRetry(service.lifecycle, requestCtx, requestCancel, time.Now(), request, "", events, newGenerationDiagnostics(request, "未发送"))
	var text string
	var err error
	for event := range events {
		switch event.Kind {
		case aistudio.EventText:
			text += event.Text
		case aistudio.EventError:
			err = event.Err
		}
	}
	return text, err
}

// TestDriveInlineFallbackLog 账号没有 Drive 授权、附件改为内联发送：请求在同一账号上完成（不换号、不冷却），写“Drive 未授权，附件改为内联”
// （带附件数与原始字节数，同账号 10 分钟最多一行，走 logDurable：控制台队列满时也写进运行日志），不写“内联附件处理完成”；
// Drive 可用时照旧写“内联附件处理完成”
func TestDriveInlineFallbackLog(t *testing.T) {
	const account = "drive-inline@example.com"
	upstream := &driveInlineUpstream{bareRateUpstream: &bareRateUpstream{limited: map[string]bool{}}}
	service, pool := firstEventTestService(t, upstream, 0, account)
	// 普通控制台队列长度为 0、没有写入协程：普通日志一律按队列满丢弃，只有 logDurable 的行排进独立队列
	registry, _ := newConsoleTestRegistry(0, 8)
	service.requests = registry
	for index, requestID := range []string{"req-inline-1", "req-inline-2"} {
		text, err := runInlineImageRequest(t, service, requestID, account)
		if err != nil || text != "hello" {
			t.Fatalf("第 %d 次：text=%q err=%v，期望在同一账号上内联发送完成", index+1, text, err)
		}
	}
	if calls := upstream.called(); len(calls) != 2 || calls[0] != account+"/"+string(aistudio.ChannelPlayground) {
		t.Fatalf("上游生成调用 = %v，期望两次都在原账号的 Playground 通道", calls)
	}
	if got := countSourceLogs(service.requests, account, "Drive 未授权，附件改为内联 | 附件=1 | 原始=3B | 耗时="); got != 1 {
		t.Fatalf("“Drive 未授权，附件改为内联”%d 条，期望同账号 10 分钟内只写 1 条；日志 = %v", got, serviceLogMessages(service.requests))
	}
	if got := countSourceLogs(service.requests, account, "内联附件处理完成"); got != 0 {
		t.Fatalf("改为内联时不应写“内联附件处理完成”：日志 = %v", serviceLogMessages(service.requests))
	}
	if got := countSourceLogs(service.requests, account, "账号切换"); got != 0 {
		t.Fatalf("改为内联时不应换号：日志 = %v", serviceLogMessages(service.requests))
	}
	if status, _ := pool.StatusOf(account); len(status.Cooldowns) != 0 {
		t.Fatalf("改为内联时不应冷却账号：%+v", status.Cooldowns)
	}
	durable := durableMessages(registry)
	if len(durable) != 1 || durable[0].Source != account || !strings.HasPrefix(durable[0].Message, "Drive 未授权，附件改为内联 | 附件=1 | 原始=3B") {
		t.Fatalf("独立队列里的日志 = %+v，期望只有 1 条“Drive 未授权，附件改为内联”（控制台队列满时也不丢）", durable)
	}
	if registry.consoleDropped.Load() == 0 {
		t.Fatal("普通日志应因控制台队列满被丢弃并计数，测试前提不成立")
	}

	upstream.driveMu.Lock()
	upstream.driveOK = true
	upstream.driveMu.Unlock()
	if text, err := runInlineImageRequest(t, service, "req-inline-drive", account); err != nil || text != "hello" {
		t.Fatalf("Drive 可用时：text=%q err=%v", text, err)
	}
	if got := countSourceLogs(service.requests, account, "内联附件处理完成 | 附件=1 | 原始=3B"); got != 1 {
		t.Fatalf("Drive 可用时应写 1 条“内联附件处理完成”：日志 = %v", serviceLogMessages(service.requests))
	}
	if got := countSourceLogs(service.requests, account, "Drive 未授权"); got != 1 {
		t.Fatalf("Drive 可用时不应再写“Drive 未授权”：日志 = %v", serviceLogMessages(service.requests))
	}

	// 人工恢复登录（管理验证通过等）清掉生成服务里这个账号的“Drive 不可用”记忆
	service.noteLoginRestored(account)
	upstream.driveMu.Lock()
	forgotten := append([]string(nil), upstream.forgotten...)
	upstream.driveMu.Unlock()
	if len(forgotten) != 1 || forgotten[0] != account {
		t.Fatalf("ForgetDriveUnavailable 调用 = %v，期望一次 %s", forgotten, account)
	}
}

// TestDriveInlineNotesInterval 同账号 10 分钟最多一条，不同账号互不影响；记录达到清理门槛时清掉过期的
func TestDriveInlineNotesInterval(t *testing.T) {
	var notes driveInlineNotes
	now := time.Now()
	if !notes.allow("a", now) || notes.allow("a", now.Add(driveInlineNoteInterval-time.Second)) {
		t.Fatal("同账号 10 分钟内只应允许一条")
	}
	if !notes.allow("b", now) {
		t.Fatal("不同账号互不影响")
	}
	if !notes.allow("a", now.Add(driveInlineNoteInterval)) {
		t.Fatal("满 10 分钟后应允许下一条")
	}
	later := now.Add(2 * driveInlineNoteInterval)
	for index := range driveInlineNotePrune {
		notes.logged[fmt.Sprintf("old-%d", index)] = now
	}
	notes.allow("c", later)
	notes.mu.Lock()
	remaining := len(notes.logged)
	notes.mu.Unlock()
	if remaining != 1 {
		t.Fatalf("清理后剩 %d 条，期望只剩刚写入的 1 条", remaining)
	}
}

// TestDriveInlineBuildChannelNoNote Build 通道本来就直接携带 inlineData（UploadInlineMediaToLease 原样返回）：附件仍是 inline data
// 不说明账号没有 Drive 授权，只写“内联附件处理完成”，不写“Drive 未授权，附件改为内联”，也不换号、不冷却
func TestDriveInlineBuildChannelNoNote(t *testing.T) {
	const account = "drive-inline-build@example.com"
	upstream := &driveInlineUpstream{bareRateUpstream: &bareRateUpstream{limited: map[string]bool{}}}
	service, pool := firstEventTestService(t, upstream, 0, account)
	pool.SetUpstreamChannels([]aistudio.Channel{aistudio.ChannelBuild})
	// testAccountPool 的目录带 transcription_output，这样的模型不走 Build：改成普通对话模型
	chat := aistudio.Model{ID: testRuntimeModel, Methods: []string{"generateContent"}, Capabilities: map[string]bool{"chat_model": true}}
	if err := pool.SetCatalog(account, aistudio.BenefitTierFree, []aistudio.Model{chat}); err != nil {
		t.Fatal(err)
	}
	if err := pool.SetBuildCatalog(account, []aistudio.Model{chat}); err != nil {
		t.Fatal(err)
	}
	for index, requestID := range []string{"req-build-1", "req-build-2"} {
		text, err := runInlineImageRequest(t, service, requestID, account)
		if err != nil || text != "hello" {
			t.Fatalf("第 %d 次：text=%q err=%v，期望在 Build 通道上完成", index+1, text, err)
		}
	}
	if calls := upstream.called(); len(calls) != 2 || calls[0] != account+"/"+string(aistudio.ChannelBuild) || calls[1] != calls[0] {
		t.Fatalf("上游生成调用 = %v，期望两次都在原账号的 Build 通道", calls)
	}
	if got := countSourceLogs(service.requests, account, "内联附件处理完成 | 附件=1 | 原始=3B"); got != 2 {
		t.Fatalf("Build 通道应每次写“内联附件处理完成”（共 2 条），实际 %d 条；日志 = %v", got, serviceLogMessages(service.requests))
	}
	if got := countSourceLogs(service.requests, account, "Drive 未授权"); got != 0 {
		t.Fatalf("Build 通道不应写“Drive 未授权”：日志 = %v", serviceLogMessages(service.requests))
	}
	if got := countSourceLogs(service.requests, account, "账号切换"); got != 0 {
		t.Fatalf("Build 通道携带附件不应换号：日志 = %v", serviceLogMessages(service.requests))
	}
	if status, _ := pool.StatusOf(account); len(status.Cooldowns) != 0 {
		t.Fatalf("Build 通道携带附件不应冷却账号：%+v", status.Cooldowns)
	}
}
