package app

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// 失效类状态的账户重新上传认证文件（见 account_reupload.go）：403 无权限、需要验证账号、单个模型需要验证等旧判定全部清除，
// 内容没变也按重新上传处理；重新上传后的结果在面板与日志里写明（已恢复、仍需登录、仍需验证、跳过（手动停用）等）

const verifyAccountMessage = "AI Studio GenerateContent 返回 HTTP 403、协议错误码 7: Your account must be verified. Visit https://aistudio.google.com/verify-account"

// reauthDeniedAdmin 返回带 403 无权限登记表的管理端（listed 为登记了 403、带单模型 403 冷却的账户），接入新账户自动处理队列与
// 测试浏览器验证（默认判定已登录）；重新上传后验证通过的账户之后又返回 403 时由队列改记结果
func reauthDeniedAdmin(t *testing.T, listed []string, accountIDs ...string) (*runtimeAdmin, *accountOnboarder, *verifyTestLogin) {
	t.Helper()
	isolateDisableNotes(t)
	admin, _ := deniedWatcherAdmin(t, listed, accountIDs...)
	login := &verifyTestLogin{verification: aistudio.LoginVerification{Authenticated: true}}
	admin.login = login
	onboarder := newTestOnboarder(admin, t.TempDir())
	t.Cleanup(onboarder.flush)
	admin.onboard = onboarder
	admin.service.reauthOnboard.Store(onboarder)
	return admin, onboarder, login
}

// forbiddenCooldowns 返回账户上仍生效的 403 类冷却（单模型 403、自动暂停、需要验证账号的整户暂停与单个模型需要验证）
func forbiddenCooldowns(t *testing.T, pool *aistudio.AccountPool, accountID string) []string {
	t.Helper()
	var reasons []string
	for scope, cooldown := range testStatus(t, pool, accountID).Cooldowns {
		if cooldown.Active(time.Now()) && forbiddenCooldown(scope, cooldown) {
			reasons = append(reasons, scope+"="+cooldown.Reason)
		}
	}
	return reasons
}

// recordVerifyRequired 登记账户在这些模型上返回过需要验证账号的 403
func recordVerifyRequired(admin *runtimeAdmin, accountID string, models ...string) {
	for _, model := range models {
		admin.service.recordPermissionDenied(accountID, model, aistudio.ChannelPlayground, verifyAccountMessage, time.Now(), nil)
	}
}

// TestReauthForbiddenCooldownReupload 403 无权限（登记与单模型 403 冷却）的账户重新上传认证文件：内容变了（同一会话重新导出）
// 与内容完全没变（在官网解开 403 后原样重新上传）都清除登记与冷却、重新认证并加入验证队列；验证通过后恢复调度，面板记为已恢复
func TestReauthForbiddenCooldownReupload(t *testing.T) {
	const changed, unchanged = "changed@example.com", "unchanged@example.com"
	admin, onboarder, _ := reauthDeniedAdmin(t, []string{changed, unchanged})
	pool := admin.pool
	registry := admin.service.permissionDenied
	watcher := newTestAccountWatcher(admin)
	later := time.Now().Add(time.Minute)
	scanLoginAt(t, watcher, later)
	for _, id := range []string{changed, unchanged} {
		if _, listed := registry.identityOf(id); !listed || len(forbiddenCooldowns(t, pool, id)) == 0 {
			t.Fatalf("前提：%s 应登记 403 并在冷却", id)
		}
	}
	generation := pool.AuthGeneration(unchanged)
	reexportAuthFile(t, pool, changed)
	rewriteAuthFile(t, pool, unchanged)
	scanLoginAt(t, watcher, later.Add(time.Minute))
	for _, id := range []string{changed, unchanged} {
		if _, listed := registry.identityOf(id); listed {
			t.Fatalf("%s 重新上传后应清除 403 登记：%+v", id, registry.list())
		}
		if reasons := forbiddenCooldowns(t, pool, id); len(reasons) != 0 {
			t.Fatalf("%s 重新上传后应解除 403 冷却，仍有：%v", id, reasons)
		}
		if item := reauthPending(onboarder, id); item == nil || item.reauthAt.IsZero() {
			t.Fatalf("%s 应加入队列重新验证", id)
		}
	}
	if pool.AuthGeneration(unchanged) == generation {
		t.Fatal("原样重新上传也应重新认证（推进认证代际）")
	}
	if !hasLogContaining(admin.requests, "按重新上传处理", "账户="+unchanged, "状态=403 无权限冷却") {
		t.Fatalf("应写明原样重新上传按重新上传处理：%v", serviceLogMessages(admin.requests))
	}
	if batch := runReauthBatch(t, onboarder); len(batch) != 2 {
		t.Fatalf("本批 = %v", batch)
	}
	for _, id := range []string{changed, unchanged} {
		record := onboarder.records[id]
		if record.Source != onboardSourceReauth || record.Outcome != reauthRestored {
			t.Fatalf("%s 验证通过应记为已恢复：%+v", id, record)
		}
		if !hasLogContaining(admin.requests, "认证文件更新后 | 重新上传后：已恢复 | 账户="+id) {
			t.Fatalf("%s 缺少重新上传后的结果日志：%v", id, serviceLogMessages(admin.requests))
		}
	}
	if counts := onboarder.status().ReauthOutcomes; counts[reauthRestored] != 2 {
		t.Fatalf("面板重新上传结果计数 = %v", counts)
	}
}

// TestReauthVerifyPauseReupload 需要验证账号整户暂停的账户（两个模型返回过 verify-account，Worker 已释放、不参与调度）
// 在官网验证后原样重新上传认证文件：清除登记与整户暂停，账户重新可调度，加入验证队列，验证通过后记为已恢复
func TestReauthVerifyPauseReupload(t *testing.T) {
	const id = "paused@example.com"
	admin, onboarder, _ := reauthDeniedAdmin(t, nil, id)
	pool := admin.pool
	recordVerifyRequired(admin, id, testRuntimeModel, "other-model")
	coolModel(t, pool, id, "", verifyRequiredPause, verifyRequiredPauseReason)
	if !pool.VerifyRequiredPaused(id) {
		t.Fatal("前提：账户应整户暂停")
	}
	watcher := newTestAccountWatcher(admin)
	later := time.Now().Add(time.Minute)
	scanLoginAt(t, watcher, later)
	rewriteAuthFile(t, pool, id)
	scanLoginAt(t, watcher, later.Add(time.Minute))
	if pool.VerifyRequiredPaused(id) {
		t.Fatal("重新上传后应解除需要验证账号的整户暂停")
	}
	if _, listed := admin.service.permissionDenied.identityOf(id); listed {
		t.Fatal("重新上传后应清除需要验证账号的登记")
	}
	if reason := pool.UnschedulableSet([]string{id})[id]; reason != "" {
		t.Fatalf("重新上传后账户应可调度，仍不可调度：%s", reason)
	}
	if !hasLogContaining(admin.requests, "按重新上传处理", "账户="+id, "状态=需要验证账号（整户暂停）") {
		t.Fatalf("应写明账户状态：%v", serviceLogMessages(admin.requests))
	}
	runReauthBatch(t, onboarder)
	if record := onboarder.records[id]; record.Outcome != reauthRestored {
		t.Fatalf("验证通过应记为已恢复：%+v", record)
	}
}

// TestReauthModelVerifyCooldownReupload 只在一个模型上要求验证（单个模型需要验证的冷却）的账户原样重新上传认证文件：
// 清除这个模型的暂停与登记，加入验证队列
func TestReauthModelVerifyCooldownReupload(t *testing.T) {
	const id = "model@example.com"
	admin, onboarder, _ := reauthDeniedAdmin(t, nil, id)
	pool := admin.pool
	recordVerifyRequired(admin, id, testRuntimeModel)
	coolModel(t, pool, id, testRuntimeModel, verifyRequiredPause, modelVerifyRequiredReason)
	if until, ok := pool.ModelVerifyCooled(id, testRuntimeModel); !ok || until.IsZero() {
		t.Fatal("前提：模型应在单个模型需要验证的冷却里")
	}
	if label := admin.reauthFailure(loginSummaryOf(t, pool, id)); label != "单个模型需要验证" {
		t.Fatalf("失效状态 = %q，期望 单个模型需要验证", label)
	}
	watcher := newTestAccountWatcher(admin)
	later := time.Now().Add(time.Minute)
	scanLoginAt(t, watcher, later)
	rewriteAuthFile(t, pool, id)
	scanLoginAt(t, watcher, later.Add(time.Minute))
	if _, ok := pool.ModelVerifyCooled(id, testRuntimeModel); ok {
		t.Fatal("重新上传后应解除单个模型需要验证的冷却")
	}
	if _, listed := admin.service.permissionDenied.identityOf(id); listed {
		t.Fatal("重新上传后应清除登记")
	}
	if item := reauthPending(onboarder, id); item == nil || item.reauthAt.IsZero() {
		t.Fatal("应加入队列重新验证")
	}
}

// TestReauthHealthyUnchangedHintOnly 账户本来就健康（就绪；只是手动停用的也一样）、原样重新上传同一份认证文件：不重新认证、
// 不加入队列，只写一行“内容与当前登录状态相同”的提示并计入面板
func TestReauthHealthyUnchangedHintOnly(t *testing.T) {
	const ready, manual = "ready@example.com", "manual@example.com"
	admin, onboarder, login := reauthTestAdmin(t, ready, manual)
	pool := admin.pool
	disableTestAccount(t, pool, manual)
	accountDisableNotes.set(manual, "管理页停用", false)
	watcher := newTestAccountWatcher(admin)
	later := time.Now().Add(time.Minute)
	scanLoginAt(t, watcher, later)
	generations := map[string]uint64{ready: pool.AuthGeneration(ready), manual: pool.AuthGeneration(manual)}
	rewriteAuthFile(t, pool, ready)
	rewriteAuthFile(t, pool, manual)
	scanLoginAt(t, watcher, later.Add(time.Minute))
	for id, state := range map[string]string{ready: "正常", manual: "手动停用"} {
		if pool.AuthGeneration(id) != generations[id] || reauthPending(onboarder, id) != nil {
			t.Fatalf("%s 健康账户原样重新上传不应重新认证或加入队列", id)
		}
		if !hasLogContaining(admin.requests, "认证文件已更新，但内容与当前登录状态相同，未重新认证", "账户="+id, "状态="+state) {
			t.Fatalf("%s 应写一行提示：%v", id, serviceLogMessages(admin.requests))
		}
	}
	if testStatus(t, pool, manual).Enabled || login.calls.Load() != 0 {
		t.Fatal("手动停用的账户应保持停用、不验证")
	}
	if onboarder.status().Unchanged != 2 {
		t.Fatalf("面板认证文件未变化 = %d，期望 2", onboarder.status().Unchanged)
	}
}

// TestReauthFailingWriteBackIgnored 需要登录的账户的认证文件被共用账户目录的另一个进程回写了轮换 Cookie 与 AI Studio 的 localStorage：
// 不是重新上传，账户仍需要登录、原因不变、不加入队列
func TestReauthFailingWriteBackIgnored(t *testing.T) {
	const id = "a@example.com"
	admin, onboarder, _ := reauthTestAdmin(t, id)
	pool := admin.pool
	markReauthRequired(t, pool, id)
	account, err := pool.Account(id)
	if err != nil {
		t.Fatal(err)
	}
	accounts, failures := aistudio.NewAccountStore(filepath.Dir(account.Directory)).LoadTolerant()
	if len(failures) > 0 {
		t.Fatal(failures)
	}
	other := aistudio.NewAccountPool(accounts, 1)
	t.Cleanup(other.WaitBackground)
	watcher := newTestAccountWatcher(admin)
	later := time.Now().Add(time.Minute)
	scanLoginAt(t, watcher, later)
	lease, err := other.AcquireAccount(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if err := lease.MergeSetCookieHeaders(
		[]string{"__Secure-1PSIDTS=other-process; Domain=.google.com; Path=/; Secure", "SIDCC=other-process; Domain=.google.com; Path=/; Secure"},
		"https://aistudio.google.com/", time.Now(),
	); err != nil {
		t.Fatal(errors.Join(err, lease.Release()))
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
	scanLoginAt(t, watcher, later.Add(time.Minute))
	if written, err := other.MergeBrowserLocalStorage(id, other.AuthGeneration(id), aistudio.StorageOrigin{
		Origin: aistudio.BrowserLocalStorageOrigin, LocalStorage: []aistudio.StorageItem{{Name: "aiStudioUserPreference", Value: "other"}},
	}); err != nil || !written {
		t.Fatalf("另一个进程应写回 localStorage: written=%t err=%v", written, err)
	}
	scanLoginAt(t, watcher, later.Add(2*time.Minute))
	if status := testStatus(t, pool, id); status.State != aistudio.AccountAuthRequired || status.Message != reauthOldReason {
		t.Fatalf("另一个进程回写不应重新认证：state=%s message=%q", status.State, status.Message)
	}
	if reauthPending(onboarder, id) != nil || hasLogContaining(admin.requests, "按重新上传处理") {
		t.Fatalf("另一个进程回写不应加入队列：%v", serviceLogMessages(admin.requests))
	}
}

// TestReauthOutcomes 重新上传后的结果：验证未通过为仍需登录（这次的原因）；排队期间又返回需要验证账号的 403 的验证通过后为仍需验证；
// 验证通过记为已恢复之后又返回 403 时改记为仍有 403；手动停用的只换认证，记为跳过（手动停用）；自动验证关闭时记为已重新载入、未验证
func TestReauthOutcomes(t *testing.T) {
	const failed, queued, restored, manual, off = "failed@example.com", "queued@example.com", "restored@example.com",
		"manual@example.com", "off@example.com"
	admin, onboarder, login := reauthDeniedAdmin(t, nil, failed, queued, restored, manual, off)
	pool := admin.pool

	// 验证未通过：仍需登录
	login.verification = aistudio.LoginVerification{Reason: "跳转到登录页", PageURL: "accounts.google.com/v3/signin"}
	markReauthRequired(t, pool, failed)
	onboarder.queueReauth(failed, reauthSourceSameFile)
	runReauthBatch(t, onboarder)
	if record := onboarder.records[failed]; record.Outcome != reauthAuthRequired || !strings.Contains(record.Detail, "跳转到登录页") {
		t.Fatalf("验证未通过应记为仍需登录：%+v", record)
	}
	if !hasLogContaining(admin.requests, "认证文件更新后 | 重新上传后：仍需登录 | 账户="+failed, "原因=跳转到登录页") {
		t.Fatalf("缺少仍需登录的日志：%v", serviceLogMessages(admin.requests))
	}

	// 排队期间又返回需要验证账号的 403：验证通过（登录有效）也记为仍需验证
	login.verification = aistudio.LoginVerification{Authenticated: true}
	onboarder.queueReauth(queued, reauthSourceAuthFile)
	time.Sleep(2 * time.Millisecond)
	recordVerifyRequired(admin, queued, testRuntimeModel)
	runReauthBatch(t, onboarder)
	if record := onboarder.records[queued]; record.Outcome != reauthVerifyRequired || !strings.Contains(record.Detail, "must be verified") {
		t.Fatalf("排队期间又返回需要验证账号的 403 应记为仍需验证：%+v", record)
	}

	// 验证通过记为已恢复，之后的请求又返回 403：改记为仍有 403
	onboarder.queueReauth(restored, reauthSourceAuthFile)
	runReauthBatch(t, onboarder)
	if record := onboarder.records[restored]; record.Outcome != reauthRestored {
		t.Fatalf("验证通过应记为已恢复：%+v", record)
	}
	time.Sleep(2 * time.Millisecond)
	admin.service.recordPermissionDenied(restored, testRuntimeModel, aistudio.ChannelPlayground, "The caller does not have permission", time.Now(), nil)
	if record := onboarder.records[restored]; record.Outcome != reauthForbidden || !strings.Contains(record.Detail, "does not have permission") {
		t.Fatalf("验证通过后又返回 403 应改记为仍有 403：%+v", record)
	}
	if recent := onboarder.status().Recent; len(recent) == 0 || recent[0].AccountID != restored || recent[0].Outcome != reauthForbidden ||
		recent[0].Source != onboardSourceReauth {
		t.Fatalf("最近处理应加一条仍有 403：%+v", recent)
	}
	if !hasLogContaining(admin.requests, "认证文件更新后 | 重新上传后：仍有 403 | 账户="+restored, "来源=验证通过后的请求") {
		t.Fatalf("缺少仍有 403 的日志：%v", serviceLogMessages(admin.requests))
	}

	// 验证未通过的记录之后再返回 403 不改记（只改已恢复的）；已恢复之后返回需要验证账号的 403 改记为仍需验证
	admin.service.recordPermissionDenied(failed, testRuntimeModel, aistudio.ChannelPlayground, verifyAccountMessage, time.Now(), nil)
	if record := onboarder.records[failed]; record.Outcome != reauthAuthRequired {
		t.Fatalf("仍需登录的记录不应被之后的 403 改记：%+v", record)
	}
	onboarder.queueReauth(manual, reauthSourceAuthFile)
	runReauthBatch(t, onboarder)
	if record := onboarder.records[manual]; record.Outcome != reauthRestored {
		t.Fatalf("验证通过应记为已恢复：%+v", record)
	}
	time.Sleep(2 * time.Millisecond)
	recordVerifyRequired(admin, manual, testRuntimeModel)
	if record := onboarder.records[manual]; record.Outcome != reauthVerifyRequired || !strings.Contains(record.Detail, "must be verified") {
		t.Fatalf("验证通过后又返回需要验证账号的 403 应改记为仍需验证：%+v", record)
	}
	admin.service.permissionDenied.clearAccount(manual)

	// 手动停用：只换认证、保持停用，记为跳过（手动停用）
	disableTestAccount(t, pool, manual)
	accountDisableNotes.set(manual, "管理页停用", false)
	coolModel(t, pool, manual, testRuntimeModel, time.Hour, forbiddenModelReasonPrefix+"The caller does not have permission")
	watcher := newTestAccountWatcher(admin)
	later := time.Now().Add(time.Minute)
	scanLoginAt(t, watcher, later)
	rewriteAuthFile(t, pool, manual)
	scanLoginAt(t, watcher, later.Add(time.Minute))
	if len(forbiddenCooldowns(t, pool, manual)) != 0 || testStatus(t, pool, manual).Enabled || reauthPending(onboarder, manual) != nil {
		t.Fatal("手动停用的账户重新上传后应清除 403 冷却、保持停用、不验证")
	}
	if record := onboarder.records[manual]; record.Outcome != reauthManualDisabled || record.Source != onboardSourceReauth {
		t.Fatalf("手动停用的应记为跳过（手动停用）：%+v", record)
	}
	if !hasLogContaining(admin.requests, "认证文件更新后 | 重新上传后：跳过（手动停用） | 账户="+manual) {
		t.Fatalf("缺少跳过（手动停用）的日志：%v", serviceLogMessages(admin.requests))
	}

	// 自动验证关闭：只重新载入，记为已重新载入、未验证
	onboarder.mu.Lock()
	onboarder.policy.AutoVerify = false
	onboarder.mu.Unlock()
	onboarder.queueReauth(off, reauthSourceAuthFile)
	if record := onboarder.records[off]; record.Outcome != reauthVerifyOff {
		t.Fatalf("自动验证关闭时应记为已重新载入、未验证：%+v", record)
	}

	counts := onboarder.status().ReauthOutcomes
	for outcome, want := range map[string]int{
		reauthAuthRequired: 1, reauthVerifyRequired: 1, reauthForbidden: 1, reauthManualDisabled: 1, reauthVerifyOff: 1,
	} {
		if counts[outcome] != want {
			t.Fatalf("面板重新上传结果计数 = %v，%s 期望 %d", counts, outcome, want)
		}
	}
}

// TestReauthFailureStates 失效类状态的判断：需要登录、自动停用、组织停用、403 登记、各种 403 类冷却与疑似登录失效都算；
// 健康账户与只是手动停用的账户不算
func TestReauthFailureStates(t *testing.T) {
	ids := []string{"healthy@example.com", "auth@example.com", "auto@example.com", "manual@example.com",
		"listed@example.com", "pause@example.com", "suspect@example.com", "expired@example.com"}
	admin, _, _ := reauthDeniedAdmin(t, nil, ids...)
	pool := admin.pool
	markReauthRequired(t, pool, "auth@example.com")
	disableTestAccount(t, pool, "auto@example.com")
	accountDisableNotes.set("auto@example.com", "新账户自动处理验证未通过（跳转到登录页）", true)
	disableTestAccount(t, pool, "manual@example.com")
	accountDisableNotes.set("manual@example.com", "管理页停用", false)
	admin.service.recordPermissionDenied("listed@example.com", testRuntimeModel, aistudio.ChannelPlayground, "denied", time.Now(), nil)
	coolModel(t, pool, "pause@example.com", "", time.Hour, verifyRequiredPauseReason)
	coolModel(t, pool, "suspect@example.com", "", time.Hour, authSuspectReasonPrefix+"HTTP 401")
	coolModel(t, pool, "expired@example.com", testRuntimeModel, time.Millisecond, forbiddenModelReasonPrefix+"denied")
	time.Sleep(5 * time.Millisecond)
	want := map[string]string{
		"healthy@example.com": "", "auth@example.com": "需要登录", "auto@example.com": "自动停用", "manual@example.com": "",
		"listed@example.com": "403 无权限", "pause@example.com": "需要验证账号（整户暂停）", "suspect@example.com": "疑似登录失效",
		"expired@example.com": "",
	}
	for _, id := range ids {
		if got := admin.reauthFailure(loginSummaryOf(t, pool, id)); got != want[id] {
			t.Fatalf("%s 失效状态 = %q，期望 %q", id, got, want[id])
		}
	}
}

// TestCompareAuthFile 认证文件与内存登录状态的关系：完全相同为相同；只是轮换 Cookie 比内存旧或少了轮换 Cookie、localStorage 项为较旧
// （按其他进程回写处理）；多了或换新了轮换 Cookie、多了 localStorage 项、只改了 localStorage 的值为回写；其他 Cookie 变了为内容变了。
// 文件逐字节就是本服务读入或写过的那一份（known）时，只差轮换 Cookie 与 localStorage 的一律为相同（原样重新上传），内容变了的仍为内容变了
func TestCompareAuthFile(t *testing.T) {
	expiring := func(state aistudio.StorageState, name string, value string, expires float64) aistudio.StorageState {
		state = withCookie(state, name, value)
		state.Cookies[len(state.Cookies)-1].Expires = expires
		return state
	}
	withItem := func(state aistudio.StorageState, name string, value string) aistudio.StorageState {
		items := map[string]string{}
		for key, existing := range browserLocalStorageItems(state) {
			items[key] = existing
		}
		items[name] = value
		origins := make([]aistudio.StorageOrigin, 0, len(state.Origins)+1)
		for _, origin := range state.Origins {
			if origin.Origin != aistudio.BrowserLocalStorageOrigin {
				origins = append(origins, origin)
			}
		}
		origin := aistudio.StorageOrigin{Origin: aistudio.BrowserLocalStorageOrigin}
		for key, existing := range items {
			origin.LocalStorage = append(origin.LocalStorage, aistudio.StorageItem{Name: key, Value: existing})
		}
		state.Origins = append(origins, origin)
		return state
	}
	memory := withItem(expiring(authTestState(t, "A", false), "__Secure-1PSIDTS", "new", 2000), "pref", "v2")
	cases := []struct {
		name string
		disk aistudio.StorageState
		want authFileChange
	}{
		{"完全相同", memory, authFileSame},
		{"轮换 Cookie 更旧", expiring(memory, "__Secure-1PSIDTS", "old", 1000), authFileOlder},
		{"轮换 Cookie 换了值、看不出新旧", withCookie(memory, "__Secure-1PSIDTS", "x"), authFileWriteBack},
		{"少了轮换 Cookie", withoutCookie(memory, "__Secure-1PSIDTS"), authFileOlder},
		{"轮换 Cookie 更新", expiring(memory, "__Secure-1PSIDTS", "newer", 3000), authFileWriteBack},
		{"多了轮换 Cookie", expiring(memory, "SIDCC", "added", 3000), authFileWriteBack},
		{"多了 localStorage 项", withItem(memory, "added", "x"), authFileWriteBack},
		{"只改了 localStorage 的值", withItem(memory, "pref", "v3"), authFileWriteBack},
		{"其他 Cookie 变了", withCookie(memory, "NID", "changed"), authFileChanged},
		{"换了登录", authTestState(t, "B", false), authFileChanged},
	}
	// 少了轮换 Cookie、localStorage 的值也不同：较旧
	cases = append(cases, struct {
		name string
		disk aistudio.StorageState
		want authFileChange
	}{"较早版本少了轮换 Cookie、localStorage 值不同", withItem(withoutCookie(memory, "__Secure-1PSIDTS"), "pref", "v1"), authFileOlder})
	// 少了 localStorage 项且轮换 Cookie 更旧：较旧
	older := expiring(memory, "__Secure-1PSIDTS", "old", 1000)
	older.Origins = nil
	cases = append(cases, struct {
		name string
		disk aistudio.StorageState
		want authFileChange
	}{"较早版本少了 localStorage", older, authFileOlder})
	for _, testCase := range cases {
		if got := compareAuthFile(testCase.disk, memory, false); got != testCase.want {
			t.Fatalf("%s：compareAuthFile = %d，期望 %d", testCase.name, got, testCase.want)
		}
		known := authFileSame
		if testCase.want == authFileChanged {
			known = authFileChanged
		}
		if got := compareAuthFile(testCase.disk, memory, true); got != known {
			t.Fatalf("%s（本服务读入或写过的那一份）：compareAuthFile = %d，期望 %d", testCase.name, got, known)
		}
	}
}

// loginSummaryOf 返回账户池里账户的轻量状态
func loginSummaryOf(t *testing.T, pool *aistudio.AccountPool, accountID string) aistudio.AccountLoginSummary {
	t.Helper()
	for _, summary := range pool.LoginSummaries() {
		if summary.ID == accountID {
			return summary
		}
	}
	t.Fatalf("账户 %s 不存在", accountID)
	return aistudio.AccountLoginSummary{}
}

// withoutCookie 返回去掉一个 Cookie 后的登录状态
func withoutCookie(state aistudio.StorageState, name string) aistudio.StorageState {
	cookies := make([]aistudio.StateCookie, 0, len(state.Cookies))
	for _, cookie := range state.Cookies {
		if cookie.Name != name {
			cookies = append(cookies, cookie)
		}
	}
	state.Cookies = cookies
	return state
}

// TestReauthRestrictedVerifiedNotSkipped 组织停用（受限）的账户重新上传后以验证结果为准：重新验证不按组织停用跳过，照常启动验证；
// 验证仍判定受限时记为仍受限，验证通过时记为已恢复
func TestReauthRestrictedVerifiedNotSkipped(t *testing.T) {
	const restricted, reopened = "restricted@example.com", "reopened@example.com"
	admin, onboarder, login := reauthDeniedAdmin(t, nil, restricted, reopened)
	pool := admin.pool
	for _, id := range []string{restricted, reopened} {
		if _, err := pool.MarkOrgDisabled(id, aistudio.RestrictionOrgDisabled); err != nil {
			t.Fatal(err)
		}
	}
	if label := admin.reauthFailure(loginSummaryOf(t, pool, restricted)); label == "" {
		t.Fatal("组织停用的账户应算失效状态")
	}
	// 仍受限：验证判定组织停用
	login.verification = aistudio.LoginVerification{OrgDisabled: true, Restriction: aistudio.RestrictionOrgDisabled, Reason: aistudio.RestrictionReason(aistudio.RestrictionOrgDisabled)}
	onboarder.queueReauth(restricted, reauthSourceAuthFile)
	runReauthBatch(t, onboarder)
	if login.calls.Load() != 1 {
		t.Fatalf("重新上传后的验证不应按组织停用跳过，验证 %d 次", login.calls.Load())
	}
	if record := onboarder.records[restricted]; record.Outcome != reauthRestricted {
		t.Fatalf("验证仍判定受限应记为仍受限：%+v", record)
	}
	// 组织重新开放：验证通过，恢复
	login.verification = aistudio.LoginVerification{Authenticated: true}
	onboarder.queueReauth(reopened, reauthSourceAuthFile)
	runReauthBatch(t, onboarder)
	if login.calls.Load() != 2 {
		t.Fatalf("应再验证一次，共 %d 次", login.calls.Load())
	}
	if record := onboarder.records[reopened]; record.Outcome != reauthRestored {
		t.Fatalf("验证通过应记为已恢复：%+v", record)
	}
}

// TestReauthSameLoginKeepsNewerRotatingCookies 403 冷却中的账户本服务已收到并写回了更新的轮换 Cookie，之后原样重新上传最初那份文件
// （轮换 Cookie 比当前的旧）：同样按重新上传处理，但沿用当前登录状态（新的轮换 Cookie 不回退），磁盘也写回当前的
func TestReauthSameLoginKeepsNewerRotatingCookies(t *testing.T) {
	const id = "rotated@example.com"
	admin, onboarder, _ := reauthDeniedAdmin(t, []string{id})
	pool := admin.pool
	original := diskAuthState(t, pool, id)
	watcher := newTestAccountWatcher(admin)
	later := time.Now().Add(time.Minute)
	scanLoginAt(t, watcher, later)
	lease, err := pool.AcquireAccount(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	expires := time.Now().Add(365 * 24 * time.Hour).UTC().Format(time.RFC1123)
	if err := lease.MergeSetCookieHeaders(
		[]string{"__Secure-1PSIDTS=service-newer; Domain=.google.com; Path=/; Secure; Expires=" + expires}, "https://aistudio.google.com/", time.Now(),
	); err != nil {
		t.Fatal(errors.Join(err, lease.Release()))
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
	scanLoginAt(t, watcher, later.Add(time.Minute))
	if reauthPending(onboarder, id) != nil {
		t.Fatal("本服务自己写回不应按重新上传处理")
	}
	writeAuthFile(t, pool, id, original)
	scanLoginAt(t, watcher, later.Add(2*time.Minute))
	if item := reauthPending(onboarder, id); item == nil || item.reauthAt.IsZero() {
		t.Fatalf("原样重新上传最初的文件应按重新上传处理：%v", serviceLogMessages(admin.requests))
	}
	if len(forbiddenCooldowns(t, pool, id)) != 0 {
		t.Fatal("应解除 403 冷却")
	}
	memory, _ := pool.StorageStateOf(id)
	if value, ok := memory.CookieValue("__Secure-1PSIDTS", "https://aistudio.google.com/", time.Now()); !ok || value != "service-newer" {
		t.Fatalf("应沿用当前登录状态里更新的轮换 Cookie，内存里为 %q", value)
	}
	if value, ok := diskAuthState(t, pool, id).CookieValue("__Secure-1PSIDTS", "https://aistudio.google.com/", time.Now()); !ok || value != "service-newer" {
		t.Fatalf("磁盘应写回当前登录状态，磁盘里为 %q", value)
	}
}

// TestReauthSameLoginKeepsCookiesMergedWhileWaiting 原样重新上传按当前登录状态重新认证时，等账户空闲期间正在进行的请求又合并了
// 轮换 Cookie：取得租约后写回的是租约内的最新状态，不回退到等待之前取的那一份
func TestReauthSameLoginKeepsCookiesMergedWhileWaiting(t *testing.T) {
	const id = "busy@example.com"
	admin, _, _ := reauthDeniedAdmin(t, []string{id})
	pool := admin.pool
	held, err := pool.AcquireFor(context.Background(), aistudio.AccountSelection{AccountID: id})
	if err != nil {
		t.Fatal(err)
	}
	summary := loginSummaryOf(t, pool, id)
	done := make(chan bool, 1)
	account, err := pool.Account(id)
	if err != nil {
		t.Fatal(err)
	}
	_, digest, err := aistudio.LoadStorageStateDigest(account.StoragePath)
	if err != nil {
		t.Fatal(err)
	}
	go func() { done <- admin.applyLoginUpdateMode(admin.lifecycle, summary, nil, &digest) == loginApplied }()
	// 重新认证已取了当前状态、正在等账户空闲（这次请求的租约还没释放）
	time.Sleep(200 * time.Millisecond)
	expires := time.Now().Add(365 * 24 * time.Hour).UTC().Format(time.RFC1123)
	if err := held.MergeSetCookieHeaders(
		[]string{"__Secure-1PSIDTS=merged-while-waiting; Domain=.google.com; Path=/; Secure; Expires=" + expires},
		"https://aistudio.google.com/", time.Now(),
	); err != nil {
		t.Fatal(errors.Join(err, held.Release()))
	}
	if err := held.Release(); err != nil {
		t.Fatal(err)
	}
	select {
	case applied := <-done:
		if !applied {
			t.Fatal("等到账户空闲后应完成重新认证")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("重新认证没有结束")
	}
	memory, _ := pool.StorageStateOf(id)
	if value, ok := memory.CookieValue("__Secure-1PSIDTS", "https://aistudio.google.com/", time.Now()); !ok || value != "merged-while-waiting" {
		t.Fatalf("应写回租约内的最新状态，内存里为 %q", value)
	}
	if len(forbiddenCooldowns(t, pool, id)) != 0 {
		t.Fatal("应解除 403 冷却")
	}
}
