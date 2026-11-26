package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// 重新上传认证文件第 2 轮复查的回归测试：仍在接请求的账户上传新登录后被旧 Worker 写回的 Cookie 顶掉、共用账户目录一次上传两个进程
// 无限循环、服务停止期间的重新上传、面板看不出未按重新上传处理与等待载入、手动修好后结果不更新、队列里的暂缓被提前取出

// browserCookiesOf 为账户旧 Worker 浏览器里的 Cookie（当前登录，NID 为浏览器自己的值）
func browserCookiesOf(t *testing.T, pool *aistudio.AccountPool, accountID string) []aistudio.StateCookie {
	t.Helper()
	state := withCookie(diskAuthState(t, pool, accountID), "NID", "browser-nid")
	return append([]aistudio.StateCookie(nil), state.Cookies...)
}

// cookieValue 返回登录状态里某个 Cookie 的值
func cookieValue(state aistudio.StorageState, name string) string {
	for _, cookie := range state.Cookies {
		if cookie.Name == name {
			return cookie.Value
		}
	}
	return ""
}

// knownSummaries 返回扫描用的账户轻量状态表
func knownSummaries(pool *aistudio.AccountPool) ([]aistudio.AccountLoginSummary, map[string]aistudio.AccountLoginSummary) {
	summaries := pool.LoginSummaries()
	known := make(map[string]aistudio.AccountLoginSummary, len(summaries))
	for _, summary := range summaries {
		known[summary.ID] = summary
	}
	return summaries, known
}

// TestReauthOldWorkerWriteBackKeepsNewLogin 403 单模型冷却、仍在接其他模型请求的账户上传了新登录的认证文件；后台载入读到新文件、在等
// 账户空闲时，进行中的请求结束，旧 Worker 把浏览器里旧登录的 Cookie 写回（ReplaceCookies）。原先写回覆盖了新文件，载入时又把这份
// “旧登录 + 新文件残余”当成又上传的文件写回，新登录丢失，403 登记却照样清除。现在写回不合并进新登录的文件，载入的是新登录
func TestReauthOldWorkerWriteBackKeepsNewLogin(t *testing.T) {
	const id = "busy@example.com"
	admin, onboarder, _ := reauthDeniedAdmin(t, []string{id})
	pool := admin.pool
	old := browserCookiesOf(t, pool, id)
	watcher := newTestAccountWatcher(admin)
	later := time.Now().Add(time.Minute)
	scanLoginAt(t, watcher, later)
	fresh := authTestState(t, "NEW", false)
	writeAuthFile(t, pool, id, fresh)
	inflight, err := pool.AcquireAccount(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	summaries, known := knownSummaries(pool)
	watcher.detectLoginUpdates(summaries, later.Add(time.Minute))
	watcher.startLoginRefresh(admin.lifecycle, known)
	// 后台载入已读到新文件、在等账户空闲（loginRefreshAcquireWait）
	time.Sleep(200 * time.Millisecond)
	if err := errors.Join(inflight.ReplaceCookies(old), inflight.Release()); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "后台载入结束", func() bool { return !watcher.refreshing.Load() })
	want := aistudio.LoginIdentity(fresh)
	if memory, _ := pool.LoginIdentity(id); memory != want {
		t.Fatalf("内存里应是新登录，得到 %s，新登录 %s", memory, want)
	}
	if disk := aistudio.LoginIdentity(diskAuthState(t, pool, id)); disk != want {
		t.Fatalf("磁盘上的新登录被旧 Worker 的 Cookie 顶掉：%s", disk)
	}
	if cookieValue(diskAuthState(t, pool, id), "NID") == "browser-nid" {
		t.Fatal("旧浏览器的 Cookie 不应合并进新登录的文件")
	}
	if _, listed := admin.service.permissionDenied.identityOf(id); listed {
		t.Fatal("载入新登录后应清除 403 登记")
	}
	if item := reauthPending(onboarder, id); item == nil || item.reauthAt.IsZero() {
		t.Fatal("载入新登录后应加入验证队列")
	}
}

// TestReauthWriteBacksBeforeScanKeepNewLogin 上传新登录之后、扫描发现之前（5～15 秒），账户上的请求照常结束：旧 Worker 的浏览器 Cookie
// 写回（ReplaceCookies）、响应的 Set-Cookie 合并（MergeSetCookieHeaders）都不写进新登录的文件，Go 通道发请求前读认证状态
// （ReloadStorageState）照样按磁盘返回但不读进内存，磁盘上仍是上传的那一份（含它自己的 __Secure-1PSIDTS）。扫描随后载入新登录、
// 清除 403 登记与冷却并排队验证。原先浏览器写回直接覆盖新文件，扫描只看到本服务自己写的文件，整次上传丢失；Set-Cookie 把旧会话的
// 轮换 Cookie 写进新登录的文件；Go 通道把新文件悄悄读进内存，扫描看不出登录变了
func TestReauthWriteBacksBeforeScanKeepNewLogin(t *testing.T) {
	const id = "busy@example.com"
	admin, onboarder, _ := reauthDeniedAdmin(t, []string{id})
	pool := admin.pool
	old := browserCookiesOf(t, pool, id)
	oldIdentity, _ := pool.LoginIdentity(id)
	watcher := newTestAccountWatcher(admin)
	later := time.Now().Add(time.Minute)
	scanLoginAt(t, watcher, later)
	fresh := withCookie(authTestState(t, "NEW", false), "__Secure-1PSIDTS", "new-session")
	writeAuthFile(t, pool, id, fresh)
	lease, err := pool.AcquireAccount(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	reloaded, reloadErr := lease.ReloadStorageState()
	err = errors.Join(
		reloadErr,
		lease.MergeSetCookieHeaders(
			[]string{"__Secure-1PSIDTS=old-session; Domain=.google.com; Path=/; Secure; Max-Age=31536000"},
			"https://aistudio.google.com/", time.Now(),
		),
		lease.ReplaceCookies(old),
		lease.Release(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if aistudio.LoginIdentity(reloaded) != aistudio.LoginIdentity(fresh) {
		t.Fatal("Go 通道读认证状态照样按磁盘返回")
	}
	if memory, _ := pool.LoginIdentity(id); memory != oldIdentity {
		t.Fatal("扫描载入之前不应把新文件读进内存")
	}
	disk := diskAuthState(t, pool, id)
	if aistudio.LoginIdentity(disk) != aistudio.LoginIdentity(fresh) || cookieValue(disk, "__Secure-1PSIDTS") != "new-session" ||
		cookieValue(disk, "NID") == "browser-nid" {
		t.Fatalf("磁盘上的新文件被回写改了：PSIDTS=%s NID=%s", cookieValue(disk, "__Secure-1PSIDTS"), cookieValue(disk, "NID"))
	}
	scanLoginAt(t, watcher, later.Add(time.Minute))
	memory, _ := pool.StorageStateOf(id)
	if aistudio.LoginIdentity(memory) != aistudio.LoginIdentity(fresh) || cookieValue(memory, "__Secure-1PSIDTS") != "new-session" {
		t.Fatalf("扫描应载入上传的新登录（不带旧会话的 Cookie）：PSIDTS=%s", cookieValue(memory, "__Secure-1PSIDTS"))
	}
	if _, listed := admin.service.permissionDenied.identityOf(id); listed || len(forbiddenCooldowns(t, pool, id)) != 0 {
		t.Fatal("载入新登录后应清除 403 登记与冷却")
	}
	if item := reauthPending(onboarder, id); item == nil || item.reauthAt.IsZero() {
		t.Fatal("载入新登录后应加入验证队列")
	}
}

// TestReauthUnusableUploadStillWrittenBack 换上的文件缺登录签名 Cookie、不能签名（坏文件）：浏览器写回照原来的方式合并，账户继续用能用的
// 登录，不会因为一份坏文件一直跳过回写；回写之后磁盘上的登录签名与内存一致，扫描不再为它重新载入（不会每轮都载入失败一次）
func TestReauthUnusableUploadStillWrittenBack(t *testing.T) {
	const id = "healthy@example.com"
	admin, onboarder, _ := reauthTestAdmin(t, id)
	pool := admin.pool
	old := browserCookiesOf(t, pool, id)
	identity, _ := pool.LoginIdentity(id)
	watcher := newTestAccountWatcher(admin)
	later := time.Now().Add(time.Minute)
	scanLoginAt(t, watcher, later)
	broken := withoutCookie(withoutCookie(withoutCookie(diskAuthState(t, pool, id), "SAPISID"), "__Secure-1PAPISID"), "__Secure-3PAPISID")
	writeAuthFile(t, pool, id, withCookie(broken, "NID", "broken-upload"))
	lease, err := pool.AcquireAccount(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if err := errors.Join(lease.ReplaceCookies(old), lease.Release()); err != nil {
		t.Fatal(err)
	}
	if disk := aistudio.LoginIdentity(diskAuthState(t, pool, id)); disk != identity {
		t.Fatal("坏文件上应照常合并浏览器里能用的登录")
	}
	before := len(serviceLogMessages(admin.requests))
	scanLoginAt(t, watcher, later.Add(time.Minute))
	scanLoginAt(t, watcher, later.Add(2*time.Minute))
	for _, line := range serviceLogMessages(admin.requests)[before:] {
		t.Logf("LOG %s", line)
	}
	if hasLogContaining(admin.requests, "载入更新后的认证文件失败", "账户="+id) {
		t.Fatal("回写已恢复能用的登录，扫描不应再载入失败")
	}
	if reauthPending(onboarder, id) != nil {
		t.Fatal("不应排队验证")
	}
}

// TestReauthLeaseStateIgnoresOwnWriteWhileWaiting 载入新文件时（leaseLoginState）磁盘上是本服务在等账户空闲期间自己写的文件（同一登录的
// 浏览器 Cookie 写回）：认证代际没变时仍写回检测时读到的那一份；期间在管理页重新登录（代际变了）时不写回，由下一轮扫描重新检查
func TestReauthLeaseStateIgnoresOwnWriteWhileWaiting(t *testing.T) {
	const id = "busy@example.com"
	admin, _, _ := reauthTestAdmin(t, id)
	pool := admin.pool
	account, err := pool.Account(id)
	if err != nil {
		t.Fatal(err)
	}
	uploaded := withCookie(diskAuthState(t, pool, id), "NID", "user-reexport")
	writeAuthFile(t, pool, id, uploaded)
	generation := pool.AuthGeneration(id)
	prepared, digest, err := admin.loginSourceState(id, account.StoragePath, nil)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := pool.AcquireAccount(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lease.Release() }()
	// 同一登录：浏览器写回照常合并，文件变成本服务自己写的
	if err := lease.ReplaceCookies(browserCookiesOf(t, pool, id)); err != nil {
		t.Fatal(err)
	}
	if cookieValue(diskAuthState(t, pool, id), "NID") != "browser-nid" {
		t.Fatal("前提：同一登录的浏览器写回应照常合并")
	}
	state, gotDigest, err := admin.leaseLoginState(id, account.StoragePath, nil, prepared, digest, generation)
	if err != nil || gotDigest != digest || cookieValue(state, "NID") != "user-reexport" {
		t.Fatalf("应写回检测时读到的那一份：err=%v NID=%s", err, cookieValue(state, "NID"))
	}
	// 期间保存过新的登录状态（管理页重新登录、Chrome 重新导入），代际变了
	if err := lease.SaveStorageState(withCookie(diskAuthState(t, pool, id), "NID", "admin-login")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := admin.leaseLoginState(id, account.StoragePath, nil, prepared, digest, generation); !errors.Is(err, errAuthFileReplaced) {
		t.Fatalf("代际变了时不应写回检测时的文件：%v", err)
	}
}

// TestReauthSharedDirSingleUploadNoLoop 两个进程共用账户目录，需要登录的账户只上传了一次（原样上传紧凑 JSON，或同一会话重新导出但仍无效）：
// 载入时磁盘上已经是要写的内容就不重写文件，另一个进程看不到“又被重新写入”，各自只处理一次。原先每个进程载入后都重写文件，
// 另一个进程看到修改时间变新、账户又失效，再按重新上传处理并重写，两边无限循环
func TestReauthSharedDirSingleUploadNoLoop(t *testing.T) {
	for _, tc := range []struct {
		name   string
		upload func(t *testing.T, pool *aistudio.AccountPool, id string)
	}{
		{"same-bytes-compact", func(t *testing.T, pool *aistudio.AccountPool, id string) {
			account, err := pool.Account(id)
			if err != nil {
				t.Fatal(err)
			}
			data, err := json.Marshal(diskAuthState(t, pool, id))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(account.StoragePath, data, 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"reexport-still-invalid", func(t *testing.T, pool *aistudio.AccountPool, id string) {
			writeAuthFile(t, pool, id, withCookie(diskAuthState(t, pool, id), "NID", "reexport"))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const id = "loop@example.com"
			admin1, _, _ := reauthTestAdmin(t, id)
			pool1 := admin1.pool
			admin2, _ := sharedDirProcess(t, pool1, id)
			pool2 := admin2.pool
			markReauthRequired(t, pool1, id)
			markReauthRequired(t, pool2, id)
			w1, w2 := newTestAccountWatcher(admin1), newTestAccountWatcher(admin2)
			later := time.Now().Add(time.Minute)
			scanLoginAt(t, w1, later)
			scanLoginAt(t, w2, later)
			tc.upload(t, pool1, id)
			for round := 1; round <= 3; round++ {
				scanLoginAt(t, w1, later.Add(time.Duration(2*round-1)*time.Minute))
				markReauthRequired(t, pool1, id) // 进程 1：新 Cookie 仍无效
				scanLoginAt(t, w2, later.Add(time.Duration(2*round)*time.Minute))
				markReauthRequired(t, pool2, id) // 进程 2：同上
			}
			handled := func(admin *runtimeAdmin) int {
				return countLogs(admin.requests, "已清除旧判定并恢复调度") + countLogs(admin.requests, "检测到新的认证文件，已重新载入并恢复调度")
			}
			if first, second := handled(admin1), handled(admin2); first != 1 || second != 1 {
				t.Fatalf("用户只上传了一次，两个进程应各处理一次：进程 1=%d 进程 2=%d", first, second)
			}
		})
	}
}

// sharedDirProcess 返回共用 pool 账户目录的另一个进程：新的账户池、管理端与新账户自动处理队列（测试浏览器验证判定已登录）
func sharedDirProcess(t *testing.T, pool *aistudio.AccountPool, accountID string) (*runtimeAdmin, *accountOnboarder) {
	t.Helper()
	account, err := pool.Account(accountID)
	if err != nil {
		t.Fatal(err)
	}
	return startupTestAdmin(t, filepath.Dir(account.Directory))
}

// startupTestAdmin 模拟一次启动：从账户目录 root 载入账户池（与 enableTestAdmin 相同的装配），403 登记表从 root 读入，
// 新账户自动处理队列与测试浏览器验证（判定已登录）
func startupTestAdmin(t *testing.T, root string) (*runtimeAdmin, *accountOnboarder) {
	t.Helper()
	store := aistudio.NewAccountStore(root)
	accounts, failures := store.LoadTolerant()
	if len(failures) > 0 {
		t.Fatal(failures)
	}
	pool := aistudio.NewAccountPool(nil, 2)
	t.Cleanup(pool.WaitBackground)
	for _, account := range accounts {
		if err := pool.Add(account); err != nil {
			t.Fatal(err)
		}
		if err := pool.SetCatalog(account.ID, aistudio.BenefitTierFree, []aistudio.Model{{
			ID: testRuntimeModel, Methods: []string{"generateContent", "countTokens", "bidiGenerateContent"},
			Capabilities: map[string]bool{"chat_model": true},
		}}); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	requests := newRequestRegistry(ctx)
	workers := newAccountWorkerManager(pool, accounts, requests, "", "", time.Minute, 1, 1, 1, false)
	t.Cleanup(func() { _ = workers.Close() })
	headers, err := newAccountHeaderProvider(accounts, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(headers.Close)
	service := &trackedService{
		lifecycle: ctx, catalog: &enableTestCatalog{pool: pool}, pool: pool, requests: requests, workers: workers,
		forbidden: newForbiddenTracker(), quota: newQuotaSharing("", requests),
	}
	service.timeout.Store(int64(time.Minute))
	service.permissionDenied = newPermissionDeniedRegistry(root, requests)
	admin := &runtimeAdmin{
		lifecycle: ctx, pool: pool, service: service, requests: requests, workers: workers, headers: headers, store: store,
	}
	admin.login = &verifyTestLogin{verification: aistudio.LoginVerification{Authenticated: true}}
	onboarder := newTestOnboarder(admin, t.TempDir())
	t.Cleanup(onboarder.flush)
	admin.onboard = onboarder
	service.reauthOnboard.Store(onboarder)
	return admin, onboarder
}

// stoppedRun 为一次启动（“上一次运行”）：在账户目录 root 上装配，before 里登记 403、写冷却等，结束时与正常关闭一样写出认证文件记录
func stoppedRun(t *testing.T, root string, stamps string, before func(admin *runtimeAdmin)) {
	t.Helper()
	admin, _ := startupTestAdmin(t, root)
	before(admin)
	admin.service.permissionDenied.flush()
	if err := admin.pool.FlushDeferredModelAccess(); err != nil {
		t.Fatal(err)
	}
	admin.pool.WaitBackground()
	saveAuthFileStamps(stamps, admin.pool, authFileStampsOwner.Add(1), admin.requests)
}

// startedRun 为服务停止期间改过认证文件后的下一次启动：对账（与 newRuntime 相同的顺序）并对照上次关闭时写下的记录
func startedRun(t *testing.T, root string, stamps string) (*runtimeAdmin, *accountOnboarder) {
	t.Helper()
	admin, onboarder := startupTestAdmin(t, root)
	reconciled := admin.service.reconcilePermissionDenied()
	admin.reconcileAuthFiles(loadAuthFileStamps(stamps, admin.requests), reconciled)
	return admin, onboarder
}

// createAccounts 在账户目录 root 里建好账户（登录签名 Cookie 为 V、NID 为 original）
func createAccounts(t *testing.T, root string, ids ...string) {
	t.Helper()
	store := aistudio.NewAccountStore(root)
	for _, id := range ids {
		_, publish, err := store.Create(aistudio.DefaultAccountConfig(id), withCookie(authTestState(t, "V", false), "NID", "original"))
		if err != nil {
			t.Fatal(err)
		}
		if err := publish.Release(); err != nil {
			t.Fatal(err)
		}
	}
}

// rewriteStopped 服务停止期间改写账户的认证文件（change 为空时原样重写，只有修改时间变了）
func rewriteStopped(t *testing.T, root string, id string, change func(aistudio.StorageState) aistudio.StorageState) {
	t.Helper()
	path := aistudio.StorageStatePath(filepath.Join(root, id))
	state, err := aistudio.LoadStorageState(path)
	if err != nil {
		t.Fatal(err)
	}
	if change != nil {
		state = change(state)
	}
	// 修改时间一定要变：记录里的是上次运行写入时的修改时间
	time.Sleep(10 * time.Millisecond)
	if err := aistudio.WriteStorageState(path, state); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(time.Second)
	if err := os.Chtimes(path, later, later); err != nil {
		t.Fatal(err)
	}
}

// TestReauthUploadedWhileStopped 服务停止期间（停服、上传、更新程序再启动）重新上传认证文件：上次正常关闭时写下了各账户认证文件的记录，
// 启动时对照。403 无权限账户同一会话重新导出与原样重新上传、需要验证账号整户暂停的账户同一会话重新导出，都清除这个账户的旧判定并
// 加入验证队列；重新登录（登录签名变了）的启动对账清除登记后同样加入验证；只差轮换 Cookie 的失效账户只提示（计入面板“未按重新上传
// 处理”）；状态正常的账户原样重写只计入“认证文件未变化”、换了登录的加入验证；没改过的账户不动。记录读后删除。原先启动时只认登录签名，
// 同一会话重新导出与原样上传的 403 登记、冷却与暂停都留着，换了登录的也不排队验证
func TestReauthUploadedWhileStopped(t *testing.T) {
	isolateDisableNotes(t)
	root := t.TempDir()
	stamps := filepath.Join(t.TempDir(), authFileStampsName)
	const (
		reexport  = "reexport@example.com"
		identical = "identical@example.com"
		paused    = "paused@example.com"
		relogin   = "relogin@example.com"
		rotating  = "rotating@example.com"
		untouched = "untouched@example.com"
		healthy   = "healthy@example.com"
		moved     = "moved@example.com"
	)
	denied := []string{reexport, identical, relogin, rotating, untouched}
	createAccounts(t, root, append(append([]string(nil), denied...), paused, healthy, moved)...)
	stoppedRun(t, root, stamps, func(admin *runtimeAdmin) {
		for _, id := range denied {
			if err := admin.pool.MarkCooldownIfGeneration(id, testRuntimeModel, admin.pool.ModelAccessGeneration(id), time.Now(),
				time.Now().Add(forbiddenModelRepeatCooldown), forbiddenModelReasonPrefix+"The caller does not have permission"); err != nil {
				t.Fatal(err)
			}
			admin.service.recordPermissionDenied(id, testRuntimeModel, aistudio.ChannelPlayground, "The caller does not have permission", time.Now(), nil)
		}
		recordVerifyRequired(admin, paused, testRuntimeModel, "other-model")
		coolModel(t, admin.pool, paused, "", verifyRequiredPause, verifyRequiredPauseReason)
	})
	if _, err := os.Stat(stamps); err != nil {
		t.Fatalf("正常关闭时应写出认证文件记录：%v", err)
	}
	rewriteStopped(t, root, reexport, func(state aistudio.StorageState) aistudio.StorageState {
		return withCookie(state, "NID", "user-browser")
	})
	rewriteStopped(t, root, identical, nil)
	rewriteStopped(t, root, paused, func(state aistudio.StorageState) aistudio.StorageState {
		return withCookie(state, "NID", "user-browser")
	})
	rewriteStopped(t, root, relogin, func(aistudio.StorageState) aistudio.StorageState {
		return withCookie(authTestState(t, "NEW", false), "NID", "fresh")
	})
	rewriteStopped(t, root, rotating, func(state aistudio.StorageState) aistudio.StorageState {
		return withCookie(state, "__Secure-1PSIDTS", "rotated")
	})
	rewriteStopped(t, root, healthy, nil)
	rewriteStopped(t, root, moved, func(aistudio.StorageState) aistudio.StorageState {
		return withCookie(authTestState(t, "MOVED", false), "NID", "fresh")
	})

	admin, onboarder := startedRun(t, root, stamps)
	if _, err := os.Stat(stamps); !os.IsNotExist(err) {
		t.Fatalf("记录读后应删除（进程被强杀后不再用它对照）：%v", err)
	}
	registry := admin.service.permissionDenied
	for _, id := range []string{reexport, identical, paused, relogin} {
		if _, listed := registry.identityOf(id); listed {
			t.Fatalf("%s：服务停止期间重新上传，启动后应清除 403 登记", id)
		}
		if reasons := forbiddenCooldowns(t, admin.pool, id); len(reasons) != 0 {
			t.Fatalf("%s：启动后应解除 403 冷却与暂停，仍有 %v", id, reasons)
		}
		if item := reauthPending(onboarder, id); item == nil || item.reauthAt.IsZero() {
			t.Fatalf("%s：启动后应加入验证队列", id)
		}
		if !hasLogContaining(admin.requests, "认证文件更新后自动验证", "账户="+id, "来源="+reauthSourceStopped) {
			t.Fatalf("%s：缺少加入队列的日志：%v", id, serviceLogMessages(admin.requests))
		}
	}
	if !hasLogContaining(admin.requests, "服务停止期间认证文件已重新上传，已清除旧判定", "账户="+reexport, "状态=403 无权限冷却") {
		t.Fatalf("缺少逐账户日志：%v", serviceLogMessages(admin.requests))
	}
	for _, id := range []string{rotating, untouched} {
		if _, listed := registry.identityOf(id); !listed || len(forbiddenCooldowns(t, admin.pool, id)) == 0 {
			t.Fatalf("%s：不应清除 403 登记与冷却", id)
		}
		if reauthPending(onboarder, id) != nil {
			t.Fatalf("%s：不应加入验证队列", id)
		}
	}
	if !hasLogContaining(admin.requests, "服务停止期间认证文件被重新写入，但只差 Google 轮换的 Cookie", "账户="+rotating) {
		t.Fatalf("只差轮换 Cookie 的失效账户应写一行提示：%v", serviceLogMessages(admin.requests))
	}
	if reauthPending(onboarder, healthy) != nil {
		t.Fatal("状态正常、原样重写的账户不应加入验证")
	}
	if item := reauthPending(onboarder, moved); item == nil || item.reauthAt.IsZero() {
		t.Fatal("状态正常、换了登录的账户应加入验证")
	}
	status := onboarder.status()
	if status.Unchanged != 1 || status.NotReuploaded != 1 || status.Reauth != 5 {
		t.Fatalf("面板计数：未变化=%d 未按重新上传处理=%d 重新上传=%d", status.Unchanged, status.NotReuploaded, status.Reauth)
	}
	if !hasLogContaining(admin.requests, "启动时对照认证文件", "按重新上传处理 4 个", "加入验证 1 个", "内容相同 1 个", "只差轮换 Cookie 或 localStorage 1 个") {
		t.Fatalf("缺少启动时的汇总：%v", serviceLogMessages(admin.requests))
	}
}

// TestReauthStartupWithoutStamps 没有上次正常关闭时写下的记录（进程被强杀、断电，或刚从没有这份记录的版本更新）：启动时不对照认证文件，
// 原样重写的 403 账户保持原样；登录签名与登记时不同的照旧由启动对账清除登记，并且加入验证队列（原先只清除、不验证）
func TestReauthStartupWithoutStamps(t *testing.T) {
	isolateDisableNotes(t)
	root := t.TempDir()
	stamps := filepath.Join(t.TempDir(), authFileStampsName)
	const identical, relogin = "identical@example.com", "relogin@example.com"
	createAccounts(t, root, identical, relogin)
	stoppedRun(t, root, stamps, func(admin *runtimeAdmin) {
		for _, id := range []string{identical, relogin} {
			admin.service.recordPermissionDenied(id, testRuntimeModel, aistudio.ChannelPlayground, "The caller does not have permission", time.Now(), nil)
		}
	})
	if err := os.Remove(stamps); err != nil {
		t.Fatal(err)
	}
	rewriteStopped(t, root, identical, nil)
	rewriteStopped(t, root, relogin, func(aistudio.StorageState) aistudio.StorageState { return authTestState(t, "NEW", false) })
	admin, onboarder := startedRun(t, root, stamps)
	if _, listed := admin.service.permissionDenied.identityOf(identical); !listed || reauthPending(onboarder, identical) != nil {
		t.Fatal("没有记录时原样重写的不按重新上传处理")
	}
	if _, listed := admin.service.permissionDenied.identityOf(relogin); listed {
		t.Fatal("登录签名变了的照旧清除登记")
	}
	if item := reauthPending(onboarder, relogin); item == nil || item.reauthAt.IsZero() {
		t.Fatal("启动对账清除了登记的账户应加入验证队列")
	}
}

// TestAuthFileStampsOwner 只有最近一次装配的生成服务实例在关闭时写出记录：重建生成服务时新实例先装配、旧实例后关闭，旧实例写出的
// 记录在新实例改写认证文件之后就过时了；新实例启动被取消、先于旧实例关闭时写下的记录，旧实例关闭时删除（旧实例之后还改写过认证文件）
func TestAuthFileStampsOwner(t *testing.T) {
	admin, _, _ := reauthTestAdmin(t, "alice@example.com")
	path := filepath.Join(t.TempDir(), authFileStampsName)
	older := authFileStampsOwner.Add(1)
	newer := authFileStampsOwner.Add(1)
	saveAuthFileStamps(path, admin.pool, older, admin.requests)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("旧实例不应写出记录：%v", err)
	}
	saveAuthFileStamps(path, admin.pool, newer, admin.requests)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("最新实例应写出记录：%v", err)
	}
	saveAuthFileStamps(path, admin.pool, older, admin.requests)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("旧实例之后关闭时应删除过时的记录：%v", err)
	}
	saveAuthFileStamps(path, admin.pool, newer, admin.requests)
	stamps := loadAuthFileStamps(path, admin.requests)
	if _, ok := stamps["alice@example.com"]; !ok {
		t.Fatalf("最新实例应写出记录：%v", stamps)
	}
	if loadAuthFileStamps(path, admin.requests) != nil {
		t.Fatal("记录读一次就删除")
	}
}

// TestReauthNotReuploadedCounted 失效账户换上的文件被当成本服务自己的文件（保留了原文件的修改时间）或其他进程回写（只差轮换 Cookie）：
// 面板计入“未按重新上传处理”。原先只有一行日志，面板上什么都看不出来
func TestReauthNotReuploadedCounted(t *testing.T) {
	const rotating, preserved = "rotating@example.com", "preserved@example.com"
	admin, onboarder, _ := reauthDeniedAdmin(t, []string{rotating, preserved})
	pool := admin.pool
	watcher := newTestAccountWatcher(admin)
	later := time.Now().Add(time.Minute)
	scanLoginAt(t, watcher, later)
	exported := withCookie(diskAuthState(t, pool, rotating), "__Secure-1PSIDTS", "user-browser")
	writeAuthFile(t, pool, rotating, exported)
	account, err := pool.Account(preserved)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(account.StoragePath)
	if err != nil {
		t.Fatal(err)
	}
	temporary := account.StoragePath + ".upload"
	if err := os.WriteFile(temporary, data, 0o600); err != nil {
		t.Fatal(err)
	}
	earlier := time.Now().Add(-48 * time.Hour)
	if err := errors.Join(os.Chtimes(temporary, earlier, earlier), os.Rename(temporary, account.StoragePath)); err != nil {
		t.Fatal(err)
	}
	scanLoginAt(t, watcher, later.Add(time.Minute))
	if count := onboarder.status().NotReuploaded; count != 2 {
		t.Fatalf("面板“未按重新上传处理”= %d，日志：%v", count, serviceLogMessages(admin.requests))
	}
}

// TestReauthBulkBacklogVisible 批量重新上传时每轮扫描只载入 40 个：没载入的计入面板“等待载入”，并写一行批量的说明；载入完后归零
func TestReauthBulkBacklogVisible(t *testing.T) {
	ids := make([]string, 0, 50)
	for index := 0; index < 50; index++ {
		ids = append(ids, fmt.Sprintf("bulk%02d@example.com", index))
	}
	admin, onboarder, _ := reauthTestAdmin(t, ids...)
	pool := admin.pool
	markReauthRequired(t, pool, ids...)
	watcher := newTestAccountWatcher(admin)
	later := time.Now().Add(time.Minute)
	scanLoginAt(t, watcher, later)
	for _, id := range ids {
		rewriteAuthFile(t, pool, id)
	}
	scanLoginAt(t, watcher, later.Add(time.Minute))
	if loading := onboarder.status().Loading; loading != 50-loginRefreshPerScan {
		t.Fatalf("等待载入 = %d", loading)
	}
	if !hasLogContaining(admin.requests, "检测到认证文件更新 | 本轮新增 50 个 | 等待载入 50 个") {
		t.Fatalf("缺少批量更新的说明：%v", serviceLogMessages(admin.requests))
	}
	scanLoginAt(t, watcher, later.Add(2*time.Minute))
	if loading := onboarder.status().Loading; loading != 0 {
		t.Fatalf("载入完后等待载入应为 0，得到 %d", loading)
	}
	if reauth := onboarder.status().Reauth; reauth != 50 {
		t.Fatalf("50 个都应加入验证，得到 %d", reauth)
	}
}

// TestReauthOutcomeUpdatedAfterManualVerify 重新上传后验证未通过（仍需登录），之后在管理页点“验证”通过：这条结果改记为已恢复，
// 面板不再计“重新上传后：仍需登录”（原先一直留着并随处理记录保存）
func TestReauthOutcomeUpdatedAfterManualVerify(t *testing.T) {
	const id = "stale@example.com"
	admin, onboarder, login := reauthTestAdmin(t, id)
	markReauthRequired(t, admin.pool, id)
	watcher := newTestAccountWatcher(admin)
	later := time.Now().Add(time.Minute)
	scanLoginAt(t, watcher, later)
	rewriteAuthFile(t, admin.pool, id)
	scanLoginAt(t, watcher, later.Add(time.Minute))
	login.verification = aistudio.LoginVerification{Authenticated: false, Reason: "跳转到登录页"}
	runReauthBatch(t, onboarder)
	if outcomes := onboarder.status().ReauthOutcomes; outcomes[reauthAuthRequired] != 1 {
		t.Fatalf("前提：验证未通过应记为仍需登录，%v", outcomes)
	}
	login.verification = aistudio.LoginVerification{Authenticated: true}
	if _, err := admin.VerifyAccount(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	outcomes := onboarder.status().ReauthOutcomes
	if outcomes[reauthAuthRequired] != 0 || outcomes[reauthRestored] != 1 {
		t.Fatalf("管理页验证通过后应改记为已恢复：%v", outcomes)
	}
	if !hasLogContaining(admin.requests, "重新上传后：已恢复 | 账户="+id, "原因=管理页验证通过") {
		t.Fatalf("缺少改记的日志：%v", serviceLogMessages(admin.requests))
	}
	// 本来就是已恢复的不再写一遍
	before := countLogs(admin.requests, "重新上传后：已恢复 | 账户="+id)
	if _, err := admin.VerifyAccount(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if after := countLogs(admin.requests, "重新上传后：已恢复 | 账户="+id); after != before {
		t.Fatal("已恢复的记录不应重复改记")
	}
}

// TestReauthQueueKeepsDeferral 已在队列里的账户又换了认证文件：新账户的项不动（重试次数与暂缓时间照旧）；重新验证的项在等出错重试的
// 清零、立即重试，只是因固定端口暂停而暂缓的照旧等端口恢复。原先一律清零，暂缓的新账户被提前取出再写一次“暂缓”，重试次数也被清零
func TestReauthQueueKeepsDeferral(t *testing.T) {
	const fresh, retrying, deferred = "fresh@example.com", "retrying@example.com", "deferred@example.com"
	admin, onboarder, _ := reauthTestAdmin(t, fresh, retrying, deferred)
	_ = admin
	until := time.Now().Add(time.Hour)
	onboarder.mu.Lock()
	onboarder.pending[fresh] = &onboardPending{queuedAt: time.Now(), attempts: 1, notBefore: until}
	onboarder.pending[retrying] = &onboardPending{queuedAt: time.Now(), reauthAt: time.Now(), attempts: 2, notBefore: until}
	onboarder.pending[deferred] = &onboardPending{queuedAt: time.Now(), reauthAt: time.Now(), notBefore: until}
	onboarder.mu.Unlock()
	for _, id := range []string{fresh, retrying, deferred} {
		onboarder.queueReauth(id, reauthSourceAuthFile)
	}
	if item := reauthPending(onboarder, fresh); item.attempts != 1 || !item.notBefore.Equal(until) || !item.reauthAt.IsZero() {
		t.Fatalf("新账户的项不应改动：%+v", item)
	}
	if item := reauthPending(onboarder, retrying); item.attempts != 0 || !item.notBefore.IsZero() {
		t.Fatalf("在等出错重试的重新验证应清零、立即重试：%+v", item)
	}
	if item := reauthPending(onboarder, deferred); !item.notBefore.Equal(until) {
		t.Fatalf("因端口暂停暂缓的应照旧等端口恢复：%+v", item)
	}
}
