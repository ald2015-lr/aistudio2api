package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/api"
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
	if err != nil || !ok || loaded.Key != testStudioKey || loaded.ProjectNumber != "123" || loaded.Version != studioKeyVersion {
		t.Fatalf("读取 ok=%v err=%v project=%s", ok, err, loaded.ProjectNumber)
	}
	if _, ok, err := readStudioKeyFile(t.TempDir()); ok || err != nil {
		t.Fatalf("没有文件时 ok=%v err=%v", ok, err)
	}
}

// TestStudioKeyViewRedacted 列表项只有末四位，编码后的 JSON 里没有完整 Key（原因里的 Key 也打码）
func TestStudioKeyViewRedacted(t *testing.T) {
	record := studioKeyFile{Key: testStudioKey, ProjectName: "projects/123", Status: studioKeyFailed, Reason: "页面 " + testStudioKey}
	view := studioKeyView("a@example.com", "a@example.com", record, studioKeyVerdict{})
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
	if !job.enqueue([]string{"a@example.com", "b@example.com", "a@example.com"}, false) {
		t.Fatal("第一次排队应启动处理")
	}
	if job.enqueue([]string{"b@example.com", "c@example.com"}, false) {
		t.Fatal("处理中不应再次启动")
	}
	var order []string
	for {
		id, _, ok := job.next()
		if !ok {
			break
		}
		order = append(order, id)
	}
	if strings.Join(order, ",") != "a@example.com,b@example.com,c@example.com" {
		t.Fatalf("order = %v", order)
	}
	if snapshot := job.snapshot(); snapshot.running || len(snapshot.queue) != 0 {
		t.Fatalf("结束后 running=%v queue=%v", snapshot.running, snapshot.queue)
	}
}

// TestStudioKeyJobClear 停止清空排队中的账户，正在处理的那个不受影响
func TestStudioKeyJobClear(t *testing.T) {
	var job studioKeyJob
	job.enqueue([]string{"a@example.com", "b@example.com", "c@example.com"}, false)
	if id, _, _ := job.next(); id != "a@example.com" {
		t.Fatalf("first = %q", id)
	}
	job.clear()
	if job.pending() {
		t.Fatal("停止后仍有排队")
	}
	if snapshot := job.snapshot(); snapshot.current != "a@example.com" || len(snapshot.queue) != 0 {
		t.Fatalf("current=%q queue=%v", snapshot.current, snapshot.queue)
	}
	if _, _, ok := job.next(); ok {
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
	_ context.Context, request aistudio.IsolatedLoginRequest, _ aistudio.StorageState, _ camoufoxnative.StudioKeyOptions,
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
	if opened := manager.fetchStudioKey(context.Background(), accountID, false); !opened {
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

// TestStudioKeyJobFillMerge 同一账户既按普通方式又按补项目信息排队时按普通方式：先排补项目信息、再排普通的，变成普通；先普通、再补的，仍普通；
// 正在补项目信息的账户再按普通方式排一次会排进去，正在按普通方式处理的不重复排
func TestStudioKeyJobFillMerge(t *testing.T) {
	var job studioKeyJob
	job.enqueue([]string{"a@example.com", "c@example.com"}, true)
	job.enqueue([]string{"b@example.com"}, false)
	job.enqueue([]string{"a@example.com"}, false)
	job.enqueue([]string{"b@example.com"}, true)
	type item struct {
		id   string
		fill bool
	}
	var got []item
	for range 3 {
		id, fill, ok := job.next()
		if !ok {
			t.Fatal("队列提前空了")
		}
		got = append(got, item{id, fill})
	}
	want := []item{{"a@example.com", false}, {"c@example.com", true}, {"b@example.com", false}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("取出 %+v，期望 %+v", got, want)
	}
	// 正在处理 b（普通）：再排 b 不重复；排 b 补项目信息也不重复
	job.enqueue([]string{"b@example.com"}, false)
	job.enqueue([]string{"b@example.com"}, true)
	if job.pending() {
		t.Fatalf("正在按普通方式处理的账户不应再排: %v", job.snapshot().queue)
	}
	job.enqueue([]string{"d@example.com"}, true)
	if id, fill, _ := job.next(); id != "d@example.com" || !fill || !job.snapshot().currentFill {
		t.Fatalf("应取出补项目信息的 d: %s %v", id, fill)
	}
	job.enqueue([]string{"d@example.com"}, true)
	if job.pending() {
		t.Fatal("正在补项目信息的账户再补一次不应重复排")
	}
	job.enqueue([]string{"d@example.com"}, false)
	if id, fill, ok := job.next(); !ok || id != "d@example.com" || fill {
		t.Fatalf("正在补项目信息时按普通方式排的应排进去: %s %v %v", id, fill, ok)
	}
}

// fakeStudioKeyDriver 为测试用的获取 Key 驱动：返回固定结果，记下每次的方式（只读、已保存的 Key）
type fakeStudioKeyDriver struct {
	verifyTestLogin
	result  camoufoxnative.StudioKeyResult
	err     error
	options []camoufoxnative.StudioKeyOptions
}

func (driver *fakeStudioKeyDriver) FetchStudioKey(
	_ context.Context, _ aistudio.IsolatedLoginRequest, _ aistudio.StorageState, options camoufoxnative.StudioKeyOptions,
) (camoufoxnative.StudioKeyResult, error) {
	driver.options = append(driver.options, options)
	return driver.result, driver.err
}

// studioKeyTestManager 创建带 accountIDs 个账户的管理端与 runtimeManager（队列标为运行中，排队时不启动处理协程）
func studioKeyTestManager(t *testing.T, accountIDs ...string) (*runtimeManager, *runtimeAdmin, []*aistudio.Account) {
	t.Helper()
	isolateDisableNotes(t)
	admin, _, accounts := enableTestAdmin(t, accountIDs...)
	manager := &runtimeManager{current: &runtimeGeneration{admin: admin}}
	return manager, admin, accounts
}

var (
	otherStudioKey = "AQ.TESTKEY" + strings.Repeat("o", 24)
	studioKeyAt    = time.Date(2026, 11, 1, 8, 0, 0, 0, time.UTC)
	studioKeyLater = time.Date(2026, 11, 2, 8, 0, 0, 0, time.UTC)
)

// TestStudioKeyClear 清除记录：没有 Key 的删掉文件；保存着 Key 的只清掉失败信息（Key、项目、读取与创建时间不动，状态回到成功，
// 最近尝试回到读取时间）；本来就成功的、没有记录的不动；正在处理的账户不动；不认识的只计数；列表缓存随之失效；运行日志只有计数
func TestStudioKeyClear(t *testing.T) {
	manager, admin, accounts := studioKeyTestManager(t, "gone@example.com", "kept@example.com", "fine@example.com", "busy@example.com", "none@example.com")
	write := func(index int, record studioKeyFile) {
		t.Helper()
		if err := writeStudioKeyFile(accounts[index].Directory, record); err != nil {
			t.Fatal(err)
		}
	}
	write(0, studioKeyFile{Status: studioKeySkipped, Reason: "账户需要验证账号，整户暂停中", AttemptAt: &studioKeyLater})
	write(1, studioKeyFile{Key: testStudioKey, ProjectName: "projects/123456789012", ProjectNumber: "123456789012", ProjectDisplay: "Gemini Project",
		ProjectID: "gen-lang-client-0123456789", Created: true, FetchedAt: &studioKeyAt, CreatedAt: &studioKeyAt, Status: studioKeyFailed,
		Reason: "获取 API Key 超时", AttemptAt: &studioKeyLater})
	write(2, studioKeyFile{Key: otherStudioKey, FetchedAt: &studioKeyAt, Status: studioKeyOK, AttemptAt: &studioKeyAt})
	write(3, studioKeyFile{Status: studioKeyFailed, Reason: "失败"})
	// 列表先读一次，让缓存里有这些记录
	if status, err := manager.StudioKeys(context.Background()); err != nil || len(status.Records) != 4 {
		t.Fatalf("清除前 err=%v records=%d", err, len(status.Records))
	}
	manager.studioKeys.enqueue([]string{accounts[3].ID}, false)
	manager.studioKeys.next()
	ids := []string{accounts[0].ID, accounts[1].ID, accounts[2].ID, accounts[3].ID, accounts[4].ID, "nobody@example.com"}
	result, err := manager.ClearStudioKeys(context.Background(), api.StudioKeyClearInput{AccountIDs: ids})
	if err != nil {
		t.Fatal(err)
	}
	want := api.StudioKeyClearCounts{Deleted: 1, Reset: 1, Busy: 1, Unknown: 1, Unchanged: 2}
	if result.Cleared != want {
		t.Fatalf("计数 = %+v，期望 %+v", result.Cleared, want)
	}
	if _, err := os.Stat(filepath.Join(accounts[0].Directory, studioKeyFileName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("没有 Key 的记录应删掉: %v", err)
	}
	kept, ok, err := readStudioKeyFile(accounts[1].Directory)
	if err != nil || !ok || kept.Key != testStudioKey || kept.Status != studioKeyOK || kept.Reason != "" || kept.ProjectNumber != "123456789012" ||
		kept.ProjectDisplay != "Gemini Project" || kept.ProjectID != "gen-lang-client-0123456789" || !kept.Created ||
		!kept.FetchedAt.Equal(studioKeyAt) || !kept.CreatedAt.Equal(studioKeyAt) || !kept.AttemptAt.Equal(studioKeyAt) {
		t.Fatalf("保存着 Key 的记录只应清掉失败信息: ok=%v err=%v status=%s reason=%q project=%s", ok, err, kept.Status, kept.Reason, kept.ProjectNumber)
	}
	if busy, ok, _ := readStudioKeyFile(accounts[3].Directory); !ok || busy.Status != studioKeyFailed {
		t.Fatal("正在处理的账户不应动")
	}
	if fine, ok, _ := readStudioKeyFile(accounts[2].Directory); !ok || fine.Key != otherStudioKey {
		t.Fatal("本来就成功的记录不应动")
	}
	// 返回的列表（与缓存）已是清除之后的
	byID := map[string]api.StudioKeyRecord{}
	for _, record := range result.Records {
		byID[record.AccountID] = record
	}
	if _, ok := byID[accounts[0].ID]; ok || len(result.Records) != 3 || byID[accounts[1].ID].Status != studioKeyOK || byID[accounts[1].ID].Reason != "" ||
		!byID[accounts[1].ID].HasKey {
		t.Fatalf("清除后的列表 = %+v", result.Records)
	}
	if !hasLogContaining(admin.requests, "清除记录 | 删除=1 | 删除作废的 Key=0 | 清掉失败信息=1 | 正在处理没动=1 | 未知账户=1") {
		t.Fatalf("日志 = %v", serviceLogMessages(admin.requests))
	}
	for _, message := range serviceLogMessages(admin.requests) {
		if strings.Contains(message, "TESTKEY") || strings.Contains(message, "@example.com") && strings.Contains(message, "清除记录") {
			t.Fatalf("清除记录的日志只应有计数: %s", message)
		}
	}
}

// TestStudioKeyClearNeverDeletesKey 清除时在锁内重新读文件：列表里还没有 Key、文件里已经写进了 Key（刚获取完）时只清信息，不删文件
func TestStudioKeyClearNeverDeletesKey(t *testing.T) {
	manager, _, accounts := studioKeyTestManager(t, "race@example.com")
	directory := accounts[0].Directory
	if err := writeStudioKeyFile(directory, studioKeyFile{Status: studioKeyFailed, Reason: "失败"}); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.StudioKeys(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := manager.studioKeys.write(accounts[0].ID, directory, studioKeyFile{Key: testStudioKey, Status: studioKeyFailed, Reason: "x"}); err != nil {
		t.Fatal(err)
	}
	result, err := manager.ClearStudioKeys(context.Background(), api.StudioKeyClearInput{AccountIDs: []string{accounts[0].ID}})
	if err != nil || result.Cleared.Deleted != 0 || result.Cleared.Reset != 1 {
		t.Fatalf("err=%v 计数=%+v", err, result.Cleared)
	}
	if record, ok, _ := readStudioKeyFile(directory); !ok || record.Key != testStudioKey {
		t.Fatal("保存着 Key 的记录不应删掉")
	}
}

// TestStudioKeyClearIgnoresStaleCache 缓存里是没有 Key 的旧记录、磁盘上已经写进了 Key（并且缓存的 stamp 与新文件相同）时，清除只看锁内
// 重新读到的文件：只清信息、不删文件
func TestStudioKeyClearIgnoresStaleCache(t *testing.T) {
	manager, _, accounts := studioKeyTestManager(t, "stale@example.com")
	id, directory := accounts[0].ID, accounts[0].Directory
	stale := studioKeyFile{Status: studioKeyFailed, Reason: "失败"}
	if err := writeStudioKeyFile(directory, stale); err != nil {
		t.Fatal(err)
	}
	if err := writeStudioKeyFile(directory, studioKeyFile{Key: testStudioKey, FetchedAt: &studioKeyAt, Status: studioKeyFailed, Reason: "超时"}); err != nil {
		t.Fatal(err)
	}
	manager.studioKeys.remember(id, directory, statStudioKeyFile(directory), stale, true)
	if cached, ok, err := manager.studioKeys.load(id, directory); err != nil || !ok || cached.Key != "" {
		t.Fatalf("测试前提：缓存里应是没有 Key 的旧记录: ok=%v err=%v", ok, err)
	}
	result, err := manager.ClearStudioKeys(context.Background(), api.StudioKeyClearInput{AccountIDs: []string{id}})
	if err != nil || result.Cleared.Deleted != 0 || result.Cleared.Reset != 1 {
		t.Fatalf("err=%v 计数=%+v", err, result.Cleared)
	}
	record, ok, err := readStudioKeyFile(directory)
	if err != nil || !ok || record.Key != testStudioKey || record.Status != studioKeyOK || record.Reason != "" {
		t.Fatalf("保存着 Key 的记录只应清信息: ok=%v err=%v status=%s", ok, err, record.Status)
	}
	if len(result.Records) != 1 || !result.Records[0].HasKey || result.Records[0].Status != studioKeyOK {
		t.Fatalf("清除后的列表 = %+v", result.Records)
	}
}

// TestStudioKeyClearConcurrentLoad 管理页读列表（load）与清除同时进行：清除之后的列表里不再有删掉的记录与清掉的失败信息
// （缓存不会把读到的旧内容配上清除后的 stamp 留下来）
func TestStudioKeyClearConcurrentLoad(t *testing.T) {
	manager, _, accounts := studioKeyTestManager(t, "gone@example.com", "kept@example.com")
	job := &manager.studioKeys
	for round := 0; round < 200; round++ {
		if err := writeStudioKeyFile(accounts[0].Directory, studioKeyFile{Status: studioKeySkipped, Reason: "账户需要验证账号，整户暂停中"}); err != nil {
			t.Fatal(err)
		}
		if err := writeStudioKeyFile(accounts[1].Directory, studioKeyFile{Key: testStudioKey, FetchedAt: &studioKeyAt, Status: studioKeyFailed, Reason: "超时"}); err != nil {
			t.Fatal(err)
		}
		stop := make(chan struct{})
		done := make(chan struct{})
		go func() {
			defer close(done)
			for {
				select {
				case <-stop:
					return
				default:
				}
				for _, account := range accounts {
					_, _, _ = job.load(account.ID, account.Directory)
				}
				runtime.Gosched()
			}
		}()
		_, err := manager.ClearStudioKeys(context.Background(), api.StudioKeyClearInput{AccountIDs: []string{accounts[0].ID, accounts[1].ID}})
		close(stop)
		<-done
		if err != nil {
			t.Fatal(err)
		}
		if _, ok, err := job.load(accounts[0].ID, accounts[0].Directory); err != nil || ok {
			t.Fatalf("第 %d 轮：删掉的记录仍在列表里: ok=%v err=%v", round, ok, err)
		}
		if record, ok, err := job.load(accounts[1].ID, accounts[1].Directory); err != nil || !ok || record.Key != testStudioKey ||
			record.Status != studioKeyOK || record.Reason != "" {
			t.Fatalf("第 %d 轮：清掉的失败信息仍在列表里: ok=%v err=%v status=%s", round, ok, err, record.Status)
		}
	}
}

// TestStudioKeyClearConcurrentWrite 获取写记录（job.write，写进 Key）与清除同时进行：无论谁先，Key 都在，列表里也有 Key
func TestStudioKeyClearConcurrentWrite(t *testing.T) {
	manager, _, accounts := studioKeyTestManager(t, "race@example.com")
	id, directory := accounts[0].ID, accounts[0].Directory
	job := &manager.studioKeys
	for round := 0; round < 200; round++ {
		if err := writeStudioKeyFile(directory, studioKeyFile{Status: studioKeyFailed, Reason: "失败"}); err != nil {
			t.Fatal(err)
		}
		// 列表先读一次：缓存里是没有 Key 的记录
		if _, ok, err := job.load(id, directory); err != nil || !ok {
			t.Fatalf("第 %d 轮：ok=%v err=%v", round, ok, err)
		}
		written := make(chan error, 1)
		go func() {
			written <- job.write(id, directory, studioKeyFile{Key: testStudioKey, FetchedAt: &studioKeyAt, Status: studioKeyOK})
		}()
		_, err := manager.ClearStudioKeys(context.Background(), api.StudioKeyClearInput{AccountIDs: []string{id}})
		if writeErr := <-written; writeErr != nil || err != nil {
			t.Fatalf("第 %d 轮：write=%v clear=%v", round, writeErr, err)
		}
		if record, ok, err := readStudioKeyFile(directory); err != nil || !ok || record.Key != testStudioKey {
			t.Fatalf("第 %d 轮：Key 被清除删掉了: ok=%v err=%v", round, ok, err)
		}
		if record, ok, err := job.load(id, directory); err != nil || !ok || record.Key != testStudioKey {
			t.Fatalf("第 %d 轮：列表里没有 Key: ok=%v err=%v", round, ok, err)
		}
	}
}

// TestStudioKeyClearWaitsForWrite 清除与写记录互斥：写记录持有 files 锁期间清除不完成、不删文件；写记录在锁内写进 Key 后，清除在锁内
// 重新读到的是有 Key 的记录，不删（清除不拿锁时这里会失败）
func TestStudioKeyClearWaitsForWrite(t *testing.T) {
	manager, _, accounts := studioKeyTestManager(t, "wait@example.com")
	id, directory := accounts[0].ID, accounts[0].Directory
	job := &manager.studioKeys
	if err := writeStudioKeyFile(directory, studioKeyFile{Status: studioKeyFailed, Reason: "失败"}); err != nil {
		t.Fatal(err)
	}
	job.files.Lock()
	done := make(chan api.StudioKeyClearResult, 1)
	go func() {
		result, err := manager.ClearStudioKeys(context.Background(), api.StudioKeyClearInput{AccountIDs: []string{id}})
		if err != nil {
			t.Error(err)
		}
		done <- result
	}()
	select {
	case <-done:
		job.files.Unlock()
		t.Fatal("写记录持有锁时清除不应完成")
	case <-time.After(300 * time.Millisecond):
	}
	if _, ok, err := readStudioKeyFile(directory); err != nil || !ok {
		job.files.Unlock()
		t.Fatalf("写记录持有锁时清除不应删文件: ok=%v err=%v", ok, err)
	}
	// 写记录在锁内把 Key 写进去
	err := writeStudioKeyFile(directory, studioKeyFile{Key: testStudioKey, FetchedAt: &studioKeyAt, Status: studioKeyOK})
	job.files.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	result := <-done
	if result.Cleared.Deleted != 0 || result.Cleared.Unchanged != 1 {
		t.Fatalf("计数 = %+v", result.Cleared)
	}
	if record, ok, err := readStudioKeyFile(directory); err != nil || !ok || record.Key != testStudioKey {
		t.Fatalf("Key 被清除删掉了: ok=%v err=%v", ok, err)
	}
}

// TestStudioKeyFillProjectQueue 补项目信息只排保存着 Key 的账户：all + missing_project 只排没有项目编号的；按账户排时没有 Key 的不排；
// 都不符合时不算错误
func TestStudioKeyFillProjectQueue(t *testing.T) {
	manager, _, accounts := studioKeyTestManager(t, "nokey@example.com", "noproject@example.com", "project@example.com")
	manager.studioKeys.running = true
	if err := writeStudioKeyFile(accounts[1].Directory, studioKeyFile{Key: testStudioKey, Status: studioKeyOK}); err != nil {
		t.Fatal(err)
	}
	if err := writeStudioKeyFile(accounts[2].Directory, studioKeyFile{Key: otherStudioKey, ProjectNumber: "123456789012", Status: studioKeyOK}); err != nil {
		t.Fatal(err)
	}
	status, err := manager.FetchStudioKeys(context.Background(), api.StudioKeyFetchInput{All: true, MissingProject: true})
	if err != nil || !slices.Equal(status.Queue, []string{accounts[1].ID}) || !manager.studioKeys.fill[accounts[1].ID] {
		t.Fatalf("应只排保存着 Key、没有项目编号的账户: err=%v queue=%v", err, status.Queue)
	}
	manager.studioKeys.clear()
	status, err = manager.FetchStudioKeys(context.Background(), api.StudioKeyFetchInput{
		AccountIDs: []string{accounts[0].ID, accounts[2].ID}, FillProject: true,
	})
	if err != nil || !slices.Equal(status.Queue, []string{accounts[2].ID}) || !manager.studioKeys.fill[accounts[2].ID] {
		t.Fatalf("按账户补项目信息时没有 Key 的不排: err=%v queue=%v", err, status.Queue)
	}
	manager.studioKeys.clear()
	if status, err := manager.FetchStudioKeys(context.Background(), api.StudioKeyFetchInput{AccountIDs: []string{accounts[0].ID}, FillProject: true}); err != nil ||
		len(status.Queue) != 0 {
		t.Fatalf("没有可补的账户时不算错误、不排队: err=%v queue=%v", err, status.Queue)
	}
	if _, err := manager.FetchStudioKeys(context.Background(), api.StudioKeyFetchInput{AccountIDs: []string{"nobody@example.com"}, FillProject: true}); err == nil {
		t.Fatal("不认识的账户应报错")
	}
	// 同一账户先补项目信息、再按普通方式排：按普通方式
	manager.FetchStudioKeys(context.Background(), api.StudioKeyFetchInput{AccountIDs: []string{accounts[1].ID}, FillProject: true})
	manager.FetchStudioKeys(context.Background(), api.StudioKeyFetchInput{AccountIDs: []string{accounts[1].ID}})
	if manager.studioKeys.fill[accounts[1].ID] {
		t.Fatal("普通方式应覆盖补项目信息")
	}
}

// TestStudioKeyFillReadOnly 补项目信息：驱动收到只读与已保存的 Key；读到项目时写进记录（含项目 ID），Key 与创建标记不变；
// 运行日志没有完整 Key
func TestStudioKeyFillReadOnly(t *testing.T) {
	manager, admin, accounts := studioKeyTestManager(t, "fill@example.com")
	directory := accounts[0].Directory
	if err := writeStudioKeyFile(directory, studioKeyFile{Key: testStudioKey, Created: true, CreatedAt: &studioKeyAt, FetchedAt: &studioKeyAt, Status: studioKeyOK}); err != nil {
		t.Fatal(err)
	}
	driver := &fakeStudioKeyDriver{result: camoufoxnative.StudioKeyResult{Key: testStudioKey, ProjectName: "projects/210987654321",
		ProjectNumber: "210987654321", ProjectDisplay: "Default Gemini Project", ProjectID: "gen-lang-client-0987654321", KeyCount: 2,
		Steps: []string{"项目信息：从 Key 详情读到"}}}
	admin.login = driver
	if !manager.fetchStudioKey(context.Background(), accounts[0].ID, true) {
		t.Fatal("应打开浏览器")
	}
	if len(driver.options) != 1 || !driver.options[0].ReadOnly || driver.options[0].SavedKey != testStudioKey {
		t.Fatalf("应只读并带上已保存的 Key: %d 次", len(driver.options))
	}
	record, _, _ := readStudioKeyFile(directory)
	if record.Key != testStudioKey || record.ProjectNumber != "210987654321" || record.ProjectID != "gen-lang-client-0987654321" ||
		record.ProjectDisplay != "Default Gemini Project" || !record.Created || !record.CreatedAt.Equal(studioKeyAt) || record.Status != studioKeyOK {
		t.Fatalf("记录 = project=%s id=%s created=%v status=%s", record.ProjectNumber, record.ProjectID, record.Created, record.Status)
	}
	if !hasLogContaining(admin.requests, "补 API Key 项目信息完成 | 只读 | 末四位=kkkk | 项目=projects/210987654321 | 项目编号=210987654321",
		"显示名=Default Gemini Project | 项目 ID=gen-lang-client-0987654321") {
		t.Fatalf("日志 = %v", serviceLogMessages(admin.requests))
	}
	for _, message := range serviceLogMessages(admin.requests) {
		if strings.Contains(message, testStudioKey) {
			t.Fatalf("日志泄露 Key: %s", message)
		}
	}
	// 记录被清除（没有 Key）之后再补：不打开浏览器、不写记录
	if err := os.Remove(filepath.Join(directory, studioKeyFileName)); err != nil {
		t.Fatal(err)
	}
	if manager.fetchStudioKey(context.Background(), accounts[0].ID, true) || len(driver.options) != 1 {
		t.Fatal("没有保存的 Key 时补项目信息不应打开浏览器")
	}
	if _, ok, _ := readStudioKeyFile(directory); ok {
		t.Fatal("没有保存的 Key 时补项目信息不应写记录")
	}
}

// TestStudioKeyFillSavedMissing 补项目信息时页面上没有已保存的 Key：记为失败、写明原因，Key 与项目保留（仍在“成功”标签，标着保留上次的 Key）
func TestStudioKeyFillSavedMissing(t *testing.T) {
	manager, admin, accounts := studioKeyTestManager(t, "missing@example.com")
	directory := accounts[0].Directory
	if err := writeStudioKeyFile(directory, studioKeyFile{Key: testStudioKey, ProjectDisplay: "Gemini Project", FetchedAt: &studioKeyAt, Status: studioKeyOK}); err != nil {
		t.Fatal(err)
	}
	admin.login = &fakeStudioKeyDriver{err: &camoufoxnative.StudioKeyError{Reason: camoufoxnative.StudioKeyReasonSavedMissing}}
	manager.fetchStudioKey(context.Background(), accounts[0].ID, true)
	record, _, _ := readStudioKeyFile(directory)
	if record.Key != testStudioKey || record.Status != studioKeyFailed || record.Reason != camoufoxnative.StudioKeyReasonSavedMissing ||
		record.ProjectDisplay != "Gemini Project" || !record.FetchedAt.Equal(studioKeyAt) {
		t.Fatalf("记录 status=%s reason=%q", record.Status, record.Reason)
	}
	if view := studioKeyView(accounts[0].ID, "", record, studioKeyVerdict{}); !view.HasKey || view.Status != studioKeyFailed {
		t.Fatalf("列表项 = %+v", view)
	}
	// 驱动万一返回了别的 Key：不改记录里的 Key
	admin.login = &fakeStudioKeyDriver{result: camoufoxnative.StudioKeyResult{Key: otherStudioKey, ProjectNumber: "1"}}
	manager.fetchStudioKey(context.Background(), accounts[0].ID, true)
	if record, _, _ := readStudioKeyFile(directory); record.Key != testStudioKey || record.Status != studioKeyFailed {
		t.Fatal("补项目信息不应换掉已保存的 Key")
	}
}

// TestStudioKeyKeepsProject 读到与记录里同一条 Key、这次没有项目信息时沿用记录里的项目字段（不清空）；读到的是另一条 Key 时不沿用；
// 普通获取把已保存的 Key 交给驱动（页面上有它时优先选它），不是只读
func TestStudioKeyKeepsProject(t *testing.T) {
	manager, admin, accounts := studioKeyTestManager(t, "keep@example.com")
	directory := accounts[0].Directory
	previous := studioKeyFile{Key: testStudioKey, ProjectName: "projects/123456789012", ProjectNumber: "123456789012", ProjectDisplay: "Gemini Project",
		ProjectID: "gen-lang-client-0123456789", FetchedAt: &studioKeyAt, Status: studioKeyOK}
	if err := writeStudioKeyFile(directory, previous); err != nil {
		t.Fatal(err)
	}
	driver := &fakeStudioKeyDriver{result: camoufoxnative.StudioKeyResult{Key: testStudioKey, KeyCount: 1}}
	admin.login = driver
	manager.fetchStudioKey(context.Background(), accounts[0].ID, false)
	if len(driver.options) != 1 || driver.options[0].ReadOnly || driver.options[0].SavedKey != testStudioKey {
		t.Fatal("普通获取应带上已保存的 Key、不是只读")
	}
	record, _, _ := readStudioKeyFile(directory)
	if record.ProjectNumber != "123456789012" || record.ProjectName != "projects/123456789012" || record.ProjectDisplay != "Gemini Project" ||
		record.ProjectID != "gen-lang-client-0123456789" {
		t.Fatalf("同一条 Key 没读到项目时应沿用: %+v", record.ProjectNumber)
	}
	// 只读到显示名：编号沿用，显示名用新的
	driver.result = camoufoxnative.StudioKeyResult{Key: testStudioKey, ProjectDisplay: "Renamed Project"}
	manager.fetchStudioKey(context.Background(), accounts[0].ID, true)
	if record, _, _ := readStudioKeyFile(directory); record.ProjectNumber != "123456789012" || record.ProjectDisplay != "Renamed Project" {
		t.Fatalf("应沿用编号、用新显示名: %s %s", record.ProjectNumber, record.ProjectDisplay)
	}
	driver.result = camoufoxnative.StudioKeyResult{Key: otherStudioKey}
	manager.fetchStudioKey(context.Background(), accounts[0].ID, false)
	if record, _, _ := readStudioKeyFile(directory); record.Key != otherStudioKey || record.ProjectNumber != "" || record.ProjectID != "" {
		t.Fatal("换了一条 Key 时不应沿用上一条的项目")
	}
}

// TestStudioKeyExportProject 导出与 reveal 带上项目 ID 与显示名
func TestStudioKeyExportProject(t *testing.T) {
	manager, _, accounts := studioKeyTestManager(t, "export@example.com")
	if err := writeStudioKeyFile(accounts[0].Directory, studioKeyFile{Key: testStudioKey, ProjectName: "projects/123456789012", ProjectNumber: "123456789012",
		ProjectDisplay: "Gemini Project", ProjectID: "gen-lang-client-0123456789", Status: studioKeyOK}); err != nil {
		t.Fatal(err)
	}
	export, err := manager.ExportStudioKeys(context.Background())
	if err != nil || len(export.Keys) != 1 || export.Keys[0].ProjectID != "gen-lang-client-0123456789" || export.Keys[0].ProjectDisplay != "Gemini Project" ||
		export.Keys[0].Key != testStudioKey {
		t.Fatalf("导出 err=%v keys=%d", err, len(export.Keys))
	}
	encoded, _ := json.Marshal(export.Keys[0])
	if !strings.Contains(string(encoded), `"project_id":"gen-lang-client-0123456789"`) {
		t.Fatalf("导出 JSON 应有 project_id: %s", strings.ReplaceAll(string(encoded), testStudioKey, "<key>"))
	}
	secret, err := manager.RevealStudioKey(context.Background(), accounts[0].ID)
	if err != nil || secret.ProjectID != "gen-lang-client-0123456789" {
		t.Fatalf("reveal err=%v", err)
	}
	status, _ := manager.StudioKeys(context.Background())
	if len(status.Records) != 1 || status.Records[0].ProjectID != "gen-lang-client-0123456789" {
		t.Fatalf("列表应有项目 ID: %+v", status.Records)
	}
}

// writeStudioKeyRecords 给账户写记录（按 accounts 的顺序，records 里没有的不写）
func writeStudioKeyRecords(t *testing.T, accounts []*aistudio.Account, records map[int]studioKeyFile) {
	t.Helper()
	for index, record := range records {
		if err := writeStudioKeyFile(accounts[index].Directory, record); err != nil {
			t.Fatal(err)
		}
	}
}

// studioKeyRecordsByID 返回列表里的记录（按账户）
func studioKeyRecordsByID(t *testing.T, manager *runtimeManager) map[string]api.StudioKeyRecord {
	t.Helper()
	status, err := manager.StudioKeys(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]api.StudioKeyRecord{}
	for _, record := range status.Records {
		byID[record.AccountID] = record
	}
	return byID
}

// TestStudioKeyInvalidShared 同一条 Key 保存在两个以上账户里时作废：列表里两条都是 invalid（不算有 Key、没有末四位与项目、原因写明另外几个账户、
// JSON 里没有完整 Key），各自独有的照常；reveal 返回 409；导出不含它；“全部获取”（只补没有 Key 的）把它们排进去，补项目信息不排
func TestStudioKeyInvalidShared(t *testing.T) {
	manager, _, accounts := studioKeyTestManager(t, "one@example.com", "two@example.com", "three@example.com", "own@example.com")
	manager.studioKeys.running = true
	shared := studioKeyFile{Key: testStudioKey, ProjectName: "projects/123456789012", ProjectNumber: "123456789012", FetchedAt: &studioKeyAt, Status: studioKeyOK}
	writeStudioKeyRecords(t, accounts, map[int]studioKeyFile{0: shared, 1: shared, 2: shared,
		3: {Key: otherStudioKey, FetchedAt: &studioKeyAt, Status: studioKeyOK}})
	byID := studioKeyRecordsByID(t, manager)
	for _, index := range []int{0, 1, 2} {
		record := byID[accounts[index].ID]
		if record.Status != studioKeyInvalid || record.HasKey || record.Last4 != "" || record.ProjectNumber != "" ||
			!strings.Contains(record.Reason, "与另外 2 个账户保存的相同") || !strings.Contains(record.Reason, "页面自己的公共 Key") {
			t.Fatalf("第 %d 个账户的记录应作废: %+v", index, record)
		}
	}
	if own := byID[accounts[3].ID]; own.Status != studioKeyOK || !own.HasKey || own.Last4 != "oooo" {
		t.Fatalf("独有的 Key 照常: %+v", own)
	}
	status, _ := manager.StudioKeys(context.Background())
	if encoded, _ := json.Marshal(status); strings.Contains(string(encoded), "TESTKEY") {
		t.Fatalf("列表泄露 Key: %s", encoded)
	}
	_, err := manager.RevealStudioKey(context.Background(), accounts[0].ID)
	var operation *adminOperationError
	if !errors.As(err, &operation) || operation.status != 409 || operation.code != "studio_key_invalid" || strings.Contains(err.Error(), "TESTKEY") {
		t.Fatalf("reveal 作废的 Key 应 409: %v", err)
	}
	if secret, err := manager.RevealStudioKey(context.Background(), accounts[3].ID); err != nil || secret.Key != otherStudioKey {
		t.Fatalf("独有的 Key 照常 reveal: %v", err)
	}
	export, err := manager.ExportStudioKeys(context.Background())
	if err != nil || len(export.Keys) != 1 || export.Keys[0].AccountID != accounts[3].ID {
		t.Fatalf("导出只应有独有的 Key: err=%v keys=%d", err, len(export.Keys))
	}
	queued, err := manager.FetchStudioKeys(context.Background(), api.StudioKeyFetchInput{All: true, MissingOnly: true})
	if err != nil || !slices.Equal(queued.Queue, []string{accounts[0].ID, accounts[1].ID, accounts[2].ID}) {
		t.Fatalf("全部获取应把作废的当作没有 Key: err=%v queue=%v", err, queued.Queue)
	}
	manager.studioKeys.clear()
	filled, err := manager.FetchStudioKeys(context.Background(), api.StudioKeyFetchInput{All: true, FillProject: true})
	if err != nil || !slices.Equal(filled.Queue, []string{accounts[3].ID}) {
		t.Fatalf("补项目信息不应排作废的: err=%v queue=%v", err, filled.Queue)
	}
}

// TestStudioKeyInvalidFetch 普通获取作废记录的账户：已保存的 Key 不交给驱动、失败时不保留（Key 与项目去掉，进失败标签），其余保存着它的
// 账户写上 invalid 标记（哪怕只剩一个也仍作废）；补项目信息不打开浏览器、不写记录
func TestStudioKeyInvalidFetch(t *testing.T) {
	manager, admin, accounts := studioKeyTestManager(t, "one@example.com", "two@example.com")
	shared := studioKeyFile{Key: testStudioKey, ProjectNumber: "123456789012", Created: true, FetchedAt: &studioKeyAt, Status: studioKeyOK}
	writeStudioKeyRecords(t, accounts, map[int]studioKeyFile{0: shared, 1: shared})
	driver := &fakeStudioKeyDriver{err: &camoufoxnative.StudioKeyError{Reason: camoufoxnative.StudioKeyReasonUnreadable}}
	admin.login = driver
	if manager.fetchStudioKey(context.Background(), accounts[0].ID, true) || len(driver.options) != 0 {
		t.Fatal("补项目信息不应为作废的 Key 打开浏览器")
	}
	if !manager.fetchStudioKey(context.Background(), accounts[0].ID, false) || len(driver.options) != 1 || driver.options[0].SavedKey != "" {
		t.Fatalf("普通获取不应把作废的 Key 交给驱动: %+v", len(driver.options))
	}
	record, _, _ := readStudioKeyFile(accounts[0].Directory)
	if record.Key != "" || record.ProjectNumber != "" || record.Created || record.Status != studioKeyFailed ||
		record.Reason != camoufoxnative.StudioKeyReasonUnreadable {
		t.Fatalf("失败时不应保留作废的 Key: status=%s reason=%q project=%s", record.Status, record.Reason, record.ProjectNumber)
	}
	other, _, _ := readStudioKeyFile(accounts[1].Directory)
	if other.Key != testStudioKey || !other.Invalid {
		t.Fatal("另一个账户的记录应写上 invalid 标记")
	}
	byID := studioKeyRecordsByID(t, manager)
	if view := byID[accounts[1].ID]; view.Status != studioKeyInvalid || view.HasKey || !strings.Contains(view.Reason, "曾与其他账户保存的相同") {
		t.Fatalf("只剩一个账户保存着它也仍作废: %+v", view)
	}
	if view := byID[accounts[0].ID]; view.Status != studioKeyFailed || view.HasKey {
		t.Fatalf("重新获取失败的账户进失败标签: %+v", view)
	}
	// 只剩下的那个账户重新获取成功：换成新的 Key，标记随之去掉
	driver.err, driver.result = nil, camoufoxnative.StudioKeyResult{Key: otherStudioKey, KeyCount: 1}
	manager.fetchStudioKey(context.Background(), accounts[1].ID, false)
	if record, _, _ := readStudioKeyFile(accounts[1].Directory); record.Key != otherStudioKey || record.Invalid || record.Status != studioKeyOK {
		t.Fatalf("获取到新 Key 后应有效: key=%v invalid=%v", record.Key == otherStudioKey, record.Invalid)
	}
	for _, message := range serviceLogMessages(admin.requests) {
		if strings.Contains(message, "TESTKEY") {
			t.Fatalf("日志泄露 Key: %s", message)
		}
	}
}

// TestStudioKeyDuplicateNotSaved 获取到的 Key 与别的账户保存的相同：不保存（上次的有效 Key 照旧保留），记为失败并写明原因，
// 别的账户的记录写上 invalid 标记；日志只有末四位
func TestStudioKeyDuplicateNotSaved(t *testing.T) {
	manager, admin, accounts := studioKeyTestManager(t, "fetch@example.com", "holder@example.com")
	previous := studioKeyFile{Key: otherStudioKey, ProjectNumber: "210987654321", FetchedAt: &studioKeyAt, Status: studioKeyOK}
	writeStudioKeyRecords(t, accounts, map[int]studioKeyFile{0: previous, 1: {Key: testStudioKey, FetchedAt: &studioKeyAt, Status: studioKeyOK}})
	admin.login = &fakeStudioKeyDriver{result: camoufoxnative.StudioKeyResult{Key: testStudioKey, ProjectNumber: "123456789012", KeyCount: 1}}
	manager.fetchStudioKey(context.Background(), accounts[0].ID, false)
	record, _, _ := readStudioKeyFile(accounts[0].Directory)
	if record.Key != otherStudioKey || record.ProjectNumber != "210987654321" || record.Status != studioKeyFailed ||
		record.Reason != "读到的 Key 与另外 1 个账户相同（多半是页面自己的公共 Key，不是这个账户的），没有保存" {
		t.Fatalf("重复的 Key 不应保存: status=%s reason=%q", record.Status, record.Reason)
	}
	if holder, _, _ := readStudioKeyFile(accounts[1].Directory); holder.Key != testStudioKey || !holder.Invalid {
		t.Fatal("别的账户的记录应写上 invalid 标记")
	}
	byID := studioKeyRecordsByID(t, manager)
	if view := byID[accounts[0].ID]; !view.HasKey || view.Last4 != "oooo" || view.Status != studioKeyFailed {
		t.Fatalf("上次的有效 Key 照旧保留: %+v", view)
	}
	if view := byID[accounts[1].ID]; view.Status != studioKeyInvalid || view.HasKey {
		t.Fatalf("别的账户的记录应作废: %+v", view)
	}
	if !hasLogContaining(admin.requests, "获取 API Key 失败", "末四位=kkkk", "没有保存") {
		t.Fatalf("日志 = %v", serviceLogMessages(admin.requests))
	}
	for _, message := range serviceLogMessages(admin.requests) {
		if strings.Contains(message, "TESTKEY") {
			t.Fatalf("日志泄露 Key: %s", message)
		}
	}
}

// TestStudioKeySavedIsClient 驱动认出已保存的 Key 就是页面自己的公共 Key（只有这一个账户保存着它，按个数认不出）：去掉这条 Key 与项目，记为失败
func TestStudioKeySavedIsClient(t *testing.T) {
	manager, admin, accounts := studioKeyTestManager(t, "single@example.com")
	writeStudioKeyRecords(t, accounts, map[int]studioKeyFile{0: {Key: testStudioKey, ProjectNumber: "123456789012", FetchedAt: &studioKeyAt, Status: studioKeyOK}})
	admin.login = &fakeStudioKeyDriver{err: &camoufoxnative.StudioKeyError{Reason: camoufoxnative.StudioKeyReasonSavedIsClient, SavedIsClient: true}}
	manager.fetchStudioKey(context.Background(), accounts[0].ID, true)
	record, _, _ := readStudioKeyFile(accounts[0].Directory)
	if record.Key != "" || record.ProjectNumber != "" || record.Status != studioKeyFailed || record.Reason != camoufoxnative.StudioKeyReasonSavedIsClient {
		t.Fatalf("应去掉公共 Key: status=%s reason=%q", record.Status, record.Reason)
	}
}

// TestStudioKeyClearInvalid 清除作废的记录：连同 Key 删掉（这是“清除不删 Key”的唯一例外；记录只留下这条 Key 的指纹），计入 invalid；同一次清除里按开始时算，
// 先删掉的不让后面的变成有效；不在这次清除里的其余账户写上 invalid 标记（只剩一个也仍作废）；有效的 Key 照旧只清信息
func TestStudioKeyClearInvalid(t *testing.T) {
	manager, admin, accounts := studioKeyTestManager(t, "one@example.com", "two@example.com", "left@example.com", "own@example.com")
	shared := studioKeyFile{Key: testStudioKey, FetchedAt: &studioKeyAt, Status: studioKeyOK}
	writeStudioKeyRecords(t, accounts, map[int]studioKeyFile{0: shared, 1: shared, 2: shared,
		3: {Key: otherStudioKey, FetchedAt: &studioKeyAt, Status: studioKeyFailed, Reason: "超时"}})
	result, err := manager.ClearStudioKeys(context.Background(), api.StudioKeyClearInput{AccountIDs: []string{accounts[0].ID, accounts[1].ID, accounts[3].ID}})
	if err != nil || result.Cleared != (api.StudioKeyClearCounts{Invalid: 2, Reset: 1}) {
		t.Fatalf("计数 err=%v %+v", err, result.Cleared)
	}
	for _, index := range []int{0, 1} {
		// 记录连同 Key 清除，只留下这条 Key 的指纹（列表里不显示）
		if record, ok, err := readStudioKeyFile(accounts[index].Directory); err != nil || !ok || !studioKeyTombstone(record) ||
			!slices.Equal(record.VoidKeys, []string{studioKeyFingerprint(testStudioKey)}) {
			t.Fatalf("作废的记录应只留下指纹: ok=%v err=%v key=%v status=%q", ok, err, record.Key != "", record.Status)
		}
	}
	if left, _, _ := readStudioKeyFile(accounts[2].Directory); left.Key != testStudioKey || !left.Invalid {
		t.Fatal("不在这次清除里的账户应写上 invalid 标记")
	}
	if own, _, _ := readStudioKeyFile(accounts[3].Directory); own.Key != otherStudioKey || own.Status != studioKeyOK {
		t.Fatal("有效的 Key 只清信息")
	}
	byID := map[string]api.StudioKeyRecord{}
	for _, record := range result.Records {
		byID[record.AccountID] = record
	}
	if len(result.Records) != 2 || byID[accounts[2].ID].Status != studioKeyInvalid || !byID[accounts[3].ID].HasKey {
		t.Fatalf("清除后的列表 = %+v", result.Records)
	}
	// 清除前读全部记录时就给三条都写上了标记（见 studioKeyIndex），清除时不必再标
	if !hasLogContaining(admin.requests, "同一条 Key 保存在 3 个账户里，记录标为作废", "标记=3") ||
		!hasLogContaining(admin.requests, "清除记录 | 删除=0 | 删除作废的 Key=2 | 清掉失败信息=1", "其余同一 Key 标为作废=0") {
		t.Fatalf("日志 = %v", serviceLogMessages(admin.requests))
	}
	// 剩下的那个也清掉
	if result, err := manager.ClearStudioKeys(context.Background(), api.StudioKeyClearInput{AccountIDs: []string{accounts[2].ID}}); err != nil ||
		result.Cleared.Invalid != 1 {
		t.Fatalf("只剩一个的作废记录也应删掉: err=%v %+v", err, result.Cleared)
	}
}

// TestStudioKeyWriteKeepsInvalid 写记录时磁盘上同一条 Key 已标着 invalid（获取期间被别处标上）：保留标记；换了 Key 时不保留
func TestStudioKeyWriteKeepsInvalid(t *testing.T) {
	var job studioKeyJob
	directory := t.TempDir()
	if err := writeStudioKeyFile(directory, studioKeyFile{Key: testStudioKey, Invalid: true, Status: studioKeyOK}); err != nil {
		t.Fatal(err)
	}
	if err := job.write("a@example.com", directory, studioKeyFile{Key: testStudioKey, Status: studioKeyFailed}); err != nil {
		t.Fatal(err)
	}
	if record, _, _ := readStudioKeyFile(directory); !record.Invalid {
		t.Fatal("同一条 Key 应保留 invalid 标记")
	}
	if err := job.write("a@example.com", directory, studioKeyFile{Key: otherStudioKey, Status: studioKeyOK}); err != nil {
		t.Fatal(err)
	}
	if record, _, _ := readStudioKeyFile(directory); record.Invalid {
		t.Fatal("换了 Key 时不应保留标记")
	}
}

// studioKeyNoneValid 断言没有账户算有 Key、导出为空（作废的 Key 不能变回“成功”）
func studioKeyNoneValid(t *testing.T, manager *runtimeManager, accounts []*aistudio.Account) {
	t.Helper()
	byID := studioKeyRecordsByID(t, manager)
	for _, account := range accounts {
		if view := byID[account.ID]; view.HasKey || view.Status == studioKeyOK {
			t.Fatalf("%s 不应有有效的 Key: %+v", account.ID, view)
		}
	}
	if export, err := manager.ExportStudioKeys(context.Background()); err != nil || len(export.Keys) != 0 {
		t.Fatalf("导出应为空: err=%v keys=%d", err, len(export.Keys))
	}
}

// TestStudioKeyVoidAfterClear 三个账户都保存着同一条 Key：清除这些作废记录后（记录只留下指纹，列表里不显示），驱动又给每个账户都返回它：
// 一个都不保存（第一个账户也不行：作废过的指纹对上），导出为空；重启（新的进程状态）之后再读到它照样不保存
func TestStudioKeyVoidAfterClear(t *testing.T) {
	manager, admin, accounts := studioKeyTestManager(t, "a@example.com", "b@example.com", "c@example.com")
	shared := studioKeyFile{Key: testStudioKey, FetchedAt: &studioKeyAt, Status: studioKeyOK}
	writeStudioKeyRecords(t, accounts, map[int]studioKeyFile{0: shared, 1: shared, 2: shared})
	ids := []string{accounts[0].ID, accounts[1].ID, accounts[2].ID}
	result, err := manager.ClearStudioKeys(context.Background(), api.StudioKeyClearInput{AccountIDs: ids})
	if err != nil || result.Cleared.Invalid != 3 || len(result.Records) != 0 {
		t.Fatalf("清除 err=%v %+v 列表 %d 条", err, result.Cleared, len(result.Records))
	}
	for _, account := range accounts {
		record, ok, _ := readStudioKeyFile(account.Directory)
		if !ok || !studioKeyTombstone(record) || len(record.VoidKeys) != 1 || record.VoidKeys[0] != studioKeyFingerprint(testStudioKey) {
			t.Fatalf("清除后应只留下指纹: ok=%v key=%v status=%q voids=%d", ok, record.Key != "", record.Status, len(record.VoidKeys))
		}
		data, _ := os.ReadFile(filepath.Join(account.Directory, studioKeyFileName))
		if strings.Contains(string(data), "TESTKEY") {
			t.Fatal("留下的记录里不应有 Key")
		}
	}
	driver := &fakeStudioKeyDriver{result: camoufoxnative.StudioKeyResult{Key: testStudioKey, KeyCount: 1}}
	admin.login = driver
	for _, account := range accounts {
		manager.fetchStudioKey(context.Background(), account.ID, false)
	}
	studioKeyNoneValid(t, manager, accounts)
	first, _, _ := readStudioKeyFile(accounts[0].Directory)
	if first.Key != "" || first.Status != studioKeyFailed || first.Reason != studioKeyDuplicateReason(0) {
		t.Fatalf("第一个账户也不应保存: key=%v status=%s reason=%q", first.Key != "", first.Status, first.Reason)
	}
	// 重启：进程里记下的指纹没了，只靠记录里留下的
	restarted := &runtimeManager{current: manager.current}
	restarted.fetchStudioKey(context.Background(), accounts[1].ID, false)
	studioKeyNoneValid(t, restarted, accounts)
	// 清掉失败记录后（没有 Key、带指纹的只留下指纹）再重启，照样不保存
	if _, err := restarted.ClearStudioKeys(context.Background(), api.StudioKeyClearInput{AccountIDs: ids}); err != nil {
		t.Fatal(err)
	}
	again := &runtimeManager{current: manager.current}
	if status, _ := again.StudioKeys(context.Background()); len(status.Records) != 0 {
		t.Fatalf("只留下指纹的记录不应列出: %+v", status.Records)
	}
	again.fetchStudioKey(context.Background(), accounts[2].ID, false)
	studioKeyNoneValid(t, again, accounts)
	for _, message := range serviceLogMessages(admin.requests) {
		if strings.Contains(message, "TESTKEY") || strings.Contains(message, studioKeyFingerprint(testStudioKey)) {
			t.Fatalf("日志泄露 Key 或指纹: %s", message)
		}
	}
}

// TestStudioKeyVoidAfterRetry 两个账户都保存着同一条 Key：“全部获取”时都失败（Key 去掉，记录里留下指纹），重启之后“重试失败的”时驱动都
// 返回这条 Key：一个都不保存。之后读到别的 Key 时照常保存，记录里的指纹留着
func TestStudioKeyVoidAfterRetry(t *testing.T) {
	manager, admin, accounts := studioKeyTestManager(t, "a@example.com", "b@example.com")
	shared := studioKeyFile{Key: testStudioKey, FetchedAt: &studioKeyAt, Status: studioKeyOK}
	writeStudioKeyRecords(t, accounts, map[int]studioKeyFile{0: shared, 1: shared})
	driver := &fakeStudioKeyDriver{err: &camoufoxnative.StudioKeyError{Reason: camoufoxnative.StudioKeyReasonUnreadable}}
	admin.login = driver
	for _, account := range accounts {
		manager.fetchStudioKey(context.Background(), account.ID, false)
	}
	for _, account := range accounts {
		if record, _, _ := readStudioKeyFile(account.Directory); record.Key != "" || !slices.Contains(record.VoidKeys, studioKeyFingerprint(testStudioKey)) {
			t.Fatalf("去掉的作废 Key 应留下指纹: key=%v voids=%d", record.Key != "", len(record.VoidKeys))
		}
	}
	restarted := &runtimeManager{current: manager.current}
	driver.err, driver.result = nil, camoufoxnative.StudioKeyResult{Key: testStudioKey, KeyCount: 1}
	for _, account := range accounts {
		restarted.fetchStudioKey(context.Background(), account.ID, false)
	}
	studioKeyNoneValid(t, restarted, accounts)
	for _, options := range driver.options {
		if options.SavedKey != "" {
			t.Fatal("作废的 Key 不应交给驱动")
		}
	}
	driver.result = camoufoxnative.StudioKeyResult{Key: otherStudioKey, KeyCount: 1}
	restarted.fetchStudioKey(context.Background(), accounts[0].ID, false)
	record, _, _ := readStudioKeyFile(accounts[0].Directory)
	if record.Key != otherStudioKey || record.Status != studioKeyOK || !slices.Contains(record.VoidKeys, studioKeyFingerprint(testStudioKey)) {
		t.Fatalf("读到别的 Key 时照常保存、指纹留着: ok=%v status=%s voids=%d", record.Key == otherStudioKey, record.Status, len(record.VoidKeys))
	}
	if view := studioKeyRecordsByID(t, restarted)[accounts[0].ID]; !view.HasKey || view.Last4 != "oooo" {
		t.Fatalf("新 Key 有效: %+v", view)
	}
}

// TestStudioKeyVoidedRecordInvalid 某个账户的记录里保存着一条作废过的 Key（只有它保存着、没有 invalid 标记，如从备份恢复的旧记录），
// 另一个账户的记录里留着这条 Key 的指纹：列表里仍作废，导出不含它
func TestStudioKeyVoidedRecordInvalid(t *testing.T) {
	manager, _, accounts := studioKeyTestManager(t, "restored@example.com", "cleared@example.com")
	writeStudioKeyRecords(t, accounts, map[int]studioKeyFile{0: {Key: testStudioKey, FetchedAt: &studioKeyAt, Status: studioKeyOK},
		1: {VoidKeys: []string{studioKeyFingerprint(testStudioKey)}}})
	byID := studioKeyRecordsByID(t, manager)
	if view := byID[accounts[0].ID]; view.Status != studioKeyInvalid || view.HasKey {
		t.Fatalf("作废过的 Key 应作废: %+v", view)
	}
	if _, listed := byID[accounts[1].ID]; listed {
		t.Fatal("只留下指纹的记录不应列出")
	}
	studioKeyNoneValid(t, manager, accounts)
}

// TestStudioKeyVoidOwnInvalid 只有一个账户保存着这条 Key、记录上标着 invalid：重新获取时驱动又返回它，不保存，原因写明是作废的那条
func TestStudioKeyVoidOwnInvalid(t *testing.T) {
	manager, admin, accounts := studioKeyTestManager(t, "single@example.com")
	writeStudioKeyRecords(t, accounts, map[int]studioKeyFile{0: {Key: testStudioKey, Invalid: true, FetchedAt: &studioKeyAt, Status: studioKeyOK}})
	admin.login = &fakeStudioKeyDriver{result: camoufoxnative.StudioKeyResult{Key: testStudioKey, KeyCount: 1}}
	manager.fetchStudioKey(context.Background(), accounts[0].ID, false)
	record, _, _ := readStudioKeyFile(accounts[0].Directory)
	if record.Key != "" || record.Status != studioKeyFailed || record.Reason != studioKeyDuplicateReason(0) {
		t.Fatalf("不应保存作废的那条: key=%v status=%s reason=%q", record.Key != "", record.Status, record.Reason)
	}
	studioKeyNoneValid(t, manager, accounts)
}

// TestStudioKeyDuplicateDuringFetch 上次的 Key 有效（只有这个账户保存着），获取期间别的账户的记录里也出现了同一条（进程外的改动），驱动读回的
// 正是它：不保存，上次的这条同样作废（去掉，只留下指纹），另一个账户的记录写上 invalid 标记
func TestStudioKeyDuplicateDuringFetch(t *testing.T) {
	manager, admin, accounts := studioKeyTestManager(t, "fetch@example.com", "other@example.com")
	writeStudioKeyRecords(t, accounts, map[int]studioKeyFile{0: {Key: testStudioKey, FetchedAt: &studioKeyAt, Status: studioKeyOK}})
	admin.login = &writingStudioKeyDriver{fakeStudioKeyDriver: fakeStudioKeyDriver{result: camoufoxnative.StudioKeyResult{Key: testStudioKey, KeyCount: 1}},
		write: func() {
			writeStudioKeyRecords(t, accounts, map[int]studioKeyFile{1: {Key: testStudioKey, FetchedAt: &studioKeyAt, Status: studioKeyOK}})
		}}
	manager.fetchStudioKey(context.Background(), accounts[0].ID, false)
	record, _, _ := readStudioKeyFile(accounts[0].Directory)
	if record.Key != "" || record.Status != studioKeyFailed || !slices.Contains(record.VoidKeys, studioKeyFingerprint(testStudioKey)) {
		t.Fatalf("上次的这条应作废: key=%v status=%s", record.Key != "", record.Status)
	}
	if other, _, _ := readStudioKeyFile(accounts[1].Directory); !other.Invalid {
		t.Fatal("另一个账户的记录应写上 invalid 标记")
	}
	studioKeyNoneValid(t, manager, accounts)
}

// writingStudioKeyDriver 在获取期间调用 write（模拟进程外改了别的账户的记录）
type writingStudioKeyDriver struct {
	fakeStudioKeyDriver
	write func()
}

func (driver *writingStudioKeyDriver) FetchStudioKey(
	ctx context.Context, request aistudio.IsolatedLoginRequest, state aistudio.StorageState, options camoufoxnative.StudioKeyOptions,
) (camoufoxnative.StudioKeyResult, error) {
	driver.write()
	return driver.fakeStudioKeyDriver.FetchStudioKey(ctx, request, state, options)
}

// TestStudioKeyInvalidSurvivesDelete 两个账户都保存着同一条 Key（旧版本留下的，没有 invalid 标记）：读一次列表就给两条记录写上标记；
// 删掉其中一个账户后，剩下的仍作废（不变回“成功”、reveal 报错、导出为空）
func TestStudioKeyInvalidSurvivesDelete(t *testing.T) {
	manager, admin, accounts := studioKeyTestManager(t, "a@example.com", "b@example.com")
	shared := studioKeyFile{Key: testStudioKey, FetchedAt: &studioKeyAt, Status: studioKeyOK}
	writeStudioKeyRecords(t, accounts, map[int]studioKeyFile{0: shared, 1: shared})
	if view := studioKeyRecordsByID(t, manager)[accounts[0].ID]; view.Status != studioKeyInvalid {
		t.Fatalf("应作废: %+v", view)
	}
	for _, account := range accounts {
		if record, _, _ := readStudioKeyFile(account.Directory); !record.Invalid {
			t.Fatal("读列表时应给两条记录写上 invalid 标记")
		}
	}
	if !hasLogContaining(admin.requests, "同一条 Key 保存在 2 个账户里，记录标为作废", "末四位=kkkk", "标记=2") {
		t.Fatalf("日志 = %v", serviceLogMessages(admin.requests))
	}
	if err := admin.DeleteAccount(context.Background(), accounts[1].ID); err != nil {
		t.Fatal(err)
	}
	if view := studioKeyRecordsByID(t, manager)[accounts[0].ID]; view.Status != studioKeyInvalid || view.HasKey {
		t.Fatalf("删掉另一个账户后仍应作废: %+v", view)
	}
	if _, err := manager.RevealStudioKey(context.Background(), accounts[0].ID); err == nil {
		t.Fatal("reveal 应报错")
	}
	studioKeyNoneValid(t, manager, accounts[:1])
}

// TestStudioKeyListReasonCapped 列表里的失败原因截短（记录里的完整原因不动，写在运行日志里）
func TestStudioKeyListReasonCapped(t *testing.T) {
	long := strings.Repeat("诊", 1000)
	view := studioKeyView("a@example.com", "a@example.com", studioKeyFile{Status: studioKeyFailed, Reason: long}, studioKeyVerdict{})
	if runes := []rune(view.Reason); len(runes) > studioKeyListReasonLimit+20 || !strings.HasSuffix(view.Reason, "（完整原因与诊断见运行日志）") {
		t.Fatalf("列表里的原因应截短: %d 字", len(runes))
	}
	short := studioKeyView("a@example.com", "a@example.com", studioKeyFile{Status: studioKeyFailed, Reason: "超时"}, studioKeyVerdict{})
	if short.Reason != "超时" {
		t.Fatalf("短原因原样: %q", short.Reason)
	}
}

// TestStudioKeyDuplicateRemembered 读到的 Key 与另一个账户保存的相同、没有保存：这个账户的记录里留下它的指纹。删掉另一个账户、重启之后
// 再读到它照样不保存
func TestStudioKeyDuplicateRemembered(t *testing.T) {
	manager, admin, accounts := studioKeyTestManager(t, "fetch@example.com", "holder@example.com")
	writeStudioKeyRecords(t, accounts, map[int]studioKeyFile{1: {Key: testStudioKey, FetchedAt: &studioKeyAt, Status: studioKeyOK}})
	admin.login = &fakeStudioKeyDriver{result: camoufoxnative.StudioKeyResult{Key: testStudioKey, KeyCount: 1}}
	manager.fetchStudioKey(context.Background(), accounts[0].ID, false)
	if record, _, _ := readStudioKeyFile(accounts[0].Directory); record.Key != "" || !slices.Contains(record.VoidKeys, studioKeyFingerprint(testStudioKey)) {
		t.Fatalf("没有保存，记录里应留下指纹: key=%v voids=%d", record.Key != "", len(record.VoidKeys))
	}
	if err := admin.DeleteAccount(context.Background(), accounts[1].ID); err != nil {
		t.Fatal(err)
	}
	restarted := &runtimeManager{current: manager.current}
	restarted.fetchStudioKey(context.Background(), accounts[0].ID, false)
	studioKeyNoneValid(t, restarted, accounts[:1])
}

// TestStudioKeyLegacyUnconfirmed 旧版本（记录格式版本 1）保存的 Key 没有与页面的 Key 列表核对过：只有一个账户保存着它时也作废——列表里是
// invalid（原因写明是旧版本保存的）、reveal 409、不导出，“全部获取”把它当作没有 Key、补项目信息不排；普通获取时不交给驱动当已保存的 Key，
// 也不排除、不留指纹，驱动核对后读回同一条照样保存（新的格式版本）；清除时连同记录删掉，不留指纹
func TestStudioKeyLegacyUnconfirmed(t *testing.T) {
	manager, admin, accounts := studioKeyTestManager(t, "legacy@example.com", "none@example.com", "own@example.com")
	manager.studioKeys.running = true
	writeStudioKeyRecords(t, accounts, map[int]studioKeyFile{
		0: {Version: 1, Key: testStudioKey, ProjectNumber: "123456789012", FetchedAt: &studioKeyAt, Status: studioKeyOK},
		2: {Key: otherStudioKey, FetchedAt: &studioKeyAt, Status: studioKeyOK}})
	if record, _, _ := readStudioKeyFile(accounts[0].Directory); record.Version != 1 || !studioKeyUnconfirmed(record) {
		t.Fatalf("旧版本的记录改写之前应保持版本 1: %d", record.Version)
	}
	byID := studioKeyRecordsByID(t, manager)
	if view := byID[accounts[0].ID]; view.Status != studioKeyInvalid || view.HasKey || view.Last4 != "" || !strings.Contains(view.Reason, "旧版本保存的") {
		t.Fatalf("旧版本的 Key 应作废: %+v", view)
	}
	if view := byID[accounts[2].ID]; view.Status != studioKeyOK || !view.HasKey {
		t.Fatalf("新版本的 Key 照常: %+v", view)
	}
	_, err := manager.RevealStudioKey(context.Background(), accounts[0].ID)
	var operation *adminOperationError
	if !errors.As(err, &operation) || operation.status != 409 || !strings.Contains(operation.message, "旧版本保存的") {
		t.Fatalf("reveal 旧版本的 Key 应 409: %v", err)
	}
	if export, err := manager.ExportStudioKeys(context.Background()); err != nil || len(export.Keys) != 1 || export.Keys[0].AccountID != accounts[2].ID {
		t.Fatalf("导出不应含旧版本的 Key: err=%v keys=%d", err, len(export.Keys))
	}
	queued, err := manager.FetchStudioKeys(context.Background(), api.StudioKeyFetchInput{All: true, MissingOnly: true})
	if err != nil || !slices.Equal(queued.Queue, []string{accounts[0].ID, accounts[1].ID}) {
		t.Fatalf("全部获取应把旧版本的 Key 当作没有 Key: err=%v queue=%v", err, queued.Queue)
	}
	manager.studioKeys.clear()
	if filled, err := manager.FetchStudioKeys(context.Background(), api.StudioKeyFetchInput{All: true, FillProject: true}); err != nil ||
		!slices.Equal(filled.Queue, []string{accounts[2].ID}) {
		t.Fatalf("补项目信息不应排旧版本的 Key: err=%v queue=%v", err, filled.Queue)
	}
	manager.studioKeys.clear()
	driver := &fakeStudioKeyDriver{result: camoufoxnative.StudioKeyResult{Key: testStudioKey, KeyCount: 1}}
	admin.login = driver
	manager.fetchStudioKey(context.Background(), accounts[0].ID, false)
	if len(driver.options) != 1 || driver.options[0].SavedKey != "" || slices.Contains(driver.options[0].Exclude, testStudioKey) ||
		slices.Contains(driver.options[0].ExcludeFingerprints, studioKeyFingerprint(testStudioKey)) || !slices.Contains(driver.options[0].Exclude, otherStudioKey) {
		t.Fatal("旧版本的 Key 不应交给驱动当已保存的 Key，也不应排除；其他账户的 Key 要排除")
	}
	if record, _, _ := readStudioKeyFile(accounts[0].Directory); record.Key != testStudioKey || record.Version != studioKeyVersion || len(record.VoidKeys) != 0 {
		t.Fatalf("核对后读回同一条应照常保存: key=%v version=%d voids=%d", record.Key == testStudioKey, record.Version, len(record.VoidKeys))
	}
	if view := studioKeyRecordsByID(t, manager)[accounts[0].ID]; view.Status != studioKeyOK || !view.HasKey {
		t.Fatalf("核对过的 Key 应有效: %+v", view)
	}
	// 清除旧版本的记录：连同 Key 删掉，不留指纹（它也可能确实是这个账户的）
	writeStudioKeyRecords(t, accounts, map[int]studioKeyFile{1: {Version: 1, Key: "AQ.TESTKEY" + strings.Repeat("l", 24), Status: studioKeyOK}})
	result, err := manager.ClearStudioKeys(context.Background(), api.StudioKeyClearInput{AccountIDs: []string{accounts[1].ID}})
	if err != nil || result.Cleared != (api.StudioKeyClearCounts{Invalid: 1}) {
		t.Fatalf("清除旧版本的记录: err=%v %+v", err, result.Cleared)
	}
	if _, ok, _ := readStudioKeyFile(accounts[1].Directory); ok || manager.studioKeys.voidedKey("AQ.TESTKEY"+strings.Repeat("l", 24)) {
		t.Fatal("旧版本的记录应删掉文件、不留指纹")
	}
}

// TestStudioKeyClientFingerprints 驱动认出的页面公共 Key（指纹随结果或错误交回）：只有一个账户保存着它（旧版本只给它记过）也作废——
// 那条记录写上 invalid 标记，列表里是 invalid、不导出、“全部获取”重新获取；这次获取的账户的记录里留下指纹，重启之后仍认得；
// 驱动失败时交回的指纹同样处理；已保存的 Key 就是它时不再保留
func TestStudioKeyClientFingerprints(t *testing.T) {
	manager, admin, accounts := studioKeyTestManager(t, "holder@example.com", "fetch@example.com", "failed@example.com", "later@example.com")
	manager.studioKeys.running = true
	clientKey := "AQ.TESTKEY" + strings.Repeat("c", 24)
	writeStudioKeyRecords(t, accounts, map[int]studioKeyFile{0: {Key: clientKey, FetchedAt: &studioKeyAt, Status: studioKeyOK},
		2: {Key: otherStudioKey, FetchedAt: &studioKeyAt, Status: studioKeyOK}})
	if view := studioKeyRecordsByID(t, manager)[accounts[0].ID]; view.Status != studioKeyOK {
		t.Fatalf("按个数认不出: %+v", view)
	}
	fingerprints := []string{camoufoxnative.StudioKeyFingerprint(clientKey)}
	admin.login = &fakeStudioKeyDriver{result: camoufoxnative.StudioKeyResult{Key: testStudioKey, KeyCount: 1, ClientFingerprints: fingerprints}}
	manager.fetchStudioKey(context.Background(), accounts[1].ID, false)
	if holder, _, _ := readStudioKeyFile(accounts[0].Directory); holder.Key != clientKey || !holder.Invalid {
		t.Fatal("保存着公共 Key 的记录应写上 invalid 标记")
	}
	if fetched, _, _ := readStudioKeyFile(accounts[1].Directory); fetched.Key != testStudioKey || !slices.Equal(fetched.VoidKeys, fingerprints) {
		t.Fatalf("这次获取的账户应保存新 Key，并留下公共 Key 的指纹: voids=%d", len(fetched.VoidKeys))
	}
	byID := studioKeyRecordsByID(t, manager)
	if view := byID[accounts[0].ID]; view.Status != studioKeyInvalid || view.HasKey || !strings.Contains(view.Reason, "浏览器认出它是页面自己的公共 Key") {
		t.Fatalf("只有一个账户保存着的公共 Key 也应作废: %+v", view)
	}
	if export, _ := manager.ExportStudioKeys(context.Background()); len(export.Keys) != 2 || slices.ContainsFunc(export.Keys, func(secret api.StudioKeySecret) bool {
		return secret.Key == clientKey
	}) {
		t.Fatalf("导出不应含公共 Key: %d 条", len(export.Keys))
	}
	queued, err := manager.FetchStudioKeys(context.Background(), api.StudioKeyFetchInput{All: true, MissingOnly: true})
	if err != nil || !slices.Equal(queued.Queue, []string{accounts[0].ID, accounts[3].ID}) {
		t.Fatalf("全部获取应重新获取它: err=%v queue=%v", err, queued.Queue)
	}
	manager.studioKeys.clear()
	// 驱动失败时交回的指纹：已保存的 Key 就是它时不再保留，记录里留下指纹；重启之后（只靠记录里的指纹）别的账户再存进它也作废
	failedKey := otherStudioKey
	admin.login = &fakeStudioKeyDriver{err: &camoufoxnative.StudioKeyError{Reason: camoufoxnative.StudioKeyReasonTimeout,
		ClientFingerprints: []string{camoufoxnative.StudioKeyFingerprint(failedKey)}}}
	manager.fetchStudioKey(context.Background(), accounts[2].ID, false)
	if record, _, _ := readStudioKeyFile(accounts[2].Directory); record.Key != "" || record.Status != studioKeyFailed ||
		!slices.Contains(record.VoidKeys, camoufoxnative.StudioKeyFingerprint(failedKey)) {
		t.Fatalf("已保存的 Key 是公共 Key 时不应保留: key=%v status=%s", record.Key != "", record.Status)
	}
	writeStudioKeyRecords(t, accounts, map[int]studioKeyFile{3: {Key: failedKey, FetchedAt: &studioKeyAt, Status: studioKeyOK}})
	restarted := &runtimeManager{current: manager.current}
	if view := studioKeyRecordsByID(t, restarted)[accounts[3].ID]; view.Status != studioKeyInvalid || view.HasKey {
		t.Fatalf("重启之后仍应认得公共 Key 的指纹: %+v", view)
	}
	for _, message := range serviceLogMessages(admin.requests) {
		if strings.Contains(message, "TESTKEY") || strings.Contains(message, fingerprints[0]) {
			t.Fatalf("日志泄露 Key 或指纹: %s", message)
		}
	}
}

// TestStudioKeyExcludeOptions 获取时交给驱动排除的 Key：其他账户保存着的（不论有效与否），以及这个账户确定作废的 Key（连同全部作废指纹）；
// 这个账户确定作废的 Key 不交给驱动当已保存的 Key
func TestStudioKeyExcludeOptions(t *testing.T) {
	manager, admin, accounts := studioKeyTestManager(t, "fetch@example.com", "holder@example.com", "voided@example.com")
	voidedKey := "AQ.TESTKEY" + strings.Repeat("v", 24)
	writeStudioKeyRecords(t, accounts, map[int]studioKeyFile{0: {Key: voidedKey, Invalid: true, FetchedAt: &studioKeyAt, Status: studioKeyOK},
		1: {Key: otherStudioKey, FetchedAt: &studioKeyAt, Status: studioKeyOK}, 2: {VoidKeys: []string{studioKeyFingerprint(testStudioKey)}}})
	driver := &fakeStudioKeyDriver{err: &camoufoxnative.StudioKeyError{Reason: camoufoxnative.StudioKeyReasonExcluded}}
	admin.login = driver
	manager.fetchStudioKey(context.Background(), accounts[0].ID, false)
	if len(driver.options) != 1 {
		t.Fatal("应打开一次浏览器")
	}
	options := driver.options[0]
	if options.SavedKey != "" || !slices.Contains(options.Exclude, otherStudioKey) || !slices.Contains(options.Exclude, voidedKey) ||
		!slices.Contains(options.ExcludeFingerprints, studioKeyFingerprint(voidedKey)) || !slices.Contains(options.ExcludeFingerprints, studioKeyFingerprint(testStudioKey)) {
		t.Fatalf("排除的 Key: saved=%v exclude=%d fingerprints=%d", options.SavedKey != "", len(options.Exclude), len(options.ExcludeFingerprints))
	}
	if record, _, _ := readStudioKeyFile(accounts[0].Directory); record.Key != "" || record.Reason != camoufoxnative.StudioKeyReasonExcluded {
		t.Fatalf("失败时记下原因: key=%v reason=%q", record.Key != "", record.Reason)
	}
}
