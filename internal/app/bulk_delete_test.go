package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/api"
)

// deleteTestAdmin 创建批量删除测试用的管理端：带内存中的 403 登记表与新账户自动处理记录（现有账户记为基线）
func deleteTestAdmin(t *testing.T, accountIDs ...string) (*runtimeAdmin, *enableTestCatalog, []*aistudio.Account) {
	t.Helper()
	admin, catalog, accounts := enableTestAdmin(t, accountIDs...)
	admin.service.permissionDenied = newPermissionDeniedRegistry("", admin.requests)
	admin.onboard = newAccountOnboarder(admin, t.TempDir())
	catalog.cached.Store(0)
	return admin, catalog, accounts
}

// directoryExists 判断账户目录是否还在
func directoryExists(t *testing.T, directory string) bool {
	t.Helper()
	_, err := os.Stat(directory)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return err == nil
}

// onboardRecorded 判断新账户自动处理是否还有账户的记录
func onboardRecorded(onboarder *accountOnboarder, accountID string) bool {
	onboarder.mu.Lock()
	defer onboarder.mu.Unlock()
	_, exists := onboarder.records[accountID]
	return exists
}

// workerRegistered 判断 Worker 管理器是否还登记着账户
func workerRegistered(workers *accountWorkerManager, accountID string) bool {
	workers.mu.RLock()
	defer workers.mu.RUnlock()
	_, exists := workers.accounts[accountID]
	return exists
}

// TestDeleteAccountsBulk 批量删除：去重、去空白；删除的账户移出号池、认证目录被删除，Worker、403 登记、停用原因与新账户自动处理记录都被清除；
// 号池里没有的账户计入 missing 并清除 403 登记；整批只合并一次模型目录、只推送一次，单个账户仍写“账户已删除”，另写一条汇总日志
func TestDeleteAccountsBulk(t *testing.T) {
	notes := isolateDisableNotes(t)
	admin, catalog, accounts := deleteTestAdmin(t, "a@example.com", "b@example.com", "c@example.com", "keep@example.com")
	registry := admin.service.permissionDenied
	now := time.Now()
	for _, accountID := range []string{"a@example.com", "b@example.com", "ghost@example.com", "keep@example.com"} {
		registry.record(accountID, "pro", aistudio.ChannelPlayground, "denied", "", now)
	}
	notes.set("a@example.com", "通过管理页或管理接口停用", false)
	notes.set("b@example.com", "新账户自动处理验证未通过", true)
	if !onboardRecorded(admin.onboard, "a@example.com") {
		t.Fatal("现有账户应记为基线")
	}
	manager := &runtimeManager{current: &runtimeGeneration{admin: admin}}

	result, err := manager.DeleteAccounts(context.Background(), api.BulkDeleteInput{AccountIDs: []string{
		"a@example.com", "a@example.com", " b@example.com ", "", "ghost@example.com", "c@example.com",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if result.Deleted != 3 || result.Missing != 1 || len(result.Failed) != 0 || result.Remaining != 0 {
		t.Fatalf("结果 = %+v，期望删除 3、不存在 1", result)
	}
	if catalog.cached.Load() != 1 {
		t.Fatalf("整批应只合并一次模型目录，实际 %d 次", catalog.cached.Load())
	}
	for index, accountID := range []string{"a@example.com", "b@example.com", "c@example.com"} {
		if _, ok := admin.pool.StatusOf(accountID); ok {
			t.Fatalf("%s 应移出号池", accountID)
		}
		if directoryExists(t, accounts[index].Directory) {
			t.Fatalf("%s 的认证目录应被删除", accountID)
		}
		if workerRegistered(admin.workers, accountID) {
			t.Fatalf("%s 的 Worker 登记应被移除", accountID)
		}
		if onboardRecorded(admin.onboard, accountID) {
			t.Fatalf("%s 的新账户自动处理记录应被移除", accountID)
		}
		if _, ok := permissionDeniedEntryOf(registry, accountID); ok {
			t.Fatalf("%s 的 403 登记应被清除", accountID)
		}
		if logCount(admin.requests, "账户已删除 | 账户="+accountID) != 1 {
			t.Fatalf("%s 应写一条“账户已删除”日志", accountID)
		}
	}
	if _, ok := permissionDeniedEntryOf(registry, "ghost@example.com"); ok {
		t.Fatal("号池里没有的账户也应清除 403 登记")
	}
	if notes.manual("a@example.com") || notes.automatic("b@example.com") {
		t.Fatal("删除后应清除停用原因")
	}
	if _, ok := admin.pool.StatusOf("keep@example.com"); !ok || !directoryExists(t, accounts[3].Directory) {
		t.Fatal("没有指定的账户不应删除")
	}
	if _, ok := permissionDeniedEntryOf(registry, "keep@example.com"); !ok || !onboardRecorded(admin.onboard, "keep@example.com") {
		t.Fatal("没有指定的账户应保留 403 登记与自动处理记录")
	}
	if logCount(admin.requests, "批量删除账户 | 删除=3 | 不存在=1 | 失败=0 | 剩余=0") != 1 {
		t.Fatal("应写一条批量删除汇总日志")
	}

	// 全部不存在：只清除登记，不合并模型目录
	result, err = admin.DeleteAccounts(context.Background(), api.BulkDeleteInput{AccountIDs: []string{"a@example.com"}})
	if err != nil || result.Deleted != 0 || result.Missing != 1 {
		t.Fatalf("重复删除 = %+v %v，期望按不存在处理", result, err)
	}
	if catalog.cached.Load() != 1 {
		t.Fatalf("没有删除账户时不应合并模型目录，实际共 %d 次", catalog.cached.Load())
	}
}

// TestDeleteAccountsEmpty 没有指定账户（全是空白）时返回 400
func TestDeleteAccountsEmpty(t *testing.T) {
	isolateDisableNotes(t)
	admin, _, _ := deleteTestAdmin(t, "a@example.com")
	for _, input := range []api.BulkDeleteInput{{}, {AccountIDs: []string{" ", ""}}} {
		_, err := admin.DeleteAccounts(context.Background(), input)
		var operation *adminOperationError
		if !errors.As(err, &operation) || operation.status != http.StatusBadRequest {
			t.Fatalf("输入 %+v：err = %v，期望 400", input, err)
		}
	}
	if _, ok := admin.pool.StatusOf("a@example.com"); !ok {
		t.Fatal("空请求不应删除账户")
	}
}

// TestDeleteAccountsMissingExactID 号池按原样精确查找账户，只差大小写的 ID 按不存在处理；清除它的 403 登记时也按原样精确匹配，
// 不能清掉号池里那个账户的登记（否则账户还在、仍然返回 403，却从 403 无权限列表里消失）
func TestDeleteAccountsMissingExactID(t *testing.T) {
	isolateDisableNotes(t)
	admin, catalog, accounts := deleteTestAdmin(t, "alice@example.com")
	registry := admin.service.permissionDenied
	now := time.Now()
	registry.record("alice@example.com", "pro", aistudio.ChannelPlayground, "denied", "", now)
	registry.record("Ghost@Example.com", "pro", aistudio.ChannelPlayground, "denied", "", now)

	result, err := admin.DeleteAccounts(context.Background(), api.BulkDeleteInput{AccountIDs: []string{
		"ALICE@example.com", "ghost@example.com",
	}})
	if err != nil || result.Deleted != 0 || result.Missing != 2 || len(result.Failed) != 0 {
		t.Fatalf("结果 = %+v %v，期望两个都按不存在处理", result, err)
	}
	if _, ok := admin.pool.StatusOf("alice@example.com"); !ok || !directoryExists(t, accounts[0].Directory) {
		t.Fatal("只差大小写的 ID 不应删除号池里的账户")
	}
	if _, ok := permissionDeniedEntryOf(registry, "alice@example.com"); !ok {
		t.Fatal("只差大小写的 ID 不应清除号池里账户的 403 登记")
	}
	if _, ok := permissionDeniedEntryOf(registry, "Ghost@Example.com"); !ok {
		t.Fatal("清除不存在账户的登记应按原样精确匹配")
	}
	if catalog.cached.Load() != 0 {
		t.Fatalf("没有删除账户时不应合并模型目录，实际 %d 次", catalog.cached.Load())
	}
}

// TestDeleteAccountsWaitsForBusyAccount 正在处理请求的账户：登记后不再接新请求，进行中的请求照常结束，在等待上限内空闲后删除；
// 其他空闲的账户不用等它
func TestDeleteAccountsWaitsForBusyAccount(t *testing.T) {
	isolateDisableNotes(t)
	previous := bulkDeleteBusyWait
	bulkDeleteBusyWait = 10 * time.Second
	t.Cleanup(func() { bulkDeleteBusyWait = previous })
	admin, catalog, accounts := deleteTestAdmin(t, "busy@example.com", "idle@example.com")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	inUse, err := admin.pool.AcquireFor(ctx, aistudio.AccountSelection{ModelID: testRuntimeModel, AccountID: "busy@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	type outcome struct {
		result api.BulkDeleteResult
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := admin.DeleteAccounts(ctx, api.BulkDeleteInput{AccountIDs: []string{"busy@example.com", "idle@example.com"}})
		done <- outcome{result, err}
	}()
	waitFor(t, "空闲账户先删除", func() bool {
		_, ok := admin.pool.StatusOf("idle@example.com")
		return !ok
	})
	// 登记期间新请求不能取得正在删除的账户
	probeCtx, probeCancel := context.WithTimeout(ctx, 150*time.Millisecond)
	defer probeCancel()
	if lease, err := admin.pool.AcquireFor(probeCtx, aistudio.AccountSelection{ModelID: testRuntimeModel}); err == nil {
		_ = lease.Release()
		t.Fatalf("删除等待期间新请求取得了账户 %s", lease.Account().ID)
	}
	select {
	case got := <-done:
		t.Fatalf("进行中的请求结束前不应返回：%+v %v", got.result, got.err)
	default:
	}
	if err := inUse.Release(); err != nil {
		t.Fatal(err)
	}
	var got outcome
	select {
	case got = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("请求结束后应删除账户并返回")
	}
	if got.err != nil || got.result.Deleted != 2 || len(got.result.Failed) != 0 {
		t.Fatalf("结果 = %+v %v，期望删除 2", got.result, got.err)
	}
	if _, ok := admin.pool.StatusOf("busy@example.com"); ok || directoryExists(t, accounts[0].Directory) {
		t.Fatal("空闲后应删除正在使用的账户及其目录")
	}
	if catalog.cached.Load() != 1 {
		t.Fatalf("整批应只合并一次模型目录，实际 %d 次", catalog.cached.Load())
	}
}

// TestDeleteAccountsBusyTimeout 等待上限内没有空闲的账户报告 account_busy：撤销为它登记的独占等待，账户留在号池、恢复调度，
// 认证目录与 403 登记都在；之后空闲时再次提交即可删除
func TestDeleteAccountsBusyTimeout(t *testing.T) {
	isolateDisableNotes(t)
	previous := bulkDeleteBusyWait
	bulkDeleteBusyWait = 150 * time.Millisecond
	t.Cleanup(func() { bulkDeleteBusyWait = previous })
	admin, catalog, accounts := deleteTestAdmin(t, "busy@example.com")
	admin.service.permissionDenied.record("busy@example.com", "pro", aistudio.ChannelPlayground, "denied", "", time.Now())
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	inUse, err := admin.pool.AcquireFor(ctx, aistudio.AccountSelection{ModelID: testRuntimeModel, AccountID: "busy@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	startedAt := time.Now()
	result, err := admin.DeleteAccounts(ctx, api.BulkDeleteInput{AccountIDs: []string{"busy@example.com"}})
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(startedAt); elapsed > 5*time.Second {
		t.Fatalf("等待上限后应放弃，实际耗时 %s", elapsed)
	}
	if result.Deleted != 0 || len(result.Failed) != 1 || result.Failed[0].ID != "busy@example.com" ||
		result.Failed[0].Code != "account_busy" {
		t.Fatalf("结果 = %+v，期望 account_busy", result)
	}
	if catalog.cached.Load() != 0 {
		t.Fatalf("没有删除账户时不应合并模型目录，实际 %d 次", catalog.cached.Load())
	}
	if !directoryExists(t, accounts[0].Directory) {
		t.Fatal("没删掉的账户应保留认证目录")
	}
	if _, ok := permissionDeniedEntryOf(admin.service.permissionDenied, "busy@example.com"); !ok {
		t.Fatal("没删掉的账户应保留 403 登记")
	}
	if logCount(admin.requests, "批量删除账户 | 删除=0 | 不存在=0 | 失败=1 | 剩余=0") != 1 {
		t.Fatal("应写一条批量删除汇总日志")
	}
	if err := inUse.Release(); err != nil {
		t.Fatal(err)
	}
	if busy, _ := admin.pool.Activity("busy@example.com"); busy || admin.pool.Draining("busy@example.com") {
		t.Fatal("放弃删除后不应残留独占等待或排空")
	}
	lease, err := admin.pool.AcquireFor(ctx, aistudio.AccountSelection{ModelID: testRuntimeModel})
	if err != nil {
		t.Fatalf("放弃删除后账户应恢复调度：%v", err)
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}

	result, err = admin.DeleteAccounts(ctx, api.BulkDeleteInput{AccountIDs: []string{"busy@example.com"}})
	if err != nil || result.Deleted != 1 || len(result.Failed) != 0 {
		t.Fatalf("空闲后再次删除 = %+v %v，期望删除 1", result, err)
	}
}

// TestDeleteAccountsLimit 单次上限只计号池里还有的账户，其余计入 remaining；原样再次提交同一列表时已删除的按不存在处理、继续删除剩下的
func TestDeleteAccountsLimit(t *testing.T) {
	isolateDisableNotes(t)
	previous := bulkDeleteMaxPerCall
	bulkDeleteMaxPerCall = 2
	t.Cleanup(func() { bulkDeleteMaxPerCall = previous })
	admin, _, _ := deleteTestAdmin(t, "a@example.com", "b@example.com", "c@example.com")
	input := api.BulkDeleteInput{AccountIDs: []string{"a@example.com", "ghost@example.com", "b@example.com", "c@example.com"}}
	result, err := admin.DeleteAccounts(context.Background(), input)
	if err != nil || result.Deleted != 2 || result.Missing != 1 || result.Remaining != 1 {
		t.Fatalf("第一次结果 = %+v %v，期望删除 2、不存在 1、剩余 1", result, err)
	}
	if _, ok := admin.pool.StatusOf("c@example.com"); !ok {
		t.Fatal("超过上限的账户本次不应删除")
	}
	if logCount(admin.requests, "批量删除账户 | 删除=2 | 不存在=1 | 失败=0 | 剩余=1") != 1 {
		t.Fatal("汇总日志应记下剩余数")
	}
	result, err = admin.DeleteAccounts(context.Background(), input)
	if err != nil || result.Deleted != 1 || result.Missing != 3 || result.Remaining != 0 {
		t.Fatalf("第二次结果 = %+v %v，期望删除剩下的 1 个", result, err)
	}
}

// TestDeleteAccountUnchanged 单个删除不变：正在使用的账户立即返回 account_busy（不等待），空闲后删除并合并一次模型目录
func TestDeleteAccountUnchanged(t *testing.T) {
	isolateDisableNotes(t)
	admin, catalog, accounts := deleteTestAdmin(t, "a@example.com")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	inUse, err := admin.pool.AcquireFor(ctx, aistudio.AccountSelection{ModelID: testRuntimeModel, AccountID: "a@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	var operation *adminOperationError
	if err := admin.DeleteAccount(ctx, "a@example.com"); !errors.As(err, &operation) || operation.code != "account_busy" {
		t.Fatalf("正在使用的账户 err = %v，期望 account_busy", err)
	}
	if err := inUse.Release(); err != nil {
		t.Fatal(err)
	}
	if busy, _ := admin.pool.Activity("a@example.com"); busy {
		t.Fatal("单个删除失败后不应残留独占")
	}
	if err := admin.DeleteAccount(ctx, "a@example.com"); err != nil {
		t.Fatal(err)
	}
	if directoryExists(t, accounts[0].Directory) || onboardRecorded(admin.onboard, "a@example.com") {
		t.Fatal("单个删除应删除认证目录与自动处理记录")
	}
	if logCount(admin.requests, "账户已删除 | 账户=a@example.com") != 1 || logCount(admin.requests, "批量删除账户") != 0 {
		t.Fatal("单个删除只写“账户已删除”")
	}
	// 两次删除各合并一次（失败的那次同样在返回前刷新）
	if catalog.cached.Load() != 2 {
		t.Fatalf("单个删除每次合并一次模型目录，实际 %d 次", catalog.cached.Load())
	}
	if err := admin.DeleteAccount(ctx, "a@example.com"); !errors.As(err, &operation) || operation.code != "account_not_found" {
		t.Fatalf("已删除的账户 err = %v，期望 account_not_found", err)
	}
}

// TestDeleteAccountsConcurrent 批量删除与单个删除、账户目录扫描、新账户自动处理记录的写入同时进行（-race）：
// 每个账户恰好删除一次，后到的一方按不存在处理，没有失败
func TestDeleteAccountsConcurrent(t *testing.T) {
	isolateDisableNotes(t)
	ids := make([]string, 0, 24)
	for index := range 24 {
		ids = append(ids, fmt.Sprintf("u%02d@example.com", index))
	}
	admin, _, accounts := deleteTestAdmin(t, ids...)
	now := time.Now()
	for _, accountID := range ids {
		admin.service.permissionDenied.record(accountID, "pro", aistudio.ChannelPlayground, "denied", "", now)
	}
	watcher := &accountWatcher{
		admin: admin, failures: map[string]string{}, missing: map[string]int{},
		authFiles: map[string]authFileStat{}, loginPending: map[string]struct{}{},
	}
	stop := make(chan struct{})
	var background sync.WaitGroup
	background.Add(2)
	go func() {
		defer background.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			watcher.scan(admin.lifecycle)
		}
	}()
	go func() {
		defer background.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			admin.onboard.enqueue([]string{"other@example.com"})
			admin.onboard.forget("other@example.com")
		}
	}()

	// 单个删除列表后部的账户（批量删除按顺序登记，后部的最晚登记），与批量删除同时开始
	var singles atomic.Int32
	var singleDeletes sync.WaitGroup
	start := make(chan struct{})
	for _, accountID := range ids[len(ids)/2:] {
		singleDeletes.Add(1)
		go func() {
			defer singleDeletes.Done()
			<-start
			if err := admin.DeleteAccount(context.Background(), accountID); err == nil {
				singles.Add(1)
			}
		}()
	}
	close(start)
	result, err := admin.DeleteAccounts(context.Background(), api.BulkDeleteInput{AccountIDs: ids})
	singleDeletes.Wait()
	close(stop)
	background.Wait()
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Failed) != 0 {
		t.Fatalf("失败 = %+v，期望没有失败", result.Failed)
	}
	if result.Deleted+int(singles.Load()) != len(ids) || result.Missing != int(singles.Load()) {
		t.Fatalf("批量删除 %d、不存在 %d，单个删除 %d，期望每个账户恰好删除一次", result.Deleted, result.Missing, singles.Load())
	}
	for index, accountID := range ids {
		if _, ok := admin.pool.StatusOf(accountID); ok || directoryExists(t, accounts[index].Directory) {
			t.Fatalf("%s 应已删除", accountID)
		}
		if logCount(admin.requests, "账户已删除 | 账户="+accountID) != 1 {
			t.Fatalf("%s 应恰好删除一次", accountID)
		}
	}
	if entries := admin.service.permissionDenied.list(); len(entries) != 0 {
		t.Fatalf("403 登记应全部清除，剩余 %d", len(entries))
	}
}
