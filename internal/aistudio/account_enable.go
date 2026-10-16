package aistudio

import (
	"errors"
	"fmt"
)

// EnableLoadedAccount 把已经载入、尚未加入账户池的停用账户改为启用：内存中的配置与状态总是改为启用
// （有登录 Cookie 为就绪，否则为需要登录，与启用账户载入时一致），再在账户的跨进程文件租约内原子写回
// account.json（格式与 SaveConfig 相同）。返回的错误只表示 account.json 没有写回：
//   - 另一个进程（或仍在运行的上一代生成服务）正持有该账户的租约时不等待、不写文件，返回 ErrAccountLeased；
//   - 账户目录已被删除时返回 ErrAccountNotFound，不会因写文件重新创建目录。
//
// 账户默认启用，没有写回的账户下次载入时会再次按同样的规则启用。只能在账户加入账户池之前调用：
// 这里直接修改账户字段，不经过账户池锁
func EnableLoadedAccount(account *Account) error {
	if account == nil {
		return fmt.Errorf("账户未初始化")
	}
	if account.Config.Enabled {
		return nil
	}
	value := account.Config
	value.Enabled = true
	account.Config = value
	account.State = initialAccountState(value, account.StorageState)
	account.stateMessage = ""
	return persistLoadedAccountConfig(account, value)
}

// persistLoadedAccountConfig 在账户文件租约内写回 account.json；没有持久目录的内存账户直接返回
func persistLoadedAccountConfig(account *Account, value AccountConfig) (resultErr error) {
	if account.ConfigPath == "" {
		return nil
	}
	lock, _, err := acquireAccountFileLease(account.StoragePath)
	if errors.Is(err, errAccountLeaseBusy) {
		return fmt.Errorf("%w: %s", ErrAccountLeased, account.ID)
	}
	if err != nil {
		return err
	}
	if lock != nil {
		defer func() {
			resultErr = errors.Join(resultErr, lock.Unlock())
		}()
	}
	if err := validatePersistentAccountFiles(account); err != nil {
		return err
	}
	account.storageMu.Lock()
	defer account.storageMu.Unlock()
	return writeAccountConfig(account.ConfigPath, value)
}
