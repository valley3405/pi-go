//go:build unix

package tools

import (
	"os/exec"
	"syscall"
)

// setProcGroup 让子进程成为新进程组的组长（SysProcAttr.Setpgid），
// 这样超时可以整组杀死，而不是只杀 bash 本体。
func setProcGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killProcessGroup 向整个进程组发 SIGKILL（负 PID 表示进程组）。
func killProcessGroup(pid int) error {
	// 进程组可能已整体退出，忽略 ESRCH
	if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
		return err
	}
	return nil
}
