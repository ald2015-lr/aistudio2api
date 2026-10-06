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
	// forbiddenMinModels 至少涉及的不同模型数：只对单个模型 403 往往是免费账号调用了付费模型，不代表账号有问题
	forbiddenMinModels = 2
	// forbiddenPause 自动暂停时长，到期后自动恢复调度
	forbiddenPause = 30 * time.Minute
)

type forbiddenEvent struct {
	at    time.Time
	model string
}

// forbiddenTracker 按账号记录连续的 403 无权限错误；任意一次成功即清零
type forbiddenTracker struct {
	mu     sync.Mutex
	events map[string][]forbiddenEvent
}

func newForbiddenTracker() *forbiddenTracker {
	return &forbiddenTracker{events: make(map[string][]forbiddenEvent)}
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
