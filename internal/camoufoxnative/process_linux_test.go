//go:build linux

package camoufoxnative

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestBrowserProcessDiesWithParent 服务进程被 SIGKILL 后，由它启动的浏览器进程随之结束
func TestBrowserProcessDiesWithParent(t *testing.T) {
	if os.Getenv("AISTUDIO2API_PDEATH_HELPER") == "1" {
		child := exec.Command("sleep", "60")
		configureBrowserProcess(child, true)
		if err := child.Start(); err != nil {
			fmt.Println("error", err)
			os.Exit(1)
		}
		fmt.Println(child.Process.Pid)
		time.Sleep(time.Minute)
		return
	}
	helper := exec.Command(os.Args[0], "-test.run=^TestBrowserProcessDiesWithParent$")
	helper.Env = append(os.Environ(), "AISTUDIO2API_PDEATH_HELPER=1")
	output, err := helper.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := helper.Start(); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(output).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	childPID, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil {
		t.Fatalf("辅助进程输出 %q", line)
	}
	_ = helper.Process.Kill()
	_ = helper.Wait()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(childPID, 0); err == syscall.ESRCH {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	_ = syscall.Kill(childPID, syscall.SIGKILL)
	t.Fatalf("服务进程退出 5 秒后浏览器进程 %d 仍在运行", childPID)
}
