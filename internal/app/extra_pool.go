package app

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/api"
	"github.com/Mag1cFall/AIStudio2API/internal/config"
)

// 补充池（额外浏览器号池，EXTRA_POOL）在 app 层的部分
//
// 账户属于哪个号池（家分区）完全不动：补充池由一批带“补充池”标记的 Worker（有自己的常驻数与峰值）与一条不阻塞的借用选号通道组成。
// 每个账户最多一个已发布的 Worker，只计入一处：带标记的计入补充池，其余照旧计入家分区。主分区的各个循环（预热、淘汰、轮换、清扫、
// 回收、替换、平均调用）读的是叠加视图（slotMembers）：带标记的账户记为 PoolScopeExtra，其余同账户池的分区快照；没有标记时就是
// 账户池的快照本身，与没有补充池时完全相同。
// 设置（extraPoolSettings）在装配与每次热更新时写入，账户池里的补充池规则（权益掩码、默认地址借付费模型）经 SetExtraPoolRules 同步

// extraEntryMin 为账户进入补充池要求的冷却剩余：与长冷却口径（Worker 淘汰与轮换）的门槛相同，两处必须一起改
const extraEntryMin = rotationMinRemaining

// extraPoolSettings 为补充池的生效设置（EXTRA_*），装配与热更新时整体替换
type extraPoolSettings struct {
	enabled      bool
	mode         string
	defaultPaid  bool
	includeUltra bool
	warm         int
	max          int
}

// extraSettingsOf 返回配置里的补充池设置
func extraSettingsOf(cfg config.Config) extraPoolSettings {
	mode := config.NormalizeExtraPoolMode(cfg.ExtraPoolMode)
	if mode == "" {
		mode = config.ExtraPoolModeShare
	}
	return extraPoolSettings{
		enabled: cfg.ExtraPool, mode: mode, defaultPaid: cfg.ExtraPoolDefaultPaid, includeUltra: cfg.ExtraPoolIncludeUltra,
		warm: cfg.ExtraWarmWorkerLimit, max: cfg.ExtraMaxActiveWorkers,
	}
}

// extraRulesOf 返回配置里属于账户池的补充池规则
func extraRulesOf(cfg config.Config) aistudio.ExtraPoolRules {
	return aistudio.ExtraPoolRules{Enabled: cfg.ExtraPool, IncludeUltra: cfg.ExtraPoolIncludeUltra, DefaultPaid: cfg.ExtraPoolDefaultPaid}
}

// setExtraSettings 写入补充池的生效设置（原子替换，读取方无锁）
func (manager *accountWorkerManager) setExtraSettings(settings extraPoolSettings) {
	manager.extraSettings.Store(&settings)
}

// extraSettingsValue 返回补充池的生效设置；从没写过（测试直接构造的管理器）时为关闭
func (manager *accountWorkerManager) extraSettingsValue() extraPoolSettings {
	if settings := manager.extraSettings.Load(); settings != nil {
		return *settings
	}
	return extraPoolSettings{}
}

// extraCapacity 返回补充池的常驻数与峰值数：补充池关闭时都为 0（不再启动、不再接手，空闲的补充池 Worker 照常被回收）
func (manager *accountWorkerManager) extraCapacity() (int, int) {
	settings := manager.extraSettingsValue()
	if !settings.enabled {
		return 0, 0
	}
	return settings.warm, settings.max
}

// extraSettingsSummary 为补充池设置的日志字段：开启、方式、默认地址借付费模型、是否含 Ultra、常驻与峰值
func extraSettingsSummary(settings extraPoolSettings) string {
	mode := "分担"
	if settings.mode == config.ExtraPoolModeOverflow {
		mode = "补充"
	}
	return fmt.Sprintf("开启=%s | 方式=%s | 默认地址借付费模型=%s | 含 Ultra=%s | 常驻 Worker=%d | 峰值 Worker=%d",
		yesNo(settings.enabled), mode, yesNo(settings.defaultPaid), yesNo(settings.includeUltra), settings.warm, settings.max)
}

// yesNo 返回布尔值的中文写法
func yesNo(value bool) string {
	if value {
		return "是"
	}
	return "否"
}

// extraConfigFields 为补充池的 6 个配置项
type extraConfigFields struct {
	ExtraPool             bool
	ExtraPoolMode         string
	ExtraPoolDefaultPaid  bool
	ExtraPoolIncludeUltra bool
	ExtraWarmWorkerLimit  int
	ExtraMaxActiveWorkers int
}

// extraSettingsFromInput 合并页面提交的补充池设置：没有提交的字段（旧版页面）沿用 base；使用方式按规范写法（小写）
func extraSettingsFromInput(value api.RuntimeConfig, base config.Config) extraConfigFields {
	fields := extraConfigFields{
		ExtraPool: base.ExtraPool, ExtraPoolMode: config.NormalizeExtraPoolMode(base.ExtraPoolMode),
		ExtraPoolDefaultPaid: base.ExtraPoolDefaultPaid, ExtraPoolIncludeUltra: base.ExtraPoolIncludeUltra,
		ExtraWarmWorkerLimit: base.ExtraWarmWorkerLimit, ExtraMaxActiveWorkers: base.ExtraMaxActiveWorkers,
	}
	if value.ExtraPool != nil {
		fields.ExtraPool = *value.ExtraPool
	}
	if value.ExtraPoolMode != nil {
		fields.ExtraPoolMode = config.NormalizeExtraPoolMode(*value.ExtraPoolMode)
	}
	if value.ExtraPoolDefaultPaid != nil {
		fields.ExtraPoolDefaultPaid = *value.ExtraPoolDefaultPaid
	}
	if value.ExtraPoolIncludeUltra != nil {
		fields.ExtraPoolIncludeUltra = *value.ExtraPoolIncludeUltra
	}
	if value.ExtraWarmWorkerLimit != nil {
		fields.ExtraWarmWorkerLimit = *value.ExtraWarmWorkerLimit
	}
	if value.ExtraMaxActiveWorkers != nil {
		fields.ExtraMaxActiveWorkers = *value.ExtraMaxActiveWorkers
	}
	return fields
}

// extraFieldsEqual 判断两份配置的补充池设置相同（使用方式按规范写法比较）
func extraFieldsEqual(saved config.Config, active config.Config) bool {
	return saved.ExtraPool == active.ExtraPool &&
		config.NormalizeExtraPoolMode(saved.ExtraPoolMode) == config.NormalizeExtraPoolMode(active.ExtraPoolMode) &&
		saved.ExtraPoolDefaultPaid == active.ExtraPoolDefaultPaid && saved.ExtraPoolIncludeUltra == active.ExtraPoolIncludeUltra &&
		saved.ExtraWarmWorkerLimit == active.ExtraWarmWorkerLimit && saved.ExtraMaxActiveWorkers == active.ExtraMaxActiveWorkers
}

// extraOwnedSet 为带补充池标记的账户（写时复制，读取方无锁）；version 每次变化加一，叠加视图的缓存按它失效
type extraOwnedSet struct {
	ids     map[string]struct{}
	version uint64
}

// slotCacheEntry 为叠加视图的缓存：按账户池分区快照（home，持有旧对象，指针不会被复用）与标记版本缓存的克隆
type slotCacheEntry struct {
	home    aistudio.PartitionSet
	version uint64
	set     aistudio.PartitionSet
}

// extraState 为 accountWorkerManager 上补充池的状态
type extraState struct {
	// extraSettings 为补充池的生效设置（见 setExtraSettings）
	extraSettings atomic.Pointer[extraPoolSettings]
	// extraMu 为叶子锁（持有时不取任何其他锁）：保护 extraOwned 的写时复制、extraStarting、extraSkip 与预热等待日志的去重
	extraMu sync.Mutex
	// extraOwned 为带补充池标记的账户；slotCache 为叠加视图的缓存
	extraOwned atomic.Pointer[extraOwnedSet]
	slotCache  atomic.Pointer[slotCacheEntry]
	// extraStarting 为补充池预热在途的账户（extraLaunch 发起、启动结束时移除）；extraSkip 为这一轮对账之后启动时已不是成员、
	// 已在启动或槽位已满而跳过的账户，下一轮对账清空（extraMu）
	extraStarting map[string]struct{}
	extraSkip     map[string]struct{}
	// extraIdleReason、extraIdleAt 为补充池预热等待与失败日志的去重（与主预热的 noteWarmIdle 同一规则、各记各的，extraMu）
	extraIdleReason string
	extraIdleAt     time.Time
	// extraLaunchMu 防止补充池预热重入（TryLock）；extraLaunches 跟踪补充池预热的 goroutine，Close 时等它们退出
	extraLaunchMu sync.Mutex
	extraLaunches sync.WaitGroup
	// extraView 为 extraPass 算好的成员缓存（见 extraView）；补充池关闭且没有补充池 Worker 时为 nil
	extraView atomic.Pointer[extraView]
	// extraLastLaunch 为最近一次发起补充池预热的时刻（UnixNano），防饿死用；extraPausedUntil 为出口限流暂停冷启动后的退避期限
	extraLastLaunch  atomic.Int64
	extraPausedUntil atomic.Int64
	// extraOverPeak 为超峰值 WARN 的去重（越过峰值时写一次，回落后才会再写）；extraBorrows 为本进程借出次数（状态接口）
	extraOverPeak atomic.Bool
	extraBorrows  atomic.Int64
	// extraTurn 为一起分担方式按两边空闲数轮取的计数器（manager 级一个，不按路由或模型分开，见 extra_select.go 的 shareFirst）；
	// extraClassified 为请求侧借用分类（热分类与全量分类）的次数，只供测试核对不借用的请求没有做补充池分类
	extraTurn       atomic.Uint64
	extraClassified atomic.Int64
	// extraMembership 为成员变化汇总日志的状态（见 noteMembershipChange）
	extraMembership extraMembershipLog
}

// slotMembers 返回 Worker 槽位用的分区快照（叠加视图）：带补充池标记的账户记为 PoolScopeExtra，其余同 pool.PartitionMembers()。
// 没有补充池 Worker 时直接返回账户池的快照（同一个 map 对象，与没有补充池时完全相同）；否则按（账户池快照、标记版本）缓存一份克隆，
// 克隆只在补充池 Worker 启停或账户池成员变化时发生。返回值是共享的只读快照，调用方不得修改
func (manager *accountWorkerManager) slotMembers() aistudio.PartitionSet {
	home := manager.pool.PartitionMembers()
	owned := manager.extraOwned.Load()
	if owned == nil || len(owned.ids) == 0 {
		return home
	}
	homePointer := reflect.ValueOf(home).UnsafePointer()
	if cache := manager.slotCache.Load(); cache != nil && cache.version == owned.version &&
		reflect.ValueOf(cache.home).UnsafePointer() == homePointer {
		return cache.set
	}
	set := make(aistudio.PartitionSet, len(home)+len(owned.ids))
	maps.Copy(set, home)
	for accountID := range owned.ids {
		set[accountID] = aistudio.PoolScopeExtra
	}
	manager.slotCache.Store(&slotCacheEntry{home: home, version: owned.version, set: set})
	return set
}

// setExtraOwnedLocked 给账户打上或去掉补充池标记；调用方持有 rebalanceMu（标记只在容量判断的同一段锁内变化）。extraMu 是叶子锁
func (manager *accountWorkerManager) setExtraOwnedLocked(accountID string, on bool) {
	manager.extraMu.Lock()
	defer manager.extraMu.Unlock()
	manager.setExtraOwnedMuLocked(accountID, on)
}

// setExtraOwnedMuLocked 为 setExtraOwnedLocked 的实现，调用方持有 extraMu
func (manager *accountWorkerManager) setExtraOwnedMuLocked(accountID string, on bool) {
	current := manager.extraOwned.Load()
	var ids map[string]struct{}
	var version uint64
	if current != nil {
		ids, version = current.ids, current.version
	}
	if _, has := ids[accountID]; has == on {
		return
	}
	next := &extraOwnedSet{ids: maps.Clone(ids), version: version + 1}
	if next.ids == nil {
		next.ids = make(map[string]struct{}, 1)
	}
	if on {
		next.ids[accountID] = struct{}{}
	} else {
		delete(next.ids, accountID)
	}
	manager.extraOwned.Store(next)
}

// clearExtraOwnedLocked 去掉账户的补充池标记（没有标记时什么也不做）；调用方持有 rebalanceMu
func (manager *accountWorkerManager) clearExtraOwnedLocked(accountID string) {
	if manager.extraOwnedHas(accountID) {
		manager.setExtraOwnedLocked(accountID, false)
	}
}

// clearExtraTagOnClose 在 Worker 完全关闭时（closeCleanupLeftovers，调用方持有账户锁）去掉补充池标记；账户正在启动（openingSet）时保留：
// 那次启动已按补充池判断容量、启动占位记在补充池（原槽位重开、请求触发的扩容），在 extraMu 内与 storeExtraOpening 互斥，
// 先登记的启动占位让这里保留标记，新 Worker 发布时仍计入补充池；启动没有发布时由 ensureWorker 结束占位后的 dropExtraTagLocked 去掉，
// 再兜底由对账的 clearStaleExtraTags 去掉。原先一律去掉：启动等 startupMu 期间旧 Worker 被不取独占租约的路径（登录续签、凭据变化、
// 重新导入、停用、改账户配置）关掉，新 Worker 发布时已不带标记，不经容量判断落在家分区，家分区超过峰值。没有标记时什么也不做
func (manager *accountWorkerManager) clearExtraTagOnClose(accountID string) {
	if !manager.extraOwnedHas(accountID) {
		return
	}
	manager.extraMu.Lock()
	defer manager.extraMu.Unlock()
	if _, opening := manager.openingSet.Load(accountID); opening {
		return
	}
	manager.setExtraOwnedMuLocked(accountID, false)
}

// storeExtraOpening 为按补充池判断了容量、但标记不是这次打上的启动（原槽位重开，或残留标记的账户）写启动占位的无锁镜像：在 extraMu 内
// 核对标记还在才写入 openingSet，与 clearExtraTagOnClose 互斥；标记已被关闭流程去掉时返回 false（账户已改记家分区，调用方按叠加视图重新判断：
// 判断容量之后、登记之前，不取独占租约的路径关掉了旧 Worker）。调用方持有 rebalanceMu
func (manager *accountWorkerManager) storeExtraOpening(accountID string) bool {
	manager.extraMu.Lock()
	defer manager.extraMu.Unlock()
	if !manager.extraOwnedHas(accountID) {
		return false
	}
	manager.openingSet.Store(accountID, struct{}{})
	return true
}

// extraSelfLockBusy 判断 ensureWorker 判断容量时账户自己的锁被占用（正在关闭、发布或核对 Worker，按原槽位计入了占用），且本进程有过补充池 Worker：
// 这时不按原槽位绕过容量判断，调用方回到快路径等这把锁再重新判断。关闭的可能是补充池 Worker（关完标记就去掉、改记家分区），也可能是正被淘汰、
// 替补已占了它槽位的 Worker，按原槽位重开会不经容量判断落在家分区，家分区超过峰值。没有过补充池 Worker 时恒为假（与没有补充池时相同）。
// 调用方持有 rebalanceMu
func (manager *accountWorkerManager) extraSelfLockBusy(accountID string, occupancy workerOccupancy) bool {
	if manager.extraOwned.Load() == nil {
		return false
	}
	_, busy := occupancy.lockBusy[accountID]
	return busy
}

// extraSlotMoved 在淘汰选号的账户锁内复核候选（调用方持有 account.mu）：本进程有过补充池 Worker 时（extraOwned 不为空指针），
// Worker 已不在、或补充池标记与要选的分区不一致的不作为候选。选号先读驻留快照（WarmAccountIDs）、再读叠加视图，两次读之间关闭的补充池 Worker
// 标记已去掉、在叠加视图里改记家分区，原先会被当成家分区的空闲 Worker 选中：“淘汰”它什么也空不出（它的槽位在补充池），按需替换照样在家分区发布，
// 家分区超过峰值。账户锁内 Worker 与标记都不会变（关闭、接手、归还都要账户锁；有 Worker 的账户占着槽位，不会被新的补充池启动打上标记）。
// 没有补充池时被关的 Worker 与新 Worker 在同一分区，这里恒为假（与没有补充池时相同）
func (manager *accountWorkerManager) extraSlotMoved(account *accountWorker, partition aistudio.PoolScope) bool {
	if manager.extraOwned.Load() == nil {
		return false
	}
	if account.worker == nil || !account.warm.Load() {
		return true
	}
	return partition != aistudio.PoolScopeAll && manager.extraOwnedHas(account.id) != (partition == aistudio.PoolScopeExtra)
}

// extraOwnedHas 判断账户带补充池标记
func (manager *accountWorkerManager) extraOwnedHas(accountID string) bool {
	owned := manager.extraOwned.Load()
	if owned == nil {
		return false
	}
	_, has := owned.ids[accountID]
	return has
}

// extraOwnedIDs 返回带补充池标记的账户，按 ID 排序
func (manager *accountWorkerManager) extraOwnedIDs() []string {
	owned := manager.extraOwned.Load()
	if owned == nil || len(owned.ids) == 0 {
		return nil
	}
	ids := slices.Collect(maps.Keys(owned.ids))
	slices.Sort(ids)
	return ids
}

// excludeExtraOwned 把带补充池标记的账户加进 held：主分区核对“有没有可接替账户”（SpareAccounts 按家分区数备用号）时，它们的家分区虽然
// 没变，叠加视图里却计在补充池、不在家分区的驻留里，不排除就会被当成家分区的空闲备用号；替补挑选（pickReplacement 排除全部驻留）挑不到它们，
// 于是没有真正的替补也会淘汰或轮换主分区的 Worker，被换下的账户又被主预热拉起。没有补充池 Worker 时什么也不做（与没有补充池时相同）
func (manager *accountWorkerManager) excludeExtraOwned(held map[string]struct{}) {
	owned := manager.extraOwned.Load()
	if owned == nil {
		return
	}
	for accountID := range owned.ids {
		held[accountID] = struct{}{}
	}
}

// markExtraAccounts 填写账户列表的补充池关系（AdminAccount.Extra）：带补充池标记的为 worker，其余成员（最近一次对账的成员缓存）
// 为 member。补充池关闭时不读成员缓存（只标还带标记的 worker）：停服期间关闭补充池时不对账，缓存还是关闭之前的。
// 补充池关闭且没有补充池 Worker 时什么也不做
func (manager *accountWorkerManager) markExtraAccounts(accounts []api.AdminAccount) {
	if manager == nil {
		return
	}
	owned, view := manager.extraOwned.Load(), manager.extraView.Load()
	if !manager.extraSettingsValue().enabled {
		view = nil
	}
	if (owned == nil || len(owned.ids) == 0) && view == nil {
		return
	}
	for index := range accounts {
		accountID := accounts[index].ID
		if owned != nil {
			if _, has := owned.ids[accountID]; has {
				accounts[index].Extra = adminAccountExtraWorker
				continue
			}
		}
		if view != nil {
			if _, member := view.memberSet[accountID]; member {
				accounts[index].Extra = adminAccountExtraMember
			}
		}
	}
}

// adminAccountExtraWorker、adminAccountExtraMember 为 AdminAccount.Extra 的取值
const (
	adminAccountExtraWorker = "worker"
	adminAccountExtraMember = "member"
)

// extraOwnedCount 返回带补充池标记的账户数
func (manager *accountWorkerManager) extraOwnedCount() int {
	owned := manager.extraOwned.Load()
	if owned == nil {
		return 0
	}
	return len(owned.ids)
}

// logExtraPoolEnabled 在装配时 EXTRA_POOL=true 写一行补充池已开启（关闭时不写）
func logExtraPoolEnabled(requests *requestRegistry, cfg config.Config) {
	if requests != nil && cfg.ExtraPool {
		requests.log("service", "INFO", "补充池已开启 | "+extraSettingsSummary(extraSettingsOf(cfg)))
	}
}

// 补充池 Worker 的生命周期（批 2）：补充池预热（常驻数）、请求触发的扩容入口（峰值）、空闲回收、原地接手、对账（归还与释放）、
// 残留标记的兜底与状态。标记只在两种场合置上，都持有 rebalanceMu：ensureWorker 登记启动占位且已通过补充池的容量判断、
// 接手且补充池低于常驻数；只在三种场合去掉：归还（rebalanceMu）、启动失败（rebalanceMu）、Worker 完全关闭（closeCleanupLeftovers，
// 此时账户已不占任何槽位）。所以浏览器总数不超过各分区峰值之和加补充池峰值（各加排空或替换余量）

const (
	// extraUsableModels 为判断“还有初始化模型能用”时取的初始化模型数（bootstrapSummary().ModelIDs 的前几个）：控制补充池候选集的数量
	extraUsableModels = 8
	// extraColdSample、extraColdKeep 为冷成员排序的抽样：按冷却结束从晚到早取前 extraColdSample 个排序，留前 extraColdKeep 个
	extraColdSample = 512
	extraColdKeep   = 64
	// extraStarveAfter 为防饿死时间：主预热占满启动名额时，补充池连续这么久没有发起启动就放行一个
	extraStarveAfter = 2 * time.Minute
	// extraEgressBackoff 为补充池的冷启动被出口闸门暂停后不再发起的时长（与 keepWarm 周期相同）
	extraEgressBackoff = 5 * time.Second
	// extraMembershipLogEvery 为成员变化汇总日志的最短间隔
	extraMembershipLogEvery = 30 * time.Second
)

// errExtraMemberGone 表示要按补充池启动的账号此刻已不是补充池成员（冷却已结束、补充池已关闭、权益不在范围或整户不可用）：
// 补充池预热与扩容按跳过处理，借用后的重开换号（见 retryableGenerateAccountError）
var errExtraMemberGone = errors.New("补充池账号已不是成员")

// extraStartOptions 为补充池启动的 ctx 选项（见 withExtraStart）：stay 为按留存口径判断成员（重开、重放），否则按进入口径（预热、扩容）；
// fallbackHome 为已不是成员时按家分区启动（借用的账户本来就属于本路由），否则返回 errExtraMemberGone；prewarm 为补充池预热：
// 补充池已满时不替换空闲 Worker，返回 errAccountWorkerCapacity
type extraStartOptions struct {
	stay         bool
	fallbackHome bool
	prewarm      bool
}

type extraStartKey struct{}

// withExtraStart 给 ctx 标上补充池启动的选项：ensureWorker 据此把冷启动计入补充池（见 extraStartAs）
func withExtraStart(ctx context.Context, options extraStartOptions) context.Context {
	return context.WithValue(ctx, extraStartKey{}, options)
}

// extraStartFrom 返回 ctx 上补充池启动的选项；没有时 ok 为假（与没有补充池时完全相同）
func extraStartFrom(ctx context.Context) (extraStartOptions, bool) {
	if ctx == nil {
		return extraStartOptions{}, false
	}
	options, ok := ctx.Value(extraStartKey{}).(extraStartOptions)
	return options, ok
}

// extraPrewarmStartup 判断 ctx 是补充池预热：分区已满时不替换空闲 Worker
func extraPrewarmStartup(ctx context.Context) bool {
	options, ok := extraStartFrom(ctx)
	return ok && options.prewarm
}

// extraStartDecision 为补充池启动时账户按哪里计（见 extraStartAs）
type extraStartDecision int

const (
	// extraAsHome 按家分区启动
	extraAsHome extraStartDecision = iota
	// extraAsExtra 计入补充池，启动占位时打上补充池标记
	extraAsExtra
	// extraAsGone 已不是成员：返回 errExtraMemberGone
	extraAsGone
)

// extraStartAs 在 ensureWorker 的容量判断处决定补充池启动的账户按哪里计：补充池开启、峰值大于 0 且账户此刻仍是成员（现查，stay 决定门槛）
// 时计入补充池；否则 fallbackHome 时按家分区，不然按已不是成员。调用方持有 rebalanceMu（ExtraMemberStates 取账户池锁，
// 与在 rebalanceMu 内取分区快照的锁序相同）
func (manager *accountWorkerManager) extraStartAs(accountID string, options extraStartOptions) extraStartDecision {
	if settings := manager.extraSettingsValue(); settings.enabled && settings.max > 0 {
		threshold := extraEntryMin
		if options.stay {
			threshold = 0
		}
		if manager.pool.ExtraMemberStates([]string{accountID}, time.Now(), threshold, manager.extraUsable())[accountID] == aistudio.ExtraStay {
			return extraAsExtra
		}
	}
	if options.fallbackHome {
		return extraAsHome
	}
	return extraAsGone
}

// extraUsable 返回判断“还有初始化模型能用”的初始化模型（bootstrapSummary().ModelIDs 的前 extraUsableModels 个）
func (manager *accountWorkerManager) extraUsable() []string {
	models := manager.bootstrapSummary().ModelIDs
	return slices.Clone(models[:min(len(models), extraUsableModels)])
}

// extraSlotsActive 判断补充池的循环（空闲回收）要不要执行：补充池开启或还有补充池 Worker；关闭且没有时整段跳过
func (manager *accountWorkerManager) extraSlotsActive() bool {
	return manager.extraSettingsValue().enabled || manager.extraOwnedCount() > 0
}

// extraAccount 返回账户的 Worker 记录（没有时为 nil）
func (manager *accountWorkerManager) extraAccount(accountID string) *accountWorker {
	manager.mu.RLock()
	defer manager.mu.RUnlock()
	return manager.accounts[accountID]
}

// extraLabel 返回账户在运行日志里的标签（与“WAA Worker 就绪”等行相同），没有标签时为账户 ID；不在 rebalanceMu 内调用
func (manager *accountWorkerManager) extraLabel(accountID string) string {
	if account := manager.extraAccount(accountID); account != nil {
		account.mu.Lock()
		label := account.label
		account.mu.Unlock()
		if label != "" {
			return label
		}
	}
	return accountID
}

// partitionOccupancyLocked 按 ensureWorker 扩容判断的口径统计分区的占用：驻留、正在关闭的 Worker（occupiedWorkersIn，排空按已让出计）
// 加上同分区的启动占位（账户锁被占用、已按 1 个槽位估计的不重复计入：否则两个账户同时启动时后一个会把前一个算成两个槽位，
// 分区还有空槽却按已满返回），返回占用与占着槽位的账户。调用方持有 rebalanceMu
func (manager *accountWorkerManager) partitionOccupancyLocked(
	partition aistudio.PoolScope, members aistudio.PartitionSet,
) (workerOccupancy, map[string]struct{}) {
	occupancy := manager.occupiedWorkersIn(partition, members)
	occupied := make(map[string]struct{}, len(occupancy.accountIDs)+len(manager.openings))
	for _, occupiedAccountID := range occupancy.accountIDs {
		occupied[occupiedAccountID] = struct{}{}
	}
	for openingAccountID := range manager.openings {
		if partitionOf(openingAccountID, members) != partition {
			continue
		}
		occupied[openingAccountID] = struct{}{}
		if _, estimated := occupancy.lockBusy[openingAccountID]; !estimated {
			occupancy.slots++
		}
	}
	return occupancy, occupied
}

// dropExtraTagLocked 在补充池启动结束占位之后（启动失败、取消、放弃，或发布后的复核）去掉标记：账户已不占任何槽位时清除；Worker、
// 清理失败留下的浏览器或租约还在时保留（仍按补充池计数，关闭或清理完成时由 closeCleanupLeftovers 去掉），账户锁被占用时同样保留
// （由对账的 clearStaleExtraTags 兜底）。调用方持有 rebalanceMu：占位刚结束、还没放开这把锁，期间没有别的路径能给这个账户启动 Worker；
// 放在结束占位之后，占位还在时关闭流程保留标记（见 clearExtraTagOnClose），结束之后的关闭照常去掉
func (manager *accountWorkerManager) dropExtraTagLocked(accountID string) {
	account := manager.extraAccount(accountID)
	if account == nil {
		manager.clearExtraOwnedLocked(accountID)
		return
	}
	if !account.mu.TryLock() {
		return
	}
	idle := account.worker == nil && account.cleanupWorker == nil && account.runtimeLease == nil && account.cleanupLease == nil
	account.mu.Unlock()
	if idle {
		manager.clearExtraOwnedLocked(accountID)
	}
}

// extraReadySuffix 为“WAA Worker 就绪”末尾的字段：带补充池标记时为“ | 分区=补充池”，否则为空（文本与没有补充池时相同）
func (manager *accountWorkerManager) extraReadySuffix(accountID string) string {
	if manager != nil && manager.extraOwnedHas(accountID) {
		return " | 分区=补充池"
	}
	return ""
}

// handOffSuffix 为冷却淘汰、回收冷却占位、空闲回收、按需替换日志末尾的去向：Worker 被补充池原地接手（账户此刻带补充池标记）时为
// “ | 去向=补充池接手”，否则为空
func (manager *accountWorkerManager) handOffSuffix(accountID string) string {
	if manager.extraOwnedHas(accountID) {
		return " | 去向=补充池接手"
	}
	return ""
}

// mainWarmCount 返回 warm 中计入三个分区的 Worker 数（主预热的日志：补充池 Worker 由补充池自己预热）；没有补充池 Worker 时为 len(warm)
func (manager *accountWorkerManager) mainWarmCount(warm []string) int {
	if manager.extraOwnedCount() == 0 {
		return len(warm)
	}
	return len(warm) - len(inPartition(warm, manager.slotMembers(), aistudio.PoolScopeExtra))
}

// extraHandOffCauses 为可以原地接手的停止起因：冷却淘汰、冷却轮换（情形 B 的典型去路）、回收冷却占位、空闲回收、按需替换
// （接手同样空出家分区的槽位）。平均调用轮换不接手（轮换就是要让用量多的号下线，接手后它会在补充池继续接请求）；冷却清扫只处理
// 全局长冷却（整户不可用）；不可调度释放与其余起因是整户不可用或运维操作
var extraHandOffCauses = map[string]bool{
	workerStopCoolingRetire:      true,
	workerStopCoolingRotation:    true,
	workerStopReclaimPlaceholder: true,
	workerStopIdleReclaim:        true,
	workerStopReplace:            true,
}

// extraAdopt 为原地接手（evictIdleWorkerFor 的唯一入口，调用方已取得账户的独占租约，不持有 rebalanceMu）：补充池开启、起因在白名单里、
// Worker 还不是补充池 Worker、账户按进入口径是成员（锁外现查），并且补充池低于常驻数（只补到常驻数、不借峰值）时，给它打上补充池标记、
// 不关浏览器，返回 true（调用方按“已空出槽位”继续）。持有 rebalanceMu 时对账户锁只用 TryLock：取不到（正在启动、关闭或核对）就不接手，
// 照常关闭。接手后家分区少 1、补充池多 1，补充池仍在常驻数以内，家分区随后的替补用刚空出的槽位
func (manager *accountWorkerManager) extraAdopt(accountID string, cause string) bool {
	settings := manager.extraSettingsValue()
	if !settings.enabled || !extraHandOffCauses[cause] || manager.extraOwnedHas(accountID) {
		return false
	}
	if manager.pool.ExtraMemberStates([]string{accountID}, time.Now(), extraEntryMin, manager.extraUsable())[accountID] != aistudio.ExtraStay {
		return false
	}
	account := manager.extraAccount(accountID)
	if account == nil {
		return false
	}
	manager.rebalanceMu.Lock()
	slots := manager.slotMembers()
	from := partitionOf(accountID, slots)
	occupancy, _ := manager.partitionOccupancyLocked(aistudio.PoolScopeExtra, slots)
	resident := manager.warmTargetFor(aistudio.PoolScopeExtra)
	adopted, label := false, ""
	if occupancy.effectiveSlots() < resident && manager.openings[accountID] == nil && account.startupMu.TryLock() {
		if account.mu.TryLock() {
			if worker := account.worker; worker != nil && account.cleanupWorker == nil && account.warm.Load() {
				phase := worker.State().Phase
				adopted = phase == aistudio.WorkerReady || phase == aistudio.WorkerBusy
			}
			if adopted {
				manager.setExtraOwnedLocked(accountID, true)
				label = account.label
			}
			account.mu.Unlock()
		}
		account.startupMu.Unlock()
	}
	count := occupancy.effectiveSlots() + 1
	manager.rebalanceMu.Unlock()
	if !adopted {
		return false
	}
	// 原有的淘汰登记与排空随之结束（都在锁外：retireMu、drainMu）；唤醒等待容量的请求
	manager.dropRetirement(accountID)
	manager.notifyScheduling()
	if label == "" {
		label = accountID
	}
	manager.requests.log(label, "INFO", fmt.Sprintf(
		"补充池接手 Worker | 原分区=%s | 起因=%s | 补充池 Worker=%d/%d", partitionLabel(from), cause, count, resident,
	))
	return true
}

// extraReturnResult 为原地归还的结果
type extraReturnResult int

const (
	// extraReturned 已去掉标记，Worker 回到家分区计数
	extraReturned extraReturnResult = iota
	// extraHomeFull 家分区再加 1 个会超过峰值：空闲时释放
	extraHomeFull
	// extraReturnBusy 账户正在启动、被选中淘汰、账户锁被占用或已没有 Worker：留到下一轮
	extraReturnBusy
)

// extraReturn 把已不是成员的补充池 Worker 原地归还家分区（去掉补充池标记，不关浏览器）：持有 rebalanceMu，按叠加视图核对家分区再加 1 个
// 不超过峰值、账户不在启动中、没有被选中淘汰、账户锁 TryLock 成功且 Worker 还在。返回家分区与结果；归还后调用方唤醒排队的请求。
// 家分区高出常驻数的部分由家分区的空闲回收照常收敛
func (manager *accountWorkerManager) extraReturn(accountID string) (aistudio.PoolScope, extraReturnResult) {
	home := manager.pool.PartitionMembers().Of(accountID)
	account := manager.extraAccount(accountID)
	manager.rebalanceMu.Lock()
	defer manager.rebalanceMu.Unlock()
	if account == nil || !manager.extraOwnedHas(accountID) || manager.openings[accountID] != nil || manager.victimReserved(accountID) {
		return home, extraReturnBusy
	}
	occupancy, _ := manager.partitionOccupancyLocked(home, manager.slotMembers())
	if occupancy.effectiveSlots()+1 > manager.maxActiveFor(home) {
		return home, extraHomeFull
	}
	if !account.startupMu.TryLock() {
		return home, extraReturnBusy
	}
	defer account.startupMu.Unlock()
	if !account.mu.TryLock() {
		return home, extraReturnBusy
	}
	defer account.mu.Unlock()
	if account.worker == nil {
		return home, extraReturnBusy
	}
	manager.setExtraOwnedLocked(accountID, false)
	return home, extraReturned
}

// clearStaleExtraTags 去掉不占任何槽位却仍带补充池标记的账户（兜底：启动失败时账户锁恰好被占用、账户已从管理器移除等）：
// 在 rebalanceMu 内核对不在启动中、账户锁 TryLock 成功且没有 Worker、清理残留与租约
func (manager *accountWorkerManager) clearStaleExtraTags() {
	owned := manager.extraOwnedIDs()
	if len(owned) == 0 {
		return
	}
	manager.rebalanceMu.Lock()
	defer manager.rebalanceMu.Unlock()
	for _, accountID := range owned {
		if manager.openings[accountID] != nil {
			continue
		}
		account := manager.extraAccount(accountID)
		if account == nil {
			manager.clearExtraOwnedLocked(accountID)
			continue
		}
		if !account.startupMu.TryLock() {
			continue
		}
		if account.mu.TryLock() {
			if account.worker == nil && account.cleanupWorker == nil && account.runtimeLease == nil && account.cleanupLease == nil {
				manager.clearExtraOwnedLocked(accountID)
			}
			account.mu.Unlock()
		}
		account.startupMu.Unlock()
	}
}

// extraView 为一轮对账（extraPass）算好的成员缓存：状态接口、补充池预热与借用只读它，不在请求里对账户分类
type extraView struct {
	// members 为按进入口径的成员数（含已有 Worker 的），free（含 Plus 与权益尚未识别的）、pro、ultra 为其中各权益的个数
	members, free, pro, ultra int
	// memberSet 为成员：按进入口径的成员加上仍是成员（留存口径）的补充池 Worker
	memberSet map[string]struct{}
	// returning 为补充池 Worker 中已不是成员、等待归还或释放的账户；staying 为仍是成员的补充池 Worker 数
	returning map[string]struct{}
	staying   int
	// cold 为冷成员（是成员、在任何分区都不占槽位）中排好序的前 extraColdKeep 个，coldCount 为冷成员总数
	cold      []string
	coldCount int
}

// buildExtraView 按这一轮的成员（ExtraMembers，已按冷却结束从晚到早排好）与补充池 Worker 的留存判断建立成员缓存。冷成员排序：
// 去掉占槽位的（驻留、启动中与补充池在途的）与 5 分钟内预热失败的，取前 extraColdSample 个按 rankColdCandidates（去掉坏号、
// 近期成功的整组排前、平均调用时今日发送少的在前、能服务的主用模型多的在前，主用模型为普通与 Ultra 的并集）排序，
// 再把疑似登录失效的排到最后，取前 extraColdKeep 个
func (manager *accountWorkerManager) buildExtraView(
	members []aistudio.ExtraMember, owned []string, states map[string]aistudio.ExtraExit, now time.Time,
) *extraView {
	view := &extraView{
		members: len(members), memberSet: make(map[string]struct{}, len(members)+len(owned)), returning: make(map[string]struct{}),
	}
	for _, member := range members {
		view.memberSet[member.ID] = struct{}{}
		switch member.Tier {
		case aistudio.BenefitTierPro:
			view.pro++
		case aistudio.BenefitTierUltra:
			view.ultra++
		default:
			view.free++
		}
	}
	for _, accountID := range owned {
		if states[accountID] == aistudio.ExtraStay {
			view.memberSet[accountID] = struct{}{}
			view.staying++
		} else {
			view.returning[accountID] = struct{}{}
		}
	}
	held := make(map[string]struct{}, len(owned))
	for _, accountID := range owned {
		held[accountID] = struct{}{}
	}
	for _, accountID := range manager.WarmAccountIDs() {
		held[accountID] = struct{}{}
	}
	for _, accountID := range manager.OpeningAccountIDs() {
		held[accountID] = struct{}{}
	}
	manager.extraMu.Lock()
	for accountID := range manager.extraStarting {
		held[accountID] = struct{}{}
	}
	manager.extraMu.Unlock()
	failed := manager.recentWarmFailures(now)
	sample := make([]string, 0, min(len(members), extraColdSample))
	for _, member := range members {
		if _, occupied := held[member.ID]; occupied {
			continue
		}
		view.coldCount++
		if _, recent := failed[member.ID]; recent || len(sample) >= extraColdSample {
			continue
		}
		sample = append(sample, member.ID)
	}
	ranked := manager.deferAuthSuspects(manager.rankColdCandidates(aistudio.PoolScopeExtra, sample))
	view.cold = slices.Clone(ranked[:min(len(ranked), extraColdKeep)])
	return view
}

// extraTarget 返回补充池要补齐到的 Worker 数：常驻数与（仍是成员的补充池 Worker + 冷成员）取小；补充池关闭时为 0
func (manager *accountWorkerManager) extraTarget(view *extraView) int {
	if view == nil {
		return 0
	}
	return min(manager.warmTargetFor(aistudio.PoolScopeExtra), view.staying+view.coldCount)
}

// extraPass 为一轮完整的补充池对账加补充池预热（keepWarm 每个周期分两段做，见下）：
//  1. 补充池关闭且没有补充池 Worker 时什么也不做（与没有补充池时完全相同）；
//  2. 算这一轮的成员（进入口径）与补充池 Worker 的留存（留存口径），存成员缓存，必要时写成员变化汇总；
//  3. 已不是成员的补充池 Worker：冷却已结束、补充池已关闭或权益不在范围时，家分区有空位就原地归还，否则空闲时释放；全局长冷却、
//     没有能用的模型时空闲释放；不可调度的交给不可调度释放（它遍历全部驻留 Worker）。释放每轮最多 max(WSC,1) 个，忙碌的等下一轮；
//  4. 清残留标记、按边沿写超峰值 WARN，最后补充池预热。
//
// keepWarmPass 把对账（extraReconcile）与补充池预热（extraLaunchAfterPrewarm）分开调用：对账放在平均调用之后、主分区补齐预热之前
// （归还先于主分区的预热判断），补充池预热挪到主分区补齐预热之后（I8 主预热先拿冷启动名额）
func (service *trackedService) extraPass(ctx context.Context) {
	if view := service.extraReconcile(ctx); view != nil {
		service.workers.extraLaunch(view)
	}
}

// extraReconcile 为 extraPass 的第 1–3 步与第 4 步的清残留标记、超峰值 WARN（不含补充池预热），返回这一轮的成员缓存；
// 补充池关闭且没有补充池 Worker 时什么也不做，返回 nil
func (service *trackedService) extraReconcile(ctx context.Context) *extraView {
	workers := service.workers
	if workers == nil {
		return nil
	}
	settings := workers.extraSettingsValue()
	owned := workers.extraOwnedIDs()
	if !settings.enabled && len(owned) == 0 {
		if workers.extraView.Load() != nil {
			workers.extraView.Store(nil)
			workers.resetMembershipLog()
		}
		return nil
	}
	now, usable := time.Now(), workers.extraUsable()
	states := service.pool.ExtraMemberStates(owned, now, 0, usable)
	members := service.pool.ExtraMembers(now, extraEntryMin, usable)
	view := workers.buildExtraView(members, owned, states, now)
	workers.extraView.Store(view)
	workers.extraMu.Lock()
	workers.extraSkip = nil
	workers.extraMu.Unlock()
	if settings.enabled {
		workers.noteMembershipChange(members, now)
	} else {
		workers.resetMembershipLog()
	}
	type release struct {
		accountID string
		reason    string
	}
	var releases []release
	budget := max(workers.warmConcurrencyValue(), 1)
	for _, accountID := range owned {
		exit := states[accountID]
		account := workers.extraAccount(accountID)
		switch {
		case exit == aistudio.ExtraStay, exit == aistudio.ExtraUnschedulable, exit == aistudio.ExtraMissing, account == nil:
			continue
		case workers.workerStarting(account, accountID):
			continue
		}
		reason := "家分区已满"
		switch exit {
		case aistudio.ExtraRecovered, aistudio.ExtraRuleOff:
			home, result := workers.extraReturn(accountID)
			if result == extraReturnBusy {
				continue
			}
			if result == extraReturned {
				workers.notifyScheduling()
				cause := "冷却已结束"
				if exit == aistudio.ExtraRuleOff {
					cause = "权益不在补充池范围"
					if !settings.enabled {
						cause = "补充池已关闭"
					}
				}
				service.requests.log(workers.extraLabel(accountID), "INFO", fmt.Sprintf(
					"补充池 Worker 归还 | 分区=%s | 原因=%s | %s=%d/%d",
					partitionLabel(home), cause, workerSlotName(home), len(workers.warmAccountIDsIn(home)), workers.maxActiveFor(home),
				))
				continue
			}
			// 补充池关闭导致离开、家分区又已满：原因写明是关闭补充池（关闭时补充池的峰值按 0 计）
			if exit == aistudio.ExtraRuleOff && !settings.enabled {
				reason = "补充池已关闭，家分区已满"
			}
		case aistudio.ExtraGlobalCooling:
			reason = "全局冷却"
		case aistudio.ExtraNoUsableModel:
			reason = "没有能用的模型"
		}
		if len(releases) >= budget || !workers.tryReserveVictim(accountID) {
			continue
		}
		releases = append(releases, release{accountID: accountID, reason: reason})
	}
	// 释放并发进行（关一个浏览器要几秒），数量不超过每轮的预算；账号忙时（100 毫秒内取不到独占租约）跳过，下一轮再试
	var closes sync.WaitGroup
	for _, target := range releases {
		closes.Add(1)
		go func(target release) {
			defer closes.Done()
			evicted, err := workers.evictIdleWorkerFor(ctx, target.accountID, workerStopExtraRelease)
			workers.releaseVictim(target.accountID)
			switch {
			case err != nil:
				if ctx.Err() == nil {
					service.requests.log(workers.extraLabel(target.accountID), "WARN", fmt.Sprintf("补充池 Worker 释放失败 | 错误=%v", err))
				}
			case evicted:
				service.requests.log(workers.extraLabel(target.accountID), "INFO", fmt.Sprintf(
					"补充池 Worker 释放 | 原因=%s | 补充池 Worker=%d/%d",
					target.reason, len(workers.warmAccountIDsIn(aistudio.PoolScopeExtra)), workers.maxActiveFor(aistudio.PoolScopeExtra),
				))
			}
		}(target)
	}
	closes.Wait()
	workers.clearStaleExtraTags()
	workers.noteExtraOverPeak()
	return view
}

// noteExtraOverPeak 补充池开启且补充池 Worker（驻留与启动中，减去已选为淘汰对象、即将关闭的）超过峰值时写一次 WARN（调小
// EXTRA_MAX_ACTIVE_WORKERS 时不强制关闭，由空闲回收与对账逐步收敛），回落之后才会再写。补充池满到峰值后的按需替换是先启后停：
// 新 Worker 启动期间旧 Worker 还在、已被预留为淘汰对象，按峰值加进行中的替换计，不算越过峰值
func (manager *accountWorkerManager) noteExtraOverPeak() {
	settings := manager.extraSettingsValue()
	slots := manager.slotMembers()
	warm := inPartition(manager.WarmAccountIDs(), slots, aistudio.PoolScopeExtra)
	current := len(warm) + len(inPartition(manager.OpeningAccountIDs(), slots, aistudio.PoolScopeExtra))
	for _, accountID := range warm {
		if manager.victimReserved(accountID) {
			current--
		}
	}
	peak := manager.maxActiveFor(aistudio.PoolScopeExtra)
	if !settings.enabled || current <= peak {
		manager.extraOverPeak.Store(false)
		return
	}
	if manager.extraOverPeak.CompareAndSwap(false, true) {
		manager.requests.log("service", "WARN", fmt.Sprintf(
			"补充池 Worker 超过峰值 | 当前=%d | 峰值=%d | 空闲 Worker 将逐步回收，可调大 EXTRA_MAX_ACTIVE_WORKERS", current, peak,
		))
	}
}

// extraMembershipLog 为成员变化汇总日志的状态：last 为上一次写日志时的成员，at 为那一次的时刻
type extraMembershipLog struct {
	mu   sync.Mutex
	last map[string]struct{}
	at   time.Time
}

// clearExtraView 清空补充池的成员缓存与成员变化汇总日志的状态：停服（ResetAll）时调用。停服期间 keepWarm 不运行、不对账，
// 原先缓存一直是停服前的，状态接口的成员数与账户页的“可补充”都是过时的；服务再次启动后的第一轮对账重新计算。没有缓存时什么也不做
func (manager *accountWorkerManager) clearExtraView() {
	if manager.extraView.Load() == nil {
		return
	}
	manager.extraView.Store(nil)
	manager.resetMembershipLog()
}

// resetMembershipLog 清空成员变化汇总日志的状态（补充池关闭后，再开启时从零比较）
func (manager *accountWorkerManager) resetMembershipLog() {
	manager.extraMembership.mu.Lock()
	manager.extraMembership.last, manager.extraMembership.at = nil, time.Time{}
	manager.extraMembership.mu.Unlock()
}

// noteMembershipChange 写成员变化汇总：与上一次写日志时的成员（按进入口径）相比有加入或退出时写一行，最多每 extraMembershipLogEvery 一行
// （间隔内的变化合并到下一行）；没有进出时不写
func (manager *accountWorkerManager) noteMembershipChange(members []aistudio.ExtraMember, now time.Time) {
	state := &manager.extraMembership
	state.mu.Lock()
	if !state.at.IsZero() && now.Sub(state.at) < extraMembershipLogEvery {
		state.mu.Unlock()
		return
	}
	current := make(map[string]struct{}, len(members))
	joined, left := 0, 0
	var free, pro, ultra int
	for _, member := range members {
		current[member.ID] = struct{}{}
		if _, was := state.last[member.ID]; !was {
			joined++
		}
		switch member.Tier {
		case aistudio.BenefitTierPro:
			pro++
		case aistudio.BenefitTierUltra:
			ultra++
		default:
			free++
		}
	}
	for accountID := range state.last {
		if _, still := current[accountID]; !still {
			left++
		}
	}
	if joined+left == 0 {
		state.mu.Unlock()
		return
	}
	state.last, state.at = current, now
	state.mu.Unlock()
	slots := manager.slotMembers()
	workers := len(inPartition(manager.WarmAccountIDs(), slots, aistudio.PoolScopeExtra))
	manager.requests.log("service", "INFO", fmt.Sprintf(
		"补充池成员变化 | 加入=%d | 退出=%d | 成员=%d（Free %d · Pro %d · Ultra %d） | 补充池 Worker=%d/%d",
		joined, left, len(members), free, pro, ultra, workers, manager.maxActiveFor(aistudio.PoolScopeExtra),
	))
}

// noteExtraIdle 写补充池预热等待或失败的日志：原因不变时每 warmIdleLogInterval 最多一次（与主预热的 noteWarmIdle 同一规则，各记各的，
// 不改主预热在状态接口里的等待原因）
func (manager *accountWorkerManager) noteExtraIdle(level string, reason string, message string) {
	now := time.Now()
	manager.extraMu.Lock()
	if reason == manager.extraIdleReason && now.Sub(manager.extraIdleAt) < warmIdleLogInterval {
		manager.extraMu.Unlock()
		return
	}
	manager.extraIdleReason, manager.extraIdleAt = reason, now
	manager.extraMu.Unlock()
	manager.requests.log("service", level, message)
}

// extraLaunchRoom 返回补充池预热这一刻还能发起的启动数（扣掉在途的）：主预热不在运行时为 max(1, WSC−1)（至少给主分区的按需启动留一个名额）；
// 主预热在运行时，它没占满启动名额、或补充池已连续 extraStarveAfter 没有发起启动（防饿死：每日额度集中用完时主预热几乎一直在运行）时为 1，
// 否则为 0
func (manager *accountWorkerManager) extraLaunchRoom() int {
	return manager.extraLaunchRoomWith(false)
}

// extraLaunchRoomWith 为 extraLaunchRoom 的实现；mainStarted 为 keepWarmPass 这一轮刚发起了主分区的补齐预热：fillWarm 在 goroutine 里才
// 置 fillActive，这时还读不到，按主预热在运行处理（它的在途数还没登记，按有空余名额计），不再按主预热不在运行给 max(1, WSC−1)
func (manager *accountWorkerManager) extraLaunchRoomWith(mainStarted bool) int {
	concurrency := max(manager.warmConcurrencyValue(), 1)
	limit := max(1, concurrency-1)
	if manager.fillActive.Load() || mainStarted {
		free := concurrency - int(manager.fillInflight.Load())
		starving := time.Since(time.Unix(0, manager.extraLastLaunch.Load())) >= extraStarveAfter
		limit = 0
		if free > 0 || starving {
			limit = 1
		}
	}
	manager.extraMu.Lock()
	starting := len(manager.extraStarting)
	manager.extraMu.Unlock()
	return limit - starting
}

// extraLaunch 为补充池预热（对账时调用，每个补充池启动成功后再调用一次以便连续补满；TryLock 防重入）：补充池开启、各主分区的首轮预热都已
// 出结果、不在出口暂停的退避期内时，从冷成员里补到 extraTarget（常驻数与成员数取小），每次最多 extraLaunchRoom 个。
// 启动按后台冷启动名额排队（让给按需启动），只用补充池的空槽、不替换（ensureWorker 返回容量已满），冷启动闸门按超出常驻处理。
// 低于目标却发不出启动时按 noteWarmIdle 的规则写预热等待。生成服务不在运行时（停服、重建期间）什么也不做：上一个启动成功后的接续
// 可能落在停服的 ResetAll 之后
func (manager *accountWorkerManager) extraLaunch(view *extraView) {
	manager.extraLaunchWith(view, false)
}

// extraLaunchAfterPrewarm 为 keepWarmPass 在主分区补齐预热之后发起的补充池预热（I8）：mainStarted 为这一轮刚发起了主预热，
// 在途上限按主预热在运行计（见 extraLaunchRoomWith）。原先补充池预热在对账里、先于主预热发起，主预热那一刻没在运行时补充池先按
// max(1, WSC−1) 发起冷启动、占住后台冷启动名额，同一轮紧接着要补齐的主分区只能拿到剩下的名额。view 为 nil（补充池关闭且没有补充池 Worker）
// 时什么也不做
func (manager *accountWorkerManager) extraLaunchAfterPrewarm(view *extraView, mainStarted bool) {
	manager.extraLaunchWith(view, mainStarted)
}

// extraLaunchWith 为 extraLaunch 的实现（mainStarted 见 extraLaunchAfterPrewarm）
func (manager *accountWorkerManager) extraLaunchWith(view *extraView, mainStarted bool) {
	if view == nil || manager.lifecycle.Err() != nil || !manager.extraSettingsValue().enabled || !manager.serviceRunning() ||
		!manager.extraLaunchMu.TryLock() {
		return
	}
	defer manager.extraLaunchMu.Unlock()
	now := time.Now()
	owned := manager.extraOwned.Load()
	manager.extraMu.Lock()
	starting := maps.Clone(manager.extraStarting)
	skip := maps.Clone(manager.extraSkip)
	manager.extraMu.Unlock()
	have := 0
	if owned != nil {
		for accountID := range owned.ids {
			if _, returning := view.returning[accountID]; !returning {
				have++
			}
		}
	}
	for accountID := range starting {
		if owned == nil || !hasKey(owned.ids, accountID) {
			have++
		}
	}
	target := manager.extraTarget(view)
	if have >= target {
		return
	}
	reason := ""
	switch {
	case len(manager.firstWarmPendingIn(aistudio.PoolScopeAll)) > 0:
		reason = "首轮预热未完成"
	case now.UnixNano() < manager.extraPausedUntil.Load():
		reason = egressColdStartPausedReason
	}
	if reason == "" {
		room := manager.extraLaunchRoomWith(mainStarted)
		if room <= 0 {
			if len(starting) > 0 {
				return
			}
			reason = "主预热占满启动名额"
		} else if picks := manager.extraPicks(view, owned, starting, skip, min(target-have, room), now); len(picks) > 0 {
			for _, accountID := range picks {
				manager.extraStart(accountID)
			}
			return
		} else if len(starting) > 0 {
			return
		} else {
			reason = "暂无可启动的成员"
		}
	}
	manager.noteExtraIdle("INFO", reason, fmt.Sprintf("补充池 WAA Worker 预热等待 | Worker=%d/%d | 原因=%s", have, target, reason))
}

// hasKey 判断集合里有 key
func hasKey(set map[string]struct{}, key string) bool {
	_, ok := set[key]
	return ok
}

// extraPicks 从冷成员里依次挑最多 limit 个可以启动的：跳过已在补充池在途、这一轮已跳过、已带标记、正在启动、5 分钟内预热失败、
// 已有 Worker（成员缓存之后被主分区启动了）与被其他进程占用的账户
func (manager *accountWorkerManager) extraPicks(
	view *extraView, owned *extraOwnedSet, starting map[string]struct{}, skip map[string]struct{}, limit int, now time.Time,
) []string {
	failed := manager.recentWarmFailures(now)
	candidates := make([]string, 0, len(view.cold))
	for _, accountID := range view.cold {
		if hasKey(starting, accountID) || hasKey(skip, accountID) || hasKey(failed, accountID) || owned != nil && hasKey(owned.ids, accountID) {
			continue
		}
		if _, opening := manager.openingSet.Load(accountID); opening {
			continue
		}
		if account := manager.extraAccount(accountID); account == nil || account.warm.Load() {
			continue
		}
		candidates = append(candidates, accountID)
	}
	candidates, _ = manager.runtimeAvailable(candidates)
	return candidates[:min(len(candidates), limit)]
}

// extraStart 在后台按补充池预热启动一个账户：登记在途、记下发起时刻（防饿死），结束后按结果处理，成功时再补下一个。
// 管理器已关闭时什么也不做：在 manager.mu 内判断并登记 goroutine，Close 置位关闭之后不会再登记，它等的 goroutine 不会漏掉。
// 与 startReplacement 相同，先取后台 context 再判断生成服务在运行：停服时先改状态再取消后台启动（ResetAll），判断通过时取到的 context
// 一定会被随后的 ResetAll 取消；goroutine 里不再另取（停服之后 ResetAll 新建的 context 不会再被取消，在它上面启动的浏览器会一直留着）
func (manager *accountWorkerManager) extraStart(accountID string) {
	background := manager.backgroundContext()
	if background.Err() != nil || !manager.serviceRunning() {
		return
	}
	manager.mu.RLock()
	if manager.closed {
		manager.mu.RUnlock()
		return
	}
	manager.extraLaunches.Add(1)
	manager.mu.RUnlock()
	manager.extraMu.Lock()
	if manager.extraStarting == nil {
		manager.extraStarting = make(map[string]struct{})
	}
	manager.extraStarting[accountID] = struct{}{}
	manager.extraMu.Unlock()
	manager.extraLastLaunch.Store(time.Now().UnixNano())
	go func() {
		defer manager.extraLaunches.Done()
		ctx := withExtraStart(withWorkerStartSource(withBackgroundStartup(background), workerStartPrewarm),
			extraStartOptions{prewarm: true})
		taskCtx, cancel := context.WithTimeout(ctx, time.Duration(manager.initTimeout.Load())+warmTaskGrace)
		_, err := manager.ensureWorker(taskCtx, accountID, "", false)
		cancel()
		manager.extraMu.Lock()
		delete(manager.extraStarting, accountID)
		manager.extraMu.Unlock()
		if manager.extraLaunchDone(ctx, accountID, err) {
			manager.extraLaunch(manager.extraView.Load())
		}
	}()
}

// extraLaunchDone 处理一次补充池预热的结果，返回要不要接着补下一个：成功与启动失败（记入预热失败，5 分钟内不再选它）时接着补；
// 槽位已满、已在启动、已不是成员与被其他进程占用不算失败，这一轮不再选它，等下一轮对账；出口闸门暂停时 extraEgressBackoff 内不再发起。
// 这次的后台 context 已取消或生成服务已不在运行（停服时启动恰好成功，随后的 ResetAll 会关闭它）时不接着补
func (manager *accountWorkerManager) extraLaunchDone(ctx context.Context, accountID string, err error) bool {
	switch {
	case err == nil:
		manager.clearWarmFailure(accountID)
		return ctx.Err() == nil && manager.serviceRunning()
	case ctx.Err() != nil:
		return false
	case errors.Is(err, errEgressColdStartPaused):
		manager.extraPausedUntil.Store(time.Now().Add(extraEgressBackoff).UnixNano())
		return false
	case errors.Is(err, errAccountWorkerCapacity), errors.Is(err, errAccountWorkerOpening), errors.Is(err, errExtraMemberGone),
		errors.Is(err, errAccountRuntimeBusy):
		manager.extraMu.Lock()
		if manager.extraSkip == nil {
			manager.extraSkip = make(map[string]struct{})
		}
		manager.extraSkip[accountID] = struct{}{}
		manager.extraMu.Unlock()
		return false
	}
	manager.noteWarmFailure(accountID)
	manager.noteExtraIdle("WARN", "预热失败: "+err.Error(), fmt.Sprintf("补充池 WAA Worker 预热失败 | 错误=%v", err))
	return true
}

// extraExpandInBackground 为请求触发的补充池扩容（照 expandInBackground）：ctx 带补充池启动选项（进入口径），来源仍是扩容，
// 冷启动名额按按需启动计（有请求在等它，主号池的优先由插入点顺序保证）。不等启动占位：补充池已满时有可替换的补充池空闲 Worker 就按替换启动，
// 没有就按容量已满结束，请求继续排队。返回的通道在这次扩容结束时关闭
func (manager *accountWorkerManager) extraExpandInBackground(accountID string, modelID string) <-chan struct{} {
	ctx := withExtraStart(withWorkerStartSource(manager.backgroundContext(), workerStartExpand), extraStartOptions{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, err := manager.ensureWorker(ctx, accountID, modelID, false)
		if err == nil || ctx.Err() != nil || errors.Is(err, errAccountWorkerOpening) || errors.Is(err, errAccountWorkerCapacity) ||
			errors.Is(err, errAccountRuntimeBusy) || errors.Is(err, errExtraMemberGone) {
			return
		}
		manager.requests.log(accountID, "WARN", fmt.Sprintf("补充池 WAA Worker 后台扩容失败 | 错误=%v", err))
	}()
	return done
}

// extraStatus 返回状态接口的补充池概况；补充池关闭且没有补充池 Worker 时为 nil（不输出）。计数读成员缓存，不在状态请求里对账户分类；
// Worker 计数按叠加视图（members 为调用方取的叠加视图快照）
func (manager *accountWorkerManager) extraStatus(
	members aistudio.PartitionSet, warm []string, starting []string, occupied partitionCounts, unschedulable partitionCounts,
) *api.AdminExtraPool {
	settings := manager.extraSettingsValue()
	owned := manager.extraOwnedIDs()
	if !settings.enabled && len(owned) == 0 {
		return nil
	}
	status := &api.AdminExtraPool{
		Enabled: settings.enabled, Mode: settings.mode, DefaultPaid: settings.defaultPaid, IncludeUltra: settings.includeUltra,
		Borrows: manager.extraBorrows.Load(), Lendable: len(manager.extraLendableIDs()),
	}
	status.Workers = manager.partitionWorkerCounts(aistudio.PoolScopeExtra, members, warm, starting, occupied[aistudio.PoolScopeExtra], 0)
	status.Workers.Unschedulable = unschedulable[aistudio.PoolScopeExtra]
	view := manager.extraView.Load()
	status.Workers.Target = manager.extraTarget(view)
	if view != nil {
		status.Members, status.Free, status.Pro, status.Ultra = view.members, view.free, view.pro, view.ultra
		status.ColdMembers = view.coldCount
		for _, accountID := range owned {
			if hasKey(view.returning, accountID) {
				status.Returning++
			}
		}
	}
	return status
}
