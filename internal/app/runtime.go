package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
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
	proxyPool *proxyPool,
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
	// 账户默认启用：先载入停用原因（载入前写出上一代实例未保存的变化），再在建立账户池、固定出口与 Worker 之前
	// 把没有手动停用记录的停用账户改为启用，后续的状态推导与模型目录同步把它们当作普通的启用账户
	accountDisableNotes.load(store.PrimaryDirectory(), requests)
	enableUnmarkedAccounts(accounts, requests, "启动时")

	requests.log("service", "INFO", "运行时装配 | 3/3 | 创建协议客户端")
	// IP 池（进程级）先按本次配置生效，再给还没进入账户池的账户对象写默认出口：账户池、公共头与 Go 侧请求第一次就按带绑定的
	// EffectiveProxy 取出口（见 proxy_pool.go）
	proxyPool.configure(cfg.ProxyPool, cfg.Proxy, proxyPoolCapacity(cfg))
	for _, account := range accounts {
		proxyPool.prime(account)
	}
	pool := aistudio.NewAccountPool(accounts, cfg.PerAccountConcurrency)
	configurePool(pool, cfg)
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
	workers.setUltraCapacity(cfg.UltraWarmWorkerLimit, cfg.UltraMaxActiveWorkers)
	// 按需替换与扩容的启动宽限、每分区每分钟的替换上限（N6，见 replace_rate.go）
	workers.replaceGrace.Store(int64(rotationWarmupGrace))
	workers.replacements.limit.Store(replacementsPerMinute)
	workers.proxyPool = proxyPool
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
	workers.refresher = refresher
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
	// 出口限流判定（见 egress_guard.go）与冷启动闸门：闸门按账户取出口键与分区通道，补位并发按 WARM_STARTUP_CONCURRENCY 计算（热更新即生效）
	service.egress = newEgressGuard(pool, requests)
	coldStartGate := service.egressColdStartGate()
	workers.coldStartGate.Store(&coldStartGate)
	// 坏号记忆（见 bad_accounts.go）：连续裸限流的账号移出冷启动候选，出口判定的“冷号首发被拒”不计它们；记录跨重启保留
	badAccounts := openBadAccountMemory(badAccountPath(), requests)
	workers.badAccounts.Store(badAccounts)
	service.egress.badAccount = badAccounts.bad
	// 登录复核（见 auth_recheck.go）：租约路径上的上游 401 先记疑似登录失效、写短冷却，用磁盘 Cookie 复核仍是 401 才标需要登录
	service.authRecheck = newAuthRecheck(service)
	if service.authRecheck != nil {
		pool.SetAuthFailureGate(service.authRecheck.gate)
	}
	service.firstEventTimeout.Store(int64(cfg.FirstEventTimeout))
	// 账户因登录失效退出调度后用合并推送更新管理页，不为单个账户立即推送全部账户状态
	refresher.publish = service.publishModelAccess
	service.quota = newQuotaSharing(store.PrimaryDirectory(), requests)
	service.permissionDenied = openPermissionDeniedRegistry(store.PrimaryDirectory(), requests)
	// 续签换掉登录签名 Cookie 时在提交处更新登记记下的登录签名，不当作重新上传（见 authRuntimeRefresher.renewed）
	refresher.renewed = service.permissionDenied.setIdentity
	// 服务停止期间替换了认证文件或删除了账户目录：账户池与登记表都载入后对照一次（账户目录扫描第一次看到的文件只记作基准）
	service.reconcilePermissionDenied()
	service.ignoreSeed.Store(cfg.IgnoreClientSeed)
	service.repeatNonce.Store(cfg.RepeatPromptNonce)
	service.minOutputTokens.Store(int64(cfg.MinOutputTokens))
	service.setDowngradeGuard(cfg.DowngradeGuard)
	service.setStreamPlaygroundModels(cfg.StreamPlaygroundModels)
	service.setPrimaryModelSettings(cfg.PrimaryModels, cfg.PrimaryQueueWait, cfg.UltraPrimaryModels, cfg.UltraPrimaryQueueWait)
	// 模型目录写盘缓存：一次加锁恢复账户池与 Client 的目录与权益（含 Pro/Ultra 的权益头），启动时不发目录请求即可预热，
	// 后台只低速刷新缺失与过期的账户（见 catalog_cache.go、catalog_sync.go）。要在账户目录扫描与新账户处理开始之前
	catalogCache := openCatalogCache(catalogCacheDir, requests)
	service.catalogSync.attach(catalogCache, catalogCache.restore(pool, client, pool.BuildEnabled()))
	admin := newRuntimeAdmin(lifecycle, pool, store, service, requests, login, workers, headers, cfg)
	// 新账户自动处理：先载入处理记录，再开始扫描目录，保证扫描导入的账户能进入队列
	admin.onboard = newAccountOnboarder(admin, store.PrimaryDirectory())
	go admin.onboard.run(lifecycle)
	// 生成服务实例存活期间自动识别新增、删除与重新登录的账户目录
	go admin.watchAccountDirectories(lifecycle)
	// 装配成功后 IP 池的推送目标切换为新账户池（装配失败时旧生成服务的账户池仍是推送目标）；之后旧生成服务的 refresh 被忽略，
	// 旧实例浏览器的归还按账户 ID 推给新账户池。端口冷却的回调同时换成新生成服务的（撤回误伤的逐级冷却、换端口重启 Worker，
	// 见 proxy_pool_egress.go）
	proxyPool.setPortCooledHook(service.proxyPortCooled)
	proxyPool.attach(pool)
	requests.log("service", "INFO", fmt.Sprintf(
		"协议运行时就绪 | 账户=%d | 耗时=%s",
		len(accounts), time.Since(startedAt).Round(time.Millisecond),
	))
	closeRuntime := func() error {
		// 停用原因与新账户处理记录都是合并几秒后才写文件，关闭时立即写出，退出前的变化不会丢失
		admin.onboard.flush()
		accountDisableNotes.flush()
		err := workers.Close()
		// Worker 关闭时归还的端口改变了亲和与归属：立即写出 IP 池记录
		proxyPool.flush()
		transport.CloseIdleConnections()
		headers.Close()
		// 403 无权限账户的登记平时几秒后合并写入，关闭时立即写入，进程退出也不丢最后几秒的变化
		service.permissionDenied.flush()
		// 模型目录缓存平时最多每 30 秒写一次，停止服务时调度器退出前已写过；这里兜底（没有变化时是空操作）
		if flushErr := catalogCache.close(); flushErr != nil {
			requests.log("service", "WARN", "模型目录缓存写入失败 | 错误="+flushErr.Error())
		}
		// 坏号记忆平时最多每 30 秒（badAccountSaveInterval）写一次，停止服务时已写过；关闭时再兜底写出
		if flushErr := badAccounts.close(); flushErr != nil {
			requests.log("service", "WARN", "坏号记忆写入失败 | 错误="+flushErr.Error())
		}
		return err
	}
	return service, admin, closeRuntime, nil
}

// configurePool 把配置里属于账户池的设置写入新建的账户池：路由策略、上游通道、ULTRA_INCLUDE_PRO 与单账户每分钟上限（ACCOUNT_RPM_LIMIT）。
// 必须在创建 Worker 管理器、同步目录与预热之前调用，第一次 Worker 分区与预热目标就按正确的号池成员计算
func configurePool(pool *aistudio.AccountPool, cfg config.Config) {
	// 出口键按账户固定代理或全局代理计算（见 aistudio.EgressKey）：先设全局代理，账户按出口分组；PROXY 变化本来就要重建运行时
	pool.SetGlobalProxy(cfg.Proxy)
	pool.SetRoutingStrategy(cfg.RoutingStrategy)
	pool.SetUpstreamChannels(upstreamChannels(cfg.UpstreamChannels))
	pool.SetUltraIncludePro(cfg.UltraIncludePro)
	// 账户池构造时不限速（0），这里按配置打开；运行中由 applyLiveFields 热更新
	pool.SetAccountRateLimit(cfg.AccountRPMLimit)
}

// accountWorkerManager 管理每账户的长驻 WAA worker
type accountWorkerManager struct {
	// replacements 为各分区最近一分钟的按需替换（每分钟上限，见 replace_rate.go）；replaceGrace 为按需替换与扩容不换刚就绪的 Worker 的宽限
	// （见 pickReplaceVictim）。两者都只在 newRuntime 打开，测试直接构造的管理器不限
	replacements replacementRate
	replaceGrace atomic.Int64
	// badAccounts 为坏号记忆（连续裸限流的账号移出冷启动候选，见 bad_accounts.go）；只在 newRuntime 挂上，测试构造的管理器为空（不调整候选）
	badAccounts atomic.Pointer[badAccountMemory]
	// bootstrapCache 为预热概况的短时缓存（见 bootstrapSummary）；bootstrapVersion 为计算前读到的号池规则版本
	bootstrapMu      sync.Mutex
	bootstrapCache   aistudio.BootstrapSummary
	bootstrapAt      time.Time
	bootstrapVersion uint64
	mu               sync.RWMutex
	fillMu           sync.Mutex
	rebalanceMu      sync.Mutex
	pool             *aistudio.AccountPool
	accounts         map[string]*accountWorker
	openings         map[string]chan struct{}
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
	ultraWarmTarget atomic.Int64
	ultraMaxActive  atomic.Int64
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
	// retirements 为账号进入长时间冷却、等待空闲后淘汰的驻留 Worker（见 worker_retire.go），retirePending 为其数量的无锁镜像；
	// retireWake 唤醒淘汰循环（容量 1，重复唤醒合并）；afterRetire 在淘汰成功后补齐预热，由生成服务按运行状态设置
	retireMu      sync.Mutex
	retirements   map[string]*retirement
	retirePending atomic.Int32
	retireWake    chan struct{}
	afterRetire   atomic.Pointer[func()]
	// drains 为排空中的账号（见 worker_drain.go），由 drainMu 保护；账户池的排空标记只在 drainMu 内设置与清除，两边始终一致
	drainMu sync.Mutex
	drains  map[string]*drainState
	// resets 为进行中的后台关闭失败 Worker（见 worker_reset.go），按账户与 Worker 版本登记，由 resetMu 保护（只在登记与移除时短暂持有）；
	// resetHolds 为 ResetAll 暂停登记的层数，resetClosed 在 Close 后为真，这两种情况下不再登记；resetWG 跟踪后台 goroutine，
	// ResetAll 与 Close 取消后等它们退出；resetLimit 为后台等待独占租约的上限（纳秒，0 为排空超时），测试调低
	resetMu     sync.Mutex
	resets      map[workerResetKey]*workerReset
	resetHolds  int
	resetClosed bool
	resetWG     sync.WaitGroup
	resetLimit  atomic.Int64
	// primaryModels 为两份主用模型（PRIMARY_MODELS 与 ULTRA_PRIMARY_MODELS，由生成服务设置），按分区与热门模型一起判断死槽位、
	// 挑替补与放置请求（见 worker_drain.go）；
	// requestTimeout 为请求超时（REQUEST_TIMEOUT），排空超时按它与总时长上限计算；serviceActive 返回生成服务是否在运行，替补只在运行时启动
	primaryModels  atomic.Pointer[workerPrimaryModels]
	requestTimeout atomic.Int64
	serviceActive  atomic.Pointer[func() bool]
	// primarySupply 为 keepWarm 定期统计的主用模型供给（见 primary_supply.go），状态接口只读缓存
	primarySupply  atomic.Pointer[primarySupplySnapshot]
	backgroundMu   sync.Mutex
	background     context.Context
	stopBackground context.CancelFunc
	// startupSlots 限制同时冷启动的浏览器 Worker 数，容量随 warmConcurrency 热更新（见 startupSlots）
	startupSlots startupSlots
	// refresher 在 Worker 启动阶段确认登录失效时恢复认证；为空时只把账户标为需要登录
	refresher *authRuntimeRefresher
	// launch 启动单个账户的 WAA Worker，为空时使用 newWAAWorker（测试替换为不启动浏览器的实现）
	launch func(context.Context, string, camoufoxnative.Options) (*aistudio.NativeWorker, error)
	// coldStartGate 为冷启动闸门（出口限流期间按路径放行，见 egress_guard.go）；nil 时不限
	coldStartGate atomic.Pointer[coldStartGateFunc]
	// authSuspects 为疑似登录失效的账户（目录缓存记过需要登录、认证文件没变），预热时排在最后（见 catalog_sync.go）；nil 时为空
	authSuspects atomic.Pointer[map[string]struct{}]
	// authRequired 为账户确认需要登录后写“账号需要登录”日志的生成服务回调（见 worker_unschedulable.go）；newTrackedService 挂上，
	// nil 时只登记释放 Worker、不写日志
	authRequired atomic.Pointer[authRequiredHook]
	// proxyPool 为进程级 IP 池（见 proxy_pool.go）：没有自己代理的账户每次启动 Worker 时申请出口租约，浏览器确认关闭后归还。
	// 测试构造的管理器为 nil（方法对 nil 安全），行为与没有 IP 池时完全一样
	proxyPool *proxyPool
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
	// busyUntil（UnixNano，0 为没有）与 busyErr 为被其他进程占用时的退避期限与原因：每次调度都要对上万个备用账户读一遍，
	// 用原子值读取，不取账户锁（取账户锁会让 occupiedWorkersIn 把正被读取的备用账户误算成占用的浏览器槽位）；
	// 写入与 busyDelay 一样在账户锁内进行
	busyUntil atomic.Int64
	busyDelay time.Duration
	busyErr   atomic.Pointer[error]
	// cleanupRetryAt 与 cleanupBackoff 为清理失败后下一次后台重试的时间与间隔（由 account.mu 保护）
	cleanupRetryAt time.Time
	cleanupBackoff time.Duration
	// ownProxy 表示账户固定了自己的代理（account.json 的 proxy 非空）：启动 Worker 时不向 IP 池申请出口；由 mu 保护
	ownProxy bool
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
	// source 为冷启动的来源（预热、替补、扩容、按需、补覆盖、重开，见 worker_start_source.go），写进“WAA Worker 就绪”
	source string
}

type accountWorkerUpdate struct {
	account  *accountWorker
	config   camoufoxnative.Options
	ownProxy bool
	label    string
	pending  bool
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

// Prepare 在账户 Worker 有效期间生成 proof。proof 每次要 1～5 秒，在账户锁外执行：原先整段持有 account.mu，
// 同账户的状态读取（WorkerFailed、runtimeAvailable、预热巡检等）和取 Worker 都要排在 proof 之后。
// 执行前后各在锁内核对一次，执行期间 Worker 被关闭或替换时丢弃结果、按 Worker 已更新返回，由上层重建后重放；
// 同账户的 proof 仍由 Worker 自身的 operationMu 串行生成，关闭也要等进行中的 proof 结束
func (preparer *accountWorkerPreparer) Prepare(ctx context.Context, request aistudio.ProtectedRequest) (aistudio.PreparedProtectedRequest, error) {
	if !preparer.current() {
		return aistudio.PreparedProtectedRequest{}, errAccountWorkerReplaced
	}
	// 只对 Worker 的准备计时并计入请求日志的 proof_ms（含排在同账户进行中的 proof 之后的等待），
	// 同样在账户锁外完成，不含前后两次核对；失败或 Worker 被替换的尝试也计入，累计值反映请求在 proof 上花的全部时间
	startedAt := time.Now()
	prepared, err := preparer.worker.Prepare(ctx, request)
	api.AddAccessLogProof(ctx, time.Since(startedAt))
	if !preparer.current() {
		return aistudio.PreparedProtectedRequest{}, errAccountWorkerReplaced
	}
	return prepared, err
}

// SendProtected 校验当前账户 Worker 后发送浏览器请求
func (preparer *accountWorkerPreparer) SendProtected(ctx context.Context, request aistudio.ProtectedRequest) (*aistudio.RPCResponse, error) {
	if !preparer.current() {
		return nil, errAccountWorkerReplaced
	}
	return preparer.worker.SendProtected(ctx, request)
}

// BrowserStorageState 返回同一有效账户 Worker 的浏览器 Cookie 状态。导出同样在账户锁外执行，
// 执行后再核对一次：Worker 在导出期间被替换（如续签后重建）时，旧浏览器的 Cookie 不能再写回账户状态
func (preparer *accountWorkerPreparer) BrowserStorageState(ctx context.Context) (aistudio.StorageState, error) {
	if !preparer.current() {
		return aistudio.StorageState{}, errAccountWorkerReplaced
	}
	state, err := preparer.worker.BrowserStorageState(ctx)
	if !preparer.current() {
		return aistudio.StorageState{}, errAccountWorkerReplaced
	}
	return state, err
}

// current 在账户锁内核对 preparer 的 Worker 仍是账户当前发布的 Worker：没有被替换或关闭（指针不变），
// 页面模型不变，也没有开始关闭（关闭失败时 Worker 仍挂在账户上，状态停在 WorkerClosing）
func (preparer *accountWorkerPreparer) current() bool {
	preparer.account.mu.Lock()
	defer preparer.account.mu.Unlock()
	if preparer.account.worker != preparer.worker || preparer.account.bootstrapModel != preparer.bootstrapModel {
		return false
	}
	phase := preparer.worker.State().Phase
	return phase != aistudio.WorkerClosing && phase != aistudio.WorkerClosed
}

// accountWorkerInitError 表示单个账户的 WAA worker 初始化失败
type accountWorkerInitError struct {
	err error
	// authHandled 表示启动阶段已经处理过登录失效（已恢复一次或已标为需要登录），上层不再恢复第二次
	authHandled bool
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
		victims: make(map[string]struct{}), retirements: make(map[string]*retirement), retireWake: make(chan struct{}, 1),
		drains: make(map[string]*drainState), resets: make(map[workerResetKey]*workerReset),
	}
	manager.initTimeout.Store(int64(initTimeout))
	manager.warmTarget.Store(int64(warmTarget))
	manager.maxActive.Store(int64(maxActive))
	manager.setUltraCapacity(defaultUltraCapacity())
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
	go manager.retireLoop()
	return manager
}

// expandInBackground 在后台为账户启动 Worker，就绪后由调度信号唤醒排队请求。
// 它由正在排队的请求发起，冷启动名额按按需启动计（不加 withBackgroundStartup）
// 返回的通道在这次扩容结束（就绪、失败或被取消）时关闭，发起扩容的选号据此只等自己这一个，不再连着扩容别的账户
func (manager *accountWorkerManager) expandInBackground(accountID string, modelID string) <-chan struct{} {
	ctx := withWorkerStartSource(manager.backgroundContext(), workerStartExpand)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, err := manager.ensureWorker(ctx, accountID, modelID, false)
		if err == nil || ctx.Err() != nil || errors.Is(err, errAccountWorkerOpening) ||
			errors.Is(err, errAccountWorkerCapacity) || errors.Is(err, errAccountRuntimeBusy) {
			return
		}
		manager.requests.log(accountID, "WARN", fmt.Sprintf("WAA Worker 后台扩容失败 | 错误=%v", err))
	}()
	return done
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
	busyErr := fmt.Errorf("%w: %w", errAccountRuntimeBusy, cause)
	// 先写原因再写期限：读到期限未到的一方总能拿到原因
	account.busyErr.Store(&busyErr)
	account.busyUntil.Store(time.Now().Add(delay).UnixNano())
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
	account.busyUntil.Store(0)
	account.busyErr.Store(nil)
	label := account.label
	account.mu.Unlock()
	if wasBusy {
		manager.requests.log(label, "INFO", "WAA Worker 账户占用解除")
	}
}

// runtimeAvailable 过滤仍在占用退避期内的账户，并返回其中一个占用原因。只读原子值、不取账户锁：
// 每次调度都要对上万个备用账户调用，原先逐个取账户锁，同时让 occupiedWorkersIn 把正被读取的账户按“锁被占用”误算成浏览器槽位
func (manager *accountWorkerManager) runtimeAvailable(accountIDs []string) ([]string, error) {
	nowNanos := time.Now().UnixNano()
	available := make([]string, 0, len(accountIDs))
	var busyErr error
	manager.mu.RLock()
	defer manager.mu.RUnlock()
	for _, accountID := range accountIDs {
		account := manager.accounts[accountID]
		if account == nil {
			available = append(available, accountID)
			continue
		}
		if until := account.busyUntil.Load(); until == 0 || nowNanos >= until {
			available = append(available, accountID)
			continue
		}
		if busyErr == nil {
			if stored := account.busyErr.Load(); stored != nil {
				busyErr = *stored
			}
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

// reapIdleWorker 关闭一个最近使用早于 before 的多余空闲 Worker，并返回是否关闭；各分区按自己的常驻数回收
func (manager *accountWorkerManager) reapIdleWorker(before time.Time) bool {
	for _, partition := range workerPartitions {
		if manager.reapIdleWorkerIn(partition, before) {
			return true
		}
	}
	return false
}

// reapIdleWorkerIn 在分区的 Worker 超出该分区常驻数时关闭其中一个空闲超时的 Worker：在热门或主用模型上长时间冷却的死槽位优先，
// 留下还能服务主用模型的 Worker
func (manager *accountWorkerManager) reapIdleWorkerIn(partition aistudio.PoolScope, before time.Time) bool {
	manager.rebalanceMu.Lock()
	defer manager.rebalanceMu.Unlock()
	ultra := manager.pool.UltraAccountIDs()
	if manager.openingIn(partition, ultra) || len(inPartition(manager.WarmAccountIDs(), ultra, partition)) <= manager.warmTargetFor(partition) {
		return false
	}
	victim := manager.pickIdleVictimFor("", "", manager.deadSlotModelsFor(partition), victimAny, partition, before)
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
			"%s 空闲回收 | Worker=%d/%d", workerLogName(partition), len(manager.warmAccountIDsIn(partition)), manager.maxActiveFor(partition),
		))
	}
	return evicted
}

// forwardPoolChanges 在账户池租约或状态变化后唤醒排队请求，并让待淘汰的冷却 Worker 在租约释放后被关闭
func (manager *accountWorkerManager) forwardPoolChanges() {
	for {
		changed := manager.pool.Changed()
		select {
		case <-manager.lifecycle.Done():
			return
		case <-changed:
			manager.dispatch.wakeHeads()
			manager.wakeRetire()
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

// notifySchedulingIfAny 与 notifyScheduling 相同，manager 为 nil 时不做任何事（登录复核恢复账户后唤醒排队的请求）
func (manager *accountWorkerManager) notifySchedulingIfAny() {
	if manager == nil || manager.dispatch == nil {
		return
	}
	manager.notifyScheduling()
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
	// 账户对象还没进入账户池（AccountPool.Add 在这之后）：先写 IP 池的默认出口，账户池挂载时按带绑定的出口计算。
	// 在 manager.mu 外调用（IP 池会取账户池的锁）；注册失败时这次写入无害
	manager.proxyPool.prime(account)
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

// ResetIfGeneration 在已登记的独占等待上等账户的活动请求结束、取得独占租约后，仅关闭产生当前失败的 Worker 版本
// （由 scheduleWorkerReset 在后台调用，见 worker_reset.go）
func (manager *accountWorkerManager) ResetIfGeneration(
	ctx context.Context, reservation *aistudio.AccountReservation, accountID string, generation uint64,
) (reset bool, err error) {
	lease, err := reservation.Wait(ctx)
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
		ownProxy: strings.TrimSpace(config.Proxy) != "",
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
	update.account.ownProxy = update.ownProxy
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
	// 先暂停登记后台关闭失败 Worker 再取消后台 context：进行中的后台关闭随之放弃，等它们退出后再逐个关闭（这里直接关闭全部 Worker）
	resumeResets := manager.pauseWorkerResets()
	defer resumeResets()
	manager.cancelBackgroundStartups()
	manager.resetWG.Wait()
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
	// 关闭失败的 Worker 不再排空：生成服务停止后不会有淘汰来清除它们的标记；主用模型供给按 Worker 统计，一并清空
	manager.clearAllDrains()
	manager.clearPrimarySupply()
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
	return &accountWorker{
		id: account.ID, label: account.Config.Label, config: manager.workerConfig(account),
		ownProxy: strings.TrimSpace(account.Config.Proxy) != "",
	}
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
	// lockBusy 为账户锁被占用、按占用 1 个槽位估计的账户（锁内可能正在启动、发布或关闭 Worker）
	lockBusy map[string]struct{}
	// drainCredit 为其中排空中、视为已让出的槽位数（见 drainCredit，只按单个分区统计时计算）：容量按 slots - drainCredit 与峰值比较
	drainCredit int
}

// effectiveSlots 返回容量判断用的占用数：排空中的 Worker 按已让出计（先启后停，见 worker_drain.go）
func (occupancy workerOccupancy) effectiveSlots() int {
	return occupancy.slots - occupancy.drainCredit
}

// occupiedWorkers 返回仍持有进程或运行锁的账户与容量槽位
func (manager *accountWorkerManager) occupiedWorkers() workerOccupancy {
	return manager.occupiedWorkersIn(aistudio.PoolScopeAll, nil)
}

// occupiedWorkersIn 返回分区内仍持有进程或运行锁的账户与容量槽位；partition 为不限号池时统计全部分区（不计排空让出的槽位）
func (manager *accountWorkerManager) occupiedWorkersIn(partition aistudio.PoolScope, ultra map[string]struct{}) workerOccupancy {
	manager.mu.RLock()
	accounts := make([]*accountWorker, 0, len(manager.accounts))
	for _, account := range manager.accounts {
		if partition == aistudio.PoolScopeAll || partitionOf(account.id, ultra) == partition {
			accounts = append(accounts, account)
		}
	}
	manager.mu.RUnlock()
	occupied := workerOccupancy{accountIDs: make([]string, 0, len(accounts))}
	for _, account := range accounts {
		// 账户锁被占用时账户正在关闭、发布或核对 Worker（WAA proof 在锁外生成），按占用 1 个槽位计，不排队等锁
		if !account.mu.TryLock() {
			occupied.accountIDs = append(occupied.accountIDs, account.id)
			occupied.slots++
			if occupied.lockBusy == nil {
				occupied.lockBusy = make(map[string]struct{})
			}
			occupied.lockBusy[account.id] = struct{}{}
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
	if partition != aistudio.PoolScopeAll {
		held := make(map[string]struct{}, len(occupied.accountIDs))
		for _, accountID := range occupied.accountIDs {
			held[accountID] = struct{}{}
		}
		occupied.drainCredit = manager.drainCredit(partition, held)
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
		// 账户锁只在核对、发布或关闭 Worker 时持有（WAA proof 在锁外生成），拿不到锁时按 warm 标志计入。
		// 这里不排队等锁：每个请求分配账号时都会调用本函数，关闭浏览器可能要几秒，逐个等待会让调度随并发线性变慢
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

// PrewarmTarget 返回当前配置需要预热的账户数：各分区的常驻数与分区内可预热的账户数取小后相加
func (manager *accountWorkerManager) PrewarmTarget() int {
	summary := manager.bootstrapSummary()
	target := 0
	for _, partition := range workerPartitions {
		target += manager.prewarmTargetFrom(partition, summary)
	}
	return target
}

// classifyBootstrapCandidates 分类分区内可预热的候选账户
func (manager *accountWorkerManager) classifyBootstrapCandidates(
	ctx context.Context,
	warm []string,
	partition aistudio.PoolScope,
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
			ModelID: modelID, Method: "generateContent", Pool: partition,
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

// Worker 在活动上限内返回账户的通用 WAA preparer。冷启动来源由调用方标记：生成请求的每次尝试标为重开（generateWithRetry：
// 租约来自 acquireLease，取得租约前已确认或启动了 Worker，发送时 Worker 不在了才要重新启动），冷启动闸门不拦它。
// 没有标记的是转写、Live、Veo 这些数据路径：它们经账户池直接取租约，候选里有没有 Worker 的备用账号，这里按“按需”经冷启动闸门，
// 原先一律记为重开，出口限流期间在受限出口上绕过了补位、补覆盖的配额。闸门暂停时按号池繁忙返回（429，带 Retry-After），不冷启动
func (manager *accountWorkerManager) Worker(ctx context.Context, accountID string, modelID string) (aistudio.ProtectedPreparer, error) {
	if workerStartSourceFrom(ctx) == "" {
		ctx = withWorkerStartSource(ctx, workerStartOnDemand)
	}
	preparer, err := manager.ensureWorker(ctx, accountID, modelID, true)
	if err != nil && errors.Is(err, errEgressColdStartPaused) {
		return nil, newEgressColdStartBusy(modelID, partitionOf(accountID, manager.pool.UltraAccountIDs()), err)
	}
	return preparer, err
}

func (manager *accountWorkerManager) readyWorker(accountID string, bootstrapModel string) (aistudio.ProtectedPreparer, bool, error) {
	preparer, ready, _, err := manager.checkReadyWorker(accountID, bootstrapModel, true)
	return preparer, ready, err
}

// checkReadyWorker 检查账户是否已有可用 Worker。wait 为 false 时不等待账户锁：
// 锁正被占用（如该账户的 Worker 正在关闭或替换）时立即返回 busy
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
	ownProxy := account.ownProxy
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
	releaseSlot, err := manager.acquireStartupSlot(ctx, label, options.ExecutablePath)
	if err != nil {
		if ownsLease {
			_ = runtimeLease.Release()
		}
		account.startupMu.Unlock()
		return nil, err
	}
	// 名额到手后才申请 IP 池出口：排队等名额的启动不占端口。只改这次启动的局部参数，accountWorker.config 不变
	poolLease := manager.workerProxyLease(account.id, label, ownProxy, &options)
	manager.requests.log(label, "INFO", "WAA Worker 启动 | 1/7 | 初始化页面 | 页面模型="+bootstrapModel)
	initTimeout := time.Duration(manager.initTimeout.Load())
	initCtx, cancel := context.WithTimeout(ctx, initTimeout)
	options.Model = bootstrapModel
	options.StartupProgress = func(stage camoufoxnative.StartupStage) {
		step, message := workerStartupProgress(stage)
		manager.requests.log(label, "INFO", fmt.Sprintf("WAA Worker 启动 | %d/7 | %s", step, message))
	}
	launch := manager.launch
	if launch == nil {
		launch = newWAAWorker
	}
	// 主用模型请求自己的现场冷启动：浏览器启动本身按实测时长延后排队截止时间（请求在等自己的 Worker，不是在排队，
	// 合计不超过 INIT_TIMEOUT，见 primary_models.go），Ultra 主用模型的流式请求在启动期间推迟写出响应头（同样以还能抵扣的时长为限）；
	// 预热与后台扩容没有预算
	var budget *leaseWaitBudget
	if !backgroundStartup(ctx) {
		budget = leaseWaitBudgetFromContext(ctx)
	}
	launchStartedAt := time.Now()
	budget.beginColdStart(initTimeout)
	worker, initErr := launch(initCtx, account.id, options)
	cancel()
	// 名额只覆盖浏览器启动本身，发布、替换与淘汰旧 Worker 不再占用
	releaseSlot()
	budget.creditColdStart(time.Since(launchStartedAt), initTimeout)
	if initErr != nil {
		// 这次启动用的出口（端口号或会话号）要在归还之前取：之后账户的出口是默认出口，不是失败的那一个
		slot := proxyPoolLogSuffix(poolLease.logField())
		// 归还出口租约：取消的启动与代理无关的失败不计入端口健康与换会话
		poolLease.launchFailed(initErr, ctx.Err() != nil)
		if ownsLease {
			_ = runtimeLease.Release()
		}
		account.startupMu.Unlock()
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		manager.requests.log(label, "ERROR", fmt.Sprintf(
			"WAA Worker 启动失败 | 页面模型=%s | 耗时=%s | 错误=%s%s",
			bootstrapModel, time.Since(startedAt).Round(time.Millisecond), strings.TrimSpace(initErr.Error()), slot,
		))
		return nil, &accountWorkerInitError{err: initErr}
	}
	// 出口租约随浏览器确认关闭（NativeWorker.Close 成功，与本机中转释放同一时机）归还：发布、替换、丢弃都经 Close
	poolLease.attach(worker)
	return &accountWorkerPreparer{
		account: account, worker: worker, bootstrapModel: bootstrapModel, manager: manager,
		runtimeLease: runtimeLease, ownsLease: ownsLease, startedAt: startedAt, pending: true,
		source: workerStartSourceFrom(ctx),
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
	// 末尾写启动来源，诊断按来源统计冷启动（预热、替补、扩容、按需、补覆盖、重开）
	source := ""
	if preparer.source != "" {
		source = " | 来源=" + preparer.source
	}
	preparer.manager.requests.log(label, "INFO", fmt.Sprintf(
		"WAA Worker 就绪 | 页面模型=%s | PID=%d | 耗时=%s%s",
		preparer.bootstrapModel, preparer.worker.State().PID, time.Since(preparer.startedAt).Round(time.Millisecond), source,
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

// tryReserveVictim 在旧 Worker 尚未被其他替换选中时标记它，返回是否标记成功。
// ensureWorker 的替换与不经过 rebalanceMu 的冷却轮换都用它原子检查并标记，避免同一个 Worker 被两边同时淘汰
func (manager *accountWorkerManager) tryReserveVictim(accountID string) bool {
	manager.victimMu.Lock()
	defer manager.victimMu.Unlock()
	if _, reserved := manager.victims[accountID]; reserved {
		return false
	}
	manager.victims[accountID] = struct{}{}
	return true
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

// idleWarmVictimFor 在分区内选择空闲热 Worker：账号已不可调度（需要登录、已停用、没有模型目录）的优先，其次账号长时间冷却的死槽位、
// 冷却中的账户，再次最久未用；coolingOnly 时只返回冷却中或不可调度的账户（分区有 Worker 正在启动时按需替换也能换掉僵尸 Worker）。
// 冷却中只算剩余不少于 aistudio.ShortCooldownMax 的冷却：分钟限额、限流这类短冷却一分钟内就恢复，
// 与健康的空闲 Worker 同档按最久未用排序，不为它优先关掉。只从同一分区选：Ultra 账户的启动不会淘汰普通 Worker，反之亦然。
// 候选的活动与冷却状态由账户池一次加锁给出（见 pickIdleVictim）
func (manager *accountWorkerManager) idleWarmVictimFor(excludeID string, modelID string, coolingOnly bool, partition aistudio.PoolScope) string {
	mode := victimAny
	if coolingOnly {
		mode = victimCooling
	}
	return manager.pickIdleVictim(excludeID, modelID, mode, partition)
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
		// 该账户的 Worker 正在关闭或替换时可能等几秒（WAA proof 已在锁外生成）；原先持有 rebalanceMu 等待，
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
		// 容量只计账户所在分区：Worker 按账户当前所在号池（权益与 ULTRA_INCLUDE_PRO 共同决定）计入普通或 Ultra 分区
		ultra := manager.pool.UltraAccountIDs()
		partition := partitionOf(accountID, ultra)
		occupancy := manager.occupiedWorkersIn(partition, ultra)
		occupied := make(map[string]struct{}, len(occupancy.accountIDs)+len(manager.openings))
		for _, occupiedAccountID := range occupancy.accountIDs {
			occupied[occupiedAccountID] = struct{}{}
		}
		for openingAccountID := range manager.openings {
			if partitionOf(openingAccountID, ultra) != partition {
				continue
			}
			occupied[openingAccountID] = struct{}{}
			// 正在启动的账户锁被占用时（启动流程短暂持有账户锁）上面已按 1 个槽位估计，不再重复计入：
			// 否则两个账户同时启动时后一个会把前一个算成两个槽位，分区还有空槽却按已满返回
			if _, estimated := occupancy.lockBusy[openingAccountID]; !estimated {
				occupancy.slots++
			}
		}
		_, replacing := occupied[accountID]
		// 排空中的 Worker 按已让出槽位计（最多 drainLimit 个）：替补可以在旧 Worker 排空前启动，分区浏览器总数不超过 峰值 + drainLimit
		if replacing || occupancy.effectiveSlots() < manager.maxActiveFor(partition) {
			// 冷启动闸门（出口限流期间按路径放行，见 egress_guard.go）：低于常驻数的补位只受并发约束，补覆盖另受每分钟配额，
			// 已不低于常驻数的扩容暂停；账户自己重开不经闸门
			path := coldStartExpand
			switch {
			case workerStartSourceFrom(ctx) == workerStartCoverage:
				path = coldStartCoverage
			case occupancy.effectiveSlots() < manager.warmTargetFor(partition):
				path = coldStartRefill
			}
			releaseGate, gateErr := manager.admitColdStart(ctx, accountID, modelID, path, replacing)
			if gateErr != nil {
				manager.rebalanceMu.Unlock()
				return nil, gateErr
			}
			opening := make(chan struct{})
			manager.openings[accountID] = opening
			manager.openingSet.Store(accountID, struct{}{})
			manager.rebalanceMu.Unlock()
			preparer, err := manager.startAuthenticatedWorker(ctx, accountID, bootstrapModel)
			releaseGate(err == nil)
			manager.rebalanceMu.Lock()
			if err == nil {
				if ctxErr := ctx.Err(); ctxErr != nil {
					err = errors.Join(ctxErr, discardAccountWorker(preparer))
				} else {
					err = activateAccountWorker(preparer)
				}
			}
			manager.finishOpening(accountID, opening)
			warmCount := len(manager.warmAccountIDsIn(partition))
			manager.rebalanceMu.Unlock()
			// 替补由 startReplacement 自己记录，预热由预热循环记录，不算按需扩容；其余无条件写，标明是否超出常驻
			// （原先只在超出常驻时写，诊断数不出低于常驻数的现场启动）
			if err == nil && !replacementStartup(ctx) && workerStartSourceFrom(ctx) != workerStartPrewarm {
				manager.requests.log("service", "INFO", fmt.Sprintf(
					"%s 按需扩容 | Worker=%d/%d | 超出常驻=%s",
					workerLogName(partition), warmCount, manager.maxActiveFor(partition), aboveResidentText(warmCount > manager.warmTargetFor(partition)),
				))
			}
			return preparer, err
		}
		// 不等待的选号（acquireWarmLeaseNow）只在分区有空槽时现场启动：替换空闲 Worker 要等旧 Worker 让出，按容量已满返回；
		// 替补（startReplacement）同样只用空槽，不替换其他账号的空闲 Worker
		if immediateStartup(ctx) || replacementStartup(ctx) {
			manager.rebalanceMu.Unlock()
			return nil, errAccountWorkerCapacity
		}
		// 淘汰选号与现场提升判断“能否替换”同一口径（replaceVictimFor，N3）：同分区有 Worker 正在启动时只换冷却中、死槽位、不可调度的，
		// 以及对该模型不可服务或空闲超过 victimIdleReplaceAfter 的健康空闲 Worker；刚就绪不到 rotationWarmupGrace 的不换（N6）。
		// 每个分区每分钟的按需替换不超过 replacementsPerMinute（N6），超过时排队等待（不等待的选号按容量已满返回）
		victim := ""
		if manager.replacementAllowed(ctx, partition) {
			victim = manager.replaceVictimFor(accountID, modelID, partition)
		}
		// 与冷却轮换共用淘汰标记：选中后原子标记，已被轮换标记的视为不可用，下一轮重新选择
		if victim != "" && !manager.tryReserveVictim(victim) {
			victim = ""
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
		// 冷启动闸门：出口限流期间替换空闲 Worker 一律暂停，只有补覆盖（驻留里没有能服务该模型的热 Worker）按配额放行；
		// 选中淘汰对象之后才判断，等待淘汰对象的复查不会重复计数
		path := coldStartReplace
		if workerStartSourceFrom(ctx) == workerStartCoverage {
			path = coldStartCoverage
		}
		releaseGate, gateErr := manager.admitColdStart(ctx, accountID, modelID, path, false)
		if gateErr != nil {
			manager.releaseVictim(victim)
			manager.rebalanceMu.Unlock()
			return nil, gateErr
		}
		// 闸门放行后才记一次按需替换（N6）：被闸门拒掉的尝试没有启动浏览器，不占每分钟的替换名额。原先选中淘汰对象就记，
		// 出口限流期间每个排队请求每次复查都用掉一次，名额很快用完，闸门本来会放行的补覆盖也拿不到可替换的 Worker
		manager.noteReplacement(ctx, partition)
		defer func() { manager.releaseVictim(victim) }()
		opening := make(chan struct{})
		manager.openings[accountID] = opening
		manager.openingSet.Store(accountID, struct{}{})
		manager.rebalanceMu.Unlock()
		pending, startErr := manager.startAuthenticatedWorker(ctx, accountID, bootstrapModel)
		releaseGate(startErr == nil)
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
				victim = manager.pickReplaceVictim(accountID, modelID, victimAny, partition)
				if victim != "" && !manager.tryReserveVictim(victim) {
					victim = ""
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
				warmCount := len(manager.warmAccountIDsIn(partition))
				manager.rebalanceMu.Unlock()
				if activationErr != nil {
					return nil, activationErr
				}
				if workerStartSourceFrom(ctx) != workerStartPrewarm {
					manager.requests.log("service", "INFO", fmt.Sprintf(
						"%s 按需替换 | Worker=%d/%d | 超出常驻=%s",
						workerLogName(partition), warmCount, manager.maxActiveFor(partition), aboveResidentText(warmCount > manager.warmTargetFor(partition)),
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

// admitColdStart 经冷启动闸门（见 egress_guard.go）判断这次冷启动：没有闸门、账户自己重开（replacing，或来源为重开：请求已取得租约、
// 发送时 Worker 不在了）时直接放行；暂停时返回 errEgressColdStartPaused（调用方按容量处理）。放行时返回的函数在
// startAuthenticatedWorker 返回后调用一次，退还并发名额
func (manager *accountWorkerManager) admitColdStart(
	ctx context.Context, accountID string, modelID string, path coldStartPath, replacing bool,
) (func(started bool), error) {
	noop := func(bool) {}
	gate := manager.coldStartGate.Load()
	if gate == nil || *gate == nil || replacing || workerStartSourceFrom(ctx) == workerStartReopen {
		return noop, nil
	}
	release, ok := (*gate)(accountID, modelID, path)
	if !ok {
		return noop, errEgressColdStartPaused
	}
	if release == nil {
		release = noop
	}
	return release, nil
}

// aboveResidentText 为“按需扩容”“按需替换”日志里的“超出常驻=是|否”
func aboveResidentText(above bool) string {
	if above {
		return "是"
	}
	return "否"
}

// startAuthenticatedWorker 启动账户 Worker。启动阶段确认登录失效（登录页跳转、签名 Cookie 失效）时，
// 在请求租约内恢复一次认证并重新启动；仍失败时账户标为需要登录，错误带 authHandled，上层不再恢复第二次。
// 预热与后台扩容没有租约，也不为此另取普通租约（会更新 LastUsed、把账户显示为忙碌，干扰 idleWarmVictimFor
// 与预热轮换选号）：账户保存了续签材料时留给带租约的按需启动恢复，没有续签材料时按启动前的认证代际标为需要登录
func (manager *accountWorkerManager) startAuthenticatedWorker(ctx context.Context, accountID string, model string) (*accountWorkerPreparer, error) {
	generation := manager.pool.AuthGeneration(accountID)
	startedAt := time.Now()
	worker, err := manager.startReservedWorker(ctx, accountID, model)
	if err == nil {
		manager.persistStartupCookies(accountID, generation, worker)
	}
	if err == nil || ctx.Err() != nil || !aistudio.DefinitiveAuthenticationFailure(err) {
		return worker, err
	}
	lease, leased := aistudio.AccountLeaseFromContext(ctx)
	if !leased || lease.Account().ID != accountID {
		marked, markErr := manager.markStartupAuthRequired(accountID, generation, startedAt, err)
		if !marked && markErr == nil {
			return nil, err
		}
		return nil, &accountWorkerInitError{err: errors.Join(err, markErr), authHandled: true}
	}
	if manager.refresher == nil {
		marked, markErr := lease.MarkAuthenticationFailure(err)
		if marked && markErr == nil {
			manager.confirmAuthRequired(accountID, lease.Account().Config.Label, authFromStartup, err)
		}
		return nil, &accountWorkerInitError{err: errors.Join(err, markErr), authHandled: true}
	}
	// 续签器确认的登录失效按“启动”来源写“账号需要登录”（没有标记时记成续签重放）
	recoverCtx := withAuthRequiredSource(ctx, authFromStartup)
	if recoverErr := manager.refresher.Recover(recoverCtx, err); recoverErr != nil {
		return nil, &accountWorkerInitError{err: recoverErr, authHandled: true}
	}
	renewed := manager.pool.AuthGeneration(accountID)
	worker, err = manager.startReservedWorker(ctx, accountID, model)
	if err == nil {
		manager.persistStartupCookies(accountID, renewed, worker)
		return worker, nil
	}
	if aistudio.DefinitiveAuthenticationFailure(err) {
		err = manager.refresher.markAuthenticationRequired(recoverCtx, err)
	}
	return nil, &accountWorkerInitError{err: err, authHandled: true}
}

// startupCookieTimeout 为 Worker 启动完成后导出浏览器 Cookie 的上限
const startupCookieTimeout = 5 * time.Second

// persistStartupCookies 在 Worker 启动完成后立即把浏览器 Cookie 合并写回账户的认证文件（N1）：浏览器登录 AI Studio 时会轮换
// __Secure-1PSIDTS、SIDCC 等 Cookie，原先只在一次生成之后才写回，Worker 启动后还没生成过时，Go 通道（GenerateAccessToken、
// GetAiStudioBenefitTier、目录同步、登录复核）一直用启动前的旧 Cookie，401 多半来自这里。generation 为启动前读取的认证代际，
// 期间保存过新登录或续签时不写；同步执行（有上限），不在测试的临时目录清理之后还写文件。失败只写一条警告，不影响启动
func (manager *accountWorkerManager) persistStartupCookies(accountID string, generation uint64, preparer *accountWorkerPreparer) {
	if manager == nil || manager.pool == nil || preparer == nil || preparer.worker == nil {
		return
	}
	base := manager.lifecycle
	if base == nil {
		base = context.Background()
	}
	ctx, cancel := context.WithTimeout(base, startupCookieTimeout)
	defer cancel()
	state, err := preparer.worker.BrowserStorageState(ctx)
	if err != nil || len(state.Cookies) == 0 {
		return
	}
	if _, err := manager.pool.MergeBrowserCookies(accountID, generation, state.Cookies); err != nil && manager.requests != nil {
		manager.requests.log(accountID, "WARN", "WAA Worker 启动后写回 Cookie 失败 | 错误="+err.Error())
	}
}

// markStartupAuthRequired 处理没有请求租约的启动路径上的登录失效，返回是否把账户标为需要登录
func (manager *accountWorkerManager) markStartupAuthRequired(
	accountID string,
	generation uint64,
	startedAt time.Time,
	cause error,
) (bool, error) {
	account, err := manager.pool.Account(accountID)
	if err != nil {
		return false, err
	}
	// 有续签材料、且本平台能续签时留给带租约的按需启动恢复；非 Windows 上续签必然失败，按没有材料处理
	if state, loadErr := aistudio.LoadStorageState(account.StoragePath); loadErr == nil && authRefreshMaterial(state) &&
		manager.refresher.renewPlatform() {
		return false, nil
	}
	// 启动阶段的登录失效多为登录页跳转或签名 Cookie 失效（本地证据），直接标记；上游 401 的复核只在请求路径（见 auth_recheck.go）
	marked, err := manager.pool.MarkAuthenticationRequiredIfGeneration(accountID, generation, startedAt, cause.Error())
	if marked && manager.refresher != nil && manager.refresher.publish != nil {
		manager.refresher.publish()
	}
	if marked {
		// 启动失败的账户没有驻留 Worker，释放登记不做任何事；只写“账号需要登录 | 来源=启动”
		manager.confirmAuthRequired(accountID, account.Config.Label, authFromStartup, cause)
	}
	return marked, err
}

// promote 为请求现场提升备用账户（来源默认为按需；补覆盖由调用方标记）
func (manager *accountWorkerManager) promote(ctx context.Context, accountID string, modelID string) (aistudio.ProtectedPreparer, error) {
	if workerStartSourceFrom(ctx) == "" {
		ctx = withWorkerStartSource(ctx, workerStartOnDemand)
	}
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
	// inflight 为本轮正在启动的账户与其所在的 Worker 分区
	inflight := make(map[string]aistudio.PoolScope)
	results := make(chan warmResult, warmResultBuffer)
	launched := 0
	// capacityFull 为 Worker 槽位已满的分区：该分区本轮不再启动新的
	capacityFull := make(map[aistudio.PoolScope]bool, len(workerPartitions))
	// egressPaused 为冷启动被出口闸门暂停（补位并发已满）的分区：同样不再启动新的，直到该分区有一个启动结束（名额退还）
	egressPaused := make(map[aistudio.PoolScope]bool, len(workerPartitions))
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
		partition := inflight[result.accountID]
		delete(inflight, result.accountID)
		manager.fillInflight.Store(int32(len(inflight)))
		// 一个启动结束（成功或失败）就退还了补位名额：被出口闸门暂停的分区可以再试
		pausedHere := errors.Is(result.err, errEgressColdStartPaused)
		if !pausedHere {
			delete(egressPaused, partition)
		}
		if result.err == nil {
			manager.clearWarmFailure(result.accountID)
			notify(nil)
			return
		}
		if ctx.Err() != nil {
			return
		}
		// 出口限流中、补位名额已满：不算失败，不进预热失败黑名单，原因写明是出口限流而不是槽位已满（先于容量判断：它也满足 errAccountWorkerCapacity）
		if pausedHere {
			egressPaused[partition] = true
			return
		}
		// 槽位已满或该账户已在启动：不算失败，该分区本轮不再启动新的，交给 keepWarm 稍后重试
		if errors.Is(result.err, errAccountWorkerCapacity) {
			capacityFull[partition] = true
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
		// 每个分区从自己的账户补齐到自己的常驻数（见 worker_partition.go）
		ultra := manager.pool.UltraAccountIDs()
		summary := manager.bootstrapSummary()
		targets := make(map[aistudio.PoolScope]int, len(workerPartitions))
		warmCounts := make(map[aistudio.PoolScope]int, len(workerPartitions))
		inflightCounts := make(map[aistudio.PoolScope]int, len(workerPartitions))
		for _, partition := range inflight {
			inflightCounts[partition]++
		}
		filled := true
		for _, partition := range workerPartitions {
			targets[partition] = manager.fillTargetFor(partition, summary)
			warmCounts[partition] = len(inPartition(warm, ultra, partition))
			filled = filled && warmCounts[partition] >= targets[partition]
		}
		if filled && len(inflight) == 0 {
			if launched > 0 {
				manager.requests.log("service", "INFO", fmt.Sprintf(
					"WAA Worker 预热完成 | Worker=%d/%d | 耗时=%s",
					len(warm), manager.PrewarmTarget(), time.Since(startedAt).Round(time.Millisecond),
				))
			}
			notify(nil)
			return
		}
		remaining := manager.warmConcurrencyValue() - len(inflight)
		pendingBusy := false
		readyCount := 0
		classified := false
		var roundClassifyErr error
		var runtimeBusyErr error
		for _, partition := range workerPartitions {
			slots := min(remaining, targets[partition]-warmCounts[partition]-inflightCounts[partition])
			if capacityFull[partition] || egressPaused[partition] || slots <= 0 {
				continue
			}
			classified = true
			groups, err := manager.classifyBootstrapCandidates(ctx, warm, partition)
			if err != nil {
				if roundClassifyErr == nil {
					roundClassifyErr = err
				}
				continue
			}
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
			runtimeBusyErr = errors.Join(runtimeBusyErr, readyBusyErr, standbyBusyErr)
			pendingBusy = pendingBusy || len(busy) > 0
			ready = manager.preferUncooled(partition, ready)
			// 坏号（连续裸限流）移出、近期成功过的账号排前（见 bad_accounts.go）
			ready = manager.orderColdCandidates(ready)
			// 疑似登录失效的账户（目录缓存记过需要登录、认证文件没变，见 catalog_sync.go）排在最后，预热先用没有嫌疑的账户
			ready = manager.deferAuthSuspects(ready)
			readyCount += len(ready)
			started := ready[:min(slots, len(ready))]
			for _, accountID := range started {
				inflight[accountID] = partition
				launched++
				go func(accountID string) {
					// 不等待 Worker 槽位：槽位满时立即返回；冷启动名额按后台启动排队，让给按需启动（见 startupSlots）。
					// 单个任务限时，避免个别账户拖住整轮预热
					taskCtx, taskCancel := context.WithTimeout(
						withWorkerStartSource(withBackgroundStartup(ctx), workerStartPrewarm), time.Duration(manager.initTimeout.Load())+warmTaskGrace,
					)
					_, err := manager.ensureWorker(taskCtx, accountID, "", false)
					taskCancel()
					results <- warmResult{accountID: accountID, err: err}
				}(accountID)
			}
			remaining -= len(started)
			manager.fillLaunched.Store(int32(launched))
			manager.fillInflight.Store(int32(len(inflight)))
			if len(started) > 0 {
				busySince = time.Time{}
			}
		}
		if classified {
			classifyErr = roundClassifyErr
		}
		if len(inflight) == 0 {
			if pendingBusy {
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
				full, anyFull := fullPartition(capacityFull)
				_, anyPaused := fullPartition(egressPaused)
				switch {
				case anyPaused && !anyFull:
					reason = egressColdStartPausedReason
				case anyFull:
					occupancy := manager.occupiedWorkersIn(full, manager.pool.UltraAccountIDs())
					reason = fmt.Sprintf("%s 槽位已满（占用 %d / 峰值上限 %d）", workerSlotName(full), occupancy.effectiveSlots(), manager.maxActiveFor(full))
					level = "WARN"
				case classifyErr != nil:
					reason = "候选账户分类失败: " + strings.TrimSpace(classifyErr.Error())
					level = "WARN"
				case pendingBusy:
					reason = "候选账户均在忙碌"
				case readyCount > 0:
					reason = "等待本轮启动结果"
				}
				// 按分区判断是否预热不足，日志写出该分区的计数（没有 Ultra 账户时与总数相同）
				if short, needed := manager.shortPartition(); needed {
					manager.noteWarmIdle(level, reason, fmt.Sprintf(
						"%s 预热等待 | Worker=%d/%d | 原因=%s",
						workerLogName(short), len(manager.warmAccountIDsIn(short)), manager.prewarmTargetFor(short), reason,
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
	// 后台关闭失败 Worker 的 context 随 lifecycle 取消：不再登记新的，等进行中的退出后再关闭全部 Worker，
	// Close 返回后不再有 goroutine 访问账户目录
	manager.closeWorkerResets()
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
	manager.clearAllDrains()
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
	return errors.Join(append(closeErrors, manager.closeCleanupLeftovers(account))...)
}

// closeCleanupLeftovers 只关闭之前清理失败留下的替换 Worker 与租约，不动当前主 Worker（调用方持有 account.mu）
func (manager *accountWorkerManager) closeCleanupLeftovers(account *accountWorker) error {
	var closeErrors []error
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
		// Worker 已关闭：排空随之结束（淘汰、清扫、回收、ResetAll、Close 等任何关闭路径），账号以后再启动 Worker 时照常接受请求
		manager.clearDrain(account.id)
		manager.notifyScheduling()
	}
	return errors.Join(closeErrors...)
}

type accountHeaderProvider struct {
	mu          sync.RWMutex
	accounts    map[string]*accountHeaderState
	globalProxy string
	// closed 在 Close 后为 true：之后的 Add 与 Commit 不再登记（关闭期间目录监视仍可能加入新账户）
	closed bool
}

type accountHeaderState struct {
	mu      sync.Mutex
	headers http.Header
	// account 为账户对象，proxy 为建 client 时的出口：IP 池的绑定会随浏览器启停变化，需要发现公共头时按账户当前的
	// EffectiveProxy 取出口，与 proxy 不同就换新 client（见 clientFor）。account 为 nil 时一直用 client。
	// client、proxy、closed 由 clientMu 保护（mu 在发现公共头的网络请求期间一直持有，关闭出口不等它）
	account  *aistudio.Account
	clientMu sync.Mutex
	client   *http.Client
	proxy    string
	closed   bool
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
	proxy := account.EffectiveProxy(provider.globalProxy)
	client, err := aistudio.NewProxyHTTPClient(proxy)
	if err != nil {
		return fmt.Errorf("创建账户 %s 的固定出口: %w", account.ID, err)
	}
	provider.mu.Lock()
	defer provider.mu.Unlock()
	if provider.closed {
		client.CloseIdleConnections()
		return fmt.Errorf("账户固定出口已关闭")
	}
	if _, exists := provider.accounts[account.ID]; exists {
		client.CloseIdleConnections()
		return fmt.Errorf("账户固定出口已存在: %s", account.ID)
	}
	provider.accounts[account.ID] = &accountHeaderState{client: client, account: account, proxy: proxy}
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
		provider: provider, accountID: account.ID, pending: true,
		state: &accountHeaderState{client: client, account: account, proxy: proxy},
	}, nil
}

// Commit 发布已准备的账户固定出口
func (update *accountHeaderUpdate) Commit() {
	if update == nil || !update.pending {
		return
	}
	update.provider.mu.Lock()
	current := update.provider.accounts[update.accountID]
	// 准备之后账户已被删除或出口已关闭：不再发布，关闭新建的出口
	stale := update.provider.closed || current == nil
	if !stale {
		update.provider.accounts[update.accountID] = update.state
	}
	update.provider.mu.Unlock()
	update.pending = false
	if stale {
		update.state.closeIdle()
		return
	}
	current.closeIdle()
}

// Discard 关闭未发布的账户固定出口
func (update *accountHeaderUpdate) Discard() {
	if update == nil || !update.pending {
		return
	}
	update.pending = false
	update.state.closeIdle()
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
	account.closeIdle()
	return nil
}

// Close 关闭全部账户固定出口
func (provider *accountHeaderProvider) Close() {
	provider.mu.Lock()
	accounts := provider.accounts
	provider.accounts = map[string]*accountHeaderState{}
	provider.closed = true
	provider.mu.Unlock()
	for _, account := range accounts {
		account.closeIdle()
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
		client, err := account.clientFor(provider.globalProxy)
		if err != nil {
			return nil, err
		}
		headers, err := aistudio.DiscoverPublicHeaders(ctx, client)
		if err != nil {
			return nil, err
		}
		account.headers = headers.Clone()
	}
	return account.headers.Clone(), nil
}

// clientFor 返回发现公共头用的 client：按账户当前的出口（账户代理 > IP 池绑定 > 默认代理），与建 client 时不同才新建，
// 并关闭旧 client 的空闲连接（进行中的请求不受影响）；出口已关闭时沿用原 client
func (state *accountHeaderState) clientFor(globalProxy string) (*http.Client, error) {
	state.clientMu.Lock()
	defer state.clientMu.Unlock()
	if state.account == nil || state.closed {
		return state.client, nil
	}
	proxy := state.account.EffectiveProxy(globalProxy)
	if proxy == state.proxy {
		return state.client, nil
	}
	client, err := aistudio.NewProxyHTTPClient(proxy)
	if err != nil {
		return nil, fmt.Errorf("创建账户 %s 的固定出口: %w", state.account.ID, err)
	}
	state.client.CloseIdleConnections()
	state.client, state.proxy = client, proxy
	return client, nil
}

// closeIdle 关闭出口的空闲连接，之后不再换 client
func (state *accountHeaderState) closeIdle() {
	state.clientMu.Lock()
	state.closed = true
	client := state.client
	state.clientMu.Unlock()
	client.CloseIdleConnections()
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
	// rateLimits 判断号池级限流（号池内多个账号对同一模型连续裸限流、又没有成功时不升级冷却，见 rate_limit_guard.go）；为空时不判断
	rateLimits *rateLimitGuard
	// rateLimitLadder 为连续裸限流各档的冷却时长，为空时取默认四档（见 rateLimitSteps）；只给包内测试改小
	rateLimitLadder []time.Duration
	// permissionDenied 为 403 无权限账户的登记表（见 permission_denied.go），保存在账户目录，跨重启保留；为空时不登记
	permissionDenied *permissionDeniedRegistry
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
	// firstEventTimeout 为每次尝试等待首个上游事件的上限（FIRST_EVENT_TIMEOUT，0 为关闭，可热更新）
	firstEventTimeout atomic.Int64
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
	modelRefreshDone   <-chan struct{}
	modelChangeMu      sync.Mutex
	modelRevision      uint64
	modelApplied       uint64

	// streamPlaygroundModels 为流式请求优先走 Playground 的模型（STREAM_PLAYGROUND_MODELS，可热更新，见 stream_channel.go）
	streamPlaygroundModels atomic.Pointer[map[string]struct{}]
	// primaryModels 为两份主用模型列表与排队上限（普通号池 PRIMARY_MODELS / PRIMARY_QUEUE_WAIT 与 Ultra 号池
	// ULTRA_PRIMARY_MODELS / ULTRA_PRIMARY_QUEUE_WAIT，可热更新，见 primary_models.go）
	primaryModels atomic.Pointer[primaryModelSettings]
	// queueProgress 为重复的排队进度日志全进程共用的限流（见 queue_progress.go）
	queueProgress queueProgressLimiter
	// unparsedQuotaDetails 为“额度 429 详情未识别”日志的去重状态（见 quota_detail_log.go）
	unparsedQuotaDetails unparsedQuotaDetailLog
	// resetWait 为不固定账号的请求等待同账号关闭失败 Worker 的上限，nil 时为 defaultWorkerResetWait（见 worker_reset.go，测试调整）
	resetWait atomic.Pointer[time.Duration]
	// maxDuration 为生成请求的总时长上限（见 generate_context.go），0 为默认（generateMaxDurationFor），只给包内测试调低
	maxDuration atomic.Int64
	// egress 为按出口的限流判定与限速（见 egress_guard.go）；只在 newRuntime 创建，测试构造的服务为 nil（判定关闭）
	egress *egressGuard
	// catalogSync 为模型目录的后台调度器（见 catalog_sync.go）；newTrackedService 创建（不带磁盘缓存），newRuntime 挂上缓存；
	// 测试直接构造的服务为 nil，syncAccountModels 退回直接同步
	catalogSync *catalogSyncer
	// quotaJitter 为普通号池第 2 次限流类换号前的等待，nil 时为默认 300–800ms 随机（见 quota_switch.go，只给包内测试改）
	quotaJitter func() time.Duration
	// egressQueueWait 为非主用模型按“出口限速”累计排队的上限，0 为默认 egressQueueWaitMax（只给包内测试改小）
	egressQueueWait time.Duration
	// authNotes 为“账号需要登录”日志的去重状态（同账号 1 分钟最多一条，见 worker_unschedulable.go），零值可用
	authNotes authRequiredNotes
	// authMarks 为“账号需要登录”的最近计数（状态接口的 auth_required_10m），零值可用
	authMarks recentEvents
	// authRecheck 为登录复核器（疑似登录失效的复核与需要登录账户的定期复核，见 auth_recheck.go）；只在 newRuntime 创建并挂上账户池的 gate，
	// 测试构造的服务为 nil（401 照常直接标记）
	authRecheck *authRecheck
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
		forbidden: newForbiddenTracker(), rateLimits: newRateLimitGuard(),
	}
	tracked.catalogSync = newCatalogSyncer(tracked)
	// 冷却淘汰成功后立即补齐预热，不等 keepWarm 的下一个周期；是否在运行由 prewarmIfRunning 判断。
	// 替补只在生成服务运行时启动，排空超时按请求超时与总时长上限计算（见 worker_drain.go）
	if workers != nil {
		refill := tracked.prewarmIfRunning
		workers.afterRetire.Store(&refill)
		running := tracked.Running
		workers.serviceActive.Store(&running)
		// 认证续签器与 Worker 启动阶段确认需要登录时同样写“账号需要登录”（见 worker_unschedulable.go）
		note := authRequiredHook(tracked.noteAuthRequired)
		workers.authRequired.Store(&note)
	}
	tracked.setRequestTimeout(timeout)
	return tracked
}

// setRequestTimeout 更新请求超时（REQUEST_TIMEOUT），同步给 Worker 管理器计算排空超时（总时长上限 + 30 秒）。
// 进行中的生成请求的三段期限在建立时已取值，只影响新请求
func (service *trackedService) setRequestTimeout(timeout time.Duration) {
	service.timeout.Store(int64(timeout))
	if service.workers != nil {
		service.workers.requestTimeout.Store(int64(timeout))
	}
}

type serviceStoppedError struct{}

var errServiceTransitioning = errors.New("生成服务正在切换状态")

const (
	serviceStopped int32 = iota
	serviceLaunching
	serviceRunning
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
	// 目录已在 newRuntime 从写盘缓存恢复：启动时只排缺失、不一致、疑似失效与过期的账户，由后台低速刷新，不再全量同步
	service.requests.log("service", "INFO", service.catalogSync.plan().startLine(len(models)))
	catalogReady, catalogSynced, catalogDone := service.catalogSync.start(dataContext)
	// 登录复核与目录调度器同时开始：启动期间目录同步遇到的上游 401 同样先记疑似登录失效、复核后才标记（见 auth_recheck.go）
	go service.authRecheck.run(dataContext)
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
	// 出口判定器用自己的计时器，不放进 keepWarmPass：清扫死槽位会同步等浏览器关闭，会拖慢限速调整与安全阀计时
	go service.egress.run(dataContext)
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
	// 写入由账户池登记为后台写入，停止或测试清理账户目录前可用 pool.WaitBackground 等待结束；
	// 同时清零该通道的连续限流档位（见 rate_limit_guard.go），此前到第二档以上时写一行，排查时能看到档位从哪里回到第一档
	service.pool.MarkGenerationVerifiedAsync(accountID, modelID, accessGeneration, checkedAt, func(changed bool, clearedLevel int, err error) {
		if err != nil {
			service.requests.log(accountLabel, "ERROR", fmt.Sprintf(
				"模型资格保存失败 | 模型=%s | 错误=%s", modelID, strings.TrimSpace(err.Error()),
			))
			return
		}
		if clearedLevel >= 2 {
			service.requests.log(accountLabel, "INFO", fmt.Sprintf(
				"限流档位清零 | 范围=%s | 此前连续=第 %d 档", modelID, clearedLevel,
			))
		}
		if changed {
			service.publishModelAccess()
		}
	})
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
	// 坏号记忆平时最多每 30 秒写一次：停止服务时立即写出。重建运行时（进程内重启、重建服务的配置保存）先停旧服务、再建新一代
	// （新一代此时读文件）、最后才关闭旧一代，原先旧一代在关闭时才写，新一代读到的是旧数据，随后又被新一代的第一次写盘整份覆盖
	if err := service.workers.badAccountMemory().save(); err != nil && service.requests != nil {
		service.requests.log("service", "WARN", "坏号记忆写入失败 | 错误="+err.Error())
	}
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

// Models 返回请求号池内启用账户具有模型资格的公开目录：/ultra 只列 Ultra 号池账户可用的模型
func (service *trackedService) Models(ctx context.Context) ([]aistudio.Model, error) {
	return service.pool.EligibleModelsIn(aistudio.PoolScopeFromContext(ctx), service.modelSnapshot()), nil
}

// catalogModels 返回管理页使用的完整目录与各模型可用通道；BuildUnary 标出在 Build 通道只能一次性返回整段回复的模型
// （需要订阅权益），服务配置页据此提示哪些模型适合加入 STREAM_PLAYGROUND_MODELS
func (service *trackedService) catalogModels() []aistudio.Model {
	models := service.pool.CatalogModels(service.modelSnapshot())
	for index := range models {
		models[index].BuildUnary = aistudio.BuildUsesUnary(models[index])
	}
	return models
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
	service.workers.ipPool().markPinned(accounts)
	service.requests.publish(api.AdminEvent{Type: "accounts", Data: map[string]any{"accounts": accounts}})
	service.requests.publish(api.AdminEvent{Type: "models", Data: map[string]any{"models": service.catalogModels()}})
}

// refreshAccountModelCatalog 立即同步单个账户的目录（管理操作、导入与批量启用：不受后台同步的暂停与速率限制）
func (service *trackedService) refreshAccountModelCatalog(ctx context.Context, accountID string) ([]aistudio.Model, error) {
	models, _, err := service.refreshAccountModelCatalogWith(ctx, accountID, false)
	return models, err
}

// refreshAccountModelCatalogWith 为后台调度器与管理操作共用的单账户同步：background 为真时只在账户空闲时进行
// （TryRefreshAccountModels，账户忙时 busy 为真、不发请求），否则照旧取独占租约（RefreshAccountModels）。
// 结果交给调度器写缓存、排下次刷新或退避；没有调度器（测试直接构造的服务）时只写失败日志
func (service *trackedService) refreshAccountModelCatalogWith(
	ctx context.Context,
	accountID string,
	background bool,
) ([]aistudio.Model, bool, error) {
	requestCtx, cancel := service.lifecycleRequestContext(ctx)
	defer cancel()
	var models []aistudio.Model
	var busy bool
	var err error
	if refresher, ok := service.catalog.(catalogTryRefresher); background && ok {
		models, busy, err = refresher.TryRefreshAccountModels(requestCtx, accountID)
	} else {
		models, err = service.catalog.RefreshAccountModels(requestCtx, accountID)
	}
	if service.catalogSync == nil {
		if err != nil && ctx.Err() == nil {
			service.requests.log(accountID, "WARN", "模型目录同步失败 | 错误="+err.Error())
		}
		return models, busy, err
	}
	service.catalogSync.afterRefresh(ctx, accountID, models, busy, err, background)
	return models, busy, err
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

// syncAccountModels 为管理操作立即同步单个账户的目录并合并应用（经调度器的 syncNow，结果写缓存、排下次刷新）；
// 没有调度器（测试直接构造的服务）时直接同步
func (service *trackedService) syncAccountModels(ctx context.Context, accountID string) ([]aistudio.Model, error) {
	var models []aistudio.Model
	var err error
	if service.catalogSync != nil {
		models, err = service.catalogSync.syncNow(ctx, accountID)
	} else {
		models, err = service.refreshAccountModelCatalog(ctx, accountID)
	}
	service.modelSyncMu.Lock()
	service.applyCachedModelCatalog()
	service.modelSyncMu.Unlock()
	return models, err
}

// removeAccountModelRetry 移除已删除或移出号池的账户的后台同步任务与疑似失效记录
func (service *trackedService) removeAccountModelRetry(accountID string) {
	service.catalogSync.forget(accountID)
	service.authRecheck.forget(accountID)
	service.workers.badAccountMemory().forget(accountID)
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

// markModelsChanged 登记一次已经写入账户池的模型目录变化：批量启用逐个写入账户配置时不占用变化锁（写配置要同步到磁盘），
// 全部写完后统一登记一次，之后的 SyncModels 会重新合并
func (service *trackedService) markModelsChanged() {
	service.modelChangeMu.Lock()
	service.modelRevision++
	service.modelChangeMu.Unlock()
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
	// 不固定账户的数据请求（CountTokens 等）最多换 dataAttemptCeiling-1 个账号（见 quota_switch.go），固定账户的照旧只用这一个
	requestCtx = aistudio.WithAttemptCeiling(requestCtx, dataAttemptCeiling)
	observed := aistudio.ContextWithAccountSelectionObserver(requestCtx, func(account *aistudio.Account) {
		api.SetAccessLogTarget(requestCtx, model, account.Config.Label)
	})
	return observed, cancel, nil
}

// CountTokens 返回上游权威输入 token 数
func (service *trackedService) CountTokens(ctx context.Context, request aistudio.TokenCountRequest) (aistudio.TokenCount, error) {
	request.Model = service.pool.CanonicalModelIDIn(aistudio.PoolScopeFromContext(ctx), request.Model)
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
	request.Model = service.pool.CanonicalModelIDIn(aistudio.PoolScopeFromContext(ctx), request.Model)
	requestCtx, cancel, err := service.dataRequestContext(ctx)
	if err != nil {
		api.SetAccessLogError(ctx, err)
		return aistudio.VideoOperation{}, err
	}
	defer cancel()
	// 不固定账户的 Veo 请求最多换 dataAttemptCeiling-1 个账号（见 quota_switch.go）
	requestCtx = aistudio.WithAttemptCeiling(requestCtx, dataAttemptCeiling)
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
		// 一直等关闭完成后在原账号重放（受 REQUEST_TIMEOUT 限制），不按 dataResetSwitchWait 只等一小会儿：aistudio 层的视频循环
		// 只对 401/403/429/5xx 换号，协议 Code 5（404）与 Worker 浏览器失败都不换号，等不到返回未恢复时请求直接失败（Code 5 对外 502）
		recovered, _, _, recoveryErr := service.recoverWorkerOnce(
			recoveryCtx, nil, accountID, expectedGeneration, recoveredWorkers,
			recoverCurrentGeneration, workerFailed || waaRuntimeFailed, workerResetWaitUnbounded,
		)
		return recovered, recoveryErr
	}
	video, ok := service.service.(aistudio.VideoService)
	if !ok {
		return aistudio.VideoOperation{}, fmt.Errorf("video service 不可用")
	}
	// 出口限流期间不固定账户（没有指定账户、首帧不是已上传文件）的 Veo 只在热 Worker 与未受限出口的备用账户里选号（见 videoCandidates）
	if strings.TrimSpace(request.AccountID) == "" && (request.StartImage == nil || request.StartImage.File == nil) {
		request.AllowedAccountIDs = service.videoCandidates(requestCtx, request.Model)
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

// recoverWorkerOnce 对指定账户的失败 Worker 版本执行至多一次恢复。
// lease 为调用方仍持有的失败租约（已释放时为 nil），返回前一定已经释放：要关闭这个 Worker 版本时原子地转成独占等待交给后台关闭
// （见 scheduleWorkerReset），其余情况照常释放，释放的错误作为 err 返回。关闭在后台进行，同一版本只关闭一次，请求取消后照常继续；
// wait 为本次最多等多久（0 为不等待，workerResetWaitUnbounded 为一直等），有上限表示调用方可以换号。
// 等不到、后台放弃，或关闭出错而调用方可以换号时返回 deferred：本次不在原账号重放（账号同样记为已恢复），
// 调用方换号找不到可用账号时可以等它结束后回到原账号重放
func (service *trackedService) recoverWorkerOnce(
	ctx context.Context,
	lease *aistudio.AccountLease,
	accountID string,
	expectedGeneration uint64,
	recoveredWorkers map[string]struct{},
	currentGenerationEligible bool,
	resetCurrentGeneration bool,
	wait time.Duration,
) (recovered bool, currentGeneration bool, deferred *workerReset, err error) {
	if _, recovered := recoveredWorkers[accountID]; recovered {
		return false, false, nil, lease.Release()
	}
	if service.workers.WorkerGeneration(accountID) != expectedGeneration {
		recoveredWorkers[accountID] = struct{}{}
		return true, false, nil, lease.Release()
	}
	if !currentGenerationEligible {
		return false, false, nil, lease.Release()
	}
	if resetCurrentGeneration {
		pending, releaseErr := service.workers.scheduleWorkerReset(accountID, expectedGeneration, lease, workerResetRequest)
		if releaseErr != nil {
			return false, false, nil, releaseErr
		}
		finished, waitErr := pending.wait(ctx, wait)
		if waitErr != nil {
			return false, false, nil, waitErr
		}
		switch {
		case !finished || pending.expired:
			recoveredWorkers[accountID] = struct{}{}
			return false, false, pending, nil
		case pending.err != nil && wait >= 0:
			// 调用方还能换号：关闭出错（多半是浏览器关不掉）时同样换号，不按这个错误结束请求；错误已由后台写入日志
			recoveredWorkers[accountID] = struct{}{}
			return false, false, pending, nil
		case pending.err != nil:
			return false, false, nil, pending.err
		}
	}
	recoveredWorkers[accountID] = struct{}{}
	return true, true, nil, nil
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

// acquireWarmLease 按选择条件取得账号租约：没有立即可用的账号时排进调度等待队列，等账号空闲、Worker 启动或冷却结束
func (service *trackedService) acquireWarmLease(ctx context.Context, selection aistudio.AccountSelection) (*aistudio.AccountLease, error) {
	return service.acquireLease(ctx, selection, false, nil)
}

// acquireWarmLeaseNow 与 acquireWarmLease 相同，但不等待：只接受现在就能取得的租约，包括空闲的热 Worker 账号，以及所在分区有空槽、
// 冷启动名额空闲时现场冷启动的备用账号；不排进调度等待队列，不等账号空闲、Worker 启动或替换、冷却结束。
// 没有立即可用的账号时返回 *noImmediateLeaseError；没有账号支持、全部冷却超过排队上限、启动失败等错误与 acquireWarmLease 相同。
// failures 不为 nil 时记下现场启动失败的账号，调用方随后改用其他条件选号时交给 acquireLease，不再启动这些账号
func (service *trackedService) acquireWarmLeaseNow(
	ctx context.Context, selection aistudio.AccountSelection, failures *workerStartupFailures,
) (*aistudio.AccountLease, error) {
	return service.acquireLease(ctx, selection, true, failures)
}

// workerStartupFailures 记录选号中现场启动 Worker 失败的账号与最后一次失败原因：这些账号不再作为候选，候选用尽时按启动失败原因返回。
// 默认每次选号各用一份；流式优先 Playground 的不等待选号与随后不限通道的选号共用一份，同一次尝试里不会把启动失败的账号再启动一次
type workerStartupFailures struct {
	accounts map[string]struct{}
	last     error
}

func newWorkerStartupFailures() *workerStartupFailures {
	return &workerStartupFailures{accounts: make(map[string]struct{})}
}

// noImmediateLeaseError 表示不等待的选号（acquireWarmLeaseNow）没有立即可用的账号；reason 说明原因（账号忙碌、Worker 容量已满等）
type noImmediateLeaseError struct {
	reason string
}

func (err *noImmediateLeaseError) Error() string {
	return "没有立即可用的账号：" + err.reason
}

// acquireLease 为 acquireWarmLease 与 acquireWarmLeaseNow 的实现；immediate 为真时每个需要等待的地方都改为返回 *noImmediateLeaseError，
// 从不加入调度等待队列，取得的租约在启动 Worker 失败时照常释放。failures 为 nil 时只记在本次选号内
func (service *trackedService) acquireLease(
	ctx context.Context, selection aistudio.AccountSelection, immediate bool, failures *workerStartupFailures,
) (*aistudio.AccountLease, error) {
	// 号池写进选择条件：分类、选号、等待队列与没有账户时的错误都只看请求的号池
	selection = aistudio.ScopedSelection(ctx, selection)
	fixedAccount := strings.TrimSpace(selection.AccountID) != "" || strings.TrimSpace(selection.ResourceID) != ""
	if failures == nil {
		failures = newWorkerStartupFailures()
	}
	failedWorkers := failures.accounts
	// 主用模型的选号带排队预算（见 primary_models.go）：容量原因的等待累计超过上限时 wait 返回 CapacityBusyError，没有预算时不限时
	budget := leaseWaitBudgetFromContext(ctx)
	// 主用模型的生成选号在出口令牌桶受限时优先取得令牌（只影响出口准入，见 aistudio/egress_rate.go）
	selection.EgressPriority = budget != nil
	// 出口限流（见 egress_guard.go）：egressKey 为本轮复查时受限的出口（等待原因是出口限速或驻留额度用完时归属它），egressBucketWait 为
	// 候选里有因出口令牌桶已满按忙碌分组的；egressLeave 为进入“出口限速”排队的离开函数（原因变化或选号结束时调用）。
	// 非主用模型按出口排队的时间整个请求累计（egressWaitBudgetFrom，没有时只按本次选号累计），超过上限按 429 返回
	ultraLane := selection.Pool == aistudio.PoolScopeUltra
	egressKey, egressBucketWait := "", false
	var egressLeave func()
	leaveEgressWait := func() {
		if egressLeave != nil {
			egressLeave()
			egressLeave = nil
		}
	}
	defer func() { leaveEgressWait() }()
	egressBudget := egressWaitBudgetFrom(ctx)
	var egressWaitedHere time.Duration
	// coveragePausedAt 为本次选号的补覆盖被冷启动闸门暂停的时刻：之后几秒内不再尝试，等待原因写驻留账号该模型额度已用完
	var coveragePausedAt time.Time
	// warmOnly：Ultra 主用模型每次额度失败后紧接着的那次选号只用已启动的 Worker（见 withoutColdStart）：不回收、不替换、不现场冷启动，
	// 也不排队等后台扩容；热 Worker 都忙时照常排队（受排队预算限制，其中有每分钟请求已满的另发起一次后台扩容、不等它），
	// 没有热候选、只剩备用账号时发起一次后台扩容后立即返回（本次选号已发起、还没结束的扩容时不再发起）
	warmOnly := coldStartBlocked(ctx)
	// 生成请求的选号带排队进度（见 queue_progress.go）：排队满宽限后写明原因，请求在排队中结束时错误带上最后的原因；没有时不写
	progress := leaseWaitProgressFromContext(ctx)
	var waiter *dispatchWaiter
	// 选号结束时把这次排队的时间计入已等待：之后上游处理与退避的时间不算排队（见 queue_progress.go）
	defer func() {
		progress.acquireEnded(time.Now())
		service.workers.dispatch.leave(waiter)
	}()
	// snapshot 为本轮复查的候选计数，detail 只在真正写排队进度时调用（含同条件队列位置，队列内逐个查找）
	var snapshot queueSnapshot
	detail := func(now time.Time) string {
		text := " | 模型=" + selection.ModelID
		if position, total := service.workers.dispatch.position(waiter); position > 0 {
			text += fmt.Sprintf(" | 同条件队列=%d/%d", position, total)
		}
		if snapshot.egressLimited > 0 {
			// 出口限速与下一个令牌只在真正写进度时取（要读账户池的出口计数）
			snapshot.egressLimit, snapshot.egressNext = service.egressQueueState(egressKey, ultraLane)
		}
		return text + snapshot.text(now)
	}
	wait := func(delay time.Duration, reason string) error {
		if immediate {
			return &noImmediateLeaseError{reason: reason}
		}
		if waiter == nil {
			waiter = service.workers.dispatch.joinAt(dispatchKey(selection), dispatchOrderFromContext(ctx))
		} else if len(selection.ExcludedAccountIDs) > 0 {
			// 排除列表不进队列键，换号重试的请求与同模型的请求同队：复查后仍要等待时把唤醒交给下一个请求，
			// 不让排除了刚空出槽位的账户的队首挡住后面能用它的请求
			service.workers.dispatch.passOn(waiter)
		}
		// 只有因出口令牌桶已满的排队算“出口限速”排队（判定器据此判断限速是否起作用、状态里的排队数）
		if reason == waitReasonEgress && egressBucketWait {
			if egressLeave == nil {
				egressLeave = service.egress.enterEgressWait(egressKey)
			}
		} else {
			leaveEgressWait()
		}
		if reason != waitReasonEgress && reason != waitReasonResidentExhausted {
			return service.waitQueued(ctx, progress, budget, waiter.wake, delay, reason, detail)
		}
		if budget != nil {
			// 主用模型：出口限速的等待照常计入排队预算（PRIMARY_QUEUE_WAIT），超限时建议重试时间按出口的排队长度估计
			err := service.waitQueued(ctx, progress, budget, waiter.wake, delay, reason, detail)
			var busy *aistudio.CapacityBusyError
			if errors.As(err, &busy) {
				now := time.Now()
				if verdict := service.egress.verdict(egressKey, ultraLane, now); reason == waitReasonEgress && !verdict.RetryAt.IsZero() {
					busy.Until = verdict.RetryAt
				}
				service.egress.noteLocalReject(egressKey, now)
			}
			return err
		}
		// 非主用模型：没有排队预算，原先一直排到首事件前期限（线上“超时 504（排队中）”）；按出口排队整个请求累计不超过 egressQueueWaitFor
		limit := service.egressQueueWaitFor()
		waited := egressBudget.total() + egressWaitedHere
		if waited < limit {
			startedAt := time.Now()
			err := service.waitQueued(ctx, progress, nil, waiter.wake, min(delay, limit-waited), reason, detail)
			elapsed := time.Since(startedAt)
			if egressBudget != nil {
				waited = egressBudget.add(elapsed)
			} else {
				egressWaitedHere += elapsed
				waited = egressWaitedHere
			}
			if err != nil || waited < limit {
				return err
			}
		}
		return service.egressQueueExceeded(selection, reason, egressKey, ultraLane, waited, limit)
	}
	// 带排除列表的请求现场冷启动（及随后等容量）期间不看调度信号：开始前离开等待队列并把这次唤醒交给下一个请求，
	// 否则号池变化只唤醒仍在队首的它，后面能用被它排除的账户的请求只能等兜底复查；之后还要等待时按到达序号重新排队
	yieldQueue := func() {
		if waiter == nil || len(selection.ExcludedAccountIDs) == 0 {
			return
		}
		service.workers.dispatch.passOn(waiter)
		service.workers.dispatch.leave(waiter)
		waiter = nil
	}
	// expanding 在这次选号发起的后台扩容结束时关闭：扩容还没结束时不再为同一次选号扩容别的账户，只等它。
	// 原先每次复查（兜底计时与每次号池变化）都会再扩容一个，一个排队的请求能连着冷启动好几个浏览器，
	// 偏好靠前的账户都在启动中时还会启动排在后面的、对主用模型已冷却的账户
	var expanding <-chan struct{}
	for {
		if err := ctx.Err(); err != nil {
			return nil, progress.queueEnded(err)
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
		// 坏号（连续裸限流，见 bad_accounts.go）不做现场提升、扩容与回收后的启动：还有其他备用账号时去掉；固定账户与文件绑定的选号不去
		if !fixedAccount {
			groups.StandbyReady = service.workers.withoutBadAccounts(groups.StandbyReady)
		}
		// 出口限流期间不在受限出口上现场提升、扩容或回收（冷启动出来的账号同样经被限的出口，首个请求多半被拒）：剔除这些出口上的备用账户。
		// 单出口时等于清空备用账户；多出口时扩容自然落到未受限出口的备用账户。由 IP 池分配出口的账户留下（启动时池另分出口，
		// 与冷启动闸门一致，见 filterLimitedStandby）。固定账户与文件绑定的选号不剔除，交给冷启动闸门
		// （文件所属账号要冷启动被暂停时调用方改为复制文件）
		var removedReady []string
		removedStandby := false
		limitedEgress := service.egress.limitedKeys()
		egressKey, egressBucketWait = "", groups.EgressLimited > 0
		if len(limitedEgress) > 0 {
			egressKey = service.egressWaitKey(limitedEgress, ultraLane)
			if !fixedAccount {
				var removedBusy []string
				groups.StandbyReady, removedReady = service.filterLimitedStandby(groups.StandbyReady, limitedEgress)
				groups.StandbyBusy, removedBusy = service.filterLimitedStandby(groups.StandbyBusy, limitedEgress)
				removedStandby = len(removedReady) > 0 || len(removedBusy) > 0
			}
		}
		warmAvailable := append(append([]string(nil), groups.WarmReady...), groups.WarmAvailable...)
		snapshot = queueSnapshot{
			warmIdle: len(warmAvailable), warmBusy: len(groups.WarmBusy),
			standby: len(groups.StandbyReady), standbyBusy: len(groups.StandbyBusy),
			opening: opening, earliestCooldown: groups.EarliestCooldown, rateLimited: groups.RateLimited,
			egressLimited: groups.EgressLimited,
		}
		if len(warmAvailable) > 0 {
			// 不在主用与热门模型里的请求先用对这些模型是死槽位的热 Worker（它们本来就服务不了主用模型），
			// 取不到再用其余热 Worker：还能服务主用模型的 Worker 的租约位留给主用模型
			if dead := service.deadSlotWorkers(selection, fixedAccount, warmAvailable); len(dead) > 0 {
				candidate := selection
				candidate.AllowedAccountIDs = dead
				if lease, _, _ := service.pool.TryAcquireFor(ctx, candidate); lease != nil {
					return lease, nil
				}
			}
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
		warmCandidates := len(warmAvailable) + len(groups.WarmBusy)
		// 补覆盖：驻留里没有能服务该模型的热 Worker（诊断里 pro 的热空闲为 0，或驻留账号对该模型额度都已用完），不是判定期间 10 秒短冷却
		// 造成的，又没有未受限出口的备用账户时，把剔除的备用账户里按偏好排第一的放回去，按补覆盖启动（冷启动闸门另有每分钟配额，
		// 同模型同时只启动一个）。只用已启动 Worker 与不等待的选号不放回；能服务该模型的账户正在启动时先等它
		coverageID := ""
		if len(removedReady) > 0 && warmCandidates == 0 && len(groups.StandbyReady) == 0 && !opening && !warmOnly && !immediate &&
			(coveragePausedAt.IsZero() || time.Since(coveragePausedAt) >= egressCoverageRetry) &&
			(groups.EarliestCooldown.IsZero() || time.Until(groups.EarliestCooldown) > service.egress.limitedCooldown()) {
			coverageID = removedReady[0]
			groups.StandbyReady = []string{coverageID}
			snapshot.standby = 1
		}
		// 备用账户能否启动 Worker 按它所在的 Worker 分区判断：分区未满，或同分区有可淘汰的空闲 Worker
		capacity := service.workers.standbyCapacity(active, selection.ModelID)
		// 出口限流时备用账户已被剔除：同样不扩容，直接按额度 429 后没有已启动的 Worker 返回（Retry-After 5s）
		if warmOnly && warmCandidates == 0 && (len(groups.StandbyReady) > 0 || len(groups.StandbyBusy) > 0 || opening || removedStandby) {
			// 额度 429 之后没有已启动的 Worker，只剩要冷启动的备用账号（或正在启动的）：不现场冷启动（10–30s 以上，冷启动出来的账号
			// 也不一定有额度），为其中一个发起后台扩容（所在分区能扩容时），不等它，立即返回；调用方按额度不足返回 429，
			// 客户端稍后重试时扩容出来的 Worker 多半已经就绪。候选全部冷却仍走下面的全部冷却。
			// 本次选号已由下面的满窗扩容发起过、还没结束的扩容时不再扩容别的账户（例如扩容后热 Worker 被写入冷却、复查落到这里），
			// 一次选号最多冷启动一个；扩容已结束（成功或失败）时照常再扩容一个。其他请求发起的启动不在此列，各请求仍各扩容一个
			if expanding != nil {
				select {
				case <-expanding:
					expanding = nil
				default:
				}
			}
			if len(groups.StandbyReady) > 0 && !fixedAccount && expanding == nil {
				if accountID := capacity.expandable(groups.StandbyReady); accountID != "" {
					service.workers.expandInBackground(accountID, selection.ModelID)
				}
			}
			return nil, &noImmediateLeaseError{reason: primaryQuotaReasonNoWarm}
		}
		// 只用已启动 Worker 的选号有启动中的 Worker 时，走到这里一定还有热候选（没有热候选的已在上面返回）：不按 Worker 正在启动等待，
		// 它不排队等后台扩容（包括下面满窗时自己发起的那次），照常按下面的账号全部忙碌在排队预算内等待
		if len(groups.StandbyReady) == 0 && opening && !(warmOnly && warmCandidates > 0) {
			// 出口限流剔除了备用账户、没有热候选时，正在启动的多半是补覆盖：按驻留账号该模型额度已用完排队（非主用模型受出口排队上限约束）
			reason := "Worker 正在启动"
			if removedStandby && warmCandidates == 0 && !fixedAccount {
				reason = waitReasonResidentExhausted
			}
			if err := wait(schedulingRecheck, reason); err != nil {
				return nil, err
			}
			continue
		}
		// 没有热 Worker 能服务该模型、备用账户所在的分区又已满，而分区里有对该模型是死槽位的空闲 Worker
		// （账号长时间冷却，见 worker_retire.go）时，先现场回收一个再重新选号：不等冷却轮换，也不先冷启动替代 Worker
		// 再等旧 Worker 让位；每轮最多回收一个。还有热 Worker 在忙时不在请求路径上关闭浏览器，交给后台扩容与冷却登记处理
		if len(groups.StandbyReady) > 0 && warmCandidates == 0 && !fixedAccount && !immediate && !warmOnly {
			if victim, partition := capacity.reclaimDead(ctx, groups.StandbyReady); victim != "" {
				// 请求 ID 先取排队进度里的（主路由也有），否则取 /trace/ 排查记录的
				if requestID := leaseRequestID(ctx); requestID != "" {
					service.requests.logRequestProgress(requestID, "service", "INFO", fmt.Sprintf(
						"回收冷却占位的 %s，让备用账号启动 | 账号=%s", workerSlotName(partition), victim,
					))
				}
				continue
			}
		}
		// 不等待的选号不做后台扩容再排队，直接按下面的现场启动处理：分区有空槽时现场冷启动，否则返回没有立即可用的账号。
		// 只用已启动 Worker 的选号同样不排队等扩容，只等忙碌的热 Worker（下面的账号全部忙碌）
		if len(groups.StandbyReady) > 0 && warmCandidates > 0 && !fixedAccount && !immediate && !warmOnly {
			if expanding != nil {
				select {
				case <-expanding:
					expanding = nil
				default:
					if err := wait(schedulingRecheck, waitReasonExpanding); err != nil {
						return nil, err
					}
					continue
				}
			}
			if accountID := capacity.expandable(groups.StandbyReady); accountID != "" {
				expanding = service.workers.expandInBackground(accountID, selection.ModelID)
				if err := wait(schedulingRecheck, waitReasonExpanding); err != nil {
					return nil, err
				}
				continue
			}
		}
		// 只用已启动 Worker 的选号：热 Worker 都忙、其中有每分钟请求已满的（最长要等一分钟才释放，不像并发忙碌很快空出），
		// 又有没冷却的备用账号且所在分区能扩容时，为其中一个发起一次后台扩容，不等它，照常按下面的账号全部忙碌在排队预算内等待，
		// 客户端重试时扩容出来的 Worker 多半已经就绪；只是并发忙碌时仍不扩容。每次选号最多发起一次：扩容的账户进入启动中后，
		// 复查不按 Worker 正在启动等待（见上），热 Worker 随后被写入冷却、落到上面没有热候选的分支时也不在扩容结束前再扩容别的账户
		if warmOnly && !fixedAccount && !immediate && expanding == nil && len(warmAvailable) == 0 && len(groups.WarmBusy) > 0 &&
			groups.WarmRateLimited > 0 && len(groups.StandbyReady) > 0 {
			if accountID := capacity.expandable(groups.StandbyReady); accountID != "" {
				expanding = service.workers.expandInBackground(accountID, selection.ModelID)
			}
		}
		// 替换空闲 Worker 要等旧 Worker 让出，不等待的选号只在分区有空槽时现场启动；只用已启动 Worker 的选号从不现场启动
		promoteID := ""
		if !warmOnly {
			promoteID = capacity.promotable(groups.StandbyReady, warmCandidates == 0 && !immediate)
		}
		if accountID := promoteID; accountID != "" {
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
				if err := wait(schedulingRecheck, "账号忙碌"); err != nil {
					return nil, err
				}
				continue
			}
			yieldQueue()
			workersChanged := service.workers.schedulingChanged()
			// 带上刚取得的租约：启动阶段确认登录失效时在这个租约内恢复认证，不另取租约
			promoteCtx := aistudio.ContextWithAccountLease(ctx, lease)
			if immediate {
				promoteCtx = withImmediateStartup(promoteCtx)
			}
			if accountID == coverageID {
				promoteCtx = withWorkerStartSource(promoteCtx, workerStartCoverage)
			}
			_, promoteErr := service.workers.promote(promoteCtx, accountID, selection.ModelID)
			if promoteErr == nil {
				// 租约在 Worker 启动之前取得，出口是账户当时的默认出口；IP 池静态 IP 下 Worker 另分端口，绑定随之重挂：
				// 租约跟到实际出口（预占一起搬过去），这次尝试的结果、预占与发送都记在请求实际发出的端口上
				lease.RebindEgress()
				return lease, nil
			}
			if releaseErr := lease.Release(); releaseErr != nil {
				return nil, errors.Join(promoteErr, releaseErr)
			}
			if errors.Is(promoteErr, errAccountWorkerOpening) {
				continue
			}
			if errors.Is(promoteErr, errEgressColdStartPaused) {
				switch {
				case immediate:
					return nil, &noImmediateLeaseError{reason: egressColdStartPausedReason}
				case fixedAccount:
					// 固定账户的选号要冷启动、被出口闸门暂停：直接交给调用方，不按“Worker 容量已满”每秒重试（非主用模型没有上限，
					// 会一直排到请求期限）。文件所属账号由 generateWithRetry 改为把文件复制到已启动的账号（ownerUnavailable），
					// Worker 重建后回到原账号的重放据此排除原账号换号（replayUnavailable）
					return nil, promoteErr
				case accountID == coverageID:
					// 补覆盖配额已满或同模型正在补覆盖：按驻留账号该模型额度已用完排队（非主用模型同样受出口排队上限约束）
					coveragePausedAt = time.Now()
					if err := wait(schedulingRecheck, waitReasonResidentExhausted); err != nil {
						return nil, err
					}
					continue
				}
			}
			if errors.Is(promoteErr, errAccountWorkerCapacity) {
				if immediate {
					return nil, &noImmediateLeaseError{reason: "Worker 容量已满"}
				}
				// 等 Worker 状态变化（workersChanged），不等调度队列的唤醒：此前可能已让出队列（yieldQueue）
				if err := service.waitQueued(ctx, progress, budget, workersChanged, schedulingRecheck, "Worker 容量已满", detail); err != nil {
					return nil, err
				}
				continue
			}
			if fixedAccount {
				return nil, promoteErr
			}
			failedWorkers[accountID] = struct{}{}
			failures.last = promoteErr
			continue
		}
		// 热候选取租约时被其他请求抢先（分类时还有空位，TryAcquireFor 返回空）同样按账号全部忙碌等待，不落到下面的冷却判断。
		// 没有空闲的热候选、其中有因出口令牌桶已满按忙碌分组的：按“出口限速”排队
		busyReason := "账号全部忙碌"
		if len(warmAvailable) == 0 && groups.EgressLimited > 0 {
			busyReason = waitReasonEgress
		}
		if len(warmAvailable) > 0 || len(groups.WarmBusy) > 0 || len(groups.StandbyBusy) > 0 || opening {
			if err := wait(schedulingRecheck, busyReason); err != nil {
				return nil, err
			}
			continue
		}
		// 还有没冷却的备用账户，只是所在分区没有空槽、也没有可回收或可替换的空闲 Worker：这是容量原因，按没有空闲的 Worker 槽位排队
		// （主用模型受排队预算限制，其余模型等到请求超时），不按全部冷却返回 429，也不按冷却等待绕过排队预算。EarliestCooldown 是
		// 候选集里最早恢复的账户（常常是冷却中的热 Worker 或其他备用账户），不代表槽位何时空出，原先据此返回的 Retry-After 可能长达几小时。
		// 降级判定的 Playground 优先选号除外：它全部冷却时调用方立即改为不限通道选号
		// 补覆盖放回的账户拿不到槽位（分区已满、没有可替换的空闲 Worker）不在这里等：它是出口限流期间剔除的备用账户，
		// 按下面出口限流的分支排队（非主用模型受出口排队上限约束），原先按没有空闲的 Worker 槽位一直排到请求期限
		if len(groups.StandbyReady) > 0 && !selection.PlaygroundFirst && coverageID == "" {
			if err := wait(schedulingRecheck, waitReasonNoSlot); err != nil {
				return nil, err
			}
			continue
		}
		// 出口限流剔除了备用账户、又没有热候选：号池里还有健康的备用账户，只是暂不在受限出口上冷启动，按容量原因排队，
		// 不落到下面的全部冷却（EarliestCooldown 常常是长冷却的热 Worker，Retry-After 可能长达几小时）。补覆盖被暂停或拿不到槽位时
		// 写驻留账号该模型额度已用完
		if removedStandby && warmCandidates == 0 && !fixedAccount && !selection.PlaygroundFirst {
			reason := waitReasonEgress
			if !coveragePausedAt.IsZero() || coverageID != "" {
				reason = waitReasonResidentExhausted
			}
			if err := wait(schedulingRecheck, reason); err != nil {
				return nil, err
			}
			continue
		}
		if !groups.EarliestCooldown.IsZero() {
			remaining := time.Until(groups.EarliestCooldown)
			// Ultra 主用模型（额度快速失败）候选全部冷却时立即返回，不原地等一分钟内的恢复，也不因此延后排队截止时间：
			// 等到的多半是分钟限额刚恢复、随即又被打满的账号，客户端按 Retry-After（最早恢复）重试更快
			if remaining > cooldownQueueLimit || budget.failFastOnQuota() {
				return nil, &aistudio.AllCoolingError{ModelID: selection.ModelID, Until: groups.EarliestCooldown}
			}
			if err := wait(coolingRecheckDelay(remaining), waitReasonCooling); err != nil {
				return nil, err
			}
			continue
		}
		if candidates > 0 {
			reason := waitReasonNoSlot
			if groups.EgressLimited > 0 {
				reason = waitReasonEgress
			}
			if err := wait(schedulingRecheck, reason); err != nil {
				return nil, err
			}
			continue
		}
		if promoteFailure := failures.last; promoteFailure != nil {
			// 候选因启动时登录失效退出调度：按账户需要重新登录返回（503，列出账户与原因），并保留启动失败原因
			var notReady *aistudio.AccountsNotReadyError
			if noEligible := service.pool.NoEligibleError(selection); aistudio.DefinitiveAuthenticationFailure(promoteFailure) &&
				errors.As(noEligible, &notReady) {
				return nil, errors.Join(noEligible, promoteFailure)
			}
			return nil, promoteFailure
		}
		if runtimeBusyErr != nil {
			// 候选账户的 runtime 都被其他进程占用：号池一侧的暂时性原因，按 503 返回
			return nil, aistudio.PoolNotReadyIn(selection.Pool, "候选账户被其他进程占用", runtimeBusyErr)
		}
		return nil, service.pool.NoEligibleError(selection)
	}
}

// deadSlotWorkers 返回 warmAvailable 中对热门或主用模型是死槽位（长时间冷却）的账户，一次 LongCoolingSet：
// 只用于不固定账户、模型不在主用与热门模型里的请求；没有死槽位或全部都是死槽位（与直接选号相同）时返回 nil。
// 主用模型按选号的号池取（deadSlotModelsForScope）：模型在该号池的主用列表里时它本身就是要保护的模型，返回 nil
func (service *trackedService) deadSlotWorkers(selection aistudio.AccountSelection, fixedAccount bool, warmAvailable []string) []string {
	if fixedAccount || service.workers == nil || len(warmAvailable) < 2 {
		return nil
	}
	models := service.workers.deadSlotModelsForScope(selection.Pool)
	modelID := normalizeGuardModel(selection.ModelID)
	if len(models) == 0 || modelID == "" {
		return nil
	}
	for _, deadModel := range models {
		if normalizeGuardModel(deadModel) == modelID {
			return nil
		}
	}
	dead := service.pool.LongCoolingSet(warmAvailable, models, rotationMinRemaining, false)
	if len(dead) == 0 || len(dead) == len(warmAvailable) {
		return nil
	}
	result := make([]string, 0, len(dead))
	for _, accountID := range warmAvailable {
		if _, ok := dead[accountID]; ok {
			result = append(result, accountID)
		}
	}
	return result
}

// schedulingRecheck 为等待调度信号时的兜底复查间隔
const schedulingRecheck = time.Second

// cooldownWakeJitter 为候选全部冷却、最早恢复不到一次兜底复查间隔时的随机延后上限：冷却到期不发调度信号，排队请求各按自己的计时器复查，
// 加抖动免得同一时刻一起在账户池锁上分类候选
const cooldownWakeJitter = 500 * time.Millisecond

// coolingRecheckDelay 返回候选全部冷却时下一次复查的等待：最早恢复不少于一次兜底复查间隔时按兜底间隔；不到时等到恢复，
// 再随机延后不到 cooldownWakeJitter（只在最后一次不足 1 秒的等待上加）
func coolingRecheckDelay(remaining time.Duration) time.Duration {
	if remaining >= schedulingRecheck {
		return schedulingRecheck
	}
	return max(remaining, 0) + rand.N(cooldownWakeJitter)
}

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

// activityEpoch 为 upstreamActivity 记录时间的起点：lastMono 存相对它的单调时钟纳秒，墙钟跳变不影响首事件后空闲判断
var activityEpoch = time.Now()

type upstreamActivity struct {
	bytes atomic.Int64
	// lastMono 为最近一次收到上游字节相对 activityEpoch 的纳秒数加 1（0 表示还没有字节）
	lastMono atomic.Int64
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
	activity.lastMono.Store(int64(time.Since(activityEpoch)) + 1)
	activity.bytes.Add(int64(count))
}

// sinceLastByte 返回最近一次上游字节距 now 的时间（now 应来自 time.Now()，带单调时钟读数）；还没有字节时 ok 为假
func (activity *upstreamActivity) sinceLastByte(now time.Time) (time.Duration, bool) {
	if activity == nil {
		return 0, false
	}
	last := activity.lastMono.Load()
	if last == 0 {
		return 0, false
	}
	return now.Sub(activityEpoch) - time.Duration(last-1), true
}

// quietFor 返回上游连续没有活动的时间：从 progress（最近一次收到事件或向下游交付完成）与最近一次上游字节中较晚的一个算起
func (activity *upstreamActivity) quietFor(now, progress time.Time) time.Duration {
	quiet := now.Sub(progress)
	if since, ok := activity.sinceLastByte(now); ok && since < quiet {
		quiet = since
	}
	return quiet
}

func (activity *upstreamActivity) logFields(now time.Time) string {
	since, ok := activity.sinceLastByte(now)
	if !ok {
		return "网络字节=0"
	}
	return fmt.Sprintf(
		"网络字节=%d | 最近网络=%s",
		activity.bytes.Load(), since.Round(time.Millisecond),
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
	// 安全设置中无法识别的类别或阈值跳过并记 WARN（同时写进排查时间线），不返回 400，其余设置照常发送
	if len(request.SafetySettings) > 0 {
		var skipped []string
		request.SafetySettings, skipped = aistudio.NormalizeSafetySettings(request.SafetySettings)
		if len(skipped) > 0 {
			service.requests.logRequestProgress(request.ID, "service", "WARN", "安全设置已跳过无法识别的条目 | "+strings.Join(skipped, "；"))
		}
	}
	trace.AddParameters("发往上游（seed 与随机后缀处理后）", request)
	trace.SetPrompt(diag.promptHash, request)
	api.StartAccessLog(ctx)
	request.Model = service.pool.CanonicalModelIDIn(aistudio.PoolScopeFromContext(ctx), request.Model)
	// 选号前就能判断的参数错误直接返回 400，不占用账号：既没有系统提示也没有对话内容（例如消息全被过滤掉），
	// 工具选择本身不成立（指定了未声明的函数、要求调用却没有工具），停止序列超限，或媒体分辨率不是可用的枚举名
	var invalid error
	if len(request.Contents) == 0 {
		invalid = fmt.Errorf("%w: 请求没有系统提示或对话内容", aistudio.ErrInvalidArgument)
	} else if err := aistudio.ValidateToolChoice(request.Tools); err != nil {
		invalid = fmt.Errorf("%w: %v", aistudio.ErrInvalidArgument, err)
	} else if err := aistudio.ValidateStopSequences(request.Config.StopSequences); err != nil {
		invalid = fmt.Errorf("%w: %v", aistudio.ErrInvalidArgument, err)
	} else if err := aistudio.ValidateMediaResolution(request.Config.MediaResolution); err != nil {
		invalid = fmt.Errorf("%w: %v", aistudio.ErrInvalidArgument, err)
	}
	if invalid != nil {
		api.SetAccessLogError(ctx, invalid)
		service.requests.start(ctx, request, func() {})
		service.requests.finish(request.ID, "failed", invalid)
		return nil, nil, invalid
	}
	// 降级判定：被拦截的模型准备判定；同一段对话近期被判定为降级、且当时的消息原样都在时，发送前直接拒绝（不换号重试）
	gate, remembered := service.prepareDowngradeGate(ctx, request, guardContents)
	if remembered != nil {
		api.SetAccessLogError(ctx, remembered)
		service.requests.start(ctx, request, func() {})
		service.recordDowngradeDecision(ctx, request.ID, "", request.Model, remembered.Decision, true)
		service.requests.finish(request.ID, "failed", remembered)
		return nil, nil, remembered
	}
	requestCtx, cancel, err := service.generateRequestContext(ctx)
	if err != nil {
		api.SetAccessLogError(ctx, err)
		service.requests.start(ctx, request, func() {})
		service.requests.finish(request.ID, "failed", err)
		return nil, nil, err
	}
	service.requests.start(ctx, request, cancel)
	resourceID, err := service.pool.ResourceIDForContents(requestCtx, request.Contents)
	if err != nil {
		// 文件资源刷新属于首事件前：期限到期时同样标明是本服务的首事件前期限
		if cut, ok := generateContextFrom(requestCtx).startDeadlineError(err, "文件资源刷新"); ok {
			err = cut
		}
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
	// scope 为请求可以使用的号池：换号、文件复制与通道回退都只在这个号池内进行
	scope := aistudio.PoolScopeFromContext(requestCtx)
	// phase 为生成请求的上下文（见 generate_context.go）；测试直接传普通 context 时为 nil，各方法按原来的整段期限处理
	phase := generateContextFrom(requestCtx)
	// startCutWhere 为首事件前期限在某次尝试中到期时那次尝试停在哪一步（写进最终错误，见 startDeadlineError）
	startCutWhere := ""
	maxAttempts := 1
	requestedAccountID := strings.TrimSpace(request.AccountID)
	modelID := strings.TrimPrefix(strings.TrimSpace(request.Model), "models/")
	unbound := requestedAccountID == "" && resourceID == ""
	fileBound := requestedAccountID == "" && resourceID != ""
	if unbound || fileBound {
		_, eligible := service.pool.EnabledAccountsIn(scope)
		if eligible > 1 {
			maxAttempts = eligible
		}
	}
	var lease *aistudio.AccountLease
	var source <-chan aistudio.Event
	var first aistudio.Event
	var activity *upstreamActivity
	// finishAttempt 为成功尝试的上下文取消函数，事件流转发结束后调用
	var finishAttempt context.CancelCauseFunc
	var temporaryCopies *aistudio.TemporaryFileCopies
	var err error
	originalContents := request.Contents
	attempted := make(map[string]struct{}, maxAttempts)
	// tried 为本请求取得过租约的不同账号（每请求换号硬上限 generateSwitchCeiling，见 quota_switch.go）：单独维护，
	// 不受 attempted 的删除（同账号换通道、Worker 重建后重放、回到原账号）与 maxAttempts 的补偿影响
	tried := make(map[string]struct{}, generateSwitchCeiling)
	recoveredWorker := make(map[string]struct{})
	recoveryAccountID := ""
	copyFiles := false
	// buildOnly：Playground 返回输入 token 超过上限后，后续尝试只走 Build 通道
	buildOnly := false
	// inputLimitErr 为最近一次"输入 token 超过上限"的上游错误：为它安排的再试（改走 Build、改用更高权益账号）没有立即可用的账号时
	// 按这个错误结束，不排队等待，也不换成限流返回；immediateRetry 让下一次选号不等待；tierRetryAccountIDs 限定下一次选号的账号；
	// tierRetried 保证更高权益账号只试一次
	var inputLimitErr error
	var tierRetryAccountIDs []string
	immediateRetry, tierRetried := false, false
	// authFailures：本请求遇到的登录失效（上游 401 / Code 16、登录页跳转）次数，超过 authFailureSwitchLimit 次换号后不再换号
	authFailures := 0
	// hardFailures：既没有让账号进入冷却、也不是 Worker 重建或改走 Build 的失败次数（5xx、404、超时、带文件的 403 等；
	// 单模型 403 写入冷却后同样计入，但不退避；没等到同账号关闭失败的 Worker 就换号时同样计入，已经等过，不再退避）。
	// 这类失败往往与请求本身有关，换号次数不再随账号数增长，并在两次之间退避。租约账号在发出前已进入冷却（ErrAccountCoolingDown，
	// 请求没有发到上游）不计入
	hardFailures := 0
	// deferredReset、deferredAccountID 为最近一次没等到关闭失败 Worker 就换号的账号与它的后台关闭（见 worker_reset.go）：
	// 换号找不到可用账号、换号次数用完或到连续失败上限时回到这个账号，等关闭结束后在原账号重放；换号的选号排队时关闭结束、
	// 可以重放就不再排除这个账号，按原来的排队位置重新选号（不固定到它，见下文 errWorkerResetReady 的处理）
	var deferredReset *workerReset
	deferredAccountID := ""
	// returnToDeferred 在请求不能再换号时（why 为原因，reason 为写进进度的错误）回到 deferredReset 的原账号：一直等后台关闭结束，
	// 能重放时安排下一次尝试在原账号重放（replay）；请求在等待中超时或被取消时把 err 换成带等待原因的 ctx 错误（ended，504/499 映射不变，
	// 原先丢掉 ctx 错误、按上次的 Code 5 失败对外 502）；没有要等的原账号、请求已结束、后台放弃或关闭出错时两者都为 false，按原来的错误结束
	returnToDeferred := func(why string, reason error) (replay bool, ended bool) {
		if deferredReset == nil || !deferredReset.awaitable() || requestCtx.Err() != nil {
			return false, false
		}
		pending, accountID := deferredReset, deferredAccountID
		deferredReset, deferredAccountID = nil, ""
		trace.Note(why + "，回到原账号等待 Worker 恢复: " + reason.Error())
		service.requests.logRequestProgress(request.ID, "service", "WARN",
			why+"，回到原账号等待 Worker 恢复后重放 | 原因="+progressReason(reason))
		waitStartedAt := time.Now()
		finished, waitErr := pending.wait(requestCtx, workerResetWaitUnbounded)
		if waitErr != nil {
			err = workerResetWaitEnded(err, waitErr, time.Since(waitStartedAt))
			return false, true
		}
		if !finished || !pending.replayable() {
			return false, false
		}
		recoveryAccountID = accountID
		delete(attempted, accountID)
		maxAttempts++
		return true, false
	}
	// 主用模型（按请求的号池取普通列表 PRIMARY_MODELS 或 Ultra 列表 ULTRA_PRIMARY_MODELS，见 primaryPolicyFor）的请求带一份排队预算，
	// 整个请求共用：换号重试不重新计时，失败的尝试与退避都计入截止时间，
	// 截止时间过后只接受立即可用的账号，否则按 429 结束（见 primary_models.go）；其他模型为 nil，原样排队
	budget := service.primaryBudget(modelID, scope)
	// 额度失败计入换号上限（见 quota_switch.go 与 primary_models.go）：不固定账号与文件绑定的请求额度失败分两类计数（每日类、限流类，
	// 见 quotaSwitchPolicy.classOf），两类都不计入连续失败上限、不退避；某类额度失败超过它的换号上限，或额度失败后换号没有已启动的 Worker、
	// 其余候选全部冷却、没有其他可换的账号时按 PrimaryQuotaError 结束。Ultra 主用模型（ULTRA_PRIMARY_MODELS）沿用原规则：
	// 租约账号在发出前只是短暂冷却时不算额度失败（见 coolingDownLong），候选全部冷却时立即返回（failFast）。指定账号的请求不计数
	policy := service.quotaPolicyFor(budget, unbound || fileBound || budget.failFastOnQuota())
	failFast := policy.ultra
	// meta 为构造额度不足错误的请求信息；accounts 在结束时按 tried 填
	meta := quotaFailureMeta{modelID: modelID, pool: scope, startedAt: generationStartedAt}
	// 流式请求：自己现场冷启动时推迟写出响应头，启动结束后才遇到的额度不足仍以 429 返回（见 holdStreamStart）
	budget.holdStreamStart(aistudio.StreamStartHoldFromContext(clientCtx))
	quota := primaryQuotaTracker{policy: policy}
	// warmOnlyNext：上一次尝试是额度失败，下一次选号只用已启动的 Worker（withoutColdStart），取得租约后清除；
	// 标记只加在那次选号的 context 上，不进整个请求共用的 acquireCtx
	warmOnlyNext := false
	// 排队进度同样整个请求共用一份，带上请求 ID：主路由（没有排查记录）的排队原因与回收记录也能写进请求时间线（见 queue_progress.go）
	progress := newLeaseWaitProgress(request.ID)
	baseCtx := withLeaseWaitBudget(requestCtx, budget)
	if budget == nil {
		// 非主用模型按“出口限速”的排队整个请求累计不超过 egressQueueWaitMax，换号重试不重新计时（见 egress_wait.go）
		baseCtx = withEgressWaitBudget(baseCtx)
	}
	acquireCtx := withLeaseWaitProgress(withDispatchOrder(baseCtx), progress)
	for attempt := 0; attempt < maxAttempts; attempt++ {
		budget.beginAttempt()
		progress.beginAttempt()
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
			AccountID: selectionAccountID, ResourceID: selectionResourceID, Pool: scope,
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
		// 换号重试排除已尝试过的账户（通常只有几个）：选号留在候选索引的快路径上，与同模型的请求同队排队；
		// 原先把"全部启用账户减去已尝试的"作为限定列表，3 万多个账户时每次复查候选都在账户池锁内建表排序几十毫秒
		if (unbound || fileBound) && len(attempted) > 0 {
			selection.ExcludedAccountIDs = make([]string, 0, len(attempted))
			for accountID := range attempted {
				selection.ExcludedAccountIDs = append(selection.ExcludedAccountIDs, accountID)
			}
		}
		// 更高权益账号的再试限定在一小组账号上（见下文输入超限的处理），仍用限定列表
		if tierRetryAccountIDs != nil {
			selection.AllowedAccountIDs = tierRetryAccountIDs
			tierRetryAccountIDs = nil
		}
		useImmediate := immediateRetry
		immediateRetry = false
		// 没等到关闭就换走的原账号在这次选号里仍被排除：选号排队时它的关闭结束、可以重放就提前结束（errWorkerResetReady），
		// 随后不再排除它、重新选号，不只等其他账号空出槽位（一直排除时原账号关闭完成后也用不上，可能一直等到请求超时）
		selectCtx := withWorkerResetWake(acquireCtx, deferredReset)
		if warmOnlyNext {
			selectCtx = withoutColdStart(selectCtx)
		}
		var nextLease *aistudio.AccountLease
		var acquireErr error
		// 流式优先 Playground（STREAM_PLAYGROUND_MODELS）：先不等待地取 Playground 账号，Playground 繁忙或没有可用账号时
		// 立即按默认条件在全部启用通道中选号，不排队等 Playground。降级判定的 Playground 优先（会等待）不受影响
		// 输入超限后的再试只接受现在就能取得的账号，不排队
		if useImmediate {
			nextLease, acquireErr = service.acquireWarmLeaseNow(selectCtx, selection, nil)
		} else if service.streamPrefersPlayground(request, selection) {
			nextLease, acquireErr = service.acquireStreamPlaygroundLease(selectCtx, request.ID, selection)
		} else {
			nextLease, acquireErr = service.acquireWarmLease(selectCtx, selection)
		}
		if acquireErr != nil && selection.PlaygroundFirst && requestCtx.Err() == nil && (playgroundUnavailable(acquireErr) || noWarmWorker(acquireErr)) {
			// 没有可用的 Playground 账号（不支持该模型或全部冷却）：退回其他通道，Build 通道按估算 + CountTokens 判定。
			// 只用已启动 Worker 的选号在 Playground 上没有热 Worker 时同样退回：已启动的账号可能还能走 Build
			trace.Note("降级判定：Playground 通道没有可用账号（" + acquireErr.Error() + "），改为不限通道选号")
			selection.PlaygroundFirst = false
			nextLease, acquireErr = service.acquireWarmLease(selectCtx, selection)
		}
		if errors.Is(acquireErr, errWorkerResetReady) {
			if requestCtx.Err() == nil {
				// 换号的选号在排队，原账号的失败 Worker 已关闭：把原账号移出排除列表，按同样的条件重新选号（不占换号次数），
				// 不固定到原账号。关闭后原账号没有 Worker，要冷启动并占一个 Worker 槽位：分区满时（号池大时常态，换号排队又正是
				// 号池忙的时候）固定到它只能等槽位，这次排队刚为备用账号发起的扩容占着空出的槽位，扩容出来的 Worker 的空闲并发却
				// 交给后面排队的请求，请求一直等到超时。重新选号沿用请求的到达序号，排回原来的位置，原账号与其他账号哪个先能用就用哪个。
				// 关闭还没结束时换号没有可用账号、换号次数用完或到连续失败上限仍回到原账号等关闭结束后重放（returnToDeferred）：那时没有别的账号可用
				accountID := deferredAccountID
				deferredReset, deferredAccountID = nil, ""
				trace.Note("换号排队中原账号的失败 Worker 已关闭，原账号重新参与选号")
				service.requests.logRequestProgress(request.ID, "service", "WARN", "换号排队中原账号的 WAA Worker 已关闭完成，原账号重新参与选号")
				delete(attempted, accountID)
				maxAttempts++
				continue
			}
			// 请求恰好同时结束：按请求结束的错误处理
			acquireErr = progress.queueEnded(requestCtx.Err())
		}
		if acquireErr != nil {
			trace.Note("获取账号失败: " + acquireErr.Error())
			if inputLimitErr != nil {
				// 输入超限后的再试（改走 Build、改用更高权益账号）没有立即可用的账号：按输入超限的原错误结束（对外 400），
				// 不排队等冷却或 Worker，也不把它换成全部冷却、号池繁忙等限流错误
				service.requests.logRequestProgress(request.ID, "service", "WARN", "输入超限后的再试没有立即可用的账号，按 400 返回 | 原因="+progressReason(acquireErr))
				err = inputLimitErr
				break
			}
			var capacityBusy *aistudio.CapacityBusyError
			if errors.As(acquireErr, &capacityBusy) {
				// 主用模型排队超过上限：立即按 429 结束，不换号、不复制文件、不冷却、不退避
				service.requests.logRequestProgress(request.ID, "service", "WARN", primaryQueueExceededMessage(capacityBusy))
				err = acquireErr
				break
			}
			// ownerUnavailable：文件所属账号不可用（没有符合条件的账号、全部冷却，或出口限流期间它的冷启动被闸门暂停），
			// 下面改为复制文件到其他账号再试（见 fileOwnerUnavailable）
			ownerUnavailable := fileBound && !copyFiles && requestCtx.Err() == nil && fileOwnerUnavailable(acquireErr)
			// replayUnavailable：不固定账号的请求在 Worker 重建后固定到原账号的重放选号失败，下面只排除这个账号、照常换号
			replayUnavailable := recoveringAccount && unbound
			if policy.counted && quota.hits() > 0 && !ownerUnavailable && !replayUnavailable && requestCtx.Err() == nil {
				// 额度失败后的换号没有可用的已启动 Worker、其余候选全部冷却或没有其他可换的账号：按额度不足立即返回 429，
				// 不回到没等到关闭的原账号等待（returnToDeferred），也不回落到原地等待或冷启动。文件所属账号不可用时先照常复制文件到其他账号再试；
				// Worker 重建后的重放选号只看了原账号一个（例如它在重建期间被其他请求的额度 429 冷却），不代表其余候选，
				// 同样先排除它换号，下一次选号再按全部候选判断
				if reason, cooling, ok := primaryQuotaSelectionReason(acquireErr); ok {
					meta.accounts = len(tried)
					err = service.finishQuotaFailure(request.ID, "service", &quota, meta, reason, cooling, nil)
					break
				}
			}
			if switchUnavailable(acquireErr) {
				// 没等到同账号关闭失败的 Worker 就换号，换号却没有可用账号（例如只有原账号支持该模型、其余账号都在冷却）：
				// 回到原账号，一直等后台关闭结束后在原账号重放，与固定账号的请求相同；后台放弃或关闭出错时按原来的错误结束；
				// 请求在等待中超时或被取消时按请求结束的错误结束，不再落到下面按选号错误处理
				replay, ended := returnToDeferred("换号没有可用账号", acquireErr)
				if replay {
					continue
				}
				if ended {
					break
				}
			}
			if ownerUnavailable {
				// 文件所属账号不可用：改为把文件复制到其他账号再试一次。这次重试不占用换号次数，
				// 否则只有一个可调度账号时循环直接结束，既没有错误也没有租约
				copyFiles = true
				err = acquireErr
				maxAttempts++
				continue
			}
			if !replayUnavailable && errors.Is(acquireErr, errEgressColdStartPaused) {
				// 固定账户（指定账户的请求、账号测试）要冷启动、被出口闸门暂停：与数据路径相同按号池繁忙返回（429，Retry-After 5s），
				// 不以裸错误按 502 结束。只在这里包装：acquireLease 返回原错误，文件绑定的请求据此改为复制文件（上面的 ownerUnavailable），
				// 包装成 CapacityBusyError 会被上面的号池繁忙分支直接结束
				acquireErr = newEgressColdStartBusy(selection.ModelID, scope, acquireErr)
			}
			if err == nil || !errors.Is(acquireErr, aistudio.ErrNoEligibleAccount) {
				err = acquireErr
			}
			if replayUnavailable && requestCtx.Err() == nil {
				attempted[selectionAccountID] = struct{}{}
				continue
			}
			if policy.counted && quota.lastErr != nil && errors.Is(err, quota.lastErr) && requestCtx.Err() == nil {
				// 额度失败后的换号只剩号池一侧暂时不可调度的账号（需要重新登录、已停用、runtime 被其他进程占用等，
				// AccountsNotReadyError），上面也回不到没等到关闭的原账号：按没有其他可换的账号返回额度不足（429，带 Retry-After），
				// 不以上一次不带 Retry-After 的上游额度错误结束。上一次失败不是额度失败时 err 不是它，照原来的错误结束
				meta.accounts = len(tried)
				err = service.finishQuotaFailure(request.ID, "service", &quota, meta, primaryQuotaReasonNoOther, nil, nil)
				break
			}
			var cooling *aistudio.AllCoolingError
			if failFast && errors.As(err, &cooling) {
				// Ultra 主用模型候选全部冷却：不原地等恢复，立即按 429 返回（Retry-After 为最早恢复，见 acquireLease）
				service.requests.logRequestProgress(request.ID, "service", "WARN", primaryAllCoolingMessage(cooling))
			}
			break
		}
		lease = nextLease
		warmOnlyNext = false
		err = nil
		source = nil
		request.AccountID = lease.Account().ID
		workerGeneration := service.workers.WorkerGeneration(request.AccountID)
		attempted[request.AccountID] = struct{}{}
		tried[request.AccountID] = struct{}{}
		// 租约所在的出口与分区通道（现场提升时已跟到新 Worker 的出口，见 acquireLease）：这次尝试的结果按它交给出口判定器（见 egress_guard.go）
		egressKey, egressUltra := lease.EgressKey(), lease.EgressLane() == aistudio.EgressLaneUltra
		accountLabel := lease.Account().Config.Label
		if lease.ClaimRateWindowNote() {
			// 这个租约刚好占满账号的每分钟窗口（同一账号 10 分钟最多一条，名额在这里写日志时才占用，转写等不写日志的路径不占用）
			service.requests.log(accountLabel, "INFO", fmt.Sprintf(
				"账号每分钟请求已满 | 上限=%d 次/60 秒 | 窗口释放前新请求换到其他账号，热 Worker 都满且分区有空槽时扩容",
				service.pool.AccountRateLimit()))
		}
		trace.StartAttempt(accountLabel, string(lease.Channel()))
		attemptStartedAt := time.Now()
		api.SetAccessLogChannel(requestCtx, string(lease.Channel()))
		api.SetAccessLogTarget(requestCtx, modelID, accountLabel)
		api.MarkAccessLogScheduled(requestCtx)
		service.requests.markRunning(request.ID, request.AccountID, accountLabel)
		service.requests.markChannel(request.ID, string(lease.Channel()))
		service.requests.logRequestProgress(request.ID, accountLabel, "INFO", "等待上游响应")
		// 每次尝试有自己的上下文：首事件超时只取消这一次尝试，请求本身继续换号重试。先从 requestCtx 派生再附加租约：
		// 直接子上下文登记在请求上下文上（不起 goroutine），收到首事件后随请求上下文延长到总时长上限
		attemptBase, cancelAttempt := context.WithCancelCause(requestCtx)
		attemptCtx := aistudio.ContextWithAccountLease(attemptBase, lease)
		// 租约来自 acquireLease，取得租约前已确认或启动了 Worker：发送时 Worker 不在了（失败后被关闭、被淘汰）才要重新启动，
		// 来源记为重开，冷启动闸门不拦它（见 accountWorkerManager.Worker）
		attemptCtx = withWorkerStartSource(attemptCtx, workerStartReopen)
		firstEvent := newFirstEventDeadline(time.Duration(service.firstEventTimeout.Load()), cancelAttempt)
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
			if phase == aistudio.RequestPhaseSendingUpstream {
				// 首事件超时从向上游发送时开始计（WAA proof 排队与生成不计入）
				firstEvent.start()
			}
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
		// 降级判定的输入字数按本次尝试实际发送的内容（工具约束提示、附带的 Schema、截断之后）校准
		if gate := diag.guard(); gate != nil {
			attemptCtx = aistudio.ContextWithSentInputObserver(attemptCtx, gate.observeSentInput)
		}
		// generated 表示本次尝试已经向上游发出生成请求：之后的错误来自生成本身，不是文件复制或附件上传
		generated := false
		if err == nil {
			generated = true
			source, err = service.service.Generate(attemptCtx, request)
			// 发送时 Worker 不在了、在这个租约内重新启动时可能换了端口：租约在发送前已跟到实际出口（见 RebindEgress），重读一次
			egressKey, egressUltra = lease.EgressKey(), lease.EgressLane() == aistudio.EgressLaneUltra
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
			// 没有报告发送阶段的通道从拿到事件流时开始计
			firstEvent.start()
			upstreamStartedAt := time.Now()
			firstEventDelayed := false
			first, err = firstGenerateEvent(attemptCtx, source, func() {
				firstEventDelayed = true
				service.requests.logRequestProgress(request.ID, accountLabel, "WARN", fmt.Sprintf(
					"上游首事件等待 | 已等待=%s | 模型=%s | %s",
					streamStallThreshold, modelID, activity.logFields(time.Now()),
				))
			}, func() {
				// 收到首个上游事件（含错误事件）即停止计时：错误流关闭得慢时不能把上游返回的错误改成首事件超时
				_ = firstEvent.stop()
			})
			if firstEventDelayed && err == nil {
				service.requests.logRequestProgress(request.ID, accountLabel, "INFO", fmt.Sprintf(
					"上游首事件到达 | 等待=%s | 事件=%s | 模型=%s",
					time.Since(upstreamStartedAt).Round(time.Millisecond), first.Kind, modelID,
				))
			}
		}
		// 停止计时。计时已到时本次尝试的上下文已取消：即使恰好收到了首事件也按超时处理（还没有向客户端输出任何内容），
		// 读完事件流确认上游 goroutine 退出后再释放租约；上游先返回的真实错误保留原样
		if timeoutErr := firstEvent.stop(); timeoutErr != nil && requestCtx.Err() == nil && (err == nil || errors.Is(err, context.Canceled)) {
			err = timeoutErr
			if !drainAttemptEvents(source, attemptDrainTimeout) {
				service.requests.logRequestProgress(request.ID, accountLabel, "WARN", "首事件超时后上游事件流未及时结束，继续换号")
			}
			service.requests.logRequestProgress(request.ID, accountLabel, "WARN", fmt.Sprintf(
				"上游首事件超时 | 超时=%s | 模型=%s | %s", firstEvent.timeout, modelID, activity.logFields(time.Now()),
			))
		}
		// 停止首事件超时计时之后、转发首事件之前切到首事件后（期限延长到总时长上限）。首事件与首事件前期限到期（或请求取消）
		// 同时发生时期限已经结束了请求：这个事件不转发（还没有向客户端输出任何内容），按请求结束的错误处理（到期对外 504，取消 499/503），
		// 读完事件流确认上游 goroutine 退出后再释放租约。顺序不能反：先切换再检查首事件超时，会交出一个已被首事件超时取消的尝试
		sameTime := false
		if err == nil && !phase.beginStreaming() {
			sameTime = true
			err = requestCtx.Err()
			cancelAttempt(err)
			if !drainAttemptEvents(source, attemptDrainTimeout) {
				service.requests.logRequestProgress(request.ID, accountLabel, "WARN", "首事件到达时请求已结束，上游事件流未及时结束")
			}
			service.requests.logRequestProgress(request.ID, accountLabel, "WARN", "上游首事件到达时请求已结束，不转发 | 原因="+progressReason(err))
		}
		// 首事件前期限在这次尝试中到期：记下停在哪一步，写进最终错误（见 startDeadlineError）
		if err != nil && startCutWhere == "" && phase.startExpired() &&
			(errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)) {
			startCutWhere = startCutStage(generated, source != nil, sameTime, prepareTiming)
		}
		if err == nil {
			api.SetAccessLogFirstEvent(requestCtx, time.Since(generationStartedAt))
			service.requests.logRequestProgress(request.ID, accountLabel, "INFO", "收到上游首个事件")
			service.workers.ipPool().succeeded(request.AccountID)
			service.quota.attemptSucceeded(request.AccountID, modelID, lease.Channel(), lease.CheckedAt())
			api.SetAccessLogTarget(requestCtx, first.ProviderModel, accountLabel)
			temporaryCopies = attemptCopies
			service.forbidden.reset(request.AccountID)
			service.forbidden.resetModel(request.AccountID, lease.CooldownScope(modelID))
			service.forbidden.modelSucceeded(scope, modelID, time.Now())
			service.rateLimits.succeeded(scope, modelID, time.Now())
			service.egress.observeOutcome(egressObservation{
				Key: egressKey, AccountID: request.AccountID, Ultra: egressUltra, Outcome: egressSuccess, At: time.Now(),
			})
			// 真正成功过的账户：清掉登录复核的退避（见 auth_recheck.go）与坏号记录，记为近期成功（预热与替补优先，见 bad_accounts.go）
			service.authRecheck.noteSuccess(request.AccountID)
			service.workers.badAccountMemory().observe(request.AccountID, accountLabel, egressSuccess)
			service.permissionDenied.succeeded(request.AccountID, modelID, lease.Channel())
			trace.FinishAttempt(nil)
			// 成功尝试的上下文在事件流转发结束后才取消
			finishAttempt = cancelAttempt
			break
		}
		cancelAttempt(nil)
		// attemptErr 为本次尝试本身的错误（文件复制、附件上传或生成），不含随后删除临时 Drive 副本的错误：额度冷却与单模型 403
		// 只按它判断。清理错误照旧并入 err 写日志与返回；Drive 删除返回的 429、403 与模型额度、模型权限无关，一次非额度的生成失败
		// （连接重置、首事件超时等）不能因为清理时的 DriveDelete 429 被当成生成本身的裸限流逐级冷却、同步冷却另一通道
		attemptErr := err
		if attemptCopies != nil {
			err = errors.Join(err, attemptCopies.Cleanup())
		}
		trace.FinishAttempt(err)
		api.AddAccessLogAttempt(requestCtx, api.RequestAttempt{
			Account: accountLabel, Channel: string(lease.Channel()), Error: err.Error(),
			DurationMS: time.Since(attemptStartedAt).Milliseconds(),
		})
		workerFailed := service.workers.WorkerFailed(request.AccountID)
		waaRuntimeFailed := aistudio.DefinitiveWAARuntimeFailure(err)
		workerReplaced := errors.Is(err, errAccountWorkerReplaced)
		localWorkerFailure := (workerFailed || workerReplaced) && requestCtx.Err() == nil
		retryable := retryableGenerateAccountError(requestCtx, err) || localWorkerFailure
		if !inputTokenLimitError(err) {
			inputLimitErr = nil
		}
		recoverWorker := false
		// authMarked：本次生成的 401 已把账号标成需要登录。它的驻留 Worker 已不是任何模型的候选，租约释放后登记释放（见 worker_unschedulable.go）。
		// 只按本次尝试本身的错误（attemptErr）判断：随后删除临时 Drive 副本的 401（Bearer token 过期）不是登录失效的证据。
		// 上游 401 先经账户池的 gate：运行时只记“疑似登录失效”、写短冷却并异步复核（见 auth_recheck.go），复核仍是 401 才标记
		authMarked := false
		authFailure := aistudio.DefinitiveAuthenticationFailure(attemptErr)
		if authFailure {
			if marked, stateErr := lease.MarkAuthenticationFailure(attemptErr); stateErr != nil {
				err = errors.Join(err, stateErr)
				retryable = false
			} else {
				authMarked = marked
			}
			// 单个请求因登录失效最多换号 authFailureSwitchLimit 次（与额度换号分开计数）：一批账号同时 401（出口被拒、Cookie 批量作废）时
			// 不把号池逐个试一遍
			authFailures++
			if authFailures > authFailureSwitchLimit && retryable {
				retryable = false
				service.requests.logRequestProgress(request.ID, accountLabel, "WARN", fmt.Sprintf(
					"登录失效换号已达上限，不再换号 | 登录失效=%d/%d | 原因=%s",
					authFailures, authFailureSwitchLimit+1, progressReason(attemptErr),
				))
			}
		}
		cooled, switchedToBuild, inputLimitRetry := false, false, false
		// modelForbidden：本次尝试的单模型 403 已写入冷却。换号不退避，但仍计入连续失败上限：模型对整个号池都返回 403 时
		// （号池里没有付费账号、纯 Go proof 稳定 403 等）不会一个请求把账号逐个试完，逐个冷却、登记淘汰并现场冷启动
		modelForbidden := false
		// quotaFailure：本次尝试是计入换号上限的额度失败（只在 policy.counted 时计数，见上文 quota）；quotaKind 为写进进度的类型；
		// quotaSameAccount 为额度冷却后不换号、在同账号另一通道再试（进度写“同账号换通道重试”）；
		// bare 为生成本身返回的裸限流（已由 applyQuotaCooldown 交给出口判定器，见 rate_limit_guard.go）
		quotaFailure, quotaKind, quotaSameAccount, bare := false, "", false, false
		if cooldown, ok, stateErr := service.applyQuotaCooldown(lease, accountLabel, modelID, attemptErr, "", scope, true, generated); ok {
			cooled = true
			now := time.Now()
			bare = cooldown.Escalate && generated
			// 这里只读判定结果：限流类上限、文案与 Retry-After 按这次失败时账户所在出口的判定取
			var verdict egressVerdict
			if bare {
				verdict = service.egress.verdict(egressKey, egressUltra, now)
			}
			if policy.counted {
				class := policy.classOf(cooldown, now)
				quota.record(class, request.AccountID, err, cooldown.Until)
				if class == primaryQuotaRate {
					quota.egress, quota.egressKey = verdict, egressKey
				}
				quotaFailure, quotaKind = true, cooldown.Kind
			}
			if stateErr != nil {
				err = errors.Join(err, stateErr)
				retryable = false
			} else if !cooldown.Global && !verdict.Limited &&
				service.pool.AccountChannelAvailable(request.AccountID, channelFallback(selection)) {
				// 出口限流时同账号换通道同样经被限的出口，不再探测
				delete(attempted, request.AccountID)
				maxAttempts++
				quotaSameAccount = true
			}
		}
		// 租约账号在请求发出前进入冷却（多半是同账号别的请求刚遇到额度 429，请求没有发到上游）：任何模型都按已冷却处理，
		// 不计入连续失败上限、不退避，照常换号（计入每请求硬上限）。Ultra 主用模型只在拦下它的冷却剩余不少于 rotationMinRemaining
		// （多半是每日限额）时记一次每日类额度失败，短冷却的连带拦截不计数；普通号池不计入额度上限：请求没有发到上游，不消耗出口额度
		coolingDown := errors.Is(err, aistudio.ErrAccountCoolingDown)
		if coolingDown {
			cooled = true
			if failFast {
				if until, long := coolingDownLong(service.pool, err, request.AccountID, modelID, time.Now()); long {
					quota.record(primaryQuotaDaily, request.AccountID, err, until)
					quotaFailure, quotaKind = true, "发出前已冷却"
				}
			}
		}
		// 已发往上游、与出口有关的其他失败（5xx、首事件超时、带指标的限额、403 等）计入出口被拒比例的分母（见 egressOtherOutcome）
		if egressOtherOutcome(requestCtx, generated, coolingDown, bare, attemptErr) {
			service.egress.observeOutcome(egressObservation{
				Key: egressKey, AccountID: request.AccountID, Ultra: egressUltra, Outcome: egressOther, At: time.Now(),
			})
		}
		// 额度失败后下一次选号只用已启动的 Worker（普通号池只在限流类之后，见 quotaSwitchPolicy）；Ultra 主用模型短冷却的连带拦截不计数，
		// 此前已有额度失败时同样保持（否则选号失败时按额度不足结束的判断会漏掉）
		if quotaFailure && policy.warmAfter(quota.last) || policy.ultra && coolingDown && quota.hits() > 0 {
			warmOnlyNext = true
		}
		// 同一账号在短时间内对多个模型连续返回 403 无权限：自动暂停一段时间，不再参与调度
		if isPermissionDenied(err) {
			now := time.Now()
			if pause, count, models := service.forbidden.record(request.AccountID, modelID, now); pause {
				until := now.Add(forbiddenPause)
				reason := fmt.Sprintf("%d 分钟内 %d 个模型连续 %d "+forbiddenPauseReasonSuffix,
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
		// 单个模型返回 403 无权限（常见于免费账号调用付费模型）：对该账号该通道的这个模型写入冷却，时长按 24 小时内该通道的次数递增
		// （30 分钟、6 小时、到太平洋时间次日零点），该通道上该模型成功一次即从头计（见 forbidden_guard.go）。原先不冷却，账号一直留在
		// 候选里，空出的槽位又最多，几乎每个新请求的第一次尝试都落在它上面白耗一次。冷却后换号不退避，但仍计入连续失败上限，
		// 同一账号另一通道可用时在那里再试一次，驻留 Worker 因此成了死槽位时登记淘汰。只处理没有文件绑定、错误来自
		// 生成本身的尝试：文件复制与附件上传的 403 与模型权限无关；登录失效与 WAA 运行态失败另有处理；号池级 403 见下文
		modelDenied := isPermissionDenied(attemptErr) && resourceID == "" && generated && requestCtx.Err() == nil &&
			!authFailure && !waaRuntimeFailed
		// 号池级 403：号池内多个账号对该模型连续 403、这段时间又没有任何成功，多半是 proof、请求形态等号池一侧的问题，不是账号没有
		// 权限：不写冷却、不升档、不登记淘汰，按一般失败计入连续失败并退避（见 forbiddenSystemicAccounts）
		if modelDenied && service.forbidden.observeModelDenied(scope, request.AccountID, modelID, time.Now()) {
			modelDenied = false
			// 判定成立之前号池里的 403 已经登记为无权限账户：撤销号池里该模型自最近一次成功以来登记的全部条目（不只是判定记着的
			// 几个账号），免得修复后列表里留下一批正常账户
			service.dropSystemicPermissionDenied(scope, modelID, time.Now())
			service.requests.logRequestProgress(request.ID, accountLabel, "WARN", fmt.Sprintf(
				"号池内 %d 分钟内至少 %d 个账号对模型 %s 返回 403 且没有成功，按号池级 403 处理，不冷却账号",
				int(forbiddenWindow/time.Minute), forbiddenSystemicAccounts, modelID,
			))
		}
		if modelDenied {
			// 登记为 403 无权限账户，管理页单独列出（多半是没有同意 Google 的政策，重新获取认证文件上传后清除，见 permission_denied.go）
			service.recordPermissionDenied(request.AccountID, modelID, lease.Channel(), err.Error(), time.Now())
			cooldownScope := lease.CooldownScope(modelID)
			until, level := service.forbidden.recordModel(request.AccountID, cooldownScope, time.Now())
			reason := forbiddenModelReasonPrefix + err.Error()
			if stateErr := service.pool.MarkCooldownIfGeneration(
				request.AccountID, cooldownScope, lease.ModelAccessGeneration(), lease.CheckedAt(), until, reason,
			); stateErr != nil {
				err = errors.Join(err, stateErr)
				retryable = false
			} else {
				cooled, modelForbidden = true, true
				service.requests.log(accountLabel, "WARN", fmt.Sprintf(
					"账号冷却 | 类型=无权限 | 范围=%s | 恢复=%s | 档位=%d",
					cooldownScope, until.Format(time.RFC3339), level,
				))
				if service.pool.AccountChannelAvailable(request.AccountID, channelFallback(selection)) {
					delete(attempted, request.AccountID)
					maxAttempts++
				}
				service.retireCooledWorker(request.AccountID, modelID, aistudio.QuotaCooldown{Until: until, Kind: "无权限", Reason: reason})
			}
		}
		// 输入 token 超过上限（提示词过长）与账号无关：不冷却、不按一般失败换号，对外按 400 返回（上游有时按 429 返回这个错误，
		// 见 aistudio.InputTokenLimitError）。部分账号另有权益带来的输入上限（如 131072 token，见 InputTokenCapError）：
		// Playground 超限改走 Build 再试一次（同一账号也可以用），仍超限且号池里有更高权益的账号时再用它们试一次；两次再试都只接受
		// 现在就能取得的账号，不排队。带具体计数的模型本身上限任何账号都放不下，直接结束
		if inputTokenLimitError(err) {
			inputLimitErr = err
			capError := inputTokenCapError(err)
			var higherTier []string
			if capError && !tierRetried {
				for _, accountID := range service.pool.EnabledAccountsAboveTierOf(scope, request.AccountID) {
					if _, done := attempted[accountID]; !done {
						higherTier = append(higherTier, accountID)
					}
				}
			}
			switch {
			case capError && !buildOnly && !selection.PlaygroundOnly && lease.Channel() == aistudio.ChannelPlayground && service.pool.BuildEnabled():
				buildOnly, switchedToBuild, retryable, immediateRetry = true, true, true, true
				delete(attempted, request.AccountID)
				maxAttempts++
				trace.Note("Playground 返回输入 token 超过账号上限，改用 Build 通道再试一次")
				service.requests.logRequestProgress(request.ID, accountLabel, "WARN", "Playground 输入超过上限，改用 Build 通道重试 | 原因="+progressReason(err))
			case len(higherTier) > 0:
				tierRetried, inputLimitRetry, retryable, immediateRetry = true, true, true, true
				buildOnly = false
				tierRetryAccountIDs = higherTier
				maxAttempts++
				trace.Note(fmt.Sprintf("输入 token 超过账号上限，改用更高权益的账号再试一次 | 候选=%d", len(higherTier)))
				service.requests.logRequestProgress(request.ID, accountLabel, "WARN", fmt.Sprintf(
					"输入超过本账号上限，改用更高权益的账号再试一次 | 候选=%d | 原因=%s", len(higherTier), progressReason(err)))
			default:
				retryable = false
				trace.Note("输入 token 超过模型上限（提示词过长），不换号、不冷却，按 400 返回")
				service.requests.logRequestProgress(request.ID, accountLabel, "WARN", "输入超过模型上限（提示词过长），不换号、不冷却，按 400 返回 | 原因="+progressReason(err))
			}
		}
		// countsHard：这次失败换号时计入连续失败上限（hardFailures，见下文）。没等到关闭就换号时 recoverWorker 一定为 false，
		// 是否计入只看这几项
		countsHard := (!cooled || modelForbidden) && !switchedToBuild && !inputLimitRetry && !quotaFailure
		// 最近一次额度失败所在类别超过它的换号上限：不再换号（租约照常由下面的 recoverWorkerOnce 释放），随后按额度不足结束
		exhaustReason, exhausted := quota.exhaustedReason()
		quotaExhausted := quotaFailure && exhausted
		if quotaExhausted {
			retryable = false
		}
		// 租约由 recoverWorkerOnce 释放：本次失败要关闭这个 Worker 版本时原子地转成独占等待交给后台关闭（见 worker_reset.go），
		// 排队的请求不会在释放与登记之间落到坏掉的 Worker 上；其余情况照常释放，释放出错时结束。
		// 等待关闭的上限：请求已取消或不再重试时只登记后台关闭、不等待；不固定账号、还能换号的请求只等一小会儿（见 workerResetWaitFor），
		// 等不到就换号；固定账号与文件绑定的请求只能用这个账号，等关闭结束后在原账号重放。
		// 还能换号要看换号次数与连续失败上限：这次失败计入上限后就到上限的请求不会再换号，同样一直等关闭结束后在原账号重放。
		// 原先只看换号次数（不固定账号时为启用账号数，生产上几万），连续 5 个账号 Code 5 且都有其他请求在输出时每个只等 2 秒，
		// 第 5 次换号到上限按 Code 5 失败，既没换号也没等原账号恢复
		resetWait := workerResetWaitUnbounded
		if requestCtx.Err() != nil || !retryable {
			resetWait = 0
		} else if unbound && attempt+1 < maxAttempts && (!countsHard || hardFailures+1 < generateFailureLimit) &&
			len(tried) < generateSwitchCeiling {
			resetWait = service.workerResetWaitFor(budget)
		}
		recovered, _, resetDeferred, recoveryErr := service.recoverWorkerOnce(
			requestCtx, lease, request.AccountID, workerGeneration, recoveredWorker,
			needsWAARuntimeRecovery(err, false, workerFailed, workerReplaced),
			workerFailed || waaRuntimeFailed, resetWait,
		)
		lease = nil
		if authMarked {
			// 账号已需要登录：统一写“账号需要登录”，它的驻留 Worker 登记释放（空闲即关，忙碌等请求结束），槽位由预热补上
			service.noteAuthRequired(request.AccountID, accountLabel, authFromGeneration, err)
			service.workers.releaseUnschedulable(request.AccountID, unschedulableAuthRequired)
		}
		if recoveryErr != nil {
			err = errors.Join(err, recoveryErr)
			retryable = false
		}
		if !retryable {
			if quotaExhausted {
				meta.accounts = len(tried)
				err = service.finishQuotaFailure(request.ID, accountLabel, &quota, meta, exhaustReason, nil, err)
			}
			break
		}
		// 请求已取消时只登记了后台关闭，不重放也不换号（与原先不恢复相同）
		if requestCtx.Err() == nil {
			recoverWorker = recovered
		} else {
			resetDeferred = nil
		}
		if recoverWorker {
			recoveryAccountID = request.AccountID
			delete(attempted, request.AccountID)
			maxAttempts++
		}
		if resetDeferred != nil {
			// 没等到同账号关闭失败的 Worker：后台继续关闭，期间账号不接新请求，本请求换号。原账号重新记为已尝试：同一次失败的额度冷却、
			// 单模型 403 与改走 Build 可能刚把它移出；换号找不到可用账号时再回到这里等关闭结束（见上文选号失败的处理）
			attempted[request.AccountID] = struct{}{}
			deferredReset, deferredAccountID = resetDeferred, request.AccountID
			note := resetDeferred.deferralNote(resetWait)
			trace.Note(note + "，换号重试")
			service.requests.logRequestProgress(request.ID, accountLabel, "WARN", note+"，换号重试 | 原因="+progressReason(err))
		}
		// 不再换号（换号次数用完或到连续失败上限）时还有没等到关闭就换走的原账号：回到那里一直等关闭结束后重放，
		// 与换号没有可用账号时相同，不按这次的失败结束（原先只在换号没有可用账号时回去）
		if attempt+1 == maxAttempts {
			if quotaFailure && requestCtx.Err() == nil {
				// 最后一次尝试是额度失败、换号次数已用完：按额度不足返回 429（带 Retry-After），不回到原账号等待
				meta.accounts = len(tried)
				err = service.finishQuotaFailure(request.ID, accountLabel, &quota, meta, primaryQuotaReasonNoOther, nil, err)
				break
			}
			if replay, _ := returnToDeferred("换号次数已用完", err); replay {
				continue
			}
			break
		}
		// 每请求换号硬上限：已在 generateSwitchCeiling 个不同账号上尝试过，不再换号（同账号重建后重放不算换号）。还有没等到关闭就换走的
		// 原账号时先回到那里等关闭结束后重放；否则这次是额度失败时按额度不足结束（429，带 Retry-After），其余按这次的错误结束
		if !recoverWorker && (unbound || fileBound) && len(tried) >= generateSwitchCeiling {
			service.requests.logRequestProgress(request.ID, accountLabel, "WARN", fmt.Sprintf(
				"已尝试 %d 个账号，不再换号 | 原因=%s", len(tried), progressReason(err)))
			replay, ended := returnToDeferred(fmt.Sprintf("已尝试 %d 个账号", len(tried)), err)
			if replay {
				continue
			}
			if !ended && quotaFailure && requestCtx.Err() == nil {
				meta.accounts = len(tried)
				err = service.finishQuotaFailure(request.ID, accountLabel, &quota, meta, primaryQuotaReasonCeiling, nil, err)
			}
			break
		}
		if !recoverWorker && countsHard {
			hardFailures++
			if hardFailures >= generateFailureLimit {
				service.requests.logRequestProgress(request.ID, accountLabel, "WARN", fmt.Sprintf(
					"已连续 %d 次失败，不再换号 | 原因=%s", hardFailures, progressReason(err)))
				if replay, _ := returnToDeferred(fmt.Sprintf("已连续 %d 次失败", hardFailures), err); replay {
					continue
				}
				break
			}
			// 单模型 403 已冷却该账号，换号不退避；没等到 Worker 关闭就换号时已经等过，同样不退避；
			// 主用模型的排队截止时间已过时同样不退避：下一次尝试只接受立即可用的账号，退避只会推迟 429
			if !modelForbidden && resetDeferred == nil && !budget.expired() {
				if waitErr := waitRetryBackoff(requestCtx, hardFailures); waitErr != nil {
					break
				}
			}
		}
		// 出口正常时第 2 次起的限流类换号前短暂等待（错开并发请求的重试，见 quota_switch.go）：出口限速生效时令牌桶已错开，
		// 排队预算已过时不等待；走到这里已确定要换号（换号上限、换号次数、硬上限都已检查过）
		if !recoverWorker && !quotaSameAccount && quotaFailure && quota.last == primaryQuotaRate && quota.rate >= 2 &&
			policy.jitter && !quota.egress.Throttled && !budget.expired() {
			if waitErr := waitQuotaJitter(requestCtx, service.quotaJitterDelay(), budget); waitErr != nil {
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
		if quotaFailure {
			service.requests.logRequestProgress(request.ID, accountLabel, "INFO", primaryQuotaSwitchMessage(&quota, quotaKind, quotaSameAccount, err))
		} else {
			service.requests.logRequestProgress(request.ID, accountLabel, "WARN", "换号重试 | 原因="+progressReason(err))
		}
	}
	if err == nil && (lease == nil || source == nil) {
		// 兜底：循环结束却没有拿到可用的上游流时按号池暂时没有可用账号结束（503），不能把空租约交给 forwardEvents
		err = aistudio.PoolNotReadyIn(scope, fmt.Sprintf("模型 %s 换号结束时没有可用账号", modelID), nil)
	}
	// 首事件前期限到期：错误标明是本服务自己的期限（与上游的“协议错误码 4”、上游断流区分开），仍满足 context.DeadlineExceeded（对外 504）
	if cut, ok := phase.startDeadlineError(err, startCutWhere); ok {
		reason := progressReason(err)
		err = cut
		where := cut.where
		if where == "" {
			where = "-"
		}
		service.requests.logRequestProgress(request.ID, "service", "WARN", fmt.Sprintf(
			"本服务期限截断 | 阶段=首事件前 | 期限=%s | 已用时=%s | 停在=%s | 原因=%s",
			cut.limit, cut.elapsed.Round(time.Millisecond), where, reason))
		trace.Note("本服务首事件前期限到期: " + cut.Error())
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
	if finishAttempt != nil {
		finishAttempt(nil)
	}
}

// channelFallback 返回判断“同一账号能否换通道重试”用的选号条件：去掉 PlaygroundFirst（降级判定的通道偏好，
// Playground 没有可用账号时本来就会退回其他通道），保留 BuildOnly、PlaygroundOnly 等硬性限制。
// 原先带着 PlaygroundFirst 判断，Playground 冷却后同一账号的 Build 通道永远不会被尝试
func channelFallback(selection aistudio.AccountSelection) aistudio.AccountSelection {
	selection.PlaygroundFirst = false
	return selection
}

// applyQuotaCooldown 在上游返回额度 429 时冷却该账号的模型（或整个账号），并记录日志；
// 每日限额且该模型已判定两个通道共用额度时，同时冷却另一个通道，避免请求再去另一个通道失败一次。
// 没有额度周期信息、上游也没给恢复时间的非全局限流（裸 “Quota exceeded”，cooldown.Escalate）按连续次数逐级加长冷却
// （见 rate_limit_guard.go）：pool 为请求的号池（号池级限流判定的范围），escalate 为这次失败能否升档（已向客户端输出后为假），
// fromGeneration 为错误来自生成本身（不是文件复制、附件上传等发出生成请求之前的步骤）。
// ok 表示 err 是额度错误；stateErr 为写入冷却状态失败的原因。冷却没有写入（模型目录已更新，或同账号取得租约更晚的请求已写回结果）时
// 如实记一行 INFO，不登记淘汰、不做通道共用额度的学习，ok 仍为真：调用方照常按已冷却换号。
// 额度错误带着 GeminiApiErrorDetails、却没有解析出额度指标时，在冷却日志之后另记一行“额度 429 详情未识别”（见 quota_detail_log.go）
func (service *trackedService) applyQuotaCooldown(
	lease *aistudio.AccountLease, accountLabel string, modelID string, err error, note string,
	pool aistudio.PoolScope, escalate bool, fromGeneration bool,
) (cooldown aistudio.QuotaCooldown, ok bool, stateErr error) {
	cooldown, ok = aistudio.QuotaCooldownForError(err, time.Now())
	if !ok {
		return cooldown, false, nil
	}
	// 最先登记、最后执行：排在冷却日志（含另一通道的同步冷却与淘汰登记）之后，各个返回路径都会写
	defer service.logUnparsedQuotaDetail(accountLabel, err)
	// 只有生成本身返回的裸限流逐级冷却：文件复制、附件上传（Drive）返回的 429 与模型额度无关（同 403 只处理生成本身的错误），
	// 照原路径只冷却本通道 30 秒，不建档位、不同步另一通道、不计入号池级限流，之后照常在同账号另一通道再试
	if cooldown.Escalate && fromGeneration {
		// IP 池按端口统计生成本身的裸限流 429（不同账号在同一端口上都被限流时冷却端口，见 proxy_pool.go）。在写完这次的逐级冷却之后再记：
		// 端口因此冷却时，回调撤回的逐级冷却包括这次刚写的（见 proxy_pool_egress.go）。规则 A 要的是这次之前的坏号状态：
		// applyRateLimitCooldown 先记坏号记忆，第 2 次裸限流当场标上坏号
		accountID := lease.Account().ID
		wasBad := service.workers.badAccountMemory().bad(accountID)
		cooldown, ok, stateErr = service.applyRateLimitCooldown(lease, accountLabel, modelID, cooldown, note, pool, escalate)
		service.workers.ipPool().rateLimited(accountID, wasBad)
		return cooldown, ok, stateErr
	}
	accountID := lease.Account().ID
	modelAccessScope := lease.CooldownScope(modelID)
	scopeLabel := modelAccessScope
	if cooldown.Global {
		modelAccessScope = ""
		scopeLabel = "全局"
	}
	// 不缩短已有的冷却：同账号并发请求刚写入的更长冷却（例如连续限流第 3 档的 30 分钟，或分钟限额、每日限额）仍生效且更晚时保留它，
	// 只推进检查时间（CooldownKeptLater）。原先无条件覆盖，租约稍晚的附件上传 429（只冷却本通道 30 秒）会把第 3 档缩短成 30 秒，
	// 驻留 Worker 的淘汰登记随之失去理由被丢掉，30 秒后账号又被选中白白失败一次
	write, effectiveUntil, stateErr := service.pool.TryExtendCooldownIfGeneration(
		accountID, modelAccessScope, lease.ModelAccessGeneration(), lease.CheckedAt(),
		cooldown.Until, cooldown.Reason,
	)
	if stateErr != nil {
		return cooldown, true, stateErr
	}
	suffix := ""
	if note != "" {
		suffix = " | " + note
	}
	if write != aistudio.CooldownWritten {
		// 保留了更晚的冷却时写它的恢复时间（实际生效的），不登记淘汰：原冷却写入时已经判断过
		recoverAt := cooldown.Until
		if write == aistudio.CooldownKeptLater {
			recoverAt = effectiveUntil
		}
		service.requests.log(accountLabel, "INFO", fmt.Sprintf(
			"账号冷却未写入 | 类型=%s | 范围=%s | 恢复=%s%s | 原因=%s%s",
			cooldown.Kind, scopeLabel, recoverAt.Format(time.RFC3339), quotaCooldownFields(cooldown), write.String(), suffix,
		))
		return cooldown, true, nil
	}
	// 账号进入长时间冷却后它的驻留 Worker 成了死槽位：空闲时立即淘汰，忙碌时等请求释放租约后淘汰（见 worker_retire.go）；
	// 放在函数结束时判断，按模型的冷却要等另一通道同步冷却后才知道是否所有启用通道都已冷却
	defer service.retireCooledWorker(accountID, modelID, cooldown)
	service.requests.log(accountLabel, "WARN", fmt.Sprintf(
		"账号冷却 | 类型=%s | 范围=%s | 恢复=%s%s%s",
		cooldown.Kind, scopeLabel, cooldown.Until.Format(time.RFC3339), quotaCooldownFields(cooldown), suffix,
	))
	if cooldown.Global || cooldown.Kind != dailyQuotaKind {
		return cooldown, true, nil
	}
	// 通道共用额度的判定按账户真实权益分开学习（Ultra 权益单独一组），不随 ULTRA_INCLUDE_PRO 变化：
	// 并入 Ultra 号池的 Pro 账户额度与 Ultra 不同，证据混进 Ultra 的判定后关掉开关也不会消失
	other, coolOther := service.quota.dailyLimitHit(accountID, modelID, service.pool.TierPoolOf(accountID), lease.Channel(), lease.CheckedAt(), time.Now())
	if !coolOther {
		return cooldown, true, nil
	}
	otherScope := aistudio.ChannelCooldownScope(other, modelID)
	if otherWrite, otherErr := service.pool.TryMarkCooldownIfGeneration(
		accountID, otherScope, lease.ModelAccessGeneration(), lease.CheckedAt(),
		cooldown.Until, "每日限额（与另一通道共用额度，同步冷却）: "+strings.TrimPrefix(cooldown.Reason, dailyQuotaKind+": "),
	); otherErr == nil && otherWrite == aistudio.CooldownWritten {
		service.requests.log(accountLabel, "WARN", fmt.Sprintf(
			"账号冷却 | 类型=%s | 范围=%s | 恢复=%s | 与另一通道共用额度，同步冷却",
			cooldown.Kind, otherScope, cooldown.Until.Format(time.RFC3339),
		))
	}
	return cooldown, true, nil
}

// quotaCooldownFields 为冷却日志补记上游给出的限额值、指标与额度计数的模型（核实各档位、各模型的真实上限）；没有时为空。
// 没有指标时写出状态 details 的类型（只用于排查裸 429 带了哪些没有解析的详情），没有 details 时不写
func quotaCooldownFields(cooldown aistudio.QuotaCooldown) string {
	fields := ""
	if cooldown.LimitValue != "" {
		fields += " | 上限=" + cooldown.LimitValue
	}
	if cooldown.Metric != "" {
		fields += " | 指标=" + cooldown.Metric
	}
	if cooldown.Model != "" {
		fields += " | 模型=" + cooldown.Model
	}
	if cooldown.Metric == "" && cooldown.DetailTypes != "" {
		fields += " | 详情类型=" + cooldown.DetailTypes
	}
	return fields
}

// coolingDownLong 判断租约账号在发出前被拦下的冷却是否为长时间冷却（剩余不少于 rotationMinRemaining），并返回它的恢复时间：
// AccountCoolingDownError 按其恢复时间；不带恢复时间的裸哨兵（测试替身）按 AccountLongCooling 判断，恢复时间为零值
func coolingDownLong(pool *aistudio.AccountPool, err error, accountID string, modelID string, now time.Time) (time.Time, bool) {
	var blocked *aistudio.AccountCoolingDownError
	if errors.As(err, &blocked) && !blocked.Until.IsZero() {
		return blocked.Until, blocked.Until.Sub(now) >= rotationMinRemaining
	}
	return time.Time{}, pool.AccountLongCooling(accountID, modelID, rotationMinRemaining)
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

// attemptDrainTimeout 为首事件超时取消尝试后等待上游事件流结束的上限
const attemptDrainTimeout = 5 * time.Second

// firstEventTimeoutError 表示本次尝试在首事件超时内没有收到任何上游事件。按上游超时处理：
// 满足 context.DeadlineExceeded（换号重试；不能再换号时对外 504），不额外冷却或标记账号
type firstEventTimeoutError struct {
	timeout time.Duration
}

func (e *firstEventTimeoutError) Error() string {
	return fmt.Sprintf("上游首事件超时：%s 内没有收到上游事件", e.timeout)
}

func (e *firstEventTimeoutError) Unwrap() error {
	return context.DeadlineExceeded
}

// firstEventDeadline 为一次尝试的首事件超时计时（FIRST_EVENT_TIMEOUT，0 为关闭）：开始向上游发送时启动，
// 收到首事件或尝试结束时停止；到时只取消本次尝试的上下文
type firstEventDeadline struct {
	timeout time.Duration
	cancel  context.CancelCauseFunc
	mu      sync.Mutex
	timer   *time.Timer
	stopped bool
	fired   *firstEventTimeoutError
}

func newFirstEventDeadline(timeout time.Duration, cancel context.CancelCauseFunc) *firstEventDeadline {
	return &firstEventDeadline{timeout: timeout, cancel: cancel}
}

// start 开始计时；未启用、已开始或已停止时不做任何事
func (deadline *firstEventDeadline) start() {
	deadline.mu.Lock()
	defer deadline.mu.Unlock()
	if deadline.timeout <= 0 || deadline.timer != nil || deadline.stopped {
		return
	}
	deadline.timer = time.AfterFunc(deadline.timeout, deadline.expire)
}

func (deadline *firstEventDeadline) expire() {
	deadline.mu.Lock()
	if deadline.stopped {
		deadline.mu.Unlock()
		return
	}
	deadline.stopped = true
	deadline.fired = &firstEventTimeoutError{timeout: deadline.timeout}
	err := deadline.fired
	deadline.mu.Unlock()
	deadline.cancel(err)
}

// stop 停止计时，返回已经触发的超时错误（没有触发时为 nil）。停止与触发互斥：stop 返回 nil 后本次尝试不会再被取消
func (deadline *firstEventDeadline) stop() error {
	deadline.mu.Lock()
	defer deadline.mu.Unlock()
	deadline.stopped = true
	if deadline.timer != nil {
		deadline.timer.Stop()
	}
	if deadline.fired == nil {
		return nil
	}
	return deadline.fired
}

// drainAttemptEvents 读完已取消尝试的事件流，确认上游 goroutine 已经退出；超过 limit 仍未结束时返回 false
func drainAttemptEvents(source <-chan aistudio.Event, limit time.Duration) bool {
	if source == nil {
		return true
	}
	timer := time.NewTimer(limit)
	defer timer.Stop()
	for {
		select {
		case _, ok := <-source:
			if !ok {
				return true
			}
		case <-timer.C:
			return false
		}
	}
}

// startCutStage 返回首事件前期限到期时本次尝试停在哪一步（写进最终错误与进度日志）：streamOpened 为 Generate 已返回事件流，
// sameTime 为首事件与期限同时到达（首事件已丢弃）
func startCutStage(generated, streamOpened, sameTime bool, timing *requestPreparationTiming) string {
	switch {
	case sameTime:
		return "首事件与期限同时到达（已丢弃，未向客户端输出）"
	case !generated:
		return "文件复制或附件上传"
	case streamOpened:
		return "等待上游首事件"
	}
	current, _, _ := timing.snapshot(time.Now())
	if current == "流已建立" {
		return "等待上游首事件"
	}
	return current
}

var errStreamClosedBeforeFirstEvent = errors.New("AI Studio stream closed before first event")
var errStreamClosedBeforeFinish = errors.New("AI Studio stream closed before finish")

// firstGenerateEvent 等待首事件并在请求期限内完成错误流清理；onEvent 在收到第一个上游事件（含错误事件）时调用，
// 用于停止首事件超时，之后等待错误流结束的时间不再计入
func firstGenerateEvent(ctx context.Context, source <-chan aistudio.Event, onWait func(), onEvent func()) (aistudio.Event, error) {
	timer := time.NewTimer(streamStallThreshold)
	defer timer.Stop()
	wait := timer.C
	for {
		select {
		case event, ok := <-source:
			if !ok {
				// 本次尝试的上下文已结束时上游不再发错误事件、直接关闭事件流（aistudio/generate.go），与 ctx.Done 同时就绪时
				// select 可能先读到关闭：按 ctx 的错误返回（首事件超时、本服务期限、请求取消照常识别），不当成上游在首事件前断流
				if err := ctx.Err(); err != nil {
					return aistudio.Event{}, err
				}
				return aistudio.Event{}, errStreamClosedBeforeFirstEvent
			}
			if onEvent != nil {
				onEvent()
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

// inputTokenLimitError 判断上游返回的"输入 token 超过上限"（提示词过长）错误，400 与 429 两种写法都识别
func inputTokenLimitError(err error) bool {
	return aistudio.InputTokenLimitError(err)
}

// inputTokenCapError 判断是否为部分账号的输入上限错误（换通道或换更高权益账号可能放得下），见 aistudio.InputTokenCapError
func inputTokenCapError(err error) bool {
	return aistudio.InputTokenCapError(err)
}

func retryableGenerateAccountError(ctx context.Context, err error) bool {
	if errors.Is(err, errStreamClosedBeforeFirstEvent) || errors.Is(err, aistudio.ErrAccountCoolingDown) {
		return true
	}
	// 输入 token 超过上限与账号无关：即使上游按 429 返回也不换号（Playground 超限改走 Build 的那一次重试另行处理）
	if inputTokenLimitError(err) {
		return false
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

// generateRequestContext 为生成请求建立上下文（见 generate_context.go）：首事件前期限与首事件后空闲期限按此刻的 REQUEST_TIMEOUT，
// 总时长上限按 generateMaxDurationFor（maxDuration 大于 0 时用它，测试调低）；生成服务停止时随之取消。
// 其他数据请求（CountTokens、视频、转写）仍用 dataRequestContext，整段受 REQUEST_TIMEOUT 限制
func (service *trackedService) generateRequestContext(ctx context.Context) (context.Context, context.CancelFunc, error) {
	service.lifecycleMu.Lock()
	if service.state.Load() != serviceRunning || service.dataContext == nil {
		service.lifecycleMu.Unlock()
		return nil, nil, &serviceStoppedError{}
	}
	timeout := time.Duration(service.timeout.Load())
	maxDuration := generateMaxDurationFor(timeout)
	if limit := time.Duration(service.maxDuration.Load()); limit > 0 {
		maxDuration = max(limit, timeout)
	}
	requestCtx := newGenerateContext(ctx, time.Now(), timeout, maxDuration)
	stopData := context.AfterFunc(service.dataContext, requestCtx.cancel)
	service.lifecycleMu.Unlock()
	return requestCtx, func() {
		stopData()
		requestCtx.cancel()
	}, nil
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
	// phase 为生成请求的上下文（见 generate_context.go）；测试直接传普通 context 时为 nil，不检查空闲
	phase := generateContextFrom(requestCtx)
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
	// 首事件后空闲期限（见 generate_context.go）：上游连续 idleLimit 没有事件、也没有网络字节时以本服务期限错误结束；
	// 向下游交付被阻塞的时间不算上游空闲（只在接收处检查，交付完成后重新计时；下游一直不读由总时长上限兜底）。
	// 计时器到时才复查实际空闲时间，不在每个事件上重设
	idleLimit := phase.idleTimeout()
	lastProgress := time.Now()
	var idleTimer *time.Timer
	var idle <-chan time.Time
	if idleLimit > 0 {
		idleTimer = time.NewTimer(idleLimit)
		defer idleTimer.Stop()
		idle = idleTimer.C
	}
	// publishResult 把网络字节、用量与工具调用数写进访问日志，每次把错误事件交给下游之前调用：API 层收到错误事件就结束请求并记录
	// 访问日志（requests.db、request.finished），不等事件流关闭；截断后还要读完事件流（最长 attemptDrainTimeout）时只在 defer 里写会丢失。
	// defer 里的同样写入保留，作为正常结束与终态路径的兜底（重复写为覆盖）
	publishResult := func() {
		api.SetAccessLogUpstreamBytes(requestCtx, activity.bytes.Load())
		api.SetAccessLogGenerationResult(requestCtx, usage, toolCalls)
	}
	finishFromContext := func() {
		requestErr = requestCtx.Err()
		if cause := phase.deadlineCause(); cause != nil {
			// 本服务的空闲期限或总时长上限：错误写明是哪一段与已输出的事件数，仍满足 context.DeadlineExceeded（对外 504）
			cut := cause.withOutput(reasoningEvents, contentEvents)
			requestErr = cut
			if !terminal {
				now := time.Now()
				service.requests.logRequestProgress(requestID, accountLabel, "WARN", fmt.Sprintf(
					"本服务期限截断 | 阶段=%s | 期限=%s | 已用时=%s | 最近事件=%s | 推理=%d | 正文=%d | %s",
					cut.stage.label(), cut.limit, now.Sub(phase.startedAt).Round(time.Millisecond),
					lastEventKind, reasoningEvents, contentEvents, activity.logFields(now)))
				trace.Note("本服务期限截断: " + cut.Error())
			}
		}
		state = finalRequestState(requestErr)
		if requestErr == nil || terminal || clientCtx.Err() != nil {
			return
		}
		publishResult()
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
			case <-idle:
				now := time.Now()
				if remaining := idleLimit - activity.quietFor(now, lastProgress); remaining > 0 {
					idleTimer.Reset(remaining)
					continue
				}
				idle = nil
				expired := phase.expire(&serviceDeadlineError{
					stage: serviceDeadlineIdle, limit: idleLimit, elapsed: now.Sub(phase.startedAt),
				})
				if expired && terminal {
					// 终态已经交给下游：只结束上游与租约，结果仍按终态记录（不把已完整的回复记成失败）
					service.requests.logRequestProgress(requestID, accountLabel, "WARN", fmt.Sprintf(
						"终态后上游事件流 %s 内没有结束，结束转发 | 模型=%s | %s", idleLimit, modelID, activity.logFields(now)))
				} else {
					// 本服务空闲期限到期（已被其他原因结束时按那个原因）：往流里写错误后结束
					finishFromContext()
				}
				// 读完事件流确认上游 goroutine 退出后再返回，defer 随后释放租约（与首事件前取消尝试后的做法相同）
				if !drainAttemptEvents(source, attemptDrainTimeout) {
					service.requests.logRequestProgress(requestID, accountLabel, "WARN", "本服务期限截断后上游事件流未及时结束")
				}
				return
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
				publishResult()
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
		lastProgress = now
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
				// 上游 401 先经账户池的 gate（疑似登录失效、复核后才标记，见 auth_recheck.go）；没有标记时不释放 Worker、不写“账号需要登录”
				if marked, stateErr := lease.MarkAuthenticationFailure(event.Err); stateErr != nil {
					requestErr = errors.Join(requestErr, stateErr)
					event.Err = requestErr
				} else if marked {
					// 流内确认需要登录：驻留 Worker 不再是任何模型的候选，登记释放（这次请求释放租约后关闭），写“账号需要登录”
					service.workers.releaseUnschedulable(lease.Account().ID, unschedulableAuthRequired)
					service.noteAuthRequired(lease.Account().ID, accountLabel, authFromStream, event.Err)
				}
			} else if _, ok, stateErr := service.applyQuotaCooldown(
				lease, accountLabel, requestedModelID, event.Err, "已输出内容后", aistudio.PoolScopeFromContext(requestCtx), false, true,
			); ok && stateErr != nil {
				// 已经开始输出后才出现的 429 无法换号重试，但仍写回冷却，后续请求不再选中这个账号或模型；
				// 这次尝试已经拿到上游输出，裸限流不作为“冷却到期后仍失败”的证据，不升档
				requestErr = errors.Join(requestErr, stateErr)
				event.Err = requestErr
			}
			state = finalRequestState(event.Err)
			terminal = true
		}
		if event.Kind == aistudio.EventFinish {
			api.SetAccessLogFinishReason(requestCtx, event.FinishReason)
			if event.FinishMessage != "" {
				// 上游对结束原因的说明（如 MALFORMED_FUNCTION_CALL 出错的调用）写进这次请求的管理日志，排查时不用再抓原始响应
				level, message := upstreamFinishLog(event.FinishReason, event.FinishMessage)
				service.requests.logRequestProgress(requestID, accountLabel, level, message)
			}
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
		if event.Kind == aistudio.EventError {
			// 上游错误之后事件流可能迟迟不关闭：先写访问日志的结果字段，再交给下游
			publishResult()
		}
		select {
		case destination <- event:
			lastProgress = time.Now()
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
// 预热循环与状态接口频繁调用，诊断中占了约两成的账户池锁时间；预热目标本身按分钟变化，不需要实时。
// 缓存同时记下计算前读到的号池规则版本，ULTRA_INCLUDE_PRO 切换后版本不同即重算：UltraAvailable 按号池成员计数，
// 沿用旧值会让 Ultra 分区的预热目标在缓存期内仍按旧成员计算。先读版本再计算，切换前开始、切换后才写回的旧结果
// 带的是旧版本，下一次调用照样重算，不会再被用满一个缓存期
func (manager *accountWorkerManager) bootstrapSummary() aistudio.BootstrapSummary {
	version := manager.pool.PoolRuleVersion()
	manager.bootstrapMu.Lock()
	cached, at, cachedVersion := manager.bootstrapCache, manager.bootstrapAt, manager.bootstrapVersion
	manager.bootstrapMu.Unlock()
	if !at.IsZero() && cached.Available > bootstrapCacheMinAccounts && time.Since(at) < bootstrapSummaryTTL && cachedVersion == version {
		return cached
	}
	summary := manager.pool.BootstrapSummary()
	manager.bootstrapMu.Lock()
	manager.bootstrapCache, manager.bootstrapAt, manager.bootstrapVersion = summary, time.Now(), version
	manager.bootstrapMu.Unlock()
	return summary
}
