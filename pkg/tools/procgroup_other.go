//go:build !unix

package tools

import "os/exec"

// 非 Unix 平台（Windows）无进程组语义，退化为默认行为：
// CommandContext 只杀直接子进程。
func setProcGroup(cmd *exec.Cmd) {}

func killProcessGroup(pid int) error { return nil }
