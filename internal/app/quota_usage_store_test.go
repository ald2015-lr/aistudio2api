package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// sendGeneration 在账户上取一个生成租约、记一次发送并释放（今日发送数加一）
func sendGeneration(t *testing.T, pool *aistudio.AccountPool, accountID string) {
	t.Helper()
	lease, _, err := pool.TryAcquireFor(context.Background(), aistudio.AccountSelection{
		ModelID: testRuntimeModel, Method: "generateContent", AccountID: accountID,
	})
	if err != nil || lease == nil {
		t.Fatalf("取生成租约: %v", err)
	}
	lease.RecordGenerationSent()
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
}

// readQuotaUsageFile 读出记录文件
func readQuotaUsageFile(t *testing.T, path string) quotaUsageFile {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var file quotaUsageFile
	if err := json.Unmarshal(data, &file); err != nil {
		t.Fatal(err)
	}
	return file
}

// TestQuotaUsageStoreRoundTrip 写盘再读回：版本有变化才写（原子写、权限 0600、不留临时文件），文件带额度日与按账户 ID 的计数；
// 新的账户池打开同一天的文件时按 max 并回，打开后没有新发送不再写
func TestQuotaUsageStoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, quotaUsageFileName)
	pool, _ := testAccountPool(t, "store-a@example.com", "store-b@example.com")
	store := openQuotaUsageStore(path, pool, nil)
	if err := store.save(pool.QuotaDayUsage()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("没有发送过时不应写文件: %v", err)
	}
	sendGeneration(t, pool, "store-a@example.com")
	sendGeneration(t, pool, "store-a@example.com")
	sendGeneration(t, pool, "store-b@example.com")
	snapshot := pool.QuotaDayUsage()
	if err := store.save(snapshot); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("文件权限 = %v，期望 0600", info.Mode().Perm())
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".tmp") {
			t.Fatalf("原子写不应留下临时文件: %s", entry.Name())
		}
	}
	file := readQuotaUsageFile(t, path)
	if file.Version != quotaUsageFileVersion || file.DayKey != snapshot.DayKey || file.Day != snapshot.Day ||
		file.Accounts["store-a@example.com"] != 2 || file.Accounts["store-b@example.com"] != 1 {
		t.Fatalf("文件内容 = %+v，快照 = %+v", file, snapshot)
	}
	// 版本没变时不写：改坏文件后再保存同一个快照，文件保持改坏的样子
	if err := os.WriteFile(path, []byte("marker"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.save(snapshot); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(path); string(data) != "marker" {
		t.Fatal("计数版本没变时不应重写文件")
	}
	if err := store.save(aistudio.QuotaUsageSnapshot{DayKey: snapshot.DayKey, Day: snapshot.Day, Version: snapshot.Version + 1, Counts: snapshot.Counts}); err != nil {
		t.Fatal(err)
	}

	// 新的账户池（重启后）：同一天的文件并回，本池已有的计数按 max 合并
	restarted, _ := testAccountPool(t, "store-a@example.com", "store-b@example.com")
	sendGeneration(t, restarted, "store-b@example.com")
	sendGeneration(t, restarted, "store-b@example.com")
	reopened := openQuotaUsageStore(path, restarted, nil)
	counts := restarted.QuotaDayUsage().Counts
	if counts["store-a@example.com"] != 2 || counts["store-b@example.com"] != 2 {
		t.Fatalf("并回后的计数 = %v，期望 a=2、b=max(1,2)=2", counts)
	}
	before, _ := os.ReadFile(path)
	if err := reopened.save(restarted.QuotaDayUsage()); err != nil {
		t.Fatal(err)
	}
	if after, _ := os.ReadFile(path); string(after) != string(before) {
		t.Fatal("打开后没有新的发送，不应再写文件")
	}
}

// TestQuotaUsageStoreIgnoresStaleAndCorrupt 不是今天（太平洋时间）的文件、版本不符的文件忽略；损坏的文件忽略并写一条警告；
// 文件不存在时从 0 开始、不报错
func TestQuotaUsageStoreIgnoresStaleAndCorrupt(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	requests := newRequestRegistry(ctx)
	dir := t.TempDir()
	now := time.Now()
	today := aistudio.QuotaDayKey(now)
	write := func(name string, file quotaUsageFile) string {
		t.Helper()
		data, err := json.Marshal(file)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	accounts := map[string]uint32{"stale-a@example.com": 40}
	for _, test := range []struct {
		name string
		path string
		now  func() time.Time
	}{
		{name: "昨天的文件", path: write("yesterday.json", quotaUsageFile{Version: quotaUsageFileVersion, DayKey: aistudio.QuotaDayKey(now.Add(-36 * time.Hour)), Accounts: accounts})},
		{name: "版本不符", path: write("version.json", quotaUsageFile{Version: quotaUsageFileVersion + 1, DayKey: today, Accounts: accounts})},
		// 文件是今天写的，但按 now 已经到了第二天（跨过太平洋时间零点后才启动）
		{
			name: "启动时已是第二天", path: write("today.json", quotaUsageFile{Version: quotaUsageFileVersion, DayKey: today, Accounts: accounts}),
			now: func() time.Time { return now.Add(36 * time.Hour) },
		},
		{name: "文件不存在", path: filepath.Join(dir, "missing.json")},
	} {
		pool, _ := testAccountPool(t, "stale-a@example.com")
		openQuotaUsageStoreAt(test.path, pool, requests, test.now)
		if got := pool.QuotaDayUsage().Counts["stale-a@example.com"]; got != 0 {
			t.Fatalf("%s：不应并回，计数 = %d", test.name, got)
		}
	}
	if hasLogContaining(requests, "今日发送数记录读取失败") {
		t.Fatal("只有损坏的文件才写警告")
	}
	corrupt := filepath.Join(dir, "corrupt.json")
	if err := os.WriteFile(corrupt, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	pool, _ := testAccountPool(t, "stale-a@example.com")
	openQuotaUsageStoreAt(corrupt, pool, requests, nil)
	if got := pool.QuotaDayUsage().Counts["stale-a@example.com"]; got != 0 {
		t.Fatalf("损坏的文件不应并回，计数 = %d", got)
	}
	if !hasLogContaining(requests, "今日发送数记录读取失败，从 0 开始", "corrupt.json") {
		t.Fatalf("损坏的文件应写警告，日志 = %v", serviceLogMessages(requests))
	}
	// 今天的文件照常并回
	good := write("good.json", quotaUsageFile{Version: quotaUsageFileVersion, DayKey: today, Accounts: accounts})
	pool, _ = testAccountPool(t, "stale-a@example.com")
	openQuotaUsageStoreAt(good, pool, requests, func() time.Time { return now })
	if got := pool.QuotaDayUsage().Counts["stale-a@example.com"]; got != 40 {
		t.Fatalf("今天的文件应并回，计数 = %d", got)
	}
}

// TestQuotaUsageStoreOnlyWhenBalanced 默认部署不新增磁盘写入：平均调用关闭时 keepWarm 的一轮与关闭运行时都不写文件；
// 开启后 keepWarm 的一轮写出计数，关闭运行时把最后的计数写出
func TestQuotaUsageStoreOnlyWhenBalanced(t *testing.T) {
	const account = "only-a@example.com"
	env := newDrainEnv(t, []string{account}, 1, 2)
	path := filepath.Join(t.TempDir(), quotaUsageFileName)
	store := openQuotaUsageStore(path, env.pool, env.requests)
	env.manager.balancedState.store.Store(store)
	sendGeneration(t, env.pool, account)
	now := time.Now()
	env.service.balancedPass(env.ctx, now)
	if err := store.close(env.pool, env.manager.balancedOn()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("平均调用关闭时不应写文件: %v", err)
	}
	env.manager.setBalanced(true)
	env.service.balancedPass(env.ctx, now)
	if got := readQuotaUsageFile(t, path).Accounts[account]; got != 1 {
		t.Fatalf("开启后 keepWarm 的一轮应写出计数，文件里 = %d", got)
	}
	sendGeneration(t, env.pool, account)
	if err := store.close(env.pool, env.manager.balancedOn()); err != nil {
		t.Fatal(err)
	}
	if got := readQuotaUsageFile(t, path).Accounts[account]; got != 2 {
		t.Fatalf("关闭运行时应写出最后的计数，文件里 = %d", got)
	}
}

// TestQuotaUsageSavedOnStop 停止生成服务时立即写出今日发送数（平均调用开启时）：重建运行时（管理页停止再启动、应用配置后的重启）先停旧服务、
// 再建新一代（新一代此时读文件并回）、最后才关闭旧一代。原先只在关闭运行时写，新一代并回的是旧一代最后一次定时保存，旧一代之后的
// 20 次发送随后被新一代的写盘整份覆盖；现在新一代并回 30，旧一代关闭时没有新发送、不再写，新一代保存后文件为 31
func TestQuotaUsageSavedOnStop(t *testing.T) {
	const accountID = "stop-a@example.com"
	manager, oldPool, requests := partitionTestManager(t, []string{accountID}, nil, 1, 1, 0, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	service := retireTestService(ctx, manager, oldPool, requests)
	path := filepath.Join(t.TempDir(), quotaUsageFileName)
	oldStore := openQuotaUsageStore(path, oldPool, requests)
	manager.balancedState.store.Store(oldStore)
	manager.setBalanced(true)
	for range 10 {
		sendGeneration(t, oldPool, accountID)
	}
	// keepWarm 的定时保存，之后又发了 20 次
	if err := oldStore.save(oldPool.QuotaDayUsage()); err != nil {
		t.Fatal(err)
	}
	for range 20 {
		sendGeneration(t, oldPool, accountID)
	}
	modelRefreshDone := make(chan struct{})
	close(modelRefreshDone)
	service.finishStop(make(chan struct{}), modelRefreshDone)
	// 新一代（newRuntime）在旧一代关闭之前打开记录
	newPool, _ := testAccountPool(t, accountID)
	newStore := openQuotaUsageStore(path, newPool, requests)
	if got := newPool.QuotaDayUsage().Counts[accountID]; got != 30 {
		t.Fatalf("新一代应并回旧一代停止时的 30 次，实际 %d", got)
	}
	sendGeneration(t, newPool, accountID)
	// 旧一代关闭（closeRuntime）：停止之后没有新的发送，不再写
	if err := oldStore.close(oldPool, true); err != nil {
		t.Fatal(err)
	}
	if err := newStore.save(newPool.QuotaDayUsage()); err != nil {
		t.Fatal(err)
	}
	if got := readQuotaUsageFile(t, path).Accounts[accountID]; got != 31 {
		t.Fatalf("文件计数应为 31（旧一代 30 + 新一代 1），实际 %d", got)
	}
	// 平均调用关闭时停止服务不写盘
	manager.setBalanced(false)
	sendGeneration(t, oldPool, accountID)
	service.finishStop(make(chan struct{}), modelRefreshDone)
	if got := readQuotaUsageFile(t, path).Accounts[accountID]; got != 31 {
		t.Fatalf("平均调用关闭时停止服务不应写盘，文件计数 %d", got)
	}
}
