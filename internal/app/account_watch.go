package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

const (
	// accountScanInterval 为账户目录扫描周期
	accountScanInterval = 10 * time.Second
	// accountSettleTime 为文件最后修改后等待的时间，避免读到正在复制的目录
	accountSettleTime = 5 * time.Second
	// accountMissingScans 为目录连续缺失多少轮后才移出账户池
	accountMissingScans = 2
	// importCatalogConcurrency 为新导入账户并发同步模型目录的数量
	importCatalogConcurrency = 8
	// massMissingMin 与 massMissingRatio：一轮缺失的账户超过 max(下限, 总数×比例) 视为批量消失
	massMissingMin   = 20
	massMissingRatio = 0.1
	// massMissingScans 批量消失时需要连续缺失的轮数（约 1 分钟），防止整体替换目录期间把账户全部移出
	massMissingScans = 6
	// loginRefreshPerScan 为每轮最多载入的登录状态更新数，loginRefreshConcurrency 为并发数。
	// 载入在后台进行，不阻塞扫描，也不在请求路径上
	loginRefreshPerScan     = 40
	loginRefreshConcurrency = 4
	// loginRefreshAcquireWait 为等待账户空闲的最长时间；账户正在处理请求时下一轮再试，不打断请求
	loginRefreshAcquireWait = 2 * time.Second
	// authFileHintInterval 为同一账户同一种认证文件提示（看起来是共用账户目录的其他进程回写、登录签名没变未重新载入）的最短间隔：
	// 共用账户目录时另一个进程每次回写都会触发，不按间隔会刷屏
	authFileHintInterval = time.Hour
)

// authFileStat 为认证文件上次扫描时的修改时间与大小；没有变化的文件不读取
type authFileStat struct {
	modTime time.Time
	size    int64
}

// accountWatcher 定期扫描账户目录，热更新当前生成服务的账户池
//
//   - 新目录：自动载入并加入账户池，无需重启
//   - 目录被删除：连续缺失两轮后移出账户池（不删除任何文件）
//   - 认证文件被外部更新（重新登录、后台程序刷新 Cookie、重新上传）：任何状态的账户都会载入新的登录状态。
//     需要登录的账户随之恢复调度；停用但不是手动停用的账户按默认启用，并重新同步模型目录；载入后仍是启用的账户
//     加入新账户自动处理队列验证一次（见 accountOnboarder.queueReauth）。处于失效类状态的账户（见 reauthFailure）按文件内容判断，
//     内容与当前登录状态相同的重新上传同样清除旧判定、重新认证并验证；健康账户内容没变的写一行提示（见 detectLoginUpdates）
type accountWatcher struct {
	admin     *runtimeAdmin
	failures  map[string]string
	missing   map[string]int
	authFiles map[string]authFileStat
	// loginPending 为检测到登录状态更新、尚未载入的账户；账户忙碌时留到下一轮。loginSame 为其中重新上传了与当前登录状态相同的
	// 认证文件的失效账户（沿用当前登录状态重新认证，见 detectLoginUpdates）与检测时那份文件原始内容的指纹，按需建立
	loginMu      sync.Mutex
	loginPending map[string]struct{}
	loginSame    map[string]aistudio.StorageDigest
	// loading 为这一批正在后台载入、还没结束的账户数（受 loginMu 保护），与 loginPending 一起计入面板的“等待载入”（见 publishBacklogLocked）
	loading int
	// hinted 为各账户各种认证文件提示上一次写的时间（见 hintDue），只在扫描里读写
	hinted map[authFileHint]time.Time
	// refreshing 为真时上一批登录状态更新仍在后台载入
	refreshing atomic.Bool
	// recoverAt 为文件缺失后又恢复的账户下一次重新同步的时间，避免同步失败时每轮扫描都重试
	recoverAt map[string]time.Time
}

// watchAccountDirectories 在生成服务实例的生命周期内持续扫描账户目录
func (admin *runtimeAdmin) watchAccountDirectories(ctx context.Context) {
	watcher := &accountWatcher{
		admin:        admin,
		failures:     make(map[string]string),
		missing:      make(map[string]int),
		authFiles:    make(map[string]authFileStat),
		loginPending: make(map[string]struct{}),
	}
	ticker := time.NewTicker(accountScanInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			watcher.scan(ctx)
		}
	}
}

// report 记录一次问题；同一对象的相同错误只记录一次，避免每轮扫描刷屏
func (watcher *accountWatcher) report(key string, message string) {
	if watcher.failures[key] == message {
		return
	}
	watcher.failures[key] = message
	watcher.admin.requests.log("auth", "WARN", message)
}

func (watcher *accountWatcher) scan(ctx context.Context) {
	directories, err := watcher.admin.store.DiscoverDirectories()
	if err != nil {
		watcher.report("*", "账户目录扫描失败 | 错误="+strings.TrimSpace(err.Error()))
		return
	}
	delete(watcher.failures, "*")
	// 一次加锁取得全部账户的轻量状态（不统计模型目录）；原先每轮要做三次完整的账户状态统计
	summaries := watcher.admin.pool.LoginSummaries()
	known := make(map[string]aistudio.AccountLoginSummary, len(summaries))
	for _, summary := range summaries {
		known[summary.ID] = summary
	}
	present := make(map[string]struct{}, len(directories))
	changed := false
	imported := make([]*aistudio.Account, 0)
	importedIDs := make([]string, 0)
	keptDisabled := make([]string, 0)
	now := time.Now()
	loaded := make([]*aistudio.Account, 0)
	loadedDirectories := make([]string, 0)
	for _, directory := range directories {
		if ctx.Err() != nil {
			return
		}
		id := filepath.Base(directory)
		present[id] = struct{}{}
		if _, exists := known[id]; exists || !accountFilesSettled(directory, now) {
			continue
		}
		account, err := watcher.admin.store.LoadDirectory(directory)
		if err != nil {
			watcher.reportImportFailure(directory, err)
			continue
		}
		loaded = append(loaded, account)
		loadedDirectories = append(loadedDirectories, directory)
	}
	// 账户默认启用：account.json 为停用、没有手动停用记录的账户先改为启用（并行写回 account.json），再加入账户池，
	// 与启动时的规则一致；一次放进几千个目录时也不逐个推送账户状态
	enableUnmarkedAccounts(loaded, watcher.admin.requests, "导入时")
	// 设备绑定会话的统计在加入账户池之前（读认证状态不加锁），一批只写一行
	logDeviceBoundSessions(watcher.admin.requests, loaded, "导入时")
	for index, account := range loaded {
		if ctx.Err() != nil {
			return
		}
		directory := loadedDirectories[index]
		if err := watcher.admin.registerExistingAccount(ctx, account); err != nil {
			watcher.reportImportFailure(directory, err)
			continue
		}
		delete(watcher.failures, directory)
		// 重新导入的账户目录带着新的认证文件：清除之前的 403 无权限记录与冷却（目录里旧的 runtime-state.json 可能还留着）
		watcher.admin.service.forgetPermissionDenied(account.ID, "重新导入")
		known[account.ID] = aistudio.AccountLoginSummary{ID: account.ID}
		changed = true
		message := "自动导入账户 | 账户=" + account.Config.Label
		if account.Config.Enabled {
			imported = append(imported, account)
			importedIDs = append(importedIDs, account.ID)
		} else {
			// 只有手动停用过的账户（目录被移走后又放回）还是停用；不进入自动处理队列，原先排进队列后验证通过又被启用
			message += " | 账户是手动停用的，保持停用"
			keptDisabled = append(keptDisabled, account.ID)
		}
		watcher.admin.requests.log("auth", "INFO", message)
	}
	// 新账户进入自动处理队列（攒够一批后验证、通过后启用）；手动停用的直接记为跳过
	watcher.admin.onboard.enqueue(importedIDs)
	watcher.admin.onboard.skipManuallyDisabled(keptDisabled)
	// 模型目录要走网络，放到后台并发同步，不阻塞本轮扫描；批量上传几百个账户时也不会卡住扫描
	if len(imported) > 0 {
		go watcher.admin.syncImportedCatalogs(ctx, imported)
	}
	watcher.recoverRestored(ctx, summaries, present, now)
	if watcher.detachMissing(summaries, present) {
		changed = true
	}
	watcher.detectLoginUpdates(summaries, now)
	watcher.startLoginRefresh(ctx, known)
	if changed {
		watcher.admin.syncModelCache()
	}
}

// reportImportFailure 记录一次目录导入失败；同一目录的相同错误只记录一次
func (watcher *accountWatcher) reportImportFailure(directory string, err error) {
	watcher.report(directory, fmt.Sprintf(
		"自动导入账户失败 | 目录=%s | 错误=%s", filepath.Base(directory), strings.TrimSpace(err.Error()),
	))
}

// accountRecoverRetry 为恢复账户的模型目录同步失败后再次尝试的间隔
const accountRecoverRetry = time.Minute

// recoverRestored 让文件缺失时被标为不可用的账户在文件恢复后重新同步模型目录，同步成功即恢复可调度。
// 原先“不可用”只能靠重启服务或管理操作解除：整体替换账户目录时，复制期间被访问到的账户会一直不可用
func (watcher *accountWatcher) recoverRestored(
	ctx context.Context,
	summaries []aistudio.AccountLoginSummary,
	present map[string]struct{},
	now time.Time,
) {
	if watcher.recoverAt == nil {
		watcher.recoverAt = make(map[string]time.Time)
	}
	var restored []*aistudio.Account
	for _, summary := range summaries {
		if !summary.FilesMissing {
			delete(watcher.recoverAt, summary.ID)
			continue
		}
		if _, exists := present[summary.ID]; !exists || !summary.Enabled || now.Before(watcher.recoverAt[summary.ID]) ||
			!accountFilesSettled(summary.Directory, now) {
			continue
		}
		account, err := watcher.admin.pool.Account(summary.ID)
		if err != nil {
			continue
		}
		watcher.recoverAt[summary.ID] = now.Add(accountRecoverRetry)
		restored = append(restored, account)
		watcher.admin.requests.log("auth", "INFO", "账户文件已恢复，重新同步模型目录 | 账户="+account.Config.Label)
	}
	if len(restored) > 0 {
		go watcher.admin.syncImportedCatalogs(ctx, restored)
	}
}

// detachMissing 把目录已被删除的账户移出账户池。
// 本轮一个目录都没扫到时视为异常，不做移除；一次消失很多账户时（例如整体替换 auth 目录），
// 需要连续缺失约 1 分钟才移出，避免复制过程中把大量账户移出又重新导入
func (watcher *accountWatcher) detachMissing(summaries []aistudio.AccountLoginSummary, present map[string]struct{}) bool {
	if len(present) == 0 {
		return false
	}
	missingNow := 0
	for _, summary := range summaries {
		if _, exists := present[summary.ID]; !exists {
			missingNow++
		}
	}
	required := accountMissingScans
	if threshold := max(massMissingMin, int(float64(len(summaries))*massMissingRatio)); missingNow > threshold {
		required = massMissingScans
		watcher.report("mass-missing", fmt.Sprintf(
			"账户目录大量缺失 | 缺失=%d/%d | 连续缺失约 1 分钟后才会移出，如在整体替换目录可忽略", missingNow, len(summaries),
		))
	} else {
		delete(watcher.failures, "mass-missing")
	}
	changed := false
	for _, summary := range summaries {
		if _, exists := present[summary.ID]; exists {
			delete(watcher.missing, summary.ID)
			continue
		}
		watcher.missing[summary.ID]++
		if watcher.missing[summary.ID] < required {
			continue
		}
		if err := watcher.admin.detachAccount(summary.ID); err != nil {
			watcher.report("missing:"+summary.ID, fmt.Sprintf(
				"移出已删除目录的账户失败，下轮重试 | 账户=%s | 错误=%s", summary.ID, strings.TrimSpace(err.Error()),
			))
			continue
		}
		delete(watcher.missing, summary.ID)
		delete(watcher.failures, "missing:"+summary.ID)
		changed = true
		watcher.admin.requests.log("auth", "INFO", "账户目录已删除，已移出账户池 | 账户="+summary.ID)
	}
	return changed
}

// authFileHint 为一种认证文件提示与账户（见 hintDue）
type authFileHint struct {
	kind      string
	accountID string
}

// hintDue 判断账户这种提示距上一次写超过 authFileHintInterval，是时记下这一次
func (watcher *accountWatcher) hintDue(kind string, accountID string, now time.Time) bool {
	if watcher.hinted == nil {
		watcher.hinted = make(map[authFileHint]time.Time)
	}
	key := authFileHint{kind: kind, accountID: accountID}
	if last, ok := watcher.hinted[key]; ok && now.Sub(last) < authFileHintInterval {
		return false
	}
	watcher.hinted[key] = now
	return true
}

// detectLoginUpdates 找出认证文件里的登录状态被外部更新的账户：
//   - 回写 Cookie 时已经发现磁盘上的登录状态与内存不同（由 aistudio 记录；磁盘上的登录签名到扫描时已与内存一致的不再载入，
//     见 externalLoginsPending）
//   - 认证文件的修改时间或大小变了，并且文件里的登录签名 Cookie 与内存中的不同
//   - 处于失效类状态的账户（需要登录、自动停用、组织停用或受限、403 无权限、需要验证账号、单个模型需要验证、疑似登录失效，
//     见 reauthFailure）与停用账户另按文件内容判断（见 deniedAuthUpdated）。失效类状态的账户内容与当前登录状态相同、
//     又是外部重新写入的（修改时间变新，
//     不是本服务自己读入或写入的那一份，见 aistudio.StorageFileOwned）同样按重新上传处理（记入 loginSame，沿用当前登录状态重新认证）：
//     在官网解开了 403、Cookie 没变就重新上传了同一份时，原先什么都不做，账户一直显示原来的失效原因。只多了、换了或缺了轮换 Cookie、
//     AI Studio 的 localStorage 的（共用账户目录的另一个进程回写，见 compareAuthFile）不算，文件逐字节就是本服务读入或写过的那一份时除外；
//     失效账户写一行提示（每小时最多一次，见 noteWriteBackAuthFile）
//
// 就绪的启用账户只比较登录签名 Cookie（SAPISID 等），本服务自己回写的轮换 Cookie 不会触发；健康账户（含只是手动停用的）内容与
// 当前登录状态相同的重新上传写一行提示（见 noteAuthUnchanged），内容有变化、登录签名没变的另写一行提示（见 noteSameLoginAuthFile）。
// 失效账户的文件被重新写入、内容与本服务上次读入或写入的相同、修改时间却变早的（上传工具保留了原文件的修改时间）写一行提示
// （见 noteOwnedRewrite）。没有变化的文件只做一次 stat，不读取内容；整个过程不在请求路径上
func (watcher *accountWatcher) detectLoginUpdates(summaries []aistudio.AccountLoginSummary, now time.Time) {
	found := watcher.externalLoginsPending(aistudio.TakeExternalLoginChanges(), summaries)
	same := make(map[string]aistudio.StorageDigest)
	current := make(map[string]struct{}, len(summaries))
	for _, summary := range summaries {
		current[summary.ID] = struct{}{}
		if summary.StoragePath == "" {
			continue
		}
		info, err := os.Stat(summary.StoragePath)
		if err != nil {
			continue
		}
		stat := authFileStat{modTime: info.ModTime(), size: info.Size()}
		previous, seen := watcher.authFiles[summary.ID]
		if !seen {
			// 第一次看到的认证文件只记作基准、不读取内容（启动时可能有上万个账户）。403 无权限账户与组织停用账户例外：
			// 载入账户池之后、第一次扫描之前被替换的文件（例如重启生成服务时正好在等待文件稳定），只记作基准就再也发现不了。
			// 其他失效类状态的账户同样例外，但文件还是本服务载入或写入的那一份时（绝大多数，按修改时间与大小判断）不读。
			// 第一次看到时没有上一次的修改时间可比，内容与当前登录状态相同的不按重新上传处理
			if watcher.admin.service.watchedLogin(summary.ID) ||
				!watcher.admin.pool.StorageFileSynced(summary.ID, stat.modTime, stat.size) && watcher.admin.byContent(summary) {
				if now.Sub(stat.modTime) < accountSettleTime {
					// 仍在写入，下一轮再看
					continue
				}
				state, err := aistudio.LoadStorageState(summary.StoragePath)
				if err != nil {
					watcher.authFiles[summary.ID] = stat
					continue
				}
				refresh, settled := watcher.deniedAuthUpdated(summary, state, stat)
				if !settled {
					continue
				}
				if refresh {
					found = append(found, summary.ID)
				}
			}
			watcher.authFiles[summary.ID] = stat
			continue
		}
		if previous.modTime.Equal(stat.modTime) && previous.size == stat.size {
			continue
		}
		if now.Sub(stat.modTime) < accountSettleTime {
			// 仍在写入，下一轮再看
			continue
		}
		watcher.authFiles[summary.ID] = stat
		state, digest, err := aistudio.LoadStorageStateDigest(summary.StoragePath)
		if err != nil {
			watcher.report("login:"+summary.ID, fmt.Sprintf(
				"读取更新后的认证文件失败 | 账户=%s | 错误=%s", summary.ID, strings.TrimSpace(err.Error()),
			))
			watcher.admin.markUnusableAuthFile(summary, "新的认证文件无法读取：", err)
			continue
		}
		delete(watcher.failures, "login:"+summary.ID)
		memory, ok := watcher.admin.pool.LoginIdentity(summary.ID)
		if !ok {
			continue
		}
		if memory != aistudio.LoginIdentity(state) {
			found = append(found, summary.ID)
			continue
		}
		// 本服务自己读入或写入的那一份（只是修改时间变了，或就是本服务刚写的）：不是重新上传。403 无权限与组织停用账户仍要对照，
		// 本服务自己续签换掉登录签名时更新登记里记下的指纹（见 deniedAuthUpdated）
		owned := watcher.admin.pool.StorageFileOwned(summary.ID, stat.modTime, digest)
		watched := watcher.admin.service.watchedLogin(summary.ID)
		if owned && !watched {
			watcher.noteOwnedRewrite(summary, stat, "")
			continue
		}
		failure := watcher.admin.reauthFailure(summary)
		if failure != "" || !summary.Enabled {
			refresh, settled := watcher.contentAuthUpdated(summary, state, stat, true)
			if !settled {
				// 比较期间文件又被改写：恢复上一次的基准，下一轮重新检查
				watcher.authFiles[summary.ID] = previous
				continue
			}
			if refresh {
				found = append(found, summary.ID)
				continue
			}
		}
		if owned {
			watcher.noteOwnedRewrite(summary, stat, failure)
			continue
		}
		memoryState, ok := watcher.admin.pool.StorageStateOf(summary.ID)
		if !ok {
			continue
		}
		change := compareAuthFile(state, memoryState, watcher.admin.pool.StorageFileKnown(summary.ID, digest))
		switch change {
		case authFileSame:
		case authFileChanged:
			// 只有就绪的启用账户会到这里（失效与停用账户内容变了已在上面载入）：登录签名没变、内容变了（同一会话重新导出，或共用账户目录的
			// 其他进程回写了其他 Cookie）按原口径不重新载入，写一行提示
			watcher.noteSameLoginAuthFile(summary, now)
			continue
		default:
			// 只差轮换 Cookie、localStorage（多了、换了或比当前的旧）：按共用账户目录的其他进程回写处理，失效账户写一行提示
			if failure != "" {
				watcher.noteWriteBackAuthFile(summary, failure, change == authFileOlder, now)
			}
			continue
		}
		if failure == "" {
			// 账户本来就健康（含只是手动停用的）、内容又没变（重复上传同一份）：不重新认证，写一行提示
			watcher.admin.noteAuthUnchanged(summary)
			continue
		}
		// 失效账户重新上传了内容相同的认证文件：同样按重新上传处理，清除旧判定、重新认证并加入验证队列
		same[summary.ID] = digest
		watcher.admin.requests.log("auth", "INFO", fmt.Sprintf(
			"认证文件已重新上传，内容与当前登录状态相同；账户处于失效状态，按重新上传处理（清除旧判定、重新认证并验证） | 账户=%s | 状态=%s",
			summary.ID, failure,
		))
	}
	for id := range watcher.authFiles {
		if _, exists := current[id]; !exists {
			delete(watcher.authFiles, id)
		}
	}
	for key := range watcher.hinted {
		if _, exists := current[key.accountID]; !exists {
			delete(watcher.hinted, key)
		}
	}
	if len(found) == 0 && len(same) == 0 {
		return
	}
	watcher.loginMu.Lock()
	if watcher.loginSame == nil {
		watcher.loginSame = make(map[string]aistudio.StorageDigest)
	}
	added := 0
	for id, digest := range same {
		if _, pending := watcher.loginPending[id]; !pending {
			added++
		}
		watcher.loginPending[id] = struct{}{}
		watcher.loginSame[id] = digest
	}
	// 文件内容变了的载入磁盘上的新文件（同一账户先后两次上传，以最近一次为准）
	for _, id := range found {
		if _, pending := watcher.loginPending[id]; !pending {
			added++
		}
		watcher.loginPending[id] = struct{}{}
		delete(watcher.loginSame, id)
	}
	backlog := len(watcher.loginPending) + watcher.loading
	watcher.publishBacklogLocked()
	watcher.loginMu.Unlock()
	if added > loginRefreshPerScan {
		// 批量重新上传：载入要分多轮，载入之前账户列表仍显示原来的状态，写一行说明（面板显示“等待载入”）
		watcher.admin.requests.log("auth", "INFO", fmt.Sprintf(
			"检测到认证文件更新 | 本轮新增 %d 个 | 等待载入 %d 个 | 后台每轮扫描（约 %d 秒）最多载入 %d 个，载入前账户列表仍显示原来的状态",
			added, backlog, int(accountScanInterval/time.Second), loginRefreshPerScan,
		))
	}
}

// publishBacklogLocked 把检测到认证文件更新、还没载入完的账户数（等后台载入的与这一批正在载入的）交给新账户自动处理面板
// （见 runtimeAdmin.loginBacklog）。调用方持有 loginMu
func (watcher *accountWatcher) publishBacklogLocked() {
	watcher.admin.loginBacklog.Store(int64(len(watcher.loginPending) + watcher.loading))
}

// externalLoginsPending 过滤回写 Cookie 时记下的“磁盘上的登录状态已被外部更新”的账户（见 aistudio.TakeExternalLoginChanges）：
// 磁盘上的登录签名已经与内存一致的不再载入（例如换上的文件不能签名、浏览器回写时已把能用的登录写回，或之后在管理页重新登录了），
// 免得白白关闭浏览器、排队验证一次；读不出来的照常交给载入（由它报告错误）
func (watcher *accountWatcher) externalLoginsPending(marked []string, summaries []aistudio.AccountLoginSummary) []string {
	if len(marked) == 0 {
		return nil
	}
	paths := make(map[string]string, len(summaries))
	for _, summary := range summaries {
		paths[summary.ID] = summary.StoragePath
	}
	pending := make([]string, 0, len(marked))
	for _, id := range marked {
		path := paths[id]
		if path == "" {
			continue
		}
		if state, err := aistudio.LoadStorageState(path); err == nil {
			if memory, ok := watcher.admin.pool.LoginIdentity(id); ok && memory == aistudio.LoginIdentity(state) {
				continue
			}
		}
		pending = append(pending, id)
	}
	return pending
}

// deniedAuthUpdated 按内容检查 403 无权限、组织停用、其他失效类状态（见 reauthFailure）与停用账户磁盘上的认证文件（调用方已确认
// 文件稳定、已读入 disk），返回是否要载入它（由 startLoginRefresh 载入，载入时清除登记、403 冷却与需要登录的旧原因），
// 以及本轮能否下结论。就绪的启用账户不检查，见 contentAuthUpdated
func (watcher *accountWatcher) deniedAuthUpdated(
	summary aistudio.AccountLoginSummary, disk aistudio.StorageState, stat authFileStat,
) (bool, bool) {
	return watcher.contentAuthUpdated(summary, disk, stat, watcher.admin.byContent(summary))
}

// byContent 判断账户的认证文件是否按内容判断更新：处于失效类状态（见 reauthFailure）或停用（手动停用的账户也要能换上
// 同一会话重新导出的认证文件，只换认证、保持停用）。就绪的启用账户只比较登录签名 Cookie
func (admin *runtimeAdmin) byContent(summary aistudio.AccountLoginSummary) bool {
	return !summary.Enabled || admin.reauthFailure(summary) != ""
}

// contentAuthUpdated 与 deniedAuthUpdated 相同，byContent 为调用方已判断的账户是否按内容判断（见 byContent）。为假、也不在 403 无权限
// 与组织停用登记表里的账户（就绪的启用账户）不检查：
//   - 磁盘上的内容与内存不同：本服务写回认证文件时同时更新内存，不同说明文件被外部更新了。同一登录会话里重新导出
//     （在已登录的浏览器里同意政策后重新获取）时登录签名 Cookie 不变，只比较它发现不了，按说明重新上传后账户仍然列着、仍在冷却，
//     需要登录的账户仍然显示原来的错误。内容比较不计 Google 定期轮换的 Cookie 与 AI Studio 的 localStorage（见 storageStateDigest）：
//     共用账户目录的其他进程回写它们（轮换 Cookie 随请求回写，localStorage 在 Worker 启动成功后回写）不算重新上传
//   - 磁盘与内存相同（第一次看到时也一样）：内存里就是本服务自己的登录状态，这里不算重新上传（外部重新写入的同一份由调用方另判，
//     见 detectLoginUpdates）。登录签名与登记时记下的不同时（本服务自己写回时换掉了登录签名 Cookie；续签已在提交时更新，
//     见 authRuntimeRefresher.renewed）只更新记下的登录签名，免得下次启动时被当作服务停止期间替换了认证文件。原先第一次看到时按
//     重新上传清除：扫描第一次看到文件之前续签的账户（启动后几十秒内）被移出列表、解除 403 冷却，冷却档位从头计，永远升不到
//     6 小时与次日零点两档。服务停止期间替换的认证文件由启动时的 reconcilePermissionDenied 处理，载入账户池之后替换的由上一条
//     （磁盘与内存不同）发现
//
// 比较期间文件又被改写（多半是本服务正在写回 Cookie，写盘与更新内存之间可能被读到）时不下结论，由调用方下一轮重新检查
func (watcher *accountWatcher) contentAuthUpdated(
	summary aistudio.AccountLoginSummary, disk aistudio.StorageState, stat authFileStat, byContent bool,
) (bool, bool) {
	// 组织停用账户（见 org_disabled.go）同样按文件内容判断：在组织重新开放后同一会话里重新导出的认证文件，登录签名 Cookie 不变
	if !byContent && !watcher.admin.service.watchedLogin(summary.ID) {
		return false, true
	}
	memory, ok := watcher.admin.pool.StorageStateOf(summary.ID)
	if !ok {
		return false, true
	}
	if info, err := os.Stat(summary.StoragePath); err != nil || !info.ModTime().Equal(stat.modTime) || info.Size() != stat.size {
		return false, false
	}
	identity := aistudio.LoginIdentity(disk)
	if identity != aistudio.LoginIdentity(memory) || storageStateDigest(disk) != storageStateDigest(memory) {
		return true, true
	}
	// 登录签名与登记时记下的不同（本服务自己写回时换掉了登录签名 Cookie）：两张登记表各自更新（没有登记或相同时不动）
	watcher.admin.service.noteLoginIdentity(summary.ID, identity)
	return false, true
}

// noteAuthUnchanged 健康账户（或只是手动停用的账户）换上的认证文件与当前登录状态相同（重复上传同一份）：不重新载入、不重新验证，
// 写一行提示并计入新账户自动处理面板的“认证文件未变化”。每次外部改动写一行。处于失效类状态的账户不走这里，按重新上传处理
func (admin *runtimeAdmin) noteAuthUnchanged(summary aistudio.AccountLoginSummary) {
	state := "正常"
	if !summary.Enabled {
		state = "手动停用"
	}
	admin.requests.log("auth", "INFO", fmt.Sprintf(
		"认证文件已更新，但内容与当前登录状态相同，未重新认证 | 账户=%s | 状态=%s | 需要换登录时请重新获取认证文件，或在管理页点“验证”",
		summary.ID, state,
	))
	admin.onboard.noteUnchanged()
}

// noteOwnedRewrite 认证文件被重新写入（修改时间或大小变了），内容却与本服务上次读入或写入的逐字节相同、修改时间也不晚于那一次
// （见 aistudio.AccountPool.StorageFileOwned）：按本服务自己的文件处理，不算重新上传。修改时间与那一次不同（变早）时多半是上传工具
// 保留了原文件的修改时间（WinSCP 默认、rsync -a、scp -p、cp -p、解压），失效账户写一行提示并计入面板“未按重新上传处理”，
// 原先完全静默，看起来“上传后没变化”。failure 为调用方已查的失效类状态（为空时这里查）；本服务刚写的（修改时间与大小与那一次相同）
// 与健康账户不写
func (watcher *accountWatcher) noteOwnedRewrite(summary aistudio.AccountLoginSummary, stat authFileStat, failure string) {
	if watcher.admin.pool.StorageFileSynced(summary.ID, stat.modTime, stat.size) {
		return
	}
	if failure == "" {
		failure = watcher.admin.reauthFailure(summary)
	}
	if failure == "" {
		return
	}
	watcher.admin.requests.log("auth", "INFO", fmt.Sprintf(
		"认证文件被重新写入，但内容与本服务上次读入或写入的相同、修改时间没有变新，按原来的文件处理，未重新认证 | 账户=%s | 状态=%s | "+
			"多半是上传时保留了原文件的修改时间：重新上传时不要保留时间戳（WinSCP 取消“保留时间戳”，rsync 不用 -a/-t，scp 不用 -p），"+
			"上传后 touch 一下，或在管理页对该账户点“验证”",
		summary.ID, failure,
	))
	// 面板上计入“失效账户的上传未按重新上传处理”：原先只有这一行日志，面板上什么都看不出来
	watcher.admin.onboard.noteNotReuploaded()
}

// noteSameLoginAuthFile 就绪的启用账户换上的认证文件内容有变化、登录签名 Cookie 没变（同一会话重新导出，或共用账户目录的其他进程
// 回写了轮换 Cookie 之外的 Cookie）：按原口径不重新载入、不重新验证，写一行提示并计入面板（同一账户每小时最多一次，见 hintDue）。
// 原先完全静默
func (watcher *accountWatcher) noteSameLoginAuthFile(summary aistudio.AccountLoginSummary, now time.Time) {
	if !watcher.hintDue("same-login", summary.ID, now) {
		return
	}
	watcher.admin.requests.log("auth", "INFO", fmt.Sprintf(
		"认证文件内容有变化，但登录签名 Cookie 没变（同一会话重新导出，或共用账户目录的其他进程回写）；账户状态正常，未重新载入、未重新验证 | "+
			"账户=%s | 换了登录（登录签名不同）的认证文件才会重新认证；同一账户每小时最多提示一次",
		summary.ID,
	))
	watcher.admin.onboard.noteSameLogin()
}

// noteWriteBackAuthFile 失效账户的认证文件只差轮换 Cookie 或 AI Studio 的 localStorage（多了、换了，older 为真时是比当前登录状态旧
// 或缺了几项），其余相同，又不是本服务读入或写过的那一份（见 compareAuthFile）：按共用账户目录的其他进程回写处理，不清除旧判定，
// 写一行提示并计入面板“未按重新上传处理”（同一账户每小时最多一次，见 hintDue），原先完全静默。在官网解开 403 后同一会话重新导出（只有轮换 Cookie 更新）、
// 重启之后原样重新上传最初那份文件（这期间本服务只换过轮换 Cookie 与 localStorage）时也是这样，提示里写明怎么处理
func (watcher *accountWatcher) noteWriteBackAuthFile(summary aistudio.AccountLoginSummary, failure string, older bool, now time.Time) {
	if !watcher.hintDue("write-back", summary.ID, now) {
		return
	}
	difference := "只多了或换了 Google 轮换的 Cookie、AI Studio 的 localStorage"
	if older {
		difference = "只是 Google 轮换的 Cookie 或 AI Studio 的 localStorage 比当前登录状态旧"
	}
	watcher.admin.requests.log("auth", "INFO", fmt.Sprintf(
		"认证文件%s，其余与当前登录状态相同，也不是本服务读入或写过的那一份：按共用账户目录的其他进程回写处理，未清除旧判定、未重新认证 | "+
			"账户=%s | 状态=%s | 如果是你重新上传的（例如在官网解开 403 后同一会话重新导出、较早导出的文件），请在管理页对该账户点“验证”"+
			"（验证通过即清除 403 与冷却），或重新导出后再上传；同一账户每小时最多提示一次",
		difference, summary.ID, failure,
	))
	watcher.admin.onboard.noteNotReuploaded()
}

// authFileUnusableError 为新认证文件读不出来或不能签名（缺登录签名 Cookie）：Error 与原来相同（写进“重新载入认证文件失败”），
// reason 与 cause 为管理页显示的新原因（见 markUnusableAuthFile；cause 只含 Cookie 名，不含值）
type authFileUnusableError struct {
	reason string
	cause  error
	err    error
}

func (e *authFileUnusableError) Error() string { return e.err.Error() }

func (e *authFileUnusableError) Unwrap() error { return e.err }

// markUnusableAuthFile 需要登录的启用账户换上的新认证文件读不出来或不能签名：账户仍需要登录，把显示的原因换成这次的
// （prefix + 错误摘要，错误只含 Cookie 名、不含值），不再显示上一次的旧原因。其他状态的账户不改
func (admin *runtimeAdmin) markUnusableAuthFile(summary aistudio.AccountLoginSummary, prefix string, cause error) {
	if !summary.Enabled || summary.State != aistudio.AccountAuthRequired || cause == nil {
		return
	}
	if err := admin.pool.MarkAuthRequired(summary.ID, prefix+authRequiredCauseSummary(cause)); err != nil {
		return
	}
	admin.service.publishModelAccess()
}

// startLoginRefresh 在后台载入检测到的登录状态更新；上一批未完成时不启动新的一批
func (watcher *accountWatcher) startLoginRefresh(ctx context.Context, known map[string]aistudio.AccountLoginSummary) {
	if watcher.refreshing.Load() {
		return
	}
	watcher.loginMu.Lock()
	batch := make([]aistudio.AccountLoginSummary, 0, loginRefreshPerScan)
	sameLogin := make(map[string]*aistudio.StorageDigest)
	for id := range watcher.loginPending {
		delete(watcher.loginPending, id)
		digest, same := watcher.loginSame[id]
		delete(watcher.loginSame, id)
		summary, exists := known[id]
		if !exists || summary.StoragePath == "" {
			continue
		}
		batch = append(batch, summary)
		if same {
			sameLogin[summary.ID] = &digest
		}
		if len(batch) >= loginRefreshPerScan {
			break
		}
	}
	watcher.loading = len(batch)
	watcher.publishBacklogLocked()
	watcher.loginMu.Unlock()
	if len(batch) == 0 {
		return
	}
	watcher.refreshing.Store(true)
	go func() {
		defer watcher.refreshing.Store(false)
		defer func() {
			// 这一批结束（含服务停止时没来得及载入的）：不再计入等待载入
			watcher.loginMu.Lock()
			watcher.loading = 0
			watcher.publishBacklogLocked()
			watcher.loginMu.Unlock()
		}()
		limiter := make(chan struct{}, loginRefreshConcurrency)
		// 这一批载入的新认证文件里的设备绑定会话，批量覆盖已有账户的 storage-state.json 时集中失效最常见：整批结束后写一行
		devices := &deviceBoundTally{}
		var group sync.WaitGroup
		for _, summary := range batch {
			if ctx.Err() != nil {
				break
			}
			limiter <- struct{}{}
			group.Add(1)
			go func(summary aistudio.AccountLoginSummary, same *aistudio.StorageDigest) {
				defer group.Done()
				defer func() { <-limiter }()
				applied := watcher.admin.applyLoginUpdateMode(ctx, summary, devices, same)
				watcher.loginMu.Lock()
				defer watcher.loginMu.Unlock()
				watcher.loading = max(watcher.loading-1, 0)
				// 已处理的不重新排队；文件在检测之后又被换掉的（loginReplaced）同样不重新排队：扫描记下的是检测时的文件状态，下一轮看到
				// 新文件的修改时间，等它稳定后重新检查。账户正在处理请求（loginBusy）时下一轮再试（这期间又有内容变了的新文件时以新文件为准）
				if _, pending := watcher.loginPending[summary.ID]; applied == loginBusy && !pending {
					watcher.loginPending[summary.ID] = struct{}{}
					if same != nil {
						if watcher.loginSame == nil {
							watcher.loginSame = make(map[string]aistudio.StorageDigest)
						}
						watcher.loginSame[summary.ID] = *same
					}
				}
				watcher.publishBacklogLocked()
			}(summary, sameLogin[summary.ID])
		}
		group.Wait()
		devices.log(watcher.admin.requests, "认证文件更新时")
	}()
}

// loginApply 为载入一次认证文件更新的结果（见 applyLoginUpdateMode）
type loginApply int

const (
	// loginApplied 已处理（失败的已写日志）
	loginApplied loginApply = iota
	// loginBusy 账户正在处理请求，下一轮再试
	loginBusy
	// loginReplaced 认证文件在检测之后、写回之前又被换掉（见 errAuthFileReplaced）：这次不写回，由下一轮扫描按新的文件重新检查
	loginReplaced
)

// errAuthFileReplaced 为载入认证文件更新时，文件在检测（或读入）之后、取得账户租约之前又被换掉：新的读不出来或不能签名，
// 或原样重新上传检测之后又上传了别的文件。不写回，由下一轮扫描按新的文件重新检查（见 leaseLoginState）
var errAuthFileReplaced = errors.New("认证文件在载入前又被替换")

// applyLoginUpdate 载入外部更新的登录状态；返回 false 表示账户正在处理请求（或文件刚又被换掉），需要下一轮再试
func (admin *runtimeAdmin) applyLoginUpdate(ctx context.Context, summary aistudio.AccountLoginSummary) bool {
	return admin.applyLoginUpdateCounting(ctx, summary, nil)
}

// applyLoginUpdateCounting 与 applyLoginUpdate 相同，载入了新的认证文件时把它计入 devices（设备绑定会话统计，可为 nil）
func (admin *runtimeAdmin) applyLoginUpdateCounting(
	ctx context.Context, summary aistudio.AccountLoginSummary, devices *deviceBoundTally,
) bool {
	return admin.applyLoginUpdateMode(ctx, summary, devices, nil) == loginApplied
}

// applyLoginUpdateMode 载入外部更新的认证文件（same 非空时是失效账户重新上传了与当前登录状态相同的文件，*same 为检测时那份文件原始内容
// 的指纹：不读文件，沿用当前登录状态，同样关闭旧 Worker、推进认证代际、清除旧判定）；返回 loginBusy 表示账户正在处理请求，需要下一轮再试，
// loginReplaced 表示文件在检测之后又被换掉、这次没有写回（见 leaseLoginState）。
// 载入之后仍是启用的账户（含按默认启用的停用账户）加入新账户自动处理队列，按新的 Cookie 验证一次（见 queueReauth）；
// 手动停用的账户只换认证、保持停用，不验证，在面板上记为“跳过（手动停用）”
func (admin *runtimeAdmin) applyLoginUpdateMode(
	ctx context.Context, summary aistudio.AccountLoginSummary, devices *deviceBoundTally, same *aistudio.StorageDigest,
) loginApply {
	counted := func() {
		if state, ok := admin.pool.StorageStateOf(summary.ID); ok && devices != nil {
			devices.add(summary.ID, state)
		}
	}
	source := reauthSourceAuthFile
	if same != nil {
		source = reauthSourceSameFile
	}
	replaced := func() loginApply {
		admin.requests.log("auth", "INFO",
			"认证文件在载入前又被替换（检测之后又上传了新的文件，或在管理页重新登录、续签），这次不写回，下一轮扫描按新的文件重新检查 | 账户="+summary.ID)
		return loginReplaced
	}
	if summary.Enabled && summary.State == aistudio.AccountAuthRequired {
		// 需要登录的账户：完整重新载入，恢复调度并重新同步模型目录。需要登录的账户仍有未结束的租约时也按此处理
		// （不再显示为忙碌），等不到账户空闲就下一轮再试
		reloaded, err := admin.reloadAccountLogin(ctx, summary.ID, summary.StoragePath, same)
		if errors.Is(err, errAuthFileReplaced) {
			return replaced()
		}
		if err != nil {
			admin.requests.log("auth", "WARN", fmt.Sprintf(
				"重新载入认证文件失败 | 账户=%s | 错误=%s", summary.ID, strings.TrimSpace(err.Error()),
			))
			var unusable *authFileUnusableError
			if errors.As(err, &unusable) {
				admin.markUnusableAuthFile(summary, unusable.reason, unusable.cause)
			}
			return loginApplied
		}
		if !reloaded {
			return loginBusy
		}
		counted()
		if same != nil {
			admin.requests.log("auth", "INFO", "认证文件已重新上传（内容与当前登录状态相同），已清除旧判定并恢复调度 | 账户="+summary.ID)
		} else {
			admin.requests.log("auth", "INFO", "检测到新的认证文件，已重新载入并恢复调度 | 账户="+summary.ID)
		}
		admin.queueReauthIfEnabled(summary.ID, source)
		return loginApplied
	}
	refreshed, err := admin.refreshAccountLoginFrom(ctx, summary.ID, summary.StoragePath, same)
	if errors.Is(err, errAuthFileReplaced) {
		return replaced()
	}
	if err != nil {
		admin.requests.log("auth", "WARN", fmt.Sprintf(
			"载入更新后的认证文件失败 | 账户=%s | 错误=%s", summary.ID, strings.TrimSpace(err.Error()),
		))
		return loginApplied
	}
	if !refreshed {
		return loginBusy
	}
	counted()
	switch {
	case summary.Enabled && same != nil:
		admin.requests.log("auth", "INFO", "认证文件已重新上传（内容与当前登录状态相同），已清除旧判定并重新认证 | 账户="+summary.ID)
	case summary.Enabled:
		admin.requests.log("auth", "INFO", "检测到认证文件更新，已载入新的登录 Cookie | 账户="+summary.ID)
	case accountDisableNotes.keepDisabled(summary.ID):
		if same != nil {
			admin.requests.log("auth", "INFO", "认证文件已重新上传（内容与当前登录状态相同），已清除旧判定；账户是手动停用的，保持停用 | 账户="+summary.ID)
		} else {
			admin.requests.log("auth", "INFO", "检测到认证文件更新，已载入新的登录 Cookie；账户是手动停用的，保持停用 | 账户="+summary.ID)
		}
		admin.onboard.noteReauthResult(summary.ID, source, onboardSkipped, onboardManuallyDisabled)
		return loginApplied
	default:
		// 账户默认启用：自动停用（新账户自动处理验证未通过等）或没有停用记录的账户换了新的登录后直接启用并同步模型目录，
		// 登录仍然无效的由模型目录同步标为需要登录；原先没有停用记录的按手动停用处理，与重启后默认启用的规则不一致
		if !admin.enableAfterLoginUpdate(ctx, summary.ID) {
			return loginBusy
		}
	}
	admin.queueReauthIfEnabled(summary.ID, source)
	return loginApplied
}

// queueReauthIfEnabled 把刚换上新认证文件、当前是启用状态的账户加入新账户自动处理队列验证一次（见 accountOnboarder.queueReauth）；
// 停用的账户（手动停用、启用失败）不加入，手动停用的在面板上记为“跳过（手动停用）”，其他停用账户（启用失败、正忙）记为跳过并写明原因
func (admin *runtimeAdmin) queueReauthIfEnabled(accountID string, source string) {
	status, ok := admin.pool.StatusOf(accountID)
	switch {
	case !ok:
	case status.Enabled:
		admin.onboard.queueReauth(accountID, source)
	case accountDisableNotes.keepDisabled(accountID):
		admin.onboard.noteReauthResult(accountID, source, onboardSkipped, onboardManuallyDisabled)
	default:
		admin.onboard.noteReauthResult(accountID, source, onboardSkipped, reauthStillDisabledDetail)
	}
}

// enableAfterLoginUpdate 启用认证文件刚更新的停用账户并同步它的模型目录；返回 false 表示账户正忙，下一轮再试。
// 按默认启用属于自动启用：在账户租约内再看一次停用记录，刚被手动停用的账户保持停用、记录保留
func (admin *runtimeAdmin) enableAfterLoginUpdate(ctx context.Context, accountID string) bool {
	enabled, busy := admin.enableAutoDisabled(ctx, accountID, "检测到认证文件更新")
	if busy {
		return false
	}
	if enabled {
		if account, err := admin.pool.Account(accountID); err == nil {
			admin.syncAccountModelCatalog(ctx, account)
		}
	}
	return true
}

// enableAutoDisabled 按默认启用换上了新认证的停用账户（不同步模型目录，见 enableAfterLoginUpdate），返回是否启用了、是否因账户正忙
// 没有启用。手动停用的（租约内再看一次停用记录）保持停用，启用失败写一行日志；subject 为日志开头（“检测到认证文件更新”、Chrome 重新导入的来源）
func (admin *runtimeAdmin) enableAutoDisabled(ctx context.Context, accountID string, subject string) (bool, bool) {
	enabled, err := admin.enableAccountConfig(ctx, accountID, true)
	if err != nil {
		if accountBusyError(err) {
			return false, true
		}
		if errors.Is(err, errAccountManuallyDisabled) {
			admin.requests.log("auth", "INFO", subject+"，已载入新的登录 Cookie；账户是手动停用的，保持停用 | 账户="+accountID)
			return false, false
		}
		admin.requests.log("auth", "WARN", fmt.Sprintf(
			"%s，启用账户失败 | 账户=%s | 错误=%s", subject, accountID, strings.TrimSpace(err.Error()),
		))
		return false, false
	}
	if !enabled {
		return false, false
	}
	admin.service.markModelsChanged()
	admin.requests.log("auth", "INFO", subject+"，已载入新的登录 Cookie 并启用账户（没有手动停用记录） | 账户="+accountID)
	return true, false
}

// refreshAccountLogin 用磁盘上更新过的认证文件替换账户的登录状态，并重启该账户的浏览器（没有运行中的浏览器时
// 什么都不做），让浏览器与签名使用同一份新 Cookie。不重置模型目录、不重新同步；只在账户空闲时进行，
// 返回 false 表示账户正在处理请求
func (admin *runtimeAdmin) refreshAccountLogin(ctx context.Context, accountID string, storagePath string) (bool, error) {
	return admin.refreshAccountLoginFrom(ctx, accountID, storagePath, nil)
}

// refreshAccountLoginFrom 与 refreshAccountLogin 相同；same 非空时（失效账户重新上传了与当前登录状态相同的文件，*same 为检测时那份文件
// 原始内容的指纹）不读文件，在账户租约内取当前的登录状态写回（推进认证代际），同样关闭旧 Worker、清除旧判定。文件在检测或读入之后
// 又被换掉时返回 errAuthFileReplaced（见 leaseLoginState）
func (admin *runtimeAdmin) refreshAccountLoginFrom(
	ctx context.Context, accountID string, storagePath string, same *aistudio.StorageDigest,
) (bool, error) {
	generation := admin.pool.AuthGeneration(accountID)
	state, digest, err := admin.loginSourceState(accountID, storagePath, same)
	if err != nil {
		return false, err
	}
	if _, err := aistudio.NewSigner().Sign(state); err != nil {
		return false, fmt.Errorf("认证状态无法用于 AI Studio: %w", err)
	}
	acquireCtx, cancel := context.WithTimeout(ctx, loginRefreshAcquireWait)
	defer cancel()
	lease, err := admin.pool.AcquireAccount(acquireCtx, accountID)
	if err != nil {
		if acquireCtx.Err() != nil && ctx.Err() == nil {
			return false, nil
		}
		return false, err
	}
	state, digest, err = admin.leaseLoginState(accountID, storagePath, same, state, digest, generation)
	if err != nil {
		return false, errors.Join(err, lease.Release())
	}
	if err := admin.workers.ResetFor(accountID, workerStopCredentials); err != nil {
		return false, errors.Join(err, lease.Release())
	}
	if err := lease.SaveLoadedStorageState(state, digest); err != nil {
		return false, errors.Join(err, lease.Release())
	}
	// 新的认证文件可能已经解决了 403 无权限（多半是之前没有同意 Google 的政策）：释放账户之前清除记录与冷却，释放后立即重新参与调度
	admin.service.forgetPermissionDenied(accountID, "认证文件更新")
	if err := lease.Release(); err != nil {
		return false, err
	}
	// 与 reloadAccountCredentials 同一处理：清掉疑似失效、登录复核记录与疑似登录失效冷却（原先这条路径只清 403），旧的判定不跟到新登录上
	admin.service.noteLoginRestored(accountID)
	return true, nil
}

// accountFilesSettled 判断目录与认证文件都已有一段时间没有变化
func accountFilesSettled(directory string, now time.Time) bool {
	paths := []string{
		directory,
		filepath.Join(directory, "account.json"),
		aistudio.StorageStatePath(directory),
	}
	for _, path := range paths {
		info, err := os.Stat(path)
		if err != nil || now.Sub(info.ModTime()) < accountSettleTime {
			return false
		}
	}
	return true
}

// registerExistingAccount 把磁盘上已存在的账户目录加入当前生成服务；失败时不删除目录
func (admin *runtimeAdmin) registerExistingAccount(ctx context.Context, account *aistudio.Account) error {
	if err := admin.headers.Add(account); err != nil {
		return err
	}
	if err := admin.workers.Add(account); err != nil {
		return errors.Join(err, admin.headers.Remove(account.ID))
	}
	if err := admin.service.changeModels(func() error {
		return admin.pool.Add(account)
	}); err != nil {
		return errors.Join(err, admin.workers.Remove(account.ID), admin.headers.Remove(account.ID))
	}
	// 账户已进入账户池：IP 池记下它的固定代理并推送默认出口；旧生成服务的目录监视在这里被忽略（推送目标已是新账户池）
	admin.ipPool().refresh(admin.pool, account.ID)
	return nil
}

// syncImportedCatalogs 并发同步新导入账户的模型目录，全部完成后统一推送一次管理页数据
func (admin *runtimeAdmin) syncImportedCatalogs(ctx context.Context, accounts []*aistudio.Account) {
	limiter := make(chan struct{}, importCatalogConcurrency)
	var syncs sync.WaitGroup
	for _, account := range accounts {
		syncs.Add(1)
		go func(account *aistudio.Account) {
			defer syncs.Done()
			select {
			case limiter <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-limiter }()
			models, err := admin.service.syncAccountModels(ctx, account.ID)
			if err == nil && len(models) > 0 {
				admin.requests.log(account.Config.Label, "INFO", fmt.Sprintf("账户模型目录同步完成 | 模型=%d", len(models)))
			}
		}(account)
	}
	syncs.Wait()
	if ctx.Err() == nil {
		admin.service.publishModelAccess()
	}
}

// detachAccount 把账户移出账户池但不删除任何文件（目录已经不存在）
func (admin *runtimeAdmin) detachAccount(accountID string) error {
	err := admin.service.changeModels(func() error {
		_, removeErr := admin.pool.Remove(accountID, func(account *aistudio.Account) error {
			return admin.workers.ResetFor(account.ID, workerStopDetach)
		})
		return removeErr
	})
	if err != nil {
		return accountOperationError(err)
	}
	admin.service.removeAccountModelRetry(accountID)
	admin.onboard.forget(accountID)
	// 只清除自动停用记录：手动停用记录要留到目录放回之后，账户随 account.json 的停用状态保持停用。原先一并清除，
	// 整体替换账户目录、rsync 较慢时，手动停用的账户放回后没有记录，按默认启用。手动停用记录只在删除账户
	// （DeleteAccount）或管理员启用时清除
	accountDisableNotes.clearAutomatic(accountID)
	admin.service.permissionDenied.clearAccount(accountID)
	admin.service.orgDisabled.clearAccount(accountID)
	// Build 生成的按账号暂停记录随账户移出删除：目录放回后加入的是新的账户（没有暂停标记），从头判定
	admin.service.buildPauses().forget(accountID)
	return errors.Join(admin.workers.Remove(accountID), admin.headers.Remove(accountID))
}

// credentialReloadAcquireWait 为重新载入需要登录账户的认证文件时等待账户空闲的最长时间
var credentialReloadAcquireWait = 10 * time.Second

// reloadAccountCredentials 用磁盘上的新认证文件替换账户的认证状态并恢复调度；
// 返回 false 表示账户仍有请求未结束、等待超时，需要下一轮再试
func (admin *runtimeAdmin) reloadAccountCredentials(ctx context.Context, accountID string, storagePath string) (bool, error) {
	return admin.reloadAccountLogin(ctx, accountID, storagePath, nil)
}

// reloadAccountLogin 与 reloadAccountCredentials 相同；same 非空时（需要登录的账户重新上传了与当前登录状态相同的文件）不读文件，
// 在账户租约内取当前的登录状态写回（推进认证代际），同样关闭旧 Worker、标为就绪、清除旧判定并同步一次模型目录。
// 文件在检测或读入之后又被换掉时返回 errAuthFileReplaced（见 leaseLoginState）
func (admin *runtimeAdmin) reloadAccountLogin(
	ctx context.Context, accountID string, storagePath string, same *aistudio.StorageDigest,
) (bool, error) {
	generation := admin.pool.AuthGeneration(accountID)
	state, digest, err := admin.loginSourceState(accountID, storagePath, same)
	if err != nil {
		return false, &authFileUnusableError{reason: "新的认证文件无法读取：", cause: err, err: err}
	}
	if _, err := aistudio.NewSigner().Sign(state); err != nil {
		return false, &authFileUnusableError{
			reason: "新的认证文件无法用于 AI Studio：", cause: err, err: fmt.Errorf("认证状态无法用于 AI Studio: %w", err),
		}
	}
	acquireCtx, cancel := context.WithTimeout(ctx, credentialReloadAcquireWait)
	defer cancel()
	lease, err := admin.pool.AcquireAccount(acquireCtx, accountID)
	if err != nil {
		if acquireCtx.Err() != nil && ctx.Err() == nil {
			return false, nil
		}
		return false, accountOperationError(err)
	}
	account := lease.Account()
	state, digest, err = admin.leaseLoginState(accountID, storagePath, same, state, digest, generation)
	if err != nil {
		return false, errors.Join(err, lease.Release())
	}
	if err := admin.workers.ResetFor(account.ID, workerStopCredentials); err != nil {
		return false, errors.Join(err, lease.Release())
	}
	if err := lease.SaveLoadedStorageState(state, digest); err != nil {
		return false, errors.Join(err, lease.Release())
	}
	// 不清空模型目录：同一账户换了认证文件，目录与权益由随后的同步刷新纠正，同步失败时旧目录仍在、账号照常可调度
	if err := admin.service.changeModels(func() error {
		return errors.Join(
			admin.pool.MarkReady(account.ID),
			admin.pool.ResetModelAccess(account.ID),
		)
	}); err != nil {
		return false, errors.Join(err, lease.Release())
	}
	// 同 refreshAccountLogin：新的认证文件可能已经解决了 403 无权限，释放账户之前清除记录与冷却
	admin.service.forgetPermissionDenied(account.ID, "认证文件更新")
	if err := lease.Release(); err != nil {
		return false, err
	}
	// 清掉疑似失效、登录复核记录与疑似登录失效冷却（与管理验证通过、浏览器登录同一处理）
	admin.service.noteLoginRestored(account.ID)
	admin.syncAccountModelCatalog(ctx, account)
	return true, nil
}

// loginSourceState 返回重新载入要用的登录状态与认证文件原始内容的指纹：磁盘上的认证文件；same 非空时为内存里当前的登录状态与 *same
// （失效账户重新上传了与它相同的文件，见 detectLoginUpdates）
func (admin *runtimeAdmin) loginSourceState(
	accountID string, storagePath string, same *aistudio.StorageDigest,
) (aistudio.StorageState, aistudio.StorageDigest, error) {
	if same == nil {
		return aistudio.LoadStorageStateDigest(storagePath)
	}
	state, ok := admin.pool.StorageStateOf(accountID)
	if !ok {
		return aistudio.StorageState{}, aistudio.StorageDigest{}, fmt.Errorf("%w: %s", aistudio.ErrAccountNotFound, accountID)
	}
	return state, *same, nil
}

// leaseLoginState 在账户租约内确定要写回的登录状态与它来自的认证文件原始内容的指纹（prepared、digest 为取得租约前准备好的，
// 见 loginSourceState；generation 为准备之前读到的认证代际）。等账户空闲最长 2 秒（需要登录的 10 秒），批量重新上传时排队可达
// 几十分钟，这期间用户可能又上传了新文件：
//   - 载入磁盘上的新文件（same 为空）：文件又被换掉（原始内容的指纹变了）时改用租约内重新读到的；读不出来或不能签名时返回
//     errAuthFileReplaced（可能还在写入，下一轮扫描等它稳定后重新检查）。原先写回等待之前读到的内容，覆盖刚上传的新文件。
//     文件是本服务在等待期间自己写的（修改时间与大小同本服务最近一次写入之后相同，例如这期间结束的请求把旧浏览器的 Cookie 合并写回）
//     不算又上传了新文件：认证代际没变时仍写回 prepared，变了（期间在管理页重新登录、Chrome 重新导入或续签）时返回 errAuthFileReplaced。
//     原先把这份“旧登录 Cookie + 新文件残余”当成新上传的写回，新登录丢失，403 登记却照样清除
//   - 原样重新上传（same 非空）：租约内的最新状态（不回退取得租约前合并的轮换 Cookie）；磁盘上已不是检测时那一份、也不是本服务
//     之后自己写的（检测之后又上传了别的文件）时返回 errAuthFileReplaced。原先无条件把内存里的旧登录写回，覆盖刚上传的新文件，
//     之后扫描看到的是本服务自己写的文件，新文件再也不会载入
func (admin *runtimeAdmin) leaseLoginState(
	accountID string, storagePath string, same *aistudio.StorageDigest, prepared aistudio.StorageState, digest aistudio.StorageDigest,
	generation uint64,
) (aistudio.StorageState, aistudio.StorageDigest, error) {
	info, statErr := os.Stat(storagePath)
	state, current, err := aistudio.LoadStorageStateDigest(storagePath)
	if statErr != nil || err != nil {
		return aistudio.StorageState{}, aistudio.StorageDigest{}, errAuthFileReplaced
	}
	if same != nil {
		if current != *same && !admin.pool.StorageFileOwned(accountID, info.ModTime(), current) {
			return aistudio.StorageState{}, aistudio.StorageDigest{}, errAuthFileReplaced
		}
		// 取得租约之前可能又合并了轮换 Cookie：写回租约内的最新值，不回退
		if latest, ok := admin.pool.StorageStateOf(accountID); ok {
			prepared = latest
		}
		return prepared, *same, nil
	}
	if current == digest {
		return prepared, digest, nil
	}
	if admin.pool.StorageFileSynced(accountID, info.ModTime(), info.Size()) {
		if admin.pool.AuthGeneration(accountID) != generation {
			return aistudio.StorageState{}, aistudio.StorageDigest{}, errAuthFileReplaced
		}
		return prepared, digest, nil
	}
	if _, err := aistudio.NewSigner().Sign(state); err != nil {
		return aistudio.StorageState{}, aistudio.StorageDigest{}, errAuthFileReplaced
	}
	return state, current, nil
}
