package app

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/api"
	"github.com/Mag1cFall/AIStudio2API/internal/config"
	"github.com/Mag1cFall/AIStudio2API/internal/mediastore"
	"github.com/Mag1cFall/AIStudio2API/internal/proxyrelay"
	"github.com/Mag1cFall/AIStudio2API/internal/requestdb"
)

// managedService 表示可整体替换的生成服务
type managedService interface {
	aistudio.Service
	State() string
}

// runtimeGeneration 保存同一份配置装配的完整运行时
type runtimeGeneration struct {
	service         managedService
	admin           api.AdminService
	config          config.Config
	lifecycleCancel context.CancelFunc
	closeRuntime    func() error
}

// runtimeFactory 创建一个完整生成服务实例
type runtimeFactory func(context.Context, context.Context, config.Config, *requestRegistry) (*runtimeGeneration, error)

// cancelLifecycle 取消当前生成服务实例的全部后台操作
func (generation *runtimeGeneration) cancelLifecycle() {
	generation.lifecycleCancel()
}

// Close 取消当前生成服务实例并释放运行时
func (generation *runtimeGeneration) Close() error {
	generation.cancelLifecycle()
	return generation.closeRuntime()
}

// dataConfigOverrides 保存当前进程的命令行生成服务配置覆盖
type dataConfigOverrides struct {
	authStates *string
	proxy      *string
}

// Apply 将命令行覆盖应用到新实例配置
func (overrides dataConfigOverrides) Apply(cfg *config.Config) {
	if overrides.authStates != nil {
		cfg.AuthStates = *overrides.authStates
	}
	if overrides.proxy != nil {
		cfg.Proxy = *overrides.proxy
	}
}

// runtimeManager 在固定管理监听器内切换完整生成服务
type runtimeManager struct {
	lifecycle        context.Context
	configPath       string
	activeManagement config.Config
	overrides        dataConfigOverrides
	requests         *requestRegistry
	factory          runtimeFactory
	startMu          sync.Mutex
	mu               sync.RWMutex
	current          *runtimeGeneration
	startCancel      context.CancelFunc
	apiKey           *apiKeyHolder
	intent           *serviceIntent
	// ultraExclusive 为生效的 ULTRA_EXCLUSIVE：公开 API 按它决定普通路径的请求能否使用 Ultra 号池的账户，保存配置后立即生效，
	// 启动生成服务时按新读取的配置同步
	ultraExclusive atomic.Bool
	// liveConfigMu 让配置热更新逐次进行：applyLiveConfig 在锁外读取 .env，两次保存并发时，先读到旧文件的一次可能最后应用，
	// 账户池里的 ULTRA_INCLUDE_PRO、current.config 与 .env 就会不一致。逐次进行后，最后一次应用读到的一定是最后写入的文件
	liveConfigMu sync.Mutex
	// shuttingDown 在进程退出时置为 true（由 mu 保护），之后不再启动生成服务
	shuttingDown bool
	// ledger 为用量账本，在开始接收请求之前设置、之后不再改变；打开失败时为 nil（方法对 nil 安全）
	ledger *requestdb.Store
	// proxyTestMu 让“测试代理”逐次进行（见 proxy_probe.go）
	proxyTestMu sync.Mutex
	// probeProxy 经指定代理访问一次出口 IP 服务，nil 时为 probeProxyExit；测试替换
	probeProxy func(context.Context, string) api.ProxyTestProbe
	// proxyRelays 为“测试代理”申请本机中转的注册表，nil 时用包级默认注册表；测试注入
	proxyRelays *proxyrelay.Registry
	// proxyPool 为进程级 IP 池（见 proxy_pool.go）：端口占用对应进程里活着的浏览器，与生成服务实例无关，重建生成服务时交给新实例；
	// 测试直接构造的管理器为 nil（方法对 nil 安全）
	proxyPool *proxyPool
	// media 为生效的媒体设置（MEDIA_*，见 media.go）：与 ultraExclusive 一样是进程级设置，保存配置后立即生效，
	// 启动生成服务时按新读取的配置同步；为 nil 时按默认设置
	media atomic.Pointer[config.MediaSettings]
	// mediaCleaner 定期清理媒体目录，由 runCommand 启动；测试直接构造的管理器为 nil（Kick 对 nil 安全）
	mediaCleaner *mediastore.Cleaner
}

// newRuntimeManager 创建进程级管理器与初始生成服务。
// launchCtx 只用于装配初始生成服务（首次运行会下载 Camoufox），收到退出信号时可以中断；ctx 为运行时生命周期
func newRuntimeManager(
	launchCtx context.Context,
	ctx context.Context,
	configPath string,
	cfg config.Config,
	overrides dataConfigOverrides,
) (*runtimeManager, error) {
	requests := newRequestRegistry(ctx)
	// 本机代理中转（只服务 Camoufox）的日志写进管理页与控制台；注册表是进程级的，多次构造 manager 时原子替换
	proxyrelay.SetLogger(func(level, message string) { requests.log("service", level, message) })
	manager := &runtimeManager{
		lifecycle: ctx, configPath: configPath, activeManagement: cfg,
		overrides: overrides, requests: requests, proxyPool: newProcessProxyPool(configPath, requests),
		apiKey: newAPIKeyHolder(cfg.ProxyAPIKey),
		intent: &serviceIntent{},
	}
	// 每个生成服务实例都用同一个进程级 IP 池；runtimeFactory 的签名不变（测试替换 factory 时不涉及 IP 池）
	manager.factory = func(
		launchCtx context.Context, parentLifecycle context.Context, cfg config.Config, requests *requestRegistry,
	) (*runtimeGeneration, error) {
		return buildRuntimeGeneration(launchCtx, parentLifecycle, cfg, requests, manager.proxyPool)
	}
	manager.ultraExclusive.Store(cfg.UltraExclusive)
	media := cfg.Media.Normalized()
	manager.media.Store(&media)
	manager.mediaCleaner = newMediaCleaner(manager)
	generation, err := manager.factory(launchCtx, ctx, cfg, requests)
	if err != nil {
		return nil, err
	}
	manager.current = generation
	go manager.superviseService()
	return manager, nil
}

// newProcessProxyPool 创建进程级 IP 池：记录文件为配置文件所在目录下的 runtime/proxy-pool.json（生产上为工作目录下的 runtime/，
// 与 requests.db 同目录），跨进程端口锁与账户 WAA runtime 租约同目录；锁目录不可用时 WARN 并只在本进程内互斥
func newProcessProxyPool(configPath string, requests *requestRegistry) *proxyPool {
	lockDir, err := aistudio.ProxyPortLockDirectory()
	if err != nil {
		requests.log("service", "WARN", "IP 池跨进程端口锁不可用，多个进程使用同一端口段时会重复分配 | 错误="+err.Error())
		lockDir = ""
	}
	return newProxyPool(proxyPoolOptions{
		path:     filepath.Join(filepath.Dir(configPath), "runtime", proxyPoolFileName),
		lockDir:  lockDir,
		requests: requests,
	})
}

// buildRuntimeGeneration 从配置快照创建账户池、Worker 与协议运行时；proxyPool 为进程级 IP 池（可以为 nil）
func buildRuntimeGeneration(
	launchCtx context.Context,
	parentLifecycle context.Context,
	cfg config.Config,
	requests *requestRegistry,
	proxyPool *proxyPool,
) (*runtimeGeneration, error) {
	lifecycle, lifecycleCancel := context.WithCancel(parentLifecycle)
	service, admin, closeRuntime, err := newRuntime(launchCtx, lifecycle, cfg, requests, proxyPool)
	if err != nil {
		lifecycleCancel()
		return nil, err
	}
	return &runtimeGeneration{
		service: service, admin: admin, config: cfg,
		lifecycleCancel: lifecycleCancel, closeRuntime: closeRuntime,
	}, nil
}

// StartService 从最新配置创建并启动新生成服务（用户启动、自动启动与应用配置）
func (manager *runtimeManager) StartService(ctx context.Context) (api.AdminStatus, error) {
	return manager.startService(ctx, true)
}

// startService 启动生成服务。user 为 false 时是监督器的自动重启：不改变用户期望的运行状态，
// 用户已经停止服务时直接放弃。开始启动之后用户按了停止（stops 变化）时同样放弃
func (manager *runtimeManager) startService(ctx context.Context, user bool) (api.AdminStatus, error) {
	if user {
		manager.intent.running.Store(true)
	}
	stops := manager.intent.stops.Load()
	manager.startMu.Lock()
	defer manager.startMu.Unlock()

	manager.mu.Lock()
	// 与 StopService 在同一把锁内检查：要么停止看到 startCancel 并取消启动，要么启动看到停止并放弃
	if manager.shuttingDown || manager.intent.stops.Load() != stops || !manager.intent.running.Load() {
		manager.mu.Unlock()
		return manager.Status(ctx)
	}
	current := manager.current
	if current.service.State() != "STOPPED" {
		status, err := current.admin.StartService(ctx)
		manager.mu.Unlock()
		return status, err
	}
	if _, err := current.admin.StopService(ctx); err != nil {
		manager.mu.Unlock()
		return api.AdminStatus{}, err
	}
	launchCtx, launchCancel := context.WithCancel(manager.lifecycle)
	manager.startCancel = launchCancel
	manager.mu.Unlock()

	cfg, err := config.Load(manager.configPath)
	if err != nil {
		manager.finishStart(launchCancel)
		return api.AdminStatus{}, err
	}
	manager.overrides.Apply(&cfg)
	if err := cfg.Validate(); err != nil {
		manager.finishStart(launchCancel)
		return api.AdminStatus{}, err
	}

	next, err := manager.factory(launchCtx, manager.lifecycle, cfg, manager.requests)
	if err != nil {
		manager.finishStart(launchCancel)
		return api.AdminStatus{}, err
	}
	if launchCtx.Err() != nil {
		manager.finishStart(launchCancel)
		_ = next.Close()
		return manager.Status(ctx)
	}

	manager.mu.Lock()
	current = manager.current
	manager.current = next
	manager.mu.Unlock()
	// 独占设置与媒体设置与新生成服务读到的配置一致：手动改过 .env 后停止再启动时同样生效
	manager.applyUltraExclusive(cfg.UltraExclusive)
	manager.applyMediaSettings(cfg.Media)

	current.cancelLifecycle()
	status, startErr := next.admin.StartService(launchCtx)
	manager.finishStart(launchCancel)
	if err := current.Close(); err != nil {
		manager.requests.log("service", "WARN", "旧生成服务关闭失败 | 错误="+err.Error())
	}
	return status, startErr
}

// beginShutdown 在进程开始退出时调用：取消正在进行的生成服务启动，之后不再启动（监督器的自动重启也不会），
// 不改变用户期望的运行状态
func (manager *runtimeManager) beginShutdown() {
	manager.mu.Lock()
	manager.shuttingDown = true
	cancel := manager.startCancel
	manager.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// finishStart 清理本轮生成服务启动取消句柄
func (manager *runtimeManager) finishStart(cancel context.CancelFunc) {
	manager.mu.Lock()
	manager.startCancel = nil
	manager.mu.Unlock()
	cancel()
}

// StopService 停止当前生成服务并保持管理监听器运行
func (manager *runtimeManager) StopService(ctx context.Context) (api.AdminStatus, error) {
	manager.mu.Lock()
	manager.intent.running.Store(false)
	manager.intent.stops.Add(1)
	cancel := manager.startCancel
	current := manager.current
	manager.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return current.admin.StopService(ctx)
}

// stopCurrent 停止当前生成服务但不改变用户期望的运行状态（应用配置时的重启）
func (manager *runtimeManager) stopCurrent(ctx context.Context) (api.AdminStatus, error) {
	manager.mu.RLock()
	cancel := manager.startCancel
	current := manager.current
	manager.mu.RUnlock()
	if cancel != nil {
		cancel()
	}
	return current.admin.StopService(ctx)
}

// Close 释放当前生成服务；在锁外关闭，关闭期间仍在收尾的请求可以照常记录访问日志。最后写出 IP 池记录
func (manager *runtimeManager) Close() error {
	err := manager.generation().Close()
	manager.proxyPool.flush()
	return err
}

// generation 返回当前生成服务实例的快照。
//
// 只在读取指针时持有 mu，调用方在锁外使用快照：原先各方法在整个调用期间持有读锁，
// 调用链里再次读锁（例如生成请求写访问日志时回调 RecordAccessStart）会与排队的写锁
// （保存配置、启停服务）互相等待，整个服务永久卡死；登录、验证等长操作持锁时，
// 保存配置也会让所有新请求排在写锁后面。替换实例只交换指针，旧实例由 StartService 关闭，
// 仍在使用旧快照的调用会收到服务已停止的错误
func (manager *runtimeManager) generation() *runtimeGeneration {
	manager.mu.RLock()
	defer manager.mu.RUnlock()
	return manager.current
}

// Models 返回当前生成服务模型
func (manager *runtimeManager) Models(ctx context.Context) ([]aistudio.Model, error) {
	return manager.generation().service.Models(ctx)
}

// CountTokens 由当前生成服务计数
func (manager *runtimeManager) CountTokens(ctx context.Context, request aistudio.TokenCountRequest) (aistudio.TokenCount, error) {
	return manager.generation().service.CountTokens(ctx, request)
}

// Generate 由当前生成服务生成事件流
func (manager *runtimeManager) Generate(ctx context.Context, request aistudio.GenerateRequest) (<-chan aistudio.Event, error) {
	var current any = manager.generation().service
	starter, ok := current.(interface {
		startGenerate(context.Context, aistudio.GenerateRequest) (<-chan aistudio.Event, func() error, error)
	})
	if !ok {
		return current.(managedService).Generate(ctx, request)
	}
	events, wait, err := starter.startGenerate(ctx, request)
	if err != nil || wait == nil {
		return events, err
	}
	// 降级判定（流式严格模式）要等到判定出结果才返回
	if err := wait(); err != nil {
		return nil, err
	}
	return events, nil
}

// StreamStartWait 由当前生成服务给出流式请求写出响应头前等待首个事件的下限（请求号池对应列表里的主用模型为排队上限加余量，
// 见 primary_models.go）；生成服务不支持时为 0。断言用 api.StreamStartWaitProvider，签名与 API 层的接口始终一致
func (manager *runtimeManager) StreamStartWait(ctx context.Context, model string) time.Duration {
	provider, ok := manager.generation().service.(api.StreamStartWaitProvider)
	if !ok {
		return 0
	}
	return provider.StreamStartWait(ctx, model)
}

// 流式首事件等待按接口类型断言取得，签名不一致不会编译报错、只会静默返回 0：两个实现方各加一条编译期断言
var (
	_ api.StreamStartWaitProvider = (*trackedService)(nil)
	_ api.StreamStartWaitProvider = (*runtimeManager)(nil)
)

// GenerateVideo 由当前生成服务创建视频任务
func (manager *runtimeManager) GenerateVideo(ctx context.Context, request aistudio.VideoRequest) (aistudio.VideoOperation, error) {
	service, ok := manager.generation().service.(aistudio.VideoService)
	if !ok {
		return aistudio.VideoOperation{}, fmt.Errorf("video service 不可用")
	}
	return service.GenerateVideo(ctx, request)
}

// GetGenerateVideoOperation 由当前生成服务读取视频任务
func (manager *runtimeManager) GetGenerateVideoOperation(ctx context.Context, id string) (aistudio.VideoOperation, error) {
	service, ok := manager.generation().service.(aistudio.VideoService)
	if !ok {
		return aistudio.VideoOperation{}, fmt.Errorf("video service 不可用")
	}
	return service.GetGenerateVideoOperation(ctx, id)
}

// DownloadFile 由当前生成服务下载文件
func (manager *runtimeManager) DownloadFile(ctx context.Context, id string) (aistudio.MediaStream, error) {
	service, ok := manager.generation().service.(aistudio.VideoService)
	if !ok {
		return aistudio.MediaStream{}, fmt.Errorf("video service 不可用")
	}
	return service.DownloadFile(ctx, id)
}

// UploadFile 由当前生成服务上传文件
func (manager *runtimeManager) UploadFile(ctx context.Context, request aistudio.UploadRequest) (aistudio.FileRef, error) {
	service, ok := manager.generation().service.(aistudio.FileService)
	if !ok {
		return aistudio.FileRef{}, fmt.Errorf("file service 不可用")
	}
	return service.UploadFile(ctx, request)
}

// FileMetadata 由当前生成服务读取文件元数据
func (manager *runtimeManager) FileMetadata(ctx context.Context, id string) (aistudio.FileMetadata, error) {
	service, ok := manager.generation().service.(aistudio.FileService)
	if !ok {
		return aistudio.FileMetadata{}, fmt.Errorf("file service 不可用")
	}
	return service.FileMetadata(ctx, id)
}

// DeleteFile 由当前生成服务删除文件
func (manager *runtimeManager) DeleteFile(ctx context.Context, id string) error {
	service, ok := manager.generation().service.(aistudio.FileService)
	if !ok {
		return fmt.Errorf("file service 不可用")
	}
	return service.DeleteFile(ctx, id)
}

// OpenBidi 由当前生成服务创建实时会话
func (manager *runtimeManager) OpenBidi(ctx context.Context, request aistudio.BidiRequest) (*aistudio.BidiSession, error) {
	service, ok := manager.generation().service.(aistudio.BidiService)
	if !ok {
		return nil, fmt.Errorf("bidi service 不可用")
	}
	return service.OpenBidi(ctx, request)
}

// Transcribe 由当前生成服务执行音频转录
func (manager *runtimeManager) Transcribe(ctx context.Context, request aistudio.TranscriptionRequest) (aistudio.TranscriptionResult, error) {
	service, ok := manager.generation().service.(aistudio.TranscriptionService)
	if !ok {
		return aistudio.TranscriptionResult{}, fmt.Errorf("transcription service 不可用")
	}
	return service.Transcribe(ctx, request)
}

// Status 返回当前生成服务状态
func (manager *runtimeManager) Status(ctx context.Context) (api.AdminStatus, error) {
	return manager.generation().admin.Status(ctx)
}

// Accounts 返回当前生成服务账户
func (manager *runtimeManager) Accounts(ctx context.Context) ([]api.AdminAccount, error) {
	return manager.generation().admin.Accounts(ctx)
}

// CreateAccount 在当前生成服务创建账户
func (manager *runtimeManager) CreateAccount(ctx context.Context, input api.AccountCreateInput) (api.AdminAccount, error) {
	return manager.generation().admin.CreateAccount(ctx, input)
}

// ChromeImportProfiles 返回当前生成服务可导入的 Chrome 账号
func (manager *runtimeManager) ChromeImportProfiles(ctx context.Context) ([]api.ChromeImportProfile, error) {
	return manager.generation().admin.ChromeImportProfiles(ctx)
}

// ImportChromeAccounts 在当前生成服务批量导入 Chrome 账号
func (manager *runtimeManager) ImportChromeAccounts(ctx context.Context, input api.ChromeImportInput) ([]api.AdminAccount, error) {
	return manager.generation().admin.ImportChromeAccounts(ctx, input)
}

// UpdateAccount 在当前生成服务更新账户
func (manager *runtimeManager) UpdateAccount(ctx context.Context, id string, input api.AccountInput) (api.AdminAccount, error) {
	return manager.generation().admin.UpdateAccount(ctx, id, input)
}

// EnableAccounts 在当前生成服务批量启用账户
func (manager *runtimeManager) EnableAccounts(ctx context.Context, input api.BulkEnableInput) (api.BulkEnableResult, error) {
	admin, ok := manager.generation().admin.(bulkAccountEnabler)
	if !ok {
		return api.BulkEnableResult{}, fmt.Errorf("批量启用尚未就绪")
	}
	return admin.EnableAccounts(ctx, input)
}

// DeleteAccounts 在当前生成服务批量删除账户；生成服务停止时管理端照常删除（与单个删除、批量启用一致）
func (manager *runtimeManager) DeleteAccounts(ctx context.Context, input api.BulkDeleteInput) (api.BulkDeleteResult, error) {
	admin, ok := manager.generation().admin.(bulkAccountDeleter)
	if !ok {
		return api.BulkDeleteResult{}, fmt.Errorf("批量删除尚未就绪")
	}
	return admin.DeleteAccounts(ctx, input)
}

// DeleteAccount 在当前生成服务删除账户
func (manager *runtimeManager) DeleteAccount(ctx context.Context, id string) error {
	return manager.generation().admin.DeleteAccount(ctx, id)
}

// LoginAccount 在当前生成服务登录账户
func (manager *runtimeManager) LoginAccount(ctx context.Context, id string) (api.AdminAccount, error) {
	return manager.generation().admin.LoginAccount(ctx, id)
}

// VerifyAccount 在当前生成服务验证账户
func (manager *runtimeManager) VerifyAccount(ctx context.Context, id string) (api.AdminAccount, error) {
	return manager.generation().admin.VerifyAccount(ctx, id)
}

// ClearLogs 清空进程级管理日志
func (manager *runtimeManager) ClearLogs(context.Context) error {
	manager.requests.clearLogs()
	return nil
}

// RuntimeConfig 返回已保存配置与进程级生效状态
func (manager *runtimeManager) RuntimeConfig(ctx context.Context) (api.RuntimeConfig, error) {
	value, err := manager.generation().admin.RuntimeConfig(ctx)
	if err != nil {
		return value, err
	}
	return manager.decorateCurrent(value), nil
}

// UpdateRuntimeConfig 保存下一次启动生成服务时使用的配置
func (manager *runtimeManager) UpdateRuntimeConfig(ctx context.Context, value api.RuntimeConfig) (api.RuntimeConfig, error) {
	updated, err := manager.generation().admin.UpdateRuntimeConfig(ctx, value)
	if err != nil {
		return updated, err
	}
	// API 密钥与 Ultra 独占设置保存后立即生效（独占设置在 applyLiveConfig 中按读取到的配置应用）；
	// Worker 数、并发、策略、超时等直接热更新；监听地址仍需重启管理进程
	manager.applyAPIKey(updated.APIKey)
	manager.applyLiveConfig()
	return manager.decorateCurrent(updated), nil
}

// decorateCurrent 在锁内读取进程级配置与当前实例配置后标记生效时机；不回调任何实例方法
func (manager *runtimeManager) decorateCurrent(value api.RuntimeConfig) api.RuntimeConfig {
	manager.mu.RLock()
	defer manager.mu.RUnlock()
	return manager.decorateRuntimeConfig(value, manager.current.config)
}

// Cooldowns 返回当前生成服务冷却状态
func (manager *runtimeManager) Cooldowns(ctx context.Context) ([]api.AdminCooldown, error) {
	return manager.generation().admin.Cooldowns(ctx)
}

// PermissionDenied 返回当前生成服务的 403 无权限账户列表
func (manager *runtimeManager) PermissionDenied(ctx context.Context) ([]api.AdminPermissionDenied, error) {
	return manager.generation().admin.PermissionDenied(ctx)
}

// ClearPermissionDenied 清除当前生成服务的 403 无权限账户记录
func (manager *runtimeManager) ClearPermissionDenied(ctx context.Context, input api.PermissionDeniedClearInput) (int, error) {
	return manager.generation().admin.ClearPermissionDenied(ctx, input)
}

// OrgDisabled 返回当前生成服务的组织停用账户列表（见 org_disabled.go）；生成服务的管理实现不支持时为空
func (manager *runtimeManager) OrgDisabled(ctx context.Context) ([]api.AdminOrgDisabled, error) {
	if admin, ok := manager.generation().admin.(api.OrgDisabledService); ok {
		return admin.OrgDisabled(ctx)
	}
	return nil, nil
}

// ClearOrgDisabled 清除当前生成服务的组织停用账户的状态，立即恢复调度
func (manager *runtimeManager) ClearOrgDisabled(ctx context.Context, input api.PermissionDeniedClearInput) (int, error) {
	if admin, ok := manager.generation().admin.(api.OrgDisabledService); ok {
		return admin.ClearOrgDisabled(ctx, input)
	}
	return 0, nil
}

// Requests 返回进程级活动请求
func (manager *runtimeManager) Requests(ctx context.Context) ([]api.AdminRequest, error) {
	return manager.generation().admin.Requests(ctx)
}

// CancelRequest 取消进程级活动请求
func (manager *runtimeManager) CancelRequest(ctx context.Context, id string) error {
	return manager.generation().admin.CancelRequest(ctx, id)
}

// Events 创建生成服务实例切换期间持续可用的管理事件流
func (manager *runtimeManager) Events(ctx context.Context) (<-chan api.AdminEvent, error) {
	return openAdminEvents(ctx, manager.lifecycle, manager.requests, manager)
}

// RecordAccessStart 记录公开 API 请求开始；会在生成调用链内被回调，不能在持锁期间调用实例方法
func (manager *runtimeManager) RecordAccessStart(entry api.AccessLog) {
	manager.generation().admin.RecordAccessStart(entry)
}

// RecordAccessLog 记录公开 API 请求结果，通过 API key 校验的 POST 请求同时写入用量账本。
// 与 RecordAccessStart 一样只取实例快照、不持锁；账本只是排队，不等待落盘
func (manager *runtimeManager) RecordAccessLog(entry api.AccessLog) {
	manager.generation().admin.RecordAccessLog(entry)
	if ledgerEntry(entry) {
		manager.ledger.Record(requestRow(entry, time.Now().UTC()))
	}
}

// requestLedger 返回交给 API 路由的账本：账本没有打开时返回 nil 接口值。
// 直接把 nil 的 *requestdb.Store 赋给接口会得到非 nil 的接口，路由会注册用量接口并在请求时解引用空指针
func (manager *runtimeManager) requestLedger() api.RequestLedger {
	if manager.ledger == nil {
		return nil
	}
	return manager.ledger
}

// decorateRuntimeConfig 标记配置所属的进程级与生成服务生效时机
func (manager *runtimeManager) decorateRuntimeConfig(value api.RuntimeConfig, active config.Config) api.RuntimeConfig {
	value.ActiveListenAddr = manager.activeManagement.ListenAddr
	value.ActiveAPIKey = manager.activeManagement.ProxyAPIKey
	value.ManagementRestartRequired = value.ListenAddr != value.ActiveListenAddr || value.APIKey != value.ActiveAPIKey
	value.ServiceRestartRequired = !sameDataConfig(value, active, manager.overrides)
	return value
}

// sameDataConfig 比较已保存配置与当前生成服务配置
func sameDataConfig(value api.RuntimeConfig, active config.Config, overrides dataConfigOverrides) bool {
	initTimeout, initErr := time.ParseDuration(value.InitTimeout)
	requestTimeout, requestErr := time.ParseDuration(value.RequestTimeout)
	firstEventTimeout, firstEventErr := time.ParseDuration(value.FirstEventTimeout)
	if initErr != nil || requestErr != nil || firstEventErr != nil {
		return false
	}
	// 主用模型的排队上限：提交时省略按生效值比较
	primaryQueueWait := active.PrimaryQueueWait
	if strings.TrimSpace(value.PrimaryQueueWait) != "" {
		parsed, err := time.ParseDuration(strings.TrimSpace(value.PrimaryQueueWait))
		if err != nil {
			return false
		}
		primaryQueueWait = parsed
	}
	// Ultra 主用模型的排队上限：同样在提交时省略按生效值比较
	ultraPrimaryQueueWait := active.UltraPrimaryQueueWait
	if strings.TrimSpace(value.UltraPrimaryQueueWait) != "" {
		parsed, err := time.ParseDuration(strings.TrimSpace(value.UltraPrimaryQueueWait))
		if err != nil {
			return false
		}
		ultraPrimaryQueueWait = parsed
	}
	// 默认代理按规范值比较（幂等、无副作用）：写法不同但上游相同时不误报需要重启
	saved := config.Config{
		AuthStates: value.AuthStates, Proxy: config.CanonicalProxy(value.Proxy),
		InitTimeout: initTimeout, RequestTimeout: requestTimeout, FirstEventTimeout: firstEventTimeout,
		WarmWorkerLimit: value.WarmWorkerLimit, MaxActiveWorkers: value.MaxActiveWorkers,
		WarmStartupConcurrency: value.WarmStartupConcurrency,
		PerAccountConcurrency:  value.PerAccountConcurrency, AccountRPMLimit: active.AccountRPMLimit, TemporaryChat: value.TemporaryChat,
		IgnoreClientSeed: value.IgnoreClientSeed, RepeatPromptNonce: value.RepeatPromptNonce,
		MinOutputTokens: value.MinOutputTokens,
		RoutingStrategy: value.RoutingStrategy, UpstreamChannels: value.UpstreamChannels,
		WAABackend:     value.WAABackend,
		DowngradeGuard: active.DowngradeGuard,
		UltraExclusive: active.UltraExclusive, UltraIncludePro: active.UltraIncludePro, UltraWarmWorkerLimit: active.UltraWarmWorkerLimit,
		UltraMaxActiveWorkers: active.UltraMaxActiveWorkers, StreamPlaygroundModels: active.StreamPlaygroundModels,
		PrimaryModels: active.PrimaryModels, PrimaryQueueWait: primaryQueueWait,
		UltraPrimaryModels: active.UltraPrimaryModels, UltraPrimaryQueueWait: ultraPrimaryQueueWait,
		ProxyPool: active.ProxyPool,
	}
	if value.DowngradeGuard != nil {
		saved.DowngradeGuard = downgradeGuardFromAPI(*value.DowngradeGuard)
	}
	// 媒体设置：提交时省略（旧版页面）的字段按生效值比较，期限与大小按含义比较
	media, err := mediaSettingsFromInput(active.Media, value)
	if err != nil {
		return false
	}
	saved.Media = media
	// IP 池：提交时省略（旧版页面）按生效值比较；地址按规范值比较，写法不同但上游相同时不误报
	if value.ProxyPool != nil {
		poolSettings, err := config.NormalizeProxyPool(proxyPoolFromAPI(*value.ProxyPool))
		if err != nil {
			return false
		}
		saved.ProxyPool = poolSettings
	}
	if value.UltraExclusive != nil {
		saved.UltraExclusive = *value.UltraExclusive
	}
	if value.UltraIncludePro != nil {
		saved.UltraIncludePro = *value.UltraIncludePro
	}
	if value.UltraWarmWorkerLimit != nil {
		saved.UltraWarmWorkerLimit = *value.UltraWarmWorkerLimit
	}
	if value.UltraMaxActiveWorkers != nil {
		saved.UltraMaxActiveWorkers = *value.UltraMaxActiveWorkers
	}
	if value.StreamPlaygroundModels != nil {
		saved.StreamPlaygroundModels = config.NormalizeModelList(*value.StreamPlaygroundModels)
	}
	if value.PrimaryModels != nil {
		saved.PrimaryModels = config.NormalizeModelList(*value.PrimaryModels)
	}
	if value.UltraPrimaryModels != nil {
		saved.UltraPrimaryModels = config.NormalizeModelList(*value.UltraPrimaryModels)
	}
	// 单账户每分钟上限：提交时省略（旧版页面）按生效值比较
	if value.AccountRPMLimit != nil {
		saved.AccountRPMLimit = *value.AccountRPMLimit
	}
	overrides.Apply(&saved)
	return saved.AuthStates == active.AuthStates && saved.Proxy == active.Proxy &&
		saved.InitTimeout == active.InitTimeout && saved.RequestTimeout == active.RequestTimeout &&
		saved.FirstEventTimeout == active.FirstEventTimeout &&
		saved.WarmWorkerLimit == active.WarmWorkerLimit && saved.MaxActiveWorkers == active.MaxActiveWorkers &&
		saved.UltraExclusive == active.UltraExclusive && saved.UltraIncludePro == active.UltraIncludePro &&
		saved.UltraWarmWorkerLimit == active.UltraWarmWorkerLimit &&
		saved.UltraMaxActiveWorkers == active.UltraMaxActiveWorkers &&
		saved.WarmStartupConcurrency == active.WarmStartupConcurrency &&
		saved.PerAccountConcurrency == active.PerAccountConcurrency && saved.AccountRPMLimit == active.AccountRPMLimit &&
		saved.TemporaryChat == active.TemporaryChat &&
		saved.IgnoreClientSeed == active.IgnoreClientSeed && saved.RepeatPromptNonce == active.RepeatPromptNonce &&
		saved.MinOutputTokens == active.MinOutputTokens &&
		saved.RoutingStrategy == active.RoutingStrategy &&
		slices.Equal(saved.UpstreamChannels, active.UpstreamChannels) && saved.WAABackend == active.WAABackend &&
		saved.DowngradeGuard.Equal(active.DowngradeGuard) &&
		slices.Equal(saved.StreamPlaygroundModels, active.StreamPlaygroundModels) &&
		slices.Equal(saved.PrimaryModels, active.PrimaryModels) && saved.PrimaryQueueWait == active.PrimaryQueueWait &&
		slices.Equal(saved.UltraPrimaryModels, active.UltraPrimaryModels) && saved.UltraPrimaryQueueWait == active.UltraPrimaryQueueWait &&
		saved.ProxyPool.Equal(active.ProxyPool) && saved.Media.Equal(active.Media)
}

var _ aistudio.Service = (*runtimeManager)(nil)
var _ aistudio.VideoService = (*runtimeManager)(nil)
var _ aistudio.FileService = (*runtimeManager)(nil)
var _ aistudio.BidiService = (*runtimeManager)(nil)
var _ aistudio.TranscriptionService = (*runtimeManager)(nil)
var _ api.AdminService = (*runtimeManager)(nil)
