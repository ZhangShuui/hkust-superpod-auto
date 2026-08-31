package main

import (
	"bufio"
	"bytes"
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
	info("解析远程路径...")
	var quoted []string
	for _, r := range remotes {
		quoted = append(quoted, "'"+strings.ReplaceAll(r, "'", `'\''`)+"'")
	}
	listing, err := ssh("for p in " + strings.Join(quoted, " ") + `; do for f in $p; do [ -f "$f" ] && [ -r "$f" ] && printf '%s\t%s\n' "$(stat -c %s "$f")" "$f"; done; done`)
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

	// Snapshot checksums before pulling — see the comment on cmdGet.
	info(fmt.Sprintf("计算远端 MD5（%d 个文件）...", len(files)))
	var shellList []string
	for _, f := range files {
		shellList = append(shellList, shellQuote(f.path))
	}
	sums, err := ssh("md5sum " + strings.Join(shellList, " "))
	want := map[string]string{} // basename → md5
	if err != nil {
		warn(fmt.Sprintf("远端 MD5 失败，将跳过校验: %v", err))
	} else {
		for _, line := range strings.Split(sums, "\n") {
			f := strings.Fields(strings.TrimSpace(line))
			if len(f) >= 2 {
				want[filepath.Base(strings.Join(f[1:], " "))] = f[0]
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
		fail(fmt.Sprintf("下载失败: %v", fetchErr))
		info("已传输的部分会保留，重跑同一条命令可断点续传")
		os.Exit(1)
	}

	// Verify.
	bad := 0
	for _, f := range files {
		base := f.base
		local := filepath.Join(dest, base)
		fi, err := os.Stat(local)
		if err != nil {
			fail(base + " — 本地文件缺失")
			bad++
			continue
		}
		exp, haveSum := want[base]
		if !haveSum {
			warn(fmt.Sprintf("%s (%s) — 无远端 MD5，未校验", base, humanBytes(fi.Size())))
			continue
		}
		out, err := exec.Command("md5sum", local).Output()
		got := ""
		if err == nil {
			got = strings.Fields(string(out))[0]
		}
		if got == exp {
			ok(fmt.Sprintf("%s (%s) — MD5 校验通过", base, humanBytes(fi.Size())))
		} else {
			fail(fmt.Sprintf("%s — MD5 不匹配（远端 %s / 本地 %s）", base, exp, got))
			bad++
		}
	}

	if bad > 0 {
		fail(fmt.Sprintf("%d 个文件校验未通过 — 重跑同一条命令可续传/重取", bad))
		os.Exit(1)
	}
	ok(fmt.Sprintf("全部完成 → %s", dest))
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
