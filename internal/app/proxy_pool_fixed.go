package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/api"
	"github.com/Mag1cFall/AIStudio2API/internal/config"
)

// IP 池固定分配（PROXY_POOL_FIXED，见 config.ProxyPool.Fixed）：
//   - 静态：每个走 IP 池的账户（登记过、没有自己的代理，不论启用与状态）在固定表里有一个端口，按账户数平均分到端口段的端口上；同一端口的
//     账户共用这个 IP、可以同时开浏览器（固定租约不独占、不写亲和与归属）。账户的 Worker、登录、验证、导入与 Go 侧出口都只用固定端口；
//     固定表只在删除账户、账户改用自己的代理、端口段变化（只挪段外的）与管理员确认的“重新平均分配”“换 IP”时改变，冷却、被其他进程占用、
//     热更新其他设置、重启、更新都不改变。端口冷却或被其他进程占用时这个端口上的账户暂停使用（绑定带 Hold，账户池按 AccountProxyPaused
//     排除调度），等端口恢复，不换端口、不走默认代理、不直连；
//   - 动态：不再自动换会话（连续出口连不通与出口判定都不换），每个账户一直用它的会话号；
//   - 关闭（默认）：以下全部分支以 fixedStaticLocked / fixedDynamicLocked 或“固定表、预留、暂停记录非空”守门，与原来逐字节一致；
//     固定表保留（休眠），只有删除账户会释放其中的位置。
//
// 固定表持久化在 runtime/proxy-pool-fixed.json（与 proxy-pool.json 分开：回退旧版再升级分配不变，旧版不碰它），只有账户 ID、端口号、时间
// 与来源代码。锁序 fixed.saveMu → pool.mu（与 proxy-pool.json 的写盘相同）；fixedFlush 的调用方不能持有 pool.mu 或账户池的锁

const (
	proxyPoolFixedFileName = "proxy-pool-fixed.json"
	proxyPoolFixedVersion  = 1
	// proxyPoolFixedSaveDelay 为单个新增、登录与导入、改用自己代理的释放合并写盘的延迟（首次迁移、端口段撤离、手动操作与删除立即写）
	proxyPoolFixedSaveDelay = time.Second
	// proxyPoolFixedReserveTTL 为 Chrome 导入 claim 的预留过期时间（兜底：导入中途失败而没有 discard 时不永久占位）
	proxyPoolFixedReserveTTL = time.Hour
	// proxyPoolFixedListLimit 为状态接口里暂停与停用端口列表的上限；proxyPoolFixedTargetsLimit 为“换 IP”结果里挪到的端口上限
	proxyPoolFixedListLimit    = 50
	proxyPoolFixedTargetsLimit = 20
	// proxyPoolFixedLogTargets 为“换 IP”日志里列出的端口数（超过时加“等”）
	proxyPoolFixedLogTargets = 10
	// proxyPoolFixedSessionWarnWindow 为动态固定会话“不换会话”WARN 同一账户的最短间隔
	proxyPoolFixedSessionWarnWindow = 30 * time.Minute
	// proxyPoolFixedAddingTTL 为“正在新增”记号的有效期（兜底：目录扫描注册失败、没有回滚时不永久挡住清理）
	proxyPoolFixedAddingTTL = 10 * time.Minute
)

// 固定表条目的来源代码（写进 proxy-pool-fixed.json）
const (
	fixedSourceAffinity  = "affinity"  // 首次迁移保留原端口
	fixedSourceBalance   = "balance"   // 首次迁移按最少分配
	fixedSourceNew       = "new"       // 新增账户（目录扫描、管理页添加、清空自己代理后）
	fixedSourceLogin     = "login"     // 新账户浏览器登录用的端口
	fixedSourceImport    = "import"    // Chrome 导入用的端口
	fixedSourceRange     = "range"     // 端口段变化后段外账户重新分配
	fixedSourceRebalance = "rebalance" // 管理员“重新平均分配”
	fixedSourceReassign  = "reassign"  // 管理员“给端口换 IP”
)

// 固定端口暂停的原因代码（状态接口 paused[].reason）
const (
	fixedPauseCoolingUpstream    = "cooling_upstream"
	fixedPauseCoolingRateLimited = "cooling_rate_limited"
	fixedPauseCooling            = "cooling"
	fixedPauseLockedElsewhere    = "locked_elsewhere"
	// fixedPauseNoPort 为新账户登录、导入时全部端口暂停使用
	fixedPauseNoPort = "no_port"
)

// errProxyPoolPortPaused 为固定端口暂停使用时 Worker 不启动、登录与验证不进行的错误（errors.Is 识别，见 proxyPoolPausedError）
var errProxyPoolPortPaused = errors.New("IP 池固定端口暂停使用")

// proxyPoolPausedError 为固定分配下被拒的申请：Port 为账户的固定端口（新账户全部端口暂停时为 0），Reason 为暂停原因代码，Until 为冷却
// 恢复时间或下次检查时间。文案只含端口号与固定中文
type proxyPoolPausedError struct {
	Port   int
	Reason string
	Until  time.Time
}

func (err *proxyPoolPausedError) Error() string {
	if err.Port == 0 {
		return "IP 池固定分配没有可用的端口（全部暂停使用）"
	}
	return fmt.Sprintf("IP 池固定端口 %d 暂停使用：%s；固定分配不换端口", err.Port, fixedPauseDetail(err.Reason, err.Until))
}

// Is 让 errors.Is(err, errProxyPoolPortPaused) 识别
func (err *proxyPoolPausedError) Is(target error) bool {
	return target == errProxyPoolPortPaused
}

// proxyPoolFixed 为固定分配表与暂停记录（proxyPool 的值字段，受 pool.mu 保护，saveMu 除外）
type proxyPoolFixed struct {
	path       string                               // runtime/proxy-pool-fixed.json；为空时只在内存
	since      time.Time                            // 首次迁移完成的时刻；零为还没迁移过
	start, end int                                  // 最近一次对齐时的端口段（判断端口段变化）
	entries    map[string]proxyPoolFixedEntry       // 账户 ID → 分配
	byPort     map[int]map[string]struct{}          // 端口 → 账户（反查）
	excluded   map[int]time.Time                    // “换 IP”后停用的端口 → 停用时刻
	reserved   map[string]proxyPoolFixedReservation // 预留令牌 → 端口与过期（只在内存）
	reservedBy map[int]int                          // 端口 → 预留数（只在内存）
	tokenSeq   uint64
	// adding 为正在新增、还没进账户池的账户 → 记下的时刻（只在内存）：新账户登录或导入 adopt 与 prime 时记下，账户池同步看到它、删除或
	// 新增回滚时去掉。“重新平均分配”不把它当成已不在账户池的记录清理（addAccount 在 adopt 之后、AccountPool.Add 之前还要写指纹、
	// 公共头与 Worker 配置，目录扫描在 prime 之后才 Add）
	adding map[string]time.Time
	// held 为已推送过暂停的端口（只在内存，检测暂停与恢复的转换）；nextCheck 为其中最早的恢复或检查时刻；rescan 为下一次 wake 时全量扫描
	// 段内有固定账户的端口（启动读回的冷却、开关切换、换代理商之后、账户被分到正在暂停的端口上）
	held      map[int]proxyPoolFixedHold
	nextCheck time.Time
	rescan    bool
	// logs 为锁内产生、待取走的日志：bindingLocked 在账户池的回调里不能写日志，每个公共方法解锁前用 takeFixedLogsLocked 取走
	logs           []proxyPoolLog
	notify         bool // 有账户页可见的变化（端口暂停与恢复、开启与关闭、对齐改了分配表）待推送账户列表，takeFixedLogsLocked 取走时换成回调
	dirty          bool
	saveTimer      *time.Timer
	saveMu         sync.Mutex
	externalWarned string               // 固定分配下外部占用 WARN 的去重键
	sessionWarned  map[string]time.Time // 动态固定会话“不换会话”WARN 的账户 → 上次时间
	lazyAssigned   int                  // 惰性分配的次数（测试：首次开启与重启路径里应为 0）
}

// proxyPoolFixedEntry 为一个账户的固定端口：at 为分配时间，source 为来源代码
type proxyPoolFixedEntry struct {
	port   int
	at     time.Time
	source string
}

// proxyPoolFixedReservation 为登录或导入中的新账户预留的位置（还没有账户 ID）：expires 为零时随租约存活
type proxyPoolFixedReservation struct {
	port    int
	expires time.Time
}

// proxyPoolFixedHold 为已推送的暂停：since 为这一段暂停开始的时刻
type proxyPoolFixedHold struct {
	reason string
	until  time.Time
	since  time.Time
}

// fixedSummary 为一次对齐的结果（启动与开启时的汇总日志）
type fixedSummary struct {
	initial                             bool
	kept, assigned, released, evacuated int
}

// init 初始化固定表（newProxyPool 里调用）
func (fixed *proxyPoolFixed) init(poolPath string) {
	if poolPath != "" {
		fixed.path = filepath.Join(filepath.Dir(poolPath), proxyPoolFixedFileName)
	}
	fixed.entries = make(map[string]proxyPoolFixedEntry)
	fixed.byPort = make(map[int]map[string]struct{})
	fixed.excluded = make(map[int]time.Time)
	fixed.reserved = make(map[string]proxyPoolFixedReservation)
	fixed.reservedBy = make(map[int]int)
	fixed.adding = make(map[string]time.Time)
	fixed.held = make(map[int]proxyPoolFixedHold)
	fixed.sessionWarned = make(map[string]time.Time)
}

// fixedStaticLocked 判断静态固定分配生效：开关开启、静态模式且有可分配的端口段。调用方持有 pool.mu
func (pool *proxyPool) fixedStaticLocked() bool {
	settings := pool.settings
	return settings.cfg.Fixed && settings.cfg.Mode == config.ProxyPoolStatic && settings.size > 0
}

// fixedDynamicLocked 判断动态固定会话生效：开关开启、动态模式且有地址模板。调用方持有 pool.mu
func (pool *proxyPool) fixedDynamicLocked() bool {
	settings := pool.settings
	return settings.cfg.Fixed && settings.cfg.Mode == config.ProxyPoolDynamic && settings.sessionTemplate != ""
}

// fixedKnownLocked 判断账户已在 IP 池登记（prime 或账户池同步时记下；删除与新增回滚后取消，见 releaseFixed）：只有登记过的账户才惰性分配
// 固定端口，删除之后才归还的租约推送它时不会把它写回固定表
func (pool *proxyPool) fixedKnownLocked(accountID string) bool {
	state := pool.accounts[accountID]
	return state != nil && state.known
}

// ownProxyLocked 判断账户已登记且有自己的代理（不参与固定分配）
func (pool *proxyPool) ownProxyLocked(accountID string) bool {
	if accountID == "" {
		return false
	}
	state := pool.accounts[accountID]
	return state != nil && state.known && state.proxy != ""
}

// takeFixedLogsLocked 取走锁内产生的固定分配日志；有待推送的账户页变化时在末尾加上推送回调（emit 在锁外调用它）。每个公共方法解锁前调用
func (pool *proxyPool) takeFixedLogsLocked() []proxyPoolLog {
	logs := pool.fixed.logs
	pool.fixed.logs = nil
	if pool.fixed.notify {
		pool.fixed.notify = false
		if hook := pool.onAccountsChanged; hook != nil {
			logs = append(logs, proxyPoolLog{notify: hook})
		}
	}
	return logs
}

// queueFixedLogLocked 记下一行固定分配日志（入口解锁前取走）
func (pool *proxyPool) queueFixedLogLocked(source, level string, durable bool, message string) {
	pool.fixed.logs = append(pool.fixed.logs, proxyPoolLog{source: source, level: level, durable: durable, message: message})
}

// ---- 固定表 ----

// fixedSetLocked 把账户分到端口（同端口时不变）；分到正在暂停的端口时安排一次全量扫描（登记暂停、恢复时重推）
func (pool *proxyPool) fixedSetLocked(accountID string, port int, now time.Time, source string) {
	fixed := &pool.fixed
	if previous, ok := fixed.entries[accountID]; ok {
		if previous.port == port {
			return
		}
		fixed.unindex(accountID, previous.port)
	}
	fixed.entries[accountID] = proxyPoolFixedEntry{port: port, at: now, source: source}
	users := fixed.byPort[port]
	if users == nil {
		users = make(map[string]struct{})
		fixed.byPort[port] = users
	}
	users[accountID] = struct{}{}
	pool.fixedMarkDirtyLocked()
	if pool.fixedStaticLocked() {
		if reason, _ := pool.fixedPauseLocked(port, now); reason != "" {
			fixed.rescan = true
		}
	}
}

// fixedDropLocked 释放账户的位置，返回原端口
func (pool *proxyPool) fixedDropLocked(accountID string) (int, bool) {
	fixed := &pool.fixed
	entry, ok := fixed.entries[accountID]
	if !ok {
		return 0, false
	}
	delete(fixed.entries, accountID)
	fixed.unindex(accountID, entry.port)
	pool.fixedMarkDirtyLocked()
	return entry.port, true
}

// unindex 从端口反查表里去掉账户
func (fixed *proxyPoolFixed) unindex(accountID string, port int) {
	if users := fixed.byPort[port]; users != nil {
		delete(users, accountID)
		if len(users) == 0 {
			delete(fixed.byPort, port)
		}
	}
}

// fixedCountLocked 返回端口上的固定账户数
func (pool *proxyPool) fixedCountLocked(port int) int {
	return len(pool.fixed.byPort[port])
}

// fixedLoadLocked 返回端口的负载：固定账户数加预留数
func (pool *proxyPool) fixedLoadLocked(port int) int {
	return len(pool.fixed.byPort[port]) + pool.fixed.reservedBy[port]
}

// fixedUsersLocked 返回端口上的固定账户（排序）
func (pool *proxyPool) fixedUsersLocked(port int) []string {
	users := pool.fixed.byPort[port]
	ids := make([]string, 0, len(users))
	for accountID := range users {
		ids = append(ids, accountID)
	}
	sort.Strings(ids)
	return ids
}

// fixedPortsLocked 返回参与分配的端口 U（升序）：当前端口段内、没有停用的（含外部占用的端口）；段内全部停用时退为段内全部端口
func (pool *proxyPool) fixedPortsLocked() []int {
	size := pool.settings.size
	if size == 0 {
		return nil
	}
	ports := make([]int, 0, size)
	for port := pool.settings.portStart; port <= pool.settings.portEnd; port++ {
		if _, excluded := pool.fixed.excluded[port]; !excluded {
			ports = append(ports, port)
		}
	}
	if len(ports) == 0 {
		for port := pool.settings.portStart; port <= pool.settings.portEnd; port++ {
			ports = append(ports, port)
		}
	}
	return ports
}

// fixedExcludedLocked 判断端口已“换 IP”停用（之后不再分配）
func (pool *proxyPool) fixedExcludedLocked(port int) bool {
	_, excluded := pool.fixed.excluded[port]
	return excluded
}

// fixedSpreadLocked 返回参与分配的端口上每端口账户数的最少与最多
func (pool *proxyPool) fixedSpreadLocked(ports []int) (low, high int) {
	for index, port := range ports {
		count := pool.fixedCountLocked(port)
		if index == 0 || count < low {
			low = count
		}
		if count > high {
			high = count
		}
	}
	return low, high
}

// fixedPausedFuncLocked 返回“端口此刻暂停使用”的判断（选端口时避开）
func (pool *proxyPool) fixedPausedFuncLocked(now time.Time) func(int) bool {
	return func(port int) bool {
		reason, _ := pool.fixedPauseLocked(port, now)
		return reason != ""
	}
}

// fixedPortLocked 返回账户的固定端口：有段内条目时为它；没有时 assign 为真则分到负载最少、不暂停的端口（惰性分配：新增账户、清空了自己代理的
// 账户，写 F6；段外条目按端口段变化重新分配），assign 为假时只返回此刻会分到的端口（预览，不改表）
func (pool *proxyPool) fixedPortLocked(accountID string, now time.Time, assign bool) int {
	fixed := &pool.fixed
	entry, had := fixed.entries[accountID]
	if had && pool.inRangeLocked(entry.port) {
		return entry.port
	}
	if assign {
		pool.fixedSweepReservedLocked(now)
	}
	port := fixedPickLeast(pool.fixedPortsLocked(), pool.fixedLoadLocked, pool.fixedPausedFuncLocked(now))
	if !assign || port == 0 || accountID == "" {
		return port
	}
	source := fixedSourceNew
	if had {
		source = fixedSourceRange
	}
	pool.fixedSetLocked(accountID, port, now, source)
	fixed.lazyAssigned++
	if had {
		pool.queueFixedLogLocked(accountID, "INFO", false, fmt.Sprintf("IP 池固定分配换端口 | 端口=%d → %d | 原因=端口段变化", entry.port, port))
	} else {
		pool.queueFixedLogLocked(accountID, "INFO", false, fmt.Sprintf("IP 池固定分配 | 端口=%d | 来源=新增 | 该端口账户=%d",
			port, pool.fixedCountLocked(port)))
	}
	return port
}

// ---- 预留 ----

// fixedReserveLocked 为还没有账户 ID 的登录或导入预留端口上的一个位置，返回令牌
func (pool *proxyPool) fixedReserveLocked(port int, expires time.Time) string {
	fixed := &pool.fixed
	fixed.tokenSeq++
	token := "r" + strconv.FormatUint(fixed.tokenSeq, 36)
	fixed.reserved[token] = proxyPoolFixedReservation{port: port, expires: expires}
	fixed.reservedBy[port]++
	return token
}

// fixedRevokeLocked 撤销预留（令牌为空或已撤销时什么也不做）
func (pool *proxyPool) fixedRevokeLocked(token string) {
	fixed := &pool.fixed
	reservation, ok := fixed.reserved[token]
	if !ok {
		return
	}
	delete(fixed.reserved, token)
	if fixed.reservedBy[reservation.port]--; fixed.reservedBy[reservation.port] <= 0 {
		delete(fixed.reservedBy, reservation.port)
	}
}

// fixedSweepReservedLocked 撤销已过期的预留（导入 claim 没有 adopt 也没有 discard 时的兜底）
func (pool *proxyPool) fixedSweepReservedLocked(now time.Time) {
	for token, reservation := range pool.fixed.reserved {
		if !reservation.expires.IsZero() && !now.Before(reservation.expires) {
			pool.fixedRevokeLocked(token)
		}
	}
}

// ---- 暂停 ----

// fixedPauseLocked 返回端口此刻暂停使用的原因与恢复（或下次检查）时间：冷却中（按冷却规则），或有“被其他进程占用”的记录（探测确认释放后才
// 解除，与默认出口同一口径）。外部占用、规则 A/B 计数未到、整体故障保护拦下的冷却、锁出错退避都不暂停。纯计算，不登记
func (pool *proxyPool) fixedPauseLocked(port int, now time.Time) (string, time.Time) {
	if state := pool.ports[port]; state != nil && now.Before(state.coolUntil) {
		switch state.coolRule {
		case proxyPoolRuleUpstream:
			return fixedPauseCoolingUpstream, state.coolUntil
		case proxyPoolRuleRateLimited:
			return fixedPauseCoolingRateLimited, state.coolUntil
		default:
			return fixedPauseCooling, state.coolUntil
		}
	}
	if until, locked := pool.lockedElsewhere[port]; locked {
		return fixedPauseLockedElsewhere, until
	}
	return "", time.Time{}
}

// fixedPauseLabel 返回暂停原因的中文（日志的“原因=”）
func fixedPauseLabel(reason string) string {
	switch reason {
	case fixedPauseCoolingUpstream:
		return "冷却中（连续出口连不通）"
	case fixedPauseCoolingRateLimited:
		return "冷却中（不同账号返回限流）"
	case fixedPauseCooling:
		return "冷却中"
	case fixedPauseLockedElsewhere:
		return "被其他进程占用"
	default:
		return "全部端口暂停使用"
	}
}

// fixedPauseDetail 返回暂停原因与恢复时间（错误文案用）
func fixedPauseDetail(reason string, until time.Time) string {
	if reason == fixedPauseLockedElsewhere {
		return "被其他进程占用，释放后恢复"
	}
	return fixedPauseLabel(reason) + "，恢复=" + until.UTC().Format(time.RFC3339)
}

// fixedHoldTextLocked 返回端口暂停时写进账户绑定的说明（空为不暂停）：冷却写恢复时间；被其他进程占用不写时间（重试时刻每分钟变，
// 写进去会每分钟重推绑定）
func (pool *proxyPool) fixedHoldTextLocked(port int, now time.Time) string {
	reason, until := pool.fixedPauseLocked(port, now)
	switch reason {
	case "":
		return ""
	case fixedPauseLockedElsewhere:
		return fmt.Sprintf("IP 池固定端口 %d 被其他进程占用，释放后恢复；固定分配不换端口", port)
	case fixedPauseCoolingUpstream:
		return fmt.Sprintf("IP 池固定端口 %d 冷却中（连续出口连不通），恢复=%s；固定分配不换端口", port, until.UTC().Format(time.RFC3339))
	case fixedPauseCoolingRateLimited:
		return fmt.Sprintf("IP 池固定端口 %d 冷却中（不同账号返回限流），恢复=%s；固定分配不换端口", port, until.UTC().Format(time.RFC3339))
	default:
		return fmt.Sprintf("IP 池固定端口 %d 冷却中，恢复=%s；固定分配不换端口", port, until.UTC().Format(time.RFC3339))
	}
}

// fixedPortChangedLocked 重算端口的暂停：与已推送的记录（held）不同时重推这个端口上的全部固定账户（暂停写 F9、恢复写 F10），更新 held 与
// nextCheck。只在入口函数里调用（持有 pool.mu、不在账户池的 UpdateProxyBindings 回调里）
func (pool *proxyPool) fixedPortChangedLocked(port int, now time.Time) {
	fixed := &pool.fixed
	reason, until := pool.fixedPauseLocked(port, now)
	previous, had := fixed.held[port]
	count := pool.fixedCountLocked(port)
	switch {
	case reason == "" || count == 0 || !pool.inRangeLocked(port):
		if !had {
			if reason == "" {
				// 没有登记过暂停、已推送的绑定却带着暂停说明（绑定先于登记推送：账户刚被分到正在暂停、还没登记的端口上，同一次 wake
				// 里检测就确认了释放）：重推这些账户，暂停说明不会一直留着
				pool.fixedRepushStaleHoldsLocked(port)
			}
			return
		}
		delete(fixed.held, port)
		pool.pushLocked(pool.fixedUsersLocked(port)...)
		fixed.notify = true
		if count > 0 {
			pool.queueFixedLogLocked("service", "INFO", true, fmt.Sprintf("IP 池固定端口恢复使用 | 端口=%d | 账户=%d | 暂停=%s",
				port, count, now.Sub(previous.since).Round(time.Second)))
		}
	case had && previous.reason == reason && (reason == fixedPauseLockedElsewhere || previous.until.Equal(until)):
		if !previous.until.Equal(until) {
			previous.until = until
			fixed.held[port] = previous
		}
	default:
		since := now
		if had {
			since = previous.since
		}
		fixed.held[port] = proxyPoolFixedHold{reason: reason, until: until, since: since}
		pool.pushLocked(pool.fixedUsersLocked(port)...)
		fixed.notify = true
		message := fmt.Sprintf("IP 池固定端口暂停使用 | 端口=%d | 原因=%s | 恢复=%s | 账户=%d | 固定分配不换端口",
			port, fixedPauseLabel(reason), until.UTC().Format(time.RFC3339), count)
		if reason == fixedPauseLockedElsewhere {
			message = fmt.Sprintf("IP 池固定端口暂停使用 | 端口=%d | 原因=被其他进程占用（多个进程不要共用同一端口段） | 下次检查=%s | 账户=%d | 固定分配不换端口",
				port, until.UTC().Format(time.RFC3339), count)
		}
		pool.queueFixedLogLocked("service", "WARN", true, message)
	}
	fixed.nextCheck = time.Time{}
	for _, hold := range fixed.held {
		if fixed.nextCheck.IsZero() || hold.until.Before(fixed.nextCheck) {
			fixed.nextCheck = hold.until
		}
	}
}

// fixedRepushStaleHoldsLocked 重推端口上已推送的绑定仍带暂停说明的固定账户（端口此刻不暂停、也没有暂停记录时调用）
func (pool *proxyPool) fixedRepushStaleHoldsLocked(port int) {
	var stale []string
	for _, accountID := range pool.fixedUsersLocked(port) {
		if state := pool.accounts[accountID]; state != nil && state.binding != nil && state.binding.Hold != "" {
			stale = append(stale, accountID)
		}
	}
	if len(stale) > 0 {
		pool.pushLocked(stale...)
		pool.fixed.notify = true
	}
}

// fixedRefreshHoldsLocked 在 wake 时检查暂停的转换：到了 nextCheck 时重算已推送暂停的端口（冷却到期、重试时间到）；rescan 时全量扫描段内有
// 固定账户的端口（登记启动时读回的冷却等）。静态固定之外什么也不做
func (pool *proxyPool) fixedRefreshHoldsLocked(now time.Time) {
	fixed := &pool.fixed
	if !pool.fixedStaticLocked() {
		return
	}
	due := !fixed.nextCheck.IsZero() && !now.Before(fixed.nextCheck)
	if !fixed.rescan && !due {
		return
	}
	full := fixed.rescan
	fixed.rescan = false
	ports := make([]int, 0, len(fixed.held))
	for port := range fixed.held {
		ports = append(ports, port)
	}
	if full {
		for port := range fixed.byPort {
			if _, ok := fixed.held[port]; ok || !pool.inRangeLocked(port) {
				continue
			}
			if reason, _ := pool.fixedPauseLocked(port, now); reason != "" {
				ports = append(ports, port)
			}
		}
	}
	sort.Ints(ports)
	for _, port := range ports {
		pool.fixedPortChangedLocked(port, now)
	}
}

// fixedPausedError 返回账户的固定端口此刻暂停使用的错误（Worker 启动前的预检，不占启动名额）：不是静态固定、账户有自己的代理或还没有
// 段内条目时为 nil。只读，不分配、不写日志。方法对 nil 安全
func (pool *proxyPool) fixedPausedError(accountID string) error {
	if pool == nil || accountID == "" {
		return nil
	}
	pool.mu.Lock()
	defer pool.mu.Unlock()
	if !pool.fixedStaticLocked() || pool.ownProxyLocked(accountID) {
		return nil
	}
	entry, ok := pool.fixed.entries[accountID]
	if !ok || !pool.inRangeLocked(entry.port) {
		return nil
	}
	if reason, until := pool.fixedPauseLocked(entry.port, pool.now()); reason != "" {
		return &proxyPoolPausedError{Port: entry.port, Reason: reason, Until: until}
	}
	return nil
}

// Refused 返回固定分配下被拒的原因（端口暂停使用、新账户没有可用端口）；其他情况为 nil。方法对 nil 安全
func (lease *proxyPoolLease) Refused() error {
	if lease == nil || lease.refused == nil {
		return nil
	}
	return lease.refused
}

// ---- 对齐 ----

// fixedReconcileLocked 让固定表与账户集合 present（ID → 有自己的代理）对齐：还没迁移过时首次迁移（保留没超过份额的亲和端口，其余按最少）；
// 之后端口段变化时只挪段外的条目（含不在账户池的），改用自己代理的账户释放，没有条目的新账户分到最少的端口；不在 present 里的条目不动
// （载入失败、目录暂时移走不换 IP）。幂等：没有可做的事时不改表、不写日志。返回是否改了表（调用方锁外立即写盘）
func (pool *proxyPool) fixedReconcileLocked(present map[string]bool, now time.Time) (bool, fixedSummary) {
	fixed := &pool.fixed
	ports := pool.fixedPortsLocked()
	start, end := pool.settings.portStart, pool.settings.portEnd
	var summary fixedSummary
	ids := make([]string, 0, len(present))
	for accountID := range present {
		ids = append(ids, accountID)
	}
	sort.Strings(ids)
	if fixed.since.IsZero() && len(fixed.entries) == 0 {
		var participants []string
		prior := make(map[string]fixedPrior)
		for _, accountID := range ids {
			if present[accountID] {
				continue
			}
			participants = append(participants, accountID)
			if affinity, ok := pool.affinity[accountID]; ok && pool.inRangeLocked(affinity.port) {
				if _, excluded := fixed.excluded[affinity.port]; !excluded {
					prior[accountID] = fixedPrior{Port: affinity.port, Owner: pool.ownsLocked(accountID, affinity.port), At: affinity.at}
				}
			}
		}
		assign, kept := fixedPlanInitial(participants, ports, prior)
		for _, accountID := range participants {
			source := fixedSourceBalance
			if previous, ok := prior[accountID]; ok && previous.Port == assign[accountID] {
				source = fixedSourceAffinity
			}
			pool.fixedSetLocked(accountID, assign[accountID], now, source)
		}
		fixed.since, fixed.start, fixed.end = now, start, end
		pool.fixedMarkDirtyLocked()
		summary = fixedSummary{initial: true, kept: kept, assigned: len(assign) - kept}
		return true, summary
	}
	changed := false
	if fixed.since.IsZero() {
		fixed.since = now
		changed = true
	}
	for port := range fixed.excluded {
		if !pool.inRangeLocked(port) {
			delete(fixed.excluded, port)
			changed = true
		}
	}
	var movers []string
	for accountID, entry := range fixed.entries {
		if !pool.inRangeLocked(entry.port) {
			movers = append(movers, accountID)
		}
	}
	if len(movers) > 0 {
		load := make(map[int]int, len(ports))
		for _, port := range ports {
			// 只按固定账户数，不看暂停与进行中的登录、导入预留：结果只取决于持久的输入（端口段、停用端口与固定表）
			load[port] = pool.fixedCountLocked(port)
		}
		moves := fixedPlanEvacuate(movers, ports, load, nil)
		for _, move := range moves {
			from := fixed.entries[move.Account].port
			pool.fixedSetLocked(move.Account, move.To, now, fixedSourceRange)
			pool.queueFixedLogLocked(move.Account, "INFO", false, fmt.Sprintf("IP 池固定分配换端口 | 端口=%d → %d | 原因=端口段变化", from, move.To))
		}
		summary.evacuated = len(moves)
		changed = true
		if fixed.start != start || fixed.end != end {
			low, high := pool.fixedSpreadLocked(ports)
			pool.queueFixedLogLocked("service", "WARN", true, fmt.Sprintf(
				"IP 池端口段变化，固定分配只挪段外的账户 | 端口=%d-%d（参与 %d 个） | 挪动=%d | 每端口=%d-%d | 其他账户不变；需要均匀时在 IP 池设置里重新平均分配",
				start, end, len(ports), len(moves), low, high))
		}
	}
	if fixed.start != start || fixed.end != end {
		fixed.start, fixed.end = start, end
		changed = true
	}
	avoid := pool.fixedPausedFuncLocked(now)
	for _, accountID := range ids {
		_, has := fixed.entries[accountID]
		switch {
		case present[accountID] && has:
			port, _ := pool.fixedDropLocked(accountID)
			summary.released++
			pool.queueFixedLogLocked(accountID, "INFO", false, fmt.Sprintf("IP 池固定分配已释放 | 端口=%d | 原因=账户改用自己的代理 | 该端口账户=%d",
				port, pool.fixedCountLocked(port)))
		case !present[accountID] && !has:
			port := fixedPickLeast(ports, pool.fixedLoadLocked, avoid)
			pool.fixedSetLocked(accountID, port, now, fixedSourceNew)
			summary.assigned++
		}
	}
	if summary.released > 0 || summary.assigned > 0 {
		changed = true
	}
	if changed {
		pool.fixedMarkDirtyLocked()
	}
	return changed, summary
}

// fixedEnabledLogLocked 为开启（首次迁移 F1，或沿用分配表 F2）时的汇总行
func (pool *proxyPool) fixedEnabledLogLocked(summary fixedSummary) proxyPoolLog {
	ports := pool.fixedPortsLocked()
	low, high := pool.fixedSpreadLocked(ports)
	start, end := pool.settings.portStart, pool.settings.portEnd
	transitional := pool.fixedTransitionalLocked()
	message := fmt.Sprintf("IP 池固定分配已开启（沿用分配表） | 端口=%d-%d（参与 %d 个） | 账户=%d | 新分配=%d | 释放=%d | 段外重分=%d | 每端口=%d-%d | 运行中不在固定端口的 Worker=%d",
		start, end, len(ports), len(pool.fixed.entries), summary.assigned, summary.released, summary.evacuated, low, high, transitional)
	if summary.initial {
		message = fmt.Sprintf("IP 池固定分配已开启 | 端口=%d-%d（参与 %d 个） | 账户=%d | 每端口=%d-%d | 保留原端口=%d | 新分配=%d | 运行中不在固定端口的 Worker=%d（关闭后改用固定端口；立即切换：停止服务后再启动）",
			start, end, len(ports), len(pool.fixed.entries), low, high, summary.kept, summary.assigned, transitional)
	}
	return proxyPoolLog{source: "service", level: "INFO", durable: true, message: message}
}

// prepareFixed 在账户进入账户池、第一次 prime 之前按载入的账户对齐固定表（newRuntime 在 configure 之后、逐个 prime 之前调用）：
// 每次静态固定启动都写一行 F1（首次迁移）或 F2（沿用分配表）；改了表时锁外立即写盘。不是静态固定时什么也不做。方法对 nil 安全
func (pool *proxyPool) prepareFixed(accounts []*aistudio.Account) {
	if pool == nil {
		return
	}
	pool.mu.Lock()
	if !pool.fixedStaticLocked() {
		pool.mu.Unlock()
		return
	}
	present := make(map[string]bool, len(accounts))
	for _, account := range accounts {
		if account != nil && account.ID != "" {
			present[account.ID] = strings.TrimSpace(account.Config.Proxy) != ""
		}
	}
	changed, summary := pool.fixedReconcileLocked(present, pool.now())
	pool.fixed.rescan = true
	logs := []proxyPoolLog{pool.fixedEnabledLogLocked(summary)}
	logs = append(logs, pool.takeFixedLogsLocked()...)
	pool.mu.Unlock()
	pool.emit(logs)
	if changed {
		pool.fixedFlush()
	}
}

// fixedPresentLocked 返回当前账户池里登记过的账户（ID → 有自己的代理），供热更新时对齐。调用方持有 pool.mu（锁序 pool.mu → 账户池锁）
func (pool *proxyPool) fixedPresentLocked() map[string]bool {
	ids := pool.owner.AccountIDs()
	present := make(map[string]bool, len(ids))
	for _, accountID := range ids {
		if state := pool.accounts[accountID]; state != nil && state.known {
			present[accountID] = state.proxy != ""
		}
	}
	return present
}

// fixedConfigureLocked 为 configure 里配置变化时的固定分配处理（在全量重推之前）：静态固定且有账户池时对齐固定表；开关与模式的转换写
// F1/F2（开启）、F3（关闭，清掉暂停记录）、F4/F5（动态固定会话）。返回要写的日志与是否改了表
func (pool *proxyPool) fixedConfigureLocked(old proxyPoolSettings, now time.Time) ([]proxyPoolLog, bool) {
	fixed := &pool.fixed
	wasStatic := old.initialized && old.cfg.Fixed && old.cfg.Mode == config.ProxyPoolStatic && old.size > 0
	wasDynamic := old.initialized && old.cfg.Fixed && old.cfg.Mode == config.ProxyPoolDynamic && old.sessionTemplate != ""
	nowStatic, nowDynamic := pool.fixedStaticLocked(), pool.fixedDynamicLocked()
	var logs []proxyPoolLog
	changed := false
	if nowStatic {
		fixed.rescan = true
		if pool.owner != nil {
			var summary fixedSummary
			changed, summary = pool.fixedReconcileLocked(pool.fixedPresentLocked(), now)
			if !wasStatic {
				logs = append(logs, pool.fixedEnabledLogLocked(summary))
			}
		}
	}
	if nowStatic != wasStatic || changed {
		// 开启、关闭与对齐改了分配表：账户页的固定端口与“IP 暂停”随全量重推变化，推送账户列表
		fixed.notify = true
	}
	if wasStatic && !nowStatic {
		clear(fixed.held)
		fixed.nextCheck, fixed.rescan = time.Time{}, false
		logs = append(logs, proxyPoolLog{source: "service", level: "INFO", durable: true, message: fmt.Sprintf(
			"IP 池固定分配已关闭，分配表保留（再次开启时沿用） | 账户=%d | 运行中的浏览器关闭后按原方式分配端口", len(fixed.entries))})
	}
	switch {
	case !old.initialized:
		// 启动时的第一次生效：“IP 池已更新”那一行已带“固定会话=开启”
	case nowDynamic && !wasDynamic:
		accounts := 0
		for _, state := range pool.accounts {
			if state.known && state.proxy == "" {
				accounts++
			}
		}
		logs = append(logs, proxyPoolLog{source: "service", level: "INFO", durable: true, message: fmt.Sprintf(
			"IP 池固定会话已开启：不再自动换会话 | 账户=%d", accounts)})
	case wasDynamic && !nowDynamic:
		logs = append(logs, proxyPoolLog{source: "service", level: "INFO", durable: true, message: "IP 池固定会话已关闭，连续出口连不通时恢复自动换会话"})
	}
	return logs, changed
}

// fixedTransitionalLocked 返回运行中、不在固定端口上的 Worker 数（开启或手动挪动之前就在别的出口上运行、还没关闭的，直通的也算）
func (pool *proxyPool) fixedTransitionalLocked() int {
	count := 0
	for accountID := range pool.byAccount {
		entry, ok := pool.fixed.entries[accountID]
		if !ok {
			continue
		}
		lease := pool.attachedWorkerLeaseLocked(accountID)
		if lease == nil {
			continue
		}
		if !lease.pooled || lease.port != entry.port || !pool.currentStaticLocked(lease) {
			count++
		}
	}
	return count
}

// attachedWorkerLeaseLocked 返回账户最新的已启动 Worker 租约（没有时为 nil）
func (pool *proxyPool) attachedWorkerLeaseLocked(accountID string) *proxyPoolLease {
	leases := pool.byAccount[accountID]
	for index := len(leases) - 1; index >= 0; index-- {
		if lease := leases[index]; lease.attached && lease.kind == proxyPoolKindWorker {
			return lease
		}
	}
	return nil
}

// ---- 申请 ----

// acquireFixedLocked 为静态固定分配下的申请选端口（不调用独占分配的任何一步）：
//   - 新账户（登录、导入，还没有 ID）：负载最少、不暂停的端口，锁不到（被其他进程占用）换下一个，全部不行拒绝；成功时预留一个位置；
//   - 已有账户：Worker 一律用固定端口；登录、验证与导入先跟随仍走默认代理的直通 Worker，再跟随已启动、不暂停、所在端口没有
//     停用的过渡 Worker（同一账户同时只用一个 IP），否则用固定端口；
//   - 端口暂停使用或锁不到时拒绝（lease.refused），不换端口、不填默认代理。
//
// 共用不独占：shared 为假、不写亲和与归属
func (pool *proxyPool) acquireFixedLocked(lease *proxyPoolLease, now time.Time) []proxyPoolLog {
	var logs []proxyPoolLog
	if lease.accountID == "" {
		pool.fixedSweepReservedLocked(now)
		port := 0
		for _, candidate := range pool.fixedLoginCandidatesLocked(now) {
			// 锁不到时 lockPortLocked 已记下“被其他进程占用”（并推送这个端口上的固定账户的暂停）：新账户还没有分配，换下一个端口不算挪动
			if pool.lockPortLocked(candidate, now, &logs) {
				port = candidate
				break
			}
		}
		if port == 0 {
			lease.refused = &proxyPoolPausedError{Reason: fixedPauseNoPort}
			return append(logs, proxyPoolLog{source: lease.logSource(), level: "WARN", message: fmt.Sprintf(
				"IP 池固定分配没有可用的端口（全部暂停使用），不分配 | 用途=%s", proxyPoolKindLabel(lease.kind))})
		}
		lease.fixedToken = pool.fixedReserveLocked(port, time.Time{})
		return append(logs, pool.fixedBookLocked(lease, port, now, "新账户"))
	}
	// 删除或新增回滚之后才到的申请（账户已取消登记）不惰性分配：只用此刻会分到的端口，不写固定表
	port := pool.fixedPortLocked(lease.accountID, now, pool.fixedKnownLocked(lease.accountID))
	source := "固定"
	if lease.kind != proxyPoolKindWorker {
		followed, followLogs := pool.followServingLocked(lease, now)
		if followed {
			return append(logs, followLogs...)
		}
		if serving := pool.attachedWorkerLeaseLocked(lease.accountID); serving != nil && serving.pooled && serving.port != 0 &&
			serving.port != port && pool.currentStaticLocked(serving) && !pool.fixedExcludedLocked(serving.port) {
			// 过渡 Worker 所在的端口已“换 IP”停用时不跟随（它已登记后台关闭），改用固定端口
			if reason, _ := pool.fixedPauseLocked(serving.port, now); reason == "" {
				port, source = serving.port, "跟随运行中的 Worker"
			}
		}
	}
	if reason, until := pool.fixedPauseLocked(port, now); reason != "" {
		return append(logs, pool.fixedRefuseLocked(lease, port, reason, until))
	}
	if !pool.lockPortLocked(port, now, &logs) {
		_, until := pool.fixedPauseLocked(port, now)
		return append(logs, pool.fixedRefuseLocked(lease, port, fixedPauseLockedElsewhere, until))
	}
	return append(logs, pool.fixedBookLocked(lease, port, now, source))
}

// fixedRefuseLocked 拒绝申请：记下原因，返回 WARN 行
func (pool *proxyPool) fixedRefuseLocked(lease *proxyPoolLease, port int, reason string, until time.Time) proxyPoolLog {
	lease.refused = &proxyPoolPausedError{Port: port, Reason: reason, Until: until}
	return proxyPoolLog{source: lease.logSource(), level: "WARN", message: fmt.Sprintf(
		"IP 池固定端口暂停使用，不分配 | 端口=%d | 原因=%s | 用途=%s", port, fixedPauseLabel(reason), proxyPoolKindLabel(lease.kind))}
}

// fixedBookLocked 把租约记到端口上（共用，不独占、不写亲和与归属），返回分配日志
func (pool *proxyPool) fixedBookLocked(lease *proxyPoolLease, port int, now time.Time, source string) proxyPoolLog {
	state := pool.portLocked(port)
	if state.holders == nil {
		state.holders = make(map[string]int)
	}
	state.holders[lease.key]++
	state.lastUsedAt = now
	lease.port = port
	lease.slot = strconv.Itoa(port)
	lease.url = pool.staticURLLocked(port)
	lease.pooled = true
	lease.shared = false
	return proxyPoolLog{source: lease.logSource(), level: "INFO", message: fmt.Sprintf(
		"IP 池分配 | 端口=%d | 来源=%s | 用途=%s | 使用中=%d/%d", port, source, proxyPoolKindLabel(lease.kind),
		pool.portsInUseLocked(), pool.settings.size)}
}

// fixedLoginCandidatesLocked 返回新账户可以用的端口：参与分配、不暂停，按（负载、端口号）升序
func (pool *proxyPool) fixedLoginCandidatesLocked(now time.Time) []int {
	paused := pool.fixedPausedFuncLocked(now)
	var ports []int
	for _, port := range pool.fixedPortsLocked() {
		if !paused(port) {
			ports = append(ports, port)
		}
	}
	sort.SliceStable(ports, func(i, j int) bool {
		left, right := pool.fixedLoadLocked(ports[i]), pool.fixedLoadLocked(ports[j])
		if left != right {
			return left < right
		}
		return ports[i] < ports[j]
	})
	return ports
}

// fixedAdoptLocked 新账户登录成功：登录用的端口就是它的固定端口（撤销预留，写表，合并写盘；登录期间这个端口被“换 IP”停用或已不在
// 端口段内时改分到账户最少的端口，见 fixedAdoptPortLocked）；返回是否处理了（固定分配之外为假）
func (pool *proxyPool) fixedAdoptLocked(lease *proxyPoolLease, accountID string, now time.Time) bool {
	if lease.fixedToken == "" {
		return false
	}
	pool.fixedRevokeLocked(lease.fixedToken)
	lease.fixedToken = ""
	if !pool.fixedStaticLocked() || !lease.pooled || lease.port == 0 {
		return false
	}
	pool.fixedAdoptPortLocked(accountID, lease.port, now, fixedSourceLogin, "登录")
	pool.pushLocked(accountID)
	return true
}

// fixedClaimAdoptLocked Chrome 导入的新账户新建成功：导入用的端口就是它的固定端口（共用是常态，不检查端口有没有被别的键持有，换代理商也不改
// 分配）；端口已被“换 IP”停用或不在段内时改分到账户最少的端口。撤销预留，合并写盘
func (pool *proxyPool) fixedClaimAdoptLocked(claim *proxyPoolClaim, accountID string, now time.Time) {
	pool.fixedRevokeLocked(claim.fixedToken)
	claim.fixedToken = ""
	pool.fixedAdoptPortLocked(accountID, claim.port, now, fixedSourceImport, "导入")
	pool.pushLocked(accountID)
}

// fixedAdoptPortLocked 把新账户固定到登录或导入用的端口 used：used 在段内且没有停用时就是它；登录或导入期间 used 被“换 IP”停用（管理员
// 刚换掉这个 IP）或端口段变了时，分到参与分配、负载最少、不暂停的端口，日志写明原因。label 为“登录”或“导入”
func (pool *proxyPool) fixedAdoptPortLocked(accountID string, used int, now time.Time, source, label string) {
	// 还没进账户池：重新平均分配不把它当成已不在账户池的记录清理（进账户池后的同步、新增回滚时去掉）
	pool.fixed.adding[accountID] = now
	inRange, excluded := pool.inRangeLocked(used), pool.fixedExcludedLocked(used)
	if inRange && !excluded {
		pool.fixedSetLocked(accountID, used, now, source)
		pool.queueFixedLogLocked(accountID, "INFO", false, fmt.Sprintf("IP 池固定分配 | 端口=%d | 来源=%s | 该端口账户=%d",
			used, label, pool.fixedCountLocked(used)))
		return
	}
	pool.fixedSweepReservedLocked(now)
	port := fixedPickLeast(pool.fixedPortsLocked(), pool.fixedLoadLocked, pool.fixedPausedFuncLocked(now))
	pool.fixedSetLocked(accountID, port, now, source)
	why := "已不在端口段内"
	if inRange {
		why = "已换 IP 停用"
	}
	pool.queueFixedLogLocked(accountID, "INFO", false, fmt.Sprintf("IP 池固定分配 | 端口=%d | 来源=%s | 该端口账户=%d | %s用的端口 %d %s，改分到账户最少的端口",
		port, label, pool.fixedCountLocked(port), label, used, why))
}

// discard 放弃没有 adopt 的导入记录（导入中途失败、新建失败）：撤销它的预留。adopt 过的与不是固定分配的为空操作。方法对 nil 安全
func (claim *proxyPoolClaim) discard() {
	if claim == nil || claim.pool == nil || !claim.fixed {
		return
	}
	pool := claim.pool
	pool.mu.Lock()
	pool.fixedRevokeLocked(claim.fixedToken)
	claim.fixedToken = ""
	pool.mu.Unlock()
}

// releaseFixed 删除账户时释放它的固定位置（单个与批量删除、新增回滚）：只删这一条、不重排其他账户；不论开关（休眠的表同样释放）。
// 静态固定下同时取消这个账户在 IP 池的登记（之后的推送不再惰性分配）；动态固定会话下释放它的会话记录。没有条目时不写日志、
// 不写固定表。方法对 nil 安全
func (pool *proxyPool) releaseFixed(accountID, reason string) {
	if pool == nil || accountID == "" {
		return
	}
	pool.mu.Lock()
	delete(pool.fixed.adding, accountID)
	port, ok := pool.fixedDropLocked(accountID)
	var logs []proxyPoolLog
	if ok {
		logs = append(logs, proxyPoolLog{source: accountID, level: "INFO", message: fmt.Sprintf(
			"IP 池固定分配已释放 | 端口=%d | 原因=%s | 该端口账户=%d", port, reason, pool.fixedCountLocked(port))})
		if pool.fixedStaticLocked() {
			pool.fixedPortChangedLocked(port, pool.now())
		}
	}
	if state := pool.accounts[accountID]; state != nil && pool.fixedStaticLocked() {
		// 账户已不在账户池：取消登记，之后才归还的租约（就地重导入、新增回滚前的登录）推送它时不再惰性分配、把它写回固定表；
		// 账户回来时 prime 或账户池同步重新登记。记录本身在下一次全量同步时随“不在账户池”丢弃
		state.known = false
	}
	if pool.fixedDynamicLocked() {
		// 动态固定会话不按 30 天修剪会话记录（账户暂时移出账户池回来不换会话），删除账户时在这里释放
		if _, had := pool.sessions[accountID]; had {
			delete(pool.sessions, accountID)
			pool.markDirtyLocked()
		}
	}
	logs = append(logs, pool.takeFixedLogsLocked()...)
	pool.mu.Unlock()
	pool.emit(logs)
	if ok {
		pool.fixedFlush()
	}
}

// fixedObserveOwnProxyLocked 账户改用自己的代理（静态固定下）：释放它的位置（日志进待写队列，合并写盘；可能在账户池回调里调用）
func (pool *proxyPool) fixedObserveOwnProxyLocked(accountID string, state *proxyPoolAccount) {
	if state.proxy == "" || !pool.fixedStaticLocked() {
		return
	}
	port, ok := pool.fixedDropLocked(accountID)
	if !ok {
		return
	}
	pool.queueFixedLogLocked(accountID, "INFO", false, fmt.Sprintf("IP 池固定分配已释放 | 端口=%d | 原因=账户改用自己的代理 | 该端口账户=%d",
		port, pool.fixedCountLocked(port)))
}

// ---- 动态 ----

// fixedSessionKeepLocked 动态固定会话下达到换会话条件时不换，写一行 WARN（同一账户 30 分钟一次）；返回是否拦下了换会话
func (pool *proxyPool) fixedSessionKeepLocked(lease *proxyPoolLease, strikes int, now time.Time) ([]proxyPoolLog, bool) {
	if !pool.fixedDynamicLocked() {
		return nil, false
	}
	if last, ok := pool.fixed.sessionWarned[lease.accountID]; ok && now.Sub(last) < proxyPoolFixedSessionWarnWindow {
		return nil, true
	}
	pool.fixed.sessionWarned[lease.accountID] = now
	return []proxyPoolLog{{source: lease.logSource(), level: "WARN", message: fmt.Sprintf(
		"IP 池固定会话不换会话 | 会话=%s | 原因=连续 %d 次出口连不通 | 代理商恢复这个会话前，这个账户的 Worker 启动会失败", lease.slot, strikes)}}, true
}

// ---- 状态与账户页 ----

// fixedStatusLocked 返回状态接口的固定分配字段（固定分配没有生效时为 nil）：暂停的端口按此刻现算。known 为账户池里的账户（为 nil 时不算 stray；
// 正在新增与还有活动租约的不算，与重新平均分配的清理同一口径）
func (pool *proxyPool) fixedStatusLocked(now time.Time, known map[string]struct{}) *api.AdminProxyPoolFixed {
	fixed := &pool.fixed
	switch {
	case pool.fixedStaticLocked():
		ports := pool.fixedPortsLocked()
		status := &api.AdminProxyPoolFixed{
			Mode: config.ProxyPoolStatic, Since: proxyPoolUTC(fixed.since), Ports: len(ports), Accounts: len(fixed.entries),
			Reserved: len(fixed.reserved), Transitional: pool.fixedTransitionalLocked(), ExcludedTotal: len(fixed.excluded),
		}
		status.MinPerPort, status.MaxPerPort = pool.fixedSpreadLocked(ports)
		for _, port := range ports {
			if pool.fixedCountLocked(port) == 0 {
				status.Empty++
			}
		}
		if known != nil {
			for accountID := range fixed.entries {
				// 与重新平均分配的清理同一口径：正在新增（还没进账户池）与还有活动租约的不算
				if _, ok := known[accountID]; !ok && !pool.fixedAddingLocked(accountID, now) && len(pool.byAccount[accountID]) == 0 {
					status.Stray++
				}
			}
		}
		for accountID, state := range pool.accounts {
			if !state.known || state.proxy != "" {
				continue
			}
			if entry, ok := fixed.entries[accountID]; !ok || !pool.inRangeLocked(entry.port) {
				status.Unassigned++
			}
		}
		for port := range fixed.excluded {
			status.Excluded = append(status.Excluded, port)
		}
		sort.Ints(status.Excluded)
		if len(status.Excluded) > proxyPoolFixedListLimit {
			status.Excluded = status.Excluded[:proxyPoolFixedListLimit]
		}
		for port, users := range fixed.byPort {
			if len(users) == 0 || !pool.inRangeLocked(port) {
				continue
			}
			if pool.external[port] > 0 {
				status.ExternalPorts++
			}
			if reason, until := pool.fixedPauseLocked(port, now); reason != "" {
				status.PausedPorts++
				status.PausedAccounts += len(users)
				status.Paused = append(status.Paused, api.AdminProxyPoolPausedPort{
					Port: port, Reason: reason, Until: proxyPoolUTC(until), Accounts: len(users),
				})
			}
		}
		sort.Slice(status.Paused, func(i, j int) bool {
			if !status.Paused[i].Until.Equal(status.Paused[j].Until) {
				return status.Paused[i].Until.Before(status.Paused[j].Until)
			}
			return status.Paused[i].Port < status.Paused[j].Port
		})
		if len(status.Paused) > proxyPoolFixedListLimit {
			status.Paused = status.Paused[:proxyPoolFixedListLimit]
		}
		return status
	case pool.fixedDynamicLocked():
		status := &api.AdminProxyPoolFixed{Mode: config.ProxyPoolDynamic}
		for _, state := range pool.accounts {
			if state.known && state.proxy == "" {
				status.Accounts++
			}
		}
		return status
	}
	return nil
}

// markFixedLocked 给账户列表填固定端口（静态固定下有段内条目、没有自己代理的账户；只有端口号）
func (pool *proxyPool) markFixedLocked(accounts []api.AdminAccount) {
	if !pool.fixedStaticLocked() {
		return
	}
	for index := range accounts {
		accountID := accounts[index].ID
		if state := pool.accounts[accountID]; state != nil && state.proxy != "" {
			continue
		}
		if entry, ok := pool.fixed.entries[accountID]; ok && pool.inRangeLocked(entry.port) {
			accounts[index].ProxyPoolFixedPort = entry.port
		}
	}
}

// fixedLoginPreviewLocked 返回新账户登录此刻会分到的端口（只读：不加锁、不预留；跳过暂停的端口）
func (pool *proxyPool) fixedLoginPreviewLocked(now time.Time) int {
	candidates := pool.fixedLoginCandidatesLocked(now)
	if len(candidates) == 0 {
		return 0
	}
	return candidates[0]
}

// ---- 管理操作 ----

// errProxyPoolNotFixed 等为固定分配管理操作的错误（管理接口的中文说明）
func errProxyPoolNotFixed() error {
	return &adminOperationError{status: http.StatusConflict, code: "proxy_pool_not_fixed",
		message: "固定分配没有开启（需要 PROXY_POOL_MODE=static 且 PROXY_POOL_FIXED=true）"}
}

func errProxyPoolPortOutOfRange() error {
	return &adminOperationError{status: http.StatusBadRequest, code: "proxy_pool_port_out_of_range", message: "端口不在当前端口段内"}
}

func errProxyPoolNoTarget() error {
	return &adminOperationError{status: http.StatusConflict, code: "proxy_pool_no_target", message: "没有其他可分配的端口"}
}

// fixedPausedAdminError 把被拒的申请转成管理接口的错误：固定端口暂停为 409，新账户没有可用端口为 503；不含地址。原错误留作 cause，
// 调用方（新账户自动处理）可用 errors.Is(err, errProxyPoolPortPaused) 或 errors.As 取暂停原因与恢复时间
func fixedPausedAdminError(err error) error {
	var paused *proxyPoolPausedError
	if !errors.As(err, &paused) {
		return err
	}
	if paused.Port == 0 {
		return &adminOperationError{status: http.StatusServiceUnavailable, code: "proxy_pool_no_port",
			message: "IP 池固定分配没有可用的端口（全部暂停使用），恢复后再试", cause: paused}
	}
	return &adminOperationError{status: http.StatusConflict, code: "proxy_pool_port_paused", message: fmt.Sprintf(
		"这个账户的固定端口 %d 暂停使用（%s），恢复后再试；固定分配不会换端口", paused.Port, fixedPauseDetail(paused.Reason, paused.Until)),
		cause: paused}
}

// fixedKeepLocked 返回重新平均分配时账户的保留优先级：2 为有已启动的 Worker 在它的固定端口上服务，1 为有活动租约在固定端口上，0 为其他
func (pool *proxyPool) fixedKeepLocked(accountID string, port int) int {
	if pool.servesOnPortLocked(accountID, port) {
		return 2
	}
	for _, lease := range pool.byAccount[accountID] {
		if lease.pooled && lease.port == port {
			return 1
		}
	}
	return 0
}

// fixedStrayFuncLocked 返回“固定表里这条记录已不在账户池、重新平均分配时清理”的判断：known 为 nil（服务停止）时谁都不清理；否则不在 known、
// 也不在此刻（锁内再取一次，锁序 pool.mu → 账户池锁）的账户池里，没有“正在新增”的记号（adopt 或 prime 之后、AccountPool.Add 之前，
// 见 fixed.adding）也没有活动租约（还没归还的登录租约）的才算。顺带去掉过期的“正在新增”记号
func (pool *proxyPool) fixedStrayFuncLocked(known map[string]struct{}, now time.Time) func(string) bool {
	if known == nil {
		return func(string) bool { return false }
	}
	for accountID := range pool.fixed.adding {
		if !pool.fixedAddingLocked(accountID, now) {
			delete(pool.fixed.adding, accountID)
		}
	}
	var live map[string]struct{}
	if pool.owner != nil {
		ids := pool.owner.AccountIDs()
		live = make(map[string]struct{}, len(ids))
		for _, accountID := range ids {
			live[accountID] = struct{}{}
		}
	}
	return func(accountID string) bool {
		if _, ok := known[accountID]; ok {
			return false
		}
		if _, ok := live[accountID]; ok {
			return false
		}
		return !pool.fixedAddingLocked(accountID, now) && len(pool.byAccount[accountID]) == 0
	}
}

// fixedAddingLocked 判断账户有还没过期的“正在新增”记号（adopt 或 prime 之后、还没进账户池）
func (pool *proxyPool) fixedAddingLocked(accountID string, now time.Time) bool {
	since, ok := pool.fixed.adding[accountID]
	return ok && now.Sub(since) < proxyPoolFixedAddingTTL
}

// fixedRebalance 重新平均分配（最少挪动）：known 不为 nil（服务运行中）时先清理固定表里已不在账户池的记录（判断见 fixedStrayFuncLocked）；
// dryRun 时只计算。
// 被挪账户运行中的 Worker 不强制重启（关闭后改用新端口，结果的 running 告知）
func (pool *proxyPool) fixedRebalance(known map[string]struct{}, dryRun bool) (api.ProxyPoolFixedRebalanceResult, error) {
	pool.mu.Lock()
	if !pool.fixedStaticLocked() {
		pool.mu.Unlock()
		return api.ProxyPoolFixedRebalanceResult{}, errProxyPoolNotFixed()
	}
	now := pool.now()
	fixed := &pool.fixed
	ports := pool.fixedPortsLocked()
	if len(ports) == 0 {
		pool.mu.Unlock()
		return api.ProxyPoolFixedRebalanceResult{}, errProxyPoolNoTarget()
	}
	var pruned []string
	assign := make(map[string]int, len(fixed.entries))
	keep := make(map[string]int, len(fixed.entries))
	at := make(map[string]time.Time, len(fixed.entries))
	stray := pool.fixedStrayFuncLocked(known, now)
	for accountID, entry := range fixed.entries {
		if stray(accountID) {
			pruned = append(pruned, accountID)
			continue
		}
		assign[accountID] = entry.port
		keep[accountID] = pool.fixedKeepLocked(accountID, entry.port)
		at[accountID] = entry.at
	}
	sort.Strings(pruned)
	moves := fixedPlanRebalance(assign, ports, keep, at)
	counts := make(map[int]int, len(ports))
	for _, port := range assign {
		counts[port]++
	}
	running := 0
	for _, move := range moves {
		counts[move.From]--
		counts[move.To]++
		if pool.attachedWorkerLeaseLocked(move.Account) != nil {
			running++
		}
	}
	result := api.ProxyPoolFixedRebalanceResult{
		DryRun: dryRun, Accounts: len(assign), Ports: len(ports), Moved: len(moves), Pruned: len(pruned), Running: running,
	}
	for index, port := range ports {
		if index == 0 || counts[port] < result.MinPerPort {
			result.MinPerPort = counts[port]
		}
		result.MaxPerPort = max(result.MaxPerPort, counts[port])
	}
	var logs []proxyPoolLog
	changed := !dryRun && (len(moves) > 0 || len(pruned) > 0)
	if !dryRun {
		touched := make(map[int]struct{})
		for _, accountID := range pruned {
			if port, ok := pool.fixedDropLocked(accountID); ok {
				touched[port] = struct{}{}
			}
		}
		moved := make([]string, 0, len(moves))
		for _, move := range moves {
			pool.fixedSetLocked(move.Account, move.To, now, fixedSourceRebalance)
			touched[move.From], touched[move.To] = struct{}{}, struct{}{}
			moved = append(moved, move.Account)
			logs = append(logs, proxyPoolLog{source: move.Account, level: "INFO", message: fmt.Sprintf(
				"IP 池固定分配换端口 | 端口=%d → %d | 原因=重新平均分配", move.From, move.To)})
		}
		// 没有浏览器的账户立即换出口；端口的暂停记录随账户变化重算
		pool.pushLocked(moved...)
		pool.fixedRefreshTouchedLocked(touched, now)
		logs = append(logs, proxyPoolLog{source: "service", level: "INFO", durable: true, message: fmt.Sprintf(
			"IP 池固定分配已重新平均 | 账户=%d | 端口=%d | 挪动=%d | 清理=%d | 每端口=%d-%d | 运行中的浏览器=%d（关闭后改用新端口）",
			result.Accounts, result.Ports, result.Moved, result.Pruned, result.MinPerPort, result.MaxPerPort, result.Running)})
	}
	logs = append(logs, pool.takeFixedLogsLocked()...)
	pool.mu.Unlock()
	pool.emit(logs)
	if changed {
		pool.fixedFlush()
	}
	return result, nil
}

// fixedRefreshTouchedLocked 对账户变化过的端口重算暂停记录（被挪进正在暂停的端口的账户登记暂停，挪空的端口去掉记录）
func (pool *proxyPool) fixedRefreshTouchedLocked(touched map[int]struct{}, now time.Time) {
	ports := make([]int, 0, len(touched))
	for port := range touched {
		ports = append(ports, port)
	}
	sort.Ints(ports)
	for _, port := range ports {
		pool.fixedPortChangedLocked(port, now)
	}
}

// fixedReassign 给端口换 IP：这个端口上的账户按 ID 升序逐个挪到其他负载最少、不暂停的端口（全部暂停时退回全部），这个端口停用（之后不再分配，
// 可恢复；端口上没有账户时也停用）。dryRun 时只计算。返回结果与挪动前正在旧端口上服务的账户（调用方登记后台关闭它们的 Worker）
func (pool *proxyPool) fixedReassign(port int, dryRun bool) (api.ProxyPoolFixedReassignResult, []string, error) {
	pool.mu.Lock()
	if !pool.fixedStaticLocked() {
		pool.mu.Unlock()
		return api.ProxyPoolFixedReassignResult{}, nil, errProxyPoolNotFixed()
	}
	if !pool.inRangeLocked(port) {
		pool.mu.Unlock()
		return api.ProxyPoolFixedReassignResult{}, nil, errProxyPoolPortOutOfRange()
	}
	now := pool.now()
	var targets []int
	for _, candidate := range pool.fixedPortsLocked() {
		if candidate != port {
			targets = append(targets, candidate)
		}
	}
	if len(targets) == 0 {
		pool.mu.Unlock()
		return api.ProxyPoolFixedReassignResult{}, nil, errProxyPoolNoTarget()
	}
	movers := pool.fixedUsersLocked(port)
	load := make(map[int]int, len(targets))
	for _, target := range targets {
		load[target] = pool.fixedLoadLocked(target)
	}
	moves := fixedPlanEvacuate(movers, targets, load, pool.fixedPausedFuncLocked(now))
	result := api.ProxyPoolFixedReassignResult{DryRun: dryRun, Port: port, Moved: len(moves), Excluded: !dryRun}
	// 此刻正用这个端口服务的全部 Worker（与端口冷却同一口径）：被挪走的固定账户，以及固定端口在别处、仍在这个端口上的过渡 Worker
	// （开启前就在这里、或重新平均分配挪走后还没关闭的）。换 IP 就是不再用这个 IP，它们都登记后台关闭，下次启动用各自的固定端口
	serving := pool.portWorkerAccountsLocked(pool.portHolderAccountsLocked(port), port)
	result.Closing = len(serving)
	seen := make(map[int]struct{})
	for _, move := range moves {
		if _, ok := seen[move.To]; !ok {
			seen[move.To] = struct{}{}
			result.Targets = append(result.Targets, move.To)
		}
		if pool.attachedWorkerLeaseLocked(move.Account) != nil {
			result.Running++
		}
	}
	sort.Ints(result.Targets)
	var logs []proxyPoolLog
	if !dryRun {
		pool.fixed.excluded[port] = now
		touched := map[int]struct{}{port: {}}
		for _, move := range moves {
			pool.fixedSetLocked(move.Account, move.To, now, fixedSourceReassign)
			touched[move.To] = struct{}{}
			logs = append(logs, proxyPoolLog{source: move.Account, level: "INFO", message: fmt.Sprintf(
				"IP 池固定分配换端口 | 端口=%d → %d | 原因=换 IP", port, move.To)})
		}
		pool.fixedMarkDirtyLocked()
		pool.pushLocked(movers...)
		pool.fixedRefreshTouchedLocked(touched, now)
	}
	if len(result.Targets) > proxyPoolFixedTargetsLimit {
		result.Targets = result.Targets[:proxyPoolFixedTargetsLimit]
	}
	logs = append(logs, pool.takeFixedLogsLocked()...)
	pool.mu.Unlock()
	pool.emit(logs)
	if !dryRun {
		pool.fixedFlush()
	}
	return result, serving, nil
}

// fixedReassignLog 为“换 IP”的汇总行（登记了后台关闭之后写，带重启的 Worker 数）
func fixedReassignLog(result api.ProxyPoolFixedReassignResult) string {
	parts := make([]string, 0, proxyPoolFixedLogTargets)
	for index, target := range result.Targets {
		if index >= proxyPoolFixedLogTargets {
			break
		}
		parts = append(parts, strconv.Itoa(target))
	}
	targets := strings.Join(parts, "、")
	if len(result.Targets) > proxyPoolFixedLogTargets {
		targets += "等"
	}
	if targets == "" {
		targets = "无"
	}
	return fmt.Sprintf("IP 池固定端口换 IP | 端口=%d | 挪动=%d | 挪到=%s | 重启 Worker=%d | 这个端口不再分配（可在 IP 池设置里恢复）",
		result.Port, result.Moved, targets, result.Restarted)
}

// fixedRestore 恢复“换 IP”后停用的端口：不挪任何账户（之后新增账户与重新平均分配会用到它）；端口没有停用时 restored 为假
func (pool *proxyPool) fixedRestore(port int) (api.ProxyPoolFixedRestoreResult, error) {
	pool.mu.Lock()
	if !pool.fixedStaticLocked() {
		pool.mu.Unlock()
		return api.ProxyPoolFixedRestoreResult{}, errProxyPoolNotFixed()
	}
	result := api.ProxyPoolFixedRestoreResult{Port: port}
	var logs []proxyPoolLog
	if _, ok := pool.fixed.excluded[port]; ok {
		delete(pool.fixed.excluded, port)
		pool.fixedMarkDirtyLocked()
		result.Restored = true
		logs = append(logs, proxyPoolLog{source: "service", level: "INFO", durable: true, message: fmt.Sprintf("IP 池固定端口恢复分配 | 端口=%d", port)})
	}
	logs = append(logs, pool.takeFixedLogsLocked()...)
	pool.mu.Unlock()
	pool.emit(logs)
	if result.Restored {
		pool.fixedFlush()
	}
	return result, nil
}

// ---- 持久化 ----

// proxyPoolFixedRecord 为 runtime/proxy-pool-fixed.json（版本 1，0600）：只有账户 ID、端口号、时间与来源代码
type proxyPoolFixedRecord struct {
	Version   int                                  `json:"version"`
	SavedAt   time.Time                            `json:"saved_at"`
	Since     time.Time                            `json:"since,omitzero"`
	PortStart int                                  `json:"port_start,omitempty"`
	PortEnd   int                                  `json:"port_end,omitempty"`
	Accounts  map[string]proxyPoolFixedEntryRecord `json:"accounts"`
	Excluded  map[string]time.Time                 `json:"excluded,omitempty"`
}

type proxyPoolFixedEntryRecord struct {
	Port   int       `json:"port"`
	At     time.Time `json:"at"`
	Source string    `json:"source,omitempty"`
}

// loadFixed 载入固定表（newProxyPool 里、proxy-pool.json 之后调用，还没有其他协程）：不存在时为空表；JSON 坏或版本不认识时不覆盖，
// 改名保留为 proxy-pool-fixed.json.bad-<时间> 后从空表开始（下次开启按首次迁移从亲和确定性重算）；改名也失败时本次运行不写盘
func (pool *proxyPool) loadFixed() []proxyPoolLog {
	fixed := &pool.fixed
	if fixed.path == "" {
		return nil
	}
	data, err := os.ReadFile(fixed.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	var record proxyPoolFixedRecord
	if err == nil {
		err = json.Unmarshal(data, &record)
	}
	if err == nil && record.Version != proxyPoolFixedVersion {
		err = fmt.Errorf("版本 %d 不认识", record.Version)
	}
	if err != nil {
		name := proxyPoolFixedFileName + ".bad-" + pool.now().UTC().Format("20060102T150405Z")
		if renameErr := os.Rename(fixed.path, filepath.Join(filepath.Dir(fixed.path), name)); renameErr != nil {
			fixed.path = ""
			return []proxyPoolLog{{source: "service", level: "WARN", durable: true, message: fmt.Sprintf(
				"IP 池固定分配记录读取失败且无法改名保留，本次运行不写分配记录 | 错误=%s", err.Error())}}
		}
		return []proxyPoolLog{{source: "service", level: "WARN", durable: true, message: fmt.Sprintf(
			"IP 池固定分配记录读取失败，已改名保留为 %s，从头分配 | 错误=%s", name, err.Error())}}
	}
	fixed.since = record.Since
	fixed.start, fixed.end = record.PortStart, record.PortEnd
	for accountID, entry := range record.Accounts {
		accountID = strings.TrimSpace(accountID)
		if accountID == "" || entry.Port < 1 || entry.Port > 65535 {
			continue
		}
		fixed.entries[accountID] = proxyPoolFixedEntry{port: entry.Port, at: entry.At, source: entry.Source}
		users := fixed.byPort[entry.Port]
		if users == nil {
			users = make(map[string]struct{})
			fixed.byPort[entry.Port] = users
		}
		users[accountID] = struct{}{}
	}
	for key, at := range record.Excluded {
		if port, err := strconv.Atoi(key); err == nil && port >= 1 && port <= 65535 {
			fixed.excluded[port] = at
		}
	}
	return nil
}

// fixedMarkDirtyLocked 记下固定表有变化：proxyPoolFixedSaveDelay 后合并写盘一次（立即写的路径另在锁外调 fixedFlush）
func (pool *proxyPool) fixedMarkDirtyLocked() {
	fixed := &pool.fixed
	fixed.dirty = true
	if fixed.path == "" || fixed.saveTimer != nil {
		return
	}
	fixed.saveTimer = time.AfterFunc(proxyPoolFixedSaveDelay, pool.fixedFlush)
}

// fixedFlush 写出未保存的固定表：saveMu 内、锁内复制快照，锁外编码与原子写入；写入失败时 WARN、保持 dirty。从没开过（没有迁移过、表与
// 停用端口都为空）时不写，文件永不出现。调用方不能持有 pool.mu 或账户池的锁。方法对 nil 安全
func (pool *proxyPool) fixedFlush() {
	if pool == nil {
		return
	}
	fixed := &pool.fixed
	fixed.saveMu.Lock()
	defer fixed.saveMu.Unlock()
	pool.mu.Lock()
	if fixed.saveTimer != nil {
		fixed.saveTimer.Stop()
		fixed.saveTimer = nil
	}
	if fixed.path == "" || !fixed.dirty || fixed.since.IsZero() && len(fixed.entries) == 0 && len(fixed.excluded) == 0 {
		if fixed.path == "" {
			fixed.dirty = false
		}
		pool.mu.Unlock()
		return
	}
	record := pool.fixedSnapshotLocked(pool.now())
	fixed.dirty = false
	path := fixed.path
	pool.mu.Unlock()
	data, err := json.MarshalIndent(record, "", "  ")
	if err == nil {
		err = writeFileAtomic(path, append(data, '\n'), 0o600)
	}
	if err != nil {
		pool.mu.Lock()
		fixed.dirty = true
		pool.mu.Unlock()
		pool.emit([]proxyPoolLog{{source: "service", level: "WARN", message: "IP 池固定分配记录写入失败，下次变化再写 | 错误=" + err.Error()}})
	}
}

// fixedSnapshotLocked 复制要写盘的固定表
func (pool *proxyPool) fixedSnapshotLocked(now time.Time) proxyPoolFixedRecord {
	fixed := &pool.fixed
	record := proxyPoolFixedRecord{
		Version: proxyPoolFixedVersion, SavedAt: now.UTC(), Since: proxyPoolUTC(fixed.since),
		PortStart: fixed.start, PortEnd: fixed.end,
		Accounts: make(map[string]proxyPoolFixedEntryRecord, len(fixed.entries)),
	}
	for accountID, entry := range fixed.entries {
		record.Accounts[accountID] = proxyPoolFixedEntryRecord{Port: entry.port, At: proxyPoolUTC(entry.at), Source: entry.source}
	}
	if len(fixed.excluded) > 0 {
		record.Excluded = make(map[string]time.Time, len(fixed.excluded))
		for port, at := range fixed.excluded {
			record.Excluded[strconv.Itoa(port)] = proxyPoolUTC(at)
		}
	}
	return record
}

// fixedWorkerRefusedText 为 Worker 名额到手后申请出口被拒（固定端口暂停使用）时的日志
func fixedWorkerRefusedText(err error) string {
	var paused *proxyPoolPausedError
	if !errors.As(err, &paused) {
		return "WAA Worker 不启动：IP 池固定端口暂停使用"
	}
	return fmt.Sprintf("WAA Worker 不启动：IP 池固定端口暂停使用 | 端口=%d | 原因=%s", paused.Port, fixedPauseLabel(paused.Reason))
}
