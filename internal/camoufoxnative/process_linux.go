//go:build linux

package camoufoxnative

import "syscall"

// setParentDeathSignal 父进程退出时向浏览器发送 SIGKILL
func setParentDeathSignal(attributes *syscall.SysProcAttr) {
	attributes.Pdeathsig = syscall.SIGKILL
}
