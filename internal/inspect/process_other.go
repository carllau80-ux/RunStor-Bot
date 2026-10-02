//go:build !unix

package inspect

import (
	"os/exec"
	"runtime"
)

// setProcessGroup 在非 unix 平台上没有进程组语义, 退化为默认行为。
//
// 本项目的巡检脚本 (读 /proc、systemctl) 本身只对 Linux 有意义, 这里保留空实现
// 只为让包在任何平台上都能编译通过 —— 部署环境是 Debian, 真正走的是
// process_unix.go 里的实现。
func setProcessGroup(command *exec.Cmd) {
	_ = command
	_ = runtime.GOOS
}

// killProcessGroup 退化为只杀子进程自身。
func killProcessGroup(command *exec.Cmd) {
	if command.Process == nil {
		return
	}
	_ = command.Process.Kill()
}
