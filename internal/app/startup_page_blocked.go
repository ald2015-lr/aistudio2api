package app

import (
	"sync"
	"time"
)

// 页面受阻（page_blocked，见 classifyStartupFailure）的失效确认：页面已经打开之后填不进 bootstrap 提示词、Run 按钮一直不可点，
// 多半是账户本身的问题（官网弹出需要同意的条款或通知，挡住了输入框），但代理慢、官网改版时健康账户也可能这样。所以只有同一账户、
// 同一认证代际下连续 startupPageBlockedConfirmCount 次页面受阻，而且每一次失败时号池都健康，才确认为失效：
//   - 失败前 startupHealthyWindow 内至少有一个其他账户的 Worker 启动成功（只有这一个账户启动不了，不是代理或机器的问题）；
//   - 不处于页面受阻的大面积爆发：startupBurstWindow 内出现页面受阻的不同账户数超过 max(startupBurstMinAccounts,
//     同一时间窗内尝试启动的不同账户数 × startupBurstPercent%) 时，多半是官网改版或灰度，不确认任何新的页面受阻失效
//     （已有记录不动），进入爆发时写一次 WARN，状态接口 startup.page_blocked_burst 为真。
//     已确认为页面受阻失效的账户不计入这两个账户数：它们每 30 分钟重试一次、每次都页面受阻，号池里留着十来个等用户到浏览器里检查的
//     失效账户就会让爆发一直成立（或时有时无），既不再确认新的失效，又提示用户不要删除的恰恰是它们。
//
// 不满足条件的那次失败照常计入暂停（2m、10m、30m 封顶），连续计数既不增加也不清零：它说明不了账户有没有问题。
// 换一类原因的失败、启动成功、认证代际变化时连续计数从头计（与暂停的连续次数一起清除，见 notePausedFailure、clearStartupFailureLocked）。
// 近 5 分钟没有任何 Worker 启动成功时与原因不确定一样按系统性问题处理（只暂停 1m、不计次数）；已确认的账户仍暂停 startupMaxPause
const (
	// startupPageBlockedConfirmCount 为确认页面受阻失效所需的、号池健康时的连续次数
	startupPageBlockedConfirmCount = 3
	// startupHealthyWindow 为判断号池健康时“其他账户启动成功”的时间窗
	startupHealthyWindow = 10 * time.Minute
	// startupBurstWindow 为统计页面受阻爆发的时间窗；startupBurstMinAccounts 与 startupBurstPercent 为爆发的门槛
	startupBurstWindow      = 30 * time.Minute
	startupBurstMinAccounts = 10
	startupBurstPercent     = 20
	// startupRecentLimit 为每个时间窗保留的事件数上限（每次启动尝试或页面受阻一条），超过时丢弃最早的
	startupRecentLimit = 1 << 16
)

// startupSuccessMark 为一次 Worker 启动成功
type startupSuccessMark struct {
	accountID string
	at        time.Time
}

// recentAccountEvent 为时间窗里的一次事件
type recentAccountEvent struct {
	accountID string
	at        time.Time
}

// recentAccountSet 记下时间窗内出现过的不同账户：seen 为每个账户最近一次出现的时间，events 按时间先后排列，过期的从队首去掉
// （只有账户最近一次出现也已过期时才从 seen 删除），统计不同账户数不需要遍历全部账户。由 startupHealthTracker.mu 保护
type recentAccountSet struct {
	seen   map[string]time.Time
	events []recentAccountEvent
}

// note 记下账户在 now 出现一次
func (set *recentAccountSet) note(accountID string, now time.Time, window time.Duration) {
	set.prune(now, window)
	if set.seen == nil {
		set.seen = make(map[string]time.Time)
	}
	if previous, exists := set.seen[accountID]; !exists || now.After(previous) {
		set.seen[accountID] = now
	}
	set.events = append(set.events, recentAccountEvent{accountID: accountID, at: now})
	if extra := len(set.events) - startupRecentLimit; extra > 0 {
		for _, event := range set.events[:extra] {
			set.forget(event)
		}
		set.events = append(set.events[:0], set.events[extra:]...)
	}
}

// count 返回时间窗内出现过的不同账户数
func (set *recentAccountSet) count(now time.Time, window time.Duration) int {
	set.prune(now, window)
	return len(set.seen)
}

// prune 去掉早于时间窗的事件
func (set *recentAccountSet) prune(now time.Time, window time.Duration) {
	cutoff := now.Add(-window)
	index := 0
	for index < len(set.events) && !set.events[index].at.After(cutoff) {
		set.forget(set.events[index])
		index++
	}
	if index > 0 {
		set.events = append(set.events[:0], set.events[index:]...)
	}
}

// forget 在账户最近一次出现不晚于被丢弃的事件时把账户从 seen 删除
func (set *recentAccountSet) forget(event recentAccountEvent) {
	if last, exists := set.seen[event.accountID]; exists && !last.After(event.at) {
		delete(set.seen, event.accountID)
	}
}

// startupHealthTracker 为判断页面受阻时号池是否健康用的近期启动记录；零值可用。mu 是叶子锁，持有时不取其他锁
type startupHealthTracker struct {
	mu sync.Mutex
	// lastSuccess 为最近一次启动成功，otherSuccess 为 lastSuccess 之外的账户最近一次启动成功：
	// 任何账户“最近一次其他账户的启动成功”都是两者之一
	lastSuccess  startupSuccessMark
	otherSuccess startupSuccessMark
	// attempts 与 blocked 为近 startupBurstWindow 尝试启动、页面受阻的账户
	attempts recentAccountSet
	blocked  recentAccountSet
	// burstWarned 为这一轮爆发已经写过 WARN；爆发结束后清除，下一轮再写
	burstWarned bool
}

// pageBlockedVerdict 为一次页面受阻失败时号池的情况
type pageBlockedVerdict struct {
	// otherSuccess 为失败前 startupHealthyWindow 内有其他账户启动成功
	otherSuccess bool
	// burst 为处于页面受阻的大面积爆发，blockedAccounts 与 attemptedAccounts 为近 startupBurstWindow 页面受阻、尝试启动的不同账户数
	burst             bool
	blockedAccounts   int
	attemptedAccounts int
	// warnBurst 为这次失败让号池进入爆发，调用方写一次 WARN
	warnBurst bool
}

// healthy 返回这次失败能否计入失效确认
func (verdict pageBlockedVerdict) healthy() bool {
	return verdict.otherSuccess && !verdict.burst
}

// noteAttempt 记下一次启动尝试（调用启动浏览器之前；已确认为页面受阻失效的账户不记，见文件开头）
func (tracker *startupHealthTracker) noteAttempt(accountID string, now time.Time) {
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	tracker.attempts.note(accountID, now, startupBurstWindow)
}

// noteSuccess 记下一次启动成功
func (tracker *startupHealthTracker) noteSuccess(accountID string, now time.Time) {
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	if tracker.lastSuccess.accountID != "" && tracker.lastSuccess.accountID != accountID {
		tracker.otherSuccess = tracker.lastSuccess
	}
	tracker.lastSuccess = startupSuccessMark{accountID: accountID, at: now}
}

// notePageBlocked 记下账户的一次页面受阻，返回此刻号池的情况。counted 为假（账户已确认为页面受阻失效）时只返回号池的情况，
// 不计入爆发统计（见文件开头）
func (tracker *startupHealthTracker) notePageBlocked(accountID string, counted bool, now time.Time) pageBlockedVerdict {
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	if counted {
		tracker.blocked.note(accountID, now, startupBurstWindow)
	}
	verdict := pageBlockedVerdict{}
	cutoff := now.Add(-startupHealthyWindow)
	for _, mark := range []startupSuccessMark{tracker.lastSuccess, tracker.otherSuccess} {
		if mark.accountID != "" && mark.accountID != accountID && mark.at.After(cutoff) {
			verdict.otherSuccess = true
		}
	}
	verdict.burst, verdict.blockedAccounts, verdict.attemptedAccounts = tracker.burstLocked(now)
	if verdict.burst && !tracker.burstWarned {
		tracker.burstWarned = true
		verdict.warnBurst = true
	}
	return verdict
}

// burst 返回此刻是否处于页面受阻的大面积爆发（状态接口用）
func (tracker *startupHealthTracker) burst(now time.Time) bool {
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	burst, _, _ := tracker.burstLocked(now)
	return burst
}

// burstLocked 按近 startupBurstWindow 页面受阻与尝试启动的不同账户数判断爆发：页面受阻的账户数超过
// max(startupBurstMinAccounts, 尝试启动的账户数 × startupBurstPercent%)。不在爆发时清除 burstWarned
func (tracker *startupHealthTracker) burstLocked(now time.Time) (bool, int, int) {
	blocked := tracker.blocked.count(now, startupBurstWindow)
	attempted := tracker.attempts.count(now, startupBurstWindow)
	burst := blocked > startupBurstMinAccounts && blocked*100 > attempted*startupBurstPercent
	if !burst {
		tracker.burstWarned = false
	}
	return burst, blocked, attempted
}
