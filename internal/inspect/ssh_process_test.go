package inspect

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// 这一组测试守的是两类"不会报错、只会悄悄烂掉"的故障:
//
//  1. askpass 助手认不出自己 —— 它不会报错, 而是当成"再启动一个服务实例"常驻,
//     于是每试一次连接泄漏一个进程, 同时 ssh 永远等不到密码, 巡检挂死。
//  2. 目标机在认证后失去响应 —— ssh 自己没有总超时, 一次巡检能把 HTTP 请求
//     永久挂住, 并发额度也被占死。

// writeScript 写一个可执行的 shell 脚本。
func writeScript(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatalf("写脚本失败: %v", err)
	}
	return path
}

// 认不出自己就会变成"又启动了一个服务实例": 这是本次修复的核心。
//
// 第一道判断是标记; 万一标记漏了 (旧版本、包装脚本、以后有人改了调用链), 只要
// SSH_ASKPASS 指的就是本程序, 也必须认出来。
func TestAskpassIsRecognisedEvenWithoutMarker(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("取自身路径失败: %v", err)
	}

	t.Setenv(askpassEnvMarker, "")
	t.Setenv("SSH_ASKPASS", self)
	if !IsAskpassInvocation() {
		t.Error("SSH_ASKPASS 指向本程序时必须认出自己是 askpass 助手, 否则会常驻成多余的服务实例")
	}

	// 反过来不能认错: SSH_ASKPASS 指向别的程序时, 服务端不该把自己当成助手
	other := writeScript(t, t.TempDir(), "not-bra", "exit 0\n")
	t.Setenv(askpassEnvMarker, "")
	t.Setenv("SSH_ASKPASS", other)
	if IsAskpassInvocation() {
		t.Error("SSH_ASKPASS 指向别的程序时不该自认 askpass")
	}

	// 标记在就必须认 —— 这条路径是正常路径
	t.Setenv("SSH_ASKPASS", other)
	t.Setenv(askpassEnvMarker, "1")
	if !IsAskpassInvocation() {
		t.Error("有标记时应处于 askpass 模式")
	}
}

// 只要把 SSH_ASKPASS 指向本程序, 标记就必须一起设上。
//
// 少了标记 + 密钥被拒 → ssh 转去问密码 → askpass 助手把自己当服务端常驻 +
// ssh 永远等不到密码。这条断言就是那次故障的护栏。
func TestAskpassMarkerAlwaysAccompaniesAskpassBinary(t *testing.T) {
	dir := t.TempDir()
	binary, argDir := fakeSSH(t, dir)

	targets := []Target{
		{Host: "h", User: "root", PrivateKey: privateKeyFixture},
		{Host: "h", User: "root", Password: "p"},
		{Host: "h", User: "root", PrivateKey: privateKeyFixture, Password: "p"},
	}
	for _, target := range targets {
		runner, err := newSSHRunner(target,
			SSHOptions{SSHBinary: binary, AskpassBinary: "/proc/self/exe", HostKeyPolicy: HostKeyTOFU})
		if err != nil {
			t.Fatalf("构造失败: %v", err)
		}
		if _, err := runner.Run(remoteScript(15)); err != nil {
			t.Fatalf("执行失败: %v", err)
		}
		runner.Cleanup()

		facts := readEnvFacts(t, argDir)
		if facts["ASKPASS"] == "" {
			t.Fatalf("%s: 假 ssh 没拿到 SSH_ASKPASS", target.AuthMode())
		}
		if facts["MARKER"] != "1" {
			t.Errorf("%s: SSH_ASKPASS=%q 但标记缺失 —— 被调起时会常驻成多余的服务实例",
				target.AuthMode(), facts["ASKPASS"])
		}
	}
}

// secret-scan:allow-test-key —— 下面是**假**密钥样本 (正文只有 AAAA / SECRETKEY / xxx
// 之类的占位符), 只用来验证凭据处理路径会不会把密钥写进日志、argv 或报告。
const privateKeyFixture = "-----BEGIN OPENSSH PRIVATE KEY-----\nAAAA\n-----END OPENSSH PRIVATE KEY-----\n"

// 卡死的 ssh 必须被总预算收掉, 而且连它的后代进程一起。
func TestRunKillsHungSSHWithinBudget(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "child.pid")
	// 假 ssh: 先留下一个后代进程, 再把自己挂住 —— 模拟"认证后失去响应",
	// 并且顺带验证超时会不会把后代留成孤儿。
	binary := writeScript(t, dir, "hanging-ssh", strings.Join([]string{
		"sleep 300 &",
		"echo $! > " + pidFile,
		"echo $$ >> " + pidFile,
		"sleep 300",
	}, "\n")+"\n")

	runner, err := newSSHRunner(Target{Host: "h", User: "root", Password: "p"},
		SSHOptions{
			SSHBinary: binary,
			// 3 秒总预算, 假 ssh 要睡 300 秒
			Timeout:        3 * time.Second,
			ConnectTimeout: 500 * time.Millisecond,
			HostKeyPolicy:  HostKeyTOFU,
		})
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}

	started := time.Now()
	_, runErr := runner.Run(remoteScript(15))
	elapsed := time.Since(started)

	if runErr == nil {
		t.Fatal("卡死的 ssh 不该被当作成功")
	}
	if !strings.Contains(runErr.Error(), "超时") {
		t.Errorf("应报告超时, 实际 %v", runErr)
	}
	if elapsed > 30*time.Second {
		t.Fatalf("超时没生效: 耗时 %s", elapsed)
	}

	// 后代进程也必须被收掉, 否则一次巡检留一串孤儿
	pids := readPIDs(t, pidFile)
	if len(pids) == 0 {
		t.Skip("假 ssh 未及写出子进程 pid (进程被太快杀掉), 跳过孤儿检查")
	}
	// 给进程组回收一点时间
	deadline := time.Now().Add(3 * time.Second)
	for _, pid := range pids {
		for {
			if !processAlive(pid) {
				break
			}
			if time.Now().After(deadline) {
				t.Errorf("超时后进程 %d 仍在运行, 会积累成孤儿", pid)
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
}

// runBudget 要覆盖"每项检查都拖到自己的超时"这种最坏情况, 又不能没有上限。
func TestRunBudgetIsBoundedAndCoversChecks(t *testing.T) {
	runner, err := newSSHRunner(Target{Host: "h", User: "root", Password: "p"}, SSHOptions{})
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}
	script := remoteScript(15)
	checks := strings.Count(script, "timeout ")
	if checks < 5 {
		t.Fatalf("采集脚本里应有若干项检查, 实际统计到 %d 项", checks)
	}
	budget := runner.runBudget(script)
	// 至少覆盖: 连接超时 + 每项检查各自的超时
	if minimum := runner.options.ConnectTimeout + runner.options.CommandTimeout*time.Duration(checks); budget < minimum {
		t.Errorf("总预算 %s 不足以覆盖最坏情况 %s", budget, minimum)
	}
	// 但必须有上限, 不能无限等
	if budget > 10*time.Minute {
		t.Errorf("总预算 %s 过长", budget)
	}
	// 显式指定时以指定值为准
	runner.options.Timeout = 42 * time.Second
	if got := runner.runBudget(script); got != 42*time.Second {
		t.Errorf("显式总预算应生效, 实际 %s", got)
	}
}

// readPIDs 读取脚本写下的 pid 列表。
func readPIDs(t *testing.T, path string) []int {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var pids []int
	for _, line := range strings.Fields(string(raw)) {
		if pid, err := strconv.Atoi(line); err == nil && pid > 1 {
			pids = append(pids, pid)
		}
	}
	return pids
}

// processAlive 报告进程是否还在 (signal 0 只做存在性检查)。
func processAlive(pid int) bool {
	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = process.Signal(syscall.Signal(0))
	if err == nil {
		return true
	}
	// 僵尸进程还占着进程表项, 但它已经被收掉了
	return !strings.Contains(err.Error(), "no such process") && strings.Contains(err.Error(), "permission")
}
