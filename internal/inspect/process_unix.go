//go:build unix

package inspect

import (
	"os/exec"
	"syscall"
)

// setProcessGroup 让子进程成为独立进程组的组长。
//
// 目的是"一次执行的所有后代都能被一起收掉": ssh 需要密码时会fork出 askpass
// 助手, 只杀 ssh 自己会把助手留成孤儿进程。
func setProcessGroup(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killProcessGroup 杀掉整个进程组 (Setpgid 之后组长 pid 就是组 id)。
func killProcessGroup(command *exec.Cmd) {
	if command.Process == nil {
		return
	}
	_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
}
