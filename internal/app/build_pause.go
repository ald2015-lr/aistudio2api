package app

import (
	"fmt"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/api"
)

// Build 通道生成暂停：官网停用了经 Build 应用代理的生成。
//
// 线上 Build 代理调用开始返回 HTTP 400 “PROXY_GENERATION_DISABLED: streamGenerateContent is not supported”（需要订阅权益的模型
// 走单次代理调用，为 “… generateContent is not supported”），流式与非流式都关了。原先按请求本身的 400 直接返回客户端，不换通道；
// Playground 输入超限改走 Build 的请求与只能经 Build 调用的模型同样只拿到这个看不懂的 400。现在（识别见 aistudio/build_pause.go）：
//   - 当次请求：生成请求在 Build 上遇到它，不返回客户端，立即改走 Playground（同账号的 Playground 可用时在同账号重试，否则按现有规则换号），
//     不计入连续失败上限、不退避、不冷却任何额度；错误在首事件之前，流式请求同样无缝重试
//   - 按账号暂停：该账号不再经 Build 接生成请求（账户池标记，选号跳过它的 Build），buildPauseInterval（加少量抖动）后到期，
//     到期后它在 Build 上的下一次真实请求就是试探：成功即解除，再遇到就再暂停一段（不重复写日志）
//   - 全局暂停：buildPauseWindow 内有 buildPauseAccounts 个不同账号都遇到，所有账号都不再走 Build（选号里 Build 视为没有启用，
//     Playground 照常；死槽位与冷却状态的口径同样把 Build 当作不可用，见 aistudio.buildGenerationOpenLocked）；之后每隔
//     buildPauseInterval（加少量抖动）放行一个真实请求走 Build 试探（BuildProbe，只取已启动 Worker、现在就能用的 Build 账号，
//     不在请求路径上冷启动；取不到就按原条件选号，buildProbeRetry 后再放行；连续 buildProbeQueueAfter 次都取不到时号池多半持续饱和，
//     之后的试探改为排队等 Build 租约，最多 buildProbeQueueWait），试探失败时这次请求照常换到 Playground，用户无感知。
//     试探只给 Build 能生成其模型的请求，优先不限定通道的，到期超过 buildProbeRelax 仍没有时放宽到降级判定与流式优先 Playground
//     的请求（见 claimProbe 与 generateWithRetry）
//   - 试探成功（全局暂停中，或暂停过的账号在 Build 上成功）即解除全局与全部账号的暂停，写一行 INFO；进入按账号暂停、进入全局暂停
//     各写一行 WARN，同一状态不重复写
//   - 依赖 Build 的请求在暂停中给出清楚的错误（见 aistudio.BuildGenerationUnavailableError）：输入超过 Playground 上限、要改走 Build 的
//     请求按 400 返回（说明上限与原因），只能经 Build 调用的模型按 503 返回，不再把 PROXY_GENERATION_DISABLED 原文交给客户端
//   - 按账号的记录：账号移出账户池（删除、目录被移走）时删除（forget）；不在全局暂停时，暂停到期后又过了 buildPauseInterval 仍没有
//     Build 结果的记录（账号被停用、需要登录，或之后都没再经 Build）同样删除，状态与管理页不再一直显示等待试探；全部删光时请求路径恢复不加锁
//   - 进程内状态，不持久化：服务重启（含重建生成服务）后从头判定，最多再有几个请求在 Build 上遇到这个错误，都会无缝改走 Playground

const (
	// buildPauseInterval 为暂停到第一次（以及每次失败后到下一次）试探的间隔，buildPauseJitter 为在此之上随机增加的最长时间
	buildPauseInterval = 30 * time.Minute
	buildPauseJitter   = 3 * time.Minute
	// buildPauseWindow 内有 buildPauseAccounts 个不同账号遇到即判定为全局暂停
	buildPauseWindow   = 10 * time.Minute
	buildPauseAccounts = 3
	// buildProbeRetry 为试探没有取得 Build 租约、或试探请求以其他原因失败（结果说明不了 Build 是否恢复）时，到下一次试探的间隔
	buildProbeRetry = time.Minute
	// buildProbeStale 为认领的试探一直没有结论时，允许另一个请求重新试探的时间
	buildProbeStale = 10 * time.Minute
	// buildProbeRelax 为试探到期后只等优先请求（不限定通道的生成请求）的时间：超过后降级判定的 Playground 优先与流式优先 Playground
	// 的请求也拿来试探，以这类流量为主的服务同样能在官网恢复 Build 后解除暂停
	buildProbeRelax = 5 * time.Minute
	// buildProbeQueueAfter 为试探连续几次没有取得立即可用的 Build 账号后改为排队等 Build 租约：全局暂停时 Build 那一半额度不可用，
	// 号池更容易持续饱和（各账号的并发或每分钟请求已满、空出的槽位马上被排队的请求取走），只取立即可用的账号可能几个小时都试探不到；
	// buildProbeQueueWait 为排队的最长时间（主用模型的请求另不超过排队预算剩余的一半，见 probeQueueWait），等不到就照常选号
	buildProbeQueueAfter = 3
	buildProbeQueueWait  = 5 * time.Second
	// buildProbeQueueMin 为排队试探至少要有的等待时间：主用模型的排队预算剩余太少时这次仍只取立即可用的账号
	buildProbeQueueMin = 500 * time.Millisecond
)

// buildPauseTracker 为 Build 生成暂停的状态：按账号的暂停与全局暂停，以及全局暂停时的试探时间。
// 状态变化时同步写进账户池的暂停标记（SetAccountBuildPaused、SetBuildGenerationPaused、ClearBuildPauses），选号据此跳过 Build。
// 锁顺序：tracker.mu 再到账户池锁（账户池从不回调这里）
type buildPauseTracker struct {
	pool *aistudio.AccountPool
	log  func(source string, level string, message string)
	// now 与 jitter 只给测试替换
	now    func() time.Time
	jitter func(limit time.Duration) time.Duration
	// engaged 为有任何暂停记录（全局或按账号）：没有记录时请求路径不加锁
	engaged atomic.Bool

	mu sync.Mutex
	// accounts 为遇到过 PROXY_GENERATION_DISABLED、还没恢复的账号；hits 为各账号最近一次遇到的时间（全局判定用，超过窗口的删除）
	accounts map[string]*buildPausedAccount
	hits     map[string]time.Time
	// global 为全局暂停，since 为开始时间，nextProbe 为下次放行试探的时间；probing 为已认领、还没有结论的试探，probeAt 为认领时间；
	// probeMisses 为连续几次试探没有取得 Build 租约（达到 buildProbeQueueAfter 后试探改为排队，取得租约后清零）
	global      bool
	since       time.Time
	nextProbe   time.Time
	probing     bool
	probeAt     time.Time
	probeMisses int
	// nextExpiry 为按账号记录最早要处理的时间（没有时为零）：暂停中的为到期时间，到期等待试探的（不在全局暂停时）为删除时间，
	// 请求路径据此判断要不要解除到期的账号标记、删除没有结果的记录
	nextExpiry time.Time
}

// buildPausedAccount 为一个账号的 Build 生成暂停：paused 为账户池里的暂停标记还在，until 为到期（到期后清掉标记，
// 等它在 Build 上的下一次结果）；since 为第一次遇到的时间，label 为日志里的账号名
type buildPausedAccount struct {
	label  string
	since  time.Time
	until  time.Time
	paused bool
}

func newBuildPauseTracker(pool *aistudio.AccountPool, requests *requestRegistry) *buildPauseTracker {
	tracker := &buildPauseTracker{
		pool: pool, accounts: make(map[string]*buildPausedAccount), hits: make(map[string]time.Time),
		log: func(string, string, string) {},
	}
	if requests != nil {
		// 状态变化的日志很少出现、又必须留在运行日志里：走独立队列，控制台队列满时也不丢
		tracker.log = requests.logDurable
	}
	return tracker
}

// buildPauses 返回生成服务的 Build 暂停状态；测试直接构造的 trackedService 在第一次使用时建立
func (service *trackedService) buildPauses() *buildPauseTracker {
	service.buildPauseOnce.Do(func() {
		if service.buildPause == nil {
			service.buildPause = newBuildPauseTracker(service.pool, service.requests)
		}
	})
	return service.buildPause
}

func (tracker *buildPauseTracker) clock() time.Time {
	if tracker.now != nil {
		return tracker.now()
	}
	return time.Now()
}

// pauseFor 返回一次暂停（或到下一次试探）的时长：buildPauseInterval 加上不超过 buildPauseJitter 的随机时间，
// 多个账号、多个进程不在同一时刻一起试探
func (tracker *buildPauseTracker) pauseFor() time.Duration {
	if tracker.jitter != nil {
		return buildPauseInterval + tracker.jitter(buildPauseJitter)
	}
	return buildPauseInterval + rand.N(buildPauseJitter+1)
}

// disabled 记录账号在 Build 上遇到 PROXY_GENERATION_DISABLED（probe 为这次是全局暂停中的试探），返回此后是否处于全局暂停。
// 第一次遇到时暂停该账号并写一行 WARN；窗口内不同账号达到 buildPauseAccounts 个时进入全局暂停并写一行 WARN（不再另写账号那一行）；
// 已暂停或试探失败只顺延到期与下次试探，不再写日志
func (tracker *buildPauseTracker) disabled(accountID string, label string, probe bool) bool {
	if tracker == nil {
		return false
	}
	now := tracker.clock()
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	record := tracker.accounts[accountID]
	first := record == nil
	if first {
		record = &buildPausedAccount{label: label, since: now}
		tracker.accounts[accountID] = record
	}
	record.until = now.Add(tracker.pauseFor())
	// 每次都写回账户池的暂停标记（状态没变时 SetAccountBuildPaused 直接返回）：账号移出后又加入账户池时池里是新的账户、没有标记，
	// 只在记录不是暂停中时才写回的话，记录一直顺延不到期，它的每个请求都会先在 Build 上白失败一次
	record.paused = true
	tracker.pool.SetAccountBuildPaused(accountID, true)
	tracker.hits[accountID] = now
	for id, at := range tracker.hits {
		if now.Sub(at) > buildPauseWindow {
			delete(tracker.hits, id)
		}
	}
	if probe {
		tracker.probing, tracker.probeMisses = false, 0
	}
	tracker.engaged.Store(true)
	switch {
	case tracker.global:
		if probe {
			tracker.nextProbe = now.Add(tracker.pauseFor())
		}
	case len(tracker.hits) >= buildPauseAccounts:
		tracker.global, tracker.since = true, now
		tracker.nextProbe = now.Add(tracker.pauseFor())
		tracker.pool.SetBuildGenerationPaused(true)
		tracker.log("service", "WARN", fmt.Sprintf(
			"官网停用了 Build 通道的生成（PROXY_GENERATION_DISABLED），暂停 Build，生成改走 Playground | 下次试探=%s | 范围=全部账号 | "+
				"触发=%d 分钟内 %d 个账号",
			tracker.nextProbe.Format(time.RFC3339), int(buildPauseWindow/time.Minute), len(tracker.hits),
		))
	case first:
		tracker.log(label, "WARN", fmt.Sprintf(
			"官网停用了 Build 通道的生成（PROXY_GENERATION_DISABLED），暂停 Build，生成改走 Playground | 下次试探=%s | 范围=该账号",
			record.until.Format(time.RFC3339),
		))
	}
	tracker.refreshExpiryLocked()
	return tracker.global
}

// succeeded 记录账号在 Build 上成功收到首个事件（probe 为全局暂停中的试探）：全局暂停中，或这个账号暂停过，说明 Build 的生成已恢复，
// 解除全局与全部账号的暂停并写一行 INFO；没有暂停记录时只是正常的 Build 请求，不加锁直接返回
func (tracker *buildPauseTracker) succeeded(accountID string, label string, probe bool) {
	if tracker == nil || !tracker.engaged.Load() {
		return
	}
	now := tracker.clock()
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	if probe {
		tracker.probing, tracker.probeMisses = false, 0
	}
	record := tracker.accounts[accountID]
	if !tracker.global && record == nil {
		return
	}
	// 范围：全局暂停中，或还有别的账号暂停着（一起解除）时为全部账号，只有这一个账号暂停过时为该账号
	scope := "该账号"
	since := now
	if record != nil {
		since = record.since
	}
	if tracker.global {
		scope, since = "全部账号", tracker.since
	} else if len(tracker.accounts) > 1 {
		scope = "全部账号"
	}
	cleared := tracker.pool.ClearBuildPauses()
	tracker.log(label, "INFO", fmt.Sprintf(
		"Build 通道的生成已恢复，解除暂停 | 范围=%s | 试探账号=%s | 暂停时长=%s | 解除账号=%d",
		scope, label, now.Sub(since).Round(time.Second), cleared,
	))
	tracker.resetLocked()
}

// resetLocked 清空全部暂停记录；调用方持有 tracker.mu
func (tracker *buildPauseTracker) resetLocked() {
	clear(tracker.accounts)
	clear(tracker.hits)
	tracker.global, tracker.since, tracker.nextProbe = false, time.Time{}, time.Time{}
	tracker.probing, tracker.probeAt, tracker.probeMisses, tracker.nextExpiry = false, time.Time{}, 0, time.Time{}
	tracker.engaged.Store(false)
}

// forget 删除账号的暂停记录：账号移出账户池（删除、目录被移走）时调用，删除后又加入的同 ID 账号从头判定。最近遇到的时间（hits）保留，
// 仍算进全局判定：它说明的是官网停用了 Build 的生成，不因账号删除而失效（超过 buildPauseWindow 照常删除）。没有任何记录、
// 也没有全局暂停时请求路径恢复不加锁
func (tracker *buildPauseTracker) forget(accountID string) {
	if tracker == nil || !tracker.engaged.Load() {
		return
	}
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	if _, ok := tracker.accounts[accountID]; !ok {
		return
	}
	delete(tracker.accounts, accountID)
	tracker.refreshExpiryLocked()
	tracker.disengageIfIdleLocked()
}

// disengageIfIdleLocked 在没有任何按账号记录、也没有全局暂停时清除 engaged，请求路径恢复不加锁；调用方持有 tracker.mu
func (tracker *buildPauseTracker) disengageIfIdleLocked() {
	if !tracker.global && len(tracker.accounts) == 0 {
		tracker.probing, tracker.probeAt, tracker.probeMisses = false, time.Time{}, 0
		tracker.engaged.Store(false)
	}
}

// claimProbe 在每次选号前调用：先解除到期的按账号暂停；全局暂停中、到了试探时间且没有进行中的试探时，这次选号可以作为试探就认领
// 试探并返回 claimed 为真。preferred 为优先拿来试探的请求（不固定账号、不限定通道，见 generateWithRetry），allowed 为可以试探的请求
// （另含降级判定的 Playground 优先与流式优先 Playground）：到期后先只给 preferred，超过 buildProbeRelax 仍没有试探时 allowed 也可以。
// queue 为这次试探排队等 Build 租约（连续 buildProbeQueueAfter 次没有取得立即可用的 Build 账号之后），否则只取立即可用的。
// 认领后调用方必须以 disabled、succeeded 或 probeUnresolved 之一给出结论（一直没有结论时 buildProbeStale 后允许重新认领）
func (tracker *buildPauseTracker) claimProbe(preferred bool, allowed bool) (claimed bool, queue bool) {
	if tracker == nil || !tracker.engaged.Load() {
		return false, false
	}
	now := tracker.clock()
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	tracker.expireLocked(now)
	if !preferred && !allowed || !tracker.probeDueLocked(now) {
		return false, false
	}
	if !preferred && now.Sub(tracker.nextProbe) < buildProbeRelax {
		return false, false
	}
	tracker.probing, tracker.probeAt = true, now
	return true, tracker.probeMisses >= buildProbeQueueAfter
}

// probeDue 返回全局暂停中现在是否可以放行一次试探（不认领）
func (tracker *buildPauseTracker) probeDue() bool {
	if tracker == nil || !tracker.engaged.Load() {
		return false
	}
	now := tracker.clock()
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	return tracker.probeDueLocked(now)
}

func (tracker *buildPauseTracker) probeDueLocked(now time.Time) bool {
	if !tracker.global || now.Before(tracker.nextProbe) {
		return false
	}
	return !tracker.probing || now.Sub(tracker.probeAt) >= buildProbeStale
}

// probeUnresolved 结束一次没有结论的试探：missed 为没有取得 Build 租约（连续次数达到 buildProbeQueueAfter 后试探改为排队），
// 否则为试探请求以 PROXY_GENERATION_DISABLED 以外的原因失败（取得过租约，连续次数清零）。buildProbeRetry 后再放行下一次
func (tracker *buildPauseTracker) probeUnresolved(missed bool) {
	if tracker == nil {
		return
	}
	now := tracker.clock()
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	if !tracker.probing {
		return
	}
	tracker.probing = false
	if missed {
		tracker.probeMisses++
	} else {
		tracker.probeMisses = 0
	}
	if tracker.global {
		tracker.nextProbe = now.Add(buildProbeRetry)
	}
}

// probeQueueWait 返回排队试探最多等待的时间：buildProbeQueueWait；主用模型的请求不超过排队预算剩余的一半（其余留给随后照常的选号），
// 不到 buildProbeQueueMin 时为 0（这次仍只取立即可用的账号）
func probeQueueWait(budget *leaseWaitBudget) time.Duration {
	wait := buildProbeQueueWait
	if budget != nil {
		wait = min(wait, budget.remaining()/2)
	}
	if wait < buildProbeQueueMin {
		return 0
	}
	return wait
}

// expireLocked 解除到期的按账号暂停标记（记录保留，等这个账号在 Build 上的下一次结果），已不在账户池里的账号的记录直接删除；
// 不在全局暂停时，到期后又过了 buildPauseInterval 仍没有 Build 结果的记录（账号被停用、需要登录，或之后的请求都没再经 Build）也删除，
// 状态与管理页不再一直显示等待试探，全部删光时请求路径恢复不加锁。全局暂停中到期的记录保留（状态里算作遇到过的账号），由试探成功一起清掉。
// 调用方持有 tracker.mu
func (tracker *buildPauseTracker) expireLocked(now time.Time) {
	if tracker.nextExpiry.IsZero() || now.Before(tracker.nextExpiry) {
		return
	}
	for accountID, record := range tracker.accounts {
		switch {
		case record.paused && !now.Before(record.until):
			record.paused = false
			// 状态没变（标记已不在）时确认账号还在账户池里，不在就删除记录
			if !tracker.pool.SetAccountBuildPaused(accountID, false) {
				if _, err := tracker.pool.Account(accountID); err != nil {
					delete(tracker.accounts, accountID)
				}
			}
		case !record.paused && !tracker.global && !now.Before(record.until.Add(buildPauseInterval)):
			delete(tracker.accounts, accountID)
		}
	}
	tracker.refreshExpiryLocked()
	tracker.disengageIfIdleLocked()
}

// refreshExpiryLocked 重算按账号记录最早要处理的时间：暂停中的取到期时间，到期等待试探的（不在全局暂停时）取删除时间；
// 调用方持有 tracker.mu
func (tracker *buildPauseTracker) refreshExpiryLocked() {
	tracker.nextExpiry = time.Time{}
	for _, record := range tracker.accounts {
		due := record.until
		if !record.paused {
			if tracker.global {
				continue
			}
			due = record.until.Add(buildPauseInterval)
		}
		if tracker.nextExpiry.IsZero() || due.Before(tracker.nextExpiry) {
			tracker.nextExpiry = due
		}
	}
}

// status 返回状态接口里的 Build 通道状态：不在全局暂停时 PausedAccounts 为按账号暂停中的账号数，AwaitingProbe 为按账号暂停已到期、
// 等它在 Build 上的下一次结果（试探）的账号数；全局暂停时所有账号都不走 Build，各账号也不会单独试探，PausedAccounts 为遇到过
// PROXY_GENERATION_DISABLED 的全部账号数（含各自暂停已到期的），AwaitingProbe 为 0
func (tracker *buildPauseTracker) status() *api.AdminBuildChannel {
	if tracker == nil {
		return &api.AdminBuildChannel{}
	}
	now := tracker.clock()
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	tracker.expireLocked(now)
	status := &api.AdminBuildChannel{Paused: tracker.global, Probing: tracker.probing && tracker.global}
	if tracker.global {
		status.Since, status.NextProbe = tracker.since, tracker.nextProbe
	}
	for _, record := range tracker.accounts {
		if record.paused || tracker.global {
			status.PausedAccounts++
		} else {
			status.AwaitingProbe++
		}
	}
	return status
}

// buildInputLimitUnavailable 返回输入超过 Playground 上限、而官网停用了 Build 通道的生成时给客户端的错误（对外 400，写明上限与原因）；
// capErr 为 Playground 返回的账号输入上限错误，上限从它的文案里取
func buildInputLimitUnavailable(modelID string, capErr error, cause error) error {
	return &aistudio.BuildGenerationUnavailableError{
		Reason: aistudio.BuildUnavailableInputLimit, Model: modelID,
		InputLimit: aistudio.InputTokenLimitValue(capErr), Cause: joinBuildCause(capErr, cause),
	}
}

// buildUnavailableAtEnd 返回换号结束、最后的错误仍是 Build 的 PROXY_GENERATION_DISABLED 时给客户端的错误：
// 只能走 Build 的请求（输入超过 Playground 上限，capErr 为改走 Build 时 Playground 的上限错误）为输入超限（400），
// Playground 目录没有的模型为只能经 Build 调用（503），其余为 Playground 接不了（503）
func (service *trackedService) buildUnavailableAtEnd(modelID string, buildOnly bool, capErr error, cause error) error {
	if buildOnly && capErr != nil {
		return buildInputLimitUnavailable(modelID, capErr, cause)
	}
	reason := aistudio.BuildUnavailableNoPlayground
	if service.pool.PlaygroundEnabled() && !service.pool.PlaygroundListsModel(modelID) {
		reason = aistudio.BuildUnavailableModel
	}
	return &aistudio.BuildGenerationUnavailableError{Reason: reason, Model: modelID, Cause: cause}
}

// joinBuildCause 合并内部原因，跳过空值
func joinBuildCause(errs ...error) error {
	var kept []error
	for _, err := range errs {
		if err != nil {
			kept = append(kept, err)
		}
	}
	if len(kept) == 0 {
		return nil
	}
	joined := kept[0]
	for _, err := range kept[1:] {
		joined = fmt.Errorf("%w; %w", joined, err)
	}
	return joined
}
