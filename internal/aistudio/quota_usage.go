package aistudio

import (
	"math"
	"time"
)

// RoutingBalanced 为 ROUTING_STRATEGY 的平均调用（balanced）：账户池内的选号与 round-robin 完全相同（SetRoutingStrategy 把它存成
// round-robin），让流量摊到更多账号上的做法都在 app 层（空闲热账号优先、冷启动按今日发送数挑号、驻留 Worker 轮换，见 app/balanced.go）。
// 账户池里五处按 "round-robin" 比较的判断不认识这个值，原样交给账户池会被当成 fill-first，所以只能在 SetRoutingStrategy 收口
const RoutingBalanced = "balanced"

// 每账号今日发送数（平均调用用，见 app/balanced.go）
//
// 口径为每个账号在当前太平洋时间额度日（次日零点恢复，与每日限额同步，见 NextQuotaDay）里真正发往上游的生成请求数：
//   - 在 recordGenerationSentLocked 里记，与每分钟限速的发送记账同一次加锁、O(1)：beginGenerationSend 与 RecordGenerationSent 每次加 1，
//     401 续签后的重放、long- 续写的每一轮、重新生成都各算一次，最终返回 429、403 的发送同样计入（上游看到的就是发送次数）
//   - 不经过发送记账的 Live、Veo、countTokens 与文件操作不计；取得租约却没有发出就释放的不计
//   - 按账户 ID 存在账户池级别的表里，不放在 Account 上：运行中重新导入账号是先 Remove 再 Add 一个新对象，计数照样保留
//   - 总是开着（与 ROUTING_STRATEGY 无关），只统计本进程；不进候选索引，分类与选号都不读它
//   - 跨重启由 app 层每 30 秒写一次 quota-usage.json（只在平均调用开启时），启动时同一额度日才经 SeedQuotaDayUsage 并回
//
// 全部状态受账户池 mu 保护
type quotaUsage struct {
	// dayEnd 为当前额度日的结束（太平洋时间次日零点），dayFrom 为算出 dayEnd 时的时刻：now 不在 [dayFrom, dayEnd) 内才重新计算
	// （NextQuotaDay 每次都要 LoadLocation，只在跨日或墙钟回拨时调用，写法同 app 层 egressGuard.quotaDayLocked）
	dayEnd, dayFrom time.Time
	// dayKey 为额度日的编号（次日零点的 Unix 分钟）：变大时清空计数；墙钟回拨从不清零，回拨到同一额度日或前一个额度日时编号不变，
	// 回拨超过一个额度日（墙钟先跳到未来、随后被校正回来）时编号跟着回到 now 所在的额度日（见 rollLocked）
	dayKey int32
	// day 为 dayKey 对应的太平洋日期（如 2026-11-18）
	day string
	// counts 为按账户 ID 的发送数
	counts map[string]uint32
	// version 在计数或额度日每次变化（发送、跨日清空、并回、时钟校正）时加一，app 层按它判断要不要写盘
	version uint64
}

// QuotaUsageSnapshot 为某一刻的今日发送数副本（见 QuotaDayUsage）
type QuotaUsageSnapshot struct {
	// DayKey 为额度日的编号（次日零点的 Unix 分钟），Day 为太平洋日期（如 2026-11-18）
	DayKey int32
	Day    string
	// Version 为取副本时的计数版本
	Version uint64
	// Counts 为按账户 ID 的发送数（副本，调用方可以随意读；只出现发送过的账号）
	Counts map[string]uint32
}

// QuotaDayKey 返回 now 所在的太平洋时间额度日编号（次日零点的 Unix 分钟），与 QuotaUsageSnapshot.DayKey 同一口径
func QuotaDayKey(now time.Time) int32 {
	return int32(nextQuotaDay(now).Unix() / 60)
}

// quotaDayMaxMinutes 为一个额度日最长的分钟数（夏令时结束那天 25 小时）：额度日编号变小不超过它时是回拨到前一个额度日
const quotaDayMaxMinutes = 25 * 60

// rollLocked 让计数跟上 now 所在的额度日：在同一额度日内什么也不做；跨到之后的额度日时清空计数。墙钟回拨从不清零：
//   - 回拨到同一额度日或前一个额度日（NTP 在零点前后的小幅校正）：只重算边界，编号不倒退，时间回到原额度日后照常累加；
//   - 回拨超过一个额度日（墙钟先跳到未来某天、随后被校正回来）：编号跟着回到 now 所在的额度日，计数保留（错误时钟期间的发送是刚刚
//     真实发出的），之后照常在零点清零。原先编号只增不减，会停在那个未来的日子：之后几个真实额度日都不清零，状态页与 quota-usage.json
//     的额度日写成未来日期（重启时与当天对不上，整个文件被忽略）
//
// 时刻一律先去掉单调时钟读数、按墙钟比较：生产里 now 都来自 time.Now()（带单调时钟读数），dayFrom 就是上一次的 now，两个时刻都带读数时
// Before 只比单调时钟，墙钟被校正回来时 now.Before(dayFrom) 永远为假，上面“回拨超过一个额度日”的处理根本走不到（dayEnd 来自 time.Date，
// 本来就按墙钟比较）
//
// 调用方持有 p.mu
func (usage *quotaUsage) rollLocked(now time.Time) {
	now = now.Round(0)
	if !usage.dayEnd.IsZero() && now.Before(usage.dayEnd) && !now.Before(usage.dayFrom) {
		return
	}
	end := nextQuotaDay(now)
	usage.dayEnd, usage.dayFrom = end, now
	key := int32(end.Unix() / 60)
	if key <= usage.dayKey && usage.dayKey-key <= quotaDayMaxMinutes {
		return
	}
	forward := key > usage.dayKey
	usage.dayKey = key
	usage.day = end.AddDate(0, 0, -1).Format(time.DateOnly)
	if len(usage.counts) == 0 {
		return
	}
	if forward {
		clear(usage.counts)
	}
	// 时钟校正时计数不变、额度日变了：同样加版本，写盘时改成正确的额度日
	usage.version++
}

// noteQuotaUsageLocked 给账号记一次今日发送（见 recordGenerationSentLocked）。调用方持有 p.mu
func (p *AccountPool) noteQuotaUsageLocked(accountID string, now time.Time) {
	usage := &p.quotaUsage
	usage.rollLocked(now)
	if usage.counts == nil {
		usage.counts = make(map[string]uint32)
	}
	if count := usage.counts[accountID]; count < math.MaxUint32 {
		usage.counts[accountID] = count + 1
	}
	usage.version++
}

// quotaDayUsageLocked 返回 now 所在额度日的发送数副本。调用方持有 p.mu
func (p *AccountPool) quotaDayUsageLocked(now time.Time) QuotaUsageSnapshot {
	usage := &p.quotaUsage
	usage.rollLocked(now)
	counts := make(map[string]uint32, len(usage.counts))
	for accountID, count := range usage.counts {
		counts[accountID] = count
	}
	return QuotaUsageSnapshot{DayKey: usage.dayKey, Day: usage.day, Version: usage.version, Counts: counts}
}

// QuotaDayUsage 一次加锁返回当前太平洋时间额度日里各账号的发送数副本（1300 个账号约几十微秒）。
// app 层按 1 秒缓存读取（见 app/balanced.go），请求路径不为它对账户池加锁
func (p *AccountPool) QuotaDayUsage() QuotaUsageSnapshot {
	if p == nil {
		return QuotaUsageSnapshot{}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.quotaDayUsageLocked(time.Now())
}

// seedQuotaDayUsageLocked 见 SeedQuotaDayUsage。调用方持有 p.mu
func (p *AccountPool) seedQuotaDayUsageLocked(dayKey int32, counts map[string]uint32, now time.Time) bool {
	usage := &p.quotaUsage
	usage.rollLocked(now)
	if dayKey != usage.dayKey {
		return false
	}
	changed := false
	for accountID, count := range counts {
		if accountID == "" || count <= usage.counts[accountID] {
			continue
		}
		if usage.counts == nil {
			usage.counts = make(map[string]uint32, len(counts))
		}
		usage.counts[accountID] = count
		changed = true
	}
	if changed {
		usage.version++
	}
	return true
}

// SeedQuotaDayUsage 并回重启前保存的今日发送数：dayKey 必须是当前额度日（见 QuotaDayKey），否则忽略并返回 false；
// 同一账号取两边较大的一个，可以重复调用（不会重复累加）。不存在的账号照样并回（按 ID 存，账号随后导入时计数已在）
func (p *AccountPool) SeedQuotaDayUsage(dayKey int32, counts map[string]uint32) bool {
	if p == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.seedQuotaDayUsageLocked(dayKey, counts, time.Now())
}
