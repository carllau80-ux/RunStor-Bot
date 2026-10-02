package inspect

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 密码路径的真实 sshd 不容易在测试环境里备好 (需要能改系统认证配置), 因此这里用
// 一个假 ssh 替身精确验证"密码怎么进到子进程"这条最容易出错的链路:
//   - 只经环境变量, 不进 argv (ps 里能看到命令行)
//   - 设置 SSH_ASKPASS 指向本程序, 且强制 ssh 用它 (SSH_ASKPASS_REQUIRE=force)
//   - 私钥写入 0600 临时文件, 且用完删除
//
// 假 ssh 会把收到的 argv 与环境写成 JSON, 供断言检查。

// fakeSSH 生成一个假 ssh 可执行文件。
//
// 记录方式: **每个参数写一个文件** + 一份环境事实。不用 JSON 是因为脚本参数本身
// 含换行, 嵌进 JSON 字符串会破坏格式 —— 而"能不能正确记录"不该成为测试的负担。
func fakeSSH(t *testing.T, dir string) (binary string, argDir string) {
	t.Helper()
	binary = filepath.Join(dir, "fake-ssh")
	argDir = filepath.Join(dir, "args")
	if err := os.MkdirAll(argDir, 0o755); err != nil {
		t.Fatalf("建目录失败: %v", err)
	}
	envFile := filepath.Join(dir, "env.txt")

	script := "#!/bin/sh\n" +
		"i=0\n" +
		"for a in \"$@\"; do printf '%s' \"$a\" > \"" + argDir + "/$i\"; i=$((i+1)); done\n" +
		"{\n" +
		"  echo \"COUNT=$i\"\n" +
		"  echo \"ASKPASS=$SSH_ASKPASS\"\n" +
		"  echo \"REQUIRE=$SSH_ASKPASS_REQUIRE\"\n" +
		"  echo \"MARKER=$BRA_ASKPASS\"\n" +
		"  echo \"SECRETLEN=${#BRA_ASKPASS_SECRET}\"\n" +
		"  echo \"PASSPHRASELEN=${#BRA_ASKPASS_PASSPHRASE}\"\n" +
		"} > " + envFile + "\n" +
		"echo '" + markPrefix + "end" + markSuffix + "'\n"

	if err := os.WriteFile(binary, []byte(script), 0o755); err != nil {
		t.Fatalf("写假 ssh 失败: %v", err)
	}
	return binary, argDir
}

// readArgs 返回假 ssh 收到的参数 (按序号排序)。
func readArgs(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("假 ssh 未被调用: %v", err)
	}
	out := make([]string, 0, len(entries))
	for index := 0; index < len(entries); index++ {
		raw, err := os.ReadFile(filepath.Join(dir, fmt.Sprintf("%d", index)))
		if err != nil {
			t.Fatalf("读取参数 %d 失败: %v", index, err)
		}
		out = append(out, string(raw))
	}
	return out
}

// readEnvFacts 返回假 ssh 记录的环境事实。
func readEnvFacts(t *testing.T, dir string) map[string]string {
	t.Helper()
	envRaw, err := os.ReadFile(filepath.Join(filepath.Dir(dir), "env.txt"))
	if err != nil {
		t.Fatalf("未记录环境: %v", err)
	}
	facts := map[string]string{}
	for _, line := range strings.Split(string(envRaw), "\n") {
		if key, value, ok := strings.Cut(line, "="); ok {
			facts[key] = value
		}
	}
	return facts
}

func TestPasswordGoesThroughEnvNotArgv(t *testing.T) {
	const password = "sup3r-s3cret-passw0rd"
	dir := t.TempDir()
	binary, argDir := fakeSSH(t, dir)

	runner, err := newSSHRunner(Target{Host: "10.0.0.1", User: "root", Password: password},
		SSHOptions{SSHBinary: binary, AskpassBinary: "/proc/self/exe", HostKeyPolicy: HostKeyTOFU})
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}
	if _, err := runner.Run(remoteScript(15)); err != nil {
		t.Fatalf("执行失败: %v", err)
	}

	// 密码绝不能出现在命令行里 —— ps 对同机用户可见
	for index, arg := range readArgs(t, argDir) {
		if strings.Contains(arg, password) {
			t.Fatalf("密码出现在第 %d 个参数里 (%d 字节)", index, len(arg))
		}
	}

	facts := readEnvFacts(t, argDir)
	// 而是通过环境变量交给子进程 (只有同用户可读, ps 看不到)
	if facts["SECRETLEN"] != fmt.Sprintf("%d", len(password)) {
		t.Fatalf("环境变量里的密码长度不对: %q", facts["SECRETLEN"])
	}
	// SSH_ASKPASS 是环境变量: OpenSSH 靠它找到"取密码的程序"
	if facts["ASKPASS"] != "/proc/self/exe" {
		t.Errorf("SSH_ASKPASS 应指向本程序: %q", facts["ASKPASS"])
	}
	if facts["MARKER"] != "1" {
		t.Errorf("应进入 askpass 模式: %q", facts["MARKER"])
	}

	// SSH_ASKPASS_REQUIRE 必须是**环境变量**: 服务进程既没终端也没 DISPLAY, 没有它
	// ssh 根本不会去问 askpass, 密码就递不进去。
	//
	// 而它作为配置项传 (-o SSH_ASKPASS_REQUIRE=force) 会让 ssh 直接
	// "Bad configuration option" 退出 —— 表现是"提供了密码反而立刻失败", 而且失败
	// 得毫无头绪。曾经就这么错过一次, 所以这里两个方向都断言。
	if facts["REQUIRE"] != "force" {
		t.Errorf("应以环境变量 SSH_ASKPASS_REQUIRE=force 强制使用 askpass, 实际 %q", facts["REQUIRE"])
	}
	joined := strings.Join(readArgs(t, argDir), " ")
	if strings.Contains(joined, "SSH_ASKPASS_REQUIRE") {
		t.Errorf("SSH_ASKPASS_REQUIRE 不是 ssh 配置项, 不能通过 -o 传: %s", joined)
	}
}

// authMethods 取出传给 ssh 的 PreferredAuthentications 值。
func authMethods(t *testing.T, argDir string) string {
	t.Helper()
	for _, arg := range readArgs(t, argDir) {
		if value, ok := strings.CutPrefix(arg, "PreferredAuthentications="); ok {
			return value
		}
	}
	t.Fatal("未找到 PreferredAuthentications 参数")
	return ""
}

// 只有私钥时, 不要去试密码认证。
//
// 这条最初写反了: 当时断言"只有私钥就不进入 askpass 模式", 结果 ssh 在密钥被拒后
// 转去问密码, 而 askpass 助手 (本程序) 因为缺标记不认识自己, 就当成了"再启动一个
// 服务实例" —— 于是每试一次泄漏一个常驻进程, 且 ssh 永远等不到密码, 巡检挂死。
// 现在: 标记必须设 (它只表示"被调起时该扮演 askpass"), 但既不强制 SSH_ASKPASS_REQUIRE,
// 也不交出任何秘密, 认证方式里更没有 password。
func TestKeyAuthDoesNotOfferPasswordAuth(t *testing.T) {
	dir := t.TempDir()
	binary, argDir := fakeSSH(t, dir)

	runner, err := newSSHRunner(Target{
		Host: "10.0.0.1", User: "root",
		// secret-scan:allow-test-key —— 下面是**假**密钥样本 (正文只有 AAAA / SECRETKEY / xxx
		// 之类的占位符), 只用来验证凭据处理路径会不会把密钥写进日志、argv 或报告。
		PrivateKey: "-----BEGIN OPENSSH PRIVATE KEY-----\nAAAA\n-----END OPENSSH PRIVATE KEY-----\n",
	}, SSHOptions{SSHBinary: binary, AskpassBinary: "/proc/self/exe", HostKeyPolicy: HostKeyTOFU})
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}
	defer runner.Cleanup()

	if _, err := runner.Run(remoteScript(15)); err != nil {
		t.Fatalf("执行失败: %v", err)
	}

	facts := readEnvFacts(t, argDir)
	// 标记必须设: SSH_ASKPASS 指向本程序, 少了标记就等着进程泄漏
	if facts["MARKER"] != "1" {
		t.Errorf("SSH_ASKPASS 指向本程序时标记必须设置, 实际 %q", facts["MARKER"])
	}
	// 但没有任何秘密交给子进程
	if facts["SECRETLEN"] != "0" {
		t.Errorf("只有私钥时不该提供密码, 实际长度 %q", facts["SECRETLEN"])
	}
	joined := strings.Join(readArgs(t, argDir), " ")
	if facts["REQUIRE"] != "" {
		t.Errorf("只有私钥时不该强制 ssh 使用 askpass, 实际 %q", facts["REQUIRE"])
	}
	// 认证方式只留 publickey: 没密码可给, 白试一次只会污染目标机的失败计数
	if got := authMethods(t, argDir); got != "publickey" {
		t.Errorf("只给私钥时认证方式应为 publickey, 实际 %q", got)
	}
	// 私钥本身要作为 -i 参数传进去
	if !strings.Contains(joined, "-i ") {
		t.Error("应通过 -i 指定私钥")
	}
}

// 只给密码时不该列上 publickey —— 没有密钥可试, 列上它只会让 ssh 拿默认身份文件
// 去碰运气, 那也是"用了不该用的凭据"。
func TestPasswordAuthOffersNoPublickey(t *testing.T) {
	dir := t.TempDir()
	binary, argDir := fakeSSH(t, dir)

	runner, err := newSSHRunner(Target{Host: "h", User: "root", Password: "p"},
		SSHOptions{SSHBinary: binary, AskpassBinary: "/proc/self/exe", HostKeyPolicy: HostKeyTOFU})
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}
	if _, err := runner.Run(remoteScript(15)); err != nil {
		t.Fatalf("执行失败: %v", err)
	}

	if got := authMethods(t, argDir); got != "password,keyboard-interactive" {
		t.Errorf("只给密码时认证方式应为密码类, 实际 %q", got)
	}
}

// 私钥要落成 0600 临时文件 (ssh 拒绝读取权限过宽的私钥), 且用完删除。
func TestPrivateKeyIsTemporaryAndTight(t *testing.T) {
	dir := t.TempDir()
	binary, _ := fakeSSH(t, dir)
	// secret-scan:allow-test-key —— 下面是**假**密钥样本 (正文只有 AAAA / SECRETKEY / xxx
	// 之类的占位符), 只用来验证凭据处理路径会不会把密钥写进日志、argv 或报告。
	const key = "-----BEGIN OPENSSH PRIVATE KEY-----\nSECRETKEY\n-----END OPENSSH PRIVATE KEY-----"

	runner, err := newSSHRunner(Target{Host: "h", User: "root", PrivateKey: key},
		SSHOptions{SSHBinary: binary, HostKeyPolicy: HostKeyTOFU})
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}
	if runner.keyPath == "" {
		t.Fatal("应准备临时私钥文件")
	}
	info, err := os.Stat(runner.keyPath)
	if err != nil {
		t.Fatalf("临时私钥不存在: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("临时私钥权限应为 0600, 实际 %04o", mode)
	}
	if _, err := os.Stat(runner.secretDir); err != nil {
		t.Fatalf("临时目录不存在: %v", err)
	}

	runner.Cleanup()
	if _, err := os.Stat(runner.secretDir); !os.IsNotExist(err) {
		t.Error("清理后临时私钥必须删除, 不能在磁盘上留下凭据")
	}
}

// 主机指纹策略要真的传给 ssh。
func TestHostKeyPolicyIsPassedToSSH(t *testing.T) {
	dir := t.TempDir()
	binary, argDir := fakeSSH(t, dir)

	cases := map[string]string{
		HostKeyTOFU:     "StrictHostKeyChecking=accept-new",
		HostKeyStrict:   "StrictHostKeyChecking=yes",
		HostKeyInsecure: "StrictHostKeyChecking=no",
	}
	for policy, want := range cases {
		runner, err := newSSHRunner(Target{Host: "h", User: "root", Password: "p"},
			SSHOptions{SSHBinary: binary, HostKeyPolicy: policy})
		if err != nil {
			t.Fatalf("构造失败: %v", err)
		}
		if _, err := runner.Run(remoteScript(15)); err != nil {
			t.Fatalf("执行失败: %v", err)
		}
		joined := strings.Join(readArgs(t, argDir), " ")
		if !strings.Contains(joined, want) {
			t.Errorf("策略 %s 应产生 %q, 实际参数: %s", policy, want, joined)
		}
	}
}

// ssh 的各类失败要能区分开: 认证失败、主机指纹不符、连不上, 处置方式完全不同。
func TestClassifySSHError(t *testing.T) {
	cases := []struct {
		stderr string
		want   string
	}{
		{"root@1.2.3.4: Permission denied (publickey,password).", "认证失败"},
		{"Host key verification failed.", "主机指纹校验失败"},
		{"ssh: connect to host 1.2.3.4 port 22: Connection refused", "连接被拒绝"},
		{"ssh: connect to host 1.2.3.4 port 22: Connection timed out", "连接超时"},
		{"ssh: Could not resolve hostname nope: Name or service not known", "无法解析主机名"},
		{"No route to host", "网络不可达"},
		{"something weird happened", "ssh 执行失败"},
		// 连续认证失败会触发 sshd 的按来源限流, 表现是"连接被重置" —— 要给得出原因
		{"kex_exchange_identification: read: Connection reset by peer", "按来源限流"},
	}
	for _, entry := range cases {
		err := classifySSHError(entry.stderr, os.ErrNotExist)
		if err == nil || !strings.Contains(err.Error(), entry.want) {
			t.Errorf("%q 应归类为 %q, 实际 %v", entry.stderr, entry.want, err)
		}
	}
}

// askpass 模式: 被 ssh 调起时应把密码打印到标准输出后退出。
func TestRunAskpassPrintsSecret(t *testing.T) {
	t.Setenv(askpassEnvMarker, "1")
	t.Setenv(askpassEnvSecret, "the-password")
	t.Setenv(askpassEnvPassphrase, "the-passphrase")

	if !IsAskpassInvocation() {
		t.Fatal("设置了标记就该处于 askpass 模式")
	}

	// 捕获标准输出: 这是 ssh 读取密码的通道, 必须精确断言
	original := os.Stdout
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("建管道失败: %v", err)
	}
	os.Stdout = writer
	code := RunAskpass([]string{"root@host's password:"})
	_ = writer.Close()
	captured := make([]byte, 128)
	count, _ := reader.Read(captured)
	_ = reader.Close()
	os.Stdout = original

	if code != 0 {
		t.Errorf("askpass 应正常退出: %d", code)
	}
	if got := strings.TrimSpace(string(captured[:count])); got != "the-password" {
		t.Fatalf("应打印登录密码, 实际 %q", got)
	}
}

// 私钥口令的提示与登录密码的提示要能区分, 否则会用密码去解私钥。
func TestAskpassDistinguishesPassphrasePrompt(t *testing.T) {
	// RunAskpass 输出到标准输出, 这里只验证它不 panic 且返回值正常;
	// 选择逻辑由下面的纯函数断言覆盖。
	t.Setenv(askpassEnvMarker, "1")
	t.Setenv(askpassEnvSecret, "login-password")
	t.Setenv(askpassEnvPassphrase, "key-passphrase")

	if got := askpassSecretFor("Enter passphrase for key '/tmp/id':"); got != "key-passphrase" {
		t.Errorf("私钥口令提示应取口令: %q", got)
	}
	if got := askpassSecretFor("root@host's password:"); got != "login-password" {
		t.Errorf("登录提示应取密码: %q", got)
	}
}

// 必须只用本次提供的凭据: 不加 IdentitiesOnly/IdentityAgent=none 时 ssh 会回退用
// agent 里的密钥, 于是"错密码/错私钥"也可能连上 —— 那会让用户误判自己的凭据有效。
func TestSSHUsesOnlyProvidedCredentials(t *testing.T) {
	dir := t.TempDir()
	binary, argDir := fakeSSH(t, dir)

	cases := []Target{
		{Host: "h", User: "root", Password: "p"},
		{Host: "h", User: "root", PrivateKey: "-----BEGIN OPENSSH PRIVATE KEY-----\nA\n-----END OPENSSH PRIVATE KEY-----\n"},
	}
	for _, target := range cases {
		runner, err := newSSHRunner(target, SSHOptions{SSHBinary: binary, HostKeyPolicy: HostKeyTOFU})
		if err != nil {
			t.Fatalf("构造失败: %v", err)
		}
		if _, err := runner.Run(remoteScript(15)); err != nil {
			t.Fatalf("执行失败: %v", err)
		}
		runner.Cleanup()

		joined := strings.Join(readArgs(t, argDir), " ")
		if !strings.Contains(joined, "IdentitiesOnly=yes") {
			t.Errorf("%s: 应禁用 agent 密钥回退, 实际参数: %s", target.AuthMode(), joined)
		}
		if !strings.Contains(joined, "IdentityAgent=none") {
			t.Errorf("%s: 应关闭 agent 通道, 实际参数: %s", target.AuthMode(), joined)
		}
	}
}
