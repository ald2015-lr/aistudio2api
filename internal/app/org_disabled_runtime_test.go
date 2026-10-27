package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/config"
)

// TestOrgDisabledRuntimeWiring 经 newRuntime 装配的生成服务（生产装配，不经 withOrgDisabled 手工接线）：
//   - 启动时按账户目录下的 .org-disabled.json 对照：认证文件没变的账户恢复标记、不参与调度；服务停止期间替换了认证文件的清除；
//     目录已删除的账户的记录清除；
//   - Worker 启动确认（confirmOrgDisabled）与 aistudio 内部识别（目录同步等经 AccountPool.ReportOrgDisabled）都进入组织停用状态：
//     登记来源、写一行日志、模型目录改排长间隔复查、管理页列出；
//   - 关闭时立即写出合并中的登记（不等几秒后的合并写入）。
//
// 原先这几处接线去掉任何一处，其余测试仍然全部通过（它们都经 withOrgDisabled 自己建登记表、挂回调）
func TestOrgDisabledRuntimeWiring(t *testing.T) {
	isolateDisableNotes(t)
	root := filepath.Join(t.TempDir(), "auth")
	const kept, replaced, gone = "kept-org@example.com", "replaced-org@example.com", "gone-org@example.com"
	const startup, catalog = "startup-org@example.com", "catalog-org@example.com"
	replacedDirectory := writeEnableTestAccount(t, root, replaced, true)
	goneDirectory := writeEnableTestAccount(t, root, gone, true)
	for _, accountID := range []string{kept, startup, catalog} {
		writeEnableTestAccount(t, root, accountID, true)
	}
	{
		// 停止之前的一代：三个账户进入过组织停用状态，登记表写入账户目录
		accounts, failures := aistudio.NewAccountStore(root).LoadTolerant()
		if len(failures) > 0 {
			t.Fatal(failures)
		}
		previous := aistudio.NewAccountPool(accounts, 1)
		registry := newOrgDisabledRegistry(root, nil)
		for _, accountID := range []string{kept, replaced, gone} {
			identity, _ := previous.LoginIdentity(accountID)
			registry.record(accountID, aistudio.OrgDisabledFromStartup, aistudio.OrgDisabledReason, identity, time.Now())
		}
		registry.flush()
		previous.WaitBackground()
	}
	// 服务停止期间：重新获取 replaced 的认证文件覆盖上传，删除 gone 的账户目录
	if err := aistudio.WriteStorageState(aistudio.StorageStatePath(replacedDirectory), authTestState(t, "NEW", false)); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(goneDirectory); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	requests := newRequestRegistry(ctx)
	cfg := config.Default()
	cfg.AuthStates = root
	cfg.WAABackend = config.WAABackendGo
	lifecycle, stop := context.WithCancel(ctx)
	useTempCatalogCacheDir(t)
	_, admin, closeRuntime, err := newRuntime(ctx, lifecycle, cfg, requests, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.pool.WaitBackground)
	closed := false
	defer func() {
		if closed {
			return
		}
		stop()
		if err := closeRuntime(); err != nil {
			t.Error(err)
		}
	}()
	registry := admin.service.orgDisabled

	// 启动时的对照
	if !admin.pool.OrgDisabled(kept) || testStatus(t, admin.pool, kept).State != aistudio.AccountOrgDisabled {
		t.Fatal("认证文件没变的账户重启后应恢复组织停用标记")
	}
	if _, listed := registry.entryOf(kept); !listed {
		t.Fatal("认证文件没变的账户应仍在登记表里")
	}
	if admin.pool.OrgDisabled(replaced) {
		t.Fatal("服务停止期间替换了认证文件的账户不应恢复标记")
	}
	for _, accountID := range []string{replaced, gone} {
		if _, listed := registry.entryOf(accountID); listed {
			t.Fatalf("%s 的记录应在启动时清除", accountID)
		}
	}
	if !hasLogContaining(requests, "已清除组织停用标记，恢复调度 | 来源=启动时发现认证文件已更新") ||
		!hasLogContaining(requests, "恢复组织停用账户的标记，不参与调度 | 账户=1") ||
		!hasLogContaining(requests, "清除已不在账户池中的账户的组织停用记录 | 账户=1") {
		t.Fatalf("应记录启动时的对照，日志 = %v", serviceLogMessages(requests))
	}

	// Worker 启动确认与 aistudio 内部识别（目录同步）
	admin.workers.confirmOrgDisabled(startup, serviceNotAllowedStartupError())
	admin.pool.ReportOrgDisabled(catalog, aistudio.OrgDisabledFromCatalog, orgDisabledRPCError("GetAiStudioBenefitTier"))
	for accountID, source := range map[string]string{startup: aistudio.OrgDisabledFromStartup, catalog: aistudio.OrgDisabledFromCatalog} {
		entry, listed := registry.entryOf(accountID)
		if !listed || entry.Source != source || !admin.pool.OrgDisabled(accountID) {
			t.Fatalf("%s 应经生成服务进入组织停用状态（来源=%s）：登记=%+v listed=%t", accountID, source, entry, listed)
		}
		if orgDisabledLogs(requests, accountID) != 1 {
			t.Fatalf("%s 应写一行“账号所属组织已停用 AI Studio，停止调度”，日志 = %v", accountID, serviceLogMessages(requests))
		}
		admin.service.catalogSync.mu.Lock()
		var task catalogTask
		if current := admin.service.catalogSync.tasks[accountID]; current != nil {
			task = *current
		}
		admin.service.catalogSync.mu.Unlock()
		if task.accountID != accountID || task.priority != catalogPriorityOrgRecheck {
			t.Fatalf("%s 的模型目录应改排长间隔复查: %+v", accountID, task)
		}
	}
	listedIDs := map[string]bool{}
	entries, err := admin.OrgDisabled(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		listedIDs[entry.ID] = true
	}
	if len(entries) != 3 || !listedIDs[kept] || !listedIDs[startup] || !listedIDs[catalog] {
		t.Fatalf("管理页应列出三个组织停用账户: %+v", entries)
	}

	// 关闭时立即写出（新登记的两个账户还在几秒的合并窗口里）
	stop()
	closed = true
	if err := closeRuntime(); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(root, orgDisabledFileName))
	if err != nil {
		t.Fatal(err)
	}
	var saved orgDisabledFile
	if err := json.Unmarshal(content, &saved); err != nil {
		t.Fatal(err)
	}
	for _, accountID := range []string{kept, startup, catalog} {
		if saved.Accounts[accountID] == nil {
			t.Fatalf("关闭时应写出 %s 的登记: %s", accountID, content)
		}
	}
	for _, accountID := range []string{replaced, gone} {
		if saved.Accounts[accountID] != nil {
			t.Fatalf("已清除的 %s 不应写回: %s", accountID, content)
		}
	}
}
