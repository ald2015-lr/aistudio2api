package aistudio

import (
	"os"
	"strings"
	"time"
)

// 减少 Cookie 写盘
//
// 每个受保护请求结束后浏览器 Cookie 写回认证文件（ReplaceCookies），Go 通道的响应 Set-Cookie 也逐次合并写回（MergeSetCookieHeaders）。
// Google 几乎每次响应都轮换 SIDCC、__Secure-1PSIDCC、__Secure-3PSIDCC，原先每个请求都要整份读写一次认证 JSON。
// 现在只有这三个轮换 Cookie 的值或过期时间变化、且认证文件在 rotatingCookieWriteInterval 内写过时不立即写盘：
//   - 合并后的完整状态暂存在账户上（deferredStorage，受 storageMu 保护），内存里的 StorageState 照常更新；
//     到写盘间隔时由计时器写盘，期间任何其他写入（登录、续签、Set-Cookie 里的其他 Cookie、localStorage）以暂存状态为底一并写出
//   - 在 storageMu 内读认证文件的路径（ReloadStorageState，即 Go 通道发请求前读取 Cookie，以及各写入路径）先取暂存状态，
//     Worker 冷启动前（浏览器或纯 Go runtime 从文件读 Cookie）先把暂存状态写盘
//   - 生成服务停止时（Worker 都已重置）把全部暂存写盘，之后直接写、不再暂存（SetStorageDeferral）：停止再启动时新一代生成服务
//     从磁盘载入认证文件，读到的是最新的 Cookie；进程关闭时在关闭 Worker 之前与之后各写一次
//   - 认证文件的修改时间在未来（从时钟偏快的机器拷来、解压保留了时间、手动 touch）时按刚写过处理，但最多等 rotatingCookieWriteInterval
//   - 磁盘文件在暂存之后被外部改过（重新上传等，按修改时间与大小判断）或账户认证代际变了（新登录、续签）时丢弃暂存，以磁盘为准
//   - 其他 Cookie 变化照常立即写；账户删除时丢弃暂存
//
// 只影响本机磁盘写入的频率，不改变发给 Google 的任何请求：本进程正常运行与正常停止时，发出的 Cookie 与每次都写盘时相同。
// 进程被强杀（OOM、kill -9、守护脚本判定卡死后结束进程）时每个账号最多丢失 rotatingCookieWriteInterval 内的轮换 Cookie，
// 重启后带的是稍早的值（与浏览器自身约每 30 秒落盘一次 Cookie 相当）

// rotatingCookieNames 为 Google 每次响应都会轮换的 Cookie 名
var rotatingCookieNames = map[string]bool{"SIDCC": true, "__Secure-1PSIDCC": true, "__Secure-3PSIDCC": true}

// rotatingCookieWriteInterval 为只有轮换 Cookie 变化时认证文件的最短写盘间隔
const rotatingCookieWriteInterval = 30 * time.Second

// onlyRotatingCookiesChanged 判断 next 与 previous 是否只在轮换 Cookie 的值与过期时间上不同（完全相同也算）：按 name、domain、path
// 对应（Set-Cookie 合并会把更新的 Cookie 挪到末尾，顺序不算变化），数量与其他字段都要相同；有重复的 Cookie 时按有变化处理
func onlyRotatingCookiesChanged(previous []StateCookie, next []StateCookie) bool {
	if len(previous) != len(next) {
		return false
	}
	byKey := make(map[stateCookieKey]StateCookie, len(previous))
	for _, cookie := range previous {
		key := stateCookieKey{cookie.Name, strings.ToLower(cookie.Domain), cookie.Path}
		if _, duplicate := byKey[key]; duplicate {
			return false
		}
		byKey[key] = cookie
	}
	for _, cookie := range next {
		key := stateCookieKey{cookie.Name, strings.ToLower(cookie.Domain), cookie.Path}
		old, ok := byKey[key]
		if !ok {
			return false
		}
		delete(byKey, key)
		if rotatingCookieNames[cookie.Name] {
			old.Value, old.Expires = cookie.Value, cookie.Expires
		}
		if old != cookie {
			return false
		}
	}
	return true
}

// storageStamp 为认证文件的修改时间与大小，判断暂存之后文件是否被外部改过
type storageStamp struct {
	modTime time.Time
	size    int64
}

// same 判断两次查看的文件状态相同
func (stamp storageStamp) same(other storageStamp) bool {
	return stamp.modTime.Equal(other.modTime) && stamp.size == other.size
}

func statStorage(path string) (storageStamp, bool) {
	info, err := os.Stat(path)
	if err != nil {
		return storageStamp{}, false
	}
	return storageStamp{modTime: info.ModTime(), size: info.Size()}, true
}

// deferredStorage 为还没写盘的认证状态：state 为应当写进文件的完整状态，generation 为暂存时的认证代际，base 为暂存时磁盘文件的状态，
// timer 到写盘间隔（due）时写盘
type deferredStorage struct {
	state      StorageState
	generation uint64
	base       storageStamp
	due        time.Time
	timer      *time.Timer
}

// loadStorageLocked 返回账户认证状态的最新值：有仍然有效的暂存状态时返回它（磁盘文件没被外部改过、认证代际没变），否则读磁盘
// （暂存失效时一并丢弃）。调用方持有 account.storageMu，不持有 p.mu
func (p *AccountPool) loadStorageLocked(account *Account) (StorageState, error) {
	if deferred := account.deferredStorage; deferred != nil {
		if p.deferredValidLocked(account, deferred) {
			return cloneStorageState(deferred.state), nil
		}
		account.dropDeferredStorageLocked()
	}
	return LoadStorageState(account.StoragePath)
}

// deferredValidLocked 判断暂存状态仍然有效。调用方持有 account.storageMu
func (p *AccountPool) deferredValidLocked(account *Account, deferred *deferredStorage) bool {
	stamp, ok := statStorage(account.StoragePath)
	if !ok || !stamp.same(deferred.base) {
		return false
	}
	p.mu.Lock()
	generation := account.authGeneration
	p.mu.Unlock()
	return generation == deferred.generation
}

// writeStorageLocked 把认证状态写盘并丢弃暂存（暂存的内容已经是 state 的底）。调用方持有 account.storageMu
func (account *Account) writeStorageLocked(state StorageState) error {
	account.dropDeferredStorageLocked()
	return WriteStorageState(account.StoragePath, state)
}

// dropDeferredStorageLocked 丢弃暂存状态并停止写盘计时。调用方持有 account.storageMu
func (account *Account) dropDeferredStorageLocked() {
	if deferred := account.deferredStorage; deferred != nil {
		if deferred.timer != nil {
			deferred.timer.Stop()
		}
		account.deferredStorage = nil
	}
}

// persistCookiesLocked 写回合并后的 Cookie：与 previous（当前最新状态，可能是暂存状态）相比只有轮换 Cookie 变化、且认证文件
// rotatingCookieWriteInterval 内写过时暂存（到间隔时写盘），否则立即写盘；Cookie 完全相同时不写（已有的暂存照旧）。
// 调用方持有 account.storageMu，成功后照常把内存里的 StorageState 换成 state
func (p *AccountPool) persistCookiesLocked(account *Account, previous StorageState, state StorageState) error {
	if onlyRotatingCookiesChanged(previous.Cookies, state.Cookies) {
		if storageCookiesEqual(previous.Cookies, state.Cookies) {
			return nil
		}
		if stamp, ok := statStorage(account.StoragePath); ok && !p.storageDirect.Load() {
			if wait := rotatingWriteWait(stamp.modTime, time.Now()); wait > 0 {
				p.deferStorageLocked(account, state, stamp, wait)
				return nil
			}
		}
	}
	return account.writeStorageLocked(state)
}

// rotatingWriteWait 返回认证文件上一次写盘（修改时间 modTime）之后还要等多久才能再写，不大于 0 时立即写。修改时间在未来时
// （从时钟偏快的机器拷来、解压保留了时间、手动 touch）按刚写过处理，但最多等一个写盘间隔，免得暂存的轮换 Cookie 长时间不落盘
func rotatingWriteWait(modTime time.Time, now time.Time) time.Duration {
	return min(rotatingCookieWriteInterval-now.Sub(modTime), rotatingCookieWriteInterval)
}

// deferStorageLocked 暂存认证状态，wait 后写盘（已有计时时沿用：计时按上一次真正写盘的时刻算）。调用方持有 account.storageMu
func (p *AccountPool) deferStorageLocked(account *Account, state StorageState, base storageStamp, wait time.Duration) {
	p.mu.Lock()
	generation := account.authGeneration
	p.mu.Unlock()
	deferred := account.deferredStorage
	if deferred == nil || !deferred.base.same(base) {
		account.dropDeferredStorageLocked()
		deferred = &deferredStorage{base: base, due: time.Now().Add(wait)}
		account.deferredStorage = deferred
		deferred.timer = time.AfterFunc(wait, func() { p.flushDeferredStorage(account, deferred) })
	}
	deferred.state, deferred.generation = cloneStorageState(state), generation
}

// flushDeferredStorage 为写盘计时到期：暂存仍是 deferred 且有效时写盘，否则什么也不做
func (p *AccountPool) flushDeferredStorage(account *Account, deferred *deferredStorage) {
	account.storageMu.Lock()
	defer account.storageMu.Unlock()
	if account.deferredStorage != deferred {
		return
	}
	_ = p.flushDeferredLocked(account)
}

// flushDeferredLocked 把仍然有效的暂存状态写盘（失效的直接丢弃）。调用方持有 account.storageMu
func (p *AccountPool) flushDeferredLocked(account *Account) error {
	deferred := account.deferredStorage
	if deferred == nil {
		return nil
	}
	if !p.deferredValidLocked(account, deferred) {
		account.dropDeferredStorageLocked()
		return nil
	}
	return account.writeStorageLocked(deferred.state)
}

// FlushDeferredStorage 把账户暂存的认证状态立即写盘（见 cookie_write.go）：Worker 冷启动前调用，浏览器与纯 Go runtime 从认证文件读到
// 最新的 Cookie。账户不存在或没有暂存时什么也不做
func (p *AccountPool) FlushDeferredStorage(accountID string) error {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	account := p.byID[accountID]
	p.mu.Unlock()
	if account == nil {
		return nil
	}
	account.storageMu.Lock()
	defer account.storageMu.Unlock()
	return p.flushDeferredLocked(account)
}

// SetStorageDeferral 开关只有轮换 Cookie 变化时的暂存写盘。关闭时先把全部暂存写盘，之后每次都直接写（生成服务停止后：
// 停止再启动时新一代生成服务从磁盘载入认证文件，旧账户池不能再留着没写盘的 Cookie）；生成服务启动时打开。返回写盘遇到的第一个错误
func (p *AccountPool) SetStorageDeferral(enabled bool) error {
	if p == nil {
		return nil
	}
	// 先改标记再写盘：之后在 storageMu 内暂存的路径都会看到标记、直接写；之前暂存的由下面逐个写出
	p.storageDirect.Store(!enabled)
	if enabled {
		return nil
	}
	return p.FlushAllDeferredStorage()
}

// FlushAllDeferredStorage 把全部账户暂存的认证状态写盘（生成服务停止与关闭时），返回遇到的第一个错误
func (p *AccountPool) FlushAllDeferredStorage() error {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	accounts := append([]*Account(nil), p.accounts...)
	p.mu.Unlock()
	var first error
	for _, account := range accounts {
		account.storageMu.Lock()
		if err := p.flushDeferredLocked(account); err != nil && first == nil {
			first = err
		}
		account.storageMu.Unlock()
	}
	return first
}

func storageCookiesEqual(left []StateCookie, right []StateCookie) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

// cloneStorageState 复制认证状态的 Cookie 与 origin 列表，暂存与返回的状态互不影响
func cloneStorageState(state StorageState) StorageState {
	state.Cookies = append([]StateCookie(nil), state.Cookies...)
	origins := make([]StorageOrigin, len(state.Origins))
	for index, origin := range state.Origins {
		origin.LocalStorage = append([]StorageItem(nil), origin.LocalStorage...)
		origins[index] = origin
	}
	if state.Origins == nil {
		origins = nil
	}
	state.Origins = origins
	return state
}
