package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/api"
)

// 手动停用记录的回归测试：账户默认启用后，只有手动停用记录让停用账户保持停用，这条记录不能被自动处理改掉或丢掉

// manualDisableInput 返回把账户改为启用或停用的管理接口输入
func manualDisableInput(accountID string, enabled bool) api.AccountInput {
	return api.AccountInput{
		Label: accountID, Enabled: enabled, Locale: aistudio.DefaultAccountLocale(), Timezone: aistudio.DefaultAccountTimezone(),
	}
}

// readDisableNotesFile 读取并解析 .disable-reasons.json
func readDisableNotesFile(t *testing.T, directory string) map[string]disableNote {
	t.Helper()
	content, err := os.ReadFile(filepath.Join(directory, disableNotesFileName))
	if err != nil {
		t.Fatal(err)
	}
	var notes map[string]disableNote
	if err := json.Unmarshal(content, &notes); err != nil {
		t.Fatalf("停用记录文件应可解析：%v | %s", err, content)
	}
	return notes
}

// settleAccountFiles 把账户目录与文件的修改时间提前，目录扫描视为已经复制完成
func settleAccountFiles(t *testing.T, directories ...string) {
	t.Helper()
	settled := time.Now().Add(-time.Minute)
	for _, directory := range directories {
		for _, path := range []string{directory, filepath.Join(directory, "account.json"), aistudio.StorageStatePath(directory)} {
			if err := os.Chtimes(path, settled, settled); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// newManualTestWatcher 创建测试用目录扫描器
func newManualTestWatcher(admin *runtimeAdmin) *accountWatcher {
	return &accountWatcher{
		admin: admin, failures: map[string]string{}, missing: map[string]int{},
		authFiles: map[string]authFileStat{}, loginPending: map[string]struct{}{},
	}
}

// TestOnboardingKeepsAccountDisabledWhileQueued 账户导入时启用、排队等待自动处理期间被手动停用：处理时保持停用、
// 不启动浏览器验证，手动停用记录保留。原先验证通过后被重新启用，手动停用记录也被清除，重启后同样不再保持停用
func TestOnboardingKeepsAccountDisabledWhileQueued(t *testing.T) {
	notes := isolateDisableNotes(t)
	admin, _, accounts := enableTestAdmin(t, "a@example.com")
	login := &verifyTestLogin{verification: aistudio.LoginVerification{Authenticated: true}}
	admin.login = login
	onboarder := newTestOnboarder(admin, t.TempDir())
	t.Cleanup(onboarder.flush)
	admin.onboard = onboarder
	accountID := accounts[0].ID
	onboarder.enqueue([]string{accountID})
	if _, err := admin.UpdateAccount(context.Background(), accountID, manualDisableInput(accountID, false)); err != nil {
		t.Fatal(err)
	}

	batch, policy := onboarder.takeBatch(time.Now().Add(onboardingMaxWait))
	if len(batch) != 1 || batch[0] != accountID {
		t.Fatalf("本批 = %v，期望只有排队的账户", batch)
	}
	result, detail, retry := onboarder.process(context.Background(), accountID, policy)
	if final := onboarder.finish(accountID, result, detail, retry); final != onboardSkipped || detail != onboardManuallyDisabled {
		t.Fatalf("结果 = %s %q，期望跳过：%s", final, detail, onboardManuallyDisabled)
	}
	if status := testStatus(t, admin.pool, accountID); status.Enabled || diskEnabled(t, accounts[0].Directory) {
		t.Fatal("排队期间手动停用的账户不应被自动处理启用")
	}
	if !notes.manual(accountID) {
		t.Fatal("手动停用记录应保留")
	}
	if login.calls.Load() != 0 {
		t.Fatal("手动停用的账户不应启动浏览器验证")
	}
}

// TestOnboardingKeepsAccountDisabledDuringVerification 处理开始时是启用的、验证期间被手动停用：验证通过后不启用
func TestOnboardingKeepsAccountDisabledDuringVerification(t *testing.T) {
	notes := isolateDisableNotes(t)
	admin, catalog, accounts := enableTestAdmin(t, "a@example.com")
	accountID := accounts[0].ID
	// 验证结束、释放租约后同步模型目录：等手动停用完成才返回，确保处理再看账户状态时已经停用
	catalog.gate = make(chan struct{})
	disabled := make(chan error, 1)
	login := &verifyTestLogin{verification: aistudio.LoginVerification{Authenticated: true}}
	login.onVerify = func() {
		go func() {
			defer close(catalog.gate)
			_, err := admin.UpdateAccount(context.Background(), accountID, manualDisableInput(accountID, false))
			disabled <- err
		}()
	}
	admin.login = login
	onboarder := newTestOnboarder(admin, t.TempDir())
	admin.onboard = onboarder

	result, detail, _ := onboarder.process(context.Background(), accountID, defaultOnboardingPolicy())
	if err := <-disabled; err != nil {
		t.Fatal(err)
	}
	if result != onboardSkipped || detail != onboardManuallyDisabled {
		t.Fatalf("结果 = %s %q，期望跳过", result, detail)
	}
	if testStatus(t, admin.pool, accountID).Enabled || !notes.manual(accountID) {
		t.Fatal("验证期间手动停用的账户应保持停用并保留手动停用记录")
	}
}

// TestAutomaticEnableRechecksManualNoteUnderLease 自动启用在账户租约内再看一次停用记录：新账户自动处理与认证文件更新后的
// 按默认启用都不启用手动停用的账户、不清除记录；管理员的启用不受影响
func TestAutomaticEnableRechecksManualNoteUnderLease(t *testing.T) {
	notes := isolateDisableNotes(t)
	admin, _, accounts := enableTestAdmin(t, "a@example.com", "b@example.com")
	for _, account := range accounts {
		disableTestAccount(t, admin.pool, account.ID)
		notes.set(account.ID, "通过管理页或管理接口停用", false)
	}
	first, second := accounts[0].ID, accounts[1].ID

	_, err := admin.updateAccount(context.Background(), first, manualDisableInput(first, true), &automaticEnable{})
	if !errors.Is(err, errAccountManuallyDisabled) {
		t.Fatalf("新账户自动处理启用手动停用的账户应被拒绝，错误 = %v", err)
	}
	if testStatus(t, admin.pool, first).Enabled || !notes.manual(first) {
		t.Fatal("新账户自动处理不应启用手动停用的账户或清除记录")
	}
	if !admin.enableAfterLoginUpdate(context.Background(), second) {
		t.Fatal("手动停用的账户应直接完成，不留到下一轮")
	}
	if testStatus(t, admin.pool, second).Enabled || diskEnabled(t, accounts[1].Directory) || !notes.manual(second) {
		t.Fatal("认证文件更新后不应启用手动停用的账户或清除记录")
	}
	if logCount(admin.requests, "账户是手动停用的，保持停用 | 账户="+second) != 1 {
		t.Fatal("缺少保持停用的日志")
	}

	if _, err := admin.UpdateAccount(context.Background(), first, manualDisableInput(first, true)); err != nil {
		t.Fatal(err)
	}
	if !testStatus(t, admin.pool, first).Enabled || notes.manual(first) {
		t.Fatal("管理员启用应启用账户并清除手动停用记录")
	}
	// 启用后在后台同步模型目录（写入账户目录），等它结束再删除临时目录
	waitFor(t, "启用后的模型目录同步", func() bool {
		return logCount(admin.requests, "启用后的模型目录同步完成") == 1
	})
}

// TestOnboardingFinishKeepsManualNote 处理结束记录结果时不改动手动停用记录：启用后、记录结果前刚被手动停用的账户记录保留；
// 验证未通过的结果不把手动停用改记为自动停用（原先改记后重启或认证文件更新时被按默认启用）
func TestOnboardingFinishKeepsManualNote(t *testing.T) {
	notes := isolateDisableNotes(t)
	onboarder := newTestOnboarder(nil, t.TempDir())
	t.Cleanup(onboarder.flush)
	for _, test := range []struct{ id, result string }{
		{"enabled@example.com", onboardEnabled},
		{"failed@example.com", onboardVerifyFailed},
		{"error@example.com", onboardError},
	} {
		notes.set(test.id, "通过管理页或管理接口停用", false)
		onboarder.processing[test.id] = &onboardPending{queuedAt: time.Now()}
		if final := onboarder.finish(test.id, test.result, "原因", false); final != test.result {
			t.Fatalf("%s：最终结果 = %q", test.id, final)
		}
		if !notes.manual(test.id) {
			t.Fatalf("%s：结果为 %s 时手动停用记录应保留", test.id, test.result)
		}
	}
	notes.set("auto@example.com", "新账户自动处理验证未通过", true)
	notes.set("auto@example.com", "新账户自动处理验证出错", true)
	if !notes.automatic("auto@example.com") {
		t.Fatal("自动停用记录仍应可以更新")
	}
}

// TestWatcherImportSkipsManuallyDisabledOnboarding 导入时保持停用的手动停用账户直接记为跳过，不进入新账户自动处理队列；
// 原先与其他新账户一起排队，验证通过后被启用
func TestWatcherImportSkipsManuallyDisabledOnboarding(t *testing.T) {
	notes := isolateDisableNotes(t)
	admin, _, _ := enableTestAdmin(t, "existing@example.com")
	onboarder := newTestOnboarder(admin, t.TempDir())
	admin.onboard = onboarder
	root := filepath.Dir(admin.pool.LoginSummaries()[0].Directory)
	plain := writeEnableTestAccount(t, root, "plain@example.com", false)
	manual := writeEnableTestAccount(t, root, "manual@example.com", false)
	notes.set("manual@example.com", "通过管理页或管理接口停用", false)
	settleAccountFiles(t, plain, manual)

	newManualTestWatcher(admin).scan(admin.lifecycle)

	onboarder.mu.Lock()
	_, plainQueued := onboarder.pending["plain@example.com"]
	_, manualQueued := onboarder.pending["manual@example.com"]
	record := onboarder.records["manual@example.com"]
	onboarder.mu.Unlock()
	if !plainQueued {
		t.Fatal("导入后启用的账户应进入自动处理队列")
	}
	if manualQueued || record.Result != onboardSkipped || record.Detail != onboardManuallyDisabled {
		t.Fatalf("手动停用的账户不应进入队列，应记为跳过：排队=%t 记录=%+v", manualQueued, record)
	}
	if testStatus(t, admin.pool, "manual@example.com").Enabled || !notes.manual("manual@example.com") {
		t.Fatal("手动停用的账户导入后应保持停用并保留记录")
	}
	// 启用的导入账户在后台同步模型目录（写入账户目录），等它结束再删除临时目录
	waitFor(t, "导入账户的模型目录同步", func() bool {
		return len(testStatus(t, admin.pool, "plain@example.com").Models) > 0
	})
}

// TestQueueDisabledHonorsLaterManualDisable “处理现有停用账户”是管理员的明确操作（确认框写明手动停用的也会启用）：
// 加入之前手动停用的账户验证通过后启用（已在队列中的同样按这次的要求处理）；加入之后才手动停用的保持停用
func TestQueueDisabledHonorsLaterManualDisable(t *testing.T) {
	notes := isolateDisableNotes(t)
	admin, _, accounts := enableTestAdmin(t, "earlier@example.com", "later@example.com")
	admin.login = &verifyTestLogin{verification: aistudio.LoginVerification{Authenticated: true}}
	onboarder := newTestOnboarder(admin, t.TempDir())
	t.Cleanup(onboarder.flush)
	admin.onboard = onboarder
	earlier, later := accounts[0].ID, accounts[1].ID
	for _, accountID := range []string{earlier, later} {
		if _, err := admin.UpdateAccount(context.Background(), accountID, manualDisableInput(accountID, false)); err != nil {
			t.Fatal(err)
		}
	}
	// earlier 已在队列中（例如重启后没有处理记录、重新排队）
	onboarder.enqueue([]string{earlier})
	time.Sleep(time.Millisecond)
	if added := onboarder.queueDisabled(); added != 1 {
		t.Fatalf("新加入 %d 个，期望 1 个", added)
	}
	time.Sleep(time.Millisecond)
	if _, err := admin.UpdateAccount(context.Background(), later, manualDisableInput(later, false)); err != nil {
		t.Fatal(err)
	}

	batch, policy := onboarder.takeBatch(time.Now())
	if len(batch) != 2 {
		t.Fatalf("本批 = %v，期望两个账户", batch)
	}
	results := make(map[string]string)
	for _, accountID := range batch {
		result, detail, retry := onboarder.process(context.Background(), accountID, policy)
		results[accountID] = onboarder.finish(accountID, result, detail, retry)
	}
	if results[earlier] != onboardEnabled || !testStatus(t, admin.pool, earlier).Enabled || notes.manual(earlier) {
		t.Fatalf("加入之前手动停用的账户应启用并清除记录：结果=%s", results[earlier])
	}
	if results[later] != onboardSkipped || testStatus(t, admin.pool, later).Enabled || !notes.manual(later) {
		t.Fatalf("加入之后再次手动停用的账户应保持停用：结果=%s", results[later])
	}
}

// TestQueueDisabledSurvivesRestart “处理现有停用账户”的要求随处理记录保存：队列没处理完就重启（只留下磁盘上的停用记录与处理记录），
// 重新排队的账户仍按管理员当时的要求处理，加入之前手动停用的验证通过后启用；加入之后再次手动停用的仍保持停用
// （再次停用只更新记录的时间，这次的时间决定它保持停用，返回前已写入文件）。原先这个要求只在内存中，重启后手动停用的全部记为跳过
func TestQueueDisabledSurvivesRestart(t *testing.T) {
	notes := isolateDisableNotes(t)
	admin, _, accounts := enableTestAdmin(t, "earlier@example.com", "later@example.com")
	admin.login = &verifyTestLogin{verification: aistudio.LoginVerification{Authenticated: true}}
	notesDirectory, onboardDirectory := t.TempDir(), t.TempDir()
	notes.load(notesDirectory, admin.requests)
	// 等待中的定时保存到时已没有变化，不会在临时目录删除后再写
	t.Cleanup(notes.flush)
	onboarder := newTestOnboarder(admin, onboardDirectory)
	admin.onboard = onboarder
	earlier, later := accounts[0].ID, accounts[1].ID
	for _, accountID := range []string{earlier, later} {
		if _, err := admin.UpdateAccount(context.Background(), accountID, manualDisableInput(accountID, false)); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(time.Millisecond)
	if added := onboarder.queueDisabled(); added != 2 {
		t.Fatalf("加入 %d 个，期望 2 个", added)
	}
	time.Sleep(time.Millisecond)
	if _, err := admin.UpdateAccount(context.Background(), later, manualDisableInput(later, false)); err != nil {
		t.Fatal(err)
	}

	// 重启：停用记录与处理队列都从磁盘重新载入，内存中没写出的变化丢失
	restartedNotes := &disableNotes{notes: make(map[string]disableNote)}
	accountDisableNotes = restartedNotes
	restartedNotes.load(notesDirectory, admin.requests)
	t.Cleanup(restartedNotes.flush)
	restarted := newAccountOnboarder(admin, onboardDirectory)
	t.Cleanup(restarted.flush)
	admin.onboard = restarted
	batch, policy := restarted.takeBatch(time.Now().Add(onboardingMaxWait))
	if len(batch) != 2 {
		t.Fatalf("本批 = %v，期望两个账户", batch)
	}
	results := make(map[string]string)
	for _, accountID := range batch {
		result, detail, retry := restarted.process(context.Background(), accountID, policy)
		results[accountID] = restarted.finish(accountID, result, detail, retry)
	}
	if results[earlier] != onboardEnabled || !testStatus(t, admin.pool, earlier).Enabled || restartedNotes.manual(earlier) {
		t.Fatalf("加入之前手动停用的账户重启后仍应启用并清除记录：结果=%s", results[earlier])
	}
	if results[later] != onboardSkipped || testStatus(t, admin.pool, later).Enabled || !restartedNotes.manual(later) {
		t.Fatalf("加入之后再次手动停用的账户重启后仍应保持停用：结果=%s", results[later])
	}
	if logCount(admin.requests, "其中 2 个是“处理现有停用账户”加入、尚未处理完的") != 1 {
		t.Fatal("缺少重新加入队列的日志")
	}
}

// TestDisableNotesManualWrittenImmediately 手动停用记录在管理接口返回前已经写入文件；原先合并 3 秒后才写，
// 这期间进程退出就丢了记录，account.json 已是停用，重启后按默认启用
func TestDisableNotesManualWrittenImmediately(t *testing.T) {
	notes := isolateDisableNotes(t)
	admin, _, accounts := enableTestAdmin(t, "a@example.com")
	directory := t.TempDir()
	notes.load(directory, admin.requests)
	accountID := accounts[0].ID

	if _, err := admin.UpdateAccount(context.Background(), accountID, manualDisableInput(accountID, false)); err != nil {
		t.Fatal(err)
	}
	if note, exists := readDisableNotesFile(t, directory)[accountID]; !exists || note.Auto {
		t.Fatal("管理接口返回时手动停用记录应已写入文件")
	}
}

// TestDisableNotesUnreadableKeepsDisabled 停用记录文件存在却无法解析：记 WARN，本次运行没有记录的停用账户不按默认启用
// （启动与导入、认证文件更新后、新账户自动处理），补记为手动停用后再改写文件，原文件另存一份。
// 原先静默当作没有记录，手动停用的账户全部被启用
func TestDisableNotesUnreadableKeepsDisabled(t *testing.T) {
	notes := isolateDisableNotes(t)
	root := t.TempDir()
	plain := writeEnableTestAccount(t, root, "plain@example.com", false)
	corrupt := []byte(`{"plain@example.com": {"reason": "通过管理页`)
	if err := os.WriteFile(filepath.Join(root, disableNotesFileName), corrupt, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	requests := newRequestRegistry(ctx)
	notes.load(root, requests)
	if logCount(requests, "停用记录文件无法读取") != 1 {
		t.Fatal("停用记录文件无法读取时应记一条 WARN")
	}
	loaded, failures := aistudio.NewAccountStore(root).LoadTolerant()
	if len(failures) > 0 {
		t.Fatal(failures)
	}

	if enabled := enableUnmarkedAccounts(loaded, requests, "启动时"); len(enabled) != 0 {
		t.Fatalf("停用记录文件无法读取时不应启用停用账户：%v", enabled)
	}
	if loaded[0].Config.Enabled || diskEnabled(t, plain) {
		t.Fatal("没有停用记录的停用账户应保持停用")
	}
	if note, exists := readDisableNotesFile(t, root)["plain@example.com"]; !exists || note.Auto {
		t.Fatal("应补记手动停用并写入文件，之后改写文件、重启也不会把它启用")
	}
	backups, err := filepath.Glob(filepath.Join(root, disableNotesFileName+".unreadable-*"))
	if err != nil || len(backups) != 1 {
		t.Fatalf("改写前应另存原文件：%v %v", backups, err)
	}
	if content, err := os.ReadFile(backups[0]); err != nil || string(content) != string(corrupt) {
		t.Fatalf("另存的应是原文件内容：%s %v", content, err)
	}

	// 已在账户池中、没有记录的停用账户：认证文件更新后与新账户自动处理都不启用
	admin, _, accounts := enableTestAdmin(t, "pooled@example.com", "queued@example.com")
	pooled, queued := accounts[0].ID, accounts[1].ID
	for _, account := range accounts {
		disableTestAccount(t, admin.pool, account.ID)
	}
	if !admin.enableAfterLoginUpdate(context.Background(), pooled) || testStatus(t, admin.pool, pooled).Enabled {
		t.Fatal("停用记录文件无法读取时认证文件更新后不应启用停用账户")
	}
	admin.login = &verifyTestLogin{verification: aistudio.LoginVerification{Authenticated: true}}
	onboarder := newTestOnboarder(admin, t.TempDir())
	admin.onboard = onboarder
	if result, _, _ := onboarder.process(context.Background(), queued, defaultOnboardingPolicy()); result != onboardSkipped ||
		testStatus(t, admin.pool, queued).Enabled {
		t.Fatalf("停用记录文件无法读取时新账户自动处理不应启用停用账户：结果=%s", result)
	}
}

// TestDisableNotesWriteFailureLogged 停用记录写入失败时记 WARN 并保留待保存的变化，下一次保存再写；
// 原先忽略错误并清掉待保存标记，变化悄悄丢失
func TestDisableNotesWriteFailureLogged(t *testing.T) {
	directory := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	requests := newRequestRegistry(ctx)
	store := &disableNotes{notes: make(map[string]disableNote)}
	store.load(directory, requests)
	// 让文件所在的目录变成普通文件，写入必然失败
	blocked := filepath.Join(directory, "blocked")
	if err := os.WriteFile(blocked, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	store.path = filepath.Join(blocked, disableNotesFileName)
	store.mu.Unlock()

	store.set("a@example.com", "通过管理页或管理接口停用", false)
	if logCount(requests, "停用记录保存失败") != 1 {
		t.Fatal("写入失败应记一条 WARN")
	}
	if err := os.Remove(blocked); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(blocked, 0o700); err != nil {
		t.Fatal(err)
	}
	store.flush()
	if note, exists := readDisableNotesFile(t, blocked)["a@example.com"]; !exists || note.Auto {
		t.Fatal("写入失败的变化应在下一次保存时写入")
	}
}

// TestDetachAccountKeepsManualNote 目录消失后移出账户池时只清除自动停用记录：手动停用的账户目录放回后仍保持停用。
// 原先一并清除，整体替换账户目录后手动停用的账户被按默认启用
func TestDetachAccountKeepsManualNote(t *testing.T) {
	notes := isolateDisableNotes(t)
	admin, _, accounts := enableTestAdmin(t, "existing@example.com", "manual@example.com", "auto@example.com")
	manual, auto := accounts[1], accounts[2]
	for _, account := range []*aistudio.Account{manual, auto} {
		disableTestAccount(t, admin.pool, account.ID)
	}
	notes.set(manual.ID, "通过管理页或管理接口停用", false)
	notes.set(auto.ID, "新账户自动处理验证未通过", true)
	for _, account := range []*aistudio.Account{manual, auto} {
		if err := admin.detachAccount(account.ID); err != nil {
			t.Fatal(err)
		}
	}
	if !notes.manual(manual.ID) {
		t.Fatal("移出账户池时应保留手动停用记录")
	}
	if notes.automatic(auto.ID) {
		t.Fatal("移出账户池时应清除自动停用记录")
	}

	// 目录放回（文件一直在）：重新导入时手动停用的保持停用，自动停用的按默认启用
	settleAccountFiles(t, manual.Directory, auto.Directory)
	newManualTestWatcher(admin).scan(admin.lifecycle)
	if testStatus(t, admin.pool, manual.ID).Enabled || diskEnabled(t, manual.Directory) {
		t.Fatal("手动停用的账户目录放回后应保持停用")
	}
	if !testStatus(t, admin.pool, auto.ID).Enabled {
		t.Fatal("自动停用的账户目录放回后应按默认启用")
	}
	waitFor(t, "导入账户的模型目录同步", func() bool {
		return len(testStatus(t, admin.pool, auto.ID).Models) > 0
	})
}

// TestUpdateAccountRecordsManualForAlreadyDisabled 已经停用的账户再次停用（批量停用、改代理后仍保存为停用）也记为手动停用；
// 原先只在启用改为停用时记录，自动停用或没有记录的账户重启后又被按默认启用
func TestUpdateAccountRecordsManualForAlreadyDisabled(t *testing.T) {
	notes := isolateDisableNotes(t)
	admin, _, accounts := enableTestAdmin(t, "auto@example.com", "plain@example.com")
	for _, account := range accounts {
		disableTestAccount(t, admin.pool, account.ID)
	}
	notes.set(accounts[0].ID, "新账户自动处理验证未通过", true)
	for _, account := range accounts {
		if _, err := admin.UpdateAccount(context.Background(), account.ID, manualDisableInput(account.ID, false)); err != nil {
			t.Fatal(err)
		}
		if !notes.manual(account.ID) {
			t.Fatalf("%s：再次停用后应记为手动停用", account.ID)
		}
		if message := notes.message(account.ID); !strings.Contains(message, "通过管理页或管理接口停用") {
			t.Fatalf("%s：停用原因 = %q", account.ID, message)
		}
	}
}
