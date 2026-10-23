package app

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/api"
)

// 登录复核（第 5 项：需要登录从 334 涨到 2041，用户“验证”成功仍显示需要登录）。排查结论（auth_surge_result）：
//   - 一次上游 401 就判死：DefinitiveAuthenticationFailure 只看 401 / Code 16，Linux 上续签必然失败（chromeauth 只支持 Windows），
//     被标记的账户拿不到租约、永远等不到能恢复它的成功，状态只在内存（重启才“好”）；
//   - 401 多半来自 Go 通道用的磁盘 Cookie（Worker 启动后不写回）或 Google 一侧短暂作废会话，代码无法区分，只能不误伤、可恢复、可观测。
//
// 这里做两件事，共用同一个低速复核器（并发 authRecheckConcurrency、两次开始至少间隔 authRecheckInterval，出口限流判定成立期间暂停）：
//  1. 疑似登录失效：租约路径上的上游 401 先经账户池的 gate（aistudio.AuthFailureGate，见 gate）只写 authSuspectCooldown 的短冷却并排队复核：
//     用 WithoutAuthRecovery 的 GetAiStudioBenefitTier（Go HTTP、磁盘 Cookie）再问一次，仍是 401 才标需要登录（释放驻留 Worker、
//     写“账号需要登录”），通过则清掉疑似冷却。登录页跳转、签名 Cookie 缺失等本地证据不经过这里，照常直接标记；
//  2. 定期复核：需要登录的账户每 authRecheckRound 一轮、每轮最多 authRecheckRoundLimit 个，按账户指数退避（15 分钟起到 6 小时），
//     复核通过即恢复就绪并放回目录同步队列。只复核按接口证据（上游 401）标记的账户：管理验证的浏览器判定未登录、Worker 启动跳到登录页
//     是浏览器证据，接口用同一份磁盘 Cookie 通过不说明浏览器能登录，改回就绪只会被预热选中再冷启动一次、又被标回（启停风暴），
//     这些账户等重新登录、认证文件更新或管理验证通过。每轮结束写一条持久的汇总“登录复核 | 恢复=… | 仍失效=… | 跳过=… | 下一轮=…”：
//     定期队列做完时写一条；期间疑似账户的结果累计到下一次轮次边界，不按每个疑似账户写（持久队列只放很少出现或已限频的行）。
// 复核只发这一个请求（与目录同步的第一步相同），不续签、不重放，不改变面向 Google 的请求形态

const (
	// authSuspectCooldown 为疑似登录失效的账户的全局冷却：复核通常几秒内完成；出口限流期间复核暂停，冷却到期前再次 401 会续上
	authSuspectCooldown = 5 * time.Minute
	// authSuspectReasonPrefix 为疑似登录失效冷却的原因前缀（复核通过时按它清除，管理页冷却列表显示）
	authSuspectReasonPrefix = "疑似登录失效，等待复核："
	// authRecheckConcurrency 与 authRecheckInterval 为复核速率：最多 2 个同时进行，两次开始至少间隔 1 秒（≤1 账户/秒）
	authRecheckConcurrency = 2
	authRecheckInterval    = time.Second
	// authRecheckRound 与 authRecheckRoundLimit 为需要登录账户的定期复核：每 15 分钟一轮，每轮最多 50 个（按到期先后）
	authRecheckRound      = 15 * time.Minute
	authRecheckRoundLimit = 50
	// authRecheckBackoffBase 与 authRecheckBackoffMax 为需要登录账户复核仍失效后的按账户退避：15 分钟×2^(n-1)，上限 6 小时
	authRecheckBackoffBase = 15 * time.Minute
	authRecheckBackoffMax  = 6 * time.Hour
	// authRecheckTimeout 为一次复核请求的上限
	authRecheckTimeout = 30 * time.Second
	// authSuspectRetry 为疑似账户复核出错（上游 429、连接失败）或账户忙、出口限流时推后的时间；authSuspectMaxAttempts 次复核都没有结论时
	// 不再复核、不标记，疑似冷却自行到期
	authSuspectRetry       = 30 * time.Second
	authSuspectMaxAttempts = 4
	// authRecheckPauseShown 为出口限流推后复核后，状态里“暂停原因=出口限流”保留的时间
	authRecheckPauseShown = time.Minute
	// authRecentWindow 为状态里“最近新增需要登录”的窗口
	authRecentWindow = 10 * time.Minute
)

// authRecheckPauseEgress 为复核暂停的原因（状态接口）
const authRecheckPauseEgress = "出口限流"

// loginProber 为能用磁盘 Cookie 复核登录的服务（*aistudio.PooledService 实现）；没有实现时复核器不工作，401 照常直接标记
type loginProber interface {
	ProbeLogin(ctx context.Context, lease *aistudio.AccountLease) error
}

// authSuspect 为一个疑似登录失效、等待复核的账户
type authSuspect struct {
	accountID string
	source    authRequiredSource
	cause     error
	since     time.Time
	// due 为最早可以复核的时间，cooledUntil 为最近一次写入的疑似冷却到期时间，attempts 为没有结论的复核次数
	due         time.Time
	cooledUntil time.Time
	attempts    int
	// generation 为疑似时账户的认证代际：复核确认时期间保存过新登录则不标记
	generation uint64
	running    bool
}

// authRecheckRecord 为需要登录账户的定期复核退避：failures 为复核仍失效的次数，recoveries 为复核恢复后又被标记的次数（一并计入退避，
// 免得浏览器打不开、接口却通过的账户每 15 分钟恢复一次又被标回），nextAt 为最早的下次复核。browserEvidence 为最近一次标需要登录
// 的证据来自浏览器（管理验证判定未登录、Worker 启动跳到登录页）：定期复核不复核它（见本文件开头）
type authRecheckRecord struct {
	failures, recoveries int
	nextAt               time.Time
	browserEvidence      bool
}

// authRecheckBucket 为一分钟内的复核计数
type authRecheckBucket struct {
	minute                                               int64
	cleared, confirmed, recovered, stillInvalid, skipped int
}

// authRecheckRoundStats 为一轮（及期间疑似账户）的复核结果，轮次结束时写汇总；browser 为本轮开始时因浏览器证据不复核的需要登录账户数
type authRecheckRoundStats struct {
	recovered, stillInvalid, skipped, cleared, confirmed int
	browser                                              int
}

func (stats authRecheckRoundStats) empty() bool {
	return stats.recovered+stats.stillInvalid+stats.skipped+stats.cleared+stats.confirmed == 0
}

// authRecheck 为登录复核器；方法全部 nil 安全（测试直接构造的服务没有复核器，401 照常直接标记）
type authRecheck struct {
	service *trackedService
	mu      sync.Mutex
	// suspects 为疑似登录失效的账户，records 为需要登录账户的定期复核退避，queue 为本轮待复核的需要登录账户
	suspects map[string]*authSuspect
	records  map[string]*authRecheckRecord
	queue    []string
	running  int
	// lastStart 为最近一次开始复核的时间，nextRound 为下一轮定期复核的时间；periodicOpen 为本轮定期复核入过队、还没写汇总
	// （只在 startRoundLocked 置位：疑似账户的结果只累计进 round，到轮次边界才写）
	lastStart    time.Time
	nextRound    time.Time
	periodicOpen bool
	round        authRecheckRoundStats
	buckets      [60]authRecheckBucket
	// egressPausedAt 为最近一次因出口限流推后复核的时间（状态里的暂停原因）
	egressPausedAt time.Time
	wake           chan struct{}
	// active 为在跑的复核循环数（服务运行中为 1；停止后重启时上一代循环可能还在收尾）：为 0 时 gate 放行，401 照常直接标记
	active atomic.Int32
	// 以下只给包内测试替换（零值取默认）
	now                                          func() time.Time
	interval, round0, suspectCooldown, suspectRe time.Duration
	roundLimit                                   int
	egressPaused                                 func(key string) bool
}

// newAuthRecheck 创建复核器；服务没有实现 loginProber 时返回 nil
func newAuthRecheck(service *trackedService) *authRecheck {
	if service == nil || service.pool == nil {
		return nil
	}
	if _, ok := service.service.(loginProber); !ok {
		return nil
	}
	return &authRecheck{
		service: service, suspects: make(map[string]*authSuspect), records: make(map[string]*authRecheckRecord),
		wake: make(chan struct{}, 1),
	}
}

func (recheck *authRecheck) clock() time.Time {
	if recheck.now != nil {
		return recheck.now()
	}
	return time.Now()
}

func (recheck *authRecheck) intervalValue() time.Duration {
	if recheck.interval > 0 {
		return recheck.interval
	}
	return authRecheckInterval
}

func (recheck *authRecheck) roundValue() time.Duration {
	if recheck.round0 > 0 {
		return recheck.round0
	}
	return authRecheckRound
}

func (recheck *authRecheck) roundLimitValue() int {
	if recheck.roundLimit > 0 {
		return recheck.roundLimit
	}
	return authRecheckRoundLimit
}

func (recheck *authRecheck) suspectCooldownValue() time.Duration {
	if recheck.suspectCooldown > 0 {
		return recheck.suspectCooldown
	}
	return authSuspectCooldown
}

func (recheck *authRecheck) suspectRetryValue() time.Duration {
	if recheck.suspectRe > 0 {
		return recheck.suspectRe
	}
	return authSuspectRetry
}

// kick 唤醒复核循环
func (recheck *authRecheck) kick() {
	if recheck == nil {
		return
	}
	select {
	case recheck.wake <- struct{}{}:
	default:
	}
}

// authRecheckBackoff 返回第 n 次复核仍失效（含恢复后又被标记）之后的等待：15 分钟×2^(n-1)，上限 6 小时
func authRecheckBackoff(n int) time.Duration {
	if n < 1 {
		n = 1
	}
	return min(authRecheckBackoffBase<<min(n-1, 16), authRecheckBackoffMax)
}

// authSourceOf 按上游 RPC 方法推断疑似登录失效的来源（gate 拿不到请求的 ctx）：生成、目录同步，其余为其他请求
func authSourceOf(cause error) authRequiredSource {
	var rpcError *aistudio.RPCError
	if !errors.As(cause, &rpcError) {
		return authFromOther
	}
	switch strings.ToLower(strings.TrimSpace(rpcError.Method)) {
	case "generatecontent", "streamgeneratecontent", "createinteractionstream":
		return authFromGeneration
	case "listmodels", "getaistudiobenefittier":
		return authFromCatalog
	}
	return authFromOther
}

// gate 为挂在账户池上的 aistudio.AuthFailureGate：复核循环在跑时，上游 401 只记疑似登录失效（写短冷却、排队复核），返回 false 不改账户状态；
// 已在复核中的账户再次 401 时只在疑似冷却到期后续上冷却。复核循环没有在跑（服务停止、启动早期）时返回 true，照常直接标记
func (recheck *authRecheck) gate(lease *aistudio.AccountLease, cause error) bool {
	if recheck == nil || recheck.active.Load() <= 0 || lease == nil || lease.Account() == nil {
		return true
	}
	accountID := lease.Account().ID
	now := recheck.clock()
	cooldown := recheck.suspectCooldownValue()
	recheck.mu.Lock()
	suspect := recheck.suspects[accountID]
	fresh := suspect == nil
	if fresh {
		suspect = &authSuspect{
			accountID: accountID, source: authSourceOf(cause), cause: cause, since: now, due: now,
			generation: recheck.service.pool.AuthGeneration(accountID),
		}
		recheck.suspects[accountID] = suspect
	}
	rewrite := fresh || !now.Before(suspect.cooledUntil)
	if rewrite {
		suspect.cooledUntil = now.Add(cooldown)
	}
	recheck.mu.Unlock()
	if rewrite {
		reason := authSuspectReasonPrefix + authRequiredCauseSummary(cause)
		write, _, err := recheck.service.pool.TryExtendCooldownIfGeneration(
			accountID, "", lease.ModelAccessGeneration(), lease.CheckedAt(), now.Add(cooldown), reason,
		)
		if err != nil {
			recheck.service.requests.log(lease.Account().Config.Label, "WARN", "疑似登录失效冷却写入失败 | 错误="+err.Error())
		}
		if write != aistudio.CooldownWritten && write != aistudio.CooldownKeptLater {
			// 没有写上（目录代际已变、已有更晚的结果）：下一次 401 再写，不按已冷却处理
			recheck.mu.Lock()
			if recheck.suspects[accountID] == suspect {
				suspect.cooledUntil = time.Time{}
			}
			recheck.mu.Unlock()
		}
	}
	if fresh {
		key, _ := recheck.service.pool.EgressKeyOf(accountID)
		recheck.service.requests.log(accountLabelOf(lease), "WARN", fmt.Sprintf(
			"账号疑似登录失效 | 来源=%s | 出口=%s | 冷却=%s | 复核=排队 | 错误=%s",
			suspect.source, aistudio.EgressLabel(key), cooldown, authRequiredCauseSummary(cause),
		))
		recheck.kick()
	}
	return false
}

// noteLoginRestored 在人工恢复了账户的登录之后调用（浏览器登录、认证文件更新、管理验证通过），三条路径同一处理：
// 目录缓存里的需要登录记录与疑似失效（catalogSyncer.noteAuthValid）、登录复核的疑似记录与退避（authRecheck.forget），
// 以及疑似登录失效的全局冷却（authSuspectReasonPrefix）。原先只有验证通过清了前两项的一部分：随后的目录同步失败（如上游 429）、
// 或出口限流期间复核暂停时，刚验证有效的账号最多还要等 authSuspectCooldown 才能调度；浏览器登录与认证文件更新则一直带着嫌疑
func (service *trackedService) noteLoginRestored(accountID string) {
	if service == nil {
		return
	}
	service.catalogSync.noteAuthValid(accountID)
	service.authRecheck.forget(accountID)
	if service.pool == nil {
		return
	}
	cleared, err := service.pool.ClearCooldownsMatching(accountID, func(_ string, cooldown aistudio.CooldownState) bool {
		return strings.HasPrefix(cooldown.Reason, authSuspectReasonPrefix)
	})
	if err != nil {
		if service.requests != nil {
			service.requests.log(accountID, "WARN", "疑似登录失效冷却清除失败 | 错误="+err.Error())
		}
		return
	}
	if cleared > 0 {
		service.publishModelAccess()
		service.workers.notifySchedulingIfAny()
	}
}

// accountLabelOf 返回租约账户的显示名（没有时为账户 ID）
func accountLabelOf(lease *aistudio.AccountLease) string {
	account := lease.Account()
	if label := strings.TrimSpace(account.Config.Label); label != "" {
		return label
	}
	return account.ID
}

// suspected 返回账户是否疑似登录失效、等待复核（目录同步据此推后，不在复核前重复打 401）
func (recheck *authRecheck) suspected(accountID string) bool {
	if recheck == nil {
		return false
	}
	recheck.mu.Lock()
	defer recheck.mu.Unlock()
	_, ok := recheck.suspects[accountID]
	return ok
}

// noteSuccess 在账户成功取得上游首事件后调用：清掉它的定期复核退避（真正能用的账户以后再被标记时从 15 分钟起复核）
func (recheck *authRecheck) noteSuccess(accountID string) {
	if recheck == nil {
		return
	}
	recheck.mu.Lock()
	if _, ok := recheck.records[accountID]; ok {
		delete(recheck.records, accountID)
	}
	recheck.mu.Unlock()
}

// forget 移除账户的复核记录（账户删除后，或人工恢复了登录：浏览器登录、认证文件更新、管理验证通过，见 noteLoginRestored）。
// 正在复核的疑似账户由 finishSuspect 按“已移除”处理，不再据旧结论标记或清冷却
func (recheck *authRecheck) forget(accountID string) {
	if recheck == nil {
		return
	}
	recheck.mu.Lock()
	delete(recheck.suspects, accountID)
	delete(recheck.records, accountID)
	recheck.mu.Unlock()
}

// noteMarked 记下账户这次被标需要登录的证据来源（noteAuthRequired 与管理验证调用）：browser 为浏览器证据，定期复核不复核它；
// 接口证据（上游 401）照常按退避复核。只记最近一次：之后按接口证据再被标记时恢复复核
func (recheck *authRecheck) noteMarked(accountID string, browser bool) {
	if recheck == nil {
		return
	}
	recheck.mu.Lock()
	if record := recheck.records[accountID]; record != nil || browser {
		recheck.recordLocked(accountID).browserEvidence = browser
	}
	recheck.mu.Unlock()
}

// run 为复核循环（Start 里与目录调度器一起启动，dataContext 结束时退出、等进行中的复核结束）
func (recheck *authRecheck) run(ctx context.Context) {
	if recheck == nil {
		return
	}
	recheck.active.Add(1)
	var tasks sync.WaitGroup
	defer func() {
		recheck.active.Add(-1)
		tasks.Wait()
	}()
	recheck.mu.Lock()
	if recheck.nextRound.IsZero() {
		// 启动后先让预热与目录同步跑一会儿，第一轮定期复核在一分钟后
		recheck.nextRound = recheck.clock().Add(min(time.Minute, recheck.roundValue()))
	}
	recheck.mu.Unlock()
	for {
		wait := recheck.dispatch(ctx, &tasks)
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-recheck.wake:
		case <-timer.C:
		}
		timer.Stop()
	}
}

// authRecheckTask 为一次要开始的复核：suspect 非空为疑似账户，否则为需要登录账户的定期复核
type authRecheckTask struct {
	accountID string
	suspect   *authSuspect
}

// dispatch 尽量开始复核，返回到下次需要检查的时间（不超过 1 秒）
func (recheck *authRecheck) dispatch(ctx context.Context, tasks *sync.WaitGroup) time.Duration {
	const tick = time.Second
	for ctx.Err() == nil {
		recheck.mu.Lock()
		now := recheck.clock()
		if !now.Before(recheck.nextRound) {
			recheck.startRoundLocked(now)
		}
		if recheck.running >= authRecheckConcurrency {
			recheck.mu.Unlock()
			return tick
		}
		if !recheck.lastStart.IsZero() {
			if next := recheck.lastStart.Add(recheck.intervalValue()); now.Before(next) {
				recheck.mu.Unlock()
				return min(next.Sub(now), tick)
			}
		}
		task, ok := recheck.nextLocked(now)
		if !ok {
			recheck.finishRoundIfDoneLocked(now)
			recheck.mu.Unlock()
			return tick
		}
		recheck.mu.Unlock()
		// 锁外判断出口：判定成立期间该出口上的账户不复核（疑似账户推后、仍保持短冷却，定期复核记为跳过）
		key, _ := recheck.service.pool.EgressKeyOf(task.accountID)
		if recheck.egressPausedFor(key) {
			recheck.mu.Lock()
			recheck.egressPausedAt = now
			if task.suspect != nil {
				task.suspect.running = false
				task.suspect.due = now.Add(recheck.suspectRetryValue())
			} else {
				recheck.skipLocked(task.accountID, now)
			}
			recheck.mu.Unlock()
			continue
		}
		recheck.mu.Lock()
		recheck.running++
		recheck.lastStart = now
		recheck.mu.Unlock()
		tasks.Add(1)
		go func() {
			defer tasks.Done()
			recheck.execute(ctx, task)
			recheck.kick()
		}()
	}
	return tick
}

// egressPausedFor 返回出口上的复核是否暂停（判定成立期间）
func (recheck *authRecheck) egressPausedFor(key string) bool {
	if recheck.egressPaused != nil {
		return recheck.egressPaused(key)
	}
	return recheck.service.egress.catalogPaused(key)
}

// startRoundLocked 开始一轮定期复核：取启用、需要登录、退避已到期的账户（没有记录的最先），最多 roundLimit 个。调用方持有 mu
func (recheck *authRecheck) startRoundLocked(now time.Time) {
	recheck.nextRound = now.Add(recheck.roundValue())
	// 上一轮还没做完（账户忙、出口限流）或期间只有疑似账户的结果：先写它的汇总（每个轮次边界最多一条），剩下的并入本轮
	recheck.writeRoundLocked(now)
	required := recheck.service.pool.AuthRequiredAccountIDs()
	type due struct {
		accountID string
		at        time.Time
	}
	candidates := make([]due, 0, len(required))
	queued := make(map[string]struct{}, len(recheck.queue))
	for _, accountID := range recheck.queue {
		queued[accountID] = struct{}{}
	}
	for _, accountID := range required {
		if _, ok := queued[accountID]; ok {
			continue
		}
		if _, suspect := recheck.suspects[accountID]; suspect {
			continue
		}
		var at time.Time
		if record := recheck.records[accountID]; record != nil {
			if record.browserEvidence {
				recheck.round.browser++
				continue
			}
			at = record.nextAt
		}
		if at.After(now) {
			continue
		}
		candidates = append(candidates, due{accountID: accountID, at: at})
	}
	slices.SortStableFunc(candidates, func(left, right due) int { return left.at.Compare(right.at) })
	limit := recheck.roundLimitValue() - len(recheck.queue)
	for index := 0; index < len(candidates) && index < limit; index++ {
		recheck.queue = append(recheck.queue, candidates[index].accountID)
	}
	recheck.periodicOpen = len(recheck.queue) > 0
}

// nextLocked 取下一个要复核的账户：已到时间的疑似账户先（按疑似先后），其次本轮定期复核的队列。调用方持有 mu
func (recheck *authRecheck) nextLocked(now time.Time) (authRecheckTask, bool) {
	var picked *authSuspect
	for _, suspect := range recheck.suspects {
		if suspect.running || suspect.due.After(now) {
			continue
		}
		if picked == nil || suspect.since.Before(picked.since) ||
			suspect.since.Equal(picked.since) && suspect.accountID < picked.accountID {
			picked = suspect
		}
	}
	if picked != nil {
		picked.running = true
		return authRecheckTask{accountID: picked.accountID, suspect: picked}, true
	}
	if len(recheck.queue) > 0 {
		accountID := recheck.queue[0]
		recheck.queue = recheck.queue[1:]
		return authRecheckTask{accountID: accountID}, true
	}
	return authRecheckTask{}, false
}

// finishRoundIfDoneLocked 在本轮定期复核入过队、队列做完、没有进行中的复核时写汇总；只有疑似账户的结果时不写（到轮次边界再写）。
// 调用方持有 mu
func (recheck *authRecheck) finishRoundIfDoneLocked(now time.Time) {
	if !recheck.periodicOpen || len(recheck.queue) > 0 || recheck.running > 0 {
		return
	}
	recheck.writeRoundLocked(now)
}

// writeRoundLocked 写一轮的汇总（持久行，诊断脚本按“登录复核 |”统计），没有任何结果时不写（只因浏览器证据不复核的账户不算结果）。
// 调用方持有 mu
func (recheck *authRecheck) writeRoundLocked(now time.Time) {
	recheck.periodicOpen = false
	stats := recheck.round
	recheck.round = authRecheckRoundStats{}
	if stats.empty() {
		return
	}
	next := max(recheck.nextRound.Sub(now), 0).Round(time.Second)
	recheck.service.requests.logDurable("service", "INFO", fmt.Sprintf(
		"登录复核 | 恢复=%d | 仍失效=%d | 跳过=%d | 下一轮=%s | 疑似排除=%d | 疑似确认=%d | 待复核疑似=%d | 浏览器判定失效不复核=%d",
		stats.recovered, stats.stillInvalid, stats.skipped, next, stats.cleared, stats.confirmed, len(recheck.suspects), stats.browser,
	))
}

// addLocked 记一分钟内的计数并累计到本轮汇总。调用方持有 mu
func (recheck *authRecheck) addLocked(now time.Time, change func(*authRecheckBucket), roundChange func(*authRecheckRoundStats)) {
	minute := now.Unix() / 60
	bucket := &recheck.buckets[minute%int64(len(recheck.buckets))]
	if bucket.minute != minute {
		*bucket = authRecheckBucket{minute: minute}
	}
	change(bucket)
	roundChange(&recheck.round)
}

// skipLocked 记定期复核跳过一个账户（账户忙、出口限流、复核出错）：保持退避，下一轮再取。调用方持有 mu
func (recheck *authRecheck) skipLocked(accountID string, now time.Time) {
	recheck.addLocked(now, func(bucket *authRecheckBucket) { bucket.skipped++ },
		func(stats *authRecheckRoundStats) { stats.skipped++ })
}

// execute 执行一次复核：取空闲独占租约（账户有进行中的请求时推后），用磁盘 Cookie 发一次 GetAiStudioBenefitTier，按结果处理
func (recheck *authRecheck) execute(ctx context.Context, task authRecheckTask) {
	prober, _ := recheck.service.service.(loginProber)
	startedAt := recheck.clock()
	generation := recheck.service.pool.AuthGeneration(task.accountID)
	lease, acquired, err := recheck.service.pool.TryAcquireIdleAccount(ctx, task.accountID)
	if err == nil && !acquired {
		err = errAuthRecheckBusy
	}
	var probeErr error
	valid := false
	if err == nil {
		probeCtx, cancel := context.WithTimeout(ctx, authRecheckTimeout)
		probeErr = prober.ProbeLogin(probeCtx, lease)
		cancel()
		if probeErr == nil && task.suspect == nil {
			// 需要登录的账户复核通过：在租约内写回有效（与请求成功确认登录有效相同的顺序规则），恢复就绪
			valid = true
			probeErr = lease.MarkAuthenticationValid()
		}
		if releaseErr := lease.Release(); releaseErr != nil && probeErr == nil {
			probeErr = releaseErr
			valid = false
		}
	} else {
		probeErr = err
	}
	if ctx.Err() != nil {
		// 停止服务：放回原处，不记结果
		recheck.mu.Lock()
		recheck.running--
		if task.suspect != nil {
			task.suspect.running = false
		} else {
			recheck.queue = append(recheck.queue, task.accountID)
		}
		recheck.mu.Unlock()
		return
	}
	if task.suspect != nil {
		recheck.finishSuspect(task.suspect, generation, startedAt, probeErr)
		return
	}
	recheck.finishPeriodic(task.accountID, startedAt, valid, probeErr)
}

// errAuthRecheckBusy 表示账户有进行中的请求或独占操作，这次不复核
var errAuthRecheckBusy = errors.New("账户忙，推后复核")

// finishSuspect 处理疑似账户的复核结果：通过则清掉疑似冷却；仍是 401 则标需要登录（经认证代际核对）、释放驻留 Worker、
// 写“账号需要登录”；出错或账户忙时推后，几次都没有结论就不再复核、不标记
func (recheck *authRecheck) finishSuspect(suspect *authSuspect, generation uint64, startedAt time.Time, probeErr error) {
	service := recheck.service
	accountID := suspect.accountID
	label := accountID
	if status, ok := service.pool.StatusOf(accountID); ok && status.Label != "" {
		label = status.Label
	}
	now := recheck.clock()
	recheck.mu.Lock()
	if recheck.suspects[accountID] != suspect {
		// 复核期间账户已被移除（删除，或人工恢复了登录，见 forget）：不再按这次的结论标记或清冷却
		recheck.running--
		recheck.mu.Unlock()
		return
	}
	recheck.mu.Unlock()
	switch {
	case probeErr == nil:
		recheck.mu.Lock()
		recheck.running--
		delete(recheck.suspects, accountID)
		recheck.addLocked(now, func(bucket *authRecheckBucket) { bucket.cleared++ },
			func(stats *authRecheckRoundStats) { stats.cleared++ })
		recheck.mu.Unlock()
		if _, err := service.pool.ClearCooldownsMatching(accountID, func(_ string, cooldown aistudio.CooldownState) bool {
			return strings.HasPrefix(cooldown.Reason, authSuspectReasonPrefix)
		}); err != nil {
			service.requests.log(label, "WARN", "疑似登录失效冷却清除失败 | 错误="+err.Error())
		}
		service.requests.log(label, "INFO", fmt.Sprintf(
			"疑似登录失效复核通过 | 来源=%s | 疑似=%s | 接口认证正常，已解除冷却",
			suspect.source, now.Sub(suspect.since).Round(time.Second),
		))
		service.workers.notifySchedulingIfAny()
	case aistudio.DefinitiveAuthenticationFailure(probeErr):
		recheck.mu.Lock()
		recheck.running--
		delete(recheck.suspects, accountID)
		record := recheck.recordLocked(accountID)
		record.failures++
		record.nextAt = now.Add(authRecheckBackoff(record.failures + record.recoveries))
		recheck.addLocked(now, func(bucket *authRecheckBucket) { bucket.confirmed++ },
			func(stats *authRecheckRoundStats) { stats.confirmed++ })
		recheck.mu.Unlock()
		// 用疑似时的认证代际：期间保存过新登录或续签时不标记；checkedAt 取复核开始的时间，已有更晚的认证结果（请求成功）时不写
		marked, err := service.pool.MarkAuthenticationRequiredIfGeneration(accountID, suspect.generation, startedAt, suspect.cause.Error())
		if err != nil {
			service.requests.log(label, "WARN", "登录复核写回需要登录失败 | 错误="+err.Error())
			return
		}
		if marked {
			service.workers.confirmAuthRequiredWith(accountID, label, suspect.source, suspect.cause, "复核=仍失效")
			service.publishModelAccess()
		}
	case errors.Is(probeErr, errAuthRecheckBusy):
		// 账户有进行中的请求（长流）：不算一次复核，稍后再试
		recheck.mu.Lock()
		recheck.running--
		suspect.running = false
		suspect.due = now.Add(recheck.suspectRetryValue())
		recheck.mu.Unlock()
	default:
		recheck.mu.Lock()
		recheck.running--
		suspect.running = false
		suspect.attempts++
		giveUp := suspect.attempts >= authSuspectMaxAttempts
		if giveUp {
			delete(recheck.suspects, accountID)
		} else {
			suspect.due = now.Add(recheck.suspectRetryValue() * time.Duration(suspect.attempts))
		}
		recheck.mu.Unlock()
		if giveUp {
			service.requests.log(label, "WARN", fmt.Sprintf(
				"疑似登录失效复核未完成，不标记 | 来源=%s | 复核=%d 次 | 错误=%s",
				suspect.source, suspect.attempts, authRequiredCauseSummary(probeErr),
			))
		}
	}
}

// recordLocked 返回账户的定期复核记录，没有时新建。调用方持有 mu
func (recheck *authRecheck) recordLocked(accountID string) *authRecheckRecord {
	record := recheck.records[accountID]
	if record == nil {
		record = &authRecheckRecord{}
		recheck.records[accountID] = record
	}
	return record
}

// finishPeriodic 处理需要登录账户的定期复核结果：通过则恢复（已在租约内写回有效）、清掉目录缓存的需要登录记录并放回目录同步队列，
// 写“登录复核恢复”；仍是 401 则按账户退避；出错或账户忙记为跳过
func (recheck *authRecheck) finishPeriodic(accountID string, startedAt time.Time, valid bool, probeErr error) {
	service := recheck.service
	now := recheck.clock()
	label := accountID
	if status, ok := service.pool.StatusOf(accountID); ok && status.Label != "" {
		label = status.Label
	}
	recheck.mu.Lock()
	recheck.running--
	record := recheck.recordLocked(accountID)
	switch {
	case valid && probeErr == nil:
		attempts := record.failures + 1
		record.recoveries++
		// 恢复后又被标记时从更长的退避起复核（见 authRecheckRecord）；之后真正成功一次（noteSuccess）就清掉
		record.nextAt = now.Add(authRecheckBackoff(record.failures + record.recoveries))
		recheck.addLocked(now, func(bucket *authRecheckBucket) { bucket.recovered++ },
			func(stats *authRecheckRoundStats) { stats.recovered++ })
		recheck.mu.Unlock()
		service.catalogSync.noteAuthValid(accountID)
		service.catalogSync.requeue(accountID)
		service.requests.log(label, "INFO", fmt.Sprintf(
			"登录复核恢复 | 复核次数=%d | 耗时=%s | 接口认证正常，账号恢复就绪",
			attempts, now.Sub(startedAt).Round(time.Millisecond),
		))
		service.publishModelAccess()
		service.workers.notifySchedulingIfAny()
	case aistudio.DefinitiveAuthenticationFailure(probeErr):
		record.failures++
		record.nextAt = now.Add(authRecheckBackoff(record.failures + record.recoveries))
		recheck.addLocked(now, func(bucket *authRecheckBucket) { bucket.stillInvalid++ },
			func(stats *authRecheckRoundStats) { stats.stillInvalid++ })
		recheck.mu.Unlock()
	default:
		recheck.skipLocked(accountID, now)
		recheck.mu.Unlock()
	}
}

// status 返回状态接口的登录复核状态
func (recheck *authRecheck) status() *api.AdminAuthRecheck {
	if recheck == nil {
		return nil
	}
	recheck.mu.Lock()
	defer recheck.mu.Unlock()
	now := recheck.clock()
	status := &api.AdminAuthRecheck{
		Suspects: len(recheck.suspects), Queued: len(recheck.queue), Running: recheck.running,
		NextRound: recheck.nextRound,
	}
	minute := now.Unix() / 60
	for _, bucket := range recheck.buckets {
		if age := minute - bucket.minute; age >= 0 && age < int64(len(recheck.buckets)) {
			status.Cleared1h += bucket.cleared
			status.Confirmed1h += bucket.confirmed
			status.Recovered1h += bucket.recovered
			status.StillInvalid1h += bucket.stillInvalid
			status.Skipped1h += bucket.skipped
		}
	}
	if !recheck.egressPausedAt.IsZero() && now.Sub(recheck.egressPausedAt) < authRecheckPauseShown {
		status.PauseReason = authRecheckPauseEgress
	}
	return status
}

// recentEvents 为最近一段时间内的事件计数（按分钟分桶，最多 1 小时）；零值可用，并发安全
type recentEvents struct {
	mu      sync.Mutex
	buckets [60]struct {
		minute int64
		count  int
	}
}

// add 记一次事件
func (events *recentEvents) add(now time.Time) {
	minute := now.Unix() / 60
	events.mu.Lock()
	bucket := &events.buckets[minute%int64(len(events.buckets))]
	if bucket.minute != minute {
		bucket.minute, bucket.count = minute, 0
	}
	bucket.count++
	events.mu.Unlock()
}

// count 返回最近 window（按整分钟，含当前分钟）内的事件数
func (events *recentEvents) count(now time.Time, window time.Duration) int {
	minutes := min(int64(window/time.Minute), int64(len(events.buckets)))
	minute := now.Unix() / 60
	total := 0
	events.mu.Lock()
	for _, bucket := range events.buckets {
		if age := minute - bucket.minute; age >= 0 && age < minutes {
			total += bucket.count
		}
	}
	events.mu.Unlock()
	return total
}
