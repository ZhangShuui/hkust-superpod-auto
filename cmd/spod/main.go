package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/json"
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
	"sync/atomic"
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

// ── Targets ──
//
// spod drives more than one HKUST cluster. A target bundles everything that is
// per-cluster — ssh alias, .env key names, /tmp file tag, systemd unit — so
// `spod` (SuperPod) and `spod hpc4` can be used at the same time without
// touching each other's tunnel, relay, lock files, UID cache or ssh config
// block. Anything shared (the VPN itself, the local Clash port) stays global.
type target struct {
	key       string // subcommand name: "superpod" / "hpc4"
	label     string // display name in messages
	alias     string // ~/.ssh/config Host alias (resolved in init from env)
	aliasEnv  string // .env key overriding the alias
	defAlias  string // fallback alias
	userEnv   string // .env key holding the login name
	hostEnv   string // .env key holding the hostname
	defHost   string // fallback hostname
	proxyEnv  string // .env key holding an optional SOCKS proxy (rider mode)
	tunnelEnv string // .env key pinning the remote reverse-tunnel port
	relayEnv  string // .env key pinning the remote relay port
	socksEnv  string // .env key holding the local SOCKS5 listen port
	defSocks  string // fallback SOCKS5 port — must differ per target
	unit      string // systemd --user unit that may already own the tunnel
	tag       string // /tmp filename suffix; "" keeps SuperPod's legacy paths
}

var targets = map[string]*target{
	"superpod": {
		key: "superpod", label: "SuperPod",
		aliasEnv: "SPOD_SSH_HOST", defAlias: "superpod",
		userEnv: "SUPERPOD_USER", hostEnv: "SUPERPOD_HOST", defHost: "superpod.ust.hk",
		proxyEnv:  "SUPERPOD_SSH_PROXY",
		tunnelEnv: "TUNNEL_PORT", relayEnv: "SPOD_RELAY_PORT",
		socksEnv: "SOCKS_PORT", defSocks: "1080",
		unit: "spod-tunnel.service", tag: "",
	},
	"hpc4": {
		key: "hpc4", label: "HPC4",
		aliasEnv: "HPC4_SSH_HOST", defAlias: "hpc4",
		userEnv: "HPC4_USER", hostEnv: "HPC4_HOST", defHost: "hpc4.ust.hk",
		proxyEnv:  "HPC4_SSH_PROXY",
		tunnelEnv: "HPC4_TUNNEL_PORT", relayEnv: "HPC4_RELAY_PORT",
		socksEnv: "HPC4_SOCKS_PORT", defSocks: "1081",
		unit: "spod-tunnel-hpc4.service", tag: "-hpc4",
	},
}

// tgt is the cluster the current invocation talks to. `spod hpc4 ...` flips it
// before anything else runs (see useTarget).
var tgt = targets["superpod"]

func (t *target) user() string     { return envOr(t.userEnv, "") }
func (t *target) hostname() string { return envOr(t.hostEnv, t.defHost) }
func (t *target) proxy() string    { return envOr(t.proxyEnv, "") }

// spodCmd is how the user re-invokes spod for THIS cluster ("spod" /
// "spod hpc4"), so hint messages stay copy-pasteable on either one.
func spodCmd() string {
	if tgt.key == "superpod" {
		return "spod"
	}
	return "spod " + tgt.key
}

// tmp returns a per-target path under /tmp. SuperPod keeps its historical
// names (tag "") so an already-running tunnel/socks from an older binary is
// still found; HPC4 gets its own set.
func (t *target) tmp(base string) string {
	return filepath.Join(os.TempDir(), "spod"+t.tag+"-"+base)
}

// useTarget repoints every per-cluster global at another cluster. Called once,
// early, from dispatch() — before ports, tunnel or ssh touch anything.
func useTarget(key string) {
	t, exists := targets[key]
	if !exists {
		fail(fmt.Sprintf("未知集群: %s", key))
		os.Exit(1)
	}
	if t.user() == "" {
		fail(fmt.Sprintf("%s 未配置：在 .env 里设 %s=<登录名>", t.label, t.userEnv))
		info(fmt.Sprintf("可选：%s=<主机名，默认 %s>", t.hostEnv, t.defHost))
		os.Exit(1)
	}
	tgt = t
	host = t.alias
	tunnelPort = os.Getenv(t.tunnelEnv)
	relayPort = os.Getenv(t.relayEnv)
	socksPort = envOr(t.socksEnv, t.defSocks)
	remoteUID = 0     // UID is per-cluster; drop SuperPod's cached value
	ensureSSHConfig() // the alias block may not exist yet
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

	// Resolve each cluster's ssh alias now that .env is loaded, and re-derive
	// the active target's globals from it.
	for _, t := range targets {
		t.alias = envOr(t.aliasEnv, t.defAlias)
	}
	host = tgt.alias
	socksPort = envOr(tgt.socksEnv, tgt.defSocks)

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

// ensureSSHConfig syncs one ~/.ssh/config block per configured cluster, so
// `ssh superpod` and `ssh hpc4` both work and stay in step with .env. A cluster
// with no user configured is skipped (its block, if any, is left alone).
func ensureSSHConfig() {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	sshDir := filepath.Join(home, ".ssh")
	configPath := filepath.Join(sshDir, "config")

	existing, _ := os.ReadFile(configPath)
	content := string(existing)
	updated := content

	// Deterministic order: the map iterates randomly, and a stable order keeps
	// the file from being rewritten (block-swapped) on every run.
	for _, key := range []string{"superpod", "hpc4"} {
		t := targets[key]
		if t.user() == "" {
			continue
		}
		updated = upsertSSHBlock(updated, t.alias, sshBlockFor(t))
	}

	if updated == content {
		return // already correct
	}
	os.MkdirAll(sshDir, 0700)
	os.WriteFile(configPath, []byte(updated), 0600)
}

// sshBlockFor renders the ~/.ssh/config block for one cluster.
func sshBlockFor(t *target) string {
	// Optional: route SSH through another machine's shared VPN via its SOCKS5
	// proxy (a rider borrowing the provider's `spod socks`). Pair with
	// <CLUSTER>_HOST=<internal IP> to dodge hairpin NAT on the public VIP.
	proxyLine := ""
	if p := t.proxy(); p != "" {
		proxyLine = fmt.Sprintf("\n    ProxyCommand nc -X 5 -x %s %%h %%p", p)
	}
	return fmt.Sprintf(`Host %s
    HostName %s
    User %s%s

    # 连接复用：避免多次 SSH 握手触发服务端限流
    ControlMaster auto
    ControlPath /tmp/spod-ssh-%%r@%%h:%%p
    ControlPersist 300

    # 心跳：每 15s 发一次，连续 4 次无响应才断（容忍 60s 网络抖动）
    ServerAliveInterval 15
    ServerAliveCountMax 4
    TCPKeepAlive yes

    # 首次连接自动记住主机密钥：交互式提示会挂住后台的 ssh/autossh
    StrictHostKeyChecking accept-new`, t.alias, t.hostname(), t.user(), proxyLine)
}

// upsertSSHBlock replaces the `Host <alias>` block in content with desired,
// appending it when absent. Returns content unchanged when it already holds
// exactly this block.
//
// Exact-match idempotency: a loose check (just ControlMaster+User) would
// wrongly treat a stale block as correct and never add/update the ProxyCommand
// when <CLUSTER>_SSH_PROXY changes.
func upsertSSHBlock(content, alias, desired string) string {
	header := "Host " + alias
	if strings.Contains(content, desired) {
		return content
	}
	if !strings.Contains(content, header) {
		// No block for this alias — append
		if content != "" && !strings.HasSuffix(content, "\n") {
			content += "\n"
		}
		if content != "" {
			content += "\n"
		}
		return content + desired + "\n"
	}
	// Block outdated — drop it (from "Host <alias>" to the next "Host " or EOF)
	// and re-append the fresh one.
	lines := strings.Split(content, "\n")
	var result []string
	inBlock := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == header {
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
	cleaned := strings.TrimSpace(strings.Join(result, "\n"))
	if cleaned != "" {
		cleaned += "\n\n"
	}
	return cleaned + desired + "\n"
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

// isFatalSSHErr reports whether ssh failed for a reason retrying cannot fix.
// It exits 255 for these just as it does for a reset connection, but:
//   - a rejected login must not be repeated — HKUST locks the ITSC account
//     after enough failed attempts, which would take the VPN down with it;
//   - a host-key mismatch will fail identically every time, and three rounds
//     of backoff only bury the one line that says what to do about it.
func isFatalSSHErr(stderr string) bool {
	for _, m := range []string{
		"Permission denied",
		"Too many authentication failures",
		"No supported authentication methods",
		"Host key verification failed",
		"REMOTE HOST IDENTIFICATION HAS CHANGED",
	} {
		if strings.Contains(stderr, m) {
			return true
		}
	}
	return false
}

// sshAuthOK reports whether we can log in without a password prompt.
// Used as a gate before spawning autossh: autossh has no tty and retries
// forever, so pointing it at an account we can't key into produces an endless
// stream of failed logins.
func sshAuthOK() bool {
	err := exec.Command("ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=8", host, "true").Run()
	return err == nil
}

func ssh(args ...string) (string, error) {
	const maxRetries = 3
	delays := []time.Duration{2 * time.Second, 4 * time.Second, 8 * time.Second}

	// For multi-line scripts, use stdin instead of command argument.
	// SSH's remote bash -c has issues with heredocs passed as arguments.
	useStdin := len(args) == 1 && strings.Contains(args[0], "\n")

	for attempt := 0; ; attempt++ {
		var cmd *exec.Cmd
		// BatchMode: this helper runs unattended, so a password prompt would
		// either hang it on /dev/tty or spend login attempts nobody is watching.
		// Interactive sessions (sshInteractive) still allow passwords.
		if useStdin {
			cmd = exec.Command("ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=5", host, "bash -s")
			cmd.Stdin = strings.NewReader(args[0])
		} else {
			cmd = exec.Command("ssh", append([]string{"-o", "BatchMode=yes", "-o", "ConnectTimeout=5", host}, args...)...)
		}
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		err := cmd.Run()
		if err == nil {
			return strings.TrimSpace(stdout.String()), nil
		}
		if isSSHConnErr(err) && !isFatalSSHErr(stderr.String()) && attempt < maxRetries {
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
	//
	// The trailing ssh alias is what keeps clusters apart: every target's
	// autossh forwards the SAME local Clash port, so a pattern ending at
	// :7897 matches `spod hpc4`'s tunnel just as well as SuperPod's — and
	// ensureTunnel would then "clean up" the other cluster's tunnel as a
	// stale one on every run. autossh puts the alias last, after the -R.
	quotedLocal := regexp.QuoteMeta(localPort)
	quotedHost := regexp.QuoteMeta(host)
	pgrepPattern := `autossh.*-R [0-9]+:127\.0\.0\.1:` + quotedLocal + ` ` + quotedHost + `( |$)`
	procRE := regexp.MustCompile(`-R (\d+):127\.0\.0\.1:` + quotedLocal + ` ` + quotedHost + `(?:\s|$)`)

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
	sshUser := tgt.user()
	sshHost := tgt.hostname()
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

// pickActiveNode returns the first candidate node whose relay is live, retrying
// each node up to `retries` times (with `delay` between attempts) before moving
// on. Returns "" when no candidate's relay is reachable. `probe` is injected so
// the selection logic is unit-testable without real SSH.
func pickActiveNode(candidates []string, retries int, delay time.Duration, probe func(node string) bool) string {
	for _, node := range candidates {
		for attempt := 0; attempt < retries; attempt++ {
			if probe(node) {
				return node
			}
			if attempt < retries-1 {
				time.Sleep(delay)
			}
		}
	}
	return ""
}

// probeRiderNode reports whether `node` has a LIVE relay: it ssh's into the node
// (through the provider's SOCKS, if configured) and asks the local relay to reach
// OpenAI. The remote self-computes its relay port (18000+uid%1000) so this is
// self-contained. A 405 (relay reached OpenAI) means live; anything else = dead.
func probeRiderNode(node string) bool {
	user := envOr("SUPERPOD_USER", "")
	proxy := os.Getenv("SUPERPOD_SSH_PROXY")
	remote := `p=$((18000 + $(id -u)%1000)); ` +
		`curl -s -x http://127.0.0.1:$p --max-time 8 -o /dev/null ` +
		`-w '%{http_code}' https://chatgpt.com/backend-api/codex/responses`
	sshArgs := []string{
		"-o", "ConnectTimeout=8",
		"-o", "BatchMode=yes",
		"-o", "StrictHostKeyChecking=accept-new",
		"-o", "ControlMaster=auto",
		"-o", "ControlPath=/tmp/spod-rider-%r@%h:%p",
		"-o", "ControlPersist=60",
	}
	if proxy != "" {
		sshArgs = append(sshArgs, "-o", fmt.Sprintf("ProxyCommand=nc -X 5 -x %s %%h %%p", proxy))
	}
	target := node
	if user != "" {
		target = user + "@" + node
	}
	sshArgs = append(sshArgs, target, remote)
	out, err := exec.Command("ssh", sshArgs...).Output()
	return err == nil && strings.TrimSpace(string(out)) == "405"
}

// riderConnect connects as a "rider": it borrows the provider's VPN/tunnel/relay
// (SPOD_NO_VPN), probes the candidate login nodes for the one whose relay is live,
// points the `superpod` ssh alias at it, then hands off to the normal connect path.
func riderConnect(sessionArg string) {
	if tgt.key != "superpod" {
		fail("rider 模式只对 SuperPod 有意义（借用 provider 的 SuperPod 隧道）")
		os.Exit(1)
	}
	// Rider has no local tun0 and must not build its own tunnel/socks — it adopts
	// the provider's. SPOD_NO_VPN short-circuits ensureVPN(); we simply never call
	// ensureTunnel()/ensureSocks() here.
	os.Setenv("SPOD_NO_VPN", "1")
	// Rider borrows the provider's already-running relay (just probed live) — it
	// must never deploy or pkill the relay on the shared account (that restarts a
	// reconnect war on version skew). SPOD_RIDER makes ensureRemoteSetup skip
	// ensureRelay and point the proxy straight at the live relay port.
	os.Setenv("SPOD_RIDER", "1")

	if os.Getenv("SUPERPOD_SSH_PROXY") == "" {
		fail("rider 模式需经 provider 的 SOCKS：请在 .env 配 SUPERPOD_SSH_PROXY=<provider-ip>:1080")
		os.Exit(1)
	}
	if envOr("SUPERPOD_USER", "") == "" {
		fail("请在 .env 配 SUPERPOD_USER（SuperPod 账号，如 szhangfa）")
		os.Exit(1)
	}

	candidates := strings.Fields(os.Getenv("SUPERPOD_HOSTS"))
	if len(candidates) == 0 {
		candidates = []string{"10.22.4.12", "10.22.4.13"} // slogin-01 优先，slogin-02 兜底
	}

	info(fmt.Sprintf("探测活节点（relay 上游存活）：%s", strings.Join(candidates, ", ")))
	node := pickActiveNode(candidates, 3, 2*time.Second, probeRiderNode)
	if node == "" {
		fail("活节点未就绪（provider 隧道可能在重连），稍等重试")
		os.Exit(1)
	}
	ok(fmt.Sprintf("活节点：%s（relay 通）", node))

	// Point the `superpod` alias at the chosen node, then reuse the normal path.
	os.Setenv("SUPERPOD_HOST", node)
	ensureSSHConfig()

	if sessionArg == "" {
		cmdInteractive()
	} else {
		attachOrCreate(fullName(sessionArg))
	}
}

func ensureVPN() {
	// A rider borrowing another machine's VPN (via SUPERPOD_SSH_PROXY → that
	// machine's `spod socks`) has no local tun0. SPOD_NO_VPN=1 skips the check
	// so such a machine can still run spod (it reaches SuperPod through the
	// provider's shared VPN, and adopts the provider's tunnel/relay).
	if os.Getenv("SPOD_NO_VPN") == "1" {
		return
	}
	if vpnTunnelUp() {
		ensureRoute()
		return
	}
	fail("VPN 未连接 — tun0 不存在")
	info("运行 `spod vpn` 启动 VPN")
	info("（借用他人 VPN 时用 SPOD_NO_VPN=1 跳过此检查）")
	os.Exit(1)
}

// ensureRoute makes sure the active cluster's IP is routed into the VPN.
//
// The VPN is split-tunnel: vpn-slice installs a /32 via tun0 only for the hosts
// named in VPN_HOSTS. A cluster that isn't in that list still RESOLVES fine
// (143.89.x.x is public DNS), so it looks configured — but its packets leave
// via eth0 and the TCP connect just hangs until timeout. That's the whole
// failure mode for HPC4 on a VPN brought up for SuperPod only.
//
// The permanent fix is VPN_HOSTS (applied on the next `spod vpn restart`);
// this adds the missing /32 in place so an already-running VPN doesn't have to
// be restarted. Needs root: uses SUDO_PASSWORD from .env, else passwordless
// sudo. Skipped for riders (no local tun0 of their own) and with SPOD_NO_ROUTE=1.
func ensureRoute() {
	if os.Getenv("SPOD_NO_ROUTE") == "1" || tgt.proxy() != "" {
		return
	}
	for _, ip := range resolveTarget(tgt.hostname()) {
		out, err := exec.Command("ip", "route", "get", ip).Output()
		if err == nil && strings.Contains(string(out), " dev tun0") {
			continue
		}
		cmd := exec.Command("sudo", "-S", "-p", "", "ip", "route", "replace", ip, "dev", "tun0", "scope", "link")
		if pw := os.Getenv("SUDO_PASSWORD"); pw != "" {
			cmd.Stdin = strings.NewReader(pw + "\n")
		} else {
			cmd = exec.Command("sudo", "-n", "ip", "route", "replace", ip, "dev", "tun0", "scope", "link")
		}
		if err := cmd.Run(); err != nil {
			warn(fmt.Sprintf("%s (%s) 没有走 VPN 的路由，自动补路由失败: %v", tgt.label, ip, err))
			info(fmt.Sprintf("手动修：sudo ip route replace %s dev tun0 scope link", ip))
			info(fmt.Sprintf("或在 .env 的 VPN_HOSTS 里加上 %s 后 spod vpn restart", tgt.hostname()))
			continue
		}
		ok(fmt.Sprintf("已补 %s 的 VPN 路由 (%s → tun0)", tgt.label, ip))
	}
}

// resolveTarget returns the IPv4 addresses for a hostname (or the literal IP).
func resolveTarget(hostname string) []string {
	if net.ParseIP(hostname) != nil {
		return []string{hostname}
	}
	addrs, err := net.LookupIP(hostname)
	if err != nil {
		return nil
	}
	var out []string
	for _, a := range addrs {
		if v4 := a.To4(); v4 != nil {
			out = append(out, v4.String())
		}
	}
	return out
}

// tunnelManagedExternally reports whether a systemd user unit already owns the
// reverse tunnel. The unit runs a plain `ssh -R`, which the autossh-matching
// tunnelPIDAndPort() can't see — so without this guard ensureTunnel would spawn
// a competing autossh that collides on the remote port. On machines without the
// unit (e.g. a rider colleague's box) is-active != "active" and we fall through
// to spod's own autossh management. SPOD_FORCE_TUNNEL=1 overrides.
func tunnelManagedExternally() bool {
	if os.Getenv("SPOD_FORCE_TUNNEL") == "1" {
		return false
	}
	out, _ := exec.Command("systemctl", "--user", "is-active", tgt.unit).Output()
	return strings.TrimSpace(string(out)) == "active"
}

func ensureTunnel() {
	ensureVPN()
	// Lockfile to prevent concurrent tunnel starts
	lockPath := tgt.tmp("tunnel.lock")
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

	if tunnelManagedExternally() {
		ok(fmt.Sprintf("隧道由 systemd 守护 (%s)，本机不自建", tgt.unit))
		return
	}

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

	// Shared-account tunnel adoption: another machine logged into the SAME
	// SuperPod account already provides this reverse tunnel. The per-user
	// tunnelPort is identical across machines on one account, and the remote
	// :tunnelPort lives on the shared login-node loopback — so our claude/codex
	// (and the shared relay) can ride it. Starting our own autossh here would
	// only collide on the remote bind (ExitOnForwardFailure), thrash, and risk
	// hijacking the port mid-flap onto a worse exit. If the remote port is
	// already up and reachable as a proxy, adopt it instead of racing.
	// SPOD_FORCE_TUNNEL=1 skips adoption (designated provider override).
	if os.Getenv("SPOD_FORCE_TUNNEL") != "1" && remoteTunnelHealthy() {
		ok(fmt.Sprintf("复用隧道 (%s:%s 已由同账号另一台机器提供，本机不自建)", tgt.label, tunnelPort))
		return
	}

	if !sshAuthOK() {
		warn(fmt.Sprintf("%s 免密登录不可用，跳过隧道", tgt.label))
		info(fmt.Sprintf("autossh 没有 tty 输密码，会一直重试到锁账号；先装公钥：ssh-copy-id %s", host))
		return
	}

	info(fmt.Sprintf("启动隧道 (%s:%s → 本地:%s)...", tgt.label, tunnelPort, localPort))

	logPath := tgt.tmp("tunnel.log")
	// Truncate on each new autossh start: the prior autossh (if any) was
	// already killed in the loop above, and append-mode would otherwise
	// grow without bound across reconnect storms.
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		warn(fmt.Sprintf("无法打开日志文件: %v", err))
	}

	// ControlPath=none is load-bearing, not belt-and-braces. ensureSSHConfig
	// gives every target `ControlMaster auto` + a shared ControlPath, and
	// `ControlMaster=no` alone only says "don't *become* the master" — the
	// child still *joins* an existing mux, hands the -R to that master, and
	// exits 0. autossh sees a clean exit and quits, so the forward survives
	// only as long as the mux's ControlPersist window: tunnelPID() finds
	// nothing, the port looks like a peer's tunnel, and it dies minutes later
	// with no autossh to rebuild it. Own socket = own connection = own life.
	cmd := exec.Command("autossh", "-M", "0", "-f", "-N",
		"-o", "ServerAliveInterval=15",
		"-o", "ServerAliveCountMax=4",
		"-o", "ExitOnForwardFailure=yes",
		"-o", "TCPKeepAlive=yes",
		"-o", "ControlMaster=no",
		"-o", "ControlPath=none",
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

// remoteTunnelHealthy reports whether the remote reverse-tunnel port is already
// bound on SuperPod AND reachable as an HTTP proxy — i.e. another machine on the
// shared account is already providing it. Used by ensureTunnel to adopt a peer's
// tunnel instead of racing to create a duplicate on the same per-user port.
// Only callers without a live local autossh reach this, so the SSH round-trip
// (and proxied probe) is paid by riders, never by the provider.
func remoteTunnelHealthy() bool {
	if tunnelPort == "" {
		return false
	}
	probe := fmt.Sprintf(
		`ss -ltn 2>/dev/null | grep -q "127.0.0.1:%s " || exit 1
code=$(curl -s -o /dev/null -w '%%{http_code}' -x http://127.0.0.1:%s --max-time 8 https://api.anthropic.com/v1/messages 2>/dev/null)
[ -n "$code" ] && [ "$code" != "000" ] && echo OK`,
		tunnelPort, tunnelPort,
	)
	out, err := ssh(probe)
	if err != nil {
		return false
	}
	return strings.TrimSpace(out) == "OK"
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
	lockPath := tgt.tmp("socks.lock")
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

	// The port may already be served by an external supervisor (e.g. a systemd
	// unit running plain `ssh -D`), which socksPID() — matching only autossh —
	// cannot see. Probe the port so we don't spawn a second autossh that fails
	// ExitOnForwardFailure on the already-bound port and flaps forever. This
	// lets `spod socks` / `spod vscode` coexist with a systemd-managed proxy.
	if c, err := net.DialTimeout("tcp", "127.0.0.1:"+socksPort, time.Second); err == nil {
		c.Close()
		ok(fmt.Sprintf("SOCKS5 代理已在运行 (0.0.0.0:%s 已监听，外部托管)", socksPort))
		return
	}

	if !sshAuthOK() {
		fail(fmt.Sprintf("%s 免密登录不可用，不启动 SOCKS（autossh 会一直重试到锁账号）", tgt.label))
		info(fmt.Sprintf("先装公钥：ssh-copy-id %s", host))
		os.Exit(1)
	}

	info(fmt.Sprintf("启动 SOCKS5 代理 (0.0.0.0:%s → %s)...", socksPort, tgt.label))

	logPath := filepath.Join(os.TempDir(), "spod-socks.log")
	// Truncate on each new autossh start (mirrors ensureTunnel rationale).
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		warn(fmt.Sprintf("无法打开日志文件: %v", err))
	}

	// ControlPath=none for the same reason as the reverse tunnel above: a
	// mux-borne -D dies with the master's ControlPersist window.
	cmd2 := exec.Command("autossh", "-M", "0", "-f", "-N",
		"-o", "ServerAliveInterval=15",
		"-o", "ServerAliveCountMax=4",
		"-o", "ExitOnForwardFailure=yes",
		"-o", "TCPKeepAlive=yes",
		"-o", "ControlMaster=no",
		"-o", "ControlPath=none",
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
			warn(fmt.Sprintf("端口未监听 — 进程可能卡住，尝试 `%s socks stop` 后重启", spodCmd()))
		} else {
			conn.Close()
			ok("端口监听正常")
		}
		return
	}
	// No autossh of ours — but the proxy may be externally managed (the systemd
	// unit runs plain `ssh -D`, which socksPID() cannot see). ensureSocks
	// already probes the port for exactly this reason; report the same truth
	// here instead of claiming the proxy is down while it is serving traffic.
	if conn, err := net.DialTimeout("tcp", "127.0.0.1:"+socksPort, 2*time.Second); err == nil {
		conn.Close()
		ok(fmt.Sprintf("SOCKS5 代理运行中 (0.0.0.0:%s 已监听，外部托管)", socksPort))
		return
	}
	warn("SOCKS5 代理未运行")
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

// windowsUserSID returns the current Windows user's SID via whoami.exe, or "".
func windowsUserSID() string {
	whoami := `/mnt/c/Windows/System32/whoami.exe`
	if _, err := os.Stat(whoami); err != nil {
		return ""
	}
	out, err := exec.Command(whoami, "/user", "/fo", "csv", "/nh").Output()
	if err != nil {
		return ""
	}
	// Output: "DOMAIN\user","S-1-5-21-..."  → SID is the last comma field.
	fields := strings.Split(strings.TrimSpace(string(out)), ",")
	sid := strings.Trim(strings.TrimSpace(fields[len(fields)-1]), `"`)
	if strings.HasPrefix(sid, "S-1-") {
		return sid
	}
	return ""
}

// repairWinSSHConfigPerms fixes the ACL on the WSL-written Windows SSH config so
// Windows OpenSSH (ssh.exe) accepts it. Files written from WSL via /mnt/c inherit
// the user profile's ACEs, which often include stray, unresolvable SIDs with Write
// access (left over from migrated/restored Windows profiles). Windows OpenSSH then
// rejects the config with "Bad owner or permissions" and exits, which breaks VS
// Code Remote-SSH (error: "过程试图写入的管道不存在" / "the pipe ... does not exist").
// We break inheritance and grant only the current user, SYSTEM, and Administrators.
// Best-effort: it warns but never fails the command, and skips entirely unless it
// can resolve the user's SID (so it can never lock the user out of their config).
func repairWinSSHConfigPerms(winUser string) {
	icacls := `/mnt/c/Windows/System32/icacls.exe`
	if _, err := os.Stat(icacls); err != nil {
		return
	}
	sid := windowsUserSID()
	if sid == "" {
		return // can't safely grant the right user; leave perms untouched
	}
	configWin := fmt.Sprintf(`C:\Users\%s\.ssh\config`, winUser)
	// /reset clears any stray *explicit* ACEs (e.g. a leftover Everyone grant);
	// /inheritance:r then drops *inherited* ACEs (the common WSL case, where the
	// profile's unresolvable SIDs leak in) and grants only the safe three —
	// together yielding an exact, clean DACL regardless of how it got dirty.
	exec.Command(icacls, configWin, "/reset").Run()
	err := exec.Command(icacls, configWin, "/inheritance:r",
		"/grant:r", "*"+sid+":F", "*S-1-5-18:F", "*S-1-5-32-544:F").Run() // user, SYSTEM, Administrators
	if err != nil {
		warn(fmt.Sprintf("修复 Windows SSH 配置权限失败（可手动 icacls 修）: %v", err))
		return
	}
	ok("Windows SSH 配置权限已修正（仅 当前用户/SYSTEM/Administrators）")
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

	// 3. Get the cluster's internal IP
	info(fmt.Sprintf("查询 %s 内网 IP...", tgt.label))
	internalIP, err := ssh("hostname -I | awk '{print $1}'")
	if err != nil || internalIP == "" {
		fail(fmt.Sprintf("无法获取 %s 内网 IP: %v", tgt.label, err))
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

	// 5. Ensure Windows SSH key exists and is authorized on the cluster
	sshUser := tgt.user()
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
			info(fmt.Sprintf("添加 Windows SSH 公钥到 %s...", tgt.label))
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
	desired := fmt.Sprintf(`Host %s
    HostName %s
    User %s
    ProxyCommand "%s" -S 127.0.0.1:%s %%h %%p
    ServerAliveInterval 15
    ServerAliveCountMax 4`, tgt.alias, internalIP, sshUser, connectExe, socksPort)

	os.MkdirAll(winSSHDir, 0700)
	existing, _ := os.ReadFile(winConfigPath)
	content := string(existing)

	if strings.Contains(content, "Host "+tgt.alias) {
		// Replace existing block
		lines := strings.Split(content, "\n")
		var result []string
		inBlock := false
		for _, line := range lines {
			trimmed := strings.TrimSpace(line)
			if trimmed == "Host "+tgt.alias || strings.HasPrefix(trimmed, "Host "+tgt.alias+" ") ||
				trimmed == "Host "+tgt.hostname()+" "+tgt.alias {
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
	repairWinSSHConfigPerms(winUser)

	fmt.Println()
	info("VS Code 连接方式:")
	info("  1. 安装 Remote-SSH 扩展")
	info(fmt.Sprintf("  2. Ctrl+Shift+P → Remote-SSH: Connect to Host → %s", tgt.alias))
	info(fmt.Sprintf("  （确保 WSL 中 SOCKS 代理运行: %s socks）", spodCmd()))
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
	conn, err := net.DialTimeout("tcp", tgt.hostname()+":22", 5*time.Second)
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
	// SIGTERM may be ignored or stuck in cleanup; escalate to SIGKILL so a
	// later cmdVpnRestart can't double-launch a second VPN against a still-
	// alive openconnect that's holding tun0.
	if err := syscall.Kill(pid, 0); err == nil {
		warn(fmt.Sprintf("SIGTERM 后 pid=%d 仍存活，发送 SIGKILL", pid))
		syscall.Kill(-pid, syscall.SIGKILL)
		syscall.Kill(pid, syscall.SIGKILL)
		killDeadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(killDeadline) {
			if err := syscall.Kill(pid, 0); err != nil {
				break
			}
			time.Sleep(200 * time.Millisecond)
		}
		if err := syscall.Kill(pid, 0); err == nil {
			fail(fmt.Sprintf("pid=%d 抗 SIGKILL（D 状态？），保留 PID 文件以防重启冲突", pid))
			return
		}
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
	// Report every configured cluster: they share one VPN but need separate
	// split-tunnel routes, so one can be reachable while the other is not.
	up := vpnTunnelUp()
	for _, key := range []string{"superpod", "hpc4"} {
		t := targets[key]
		if t.user() == "" {
			continue
		}
		addr := t.hostname() + ":22"
		if !up {
			fail(fmt.Sprintf("%s 不可达 (%s) — tun0 不存在", t.label, addr))
			continue
		}
		conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
		if err != nil {
			fail(fmt.Sprintf("%s 不可达 (%s)", t.label, addr))
			for _, ip := range resolveTarget(t.hostname()) {
				out, _ := exec.Command("ip", "route", "get", ip).Output()
				if !strings.Contains(string(out), " dev tun0") {
					info(fmt.Sprintf("  %s 没走 VPN 路由 — 在 .env 的 VPN_HOSTS 里加上 %s 后 spod vpn restart", ip, t.hostname()))
				}
			}
			continue
		}
		conn.Close()
		ok(fmt.Sprintf("%s 可达 (%s)", t.label, addr))
	}
}

func cmdVpnWidget() {
	// Compact one-line status for shell prompt / tmux
	// --color flag enables ANSI colors
	color := false
	for _, a := range os.Args[1:] {
		if a == "--color" {
			color = true
		}
	}
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
		fail(fmt.Sprintf("用法: %s sync <remote_path> <local_path>", spodCmd()))
		info(fmt.Sprintf("示例: %s sync /project/data/train ./data/", spodCmd()))
		info(fmt.Sprintf("示例: %s sync /home/user/results /mnt/e/results/", spodCmd()))
		os.Exit(1)
	}

	sshUser := tgt.user()
	if sshUser == "" {
		fail(fmt.Sprintf("需要设置 %s（在 .env 或环境变量中）", tgt.userEnv))
		os.Exit(1)
	}
	src := sshUser + "@" + tgt.hostname() + ":" + remotePath
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

	out, _ := exec.Command("bash", "-c", "pgrep -fc '"+syncPattern()+"' || echo 0").CombinedOutput()
	count := strings.TrimSpace(string(out))
	ok(fmt.Sprintf("%s 路 rsync 运行中", count))
	info(fmt.Sprintf("查看进度: du -sh %s", dst))
	info(fmt.Sprintf("停止所有: %s sync stop", spodCmd()))
}

func cmdSyncStop() {
	exec.Command("pkill", "-f", syncPattern()).Run()
	ok(fmt.Sprintf("已停止所有 %s 的 rsync", tgt.label))
}

// syncPattern matches only THIS cluster's rsync jobs. The remote host appears
// in every job's argv (user@host:path), so scoping by it keeps `spod sync stop`
// from killing a transfer running against the other cluster.
func syncPattern() string {
	return "rsync.*-rlP.*" + regexp.QuoteMeta(tgt.hostname())
}

// ── Get (pull individual files) ──

// winPathRe matches a Windows drive path like C:\Users\foo or D:/bar.
var winPathRe = regexp.MustCompile(`^([A-Za-z]):[\\/](.*)$`)

// toWSLPath converts `C:\Users\Win11\Downloads` → `/mnt/c/Users/Win11/Downloads`.
// Non-Windows paths pass through untouched.
func toWSLPath(p string) string {
	m := winPathRe.FindStringSubmatch(p)
	if m == nil {
		return p
	}
	return "/mnt/" + strings.ToLower(m[1]) + "/" + strings.ReplaceAll(m[2], `\`, "/")
}

// defaultDownloadDir returns the Windows Downloads folder when running under
// WSL (that's where pulled files are almost always wanted), else the cwd.
func defaultDownloadDir() string {
	if u := findWindowsUser(); u != "" {
		d := filepath.Join("/mnt/c/Users", u, "Downloads")
		if fi, err := os.Stat(d); err == nil && fi.IsDir() {
			return d
		}
	}
	cwd, _ := os.Getwd()
	return cwd
}

// ── Progress bar ──

func stderrIsTTY() bool {
	fi, err := os.Stderr.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

func humanBytes(b int64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.2f GB", float64(b)/(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(b)/(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(b)/(1<<10))
	}
	return fmt.Sprintf("%d B", b)
}

// humanAge renders how long something has been sitting — a queued file, a
// receiver that stopped. humanDuration is a stopwatch and gives up past 99
// hours ("--:--"), which is precisely the range that matters here: a queue
// nobody drained for four days rendered as "96:00:02", and a receiver last
// seen last week as "已离线 --:--", which reads as "unknown".
func humanAge(secs int64) string {
	switch {
	case secs < 60:
		return fmt.Sprintf("%d 秒", secs)
	case secs < 3600:
		return fmt.Sprintf("%d 分钟", secs/60)
	case secs < 86400:
		return fmt.Sprintf("%d 小时", secs/3600)
	default:
		return fmt.Sprintf("%d 天", secs/86400)
	}
}

func humanDuration(d time.Duration) string {
	if d < 0 || d > 99*time.Hour {
		return "--:--"
	}
	total := int(d.Seconds())
	if h := total / 3600; h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, (total%3600)/60, total%60)
	}
	return fmt.Sprintf("%02d:%02d", total/60, total%60)
}

// (progress is driven by a byte counter the fetchers bump — see parallelFetch.
// Polling file sizes would not work: destination files are preallocated to
// their full length so workers can pwrite chunks at absolute offsets.)

// renderBar draws a single-line progress bar on stderr, overwriting in place.
func renderBar(done, total int64, rate float64, label string) {
	const width = 28
	frac := 0.0
	if total > 0 {
		frac = float64(done) / float64(total)
	}
	if frac > 1 {
		frac = 1
	}
	filled := int(frac * width)
	bar := strings.Repeat("█", filled) + strings.Repeat("░", width-filled)

	eta := "--:--"
	if rate > 0 && total > done {
		eta = humanDuration(time.Duration(float64(total-done)/rate) * time.Second)
	}
	speed := "  --  "
	if rate > 0 {
		speed = humanBytes(int64(rate)) + "/s"
	}

	line := fmt.Sprintf("  %s▕%s%s%s▏%s %3.0f%%  %s/%s  %s  ETA %s  %s%s%s",
		cGray, reset, bar, cGray, reset,
		frac*100, humanBytes(done), humanBytes(total), speed, eta,
		cGray, label, reset)
	fmt.Fprintf(os.Stderr, "\r\033[K%s", line)
}

// progressBar samples `sample` until stop is closed, drawing a bar. Speed is
// measured over a trailing window so a stalled or bursty link doesn't swing the
// ETA. `label` is re-evaluated every tick for the trailing annotation.
func progressBar(sample func() int64, total int64, label func() string, stop <-chan struct{}, done chan<- struct{}) {
	defer close(done)
	if !stderrIsTTY() {
		return
	}
	const tick = 500 * time.Millisecond
	const window = 10 // samples → 5s trailing window

	samples := []int64{sample()}
	ticker := time.NewTicker(tick)
	defer ticker.Stop()

	for {
		select {
		case <-stop:
			renderBar(sample(), total, 0, "")
			fmt.Fprint(os.Stderr, "\r\033[K")
			return
		case <-ticker.C:
			cur := sample()
			samples = append(samples, cur)
			if len(samples) > window+1 {
				samples = samples[len(samples)-(window+1):]
			}
			rate := 0.0
			if n := len(samples); n > 1 {
				elapsed := time.Duration(n-1) * tick
				rate = float64(samples[n-1]-samples[0]) / elapsed.Seconds()
			}
			lbl := ""
			if label != nil {
				lbl = label()
			}
			renderBar(cur, total, rate, lbl)
		}
	}
}

// shellQuote wraps s in single quotes for safe interpolation into a remote
// shell command.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// remoteFile is one file selected for download.
type remoteFile struct {
	path string // absolute path on SuperPod
	base string // basename — also the local filename under dest
	size int64
}

// fetchChunk is one contiguous byte range of one file.
type fetchChunk struct {
	rf  *remoteFile
	idx int   // chunk index within the file
	off int64 // absolute offset in the file
	n   int64
}

// getState is the resume sidecar written next to each destination file.
//
// Chunks land at absolute offsets via pwrite, so a half-fetched file is already
// full-length with holes in it — its size says nothing about what completed.
// The sidecar is the only record of which ranges are done.
type getState struct {
	Size      int64 `json:"size"`
	ChunkSize int64 `json:"chunk"`
	Done      []int `json:"done"`
}

func statePathFor(dest, base string) string {
	return filepath.Join(dest, "."+base+".spodget")
}

func loadGetState(p string) *getState {
	b, err := os.ReadFile(p)
	if err != nil {
		return nil
	}
	var st getState
	if json.Unmarshal(b, &st) != nil || st.Size <= 0 || st.ChunkSize <= 0 {
		return nil
	}
	return &st
}

// fetchChunkSize aims for a few chunks per stream so a slow flow can't strand
// the tail of a transfer, while keeping chunks big enough that per-chunk SSH
// round-trips stay negligible.
func fetchChunkSize(size int64, streams int) int64 {
	const minChunk, maxChunk = 4 << 20, 64 << 20
	target := size / int64(streams*4)
	switch {
	case target < minChunk:
		return minChunk
	case target > maxChunk:
		return maxChunk
	default:
		return target
	}
}

// getStreams is how many independent SSH connections a download fans out over.
//
// Each stream is its own TCP flow. That matters because the HKUST VPN carries
// everything over a single TLS/TCP connection with no ESP datagram channel
// (openconnect logs "Set up UDP failed; using SSL instead"), which puts the
// inner RTT around 300ms and pins any *one* TCP flow near 255 KB/s regardless
// of available bandwidth. Measured on that link: 1 flow 254 KB/s, 3 flows
// 427 KB/s, 6 flows 695 KB/s. Beyond ~6 the gain flattens and login-rate
// pressure on the shared account starts to matter.
func getStreams() int {
	if v := os.Getenv("SPOD_GET_STREAMS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 1 && n <= 16 {
			return n
		}
	}
	return 4
}

// getSSHOpts builds the SSH options for one fetch worker.
//
// The per-worker ControlPath is the whole point: `Host superpod` in ~/.ssh/config
// sets ControlMaster auto on a shared socket, so without this override every
// "parallel" stream would be a channel on ONE multiplexed TCP connection and
// share one congestion window — measured at 100 KB/s against 254 KB/s for a
// dedicated connection. A private socket per worker gives each its own flow
// while still reusing that connection across all of the worker's chunks, so a
// large file costs `streams` logins rather than one per chunk.
func getSSHOpts(worker int) []string {
	return []string{
		"-o", "ControlMaster=auto",
		"-o", fmt.Sprintf("ControlPath=/tmp/spod-get-%d-%d", os.Getpid(), worker),
		"-o", "ControlPersist=30",
		"-o", "ConnectTimeout=15",
		"-o", "ServerAliveInterval=15",
		"-o", "ServerAliveCountMax=4",
	}
}

// chunkWriter pwrites a chunk's bytes at their absolute offset and reports
// progress as they land. Concurrent WriteAt on one *os.File is safe — it is a
// pwrite syscall and does not touch the shared file offset.
type chunkWriter struct {
	f       *os.File
	off     int64
	written int64
	counter *atomic.Int64
}

func (w *chunkWriter) Write(p []byte) (int, error) {
	n, err := w.f.WriteAt(p, w.off)
	w.off += int64(n)
	w.written += int64(n)
	w.counter.Add(int64(n))
	return n, err
}

// fetchOne pulls a single chunk, retrying on transport failure. A retry rolls
// the progress counter back by what the failed attempt wrote, since the chunk
// restarts from its beginning.
func fetchOne(c fetchChunk, f *os.File, opts []string, counter *atomic.Int64) error {
	const attempts = 3
	var lastErr error
	for a := 0; a < attempts; a++ {
		if a > 0 {
			time.Sleep(time.Duration(int64(2*time.Second) << uint(a-1)))
		}
		w := &chunkWriter{f: f, off: c.off, counter: counter}
		dd := fmt.Sprintf("dd if=%s bs=1M iflag=skip_bytes,count_bytes skip=%d count=%d status=none",
			shellQuote(c.rf.path), c.off, c.n)
		cmd := exec.Command("ssh", append(append([]string{}, opts...), host, dd)...)
		cmd.Stdout = w
		var errBuf bytes.Buffer
		cmd.Stderr = &errBuf
		err := cmd.Run()

		switch {
		case err == nil && w.written == c.n:
			return nil
		case err == nil:
			// Short read: the file shrank or was silly-renamed mid-transfer.
			lastErr = fmt.Errorf("%s 偏移 %d 只收到 %d/%d 字节", c.rf.base, c.off, w.written, c.n)
		default:
			lastErr = err
			if msg := strings.TrimSpace(errBuf.String()); msg != "" {
				lastErr = fmt.Errorf("%w: %s", err, msg)
			}
		}
		counter.Add(-w.written)
	}
	return lastErr
}

// parallelFetch downloads every file in files into dest over `streams`
// independent SSH connections, resuming from any sidecar left by a previous
// run. It returns a sampler for the progress bar via the counter it fills.
func parallelFetch(files []*remoteFile, dest string, streams int, counter *atomic.Int64, doneFiles *atomic.Int64) error {
	type fileCtx struct {
		f         *os.File
		statePath string
		state     getState
		remaining int
	}

	ctxs := map[*remoteFile]*fileCtx{}
	closeAll := func() {
		for _, c := range ctxs {
			c.f.Close()
		}
	}

	var chunks []fetchChunk
	for _, rf := range files {
		chunkSize := fetchChunkSize(rf.size, streams)
		st := getState{Size: rf.size, ChunkSize: chunkSize}
		// Adopt the previous run's chunk size so resume still lines up when
		// SPOD_GET_STREAMS changed between runs.
		if prev := loadGetState(statePathFor(dest, rf.base)); prev != nil && prev.Size == rf.size {
			st = *prev
		}
		done := map[int]bool{}
		for _, i := range st.Done {
			done[i] = true
		}

		local := filepath.Join(dest, rf.base)
		f, err := os.OpenFile(local, os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			closeAll()
			return fmt.Errorf("打开 %s: %w", local, err)
		}
		if err := f.Truncate(rf.size); err != nil {
			f.Close()
			closeAll()
			return fmt.Errorf("预分配 %s: %w", local, err)
		}

		fc := &fileCtx{f: f, statePath: statePathFor(dest, rf.base), state: st}
		ctxs[rf] = fc

		for off, idx := int64(0), 0; off < rf.size; off, idx = off+st.ChunkSize, idx+1 {
			n := st.ChunkSize
			if rem := rf.size - off; rem < n {
				n = rem
			}
			if done[idx] {
				counter.Add(n)
				continue
			}
			fc.remaining++
			chunks = append(chunks, fetchChunk{rf: rf, idx: idx, off: off, n: n})
		}
		if fc.remaining == 0 {
			doneFiles.Add(1)
		}
	}
	defer closeAll()

	if streams > len(chunks) {
		streams = len(chunks)
	}
	if streams < 1 {
		return nil
	}

	// A worker's SSH connection outlives its chunks; drop it explicitly rather
	// than leaving ControlPersist sockets behind after the command exits.
	defer func() {
		for i := 0; i < streams; i++ {
			exec.Command("ssh", append(append([]string{"-O", "exit"}, getSSHOpts(i)...), host)...).Run()
		}
	}()

	var (
		mu      sync.Mutex // guards fileCtx bookkeeping and sidecar writes
		wg      sync.WaitGroup
		next    atomic.Int64
		errOnce sync.Once
		fetchEr error
		abort   = make(chan struct{})
	)

	for w := 0; w < streams; w++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			opts := getSSHOpts(worker)
			for {
				select {
				case <-abort:
					return
				default:
				}
				i := int(next.Add(1)) - 1
				if i >= len(chunks) {
					return
				}
				c := chunks[i]
				if err := fetchOne(c, ctxs[c.rf].f, opts, counter); err != nil {
					errOnce.Do(func() { fetchEr = err; close(abort) })
					return
				}

				mu.Lock()
				fc := ctxs[c.rf]
				fc.state.Done = append(fc.state.Done, c.idx)
				fc.remaining--
				if fc.remaining == 0 {
					doneFiles.Add(1)
					fc.f.Sync()
					os.Remove(fc.statePath)
				} else if b, err := json.Marshal(fc.state); err == nil {
					os.WriteFile(fc.statePath, b, 0644)
				}
				mu.Unlock()
			}
		}(w)
	}
	wg.Wait()
	return fetchEr
}

// cmdGet pulls one or more remote files to a local directory, verifying each
// by MD5 against the checksum taken *before* the transfer.
//
// Why the pre-transfer checksum: a job on SuperPod can unlink a file while
// we're still reading it. NFS silly-renames it to `.nfsXXXX` instead of
// deleting, so rsync finishes cleanly (exit 0) but the original path is gone
// and there is nothing left to verify against afterwards. Grabbing the digest
// up front means a completed transfer can always be proven intact.
//
// Transfers fan out over several independent SSH connections — see getStreams
// and getSSHOpts for why that is worth roughly 3x on this link.
// fetchAndVerify pulls files into dest over the parallel fetcher and checks
// each one against an MD5 snapshot taken *before* the transfer (see cmdGet for
// why the snapshot has to come first). The map it returns holds a per-file
// verification result — a nil entry means that file arrived intact. A non-nil
// second return means the transfer itself failed and nothing was verified.
func fetchAndVerify(files []*remoteFile, dest string) (map[*remoteFile]error, error) {
	var totalBytes int64
	for _, f := range files {
		totalBytes += f.size
	}

	info(fmt.Sprintf("计算远端 MD5（%d 个文件）...", len(files)))
	var shellList []string
	for _, f := range files {
		shellList = append(shellList, shellQuote(f.path))
	}
	want := map[string]string{} // remote path → md5
	if sums, err := ssh("md5sum " + strings.Join(shellList, " ")); err != nil {
		warn(fmt.Sprintf("远端 MD5 失败，将跳过校验: %v", err))
	} else {
		for _, line := range strings.Split(sums, "\n") {
			f := strings.Fields(strings.TrimSpace(line))
			if len(f) >= 2 {
				want[strings.Join(f[1:], " ")] = f[0]
			}
		}
	}

	streams := getStreams()
	info(fmt.Sprintf("下载到 %s（%d 路并行）...", dest, streams))

	var fetched, doneFiles atomic.Int64
	label := func() string {
		if len(files) <= 1 {
			return ""
		}
		return fmt.Sprintf("(%d/%d 文件)", doneFiles.Load(), len(files))
	}
	stop, barDone := make(chan struct{}), make(chan struct{})
	go progressBar(fetched.Load, totalBytes, label, stop, barDone)
	fetchErr := parallelFetch(files, dest, streams, &fetched, &doneFiles)
	close(stop)
	<-barDone
	if fetchErr != nil {
		return nil, fetchErr
	}

	results := map[*remoteFile]error{}
	for _, f := range files {
		local := filepath.Join(dest, f.base)
		fi, err := os.Stat(local)
		if err != nil {
			fail(f.base + " — 本地文件缺失")
			results[f] = errors.New("本地文件缺失")
			continue
		}
		exp, haveSum := want[f.path]
		if !haveSum {
			warn(fmt.Sprintf("%s (%s) — 无远端 MD5，未校验", f.base, humanBytes(fi.Size())))
			results[f] = nil
			continue
		}
		out, err := exec.Command("md5sum", local).Output()
		got := ""
		if err == nil {
			got = strings.Fields(string(out))[0]
		}
		if got == exp {
			ok(fmt.Sprintf("%s (%s) — MD5 校验通过", f.base, humanBytes(fi.Size())))
			results[f] = nil
			continue
		}
		fail(fmt.Sprintf("%s — MD5 不匹配（远端 %s / 本地 %s）", f.base, exp, got))
		results[f] = errors.New("MD5 不匹配")
	}
	return results, nil
}

func cmdGet(args []string) {
	// Parse: spod get <remote>... [-o <dest>]
	var remotes []string
	dest := ""
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-o", "--out":
			if i+1 >= len(args) {
				fail("-o 需要一个目标目录")
				os.Exit(1)
			}
			dest = args[i+1]
			i++
		default:
			remotes = append(remotes, args[i])
		}
	}

	if len(remotes) == 0 {
		fail(fmt.Sprintf("用法: %s get <远程路径>... [-o <本地目录>]", spodCmd()))
		info(`示例: spod get /project/foo/a.mp4`)
		info(`示例: spod get '/project/foo/*.mp4' -o .`)
		info(`示例: spod get /project/foo/a.mp4 -o 'C:\Users\Win11\Desktop'`)
		info("默认下载到 Windows 的 Downloads 文件夹")
		os.Exit(1)
	}

	if dest == "" {
		dest = defaultDownloadDir()
	}
	dest = toWSLPath(dest)
	if err := os.MkdirAll(dest, 0755); err != nil {
		fail(fmt.Sprintf("创建目录失败: %v", err))
		os.Exit(1)
	}

	ensureVPN()

	// Expand globs remotely and drop anything that isn't a readable regular file.
	// stat needs -L: without it a symlink reports the length of the link text
	// (~98 bytes), so the chunked pull copies only that much while md5sum —
	// which does follow the link — hashes the real file, and every symlinked
	// file fails verification instead of downloading.
	info("解析远程路径...")
	var quoted []string
	for _, r := range remotes {
		quoted = append(quoted, "'"+strings.ReplaceAll(r, "'", `'\''`)+"'")
	}
	listing, err := ssh("for p in " + strings.Join(quoted, " ") + `; do for f in $p; do [ -f "$f" ] && [ -r "$f" ] && printf '%s\t%s\n' "$(stat -Lc %s "$f")" "$f"; done; done`)
	if err != nil && listing == "" {
		fail(fmt.Sprintf("列出远程文件失败: %v", err))
		os.Exit(1)
	}
	var files []*remoteFile
	var totalBytes int64
	seen := map[string]string{} // basename → first path that claimed it
	for _, l := range strings.Split(listing, "\n") {
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		szStr, path, cut := strings.Cut(l, "\t")
		if !cut {
			continue
		}
		sz, _ := strconv.ParseInt(szStr, 10, 64)
		base := filepath.Base(path)
		// Everything is flattened into dest, so two same-named files from
		// different directories would race each other's chunks into one local
		// file. Refuse rather than produce a corrupt download.
		if first, dup := seen[base]; dup {
			fail(fmt.Sprintf("文件名冲突: %s 与 %s 同名，都会写到 %s", first, path, filepath.Join(dest, base)))
			info("分开下载，或用 -o 指定不同目录")
			os.Exit(1)
		}
		seen[base] = path
		files = append(files, &remoteFile{path: path, base: base, size: sz})
		totalBytes += sz
	}
	if len(files) == 0 {
		fail("没有匹配到可读的远程文件")
		os.Exit(1)
	}
	for _, f := range files {
		info("  " + f.path)
	}
	info(fmt.Sprintf("共 %d 个文件, %s", len(files), humanBytes(totalBytes)))

	results, fetchErr := fetchAndVerify(files, dest)
	if fetchErr != nil {
		fail(fmt.Sprintf("下载失败: %v", fetchErr))
		info("已传输的部分会保留，重跑同一条命令可断点续传")
		os.Exit(1)
	}

	bad := 0
	for _, r := range results {
		if r != nil {
			bad++
		}
	}
	if bad > 0 {
		fail(fmt.Sprintf("%d 个文件校验未通过 — 重跑同一条命令可续传/重取", bad))
		os.Exit(1)
	}
	ok(fmt.Sprintf("全部完成 → %s", dest))
}

// ── Recv: files pushed from the cluster ──
//
// The cluster cannot open a connection to this machine. The VPN is
// split-tunnel and WSL2 sits behind NAT, so the only inbound path that exists
// at all is the reverse tunnel — and that one is owned by systemd and carries
// the API traffic claude/codex depend on, which a bulk upload would starve.
//
// So a push is a queue, not a connection: `spush` on the cluster appends the
// file's path to ~/.spod/outbox, and `spod recv` here claims the queue and
// pulls those files down the same path as `spod get` (parallel streams,
// MD5-verified, resumable). Nothing new has to listen anywhere, both clusters
// work identically, and a push made while this machine is asleep simply waits
// in the queue until the next `spod recv`.

// pushHelperVersion identifies the deployed copy of pushHelperScript. It is the
// script's own content hash rather than a hand-maintained number: the installer
// only rewrites the file when the marker differs, so a bump that someone forgot
// leaves a stale helper on the cluster with no sign of it.
func pushHelperVersion() string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(pushHelperScript)))[:12]
}

const pushHelperScript = `#!/bin/bash
# spod-push — 把集群上的文件排进队列，等本地的 spod recv 取走。
# 由 spod 自动部署（cmd/spod/main.go: pushHelperScript），手改会在下次连接时被覆盖。
set -u

SPOD_DIR="$HOME/.spod"
OUTBOX="$SPOD_DIR/outbox"
LOCK="$SPOD_DIR/outbox.lock"
RECEIPTS="$SPOD_DIR/receipts"
ALIVE="$SPOD_DIR/recv-alive"
TAB=$(printf '\t')

c_red=$(printf '\033[31m'); c_grn=$(printf '\033[32m'); c_blu=$(printf '\033[34m')
c_amb=$(printf '\033[33m'); c_gry=$(printf '\033[90m'); c_off=$(printf '\033[0m')
say()  { printf '  %s>%s %s\n' "$c_blu" "$c_off" "$*" >&2; }
yay()  { printf '  %s+%s %s\n' "$c_grn" "$c_off" "$*" >&2; }
oops() { printf '  %s!%s %s\n' "$c_amb" "$c_off" "$*" >&2; }
die()  { printf '  %sx%s %s\n' "$c_red" "$c_off" "$*" >&2; exit 1; }

human() {
    awk -v b="$1" 'BEGIN{split("B KB MB GB TB",u," ");i=1;while(b>=1024&&i<5){b/=1024;i++};
        if(i==1) printf "%d %s", b, u[i]; else printf "%.1f %s", b, u[i]}'
}

usage() {
    cat >&2 <<'USAGE_EOF'
  用法: spush [-d 子目录] [-w [秒]] [-f] <文件或目录>...
        spush -l          看队列
        spush -c          清空队列

  文件不是直接推过去的：集群连不到你的机器，只有本地能主动发起连接。
  spush 把路径排进 ~/.spod/outbox，本地的 spod recv 认领后并行拉走
  （4 路并发 + MD5 校验 + 断点续传），默认落到 Windows 的 Downloads。

    -d DIR    放进本地下载目录下的子目录
    -w [秒]   等本地确认收到（默认 600 秒；Ctrl-C 走开也不影响传输）
    -f        已经送达过的也重新排队（默认跳过，见下）
    -l        列出还没被取走的条目
    -c        清空队列

  内容没变、而且已经送达过的文件会被跳过，所以再推一次整个目录只排新东西，
  不会把上次已经取走的再拉一遍。确实要重发就加 -f。
USAGE_EOF
}

mkdir -p "$SPOD_DIR" 2>/dev/null || die "创建 $SPOD_DIR 失败"

now=$(date +%s)
sub=""; wait_secs=0; mode="push"; force=0
while [ $# -gt 0 ]; do
    case "$1" in
        -d|--dir)   shift; [ $# -gt 0 ] || die "-d 需要一个子目录名"; sub="$1"; shift ;;
        -w|--wait)  shift; wait_secs=600
                    case "${1:-}" in
                        ''|-*)      ;;
                        *[!0-9]*)   ;;
                        *) wait_secs="$1"; shift ;;
                    esac ;;
        -f|--force) force=1; shift ;;
        -l|--list)  mode="list";  shift ;;
        -c|--clear) mode="clear"; shift ;;
        -h|--help)  usage; exit 0 ;;
        --)         shift; break ;;
        -*)         die "未知参数: $1（spush -h 看用法）" ;;
        *)          break ;;
    esac
done

if [ "$mode" = "list" ]; then
    [ -s "$OUTBOX" ] || { say "队列是空的"; exit 0; }
    awk -F'\t' -v g="$c_gry" -v o="$c_off" -v now="$now" '
        function age(s) { if (s < 0) s = 0
                          if (s < 3600)  return sprintf("%dm", s / 60)
                          if (s < 86400) return sprintf("%dh", s / 3600)
                          return sprintf("%dd", s / 86400) }
        function hs(b) { split("B KB MB GB TB", u, " "); i = 1
                         while (b >= 1024 && i < 5) { b /= 1024; i++ }
                         return (i == 1) ? sprintf("%d %s", b, u[i]) : sprintf("%.1f %s", b, u[i]) }
        !seen[$2 SUBSEP $4]++ {
            n++; t += $3; d = ($2 == "-" ? "" : "  ->  " $2 "/")
            printf "    %s%s%s  (%s)%s\n", g, $4, d, age(now - $1), o
        }
        END{printf "  %d 个文件在队列里（%s）\n", n, hs(t)}' "$OUTBOX" >&2
    exit 0
fi

if [ "$mode" = "clear" ]; then
    n=$(wc -l < "$OUTBOX" 2>/dev/null || echo 0)
    ( flock -w 10 9 2>/dev/null; : > "$OUTBOX"; rm -f "$SPOD_DIR"/claim.* ) 9>>"$LOCK"
    yay "已清空队列（$n 条）"
    exit 0
fi

[ $# -gt 0 ] || { usage; exit 1; }

TMPC=$(mktemp "${TMPDIR:-/tmp}/spod-push.XXXXXX") || die "mktemp 失败"
TMPQ=$(mktemp "${TMPDIR:-/tmp}/spod-push.XXXXXX") || die "mktemp 失败"
trap 'rm -f "$TMPC" "$TMPQ"' EXIT

# Candidates first, queue lines later: whether a file is worth queueing depends
# on the outbox and the receipts, and those are read once for the whole batch.
add_file() {  # $1 绝对路径, $2 目标子目录（可空）
    f="$1"; s="${2:-}"
    case "$f$s" in
        *"$TAB"*) oops "跳过（路径含制表符）: $f"; return ;;
    esac
    [ -f "$f" ] || { oops "跳过（不是普通文件）: $f"; return; }
    [ -r "$f" ] || { oops "跳过（读不了）: $f"; return; }
    st=$(stat -Lc '%s %.Y' "$f" 2>/dev/null) || st="0 0"
    printf '%s\t%s\t%s\t%s\n' "${s:--}" "${st%% *}" "${st#* }" "$f" >> "$TMPC"
}

add_path() {
    p="$1"
    [ -e "$p" ] || { oops "跳过（不存在）: $p"; return; }
    abs=$(readlink -f "$p" 2>/dev/null) || abs="$p"
    case "$abs" in
        *"$TAB"*) oops "跳过（路径含制表符）: $p"; return ;;
    esac
    if [ -d "$abs" ]; then
        base=$(basename "$abs")
        while IFS= read -r f; do
            rel=${f#"$abs"/}
            dir=$(dirname "$rel")
            target="$base"
            [ "$dir" != "." ] && target="$base/$dir"
            [ -n "$sub" ] && target="$sub/$target"
            add_file "$f" "$target"
        done < <(find "$abs" -type f -print 2>/dev/null)
    else
        add_file "$abs" "$sub"
    fi
}

for p in "$@"; do add_path "$p"; done
[ -s "$TMPC" ] || die "没有可排队的文件"

# Two things must not be queued again: a path already sitting in the outbox, and
# a path the receipts say already arrived with this exact size and mtime.
# Without this, an agent that adds one file to results/ and re-runs
# "spush -d run7 results/" re-queues the whole directory, and spod recv pulls
# everything it already delivered — the queue accumulates what you have.
# Reading and appending happen under the same lock the claim side takes, so a
# concurrent push cannot slip a duplicate past the check — unless the lock did
# not come, in which case say so rather than race quietly.
: >> "$OUTBOX"; : >> "$RECEIPTS"
exec 9>>"$LOCK"
flock -w 10 9 2>/dev/null || oops "没拿到队列锁（等了 10s），继续 — 与并发的 spush 可能重复排队" 
plan=$(awk -F'\t' -v now="$now" -v force="$force" -v ob="$OUTBOX" -v rc="$RECEIPTS" -v tq="$TMPQ" '
    function base(p,   k, a) { k = split(p, a, "/"); return a[k] }
    FILENAME == ob { queued[$2 SUBSEP $4] = 1; next }
    FILENAME == rc {
        # Last word wins: a later failure retires an earlier delivery.
        if ($2 == "ok") { dsz[$3] = $5; dmt[$3] = $6; ddst[$3] = $4 }
        else            { delete dsz[$3]; delete dmt[$3]; delete ddst[$3] }
        next
    }
    {
        s = $1; sz = $2; mt = $3; path = $4
        # Queued is queued, even under -f: the drain fetches whatever the file
        # holds then, so a second identical line buys nothing but a longer queue.
        if ((s SUBSEP path) in queued) { nq++; next }
        # Concatenating "" forces a string compare. As numbers these go through
        # a double and a sub-microsecond mtime difference would vanish.
        if (!force && path in dsz && (dsz[path] "") == (sz "") && (dmt[path] "") == (mt "")) {
            nd++
            if (nd <= 5) dl[nd] = base(path) "  ->  " ddst[path]
            next
        }
        queued[s SUBSEP path] = 1
        printf("%s\t%s\t%s\t%s\n", now, s, sz, path) >> tq
        n++; t += sz
    }
    END {
        close(tq)
        printf "TALLY\t%d\t%d\t%d\t%d\n", n + 0, t + 0, nq + 0, nd + 0
        for (i = 1; i <= nd && i <= 5; i++) printf "DLIST\t%s\n", dl[i]
    }
' "$OUTBOX" "$RECEIPTS" "$TMPC")

set -- $(printf '%s\n' "$plan" | awk -F'\t' '$1 == "TALLY" { print $2, $3, $4, $5 }')
n=${1:-0}; total=${2:-0}; nq=${3:-0}; nd=${4:-0}

# Every candidate must come out somewhere. Without this an awk that died — bad
# receipts, a full /tmp — leaves an empty tally, and the arithmetic below reads
# it as "nothing new to queue" and exits 0: the push silently never happened.
ncand=$(wc -l < "$TMPC")
if [ "$((n + nq + nd))" -ne "$ncand" ] || [ "$(wc -l < "$TMPQ")" -ne "$n" ]; then
    die "排队失败（队列未改动）: $ncand 个候选只落实了 $((n + nq + nd)) 个"
fi

if [ "$nd" -gt 0 ]; then
    say "跳过 $nd 个已经送达过、内容也没变的（要重发: spush -f）"
    printf '%s\n' "$plan" | awk -F'\t' -v g="$c_gry" -v o="$c_off" \
        '$1 == "DLIST" { printf "    %s%s%s\n", g, $2, o }' >&2
    [ "$nd" -gt 5 ] && say "  ...还有 $((nd - 5)) 个"
fi
[ "$nq" -gt 0 ] && say "跳过 $nq 个已经在队列里的"

if [ "$n" -eq 0 ]; then
    yay "没有新东西要排队"
    exit 0
fi

cat "$TMPQ" >> "$OUTBOX"
flock -u 9 2>/dev/null
yay "已排队 $n 个文件（$(human $total)）"

alive=""
if [ -f "$ALIVE" ]; then
    age=$(( now - $(stat -c %Y "$ALIVE" 2>/dev/null || echo 0) ))
    [ "$age" -ge 0 ] && [ "$age" -lt 180 ] && alive=1
fi
if [ -n "$alive" ]; then
    say "本地接收端在线，马上就会开始拉"
else
    say "本地接收端没在跑 — 在本地执行 spod recv 就会取走（队列一直留着）"
fi

[ "$wait_secs" -gt 0 ] || exit 0

say "等本地确认收到（最多 ${wait_secs}s；Ctrl-C 走开不影响传输）..."
deadline=$((now + wait_secs))
while [ "$(date +%s)" -lt "$deadline" ]; do
    pending=0
    while IFS= read -r line; do
        path=$(printf '%s' "$line" | cut -f4)
        got=$(awk -F'\t' -v t="$now" -v p="$path" '$1+0>=t && $3==p {s=$2} END{print s}' "$RECEIPTS" 2>/dev/null)
        [ -z "$got" ] && pending=$((pending + 1))
    done < "$TMPQ"
    [ "$pending" -eq 0 ] && break
    sleep 3
done

rc=0
while IFS= read -r line; do
    path=$(printf '%s' "$line" | cut -f4)
    res=$(awk -F'\t' -v t="$now" -v p="$path" '$1+0>=t && $3==p {s=$2; d=$4} END{printf "%s\t%s", s, d}' "$RECEIPTS" 2>/dev/null)
    st=$(printf '%s' "$res" | cut -f1); dst=$(printf '%s' "$res" | cut -f2)
    case "$st" in
        ok)   yay "$(basename "$path")  ->  $dst" ;;
        fail) oops "$(basename "$path") 本地拉取失败（已重新排队，会再试）"; rc=1 ;;
        *)    oops "$(basename "$path") 还没被取走 — 队列保留，本地 spod recv 起来后会继续"; rc=1 ;;
    esac
done < "$TMPQ"
exit $rc
`

// pushItem is one queued file as the cluster recorded it.
type pushItem struct {
	ts    int64  // queued at (cluster clock)
	sub   string // sanitized destination subdirectory under dest, "" for none
	size  int64  // size at queue time — informational; the fetcher re-stats
	mtime string // source mtime as of the pre-fetch stat; goes into the receipt
	path  string // absolute path on the cluster
	raw   string // the queue line verbatim, so a failure can be re-queued as-is
	name  string // local filename under dest/sub — set by assignNames, not parsed
}

// parsePushLine decodes one outbox line: ts \t sub \t size \t abspath.
func parsePushLine(line string) (pushItem, bool) {
	line = strings.TrimRight(line, "\r\n")
	if strings.TrimSpace(line) == "" {
		return pushItem{}, false
	}
	f := strings.Split(line, "\t")
	if len(f) != 4 || !strings.HasPrefix(f[3], "/") {
		return pushItem{}, false
	}
	ts, err := strconv.ParseInt(f[0], 10, 64)
	if err != nil {
		return pushItem{}, false
	}
	size, _ := strconv.ParseInt(f[2], 10, 64)
	sub := ""
	if f[1] != "-" {
		sub = sanitizeSub(f[1])
	}
	return pushItem{ts: ts, sub: sub, size: size, path: f[3], raw: line}, true
}

// sanitizeSub keeps a queued destination subdirectory inside the download
// directory. The queue lives in a cluster home that may be shared with other
// people on the same account, so ".." — or an absolute path — must not be able
// to steer a write anywhere else on this machine.
func sanitizeSub(s string) string {
	var out []string
	for _, part := range strings.Split(filepath.ToSlash(s), "/") {
		switch part {
		case "", ".", "..", "~":
			continue
		}
		out = append(out, part)
		if len(out) == 8 { // depth cap: a queue line can't grow a deep tree here
			break
		}
	}
	return filepath.Join(out...)
}

// wslMntRe matches /mnt/c/... so a destination can be echoed back to the
// cluster in the form the user will actually type into Windows Explorer.
var wslMntRe = regexp.MustCompile(`^/mnt/([a-z])/(.*)$`)

func displayPath(p string) string {
	m := wslMntRe.FindStringSubmatch(p)
	if m == nil {
		return p
	}
	return strings.ToUpper(m[1]) + `:\` + strings.ReplaceAll(m[2], "/", `\`)
}

// ensurePushHelper deploys ~/.local/bin/spod-push on the cluster, skipping the
// write when the version marker already matches. ~/.local/bin is not on the
// non-interactive PATH there, which is why the bashrc block wraps it as
// `spush` with an absolute path rather than relying on PATH.
func ensurePushHelper() {
	installer := fmt.Sprintf(`mkdir -p ~/.local/bin ~/.spod
# ~/.local/bin IS on the login-shell PATH (it is missing only from the
# non-interactive one), so the symlink makes spush work in tmux sessions that
# were opened before the bashrc wrapper was written.
ln -sfn ~/.local/bin/spod-push ~/.local/bin/spush
have=$(sed -n 's/^# spod-push-version: //p' ~/.local/bin/spod-push 2>/dev/null | head -1)
if [ "$have" = "%s" ]; then echo UPTODATE; exit 0; fi
cat > ~/.local/bin/spod-push.new << 'SPOD_PUSH_EOF'
%s
# spod-push-version: %s
SPOD_PUSH_EOF
chmod 755 ~/.local/bin/spod-push.new && mv -f ~/.local/bin/spod-push.new ~/.local/bin/spod-push && echo INSTALLED`,
		pushHelperVersion(), pushHelperScript, pushHelperVersion())
	out, err := ssh(installer)
	if err != nil {
		warn(fmt.Sprintf("spush 助手部署失败: %v", err))
		return
	}
	if strings.Contains(out, "INSTALLED") {
		ok(fmt.Sprintf("已在 %s 上安装 spush（推文件回本地）", tgt.label))
	}
}

// agentNote is the block spod keeps in the cluster's ~/.claude/CLAUDE.md, so a
// Claude Code session running THERE knows how to hand a file back to the user.
// It cannot be left to the shell wrapper alone: an agent's Bash tool runs a
// non-login shell whose snapshot was taken before this feature existed, so the
// `spush` function is usually absent and only the absolute path works.
const agentNoteBegin = "<!-- spod-agent-begin -->"
const agentNoteEnd = "<!-- spod-agent-end -->"

const agentNoteBody = `## Sending a file back to the user's local machine

This cluster cannot open a connection to the user's machine. Queue the file with
the spod push helper instead — their local ` + "`spod recv`" + ` pulls it:

    ~/.local/bin/spush <file>...          # use the absolute path: the spush shell
                                          # function exists only in login shells
    ~/.local/bin/spush -d run7 results/   # a directory keeps its tree
    ~/.local/bin/spush -w 300 out.mp4     # wait up to 300s; exits 1 if it did not arrive
    ~/.local/bin/spush -l                 # what is still queued, and how long it has waited

A file the user already received, unchanged, is skipped rather than queued
again, so re-pushing a whole results/ directory sends only what is new; add -f
to send it anyway. Files land in the user's Windows Downloads folder (or
wherever their ` + "`spod recv`" + ` points). If nothing is receiving, spush says so and the entry stays queued — tell
the user to run ` + "`spod recv`" + ` locally rather than retrying. Queue a path that will
still exist later: the pull happens afterwards and fails if the job removed the
file. Throughput is ~1 MB/s, so tar or subset large results before queueing.

Managed by spod — edits here are overwritten on the next connect.`

// ensureAgentNote installs that block, replacing any previous copy.
//
// The blank separator has to be stripped in its own pass: sed's `$` matches the
// last line of the *input*, which is the end marker being deleted, so a
// same-pass `${/^$/d}` never sees the blank and the file grows by one line on
// every connect.
func ensureAgentNote() {
	script := fmt.Sprintf(`mkdir -p ~/.claude
touch ~/.claude/CLAUDE.md
sed -i '/%s/,/%s/d' ~/.claude/CLAUDE.md 2>/dev/null
awk 'NF{last=NR} {l[NR]=$0} END{for(i=1;i<=last;i++) print l[i]}' ~/.claude/CLAUDE.md > ~/.claude/CLAUDE.md.spodtmp &&
    mv -f ~/.claude/CLAUDE.md.spodtmp ~/.claude/CLAUDE.md
[ -s ~/.claude/CLAUDE.md ] && printf '\n' >> ~/.claude/CLAUDE.md
cat >> ~/.claude/CLAUDE.md << 'SPOD_NOTE_EOF'
%s
%s
%s
SPOD_NOTE_EOF`, agentNoteBegin, agentNoteEnd, agentNoteBegin, agentNoteBody, agentNoteEnd)
	if _, err := ssh(script); err != nil {
		warn(fmt.Sprintf("集群侧 agent 说明写入失败: %v", err))
	}
}

// claimQueue takes ownership of everything queued on the cluster.
//
// The outbox is *renamed* under flock rather than truncated: ssh() retries a
// reset connection, and a truncate that succeeded remotely but whose output
// never made it back would silently drop every queued path. A claim file
// survives that, and any left behind by an earlier interrupted run is picked
// up by the next claim.
func claimQueue() ([]pushItem, []string, error) {
	out, err := ssh(`mkdir -p ~/.spod
cd ~/.spod || exit 1
: >> outbox
exec 9>>outbox.lock
flock -w 10 9 2>/dev/null
[ -s outbox ] && mv -f outbox "claim.$$.$(date +%s)"
touch outbox
flock -u 9 2>/dev/null
for c in claim.*; do
    [ -e "$c" ] || continue
    printf 'CLAIM\t%s\n' "$c"
    cat "$c"
done`)
	if err != nil {
		return nil, nil, err
	}
	var items []pushItem
	var claims []string
	for _, line := range strings.Split(out, "\n") {
		if name, isClaim := strings.CutPrefix(line, "CLAIM\t"); isClaim {
			claims = append(claims, strings.TrimSpace(name))
			continue
		}
		if it, valid := parsePushLine(line); valid {
			items = append(items, it)
		}
	}
	return items, claims, nil
}

// dropClaims deletes claim files once their contents are safely recorded in the
// local inflight file, and reports whether the cluster is now clean.
//
// This has to actually work: a claim file left behind is picked up by the next
// claim, so a silently-failing delete turns the watch loop into an endless
// re-download of the same files. Names are passed relative to ~/.spod — they go
// through shellQuote, which single-quotes, so a "$HOME/..." prefix would never
// be expanded.
func dropClaims(claims []string) bool {
	if len(claims) == 0 {
		return true
	}
	var q []string
	for _, c := range claims {
		if strings.ContainsAny(c, "/ \t") || !strings.HasPrefix(c, "claim.") {
			continue // only ever remove names this code produced
		}
		q = append(q, shellQuote(c))
	}
	if len(q) == 0 {
		return true
	}
	out, err := ssh("cd ~/.spod && rm -f " + strings.Join(q, " ") + "; ls claim.* 2>/dev/null | wc -l")
	if err != nil {
		warn(fmt.Sprintf("清理队列文件失败: %v", err))
		return false
	}
	if strings.TrimSpace(out) != "0" {
		warn(fmt.Sprintf("集群上还留着 %s 个队列文件，手动清: ssh %s 'rm ~/.spod/claim.*'", strings.TrimSpace(out), host))
		return false
	}
	return true
}

// requeue puts lines back on the cluster's outbox after a failed pull, so the
// next drain (or the next machine) retries them instead of losing them.
func requeue(items []pushItem) {
	if len(items) == 0 {
		return
	}
	var b strings.Builder
	b.WriteString("mkdir -p ~/.spod\n( flock -w 10 9 2>/dev/null; cat >> ~/.spod/outbox ) 9>>~/.spod/outbox.lock << 'SPOD_REQ_EOF'\n")
	for _, it := range items {
		b.WriteString(it.raw + "\n")
	}
	b.WriteString("SPOD_REQ_EOF\n")
	if _, err := ssh(b.String()); err != nil {
		warn(fmt.Sprintf("重新排队失败（%d 条）: %v", len(items), err))
	}
}

type receipt struct {
	status string // "ok" | "fail"
	path   string // the cluster-side path, as queued
	dest   string // where it landed locally, in Windows form when applicable
	// The delivered file's size and mtime. `spush` skips re-queueing a path
	// whose receipt says it already arrived with exactly this stamp, which is
	// what stops a re-pushed directory from dragging its whole history along.
	size  int64
	mtime string
}

// writeReceipts reports back what arrived. It is two things at once: the log
// `spush -w` polls to say "delivered", and the ledger `spush` consults to avoid
// re-queueing a file that is already here unchanged.
//
// The timestamp comes from the *cluster's* clock, not this machine's: both
// readers compare it against cluster-side times (`spush -w` against its own
// `date +%s`), so a laptop drifting from the cluster must not be the one
// stamping it.
func writeReceipts(rs []receipt) {
	if len(rs) == 0 {
		return
	}
	var b strings.Builder
	b.WriteString("mkdir -p ~/.spod\nnow=$(date +%s)\n")
	b.WriteString("awk -v t=\"$now\" 'BEGIN{OFS=\"\\t\"} {print t, $0}' >> ~/.spod/receipts << 'SPOD_RCPT_EOF'\n")
	for _, r := range rs {
		fmt.Fprintf(&b, "%s\t%s\t%s\t%d\t%s\n", r.status, r.path, r.dest, r.size, r.mtime)
	}
	b.WriteString("SPOD_RCPT_EOF\n")
	// The ledger has to outlive one big drop: at 200 lines a 168-file batch
	// would push its own earlier entries out, and `spush` would offer to send
	// them all again.
	b.WriteString("tail -n 2000 ~/.spod/receipts > ~/.spod/receipts.tmp && mv -f ~/.spod/receipts.tmp ~/.spod/receipts\n")
	if _, err := ssh(b.String()); err != nil {
		warn(fmt.Sprintf("回执写入失败: %v", err))
	}
}

// inflightPath records what this machine claimed but has not finished pulling,
// so a Ctrl-C or a crash re-tries those files instead of dropping them (the
// cluster's copy of those lines is already gone by then).
func inflightPath() string { return tgt.tmp("recv-inflight") }

func loadInflight() []pushItem {
	b, err := os.ReadFile(inflightPath())
	if err != nil {
		return nil
	}
	var items []pushItem
	for _, line := range strings.Split(string(b), "\n") {
		if it, valid := parsePushLine(line); valid {
			items = append(items, it)
		}
	}
	return items
}

func saveInflight(items []pushItem) {
	if len(items) == 0 {
		os.Remove(inflightPath())
		return
	}
	var b strings.Builder
	for _, it := range items {
		b.WriteString(it.raw + "\n")
	}
	os.WriteFile(inflightPath(), []byte(b.String()), 0600)
}

// dedupePush collapses repeats of the same (subdir, path) — an entry recovered
// from inflight is usually also the one just claimed.
func dedupePush(items []pushItem) []pushItem {
	seen := map[string]bool{}
	var out []pushItem
	for _, it := range items {
		k := it.sub + "\x00" + it.path
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, it)
	}
	return out
}

// assignNames picks the local filename for every queued file. Usually that is
// its basename, but a batch is flattened into one directory and nothing stops
// two queued paths from sharing a basename — decoded_e24c_anchor/compare.png
// and decoded_e25_fanchor/compare.png are one plot from two runs. Left alone
// they are fetched one after another into the same local path, the last one
// replaces the others, and all of them get an "ok" receipt, so the loss is
// silent. `spod get` refuses such a command and tells the user to split it; a
// drain has nobody to ask and must not drop what it has already claimed, so it
// prefixes the directory the file came from instead.
func assignNames(items []pushItem) []pushItem {
	out := append([]pushItem(nil), items...)

	// Only files landing in the same directory can collide.
	subs := map[string][]int{}
	for i := range out {
		out[i].name = filepath.Base(out[i].path)
		subs[out[i].sub] = append(subs[out[i].sub], i)
	}

	for _, idxs := range subs {
		count := map[string]int{}
		for _, i := range idxs {
			count[out[i].name]++
		}
		taken := map[string]bool{}
		var contested []int
		for _, i := range idxs {
			if count[out[i].name] > 1 {
				contested = append(contested, i)
				continue
			}
			taken[out[i].name] = true // uncontested names keep the plain basename
		}
		// Queue order, so the same queue always yields the same names and a
		// resumed transfer still finds its sidecar.
		for _, i := range contested {
			out[i].name = freeName(out[i].path, taken)
			taken[out[i].name] = true
		}
	}
	return out
}

// freeName walks up from the file's own directory, prefixing one more ancestor
// each round, until the flattened name is not already spoken for. A path deep
// enough to keep colliding settles for a digest of itself — ugly, but unique
// and derived only from the remote path, so a retry picks the same name.
func freeName(path string, taken map[string]bool) string {
	base := filepath.Base(path)
	prefix := ""
	dir := filepath.Dir(path)
	for depth := 0; depth < 4 && dir != "/" && dir != "." && dir != ""; depth++ {
		prefix = filepath.Base(dir) + "__" + prefix
		if n := prefix + base; !taken[n] {
			return n
		}
		dir = filepath.Dir(dir)
	}
	sum := sha256.Sum256([]byte(path))
	return fmt.Sprintf("%x-%s", sum[:4], base)
}

// pushBatch is one parallelFetch call's worth of queued files: same
// destination subdirectory, no two files sharing a local name.
type pushBatch struct {
	sub   string
	items []pushItem
}

// batchBySub groups queued files into fetch batches. parallelFetch flattens
// everything into one directory, so a batch may hold each basename only once —
// same-named files from different directories go into later batches instead of
// racing each other's chunks into one local file (cmdGet refuses that case; a
// queue can't ask the user, so it serializes instead).
func batchBySub(items []pushItem) []pushBatch {
	var order []string
	bySub := map[string][]pushItem{}
	for _, it := range items {
		if _, seen := bySub[it.sub]; !seen {
			order = append(order, it.sub)
		}
		bySub[it.sub] = append(bySub[it.sub], it)
	}
	var out []pushBatch
	for _, sub := range order {
		for len(bySub[sub]) > 0 {
			var batch, rest []pushItem
			taken := map[string]bool{}
			for _, it := range bySub[sub] {
				name := it.name
				if name == "" {
					name = filepath.Base(it.path)
				}
				if taken[name] {
					rest = append(rest, it)
					continue
				}
				taken[name] = true
				batch = append(batch, it)
			}
			out = append(out, pushBatch{sub, batch})
			bySub[sub] = rest
		}
	}
	return out
}

// statQueued re-stats queued paths on the cluster, returning current sizes for
// the ones that are still readable regular files.
//
// A queued path can rot: the queue outlives the job that wrote it, and a file
// that has been deleted (or silly-renamed by NFS) would fail every fetch. Left
// alone such an entry is re-queued after each failure and the watch loop claims
// it straight back — a poison entry that spins forever. Checking first lets
// drainOnce drop it with a receipt instead.
func statQueued(items []pushItem) map[string]fileStamp {
	var quoted []string
	seen := map[string]bool{}
	for _, it := range items {
		if seen[it.path] {
			continue
		}
		seen[it.path] = true
		quoted = append(quoted, shellQuote(it.path))
	}
	// `%.Y` is the mtime with fractional seconds. Whole seconds would call a
	// file unchanged when a job rewrote it to the same length within the same
	// second — the stamp has to be finer than the thing it is watching.
	out, err := ssh("for p in " + strings.Join(quoted, " ") + `; do
    [ -f "$p" ] && [ -r "$p" ] && printf '%s\t%s\n' "$(stat -Lc '%s %.Y' "$p")" "$p"
done
echo STATDONE`)
	// Only a *complete* listing may be trusted, so the marker has to be the last
	// line, not merely present: a reply cut short mid-stream makes every path
	// past the cut look deleted, and the caller drops those with a "fail"
	// receipt — turning a flaky connection into silent data loss.
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[len(lines)-1]) != "STATDONE" {
		if err != nil {
			warn(fmt.Sprintf("检查队列文件失败: %v", err))
		}
		return nil // "unknown" — the fetch will report the real error
	}
	stamps := map[string]fileStamp{}
	for _, line := range lines {
		nums, path, cut := strings.Cut(strings.TrimSpace(line), "\t")
		if !cut {
			continue
		}
		szStr, mtStr, split := strings.Cut(nums, " ")
		if !split {
			continue
		}
		// mtime stays a string: it is only ever compared for equality, and
		// 19 significant digits do not survive a round trip through a float.
		if sz, szErr := strconv.ParseInt(szStr, 10, 64); szErr == nil && mtStr != "" {
			stamps[path] = fileStamp{size: sz, mtime: mtStr}
		}
	}
	return stamps
}

// fileStamp identifies one version of a file: re-stat it later and a different
// size or mtime means the content may have changed.
type fileStamp struct {
	size  int64
	mtime string
}

// drainOnce claims the cluster's queue and pulls everything in it. It returns
// how many files landed and verified, and whether the drain settled — false
// means work was left on the cluster (an undeleted claim, or files re-queued
// after a failure) that would be claimed straight back, so the caller must back
// off instead of looping into it.
func drainOnce(dest string) (int, bool) {
	claimed, claims, err := claimQueue()
	if err != nil {
		warn(fmt.Sprintf("读取 %s 队列失败: %v", tgt.label, err))
		return 0, false
	}
	items := dedupePush(append(loadInflight(), claimed...))
	if len(items) == 0 {
		return 0, dropClaims(claims)
	}
	// Record before deleting the cluster's copy — this file is the only record
	// of the claim from here on.
	saveInflight(items)
	clean := dropClaims(claims)

	// Drop entries whose source no longer exists, with a receipt, rather than
	// re-queueing them forever.
	if stamps := statQueued(items); stamps != nil {
		var live []pushItem
		var gone []receipt
		for _, it := range items {
			st, exists := stamps[it.path]
			if !exists {
				warn(fmt.Sprintf("源文件已不在集群上，丢弃: %s", it.path))
				gone = append(gone, receipt{status: "fail", path: it.path})
				continue
			}
			it.size, it.mtime = st.size, st.mtime
			live = append(live, it)
		}
		writeReceipts(gone)
		items = live
		if len(items) == 0 {
			saveInflight(nil)
			return 0, clean
		}
	}

	items = assignNames(items)
	var totalBytes int64
	for _, it := range items {
		totalBytes += it.size
		if b := filepath.Base(it.path); it.name != b {
			info(fmt.Sprintf("同名文件，改名保存: %s → %s", b, it.name))
		}
	}
	info(fmt.Sprintf("收到 %d 个文件（%s）", len(items), humanBytes(totalBytes)))

	var receipts []receipt
	var failed []pushItem
	done := 0

	for _, batch := range batchBySub(items) {
		outDir := filepath.Join(dest, batch.sub)
		if err := os.MkdirAll(outDir, 0755); err != nil {
			fail(fmt.Sprintf("创建目录失败: %v", err))
			failed = append(failed, batch.items...)
			continue
		}
		files := make([]*remoteFile, 0, len(batch.items))
		owner := map[*remoteFile]pushItem{}
		for _, it := range batch.items {
			rf := &remoteFile{path: it.path, base: it.name, size: it.size}
			files = append(files, rf)
			owner[rf] = it
		}
		results, batchErr := fetchAndVerify(files, outDir)
		for _, rf := range files {
			it := owner[rf]
			if batchErr != nil {
				failed = append(failed, it)
				receipts = append(receipts, receipt{status: "fail", path: it.path})
				continue
			}
			if results[rf] != nil {
				failed = append(failed, it)
				receipts = append(receipts, receipt{status: "fail", path: it.path, dest: displayPath(filepath.Join(outDir, rf.base))})
				continue
			}
			done++
			receipts = append(receipts, receipt{
				status: "ok", path: it.path,
				dest:  displayPath(filepath.Join(outDir, rf.base)),
				size:  it.size,
				mtime: it.mtime,
			})
		}
		if batchErr != nil {
			fail(fmt.Sprintf("拉取失败: %v", batchErr))
			info("已传输的部分保留，重新排队后会断点续传")
		}
	}

	saveInflight(nil)
	requeue(failed)
	writeReceipts(receipts)
	if done > 0 {
		ok(fmt.Sprintf("已收下 %d 个文件 → %s", done, displayPath(dest)))
	}
	return done, clean && len(failed) == 0
}

// waitForPush blocks on the cluster until something is queued, touching the
// heartbeat the push helper reads so `spush` can tell the user whether anyone
// is listening. Returns true when there is work; false on timeout or error.
func waitForPush() bool {
	out, err := ssh(`mkdir -p ~/.spod
for i in $(seq 1 60); do
    touch ~/.spod/recv-alive 2>/dev/null
    [ -s ~/.spod/outbox ] && { echo READY; exit 0; }
    ls ~/.spod/claim.* >/dev/null 2>&1 && { echo READY; exit 0; }
    sleep 3
done
echo IDLE`)
	if err != nil {
		if !vpnTunnelUp() {
			warn("VPN 断了，等它回来...")
		} else {
			warn(fmt.Sprintf("等待队列时 SSH 出错: %v", err))
		}
		return false
	}
	return strings.Contains(out, "READY")
}

// heartbeat keeps ~/.spod/recv-alive fresh during a long transfer, when no
// waitForPush call is running to touch it.
func heartbeat(stop <-chan struct{}) {
	t := time.NewTicker(60 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			ssh("mkdir -p ~/.spod && touch ~/.spod/recv-alive")
		}
	}
}

func cmdRecvStatus() {
	out, err := ssh(`mkdir -p ~/.spod
q="0 0 0"
if [ -s ~/.spod/outbox ]; then
    q=$(awk -F'\t' -v now="$(date +%s)" '
        !seen[$2 SUBSEP $4]++ { n++; t += $3; if (m == "" || $1 + 0 < m) m = $1 + 0 }
        END { printf "%d %d %d", n + 0, t + 0, (m == "" ? 0 : now - m) }' ~/.spod/outbox) || q="0 0 0"
fi
echo "QUEUE $q"
if [ -f ~/.spod/recv-alive ]; then
    echo "ALIVE $(( $(date +%s) - $(stat -c %Y ~/.spod/recv-alive) ))"
else
    echo "ALIVE -1"
fi
echo RECENT
tail -n 5 ~/.spod/receipts 2>/dev/null || true`)
	if err != nil {
		fail(fmt.Sprintf("查询 %s 队列失败: %v", tgt.label, err))
		os.Exit(1)
	}
	inRecent := false
	var recent []string
	for _, line := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(line, "QUEUE "):
			var n int
			var total, age int64
			fmt.Sscanf(line, "QUEUE %d %d %d", &n, &total, &age)
			switch {
			case n == 0:
				info("队列: 空")
			case age >= 3600:
				// A queue nobody drains just grows; say so rather than let a
				// four-day-old entry ride along with the next push unnoticed.
				ok(fmt.Sprintf("队列: %d 个文件待取（%s，最早的已等 %s）",
					n, humanBytes(total), humanAge(age)))
			default:
				ok(fmt.Sprintf("队列: %d 个文件待取（%s）", n, humanBytes(total)))
			}
		case strings.HasPrefix(line, "ALIVE "):
			var age int64
			fmt.Sscanf(line, "ALIVE %d", &age)
			switch {
			case age < 0:
				warn("接收端: 从来没连上过（本地跑 " + spodCmd() + " recv）")
			case age < 180:
				ok(fmt.Sprintf("接收端: 在线（%d 秒前）", age))
			default:
				warn(fmt.Sprintf("接收端: 已离线 %s", humanAge(age)))
			}
		case line == "RECENT":
			inRecent = true
		case inRecent && strings.TrimSpace(line) != "":
			recent = append(recent, line)
		}
	}
	if len(recent) > 0 {
		info("最近送达:")
		for _, r := range recent {
			f := strings.Split(r, "\t")
			if len(f) < 4 {
				continue
			}
			mark := "✓"
			if f[1] != "ok" {
				mark = "✗"
			}
			fmt.Fprintf(os.Stderr, "    %s%s %s → %s%s\n", cGray, mark, filepath.Base(f[2]), f[3], reset)
		}
	}
	if n := len(loadInflight()); n > 0 {
		warn(fmt.Sprintf("本机还有 %d 条认领后没传完的，下次 %s recv 会续传", n, spodCmd()))
	}
}

func cmdRecvClear() {
	out, err := ssh(`mkdir -p ~/.spod
exec 9>>~/.spod/outbox.lock
flock -w 10 9 2>/dev/null
n=$(wc -l < ~/.spod/outbox 2>/dev/null || echo 0)
: > ~/.spod/outbox
rm -f ~/.spod/claim.*
echo "$n"`)
	if err != nil {
		fail(fmt.Sprintf("清空队列失败: %v", err))
		os.Exit(1)
	}
	os.Remove(inflightPath())
	ok(fmt.Sprintf("已清空 %s 的队列（%s 条）", tgt.label, strings.TrimSpace(out)))
}

func cmdRecv(args []string) {
	mode, dest := "watch", ""
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-o", "--out":
			if i+1 >= len(args) {
				fail("-o 需要一个目标目录")
				os.Exit(1)
			}
			dest = args[i+1]
			i++
		case "once", "one":
			mode = "once"
		case "status":
			mode = "status"
		case "clear":
			mode = "clear"
		case "-h", "--help":
			info(fmt.Sprintf("用法: %s recv [once|status|clear] [-o <本地目录>]", spodCmd()))
			info(fmt.Sprintf("在 %s 上用 spush <文件> 推送，这里负责取回", tgt.label))
			return
		default:
			fail(fmt.Sprintf("未知参数: %s", args[i]))
			info(fmt.Sprintf("用法: %s recv [once|status|clear] [-o <本地目录>]", spodCmd()))
			os.Exit(1)
		}
	}

	ensureVPN()
	if mode == "status" {
		cmdRecvStatus()
		return
	}
	if mode == "clear" {
		cmdRecvClear()
		return
	}

	if dest == "" {
		dest = defaultDownloadDir()
	}
	dest = toWSLPath(dest)
	if err := os.MkdirAll(dest, 0755); err != nil {
		fail(fmt.Sprintf("创建目录失败: %v", err))
		os.Exit(1)
	}
	ensurePushHelper()
	ensureAgentNote()

	if mode == "once" {
		if n, _ := drainOnce(dest); n == 0 {
			info("队列是空的")
		}
		return
	}

	ok(fmt.Sprintf("接收中：%s 上 spush 推来的文件 → %s", tgt.label, displayPath(dest)))
	info(fmt.Sprintf("在 %s 上执行 spush <文件>（-w 可等回执），Ctrl-C 退出", tgt.label))
	stop := make(chan struct{})
	defer close(stop)
	go heartbeat(stop)
	for {
		n, clean := drainOnce(dest)
		switch {
		case !clean:
			// Something is still claimed on the cluster; re-claiming it
			// immediately would re-download the same files in a tight loop.
			time.Sleep(30 * time.Second)
		case n == 0 && !waitForPush():
			time.Sleep(5 * time.Second)
		}
	}
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
	fmt.Fprintf(os.Stderr, "\n  %s%s%s Sessions%s\n", bold, cPurple, tgt.label, reset)
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
// The claude wrapper also exports CLAUDE_CODE_OAUTH_TOKEN from ~/.claude/oauth_token
// when that file exists, so a cluster can run on its own long-lived grant instead
// of a copied .credentials.json (whose single-use refresh token the first machine
// to refresh consumes, leaving the other wiped with "Login expired").
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
sed -i '/# spod-proxy/,/# spod-proxy-end/d; /# spod-proxy/d; /^export [hH][tT][tT][pP][sS]*_[pP][rR][oO][xX][yY]=.*127\.0\.0\.1/d; /^export [nN][oO]_[pP][rR][oO][xX][yY]=/d; /^_spod_proxy=/d; /^_spod_claude_bin=/d; /^_spod_codex_bin=/d; /^claude()/d; /^codex()/d; /^spush()/d; /^unset .*_proxy.*spod/d' ~/.bashrc 2>/dev/null
cat >> ~/.bashrc << 'SPOD_EOF'

%s
unset http_proxy https_proxy HTTP_PROXY HTTPS_PROXY no_proxy NO_PROXY 2>/dev/null # spod: clear stale proxy
_spod_proxy="http://127.0.0.1:%s"
# spod: ~/.claude/oauth_token (from a local 'claude setup-token') gives this host its own long-lived grant;
# a shared .credentials.json breaks because refresh tokens are single-use (first machine to refresh wins)
claude() { local _tok=0; if [ -z "${CLAUDE_CODE_OAUTH_TOKEN:-}" ] && [ -r "$HOME/.claude/oauth_token" ]; then export CLAUDE_CODE_OAUTH_TOKEN="$(<"$HOME/.claude/oauth_token")"; _tok=1; fi; export http_proxy="$_spod_proxy" https_proxy="$_spod_proxy" HTTP_PROXY="$_spod_proxy" HTTPS_PROXY="$_spod_proxy"; command claude "$@"; local rc=$?; unset http_proxy https_proxy HTTP_PROXY HTTPS_PROXY; [ "$_tok" = 1 ] && unset CLAUDE_CODE_OAUTH_TOKEN; return $rc; }
codex() { export http_proxy="$_spod_proxy" https_proxy="$_spod_proxy" HTTP_PROXY="$_spod_proxy" HTTPS_PROXY="$_spod_proxy"; command codex "$@"; local rc=$?; unset http_proxy https_proxy HTTP_PROXY HTTPS_PROXY; return $rc; }
# spod: push files back to the local machine (queued here, pulled by 'spod recv' there).
# ~/.local/bin is not on the non-interactive PATH, so call it by absolute path.
spush() { "$HOME/.local/bin/spod-push" "$@"; }
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
# Cap concurrent handlers. During a long tunnel outage, every retry from
# claude/codex would otherwise spawn a fresh handler that sits in the 900s
# reconnect loop, accumulating threads + sockets without bound. Drop the
# overflow immediately so the client sees a fast reset and retries later.
MAX_INFLIGHT=50; SEM=threading.Semaphore(MAX_INFLIGHT)
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
    if not SEM.acquire(blocking=False):
        if LOG:print(f"[relay] backpressure: dropped (>={MAX_INFLIGHT} inflight)",flush=True)
        try:c.close()
        except:pass
        return
    try:
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
    finally:
        SEM.release()
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
	//
	// Shared-account safety: the SuperPod home — and therefore this relay plus
	// the claude/codex proxy in ~/.bashrc — is shared by every machine that
	// logs into the same account. A second machine running spod must NOT tear
	// down a relay another machine already started: `pkill -f spod-relay.py`
	// kills ALL of them regardless of port, so two machines on different port
	// schemes murder each other's relay on every spod run — a reconnect war
	// that starves the proxy and looks exactly like "relay broken". So if a
	// spod relay is already running (ANY ports — the other machine may use a
	// different scheme), ADOPT it and point our proxy at its ports instead of
	// recreating one.
	//
	// Escape hatch: SPOD_FORCE_RELAY=1 tears down whatever is running and
	// rebinds the relay to THIS machine's tunnel — use it to reclaim the exit
	// when the adopted relay routes through a dead/blocked upstream.
	force := "0"
	if os.Getenv("SPOD_FORCE_RELAY") == "1" {
		force = "1"
	}
	script := fmt.Sprintf(
		`mkdir -p ~/.local/bin ~/.local/share
NEW_SCRIPT=$(cat << 'RELAY_SCRIPT'
%s
RELAY_SCRIPT
)
OLD_SCRIPT=""
[ -f ~/.local/bin/spod-relay.py ] && OLD_SCRIPT=$(cat ~/.local/bin/spod-relay.py)
FORCE="%s"
# Port-agnostic: find any spod-relay this account is already running and read
# back its actual <upstream listen> ports (a peer machine may differ from us).
RUNNING=$(pgrep -u $(id -u) -af 'spod-relay\.py' 2>/dev/null | grep -oE 'spod-relay\.py +[0-9]+ +[0-9]+' | head -1)
RUN_UP=$(echo "$RUNNING" | awk '{print $2}')
RUN_DOWN=$(echo "$RUNNING" | awk '{print $3}')
if [ "$FORCE" != "1" ] && [ "$NEW_SCRIPT" = "$OLD_SCRIPT" ] && [ -n "$RUN_UP" ]; then
    echo "adopt $RUN_UP $RUN_DOWN"
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
		force,
		tunnelPort, relayPort,
		tunnelPort, relayPort,
	)
	out, err := ssh(script)
	if err != nil {
		warn(fmt.Sprintf("Relay 部署失败: %v", err))
		return false
	}
	fields := strings.Fields(strings.TrimSpace(out))
	switch {
	case len(fields) == 3 && fields[0] == "adopt":
		adoptUp, adoptDown := fields[1], fields[2]
		if adoptUp != tunnelPort || adoptDown != relayPort {
			info(fmt.Sprintf("采纳已运行的 relay (:%s→:%s)，未重建（共享账号，避免互相踢）", adoptDown, adoptUp))
			info("要改用本机出口接管： SPOD_FORCE_RELAY=1 spod")
		} else {
			ok(fmt.Sprintf("Relay 运行中 (:%s→:%s)", relayPort, tunnelPort))
		}
		// Point the proxy at the relay that actually exists.
		tunnelPort, relayPort = adoptUp, adoptDown
		return true
	case len(fields) >= 1 && fields[0] == "started":
		ok(fmt.Sprintf("Relay 已启动 (:%s → :%s，隧道断开时自动等待重连)", relayPort, tunnelPort))
		return true
	case len(fields) == 1 && fields[0] == "failed":
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
	var relayOK bool
	if os.Getenv("SPOD_RIDER") == "1" {
		// Rider borrows the provider's already-running relay — never deploy or
		// pkill it on the shared account. Still compute ports so the proxy can
		// point at the live relay port the provider published.
		ensurePorts()
		relayOK = relayPort != ""
	} else {
		relayOK = ensureRelay()
	}
	ensureTmuxConfAndProxy(relayOK)
	ensurePushHelper()
	ensureAgentNote()
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
	// No conda env at all → this cluster was never set up for claude/codex
	// (HPC4, a fresh account). Stay silent instead of warning "can't auto-fix"
	// on every connect; the tunnel and tmux session still work.
	probe := `ENV_BIN="$HOME/.conda/envs/claude/bin"
[ -d "$ENV_BIN" ] || { echo "NOENV"; exit 0; }
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
	if strings.TrimSpace(out) == "NOENV" {
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
		warn(fmt.Sprintf("手动修复: ssh %s 后激活 claude env，运行 npm install -g @anthropic-ai/claude-code @openai/codex", host))
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
		fail(fmt.Sprintf("用法: %s kill <name>", spodCmd()))
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
		ok(fmt.Sprintf("已同步 %d 个凭证文件到 %s", synced, tgt.label))
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
		{"spod get <路径>...", "拉文件到本地（默认 Windows Downloads，带 MD5 校验）"},
		{"spod recv", "接收集群上 spush 推来的文件（守着队列，Ctrl-C 退出）"},
		{"spod recv once", "把队列里的文件取一次就退出"},
		{"spod recv status", "看队列 / 接收端在线状态"},
		{"spod sync <r> <l>", "从 SuperPod 并行 rsync 到本地"},
		{"spod sync stop", "停止所有 rsync"},
		{"spod speed [秒]", "VPN 隧道测速（默认 60s）"},
		{"spod ssh", "裸 SSH（不用 tmux）"},
		{"spod rider", "借用 provider 的 SOCKS，自动连到 relay 活的登录节点"},
		{"spod creds", "同步本地凭证到 SuperPod"},
		{"spod uptime", "查看 login 节点启动时间和负载"},
		{"spod hpc4 <子命令>", "同样的命令，但走 HPC4（隧道/relay/端口全独立）"},
	}
	for _, c := range cmds {
		fmt.Fprintf(os.Stderr, "    %s%-22s%s %s%s%s\n", cBlue, c[0], reset, cGray, c[1], reset)
	}
	fmt.Fprintf(os.Stderr, "\n  %s集群 → 本地推文件%s\n", bold, reset)
	fmt.Fprintf(os.Stderr, "  %s────────────────────────────────────%s\n", cGray, reset)
	for _, l := range []string{
		"集群连不到本机，所以推送 = 排队 + 本地拉取（走 spod get 那条并行通道）：",
		"  本地：spod recv                    守着，推一个取一个",
		"  集群：spush out.mp4                排进队列",
		"  集群：spush -d run7 -w ckpt/*.pt   放进子目录，并等回执",
	} {
		fmt.Fprintf(os.Stderr, "    %s%s%s\n", cGray, l, reset)
	}
	fmt.Fprintf(os.Stderr, "\n  %sHPC4%s\n", bold, reset)
	fmt.Fprintf(os.Stderr, "  %s────────────────────────────────────%s\n", cGray, reset)
	for _, l := range []string{
		"上面除 vpn/rider 外的子命令都能加 hpc4 前缀，两个集群可同时用：",
		"  spod              spod hpc4            两套 tmux 会话",
		"  spod get ...      spod hpc4 get ...    两套隧道/relay/SOCKS 端口",
		".env 需要 HPC4_USER；VPN_HOSTS 要包含 hpc4.ust.hk（否则没有 VPN 路由）。",
	} {
		fmt.Fprintf(os.Stderr, "    %s%s%s\n", cGray, l, reset)
	}
	fmt.Fprintln(os.Stderr)
}

func main() {
	dispatch(os.Args[1:])
}

// dispatch runs one spod command. A leading cluster name ("hpc4") repoints
// every per-cluster global and re-enters with the remaining arguments, so each
// subcommand works against either cluster without a duplicated switch — and
// `spod` and `spod hpc4` can run side by side.
//
// Cost of that shape: a cluster name can no longer be a tmux session name.
// `spod hpc4` means the cluster, not a session called "hpc4".
func dispatch(args []string) {
	cmd := ""
	if len(args) > 0 {
		cmd = args[0]
	}

	switch cmd {
	case "-h", "--help", "help":
		cmdHelp()
	case "hpc4", "superpod":
		useTarget(cmd)
		dispatch(args[1:])
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
	case "get":
		cmdGet(args[1:])
	case "recv":
		cmdRecv(args[1:])
	case "push":
		// Pushing happens ON the cluster — this is the mistake people make
		// first, so say where the command actually lives.
		fail("推送是在集群上发起的，不是这里")
		info(fmt.Sprintf("在 %s 上: spush <文件>...（%s recv 在本地取回）", tgt.label, spodCmd()))
		info(fmt.Sprintf("本地往集群传文件用: scp <文件> %s:<路径>", host))
		os.Exit(1)
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
	case "rider":
		sessionArg := ""
		if len(args) > 1 {
			sessionArg = args[1]
		}
		riderConnect(sessionArg)
	case "":
		ensureTunnel()
		cmdInteractive()
	default:
		ensureTunnel()
		attachOrCreate(fullName(cmd))
	}
}
