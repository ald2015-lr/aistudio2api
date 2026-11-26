package app

import (
	"context"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// 已有账户换上新的认证文件后自动重新认证并验证（见 account_watch.go 的 detectLoginUpdates、account_reupload.go 的 reauthFailure
// 与 onboarding.go 的 queueReauth）

const reauthOldReason = "AI Studio GetAiStudioBenefitTier 返回 HTTP 401"

// reauthTestAdmin 返回接入新账户自动处理队列与测试浏览器验证（默认判定已登录）的管理端；账户都是启用的
func reauthTestAdmin(t *testing.T, accountIDs ...string) (*runtimeAdmin, *accountOnboarder, *verifyTestLogin) {
	t.Helper()
	isolateDisableNotes(t)
	admin, _, _ := enableTestAdmin(t, accountIDs...)
	login := &verifyTestLogin{verification: aistudio.LoginVerification{Authenticated: true}}
	admin.login = login
	onboarder := newTestOnboarder(admin, t.TempDir())
	t.Cleanup(onboarder.flush)
	admin.onboard = onboarder
	return admin, onboarder, login
}

// markReauthRequired 把账户标为需要登录，原因为 reauthOldReason（目录同步 401 留下的旧原因）
func markReauthRequired(t *testing.T, pool *aistudio.AccountPool, accountIDs ...string) {
	t.Helper()
	for _, accountID := range accountIDs {
		if err := pool.MarkAuthRequired(accountID, reauthOldReason); err != nil {
			t.Fatal(err)
		}
	}
}

// writeAuthFile 外部（不经本服务）把账户的认证文件换成 state
func writeAuthFile(t *testing.T, pool *aistudio.AccountPool, accountID string, state aistudio.StorageState) {
	t.Helper()
	account, err := pool.Account(accountID)
	if err != nil {
		t.Fatal(err)
	}
	if err := aistudio.WriteStorageState(account.StoragePath, state); err != nil {
		t.Fatal(err)
	}
}

// diskAuthState 读账户磁盘上的认证文件
func diskAuthState(t *testing.T, pool *aistudio.AccountPool, accountID string) aistudio.StorageState {
	t.Helper()
	account, err := pool.Account(accountID)
	if err != nil {
		t.Fatal(err)
	}
	state, err := aistudio.LoadStorageState(account.StoragePath)
	if err != nil {
		t.Fatal(err)
	}
	return state
}

// reexportAuthFile 外部在同一登录会话里重新导出认证文件：登录签名 Cookie 不变，PSIDTS、PSID 与 NID 换了
func reexportAuthFile(t *testing.T, pool *aistudio.AccountPool, accountID string) {
	t.Helper()
	state := diskAuthState(t, pool, accountID)
	writeAuthFile(t, pool, accountID,
		withCookie(withCookie(withCookie(state, "__Secure-1PSIDTS", "reexport"), "__Secure-1PSID", "reexport"), "NID", "reexport"))
}

// rewriteAuthFile 外部把同一份认证文件原样重新上传（只有修改时间变了）
func rewriteAuthFile(t *testing.T, pool *aistudio.AccountPool, accountID string) {
	t.Helper()
	writeAuthFile(t, pool, accountID, diskAuthState(t, pool, accountID))
}

// reauthPending 返回账户在队列中的待处理项（副本）；不在队列中时为 nil
func reauthPending(onboarder *accountOnboarder, accountID string) *onboardPending {
	onboarder.mu.Lock()
	defer onboarder.mu.Unlock()
	item := onboarder.pending[accountID]
	if item == nil {
		return nil
	}
	copied := *item
	return &copied
}

// runReauthBatch 立即处理一批（不等攒批），返回本批账户
func runReauthBatch(t *testing.T, onboarder *accountOnboarder) []string {
	t.Helper()
	batch, policy := onboarder.takeBatch(time.Now())
	if len(batch) > 0 {
		onboarder.processBatch(context.Background(), batch, policy)
	}
	return batch
}

// scanLoginAt 只做目录扫描里认证文件更新的那一部分，按 now 判断文件是否已稳定（本服务自己写的文件不能用 settleAuthFile 改修改时间），
// 并等后台载入结束
func scanLoginAt(t *testing.T, watcher *accountWatcher, now time.Time) {
	t.Helper()
	summaries := watcher.admin.pool.LoginSummaries()
	known := make(map[string]aistudio.AccountLoginSummary, len(summaries))
	for _, summary := range summaries {
		known[summary.ID] = summary
	}
	watcher.detectLoginUpdates(summaries, now)
	watcher.startLoginRefresh(watcher.admin.lifecycle, known)
	waitFor(t, "后台载入认证文件更新结束", func() bool { return !watcher.refreshing.Load() })
}

// TestReauthUploadRepro 复现原问题：需要登录的账户（旧原因是目录同步 401）换上新的认证文件后，同一会话重新导出的原先完全没有反应，
// 换了登录的只载入、不验证，原样重新上传同一份的什么都不做。现在三种都清掉旧原因、恢复就绪并加入验证队列（原样重新上传的
// 沿用当前登录状态重新认证，日志写明“按重新上传处理”）
func TestReauthUploadRepro(t *testing.T) {
	const same, fresh, dup = "same@example.com", "fresh@example.com", "dup@example.com"
	admin, onboarder, _ := reauthTestAdmin(t, same, fresh, dup)
	pool := admin.pool
	markReauthRequired(t, pool, same, fresh, dup)
	watcher := newTestAccountWatcher(admin)
	later := time.Now().Add(time.Minute)
	scanLoginAt(t, watcher, later)
	if onboarder.status().Pending != 0 || onboarder.status().Unchanged != 0 {
		t.Fatal("第一次扫描只记基准，不应加入队列或提示")
	}
	generation := pool.AuthGeneration(dup)

	reexportAuthFile(t, pool, same)
	writeAuthFile(t, pool, fresh, authTestState(t, "NEW", false))
	rewriteAuthFile(t, pool, dup)
	scanLoginAt(t, watcher, later.Add(time.Minute))

	for _, id := range []string{same, fresh, dup} {
		status := testStatus(t, pool, id)
		if status.State == aistudio.AccountAuthRequired || strings.Contains(status.Message, "GetAiStudioBenefitTier") {
			t.Fatalf("%s 重新上传认证文件后应清掉旧原因并恢复：state=%s message=%q", id, status.State, status.Message)
		}
		item := reauthPending(onboarder, id)
		if item == nil || item.reauthAt.IsZero() {
			t.Fatalf("%s 应加入队列重新验证：%+v", id, item)
		}
	}
	for _, id := range []string{same, fresh} {
		if !hasLogContaining(admin.requests, "认证文件更新后自动验证 | 已加入队列", "账户="+id, "来源=认证文件更新") {
			t.Fatalf("%s 缺少加入队列的日志：%v", id, serviceLogMessages(admin.requests))
		}
	}
	if memoryDigest(t, pool, same) != storageStateDigest(diskAuthState(t, pool, same)) {
		t.Fatal("同一会话重新导出的认证文件应载入内存")
	}
	if !hasLogContaining(admin.requests, "认证文件已重新上传，内容与当前登录状态相同；账户处于失效状态，按重新上传处理", "账户="+dup, "状态=需要登录") ||
		!hasLogContaining(admin.requests, "认证文件已重新上传（内容与当前登录状态相同），已清除旧判定并恢复调度 | 账户="+dup) ||
		!hasLogContaining(admin.requests, "认证文件更新后自动验证 | 已加入队列 | 账户="+dup+" | 来源=认证文件重新上传（内容相同）") {
		t.Fatalf("原样重新上传应按重新上传处理并写明：%v", serviceLogMessages(admin.requests))
	}
	if pool.AuthGeneration(dup) == generation {
		t.Fatal("原样重新上传也应重新认证（推进认证代际）")
	}
	onboard := onboarder.status()
	if onboard.Unchanged != 0 || onboard.Reauth != 3 || onboard.Pending != 3 || onboard.NextBatchSeconds != 0 {
		t.Fatalf("面板状态 = unchanged %d reauth %d pending %d next %d，期望 0、3、3、0",
			onboard.Unchanged, onboard.Reauth, onboard.Pending, onboard.NextBatchSeconds)
	}
}

// TestReauthVerifyPassRestores 重新验证通过：恢复就绪、记为验证通过，最近处理记录标为认证文件更新，日志写“通过，恢复调度”；
// 本批全是重新上传的账户时开始与完成的日志开头写“认证文件更新后自动验证”（不写“新账户”）；顶部的处理结果计数不计重新上传的记录
// （只计入重新上传后的结果，原先同一账户既算“验证通过”又算“已恢复”）
func TestReauthVerifyPassRestores(t *testing.T) {
	const id = "a@example.com"
	admin, onboarder, login := reauthTestAdmin(t, id)
	markReauthRequired(t, admin.pool, id)
	onboarder.mu.Lock()
	onboarder.records[id] = onboardRecord{Result: onboardBaseline, At: time.Now()}
	onboarder.mu.Unlock()
	onboarder.queueReauth(id, reauthSourceAuthFile)
	if batch := runReauthBatch(t, onboarder); len(batch) != 1 {
		t.Fatalf("重新验证应立即开批：%v", batch)
	}
	if login.calls.Load() != 1 {
		t.Fatalf("应启动一次浏览器验证，实际 %d 次", login.calls.Load())
	}
	if status := testStatus(t, admin.pool, id); status.State != aistudio.AccountReady || status.Message != "" {
		t.Fatalf("验证通过应恢复就绪：state=%s message=%q", status.State, status.Message)
	}
	onboard := onboarder.status()
	if onboarder.records[id].Result != onboardVerified || len(onboard.Recent) != 1 ||
		onboard.Recent[0].Source != onboardSourceReauth || onboard.Recent[0].Result != onboardVerified {
		t.Fatalf("应记为验证通过并标出来源：record=%+v recent=%+v", onboarder.records[id], onboard.Recent)
	}
	if onboard.Reauth != 0 || onboard.Pending != 0 {
		t.Fatalf("处理完后不应仍在队列：%+v", onboard)
	}
	if onboard.Counts[onboardVerified] != 0 || onboard.ReauthOutcomes[reauthRestored] != 1 {
		t.Fatalf("重新上传的记录只计入重新上传后的结果：counts=%v outcomes=%v", onboard.Counts, onboard.ReauthOutcomes)
	}
	if !hasLogContaining(admin.requests, "认证文件更新后自动验证 | 通过，恢复调度") ||
		!hasLogContaining(admin.requests, "认证文件更新后自动验证开始 | 本批=1 | 并发=3") ||
		!hasLogContaining(admin.requests, "认证文件更新后自动验证完成 | 已启用=0 | 验证通过=1 | 验证未通过=0 | 出错=0 | 耗时=") ||
		hasLogContaining(admin.requests, "新账户自动处理开始") || hasLogContaining(admin.requests, "新账户自动处理完成") {
		t.Fatalf("缺少日志：%v", serviceLogMessages(admin.requests))
	}
}

// TestReauthMixedBatchLog 本批混有新账户与重新上传的账户时，开始与完成的日志照原句（新账户自动处理），末尾写其中认证文件更新的个数
func TestReauthMixedBatchLog(t *testing.T) {
	const fresh, reuploaded = "fresh@example.com", "reuploaded@example.com"
	admin, onboarder, _ := reauthTestAdmin(t, fresh, reuploaded)
	onboarder.enqueue([]string{fresh})
	onboarder.queueReauth(reuploaded, reauthSourceAuthFile)
	batch, policy := onboarder.takeBatch(time.Now().Add(onboardingMaxWait))
	if len(batch) != 2 {
		t.Fatalf("本批 = %v", batch)
	}
	onboarder.processBatch(context.Background(), batch, policy)
	if !hasLogContaining(admin.requests, "新账户自动处理开始 | 本批=2 | 并发=3 | 自动验证=true | 自动启用=true | 其中认证文件更新=1") ||
		!hasLogContaining(admin.requests, "新账户自动处理完成 | 已启用=0 | 验证通过=2 | 验证未通过=0 | 出错=0 | 其中认证文件更新=1 | 耗时=") {
		t.Fatalf("缺少日志：%v", serviceLogMessages(admin.requests))
	}
	status := onboarder.status()
	if status.Counts[onboardVerified] != 1 || status.ReauthOutcomes[reauthRestored] != 1 {
		t.Fatalf("新账户计入处理结果、重新上传的只计入重新上传后的结果：counts=%v outcomes=%v", status.Counts, status.ReauthOutcomes)
	}
}

// TestReauthVerifyFailNewReason 重新验证未通过：仍需要登录，显示这次验证的原因（不再是旧原因）；自动停用记录不写“新账户”
func TestReauthVerifyFailNewReason(t *testing.T) {
	const id = "a@example.com"
	admin, onboarder, login := reauthTestAdmin(t, id)
	login.verification = aistudio.LoginVerification{Reason: "跳转到登录页", PageURL: "accounts.google.com/v3/signin"}
	markReauthRequired(t, admin.pool, id)
	onboarder.queueReauth(id, reauthSourceAuthFile)
	runReauthBatch(t, onboarder)
	status := testStatus(t, admin.pool, id)
	if status.State != aistudio.AccountAuthRequired || !strings.Contains(status.Message, "跳转到登录页") ||
		strings.Contains(status.Message, "GetAiStudioBenefitTier") {
		t.Fatalf("验证未通过应显示新的原因：state=%s message=%q", status.State, status.Message)
	}
	if record := onboarder.records[id]; record.Result != onboardVerifyFailed || !strings.Contains(record.Detail, "跳转到登录页") {
		t.Fatalf("应记为验证未通过：%+v", record)
	}
	note := accountDisableNotes.message(id)
	if !strings.Contains(note, "认证文件更新后验证未通过（跳转到登录页") || strings.Contains(note, "新账户") {
		t.Fatalf("自动停用记录应写认证文件更新后的验证：%q", note)
	}
	if !hasLogContaining(admin.requests, "认证文件更新后自动验证 | 验证未通过，账户需要重新登录 | 原因=跳转到登录页") {
		t.Fatalf("缺少验证未通过的日志：%v", serviceLogMessages(admin.requests))
	}
}

// TestReauthManualDisabledKeepsDisabled 手动停用的账户换上新的认证文件（同一会话重新导出）：按内容发现并载入新 Cookie，保持停用，不验证
func TestReauthManualDisabledKeepsDisabled(t *testing.T) {
	const id = "a@example.com"
	admin, onboarder, login := reauthTestAdmin(t, id)
	pool := admin.pool
	disableTestAccount(t, pool, id)
	accountDisableNotes.set(id, "管理页停用", false)
	base := time.Now().Add(-time.Minute)
	settleAuthFile(t, pool, id, base)
	watcher := newTestAccountWatcher(admin)
	scanAndWait(t, watcher)
	reexportAuthFile(t, pool, id)
	settleAuthFile(t, pool, id, base.Add(20*time.Second))
	scanAndWait(t, watcher)
	if memoryDigest(t, pool, id) != storageStateDigest(diskAuthState(t, pool, id)) {
		t.Fatal("手动停用的账户也应载入新的认证文件")
	}
	if testStatus(t, pool, id).Enabled {
		t.Fatal("手动停用的账户应保持停用")
	}
	if reauthPending(onboarder, id) != nil || login.calls.Load() != 0 {
		t.Fatal("手动停用的账户不应加入验证队列")
	}
	if !hasLogContaining(admin.requests, "已载入新的登录 Cookie；账户是手动停用的，保持停用", "账户="+id) {
		t.Fatalf("缺少保持停用的日志：%v", serviceLogMessages(admin.requests))
	}
	// 排队期间被手动停用的重新验证项：开始处理时跳过，不启动浏览器
	onboarder.mu.Lock()
	onboarder.pending[id] = &onboardPending{queuedAt: time.Now().Add(-onboardingMaxWait), reauthAt: time.Now()}
	onboarder.mu.Unlock()
	runReauthBatch(t, onboarder)
	if record := onboarder.records[id]; record.Result != onboardSkipped || record.Detail != onboardManuallyDisabled || login.calls.Load() != 0 {
		t.Fatalf("排队期间手动停用的应跳过：%+v calls=%d", record, login.calls.Load())
	}
}

// TestReauthAutoDisabledEnabledAndQueued 自动停用（之前验证未通过）的账户换上同一会话重新导出的认证文件：按默认启用、加入队列，
// 疑似登录失效冷却随认证文件更新清除
func TestReauthAutoDisabledEnabledAndQueued(t *testing.T) {
	const id = "a@example.com"
	admin, onboarder, _ := reauthTestAdmin(t, id)
	pool := admin.pool
	disableTestAccount(t, pool, id)
	accountDisableNotes.set(id, "新账户自动处理验证未通过（跳转到登录页）", true)
	coolModel(t, pool, id, "", time.Hour, authSuspectReasonPrefix+"HTTP 401")
	base := time.Now().Add(-time.Minute)
	settleAuthFile(t, pool, id, base)
	watcher := newTestAccountWatcher(admin)
	scanAndWait(t, watcher)
	reexportAuthFile(t, pool, id)
	settleAuthFile(t, pool, id, base.Add(20*time.Second))
	scanAndWait(t, watcher)
	if !testStatus(t, pool, id).Enabled {
		t.Fatal("自动停用的账户换上新的认证文件后应按默认启用")
	}
	if item := reauthPending(onboarder, id); item == nil || item.reauthAt.IsZero() {
		t.Fatal("启用后应加入队列重新验证")
	}
	if reason := suspectCooldown(t, pool, id); reason != "" {
		t.Fatalf("认证文件更新应清掉疑似登录失效冷却，仍有：%s", reason)
	}
}

// TestReauthReadyIdentityChangeQueued 就绪账户换了登录（登录签名 Cookie 变了）：载入、清掉疑似登录失效冷却并加入队列；
// 就绪账户同一会话重新导出（只有内容变了）仍按原来的口径不载入、不加入，不写“内容相同”的提示（另写登录签名没变的提示，
// 见 TestReauthReadySameLoginHint）
func TestReauthReadyIdentityChangeQueued(t *testing.T) {
	const relogged, reexported = "relogged@example.com", "reexported@example.com"
	admin, onboarder, _ := reauthTestAdmin(t, relogged, reexported)
	pool := admin.pool
	coolModel(t, pool, relogged, "", time.Hour, authSuspectReasonPrefix+"HTTP 401")
	base := time.Now().Add(-time.Minute)
	settleAuthFile(t, pool, relogged, base)
	settleAuthFile(t, pool, reexported, base)
	watcher := newTestAccountWatcher(admin)
	scanAndWait(t, watcher)
	before := memoryDigest(t, pool, reexported)
	writeAuthFile(t, pool, relogged, authTestState(t, "NEW", false))
	reexportAuthFile(t, pool, reexported)
	settleAuthFile(t, pool, relogged, base.Add(20*time.Second))
	settleAuthFile(t, pool, reexported, base.Add(20*time.Second))
	scanAndWait(t, watcher)
	if identity, _ := pool.LoginIdentity(relogged); identity != aistudio.LoginIdentity(authTestState(t, "NEW", false)) {
		t.Fatal("就绪账户换了登录应载入新的认证文件")
	}
	if item := reauthPending(onboarder, relogged); item == nil || item.reauthAt.IsZero() {
		t.Fatal("就绪账户换了登录应加入队列验证")
	}
	if reason := suspectCooldown(t, pool, relogged); reason != "" {
		t.Fatalf("换了登录应清掉疑似登录失效冷却，仍有：%s", reason)
	}
	if memoryDigest(t, pool, reexported) != before || reauthPending(onboarder, reexported) != nil {
		t.Fatal("就绪账户同一会话重新导出不应载入或加入队列")
	}
	if onboarder.status().Unchanged != 0 || hasLogContaining(admin.requests, "内容与当前登录状态相同") {
		t.Fatal("就绪账户不应写内容未变化的提示")
	}
}

// TestReauthOwnWriteNotReupload 需要登录的账户由本服务自己写回认证文件（收到 Set-Cookie）不算重新上传，不重新认证、不提示；
// 之后外部原样重新上传（重新写入，修改时间变新）才按重新上传处理
func TestReauthOwnWriteNotReupload(t *testing.T) {
	const id = "a@example.com"
	admin, onboarder, _ := reauthTestAdmin(t, id)
	pool := admin.pool
	markReauthRequired(t, pool, id)
	watcher := newTestAccountWatcher(admin)
	later := time.Now().Add(time.Minute)
	scanLoginAt(t, watcher, later)
	lease, err := pool.AcquireAccount(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if err := lease.MergeSetCookieHeaders(
		[]string{"NID=service-written; Domain=.google.com; Path=/; Secure"}, "https://aistudio.google.com/", time.Now(),
	); err != nil {
		t.Fatal(err)
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
	if value, ok := diskAuthState(t, pool, id).CookieValue("NID", "https://aistudio.google.com/", time.Now()); !ok || value != "service-written" {
		t.Fatal("前提：本服务应已写回认证文件")
	}
	scanLoginAt(t, watcher, later.Add(time.Minute))
	if onboarder.status().Unchanged != 0 || hasLogContaining(admin.requests, "内容与当前登录状态相同") {
		t.Fatalf("本服务自己写的文件不应提示或重新认证：%v", serviceLogMessages(admin.requests))
	}
	if reauthPending(onboarder, id) != nil || testStatus(t, pool, id).Message != reauthOldReason {
		t.Fatal("本服务自己写的文件不应重新认证")
	}
	rewriteAuthFile(t, pool, id)
	scanLoginAt(t, watcher, later.Add(2*time.Minute))
	if item := reauthPending(onboarder, id); item == nil || item.reauthAt.IsZero() {
		t.Fatalf("外部原样重新上传应按重新上传处理：%v", serviceLogMessages(admin.requests))
	}
	if onboarder.status().Unchanged != 0 {
		t.Fatal("失效账户原样重新上传不应只提示")
	}
}

// TestReauthFirstScanDetectsReplacedFile 账户载入之后、第一次扫描之前换上的认证文件（同一会话重新导出）：需要登录的账户第一次扫描时就对照，
// 不只记基准；文件没换过的不读、不提示
func TestReauthFirstScanDetectsReplacedFile(t *testing.T) {
	const replaced, untouched = "replaced@example.com", "untouched@example.com"
	admin, onboarder, _ := reauthTestAdmin(t, replaced, untouched)
	pool := admin.pool
	markReauthRequired(t, pool, replaced, untouched)
	reexportAuthFile(t, pool, replaced)
	scanLoginAt(t, newTestAccountWatcher(admin), time.Now().Add(time.Minute))
	if status := testStatus(t, pool, replaced); status.State == aistudio.AccountAuthRequired {
		t.Fatal("第一次扫描前换上的认证文件应载入")
	}
	if reauthPending(onboarder, replaced) == nil {
		t.Fatal("应加入队列重新验证")
	}
	if status := testStatus(t, pool, untouched); status.Message != reauthOldReason || reauthPending(onboarder, untouched) != nil {
		t.Fatal("没换过的认证文件不应重新认证")
	}
	if onboarder.status().Unchanged != 0 {
		t.Fatal("第一次扫描不应提示内容未变化")
	}
}

// TestReauthBypassesRecordAndRequeue 已有处理记录（基线）的账户照样加入；已取进本批、还没开始验证时又换了认证文件，只更新加入时间、
// 不重复验证；已开始验证时又换了，这次结果写进最近处理（不写处理记录）后重新排队；出错要重试时按新的认证文件从头验证，不计这次的重试次数
func TestReauthBypassesRecordAndRequeue(t *testing.T) {
	const id = "a@example.com"
	_, onboarder, _ := reauthTestAdmin(t, id)
	onboarder.mu.Lock()
	onboarder.records[id] = onboardRecord{Result: onboardBaseline, At: time.Now()}
	onboarder.mu.Unlock()
	onboarder.queueReauth(id, reauthSourceAuthFile)
	if item := reauthPending(onboarder, id); item == nil || item.reauthAt.IsZero() {
		t.Fatal("已有处理记录的账户也应加入")
	}
	if _, recorded := onboarder.records[id]; recorded {
		t.Fatal("加入时应移除旧记录")
	}
	batch, _ := onboarder.takeBatch(time.Now())
	if len(batch) != 1 {
		t.Fatalf("本批 = %v", batch)
	}
	onboarder.queueReauth(id, reauthSourceAuthFile)
	if reauthPending(onboarder, id) != nil {
		t.Fatal("处理中不应同时在待处理里")
	}
	onboarder.mu.Lock()
	requeue := onboarder.processing[id].requeue
	onboarder.mu.Unlock()
	if requeue || !hasLogContaining(onboarder.admin.requests, "认证文件更新后自动验证 | 已在本批中、尚未开始验证，按新的认证文件验证") {
		t.Fatal("还没开始验证时又换了认证文件：验证用的就是新的，不应结束后再排一次")
	}
	onboarder.markStarted(id)
	onboarder.queueReauth(id, reauthSourceAuthFile)
	if final := onboarder.finish(id, onboardVerified, "", false); final != onboardVerified {
		t.Fatalf("最终结果 = %q", final)
	}
	if item := reauthPending(onboarder, id); item == nil || item.reauthAt.IsZero() {
		t.Fatalf("验证开始后又换了认证文件：应重新排队：%+v", item)
	}
	if record, recorded := onboarder.records[id]; recorded {
		t.Fatalf("这次结果属于上一份文件，不应写处理记录（重启后会跳过待验证的这一项）：%+v", record)
	}
	if recent := onboarder.status().Recent; len(recent) != 1 || recent[0].Outcome != reauthRestored || recent[0].Source != onboardSourceReauth {
		t.Fatalf("这次结果应写进最近处理：%+v", recent)
	}
	batch, _ = onboarder.takeBatch(time.Now())
	if len(batch) != 1 {
		t.Fatalf("重新排队的应立即开批：%v", batch)
	}
	onboarder.markStarted(id)
	onboarder.queueReauth(id, reauthSourceAuthFile)
	if final := onboarder.finish(id, onboardError, "上一份文件出错", true); final != "" {
		t.Fatalf("要重试时不是最终结果：%q", final)
	}
	if item := reauthPending(onboarder, id); item == nil || item.attempts != 0 || !item.notBefore.IsZero() || item.reauthAt.IsZero() {
		t.Fatalf("出错的是上一份文件：应按新的认证文件从头验证：%+v", item)
	}
	if !hasLogContaining(onboarder.admin.requests, "认证文件更新后自动验证 | 正在验证，结束后按新的认证文件再验证一次") {
		t.Fatal("缺少处理中重新排队的日志")
	}
}

// TestReauthPersistedAcrossRestart 重新验证的账户随处理记录保存：没验证完就重启，重新排队时仍按重新验证处理，日志分项写明数量
// （不写成“服务停止期间新增的账户”）
func TestReauthPersistedAcrossRestart(t *testing.T) {
	const id = "a@example.com"
	admin, _, _ := reauthTestAdmin(t, id)
	directory := t.TempDir()
	onboarder := newTestOnboarder(admin, directory)
	onboarder.queueReauth(id, reauthSourceAuthFile)
	onboarder.flush()
	restarted := newAccountOnboarder(admin, directory)
	if item := reauthPending(restarted, id); item == nil || item.reauthAt.IsZero() {
		t.Fatalf("重启后应仍按重新验证排队：%+v", item)
	}
	if !hasLogContaining(admin.requests, "新账户自动处理 | 重新加入队列 1 个账户：认证文件更新后待验证的 1 个") ||
		hasLogContaining(admin.requests, "服务停止期间新增") {
		t.Fatalf("缺少重新排队的日志：%v", serviceLogMessages(admin.requests))
	}
	if restarted.status().Reauth != 1 {
		t.Fatal("面板应显示 1 个重新验证")
	}
}

// TestReauthFixedPortPausedDefers 重新验证时账户的固定端口暂停使用：暂缓、不计重试次数，日志写“认证文件更新后自动验证暂缓”
func TestReauthFixedPortPausedDefers(t *testing.T) {
	const id = "a@example.com"
	admin, ipPool, _, _ := fixedAdminEnv(t, fixedStaticPool(10001, 10001), id)
	onboarder := newTestOnboarder(admin, t.TempDir())
	t.Cleanup(onboarder.flush)
	admin.onboard = onboarder
	coolFixedPort(t, ipPool, id)
	onboarder.queueReauth(id, reauthSourceAuthFile)
	batch, policy := onboarder.takeBatch(time.Now())
	if len(batch) != 1 {
		t.Fatalf("本批 = %v", batch)
	}
	result, detail, retry := onboarder.process(context.Background(), id, policy)
	if result != onboardDeferred || retry {
		t.Fatalf("结果 = %s %q %v，期望暂缓", result, detail, retry)
	}
	if final := onboarder.finish(id, result, detail, retry); final != "" {
		t.Fatalf("暂缓不应是最终结果：%s", final)
	}
	if item := reauthPending(onboarder, id); item == nil || item.attempts != 0 || item.reauthAt.IsZero() {
		t.Fatalf("应放回队列、不计重试次数、仍是重新验证：%+v", item)
	}
	if countSourceLogs(admin.requests, id, "认证文件更新后自动验证暂缓 | 原因=IP 池固定端口暂停使用 | 端口=10001") != 1 {
		t.Fatalf("应写暂缓日志：%v", serviceLogMessages(admin.requests))
	}
}

// TestReauthAutoVerifyOff 自动验证关闭：只重新载入新的认证文件（清掉旧原因），不加入队列，写明原因；排队后才关闭的跳过
func TestReauthAutoVerifyOff(t *testing.T) {
	const id, queued = "a@example.com", "b@example.com"
	admin, onboarder, login := reauthTestAdmin(t, id, queued)
	pool := admin.pool
	markReauthRequired(t, pool, id, queued)
	onboarder.queueReauth(queued, reauthSourceAuthFile)
	onboarder.mu.Lock()
	onboarder.policy.AutoVerify = false
	onboarder.mu.Unlock()
	base := time.Now().Add(-time.Minute)
	settleAuthFile(t, pool, id, base)
	watcher := newTestAccountWatcher(admin)
	scanAndWait(t, watcher)
	writeAuthFile(t, pool, id, authTestState(t, "NEW", false))
	settleAuthFile(t, pool, id, base.Add(20*time.Second))
	scanAndWait(t, watcher)
	if status := testStatus(t, pool, id); status.State == aistudio.AccountAuthRequired {
		t.Fatal("自动验证关闭时仍应重新载入")
	}
	if reauthPending(onboarder, id) != nil {
		t.Fatal("自动验证关闭时不应加入队列")
	}
	if !hasLogContaining(admin.requests, "认证文件更新后自动验证 | 自动验证已关闭，只重新载入", "账户="+id) {
		t.Fatalf("缺少说明日志：%v", serviceLogMessages(admin.requests))
	}
	runReauthBatch(t, onboarder)
	if record := onboarder.records[queued]; record.Result != onboardSkipped || record.Detail != "自动验证已关闭" || login.calls.Load() != 0 {
		t.Fatalf("排队后才关闭自动验证的应跳过：%+v calls=%d", record, login.calls.Load())
	}
}

// TestReauthUnusableFileShowsNewReason 需要登录的账户换上的认证文件不能签名（缺登录签名 Cookie）或读不出来：仍需要登录，
// 显示这次的原因（不含 Cookie 值），不加入队列
func TestReauthUnusableFileShowsNewReason(t *testing.T) {
	const unsigned, broken = "unsigned@example.com", "broken@example.com"
	admin, onboarder, _ := reauthTestAdmin(t, unsigned, broken)
	pool := admin.pool
	markReauthRequired(t, pool, unsigned, broken)
	base := time.Now().Add(-time.Minute)
	settleAuthFile(t, pool, unsigned, base)
	settleAuthFile(t, pool, broken, base)
	watcher := newTestAccountWatcher(admin)
	scanAndWait(t, watcher)
	writeAuthFile(t, pool, unsigned, aistudio.StorageState{Cookies: []aistudio.StateCookie{{
		Name: "NID", Value: "secret-cookie-value", Domain: ".google.com", Path: "/", Expires: -1, Secure: true, SameSite: "None",
	}}})
	account, err := pool.Account(broken)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(account.StoragePath, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	settleAuthFile(t, pool, unsigned, base.Add(20*time.Second))
	settleAuthFile(t, pool, broken, base.Add(20*time.Second))
	scanAndWait(t, watcher)
	status := testStatus(t, pool, unsigned)
	if status.State != aistudio.AccountAuthRequired || !strings.HasPrefix(status.Message, "新的认证文件无法用于 AI Studio：") ||
		!strings.Contains(status.Message, "SAPISID") || strings.Contains(status.Message, "secret-cookie-value") {
		t.Fatalf("不能签名的认证文件应显示新的原因（不含 Cookie 值）：state=%s message=%q", status.State, status.Message)
	}
	status = testStatus(t, pool, broken)
	if status.State != aistudio.AccountAuthRequired || !strings.HasPrefix(status.Message, "新的认证文件无法读取：") {
		t.Fatalf("读不出来的认证文件应显示新的原因：state=%s message=%q", status.State, status.Message)
	}
	if onboarder.status().Pending != 0 {
		t.Fatal("不能用的认证文件不应加入队列")
	}
	for _, message := range serviceLogMessages(admin.requests) {
		if strings.Contains(message, "secret-cookie-value") {
			t.Fatalf("日志不应出现 Cookie 值：%s", message)
		}
	}
}

// TestReauthConcurrencyBounded 上千个账户一起换认证文件时按队列依次验证：同时验证的数量不超过并发设置；验证进行中加入新的账户立即返回
func TestReauthConcurrencyBounded(t *testing.T) {
	ids := make([]string, 0, 30)
	for index := range 30 {
		ids = append(ids, "u"+string(rune('a'+index/26))+string(rune('a'+index%26))+"@example.com")
	}
	extra := "late@example.com"
	admin, onboarder, login := reauthTestAdmin(t, append(ids, extra)...)
	var active, peak atomic.Int32
	gate := make(chan struct{})
	var gateOnce sync.Once
	login.onVerify = func() {
		current := active.Add(1)
		for {
			previous := peak.Load()
			if current <= previous || peak.CompareAndSwap(previous, current) {
				break
			}
		}
		<-gate
		time.Sleep(2 * time.Millisecond)
		active.Add(-1)
	}
	t.Cleanup(func() { gateOnce.Do(func() { close(gate) }) })
	markReauthRequired(t, admin.pool, ids...)
	for _, id := range ids {
		onboarder.queueReauth(id, reauthSourceAuthFile)
	}
	done := make(chan []string)
	go func() { done <- runReauthBatch(t, onboarder) }()
	waitFor(t, "开始验证", func() bool { return active.Load() == 3 })
	started := time.Now()
	onboarder.queueReauth(extra, reauthSourceAuthFile)
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("验证进行中加入队列不应等待：%s", elapsed)
	}
	if status := onboarder.status(); status.Reauth != 31 || len(status.Processing) != 30 || status.Pending != 1 {
		t.Fatalf("面板 reauth=%d processing=%d pending=%d，期望 31、30、1", status.Reauth, len(status.Processing), status.Pending)
	}
	gateOnce.Do(func() { close(gate) })
	if batch := <-done; len(batch) != 30 {
		t.Fatalf("本批 = %d 个", len(batch))
	}
	if peak.Load() != 3 || login.calls.Load() != 30 {
		t.Fatalf("同时验证最多 %d 个（期望 3），共验证 %d 次（期望 30）", peak.Load(), login.calls.Load())
	}
	if outcomes := onboarder.status().ReauthOutcomes; outcomes[reauthRestored] != 30 {
		t.Fatalf("重新上传后的结果计数 = %v", outcomes)
	}
}

// TestReauthChromeReimportQueued 管理页 Chrome 就地重新导入已有的启用账户：导入后加入队列验证，来源写 Chrome 重新导入；停用的不加入
func TestReauthChromeReimportQueued(t *testing.T) {
	isolateDisableNotes(t)
	admin, _, _ := keepCatalogAdmin(t, "a@example.com", "b@example.com")
	onboarder := newTestOnboarder(admin, t.TempDir())
	t.Cleanup(onboarder.flush)
	admin.onboard = onboarder
	chromeReimportStubs(t, func(id string) string {
		return map[string]string{"Default/a": "a@example.com", "Profile 1/b": "b@example.com"}[id]
	})
	disableTestAccount(t, admin.pool, "b@example.com")
	accountDisableNotes.set("b@example.com", "管理页停用", false)
	for _, target := range []chromeReimport{{chromeID: "Default/a", accountID: "a@example.com"}, {chromeID: "Profile 1/b", accountID: "b@example.com"}} {
		if _, err := admin.reimportChromeAccount(context.Background(), "root", target); err != nil {
			t.Fatal(err)
		}
	}
	if item := reauthPending(onboarder, "a@example.com"); item == nil || item.reauthAt.IsZero() {
		t.Fatal("就地重新导入的启用账户应加入队列验证")
	}
	if reauthPending(onboarder, "b@example.com") != nil {
		t.Fatal("停用的账户不应加入")
	}
	if record := onboarder.records["b@example.com"]; record.Source != onboardSourceReauth || record.Outcome != reauthManualDisabled {
		t.Fatalf("手动停用的账户重新导入后应记为跳过（手动停用）：%+v", record)
	}
	if !hasLogContaining(admin.requests, "认证文件更新后自动验证 | 已加入队列 | 账户=a@example.com | 来源=Chrome 重新导入") {
		t.Fatalf("缺少加入队列的日志：%v", serviceLogMessages(admin.requests))
	}
}

// TestReauthBatchLogUnchangedWhenNone 本批没有重新验证的账户时，“新账户自动处理开始”与原来逐字相同
func TestReauthBatchLogUnchangedWhenNone(t *testing.T) {
	const id = "a@example.com"
	admin, onboarder, _ := reauthTestAdmin(t, id)
	onboarder.enqueue([]string{id})
	batch, policy := onboarder.takeBatch(time.Now().Add(onboardingMaxWait))
	onboarder.processBatch(context.Background(), batch, policy)
	want := "新账户自动处理开始 | 本批=1 | 并发=3 | 自动验证=true | 自动启用=true"
	found := false
	for _, message := range serviceLogMessages(admin.requests) {
		if strings.HasPrefix(message, "新账户自动处理开始") {
			if message != want {
				t.Fatalf("本批开始日志 = %q，期望 %q", message, want)
			}
			found = true
		}
	}
	if !found || len(onboarder.status().Recent) != 1 || onboarder.status().Recent[0].Source != "" {
		t.Fatalf("新账户的处理记录不应标为重新验证：%+v", onboarder.status().Recent)
	}
}

// TestStorageStateDigestIgnoresCookieOrder 内容相同、只是 Cookie 顺序不同（换了导出工具）的认证文件摘要相同，不算重新上传；值不同时仍不同
func TestStorageStateDigestIgnoresCookieOrder(t *testing.T) {
	state := withCookie(withCookie(authTestState(t, "A", false), "NID", "n"), "__Secure-1PSID", "p")
	reordered := state
	reordered.Cookies = append([]aistudio.StateCookie(nil), state.Cookies...)
	for left, right := 0, len(reordered.Cookies)-1; left < right; left, right = left+1, right-1 {
		reordered.Cookies[left], reordered.Cookies[right] = reordered.Cookies[right], reordered.Cookies[left]
	}
	if storageStateDigest(reordered) != storageStateDigest(state) {
		t.Fatal("只有 Cookie 顺序不同时摘要应相同")
	}
	if storageStateDigest(withCookie(reordered, "NID", "changed")) == storageStateDigest(state) {
		t.Fatal("Cookie 值不同时摘要应不同")
	}
}
