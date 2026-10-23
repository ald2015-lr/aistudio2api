package app

import (
	"cmp"
	"context"
	"fmt"
	"math"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/api"
)

// 出口限流判定：同一出口（账户有效代理规范化后的值，空为直连，见 aistudio.EgressKey）上的多个账号同时被上游裸限流、
// 新换上的账号第一次请求就被拒时，问题在出口而不在账号。判定成立期间裸 429 只短冷却、不升档，冷启动按配额进行，
// 账户池按出口限速发送（主用模型优先），模型目录同步暂停。
//
// 线上（单出口直连，两份诊断）：每 2 分钟约 270 次裸 429、49 次成功（比例 0.83–0.85），涉及数百个账号；新号第一次请求就被拒
// （之前成功次数中位 0）；成功数封顶在每分钟 17–27 个，发送从 112 涨到 168 次/分钟成功也不变多。原来的号池级保护要求 10 分钟零成功，
// 任一成功就清空记录（rate_limit_guard.go），从不触发；逐级冷却把驻留 Worker 盖成死槽位，淘汰与替补冷启动 30 分钟 700–1000 次。
//
// 判定按出口独立（多出口时互不影响），只统计已发往上游的生成尝试（成功、裸 429、其他失败，见 egressOutcome）：
//   - 进入：2 分钟内裸 429 ≥ egressMinBare 次、来自 ≥ egressMinAccounts 个账号，并且（比例 ≥ 0.5 且冷号首发被拒 ≥ 3）或
//     （比例 ≥ 0.8 且 ≥ 20 个账号）。冷号首发：本进程本额度日内这个账号此前没有任何结果，第一次就被裸 429——每日额度耗尽的账号当天
//     一定先成功过，产生不了这个信号；平稳期驻留账号都不再是冷号，由高比例加多账号的条件覆盖；
//   - 限速：进入时普通通道按最近 60 秒成功 ×1.25 启用令牌桶（账户池执行，见 aistudio/egress_rate.go），之后 AIMD：降速后成功数下降就回退，
//     限速 3 分钟仍不能把比例降 0.2 就判为无效、停止限速（拒绝与速率无关时少发只会少成功），其余保护保留；
//   - 按账号证据降级：判定期间连续被拒、与出口按比例拒绝区分得开的账号写 30 分钟账号冷却，此后不计入出口统计；
//   - 解除：持续 ≥3 分钟、2 分钟比例 <5%、限速连续 2 分钟不起作用；安全阀 A（≥30 个复试账号零成功、出口仍有成功）撤销判定 30 分钟，
//     更像账号当天额度用完；安全阀 B（下限仍 ≥90% 被拒）只告警——撤销会退回逐级冷却风暴；管理页可以手动撤销或恢复。
//
// 判定器自带一把锁（mu），只在锁内算出动作，解锁后再调用账户池（设限速、取快照、撤回逐级冷却）与写日志：mu 从不与账户池的锁嵌套，
// 也不在持有它时拿 Worker 管理器的锁（冷启动闸门的锁顺序为 rebalanceMu → mu）。计时器独立（run，5 秒），不放进 keepWarmPass：
// 清扫死槽位会同步等浏览器关闭，会拖慢限速调整与安全阀计时。全部状态只在内存，重启后从 normal 开始

const (
	// egressSuspendedRateSwitches 为限速判为无效（Suspended）时普通号池限流类换号的上限：这时拒绝与发送速率无关，
	// 近似按概率拒绝，每次换号是一次独立机会
	egressSuspendedRateSwitches = 4

	// egressJudgeWindow 为进入判定与比例的窗口：线上每 2 分钟约 270 次裸 429，样本足够，几秒到十几秒即可成立
	egressJudgeWindow = 2 * time.Minute
	// egressMinBare 与 egressMinAccounts：小样本不判；5 与原号池级判定的 rateLimitSystemicAccounts 一致
	egressMinBare     = 10
	egressMinAccounts = 5
	// egressEnterRatio 与 egressMinFresh：健康时比例远低于 0.1；冷号首发是出口维度最强的信号
	egressEnterRatio = 0.5
	egressMinFresh   = 3
	// egressEnterAltRatio 与 egressEnterAltAccounts：平稳期没有冷号时的进入条件（线上比例 0.83–0.85、账号数百）
	egressEnterAltRatio    = 0.8
	egressEnterAltAccounts = 20
	// egressUltraMinBare 与 egressUltraEnterRatio：判定期间 Ultra 通道自己有这么多裸 429 证据时才启用 Ultra 限速，
	// Ultra 号池（约 20 个账号）不因普通号池的流量被限住
	egressUltraMinBare    = 3
	egressUltraEnterRatio = 0.5

	// egressLimitedCooldown 为判定期间裸 429 的冷却：< ShortCooldownMax（不算冷却中、成不了死槽位）、< rotationMinRemaining，
	// 又足够让并发请求不立刻再选中它
	egressLimitedCooldown = 10 * time.Second

	// egressMinLimit、egressInitialFactor 与 egressFloorShare：初值 = max(6, 最近 60 秒成功 ×1.25)，下限 = max(6, 初值 ×0.5)。
	// 线上成功 17–27 个/分钟 → 初值约 21–34、下限约 11–17；下限低于初值，留出“被拒的请求也占出口额度、少发反而多成功”的空间，
	// 降速回退兜住相反情形
	egressMinLimit      = 6
	egressInitialFactor = 1.25
	egressFloorShare    = 0.5
	// egressDecreaseFactor、egressDecreaseQuiet、egressDecreaseMinBare、egressDecreaseMinRatio：30 秒内裸 429 ≥3 且比例 ≥0.2 时乘性减；
	// 冷静期约等于在途请求的首事件时间，避免对降速前发出的请求重复惩罚
	egressDecreaseFactor   = 0.7
	egressDecreaseQuiet    = 30 * time.Second
	egressDecreaseMinBare  = 3
	egressDecreaseMinRatio = 0.2
	// egressDecreaseCheck、egressDecreaseRevertShare、egressDecreaseMinSuccess、egressDecreaseLock：降速满 60 秒后成功数比降速前少 20% 以上
	// （降速前至少 5 个）就回到原值并锁定 10 分钟。出口能力固定时少发不会少成功，回退不触发；拒绝与速率无关时少发就少成功
	egressDecreaseCheck       = time.Minute
	egressDecreaseRevertShare = 0.8
	egressDecreaseMinSuccess  = 5
	egressDecreaseLock        = 10 * time.Minute
	// egressFastFactor、egressSlowStepRatio、egressIncreaseEvery、egressIncreaseAfterDecrease、egressIncreaseMinSent：
	// 30 秒内没有裸 429 时每 15 秒 ×1.5（6 步约 11 倍）；比例 <0.1 时 +max(1, 10%)；距上次降速不足 60 秒、限速不起作用或
	// 30 秒内实际发送不足 max(3, 限速/4) 时不提速（没有发送就没有证据，避免空窗期盲目提速）
	egressFastFactor            = 1.5
	egressSlowStepRatio         = 0.1
	egressIncreaseEvery         = 15 * time.Second
	egressIncreaseAfterDecrease = time.Minute
	egressIncreaseMinSent       = 3
	// egressBindingShare 与 egressBindingWaitWindow：最近 60 秒发送 ≥ 限速 ×0.8，或最近 30 秒有过“出口限速”排队，为限速在起作用
	egressBindingShare      = 0.8
	egressBindingWaitWindow = 30 * time.Second
	// egressLimiterJudgeAfter、egressLimiterJudgeSpan、egressLimiterImproveMin：通道限速启用满 3 分钟、最近 2 分钟一直在起作用，
	// 比例却没有比进入时降 0.2 以上（又低于 egressValveRatio）时判为无效、停止限速
	egressLimiterJudgeAfter = 3 * time.Minute
	egressLimiterJudgeSpan  = 2 * time.Minute
	egressLimiterImproveMin = 0.2
	// egressValveRatio 为安全阀 B（只告警）的比例，也是限速有效性检查的上界；egressFloorWarnAfter 为停在下限多久才告警
	egressValveRatio     = 0.9
	egressFloorWarnAfter = 10 * time.Minute

	// egressDemoteConfidence、egressDemoteMinStreak、egressDemoteMaxStreak：出口按比例 r 随机拒绝时连续 k 次被拒的概率 <1%，
	// k = clamp(ceil(ln 0.01 / ln r), 3, 8)；r 更高时个别账号无法与出口区分，不按这条降级
	egressDemoteConfidence = 0.01
	egressDemoteMinStreak  = 3
	egressDemoteMaxStreak  = 8
	// egressDemoteBackstopStreak 与 egressDemoteBackstopSpan：本次判定期间零成功、连续 6 次、跨 10 分钟的账号不论 r 都降级
	// （重启后一批当天额度已用完、只回裸 QE 的账号由它清出驻留）
	egressDemoteBackstopStreak = 6
	egressDemoteBackstopSpan   = 10 * time.Minute
	// egressDemoteCooldown 为降级写的账号冷却：≥ rotationMinRemaining+10m，成为死槽位，由淘汰与补位换掉
	egressDemoteCooldown = 30 * time.Minute

	// egressMinHold、egressExitRatio、egressExitIdle：解除要足够稳定，又不拖太久
	egressMinHold   = 3 * time.Minute
	egressExitRatio = 0.05
	egressExitIdle  = 2 * time.Minute
	// egressValveHold、egressValveRetried、egressValveMinSuccess、egressRejectFor：安全阀 A。限速生效时单次成功约 0.8，
	// 30 个复试账号全部失败的概率可以忽略；样本不足不撤销（round-robin 下同一账号再被选中的间隔约为 驻留数/限速）
	egressValveHold       = 10 * time.Minute
	egressValveRetried    = 30
	egressValveMinSuccess = 10
	egressRejectFor       = 30 * time.Minute

	// egressRefillConcurrencyMin 与 egressRefillConcurrencyShare：补位并发 max(2, WARM_STARTUP_CONCURRENCY/4)，线上 16 → 4。
	// 线上 401 约 4.8 个/分钟、启动约 20 秒、失败 9–22%，4 并发约 9–11 个/分钟，补得上
	egressRefillConcurrencyMin   = 2
	egressRefillConcurrencyShare = 0.25
	// egressCoveragePerMinute 为补覆盖每（出口，通道）每分钟的上限
	egressCoveragePerMinute = 6
	// egressRelaxConcurrency 与 egressRelaxMinLevel：进入时撤回第 2 档以上的逐级冷却（一次性，线上 170–590 个账户要写盘）
	egressRelaxConcurrency = 4
	egressRelaxMinLevel    = 2

	// egressRetryMin 与 egressRetryMax 为 Retry-After 的范围：按当前排队长度估计轮到的时间
	egressRetryMin = 5 * time.Second
	egressRetryMax = 60 * time.Second

	// egressBucket、egressBuckets、egressTick：10 秒一桶、60 桶（10 分钟历史），O(1) 摊销；独立计时器 5 秒
	egressBucket  = 10 * time.Second
	egressBuckets = 60
	egressTick    = 5 * time.Second

	// 节流：暂停冷启动、每分钟请求已满同出口 10 分钟一条；降级同出口每分钟汇总一条
	egressSkipLogEvery   = 10 * time.Minute
	egressFullLogEvery   = 10 * time.Minute
	egressDemoteLogEvery = time.Minute
	// egressLocalRejectWindow 为状态里本地 429 计数的窗口
	egressLocalRejectWindow = time.Minute
)

// 通道下标（与 aistudio.EgressLane 一致）
const (
	egressLaneNormal = int(aistudio.EgressLaneNormal)
	egressLaneUltra  = int(aistudio.EgressLaneUltra)
)

// errEgressColdStartPaused 表示账户所在出口正在限流、冷启动闸门暂停了这一路径的冷启动：调用方按容量处理
// （errors.Is(err, errAccountWorkerCapacity) 为真），不记启动失败，也不进预热失败黑名单
var errEgressColdStartPaused = fmt.Errorf("%w: %s", errAccountWorkerCapacity, egressColdStartPausedReason)

// egressColdStartPausedReason 为冷启动被出口闸门暂停时写进预热与替补日志的原因（也是对外号池繁忙的原因，见 aistudio.EgressColdStartReason）
const egressColdStartPausedReason = aistudio.EgressColdStartReason

// egressColdStartBusy 为数据路径（转写、Live、Veo 经 accountWorkerManager.Worker）与固定账户的生成请求（generateWithRetry）
// 的冷启动被出口闸门暂停：按号池繁忙返回
// （429，Retry-After 为 egressRetryMin，原因为 egressColdStartPausedReason），errors.Is 仍认 errEgressColdStartPaused
type egressColdStartBusy struct {
	busy  *aistudio.CapacityBusyError
	cause error
}

func newEgressColdStartBusy(modelID string, pool aistudio.PoolScope, cause error) *egressColdStartBusy {
	return &egressColdStartBusy{
		busy: &aistudio.CapacityBusyError{
			ModelID: modelID, Pool: pool, Reason: egressColdStartPausedReason, Until: time.Now().Add(egressRetryMin),
		},
		cause: cause,
	}
}

func (err *egressColdStartBusy) Error() string { return err.busy.Error() }

func (err *egressColdStartBusy) Unwrap() []error { return []error{err.busy, err.cause} }

// egressOutcome 为一次已发往上游的生成尝试的结果（判定器只统计这三类）
type egressOutcome uint8

const (
	egressSuccess egressOutcome = iota + 1 // 收到上游首个事件
	egressBare                             // 裸限流 429（QuotaCooldown.Escalate 且来自生成本身）
	egressOther                            // 其他失败：5xx、首事件超时、带指标的每日或分钟限额、403、404（输入超限、参数错误、上游 400、客户端取消除外）
)

// egressObservation 为交给判定器的一次生成尝试结果
type egressObservation struct {
	Key, AccountID string
	Ultra          bool // 账户在 Ultra 分区（租约的 EgressLane）
	Outcome        egressOutcome
	At             time.Time
}

// egressMode 为出口的判定状态
type egressMode uint8

const (
	egressNormal   egressMode = iota // 没有判定
	egressLimited                    // 判定为出口限流
	egressRejected                   // 判定已撤销（安全阀或管理操作），期间不再判定
)

// String 返回状态接口里的写法
func (mode egressMode) String() string {
	switch mode {
	case egressLimited:
		return "limited"
	case egressRejected:
		return "rejected"
	default:
		return "normal"
	}
}

// egressVerdict 为某出口（及账户所在通道）当前的判定
type egressVerdict struct {
	Mode      egressMode
	Limited   bool      // Mode == egressLimited：短冷却不升档、冷启动闸门、目录暂停、限流类换号按出口限流的规则
	Throttled bool      // 账户所在通道的令牌桶生效（limit>0）
	Suspended bool      // 限速已判为无效并停止：限流类换号放宽到 egressSuspendedRateSwitches
	Label     string    // 脱敏标签
	Limit     int       // 当前限速（次/分钟），0 为不限
	RetryAt   time.Time // 建议重试时间，已限定在 [egressRetryMin, egressRetryMax]
	Demote    bool      // 只由 observeOutcome 返回：该账户按账号证据降级（写 30 分钟账号冷却）
	// DemoteStreak 只与 Demote 一起返回：该账户本次判定期间连续裸 429 的次数（写进降级冷却的原因与日志）
	DemoteStreak int
}

// coldStartPath 为冷启动闸门区分的路径：
//   - coldStartRefill 补位：空槽且低于常驻数，只受并发约束；
//   - coldStartCoverage 补覆盖：驻留里没有能服务该模型的热 Worker 时放回一个备用账户启动，另受每分钟配额约束；
//   - coldStartReplace 替换空闲 Worker：出口限流时暂停；
//   - coldStartExpand 空槽但已不低于常驻数（超出常驻）：出口限流时暂停
type coldStartPath uint8

const (
	coldStartRefill coldStartPath = iota
	coldStartCoverage
	coldStartReplace
	coldStartExpand
)

// coldStartGateFunc 为 Worker 管理器使用的冷启动闸门：ok 为假时本次冷启动暂停（返回 errEgressColdStartPaused）；
// ok 为真时启动结束后调用 release，started 报告是否启动成功
type coldStartGateFunc func(accountID, modelID string, path coldStartPath) (release func(started bool), ok bool)

// egressRefillLimit 返回补位冷启动的并发上限：max(2, WARM_STARTUP_CONCURRENCY × 0.25)
func egressRefillLimit(warmConcurrency int) int {
	return max(egressRefillConcurrencyMin, int(float64(warmConcurrency)*egressRefillConcurrencyShare))
}

// egressPool 为判定器用到的账户池方法（包内测试换成假账户池）
type egressPool interface {
	EgressSnapshot() []aistudio.EgressStatus
	SetEgressRateLimit(key string, lane aistudio.EgressLane, limit int)
	RateLadderAccounts(key string, minLevel int) []string
	RelaxRateLimitCooldowns(accountID string, until time.Time, minLevel int) (int, error)
}

// egressLogEntry 为一条判定器日志（durable 为走 logDurable 的持久行）
type egressLogEntry struct {
	durable bool
	level   string
	message string
}

// egressLaneRef 为一个出口通道（限速同步的单位）
type egressLaneRef struct {
	key  string
	lane int
}

// egressRelaxJob 为进入判定时撤回逐级冷却的后台任务
type egressRelaxJob struct {
	key, label string
	since      time.Time
}

// egressActions 为在判定器锁内算出、解锁后执行的动作
type egressActions struct {
	limits bool
	logs   []egressLogEntry
	relax  []egressRelaxJob
}

// log 记一条持久日志（判定器的日志都很少出现或已限频，一律走 logDurable）
func (actions *egressActions) log(level string, format string, args ...any) {
	actions.logs = append(actions.logs, egressLogEntry{durable: true, level: level, message: fmt.Sprintf(format, args...)})
}

// egressGuard 为按出口的限流判定与限速：自带一把锁，不与账户池的锁嵌套
type egressGuard struct {
	mu       sync.Mutex
	pool     egressPool
	requests *requestRegistry
	states   map[string]*egressState
	byID     map[string]string // 出口编号 → 键（管理动作）
	// seen 为账户 → 最近一次尝试结果所在的太平洋时间额度日（判断“冷号”），seenDay 为上次清理时的额度日；
	// dayKey、dayFrom、dayEnd 为当前额度日的缓存（NextQuotaDay 每次都要载入时区）
	seen    map[string]int32
	seenDay int32
	dayKey  int32
	dayFrom time.Time
	dayEnd  time.Time
	// dirty 为限速有变化、待同步到账户池的通道；lastSnap 为上一次计时取到的账户池计数（只用于日志里的发送数）
	dirty    map[egressLaneRef]struct{}
	lastSnap map[string]aistudio.EgressStatus
	// limited 为判定成立的出口（无锁读，见 limitedKeys），判定变化时整份替换
	limited atomic.Pointer[map[string]struct{}]
	// applyMu 串行化限速同步：按最新判定写入账户池，先算出的旧值不会覆盖后算出的新值
	applyMu sync.Mutex
	relaxWG sync.WaitGroup
	ticks   atomic.Int64
	// forced 为测试写入的判定（见 forceVerdictForTest），只给包内测试
	forced map[string]egressVerdict
	// badAccount 判断账户是否已标坏号（连续裸限流，见 bad_accounts.go）：已标坏号的账户裸 429 不计“冷号首发被拒”（它被拒是账号的原因，
	// 不是出口的）；为空时不排除。newRuntime 在启动前挂上
	badAccount func(accountID string) bool
	// 以下只给包内测试替换（零值取默认）：now 为时钟，logf 截获日志，tickEvery 为计时器间隔，shortCooldown 为判定期间裸 429 的冷却
	now           func() time.Time
	logf          func(egressLogEntry)
	tickEvery     time.Duration
	shortCooldown time.Duration
}

// egressCounts 为一个桶里一个通道的计数：成功、裸 429、其他失败、冷号首发被拒，以及进入“出口限速”排队的次数（记在普通通道上）
type egressCounts struct {
	success, bare, other, fresh, waits int
}

func (counts *egressCounts) add(other egressCounts) {
	counts.success += other.success
	counts.bare += other.bare
	counts.other += other.other
	counts.fresh += other.fresh
	counts.waits += other.waits
}

func (counts egressCounts) attempts() int {
	return counts.success + counts.bare + counts.other
}

// ratio 为裸 429 占尝试结果的比例，没有尝试时为 0
func (counts egressCounts) ratio() float64 {
	if attempts := counts.attempts(); attempts > 0 {
		return float64(counts.bare) / float64(attempts)
	}
	return 0
}

// egressBucketSlot 为一个 10 秒桶：index 为桶序号（UnixNano / egressBucket），0 为空
type egressBucketSlot struct {
	index int64
	lanes [2]egressCounts
}

// egressHistory 为 10 分钟的环形计数（10 秒一桶）
type egressHistory struct {
	slots [egressBuckets]egressBucketSlot
}

func egressBucketIndex(now time.Time) int64 {
	return now.UnixNano() / int64(egressBucket)
}

// slot 返回 now 所在的桶，过期的桶先清空
func (history *egressHistory) slot(now time.Time) *egressBucketSlot {
	index := egressBucketIndex(now)
	slot := &history.slots[((index%egressBuckets)+egressBuckets)%egressBuckets]
	if slot.index != index {
		*slot = egressBucketSlot{index: index}
	}
	return slot
}

// sum 返回最近 window（按整桶向上取整，含当前桶）的计数；lane 为 -1 时合计两个通道
func (history *egressHistory) sum(now time.Time, window time.Duration, lane int) egressCounts {
	buckets := int64((window + egressBucket - 1) / egressBucket)
	buckets = min(max(buckets, 1), egressBuckets)
	current := egressBucketIndex(now)
	var total egressCounts
	for index := range history.slots {
		slot := &history.slots[index]
		if slot.index == 0 || slot.index > current || slot.index <= current-buckets {
			continue
		}
		if lane >= 0 {
			total.add(slot.lanes[lane])
			continue
		}
		total.add(slot.lanes[0])
		total.add(slot.lanes[1])
	}
	return total
}

// egressAccount 为本次判定期间有结果的账户
type egressAccount struct {
	streak    int       // 连续裸 429（成功清零）
	firstBare time.Time // 本次判定期间第一次裸 429
	bared     bool      // 本次判定期间裸 429 过
	succeeded bool      // 本次判定期间成功过
	retried   bool      // 裸 429 后又被尝试过（计入复试账号）
	recovered bool      // 复试后成功过（计入恢复账号）
	demoted   bool      // 已按账号证据降级：此后的结果不计入出口统计
}

// egressLaneState 为一个通道在本次判定期间的限速状态
type egressLaneState struct {
	active, suspended bool
	limit             int
	initial, floor    int
	ratioEntry        float64
	enabledAt         time.Time
	lastAdjust        time.Time
	lastDecrease      time.Time
	lastIncrease      time.Time
	// 降速回退检查：checkAt 为降速时刻（零值为没有待检查的降速），checkBefore 为降速前 60 秒成功数，checkPrev 为降速前的限速
	checkAt     time.Time
	checkBefore int
	checkPrev   int
	lockUntil   time.Time // 降速锁定期（回退后 10 分钟内不再降速）
	floorSince  time.Time // 限速停在下限的起点
	// bindingSince 与 idleSince 为限速当前连续起作用、连续不起作用的起点（另一个为零值）
	bindingSince time.Time
	idleSince    time.Time
}

// egressColdStart 为一个通道的冷启动配额（不随判定解除清空：在途启动结束时照常退还）
type egressColdStart struct {
	starting        int
	started         []time.Time // 最近一分钟闸门放行的冷启动
	coverage        []time.Time // 最近一分钟放行的补覆盖
	coverageOpening map[string]int
	deferred        []time.Time // 最近一分钟因补位并发已满推迟的补位
}

// egressSkipLog 为一个冷启动路径被暂停的计数与上次写日志的时间
type egressSkipLog struct {
	count    int
	loggedAt time.Time
}

// egressState 为一个出口的判定状态
type egressState struct {
	key, id, label string
	mode           egressMode
	since          time.Time
	history        egressHistory
	bareAccounts   map[string]time.Time // 窗口内裸 429 的账户 → 最近一次（计不同账号数）
	ratioEntry     float64
	accounts       map[string]*egressAccount
	retried        int
	recovered      int
	demoted        int
	relaxed        int
	periodSuccess  int
	periodBare     int
	lanes          [2]egressLaneState
	cold           [2]*egressColdStart
	queued         int
	lastWaitAt     time.Time
	localRejects   []time.Time
	rejectedUntil  time.Time
	rejectedBy     string
	skips          map[coldStartPath]*egressSkipLog
	floorWarnedAt  time.Time
	fullLoggedAt   time.Time
	demotePending  int
	demoteBackstop int
	demoteLoggedAt time.Time
}

// newEgressGuard 创建出口判定器
func newEgressGuard(pool *aistudio.AccountPool, requests *requestRegistry) *egressGuard {
	guard := &egressGuard{requests: requests}
	if pool != nil {
		guard.pool = pool
	}
	return guard
}

func (guard *egressGuard) clock() time.Time {
	if guard.now != nil {
		return guard.now()
	}
	return time.Now()
}

// limitedCooldown 返回判定期间裸 429 的冷却时长
func (guard *egressGuard) limitedCooldown() time.Duration {
	if guard != nil && guard.shortCooldown > 0 {
		return guard.shortCooldown
	}
	return egressLimitedCooldown
}

// stateLocked 返回出口的状态，没有时创建；调用方持有 mu
func (guard *egressGuard) stateLocked(key string) *egressState {
	if guard.states == nil {
		guard.states = make(map[string]*egressState)
		guard.byID = make(map[string]string)
	}
	state := guard.states[key]
	if state == nil {
		state = &egressState{
			key: key, id: aistudio.EgressID(key), label: aistudio.EgressLabel(key),
			bareAccounts: make(map[string]time.Time), accounts: make(map[string]*egressAccount),
			skips: make(map[coldStartPath]*egressSkipLog),
			cold:  [2]*egressColdStart{{coverageOpening: make(map[string]int)}, {coverageOpening: make(map[string]int)}},
		}
		guard.states[key] = state
		guard.byID[state.id] = key
	}
	return state
}

// quotaDayLocked 返回 now 所在的太平洋时间额度日（以次日零点的 Unix 分钟表示）；调用方持有 mu
func (guard *egressGuard) quotaDayLocked(now time.Time) int32 {
	if guard.dayEnd.IsZero() || !now.Before(guard.dayEnd) || now.Before(guard.dayFrom) {
		guard.dayEnd = aistudio.NextQuotaDay(now)
		guard.dayFrom = now
		guard.dayKey = int32(guard.dayEnd.Unix() / 60)
	}
	return guard.dayKey
}

// egressLaneOf 返回通道下标
func egressLaneOf(ultra bool) int {
	if ultra {
		return egressLaneUltra
	}
	return egressLaneNormal
}

// egressLaneName 返回通道在日志里的写法
func egressLaneName(lane int) string {
	if lane == egressLaneUltra {
		return "Ultra"
	}
	return "普通"
}

// egressPercent 返回比例的百分数写法
func egressPercent(ratio float64) string {
	return fmt.Sprintf("%.0f%%", ratio*100)
}

// egressWindowText 返回窗口的简写（2m、10m）
func egressWindowText(window time.Duration) string {
	if window >= time.Minute && window%time.Minute == 0 {
		return fmt.Sprintf("%dm", int(window/time.Minute))
	}
	return window.String()
}

// observeOutcome 记录一次已发往上游的生成尝试，返回记录后的判定（Demote 为该账户按账号证据降级）
func (guard *egressGuard) observeOutcome(obs egressObservation) egressVerdict {
	if guard == nil || obs.Outcome == 0 {
		return egressVerdict{}
	}
	now := obs.At
	if now.IsZero() {
		now = guard.clock()
	}
	var actions egressActions
	guard.mu.Lock()
	state := guard.stateLocked(obs.Key)
	lane := egressLaneOf(obs.Ultra)
	fresh := false
	if obs.AccountID != "" {
		day := guard.quotaDayLocked(now)
		if guard.seen == nil {
			guard.seen = make(map[string]int32)
		}
		fresh = guard.seen[obs.AccountID] != day
		guard.seen[obs.AccountID] = day
	}
	if state.mode == egressRejected && !now.Before(state.rejectedUntil) {
		guard.resetLocked(state, egressNormal, now)
		actions.log("INFO", "出口限流判定恢复 | 出口=%s | 原因=撤销到期", state.label)
	}
	demote, streak := false, 0
	if record := state.accounts[obs.AccountID]; record == nil || !record.demoted {
		counts := &state.history.slot(now).lanes[lane]
		switch obs.Outcome {
		case egressSuccess:
			counts.success++
		case egressBare:
			counts.bare++
			if fresh && (guard.badAccount == nil || !guard.badAccount(obs.AccountID)) {
				counts.fresh++
			}
			if obs.AccountID != "" {
				state.bareAccounts[obs.AccountID] = now
			}
		case egressOther:
			counts.other++
		}
		switch state.mode {
		case egressLimited:
			demote, streak = guard.trackAccountLocked(state, obs, now)
			if obs.Outcome == egressBare && lane == egressLaneUltra {
				guard.maybeEnableUltraLocked(state, now, &actions)
			}
		case egressNormal:
			if obs.Outcome == egressBare {
				guard.maybeEnterLocked(state, now, &actions)
			}
		}
	}
	verdict := guard.verdictLocked(state, obs.Ultra, now)
	if forced, ok := guard.forced[obs.Key]; ok {
		verdict = forced
	}
	verdict.Demote, verdict.DemoteStreak = demote, streak
	guard.mu.Unlock()
	guard.apply(actions)
	return verdict
}

// trackAccountLocked 记录判定期间账户的结果（连续裸 429、复试、恢复），返回这次裸 429 是否按账号证据降级与连续次数；调用方持有 mu
func (guard *egressGuard) trackAccountLocked(state *egressState, obs egressObservation, now time.Time) (bool, int) {
	switch obs.Outcome {
	case egressSuccess:
		state.periodSuccess++
	case egressBare:
		state.periodBare++
	}
	if obs.AccountID == "" {
		return false, 0
	}
	record := state.accounts[obs.AccountID]
	if record == nil {
		record = &egressAccount{}
		state.accounts[obs.AccountID] = record
	}
	if record.bared && !record.retried {
		record.retried = true
		state.retried++
	}
	switch obs.Outcome {
	case egressSuccess:
		if record.bared && !record.recovered {
			record.recovered = true
			state.recovered++
		}
		record.streak = 0
		record.succeeded = true
		return false, 0
	case egressOther:
		return false, 0
	}
	record.bared = true
	record.streak++
	if record.firstBare.IsZero() {
		record.firstBare = now
	}
	threshold, dynamic := egressDemoteThreshold(state.history.sum(now, egressJudgeWindow, -1).ratio())
	byThreshold := dynamic && record.streak >= threshold
	backstop := !record.succeeded && record.streak >= egressDemoteBackstopStreak && now.Sub(record.firstBare) >= egressDemoteBackstopSpan
	if !byThreshold && !backstop {
		return false, 0
	}
	record.demoted = true
	state.demoted++
	state.demotePending++
	if !byThreshold {
		state.demoteBackstop++
	}
	return true, record.streak
}

// egressDemoteThreshold 返回按出口比例 r 的动态降级门槛 k(r) = clamp(ceil(ln 0.01 / ln r), 3, 8)：出口按比例 r 随机拒绝时连续 k 次
// 被拒的概率 <1%。向上取整后超过 8（r 约大于 0.56）时个别账号无法与出口区分，ok 为假
func egressDemoteThreshold(ratio float64) (int, bool) {
	if ratio <= 0 {
		return egressDemoteMinStreak, true
	}
	if ratio >= 1 {
		return 0, false
	}
	threshold := int(math.Ceil(math.Log(egressDemoteConfidence)/math.Log(ratio) - 1e-9))
	if threshold > egressDemoteMaxStreak {
		return 0, false
	}
	return max(threshold, egressDemoteMinStreak), true
}

// maybeEnterLocked 在一次裸 429 之后判断出口是否进入判定（成功与其他失败只会降低比例，不必判断）；调用方持有 mu
func (guard *egressGuard) maybeEnterLocked(state *egressState, now time.Time, actions *egressActions) {
	counts := state.history.sum(now, egressJudgeWindow, -1)
	if counts.bare < egressMinBare {
		return
	}
	accounts := 0
	for accountID, at := range state.bareAccounts {
		if now.Sub(at) > egressJudgeWindow {
			delete(state.bareAccounts, accountID)
			continue
		}
		accounts++
	}
	if accounts < egressMinAccounts {
		return
	}
	ratio := counts.ratio()
	condition := ""
	switch {
	case ratio >= egressEnterRatio && counts.fresh >= egressMinFresh:
		condition = "冷号首发"
	case ratio >= egressEnterAltRatio && accounts >= egressEnterAltAccounts:
		condition = "高比例"
	default:
		return
	}
	guard.resetLocked(state, egressLimited, now)
	state.ratioEntry = ratio
	success := state.history.sum(now, time.Minute, egressLaneNormal).success
	initial, floor := guard.enableLaneLocked(state, egressLaneNormal, success, ratio, now, actions)
	actions.log("WARN",
		"出口限流判定成立 | 出口=%s | 窗口=%s | 账号=%d | 裸限流=%d | 尝试=%d | 比例=%s | 冷号首发被拒=%d | 条件=%s | 初始限速=%d/分钟 | 下限=%d/分钟",
		state.label, egressWindowText(egressJudgeWindow), accounts, counts.bare, counts.attempts(), egressPercent(ratio), counts.fresh,
		condition, initial, floor)
	actions.relax = append(actions.relax, egressRelaxJob{key: state.key, label: state.label, since: now})
}

// maybeEnableUltraLocked 在判定期间 Ultra 通道自己有足够的裸 429 证据时启用 Ultra 限速；调用方持有 mu
func (guard *egressGuard) maybeEnableUltraLocked(state *egressState, now time.Time, actions *egressActions) {
	lane := &state.lanes[egressLaneUltra]
	if lane.active || lane.suspended {
		return
	}
	counts := state.history.sum(now, egressJudgeWindow, egressLaneUltra)
	if counts.bare < egressUltraMinBare || counts.ratio() < egressUltraEnterRatio {
		return
	}
	success := state.history.sum(now, time.Minute, egressLaneUltra).success
	guard.enableLaneLocked(state, egressLaneUltra, success, counts.ratio(), now, actions)
	recent := state.history.sum(now, 30*time.Second, egressLaneUltra)
	actions.log("INFO",
		"出口限速调整 | 出口=%s | 通道=%s | 限速=%d→%d/分钟 | 原因=启用 | 30秒裸限流=%d | 30秒成功=%d | 30秒发送=%d | 排队30秒=%d",
		state.label, egressLaneName(egressLaneUltra), 0, lane.limit, recent.bare, recent.success,
		guard.lastSnap[state.key].Lanes[egressLaneUltra].Sent30s, state.history.sum(now, 30*time.Second, -1).waits)
}

// enableLaneLocked 按通道最近 60 秒的成功数启用限速：初值 max(6, 成功 ×1.25)，下限 max(6, 初值 ×0.5)，账户池从空桶开始；调用方持有 mu
func (guard *egressGuard) enableLaneLocked(
	state *egressState, lane int, success int, ratio float64, now time.Time, actions *egressActions,
) (initial int, floor int) {
	initial = max(egressMinLimit, int(math.Round(float64(success)*egressInitialFactor)))
	floor = max(egressMinLimit, int(math.Ceil(float64(initial)*egressFloorShare)))
	state.lanes[lane] = egressLaneState{
		active: true, limit: initial, initial: initial, floor: floor, ratioEntry: ratio,
		enabledAt: now, lastAdjust: now, idleSince: now,
	}
	if initial <= floor {
		state.lanes[lane].floorSince = now
	}
	guard.markDirtyLocked(state.key, lane, actions)
	return initial, floor
}

// resetLocked 清空本次判定期间的计数并转到 mode（normal、limited 或 rejected）；冷启动配额与排队数不清（在途的照常退还）。
// 启用限速的通道记为待同步（限速归零），调用方另把 actions.limits 置真；调用方持有 mu，并负责写日志
func (guard *egressGuard) resetLocked(state *egressState, mode egressMode, now time.Time) {
	for lane := range state.lanes {
		if state.lanes[lane].active {
			guard.markDirtyLocked(state.key, lane, nil)
		}
		state.lanes[lane] = egressLaneState{}
	}
	state.mode = mode
	state.since = now
	state.ratioEntry = 0
	state.accounts = make(map[string]*egressAccount)
	state.retried, state.recovered, state.demoted, state.relaxed = 0, 0, 0, 0
	state.periodSuccess, state.periodBare = 0, 0
	state.demotePending, state.demoteBackstop = 0, 0
	state.demoteLoggedAt, state.floorWarnedAt, state.fullLoggedAt = time.Time{}, time.Time{}, time.Time{}
	if mode != egressRejected {
		state.rejectedUntil, state.rejectedBy = time.Time{}, ""
	}
	guard.publishLimitedLocked()
}

// markDirtyLocked 记下通道的限速有变化，解锁后由 apply 同步到账户池；调用方持有 mu
func (guard *egressGuard) markDirtyLocked(key string, lane int, actions *egressActions) {
	if guard.dirty == nil {
		guard.dirty = make(map[egressLaneRef]struct{})
	}
	guard.dirty[egressLaneRef{key: key, lane: lane}] = struct{}{}
	if actions != nil {
		actions.limits = true
	}
}

// publishLimitedLocked 重算判定成立的出口（无锁读的快照，含测试写入的判定）；调用方持有 mu
func (guard *egressGuard) publishLimitedLocked() {
	limited := make(map[string]struct{})
	for key, state := range guard.states {
		if state.mode == egressLimited {
			limited[key] = struct{}{}
		}
	}
	for key, verdict := range guard.forced {
		if verdict.Limited {
			limited[key] = struct{}{}
		} else {
			delete(limited, key)
		}
	}
	guard.limited.Store(&limited)
}

// verdictLocked 返回出口状态在 ultra 指定的通道上的判定（不含测试写入的判定）；调用方持有 mu
func (guard *egressGuard) verdictLocked(state *egressState, ultra bool, now time.Time) egressVerdict {
	verdict := egressVerdict{Mode: state.mode, Label: state.label}
	if state.mode == egressRejected && !now.Before(state.rejectedUntil) {
		verdict.Mode = egressNormal
	}
	if verdict.Mode != egressLimited {
		return verdict
	}
	lane := state.lanes[egressLaneOf(ultra)]
	verdict.Limited = true
	verdict.Suspended = lane.suspended
	if lane.active && lane.limit > 0 {
		verdict.Throttled = true
		verdict.Limit = lane.limit
	}
	verdict.RetryAt = now.Add(guard.retryDelayLocked(state, lane))
	return verdict
}

// retryDelayLocked 按当前排队长度估计轮到的时间：clamp(60s/限速 × (排队 + 1), 5s, 60s)；不限速时 5s
func (guard *egressGuard) retryDelayLocked(state *egressState, lane egressLaneState) time.Duration {
	if !lane.active || lane.limit <= 0 {
		return egressRetryMin
	}
	delay := time.Minute / time.Duration(lane.limit) * time.Duration(state.queued+1)
	return min(max(delay, egressRetryMin), egressRetryMax)
}

// verdict 返回出口（及 ultra 指定的通道）当前的判定；没有见过的出口为零值
func (guard *egressGuard) verdict(key string, ultra bool, now time.Time) egressVerdict {
	if guard == nil {
		return egressVerdict{}
	}
	guard.mu.Lock()
	defer guard.mu.Unlock()
	if forced, ok := guard.forced[key]; ok {
		return forced
	}
	state := guard.states[key]
	if state == nil {
		return egressVerdict{}
	}
	return guard.verdictLocked(state, ultra, now)
}

// anyLimited 返回是否有出口判定成立
func (guard *egressGuard) anyLimited() bool {
	return len(guard.limitedKeys()) > 0
}

// limitedKeys 返回判定成立的出口；没有时为 nil。返回的表只读（无锁快照，判定变化时整份替换）
func (guard *egressGuard) limitedKeys() map[string]struct{} {
	if guard == nil {
		return nil
	}
	limited := guard.limited.Load()
	if limited == nil || len(*limited) == 0 {
		return nil
	}
	return *limited
}

// catalogPaused 返回出口上的模型目录后台同步是否暂停（判定成立期间暂停）
func (guard *egressGuard) catalogPaused(key string) bool {
	_, limited := guard.limitedKeys()[key]
	return limited
}

// withoutLimitedStandby 出口限流期间收窄转写、Live、Veo 的候选：这些路径不经 acquireLease（那里已剔除受限出口上的备用账户），
// 由账户池在候选里按 ID 顺序（轮询时旋转）选号，不认调用方排好的“热 Worker 在前”的顺序。备用账号占绝大多数（约 1.2 万对 150 个驻留），
// 几乎每次都选中要冷启动的备用账号，在受限出口上被冷启动闸门暂停后按号池繁忙 429 返回，换号也一样，驻留 Worker 空闲时这些端点
// 整个限流期间都不可用；候选上万时等出口令牌的选号每次唤醒都在账户池锁内扫一遍全部候选。
// 还有热 Worker 候选（含忙碌的）时去掉受限出口上的备用账户，只留热 Worker 与未受限出口上的备用账户；一个热候选都没有时照旧保留，
// 交给冷启动闸门（低于常驻数的补位照常放行）。返回是否剔除了账户
func (service *trackedService) withoutLimitedStandby(groups *aistudio.AccountCandidateGroups) bool {
	limited := service.egress.limitedKeys()
	if len(limited) == 0 || len(groups.WarmReady)+len(groups.WarmAvailable)+len(groups.WarmBusy) == 0 {
		return false
	}
	var removedReady, removedBusy []string
	groups.StandbyReady, removedReady = service.pool.FilterAccountsByEgress(groups.StandbyReady, limited)
	groups.StandbyBusy, removedBusy = service.pool.FilterAccountsByEgress(groups.StandbyBusy, limited)
	return len(removedReady)+len(removedBusy) > 0
}

// videoCandidates 出口限流期间给不固定账户的 Veo 选号限定候选（Veo 原本不限定候选，由账户池在全部账户里选号，见 withoutLimitedStandby）：
// 热 Worker 在前，去掉受限出口上的备用账户。没有出口受限、分类失败或没有剔除任何账户时返回 nil，照旧不限定候选
func (service *trackedService) videoCandidates(ctx context.Context, model string) []string {
	if !service.egress.anyLimited() {
		return nil
	}
	modelID := strings.TrimPrefix(strings.TrimSpace(model), "models/")
	groups, err := service.pool.ClassifyCandidates(ctx, aistudio.AccountSelection{
		ModelID: modelID, Method: "predictLongRunning", Pool: aistudio.PoolScopeFromContext(ctx),
	}, service.workers.WarmAccountIDs())
	if err != nil || !service.withoutLimitedStandby(&groups) {
		return nil
	}
	return slices.Concat(groups.WarmReady, groups.WarmAvailable, groups.WarmBusy, groups.StandbyReady, groups.StandbyBusy)
}

// admitColdStart 为冷启动闸门（在 ensureWorker 调用）；refillLimit 为补位并发上限（Worker 管理器按 WARM_STARTUP_CONCURRENCY 算好传入）。
// 出口没有判定成立时直接放行、不计数；成立时按路径：补位只受并发约束（不计每分钟配额，补位不增加生成发送，发送量已由令牌桶封顶），
// 补覆盖与补位共用并发、另受每分钟配额与同模型一个在途，替换与超出常驻暂停。配额按（出口，通道）分开计
func (guard *egressGuard) admitColdStart(key string, ultra bool, modelID string, path coldStartPath, refillLimit int) (release func(started bool), ok bool) {
	noop := func(bool) {}
	if guard == nil {
		return noop, true
	}
	if _, limited := guard.limitedKeys()[key]; !limited {
		return noop, true
	}
	now := guard.clock()
	var actions egressActions
	guard.mu.Lock()
	state := guard.stateLocked(key)
	lane := egressLaneOf(ultra)
	cold := state.cold[lane]
	cold.prune(now)
	refillLimit = max(refillLimit, egressRefillConcurrencyMin)
	skip := ""
	switch path {
	case coldStartReplace:
		skip = "替换"
	case coldStartExpand:
		skip = "超出常驻"
	case coldStartCoverage:
		switch {
		case cold.starting >= refillLimit:
			skip = "补位并发已满"
		case len(cold.coverage) >= egressCoveragePerMinute || cold.coverageOpening[modelID] > 0:
			skip = "补覆盖配额已满"
		}
	default:
		if cold.starting >= refillLimit {
			skip = "补位并发已满"
			cold.deferred = append(cold.deferred, now)
		}
	}
	if skip != "" {
		guard.noteSkipLocked(state, lane, path, skip, now, &actions)
		guard.mu.Unlock()
		guard.apply(actions)
		return noop, false
	}
	cold.starting++
	cold.started = append(cold.started, now)
	if path == coldStartCoverage {
		cold.coverage = append(cold.coverage, now)
		cold.coverageOpening[modelID]++
	}
	guard.mu.Unlock()
	var once sync.Once
	return func(bool) {
		once.Do(func() {
			guard.mu.Lock()
			defer guard.mu.Unlock()
			cold.starting = max(cold.starting-1, 0)
			if path == coldStartCoverage {
				if cold.coverageOpening[modelID] <= 1 {
					delete(cold.coverageOpening, modelID)
				} else {
					cold.coverageOpening[modelID]--
				}
			}
		})
	}, true
}

// prune 丢掉一分钟以前的冷启动记录；调用方持有判定器的锁
func (cold *egressColdStart) prune(now time.Time) {
	cold.started = pruneEgressTimes(cold.started, now, time.Minute)
	cold.coverage = pruneEgressTimes(cold.coverage, now, time.Minute)
	cold.deferred = pruneEgressTimes(cold.deferred, now, time.Minute)
}

// pruneEgressTimes 丢掉 window 以前的时刻（按时间顺序追加）
func pruneEgressTimes(times []time.Time, now time.Time, window time.Duration) []time.Time {
	drop := 0
	for drop < len(times) && now.Sub(times[drop]) > window {
		drop++
	}
	if drop == 0 {
		return times
	}
	return append(times[:0], times[drop:]...)
}

// noteSkipLocked 记一次被暂停的冷启动：同出口同路径 10 分钟写一条（持久）；调用方持有 mu
func (guard *egressGuard) noteSkipLocked(state *egressState, lane int, path coldStartPath, reason string, now time.Time, actions *egressActions) {
	skip := state.skips[path]
	if skip == nil {
		skip = &egressSkipLog{}
		state.skips[path] = skip
	}
	skip.count++
	if !skip.loggedAt.IsZero() && now.Sub(skip.loggedAt) < egressSkipLogEvery {
		return
	}
	actions.log("INFO", "出口限流暂停冷启动 | 出口=%s | 通道=%s | 路径=%s | 10分钟内跳过=%d",
		state.label, egressLaneName(lane), reason, skip.count)
	skip.count = 0
	skip.loggedAt = now
}

// enterEgressWait 进入“出口限速”排队（判断限速是否起作用、状态里的排队数），返回离开时调用的函数（多次调用只算一次）。
// 判定成立期间第一次排队时写“出口每分钟请求已满”（同出口 10 分钟一条）
func (guard *egressGuard) enterEgressWait(key string) (leave func()) {
	if guard == nil {
		return func() {}
	}
	now := guard.clock()
	var actions egressActions
	guard.mu.Lock()
	state := guard.stateLocked(key)
	state.queued++
	state.lastWaitAt = now
	state.history.slot(now).lanes[egressLaneNormal].waits++
	if state.mode == egressLimited && (state.fullLoggedAt.IsZero() || now.Sub(state.fullLoggedAt) >= egressFullLogEvery) {
		lanes := make([]string, 0, 2)
		limit := 0
		for lane := range state.lanes {
			if state.lanes[lane].active {
				lanes = append(lanes, egressLaneName(lane))
				if limit == 0 {
					limit = state.lanes[lane].limit
				}
			}
		}
		if len(lanes) > 0 {
			state.fullLoggedAt = now
			actions.log("INFO", "出口每分钟请求已满 | 出口=%s | 通道=%s | 限速=%d/分钟 | 令牌释放前新请求按“出口限速”排队",
				state.label, strings.Join(lanes, "、"), limit)
		}
	}
	guard.mu.Unlock()
	guard.apply(actions)
	var once sync.Once
	return func() {
		once.Do(func() {
			guard.mu.Lock()
			state.queued = max(state.queued-1, 0)
			state.lastWaitAt = guard.clock()
			guard.mu.Unlock()
		})
	}
}

// noteLocalReject 记录一次本地返回的出口限速 429（排队超限或出口限流换号已达上限）
func (guard *egressGuard) noteLocalReject(key string, now time.Time) {
	if guard == nil {
		return
	}
	if now.IsZero() {
		now = guard.clock()
	}
	guard.mu.Lock()
	defer guard.mu.Unlock()
	state := guard.stateLocked(key)
	state.localRejects = append(pruneEgressTimes(state.localRejects, now, egressLocalRejectWindow), now)
}

// run 为判定器的计时器（独立的 5 秒计时器，与 keepWarm 一起在 Start 里启动，不放进 keepWarmPass），ctx 结束时返回
func (guard *egressGuard) run(ctx context.Context) {
	if guard == nil || ctx == nil {
		return
	}
	interval := guard.tickEvery
	if interval <= 0 {
		interval = egressTick
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			guard.tick(guard.clock())
		}
	}
}

// tick 为一次计时：限速调整（AIMD）、限速有效性检查、安全阀、降级汇总日志、解除与撤销到期
func (guard *egressGuard) tick(now time.Time) {
	if guard == nil {
		return
	}
	guard.ticks.Add(1)
	snapshot := guard.poolSnapshot()
	var actions egressActions
	guard.mu.Lock()
	guard.lastSnap = snapshot
	if day := guard.quotaDayLocked(now); day != guard.seenDay {
		for accountID, seenDay := range guard.seen {
			if seenDay != day {
				delete(guard.seen, accountID)
			}
		}
		guard.seenDay = day
	}
	for key, state := range guard.states {
		guard.tickStateLocked(state, snapshot[key], now, &actions)
	}
	guard.mu.Unlock()
	guard.apply(actions)
}

// poolSnapshot 取账户池侧各出口的计数（一次加锁，O(出口数)），按键索引；没有账户池时为空
func (guard *egressGuard) poolSnapshot() map[string]aistudio.EgressStatus {
	if guard.pool == nil {
		return nil
	}
	statuses := guard.pool.EgressSnapshot()
	snapshot := make(map[string]aistudio.EgressStatus, len(statuses))
	for _, status := range statuses {
		snapshot[status.Key] = status
	}
	return snapshot
}

// tickStateLocked 为一个出口的一次计时；调用方持有 mu
func (guard *egressGuard) tickStateLocked(state *egressState, pool aistudio.EgressStatus, now time.Time, actions *egressActions) {
	for accountID, at := range state.bareAccounts {
		if now.Sub(at) > egressJudgeWindow {
			delete(state.bareAccounts, accountID)
		}
	}
	state.localRejects = pruneEgressTimes(state.localRejects, now, egressLocalRejectWindow)
	for _, cold := range state.cold {
		cold.prune(now)
	}
	switch state.mode {
	case egressRejected:
		if !now.Before(state.rejectedUntil) {
			guard.resetLocked(state, egressNormal, now)
			actions.log("INFO", "出口限流判定恢复 | 出口=%s | 原因=撤销到期", state.label)
		}
		return
	case egressNormal:
		return
	}
	for lane := range state.lanes {
		if state.lanes[lane].active {
			guard.adjustLaneLocked(state, lane, pool.Lanes[lane], now, actions)
		}
	}
	guard.floorWarnLocked(state, now, actions)
	if state.demotePending > 0 && (state.demoteLoggedAt.IsZero() || now.Sub(state.demoteLoggedAt) >= egressDemoteLogEvery) {
		threshold := "无"
		if value, ok := egressDemoteThreshold(state.history.sum(now, egressJudgeWindow, -1).ratio()); ok {
			threshold = fmt.Sprintf("%d次", value)
		}
		actions.log("WARN", "出口限流：账号按账号级失败冷却 | 出口=%s | 新增=%d | 合计=%d | 门槛=%s | 兜底=%d",
			state.label, state.demotePending, state.demoted, threshold, state.demoteBackstop)
		state.demotePending, state.demoteBackstop = 0, 0
		state.demoteLoggedAt = now
	}
	// 安全阀 A：出口还有成功，被拒的账号再试却都不成功，更像账号当天额度用完、不是出口限流
	if now.Sub(state.since) >= egressValveHold && state.retried >= egressValveRetried && state.recovered == 0 &&
		state.history.sum(now, egressBuckets*egressBucket, -1).success >= egressValveMinSuccess {
		guard.rejectLocked(state, now, "安全阀", "复试账号零成功", actions)
		return
	}
	if now.Sub(state.since) < egressMinHold || state.history.sum(now, egressJudgeWindow, -1).ratio() >= egressExitRatio {
		return
	}
	for lane := range state.lanes {
		laneState := state.lanes[lane]
		if laneState.active && (laneState.idleSince.IsZero() || now.Sub(laneState.idleSince) < egressExitIdle) {
			return
		}
	}
	final := 0
	if state.lanes[egressLaneNormal].active {
		final = state.lanes[egressLaneNormal].limit
	}
	actions.log("INFO",
		"出口限流解除 | 出口=%s | 持续=%s | 最终限速=%d/分钟 | 期间成功=%d | 期间裸限流=%d | 复试账号=%d | 恢复账号=%d | 降级账号=%d",
		state.label, now.Sub(state.since).Round(time.Second), final, state.periodSuccess, state.periodBare,
		state.retried, state.recovered, state.demoted)
	guard.resetLocked(state, egressNormal, now)
	actions.limits = true
}

// adjustLaneLocked 为一个启用限速的通道做 AIMD 与限速有效性检查；调用方持有 mu
func (guard *egressGuard) adjustLaneLocked(state *egressState, lane int, pool aistudio.EgressLaneStatus, now time.Time, actions *egressActions) {
	laneState := &state.lanes[lane]
	binding := (laneState.limit > 0 && float64(pool.Sent1m) >= egressBindingShare*float64(laneState.limit)) || state.queued > 0 ||
		(!state.lastWaitAt.IsZero() && now.Sub(state.lastWaitAt) <= egressBindingWaitWindow)
	if binding {
		if laneState.bindingSince.IsZero() {
			laneState.bindingSince = now
		}
		laneState.idleSince = time.Time{}
	} else {
		if laneState.idleSince.IsZero() {
			laneState.idleSince = now
		}
		laneState.bindingSince = time.Time{}
	}
	recent := state.history.sum(now, 30*time.Second, lane)
	minute := state.history.sum(now, time.Minute, lane)
	waits := state.history.sum(now, 30*time.Second, -1).waits
	adjust := func(next int, reason string, level string) {
		previous := laneState.limit
		laneState.limit = next
		laneState.lastAdjust = now
		guard.markDirtyLocked(state.key, lane, actions)
		actions.log(level,
			"出口限速调整 | 出口=%s | 通道=%s | 限速=%d→%d/分钟 | 原因=%s | 30秒裸限流=%d | 30秒成功=%d | 30秒发送=%d | 排队30秒=%d",
			state.label, egressLaneName(lane), previous, next, reason, recent.bare, recent.success, pool.Sent30s, waits)
	}
	switch {
	case !laneState.checkAt.IsZero() && now.Sub(laneState.checkAt) >= egressDecreaseCheck:
		// 降速回退：降速后成功数下降，说明拒绝与发送速率无关，少发只会少成功
		if laneState.checkBefore >= egressDecreaseMinSuccess &&
			float64(minute.success) < egressDecreaseRevertShare*float64(laneState.checkBefore) {
			adjust(laneState.checkPrev, "降速回退", "WARN")
			laneState.lockUntil = now.Add(egressDecreaseLock)
		}
		laneState.checkAt = time.Time{}
	case now.Sub(laneState.lastAdjust) >= egressDecreaseQuiet && !now.Before(laneState.lockUntil) &&
		recent.bare >= egressDecreaseMinBare && recent.ratio() >= egressDecreaseMinRatio:
		next := max(laneState.floor, int(math.Floor(float64(laneState.limit)*egressDecreaseFactor)))
		if next < laneState.limit {
			laneState.checkAt, laneState.checkBefore, laneState.checkPrev = now, minute.success, laneState.limit
			laneState.lastDecrease = now
			adjust(next, "降速", "WARN")
		}
	case binding && (recent.bare == 0 || recent.ratio() < egressSlowStepRatio) &&
		pool.Sent30s >= max(egressIncreaseMinSent, laneState.limit/4) &&
		now.Sub(laneState.lastDecrease) >= egressIncreaseAfterDecrease && now.Sub(laneState.lastIncrease) >= egressIncreaseEvery:
		laneState.lastIncrease = now
		if recent.bare == 0 {
			adjust(int(math.Ceil(float64(laneState.limit)*egressFastFactor)), "快速提速", "INFO")
		} else {
			adjust(laneState.limit+max(1, int(math.Ceil(float64(laneState.limit)*egressSlowStepRatio))), "提速", "INFO")
		}
	}
	if laneState.limit <= laneState.floor {
		if laneState.floorSince.IsZero() {
			laneState.floorSince = now
		}
	} else {
		laneState.floorSince = time.Time{}
	}
	// 限速有效性检查：限速一直在起作用，比例却没有比进入时改善 egressLimiterImproveMin（又低于安全阀 B 的比例）时，拒绝与发送速率无关，
	// 停止限速（本次判定期间不再启用），其余保护保留，限流类换号放宽到 egressSuspendedRateSwitches。出口能力固定时比例会从 0.83–0.85
	// 降到约 0.2，不会触发
	if now.Sub(laneState.enabledAt) < egressLimiterJudgeAfter || !binding || laneState.bindingSince.IsZero() ||
		now.Sub(laneState.bindingSince) < egressLimiterJudgeSpan {
		return
	}
	ratio := state.history.sum(now, egressLimiterJudgeSpan, lane).ratio()
	if ratio < laneState.ratioEntry-egressLimiterImproveMin || ratio >= egressValveRatio {
		return
	}
	actions.log("WARN",
		"出口限速无效，停止限速 | 出口=%s | 通道=%s | 进入时比例=%s | 限速后比例=%s | 成功1分钟=%d | 限流类换号放宽到 %d 次",
		state.label, egressLaneName(lane), egressPercent(laneState.ratioEntry), egressPercent(ratio), minute.success,
		egressSuspendedRateSwitches)
	laneState.active, laneState.suspended = false, true
	guard.markDirtyLocked(state.key, lane, actions)
}

// floorWarnLocked 为安全阀 B：普通通道限速停在下限 ≥10 分钟、这 10 分钟比例仍 ≥0.9 时只告警（同出口 10 分钟一条），判定保持。
// 撤销判定会交回逐级冷却，而原号池级判定要求零成功，零星成功就不触发，最严重的出口限流反而退回风暴路径；
// 这种情况下驻留账号的清理靠按账号降级的兜底条件。调用方持有 mu
func (guard *egressGuard) floorWarnLocked(state *egressState, now time.Time, actions *egressActions) {
	lane := state.lanes[egressLaneNormal]
	if !lane.active || lane.floorSince.IsZero() || now.Sub(lane.floorSince) < egressFloorWarnAfter {
		return
	}
	if !state.floorWarnedAt.IsZero() && now.Sub(state.floorWarnedAt) < egressFloorWarnAfter {
		return
	}
	counts := state.history.sum(now, egressFloorWarnAfter, egressLaneNormal)
	if counts.ratio() < egressValveRatio {
		return
	}
	state.floorWarnedAt = now
	actions.log("WARN", "出口几乎完全被拒 | 出口=%s | 限速=%d/分钟（下限） | 10分钟比例=%s | 10分钟成功=%d | 判定保持，不交回逐级冷却",
		state.label, lane.limit, egressPercent(counts.ratio()), counts.success)
}

// rejectLocked 撤销判定 egressRejectFor（安全阀 A 或管理操作）：两条通道限速归零，期间交回逐级冷却与原号池级判定、不再进入；调用方持有 mu
func (guard *egressGuard) rejectLocked(state *egressState, now time.Time, by string, reason string, actions *egressActions) {
	held := time.Duration(0)
	if state.mode == egressLimited {
		held = now.Sub(state.since)
	}
	guard.resetLocked(state, egressRejected, now)
	state.rejectedUntil = now.Add(egressRejectFor)
	state.rejectedBy = by
	actions.limits = true
	actions.log("WARN", "出口限流判定撤销 | 出口=%s | 原因=%s | 持续=%s | %d分钟内不再判定",
		state.label, reason, held.Round(time.Second), int(egressRejectFor/time.Minute))
}

// apply 执行锁内算出的动作：限速同步到账户池、写日志、启动撤回逐级冷却
func (guard *egressGuard) apply(actions egressActions) {
	if actions.limits {
		guard.syncLimits()
	}
	for _, entry := range actions.logs {
		guard.emit(entry)
	}
	for _, job := range actions.relax {
		guard.startRelax(job)
	}
}

// syncLimits 把有变化的通道按最新判定写入账户池。applyMu 串行化写入：每次都在判定器锁内重读最新的限速，先算出的旧值不会覆盖
// 后算出的新值；另一个协程正在同步时直接返回，由它在释放后复查待同步的通道
func (guard *egressGuard) syncLimits() {
	for {
		if !guard.applyMu.TryLock() {
			return
		}
		for {
			guard.mu.Lock()
			pending := guard.dirty
			guard.dirty = nil
			limits := make(map[egressLaneRef]int, len(pending))
			for ref := range pending {
				limit := 0
				if state := guard.states[ref.key]; state != nil && state.lanes[ref.lane].active {
					limit = state.lanes[ref.lane].limit
				}
				limits[ref] = limit
			}
			guard.mu.Unlock()
			if len(limits) == 0 {
				break
			}
			if guard.pool != nil {
				for ref, limit := range limits {
					guard.pool.SetEgressRateLimit(ref.key, aistudio.EgressLane(ref.lane), limit)
				}
			}
		}
		guard.applyMu.Unlock()
		guard.mu.Lock()
		pending := len(guard.dirty) > 0
		guard.mu.Unlock()
		if !pending {
			return
		}
	}
}

// emit 写一条判定器日志：持久行走 logDurable（控制台队列满时也不丢）
func (guard *egressGuard) emit(entry egressLogEntry) {
	if guard.logf != nil {
		guard.logf(entry)
		return
	}
	if guard.requests == nil {
		return
	}
	if entry.durable {
		guard.requests.logDurable("service", entry.level, entry.message)
		return
	}
	guard.requests.log("service", entry.level, entry.message)
}

// startRelax 在后台撤回出口上第 2 档以上的逐级冷却（并发 egressRelaxConcurrency），缩到现在 + 判定期间的短冷却：
// 冷却缩短后登记的淘汰与排空随 stillCooling 为假自动撤销（见 worker_retire.go）。每日限额、分钟限额、403 与按账号降级的冷却不动
func (guard *egressGuard) startRelax(job egressRelaxJob) {
	if guard.pool == nil {
		return
	}
	guard.relaxWG.Add(1)
	go func() {
		defer guard.relaxWG.Done()
		startedAt := time.Now()
		accounts := guard.pool.RateLadderAccounts(job.key, egressRelaxMinLevel)
		until := time.Now().Add(guard.limitedCooldown())
		var relaxed, failed atomic.Int64
		work := make(chan string)
		var workers sync.WaitGroup
		for range min(egressRelaxConcurrency, max(len(accounts), 1)) {
			workers.Add(1)
			go func() {
				defer workers.Done()
				for accountID := range work {
					changed, err := guard.pool.RelaxRateLimitCooldowns(accountID, until, egressRelaxMinLevel)
					switch {
					case err != nil:
						failed.Add(1)
					case changed > 0:
						relaxed.Add(1)
					}
				}
			}()
		}
		for _, accountID := range accounts {
			work <- accountID
		}
		close(work)
		workers.Wait()
		guard.mu.Lock()
		if state := guard.states[job.key]; state != nil && state.mode == egressLimited && state.since.Equal(job.since) {
			state.relaxed += int(relaxed.Load())
		}
		guard.mu.Unlock()
		guard.emit(egressLogEntry{durable: true, level: "INFO", message: fmt.Sprintf(
			"出口限流：撤回逐级冷却 | 出口=%s | 账号=%d | 失败=%d | 耗时=%s",
			job.label, relaxed.Load(), failed.Load(), time.Since(startedAt).Round(time.Millisecond),
		)})
	}()
}

// status 返回各出口的判定与限速（状态接口）：账户池侧的计数来自一次 EgressSnapshot（O(出口数)），其余来自判定器；
// 受限优先、再按标签排序。只有直连一个出口且一直 normal 时也输出（侧栏只显示非 normal 的）
func (guard *egressGuard) status() []api.AdminEgress {
	if guard == nil {
		return nil
	}
	snapshot := guard.poolSnapshot()
	now := guard.clock()
	guard.mu.Lock()
	defer guard.mu.Unlock()
	result := make([]api.AdminEgress, 0, max(len(snapshot), len(guard.states)))
	for key, pool := range snapshot {
		result = append(result, guard.egressStatusLocked(guard.states[key], pool, now))
	}
	for key, state := range guard.states {
		if _, listed := snapshot[key]; listed {
			continue
		}
		result = append(result, guard.egressStatusLocked(state, aistudio.EgressStatus{Key: key, ID: state.id, Label: state.label}, now))
	}
	if len(result) == 0 {
		return nil
	}
	rank := map[string]int{"limited": 0, "rejected": 1, "normal": 2}
	slices.SortFunc(result, func(left, right api.AdminEgress) int {
		return cmp.Or(cmp.Compare(rank[left.Mode], rank[right.Mode]), cmp.Compare(left.Label, right.Label), cmp.Compare(left.ID, right.ID))
	})
	return result
}

// egressStatusLocked 投影一个出口的状态；state 为 nil 时只有账户池侧计数；调用方持有 mu
func (guard *egressGuard) egressStatusLocked(state *egressState, pool aistudio.EgressStatus, now time.Time) api.AdminEgress {
	entry := api.AdminEgress{ID: pool.ID, Label: pool.Label, Mode: egressNormal.String(), Limiter: "off", Lanes: []api.AdminEgressLane{}}
	if entry.ID == "" {
		entry.ID = aistudio.EgressID(pool.Key)
	}
	if entry.Label == "" {
		entry.Label = aistudio.EgressLabel(pool.Key)
	}
	for lane := range pool.Lanes {
		counts := pool.Lanes[lane]
		item := api.AdminEgressLane{
			Lane: "normal", Accounts: counts.Accounts, LimitRPM: counts.Limit, Burst: counts.Burst, Sent1m: counts.Sent1m,
			PrimarySent1m: counts.PrimarySent1m, OtherSent1m: counts.OtherSent1m, Pending: counts.Pending,
		}
		if lane == egressLaneUltra {
			item.Lane = "ultra"
		}
		include := lane == egressLaneNormal || counts.Accounts > 0 || counts.Limit > 0
		if state != nil {
			laneState := state.lanes[lane]
			if laneState.active {
				item.LimitRPM = laneState.limit
			}
			cold := state.cold[lane]
			cold.prune(now)
			item.ColdStarting, item.ColdStarted1m, item.RefillDeferred1m = cold.starting, len(cold.started), len(cold.deferred)
			include = include || laneState.active || laneState.suspended || cold.starting > 0 || len(cold.started) > 0
		}
		entry.Accounts += counts.Accounts
		if include {
			entry.Lanes = append(entry.Lanes, item)
		}
	}
	if state == nil {
		return entry
	}
	mode := state.mode
	if mode == egressRejected && !now.Before(state.rejectedUntil) {
		mode = egressNormal
	}
	entry.Mode = mode.String()
	minute := state.history.sum(now, time.Minute, -1)
	window := state.history.sum(now, egressJudgeWindow, -1)
	entry.Success1m, entry.Bare1m, entry.Other1m = minute.success, minute.bare, minute.other
	entry.Ratio2m = math.Round(window.ratio()*1000) / 1000
	entry.FreshRejects2m = window.fresh
	entry.Queued = state.queued
	for _, at := range state.localRejects {
		if now.Sub(at) <= egressLocalRejectWindow {
			entry.LocalRejects1m++
		}
	}
	for _, laneState := range state.lanes {
		switch {
		case laneState.active:
			entry.Limiter = "active"
		case laneState.suspended && entry.Limiter != "active":
			entry.Limiter = "suspended"
		}
	}
	switch mode {
	case egressLimited:
		entry.Since = state.since
		entry.RatioEntry = math.Round(state.ratioEntry*1000) / 1000
		entry.Retried, entry.Recovered, entry.Demoted, entry.Relaxed = state.retried, state.recovered, state.demoted, state.relaxed
		entry.RetryAt = now.Add(guard.retryDelayLocked(state, state.lanes[egressLaneNormal]))
	case egressRejected:
		entry.Since = state.since
		entry.RejectedUntil = state.rejectedUntil
		entry.RejectedBy = state.rejectedBy
	}
	return entry
}

// withdraw 为管理动作：撤销出口的判定 30 分钟（立即转 rejected，两条通道限速归零）。只改内存，不落盘，也不是配置项
func (guard *egressGuard) withdraw(id string, now time.Time) error {
	if guard == nil {
		return egressNotFoundError{id: id}
	}
	if now.IsZero() {
		now = guard.clock()
	}
	key, ok := guard.lookupEgress(id)
	if !ok {
		return egressNotFoundError{id: id}
	}
	var actions egressActions
	guard.mu.Lock()
	state := guard.stateLocked(key)
	guard.rejectLocked(state, now, "管理操作", "管理操作", &actions)
	guard.mu.Unlock()
	guard.apply(actions)
	return nil
}

// resume 为管理动作：清除撤销，回到 normal（可以重新进入判定）；没有撤销时不做任何事
func (guard *egressGuard) resume(id string, now time.Time) error {
	if guard == nil {
		return egressNotFoundError{id: id}
	}
	if now.IsZero() {
		now = guard.clock()
	}
	key, ok := guard.lookupEgress(id)
	if !ok {
		return egressNotFoundError{id: id}
	}
	var actions egressActions
	guard.mu.Lock()
	state := guard.stateLocked(key)
	if state.mode == egressRejected {
		guard.resetLocked(state, egressNormal, now)
		actions.log("INFO", "出口限流判定恢复 | 出口=%s | 原因=管理操作", state.label)
	}
	guard.mu.Unlock()
	guard.apply(actions)
	return nil
}

// lookupEgress 按出口编号找出口键：先找判定器见过的，再找账户池里的出口（还没有任何结果的出口同样可以撤销）
func (guard *egressGuard) lookupEgress(id string) (string, bool) {
	id = strings.TrimSpace(id)
	guard.mu.Lock()
	key, ok := guard.byID[id]
	guard.mu.Unlock()
	if ok || id == "" || guard.pool == nil {
		return key, ok
	}
	for _, status := range guard.pool.EgressSnapshot() {
		if status.ID == id || aistudio.EgressID(status.Key) == id {
			return status.Key, true
		}
	}
	return "", false
}

// forceVerdictForTest 只给包内测试：写入出口的判定，此后 verdict 对该出口返回它；Limited 时 anyLimited、limitedKeys、
// catalogPaused 与冷启动闸门同样按判定成立处理
func (guard *egressGuard) forceVerdictForTest(key string, verdict egressVerdict) {
	if guard == nil {
		return
	}
	guard.mu.Lock()
	defer guard.mu.Unlock()
	if guard.forced == nil {
		guard.forced = make(map[string]egressVerdict)
	}
	guard.forced[key] = verdict
	guard.publishLimitedLocked()
}

// egressNotFoundError 表示管理动作引用的出口编号不存在（管理接口返回 404）
type egressNotFoundError struct {
	id string
}

func (err egressNotFoundError) Error() string {
	return fmt.Sprintf("出口 %s 不存在", err.id)
}

// HTTPStatus 让管理接口按 404 返回
func (egressNotFoundError) HTTPStatus() int {
	return http.StatusNotFound
}
