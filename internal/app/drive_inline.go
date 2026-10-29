package app

import (
	"context"
	"sync"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
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

// driveAuthorizedNoter 为能记下账号 Drive 可用的生成服务（aistudio.PooledService）：经本服务的登录流程登录、完成官网 Drive 授权后调用
type driveAuthorizedNoter interface {
	NoteDriveAuthorized(string)
}

// driveCandidateSource 为能报告候选账号 Drive 授权状态的生成服务（aistudio.PooledService）：Interaction 模型带内联媒体的生成选号
// 先试记着 Drive 可用的账号、排除记着不可用的账号（见 generateWithRetry）；没有实现时不按 Drive 状态选号，只在发送前发现没有 Drive 授权时换号
type driveCandidateSource interface {
	DriveCandidates(context.Context, aistudio.AccountSelection) aistudio.DriveCandidates
}

// driveSummarySource 为能统计启用账号 Drive 授权状态的生成服务（aistudio.PooledService），供 /api/status 与管理页显示
type driveSummarySource interface {
	DriveSummary() aistudio.DriveCounts
}

// inlineMediaUploader 为能把内联附件上传到租约账号 Drive 的生成服务（aistudio.PooledService，generateWithRetry 按匿名接口断言）
type inlineMediaUploader interface {
	UploadInlineMediaToLease(context.Context, *aistudio.AccountLease, []aistudio.Content, *aistudio.TemporaryFileCopies) ([]aistudio.Content, *aistudio.TemporaryFileCopies, error)
}

// 生产路径靠类型断言接上 Drive 选号、状态计数与附件上传：断言失败时静默跳过（Drive 选号整体失效、/api/status 不再返回 drive），
// 测试只用假实现，这里在编译期确认 PooledService 满足这些接口，签名改了编译不过
var (
	_ driveCandidateSource      = (*aistudio.PooledService)(nil)
	_ driveSummarySource        = (*aistudio.PooledService)(nil)
	_ inlineMediaUploader       = (*aistudio.PooledService)(nil)
	_ driveUnavailableForgetter = (*aistudio.PooledService)(nil)
	_ driveAuthorizedNoter      = (*aistudio.PooledService)(nil)
)
