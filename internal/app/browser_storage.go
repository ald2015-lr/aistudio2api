package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/camoufoxnative"
	"github.com/Mag1cFall/AIStudio2API/internal/proxyrelay"
)

// startupLocalStorageTimeout 为 Worker 启动完成后导出浏览器 localStorage 的上限
const startupLocalStorageTimeout = 5 * time.Second

// persistStartupLocalStorage 在 Worker 启动完成后把浏览器里 aistudio.google.com 的 localStorage 写回账户的认证文件：
// Google 更新网页后，页面会把保存的旧格式设置（aiStudioUserPreference 等）升级成新格式；原先 localStorage 从不写回，
// 老账号每次启动都带着登录当天的旧值。替换该 origin 的 localStorage、保留其他 origin（见 MergeBrowserLocalStorage）。
// generation 为启动前读取的认证代际，期间保存过新登录或续签时不写；同步执行（有上限），在 Worker 发布之前，没有请求同时使用页面。
// 读取或写回失败、超过大小上限只写一条警告（不含值），不影响启动
func (manager *accountWorkerManager) persistStartupLocalStorage(accountID string, generation uint64, preparer *accountWorkerPreparer) {
	if manager == nil || manager.pool == nil || preparer == nil || preparer.worker == nil {
		return
	}
	base := manager.lifecycle
	if base == nil {
		base = context.Background()
	}
	ctx, cancel := context.WithTimeout(base, startupLocalStorageTimeout)
	defer cancel()
	origin, ok, err := preparer.worker.BrowserLocalStorage(ctx)
	if err != nil {
		manager.warnStartupLocalStorage(accountID, err)
		return
	}
	if !ok {
		return
	}
	if _, err := manager.pool.MergeBrowserLocalStorage(accountID, generation, origin); err != nil {
		manager.warnStartupLocalStorage(accountID, err)
	}
}

// warnStartupLocalStorage 记录启动后写回 localStorage 失败；生成服务停止导致的取消不记
func (manager *accountWorkerManager) warnStartupLocalStorage(accountID string, err error) {
	if manager.requests == nil || manager.lifecycle != nil && manager.lifecycle.Err() != nil {
		return
	}
	manager.requests.log(accountID, "WARN", "WAA Worker 启动后写回 localStorage 失败 | 错误="+strings.TrimSpace(err.Error()))
}

// browserCacheHealInterval 为同一账户两次“启动失败后清理浏览器缓存”的最短间隔（内存里的第二道防线；
// 主要的限制是缓存目录里的创建时间，见 camoufoxnative.AccountCacheClearMinAge）
const browserCacheHealInterval = 10 * time.Minute

// browserCacheHealMissLimit 为暂停清理之前允许的连续无效清理数：清理过缓存的账户下一次启动仍以页面状态类失败，记一次无效
const browserCacheHealMissLimit = 20

// browserCacheHealPause 为连续无效后暂停清理的时长
const browserCacheHealPause = time.Hour

// browserCacheHealOutcomeWindow 为等待清理过缓存的账户下一次启动结果的上限，更早的不再计入
const browserCacheHealOutcomeWindow = time.Hour

// healBrowserCache 在页面状态类的启动失败（输入框未就绪、提示词输入框不存在、Run 按钮、WAA service 未暴露等，
// 见 camoufoxnative.PageStateFailure）之后清空账户的浏览器 HTTP 缓存，下次启动重新载入官网页面：缓存跨启动保留，
// 官网更新后里面与新页面不配套的旧资源会让页面长不出这些元素。只在失败的这次启动用上了账户缓存时调用（见 camoufoxnative.Options.CacheAttached，
// 拿不到缓存锁、用临时 profile 内缓存的启动不调用）。调用时 launch 已返回；浏览器确认退出时缓存锁已释放，
// 没能确认退出的浏览器仍持锁（见 camoufoxnative.retainAccountCacheUntilExit）。清理前重新取得缓存锁，取不到（同账户另一个
// runtime 正在用，或浏览器没能确认退出）时不清理；最近 12 小时内新建或清空过的缓存不清理（见 camoufoxnative.ClearAccountCache，
// 按缓存目录里的创建时间判断，重启后仍然有效）。本机代理中转记到上游失败的不算页面问题。
// 同一账户 10 分钟内最多清理一次：清理成功或出错都占用这 10 分钟（出错不反复刷警告），取不到锁或缓存太新时不占用。
// 清理没有用时暂停：连续 20 个清理过缓存的账户下一次启动仍以页面状态类失败（多半是官网整体改版，或失败原因不在缓存），
// 暂停清理 1 小时，免得每个轮到的账户都被清空、下一次都整页冷加载
func (manager *accountWorkerManager) healBrowserCache(accountID string, label string, storageStatePath string, cause error) {
	reason, ok := camoufoxnative.PageStateFailure(cause)
	if !ok || errors.Is(cause, proxyrelay.ErrUpstream) || strings.TrimSpace(storageStatePath) == "" {
		return
	}
	now := time.Now()
	manager.cacheHealMu.Lock()
	manager.noteBrowserCacheHealOutcomeLocked(accountID, false, now)
	if now.Before(manager.cacheHealPausedUntil) {
		manager.cacheHealMu.Unlock()
		return
	}
	if last, seen := manager.cacheHealedAt[accountID]; seen && now.Sub(last) < browserCacheHealInterval {
		manager.cacheHealMu.Unlock()
		return
	}
	if manager.cacheHealedAt == nil {
		manager.cacheHealedAt = make(map[string]time.Time)
	}
	for id, at := range manager.cacheHealedAt {
		if now.Sub(at) >= browserCacheHealInterval {
			delete(manager.cacheHealedAt, id)
		}
	}
	manager.cacheHealedAt[accountID] = now
	manager.cacheHealMu.Unlock()
	cleared, size, err := camoufoxnative.ClearAccountCache(storageStatePath)
	if !cleared && err == nil {
		manager.cacheHealMu.Lock()
		if at, seen := manager.cacheHealedAt[accountID]; seen && at.Equal(now) {
			delete(manager.cacheHealedAt, accountID)
		}
		manager.cacheHealMu.Unlock()
		return
	}
	if cleared {
		manager.cacheHealMu.Lock()
		if manager.cacheHealPending == nil {
			manager.cacheHealPending = make(map[string]time.Time)
		}
		for id, at := range manager.cacheHealPending {
			if now.Sub(at) >= browserCacheHealOutcomeWindow {
				delete(manager.cacheHealPending, id)
			}
		}
		manager.cacheHealPending[accountID] = now
		manager.cacheHealMu.Unlock()
	}
	// 清空成功但释放锁出错时两行都写
	if cleared {
		manager.logBrowserCacheCleared(label, reason, size, nil)
	}
	if err != nil {
		manager.logBrowserCacheCleared(label, reason, size, err)
	}
}

// noteBrowserCacheHealed 记录 Worker 启动成功：账户刚清理过缓存时算一次有效清理，连续无效计数归零
func (manager *accountWorkerManager) noteBrowserCacheHealed(accountID string) {
	manager.cacheHealMu.Lock()
	manager.noteBrowserCacheHealOutcomeLocked(accountID, true, time.Now())
	manager.cacheHealMu.Unlock()
}

// noteBrowserCacheHealOutcomeLocked 记录清理过缓存的账户下一次启动的结果（调用方持有 cacheHealMu）：success 为启动成功，
// 否则为页面状态类失败。不是刚清理过缓存的账户不计；连续无效达到 browserCacheHealMissLimit 时暂停清理 browserCacheHealPause
func (manager *accountWorkerManager) noteBrowserCacheHealOutcomeLocked(accountID string, success bool, now time.Time) {
	healedAt, pending := manager.cacheHealPending[accountID]
	if !pending {
		return
	}
	delete(manager.cacheHealPending, accountID)
	if now.Sub(healedAt) >= browserCacheHealOutcomeWindow {
		return
	}
	if success {
		manager.cacheHealMisses = 0
		return
	}
	manager.cacheHealMisses++
	if manager.cacheHealMisses < browserCacheHealMissLimit {
		return
	}
	manager.cacheHealMisses = 0
	manager.cacheHealPausedUntil = now.Add(browserCacheHealPause)
	if manager.requests != nil {
		manager.requests.log("service", "WARN", fmt.Sprintf(
			"暂停启动失败后的浏览器缓存清理 | 原因=连续 %d 个清理过缓存的账号下一次启动仍失败 | 时长=%s",
			browserCacheHealMissLimit, browserCacheHealPause,
		))
	}
}

// browserCacheExpiredReason 为缓存超过保留期限被清空时日志里的原因
func browserCacheExpiredReason() string {
	return fmt.Sprintf("缓存超过 %d 天", int(camoufoxnative.AccountCacheMaxAge/(24*time.Hour)))
}

// logBrowserCacheCleared 记录一次账户浏览器缓存清理：成功时 INFO，大小为清理前的 MB 数；失败时 WARN
func (manager *accountWorkerManager) logBrowserCacheCleared(label string, reason string, size int64, err error) {
	if manager == nil || manager.requests == nil {
		return
	}
	if err != nil {
		manager.requests.log(label, "WARN", fmt.Sprintf(
			"清理账号浏览器缓存失败 | 原因=%s | 错误=%s", reason, strings.TrimSpace(err.Error()),
		))
		return
	}
	manager.requests.log(label, "INFO", fmt.Sprintf(
		"清理账号浏览器缓存 | 原因=%s | 大小=%.1fMB", reason, float64(size)/(1<<20),
	))
}
