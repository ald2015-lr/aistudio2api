package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/camoufoxnative"
	"github.com/Mag1cFall/AIStudio2API/internal/proxyrelay"
	"github.com/gofrs/flock"
)

// levelLogs 返回指定级别、包含全部片段的运行日志
func levelLogs(requests *requestRegistry, level string, fragments ...string) []string {
	requests.mu.Lock()
	defer requests.mu.Unlock()
	var matched []string
	for _, entry := range requests.logs {
		if entry.Level != level {
			continue
		}
		ok := true
		for _, fragment := range fragments {
			ok = ok && strings.Contains(entry.Message, fragment)
		}
		if ok {
			matched = append(matched, entry.Message)
		}
	}
	return matched
}

// saveTestOrigins 把账户认证文件里的 localStorage 换成 origins
func saveTestOrigins(t *testing.T, pool *aistudio.AccountPool, accountID string, origins []aistudio.StorageOrigin) string {
	t.Helper()
	account, err := pool.Account(accountID)
	if err != nil {
		t.Fatal(err)
	}
	state, err := aistudio.LoadStorageState(account.StoragePath)
	if err != nil {
		t.Fatal(err)
	}
	state.Origins = origins
	if err := aistudio.WriteStorageState(account.StoragePath, state); err != nil {
		t.Fatal(err)
	}
	return account.StoragePath
}

func diskLocalStorage(t *testing.T, path string) map[string]map[string]string {
	t.Helper()
	state, err := aistudio.LoadStorageState(path)
	if err != nil {
		t.Fatal(err)
	}
	result := make(map[string]map[string]string, len(state.Origins))
	for _, origin := range state.Origins {
		items := make(map[string]string, len(origin.LocalStorage))
		for _, item := range origin.LocalStorage {
			items[item.Name] = item.Value
		}
		result[origin.Origin] = items
	}
	return result
}

const testAIStudioOrigin = "https://aistudio.google.com"

var savedTestOrigins = []aistudio.StorageOrigin{
	{Origin: "https://accounts.google.com", LocalStorage: []aistudio.StorageItem{{Name: "keep", Value: "1"}}},
	{Origin: testAIStudioOrigin, LocalStorage: []aistudio.StorageItem{
		{Name: "aiStudioUserPreference", Value: `{"format":"old"}`},
		{Name: "xap-tour", Value: "1"},
	}},
}

// localStorageLaunch 返回启动 stub Worker 的 launch，Worker 导出的 localStorage 由 export 给出
func localStorageLaunch(export func(context.Context) ([]byte, error)) func(context.Context, string, camoufoxnative.Options) (*aistudio.NativeWorker, error) {
	return func(_ context.Context, accountID string, _ camoufoxnative.Options) (*aistudio.NativeWorker, error) {
		worker, _ := aistudio.NewStubWorkerWithHooks(accountID, aistudio.StubWorkerHooks{LocalStorage: export})
		return worker, nil
	}
}

// TestStartupLocalStorageWriteback Worker 启动完成后把浏览器里 AI Studio 的 localStorage 写回认证文件：替换该 origin、保留其他 origin
func TestStartupLocalStorageWriteback(t *testing.T) {
	manager, pool, requests := partitionTestManager(t, []string{partitionNormalA}, nil, 1, 1, 0, 1)
	path := saveTestOrigins(t, pool, partitionNormalA, savedTestOrigins)
	encoded, err := json.Marshal(aistudio.StorageOrigin{Origin: testAIStudioOrigin, LocalStorage: []aistudio.StorageItem{
		{Name: "aiStudioUserPreference", Value: `{"format":"new"}`},
		{Name: "selectedProject", Value: "p"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	manager.launch = localStorageLaunch(func(context.Context) ([]byte, error) { return encoded, nil })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := manager.promote(ctx, partitionNormalA, testRuntimeModel); err != nil {
		t.Fatal(err)
	}
	stored := diskLocalStorage(t, path)
	aiStudio := stored[testAIStudioOrigin]
	if aiStudio["aiStudioUserPreference"] != `{"format":"new"}` || aiStudio["selectedProject"] != "p" || len(aiStudio) != 2 {
		t.Fatalf("AI Studio localStorage = %v，期望换成浏览器里的值", aiStudio)
	}
	if stored["https://accounts.google.com"]["keep"] != "1" {
		t.Fatalf("其他 origin 应保留，实际 %v", stored)
	}
	if warnings := levelLogs(requests, "WARN", "localStorage"); len(warnings) != 0 {
		t.Fatalf("写回成功不应有警告: %v", warnings)
	}
}

// TestStartupLocalStorageWritebackGeneration 启动期间保存过新登录（认证代际变化）时不写回，新登录的 localStorage 保留
func TestStartupLocalStorageWritebackGeneration(t *testing.T) {
	manager, pool, _ := partitionTestManager(t, []string{partitionNormalA}, nil, 1, 1, 0, 1)
	path := saveTestOrigins(t, pool, partitionNormalA, savedTestOrigins)
	encoded, err := json.Marshal(aistudio.StorageOrigin{Origin: testAIStudioOrigin, LocalStorage: []aistudio.StorageItem{
		{Name: "aiStudioUserPreference", Value: "from-stale-browser"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	manager.launch = localStorageLaunch(func(ctx context.Context) ([]byte, error) {
		// 导出之前模拟管理端保存了新登录：认证代际推进
		lease, err := pool.AcquireFor(ctx, aistudio.AccountSelection{AccountID: partitionNormalA})
		if err != nil {
			return nil, err
		}
		defer func() { _ = lease.Release() }()
		state, err := aistudio.LoadStorageState(path)
		if err != nil {
			return nil, err
		}
		state.Origins = []aistudio.StorageOrigin{{Origin: testAIStudioOrigin, LocalStorage: []aistudio.StorageItem{
			{Name: "aiStudioUserPreference", Value: "from-new-login"},
		}}}
		if err := lease.SaveStorageState(state); err != nil {
			return nil, err
		}
		return encoded, nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := manager.promote(ctx, partitionNormalA, testRuntimeModel); err != nil {
		t.Fatal(err)
	}
	if got := diskLocalStorage(t, path)[testAIStudioOrigin]["aiStudioUserPreference"]; got != "from-new-login" {
		t.Fatalf("代际变化后磁盘 = %q，期望保留新登录的值", got)
	}
}

// TestStartupLocalStorageWritebackFailure 导出失败与超过大小上限只写一条警告（不含值），不影响启动，认证文件不变
func TestStartupLocalStorageWritebackFailure(t *testing.T) {
	for _, test := range []struct {
		name     string
		export   func(context.Context) ([]byte, error)
		fragment string
	}{
		{
			name: "导出失败",
			export: func(context.Context) ([]byte, error) {
				return nil, errors.New("导出浏览器 localStorage: 页面 origin 不是 AI Studio")
			},
			fragment: "页面 origin 不是 AI Studio",
		},
		{
			name: "超过上限",
			export: func(context.Context) ([]byte, error) {
				return json.Marshal(aistudio.StorageOrigin{Origin: testAIStudioOrigin, LocalStorage: []aistudio.StorageItem{
					{Name: "big", Value: "SECRET-" + strings.Repeat("v", aistudio.BrowserLocalStorageLimit)},
				}})
			},
			fragment: "超过写回上限",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			manager, pool, requests := partitionTestManager(t, []string{partitionNormalA}, nil, 1, 1, 0, 1)
			path := saveTestOrigins(t, pool, partitionNormalA, savedTestOrigins)
			manager.launch = localStorageLaunch(test.export)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if _, err := manager.promote(ctx, partitionNormalA, testRuntimeModel); err != nil {
				t.Fatalf("写回失败不应影响启动: %v", err)
			}
			warnings := levelLogs(requests, "WARN", "WAA Worker 启动后写回 localStorage 失败 | 错误=", test.fragment)
			if len(warnings) != 1 {
				t.Fatalf("警告 = %v，期望一条", serviceLogMessages(requests))
			}
			if strings.Contains(warnings[0], "SECRET-") {
				t.Fatal("警告不应包含 localStorage 的值")
			}
			if got := diskLocalStorage(t, path)[testAIStudioOrigin]["aiStudioUserPreference"]; got != `{"format":"old"}` {
				t.Fatalf("失败时认证文件 = %q，期望不变", got)
			}
			if got := manager.WarmAccountIDs(); len(got) != 1 {
				t.Fatalf("Worker 应照常就绪，驻留 %v", got)
			}
		})
	}
}

// fillTestCache 在账户缓存目录写入约 3MB 模拟缓存，返回缓存目录
func fillTestCache(t *testing.T, storagePath string) string {
	t.Helper()
	directory := filepath.Join(filepath.Dir(storagePath), "camoufox-cache")
	entries := filepath.Join(directory, "cache2", "entries")
	if err := os.MkdirAll(entries, 0o700); err != nil {
		t.Fatal(err)
	}
	for index := range 3 {
		if err := os.WriteFile(filepath.Join(entries, fmt.Sprintf("e%d", index)), make([]byte, 1<<20), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return directory
}

func cacheFilled(t *testing.T, directory string) bool {
	t.Helper()
	_, err := os.Stat(filepath.Join(directory, "cache2", "entries", "e0"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return err == nil
}

// failingLaunch 返回总是以 err 失败的 launch：与拿到账户缓存锁的 camoufoxnative.Start 一样先报告用上了账户缓存
func failingLaunch(err error) func(context.Context, string, camoufoxnative.Options) (*aistudio.NativeWorker, error) {
	return func(_ context.Context, _ string, options camoufoxnative.Options) (*aistudio.NativeWorker, error) {
		if options.CacheAttached == nil {
			return nil, errors.New("启动参数缺少账户缓存回调")
		}
		options.CacheAttached()
		return nil, err
	}
}

// TestStartupFailureWithoutAccountCacheKeepsCache 失败的这次启动没有用上账户缓存（同账户另一个 runtime 占着缓存锁，用的是临时 profile
// 内的缓存）时不清理：即使那个 runtime 在 launch 返回之前关闭、释放了锁，它一直正常使用的缓存也保留，不写清理日志、不占用 10 分钟名额；
// 下一次用上了账户缓存的启动同样失败时照常清理
func TestStartupFailureWithoutAccountCacheKeepsCache(t *testing.T) {
	manager, pool, requests := partitionTestManager(t, []string{partitionNormalA}, nil, 1, 1, 0, 1)
	directory := fillTestCache(t, accountStoragePath(t, pool, partitionNormalA))
	writeTestCacheCreated(t, directory, time.Now().Add(-camoufoxnative.AccountCacheClearMinAge-time.Hour))
	holder := flock.New(filepath.Join(directory, ".lock"))
	if locked, err := holder.TryLock(); err != nil || !locked {
		t.Fatalf("持有缓存锁失败: %t %v", locked, err)
	}
	pageErr := fmt.Errorf("官网 WAA service 未暴露: %w", errors.New("等待页面条件超时"))
	manager.launch = func(context.Context, string, camoufoxnative.Options) (*aistudio.NativeWorker, error) {
		// 与拿不到缓存锁的 camoufoxnative.Start 一样不报告用上账户缓存；占着锁的 runtime 在这次启动返回之前关闭
		if err := holder.Unlock(); err != nil {
			return nil, err
		}
		return nil, pageErr
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := manager.promote(ctx, partitionNormalA, testRuntimeModel); err == nil {
		t.Fatal("启动应失败")
	}
	if !cacheFilled(t, directory) {
		t.Fatal("没有用上账户缓存的启动失败不应清理缓存")
	}
	if got := levelLogs(requests, "INFO", "清理账号浏览器缓存"); len(got) != 0 {
		t.Fatalf("没有清理时不应写清理日志: %v", got)
	}

	manager.launch = failingLaunch(pageErr)
	if _, err := manager.promote(ctx, partitionNormalA, testRuntimeModel); err == nil {
		t.Fatal("启动应失败")
	}
	if cacheFilled(t, directory) {
		t.Fatal("用上了账户缓存的启动失败应清理（上一次没有占用 10 分钟名额）")
	}
	if got := levelLogs(requests, "INFO", "清理账号浏览器缓存 | 原因=官网 WAA service 未暴露 | 大小=3.0MB"); len(got) != 1 {
		t.Fatalf("应写一行清理日志: %v", serviceLogMessages(requests))
	}
}

func accountStoragePath(t *testing.T, pool *aistudio.AccountPool, accountID string) string {
	t.Helper()
	account, err := pool.Account(accountID)
	if err != nil {
		t.Fatal(err)
	}
	return account.StoragePath
}

// TestStartupFailureHealsBrowserCache 页面状态类启动失败后清空账户浏览器缓存并写一行 INFO；同账号 10 分钟内最多清理一次；
// 过了 10 分钟，缓存仍是 12 小时内新建的不清，早于 12 小时的再次失败时再清
func TestStartupFailureHealsBrowserCache(t *testing.T) {
	manager, pool, requests := partitionTestManager(t, []string{partitionNormalA}, nil, 1, 1, 0, 1)
	directory := fillTestCache(t, accountStoragePath(t, pool, partitionNormalA))
	pageErr := fmt.Errorf("AI Studio 输入框未就绪 url=%s: %w", "https://aistudio.google.com/prompts/new_chat",
		errors.New("等待页面条件超时"))
	manager.launch = failingLaunch(pageErr)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := manager.promote(ctx, partitionNormalA, testRuntimeModel); err == nil {
		t.Fatal("启动应失败")
	}
	if cacheFilled(t, directory) {
		t.Fatal("页面状态类失败后缓存应被清空")
	}
	if _, err := os.Stat(filepath.Join(directory, ".lock")); err != nil {
		t.Fatalf("清空时保留锁文件: %v", err)
	}
	infos := levelLogs(requests, "INFO", "清理账号浏览器缓存 | 原因=AI Studio 输入框未就绪 | 大小=3.0MB")
	if len(infos) != 1 {
		t.Fatalf("应写一行清理日志，日志 = %v", serviceLogMessages(requests))
	}

	// 10 分钟内再次失败：不再清理
	fillTestCache(t, accountStoragePath(t, pool, partitionNormalA))
	if _, err := manager.promote(ctx, partitionNormalA, testRuntimeModel); err == nil {
		t.Fatal("启动应失败")
	}
	if !cacheFilled(t, directory) {
		t.Fatal("10 分钟内不应再次清理")
	}
	if got := levelLogs(requests, "INFO", "清理账号浏览器缓存"); len(got) != 1 {
		t.Fatalf("10 分钟内不应再写清理日志: %v", got)
	}

	// 上次清理已过 10 分钟（或服务重启、内存去重清零），但缓存是上次清空后新建的（创建时间不到 12 小时）：不清理、不写日志
	manager.cacheHealMu.Lock()
	manager.cacheHealedAt[partitionNormalA] = time.Now().Add(-browserCacheHealInterval - time.Second)
	manager.cacheHealMu.Unlock()
	manager.launch = failingLaunch(fmt.Errorf("官网 WAA service 未暴露: %w", errors.New("等待页面条件超时")))
	if _, err := manager.promote(ctx, partitionNormalA, testRuntimeModel); err == nil {
		t.Fatal("启动应失败")
	}
	if !cacheFilled(t, directory) {
		t.Fatal("最近 12 小时内清空过的缓存不应再清理")
	}
	if got := levelLogs(requests, "INFO", "清理账号浏览器缓存"); len(got) != 1 {
		t.Fatalf("缓存太新时不应写清理日志: %v", got)
	}

	// 缓存创建时间早于 12 小时：再次失败时再清（缓存太新的那次不占用 10 分钟名额）
	writeTestCacheCreated(t, directory, time.Now().Add(-camoufoxnative.AccountCacheClearMinAge-time.Minute))
	if _, err := manager.promote(ctx, partitionNormalA, testRuntimeModel); err == nil {
		t.Fatal("启动应失败")
	}
	if cacheFilled(t, directory) {
		t.Fatal("缓存早于 12 小时应再次清理")
	}
	if got := levelLogs(requests, "INFO", "清理账号浏览器缓存 | 原因=官网 WAA service 未暴露"); len(got) != 1 {
		t.Fatalf("应写第二行清理日志: %v", serviceLogMessages(requests))
	}
}

// writeTestCacheCreated 把缓存目录的创建时间标记改成 created
func writeTestCacheCreated(t *testing.T, directory string, created time.Time) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(directory, ".created"), []byte(created.UTC().Format(time.RFC3339)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// healTestAccounts 创建 count 个带认证文件与约 3MB 缓存的账户目录，返回认证文件路径
func healTestAccounts(t *testing.T, count int) []string {
	t.Helper()
	root := t.TempDir()
	paths := make([]string, 0, count)
	for index := range count {
		directory := filepath.Join(root, fmt.Sprintf("heal-%02d@example.com", index))
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(directory, "storage-state.json")
		if err := os.WriteFile(path, []byte(`{"cookies":[],"origins":[]}`), 0o600); err != nil {
			t.Fatal(err)
		}
		fillTestCache(t, path)
		paths = append(paths, path)
	}
	return paths
}

// TestBrowserCacheHealPause 清理没有用时暂停：连续 20 个清理过缓存的账户下一次启动仍以页面状态类失败，写一行警告并暂停清理 1 小时，
// 暂停期间别的账户失败也不清理；清理过缓存的账户启动成功时连续无效计数归零；超过 1 小时才有结果的不计
func TestBrowserCacheHealPause(t *testing.T) {
	manager, _, requests := partitionTestManager(t, []string{partitionNormalA}, nil, 1, 1, 0, 1)
	paths := healTestAccounts(t, browserCacheHealMissLimit+2)
	pageErr := fmt.Errorf("官网 WAA service 未暴露: %w", errors.New("等待页面条件超时"))
	cacheOf := func(path string) string { return filepath.Join(filepath.Dir(path), "camoufox-cache") }
	idOf := func(path string) string { return filepath.Base(filepath.Dir(path)) }

	// 清理过缓存的账户再次页面失败记一次无效；清理过缓存的账户（这里直接登记 partitionNormalA）启动成功：有效，计数归零
	// （经 Worker 启动成功的路径记录）
	manager.healBrowserCache(idOf(paths[0]), idOf(paths[0]), paths[0], pageErr)
	manager.healBrowserCache(idOf(paths[0]), idOf(paths[0]), paths[0], pageErr)
	manager.cacheHealMu.Lock()
	misses := manager.cacheHealMisses
	manager.cacheHealPending[partitionNormalA] = time.Now()
	manager.cacheHealMu.Unlock()
	if misses != 1 {
		t.Fatalf("清理过缓存的账户再次页面失败应记一次无效，实际 %d", misses)
	}
	manager.launch = localStorageLaunch(func(context.Context) ([]byte, error) { return nil, nil })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := manager.promote(ctx, partitionNormalA, testRuntimeModel); err != nil {
		t.Fatal(err)
	}
	manager.cacheHealMu.Lock()
	misses = manager.cacheHealMisses
	_, stillPending := manager.cacheHealPending[partitionNormalA]
	manager.cacheHealMu.Unlock()
	if misses != 0 || stillPending {
		t.Fatalf("清理过缓存的账户启动成功后计数应归零：misses=%d pending=%t", misses, stillPending)
	}

	// 超过 1 小时才有结果的不计
	manager.cacheHealMu.Lock()
	manager.cacheHealPending["late@example.com"] = time.Now().Add(-browserCacheHealOutcomeWindow - time.Second)
	manager.noteBrowserCacheHealOutcomeLocked("late@example.com", false, time.Now())
	misses = manager.cacheHealMisses
	manager.cacheHealMu.Unlock()
	if misses != 0 {
		t.Fatalf("超过 1 小时才有的结果不应计入，实际 %d", misses)
	}

	// 连续 20 个清理过缓存的账户下一次启动仍失败：暂停
	for _, path := range paths[1 : browserCacheHealMissLimit+1] {
		manager.healBrowserCache(idOf(path), idOf(path), path, pageErr)
		if cacheFilled(t, cacheOf(path)) {
			t.Fatalf("%s 第一次页面失败应清理", idOf(path))
		}
	}
	if warnings := levelLogs(requests, "WARN", "暂停启动失败后的浏览器缓存清理"); len(warnings) != 0 {
		t.Fatalf("还没有结果时不应暂停: %v", warnings)
	}
	for _, path := range paths[1 : browserCacheHealMissLimit+1] {
		manager.healBrowserCache(idOf(path), idOf(path), path, pageErr)
	}
	warnings := levelLogs(requests, "WARN", "暂停启动失败后的浏览器缓存清理 | 原因=连续 20 个清理过缓存的账号下一次启动仍失败 | 时长=1h0m0s")
	if len(warnings) != 1 {
		t.Fatalf("连续 20 次无效应写一行暂停警告，日志 = %v", serviceLogMessages(requests))
	}
	last := paths[browserCacheHealMissLimit+1]
	manager.healBrowserCache(idOf(last), idOf(last), last, pageErr)
	if !cacheFilled(t, cacheOf(last)) {
		t.Fatal("暂停期间不应清理")
	}

	// 暂停结束后照常清理
	manager.cacheHealMu.Lock()
	manager.cacheHealPausedUntil = time.Now().Add(-time.Second)
	manager.cacheHealMu.Unlock()
	manager.healBrowserCache(idOf(last), idOf(last), last, pageErr)
	if cacheFilled(t, cacheOf(last)) {
		t.Fatal("暂停结束后应照常清理")
	}
}

// TestStartupFailureKeepsBrowserCache 其他启动失败（连接 BiDi、登录跳转）与本机代理中转记到上游失败的页面错误都不清理缓存；
// 缓存锁被别的 runtime 持有时不清理、也不占用 10 分钟的名额，锁释放后的下一次失败照常清理
func TestStartupFailureKeepsBrowserCache(t *testing.T) {
	manager, pool, requests := partitionTestManager(t, []string{partitionNormalA}, nil, 1, 1, 0, 1)
	storagePath := accountStoragePath(t, pool, partitionNormalA)
	directory := fillTestCache(t, storagePath)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pageErr := fmt.Errorf("填写 bootstrap 提示词失败 value=%q err=%v", "", errors.New("提示词输入框不存在"))
	for _, cause := range []error{
		fmt.Errorf("连接 Camoufox BiDi: %w", errors.New("connection refused")),
		errors.New("等待 Camoufox BiDi 端点超时: 45s"),
		fmt.Errorf("%w；本机代理中转：连接上游超时%w", pageErr, proxyrelay.ErrUpstream),
	} {
		manager.launch = failingLaunch(cause)
		if _, err := manager.promote(ctx, partitionNormalA, testRuntimeModel); err == nil {
			t.Fatal("启动应失败")
		}
		if !cacheFilled(t, directory) {
			t.Fatalf("%v 不应清理缓存", cause)
		}
	}

	holder := flock.New(filepath.Join(directory, ".lock"))
	if locked, err := holder.TryLock(); err != nil || !locked {
		t.Fatalf("持有缓存锁失败: %t %v", locked, err)
	}
	manager.launch = failingLaunch(pageErr)
	if _, err := manager.promote(ctx, partitionNormalA, testRuntimeModel); err == nil {
		t.Fatal("启动应失败")
	}
	if !cacheFilled(t, directory) {
		t.Fatal("缓存锁被持有时不应清理")
	}
	if got := levelLogs(requests, "INFO", "清理账号浏览器缓存"); len(got) != 0 {
		t.Fatalf("没有清理时不应写清理日志: %v", got)
	}
	if err := holder.Unlock(); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.promote(ctx, partitionNormalA, testRuntimeModel); err == nil {
		t.Fatal("启动应失败")
	}
	if cacheFilled(t, directory) {
		t.Fatal("锁释放后的下一次页面失败应清理（取不到锁时不占用 10 分钟名额）")
	}
	if got := levelLogs(requests, "INFO", "清理账号浏览器缓存 | 原因=填写 bootstrap 提示词失败"); len(got) != 1 {
		t.Fatalf("应写一行清理日志: %v", serviceLogMessages(requests))
	}
}

// TestStartupCacheExpiredLog 启动时清空过期缓存的回调写同一种清理日志（原因=缓存超过 3 天），清空失败时写 WARN
func TestStartupCacheExpiredLog(t *testing.T) {
	manager, _, requests := partitionTestManager(t, []string{partitionNormalA}, nil, 1, 1, 0, 1)
	manager.launch = func(_ context.Context, accountID string, options camoufoxnative.Options) (*aistudio.NativeWorker, error) {
		if options.CacheExpired == nil {
			return nil, errors.New("启动参数缺少过期缓存回调")
		}
		options.CacheExpired(5<<20, nil)
		options.CacheExpired(0, errors.New("清空账户浏览器缓存: permission denied"))
		worker, _ := aistudio.NewStubWorker(accountID, 0)
		return worker, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := manager.promote(ctx, partitionNormalA, testRuntimeModel); err != nil {
		t.Fatal(err)
	}
	if browserCacheExpiredReason() != "缓存超过 3 天" {
		t.Fatalf("过期原因 = %q", browserCacheExpiredReason())
	}
	if got := levelLogs(requests, "INFO", "清理账号浏览器缓存 | 原因=缓存超过 3 天 | 大小=5.0MB"); len(got) != 1 {
		t.Fatalf("应写过期清理日志: %v", serviceLogMessages(requests))
	}
	if got := levelLogs(requests, "WARN", "清理账号浏览器缓存失败 | 原因=缓存超过 3 天 | 错误=清空账户浏览器缓存: permission denied"); len(got) != 1 {
		t.Fatalf("清空失败应写警告: %v", serviceLogMessages(requests))
	}
}

// TestStartupWritebackSkipsExternalLogin 外部替换认证文件（重新上传）后、扫描载入之前完成的 Worker 启动不写回 Cookie 与 localStorage：
// 新登录不被旧浏览器的值覆盖，之后的账户目录扫描仍能发现并载入这次重新上传
func TestStartupWritebackSkipsExternalLogin(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	const accountID = "external@example.com"
	admin, _ := deniedWatcherAdmin(t, nil, accountID)
	pool := admin.pool
	account, err := pool.Account(accountID)
	if err != nil {
		t.Fatal(err)
	}
	base := time.Now().Add(-time.Minute)
	settleAuthFile(t, pool, accountID, base)
	watcher := newTestAccountWatcher(admin)
	scanAndWait(t, watcher)

	staleCookies, err := json.Marshal(authTestState(t, "STALE-BROWSER", false).Cookies)
	if err != nil {
		t.Fatal(err)
	}
	staleStorage, err := json.Marshal(aistudio.StorageOrigin{Origin: testAIStudioOrigin, LocalStorage: []aistudio.StorageItem{
		{Name: "aiStudioUserPreference", Value: "old-browser"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	uploaded := authTestState(t, "UPLOADED", false)
	uploaded.Origins = []aistudio.StorageOrigin{{Origin: testAIStudioOrigin, LocalStorage: []aistudio.StorageItem{
		{Name: "aiStudioUserPreference", Value: "new-login"},
	}}}
	admin.workers.launch = func(_ context.Context, id string, _ camoufoxnative.Options) (*aistudio.NativeWorker, error) {
		// 浏览器启动期间外部换了认证文件，扫描还没载入（认证代际没变）
		if err := aistudio.WriteStorageState(account.StoragePath, uploaded); err != nil {
			return nil, err
		}
		worker, _ := aistudio.NewStubWorkerWithHooks(id, aistudio.StubWorkerHooks{
			StorageCookies: func(context.Context) ([]byte, error) { return staleCookies, nil },
			LocalStorage:   func(context.Context) ([]byte, error) { return staleStorage, nil },
		})
		return worker, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := admin.workers.promote(ctx, accountID, testRuntimeModel); err != nil {
		t.Fatal(err)
	}
	disk, err := aistudio.LoadStorageState(account.StoragePath)
	if err != nil {
		t.Fatal(err)
	}
	if aistudio.LoginIdentity(disk) != aistudio.LoginIdentity(uploaded) {
		t.Fatal("新登录的签名 Cookie 不应被旧浏览器的 Cookie 覆盖")
	}
	if got := diskLocalStorage(t, account.StoragePath)[testAIStudioOrigin]["aiStudioUserPreference"]; got != "new-login" {
		t.Fatalf("新登录的 localStorage = %q，不应被旧浏览器的值覆盖", got)
	}
	if identity, _ := pool.LoginIdentity(accountID); identity == aistudio.LoginIdentity(uploaded) {
		t.Fatal("跳过写回时内存不应变成磁盘上的新登录")
	}

	settleAuthFile(t, pool, accountID, base.Add(10*time.Second))
	scanAndWait(t, watcher)
	if identity, _ := pool.LoginIdentity(accountID); identity != aistudio.LoginIdentity(uploaded) {
		t.Fatalf("扫描应载入重新上传的认证文件，日志 = %v", serviceLogMessages(admin.requests))
	}
	if !hasLogContaining(admin.requests, "检测到认证文件更新", "账户="+accountID) {
		t.Fatalf("应写载入日志，日志 = %v", serviceLogMessages(admin.requests))
	}
}
