package app

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/api"
)

// 固定分配下坏 IP 自动换（PROXY_POOL_FIXED_AUTO_REPLACE，见 config.Config.ProxyPoolFixedAutoReplace）：
//   - 静态：端口真正冷却（规则 A、规则 B，走过了整体故障保护）时，在 coolLocked 里对这个端口做与手动“换 IP”同样的挪动与停用：账户按 ID 升序
//     逐个挪到其他账户最少、此刻不暂停、没有停用的端口，坏端口停用（之后可在管理页恢复），此刻用它的 Worker 由端口冷却回调登记后台关闭（等
//     进行中的请求结束）；挪动时还在旧端口上服务或启动中的 Worker 让账户绑定带暂停说明（fixedAutoHoldLocked），关闭后解除；
//   - 动态固定会话：账户连续出口连不通达到换会话条件时自动换这个账户的会话号（与非固定的动态模式相同）；
//   - 坏得太多时停止自动换（熔断），之后坏掉的按原来的规则暂停、不换，等管理员手动处理。四条门槛，任一条成立即熔断：
//     (1) 30 分钟（滑动窗口）内坏掉的端口或会话数超过上限；
//     (2) 静态：再自动换会让端口段里能用的端口（没有停用、不在冷却、没被其他进程占用）不到参与计算的端口（被其他进程占用的不计入）的一半
//     （端口少的部署里一次成片故障到不了上限，却会把账户一个个挤到最后几个端口上）；
//     (3) 连锁：坏掉的端口上有 30 分钟内刚被自动换 IP 挪过来的账户，或账户 30 分钟内自动换出的新会话又连不通——换过的新 IP 也坏了，像代理商
//     整体出了问题（账户少时一次整体故障里同时出错的端口不到整体故障保护的 5 个、也到不了上限，没有这条会每隔十几分钟给每个账户换一次 IP，
//     故障过去后全部账户都换了 IP；动态下 10 分钟的最短间隔让一个账户 30 分钟内最多 3 个坏会话，上限 10 时同一账户反复连不通永远不熔断）；
//     (4) 按账户：30 分钟内 IP（会话）坏掉的账户超过走 IP 池的账户的一半（账户多时上限先起作用，这条只在小部署里生效）。
//     (2) 在判断有没有可挪的端口之前判断，(3)(4) 在确认有可挪的端口之后判断（没有可挪的端口时照旧按 no_target 暂停，不因它们熔断）；
//     熔断跨重启保留，只有管理页“恢复自动换”解除（同时清空计数与连锁、按账户的记录）；已经自动换过的不撤回；
//   - 上限为 0（以及 IP 池从没设置过上限）时以上全部不发生：状态、日志、绑定与固定表文件与没有这项设置时逐字节相同。
//
// 被其他进程占用、换代理商、服务停止、重启后读回的冷却、端口上没有固定账户都不算坏。全部在 pool.mu 内完成（map 操作与推送，没有文件 I/O、
// 没有等待），日志进待写队列由入口解锁前取走（见 takeFixedLogsLocked），写盘由 fixedMarkDirtyLocked 的定时器在自己的协程里做

const (
	// proxyPoolFixedAutoWindow 为自动换的计数窗口（滑动，now − 记录时间 < 30 分钟算在窗口内）。30 分钟而不是 10 分钟：固定分配下坏端口几乎都来自
	// 规则 B，规则 B 只在 Worker 启动时检出（连续两次启动失败，启动失败还有退避），同一次成片故障要十几到几十分钟才陆续检出，10 分钟的窗口会把它
	// 切成几段、每段都不超过上限，结果全部被自动换；被挪的账户压到目标端口上引起的下一波（规则 A 的 10 分钟窗口加换端口重启）也落在 30 分钟里
	proxyPoolFixedAutoWindow = 30 * time.Minute
	// proxyPoolFixedAutoListLimit 为状态接口里自动换过的端口、会话连不通的账户列表上限
	proxyPoolFixedAutoListLimit = 50
)

// 暂停端口没有自动换的原因（状态接口 paused[].auto）
const (
	fixedAutoHalted   = "halted"    // 坏 IP 过多，已停止自动换
	fixedAutoNoTarget = "no_target" // 当时没有可挪的端口
)

// 熔断的原因（状态接口 auto.halted_reason，持久化）
const (
	fixedHaltLimit    = "limit"    // 30 分钟内坏掉的个数超过上限
	fixedHaltHalf     = "half"     // 静态：再自动换的话，端口段里能用的端口会不到一半
	fixedHaltChain    = "chain"    // 换过的新 IP（新会话）30 分钟内又坏了
	fixedHaltAccounts = "accounts" // 30 分钟内 IP（会话）坏掉的账户超过走 IP 池的账户的一半
)

// fixedSourceAuto 为固定表条目的来源代码：自动换 IP
const fixedSourceAuto = "auto"

// proxyPoolFixedAuto 为自动换坏 IP / 坏会话的状态（proxyPoolFixed 的值字段，受 pool.mu 保护）
type proxyPoolFixedAuto struct {
	limit    int  // PROXY_POOL_FIXED_AUTO_REPLACE；0 为不自动换；setFixedAutoReplace 之前为 0
	limitSet bool // 已按配置设置过（第一次设置不算“设置已更新”）
	// ports、sessions 为窗口内坏掉的端口 → 最近一次、坏掉的会话（fixedSessionKey：账户 ID 加会话号）→ 最近一次（持久化；同一端口或同一会话
	// 窗口内只算一个；账户换了会话后新会话再坏另算一个，与静态下被挪账户的新端口再坏另算一个相同；两种“新的又坏了”同时按连锁熔断）
	ports    map[int]time.Time
	sessions map[string]time.Time
	// affected 为静态下窗口内坏掉的端口上的账户 → 最近一次（持久化；连锁与按账户的门槛用，随 ports 一起剪、随恢复清空；动态下由 sessions 的
	// 键得出，不用它）
	affected map[string]time.Time
	// haltedAt 为熔断时刻（零为没有熔断），haltedCount 为熔断时窗口内的个数，haltedReason 为熔断原因（fixedHalt*），
	// haltedKey 为触发熔断的端口（"p<端口>"）或会话（"s<会话键>"）（持久化）
	haltedAt     time.Time
	haltedCount  int
	haltedReason string
	haltedKey    string
	// afterHalt 为熔断之后又坏、没有自动换的端口与会话（键同 haltedKey；不含触发熔断的那一个；持久化，状态里的 skipped 是它的个数）
	afterHalt map[string]struct{}
	// replaced 为自动换过、仍停用的端口（持久化；是 excluded 的子集）
	replaced map[int]proxyPoolFixedReplaced
	// noTarget 为当时没有可挪的端口、按暂停处理的端口 → 时刻（只在内存；端口恢复或空了时去掉）
	noTarget map[int]time.Time
	// stuck 为动态固定会话下会话连不通、没有自动换的账户（只在内存）
	stuck map[string]proxyPoolFixedStuck
}

// proxyPoolFixedReplaced 为一个自动换过的端口：rule 为冷却规则，moved 为挪走的账户数
type proxyPoolFixedReplaced struct {
	at    time.Time
	rule  string
	moved int
}

// proxyPoolFixedStuck 为会话连不通、没有自动换的账户：session 为当时的会话号，since 为开始时刻
type proxyPoolFixedStuck struct {
	session string
	since   time.Time
}

// init 初始化自动换的状态（proxyPoolFixed.init 里调用）
func (auto *proxyPoolFixedAuto) init() {
	auto.ports = make(map[int]time.Time)
	auto.sessions = make(map[string]time.Time)
	auto.affected = make(map[string]time.Time)
	auto.afterHalt = make(map[string]struct{})
	auto.replaced = make(map[int]proxyPoolFixedReplaced)
	auto.noTarget = make(map[int]time.Time)
	auto.stuck = make(map[string]proxyPoolFixedStuck)
}

// halted 判断已熔断（坏得太多、停止自动换）
func (auto *proxyPoolFixedAuto) halted() bool {
	return !auto.haltedAt.IsZero()
}

// haltedReasonText 返回熔断原因（没有记下原因或不认识的旧记录按超过上限）
func (auto *proxyPoolFixedAuto) haltedReasonText() string {
	switch auto.haltedReason {
	case fixedHaltHalf, fixedHaltChain, fixedHaltAccounts:
		return auto.haltedReason
	}
	return fixedHaltLimit
}

// prune 剪掉窗口外的记录
func (auto *proxyPoolFixedAuto) prune(now time.Time) {
	for port, at := range auto.ports {
		if now.Sub(at) >= proxyPoolFixedAutoWindow {
			delete(auto.ports, port)
		}
	}
	for session, at := range auto.sessions {
		if now.Sub(at) >= proxyPoolFixedAutoWindow {
			delete(auto.sessions, session)
		}
	}
	for accountID, at := range auto.affected {
		if now.Sub(at) >= proxyPoolFixedAutoWindow {
			delete(auto.affected, accountID)
		}
	}
}

// recent 返回窗口内坏掉的端口与会话数（只数，不删）
func (auto *proxyPoolFixedAuto) recent(now time.Time) int {
	count := 0
	for _, at := range auto.ports {
		if now.Sub(at) < proxyPoolFixedAutoWindow {
			count++
		}
	}
	for _, at := range auto.sessions {
		if now.Sub(at) < proxyPoolFixedAutoWindow {
			count++
		}
	}
	return count
}

// persistent 判断有要写盘的自动换状态（窗口记录、熔断或自动换过的端口）：没有时固定表文件与没有这项设置时相同
func (auto *proxyPoolFixedAuto) persistent() bool {
	return len(auto.ports) > 0 || len(auto.sessions) > 0 || len(auto.affected) > 0 || len(auto.replaced) > 0 || auto.halted()
}

// setFixedAutoReplace 设置坏 IP 自动换的上限（PROXY_POOL_FIXED_AUTO_REPLACE；newRuntime 在第一次 configure 之前、热更新在 configure 之前调用）：
// 返回是否改变了已设置的值（第一次设置不算）。改小不回溯：已经自动换过的不撤回，下一次坏按新上限判断；熔断不随设置改变。方法对 nil 安全
func (pool *proxyPool) setFixedAutoReplace(limit int) bool {
	if pool == nil {
		return false
	}
	limit = max(limit, 0)
	pool.mu.Lock()
	defer pool.mu.Unlock()
	auto := &pool.fixed.auto
	changed := auto.limitSet && auto.limit != limit
	auto.limit, auto.limitSet = limit, true
	return changed
}

// fixedSessionKey 返回动态固定会话下一个会话的计数键（账户 ID、“#”、会话号；会话号只有小写字母与数字）
func fixedSessionKey(accountID, session string) string {
	return accountID + "#" + session
}

// fixedSessionAccount 返回会话计数键里的账户 ID（最后一个“#”之前；账户 ID 本身可以带“#”，会话号不带）
func fixedSessionAccount(key string) string {
	if index := strings.LastIndex(key, "#"); index >= 0 {
		return key[:index]
	}
	return key
}

// sessionChained 判断窗口里这个账户还有别的会话坏过（session 为刚记下的这一个）：自动换出的新会话又连不通（连锁）。只有自动换会话前记下的才在
// 窗口里（手动换会话不记；熔断后记下的随恢复清空），调用方已剪过窗口
func (auto *proxyPoolFixedAuto) sessionChained(accountID, session string) bool {
	for key := range auto.sessions {
		if key != session && fixedSessionAccount(key) == accountID {
			return true
		}
	}
	return false
}

// sessionAccounts 返回窗口里会话坏过的不同账户数（调用方已剪过窗口）
func (auto *proxyPoolFixedAuto) sessionAccounts() int {
	accounts := make(map[string]struct{}, len(auto.sessions))
	for key := range auto.sessions {
		accounts[fixedSessionAccount(key)] = struct{}{}
	}
	return len(accounts)
}

// fixedAutoPooledLocked 返回走 IP 池的账户数（已登记、没有自己的代理）：按账户的门槛的分母
func (pool *proxyPool) fixedAutoPooledLocked() int {
	count := 0
	for _, state := range pool.accounts {
		if state != nil && state.known && state.proxy == "" {
			count++
		}
	}
	return count
}

// fixedAutoNoteLocked 记下一个坏掉的端口（session 为空）或会话（session 为 fixedSessionKey），返回记下之后窗口内的个数：先剪掉窗口外的记录，
// 同一端口或同一会话只算一个
func (pool *proxyPool) fixedAutoNoteLocked(port int, session string, now time.Time) int {
	auto := &pool.fixed.auto
	auto.prune(now)
	if session != "" {
		auto.sessions[session] = now
	} else {
		auto.ports[port] = now
	}
	pool.fixedMarkDirtyLocked()
	return auto.recent(now)
}

// fixedRuleLabel 返回冷却规则的中文
func fixedRuleLabel(rule string) string {
	if rule == proxyPoolRuleRateLimited {
		return "不同账号返回限流"
	}
	return "连续出口连不通"
}

// haltLocked 熔断（停止自动换）：key 为触发的端口或会话，reason 为 fixedHalt*，count 为窗口内的个数
func (auto *proxyPoolFixedAuto) haltLocked(key, reason string, count int, now time.Time) {
	auto.haltedAt, auto.haltedCount, auto.haltedReason, auto.haltedKey = now, count, reason, key
	clear(auto.afterHalt)
}

// skippedLocked 记下熔断之后又坏、没有自动换的端口或会话（触发熔断的那一个再坏不算），返回熔断之后又坏的个数
func (auto *proxyPoolFixedAuto) skippedLocked(key string) int {
	if key != auto.haltedKey {
		auto.afterHalt[key] = struct{}{}
	}
	return len(auto.afterHalt)
}

// fixedAutoReplaceLocked 端口真正冷却时（coolLocked 的固定分配分支，过滤 cooling.Workers 之前）自动换 IP：上限为 0 或端口上没有固定账户时什么也
// 不做、不计数；否则记进窗口（端口与它上面的账户），已熔断、这一个让窗口内超过上限、或再自动换会让能用的端口不到一半时按暂停处理（后两种熔断），
// 没有可挪的端口时也按暂停处理；有可挪的端口时再看连锁（这个端口上有窗口内刚被自动挪过来的账户）与按账户的门槛（窗口内 IP 坏掉的账户超过走
// IP 池的账户的一半），成立时熔断；其余把这个端口上的账户按 ID 升序逐个挪到其他账户最少、此刻不暂停、没有停用的端口，这个端口停用。返回挪走
// 的账户数（0 为没有自动换）。只做 map 操作与推送
func (pool *proxyPool) fixedAutoReplaceLocked(port int, rule string, now time.Time) int {
	fixed := &pool.fixed
	auto := &fixed.auto
	users := pool.fixedCountLocked(port)
	if auto.limit <= 0 || users == 0 {
		return 0
	}
	count := pool.fixedAutoNoteLocked(port, "", now)
	movers := pool.fixedUsersLocked(port)
	// 连锁要在记下这个端口上的账户之前数（记下之后每个账户都在 affected 里）
	chained := pool.fixedAutoChainedLocked(movers, now)
	for _, accountID := range movers {
		auto.affected[accountID] = now
	}
	label := fixedRuleLabel(rule)
	key := "p" + strconv.Itoa(port)
	if auto.halted() {
		skipped := auto.skippedLocked(key)
		pool.queueFixedLogLocked("service", "INFO", false, fmt.Sprintf(
			"IP 池自动换 IP 已停止（坏 IP 过多），这个端口按暂停处理 | 端口=%d | 原因=%s | 账户=%d | 停止后又坏=%d",
			port, label, users, skipped))
		return 0
	}
	if count > auto.limit {
		pool.fixedAutoHaltStaticLocked(port, key, fixedHaltLimit, "", count, now)
		return 0
	}
	// 相对门槛：再挪只会把账户越挤越少的端口上（端口少的部署里整体故障保护的门槛 5 个与上限都到不了，一次成片故障会把全部账户逐个挪到最后一个
	// 端口）时按坏得太多熔断。被其他进程占用的端口不算坏，也不是可挪的目标：分子分母都不计入（计入分母的话，目标越来越少相对门槛却一直不触发，
	// 账户照样被挤到最后一个能用的端口上）。参与计算的端口不到 2 个时不按这一条（下面按没有可挪的端口处理）
	if usable, base, locked := pool.fixedAutoUsableLocked(port, now); base > 1 && 2*usable < base {
		detail := fmt.Sprintf("能用的端口不到一半：再自动换只剩 %d/%d 个（停用与冷却中的不算）", usable, base)
		if locked > 0 {
			detail = fmt.Sprintf("能用的端口不到一半：再自动换只剩 %d/%d 个（停用与冷却中的不算，另有 %d 个被其他进程占用、不计入）", usable, base, locked)
		}
		pool.fixedAutoHaltStaticLocked(port, key, fixedHaltHalf, detail, count, now)
		return 0
	}
	targets := pool.fixedAutoTargetsLocked(port, now)
	if len(targets) == 0 {
		auto.noTarget[port] = now
		pool.queueFixedLogLocked("service", "WARN", true, fmt.Sprintf(
			"IP 池固定端口没有自动换 IP：没有可挪的端口（其余端口都暂停或停用），这个端口上的账户暂停使用 | 端口=%d | 原因=%s | 账户=%d | 30 分钟内坏掉=%d/%d",
			port, label, users, count, auto.limit))
		return 0
	}
	if chained > 0 {
		pool.fixedAutoHaltStaticLocked(port, key, fixedHaltChain, fmt.Sprintf(
			"换过的新 IP 又坏了：这个端口上 %d 个账户是 30 分钟内自动换 IP 挪过来的（像是代理商整体出了问题）", chained), count, now)
		return 0
	}
	if base := pool.fixedAutoPooledLocked(); base > 1 && 2*len(auto.affected) > base {
		pool.fixedAutoHaltStaticLocked(port, key, fixedHaltAccounts, fmt.Sprintf(
			"IP 坏掉的账户超过一半：30 分钟内 %d/%d 个账户所在的端口坏了（像是代理商整体出了问题）", len(auto.affected), base), count, now)
		return 0
	}
	load := make(map[int]int, len(targets))
	for _, target := range targets {
		load[target] = pool.fixedLoadLocked(target)
	}
	moves := fixedPlanEvacuate(movers, targets, load, nil)
	// 先停用并记下自动换过，再改表与推送：推送时还在旧端口上的 Worker 的账户按 fixedAutoHoldLocked 带暂停说明
	fixed.excluded[port] = now
	auto.replaced[port] = proxyPoolFixedReplaced{at: now, rule: rule, moved: len(moves)}
	delete(auto.noTarget, port)
	touched := make(map[int]struct{}, len(moves))
	var moved []int
	for _, move := range moves {
		pool.fixedSetLocked(move.Account, move.To, now, fixedSourceAuto)
		if _, ok := touched[move.To]; !ok {
			touched[move.To] = struct{}{}
			moved = append(moved, move.To)
		}
		pool.queueFixedLogLocked(move.Account, "INFO", false, fmt.Sprintf("IP 池固定分配换端口 | 端口=%d → %d | 原因=自动换 IP（%s）",
			port, move.To, label))
	}
	pool.fixedMarkDirtyLocked()
	// 没有浏览器的账户立即换出口；还在这个端口上的 Worker（被挪账户的与过渡 Worker）的账户带自动换的暂停；目标端口都不暂停，暂停记录照
	// 手动“换 IP”的写法重算
	pool.pushLocked(append(movers, pool.portHolderAccountsLocked(port)...)...)
	pool.fixedRefreshTouchedLocked(touched, now)
	fixed.notify = true
	sort.Ints(moved)
	pool.queueFixedLogLocked("service", "WARN", true, fmt.Sprintf(
		"IP 池固定端口自动换 IP | 端口=%d | 原因=%s | 挪动=%d | 挪到=%s | 30 分钟内坏掉=%d/%d | 这个端口不再分配（可在 IP 池设置里恢复），用它的 Worker 在进行中的请求结束后关闭",
		port, label, len(moves), fixedTargetsText(moved), count, auto.limit))
	return len(moves)
}

// fixedAutoTargetsLocked 返回自动换 IP 可以挪去的端口（升序）：当前端口段内、没有停用、此刻不暂停（冷却、被其他进程占用都不算）、不是 port。
// 不用 fixedPortsLocked（段内全部停用时退回全部）与 fixedPickLeast 的 avoid（全部暂停时退回全部）：没有可挪的端口时宁可暂停，不挪到坏端口上
func (pool *proxyPool) fixedAutoTargetsLocked(port int, now time.Time) []int {
	var targets []int
	if pool.settings.size == 0 {
		return nil
	}
	for candidate := pool.settings.portStart; candidate <= pool.settings.portEnd; candidate++ {
		if candidate == port || pool.fixedExcludedLocked(candidate) {
			continue
		}
		if reason, _ := pool.fixedPauseLocked(candidate, now); reason != "" {
			continue
		}
		targets = append(targets, candidate)
	}
	return targets
}

// fixedAutoUsableLocked 返回相对门槛的三个数：usable 为自动换 port 之后还能用的端口（不是 port、没有停用、不在冷却、没被其他进程占用，即可挪的
// 目标数），base 为参与计算的端口（端口段里除去没有停用、不在冷却、被其他进程占用的端口；port 此刻已在冷却，计入），locked 为除去的端口数。
// 被其他进程占用不算坏，所以不计入分母；占用的端口同时在冷却或已停用时按坏的计入
func (pool *proxyPool) fixedAutoUsableLocked(port int, now time.Time) (usable, base, locked int) {
	if pool.settings.size == 0 {
		return 0, 0, 0
	}
	for candidate := pool.settings.portStart; candidate <= pool.settings.portEnd; candidate++ {
		excluded, cooling := pool.fixedExcludedLocked(candidate), pool.coolingLocked(candidate, now)
		if _, elsewhere := pool.lockedElsewhere[candidate]; elsewhere && candidate != port && !excluded && !cooling {
			locked++
			continue
		}
		base++
		if candidate != port && !excluded && !cooling {
			usable++
		}
	}
	return usable, base, locked
}

// fixedAutoChainedLocked 返回 users（坏掉的端口上的账户）里窗口内刚被自动换 IP 挪到这个端口的账户数：条目来源为自动换、分配时间在窗口内，且
// 窗口里记过它所在的端口坏了（恢复自动换时清空，恢复之前挪的不算）。在记下这个端口的账户之前调用，调用方已剪过窗口
func (pool *proxyPool) fixedAutoChainedLocked(users []string, now time.Time) int {
	chained := 0
	for _, accountID := range users {
		entry, ok := pool.fixed.entries[accountID]
		if !ok || entry.source != fixedSourceAuto || now.Sub(entry.at) >= proxyPoolFixedAutoWindow {
			continue
		}
		if _, affected := pool.fixed.auto.affected[accountID]; affected {
			chained++
		}
	}
	return chained
}

// fixedAutoHaltStaticLocked 静态熔断：detail 为原因说明（超过上限时为空），写持久 WARN 并推送账户列表；这个端口按原规则暂停
func (pool *proxyPool) fixedAutoHaltStaticLocked(port int, key, reason, detail string, count int, now time.Time) {
	auto := &pool.fixed.auto
	auto.haltLocked(key, reason, count, now)
	pool.fixed.notify = true
	if detail != "" {
		detail += " | "
	}
	pool.queueFixedLogLocked("service", "WARN", true, fmt.Sprintf(
		"IP 池坏 IP 过多，已停止自动换 IP，请手动处理 | %s30 分钟内坏掉=%d（上限 %d） | 端口=%d 按暂停处理 | 已自动换过的不撤回；处理后在 IP 池设置里点“恢复自动换 IP”",
		detail, count, auto.limit, port))
}

// fixedTargetsText 返回日志里挪到的端口（最多 proxyPoolFixedLogTargets 个，超出加“等”；没有时为“无”）
func fixedTargetsText(targets []int) string {
	parts := make([]string, 0, min(len(targets), proxyPoolFixedLogTargets))
	for index, target := range targets {
		if index >= proxyPoolFixedLogTargets {
			break
		}
		parts = append(parts, strconv.Itoa(target))
	}
	text := strings.Join(parts, "、")
	if len(targets) > proxyPoolFixedLogTargets {
		text += "等"
	}
	if text == "" {
		text = "无"
	}
	return text
}

// fixedAutoHoldLocked 账户绑定所用的租约还在已自动换 IP 停用的旧端口上（正在服务或正在启动的 Worker、登录、验证；被挪走的账户与固定端口本来就在
// 别处的过渡 Worker 都算）时返回暂停说明：账户暂停调度
// （不再往坏 IP 上派请求），它的 Worker 按不可调度释放（空闲即关、忙碌等请求结束）；租约归还时（releaseLocked 推送）绑定改为新端口、暂停解除。
// 只看 auto.replaced：手动“换 IP”与上限为 0 时永远为空串。纯计算（在账户池的回调里调用）
func (pool *proxyPool) fixedAutoHoldLocked(lease *proxyPoolLease, port int) string {
	if lease == nil || !lease.pooled || lease.port == 0 || lease.port == port || !pool.fixedExcludedLocked(lease.port) {
		return ""
	}
	if _, ok := pool.fixed.auto.replaced[lease.port]; !ok {
		return ""
	}
	return fmt.Sprintf("IP 池固定端口 %d 已自动换 IP，这个账户改用端口 %d；用旧端口的 Worker 关闭后恢复", lease.port, port)
}

// fixedAutoHeldLocked 判断账户已推送的绑定带自动换 IP 的暂停说明（releaseLocked 据此在暂停解除时推送账户列表）
func (pool *proxyPool) fixedAutoHeldLocked(accountID string) bool {
	if len(pool.fixed.auto.replaced) == 0 {
		return false
	}
	state := pool.accounts[accountID]
	return state != nil && state.binding != nil && strings.Contains(state.binding.Hold, "已自动换 IP")
}

// fixedAutoPausedLocked 返回暂停端口没有自动换的原因（状态接口 paused[].auto）：上限为 0 时为空；暂停原因是冷却、这个端口触发了熔断或熔断后又坏
// 时为 halted，当时没有可挪的端口时为 no_target；其余（上限为 0 时冷却的、重启读回的冷却、被其他进程占用）为空
func (pool *proxyPool) fixedAutoPausedLocked(port int, reason string) string {
	auto := &pool.fixed.auto
	if auto.limit <= 0 {
		return ""
	}
	if key := "p" + strconv.Itoa(port); auto.halted() && reason != fixedPauseLockedElsewhere {
		if _, after := auto.afterHalt[key]; after || key == auto.haltedKey {
			return fixedAutoHalted
		}
	}
	if _, ok := auto.noTarget[port]; ok {
		return fixedAutoNoTarget
	}
	return ""
}

// fixedAutoStatusLocked 返回状态接口的自动换字段（上限为 0 时为 nil）
func (pool *proxyPool) fixedAutoStatusLocked(now time.Time, dynamic bool) *api.AdminProxyPoolFixedAuto {
	auto := &pool.fixed.auto
	if auto.limit <= 0 {
		return nil
	}
	status := &api.AdminProxyPoolFixedAuto{
		Limit: auto.limit, WindowMinutes: int(proxyPoolFixedAutoWindow / time.Minute), Recent: auto.recent(now), Halted: auto.halted(),
	}
	if auto.halted() {
		status.HaltedAt, status.HaltedCount, status.Skipped = proxyPoolUTC(auto.haltedAt), auto.haltedCount, len(auto.afterHalt)
		status.HaltedReason = auto.haltedReasonText()
	}
	if !dynamic {
		for port, replaced := range auto.replaced {
			if !pool.fixedExcludedLocked(port) {
				continue
			}
			status.Replaced = append(status.Replaced, api.AdminProxyPoolAutoReplaced{
				Port: port, At: proxyPoolUTC(replaced.at), Rule: replaced.rule, Moved: replaced.moved,
			})
		}
		sort.Slice(status.Replaced, func(i, j int) bool {
			if !status.Replaced[i].At.Equal(status.Replaced[j].At) {
				return status.Replaced[i].At.After(status.Replaced[j].At)
			}
			return status.Replaced[i].Port < status.Replaced[j].Port
		})
		status.ReplacedTotal = len(status.Replaced)
		if len(status.Replaced) > proxyPoolFixedAutoListLimit {
			status.Replaced = status.Replaced[:proxyPoolFixedAutoListLimit]
		}
		return status
	}
	for accountID, stuck := range auto.stuck {
		status.Stuck = append(status.Stuck, api.AdminProxyPoolStuckSession{AccountID: accountID, Session: stuck.session, Since: proxyPoolUTC(stuck.since)})
	}
	sort.Slice(status.Stuck, func(i, j int) bool {
		if !status.Stuck[i].Since.Equal(status.Stuck[j].Since) {
			return status.Stuck[i].Since.Before(status.Stuck[j].Since)
		}
		return status.Stuck[i].AccountID < status.Stuck[j].AccountID
	})
	status.StuckTotal = len(status.Stuck)
	if len(status.Stuck) > proxyPoolFixedAutoListLimit {
		status.Stuck = status.Stuck[:proxyPoolFixedAutoListLimit]
	}
	return status
}

// fixedAutoStartupLogLocked 为启动时第一次生效（固定分配或固定会话开启、上限大于 0、读回的状态已熔断）的提示
func (pool *proxyPool) fixedAutoStartupLogLocked(dynamic bool) []proxyPoolLog {
	auto := &pool.fixed.auto
	if auto.limit <= 0 || !auto.halted() {
		return nil
	}
	at := auto.haltedAt.UTC().Format(time.RFC3339)
	message := fmt.Sprintf("IP 池自动换坏 IP 仍处于停止状态（%s 因坏 IP 过多停止），请在 IP 池设置里处理后点“恢复自动换 IP”", at)
	if dynamic {
		message = fmt.Sprintf("IP 池自动换坏会话仍处于停止状态（%s 因坏会话过多停止），请在 IP 池设置里处理后点“恢复自动换会话”", at)
	}
	return []proxyPoolLog{{source: "service", level: "WARN", durable: true, message: message}}
}

// fixedAutoForgetPortLocked 去掉端口的“没有可挪的端口”记录（端口恢复、空了或离开端口段时）
func (pool *proxyPool) fixedAutoForgetPortLocked(port int) {
	delete(pool.fixed.auto.noTarget, port)
}

// fixedAutoReconcileLocked 去掉端口段外的自动换记录（端口段变化时，与停用端口一起）
func (pool *proxyPool) fixedAutoReconcileLocked() bool {
	auto := &pool.fixed.auto
	changed := false
	for port := range auto.replaced {
		if !pool.inRangeLocked(port) {
			delete(auto.replaced, port)
			changed = true
		}
	}
	for port := range auto.noTarget {
		if !pool.inRangeLocked(port) {
			delete(auto.noTarget, port)
		}
	}
	return changed
}

// fixedAutoRestoreLocked 管理员恢复停用的端口时去掉它的自动换记录：还在这个端口上的旧 Worker 的账户解除自动换的暂停（之后与手动换 IP 的过渡
// Worker 相同）
func (pool *proxyPool) fixedAutoRestoreLocked(port int) {
	if _, ok := pool.fixed.auto.replaced[port]; !ok {
		return
	}
	delete(pool.fixed.auto.replaced, port)
	pool.fixedMarkDirtyLocked()
	pool.pushLocked(pool.portHolderAccountsLocked(port)...)
	pool.fixed.notify = true
}

// ---- 动态 ----

// fixedSessionDecideLocked 账户达到换会话条件时（sessionStrikeLocked，已过 30 分钟 2 次与 10 分钟间隔）的固定会话处理：返回要写的日志、是否不换
// （keep）与换会话日志的原因。不是动态固定会话时照常换；上限为 0 时与没有这项设置时相同（不换，同一账户 30 分钟 WARN 一次）；否则记进窗口，
// 已熔断、这一个让窗口内超过上限、这个账户窗口内自动换出的新会话又连不通（连锁）、或窗口内会话连不通的账户超过走 IP 池的账户的一半时不换
// （后三种熔断），其余自动换会话
func (pool *proxyPool) fixedSessionDecideLocked(lease *proxyPoolLease, strikes int, now time.Time) ([]proxyPoolLog, bool, string) {
	reason := fmt.Sprintf("连续 %d 次出口连不通", proxyPoolUpstreamFails)
	if !pool.fixedDynamicLocked() {
		return nil, false, reason
	}
	auto := &pool.fixed.auto
	accountID := lease.accountID
	if stuck, ok := auto.stuck[accountID]; !ok || stuck.session != lease.slot {
		auto.stuck[accountID] = proxyPoolFixedStuck{session: lease.slot, since: now}
	}
	if auto.limit <= 0 {
		logs, _ := pool.fixedSessionKeepLocked(lease, strikes, now)
		return logs, true, ""
	}
	// 按会话计数（同一会话只算一个）；换出的新会话再坏另算一个，并按下面的连锁熔断（按账户去重的话，账户数不超过上限时永远不会熔断，每个账户
	// 每 10 分钟换一次会话）
	session := fixedSessionKey(accountID, lease.slot)
	count := pool.fixedAutoNoteLocked(0, session, now)
	key := "s" + session
	switch {
	case auto.halted():
		auto.skippedLocked(key)
		return pool.fixedSessionHaltedWarnLocked(lease, strikes, now), true, ""
	case count > auto.limit:
		return pool.fixedSessionHaltLocked(lease, strikes, key, fixedHaltLimit, "", count, now), true, ""
	case auto.sessionChained(accountID, session):
		// 连锁：上限 10 时一个账户 30 分钟内最多 3 个坏会话（10 分钟最短间隔），只按个数的话同一账户反复连不通永远不熔断
		return pool.fixedSessionHaltLocked(lease, strikes, key, fixedHaltChain,
			"换出的新会话又连不通：这个账户 30 分钟内已自动换过会话（像是代理商整体出了问题）", count, now), true, ""
	}
	if base := pool.fixedAutoPooledLocked(); base > 1 {
		if affected := auto.sessionAccounts(); 2*affected > base {
			return pool.fixedSessionHaltLocked(lease, strikes, key, fixedHaltAccounts, fmt.Sprintf(
				"连不通的账户超过一半：30 分钟内 %d/%d 个账户的会话连不通（像是代理商整体出了问题）", affected, base), count, now), true, ""
		}
	}
	delete(auto.stuck, accountID)
	return nil, false, fmt.Sprintf("%s，固定会话自动换会话（30 分钟内第 %d 个，上限 %d）", reason, count, auto.limit)
}

// fixedSessionHaltLocked 动态熔断：detail 为原因说明（超过上限时为空），写持久 WARN 并推送账户列表，返回这个账户不换会话的 WARN
func (pool *proxyPool) fixedSessionHaltLocked(lease *proxyPoolLease, strikes int, key, reason, detail string, count int, now time.Time) []proxyPoolLog {
	auto := &pool.fixed.auto
	auto.haltLocked(key, reason, count, now)
	pool.fixed.notify = true
	if detail != "" {
		detail += " | "
	}
	pool.queueFixedLogLocked("service", "WARN", true, fmt.Sprintf(
		"IP 池坏会话过多，已停止自动换会话，请手动处理 | %s30 分钟内连不通的会话=%d（上限 %d） | 已自动换过的不撤回；可在 IP 池设置里给账户“换会话”，处理后点“恢复自动换会话”",
		detail, count, auto.limit))
	return pool.fixedSessionHaltedWarnLocked(lease, strikes, now)
}

// fixedSessionHaltedWarnLocked 为熔断后不换会话的 WARN（同一账户 30 分钟一次，与固定会话不换会话共用去重记录）
func (pool *proxyPool) fixedSessionHaltedWarnLocked(lease *proxyPoolLease, strikes int, now time.Time) []proxyPoolLog {
	if last, ok := pool.fixed.sessionWarned[lease.accountID]; ok && now.Sub(last) < proxyPoolFixedSessionWarnWindow {
		return nil
	}
	pool.fixed.sessionWarned[lease.accountID] = now
	return []proxyPoolLog{{source: lease.logSource(), level: "WARN", message: fmt.Sprintf(
		"IP 池固定会话不换会话 | 会话=%s | 原因=连续 %d 次出口连不通 | 已停止自动换会话（坏会话过多），可在 IP 池设置里给这个账户换会话",
		lease.slot, strikes)}}
}

// fixedSessionAttachedLocked Worker 在会话上启动成功（attach 清零会话的连通失败计数时）：会话通了，去掉这个账户的“会话连不通”记录
func (pool *proxyPool) fixedSessionAttachedLocked(lease *proxyPoolLease) {
	if stuck, ok := pool.fixed.auto.stuck[lease.accountID]; ok && stuck.session == lease.slot {
		delete(pool.fixed.auto.stuck, lease.accountID)
	}
}

// ---- 管理操作 ----

// errProxyPoolNotFixedAny 等为自动换管理操作的错误（管理接口的中文说明）
func errProxyPoolNotFixedAny() error {
	return &adminOperationError{status: http.StatusConflict, code: "proxy_pool_not_fixed",
		message: "固定分配或固定会话没有开启（PROXY_POOL_FIXED=true）"}
}

func errProxyPoolNotFixedDynamic() error {
	return &adminOperationError{status: http.StatusConflict, code: "proxy_pool_not_fixed_dynamic",
		message: "固定会话没有开启（需要 PROXY_POOL_MODE=dynamic 且 PROXY_POOL_FIXED=true）"}
}

func errProxyPoolAccountNotPooled() error {
	return &adminOperationError{status: http.StatusConflict, code: "proxy_pool_account_not_pooled", message: "这个账户有自己的代理，不用 IP 池"}
}

func errProxyPoolAccountUnknown() error {
	return &adminOperationError{status: http.StatusNotFound, code: "account_not_found", message: "账户不存在"}
}

// fixedAutoResume 管理页“恢复自动换 IP（会话）”：清除熔断，并清空 30 分钟计数与连锁、按账户的记录（否则窗口里仍超过上限，下一个坏的立刻再次
// 熔断）；此刻因熔断或没有
// 可挪端口而暂停的端口不补做自动换（冷却到期后恢复使用，再坏时按新的计数），此刻会话连不通的账户不立刻换（它的 Worker 再启动失败、达到换会话
// 条件时按新的计数自动换）；停用的端口不恢复（停用与冷却中的端口仍占一半以上时，下一个坏的按相对门槛再次熔断）。没有熔断时什么也不做（resumed
// 为假）。不计数；立即写盘
func (pool *proxyPool) fixedAutoResume() (api.ProxyPoolFixedAutoResumeResult, error) {
	pool.mu.Lock()
	static, dynamic := pool.fixedStaticLocked(), pool.fixedDynamicLocked()
	if !static && !dynamic {
		pool.mu.Unlock()
		return api.ProxyPoolFixedAutoResumeResult{}, errProxyPoolNotFixedAny()
	}
	auto := &pool.fixed.auto
	var result api.ProxyPoolFixedAutoResumeResult
	if auto.halted() {
		result = api.ProxyPoolFixedAutoResumeResult{Resumed: true, HaltedAt: proxyPoolUTC(auto.haltedAt), Skipped: len(auto.afterHalt)}
		auto.haltedAt, auto.haltedCount, auto.haltedReason, auto.haltedKey = time.Time{}, 0, "", ""
		clear(auto.afterHalt)
		clear(auto.ports)
		clear(auto.sessions)
		clear(auto.affected)
		pool.fixedMarkDirtyLocked()
		pool.fixed.notify = true
		message := fmt.Sprintf("IP 池已恢复自动换坏 IP（管理页） | 停止于=%s | 停止后又坏=%d（仍按暂停处理，需要时手动换 IP） | 30 分钟计数从 0 开始",
			result.HaltedAt.Format(time.RFC3339), result.Skipped)
		if dynamic {
			message = fmt.Sprintf("IP 池已恢复自动换坏会话（管理页） | 停止于=%s | 停止后又坏=%d（需要时手动换会话） | 30 分钟计数从 0 开始",
				result.HaltedAt.Format(time.RFC3339), result.Skipped)
		}
		pool.queueFixedLogLocked("service", "INFO", true, message)
	}
	logs := pool.takeFixedLogsLocked()
	pool.mu.Unlock()
	pool.emit(logs)
	if result.Resumed {
		pool.fixedFlush()
	}
	return result, nil
}

// fixedRotateSession 动态固定会话下给账户手动换会话（代号 +1，持久化；没有活动租约时绑定立即换成新会话）：不计数、不熔断。返回换前与换后的会话号
func (pool *proxyPool) fixedRotateSession(accountID string) (api.ProxyPoolFixedRotateResult, error) {
	pool.mu.Lock()
	if !pool.fixedDynamicLocked() {
		pool.mu.Unlock()
		return api.ProxyPoolFixedRotateResult{}, errProxyPoolNotFixedDynamic()
	}
	state := pool.accounts[accountID]
	if accountID == "" || state == nil || !state.known {
		pool.mu.Unlock()
		return api.ProxyPoolFixedRotateResult{}, errProxyPoolAccountUnknown()
	}
	if state.proxy != "" {
		pool.mu.Unlock()
		return api.ProxyPoolFixedRotateResult{}, errProxyPoolAccountNotPooled()
	}
	now := pool.now()
	previous := pool.sessionLocked(accountID)
	pool.rotateLocked(accountID, now)
	pool.pushLocked(accountID)
	delete(pool.fixed.auto.stuck, accountID)
	pool.fixed.notify = true
	result := api.ProxyPoolFixedRotateResult{AccountID: accountID, From: previous, To: pool.sessionLocked(accountID)}
	logs := pool.takeFixedLogsLocked()
	pool.mu.Unlock()
	pool.emit(logs)
	return result, nil
}

// servesSession 判断账户现在正用会话 session 服务：绑定所用的租约是 IP 池分配、已启动的 Worker，在这个会话上（动态，端口为 0）。手动换会话后
// 登记关闭旧 Worker 之前核对。方法对 nil 安全
func (pool *proxyPool) servesSession(accountID, session string) bool {
	if pool == nil || accountID == "" || session == "" {
		return false
	}
	pool.mu.Lock()
	defer pool.mu.Unlock()
	lease := pool.bindingLeaseLocked(accountID)
	return lease != nil && lease.attached && lease.kind == proxyPoolKindWorker && lease.pooled && lease.port == 0 && lease.slot == session
}

// ---- 持久化 ----

// proxyPoolFixedAutoRecord 为固定表文件里的自动换状态（只有端口号、账户 ID、时间、规则代码与计数）
// （Sessions 的键为 fixedSessionKey，Affected 的键为账户 ID；HaltedReason、HaltedKey、AfterHalt 只在熔断时有，AfterHalt 升序）
type proxyPoolFixedAutoRecord struct {
	Ports        map[string]time.Time                    `json:"ports,omitempty"`
	Sessions     map[string]time.Time                    `json:"sessions,omitempty"`
	Affected     map[string]time.Time                    `json:"affected,omitempty"`
	HaltedAt     time.Time                               `json:"halted_at,omitzero"`
	HaltedCount  int                                     `json:"halted_count,omitempty"`
	HaltedReason string                                  `json:"halted_reason,omitempty"`
	HaltedKey    string                                  `json:"halted_key,omitempty"`
	AfterHalt    []string                                `json:"after_halt,omitempty"`
	Replaced     map[string]proxyPoolFixedReplacedRecord `json:"replaced,omitempty"`
}

type proxyPoolFixedReplacedRecord struct {
	At    time.Time `json:"at"`
	Rule  string    `json:"rule,omitempty"`
	Moved int       `json:"moved"`
}

// fixedAutoSnapshotLocked 复制要写盘的自动换状态（没有时为 nil，文件里不出现这一项）：窗口记录只写窗口内的
func (pool *proxyPool) fixedAutoSnapshotLocked(now time.Time) *proxyPoolFixedAutoRecord {
	auto := &pool.fixed.auto
	auto.prune(now)
	if !auto.persistent() {
		return nil
	}
	record := &proxyPoolFixedAutoRecord{HaltedAt: proxyPoolUTC(auto.haltedAt), HaltedCount: auto.haltedCount}
	if auto.halted() {
		record.HaltedReason, record.HaltedKey = auto.haltedReasonText(), auto.haltedKey
		for key := range auto.afterHalt {
			record.AfterHalt = append(record.AfterHalt, key)
		}
		sort.Strings(record.AfterHalt)
	}
	if len(auto.ports) > 0 {
		record.Ports = make(map[string]time.Time, len(auto.ports))
		for port, at := range auto.ports {
			record.Ports[strconv.Itoa(port)] = proxyPoolUTC(at)
		}
	}
	if len(auto.sessions) > 0 {
		record.Sessions = make(map[string]time.Time, len(auto.sessions))
		for session, at := range auto.sessions {
			record.Sessions[session] = proxyPoolUTC(at)
		}
	}
	if len(auto.affected) > 0 {
		record.Affected = make(map[string]time.Time, len(auto.affected))
		for accountID, at := range auto.affected {
			record.Affected[accountID] = proxyPoolUTC(at)
		}
	}
	if len(auto.replaced) > 0 {
		record.Replaced = make(map[string]proxyPoolFixedReplacedRecord, len(auto.replaced))
		for port, replaced := range auto.replaced {
			record.Replaced[strconv.Itoa(port)] = proxyPoolFixedReplacedRecord{At: proxyPoolUTC(replaced.at), Rule: replaced.rule, Moved: replaced.moved}
		}
	}
	return record
}

// fixedAutoLoad 载入固定表文件里的自动换状态（loadFixed 读完停用端口之后调用）：窗口记录按此刻剪一次，自动换过的端口只保留仍停用的
func (pool *proxyPool) fixedAutoLoad(record *proxyPoolFixedAutoRecord) {
	if record == nil {
		return
	}
	auto := &pool.fixed.auto
	for key, at := range record.Ports {
		if port, err := strconv.Atoi(key); err == nil && port >= 1 && port <= 65535 {
			auto.ports[port] = at
		}
	}
	for session, at := range record.Sessions {
		if session = strings.TrimSpace(session); session != "" {
			auto.sessions[session] = at
		}
	}
	for accountID, at := range record.Affected {
		if accountID = strings.TrimSpace(accountID); accountID != "" {
			auto.affected[accountID] = at
		}
	}
	auto.haltedAt, auto.haltedCount = record.HaltedAt, record.HaltedCount
	if auto.halted() {
		auto.haltedReason, auto.haltedKey = record.HaltedReason, record.HaltedKey
		for _, key := range record.AfterHalt {
			if key = strings.TrimSpace(key); key != "" && key != auto.haltedKey {
				auto.afterHalt[key] = struct{}{}
			}
		}
	}
	for key, replaced := range record.Replaced {
		port, err := strconv.Atoi(key)
		if err != nil || !pool.fixedExcludedLocked(port) {
			continue
		}
		auto.replaced[port] = proxyPoolFixedReplaced{at: replaced.At, rule: replaced.Rule, moved: replaced.Moved}
	}
	auto.prune(pool.now())
}
