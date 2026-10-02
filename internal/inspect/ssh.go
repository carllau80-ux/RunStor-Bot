package inspect

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Runner 抽象"在目标机上执行巡检脚本"。
//
// 抽成接口是为了让判定逻辑可测: 真实实现要连服务器, 测试注入一个假实现即可覆盖
// 解析与阈值, 不必依赖网络与凭据。
type Runner interface {
	// Run 在目标机上执行固定脚本, 返回其标准输出 (含标准错误)。
	Run(script string) (string, error)
	// HostKeyFingerprint 返回本次连接实际使用的主机指纹 (取不到时返回空串)。
	HostKeyFingerprint() string
}

// SSHOptions 是 SSH 执行的参数。
type SSHOptions struct {
	SSHBinary      string
	ConnectTimeout time.Duration
	// CommandTimeout 是远端单项检查的超时 (与写进采集脚本的值一致)。
	// 用来推算一次执行的总预算, 见 Timeout。
	CommandTimeout time.Duration
	// Timeout 是单次 ssh 执行的总预算, 为 0 时按 ConnectTimeout + 各项检查超时推算。
	Timeout time.Duration
	// HostKeyPolicy: tofu (默认, 未知主机接受但报告指纹) / strict / insecure
	HostKeyPolicy  string
	KnownHostsPath string
	// AskpassBinary 是拿密码用的辅助程序路径 (通常是本程序自身)。
	AskpassBinary string
}

// 主机指纹策略。
const (
	HostKeyTOFU     = "tofu"
	HostKeyStrict   = "strict"
	HostKeyInsecure = "insecure"
)

// sshRunner 通过系统 OpenSSH 客户端执行巡检。
//
// 为什么用系统 ssh 而不是纯 Go 的 golang.org/x/crypto/ssh:
// 本项目要能在不通外网的环境里从零构建, 而 x/crypto/ssh 会引入 x/sys 与 x/term
// (前者是 chacha20 的 CPU 特性探测, 后者是它测试包的依赖), 这两者在离线环境里取不到。
// 自己写桩模块替换一个密码学依赖是长期隐患, 不如用系统自带、且在目标环境必然存在的
// OpenSSH 客户端。代价是需要目标机装有 ssh (Linux 服务器默认都有)。
type sshRunner struct {
	target  Target
	options SSHOptions
	// secretDir 存放本次连接用到的临时文件 (私钥), 用完即删。
	secretDir string
	// keyPath 非空表示本次使用公钥认证 (私钥来自请求)。
	keyPath string
	// fingerprint 是本次连接实际信任的主机指纹。
	fingerprint string
}

// newSSHRunner 准备一次连接所需的临时资源。
//
// 密码走 SSH_ASKPASS: 交给子进程一个环境变量, 由本程序自己在被 ssh 调用时把密码
// 打印出来。这样密码既不进 argv (ps 可见), 也不落盘 (无需临时文件)。
func newSSHRunner(target Target, options SSHOptions) (*sshRunner, error) {
	if strings.TrimSpace(target.Host) == "" {
		return nil, errors.New("未提供主机地址")
	}
	if options.SSHBinary == "" {
		options.SSHBinary = "ssh"
	}
	if options.ConnectTimeout <= 0 {
		options.ConnectTimeout = 8 * time.Second
	}
	if options.CommandTimeout <= 0 {
		options.CommandTimeout = 15 * time.Second
	}

	runner := &sshRunner{target: target, options: options}

	if key := strings.TrimSpace(target.PrivateKey); key != "" {
		dir, err := os.MkdirTemp("", "bra-inspect-*")
		if err != nil {
			return nil, fmt.Errorf("创建临时目录失败: %w", err)
		}
		runner.secretDir = dir
		keyPath := filepath.Join(dir, "id_key")
		if err := os.WriteFile(keyPath, []byte(ensureTrailingNewline(key)), 0o600); err != nil {
			runner.Cleanup()
			return nil, fmt.Errorf("写入临时私钥失败: %w", err)
		}
		runner.keyPath = keyPath
	}
	return runner, nil
}

// Cleanup 删除本次连接用到的临时文件 (私钥)。
func (r *sshRunner) Cleanup() {
	if r.secretDir != "" {
		_ = os.RemoveAll(r.secretDir)
		r.secretDir = ""
	}
}

// HostKeyFingerprint 实现 Runner。
func (r *sshRunner) HostKeyFingerprint() string { return r.fingerprint }

// buildArgs 组装 ssh 参数。
//
// 用户输入 (主机/用户/端口) 只作为独立参数传递, 从不经过 shell —— 因此不存在
// 命令注入的可能。
func (r *sshRunner) buildArgs(script string) []string {
	target := r.target
	port := target.Port
	if port == 0 {
		port = 22
	}
	user := target.User
	if user == "" {
		user = "root"
	}

	args := []string{
		"-p", fmt.Sprintf("%d", port),
		"-o", fmt.Sprintf("ConnectTimeout=%d", int(r.options.ConnectTimeout.Seconds())),
		// 只尝试一次密码, 避免认证失败时反复重试把账号锁掉
		"-o", "NumberOfPasswordPrompts=1",
		"-o", "LogLevel=ERROR",
		"-o", "PreferredAuthentications=" + r.preferredAuthentications(),
		// 只用本次提供的凭据。
		//
		// 不加这两项时 ssh 会回退去用 ssh-agent 里的密钥 —— 于是"给了错私钥"或
		// "给了错密码"都可能因为 agent 里恰好有可用密钥而连上, 用户会以为自己的
		// 凭据是有效的。巡检必须如实反映"用你给的凭据能不能连上"。
		"-o", "IdentitiesOnly=yes",
		"-o", "IdentityAgent=none",
	}

	switch r.options.HostKeyPolicy {
	case HostKeyStrict:
		args = append(args, "-o", "StrictHostKeyChecking=yes")
	case HostKeyInsecure:
		args = append(args, "-o", "StrictHostKeyChecking=no",
			"-o", "UserKnownHostsFile=/dev/null")
	default: // tofu
		args = append(args, "-o", "StrictHostKeyChecking=accept-new")
	}
	if path := strings.TrimSpace(r.options.KnownHostsPath); path != "" {
		args = append(args, "-o", "UserKnownHostsFile="+path)
	}

	if r.keyPath != "" {
		args = append(args, "-i", r.keyPath)
	}

	args = append(args, user+"@"+target.Host, script)
	return args
}

// preferredAuthentications 按本次给出的凭据决定认证方式。
//
// 只给了私钥就不要列上 password: ssh 在密钥被拒后会转去问密码, 那既无意义
// (本来就没密码可给), 又会在目标机上留下失败计数 —— 有的环境配了 fail2ban 或
// 账号锁定策略, 几次巡检就能把账号试锁。反过来, 只给了密码就不要列 publickey。
func (r *sshRunner) preferredAuthentications() string {
	methods := make([]string, 0, 3)
	if r.keyPath != "" {
		methods = append(methods, "publickey")
	}
	if strings.TrimSpace(r.target.Password) != "" {
		methods = append(methods, "password", "keyboard-interactive")
	}
	if len(methods) == 0 {
		// 理论上不会到这里 (上层会先拒绝没有凭据的请求), 但真到了也别让 ssh
		// 自己去挑方法 —— 那正是 agent 回退的入口。
		methods = append(methods, "publickey")
	}
	return strings.Join(methods, ",")
}

// Run 执行巡检脚本。
func (r *sshRunner) Run(script string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), r.runBudget(script))
	defer cancel()

	command := exec.CommandContext(ctx, r.options.SSHBinary, r.buildArgs(script)...)
	// 超时后连整个进程组一起收掉: ssh 可能已经fork出 askpass 助手, 只杀 ssh 会
	// 把助手留成孤儿。
	setProcessGroup(command)
	command.Cancel = func() error {
		killProcessGroup(command)
		return nil
	}

	env := os.Environ()
	if password := r.target.Password; strings.TrimSpace(password) != "" {
		env = append(env, askpassEnvSecret+"="+password)
	}
	if binary := strings.TrimSpace(r.options.AskpassBinary); binary != "" {
		// 标记必须**无条件**跟着 SSH_ASKPASS 一起设。
		//
		// ssh 只在需要密码时才调 askpass, 但被调起的就是本程序: 若这时缺标记,
		// 本程序不会认为自己是 askpass 助手, 而会当作"又启动了一个服务实例"去
		// 加载配置、绑定端口并常驻。于是 ssh 永远等不到密码而挂死, 同时每试一次
		// 就留下一个不知道自己该干什么的进程。曾经就是"只给了私钥、密钥被拒 →
		// ssh 转而问密码"这条路触发的。
		env = append(env, askpassEnvMarker+"=1", "SSH_ASKPASS="+binary)
	}
	if passphrase := strings.TrimSpace(r.target.Passphrase); strings.TrimSpace(passphrase) != "" {
		env = append(env, askpassEnvPassphrase+"="+passphrase)
	}
	if strings.TrimSpace(r.target.Password) != "" || strings.TrimSpace(r.target.Passphrase) != "" {
		// 让 ssh 在没有终端时也去问 askpass。
		//
		// 这是**环境变量**, 不是 ssh_config 配置项 —— 写成 `-o SSH_ASKPASS_REQUIRE=force`
		// 会让 ssh 直接以 "Bad configuration option" 退出, 也就是"给了密码反而立刻失败"。
		// 没有它的话, ssh 只在"没有终端且有 DISPLAY"时才用 askpass; 服务进程既没有
		// 终端也没有 DISPLAY, 于是密码永远递不进去。
		env = append(env, askpassEnvRequire+"=force")
	}
	command.Env = env

	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	// 不分配 stdin: ssh 没有终端时才会走 askpass
	command.Stdin = nil

	runErr := command.Run()
	output := stdout.String()
	stderrText := strings.TrimSpace(stderr.String())

	// 记录本次实际信任的主机指纹 —— TOFU 的意义就在于"信任了什么"是可见的,
	// 不给出指纹的话这个策略只剩一句空话。
	r.fingerprint = r.hostKeyFingerprint()

	if runErr != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return output, fmt.Errorf("巡检超时: 本次执行超过 %s 仍未完成, 已终止 (目标机可能在认证后失去响应)",
				r.runBudget(script))
		}
		if output != "" {
			// 部分输出可能已经拿到 (脚本内部有 timeout, 单项失败不影响整体)
			return output, classifySSHError(stderrText, runErr)
		}
		return "", classifySSHError(stderrText, runErr)
	}
	return output, nil
}

// runBudget 计算一次执行的总预算。
//
// 远端脚本里每项检查都自带 timeout, 但那要等登录成功才生效: 目标机接受 TCP 连接
// 却在认证或读输出阶段卡住时, ssh 自己会一直等下去。没有总预算, 这样"半死不活"
// 的主机就能把一次巡检 (以及它背后的 HTTP 请求) 永远挂在那里。
func (r *sshRunner) runBudget(script string) time.Duration {
	if r.options.Timeout > 0 {
		return r.options.Timeout
	}
	checks := strings.Count(script, "timeout ")
	if checks <= 0 {
		checks = 1
	}
	// 每项检查各自的超时之和 + 连接超时 + 收尾余量
	return r.options.ConnectTimeout +
		r.options.CommandTimeout*time.Duration(checks) +
		10*time.Second
}

// classifySSHError 把 ssh 的退出码与 stderr 翻译成可读的失败原因。
//
// 巡检最怕"看起来成功了但其实没连上": 认证失败、主机不可达、脚本没拿到, 这三类
// 必须能被区分开, 否则报告会把"连不上"显示成"没有发现问题"。
func classifySSHError(stderr string, err error) error {
	lowered := strings.ToLower(stderr)

	switch {
	case strings.Contains(lowered, "permission denied"):
		return fmt.Errorf("认证失败: 用户名、密码或密钥不正确 (%s)", firstLineOf(stderr))
	case strings.Contains(lowered, "host key verification failed"):
		return fmt.Errorf("主机指纹校验失败: 该主机的指纹与 known_hosts 不一致 (%s)",
			firstLineOf(stderr))
	case strings.Contains(lowered, "connection refused"):
		return fmt.Errorf("连接被拒绝: 目标端口未开放 SSH 服务")
	case strings.Contains(lowered, "no route to host"), strings.Contains(lowered, "network is unreachable"):
		return fmt.Errorf("网络不可达: %s", firstLineOf(stderr))
	case strings.Contains(lowered, "connection timed out"), strings.Contains(lowered, "timed out"):
		return fmt.Errorf("连接超时: 主机不可达或被防火墙拦截")
	case strings.Contains(lowered, "connection reset by peer"), strings.Contains(lowered, "kex_exchange_identification"):
		// OpenSSH 9.8 起有"按来源惩罚": 同一个来源连续认证失败会被临时拒绝连接。
		// 巡检密码填错几次就可能撞上, 表现出来却是"连接被重置"这种看不出原因的错误。
		return fmt.Errorf("连接被重置: 目标机可能已临时拒绝本机 (%s)。"+
			"常见原因是连续认证失败触发了 sshd 的按来源限流 (PerSourcePenalties), 等一会儿再试;"+
			"若反复出现请核对凭据",
			firstLineOf(stderr))
	case strings.Contains(lowered, "could not resolve hostname"):
		return fmt.Errorf("无法解析主机名: %s", firstLineOf(stderr))
	case strings.Contains(lowered, "no matching key exchange"), strings.Contains(lowered, "no matching cipher"):
		return fmt.Errorf("SSH 协议协商失败: 目标机的算法与本机 ssh 客户端不兼容")
	}

	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("连接超时")
	}
	if stderr != "" {
		return fmt.Errorf("ssh 执行失败: %s", firstLineOf(stderr))
	}
	return fmt.Errorf("ssh 执行失败: %v", err)
}

func firstLineOf(text string) string {
	trimmed := strings.TrimSpace(text)
	if index := strings.Index(trimmed, "\n"); index != -1 {
		trimmed = trimmed[:index]
	}
	return truncateText(trimmed, 200)
}

// ---------------------------------------------------------------------------
// askpass
//
// 密码通过环境变量交给子进程, 由本程序在被 ssh 调用时输出来。
// 之所以这么做: 密码既不进 argv (ps 里能看到), 也不需要临时文件 (落盘)。
// ---------------------------------------------------------------------------

const (
	askpassEnvMarker     = "BRA_ASKPASS"
	askpassEnvSecret     = "BRA_ASKPASS_SECRET"
	askpassEnvPassphrase = "BRA_ASKPASS_PASSPHRASE"
	// askpassEnvRequire 是 OpenSSH 认的环境变量 (SSH_ASKPASS_REQUIRE), 不是配置项。
	askpassEnvRequire = "SSH_ASKPASS_REQUIRE"
)

// IsAskpassInvocation 报告当前进程是不是被 ssh 当作 askpass 助手调起的。
//
// 两道判断, 缺一不可: 标记 (我们自己在拉起 ssh 时设的) 与 SSH_ASKPASS 是否指向
// 本程序。只认标记的话, 任何漏设标记的路径都会让本程序误以为自己是服务端 ——
// 而它的表现是"再起一个实例并常驻", 不是报错, 很难被发现。
func IsAskpassInvocation() bool {
	if os.Getenv(askpassEnvMarker) == "1" {
		return true
	}
	return askpassIsSelf()
}

// askpassIsSelf 判断 SSH_ASKPASS 是否就是本程序。
func askpassIsSelf() bool {
	helper := strings.TrimSpace(os.Getenv("SSH_ASKPASS"))
	if helper == "" {
		return false
	}
	self, err := os.Executable()
	if err != nil || strings.TrimSpace(self) == "" {
		return false
	}
	selfInfo, err := os.Stat(self)
	if err != nil {
		return false
	}
	helperInfo, err := os.Stat(helper)
	if err != nil {
		return false
	}
	// 用 SameFile 而不是比字符串: 同一个程序经软链、相对路径、不同挂载点进来的
	// 路径写法都可能不同。
	return os.SameFile(selfInfo, helperInfo)
}

// RunAskpass 以 askpass 助手的身份运行: 把密码打印到标准输出后退出。
//
// ssh 会把提示语作为参数传进来; 登录密码与私钥口令的提示不同, 据此区分 ——
// 拿密码去解私钥会白费一次认证尝试, 反过来也一样。
func RunAskpass(args []string) int {
	if os.Getenv(askpassEnvMarker) != "1" {
		// 走到这里说明标记漏了, 靠 SSH_ASKPASS 兜底认出了自己。写一行到 stderr:
		// 正常路径不会有这条输出, 出现了就说明调用链被改坏了, 值得留下痕迹。
		fmt.Fprintln(os.Stderr, "RunStor Bot: 以 askpass 助手身份运行 (缺少 "+askpassEnvMarker+" 标记)")
	}
	fmt.Fprintln(os.Stdout, askpassSecretFor(strings.Join(args, " ")))
	return 0
}

// askpassSecretFor 按提示语挑选该回答哪一个秘密。
func askpassSecretFor(prompt string) string {
	lowered := strings.ToLower(prompt)
	if strings.Contains(lowered, "passphrase") {
		if passphrase := os.Getenv(askpassEnvPassphrase); passphrase != "" {
			return passphrase
		}
	}
	return os.Getenv(askpassEnvSecret)
}

// ensureTrailingNewline 保证私钥以换行结尾 (OpenSSH 对此敏感)。
func ensureTrailingNewline(value string) string {
	if strings.HasSuffix(value, "\n") {
		return value
	}
	return value + "\n"
}

// hostKeyFingerprint 从 known_hosts 里取出该主机的指纹。
//
// 用 `ssh-keygen -F` 查询而不是自己逐行比对: OpenSSH 默认把主机名**哈希**后存储
// (HashKnownHosts), 条目长这样 "|1|ms0i3hy...=|egnMzm...= ssh-ed25519 AAAA..." ——
// 按主机名比字段永远匹配不上, 表现为"指纹始终为空"。ssh-keygen -F 本身理解哈希条目。
//
// 取不到时返回空串: 报告里就不显示指纹, 而不是编一个出来。
func (r *sshRunner) hostKeyFingerprint() string {
	path := strings.TrimSpace(r.options.KnownHostsPath)
	if path == "" {
		return ""
	}
	if _, err := os.Stat(path); err != nil {
		return ""
	}
	keygen, err := exec.LookPath("ssh-keygen")
	if err != nil {
		return ""
	}

	// 注意: ssh-keygen -F 没有 -p 参数。非默认端口的条目在 known_hosts 里写作
	// "[host]:port", 查询时就把这个整体当作主机名传进去。
	lookup := r.target.Host
	if port := r.target.Port; port != 0 && port != 22 {
		lookup = fmt.Sprintf("[%s]:%d", r.target.Host, port)
	}
	args := []string{"-F", lookup, "-f", path}
	out, err := exec.Command(keygen, args...).Output()
	if err != nil && len(out) == 0 {
		return ""
	}

	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 || strings.HasPrefix(fields[0], "#") {
			continue
		}
		// ssh-keygen -F 会先打一行 "# Host xxx found: ..."
		if strings.HasPrefix(fields[0], "#") {
			continue
		}
		return computeFingerprint(fields[1] + " " + fields[2])
	}
	return ""
}

// computeFingerprint 用 ssh-keygen 算出 "SHA256:..." 形式的指纹。
//
// 自己实现指纹计算要引入哈希与 base64 细节, 而 ssh-keygen 必然随 ssh 一起存在
// (两者同属 OpenSSH 包), 复用它更稳。
func computeFingerprint(keyLine string) string {
	keygen := "ssh-keygen"
	if _, err := exec.LookPath(keygen); err != nil {
		return ""
	}
	command := exec.Command(keygen, "-l", "-f", "-")
	command.Stdin = strings.NewReader(keyLine + "\n")

	var out bytes.Buffer
	command.Stdout = &out
	if err := command.Run(); err != nil {
		return ""
	}
	// 输出形如 "256 SHA256:xxxx comment (ED25519)"
	fields := strings.Fields(out.String())
	for _, field := range fields {
		if strings.HasPrefix(field, "SHA256:") {
			return field
		}
	}
	return ""
}
