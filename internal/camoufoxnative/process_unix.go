//go:build !windows && !linux

package camoufoxnative

import "syscall"

// setParentDeathSignal 其他类 Unix 平台没有父进程退出信号，由 start.sh 的残留清理兜底
func setParentDeathSignal(*syscall.SysProcAttr) {}
