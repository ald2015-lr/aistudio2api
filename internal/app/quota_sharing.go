package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// 同一账户的 Build 与 Playground 两个通道是否共用某模型的每日额度，从实际请求中学习：
// 一个通道达到每日限额后，看该账户在另一个通道上的下一次尝试——
// 成功说明两个通道额度独立，同样达到每日限额说明共用。
// 证据足够判定为共用后，一个通道达到每日限额就同时冷却另一个通道，
// 不再让请求去另一个通道白白失败一次、再换号重试。
// 学到的结论保存在账户目录的 .quota-sharing.json，重启后继续有效。
// 结论按账户真实权益分组学习：Ultra 账户的额度与其他权益不同，键为 模型|ultra；其余权益（含 ULTRA_INCLUDE_PRO=true 时
// 并入 Ultra 号池的 Pro 账户）的键仍为模型 ID，已保存的记录继续有效。分组不随 ULTRA_INCLUDE_PRO 变化：
// Pro 的证据若记进 模型|ultra，关掉开关后也不会消失
const (
	quotaSharingFileName = ".quota-sharing.json"
	// quotaSharedMinEvidence 至少这么多次"另一通道随后也达到每日限额"才判定为共用
	quotaSharedMinEvidence = 8
	// quotaSharedRatio 共用证据需不少于独立证据的该倍数（即独立占比低于约 5%）
	quotaSharedRatio = 20
	// quotaPendingTTL 另一个通道在这段时间内没有新的尝试，就放弃这次观察
	quotaPendingTTL = 30 * time.Minute
	// quotaRevisitEvery 判定为共用后，每这么多次每日限额放行一次另一个通道作为复核
	quotaRevisitEvery = 20
	// dailyQuotaKind 为 aistudio.QuotaCooldownForError 返回的每日限额类型
	dailyQuotaKind = aistudio.DailyQuotaKind
)

// quotaSharingStats 为某模型的观察计数
type quotaSharingStats struct {
	Shared      int `json:"shared"`
	Independent int `json:"independent"`
}

// quotaSharing 记录各模型两个通道是否共用每日额度
type quotaSharing struct {
	mu       sync.Mutex
	path     string
	requests *requestRegistry
	models   map[string]*quotaSharingStats
	// pending 为等待验证的观察：键为 账户|模型|通道，值为另一个通道达到每日限额的时间与账户当时的权益分组
	pending map[string]quotaPending
	// sinceRevisit 为判定共用后各模型（按权益分组分开）距上次复核的每日限额次数（只在内存中）
	sinceRevisit map[string]int
}

// quotaPending 为一次等待验证的观察：证据记入账户当时权益分组的判定（pool 为 TierPoolOf 的结果，不是号池成员关系）
type quotaPending struct {
	since time.Time
	pool  aistudio.PoolScope
}

// quotaStatsKey 返回模型在权益分组内的判定键：Ultra 权益为 模型|ultra，其余权益沿用模型 ID
func quotaStatsKey(modelID string, pool aistudio.PoolScope) string {
	if pool == aistudio.PoolScopeUltra {
		return modelID + "|" + pool.String()
	}
	return modelID
}

func newQuotaSharing(directory string, requests *requestRegistry) *quotaSharing {
	sharing := &quotaSharing{
		requests:     requests,
		models:       make(map[string]*quotaSharingStats),
		pending:      make(map[string]quotaPending),
		sinceRevisit: make(map[string]int),
	}
	if strings.TrimSpace(directory) != "" {
		sharing.path = filepath.Join(directory, quotaSharingFileName)
		if err := sharing.load(); err != nil && requests != nil {
			requests.log("service", "WARN", fmt.Sprintf("每日额度通道判定记录读取失败，将重新学习 | 错误=%v", err))
		}
	}
	return sharing
}

func (sharing *quotaSharing) load() error {
	data, err := os.ReadFile(sharing.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var stored struct {
		Models map[string]*quotaSharingStats `json:"models"`
	}
	if err := json.Unmarshal(data, &stored); err != nil {
		return err
	}
	for model, stats := range stored.Models {
		if stats != nil {
			sharing.models[model] = stats
		}
	}
	return nil
}

// saveLocked 原子写入判定记录；调用方持有 mu
func (sharing *quotaSharing) saveLocked() {
	if sharing.path == "" {
		return
	}
	data, err := json.MarshalIndent(struct {
		Models map[string]*quotaSharingStats `json:"models"`
	}{Models: sharing.models}, "", "  ")
	if err == nil {
		err = writeFileAtomic(sharing.path, append(data, '\n'), 0o600)
	}
	if err != nil && sharing.requests != nil {
		sharing.requests.log("service", "WARN", fmt.Sprintf("每日额度通道判定记录保存失败 | 错误=%v", err))
	}
}

func quotaSharingKey(accountID string, modelID string, channel aistudio.Channel) string {
	return accountID + "|" + modelID + "|" + string(channel)
}

// otherChannel 返回另一个上游通道
func otherChannel(channel aistudio.Channel) aistudio.Channel {
	if channel == aistudio.ChannelBuild {
		return aistudio.ChannelPlayground
	}
	return aistudio.ChannelBuild
}

// sharedLocked 判断判定键（quotaStatsKey）对应模型的两个通道是否已判定为共用每日额度；调用方持有 mu
func (sharing *quotaSharing) sharedLocked(statsKey string) bool {
	stats := sharing.models[statsKey]
	return stats != nil && stats.Shared >= quotaSharedMinEvidence && stats.Independent*quotaSharedRatio <= stats.Shared
}

// verdictLocked 返回用于日志的判定文字；调用方持有 mu
func (sharing *quotaSharing) verdictLocked(statsKey string) string {
	stats := sharing.models[statsKey]
	switch {
	case sharing.sharedLocked(statsKey):
		return "共用（达到每日限额时同时冷却两个通道）"
	case stats != nil && stats.Independent > 0 && stats.Independent*quotaSharedRatio > stats.Shared:
		return "独立（两个通道分别计算额度）"
	default:
		return "观察中"
	}
}

// sharedFor 判断模型在权益分组（TierPoolOf 的结果）内的两个通道是否按共用额度处理：只有已判定为独立（与 verdictLocked 的“独立”
// 条件一致）时返回 false，共用、观察中与没有记录都返回 true。只读，不改等待验证的观察与复核计数。
// 连续裸限流的冷却档位按它决定两个通道是否共用一份（见 rate_limit_guard.go）：未知时按共用，免得同一账号在两个通道上轮流白试；nil 时为 true
func (sharing *quotaSharing) sharedFor(modelID string, pool aistudio.PoolScope) bool {
	if sharing == nil {
		return true
	}
	sharing.mu.Lock()
	defer sharing.mu.Unlock()
	stats := sharing.models[quotaStatsKey(modelID, pool)]
	return stats == nil || stats.Independent <= 0 || stats.Independent*quotaSharedRatio <= stats.Shared
}

// recordLocked 记录模型在权益分组内的一次证据并保存、写日志；调用方持有 mu
func (sharing *quotaSharing) recordLocked(modelID string, pool aistudio.PoolScope, shared bool) {
	statsKey := quotaStatsKey(modelID, pool)
	stats := sharing.models[statsKey]
	if stats == nil {
		stats = &quotaSharingStats{}
		sharing.models[statsKey] = stats
	}
	before := sharing.verdictLocked(statsKey)
	if shared {
		stats.Shared++
	} else {
		stats.Independent++
	}
	sharing.saveLocked()
	if sharing.requests != nil {
		level := "INFO"
		if after := sharing.verdictLocked(statsKey); after != before {
			level = "WARN"
		}
		poolField := ""
		if pool == aistudio.PoolScopeUltra {
			poolField = " | 权益=Ultra"
		}
		message := fmt.Sprintf(
			"每日额度通道判定 | 模型=%s%s | 另一通道也达到限额=%d | 另一通道仍可用=%d | 结论=%s",
			modelID, poolField, stats.Shared, stats.Independent, sharing.verdictLocked(statsKey),
		)
		if level == "WARN" {
			// 结论变化决定两个通道是否同步冷却，很少出现，控制台队列满时也不丢
			sharing.requests.logDurable("service", level, message)
		} else {
			sharing.requests.log("service", level, message)
		}
	}
}

// pruneLocked 丢弃过期的观察；调用方持有 mu
func (sharing *quotaSharing) pruneLocked(now time.Time) {
	for key, pending := range sharing.pending {
		if now.Sub(pending.since) > quotaPendingTTL {
			delete(sharing.pending, key)
		}
	}
}

// dailyLimitHit 在账户的某个通道达到模型每日限额时调用，pool 为账户按真实权益的分组（TierPoolOf，不随 ULTRA_INCLUDE_PRO 变化）。
// 若这是另一个通道达到限额后该通道的首次尝试，记为"共用"证据；
// 返回是否应同时冷却另一个通道（模型在该权益分组内已判定为共用额度时）
func (sharing *quotaSharing) dailyLimitHit(
	accountID string,
	modelID string,
	pool aistudio.PoolScope,
	channel aistudio.Channel,
	leaseCheckedAt time.Time,
	now time.Time,
) (aistudio.Channel, bool) {
	if sharing == nil || accountID == "" || modelID == "" {
		return "", false
	}
	sharing.mu.Lock()
	defer sharing.mu.Unlock()
	sharing.pruneLocked(now)
	key := quotaSharingKey(accountID, modelID, channel)
	if pending, exists := sharing.pending[key]; exists {
		delete(sharing.pending, key)
		// 只有在另一个通道达到限额之后才发起的尝试才算证据
		if leaseCheckedAt.After(pending.since) {
			sharing.recordLocked(modelID, pending.pool, true)
		}
	}
	other := otherChannel(channel)
	statsKey := quotaStatsKey(modelID, pool)
	if sharing.sharedLocked(statsKey) {
		// 判定为共用后另一个通道总是同时冷却，不再有新的证据，判定原先永远不会改变。
		// 定期放行一次另一个通道作为复核：额度后来变为独立时，判定随之修正
		sharing.sinceRevisit[statsKey]++
		if sharing.sinceRevisit[statsKey] < quotaRevisitEvery {
			return other, true
		}
		sharing.sinceRevisit[statsKey] = 0
	}
	sharing.pending[quotaSharingKey(accountID, modelID, other)] = quotaPending{since: now, pool: pool}
	return other, false
}

// attemptSucceeded 在某次尝试成功收到上游首个事件时调用。
// 若该通道正等待验证、且这次尝试是在另一个通道达到限额之后才发起的，记为"独立"证据
func (sharing *quotaSharing) attemptSucceeded(
	accountID string,
	modelID string,
	channel aistudio.Channel,
	leaseCheckedAt time.Time,
) {
	if sharing == nil || accountID == "" || modelID == "" {
		return
	}
	sharing.mu.Lock()
	defer sharing.mu.Unlock()
	key := quotaSharingKey(accountID, modelID, channel)
	pending, exists := sharing.pending[key]
	if !exists || !leaseCheckedAt.After(pending.since) {
		return
	}
	delete(sharing.pending, key)
	sharing.recordLocked(modelID, pending.pool, false)
}
