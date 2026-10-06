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
//     需要登录的账户随之恢复调度；自动停用（非手动停用）的账户重新加入新账户自动处理，验证通过后启用
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
	now := time.Now()
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
		if err == nil {
			err = watcher.admin.registerExistingAccount(ctx, account)
		}
		if err != nil {
			watcher.report(directory, fmt.Sprintf(
				"自动导入账户失败 | 目录=%s | 错误=%s", id, strings.TrimSpace(err.Error()),
			))
			continue
		}
		delete(watcher.failures, directory)
		known[id] = aistudio.AccountLoginSummary{ID: account.ID}
		changed = true
		if account.Config.Enabled {
			imported = append(imported, account)
		} else {
			accountDisableNotes.setIfAbsent(account.ID, "导入时 account.json 为停用，等待新账户自动处理验证后启用", true)
		}
		importedIDs = append(importedIDs, account.ID)
		watcher.admin.requests.log("auth", "INFO", "自动导入账户 | 账户="+account.Config.Label)
	}
	// 新账户进入自动处理队列（攒够一批后验证、通过后启用）
	watcher.admin.onboard.enqueue(importedIDs)
	// 模型目录要走网络，放到后台并发同步，不阻塞本轮扫描；批量上传几百个账户时也不会卡住扫描
	if len(imported) > 0 {
		go watcher.admin.syncImportedCatalogs(ctx, imported)
	}
	if watcher.detachMissing(summaries, present) {
		changed = true
	}
	watcher.detectLoginUpdates(summaries, now)
	watcher.startLoginRefresh(ctx, known)
	if changed {
		watcher.admin.syncModelCache()
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
// 只比较登录签名 Cookie（SAPISID 等），本服务自己回写的轮换 Cookie 不会触发。
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
		if ok && memory != aistudio.LoginIdentity(state) {
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
		// 需要登录的账户：完整重新载入，恢复调度并重新同步模型目录
		if err := admin.reloadAccountCredentials(ctx, summary.ID, summary.StoragePath); err != nil {
			admin.requests.log("auth", "WARN", fmt.Sprintf(
				"重新载入认证文件失败 | 账户=%s | 错误=%s", summary.ID, strings.TrimSpace(err.Error()),
			))
			return true
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
	case accountDisableNotes.manual(summary.ID):
		admin.requests.log("auth", "INFO", "检测到认证文件更新，已载入新的登录 Cookie；账户是手动停用的，保持停用 | 账户="+summary.ID)
	case admin.onboard.requeue(summary.ID):
		admin.requests.log("auth", "INFO", "检测到认证文件更新，停用账户已重新加入新账户自动处理，验证通过后启用 | 账户="+summary.ID)
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
	accountDisableNotes.clear(accountID)
	return errors.Join(admin.workers.Remove(accountID), admin.headers.Remove(accountID))
}

// reloadAccountCredentials 用磁盘上的新认证文件替换账户的认证状态并恢复调度
func (admin *runtimeAdmin) reloadAccountCredentials(ctx context.Context, accountID string, storagePath string) error {
	state, err := aistudio.LoadStorageState(storagePath)
	if err != nil {
		return err
	}
	if _, err := aistudio.NewSigner().Sign(state); err != nil {
		return fmt.Errorf("认证状态无法用于 AI Studio: %w", err)
	}
	acquireCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	lease, err := admin.pool.AcquireAccount(acquireCtx, accountID)
	if err != nil {
		return accountOperationError(err)
	}
	account := lease.Account()
	if err := admin.workers.Reset(account.ID); err != nil {
		return errors.Join(err, lease.Release())
	}
	if err := lease.SaveStorageState(state); err != nil {
		return errors.Join(err, lease.Release())
	}
	if err := admin.service.changeModels(func() error {
		return errors.Join(
			admin.pool.MarkReady(account.ID),
			admin.pool.ResetModelAccess(account.ID),
			admin.pool.SetCatalog(account.ID, account.BenefitTier, nil),
		)
	}); err != nil {
		return errors.Join(err, lease.Release())
	}
	if err := lease.Release(); err != nil {
		return err
	}
	admin.syncAccountModelCatalog(ctx, account)
	return nil
}
