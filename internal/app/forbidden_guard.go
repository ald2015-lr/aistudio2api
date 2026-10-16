package app

import (
	"errors"
	"net/http"
	"strings"
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
	// forbiddenModelFirstCooldown、forbiddenModelRepeatCooldown 为单模型 403 冷却的递增时长：同一账号同一通道同一模型第一次 403
	// 冷却 30 分钟，forbiddenModelWindow（24 小时）内第二次 6 小时，第三次起到太平洋时间次日零点（额度日切换），至少 6 小时；
	// 该通道上该模型成功一次即从第一档重新计
	forbiddenModelFirstCooldown  = 30 * time.Minute
	forbiddenModelRepeatCooldown = 6 * time.Hour
	forbiddenModelWindow         = 24 * time.Hour
	// forbiddenSystemicAccounts：forbiddenWindow 内同一号池同一模型有这么多个不同账号返回 403、且这段时间该模型在号池里没有任何成功时，
	// 视为号池级 403（纯 Go proof、动态头、请求形态或 WAA binding 失效等，见 docs/waa.md），不是账号没有权限：不写单模型冷却、
	// 不升档、不登记淘汰，按一般失败处理（计入连续失败、退避），免得修复上线后号池仍按递增档位冷却到数小时后（冷却会持久化）
	forbiddenSystemicAccounts = 5
	// forbiddenModelReasonPrefix 与 forbiddenPauseReasonSuffix 为单模型 403 冷却与多模型连续 403 自动暂停写入的冷却原因的固定部分：
	// 认证文件更新后按它们找出 403 无权限的冷却并清除（见 forbiddenCooldown）
	forbiddenModelReasonPrefix = "无权限（模型）: "
	forbiddenPauseReasonSuffix = "次返回 403 无权限，自动暂停"
)

type forbiddenEvent struct {
	at    time.Time
	model string
}

// forbiddenModelKey 为单模型 403 冷却档位的键：账号与冷却作用域（通道与模型，见 aistudio.ChannelCooldownScope）。
// 冷却按通道写入，档位也按通道分开计：两个通道共用一份档位时，一个通道冷却到期后再 403 总落在另一通道刚续上的到期时间之内，
// 两个通道的 403 错开时档位永远升不上去
type forbiddenModelKey struct {
	accountID string
	scope     string
}

// forbiddenModelStrike 为同一账号同一冷却作用域的 403 记录：level 为冷却档位（1 起），last 为最近一次 403 的时间，until 为本档冷却的到期时间
type forbiddenModelStrike struct {
	level int
	last  time.Time
	until time.Time
}

// forbiddenScopeKey 为号池级 403 判定的键：号池与模型
type forbiddenScopeKey struct {
	pool    aistudio.PoolScope
	modelID string
}

// forbiddenDenial 为号池内一个账号对该模型最近一次返回 403 的时间
type forbiddenDenial struct {
	at        time.Time
	accountID string
}

// forbiddenScopeState 记录号池内该模型最近返回 403 的不同账号（最多 forbiddenSystemicAccounts 个）与最近一次成功的时间
type forbiddenScopeState struct {
	denied      []forbiddenDenial
	lastSuccess time.Time
}

// forbiddenTracker 按账号记录连续的 403 无权限错误（任意一次成功即清零，多个模型连续 403 时暂停账号），
// 按账号与冷却作用域记录单模型 403 的冷却档位（该通道上该模型成功一次即清零），并按号池与模型判断号池级 403
type forbiddenTracker struct {
	mu     sync.Mutex
	events map[string][]forbiddenEvent
	models map[forbiddenModelKey]forbiddenModelStrike
	scopes map[forbiddenScopeKey]forbiddenScopeState
}

func newForbiddenTracker() *forbiddenTracker {
	return &forbiddenTracker{
		events: make(map[string][]forbiddenEvent),
		models: make(map[forbiddenModelKey]forbiddenModelStrike),
		scopes: make(map[forbiddenScopeKey]forbiddenScopeState),
	}
}

// observeModelDenied 记录号池内一次单模型 403，返回是否为号池级 403：forbiddenWindow 内返回 403 的不同账号达到
// forbiddenSystemicAccounts 个，且这段时间该模型在号池里没有成功。只保留每个账号最近的一次、最多 forbiddenSystemicAccounts 个
func (tracker *forbiddenTracker) observeModelDenied(pool aistudio.PoolScope, accountID string, modelID string, now time.Time) bool {
	if tracker == nil {
		return false
	}
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	if tracker.scopes == nil {
		tracker.scopes = make(map[forbiddenScopeKey]forbiddenScopeState)
	}
	key := forbiddenScopeKey{pool: pool, modelID: modelID}
	state := tracker.scopes[key]
	kept := make([]forbiddenDenial, 0, forbiddenSystemicAccounts)
	for _, denial := range state.denied {
		if denial.accountID != accountID && now.Sub(denial.at) <= forbiddenWindow {
			kept = append(kept, denial)
		}
	}
	kept = append(kept, forbiddenDenial{at: now, accountID: accountID})
	if len(kept) > forbiddenSystemicAccounts {
		kept = kept[len(kept)-forbiddenSystemicAccounts:]
	}
	state.denied = kept
	tracker.scopes[key] = state
	return len(kept) >= forbiddenSystemicAccounts && now.Sub(state.lastSuccess) > forbiddenWindow
}

// modelSucceeded 号池内该模型请求成功：清空号池级 403 的记录并记下成功时间
func (tracker *forbiddenTracker) modelSucceeded(pool aistudio.PoolScope, modelID string, now time.Time) {
	if tracker == nil {
		return
	}
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	if tracker.scopes == nil {
		tracker.scopes = make(map[forbiddenScopeKey]forbiddenScopeState)
	}
	tracker.scopes[forbiddenScopeKey{pool: pool, modelID: modelID}] = forbiddenScopeState{lastSuccess: now}
}

// recordModel 记录一次单模型 403（账号对该模型没有权限，常见于免费账号调用付费模型），返回本次冷却的到期时间与档位。
// scope 为冷却作用域（lease.CooldownScope(modelID)），两个通道分别计档。同一通道本档冷却还没到期时再次 403（冷却写入前已经
// 发出的请求）不升档、只从现在起重新计；距上一次 403 不超过 24 小时升一档，否则从第一档重新计。
// 顺带清理超过 24 小时且冷却已到期的记录
func (tracker *forbiddenTracker) recordModel(accountID string, scope string, now time.Time) (time.Time, int) {
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
	key := forbiddenModelKey{accountID: accountID, scope: scope}
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

// resetModel 账号在该通道上对该模型请求成功后清除这个作用域的冷却档位（scope 同 recordModel）
func (tracker *forbiddenTracker) resetModel(accountID string, scope string) {
	if tracker == nil {
		return
	}
	tracker.mu.Lock()
	delete(tracker.models, forbiddenModelKey{accountID: accountID, scope: scope})
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

// deniedAccounts 返回号池内最近对该模型返回 403 的账号（号池级 403 判定窗口内的记录）
func (tracker *forbiddenTracker) deniedAccounts(pool aistudio.PoolScope, modelID string) []string {
	if tracker == nil {
		return nil
	}
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	state := tracker.scopes[forbiddenScopeKey{pool: pool, modelID: modelID}]
	accountIDs := make([]string, 0, len(state.denied))
	for _, denial := range state.denied {
		accountIDs = append(accountIDs, denial.accountID)
	}
	return accountIDs
}

// forgetAccount 认证文件更新后清除账号的连续 403 计数与全部单模型 403 冷却档位：新的登录状态再 403 时从第一档重新计
func (tracker *forbiddenTracker) forgetAccount(accountID string) {
	if tracker == nil {
		return
	}
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	delete(tracker.events, accountID)
	for key := range tracker.models {
		if key.accountID == accountID {
			delete(tracker.models, key)
		}
	}
}

// forbiddenCooldown 判断冷却是否由 403 无权限写入：单模型 403 冷却，或多模型连续 403 的自动暂停
func forbiddenCooldown(_ string, cooldown aistudio.CooldownState) bool {
	return strings.HasPrefix(cooldown.Reason, forbiddenModelReasonPrefix) ||
		strings.HasSuffix(cooldown.Reason, forbiddenPauseReasonSuffix)
}

// isPermissionDenied 判断上游是否返回 HTTP 403 无权限（协议错误码 7）
func isPermissionDenied(err error) bool {
	var rpcError *aistudio.RPCError
	if !errors.As(err, &rpcError) {
		return false
	}
	return rpcError.StatusCode == http.StatusForbidden && (rpcError.Code == 7 || rpcError.Code == 0)
}
