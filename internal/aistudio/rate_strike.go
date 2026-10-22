package aistudio

import (
	"fmt"
	"strings"
	"time"
)

// 连续裸限流 429 的冷却档位：上游对额度已用完的账号常只返回 “Quota exceeded”（没有 ErrorInfo，也没有 RetryInfo、Retry-After），
// QuotaCooldownForError 按限流冷却 30 秒；没有任何“反复出现就加长”的机制时，这样的账号每 30 秒恢复、被选中、再 429，
// 驻留 Worker 也因为冷却太短永远不算冷却中或死槽位，换不上新号。这里按账号、模型（两个通道共用额度时共用一份）记录连续次数，
// 冷却到期后再次失败才升一档，档位时长由 app 层给出（Ladder，见 app/rate_limit_guard.go），超出各档后冷却到太平洋时间次日零点；
// 该通道上成功一次即清零（见 markModelAccessVerifiedClearing）。档位只在内存中，重启后从第一档起，冷却本身照常落盘

// rateStrikeKey 为连续限流档位的键：model 为按账户目录归一的裸模型 ID（不带 build: 前缀），group 为通道，为空表示两个通道共用一份
type rateStrikeKey struct {
	model string
	group Channel
}

// rateStrike 为一个档位记录
type rateStrike struct {
	level int
	// until 为本档冷却的实际到期时间
	until time.Time
	// failedAt 为最近一次写入档位的失败租约的 CheckedAt：取得租约晚于它的成功才清零
	failedAt time.Time
	// reset 为写入时的 NextQuotaDay(now)：now 不早于它时档位作废（跨额度日从第一档起）
	reset time.Time
}

// RateLimitMark 为一次裸限流 429 的冷却写入（见 MarkRateLimitCooldown）
type RateLimitMark struct {
	// AccountID 为账号，ModelID 为请求的裸模型（不带 build: 前缀）
	AccountID, ModelID string
	// Channel 为失败租约的通道（lease.Channel()），Generation 为租约的模型目录代际（lease.ModelAccessGeneration()），
	// CheckedAt 为租约的检查时间（lease.CheckedAt()）
	Channel    Channel
	Generation uint64
	CheckedAt  time.Time
	// Cooldown 为 QuotaCooldownForError 的结果（Escalate 为真），只用它的原因
	Cooldown QuotaCooldown
	// Ladder 为各档冷却时长（至少一档）：第 N 档冷却 Ladder[N-1]，超出各档后冷却到太平洋时间次日零点；每档都不超过次日零点
	Ladder []time.Duration
	// Shared 为两个通道共用档位与冷却：同时冷却另一个启用通道
	Shared bool
	// Escalate 为允许升档（号池级限流判定没有成立，且请求还没向客户端输出）；为假时只按第一档续冷却，档位不变
	Escalate bool
}

// RateLimitMarkResult 为一次裸限流冷却写入的结果
type RateLimitMarkResult struct {
	// Write 为本通道冷却键的写入结果
	Write CooldownWrite
	// Until 为本通道冷却键实际生效的恢复时间（已有更晚的冷却时保留它）
	Until time.Time
	// Level 为写入后的档位（1 起）
	Level int
	// Synced 为另一个启用通道已同步冷却（Shared 时）
	Synced bool
}

// rateLimitStepUntil 返回第 level 档的冷却到期时间：now 加该档时长，超出各档后为太平洋时间次日零点；都不晚于次日零点
func rateLimitStepUntil(ladder []time.Duration, level int, now time.Time) time.Time {
	reset := NextQuotaDay(now)
	if level < 1 || level > len(ladder) {
		return reset
	}
	if until := now.Add(ladder[level-1]); until.Before(reset) {
		return until
	}
	return reset
}

// MarkRateLimitCooldown 按连续次数写入裸限流 429 的冷却（QuotaCooldown.Escalate 为真的 429），与 TryMarkCooldownIfGeneration 一样
// 先核对目录代际与本通道冷却键的 CheckedAt。档位与冷却在一次运行态更新内完成，同账号的成功清零与其他冷却写入被串行化：
//   - 没有档位记录（或已跨额度日）时为第一档；
//   - 租约检查时间早于本档冷却到期时间（冷却写入前已经发出的并发请求，或同一请求换通道再试）或不允许升档时不升档，
//     冷却续到本档到期与第一档时长中较晚的一个；
//   - 否则（本档冷却到期后取得的租约再次失败）升一档，最多到“次日零点”一档。
//
// Shared 时另一个启用通道同步冷却（该键已有不早于本次租约的记录时不写，Synced 为假）。每个键已有更晚的冷却时保留它，不缩短；
// 两种情况都把该键的 CheckedAt 推进到本次租约。档位在运行态写盘成功后才提交，写盘失败时不变
func (p *AccountPool) MarkRateLimitCooldown(mark RateLimitMark) (RateLimitMarkResult, error) {
	checkedAt := mark.CheckedAt.UTC()
	if checkedAt.IsZero() {
		return RateLimitMarkResult{}, fmt.Errorf("冷却检查时间不能为空")
	}
	modelID := strings.TrimPrefix(strings.TrimSpace(mark.ModelID), "models/")
	if modelID == "" {
		return RateLimitMarkResult{}, fmt.Errorf("模型 ID 不能为空")
	}
	if len(mark.Ladder) == 0 {
		return RateLimitMarkResult{}, fmt.Errorf("限流冷却档位不能为空")
	}
	for _, step := range mark.Ladder {
		if step <= 0 {
			return RateLimitMarkResult{}, fmt.Errorf("限流冷却档位必须为正数")
		}
	}
	channel := mark.Channel
	if channel == "" {
		channel = ChannelPlayground
	}
	var result RateLimitMarkResult
	_, err := p.updateRuntime(mark.AccountID, func(account *Account, runtimeState *accountRuntimeState) (bool, func(*Account), error) {
		result = RateLimitMarkResult{}
		if mark.Generation != account.modelAccessGeneration {
			result.Write = CooldownSkippedGeneration
			return false, nil, nil
		}
		scopeKey := canonicalAccountModelID(account, ChannelCooldownScope(channel, modelID))
		if access := runtimeState.ModelAccess[scopeKey]; !access.CheckedAt.Before(checkedAt) {
			result.Write = CooldownSkippedStale
			return false, nil, nil
		}
		now := time.Now()
		key := rateStrikeKey{model: canonicalAccountModelID(account, modelID)}
		if !mark.Shared {
			key.group = channel
		}
		previous, exists := account.rateStrikes[key]
		if exists && !now.Before(previous.reset) {
			previous, exists = rateStrike{}, false
		}
		level, until := 1, rateLimitStepUntil(mark.Ladder, 1, now)
		switch {
		case !exists:
		case checkedAt.Before(previous.until) || !mark.Escalate:
			level = previous.level
			if previous.until.After(until) {
				until = previous.until
			}
		default:
			level = min(previous.level+1, len(mark.Ladder)+1)
			until = rateLimitStepUntil(mark.Ladder, level, now)
		}
		reason := fmt.Sprintf("%s: 连续限流第 %d 档，%s", RateLimitKind, level, strings.TrimPrefix(mark.Cooldown.Reason, RateLimitKind+": "))
		// writeScope 写入一个冷却键：已有仍生效且更晚的冷却时保留（不缩短，原因也保留），返回该键实际生效的恢复时间
		writeScope := func(scope string) time.Time {
			access := runtimeState.ModelAccess[scope]
			access.CheckedAt = checkedAt
			runtimeState.ModelAccess[scope] = access
			if current, cooling := runtimeState.Cooldowns[scope]; cooling && current.Active(now) && current.Until.After(until) {
				return current.Until
			}
			runtimeState.Cooldowns[scope] = CooldownState{Until: until.UTC(), Reason: reason}
			return until
		}
		result.Write, result.Level = CooldownWritten, level
		result.Until = writeScope(scopeKey)
		if other := otherGenerationChannel(channel); mark.Shared && p.channelEnabledLocked(other) {
			otherKey := canonicalAccountModelID(account, ChannelCooldownScope(other, modelID))
			if access := runtimeState.ModelAccess[otherKey]; access.CheckedAt.Before(checkedAt) {
				writeScope(otherKey)
				result.Synced = true
			}
		}
		strike := rateStrike{level: level, until: result.Until, failedAt: checkedAt, reset: NextQuotaDay(now)}
		if previous.failedAt.After(strike.failedAt) {
			strike.failedAt = previous.failedAt
		}
		return true, func(account *Account) {
			for stale, record := range account.rateStrikes {
				if !now.Before(record.reset) {
					delete(account.rateStrikes, stale)
				}
			}
			if account.rateStrikes == nil {
				account.rateStrikes = make(map[rateStrikeKey]rateStrike)
			}
			account.rateStrikes[key] = strike
		}, nil
	})
	if err != nil {
		return RateLimitMarkResult{}, err
	}
	return result, nil
}

// otherGenerationChannel 返回生成请求的另一个上游通道
func otherGenerationChannel(channel Channel) Channel {
	if channel == ChannelBuild {
		return ChannelPlayground
	}
	return ChannelBuild
}

// verifiedStrikeKeys 返回一次生成成功可以清零的档位键：成功记录的作用域以 build: 开头时为 Build 通道，否则为 Playground；
// 清两个通道共用的一份与该通道自己的一份（不共用时另一通道的档位与这次成功无关）。调用方持有 p.mu
func verifiedStrikeKeys(account *Account, scope string) [2]rateStrikeKey {
	channel, modelID := ChannelPlayground, scope
	if rest, found := strings.CutPrefix(scope, string(ChannelBuild)+":"); found {
		channel, modelID = ChannelBuild, rest
	}
	modelID = canonicalAccountModelID(account, modelID)
	return [2]rateStrikeKey{{model: modelID}, {model: modelID, group: channel}}
}

// clearableRateStrikeLocked 判断取得租约时间为 checkedAt 的成功能否清零某个档位（档位最近一次失败早于这次租约、且没有跨额度日）；
// 调用方持有 p.mu
func clearableRateStrikeLocked(account *Account, scope string, checkedAt time.Time, now time.Time) bool {
	for _, key := range verifiedStrikeKeys(account, scope) {
		if strike, exists := account.rateStrikes[key]; exists && now.Before(strike.reset) && checkedAt.After(strike.failedAt) {
			return true
		}
	}
	return false
}

// clearRateStrikesLocked 清零取得租约时间为 checkedAt 的成功所在通道的档位，返回被清档位中的最高档（没有时为 0）；
// 已跨额度日的记录一并删除，不计入返回值。调用方持有 p.mu
func clearRateStrikesLocked(account *Account, scope string, checkedAt time.Time, now time.Time) int {
	cleared := 0
	for _, key := range verifiedStrikeKeys(account, scope) {
		strike, exists := account.rateStrikes[key]
		if !exists {
			continue
		}
		if !now.Before(strike.reset) {
			delete(account.rateStrikes, key)
			continue
		}
		if checkedAt.After(strike.failedAt) {
			delete(account.rateStrikes, key)
			cleared = max(cleared, strike.level)
		}
	}
	return cleared
}
