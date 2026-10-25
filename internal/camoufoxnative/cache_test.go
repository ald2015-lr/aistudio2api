package camoufoxnative

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gofrs/flock"
)

// testAccountDirectory 创建带认证文件的账户目录，返回认证文件路径
func testAccountDirectory(t *testing.T) string {
	t.Helper()
	directory := filepath.Join(t.TempDir(), "account@example.com")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "storage-state.json")
	if err := os.WriteFile(path, []byte(`{"cookies":[],"origins":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// fillAccountCache 在缓存目录里写入模拟的 Firefox 缓存文件，返回写入的字节数
func fillAccountCache(t *testing.T, directory string) int64 {
	t.Helper()
	entries := filepath.Join(directory, "cache2", "entries")
	if err := os.MkdirAll(entries, 0o700); err != nil {
		t.Fatal(err)
	}
	var total int64
	for index := range 3 {
		data := []byte(strings.Repeat("x", 1000*(index+1)))
		if err := os.WriteFile(filepath.Join(entries, fmt.Sprintf("entry-%d", index)), data, 0o600); err != nil {
			t.Fatal(err)
		}
		total += int64(len(data))
	}
	if err := os.WriteFile(filepath.Join(directory, "cache2", "index"), []byte("index"), 0o600); err != nil {
		t.Fatal(err)
	}
	return total + int64(len("index"))
}

// cacheEntries 返回缓存目录第一层的名称
func cacheEntries(t *testing.T, directory string) []string {
	t.Helper()
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

func writeCreated(t *testing.T, directory string, created time.Time) {
	t.Helper()
	if err := writeAccountCacheCreated(directory, created); err != nil {
		t.Fatal(err)
	}
}

// TestLockAccountCacheCreatedMarker 锁定缓存时补写缺失的创建时间标记；未过期的缓存原样使用，不调用过期回调
func TestLockAccountCacheCreatedMarker(t *testing.T) {
	storagePath := testAccountDirectory(t)
	called := 0
	directory, lock, err := lockAccountCache(storagePath, func(int64, error) { called++ })
	if err != nil {
		t.Fatal(err)
	}
	if directory != filepath.Join(filepath.Dir(storagePath), accountCacheDirectoryName) || lock == nil {
		t.Fatalf("缓存目录 = %q lock=%v", directory, lock)
	}
	created, ok := readAccountCacheCreated(directory)
	if !ok || time.Since(created) > time.Minute {
		t.Fatalf("标记缺失时应补写当前时间，实际 %v %t", created, ok)
	}
	if err := releaseAccountCache(lock); err != nil {
		t.Fatal(err)
	}

	// 两天前创建的缓存未过期：内容与标记都保留
	size := fillAccountCache(t, directory)
	twoDays := time.Now().Add(-48 * time.Hour).Truncate(time.Second)
	writeCreated(t, directory, twoDays)
	_, lock, err = lockAccountCache(storagePath, func(int64, error) { called++ })
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = releaseAccountCache(lock) }()
	if called != 0 {
		t.Fatalf("未过期时不应调用过期回调，调用 %d 次", called)
	}
	if got := accountCacheSize(directory); got < size {
		t.Fatalf("未过期的缓存不应清空，剩余 %d 字节", got)
	}
	if created, _ := readAccountCacheCreated(directory); !created.Equal(twoDays) {
		t.Fatalf("未过期时标记应保持 %v，实际 %v", twoDays, created)
	}
}

// TestLockAccountCacheClearsExpired 超过 3 天的缓存在拿到锁之后先清空再用：锁文件保留（锁仍有效），标记改为当前时间，回调拿到清空前的大小；
// 标记晚于当前时间（时钟回拨）时按缺失处理：不清空，已有内容时补写回拨 12 小时的时间（见 TestLegacyCacheHealableAfterLock）
func TestLockAccountCacheClearsExpired(t *testing.T) {
	storagePath := testAccountDirectory(t)
	directory := accountCacheDirectory(storagePath)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	size := fillAccountCache(t, directory)
	writeCreated(t, directory, time.Now().Add(-AccountCacheMaxAge-time.Hour))
	var reported []int64
	got, lock, err := lockAccountCache(storagePath, func(size int64, err error) {
		if err != nil {
			t.Errorf("清空不应失败: %v", err)
		}
		reported = append(reported, size)
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = releaseAccountCache(lock) }()
	if got != directory || lock == nil || !lock.Locked() {
		t.Fatalf("清空后应继续持锁使用缓存目录，实际 %q %v", got, lock)
	}
	if len(reported) != 1 || reported[0] < size {
		t.Fatalf("过期回调 = %v，期望一次且大小不小于 %d", reported, size)
	}
	if names := cacheEntries(t, directory); strings.Join(names, ",") != ".created,.lock" {
		t.Fatalf("清空后目录 = %v，期望只剩 .created 与 .lock", names)
	}
	if created, ok := readAccountCacheCreated(directory); !ok || time.Since(created) > time.Minute {
		t.Fatalf("清空后标记应为当前时间，实际 %v", created)
	}
	other := flock.New(filepath.Join(directory, accountCacheLockName))
	if locked, err := other.TryLock(); err != nil || locked {
		_ = other.Unlock()
		t.Fatalf("清空后锁仍应有效，另一个持有者拿到锁=%t err=%v", locked, err)
	}

	future := time.Now().Add(48 * time.Hour)
	writeCreated(t, directory, future)
	fillAccountCache(t, directory)
	if _, _, err := expireAccountCache(storagePath, directory, lock, time.Now()); err != nil {
		t.Fatal(err)
	}
	if created, _ := readAccountCacheCreated(directory); created.After(time.Now().Add(-AccountCacheClearMinAge)) {
		t.Fatalf("已有内容时未来的标记应改为回拨 12 小时的时间，实际 %v", created)
	}
	if accountCacheSize(directory) < size {
		t.Fatal("时钟回拨时不应清空缓存")
	}
}

// TestClearAccountCacheLocking 启动失败后的清理先取缓存锁：目录不存在时什么都不做（也不创建）；
// 锁被别的 runtime 持有时不清理；拿到锁后清空除锁文件外的内容并释放锁
func TestClearAccountCacheLocking(t *testing.T) {
	storagePath := testAccountDirectory(t)
	directory := accountCacheDirectory(storagePath)
	cleared, size, err := ClearAccountCache(storagePath)
	if err != nil || cleared || size != 0 {
		t.Fatalf("目录不存在时 = %t %d %v，期望什么都不做", cleared, size, err)
	}
	if _, err := os.Stat(directory); !os.IsNotExist(err) {
		t.Fatalf("目录不存在时不应创建: %v", err)
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	written := fillAccountCache(t, directory)
	holder := flock.New(filepath.Join(directory, accountCacheLockName))
	if locked, err := holder.TryLock(); err != nil || !locked {
		t.Fatalf("持有锁失败: %t %v", locked, err)
	}
	cleared, _, err = ClearAccountCache(storagePath)
	if err != nil || cleared {
		t.Fatalf("锁被持有时 = %t %v，期望不清理", cleared, err)
	}
	if accountCacheSize(directory) < written {
		t.Fatal("锁被持有时缓存内容不应被删除")
	}
	if err := holder.Unlock(); err != nil {
		t.Fatal(err)
	}
	cleared, size, err = ClearAccountCache(storagePath)
	if err != nil || !cleared || size < written {
		t.Fatalf("拿到锁后 = %t %d %v，期望清理且大小不小于 %d", cleared, size, err, written)
	}
	if names := cacheEntries(t, directory); strings.Join(names, ",") != ".created,.lock" {
		t.Fatalf("清理后目录 = %v", names)
	}
	again := flock.New(filepath.Join(directory, accountCacheLockName))
	if locked, err := again.TryLock(); err != nil || !locked {
		t.Fatalf("清理后应释放锁: %t %v", locked, err)
	}
	_ = again.Unlock()
}

// TestCheckAccountCacheDirectory 删除前校验路径：只接受认证文件所在账户目录下名为 camoufox-cache 的真实目录
func TestCheckAccountCacheDirectory(t *testing.T) {
	storagePath := testAccountDirectory(t)
	accountDirectory := filepath.Dir(storagePath)
	directory := accountCacheDirectory(storagePath)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := checkAccountCacheDirectory(storagePath, directory+string(filepath.Separator)); err != nil {
		t.Fatalf("Clean 后相同的路径应通过: %v", err)
	}
	otherAccount := filepath.Join(filepath.Dir(accountDirectory), "other@example.com", accountCacheDirectoryName)
	for name, candidate := range map[string]string{
		"其他名称":   filepath.Join(accountDirectory, "camoufox-cache2"),
		"账户目录本身": accountDirectory,
		"其他账户":   otherAccount,
		"上一级":    filepath.Join(accountDirectory, accountCacheDirectoryName, ".."),
		"空路径":    "",
	} {
		if err := checkAccountCacheDirectory(storagePath, candidate); err == nil {
			t.Fatalf("%s（%s）应被拒绝", name, candidate)
		}
	}
	if err := checkAccountCacheDirectory("storage-state.json", accountCacheDirectoryName); err == nil {
		t.Fatal("账户目录为当前目录时应被拒绝")
	}
	root := string(filepath.Separator) + "storage-state.json"
	if err := checkAccountCacheDirectory(root, accountCacheDirectory(root)); err == nil {
		t.Fatal("账户目录为根目录时应被拒绝")
	}
	if err := checkAccountCacheDirectory(filepath.Join(accountDirectory, "missing.json"), directory); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("认证文件不存在时应返回不存在: %v", err)
	}
	var lock *flock.Flock
	if _, err := clearAccountCache(storagePath, directory, lock, time.Now()); err == nil {
		t.Fatal("没有持锁时应拒绝清理")
	}
	foreign := flock.New(filepath.Join(t.TempDir(), accountCacheLockName))
	if locked, err := foreign.TryLock(); err != nil || !locked {
		t.Fatalf("持有锁失败: %t %v", locked, err)
	}
	defer func() { _ = foreign.Unlock() }()
	fillAccountCache(t, directory)
	if _, err := clearAccountCache(storagePath, directory, foreign, time.Now()); err == nil {
		t.Fatal("持有的不是这个目录的锁时应拒绝清理")
	}
	if accountCacheSize(directory) == 0 {
		t.Fatal("拒绝清理时不应删除任何内容")
	}
}

// TestClearAccountCacheRefusesSymlink 缓存目录是符号链接时拒绝清理，链接指向的目录内容不受影响
func TestClearAccountCacheRefusesSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 上创建符号链接需要额外权限")
	}
	storagePath := testAccountDirectory(t)
	target := t.TempDir()
	written := fillAccountCache(t, target)
	if err := os.Symlink(target, accountCacheDirectory(storagePath)); err != nil {
		t.Fatal(err)
	}
	if cleared, _, err := ClearAccountCache(storagePath); err == nil || cleared {
		t.Fatalf("符号链接 = %t %v，期望拒绝", cleared, err)
	}
	if accountCacheSize(target) < written {
		t.Fatal("链接指向的目录内容不应被删除")
	}
}

// TestPageStateFailure 页面状态类启动失败按错误文案前缀判断，包装后的错误（本机代理中转说明、errors.Join、%w）同样识别；
// 登录跳转、连接 BiDi 等其他失败不算，等待输入框超时时页面不在 AI Studio 的也不算
func TestPageStateFailure(t *testing.T) {
	base := errors.New("等待页面条件超时")
	matching := map[string]error{
		"AI Studio 输入框未就绪":          fmt.Errorf("AI Studio 输入框未就绪 url=%s: %w", "https://aistudio.google.com/prompts/new_chat", base),
		"填写 bootstrap 提示词失败":        fmt.Errorf("填写 bootstrap 提示词失败 value=%q err=%v", "", errors.New("提示词输入框不存在")),
		"等待官网 Run 按钮启用":             fmt.Errorf("等待官网 Run 按钮启用: %w", base),
		"提交官网提示词":                   fmt.Errorf("提交官网提示词: %w", errors.New("官网 Run 按钮不存在")),
		"官网 WAA service 未暴露":        fmt.Errorf("官网 WAA service 未暴露: %w", base),
		"官网 GenerateContent 拦截事件超时": errors.New("官网 GenerateContent 拦截事件超时"),
		"官网高层 snapshot 函数定位超时":      errors.New("官网高层 snapshot 函数定位超时"),
	}
	for want, err := range matching {
		for name, wrapped := range map[string]error{
			"原文":   err,
			"中转说明": fmt.Errorf("%w；本机代理中转：连接上游超时", err),
			"Join": errors.Join(err, errors.New("释放账户浏览器缓存失败")),
			"外层":   fmt.Errorf("WAA Worker 初始化: %w", err),
		} {
			if got, ok := PageStateFailure(wrapped); !ok || got != want {
				t.Fatalf("%s/%s: PageStateFailure = %q %t，期望 %q", want, name, got, ok, want)
			}
		}
	}
	for _, err := range []error{
		nil,
		errors.New("隔离登录态失效 url=https://accounts.google.com/v3/signin"),
		fmt.Errorf("连接 Camoufox BiDi: %w", base),
		errors.New("等待 Camoufox BiDi 端点超时: 45s"),
		errors.New("官网初始化页面模型不匹配 expected=a actual=b"),
		errors.New("同步官网 prompt: 提示词输入框不存在"),
		// 等待输入框超时时页面不在 AI Studio：代理错误页、Google sorry 页、登录页、读不到 url，与缓存无关
		fmt.Errorf("AI Studio 输入框未就绪 url=%s: %w", "about:neterror?e=proxyConnectFailure&u=https%3A//aistudio.google.com/", base),
		fmt.Errorf("AI Studio 输入框未就绪 url=%s: %w", "https://www.google.com/sorry/index?continue=https://aistudio.google.com/", base),
		fmt.Errorf("AI Studio 输入框未就绪 url=%s: %w", "https://accounts.google.com/v3/signin/identifier", base),
		fmt.Errorf("AI Studio 输入框未就绪 url=%s: %w", "about:blank", base),
		fmt.Errorf("AI Studio 输入框未就绪 url=%s: %w", "", base),
		fmt.Errorf("WAA Worker 初始化: %w", fmt.Errorf("AI Studio 输入框未就绪 url=%s: %w", "https://aistudio.google.comevil.example/", base)),
	} {
		if got, ok := PageStateFailure(err); ok {
			t.Fatalf("%v 不应算页面状态类，匹配 %q", err, got)
		}
	}
}

// TestPageStateFailureSkipsOnboarding 首次使用页上的失败不算页面状态类（清缓存没用）：没能完成首次使用页，
// 以及订阅之后的页面类失败发生时页面停在首次使用页（原错误前缀照旧是页面状态类的），包装后同样识别
func TestPageStateFailureSkipsOnboarding(t *testing.T) {
	base := errors.New("等待页面条件超时")
	failure := &onboardingFailure{reason: "找不到可点的继续按钮", snapshot: " | 页面={}"}
	original := fmt.Errorf("等待官网 Run 按钮启用: %w", base)
	interrupted := &onboardingInterrupted{err: original, text: original.Error() + "；已完成首次使用页"}
	if !strings.HasPrefix(failure.Error(), OnboardingFailedPrefix+": ") {
		t.Fatalf("文案前缀不对: %s", failure.Error())
	}
	for name, err := range map[string]error{
		"未能完成":    failure,
		"已就地完成":   interrupted,
		"中转说明":    fmt.Errorf("%w；本机代理中转：连接上游超时", interrupted),
		"Join":    errors.Join(failure, errors.New("释放账户浏览器缓存失败")),
		"外层":      fmt.Errorf("WAA Worker 初始化: %w", interrupted),
		"包着页面类原文": fmt.Errorf("%w", &onboardingFailure{reason: "x", cause: fmt.Errorf("官网 WAA service 未暴露: %w", base)}),
	} {
		if got, ok := PageStateFailure(err); ok {
			t.Fatalf("%s: 首次使用页上的失败不应算页面状态类，匹配 %q", name, got)
		}
	}
	if !errors.Is(interrupted, base) {
		t.Fatal("就地完成后错误链应保留原错误")
	}
}

// TestPageStateFailurePrefixesMatchBootstrap 前缀表里的每一项都要在启动代码的错误文案里出现：bootstrap 改了文案时提醒同步修改
func TestPageStateFailurePrefixesMatchBootstrap(t *testing.T) {
	var source strings.Builder
	for _, name := range []string{"worker.go", "bidi.go"} {
		data, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		source.Write(data)
	}
	for _, prefix := range pageStateFailurePrefixes {
		if !strings.Contains(source.String(), `"`+prefix) {
			t.Fatalf("前缀 %q 没有出现在 worker.go/bidi.go 的错误文案开头", prefix)
		}
	}
}

// TestClearAccountCacheMinAge 启动失败后的清理不清最近新建或清空过的缓存（创建时间标记距今不到 12 小时），标记更早时照常清理；
// 标记缺失、无法解析或晚于当前时间时年龄未知，照常清理。不清理时同样释放锁
func TestClearAccountCacheMinAge(t *testing.T) {
	storagePath := testAccountDirectory(t)
	directory := accountCacheDirectory(storagePath)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	written := fillAccountCache(t, directory)
	recent := time.Now().Add(-AccountCacheClearMinAge + time.Hour).Truncate(time.Second)
	writeCreated(t, directory, recent)
	cleared, size, err := ClearAccountCache(storagePath)
	if err != nil || cleared || size != 0 {
		t.Fatalf("最近新建的缓存 = %t %d %v，期望不清理", cleared, size, err)
	}
	if accountCacheSize(directory) < written {
		t.Fatal("最近新建的缓存内容不应被删除")
	}
	if created, _ := readAccountCacheCreated(directory); !created.Equal(recent) {
		t.Fatalf("不清理时标记应保持 %v，实际 %v", recent, created)
	}
	probe := flock.New(filepath.Join(directory, accountCacheLockName))
	if locked, err := probe.TryLock(); err != nil || !locked {
		t.Fatalf("不清理时也应释放锁: %t %v", locked, err)
	}
	_ = probe.Unlock()

	for name, prepare := range map[string]func(){
		"超过下限": func() { writeCreated(t, directory, time.Now().Add(-AccountCacheClearMinAge-time.Minute)) },
		"标记缺失": func() { _ = os.Remove(filepath.Join(directory, accountCacheCreatedName)) },
		"无法解析": func() {
			if err := os.WriteFile(filepath.Join(directory, accountCacheCreatedName), []byte("garbage"), 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"晚于当前时间": func() { writeCreated(t, directory, time.Now().Add(time.Hour)) },
	} {
		fillAccountCache(t, directory)
		prepare()
		cleared, size, err := ClearAccountCache(storagePath)
		if err != nil || !cleared || size < written {
			t.Fatalf("%s：= %t %d %v，期望清理", name, cleared, size, err)
		}
		if created, ok := readAccountCacheCreated(directory); !ok || time.Since(created) > time.Minute {
			t.Fatalf("%s：清理后标记应为当前时间，实际 %v %t", name, created, ok)
		}
	}
	// 刚清空过：标记是当前时间，再次失败时不清理
	fillAccountCache(t, directory)
	if cleared, _, err := ClearAccountCache(storagePath); err != nil || cleared {
		t.Fatalf("刚清空过的缓存 = %t %v，期望不清理", cleared, err)
	}
}

// TestLegacyCacheHealableAfterLock 没有 .created 的旧缓存（加入标记之前的版本留下的）按真实顺序：启动锁定补写标记、启动失败释放锁、
// 启动失败后的清理，必须能清掉（原先锁定时补写当前时间，清理读到不到 12 小时的标记而跳过）；补写的是回拨 12 小时的时间。
// 刚新建的空目录补写当前时间，浏览器写入内容后同样的流程不清理。标记无法解析或晚于当前时间时按同样规则处理
func TestLegacyCacheHealableAfterLock(t *testing.T) {
	for name, prepare := range map[string]func(t *testing.T, directory string){
		"标记缺失": func(*testing.T, string) {},
		"无法解析": func(t *testing.T, directory string) {
			if err := os.WriteFile(filepath.Join(directory, accountCacheCreatedName), []byte("garbage"), 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"晚于当前时间": func(t *testing.T, directory string) { writeCreated(t, directory, time.Now().Add(48*time.Hour)) },
	} {
		t.Run(name, func(t *testing.T) {
			storagePath := testAccountDirectory(t)
			directory := accountCacheDirectory(storagePath)
			if err := os.MkdirAll(directory, 0o700); err != nil {
				t.Fatal(err)
			}
			written := fillAccountCache(t, directory)
			prepare(t, directory)
			before := time.Now()
			got, lock, err := lockAccountCache(storagePath, func(int64, error) { t.Error("年龄未知的缓存不应按过期清空") })
			if err != nil || got != directory || lock == nil {
				t.Fatalf("锁定 = %q %v %v", got, lock, err)
			}
			created, ok := readAccountCacheCreated(directory)
			want := before.Add(-AccountCacheClearMinAge).Truncate(time.Second)
			if !ok || created.Before(want.Add(-time.Second)) || created.After(time.Now().Add(-AccountCacheClearMinAge)) {
				t.Fatalf("已有内容时应补写回拨 12 小时的标记，实际 %v %t（期望约 %v）", created, ok, want)
			}
			if accountCacheSize(directory) < written {
				t.Fatal("补写标记时不应删除内容")
			}
			if err := releaseAccountCache(lock); err != nil {
				t.Fatal(err)
			}
			cleared, size, err := ClearAccountCache(storagePath)
			if err != nil || !cleared || size < written {
				t.Fatalf("旧缓存第一次启动失败后 = %t %d %v，期望清理", cleared, size, err)
			}
			// 清理后照常有 12 小时的保护
			fillAccountCache(t, directory)
			if cleared, _, err := ClearAccountCache(storagePath); err != nil || cleared {
				t.Fatalf("刚清理过的缓存 = %t %v，期望不清理", cleared, err)
			}
		})
	}

	t.Run("新建的空目录", func(t *testing.T) {
		storagePath := testAccountDirectory(t)
		directory, lock, err := lockAccountCache(storagePath, nil)
		if err != nil || lock == nil {
			t.Fatalf("锁定: %v", err)
		}
		if created, ok := readAccountCacheCreated(directory); !ok || time.Since(created) > time.Minute {
			t.Fatalf("新建的空目录应补写当前时间，实际 %v %t", created, ok)
		}
		// 浏览器写入了缓存，随后这次启动失败
		written := fillAccountCache(t, directory)
		if err := releaseAccountCache(lock); err != nil {
			t.Fatal(err)
		}
		if cleared, _, err := ClearAccountCache(storagePath); err != nil || cleared {
			t.Fatalf("刚新建的缓存 = %t %v，期望不清理", cleared, err)
		}
		if accountCacheSize(directory) < written {
			t.Fatal("刚新建的缓存内容不应被删除")
		}
	})
}

// TestStartReportsCacheAttached 真实 Start 流程（Camoufox 不存在，启动浏览器时失败）：拿到账户缓存锁时调用 CacheAttached，
// 没有标记的旧缓存在这之后能被启动失败后的清理清掉；锁被同账户另一个 runtime 持有时不调用（用的是临时 profile 内的缓存）
func TestStartReportsCacheAttached(t *testing.T) {
	storagePath := testAccountDirectory(t)
	state := `{"cookies":[{"name":"SID","value":"test","domain":".google.com","path":"/"}],"origins":[]}`
	if err := os.WriteFile(storagePath, []byte(state), 0o600); err != nil {
		t.Fatal(err)
	}
	directory := accountCacheDirectory(storagePath)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	written := fillAccountCache(t, directory)
	attached := 0
	options := Options{
		ExecutablePath:   filepath.Join(t.TempDir(), "missing-camoufox"),
		StorageStatePath: storagePath,
		Model:            "models/test",
		BootstrapPrompt:  "test",
		Headless:         true,
		CacheAttached:    func() { attached++ },
	}
	if _, err := Start(context.Background(), options); err == nil || !strings.Contains(err.Error(), "Camoufox 不可用") {
		t.Fatalf("启动应在启动浏览器时失败，实际 %v", err)
	}
	if attached != 1 {
		t.Fatalf("拿到账户缓存锁时应调用一次 CacheAttached，实际 %d", attached)
	}
	if cleared, size, err := ClearAccountCache(storagePath); err != nil || !cleared || size < written {
		t.Fatalf("启动失败后清理没有标记的旧缓存 = %t %d %v，期望清理", cleared, size, err)
	}

	fillAccountCache(t, directory)
	holder := flock.New(filepath.Join(directory, accountCacheLockName))
	if locked, err := holder.TryLock(); err != nil || !locked {
		t.Fatalf("持有锁失败: %t %v", locked, err)
	}
	defer func() { _ = holder.Unlock() }()
	attached = 0
	if _, err := Start(context.Background(), options); err == nil {
		t.Fatal("启动应失败")
	}
	if attached != 0 {
		t.Fatalf("锁被别的 runtime 持有时不应调用 CacheAttached，实际 %d", attached)
	}
}

// TestExpiredCacheRefusedKeepsUsing 过期清空时删除前的校验没有通过（camoufox-cache 是符号链接）：什么都不删，照常把目录交给浏览器
// （与加入过期之前一致），写一次警告并把标记改为当前时间；下一次启动不再重试、不再写警告。原先每次启动都改用临时缓存整页冷加载、都写警告
func TestExpiredCacheRefusedKeepsUsing(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 上创建符号链接需要额外权限")
	}
	storagePath := testAccountDirectory(t)
	target := t.TempDir()
	written := fillAccountCache(t, target)
	directory := accountCacheDirectory(storagePath)
	if err := os.Symlink(target, directory); err != nil {
		t.Fatal(err)
	}
	writeCreated(t, target, time.Now().Add(-AccountCacheMaxAge-time.Hour))
	var reported []error
	expired := func(_ int64, err error) { reported = append(reported, err) }
	for attempt := range 2 {
		got, lock, err := lockAccountCache(storagePath, expired)
		if err != nil || got != directory || lock == nil || !lock.Locked() {
			t.Fatalf("第 %d 次锁定 = %q %v %v，期望照常使用缓存目录", attempt+1, got, lock, err)
		}
		if err := releaseAccountCache(lock); err != nil {
			t.Fatal(err)
		}
	}
	if len(reported) != 1 || reported[0] == nil || !strings.Contains(reported[0].Error(), "拒绝清理") {
		t.Fatalf("过期回调 = %v，期望只有第一次报告一次拒绝清理", reported)
	}
	if accountCacheSize(target) < written {
		t.Fatal("链接指向的目录内容不应被删除")
	}
	if created, ok := readAccountCacheCreated(target); !ok || time.Since(created) > time.Minute {
		t.Fatalf("拒绝清理后标记应改为当前时间，实际 %v %t", created, ok)
	}
}

// TestExpiredCachePartialClearNotRetried 过期清空只删了一部分：这一次释放锁、改用临时 profile 内的缓存，写一次警告并把标记改为当前时间；
// 下一次启动照常使用这份缓存，不再重试清空、不再写警告
func TestExpiredCachePartialClearNotRetried(t *testing.T) {
	storagePath := testAccountDirectory(t)
	directory := accountCacheDirectory(storagePath)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	fillAccountCache(t, directory)
	writeCreated(t, directory, time.Now().Add(-AccountCacheMaxAge-time.Hour))
	original := removeAccountCacheEntry
	t.Cleanup(func() { removeAccountCacheEntry = original })
	removeAccountCacheEntry = func(path string) error {
		if filepath.Base(path) == "cache2" {
			return &os.PathError{Op: "unlinkat", Path: path, Err: os.ErrPermission}
		}
		return original(path)
	}
	var reported []error
	expired := func(_ int64, err error) { reported = append(reported, err) }
	got, lock, err := lockAccountCache(storagePath, expired)
	if err != nil || got != "" || lock != nil {
		t.Fatalf("只删了一部分时 = %q %v %v，期望这一次改用临时缓存", got, lock, err)
	}
	if !waitCacheLockFree(t, directory, time.Second) {
		t.Fatal("改用临时缓存时应释放缓存锁")
	}
	if created, ok := readAccountCacheCreated(directory); !ok || time.Since(created) > time.Minute {
		t.Fatalf("清空失败后标记应改为当前时间，实际 %v %t", created, ok)
	}
	got, lock, err = lockAccountCache(storagePath, expired)
	if err != nil || got != directory || lock == nil {
		t.Fatalf("下一次锁定 = %q %v %v，期望照常使用缓存目录", got, lock, err)
	}
	defer func() { _ = releaseAccountCache(lock) }()
	if len(reported) != 1 || reported[0] == nil || !strings.Contains(reported[0].Error(), "清空账户浏览器缓存") {
		t.Fatalf("过期回调 = %v，期望只有第一次报告一次清空失败", reported)
	}
}

// waitCacheLockFree 等到缓存锁能被另一个持有者拿到，超时返回 false
func waitCacheLockFree(t *testing.T, directory string, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		probe := flock.New(filepath.Join(directory, accountCacheLockName))
		locked, err := probe.TryLock()
		if err != nil {
			t.Fatal(err)
		}
		if locked {
			_ = probe.Unlock()
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestUnconfirmedBrowserKeepsCacheLock 浏览器没能确认退出（关闭超时）时缓存锁一直被持有，即使 Worker 已不再被引用、发生了 GC
// （原先锁对象随 Worker 被回收，文件描述符被终结器关闭，flock 随之释放）：清理拿不到锁、不删除；进程确认退出后才释放。
// 覆盖 Start 失败时的 abort、Worker.Close 与报告 BiDi 端点之前的 abortLaunch 三条路径
func TestUnconfirmedBrowserKeepsCacheLock(t *testing.T) {
	type pending struct {
		storagePath string
		directory   string
		exited      chan struct{}
	}
	paths := []string{testAccountDirectory(t), testAccountDirectory(t), testAccountDirectory(t)}
	cases := make([]pending, 0, len(paths))
	var group sync.WaitGroup
	results := make([]error, len(paths))
	for index, storagePath := range paths {
		directory, lock, err := lockAccountCache(storagePath, nil)
		if err != nil || lock == nil {
			t.Fatalf("锁定缓存: %v", err)
		}
		fillAccountCache(t, directory)
		// 浏览器进程不会退出：关闭等待超时
		process := &browserProcess{done: make(chan struct{})}
		cases = append(cases, pending{storagePath: storagePath, directory: directory, exited: process.done})
		group.Add(1)
		go func(index int, process *browserProcess, lock *flock.Flock) {
			defer group.Done()
			switch index {
			case 0:
				results[index] = (&Worker{process: process, cacheLock: lock}).abort()
			case 1:
				results[index] = (&Worker{process: process, cacheLock: lock}).Close()
			default:
				// 与 Start 的处理相同：没能确认退出时保留锁
				err := process.abortLaunch(errors.New("等待 Camoufox BiDi 端点超时: 45s"))
				var unconfirmed *browserExitUnconfirmedError
				if errors.As(err, &unconfirmed) {
					retainAccountCacheUntilExit(lock, unconfirmed.exited)
				}
				if err.Error() != "等待 Camoufox BiDi 端点超时: 45s" {
					results[index] = fmt.Errorf("文案应与原错误相同，实际 %q", err.Error())
					return
				}
				if unconfirmed == nil {
					return
				}
				results[index] = unconfirmed.err
			}
		}(index, process, lock)
	}
	group.Wait()
	for index, err := range results {
		if err == nil {
			t.Fatalf("路径 %d：关不掉的浏览器应返回错误", index)
		}
	}
	for range 3 {
		runtime.GC()
		time.Sleep(20 * time.Millisecond)
	}
	for index, item := range cases {
		if waitCacheLockFree(t, item.directory, 100*time.Millisecond) {
			t.Fatalf("路径 %d：浏览器没能确认退出时缓存锁不应被释放", index)
		}
		if cleared, _, err := ClearAccountCache(item.storagePath); err != nil || cleared {
			t.Fatalf("路径 %d：浏览器没能确认退出时不应清理: %t %v", index, cleared, err)
		}
	}
	for index, item := range cases {
		close(item.exited)
		if !waitCacheLockFree(t, item.directory, 5*time.Second) {
			t.Fatalf("路径 %d：浏览器进程退出后应释放缓存锁", index)
		}
	}
	unconfirmedCacheMu.Lock()
	remaining := len(unconfirmedCacheLocks)
	unconfirmedCacheMu.Unlock()
	if remaining != 0 {
		t.Fatalf("进程退出后应移出保留列表，剩余 %d", remaining)
	}
}
