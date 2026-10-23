package app

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// syncTestCatalog 为调度器测试的假目录服务：成功时把 data 写进账户池；outcome 非空时按它决定第 call 次（全部账户合计）的结果；
// busy 中的账户在 Try 版本里返回忙；gate 非空时同步要等它关闭
type syncTestCatalog struct {
	pool    *aistudio.AccountPool
	pooled  *aistudio.PooledService
	data    aistudio.ModelCatalogData
	mu      sync.Mutex
	calls   []string
	starts  []time.Time
	active  int
	peak    int
	delay   time.Duration
	gate    chan struct{}
	busy    map[string]bool
	outcome func(accountID string, call int) error
	// cachedCalls 为 CachedModels（合并应用全量目录）的调用次数
	cachedCalls atomic.Int32
}

func (catalog *syncTestCatalog) RefreshAccountModels(ctx context.Context, accountID string) ([]aistudio.Model, error) {
	models, _, err := catalog.run(ctx, accountID, false)
	return models, err
}

func (catalog *syncTestCatalog) TryRefreshAccountModels(ctx context.Context, accountID string) ([]aistudio.Model, bool, error) {
	return catalog.run(ctx, accountID, true)
}

func (catalog *syncTestCatalog) CachedModels() []aistudio.Model {
	catalog.cachedCalls.Add(1)
	return catalog.pooled.CachedModels()
}

func (catalog *syncTestCatalog) CatalogSnapshot(accountID string) (aistudio.CatalogSnapshot, bool) {
	status, ok := catalog.pool.StatusOf(accountID)
	if !ok || len(status.Models) == 0 {
		return aistudio.CatalogSnapshot{}, false
	}
	return aistudio.CatalogSnapshot{Playground: catalog.data}, true
}

func (catalog *syncTestCatalog) run(ctx context.Context, accountID string, try bool) ([]aistudio.Model, bool, error) {
	catalog.mu.Lock()
	if try && catalog.busy[accountID] {
		catalog.mu.Unlock()
		return nil, true, nil
	}
	catalog.calls = append(catalog.calls, accountID)
	catalog.starts = append(catalog.starts, time.Now())
	call := len(catalog.calls)
	catalog.active++
	catalog.peak = max(catalog.peak, catalog.active)
	gate, delay, outcome := catalog.gate, catalog.delay, catalog.outcome
	catalog.mu.Unlock()
	defer func() {
		catalog.mu.Lock()
		catalog.active--
		catalog.mu.Unlock()
	}()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return nil, false, ctx.Err()
		}
	}
	if delay > 0 {
		time.Sleep(delay)
	}
	if outcome != nil {
		if err := outcome(accountID, call); err != nil {
			if aistudio.DefinitiveAuthenticationFailure(err) {
				_ = catalog.pool.MarkAuthRequired(accountID, err.Error())
			}
			return nil, false, err
		}
	}
	models := catalog.data.Models()
	if err := catalog.pool.SetCatalog(accountID, aistudio.BenefitTierFree, models); err != nil {
		return nil, false, err
	}
	return models, false, nil
}

func (catalog *syncTestCatalog) callCount() int {
	catalog.mu.Lock()
	defer catalog.mu.Unlock()
	return len(catalog.calls)
}

func (catalog *syncTestCatalog) callsFor(accountID string) int {
	catalog.mu.Lock()
	defer catalog.mu.Unlock()
	count := 0
	for _, called := range catalog.calls {
		if called == accountID {
			count++
		}
	}
	return count
}

// syncTestClock 为假时钟：每次读取前进 1 毫秒（调度器的开始间隔不会卡住），Advance 手动推进
type syncTestClock struct {
	mu  sync.Mutex
	now time.Time
}

func newSyncTestClock() *syncTestClock {
	return &syncTestClock{now: time.Date(2026, 10, 23, 8, 0, 0, 0, time.UTC)}
}

func (clock *syncTestClock) Now() time.Time {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	clock.now = clock.now.Add(time.Millisecond)
	return clock.now
}

func (clock *syncTestClock) Advance(duration time.Duration) {
	clock.mu.Lock()
	clock.now = clock.now.Add(duration)
	clock.mu.Unlock()
}

// syncTestEnv 为调度器测试环境：root 下的账户、账户池、假目录服务与调度器（不带磁盘缓存，见 withCache）
type syncTestEnv struct {
	root    string
	ctx     context.Context
	cancel  context.CancelFunc
	gen     *catalogGeneration
	catalog *syncTestCatalog
	service *trackedService
	syncer  *catalogSyncer
}

func newSyncTestEnv(t *testing.T, ids ...string) *syncTestEnv {
	t.Helper()
	root := t.TempDir()
	catalogTestAccounts(t, root, ids...)
	return newSyncTestEnvAt(t, root)
}

func newSyncTestEnvAt(t *testing.T, root string) *syncTestEnv {
	t.Helper()
	return newSyncTestEnvWith(t, root, nil)
}

// newSyncTestEnvWith 与 newSyncTestEnvAt 相同，账户池服务用 transport 作假上游（测试可把 service.catalog 换成真实的 gen.pooled）
func newSyncTestEnvWith(t *testing.T, root string, transport *catalogRPCTransport) *syncTestEnv {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	gen := newCatalogGeneration(t, root, transport)
	catalog := &syncTestCatalog{pool: gen.pool, pooled: gen.pooled, data: catalogTestData(t, "alpha"), busy: make(map[string]bool)}
	service := &trackedService{lifecycle: ctx, catalog: catalog, pool: gen.pool, requests: newRequestRegistry(ctx)}
	service.timeout.Store(int64(time.Minute))
	syncer := newCatalogSyncer(service)
	service.catalogSync = syncer
	syncer.tick = 5 * time.Millisecond
	syncer.interval, syncer.fillInterval = time.Nanosecond, time.Nanosecond
	syncer.random = func() float64 { return 0.5 }
	return &syncTestEnv{root: root, ctx: ctx, cancel: cancel, gen: gen, catalog: catalog, service: service, syncer: syncer}
}

// start 启动调度器，测试结束时停止并等它退出
func (env *syncTestEnv) start(t *testing.T) (<-chan error, <-chan struct{}, <-chan struct{}) {
	t.Helper()
	ready, synced, done := env.syncer.start(env.ctx)
	t.Cleanup(func() {
		env.cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("调度器 5 秒内没有退出")
		}
	})
	return ready, synced, done
}

func (env *syncTestEnv) task(accountID string) (catalogTask, bool) {
	env.syncer.mu.Lock()
	defer env.syncer.mu.Unlock()
	task := env.syncer.tasks[accountID]
	if task == nil {
		return catalogTask{}, false
	}
	return *task, true
}

// syncTestCacheFor 模拟上一代运行时同步过这些账户：账户池写入目录（运行态记下目录指纹），缓存写好记录（at 为同步时间）并落盘。
// 返回上一代的账户池
func syncTestCacheFor(t *testing.T, root string, dir string, data aistudio.ModelCatalogData, synced map[string]time.Time) *catalogGeneration {
	t.Helper()
	previous := newCatalogGeneration(t, root, nil)
	cache := openCatalogCache(dir, nil)
	for accountID, at := range synced {
		if err := previous.pool.SetCatalog(accountID, aistudio.BenefitTierFree, data.Models()); err != nil {
			t.Fatal(err)
		}
		cache.put(accountID, aistudio.CatalogSnapshot{Playground: data}, at)
	}
	if err := cache.flush(); err != nil {
		t.Fatal(err)
	}
	return previous
}

// restoreFrom 从 dir 恢复缓存并挂到调度器
func (env *syncTestEnv) restoreFrom(dir string) catalogRestoreSummary {
	cache := openCatalogCache(dir, env.service.requests)
	summary := cache.restore(env.gen.pool, env.gen.client, false)
	env.syncer.attach(cache, summary)
	return summary
}

func waitCatalog(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatalf("等待超时：%s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// TestCatalogStartWithCacheNoRequests 有缓存时启动不发目录请求：ready 立即为 nil、synced 立即关闭，
// 启动日志“来自磁盘=N”
func TestCatalogStartWithCacheNoRequests(t *testing.T) {
	ids := []string{"a@example.com", "b@example.com", "c@example.com"}
	root, dir := t.TempDir(), filepath.Join(t.TempDir(), "model-catalogs")
	catalogTestAccounts(t, root, ids...)
	now := time.Now()
	syncTestCacheFor(t, root, dir, catalogTestData(t, "alpha"), map[string]time.Time{ids[0]: now, ids[1]: now, ids[2]: now})
	env := newSyncTestEnvAt(t, root)
	if summary := env.restoreFrom(dir); summary.FromDisk != 3 {
		t.Fatalf("恢复结果 %+v", summary)
	}
	plan := env.syncer.plan()
	if plan.fromDisk != 3 || plan.missing != 0 || plan.expired != 0 {
		t.Fatalf("启动计划 %+v", plan)
	}
	if line := plan.startLine(1); line != "生成服务启动 | 1/2 | 准备模型目录 | 缓存=1 | 来自磁盘=3 | 待同步=0 | 过期=0 | 不一致=0 | 疑似失效=0" {
		t.Fatalf("启动日志 %q", line)
	}
	ready, synced, _ := env.start(t)
	select {
	case err := <-ready:
		if err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatal("有缓存时 ready 应立即送出")
	}
	select {
	case <-synced:
	default:
		t.Fatal("没有缺失任务时 synced 应立即关闭")
	}
	time.Sleep(50 * time.Millisecond)
	if calls := env.catalog.callCount(); calls != 0 {
		t.Fatalf("有缓存时启动不应同步目录，实际 %d 次", calls)
	}
	if !hasLogContaining(env.service.requests, "模型目录缓存载入 | 账户=3 | 目录=1") {
		t.Fatal("缺少“模型目录缓存载入”日志")
	}
}

// TestCatalogStartWithoutCacheWaitsFirst 没有缓存时 ready 等第一个非空结果；第一轮缺失任务都有结果后 synced 关闭并写
// “模型目录后台同步完成”；第一轮全部失败时 ready 送出合并的错误
func TestCatalogStartWithoutCacheWaitsFirst(t *testing.T) {
	t.Run("等第一个结果", func(t *testing.T) {
		env := newSyncTestEnv(t, "a@example.com", "b@example.com", "c@example.com")
		gate := make(chan struct{})
		env.catalog.gate = gate
		ready, synced, _ := env.start(t)
		select {
		case <-ready:
			t.Fatal("没有目录时 ready 应等第一个结果")
		case <-time.After(50 * time.Millisecond):
		}
		close(gate)
		select {
		case err := <-ready:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("第一个结果后 ready 应送出")
		}
		if len(env.service.modelSnapshot()) == 0 {
			t.Fatal("ready 送出前应已应用第一个目录")
		}
		select {
		case <-synced:
		case <-time.After(5 * time.Second):
			t.Fatal("第一轮缺失任务完成后 synced 应关闭")
		}
		if !hasLogContaining(env.service.requests, "模型目录后台同步完成 | 同步=3 | 非空=3 | 模型=1 | 待重试账户=0 | 耗时=", "| 来自磁盘=0") {
			t.Fatal("缺少“模型目录后台同步完成”日志")
		}
	})
	t.Run("全部失败", func(t *testing.T) {
		env := newSyncTestEnv(t, "a@example.com", "b@example.com")
		env.catalog.outcome = func(string, int) error { return errors.New("上游不可用") }
		ready, synced, _ := env.start(t)
		select {
		case err := <-ready:
			if err == nil || !strings.Contains(err.Error(), "上游不可用") {
				t.Fatalf("全部失败时 ready 应送出合并的错误，实际 %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("第一轮全部失败后 ready 应送出")
		}
		<-synced
	})
}

func startGaps(starts []time.Time) []time.Duration {
	sorted := slices.Clone(starts)
	slices.SortFunc(sorted, func(left, right time.Time) int { return left.Compare(right) })
	gaps := make([]time.Duration, 0, len(sorted))
	for index := 1; index < len(sorted); index++ {
		gaps = append(gaps, sorted[index].Sub(sorted[index-1]))
	}
	return gaps
}

// TestCatalogSyncRate 并发不超过上限、两次开始的间隔不小于设定；没有缓存（有缺失、有目录的启用账户不到填充目标）时用填充速率，
// 否则用常规速率
func TestCatalogSyncRate(t *testing.T) {
	t.Run("填充", func(t *testing.T) {
		ids := make([]string, 10)
		for index := range ids {
			ids[index] = fmt.Sprintf("fill-%02d@example.com", index)
		}
		env := newSyncTestEnv(t, ids...)
		env.catalog.delay = 200 * time.Millisecond
		env.syncer.fillInterval = 40 * time.Millisecond
		env.syncer.interval = time.Second
		_, synced, _ := env.start(t)
		select {
		case <-synced:
		case <-time.After(10 * time.Second):
			t.Fatal("填充没有完成")
		}
		env.catalog.mu.Lock()
		peak, starts := env.catalog.peak, slices.Clone(env.catalog.starts)
		env.catalog.mu.Unlock()
		if peak > catalogFillConcurrency || peak < 3 {
			t.Fatalf("填充期并发峰值 %d，期望 3–%d", peak, catalogFillConcurrency)
		}
		for _, gap := range startGaps(starts) {
			if gap < 40*time.Millisecond-15*time.Millisecond {
				t.Fatalf("填充期两次开始间隔 %s，小于设定的 40ms", gap)
			}
		}
	})
	t.Run("常规", func(t *testing.T) {
		ids := []string{"cached@example.com"}
		for index := range 5 {
			ids = append(ids, fmt.Sprintf("regular-%02d@example.com", index))
		}
		env := newSyncTestEnv(t, ids...)
		if err := env.gen.pool.SetCatalog("cached@example.com", aistudio.BenefitTierFree, env.catalog.data.Models()); err != nil {
			t.Fatal(err)
		}
		env.catalog.delay = 150 * time.Millisecond
		env.syncer.fillTarget = 1
		env.syncer.interval = 60 * time.Millisecond
		_, synced, _ := env.start(t)
		select {
		case <-synced:
		case <-time.After(10 * time.Second):
			t.Fatal("同步没有完成")
		}
		env.catalog.mu.Lock()
		peak, starts := env.catalog.peak, slices.Clone(env.catalog.starts)
		env.catalog.mu.Unlock()
		if peak > catalogSyncConcurrency {
			t.Fatalf("常规并发峰值 %d，超过 %d", peak, catalogSyncConcurrency)
		}
		if len(starts) != 5 {
			t.Fatalf("应同步 5 个缺失账户，实际 %d 次", len(starts))
		}
		for _, gap := range startGaps(starts) {
			if gap < 60*time.Millisecond-15*time.Millisecond {
				t.Fatalf("常规两次开始间隔 %s，小于设定的 60ms", gap)
			}
		}
	})
}

// TestCatalogSync429GlobalPause 上游 429 全局暂停 30s 起指数增长（±20%），失败账户不记个人失败；暂停期间不发请求，
// 暂停结束写“模型目录同步恢复”；连续 10 次成功后指数清零
func TestCatalogSync429GlobalPause(t *testing.T) {
	ids := make([]string, 11)
	for index := range ids {
		ids[index] = fmt.Sprintf("p%02d@example.com", index)
	}
	env := newSyncTestEnv(t, ids...)
	clock := newSyncTestClock()
	env.syncer.now = clock.Now
	env.syncer.concurrency, env.syncer.fillConcurrency = 1, 1
	limited := &aistudio.RPCError{Method: "ListModels", StatusCode: http.StatusTooManyRequests, Message: "Too Many Requests"}
	env.catalog.outcome = func(_ string, call int) error {
		if call <= 3 || call == 14 {
			return limited
		}
		return nil
	}
	env.start(t)
	pausedFor := func() time.Duration {
		env.syncer.mu.Lock()
		defer env.syncer.mu.Unlock()
		return env.syncer.pausedUntil.Sub(clock.Now())
	}
	expect := []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute}
	for round, pause := range expect {
		waitCatalog(t, fmt.Sprintf("第 %d 次 429", round+1), func() bool { return env.catalog.callCount() == round+1 })
		waitCatalog(t, "暂停生效", func() bool {
			remaining := pausedFor()
			return remaining > pause-time.Second && remaining < pause+time.Second
		})
		if !hasLogContaining(env.service.requests, fmt.Sprintf(
			"模型目录同步暂停 | 原因=上游 429 | 暂停=%s | 连续=%d | 错误=AI Studio ListModels 返回 HTTP 429", pause, round+1,
		)) {
			t.Fatalf("缺少第 %d 次暂停日志", round+1)
		}
		clock.Advance(pause - 2*time.Second)
		env.syncer.kick()
		time.Sleep(30 * time.Millisecond)
		if calls := env.catalog.callCount(); calls != round+1 {
			t.Fatalf("暂停期间不应发请求，实际 %d 次", calls)
		}
		clock.Advance(3 * time.Second)
		env.syncer.kick()
	}
	waitCatalog(t, "10 次成功", func() bool { return env.catalog.callCount() == 14 })
	if !hasLogContaining(env.service.requests, "模型目录同步恢复 | 暂停=30s") {
		t.Fatal("缺少“模型目录同步恢复”日志")
	}
	waitCatalog(t, "指数清零后的暂停", func() bool {
		remaining := pausedFor()
		return remaining > 29*time.Second && remaining < 31*time.Second
	})
	if !hasLogContaining(env.service.requests, "模型目录同步暂停 | 原因=上游 429 | 暂停=30s | 连续=1") {
		t.Fatal("连续 10 次成功后暂停应从 30 秒重新开始")
	}
	// 429 不记个人失败
	for _, accountID := range ids {
		if task, ok := env.task(accountID); ok && task.failures != 0 {
			t.Fatalf("%s 记了个人失败 %d 次", accountID, task.failures)
		}
	}
	status := env.syncer.status()
	if status.PauseReason != catalogPauseUpstream || status.PausedUntil.IsZero() || status.RateLimited1h != 4 || status.Synced1h != 10 {
		t.Fatalf("状态 %+v", status)
	}
	// 暂停时长的抖动在 ±20% 内
	env.syncer.random = func() float64 { return rand.Float64() }
	for range 50 {
		env.syncer.mu.Lock()
		env.syncer.pausedUntil, env.syncer.pauseLevel = time.Time{}, 0
		env.syncer.mu.Unlock()
		now := clock.Now()
		env.syncer.mu.Lock()
		env.syncer.rateLimitedLocked("p00@example.com", limited, now)
		env.syncer.mu.Lock()
		pause := env.syncer.pausedUntil.Sub(now)
		env.syncer.mu.Unlock()
		if pause < 24*time.Second || pause > 36*time.Second {
			t.Fatalf("第一次暂停 %s 超出 30s±20%%", pause)
		}
	}
}

// TestCatalogSyncSkipsBusyAccount 账户有活动租约时后台刷新不登记独占、不发请求，推后 2 分钟（±20%）并计 SkippedBusy1h；
// 期间租约照常发出
func TestCatalogSyncSkipsBusyAccount(t *testing.T) {
	const accountID = "busy@example.com"
	root := t.TempDir()
	catalogTestAccounts(t, root, accountID)
	transport := &catalogRPCTransport{listModels: catalogTestRaw(t, catalogTestRow("alpha", 0.7))}
	env := newSyncTestEnvWith(t, root, transport)
	gen := env.gen
	env.service.catalog = gen.pooled
	if err := gen.pool.SetCatalog(accountID, aistudio.BenefitTierFree, env.catalog.data.Models()); err != nil {
		t.Fatal(err)
	}
	env.syncer.attach(nil, catalogRestoreSummary{Stale: map[string]struct{}{accountID: {}}})
	lease, err := gen.pool.AcquireFor(context.Background(), aistudio.AccountSelection{AccountID: accountID})
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	startedAt := time.Now()
	env.start(t)
	waitCatalog(t, "推后忙碌账户", func() bool { return env.syncer.status().SkippedBusy1h == 1 })
	task, _ := env.task(accountID)
	if delay := task.due.Sub(startedAt); delay < catalogBusyRetry-time.Second || delay > catalogBusyRetry+time.Second {
		t.Fatalf("忙碌账户应推后 %s，实际 %s", catalogBusyRetry, delay)
	}
	if calls := transport.count("ListModels") + transport.count("GetAiStudioBenefitTier"); calls != 0 {
		t.Fatalf("账户忙时不应发请求，实际 %d 次", calls)
	}
	other, err := gen.pool.AcquireFor(context.Background(), aistudio.AccountSelection{AccountID: accountID})
	if err != nil {
		t.Fatalf("后台刷新不应挡住新租约: %v", err)
	}
	_ = other.Release()
}

// TestCatalogFlushOnStopBeforeNextGeneration 停止服务时调度器退出前写盘：新一代恢复的缓存包含旧一代最后一次同步
func TestCatalogFlushOnStopBeforeNextGeneration(t *testing.T) {
	const accountID = "a@example.com"
	root, dir := t.TempDir(), filepath.Join(t.TempDir(), "model-catalogs")
	catalogTestAccounts(t, root, accountID)
	env := newSyncTestEnvAt(t, root)
	cache := openCatalogCache(dir, env.service.requests)
	cache.saveInterval = time.Hour
	env.syncer.attach(cache, catalogRestoreSummary{})
	ready, _, done := env.syncer.start(env.ctx)
	if err := <-ready; err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "index.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("写盘间隔内不应已经写盘")
	}
	env.cancel()
	<-done
	next := newCatalogGeneration(t, root, nil)
	summary := openCatalogCache(dir, nil).restore(next.pool, next.client, false)
	if summary.FromDisk != 1 {
		t.Fatalf("新一代应恢复旧一代最后一次同步，恢复结果 %+v", summary)
	}
}

// TestSyncNowWithoutSyncer 测试直接构造、没有调度器的服务：syncAccountModels 直接同步并应用，不 panic
func TestSyncNowWithoutSyncer(t *testing.T) {
	env := newSyncTestEnv(t, "a@example.com")
	env.service.catalogSync = nil
	models, err := env.service.syncAccountModels(context.Background(), "a@example.com")
	if err != nil || len(models) != 1 || env.catalog.callCount() != 1 {
		t.Fatalf("models=%v err=%v 调用 %d 次", models, err, env.catalog.callCount())
	}
	if len(env.service.modelSnapshot()) != 1 {
		t.Fatal("同步后应应用目录")
	}
	env.service.removeAccountModelRetry("a@example.com")
	var syncer *catalogSyncer
	if syncer.status() != nil || syncer.plan() != (catalogPlan{}) {
		t.Fatal("nil 调度器应返回零值")
	}
	syncer.noteAuthRequired("a@example.com", time.Now())
	syncer.noteAuthValid("a@example.com")
	syncer.attach(nil, catalogRestoreSummary{})
}

// TestCatalogNoteAuthValidClearsSuspect 管理验证通过（noteAuthValid）即移出嫌疑、清掉缓存里的需要登录记录，
// 不等随后的目录同步成功；落盘后下一代运行时不再把它列为疑似失效
func TestCatalogNoteAuthValidClearsSuspect(t *testing.T) {
	const accountID = "suspect@example.com"
	root, dir := t.TempDir(), filepath.Join(t.TempDir(), "model-catalogs")
	catalogTestAccounts(t, root, accountID)
	now := time.Now()
	first := syncTestCacheFor(t, root, dir, catalogTestData(t, "alpha"), map[string]time.Time{accountID: now})
	cache := openCatalogCache(dir, nil)
	account, err := first.pool.Account(accountID)
	if err != nil {
		t.Fatal(err)
	}
	modTime, _ := fileModTime(account.StoragePath)
	cache.noteAuthRequired(accountID, now, modTime)
	if err := cache.flush(); err != nil {
		t.Fatal(err)
	}

	env := newSyncTestEnvAt(t, root)
	env.service.workers = &accountWorkerManager{}
	if summary := env.restoreFrom(dir); len(summary.Suspects) != 1 {
		t.Fatalf("疑似失效 %v，期望 %s", summary.Suspects, accountID)
	}
	env.syncer.noteAuthValid(accountID)
	if env.syncer.status().AuthSuspects != 0 {
		t.Fatal("验证通过后应移出嫌疑")
	}
	if suspects := env.service.workers.authSuspects.Load(); suspects != nil && len(*suspects) != 0 {
		t.Fatalf("Worker 管理器的疑似失效 %v，应为空", *suspects)
	}
	if entry, _ := env.syncer.cache.entryOf(accountID); entry.AuthRequiredAt != 0 || entry.AuthModTime != 0 {
		t.Fatal("验证通过后应清掉缓存里的需要登录记录")
	}
	if env.catalog.callCount() != 0 {
		t.Fatal("noteAuthValid 不应发目录请求")
	}
	if err := env.syncer.cache.flush(); err != nil {
		t.Fatal(err)
	}
	next := newSyncTestEnvAt(t, root)
	if summary := next.restoreFrom(dir); len(summary.Suspects) != 0 {
		t.Fatalf("落盘后下一代仍列为疑似失效：%v", summary.Suspects)
	}
}

// TestCatalogConfirmedSuspectDropped 疑似失效的账户确认需要登录后移出“疑似失效”：经 noteAuthRequired（登录复核确认、管理验证等）
// 时立即移出；aistudio 内部标记、没有经 app 的，对照账户池或调度时发现它需要登录同样移出（原先确认失效的一直算在“疑似失效”里）
func TestCatalogConfirmedSuspectDropped(t *testing.T) {
	ids := []string{"confirmed@example.com", "internal@example.com"}
	root, dir := t.TempDir(), filepath.Join(t.TempDir(), "model-catalogs")
	catalogTestAccounts(t, root, ids...)
	now := time.Now()
	first := syncTestCacheFor(t, root, dir, catalogTestData(t, "alpha"), map[string]time.Time{ids[0]: now, ids[1]: now})
	cache := openCatalogCache(dir, nil)
	for _, accountID := range ids {
		account, err := first.pool.Account(accountID)
		if err != nil {
			t.Fatal(err)
		}
		modTime, _ := fileModTime(account.StoragePath)
		cache.noteAuthRequired(accountID, now, modTime)
	}
	if err := cache.flush(); err != nil {
		t.Fatal(err)
	}
	env := newSyncTestEnvAt(t, root)
	env.service.workers = &accountWorkerManager{}
	if summary := env.restoreFrom(dir); len(summary.Suspects) != 2 {
		t.Fatalf("疑似失效 %v，期望两个", summary.Suspects)
	}
	env.syncer.noteAuthRequired(ids[0], time.Now())
	if got := env.syncer.status().AuthSuspects; got != 1 {
		t.Fatalf("确认需要登录后疑似失效 = %d，期望 1", got)
	}
	if suspects := env.service.workers.authSuspects.Load(); suspects == nil || len(*suspects) != 1 {
		t.Fatalf("Worker 管理器的疑似失效应只剩一个：%v", suspects)
	}
	if entry, _ := env.syncer.cache.entryOf(ids[0]); entry.AuthRequiredAt == 0 {
		t.Fatal("确认需要登录仍应记进缓存")
	}
	if err := env.gen.pool.MarkAuthRequired(ids[1], "HTTP 401"); err != nil {
		t.Fatal(err)
	}
	env.start(t)
	waitCatalog(t, "调度时发现需要登录、移出疑似失效", func() bool { return env.syncer.status().AuthSuspects == 0 })
	if calls := env.catalog.callsFor(ids[1]); calls != 0 {
		t.Fatalf("需要登录的账户不应同步，实际 %d 次", calls)
	}
}

// TestAuthSuspectsPrewarmLastAndProbe 缓存记过需要登录、认证文件之后没变过的账户列为疑似失效：预热排最后、
// 优先探测，探测成功后清掉嫌疑；认证文件变过的不列嫌疑并清掉记录
func TestAuthSuspectsPrewarmLastAndProbe(t *testing.T) {
	ids := []string{"suspect@example.com", "relogin@example.com", "fine@example.com"}
	root, dir := t.TempDir(), filepath.Join(t.TempDir(), "model-catalogs")
	catalogTestAccounts(t, root, ids...)
	now := time.Now()
	first := syncTestCacheFor(t, root, dir, catalogTestData(t, "alpha"), map[string]time.Time{ids[0]: now, ids[1]: now, ids[2]: now})
	cache := openCatalogCache(dir, nil)
	for _, accountID := range ids[:2] {
		account, err := first.pool.Account(accountID)
		if err != nil {
			t.Fatal(err)
		}
		modTime, _ := fileModTime(account.StoragePath)
		cache.noteAuthRequired(accountID, now, modTime)
	}
	if err := cache.flush(); err != nil {
		t.Fatal(err)
	}
	relogin, _ := first.pool.Account(ids[1])
	later := time.Now().Add(time.Minute)
	if err := os.Chtimes(relogin.StoragePath, later, later); err != nil {
		t.Fatal(err)
	}

	env := newSyncTestEnvAt(t, root)
	env.service.workers = &accountWorkerManager{}
	summary := env.restoreFrom(dir)
	if len(summary.Suspects) != 1 {
		t.Fatalf("疑似失效 %v，期望只有 %s", summary.Suspects, ids[0])
	}
	if entry, _ := env.syncer.cache.entryOf(ids[1]); entry.AuthRequiredAt != 0 || entry.AuthModTime != 0 {
		t.Fatal("认证文件变过的账户应清掉需要登录记录")
	}
	if order := env.service.workers.deferAuthSuspects(slices.Clone(ids)); !slices.Equal(order, []string{ids[1], ids[2], ids[0]}) {
		t.Fatalf("预热顺序 %v，疑似失效应排最后", order)
	}
	if plan := env.syncer.plan(); plan.suspects != 1 {
		t.Fatalf("启动计划 %+v", plan)
	}
	if task, _ := env.task(ids[0]); task.priority != catalogPriorityAuthProbe {
		t.Fatalf("疑似失效账户应按探测优先级排队，实际 %d", task.priority)
	}
	env.start(t)
	waitCatalog(t, "探测疑似失效账户", func() bool { return env.catalog.callCount() == 1 })
	waitCatalog(t, "探测成功清掉嫌疑", func() bool { return env.syncer.status().AuthSuspects == 0 })
	if env.catalog.callsFor(ids[0]) != 1 {
		t.Fatal("应先探测疑似失效账户")
	}
	if order := env.service.workers.deferAuthSuspects(slices.Clone(ids)); !slices.Equal(order, ids) {
		t.Fatal("嫌疑清掉后预热顺序应不变")
	}
	waitCatalog(t, "探测成功清掉缓存里的需要登录记录", func() bool {
		entry, _ := env.syncer.cache.entryOf(ids[0])
		return entry.AuthRequiredAt == 0 && entry.At > 0
	})
	time.Sleep(30 * time.Millisecond)
	if calls := env.catalog.callCount(); calls != 1 {
		t.Fatalf("没有过期的账户不应同步，实际 %d 次", calls)
	}
}

// TestCatalogSyncPerAccountBackoff 其他失败按账户退避 1m×2^(f-1)，上限 6 小时；失败日志只在 f=1、2、4、8 时写
func TestCatalogSyncPerAccountBackoff(t *testing.T) {
	const accountID = "a@example.com"
	env := newSyncTestEnv(t, accountID)
	clock := newSyncTestClock()
	env.syncer.now = clock.Now
	env.catalog.outcome = func(string, int) error { return errors.New("上游 500") }
	env.start(t)
	for failures := 1; failures <= 10; failures++ {
		waitCatalog(t, fmt.Sprintf("第 %d 次失败", failures), func() bool {
			task, _ := env.task(accountID)
			return task.failures == failures && !task.running
		})
		task, _ := env.task(accountID)
		want := min(catalogRetryBase<<(failures-1), catalogRetryMax)
		if delay := task.due.Sub(clock.Now()); delay > want+time.Second || delay < want-time.Second {
			t.Fatalf("第 %d 次失败后退避 %s，期望 %s", failures, delay, want)
		}
		clock.Advance(want + time.Second)
		env.syncer.kick()
	}
	if count := logCount(env.service.requests, "模型目录同步失败 | 错误=上游 500 | 下次="); count != 4 {
		t.Fatalf("失败日志 %d 条，期望 4 条（f=1、2、4、8）", count)
	}
	if !hasLogContaining(env.service.requests, "模型目录同步失败 | 错误=上游 500 | 下次=1m0s | 连续失败=1") ||
		!hasLogContaining(env.service.requests, "模型目录同步失败 | 错误=上游 500 | 下次=2h8m0s | 连续失败=8") {
		t.Fatal("失败日志格式不符")
	}
	if entry, ok := env.syncer.cache.entryOf(accountID); ok || entry.Failures != 0 {
		t.Fatal("没有缓存时不写记录")
	}
}

// TestCatalogSyncEgressPaused 出口限流时该出口上的账户推后 30 秒、不发请求，状态原因为“出口限流”；管理操作照常同步
func TestCatalogSyncEgressPaused(t *testing.T) {
	const accountID = "a@example.com"
	env := newSyncTestEnv(t, accountID)
	var paused atomic.Bool
	paused.Store(true)
	env.syncer.egressPaused = func(string) bool { return paused.Load() }
	startedAt := time.Now()
	env.start(t)
	waitCatalog(t, "出口限流推后", func() bool { return env.syncer.status().PauseReason == catalogPauseEgress })
	task, _ := env.task(accountID)
	if delay := task.due.Sub(startedAt); delay < catalogEgressRetry-time.Second || delay > catalogEgressRetry+time.Second {
		t.Fatalf("出口限流应推后 %s，实际 %s", catalogEgressRetry, delay)
	}
	if calls := env.catalog.callCount(); calls != 0 {
		t.Fatalf("出口限流时后台不应同步，实际 %d 次", calls)
	}
	models, err := env.syncer.syncNow(context.Background(), accountID)
	if err != nil || len(models) != 1 || env.catalog.callCount() != 1 {
		t.Fatalf("管理操作应照常同步，models=%v err=%v", models, err)
	}
}

// TestCatalogSyncExpiredRefresh 上次成功同步超过 24 小时（±10%）的账户后台刷新，没有过期的不刷新；刷新后下次到期为 24 小时后
func TestCatalogSyncExpiredRefresh(t *testing.T) {
	ids := []string{"expired@example.com", "recent@example.com"}
	root, dir := t.TempDir(), filepath.Join(t.TempDir(), "model-catalogs")
	catalogTestAccounts(t, root, ids...)
	now := time.Now()
	syncTestCacheFor(t, root, dir, catalogTestData(t, "alpha"), map[string]time.Time{
		ids[0]: now.Add(-25 * time.Hour), ids[1]: now.Add(-time.Hour),
	})
	env := newSyncTestEnvAt(t, root)
	env.restoreFrom(dir)
	if plan := env.syncer.plan(); plan.expired != 1 || plan.missing != 0 {
		t.Fatalf("启动计划 %+v", plan)
	}
	env.start(t)
	waitCatalog(t, "刷新过期账户", func() bool { return env.catalog.callsFor(ids[0]) == 1 })
	waitCatalog(t, "排下次过期刷新", func() bool {
		task, _ := env.task(ids[0])
		return !task.running && task.due.After(now.Add(catalogTTL-time.Minute))
	})
	time.Sleep(30 * time.Millisecond)
	if calls := env.catalog.callsFor(ids[1]); calls != 0 {
		t.Fatalf("没有过期的账户不应刷新，实际 %d 次", calls)
	}
	if entry, _ := env.syncer.cache.entryOf(ids[0]); time.Since(time.Unix(entry.At, 0)) > time.Minute {
		t.Fatal("刷新后缓存的同步时间应更新")
	}
}

// TestCatalogSyncAuthRequiredSkipped 需要登录的账户不进队列；同步返回 401 的账户移出队列、不再重试
func TestCatalogSyncAuthRequiredSkipped(t *testing.T) {
	env := newSyncTestEnv(t, "required@example.com", "expired-login@example.com")
	if err := env.gen.pool.MarkAuthRequired("required@example.com", "登录态失效"); err != nil {
		t.Fatal(err)
	}
	env.catalog.outcome = func(accountID string, _ int) error {
		if accountID == "expired-login@example.com" {
			return &aistudio.RPCError{Method: "ListModels", StatusCode: http.StatusUnauthorized, Code: 16, Message: "unauthenticated"}
		}
		return nil
	}
	_, synced, _ := env.start(t)
	<-synced
	time.Sleep(30 * time.Millisecond)
	if calls := env.catalog.callsFor("required@example.com"); calls != 0 {
		t.Fatalf("需要登录的账户不应同步，实际 %d 次", calls)
	}
	if calls := env.catalog.callsFor("expired-login@example.com"); calls != 1 {
		t.Fatalf("401 的账户应只同步一次，实际 %d 次", calls)
	}
	for _, accountID := range []string{"required@example.com", "expired-login@example.com"} {
		if _, exists := env.task(accountID); exists {
			t.Fatalf("%s 不应留在队列里", accountID)
		}
	}
}

// TestCatalogSyncWritesCacheAndFlush 后台同步成功后写缓存，写盘间隔到了落盘
func TestCatalogSyncWritesCacheAndFlush(t *testing.T) {
	const accountID = "a@example.com"
	dir := filepath.Join(t.TempDir(), "model-catalogs")
	env := newSyncTestEnv(t, accountID)
	cache := openCatalogCache(dir, env.service.requests)
	cache.saveInterval = 20 * time.Millisecond
	env.syncer.attach(cache, catalogRestoreSummary{})
	env.start(t)
	waitCatalog(t, "写缓存", func() bool {
		entry, ok := cache.entryOf(accountID)
		return ok && entry.Playground == env.catalog.data.ContentKey() && entry.At > 0
	})
	waitCatalog(t, "落盘", func() bool {
		data, err := os.ReadFile(filepath.Join(dir, "index.json"))
		return err == nil && strings.Contains(string(data), accountID)
	})
	if _, err := os.Stat(cache.blobPath(env.catalog.data.ContentKey())); err != nil {
		t.Fatalf("目录 blob 应落盘: %v", err)
	}
}

// TestCatalogSyncConcurrentSyncNow 调度器与管理操作对同一批账户并发同步：不死锁、计数归零，每个账户只有一个任务且堆一致
func TestCatalogSyncConcurrentSyncNow(t *testing.T) {
	ids := make([]string, 20)
	for index := range ids {
		ids[index] = fmt.Sprintf("c%02d@example.com", index)
	}
	env := newSyncTestEnv(t, ids...)
	env.catalog.delay = 2 * time.Millisecond
	env.syncer.ttl = 20 * time.Millisecond
	dir := filepath.Join(t.TempDir(), "model-catalogs")
	cache := openCatalogCache(dir, env.service.requests)
	cache.saveInterval = 5 * time.Millisecond
	env.syncer.attach(cache, catalogRestoreSummary{})
	_, _, done := env.syncer.start(env.ctx)
	var callers sync.WaitGroup
	for caller := range 8 {
		callers.Add(1)
		go func() {
			defer callers.Done()
			for round := range 20 {
				accountID := ids[(caller*7+round)%len(ids)]
				if _, err := env.syncer.syncNow(context.Background(), accountID); err != nil {
					t.Error(err)
				}
				_ = env.syncer.status()
			}
		}()
	}
	callers.Wait()
	env.cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("调度器没有退出（死锁？）")
	}
	env.syncer.mu.Lock()
	defer env.syncer.mu.Unlock()
	if env.syncer.running != 0 {
		t.Fatalf("运行中计数 %d，期望 0", env.syncer.running)
	}
	if len(env.syncer.tasks) != len(ids) {
		t.Fatalf("任务 %d 个，期望每个账户一个", len(env.syncer.tasks))
	}
	queued := 0
	for priority := range env.syncer.heaps {
		for index, task := range env.syncer.heaps[priority] {
			if task.index != index || task.priority != catalogPriority(priority) || env.syncer.tasks[task.accountID] != task || task.running {
				t.Fatalf("堆不一致：%+v 在 %d/%d", *task, priority, index)
			}
			queued++
		}
	}
	if queued != len(ids) {
		t.Fatalf("堆里 %d 个任务，期望 %d 个", queued, len(ids))
	}
}

// TestCatalogAdminSyncNoPerAccountApply 管理操作的立即同步（批量启用、导入）不逐个账户合并应用全量目录，由调用方整批应用一次
func TestCatalogAdminSyncNoPerAccountApply(t *testing.T) {
	ids := []string{"a@example.com", "b@example.com", "c@example.com"}
	env := newSyncTestEnv(t, ids...)
	for _, accountID := range ids {
		if models, err := env.service.refreshAccountModelCatalog(context.Background(), accountID); err != nil || len(models) != 1 {
			t.Fatalf("models=%v err=%v", models, err)
		}
	}
	if calls := env.catalog.cachedCalls.Load(); calls != 0 {
		t.Fatalf("管理操作的同步不应逐个账户合并应用目录，实际 %d 次", calls)
	}
	for _, accountID := range ids {
		if task, ok := env.task(accountID); !ok || task.priority != catalogPriorityExpired {
			t.Fatalf("%s 同步后应排下次过期刷新：%+v", accountID, task)
		}
	}
}

// TestCatalogStartBackoffMissingNotInRound 重启前同步失败、还在退避中的缺失账户（缓存记的下次重试在 3 小时后）：
// 有目录时不算进第一轮，synced 立即关闭、不提前同步它，任务仍按退避到期；一个目录都没有恢复出来时重启当作一次重试，
// 立即同步，ready 等它的结果（原先第一轮要等退避到期，Start 卡在等 ready 最长 6 小时）
func TestCatalogStartBackoffMissingNotInRound(t *testing.T) {
	writeBackoff := func(t *testing.T, dir string, accountID string, next time.Time) {
		t.Helper()
		cache := openCatalogCache(dir, nil)
		cache.noteFailure(accountID, 3, next)
		if err := cache.flush(); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("有目录", func(t *testing.T) {
		ids := []string{"a@example.com", "b@example.com"}
		root, dir := t.TempDir(), filepath.Join(t.TempDir(), "model-catalogs")
		catalogTestAccounts(t, root, ids...)
		syncTestCacheFor(t, root, dir, catalogTestData(t, "alpha"), map[string]time.Time{ids[0]: time.Now()})
		next := time.Now().Add(3 * time.Hour)
		writeBackoff(t, dir, ids[1], next)
		env := newSyncTestEnvAt(t, root)
		env.restoreFrom(dir)
		ready, synced, _ := env.start(t)
		select {
		case err := <-ready:
			if err != nil {
				t.Fatal(err)
			}
		default:
			t.Fatal("有目录时 ready 应立即送出")
		}
		select {
		case <-synced:
		default:
			t.Fatal("退避中的缺失任务不应算进第一轮，synced 应立即关闭")
		}
		time.Sleep(50 * time.Millisecond)
		if calls := env.catalog.callsFor(ids[1]); calls != 0 {
			t.Fatalf("退避中的缺失账户被提前同步了 %d 次", calls)
		}
		if task, ok := env.task(ids[1]); !ok || task.priority != catalogPriorityMissing || task.due.Before(next.Add(-time.Second)) {
			t.Fatalf("任务 %+v ok=%v，期望仍为缺失、按退避到期（%s）", task, ok, next)
		}
	})
	t.Run("没有目录", func(t *testing.T) {
		root, dir := t.TempDir(), filepath.Join(t.TempDir(), "model-catalogs")
		catalogTestAccounts(t, root, "b@example.com")
		writeBackoff(t, dir, "b@example.com", time.Now().Add(3*time.Hour))
		env := newSyncTestEnvAt(t, root)
		env.restoreFrom(dir)
		ready, synced, _ := env.start(t)
		select {
		case err := <-ready:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("没有目录时应立即重试退避中的缺失账户，ready 等到它的结果")
		}
		<-synced
		if calls := env.catalog.callsFor("b@example.com"); calls != 1 {
			t.Fatalf("同步了 %d 次，期望重启后立即重试 1 次", calls)
		}
	})
}
