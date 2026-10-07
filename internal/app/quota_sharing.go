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
// 学到的结论保存在账户目录的 .quota-sharing.json，重启后继续有效
const (
	quotaSharingFileName = ".quota-sharing.json"
	// quotaSharedMinEvidence 至少这么多次"另一通道随后也达到每日限额"才判定为共用
	quotaSharedMinEvidence = 8
	// quotaSharedRatio 共用证据需不少于独立证据的该倍数（即独立占比低于约 5%）
	quotaSharedRatio = 20
	// quotaPendingTTL 另一个通道在这段时间内没有新的尝试，就放弃这次观察
	quotaPendingTTL = 30 * time.Minute
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
	// pending 为等待验证的观察：键为 账户|模型|通道，值为另一个通道达到每日限额的时间
	pending map[string]time.Time
}

func newQuotaSharing(directory string, requests *requestRegistry) *quotaSharing {
	sharing := &quotaSharing{
		requests: requests,
		models:   make(map[string]*quotaSharingStats),
		pending:  make(map[string]time.Time),
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

// sharedLocked 判断模型的两个通道是否已判定为共用每日额度；调用方持有 mu
func (sharing *quotaSharing) sharedLocked(modelID string) bool {
	stats := sharing.models[modelID]
	return stats != nil && stats.Shared >= quotaSharedMinEvidence && stats.Independent*quotaSharedRatio <= stats.Shared
}

// verdictLocked 返回用于日志的判定文字；调用方持有 mu
func (sharing *quotaSharing) verdictLocked(modelID string) string {
	stats := sharing.models[modelID]
	switch {
	case sharing.sharedLocked(modelID):
		return "共用（达到每日限额时同时冷却两个通道）"
	case stats != nil && stats.Independent > 0 && stats.Independent*quotaSharedRatio > stats.Shared:
		return "独立（两个通道分别计算额度）"
	default:
		return "观察中"
	}
}

// recordLocked 记录一次证据并保存、写日志；调用方持有 mu
func (sharing *quotaSharing) recordLocked(modelID string, shared bool) {
	stats := sharing.models[modelID]
	if stats == nil {
		stats = &quotaSharingStats{}
		sharing.models[modelID] = stats
	}
	before := sharing.verdictLocked(modelID)
	if shared {
		stats.Shared++
	} else {
		stats.Independent++
	}
	sharing.saveLocked()
	if sharing.requests != nil {
		level := "INFO"
		if after := sharing.verdictLocked(modelID); after != before {
			level = "WARN"
		}
		sharing.requests.log("service", level, fmt.Sprintf(
			"每日额度通道判定 | 模型=%s | 另一通道也达到限额=%d | 另一通道仍可用=%d | 结论=%s",
			modelID, stats.Shared, stats.Independent, sharing.verdictLocked(modelID),
		))
	}
}

// pruneLocked 丢弃过期的观察；调用方持有 mu
func (sharing *quotaSharing) pruneLocked(now time.Time) {
	for key, since := range sharing.pending {
		if now.Sub(since) > quotaPendingTTL {
			delete(sharing.pending, key)
		}
	}
}

// dailyLimitHit 在账户的某个通道达到模型每日限额时调用。
// 若这是另一个通道达到限额后该通道的首次尝试，记为"共用"证据；
// 返回是否应同时冷却另一个通道（模型已判定为共用额度时）
func (sharing *quotaSharing) dailyLimitHit(
	accountID string,
	modelID string,
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
	if since, exists := sharing.pending[key]; exists {
		delete(sharing.pending, key)
		// 只有在另一个通道达到限额之后才发起的尝试才算证据
		if leaseCheckedAt.After(since) {
			sharing.recordLocked(modelID, true)
		}
	}
	other := otherChannel(channel)
	if sharing.sharedLocked(modelID) {
		return other, true
	}
	sharing.pending[quotaSharingKey(accountID, modelID, other)] = now
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
	since, exists := sharing.pending[key]
	if !exists || !leaseCheckedAt.After(since) {
		return
	}
	delete(sharing.pending, key)
	sharing.recordLocked(modelID, false)
}
