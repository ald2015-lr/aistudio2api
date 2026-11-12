package aistudio

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/gofrs/flock"
)

// ErrAccountExists 表示同一邮箱的账户目录已经存在（浏览器添加账户时）：已有账户要用重新登录，Chrome 导入的账户可以就地重新导入
var ErrAccountExists = errors.New("账户已存在")

// AccountTransition 为管理页启停账户之后的后台收尾阶段（见 app 的 setAccountEnabled），空字符串表示没有收尾
type AccountTransition string

const (
	// AccountDraining 为账户已停用，正在等进行中的请求与账户上的其他操作（登录、验证、目录同步、Worker 启动等）结束，之后关闭 Worker
	AccountDraining AccountTransition = "draining"
	// AccountClosing 为账户已停用，正在关闭 Worker：停用收尾取得独占租约、开始关闭时设上；账户状态里 draining 而已没有要等的请求与
	// 独占操作时也按它显示（见 accountStatusLocked）
	AccountClosing AccountTransition = "closing"
	// AccountSyncing 为账户已启用，正在合并模型目录（没有目录时同步一次）
	AccountSyncing AccountTransition = "syncing"
)

// accountDirectoryExists 判断以邮箱命名的账户目录是否已经是一个账户（有 account.json 或认证文件）：
// 只有空目录或没有账户文件的残留目录不算，照旧由改名覆盖或报错
func accountDirectoryExists(directory string) bool {
	return fileExists(filepath.Join(directory, accountConfigName)) || fileExists(filepath.Join(directory, storageStateName))
}

// SetEnabled 不取租约，立即把账户的启用状态写回 account.json 并在账户池生效（管理页启停账户，见 app 的 setAccountEnabled）：
// 停用后不再发出新的生成租约，进行中的请求照常结束；启用后按登录 Cookie 推导状态（与 SaveConfig 相同）。
// 只改 Enabled 一个字段（代理、语言、时区不变），与 SaveConfig 一样持有 storageMu，两者互斥。
// 写 account.json 要持有账户文件锁（跨进程，与租约同一把）：账户有租约时本进程已经持有；空闲时临时取得、写完归还，
// 另一个进程（或仍在运行的上一代生成服务）持有时不写、返回 ErrAccountLeased。账户目录已被删除时返回 ErrAccountNotFound，
// 不会因写文件重新创建目录。返回账户与是否有变化（已经是目标状态时不写文件）
func (p *AccountPool) SetEnabled(accountID string, enabled bool) (*Account, bool, error) {
	if p == nil {
		return nil, false, ErrAccountNotFound
	}
	accountID = strings.TrimSpace(accountID)
	p.mu.Lock()
	account := p.byID[accountID]
	p.mu.Unlock()
	if account == nil {
		return nil, false, fmt.Errorf("%w: %s", ErrAccountNotFound, accountID)
	}
	account.storageMu.Lock()
	defer account.storageMu.Unlock()
	p.mu.Lock()
	if p.byID[accountID] != account {
		p.mu.Unlock()
		return nil, false, fmt.Errorf("%w: %s", ErrAccountNotFound, accountID)
	}
	value := account.Config
	if value.Enabled == enabled {
		p.mu.Unlock()
		return account, false, nil
	}
	value.Enabled = enabled
	var fileLock *flock.Flock
	if account.ConfigPath != "" && account.leaseLock == nil {
		lock, _, err := acquireAccountFileLease(account.StoragePath)
		if errors.Is(err, errAccountLeaseBusy) {
			p.mu.Unlock()
			return nil, false, fmt.Errorf("%w: %s", ErrAccountLeased, accountID)
		}
		if err != nil {
			p.mu.Unlock()
			return nil, false, err
		}
		fileLock = lock
	}
	p.mu.Unlock()
	var err error
	if account.ConfigPath != "" {
		err = validatePersistentAccountFiles(account)
		if err == nil {
			err = writeAccountConfig(account.ConfigPath, value)
		}
	}
	if fileLock != nil {
		err = errors.Join(err, fileLock.Unlock())
	}
	if err != nil {
		return nil, false, err
	}
	p.mu.Lock()
	wasDisabled := account.State == AccountDisabled
	// 只写 Enabled 字段：持有租约的路径（日志、Worker 配置）会不加 p.mu 读取 Config 的其他字段
	account.Config.Enabled = enabled
	if !enabled {
		account.State = AccountDisabled
		account.stateMessage = ""
	} else if wasDisabled {
		account.State = initialAccountState(account.Config, account.StorageState)
		account.stateMessage = ""
	}
	// 启用状态影响模型可用通道的统计与候选资格（与 SaveConfig 相同）
	p.eligibilityVersion++
	p.accountChangedLocked(account)
	p.notifyLocked()
	p.mu.Unlock()
	return account, true, nil
}

// SetTransition 记录账户启停的后台收尾阶段（空字符串为收尾结束），账户列表随即显示；账户已不在账户池时返回 false
func (p *AccountPool) SetTransition(accountID string, transition AccountTransition) bool {
	if p == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	account := p.byID[strings.TrimSpace(accountID)]
	if account == nil {
		return false
	}
	if account.transition != transition {
		account.transition = transition
		p.refreshStatusEntryLocked(account)
	}
	return true
}
