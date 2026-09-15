# CLAUDE.md

## Environment Setup (MUST DO FIRST)

Any agent working on this repo MUST verify the environment before running VPN/SSH/spod commands.
Run this checklist and fix anything missing:

```bash
# 1. System packages
which openconnect autossh tmux rsync || sudo apt install -y openconnect autossh tmux rsync

# 2. Go (needed to build spod)
which go || (curl -sL https://go.dev/dl/go1.23.8.linux-amd64.tar.gz -o /tmp/go.tar.gz && sudo tar -C /usr/local -xzf /tmp/go.tar.gz && rm /tmp/go.tar.gz)
export PATH=$PATH:/usr/local/go/bin

# 3. Python venv with vpn-slice (openconnect needs this binary)
test -x .venv/bin/vpn-slice || (python3 -m venv .venv && .venv/bin/pip install vpn-slice pyotp playwright)

# 4. Playwright browsers
ls ~/.cache/ms-playwright/firefox-*/firefox/firefox >/dev/null 2>&1 || python3 -m playwright install firefox

# 5. Build and install spod CLI
which spod || (cd cmd/spod && go build -o ~/.local/bin/spod . && cd ../..)

# 6. .env must exist (copy from .env.example and fill in credentials)
test -f .env || echo "ERROR: .env missing — cp .env.example .env and fill in credentials"

# 7. Global config symlink (so spod works from any directory)
test -L ~/.config/spod/.env || (mkdir -p ~/.config/spod && ln -sf "$(pwd)/.env" ~/.config/spod/.env)
```

If sudo requires a password non-interactively: `echo "$SUDO_PASSWORD" | sudo -S <cmd>`
The sudo password is in `.env` as `SUDO_PASSWORD`.

## Project Overview

One-command toolkit for connecting to HKUST SuperPod (and HPC4) from WSL2 and running Claude Code on it.

## Key Components

| File | What it does |
|------|-------------|
| `cmd/spod/main.go` | `spod` CLI (Go) — unified entry point for VPN, tunnel, sessions, sync |
| `hkust-vpn.py` | VPN auto-login: Playwright (Microsoft SSO + TOTP MFA) + openconnect + vpn-slice |
| `.env` | All credentials and config (gitignored, real secrets — never commit) |
| `docs/` | Detailed docs on VPN, SLURM, sync |
| `skills/` | Claude Code skills for both clusters — symlinked into `~/.claude/skills/` |

## Architecture

```
Local WSL2
  ├─ spod vpn → hkust-vpn.py → openconnect + vpn-slice → HKUST network
  ├─ Clash (:7890) ◄── autossh reverse tunnel ◄── SuperPod (:per-user tunnel)
  ├─ spod socks → autossh -D 0.0.0.0:1080 → SOCKS5 代理 → Windows 可用
  ├─ spod recv ◄── ~/.spod/outbox queue ◄── `spush` on the cluster (pull, not push)
  └─ spod → SSH + tmux → SuperPod → Claude Code (→ :relay → :tunnel → Clash → Anthropic API)
                                    → Codex     (→ :relay → :tunnel → Clash → OpenAI API)

Windows ──► SOCKS5 (127.0.0.1:1080) ──► WSL VPN ──► SuperPod 内网
  ├─ SSH (connect.exe -S)
  ├─ VS Code Remote-SSH
  └─ 浏览器 (Jupyter/Grafana)
```

## Common Workflows

```bash
spod vpn            # Start VPN (background, auto-reconnect)
spod vpn status     # Check VPN + SuperPod reachability
spod                # Connect to SuperPod tmux session
spod ssh            # Raw SSH without tmux
spod tunnel         # Start/check reverse tunnel for Claude Code API proxy
spod vscode         # One-command setup for Windows VS Code Remote-SSH
spod socks          # Start SOCKS5 proxy for Windows access
spod socks status   # Check SOCKS5 proxy status
spod socks stop     # Stop SOCKS5 proxy
spod get <path>...  # Pull files to Windows Downloads (glob OK, MD5-verified, resumable)
spod recv           # Receive files pushed from the cluster with `spush` (watch queue)
spod recv once      # Drain the queue once and exit
spod recv status    # Queue depth + whether a receiver is online

spod hpc4           # Same commands against HPC4 — see below
```

### Two clusters, side by side

`spod hpc4 <subcommand>` runs any of the above against HPC4 (`hpc4.ust.hk`)
instead of SuperPod. Everything per-cluster is separate — ssh alias, reverse
tunnel, relay, SOCKS port, `/tmp` lock files, cached remote UID, tmux sessions —
so `spod` and `spod hpc4` can be used at the same time without one disturbing
the other:

```bash
spod                spod hpc4                # two independent tmux sessions
spod get /a/b.mp4   spod hpc4 get /a/b.mp4   # two independent tunnels/relays
spod socks          spod hpc4 socks          # local :1080 vs :1081
spod vpn status                              # one VPN, reports both clusters
```

Config lives in `.env` (`HPC4_USER`, optional `HPC4_HOST`/`HPC4_SSH_HOST`/
`HPC4_SOCKS_PORT`/`HPC4_TUNNEL_PORT`/`HPC4_RELAY_PORT`). The VPN is shared;
`spod vpn` and `spod rider` stay SuperPod-scoped (rider borrows a provider's
SuperPod tunnel and refuses to run under `hpc4`).

Adding a third cluster is a `targets` map entry in `main.go` plus its `.env`
keys — nothing else in the command layer is cluster-aware.

### Pulling files off SuperPod

```bash
spod get /project/foo/bar.mp4                    # → C:\Users\<you>\Downloads
spod get '/project/foo/*.mp4'                    # globs expand on the remote
spod get /project/foo/a.mp4 -o 'C:\Users\me\Desktop'   # Windows paths accepted
spod get /project/foo/a.mp4 -o ./data            # or any local dir
SPOD_GET_STREAMS=6 spod get /project/foo/a.mp4   # more parallel streams (1-16, default 4)
```

`spod get` records each file's MD5 **before** transferring, then verifies after.
This matters because a job can unlink a file mid-transfer: NFS silly-renames it
to `.nfsXXXX`, rsync still exits 0, and the original path is gone — leaving
nothing to check against unless the digest was taken up front. Re-running the
same command resumes from where it stopped (`--append-verify`).

Transfers fan out over **4 independent SSH connections** by default
(`SPOD_GET_STREAMS=1..16`), each pulling byte ranges via `dd` and pwriting them
into the preallocated destination file. Resume state lives in a
`.<name>.spodget` sidecar — the file is full-length with holes while in flight,
so its size proves nothing about what completed.

Why parallel: the VPN has no ESP/UDP channel (see below), so inner RTT is
~300 ms and any *single* TCP flow is pinned near 255 KB/s no matter how much
bandwidth is free. Measured on this link: 1 flow 254 KB/s, 3 flows 427 KB/s,
6 flows 695 KB/s; end-to-end `spod get` went 254 → 946 KB/s at 4 streams.
8 streams was slower than 4 (863 KB/s) — extra logins and tail imbalance.

**Each stream needs its own `ControlPath`.** `Host superpod` sets
`ControlMaster auto` on a shared socket, so plain parallel transfers all become
channels on *one* TCP connection sharing *one* congestion window — measured at
100 KB/s versus 254 KB/s for a dedicated connection. An earlier note here
claimed parallel streams *lowered* throughput; that measurement was run through
the shared mux and was measuring exactly this collapse, not a link ceiling.
`getSSHOpts()` gives each worker a private socket, reused across all of its
chunks so a big file costs 4 logins, not one per chunk.

### Pushing files back: `spush` on the cluster → `spod recv` here

The cluster cannot open a connection to this machine. The VPN is split-tunnel
and WSL2 sits behind NAT, so the only inbound path that exists at all is the
reverse tunnel — and that one is systemd-managed and carries the API traffic
claude/codex ride on, which a bulk upload would starve. Reverse-`ssh`-ing into
the local box was the other option and is worse: it needs a second `-R` in a
unit file outside this repo, a local sshd, and a passwordless key *into your
laptop* sitting on a cluster account other people share.

So a push is a queue, not a connection:

```bash
# on the cluster (inside tmux, or any login shell)
spush out.mp4                  # queue it
spush -d run7 results/         # a whole directory, tree preserved under Downloads/run7/
spush -w ckpt.pt               # block until the local side confirms, prints where it landed
spush -l                       # what is still queued, and how long it has waited
spush -f -d run7 results/      # -f: re-send even what was already delivered

# locally
spod recv                      # watch: claims the queue as things appear (Ctrl-C to stop)
spod recv once                 # drain and exit          (spod hpc4 recv … for HPC4)
spod recv status               # queue depth + receiver heartbeat
spod recv -o 'C:\Users\me\Desktop'
```

`spush` appends `ts \t subdir \t size \t abspath` to `~/.spod/outbox`; `spod recv`
claims it and pulls with the *same* fetcher as `spod get` (4 streams, MD5
snapshot taken before the transfer, resumable), so the reverse direction gets
the parallel-stream throughput for free instead of a fresh single-flow protocol
capped at 255 KB/s. Files with a different `-d` land in different batches
(parallelFetch flattens one batch into one directory).

An agent running **on the cluster** hands files back the same way, but must call
`~/.local/bin/spush` by absolute path: its Bash tool runs a non-login shell whose
snapshot predates the `spush` function. `ensureAgentNote()` keeps a marked block
in the cluster's `~/.claude/CLAUDE.md` saying exactly that, so a Claude Code
session there discovers the mechanism without being told. `spush -w <secs>` exits
1 when the file did not arrive, which is the signal an agent should act on —
usually "ask the user to run `spod recv`", not "retry".

Things that are load-bearing here:

- **The queue is claimed by rename, not truncate.** `ssh()` retries a reset
  connection; a truncate that succeeded remotely but whose output never came
  back would silently drop every queued path. `claim.<pid>.<ts>` survives that,
  and leftovers are picked up by the next claim.
- **A claim file that is not deleted becomes an infinite re-download.** It was,
  once: the delete used `shellQuote("$HOME/.spod/"+name)`, and shellQuote
  single-quotes, so `$HOME` never expanded and `rm -f` removed nothing while
  exiting 0. `dropClaims` now passes bare names after `cd ~/.spod`, verifies
  nothing is left, and the watch loop backs off 30 s instead of looping when it
  cannot clean up.
- **Queue lines are untrusted input** — the home may be shared with other people
  on the same account. `sanitizeSub` drops `..`, absolute paths and anything
  deeper than 8 levels so a queued line cannot steer a write out of the
  download directory.
- **`~/.local/bin` is on the login PATH but not the non-interactive one.** Hence
  both a `spush` symlink (works in tmux sessions opened before the update) and a
  `spush()` wrapper in the managed bashrc block. `ssh cluster 'spush x'` finds
  neither — use `bash -lc` or the absolute path.
- **A queued path can rot into a poison entry.** The queue outlives the job that
  wrote it; a deleted file fails every fetch, gets re-queued, and the watch loop
  claims it straight back — a spin. `statQueued` re-stats everything before
  fetching and drops what is gone (with a receipt), and a drain that re-queued
  anything reports itself unsettled so the loop backs off 30 s.
- Local claims that have not finished are kept in `/tmp/spod[-hpc4]-recv-inflight`,
  so Ctrl-C mid-transfer re-tries those files instead of losing them; failures
  are re-queued on the cluster.
- **`spush` will not queue what you already have.** Pushing a *directory* is the
  normal way an agent hands back results, and it re-walks the whole tree every
  time: add one file to `results/`, re-run `spush -d run7 results/`, and without
  a check every file already delivered is queued again and pulled again. So the
  queue grew into a pile of things already sitting in Downloads. `spush` now
  filters candidates against two facts it already had and never read — the
  current outbox (same `sub`+path already queued) and `~/.spod/receipts` (an
  `ok` for that path with the *same size and mtime*) — and reports what it
  skipped, with where those copies landed. A file whose content changed has a
  new mtime, so it is still sent. The stamp is `stat -Lc '%s %.Y'`: whole
  seconds would call a file unchanged when a job rewrote it to the same length
  within the same second, and the fractional part is real on every filesystem
  here (ns on `/tmp`, ms on NFS and `/project`). Both sides compare it as
  *text* — 19 significant digits do not survive a trip through a double.
  `-f` skips the delivered check but not the already-queued one: the drain
  fetches whatever the file holds at that point, so a second identical queue
  line buys nothing but a longer queue.
- **`spush` must never report success without queueing.** The filter is one awk
  pass, and gawk dies without running `END` if it cannot write its output (a
  full `/tmp`), leaving an empty tally that the shell arithmetic reads as
  "nothing new" — exit 0, nothing queued, caller none the wiser. So every
  candidate must be accounted for as queued, already-queued, or delivered, and
  the queue file must have grown by exactly the number claimed; otherwise
  `spush` dies and leaves the outbox untouched.
- **The receipts file is a ledger, not just a log.** It is what makes the check
  above possible, so it carries `ts status path dest size mtime` and is trimmed
  to 2000 lines, not 200 — a single 168-file drop used to push its own earlier
  entries out, which would offer every one of them for re-sending. Its
  timestamp comes from the *cluster's* `date +%s`: both readers (`spush -w`, the
  skip check) compare it against cluster-side times, so a laptop whose clock
  drifts must not be the one stamping it.
- **A duplicate queue line is not a second file.** `spod recv status` and
  `spush -l` count distinct `(sub, path)` — `dedupePush` collapses them before
  fetching, so counting raw lines reported a queue deeper than the drain. Both
  also show how long the oldest entry has waited: nothing expires a queue, and a
  four-day-old entry otherwise rides along with the next push unnoticed. That
  age prints through `humanAge`, not `humanDuration` — the latter is a
  stopwatch that returns `--:--` past 99 hours, which is exactly the range
  these two readouts live in (a queue nobody drained, a receiver last seen days
  ago showed up as `已离线 --:--`, indistinguishable from "unknown").
- **`statQueued` must see its own end marker.** It decides which queued paths
  still exist, and a reply cut short mid-stream makes everything past the cut
  look deleted — which the caller acts on by dropping those files with a "fail"
  receipt. It now prints `STATDONE` last and treats a reply without it as
  "unknown" rather than trusting a partial listing.

## Remote Setup (both clusters)

Both clusters use the **same layout**: a standalone Node 24 at `~/.local/node24`
(its own npm prefix — no conda), with `claude` and `codex` installed globally into
it. Replace `superpod` with `hpc4` below to set up the other cluster.

```bash
# 1. Install the CLIs (npm registry is reachable DIRECTLY from both clusters —
#    do not proxy this; the tunnel is only for the API calls at runtime)
ssh superpod 'bash -lc "npm install -g @anthropic-ai/claude-code @openai/codex"'

# 2. Claude auth — give each cluster its OWN grant. Do NOT scp ~/.claude/.credentials.json
#    (refresh tokens are single-use; see the gotcha below). Recommended: a long-lived
#    token, which never refreshes and so also survives tunnel outages:
claude setup-token                                   # locally, browser flow; prints sk-ant-oat01-…
ssh superpod 'umask 077; mkdir -p ~/.claude; cat > ~/.claude/oauth_token'   # paste it, Ctrl-D
#    spod's claude() wrapper exports it as CLAUDE_CODE_OAUTH_TOKEN for that call only.
#    Alternative: run `claude` on the cluster and `/login` (URL flow). Refreshes then
#    go through the relay, so the tunnel must be up whenever the token expires.

# 3. Codex credentials
ssh superpod 'mkdir -p ~/.codex'
scp ~/.codex/auth.json ~/.codex/config.toml superpod:~/.codex/

# 4. Settings — copy the CLUSTER version, not your local ~/.claude/settings.json
#    (the local one has hooks pointing at local-only paths). The cluster version
#    pins autoUpdates:false + DISABLE_AUTOUPDATER=1.
ssh superpod 'cat ~/.claude/settings.json' | ssh hpc4 'cat > ~/.claude/settings.json'

# 5. Mark onboarding complete — otherwise EVERY launch runs the 10 s first-run
#    preflight (gotcha below) and dies on this link
ssh hpc4 'test -f ~/.claude.json || echo "{}" > ~/.claude.json; jq ".hasCompletedOnboarding=true | .theme=\"dark\"" ~/.claude.json > ~/.claude.json.new && mv ~/.claude.json.new ~/.claude.json'

# Proxy is auto-configured by spod — only claude/codex commands get proxy env vars
# (via shell wrapper functions in remote ~/.bashrc), git/pip/npm etc. go direct
```

`npm install` warns `allow-scripts ... postinstall: node install.cjs` for
claude-code. Ignore it — the package ships the `bin/claude.exe` native binary and
the symlink is created regardless. Do **not** enable allow-scripts to silence it.

**Keep `autoUpdates` off.** The homes are NFS, which cannot unlink the running
300 MB `claude.exe`; it silly-renames to `.nfsXXXX`, the update half-installs, and
`claude` disappears. See `~/.claude/.last-update-result.json` for `install_failed`.

### Gotcha: `timeout claude ...` silently bypasses the proxy

`claude` and `codex` are **shell functions** in the remote `~/.bashrc` — that is
what injects `http_proxy`. Any external command in front of them (`timeout`,
`env`, `nohup`, `xargs`) execs the PATH binary directly and skips the function, so
the request goes out direct and dies as:

```
Failed to authenticate. API Error: 403 Request not allowed
```

That 403 is indistinguishable from an expired token or a rate limit. To wrap the
call, export the proxy yourself and use `command claude`:

```bash
export https_proxy=http://127.0.0.1:$RELAY_PORT http_proxy=$https_proxy
timeout 180 command claude -p "..."
```

### Gotcha: "Unable to connect to Anthropic services … timed out after 10 seconds"

This is the **first-run preflight**, and it only runs while `hasCompletedOnboarding`
is unset in `~/.claude.json`. It fetches `${API}/api/hello` and
`${TOKEN_URL}/v1/oauth/hello` with a hard-coded 10 s budget (`var ce=1e4`),
demands HTTP 200 from both, and `process.exit(1)`s otherwise;
`CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1` does not skip it. Over this VPN a TLS
handshake through the relay swings between 1 s and 25 s, so a cluster stuck in
onboarding (nobody ever picked a theme) fails ~30 % of launches. Step 5 above ends
it. `claude -p` never runs the preflight, which is why print mode "worked" while
the TUI didn't. Diagnose deterministically with a black-hole proxy — if the message
still appears, onboarding is not marked complete:

```bash
python3 -c 'import socket;s=socket.socket();s.bind(("127.0.0.1",18999));s.listen(5);c=[]
while True: c.append(s.accept())' &
https_proxy=http://127.0.0.1:18999 command claude
```

### Gotcha: one credentials file on many machines → "Login expired"

OAuth refresh tokens are single-use. If two machines hold the same
`~/.claude/.credentials.json`, the first one to refresh (an active session refreshes
~5 min before `expiresAt`) invalidates the other's refresh token; when that one
expires it is rejected and Claude Code **wipes the file** (`expiresAt: 0`,
`Login expired · Please run /login`). Independent grants coexist fine — SuperPod's
own grant refreshed at 11:58 while the local one refreshed at 17:17 and both kept
working — so the fix is one grant per machine (step 2), never a shared copy. Copying
a fresh file only buys time until the next expiry.

## Skills

`skills/` holds the Claude Code skills for this toolchain. They are the source
of truth and are symlinked into `~/.claude/skills/` — edit them here, not there:

```bash
for s in hkust-superpod-session slurm-info slurm-monitor slurm-submit; do
  ln -sfn "$(pwd)/skills/$s" ~/.claude/skills/$s
done
```

| Skill | Scope |
|-------|-------|
| `skills/CLUSTERS.md` | Shared SuperPod-vs-HPC4 fact sheet every other skill reads |
| `hkust-superpod-session` | Interactive session: VPN → `spod` tmux → `srun` allocation |
| `slurm-info` | Cache partitions / GRES / QOS caps / accounts, per cluster |
| `slurm-monitor` | Read-only queue, logs, node status |
| `slurm-submit` | Generate + submit batch jobs |

All four take an optional leading `superpod` / `hpc4` word, mirroring the CLI.
The clusters are **not** interchangeable: HPC4 has no Pyxis/enroot, and needs
`--gres=gpu:<type>:N` (a bare `--gpus` there yields a GPU-less allocation that
still starts). See `skills/CLUSTERS.md` before generating any SLURM command.

## Critical Gotchas

- **The VPN runs with ESP/UDP disabled, and that is the bandwidth ceiling.** Because openconnect is launched with `--proxy http://127.0.0.1:7890`, its datagram channel cannot traverse the HTTP CONNECT proxy — the log says `Set up UDP failed; using SSL instead` / `ESP disabled`, and every packet becomes TCP-over-TCP through Clash. Result: inner RTT 233–494 ms with `cwnd` stuck at 2–5 segments, so one TCP flow tops out near 255 KB/s. Diagnose with `grep -E "ESP|UDP failed" vpn.log` and `ss -tino | grep -A1 143.89`. Clash itself is *not* the cap (measured 1.77 MB/s to Cloudflare). Note `remote.ust.hk` is reachable without the proxy (~127 ms), but this machine's raw ISP path to the internet is much slower than Clash (126 KB/s vs 1.77 MB/s), so dropping `--proxy` to regain ESP is a real experiment, not an obvious win — measure before changing it.
- **The VPN is split-tunnel: a cluster missing from `VPN_HOSTS` resolves but never connects.** vpn-slice installs a `/32` via tun0 only for the hosts listed in `VPN_HOSTS`, so `hpc4.ust.hk` (143.89.184.3) answers DNS from the public resolver, looks configured, and then its packets leave via eth0 and hang until timeout. `.env` now sets `VPN_HOSTS="superpod.ust.hk hpc4.ust.hk"`; on an already-running VPN, `ensureRoute()` adds the missing route in place (sudo, `SUDO_PASSWORD` from `.env`) so no restart is needed. Diagnose with `ip route get 143.89.184.3` — it must say `dev tun0`.
- **Never let autossh loose on an account you can't key into.** It has no tty for a password and retries forever, so a wrong username turns into an endless stream of failed logins — and enough of those lock the ITSC account, which takes the VPN down with it. `ensureTunnel`/`ensureSocks` gate on `sshAuthOK()` (a single `BatchMode=yes` probe) and skip the tunnel with a `ssh-copy-id` hint instead. For the same reason `ssh()` never retries a `Permission denied` or `Host key verification failed` (`isFatalSSHErr`), and runs with `BatchMode=yes` so an unattended call can't stall on a password prompt.
- **vpn-slice must exist at `.venv/bin/vpn-slice`** — openconnect calls it as a script. If missing, VPN connects but routing breaks and SSH port 22 is unreachable despite ping working.
- **VPN script uses system python** (`python3 hkust-vpn.py`) but references `.venv/bin/vpn-slice` as path. The system python needs `pyotp` and `playwright` installed (or use .venv python).
- **Clash proxy on port 7890** must be running locally before VPN connects (openconnect uses it as `--proxy`).
- `.env` has real passwords and TOTP secret — never commit, never log.
- SSH config entries `Host superpod` / `Host hpc4` are auto-synced by `spod` on every run (via `ensureSSHConfig()`), one block per cluster configured in `.env`.
- SuperPod login nodes: NO computation. Always use `srun` for GPU work.
- **SOCKS proxy hairpin NAT** — `superpod.ust.hk` resolves to public IP which SuperPod can't reach from inside. `spod vscode` handles this automatically by using internal IP.
- **Windows SSH needs its own key** — WSL and Windows have different SSH keys. `spod vscode` auto-adds Windows public key to SuperPod's `~/.ssh/authorized_keys`.

## Build

```bash
cd cmd/spod && go build -o ~/.local/bin/spod .
```

Go 1.22+ required. No external Go dependencies.

## Testing VPN Connectivity

```bash
# VPN up?
ip link show tun0

# Port 22 reachable? (ping can work even without VPN routing)
timeout 3 bash -c 'echo > /dev/tcp/superpod.ust.hk/22' && echo OK || echo FAIL

# Don't trust ping alone — it may bypass VPN via regular DNS resolution
```
