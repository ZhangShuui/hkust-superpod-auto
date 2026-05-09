package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

var (
	host       = envOr("SPOD_SSH_HOST", "superpod")
	tunnelPort = os.Getenv("TUNNEL_PORT") // empty → computed from remote UID in ensurePorts()
	localPort  = envOr("CLASH_PORT", "7897")
	socksPort  = envOr("SOCKS_PORT", "1080")
	relayPort  = os.Getenv("SPOD_RELAY_PORT") // empty → computed from remote UID in ensurePorts()
	prefix     = "spod"
	vpnScript  = "" // resolved in init()

	remoteUID int // lazily fetched via ssh("id -u"), cached on disk
	portsMu   sync.Mutex
)

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func init() {
	// Load .env from project root (walk up from executable or cwd)
	for _, base := range []string{os.Getenv("SPOD_ENV_FILE"), findDotenv()} {
		if base == "" {
			continue
		}
		data, err := os.ReadFile(base)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			k, v, ok := strings.Cut(line, "=")
			if !ok {
				continue
			}
			k, v = strings.TrimSpace(k), strings.TrimSpace(v)
			// Strip inline comments (outside quotes)
			quoted := len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[0] == v[len(v)-1]
			if !quoted {
				if i := strings.Index(v, "#"); i >= 0 {
					v = strings.TrimSpace(v[:i])
				}
			}
			// Strip matching quotes
			if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[0] == v[len(v)-1] {
				v = v[1 : len(v)-1]
			}
			if k != "" && os.Getenv(k) == "" {
				os.Setenv(k, v)
			}
		}
		// Re-read vars after loading .env
		host = envOr("SPOD_SSH_HOST", "superpod")
		tunnelPort = os.Getenv("TUNNEL_PORT")
		localPort = envOr("CLASH_PORT", "7897")
		socksPort = envOr("SOCKS_PORT", "1080")
		relayPort = os.Getenv("SPOD_RELAY_PORT")
		break
	}

	// Resolve VPN script path
	vpnScript = envOr("VPN_SCRIPT", "")
	if vpnScript == "" {
		// Try to find hkust-vpn.py relative to .env (resolve symlinks) or cwd
		candidates := []string{}
		if dotenv := findDotenv(); dotenv != "" {
			// Resolve symlinks to find the real project directory
			if real, err := filepath.EvalSymlinks(dotenv); err == nil {
				candidates = append(candidates, filepath.Dir(real))
			}
			candidates = append(candidates, filepath.Dir(dotenv))
		}
		cwd, _ := os.Getwd()
		candidates = append(candidates, cwd)
		for _, dir := range candidates {
			p := filepath.Join(dir, "hkust-vpn.py")
			if _, err := os.Stat(p); err == nil {
				vpnScript = p
				break
			}
		}
	}

	// Auto-generate/sync ~/.ssh/config for superpod from .env
	ensureSSHConfig()
}

func ensureSSHConfig() {
	sshUser := envOr("SUPERPOD_USER", "")
	sshHost := envOr("SUPERPOD_HOST", "superpod.ust.hk")
	if sshUser == "" {
		return
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	sshDir := filepath.Join(home, ".ssh")
	configPath := filepath.Join(sshDir, "config")

	// Desired config block
	desired := fmt.Sprintf(`Host superpod
    HostName %s
    User %s

    # 连接复用：避免多次 SSH 握手触发服务端限流
    ControlMaster auto
    ControlPath /tmp/spod-ssh-%%r@%%h:%%p
    ControlPersist 300

    # 心跳：每 15s 发一次，连续 4 次无响应才断（容忍 60s 网络抖动）
    ServerAliveInterval 15
    ServerAliveCountMax 4
    TCPKeepAlive yes`, sshHost, sshUser)

	// Read existing config
	existing, _ := os.ReadFile(configPath)
	content := string(existing)

	// Check if superpod block exists and is up to date
	if strings.Contains(content, "Host superpod") {
		// Check if config already matches desired
		if strings.Contains(content, "ControlMaster auto") &&
			strings.Contains(content, "User "+sshUser) {
			return // already correct
		}
		// Config outdated — replace the whole superpod block
		// Find block boundaries (from "Host superpod" to next "Host " or EOF)
		lines := strings.Split(content, "\n")
		var result []string
		inBlock := false
		for _, line := range lines {
			trimmed := strings.TrimSpace(line)
			if trimmed == "Host superpod" {
				inBlock = true
				continue
			}
			if inBlock && strings.HasPrefix(trimmed, "Host ") {
				inBlock = false
			}
			if inBlock {
				continue
			}
			result = append(result, line)
		}
		// Remove leading/trailing blank lines and append new block
		cleaned := strings.TrimSpace(strings.Join(result, "\n"))
		if cleaned != "" {
			cleaned += "\n\n"
		}
		content = cleaned + desired + "\n"
	} else {
		// No superpod block — append
		if content != "" && !strings.HasSuffix(content, "\n") {
			content += "\n"
		}
		if content != "" {
			content += "\n"
		}
		content += desired + "\n"
	}

	os.MkdirAll(sshDir, 0700)
	os.WriteFile(configPath, []byte(content), 0600)
}

func findDotenv() string {
	// 1. Try well-known config path
	if home, err := os.UserHomeDir(); err == nil {
		p := filepath.Join(home, ".config", "spod", ".env")
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	// 2. Try cwd, then walk up
	dir, _ := os.Getwd()
	for i := 0; i < 5 && dir != "/"; i++ {
		p := filepath.Join(dir, ".env")
		if _, err := os.Stat(p); err == nil {
			return p
		}
		dir = filepath.Dir(dir)
	}
	return ""
}

// ── Colors (256-color) ──

const (
	// 主色调
	cBlue   = "\033[38;5;75m"  // 亮蓝 — 信息
	cGreen  = "\033[38;5;114m" // 柔绿 — 成功
	cAmber  = "\033[38;5;221m" // 琥珀 — 警告
	cRed    = "\033[38;5;203m" // 珊瑚红 — 错误
	cPurple = "\033[38;5;141m" // 淡紫 — 强调
	cGray   = "\033[38;5;243m" // 灰 — 次要信息
	// 样式
	bold  = "\033[1m"
	dim   = "\033[2m"
	reset = "\033[0m"
)

func info(msg string) { fmt.Fprintf(os.Stderr, "  %s›%s %s\n", cBlue, reset, msg) }
func ok(msg string)   { fmt.Fprintf(os.Stderr, "  %s✓%s %s\n", cGreen, reset, msg) }
func warn(msg string) { fmt.Fprintf(os.Stderr, "  %s⚠%s %s\n", cAmber, reset, msg) }
func fail(msg string) { fmt.Fprintf(os.Stderr, "  %s✗%s %s\n", cRed, reset, msg) }

// ── Validation ──

var validName = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

func sanitizeName(name string) string {
	if !validName.MatchString(name) {
		fail(fmt.Sprintf("无效会话名: %q (只允许字母、数字、下划线、连字符)", name))
		os.Exit(1)
	}
	return name
}

// ── SSH/shell helpers ──

// isSSHConnErr returns true for SSH exit code 255 (connection-level failure).
func isSSHConnErr(err error) bool {
	var exitErr *exec.ExitError
	return errors.As(err, &exitErr) && exitErr.ExitCode() == 255
}

func ssh(args ...string) (string, error) {
	const maxRetries = 3
	delays := []time.Duration{2 * time.Second, 4 * time.Second, 8 * time.Second}

	// For multi-line scripts, use stdin instead of command argument.
	// SSH's remote bash -c has issues with heredocs passed as arguments.
	useStdin := len(args) == 1 && strings.Contains(args[0], "\n")

	for attempt := 0; ; attempt++ {
		var cmd *exec.Cmd
		if useStdin {
			cmd = exec.Command("ssh", "-o", "ConnectTimeout=5", host, "bash -s")
			cmd.Stdin = strings.NewReader(args[0])
		} else {
			cmd = exec.Command("ssh", append([]string{"-o", "ConnectTimeout=5", host}, args...)...)
		}
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		err := cmd.Run()
		if err == nil {
			return strings.TrimSpace(stdout.String()), nil
		}
		if isSSHConnErr(err) && attempt < maxRetries {
			warn(fmt.Sprintf("SSH 连接被重置，%v 后重试 (%d/%d)...", delays[attempt], attempt+1, maxRetries))
			time.Sleep(delays[attempt])
			continue
		}
		errMsg := strings.TrimSpace(stderr.String())
		if errMsg != "" {
			return strings.TrimSpace(stdout.String()), fmt.Errorf("%w: %s", err, errMsg)
		}
		return strings.TrimSpace(stdout.String()), err
	}
}

func sshInteractive(args ...string) error {
	const maxRetries = 3
	delays := []time.Duration{2 * time.Second, 4 * time.Second, 8 * time.Second}
	for attempt := 0; ; attempt++ {
		cmd := exec.Command("ssh", append([]string{"-t", "-o", "ConnectTimeout=10", host}, args...)...)
		cmd.Stdin = os.Stdin
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		err := cmd.Run()
		if err == nil || !isSSHConnErr(err) || attempt >= maxRetries {
			return err
		}
		warn(fmt.Sprintf("SSH 连接被重置，%v 后重试 (%d/%d)...", delays[attempt], attempt+1, maxRetries))
		time.Sleep(delays[attempt])
	}
}

// ── Tunnel ──

// tunnelPIDAndPort returns (pid, remote port) of the running autossh that
// reverse-forwards our localPort, or (0, "") if none. Matches any remote
// port — callers decide whether it's the expected one. Regex is anchored
// to the exact -R forward targeting 127.0.0.1:<localPort> so an autossh
// with multiple -R flags cannot yield a sibling forward's port.
func tunnelPIDAndPort() (int, string) {
	// Word boundary after the port prevents false match on a longer port
	// that shares our port as prefix (e.g. 78970 vs 7897).
	quotedLocal := regexp.QuoteMeta(localPort)
	pgrepPattern := `autossh.*-R [0-9]+:127\.0\.0\.1:` + quotedLocal + `([^0-9]|$)`
	procRE := regexp.MustCompile(`-R (\d+):127\.0\.0\.1:` + quotedLocal + `(?:[^0-9]|$)`)

	cmd := exec.Command("pgrep", "-f", pgrepPattern)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		return 0, ""
	}
	for _, line := range strings.Split(strings.TrimSpace(stdout.String()), "\n") {
		pid, err := strconv.Atoi(strings.TrimSpace(line))
		if err != nil || pid <= 0 {
			continue
		}
		cmdline, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
		if err != nil {
			continue // process already gone (pgrep self-match)
		}
		// /proc cmdline separates args with NUL; normalize for regex.
		normalized := strings.ReplaceAll(string(cmdline), "\x00", " ")
		if !strings.Contains(normalized, "autossh") {
			continue
		}
		m := procRE.FindStringSubmatch(normalized)
		if len(m) < 2 {
			continue
		}
		return pid, m[1]
	}
	return 0, ""
}

func tunnelPID() int {
	pid, _ := tunnelPIDAndPort()
	return pid
}

// waitForExit polls up to maxWait for pid to exit. Returns true if gone.
func waitForExit(pid int, maxWait time.Duration) bool {
	deadline := time.Now().Add(maxWait)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); err != nil {
			return true
		}
		time.Sleep(200 * time.Millisecond)
	}
	return false
}

// killTunnelPID sends SIGTERM, waits up to 5s, then SIGKILL if still alive.
func killTunnelPID(pid int) {
	syscall.Kill(pid, syscall.SIGTERM)
	if !waitForExit(pid, 5*time.Second) {
		syscall.Kill(pid, syscall.SIGKILL)
		waitForExit(pid, 2*time.Second)
	}
}

// uidCachePath returns a per-identity cache path keyed by SSH user@host.
// Keying by identity prevents a stale cached UID after the user switches
// SUPERPOD_USER or SUPERPOD_HOST to a different account.
func uidCachePath() string {
	cacheDir, _ := os.UserCacheDir()
	if cacheDir == "" {
		return ""
	}
	sshUser := envOr("SUPERPOD_USER", "")
	sshHost := envOr("SUPERPOD_HOST", host)
	key := sshHost
	if sshUser != "" {
		key = sshUser + "@" + sshHost
	}
	safe := regexp.MustCompile(`[^a-zA-Z0-9._@-]`).ReplaceAllString(key, "_")
	return filepath.Join(cacheDir, "spod", "remote-uid-"+safe)
}

// getRemoteUID fetches the SuperPod UID via `ssh id -u`, caching the result
// in memory and on disk to avoid repeated SSH.
func getRemoteUID() int {
	portsMu.Lock()
	defer portsMu.Unlock()
	if remoteUID > 0 {
		return remoteUID
	}
	cachePath := uidCachePath()
	if cachePath != "" {
		if data, err := os.ReadFile(cachePath); err == nil {
			if uid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil && uid > 0 {
				remoteUID = uid
				return uid
			}
		}
	}
	out, err := ssh("id -u")
	if err != nil {
		return 0
	}
	uid, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil || uid <= 0 {
		return 0
	}
	remoteUID = uid
	if cachePath != "" {
		os.MkdirAll(filepath.Dir(cachePath), 0700)
		os.WriteFile(cachePath, []byte(strconv.Itoa(uid)), 0600)
	}
	return uid
}

// ensurePorts computes per-user tunnelPort / relayPort from the remote UID.
// Env vars TUNNEL_PORT and SPOD_RELAY_PORT take precedence if set.
// Uses separate bases (17000 / 18000) so tunnel and relay ports can never
// collide across users even if their UIDs share the same %1000 bucket.
func ensurePorts() {
	if tunnelPort != "" && relayPort != "" {
		return
	}
	uid := getRemoteUID()
	if uid == 0 {
		// UID lookup failed (SSH down); fall back to legacy shared defaults
		// so single-user setups still work.
		if tunnelPort == "" {
			tunnelPort = "17897"
		}
		if relayPort == "" {
			relayPort = "18897"
		}
		return
	}
	if tunnelPort == "" {
		tunnelPort = strconv.Itoa(17000 + uid%1000)
	}
	if relayPort == "" {
		relayPort = strconv.Itoa(18000 + uid%1000)
	}
}

func ensureVPN() {
	if vpnTunnelUp() {
		return
	}
	fail("VPN 未连接 — tun0 不存在")
	info("运行 `spod vpn` 启动 VPN")
	os.Exit(1)
}

func ensureTunnel() {
	ensureVPN()
	// Lockfile to prevent concurrent tunnel starts
	lockPath := filepath.Join(os.TempDir(), "spod-tunnel.lock")
	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_WRONLY, 0600)
	if err == nil {
		if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX); err != nil {
			warn(fmt.Sprintf("无法获取锁: %v", err))
		}
		defer func() {
			syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN)
			lockFile.Close()
		}()
	}

	ensurePorts()

	// Clean up any stale autossh whose -R port doesn't match our expected
	// tunnelPort. Loops so that multiple stale instances are all reaped,
	// and re-scans after each kill to confirm the PID is gone before
	// starting a new autossh (ExitOnForwardFailure=yes is unforgiving if
	// the old ssh child still holds the remote bind).
	killed := 0
	for {
		pid, runningPort := tunnelPIDAndPort()
		if pid == 0 || runningPort == tunnelPort {
			break
		}
		warn(fmt.Sprintf("旧隧道端口 %s ≠ 预期 %s，清理 pid=%d", runningPort, tunnelPort, pid))
		killTunnelPID(pid)
		killed++
		if killed >= 5 {
			warn("清理旧隧道超过 5 次，放弃")
			break
		}
	}
	if killed > 0 {
		// Give remote sshd a moment to release the -R port binding before
		// the new autossh tries to rebind.
		time.Sleep(2 * time.Second)
	}

	pid, _ := tunnelPIDAndPort()
	if pid > 0 {
		// PID exists and matches expected port — verify the underlying
		// SSH connection is still alive
		probe := exec.Command("ssh", "-o", "ConnectTimeout=3", "-o", "BatchMode=yes", host, "true")
		if err := probe.Run(); err != nil {
			warn(fmt.Sprintf("隧道进程存在 (pid=%d) 但 SSH 连接已断，重建...", pid))
			killTunnelPID(pid)
			time.Sleep(2 * time.Second)
		} else {
			ok(fmt.Sprintf("隧道运行中 (pid=%d, port=%s)", pid, tunnelPort))
			return
		}
	}

	info(fmt.Sprintf("启动隧道 (SuperPod:%s → 本地:%s)...", tunnelPort, localPort))

	logPath := filepath.Join(os.TempDir(), "spod-tunnel.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		warn(fmt.Sprintf("无法打开日志文件: %v", err))
	}

	cmd := exec.Command("autossh", "-M", "0", "-f", "-N",
		"-o", "ServerAliveInterval=15",
		"-o", "ServerAliveCountMax=4",
		"-o", "ExitOnForwardFailure=yes",
		"-o", "TCPKeepAlive=yes",
		"-o", "ControlMaster=no",
		"-R", fmt.Sprintf("%s:127.0.0.1:%s", tunnelPort, localPort),
		host,
	)
	if logFile != nil {
		cmd.Stdout = logFile
		cmd.Stderr = logFile
	}
	if err := cmd.Run(); err != nil {
		fail(fmt.Sprintf("隧道启动失败: %v", err))
		fail("检查 VPN 和 Clash 是否在运行")
		if logFile != nil {
			fail(fmt.Sprintf("日志: %s", logPath))
			logFile.Close()
		}
		os.Exit(1)
	}
	if logFile != nil {
		logFile.Close()
	}

	// Poll until tunnel process appears (max 10s)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if tunnelPID() > 0 {
			ok("隧道已建立")
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	fail("隧道启动超时")
	fail(fmt.Sprintf("日志: %s", logPath))
	os.Exit(1)
}

func stopTunnel() {
	pid := tunnelPID()
	if pid == 0 {
		warn("隧道未运行")
		return
	}
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		warn(fmt.Sprintf("进程 %d 已不存在", pid))
		return
	}
	// 等待进程退出
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); err != nil {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	ok(fmt.Sprintf("隧道已关闭 (pid=%d)", pid))
}

// ── SOCKS proxy ──

// socksPID finds the autossh process managing our SOCKS proxy.
func socksPID() int {
	cmd := exec.Command("pgrep", "-f", "autossh.*-D 0.0.0.0:"+socksPort)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		return 0
	}
	out := strings.TrimSpace(stdout.String())
	if out == "" {
		return 0
	}
	for _, line := range strings.Split(out, "\n") {
		pid, err := strconv.Atoi(strings.TrimSpace(line))
		if err != nil || pid <= 0 {
			continue
		}
		cmdline, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
		if err != nil {
			continue
		}
		if strings.Contains(string(cmdline), "autossh") {
			return pid
		}
	}
	return 0
}

func ensureSocks() {
	ensureVPN()
	lockPath := filepath.Join(os.TempDir(), "spod-socks.lock")
	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_WRONLY, 0600)
	if err == nil {
		if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX); err != nil {
			warn(fmt.Sprintf("无法获取锁: %v", err))
		}
		defer func() {
			syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN)
			lockFile.Close()
		}()
	}

	pid := socksPID()
	if pid > 0 {
		ok(fmt.Sprintf("SOCKS5 代理运行中 (pid=%d, 0.0.0.0:%s)", pid, socksPort))
		return
	}

	info(fmt.Sprintf("启动 SOCKS5 代理 (0.0.0.0:%s → SuperPod)...", socksPort))

	logPath := filepath.Join(os.TempDir(), "spod-socks.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		warn(fmt.Sprintf("无法打开日志文件: %v", err))
	}

	cmd2 := exec.Command("autossh", "-M", "0", "-f", "-N",
		"-o", "ServerAliveInterval=15",
		"-o", "ServerAliveCountMax=4",
		"-o", "ExitOnForwardFailure=yes",
		"-o", "TCPKeepAlive=yes",
		"-o", "ControlMaster=no",
		"-D", fmt.Sprintf("0.0.0.0:%s", socksPort),
		host,
	)
	if logFile != nil {
		cmd2.Stdout = logFile
		cmd2.Stderr = logFile
	}
	if err := cmd2.Run(); err != nil {
		fail(fmt.Sprintf("SOCKS5 代理启动失败: %v", err))
		fail("检查 VPN 是否在运行")
		if logFile != nil {
			fail(fmt.Sprintf("日志: %s", logPath))
			logFile.Close()
		}
		os.Exit(1)
	}
	if logFile != nil {
		logFile.Close()
	}

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if socksPID() > 0 {
			ok(fmt.Sprintf("SOCKS5 代理已建立 (0.0.0.0:%s)", socksPort))
			info("Windows 侧设置 SOCKS5 代理: 127.0.0.1:" + socksPort)
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	fail("SOCKS5 代理启动超时")
	fail(fmt.Sprintf("日志: %s", logPath))
	os.Exit(1)
}

func stopSocks() {
	pid := socksPID()
	if pid == 0 {
		warn("SOCKS5 代理未运行")
		return
	}
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		warn(fmt.Sprintf("进程 %d 已不存在", pid))
		return
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); err != nil {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	ok(fmt.Sprintf("SOCKS5 代理已关闭 (pid=%d)", pid))
}

func socksStatus() {
	pid := socksPID()
	if pid > 0 {
		ok(fmt.Sprintf("SOCKS5 代理运行中 (pid=%d, 0.0.0.0:%s)", pid, socksPort))
		// Check if port is actually listening
		conn, err := net.DialTimeout("tcp", "127.0.0.1:"+socksPort, 2*time.Second)
		if err != nil {
			warn("端口未监听 — 进程可能卡住，尝试 `spod socks stop` 后重启")
		} else {
			conn.Close()
			ok("端口监听正常")
		}
	} else {
		warn("SOCKS5 代理未运行")
	}
}

// ── VS Code (Windows Remote-SSH) ──

// findWindowsUser returns the Windows username by looking at /mnt/c/Users/.
func findWindowsUser() string {
	entries, err := os.ReadDir("/mnt/c/Users")
	if err != nil {
		return ""
	}
	skip := map[string]bool{"Public": true, "Default": true, "Default User": true, "All Users": true}
	for _, e := range entries {
		if !e.IsDir() || skip[e.Name()] || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		// Check if it has a real profile (has Desktop folder)
		if _, err := os.Stat(filepath.Join("/mnt/c/Users", e.Name(), "Desktop")); err == nil {
			return e.Name()
		}
	}
	return ""
}

// findConnectExe searches common locations for connect.exe (Git for Windows).
func findConnectExe() string {
	candidates := []string{
		`C:\Program Files\Git\mingw64\bin\connect.exe`,
		`C:\Program Files (x86)\Git\mingw64\bin\connect.exe`,
	}
	for _, c := range candidates {
		wslPath := "/mnt/c/" + strings.ReplaceAll(strings.TrimPrefix(c, `C:\`), `\`, "/")
		if _, err := os.Stat(wslPath); err == nil {
			return c
		}
	}
	return ""
}

func cmdVscode() {
	// 1. Ensure SOCKS proxy is running
	ensureSocks()

	// 2. Find Windows user
	winUser := findWindowsUser()
	if winUser == "" {
		fail("无法检测 Windows 用户名（/mnt/c/Users/ 下找不到用户目录）")
		os.Exit(1)
	}
	winSSHDir := filepath.Join("/mnt/c/Users", winUser, ".ssh")
	winConfigPath := filepath.Join(winSSHDir, "config")

	// 3. Get SuperPod internal IP
	info("查询 SuperPod 内网 IP...")
	internalIP, err := ssh("hostname -I | awk '{print $1}'")
	if err != nil || internalIP == "" {
		fail(fmt.Sprintf("无法获取 SuperPod 内网 IP: %v", err))
		os.Exit(1)
	}
	ok(fmt.Sprintf("内网 IP: %s", internalIP))

	// 4. Find connect.exe
	connectExe := findConnectExe()
	if connectExe == "" {
		fail("找不到 connect.exe（需要安装 Git for Windows）")
		info("下载: https://git-scm.com/download/win")
		os.Exit(1)
	}

	// 5. Ensure Windows SSH key exists and is authorized on SuperPod
	sshUser := envOr("SUPERPOD_USER", "")
	winPubKeyPath := filepath.Join(winSSHDir, "id_ed25519.pub")
	if _, err := os.Stat(winPubKeyPath); err != nil {
		// Try RSA
		winPubKeyPath = filepath.Join(winSSHDir, "id_rsa.pub")
	}
	if pubKey, err := os.ReadFile(winPubKeyPath); err == nil {
		key := strings.TrimSpace(string(pubKey))
		// Check if already authorized
		out, _ := ssh("grep -cF '" + strings.Split(key, " ")[1] + "' ~/.ssh/authorized_keys 2>/dev/null")
		if out == "0" || out == "" {
			info("添加 Windows SSH 公钥到 SuperPod...")
			if _, err := ssh("mkdir -p ~/.ssh && chmod 700 ~/.ssh && echo '" + key + "' >> ~/.ssh/authorized_keys && chmod 600 ~/.ssh/authorized_keys"); err != nil {
				warn(fmt.Sprintf("公钥添加失败: %v（可能需要手动添加）", err))
			} else {
				ok("Windows SSH 公钥已添加")
			}
		} else {
			ok("Windows SSH 公钥已授权")
		}
	} else {
		warn(fmt.Sprintf("找不到 Windows SSH 公钥 (%s)，VS Code 连接时可能需要密码", winSSHDir))
		info("建议在 Windows PowerShell 中运行: ssh-keygen -t ed25519")
	}

	// 6. Write/update Windows SSH config
	desired := fmt.Sprintf(`Host superpod
    HostName %s
    User %s
    ProxyCommand "%s" -S 127.0.0.1:%s %%h %%p
    ServerAliveInterval 15
    ServerAliveCountMax 4`, internalIP, sshUser, connectExe, socksPort)

	os.MkdirAll(winSSHDir, 0700)
	existing, _ := os.ReadFile(winConfigPath)
	content := string(existing)

	if strings.Contains(content, "Host superpod") {
		// Replace existing block
		lines := strings.Split(content, "\n")
		var result []string
		inBlock := false
		for _, line := range lines {
			trimmed := strings.TrimSpace(line)
			if trimmed == "Host superpod" || strings.HasPrefix(trimmed, "Host superpod ") ||
				trimmed == "Host superpod.ust.hk superpod" {
				inBlock = true
				continue
			}
			if inBlock && (strings.HasPrefix(trimmed, "Host ") || trimmed == "") {
				if trimmed == "" {
					continue // skip blank lines after block
				}
				inBlock = false
			}
			if !inBlock {
				result = append(result, line)
			}
		}
		cleaned := strings.TrimRight(strings.Join(result, "\n"), "\n\r\t ")
		if cleaned != "" {
			cleaned += "\n\n"
		}
		content = cleaned + desired + "\n"
	} else {
		if content != "" && !strings.HasSuffix(content, "\n") {
			content += "\n"
		}
		if content != "" {
			content += "\n"
		}
		content += desired + "\n"
	}

	if err := os.WriteFile(winConfigPath, []byte(content), 0600); err != nil {
		fail(fmt.Sprintf("写入 Windows SSH 配置失败: %v", err))
		os.Exit(1)
	}
	ok(fmt.Sprintf("Windows SSH 配置已写入 C:\\Users\\%s\\.ssh\\config", winUser))

	fmt.Println()
	info("VS Code 连接方式:")
	info("  1. 安装 Remote-SSH 扩展")
	info("  2. Ctrl+Shift+P → Remote-SSH: Connect to Host → superpod")
	info(fmt.Sprintf("  （确保 WSL 中 SOCKS 代理运行: spod socks）"))
}

// ── VPN ──

var vpnPIDFile = filepath.Join(os.TempDir(), "spod-vpn.pid")

func vpnPID() int {
	data, err := os.ReadFile(vpnPIDFile)
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return 0
	}
	// Verify process is alive and is python
	cmdline, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		os.Remove(vpnPIDFile)
		return 0
	}
	if !strings.Contains(string(cmdline), "hkust-vpn") {
		os.Remove(vpnPIDFile)
		return 0
	}
	return pid
}

func vpnTunnelUp() bool {
	// Cheap check: tun0 interface exists. No network traffic.
	_, err := net.InterfaceByName("tun0")
	return err == nil
}

func vpnIsUp() bool {
	// Expensive check: TCP connect to SuperPod :22. Use sparingly.
	if !vpnTunnelUp() {
		return false
	}
	conn, err := net.DialTimeout("tcp", envOr("SUPERPOD_HOST", "superpod.ust.hk")+":22", 5*time.Second)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

func cmdVpnStart() {
	if vpnScript == "" {
		fail("找不到 hkust-vpn.py，设置 VPN_SCRIPT 环境变量或在项目目录运行")
		os.Exit(1)
	}

	pid := vpnPID()
	if pid > 0 {
		ok(fmt.Sprintf("VPN 已在运行 (pid=%d)", pid))
		return
	}

	info("启动 VPN (headless, auto-reconnect)...")

	logPath := filepath.Join(filepath.Dir(vpnScript), "vpn.log")

	// Use venv python if available, otherwise system python3
	scriptDir := filepath.Dir(vpnScript)
	pythonBin := "python3"
	venvPython := filepath.Join(scriptDir, ".venv", "bin", "python3")
	if _, err := os.Stat(venvPython); err == nil {
		pythonBin = venvPython
	}

	cmd := exec.Command(pythonBin, vpnScript, "--headless")
	cmd.Dir = scriptDir
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} // detach from terminal

	// Redirect stdout/stderr to log (Python also logs to file, but capture startup errors)
	logFile, _ := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if logFile != nil {
		cmd.Stdout = logFile
		cmd.Stderr = logFile
	}

	if err := cmd.Start(); err != nil {
		fail(fmt.Sprintf("VPN 启动失败: %v", err))
		os.Exit(1)
	}

	// Save PID
	os.WriteFile(vpnPIDFile, []byte(strconv.Itoa(cmd.Process.Pid)), 0600)

	// Don't wait for the process — it runs in background
	go cmd.Wait()

	// Open a persistent status pane via Windows Terminal split
	spodBin, _ := os.Executable()
	if spodBin == "" {
		spodBin = "/home/shurui/.local/bin/spod"
	}

	// Try opening a separate small Windows Terminal window (WSL2)
	if wtBin, err := exec.LookPath("wt.exe"); err == nil {
		wtCmd := exec.Command(wtBin, "-w", "_",
			"--size", "30,8",
			"--pos", "9999,9999",
			"--title", "VPN Status",
			"wsl.exe", "--", "bash", "-lc", spodBin+" vpn watch")
		if err := wtCmd.Start(); err == nil {
			go wtCmd.Wait()
			info(fmt.Sprintf("VPN 启动中 (pid=%d)，状态窗口已打开", cmd.Process.Pid))
			if logFile != nil {
				logFile.Close()
			}
			return
		}
	}

	// Fallback: inline spinner — check tun0 only, no TCP :22 spam
	info("等待 VPN 隧道建立...")
	deadline := time.Now().Add(90 * time.Second)
	spin := []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
	i := 0
	for time.Now().Before(deadline) {
		if vpnTunnelUp() {
			fmt.Printf("\r\033[2K")
			ok("\\(ᵔᵕᵔ)/  VPN 隧道已建立！")
			if logFile != nil {
				logFile.Close()
			}
			return
		}
		fmt.Printf("\r  %s ( •_•) 连接中...", spin[i%len(spin)])
		i++
		time.Sleep(500 * time.Millisecond)
	}
	fmt.Printf("\r\033[2K")
	warn("( ×_×)  VPN 进程已启动但连接尚未就绪")
	info(fmt.Sprintf("日志: %s", logPath))
	if logFile != nil {
		logFile.Close()
	}
}

func cmdVpnStop() {
	pid := vpnPID()
	if pid == 0 {
		warn("VPN 未运行")
		return
	}
	// Kill the entire process group (openconnect runs as child)
	syscall.Kill(-pid, syscall.SIGTERM)
	syscall.Kill(pid, syscall.SIGTERM)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); err != nil {
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	os.Remove(vpnPIDFile)
	ok(fmt.Sprintf("VPN 已停止 (pid=%d)", pid))
}

func cmdVpnRestart() {
	info("重启 VPN...")
	cmdVpnStop()
	// Wait for tun0 to be cleaned up
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := net.InterfaceByName("tun0"); err != nil {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	cmdVpnStart()
}

func cmdVpnStatus() {
	pid := vpnPID()
	if pid > 0 {
		ok(fmt.Sprintf("VPN 进程运行中 (pid=%d)", pid))
	} else {
		warn("VPN 进程未运行")
	}
	if vpnIsUp() {
		ok(fmt.Sprintf("SuperPod 可达 (%s:22)", envOr("SUPERPOD_HOST", "superpod.ust.hk")))
	} else {
		fail(fmt.Sprintf("SuperPod 不可达 (%s:22)", envOr("SUPERPOD_HOST", "superpod.ust.hk")))
	}
}

func cmdVpnWidget() {
	// Compact one-line status for shell prompt / tmux
	// --color flag enables ANSI colors
	color := len(os.Args) > 3 && os.Args[3] == "--color"
	pid := vpnPID()
	if pid > 0 && vpnTunnelUp() {
		if color {
			fmt.Print("\033[32mᕕ(ᐛ)ᕗ ✔\033[0m")
		} else {
			fmt.Print("ᕕ(ᐛ)ᕗ ✔")
		}
	} else if pid > 0 {
		if color {
			fmt.Print("\033[33m(•_•) …\033[0m")
		} else {
			fmt.Print("(•_•) …")
		}
	} else {
		if color {
			fmt.Print("\033[31m(×_×) ✘\033[0m")
		} else {
			fmt.Print("(×_×) ✘")
		}
	}
}

func cmdVpnWatch() {
	// Persistent animated VPN status panel (runs in a split pane)
	type frame struct {
		art   string
		label string
	}
	connecting := []frame{
		{"    ( •_•)     ", "准备连接"},
		{"    ( •_•)>    ", "输入邮箱"},
		{"    (⌐■_■)    ", "输入密码"},
		{"   ( ˘▽˘)っ♨  ", "TOTP 验证"},
		{"   ᕕ( ᐛ )ᕗ   ", "建立隧道"},
		{"   ᕕ( ᐛ )ᕗ   ", "DNS 修复"},
		{"   ᕕ( ᐛ )ᕗ   ", "DNS 查询中..."},
	}
	celebrateFrame := frame{"   \\(ᵔᵕᵔ)/   ", "VPN 已连接"}
	idleFrames := []frame{
		{"    (￣▽￣)    ", "VPN 在线~"},
		{"    (・ω・)    ", "VPN 在线~"},
		{"    (ᵕ‿ᵕ)     ", "VPN 在线~"},
		{"    (◕‿◕)     ", "VPN 在线~"},
	}
	disconnectedFrame := frame{"    ( ×_×)    ", " VPN 未连接"}

	spin := []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
	bars := []string{"○ ○ ○ ○ ○", "● ○ ○ ○ ○", "● ● ○ ○ ○", "● ● ● ○ ○", "● ● ● ● ○", "● ● ● ● ◐", "● ● ● ● ◐"}

	// Parse VPN log to detect current step
	logPath := ""
	if vpnScript != "" {
		logPath = filepath.Join(filepath.Dir(vpnScript), "vpn.log")
	}

	currentStep := 0
	dnsDetail := ""
	var mu sync.Mutex

	if logPath != "" {
		tailCmd := exec.Command("tail", "-n", "0", "-f", logPath)
		tailOut, _ := tailCmd.StdoutPipe()
		tailCmd.Start()
		defer tailCmd.Process.Kill()
		go func() {
			scanner := bufio.NewScanner(tailOut)
			for scanner.Scan() {
				line := scanner.Text()
				mu.Lock()
				switch {
				case strings.Contains(line, "Step 1/5"):
					currentStep = 1
				case strings.Contains(line, "Step 2/5"):
					currentStep = 2
				case strings.Contains(line, "Step 3/5"), strings.Contains(line, "Step 4/5"):
					currentStep = 3
				case strings.Contains(line, "Step 5/5"), strings.Contains(line, "Connecting VPN"):
					currentStep = 4
				case strings.Contains(line, "DNS fix") && strings.Contains(line, "cached"):
					currentStep = 5
					dnsDetail = "缓存命中"
				case strings.Contains(line, "DNS fix") && strings.Contains(line, "waiting"):
					currentStep = 6
					// Extract retry info from log like "waiting for host (3/15)..."
					if idx := strings.Index(line, "("); idx >= 0 {
						if end := strings.Index(line[idx:], ")"); end >= 0 {
							dnsDetail = "重试 " + line[idx:idx+end+1]
						}
					}
				case strings.Contains(line, "DNS fix") && strings.Contains(line, "route via tun0"):
					currentStep = 7 // DNS done
					dnsDetail = ""
				case strings.Contains(line, "DNS fix") && strings.Contains(line, "resolving"):
					currentStep = 6
					dnsDetail = "查询中"
				case strings.Contains(line, "DNS fix"):
					currentStep = 5
				case strings.Contains(line, "session #"):
					currentStep = 0 // reset on reconnect
				}
				mu.Unlock()
			}
		}()
	}

	// Background connectivity probe — tun0 check is cheap (no network traffic).
	// TCP :22 probe uses exponential backoff in BOTH directions:
	//   success: 30s → 60s → ... → 5min  (slow down when stable)
	//   failure: 30s → 60s → ... → 5min  (avoid hammering overloaded login service)
	// Resets to 30s only when tun0 transitions from down→up (fresh connection).
	var isUp bool
	var lastTCPCheck time.Time
	var wasTunnelUp bool
	var tcpInterval = 30 * time.Second
	const tcpIntervalMax = 5 * time.Minute
	go func() {
		for {
			tunnelUp := vpnTunnelUp()
			up := false
			justConnected := tunnelUp && !wasTunnelUp
			if justConnected {
				// Fresh VPN connection — reset to fast probe for quick feedback
				tcpInterval = 30 * time.Second
			}
			if tunnelUp && (justConnected || time.Since(lastTCPCheck) >= tcpInterval) {
				up = vpnIsUp()
				lastTCPCheck = time.Now()
				// Exponential backoff regardless of result — both success and failure
				// back off to reduce SSH connection pressure on SuperPod login nodes
				tcpInterval = min(tcpInterval*2, tcpIntervalMax)
			} else if tunnelUp {
				mu.Lock()
				up = isUp
				mu.Unlock()
			}
			wasTunnelUp = tunnelUp
			if !tunnelUp {
				tcpInterval = 30 * time.Second
			}
			mu.Lock()
			isUp = up
			mu.Unlock()
			time.Sleep(5 * time.Second)
		}
	}()

	// Hide cursor
	fmt.Print("\033[?25l")
	defer fmt.Print("\033[?25h")

	spinIdx := 0
	var connectedSince time.Time
	wasUp := false
	for {
		pid := vpnPID()
		mu.Lock()
		up := isUp
		s := currentStep
		dd := dnsDetail
		mu.Unlock()

		// Track when connection was established
		if up && !wasUp {
			connectedSince = time.Now()
		}
		wasUp = up

		// Draw — cursor home + clear
		fmt.Print("\033[H\033[2J")

		if pid > 0 && up {
			var f frame
			if time.Since(connectedSince) < 15*time.Second {
				f = celebrateFrame
			} else {
				f = idleFrames[(spinIdx/15)%len(idleFrames)]
			}
			fmt.Printf("\n  \033[32m%s\033[0m\n", f.art)
			fmt.Printf("  \033[32m  ● ● ● ● ●  \033[0m\n\n")
			label := f.label
			if s == 6 && dd != "" {
				label += fmt.Sprintf("  \033[33m(DNS %s)\033[0m", dd)
			}
			fmt.Printf("  \033[32m  %s\033[0m\n", label)
		} else if pid > 0 {
			if s >= len(connecting) {
				s = len(connecting) - 1
			}
			f := connecting[s]
			fmt.Printf("\n  \033[33m%s\033[0m\n", f.art)
			fmt.Printf("  \033[33m  %s %s  \033[0m\n\n", spin[spinIdx%len(spin)], bars[s])
			fmt.Printf("  \033[33m  %s\033[0m\n", f.label)
		} else {
			f := disconnectedFrame
			fmt.Printf("\n  \033[31m%s\033[0m\n", f.art)
			fmt.Printf("  \033[31m  ○ ○ ○ ○ ○  \033[0m\n\n")
			fmt.Printf("  \033[31m  %s\033[0m\n", f.label)
		}

		spinIdx++
		time.Sleep(200 * time.Millisecond)
	}
}

func cmdVpnLog() {
	logPath := filepath.Join(filepath.Dir(vpnScript), "vpn.log")
	cmd := exec.Command("tail", "-f", "-n", "50", logPath)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Run()
}

// ── Sync ──

const spodSyncTag = "spod-sync" // marker for pkill

func cmdSync(remotePath, localPath string) {
	if remotePath == "" || localPath == "" {
		fail("用法: spod sync <remote_path> <local_path>")
		info("示例: spod sync /project/data/train ./data/")
		info("示例: spod sync /home/user/results /mnt/e/results/")
		os.Exit(1)
	}

	sshUser := envOr("SUPERPOD_USER", "")
	if sshUser == "" {
		fail("需要设置 SUPERPOD_USER（在 .env 或环境变量中）")
		os.Exit(1)
	}
	superpodHost := envOr("SUPERPOD_HOST", "superpod.ust.hk")
	src := sshUser + "@" + superpodHost + ":" + remotePath
	dst := localPath

	// Ensure local dir exists
	if err := os.MkdirAll(dst, 0755); err != nil {
		fail(fmt.Sprintf("创建目录失败: %v", err))
		os.Exit(1)
	}

	// List remote subdirectories for parallel sync
	info(fmt.Sprintf("扫描远程目录 %s ...", remotePath))
	dirList, err := ssh(fmt.Sprintf("ls -1d %s/*/ 2>/dev/null | xargs -n1 basename", remotePath))

	rsyncArgs := []string{
		"-rlP", "--partial", "--inplace", "--no-times", "--no-perms",
		fmt.Sprintf("--info=name0,progress2"), // compact progress
	}

	if err != nil || dirList == "" {
		// No subdirectories or ls failed — sync the whole path as one job
		info("单路 rsync...")
		args := append(rsyncArgs, src+"/", dst)
		cmd := exec.Command("rsync", args...)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		cmd.Env = append(os.Environ(), "SPOD_SYNC_TAG="+spodSyncTag)
		if err := cmd.Run(); err != nil {
			fail(fmt.Sprintf("rsync 失败: %v", err))
			os.Exit(1)
		}
		ok("同步完成")
		return
	}

	// Parallel sync each subdirectory
	dirs := strings.Fields(dirList)
	info(fmt.Sprintf("启动 %d 路并行 rsync...", len(dirs)))

	for _, dir := range dirs {
		args := append(append([]string{}, rsyncArgs...), src+"/"+dir, dst)
		cmd := exec.Command("rsync", args...)
		cmd.Env = append(os.Environ(), "SPOD_SYNC_TAG="+spodSyncTag)
		cmd.Start()
	}

	time.Sleep(3 * time.Second)

	out, _ := exec.Command("bash", "-c", "pgrep -fc 'rsync.*-rlP' || echo 0").CombinedOutput()
	count := strings.TrimSpace(string(out))
	ok(fmt.Sprintf("%s 路 rsync 运行中", count))
	info(fmt.Sprintf("查看进度: du -sh %s", dst))
	info("停止所有: spod sync stop")
}

func cmdSyncStop() {
	exec.Command("pkill", "-f", "rsync.*-rlP").Run()
	ok("已停止所有 rsync")
}

// ── Speedtest ──

func cmdSpeedtest(durationStr string) {
	duration := 60
	if durationStr != "" {
		if d, err := strconv.Atoi(durationStr); err == nil && d > 0 {
			duration = d
		}
	}

	// Read initial tun0 RX bytes
	readRX := func() int64 {
		data, err := os.ReadFile("/proc/net/dev")
		if err != nil {
			return 0
		}
		for _, line := range strings.Split(string(data), "\n") {
			if strings.Contains(line, "tun0") {
				fields := strings.Fields(strings.TrimSpace(line))
				if len(fields) >= 2 {
					// fields[0] is "tun0:", fields[1] is RX bytes
					val := strings.TrimSuffix(fields[0], ":")
					_ = val
					rx, _ := strconv.ParseInt(fields[1], 10, 64)
					return rx
				}
			}
		}
		return 0
	}

	rx1 := readRX()
	if rx1 == 0 {
		fail("tun0 接口不存在，VPN 未连接？")
		os.Exit(1)
	}

	info(fmt.Sprintf("测速中... (%ds)", duration))
	time.Sleep(time.Duration(duration) * time.Second)

	rx2 := readRX()
	bytes := rx2 - rx1
	mb := float64(bytes) / 1024 / 1024
	speed := mb / float64(duration)

	ok(fmt.Sprintf("%ds 接收: %.1f MB", duration, mb))
	ok(fmt.Sprintf("平均速度: %.2f MB/s", speed))
}

// ── Sessions ──

type session struct {
	name     string
	windows  string
	attached bool
}

// listRemoteSessions returns remote tmux sessions.
// Returns (nil, nil) when tmux has no sessions.
// Returns (nil, error) when SSH itself fails or remote command errors.
func listRemoteSessions() ([]session, error) {
	out, err := ssh(`tmux ls -F "#{session_name} #{session_windows} #{session_attached}"`)
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			switch exitErr.ExitCode() {
			case 1:
				// tmux exit 1 = no server running / no sessions
				return nil, nil
			case 255:
				return nil, fmt.Errorf("无法连接到远程主机: %w", err)
			default:
				return nil, fmt.Errorf("远程命令失败 (exit %d): %w", exitErr.ExitCode(), err)
			}
		}
		return nil, fmt.Errorf("执行失败: %w", err)
	}
	if out == "" {
		return nil, nil
	}
	var sessions []session
	for _, line := range strings.Split(out, "\n") {
		parts := strings.Fields(line)
		if len(parts) < 3 || !strings.HasPrefix(parts[0], prefix) {
			continue
		}
		sessions = append(sessions, session{
			name:     parts[0],
			windows:  parts[1],
			attached: parts[2] != "0",
		})
	}
	return sessions, nil
}

var cachedSessions []session
var sessionsCached bool

func mustListSessions() []session {
	if sessionsCached {
		return cachedSessions
	}
	sessions, err := listRemoteSessions()
	if err != nil {
		fail(err.Error())
		os.Exit(1)
	}
	cachedSessions = sessions
	sessionsCached = true
	return sessions
}

func nextName() string {
	sessions := mustListSessions()
	existing := make(map[string]bool)
	max := 0
	for _, s := range sessions {
		existing[s.name] = true
		numStr := strings.TrimPrefix(s.name, prefix+"-")
		if n, err := strconv.Atoi(numStr); err == nil && n > max {
			max = n
		}
	}
	candidate := fmt.Sprintf("%s-%d", prefix, max+1)
	for existing[candidate] {
		max++
		candidate = fmt.Sprintf("%s-%d", prefix, max+1)
	}
	return candidate
}

func fullName(name string) string {
	name = sanitizeName(name)
	if strings.HasPrefix(name, prefix+"-") {
		return name
	}
	return prefix + "-" + name
}

func printSessions(sessions []session) {
	fmt.Fprintf(os.Stderr, "\n  %s%sSuperPod Sessions%s\n", bold, cPurple, reset)
	fmt.Fprintf(os.Stderr, "  %s────────────────────────────────────%s\n", cGray, reset)
	for i, s := range sessions {
		var icon, status string
		if s.attached {
			icon = fmt.Sprintf("%s●%s", cGreen, reset)
			status = fmt.Sprintf("%sattached%s", cGreen, reset)
		} else {
			icon = fmt.Sprintf("%s○%s", cGray, reset)
			status = fmt.Sprintf("%sdetached%s", cGray, reset)
		}
		fmt.Fprintf(os.Stderr, "  %s%d)%s %s %-18s %s  %s%s win%s\n",
			cBlue, i+1, reset, icon, s.name, status, cGray, s.windows, reset)
	}
	fmt.Fprintln(os.Stderr)
}

// ensureTmuxConfAndProxy writes the claude/codex proxy wrappers into ~/.bashrc.
// Routes through relayPort when ensureRelay() confirmed the relay is up,
// so short tunnel outages are absorbed via 900s upstream retry. Falls
// back to direct tunnelPort if relay setup failed. Set SPOD_NO_RELAY=1
// to force bypass (emergency override).
func ensureTmuxConfAndProxy(useRelay bool) {
	proxyPort := tunnelPort
	if useRelay && relayPort != "" && os.Getenv("SPOD_NO_RELAY") != "1" {
		proxyPort = relayPort
	}
	if proxyPort == "" {
		warn("代理端口未就绪，跳过 bashrc 写入")
		return
	}
	beginMarker := "# spod-proxy-begin"
	endMarker := "# spod-proxy-end"
	script := fmt.Sprintf(
		`grep -q 'set -g mouse on' ~/.tmux.conf 2>/dev/null || echo 'set -g mouse on' >> ~/.tmux.conf
sed -i '/# spod-proxy/,/# spod-proxy-end/d; /# spod-proxy/d; /^export [hH][tT][tT][pP][sS]*_[pP][rR][oO][xX][yY]=.*127\.0\.0\.1/d; /^export [nN][oO]_[pP][rR][oO][xX][yY]=/d; /^_spod_proxy=/d; /^_spod_claude_bin=/d; /^_spod_codex_bin=/d; /^claude()/d; /^codex()/d; /^unset .*_proxy.*spod/d' ~/.bashrc 2>/dev/null
cat >> ~/.bashrc << 'SPOD_EOF'

%s
unset http_proxy https_proxy HTTP_PROXY HTTPS_PROXY no_proxy NO_PROXY 2>/dev/null # spod: clear stale proxy
_spod_proxy="http://127.0.0.1:%s"
claude() { export http_proxy="$_spod_proxy" https_proxy="$_spod_proxy" HTTP_PROXY="$_spod_proxy" HTTPS_PROXY="$_spod_proxy"; command claude "$@"; local rc=$?; unset http_proxy https_proxy HTTP_PROXY HTTPS_PROXY; return $rc; }
codex() { export http_proxy="$_spod_proxy" https_proxy="$_spod_proxy" HTTP_PROXY="$_spod_proxy" HTTPS_PROXY="$_spod_proxy"; command codex "$@"; local rc=$?; unset http_proxy https_proxy HTTP_PROXY HTTPS_PROXY; return $rc; }
%s
SPOD_EOF`,
		beginMarker, proxyPort, endMarker,
	)
	if _, err := ssh(script); err != nil {
		warn("远程配置写入失败（将在连接后重试）")
	}
}

// relayScript is a TCP relay proxy with retry, deployed to SuperPod.
// It sits between claude/codex and the SSH tunnel, absorbing short outages
// by retrying upstream connections instead of immediately failing.
// Both this relay and the upstream SSH tunnel bind per-user ports derived
// from the remote UID (18000+uid%1000 and 17000+uid%1000 respectively) to
// avoid collisions on shared login nodes.
const relayScript = `#!/usr/bin/env python3
# TCP relay with half-close handling + explicit settimeout(None) after
# connect. socket.create_connection(timeout=N) leaks N as the socket's
# DEFAULT timeout, so every subsequent recv/send raises TimeoutError
# after N seconds of idle — that was spuriously shutting down idle
# keepalive connections 3s after CONNECT and causing UND_ERR_SOCKET
# in Claude Code (undici pools and reuses idle HTTPS CONNECT tunnels).
import socket,threading,time,sys,signal,os
UP_PORT=int(sys.argv[1]) if len(sys.argv)>1 else 17897
LISTEN=int(sys.argv[2]) if len(sys.argv)>2 else UP_PORT+1
RETRY_SEC=900; LOG=len(sys.argv)>3 and sys.argv[3]=="--log"
def pipe(src,dst):
    try:
        while True:
            b=src.recv(65536)
            if not b:
                try:dst.shutdown(socket.SHUT_WR)
                except:pass
                return
            dst.sendall(b)
    except:
        try:dst.shutdown(socket.SHUT_WR)
        except:pass
def handle(c):
    c.settimeout(None)
    c.setsockopt(socket.SOL_SOCKET,socket.SO_KEEPALIVE,1)
    end=time.time()+RETRY_SEC;n=0;delay=3
    while time.time()<end:
        try:
            u=socket.create_connection(("127.0.0.1",UP_PORT),timeout=3);break
        except:
            n+=1;time.sleep(delay);delay=min(delay*2,30)
    else:
        if LOG:print(f"[relay] tunnel down {RETRY_SEC}s, drop",flush=True)
        c.close();return
    if n and LOG:print(f"[relay] recovered after {int(time.time()-end+RETRY_SEC)}s ({n} retries)",flush=True)
    u.settimeout(None)  # critical: clear the 3s timeout leaked by create_connection
    u.setsockopt(socket.SOL_SOCKET,socket.SO_KEEPALIVE,1)
    a=threading.Thread(target=pipe,args=(c,u),daemon=True)
    b=threading.Thread(target=pipe,args=(u,c),daemon=True)
    a.start();b.start();a.join();b.join()
    try:c.close()
    except:pass
    try:u.close()
    except:pass
signal.signal(signal.SIGTERM,lambda*_:sys.exit(0))
srv=socket.socket(socket.AF_INET,socket.SOCK_STREAM)
srv.setsockopt(socket.SOL_SOCKET,socket.SO_REUSEADDR,1)
srv.bind(("127.0.0.1",LISTEN));srv.listen(64)
if LOG:print(f"[relay] 127.0.0.1:{LISTEN} -> 127.0.0.1:{UP_PORT} (uid={os.getuid()})",flush=True)
with open(os.path.expanduser("~/.local/share/spod-relay.pid"),"w") as f:f.write(str(os.getpid()))
while True:
    c,_=srv.accept()
    threading.Thread(target=handle,args=(c,),daemon=True).start()
`

// ensureRelay deploys and starts spod-relay.py on SuperPod. Returns true iff
// the relay is confirmed running on relayPort — caller uses this to decide
// whether the claude/codex proxy should point at the relay or fall back to
// the tunnel directly.
func ensureRelay() bool {
	ensurePorts()
	if relayPort == "" || tunnelPort == "" {
		warn("端口未就绪，跳过 relay")
		return false
	}

	// Deploy and start the TCP relay on SuperPod.
	script := fmt.Sprintf(
		`mkdir -p ~/.local/bin ~/.local/share
NEW_SCRIPT=$(cat << 'RELAY_SCRIPT'
%s
RELAY_SCRIPT
)
OLD_SCRIPT=""
[ -f ~/.local/bin/spod-relay.py ] && OLD_SCRIPT=$(cat ~/.local/bin/spod-relay.py)
if [ "$NEW_SCRIPT" = "$OLD_SCRIPT" ] && pgrep -u $(id -u) -f "spod-relay\\.py %s %s" >/dev/null 2>&1; then
    echo "running"
else
    pkill -u $(id -u) -f "spod-relay\\.py" 2>/dev/null; sleep 0.3
    echo "$NEW_SCRIPT" > ~/.local/bin/spod-relay.py
    chmod +x ~/.local/bin/spod-relay.py
    nohup python3 ~/.local/bin/spod-relay.py %s %s --log > /tmp/spod-relay-$(id -u).log 2>&1 &
    sleep 0.5
    if pgrep -u $(id -u) -f "spod-relay\\.py %s %s" >/dev/null 2>&1; then
        echo "started"
    else
        echo "failed"
    fi
fi`,
		relayScript,
		tunnelPort, relayPort,
		tunnelPort, relayPort,
		tunnelPort, relayPort,
	)
	out, err := ssh(script)
	if err != nil {
		warn(fmt.Sprintf("Relay 部署失败: %v", err))
		return false
	}
	switch strings.TrimSpace(out) {
	case "running":
		return true
	case "started":
		ok(fmt.Sprintf("Relay 已启动 (:%s → :%s，隧道断开时自动等待重连)", relayPort, tunnelPort))
		return true
	case "failed":
		warn("Relay 启动失败，查看日志: /tmp/spod-relay-$(id -u).log")
		return false
	default:
		warn(fmt.Sprintf("Relay 状态未知: %q", strings.TrimSpace(out)))
		return false
	}
}

func ensureRemoteSetup() {
	// Relay first (computes per-user ports); proxy config rides the relay
	// if it came up, otherwise points direct to the tunnel.
	relayOK := ensureRelay()
	ensureTmuxConfAndProxy(relayOK)
	ensureRemoteCLIs()
}

// ensureRemoteCLIs verifies the claude/codex npm packages on SuperPod still
// exist (symlink targets are present and executable). Observed 2026-04-29:
// the @anthropic-ai/claude-code package directory was wiped clean (likely a
// stale npm install state or a home-quota cleanup), leaving a dangling
// symlink and `claude: command not found` from inside the wrapper. This
// silently re-installs broken packages so the user doesn't have to debug.
// Skip with SPOD_NO_CLI_CHECK=1.
func ensureRemoteCLIs() {
	if os.Getenv("SPOD_NO_CLI_CHECK") == "1" {
		return
	}
	probe := `ENV_BIN="$HOME/.conda/envs/claude/bin"
broken=""
for name in claude codex; do
    link="$ENV_BIN/$name"
    [ -L "$link" ] || [ -e "$link" ] || { broken="$broken $name"; continue; }
    target=$(readlink -f "$link" 2>/dev/null)
    if [ -z "$target" ] || [ ! -x "$target" ]; then
        broken="$broken $name"
    fi
done
echo "BROKEN:${broken# }"
[ -x "$ENV_BIN/npm" ] && echo "NPM:$ENV_BIN/npm" || echo "NPM:"
`
	out, err := ssh(probe)
	if err != nil {
		warn("远端 claude/codex 健康检查失败（跳过）")
		return
	}
	var brokenLine, npmPath string
	for _, line := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(line, "BROKEN:"):
			brokenLine = strings.TrimSpace(strings.TrimPrefix(line, "BROKEN:"))
		case strings.HasPrefix(line, "NPM:"):
			npmPath = strings.TrimSpace(strings.TrimPrefix(line, "NPM:"))
		}
	}
	if brokenLine == "" {
		return
	}
	if npmPath == "" {
		warn(fmt.Sprintf("远端 %s 损坏（symlink 目标缺失），但 conda env 'claude' 里没找到 npm，无法自动修复", brokenLine))
		warn("手动修复: ssh superpod 后激活 claude env，运行 npm install -g @anthropic-ai/claude-code @openai/codex")
		return
	}
	pkgs := []string{}
	for _, name := range strings.Fields(brokenLine) {
		switch name {
		case "claude":
			pkgs = append(pkgs, "@anthropic-ai/claude-code")
		case "codex":
			pkgs = append(pkgs, "@openai/codex")
		}
	}
	if len(pkgs) == 0 {
		return
	}
	warn(fmt.Sprintf("远端 %s 损坏，正在重新安装 %s ...", brokenLine, strings.Join(pkgs, " ")))
	// PATH must include npm's directory so npm's "#!/usr/bin/env node"
	// shebang resolves the node binary that lives next to it. SSH
	// non-interactive sessions don't source ~/.bashrc, so without this
	// the install fails with "/usr/bin/env: 'node': No such file or directory".
	fixCmd := fmt.Sprintf(`PATH="%s:$PATH" %s install -g %s 2>&1 | tail -3`, filepath.Dir(npmPath), npmPath, strings.Join(pkgs, " "))
	fixOut, fixErr := ssh(fixCmd)
	if fixErr != nil {
		warn(fmt.Sprintf("重装失败: %v\n%s", fixErr, strings.TrimSpace(fixOut)))
		return
	}
	info(fmt.Sprintf("重装完成: %s", strings.TrimSpace(fixOut)))
}

func attachOrCreate(name string) {
	ensureRemoteSetup()
	info(fmt.Sprintf("连接到 %s%s%s ...", bold, name, reset))
	if err := sshInteractive(fmt.Sprintf("tmux attach -t %s 2>/dev/null || tmux new -s %s", name, name)); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 255 {
			fail("SSH 连接失败")
			fail("请检查 VPN 连接: spod vpn status")
			os.Exit(1)
		} else if !errors.As(err, &exitErr) {
			fail(fmt.Sprintf("执行失败: %v", err))
			os.Exit(1)
		}
		// ExitError with code != 255: tmux detach or normal exit
	}
}

// ── Commands ──

func cmdLs() {
	sessions := mustListSessions()
	if len(sessions) == 0 {
		warn("没有活跃会话")
		return
	}
	printSessions(sessions)
}

func cmdNew(name string) {
	if name == "" {
		name = nextName()
	} else {
		name = fullName(name)
	}
	ensureRemoteSetup()
	info(fmt.Sprintf("创建会话 %s%s%s ...", bold, name, reset))
	if err := sshInteractive(fmt.Sprintf("tmux new -s %s", name)); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 255 {
			fail("SSH 连接失败")
			fail("请检查 VPN 连接: spod vpn status")
			os.Exit(1)
		} else if !errors.As(err, &exitErr) {
			fail(fmt.Sprintf("执行失败: %v", err))
			os.Exit(1)
		}
	}
}

func cmdKill(name string) {
	if name == "" {
		fail("用法: spod kill <name>")
		os.Exit(1)
	}
	name = fullName(name)
	if _, err := ssh(fmt.Sprintf("tmux kill-session -t %s", name)); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 255 {
			fail(fmt.Sprintf("SSH 连接失败: %v", err))
		} else {
			fail(fmt.Sprintf("会话 %s 不存在", name))
		}
		os.Exit(1)
	}
	ok(fmt.Sprintf("已关闭 %s", name))
}

func cmdKillAll() {
	sessions := mustListSessions()
	if len(sessions) == 0 {
		warn("没有会话需要关闭")
		return
	}
	failed := false
	for _, s := range sessions {
		if _, err := ssh(fmt.Sprintf("tmux kill-session -t %s", s.name)); err != nil {
			var exitErr *exec.ExitError
			if errors.As(err, &exitErr) && exitErr.ExitCode() == 255 {
				fail(fmt.Sprintf("SSH 连接失败: %v", err))
				os.Exit(1)
			}
			warn(fmt.Sprintf("关闭 %s 失败", s.name))
			failed = true
		} else {
			ok(fmt.Sprintf("已关闭 %s", s.name))
		}
	}
	if failed {
		os.Exit(1)
	}
}

func cmdInteractive() {
	sessions := mustListSessions()
	if len(sessions) == 0 {
		info("没有会话，创建新会话...")
		cmdNew("")
		return
	}

	printSessions(sessions)
	fmt.Fprintf(os.Stderr, "  %s+)%s 新建会话\n", cBlue, reset)
	fmt.Fprintf(os.Stderr, "  %sq)%s 退出\n", cGray, reset)
	fmt.Fprintln(os.Stderr)
	fmt.Fprintf(os.Stderr, "  %s❯%s ", cPurple, reset)

	scanner := bufio.NewScanner(os.Stdin)
	if !scanner.Scan() {
		return
	}
	choice := strings.TrimSpace(scanner.Text())

	switch strings.ToLower(choice) {
	case "q":
		return
	case "n", "+":
		cmdNew("")
	default:
		idx, err := strconv.Atoi(choice)
		if err != nil || idx < 1 || idx > len(sessions) {
			fail("无效选择")
			os.Exit(1)
		}
		attachOrCreate(sessions[idx-1].name)
	}
}

func cmdCreds() {
	ensureVPN()
	home, _ := os.UserHomeDir()
	type credFile struct {
		local  string
		remote string
	}
	files := []credFile{
		{filepath.Join(home, ".codex", "auth.json"), "~/.codex/auth.json"},
		{filepath.Join(home, ".codex", "config.toml"), "~/.codex/config.toml"},
		{filepath.Join(home, ".codex", ".credentials.json"), "~/.codex/.credentials.json"},
		{filepath.Join(home, ".claude", ".credentials.json"), "~/.claude/.credentials.json"},
	}

	// Ensure remote dirs exist
	ssh("mkdir -p ~/.codex ~/.claude")

	synced := 0
	for _, f := range files {
		if _, err := os.Stat(f.local); err != nil {
			continue
		}
		cmd := exec.Command("scp", "-o", "ConnectTimeout=5", f.local, host+":"+f.remote)
		if err := cmd.Run(); err != nil {
			warn(fmt.Sprintf("%s → 失败: %v", f.remote, err))
		} else {
			ok(fmt.Sprintf("%s → %s", filepath.Base(f.local), f.remote))
			synced++
		}
	}
	if synced == 0 {
		warn("没有找到本地凭证文件")
		info("先在本地运行 `codex login` 或 `claude login`")
	} else {
		ok(fmt.Sprintf("已同步 %d 个凭证文件到 SuperPod", synced))
	}
}

func cmdUptime() {
	out, err := ssh("hostname; awk '{print int($1)}' /proc/uptime; uptime -s; uptime")
	if err != nil {
		fail(fmt.Sprintf("查询失败: %v", err))
		os.Exit(1)
	}
	lines := strings.Split(out, "\n")
	if len(lines) < 4 {
		fail("返回格式异常")
		fmt.Fprintln(os.Stderr, out)
		os.Exit(1)
	}
	hostName := strings.TrimSpace(lines[0])
	upSec, _ := strconv.Atoi(strings.TrimSpace(lines[1]))
	bootTime := strings.TrimSpace(lines[2])
	fullLine := strings.TrimSpace(lines[3])

	load, users := "", ""
	if idx := strings.Index(fullLine, "load average:"); idx >= 0 {
		load = strings.TrimSpace(fullLine[idx+len("load average:"):])
	}
	if idx := strings.Index(fullLine, " user"); idx >= 0 {
		parts := strings.Fields(fullLine[:idx])
		if len(parts) > 0 {
			users = parts[len(parts)-1]
		}
	}

	dur := time.Duration(upSec) * time.Second
	var upStr string
	switch {
	case dur < time.Hour:
		upStr = fmt.Sprintf("%d 分钟", int(dur.Minutes()))
	case dur < 24*time.Hour:
		upStr = fmt.Sprintf("%d 小时 %d 分钟", int(dur.Hours()), int(dur.Minutes())%60)
	default:
		upStr = fmt.Sprintf("%d 天 %d 小时", int(dur.Hours())/24, int(dur.Hours())%24)
	}

	info(fmt.Sprintf("节点: %s%s%s", bold, hostName, reset))
	info(fmt.Sprintf("启动: %s  (运行 %s)", bootTime, upStr))
	if load != "" {
		info(fmt.Sprintf("负载: %s   用户: %s", load, users))
	}

	if dur < time.Hour {
		warn("登录节点近期重启 —— 之前的 tmux 会话很可能已丢失")
	} else if load != "" {
		if parts := strings.Split(load, ","); len(parts) > 0 {
			if l1, err := strconv.ParseFloat(strings.TrimSpace(parts[0]), 64); err == nil && l1 > 20 {
				warn(fmt.Sprintf("1 分钟负载 %.2f 偏高，SSH 可能不稳定", l1))
			}
		}
	}
}

func cmdHelp() {
	fmt.Fprintf(os.Stderr, "\n  %s%sspod%s %s— SuperPod 会话管理%s\n\n", bold, cPurple, reset, cGray, reset)
	fmt.Fprintf(os.Stderr, "  %s用法%s\n", bold, reset)
	fmt.Fprintf(os.Stderr, "  %s────────────────────────────────────%s\n", cGray, reset)
	cmds := [][2]string{
		{"spod", "交互选择 / 新建会话"},
		{"spod <name>", "连接到指定会话（不存在则创建）"},
		{"spod new [name]", "创建新会话（自动编号）"},
		{"spod ls", "列出所有会话"},
		{"spod kill <name>", "关掉指定会话"},
		{"spod killall", "关掉所有会话"},
		{"spod vpn", "启动 VPN（后台 headless）"},
		{"spod vpn stop", "停止 VPN"},
		{"spod vpn restart", "重启 VPN"},
		{"spod vpn status", "查看 VPN + SuperPod 状态"},
		{"spod vpn log", "实时查看 VPN 日志"},
		{"spod tunnel", "启动 / 检查 SSH 隧道"},
		{"spod tunnel stop", "关闭隧道"},
		{"spod socks", "启动 SOCKS5 代理（Windows 可用）"},
		{"spod socks stop", "关闭 SOCKS5 代理"},
		{"spod socks status", "查看 SOCKS5 代理状态"},
		{"spod vscode", "配置 Windows VS Code Remote-SSH"},
		{"spod sync <r> <l>", "从 SuperPod 并行 rsync 到本地"},
		{"spod sync stop", "停止所有 rsync"},
		{"spod speed [秒]", "VPN 隧道测速（默认 60s）"},
		{"spod ssh", "裸 SSH（不用 tmux）"},
		{"spod creds", "同步本地凭证到 SuperPod"},
		{"spod uptime", "查看 login 节点启动时间和负载"},
	}
	for _, c := range cmds {
		fmt.Fprintf(os.Stderr, "    %s%-22s%s %s%s%s\n", cBlue, c[0], reset, cGray, c[1], reset)
	}
	fmt.Fprintln(os.Stderr)
}

func main() {
	args := os.Args[1:]
	cmd := ""
	if len(args) > 0 {
		cmd = args[0]
	}

	switch cmd {
	case "-h", "--help", "help":
		cmdHelp()
	case "vpn":
		sub := ""
		if len(args) > 1 {
			sub = args[1]
		}
		switch sub {
		case "stop":
			cmdVpnStop()
		case "restart":
			cmdVpnRestart()
		case "status":
			cmdVpnStatus()
		case "log":
			cmdVpnLog()
		case "widget":
			cmdVpnWidget()
		case "watch":
			cmdVpnWatch()
		default:
			cmdVpnStart()
		}
	case "tunnel":
		if len(args) > 1 && args[1] == "stop" {
			stopTunnel()
		} else {
			ensureTunnel()
		}
	case "socks":
		sub := ""
		if len(args) > 1 {
			sub = args[1]
		}
		switch sub {
		case "stop":
			stopSocks()
		case "status":
			socksStatus()
		default:
			ensureSocks()
		}
	case "vscode":
		cmdVscode()
	case "ls":
		ensureTunnel()
		cmdLs()
	case "new":
		ensureTunnel()
		name := ""
		if len(args) > 1 {
			name = args[1]
		}
		cmdNew(name)
	case "kill":
		ensureTunnel()
		name := ""
		if len(args) > 1 {
			name = args[1]
		}
		cmdKill(name)
	case "killall":
		ensureTunnel()
		cmdKillAll()
	case "sync":
		if len(args) > 1 && args[1] == "stop" {
			cmdSyncStop()
		} else {
			remote, local := "", ""
			if len(args) > 1 {
				remote = args[1]
			}
			if len(args) > 2 {
				local = args[2]
			}
			cmdSync(remote, local)
		}
	case "speed":
		dur := ""
		if len(args) > 1 {
			dur = args[1]
		}
		cmdSpeedtest(dur)
	case "creds":
		cmdCreds()
	case "uptime":
		cmdUptime()
	case "ssh":
		ensureTunnel()
		if err := sshInteractive(); err != nil {
			var exitErr *exec.ExitError
			if errors.As(err, &exitErr) {
				os.Exit(exitErr.ExitCode())
			}
			fail(fmt.Sprintf("SSH 连接失败: %v", err))
			os.Exit(1)
		}
	case "":
		ensureTunnel()
		cmdInteractive()
	default:
		ensureTunnel()
		attachOrCreate(fullName(cmd))
	}
}
