package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/api"
	"github.com/Mag1cFall/AIStudio2API/internal/camoufoxnative"
	"github.com/Mag1cFall/AIStudio2API/internal/chromeauth"
	"github.com/Mag1cFall/AIStudio2API/internal/config"
)

// runtimeAdmin 投影运行时权威状态
type runtimeAdmin struct {
	lifecycle  context.Context
	pool       *aistudio.AccountPool
	store      *aistudio.AccountStore
	service    *trackedService
	requests   *requestRegistry
	login      aistudio.IsolatedLoginDriver
	workers    *accountWorkerManager
	headers    *accountHeaderProvider
	configPath string
	config     config.Config
	// onboard 为新账户自动处理队列
	onboard *accountOnboarder
	// catalogSlots 为启用后在后台同步模型目录的共用并发名额（见 bulk_enable.go），首次使用时创建
	catalogSlotsOnce sync.Once
	catalogSlots     chan struct{}
	// operationWait 大于 0 时为管理端账户操作等待独占租约的上限（见 accountOperationWait），只给包内测试调低
	operationWait time.Duration
}

// requestRegistry 保存活动请求与事件订阅
type requestRegistry struct {
	mu          sync.Mutex
	active      map[string]trackedRequest
	logs        []api.AdminLog
	subscribers map[*eventSubscriber]struct{}
	console     chan api.AdminLog
	// consoleDurable 为 logDurable 的独立队列：写入协程优先写出，不和普通日志抢 console 的名额
	consoleDurable chan api.AdminLog
	// consoleDropped 为队列满时丢弃的日志条数，由 writeConsole 定期汇总记一行（见 consoleDropReportInterval）
	consoleDropped atomic.Int64
	// consoleStopped 在生命周期 ctx 结束、写入协程开始收尾时置位（持有 mu），之后的日志在调用方协程直接写运行日志
	consoleStopped atomic.Bool
	// consoleDone 在写入协程收尾完成（写完两个队列与丢弃汇总）后关闭，见 waitConsole
	consoleDone chan struct{}
	// consoleHandler 为写运行日志的处理器，nil 时用 slog 默认处理器（只给包内测试注入）
	consoleHandler slog.Handler
	// dropReportInterval 大于 0 时替代 consoleDropReportInterval（只给包内测试调低）
	dropReportInterval time.Duration
}

type eventSubscriber struct {
	ctx             context.Context
	mu              sync.Mutex
	events          chan api.AdminEvent
	wake            chan struct{}
	pending         []api.AdminEvent
	pendingLogs     int
	pendingRequests int
}

const (
	adminLogRetain             = 1500
	adminLogCompactAt          = 1700
	adminLogInitialEvents      = 200
	adminRequestRetain         = 512
	adminRequestCompactAt      = 640
	adminRequestRefreshLatency = 50 * time.Millisecond
	// consoleQueueSize 为写运行日志的控制台队列长度：号池繁忙时冷却、换号与排队日志成批出现，队列满时新日志直接丢弃
	// （管理页面只保留最近 adminLogRetain 条）
	consoleQueueSize = 4096
	// consoleDurableQueueSize 为 logDurable 独立队列的长度：走这个队列的日志很少出现或已限频，写入协程优先写出
	consoleDurableQueueSize = 256
	// consoleFlushWait 为进程退出前等写入协程收尾的上限
	consoleFlushWait = 5 * time.Second
	// consoleDropReportInterval 为检查并汇总“控制台队列已满”的周期
	consoleDropReportInterval = 10 * time.Second
)

type trackedRequest struct {
	request api.AdminRequest
	cancel  context.CancelFunc
}

type adminOperationError struct {
	status  int
	code    string
	message string
}

func (err *adminOperationError) Error() string {
	return err.message
}

func (err *adminOperationError) HTTPStatus() int {
	return err.status
}

func (err *adminOperationError) ErrorCode() string {
	return err.code
}

// newRuntimeAdmin 创建管理端服务
func newRuntimeAdmin(
	lifecycle context.Context,
	pool *aistudio.AccountPool,
	store *aistudio.AccountStore,
	service *trackedService,
	registry *requestRegistry,
	login aistudio.IsolatedLoginDriver,
	workers *accountWorkerManager,
	headers *accountHeaderProvider,
	cfg config.Config,
) *runtimeAdmin {
	return &runtimeAdmin{
		lifecycle: lifecycle, pool: pool, store: store, service: service, requests: registry, login: login,
		workers: workers, headers: headers,
		configPath: ".env", config: cfg,
	}
}

// newRequestRegistry 创建活动请求注册表
func newRequestRegistry(ctx context.Context) *requestRegistry {
	registry := &requestRegistry{
		active:      make(map[string]trackedRequest),
		logs:        make([]api.AdminLog, 0, 128),
		subscribers: make(map[*eventSubscriber]struct{}),
		console:     make(chan api.AdminLog, consoleQueueSize),
		// 独立队列：普通日志占满 console 时，logDurable 的日志仍能排进来
		consoleDurable: make(chan api.AdminLog, consoleDurableQueueSize),
		consoleDone:    make(chan struct{}),
	}
	go registry.writeConsole(ctx)
	return registry
}

func (admin *runtimeAdmin) Status(context.Context) (api.AdminStatus, error) {
	// 只统计数量，不构建上千个账户的完整状态，避免长时间占用账户池锁；同一次加锁同时统计 Ultra 号池
	stateCounts, ultraCounts := admin.pool.PoolStateCounts()
	counts := adminAccountCounts(stateCounts)
	state := admin.service.State()
	running := state == "RUNNING"
	warm := admin.workers.WarmAccountIDs()
	sort.Strings(warm)
	starting := admin.workers.OpeningAccountIDs()
	// Worker 计数按分区：workers 的数量与上限只含普通分区，ultra_workers 为 Ultra 分区；Worker 列表包含两个分区
	ultra := admin.pool.UltraAccountIDs()
	normalOccupied, ultraOccupied := admin.workers.occupiedSlotsApprox(ultra)
	// 死槽位：驻留 Worker 的账号长时间冷却（见 worker_retire.go），占着名额却服务不了请求，等待淘汰
	normalDead, ultraDead := admin.workers.deadWarmCounts(warm, ultra)
	// 排空中（见 worker_drain.go）与待淘汰登记的数量
	normalDraining, ultraDraining := admin.workers.drainCounts()
	normalRetiring, ultraRetiring := admin.workers.retirementCounts(ultra)
	// 不可调度：驻留 Worker 的账号需要登录、已停用、没有模型目录（见 worker_unschedulable.go），等待释放；口径与死槽位分开
	normalUnschedulable, ultraUnschedulable := admin.workers.unschedulableWarmCounts(warm, ultra)
	ultraWorkers := admin.workers.ultraWorkerCounts(ultra, warm, starting, ultraOccupied, ultraDead)
	ultraWorkers.Draining, ultraWorkers.Retiring = ultraDraining, ultraRetiring
	ultraWorkers.Unschedulable = ultraUnschedulable
	// 主用模型供给读 keepWarm 的缓存（见 primary_supply.go），不在状态请求里对账户分类；服务没有运行时不显示
	var primarySupply []api.AdminPrimarySupply
	var catalogSync *api.AdminCatalogSync
	// 出口限流判定与限速（见 egress_guard.go adminStatus）：出口不多于 16 个时全部列出，更多时（IP 池开启后多达几百个）只列出
	// 判定成立或已撤销的出口、正常的只计入汇总；服务没有运行时不显示
	var egress []api.AdminEgress
	var egressSummary *api.AdminEgressSummary
	// 登录复核（见 auth_recheck.go）：疑似登录失效与需要登录账户的复核，服务没有运行时不显示
	var authRecheck *api.AdminAuthRecheck
	// Build 通道生成的暂停状态（见 build_pause.go）：启用了 Build 通道时才显示
	var buildChannel *api.AdminBuildChannel
	if running {
		primarySupply = admin.workers.primarySupplyStatus()
		catalogSync = admin.service.catalogSync.status()
		egress, egressSummary = admin.service.egress.adminStatus()
		authRecheck = admin.service.authRecheck.status()
		if admin.pool.BuildEnabled() {
			buildChannel = admin.service.buildPauses().status()
		}
	}
	// 启用账户的 Drive 授权状态（见 aistudio.PooledService.DriveSummary）：gemini-omni 的视频输入（及无法转为 PNG 的图片）要交给有 Drive 授权的账户
	var drive *api.AdminDriveCounts
	if source, ok := admin.service.service.(driveSummarySource); ok {
		summary := source.DriveSummary()
		drive = &api.AdminDriveCounts{Available: summary.Available, Unavailable: summary.Unavailable, Unknown: summary.Unknown}
	}
	// 最近 10 分钟新被标为需要登录的账户数，放在全部账户的计数里（持续增长说明运行期 401 在累积）
	counts.AuthRequired10m = admin.service.authMarks.count(time.Now(), authRecentWindow)
	normal := aistudio.PoolScopeNormal
	return api.AdminStatus{
		State:          state,
		Running:        running,
		Ready:          running && counts.Ready+counts.Busy > 0,
		Version:        buildVersion(),
		ActiveRequests: admin.requests.count(),
		Accounts:       counts,
		UltraAccounts:  adminAccountCounts(ultraCounts),
		Workers: api.AdminWorkerCounts{
			Warm: len(inPartition(warm, ultra, normal)), Starting: len(inPartition(starting, ultra, normal)),
			Target: admin.workers.prewarmTargetFor(normal), Max: admin.workers.maxActiveFor(normal),
			Occupied: normalOccupied, Dead: normalDead, Draining: normalDraining, Retiring: normalRetiring,
			Prewarm: admin.workers.prewarmState(),
			WarmIDs: warm, StartingIDs: starting,
			Unschedulable: normalUnschedulable,
		},
		UltraWorkers:  ultraWorkers,
		PrimarySupply: primarySupply,
		CatalogSync:   catalogSync,
		Egress:        egress,
		EgressSummary: egressSummary,
		AuthRecheck:   authRecheck,
		BadAccounts:   admin.workers.badAccountMemory().count(),
		ProxyPool:     admin.ipPool().status(),
		BuildChannel:  buildChannel,
		Drive:         drive,
	}, nil
}

// adminAccountCounts 投影账户状态计数
func adminAccountCounts(counts aistudio.AccountStateCounts) api.AdminAccountCounts {
	return api.AdminAccountCounts{
		Total: counts.Total, Ready: counts.Ready, Busy: counts.Busy,
		Cooldown: counts.Cooldown, AuthRequired: counts.AuthRequired, OrgDisabled: counts.OrgDisabled,
	}
}

func (admin *runtimeAdmin) Accounts(context.Context) ([]api.AdminAccount, error) {
	statuses := admin.pool.Status()
	accounts := make([]api.AdminAccount, 0, len(statuses))
	for _, status := range statuses {
		accounts = append(accounts, adminAccountDTO(status))
	}
	admin.ipPool().markPinned(accounts)
	return accounts, nil
}

// normalizeAccountProxy 规范化管理接口提交的账户代理（见 config.NormalizeProxy），出错按 400 invalid_account 返回。
// 错误加“账户 proxy 无效: ”前缀，与 AccountConfig.Validate 一致，避免在账户对话框里误以为是全局 PROXY；前缀与文案都是固定文本
func normalizeAccountProxy(value string) (string, error) {
	proxy, err := config.NormalizeProxy(value)
	if err != nil {
		return "", invalidAccount(fmt.Errorf("账户 proxy 无效: %w", err))
	}
	return proxy, nil
}

func (admin *runtimeAdmin) CreateAccount(ctx context.Context, input api.AccountCreateInput) (api.AdminAccount, error) {
	accountProxy, err := normalizeAccountProxy(input.Proxy)
	if err != nil {
		return api.AdminAccount{}, err
	}
	accountConfig := aistudio.DefaultAccountConfig("")
	accountConfig.Proxy = accountProxy
	if locale := strings.TrimSpace(input.Locale); locale != "" {
		accountConfig.Locale = locale
	}
	if timezone := strings.TrimSpace(input.Timezone); timezone != "" {
		accountConfig.Timezone = timezone
	}
	directory, err := os.MkdirTemp("", "aistudio2api-account-login-*")
	if err != nil {
		return api.AdminAccount{}, fmt.Errorf("创建隔离登录目录: %w", err)
	}
	defer os.RemoveAll(directory)
	startedAt := time.Now()
	admin.requests.log("auth", "INFO", "账户添加 | 1/2 | 等待隔离登录")
	// 登录浏览器同样占一个 IP 池出口：新账户还没有 ID，用临时键；登录成功后记到账户名下（adopt），之后的 Worker 回到这个 IP
	loginProxy, poolLease := admin.browserProxy(newLoginLeaseKey(), "", proxyPoolKindLogin, "auth", accountConfig.Proxy)
	defer poolLease.release()
	result, err := admin.login.Login(ctx, aistudio.IsolatedLoginRequest{
		AccountID: "new", Directory: directory, Proxy: loginProxy,
		Locale: accountConfig.Locale, Timezone: accountConfig.Timezone,
		OnboardingCompleted: admin.onboardingLogger("auth", "账户添加"),
	})
	if err != nil {
		admin.requests.log("auth", "ERROR", fmt.Sprintf(
			"账户添加失败 | 耗时=%s | 错误=%s",
			time.Since(startedAt).Round(time.Millisecond), strings.TrimSpace(err.Error()),
		))
		return api.AdminAccount{}, err
	}
	if result.DriveError != "" {
		admin.requests.log("auth", "WARN", "账户添加 | Drive 授权失败 | 错误="+result.DriveError)
	}
	admin.requests.log("auth", "INFO", "账户添加 | 2/2 | 保存认证状态")
	if _, err := aistudio.NewSigner().Sign(result.StorageState); err != nil {
		return api.AdminAccount{}, fmt.Errorf("认证状态无法用于 AI Studio: %w", err)
	}
	accountConfig.Label = result.Email
	if err := accountConfig.Validate(); err != nil {
		return api.AdminAccount{}, invalidAccount(err)
	}
	// 登录出口只在账户新建成功后记到它名下（adopt 在 store.Create 之后、Worker 与账户池挂载之前）：邮箱已存在时
	// store.Create 失败，已有账户的端口与会话号不受影响；之后为它启动的 Worker 与登录共用这个出口
	created, err := admin.addAccount(ctx, accountConfig, result.StorageState, directory, poolLease.adopt)
	if err != nil {
		return api.AdminAccount{}, err
	}
	if result.DriveError == "" {
		admin.noteDriveAuthorized(created.ID)
	}
	admin.requests.log("auth", "INFO", fmt.Sprintf(
		"账户添加完成 | 账户=%s | 耗时=%s",
		created.Label, time.Since(startedAt).Round(time.Millisecond),
	))
	return created, nil
}

func (admin *runtimeAdmin) ChromeImportProfiles(context.Context) ([]api.ChromeImportProfile, error) {
	root, err := chromeauth.DefaultChromeRoot()
	if err != nil {
		return nil, err
	}
	accounts, err := chromeauth.Discover(root)
	if err != nil {
		return nil, err
	}
	existing := make(map[string]struct{})
	for _, status := range admin.pool.Status() {
		existing[strings.ToLower(strings.TrimSpace(status.ID))] = struct{}{}
	}
	profiles := make([]api.ChromeImportProfile, 0, len(accounts))
	for _, account := range accounts {
		email := strings.ToLower(strings.TrimSpace(account.Email))
		if !account.Importable || email == "" {
			continue
		}
		if _, exists := existing[email]; exists {
			continue
		}
		existing[email] = struct{}{}
		profiles = append(profiles, api.ChromeImportProfile{
			ID: account.ID, Profile: account.Profile, DisplayName: account.DisplayName, Email: email, Locale: account.Locale,
		})
	}
	return profiles, nil
}

func (admin *runtimeAdmin) ImportChromeAccounts(ctx context.Context, input api.ChromeImportInput) ([]api.AdminAccount, error) {
	if len(input.AccountIDs) == 0 {
		return nil, invalidAccount(fmt.Errorf("未选择 Chrome 账号"))
	}
	// 代理先规范化：原先要等 Chrome 导入（含 OAuth 网络请求）做完才校验，代理写错时白白等一轮导入
	accountProxy, err := normalizeAccountProxy(input.Proxy)
	if err != nil {
		return nil, err
	}
	root, err := chromeImportRoot()
	if err != nil {
		return nil, err
	}
	// 逐个导入时每个账号导入完就归还出口、只记下端口号或会话号：新建成功的账户按记下的值登记到名下（见 addAccount 的 onCreated），
	// 已存在的账户不受影响
	results, poolClaims, err := admin.importChrome(ctx, root, accountProxy, input.AccountIDs)
	if err != nil {
		return nil, err
	}
	configs := make([]aistudio.AccountConfig, len(results))
	seen := make(map[string]struct{}, len(results))
	for index, result := range results {
		email := strings.ToLower(strings.TrimSpace(result.Email))
		if _, exists := seen[email]; exists {
			return nil, invalidAccount(fmt.Errorf("Chrome 账号重复: %s", email))
		}
		seen[email] = struct{}{}
		accountConfig := aistudio.DefaultAccountConfig(email)
		accountConfig.Proxy = accountProxy
		if locale := strings.TrimSpace(input.Locale); locale != "" {
			accountConfig.Locale = locale
		} else if locale := strings.TrimSpace(result.Locale); locale != "" {
			accountConfig.Locale = locale
		}
		if timezone := strings.TrimSpace(input.Timezone); timezone != "" {
			accountConfig.Timezone = timezone
		}
		if err := accountConfig.Validate(); err != nil {
			return nil, invalidAccount(err)
		}
		if _, err := aistudio.NewSigner().Sign(result.State); err != nil {
			return nil, fmt.Errorf("认证状态无法用于 AI Studio: %w", err)
		}
		configs[index] = accountConfig
	}
	accounts := make([]api.AdminAccount, 0, len(results))
	for index, result := range results {
		account, err := admin.addAccount(ctx, configs[index], result.State, "", poolClaims[index].adopt)
		if err != nil {
			return nil, err
		}
		accounts = append(accounts, account)
		admin.requests.log("auth", "INFO", "Chrome 账户已导入 | 账户="+account.Label)
	}
	return accounts, nil
}

// addAccount 保存并挂载新账户；onCreated 在账户新建成功（store.Create）之后、Worker 与账户池挂载之前以账户 ID 调用，
// 可以为 nil（IP 池在这里把登录或导入用的出口记到新账户名下：邮箱已存在时不会调用）
func (admin *runtimeAdmin) addAccount(
	ctx context.Context,
	accountConfig aistudio.AccountConfig,
	state aistudio.StorageState,
	fingerprintDirectory string,
	onCreated func(accountID string),
) (created api.AdminAccount, resultErr error) {
	account, publishLease, err := admin.store.Create(accountConfig, state)
	if err != nil {
		return api.AdminAccount{}, err
	}
	if onCreated != nil {
		onCreated(account.ID)
	}
	defer func() {
		if publishLease != nil {
			resultErr = errors.Join(resultErr, publishLease.Release())
		}
	}()
	if fingerprintDirectory != "" {
		if err := camoufoxnative.PersistAccountFingerprint(fingerprintDirectory, account.Directory); err != nil {
			return api.AdminAccount{}, errors.Join(err, admin.store.Delete(account))
		}
	}
	if err := admin.headers.Add(account); err != nil {
		return api.AdminAccount{}, errors.Join(err, admin.store.Delete(account))
	}
	if err := admin.workers.Add(account); err != nil {
		return api.AdminAccount{}, errors.Join(err, admin.headers.Remove(account.ID), admin.store.Delete(account))
	}
	if err := admin.service.changeModels(func() error {
		return admin.pool.Add(account)
	}); err != nil {
		return api.AdminAccount{}, errors.Join(
			err, admin.workers.Remove(account.ID), admin.headers.Remove(account.ID), admin.store.Delete(account),
		)
	}
	// 账户已进入账户池：IP 池记下它的固定代理（外部占用、固定了默认代理）并推送默认出口
	admin.ipPool().refresh(admin.pool, account.ID)
	if err := publishLease.Release(); err != nil {
		return api.AdminAccount{}, err
	}
	publishLease = nil
	// 删除后用同一邮箱重新添加的账户不沿用之前的 403 无权限记录
	admin.service.forgetPermissionDenied(account.ID, "新增账户")
	admin.onboard.markManual(account.ID)
	admin.syncAccountModelCatalog(ctx, account)
	return admin.account(account.ID)
}

func (admin *runtimeAdmin) UpdateAccount(ctx context.Context, accountID string, input api.AccountInput) (api.AdminAccount, error) {
	return admin.updateAccount(ctx, accountID, input, nil)
}

// errAccountManuallyDisabled 表示账户是手动停用的，自动启用（新账户自动处理、认证文件更新）不启用它
var errAccountManuallyDisabled = errors.New("账户是手动停用的，保持停用")

// automaticEnable 为新账户自动处理启用账户时交给 updateAccount 的约束
type automaticEnable struct {
	// explicitAt 非零表示管理员在这一时间用“处理现有停用账户”把账户加入队列（见 disableNotes.blocksAutoEnable）
	explicitAt time.Time
}

// updateAccount 更新账户配置。automatic 非空表示由新账户自动处理启用账户：在账户租约内再看一次停用记录，
// 手动停用的不启用，返回 errAccountManuallyDisabled。原先只在租约外看账户状态，排队或验证期间被手动停用的账户
// 验证通过后又被启用，手动停用记录也随之清除，重启后同样不再保持停用；手动停用记录只在租约内写入，这里再看一次就没有空档
func (admin *runtimeAdmin) updateAccount(
	ctx context.Context,
	accountID string,
	input api.AccountInput,
	automatic *automaticEnable,
) (api.AdminAccount, error) {
	if !strings.EqualFold(strings.TrimSpace(input.Label), strings.TrimSpace(accountID)) {
		return api.AdminAccount{}, invalidAccount(fmt.Errorf("账户邮箱不可修改"))
	}
	// 代理规范化：onboarding 与管理页回传的已经是规范值，规范化后不变
	accountProxy, err := normalizeAccountProxy(input.Proxy)
	if err != nil {
		return api.AdminAccount{}, err
	}
	accountConfig := aistudio.DefaultAccountConfig(strings.ToLower(strings.TrimSpace(accountID)))
	accountConfig.Enabled = input.Enabled
	accountConfig.Proxy = accountProxy
	accountConfig.Locale = strings.TrimSpace(input.Locale)
	accountConfig.Timezone = strings.TrimSpace(input.Timezone)
	if err := accountConfig.Validate(); err != nil {
		return api.AdminAccount{}, invalidAccount(err)
	}
	lease, err := admin.acquireAccountForOperation(ctx, accountID)
	if err != nil {
		return api.AdminAccount{}, err
	}
	account := lease.Account()
	wasEnabled := account.Config.Enabled
	if automatic != nil && input.Enabled && !wasEnabled &&
		accountDisableNotes.blocksAutoEnable(account.ID, automatic.explicitAt) {
		return api.AdminAccount{}, errors.Join(errAccountManuallyDisabled, lease.Release())
	}
	headerUpdate, err := admin.headers.prepareUpdate(account, accountConfig)
	if err != nil {
		return api.AdminAccount{}, errors.Join(err, lease.Release())
	}
	workerUpdate, err := admin.workers.prepareUpdate(account, accountConfig)
	if err != nil {
		headerUpdate.Discard()
		return api.AdminAccount{}, errors.Join(err, lease.Release())
	}
	if err := admin.service.changeModels(func() error {
		return lease.SaveConfig(accountConfig)
	}); err != nil {
		workerUpdate.Discard()
		headerUpdate.Discard()
		return api.AdminAccount{}, errors.Join(err, lease.Release())
	}
	workerUpdate.Commit()
	headerUpdate.Commit()
	// 账户代理可能变了：IP 池重算它的外部占用与默认出口（改代理时旧 Worker 已关闭，租约随之归还）
	admin.ipPool().refresh(admin.pool, account.ID)
	switch {
	case !input.Enabled:
		// 已经停用的账户再次停用（批量停用选中了它、改代理后仍保存为停用）同样记为手动停用：原先只在启用改为停用时记录，
		// 新账户自动处理留下的自动停用记录不变，重启或认证文件更新后账户又被按默认启用。已有手动停用记录的更新为这次的
		// 时间：管理员“处理现有停用账户”之后再次停用的仍保持停用（见 disableNotes.blocksAutoEnable）
		reason := "通过管理页或管理接口停用"
		if caller := api.AdminCaller(ctx); caller != "" {
			reason += "（来源 " + caller + "）"
		}
		accountDisableNotes.set(account.ID, reason, false)
		if wasEnabled {
			admin.requests.log(account.ID, "WARN", "账户已停用 | "+reason)
		} else {
			admin.requests.log(account.ID, "INFO", "停用账户记为手动停用，重启与认证文件更新后保持停用 | "+reason)
		}
	case input.Enabled:
		// 管理员启用时清除包括手动停用在内的记录；新账户自动处理走到这里时上面已确认没有挡住启用的手动停用记录
		accountDisableNotes.clear(account.ID)
	}
	if err := lease.Release(); err != nil {
		return api.AdminAccount{}, err
	}
	admin.syncModelCache()
	if !wasEnabled && input.Enabled {
		// 停用账户不参与启动时的模型目录同步，启用后没有目录就不能预热、接不到请求：在后台补一次同步
		admin.startCatalogBackfill([]string{account.ID})
	}
	admin.requests.log("auth", "INFO", "账户配置已更新 | 账户="+accountConfig.Label)
	return admin.account(account.ID)
}

func (admin *runtimeAdmin) DeleteAccount(_ context.Context, accountID string) error {
	if _, err := admin.account(accountID); err != nil {
		return err
	}
	defer admin.syncModelCache()
	removed, err := admin.removeAccount(func(deleteDirectory func(*aistudio.Account) error) (*aistudio.Account, error) {
		var removed *aistudio.Account
		err := admin.service.changeModels(func() error {
			var removeErr error
			removed, removeErr = admin.pool.Remove(accountID, deleteDirectory)
			return removeErr
		})
		return removed, err
	})
	if removed == nil {
		return accountOperationError(err)
	}
	if err != nil {
		return err
	}
	admin.onboard.forget(removed.ID)
	return nil
}

// accountRemover 把账户移出号池：调用 deleteDirectory 删除认证目录，成功后返回移出的账户
type accountRemover func(deleteDirectory func(*aistudio.Account) error) (*aistudio.Account, error)

// removeAccount 删除账户，单个删除（DeleteAccount）与批量删除（DeleteAccounts）共用：remove 移出号池时关闭账户的 Worker 并删除
// 认证目录，之后清除模型目录重试、403 无权限记录、Build 生成的按账号暂停记录、Worker 与固定出口、停用原因，写“账户已删除”日志。
// 不刷新、不推送模型快照，也不移除新账户自动处理记录，由调用方处理（批量删除整批只做一次）。
// 移出号池失败时返回的账户为 nil、错误为 remove 的原始错误；移出后清理失败时返回账户与错误，不清除停用原因、不写日志（与原先一致）
func (admin *runtimeAdmin) removeAccount(remove accountRemover) (*aistudio.Account, error) {
	removed, err := remove(func(account *aistudio.Account) error {
		if workerErr := admin.workers.Reset(account.ID); workerErr != nil {
			return workerErr
		}
		return admin.store.Delete(account)
	})
	if err != nil {
		return nil, err
	}
	accountID := removed.ID
	admin.service.removeAccountModelRetry(accountID)
	admin.service.permissionDenied.clearAccount(accountID)
	admin.service.orgDisabled.clearAccount(accountID)
	// Build 生成的按账号暂停记录随账户删除：不再一直显示等待试探，重新上传的同 ID 账户从头判定
	admin.service.buildPauses().forget(accountID)
	if err := errors.Join(admin.workers.Remove(accountID), admin.headers.Remove(accountID)); err != nil {
		return removed, err
	}
	// 同一账户重新上传后不再沿用旧的停用原因（手动停用的记录会让它保持停用）
	accountDisableNotes.clear(accountID)
	label := strings.TrimSpace(removed.Config.Label)
	if label == "" {
		label = accountID
	}
	admin.requests.log("auth", "INFO", "账户已删除 | 账户="+label)
	return removed, nil
}

// noteDriveAuthorized 记下经本服务的登录流程登录、完成了官网 Drive 授权的账户 Drive 可用（新的认证文件保存之后调用）：
// gemini-omni 的视频输入（及无法转为 PNG 的图片）随即优先交给它，不必等用户请求偶然试到；生成服务不支持时什么也不做
func (admin *runtimeAdmin) noteDriveAuthorized(accountID string) {
	if noter, ok := admin.service.service.(driveAuthorizedNoter); ok {
		noter.NoteDriveAuthorized(accountID)
	}
}

func (admin *runtimeAdmin) LoginAccount(ctx context.Context, accountID string) (api.AdminAccount, error) {
	lease, err := admin.acquireAccountForOperation(ctx, accountID)
	if err != nil {
		return api.AdminAccount{}, err
	}
	account := lease.Account()
	if err := admin.workers.Reset(account.ID); err != nil {
		return api.AdminAccount{}, errors.Join(err, lease.Release())
	}
	directory, err := os.MkdirTemp("", "aistudio2api-account-login-*")
	if err != nil {
		return api.AdminAccount{}, errors.Join(fmt.Errorf("创建隔离登录目录: %w", err), lease.Release())
	}
	defer os.RemoveAll(directory)
	if err := camoufoxnative.PersistAccountFingerprint(account.Directory, directory); err != nil {
		return api.AdminAccount{}, errors.Join(err, lease.Release())
	}
	startedAt := time.Now()
	admin.requests.log(account.Config.Label, "INFO", "账户登录 | 1/2 | 等待隔离登录")
	loginProxy, poolLease := admin.browserProxy(account.ID, account.ID, proxyPoolKindLogin, account.Config.Label, account.Config.Proxy)
	defer poolLease.release()
	request := admin.loginRequest(account, directory, loginProxy)
	request.OnboardingCompleted = admin.onboardingLogger(account.Config.Label, "账户登录")
	result, err := admin.login.Login(ctx, request)
	if err != nil {
		admin.requests.log(account.Config.Label, "ERROR", fmt.Sprintf(
			"账户登录失败 | 耗时=%s | 错误=%s",
			time.Since(startedAt).Round(time.Millisecond), strings.TrimSpace(err.Error()),
		))
		// 登录后页面被跳到组织停用页：可见浏览器里登录的不一定是这个账户（落到这一页时还读不到登录邮箱），先在租约内用账户已保存的
		// 认证文件复核接口，也返回 app is disabled 才进入组织停用状态（见 org_disabled.go）
		confirmed := aistudio.OrgDisabledFailure(err) && admin.loginOrgDisabledConfirmed(ctx, lease)
		releaseErr := lease.Release()
		if confirmed {
			admin.service.enterOrgDisabled(account.ID, account.Config.Label, aistudio.OrgDisabledFromLogin, err)
		}
		return api.AdminAccount{}, errors.Join(err, releaseErr)
	}
	if !strings.EqualFold(strings.TrimSpace(result.Email), account.ID) {
		return api.AdminAccount{}, errors.Join(
			invalidAccount(fmt.Errorf("登录邮箱与账户不一致: %s", result.Email)), lease.Release(),
		)
	}
	if result.DriveError != "" {
		admin.requests.log(account.Config.Label, "WARN", "账户登录 | Drive 授权失败 | 错误="+result.DriveError)
	}
	admin.requests.log(account.Config.Label, "INFO", "账户登录 | 2/2 | 保存认证状态")
	if _, err := aistudio.NewSigner().Sign(result.StorageState); err != nil {
		return api.AdminAccount{}, errors.Join(fmt.Errorf("认证状态无法用于 AI Studio: %w", err), lease.Release())
	}
	if err := camoufoxnative.PersistAccountFingerprint(directory, account.Directory); err != nil {
		return api.AdminAccount{}, errors.Join(err, lease.Release())
	}
	if err := lease.SaveStorageState(result.StorageState); err != nil {
		return api.AdminAccount{}, errors.Join(err, lease.Release())
	}
	// 不清空模型目录：同一邮箱即同一账户，权益与目录的变化由随后的刷新纠正（SetCatalog 发现权益变化时清除模型资格）。
	// 原先先清空目录，随后的同步遇到 429 时目录一直为空，账号登录成功却不是任何模型的候选
	if err := admin.service.changeModels(func() error {
		return errors.Join(
			admin.pool.MarkReady(account.ID),
			admin.pool.ResetModelAccess(account.ID),
		)
	}); err != nil {
		return api.AdminAccount{}, errors.Join(err, lease.Release())
	}
	// 重新登录得到新的认证文件：清除 403 无权限记录与冷却，释放后立即重新参与调度
	admin.service.forgetPermissionDenied(account.ID, "浏览器登录")
	if err := lease.Release(); err != nil {
		return api.AdminAccount{}, err
	}
	// 清掉疑似失效、登录复核记录与疑似登录失效冷却（与管理验证通过、认证文件更新同一处理）
	admin.service.noteLoginRestored(account.ID)
	if result.DriveError == "" {
		// 登录时完成了官网的 Drive 授权：按新的认证代际记为 Drive 可用（见 aistudio.PooledService.NoteDriveAuthorized）
		admin.noteDriveAuthorized(account.ID)
	}
	admin.syncAccountModelCatalog(ctx, account)
	admin.requests.log(account.Config.Label, "INFO", fmt.Sprintf(
		"账户登录完成 | 耗时=%s",
		time.Since(startedAt).Round(time.Millisecond),
	))
	return admin.account(account.ID)
}

// loginOrgDisabledConfirmed 在浏览器登录落到组织停用页后，用账户已保存的认证文件经 Go HTTP 复核一次 GetAiStudioBenefitTier
// （ProbeLogin，不续签、不改账户状态），返回是否确认是这个账户所属组织停用了 AI Studio：只有接口同样返回 app is disabled 才确认。
// 管理员对账户 A 点“重新登录”却在可见浏览器里登录了被组织停用的 B 时，A 自己的认证文件复核通过（或 401、出错），不把 A 标为组织停用；
// 不确认时写一行警告，账户状态不变。服务不支持复核时同样不确认
func (admin *runtimeAdmin) loginOrgDisabledConfirmed(ctx context.Context, lease *aistudio.AccountLease) bool {
	label := lease.Account().Config.Label
	outcome := "跳过"
	if prober, ok := admin.service.service.(loginProber); ok {
		probeCtx, cancel := context.WithTimeout(ctx, authRecheckTimeout)
		probeErr := prober.ProbeLogin(probeCtx, lease)
		cancel()
		switch {
		case aistudio.OrgDisabledFailure(probeErr):
			return true
		case probeErr == nil:
			outcome = "通过"
		case aistudio.DefinitiveAuthenticationFailure(probeErr):
			outcome = "401"
		default:
			outcome = "出错"
		}
	}
	admin.requests.log(label, "WARN", "账户登录 | 登录后停在组织停用页，但已保存的认证文件接口复核没有返回 app is disabled"+
		"（可能在浏览器里登录了别的账号），账户状态不变 | 接口复核="+outcome)
	return false
}

// VerifyAccount 验证账户登录并返回账户的管理视图，带上验证结论（authenticated）与原因：前端按它计“有效 / 登录失效 / 出错”，
// 不再把 HTTP 200 一律算成功（原先批量验证显示“成功 44 失败 0”，结果却是 44 个登录已失效）
func (admin *runtimeAdmin) VerifyAccount(ctx context.Context, accountID string) (api.AdminAccount, error) {
	verification, err := admin.verifyLogin(ctx, accountID)
	if err != nil {
		return api.AdminAccount{}, err
	}
	account, err := admin.account(accountID)
	if err != nil {
		return api.AdminAccount{}, err
	}
	// 与新账户自动处理同一口径（onboardVerifyFailure）：浏览器判定未登录，或验证后账户仍被标为需要登录，都算登录失效
	status, _ := admin.pool.StatusOf(accountID)
	failed, reason := onboardVerifyFailure(verification, status)
	authenticated := !failed
	account.Authenticated = &authenticated
	account.VerifyReason = reason
	if verification.OrgDisabled {
		// 停用账户的状态始终是 disabled：前端按 org_disabled 提示组织停用、批量验证单独计数，不当成登录失效
		account.OrgDisabled = true
	}
	return account, nil
}

const (
	// verifyLoginExpiredReason 为浏览器判定登录已失效、没有给出原因时的原因
	verifyLoginExpiredReason = "AI Studio 登录已失效"
	// verifyAPIRejectedReason 为浏览器已登录、接口复核却返回 401 时的原因：浏览器带着自己的 Cookie，接口用的是磁盘上保存的 Cookie
	verifyAPIRejectedReason = "浏览器已登录，但接口认证失败：Cookie 可能已被 Google 作废，需要重新登录"
)

// verifyLogin 启动隔离浏览器验证登录并写回账户状态，返回验证结论。停用账户的对外状态始终是 disabled
// （登录失效也不会显示为需要登录），调用方判断验证是否通过必须看返回的 Authenticated，不能看账户状态。
//   - 浏览器判定已登录：先不动目录，用磁盘 Cookie 经 Go HTTP 复核一次 GetAiStudioBenefitTier（不续签，见 PooledService.ProbeLogin）；
//     通过（或复核出错、不能下结论）才标就绪并同步目录；返回 401 时保留目录、标需要登录，结论改为未认证，原因为 verifyAPIRejectedReason。
//     原先不复核就标就绪、同步目录，目录同步用磁盘上的旧 Cookie 又被 401 标回需要登录，模型列变成“—”；
//   - 浏览器判定未登录：原因带上跳转到的登录页 host+path（不含查询参数），区分普通登录页与安全验证、风控页。
//
// 验证不导出浏览器 Cookie（要改 camoufoxnative 的 Verify，不在本批），接口复核失败的账户需要重新登录
func (admin *runtimeAdmin) verifyLogin(ctx context.Context, accountID string) (aistudio.LoginVerification, error) {
	lease, err := admin.acquireAccountForOperation(ctx, accountID)
	if err != nil {
		return aistudio.LoginVerification{}, err
	}
	account := lease.Account()
	startedAt := time.Now()
	admin.requests.log(account.Config.Label, "INFO", "账户验证 | 访问 AI Studio")
	// 验证是完整的浏览器会话：同样占一个 IP 池出口（账户已有 Worker 时共用它的端口），返回时归还
	verifyProxy, poolLease := admin.browserProxy(account.ID, account.ID, proxyPoolKindVerify, account.Config.Label, account.Config.Proxy)
	defer poolLease.release()
	request := admin.loginRequest(account, account.Directory, verifyProxy)
	request.OnboardingCompleted = admin.onboardingLogger(account.Config.Label, "账户验证")
	verification, err := admin.login.Verify(ctx, request, account.StorageState)
	if err != nil {
		admin.requests.log(account.Config.Label, "ERROR", fmt.Sprintf(
			"账户验证失败 | 耗时=%s | 错误=%s",
			time.Since(startedAt).Round(time.Millisecond), strings.TrimSpace(err.Error()),
		))
		return aistudio.LoginVerification{}, errors.Join(err, lease.Release())
	}
	// apiCheck 为接口复核的结论（写进“账户验证完成”）：通过、401、出错、跳过（浏览器未登录或服务不支持复核）
	apiCheck := "跳过"
	if verification.Authenticated {
		if prober, ok := admin.service.service.(loginProber); ok {
			probeCtx, cancel := context.WithTimeout(ctx, authRecheckTimeout)
			probeErr := prober.ProbeLogin(probeCtx, lease)
			cancel()
			switch {
			case probeErr == nil:
				apiCheck = "通过"
			case aistudio.DefinitiveAuthenticationFailure(probeErr):
				apiCheck = "401"
				verification.Authenticated = false
				verification.Reason = verifyAPIRejectedReason
			case aistudio.OrgDisabledFailure(probeErr):
				// 浏览器停在 AI Studio，接口却返回 “AI Studio app is disabled”：同样是账号所属组织停用了 AI Studio
				apiCheck = "组织停用"
				verification.Authenticated = false
				verification.OrgDisabled = true
				verification.Reason = aistudio.OrgDisabledReason
			default:
				// 上游 429、连接失败等不能说明登录失效：按浏览器的结论处理，随后的目录同步照常
				apiCheck = "出错"
				admin.requests.log(account.Config.Label, "WARN", "账户验证 | 接口复核出错，按浏览器结论处理 | 错误="+authRequiredCauseSummary(probeErr))
			}
		}
	}
	reason := strings.TrimSpace(verification.Reason)
	if reason == "" {
		reason = verifyLoginExpiredReason
	}
	if !verification.Authenticated && verification.PageURL != "" && apiCheck != "401" && !verification.OrgDisabled {
		reason += "（跳转到 " + verification.PageURL + "）"
	}
	if !verification.Authenticated {
		verification.Reason = reason
	}
	if verification.OrgDisabled {
		// 账号所属组织停用了 AI Studio（页面停在组织停用页，或接口返回 app is disabled）：不是登录失效，不标需要登录，
		// 释放账户后进入组织停用状态（见 org_disabled.go）
		if err := lease.Release(); err != nil {
			return aistudio.LoginVerification{}, err
		}
		admin.service.enterOrgDisabled(account.ID, account.Config.Label, aistudio.OrgDisabledFromVerify, errors.New(reason))
		admin.requests.log(account.Config.Label, "INFO", fmt.Sprintf(
			"账户验证完成 | 已认证=false | 耗时=%s | 接口复核=%s | 页面=%s | 原因=%s",
			time.Since(startedAt).Round(time.Millisecond), apiCheck, orDash(verification.PageURL), aistudio.OrgDisabledReason,
		))
		return verification, nil
	}
	err = admin.service.changeModels(func() error {
		if verification.Authenticated {
			// 保留原有目录与 Worker：随后的目录同步照旧刷新，同步失败（如上游 429）时旧目录仍在，账号立刻可调度。
			// 原先在这里清空目录且不关 Worker，同步失败后目录一直为空，Worker 成了不能服务任何模型的僵尸
			return errors.Join(
				admin.pool.MarkReady(account.ID),
				admin.pool.ResetModelAccess(account.ID),
			)
		}
		// 浏览器判定未登录或接口复核 401：直接标需要登录（管理验证是浏览器证据，不经疑似登录失效的复核），目录保留
		return admin.pool.MarkAuthRequired(account.ID, reason)
	})
	if err != nil {
		return aistudio.LoginVerification{}, errors.Join(err, lease.Release())
	}
	if verification.Authenticated && apiCheck == "通过" {
		// 浏览器停在 AI Studio 输入框，接口复核（GetAiStudioBenefitTier，组织停用时正是它返回 app is disabled）也通过：组织已重新开放
		// AI Studio，清除组织停用状态、立即恢复调度（没有这个状态时什么都不做）。原先只靠随后的目录同步成功才清除，同步遇到上游 429、
		// 网络错误时提示验证通过，账户却仍是组织停用、要等约一天后的复查。在租约内清除：改回常规的目录任务这时取不到空闲账户、
		// 按忙碌推后，由下面的同步刷新目录；接口复核出错（不能下结论）时保持原状
		admin.service.forgetOrgDisabled(account.ID, "验证通过")
	}
	if err := lease.Release(); err != nil {
		return aistudio.LoginVerification{}, err
	}
	if verification.Authenticated {
		// 验证通过即清掉目录缓存里的需要登录记录与疑似失效、登录复核的记录与疑似登录失效冷却（不等随后的同步成功：
		// 同步可能因上游 429 失败），与浏览器登录、认证文件更新同一处理
		admin.service.noteLoginRestored(account.ID)
		admin.syncAccountModelCatalog(ctx, account)
	} else {
		admin.service.publishModelAccess()
		// 验证判定登录已失效：登记释放驻留 Worker（不等 keepWarm 补登记），写“账号需要登录 | 来源=管理验证”
		admin.workers.releaseUnschedulable(account.ID, unschedulableAuthRequired)
		admin.service.noteAuthRequired(account.ID, account.Config.Label, authFromVerify, errors.New(reason))
		// 浏览器判定未登录是浏览器证据，定期登录复核（接口、磁盘 Cookie）不复核它；浏览器已登录、接口 401 是接口证据，照常复核
		admin.service.authRecheck.noteMarked(account.ID, apiCheck != "401")
	}
	page := verification.PageURL
	if page == "" {
		page = "-"
	}
	admin.requests.log(account.Config.Label, "INFO", fmt.Sprintf(
		"账户验证完成 | 已认证=%t | 耗时=%s | 接口复核=%s | 页面=%s",
		verification.Authenticated, time.Since(startedAt).Round(time.Millisecond), apiCheck, page,
	))
	return verification, nil
}

// StartService 使用管理器提供的启动生命周期启动生成服务
func (admin *runtimeAdmin) StartService(ctx context.Context) (api.AdminStatus, error) {
	startedAt := time.Now()
	models, started, err := admin.service.Start(ctx, func() {
		status, statusErr := admin.Status(ctx)
		if statusErr == nil {
			admin.publishRuntimeSnapshot(ctx, status)
		}
	})
	if errors.Is(err, errServiceTransitioning) {
		return admin.Status(ctx)
	}
	if err != nil {
		status, statusErr := admin.Status(ctx)
		if statusErr == nil {
			admin.publishRuntimeSnapshot(ctx, status)
		}
		if errors.Is(err, context.Canceled) && admin.service.State() == "STOPPED" {
			admin.requests.log("service", "INFO", fmt.Sprintf(
				"生成服务启动已取消 | 耗时=%s",
				time.Since(startedAt).Round(time.Millisecond),
			))
			return status, statusErr
		}
		admin.requests.log("service", "ERROR", fmt.Sprintf(
			"生成服务启动失败 | 耗时=%s | 错误=%s",
			time.Since(startedAt).Round(time.Millisecond), strings.TrimSpace(err.Error()),
		))
		if errors.Is(err, aistudio.ErrNoEligibleAccount) {
			return api.AdminStatus{}, &adminOperationError{
				status: http.StatusBadRequest, code: "account_required", message: "请先启用一个可用账户",
			}
		}
		return api.AdminStatus{}, err
	}
	if len(models) == 0 {
		admin.requests.log("service", "ERROR", fmt.Sprintf(
			"生成服务启动失败 | 耗时=%s | 错误=没有可用账户",
			time.Since(startedAt).Round(time.Millisecond),
		))
		return api.AdminStatus{}, &adminOperationError{
			status: http.StatusBadRequest, code: "account_required", message: "请先添加一个可用账户",
		}
	}
	if started {
		admin.requests.log("service", "INFO", fmt.Sprintf(
			"生成服务就绪 | 模型=%d | Worker=%d/%d | 耗时=%s",
			len(models), len(admin.workers.WarmAccountIDs()), admin.workers.PrewarmTarget(),
			time.Since(startedAt).Round(time.Millisecond),
		))
	} else {
		admin.requests.log("service", "INFO", fmt.Sprintf(
			"生成服务运行中 | 模型=%d | Worker=%d/%d",
			len(models), len(admin.workers.WarmAccountIDs()), admin.workers.PrewarmTarget(),
		))
	}
	status, err := admin.Status(ctx)
	if err == nil {
		admin.publishRuntimeSnapshot(ctx, status)
	}
	return status, err
}

func (admin *runtimeAdmin) StopService(ctx context.Context) (api.AdminStatus, error) {
	startedAt := time.Now()
	admin.requests.log("service", "INFO", fmt.Sprintf(
		"生成服务停止 | Worker=%d",
		len(admin.workers.WarmAccountIDs()),
	))
	stopped, err := admin.service.Stop()
	if err != nil {
		admin.requests.log("service", "ERROR", fmt.Sprintf(
			"生成服务停止失败 | 耗时=%s | 错误=%s",
			time.Since(startedAt).Round(time.Millisecond), strings.TrimSpace(err.Error()),
		))
		return api.AdminStatus{}, err
	}
	if stopped {
		admin.requests.log("service", "INFO", fmt.Sprintf(
			"生成服务已停止 | 耗时=%s",
			time.Since(startedAt).Round(time.Millisecond),
		))
	} else {
		admin.requests.log("service", "INFO", "生成服务已处于停止状态")
	}
	status, err := admin.Status(ctx)
	if err == nil {
		admin.publishRuntimeSnapshot(ctx, status)
	}
	return status, err
}

func (admin *runtimeAdmin) ClearLogs(context.Context) error {
	admin.requests.clearLogs()
	return nil
}

// syncModelCache 在账户写入后刷新权威快照
func (admin *runtimeAdmin) syncModelCache() {
	ctx := admin.lifecycle
	_ = admin.service.SyncModels(ctx)
	status, err := admin.Status(ctx)
	if err == nil {
		admin.publishRuntimeSnapshot(ctx, status)
	}
}

func (admin *runtimeAdmin) syncAccountModelCatalog(ctx context.Context, account *aistudio.Account) {
	models, err := admin.service.syncAccountModels(ctx, account.ID)
	if err != nil {
		admin.service.publishModelAccess()
		return
	}
	if len(models) > 0 {
		admin.requests.log(account.Config.Label, "INFO", fmt.Sprintf("账户模型目录同步完成 | 模型=%d", len(models)))
	}
	admin.service.publishModelAccess()
}

// publishRuntimeSnapshot 推送管理页权威运行状态
func (admin *runtimeAdmin) publishRuntimeSnapshot(ctx context.Context, status api.AdminStatus) {
	models, err := admin.Models(ctx)
	if err != nil {
		return
	}
	accounts, err := admin.Accounts(ctx)
	if err != nil {
		return
	}
	admin.requests.publish(api.AdminEvent{Type: "status", Data: status})
	admin.requests.publish(api.AdminEvent{Type: "models", Data: map[string]any{"models": models}})
	admin.requests.publish(api.AdminEvent{Type: "accounts", Data: map[string]any{"accounts": accounts}})
}

// account 返回单个账户的管理视图：直接构建这一个账户的状态，不经过 Status() 的全量列表与缓存，管理操作刚写入的变化立即可见
func (admin *runtimeAdmin) account(accountID string) (api.AdminAccount, error) {
	if status, ok := admin.pool.StatusOf(accountID); ok {
		accounts := []api.AdminAccount{adminAccountDTO(status)}
		admin.ipPool().markPinned(accounts)
		return accounts[0], nil
	}
	return api.AdminAccount{}, accountOperationError(fmt.Errorf("%w: %s", aistudio.ErrAccountNotFound, accountID))
}

func (admin *runtimeAdmin) effectiveProxy(accountProxy string) string {
	if proxy := strings.TrimSpace(accountProxy); proxy != "" {
		return proxy
	}
	return strings.TrimSpace(admin.config.Proxy)
}

// onboardingLogger 返回登录、验证时自动完成 AI Studio 首次使用页的日志回调：写“<operation> | 完成 AI Studio 首次使用页 | 勾选=N（…） | 按钮=… | 已做=… | 耗时=…”
// （格式同 Worker 启动时的那一行，见 onboardingCompletedMessage）
func (admin *runtimeAdmin) onboardingLogger(label string, operation string) func(camoufoxnative.OnboardingResult) {
	return func(result camoufoxnative.OnboardingResult) {
		admin.requests.log(label, "INFO", operation+" | "+onboardingCompletedMessage(result))
	}
}

// loginRequest 为已有账户的隔离登录或验证请求；proxy 为这次浏览器实际使用的出口（见 browserProxy）
func (admin *runtimeAdmin) loginRequest(account *aistudio.Account, directory string, proxy string) aistudio.IsolatedLoginRequest {
	return aistudio.IsolatedLoginRequest{
		AccountID: account.ID, Directory: directory, Proxy: proxy,
		Locale: account.Config.Locale, Timezone: account.Config.Timezone,
	}
}

func adminAccountDTO(status aistudio.AccountStatus) api.AdminAccount {
	models := make([]string, len(status.Models))
	copy(models, status.Models)
	message := status.Message
	if !status.Enabled {
		// 停用账户显示停用原因与时间；所属组织停用了 AI Studio 时接上这个原因（新账户自动处理验证未通过的停用原因里已经带着时不重复）
		message = accountDisableNotes.message(status.ID)
		if status.OrgDisabled && !strings.Contains(message, aistudio.OrgDisabledReason) {
			message += "；" + aistudio.OrgDisabledReason
		}
	}
	return api.AdminAccount{
		ID: status.ID, Label: status.Label, Enabled: status.Enabled, State: string(status.State),
		Proxy: status.Proxy, Locale: status.Locale, Timezone: status.Timezone,
		Models: models, BenefitTier: status.BenefitTier, Message: message,
		BenefitTierKnown: status.BenefitTierKnown, Pool: status.Pool,
		ProxyPoolSlot: status.ProxySlot, ProxyPoolActive: status.ProxySlotActive,
		OrgDisabled: status.OrgDisabled,
	}
}

func invalidAccount(err error) error {
	return &adminOperationError{
		status: http.StatusBadRequest, code: "invalid_account", message: err.Error(),
	}
}

// accountOperationWait 返回管理端账户操作（保存账户、重新登录、验证登录）等待独占租约的上限：REQUEST_TIMEOUT + 30 秒，即拆分期限之前
// 进行中的请求都受 REQUEST_TIMEOUT 限制时的隐含上限；生成服务没有设置时按默认请求超时
func (admin *runtimeAdmin) accountOperationWait() time.Duration {
	if admin.operationWait > 0 {
		return admin.operationWait
	}
	var timeout time.Duration
	if admin.service != nil {
		timeout = time.Duration(admin.service.timeout.Load())
	}
	if timeout <= 0 {
		timeout = config.Default().RequestTimeout
	}
	return timeout + drainTimeoutGrace
}

// errAccountLongBusy 返回管理端账户操作等满上限仍有请求在用账户的错误：多半是开始输出后的长流（最长到总时长上限，见 generate_context.go）
func errAccountLongBusy() error {
	return &adminOperationError{
		status: http.StatusConflict, code: "account_busy", message: "账户有长时间生成在进行，稍后重试",
	}
}

// acquireAccountForOperation 为管理端账户操作取得独占租约，最多等 accountOperationWait：不超过 REQUEST_TIMEOUT 的请求照常等它们结束后生效；
// 开始输出的长流可以持续到总时长上限，登记独占等待期间账户不发出新租约，不能让一次点击把账户挂到长流结束，等满后返回 account_busy。
// 不用秒级上限：持续有负载的账户几乎等不到短时间的空闲，保存代理或语言会一直失败。只限制取得租约，取得后的登录、验证照常用 ctx
func (admin *runtimeAdmin) acquireAccountForOperation(ctx context.Context, accountID string) (*aistudio.AccountLease, error) {
	acquireCtx, cancel := context.WithTimeout(ctx, admin.accountOperationWait())
	defer cancel()
	lease, err := admin.pool.AcquireAccount(acquireCtx, accountID)
	if err != nil {
		if acquireCtx.Err() != nil && ctx.Err() == nil {
			return nil, errAccountLongBusy()
		}
		return nil, accountOperationError(err)
	}
	return lease, nil
}

func accountOperationError(err error) error {
	switch {
	case errors.Is(err, aistudio.ErrAccountNotFound):
		return &adminOperationError{
			status: http.StatusNotFound, code: "account_not_found", message: err.Error(),
		}
	case errors.Is(err, aistudio.ErrAccountLeased):
		return &adminOperationError{
			status: http.StatusConflict, code: "account_busy", message: err.Error(),
		}
	default:
		return err
	}
}

func (admin *runtimeAdmin) RuntimeConfig(context.Context) (api.RuntimeConfig, error) {
	cfg, err := config.Load(admin.configPath)
	if err != nil {
		return api.RuntimeConfig{}, err
	}
	return runtimeConfigDTO(cfg), nil
}

// invalidConfigError 表示管理页面提交的配置无效，返回 400 invalid_config（原先按上游错误返回 502）
func invalidConfigError(err error) error {
	return &adminOperationError{status: http.StatusBadRequest, code: "invalid_config", message: err.Error()}
}

func (admin *runtimeAdmin) UpdateRuntimeConfig(_ context.Context, value api.RuntimeConfig) (api.RuntimeConfig, error) {
	initTimeout, err := time.ParseDuration(value.InitTimeout)
	if err != nil {
		return api.RuntimeConfig{}, invalidConfigError(fmt.Errorf("INIT_TIMEOUT 无效: %w", err))
	}
	requestTimeout, err := time.ParseDuration(value.RequestTimeout)
	if err != nil {
		return api.RuntimeConfig{}, invalidConfigError(fmt.Errorf("REQUEST_TIMEOUT 无效: %w", err))
	}
	// 管理页面不编辑 AUTO_START、REQUEST_BODY_LOG 与 ADMIN_PASSWORD，保存时沿用 .env 中的现值，避免被整体重写抹掉。
	// 现有 .env 读不出来时不保存：原先按默认值继续，会用空的 ADMIN_PASSWORD 与默认降级判定设置覆盖写盘
	saved, loadErr := config.Load(admin.configPath)
	if loadErr != nil {
		return api.RuntimeConfig{}, &adminOperationError{
			status: http.StatusConflict, code: "config_unreadable",
			message: "现有配置文件读取失败，未保存（请先修正 .env）: " + loadErr.Error(),
		}
	}
	autoStart := saved.AutoStart
	requestBodyLog := saved.RequestBodyLog
	adminPassword := saved.AdminPassword
	// 首事件超时：旧版页面保存时不带该字段，沿用现值
	firstEventTimeout := saved.FirstEventTimeout
	if strings.TrimSpace(value.FirstEventTimeout) != "" {
		if firstEventTimeout, err = time.ParseDuration(strings.TrimSpace(value.FirstEventTimeout)); err != nil {
			return api.RuntimeConfig{}, invalidConfigError(fmt.Errorf("FIRST_EVENT_TIMEOUT 无效: %w", err))
		}
	}
	downgradeGuard := saved.DowngradeGuard
	// 降级判定设置：旧版页面保存时不带该字段，沿用现值
	if value.DowngradeGuard != nil {
		downgradeGuard = downgradeGuardFromAPI(*value.DowngradeGuard)
	}
	// Ultra 号池设置：旧版页面保存时不带这些字段，沿用现值（Ultra 常驻数可以为 0，不能用零值判断是否缺省）
	ultraExclusive, ultraWarm, ultraMax := saved.UltraExclusive, saved.UltraWarmWorkerLimit, saved.UltraMaxActiveWorkers
	ultraIncludePro := saved.UltraIncludePro
	if value.UltraExclusive != nil {
		ultraExclusive = *value.UltraExclusive
	}
	if value.UltraIncludePro != nil {
		ultraIncludePro = *value.UltraIncludePro
	}
	if value.UltraWarmWorkerLimit != nil {
		ultraWarm = *value.UltraWarmWorkerLimit
	}
	if value.UltraMaxActiveWorkers != nil {
		ultraMax = *value.UltraMaxActiveWorkers
	}
	// 单账户每分钟上限：旧版页面保存时不带该字段，沿用现值（0 表示关闭，不能用零值判断是否缺省）
	accountRPMLimit := saved.AccountRPMLimit
	if value.AccountRPMLimit != nil {
		accountRPMLimit = *value.AccountRPMLimit
	}
	// 流式优先 Playground 的模型：旧版页面保存时不带该字段，沿用现值；空数组表示清空
	streamPlaygroundModels := saved.StreamPlaygroundModels
	if value.StreamPlaygroundModels != nil {
		streamPlaygroundModels = config.NormalizeModelList(*value.StreamPlaygroundModels)
	}
	// 主用模型与排队上限：旧版页面保存时不带这些字段，沿用现值；空数组表示清空，排队上限 0 表示不排队
	primaryModels := saved.PrimaryModels
	if value.PrimaryModels != nil {
		primaryModels = config.NormalizeModelList(*value.PrimaryModels)
	}
	primaryQueueWait := saved.PrimaryQueueWait
	if strings.TrimSpace(value.PrimaryQueueWait) != "" {
		if primaryQueueWait, err = time.ParseDuration(strings.TrimSpace(value.PrimaryQueueWait)); err != nil {
			return api.RuntimeConfig{}, invalidConfigError(fmt.Errorf("PRIMARY_QUEUE_WAIT 无效: %w", err))
		}
	} else if primaryQueueWait > requestTimeout {
		// 旧版页面看不到排队上限：它把请求超时调到现值以下时按请求超时封顶，而不是报一个页面上没有的字段无效
		primaryQueueWait = requestTimeout
	}
	// Ultra 主用模型与排队上限：规则与普通主用模型相同（旧版页面沿用现值，空数组表示关闭，排队上限 0 表示不排队）
	ultraPrimaryModels := saved.UltraPrimaryModels
	if value.UltraPrimaryModels != nil {
		ultraPrimaryModels = config.NormalizeModelList(*value.UltraPrimaryModels)
	}
	ultraPrimaryQueueWait := saved.UltraPrimaryQueueWait
	if strings.TrimSpace(value.UltraPrimaryQueueWait) != "" {
		if ultraPrimaryQueueWait, err = time.ParseDuration(strings.TrimSpace(value.UltraPrimaryQueueWait)); err != nil {
			return api.RuntimeConfig{}, invalidConfigError(fmt.Errorf("ULTRA_PRIMARY_QUEUE_WAIT 无效: %w", err))
		}
	} else if ultraPrimaryQueueWait > requestTimeout {
		ultraPrimaryQueueWait = requestTimeout
	}
	// 媒体设置：旧版页面保存时不带这些字段，沿用现值；期限与大小按文本解析，写进 .env 与返回给页面的都是规范写法
	media, err := mediaSettingsFromInput(saved.Media, value)
	if err != nil {
		return api.RuntimeConfig{}, invalidConfigError(err)
	}
	// 默认代理先规范化：写进 .env 的与返回给页面的都是规范值，页面同步回表单后不再显示未保存，也不误报需要重启
	proxy, err := config.NormalizeProxy(value.Proxy)
	if err != nil {
		return api.RuntimeConfig{}, invalidConfigError(err)
	}
	// IP 池：旧版页面保存时不带该字段，沿用现值；地址写进 .env 与返回给页面的都是规范值
	poolSettings := saved.ProxyPool
	if value.ProxyPool != nil {
		if poolSettings, err = proxyPoolFromInput(*value.ProxyPool); err != nil {
			return api.RuntimeConfig{}, invalidConfigError(err)
		}
	}
	cfg := config.Config{
		AuthStates: value.AuthStates, ListenAddr: value.ListenAddr, ProxyAPIKey: config.EffectiveProxyAPIKey(value.APIKey),
		Proxy: proxy, ProxyPool: poolSettings, InitTimeout: initTimeout, RequestTimeout: requestTimeout, FirstEventTimeout: firstEventTimeout,
		WarmWorkerLimit: value.WarmWorkerLimit, MaxActiveWorkers: value.MaxActiveWorkers,
		UltraExclusive: ultraExclusive, UltraIncludePro: ultraIncludePro, UltraWarmWorkerLimit: ultraWarm, UltraMaxActiveWorkers: ultraMax,
		WarmStartupConcurrency: value.WarmStartupConcurrency,
		PerAccountConcurrency:  value.PerAccountConcurrency,
		AccountRPMLimit:        accountRPMLimit,
		RoutingStrategy:        value.RoutingStrategy,
		UpstreamChannels:       value.UpstreamChannels,
		StreamPlaygroundModels: streamPlaygroundModels,
		PrimaryModels:          primaryModels,
		PrimaryQueueWait:       primaryQueueWait,
		UltraPrimaryModels:     ultraPrimaryModels,
		UltraPrimaryQueueWait:  ultraPrimaryQueueWait,
		TemporaryChat:          value.TemporaryChat,
		IgnoreClientSeed:       value.IgnoreClientSeed,
		RepeatPromptNonce:      value.RepeatPromptNonce,
		MinOutputTokens:        value.MinOutputTokens,
		WAABackend:             value.WAABackend,
		AutoStart:              autoStart,
		RequestBodyLog:         requestBodyLog,
		AdminPassword:          adminPassword,
		DowngradeGuard:         downgradeGuard,
		Media:                  media,
	}
	if err := cfg.Validate(); err != nil {
		return api.RuntimeConfig{}, invalidConfigError(err)
	}
	if err := cfg.Save(admin.configPath); err != nil {
		return api.RuntimeConfig{}, err
	}
	admin.requests.log("service", "INFO", "服务配置已保存")
	return runtimeConfigDTO(cfg), nil
}

func (admin *runtimeAdmin) Cooldowns(context.Context) ([]api.AdminCooldown, error) {
	statuses := admin.pool.Status()
	cooldowns := make([]api.AdminCooldown, 0)
	now := time.Now()
	for _, account := range statuses {
		models := make(map[string]struct{}, len(account.Models))
		for _, modelID := range account.Models {
			models[modelID] = struct{}{}
		}
		effective := make(map[string]aistudio.CooldownState)
		if global, ok := account.Cooldowns["*"]; ok && global.Active(now) {
			for modelID := range models {
				effective[modelID] = global
			}
		}
		for key, cooldown := range account.Cooldowns {
			if key == "*" || !cooldown.Active(now) {
				continue
			}
			modelID, build := strings.CutPrefix(key, string(aistudio.ChannelBuild)+":")
			if _, ok := models[modelID]; !ok && !build {
				continue
			}
			if current, ok := effective[key]; !ok || cooldown.Until.After(current.Until) {
				effective[key] = cooldown
			}
		}
		modelIDs := make([]string, 0, len(effective))
		for modelID := range effective {
			modelIDs = append(modelIDs, modelID)
		}
		sort.Strings(modelIDs)
		for _, key := range modelIDs {
			cooldown := effective[key]
			channel := string(aistudio.ChannelPlayground)
			modelID, build := strings.CutPrefix(key, string(aistudio.ChannelBuild)+":")
			if build {
				channel = string(aistudio.ChannelBuild)
			}
			cooldowns = append(cooldowns, api.AdminCooldown{
				AccountID: account.ID, AccountLabel: account.Label, Channel: channel,
				ModelID: modelID, Until: cooldown.Until, Reason: cooldown.Reason,
			})
		}
	}
	return cooldowns, nil
}

func (admin *runtimeAdmin) Requests(context.Context) ([]api.AdminRequest, error) {
	return admin.requests.list(), nil
}

func (admin *runtimeAdmin) CancelRequest(_ context.Context, id string) error {
	return admin.requests.cancel(id)
}

// adminEventSource 提供管理事件流的当前快照
type adminEventSource interface {
	Models(context.Context) ([]aistudio.Model, error)
	Status(context.Context) (api.AdminStatus, error)
	Accounts(context.Context) ([]api.AdminAccount, error)
	Cooldowns(context.Context) ([]api.AdminCooldown, error)
}

// Models 返回当前运行时完整目录与各模型可用通道
func (admin *runtimeAdmin) Models(ctx context.Context) ([]aistudio.Model, error) {
	return admin.service.catalogModels(), nil
}

func (admin *runtimeAdmin) Events(ctx context.Context) (<-chan api.AdminEvent, error) {
	return openAdminEvents(ctx, admin.lifecycle, admin.requests, admin)
}

// openAdminEvents 创建绑定进程生命周期的管理事件流
func openAdminEvents(
	ctx context.Context,
	lifecycle context.Context,
	requests *requestRegistry,
	source adminEventSource,
) (<-chan api.AdminEvent, error) {
	eventCtx, cancel := context.WithCancel(ctx)
	stopLifecycle := context.AfterFunc(lifecycle, cancel)
	subscriber := requests.subscribe(eventCtx)
	models, err := source.Models(eventCtx)
	if err != nil {
		stopLifecycle()
		cancel()
		return nil, err
	}
	status, err := source.Status(eventCtx)
	if err != nil {
		stopLifecycle()
		cancel()
		return nil, err
	}
	accounts, err := source.Accounts(eventCtx)
	if err != nil {
		stopLifecycle()
		cancel()
		return nil, err
	}
	cooldowns, err := source.Cooldowns(eventCtx)
	if err != nil {
		stopLifecycle()
		cancel()
		return nil, err
	}
	live := requests.activateSubscriber(
		subscriber,
		[]api.AdminEvent{
			{Type: "status", Data: status},
			{Type: "models", Data: map[string]any{"models": models}},
			{Type: "accounts", Data: map[string]any{"accounts": accounts}},
		},
		[]api.AdminEvent{{Type: "cooldowns", Data: cooldowns}},
	)
	events := make(chan api.AdminEvent, 16)
	go func() {
		defer stopLifecycle()
		defer cancel()
		defer close(events)
		var refreshTimer *time.Timer
		var refresh <-chan time.Time
		refreshAccounts := false
		refreshCooldowns := false
		defer func() {
			if refreshTimer != nil {
				refreshTimer.Stop()
			}
		}()
		send := func(event api.AdminEvent) bool {
			select {
			case events <- event:
				return true
			case <-eventCtx.Done():
				return false
			}
		}
		scheduleRequestRefresh := func(event api.AdminEvent) {
			request := event.Data.(api.AdminRequest)
			if request.State != "queued" {
				refreshAccounts = true
			}
			if request.State != "queued" && request.State != "running" {
				refreshCooldowns = true
			}
			if refresh != nil {
				return
			}
			if refreshTimer == nil {
				refreshTimer = time.NewTimer(adminRequestRefreshLatency)
			} else {
				refreshTimer.Reset(adminRequestRefreshLatency)
			}
			refresh = refreshTimer.C
		}
		for {
			select {
			case event, ok := <-live:
				if !ok {
					return
				}
				if !send(event) {
					return
				}
				if event.Type == "request" {
					scheduleRequestRefresh(event)
				}
			case <-refresh:
				refresh = nil
				accountsChanged := refreshAccounts
				cooldownsChanged := refreshCooldowns
				refreshAccounts = false
				refreshCooldowns = false
				updates, err := adminRequestStateUpdates(eventCtx, source, accountsChanged, cooldownsChanged)
				if err != nil {
					return
				}
				for _, update := range updates {
					if !send(update) {
						return
					}
				}
			case <-eventCtx.Done():
				return
			}
		}
	}()
	return events, nil
}

// adminRequestStateUpdates 合并请求状态引起的管理页快照变化
func adminRequestStateUpdates(
	ctx context.Context,
	source adminEventSource,
	accountsChanged bool,
	cooldownsChanged bool,
) ([]api.AdminEvent, error) {
	status, err := source.Status(ctx)
	if err != nil {
		return nil, err
	}
	updates := []api.AdminEvent{{Type: "status", Data: status}}
	if accountsChanged {
		accounts, accountsErr := source.Accounts(ctx)
		if accountsErr != nil {
			return nil, accountsErr
		}
		updates = append(updates, api.AdminEvent{Type: "accounts", Data: map[string]any{"accounts": accounts}})
	}
	if cooldownsChanged {
		cooldowns, cooldownsErr := source.Cooldowns(ctx)
		if cooldownsErr != nil {
			return nil, cooldownsErr
		}
		updates = append(updates, api.AdminEvent{Type: "cooldowns", Data: cooldowns})
	}
	return updates, nil
}

// start 登记活动请求；经 /ultra 进入的请求标记为 Ultra 号池
func (registry *requestRegistry) start(ctx context.Context, request aistudio.GenerateRequest, cancel context.CancelFunc) {
	tracked := trackedRequest{
		request: api.AdminRequest{
			ID: request.ID, Model: request.Model, AccountID: request.AccountID,
			State: "queued", StartedAt: time.Now().UTC(), Pool: requestPoolLabel(ctx),
		},
		cancel: cancel,
	}
	registry.mu.Lock()
	registry.active[request.ID] = tracked
	registry.publishLocked(api.AdminEvent{Type: "request", Data: tracked.request})
	registry.mu.Unlock()
}

func (registry *requestRegistry) markRunning(id string, accountID string, accountLabel string) {
	registry.mu.Lock()
	tracked, exists := registry.active[id]
	if exists {
		tracked.request.AccountID = accountID
		tracked.request.AccountLabel = accountLabel
		tracked.request.State = "running"
		registry.active[id] = tracked
		registry.publishLocked(api.AdminEvent{Type: "request", Data: tracked.request})
	}
	registry.mu.Unlock()
}

// markChannel 记录请求本次尝试使用的上游通道
func (registry *requestRegistry) markChannel(id string, channel string) {
	registry.mu.Lock()
	if tracked, exists := registry.active[id]; exists {
		tracked.request.Channel = channel
		registry.active[id] = tracked
		registry.publishLocked(api.AdminEvent{Type: "request", Data: tracked.request})
	}
	registry.mu.Unlock()
}

func (registry *requestRegistry) finish(id string, state string, requestErr error) {
	registry.mu.Lock()
	tracked, exists := registry.active[id]
	if exists {
		delete(registry.active, id)
		tracked.request.State = state
		registry.publishLocked(api.AdminEvent{Type: "request", Data: tracked.request})
	}
	registry.mu.Unlock()
}

func (registry *requestRegistry) list() []api.AdminRequest {
	registry.mu.Lock()
	requests := make([]api.AdminRequest, 0, len(registry.active))
	for _, tracked := range registry.active {
		requests = append(requests, tracked.request)
	}
	registry.mu.Unlock()
	sort.Slice(requests, func(left int, right int) bool {
		return requests[left].StartedAt.Before(requests[right].StartedAt)
	})
	return requests
}

func (registry *requestRegistry) count() int {
	registry.mu.Lock()
	count := len(registry.active)
	registry.mu.Unlock()
	return count
}

func (registry *requestRegistry) cancel(id string) error {
	registry.mu.Lock()
	tracked, exists := registry.active[id]
	registry.mu.Unlock()
	if !exists {
		return &adminOperationError{
			status: http.StatusNotFound, code: "request_not_found",
			message: fmt.Sprintf("活动请求不存在: %s", id),
		}
	}
	tracked.cancel()
	return nil
}

func (registry *requestRegistry) cancelAll() {
	registry.mu.Lock()
	cancels := make([]context.CancelFunc, 0, len(registry.active))
	for _, tracked := range registry.active {
		cancels = append(cancels, tracked.cancel)
	}
	registry.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
}

func (registry *requestRegistry) log(source string, level string, message string) {
	registry.recordLog(api.AdminLog{Source: source, Level: level, Message: message, Event: "runtime.message"})
}

// logDurable 与 log 相同，但进入独立的小队列、由写入协程优先写出：普通日志占满控制台队列时也不丢，调用方协程也不阻塞
// （独立队列满时同样丢弃并计数）。只用于很少出现或已限频、又必须留在运行日志里的行
// （如“额度 429 详情未识别”，同一种 10 分钟只写一条，丢了要再等 10 分钟）
func (registry *requestRegistry) logDurable(source string, level string, message string) {
	registry.recordLogWith(api.AdminLog{Source: source, Level: level, Message: message, Event: "runtime.message"}, true)
}

// recordLog 将同一结构化事件发布到管理页面与控制台；控制台队列满时丢弃并计数（见 writeConsole）
func (registry *requestRegistry) recordLog(entry api.AdminLog) {
	registry.recordLogWith(entry, false)
}

// recordLogWith 为 recordLog 的实现：durable 为真时进入 consoleDurable 队列。从不阻塞：队列满时丢弃并计数；
// 写入协程开始收尾（生命周期结束后的关闭阶段）后在当前协程直接写运行日志。
// 判断与入队都在 mu 内（发送不阻塞），与写入协程置位 consoleStopped 互斥：不会在队列写空后才入队、无人再读
func (registry *requestRegistry) recordLogWith(entry api.AdminLog, durable bool) {
	entry.Time = time.Now().UTC()
	registry.mu.Lock()
	registry.logs = append(registry.logs, entry)
	if len(registry.logs) >= adminLogCompactAt {
		copy(registry.logs, registry.logs[len(registry.logs)-adminLogRetain:])
		registry.logs = registry.logs[:adminLogRetain]
	}
	registry.publishLocked(api.AdminEvent{Type: "log", Data: entry})
	stopped := registry.consoleStopped.Load()
	if !stopped {
		queue := registry.console
		if durable {
			queue = registry.consoleDurable
		}
		select {
		case queue <- entry:
		default:
			registry.consoleDropped.Add(1)
		}
	}
	registry.mu.Unlock()
	if stopped {
		registry.writeConsoleEntry(context.Background(), entry)
	}
}

// writeConsole 把控制台队列里的日志写进运行日志，consoleDurable 优先；每 consoleDropReportInterval 检查一次丢弃计数，
// 有丢弃时汇总记一行 WARN “控制台队列已满，丢弃 N 条日志”（汇总行本身不经过队列，也不进管理页面）。
// ctx 结束时先在 mu 内置位 consoleStopped（之后的日志由调用方直接写），再写完两个队列里剩下的日志、汇总丢弃数，
// 最后关闭 consoleDone 后退出
func (registry *requestRegistry) writeConsole(ctx context.Context) {
	if registry.consoleDone != nil {
		defer close(registry.consoleDone)
	}
	interval := consoleDropReportInterval
	if registry.dropReportInterval > 0 {
		interval = registry.dropReportInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case entry := <-registry.consoleDurable:
			registry.writeConsoleEntry(ctx, entry)
			continue
		default:
		}
		select {
		case entry := <-registry.consoleDurable:
			registry.writeConsoleEntry(ctx, entry)
		case entry := <-registry.console:
			registry.writeConsoleEntry(ctx, entry)
		case <-ticker.C:
			registry.reportConsoleDrops(ctx)
		case <-ctx.Done():
			registry.mu.Lock()
			registry.consoleStopped.Store(true)
			registry.mu.Unlock()
			// 关闭阶段处理器仍可用：用不会取消的上下文写完剩下的日志
			flushCtx := context.WithoutCancel(ctx)
			for drained := false; !drained; {
				select {
				case entry := <-registry.consoleDurable:
					registry.writeConsoleEntry(flushCtx, entry)
				case entry := <-registry.console:
					registry.writeConsoleEntry(flushCtx, entry)
				default:
					drained = true
				}
			}
			registry.reportConsoleDrops(flushCtx)
			return
		}
	}
}

// reportConsoleDrops 在有丢弃时汇总记一行 WARN 并清零计数
func (registry *requestRegistry) reportConsoleDrops(ctx context.Context) {
	dropped := registry.consoleDropped.Swap(0)
	if dropped <= 0 {
		return
	}
	registry.writeConsoleEntry(ctx, api.AdminLog{
		Time: time.Now().UTC(), Source: "service", Level: "WARN", Event: "runtime.message",
		Message: fmt.Sprintf("控制台队列已满，丢弃 %d 条日志 | 丢弃=%d | 管理页面只保留最近 %d 条，计数以用量账本为准", dropped, dropped, adminLogRetain),
	})
}

// waitConsole 等写入协程收尾完成，最多等 timeout：进程退出前调用，保证队列里剩下的日志与丢弃汇总写出
func (registry *requestRegistry) waitConsole(timeout time.Duration) {
	if registry == nil || registry.consoleDone == nil {
		return
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-registry.consoleDone:
	case <-timer.C:
	}
}

// writeConsoleEntry 把一条日志写进运行日志（consoleHandler，未注入时为 slog 默认处理器；处理器可并发调用）
func (registry *requestRegistry) writeConsoleEntry(ctx context.Context, entry api.AdminLog) {
	level := slog.LevelInfo
	switch strings.ToUpper(entry.Level) {
	case "ERROR":
		level = slog.LevelError
	case "WARN":
		level = slog.LevelWarn
	}
	record := slog.NewRecord(entry.Time, level, entry.Message, 0)
	record.AddAttrs(slog.String("event", entry.Event), slog.String("source", entry.Source))
	if entry.Request != nil {
		record.AddAttrs(slog.Any("request", entry.Request))
	}
	handler := registry.consoleHandler
	if handler == nil {
		handler = slog.Default().Handler()
	}
	_ = handler.Handle(ctx, record)
}

func (registry *requestRegistry) clearLogs() {
	registry.mu.Lock()
	registry.logs = registry.logs[:0]
	registry.mu.Unlock()
}

// newEventSubscriber 创建管理页有界事件队列
func newEventSubscriber(ctx context.Context) *eventSubscriber {
	return &eventSubscriber{
		ctx: ctx, events: make(chan api.AdminEvent, 16), wake: make(chan struct{}, 1),
		pending: make([]api.AdminEvent, 0, 256),
	}
}

func (subscriber *eventSubscriber) enqueue(event api.AdminEvent) {
	subscriber.mu.Lock()
	subscriber.enqueueLocked(event)
	subscriber.mu.Unlock()
	subscriber.notify()
}

func (subscriber *eventSubscriber) enqueueLocked(event api.AdminEvent) {
	switch event.Type {
	case "status", "models", "accounts", "cooldowns":
		for index := len(subscriber.pending) - 1; index >= 0; index-- {
			if subscriber.pending[index].Type == event.Type {
				subscriber.pending[index] = event
				return
			}
		}
	case "request":
		request := event.Data.(api.AdminRequest)
		for index := len(subscriber.pending) - 1; index >= 0; index-- {
			pending := subscriber.pending[index]
			if pending.Type == "request" && pending.Data.(api.AdminRequest).ID == request.ID {
				subscriber.pending[index] = event
				return
			}
		}
		subscriber.pendingRequests++
	case "log":
		subscriber.pendingLogs++
	}
	subscriber.pending = append(subscriber.pending, event)
	if subscriber.pendingLogs >= adminLogCompactAt {
		subscriber.trimPendingLocked("log", adminLogRetain)
	}
	if subscriber.pendingRequests >= adminRequestCompactAt {
		subscriber.trimPendingLocked("request", adminRequestRetain)
	}
}

func (subscriber *eventSubscriber) trimPendingLocked(eventType string, retain int) {
	count := subscriber.pendingLogs
	if eventType == "request" {
		count = subscriber.pendingRequests
	}
	drop := count - retain
	compacted := subscriber.pending[:0]
	for _, event := range subscriber.pending {
		if event.Type == eventType && drop > 0 {
			drop--
			continue
		}
		compacted = append(compacted, event)
	}
	subscriber.pending = compacted
	if eventType == "request" {
		subscriber.pendingRequests = retain
	} else {
		subscriber.pendingLogs = retain
	}
}

func (subscriber *eventSubscriber) activate(initial []api.AdminEvent) {
	subscriber.mu.Lock()
	buffered := append([]api.AdminEvent(nil), subscriber.pending...)
	subscriber.pending = subscriber.pending[:0]
	subscriber.pendingLogs = 0
	subscriber.pendingRequests = 0
	for _, event := range initial {
		subscriber.enqueueLocked(event)
	}
	for _, event := range buffered {
		if event.Type != "log" {
			subscriber.enqueueLocked(event)
		}
	}
	subscriber.mu.Unlock()
	subscriber.notify()
}

func (subscriber *eventSubscriber) notify() {
	select {
	case subscriber.wake <- struct{}{}:
	default:
	}
}

func (subscriber *eventSubscriber) next() (api.AdminEvent, bool) {
	subscriber.mu.Lock()
	defer subscriber.mu.Unlock()
	if len(subscriber.pending) == 0 {
		return api.AdminEvent{}, false
	}
	event := subscriber.pending[0]
	subscriber.pending[0] = api.AdminEvent{}
	subscriber.pending = subscriber.pending[1:]
	if event.Type == "log" {
		subscriber.pendingLogs--
	}
	if event.Type == "request" {
		subscriber.pendingRequests--
	}
	if len(subscriber.pending) == 0 {
		subscriber.pending = nil
	}
	return event, true
}

func (subscriber *eventSubscriber) run() {
	defer close(subscriber.events)
	for {
		if event, ok := subscriber.next(); ok {
			select {
			case subscriber.events <- event:
				continue
			case <-subscriber.ctx.Done():
				return
			}
		}
		select {
		case <-subscriber.wake:
		case <-subscriber.ctx.Done():
			return
		}
	}
}

func (registry *requestRegistry) subscribe(ctx context.Context) *eventSubscriber {
	subscriber := newEventSubscriber(ctx)
	registry.mu.Lock()
	registry.subscribers[subscriber] = struct{}{}
	registry.mu.Unlock()
	go func() {
		<-ctx.Done()
		registry.mu.Lock()
		delete(registry.subscribers, subscriber)
		registry.mu.Unlock()
	}()
	return subscriber
}

// activateSubscriber 原子衔接日志请求快照与实时事件
func (registry *requestRegistry) activateSubscriber(
	subscriber *eventSubscriber,
	prefix []api.AdminEvent,
	suffix []api.AdminEvent,
) <-chan api.AdminEvent {
	registry.mu.Lock()
	initialLogs := registry.logs
	if len(initialLogs) > adminLogInitialEvents {
		initialLogs = initialLogs[len(initialLogs)-adminLogInitialEvents:]
	}
	initial := make([]api.AdminEvent, 0, len(prefix)+len(initialLogs)+len(suffix)+len(registry.active))
	initial = append(initial, prefix...)
	for _, entry := range initialLogs {
		initial = append(initial, api.AdminEvent{Type: "log", Data: entry})
	}
	initial = append(initial, suffix...)
	requests := make([]api.AdminRequest, 0, len(registry.active))
	for _, tracked := range registry.active {
		requests = append(requests, tracked.request)
	}
	sort.Slice(requests, func(left int, right int) bool {
		return requests[left].StartedAt.Before(requests[right].StartedAt)
	})
	for _, request := range requests {
		initial = append(initial, api.AdminEvent{Type: "request", Data: request})
	}
	subscriber.activate(initial)
	registry.mu.Unlock()
	go subscriber.run()
	return subscriber.events
}

func (registry *requestRegistry) publishLocked(event api.AdminEvent) {
	for subscriber := range registry.subscribers {
		subscriber.enqueue(event)
	}
}

// publish 向管理页订阅者发布增量事件
func (registry *requestRegistry) publish(event api.AdminEvent) {
	registry.mu.Lock()
	registry.publishLocked(event)
	registry.mu.Unlock()
}

func buildVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok || info.Main.Version == "" {
		return "(devel)"
	}
	return info.Main.Version
}

func runtimeConfigDTO(cfg config.Config) api.RuntimeConfig {
	media := cfg.Media.Normalized()
	streamPlaygroundModels := append([]string{}, cfg.StreamPlaygroundModels...)
	primaryModels := append([]string{}, cfg.PrimaryModels...)
	ultraPrimaryModels := append([]string{}, cfg.UltraPrimaryModels...)
	return api.RuntimeConfig{
		AuthStates: cfg.AuthStates, ListenAddr: cfg.ListenAddr, APIKey: cfg.ProxyAPIKey,
		ActiveListenAddr: cfg.ListenAddr, ActiveAPIKey: cfg.ProxyAPIKey,
		Proxy: cfg.Proxy, InitTimeout: cfg.InitTimeout.String(), RequestTimeout: cfg.RequestTimeout.String(),
		FirstEventTimeout: cfg.FirstEventTimeout.String(), WarmWorkerLimit: cfg.WarmWorkerLimit, MaxActiveWorkers: cfg.MaxActiveWorkers,
		WarmStartupConcurrency: cfg.WarmStartupConcurrency,
		PerAccountConcurrency:  cfg.PerAccountConcurrency,
		AccountRPMLimit:        &cfg.AccountRPMLimit,
		RoutingStrategy:        cfg.RoutingStrategy,
		UpstreamChannels:       cfg.UpstreamChannels,
		TemporaryChat:          cfg.TemporaryChat,
		IgnoreClientSeed:       cfg.IgnoreClientSeed,
		RepeatPromptNonce:      cfg.RepeatPromptNonce,
		MinOutputTokens:        cfg.MinOutputTokens,
		WAABackend:             cfg.WAABackend,
		DowngradeGuard:         downgradeGuardToAPI(cfg.DowngradeGuard),
		UltraExclusive:         &cfg.UltraExclusive,
		UltraIncludePro:        &cfg.UltraIncludePro,
		UltraWarmWorkerLimit:   &cfg.UltraWarmWorkerLimit,
		UltraMaxActiveWorkers:  &cfg.UltraMaxActiveWorkers,
		StreamPlaygroundModels: &streamPlaygroundModels,
		PrimaryModels:          &primaryModels,
		PrimaryQueueWait:       cfg.PrimaryQueueWait.String(),
		UltraPrimaryModels:     &ultraPrimaryModels,
		UltraPrimaryQueueWait:  cfg.UltraPrimaryQueueWait.String(),
		ProxyPool:              proxyPoolToAPI(cfg.ProxyPool),
		MediaOutput:            media.Output,
		MediaBaseURL:           &media.BaseURL,
		MediaRetention:         config.FormatMediaRetention(media.Retention),
		MediaMaxSize:           config.FormatMediaSize(media.MaxBytes),
	}
}

// mediaSettingsFromInput 合并页面提交的媒体设置：没有提交的字段沿用 current；输出方式不区分大小写，服务地址去掉首尾空白与末尾的 /
func mediaSettingsFromInput(current config.MediaSettings, value api.RuntimeConfig) (config.MediaSettings, error) {
	media := current
	if output := strings.TrimSpace(value.MediaOutput); output != "" {
		media.Output = strings.ToLower(output)
	}
	if value.MediaBaseURL != nil {
		media.BaseURL = *value.MediaBaseURL
	}
	if retention := strings.TrimSpace(value.MediaRetention); retention != "" {
		parsed, err := config.ParseMediaRetention(retention)
		if err != nil {
			return config.MediaSettings{}, err
		}
		media.Retention = parsed
	}
	if size := strings.TrimSpace(value.MediaMaxSize); size != "" {
		parsed, err := config.ParseMediaSize(size)
		if err != nil {
			return config.MediaSettings{}, err
		}
		media.MaxBytes = parsed
	}
	media = media.Normalized()
	return media, media.Validate()
}

var _ api.AdminService = (*runtimeAdmin)(nil)
