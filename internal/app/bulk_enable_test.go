package app

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/api"
)

// TestEnableAccountsBulk 批量启用：去重、跳过已启用的、不存在的与正忙的（等待上限内拿不到账户）列为失败；
// 启用的账户写回 account.json、清除停用原因；整批只合并一次全部账户的模型目录，没有逐个账户推送；
// 没有模型目录的账户在后台同步，整批同步完再合并一次
func TestEnableAccountsBulk(t *testing.T) {
	notes := isolateDisableNotes(t)
	previousWait := bulkEnableAcquireWait
	bulkEnableAcquireWait = 100 * time.Millisecond
	t.Cleanup(func() { bulkEnableAcquireWait = previousWait })
	admin, catalog, accounts := enableTestAdmin(t, "a@example.com", "b@example.com", "c@example.com", "d@example.com")
	for _, id := range []string{"a@example.com", "b@example.com", "c@example.com"} {
		disableTestAccount(t, admin.pool, id)
	}
	notes.set("a@example.com", "通过管理页或管理接口停用", false)
	notes.set("b@example.com", "新账户自动处理验证未通过", true)
	busy, err := admin.pool.AcquireAccount(context.Background(), "c@example.com")
	if err != nil {
		t.Fatal(err)
	}
	// 后台目录同步先停在 gate，便于确认请求返回时只合并了一次
	catalog.gate = make(chan struct{})
	catalog.cached.Store(0)
	manager := &runtimeManager{current: &runtimeGeneration{admin: admin}}

	startedAt := time.Now()
	result, err := manager.EnableAccounts(context.Background(), api.BulkEnableInput{AccountIDs: []string{
		"a@example.com", "a@example.com", " b@example.com ", "c@example.com", "d@example.com", "ghost@example.com",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(startedAt); elapsed > 5*time.Second {
		t.Fatalf("正忙的账户应在等待上限后放弃，实际耗时 %s", elapsed)
	}
	if result.Enabled != 2 || result.Skipped != 1 || result.Remaining != 0 || len(result.Failed) != 2 {
		t.Fatalf("结果 = %+v，期望启用 2、跳过 1、失败 2", result)
	}
	if result.Failed[0].ID != "c@example.com" || result.Failed[0].Code != "account_busy" ||
		result.Failed[1].ID != "ghost@example.com" || result.Failed[1].Code != "account_not_found" {
		t.Fatalf("失败明细 = %+v", result.Failed)
	}
	if catalog.cached.Load() != 1 {
		t.Fatalf("整批应只合并一次模型目录，实际 %d 次", catalog.cached.Load())
	}
	if logCount(admin.requests, "账户配置已更新") != 0 || logCount(admin.requests, "批量启用账户 | 启用=2") != 1 {
		t.Fatal("批量启用不应逐个账户记录更新，应只写一条汇总日志")
	}
	for index, id := range []string{"a@example.com", "b@example.com"} {
		if status := testStatus(t, admin.pool, id); !status.Enabled || status.State != aistudio.AccountReady {
			t.Fatalf("%s：enabled=%t state=%s，期望启用且就绪", id, status.Enabled, status.State)
		}
		if !diskEnabled(t, accounts[index].Directory) {
			t.Fatalf("%s 应写回 account.json", id)
		}
	}
	if notes.manual("a@example.com") || notes.automatic("b@example.com") {
		t.Fatal("启用后应清除停用原因")
	}
	if testStatus(t, admin.pool, "c@example.com").Enabled {
		t.Fatal("正忙的账户不应被启用")
	}

	close(catalog.gate)
	waitFor(t, "后台模型目录同步", func() bool {
		return logCount(admin.requests, "启用后的模型目录同步完成 | 账户=2 | 非空=2") == 1
	})
	refreshed := catalog.refreshedIDs()
	slices.Sort(refreshed)
	if !slices.Equal(refreshed, []string{"a@example.com", "b@example.com"}) {
		t.Fatalf("同步模型目录的账户 = %v，期望只有新启用且没有目录的账户", refreshed)
	}
	if catalog.cached.Load() != 2 {
		t.Fatalf("后台同步完应只再合并一次模型目录，实际共 %d 次", catalog.cached.Load())
	}

	if err := busy.Release(); err != nil {
		t.Fatal(err)
	}
	result, err = admin.EnableAccounts(context.Background(), api.BulkEnableInput{AllDisabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if result.Enabled != 1 || len(result.Failed) != 0 || !testStatus(t, admin.pool, "c@example.com").Enabled {
		t.Fatalf("all_disabled 应启用剩下的停用账户：%+v", result)
	}
	waitFor(t, "第二批后台模型目录同步", func() bool {
		return logCount(admin.requests, "启用后的模型目录同步完成 | 账户=1") == 1
	})
}

// TestEnableAccountsLimit 单次请求超过上限的停用账户计入 remaining，下一次请求继续；没有指定账户时返回 400
func TestEnableAccountsLimit(t *testing.T) {
	isolateDisableNotes(t)
	previous := bulkEnableMaxPerCall
	bulkEnableMaxPerCall = 2
	t.Cleanup(func() { bulkEnableMaxPerCall = previous })
	admin, _, accounts := enableTestAdmin(t, "a@example.com", "b@example.com", "c@example.com")
	for _, account := range accounts {
		disableTestAccount(t, admin.pool, account.ID)
	}
	result, err := admin.EnableAccounts(context.Background(), api.BulkEnableInput{AllDisabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if result.Enabled != 2 || result.Remaining != 1 {
		t.Fatalf("结果 = %+v，期望启用 2、剩余 1", result)
	}
	result, err = admin.EnableAccounts(context.Background(), api.BulkEnableInput{AllDisabled: true})
	if err != nil || result.Enabled != 1 || result.Remaining != 0 {
		t.Fatalf("第二次结果 = %+v %v，期望启用剩下的 1 个", result, err)
	}
	waitFor(t, "后台模型目录同步", func() bool {
		return logCount(admin.requests, "启用后的模型目录同步完成") == 2
	})
	if _, err := admin.EnableAccounts(context.Background(), api.BulkEnableInput{}); err == nil {
		t.Fatal("没有指定账户时应返回错误")
	}
}

// TestUpdateAccountEnableSyncsCatalog 管理页单个启用停用账户后，没有模型目录时在后台同步一次，启用后马上可以预热与调度
func TestUpdateAccountEnableSyncsCatalog(t *testing.T) {
	isolateDisableNotes(t)
	admin, catalog, accounts := enableTestAdmin(t, "a@example.com")
	disableTestAccount(t, admin.pool, accounts[0].ID)
	if len(testStatus(t, admin.pool, accounts[0].ID).Models) != 0 {
		t.Fatal("停用账户应没有模型目录")
	}
	if _, err := admin.UpdateAccount(context.Background(), accounts[0].ID, api.AccountInput{
		Label: accounts[0].ID, Enabled: true, Locale: aistudio.DefaultAccountLocale(), Timezone: aistudio.DefaultAccountTimezone(),
	}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "启用后的模型目录同步", func() bool {
		return len(testStatus(t, admin.pool, accounts[0].ID).Models) > 0
	})
	if refreshed := catalog.refreshedIDs(); !slices.Equal(refreshed, []string{accounts[0].ID}) {
		t.Fatalf("同步模型目录的账户 = %v", refreshed)
	}
	waitFor(t, "后台同步结束", func() bool {
		return logCount(admin.requests, "启用后的模型目录同步完成") == 1
	})
}
