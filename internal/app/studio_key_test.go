package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/camoufoxnative"
)

var testStudioKey = "AQ.TESTKEY" + strings.Repeat("k", 24)

// TestStudioKeyFileStorage 记录以 0600 写在账户目录，重新读取得到同样的内容
func TestStudioKeyFileStorage(t *testing.T) {
	directory := t.TempDir()
	at := time.Date(2026, 11, 1, 8, 0, 0, 0, time.UTC)
	record := studioKeyFile{Key: testStudioKey, ProjectName: "projects/123", ProjectNumber: "123", Status: studioKeyOK, FetchedAt: &at}
	if err := writeStudioKeyFile(directory, record); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(directory, studioKeyFileName))
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("权限 = %o，期望 600", info.Mode().Perm())
	}
	loaded, ok, err := readStudioKeyFile(directory)
	if err != nil || !ok || loaded.Key != testStudioKey || loaded.ProjectNumber != "123" || loaded.Version != 1 {
		t.Fatalf("读取 ok=%v err=%v project=%s", ok, err, loaded.ProjectNumber)
	}
	if _, ok, err := readStudioKeyFile(t.TempDir()); ok || err != nil {
		t.Fatalf("没有文件时 ok=%v err=%v", ok, err)
	}
}

// TestStudioKeyViewRedacted 列表项只有末四位，编码后的 JSON 里没有完整 Key（原因里的 Key 也打码）
func TestStudioKeyViewRedacted(t *testing.T) {
	record := studioKeyFile{Key: testStudioKey, ProjectName: "projects/123", Status: studioKeyFailed, Reason: "页面 " + testStudioKey}
	view := studioKeyView("a@example.com", "a@example.com", record)
	encoded, _ := json.Marshal(view)
	if strings.Contains(string(encoded), testStudioKey) || strings.Contains(string(encoded), "TESTKEY") {
		t.Fatalf("列表项泄露 Key: %s", encoded)
	}
	if !view.HasKey || view.Last4 != "kkkk" {
		t.Fatalf("view = %+v", view)
	}
}

// TestStudioKeyFailureReasonRedacted 失败原因去掉 Key 与页面快照，单行
func TestStudioKeyFailureReasonRedacted(t *testing.T) {
	err := &camoufoxnative.StudioKeyError{Reason: camoufoxnative.StudioKeyReasonCreateFailed,
		Cause: errors.New("x " + testStudioKey + "\n第二行" + camoufoxnative.PageSnapshotMarker + `{"body":"secret"}`)}
	reason := studioKeyFailureReason(err)
	if strings.Contains(reason, testStudioKey) || strings.Contains(reason, "secret") || strings.Contains(reason, "\n") {
		t.Fatalf("reason = %q", reason)
	}
}

// TestStudioKeyJobQueue 队列去重、逐个取出，空了之后结束
func TestStudioKeyJobQueue(t *testing.T) {
	var job studioKeyJob
	if !job.enqueue([]string{"a@example.com", "b@example.com", "a@example.com"}) {
		t.Fatal("第一次排队应启动处理")
	}
	if job.enqueue([]string{"b@example.com", "c@example.com"}) {
		t.Fatal("处理中不应再次启动")
	}
	var order []string
	for {
		id, ok := job.next()
		if !ok {
			break
		}
		order = append(order, id)
	}
	if strings.Join(order, ",") != "a@example.com,b@example.com,c@example.com" {
		t.Fatalf("order = %v", order)
	}
	if running, _, queue, _ := job.snapshot(); running || len(queue) != 0 {
		t.Fatalf("结束后 running=%v queue=%v", running, queue)
	}
}

// TestStudioKeyJobClear 停止清空排队中的账户，正在处理的那个不受影响
func TestStudioKeyJobClear(t *testing.T) {
	var job studioKeyJob
	job.enqueue([]string{"a@example.com", "b@example.com", "c@example.com"})
	if id, _ := job.next(); id != "a@example.com" {
		t.Fatalf("first = %q", id)
	}
	job.clear()
	if job.pending() {
		t.Fatal("停止后仍有排队")
	}
	if _, current, queue, _ := job.snapshot(); current != "a@example.com" || len(queue) != 0 {
		t.Fatalf("current=%q queue=%v", current, queue)
	}
	if _, ok := job.next(); ok {
		t.Fatal("停止后不应再取出账户")
	}
}

// TestStudioKeySkipReason 未登录、停用、组织停用、不可用、收尾中的账户跳过；就绪与冷却中（含需要验证账号的整户暂停）的账户不跳过
func TestStudioKeySkipReason(t *testing.T) {
	cases := map[string]aistudio.AccountStatus{
		"disabled": {Enabled: false, State: aistudio.AccountDisabled},
		"auth":     {Enabled: true, State: aistudio.AccountAuthRequired},
		"org":      {Enabled: true, State: aistudio.AccountOrgDisabled},
		"orgflag":  {Enabled: true, State: aistudio.AccountReady, OrgDisabled: true},
		"drain":    {Enabled: true, State: aistudio.AccountReady, Transition: "draining"},
		"unavail":  {Enabled: true, State: aistudio.AccountUnavailable},
	}
	for name, status := range cases {
		if studioKeySkipReason(status) == "" {
			t.Fatalf("%s 应跳过", name)
		}
	}
	for _, state := range []aistudio.AccountState{aistudio.AccountReady, aistudio.AccountCooldown, aistudio.AccountBusy} {
		if reason := studioKeySkipReason(aistudio.AccountStatus{Enabled: true, State: state}); reason != "" {
			t.Fatalf("%s 账户不应跳过: %s", state, reason)
		}
	}
}

// pausedStudioKeyDriver 为测试用的获取 Key 驱动：记下调用时账户是否仍暂停与账户状态，返回假 Key（新建项目与 Key）
type pausedStudioKeyDriver struct {
	verifyTestLogin
	pool   *aistudio.AccountPool
	calls  int
	paused bool
	state  aistudio.AccountState
}

func (driver *pausedStudioKeyDriver) FetchStudioKey(
	_ context.Context, request aistudio.IsolatedLoginRequest, _ aistudio.StorageState,
) (camoufoxnative.StudioKeyResult, error) {
	driver.calls++
	driver.paused = driver.pool.VerifyRequiredPaused(request.AccountID)
	if status, ok := driver.pool.StatusOf(request.AccountID); ok {
		driver.state = status.State
	}
	return camoufoxnative.StudioKeyResult{
		Key: testStudioKey, ProjectName: "projects/123456789012", ProjectNumber: "123456789012", ProjectDisplay: "Gemini Project",
		Created: true, ProjectCreated: true, KeyCount: 1, Steps: []string{"打开创建对话框", "确认新建项目“Create project”", "确认创建 Key“Create key”"},
	}, nil
}

// TestStudioKeyFetchesVerifyPausedAccount 需要验证账号（403 verify-account）而整户暂停的账户照常获取 Key：管理操作的租约不看调度状态，
// 暂停中也能取得；获取前后暂停、403 记录、冷却与账户状态都不变（不解除暂停，账户仍不接请求），只写 Key 记录与运行日志，日志里没有完整 Key
func TestStudioKeyFetchesVerifyPausedAccount(t *testing.T) {
	isolateDisableNotes(t)
	admin, _, accounts := enableTestAdmin(t, "verify-key@example.com")
	accountID := accounts[0].ID
	admin.service.permissionDenied = newPermissionDeniedRegistry(t.TempDir(), admin.requests)
	admin.service.permissionDenied.record(accountID, testRuntimeModel, aistudio.ChannelPlayground, verifyRequiredMessage, "", time.Now())
	pauseVerifyRequired(t, admin.pool, accountID)
	before := testStatus(t, admin.pool, accountID)
	if !admin.pool.VerifyRequiredPaused(accountID) || before.State != aistudio.AccountCooldown {
		t.Fatalf("前提：账户应在整户暂停中（冷却），状态 = %s", before.State)
	}
	driver := &pausedStudioKeyDriver{pool: admin.pool}
	admin.login = driver
	manager := &runtimeManager{current: &runtimeGeneration{admin: admin}}
	if opened := manager.fetchStudioKey(context.Background(), accountID); !opened {
		t.Fatal("需要验证账号的账户应打开浏览器获取，不应跳过")
	}
	// 获取时取得了独占租约（账户显示为忙），整户暂停仍在
	if driver.calls != 1 || !driver.paused || driver.state != aistudio.AccountBusy {
		t.Fatalf("获取时应持有独占租约且仍在暂停中：calls=%d paused=%v state=%s", driver.calls, driver.paused, driver.state)
	}
	after := testStatus(t, admin.pool, accountID)
	if !admin.pool.VerifyRequiredPaused(accountID) || after.State != before.State || after.Enabled != before.Enabled || after.Transition != "" {
		t.Fatalf("获取后暂停与状态应不变：paused=%v state=%s→%s", admin.pool.VerifyRequiredPaused(accountID), before.State, after.State)
	}
	if !reflect.DeepEqual(before.Cooldowns, after.Cooldowns) {
		t.Fatalf("冷却应不变：%+v → %+v", before.Cooldowns, after.Cooldowns)
	}
	if entries := admin.service.permissionDenied.list(); len(entries) != 1 {
		t.Fatalf("403 记录应保留，得到 %d 条", len(entries))
	}
	record, ok, err := readStudioKeyFile(accounts[0].Directory)
	if err != nil || !ok || record.Status != studioKeyOK || record.Key != testStudioKey || !record.Created || !record.ProjectCreated ||
		record.ProjectNumber != "123456789012" {
		t.Fatalf("记录 ok=%v err=%v status=%s project=%s created=%v projectCreated=%v", ok, err, record.Status, record.ProjectNumber,
			record.Created, record.ProjectCreated)
	}
	if !hasLogContaining(admin.requests, "获取 API Key 完成 | 新建项目并新建 Key | 末四位=kkkk | 项目=projects/123456789012",
		"| 步骤=打开创建对话框→确认新建项目“Create project”→确认创建 Key“Create key”") {
		t.Fatalf("日志 = %v", serviceLogMessages(admin.requests))
	}
	for _, message := range serviceLogMessages(admin.requests) {
		if strings.Contains(message, testStudioKey) || strings.Contains(message, "TESTKEY") {
			t.Fatalf("日志泄露 Key: %s", message)
		}
		if strings.Contains(message, "清除 403") || strings.Contains(message, "跳过") {
			t.Fatalf("获取 Key 不应清除 403 记录或跳过: %s", message)
		}
	}
	// 获取期间取得的是独占租约：结束后已释放，请求调度仍因暂停拿不到这个账户
	if lease, err := admin.pool.AcquireAccount(context.Background(), accountID); err != nil {
		t.Fatalf("获取结束后租约应已释放: %v", err)
	} else if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
	if !admin.pool.VerifyRequiredPaused(accountID) {
		t.Fatal("释放租约后暂停应仍在")
	}
}

// TestStudioKeyCacheNoticesExternalChange 文件在进程外被删除或替换后，列表缓存随之失效
func TestStudioKeyCacheNoticesExternalChange(t *testing.T) {
	dir := t.TempDir()
	job := &studioKeyJob{}
	if err := writeStudioKeyFile(dir, studioKeyFile{Key: "AQ.TESTKEYAAAAAAAAAAAAAAAAAAAA1"}); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := job.load("a@example.com", dir); err != nil || !ok {
		t.Fatalf("首次读取: ok=%v err=%v", ok, err)
	}
	if err := os.Remove(filepath.Join(dir, studioKeyFileName)); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := job.load("a@example.com", dir); err != nil || ok {
		t.Fatalf("删除后应读不到: ok=%v err=%v", ok, err)
	}
}
