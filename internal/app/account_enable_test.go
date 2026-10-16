package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/config"
)

// isolateDisableNotes 换上测试专用的停用原因记录（没有文件路径，不写盘），测试结束后恢复
func isolateDisableNotes(t *testing.T) *disableNotes {
	t.Helper()
	previous := accountDisableNotes
	notes := &disableNotes{notes: make(map[string]disableNote)}
	accountDisableNotes = notes
	t.Cleanup(func() { accountDisableNotes = previous })
	return notes
}

// writeEnableTestAccount 在 root 下写入账户目录：account.json 为给定的启用状态，认证文件带登录签名 Cookie
func writeEnableTestAccount(t *testing.T, root string, id string, enabled bool) string {
	t.Helper()
	directory := filepath.Join(root, id)
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	accountConfig := aistudio.DefaultAccountConfig(id)
	accountConfig.Enabled = enabled
	content, err := json.MarshalIndent(accountConfig, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "account.json"), content, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := aistudio.WriteStorageState(aistudio.StorageStatePath(directory), authTestState(t, "V", false)); err != nil {
		t.Fatal(err)
	}
	return directory
}

// diskEnabled 读取账户目录中 account.json 的 enabled 字段
func diskEnabled(t *testing.T, directory string) bool {
	t.Helper()
	content, err := os.ReadFile(filepath.Join(directory, "account.json"))
	if err != nil {
		t.Fatal(err)
	}
	var value struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.Unmarshal(content, &value); err != nil {
		t.Fatal(err)
	}
	return value.Enabled
}

// logCount 统计包含 text 的管理日志条数
func logCount(requests *requestRegistry, text string) int {
	requests.mu.Lock()
	defer requests.mu.Unlock()
	count := 0
	for _, entry := range requests.logs {
		if strings.Contains(entry.Message, text) {
			count++
		}
	}
	return count
}

// waitFor 轮询直到 condition 成立，超时则失败
func waitFor(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatalf("等待超时：%s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// enableTestCatalog 为测试用模型目录：同步时把一个模型写入账户目录；gate 非空时同步要等 gate 关闭；CachedModels 计数
type enableTestCatalog struct {
	pool      *aistudio.AccountPool
	gate      chan struct{}
	mu        sync.Mutex
	refreshed []string
	cached    atomic.Int32
}

func (catalog *enableTestCatalog) RefreshAccountModels(ctx context.Context, accountID string) ([]aistudio.Model, error) {
	if catalog.gate != nil {
		select {
		case <-catalog.gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	catalog.mu.Lock()
	catalog.refreshed = append(catalog.refreshed, accountID)
	catalog.mu.Unlock()
	models := []aistudio.Model{{
		ID: testRuntimeModel, Methods: []string{"generateContent", "countTokens"}, Capabilities: map[string]bool{"chat_model": true},
	}}
	if err := catalog.pool.SetCatalog(accountID, aistudio.BenefitTierFree, models); err != nil {
		return nil, err
	}
	return models, nil
}

func (catalog *enableTestCatalog) CachedModels() []aistudio.Model {
	catalog.cached.Add(1)
	return nil
}

func (catalog *enableTestCatalog) refreshedIDs() []string {
	catalog.mu.Lock()
	defer catalog.mu.Unlock()
	return append([]string(nil), catalog.refreshed...)
}

// enableTestAdmin 创建带账户池、Worker、固定出口与测试模型目录的管理端；账户初始为启用且有模型目录
func enableTestAdmin(t *testing.T, accountIDs ...string) (*runtimeAdmin, *enableTestCatalog, []*aistudio.Account) {
	t.Helper()
	pool, accounts := testAccountPool(t, accountIDs...)
	ctx, cancel := context.WithCancel(context.Background())
	// 后台目录同步使用生命周期 context，先取消再等账户池后台写入、删除临时目录（清理按注册的逆序执行）
	t.Cleanup(cancel)
	requests := newRequestRegistry(ctx)
	workers := newAccountWorkerManager(pool, accounts, requests, "", "", time.Minute, 1, 1, 1, false)
	t.Cleanup(func() { _ = workers.Close() })
	headers, err := newAccountHeaderProvider(accounts, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(headers.Close)
	catalog := &enableTestCatalog{pool: pool}
	service := &trackedService{
		lifecycle: ctx, catalog: catalog, pool: pool, requests: requests, workers: workers,
		modelRetries: make(map[string]struct{}), forbidden: newForbiddenTracker(), quota: newQuotaSharing("", requests),
	}
	service.timeout.Store(int64(time.Minute))
	admin := &runtimeAdmin{
		lifecycle: ctx, pool: pool, service: service, requests: requests, workers: workers, headers: headers,
		store: aistudio.NewAccountStore(filepath.Dir(accounts[0].Directory)),
	}
	return admin, catalog, accounts
}

// disableTestAccount 把账户改为停用并清空它的模型目录（与重启后停用账户没有目录一致）
func disableTestAccount(t *testing.T, pool *aistudio.AccountPool, accountID string) {
	t.Helper()
	lease, err := pool.AcquireAccount(context.Background(), accountID)
	if err != nil {
		t.Fatal(err)
	}
	accountConfig := lease.Account().Config
	accountConfig.Enabled = false
	if err := lease.SaveConfig(accountConfig); err != nil {
		t.Fatal(err)
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
	if err := pool.SetCatalog(accountID, aistudio.BenefitTierFree, nil); err != nil {
		t.Fatal(err)
	}
}

// testStatus 返回账户状态，账户不存在时失败
func testStatus(t *testing.T, pool *aistudio.AccountPool, accountID string) aistudio.AccountStatus {
	t.Helper()
	status, ok := pool.StatusOf(accountID)
	if !ok {
		t.Fatalf("账户 %s 不存在", accountID)
	}
	return status
}

// TestEnableUnmarkedAccounts 载入后、加入账户池前：没有手动停用记录的停用账户改为启用并写回 account.json，
// 状态按登录 Cookie 推导为就绪，自动停用记录被清除；手动停用的保持停用；只写一条汇总日志
func TestEnableUnmarkedAccounts(t *testing.T) {
	notes := isolateDisableNotes(t)
	root := t.TempDir()
	plain := writeEnableTestAccount(t, root, "plain@example.com", false)
	manual := writeEnableTestAccount(t, root, "manual@example.com", false)
	auto := writeEnableTestAccount(t, root, "auto@example.com", false)
	writeEnableTestAccount(t, root, "on@example.com", true)
	notes.set("manual@example.com", "通过管理页或管理接口停用", false)
	notes.set("auto@example.com", "新账户自动处理验证未通过", true)
	accounts, failures := aistudio.NewAccountStore(root).LoadTolerant()
	if len(failures) > 0 {
		t.Fatal(failures)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	requests := newRequestRegistry(ctx)

	enabled := enableUnmarkedAccounts(accounts, requests, "启动时")
	if strings.Join(enabled, ",") != "auto@example.com,plain@example.com" {
		t.Fatalf("启用的账户 = %v，期望 auto 与 plain", enabled)
	}
	byID := make(map[string]*aistudio.Account)
	for _, account := range accounts {
		byID[account.ID] = account
	}
	for _, id := range []string{"plain@example.com", "auto@example.com", "on@example.com"} {
		if account := byID[id]; !account.Config.Enabled || account.State != aistudio.AccountReady {
			t.Fatalf("%s：enabled=%t state=%s，期望启用且就绪", id, account.Config.Enabled, account.State)
		}
	}
	if account := byID["manual@example.com"]; account.Config.Enabled || account.State != aistudio.AccountDisabled {
		t.Fatal("手动停用的账户应保持停用")
	}
	if !diskEnabled(t, plain) || !diskEnabled(t, auto) || diskEnabled(t, manual) {
		t.Fatal("account.json 写回结果不对")
	}
	if notes.automatic("auto@example.com") || !notes.manual("manual@example.com") {
		t.Fatal("应清除自动停用记录、保留手动停用记录")
	}
	if logCount(requests, "启动时启用 2 个停用账户（没有手动停用记录）") != 1 {
		t.Fatal("应只写一条汇总日志")
	}
}

// TestNewRuntimeEnablesUnmarkedAccounts 服务启动（装配生成服务实例）时：先载入 .disable-reasons.json 再决定，
// account.json 为停用、没有手动停用记录的账户进入账户池时已经启用并就绪，文件随之改写；手动停用的保持停用
func TestNewRuntimeEnablesUnmarkedAccounts(t *testing.T) {
	isolateDisableNotes(t)
	root := filepath.Join(t.TempDir(), "auth")
	plain := writeEnableTestAccount(t, root, "plain@example.com", false)
	manual := writeEnableTestAccount(t, root, "manual@example.com", false)
	auto := writeEnableTestAccount(t, root, "auto@example.com", false)
	now := time.Now()
	notesFile, err := json.Marshal(map[string]disableNote{
		"manual@example.com": {Reason: "通过管理页或管理接口停用", At: now},
		"auto@example.com":   {Reason: "新账户自动处理验证未通过", At: now, Auto: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, disableNotesFileName), notesFile, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	requests := newRequestRegistry(ctx)
	cfg := config.Default()
	cfg.AuthStates = root
	cfg.WAABackend = config.WAABackendGo
	lifecycle, stop := context.WithCancel(ctx)
	_, admin, closeRuntime, err := newRuntime(ctx, lifecycle, cfg, requests)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.pool.WaitBackground)
	defer func() {
		stop()
		if err := closeRuntime(); err != nil {
			t.Error(err)
		}
	}()

	for _, id := range []string{"plain@example.com", "auto@example.com"} {
		if status := testStatus(t, admin.pool, id); !status.Enabled || status.State != aistudio.AccountReady {
			t.Fatalf("%s：enabled=%t state=%s，期望启动后启用且就绪", id, status.Enabled, status.State)
		}
	}
	if status := testStatus(t, admin.pool, "manual@example.com"); status.Enabled || status.State != aistudio.AccountDisabled {
		t.Fatal("手动停用的账户启动后应保持停用")
	}
	if !diskEnabled(t, plain) || !diskEnabled(t, auto) || diskEnabled(t, manual) {
		t.Fatal("启动时应改写没有手动停用记录的 account.json")
	}
	if logCount(requests, "启动时启用 2 个停用账户") != 1 {
		t.Fatal("缺少启动时启用的汇总日志")
	}
	stop()
	if err := closeRuntime(); err != nil {
		t.Fatal(err)
	}
	closeRuntime = func() error { return nil }
	// 关闭时立即写出停用原因：自动停用的记录已清除，手动停用的保留
	content, err := os.ReadFile(filepath.Join(root, disableNotesFileName))
	if err != nil {
		t.Fatal(err)
	}
	var saved map[string]disableNote
	if err := json.Unmarshal(content, &saved); err != nil {
		t.Fatal(err)
	}
	if _, exists := saved["auto@example.com"]; exists {
		t.Fatal("启用后自动停用记录应从文件中清除")
	}
	if note, exists := saved["manual@example.com"]; !exists || note.Auto {
		t.Fatal("手动停用记录应保留")
	}
}

// TestWatcherImportEnablesUnmarkedAccounts 运行中自动导入：account.json 为停用、没有手动停用记录的账户导入后启用并同步模型目录，
// 不再记为“导入时为停用、等待自动处理”；手动停用过的账户保持停用
func TestWatcherImportEnablesUnmarkedAccounts(t *testing.T) {
	notes := isolateDisableNotes(t)
	admin, catalog, _ := enableTestAdmin(t, "existing@example.com")
	root := filepath.Dir(admin.pool.LoginSummaries()[0].Directory)
	plain := writeEnableTestAccount(t, root, "plain@example.com", false)
	manual := writeEnableTestAccount(t, root, "manual@example.com", false)
	notes.set("manual@example.com", "通过管理页或管理接口停用", false)
	settled := time.Now().Add(-time.Minute)
	for _, directory := range []string{plain, manual} {
		for _, path := range []string{directory, filepath.Join(directory, "account.json"), aistudio.StorageStatePath(directory)} {
			if err := os.Chtimes(path, settled, settled); err != nil {
				t.Fatal(err)
			}
		}
	}
	watcher := &accountWatcher{
		admin: admin, failures: map[string]string{}, missing: map[string]int{},
		authFiles: map[string]authFileStat{}, loginPending: map[string]struct{}{},
	}
	watcher.scan(admin.lifecycle)

	if status := testStatus(t, admin.pool, "plain@example.com"); !status.Enabled || status.State != aistudio.AccountReady {
		t.Fatalf("导入的停用账户：enabled=%t state=%s，期望启用且就绪", status.Enabled, status.State)
	}
	if !diskEnabled(t, plain) || diskEnabled(t, manual) {
		t.Fatal("导入时应只改写没有手动停用记录的 account.json")
	}
	if testStatus(t, admin.pool, "manual@example.com").Enabled {
		t.Fatal("手动停用的账户导入后应保持停用")
	}
	if notes.automatic("plain@example.com") {
		t.Fatal("导入后启用的账户不应再有自动停用记录")
	}
	waitFor(t, "导入账户的模型目录同步", func() bool {
		return len(testStatus(t, admin.pool, "plain@example.com").Models) > 0
	})
	for _, id := range catalog.refreshedIDs() {
		if id == "manual@example.com" {
			t.Fatal("停用的账户不应同步模型目录")
		}
	}
	if logCount(admin.requests, "导入时启用 1 个停用账户") != 1 {
		t.Fatal("缺少导入时启用的汇总日志")
	}
}

// TestLoginUpdateEnablesUnmarkedDisabledAccount 认证文件更新后：没有手动停用记录的停用账户启用并同步模型目录
// （原先没有记录的按手动停用处理，一直停用）；手动停用的只载入新 Cookie、保持停用
func TestLoginUpdateEnablesUnmarkedDisabledAccount(t *testing.T) {
	notes := isolateDisableNotes(t)
	admin, _, accounts := enableTestAdmin(t, "plain@example.com", "manual@example.com")
	for _, account := range accounts {
		disableTestAccount(t, admin.pool, account.ID)
		if err := aistudio.WriteStorageState(account.StoragePath, authTestState(t, "NEW", false)); err != nil {
			t.Fatal(err)
		}
	}
	notes.set("manual@example.com", "通过管理页或管理接口停用", false)
	summaries := make(map[string]aistudio.AccountLoginSummary)
	for _, summary := range admin.pool.LoginSummaries() {
		summaries[summary.ID] = summary
	}

	if !admin.applyLoginUpdate(admin.lifecycle, summaries["plain@example.com"]) {
		t.Fatal("账户空闲时应完成载入")
	}
	status := testStatus(t, admin.pool, "plain@example.com")
	if !status.Enabled || status.State != aistudio.AccountReady || len(status.Models) == 0 {
		t.Fatalf("没有手动停用记录：enabled=%t state=%s 模型=%d，期望启用、就绪并同步目录",
			status.Enabled, status.State, len(status.Models))
	}
	if !diskEnabled(t, accounts[0].Directory) {
		t.Fatal("启用应写回 account.json")
	}

	if !admin.applyLoginUpdate(admin.lifecycle, summaries["manual@example.com"]) {
		t.Fatal("账户空闲时应完成载入")
	}
	if testStatus(t, admin.pool, "manual@example.com").Enabled || diskEnabled(t, accounts[1].Directory) {
		t.Fatal("手动停用的账户应保持停用")
	}
	if authTestSAPISID(t, accounts[1]) != "NEW" {
		t.Fatal("手动停用的账户也应载入新 Cookie")
	}
}

// TestDisableNotesFlushBeforeLoad 停用原因合并几秒才写文件：flush 立即写出；重新载入（重建生成服务）前先写出
// 未保存的变化，不会被文件中的旧内容覆盖
func TestDisableNotesFlushBeforeLoad(t *testing.T) {
	directory := t.TempDir()
	store := &disableNotes{notes: make(map[string]disableNote)}
	store.load(directory)
	store.set("a@example.com", "通过管理页或管理接口停用", false)
	path := filepath.Join(directory, disableNotesFileName)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("设置后不应立即写文件")
	}
	store.flush()
	if content, err := os.ReadFile(path); err != nil || !strings.Contains(string(content), "a@example.com") {
		t.Fatalf("flush 后文件应包含记录：%s %v", content, err)
	}
	store.set("b@example.com", "通过管理页或管理接口停用", false)
	store.load(directory)
	if !store.manual("b@example.com") {
		t.Fatal("重新载入前应先写出未保存的停用记录")
	}
}

// TestDeleteAccountClearsDisableNote 删除账户时清除停用原因，同一账户重新上传后不会沿用旧的手动停用记录
func TestDeleteAccountClearsDisableNote(t *testing.T) {
	notes := isolateDisableNotes(t)
	admin, _, accounts := enableTestAdmin(t, "a@example.com", "b@example.com")
	disableTestAccount(t, admin.pool, accounts[0].ID)
	notes.set(accounts[0].ID, "通过管理页或管理接口停用", false)
	if err := admin.DeleteAccount(context.Background(), accounts[0].ID); err != nil {
		t.Fatal(err)
	}
	if notes.manual(accounts[0].ID) {
		t.Fatal("删除账户后应清除停用原因")
	}
}
