package app

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/api"
)

// TestStartupFailureRegistryRoundTrip 失效账户记录合并写入 .startup-failures.json，重新读取后内容不变；同一登录签名下同一类别连续计数，
// 登录签名或类别变化时从头计；原因不确定的失败不持久化
func TestStartupFailureRegistryRoundTrip(t *testing.T) {
	directory := t.TempDir()
	registry := newStartupFailureRegistry(directory, nil)
	first := time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)
	if count := registry.record("a@example.com", startupFailureAuth, "https://accounts.google.com/x", "跳转", 5, "id-1", first); count != 1 {
		t.Fatalf("第一次计数 = %d", count)
	}
	if count := registry.record("a@example.com", startupFailureAuth, "https://accounts.google.com/y", "再次跳转", 5, "id-1", first.Add(time.Minute)); count != 2 {
		t.Fatalf("同一登录签名第二次计数 = %d", count)
	}
	if count := registry.record("b@example.com", startupFailureFiles, "", "解析 storage state", 1, "id-b", first); count != 1 {
		t.Fatalf("认证文件损坏计数 = %d", count)
	}
	if count := registry.record("c@example.com", startupFailureAmbiguous, "", "输入框未就绪", 5, "id-c", first); count != 0 {
		t.Fatalf("原因不确定的失败不应持久化，计数 = %d", count)
	}
	registry.flush()
	if _, err := os.Stat(filepath.Join(directory, startupFailureFileName)); err != nil {
		t.Fatalf("应写入 %s: %v", startupFailureFileName, err)
	}
	reloaded := newStartupFailureRegistry(directory, nil)
	entries := reloaded.list()
	if len(entries) != 2 || entries[0].accountID != "a@example.com" || entries[1].accountID != "b@example.com" {
		t.Fatalf("重新读取的记录 = %+v", entries)
	}
	record := entries[0].record
	if record.Kind != "auth" || record.Count != 2 || record.URL != "https://accounts.google.com/y" || record.Message != "再次跳转" ||
		record.Stage != 5 || record.Identity != "id-1" || !record.FirstAt.Equal(first) || !record.LastAt.Equal(first.Add(time.Minute)) {
		t.Fatalf("记录 = %+v", record)
	}
	if count := reloaded.record("a@example.com", startupFailureAuth, "", "新认证", 5, "id-2", first.Add(2*time.Minute)); count != 1 {
		t.Fatalf("登录签名变化后应从头计，计数 = %d", count)
	}
	if count := reloaded.record("a@example.com", startupFailureFiles, "", "坏了", 1, "id-2", first.Add(3*time.Minute)); count != 1 {
		t.Fatalf("类别变化后应从头计，计数 = %d", count)
	}
	reloaded.setIdentity("b@example.com", "id-b2")
	if entries := reloaded.list(); entries[len(entries)-1].record.Identity != "id-b2" {
		t.Fatalf("续签后应更新记下的登录签名，记录 = %+v", entries)
	}
}

// TestReconcileStartupFailures 重启时对照持久表与账户池：不在池里的删除；登录签名不同的删除；同一登录签名下连续 2 次登录失效、
// 没有续签材料的重新标为需要重新登录；只有 1 次的保留展示、账户不变
func TestReconcileStartupFailures(t *testing.T) {
	const twice, once, replaced, files = "twice@example.com", "once@example.com", "replaced@example.com", "files@example.com"
	service := startupTestService(t, newStartupTestLaunch(), 2, twice, once, replaced, files)
	manager := service.workers
	registry := manager.startupFailures
	now := time.Now()
	identity := func(accountID string) string {
		value, _ := service.pool.LoginIdentity(accountID)
		return value
	}
	registry.record(twice, startupFailureAuth, "https://accounts.google.com/a", "跳转", 5, identity(twice), now)
	registry.record(twice, startupFailureAuth, "https://accounts.google.com/a", "跳转", 5, identity(twice), now)
	registry.record(once, startupFailureAuth, "https://accounts.google.com/b", "跳转", 5, identity(once), now)
	registry.record(replaced, startupFailureAuth, "", "跳转", 5, "old-identity", now)
	registry.record(replaced, startupFailureAuth, "", "跳转", 5, "old-identity", now)
	registry.record(files, startupFailureFiles, "", "解析 storage state", 1, identity(files), now)
	registry.record("gone@example.com", startupFailureAuth, "", "跳转", 5, "x", now)

	manager.reconcileStartupFailures()
	if status := testStatus(t, service.pool, twice); status.State != aistudio.AccountAuthRequired || !strings.Contains(status.Message, "连续 2 次") {
		t.Fatalf("连续 2 次登录失效应重新标为需要重新登录，实际 %s %q", status.State, status.Message)
	}
	if status := testStatus(t, service.pool, once); status.State != aistudio.AccountReady {
		t.Fatalf("只有 1 次的应让账户重试，实际 %s", status.State)
	}
	listed := make(map[string]bool)
	for _, entry := range registry.list() {
		listed[entry.accountID] = true
	}
	if !listed[twice] || !listed[once] || !listed[files] || listed[replaced] || listed["gone@example.com"] {
		t.Fatalf("对账后的记录 = %v", listed)
	}
	if !hasLogContaining(service.requests, "失效账户记录对账", "重新标为需要重新登录=1", "清除已不在账户池中=1", "清除认证文件已更新=1") {
		t.Fatalf("应记录对账结果，日志 = %v", serviceLogMessages(service.requests))
	}
	// 重新标为需要重新登录的列为确定失效；只保留展示、让账户重试一次的（只有 1 次的登录失效、认证文件损坏）账户仍为就绪，
	// 列为未确认，不能被一键删除
	failures, err := (&runtimeAdmin{pool: service.pool, workers: manager, requests: service.requests}).StartupFailures(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	confirmed := make(map[string]bool)
	for _, failure := range failures {
		confirmed[failure.ID] = failure.Confirmed
	}
	if len(failures) != 3 || !confirmed[twice] || confirmed[once] || confirmed[files] {
		t.Fatalf("对账后的列表 = %+v", failures)
	}
}

// startupHookAdmin 返回带内存中失效账户持久表的管理端，账户都有一条登录失效记录与一次原因不确定的暂停
func startupHookAdmin(t *testing.T, accountIDs ...string) (*runtimeAdmin, *startupFailureRegistry) {
	t.Helper()
	admin, _, _ := deleteTestAdmin(t, accountIDs...)
	registry := newStartupFailureRegistry("", admin.requests)
	admin.workers.startupFailures = registry
	admin.workers.lastStartupSuccess.Store(time.Now().UnixNano())
	for _, accountID := range accountIDs {
		registry.record(accountID, startupFailureAuth, "https://accounts.google.com/x", "跳转", 5, "", time.Now())
		admin.workers.notePausedFailure(workerOf(t, admin.workers, accountID), startupFailureAmbiguous, admin.pool.AuthGeneration(accountID), 5, pageNotReadyError(), time.Now())
	}
	return admin, registry
}

func workerOf(t *testing.T, manager *accountWorkerManager, accountID string) *accountWorker {
	t.Helper()
	manager.mu.RLock()
	defer manager.mu.RUnlock()
	account := manager.accounts[accountID]
	if account == nil {
		t.Fatalf("账户 %s 没有 Worker 配置", accountID)
	}
	return account
}

func recorded(registry *startupFailureRegistry, accountID string) bool {
	for _, entry := range registry.list() {
		if entry.accountID == accountID {
			return true
		}
	}
	return false
}

// TestStartupFailureClearHooks 失效账户记录按精确 ID 在这些地方清除：删除账户（单个与批量，含号池里已经没有的）、账户目录被移走、
// 认证文件更新、验证通过；启动成功同样清除。内存里的暂停随认证文件更新与验证通过一起清除
func TestStartupFailureClearHooks(t *testing.T) {
	isolateDisableNotes(t)
	const deleted, detached, updated, verified, started, kept = "del@example.com", "detach@example.com", "update@example.com",
		"verify@example.com", "start@example.com", "keep@example.com"
	admin, registry := startupHookAdmin(t, deleted, detached, updated, verified, started, kept)
	registry.record("ghost@example.com", startupFailureAuth, "", "跳转", 5, "", time.Now())
	registry.record("Ghost2@Example.com", startupFailureAuth, "", "跳转", 5, "", time.Now())

	if err := admin.DeleteAccount(context.Background(), deleted); err != nil {
		t.Fatal(err)
	}
	if recorded(registry, deleted) {
		t.Fatal("删除账户应清除失效账户记录")
	}
	result, err := admin.DeleteAccounts(context.Background(), api.BulkDeleteInput{AccountIDs: []string{"ghost@example.com", "ghost2@example.com"}})
	if err != nil || result.Missing != 2 {
		t.Fatalf("批量删除结果 = %+v %v", result, err)
	}
	if recorded(registry, "ghost@example.com") || !recorded(registry, "Ghost2@Example.com") {
		t.Fatal("号池里已经没有的账户按精确 ID 清除记录")
	}
	if err := admin.detachAccount(detached); err != nil {
		t.Fatal(err)
	}
	if recorded(registry, detached) {
		t.Fatal("账户目录被移走应清除失效账户记录")
	}
	admin.service.authFilesUpdated(updated, "测试")
	if until, _ := startupPauseOf(t, admin.workers, updated); recorded(registry, updated) || !until.IsZero() {
		t.Fatal("认证文件更新应清除记录与暂停")
	}
	admin.login = &verifyTestLogin{verification: aistudio.LoginVerification{Authenticated: true}}
	if _, err := admin.verifyLogin(context.Background(), verified); err != nil {
		t.Fatal(err)
	}
	if until, _ := startupPauseOf(t, admin.workers, verified); recorded(registry, verified) || !until.IsZero() {
		t.Fatal("验证通过应清除记录与暂停")
	}
	admin.workers.launch = newStartupTestLaunch().launch
	if _, err := admin.workers.Worker(context.Background(), started, testRuntimeModel); err != nil {
		t.Fatal(err)
	}
	if until, _ := startupPauseOf(t, admin.workers, started); recorded(registry, started) || !until.IsZero() {
		t.Fatal("启动成功应清除记录与暂停")
	}
	if until, _ := startupPauseOf(t, admin.workers, kept); !recorded(registry, kept) || until.IsZero() {
		t.Fatal("其他账户的记录与暂停不应受影响")
	}
}

// TestStartupFailuresAdmin 失效账户列表：当前处于需要重新登录状态的账户为确定失效（confirmed，有持久记录的带上记录）；持久表里账户
// 当前不是需要重新登录状态的记录（重启后待重试、续签恢复之前留下的）与内存里的暂停为未确认，同一账户只列一次、暂停优先；清除接口
// 同时清除持久表与内存暂停，account_ids 为空且 all 为假时返回 400；状态接口的 startup 与列表一致
func TestStartupFailuresAdmin(t *testing.T) {
	isolateDisableNotes(t)
	const listed, relogin, paused, pending, both = "listed@example.com", "relogin@example.com", "paused@example.com",
		"pending@example.com", "both@example.com"
	admin, _, _ := deleteTestAdmin(t, listed, relogin, paused, pending, both)
	registry := newStartupFailureRegistry("", admin.requests)
	admin.workers.startupFailures = registry
	admin.workers.lastStartupSuccess.Store(time.Now().UnixNano())
	registry.record(listed, startupFailureFiles, "", "解析 storage state: unexpected EOF", 1, "", time.Now())
	if err := admin.pool.MarkAuthRequired(listed, "认证文件无法用于启动浏览器：解析 storage state: unexpected EOF"); err != nil {
		t.Fatal(err)
	}
	if err := admin.pool.MarkAuthRequired(relogin, "AI Studio 登录已失效"); err != nil {
		t.Fatal(err)
	}
	admin.workers.notePausedFailure(workerOf(t, admin.workers, paused), startupFailureAmbiguous, admin.pool.AuthGeneration(paused), 5, pageNotReadyError(), time.Now())
	registry.record(pending, startupFailureAuth, "https://accounts.google.com/x", "跳转", 5, "", time.Now())
	registry.record(both, startupFailureAuth, "https://accounts.google.com/x", "跳转", 5, "", time.Now())
	admin.workers.notePausedFailure(workerOf(t, admin.workers, both), startupFailureAmbiguous, admin.pool.AuthGeneration(both), 5, pageNotReadyError(), time.Now())

	failures, err := admin.StartupFailures(context.Background())
	if err != nil || len(failures) != 5 {
		t.Fatalf("列表 = %+v %v", failures, err)
	}
	byID := make(map[string]api.AdminStartupFailure)
	for _, failure := range failures {
		byID[failure.ID] = failure
	}
	if entry := byID[listed]; !entry.Confirmed || entry.Kind != "files" || entry.Stage != 1 || entry.Count != 1 || entry.LastAt == "" {
		t.Fatalf("持久表条目 = %+v", entry)
	}
	if entry := byID[relogin]; !entry.Confirmed || entry.Kind != "auth" || !strings.Contains(entry.Message, "登录已失效") {
		t.Fatalf("需要重新登录的账户 = %+v", entry)
	}
	if entry := byID[paused]; entry.Confirmed || entry.Kind != "ambiguous" || entry.Until == "" || entry.Count != 1 || entry.Stage != 5 {
		t.Fatalf("暂停中的账户 = %+v", entry)
	}
	if entry := byID[pending]; entry.Confirmed || entry.Kind != "auth" || entry.Count != 1 || !strings.Contains(entry.Reason, "下次启动再失败才确认") {
		t.Fatalf("账户仍可调度的持久记录应列为未确认，实际 %+v", entry)
	}
	if entry := byID[both]; entry.Confirmed || entry.Kind != "ambiguous" || entry.Until == "" {
		t.Fatalf("同时有未确认记录与暂停的账户应按暂停列一次，实际 %+v", entry)
	}
	status, err := admin.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if status.Startup.Dead != 2 || status.Startup.Paused != 2 || status.Startup.Limit != 2 || status.Startup.LastSuccess == "" {
		t.Fatalf("状态 startup = %+v", status.Startup)
	}

	var operation *adminOperationError
	if _, err := admin.ClearStartupFailures(context.Background(), api.StartupFailureClearInput{AccountIDs: []string{" "}}); !errors.As(err, &operation) || operation.status != http.StatusBadRequest {
		t.Fatalf("空的清除请求应返回 400，实际 %v", err)
	}
	cleared, err := admin.ClearStartupFailures(context.Background(), api.StartupFailureClearInput{AccountIDs: []string{"LISTED@example.com", paused}})
	if err != nil || cleared != 2 {
		t.Fatalf("清除 = %d %v，期望 2", cleared, err)
	}
	if until, _ := startupPauseOf(t, admin.workers, paused); recorded(registry, listed) || !until.IsZero() {
		t.Fatal("清除应同时清除持久表与内存暂停")
	}
	if status := testStatus(t, admin.pool, relogin); status.State != aistudio.AccountAuthRequired {
		t.Fatal("清除记录不改变账户状态")
	}
}
