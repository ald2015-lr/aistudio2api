//go:build unix

package proxyrelay

import "syscall"

// getrlimitNoFile 返回 RLIMIT_NOFILE 的软限制；测试可替换
var getrlimitNoFile = func() (uint64, error) {
	var limit syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &limit); err != nil {
		return 0, err
	}
	return uint64(limit.Cur), nil
}

// defaultTotalConnections 取 RLIMIT_NOFILE 软限制的 1/4（Go 1.19 起进程启动时已把软限制提到硬限制），
// 夹在 1024 到 65536 之间：每条中转连接占 2 个 fd，留出一半给业务连接、管理面和浏览器管道
func defaultTotalConnections() int {
	soft, err := getrlimitNoFile()
	if err != nil {
		return minTotalConnections
	}
	return clampTotalConnections(soft / 4)
}
