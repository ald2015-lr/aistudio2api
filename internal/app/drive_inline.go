package app

import (
	"sync"
	"time"
)

// driveInlineNoteInterval 为同一账号“Drive 未授权，附件改为内联”日志的最短间隔：没有 Drive 授权的账号每个带图请求都走内联，
// 逐个请求写会刷屏
const driveInlineNoteInterval = 10 * time.Minute

// driveInlineNotePrune 为去重记录数达到多少时才清理过期的记录（之后按清理后记录数的两倍再清，摊销到每次判断是常数）
const driveInlineNotePrune = 1024

// driveInlineNotes 为“Drive 未授权，附件改为内联”日志的去重状态：每个账号最近一次写日志的时间；零值可用
type driveInlineNotes struct {
	mu      sync.Mutex
	logged  map[string]time.Time
	pruneAt int
}

// allow 判断账号现在能否写一条，能写时记下时间：距上一次不足 driveInlineNoteInterval 时不能写
func (notes *driveInlineNotes) allow(accountID string, now time.Time) bool {
	notes.mu.Lock()
	defer notes.mu.Unlock()
	if last, seen := notes.logged[accountID]; seen && now.Sub(last) < driveInlineNoteInterval {
		return false
	}
	if notes.logged == nil {
		notes.logged = make(map[string]time.Time)
	}
	if len(notes.logged) >= max(notes.pruneAt, driveInlineNotePrune) {
		for other, at := range notes.logged {
			if now.Sub(at) >= driveInlineNoteInterval {
				delete(notes.logged, other)
			}
		}
		notes.pruneAt = 2 * len(notes.logged)
	}
	notes.logged[accountID] = now
	return true
}

// driveUnavailableForgetter 为能清除账号“Drive 不可用”记忆的生成服务（aistudio.PooledService）
type driveUnavailableForgetter interface {
	ForgetDriveUnavailable(string)
}
