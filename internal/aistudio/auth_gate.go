package aistudio

import (
	"errors"
	"strings"
	"time"
)

// 租约路径上的登录失效：上游对 Cookie 认证的 RPC 返回 HTTP 401 / Code 16 时，原先一次就把账户标为需要登录，状态只在内存、
// 被标记的账户拿不到租约，运行期间永远不会恢复（第 5 项：需要登录从 334 涨到 2041）。401 本身可能来自 Go 通道的过期磁盘 Cookie
// 或 Google 一侧的短暂拒绝，单次证据不足以判死。账户池挂上 AuthFailureGate（app 层的“疑似登录失效”复核，见 app/auth_recheck.go）后，
// 上游 401 先交给它：它写短冷却并异步用磁盘 Cookie 复核，复核仍是 401 才标需要登录。登录页跳转、签名 Cookie 缺失等本地证据仍直接标记

// AuthFailureGate 决定租约确认的一次上游 401 是否立即把账户标为需要登录：返回 true 时照常标记；返回 false 时由 gate 自己处理
// （写“疑似登录失效”短冷却并排队复核），账户状态不变。gate 在账户池锁外调用，可以读写账户池
type AuthFailureGate func(lease *AccountLease, cause error) bool

// SetAuthFailureGate 挂上（gate 为 nil 时撤下）上游 401 的处理决定
func (p *AccountPool) SetAuthFailureGate(gate AuthFailureGate) {
	if p == nil {
		return
	}
	if gate == nil {
		p.authGate.Store(nil)
		return
	}
	p.authGate.Store(&gate)
}

// upstreamAuthFailure 判断登录失效是否只来自上游 RPC 的 401 / Code 16（没有登录页跳转、签名 Cookie 缺失这类本地证据）
func upstreamAuthFailure(cause error) bool {
	if errors.Is(cause, ErrAuthenticationRequired) {
		return false
	}
	var rpcError *RPCError
	return errors.As(cause, &rpcError)
}

// MarkAuthenticationFailure 处理租约路径确认的一次登录失效（DefinitiveAuthenticationFailure 为真的错误），返回账户是否已被标为需要登录：
// 上游 RPC 的 401 先交给 AuthFailureGate（返回 false 时不改账户状态）；本地证据、没有挂 gate 时直接标记。
// 认证代际已变化（期间保存过新登录）或已有更晚的认证结果时不写入，返回 false
func (l *AccountLease) MarkAuthenticationFailure(cause error) (bool, error) {
	if l == nil || l.account == nil || l.pool == nil {
		return false, errors.New("账户租约未初始化")
	}
	if cause == nil {
		return false, nil
	}
	return l.markAuthenticationFailureAt(cause, l.CheckedAt())
}

// markAuthenticationFailureAt 与 MarkAuthenticationFailure 相同，按长连接中指定轮次的顺序时间写回
func (l *AccountLease) markAuthenticationFailureAt(cause error, checkedAt time.Time) (bool, error) {
	if gate := l.pool.authGate.Load(); gate != nil && upstreamAuthFailure(cause) && !(*gate)(l, cause) {
		return false, nil
	}
	return l.markAuthenticationRequiredWritten(strings.TrimSpace(cause.Error()), checkedAt)
}

// markAuthenticationRequiredWritten 与 MarkAuthenticationRequired 相同，另外返回是否真正写入
func (l *AccountLease) markAuthenticationRequiredWritten(reason string, checkedAt time.Time) (bool, error) {
	l.operation.Lock()
	authGeneration := l.authGeneration
	l.operation.Unlock()
	l.pool.mu.Lock()
	defer l.pool.mu.Unlock()
	return l.pool.markAuthenticationLocked(l.account, authGeneration, true, reason, checkedAt), nil
}

// AuthRequiredAccountIDs 返回启用、需要登录的账户（按账户池顺序），一次加锁：登录复核每轮只取它们，不构建全部账户的状态
func (p *AccountPool) AuthRequiredAccountIDs() []string {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	var accountIDs []string
	for _, account := range p.accounts {
		if account != nil && account.Config.Enabled && account.State == AccountAuthRequired {
			accountIDs = append(accountIDs, account.ID)
		}
	}
	return accountIDs
}
