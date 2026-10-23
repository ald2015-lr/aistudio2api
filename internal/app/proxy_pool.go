package app

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/api"
	"github.com/Mag1cFall/AIStudio2API/internal/config"
	"github.com/Mag1cFall/AIStudio2API/internal/proxyrelay"
)

const (
	// proxyPoolFileName 为 IP 池记录文件（runtime/ 下，与 requests.db 同目录）
	proxyPoolFileName    = "proxy-pool.json"
	proxyPoolFileVersion = 1
	// proxyPoolSaveDelay 为记录变化后合并写盘的延迟
	proxyPoolSaveDelay = 3 * time.Second
	// proxyPoolFailureWindow 为规则 A 的窗口：窗口内 proxyPoolFailureAccounts 个不同账号返回裸限流 429、其间没有生成成功时冷却端口。
	// 只计入被拒像是 IP 问题的账号（见 strikeLocked），且其中至少一个 proxyPoolProvenWithin 内在别的出口上成功过
	proxyPoolFailureWindow   = 10 * time.Minute
	proxyPoolFailureAccounts = 2
	// proxyPoolProvenWithin 为规则 A 要求的“近期在别的出口上成功过”（successes）：别处成功过、到这个端口就被拒才是 IP 的证据，
	// 只在这个端口上用过（或从没成功过）的账号被拒分不清是 IP 还是账号本身（额度用完、新号不可用）
	proxyPoolProvenWithin = 24 * time.Hour
	// proxyPoolStrikeKeep 为账户裸限流记录（proxyPoolStrike）的保留时长：超过时下一次裸 429 重新开始一串
	proxyPoolStrikeKeep = 24 * time.Hour
	// proxyPoolUpstreamFails 为规则 B：连续这么多次“代理上游失败”的启动失败、其间没有启动成功时冷却端口
	proxyPoolUpstreamFails = 2
	// 端口冷却第 n 档为 min(15m × 2^(n-1), 2h)；最近一次冷却起 24 小时内没有再冷却则档位清零
	proxyPoolCoolBase        = 15 * time.Minute
	proxyPoolCoolMax         = 2 * time.Hour
	proxyPoolLevelResetAfter = 24 * time.Hour
	// 整体故障保护：要冷却时，出错或冷却中的端口数（含本次）不少于 max(proxyPoolSystemicMin, 使用中端口数 × proxyPoolSystemicRatio) 就不冷却
	proxyPoolSystemicRatio = 0.5
	proxyPoolSystemicMin   = 5
	// proxyPoolShortageLogInterval 为端口不足、全部冷却两类 WARN 的最短间隔
	proxyPoolShortageLogInterval = time.Minute
	// proxyPoolSessionWindow 为动态 IP 两次上游失败需要落在的窗口；proxyPoolRotateInterval 为两次换会话的最短间隔
	proxyPoolSessionWindow  = 30 * time.Minute
	proxyPoolRotateInterval = 10 * time.Minute
	// proxyPoolSessionHashLen 为按账户 ID 哈希的基础会话号长度；proxyPoolMaxGeneration 为代号上限（base36 "zz"），
	// 会话号最长 12+1+2=15 个字符
	proxyPoolSessionHashLen = 12
	proxyPoolMaxGeneration  = 1295
	// proxyPoolAffinityTTL 为不在账户池里的账户记录保留的时长
	proxyPoolAffinityTTL = 30 * 24 * time.Hour
	// proxyPoolLockedRetry 为被其他进程锁住的端口再试的间隔
	proxyPoolLockedRetry = time.Minute
	// proxyPoolLockErrorRetry 为端口锁出错（打开文件数用满、锁目录暂时不可写等）后不再加锁、照常分配的时长，到期再试
	proxyPoolLockErrorRetry = 5 * time.Minute
	// proxyPoolRecheckSlack 为默认出口重新检查的定时器比到期时刻多等的时间（到期时刻本身仍在冷却或重试时间内）
	proxyPoolRecheckSlack = time.Second
	// proxyPoolWakeDelay 为端口归还后重推在等的账户（defaultWaiting）之前等的时间：这段时间里多半有 Worker 来占这个端口
	// （重启的 Worker、补齐预热），不必先把在等的账户全部挪过去、再在 Worker 占用时全部挪回来；期间的多次归还合并成一次重推
	proxyPoolWakeDelay = 30 * time.Second
	// proxyPoolSnapshotAccounts 为一次推送的账户数达到它时先扫描一次端口段（proxyPoolDefaultView），各账户的探测只读快照
	proxyPoolSnapshotAccounts = 16
	// proxyPoolCheckConcurrency、proxyPoolCheckBudget 为“检查全部端口”的并发与总期限
	proxyPoolCheckConcurrency = 10
	proxyPoolCheckBudget      = 5 * time.Minute
	// proxyPoolLoginAllowance 为容量之外登录、验证可能同时占用的浏览器数（新账户自动验证并发上限 8）
	proxyPoolLoginAllowance = 8
	// proxyPoolOverAllocWindow 为池租约数持续超过容量多久后 WARN
	proxyPoolOverAllocWindow = 5 * time.Minute
	// proxyPoolCoolingPortsLimit、proxyPoolHotPortsLimit 为状态接口列出的冷却端口与裸 429 最多的端口数
	proxyPoolCoolingPortsLimit = 20
	proxyPoolHotPortsLimit     = 5
	// proxyPoolHitsPerPort 为每个端口保留的最近裸 429 记录数（只用于状态接口的 hot_ports）
	proxyPoolHitsPerPort = 64
)

// 租约用途：worker 为 Worker 浏览器；login、verify、import 为登录、验证与 Chrome 导入（状态接口统一显示为 login）
const (
	proxyPoolKindWorker = "worker"
	proxyPoolKindLogin  = "login"
	proxyPoolKindVerify = "verify"
	proxyPoolKindImport = "import"
)

// 端口冷却的规则：rate_limited 为规则 A（不同账号的裸限流 429），upstream 为规则 B（连不上上游代理）
const (
	proxyPoolRuleRateLimited = "rate_limited"
	proxyPoolRuleUpstream    = "upstream"
)

// proxyPoolOwner 为 IP 池推送账户绑定的目标（当前生成服务的 *aistudio.AccountPool）：方法在账户池的锁内执行，不回调 IP 池
type proxyPoolOwner interface {
	SetProxyBindings(updates []aistudio.ProxyBindingUpdate) int
	UpdateProxyBindings(compute func(accountID, accountProxy string) *aistudio.ProxyBinding)
	AccountIDs() []string
}

// proxyPoolCloser 为租约登记关闭回调的浏览器（*aistudio.NativeWorker）
type proxyPoolCloser interface {
	OnClosed(hook func())
}

// proxyPoolPortLock 为跨进程端口锁（*aistudio.ProxyPortLock）
type proxyPoolPortLock interface {
	Release() error
}

// 账户池、浏览器与端口锁按这三个接口接入（契约见 aistudio 包）
var (
	_ proxyPoolOwner    = (*aistudio.AccountPool)(nil)
	_ proxyPoolCloser   = (*aistudio.NativeWorker)(nil)
	_ proxyPoolPortLock = (*aistudio.ProxyPortLock)(nil)
)

// proxyPool 为进程级 IP 池（runtimeManager 持有，经 newRuntime 交给每个生成服务实例）：静态 IP 按端口分给浏览器独占，
// 动态 IP 按账户给固定会话号；记下每个浏览器实际使用的出口，经当前账户池（owner）按 ID 推送账户绑定。方法对 nil 安全。
// 锁序：proxyPool.mu 在前、账户池的锁在后；mu 内只做 map 操作、字符串拼接、非阻塞的端口文件锁与批量推送，日志与回调在锁外
type proxyPool struct {
	mu      sync.Mutex
	path    string // runtime/proxy-pool.json；为空时只在内存
	lockDir string // 跨进程端口锁目录；为空时不跨进程加锁（测试）
	// lockDegraded 为端口锁出错后还没有再成功加过锁（这一段只 WARN 一次）；lockFailedAt 为最近一次出错的时间，
	// 之后 proxyPoolLockErrorRetry 内不加锁、照常分配，到期再试，恢复时给已持有的端口补锁
	lockDegraded bool
	lockFailedAt time.Time
	requests     *requestRegistry
	now          func() time.Time
	random       io.Reader
	tryLock      func(directory, endpointKey string, port int) (proxyPoolPortLock, bool, error)
	relayLimit   func() int
	owner        proxyPoolOwner // 推送目标：当前生成服务的账户池（attach 切换）
	settings     proxyPoolSettings
	version      uint64
	// ports 为出现过持有或记录的端口：configure 永不重建、永不清 holders；portsKey 为端口记录所属的 EndpointKey
	ports     map[int]*proxyPoolPort
	portsKey  string
	affinity  map[string]proxyPoolAffinity // 账户 → 上次用过的端口
	sessions  map[string]*proxyPoolSession // 账户 → 会话（基础会话号、代号、连通失败计数）
	byKey     map[string][]*proxyPoolLease // 租约键 → 活动租约，最后一个最新
	byAccount map[string][]*proxyPoolLease // 账户 ID → 活动租约（新账户登录 adopt 之后才有），最后一个最新
	accounts  map[string]*proxyPoolAccount // 账户 → 固定代理、外部占用、最近推送的绑定与默认出口端口
	// strikes 为账户 → 自最近一次生成成功以来的裸限流 429 记录，successes 为账户 → 最近一次生成成功的出口与时间（只在静态 IP 下记，
	// 只在内存）：规则 A 据此只计入被拒像是 IP 问题的账号，并要求其中有近期在别的出口上成功过的
	strikes   map[string]*proxyPoolStrike
	successes map[string]proxyPoolSuccess
	// defaultUsers 为端口 → 默认出口落在它上面的账户：端口被占、冷却、被其他进程锁住时按它重推
	defaultUsers map[int]map[string]struct{}
	// defaultWaiting 为静态 IP 下默认出口没有落在可分配端口上的账户（端口用满时退回 PROXY，PROXY 为空时与别人共用哈希端口）：
	// 端口重新可分配（归还后 proxyPoolWakeDelay、冷却到期、其他进程锁的重试时间到）时重推，见 wakeDefaultsLocked
	defaultWaiting map[string]struct{}
	// recheckAt 为 defaultWaiting 不为空时下一次检查的时刻（段内最早到期的冷却、其他进程锁的重试时间与 wakeAt）；recheckTimer 到时检查，
	// 申请、归还与状态接口也顺带检查。recheckArmed 为定时器已安排、还没触发
	recheckAt    time.Time
	recheckTimer *time.Timer
	recheckArmed bool
	// wakeAt 为端口归还后合并重推在等的账户的时刻（归还时刻 + proxyPoolWakeDelay；为零时没有安排），见 releaseLocked
	wakeAt time.Time
	// external 为端口 → 账户固定代理或 PROXY 指向它的个数；globalExternal 为 PROXY 指向的端口（0 为没有）
	external       map[int]int
	globalExternal int
	// lockedElsewhere 为被其他进程锁住的端口 → 下次重试时间：重试时间之前不分配；默认出口在本进程真正锁到、或到期后检测到
	// 锁已释放（只检测、不持有）之前一直避开它
	lockedElsewhere map[int]time.Time
	pinned          int // 自己的代理与 PROXY 规范值相同的账户数
	pooledLeases    int // 池分配的活动租约数
	stats           proxyPoolStats
	check           *api.AdminProxyPoolCheck
	checkRunning    bool
	onPortCooled    func(proxyPoolCooling) // 端口开始冷却时的回调（生成服务接到 429 治理，见 proxy_pool_egress.go），nil 时不调用；在锁外调用
	shortage        proxyPoolShortageLog
	coolingShortage proxyPoolShortageLog
	capacityShort   bool
	relayLow        bool
	pinnedWarned    bool // 这次开启后已经提示过固定了默认代理的账户
	externalSummary string
	systemicLogged  time.Time
	orphanedLogged  int
	overSince       time.Time
	overLogged      bool
	dirty           bool
	saveTimer       *time.Timer
	saveMu          sync.Mutex
}

// proxyPoolSettings 为生效配置与预先算好的值
type proxyPoolSettings struct {
	initialized       bool
	cfg               config.ProxyPool
	global            string   // PROXY（规范值）：默认出口的兜底与外部占用扫描
	endpoint          string   // 脱敏的 scheme://host
	key               string   // static 的 EndpointKey（端口锁与端口记录用）；其他模式为空
	urls              []string // 静态：下标 i 为端口 start+i 的 URL（同一端口每次都是同一个字符串）
	capacity          int      // 可能同时运行的浏览器数（Σ 峰值 + 排空上限）
	sessionTemplate   string   // 动态：规范化的地址（含 {session}），换会话号只做字符串替换
	sessionInPassword bool
	// portStart、portEnd、size 为静态的端口段与端口数（configure 时算好，热路径不再经 cfg.Ports() 补默认值）；不是静态或算不出 URL 时
	// size 为 0。steps 为与端口数互质的步长（默认出口的探测序列用，见 proxyPoolProbe）
	portStart, portEnd, size int
	steps                    []int
}

// proxyPoolPort 为一个静态端口的占用与健康记录
type proxyPoolPort struct {
	holders        map[string]int // 租约键 → 活动租约数
	lock           proxyPoolPortLock
	lastAccount    string             // 端口的“归属”：最后一个独占它的账户
	lastUsedAt     time.Time          // 最近一次分出或全部归还的时间
	failures       []proxyPoolFailure // 规则 A：窗口内的裸 429（每个账号只留最近一次），生成成功时清空
	hits           []proxyPoolFailure // 最近的裸 429（状态接口的 hot_ports），不因成功清空
	lastGenSuccess time.Time
	upstreamFails  int // 规则 B：连续的上游失败启动
	coolUntil      time.Time
	coolLevel      int
	cooledAt       time.Time
}

// proxyPoolFailure 为端口上一个账号最近一次计入规则 A 的裸 429：proven 为当时账号 proxyPoolProvenWithin 内在别的出口上成功过
type proxyPoolFailure struct {
	account string
	at      time.Time
	proven  bool
}

// proxyPoolStrike 为账户自最近一次生成成功以来的裸限流 429（规则 A 区分被拒的是账号还是 IP）：port 为第一次被拒的出口
// （段内端口；0 为不经 IP 池端口的出口，如 PROXY 或直通租约），spread 为之后又在另一个出口上被拒（换了 IP 照样被拒，跟着账号走），
// tainted 为第一次被拒之前账号已被 429 治理标为坏号（证据不来自这个 IP，如重启前标上的）；at 为最近一次被拒。生成成功时清除，
// 端口冷却撤回坏号（pardon）时保留：撤回后在新端口上又被拒的账号不再计入新端口的规则 A，误判不会沿着换端口一路传下去
type proxyPoolStrike struct {
	port    int
	spread  bool
	tainted bool
	at      time.Time
}

// proxyPoolSuccess 为账户最近一次生成成功：port 为当时绑定所用的段内端口（0 为不经 IP 池端口的出口）
type proxyPoolSuccess struct {
	port int
	at   time.Time
}

// proxyPoolAffinity 为账户上次独占的端口
type proxyPoolAffinity struct {
	port int
	at   time.Time
}

// proxyPoolSession 为动态 IP 的账户会话：base 为新账户登录时采用的临时会话号（空为按账户 ID 哈希），generation 为换会话的代号
type proxyPoolSession struct {
	base        string
	generation  int
	rotatedAt   time.Time
	usedAt      time.Time
	strikes     int
	firstStrike time.Time
}

// proxyPoolAccount 为 IP 池记下的账户：proxy 为规范化的账户固定代理（非空时不用 IP 池），externalPort 为它指向的池内端口
type proxyPoolAccount struct {
	known        bool
	proxyRaw     string
	proxy        string
	externalPort int
	pinned       bool
	binding      *aistudio.ProxyBinding
	defaultPort  int
	// probeSize、probeOffset、probeStep 为默认出口的探测序列（按端口数缓存，端口数变化时重算，见 proxyPoolProbe）
	probeSize, probeOffset, probeStep int
}

type proxyPoolStats struct {
	affinityHits   int
	affinityMisses int
	rotations      int
}

// proxyPoolShortageLog 为限流的 WARN：每分钟最多一行，带这一分钟内另有的次数
type proxyPoolShortageLog struct {
	last       time.Time
	suppressed int
}

// proxyPoolCooling 为一次端口冷却（onPortCooled 的参数）：RateLimitedAccounts 为计入规则 A 的账号（窗口里返回裸 429、被拒像是 IP 问题的，
// 见 strikeLocked），Holders 为当时持有该端口的账户（含登录、验证与启动中的 Worker），Workers 为其中正用这个端口服务的账户
// （绑定所用的租约是这个端口上已启动的 Worker），端口冷却后让它们换端口重启
type proxyPoolCooling struct {
	Port                int
	Until               time.Time
	Level               int
	Rule                string
	RateLimitedAccounts []string
	Holders             []string
	Workers             []string
}

// proxyPoolLog 为锁内收集、锁外写出的一行日志
type proxyPoolLog struct {
	source, level, message string
	durable                bool
}

// proxyPoolLease 为一个浏览器的出口租约：url 为实际使用的上游，pooled 为 IP 池分配（否则是直通租约，url 为当时的 PROXY）
type proxyPoolLease struct {
	pool        *proxyPool
	key         string // 租约键：已有账户为账户 ID，新账户登录为 "login:" + 随机串
	accountID   string // 新账户登录时为空，adopt 后填上
	kind        string
	label       string
	url         string
	slot        string // 静态为端口号，动态为会话号
	port        int
	pooled      bool
	shared      bool // 端口不足时与其他账号共用
	version     uint64
	egressTag   string
	tempSession string // 新账户登录用的临时会话号（adopt 时采用）
	released    bool   // pool.mu 保护
	// attached 为 Worker 浏览器已启动成功（attach）或补登记的运行中 Worker（passthrough）：账户绑定优先取最新的这种租约，
	// 新浏览器起来之前 Go 侧仍走正在服务的旧浏览器的出口（pool.mu 保护）
	attached bool
}

type proxyPoolOptions struct {
	path, lockDir string
	requests      *requestRegistry
	now           func() time.Time
	random        io.Reader
	// tryLock、relayLimit 为测试替身；为空时用 aistudio.TryLockProxyPort 与 proxyrelay.TotalConnectionLimit
	tryLock    func(directory, endpointKey string, port int) (proxyPoolPortLock, bool, error)
	relayLimit func() int
}

// newProxyPool 创建 IP 池并载入记录文件（不存在时从空开始，损坏时 WARN 后从空开始）；默认关闭，configure 后生效
func newProxyPool(options proxyPoolOptions) *proxyPool {
	pool := &proxyPool{
		path: options.path, lockDir: options.lockDir, requests: options.requests,
		now: options.now, random: options.random, tryLock: options.tryLock, relayLimit: options.relayLimit,
		ports: make(map[int]*proxyPoolPort), affinity: make(map[string]proxyPoolAffinity),
		sessions: make(map[string]*proxyPoolSession), byKey: make(map[string][]*proxyPoolLease),
		byAccount: make(map[string][]*proxyPoolLease), accounts: make(map[string]*proxyPoolAccount),
		strikes: make(map[string]*proxyPoolStrike), successes: make(map[string]proxyPoolSuccess),
		defaultUsers: make(map[int]map[string]struct{}), defaultWaiting: make(map[string]struct{}),
		external: make(map[int]int), lockedElsewhere: make(map[int]time.Time),
	}
	pool.settings.cfg = config.DefaultProxyPool()
	if pool.now == nil {
		pool.now = time.Now
	}
	if pool.random == nil {
		pool.random = rand.Reader
	}
	if pool.tryLock == nil {
		pool.tryLock = func(directory, endpointKey string, port int) (proxyPoolPortLock, bool, error) {
			lock, locked, err := aistudio.TryLockProxyPort(directory, endpointKey, port)
			if lock == nil {
				return nil, locked, err
			}
			return lock, locked, err
		}
	}
	if pool.relayLimit == nil {
		pool.relayLimit = proxyrelay.TotalConnectionLimit
	}
	if logs := pool.load(); len(logs) > 0 {
		pool.emit(logs)
	}
	return pool
}

// configure 应用 IP 池设置、PROXY 与容量：配置有变化时版本 +1、记日志、重新扫描外部占用并重推全部账户（有活动租约的账户绑定不变）；
// 只有容量变化时只重新判断端口是否够（状态切换时记一次日志）。永不重置 holders。返回配置（含 PROXY）是否有变化
func (pool *proxyPool) configure(cfg config.ProxyPool, globalProxy string, capacity int) bool {
	if pool == nil {
		return false
	}
	var logs []proxyPoolLog
	normalized, err := config.NormalizeProxyPool(cfg)
	if err != nil {
		// 校验过的配置不会走到这里；万一出现按关闭处理，错误文案不含地址内容
		logs = append(logs, proxyPoolLog{source: "service", level: "WARN", message: "IP 池设置无效，按关闭处理 | 错误=" + err.Error()})
		normalized = config.DefaultProxyPool()
	}
	global := config.CanonicalProxy(globalProxy)
	if capacity < 0 {
		capacity = 0
	}

	pool.mu.Lock()
	old := pool.settings
	cfgChanged := !old.initialized || !normalized.Equal(old.cfg)
	globalChanged := !old.initialized || global != old.global
	capacityChanged := !old.initialized || capacity != old.capacity
	if !cfgChanged && !globalChanged && !capacityChanged {
		pool.mu.Unlock()
		pool.emit(logs)
		return false
	}
	wasEnabled := old.initialized && old.cfg.Enabled()
	if cfgChanged || globalChanged {
		if cfgChanged {
			pool.version++
		}
		pool.settings = proxyPoolSettings{
			initialized: true, cfg: normalized, global: global, endpoint: normalized.Endpoint(), capacity: capacity,
		}
		switch normalized.Mode {
		case config.ProxyPoolStatic:
			pool.settings.urls = normalized.StaticURLs()
			pool.settings.key = normalized.EndpointKey()
			if len(pool.settings.urls) == 0 {
				// 校验过的配置总能算出 URL；算不出时按关闭处理，不分配端口
				pool.settings.cfg.Mode = config.ProxyPoolOff
				pool.settings.key = ""
				pool.settings.urls = nil
			} else {
				pool.settings.portStart, pool.settings.portEnd = normalized.Ports()
				pool.settings.size = len(pool.settings.urls)
				pool.settings.steps = proxyPoolSteps(pool.settings.size)
			}
		case config.ProxyPoolDynamic:
			pool.settings.sessionTemplate = normalized.URL
			pool.settings.sessionInPassword = normalized.SessionInPassword()
		}
		if key := pool.settings.key; key != "" {
			if pool.portsKey != "" && pool.portsKey != key {
				// 换了代理商或子账号：同一端口号已经不是同一个 IP，清掉冷却、归属与健康记录（不动 holders，同一端口号仍视为同一占用）
				pool.resetPortRecordsLocked()
			}
			pool.portsKey = key
		}
		pool.recomputeAccountsLocked()
		logs = append(logs, pool.externalLogLocked()...)
		pool.syncOwnerLocked(true, "")
		logs = append(logs, pool.configureLogsLocked(old, wasEnabled)...)
	} else {
		pool.settings.capacity = capacity
	}
	logs = append(logs, pool.capacityLogsLocked(cfgChanged)...)
	pool.wakeDefaultsLocked(pool.now())
	pool.mu.Unlock()
	pool.emit(logs)
	return cfgChanged || globalChanged
}

// configureLogsLocked 为配置变化时的日志：更新或关闭、运行中的 Worker 仍走默认代理、不再使用 PROXY、固定了默认代理的账户
func (pool *proxyPool) configureLogsLocked(old proxyPoolSettings, wasEnabled bool) []proxyPoolLog {
	cfg := pool.settings.cfg
	var logs []proxyPoolLog
	configChanged := !old.initialized || !cfg.Equal(old.cfg)
	switch {
	case cfg.Enabled() && configChanged:
		message := "IP 池已更新 | 模式=动态 | 出口=" + pool.settings.endpoint
		if cfg.Mode == config.ProxyPoolStatic {
			start, end := cfg.Ports()
			message = fmt.Sprintf("IP 池已更新 | 模式=静态 | 出口=%s | 端口=%d-%d（%d 个） | 需要=%d",
				pool.settings.endpoint, start, end, cfg.Size(), pool.settings.capacity)
		}
		logs = append(logs, proxyPoolLog{source: "service", level: "INFO", message: message})
	case !cfg.Enabled() && wasEnabled:
		logs = append(logs, proxyPoolLog{source: "service", level: "INFO", message: fmt.Sprintf(
			"IP 池已关闭 | 仍在使用旧出口的浏览器=%d（关闭后归还）", pool.pooledLeases)})
	}
	if !cfg.Enabled() {
		pool.pinnedWarned = false
		return logs
	}
	if !wasEnabled {
		if unpooled := pool.unpooledLocked(); unpooled > 0 {
			logs = append(logs, proxyPoolLog{source: "service", level: "INFO", message: fmt.Sprintf(
				"IP 池已开启，运行中的 Worker 仍走默认代理，关闭后改用 IP 池 | Worker=%d | 立即切换：停止服务后再启动", unpooled)})
		}
		pool.pinnedWarned = false
		logs = append(logs, pool.pinnedLogLocked()...)
	}
	if pool.settings.global != "" && (!wasEnabled || pool.settings.global != old.global) {
		logs = append(logs, proxyPoolLog{source: "service", level: "INFO", message: "IP 池开启后，没有自己代理的账户不再使用默认代理 PROXY"})
	}
	return logs
}

// pinnedLogLocked IP 池开启时有账户固定了与默认代理相同的地址：每次开启后提示一次（启动时账户在 attach 后才知道）
func (pool *proxyPool) pinnedLogLocked() []proxyPoolLog {
	if pool.pinnedWarned || pool.pinned == 0 || !pool.settings.cfg.Enabled() {
		return nil
	}
	pool.pinnedWarned = true
	return []proxyPoolLog{{source: "service", level: "WARN", message: fmt.Sprintf(
		"有账户固定了与默认代理相同的地址，不会使用 IP 池 | 账户=%d | 可在“服务配置 → IP 池”改为跟随默认代理", pool.pinned)}}
}

// capacityLogsLocked 判断端口是否够用与本机中转的连接上限，状态切换（或配置变化）时记一行
func (pool *proxyPool) capacityLogsLocked(cfgChanged bool) []proxyPoolLog {
	cfg := pool.settings.cfg
	capacity := pool.settings.capacity
	var logs []proxyPoolLog
	short := cfg.Mode == config.ProxyPoolStatic && capacity > 0 && cfg.Size() < capacity
	switch {
	case short && (!pool.capacityShort || cfgChanged):
		logs = append(logs, proxyPoolLog{source: "service", level: "WARN", message: fmt.Sprintf(
			"IP 池端口少于可能同时运行的浏览器数，超出的会与其他账号共用端口 | 端口=%d | 需要=%d", cfg.Size(), capacity)})
	case !short && pool.capacityShort && cfg.Mode == config.ProxyPoolStatic:
		logs = append(logs, proxyPoolLog{source: "service", level: "INFO", message: fmt.Sprintf(
			"IP 池端口已足够 | 端口=%d | 需要=%d", cfg.Size(), capacity)})
	}
	pool.capacityShort = short
	low := false
	if cfg.Enabled() && capacity > 0 && pool.relayLimit != nil {
		limit := pool.relayLimit()
		low = limit > 0 && limit < capacity*16
		if low && (!pool.relayLow || cfgChanged) {
			logs = append(logs, proxyPoolLog{source: "service", level: "WARN", message: fmt.Sprintf(
				"本机代理中转连接上限偏低，浏览器多时可能出现“连接数已满：整个进程” | 上限=%d | 建议 ulimit -n ≥ 65536", limit)})
		}
	}
	pool.relayLow = low
	return logs
}

// setPortCooledHook 登记端口开始冷却时的回调（生成服务装配成功后接到 429 治理，见 proxy_pool_egress.go）：在锁外调用，
// 调用方可能持有账户的 startupMu（Worker 启动失败时），回调不能同步等这个账户的 Worker 启停
func (pool *proxyPool) setPortCooledHook(hook func(proxyPoolCooling)) {
	if pool == nil {
		return
	}
	pool.mu.Lock()
	pool.onPortCooled = hook
	pool.mu.Unlock()
}

// prime 给还没进入账户池的账户对象写默认出口（newRuntime 在 NewAccountPool 之前、workers.Add 在 AccountPool.Add 之前调用）：
// 对象还没进账户池，原子写即可，账户池挂载时按带绑定的 EffectiveProxy 计算
func (pool *proxyPool) prime(account *aistudio.Account) {
	if pool == nil || account == nil || account.ID == "" {
		return
	}
	pool.mu.Lock()
	state := pool.accountLocked(account.ID)
	externalChanged := pool.observeAccountProxyLocked(account.ID, state, account.Config.Proxy)
	binding := pool.bindingLocked(account.ID, state, &proxyPoolBatch{now: pool.now()})
	account.SetProxyBinding(binding)
	if !proxyBindingEqual(binding, state.binding) {
		// 推送缓存记的是账户池里同 ID 对象的绑定：重建期间新旧实例里可能各有一个同 ID 的对象，两边保持一致
		state.binding = binding
		if pool.owner != nil {
			pool.owner.SetProxyBindings([]aistudio.ProxyBindingUpdate{{AccountID: account.ID, Binding: binding}})
		}
	}
	var logs []proxyPoolLog
	if externalChanged {
		logs = pool.externalLogLocked()
		pool.syncOwnerLocked(true, "")
	}
	pool.wakeDefaultsLocked(pool.now())
	pool.mu.Unlock()
	pool.emit(logs)
}

// attach 把推送目标切换为 owner（新生成服务的账户池），扫描固定代理（外部占用、pinned），按当前状态重推全部账户；
// 旧生成服务之后的 refresh 被忽略，旧实例浏览器的归还按账户 ID 推给新账户池
func (pool *proxyPool) attach(owner proxyPoolOwner) {
	if pool == nil {
		return
	}
	if accountPool, ok := owner.(*aistudio.AccountPool); ok && accountPool == nil {
		owner = nil
	}
	pool.mu.Lock()
	pool.owner = owner
	pool.syncOwnerLocked(true, "")
	logs := append(pool.externalLogLocked(), pool.pinnedLogLocked()...)
	pool.wakeDefaultsLocked(pool.now())
	pool.mu.Unlock()
	pool.emit(logs)
}

// refresh 账户加入或改了自己的代理后重算它的外部占用与默认出口并推送；owner 不是当前推送目标时忽略（旧生成服务）
func (pool *proxyPool) refresh(owner proxyPoolOwner, accountID string) {
	if pool == nil {
		return
	}
	if accountPool, ok := owner.(*aistudio.AccountPool); ok && accountPool == nil {
		return
	}
	pool.mu.Lock()
	if owner == nil || pool.owner != owner {
		pool.mu.Unlock()
		return
	}
	pool.syncOwnerLocked(false, accountID)
	logs := append(pool.externalLogLocked(), pool.pinnedLogLocked()...)
	pool.wakeDefaultsLocked(pool.now())
	pool.mu.Unlock()
	pool.emit(logs)
}

// acquire 为一次浏览器类操作申请出口：key 为租约键（已有账户为账户 ID，新账户登录为 "login:" + 随机串），accountID 可为空；
// IP 池关闭（或没有可用端口）时返回直通租约（url 为 fallback，参数不改）。kind 为 worker、login、verify 或 import，label 只用于日志
func (pool *proxyPool) acquire(key, accountID, kind, label, fallback string) *proxyPoolLease {
	if pool == nil {
		return nil
	}
	pool.mu.Lock()
	lease, logs := pool.acquireLocked(key, accountID, kind, label, fallback)
	pool.mu.Unlock()
	pool.emit(logs)
	return lease
}

// passthrough 登记一个直通租约（不分配端口、不记日志）：url 为浏览器实际使用的上游（通常是当时的 PROXY）。
// “改为跟随默认代理”时给运行中的 Worker 补登记，Go 侧在浏览器关闭前仍走它
func (pool *proxyPool) passthrough(key, accountID, kind, label, url string) *proxyPoolLease {
	if pool == nil {
		return nil
	}
	pool.mu.Lock()
	// 补登记的是正在服务的浏览器：直接算作已启动（参与绑定）
	lease := &proxyPoolLease{
		pool: pool, key: key, accountID: accountID, kind: kind, label: label, url: strings.TrimSpace(url), version: pool.version,
		attached: true,
	}
	pool.registerLeaseLocked(lease)
	if accountID != "" {
		pool.pushLocked(accountID)
	}
	pool.mu.Unlock()
	return lease
}

func (pool *proxyPool) acquireLocked(key, accountID, kind, label, fallback string) (*proxyPoolLease, []proxyPoolLog) {
	now := pool.now()
	if key == "" {
		key = accountID
	}
	lease := &proxyPoolLease{pool: pool, key: key, accountID: accountID, kind: kind, label: label, version: pool.version}
	var logs []proxyPoolLog
	var displaced []string
	followed := false
	if kind != proxyPoolKindWorker && accountID != "" && pool.settings.cfg.Enabled() {
		// 登录、验证与导入先跟随账户正在用的出口（只有 Worker 按新会话或新端口走），同一账户同时只用一个 IP
		followed, logs = pool.followServingLocked(lease, now)
	}
	switch {
	case followed:
	case pool.settings.cfg.Mode == config.ProxyPoolStatic:
		displaced, logs = pool.allocateStaticLocked(lease, now)
	case pool.settings.cfg.Mode == config.ProxyPoolDynamic:
		logs = pool.allocateDynamicLocked(lease, now)
	}
	if !lease.pooled && !followed {
		lease.url = strings.TrimSpace(fallback)
	}
	pool.registerLeaseLocked(lease)
	ids := make([]string, 0, len(displaced)+1)
	if accountID != "" {
		ids = append(ids, accountID)
	}
	ids = append(ids, displaced...)
	pool.pushLocked(ids...)
	logs = append(logs, pool.overAllocationLogLocked(now)...)
	pool.wakeDefaultsLocked(now)
	return lease, logs
}

// allocateStaticLocked 按 §5.1 选端口：同键共用 > 亲和 > 可分配端口（从没分过 > 孤儿 > 别人的归属，同档最久未用、默认出口最少）>
// 同一账户正在共用的端口（只有 Worker 走到这里）> 端口不足时共用 > 全部冷却时用最早恢复的。返回需要重推的默认出口账户与日志
func (pool *proxyPool) allocateStaticLocked(lease *proxyPoolLease, now time.Time) ([]string, []proxyPoolLog) {
	var logs []proxyPoolLog
	port, source := 0, ""
	// 同一账户已有端口时直接共用（同一账户，不破坏一号一 IP）：先启新浏览器再关旧的、Worker 运行中重新登录或验证、
	// 新账户的 Worker 在登录租约归还之前启动。加入的租约本身是端口不足时共用的，这一个同样算共用（Worker 不加入这种租约，见下）
	join, sharedFallback := pool.sameAccountLeaseLocked(lease, now)
	if join != nil {
		port, source, lease.shared = join.port, "同账号", join.shared
	}
	if port == 0 && lease.accountID != "" {
		if affinity, ok := pool.affinity[lease.accountID]; ok {
			if pool.ownsLocked(lease.accountID, affinity.port) && pool.allocatableLocked(affinity.port, now) &&
				pool.lockPortLocked(affinity.port, now, &logs) {
				port, source = affinity.port, "亲和"
				pool.stats.affinityHits++
			} else {
				pool.stats.affinityMisses++
			}
		}
	}
	for port == 0 {
		candidate := pool.pickFreeLocked(now)
		if candidate == 0 {
			break
		}
		if pool.lockPortLocked(candidate, now, &logs) {
			port, source = candidate, "空闲"
		}
	}
	if port == 0 && sharedFallback != nil {
		// Worker 没有可分配端口：回到同一账户正在与别人共用的端口（仍是这个账户的 IP），不另找别人的端口
		port, source, lease.shared = sharedFallback.port, "同账号", true
		logs = append(logs, pool.shortageLogLocked(lease, port, "shortage", now)...)
	}
	if port == 0 {
		var reason string
		port, reason = pool.pickSharedLocked(now)
		if port == 0 {
			logs = append(logs, proxyPoolLog{source: lease.logSource(), level: "WARN", message: "IP 池没有可用端口，改用默认代理"})
			return nil, logs
		}
		lease.shared = true
		source = "共用"
		if len(pool.portLocked(port).holders) == 0 {
			// 冷却或被其他进程锁住的端口没有本进程的持有者：照常尝试加锁，锁不到也只能共用
			pool.lockPortLocked(port, now, &logs)
		}
		logs = append(logs, pool.shortageLogLocked(lease, port, reason, now)...)
	}
	state := pool.portLocked(port)
	if state.holders == nil {
		state.holders = make(map[string]int)
	}
	first := len(state.holders) == 0
	state.holders[lease.key]++
	state.lastUsedAt = now
	lease.port = port
	lease.slot = strconv.Itoa(port)
	lease.url = pool.staticURLLocked(port)
	lease.pooled = true
	if lease.accountID != "" && !lease.shared && source != "同账号" {
		pool.recordOwnerLocked(lease.accountID, port, now)
	}
	var displaced []string
	if first {
		displaced = pool.defaultUsersOfLocked(port, lease.accountID)
	}
	logs = append(logs, proxyPoolLog{source: lease.logSource(), level: "INFO", message: fmt.Sprintf(
		"IP 池分配 | 端口=%d | 来源=%s | 用途=%s | 使用中=%d/%d", port, source, proxyPoolKindLabel(lease.kind),
		pool.portsInUseLocked(), pool.settings.size)})
	return displaced, logs
}

// followServingLocked 登录、验证与导入（不含 Worker）跟随账户绑定所用的租约（Go 侧正在用的出口），同一账户同时只用一个 IP：
// 动态 IP 下沿用它的会话号（换会话之后、Worker 重启之前仍是旧会话）；正在服务的 Worker 走直通出口（IP 池开启前启动、或当时没有
// 可用端口）时用同一个上游，不另分端口或会话。静态端口由 sameAccountLeaseLocked 共用（计入持有者）。返回是否已填好租约
func (pool *proxyPool) followServingLocked(lease *proxyPoolLease, now time.Time) (bool, []proxyPoolLog) {
	current := pool.bindingLeaseLocked(lease.accountID)
	switch {
	case current == nil:
		return false, nil
	case !current.pooled:
		if !current.attached || current.kind != proxyPoolKindWorker {
			return false, nil
		}
		lease.url = current.url
		return true, []proxyPoolLog{{source: lease.logSource(), level: "INFO", message: fmt.Sprintf(
			"IP 池不分配，与仍走默认代理的 Worker 用同一出口 | 用途=%s", proxyPoolKindLabel(lease.kind))}}
	case current.port == 0 && pool.leaseCurrentLocked(current):
		lease.url, lease.slot, lease.pooled, lease.egressTag = current.url, current.slot, true, current.egressTag
		if state := pool.sessions[lease.accountID]; state != nil {
			state.usedAt = now
		}
		return true, []proxyPoolLog{{source: lease.logSource(), level: "INFO", message: fmt.Sprintf(
			"IP 池分配 | 会话=%s | 来源=同账号 | 用途=%s | 使用中=%d", current.slot, proxyPoolKindLabel(lease.kind),
			pool.sessionsInUseLocked())}}
	}
	return false, nil
}

// allocateDynamicLocked 动态 IP：已有账户用它的固定会话号，新账户登录用临时会话号（adopt 时采用）；不独占
func (pool *proxyPool) allocateDynamicLocked(lease *proxyPoolLease, now time.Time) []proxyPoolLog {
	session := ""
	if lease.accountID != "" {
		session = pool.sessionLocked(lease.accountID)
		if state := pool.sessions[lease.accountID]; state != nil {
			state.usedAt = now
		}
	} else {
		lease.tempSession = newProxyPoolSession(pool.random, "n", proxyPoolSessionHashLen-1)
		session = lease.tempSession
	}
	url := pool.sessionURLLocked(session)
	if url == "" {
		return nil
	}
	inUse := pool.sessionsInUseLocked()
	if !pool.sessionInUseLocked(session) {
		inUse++
	}
	lease.url, lease.slot, lease.pooled = url, session, true
	if pool.settings.sessionInPassword {
		lease.egressTag = session
	}
	return []proxyPoolLog{{source: lease.logSource(), level: "INFO", message: fmt.Sprintf(
		"IP 池分配 | 会话=%s | 用途=%s | 使用中=%d", session, proxyPoolKindLabel(lease.kind), inUse)}}
}

// registerLeaseLocked 登记活动租约
func (pool *proxyPool) registerLeaseLocked(lease *proxyPoolLease) {
	pool.byKey[lease.key] = append(pool.byKey[lease.key], lease)
	if lease.accountID != "" {
		pool.byAccount[lease.accountID] = append(pool.byAccount[lease.accountID], lease)
	}
	if lease.pooled {
		pool.pooledLeases++
	}
}

// sameAccountLeaseLocked 返回同一账户可以共用端口的活动租约（join）：先看账户绑定所用的租约（正在服务的浏览器，与 Go 侧同一个 IP），
// 再按最新的优先看同键的租约与账户名下的全部租约（含 adopt 过来的登录租约）；端口须是当前设置下的段内端口。Worker 遇到冷却的
// 端口另选（换到新 IP）；登录、验证与导入不因冷却拒绝：冷却只拦把端口新分给其他账号，这时与 Worker 分开反而让同一账户同时用两个 IP。
// Worker 也不加入端口不足时与别人共用的租约（加入后整个生命周期都和别的账号共用一个 IP）：这种租约作为 shared 返回，
// 只有确实没有可分配端口时才回到它
func (pool *proxyPool) sameAccountLeaseLocked(lease *proxyPoolLease, now time.Time) (join, shared *proxyPoolLease) {
	groups := [][]*proxyPoolLease{nil, pool.byKey[lease.key]}
	if lease.accountID != "" {
		if current := pool.bindingLeaseLocked(lease.accountID); current != nil {
			groups[0] = []*proxyPoolLease{current}
		}
		groups = append(groups, pool.byAccount[lease.accountID])
	}
	for _, leases := range groups {
		for index := len(leases) - 1; index >= 0; index-- {
			other := leases[index]
			if !other.pooled || other.port == 0 || !pool.currentStaticLocked(other) {
				continue
			}
			if lease.kind == proxyPoolKindWorker {
				if pool.coolingLocked(other.port, now) {
					continue
				}
				if other.shared {
					if shared == nil {
						shared = other
					}
					continue
				}
			}
			return other, nil
		}
	}
	return nil, shared
}

// pickFreeLocked 在可分配端口里按“从没分给过账号 > 孤儿（归属账号的亲和已不指向它）> 别人的归属”挑选，同档按最久未用（刚归还的
// 端口不马上分给下一个账号），再按落在它上面的默认出口最少（占用后要挪走的账户最少，从零预热时各端口都没用过、靠它分散），再按端口号
func (pool *proxyPool) pickFreeLocked(now time.Time) int {
	if pool.settings.size == 0 {
		return 0
	}
	best, bestTier, bestUsers := 0, 0, 0
	var bestUsed time.Time
	for port := pool.settings.portStart; port <= pool.settings.portEnd; port++ {
		if !pool.allocatableLocked(port, now) {
			continue
		}
		tier, used := 0, time.Time{}
		if state := pool.ports[port]; state != nil {
			used = state.lastUsedAt
			if state.lastAccount != "" {
				tier = 2
				if affinity, ok := pool.affinity[state.lastAccount]; !ok || affinity.port != port {
					tier = 1
				}
			}
		}
		users := len(pool.defaultUsers[port])
		if best == 0 || tier < bestTier ||
			tier == bestTier && (used.Before(bestUsed) || used.Equal(bestUsed) && users < bestUsers) {
			best, bestTier, bestUsers, bestUsed = port, tier, users, used
		}
	}
	return best
}

// pickSharedLocked 没有可分配端口时选共用的端口：本进程持有、不冷却的段内端口里持有键最少的（并列取最久未用）；
// 都在冷却时取恢复最早、持有最少的；再没有时取段内任意不是外部占用的端口里持有最少的。返回端口与原因（shortage、cooling、fallback）
func (pool *proxyPool) pickSharedLocked(now time.Time) (int, string) {
	if pool.settings.size == 0 {
		return 0, ""
	}
	start, end := pool.settings.portStart, pool.settings.portEnd
	best := 0
	for port := start; port <= end; port++ {
		state := pool.ports[port]
		if state == nil || len(state.holders) == 0 || pool.coolingLocked(port, now) || pool.external[port] > 0 {
			continue
		}
		if best == 0 || len(state.holders) < len(pool.ports[best].holders) ||
			len(state.holders) == len(pool.ports[best].holders) && state.lastUsedAt.Before(pool.ports[best].lastUsedAt) {
			best = port
		}
	}
	if best != 0 {
		return best, "shortage"
	}
	for port := start; port <= end; port++ {
		state := pool.ports[port]
		if state == nil || !pool.coolingLocked(port, now) || pool.external[port] > 0 {
			continue
		}
		if best == 0 || state.coolUntil.Before(pool.ports[best].coolUntil) ||
			state.coolUntil.Equal(pool.ports[best].coolUntil) && len(state.holders) < len(pool.ports[best].holders) {
			best = port
		}
	}
	if best != 0 {
		return best, "cooling"
	}
	for port := start; port <= end; port++ {
		if pool.external[port] > 0 {
			continue
		}
		state := pool.portLocked(port)
		if best == 0 || len(state.holders) < len(pool.ports[best].holders) ||
			len(state.holders) == len(pool.ports[best].holders) && state.lastUsedAt.Before(pool.ports[best].lastUsedAt) {
			best = port
		}
	}
	if best != 0 {
		return best, "shortage"
	}
	return 0, ""
}

// shortageLogLocked 为端口不足或全部冷却时的 WARN（每分钟最多一行，带这一分钟内另有的次数）
func (pool *proxyPool) shortageLogLocked(lease *proxyPoolLease, port int, reason string, now time.Time) []proxyPoolLog {
	limiter := &pool.shortage
	if reason == "cooling" {
		limiter = &pool.coolingShortage
	}
	if !limiter.last.IsZero() && now.Sub(limiter.last) < proxyPoolShortageLogInterval {
		limiter.suppressed++
		return nil
	}
	suffix := ""
	if limiter.suppressed > 0 {
		suffix = fmt.Sprintf(" | 这一分钟内另有 %d 次", limiter.suppressed)
	}
	limiter.last, limiter.suppressed = now, 0
	state := pool.portLocked(port)
	message := fmt.Sprintf("IP 池端口不足，与其他账号共用 | 端口=%d | 共用账号=%d | 使用中=%d/%d | 冷却=%d%s",
		port, len(state.holders)+1, pool.portsInUseLocked(), pool.settings.size, pool.coolingCountLocked(now), suffix)
	if reason == "cooling" {
		message = fmt.Sprintf("IP 池端口全部冷却，使用最早恢复的端口 | 端口=%d | 恢复=%s%s",
			port, state.coolUntil.UTC().Format(time.RFC3339), suffix)
	}
	return []proxyPoolLog{{source: lease.logSource(), level: "WARN", message: message}}
}

// lockPortLocked 在端口第一次被本进程持有时加跨进程锁：被其他进程锁住时记下重试时间并返回 false；锁出错时照常分配
// （见 lockErrorLocked），出错后第一次加锁成功时给出错期间分出的端口补锁（见 lockRecoveredLocked）
func (pool *proxyPool) lockPortLocked(port int, now time.Time, logs *[]proxyPoolLog) bool {
	state := pool.portLocked(port)
	if len(state.holders) > 0 || state.lock != nil || !pool.lockUsableLocked(now) {
		return true
	}
	lock, locked, err := pool.tryLock(pool.lockDir, pool.settings.key, port)
	if err != nil {
		pool.lockErrorLocked(now, err, logs)
		return true
	}
	pool.lockRecoveredLocked(now, logs)
	if !locked {
		pool.lockedElsewhere[port] = now.Add(proxyPoolLockedRetry)
		// 默认出口落在这个端口上的账户换到别处：另一个进程的浏览器正在用它
		pool.pushLocked(pool.defaultUsersOfLocked(port, "")...)
		return false
	}
	state.lock = lock
	delete(pool.lockedElsewhere, port)
	return true
}

// lockUsableLocked 判断这次是否跨进程加锁：有锁目录与端口记录键，且不在出错后的 proxyPoolLockErrorRetry 内
func (pool *proxyPool) lockUsableLocked(now time.Time) bool {
	if pool.lockDir == "" || pool.settings.key == "" {
		return false
	}
	return !pool.lockDegraded || now.Sub(pool.lockFailedAt) >= proxyPoolLockErrorRetry
}

// lockErrorLocked 端口锁出错（例如浏览器多时打开文件数用满、缓存目录暂时不可写）：记下时间，proxyPoolLockErrorRetry 内照常分配、
// 不加锁，到期再试；一段出错期只 WARN 一次
func (pool *proxyPool) lockErrorLocked(now time.Time, err error, logs *[]proxyPoolLog) {
	pool.lockFailedAt = now
	if pool.lockDegraded {
		return
	}
	pool.lockDegraded = true
	*logs = append(*logs, proxyPoolLog{source: "service", level: "WARN", message: fmt.Sprintf(
		"IP 池跨进程端口锁不可用，多个进程使用同一端口段时可能重复分配，%d 分钟后再试 | 错误=%s",
		int(proxyPoolLockErrorRetry/time.Minute), err.Error())})
}

// lockRecoveredLocked 端口锁出错后第一次正常加锁：给出错期间分出、本进程仍持有却没有锁的段内端口补锁。补不上（已被其他进程锁住）
// 的端口已经与其他进程重复使用，只能等浏览器关闭，记 WARN；补锁时又出错则继续退避
func (pool *proxyPool) lockRecoveredLocked(now time.Time, logs *[]proxyPoolLog) {
	if !pool.lockDegraded {
		return
	}
	pool.lockDegraded, pool.lockFailedAt = false, time.Time{}
	ports := make([]int, 0, len(pool.ports))
	for port, state := range pool.ports {
		if len(state.holders) > 0 && state.lock == nil && pool.inRangeLocked(port) {
			ports = append(ports, port)
		}
	}
	sort.Ints(ports)
	relocked, conflicts := 0, 0
	for _, port := range ports {
		lock, locked, err := pool.tryLock(pool.lockDir, pool.settings.key, port)
		if err != nil {
			pool.lockErrorLocked(now, err, logs)
			return
		}
		if !locked {
			conflicts++
			continue
		}
		pool.ports[port].lock = lock
		relocked++
	}
	level := "INFO"
	if conflicts > 0 {
		level = "WARN"
	}
	*logs = append(*logs, proxyPoolLog{source: "service", level: level, message: fmt.Sprintf(
		"IP 池跨进程端口锁已恢复 | 补锁端口=%d | 已被其他进程占用=%d（浏览器关闭前与其他进程共用）", relocked, conflicts)})
}

// recordOwnerLocked 记下账户独占了端口：亲和与端口归属，变化时置 dirty
func (pool *proxyPool) recordOwnerLocked(accountID string, port int, now time.Time) {
	state := pool.portLocked(port)
	previous, had := pool.affinity[accountID]
	pool.affinity[accountID] = proxyPoolAffinity{port: port, at: now}
	if !had || previous.port != port || state.lastAccount != accountID || now.Sub(previous.at) >= time.Hour {
		pool.markDirtyLocked()
	}
	state.lastAccount = accountID
}

// release 归还租约（可重复调用）：按 lease.port 找端口，不经当前设置；端口段外或旧版本的租约照常归还，之后不再分出去
func (lease *proxyPoolLease) release() {
	if lease == nil || lease.pool == nil {
		return
	}
	pool := lease.pool
	pool.mu.Lock()
	logs := pool.releaseLocked(lease)
	pool.mu.Unlock()
	pool.emit(logs)
}

func (pool *proxyPool) releaseLocked(lease *proxyPoolLease) []proxyPoolLog {
	if lease.released {
		return nil
	}
	lease.released = true
	now := pool.now()
	pool.byKey[lease.key] = removeProxyPoolLease(pool.byKey[lease.key], lease)
	if len(pool.byKey[lease.key]) == 0 {
		delete(pool.byKey, lease.key)
	}
	if lease.accountID != "" {
		pool.byAccount[lease.accountID] = removeProxyPoolLease(pool.byAccount[lease.accountID], lease)
		if len(pool.byAccount[lease.accountID]) == 0 {
			delete(pool.byAccount, lease.accountID)
		}
	}
	var logs []proxyPoolLog
	freed := false
	if lease.pooled {
		pool.pooledLeases--
		if lease.port != 0 {
			state := pool.portLocked(lease.port)
			if state.holders[lease.key]--; state.holders[lease.key] <= 0 {
				delete(state.holders, lease.key)
			}
			if len(state.holders) == 0 {
				if state.lock != nil {
					_ = state.lock.Release()
					state.lock = nil
				}
				state.lastUsedAt = now
				freed = true
			}
			message := fmt.Sprintf("IP 池归还 | 端口=%d", lease.port)
			if size := pool.settings.size; size > 0 {
				message += fmt.Sprintf(" | 使用中=%d/%d", pool.portsInUseLocked(), size)
			}
			logs = append(logs, proxyPoolLog{source: lease.logSource(), level: "INFO", message: message})
		} else {
			logs = append(logs, proxyPoolLog{source: lease.logSource(), level: "INFO", message: "IP 池归还 | 会话=" + lease.slot})
		}
	}
	if lease.accountID != "" {
		pool.pushLocked(lease.accountID)
	}
	if freed && len(pool.defaultWaiting) > 0 && pool.wakeAt.IsZero() {
		// 端口空出来了：默认出口退回 PROXY 或与别人共用的账户在 proxyPoolWakeDelay 之后合并重推（这段时间里多半有 Worker 来占它）
		pool.wakeAt = now.Add(proxyPoolWakeDelay)
	}
	pool.wakeDefaultsLocked(now)
	return logs
}

// URL 返回租约的上游（直通租约为当时的 PROXY，可能为空）
func (lease *proxyPoolLease) URL() string {
	if lease == nil {
		return ""
	}
	return lease.url
}

// Pooled 判断租约是不是 IP 池分配的（是时调用方把这次启动或登录的代理改成 URL()）
func (lease *proxyPoolLease) Pooled() bool {
	return lease != nil && lease.pooled
}

// Slot 返回端口号或会话号（直通租约为空）
func (lease *proxyPoolLease) Slot() string {
	if lease == nil {
		return ""
	}
	return lease.slot
}

// attach 在 Worker 启动成功后登记关闭回调（OnClosed(release)，与本机中转释放同一时机），并清零端口的规则 B 计数与会话的
// 连通失败计数（不动规则 A：规则 A 的“没有成功”只看生成首事件）。租约从这时起参与账户绑定（见 bindingLeaseLocked）：
// 新浏览器启动期间 Go 侧仍走正在服务的旧浏览器的出口，WAA proof 与 RPC 出自同一个 IP
func (lease *proxyPoolLease) attach(worker proxyPoolCloser) {
	if lease == nil || lease.pool == nil {
		return
	}
	pool := lease.pool
	pool.mu.Lock()
	if !lease.released {
		lease.attached = true
		if lease.pooled {
			if lease.port != 0 {
				pool.portLocked(lease.port).upstreamFails = 0
			} else if lease.accountID != "" {
				if session := pool.sessions[lease.accountID]; session != nil {
					session.strikes, session.firstStrike = 0, time.Time{}
				}
			}
		}
		if lease.accountID != "" {
			pool.pushLocked(lease.accountID)
		}
	}
	pool.mu.Unlock()
	if native, ok := worker.(*aistudio.NativeWorker); ok && native == nil || worker == nil {
		return
	}
	worker.OnClosed(lease.release)
}

// launchFailed 在 Worker 启动失败后归还租约；canceled 为真（调用方按 ctx.Err() != nil 传入）或 cause 不是代理上游失败
// （errors.Is(cause, proxyrelay.ErrUpstream) 为假）时不计入端口健康与换会话
func (lease *proxyPoolLease) launchFailed(cause error, canceled bool) {
	if lease == nil || lease.pool == nil {
		return
	}
	pool := lease.pool
	pool.mu.Lock()
	var logs []proxyPoolLog
	var cooled []proxyPoolCooling
	rotated := ""
	if !lease.released && !canceled && cause != nil && lease.pooled && errors.Is(cause, proxyrelay.ErrUpstream) {
		now := pool.now()
		if lease.port != 0 {
			logs, cooled = pool.upstreamFailureLocked(lease, now)
		} else if lease.accountID != "" {
			var rotateLogs []proxyPoolLog
			rotated, rotateLogs = pool.sessionStrikeLocked(lease, now)
			logs = append(logs, rotateLogs...)
		}
	}
	logs = append(logs, pool.releaseLocked(lease)...)
	if rotated != "" {
		logs = append(logs, pool.rotationLogLocked(lease.accountID, lease.logSource(), rotated, "连续 2 次出口连不通"))
	}
	hook := pool.onPortCooled
	pool.mu.Unlock()
	pool.emit(logs)
	if hook != nil {
		for _, cooling := range cooled {
			hook(cooling)
		}
	}
}

// proxyPoolClaim 为已归还的导入租约留下的出口记录：Chrome 导入只在 Go 侧做 OAuth、没有浏览器在用，导入完一个账号就归还端口
// （不占着独占端口等整批添加结束），账户新建后按记下的端口号或会话号登记到账户名下（见 adopt），之后的 Worker 回到同一个 IP
type proxyPoolClaim struct {
	pool    *proxyPool
	key     string // 静态：端口记录所属的 EndpointKey，换了代理商或子账号时不再登记
	port    int    // 静态：独占过的端口
	session string // 动态：导入用的临时会话号
}

// releaseClaim 归还新账户的登录类租约，返回它的出口记录；不是 IP 池分配、与别人共用端口或已记到账户名下时为 nil（可重复调用，方法对 nil 安全）
func (lease *proxyPoolLease) releaseClaim() *proxyPoolClaim {
	if lease == nil || lease.pool == nil {
		return nil
	}
	pool := lease.pool
	pool.mu.Lock()
	var claim *proxyPoolClaim
	if !lease.released && lease.pooled && lease.accountID == "" {
		switch {
		case lease.port != 0 && !lease.shared && pool.currentStaticLocked(lease):
			claim = &proxyPoolClaim{pool: pool, key: pool.portsKey, port: lease.port}
		case lease.port == 0 && lease.tempSession != "":
			claim = &proxyPoolClaim{pool: pool, session: lease.tempSession}
		}
	}
	logs := pool.releaseLocked(lease)
	pool.mu.Unlock()
	pool.emit(logs)
	return claim
}

// adopt 账户新建成功后按记下的出口登记到账户名下（与登录租约的 adopt 相同）：静态在端口仍属于同一代理商、在段内且没有被其他键持有时
// 记亲和与端口归属（归还后已分给别人的不抢），动态把临时会话号存成账户的基础会话号。方法对 nil 安全
func (claim *proxyPoolClaim) adopt(accountID string) {
	if claim == nil || claim.pool == nil || accountID == "" {
		return
	}
	pool := claim.pool
	pool.mu.Lock()
	defer pool.mu.Unlock()
	now := pool.now()
	switch {
	case claim.port != 0:
		if claim.key == "" || claim.key != pool.portsKey || !pool.inRangeLocked(claim.port) {
			return
		}
		for key := range pool.portLocked(claim.port).holders {
			if key != accountID {
				return
			}
		}
		pool.recordOwnerLocked(accountID, claim.port, now)
		pool.markDirtyLocked()
	case claim.session != "":
		pool.sessions[accountID] = &proxyPoolSession{base: claim.session, usedAt: now}
		pool.markDirtyLocked()
	default:
		return
	}
	pool.pushLocked(accountID)
}

// adopt 新账户登录成功后把登录租约记到账户名下：静态记亲和与端口归属，动态把登录用的临时会话号存成账户的基础会话号，
// 登录与之后的 Worker 用同一个 IP
func (lease *proxyPoolLease) adopt(accountID string) {
	if lease == nil || lease.pool == nil || accountID == "" {
		return
	}
	pool := lease.pool
	pool.mu.Lock()
	defer pool.mu.Unlock()
	if lease.released || lease.accountID != "" {
		return
	}
	now := pool.now()
	lease.accountID = accountID
	pool.byAccount[accountID] = append(pool.byAccount[accountID], lease)
	if lease.pooled {
		if lease.port != 0 && !lease.shared {
			// 端口不足时共用的端口归别人，不记成这个账户的
			pool.recordOwnerLocked(accountID, lease.port, now)
			pool.markDirtyLocked()
		} else if lease.port == 0 && lease.tempSession != "" {
			pool.sessions[accountID] = &proxyPoolSession{base: lease.tempSession, usedAt: now}
			pool.markDirtyLocked()
		}
	}
	pool.pushLocked(accountID)
}

// rateLimited 记一次生成本身的裸限流 429：只在账户绑定所用的活动租约（生成请求走的出口，见 bindingLeaseLocked）是池分配的
// 段内静态端口时计入端口（hot_ports 照记每一次）。wasBad 为这次之前 429 治理已把账号标为坏号（调用方在坏号记忆记下这一次之前取）。
// 规则 A 只计入被拒像是 IP 问题的账号（strikeLocked：自最近一次成功以来只在这个端口上被拒、第一次被拒前还不是坏号），并要求其中
// 至少一个近期在别的出口上成功过（successes）：坏号记忆的依据是裸 429 多半跟着账号走，额度用完的账号在端口换手后接连被拒
// 不能算成 IP 的问题
func (pool *proxyPool) rateLimited(accountID string, wasBad bool) {
	if pool == nil || accountID == "" {
		return
	}
	pool.mu.Lock()
	now := pool.now()
	if pool.settings.size == 0 {
		pool.mu.Unlock()
		return
	}
	lease := pool.bindingLeaseLocked(accountID)
	port := 0
	if lease != nil && lease.pooled && lease.port != 0 && pool.currentStaticLocked(lease) {
		port = lease.port
	}
	counted := pool.strikeLocked(accountID, port, wasBad, now)
	if port == 0 {
		pool.mu.Unlock()
		return
	}
	state := pool.portLocked(port)
	state.hits = append(pruneProxyPoolFailures(state.hits, now), proxyPoolFailure{account: accountID, at: now})
	if len(state.hits) > proxyPoolHitsPerPort {
		state.hits = state.hits[len(state.hits)-proxyPoolHitsPerPort:]
	}
	failures := pruneProxyPoolFailures(state.failures, now)
	failures = slices.DeleteFunc(failures, func(failure proxyPoolFailure) bool { return failure.account == accountID })
	if counted {
		success, ok := pool.successes[accountID]
		proven := ok && success.port != port && now.Sub(success.at) < proxyPoolProvenWithin
		failures = append(failures, proxyPoolFailure{account: accountID, at: now, proven: proven})
	}
	state.failures = failures
	var logs []proxyPoolLog
	var cooled []proxyPoolCooling
	if len(state.failures) >= proxyPoolFailureAccounts && proxyPoolFailuresProven(state.failures) && !pool.coolingLocked(port, now) {
		reason := fmt.Sprintf("%d 分钟内 %d 个账号返回限流且没有成功", int(proxyPoolFailureWindow/time.Minute), len(state.failures))
		logs, cooled = pool.coolLocked(port, proxyPoolRuleRateLimited, reason, now)
	}
	pool.wakeDefaultsLocked(now)
	hook := pool.onPortCooled
	pool.mu.Unlock()
	pool.emit(logs)
	if hook != nil {
		for _, cooling := range cooled {
			hook(cooling)
		}
	}
}

// strikeLocked 记下账户在出口 port（0 为不经 IP 池端口的出口）上的一次裸 429，返回它能否计入这个端口的规则 A：
// 自最近一次成功以来只在这个端口上被拒，且第一次被拒之前还不是坏号（wasBad 为这次之前的坏号状态）。第一次在另一个出口上被拒时
// （被拒跟着账号走）把它从第一个端口的规则 A 记录里删掉。调用方持有 pool.mu
func (pool *proxyPool) strikeLocked(accountID string, port int, wasBad bool, now time.Time) bool {
	strike := pool.strikes[accountID]
	if strike == nil || now.Sub(strike.at) >= proxyPoolStrikeKeep {
		strike = &proxyPoolStrike{port: port, tainted: wasBad}
		pool.strikes[accountID] = strike
	}
	strike.at = now
	if strike.port != port && !strike.spread {
		strike.spread = true
		if state := pool.ports[strike.port]; state != nil {
			state.failures = slices.DeleteFunc(state.failures, func(failure proxyPoolFailure) bool { return failure.account == accountID })
		}
	}
	return port != 0 && !strike.spread && !strike.tainted
}

// proxyPoolFailuresProven 判断规则 A 的记录里至少有一个账号近期在别的出口上成功过
func proxyPoolFailuresProven(failures []proxyPoolFailure) bool {
	return slices.ContainsFunc(failures, func(failure proxyPoolFailure) bool { return failure.proven })
}

// succeeded 记一次生成成功（收到上游首个事件）：静态 IP 下清除账户的裸限流记录（strikes）、记下这次成功的出口（successes）；
// 端口同样只归到绑定所用的池分配租约，清空端口的规则 A 记录
func (pool *proxyPool) succeeded(accountID string) {
	if pool == nil || accountID == "" {
		return
	}
	pool.mu.Lock()
	defer pool.mu.Unlock()
	lease := pool.bindingLeaseLocked(accountID)
	delete(pool.strikes, accountID)
	if pool.settings.size > 0 {
		port := 0
		if lease != nil && lease.pooled && lease.port != 0 && pool.currentStaticLocked(lease) {
			port = lease.port
		}
		pool.successes[accountID] = proxyPoolSuccess{port: port, at: pool.now()}
	}
	if lease == nil || !lease.pooled || lease.port == 0 {
		return
	}
	state := pool.ports[lease.port]
	if state == nil {
		return
	}
	state.lastGenSuccess = pool.now()
	if len(state.failures) > 0 {
		state.failures = nil
	}
}

// upstreamFailureLocked 规则 B：端口上连续 proxyPoolUpstreamFails 次代理上游失败的启动失败（不论账号）时冷却端口，
// 并清除归属指向该端口的亲和
func (pool *proxyPool) upstreamFailureLocked(lease *proxyPoolLease, now time.Time) ([]proxyPoolLog, []proxyPoolCooling) {
	if !pool.currentStaticLocked(lease) {
		return nil, nil
	}
	state := pool.portLocked(lease.port)
	state.upstreamFails++
	logs := []proxyPoolLog{{source: lease.logSource(), level: "WARN", message: fmt.Sprintf(
		"IP 池出口连不通 | 端口=%d | 连续=%d/%d", lease.port, state.upstreamFails, proxyPoolUpstreamFails)}}
	if state.upstreamFails < proxyPoolUpstreamFails || pool.coolingLocked(lease.port, now) {
		return logs, nil
	}
	coolLogs, cooled := pool.coolLocked(lease.port, proxyPoolRuleUpstream, fmt.Sprintf("连续 %d 次出口连不通", state.upstreamFails), now)
	logs = append(logs, coolLogs...)
	if len(cooled) > 0 {
		for accountID, affinity := range pool.affinity {
			if affinity.port == lease.port {
				delete(pool.affinity, accountID)
			}
		}
		state.lastAccount = ""
		pool.markDirtyLocked()
	}
	return logs, cooled
}

// coolLocked 冷却端口（整体故障保护未触发时）：档位翻倍、上限 2 小时，重推落在它上面的默认出口；冷却不影响已在用它的浏览器
func (pool *proxyPool) coolLocked(port int, rule, reason string, now time.Time) ([]proxyPoolLog, []proxyPoolCooling) {
	state := pool.portLocked(port)
	if errored, inUse := pool.erroredPortsLocked(port, now); errored >= max(proxyPoolSystemicMin, int(float64(inUse)*proxyPoolSystemicRatio+0.999)) {
		var logs []proxyPoolLog
		if pool.systemicLogged.IsZero() || now.Sub(pool.systemicLogged) >= proxyPoolFailureWindow {
			pool.systemicLogged = now
			logs = append(logs, proxyPoolLog{source: "service", level: "WARN", durable: true, message: fmt.Sprintf(
				"IP 池多数端口同时出错，按上游或代理商整体问题处理，暂不冷却端口 | 出错端口=%d/%d", errored, inUse)})
		}
		return logs, nil
	}
	if !state.cooledAt.IsZero() && now.Sub(state.cooledAt) >= proxyPoolLevelResetAfter {
		state.coolLevel = 0
	}
	state.coolLevel++
	duration := proxyPoolCoolBase
	for level := 1; level < state.coolLevel && duration < proxyPoolCoolMax; level++ {
		duration *= 2
	}
	duration = min(duration, proxyPoolCoolMax)
	state.coolUntil = now.Add(duration)
	state.cooledAt = now
	cooling := proxyPoolCooling{Port: port, Until: state.coolUntil, Level: state.coolLevel, Rule: rule}
	if rule == proxyPoolRuleRateLimited {
		for _, failure := range state.failures {
			cooling.RateLimitedAccounts = append(cooling.RateLimitedAccounts, failure.account)
		}
	}
	cooling.Holders = pool.portHolderAccountsLocked(port)
	cooling.Workers = pool.portWorkerAccountsLocked(cooling.Holders, port)
	state.failures = nil
	state.upstreamFails = 0
	pool.markDirtyLocked()
	pool.pushLocked(pool.defaultUsersOfLocked(port, "")...)
	logs := []proxyPoolLog{{source: "service", level: "WARN", durable: true, message: fmt.Sprintf(
		"IP 池端口冷却 | 端口=%d | 第 %d 档 | 时长=%s | 恢复=%s | 原因=%s",
		port, state.coolLevel, duration, state.coolUntil.UTC().Format(time.RFC3339), reason)}}
	return logs, []proxyPoolCooling{cooling}
}

// erroredPortsLocked 返回段内“有规则 A/B 记录且没有成功、或正在冷却”的端口数（含 port）与使用中端口数，供整体故障保护
func (pool *proxyPool) erroredPortsLocked(port int, now time.Time) (int, int) {
	errored, inUse := 0, 0
	for candidate, state := range pool.ports {
		if !pool.inRangeLocked(candidate) {
			continue
		}
		if len(state.holders) > 0 {
			inUse++
		}
		state.failures = pruneProxyPoolFailures(state.failures, now)
		if candidate == port || pool.coolingLocked(candidate, now) || state.upstreamFails > 0 || len(state.failures) > 0 {
			errored++
		}
	}
	return errored, inUse
}

// sessionStrikeLocked 动态 IP：同一账户 30 分钟内 2 次代理上游失败的启动失败、距上次换会话不少于 10 分钟时换会话（代号 +1，持久化）。
// 返回换会话前的会话号（没有换时为空）与日志
func (pool *proxyPool) sessionStrikeLocked(lease *proxyPoolLease, now time.Time) (string, []proxyPoolLog) {
	if pool.settings.cfg.Mode != config.ProxyPoolDynamic || pool.sessionURLLocked(lease.slot) != lease.url {
		return "", nil
	}
	session := pool.sessionStateLocked(lease.accountID)
	if session.strikes > 0 && now.Sub(session.firstStrike) > proxyPoolSessionWindow {
		session.strikes = 0
	}
	if session.strikes == 0 {
		session.firstStrike = now
	}
	session.strikes++
	logs := []proxyPoolLog{{source: lease.logSource(), level: "WARN", message: fmt.Sprintf(
		"IP 池出口连不通 | 会话=%s | 连续=%d/%d", lease.slot, session.strikes, proxyPoolUpstreamFails)}}
	if session.strikes < proxyPoolUpstreamFails || !session.rotatedAt.IsZero() && now.Sub(session.rotatedAt) < proxyPoolRotateInterval {
		return "", logs
	}
	previous := pool.sessionLocked(lease.accountID)
	pool.rotateLocked(lease.accountID, now)
	return previous, logs
}

// rotateLocked 把账户的会话代号 +1（超过上限回到 1）并持久化
func (pool *proxyPool) rotateLocked(accountID string, now time.Time) {
	session := pool.sessionStateLocked(accountID)
	session.generation++
	if session.generation > proxyPoolMaxGeneration {
		session.generation = 1
	}
	session.rotatedAt = now
	session.strikes, session.firstStrike = 0, time.Time{}
	pool.stats.rotations++
	pool.markDirtyLocked()
}

// rotationLogLocked 为换会话的日志（在推送之后调用：没有活动租约的账户已立即切到新会话）
func (pool *proxyPool) rotationLogLocked(accountID, source, previous, reason string) proxyPoolLog {
	effect := "下次启动 Worker"
	if pool.latestLeaseLocked(accountID) == nil {
		effect = "立即（没有 Worker）"
	}
	return proxyPoolLog{source: source, level: "WARN", message: fmt.Sprintf(
		"IP 池换会话 | 会话=%s → %s | 原因=%s | 生效=%s", previous, pool.sessionLocked(accountID), reason, effect)}
}

// rotateSession 由“429 治理”判定账户出口受限时调用（合并时接线，见 applyRateLimitCooldown）：key 为判定成立的出口（这次请求租约的出口键），
// 只在它就是账户当前会话的出口（sessionEgressKeyLocked：动态 IP、账户没有自己的代理，正在服务的是 IP 池按当前设置分配的这个会话，
// 或没有活动租约、默认出口就是它）时换会话，受 10 分钟最短间隔约束；返回是否换了。
// 正在服务的 Worker 走直通出口（开启 IP 池或从静态切到动态之前启动、仍走 PROXY 或旧端口）时判定的是那个共用出口，不是账户的会话：
// 换一个没用过的会话只会刷日志、占掉 10 分钟间隔（推迟真正的“连续出口连不通”换会话）。每个会话各自是独立出口、只有一个账号，
// 凑不够出口判定的门槛（egressMinAccounts），所以单个会话被上游限流（裸 429）实际不会在这里换会话
func (pool *proxyPool) rotateSession(accountID, key, reason string) bool {
	if pool == nil || accountID == "" {
		return false
	}
	pool.mu.Lock()
	now := pool.now()
	if current, ok := pool.sessionEgressKeyLocked(accountID); !ok || current != key {
		pool.mu.Unlock()
		return false
	}
	session := pool.sessionStateLocked(accountID)
	if !session.rotatedAt.IsZero() && now.Sub(session.rotatedAt) < proxyPoolRotateInterval {
		pool.mu.Unlock()
		return false
	}
	previous := pool.sessionLocked(accountID)
	pool.rotateLocked(accountID, now)
	pool.pushLocked(accountID)
	source := accountID
	if lease := pool.latestLeaseLocked(accountID); lease != nil {
		source = lease.logSource()
	}
	log := pool.rotationLogLocked(accountID, source, previous, reason)
	pool.mu.Unlock()
	pool.emit([]proxyPoolLog{log})
	return true
}

// sessionEgressKeyLocked 返回账户当前会话作为出口时的出口键（与账户池挂通道的键相同，见 aistudio.ProxyBinding.EgressKey）：
// 动态 IP、账户没有自己的代理，绑定所用的租约是 IP 池按当前设置分配的当前会话（不是直通租约、静态端口、换会话之前的旧会话或旧地址），
// 或没有活动租约（默认出口就是当前会话）。不是时 ok 为假。调用方持有 pool.mu
func (pool *proxyPool) sessionEgressKeyLocked(accountID string) (key string, ok bool) {
	if pool.settings.cfg.Mode != config.ProxyPoolDynamic {
		return "", false
	}
	if state := pool.accounts[accountID]; state != nil && state.proxy != "" {
		return "", false
	}
	session := pool.sessionLocked(accountID)
	url := pool.sessionURLLocked(session)
	if url == "" {
		return "", false
	}
	tag := ""
	if pool.settings.sessionInPassword {
		tag = session
	}
	if lease := pool.bindingLeaseLocked(accountID); lease != nil {
		if !lease.pooled || lease.port != 0 || lease.slot != session || lease.url != url {
			return "", false
		}
		tag = lease.egressTag
	}
	return aistudio.ProxyBinding{URL: url, EgressTag: tag}.EgressKey(), true
}

// slotLabel 返回账户当前出口的日志字段（“端口=10023”“会话=3fa9c01b7d2e”），没有时为空；供合并后的 429 日志
func (pool *proxyPool) slotLabel(accountID string) string {
	if pool == nil || accountID == "" {
		return ""
	}
	pool.mu.Lock()
	defer pool.mu.Unlock()
	if lease := pool.bindingLeaseLocked(accountID); lease != nil {
		return lease.slotFieldLocked()
	}
	state := pool.accounts[accountID]
	if state == nil || state.binding == nil || state.binding.Slot == "" {
		return ""
	}
	if pool.settings.cfg.Mode == config.ProxyPoolStatic {
		return "端口=" + state.binding.Slot
	}
	return "会话=" + state.binding.Slot
}

// logField 返回这个租约出口的日志字段（与 slotLabel 同样写法：“端口=10023”“会话=3fa9c01b7d2e”），不是 IP 池分配的为空；
// 启动失败归还之前取（之后 slotLabel 给的是账户的默认出口，不是这次启动用的端口）。方法对 nil 安全
func (lease *proxyPoolLease) logField() string {
	if lease == nil || lease.pool == nil {
		return ""
	}
	lease.pool.mu.Lock()
	defer lease.pool.mu.Unlock()
	return lease.slotFieldLocked()
}

// slotFieldLocked 为 logField 与 slotLabel 的实现；调用方持有 pool.mu
func (lease *proxyPoolLease) slotFieldLocked() string {
	switch {
	case !lease.pooled || lease.slot == "":
		return ""
	case lease.port != 0:
		return "端口=" + lease.slot
	default:
		return "会话=" + lease.slot
	}
}

// proxyPoolLogSuffix 把 slotLabel 或 logField 的字段写成日志行末尾的“ | 端口=10023”；字段为空时为空串。
// 追加在行末，诊断脚本按行首文案与“ | ”分隔的键值解析，不受影响
func proxyPoolLogSuffix(field string) string {
	if field == "" {
		return ""
	}
	return " | " + field
}

// poolsAccount 判断 IP 池开启（静态有可分配的端口段、动态有地址模板）且账户没有自己的代理：它的 Worker 启动时由 IP 池另分出口
// （静态独占一个端口、动态用账户自己的会话），出口限流的冷启动闸门按它现在的默认出口判断没有意义（见 egressColdStartGate）。
// 账户还没登记到 IP 池（不在当前账户池）时为假。方法对 nil 安全
func (pool *proxyPool) poolsAccount(accountID string) bool {
	if pool == nil || accountID == "" {
		return false
	}
	pool.mu.Lock()
	defer pool.mu.Unlock()
	return pool.poolingLocked() && pool.poolsAccountLocked(accountID)
}

// pooledAccounts 返回 ids 里 poolsAccount 为真的账户（一次加锁）；IP 池关闭或一个都没有时为 nil。
// 出口限流期间剔除受限出口上的备用账户时据此留下由 IP 池分配出口的账户（见 filterLimitedStandby）。方法对 nil 安全
func (pool *proxyPool) pooledAccounts(ids []string) map[string]struct{} {
	if pool == nil || len(ids) == 0 {
		return nil
	}
	pool.mu.Lock()
	defer pool.mu.Unlock()
	if !pool.poolingLocked() {
		return nil
	}
	var pooled map[string]struct{}
	for _, accountID := range ids {
		if accountID == "" || !pool.poolsAccountLocked(accountID) {
			continue
		}
		if pooled == nil {
			pooled = make(map[string]struct{}, len(ids))
		}
		pooled[accountID] = struct{}{}
	}
	return pooled
}

// poolingLocked 判断 IP 池开启且能分出口：静态有端口段、动态有地址模板。调用方持有 pool.mu
func (pool *proxyPool) poolingLocked() bool {
	switch pool.settings.cfg.Mode {
	case config.ProxyPoolStatic:
		return pool.settings.size > 0
	case config.ProxyPoolDynamic:
		return pool.settings.sessionTemplate != ""
	default:
		return false
	}
}

// poolsAccountLocked 判断账户已登记到 IP 池且没有自己的代理（不看 IP 池是否开启，见 poolingLocked）。调用方持有 pool.mu
func (pool *proxyPool) poolsAccountLocked(accountID string) bool {
	state := pool.accounts[accountID]
	return state != nil && state.known && state.proxy == ""
}

// pinnedDefault 返回自己的代理与 PROXY 规范值相同、因此不使用 IP 池的账户 ID（“改为跟随默认代理”用），按 ID 排序
func (pool *proxyPool) pinnedDefault() []string {
	if pool == nil {
		return nil
	}
	pool.mu.Lock()
	defer pool.mu.Unlock()
	var ids []string
	for id, state := range pool.accounts {
		if state.pinned {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}

// proxyPoolPreview 为 loginPreview 的结果：url 为上游（可能带账号密码，只交给网络层），slot 为端口号或临时会话号
type proxyPoolPreview struct {
	cfg  config.ProxyPool
	slot string
	url  string
}

// loginPreview 返回新账户登录此刻会分到的出口（“测试代理”留空时用，不占用、不记录）：静态为按挑选次序的第一个可分配端口，
// 没有时为与别人共用的端口；动态为一个新的临时会话号（与登录相同的形式）。IP 池关闭或没有可用端口时 ok 为假
func (pool *proxyPool) loginPreview() (proxyPoolPreview, bool) {
	if pool == nil {
		return proxyPoolPreview{}, false
	}
	pool.mu.Lock()
	defer pool.mu.Unlock()
	preview := proxyPoolPreview{cfg: pool.settings.cfg}
	switch pool.settings.cfg.Mode {
	case config.ProxyPoolStatic:
		now := pool.now()
		port := pool.pickFreeLocked(now)
		if port == 0 {
			port, _ = pool.pickSharedLocked(now)
		}
		if port == 0 {
			return proxyPoolPreview{}, false
		}
		preview.slot, preview.url = strconv.Itoa(port), pool.staticURLLocked(port)
	case config.ProxyPoolDynamic:
		preview.slot = newProxyPoolSession(pool.random, "n", proxyPoolSessionHashLen-1)
		preview.url = pool.sessionURLLocked(preview.slot)
	}
	return preview, preview.url != ""
}

// accountPreview 返回账户清空自己的代理后 IP 池会给它的出口（账户编辑对话框里清空代理再点“测试代理”时用，不占用、不记录）：
// 静态为它的默认出口（归自己的端口 > 探测序列上第一个可用的端口；都没有、PROXY 不为空时 ok 为假，回到 PROXY），之后启动的 Worker
// 可能另分端口；动态为账户的会话号。IP 池关闭时 ok 为假
func (pool *proxyPool) accountPreview(accountID string) (proxyPoolPreview, bool) {
	if pool == nil || accountID == "" {
		return proxyPoolPreview{}, false
	}
	pool.mu.Lock()
	defer pool.mu.Unlock()
	preview := proxyPoolPreview{cfg: pool.settings.cfg}
	switch pool.settings.cfg.Mode {
	case config.ProxyPoolStatic:
		// 只读：探测序列缓存在临时对象上，不改账户记录、默认出口与 defaultWaiting
		state := proxyPoolAccount{}
		port, _ := pool.defaultPortLocked(accountID, &state, &proxyPoolBatch{now: pool.now()})
		if port == 0 {
			return proxyPoolPreview{}, false
		}
		preview.slot, preview.url = strconv.Itoa(port), pool.staticURLLocked(port)
	case config.ProxyPoolDynamic:
		preview.slot = pool.sessionLocked(accountID)
		preview.url = pool.sessionURLLocked(preview.slot)
	}
	return preview, preview.url != ""
}

// markPinned 给自己的代理与 PROXY 规范值相同（因此不使用 IP 池）的账户标上 ProxyPoolPinned（IP 池开启时）：与状态接口的
// pinned_default、“改为跟随默认代理”用同一份判断（当前生效的 PROXY），账户列表不再自己比较字符串
func (pool *proxyPool) markPinned(accounts []api.AdminAccount) {
	if pool == nil || len(accounts) == 0 {
		return
	}
	pool.mu.Lock()
	defer pool.mu.Unlock()
	if pool.pinned == 0 || !pool.settings.cfg.Enabled() {
		return
	}
	for index := range accounts {
		if state := pool.accounts[accounts[index].ID]; state != nil && state.pinned {
			accounts[index].ProxyPoolPinned = true
		}
	}
}

// probeSession 返回抽测用的临时会话号："t" + 14 位随机十六进制，长度等于程序可能生成的最大值 15，抽测能提前发现代理商的长度限制
func (pool *proxyPool) probeSession() string {
	random := io.Reader(rand.Reader)
	if pool != nil && pool.random != nil {
		random = pool.random
	}
	return newProxyPoolSession(random, "t", 14)
}

// status 返回 IP 池的实时状态：IP 池关闭且没有浏览器在用池出口时返回 nil；不含账号密码
func (pool *proxyPool) status() *api.AdminProxyPool {
	if pool == nil {
		return nil
	}
	pool.mu.Lock()
	owner := pool.owner
	needIDs := owner != nil && len(pool.byAccount) > 0
	pool.mu.Unlock()
	var known map[string]struct{}
	if needIDs {
		ids := owner.AccountIDs()
		known = make(map[string]struct{}, len(ids))
		for _, id := range ids {
			known[id] = struct{}{}
		}
	}
	pool.mu.Lock()
	now := pool.now()
	// 顺带检查冷却到期、其他进程锁的重试时间到：默认出口在等的账户重推（定时器之外的兜底）
	pool.wakeDefaultsLocked(now)
	cfg := pool.settings.cfg
	if !cfg.Enabled() && pool.pooledLeases == 0 {
		pool.mu.Unlock()
		return nil
	}
	status := &api.AdminProxyPool{
		Mode: cfg.Mode, Endpoint: pool.settings.endpoint, Leases: pool.pooledLeases, Capacity: pool.settings.capacity,
		PinnedDefault: pool.pinned, Rotations: pool.stats.rotations,
		AffinityHits: pool.stats.affinityHits, AffinityMisses: pool.stats.affinityMisses,
	}
	if pool.relayLimit != nil && cfg.Enabled() {
		status.RelayLimit = pool.relayLimit()
	}
	if cfg.Mode == config.ProxyPoolStatic {
		status.PortStart, status.PortEnd = cfg.Ports()
		status.Total = cfg.Size()
		status.InUse = pool.portsInUseLocked()
		status.Cooling = pool.coolingCountLocked(now)
		for port := status.PortStart; port <= status.PortEnd; port++ {
			if pool.external[port] > 0 || pool.lockedElsewhere[port].After(now) {
				status.External++
			}
			if state := pool.ports[port]; state != nil && len(state.holders) >= 2 {
				status.Shared++
			}
		}
		status.CoolingPorts = pool.coolingPortsLocked(now)
		status.HotPorts = pool.hotPortsLocked(now)
	} else if cfg.Mode == config.ProxyPoolDynamic {
		status.InUse = pool.sessionsInUseLocked()
	}
	orphaned := 0
	for _, leases := range pool.byKey {
		for _, lease := range leases {
			if lease.pooled {
				if !pool.leaseCurrentLocked(lease) {
					status.Stale++
				}
				status.Assignments = append(status.Assignments, api.AdminProxyPoolAssignment{
					AccountID: lease.accountID, Slot: lease.slot, Kind: proxyPoolStatusKind(lease.kind),
				})
			} else if cfg.Enabled() && lease.kind == proxyPoolKindWorker {
				status.Unpooled++
			}
			if known != nil && lease.accountID != "" {
				if _, ok := known[lease.accountID]; !ok {
					orphaned++
				}
			}
		}
	}
	status.Orphaned = orphaned
	sort.Slice(status.Assignments, func(i, j int) bool {
		left, right := status.Assignments[i], status.Assignments[j]
		if left.Slot != right.Slot {
			return proxyPoolSlotLess(left.Slot, right.Slot)
		}
		if left.AccountID != right.AccountID {
			return left.AccountID < right.AccountID
		}
		return left.Kind < right.Kind
	})
	if pool.check != nil {
		check := *pool.check
		check.Duplicates = slices.Clone(pool.check.Duplicates)
		check.Failed = slices.Clone(pool.check.Failed)
		status.Check = &check
	}
	var logs []proxyPoolLog
	if orphaned > pool.orphanedLogged {
		logs = append(logs, proxyPoolLog{source: "service", level: "WARN", message: fmt.Sprintf("IP 池租约泄漏 | 已删除账户仍占用=%d", orphaned)})
	}
	if known != nil {
		pool.orphanedLogged = orphaned
	}
	logs = append(logs, pool.overAllocationLogLocked(now)...)
	pool.mu.Unlock()
	pool.emit(logs)
	return status
}

// coolingPortsLocked 返回段内冷却中的端口（按恢复时间，最多 proxyPoolCoolingPortsLimit 个）
func (pool *proxyPool) coolingPortsLocked(now time.Time) []api.AdminProxyPoolPort {
	var ports []api.AdminProxyPoolPort
	for port, state := range pool.ports {
		if pool.inRangeLocked(port) && pool.coolingLocked(port, now) {
			ports = append(ports, api.AdminProxyPoolPort{Port: port, Until: state.coolUntil.UTC(), Level: state.coolLevel})
		}
	}
	sort.Slice(ports, func(i, j int) bool {
		if !ports[i].Until.Equal(ports[j].Until) {
			return ports[i].Until.Before(ports[j].Until)
		}
		return ports[i].Port < ports[j].Port
	})
	if len(ports) > proxyPoolCoolingPortsLimit {
		ports = ports[:proxyPoolCoolingPortsLimit]
	}
	return ports
}

// hotPortsLocked 返回最近 10 分钟裸 429 最多的端口（最多 proxyPoolHotPortsLimit 个）
func (pool *proxyPool) hotPortsLocked(now time.Time) []api.AdminProxyPoolHotPort {
	var ports []api.AdminProxyPoolHotPort
	for port, state := range pool.ports {
		if !pool.inRangeLocked(port) {
			continue
		}
		hits := pruneProxyPoolFailures(state.hits, now)
		state.hits = hits
		if len(hits) == 0 {
			continue
		}
		accounts := make(map[string]struct{}, len(hits))
		for _, hit := range hits {
			accounts[hit.account] = struct{}{}
		}
		ports = append(ports, api.AdminProxyPoolHotPort{Port: port, RateLimited: len(hits), Accounts: len(accounts)})
	}
	sort.Slice(ports, func(i, j int) bool {
		if ports[i].RateLimited != ports[j].RateLimited {
			return ports[i].RateLimited > ports[j].RateLimited
		}
		return ports[i].Port < ports[j].Port
	})
	if len(ports) > proxyPoolHotPortsLimit {
		ports = ports[:proxyPoolHotPortsLimit]
	}
	return ports
}

// overAllocationLogLocked 池租约数持续 5 分钟大于容量加登录/验证并发上限时 WARN 一次（回落后重新计时）
func (pool *proxyPool) overAllocationLogLocked(now time.Time) []proxyPoolLog {
	limit := pool.settings.capacity + proxyPoolLoginAllowance
	if pool.settings.capacity <= 0 || pool.pooledLeases <= limit {
		pool.overSince, pool.overLogged = time.Time{}, false
		return nil
	}
	if pool.overSince.IsZero() {
		pool.overSince = now
		return nil
	}
	if pool.overLogged || now.Sub(pool.overSince) < proxyPoolOverAllocWindow {
		return nil
	}
	pool.overLogged = true
	return []proxyPoolLog{{source: "service", level: "WARN", message: fmt.Sprintf(
		"IP 池占用超过可能的浏览器数 | 占用=%d | 需要=%d", pool.pooledLeases, limit)}}
}

// syncOwnerLocked 经 UpdateProxyBindings 对当前账户池的全部账户写绑定：记下各账户的固定代理（外部占用、pinned），
// full 为真或账户代理有变化、或是 focus 时重算绑定，其余沿用上次推送的绑定；外部占用因此变化时再全部重算一遍。
// 不在账户池里的账户记录随之丢弃。每一遍共用一份端口快照（外部占用在这一遍里变了时，下一遍按新快照全部重算）
func (pool *proxyPool) syncOwnerLocked(full bool, focus string) {
	if pool.owner == nil {
		return
	}
	for pass := 0; pass < 2; pass++ {
		seen := make(map[string]struct{}, len(pool.accounts))
		externalChanged := false
		batch := &proxyPoolBatch{now: pool.now(), snapshot: full}
		pool.owner.UpdateProxyBindings(func(accountID, accountProxy string) *aistudio.ProxyBinding {
			seen[accountID] = struct{}{}
			state := pool.accountLocked(accountID)
			changed := !state.known || state.proxyRaw != accountProxy
			if changed && pool.observeAccountProxyLocked(accountID, state, accountProxy) {
				externalChanged = true
			}
			if full || changed || accountID == focus {
				state.binding = pool.keepBinding(state.binding, pool.bindingLocked(accountID, state, batch))
			}
			return state.binding
		})
		for accountID, state := range pool.accounts {
			if _, ok := seen[accountID]; !ok {
				pool.forgetAccountLocked(accountID, state)
			}
		}
		if !externalChanged {
			return
		}
		full = true
	}
}

// pushLocked 重算这些账户的绑定，变化的经 SetProxyBindings 推给当前账户池（一次加锁）；账户多时共用一份端口快照
func (pool *proxyPool) pushLocked(accountIDs ...string) {
	if len(accountIDs) == 0 {
		return
	}
	pool.pushBatchLocked(&proxyPoolBatch{now: pool.now(), snapshot: len(accountIDs) >= proxyPoolSnapshotAccounts}, accountIDs)
}

// pushBatchLocked 为 pushLocked 的实现：batch 为这一批共用的时间与端口快照
func (pool *proxyPool) pushBatchLocked(batch *proxyPoolBatch, accountIDs []string) {
	updates := make([]aistudio.ProxyBindingUpdate, 0, len(accountIDs))
	seen := make(map[string]struct{}, len(accountIDs))
	for _, accountID := range accountIDs {
		if accountID == "" {
			continue
		}
		if _, ok := seen[accountID]; ok {
			continue
		}
		seen[accountID] = struct{}{}
		state := pool.accountLocked(accountID)
		binding := pool.bindingLocked(accountID, state, batch)
		if proxyBindingEqual(binding, state.binding) {
			continue
		}
		state.binding = binding
		updates = append(updates, aistudio.ProxyBindingUpdate{AccountID: accountID, Binding: binding})
	}
	if pool.owner != nil && len(updates) > 0 {
		pool.owner.SetProxyBindings(updates)
	}
}

// keepBinding 新算出的绑定与上次相同时沿用上次的对象，账户池按指针判断是否变化
func (pool *proxyPool) keepBinding(previous, next *aistudio.ProxyBinding) *aistudio.ProxyBinding {
	if proxyBindingEqual(previous, next) {
		return previous
	}
	return next
}

// proxyPoolBatch 为一批绑定计算共用的上下文：now 为这一批的时间；snapshot 为真时第一次需要静态默认出口时扫描一次端口段
// （proxyPoolDefaultView），之后各账户的探测只读快照。一批计算在同一次加锁内进行，期间端口是否可用不变
type proxyPoolBatch struct {
	now      time.Time
	snapshot bool
	view     *proxyPoolDefaultView
}

// proxyPoolDefaultView 为默认出口的端口快照：usable[i] 为端口 portStart+i 可以做默认出口（见 defaultUsableLocked），count 为可用的个数
type proxyPoolDefaultView struct {
	usable []bool
	count  int
}

// viewLocked 返回这一批的端口快照（snapshot 为假或不是静态 IP 时为 nil，逐个端口检查）
func (batch *proxyPoolBatch) viewLocked(pool *proxyPool) *proxyPoolDefaultView {
	if batch.view == nil && batch.snapshot && pool.settings.size > 0 {
		view := &proxyPoolDefaultView{usable: make([]bool, pool.settings.size)}
		for index := range view.usable {
			if pool.defaultUsableLocked(pool.settings.portStart+index, batch.now) {
				view.usable[index] = true
				view.count++
			}
		}
		batch.view = view
	}
	return batch.view
}

// usableLocked 判断端口可以做默认出口：有快照时读快照（段外为否），否则逐个检查
func (batch *proxyPoolBatch) usableLocked(pool *proxyPool, view *proxyPoolDefaultView, port int) bool {
	if view == nil {
		return pool.defaultUsableLocked(port, batch.now)
	}
	return pool.inRangeLocked(port) && view.usable[port-pool.settings.portStart]
}

// bindingLocked 账户的绑定：账户有自己的代理时为 nil；有活动租约时为绑定所用租约的 URL（Active，见 bindingLeaseLocked）；
// 否则为默认出口
func (pool *proxyPool) bindingLocked(accountID string, state *proxyPoolAccount, batch *proxyPoolBatch) *aistudio.ProxyBinding {
	if state.proxy != "" {
		pool.setDefaultPortLocked(accountID, state, 0, false)
		return nil
	}
	if lease := pool.bindingLeaseLocked(accountID); lease != nil {
		pool.setDefaultPortLocked(accountID, state, 0, false)
		return &aistudio.ProxyBinding{URL: lease.url, Slot: lease.slot, Active: true, EgressTag: lease.egressTag}
	}
	switch pool.settings.cfg.Mode {
	case config.ProxyPoolDynamic:
		pool.setDefaultPortLocked(accountID, state, 0, false)
		session := pool.sessionLocked(accountID)
		url := pool.sessionURLLocked(session)
		if url == "" {
			return nil
		}
		binding := &aistudio.ProxyBinding{URL: url, Slot: session}
		if pool.settings.sessionInPassword {
			binding.EgressTag = session
		}
		return binding
	case config.ProxyPoolStatic:
		port, waiting := pool.defaultPortLocked(accountID, state, batch)
		pool.setDefaultPortLocked(accountID, state, port, waiting)
		if port == 0 {
			return nil
		}
		return &aistudio.ProxyBinding{URL: pool.staticURLLocked(port), Slot: strconv.Itoa(port)}
	default:
		pool.setDefaultPortLocked(accountID, state, 0, false)
		return nil
	}
}

// defaultPortLocked 静态 IP 没有活动租约的账户的默认出口：归自己的端口 > 账户自己的探测序列上第一个可用的端口 > PROXY（返回 0）>
// 哈希端口（与别人共用）。探测序列从哈希位置起、按账户哈希出的与端口数互质的步长前进（见 proxyPoolProbe）：一个端口被占时，落在它上面的
// 账户各自走到不同的下一个端口，不会整批堆到同一个端口上、再在那个端口被占时整批挪走。可用 = 可分配且没有被其他进程锁住的记录
// （见 defaultUsableLocked）。不占独占名额、不改亲和；后两种情况 waiting 为真：端口重新可分配时重推（见 wakeDefaultsLocked）
func (pool *proxyPool) defaultPortLocked(accountID string, state *proxyPoolAccount, batch *proxyPoolBatch) (port int, waiting bool) {
	size := pool.settings.size
	if size == 0 {
		return 0, false
	}
	view := batch.viewLocked(pool)
	if affinity, ok := pool.affinity[accountID]; ok && pool.ownsLocked(accountID, affinity.port) &&
		batch.usableLocked(pool, view, affinity.port) {
		return affinity.port, false
	}
	offset, step := state.probe(accountID, size, pool.settings.steps)
	if view == nil || view.count > 0 {
		index := offset
		for range size {
			if candidate := pool.settings.portStart + index; batch.usableLocked(pool, view, candidate) {
				return candidate, false
			}
			if index += step; index >= size {
				index -= size
			}
		}
	}
	if pool.settings.global != "" {
		return 0, true
	}
	return pool.settings.portStart + offset, true
}

// probe 返回账户在 size 个端口上的探测起点与步长（按端口数缓存）
func (state *proxyPoolAccount) probe(accountID string, size int, steps []int) (offset, step int) {
	if state.probeSize != size || state.probeStep == 0 {
		state.probeOffset, state.probeStep = proxyPoolProbe(accountID, size, steps)
		state.probeSize = size
	}
	return state.probeOffset, state.probeStep
}

// defaultUsableLocked 判断端口可以做默认出口：可分配，且没有“被其他进程锁住”的记录。默认出口不加锁，记录在重试时间之后也不算数，
// 要等本进程真正锁到这个端口、或到期后检测到锁已释放（见 probeLockedElsewhereLocked）才清掉
func (pool *proxyPool) defaultUsableLocked(port int, now time.Time) bool {
	if _, locked := pool.lockedElsewhere[port]; locked {
		return false
	}
	return pool.allocatableLocked(port, now)
}

// setDefaultPortLocked 记下账户的默认出口端口（端口被占或冷却时按 defaultUsers 反查重推）与是否在等可用端口（defaultWaiting）
func (pool *proxyPool) setDefaultPortLocked(accountID string, state *proxyPoolAccount, port int, waiting bool) {
	if waiting {
		pool.defaultWaiting[accountID] = struct{}{}
	} else {
		delete(pool.defaultWaiting, accountID)
	}
	if state.defaultPort == port {
		return
	}
	if users := pool.defaultUsers[state.defaultPort]; users != nil {
		delete(users, accountID)
		if len(users) == 0 {
			delete(pool.defaultUsers, state.defaultPort)
		}
	}
	state.defaultPort = port
	if port != 0 {
		users := pool.defaultUsers[port]
		if users == nil {
			users = make(map[string]struct{})
			pool.defaultUsers[port] = users
		}
		users[accountID] = struct{}{}
	}
}

// defaultUsersOfLocked 返回默认出口落在端口上的账户（不含 except）
func (pool *proxyPool) defaultUsersOfLocked(port int, except string) []string {
	users := pool.defaultUsers[port]
	if len(users) == 0 {
		return nil
	}
	ids := make([]string, 0, len(users))
	for accountID := range users {
		if accountID != except {
			ids = append(ids, accountID)
		}
	}
	sort.Strings(ids)
	return ids
}

// wakeDefaultsLocked 在 recheckAt 到期（归还后合并重推的时刻、冷却到期、其他进程锁的重试时间到）时先检测到期的其他进程锁，
// 再重推 defaultWaiting 里的账户（默认出口退回了 PROXY、或与别人共用哈希端口）；之后按仍在等的账户重新安排下一次检查。
// 只做 map 操作、非阻塞的端口文件锁检测与一次批量推送
func (pool *proxyPool) wakeDefaultsLocked(now time.Time) {
	if !pool.recheckAt.IsZero() && !now.Before(pool.recheckAt) {
		pool.probeLockedElsewhereLocked(now)
		pool.wakeAt = time.Time{}
		pool.repushWaitingLocked(now)
	}
	pool.scheduleRecheckLocked(now)
}

// repushWaitingLocked 重推 defaultWaiting 里的账户：端口段里没有可用端口时它们的结果不变（仍退回 PROXY 或共用哈希端口），
// 不逐个重算；有可用端口时它们按各自的探测序列分散到这些端口上
func (pool *proxyPool) repushWaitingLocked(now time.Time) {
	if len(pool.defaultWaiting) == 0 || pool.settings.cfg.Mode != config.ProxyPoolStatic {
		return
	}
	batch := &proxyPoolBatch{now: now, snapshot: true}
	if view := batch.viewLocked(pool); view == nil || view.count == 0 {
		return
	}
	ids := make([]string, 0, len(pool.defaultWaiting))
	for accountID := range pool.defaultWaiting {
		ids = append(ids, accountID)
	}
	sort.Strings(ids)
	pool.pushBatchLocked(batch, ids)
}

// probeLockedElsewhereLocked 检测重试时间已到的“被其他进程锁住”记录（只检测、不持有）：锁已释放的清掉记录，默认出口又能用它
// （本进程正在共用这个端口时留下锁）；仍被锁住或检测出错的顺延 proxyPoolLockedRetry。锁不可用（出错后的退避期内）时不检测
func (pool *proxyPool) probeLockedElsewhereLocked(now time.Time) {
	if !pool.lockUsableLocked(now) {
		return
	}
	ports := make([]int, 0, len(pool.lockedElsewhere))
	for port, until := range pool.lockedElsewhere {
		if !now.Before(until) {
			ports = append(ports, port)
		}
	}
	sort.Ints(ports)
	for _, port := range ports {
		if state := pool.ports[port]; state != nil && state.lock != nil {
			delete(pool.lockedElsewhere, port)
			continue
		}
		if !pool.inRangeLocked(port) {
			delete(pool.lockedElsewhere, port)
			continue
		}
		lock, locked, err := pool.tryLock(pool.lockDir, pool.settings.key, port)
		if err != nil || !locked {
			pool.lockedElsewhere[port] = now.Add(proxyPoolLockedRetry)
			continue
		}
		delete(pool.lockedElsewhere, port)
		if state := pool.ports[port]; state != nil && len(state.holders) > 0 {
			// 本进程在共用这个端口（当初没锁到）：其他进程已经释放，补上锁
			state.lock = lock
			continue
		}
		_ = lock.Release()
	}
}

// scheduleRecheckLocked 按 defaultWaiting 安排下一次检查：归还后合并重推的时刻（wakeAt）、段内最早到期的冷却与其他进程锁的重试时间
// （已到期、等检测的记录为现在；锁在出错后的退避期内时为退避结束）；没有在等的账户或没有会到期的端口时停掉定时器
func (pool *proxyPool) scheduleRecheckLocked(now time.Time) {
	var next time.Time
	consider := func(at time.Time) {
		if next.IsZero() || at.Before(next) {
			next = at
		}
	}
	if len(pool.defaultWaiting) == 0 || pool.settings.cfg.Mode != config.ProxyPoolStatic {
		pool.wakeAt = time.Time{}
	} else {
		if !pool.wakeAt.IsZero() {
			consider(pool.wakeAt)
		}
		for port, state := range pool.ports {
			if state.coolUntil.After(now) && pool.inRangeLocked(port) {
				consider(state.coolUntil)
			}
		}
		for port, until := range pool.lockedElsewhere {
			if !pool.inRangeLocked(port) {
				continue
			}
			switch {
			case until.After(now):
				consider(until)
			case pool.lockUsableLocked(now):
				consider(now)
			default:
				consider(pool.lockFailedAt.Add(proxyPoolLockErrorRetry))
			}
		}
	}
	if next.IsZero() {
		pool.recheckAt = time.Time{}
		if pool.recheckTimer != nil {
			pool.recheckTimer.Stop()
		}
		pool.recheckArmed = false
		return
	}
	if pool.recheckArmed && next.Equal(pool.recheckAt) {
		return
	}
	pool.recheckAt = next
	delay := next.Sub(now) + proxyPoolRecheckSlack
	if pool.recheckTimer == nil {
		pool.recheckTimer = time.AfterFunc(delay, pool.recheckFired)
	} else {
		pool.recheckTimer.Reset(delay)
	}
	pool.recheckArmed = true
}

// recheckFired 为 recheckTimer 的回调：没有申请、归还与状态查询时，冷却到期、其他进程锁的重试时间到也会重推在等的账户
func (pool *proxyPool) recheckFired() {
	pool.mu.Lock()
	pool.recheckArmed = false
	pool.wakeDefaultsLocked(pool.now())
	pool.mu.Unlock()
}

// observeAccountProxyLocked 记下账户自己的代理，增量更新外部占用与 pinned；返回外部占用的端口集合是否变化
func (pool *proxyPool) observeAccountProxyLocked(accountID string, state *proxyPoolAccount, raw string) bool {
	state.known = true
	state.proxyRaw = raw
	state.proxy = ""
	if strings.TrimSpace(raw) != "" {
		state.proxy = config.CanonicalProxy(raw)
	}
	return pool.updateAccountExternalLocked(state)
}

// updateAccountExternalLocked 按当前设置重算账户的外部占用端口与 pinned；返回外部占用的端口集合是否变化
func (pool *proxyPool) updateAccountExternalLocked(state *proxyPoolAccount) bool {
	previous := state.externalPort
	if state.pinned {
		pool.pinned--
	}
	state.pinned = state.proxy != "" && pool.settings.global != "" && state.proxy == pool.settings.global
	if state.pinned {
		pool.pinned++
	}
	state.externalPort = pool.externalPortLocked(state.proxy)
	if previous == state.externalPort {
		return false
	}
	changed := false
	if previous != 0 {
		if pool.external[previous]--; pool.external[previous] <= 0 {
			delete(pool.external, previous)
			changed = true
		}
	}
	if state.externalPort != 0 {
		if pool.external[state.externalPort]++; pool.external[state.externalPort] == 1 {
			changed = true
		}
	}
	return changed
}

// externalPortLocked 返回代理指向的段内端口（静态 IP 且同一协议与主机）；不是时为 0
func (pool *proxyPool) externalPortLocked(proxy string) int {
	if proxy == "" || pool.settings.cfg.Mode != config.ProxyPoolStatic {
		return 0
	}
	port, ok := pool.settings.cfg.HostMatches(proxy)
	if !ok || !pool.inRangeLocked(port) {
		return 0
	}
	return port
}

// recomputeAccountsLocked 设置或 PROXY 变化后重算全部账户的外部占用与 pinned，以及 PROXY 本身指向的端口
func (pool *proxyPool) recomputeAccountsLocked() {
	pool.external = make(map[int]int)
	pool.pinned = 0
	for _, state := range pool.accounts {
		state.externalPort, state.pinned = 0, false
		pool.updateAccountExternalLocked(state)
	}
	pool.globalExternal = 0
	if port := pool.externalPortLocked(pool.settings.global); port != 0 {
		pool.globalExternal = port
		pool.external[port]++
	}
}

// externalLogLocked 外部占用的端口集合变化且不为空时 WARN 一次
func (pool *proxyPool) externalLogLocked() []proxyPoolLog {
	ports := make([]int, 0, len(pool.external))
	for port := range pool.external {
		ports = append(ports, port)
	}
	sort.Ints(ports)
	accounts := 0
	for _, state := range pool.accounts {
		if state.externalPort != 0 {
			accounts++
		}
	}
	summary := fmt.Sprint(ports, accounts)
	if summary == pool.externalSummary {
		return nil
	}
	pool.externalSummary = summary
	if len(ports) == 0 {
		return nil
	}
	return []proxyPoolLog{{source: "service", level: "WARN", message: fmt.Sprintf(
		"IP 池端口段与账户固定代理或 PROXY 重叠，这些端口不再分配 | 端口数=%d | 账户数=%d", len(ports), accounts)}}
}

// forgetAccountLocked 丢掉已不在账户池里的账户记录（活动租约、亲和与会话记录不受影响）
func (pool *proxyPool) forgetAccountLocked(accountID string, state *proxyPoolAccount) {
	pool.setDefaultPortLocked(accountID, state, 0, false)
	if state.pinned {
		pool.pinned--
	}
	if state.externalPort != 0 {
		if pool.external[state.externalPort]--; pool.external[state.externalPort] <= 0 {
			delete(pool.external, state.externalPort)
		}
	}
	delete(pool.accounts, accountID)
	delete(pool.strikes, accountID)
	delete(pool.successes, accountID)
}

// resetPortRecordsLocked 清掉全部端口的冷却、归属与规则 A/B 记录与账户的裸限流、成功记录（不动 holders 与端口锁）
func (pool *proxyPool) resetPortRecordsLocked() {
	clear(pool.strikes)
	clear(pool.successes)
	for _, state := range pool.ports {
		state.lastAccount = ""
		state.failures, state.hits = nil, nil
		state.lastGenSuccess = time.Time{}
		state.upstreamFails = 0
		state.coolUntil, state.cooledAt = time.Time{}, time.Time{}
		state.coolLevel = 0
	}
	pool.lockedElsewhere = make(map[int]time.Time)
	pool.markDirtyLocked()
}

func (pool *proxyPool) accountLocked(accountID string) *proxyPoolAccount {
	state := pool.accounts[accountID]
	if state == nil {
		state = &proxyPoolAccount{}
		pool.accounts[accountID] = state
	}
	return state
}

func (pool *proxyPool) portLocked(port int) *proxyPoolPort {
	state := pool.ports[port]
	if state == nil {
		state = &proxyPoolPort{}
		pool.ports[port] = state
	}
	return state
}

// latestLeaseLocked 返回账户最新的活动租约
func (pool *proxyPool) latestLeaseLocked(accountID string) *proxyPoolLease {
	leases := pool.byAccount[accountID]
	if len(leases) == 0 {
		return nil
	}
	return leases[len(leases)-1]
}

// bindingLeaseLocked 返回账户绑定所用的活动租约：最新的已启动 Worker 租约（正在服务的浏览器，WAA proof 出自它的出口）；
// 没有时为最新的活动租约（启动中的 Worker、登录、验证）。先启后停时新浏览器 attach 之前 Go 侧仍走旧浏览器的出口，启动失败也不来回切换
func (pool *proxyPool) bindingLeaseLocked(accountID string) *proxyPoolLease {
	leases := pool.byAccount[accountID]
	for index := len(leases) - 1; index >= 0; index-- {
		if lease := leases[index]; lease.attached && lease.kind == proxyPoolKindWorker {
			return lease
		}
	}
	if len(leases) == 0 {
		return nil
	}
	return leases[len(leases)-1]
}

// allocatableLocked 判断端口可分配：在当前端口段内、没有持有者、不在冷却、不是外部占用、没有被其他进程锁住（或已到重试时间）
func (pool *proxyPool) allocatableLocked(port int, now time.Time) bool {
	if !pool.inRangeLocked(port) || pool.external[port] > 0 {
		return false
	}
	if until, ok := pool.lockedElsewhere[port]; ok && now.Before(until) {
		return false
	}
	state := pool.ports[port]
	return state == nil || len(state.holders) == 0 && !now.Before(state.coolUntil)
}

// ownsLocked 判断端口归账户：端口最后的独占者是它（端口已被别人用过就不再抢回）
func (pool *proxyPool) ownsLocked(accountID string, port int) bool {
	state := pool.ports[port]
	return state != nil && state.lastAccount == accountID
}

// inRangeLocked 判断端口在当前静态端口段内（不是静态 IP 时为否）
func (pool *proxyPool) inRangeLocked(port int) bool {
	return pool.settings.size > 0 && port >= pool.settings.portStart && port <= pool.settings.portEnd
}

// staticURLLocked 返回段内端口的上游 URL（调用方保证在段内）
func (pool *proxyPool) staticURLLocked(port int) string {
	return pool.settings.urls[port-pool.settings.portStart]
}

func (pool *proxyPool) coolingLocked(port int, now time.Time) bool {
	state := pool.ports[port]
	return state != nil && now.Before(state.coolUntil)
}

// currentStaticLocked 判断静态租约仍是当前设置下的出口：端口在段内且 URL 与当前设置算出的相同
func (pool *proxyPool) currentStaticLocked(lease *proxyPoolLease) bool {
	return lease.port != 0 && pool.inRangeLocked(lease.port) && pool.staticURLLocked(lease.port) == lease.url
}

// leaseCurrentLocked 判断池租约仍是当前设置下的出口（否则计入 stale）
func (pool *proxyPool) leaseCurrentLocked(lease *proxyPoolLease) bool {
	if lease.port != 0 {
		return pool.currentStaticLocked(lease)
	}
	return pool.settings.cfg.Mode == config.ProxyPoolDynamic && pool.sessionURLLocked(lease.slot) == lease.url
}

// portsInUseLocked 返回段内有浏览器占用的端口数
func (pool *proxyPool) portsInUseLocked() int {
	count := 0
	for port, state := range pool.ports {
		if len(state.holders) > 0 && pool.inRangeLocked(port) {
			count++
		}
	}
	return count
}

// sessionsInUseLocked 返回有浏览器在用的当前会话数
func (pool *proxyPool) sessionsInUseLocked() int {
	sessions := make(map[string]struct{})
	for _, leases := range pool.byKey {
		for _, lease := range leases {
			if lease.pooled && lease.port == 0 && pool.leaseCurrentLocked(lease) {
				sessions[lease.slot] = struct{}{}
			}
		}
	}
	return len(sessions)
}

// sessionInUseLocked 判断会话号是否已有浏览器在用
func (pool *proxyPool) sessionInUseLocked(session string) bool {
	for _, leases := range pool.byKey {
		for _, lease := range leases {
			if lease.pooled && lease.port == 0 && lease.slot == session {
				return true
			}
		}
	}
	return false
}

func (pool *proxyPool) coolingCountLocked(now time.Time) int {
	count := 0
	for port := range pool.ports {
		if pool.inRangeLocked(port) && pool.coolingLocked(port, now) {
			count++
		}
	}
	return count
}

// unpooledLocked 返回仍持有直通租约（走 PROXY）的 Worker 数
func (pool *proxyPool) unpooledLocked() int {
	count := 0
	for _, leases := range pool.byKey {
		for _, lease := range leases {
			if !lease.pooled && lease.kind == proxyPoolKindWorker {
				count++
			}
		}
	}
	return count
}

// portHolderAccountsLocked 返回持有端口的账户 ID（登录中的新账户不含）
func (pool *proxyPool) portHolderAccountsLocked(port int) []string {
	state := pool.ports[port]
	if state == nil {
		return nil
	}
	seen := make(map[string]struct{})
	var ids []string
	for key := range state.holders {
		for _, lease := range pool.byKey[key] {
			if lease.port == port && lease.accountID != "" {
				if _, ok := seen[lease.accountID]; !ok {
					seen[lease.accountID] = struct{}{}
					ids = append(ids, lease.accountID)
				}
			}
		}
	}
	sort.Strings(ids)
	return ids
}

// portWorkerAccountsLocked 返回 holders 里正用端口服务的账户（见 servesOnPortLocked）
func (pool *proxyPool) portWorkerAccountsLocked(holders []string, port int) []string {
	var ids []string
	for _, accountID := range holders {
		if pool.servesOnPortLocked(accountID, port) {
			ids = append(ids, accountID)
		}
	}
	return ids
}

// servesOnPort 判断账户现在正用这个端口服务：绑定所用的租约（见 bindingLeaseLocked）是这个端口上 IP 池分配、已启动的 Worker。
// 端口冷却后重启 Worker 之前再核对一次（冷却回调在后台执行，期间账户可能已换成别的端口上的新 Worker）。方法对 nil 安全
func (pool *proxyPool) servesOnPort(accountID string, port int) bool {
	if pool == nil || accountID == "" || port == 0 {
		return false
	}
	pool.mu.Lock()
	defer pool.mu.Unlock()
	return pool.servesOnPortLocked(accountID, port)
}

// servesOnPortLocked 为 servesOnPort 的实现；调用方持有 pool.mu
func (pool *proxyPool) servesOnPortLocked(accountID string, port int) bool {
	lease := pool.bindingLeaseLocked(accountID)
	return lease != nil && lease.attached && lease.kind == proxyPoolKindWorker && lease.pooled && lease.port == port
}

// sessionURLLocked 返回会话号 session 的上游 URL（与 config.ProxyPool.SessionURL 相同，不再重复规范化）；不是动态模式时为空
func (pool *proxyPool) sessionURLLocked(session string) string {
	if pool.settings.cfg.Mode != config.ProxyPoolDynamic || pool.settings.sessionTemplate == "" || session == "" {
		return ""
	}
	return strings.ReplaceAll(pool.settings.sessionTemplate, config.ProxyPoolSessionPlaceholder, session)
}

// sessionLocked 返回账户的会话号：基础会话号（登录时采用的临时会话号，否则按账户 ID 哈希）加代号 "r" + base36
func (pool *proxyPool) sessionLocked(accountID string) string {
	base := proxyPoolHashSession(accountID)
	generation := 0
	if session := pool.sessions[accountID]; session != nil {
		if session.base != "" {
			base = session.base
		}
		generation = session.generation
	}
	if generation > 0 {
		return base + "r" + strconv.FormatInt(int64(generation), 36)
	}
	return base
}

func (pool *proxyPool) sessionStateLocked(accountID string) *proxyPoolSession {
	session := pool.sessions[accountID]
	if session == nil {
		session = &proxyPoolSession{}
		pool.sessions[accountID] = session
	}
	return session
}

// emit 写出锁内收集的日志
func (pool *proxyPool) emit(logs []proxyPoolLog) {
	if pool == nil || pool.requests == nil {
		return
	}
	for _, entry := range logs {
		if entry.durable {
			pool.requests.logDurable(entry.source, entry.level, entry.message)
		} else {
			pool.requests.log(entry.source, entry.level, entry.message)
		}
	}
}

// logSource 返回租约日志的来源：账户标签，没有时为账户 ID，再没有时为 service
func (lease *proxyPoolLease) logSource() string {
	switch {
	case lease.label != "":
		return lease.label
	case lease.accountID != "":
		return lease.accountID
	default:
		return "service"
	}
}

// proxyPoolSteps 返回 [1, size) 里与 size 互质的步长（size 为 1 时为 [1]）：默认出口的探测序列按账户哈希从中取一个
func proxyPoolSteps(size int) []int {
	if size <= 1 {
		return []int{1}
	}
	steps := make([]int, 0, size)
	for step := 1; step < size; step++ {
		left, right := step, size
		for right != 0 {
			left, right = right, left%right
		}
		if left == 1 {
			steps = append(steps, step)
		}
	}
	return steps
}

// proxyPoolProbe 返回账户在 size 个端口上的探测起点与步长：起点、步长分别取账户 ID 的 FNV-1a 64 位哈希的低、高 32 位（步长从与 size
// 互质的 steps 里取），探测序列 起点 + k × 步长（mod size，k = 0…size-1）不重复地走遍全部端口。不同账户的步长不同，一个端口被占时
// 落在它上面的账户各自走到不同的下一个端口
func proxyPoolProbe(accountID string, size int, steps []int) (offset, step int) {
	if size <= 1 || len(steps) == 0 {
		return 0, 1
	}
	hasher := fnv.New64a()
	_, _ = hasher.Write([]byte(accountID))
	sum := hasher.Sum64()
	return int(uint32(sum) % uint32(size)), steps[uint32(sum>>32)%uint32(len(steps))]
}

// proxyPoolHashSession 返回按账户 ID 哈希的基础会话号（12 个小写十六进制字符）
func proxyPoolHashSession(accountID string) string {
	sum := sha256.Sum256([]byte("aistudio2api-proxy-pool:" + accountID))
	return hex.EncodeToString(sum[:])[:proxyPoolSessionHashLen]
}

// newProxyPoolSession 返回 prefix + length 位随机小写十六进制的临时会话号；随机源出错时退回时间戳
func newProxyPoolSession(random io.Reader, prefix string, length int) string {
	buffer := make([]byte, (length+1)/2)
	if _, err := io.ReadFull(random, buffer); err != nil {
		sum := sha256.Sum256([]byte(time.Now().String()))
		copy(buffer, sum[:])
	}
	return prefix + hex.EncodeToString(buffer)[:length]
}

// pruneProxyPoolFailures 去掉窗口之外的记录（原地）
func pruneProxyPoolFailures(failures []proxyPoolFailure, now time.Time) []proxyPoolFailure {
	return slices.DeleteFunc(failures, func(failure proxyPoolFailure) bool {
		return now.Sub(failure.at) >= proxyPoolFailureWindow
	})
}

func removeProxyPoolLease(leases []*proxyPoolLease, lease *proxyPoolLease) []*proxyPoolLease {
	return slices.DeleteFunc(leases, func(candidate *proxyPoolLease) bool { return candidate == lease })
}

func proxyBindingEqual(left, right *aistudio.ProxyBinding) bool {
	if left == nil || right == nil {
		return left == right
	}
	return *left == *right
}

// proxyPoolKindLabel 返回日志里的用途
func proxyPoolKindLabel(kind string) string {
	switch kind {
	case proxyPoolKindWorker:
		return "Worker"
	case proxyPoolKindVerify:
		return "验证"
	case proxyPoolKindImport:
		return "导入"
	default:
		return "登录"
	}
}

// proxyPoolStatusKind 返回状态接口的用途：worker，或 login（登录、验证、导入）
func proxyPoolStatusKind(kind string) string {
	if kind == proxyPoolKindWorker {
		return proxyPoolKindWorker
	}
	return proxyPoolKindLogin
}

// proxyPoolSlotLess 按端口号的数值、会话号的字典序排列
func proxyPoolSlotLess(left, right string) bool {
	leftPort, leftErr := strconv.Atoi(left)
	rightPort, rightErr := strconv.Atoi(right)
	if leftErr == nil && rightErr == nil {
		return leftPort < rightPort
	}
	return left < right
}
