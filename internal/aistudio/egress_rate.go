package aistudio

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

// 出口为账户发往上游的网络出口：账户固定代理或全局代理（Account.EffectiveProxy）规范化后的值，空为直连。
// 同一出口上的账户共用上游按出口 IP 计的限流；判定与行为在 app 层（egress_guard.go），账户池只按出口与分区通道记账、准入。
//
// 每出口、每分区通道一条令牌桶（GCRA）：
//   - 账户按出口键与是否属于 Ultra 号池挂到一个通道（attachEgressLocked）：构造、加入、SetGlobalProxy、SaveConfig 改代理、
//     Ultra 成员变化时重挂，删除时摘下；候选索引按账户下标存通道编号（egressID）
//   - 限速只由 app 层判定器经 SetEgressRateLimit 写入，为 0 时只记账不拦：生成租约照常预占、发送照常记录，供状态与判定使用
//   - 取得生成租约时预占并立即消耗一个令牌（tat 前移一个间隔），发出时抵掉预占并记一次发送，没有发出就释放时退还令牌。
//     没有沿用 ACCOUNT_RPM_LIMIT 的 60 秒滑动日志：判定成立时窗口里已有 90–168 条发送，初值 22–31 要等窗口里只剩 L−1 条才放行，
//     停顿约 44–50 秒，之后旧发送成簇过期、一阵一阵放行；令牌桶从空桶开始，没有停顿，之后均匀放行。
//     预占在准入时就消耗令牌，文件复制、内联附件上传、WAA proof 慢的请求只是提前用掉自己的令牌，不会堵住整个出口
//   - 主用模型（AccountSelection.EgressPriority）优先：非主用要多等一个令牌；该通道最近一个窗口里已有发送、非主用占比却低于
//     egressNonPrimaryMinShare 时反过来（保底），主用让出一个令牌。任一类单独有需求时都不损失速率，只少一个突发容量
//   - 通道对某类选择不准入时，该通道上的账户分类时按忙碌处理（计 EgressLimited），选号跳过；单出口满桶时不逐个候选尝试（短路）。
//     下一个令牌的时刻登记到账户池唯一的窗口释放计时器（与每账号每分钟限速共用），到时 notifyLocked 唤醒各队首
//   - 指定账户的选号（管理页测试调用）不拦但照常预占；文件绑定的选号照拦（所属账户要冷启动而被暂停时由 app 层改走复制文件）
//   - 非生成 RPC（ListModels、CountTokens 等经 Client.do 发出、ctx 带租约的）只计数，不准入、不拦
//
// 只统计本进程，记录只在内存中。全部状态受账户池 mu 保护
const (
	// egressRateWindow 为发送计数与非主用占比的统计窗口，也是限速的单位（每分钟）
	egressRateWindow = time.Minute
	// egressRateMaxSent 为每条通道最多保留的发送记录（不限速期间发送也照记，防止无限增长）
	egressRateMaxSent = 4096
	// egressMinBurst 与 egressBurstShare 决定令牌桶容量：max(egressMinBurst, ceil(限速 × egressBurstShare))，限速 25 时为 3，
	// 既能吸收到达抖动，又不会一次放出一批
	egressMinBurst   = 2
	egressBurstShare = 0.1
	// egressNonPrimaryMinShare 为非主用模型的保底：非主用发送占比低于它时非主用优先（pro 需求约为出口能力的 25%，见方案 §9）
	egressNonPrimaryMinShare = 0.25
	// egressWaitRetryMin 与 egressWaitRetryMax 为转写按“出口限速”排队超限时建议重试时间的范围
	egressWaitRetryMin = 5 * time.Second
	egressWaitRetryMax = 30 * time.Second
	// egressNoLane 为没有挂到出口通道的账户（已移出号池）在候选索引中的通道编号：门控按准入处理
	egressNoLane = math.MaxUint32
	// rateLadderPrefix 为逐级限流冷却原因的开头（见 rate_strike.go 的 MarkRateLimitCooldown），其后为档位与“ 档，”
	rateLadderPrefix = RateLimitKind + ": 连续限流第 "
)

// EgressBusyReason 为 CapacityBusyError 的出口限速原因：请求只因所在出口通道的令牌桶已满而排队超过上限
const EgressBusyReason = "出口限速"

// EgressColdStartReason 为 CapacityBusyError 的出口冷启动暂停原因：账户所在出口正在限流，要冷启动的 Worker 被冷启动闸门暂停
// （转写、Live、Veo 经 Worker() 的冷启动，指定账户的生成请求），对外 429 的原因响应头同样按出口限流（egress_limited）
const EgressColdStartReason = "出口限流中，冷启动按配额进行"

// egressDirectLabel 为直连出口（没有代理）的显示
const egressDirectLabel = "直连"

// egressUnknownLabel 为代理无法解析时的显示前缀：原文可能带凭据，只追加原文哈希
const egressUnknownLabel = "<无法识别的代理>"

// EgressKey 返回代理的出口键：空为直连；去掉密码、保留用户名（很多代理按用户名选出口），scheme 与 host 小写，
// socks5h 归为 socks5，没有 scheme 的 host:port 补 http://；解析失败时取去空白的原文。并行代理改动合入后先调 config.CanonicalProxy。
// 键必须基于用户配置的原始代理（Account.EffectiveProxy），不能用本地中转地址：中转地址会让全部账户并成一个键，或每次重启换键
func EgressKey(proxy string) string {
	raw := strings.TrimSpace(proxy)
	if raw == "" {
		return ""
	}
	parsed, ok := parseEgressProxy(raw)
	if !ok {
		return raw
	}
	key := parsed.Scheme + "://"
	if username := parsed.User.Username(); username != "" {
		key += url.User(username).String() + "@"
	}
	return key + parsed.Host
}

// EgressLabel 返回出口的脱敏显示：直连、http://host:port，带用户名时追加 #用户名 sha256 前 4 位（如 socks5://gw.example:1080#a3f9）；
// 解析失败时为 <无法识别的代理>#原文 sha256 前 4 位。从不显示用户名与密码原文。
// 并行代理改动合入后 scheme://host:port 部分改用 config.RedactProxy(key)，#用户名哈希的后缀仍要保留：RedactProxy 不带用户名，
// 同一网关、不同用户名（不同会话）的出口会显示成同一个标签，侧栏分不清，诊断第 4b 段按日志里的“出口=”标签配对与汇总也会串到一起
func EgressLabel(key string) string {
	key = strings.TrimSpace(key)
	if key == "" {
		return egressDirectLabel
	}
	parsed, ok := parseEgressProxy(key)
	if !ok {
		return egressUnknownLabel + "#" + egressHash(key, 4)
	}
	label := parsed.Scheme + "://" + parsed.Host
	if username := parsed.User.Username(); username != "" {
		label += "#" + egressHash(username, 4)
	}
	return label
}

// EgressID 返回出口的不透明编号（键 sha256 前 12 位 hex），供状态接口与管理动作引用，不含代理原文
func EgressID(key string) string {
	return egressHash(key, 12)
}

// parseEgressProxy 按出口分组的口径解析代理：没有 scheme 时补 http://，scheme 与 host 小写，socks5h 归为 socks5
// （同一出口 IP，DNS 在哪一端解析不影响分组）；没有主机名时视为无法解析
func parseEgressProxy(raw string) (*url.URL, bool) {
	value := raw
	if !strings.Contains(value, "://") {
		value = "http://" + value
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Hostname() == "" {
		return nil, false
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	if parsed.Scheme == "socks5h" {
		parsed.Scheme = "socks5"
	}
	parsed.Host = strings.ToLower(parsed.Host)
	return parsed, true
}

// egressHash 返回 value 的 sha256 hex 前 n 位
func egressHash(value string, n int) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])[:n]
}

// EgressLane 为出口内的分区通道：Ultra 分区的账户单独一条令牌桶，普通号池的流量不会限住 Ultra 号池
type EgressLane uint8

const (
	EgressLaneNormal EgressLane = iota
	EgressLaneUltra
)

// EgressLaneStatus 为一个出口通道的账户池侧计数（状态接口与判定器使用）
type EgressLaneStatus struct {
	Lane          EgressLane
	Accounts      int       // 挂在该通道上的账户数
	Limit         int       // 每分钟生成请求上限，0 为不限
	Burst         int       // 令牌桶容量（Limit>0 时）
	Sent1m        int       // 最近 60 秒发往上游的生成请求数（不论是否限速都记）
	Sent30s       int       // 最近 30 秒发往上游的生成请求数
	PrimarySent1m int       // 其中主用模型的
	OtherSent1m   int       // 最近 60 秒经租约发出的非生成 RPC（ListModels、CountTokens 等，只计数不限速）
	Pending       int       // 已取得生成租约、还没发出的预占数
	NextAt        time.Time // 主用模型下一个令牌的时刻，零值为现在就有
}

// EgressStatus 为一个出口的账户池侧计数
type EgressStatus struct {
	Key, ID, Label string
	Lanes          [2]EgressLaneStatus
}

// egressRecord 为一个出口：键、编号、脱敏标签与两条通道。建立后不删除（出口数等于见过的不同代理数）
type egressRecord struct {
	key, id, label string
	lanes          [2]*egressRate
}

// egressRate 为一个出口通道的令牌桶与发送记录；受账户池 mu 保护
type egressRate struct {
	record *egressRecord // 所属出口（key、label、id），建立后不变
	lane   EgressLane
	// id 为通道编号（egressLanes 下标 = 出口序号×2 + 通道），候选索引按它查门控结果
	id       uint32
	accounts int
	// limit 为每分钟上限，0 为不限（只由 SetEgressRateLimit 写）；interval 为两个令牌的间隔（窗口/limit，纳秒），
	// burst 为令牌桶容量，tat 为 GCRA 的理论到达时刻：tat ≤ now 为桶满，每消耗一个令牌前移一个间隔
	limit    int
	interval int64
	burst    int
	tat      int64
	pending  int
	// sent 为最近一个窗口的生成发送（不论是否限速都记，供状态与主用占比），other 为非生成 RPC（只计数）
	sent  egressSentLog
	other egressSentLog
}

// egressSent 为一条发送记录
type egressSent struct {
	at      int64
	primary bool
}

// egressSentLog 为按时间升序的发送记录队列：entries[start:] 为窗口内的记录，primary 为其中主用模型的条数。
// 记录在追加与读取时按窗口修剪，摊销 O(1)
type egressSentLog struct {
	entries []egressSent
	start   int
	primary int
}

// live 返回窗口内（上次修剪后）的记录数
func (log *egressSentLog) live() int {
	return len(log.entries) - log.start
}

// prune 移出不晚于 cutoff 的记录；移出过半时整理底层数组
func (log *egressSentLog) prune(cutoff int64) {
	for log.start < len(log.entries) && log.entries[log.start].at <= cutoff {
		if log.entries[log.start].primary {
			log.primary--
		}
		log.start++
	}
	switch {
	case log.start == len(log.entries):
		log.entries, log.start = log.entries[:0], 0
	case log.start >= 64 && log.start*2 >= len(log.entries):
		log.entries = log.entries[:copy(log.entries, log.entries[log.start:])]
		log.start = 0
	}
}

// add 追加一条记录（时刻不早于上一条，墙钟回拨时最多多算），超出 egressRateMaxSent 时丢掉最旧的
func (log *egressSentLog) add(at int64, primary bool, cutoff int64) {
	log.prune(cutoff)
	if n := len(log.entries); n > log.start && log.entries[n-1].at > at {
		at = log.entries[n-1].at
	}
	log.entries = append(log.entries, egressSent{at: at, primary: primary})
	if primary {
		log.primary++
	}
	if log.live() > egressRateMaxSent {
		if log.entries[log.start].primary {
			log.primary--
		}
		log.start++
	}
}

// countAfter 返回晚于 cutoff 的记录数（已修剪过的窗口内二分）
func (log *egressSentLog) countAfter(cutoff int64) int {
	entries := log.entries[log.start:]
	return len(entries) - sort.Search(len(entries), func(i int) bool { return entries[i].at > cutoff })
}

// egressGate 为一次分类或选号内不变的出口准入判断：enabled 时 blocked 为对这次选择（主用或非主用）不准入的限速通道编号（升序）。
// 不限速的通道总是准入，不逐个计算：开销按限速通道数（通常一两个）而不是全部出口通道数（出口记录建立后不删，通道数是出口数的两倍）。
// blockedPopulated 为其中有账户的通道数，blockedNext 为它们中最早的准入时刻（全部通道满桶短路用）
type egressGate struct {
	enabled          bool
	primary          bool
	now              int64
	blocked          []uint32
	blockedPopulated int
	blockedNext      int64
}

// admits 判断账户所在通道对这次选择准入；门控关闭或账户没有通道时准入
func (g egressGate) admits(rate *egressRate) bool {
	if !g.enabled || rate == nil {
		return true
	}
	return g.admitsID(rate.id)
}

// admitsID 按通道编号判断准入（候选索引路径）
func (g egressGate) admitsID(id uint32) bool {
	if !g.enabled || len(g.blocked) == 0 {
		return true
	}
	_, blocked := slices.BinarySearch(g.blocked, id)
	return !blocked
}

// egressAttempt 为一次选号内的出口准入：gate 为门控，blocked 记下有候选只因出口令牌桶已满被跳过（或全部通道满桶短路），
// next 为其中最早的准入时刻（UnixNano）。AcquireFor 按它累计 WithEgressWaitLimit 的等待
type egressAttempt struct {
	gate    egressGate
	blocked bool
	next    int64
}

// refuse 记下一个只因出口令牌桶已满被跳过的候选
func (attempt *egressAttempt) refuse(at int64) {
	attempt.blocked = true
	if attempt.next == 0 || at < attempt.next {
		attempt.next = at
	}
}

// egressWindowLocked 返回统计窗口：egressWindow 只给测试改小，为 0 时取 egressRateWindow；调用方持有 p.mu
func (p *AccountPool) egressWindowLocked() int64 {
	if p.egressWindow > 0 {
		return int64(p.egressWindow)
	}
	return int64(egressRateWindow)
}

// egressLaneLocked 返回出口键与通道对应的令牌桶，没有时建立出口记录（两条通道一起建，编号连续）；调用方持有 p.mu
func (p *AccountPool) egressLaneLocked(key string, lane EgressLane) *egressRate {
	record := p.egresses[key]
	if record == nil {
		if p.egresses == nil {
			p.egresses = make(map[string]*egressRecord)
		}
		record = &egressRecord{key: key, id: EgressID(key), label: EgressLabel(key)}
		for index := range record.lanes {
			rate := &egressRate{record: record, lane: EgressLane(index), id: uint32(len(p.egressLanes))}
			record.lanes[index] = rate
			p.egressLanes = append(p.egressLanes, rate)
		}
		p.egresses[key] = record
	}
	return record.lanes[lane]
}

// egressLaneID 返回账户所在通道的编号，没有通道时为 egressNoLane
func egressLaneID(account *Account) uint32 {
	if account == nil || account.egress == nil {
		return egressNoLane
	}
	return account.egress.id
}

// attachEgressLocked 按账户当前的出口键（固定代理或全局代理）与 Ultra 成员关系把它挂到对应通道，返回是否换了通道：
// 先从原通道摘下（账户数减一），同步候选索引里的通道编号。账户已不在号池中时只摘下。进行中租约的预占记在租约自己的通道上，
// 释放时退还原通道（见 releaseEgressLocked），重挂不搬动预占。调用方持有 p.mu
func (p *AccountPool) attachEgressLocked(account *Account) bool {
	if account == nil {
		return false
	}
	var next *egressRate
	if p.byID[account.ID] == account {
		lane := EgressLaneNormal
		if _, ultra := p.ultraAccounts[account.ID]; ultra {
			lane = EgressLaneUltra
		}
		next = p.egressLaneLocked(EgressKey(account.EffectiveProxy(p.globalProxy)), lane)
	}
	if account.egress == next {
		return false
	}
	if p.ownsEgressLocked(account.egress) {
		p.leaveEgressLaneLocked(account.egress)
	}
	if next != nil {
		next.accounts++
		if next.accounts == 1 {
			p.egressPopulated++
		}
	}
	account.egress = next
	if p.sched != nil && p.sched.positionValid(p, account) {
		p.sched.egressID[account.position] = egressLaneID(account)
	}
	return true
}

// detachEgressLocked 把移出号池的账户从所在通道摘下；调用方持有 p.mu，账户已从 byID 删除
func (p *AccountPool) detachEgressLocked(account *Account) {
	if account == nil {
		return
	}
	if p.ownsEgressLocked(account.egress) {
		p.leaveEgressLaneLocked(account.egress)
	}
	account.egress = nil
}

// leaveEgressLaneLocked 把一个账户从通道上摘下：账户数减一，减到 0 时有账户的通道数减一。调用方持有 p.mu，通道属于这个账户池
func (p *AccountPool) leaveEgressLaneLocked(rate *egressRate) {
	rate.accounts--
	if rate.accounts == 0 {
		p.egressPopulated--
	}
}

// ownsEgressLocked 判断通道属于这个账户池：同一个账户对象被放进新建的账户池时（测试里会出现），原来的通道属于旧号池，
// 不能在新号池的锁下改它的计数。调用方持有 p.mu
func (p *AccountPool) ownsEgressLocked(rate *egressRate) bool {
	return rate != nil && p.egresses[rate.record.key] == rate.record
}

// reattachEgressAllLocked 重挂全部账户（全局代理或号池划分规则变化后），返回是否有账户换了通道；调用方持有 p.mu
func (p *AccountPool) reattachEgressAllLocked() bool {
	changed := false
	for _, account := range p.accounts {
		if p.attachEgressLocked(account) {
			changed = true
		}
	}
	return changed
}

// egressNonPrimaryStarvedLocked 判断通道最近一个窗口里已有生成发送、其中非主用占比却低于 egressNonPrimaryMinShare；调用方持有 p.mu
func (p *AccountPool) egressNonPrimaryStarvedLocked(rate *egressRate, now int64) bool {
	rate.sent.prune(now - p.egressWindowLocked())
	total := rate.sent.live()
	if total == 0 {
		return false
	}
	return float64(total-rate.sent.primary) < egressNonPrimaryMinShare*float64(total)
}

// egressAdmitLocked 判断通道当前能否给这类选择一个令牌（GCRA）：τ = (burst−1)×interval，优先一方的准入条件为 tat ≤ now+τ，
// 另一方为 tat ≤ now+τ−interval，即给优先一方留一个令牌。优先一方是主用模型；非主用占比低于保底时（见 egressNonPrimaryStarvedLocked）
// 是非主用：只把非主用提到主用的条件时，两类同时等待就要看谁先复查，主用一直在排队时非主用可能一个令牌也拿不到。
// 只有一方有需求时它照样按 1/interval 取得令牌，只少一个突发容量。不准入时返回条件成立的时刻（UnixNano）。
// 限速为 0 时总是准入。调用方持有 p.mu
func (p *AccountPool) egressAdmitLocked(rate *egressRate, primary bool, now int64) (bool, int64) {
	if rate == nil || rate.limit <= 0 {
		return true, 0
	}
	offset := int64(rate.burst-1) * rate.interval
	if primary == p.egressNonPrimaryStarvedLocked(rate, now) {
		offset -= rate.interval
	}
	if rate.tat <= now+offset {
		return true, 0
	}
	return false, rate.tat - offset
}

// egressGateLocked 返回选择的出口准入判断：有通道限速大于 0、是生成选择、且不是指定账户时打开，一次按 EgressPriority 算好每个通道。
// 指定账户的选号（管理页测试调用）不拦但照常预占，便于人工探测；文件绑定的选号照拦。调用方持有 p.mu
func (p *AccountPool) egressGateLocked(selection AccountSelection, now time.Time) egressGate {
	if len(p.egressThrottled) == 0 || !generationSelection(selection) || strings.TrimSpace(selection.AccountID) != "" {
		return egressGate{}
	}
	gate := egressGate{enabled: true, primary: selection.EgressPriority, now: now.UnixNano()}
	for _, rate := range p.egressThrottled {
		ok, at := p.egressAdmitLocked(rate, gate.primary, gate.now)
		if ok {
			continue
		}
		gate.blocked = append(gate.blocked, rate.id)
		if rate.accounts > 0 {
			if gate.blockedPopulated == 0 || at < gate.blockedNext {
				gate.blockedNext = at
			}
			gate.blockedPopulated++
		}
	}
	slices.Sort(gate.blocked)
	return gate
}

// egressAllBlockedLocked 判断门控打开时每个有账户的通道都不准入（没有不限速或还有令牌的通道），返回其中最早的准入时刻：
// 这时选号不逐个候选尝试（单出口满桶时原本要在 p.mu 内把上万候选逐个拒绝）。调用方持有 p.mu
func (p *AccountPool) egressAllBlockedLocked(gate egressGate) (int64, bool) {
	// 不限速的通道总是准入：有账户的通道全部是不准入的限速通道时才算全部满桶
	if !gate.enabled || p.egressPopulated == 0 || gate.blockedPopulated < p.egressPopulated {
		return 0, false
	}
	return gate.blockedNext, true
}

// egressWakeAtLocked 返回通道准入状态下一次变化的时刻：主用或非主用当前不准入时为两者中较早的准入时刻，都准入或不限速时为 0。
// 到时先唤醒较早的一类，计时器回调重算后再为另一类登记一次（每个令牌最多两次唤醒）。调用方持有 p.mu
func (p *AccountPool) egressWakeAtLocked(rate *egressRate, now int64) int64 {
	if rate.limit <= 0 {
		return 0
	}
	primaryOK, primaryAt := p.egressAdmitLocked(rate, true, now)
	otherOK, otherAt := p.egressAdmitLocked(rate, false, now)
	switch {
	case !primaryOK && !otherOK:
		return min(primaryAt, otherAt)
	case !primaryOK:
		return primaryAt
	case !otherOK:
		return otherAt
	default:
		return 0
	}
}

// egressArmLocked 把通道下一次准入状态变化的时刻登记到 egressFull 并提前窗口释放计时器（与每账号每分钟限速共用），
// 没有要等的时刻时移出登记。到时经 notifyLocked 由 app 层 forwardPoolChanges 按到达序号唤醒各队首；调用方持有 p.mu
func (p *AccountPool) egressArmLocked(rate *egressRate, now int64) {
	at := p.egressWakeAtLocked(rate, now)
	if at == 0 {
		delete(p.egressFull, rate)
		return
	}
	if p.egressFull == nil {
		p.egressFull = make(map[*egressRate]int64)
	}
	p.egressFull[rate] = at
	p.armPoolWakeLocked(at, now)
}

// egressReleasedLocked 为窗口释放计时器回调的出口部分：登记时刻已到的通道算作释放（调用方随后 notifyLocked），并按当前状态重新登记；
// 返回是否有通道释放与剩下的最早登记时刻（0 为没有）。调用方持有 p.mu
func (p *AccountPool) egressReleasedLocked(now int64) (bool, int64) {
	released, next := false, int64(0)
	for rate, at := range p.egressFull {
		if at <= now {
			released = true
			if at = p.egressWakeAtLocked(rate, now); at == 0 {
				delete(p.egressFull, rate)
				continue
			}
			p.egressFull[rate] = at
		}
		if next == 0 || at < next {
			next = at
		}
	}
	return released, next
}

// reserveEgressLocked 为刚取得的生成租约预占所在通道：pending 加一；限速大于 0 时立即消耗一个令牌（tat = max(tat, now) + interval），
// 消耗后通道对某类选择不准入时登记下一个令牌的唤醒。限速为 0 时只预占、不动 tat。调用方持有 p.mu
func (p *AccountPool) reserveEgressLocked(lease *AccountLease, now time.Time, primary bool) {
	rate := lease.egress
	if rate == nil {
		return
	}
	rate.pending++
	lease.egressPending = true
	lease.egressPrimary = primary
	if rate.limit <= 0 {
		return
	}
	nowNanos := now.UnixNano()
	rate.tat = max(rate.tat, nowNanos) + rate.interval
	lease.egressCharged = true
	p.egressArmLocked(rate, nowNanos)
}

// recordEgressSentLocked 为租约记一次生成发送：抵掉预占（只抵一次），追加发送记录（带是否主用）。记在租约取得时的通道上；
// 调用方持有 p.mu
func (p *AccountPool) recordEgressSentLocked(lease *AccountLease, now int64) {
	rate := lease.egress
	if rate == nil {
		return
	}
	if lease.egressPending {
		lease.egressPending = false
		if rate.pending > 0 {
			rate.pending--
		}
	}
	rate.sent.add(now, lease.egressPrimary, now-p.egressWindowLocked())
}

// releaseEgressLocked 在 releaseLocked 里退还没发出的预占：pending 减一，消耗过令牌且仍在限速时退还它（tat = max(tat − interval, now)），
// 唤醒由 releaseLocked 随后的 notifyLocked 完成。调用方持有 l.operation 与 p.mu
func (l *AccountLease) releaseEgressLocked(now int64) {
	if !l.egressPending {
		return
	}
	l.egressPending = false
	rate := l.egress
	if rate == nil {
		return
	}
	if rate.pending > 0 {
		rate.pending--
	}
	if l.egressCharged && rate.limit > 0 {
		rate.tat = max(rate.tat-rate.interval, now)
		l.pool.egressArmLocked(rate, now)
	}
}

// noteOtherSend 为租约记一次经 Client.do 发出的非生成 RPC（ListModels、CountTokens 等）：只计数，供状态与诊断，
// 不计入限速与 Sent1m（是否与生成共用出口额度未证实，计入会扭曲“限速是否起作用”的判断）
func (l *AccountLease) noteOtherSend() {
	if l == nil || l.pool == nil || l.egress == nil {
		return
	}
	p := l.pool
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now().UnixNano()
	l.egress.other.add(now, false, now-p.egressWindowLocked())
}

// egressBusyError 返回只因出口令牌桶已满而排队超过上限的错误；next 为下一个令牌的时刻（0 为未知），建议重试时间限定在
// [egressWaitRetryMin, egressWaitRetryMax]
func egressBusyError(selection AccountSelection, waited, limit time.Duration, next int64, now time.Time) *CapacityBusyError {
	retry := egressWaitRetryMin
	if next > 0 {
		retry = min(max(time.Duration(next-now.UnixNano()), egressWaitRetryMin), egressWaitRetryMax)
	}
	return &CapacityBusyError{
		ModelID: strings.TrimPrefix(strings.TrimSpace(selection.ModelID), "models/"), Pool: selection.Pool, Reason: EgressBusyReason,
		Waited: waited, Limit: limit, Attempt: 1, Until: now.Add(retry),
	}
}

// SetGlobalProxy 设置全局代理（PROXY），只用于计算出口键（configurePool 调用），按新值重挂全部账户；有账户换了通道时唤醒等待方
func (p *AccountPool) SetGlobalProxy(proxy string) {
	if p == nil {
		return
	}
	proxy = strings.TrimSpace(proxy)
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.globalProxy == proxy {
		return
	}
	p.globalProxy = proxy
	if p.reattachEgressAllLocked() {
		p.notifyLocked()
	}
}

// EgressKeyOf 返回账户当前的出口键与分区通道；账户不存在时按全局代理与普通通道
func (p *AccountPool) EgressKeyOf(accountID string) (key string, lane EgressLane) {
	if p == nil {
		return "", EgressLaneNormal
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	account := p.byID[strings.TrimSpace(accountID)]
	if account == nil || account.egress == nil {
		return EgressKey(p.globalProxy), EgressLaneNormal
	}
	return account.egress.record.key, account.egress.lane
}

// EgressSnapshot 返回各出口的账户池侧计数（一次加锁，O(出口数)），按出口建立的顺序；没有账户、没有限速、没有预占、
// 窗口内没有发送的出口（账户都换了代理后留下的记录）不列出
func (p *AccountPool) EgressSnapshot() []EgressStatus {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now().UnixNano()
	window := p.egressWindowLocked()
	var snapshot []EgressStatus
	for at := 0; at+1 < len(p.egressLanes); at += 2 {
		record := p.egressLanes[at].record
		status := EgressStatus{Key: record.key, ID: record.id, Label: record.label}
		live := false
		for index, rate := range record.lanes {
			rate.sent.prune(now - window)
			rate.other.prune(now - window)
			lane := EgressLaneStatus{
				Lane: rate.lane, Accounts: rate.accounts, Limit: rate.limit, Sent1m: rate.sent.live(),
				Sent30s: rate.sent.countAfter(now - window/2), PrimarySent1m: rate.sent.primary, OtherSent1m: rate.other.live(),
				Pending: rate.pending,
			}
			if rate.limit > 0 {
				lane.Burst = rate.burst
				if ok, next := p.egressAdmitLocked(rate, true, now); !ok {
					lane.NextAt = time.Unix(0, next).UTC()
				}
			}
			status.Lanes[index] = lane
			live = live || lane.Accounts > 0 || lane.Limit > 0 || lane.Pending > 0 || lane.Sent1m > 0 || lane.OtherSent1m > 0
		}
		if live {
			snapshot = append(snapshot, status)
		}
	}
	return snapshot
}

// SetEgressRateLimit 设置出口通道每分钟生成请求的上限（负数按 0），0 关闭：
//   - 0→L：从空桶开始（tat = now + τ），没有滑动日志那样的停顿：优先一方只有一个预留的令牌，另一方的第一个令牌在一个间隔后；
//   - L→L′：剩余欠量按比例缩放（tat′ = now + max(0, tat−now) × L/L′）；
//   - L→0：清 tat。
//
// 调大或关闭时唤醒等待方；设置后通道不准入时登记下一个令牌的唤醒。出口还没有记录时（没有账户挂在上面）先建立，账户挂上后即受限
func (p *AccountPool) SetEgressRateLimit(key string, lane EgressLane, limit int) {
	if p == nil || lane > EgressLaneUltra {
		return
	}
	limit = max(limit, 0)
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.egresses[key] == nil && limit == 0 {
		return
	}
	rate := p.egressLaneLocked(key, lane)
	previous := rate.limit
	if previous == limit {
		return
	}
	now := time.Now().UnixNano()
	if limit == 0 {
		rate.limit, rate.interval, rate.burst, rate.tat = 0, 0, 0, 0
		p.egressThrottled = slices.DeleteFunc(p.egressThrottled, func(other *egressRate) bool { return other == rate })
		delete(p.egressFull, rate)
		p.notifyLocked()
		return
	}
	interval := max(p.egressWindowLocked()/int64(limit), 1)
	burst := max(egressMinBurst, int(math.Ceil(float64(limit)*egressBurstShare)))
	if previous == 0 {
		p.egressThrottled = append(p.egressThrottled, rate)
		rate.tat = now + int64(burst-1)*interval
	} else {
		deficit := max(rate.tat-now, 0)
		rate.tat = now + int64(float64(deficit)*float64(previous)/float64(limit))
	}
	rate.limit, rate.interval, rate.burst = limit, interval, burst
	p.egressArmLocked(rate, now)
	if limit > previous {
		p.notifyLocked()
	}
}

// FilterAccountsByEgress 剔除出口在 blocked 中的账户，返回保留的与剔除的（保持原顺序，一次加锁，O(n)）；
// 不在号池中的账户保留（出口未知）。没有剔除时 kept 就是 ids
func (p *AccountPool) FilterAccountsByEgress(ids []string, blocked map[string]struct{}) (kept []string, removed []string) {
	if p == nil || len(ids) == 0 || len(blocked) == 0 {
		return ids, nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for at, accountID := range ids {
		limited := false
		if account := p.byID[strings.TrimSpace(accountID)]; account != nil && account.egress != nil {
			_, limited = blocked[account.egress.record.key]
		}
		switch {
		case limited:
			if kept == nil {
				kept = append(make([]string, 0, len(ids)), ids[:at]...)
			}
			removed = append(removed, accountID)
		case kept != nil:
			kept = append(kept, accountID)
		}
	}
	if removed == nil {
		return ids, nil
	}
	return kept, removed
}

// rateLadderLevel 解析逐级限流冷却的原因（“限流: 连续限流第 N 档，<原文>”），返回档位与原文
func rateLadderLevel(reason string) (int, string, bool) {
	tail, found := strings.CutPrefix(reason, rateLadderPrefix)
	if !found {
		return 0, "", false
	}
	digits, rest, found := strings.Cut(tail, " 档")
	if !found {
		return 0, "", false
	}
	level, err := strconv.Atoi(digits)
	if err != nil || level < 1 {
		return 0, "", false
	}
	return level, strings.TrimPrefix(rest, "，"), true
}

// relaxableRateLadder 判断冷却是仍生效、档位不低于 minLevel、到期晚于 until 的逐级限流冷却，返回档位与原因原文
func relaxableRateLadder(cooldown CooldownState, minLevel int, until time.Time, now time.Time) (int, string, bool) {
	level, rest, ok := rateLadderLevel(cooldown.Reason)
	if !ok || level < minLevel || !cooldown.Active(now) || !cooldown.Until.After(until) {
		return 0, "", false
	}
	return level, rest, true
}

// RateLadderAccounts 返回出口上逐级限流冷却达到第 minLevel 档、仍生效的账户（按 ID 排序；出口限流判定成立时撤回）。
// 一次加锁扫描全部账户，只在判定成立时调用一次
func (p *AccountPool) RateLadderAccounts(key string, minLevel int) []string {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	record := p.egresses[key]
	if record == nil {
		return nil
	}
	now := time.Now()
	var ids []string
	for _, account := range p.accounts {
		if account == nil || account.egress == nil || account.egress.record != record || p.byID[account.ID] != account {
			continue
		}
		for _, cooldown := range account.runtime.Cooldowns {
			if _, _, ok := relaxableRateLadder(cooldown, minLevel, now, now); ok {
				ids = append(ids, account.ID)
				break
			}
		}
	}
	sort.Strings(ids)
	return ids
}

// RelaxRateLimitCooldowns 把账户第 minLevel 档以上、到期晚于 until 的逐级限流冷却缩短到 until 并清除对应的档位，返回改动的冷却数。
// 原因保留原文，前缀改为“限流: 出口限流撤回（原第 N 档），”便于诊断，此后不再按逐级冷却识别。
// 这里有意打破“冷却从不缩短”（MarkRateLimitCooldown 的 writeScope、TryExtendCooldownIfGeneration）：出口限流时这些冷却
// 是出口被限造成的，不是账号额度用完，留着会让驻留 Worker 成死槽位、被淘汰后由同一出口的替补冷启动再被拒。
// 只动逐级限流的冷却：每日限额、分钟限额、403、按账号降级的冷却原因不以“连续限流第”开头，都不动。
// 缩短后 app 层淘汰的 stillCooling 为假，登记淘汰与排空随之撤销
func (p *AccountPool) RelaxRateLimitCooldowns(accountID string, until time.Time, minLevel int) (int, error) {
	if p == nil {
		return 0, ErrAccountNotFound
	}
	until = until.UTC()
	// 内存中没有符合的冷却时不读写运行态文件
	p.mu.Lock()
	account := p.byID[strings.TrimSpace(accountID)]
	if account == nil {
		p.mu.Unlock()
		return 0, fmt.Errorf("%w: %s", ErrAccountNotFound, accountID)
	}
	found := false
	for _, cooldown := range account.runtime.Cooldowns {
		if _, _, ok := relaxableRateLadder(cooldown, minLevel, until, time.Now()); ok {
			found = true
			break
		}
	}
	p.mu.Unlock()
	if !found {
		return 0, nil
	}
	relaxed := 0
	_, err := p.updateRuntime(accountID, func(_ *Account, runtimeState *accountRuntimeState) (bool, func(*Account), error) {
		var scopes []string
		now := time.Now()
		for scope, cooldown := range runtimeState.Cooldowns {
			level, rest, ok := relaxableRateLadder(cooldown, minLevel, until, now)
			if !ok {
				continue
			}
			runtimeState.Cooldowns[scope] = CooldownState{
				Until: until, Reason: fmt.Sprintf("%s: 出口限流撤回（原第 %d 档），%s", RateLimitKind, level, rest),
			}
			scopes = append(scopes, scope)
		}
		relaxed = len(scopes)
		if relaxed == 0 {
			return false, nil, nil
		}
		return true, func(account *Account) {
			// 档位随冷却一起撤回：撤回后再失败从第一档起
			for _, scope := range scopes {
				for _, key := range verifiedStrikeKeys(account, scope) {
					delete(account.rateStrikes, key)
				}
			}
		}, nil
	})
	if err != nil {
		return 0, err
	}
	return relaxed, nil
}

// EgressKey 返回租约取得时账户的出口键，不加锁（通道建立后不变）；没有通道时为直连
func (l *AccountLease) EgressKey() string {
	if l == nil || l.egress == nil {
		return ""
	}
	return l.egress.record.key
}

// EgressLane 返回租约取得时账户所在的分区通道，不加锁
func (l *AccountLease) EgressLane() EgressLane {
	if l == nil || l.egress == nil {
		return EgressLaneNormal
	}
	return l.egress.lane
}

type egressWaitLimitContextKey struct{}

// WithEgressWaitLimit 给不经 app 层 acquireLease 的生成选号（转写）设“只因出口令牌桶已满”的累计等待上限，
// 超过时 AcquireFor 返回 CapacityBusyError{Reason: "出口限速"}；没有设置时不限
func WithEgressWaitLimit(ctx context.Context, limit time.Duration) context.Context {
	if limit <= 0 {
		return ctx
	}
	return context.WithValue(ctx, egressWaitLimitContextKey{}, limit)
}

// egressWaitLimitFrom 返回 WithEgressWaitLimit 设置的上限，没有设置时为 0（不限）
func egressWaitLimitFrom(ctx context.Context) time.Duration {
	if ctx == nil {
		return 0
	}
	limit, _ := ctx.Value(egressWaitLimitContextKey{}).(time.Duration)
	return limit
}

// verifyEgressLocked 核对出口通道：每个账户挂在按当前代理与 Ultra 成员关系应在的通道上，各通道账户数与实际一致，预占与限速不为负、
// 限速通道数与 egressThrottled 一致，候选索引里的通道编号与账户字段一致；供测试在随机变更后检查。调用方持有 p.mu
func (p *AccountPool) verifyEgressLocked() error {
	counts := make(map[*egressRate]int, len(p.egressLanes))
	for _, account := range p.accounts {
		if account == nil {
			continue
		}
		if p.byID[account.ID] != account {
			if account.egress != nil {
				return fmt.Errorf("不在号池中的账户 %s 仍挂在出口通道上", account.ID)
			}
			continue
		}
		lane := EgressLaneNormal
		if _, ultra := p.ultraAccounts[account.ID]; ultra {
			lane = EgressLaneUltra
		}
		record := p.egresses[EgressKey(account.EffectiveProxy(p.globalProxy))]
		if record == nil || account.egress != record.lanes[lane] {
			return fmt.Errorf("账户 %s 的出口通道与代理 %q、通道 %d 不符", account.ID, account.EffectiveProxy(p.globalProxy), lane)
		}
		counts[account.egress]++
	}
	throttled, populated := 0, 0
	for index, rate := range p.egressLanes {
		if rate.id != uint32(index) || rate.record.lanes[rate.lane] != rate {
			return fmt.Errorf("出口通道 %d 的编号或所属出口不符", index)
		}
		if rate.accounts != counts[rate] {
			return fmt.Errorf("出口 %s 通道 %d 的账户数 %d，实际 %d", rate.record.label, rate.lane, rate.accounts, counts[rate])
		}
		if rate.pending < 0 || rate.limit < 0 {
			return fmt.Errorf("出口 %s 通道 %d 的预占 %d 或限速 %d 为负", rate.record.label, rate.lane, rate.pending, rate.limit)
		}
		if rate.limit > 0 {
			throttled++
			if !slices.Contains(p.egressThrottled, rate) {
				return fmt.Errorf("出口 %s 通道 %d 限速 %d，不在限速通道列表里", rate.record.label, rate.lane, rate.limit)
			}
		}
		if rate.accounts > 0 {
			populated++
		}
	}
	if throttled != len(p.egressThrottled) {
		return fmt.Errorf("限速通道数 %d，列表 %d", throttled, len(p.egressThrottled))
	}
	if populated != p.egressPopulated {
		return fmt.Errorf("有账户的通道数 %d，计数 %d", populated, p.egressPopulated)
	}
	if index := p.sched; index != nil {
		if len(index.egressID) != len(p.accounts) {
			return fmt.Errorf("候选索引出口通道数组长度 %d 与账户数 %d 不符", len(index.egressID), len(p.accounts))
		}
		for position, account := range p.accounts {
			if account != nil && index.egressID[position] != egressLaneID(account) {
				return fmt.Errorf("账户 %s 的出口通道编号 %d 与账户字段 %d 不符", account.ID, index.egressID[position], egressLaneID(account))
			}
		}
	}
	return nil
}
