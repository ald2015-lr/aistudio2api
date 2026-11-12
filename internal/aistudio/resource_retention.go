package aistudio

import (
	"context"
	"sort"
	"time"
)

// 本地资源绑定的保留期。资源绑定（runtime-state.json 的 resources）把上游资源 ID 固定到创建它的账户，供之后的续接、轮询与下载
// 选到同一个账户；Live 续接凭证与视频任务的绑定原先永久保留，而账户的 runtime-state.json 每次写冷却都整个重写。
// 这里只清本地绑定，不删 Drive 上的文件；上传文件（drive-file）由客户端自己删除，不按时间清理
const (
	// videoBindingRetention 为生成视频任务（video-operation）与结果文件（video-file）的绑定自登记起的保留期：
	// 官方 Veo 文档写明生成的视频在服务器上只保存 2 天，过期后上游已取不到；多留 1 天余量，覆盖任务从登记到生成完成的
	// 几分钟、结果文件晚于完成才被轮询登记与时钟偏差，官方仍可下载期间本地绑定一定还在
	videoBindingRetention = 72 * time.Hour
	// bidiSessionRetention 为 Live 续接凭证（bidi-session）的绑定自发放起的保留期（每次发放新凭证换成新绑定）：
	// 官方 Live API 的续接凭证只在会话结束后的短时间内（文档写的是 2 小时）可用，24 小时与上游 v0.3.5 相同，余量足够
	bidiSessionRetention = 24 * time.Hour
)

// resourceBindingExpired 判断绑定在 now 时是否已过保留期；没有保留期的类型（上传文件、未标类型）与登记时间缺失的不过期
func resourceBindingExpired(binding ResourceBinding, now time.Time) bool {
	if binding.CreatedAt.IsZero() {
		return false
	}
	var retention time.Duration
	switch binding.Kind {
	case "video-operation", "video-file":
		retention = videoBindingRetention
	case "bidi-session":
		retention = bidiSessionRetention
	default:
		return false
	}
	return !now.Before(binding.CreatedAt.Add(retention))
}

// ResourceReclaimResult 为一次到期绑定清理的结果
type ResourceReclaimResult struct {
	// Accounts 为清理了绑定的账户数，Bindings 为清理的绑定数，ByKind 按类型计数
	Accounts int
	Bindings int
	ByKind   map[string]int
	// Busy 为有到期绑定、但账户正在做独占操作或认证写回（见 resourceReclaimDeferredLocked）而推到下一轮的账户数
	Busy int
	// Remaining 为本轮达到上限后还没处理的账户数；Next 为本轮最后处理的账户，下一轮从它之后开始（失败的账户不挡后面的）
	Remaining int
	Next      string
	// Failed 为写回失败的账户数，Err 为其中第一个错误
	Failed int
	Err    error
}

// resourceReclaimDeferredLocked 判断账户的到期绑定是否推到下一轮：账户正持有独占租约（登录、重新导入、删除、关闭 Worker）或在做
// 认证续签写回时不清，其余情况（包括正有普通请求、正有人排队等独占）照常清理。调用方持有账户池 mu。
// 普通请求不挡清理：能清的绑定都已过了官方有效期（视频 2 天、Live 续接凭证 2 小时，见上面的保留期），上游已取不到，不会再被续接、
// 轮询或下载；已经选定账户的请求不再查绑定。原先只要账户有租约就整账户跳过，持续有请求的高负载账户（Ultra 常驻、Live 会话多的）
// 每 10 分钟采样一次几乎总在忙，绑定恰恰在最需要清的账户上一直留着。清理经 updateRuntimeContext 在 runtime-state.json 的
// 短事务锁内按磁盘上的最新状态改写，与请求路径的写回（冷却、新绑定）互不覆盖
func resourceReclaimDeferredLocked(account *Account) bool {
	return account.exclusive || account.authRefreshers > 0
}

// ReclaimExpiredResourceBindings 清理过了保留期的本地资源绑定（Live 续接凭证、视频任务与结果文件，见 resourceBindingExpired），
// 不删 Drive 上的文件。每个账户只在有到期绑定时写一次 runtime-state.json；账户正在做独占操作或认证写回时跳过（下一轮再清，
// 见 resourceReclaimDeferredLocked），写回前按磁盘上的最新状态在账户池锁内再判断一次。每轮最多处理 limit 个账户（不大于 0 时不限），
// 按账户 ID 从 after 之后开始、到末尾后回到开头
func (p *AccountPool) ReclaimExpiredResourceBindings(ctx context.Context, now time.Time, limit int, after string) ResourceReclaimResult {
	result := ResourceReclaimResult{ByKind: map[string]int{}}
	p.mu.Lock()
	candidates := make([]string, 0)
	for _, account := range p.accounts {
		if len(account.runtime.Resources) == 0 {
			continue
		}
		expired := false
		for _, binding := range account.runtime.Resources {
			if resourceBindingExpired(binding, now) {
				expired = true
				break
			}
		}
		if !expired {
			continue
		}
		if resourceReclaimDeferredLocked(account) {
			result.Busy++
			continue
		}
		candidates = append(candidates, account.ID)
	}
	p.mu.Unlock()
	sort.Strings(candidates)
	start := sort.SearchStrings(candidates, after)
	if start < len(candidates) && candidates[start] == after {
		start++
	}
	ordered := append(candidates[start:len(candidates):len(candidates)], candidates[:start]...)
accounts:
	for index, accountID := range ordered {
		if limit > 0 && index >= limit {
			result.Remaining = len(ordered) - index
			break
		}
		if ctx.Err() != nil {
			result.Remaining = len(ordered) - index
			break
		}
		result.Next = accountID
		removed := map[string]int{}
		busy := false
		_, err := p.updateRuntimeContext(ctx, accountID, func(account *Account, runtimeState *accountRuntimeState) (bool, func(*Account), error) {
			// 选出之后账户可能开始了独占操作或认证写回：账户池锁内再看一次
			if resourceReclaimDeferredLocked(account) {
				busy = true
				return false, nil, nil
			}
			for resourceID, binding := range runtimeState.Resources {
				if resourceBindingExpired(binding, now) {
					delete(runtimeState.Resources, resourceID)
					removed[binding.Kind]++
				}
			}
			return len(removed) > 0, nil, nil
		})
		switch {
		case err != nil && ctx.Err() != nil:
			// 服务停止：这个账户没有清完，算作未处理
			result.Remaining = len(ordered) - index
			break accounts
		case err != nil:
			result.Failed++
			if result.Err == nil {
				result.Err = err
			}
		case busy:
			result.Busy++
		case len(removed) > 0:
			result.Accounts++
			for kind, count := range removed {
				result.ByKind[kind] += count
				result.Bindings += count
			}
		}
	}
	return result
}
