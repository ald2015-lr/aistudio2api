package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// 重新上传认证文件第 1 轮复查的回归测试：原样重新上传排队期间又上传了新文件、共用账户目录回写较早的轮换 Cookie、Chrome 重新导入
// 自动停用的账户、验证开始前后的重新上传与重启、保留修改时间的上传、就绪账户登录签名没变的重新导出、403 路径不等队列的锁

// sharedDirPool 返回同一账户目录上的另一个账户池（共用账户目录的另一个进程）
func sharedDirPool(t *testing.T, pool *aistudio.AccountPool, accountID string) *aistudio.AccountPool {
	t.Helper()
	account, err := pool.Account(accountID)
	if err != nil {
		t.Fatal(err)
	}
	accounts, failures := aistudio.NewAccountStore(filepath.Dir(account.Directory)).LoadTolerant()
	if len(failures) > 0 {
		t.Fatal(failures)
	}
	other := aistudio.NewAccountPool(accounts, 1)
	t.Cleanup(other.WaitBackground)
	return other
}

// mergeRotating 在账户池 pool 里合并一次带 Expires 的 SIDCC（本服务收到一次轮换），flush 为真时立即写盘
func mergeRotating(t *testing.T, pool *aistudio.AccountPool, accountID string, value string, expires time.Time, flush bool) {
	t.Helper()
	lease, err := pool.AcquireAccount(context.Background(), accountID)
	if err != nil {
		t.Fatal(err)
	}
	if err := lease.MergeSetCookieHeaders(
		[]string{"SIDCC=" + value + "; Domain=.google.com; Path=/; Secure; Expires=" + expires.UTC().Format(time.RFC1123)},
		"https://aistudio.google.com/", time.Now(),
	); err != nil {
		t.Fatal(errors.Join(err, lease.Release()))
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
	if flush {
		if err := pool.FlushDeferredStorage(accountID); err != nil {
			t.Fatal(err)
		}
	}
}

// diskDigest 返回账户认证文件原始内容的指纹
func diskDigest(t *testing.T, pool *aistudio.AccountPool, accountID string) aistudio.StorageDigest {
	t.Helper()
	account, err := pool.Account(accountID)
	if err != nil {
		t.Fatal(err)
	}
	_, digest, err := aistudio.LoadStorageStateDigest(account.StoragePath)
	if err != nil {
		t.Fatal(err)
	}
	return digest
}

// TestReauthSameUploadThenNewUploadNotOverwritten 失效账户原样重新上传已判为相同、排在后台载入队列里（批量时可等几十分钟），这期间
// 用户又上传了一份新登录的文件：载入时发现磁盘上已不是检测时那一份，不把内存里的旧登录写回覆盖它（原先覆盖后扫描只看到本服务
// 自己写的文件，新文件再也不会载入），之后的扫描载入新文件并加入验证。需要登录（完整重新载入）与 403 冷却（刷新登录）两条路径都覆盖
func TestReauthSameUploadThenNewUploadNotOverwritten(t *testing.T) {
	const required, denied = "required@example.com", "denied@example.com"
	admin, onboarder, _ := reauthDeniedAdmin(t, []string{denied}, required)
	pool := admin.pool
	markReauthRequired(t, pool, required)
	watcher := newTestAccountWatcher(admin)
	later := time.Now().Add(time.Minute)
	scanLoginAt(t, watcher, later)
	fresh := map[string]aistudio.StorageState{required: authTestState(t, "NEW-REQUIRED", false), denied: authTestState(t, "NEW-DENIED", false)}
	for id := range fresh {
		rewriteAuthFile(t, pool, id)
	}
	summaries := pool.LoginSummaries()
	known := make(map[string]aistudio.AccountLoginSummary, len(summaries))
	for _, summary := range summaries {
		known[summary.ID] = summary
	}
	watcher.detectLoginUpdates(summaries, later.Add(time.Minute))
	watcher.loginMu.Lock()
	sameCount := len(watcher.loginSame)
	watcher.loginMu.Unlock()
	if sameCount != 2 {
		t.Fatalf("前提：两个账户都应判为原样重新上传，loginSame=%d", sameCount)
	}
	// 后台载入之前（还在稳定期、下一轮扫描还没到）又上传了新登录的文件
	for id, state := range fresh {
		writeAuthFile(t, pool, id, state)
	}
	watcher.startLoginRefresh(admin.lifecycle, known)
	waitFor(t, "后台载入结束", func() bool { return !watcher.refreshing.Load() })
	watcher.loginMu.Lock()
	pendingCount := len(watcher.loginPending)
	watcher.loginMu.Unlock()
	if pendingCount != 0 {
		t.Fatalf("文件又被替换的不应按原样重新上传重新排队（由下一轮扫描按新文件检查），待载入 %d 个", pendingCount)
	}
	for id, state := range fresh {
		if aistudio.LoginIdentity(diskAuthState(t, pool, id)) != aistudio.LoginIdentity(state) {
			t.Fatalf("%s：磁盘上新上传的文件被内存里的旧登录覆盖了", id)
		}
		if !hasLogContaining(admin.requests, "认证文件在载入前又被替换", "账户="+id) {
			t.Fatalf("%s 缺少文件又被替换的日志：%v", id, serviceLogMessages(admin.requests))
		}
	}
	scanLoginAt(t, watcher, later.Add(2*time.Minute))
	for id, state := range fresh {
		if identity, _ := pool.LoginIdentity(id); identity != aistudio.LoginIdentity(state) {
			t.Fatalf("%s：下一轮扫描应载入新上传的文件", id)
		}
		if item := reauthPending(onboarder, id); item == nil || item.reauthAt.IsZero() {
			t.Fatalf("%s 载入新文件后应加入验证队列", id)
		}
	}
	if status := testStatus(t, pool, required); status.State == aistudio.AccountAuthRequired {
		t.Fatal("需要登录的账户载入新文件后应恢复就绪")
	}
	if _, listed := admin.service.permissionDenied.identityOf(denied); listed {
		t.Fatal("403 账户载入新文件后应清除登记")
	}
}

// TestReauthNewUploadReplacedWhileWaiting 载入新文件时账户正忙，等账户空闲期间又上传了一份：写回的是租约内重新读到的最新文件，
// 不是等待之前读到的那一份（原先写回旧的，覆盖刚上传的新文件）
func TestReauthNewUploadReplacedWhileWaiting(t *testing.T) {
	const id = "busy@example.com"
	admin, _, _ := reauthTestAdmin(t, id)
	pool := admin.pool
	writeAuthFile(t, pool, id, authTestState(t, "FIRST", false))
	held, err := pool.AcquireFor(context.Background(), aistudio.AccountSelection{AccountID: id})
	if err != nil {
		t.Fatal(err)
	}
	summary := loginSummaryOf(t, pool, id)
	done := make(chan loginApply, 1)
	go func() { done <- admin.applyLoginUpdateMode(admin.lifecycle, summary, nil, nil) }()
	// 载入已读了 FIRST、正在等账户空闲
	time.Sleep(200 * time.Millisecond)
	second := authTestState(t, "SECOND", false)
	writeAuthFile(t, pool, id, second)
	if err := held.Release(); err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-done:
		if result != loginApplied {
			t.Fatalf("等到账户空闲后应载入，结果 %d", result)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("载入没有结束")
	}
	if identity, _ := pool.LoginIdentity(id); identity != aistudio.LoginIdentity(second) {
		t.Fatal("应载入等待期间上传的最新文件")
	}
	if aistudio.LoginIdentity(diskAuthState(t, pool, id)) != aistudio.LoginIdentity(second) {
		t.Fatal("磁盘上最新上传的文件不应被等待之前读到的旧内容覆盖")
	}
}

// TestReauthSharedDirOlderWriteBackIgnored 共用账户目录：另一个进程把较早签发的轮换 Cookie 写盘，本进程随后收到更新的、只暂存未写盘。
// 磁盘上的轮换 Cookie 比内存旧，但文件不是本服务读入或写过的那一份：按回写处理。403 冷却中的账户不清除登记与冷却、不排队验证，
// 写一行回写提示（同一账户每小时最多一次）；健康账户不写“内容相同”的提示、不计入认证文件未变化（原先都当成原样重新上传）
func TestReauthSharedDirOlderWriteBackIgnored(t *testing.T) {
	const listed, healthy = "listed@example.com", "healthy@example.com"
	admin, onboarder, _ := reauthDeniedAdmin(t, []string{listed}, healthy)
	pool := admin.pool
	watcher := newTestAccountWatcher(admin)
	later := time.Now().Add(time.Minute)
	scanLoginAt(t, watcher, later)
	others := map[string]*aistudio.AccountPool{listed: sharedDirPool(t, pool, listed), healthy: sharedDirPool(t, pool, healthy)}
	writeBack := func(round int) {
		for id, other := range others {
			expires := time.Now().Add(365*24*time.Hour + time.Duration(round)*time.Minute)
			// 另一个进程先收到较早签发的并写盘，本进程随后收到更新的（文件刚写过，只暂存）
			mergeRotating(t, other, id, "other", expires, true)
			mergeRotating(t, pool, id, "ours", expires.Add(10*time.Second), false)
			memory, _ := pool.StorageStateOf(id)
			disk := diskAuthState(t, pool, id)
			if change := compareAuthFile(disk, memory, pool.StorageFileKnown(id, diskDigest(t, pool, id))); change != authFileOlder {
				t.Fatalf("前提：%s 磁盘上的轮换 Cookie 应比内存旧（compareAuthFile=%d）", id, change)
			}
		}
	}
	writeBack(0)
	scanLoginAt(t, watcher, later.Add(time.Minute))
	if _, ok := admin.service.permissionDenied.identityOf(listed); !ok || len(forbiddenCooldowns(t, pool, listed)) == 0 {
		t.Fatal("另一个进程回写较早的轮换 Cookie 不应清除 403 登记与冷却")
	}
	if reauthPending(onboarder, listed) != nil || hasLogContaining(admin.requests, "按重新上传处理") {
		t.Fatalf("另一个进程回写不应按重新上传处理：%v", serviceLogMessages(admin.requests))
	}
	if !hasLogContaining(admin.requests, "按共用账户目录的其他进程回写处理", "账户="+listed, "状态=403 无权限冷却") {
		t.Fatalf("失效账户应写一行回写提示：%v", serviceLogMessages(admin.requests))
	}
	if onboarder.status().Unchanged != 0 || hasLogContaining(admin.requests, "账户="+healthy) {
		t.Fatalf("健康账户被另一个进程回写不应提示：%v", serviceLogMessages(admin.requests))
	}
	writeBack(1)
	scanLoginAt(t, watcher, later.Add(2*time.Minute))
	hints := 0
	for _, message := range serviceLogMessages(admin.requests) {
		if strings.Contains(message, "按共用账户目录的其他进程回写处理") {
			hints++
		}
	}
	if hints != 1 {
		t.Fatalf("同一账户一小时内只提示一次，实际 %d 次", hints)
	}
	if _, ok := admin.service.permissionDenied.identityOf(listed); !ok {
		t.Fatal("再次回写也不应清除 403 登记")
	}
}

// TestReauthRotatingOnlyReexportHinted 失效账户同一会话重新导出、只有轮换 Cookie 更新（例如在官网解开 403 后重新导出）：与另一个进程的
// 回写分不开，仍按回写处理，但写一行提示说明怎么处理（原先完全静默）
func TestReauthRotatingOnlyReexportHinted(t *testing.T) {
	const id = "a@example.com"
	admin, onboarder, _ := reauthTestAdmin(t, id)
	pool := admin.pool
	markReauthRequired(t, pool, id)
	watcher := newTestAccountWatcher(admin)
	later := time.Now().Add(time.Minute)
	scanLoginAt(t, watcher, later)
	memory, _ := pool.StorageStateOf(id)
	exported := memory
	exported.Cookies = nil
	for _, cookie := range memory.Cookies {
		if _, rotating := rotatingCookies[cookie.Name]; rotating {
			cookie.Value = "browser-now"
		}
		exported.Cookies = append(exported.Cookies, cookie)
	}
	exported = withCookie(exported, "SIDCC", "browser-now")
	writeAuthFile(t, pool, id, exported)
	scanLoginAt(t, watcher, later.Add(time.Minute))
	if reauthPending(onboarder, id) != nil {
		t.Fatal("只多了或换了轮换 Cookie 仍按回写处理")
	}
	if !hasLogContaining(admin.requests, "只多了或换了 Google 轮换的 Cookie", "账户="+id, "状态=需要登录", "点“验证”") {
		t.Fatalf("应写一行提示：%v", serviceLogMessages(admin.requests))
	}
}

// TestReauthChromeReimportAutoDisabledEnabled Chrome 就地重新导入自动停用（验证未通过、没有手动停用记录）的失效账户：按默认启用、加入
// 队列验证（与目录扫描载入认证文件更新一致）；手动停用的仍保持停用、记为跳过（手动停用）
func TestReauthChromeReimportAutoDisabledEnabled(t *testing.T) {
	isolateDisableNotes(t)
	const auto, manual = "auto@example.com", "manual@example.com"
	admin, _, _ := keepCatalogAdmin(t, auto, manual)
	onboarder := newTestOnboarder(admin, t.TempDir())
	t.Cleanup(onboarder.flush)
	admin.onboard = onboarder
	chromeReimportStubs(t, func(id string) string {
		return map[string]string{"Default/auto": auto, "Profile 1/manual": manual}[id]
	})
	disableTestAccount(t, admin.pool, auto)
	accountDisableNotes.set(auto, "新账户自动处理验证未通过（跳转到登录页），认证文件更新或重启后按默认启用", true)
	disableTestAccount(t, admin.pool, manual)
	accountDisableNotes.set(manual, "管理页停用", false)
	for _, target := range []chromeReimport{{chromeID: "Default/auto", accountID: auto}, {chromeID: "Profile 1/manual", accountID: manual}} {
		if _, err := admin.reimportChromeAccount(context.Background(), "root", target); err != nil {
			t.Fatal(err)
		}
	}
	if !testStatus(t, admin.pool, auto).Enabled {
		t.Fatal("自动停用的账户重新导入后应按默认启用")
	}
	if item := reauthPending(onboarder, auto); item == nil || item.reauthAt.IsZero() {
		t.Fatal("按默认启用后应加入队列验证")
	}
	if !hasLogContaining(admin.requests, "Chrome 重新导入，已载入新的登录 Cookie 并启用账户（没有手动停用记录） | 账户="+auto) {
		t.Fatalf("缺少启用日志：%v", serviceLogMessages(admin.requests))
	}
	if testStatus(t, admin.pool, manual).Enabled || reauthPending(onboarder, manual) != nil {
		t.Fatal("手动停用的账户应保持停用、不验证")
	}
	if record := onboarder.records[manual]; record.Outcome != reauthManualDisabled {
		t.Fatalf("手动停用的应记为跳过（手动停用）：%+v", record)
	}
}

// TestReauthUploadBeforeVerifyStartNoDoubleVerify 账户已取进本批（还在等并发名额、验证没开始）时又重新上传：验证用的就是新文件，
// 只验证一次（原先结束后又排一次）
func TestReauthUploadBeforeVerifyStartNoDoubleVerify(t *testing.T) {
	const id = "a@example.com"
	_, onboarder, login := reauthTestAdmin(t, id)
	onboarder.queueReauth(id, reauthSourceAuthFile)
	batch, policy := onboarder.takeBatch(time.Now())
	onboarder.queueReauth(id, reauthSourceAuthFile)
	onboarder.processBatch(context.Background(), batch, policy)
	runReauthBatch(t, onboarder)
	if login.calls.Load() != 1 || reauthPending(onboarder, id) != nil {
		t.Fatalf("验证开始前的重新上传不应重复验证：验证 %d 次", login.calls.Load())
	}
	if record := onboarder.records[id]; record.Outcome != reauthRestored {
		t.Fatalf("应记为已恢复：%+v", record)
	}
	// 开始验证后暂缓（固定端口暂停）放回队列：下次取进本批、还没开始验证时重新上传，同样不再多验一次
	onboarder.queueReauth(id, reauthSourceAuthFile)
	if batch, _ := onboarder.takeBatch(time.Now()); len(batch) != 1 {
		t.Fatalf("本批 = %v", batch)
	}
	onboarder.markStarted(id)
	onboarder.finish(id, onboardDeferred, "端口暂停", false)
	if batch, _ := onboarder.takeBatch(time.Now()); len(batch) != 1 {
		t.Fatalf("暂缓后的本批 = %v", batch)
	}
	onboarder.queueReauth(id, reauthSourceAuthFile)
	onboarder.mu.Lock()
	requeue := onboarder.processing[id].requeue
	onboarder.mu.Unlock()
	if requeue {
		t.Fatal("暂缓放回队列后还没开始验证，不应结束后再排一次")
	}
}

// TestReauthRequeueSurvivesRestart 验证开始后又重新上传：这次结果只进最近处理，按新文件重新排队；没处理完就重启，载入后仍按重新验证排队
// （原先写了上一份文件的处理记录，重启后因已有记录跳过）。重启日志分项写明新增与重新上传的个数
func TestReauthRequeueSurvivesRestart(t *testing.T) {
	const id, added = "a@example.com", "added@example.com"
	admin, _, _ := reauthTestAdmin(t, id, added)
	directory := t.TempDir()
	onboarder := newTestOnboarder(admin, directory)
	onboarder.mu.Lock()
	onboarder.records[added] = onboardRecord{Result: onboardBaseline, At: time.Now()}
	onboarder.mu.Unlock()
	onboarder.queueReauth(id, reauthSourceAuthFile)
	if batch, _ := onboarder.takeBatch(time.Now()); len(batch) != 1 {
		t.Fatalf("本批 = %v", batch)
	}
	onboarder.markStarted(id)
	onboarder.queueReauth(id, reauthSourceAuthFile)
	onboarder.finish(id, onboardVerifyFailed, "上一份文件验证未通过", false)
	if item := reauthPending(onboarder, id); item == nil || item.reauthAt.IsZero() {
		t.Fatal("前提：应按新文件重新排队")
	}
	if !hasLogContaining(admin.requests, "重新上传后：仍需登录 | 账户="+id, "来源=上一份认证文件（已按新的重新排队）") {
		t.Fatalf("上一份文件的结果应写日志：%v", serviceLogMessages(admin.requests))
	}
	// 服务停止期间又新增了一个账户（没有处理记录）
	onboarder.mu.Lock()
	delete(onboarder.records, added)
	onboarder.savePending = true
	onboarder.mu.Unlock()
	onboarder.flush()
	restarted := newAccountOnboarder(admin, directory)
	if item := reauthPending(restarted, id); item == nil || item.reauthAt.IsZero() {
		t.Fatalf("重启后应仍按新文件重新验证：%+v（记录 %+v）", item, restarted.records[id])
	}
	if !hasLogContaining(admin.requests, "新账户自动处理 | 重新加入队列 2 个账户：服务停止期间新增 1 个、认证文件更新后待验证的 1 个") {
		t.Fatalf("重启日志应分项写明：%v", serviceLogMessages(admin.requests))
	}
}

// TestReauthPreservedMtimeHint 失效账户的认证文件被原样重新写入，修改时间却变早（上传工具保留了原文件的修改时间）：内容与本服务上次读入的
// 相同，仍按本服务自己的文件处理，但写一行提示说明原因与办法（原先完全静默）；健康账户不提示
func TestReauthPreservedMtimeHint(t *testing.T) {
	const failing, healthy = "failing@example.com", "healthy@example.com"
	admin, onboarder, _ := reauthTestAdmin(t, failing, healthy)
	pool := admin.pool
	markReauthRequired(t, pool, failing)
	watcher := newTestAccountWatcher(admin)
	later := time.Now().Add(time.Minute)
	scanLoginAt(t, watcher, later)
	old := time.Now().Add(-24 * time.Hour)
	for _, id := range []string{failing, healthy} {
		account, err := pool.Account(id)
		if err != nil {
			t.Fatal(err)
		}
		content, err := os.ReadFile(account.StoragePath)
		if err != nil {
			t.Fatal(err)
		}
		upload := account.StoragePath + ".upload"
		if err := os.WriteFile(upload, content, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(upload, old, old); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(upload, account.StoragePath); err != nil {
			t.Fatal(err)
		}
	}
	scanLoginAt(t, watcher, later.Add(time.Minute))
	if !hasLogContaining(admin.requests, "修改时间没有变新", "账户="+failing, "状态=需要登录", "不要保留时间戳") {
		t.Fatalf("失效账户应写一行提示：%v", serviceLogMessages(admin.requests))
	}
	if hasLogContaining(admin.requests, "账户="+healthy) || onboarder.status().Unchanged != 0 {
		t.Fatalf("健康账户不提示：%v", serviceLogMessages(admin.requests))
	}
}

// TestReauthReadySameLoginHint 就绪账户同一会话重新导出（内容有变化、登录签名没变）：按原口径不重新载入、不排队，但写一行提示并计入面板
// （原先完全静默）；同一账户一小时内只提示、计数一次
func TestReauthReadySameLoginHint(t *testing.T) {
	const id = "ready@example.com"
	admin, onboarder, _ := reauthTestAdmin(t, id)
	pool := admin.pool
	watcher := newTestAccountWatcher(admin)
	later := time.Now().Add(time.Minute)
	scanLoginAt(t, watcher, later)
	generation := pool.AuthGeneration(id)
	reexportAuthFile(t, pool, id)
	scanLoginAt(t, watcher, later.Add(time.Minute))
	if pool.AuthGeneration(id) != generation || reauthPending(onboarder, id) != nil {
		t.Fatal("就绪账户登录签名没变不应重新载入或排队")
	}
	if !hasLogContaining(admin.requests, "认证文件内容有变化，但登录签名 Cookie 没变", "账户="+id) || onboarder.status().SameLogin != 1 {
		t.Fatalf("应写一行提示并计入面板：same_login=%d %v", onboarder.status().SameLogin, serviceLogMessages(admin.requests))
	}
	if onboarder.status().Unchanged != 0 {
		t.Fatal("内容有变化不计入认证文件未变化")
	}
	writeAuthFile(t, pool, id, withCookie(diskAuthState(t, pool, id), "NID", "again"))
	scanLoginAt(t, watcher, later.Add(2*time.Minute))
	if onboarder.status().SameLogin != 1 {
		t.Fatal("同一账户一小时内只计一次")
	}
	writeAuthFile(t, pool, id, withCookie(diskAuthState(t, pool, id), "NID", "next-hour"))
	scanLoginAt(t, watcher, later.Add(2*time.Hour))
	if onboarder.status().SameLogin != 2 {
		t.Fatal("超过一小时应再提示")
	}
}

// TestReauthForbiddenPathSkipsQueueLock 生成请求的 403 登记不等新账户自动处理队列的锁（保存处理记录时持有、要整份编码并写盘）：
// 不是最近重新上传后已恢复的账户直接返回；已恢复的照常改记（重启后同样）
func TestReauthForbiddenPathSkipsQueueLock(t *testing.T) {
	const plain, restored = "plain@example.com", "restored@example.com"
	admin, onboarder, _ := reauthDeniedAdmin(t, nil, plain, restored)
	onboarder.mu.Lock()
	done := make(chan struct{})
	go func() {
		admin.service.recordPermissionDenied(plain, testRuntimeModel, aistudio.ChannelPlayground, "The caller does not have permission", time.Now(), nil)
		close(done)
	}()
	select {
	case <-done:
		onboarder.mu.Unlock()
	case <-time.After(2 * time.Second):
		onboarder.mu.Unlock()
		<-done
		t.Fatal("403 登记在队列持锁期间阻塞")
	}
	onboarder.queueReauth(restored, reauthSourceAuthFile)
	runReauthBatch(t, onboarder)
	onboarder.mu.Lock()
	onboarder.savePending = true
	onboarder.mu.Unlock()
	onboarder.flush()
	restarted := newAccountOnboarder(admin, filepath.Dir(onboarder.path))
	admin.service.reauthOnboard.Store(restarted)
	admin.onboard = restarted
	admin.service.recordPermissionDenied(restored, testRuntimeModel, aistudio.ChannelPlayground, verifyAccountMessage, time.Now().Add(time.Second), nil)
	if record := restarted.records[restored]; record.Outcome != reauthVerifyRequired {
		t.Fatalf("重启后已恢复的账户又返回需要验证账号的 403 应改记为仍需验证：%+v", record)
	}
	if record, recorded := restarted.records[plain]; recorded && record.Source == onboardSourceReauth {
		t.Fatalf("没有重新上传过的账户不应有重新上传的结果：%+v", record)
	}
}
