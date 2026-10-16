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
//   - 认证文件被外部更新（重新登录、后台程序刷新 Cookie）：任何状态的账户都会载入新的登录状态。
//     需要登录的账户随之恢复调度；停用但不是手动停用的账户按默认启用，并重新同步模型目录
type accountWatcher struct {
	admin     *runtimeAdmin
	failures  map[string]string
	missing   map[string]int
	authFiles map[string]authFileStat
	// loginPending 为检测到登录状态更新、尚未载入的账户；账户忙碌时留到下一轮
	loginMu      sync.Mutex
	loginPending map[string]struct{}
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

// detectLoginUpdates 找出认证文件里的登录状态被外部更新的账户：
//   - 回写 Cookie 时已经发现磁盘上的登录状态与内存不同（由 aistudio 记录）
//   - 认证文件的修改时间或大小变了，并且文件里的登录签名 Cookie 与内存中的不同
//
// 只比较登录签名 Cookie（SAPISID 等），本服务自己回写的轮换 Cookie 不会触发；403 无权限账户另按文件内容判断（见 deniedAuthUpdated）。
// 没有变化的文件只做一次 stat，不读取内容；整个过程不在请求路径上
func (watcher *accountWatcher) detectLoginUpdates(summaries []aistudio.AccountLoginSummary, now time.Time) {
	found := aistudio.TakeExternalLoginChanges()
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
			// 第一次看到的认证文件只记作基准、不读取内容（启动时可能有上万个账户）。403 无权限账户例外：载入账户池之后、
			// 第一次扫描之前被替换的文件（例如重启生成服务时正好在等待文件稳定），只记作基准就再也发现不了
			if _, listed := watcher.admin.service.permissionDeniedIdentity(summary.ID); listed {
				if now.Sub(stat.modTime) < accountSettleTime {
					// 仍在写入，下一轮再看
					continue
				}
				state, err := aistudio.LoadStorageState(summary.StoragePath)
				if err != nil {
					watcher.authFiles[summary.ID] = stat
					continue
				}
				refresh, settled := watcher.deniedAuthUpdated(summary, state, stat, true)
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
		state, err := aistudio.LoadStorageState(summary.StoragePath)
		if err != nil {
			watcher.report("login:"+summary.ID, fmt.Sprintf(
				"读取更新后的认证文件失败 | 账户=%s | 错误=%s", summary.ID, strings.TrimSpace(err.Error()),
			))
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
		refresh, settled := watcher.deniedAuthUpdated(summary, state, stat, false)
		if !settled {
			// 比较期间文件又被改写：恢复上一次的基准，下一轮重新检查
			watcher.authFiles[summary.ID] = previous
			continue
		}
		if refresh {
			found = append(found, summary.ID)
		}
	}
	for id := range watcher.authFiles {
		if _, exists := current[id]; !exists {
			delete(watcher.authFiles, id)
		}
	}
	if len(found) == 0 {
		return
	}
	watcher.loginMu.Lock()
	for _, id := range found {
		watcher.loginPending[id] = struct{}{}
	}
	watcher.loginMu.Unlock()
}

// deniedAuthUpdated 检查 403 无权限账户磁盘上的认证文件（调用方已确认文件稳定、已读入 disk），返回是否要载入它
// （由 startLoginRefresh 载入，载入时清除登记与 403 冷却），以及本轮能否下结论。不在登记表里的账户不检查：
//   - 磁盘上的内容与内存不同：本服务写回认证文件时同时更新内存，不同说明文件被外部更新了。同一登录会话里重新导出
//     （在已登录的浏览器里同意政策后重新获取）时登录签名 Cookie 不变，只比较它发现不了，按说明重新上传后账户仍然列着、仍在冷却
//   - 第一次看到（baseline）时磁盘与内存相同、但登录签名与登记时记下的不同：内存里已经是新的登录状态，直接清除登记与 403 冷却。
//     之后的变化只看磁盘与内存是否相同：两者一起变了是本服务自己写回的（续签会换掉登录签名 Cookie），不算重新上传，
//     只更新登记时记下的登录签名，免得下次启动时被当作服务停止期间替换了认证文件
//
// 比较期间文件又被改写（多半是本服务正在写回 Cookie，写盘与更新内存之间可能被读到）时不下结论，由调用方下一轮重新检查
func (watcher *accountWatcher) deniedAuthUpdated(
	summary aistudio.AccountLoginSummary, disk aistudio.StorageState, stat authFileStat, baseline bool,
) (bool, bool) {
	stored, listed := watcher.admin.service.permissionDeniedIdentity(summary.ID)
	if !listed {
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
	switch {
	case stored == "" || identity == stored:
	case baseline:
		watcher.admin.service.forgetPermissionDenied(summary.ID, "认证文件更新")
	default:
		watcher.admin.service.permissionDenied.setIdentity(summary.ID, identity)
	}
	return false, true
}

// startLoginRefresh 在后台载入检测到的登录状态更新；上一批未完成时不启动新的一批
func (watcher *accountWatcher) startLoginRefresh(ctx context.Context, known map[string]aistudio.AccountLoginSummary) {
	if watcher.refreshing.Load() {
		return
	}
	watcher.loginMu.Lock()
	batch := make([]aistudio.AccountLoginSummary, 0, loginRefreshPerScan)
	for id := range watcher.loginPending {
		delete(watcher.loginPending, id)
		summary, exists := known[id]
		if !exists || summary.StoragePath == "" {
			continue
		}
		batch = append(batch, summary)
		if len(batch) >= loginRefreshPerScan {
			break
		}
	}
	watcher.loginMu.Unlock()
	if len(batch) == 0 {
		return
	}
	watcher.refreshing.Store(true)
	go func() {
		defer watcher.refreshing.Store(false)
		limiter := make(chan struct{}, loginRefreshConcurrency)
		var group sync.WaitGroup
		for _, summary := range batch {
			if ctx.Err() != nil {
				break
			}
			limiter <- struct{}{}
			group.Add(1)
			go func(summary aistudio.AccountLoginSummary) {
				defer group.Done()
				defer func() { <-limiter }()
				if !watcher.admin.applyLoginUpdate(ctx, summary) {
					// 账户正在处理请求，下一轮再试
					watcher.loginMu.Lock()
					watcher.loginPending[summary.ID] = struct{}{}
					watcher.loginMu.Unlock()
				}
			}(summary)
		}
		group.Wait()
	}()
}

// applyLoginUpdate 载入外部更新的登录状态；返回 false 表示账户正在处理请求，需要下一轮再试
func (admin *runtimeAdmin) applyLoginUpdate(ctx context.Context, summary aistudio.AccountLoginSummary) bool {
	if summary.Enabled && summary.State == aistudio.AccountAuthRequired {
		// 需要登录的账户：完整重新载入，恢复调度并重新同步模型目录。需要登录的账户仍有未结束的租约时也按此处理
		// （不再显示为忙碌），等不到账户空闲就下一轮再试
		reloaded, err := admin.reloadAccountCredentials(ctx, summary.ID, summary.StoragePath)
		if err != nil {
			admin.requests.log("auth", "WARN", fmt.Sprintf(
				"重新载入认证文件失败 | 账户=%s | 错误=%s", summary.ID, strings.TrimSpace(err.Error()),
			))
			return true
		}
		if !reloaded {
			return false
		}
		admin.requests.log("auth", "INFO", "检测到新的认证文件，已重新载入并恢复调度 | 账户="+summary.ID)
		return true
	}
	refreshed, err := admin.refreshAccountLogin(ctx, summary.ID, summary.StoragePath)
	if err != nil {
		admin.requests.log("auth", "WARN", fmt.Sprintf(
			"载入更新后的认证文件失败 | 账户=%s | 错误=%s", summary.ID, strings.TrimSpace(err.Error()),
		))
		return true
	}
	if !refreshed {
		return false
	}
	switch {
	case summary.Enabled:
		admin.requests.log("auth", "INFO", "检测到认证文件更新，已载入新的登录 Cookie | 账户="+summary.ID)
	case accountDisableNotes.keepDisabled(summary.ID):
		admin.requests.log("auth", "INFO", "检测到认证文件更新，已载入新的登录 Cookie；账户是手动停用的，保持停用 | 账户="+summary.ID)
	default:
		// 账户默认启用：自动停用（新账户自动处理验证未通过等）或没有停用记录的账户换了新的登录后直接启用并同步模型目录，
		// 登录仍然无效的由模型目录同步标为需要登录；原先没有停用记录的按手动停用处理，与重启后默认启用的规则不一致
		return admin.enableAfterLoginUpdate(ctx, summary.ID)
	}
	return true
}

// enableAfterLoginUpdate 启用认证文件刚更新的停用账户并同步它的模型目录；返回 false 表示账户正忙，下一轮再试。
// 按默认启用属于自动启用：在账户租约内再看一次停用记录，刚被手动停用的账户保持停用、记录保留
func (admin *runtimeAdmin) enableAfterLoginUpdate(ctx context.Context, accountID string) bool {
	enabled, err := admin.enableAccountConfig(ctx, accountID, true)
	if err != nil {
		if accountBusyError(err) {
			return false
		}
		if errors.Is(err, errAccountManuallyDisabled) {
			admin.requests.log("auth", "INFO", "检测到认证文件更新，已载入新的登录 Cookie；账户是手动停用的，保持停用 | 账户="+accountID)
			return true
		}
		admin.requests.log("auth", "WARN", fmt.Sprintf(
			"检测到认证文件更新，启用账户失败 | 账户=%s | 错误=%s", accountID, strings.TrimSpace(err.Error()),
		))
		return true
	}
	if !enabled {
		return true
	}
	admin.service.markModelsChanged()
	admin.requests.log("auth", "INFO", "检测到认证文件更新，已载入新的登录 Cookie 并启用账户（没有手动停用记录） | 账户="+accountID)
	if account, err := admin.pool.Account(accountID); err == nil {
		admin.syncAccountModelCatalog(ctx, account)
	}
	return true
}

// refreshAccountLogin 用磁盘上更新过的认证文件替换账户的登录状态，并重启该账户的浏览器（没有运行中的浏览器时
// 什么都不做），让浏览器与签名使用同一份新 Cookie。不重置模型目录、不重新同步；只在账户空闲时进行，
// 返回 false 表示账户正在处理请求
func (admin *runtimeAdmin) refreshAccountLogin(ctx context.Context, accountID string, storagePath string) (bool, error) {
	state, err := aistudio.LoadStorageState(storagePath)
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
	if err := admin.workers.Reset(accountID); err != nil {
		return false, errors.Join(err, lease.Release())
	}
	if err := lease.SaveStorageState(state); err != nil {
		return false, errors.Join(err, lease.Release())
	}
	// 新的认证文件可能已经解决了 403 无权限（多半是之前没有同意 Google 的政策）：释放账户之前清除记录与冷却，释放后立即重新参与调度
	admin.service.forgetPermissionDenied(accountID, "认证文件更新")
	return true, lease.Release()
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
			return admin.workers.Reset(account.ID)
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
	return errors.Join(admin.workers.Remove(accountID), admin.headers.Remove(accountID))
}

// credentialReloadAcquireWait 为重新载入需要登录账户的认证文件时等待账户空闲的最长时间
var credentialReloadAcquireWait = 10 * time.Second

// reloadAccountCredentials 用磁盘上的新认证文件替换账户的认证状态并恢复调度；
// 返回 false 表示账户仍有请求未结束、等待超时，需要下一轮再试
func (admin *runtimeAdmin) reloadAccountCredentials(ctx context.Context, accountID string, storagePath string) (bool, error) {
	state, err := aistudio.LoadStorageState(storagePath)
	if err != nil {
		return false, err
	}
	if _, err := aistudio.NewSigner().Sign(state); err != nil {
		return false, fmt.Errorf("认证状态无法用于 AI Studio: %w", err)
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
	if err := admin.workers.Reset(account.ID); err != nil {
		return false, errors.Join(err, lease.Release())
	}
	if err := lease.SaveStorageState(state); err != nil {
		return false, errors.Join(err, lease.Release())
	}
	if err := admin.service.changeModels(func() error {
		return errors.Join(
			admin.pool.MarkReady(account.ID),
			admin.pool.ResetModelAccess(account.ID),
			admin.pool.SetCatalog(account.ID, account.BenefitTier, nil),
		)
	}); err != nil {
		return false, errors.Join(err, lease.Release())
	}
	// 同 refreshAccountLogin：新的认证文件可能已经解决了 403 无权限，释放账户之前清除记录与冷却
	admin.service.forgetPermissionDenied(account.ID, "认证文件更新")
	if err := lease.Release(); err != nil {
		return false, err
	}
	admin.syncAccountModelCatalog(ctx, account)
	return true, nil
}
