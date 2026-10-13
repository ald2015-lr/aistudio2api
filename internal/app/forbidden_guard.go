package app

import (
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

const (
	// forbiddenWindow 统计连续 403 的时间窗口
	forbiddenWindow = 10 * time.Minute
	// forbiddenThreshold 窗口内连续 403 达到该次数时暂停账号
	forbiddenThreshold = 3
	// forbiddenMinModels 至少涉及的不同模型数：只对单个模型 403 往往是免费账号调用了付费模型，不代表账号有问题，
	// 不暂停账号，只冷却该模型（见 recordModel）
	forbiddenMinModels = 2
	// forbiddenPause 自动暂停时长，到期后自动恢复调度
	forbiddenPause = 30 * time.Minute
	// forbiddenModelFirstCooldown、forbiddenModelRepeatCooldown 为单模型 403 冷却的递增时长：同一账号同一模型第一次 403 冷却
	// 30 分钟，forbiddenModelWindow（24 小时）内第二次 6 小时，第三次起到太平洋时间次日零点（额度日切换），至少 6 小时；
	// 该模型成功一次即从第一档重新计
	forbiddenModelFirstCooldown  = 30 * time.Minute
	forbiddenModelRepeatCooldown = 6 * time.Hour
	forbiddenModelWindow         = 24 * time.Hour
)

type forbiddenEvent struct {
	at    time.Time
	model string
}

// forbiddenModelKey 为单模型 403 冷却档位的键：账号与请求的模型（不分通道，两个通道共用档位）
type forbiddenModelKey struct {
	accountID string
	modelID   string
}

// forbiddenModelStrike 为同一账号同一模型的 403 记录：level 为冷却档位（1 起），last 为最近一次 403 的时间，until 为本档冷却的到期时间
type forbiddenModelStrike struct {
	level int
	last  time.Time
	until time.Time
}

// forbiddenTracker 按账号记录连续的 403 无权限错误（任意一次成功即清零，多个模型连续 403 时暂停账号），
// 并按账号与模型记录单模型 403 的冷却档位（该模型成功一次即清零）
type forbiddenTracker struct {
	mu     sync.Mutex
	events map[string][]forbiddenEvent
	models map[forbiddenModelKey]forbiddenModelStrike
}

func newForbiddenTracker() *forbiddenTracker {
	return &forbiddenTracker{
		events: make(map[string][]forbiddenEvent),
		models: make(map[forbiddenModelKey]forbiddenModelStrike),
	}
}

// recordModel 记录一次单模型 403（账号对该模型没有权限，常见于免费账号调用付费模型），返回本次冷却的到期时间与档位。
// 本档冷却还没到期时再次 403（同一请求在另一通道上，或冷却写入前已经发出的请求）不升档、只从现在起重新计；
// 距上一次 403 不超过 24 小时升一档，否则从第一档重新计。顺带清理超过 24 小时且冷却已到期的记录
func (tracker *forbiddenTracker) recordModel(accountID string, modelID string, now time.Time) (time.Time, int) {
	if tracker == nil {
		return forbiddenModelCooldownUntil(1, now), 1
	}
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	if tracker.models == nil {
		tracker.models = make(map[forbiddenModelKey]forbiddenModelStrike)
	}
	for key, strike := range tracker.models {
		if now.Sub(strike.last) > forbiddenModelWindow && !now.Before(strike.until) {
			delete(tracker.models, key)
		}
	}
	key := forbiddenModelKey{accountID: accountID, modelID: modelID}
	strike, exists := tracker.models[key]
	switch {
	case exists && now.Before(strike.until):
	case exists && now.Sub(strike.last) <= forbiddenModelWindow:
		strike.level++
	default:
		strike.level = 1
	}
	strike.last = now
	strike.until = forbiddenModelCooldownUntil(strike.level, now)
	tracker.models[key] = strike
	return strike.until, strike.level
}

// resetModel 账号对该模型请求成功后清除单模型 403 的冷却档位
func (tracker *forbiddenTracker) resetModel(accountID string, modelID string) {
	if tracker == nil {
		return
	}
	tracker.mu.Lock()
	delete(tracker.models, forbiddenModelKey{accountID: accountID, modelID: modelID})
	tracker.mu.Unlock()
}

// forbiddenModelCooldownUntil 返回单模型 403 第 level 档的冷却到期时间
func forbiddenModelCooldownUntil(level int, now time.Time) time.Time {
	switch {
	case level <= 1:
		return now.Add(forbiddenModelFirstCooldown)
	case level == 2:
		return now.Add(forbiddenModelRepeatCooldown)
	}
	until := aistudio.NextQuotaDay(now)
	if floor := now.Add(forbiddenModelRepeatCooldown); until.Before(floor) {
		until = floor
	}
	return until
}

// record 记录一次 403；达到暂停条件时清空记录并返回 true 以及计数与涉及的模型数
func (tracker *forbiddenTracker) record(accountID string, modelID string, now time.Time) (bool, int, int) {
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	kept := make([]forbiddenEvent, 0, forbiddenThreshold)
	for _, event := range tracker.events[accountID] {
		if now.Sub(event.at) <= forbiddenWindow {
			kept = append(kept, event)
		}
	}
	kept = append(kept, forbiddenEvent{at: now, model: modelID})
	models := make(map[string]struct{}, len(kept))
	for _, event := range kept {
		models[event.model] = struct{}{}
	}
	if len(kept) >= forbiddenThreshold && len(models) >= forbiddenMinModels {
		delete(tracker.events, accountID)
		return true, len(kept), len(models)
	}
	tracker.events[accountID] = kept
	return false, len(kept), len(models)
}

// reset 账号请求成功后清零，只有"连续"的 403 才会触发暂停
func (tracker *forbiddenTracker) reset(accountID string) {
	tracker.mu.Lock()
	delete(tracker.events, accountID)
	tracker.mu.Unlock()
}

// isPermissionDenied 判断上游是否返回 HTTP 403 无权限（协议错误码 7）
func isPermissionDenied(err error) bool {
	var rpcError *aistudio.RPCError
	if !errors.As(err, &rpcError) {
		return false
	}
	return rpcError.StatusCode == http.StatusForbidden && (rpcError.Code == 7 || rpcError.Code == 0)
}
