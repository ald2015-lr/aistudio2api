package aistudio

import (
	"context"
	"errors"
	"sync"
	"time"
)

// driveUnavailableTTL 为“Drive 不可用”记忆的有效期：期间这个账号的内联附件直接内联发送，不再先换 Drive token
const driveUnavailableTTL = 6 * time.Hour

// driveMemoryPruneMin 为记忆条目达到多少时才清理过期与失效的条目（之后按清理后条目数的两倍再清，摊销到每次写入是常数）
const driveMemoryPruneMin = 1024

// driveUnavailableEntry 为一个账号的“Drive 不可用”记录
type driveUnavailableEntry struct {
	// account 为记下时的账户对象：账户删除后以同一 ID 重新添加是另一个对象（认证代际从 0 重新计），不沿用旧记录
	account *Account
	// generation 为发出那次 GenerateAccessToken 之前账户的认证代际：重新登录、续签、更新认证文件推进代际后记录失效
	generation uint64
	until      time.Time
}

// driveMemory 按账号记住 GenerateAccessToken 返回 401 / 协议 Code 16（账号没有授权 Google Drive，导入的账号多半没走过官网的 Drive 同意）：
// 只在内存，不落盘，重启或重建运行时后重新探测。用自己的小锁，不在持有时取账户池的 mu（认证代际在取这把锁之前读好），
// 不与账户池主锁形成新的锁顺序。零值可用
type driveMemory struct {
	mu      sync.Mutex
	entries map[string]driveUnavailableEntry
	pruneAt int
}

// unavailable 判断账号此刻是否记着 Drive 不可用：账户对象、认证代际都与记下时相同且没有过期；对不上的记录顺手删掉
func (memory *driveMemory) unavailable(account *Account, generation uint64, now time.Time) bool {
	if account == nil {
		return false
	}
	memory.mu.Lock()
	defer memory.mu.Unlock()
	entry, exists := memory.entries[account.ID]
	if !exists {
		return false
	}
	if entry.account != account || entry.generation != generation || !now.Before(entry.until) {
		delete(memory.entries, account.ID)
		return false
	}
	return true
}

// remember 记下账号 Drive 不可用（generation 为发出 GenerateAccessToken 之前读取的认证代际），有效期 driveUnavailableTTL
func (memory *driveMemory) remember(account *Account, generation uint64, now time.Time) {
	if account == nil {
		return
	}
	memory.mu.Lock()
	defer memory.mu.Unlock()
	if memory.entries == nil {
		memory.entries = make(map[string]driveUnavailableEntry)
	}
	if len(memory.entries) >= max(memory.pruneAt, driveMemoryPruneMin) {
		for accountID, entry := range memory.entries {
			if !now.Before(entry.until) {
				delete(memory.entries, accountID)
			}
		}
		memory.pruneAt = 2 * len(memory.entries)
	}
	memory.entries[account.ID] = driveUnavailableEntry{account: account, generation: generation, until: now.Add(driveUnavailableTTL)}
}

// forget 清除账号的记录（GenerateAccessToken 成功，或管理验证通过后重新探测）
func (memory *driveMemory) forget(accountID string) {
	memory.mu.Lock()
	defer memory.mu.Unlock()
	delete(memory.entries, accountID)
}

// count 返回记忆中的条目数（含尚未清理的过期条目），只供测试
func (memory *driveMemory) count() int {
	memory.mu.Lock()
	defer memory.mu.Unlock()
	return len(memory.entries)
}

// driveUnavailableError 判断 GenerateAccessToken 的错误是否说明这个账号的 Drive 不可用：401 / 协议 Code 16，
// 含明确的 unauthorized_client 与其他 401（线上原文 “Request is missing required authentication credential”）。
// 这些账号用 Cookie 认证的 RPC 与生成都正常，401 不是 Cookie 过期，而是没有 Drive 授权（登录流程里 camoufoxnative 的 authorizeDrive
// 同样把 GenerateAccessToken 的任何 401 当作 Drive 未授权）。登录失效（ErrAuthenticationRequired：签名 Cookie 缺失、登录页跳转）不算
func driveUnavailableError(err error) bool {
	if err == nil || errors.Is(err, ErrAuthenticationRequired) {
		return false
	}
	return AccessTokenAuthFailure(err) || driveAuthorizationMissing(err)
}

// driveAuthGeneration 返回租约账户当前的认证代际，在发出 GenerateAccessToken 之前读取（只取账户池 mu，与 driveMemory 的锁不嵌套）；
// 账户已不在池中时 ok 为假，这次结果不记
func (s *PooledService) driveAuthGeneration(lease *AccountLease) (uint64, bool) {
	if s == nil || s.pool == nil || lease == nil || lease.account == nil {
		return 0, false
	}
	p := s.pool
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.byID[lease.account.ID] != lease.account {
		return 0, false
	}
	return lease.account.authGeneration, true
}

// driveKnownUnavailable 判断租约账户是否记着 Drive 不可用（认证代际变化或过期后不算）
func (s *PooledService) driveKnownUnavailable(lease *AccountLease) bool {
	generation, current := s.driveAuthGeneration(lease)
	return current && s.drive.unavailable(lease.account, generation, time.Now())
}

// noteAccessToken 按一次 GenerateAccessToken 的结果更新记忆：取得 token 时清除；返回 401 / Code 16（driveUnavailableError）时
// 按发出前读取的认证代际记下。其他错误（网络、5xx、登录失效）不改记忆
func (s *PooledService) noteAccessToken(lease *AccountLease, generation uint64, current bool, tokenOK bool, err error) {
	if s == nil || lease == nil || lease.account == nil {
		return
	}
	switch {
	case tokenOK:
		s.drive.forget(lease.account.ID)
	case current && driveUnavailableError(err):
		s.drive.remember(lease.account, generation, time.Now())
	}
}

// accessTokenForLease 为租约账户换一次 Drive bearer token，并按结果更新“Drive 不可用”记忆
func (s *PooledService) accessTokenForLease(ctx context.Context, lease *AccountLease) (string, error) {
	generation, current := s.driveAuthGeneration(lease)
	token, err := s.client.GenerateAccessToken(ContextWithAccountLease(ctx, lease), lease.Account().ID)
	s.noteAccessToken(lease, generation, current, err == nil, err)
	return token, err
}

// uploadFileForLease 用租约账户上传 Drive 文件（request.AccountID 须为租约账户），并按其中 GenerateAccessToken 的结果更新记忆：
// 返回了 token 即换取成功（之后的 Drive 上传失败与 Drive 授权无关）
func (s *PooledService) uploadFileForLease(ctx context.Context, lease *AccountLease, request UploadRequest) (FileRef, string, error) {
	generation, current := s.driveAuthGeneration(lease)
	file, token, err := s.client.uploadFile(ContextWithAccountLease(ctx, lease), request)
	s.noteAccessToken(lease, generation, current, token != "", err)
	return file, token, err
}

// ForgetDriveUnavailable 清除账号的“Drive 不可用”记忆：管理验证通过等人工恢复路径调用，下一次内联附件重新探测 Drive 授权
// （重新登录与更新认证文件推进认证代际，记录本来就会失效）
func (s *PooledService) ForgetDriveUnavailable(accountID string) {
	if s == nil {
		return
	}
	s.drive.forget(accountID)
}

// preferDriveCandidates 把记着 Drive 不可用的账户移到候选末尾（其余保持原顺序）：自动选号的文件上传先试可能有 Drive 授权的账户，
// 记着不可用的账户仍留在候选里（记录可能已过时）。先在账户池 mu 下读好各账户的对象与认证代际，再取记忆的锁
func (s *PooledService) preferDriveCandidates(accountIDs []string) []string {
	if s == nil || s.pool == nil || len(accountIDs) < 2 || s.drive.count() == 0 {
		return accountIDs
	}
	type candidate struct {
		account    *Account
		generation uint64
	}
	snapshot := make([]candidate, len(accountIDs))
	s.pool.mu.Lock()
	for index, accountID := range accountIDs {
		if account := s.pool.byID[accountID]; account != nil {
			snapshot[index] = candidate{account: account, generation: account.authGeneration}
		}
	}
	s.pool.mu.Unlock()
	now := time.Now()
	preferred := make([]string, 0, len(accountIDs))
	var deferred []string
	for index, accountID := range accountIDs {
		if snapshot[index].account != nil && s.drive.unavailable(snapshot[index].account, snapshot[index].generation, now) {
			deferred = append(deferred, accountID)
			continue
		}
		preferred = append(preferred, accountID)
	}
	return append(preferred, deferred...)
}
