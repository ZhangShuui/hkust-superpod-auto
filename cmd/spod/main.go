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
	tunnelPort = envOr("TUNNEL_PORT", "17897")
	localPort  = envOr("CLASH_PORT", "7897")
	prefix     = "spod"
	vpnScript  = "" // resolved in init()
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
		tunnelPort = envOr("TUNNEL_PORT", "17897")
		localPort = envOr("CLASH_PORT", "7897")
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

    # 心跳：每 15s 发一次，连续 4 次无响应才断（容忍 60s 网络抖动）
    ServerAliveInterval 15
    ServerAliveCountMax 4
    TCPKeepAlive yes`, sshHost, sshUser)

	// Read existing config
	existing, _ := os.ReadFile(configPath)
	content := string(existing)

	// Check if superpod block exists
	if strings.Contains(content, "Host superpod") {
		// Extract current User from existing block
		for _, line := range strings.Split(content, "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "User ") {
				currentUser := strings.TrimPrefix(line, "User ")
				if currentUser == sshUser {
					return // already correct
				}
				break
			}
		}
		// User mismatch — replace the whole superpod block
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

func ssh(args ...string) (string, error) {
	cmd := exec.Command("ssh", append([]string{"-o", "ConnectTimeout=5", host}, args...)...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		errMsg := strings.TrimSpace(stderr.String())
		if errMsg != "" {
			return strings.TrimSpace(stdout.String()), fmt.Errorf("%w: %s", err, errMsg)
		}
		return strings.TrimSpace(stdout.String()), err
	}
	return strings.TrimSpace(stdout.String()), nil
}

func sshInteractive(args ...string) error {
	cmd := exec.Command("ssh", append([]string{"-t", "-o", "ConnectTimeout=10", host}, args...)...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// ── Tunnel ──

// tunnelPID finds the autossh process managing our tunnel.
// Verifies via /proc/PID/cmdline to avoid pgrep self-matches and
// child ssh process false positives.
func tunnelPID() int {
	cmd := exec.Command("pgrep", "-f", "autossh.*-R "+tunnelPort+":127.0.0.1:"+localPort)
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
			continue // process already gone (pgrep self-match)
		}
		if strings.Contains(string(cmdline), "autossh") {
			return pid
		}
	}
	return 0
}

func ensureVPN() {
	if vpnIsUp() {
		return
	}
	fail("VPN 未连接 — SuperPod 不可达 (:22)")
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

	pid := tunnelPID()
	if pid > 0 {
		ok(fmt.Sprintf("隧道运行中 (pid=%d)", pid))
		return
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

func vpnIsUp() bool {
	// Quick check: can we reach superpod.ust.hk on SSH port?
	conn, err := net.DialTimeout("tcp", envOr("SUPERPOD_HOST", "superpod.ust.hk")+":22", 3*time.Second)
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

	// Fallback: inline spinner
	info("等待 VPN 连接建立...")
	deadline := time.Now().Add(90 * time.Second)
	spin := []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
	i := 0
	for time.Now().Before(deadline) {
		if vpnIsUp() {
			fmt.Printf("\r\033[2K")
			ok("\\(ᵔᵕᵔ)/  VPN 已连接！")
			if logFile != nil {
				logFile.Close()
			}
			return
		}
		fmt.Printf("\r  %s ( •_•) 连接中...", spin[i%len(spin)])
		i++
		time.Sleep(300 * time.Millisecond)
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
	if pid > 0 && vpnIsUp() {
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
	bars := []string{"○ ○ ○ ○ ○", "● ○ ○ ○ ○", "● ● ○ ○ ○", "● ● ● ○ ○", "● ● ● ● ○", "● ● ● ● ◐"}

	// Parse VPN log to detect current step
	logPath := ""
	if vpnScript != "" {
		logPath = filepath.Join(filepath.Dir(vpnScript), "vpn.log")
	}

	currentStep := 0
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
				case strings.Contains(line, "DNS fix"):
					currentStep = 5
				case strings.Contains(line, "session #"):
					currentStep = 0 // reset on reconnect
				}
				mu.Unlock()
			}
		}()
	}

	// Background connectivity probe (vpnIsUp has 3s timeout, can't call in render loop)
	var isUp bool
	go func() {
		for {
			up := vpnIsUp()
			mu.Lock()
			isUp = up
			mu.Unlock()
			if up {
				time.Sleep(5 * time.Second)
			} else {
				time.Sleep(2 * time.Second)
			}
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
			fmt.Printf("  \033[32m  %s\033[0m\n", f.label)
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

func mustListSessions() []session {
	sessions, err := listRemoteSessions()
	if err != nil {
		fail(err.Error())
		os.Exit(1)
	}
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

func ensureTmuxConf() {
	// Ensure mouse mode is enabled in remote ~/.tmux.conf
	ssh(`grep -q 'set -g mouse on' ~/.tmux.conf 2>/dev/null || echo 'set -g mouse on' >> ~/.tmux.conf`)
}

func ensureRemoteProxy() {
	// Ensure proxy env vars in remote ~/.bashrc so both Claude Code and Codex
	// can reach their APIs through the reverse tunnel → local Clash proxy.
	marker := "# spod-proxy"
	proxyURL := fmt.Sprintf("http://127.0.0.1:%s", tunnelPort)
	script := fmt.Sprintf(
		`grep -q '%s' ~/.bashrc 2>/dev/null || cat >> ~/.bashrc << 'SPOD_EOF'

%s
export http_proxy=%s
export https_proxy=%s
export HTTP_PROXY=%s
export HTTPS_PROXY=%s
SPOD_EOF`,
		marker, marker, proxyURL, proxyURL, proxyURL, proxyURL,
	)
	ssh(script)
}

func attachOrCreate(name string) {
	ensureTmuxConf()
	ensureRemoteProxy()
	info(fmt.Sprintf("连接到 %s%s%s ...", bold, name, reset))
	if err := sshInteractive(fmt.Sprintf("tmux attach -t %s 2>/dev/null || tmux new -s %s", name, name)); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 255 {
			fail("SSH 连接断开")
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
	ensureTmuxConf()
	ensureRemoteProxy()
	info(fmt.Sprintf("创建会话 %s%s%s ...", bold, name, reset))
	if err := sshInteractive(fmt.Sprintf("tmux new -s %s", name)); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 255 {
			fail("SSH 连接断开")
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
		{"spod vpn status", "查看 VPN + SuperPod 状态"},
		{"spod vpn log", "实时查看 VPN 日志"},
		{"spod tunnel", "启动 / 检查 SSH 隧道"},
		{"spod tunnel stop", "关闭隧道"},
		{"spod sync <r> <l>", "从 SuperPod 并行 rsync 到本地"},
		{"spod sync stop", "停止所有 rsync"},
		{"spod speed [秒]", "VPN 隧道测速（默认 60s）"},
		{"spod ssh", "裸 SSH（不用 tmux）"},
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
