package app

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/camoufoxnative"
)

// pageNotReadyText 为 INIT_TIMEOUT 先于页面等待到期时 Camoufox 的原样错误（读不到页面地址）：机器慢或号的问题都可能
func pageNotReadyError() error {
	return fmt.Errorf("AI Studio 输入框未就绪 url=%s: %w", "", context.DeadlineExceeded)
}

// nativeLoginRedirectError 返回 Camoufox 启动时被跳转到 Google 登录页的错误，与 aistudio.NewNativeWorker 返回的一样（原样文本包装登录失效）
func nativeLoginRedirectError() error {
	raw := errors.New("隔离登录态失效 url=https://accounts.google.com/v3/signin/identifier?continue=https%3A%2F%2Faistudio.google.com%2F")
	return fmt.Errorf("%w: %w", aistudio.ErrAuthenticationRequired, raw)
}

// reportStagesUpTo 按 Camoufox 的顺序报告启动阶段，直到 last
func reportStagesUpTo(options camoufoxnative.Options, last camoufoxnative.StartupStage) {
	if options.StartupProgress == nil {
		return
	}
	for _, stage := range []camoufoxnative.StartupStage{
		camoufoxnative.StartupPreparingBrowser, camoufoxnative.StartupLaunchingBrowser, camoufoxnative.StartupConnectingBiDi,
		camoufoxnative.StartupLoadingAIStudio, camoufoxnative.StartupLocatingWAA, camoufoxnative.StartupBootstrappingWAA,
	} {
		options.StartupProgress(stage)
		if stage == last {
			return
		}
	}
}

// startupTestLaunch 记录每个账户的启动次数，按 fail 返回启动错误（为空的账户启动成功）
type startupTestLaunch struct {
	mu     sync.Mutex
	counts map[string]int
	fail   map[string]func(camoufoxnative.Options) error
	delay  time.Duration
	// gate 不为空时启动要等 gate 关闭（或 ctx 结束）
	gate chan struct{}
}

func newStartupTestLaunch() *startupTestLaunch {
	return &startupTestLaunch{counts: map[string]int{}, fail: map[string]func(camoufoxnative.Options) error{}}
}

func (launch *startupTestLaunch) setFailure(accountID string, failure func(camoufoxnative.Options) error) {
	launch.mu.Lock()
	defer launch.mu.Unlock()
	if failure == nil {
		delete(launch.fail, accountID)
		return
	}
	launch.fail[accountID] = failure
}

func (launch *startupTestLaunch) count(accountID string) int {
	launch.mu.Lock()
	defer launch.mu.Unlock()
	return launch.counts[accountID]
}

func (launch *startupTestLaunch) launch(ctx context.Context, accountID string, options camoufoxnative.Options) (*aistudio.NativeWorker, error) {
	launch.mu.Lock()
	launch.counts[accountID]++
	failure := launch.fail[accountID]
	gate := launch.gate
	launch.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if launch.delay > 0 {
		select {
		case <-time.After(launch.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if failure != nil {
		return nil, failure(options)
	}
	reportStagesUpTo(options, camoufoxnative.StartupBootstrappingWAA)
	worker, _ := aistudio.NewStubWorker(accountID, 0)
	return worker, nil
}

// failAtPageLoad 在载入 AI Studio（第 5 阶段）后以输入框未就绪失败
func failAtPageLoad(options camoufoxnative.Options) error {
	reportStagesUpTo(options, camoufoxnative.StartupLoadingAIStudio)
	return pageNotReadyError()
}

// startupTestService 返回纯 Go 后端（启动替换为 launch）的生成服务，常驻 1 个、峰值 maxActive 个 Worker，带内存中的失效账户持久表
func startupTestService(t *testing.T, launch *startupTestLaunch, maxActive int, accountIDs ...string) *trackedService {
	t.Helper()
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	pool, accounts := testAccountPool(t, accountIDs...)
	pool.SetPerAccountConcurrency(1)
	requests := newRequestRegistry(ctx)
	manager := newAccountWorkerManager(pool, accounts, requests, "", "", time.Minute, 1, maxActive, 2, false)
	manager.launch = launch.launch
	manager.startupFailures = newStartupFailureRegistry("", requests)
	t.Cleanup(func() { _ = manager.Close() })
	service := &trackedService{
		lifecycle: ctx, pool: pool, requests: requests, workers: manager,
		forbidden: newForbiddenTracker(), quota: newQuotaSharing("", requests),
	}
	service.timeout.Store(int64(time.Minute))
	return service
}

// startupPauseOf 返回账户的启动失败暂停到期时间（没有时为零）与计入退避的连续失败次数
func startupPauseOf(t *testing.T, manager *accountWorkerManager, accountID string) (time.Time, int) {
	t.Helper()
	manager.mu.RLock()
	account := manager.accounts[accountID]
	manager.mu.RUnlock()
	if account == nil {
		t.Fatalf("账户 %s 没有 Worker 配置", accountID)
	}
	account.mu.Lock()
	defer account.mu.Unlock()
	until := account.startFailUntil.Load()
	if until == 0 {
		return time.Time{}, account.startFailCount
	}
	return time.Unix(0, until), account.startFailCount
}

// expectPause 断言账户的暂停约为 delay（允许 5 秒误差）
func expectPause(t *testing.T, manager *accountWorkerManager, accountID string, delay time.Duration, count int) {
	t.Helper()
	until, got := startupPauseOf(t, manager, accountID)
	remaining := time.Until(until)
	if remaining > delay || remaining < delay-5*time.Second || got != count {
		t.Fatalf("暂停剩余 %s、计数 %d，期望约 %s、计数 %d", remaining.Round(time.Second), got, delay, count)
	}
}

// pageNotReadyFailure 返回在第 5 阶段失败的启动错误（与 startReservedWorker 返回的一样）
func pageNotReadyFailure() error {
	return &accountWorkerInitError{err: pageNotReadyError(), stage: 5}
}

// TestStartupFailurePausesFailingStandby 复现 P1：a 热且忙，b 启动必失败（假启动 20ms，载入页面后输入框未就绪），c 健康。
// 原先一个排队的请求在 2 秒内把 b 启动了 97 次，c 一次没启动；现在 b 只启动 1 次、暂停 2 分钟（账户仍为就绪），c 被扩容到就绪并取得租约
func TestStartupFailurePausesFailingStandby(t *testing.T) {
	const a, b, c = "p1-a@example.com", "p1-b@example.com", "p1-c@example.com"
	launch := newStartupTestLaunch()
	launch.delay = 20 * time.Millisecond
	launch.setFailure(b, failAtPageLoad)
	service := startupTestService(t, launch, 3, a, b, c)
	manager := service.workers
	ctx, cancel := context.WithTimeout(service.lifecycle, 10*time.Second)
	defer cancel()
	if _, err := manager.Worker(ctx, a, testRuntimeModel); err != nil {
		t.Fatal(err)
	}
	release := holdLease(t, service.pool, a)
	defer release()

	requestCtx, requestCancel := context.WithTimeout(ctx, 2*time.Second)
	defer requestCancel()
	lease, err := service.acquireWarmLease(requestCtx, aistudio.AccountSelection{ModelID: testRuntimeModel, Method: "generateContent"})
	if err != nil {
		t.Fatalf("健康的 c 应被扩容并取得租约，err=%v，b 启动 %d 次、c 启动 %d 次", err, launch.count(b), launch.count(c))
	}
	defer func() { _ = lease.Release() }()
	if lease.Account().ID != c {
		t.Fatalf("取得 %s，期望 c", lease.Account().ID)
	}
	time.Sleep(200 * time.Millisecond)
	if got := launch.count(b); got != 1 {
		t.Fatalf("失败的 b 启动了 %d 次，期望 1 次", got)
	}
	if got := launch.count(c); got != 1 {
		t.Fatalf("c 启动了 %d 次，期望 1 次", got)
	}
	if status := testStatus(t, service.pool, b); status.State != aistudio.AccountReady {
		t.Fatalf("原因不确定的失败不应改变账户状态，b = %s", status.State)
	}
	expectPause(t, manager, b, 2*time.Minute, 1)
	if !hasLogContaining(service.requests, "WAA Worker 启动失败计入账户", "类别=原因不确定", "阶段=5/7", "连续失败=1", "暂停调度=2m0s") {
		t.Fatalf("应记录计入账户的日志，日志 = %v", serviceLogMessages(service.requests))
	}
	if !hasLogContaining(service.requests, "WAA Worker 启动失败", "阶段=5/7", "类别=原因不确定", "排队=", "启动=") {
		t.Fatal("启动失败日志应带阶段、类别、排队与启动时长")
	}
}

// TestAmbiguousStartupPauseEscalates 原因不确定的连续失败依次暂停 2m、10m、30m，30 分钟封顶；启动成功后清零
func TestAmbiguousStartupPauseEscalates(t *testing.T) {
	const b = "esc-b@example.com"
	launch := newStartupTestLaunch()
	service := startupTestService(t, launch, 2, b)
	manager := service.workers
	ctx := service.lifecycle
	manager.lastStartupSuccess.Store(time.Now().UnixNano())
	for index, delay := range []time.Duration{2 * time.Minute, 10 * time.Minute, 30 * time.Minute, 30 * time.Minute} {
		manager.noteStartupFailure(ctx, b, service.pool.AuthGeneration(b), time.Now(), pageNotReadyFailure())
		expectPause(t, manager, b, delay, index+1)
	}
	if status := testStatus(t, service.pool, b); status.State != aistudio.AccountReady {
		t.Fatalf("账户状态不应改变，实际 %s", status.State)
	}
	if _, err := manager.Worker(ctx, b, testRuntimeModel); err != nil {
		t.Fatal(err)
	}
	if until, count := startupPauseOf(t, manager, b); !until.IsZero() || count != 0 {
		t.Fatalf("启动成功后应清零，暂停至 %s、计数 %d", until, count)
	}
	if !hasLogContaining(service.requests, "WAA Worker 就绪", "排队=", "启动=") {
		t.Fatal("就绪日志应带排队与启动时长")
	}
}

// TestAmbiguousStartupPauseSystemic 近 5 分钟没有任何 Worker 启动成功（含服务刚启动）时，原因不确定的失败按系统性问题只暂停 1 分钟、不计次数
func TestAmbiguousStartupPauseSystemic(t *testing.T) {
	const b = "sys-b@example.com"
	service := startupTestService(t, newStartupTestLaunch(), 2, b)
	manager := service.workers
	for range 2 {
		manager.noteStartupFailure(service.lifecycle, b, service.pool.AuthGeneration(b), time.Now(), pageNotReadyFailure())
		expectPause(t, manager, b, time.Minute, 0)
	}
	manager.lastStartupSuccess.Store(time.Now().Add(-6 * time.Minute).UnixNano())
	manager.noteStartupFailure(service.lifecycle, b, service.pool.AuthGeneration(b), time.Now(), pageNotReadyFailure())
	expectPause(t, manager, b, time.Minute, 0)
	if !hasLogContaining(service.requests, "近 5 分钟没有任何 Worker 启动成功", "暂停调度=1m0s") {
		t.Fatalf("应写明按系统性问题处理，日志 = %v", serviceLogMessages(service.requests))
	}
}

// TestPinnedAccountIgnoresStartupPause 固定账户（指定账户）的请求在暂停期内仍会启动该账户
func TestPinnedAccountIgnoresStartupPause(t *testing.T) {
	const b = "pin-b@example.com"
	launch := newStartupTestLaunch()
	service := startupTestService(t, launch, 2, b)
	manager := service.workers
	manager.lastStartupSuccess.Store(time.Now().UnixNano())
	manager.noteStartupFailure(service.lifecycle, b, service.pool.AuthGeneration(b), time.Now(), pageNotReadyFailure())
	expectPause(t, manager, b, 2*time.Minute, 1)
	ctx, cancel := context.WithTimeout(service.lifecycle, 5*time.Second)
	defer cancel()
	lease, err := service.acquireWarmLease(ctx, aistudio.AccountSelection{ModelID: testRuntimeModel, Method: "generateContent", AccountID: b})
	if err != nil {
		t.Fatalf("固定账户不受启动失败暂停影响，err=%v", err)
	}
	_ = lease.Release()
	if launch.count(b) != 1 {
		t.Fatalf("固定账户应现场启动，启动 %d 次", launch.count(b))
	}
}

// TestWorkerIgnoresStartupPause Worker()（受保护传输现场重建 Worker）在暂停期内仍会启动该账户
func TestWorkerIgnoresStartupPause(t *testing.T) {
	const b = "worker-b@example.com"
	launch := newStartupTestLaunch()
	service := startupTestService(t, launch, 2, b)
	manager := service.workers
	manager.lastStartupSuccess.Store(time.Now().UnixNano())
	manager.noteStartupFailure(service.lifecycle, b, service.pool.AuthGeneration(b), time.Now(), pageNotReadyFailure())
	if _, err := manager.Worker(service.lifecycle, b, testRuntimeModel); err != nil {
		t.Fatalf("Worker() 不受启动失败暂停影响，err=%v", err)
	}
	if launch.count(b) != 1 {
		t.Fatalf("Worker() 应启动账户，启动 %d 次", launch.count(b))
	}
}

// TestStartupFailureSkippedAfterStorageSaved 启动途中保存了新的认证文件（SaveStorageState 推进认证代际）：旧凭据的失败不记到新凭据头上
func TestStartupFailureSkippedAfterStorageSaved(t *testing.T) {
	const b = "saved-b@example.com"
	launch := newStartupTestLaunch()
	launch.gate = make(chan struct{})
	launch.setFailure(b, failAtPageLoad)
	service := startupTestService(t, launch, 2, b)
	manager := service.workers
	manager.lastStartupSuccess.Store(time.Now().UnixNano())
	done := make(chan error, 1)
	go func() {
		_, err := manager.Worker(service.lifecycle, b, testRuntimeModel)
		done <- err
	}()
	waitFor(t, "b 开始启动", func() bool { return launch.count(b) == 1 })
	lease, err := service.pool.AcquireAccount(context.Background(), b)
	if err != nil {
		t.Fatal(err)
	}
	if err := lease.SaveStorageState(lease.Account().StorageState); err != nil {
		t.Fatal(err)
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
	close(launch.gate)
	if err := <-done; err == nil {
		t.Fatal("b 应启动失败")
	}
	if until, count := startupPauseOf(t, manager, b); !until.IsZero() || count != 0 {
		t.Fatalf("认证代际已变化的失败不应记录，暂停至 %s、计数 %d", until, count)
	}
	if hasLogContaining(service.requests, "WAA Worker 启动失败计入账户") {
		t.Fatal("不应写计入账户的日志")
	}
}

// TestStartupProbeOncePerMinute 候选都在暂停、没有热 Worker 时，全局每分钟只提前放行 1 个最早到期的账户
func TestStartupProbeOncePerMinute(t *testing.T) {
	ids := []string{"probe-a@example.com", "probe-b@example.com", "probe-c@example.com"}
	launch := newStartupTestLaunch()
	for _, accountID := range ids {
		launch.setFailure(accountID, failAtPageLoad)
	}
	service := startupTestService(t, launch, 3, ids...)
	manager := service.workers
	manager.lastStartupSuccess.Store(time.Now().UnixNano())
	for _, accountID := range ids {
		manager.noteStartupFailure(service.lifecycle, accountID, service.pool.AuthGeneration(accountID), time.Now(), pageNotReadyFailure())
		time.Sleep(2 * time.Millisecond)
	}
	if got := manager.startupBackoffAvailable(ids, false); len(got) != 0 {
		t.Fatalf("不允许试探时应全部过滤，实际 %v", got)
	}
	if got := manager.startupBackoffAvailable(ids, true); len(got) != 1 || got[0] != ids[0] {
		t.Fatalf("第一次应放行最早到期的 %s，实际 %v", ids[0], got)
	}
	if got := manager.startupBackoffAvailable(ids, true); len(got) != 0 {
		t.Fatalf("一分钟内不应再放行，实际 %v", got)
	}
	manager.lastProbe.Store(time.Now().Add(-startupProbeInterval - time.Second).UnixNano())
	if got := manager.startupBackoffAvailable(ids, true); len(got) != 1 {
		t.Fatalf("满一分钟后应再放行一个，实际 %v", got)
	}

	// 选号：没有热 Worker、候选都在暂停中，排队期间只试探启动 1 个（它再次失败后继续暂停），其余的不启动
	manager.lastProbe.Store(0)
	ctx, cancel := context.WithTimeout(service.lifecycle, 1500*time.Millisecond)
	defer cancel()
	lease, err := service.acquireWarmLease(ctx, aistudio.AccountSelection{ModelID: testRuntimeModel, Method: "generateContent"})
	if lease != nil || err == nil {
		t.Fatalf("候选都在暂停中时不应取得租约，lease=%v err=%v", lease, err)
	}
	total := 0
	for _, accountID := range ids {
		total += launch.count(accountID)
	}
	if total != 1 {
		t.Fatalf("排队期间应只试探启动 1 次，实际 %d 次", total)
	}
}

// TestStartupAuthWithoutRefreshMaterial 登录页跳转、没有续签材料：第一次失败即按认证代际标为需要重新登录，记入失效账户持久表，
// 之后不再启动；不设暂停
func TestStartupAuthWithoutRefreshMaterial(t *testing.T) {
	const b = "auth-b@example.com"
	launch := newStartupTestLaunch()
	launch.setFailure(b, func(options camoufoxnative.Options) error {
		reportStagesUpTo(options, camoufoxnative.StartupLoadingAIStudio)
		return nativeLoginRedirectError()
	})
	service := startupTestService(t, launch, 2, b)
	manager := service.workers
	<-manager.expandInBackground(b, testRuntimeModel)
	status := testStatus(t, service.pool, b)
	if status.State != aistudio.AccountAuthRequired {
		t.Fatalf("登录页跳转应标为需要重新登录，实际 %s", status.State)
	}
	entries := manager.startupFailures.list()
	if len(entries) != 1 || entries[0].accountID != b || entries[0].record.Kind != string(startupFailureAuth) ||
		entries[0].record.Count != 1 || !strings.HasPrefix(entries[0].record.URL, "https://accounts.google.com/") || entries[0].record.Stage != 5 {
		t.Fatalf("失效账户记录 = %+v", entries)
	}
	if until, _ := startupPauseOf(t, manager, b); !until.IsZero() {
		t.Fatal("登录失效不设暂停")
	}
	ctx, cancel := context.WithTimeout(service.lifecycle, 500*time.Millisecond)
	defer cancel()
	if lease, err := service.acquireWarmLease(ctx, aistudio.AccountSelection{ModelID: testRuntimeModel, Method: "generateContent"}); lease != nil || err == nil {
		t.Fatalf("需要重新登录的账户不应再被选中，lease=%v err=%v", lease, err)
	}
	if launch.count(b) != 1 {
		t.Fatalf("标为需要重新登录后不应再被启动，启动 %d 次", launch.count(b))
	}
	if !hasLogContaining(service.requests, "WAA Worker 启动失败计入账户", "类别=登录失效", "已标为需要重新登录") {
		t.Fatalf("应写计入账户的日志，日志 = %v", serviceLogMessages(service.requests))
	}
	result, err := (&runtimeAdmin{pool: service.pool, workers: manager, requests: service.requests}).StartupFailures(context.Background())
	if err != nil || len(result) != 1 || !result[0].Confirmed || result[0].Kind != "auth" || result[0].Count != 1 || result[0].URL == "" {
		t.Fatalf("失效账户列表 = %+v %v", result, err)
	}
}

// TestStartupStorageFailureMarksFiles 阶段 1 读取认证文件失败按原因分别处理：
//   - 内容坏了（解析 storage state 失败）：标为需要重新登录，原因写明认证文件，记入失效账户持久表，列为确定失效；
//   - 文件不存在（整体替换账户目录的中途）：文件确实不在时与取得租约时发现文件缺失一样标为不可用（FilesMissing，文件恢复后由账户目录
//     监视重新同步恢复），不改登录状态、不写失效记录、不列为失效；报不存在但文件已经复制回来的不改变账户；
//   - 读文件的其他系统错误（打开文件过多）：按环境问题只计数，不改变账户
func TestStartupStorageFailureMarksFiles(t *testing.T) {
	const broken, missing, restored, busy = "files-a@example.com", "files-missing@example.com", "files-restored@example.com",
		"files-b@example.com"
	launch := newStartupTestLaunch()
	launch.setFailure(broken, func(camoufoxnative.Options) error {
		return errors.New("解析 storage state: unexpected end of JSON input")
	})
	notExist := func(path string) func(camoufoxnative.Options) error {
		return func(camoufoxnative.Options) error {
			return fmt.Errorf("读取 storage state: %w", &fs.PathError{Op: "open", Path: path, Err: fs.ErrNotExist})
		}
	}
	launch.setFailure(missing, notExist("/accounts/files-missing/storage_state.json"))
	launch.setFailure(restored, notExist("/accounts/files-restored/storage_state.json"))
	launch.setFailure(busy, func(camoufoxnative.Options) error {
		return fmt.Errorf("读取 storage state: %w", &fs.PathError{Op: "open", Path: "/accounts/files-b/storage_state.json", Err: syscall.EMFILE})
	})
	service := startupTestService(t, launch, 5, broken, missing, restored, busy)
	manager := service.workers
	account, err := service.pool.Account(missing)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(account.StoragePath); err != nil {
		t.Fatal(err)
	}
	for _, accountID := range []string{broken, missing, restored, busy} {
		<-manager.expandInBackground(accountID, testRuntimeModel)
	}
	status := testStatus(t, service.pool, broken)
	if status.State != aistudio.AccountAuthRequired || !strings.Contains(status.Message, "认证文件") {
		t.Fatalf("认证文件损坏应标为需要重新登录、原因带认证文件，实际 %s %q", status.State, status.Message)
	}
	entries := manager.startupFailures.list()
	if len(entries) != 1 || entries[0].accountID != broken || entries[0].record.Kind != string(startupFailureFiles) || entries[0].record.Stage != 1 {
		t.Fatalf("失效账户记录 = %+v", entries)
	}
	if status := testStatus(t, service.pool, missing); status.State != aistudio.AccountUnavailable {
		t.Fatalf("认证文件不存在应按文件缺失标为不可用，实际 %s %q", status.State, status.Message)
	}
	filesMissing := false
	for _, summary := range service.pool.LoginSummaries() {
		if summary.ID == missing {
			filesMissing = summary.FilesMissing
		}
	}
	if !filesMissing {
		t.Fatal("文件缺失的账户应能由账户目录监视在文件恢复后恢复（FilesMissing）")
	}
	if !hasLogContaining(service.requests, "账户文件缺失，标为不可用") {
		t.Fatalf("应写文件缺失的日志，日志 = %v", serviceLogMessages(service.requests))
	}
	for _, accountID := range []string{restored, busy} {
		if status := testStatus(t, service.pool, accountID); status.State != aistudio.AccountReady {
			t.Fatalf("%s：读文件的系统错误不应改变账户状态，实际 %s", accountID, status.State)
		}
	}
	if until, _ := startupPauseOf(t, manager, busy); !until.IsZero() {
		t.Fatal("环境问题不设暂停")
	}
	if got := manager.environmentFailures(time.Now()); got != 3 {
		t.Fatalf("近 10 分钟环境类失败 = %d，期望 3", got)
	}
	result, err := (&runtimeAdmin{pool: service.pool, workers: manager, requests: service.requests}).StartupFailures(context.Background())
	if err != nil || len(result) != 1 || result[0].ID != broken || !result[0].Confirmed || result[0].Kind != "files" {
		t.Fatalf("失效账户列表只应有认证文件损坏的账户，实际 %+v %v", result, err)
	}
}

// TestStartupFingerprintFailureNotConfirmed 阶段 2 账户 Camoufox 指纹文件的内容坏了（写到一半时磁盘满或进程被杀，留下空文件或截断的 JSON）：
// 指纹不是登录凭据，重新上传认证文件、验证与浏览器登录都修不好它，不能按认证文件损坏标为需要重新登录、列为确定失效（只剩一键删除）。
// 账户状态不变，不写失效账户持久表，只在内存里暂停（与机器负载无关，近 5 分钟没有成功启动时也计次数），列为未确认并写明指纹文件路径
func TestStartupFingerprintFailureNotConfirmed(t *testing.T) {
	const corrupt, empty = "fp-corrupt@example.com", "fp-empty@example.com"
	launch := newStartupTestLaunch()
	launch.setFailure(corrupt, func(options camoufoxnative.Options) error {
		reportStagesUpTo(options, camoufoxnative.StartupPreparingBrowser)
		return errors.New("解析账户 Camoufox 指纹: unexpected end of JSON input")
	})
	launch.setFailure(empty, func(options camoufoxnative.Options) error {
		reportStagesUpTo(options, camoufoxnative.StartupPreparingBrowser)
		return errors.New("账户 Camoufox 指纹为空")
	})
	service := startupTestService(t, launch, 3, corrupt, empty)
	manager := service.workers
	paths := map[string]string{}
	for _, accountID := range []string{corrupt, empty} {
		account, err := service.pool.Account(accountID)
		if err != nil {
			t.Fatal(err)
		}
		paths[accountID] = filepath.Join(filepath.Dir(account.StoragePath), "camoufox-fingerprint.json")
		<-manager.expandInBackground(accountID, testRuntimeModel)
		if status := testStatus(t, service.pool, accountID); status.State != aistudio.AccountReady {
			t.Fatalf("%s：指纹文件损坏不应改变账户状态，实际 %s %q", accountID, status.State, status.Message)
		}
		// 服务刚启动、还没有任何 Worker 启动成功：原因不确定的失败只暂停 1 分钟、不计次数，指纹文件损坏照常计次数
		expectPause(t, manager, accountID, 2*time.Minute, 1)
		if !hasLogContaining(service.requests, "WAA Worker 启动失败计入账户", "类别=指纹文件损坏", "阶段=2/7", "连续失败=1",
			"暂停调度=2m0s", "指纹文件="+paths[accountID], "不计入失效账户") {
			t.Fatalf("%s：应写明指纹文件路径，日志 = %v", accountID, serviceLogMessages(service.requests))
		}
	}
	if !hasLogContaining(service.requests, "WAA Worker 启动失败", "阶段=2/7", "类别=指纹文件损坏") {
		t.Fatal("启动失败日志的类别应为指纹文件损坏")
	}
	if entries := manager.startupFailures.list(); len(entries) != 0 {
		t.Fatalf("指纹文件损坏不应写入失效账户持久表，实际 %+v", entries)
	}
	if got := manager.environmentFailures(time.Now()); got != 0 {
		t.Fatalf("指纹文件损坏不计入环境类失败，实际 %d", got)
	}
	if available := manager.startupBackoffAvailable([]string{corrupt, empty}, false); len(available) != 0 {
		t.Fatalf("暂停中的账户应被过滤，实际 %v", available)
	}
	result, err := (&runtimeAdmin{pool: service.pool, workers: manager, requests: service.requests}).StartupFailures(context.Background())
	if err != nil || len(result) != 2 {
		t.Fatalf("启动失败列表 = %+v %v", result, err)
	}
	for _, entry := range result {
		if entry.Confirmed || entry.Kind != "fingerprint" || entry.Stage != 2 || entry.Count != 1 || entry.Until == "" ||
			!strings.Contains(entry.Reason, paths[entry.ID]) || !strings.Contains(entry.Message, "Camoufox 指纹") {
			t.Fatalf("指纹文件损坏应列为未确认并写明指纹文件路径，实际 %+v", entry)
		}
	}
	// 连续失败照常升级暂停；换一类原因（原因不确定）时从头计
	manager.lastStartupSuccess.Store(time.Now().UnixNano())
	fingerprintFailure := &accountWorkerInitError{err: errors.New("解析账户 Camoufox 指纹: unexpected end of JSON input"), stage: 2}
	manager.noteStartupFailure(service.lifecycle, corrupt, service.pool.AuthGeneration(corrupt), time.Now(), fingerprintFailure)
	expectPause(t, manager, corrupt, 10*time.Minute, 2)
	manager.noteStartupFailure(service.lifecycle, corrupt, service.pool.AuthGeneration(corrupt), time.Now(), pageNotReadyFailure())
	expectPause(t, manager, corrupt, 2*time.Minute, 1)
	if status := testStatus(t, service.pool, corrupt); status.State != aistudio.AccountReady {
		t.Fatalf("账户状态不应改变，实际 %s", status.State)
	}
}

// TestStartupAuthWithRefreshMaterialNotConfirmed 有续签材料的账户在没有租约的启动路径（后台扩容）上被跳到登录页：账户状态不变
// （留给带租约的请求恢复），不记入失效账户持久表，不列为确定失效、不能被一键删除；反复失败也不累计
func TestStartupAuthWithRefreshMaterialNotConfirmed(t *testing.T) {
	const b = "refresh-b@example.com"
	launch := newStartupTestLaunch()
	launch.setFailure(b, func(options camoufoxnative.Options) error {
		reportStagesUpTo(options, camoufoxnative.StartupLoadingAIStudio)
		return nativeLoginRedirectError()
	})
	service := startupTestService(t, launch, 2, b)
	manager := service.workers
	account, err := service.pool.Account(b)
	if err != nil {
		t.Fatal(err)
	}
	if err := aistudio.WriteStorageState(account.StoragePath, authTestState(t, "test", true)); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		<-manager.expandInBackground(b, testRuntimeModel)
	}
	if launch.count(b) != 3 {
		t.Fatalf("启动 %d 次，期望 3 次", launch.count(b))
	}
	if status := testStatus(t, service.pool, b); status.State != aistudio.AccountReady {
		t.Fatalf("有续签材料时账户状态不变，实际 %s", status.State)
	}
	if entries := manager.startupFailures.list(); len(entries) != 0 {
		t.Fatalf("没有标为需要重新登录的账户不应记入失效账户持久表，记录 = %+v", entries)
	}
	if !hasLogContaining(service.requests, "WAA Worker 启动失败计入账户", "类别=登录失效", "不计入失效账户") {
		t.Fatalf("应写不计入失效账户的日志，日志 = %v", serviceLogMessages(service.requests))
	}
	result, err := (&runtimeAdmin{pool: service.pool, workers: manager, requests: service.requests}).StartupFailures(context.Background())
	if err != nil || len(result) != 0 {
		t.Fatalf("失效账户列表应为空，实际 %+v %v", result, err)
	}
	if dead := manager.startupStatus(0).Dead; dead != 0 {
		t.Fatalf("状态接口的确定失效数 = %d，期望 0", dead)
	}
}

// TestStartupFailureMarkRejectedNotRecorded 启动开始后账户验证通过（MarkReady 把认证检查时间推后）、较早开始的启动随后失败：
// 认证文件损坏的标记按检查时间被拒绝，登录失效同样不会让账户变成需要重新登录；账户仍为就绪，两者都不记入失效账户持久表
func TestStartupFailureMarkRejectedNotRecorded(t *testing.T) {
	const files, auth = "rejected-files@example.com", "rejected-auth@example.com"
	service := startupTestService(t, newStartupTestLaunch(), 3, files, auth)
	manager := service.workers
	for accountID, failure := range map[string]*accountWorkerInitError{
		files: {err: errors.New("解析 storage state: unexpected end of JSON input"), stage: 1},
		auth:  {err: nativeLoginRedirectError(), stage: 5},
	} {
		generation := service.pool.AuthGeneration(accountID)
		startedAt := time.Now().Add(-time.Second)
		if err := service.pool.MarkReady(accountID); err != nil {
			t.Fatal(err)
		}
		manager.noteStartupFailure(service.lifecycle, accountID, generation, startedAt, failure)
		if status := testStatus(t, service.pool, accountID); status.State != aistudio.AccountReady {
			t.Fatalf("%s：验证通过之后较早开始的启动失败不应改变账户状态，实际 %s", accountID, status.State)
		}
		if recorded(manager.startupFailures, accountID) {
			t.Fatalf("%s：没有标为需要重新登录的账户不应记入失效账户持久表", accountID)
		}
	}
}

// TestStartupBackoffGenerationsBatch 暂停的过滤与统计按一次取得的认证代际核对：认证代际已变化的账户不再暂停，其余照常过滤
func TestStartupBackoffGenerationsBatch(t *testing.T) {
	const kept, renewed, idle = "batch-kept@example.com", "batch-renewed@example.com", "batch-idle@example.com"
	service := startupTestService(t, newStartupTestLaunch(), 3, kept, renewed, idle)
	manager := service.workers
	manager.lastStartupSuccess.Store(time.Now().UnixNano())
	for _, accountID := range []string{kept, renewed} {
		manager.noteStartupFailure(service.lifecycle, accountID, service.pool.AuthGeneration(accountID), time.Now(), pageNotReadyFailure())
	}
	if available := manager.startupBackoffAvailable([]string{kept, renewed, idle}, false); len(available) != 1 || available[0] != idle {
		t.Fatalf("过滤后 = %v，期望只剩 %s", available, idle)
	}
	// 记录的认证代际与当前不同，等同于记录之后认证已更新（重新上传或续签）
	workerOf(t, manager, renewed).startFailGen.Add(1)
	if available := manager.startupBackoffAvailable([]string{kept, renewed, idle}, false); len(available) != 2 ||
		available[0] != renewed || available[1] != idle {
		t.Fatalf("认证代际变化后 = %v，期望 %s、%s", available, renewed, idle)
	}
	if got := manager.pausedStartupCount(time.Now()); got != 1 {
		t.Fatalf("暂停中的账户数 = %d，期望 1", got)
	}
	if pauses := manager.startupPauses(time.Now()); len(pauses) != 1 || pauses[0].accountID != kept {
		t.Fatalf("暂停列表 = %+v，期望只有 %s", pauses, kept)
	}
}

// TestStartupSlotsLateCountsAsCapacity 复现 P2：后台启动的期限包含等冷启动名额的时间（任务期限 320ms，名额 250ms 后才放出）。
// 等到名额时剩余期限不够一次完整启动：放回名额、不调用启动，按容量已满返回（errors.Is errAccountWorkerCapacity），
// 预热不记为失败（warmFailures 为空），等待原因写明冷启动名额被按需启动占满
func TestStartupSlotsLateCountsAsCapacity(t *testing.T) {
	const a, b = "late-a@example.com", "late-b@example.com"
	manager, gate := slotTestManager(t, "camoufox-test", 1, a, b)
	gate.open()
	manager.initTimeout.Store(int64(200 * time.Millisecond))
	manager.warmGrace.Store(int64(120 * time.Millisecond))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := manager.Worker(ctx, a, testRuntimeModel); err != nil {
		t.Fatal(err)
	}
	hold := func() {
		t.Helper()
		release, err := manager.startupSlots.acquire(ctx, manager.warmConcurrencyValue, false, nil)
		if err != nil {
			t.Fatal(err)
		}
		time.AfterFunc(250*time.Millisecond, release)
	}

	hold()
	taskCtx, taskCancel := context.WithTimeout(withBackgroundStartup(ctx), manager.warmTaskTimeout())
	_, err := manager.ensureWorker(taskCtx, b, "", false)
	taskCancel()
	if !errors.Is(err, errAccountWorkerCapacity) || !errors.Is(err, errStartupSlotsLate) {
		t.Fatalf("err=%v，期望按冷启动名额被占满、剩余期限不足返回", err)
	}
	if gate.isStarted(b) {
		t.Fatal("剩余期限不足时不应调用启动")
	}
	waitStartupUsage(t, &manager.startupSlots, 0, 0, 0)

	hold()
	first := manager.StartPrewarm(ctx)
	select {
	case <-first:
	case <-ctx.Done():
		t.Fatal("预热没有结束")
	}
	manager.waitPrewarm()
	if gate.isStarted(b) {
		t.Fatal("预热不应调用启动")
	}
	if failures := manager.recentWarmFailures(time.Now()); len(failures) != 0 {
		t.Fatalf("剩余期限不足不应记为预热失败，warmFailures = %v", failures)
	}
	if !hasLogContaining(manager.requests, "预热等待", "冷启动名额被按需启动占满") {
		t.Fatalf("等待原因应写明冷启动名额被按需启动占满，日志 = %v", serviceLogMessages(manager.requests))
	}
	if until, _ := startupPauseOf(t, manager, b); !until.IsZero() {
		t.Fatal("名额不足不应计入账户")
	}
}

// TestStartupSlotsWaitDeadlineIsLate 按需启动一直占着冷启动名额，后台启动等名额一直等到期限：按冷启动名额不足返回
// （errStartupSlotsLate，仍是容量已满），不调用启动、不记预热失败，预热的等待原因写“冷启动名额被按需启动占满”而不是槽位已满；
// 等名额时被取消（不是到期）仍按容量已满返回
func TestStartupSlotsWaitDeadlineIsLate(t *testing.T) {
	const a, b = "wait-a@example.com", "wait-b@example.com"
	manager, gate := slotTestManager(t, "camoufox-test", 1, a, b)
	gate.open()
	manager.initTimeout.Store(int64(200 * time.Millisecond))
	manager.warmGrace.Store(int64(120 * time.Millisecond))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := manager.Worker(ctx, a, testRuntimeModel); err != nil {
		t.Fatal(err)
	}
	release, err := manager.startupSlots.acquire(ctx, manager.warmConcurrencyValue, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	waitCtx, waitCancel := context.WithTimeout(withBackgroundStartup(ctx), 100*time.Millisecond)
	_, err = manager.acquireStartupSlot(waitCtx, b, "camoufox-test")
	waitCancel()
	if !errors.Is(err, errStartupSlotsLate) || !errors.Is(err, errAccountWorkerCapacity) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err=%v，期望等名额到期按冷启动名额不足返回", err)
	}
	canceledCtx, cancelWait := context.WithCancel(withBackgroundStartup(ctx))
	time.AfterFunc(50*time.Millisecond, cancelWait)
	_, err = manager.acquireStartupSlot(canceledCtx, b, "camoufox-test")
	if !errors.Is(err, errAccountWorkerCapacity) || errors.Is(err, errStartupSlotsLate) {
		t.Fatalf("err=%v，期望取消按容量已满返回、不算名额不足", err)
	}

	first := manager.StartPrewarm(ctx)
	select {
	case <-first:
	case <-ctx.Done():
		t.Fatal("预热没有结束")
	}
	manager.waitPrewarm()
	if gate.isStarted(b) {
		t.Fatal("等不到名额时预热不应调用启动")
	}
	if failures := manager.recentWarmFailures(time.Now()); len(failures) != 0 {
		t.Fatalf("等名额到期不应记为预热失败，warmFailures = %v", failures)
	}
	if !hasLogContaining(manager.requests, "预热等待", "冷启动名额被按需启动占满") || hasLogContaining(manager.requests, "预热等待", "槽位已满") {
		t.Fatalf("等待原因应写明冷启动名额被按需启动占满，日志 = %v", serviceLogMessages(manager.requests))
	}
}

// TestStartupCancelLogged 启动途中请求被取消：写“WAA Worker 启动取消”（原因、阶段、排队、启动），不写启动失败、不计入账户
func TestStartupCancelLogged(t *testing.T) {
	const b = "cancel-b@example.com"
	launch := newStartupTestLaunch()
	launch.gate = make(chan struct{})
	service := startupTestService(t, launch, 2, b)
	manager := service.workers
	ctx, cancel := context.WithCancel(service.lifecycle)
	done := make(chan error, 1)
	go func() {
		_, err := manager.Worker(ctx, b, testRuntimeModel)
		done <- err
	}()
	waitFor(t, "b 开始启动", func() bool { return launch.count(b) == 1 })
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v，期望取消", err)
	}
	if !hasLogContaining(service.requests, "WAA Worker 启动取消", "原因=已取消", "阶段=1/7", "排队=", "启动=") {
		t.Fatalf("应写启动取消日志，日志 = %v", serviceLogMessages(service.requests))
	}
	if hasLogContaining(service.requests, "WAA Worker 启动失败") {
		t.Fatal("取消不应写启动失败")
	}
	if until, count := startupPauseOf(t, manager, b); !until.IsZero() || count != 0 || manager.environmentFailures(time.Now()) != 0 {
		t.Fatal("取消不应计入账户或环境类失败")
	}
}

// TestClassifyStartupFailure 分类规则按顺序取第一条匹配
func TestClassifyStartupFailure(t *testing.T) {
	for _, test := range []struct {
		name  string
		err   error
		stage int
		want  startupFailureKind
	}{
		{"不是启动错误", pageNotReadyError(), 5, startupFailureNone},
		{"容量已满", &accountWorkerInitError{err: errStartupSlotsLate, stage: 5}, 5, startupFailureNone},
		{"被其他进程占用", &accountWorkerInitError{err: fmt.Errorf("%w: x", errAccountRuntimeBusy), stage: 5}, 5, startupFailureNone},
		{"登录页跳转", &accountWorkerInitError{err: nativeLoginRedirectError(), stage: 5}, 5, startupFailureAuth},
		{"指纹解析失败", &accountWorkerInitError{err: errors.New("解析账户 Camoufox 指纹: unexpected EOF"), stage: 2}, 2, startupFailureFingerprint},
		{"指纹为空", &accountWorkerInitError{err: errors.New("账户 Camoufox 指纹为空")}, 1, startupFailureFingerprint},
		{"写指纹时磁盘满", &accountWorkerInitError{err: fmt.Errorf("写入账户 Camoufox 指纹: %w",
			&fs.PathError{Op: "open", Path: "/accounts/x/camoufox-fingerprint.json", Err: syscall.ENOSPC}), stage: 2}, 2, startupFailureEnvironment},
		{"storage state 解析失败", &accountWorkerInitError{err: errors.New("解析 storage state: unexpected EOF"), stage: 1}, 1, startupFailureFiles},
		{"阶段 5 的 storage state 文字", &accountWorkerInitError{err: errors.New("storage state 写回失败"), stage: 5}, 5, startupFailureAmbiguous},
		{"启动 Camoufox", &accountWorkerInitError{err: errors.New("等待 Camoufox BiDi 端点超时"), stage: 3}, 3, startupFailureEnvironment},
		{"runtime 租约", &accountWorkerInitError{err: errors.New("lock: permission denied"), leaseFailed: true}, 0, startupFailureEnvironment},
		{"输入框未就绪", pageNotReadyFailure(), 5, startupFailureAmbiguous},
		{"WAA 失败", &accountWorkerInitError{err: errors.New("填写 bootstrap 提示词失败"), stage: 7}, 7, startupFailureAmbiguous},
	} {
		if got := classifyStartupFailure(test.err, test.stage); got != test.want {
			t.Errorf("%s：分类 = %q，期望 %q", test.name, got, test.want)
		}
	}
}

// TestStartupFailureCountsAtomicReads 暂停读取不取账户锁：账户锁被占用时选号照常过滤
func TestStartupFailureCountsAtomicReads(t *testing.T) {
	const b = "atomic-b@example.com"
	service := startupTestService(t, newStartupTestLaunch(), 2, b)
	manager := service.workers
	manager.lastStartupSuccess.Store(time.Now().UnixNano())
	manager.noteStartupFailure(service.lifecycle, b, service.pool.AuthGeneration(b), time.Now(), pageNotReadyFailure())
	manager.mu.RLock()
	account := manager.accounts[b]
	manager.mu.RUnlock()
	account.mu.Lock()
	var filtered atomic.Bool
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		filtered.Store(len(manager.startupBackoffAvailable([]string{b}, false)) == 0)
	}()
	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		account.mu.Unlock()
		t.Fatal("过滤不应等待账户锁")
	}
	account.mu.Unlock()
	if !filtered.Load() {
		t.Fatal("暂停中的账户应被过滤")
	}
}
