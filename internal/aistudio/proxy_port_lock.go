package aistudio

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"

	"github.com/gofrs/flock"
)

// ProxyPortLock 为 IP 池端口的跨进程锁（同机多个进程使用同一端口段时互斥，见 app/proxy_pool.go）
type ProxyPortLock struct {
	lock *flock.Flock
	once sync.Once
	err  error
}

// ProxyPortLockDirectory 返回跨进程端口锁目录：用户缓存目录下的 AIStudio2API/runtime-leases，与账户 WAA runtime 租约相同
func ProxyPortLockDirectory() (string, error) {
	cacheRoot, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("读取用户缓存目录: %w", err)
	}
	return filepath.Join(cacheRoot, "AIStudio2API", "runtime-leases"), nil
}

// TryLockProxyPort 尝试锁定 IP 池端口（不阻塞）：文件名 proxy-port-<endpointKey>-<port>.lock（endpointKey 为哈希，不含地址明文）；
// 被其他进程（或同一进程里另一个句柄）占用时 locked 为 false。flock 按打开的文件描述互斥，同一进程里同一端口只应持有一把
func TryLockProxyPort(directory, endpointKey string, port int) (lock *ProxyPortLock, locked bool, err error) {
	if directory == "" || endpointKey == "" || port < 1 || port > 65535 {
		return nil, false, fmt.Errorf("端口锁参数无效")
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return nil, false, fmt.Errorf("创建 IP 池端口锁目录: %w", err)
	}
	fileLock := flock.New(filepath.Join(directory, "proxy-port-"+endpointKey+"-"+strconv.Itoa(port)+".lock"))
	ok, err := fileLock.TryLock()
	if err != nil {
		return nil, false, fmt.Errorf("锁定 IP 池端口: %w", err)
	}
	if !ok {
		return nil, false, nil
	}
	return &ProxyPortLock{lock: fileLock}, true, nil
}

// Release 释放端口锁（幂等，对 nil 安全）
func (lock *ProxyPortLock) Release() error {
	if lock == nil || lock.lock == nil {
		return nil
	}
	lock.once.Do(func() {
		lock.err = lock.lock.Unlock()
	})
	return lock.err
}
