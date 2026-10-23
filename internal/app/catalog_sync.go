package app

import (
	"container/heap"
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/api"
)

// 模型目录的后台调度：目录写盘缓存（catalog_cache.go）恢复后，只低速刷新缺失、恢复时指纹不一致、疑似登录失效、
// 失败待重试与过期（24 小时 ±10%）的账户，并且只取空闲账户（TryRefreshAccountModels，不让正在服务长流的热 Worker
// 暂停接新请求）。替换原来的启动全量 fan-out（32 并发，1.24 万账户约 3.5 万个请求、6–13 分钟，诊断 diag_busy_2141）
// 与失败账户每 30 秒全量重试：
//   - 速率：常规 2 并发、两次开始至少间隔 1 秒；没有缓存时（首次升级、缓存损坏）填充期 4 并发、间隔 500 毫秒；
//   - 上游 429（及连不上代理）全局暂停 30 秒起指数增长到 30 分钟，连续 10 次成功后清零；出口限流时该出口上的账户推后；
//   - 其他失败按账户退避 1 分钟起到 6 小时；需要登录的账户移出队列。
//
// 管理操作（验证、登录、导入、批量启用）照旧立即同步（syncNow 与 refreshAccountModelCatalog），不受暂停与速率限制

// catalogPriority 为后台同步任务的优先级（数值小的先取）
type catalogPriority uint8

const (
	catalogPriorityMissing   catalogPriority = iota // 启用、就绪、没有目录
	catalogPriorityStale                            // 恢复时指纹不一致（或 Build 目录没有恢复）
	catalogPriorityAuthProbe                        // 疑似登录失效（缓存记过需要登录、认证文件没变）
	catalogPriorityRetry                            // 失败后按账户退避
	catalogPriorityExpired                          // 上次成功同步 + TTL×U(0.9,1.1) 已到
	catalogPriorityCount
)

const (
	// catalogSyncTick 为调度器的最长休眠：任务完成与暂停结束时会提前唤醒
	catalogSyncTick = time.Second
	// catalogSyncConcurrency 与 catalogSyncInterval 为常规速率：约每秒 1 个账户、每秒 2–3 个请求（原来启动时 30–60 个/秒）
	catalogSyncConcurrency = 2
	catalogSyncInterval    = time.Second
	// catalogFillConcurrency 与 catalogFillInterval 为填充速率：有缺失任务、且有目录的启用账户少于 catalogFillTarget 时使用，
	// 约 8 分钟填满 1000 个账户。这是对“2 并发、≤1 账户/秒”的有意偏离：只在没有缓存时出现，避免首次启动后可服务账户
	// 长时间不足；同样受 429 与出口暂停约束
	catalogFillConcurrency = 4
	catalogFillInterval    = 500 * time.Millisecond
	catalogFillTarget      = 1000
	// catalogTTL 为目录的有效期：1.24 万账户 24 小时轮一遍只需 0.14 个/秒，远低于速率上限
	catalogTTL = 24 * time.Hour
	// catalogBusyRetry 为账户有进行中的请求时推后的时间（±20%）
	catalogBusyRetry = 2 * time.Minute
	// catalogEgressRetry 为账户所在出口限流时推后的时间
	catalogEgressRetry = 30 * time.Second
	// catalogPauseBase 与 catalogPauseMax 为上游 429 时的全局暂停（×2^k，±20%）；catalogPauseResetSuccesses 次连续成功后 k 归零
	catalogPauseBase           = 30 * time.Second
	catalogPauseMax            = 30 * time.Minute
	catalogPauseResetSuccesses = 10
	// catalogRetryBase 与 catalogRetryMax 为其他失败的按账户退避（×2^(f-1)，±20%）
	catalogRetryBase = time.Minute
	catalogRetryMax  = 6 * time.Hour
	// catalogEmptyAcceptSpan 与账户池的空目录规则一致（aistudio 的 noteCatalogListing）：第 3 次空结果不早于第一次空结果后 1 小时，
	// 保证“连续 3 次且跨 1 小时”能在退避节奏下达到
	catalogEmptyAcceptSpan = time.Hour
	// catalogSummaryInterval 为“模型目录后台同步”汇总日志的间隔，catalogReconcileInterval 为对照账户池补入新任务的间隔
	catalogSummaryInterval   = 5 * time.Minute
	catalogReconcileInterval = time.Minute
	// catalogStatusCacheTTL 为状态接口里需要遍历任务的计数（过期、排队）的缓存时间
	catalogStatusCacheTTL = 5 * time.Second
	// catalogEgressPauseShown 为出口限流推后任务后，状态里“原因=出口限流”保留的时间
	catalogEgressPauseShown = time.Minute
)

// 暂停原因（状态接口与日志）
const (
	catalogPauseUpstream = "上游 429"
	catalogPauseEgress   = "出口限流"
)

// catalogTryRefresher 为只在账户空闲时同步的目录服务（*aistudio.PooledService 实现）；没有实现时退回 RefreshAccountModels
type catalogTryRefresher interface {
	TryRefreshAccountModels(ctx context.Context, accountID string) ([]aistudio.Model, bool, error)
}

// catalogSnapshotter 为给出账户当前目录的服务（*aistudio.PooledService 实现），没有实现时不写缓存
type catalogSnapshotter interface {
	CatalogSnapshot(accountID string) (aistudio.CatalogSnapshot, bool)
}

// catalogTask 为一个账户的同步任务
type catalogTask struct {
	accountID string
	priority  catalogPriority
	due       time.Time
	// index 为任务在所属优先级堆里的下标，不在堆里时为 -1；running 为后台正在执行（执行期间不在堆里）
	index    int
	running  bool
	failures int
}

// catalogTaskHeap 为同一优先级的任务按 due 排的小顶堆
type catalogTaskHeap []*catalogTask

func (h catalogTaskHeap) Len() int           { return len(h) }
func (h catalogTaskHeap) Less(i, j int) bool { return h[i].due.Before(h[j].due) }
func (h catalogTaskHeap) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
	h[i].index = i
	h[j].index = j
}

func (h *catalogTaskHeap) Push(value any) {
	task := value.(*catalogTask)
	task.index = len(*h)
	*h = append(*h, task)
}

func (h *catalogTaskHeap) Pop() any {
	old := *h
	task := old[len(old)-1]
	old[len(old)-1] = nil
	task.index = -1
	*h = old[:len(old)-1]
	return task
}

// catalogStatBucket 为一分钟（或一个汇总周期）内的同步计数
type catalogStatBucket struct {
	minute                                   int64
	synced, failed, rateLimited, empty, busy int
}

// catalogSyncStats 为 1 小时滚动计数（按分钟分桶）与 5 分钟汇总
type catalogSyncStats struct {
	buckets [60]catalogStatBucket
	summary catalogStatBucket
}

func (stats *catalogSyncStats) add(now time.Time, change func(*catalogStatBucket)) {
	minute := now.Unix() / 60
	bucket := &stats.buckets[minute%int64(len(stats.buckets))]
	if bucket.minute != minute {
		*bucket = catalogStatBucket{minute: minute}
	}
	change(bucket)
	change(&stats.summary)
}

func (stats *catalogSyncStats) hour(now time.Time) catalogStatBucket {
	var total catalogStatBucket
	minute := now.Unix() / 60
	for _, bucket := range stats.buckets {
		if age := minute - bucket.minute; age >= 0 && age < int64(len(stats.buckets)) {
			total.synced += bucket.synced
			total.failed += bucket.failed
			total.rateLimited += bucket.rateLimited
			total.empty += bucket.empty
			total.busy += bucket.busy
		}
	}
	return total
}

// catalogStatusCounts 为状态与日志用的任务计数
type catalogStatusCounts struct {
	missing, stale, probe, retry, expired, queued int
}

// catalogSyncer 为模型目录的后台调度器；方法全部 nil 安全（测试直接构造的服务没有调度器）
type catalogSyncer struct {
	service *trackedService
	// cache 为写盘缓存，nil 时只在内存（测试）
	cache *catalogCache
	mu    sync.Mutex
	tasks map[string]*catalogTask
	heaps [catalogPriorityCount]catalogTaskHeap
	// running 为正在执行的后台任务数，lastStart 为最近一次开始的时间
	running   int
	lastStart time.Time
	// 上游 429 的全局暂停：pausedUntil 为暂停到，pauseStarted 为本次暂停开始（恢复时写日志后清零），pauseLevel 为指数 k
	pausedUntil   time.Time
	pauseStarted  time.Time
	pauseLevel    int
	successStreak int
	// egressDeferredAt 为最近一次因出口限流推后任务的时间（状态里的“原因=出口限流”）
	egressDeferredAt time.Time
	fromDisk         int
	stale            map[string]struct{}
	suspects         map[string]struct{}
	stats            catalogSyncStats
	// cachedAccounts 与 cachedEnabled 为有目录的账户与其中启用的账户（对照账户池时更新，缺失账户同步成功时累加）
	cachedAccounts, cachedEnabled int
	lastReconcile, lastSummary    time.Time
	// 启动后第一轮缺失任务（ready 与 synced 的依据）：round 为还没有结果的账户
	round         map[string]struct{}
	roundStarted  time.Time
	roundSynced   int
	roundNonEmpty int
	roundFailures []error
	ready         chan error
	readySent     bool
	synced        chan struct{}
	syncedClosed  bool
	// 后台成功结果合并应用：首个立即，之后最多每 catalogApplyInterval 一次
	lastApplied  time.Time
	applyPending bool
	// statusCache 为需要遍历任务的计数的缓存，statusCacheAt 为零时下次重算
	statusCache   catalogStatusCounts
	statusCacheAt time.Time
	wake          chan struct{}
	// loopDone 为上一次 start 的调度 goroutine 的 done：再次启动前等它退出，同一时刻只有一个调度 goroutine
	loopDone <-chan struct{}
	// 以下只给包内测试改小；零值为默认
	interval, fillInterval, busyRetry, tick, ttl time.Duration
	concurrency, fillConcurrency, fillTarget     int
	now                                          func() time.Time
	random                                       func() float64
	// egressPaused 替代出口判定器的 catalogPaused（只给包内测试）
	egressPaused func(key string) bool
}

// newCatalogSyncer 创建调度器（不带磁盘缓存，newRuntime 用 attach 挂上）
func newCatalogSyncer(service *trackedService) *catalogSyncer {
	return &catalogSyncer{
		service: service, tasks: make(map[string]*catalogTask),
		stale: make(map[string]struct{}), suspects: make(map[string]struct{}),
		wake: make(chan struct{}, 1),
	}
}

// attach 挂上写盘缓存与启动恢复的结果：恢复时指纹不一致的账户排在缺失之后，疑似失效的账户排在其后、预热时排最后
func (syncer *catalogSyncer) attach(cache *catalogCache, summary catalogRestoreSummary) {
	if syncer == nil {
		return
	}
	syncer.mu.Lock()
	defer syncer.mu.Unlock()
	syncer.cache = cache
	syncer.fromDisk = summary.FromDisk
	for accountID := range summary.Stale {
		syncer.stale[accountID] = struct{}{}
	}
	for accountID := range summary.Suspects {
		syncer.suspects[accountID] = struct{}{}
	}
	syncer.publishSuspectsLocked()
}

func (syncer *catalogSyncer) clock() time.Time {
	if syncer.now != nil {
		return syncer.now()
	}
	return time.Now()
}

// jitter 返回 U(1-spread, 1+spread) 的系数
func (syncer *catalogSyncer) jitter(spread float64) float64 {
	value := rand.Float64()
	if syncer.random != nil {
		value = syncer.random()
	}
	return 1 - spread + 2*spread*value
}

func (syncer *catalogSyncer) ttlValue() time.Duration {
	if syncer.ttl > 0 {
		return syncer.ttl
	}
	return catalogTTL
}

func (syncer *catalogSyncer) tickValue() time.Duration {
	if syncer.tick > 0 {
		return syncer.tick
	}
	return catalogSyncTick
}

func (syncer *catalogSyncer) busyRetryValue() time.Duration {
	if syncer.busyRetry > 0 {
		return syncer.busyRetry
	}
	return catalogBusyRetry
}

// expiryDue 返回成功同步后的下次过期刷新时间：TTL×U(0.9,1.1)，避免同一批同步的账户同时到期
func (syncer *catalogSyncer) expiryDue(syncedAt time.Time) time.Time {
	return syncedAt.Add(time.Duration(float64(syncer.ttlValue()) * syncer.jitter(0.1)))
}

// rateLocked 返回当前的并发上限与开始间隔：有缺失任务、且有目录的启用账户不到填充目标时用填充速率
func (syncer *catalogSyncer) rateLocked() (int, time.Duration) {
	concurrency, interval := catalogSyncConcurrency, catalogSyncInterval
	if syncer.concurrency > 0 {
		concurrency = syncer.concurrency
	}
	if syncer.interval > 0 {
		interval = syncer.interval
	}
	target := catalogFillTarget
	if syncer.fillTarget > 0 {
		target = syncer.fillTarget
	}
	if syncer.heaps[catalogPriorityMissing].Len() > 0 && syncer.cachedEnabled < target {
		concurrency, interval = catalogFillConcurrency, catalogFillInterval
		if syncer.fillConcurrency > 0 {
			concurrency = syncer.fillConcurrency
		}
		if syncer.fillInterval > 0 {
			interval = syncer.fillInterval
		}
	}
	return concurrency, interval
}

// kick 唤醒调度 goroutine
func (syncer *catalogSyncer) kick() {
	if syncer == nil {
		return
	}
	select {
	case syncer.wake <- struct{}{}:
	default:
	}
}

// scheduleLocked 把任务放进（或移到）priority 的堆；后台运行中的任务只记下优先级与时间，结束时再入堆。调用方持有 syncer.mu
func (syncer *catalogSyncer) scheduleLocked(task *catalogTask, priority catalogPriority, due time.Time) {
	if task.index >= 0 {
		heap.Remove(&syncer.heaps[task.priority], task.index)
	}
	task.priority, task.due = priority, due
	if !task.running {
		heap.Push(&syncer.heaps[priority], task)
	}
	syncer.statusCacheAt = time.Time{}
}

// removeLocked 移除账户的任务；调用方持有 syncer.mu
func (syncer *catalogSyncer) removeLocked(accountID string) {
	task := syncer.tasks[accountID]
	if task == nil {
		return
	}
	if task.index >= 0 {
		heap.Remove(&syncer.heaps[task.priority], task.index)
	}
	delete(syncer.tasks, accountID)
	syncer.statusCacheAt = time.Time{}
}

// taskLocked 返回账户的任务，没有时新建（不入堆）：账户没有目录时为缺失，否则为失败待重试；调用方持有 syncer.mu
func (syncer *catalogSyncer) taskLocked(accountID string) *catalogTask {
	task := syncer.tasks[accountID]
	if task == nil {
		task = &catalogTask{accountID: accountID, index: -1, priority: catalogPriorityRetry}
		if status, ok := syncer.service.pool.StatusOf(accountID); !ok || len(status.Models) == 0 {
			task.priority = catalogPriorityMissing
		}
		syncer.tasks[accountID] = task
	}
	return task
}

// classifyLocked 返回新任务的优先级、时间与连续失败次数：没有目录为缺失（不早于缓存记的下次重试），
// 恢复时不一致、疑似失效、失败待重试依次排后，其余按上次成功同步 + TTL×U(0.9,1.1)。调用方持有 syncer.mu
func (syncer *catalogSyncer) classifyLocked(accountID string, hasCatalog bool, now time.Time) (catalogPriority, time.Time, int) {
	entry, cached := syncer.cache.entryOf(accountID)
	retryAt := unixTime(entry.NextRetry)
	if !hasCatalog {
		return catalogPriorityMissing, laterOf(now, retryAt), entry.Failures
	}
	if _, stale := syncer.stale[accountID]; stale {
		return catalogPriorityStale, now, 0
	}
	if _, suspect := syncer.suspects[accountID]; suspect {
		return catalogPriorityAuthProbe, now, 0
	}
	if cached && entry.Failures > 0 {
		return catalogPriorityRetry, laterOf(now, retryAt), entry.Failures
	}
	syncedAt := now
	if cached && entry.At > 0 {
		syncedAt = unixTime(entry.At)
	}
	return catalogPriorityExpired, syncer.expiryDue(syncedAt), 0
}

func laterOf(left time.Time, right time.Time) time.Time {
	if right.After(left) {
		return right
	}
	return left
}

// catalogEligible 判断账户可以后台同步：启用，且状态为就绪（含忙碌与冷却的派生状态）。
// 需要登录、停用、不可用与不存在的账户移出队列，恢复就绪后由对照账户池或 syncNow 重新入队
func catalogEligible(status aistudio.AccountStatus) bool {
	if !status.Enabled {
		return false
	}
	switch status.State {
	case aistudio.AccountReady, aistudio.AccountBusy, aistudio.AccountCooldown:
		return true
	default:
		return false
	}
}

// reconcile 对照账户池：给可同步却没有任务的账户补任务，并更新有目录的账户数。账户池状态在锁外读取
func (syncer *catalogSyncer) reconcile(now time.Time) {
	statuses := syncer.service.pool.Status()
	syncer.mu.Lock()
	defer syncer.mu.Unlock()
	syncer.lastReconcile = now
	cachedAccounts, cachedEnabled := 0, 0
	droppedSuspect := false
	for _, status := range statuses {
		hasCatalog := len(status.Models) > 0
		if hasCatalog {
			cachedAccounts++
			if status.Enabled {
				cachedEnabled++
			}
		}
		if status.State == aistudio.AccountAuthRequired {
			// 已确认需要登录（aistudio 内部标记、没有经 app 的 noteAuthRequired，账户也可能没有任务）：不再是“疑似失效、等待探测”
			if _, suspect := syncer.suspects[status.ID]; suspect {
				delete(syncer.suspects, status.ID)
				droppedSuspect = true
			}
		}
		if !catalogEligible(status) {
			continue
		}
		if _, exists := syncer.tasks[status.ID]; exists {
			continue
		}
		task := &catalogTask{accountID: status.ID, index: -1}
		syncer.tasks[status.ID] = task
		priority, due, failures := syncer.classifyLocked(status.ID, hasCatalog, now)
		task.failures = failures
		syncer.scheduleLocked(task, priority, due)
	}
	syncer.cachedAccounts, syncer.cachedEnabled = cachedAccounts, cachedEnabled
	if droppedSuspect {
		syncer.publishSuspectsLocked()
	}
}

// countsLocked 返回任务计数（遍历任务，按 catalogStatusCacheTTL 缓存）；调用方持有 syncer.mu
func (syncer *catalogSyncer) countsLocked(now time.Time) catalogStatusCounts {
	if !syncer.statusCacheAt.IsZero() && !now.Before(syncer.statusCacheAt) && now.Sub(syncer.statusCacheAt) < catalogStatusCacheTTL {
		return syncer.statusCache
	}
	var counts catalogStatusCounts
	for _, task := range syncer.tasks {
		due := !task.due.After(now)
		switch task.priority {
		case catalogPriorityMissing:
			counts.missing++
		case catalogPriorityStale:
			counts.stale++
		case catalogPriorityAuthProbe:
			counts.probe++
		case catalogPriorityRetry:
			counts.retry++
		case catalogPriorityExpired:
			if due {
				counts.expired++
			}
		}
		if due && !task.running {
			counts.queued++
		}
	}
	syncer.statusCache, syncer.statusCacheAt = counts, now
	return counts
}

// catalogPlan 为启动时“准备模型目录”日志的计数
type catalogPlan struct {
	fromDisk, missing, expired, stale, suspects int
}

// startLine 返回启动日志“生成服务启动 | 1/2 | 准备模型目录”（“缓存=”保持第一个键，为合并后的模型数）
func (plan catalogPlan) startLine(models int) string {
	return fmt.Sprintf(
		"生成服务启动 | 1/2 | 准备模型目录 | 缓存=%d | 来自磁盘=%d | 待同步=%d | 过期=%d | 不一致=%d | 疑似失效=%d",
		models, plan.fromDisk, plan.missing, plan.expired, plan.stale, plan.suspects,
	)
}

// plan 在启动前对照账户池排好任务，返回启动日志的计数
func (syncer *catalogSyncer) plan() catalogPlan {
	if syncer == nil {
		return catalogPlan{}
	}
	now := syncer.clock()
	syncer.reconcile(now)
	syncer.mu.Lock()
	defer syncer.mu.Unlock()
	syncer.statusCacheAt = time.Time{}
	counts := syncer.countsLocked(now)
	return catalogPlan{
		fromDisk: syncer.fromDisk, missing: counts.missing, expired: counts.expired,
		stale: counts.stale, suspects: len(syncer.suspects),
	}
}

// start 启动调度 goroutine，保持 Start 原来的等待方式：
//   - ready：恢复后已有目录时立即送 nil；完全没有目录时第一个非空结果送 nil；第一轮缺失任务全部失败时送 errors.Join(失败)；
//   - synced：第一轮缺失任务都有了结果时关闭（有缓存且没有缺失时立即关闭）；
//   - done：调度 goroutine 退出、写盘完成后关闭。停止服务与重启据此等待（finishStop 等 modelRefreshDone），
//     新一代运行时恢复前，最后的同步结果已经在盘上
func (syncer *catalogSyncer) start(ctx context.Context) (<-chan error, <-chan struct{}, <-chan struct{}) {
	ready := make(chan error, 1)
	synced := make(chan struct{})
	done := make(chan struct{})
	if syncer == nil {
		ready <- nil
		close(synced)
		close(done)
		return ready, synced, done
	}
	syncer.mu.Lock()
	previous := syncer.loopDone
	syncer.loopDone = done
	syncer.mu.Unlock()
	if previous != nil {
		// 上一次停止时等调度器退出超时（finishStop 只等 2 秒）：它的 ctx 已结束，很快就会退出
		select {
		case <-previous:
		case <-ctx.Done():
		}
	}
	now := syncer.clock()
	syncer.reconcile(now)
	hasModels := len(syncer.service.catalog.CachedModels()) > 0
	syncer.mu.Lock()
	syncer.ready, syncer.readySent = ready, false
	syncer.synced, syncer.syncedClosed = synced, false
	syncer.round = make(map[string]struct{})
	if !hasModels {
		// 一个目录都没有恢复出来：服务没有目录无法工作，重启当作一次重试，缺失任务不再等重启前记下的退避（最长 6 小时），
		// 与原先启动时全量同步相同；否则 Start 一直等 ready，到这些任务到期都没有任何报错
		var backoff []*catalogTask
		for _, task := range syncer.heaps[catalogPriorityMissing] {
			if task.due.After(now) {
				backoff = append(backoff, task)
			}
		}
		for _, task := range backoff {
			syncer.scheduleLocked(task, catalogPriorityMissing, now)
		}
	}
	for _, task := range syncer.heaps[catalogPriorityMissing] {
		// 第一轮只算现在就到期的缺失任务：重启前同步失败、还在退避中的（缓存记的下次重试）已有结果，到期后照常重试。
		// 原先也算进来，第一轮要等它们到期跑完才结束，“模型目录后台同步完成”推迟到几小时后，首个 Worker 预热一直失败时
		// Start 也不按第一轮结束报错退出
		if task.due.After(now) {
			continue
		}
		syncer.round[task.accountID] = struct{}{}
	}
	syncer.roundStarted, syncer.roundSynced, syncer.roundNonEmpty, syncer.roundFailures = now, 0, 0, nil
	syncer.lastSummary = now
	if hasModels {
		syncer.sendReadyLocked(nil)
	}
	syncer.finishRoundIfDoneLocked()
	syncer.mu.Unlock()
	go syncer.loop(ctx, done)
	return ready, synced, done
}

func (syncer *catalogSyncer) sendReadyLocked(err error) {
	if syncer.readySent || syncer.ready == nil {
		return
	}
	syncer.readySent = true
	syncer.ready <- err
}

// finishRoundIfDoneLocked 在第一轮缺失任务都有结果时关闭 synced，并写“模型目录后台同步完成”；调用方持有 syncer.mu
func (syncer *catalogSyncer) finishRoundIfDoneLocked() {
	if syncer.syncedClosed || syncer.synced == nil || len(syncer.round) > 0 {
		return
	}
	if !syncer.readySent {
		var err error = aistudio.ErrNoEligibleAccount
		if len(syncer.roundFailures) > 0 {
			err = errors.Join(syncer.roundFailures...)
		}
		syncer.sendReadyLocked(err)
	}
	syncer.syncedClosed = true
	if syncer.roundSynced > 0 || len(syncer.roundFailures) > 0 {
		pending := syncer.heaps[catalogPriorityMissing].Len() + syncer.heaps[catalogPriorityRetry].Len()
		syncer.service.requests.log("service", "INFO", fmt.Sprintf(
			"模型目录后台同步完成 | 同步=%d | 非空=%d | 模型=%d | 待重试账户=%d | 耗时=%s | 来自磁盘=%d",
			syncer.roundSynced, syncer.roundNonEmpty, len(syncer.service.modelSnapshot()), pending,
			syncer.clock().Sub(syncer.roundStarted).Round(time.Millisecond), syncer.fromDisk,
		))
	}
	close(syncer.synced)
}

// roundDoneLocked 记下第一轮缺失任务里一个账户的结果；调用方持有 syncer.mu
func (syncer *catalogSyncer) roundDoneLocked(accountID string, synced bool, nonEmpty bool, failure error) {
	if _, inRound := syncer.round[accountID]; !inRound {
		return
	}
	delete(syncer.round, accountID)
	if synced {
		syncer.roundSynced++
	}
	if nonEmpty {
		syncer.roundNonEmpty++
	}
	if failure != nil {
		syncer.roundFailures = append(syncer.roundFailures, fmt.Errorf("账户 %s: %w", accountID, failure))
	}
	syncer.finishRoundIfDoneLocked()
}

// loop 为调度 goroutine：按优先级与时间取任务，受并发、开始间隔、全局暂停与出口暂停约束；ctx 结束时等进行中的任务结束、
// 写盘后关闭 done
func (syncer *catalogSyncer) loop(ctx context.Context, done chan struct{}) {
	var tasks sync.WaitGroup
	defer func() {
		tasks.Wait()
		if err := syncer.cache.flush(); err != nil {
			syncer.service.requests.log("service", "WARN", "模型目录缓存写入失败 | 错误="+err.Error())
		}
		close(done)
	}()
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		case <-syncer.wake:
		}
		if ctx.Err() != nil {
			return
		}
		syncer.periodic(ctx)
		wait := syncer.dispatch(ctx, &tasks)
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(wait)
	}
}

// periodic 做定时的事：合并应用待应用的结果、对照账户池、5 分钟汇总
func (syncer *catalogSyncer) periodic(ctx context.Context) {
	now := syncer.clock()
	syncer.mu.Lock()
	reconcile := now.Sub(syncer.lastReconcile) >= catalogReconcileInterval
	syncer.mu.Unlock()
	if reconcile {
		syncer.reconcile(now)
	}
	syncer.mu.Lock()
	apply := syncer.applyPending && now.Sub(syncer.lastApplied) >= catalogApplyInterval
	if apply {
		syncer.applyPending = false
		syncer.lastApplied = now
	}
	summary := ""
	if now.Sub(syncer.lastSummary) >= catalogSummaryInterval {
		summary = syncer.summaryLocked(now)
	}
	syncer.mu.Unlock()
	if apply && ctx.Err() == nil {
		syncer.service.applyCachedModelCatalog()
		syncer.service.publishModelAccess()
	}
	if summary != "" {
		syncer.service.requests.log("service", "INFO", summary)
	}
}

// summaryLocked 返回 5 分钟汇总日志并清零汇总计数（这段时间没有任何同步且没有暂停时为空）；调用方持有 syncer.mu
func (syncer *catalogSyncer) summaryLocked(now time.Time) string {
	syncer.lastSummary = now
	window := syncer.stats.summary
	syncer.stats.summary = catalogStatBucket{}
	paused := now.Before(syncer.pausedUntil)
	if window.synced+window.failed+window.rateLimited+window.empty+window.busy == 0 && !paused {
		return ""
	}
	syncer.statusCacheAt = time.Time{}
	counts := syncer.countsLocked(now)
	pausedUntil, reason := "-", "-"
	if paused {
		pausedUntil, reason = syncer.pausedUntil.UTC().Format(time.RFC3339), catalogPauseUpstream
	} else if !syncer.egressDeferredAt.IsZero() && now.Sub(syncer.egressDeferredAt) < catalogEgressPauseShown {
		reason = catalogPauseEgress
	}
	return fmt.Sprintf(
		"模型目录后台同步 | 成功=%d | 失败=%d | 限流429=%d | 空目录=%d | 账户忙推后=%d | 缺失=%d | 过期=%d | 疑似失效=%d | 队列=%d | 暂停到=%s | 原因=%s",
		window.synced, window.failed, window.rateLimited, window.empty, window.busy,
		counts.missing, counts.expired, len(syncer.suspects), counts.queued, pausedUntil, reason,
	)
}

// dispatch 尽量开始任务，返回到下次需要检查的时间（不超过 tick）
func (syncer *catalogSyncer) dispatch(ctx context.Context, tasks *sync.WaitGroup) time.Duration {
	tick := syncer.tickValue()
	for {
		syncer.mu.Lock()
		now := syncer.clock()
		if now.Before(syncer.pausedUntil) {
			wait := min(syncer.pausedUntil.Sub(now), tick)
			syncer.mu.Unlock()
			return wait
		}
		if !syncer.pauseStarted.IsZero() {
			paused := syncer.pausedUntil.Sub(syncer.pauseStarted)
			syncer.pauseStarted = time.Time{}
			syncer.service.requests.logDurable("service", "INFO", fmt.Sprintf(
				"模型目录同步恢复 | 暂停=%s", paused.Round(time.Second),
			))
		}
		concurrency, interval := syncer.rateLocked()
		if syncer.running >= concurrency {
			syncer.mu.Unlock()
			return tick
		}
		if !syncer.lastStart.IsZero() {
			if next := syncer.lastStart.Add(interval); now.Before(next) {
				syncer.mu.Unlock()
				return min(next.Sub(now), tick)
			}
		}
		task, wait := syncer.nextLocked(now)
		if task == nil {
			syncer.mu.Unlock()
			return min(wait, tick)
		}
		heap.Remove(&syncer.heaps[task.priority], task.index)
		task.running = true
		syncer.running++
		syncer.mu.Unlock()

		// 锁外核对账户状态与出口：不可同步的移出，出口限流的推后，都不占开始间隔
		status, exists := syncer.service.pool.StatusOf(task.accountID)
		if !exists || !catalogEligible(status) {
			syncer.mu.Lock()
			syncer.running--
			task.running = false
			if syncer.tasks[task.accountID] == task {
				delete(syncer.tasks, task.accountID)
			}
			if !exists || status.State == aistudio.AccountAuthRequired {
				// 已删除或已确认需要登录（例如由登录复核确认、aistudio 内部标记）：不再是“疑似失效、等待探测”
				syncer.dropSuspectLocked(task.accountID)
			}
			syncer.statusCacheAt = time.Time{}
			syncer.roundDoneLocked(task.accountID, false, false, nil)
			syncer.mu.Unlock()
			continue
		}
		// 疑似登录失效、等待登录复核的账户先不同步（复核前重复打 401 没有意义），复核有结论后照常（复核恢复的账户由 requeue 放回）
		if syncer.service.authRecheck.suspected(task.accountID) {
			syncer.mu.Lock()
			syncer.running--
			task.running = false
			if syncer.tasks[task.accountID] == task {
				syncer.scheduleLocked(task, task.priority, now.Add(syncer.busyRetryValue()))
			}
			syncer.mu.Unlock()
			continue
		}
		if syncer.catalogPausedFor(task.accountID) {
			syncer.mu.Lock()
			syncer.running--
			task.running = false
			syncer.egressDeferredAt = now
			if syncer.tasks[task.accountID] == task {
				syncer.scheduleLocked(task, task.priority, now.Add(catalogEgressRetry))
			}
			syncer.mu.Unlock()
			continue
		}
		syncer.mu.Lock()
		syncer.lastStart = now
		syncer.mu.Unlock()
		tasks.Add(1)
		go func(accountID string) {
			defer tasks.Done()
			_, _, _ = syncer.service.refreshAccountModelCatalogWith(ctx, accountID, true)
			syncer.kick()
		}(task.accountID)
	}
}

// catalogPausedFor 返回账户所在出口的目录同步是否暂停（出口限流判定成立期间）
func (syncer *catalogSyncer) catalogPausedFor(accountID string) bool {
	key, _ := syncer.service.pool.EgressKeyOf(accountID)
	if syncer.egressPaused != nil {
		return syncer.egressPaused(key)
	}
	return syncer.service.egress.catalogPaused(key)
}

// nextLocked 返回优先级最高、已到时间的任务；没有时返回到最早一个任务的时间。调用方持有 syncer.mu
func (syncer *catalogSyncer) nextLocked(now time.Time) (*catalogTask, time.Duration) {
	var earliest time.Time
	for priority := range syncer.heaps {
		queue := syncer.heaps[priority]
		if queue.Len() == 0 {
			continue
		}
		top := queue[0]
		if !top.due.After(now) {
			return top, 0
		}
		if earliest.IsZero() || top.due.Before(earliest) {
			earliest = top.due
		}
	}
	if earliest.IsZero() {
		return nil, time.Hour
	}
	return nil, earliest.Sub(now)
}

// syncNow 为管理操作（P0）：立即同步，不受全局暂停与速率限制；结果照常写缓存、排任务
func (syncer *catalogSyncer) syncNow(ctx context.Context, accountID string) ([]aistudio.Model, error) {
	models, _, err := syncer.service.refreshAccountModelCatalogWith(ctx, accountID, false)
	return models, err
}

// forget 移除账户的任务与嫌疑（账户删除或移出号池后）
func (syncer *catalogSyncer) forget(accountID string) {
	if syncer == nil {
		return
	}
	syncer.mu.Lock()
	defer syncer.mu.Unlock()
	syncer.removeLocked(accountID)
	delete(syncer.stale, accountID)
	if _, suspect := syncer.suspects[accountID]; suspect {
		delete(syncer.suspects, accountID)
		syncer.publishSuspectsLocked()
	}
	if _, inRound := syncer.round[accountID]; inRound {
		delete(syncer.round, accountID)
		syncer.finishRoundIfDoneLocked()
	}
}

// noteAuthRequired 记录账户被标为需要登录：移出疑似失效；stat 一次认证文件，把时间与当时的修改时间写进缓存（ar/am）。
// 下次启动时认证文件没变过的账户列为疑似失效，预热排最后、优先低速探测；syncer 为 nil 时不处理，没有缓存时只移出嫌疑
func (syncer *catalogSyncer) noteAuthRequired(accountID string, at time.Time) {
	if syncer == nil {
		return
	}
	// 已确认需要登录：移出疑似失效（预热排序与“疑似失效=”计数只算还没有结论的），之后由登录复核或人工恢复处理
	syncer.mu.Lock()
	syncer.dropSuspectLocked(accountID)
	syncer.mu.Unlock()
	if syncer.cache == nil {
		return
	}
	var modTime time.Time
	if account, err := syncer.service.pool.Account(accountID); err == nil {
		modTime, _ = fileModTime(account.StoragePath)
	}
	syncer.cache.noteAuthRequired(accountID, at, modTime)
}

// requeue 把账户放回后台同步队列、立即到期（登录复核恢复了需要登录的账户：它在需要登录期间已被移出队列）；
// syncer 为 nil 时不处理
func (syncer *catalogSyncer) requeue(accountID string) {
	if syncer == nil {
		return
	}
	now := syncer.clock()
	syncer.mu.Lock()
	task := syncer.taskLocked(accountID)
	priority := task.priority
	if priority == catalogPriorityExpired {
		priority = catalogPriorityRetry
	}
	syncer.scheduleLocked(task, priority, now)
	syncer.mu.Unlock()
	syncer.kick()
}

// noteAuthValid 记录账户登录已确认有效（管理验证通过）：移出嫌疑并清掉缓存里的需要登录记录（ar/am）。
// 随后的目录同步若失败（如上游 429），账户也不再按疑似失效排在预热最后，下次启动也不再列为疑似失效；syncer 为 nil 时不处理
func (syncer *catalogSyncer) noteAuthValid(accountID string) {
	if syncer == nil {
		return
	}
	syncer.mu.Lock()
	syncer.dropSuspectLocked(accountID)
	syncer.mu.Unlock()
	if syncer.cache != nil {
		syncer.cache.clearAuthRequired(accountID)
	}
}

// publishSuspectsLocked 把疑似失效账户交给 Worker 管理器（预热时排最后）；调用方持有 syncer.mu
func (syncer *catalogSyncer) publishSuspectsLocked() {
	if syncer.service == nil || syncer.service.workers == nil {
		return
	}
	suspects := make(map[string]struct{}, len(syncer.suspects))
	for accountID := range syncer.suspects {
		suspects[accountID] = struct{}{}
	}
	syncer.service.workers.authSuspects.Store(&suspects)
}

// dropSuspectLocked 移出嫌疑；调用方持有 syncer.mu
func (syncer *catalogSyncer) dropSuspectLocked(accountID string) {
	if _, suspect := syncer.suspects[accountID]; suspect {
		delete(syncer.suspects, accountID)
		syncer.publishSuspectsLocked()
	}
}

// afterRefresh 处理一次单账户同步的结果（后台任务与管理操作共用）：成功写缓存、排下次过期刷新；失败按种类全局暂停、
// 按账户退避或移出队列。background 为调度器开始的后台任务
func (syncer *catalogSyncer) afterRefresh(ctx context.Context, accountID string, models []aistudio.Model, busy bool, err error, background bool) {
	var snapshot aistudio.CatalogSnapshot
	var snapshotOK bool
	if err == nil && !busy && ctx.Err() == nil {
		if snapshotter, ok := syncer.service.catalog.(catalogSnapshotter); ok {
			snapshot, snapshotOK = snapshotter.CatalogSnapshot(accountID)
		}
	}
	now := syncer.clock()
	syncer.mu.Lock()
	task := syncer.tasks[accountID]
	if background {
		syncer.running--
		if task != nil {
			task.running = false
		}
	}
	if ctx.Err() != nil {
		// 停止服务或调用方放弃：放回原处，不记结果
		if task != nil && task.index < 0 && !task.running {
			heap.Push(&syncer.heaps[task.priority], task)
		}
		syncer.mu.Unlock()
		return
	}
	switch {
	case busy:
		syncer.stats.add(now, func(bucket *catalogStatBucket) { bucket.busy++ })
		task = syncer.taskLocked(accountID)
		syncer.scheduleLocked(task, task.priority, now.Add(time.Duration(float64(syncer.busyRetryValue())*syncer.jitter(0.2))))
		syncer.mu.Unlock()
	case err == nil:
		syncer.succeededLocked(accountID, models, snapshot, snapshotOK, now, background)
	case aistudio.DefinitiveAuthenticationFailure(err):
		syncer.authRequiredLocked(accountID, err, now)
	case aistudio.CatalogRateLimited(err):
		syncer.rateLimitedLocked(accountID, err, now)
	case errors.Is(err, aistudio.ErrEmptyModelCatalog):
		syncer.emptyLocked(accountID, err, now)
	default:
		syncer.failedLocked(accountID, err, now)
	}
}

// succeededLocked 处理成功：写缓存（同时清掉失败与需要登录记录），移出嫌疑，排下次过期刷新，后台结果合并应用。
// 调用方持有 syncer.mu，返回前释放
func (syncer *catalogSyncer) succeededLocked(
	accountID string,
	models []aistudio.Model,
	snapshot aistudio.CatalogSnapshot,
	snapshotOK bool,
	now time.Time,
	background bool,
) {
	task := syncer.taskLocked(accountID)
	wasMissing := task.priority == catalogPriorityMissing
	task.failures = 0
	delete(syncer.stale, accountID)
	syncer.dropSuspectLocked(accountID)
	syncer.successStreak++
	if syncer.successStreak >= catalogPauseResetSuccesses {
		syncer.pauseLevel = 0
	}
	syncer.stats.add(now, func(bucket *catalogStatBucket) { bucket.synced++ })
	acceptedEmpty := len(models) == 0
	if acceptedEmpty {
		// 接受了空目录（账户原来没有目录，或已满“连续 3 次且跨 1 小时”）：按失败的退避节奏再试，不在每个周期都重打
		task.failures++
		syncer.scheduleLocked(task, catalogPriorityMissing, now.Add(syncer.retryDelay(task.failures)))
	} else {
		if wasMissing {
			syncer.cachedEnabled++
			syncer.cachedAccounts++
		}
		syncer.scheduleLocked(task, catalogPriorityExpired, syncer.expiryDue(now))
	}
	// 启动在等 ready 时，首个非空结果立即应用（Start 收到 ready 后马上读目录快照）；之后后台结果最多每 catalogApplyInterval
	// 合并应用一次。管理操作由调用方自己应用（批量启用整批只应用一次），这里不逐个账户重算全量目录
	waitingReady := syncer.ready != nil && !syncer.readySent
	apply := false
	if len(models) > 0 && (waitingReady || background) {
		if waitingReady || syncer.lastApplied.IsZero() || now.Sub(syncer.lastApplied) >= catalogApplyInterval {
			apply = true
			syncer.lastApplied = now
			syncer.applyPending = false
		} else {
			syncer.applyPending = true
		}
	}
	cache := syncer.cache
	syncer.mu.Unlock()
	if snapshotOK {
		cache.put(accountID, snapshot, now)
	}
	if acceptedEmpty && snapshotOK && snapshot.EmptyCount > 0 {
		syncer.service.requests.log(accountID, "WARN", fmt.Sprintf(
			"模型目录为空，已接受 | 连续空结果=%d | 首次为空=%s 前",
			snapshot.EmptyCount, now.Sub(snapshot.EmptySince).Round(time.Second),
		))
	}
	if apply {
		syncer.service.applyCachedModelCatalog()
		syncer.service.publishModelAccess()
	}
	// 先应用再送 ready、再记第一轮结果：第一轮在这个账户结束时，ready 已经是成功
	syncer.mu.Lock()
	if len(models) > 0 {
		syncer.sendReadyLocked(nil)
	}
	syncer.roundDoneLocked(accountID, true, len(models) > 0, nil)
	syncer.mu.Unlock()
}

// authRequiredLocked 处理登录失效（401 / Code 16，modelsForLease 已把账户标为需要登录）：移出队列与嫌疑，
// 由 noteAuthRequired 统一写“账号需要登录 | 来源=目录同步”（不再另写失败日志）。运行时上游 401 先记疑似登录失效、由登录复核确认
// （见 auth_recheck.go）：账户还没被标记时不移出队列，推后到复核之后再同步。调用方持有 syncer.mu，返回前释放
func (syncer *catalogSyncer) authRequiredLocked(accountID string, err error, now time.Time) {
	syncer.stats.add(now, func(bucket *catalogStatBucket) { bucket.failed++ })
	if syncer.service.pool.UnschedulableSet([]string{accountID})[accountID] != unschedulableAuthRequired {
		task := syncer.taskLocked(accountID)
		syncer.scheduleLocked(task, task.priority, now.Add(syncer.busyRetryValue()))
		syncer.roundDoneLocked(accountID, false, false, err)
		syncer.mu.Unlock()
		return
	}
	syncer.removeLocked(accountID)
	syncer.dropSuspectLocked(accountID)
	syncer.roundDoneLocked(accountID, false, false, err)
	syncer.mu.Unlock()
	label := accountID
	if status, ok := syncer.service.pool.StatusOf(accountID); ok && status.Label != "" {
		label = status.Label
	}
	syncer.service.noteAuthRequired(accountID, label, authFromCatalog, err)
	syncer.service.publishModelAccess()
}

// rateLimitedLocked 处理上游 429：全局暂停 30s×2^k（±20%，上限 30 分钟），失败账户放回原优先级、不记个人失败。
// 已在暂停中（并发的另一个任务刚触发）时不再升级。调用方持有 syncer.mu，返回前释放
func (syncer *catalogSyncer) rateLimitedLocked(accountID string, err error, now time.Time) {
	syncer.stats.add(now, func(bucket *catalogStatBucket) { bucket.rateLimited++ })
	syncer.successStreak = 0
	task := syncer.taskLocked(accountID)
	if now.Before(syncer.pausedUntil) {
		syncer.scheduleLocked(task, task.priority, syncer.pausedUntil)
		syncer.mu.Unlock()
		return
	}
	pause := catalogPauseBase << min(syncer.pauseLevel, 16)
	pause = min(time.Duration(float64(pause)*syncer.jitter(0.2)), catalogPauseMax)
	syncer.pauseLevel++
	level := syncer.pauseLevel
	if syncer.pauseStarted.IsZero() {
		syncer.pauseStarted = now
	}
	syncer.pausedUntil = now.Add(pause)
	syncer.scheduleLocked(task, task.priority, syncer.pausedUntil)
	syncer.mu.Unlock()
	syncer.service.requests.logDurable("service", "WARN", fmt.Sprintf(
		"模型目录同步暂停 | 原因=%s | 暂停=%s | 连续=%d | 错误=%s",
		catalogPauseUpstream, pause.Round(time.Second), level, catalogErrorSummary(err),
	))
}

// emptyLocked 处理“空目录、保留原目录”：按其他失败退避，第 3 次空结果的时间不早于第一次空结果后 1 小时。
// 调用方持有 syncer.mu，返回前释放
func (syncer *catalogSyncer) emptyLocked(accountID string, err error, now time.Time) {
	syncer.stats.add(now, func(bucket *catalogStatBucket) { bucket.empty++ })
	var empty *aistudio.EmptyModelCatalogError
	errors.As(err, &empty)
	task := syncer.taskLocked(accountID)
	task.failures++
	due := now.Add(syncer.retryDelay(task.failures))
	if empty != nil && empty.Count >= 2 && !empty.Since.IsZero() {
		due = laterOf(due, empty.Since.Add(catalogEmptyAcceptSpan))
	}
	priority := catalogPriorityRetry
	if task.priority == catalogPriorityMissing {
		priority = catalogPriorityMissing
	}
	syncer.scheduleLocked(task, priority, due)
	syncer.roundDoneLocked(accountID, false, false, err)
	cache := syncer.cache
	failures := task.failures
	syncer.mu.Unlock()
	cache.noteFailure(accountID, failures, due)
	if empty != nil {
		cache.noteEmpty(accountID, empty.Count, empty.Since)
		syncer.service.requests.log(accountID, "WARN", fmt.Sprintf(
			"模型目录为空，保留原目录 | 模型=%d | 连续空结果=%d | 首次为空=%s 前",
			empty.Models, empty.Count, now.Sub(empty.Since).Round(time.Second),
		))
	}
}

// failedLocked 处理其他失败：按账户退避 1m×2^(f-1)（±20%，上限 6 小时）；同账户只在 f=1、2、4、8… 时写失败日志。
// 调用方持有 syncer.mu，返回前释放
func (syncer *catalogSyncer) failedLocked(accountID string, err error, now time.Time) {
	syncer.stats.add(now, func(bucket *catalogStatBucket) { bucket.failed++ })
	task := syncer.taskLocked(accountID)
	task.failures++
	delay := syncer.retryDelay(task.failures)
	priority := catalogPriorityRetry
	if task.priority == catalogPriorityMissing {
		priority = catalogPriorityMissing
	}
	syncer.scheduleLocked(task, priority, now.Add(delay))
	syncer.roundDoneLocked(accountID, false, false, err)
	cache := syncer.cache
	failures := task.failures
	syncer.mu.Unlock()
	cache.noteFailure(accountID, failures, now.Add(delay))
	if failures&(failures-1) == 0 {
		syncer.service.requests.log(accountID, "WARN", fmt.Sprintf(
			"模型目录同步失败 | 错误=%s | 下次=%s | 连续失败=%d", err.Error(), delay.Round(time.Second), failures,
		))
	}
}

// retryDelay 返回第 failures 次失败后的退避：1m×2^(f-1)，±20%，上限 6 小时
func (syncer *catalogSyncer) retryDelay(failures int) time.Duration {
	delay := catalogRetryBase << min(max(failures-1, 0), 16)
	return min(time.Duration(float64(delay)*syncer.jitter(0.2)), catalogRetryMax)
}

// catalogErrorSummary 返回错误的单行摘要（暂停日志用）
func catalogErrorSummary(err error) string {
	var rpcError *aistudio.RPCError
	if errors.As(err, &rpcError) {
		return fmt.Sprintf("AI Studio %s 返回 HTTP %d", rpcError.Method, rpcError.StatusCode)
	}
	text := err.Error()
	if runes := []rune(text); len(runes) > 160 {
		text = string(runes[:160]) + "…"
	}
	return text
}

// status 返回状态接口的目录同步状态（计数来自调度器，不在状态请求里遍历账户池）
func (syncer *catalogSyncer) status() *api.AdminCatalogSync {
	if syncer == nil {
		return nil
	}
	syncer.mu.Lock()
	defer syncer.mu.Unlock()
	now := syncer.clock()
	counts := syncer.countsLocked(now)
	hour := syncer.stats.hour(now)
	status := &api.AdminCatalogSync{
		Cached: syncer.cachedAccounts, FromDisk: syncer.fromDisk, Missing: counts.missing, Expired: counts.expired,
		AuthSuspects: len(syncer.suspects), Queued: counts.queued, Running: syncer.running,
		Synced1h: hour.synced, Failed1h: hour.failed + hour.empty, RateLimited1h: hour.rateLimited, SkippedBusy1h: hour.busy,
	}
	if now.Before(syncer.pausedUntil) {
		status.PausedUntil, status.PauseReason = syncer.pausedUntil, catalogPauseUpstream
	} else if !syncer.egressDeferredAt.IsZero() && now.Sub(syncer.egressDeferredAt) < catalogEgressPauseShown {
		status.PauseReason = catalogPauseEgress
	}
	return status
}

// deferAuthSuspects 把疑似登录失效的账户移到候选最后（稳定排序）：预热先用没有嫌疑的账户，疑似失效的由目录调度器逐个探测。
// 嫌疑来自目录缓存（记过需要登录、认证文件之后没变过），不改登录判定
func (manager *accountWorkerManager) deferAuthSuspects(accountIDs []string) []string {
	suspects := manager.authSuspects.Load()
	if suspects == nil || len(*suspects) == 0 || len(accountIDs) < 2 {
		return accountIDs
	}
	result := make([]string, 0, len(accountIDs))
	var deferred []string
	for _, accountID := range accountIDs {
		if _, suspect := (*suspects)[accountID]; suspect {
			deferred = append(deferred, accountID)
			continue
		}
		result = append(result, accountID)
	}
	return append(result, deferred...)
}
