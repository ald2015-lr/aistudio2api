package app

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// 连续裸限流 429 的逐级冷却：上游对额度已用完的账号常只返回 “Quota exceeded”（没有 ErrorInfo，也没有 RetryInfo、Retry-After），
// 按限流只冷却 30 秒时这样的账号每 30 秒恢复、被选中、再 429；冷却太短，它的驻留 Worker 既不算冷却中（剩余不到 ShortCooldownMax），
// 也成不了死槽位（剩余不到 rotationMinRemaining），分区满时换不上新号。同一账号同一模型（两个通道共用额度时共用一份，见 sharedFor）
// 在冷却到期后再次裸限流就升一档（见 aistudio.MarkRateLimitCooldown），该通道成功一次即清零：
//   - 第 2 档 5 分钟：剩余 5m→2m 这 3 分钟内算冷却中，分区满时可被 coolingVictim、victimCooling 替换；
//   - 第 3 档 30 分钟：剩余 30m→15m 这 15 分钟内是死槽位（登记淘汰、补登记、轮换），足够排空超时（对不超过 REQUEST_TIMEOUT 的请求成立；
//     更长的流由冷却登记在 15 分钟窗口结束时撤销、随之清除排空，见 worker_retire.go）；
//   - 第 4 档 2 小时，第 5 档起冷却到太平洋时间次日零点。
//
// 每档都不超过太平洋时间次日零点：23:30 之后各档被截短，可能不足 15 分钟，只算冷却中，零点后额度恢复。
// 号池级保护：号池内 10 分钟内至少 5 个账号对该模型裸限流、这段时间又没有任何成功时（上游整体限流或故障），只按第一档冷却、不升档，
// 免得把整个号池逐级冷却到数小时（冷却会落盘）。代价是常驻 Worker 全部用完额度、10 分钟没有任何成功的极端情况下仍停在第一档
const (
	rateLimitLevel1 = 30 * time.Second // 与 aistudio 的 unknownRateLimitCooldown 相同
	rateLimitLevel2 = 5 * time.Minute  // > ShortCooldownMax(2m)+1m：算“冷却中”，分区满时可被 coolingVictim/victimCooling 替换
	rateLimitLevel3 = 30 * time.Minute // >= rotationMinRemaining(15m)+10m：成为死槽位，对不超过 REQUEST_TIMEOUT 的请求留出足够的排空余量
	rateLimitLevel4 = 2 * time.Hour
	// 第 5 档起冷却到太平洋时间次日零点（NextQuotaDay）

	// rateLimitSystemicWindow 与 rateLimitSystemicAccounts：窗口内同一号池同一模型有这么多个不同账号裸限流、且这段时间该模型在号池里
	// 没有任何成功时视为号池级限流，不升档（与号池级 403 的判定一致，见 forbiddenSystemicAccounts）
	rateLimitSystemicWindow   = 10 * time.Minute
	rateLimitSystemicAccounts = 5
)

// rateLimitSteps 返回各档冷却时长：rateLimitLadder 为空时为默认四档（只给包内测试改小，放在实例上，并行测试互不影响）
func (service *trackedService) rateLimitSteps() []time.Duration {
	if len(service.rateLimitLadder) > 0 {
		return service.rateLimitLadder
	}
	return []time.Duration{rateLimitLevel1, rateLimitLevel2, rateLimitLevel3, rateLimitLevel4}
}

// rateLimitScopeKey 为号池级限流判定的键：号池与模型
type rateLimitScopeKey struct {
	pool    aistudio.PoolScope
	modelID string
}

// rateLimitFailure 为号池内一个账号对该模型最近一次裸限流的时间
type rateLimitFailure struct {
	at        time.Time
	accountID string
}

// rateLimitScopeState 记录号池内该模型最近裸限流的不同账号（最多 rateLimitSystemicAccounts 个）、最近一次成功的时间与最近一次告警的时间
type rateLimitScopeState struct {
	failed      []rateLimitFailure
	lastSuccess time.Time
	warnedAt    time.Time
}

// rateLimitGuard 按号池与模型判断号池级限流（结构同 forbiddenTracker 的号池级 403）；方法都 nil 安全
type rateLimitGuard struct {
	mu     sync.Mutex
	scopes map[rateLimitScopeKey]rateLimitScopeState
}

func newRateLimitGuard() *rateLimitGuard {
	return &rateLimitGuard{scopes: make(map[rateLimitScopeKey]rateLimitScopeState)}
}

// observe 记录号池内一次裸限流，返回是否为号池级限流：rateLimitSystemicWindow 内裸限流的不同账号达到 rateLimitSystemicAccounts 个，
// 且这段时间该模型在号池里没有成功（启动以来没有成功过也算）。判定成立且距上一次告警超过窗口时 warn 为真（每个窗口只告警一次）。
// 只保留每个账号最近的一次、最多 rateLimitSystemicAccounts 个
func (guard *rateLimitGuard) observe(pool aistudio.PoolScope, accountID string, modelID string, now time.Time) (systemic bool, warn bool) {
	if guard == nil {
		return false, false
	}
	guard.mu.Lock()
	defer guard.mu.Unlock()
	if guard.scopes == nil {
		guard.scopes = make(map[rateLimitScopeKey]rateLimitScopeState)
	}
	key := rateLimitScopeKey{pool: pool, modelID: modelID}
	state := guard.scopes[key]
	kept := make([]rateLimitFailure, 0, rateLimitSystemicAccounts)
	for _, failure := range state.failed {
		if failure.accountID != accountID && now.Sub(failure.at) <= rateLimitSystemicWindow {
			kept = append(kept, failure)
		}
	}
	kept = append(kept, rateLimitFailure{at: now, accountID: accountID})
	if len(kept) > rateLimitSystemicAccounts {
		kept = kept[len(kept)-rateLimitSystemicAccounts:]
	}
	state.failed = kept
	systemic = len(kept) >= rateLimitSystemicAccounts && now.Sub(state.lastSuccess) > rateLimitSystemicWindow
	if systemic && now.Sub(state.warnedAt) > rateLimitSystemicWindow {
		warn = true
		state.warnedAt = now
	}
	guard.scopes[key] = state
	return systemic, warn
}

// succeeded 号池内该模型请求成功：清空号池级限流的记录并记下成功时间
func (guard *rateLimitGuard) succeeded(pool aistudio.PoolScope, modelID string, now time.Time) {
	if guard == nil {
		return
	}
	guard.mu.Lock()
	defer guard.mu.Unlock()
	if guard.scopes == nil {
		guard.scopes = make(map[rateLimitScopeKey]rateLimitScopeState)
	}
	guard.scopes[rateLimitScopeKey{pool: pool, modelID: modelID}] = rateLimitScopeState{lastSuccess: now}
}

// applyRateLimitCooldown 为 applyQuotaCooldown 中裸限流 429（cooldown.Escalate 为真）的分支：按连续次数逐级加长冷却（见本文件开头）。
// escalate 为这次失败可以作为“冷却到期后仍失败”的证据（请求还没向客户端输出）；号池级限流判定成立时同样不升档。
// 两个通道没有判定为独立额度时（共用或观察中）共用档位并同步冷却另一个启用通道。返回的 cooldown 带实际写入的恢复时间。
// 生成本身返回的裸限流（首事件前与流内）先交给出口判定器（见 egress_guard.go）：账户所在出口判定成立时只短冷却、不升档
// （或按账号证据降级），不再走号池级判定与逐级冷却
func (service *trackedService) applyRateLimitCooldown(
	lease *aistudio.AccountLease, accountLabel string, modelID string, cooldown aistudio.QuotaCooldown, note string,
	pool aistudio.PoolScope, escalate bool,
) (aistudio.QuotaCooldown, bool, error) {
	accountID := lease.Account().ID
	// 坏号记忆按账号（不分模型）累计连续裸限流（见 bad_accounts.go），与出口判定解耦；先记它，出口判定的“冷号首发被拒”据此排除坏号
	service.workers.badAccountMemory().observe(accountID, accountLabel, egressBare)
	if verdict := service.egress.observeOutcome(egressObservation{
		Key: lease.EgressKey(), AccountID: accountID, Ultra: lease.EgressLane() == aistudio.EgressLaneUltra,
		Outcome: egressBare, At: time.Now(),
	}); verdict.Limited {
		return service.applyEgressLimitedCooldown(lease, accountLabel, modelID, cooldown, note, verdict)
	}
	scopeLabel := lease.CooldownScope(modelID)
	allow, systemic := escalate, false
	if escalate {
		var warn bool
		systemic, warn = service.rateLimits.observe(pool, accountID, modelID, time.Now())
		if systemic {
			allow = false
		}
		if warn {
			// applyQuotaCooldown 拿不到请求 ID，只写服务日志；紧跟在成批的冷却日志之后，控制台队列满时也不丢
			poolField := ""
			if label := pool.Label(); label != "" {
				poolField = " | 号池=" + label
			}
			service.requests.logDurable("service", "WARN", fmt.Sprintf(
				"号池级限流 | 模型=%s%s | 号池内 %d 分钟内至少 %d 个账号返回限流且没有成功，不升级冷却",
				modelID, poolField, int(rateLimitSystemicWindow/time.Minute), rateLimitSystemicAccounts,
			))
		}
	}
	// 共用判定按账户真实权益分组（与每日限额的通道学习一致，见 dailyLimitHit）
	shared := service.quota.sharedFor(modelID, service.pool.TierPoolOf(accountID))
	result, stateErr := service.pool.MarkRateLimitCooldown(aistudio.RateLimitMark{
		AccountID: accountID, ModelID: modelID, Channel: lease.Channel(),
		Generation: lease.ModelAccessGeneration(), CheckedAt: lease.CheckedAt(),
		Cooldown: cooldown, Ladder: service.rateLimitSteps(), Shared: shared, Escalate: allow,
	})
	if stateErr != nil {
		return cooldown, true, stateErr
	}
	suffix := ""
	if note != "" {
		suffix = " | " + note
	}
	if result.Write != aistudio.CooldownWritten {
		service.requests.log(accountLabel, "INFO", fmt.Sprintf(
			"账号冷却未写入 | 类型=%s | 范围=%s | 恢复=%s%s | 原因=%s%s",
			cooldown.Kind, scopeLabel, cooldown.Until.Format(time.RFC3339), quotaCooldownFields(cooldown), result.Write.String(), suffix,
		))
		return cooldown, true, nil
	}
	// 先换成实际写入的恢复时间再登记淘汰（defer 的实参在声明处求值）：第三档起剩余不少于 rotationMinRemaining、
	// 两个通道都已冷却（共用时）或只启用一个通道时成了死槽位；调用方按它记 Ultra 主用模型的额度失败类别
	cooldown.Until = result.Until
	defer service.retireCooledWorker(accountID, modelID, cooldown)
	fields := fmt.Sprintf(" | 连续限流=第 %d 档", result.Level)
	if result.Synced {
		fields += " | 同步=两个通道"
	}
	if systemic {
		fields += " | 号池级限流不升档"
	}
	service.requests.log(accountLabel, "WARN", fmt.Sprintf(
		"账号冷却 | 类型=%s | 范围=%s | 恢复=%s%s%s%s",
		cooldown.Kind, scopeLabel, cooldown.Until.Format(time.RFC3339), quotaCooldownFields(cooldown), fields, suffix,
	))
	return cooldown, true, nil
}

// applyEgressLimitedCooldown 为出口判定成立期间的裸限流冷却：这时被拒的是出口而不是账号，逐级冷却只会把驻留 Worker 盖成死槽位，
// 引发淘汰与替补冷启动的风暴（线上 30 分钟 700–1000 次启停）。
//   - 只冷却本通道 egressLimitedCooldown（10 秒）：经 TryExtendCooldownIfGeneration 写入，不缩短更晚的冷却、不建档位、不同步另一通道、
//     不登记淘汰；10 秒 < ShortCooldownMax，不算冷却中，也成不了死槽位，又足够让并发请求不立刻再选中它；
//   - 判定器按账号证据降级（verdict.Demote）时改写 egressDemoteCooldown（30 分钟）账号冷却：两个通道共用额度时（sharedFor）两个启用通道
//     各写一次，随后照常登记淘汰，成为死槽位后由淘汰与补位换掉。原因不以“连续限流第”开头，进入判定时的撤回不动它。
//
// 返回的 cooldown 保留 Escalate（换号按限流类计），Until 为实际生效的恢复时间
func (service *trackedService) applyEgressLimitedCooldown(
	lease *aistudio.AccountLease, accountLabel string, modelID string, cooldown aistudio.QuotaCooldown, note string, verdict egressVerdict,
) (aistudio.QuotaCooldown, bool, error) {
	accountID := lease.Account().ID
	now := time.Now()
	original := strings.TrimPrefix(cooldown.Reason, aistudio.RateLimitKind+": ")
	until := now.Add(service.egress.limitedCooldown())
	reason := aistudio.RateLimitKind + ": 出口限流短冷却，" + original
	fields := " | 出口限流不升档 | 出口=" + verdict.Label
	scopes := []string{lease.CooldownScope(modelID)}
	if verdict.Demote {
		until = now.Add(egressDemoteCooldown)
		reason = fmt.Sprintf("%s: 出口限流期间账号持续被拒（连续 %d 次），%s", aistudio.RateLimitKind, verdict.DemoteStreak, original)
		fields = fmt.Sprintf(" | 出口限流按账号冷却 | 连续=%d | 出口=%s", verdict.DemoteStreak, verdict.Label)
		// 共用判定按账户真实权益分组（与逐级冷却一致）；另一通道只在启用时写
		if service.quota.sharedFor(modelID, service.pool.TierPoolOf(accountID)) {
			other := aistudio.ChannelBuild
			enabled := service.pool.BuildEnabled()
			if lease.Channel() == aistudio.ChannelBuild {
				other, enabled = aistudio.ChannelPlayground, service.pool.PlaygroundEnabled()
			}
			if enabled {
				scopes = append(scopes, aistudio.ChannelCooldownScope(other, modelID))
			}
		}
	}
	suffix := ""
	if note != "" {
		suffix = " | " + note
	}
	cooldown.Reason = reason
	effective := time.Time{}
	written := false
	for index, scope := range scopes {
		write, effectiveUntil, stateErr := service.pool.TryExtendCooldownIfGeneration(
			accountID, scope, lease.ModelAccessGeneration(), lease.CheckedAt(), until, reason,
		)
		if stateErr != nil {
			cooldown.Until = until
			return cooldown, true, stateErr
		}
		recoverAt := until
		if write == aistudio.CooldownKeptLater {
			recoverAt = effectiveUntil
		}
		if index == 0 {
			effective = recoverAt
		}
		if write != aistudio.CooldownWritten {
			service.requests.log(accountLabel, "INFO", fmt.Sprintf(
				"账号冷却未写入 | 类型=%s | 范围=%s | 恢复=%s%s | 原因=%s%s%s",
				cooldown.Kind, scope, recoverAt.Format(time.RFC3339), quotaCooldownFields(cooldown), write.String(), fields, suffix,
			))
			continue
		}
		written = true
		service.requests.log(accountLabel, "WARN", fmt.Sprintf(
			"账号冷却 | 类型=%s | 范围=%s | 恢复=%s%s%s%s",
			cooldown.Kind, scope, until.Format(time.RFC3339), quotaCooldownFields(cooldown), fields, suffix,
		))
	}
	cooldown.Until = effective
	if verdict.Demote && written {
		// 降级的 30 分钟冷却与逐级冷却第 3 档一样成为死槽位：空闲时立即淘汰，忙碌时等请求释放租约后淘汰（见 worker_retire.go）
		service.retireCooledWorker(accountID, modelID, aistudio.QuotaCooldown{
			Until: until, Kind: cooldown.Kind, Reason: reason, Escalate: cooldown.Escalate,
		})
	}
	return cooldown, true, nil
}
