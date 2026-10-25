package camoufoxnative

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gofrs/flock"
)

// accountCacheDirectoryName 为账户目录下保存 Camoufox HTTP 磁盘缓存的子目录
const accountCacheDirectoryName = "camoufox-cache"

// accountCacheLockName 为缓存目录内由使用者持有的锁文件（start.sh clean-cache 用 flock 命令取同一把锁）
const accountCacheLockName = ".lock"

// accountCacheCreatedName 为缓存目录内记录这份缓存创建时间的标记文件（RFC 3339）
const accountCacheCreatedName = ".created"

// accountCacheCapacityKB 为单个账户 HTTP 磁盘缓存的容量上限
const accountCacheCapacityKB = 262144

// AccountCacheMaxAge 为账户 HTTP 磁盘缓存的保留期限：官网更新页面后，跨启动保留的旧脚本与资源可能和新页面不配套，
// 超过期限的缓存在下次启动锁定时先清空再用
const AccountCacheMaxAge = 3 * 24 * time.Hour

// AccountCacheClearMinAge 为启动失败后自愈清理的最小缓存年龄：创建时间标记距今不到这个时长的缓存（最近新建或清空过）不清理。
// 这样的缓存是最近才建的，不会是官网更新前留下、与新页面不配套的旧资源。原先每次页面状态类失败都清（只有内存里同账户 10 分钟的去重，
// 重启后清零）：失败原因不在缓存的账户每隔 10 分钟就把上一次失败时刚下载的缓存删掉，下一次必然整页冷加载；出口慢时冷加载更容易超时，
// 变成冷加载、失败、清空、再冷加载的循环。标记写在缓存目录里，服务重启后仍然有效
const AccountCacheClearMinAge = 12 * time.Hour

// accountCacheDirectory 返回账户 HTTP 磁盘缓存目录：认证文件所在的账户目录下的 camoufox-cache
func accountCacheDirectory(storageStatePath string) string {
	return filepath.Join(filepath.Dir(filepath.Clean(storageStatePath)), accountCacheDirectoryName)
}

// lockAccountCache 独占账户 HTTP 磁盘缓存目录，同账户另一个 runtime 占用时返回空目录。
// 拿到锁后按创建时间标记处理过期（见 expireAccountCache）：清空过期缓存时调用 expired（含清空失败）。
// 删除前的校验没有通过（例如 camoufox-cache 是符号链接）时什么都没删，照常交给浏览器，与加入过期之前一致；
// 只删了一部分时释放锁，这次启动改用临时 profile 内的缓存
func lockAccountCache(storageStatePath string, expired func(size int64, err error)) (string, *flock.Flock, error) {
	directory := accountCacheDirectory(storageStatePath)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return "", nil, fmt.Errorf("创建账户浏览器缓存目录: %w", err)
	}
	lock := flock.New(filepath.Join(directory, accountCacheLockName))
	locked, err := lock.TryLock()
	if err != nil {
		return "", nil, fmt.Errorf("锁定账户浏览器缓存: %w", err)
	}
	if !locked {
		return "", nil, nil
	}
	cleared, size, err := expireAccountCache(storageStatePath, directory, lock, time.Now())
	if (cleared || err != nil) && expired != nil {
		expired(size, err)
	}
	if err != nil {
		var refused *accountCacheRefusedError
		if errors.As(err, &refused) {
			return directory, lock, nil
		}
		// 清空失败的缓存可能只删了一部分，这一次不交给浏览器
		return "", nil, releaseAccountCache(lock)
	}
	return directory, lock, nil
}

// accountCacheRefusedError 为清理前的校验没有通过、什么都没删的错误（见 checkAccountCacheDirectory），文案与原错误相同
type accountCacheRefusedError struct {
	err error
}

func (refused *accountCacheRefusedError) Error() string { return refused.err.Error() }

func (refused *accountCacheRefusedError) Unwrap() error { return refused.err }

// unconfirmedCacheLocks 为浏览器没能确认退出（关闭超时或出错）时仍持有的账户缓存锁，进程确认退出后释放并移除，由 unconfirmedCacheMu 保护。
// 原先关不掉的浏览器直接返回错误，锁对象随 Worker 一起不再被引用：gofrs/flock 持有的 *os.File 带终结器，下一次 GC 关闭文件描述符，
// flock 随之释放，这时浏览器可能还活着，启动失败后的清理与之后的锁定（含过期清理）就会在它下面删除或共用缓存目录
var (
	unconfirmedCacheMu    sync.Mutex
	unconfirmedCacheLocks = make(map[*flock.Flock]struct{})
)

// retainAccountCacheUntilExit 在浏览器没能确认退出时保留账户缓存锁：放进 unconfirmedCacheLocks（文件描述符不会被终结器关闭），
// exited 关闭（浏览器进程确认退出）后再释放；exited 为 nil 时一直保留到服务进程退出。同一把锁重复保留只登记一次
func retainAccountCacheUntilExit(lock *flock.Flock, exited <-chan struct{}) {
	if lock == nil {
		return
	}
	unconfirmedCacheMu.Lock()
	if _, retained := unconfirmedCacheLocks[lock]; retained {
		unconfirmedCacheMu.Unlock()
		return
	}
	unconfirmedCacheLocks[lock] = struct{}{}
	unconfirmedCacheMu.Unlock()
	if exited == nil {
		return
	}
	go func() {
		<-exited
		unconfirmedCacheMu.Lock()
		delete(unconfirmedCacheLocks, lock)
		unconfirmedCacheMu.Unlock()
		// 进程已经退出，释放失败（文件描述符已无效）也不再有浏览器用这个目录
		_ = releaseAccountCache(lock)
	}()
}

// releaseAccountCache 在浏览器进程退出后释放账户 HTTP 磁盘缓存目录
func releaseAccountCache(lock *flock.Flock) error {
	if lock == nil {
		return nil
	}
	if err := lock.Unlock(); err != nil {
		return fmt.Errorf("释放账户浏览器缓存: %w", err)
	}
	return nil
}

// cachePreferences 返回把 HTTP 磁盘缓存放到指定目录的 Firefox 偏好
func cachePreferences(directory string) map[string]any {
	return map[string]any{
		"browser.cache.disk.parent_directory":   directory,
		"browser.cache.disk.smart_size.enabled": false,
		"browser.cache.disk.capacity":           accountCacheCapacityKB,
	}
}

// expireAccountCache 在持有缓存锁时按创建时间标记处理过期：标记缺失、无法解析或晚于当前时间（时钟回拨）时补写
// （见 accountCacheUnknownCreated）；超过 AccountCacheMaxAge 时清空缓存。返回是否清空（含清空失败）过期缓存、清空前的字节数与清空失败的原因。
// 清空失败（校验没有通过，或只删了一部分）时同样把标记改为当前时间：原先标记不变，之后每次启动都重试、都写一行警告，
// 只删了一部分的还每次改用临时缓存整页冷加载；现在 3 天后才再试。补写或改写标记失败不影响启动，下次锁定时再写
func expireAccountCache(storageStatePath string, directory string, lock *flock.Flock, now time.Time) (bool, int64, error) {
	created, ok := readAccountCacheCreated(directory)
	if !ok || created.After(now) {
		_ = writeAccountCacheCreated(directory, accountCacheUnknownCreated(directory, now))
		return false, 0, nil
	}
	if now.Sub(created) < AccountCacheMaxAge {
		return false, 0, nil
	}
	size, err := clearAccountCache(storageStatePath, directory, lock, now)
	if err != nil {
		_ = writeAccountCacheCreated(directory, now)
	}
	return true, size, err
}

// accountCacheUnknownCreated 返回标记缺失、无法解析或晚于当前时间时补写的创建时间：目录里除锁文件与标记外没有内容（刚新建，或刚被
// start.sh clean-cache 清空）时为当前时间；已有内容时这份缓存的年龄未知（多半是加入标记之前的版本留下的，正是可能与新页面不配套的旧缓存），
// 写回拨 AccountCacheClearMinAge 的时间。原先一律写当前时间：启动锁定在浏览器启动之前，这次启动以页面状态类错误失败时，
// 启动失败后的清理读到的是几分钟前刚补写的标记，不到 12 小时而跳过，部署前留下的旧缓存部署后 12 小时内都清不掉。
// 回拨后第一次页面状态类失败就能清理，清理后照常有 12 小时的保护，3 天过期从补写时提前 12 小时算起。读不了目录时为当前时间
func accountCacheUnknownCreated(directory string, now time.Time) time.Time {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return now
	}
	for _, entry := range entries {
		if name := entry.Name(); name != accountCacheLockName && name != accountCacheCreatedName {
			return now.Add(-AccountCacheClearMinAge)
		}
	}
	return now
}

// ClearAccountCache 清空账户 HTTP 磁盘缓存，供启动失败后的自愈清理，返回是否清理与清理前的字节数。
// 缓存目录不存在时不做任何事；清理前先取得缓存锁，取不到（同账户另一个 runtime 正在用，或之前的浏览器没能确认退出、锁仍被持有，
// 见 retainAccountCacheUntilExit）时不清理。使用账户缓存的浏览器运行期间都持有这把锁，拿到锁就说明没有浏览器在用这个目录。
// 只应在失败的这次启动确实用了账户缓存时调用（见 Options.CacheAttached）。拿到锁之后，创建时间标记距今不到 AccountCacheClearMinAge 的缓存不清理：
// 正常流程里启动锁定时已补写标记，部署前留下的、没有标记的旧缓存补写的是回拨 12 小时的时间（见 accountCacheUnknownCreated），
// 第一次页面状态类失败就能清理。标记缺失、无法解析或晚于当前时间（启动时没写成）时年龄未知，照常清理
func ClearAccountCache(storageStatePath string) (bool, int64, error) {
	directory := accountCacheDirectory(storageStatePath)
	if err := checkAccountCacheDirectory(storageStatePath, directory); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, 0, nil
		}
		return false, 0, err
	}
	lock := flock.New(filepath.Join(directory, accountCacheLockName))
	locked, err := lock.TryLock()
	if err != nil {
		return false, 0, fmt.Errorf("锁定账户浏览器缓存: %w", err)
	}
	if !locked {
		return false, 0, nil
	}
	now := time.Now()
	if created, ok := readAccountCacheCreated(directory); ok && !created.After(now) && now.Sub(created) < AccountCacheClearMinAge {
		return false, 0, releaseAccountCache(lock)
	}
	size, clearErr := clearAccountCache(storageStatePath, directory, lock, now)
	return clearErr == nil, size, errors.Join(clearErr, releaseAccountCache(lock))
}

// clearAccountCache 在持有缓存锁时清空缓存目录：删除锁文件以外的全部内容，再写入新的创建时间标记，返回清空前的字节数。
// 锁文件保留：它被持锁方打开着（Windows 上删不掉），删掉后同账户另一个 runtime 会在新建的同名文件上同时拿到锁；
// 除锁文件外清空的目录与删除后重建等价。删除前校验目录（见 checkAccountCacheDirectory）并确认持有的正是这个目录的锁，
// 没有通过时什么都不删，返回 accountCacheRefusedError；逐项删除的目标都直接位于该目录下
func clearAccountCache(storageStatePath string, directory string, lock *flock.Flock, now time.Time) (int64, error) {
	if err := checkAccountCacheDirectory(storageStatePath, directory); err != nil {
		return 0, &accountCacheRefusedError{err: err}
	}
	directory = filepath.Clean(directory)
	if lock == nil || !lock.Locked() || filepath.Clean(lock.Path()) != filepath.Join(directory, accountCacheLockName) {
		return 0, &accountCacheRefusedError{err: fmt.Errorf("未持有账户浏览器缓存锁，拒绝清理 %s", directory)}
	}
	size := accountCacheSize(directory)
	entries, err := os.ReadDir(directory)
	if err != nil {
		return size, fmt.Errorf("读取账户浏览器缓存目录: %w", err)
	}
	var removeErrors []error
	for _, entry := range entries {
		name := entry.Name()
		if name == accountCacheLockName {
			continue
		}
		target := filepath.Join(directory, name)
		if name == "" || name == "." || name == ".." || filepath.Dir(target) != directory {
			removeErrors = append(removeErrors, fmt.Errorf("缓存目录项名称无效: %q", name))
			continue
		}
		if err := removeAccountCacheEntry(target); err != nil {
			removeErrors = append(removeErrors, err)
		}
	}
	if err := errors.Join(removeErrors...); err != nil {
		return size, fmt.Errorf("清空账户浏览器缓存: %w", err)
	}
	// 写不了标记时下次锁定再补写，缓存已经清空
	_ = writeAccountCacheCreated(directory, now)
	return size, nil
}

// checkAccountCacheDirectory 校验要清理的目录：必须恰好是认证文件所在账户目录下名为 camoufox-cache 的目录（filepath.Clean 后比较），
// 账户目录不能是当前目录或根目录，认证文件必须是普通文件，缓存目录必须是真实目录而不是符号链接。
// 认证文件或缓存目录不存在时返回的错误满足 errors.Is(err, fs.ErrNotExist)
func checkAccountCacheDirectory(storageStatePath string, directory string) error {
	if strings.TrimSpace(storageStatePath) == "" || strings.TrimSpace(directory) == "" {
		return errors.New("缺少账户浏览器缓存路径，拒绝清理")
	}
	storageStatePath = filepath.Clean(storageStatePath)
	accountDirectory := filepath.Dir(storageStatePath)
	if accountDirectory == "." || filepath.Dir(accountDirectory) == accountDirectory {
		return fmt.Errorf("账户目录 %s 无效，拒绝清理浏览器缓存", accountDirectory)
	}
	expected := filepath.Join(accountDirectory, accountCacheDirectoryName)
	if filepath.Clean(directory) != expected || filepath.Base(expected) != accountCacheDirectoryName {
		return fmt.Errorf("%s 不是账户目录下的 %s，拒绝清理", directory, accountCacheDirectoryName)
	}
	stateInfo, err := os.Lstat(storageStatePath)
	if err != nil {
		return fmt.Errorf("检查账户认证文件: %w", err)
	}
	if !stateInfo.Mode().IsRegular() {
		return fmt.Errorf("账户认证文件 %s 不是普通文件，拒绝清理浏览器缓存", storageStatePath)
	}
	info, err := os.Lstat(expected)
	if err != nil {
		return fmt.Errorf("检查账户浏览器缓存目录: %w", err)
	}
	if info.Mode()&fs.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("%s 不是目录（或是符号链接），拒绝清理", expected)
	}
	return nil
}

// removeAccountCacheEntry 删除缓存目录下的一项；测试替换它模拟只删了一部分（以 root 运行时改权限删不掉文件）
var removeAccountCacheEntry = os.RemoveAll

// accountCacheSize 返回缓存目录内普通文件的总字节数（读不到的项不计）
func accountCacheSize(directory string) int64 {
	var total int64
	_ = filepath.WalkDir(directory, func(_ string, entry fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if entry.Type().IsRegular() {
			if info, infoErr := entry.Info(); infoErr == nil {
				total += info.Size()
			}
		}
		return nil
	})
	return total
}

// readAccountCacheCreated 读取缓存创建时间标记，缺失或无法解析时返回 false
func readAccountCacheCreated(directory string) (time.Time, bool) {
	data, err := os.ReadFile(filepath.Join(directory, accountCacheCreatedName))
	if err != nil {
		return time.Time{}, false
	}
	created, err := time.Parse(time.RFC3339, strings.TrimSpace(string(data)))
	if err != nil {
		return time.Time{}, false
	}
	return created, true
}

// writeAccountCacheCreated 写入缓存创建时间标记
func writeAccountCacheCreated(directory string, now time.Time) error {
	return os.WriteFile(filepath.Join(directory, accountCacheCreatedName), []byte(now.UTC().Format(time.RFC3339)+"\n"), 0o600)
}

// pageStateFailurePrefixes 为页面状态类启动失败的错误文案前缀（见 Worker.bootstrap）：官网页面没有长出预期的输入框、Run 按钮、
// WAA service 或 bootstrap 请求。账户 HTTP 缓存里与新页面不配套的旧资源是常见原因，清空缓存后重新载入可以恢复
var pageStateFailurePrefixes = []string{
	pageInputNotReadyPrefix,
	"填写 bootstrap 提示词失败",
	"等待官网 Run 按钮启用",
	"提交官网提示词",
	"官网 WAA service 未暴露",
	"官网 GenerateContent 拦截事件超时",
	"官网高层 snapshot 函数定位超时",
}

// pageInputNotReadyPrefix 为等待输入框超时的文案前缀，完整文案为 "AI Studio 输入框未就绪 url=<当前页面>: <原因>"
const pageInputNotReadyPrefix = "AI Studio 输入框未就绪"

// PageStateFailure 判断 Start 的错误是否属于页面状态类，返回匹配的文案前缀。错误链上任意一层的文案以前缀开头即算：
// 上层可能再包装（本机代理中转的说明追加在原文之后，errors.Join 也保留原文）。等待输入框超时只在页面确实停在 AI Studio
// （url=https://aistudio.google.com/…）时算：停在代理错误页（about:neterror）、Google sorry 页、登录页、地区不支持页，
// 或读不到 url 时与缓存无关，清掉只会让下次启动多一次整页冷加载
func PageStateFailure(err error) (string, bool) {
	pending := []error{err}
	for len(pending) > 0 {
		current := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if current == nil {
			continue
		}
		message := current.Error()
		for _, prefix := range pageStateFailurePrefixes {
			if !strings.HasPrefix(message, prefix) {
				continue
			}
			if prefix == pageInputNotReadyPrefix && !strings.HasPrefix(message, prefix+" url="+aiStudioOrigin+"/") {
				break
			}
			return prefix, true
		}
		switch wrapped := current.(type) {
		case interface{ Unwrap() []error }:
			pending = append(pending, wrapped.Unwrap()...)
		case interface{ Unwrap() error }:
			pending = append(pending, wrapped.Unwrap())
		}
	}
	return "", false
}
