package aistudio

import (
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// LoginIdentity 返回认证状态中登录签名 Cookie（SAPISID、__Secure-1PAPISID、__Secure-3PAPISID）的指纹。
// 重新登录或外部程序更新登录 Cookie 后会变；本服务自己回写的轮换 Cookie（如 __Secure-1PSIDTS）不影响它，
// 与 Cookie 是否过期也无关
func LoginIdentity(state StorageState) string {
	values := make([]string, 0, len(signatureCookies))
	for _, cookie := range state.Cookies {
		if cookie.Value == "" {
			continue
		}
		for _, item := range signatureCookies {
			if cookie.Name == item.name {
				values = append(values, cookie.Name+"\x00"+cookie.Domain+"\x00"+cookie.Value)
			}
		}
	}
	if len(values) == 0 {
		return ""
	}
	sort.Strings(values)
	sum := sha256.Sum256([]byte(strings.Join(values, "\x01")))
	return hex.EncodeToString(sum[:8])
}

// StorageStatePath 返回账户目录下认证文件的路径
func StorageStatePath(directory string) string {
	return filepath.Join(directory, storageStateName)
}

// externalLoginChanges 记录回写 Cookie 时发现"磁盘上的登录状态已被外部更新"的账户，由账户目录扫描载入
var externalLoginChanges sync.Map

// TakeExternalLoginChanges 返回并清空这些账户
func TakeExternalLoginChanges() []string {
	var ids []string
	externalLoginChanges.Range(func(key, _ any) bool {
		externalLoginChanges.Delete(key)
		if id, ok := key.(string); ok {
			ids = append(ids, id)
		}
		return true
	})
	return ids
}

// AccountLoginSummary 为账户目录扫描需要的轻量状态；不统计模型目录，开销远小于 Status
type AccountLoginSummary struct {
	ID          string
	Enabled     bool
	State       AccountState
	StoragePath string
}

// LoginSummaries 一次加锁返回全部账户的轻量状态
func (p *AccountPool) LoginSummaries() []AccountLoginSummary {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	summaries := make([]AccountLoginSummary, 0, len(p.accounts))
	for _, account := range p.accounts {
		if account == nil {
			continue
		}
		summaries = append(summaries, AccountLoginSummary{
			ID: account.ID, Enabled: account.Config.Enabled, State: accountStateLocked(account, now),
			StoragePath: account.StoragePath,
		})
	}
	return summaries
}

// LoginIdentity 返回账户内存中登录状态的指纹；只在认证文件发生变化时调用
func (p *AccountPool) LoginIdentity(accountID string) (string, bool) {
	p.mu.Lock()
	account := p.byID[accountID]
	var state StorageState
	if account != nil {
		state = account.StorageState
	}
	p.mu.Unlock()
	if account == nil {
		return "", false
	}
	return LoginIdentity(state), true
}
