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
	if !hasLogContaining(admin.requests, "清除记录 | 删除=1 | 清掉失败信息=1 | 正在处理没动=1 | 未知账户=1") {
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
	if view := studioKeyView(accounts[0].ID, "", record); !view.HasKey || view.Status != studioKeyFailed {
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
