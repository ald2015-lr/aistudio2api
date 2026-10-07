package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/api"
	"github.com/Mag1cFall/AIStudio2API/internal/camoufoxnative"
	"github.com/Mag1cFall/AIStudio2API/internal/config"
)

// newRuntime 装配单个生成服务实例
func newRuntime(
	launchCtx context.Context,
	lifecycle context.Context,
	cfg config.Config,
	requests *requestRegistry,
) (*trackedService, *runtimeAdmin, func() error, error) {
	if err := launchCtx.Err(); err != nil {
		return nil, nil, nil, err
	}
	startedAt := time.Now()
	requests.log("service", "INFO", "运行时装配 | 1/3 | 载入账户")
	store := aistudio.NewAccountStore(strings.Split(cfg.AuthStates, ",")...)
	accounts, loadFailures := store.LoadTolerant()
	for _, failure := range loadFailures {
		requests.log("service", "WARN", "跳过无法载入的账户目录 | 错误="+strings.TrimSpace(failure.Error()))
	}
	if err := launchCtx.Err(); err != nil {
		return nil, nil, nil, err
	}
	camoufoxPath, login, err := prepareWAABackend(launchCtx, cfg, requests, len(accounts))
	if err != nil {
		return nil, nil, nil, err
	}

	requests.log("service", "INFO", "运行时装配 | 3/3 | 创建协议客户端")
	pool := aistudio.NewAccountPool(accounts, cfg.PerAccountConcurrency)
	pool.SetRoutingStrategy(cfg.RoutingStrategy)
	pool.SetUpstreamChannels(upstreamChannels(cfg.UpstreamChannels))
	headers, err := newAccountHeaderProvider(accounts, cfg.Proxy)
	if err != nil {
		return nil, nil, nil, err
	}
	transport, err := aistudio.NewMakerSuiteHTTPTransport(aistudio.HTTPTransportOptions{
		Pool: pool, Signer: aistudio.NewSigner(), Headers: headers, GlobalProxy: cfg.Proxy,
	})
	if err != nil {
		headers.Close()
		return nil, nil, nil, err
	}
	workers := newAccountWorkerManager(
		pool, accounts, requests, camoufoxPath, cfg.Proxy, cfg.InitTimeout,
		cfg.WarmWorkerLimit, cfg.MaxActiveWorkers, cfg.WarmStartupConcurrency, cfg.TemporaryChat,
	)
	protected, err := aistudio.NewWorkerProtectedTransport(aistudio.WorkerProtectedTransportOptions{
		Transport: transport, Workers: workers, SetupTimeout: cfg.InitTimeout,
	})
	if err != nil {
		transport.CloseIdleConnections()
		headers.Close()
		return nil, nil, nil, errors.Join(err, workers.Close())
	}
	requestContext, err := aistudio.NewPoolRequestContextProvider(pool)
	if err != nil {
		transport.CloseIdleConnections()
		headers.Close()
		return nil, nil, nil, errors.Join(err, workers.Close())
	}
	refresher := newAuthRuntimeRefresher(workers, headers, requests, cfg.Proxy)
	client, err := aistudio.NewClient(aistudio.ClientOptions{
		Transport:       &authRetryTransport{transport: transport, refresher: refresher},
		Protected:       &authRetryProtectedTransport{transport: protected, refresher: refresher},
		ContextProvider: requestContext,
	})
	if err != nil {
		transport.CloseIdleConnections()
		headers.Close()
		return nil, nil, nil, errors.Join(err, workers.Close())
	}
	pooled, err := aistudio.NewPooledService(pool, client)
	if err != nil {
		transport.CloseIdleConnections()
		headers.Close()
		return nil, nil, nil, errors.Join(err, workers.Close())
	}
	service := newTrackedService(lifecycle, pooled, pool, requests, workers, cfg.RequestTimeout)
	service.quota = newQuotaSharing(store.PrimaryDirectory(), requests)
	service.ignoreSeed.Store(cfg.IgnoreClientSeed)
	service.repeatNonce.Store(cfg.RepeatPromptNonce)
	service.minOutputTokens.Store(int64(cfg.MinOutputTokens))
	service.setDowngradeGuard(cfg.DowngradeGuard)
	admin := newRuntimeAdmin(lifecycle, pool, store, service, requests, login, workers, headers, cfg)
	// 新账户自动处理：先载入处理记录，再开始扫描目录，保证扫描导入的账户能进入队列
	accountDisableNotes.load(store.PrimaryDirectory())
	admin.onboard = newAccountOnboarder(admin, store.PrimaryDirectory())
	go admin.onboard.run(lifecycle)
	// 生成服务实例存活期间自动识别新增、删除与重新登录的账户目录
	go admin.watchAccountDirectories(lifecycle)
	requests.log("service", "INFO", fmt.Sprintf(
		"协议运行时就绪 | 账户=%d | 耗时=%s",
		len(accounts), time.Since(startedAt).Round(time.Millisecond),
	))
	closeRuntime := func() error {
		err := workers.Close()
		transport.CloseIdleConnections()
		headers.Close()
		return err
	}
	return service, admin, closeRuntime, nil
}

// accountWorkerManager 管理每账户的长驻 WAA worker
type accountWorkerManager struct {
	// bootstrapCache 为预热概况的短时缓存（见 bootstrapSummary）
	bootstrapMu    sync.Mutex
	bootstrapCache aistudio.BootstrapSummary
	bootstrapAt    time.Time
	mu             sync.RWMutex
	fillMu         sync.Mutex
	rebalanceMu    sync.Mutex
	pool           *aistudio.AccountPool
	accounts       map[string]*accountWorker
	openings       map[string]chan struct{}
	// openingSet 是 openings 的无锁镜像，供状态接口读取正在启动的账户
	openingSet sync.Map
	// warmFailures 记录预热失败的账户与时间，冷却期内不再反复尝试
	warmFailMu   sync.Mutex
	warmFailures map[string]time.Time
	// warmIdleReason 与 warmIdleAt 用于预热等待原因的日志去重
	warmIdleReason string
	warmIdleAt     time.Time
	// fill* 为预热循环的实时状态，供管理页显示
	fillActive    atomic.Bool
	fillInflight  atomic.Int32
	fillLaunched  atomic.Int32
	fillStartedAt atomic.Int64
	fillLoopAt    atomic.Int64
	// hotModels 为预热池中被大量账户长时间冷却的模型，预热选号时优先避开在这些模型上冷却的账户
	hotModels       atomic.Pointer[[]string]
	requests        *requestRegistry
	camoufox        string
	globalProxy     string
	initTimeout     atomic.Int64
	warmTarget      atomic.Int64
	maxActive       atomic.Int64
	warmConcurrency atomic.Int64
	temporaryChat   atomic.Bool
	lifecycle       context.Context
	cancel          context.CancelFunc
	closed          bool
	signalMu        sync.Mutex
	signal          chan struct{}
	dispatch        *dispatchQueue
	victimMu        sync.Mutex
	victims         map[string]struct{}
	backgroundMu    sync.Mutex
	background      context.Context
	stopBackground  context.CancelFunc
}

type accountWorker struct {
	mu             sync.Mutex
	startupMu      sync.Mutex
	id             string
	label          string
	config         camoufoxnative.Options
	worker         *aistudio.NativeWorker
	bootstrapModel string
	runtimeLease   *aistudio.AccountRuntimeLease
	cleanupWorker  *aistudio.NativeWorker
	cleanupLease   *aistudio.AccountRuntimeLease
	warm           atomic.Bool
	readyAt        atomic.Int64
	generation     atomic.Uint64
	busyUntil      time.Time
	busyDelay      time.Duration
	busyErr        error
}

type accountWorkerPreparer struct {
	account        *accountWorker
	worker         *aistudio.NativeWorker
	bootstrapModel string
	manager        *accountWorkerManager
	runtimeLease   *aistudio.AccountRuntimeLease
	ownsLease      bool
	startedAt      time.Time
	pending        bool
}

type accountWorkerUpdate struct {
	account *accountWorker
	config  camoufoxnative.Options
	label   string
	pending bool
}

var errAccountWorkerReplaced = errors.New("WAA worker 已更新")
var errAccountWorkerOpening = errors.New("WAA worker 正在启动")

// errAccountWorkerCapacity 表示当前没有可立即使用的 Worker 槽位
var errAccountWorkerCapacity = errors.New("WAA worker 槽位已满")
var errAccountWorkerCleanupPending = errors.New("WAA worker 清理未完成")

// errAccountRuntimeBusy 表示账户 WAA runtime 租约由其他进程持有
var errAccountRuntimeBusy = errors.New("WAA runtime 由其他进程占用")

const (
	// runtimeBusyInitialDelay 为账户被其他进程占用后首次重新探测的间隔
	runtimeBusyInitialDelay = 5 * time.Second
	// runtimeBusyMaxDelay 为占用账户重新探测间隔的上限
	runtimeBusyMaxDelay = time.Minute
)

const (
	workerEvictionTimeout = 100 * time.Millisecond
)

// Prepare 在账户 Worker 有效期间生成 proof
func (preparer *accountWorkerPreparer) Prepare(ctx context.Context, request aistudio.ProtectedRequest) (aistudio.PreparedProtectedRequest, error) {
	preparer.account.mu.Lock()
	defer preparer.account.mu.Unlock()
	if preparer.account.worker != preparer.worker {
		return aistudio.PreparedProtectedRequest{}, errAccountWorkerReplaced
	}
	if preparer.account.bootstrapModel != preparer.bootstrapModel {
		return aistudio.PreparedProtectedRequest{}, errAccountWorkerReplaced
	}
	return preparer.worker.Prepare(ctx, request)
}

// SendProtected 校验当前账户 Worker 后发送浏览器请求
func (preparer *accountWorkerPreparer) SendProtected(ctx context.Context, request aistudio.ProtectedRequest) (*aistudio.RPCResponse, error) {
	preparer.account.mu.Lock()
	current := preparer.account.worker == preparer.worker && preparer.account.bootstrapModel == preparer.bootstrapModel
	preparer.account.mu.Unlock()
	if !current {
		return nil, errAccountWorkerReplaced
	}
	return preparer.worker.SendProtected(ctx, request)
}

// BrowserStorageState 返回同一有效账户 Worker 的浏览器 Cookie 状态
func (preparer *accountWorkerPreparer) BrowserStorageState(ctx context.Context) (aistudio.StorageState, error) {
	preparer.account.mu.Lock()
	defer preparer.account.mu.Unlock()
	if preparer.account.worker != preparer.worker || preparer.account.bootstrapModel != preparer.bootstrapModel {
		return aistudio.StorageState{}, errAccountWorkerReplaced
	}
	return preparer.worker.BrowserStorageState(ctx)
}

// accountWorkerInitError 表示单个账户的 WAA worker 初始化失败
type accountWorkerInitError struct {
	err error
}

func (err *accountWorkerInitError) Error() string {
	return err.err.Error()
}

func (err *accountWorkerInitError) Unwrap() error {
	return err.err
}

// newAccountWorkerManager 创建账户 worker 配置
func newAccountWorkerManager(
	pool *aistudio.AccountPool,
	accounts []*aistudio.Account,
	requests *requestRegistry,
	camoufoxPath string,
	globalProxy string,
	initTimeout time.Duration,
	warmTarget int,
	maxActive int,
	warmConcurrency int,
	temporaryChat bool,
) *accountWorkerManager {
	lifecycle, cancel := context.WithCancel(context.Background())
	manager := &accountWorkerManager{
		pool: pool, accounts: make(map[string]*accountWorker, len(accounts)), requests: requests, camoufox: camoufoxPath,
		globalProxy: globalProxy,
		openings:    make(map[string]chan struct{}),
		lifecycle:   lifecycle, cancel: cancel, signal: make(chan struct{}), dispatch: newDispatchQueue(),
		victims: make(map[string]struct{}),
	}
	manager.initTimeout.Store(int64(initTimeout))
	manager.warmTarget.Store(int64(warmTarget))
	manager.maxActive.Store(int64(maxActive))
	manager.warmConcurrency.Store(int64(warmConcurrency))
	manager.temporaryChat.Store(temporaryChat)
	manager.background, manager.stopBackground = context.WithCancel(lifecycle)
	for _, account := range accounts {
		if account == nil {
			continue
		}
		manager.accounts[account.ID] = manager.newAccountWorker(account)
	}
	go manager.forwardPoolChanges()
	go manager.reapIdleWorkers()
	return manager
}

// expandInBackground 在后台为账户启动 Worker，就绪后由调度信号唤醒排队请求
func (manager *accountWorkerManager) expandInBackground(accountID string, modelID string) {
	ctx := manager.backgroundContext()
	go func() {
		_, err := manager.ensureWorker(ctx, accountID, modelID, false)
		if err == nil || ctx.Err() != nil || errors.Is(err, errAccountWorkerOpening) ||
			errors.Is(err, errAccountWorkerCapacity) || errors.Is(err, errAccountRuntimeBusy) {
			return
		}
		manager.requests.log(accountID, "WARN", fmt.Sprintf("WAA Worker 后台扩容失败 | 错误=%v", err))
	}()
}

// backgroundContext 返回后台扩容使用的 context，ResetAll 时取消
func (manager *accountWorkerManager) backgroundContext() context.Context {
	manager.backgroundMu.Lock()
	defer manager.backgroundMu.Unlock()
	return manager.background
}

// cancelBackgroundStartups 取消进行中的后台扩容并为后续扩容建立新 context
func (manager *accountWorkerManager) cancelBackgroundStartups() {
	manager.backgroundMu.Lock()
	manager.stopBackground()
	manager.background, manager.stopBackground = context.WithCancel(manager.lifecycle)
	manager.backgroundMu.Unlock()
}

// markRuntimeBusy 让被其他进程占用的账户按指数退避暂时退出候选，每段占用只记录一次
func (manager *accountWorkerManager) markRuntimeBusy(account *accountWorker, cause error) error {
	account.mu.Lock()
	first := account.busyDelay == 0
	if first {
		account.busyDelay = runtimeBusyInitialDelay
	} else {
		account.busyDelay = min(account.busyDelay*2, runtimeBusyMaxDelay)
	}
	delay := account.busyDelay
	account.busyUntil = time.Now().Add(delay)
	account.busyErr = fmt.Errorf("%w: %w", errAccountRuntimeBusy, cause)
	busyErr := account.busyErr
	label := account.label
	account.mu.Unlock()
	if first {
		manager.requests.log(label, "WARN", fmt.Sprintf(
			"WAA Worker 账户由其他进程占用 | 暂停调度=%s | 错误=%s", delay, strings.TrimSpace(cause.Error()),
		))
	}
	time.AfterFunc(delay, manager.notifyScheduling)
	return busyErr
}

// clearRuntimeBusy 在重新取得 runtime 租约后恢复账户调度
func (manager *accountWorkerManager) clearRuntimeBusy(account *accountWorker) {
	account.mu.Lock()
	wasBusy := account.busyDelay != 0
	account.busyDelay = 0
	account.busyUntil = time.Time{}
	account.busyErr = nil
	label := account.label
	account.mu.Unlock()
	if wasBusy {
		manager.requests.log(label, "INFO", "WAA Worker 账户占用解除")
	}
}

// runtimeAvailable 过滤仍在占用退避期内的账户，并返回其中一个占用原因
func (manager *accountWorkerManager) runtimeAvailable(accountIDs []string) ([]string, error) {
	now := time.Now()
	available := make([]string, 0, len(accountIDs))
	var busyErr error
	for _, accountID := range accountIDs {
		manager.mu.RLock()
		account := manager.accounts[accountID]
		manager.mu.RUnlock()
		if account == nil {
			available = append(available, accountID)
			continue
		}
		account.mu.Lock()
		busy := now.Before(account.busyUntil)
		if busy && busyErr == nil {
			busyErr = account.busyErr
		}
		account.mu.Unlock()
		if !busy {
			available = append(available, accountID)
		}
	}
	return available, busyErr
}

// workerIdleTimeout 为超出热池目标的 Worker 空闲多久后关闭
const workerIdleTimeout = 5 * time.Minute

// reapIdleWorkers 定期关闭超出热池目标且空闲超时的 Worker
func (manager *accountWorkerManager) reapIdleWorkers() {
	ticker := time.NewTicker(workerIdleTimeout / 10)
	defer ticker.Stop()
	for {
		select {
		case <-manager.lifecycle.Done():
			return
		case <-ticker.C:
			for manager.reapIdleWorker(time.Now().Add(-workerIdleTimeout)) {
			}
		}
	}
}

// reapIdleWorker 关闭一个最近使用早于 before 的多余空闲 Worker，并返回是否关闭
func (manager *accountWorkerManager) reapIdleWorker(before time.Time) bool {
	manager.rebalanceMu.Lock()
	defer manager.rebalanceMu.Unlock()
	if len(manager.openings) > 0 || len(manager.WarmAccountIDs()) <= manager.warmTargetValue() {
		return false
	}
	victim := manager.idleWarmVictim("")
	if victim == "" {
		return false
	}
	manager.mu.RLock()
	account := manager.accounts[victim]
	manager.mu.RUnlock()
	_, lastUsed := manager.pool.Activity(victim)
	if lastUsed.After(before) || account != nil && time.Unix(0, account.readyAt.Load()).After(before) {
		return false
	}
	evicted, err := manager.evictIdleWorker(manager.lifecycle, victim)
	if err != nil {
		manager.requests.log(victim, "WARN", fmt.Sprintf("WAA Worker 空闲回收失败 | 错误=%v", err))
		return false
	}
	if evicted {
		manager.requests.log("service", "INFO", fmt.Sprintf(
			"WAA Worker 空闲回收 | Worker=%d/%d", len(manager.WarmAccountIDs()), manager.maxActiveValue(),
		))
	}
	return evicted
}

// forwardPoolChanges 在账户池租约或状态变化后唤醒排队请求
func (manager *accountWorkerManager) forwardPoolChanges() {
	for {
		changed := manager.pool.Changed()
		select {
		case <-manager.lifecycle.Done():
			return
		case <-changed:
			manager.dispatch.wakeHeads()
		}
	}
}

// schedulingChanged 返回下一次 Worker 容量或热池状态变化时关闭的通道
func (manager *accountWorkerManager) schedulingChanged() <-chan struct{} {
	manager.signalMu.Lock()
	defer manager.signalMu.Unlock()
	return manager.signal
}

// notifyScheduling 在 Worker 容量或热池变化后唤醒等待请求
func (manager *accountWorkerManager) notifyScheduling() {
	manager.signalMu.Lock()
	close(manager.signal)
	manager.signal = make(chan struct{})
	manager.signalMu.Unlock()
	manager.dispatch.wakeHeads()
}

// finishOpening 结束账户 Worker 启动占位并唤醒等待容量的请求
func (manager *accountWorkerManager) finishOpening(accountID string, opening chan struct{}) {
	delete(manager.openings, accountID)
	manager.openingSet.Delete(accountID)
	close(opening)
	manager.notifyScheduling()
}

// Add 注册新账户的 WAA worker 配置
func (manager *accountWorkerManager) Add(account *aistudio.Account) error {
	if account == nil {
		return fmt.Errorf("账户未初始化")
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.closed {
		return fmt.Errorf("WAA worker manager 已关闭")
	}
	if _, exists := manager.accounts[account.ID]; exists {
		return fmt.Errorf("WAA worker 账户已存在: %s", account.ID)
	}
	manager.accounts[account.ID] = manager.newAccountWorker(account)
	return nil
}

// Reset 关闭账户当前 WAA worker 并保留重建配置
func (manager *accountWorkerManager) Reset(accountID string) error {
	manager.mu.RLock()
	account := manager.accounts[accountID]
	manager.mu.RUnlock()
	if account == nil {
		return fmt.Errorf("WAA worker 账户不存在: %s", accountID)
	}
	account.startupMu.Lock()
	defer account.startupMu.Unlock()
	account.mu.Lock()
	defer account.mu.Unlock()
	if account.worker == nil && account.cleanupWorker == nil && account.runtimeLease == nil && account.cleanupLease == nil {
		return nil
	}
	startedAt := time.Now()
	pid := 0
	if account.worker != nil {
		pid = account.worker.State().PID
	} else if account.cleanupWorker != nil {
		pid = account.cleanupWorker.State().PID
	}
	manager.requests.log(account.label, "INFO", fmt.Sprintf("WAA Worker 停止 | PID=%d", pid))
	err := manager.closeAccountWorker(account)
	if err != nil {
		manager.requests.log(account.label, "ERROR", fmt.Sprintf(
			"WAA Worker 停止失败 | PID=%d | 耗时=%s | 错误=%s",
			pid, time.Since(startedAt).Round(time.Millisecond), strings.TrimSpace(err.Error()),
		))
		return err
	}
	manager.requests.log(account.label, "INFO", fmt.Sprintf(
		"WAA Worker 已停止 | PID=%d | 耗时=%s",
		pid, time.Since(startedAt).Round(time.Millisecond),
	))
	return err
}

// WorkerGeneration 返回账户当前 Worker 版本号
func (manager *accountWorkerManager) WorkerGeneration(accountID string) uint64 {
	manager.mu.RLock()
	account := manager.accounts[accountID]
	manager.mu.RUnlock()
	if account == nil {
		return 0
	}
	return account.generation.Load()
}

// ResetIfGeneration 等待活动请求结束后仅关闭产生当前失败的 Worker
func (manager *accountWorkerManager) ResetIfGeneration(ctx context.Context, accountID string, generation uint64) (reset bool, err error) {
	lease, err := manager.pool.AcquireAccount(ctx, accountID)
	if err != nil {
		return false, err
	}
	defer func() { err = errors.Join(err, lease.Release()) }()
	manager.mu.RLock()
	account := manager.accounts[accountID]
	manager.mu.RUnlock()
	if account == nil {
		return false, fmt.Errorf("WAA worker 账户不存在: %s", accountID)
	}
	account.startupMu.Lock()
	defer account.startupMu.Unlock()
	account.mu.Lock()
	defer account.mu.Unlock()
	if account.generation.Load() != generation {
		return false, nil
	}
	if account.worker == nil && account.cleanupWorker == nil && account.runtimeLease == nil && account.cleanupLease == nil {
		return false, nil
	}
	startedAt := time.Now()
	pid := 0
	if account.worker != nil {
		pid = account.worker.State().PID
	} else if account.cleanupWorker != nil {
		pid = account.cleanupWorker.State().PID
	}
	manager.requests.log(account.label, "INFO", fmt.Sprintf("WAA Worker 停止 | PID=%d", pid))
	if err := manager.closeAccountWorker(account); err != nil {
		manager.requests.log(account.label, "ERROR", fmt.Sprintf(
			"WAA Worker 停止失败 | PID=%d | 耗时=%s | 错误=%s",
			pid, time.Since(startedAt).Round(time.Millisecond), strings.TrimSpace(err.Error()),
		))
		return false, err
	}
	manager.requests.log(account.label, "INFO", fmt.Sprintf(
		"WAA Worker 已停止 | PID=%d | 耗时=%s",
		pid, time.Since(startedAt).Round(time.Millisecond),
	))
	return true, nil
}

// prepareUpdate 关闭旧 Worker 并保持新配置待发布
func (manager *accountWorkerManager) prepareUpdate(
	account *aistudio.Account,
	config aistudio.AccountConfig,
) (*accountWorkerUpdate, error) {
	if account == nil {
		return nil, fmt.Errorf("账户未初始化")
	}
	manager.mu.RLock()
	worker := manager.accounts[account.ID]
	manager.mu.RUnlock()
	if worker == nil {
		return nil, fmt.Errorf("WAA worker 账户不存在: %s", account.ID)
	}
	update := &accountWorkerUpdate{
		account: worker, config: manager.workerConfigFor(account, config), label: config.Label, pending: true,
	}
	worker.startupMu.Lock()
	worker.mu.Lock()
	if worker.worker != nil || worker.cleanupWorker != nil || worker.runtimeLease != nil || worker.cleanupLease != nil {
		if err := manager.closeAccountWorker(worker); err != nil {
			worker.mu.Unlock()
			worker.startupMu.Unlock()
			return nil, err
		}
	}
	worker.mu.Unlock()
	return update, nil
}

// Commit 发布已准备的 Worker 配置
func (update *accountWorkerUpdate) Commit() {
	if update == nil || !update.pending {
		return
	}
	update.account.mu.Lock()
	update.account.config = update.config
	update.account.label = update.label
	update.account.mu.Unlock()
	update.pending = false
	update.account.startupMu.Unlock()
}

// Discard 放弃已准备的 Worker 配置
func (update *accountWorkerUpdate) Discard() {
	if update == nil || !update.pending {
		return
	}
	update.pending = false
	update.account.startupMu.Unlock()
}

// ResetAll 关闭全部账户当前 worker 并保留后续按需重建能力
func (manager *accountWorkerManager) ResetAll() error {
	manager.cancelBackgroundStartups()
	manager.mu.RLock()
	accountIDs := make([]string, 0, len(manager.accounts))
	for accountID := range manager.accounts {
		accountIDs = append(accountIDs, accountID)
	}
	manager.mu.RUnlock()
	resetResults := make(chan error, len(accountIDs))
	var resets sync.WaitGroup
	for _, accountID := range accountIDs {
		resets.Add(1)
		go func(accountID string) {
			defer resets.Done()
			resetResults <- manager.Reset(accountID)
		}(accountID)
	}
	resets.Wait()
	close(resetResults)
	var resetErrors []error
	for resetErr := range resetResults {
		resetErrors = append(resetErrors, resetErr)
	}
	return errors.Join(resetErrors...)
}

// Remove 删除账户的 WAA worker 配置
func (manager *accountWorkerManager) Remove(accountID string) error {
	manager.rebalanceMu.Lock()
	manager.mu.Lock()
	account := manager.accounts[accountID]
	if account == nil {
		manager.mu.Unlock()
		manager.rebalanceMu.Unlock()
		return fmt.Errorf("WAA worker 账户不存在: %s", accountID)
	}
	delete(manager.accounts, accountID)
	manager.mu.Unlock()
	manager.rebalanceMu.Unlock()

	account.startupMu.Lock()
	defer account.startupMu.Unlock()
	account.mu.Lock()
	defer account.mu.Unlock()
	return manager.closeAccountWorker(account)
}

func (manager *accountWorkerManager) newAccountWorker(account *aistudio.Account) *accountWorker {
	return &accountWorker{id: account.ID, label: account.Config.Label, config: manager.workerConfig(account)}
}

func (manager *accountWorkerManager) workerConfig(account *aistudio.Account) camoufoxnative.Options {
	return manager.workerConfigFor(account, account.Config)
}

func (manager *accountWorkerManager) workerConfigFor(
	account *aistudio.Account,
	config aistudio.AccountConfig,
) camoufoxnative.Options {
	proxy := strings.TrimSpace(config.Proxy)
	if proxy == "" {
		proxy = strings.TrimSpace(manager.globalProxy)
	}
	return camoufoxnative.Options{
		ExecutablePath:   manager.camoufox,
		StorageStatePath: account.StoragePath,
		Locale:           config.Locale,
		Timezone:         config.Timezone,
		Proxy:            proxy,
		Headless:         true,
		TemporaryChat:    manager.temporaryChat.Load(),
	}
}

// WarmAccountIDs 返回当前驻留的健康 WAA worker
func (manager *accountWorkerManager) WarmAccountIDs() []string {
	manager.mu.RLock()
	accounts := make([]*accountWorker, 0, len(manager.accounts))
	for _, account := range manager.accounts {
		accounts = append(accounts, account)
	}
	manager.mu.RUnlock()
	warm := make([]string, 0, len(accounts))
	for _, account := range accounts {
		if account.warm.Load() {
			warm = append(warm, account.id)
		}
	}
	return warm
}

type workerOccupancy struct {
	accountIDs []string
	slots      int
}

// occupiedWorkers 返回仍持有进程或运行锁的账户与容量槽位
func (manager *accountWorkerManager) occupiedWorkers() workerOccupancy {
	manager.mu.RLock()
	accounts := make([]*accountWorker, 0, len(manager.accounts))
	for _, account := range manager.accounts {
		accounts = append(accounts, account)
	}
	manager.mu.RUnlock()
	occupied := workerOccupancy{accountIDs: make([]string, 0, len(accounts))}
	for _, account := range accounts {
		// 正在生成 proof 的账户必然有 Worker，按占用 1 个槽位计，不排队等锁
		if !account.mu.TryLock() {
			occupied.accountIDs = append(occupied.accountIDs, account.id)
			occupied.slots++
			continue
		}
		if account.worker != nil || account.cleanupWorker != nil || account.runtimeLease != nil || account.cleanupLease != nil {
			occupied.accountIDs = append(occupied.accountIDs, account.id)
		}
		if account.worker != nil {
			occupied.slots++
		}
		if account.cleanupWorker != nil {
			occupied.slots++
		}
		if account.worker == nil && account.cleanupWorker == nil && account.runtimeLease != nil {
			occupied.slots++
		}
		if account.cleanupWorker == nil && account.cleanupLease != nil {
			occupied.slots++
		}
		account.mu.Unlock()
	}
	return occupied
}

// ReadyWarmAccountIDs 返回可生成 proof 的预热账户
func (manager *accountWorkerManager) ReadyWarmAccountIDs() []string {
	manager.mu.RLock()
	accounts := make([]*accountWorker, 0, len(manager.accounts))
	for _, account := range manager.accounts {
		accounts = append(accounts, account)
	}
	manager.mu.RUnlock()
	warm := make([]string, 0, len(accounts))
	for _, account := range accounts {
		if !account.warm.Load() {
			continue
		}
		// 账户锁被占用说明该 Worker 正在为请求生成 proof（每次 1～5 秒），Worker 必然可用。
		// 这里不排队等锁：每个请求分配账号时都会调用本函数，逐个等待会让调度随并发线性变慢
		if !account.mu.TryLock() {
			warm = append(warm, account.id)
			continue
		}
		worker := account.worker
		matches := worker != nil
		if matches {
			phase := worker.State().Phase
			matches = phase == aistudio.WorkerReady || phase == aistudio.WorkerBusy
		}
		account.mu.Unlock()
		if matches {
			warm = append(warm, account.id)
		}
	}
	return warm
}

func (manager *accountWorkerManager) coldAccounts(accountIDs []string) []string {
	manager.mu.RLock()
	defer manager.mu.RUnlock()
	cold := make([]string, 0, len(accountIDs))
	for _, accountID := range accountIDs {
		account := manager.accounts[accountID]
		if account != nil && !account.warm.Load() {
			cold = append(cold, accountID)
		}
	}
	return cold
}

// PrewarmTarget 返回当前配置需要预热的账户数
func (manager *accountWorkerManager) PrewarmTarget() int {
	return min(manager.warmTargetValue(), manager.bootstrapSummary().Available)
}

func (manager *accountWorkerManager) classifyBootstrapCandidates(
	ctx context.Context,
	warm []string,
) (aistudio.AccountCandidateGroups, error) {
	combined := aistudio.AccountCandidateGroups{}
	seenWarmReady := make(map[string]struct{})
	seenWarmAvailable := make(map[string]struct{})
	seenWarmBusy := make(map[string]struct{})
	seenStandbyReady := make(map[string]struct{})
	seenStandbyBusy := make(map[string]struct{})
	appendUnique := func(target *[]string, seen map[string]struct{}, values []string) {
		for _, value := range values {
			if _, exists := seen[value]; exists {
				continue
			}
			seen[value] = struct{}{}
			*target = append(*target, value)
		}
	}
	modelIDs := manager.bootstrapSummary().ModelIDs
	var matched bool
	for _, modelID := range modelIDs {
		groups, err := manager.pool.ClassifyCandidatesCached(aistudio.AccountSelection{
			ModelID: modelID, Method: "generateContent",
		}, warm)
		if errors.Is(err, aistudio.ErrModelNotFound) {
			continue
		}
		if err != nil {
			return aistudio.AccountCandidateGroups{}, err
		}
		matched = true
		combined.Eligible = combined.Eligible || groups.Eligible
		if combined.EarliestCooldown.IsZero() || !groups.EarliestCooldown.IsZero() && groups.EarliestCooldown.Before(combined.EarliestCooldown) {
			combined.EarliestCooldown = groups.EarliestCooldown
		}
		appendUnique(&combined.WarmReady, seenWarmReady, groups.WarmReady)
		appendUnique(&combined.WarmAvailable, seenWarmAvailable, groups.WarmAvailable)
		appendUnique(&combined.WarmBusy, seenWarmBusy, groups.WarmBusy)
		appendUnique(&combined.StandbyReady, seenStandbyReady, groups.StandbyReady)
		appendUnique(&combined.StandbyBusy, seenStandbyBusy, groups.StandbyBusy)
	}
	if !matched {
		return aistudio.AccountCandidateGroups{}, aistudio.ErrNoEligibleAccount
	}
	combined.StandbyReady = manager.pool.PreferWarmPool(combined.StandbyReady)
	combined.StandbyBusy = manager.pool.PreferWarmPool(combined.StandbyBusy)
	return combined, nil
}

// WorkerFailed 返回账户驻留 worker 是否已经失败
func (manager *accountWorkerManager) WorkerFailed(accountID string) bool {
	manager.mu.RLock()
	account := manager.accounts[accountID]
	manager.mu.RUnlock()
	if account == nil {
		return false
	}
	account.mu.Lock()
	defer account.mu.Unlock()
	return account.worker != nil && account.worker.State().Phase == aistudio.WorkerFailed
}

// Worker 在活动上限内返回账户的通用 WAA preparer
func (manager *accountWorkerManager) Worker(ctx context.Context, accountID string, modelID string) (aistudio.ProtectedPreparer, error) {
	return manager.ensureWorker(ctx, accountID, modelID, true)
}

func (manager *accountWorkerManager) readyWorker(accountID string, bootstrapModel string) (aistudio.ProtectedPreparer, bool, error) {
	preparer, ready, _, err := manager.checkReadyWorker(accountID, bootstrapModel, true)
	return preparer, ready, err
}

// checkReadyWorker 检查账户是否已有可用 Worker。wait 为 false 时不等待账户锁：
// 锁正被占用（如同账户的请求正在生成 WAA proof）时立即返回 busy
func (manager *accountWorkerManager) checkReadyWorker(
	accountID string,
	bootstrapModel string,
	wait bool,
) (aistudio.ProtectedPreparer, bool, bool, error) {
	manager.mu.RLock()
	if manager.closed {
		manager.mu.RUnlock()
		return nil, false, false, fmt.Errorf("WAA worker manager 已关闭")
	}
	account := manager.accounts[accountID]
	manager.mu.RUnlock()
	if account == nil {
		return nil, false, false, fmt.Errorf("账户不存在: %s", accountID)
	}
	if wait {
		account.mu.Lock()
	} else if !account.mu.TryLock() {
		return nil, false, true, nil
	}
	defer account.mu.Unlock()
	manager.mu.RLock()
	active := !manager.closed && manager.accounts[accountID] == account
	manager.mu.RUnlock()
	if !active {
		return nil, false, false, fmt.Errorf("账户不存在: %s", accountID)
	}
	if accountWorkerCleanupPending(account) {
		return nil, false, false, fmt.Errorf("%w: %s", errAccountWorkerCleanupPending, accountID)
	}
	if account.worker != nil {
		phase := account.worker.State().Phase
		if (phase == aistudio.WorkerReady || phase == aistudio.WorkerBusy) && account.bootstrapModel == bootstrapModel {
			return &accountWorkerPreparer{
				account: account, worker: account.worker, bootstrapModel: account.bootstrapModel,
			}, true, false, nil
		}
	}
	return nil, false, false, nil
}

func (manager *accountWorkerManager) startReservedWorker(
	ctx context.Context,
	accountID string,
	bootstrapModel string,
) (*accountWorkerPreparer, error) {
	manager.mu.RLock()
	if manager.closed {
		manager.mu.RUnlock()
		return nil, fmt.Errorf("WAA worker manager 已关闭")
	}
	account := manager.accounts[accountID]
	manager.mu.RUnlock()
	if account == nil {
		return nil, fmt.Errorf("账户不存在: %s", accountID)
	}
	account.startupMu.Lock()
	account.mu.Lock()
	manager.mu.RLock()
	active := !manager.closed && manager.accounts[accountID] == account
	manager.mu.RUnlock()
	if !active {
		account.mu.Unlock()
		account.startupMu.Unlock()
		return nil, fmt.Errorf("账户不存在: %s", accountID)
	}
	if accountWorkerCleanupPending(account) {
		account.mu.Unlock()
		account.startupMu.Unlock()
		return nil, fmt.Errorf("%w: %s", errAccountWorkerCleanupPending, accountID)
	}
	if account.worker != nil {
		phase := account.worker.State().Phase
		if (phase == aistudio.WorkerReady || phase == aistudio.WorkerBusy) && account.bootstrapModel == bootstrapModel {
			preparer := &accountWorkerPreparer{
				account: account, worker: account.worker, bootstrapModel: account.bootstrapModel,
			}
			account.mu.Unlock()
			account.startupMu.Unlock()
			return preparer, nil
		}
	}
	startedAt := time.Now()
	label := account.label
	options := account.config
	runtimeLease := account.runtimeLease
	ownsLease := false
	account.mu.Unlock()
	if runtimeLease == nil {
		var err error
		runtimeLease, err = aistudio.AcquireAccountRuntimeLease(account.id)
		if errors.Is(err, aistudio.ErrAccountLeased) {
			account.startupMu.Unlock()
			return nil, manager.markRuntimeBusy(account, err)
		}
		if err != nil {
			account.startupMu.Unlock()
			manager.requests.log(label, "ERROR", fmt.Sprintf(
				"WAA Worker 启动失败 | 耗时=%s | 错误=%s",
				time.Since(startedAt).Round(time.Millisecond), strings.TrimSpace(err.Error()),
			))
			return nil, &accountWorkerInitError{err: err}
		}
		ownsLease = true
		manager.clearRuntimeBusy(account)
	}
	manager.requests.log(label, "INFO", "WAA Worker 启动 | 1/7 | 初始化页面 | 页面模型="+bootstrapModel)
	initCtx, cancel := context.WithTimeout(ctx, time.Duration(manager.initTimeout.Load()))
	options.Model = bootstrapModel
	options.StartupProgress = func(stage camoufoxnative.StartupStage) {
		step, message := workerStartupProgress(stage)
		manager.requests.log(label, "INFO", fmt.Sprintf("WAA Worker 启动 | %d/7 | %s", step, message))
	}
	worker, initErr := newWAAWorker(initCtx, account.id, options)
	cancel()
	if initErr != nil {
		if ownsLease {
			_ = runtimeLease.Release()
		}
		account.startupMu.Unlock()
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		manager.requests.log(label, "ERROR", fmt.Sprintf(
			"WAA Worker 启动失败 | 页面模型=%s | 耗时=%s | 错误=%s",
			bootstrapModel, time.Since(startedAt).Round(time.Millisecond), strings.TrimSpace(initErr.Error()),
		))
		return nil, &accountWorkerInitError{err: initErr}
	}
	return &accountWorkerPreparer{
		account: account, worker: worker, bootstrapModel: bootstrapModel, manager: manager,
		runtimeLease: runtimeLease, ownsLease: ownsLease, startedAt: startedAt, pending: true,
	}, nil
}

// activateAccountWorker 将已启动 Worker 发布到热池
func activateAccountWorker(preparer *accountWorkerPreparer) error {
	if !preparer.pending {
		return nil
	}
	preparer.account.mu.Lock()
	preparer.manager.mu.RLock()
	active := !preparer.manager.closed && preparer.manager.accounts[preparer.account.id] == preparer.account
	preparer.manager.mu.RUnlock()
	if !active {
		preparer.account.mu.Unlock()
		return errors.Join(fmt.Errorf("账户不存在: %s", preparer.account.id), discardAccountWorker(preparer))
	}
	oldWorker := preparer.account.worker
	if oldWorker != nil {
		if closeErr := oldWorker.Close(); closeErr != nil {
			label := preparer.account.label
			preparer.account.mu.Unlock()
			cleanupErr := discardAccountWorker(preparer)
			preparer.manager.requests.log(label, "ERROR", fmt.Sprintf(
				"WAA Worker 旧实例停止失败 | 错误=%s", strings.TrimSpace(closeErr.Error()),
			))
			return errors.Join(closeErr, cleanupErr)
		}
	}
	preparer.account.worker = preparer.worker
	preparer.account.bootstrapModel = preparer.bootstrapModel
	if preparer.ownsLease {
		preparer.account.runtimeLease = preparer.runtimeLease
	}
	if oldWorker != nil {
		preparer.account.generation.Add(1)
	}
	preparer.account.warm.Store(true)
	preparer.account.readyAt.Store(time.Now().UnixNano())
	label := preparer.account.label
	preparer.account.mu.Unlock()
	preparer.manager.notifyScheduling()
	preparer.manager.requests.log(label, "INFO", fmt.Sprintf(
		"WAA Worker 就绪 | 页面模型=%s | PID=%d | 耗时=%s",
		preparer.bootstrapModel, preparer.worker.State().PID, time.Since(preparer.startedAt).Round(time.Millisecond),
	))
	preparer.pending = false
	preparer.account.startupMu.Unlock()
	return nil
}

// discardAccountWorker 关闭尚未发布的 Worker
func discardAccountWorker(preparer *accountWorkerPreparer) error {
	if !preparer.pending {
		return nil
	}
	workerErr := preparer.worker.Close()
	var leaseErr error
	if workerErr == nil && preparer.ownsLease {
		leaseErr = preparer.runtimeLease.Release()
	}
	cleanupErr := errors.Join(workerErr, leaseErr)
	if cleanupErr != nil {
		preparer.account.mu.Lock()
		if workerErr != nil {
			preparer.account.cleanupWorker = preparer.worker
		}
		if preparer.ownsLease {
			preparer.account.cleanupLease = preparer.runtimeLease
		}
		preparer.account.mu.Unlock()
	}
	preparer.pending = false
	preparer.account.startupMu.Unlock()
	return cleanupErr
}

func accountWorkerCleanupPending(account *accountWorker) bool {
	if account.cleanupWorker != nil || account.cleanupLease != nil || account.worker == nil && account.runtimeLease != nil {
		return true
	}
	return account.worker != nil && account.worker.State().Phase == aistudio.WorkerClosing
}

func workerStartupProgress(stage camoufoxnative.StartupStage) (int, string) {
	switch stage {
	case camoufoxnative.StartupPreparingBrowser:
		return 2, "准备浏览器配置"
	case camoufoxnative.StartupLaunchingBrowser:
		return 3, "启动 Camoufox"
	case camoufoxnative.StartupConnectingBiDi:
		return 4, "连接 WebDriver BiDi"
	case camoufoxnative.StartupLoadingAIStudio:
		return 5, "载入 AI Studio"
	case camoufoxnative.StartupLocatingWAA:
		return 6, "定位 WAA 服务"
	case camoufoxnative.StartupBootstrappingWAA:
		return 7, "执行 WAA Bootstrap"
	}
	panic(fmt.Sprintf("未知 WAA Worker 启动阶段: %s", stage))
}

func (manager *accountWorkerManager) idleWarmVictim(excludeID string) string {
	return manager.idleWarmVictimFor(excludeID, "", false)
}

// reserveVictim 标记已被某次替换选中的旧 Worker，避免并行替换选中同一个
func (manager *accountWorkerManager) reserveVictim(accountID string) {
	manager.victimMu.Lock()
	manager.victims[accountID] = struct{}{}
	manager.victimMu.Unlock()
}

// releaseVictim 解除旧 Worker 的替换标记
func (manager *accountWorkerManager) releaseVictim(accountID string) {
	manager.victimMu.Lock()
	delete(manager.victims, accountID)
	manager.victimMu.Unlock()
}

func (manager *accountWorkerManager) victimReserved(accountID string) bool {
	manager.victimMu.Lock()
	defer manager.victimMu.Unlock()
	_, reserved := manager.victims[accountID]
	return reserved
}

// idleWarmVictimFor 选择空闲热 Worker，优先冷却中的账户，其次最久未用；coolingOnly 时只返回冷却中的账户
func (manager *accountWorkerManager) idleWarmVictimFor(excludeID string, modelID string, coolingOnly bool) string {
	warm := manager.WarmAccountIDs()
	var selected string
	var selectedUsed time.Time
	selectedCooling := false
	for _, accountID := range warm {
		if accountID == excludeID || manager.victimReserved(accountID) {
			continue
		}
		manager.mu.RLock()
		account := manager.accounts[accountID]
		manager.mu.RUnlock()
		if account == nil {
			continue
		}
		// 正在启动或替换 Worker 的账户不作为淘汰对象：启动流程持有该账户的 startupMu 之后还要拿 rebalanceMu，
		// 而淘汰在持有 rebalanceMu 时要拿 startupMu，两边互相等待会让全局调度永久停住
		if _, opening := manager.openingSet.Load(accountID); opening {
			continue
		}
		if !account.startupMu.TryLock() {
			continue
		}
		account.startupMu.Unlock()
		// 锁被占用说明正在处理请求，不可能是空闲 Worker，直接跳过而不是排队等待
		if !account.mu.TryLock() {
			continue
		}
		cleanupPending := accountWorkerCleanupPending(account)
		account.mu.Unlock()
		if cleanupPending {
			continue
		}
		busy, lastUsed := manager.pool.Activity(accountID)
		if busy {
			continue
		}
		cooling := manager.pool.AccountCoolingDown(accountID, modelID)
		if coolingOnly && !cooling {
			continue
		}
		if selected == "" || cooling && !selectedCooling || cooling == selectedCooling && lastUsed.Before(selectedUsed) {
			selected = accountID
			selectedUsed = lastUsed
			selectedCooling = cooling
		}
	}
	return selected
}

func (manager *accountWorkerManager) evictIdleWorker(ctx context.Context, accountID string) (bool, error) {
	evictionCtx, cancel := context.WithTimeout(ctx, workerEvictionTimeout)
	defer cancel()
	lease, err := manager.pool.AcquireAccount(evictionCtx, accountID)
	if err != nil {
		if ctx.Err() == nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
			return false, nil
		}
		return false, err
	}
	if err := ctx.Err(); err != nil {
		return false, errors.Join(err, lease.Release())
	}
	resetErr := manager.Reset(accountID)
	releaseErr := lease.Release()
	return true, errors.Join(resetErr, releaseErr)
}

func (manager *accountWorkerManager) ensureWorker(
	ctx context.Context,
	accountID string,
	modelID string,
	waitForOpening bool,
) (aistudio.ProtectedPreparer, error) {
	bootstrapModel, err := manager.pool.BootstrapModel(accountID)
	if err != nil {
		return nil, err
	}
	workerCtx, cancel := context.WithCancel(ctx)
	stopLifecycle := context.AfterFunc(manager.lifecycle, cancel)
	defer func() {
		stopLifecycle()
		cancel()
	}()
	ctx = workerCtx
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		// 快路径：不持有全局 rebalanceMu，直接检查账户现成的 Worker。检查要等该账户的锁，
		// 同账户的其他请求正在生成 WAA proof 时可能等几秒；原先持有 rebalanceMu 等待，
		// 全系统所有请求获取 Worker 都会跟着排队
		preparer, ready, err := manager.readyWorker(accountID, bootstrapModel)
		if err != nil {
			return nil, err
		}
		if ready {
			return preparer, nil
		}
		manager.rebalanceMu.Lock()
		if opening := manager.openings[accountID]; opening != nil {
			manager.rebalanceMu.Unlock()
			if !waitForOpening {
				return nil, errAccountWorkerOpening
			}
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-opening:
				continue
			}
		}
		// 持有 rebalanceMu 时不等待账户锁：锁被占用就放开全局锁，回到快路径里等待
		preparer, ready, busy, err := manager.checkReadyWorker(accountID, bootstrapModel, false)
		if busy {
			manager.rebalanceMu.Unlock()
			continue
		}
		if err != nil {
			manager.rebalanceMu.Unlock()
			return nil, err
		}
		if ready {
			manager.rebalanceMu.Unlock()
			return preparer, nil
		}
		occupancy := manager.occupiedWorkers()
		occupied := make(map[string]struct{}, len(occupancy.accountIDs)+len(manager.openings))
		for _, occupiedAccountID := range occupancy.accountIDs {
			occupied[occupiedAccountID] = struct{}{}
		}
		for openingAccountID := range manager.openings {
			occupied[openingAccountID] = struct{}{}
			occupancy.slots++
		}
		_, replacing := occupied[accountID]
		if replacing || occupancy.slots < manager.maxActiveValue() {
			opening := make(chan struct{})
			manager.openings[accountID] = opening
			manager.openingSet.Store(accountID, struct{}{})
			manager.rebalanceMu.Unlock()
			preparer, err := manager.startReservedWorker(ctx, accountID, bootstrapModel)
			manager.rebalanceMu.Lock()
			if err == nil {
				if ctxErr := ctx.Err(); ctxErr != nil {
					err = errors.Join(ctxErr, discardAccountWorker(preparer))
				} else {
					err = activateAccountWorker(preparer)
				}
			}
			manager.finishOpening(accountID, opening)
			warmCount := len(manager.WarmAccountIDs())
			manager.rebalanceMu.Unlock()
			if err == nil && warmCount > manager.warmTargetValue() {
				manager.requests.log("service", "INFO", fmt.Sprintf(
					"WAA Worker 按需扩容 | Worker=%d/%d", warmCount, manager.maxActiveValue(),
				))
			}
			return preparer, err
		}
		victim := ""
		if len(manager.openings) == 0 {
			victim = manager.idleWarmVictimFor(accountID, modelID, false)
		} else {
			victim = manager.idleWarmVictimFor(accountID, modelID, true)
		}
		if victim == "" {
			manager.rebalanceMu.Unlock()
			if !waitForOpening {
				return nil, errAccountWorkerCapacity
			}
			if err := waitWarmCandidate(ctx, 100*time.Millisecond); err != nil {
				return nil, err
			}
			continue
		}
		manager.reserveVictim(victim)
		defer func() { manager.releaseVictim(victim) }()
		opening := make(chan struct{})
		manager.openings[accountID] = opening
		manager.openingSet.Store(accountID, struct{}{})
		manager.rebalanceMu.Unlock()
		pending, startErr := manager.startReservedWorker(ctx, accountID, bootstrapModel)
		if startErr != nil {
			manager.rebalanceMu.Lock()
			manager.finishOpening(accountID, opening)
			manager.rebalanceMu.Unlock()
			return nil, startErr
		}
		for {
			manager.rebalanceMu.Lock()
			if ctxErr := ctx.Err(); ctxErr != nil {
				discardErr := discardAccountWorker(pending)
				manager.finishOpening(accountID, opening)
				manager.rebalanceMu.Unlock()
				return nil, errors.Join(ctxErr, discardErr)
			}
			if victim == "" {
				victim = manager.idleWarmVictimFor(accountID, modelID, false)
				if victim != "" {
					manager.reserveVictim(victim)
				}
			}
			if victim == "" {
				manager.rebalanceMu.Unlock()
				if err := waitWarmCandidate(ctx, 100*time.Millisecond); err != nil {
					manager.rebalanceMu.Lock()
					discardErr := discardAccountWorker(pending)
					manager.finishOpening(accountID, opening)
					manager.rebalanceMu.Unlock()
					return nil, errors.Join(err, discardErr)
				}
				continue
			}
			evicted, evictionErr := manager.evictIdleWorker(ctx, victim)
			if evictionErr == nil && evicted {
				activationErr := activateAccountWorker(pending)
				manager.finishOpening(accountID, opening)
				warmCount := len(manager.WarmAccountIDs())
				manager.rebalanceMu.Unlock()
				if activationErr != nil {
					return nil, activationErr
				}
				if warmCount > manager.warmTargetValue() {
					manager.requests.log("service", "INFO", fmt.Sprintf(
						"WAA Worker 按需替换 | Worker=%d/%d", warmCount, manager.maxActiveValue(),
					))
				}
				return pending, nil
			}
			if evictionErr != nil || ctx.Err() != nil {
				discardErr := discardAccountWorker(pending)
				manager.finishOpening(accountID, opening)
				manager.rebalanceMu.Unlock()
				return nil, errors.Join(evictionErr, ctx.Err(), discardErr)
			}
			manager.releaseVictim(victim)
			victim = ""
			manager.rebalanceMu.Unlock()
		}
	}
}

func (manager *accountWorkerManager) promote(ctx context.Context, accountID string, modelID string) (aistudio.ProtectedPreparer, error) {
	return manager.ensureWorker(ctx, accountID, modelID, false)
}

func (manager *accountWorkerManager) withoutOpening(accountIDs []string) ([]string, bool) {
	manager.rebalanceMu.Lock()
	defer manager.rebalanceMu.Unlock()
	result := make([]string, 0, len(accountIDs))
	pending := false
	for _, accountID := range accountIDs {
		if manager.openings[accountID] != nil {
			pending = true
			continue
		}
		result = append(result, accountID)
	}
	return result, pending
}

// StartPrewarm 启动有界预热并在当前可用账户完成后返回
func (manager *accountWorkerManager) StartPrewarm(ctx context.Context) <-chan error {
	first := make(chan error, 1)
	manager.mu.RLock()
	closed := manager.closed
	manager.mu.RUnlock()
	if closed {
		first <- fmt.Errorf("WAA worker manager 已关闭")
		close(first)
		return first
	}
	if !manager.fillMu.TryLock() {
		if len(manager.WarmAccountIDs()) > 0 {
			first <- nil
		} else {
			first <- fmt.Errorf("WAA 预热已在进行")
		}
		close(first)
		return first
	}
	fillContext, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(manager.lifecycle, cancel)
	go manager.fillWarm(fillContext, first, func() {
		stop()
		cancel()
	})
	return first
}

// fillWarm 以滑动窗口并发预热 Worker：任意一个完成后立刻补上下一个，不再等整批结束，
// 单个启动慢的账户不会拖住其他并发槽位；常驻数与并发数运行中调整后下一轮即按新值执行
func (manager *accountWorkerManager) fillWarm(ctx context.Context, first chan<- error, cleanup func()) {
	startedAt := time.Now()
	defer cleanup()
	defer manager.fillMu.Unlock()
	defer close(first)
	notified := false
	notify := func(err error) {
		if notified {
			return
		}
		notified = true
		first <- err
	}
	var failures []error
	var classifyErr error
	failedAccounts := make(map[string]struct{})
	inflight := make(map[string]struct{})
	results := make(chan warmResult, warmResultBuffer)
	launched := 0
	capacityFull := false
	// openingElsewhere 记录已由其他路径（如请求按需扩容）在启动的账户，本轮不再重复尝试，避免空转
	openingElsewhere := make(map[string]struct{})
	var busySince time.Time
	manager.fillActive.Store(true)
	manager.fillStartedAt.Store(startedAt.UnixNano())
	manager.fillLaunched.Store(0)
	defer func() {
		manager.fillActive.Store(false)
		manager.fillInflight.Store(0)
	}()
	handle := func(result warmResult) {
		delete(inflight, result.accountID)
		manager.fillInflight.Store(int32(len(inflight)))
		if result.err == nil {
			manager.clearWarmFailure(result.accountID)
			notify(nil)
			return
		}
		if ctx.Err() != nil {
			return
		}
		// 槽位已满或该账户已在启动：不算失败，本轮不再启动新的，交给 keepWarm 稍后重试
		if errors.Is(result.err, errAccountWorkerCapacity) {
			capacityFull = true
			return
		}
		if errors.Is(result.err, errAccountWorkerOpening) {
			openingElsewhere[result.accountID] = struct{}{}
			return
		}
		failures = append(failures, fmt.Errorf("预热账户 %s: %w", result.accountID, result.err))
		failedAccounts[result.accountID] = struct{}{}
		manager.noteWarmFailure(result.accountID)
	}
	for {
		if err := ctx.Err(); err != nil {
			for len(inflight) > 0 {
				handle(<-results)
			}
			if !notified {
				notify(errors.Join(append(failures, err)...))
			}
			return
		}
		manager.fillLoopAt.Store(time.Now().UnixNano())
		manager.fillInflight.Store(int32(len(inflight)))
		warm := manager.WarmAccountIDs()
		target := manager.warmTargetValue()
		if len(warm) >= target && len(inflight) == 0 {
			if launched > 0 {
				manager.requests.log("service", "INFO", fmt.Sprintf(
					"WAA Worker 预热完成 | Worker=%d/%d | 耗时=%s",
					len(warm), manager.PrewarmTarget(), time.Since(startedAt).Round(time.Millisecond),
				))
			}
			notify(nil)
			return
		}
		slots := min(manager.warmConcurrencyValue()-len(inflight), target-len(warm)-len(inflight))
		if capacityFull {
			slots = 0
		}
		pendingBusy := false
		readyCount := 0
		var runtimeBusyErr error
		if slots > 0 {
			groups, err := manager.classifyBootstrapCandidates(ctx, warm)
			if err != nil {
				classifyErr = err
			} else {
				classifyErr = nil
				skipped := make(map[string]struct{}, len(failedAccounts)+len(inflight))
				for accountID := range failedAccounts {
					skipped[accountID] = struct{}{}
				}
				for accountID := range inflight {
					skipped[accountID] = struct{}{}
				}
				for accountID := range manager.recentWarmFailures(time.Now()) {
					skipped[accountID] = struct{}{}
				}
				for accountID := range openingElsewhere {
					skipped[accountID] = struct{}{}
				}
				for _, accountID := range manager.OpeningAccountIDs() {
					skipped[accountID] = struct{}{}
				}
				ready := excludeAccountIDs(groups.StandbyReady, skipped)
				busy := excludeAccountIDs(groups.StandbyBusy, skipped)
				var readyBusyErr, standbyBusyErr error
				ready, readyBusyErr = manager.runtimeAvailable(ready)
				busy, standbyBusyErr = manager.runtimeAvailable(busy)
				runtimeBusyErr = errors.Join(readyBusyErr, standbyBusyErr)
				pendingBusy = len(busy) > 0
				ready = manager.preferUncooled(ready)
				readyCount = len(ready)
				started := ready[:min(slots, len(ready))]
				for _, accountID := range started {
					inflight[accountID] = struct{}{}
					launched++
					go func(accountID string) {
						// 不等待槽位：槽位满时立即返回；单个任务限时，避免个别账户拖住整轮预热
						taskCtx, taskCancel := context.WithTimeout(
							ctx, time.Duration(manager.initTimeout.Load())+warmTaskGrace,
						)
						_, err := manager.ensureWorker(taskCtx, accountID, "", false)
						taskCancel()
						results <- warmResult{accountID: accountID, err: err}
					}(accountID)
				}
				manager.fillLaunched.Store(int32(launched))
				manager.fillInflight.Store(int32(len(inflight)))
				if len(started) > 0 {
					busySince = time.Time{}
				}
			}
		}
		if len(inflight) == 0 {
			if pendingBusy && !capacityFull {
				if busySince.IsZero() {
					busySince = time.Now()
				}
				if time.Since(busySince) < warmBusyWaitLimit {
					if err := waitWarmCandidate(ctx, 100*time.Millisecond); err != nil && !notified {
						notify(errors.Join(append(failures, err)...))
					}
					continue
				}
			}
			warm = manager.WarmAccountIDs()
			if len(warm) > 0 {
				// 本轮暂时无法继续：记录原因后结束，由 keepWarm 定期再补
				reason := fmt.Sprintf("暂无可启动的账户（预热失败冷却中 %d 个）", len(manager.recentWarmFailures(time.Now())))
				level := "INFO"
				switch {
				case capacityFull:
					occupancy := manager.occupiedWorkers()
					reason = fmt.Sprintf("Worker 槽位已满（占用 %d / 峰值上限 %d）", occupancy.slots, manager.maxActiveValue())
					level = "WARN"
				case classifyErr != nil:
					reason = "候选账户分类失败: " + strings.TrimSpace(classifyErr.Error())
					level = "WARN"
				case pendingBusy:
					reason = "候选账户均在忙碌"
				case readyCount > 0:
					reason = "等待本轮启动结果"
				}
				if len(warm) < manager.PrewarmTarget() {
					manager.noteWarmIdle(level, reason, fmt.Sprintf(
						"WAA Worker 预热等待 | Worker=%d/%d | 原因=%s", len(warm), manager.PrewarmTarget(), reason,
					))
				}
				if launched > 0 {
					manager.requests.log("service", "INFO", fmt.Sprintf(
						"WAA Worker 预热暂停 | Worker=%d/%d | 失败=%d | 耗时=%s | 稍后自动继续",
						len(warm), manager.PrewarmTarget(), len(failures), time.Since(startedAt).Round(time.Millisecond),
					))
				}
				notify(nil)
				return
			}
			if classifyErr != nil {
				failures = append(failures, classifyErr)
			}
			if runtimeBusyErr != nil {
				failures = append(failures, runtimeBusyErr)
			}
			if len(failures) == 0 {
				failures = append(failures, aistudio.ErrNoEligibleAccount)
			}
			notify(fmt.Errorf("没有可预热的账户: %w", errors.Join(failures...)))
			return
		}
		// 等任意一个完成，或定时重新检查（并发数、常驻数可能在运行中被调整）
		timer := time.NewTimer(warmRecheckInterval)
		select {
		case <-ctx.Done():
		case result := <-results:
			handle(result)
		case <-timer.C:
		}
		timer.Stop()
	}
}

const (
	// firstWarmRetryInterval 为启动阶段首个 Worker 未就绪时的重试间隔
	firstWarmRetryInterval = 3 * time.Second
	// modelCatalogRefreshConcurrency 为同时刷新模型目录的账户数上限
	modelCatalogRefreshConcurrency = 32
	// warmRecheckInterval 为滑动窗口预热在等待结果时重新检查空闲槽位的间隔
	warmRecheckInterval = time.Second
	// warmResultBuffer 为预热结果通道容量
	warmResultBuffer = 64
	// warmBusyWaitLimit 为候选账户都在忙时本轮预热最多等待的时间
	warmBusyWaitLimit = 10 * time.Second
	// warmTaskGrace 为单个预热任务在初始化超时之外额外允许的时间
	warmTaskGrace = time.Minute
	// catalogApplyInterval 为启动阶段目录同步结果合并应用的最短间隔
	catalogApplyInterval = time.Second
)

type warmResult struct {
	accountID string
	err       error
}

func excludeAccountIDs(accountIDs []string, excluded map[string]struct{}) []string {
	result := make([]string, 0, len(accountIDs))
	for _, accountID := range accountIDs {
		if _, exists := excluded[accountID]; !exists {
			result = append(result, accountID)
		}
	}
	return result
}

func (manager *accountWorkerManager) waitPrewarm() {
	manager.fillMu.Lock()
	manager.fillMu.Unlock()
}

// Close 关闭全部账户 worker
func (manager *accountWorkerManager) Close() error {
	manager.mu.Lock()
	if manager.closed {
		manager.mu.Unlock()
		return nil
	}
	manager.closed = true
	manager.cancel()
	accounts := make([]*accountWorker, 0, len(manager.accounts))
	for _, account := range manager.accounts {
		accounts = append(accounts, account)
	}
	manager.mu.Unlock()
	manager.waitPrewarm()
	closeResults := make(chan error, len(accounts))
	var closes sync.WaitGroup
	for _, account := range accounts {
		closes.Add(1)
		go func(account *accountWorker) {
			defer closes.Done()
			account.startupMu.Lock()
			defer account.startupMu.Unlock()
			account.mu.Lock()
			defer account.mu.Unlock()
			if account.worker != nil || account.cleanupWorker != nil || account.runtimeLease != nil || account.cleanupLease != nil {
				closeResults <- manager.closeAccountWorker(account)
			}
		}(account)
	}
	closes.Wait()
	close(closeResults)
	var closeErrors []error
	for closeErr := range closeResults {
		closeErrors = append(closeErrors, closeErr)
	}
	return errors.Join(closeErrors...)
}

func (manager *accountWorkerManager) closeAccountWorker(account *accountWorker) error {
	var closeErrors []error
	if account.worker != nil {
		if closeErr := account.worker.Close(); closeErr != nil {
			closeErrors = append(closeErrors, closeErr)
		} else {
			account.worker = nil
			account.generation.Add(1)
		}
	}
	if account.cleanupWorker != nil {
		if closeErr := account.cleanupWorker.Close(); closeErr != nil {
			closeErrors = append(closeErrors, closeErr)
		} else {
			account.cleanupWorker = nil
		}
	}
	if account.cleanupWorker == nil && account.cleanupLease != nil {
		if releaseErr := account.cleanupLease.Release(); releaseErr != nil {
			closeErrors = append(closeErrors, releaseErr)
		} else {
			account.cleanupLease = nil
		}
	}
	if account.worker == nil && account.cleanupWorker == nil && account.cleanupLease == nil && account.runtimeLease != nil {
		if releaseErr := account.runtimeLease.Release(); releaseErr != nil {
			closeErrors = append(closeErrors, releaseErr)
		} else {
			account.runtimeLease = nil
		}
	}
	if account.worker == nil && account.cleanupWorker == nil && account.runtimeLease == nil && account.cleanupLease == nil {
		account.warm.Store(false)
		manager.notifyScheduling()
	}
	return errors.Join(closeErrors...)
}

type accountHeaderProvider struct {
	mu          sync.RWMutex
	accounts    map[string]*accountHeaderState
	globalProxy string
}

type accountHeaderState struct {
	mu      sync.Mutex
	client  *http.Client
	headers http.Header
}

type accountHeaderUpdate struct {
	provider  *accountHeaderProvider
	accountID string
	state     *accountHeaderState
	pending   bool
}

// newAccountHeaderProvider 创建每账户固定出口的公开头提供器
func newAccountHeaderProvider(accounts []*aistudio.Account, globalProxy string) (*accountHeaderProvider, error) {
	provider := &accountHeaderProvider{
		accounts: make(map[string]*accountHeaderState, len(accounts)), globalProxy: globalProxy,
	}
	for _, account := range accounts {
		if account == nil {
			continue
		}
		if err := provider.Add(account); err != nil {
			provider.Close()
			return nil, err
		}
	}
	return provider, nil
}

// Add 注册新账户的固定出口
func (provider *accountHeaderProvider) Add(account *aistudio.Account) error {
	if account == nil {
		return fmt.Errorf("账户未初始化")
	}
	client, err := aistudio.NewProxyHTTPClient(account.EffectiveProxy(provider.globalProxy))
	if err != nil {
		return fmt.Errorf("创建账户 %s 的固定出口: %w", account.ID, err)
	}
	provider.mu.Lock()
	defer provider.mu.Unlock()
	if _, exists := provider.accounts[account.ID]; exists {
		client.CloseIdleConnections()
		return fmt.Errorf("账户固定出口已存在: %s", account.ID)
	}
	provider.accounts[account.ID] = &accountHeaderState{client: client}
	return nil
}

// prepareUpdate 创建待发布的账户固定出口
func (provider *accountHeaderProvider) prepareUpdate(
	account *aistudio.Account,
	config aistudio.AccountConfig,
) (*accountHeaderUpdate, error) {
	if account == nil {
		return nil, fmt.Errorf("账户未初始化")
	}
	provider.mu.RLock()
	current := provider.accounts[account.ID]
	provider.mu.RUnlock()
	if current == nil {
		return nil, fmt.Errorf("账户固定出口不存在: %s", account.ID)
	}
	proxy := strings.TrimSpace(config.Proxy)
	if proxy == "" {
		proxy = strings.TrimSpace(provider.globalProxy)
	}
	client, err := aistudio.NewProxyHTTPClient(proxy)
	if err != nil {
		return nil, fmt.Errorf("创建账户 %s 的固定出口: %w", account.ID, err)
	}
	return &accountHeaderUpdate{
		provider: provider, accountID: account.ID, state: &accountHeaderState{client: client}, pending: true,
	}, nil
}

// Commit 发布已准备的账户固定出口
func (update *accountHeaderUpdate) Commit() {
	if update == nil || !update.pending {
		return
	}
	update.provider.mu.Lock()
	current := update.provider.accounts[update.accountID]
	update.provider.accounts[update.accountID] = update.state
	update.provider.mu.Unlock()
	update.pending = false
	current.client.CloseIdleConnections()
}

// Discard 关闭未发布的账户固定出口
func (update *accountHeaderUpdate) Discard() {
	if update == nil || !update.pending {
		return
	}
	update.pending = false
	update.state.client.CloseIdleConnections()
}

// Remove 删除账户的固定出口
func (provider *accountHeaderProvider) Remove(accountID string) error {
	provider.mu.Lock()
	account := provider.accounts[accountID]
	if account != nil {
		delete(provider.accounts, accountID)
	}
	provider.mu.Unlock()
	if account == nil {
		return fmt.Errorf("账户固定出口不存在: %s", accountID)
	}
	account.client.CloseIdleConnections()
	return nil
}

// Close 关闭全部账户固定出口
func (provider *accountHeaderProvider) Close() {
	provider.mu.Lock()
	accounts := provider.accounts
	provider.accounts = nil
	provider.mu.Unlock()
	for _, account := range accounts {
		account.client.CloseIdleConnections()
	}
}

// Invalidate 清除账户公共头并让下一次请求重新发现
func (provider *accountHeaderProvider) Invalidate(accountID string) error {
	provider.mu.RLock()
	account := provider.accounts[accountID]
	provider.mu.RUnlock()
	if account == nil {
		return fmt.Errorf("账户固定出口不存在: %s", accountID)
	}
	account.mu.Lock()
	account.headers = nil
	account.mu.Unlock()
	return nil
}

// ProtocolHeaders 返回账户当前使用的公开协议头
func (provider *accountHeaderProvider) ProtocolHeaders(ctx context.Context, accountID string) (http.Header, error) {
	provider.mu.RLock()
	account := provider.accounts[accountID]
	provider.mu.RUnlock()
	if account == nil {
		return nil, fmt.Errorf("账户不存在: %s", accountID)
	}
	account.mu.Lock()
	defer account.mu.Unlock()
	if len(account.headers) == 0 {
		headers, err := aistudio.DiscoverPublicHeaders(ctx, account.client)
		if err != nil {
			return nil, err
		}
		account.headers = headers.Clone()
	}
	return account.headers.Clone(), nil
}

// trackedService 跟踪生成请求及其唯一账户租约
type trackedService struct {
	lifecycle context.Context
	service   aistudio.Service
	catalog   modelCatalogService
	pool      *aistudio.AccountPool
	requests  *requestRegistry
	workers   *accountWorkerManager
	forbidden *forbiddenTracker
	// quota 学习各模型 Build 与 Playground 是否共用每日额度，结论跨重启保存
	quota *quotaSharing
	// ignoreSeed 为真时丢弃客户端传入的 seed：客户端固定 seed 会让相同提示词得到几乎相同的回复
	ignoreSeed atomic.Bool
	// repeatNonce 为真时对重复提示词加入不可见随机后缀（见 duplicates.go）
	repeatNonce atomic.Bool
	// modelAccessPublishPending 为真时已有一次合并推送在等待
	modelAccessPublishPending atomic.Bool
	// minOutputTokens 为最大输出 token 的下限（MIN_OUTPUT_TOKENS，可热更新）
	minOutputTokens atomic.Int64
	// downgradeGuard 为降级判定的生效设置（服务配置页修改后立即替换，见 downgrade_guard.go）
	downgradeGuard     atomic.Pointer[downgradeSettings]
	timeout            atomic.Int64
	state              atomic.Int32
	lifecycleMu        sync.Mutex
	transitionDone     chan struct{}
	transitionErr      error
	transitionTimedOut bool
	dataContext        context.Context
	dataCancel         context.CancelFunc
	modelsMu           sync.RWMutex
	models             []aistudio.Model
	modelSyncMu        sync.Mutex
	modelRetriesMu     sync.Mutex
	modelRetries       map[string]struct{}
	modelRefreshDone   <-chan struct{}
	modelChangeMu      sync.Mutex
	modelRevision      uint64
	modelApplied       uint64
}

type modelCatalogService interface {
	RefreshAccountModels(context.Context, string) ([]aistudio.Model, error)
	CachedModels() []aistudio.Model
}

// newTrackedService 创建带超时和生命周期的协议服务
func newTrackedService(
	lifecycle context.Context,
	service aistudio.Service,
	pool *aistudio.AccountPool,
	requests *requestRegistry,
	workers *accountWorkerManager,
	timeout time.Duration,
) *trackedService {
	catalog := service.(modelCatalogService)
	tracked := &trackedService{
		lifecycle: lifecycle, service: service, catalog: catalog, pool: pool, requests: requests, workers: workers,
		modelRetries: make(map[string]struct{}), forbidden: newForbiddenTracker(),
	}
	tracked.timeout.Store(int64(timeout))
	return tracked
}

type serviceStoppedError struct{}

var errServiceTransitioning = errors.New("生成服务正在切换状态")

const (
	serviceStopped int32 = iota
	serviceLaunching
	serviceRunning
	modelCatalogRetryInterval        = 30 * time.Second
	modelCatalogShutdownTimeout      = 2 * time.Second
	serviceTransitionShutdownTimeout = 12 * time.Second
)

func (*serviceStoppedError) Error() string {
	return "AIStudio2API 服务已停止"
}

func (*serviceStoppedError) HTTPStatus() int {
	return http.StatusServiceUnavailable
}

func (*serviceStoppedError) ErrorCode() string {
	return "service_stopped"
}

// Running 返回公开生成服务是否接受请求
func (service *trackedService) Running() bool {
	return service.state.Load() == serviceRunning
}

// State 返回生成服务生命周期状态
func (service *trackedService) State() string {
	switch service.state.Load() {
	case serviceLaunching:
		return "LAUNCHING"
	case serviceRunning:
		return "RUNNING"
	default:
		return "STOPPED"
	}
}

// Start 刷新模型并创建本次公开生成服务
func (service *trackedService) Start(ctx context.Context, launching func()) ([]aistudio.Model, bool, error) {
	service.lifecycleMu.Lock()
	if service.state.Load() == serviceRunning {
		service.lifecycleMu.Unlock()
		return service.modelSnapshot(), false, nil
	}
	if service.state.Load() == serviceLaunching || service.transitionDone != nil {
		service.lifecycleMu.Unlock()
		return service.modelSnapshot(), false, errServiceTransitioning
	}
	dataContext, dataCancel := context.WithCancel(service.lifecycle)
	transitionDone := make(chan struct{})
	service.dataContext = dataContext
	service.dataCancel = dataCancel
	service.transitionDone = transitionDone
	service.transitionErr = nil
	service.transitionTimedOut = false
	service.state.Store(serviceLaunching)
	service.lifecycleMu.Unlock()
	stopCaller := context.AfterFunc(ctx, dataCancel)
	service.replaceModelSnapshot(service.catalog.CachedModels())
	if launching != nil {
		launching()
	}
	models := service.modelSnapshot()
	service.requests.log("service", "INFO", fmt.Sprintf(
		"生成服务启动 | 1/2 | 准备模型目录 | 缓存=%d", len(models),
	))
	catalogReady, catalogSynced, catalogDone := service.startModelCatalogRefresh(dataContext)
	service.lifecycleMu.Lock()
	if service.dataContext == dataContext {
		service.modelRefreshDone = catalogDone
	}
	service.lifecycleMu.Unlock()
	if len(models) == 0 {
		select {
		case <-dataContext.Done():
			stopCaller()
			return nil, false, service.finishLaunch(transitionDone, dataCancel, catalogDone, dataContext.Err())
		case err := <-catalogReady:
			if err != nil {
				stopCaller()
				return nil, false, service.finishLaunch(transitionDone, dataCancel, catalogDone, err)
			}
		}
		models = service.modelSnapshot()
		if len(models) == 0 {
			stopCaller()
			return nil, false, service.finishLaunch(transitionDone, dataCancel, catalogDone, aistudio.ErrNoEligibleAccount)
		}
	}
	service.requests.log("service", "INFO", fmt.Sprintf(
		"生成服务启动 | 2/2 | 预热 WAA Worker | 模型=%d | 目标=%d",
		len(models), service.workers.PrewarmTarget(),
	))
	// 首批账户预热失败时不再等全部账户的模型目录同步完成（账户多时要好几分钟），
	// 而是每隔几秒用新拿到目录的账户重试，任意一个 Worker 就绪即进入 RUNNING
	synced, warmErr := service.waitFirstWarm(dataContext, catalogSynced)
	for warmErr != nil && !synced && dataContext.Err() == nil {
		timer := time.NewTimer(firstWarmRetryInterval)
		select {
		case <-dataContext.Done():
		case <-catalogSynced:
		case <-timer.C:
		}
		timer.Stop()
		if dataContext.Err() != nil {
			break
		}
		synced, warmErr = service.waitFirstWarm(dataContext, catalogSynced)
	}
	if dataContext.Err() != nil {
		stopCaller()
		return nil, false, service.finishLaunch(transitionDone, dataCancel, catalogDone, dataContext.Err())
	}
	if warmErr != nil {
		stopCaller()
		return nil, false, service.finishLaunch(transitionDone, dataCancel, catalogDone, warmErr)
	}
	if !stopCaller() {
		return nil, false, service.finishLaunch(transitionDone, dataCancel, catalogDone, ctx.Err())
	}
	models, err := service.finishModelLaunch(dataContext, transitionDone)
	if err != nil {
		return models, false, service.finishLaunch(transitionDone, dataCancel, catalogDone, err)
	}
	go service.keepWarm(dataContext)
	return models, true, nil
}

// waitFirstWarm 启动预热并等待首个就绪 Worker，同时返回结束时首轮目录同步是否已完成
func (service *trackedService) waitFirstWarm(ctx context.Context, catalogSynced <-chan struct{}) (bool, error) {
	firstWarm := service.workers.StartPrewarm(ctx)
	var warmErr error
	select {
	case <-ctx.Done():
		return true, ctx.Err()
	case err, ok := <-firstWarm:
		warmErr = err
		if !ok {
			warmErr = fmt.Errorf("WAA 预热未返回就绪账户")
		}
	}
	select {
	case <-catalogSynced:
		return true, warmErr
	default:
		return false, warmErr
	}
}

// finishModelLaunch 刷新启动期变化并原子启用生成服务
func (service *trackedService) finishModelLaunch(
	dataContext context.Context,
	transitionDone chan struct{},
) ([]aistudio.Model, error) {
	service.modelSyncMu.Lock()
	defer service.modelSyncMu.Unlock()
	for {
		modelRevision := service.currentModelRevision()
		models := service.catalog.CachedModels()
		service.replaceModelSnapshot(models)
		if len(models) == 0 {
			return models, aistudio.ErrNoEligibleAccount
		}
		service.modelChangeMu.Lock()
		if service.modelRevision != modelRevision {
			service.modelChangeMu.Unlock()
			continue
		}
		service.lifecycleMu.Lock()
		if service.transitionDone != transitionDone || service.state.Load() != serviceLaunching || dataContext.Err() != nil {
			launchErr := dataContext.Err()
			if launchErr == nil {
				launchErr = context.Canceled
			}
			service.lifecycleMu.Unlock()
			service.modelChangeMu.Unlock()
			return service.modelSnapshot(), launchErr
		}
		service.modelApplied = modelRevision
		service.state.Store(serviceRunning)
		service.transitionDone = nil
		close(transitionDone)
		service.lifecycleMu.Unlock()
		service.modelChangeMu.Unlock()
		return service.modelSnapshot(), nil
	}
}

func (service *trackedService) finishLaunch(
	transitionDone chan struct{},
	dataCancel context.CancelFunc,
	modelRefreshDone <-chan struct{},
	launchErr error,
) error {
	dataCancel()
	refreshErr := waitServiceTransition(modelRefreshDone, modelCatalogShutdownTimeout, "模型目录刷新停止")
	service.workers.waitPrewarm()
	resetErr := service.workers.ResetAll()
	service.lifecycleMu.Lock()
	if service.transitionDone == transitionDone {
		service.state.Store(serviceStopped)
		service.dataContext = nil
		service.dataCancel = nil
		service.modelRefreshDone = nil
		service.transitionErr = errors.Join(refreshErr, resetErr)
		service.transitionDone = nil
		close(transitionDone)
	}
	service.lifecycleMu.Unlock()
	return errors.Join(launchErr, refreshErr, resetErr)
}

func waitServiceTransition(done <-chan struct{}, timeout time.Duration, operation string) error {
	if done == nil {
		return nil
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-done:
		return nil
	case <-timer.C:
		return fmt.Errorf("%s超时", operation)
	}
}

func (service *trackedService) markModelAccessVerifiedAsync(
	accountID string,
	accountLabel string,
	modelID string,
	accessGeneration uint64,
	checkedAt time.Time,
) {
	go func() {
		changed, err := service.pool.MarkModelAccessVerifiedIfGeneration(
			accountID, modelID, accessGeneration, checkedAt,
		)
		if err != nil {
			service.requests.log(accountLabel, "ERROR", fmt.Sprintf(
				"模型资格保存失败 | 模型=%s | 错误=%s", modelID, strings.TrimSpace(err.Error()),
			))
			return
		}
		if changed {
			service.publishModelAccess()
		}
	}()
}

// Stop 停止公开生成服务并释放活动 worker
func (service *trackedService) Stop() (bool, error) {
	service.lifecycleMu.Lock()
	state := service.state.Load()
	if state == serviceStopped {
		done := service.transitionDone
		if done == nil && service.transitionTimedOut {
			cleanupErr := service.transitionErr
			service.transitionErr = nil
			service.transitionTimedOut = false
			service.lifecycleMu.Unlock()
			return false, cleanupErr
		}
		service.lifecycleMu.Unlock()
		transitionErr := waitServiceTransition(done, serviceTransitionShutdownTimeout, "生成服务切换停止")
		if done != nil {
			service.lifecycleMu.Lock()
			cleanupErr := service.transitionErr
			if transitionErr == nil {
				service.transitionErr = nil
				service.transitionTimedOut = false
			} else {
				service.transitionTimedOut = true
			}
			service.lifecycleMu.Unlock()
			return false, errors.Join(transitionErr, cleanupErr)
		}
		resetErr := service.workers.ResetAll()
		service.lifecycleMu.Lock()
		service.transitionErr = resetErr
		service.transitionTimedOut = false
		service.lifecycleMu.Unlock()
		return false, resetErr
	}
	dataCancel := service.dataCancel
	if state == serviceLaunching {
		done := service.transitionDone
		service.state.Store(serviceStopped)
		service.lifecycleMu.Unlock()
		dataCancel()
		transitionErr := waitServiceTransition(done, serviceTransitionShutdownTimeout, "生成服务启动停止")
		service.requests.cancelAll()
		service.lifecycleMu.Lock()
		cleanupErr := service.transitionErr
		if transitionErr == nil {
			service.transitionErr = nil
			service.transitionTimedOut = false
		} else {
			service.transitionTimedOut = true
		}
		service.lifecycleMu.Unlock()
		return true, errors.Join(transitionErr, cleanupErr)
	}
	transitionDone := make(chan struct{})
	service.transitionDone = transitionDone
	service.state.Store(serviceStopped)
	modelRefreshDone := service.modelRefreshDone
	service.lifecycleMu.Unlock()
	dataCancel()
	go service.finishStop(transitionDone, modelRefreshDone)
	transitionErr := waitServiceTransition(transitionDone, serviceTransitionShutdownTimeout, "生成服务停止")
	service.lifecycleMu.Lock()
	cleanupErr := service.transitionErr
	if transitionErr == nil {
		service.transitionErr = nil
		service.transitionTimedOut = false
	} else {
		service.transitionTimedOut = true
	}
	service.lifecycleMu.Unlock()
	return true, errors.Join(transitionErr, cleanupErr)
}

// finishStop 完成运行态服务的后台清理
func (service *trackedService) finishStop(transitionDone chan struct{}, modelRefreshDone <-chan struct{}) {
	service.requests.cancelAll()
	refreshErr := waitServiceTransition(modelRefreshDone, modelCatalogShutdownTimeout, "模型目录刷新停止")
	service.lifecycleMu.Lock()
	if service.transitionDone == transitionDone {
		service.transitionErr = refreshErr
	}
	service.lifecycleMu.Unlock()
	service.workers.waitPrewarm()
	resetErr := service.workers.ResetAll()
	service.lifecycleMu.Lock()
	if service.transitionDone == transitionDone {
		service.dataContext = nil
		service.dataCancel = nil
		service.modelRefreshDone = nil
		service.transitionErr = errors.Join(refreshErr, resetErr)
		service.transitionDone = nil
		close(transitionDone)
	}
	service.lifecycleMu.Unlock()
}

// Models 返回启用账户具有模型资格的公开目录
func (service *trackedService) Models(context.Context) ([]aistudio.Model, error) {
	return service.pool.EligibleModels(service.modelSnapshot()), nil
}

// catalogModels 返回管理页使用的完整目录与各模型可用通道
func (service *trackedService) catalogModels() []aistudio.Model {
	return service.pool.CatalogModels(service.modelSnapshot())
}

func (service *trackedService) modelSnapshot() []aistudio.Model {
	service.modelsMu.RLock()
	models := append([]aistudio.Model{}, service.models...)
	service.modelsMu.RUnlock()
	return models
}

// publishModelAccess 合并短时间内的多次账户与模型目录推送。每次推送都要在账户池锁内统计全部账户的状态并重算
// 模型目录（账户多时每次几百毫秒），重启后每个账号首次生成成功、新账户载入都会触发一次；诊断中这部分占了
// 约三成的账户池锁时间。现在 modelAccessPublishDelay 内的多次触发只推送一次，管理页最多晚几秒看到变化
func (service *trackedService) publishModelAccess() {
	if !service.modelAccessPublishPending.CompareAndSwap(false, true) {
		return
	}
	time.AfterFunc(modelAccessPublishDelay, func() {
		service.modelAccessPublishPending.Store(false)
		service.publishModelAccessNow()
	})
}

// publishModelAccessNow 立即推送账户列表与模型目录（管理页上的手动操作使用）
func (service *trackedService) publishModelAccessNow() {
	if service.lifecycle.Err() != nil {
		return
	}
	statuses := service.pool.Status()
	accounts := make([]api.AdminAccount, 0, len(statuses))
	for _, status := range statuses {
		accounts = append(accounts, adminAccountDTO(status))
	}
	service.requests.publish(api.AdminEvent{Type: "accounts", Data: map[string]any{"accounts": accounts}})
	service.requests.publish(api.AdminEvent{Type: "models", Data: map[string]any{"models": service.catalogModels()}})
}

type accountModelRefreshResult struct {
	accountID string
	models    []aistudio.Model
	err       error
	skipped   bool
}

// startModelCatalogRefresh 并发刷新全部账户并持续处理失败目录
func (service *trackedService) startModelCatalogRefresh(ctx context.Context) (<-chan error, <-chan struct{}, <-chan struct{}) {
	ready := make(chan error, 1)
	synced := make(chan struct{})
	done := make(chan struct{})
	accountIDs := service.modelCatalogAccountIDs()
	go func() {
		defer close(done)
		startedAt := time.Now()
		firstReady := false
		synchronized := 0
		refreshed := 0
		failures := make([]error, 0)
		var lastApplied time.Time
		applyPending := false
		authRequiredBefore := service.authRequiredAccountIDs()
		for result := range service.refreshAccountModelCatalogs(ctx, accountIDs, false) {
			if result.err != nil {
				failures = append(failures, fmt.Errorf("账户 %s: %w", result.accountID, result.err))
				continue
			}
			synchronized++
			if len(result.models) == 0 {
				continue
			}
			refreshed++
			if ctx.Err() == nil {
				// 首个目录立即生效；之后最多每秒合并应用一次，避免几百个账户逐个重算全量目录并长期占用账户池锁
				if !firstReady || time.Since(lastApplied) >= catalogApplyInterval {
					service.applyCachedModelCatalog()
					service.publishModelAccess()
					lastApplied = time.Now()
					applyPending = false
				} else {
					applyPending = true
				}
			}
			if !firstReady && ctx.Err() == nil {
				ready <- nil
				firstReady = true
			}
		}
		if applyPending && ctx.Err() == nil {
			service.applyCachedModelCatalog()
			service.publishModelAccess()
		}
		if !firstReady {
			launchErr := ctx.Err()
			if launchErr == nil && len(failures) > 0 {
				launchErr = errors.Join(failures...)
			}
			if launchErr == nil {
				launchErr = aistudio.ErrNoEligibleAccount
			}
			ready <- launchErr
		}
		close(ready)
		close(synced)
		if ctx.Err() != nil {
			return
		}
		if !maps.Equal(service.authRequiredAccountIDs(), authRequiredBefore) {
			service.publishModelAccess()
		}
		service.requests.log("service", "INFO", fmt.Sprintf(
			"模型目录后台同步完成 | 同步=%d | 非空=%d | 模型=%d | 待重试账户=%d | 耗时=%s",
			synchronized, refreshed, len(service.modelSnapshot()), service.pendingModelRetryCount(), time.Since(startedAt).Round(time.Millisecond),
		))
		service.retryModelCatalogs(ctx)
	}()
	return ready, synced, done
}

func (service *trackedService) modelCatalogAccountIDs() []string {
	statuses := service.pool.Status()
	accountIDs := make([]string, 0, len(statuses))
	for _, status := range statuses {
		if status.Enabled && (status.State == aistudio.AccountReady || status.State == aistudio.AccountBusy) {
			accountIDs = append(accountIDs, status.ID)
		}
	}
	sort.Strings(accountIDs)
	return accountIDs
}

func (service *trackedService) refreshAccountModelCatalogs(
	ctx context.Context,
	accountIDs []string,
	pendingOnly bool,
) <-chan accountModelRefreshResult {
	results := make(chan accountModelRefreshResult, len(accountIDs))
	var refreshes sync.WaitGroup
	// 限制同时刷新的账户数：几百个账户同时请求会让出口与代理拥塞，反而拖慢首个目录与 Worker 启动
	limiter := make(chan struct{}, modelCatalogRefreshConcurrency)
	for _, accountID := range accountIDs {
		refreshes.Add(1)
		go func(accountID string) {
			defer refreshes.Done()
			if pendingOnly && !service.modelRetryPending(accountID) {
				results <- accountModelRefreshResult{accountID: accountID, skipped: true}
				return
			}
			select {
			case limiter <- struct{}{}:
			case <-ctx.Done():
				results <- accountModelRefreshResult{accountID: accountID, err: ctx.Err()}
				return
			}
			defer func() { <-limiter }()
			models, err := service.refreshAccountModelCatalog(ctx, accountID)
			results <- accountModelRefreshResult{accountID: accountID, models: models, err: err}
		}(accountID)
	}
	go func() {
		refreshes.Wait()
		close(results)
	}()
	return results
}

func (service *trackedService) refreshAccountModelCatalog(ctx context.Context, accountID string) ([]aistudio.Model, error) {
	requestCtx, cancel := service.lifecycleRequestContext(ctx)
	defer cancel()
	models, err := service.catalog.RefreshAccountModels(requestCtx, accountID)
	service.modelRetriesMu.Lock()
	if err != nil {
		if ctx.Err() == nil {
			service.modelRetries[accountID] = struct{}{}
		}
		service.modelRetriesMu.Unlock()
		if ctx.Err() == nil {
			service.requests.log(accountID, "WARN", "模型目录同步失败 | 错误="+err.Error())
		}
		return nil, err
	}
	if len(models) == 0 {
		service.modelRetries[accountID] = struct{}{}
	} else {
		delete(service.modelRetries, accountID)
	}
	service.modelRetriesMu.Unlock()
	if len(models) == 0 && ctx.Err() == nil {
		service.requests.log(accountID, "WARN", "模型目录为空，等待重新同步")
	}
	return models, nil
}

func (service *trackedService) applyCachedModelCatalog() {
	service.replaceModelSnapshot(service.catalog.CachedModels())
	service.lifecycleMu.Lock()
	state := service.state.Load()
	dataContext := service.dataContext
	running := state == serviceRunning && dataContext != nil && dataContext.Err() == nil
	service.lifecycleMu.Unlock()
	if running {
		service.workers.StartPrewarm(dataContext)
	}
}

func (service *trackedService) syncAccountModels(ctx context.Context, accountID string) ([]aistudio.Model, error) {
	models, err := service.refreshAccountModelCatalog(ctx, accountID)
	service.modelSyncMu.Lock()
	service.applyCachedModelCatalog()
	service.modelSyncMu.Unlock()
	return models, err
}

func (service *trackedService) retryModelCatalogs(ctx context.Context) {
	ticker := time.NewTicker(modelCatalogRetryInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if !service.retryAccountModelCatalogs(ctx) {
			return
		}
	}
}

func (service *trackedService) retryAccountModelCatalogs(ctx context.Context) bool {
	if ctx.Err() != nil {
		return false
	}
	accountIDs := service.pendingModelRetryIDs()
	authRequiredBefore := service.authRequiredAccountIDs()
	for result := range service.refreshAccountModelCatalogs(ctx, accountIDs, true) {
		if ctx.Err() != nil {
			return false
		}
		if !result.skipped && result.err == nil && len(result.models) > 0 {
			service.applyCachedModelCatalog()
			service.publishModelAccess()
			service.requests.log(result.accountID, "INFO", fmt.Sprintf("模型目录已更新 | 模型=%d", len(result.models)))
		}
	}
	authChanged := !maps.Equal(service.authRequiredAccountIDs(), authRequiredBefore)
	if authChanged {
		service.publishModelAccess()
	}
	return true
}

func (service *trackedService) removeAccountModelRetry(accountID string) {
	service.modelRetriesMu.Lock()
	delete(service.modelRetries, accountID)
	service.modelRetriesMu.Unlock()
}

// pendingModelRetryIDs 清理失效账户并返回当前可同步的重试任务
func (service *trackedService) pendingModelRetryIDs() []string {
	states := make(map[string]aistudio.AccountState)
	for _, status := range service.pool.Status() {
		if status.Enabled {
			states[status.ID] = status.State
		}
	}
	service.modelRetriesMu.Lock()
	accountIDs := make([]string, 0, len(service.modelRetries))
	for accountID := range service.modelRetries {
		switch states[accountID] {
		case aistudio.AccountReady, aistudio.AccountBusy:
			accountIDs = append(accountIDs, accountID)
		case aistudio.AccountCooldown:
		default:
			delete(service.modelRetries, accountID)
		}
	}
	service.modelRetriesMu.Unlock()
	sort.Strings(accountIDs)
	return accountIDs
}

func (service *trackedService) pendingModelRetryCount() int {
	service.pendingModelRetryIDs()
	service.modelRetriesMu.Lock()
	pending := len(service.modelRetries)
	service.modelRetriesMu.Unlock()
	return pending
}

func (service *trackedService) modelRetryPending(accountID string) bool {
	service.modelRetriesMu.Lock()
	_, pending := service.modelRetries[accountID]
	service.modelRetriesMu.Unlock()
	return pending
}

func (service *trackedService) authRequiredAccountIDs() map[string]struct{} {
	accountIDs := make(map[string]struct{})
	for _, status := range service.pool.Status() {
		if status.State == aistudio.AccountAuthRequired {
			accountIDs[status.ID] = struct{}{}
		}
	}
	return accountIDs
}

func (service *trackedService) replaceModelSnapshot(models []aistudio.Model) {
	service.modelsMu.Lock()
	service.models = append([]aistudio.Model(nil), models...)
	service.modelsMu.Unlock()
}

// changeModels 原子登记影响模型目录的账户变化
func (service *trackedService) changeModels(update func() error) error {
	service.modelChangeMu.Lock()
	defer service.modelChangeMu.Unlock()
	err := update()
	service.modelRevision++
	return err
}

// SyncModels 合并刷新已登记的模型目录变化
func (service *trackedService) SyncModels(ctx context.Context) error {
	service.modelSyncMu.Lock()
	defer service.modelSyncMu.Unlock()
	return service.syncPendingModels(ctx)
}

// syncPendingModels 刷新当前生命周期内尚未提交的模型变化
func (service *trackedService) syncPendingModels(ctx context.Context) error {
	service.lifecycleMu.Lock()
	state := service.state.Load()
	if state == serviceLaunching {
		service.lifecycleMu.Unlock()
		return nil
	}
	if state != serviceRunning {
		service.lifecycleMu.Unlock()
		service.replaceModelSnapshot(service.catalog.CachedModels())
		service.applyModelRevision(service.currentModelRevision())
		return nil
	}
	dataContext := service.dataContext
	service.lifecycleMu.Unlock()
	for {
		modelRevision, modelApplied := service.modelRevisions()
		if modelApplied >= modelRevision {
			break
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := dataContext.Err(); err != nil {
			return err
		}
		service.replaceModelSnapshot(service.catalog.CachedModels())
		service.applyModelRevision(modelRevision)
	}
	service.lifecycleMu.Lock()
	running := service.state.Load() == serviceRunning && service.dataContext == dataContext
	service.lifecycleMu.Unlock()
	if running {
		service.workers.StartPrewarm(dataContext)
	}
	return nil
}

// currentModelRevision 返回最新模型变化代际
func (service *trackedService) currentModelRevision() uint64 {
	service.modelChangeMu.Lock()
	defer service.modelChangeMu.Unlock()
	return service.modelRevision
}

// modelRevisions 返回模型变化与已提交代际
func (service *trackedService) modelRevisions() (uint64, uint64) {
	service.modelChangeMu.Lock()
	defer service.modelChangeMu.Unlock()
	return service.modelRevision, service.modelApplied
}

// applyModelRevision 提交成功刷新的模型代际
func (service *trackedService) applyModelRevision(revision uint64) {
	service.modelChangeMu.Lock()
	if service.modelApplied < revision {
		service.modelApplied = revision
	}
	service.modelChangeMu.Unlock()
}

func (service *trackedService) observedDataRequestContext(
	ctx context.Context,
	model string,
) (context.Context, context.CancelFunc, error) {
	api.SetAccessLogTarget(ctx, model, "")
	requestCtx, cancel, err := service.dataRequestContext(ctx)
	if err != nil {
		api.SetAccessLogError(ctx, err)
		return nil, nil, err
	}
	observed := aistudio.ContextWithAccountSelectionObserver(requestCtx, func(account *aistudio.Account) {
		api.SetAccessLogTarget(requestCtx, model, account.Config.Label)
	})
	return observed, cancel, nil
}

// CountTokens 返回上游权威输入 token 数
func (service *trackedService) CountTokens(ctx context.Context, request aistudio.TokenCountRequest) (aistudio.TokenCount, error) {
	request.Model = service.pool.CanonicalModelID(request.Model)
	requestCtx, cancel, err := service.observedDataRequestContext(ctx, request.Model)
	if err != nil {
		return aistudio.TokenCount{}, err
	}
	defer cancel()
	count, requestErr := service.service.CountTokens(requestCtx, request)
	api.SetAccessLogError(requestCtx, requestErr)
	return count, requestErr
}

// GenerateVideo 创建一个 Veo 长任务
func (service *trackedService) GenerateVideo(ctx context.Context, request aistudio.VideoRequest) (aistudio.VideoOperation, error) {
	api.SetAccessLogTarget(ctx, request.Model, "")
	request.Model = service.pool.CanonicalModelID(request.Model)
	requestCtx, cancel, err := service.dataRequestContext(ctx)
	if err != nil {
		api.SetAccessLogError(ctx, err)
		return aistudio.VideoOperation{}, err
	}
	defer cancel()
	workerGenerations := make(map[string]uint64)
	recoveredWorkers := make(map[string]struct{})
	selectedAccountID := ""
	selectedAccountLabel := ""
	selectedAccessGeneration := uint64(0)
	requestCtx = aistudio.ContextWithAccountSelectionObserver(requestCtx, func(account *aistudio.Account) {
		workerGenerations[account.ID] = service.workers.WorkerGeneration(account.ID)
		selectedAccountID = account.ID
		selectedAccountLabel = account.Config.Label
		selectedAccessGeneration = service.pool.ModelAccessGeneration(account.ID)
		api.SetAccessLogTarget(requestCtx, request.Model, account.Config.Label)
	})
	request.RecoverWAARuntime = func(recoveryCtx context.Context, accountID string, cause error) (bool, error) {
		workerFailed := service.workers.WorkerFailed(accountID)
		workerReplaced := errors.Is(cause, errAccountWorkerReplaced)
		recoverCurrentGeneration := needsWAARuntimeRecovery(cause, false, workerFailed, workerReplaced)
		if recoveryCtx.Err() != nil || !recoverCurrentGeneration {
			return false, nil
		}
		waaRuntimeFailed := aistudio.DefinitiveWAARuntimeFailure(cause)
		expectedGeneration := workerGenerations[accountID]
		recovered, _, recoveryErr := service.recoverWorkerOnce(
			recoveryCtx, accountID, expectedGeneration, recoveredWorkers,
			recoverCurrentGeneration, workerFailed || waaRuntimeFailed,
		)
		return recovered, recoveryErr
	}
	video, ok := service.service.(aistudio.VideoService)
	if !ok {
		return aistudio.VideoOperation{}, fmt.Errorf("video service 不可用")
	}
	operation, requestErr := video.GenerateVideo(requestCtx, request)
	if requestErr == nil && selectedAccountID != "" {
		service.markModelAccessVerifiedAsync(
			selectedAccountID, selectedAccountLabel,
			strings.TrimPrefix(strings.TrimSpace(request.Model), "models/"),
			selectedAccessGeneration, operation.ModelAccessCheckedAt(),
		)
	}
	api.SetAccessLogError(requestCtx, requestErr)
	return operation, requestErr
}

func needsWAARuntimeRecovery(cause error, generationChanged bool, workerFailed bool, workerReplaced bool) bool {
	return generationChanged || workerFailed || workerReplaced ||
		aistudio.DefinitiveWAARuntimeFailure(cause)
}

// recoverWorkerOnce 对指定账户的失败 Worker 版本执行至多一次恢复
func (service *trackedService) recoverWorkerOnce(
	ctx context.Context,
	accountID string,
	expectedGeneration uint64,
	recoveredWorkers map[string]struct{},
	currentGenerationEligible bool,
	resetCurrentGeneration bool,
) (recovered bool, currentGeneration bool, err error) {
	if _, recovered := recoveredWorkers[accountID]; recovered {
		return false, false, nil
	}
	if service.workers.WorkerGeneration(accountID) != expectedGeneration {
		recoveredWorkers[accountID] = struct{}{}
		return true, false, nil
	}
	if !currentGenerationEligible {
		return false, false, nil
	}
	if resetCurrentGeneration {
		reset, err := service.workers.ResetIfGeneration(ctx, accountID, expectedGeneration)
		if err != nil {
			return false, false, err
		}
		if !reset {
			recoveredWorkers[accountID] = struct{}{}
			return true, true, nil
		}
	}
	recoveredWorkers[accountID] = struct{}{}
	return true, true, nil
}

// GetGenerateVideoOperation 读取 Veo 长任务状态
func (service *trackedService) GetGenerateVideoOperation(ctx context.Context, operationID string) (aistudio.VideoOperation, error) {
	requestCtx, cancel, err := service.observedDataRequestContext(ctx, "")
	if err != nil {
		return aistudio.VideoOperation{}, err
	}
	defer cancel()
	video, ok := service.service.(aistudio.VideoService)
	if !ok {
		return aistudio.VideoOperation{}, fmt.Errorf("video service 不可用")
	}
	operation, requestErr := video.GetGenerateVideoOperation(requestCtx, operationID)
	api.SetAccessLogError(requestCtx, requestErr)
	return operation, requestErr
}

// DownloadFile 下载生成任务绑定的 Drive 文件
func (service *trackedService) DownloadFile(ctx context.Context, fileID string) (aistudio.MediaStream, error) {
	requestCtx, cancel, err := service.observedDataRequestContext(ctx, "")
	if err != nil {
		return aistudio.MediaStream{}, err
	}
	video, ok := service.service.(aistudio.VideoService)
	if !ok {
		cancel()
		return aistudio.MediaStream{}, fmt.Errorf("video service 不可用")
	}
	media, requestErr := video.DownloadFile(requestCtx, fileID)
	api.SetAccessLogError(requestCtx, requestErr)
	if requestErr != nil {
		cancel()
		return aistudio.MediaStream{}, requestErr
	}
	media.Body = &trackedMediaReadCloser{body: media.Body, cancel: cancel}
	return media, requestErr
}

type trackedMediaReadCloser struct {
	body   io.ReadCloser
	cancel context.CancelFunc
	once   sync.Once
	err    error
}

func (closer *trackedMediaReadCloser) Read(target []byte) (int, error) {
	return closer.body.Read(target)
}

func (closer *trackedMediaReadCloser) Close() error {
	closer.once.Do(func() {
		closer.err = closer.body.Close()
		closer.cancel()
	})
	return closer.err
}

func (service *trackedService) acquireWarmLease(ctx context.Context, selection aistudio.AccountSelection) (*aistudio.AccountLease, error) {
	fixedAccount := strings.TrimSpace(selection.AccountID) != "" || strings.TrimSpace(selection.ResourceID) != ""
	failedWorkers := make(map[string]struct{})
	var promoteFailure error
	var waiter *dispatchWaiter
	defer func() { service.workers.dispatch.leave(waiter) }()
	wait := func(delay time.Duration) error {
		if waiter == nil {
			waiter = service.workers.dispatch.join(dispatchKey(selection))
		}
		return waitScheduling(ctx, waiter.wake, delay)
	}
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		warm := service.workers.ReadyWarmAccountIDs()
		active := service.workers.WarmAccountIDs()
		groups, err := service.pool.ClassifyCandidates(ctx, selection, warm)
		if err != nil {
			return nil, err
		}
		groups.WarmReady = excludeAccountIDs(groups.WarmReady, failedWorkers)
		groups.WarmAvailable = excludeAccountIDs(groups.WarmAvailable, failedWorkers)
		groups.WarmBusy = excludeAccountIDs(groups.WarmBusy, failedWorkers)
		groups.StandbyReady = excludeAccountIDs(groups.StandbyReady, failedWorkers)
		groups.StandbyBusy = excludeAccountIDs(groups.StandbyBusy, failedWorkers)
		var readyBusyErr, standbyBusyErr error
		groups.StandbyReady, readyBusyErr = service.workers.runtimeAvailable(groups.StandbyReady)
		groups.StandbyBusy, standbyBusyErr = service.workers.runtimeAvailable(groups.StandbyBusy)
		runtimeBusyErr := errors.Join(readyBusyErr, standbyBusyErr)
		candidates := len(groups.WarmReady) + len(groups.WarmAvailable) + len(groups.WarmBusy) +
			len(groups.StandbyReady) + len(groups.StandbyBusy)
		standbyReady, opening := service.workers.withoutOpening(groups.StandbyReady)
		groups.StandbyReady = standbyReady
		warmAvailable := append(append([]string(nil), groups.WarmReady...), groups.WarmAvailable...)
		if len(warmAvailable) > 0 {
			candidate := selection
			candidate.AllowedAccountIDs = warmAvailable
			lease, _, acquireErr := service.pool.TryAcquireFor(ctx, candidate)
			if errors.Is(acquireErr, aistudio.ErrAccountNotFound) && !fixedAccount {
				continue
			}
			if lease != nil || acquireErr != nil {
				return lease, acquireErr
			}
		}
		if len(groups.StandbyReady) == 0 && opening {
			if err := wait(schedulingRecheck); err != nil {
				return nil, err
			}
			continue
		}
		warmCandidates := len(warmAvailable) + len(groups.WarmBusy)
		coolingVictim := len(active) >= service.workers.maxActiveValue() &&
			service.workers.idleWarmVictimFor("", selection.ModelID, true) != ""
		if len(groups.StandbyReady) > 0 && (len(active) < service.workers.maxActiveValue() || coolingVictim) && warmCandidates > 0 && !fixedAccount {
			standby := groups.StandbyReady
			if cold := service.workers.coldAccounts(standby); len(cold) > 0 {
				standby = cold
			}
			service.workers.expandInBackground(standby[0], selection.ModelID)
			if err := wait(schedulingRecheck); err != nil {
				return nil, err
			}
			continue
		}
		if len(groups.StandbyReady) > 0 && (len(active) < service.workers.maxActiveValue() ||
			warmCandidates == 0 && service.workers.idleWarmVictim("") != "") {
			standby := groups.StandbyReady
			if len(active) < service.workers.maxActiveValue() {
				if cold := service.workers.coldAccounts(standby); len(cold) > 0 {
					standby = cold
				}
			}
			accountID := standby[0]
			candidate := selection
			candidate.AccountID = accountID
			lease, _, acquireErr := service.pool.TryAcquireFor(ctx, candidate)
			if acquireErr != nil {
				if errors.Is(acquireErr, aistudio.ErrAccountNotFound) && !fixedAccount {
					continue
				}
				return nil, acquireErr
			}
			if lease == nil {
				if err := wait(schedulingRecheck); err != nil {
					return nil, err
				}
				continue
			}
			workersChanged := service.workers.schedulingChanged()
			_, promoteErr := service.workers.promote(ctx, accountID, selection.ModelID)
			if promoteErr == nil {
				return lease, nil
			}
			if releaseErr := lease.Release(); releaseErr != nil {
				return nil, errors.Join(promoteErr, releaseErr)
			}
			if errors.Is(promoteErr, errAccountWorkerOpening) {
				continue
			}
			if errors.Is(promoteErr, errAccountWorkerCapacity) {
				if err := waitScheduling(ctx, workersChanged, schedulingRecheck); err != nil {
					return nil, err
				}
				continue
			}
			if fixedAccount {
				return nil, promoteErr
			}
			failedWorkers[accountID] = struct{}{}
			promoteFailure = promoteErr
			continue
		}
		if len(groups.WarmBusy) > 0 || len(groups.StandbyBusy) > 0 || opening {
			if err := wait(schedulingRecheck); err != nil {
				return nil, err
			}
			continue
		}
		if !groups.EarliestCooldown.IsZero() {
			remaining := time.Until(groups.EarliestCooldown)
			if remaining > cooldownQueueLimit {
				return nil, &aistudio.AllCoolingError{ModelID: selection.ModelID, Until: groups.EarliestCooldown}
			}
			if err := wait(min(remaining, schedulingRecheck)); err != nil {
				return nil, err
			}
			continue
		}
		if candidates > 0 {
			if err := wait(schedulingRecheck); err != nil {
				return nil, err
			}
			continue
		}
		if promoteFailure != nil {
			return nil, promoteFailure
		}
		if runtimeBusyErr != nil {
			return nil, runtimeBusyErr
		}
		return nil, service.pool.NoEligibleError(selection)
	}
}

// schedulingRecheck 为等待调度信号时的兜底复查间隔
const schedulingRecheck = time.Second

// cooldownQueueLimit 为全部候选账户冷却时请求排队等待恢复的上限，覆盖分钟限额窗口
const cooldownQueueLimit = time.Minute

// waitScheduling 等待账户或 Worker 可调度状态变化、兜底复查或请求取消
func waitScheduling(ctx context.Context, changed <-chan struct{}, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-changed:
		return nil
	case <-timer.C:
		return nil
	}
}

func waitWarmCandidate(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

const streamStallThreshold = 15 * time.Second

type upstreamActivity struct {
	bytes    atomic.Int64
	lastNano atomic.Int64
}

type requestPreparationTiming struct {
	mu             sync.Mutex
	phase          aistudio.RequestPhase
	phaseStarted   time.Time
	waa            time.Duration
	responseHeader time.Duration
}

func newRequestPreparationTiming(startedAt time.Time) *requestPreparationTiming {
	return &requestPreparationTiming{phase: aistudio.RequestPhasePreparingWAA, phaseStarted: startedAt}
}

func (timing *requestPreparationTiming) observe(phase aistudio.RequestPhase) {
	timing.observeChanged(phase)
}

// observeChanged 记录阶段切换，返回阶段是否真的发生了变化
func (timing *requestPreparationTiming) observeChanged(phase aistudio.RequestPhase) bool {
	timing.mu.Lock()
	defer timing.mu.Unlock()
	if phase == timing.phase {
		return false
	}
	now := time.Now()
	timing.finishPhaseLocked(now)
	timing.phase = phase
	timing.phaseStarted = now
	return true
}

func (timing *requestPreparationTiming) snapshot(now time.Time) (string, time.Duration, time.Duration) {
	timing.mu.Lock()
	defer timing.mu.Unlock()
	waa := timing.waa
	responseHeader := timing.responseHeader
	elapsed := now.Sub(timing.phaseStarted)
	switch timing.phase {
	case aistudio.RequestPhasePreparingWAA:
		waa += elapsed
	case aistudio.RequestPhaseSendingUpstream:
		responseHeader += elapsed
	}
	current := "流已建立"
	switch timing.phase {
	case aistudio.RequestPhasePreparingWAA:
		current = "WAA proof"
	case aistudio.RequestPhaseSendingUpstream:
		current = "等待上游响应头"
	}
	return current, waa, responseHeader
}

func (timing *requestPreparationTiming) finishPhaseLocked(now time.Time) {
	elapsed := now.Sub(timing.phaseStarted)
	switch timing.phase {
	case aistudio.RequestPhasePreparingWAA:
		timing.waa += elapsed
	case aistudio.RequestPhaseSendingUpstream:
		timing.responseHeader += elapsed
	}
}

func (activity *upstreamActivity) observe(count int) {
	if count <= 0 {
		return
	}
	now := time.Now().UnixNano()
	activity.lastNano.Store(now)
	activity.bytes.Add(int64(count))
}

func (activity *upstreamActivity) logFields(now time.Time) string {
	if activity == nil {
		return "网络字节=0"
	}
	lastNano := activity.lastNano.Load()
	if lastNano == 0 {
		return "网络字节=0"
	}
	return fmt.Sprintf(
		"网络字节=%d | 最近网络=%s",
		activity.bytes.Load(), now.Sub(time.Unix(0, lastNano)).Round(time.Millisecond),
	)
}

// inlineMediaInput 统计规范消息中的内联附件数量和原始大小
func inlineMediaInput(contents []aistudio.Content) (int, int64) {
	count := 0
	var bytes int64
	for _, content := range contents {
		for _, part := range content.Parts {
			if part.InlineData == nil {
				continue
			}
			count++
			bytes += int64(len(part.InlineData.Data))
		}
	}
	return count, bytes
}

// Generate 获取唯一账户并转发规范事件流
func (service *trackedService) Generate(ctx context.Context, request aistudio.GenerateRequest) (<-chan aistudio.Event, error) {
	events, wait, err := service.startGenerate(ctx, request)
	if err != nil || wait == nil {
		return events, err
	}
	if err := wait(); err != nil {
		return nil, err
	}
	return events, nil
}

// startGenerate 开始一次生成并立即返回事件流。被拦截的模型在流式严格模式下还返回 wait：调用方在不持有任何锁时调用它，
// 等到降级判定出结果（判定为降级时返回拒绝错误，见 downgrade_guard.go）
func (service *trackedService) startGenerate(ctx context.Context, request aistudio.GenerateRequest) (<-chan aistudio.Event, func() error, error) {
	generationStartedAt := time.Now()
	// trace 只在 /trace/ 排查路由的请求上存在，主路由为 nil（所有记录方法都是空操作）
	trace := aistudio.TraceFromContext(ctx)
	// 只有系统提示、没有任何对话消息的请求：OpenAI 等协议允许这样调用，模型会按系统提示直接作答；
	// AI Studio 要求至少一条对话消息，原先这类请求直接失败。这里把系统提示作为唯一的用户消息发送
	if len(request.Contents) == 0 && strings.TrimSpace(request.System) != "" {
		request.Contents = []aistudio.Content{{Role: aistudio.RoleUser, Parts: []aistudio.Part{{Text: request.System}}}}
		request.System = ""
		trace.Note("请求只有系统提示、没有对话消息：已把系统提示作为用户消息发送")
	}
	trace.AddParameters("客户端请求（协议转换后）", request)
	api.SetAccessLogTarget(ctx, request.Model, "")
	api.SetAccessLogGenerationConfig(ctx, request.Config)
	seedNote := "未发送"
	if request.Config.Seed != nil {
		seedNote = fmt.Sprintf("客户端 %d", *request.Config.Seed)
	}
	if request.Config.Seed != nil && service.ignoreSeed.Load() {
		api.MarkAccessLogSeedIgnored(ctx)
		request.Config.Seed = nil
		seedNote = "未发送"
	}
	// 客户端指定了 seed（且没有按配置忽略）时视为需要可复现，不加随机后缀
	honoredClientSeed := request.Config.Seed != nil
	// 生成参数完全按客户端：客户端没传 seed 时不再自动加随机 seed（网页版也不发 seed），
	// 避免相同请求得到相同回复靠的是随机后缀（见 maybeAddPromptNonce）
	diag := newGenerationDiagnostics(request, seedNote)
	// 最大输出下限：客户端设得太小时提高（编码时生效，不超过模型上限）；图片、语音等模型不调整
	if floor := service.minOutputTokens.Load(); floor > 0 && randomSeedApplicable(request.Model, request.Config) {
		request.MinOutputTokens = floor
		if request.Config.MaxOutputTokens != nil && *request.Config.MaxOutputTokens < floor {
			api.MarkAccessLogMaxOutputRaised(ctx, floor)
			trace.Note(fmt.Sprintf("客户端设置的最大输出 %d 低于下限，发送时已提高到 %d（不超过模型上限）", *request.Config.MaxOutputTokens, floor))
		}
	}
	// 输入统计按客户端原始提示词记录，随机后缀不计入
	api.SetAccessLogGenerationInput(ctx, request)
	// 降级判定的对话指纹与输入字数按客户端原始内容计算（随机后缀加在最后一条用户消息的副本上，不改原切片）
	guardContents := request.Contents
	if service.maybeAddPromptNonce(&request, diag, honoredClientSeed) {
		api.MarkAccessLogSeedNote(ctx, "已加随机后缀")
		trace.Note("已在最后一条用户消息末尾加入不可见的零宽字符随机后缀，避免上游对相同或相近的请求返回相同回复")
	}
	trace.SetRequestID(request.ID)
	if trace.Active() {
		// 该请求的进度日志同步写进排查记录的时间线，记录结束时自动移除
		requestID := request.ID
		activeTraces.Store(requestID, trace)
		trace.OnFinish(func() { activeTraces.Delete(requestID) })
	}
	trace.AddParameters("发往上游（seed 与随机后缀处理后）", request)
	trace.SetPrompt(diag.promptHash, request)
	api.StartAccessLog(ctx)
	request.Model = service.pool.CanonicalModelID(request.Model)
	// 降级判定：被拦截的模型准备判定；同一段对话近期被判定为降级、且当时的消息原样都在时，发送前直接拒绝（不换号重试）
	gate, remembered := service.prepareDowngradeGate(ctx, request, guardContents)
	if remembered != nil {
		api.SetAccessLogError(ctx, remembered)
		service.requests.start(request, func() {})
		service.recordDowngradeDecision(ctx, request.ID, "", request.Model, remembered.Decision, true)
		service.requests.finish(request.ID, "failed", remembered)
		return nil, nil, remembered
	}
	requestCtx, cancel, err := service.dataRequestContext(ctx)
	if err != nil {
		api.SetAccessLogError(ctx, err)
		service.requests.start(request, func() {})
		service.requests.finish(request.ID, "failed", err)
		return nil, nil, err
	}
	service.requests.start(request, cancel)
	resourceID, err := service.pool.ResourceIDForContents(requestCtx, request.Contents)
	if err != nil {
		api.SetAccessLogError(requestCtx, err)
		cancel()
		service.requests.finish(request.ID, finalRequestState(err), err)
		return nil, nil, err
	}
	events := make(chan aistudio.Event, 8)
	diag.downgrade = gate
	go service.generateWithRetry(ctx, requestCtx, cancel, generationStartedAt, request, resourceID, events, diag)
	if gate == nil {
		return events, nil, nil
	}
	guarded, wait := gate.start(ctx, cancel, events)
	return guarded, wait, nil
}

func (service *trackedService) generateWithRetry(
	clientCtx context.Context,
	requestCtx context.Context,
	cancel context.CancelFunc,
	generationStartedAt time.Time,
	request aistudio.GenerateRequest,
	resourceID string,
	destination chan<- aistudio.Event,
	diag *generationDiagnostics,
) {
	trace := aistudio.TraceFromContext(requestCtx)
	maxAttempts := 1
	requestedAccountID := strings.TrimSpace(request.AccountID)
	modelID := strings.TrimPrefix(strings.TrimSpace(request.Model), "models/")
	unbound := requestedAccountID == "" && resourceID == ""
	fileBound := requestedAccountID == "" && resourceID != ""
	if unbound || fileBound {
		_, eligible := service.pool.EnabledAccounts()
		if eligible > 1 {
			maxAttempts = eligible
		}
	}
	var lease *aistudio.AccountLease
	var source <-chan aistudio.Event
	var first aistudio.Event
	var activity *upstreamActivity
	var temporaryCopies *aistudio.TemporaryFileCopies
	var err error
	originalContents := request.Contents
	attempted := make(map[string]struct{}, maxAttempts)
	recoveredWorker := make(map[string]struct{})
	recoveryAccountID := ""
	copyFiles := false
	// buildOnly：Playground 返回输入 token 超过上限后，后续尝试只走 Build 通道
	buildOnly := false
	// hardFailures：既没有让账号进入冷却、也不是 Worker 重建或改走 Build 的失败次数（5xx、404、403、超时等）。
	// 这类失败往往与请求本身有关，换号次数不再随账号数增长，并在两次之间退避
	hardFailures := 0
	for attempt := 0; attempt < maxAttempts; attempt++ {
		request.Contents = originalContents
		selectionAccountID := requestedAccountID
		recoveringAccount := recoveryAccountID != ""
		if recoveryAccountID != "" {
			selectionAccountID = recoveryAccountID
			recoveryAccountID = ""
		}
		selectionResourceID := resourceID
		if copyFiles {
			selectionResourceID = ""
		}
		selection := aistudio.AccountSelection{
			ModelID: modelID, Method: "generateContent",
			AccountID: selectionAccountID, ResourceID: selectionResourceID,
		}
		if resourceID != "" || request.Config.SpeechConfig != nil && request.Config.SpeechConfig.Mode != "" {
			selection.PlaygroundOnly = true
		}
		if buildOnly && !selection.PlaygroundOnly {
			selection.BuildOnly = true
		}
		// 降级判定：被拦截的模型优先走 Playground（该通道每块都带累计正文数，判定最准，不需要估算）
		if diag.guard() != nil && !selection.BuildOnly && !selection.PlaygroundOnly {
			selection.PlaygroundFirst = true
		}
		if (unbound || fileBound) && len(attempted) > 0 {
			enabled, _ := service.pool.EnabledAccounts()
			for _, accountID := range enabled {
				if _, exists := attempted[accountID]; !exists {
					selection.AllowedAccountIDs = append(selection.AllowedAccountIDs, accountID)
				}
			}
		}
		nextLease, acquireErr := service.acquireWarmLease(requestCtx, selection)
		if acquireErr != nil && selection.PlaygroundFirst && requestCtx.Err() == nil && playgroundUnavailable(acquireErr) {
			// 没有可用的 Playground 账号（不支持该模型或全部冷却）：退回其他通道，Build 通道按估算 + CountTokens 判定
			trace.Note("降级判定：Playground 通道没有可用账号（" + acquireErr.Error() + "），改为不限通道选号")
			selection.PlaygroundFirst = false
			nextLease, acquireErr = service.acquireWarmLease(requestCtx, selection)
		}
		if acquireErr != nil {
			trace.Note("获取账号失败: " + acquireErr.Error())
			var ownerCooling *aistudio.AllCoolingError
			if fileBound && !copyFiles && (errors.Is(acquireErr, aistudio.ErrNoEligibleAccount) || errors.As(acquireErr, &ownerCooling)) && requestCtx.Err() == nil {
				// 文件所属账号不可用：改为把文件复制到其他账号再试一次。这次重试不占用换号次数，
				// 否则只有一个可调度账号时循环直接结束，既没有错误也没有租约
				copyFiles = true
				err = acquireErr
				maxAttempts++
				continue
			}
			if err == nil || !errors.Is(acquireErr, aistudio.ErrNoEligibleAccount) {
				err = acquireErr
			}
			if recoveringAccount && unbound && requestCtx.Err() == nil {
				attempted[selectionAccountID] = struct{}{}
				continue
			}
			break
		}
		lease = nextLease
		err = nil
		source = nil
		request.AccountID = lease.Account().ID
		workerGeneration := service.workers.WorkerGeneration(request.AccountID)
		attempted[request.AccountID] = struct{}{}
		accountLabel := lease.Account().Config.Label
		trace.StartAttempt(accountLabel, string(lease.Channel()))
		api.SetAccessLogChannel(requestCtx, string(lease.Channel()))
		api.SetAccessLogTarget(requestCtx, modelID, accountLabel)
		service.requests.markRunning(request.ID, request.AccountID, accountLabel)
		service.requests.markChannel(request.ID, string(lease.Channel()))
		service.requests.logRequestProgress(request.ID, accountLabel, "INFO", "等待上游响应")
		attemptCtx := aistudio.ContextWithAccountLease(requestCtx, lease)
		var attemptCopies *aistudio.TemporaryFileCopies
		copiedFileCount := 0
		if resourceID != "" {
			fileCopies, ok := service.service.(interface {
				CopyFileReferencesToLease(context.Context, *aistudio.AccountLease, []aistudio.Content) ([]aistudio.Content, *aistudio.TemporaryFileCopies, error)
			})
			if !ok {
				err = fmt.Errorf("文件引用跨账户服务不可用")
			} else {
				request.Contents, attemptCopies, err = fileCopies.CopyFileReferencesToLease(
					requestCtx, lease, originalContents,
				)
				if err == nil {
					copiedFileCount = attemptCopies.Count()
				}
			}
		}
		mediaCount, mediaBytes := inlineMediaInput(request.Contents)
		if err == nil && mediaCount > 0 {
			uploader, ok := service.service.(interface {
				UploadInlineMediaToLease(context.Context, *aistudio.AccountLease, []aistudio.Content, *aistudio.TemporaryFileCopies) ([]aistudio.Content, *aistudio.TemporaryFileCopies, error)
			})
			if !ok {
				err = fmt.Errorf("内联附件上传服务不可用")
			} else {
				uploadStartedAt := time.Now()
				request.Contents, attemptCopies, err = uploader.UploadInlineMediaToLease(
					requestCtx, lease, request.Contents, attemptCopies,
				)
				if err == nil {
					service.requests.log(accountLabel, "INFO", fmt.Sprintf(
						"内联附件处理完成 | 附件=%d | 原始=%dB | 耗时=%s",
						mediaCount, mediaBytes, time.Since(uploadStartedAt).Round(time.Millisecond),
					))
				}
			}
		}
		prepareStartedAt := time.Now()
		prepareTiming := newRequestPreparationTiming(prepareStartedAt)
		prepareWarningDone := make(chan struct{})
		prepareWarning := time.AfterFunc(streamStallThreshold, func() {
			current, _, _ := prepareTiming.snapshot(time.Now())
			service.requests.logRequestProgress(request.ID, accountLabel, "WARN", fmt.Sprintf(
				"请求准备等待 | 已等待=%s | 当前=%s | 模型=%s",
				streamStallThreshold, current, modelID,
			))
			close(prepareWarningDone)
		})
		activity = &upstreamActivity{}
		attemptCtx = aistudio.ContextWithStreamActivityObserver(attemptCtx, activity.observe)
		// 阶段切换写入请求进度日志，便于把"分到账号 → 首字"拆成生成 proof、等上游响应头、响应头到首字三段
		attemptCtx = aistudio.ContextWithRequestPhaseObserver(attemptCtx, func(phase aistudio.RequestPhase) {
			if !prepareTiming.observeChanged(phase) {
				return
			}
			switch phase {
			case aistudio.RequestPhaseSendingUpstream:
				service.requests.logRequestProgress(request.ID, accountLabel, "INFO", "WAA proof 完成")
			case aistudio.RequestPhaseStreaming:
				service.requests.logRequestProgress(request.ID, accountLabel, "INFO", "上游已返回响应头")
			}
		})
		if err == nil {
			source, err = service.service.Generate(attemptCtx, request)
		}
		if err == nil && copiedFileCount > 0 {
			sourceLabels := make([]string, 0, len(attemptCopies.SourceAccountIDs()))
			statuses := service.pool.Status()
			for _, sourceID := range attemptCopies.SourceAccountIDs() {
				sourceLabel := sourceID
				for _, status := range statuses {
					if status.ID == sourceID {
						sourceLabel = status.Label
						break
					}
				}
				sourceLabels = append(sourceLabels, sourceLabel)
			}
			service.requests.log(accountLabel, "INFO", fmt.Sprintf(
				"文件引用复制 | 来源=%s | 目标=%s | 文件=%d",
				strings.Join(sourceLabels, ","), accountLabel, copiedFileCount,
			))
		}
		prepareElapsed := time.Since(prepareStartedAt)
		if !prepareWarning.Stop() {
			<-prepareWarningDone
			_, waa, responseHeader := prepareTiming.snapshot(time.Now())
			service.requests.logRequestProgress(request.ID, accountLabel, "INFO", fmt.Sprintf(
				"请求准备结束 | 等待=%s | WAA=%s | 响应头=%s | 模型=%s",
				prepareElapsed.Round(time.Millisecond), waa.Round(time.Millisecond),
				responseHeader.Round(time.Millisecond), modelID,
			))
		}
		if err == nil {
			upstreamStartedAt := time.Now()
			firstEventDelayed := false
			first, err = firstGenerateEvent(requestCtx, source, func() {
				firstEventDelayed = true
				service.requests.logRequestProgress(request.ID, accountLabel, "WARN", fmt.Sprintf(
					"上游首事件等待 | 已等待=%s | 模型=%s | %s",
					streamStallThreshold, modelID, activity.logFields(time.Now()),
				))
			})
			if firstEventDelayed && err == nil {
				service.requests.logRequestProgress(request.ID, accountLabel, "INFO", fmt.Sprintf(
					"上游首事件到达 | 等待=%s | 事件=%s | 模型=%s",
					time.Since(upstreamStartedAt).Round(time.Millisecond), first.Kind, modelID,
				))
			}
			if err == nil {
				api.SetAccessLogFirstEvent(requestCtx, time.Since(generationStartedAt))
				service.requests.logRequestProgress(request.ID, accountLabel, "INFO", "收到上游首个事件")
				service.quota.attemptSucceeded(request.AccountID, modelID, lease.Channel(), lease.CheckedAt())
				api.SetAccessLogTarget(requestCtx, first.ProviderModel, accountLabel)
				temporaryCopies = attemptCopies
				service.forbidden.reset(request.AccountID)
				trace.FinishAttempt(nil)
				break
			}
		}
		if attemptCopies != nil {
			err = errors.Join(err, attemptCopies.Cleanup())
		}
		trace.FinishAttempt(err)
		workerFailed := service.workers.WorkerFailed(request.AccountID)
		waaRuntimeFailed := aistudio.DefinitiveWAARuntimeFailure(err)
		workerReplaced := errors.Is(err, errAccountWorkerReplaced)
		localWorkerFailure := (workerFailed || workerReplaced) && requestCtx.Err() == nil
		retryable := retryableGenerateAccountError(requestCtx, err) || localWorkerFailure
		recoverWorker := false
		if aistudio.DefinitiveAuthenticationFailure(err) {
			if stateErr := lease.MarkAuthenticationRequired(err.Error()); stateErr != nil {
				err = errors.Join(err, stateErr)
				retryable = false
			}
		}
		cooled, switchedToBuild := false, false
		if cooldown, ok := aistudio.QuotaCooldownForError(err, time.Now()); ok {
			cooled = true
			modelAccessScope := lease.CooldownScope(modelID)
			scopeLabel := modelAccessScope
			if cooldown.Global {
				modelAccessScope = ""
				scopeLabel = "全局"
			}
			stateErr := service.pool.MarkCooldownIfGeneration(
				request.AccountID, modelAccessScope, lease.ModelAccessGeneration(), lease.CheckedAt(),
				cooldown.Until, cooldown.Reason,
			)
			if stateErr != nil {
				err = errors.Join(err, stateErr)
				retryable = false
			} else {
				service.requests.log(accountLabel, "WARN", fmt.Sprintf(
					"账号冷却 | 类型=%s | 范围=%s | 恢复=%s",
					cooldown.Kind, scopeLabel, cooldown.Until.Format(time.RFC3339),
				))
				// 每日限额：该模型已判定两个通道共用额度时，同时冷却另一个通道，
				// 避免请求再去另一个通道失败一次、再换号重试
				if !cooldown.Global && cooldown.Kind == dailyQuotaKind {
					other, coolOther := service.quota.dailyLimitHit(
						request.AccountID, modelID, lease.Channel(), lease.CheckedAt(), time.Now(),
					)
					if coolOther {
						otherScope := aistudio.ChannelCooldownScope(other, modelID)
						if otherErr := service.pool.MarkCooldownIfGeneration(
							request.AccountID, otherScope, lease.ModelAccessGeneration(), lease.CheckedAt(),
							cooldown.Until, "每日限额（与另一通道共用额度，同步冷却）: "+strings.TrimPrefix(cooldown.Reason, dailyQuotaKind+": "),
						); otherErr == nil {
							service.requests.log(accountLabel, "WARN", fmt.Sprintf(
								"账号冷却 | 类型=%s | 范围=%s | 恢复=%s | 与另一通道共用额度，同步冷却",
								cooldown.Kind, otherScope, cooldown.Until.Format(time.RFC3339),
							))
						}
					}
				}
				if !cooldown.Global && service.pool.AccountChannelAvailable(request.AccountID, selection) {
					delete(attempted, request.AccountID)
					maxAttempts++
				}
			}
		}
		// 同一账号在短时间内对多个模型连续返回 403 无权限：自动暂停一段时间，不再参与调度
		if isPermissionDenied(err) {
			now := time.Now()
			if pause, count, models := service.forbidden.record(request.AccountID, modelID, now); pause {
				until := now.Add(forbiddenPause)
				reason := fmt.Sprintf("%d 分钟内 %d 个模型连续 %d 次返回 403 无权限，自动暂停",
					int(forbiddenWindow/time.Minute), models, count)
				stateErr := service.pool.MarkCooldownIfGeneration(
					request.AccountID, "", lease.ModelAccessGeneration(), lease.CheckedAt(), until, reason,
				)
				if stateErr == nil {
					service.requests.log(accountLabel, "WARN", fmt.Sprintf(
						"账号自动暂停 | 原因=%s | 暂停=%s | 恢复=%s",
						reason, forbiddenPause, until.Format(time.RFC3339),
					))
				}
			}
		}
		// Playground 对部分账号的模型输入另有上限（如 131072 token），Build 通道按模型本身的上限处理：
		// 改走 Build 再试，同一账号也可以用。Build 也失败时按原错误返回
		if !buildOnly && lease.Channel() == aistudio.ChannelPlayground && inputTokenLimitError(err) {
			buildOnly = true
			switchedToBuild = true
			retryable = true
			delete(attempted, request.AccountID)
			maxAttempts++
			trace.Note("Playground 返回输入 token 超过上限，改用 Build 通道重试")
			service.requests.logRequestProgress(request.ID, accountLabel, "WARN", "Playground 输入超过上限，改用 Build 通道重试 | 原因="+progressReason(err))
		}
		releaseErr := lease.Release()
		lease = nil
		if releaseErr != nil {
			err = errors.Join(err, releaseErr)
			break
		}
		if requestCtx.Err() == nil {
			var resetErr error
			recoverWorker, _, resetErr = service.recoverWorkerOnce(
				requestCtx, request.AccountID, workerGeneration, recoveredWorker,
				needsWAARuntimeRecovery(err, false, workerFailed, workerReplaced),
				workerFailed || waaRuntimeFailed,
			)
			if resetErr != nil {
				err = errors.Join(err, resetErr)
				retryable = false
			}
		}
		if !retryable {
			break
		}
		if recoverWorker {
			recoveryAccountID = request.AccountID
			delete(attempted, request.AccountID)
			maxAttempts++
		}
		if attempt+1 == maxAttempts {
			break
		}
		if !recoverWorker && !cooled && !switchedToBuild {
			hardFailures++
			if hardFailures >= generateFailureLimit {
				service.requests.logRequestProgress(request.ID, accountLabel, "WARN", fmt.Sprintf(
					"已连续 %d 次失败，不再换号 | 原因=%s", hardFailures, progressReason(err)))
				break
			}
			if waitErr := waitRetryBackoff(requestCtx, hardFailures); waitErr != nil {
				break
			}
		}
		if recoverWorker {
			service.requests.log(accountLabel, "WARN", fmt.Sprintf(
				"WAA Worker 重建 | 模型=%s | 重放当前请求\n原因: %s", modelID, strings.TrimSpace(err.Error()),
			))
			service.requests.logRequestProgress(request.ID, accountLabel, "WARN", "WAA Worker 重建，重放当前请求 | 原因="+progressReason(err))
			continue
		}
		switchMessage := fmt.Sprintf(
			"账号切换 | 模型=%s\n原因: %s",
			modelID, strings.TrimSpace(err.Error()),
		)
		service.requests.log(accountLabel, "WARN", switchMessage)
		// 换号原因同时写进请求时间线：请求详情里能直接看到每一次失败的原因
		service.requests.logRequestProgress(request.ID, accountLabel, "WARN", "换号重试 | 原因="+progressReason(err))
	}
	if err == nil && (lease == nil || source == nil) {
		// 兜底：循环结束却没有拿到可用的上游流时按没有可用账号结束，不能把空租约交给 forwardEvents
		err = fmt.Errorf("%w: 模型 %s 没有可用账号", aistudio.ErrNoEligibleAccount, modelID)
	}
	if err != nil {
		if activity != nil {
			api.SetAccessLogUpstreamBytes(requestCtx, activity.bytes.Load())
		}
		api.SetAccessLogError(requestCtx, err)
		service.requests.finish(request.ID, finalRequestState(err), err)
		select {
		case destination <- aistudio.Event{Kind: aistudio.EventError, Err: err}:
		case <-clientCtx.Done():
		}
		cancel()
		close(destination)
		return
	}
	service.forwardEvents(
		clientCtx, requestCtx, cancel, request.ID,
		first, source, destination, lease, temporaryCopies, activity, modelID, diag,
	)
}

// generateFailureLimit 为单个请求非额度类失败的换号上限
const generateFailureLimit = 5

// generateRetryBackoff 返回第 failures 次非额度类失败后换号前的等待时间：200ms 起翻倍，最长 2 秒
func generateRetryBackoff(failures int) time.Duration {
	delay := 200 * time.Millisecond
	for index := 1; index < failures && delay < 2*time.Second; index++ {
		delay *= 2
	}
	return min(delay, 2*time.Second)
}

// waitRetryBackoff 在换号前退避；请求被取消时返回错误
func waitRetryBackoff(ctx context.Context, failures int) error {
	timer := time.NewTimer(generateRetryBackoff(failures))
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

var errStreamClosedBeforeFirstEvent = errors.New("AI Studio stream closed before first event")
var errStreamClosedBeforeFinish = errors.New("AI Studio stream closed before finish")

// firstGenerateEvent 等待首事件并在请求期限内完成错误流清理
func firstGenerateEvent(ctx context.Context, source <-chan aistudio.Event, onWait func()) (aistudio.Event, error) {
	timer := time.NewTimer(streamStallThreshold)
	defer timer.Stop()
	wait := timer.C
	for {
		select {
		case event, ok := <-source:
			if !ok {
				return aistudio.Event{}, errStreamClosedBeforeFirstEvent
			}
			if event.Kind != aistudio.EventError {
				return event, nil
			}
			if event.Err == nil {
				event.Err = errors.New("AI Studio stream returned an empty error event")
			}
			for {
				select {
				case _, ok := <-source:
					if !ok {
						return aistudio.Event{}, event.Err
					}
				case <-ctx.Done():
					return aistudio.Event{}, ctx.Err()
				}
			}
		case <-wait:
			if onWait != nil {
				onWait()
			}
			wait = nil
		case <-ctx.Done():
			return aistudio.Event{}, ctx.Err()
		}
	}
}

// inputTokenLimitError 判断上游返回的"输入 token 超过上限"错误
func inputTokenLimitError(err error) bool {
	return err != nil && strings.Contains(err.Error(), "input token count exceeds the maximum number of tokens allowed")
}

func retryableGenerateAccountError(ctx context.Context, err error) bool {
	if errors.Is(err, errStreamClosedBeforeFirstEvent) || errors.Is(err, aistudio.ErrAccountCoolingDown) {
		return true
	}
	var workerInitError *accountWorkerInitError
	if errors.As(err, &workerInitError) {
		return ctx.Err() == nil
	}
	if ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var rpcError *aistudio.RPCError
	if !errors.As(err, &rpcError) {
		return false
	}
	return rpcError.StatusCode == http.StatusUnauthorized || rpcError.StatusCode == http.StatusForbidden || rpcError.StatusCode == http.StatusNotFound ||
		rpcError.StatusCode == http.StatusTooManyRequests || rpcError.StatusCode >= http.StatusInternalServerError
}

func (service *trackedService) lifecycleRequestContext(ctx context.Context) (context.Context, context.CancelFunc) {
	requestCtx, cancel := context.WithTimeout(ctx, time.Duration(service.timeout.Load()))
	stopLifecycle := context.AfterFunc(service.lifecycle, cancel)
	return requestCtx, func() {
		stopLifecycle()
		cancel()
	}
}

func (service *trackedService) dataRequestContext(ctx context.Context) (context.Context, context.CancelFunc, error) {
	service.lifecycleMu.Lock()
	if service.state.Load() != serviceRunning || service.dataContext == nil {
		service.lifecycleMu.Unlock()
		return nil, nil, &serviceStoppedError{}
	}
	requestCtx, cancel := context.WithTimeout(ctx, time.Duration(service.timeout.Load()))
	stopData := context.AfterFunc(service.dataContext, cancel)
	service.lifecycleMu.Unlock()
	return requestCtx, func() {
		stopData()
		cancel()
	}, nil
}

func (service *trackedService) forwardEvents(
	clientCtx context.Context,
	requestCtx context.Context,
	cancel context.CancelFunc,
	requestID string,
	first aistudio.Event,
	source <-chan aistudio.Event,
	destination chan<- aistudio.Event,
	lease *aistudio.AccountLease,
	temporaryCopies *aistudio.TemporaryFileCopies,
	activity *upstreamActivity,
	requestedModelID string,
	diag *generationDiagnostics,
) {
	state := "completed"
	trace := aistudio.TraceFromContext(requestCtx)
	replyBytes := 0
	var requestErr error
	terminal := false
	accountLabel := lease.Account().Config.Label
	// 降级判定要知道本次的账号与通道（Build 通道的 CountTokens 在同一账号上计数）
	guard := diag.guard()
	guard.attach(lease, requestCtx)
	// 上游标明的模型：Build 通道前面的内容块沿用请求的模型名，最后一块（含最终用量）报告实际服务的模型，
	// 所以每次变化都核对；管理日志里的"模型"仍是请求的模型，实际模型另记
	servedModel, servedMismatch := "", false
	noteServedModel := func(model string) {
		if model == "" || model == servedModel {
			return
		}
		servedModel = model
		if !servedMismatch {
			servedMismatch = service.servedModelDiffers(requestCtx, requestID, accountLabel, requestedModelID, model, string(lease.Channel()))
		}
	}
	noteServedModel(first.ProviderModel)
	accessGeneration := lease.ModelAccessGeneration()
	modelID := strings.TrimPrefix(strings.TrimSpace(first.ProviderModel), "models/")
	var lastEventAt time.Time
	lastEventKind := "-"
	reasoningEvents := 0
	contentEvents := 0
	// replyDigest 对回复正文计算指纹，用于在诊断中找出内容完全相同的回复（不保存内容本身）
	replyDigest := sha256.New()
	var usage *aistudio.Usage
	toolCalls := 0
	stalled := false
	stallTimer := time.NewTimer(streamStallThreshold)
	if !stallTimer.Stop() {
		<-stallTimer.C
	}
	defer stallTimer.Stop()
	var stall <-chan time.Time
	resetStallTimer := func() {
		if !stallTimer.Stop() {
			select {
			case <-stallTimer.C:
			default:
			}
		}
		stallTimer.Reset(streamStallThreshold)
		stall = stallTimer.C
	}
	finishFromContext := func() {
		requestErr = requestCtx.Err()
		state = finalRequestState(requestErr)
		if requestErr == nil || terminal || clientCtx.Err() != nil {
			return
		}
		select {
		case destination <- aistudio.Event{Kind: aistudio.EventError, Err: requestErr}:
		case <-clientCtx.Done():
		}
	}
	defer cancel()
	defer func() {
		api.SetAccessLogUpstreamBytes(requestCtx, activity.bytes.Load())
		api.SetAccessLogGenerationResult(requestCtx, usage, toolCalls)
		if temporaryCopies != nil {
			if err := temporaryCopies.Cleanup(); err != nil {
				service.requests.log(accountLabel, "WARN", "临时文件清理失败 | 错误="+err.Error())
			}
		}
		if err := lease.Release(); err != nil {
			state = "failed"
			requestErr = errors.Join(requestErr, err)
			if clientCtx.Err() == nil {
				select {
				case destination <- aistudio.Event{Kind: aistudio.EventError, Err: err}:
				case <-clientCtx.Done():
				}
			}
		}
		if rejection := guard.rejection(); rejection != nil {
			// 降级判定拒绝了这次回复并取消了上游：记为失败并写明原因，而不是"客户端取消"
			state, requestErr = "failed", rejection
		}
		api.SetAccessLogError(requestCtx, requestErr)
		service.requests.finish(requestID, state, requestErr)
		close(destination)
	}()
	pendingFirst := true
	verified := false
	for {
		var event aistudio.Event
		var ok bool
		if pendingFirst {
			event = first
			ok = true
			pendingFirst = false
		} else {
			select {
			case event, ok = <-source:
			case <-stall:
				stalled = true
				stall = nil
				service.requests.logRequestProgress(requestID, accountLabel, "WARN", fmt.Sprintf(
					"事件流停顿 | 模型=%s | 已等待=%s | 最近事件=%s | 推理=%d | 正文=%d | %s",
					modelID, streamStallThreshold, lastEventKind, reasoningEvents, contentEvents,
					activity.logFields(time.Now()),
				))
				continue
			case <-requestCtx.Done():
				finishFromContext()
				return
			}
		}
		if !ok {
			if err := requestCtx.Err(); err != nil {
				finishFromContext()
			} else if !terminal {
				requestErr = errStreamClosedBeforeFinish
				state = "failed"
				select {
				case destination <- aistudio.Event{Kind: aistudio.EventError, Err: requestErr}:
				case <-clientCtx.Done():
				}
			}
			return
		}
		now := time.Now()
		if stalled {
			service.requests.logRequestProgress(requestID, accountLabel, "INFO", fmt.Sprintf(
				"事件流恢复 | 模型=%s | 停顿=%s | 当前事件=%s",
				modelID, now.Sub(lastEventAt).Round(time.Millisecond), event.Kind,
			))
			stalled = false
		}
		lastEventAt = now
		lastEventKind = string(event.Kind)
		trace.RecordEvent(event)
		switch event.Kind {
		case aistudio.EventReasoning:
			reasoningEvents++
		case aistudio.EventText:
			contentEvents++
			replyDigest.Write([]byte(event.Text))
			replyBytes += len(event.Text)
		case aistudio.EventUsage:
			if event.Usage != nil {
				usage = event.Usage
			}
		case aistudio.EventToolCall:
			if event.ToolCall != nil {
				toolCalls++
			}
		}
		noteServedModel(event.ProviderModel)
		if terminal {
			continue
		}
		if event.Kind == aistudio.EventError {
			requestErr = event.Err
			if aistudio.DefinitiveAuthenticationFailure(event.Err) {
				if stateErr := lease.MarkAuthenticationRequired(event.Err.Error()); stateErr != nil {
					requestErr = errors.Join(requestErr, stateErr)
					event.Err = requestErr
				}
			}
			state = finalRequestState(event.Err)
			terminal = true
		}
		if event.Kind == aistudio.EventFinish {
			api.SetAccessLogFinishReason(requestCtx, event.FinishReason)
			if contentEvents > 0 {
				replyHash := hex.EncodeToString(replyDigest.Sum(nil))[:12]
				api.SetAccessLogReplyHash(requestCtx, replyHash)
				service.recordReplyFingerprint(
					requestID, accountLabel, string(lease.Channel()), modelID, replyHash, replyBytes, usage, diag, trace != nil,
				)
			}
			state = "completed"
			terminal = true
		}
		if terminal {
			stall = nil
		} else {
			resetStallTimer()
		}
		select {
		case destination <- event:
		case <-requestCtx.Done():
			finishFromContext()
			return
		}
		if !verified && event.Kind == aistudio.EventFinish {
			verified = true
			service.markModelAccessVerifiedAsync(
				lease.Account().ID, accountLabel, lease.CooldownScope(requestedModelID), accessGeneration, lease.CheckedAt(),
			)
		}
	}
}

// randomSeedApplicable 判断是否为普通文字生成：图片、语音合成、语音转写等请求不加随机 seed
func randomSeedApplicable(model string, config aistudio.GenerationConfig) bool {
	if config.ImageConfig != nil || config.SpeechConfig != nil || config.TranscriptionConfig != nil {
		return false
	}
	for _, modality := range config.ResponseModalities {
		if modality != aistudio.ResponseModalityText {
			return false
		}
	}
	name := strings.ToLower(model)
	for _, marker := range []string{"image", "imagen", "tts", "audio", "veo", "lyria", "embedding"} {
		if strings.Contains(name, marker) {
			return false
		}
	}
	return true
}

func finalRequestState(err error) string {
	if errors.Is(err, context.Canceled) {
		return "cancelled"
	}
	if err != nil {
		return "failed"
	}
	return "completed"
}

var _ aistudio.Service = (*trackedService)(nil)

const (
	// modelAccessPublishDelay 为账户与模型目录推送的合并窗口
	modelAccessPublishDelay = 3 * time.Second
	// 可用账户超过 bootstrapCacheMinAccounts 时，预热概况缓存 bootstrapSummaryTTL
	bootstrapCacheMinAccounts = 200
	// 账户上万时一次统计要几百毫秒（最长 3 秒），5 秒刷新一次仍占约 7% 的锁时间；预热目标按分钟变化，60 秒足够
	bootstrapSummaryTTL = 60 * time.Second
)

// bootstrapSummary 返回预热概况；账户多时缓存几秒。统计要在账户池锁内扫描全部账户的模型（账户多时几百毫秒），
// 预热循环与状态接口频繁调用，诊断中占了约两成的账户池锁时间；预热目标本身按分钟变化，不需要实时
func (manager *accountWorkerManager) bootstrapSummary() aistudio.BootstrapSummary {
	manager.bootstrapMu.Lock()
	cached, at := manager.bootstrapCache, manager.bootstrapAt
	manager.bootstrapMu.Unlock()
	if !at.IsZero() && cached.Available > bootstrapCacheMinAccounts && time.Since(at) < bootstrapSummaryTTL {
		return cached
	}
	summary := manager.pool.BootstrapSummary()
	manager.bootstrapMu.Lock()
	manager.bootstrapCache, manager.bootstrapAt = summary, time.Now()
	manager.bootstrapMu.Unlock()
	return summary
}
