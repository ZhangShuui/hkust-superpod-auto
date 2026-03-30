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
ls ~/.cache/ms-playwright/chromium-*/chrome-linux64/chrome >/dev/null 2>&1 || python3 -m playwright install chromium

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

One-command toolkit for connecting to HKUST SuperPod HPC from WSL2 and running Claude Code on it.

## Key Components

| File | What it does |
|------|-------------|
| `cmd/spod/main.go` | `spod` CLI (Go) — unified entry point for VPN, tunnel, sessions, sync |
| `hkust-vpn.py` | VPN auto-login: Playwright (Microsoft SSO + TOTP MFA) + openconnect + vpn-slice |
| `.env` | All credentials and config (gitignored, real secrets — never commit) |
| `docs/` | Detailed docs on VPN, SLURM, sync |

## Architecture

```
Local WSL2
  ├─ spod vpn → hkust-vpn.py → openconnect + vpn-slice → HKUST network
  ├─ Clash (:7890) ◄── autossh reverse tunnel ◄── SuperPod (:17897)
  ├─ spod socks → autossh -D 0.0.0.0:1080 → SOCKS5 代理 → Windows 可用
  └─ spod → SSH + tmux → SuperPod → Claude Code (→ :17897 → Clash → Anthropic API)
                                    → Codex     (→ :17897 → Clash → OpenAI API)

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
```

## SuperPod Remote Setup

After VPN + SSH are working, sync local credentials to SuperPod so Claude Code and Codex can call their APIs:

```bash
# Codex credentials (ChatGPT auth, stored in ~/.codex/)
ssh superpod 'mkdir -p ~/.codex'
scp ~/.codex/auth.json ~/.codex/config.toml superpod:~/.codex/

# Proxy is auto-configured by spod — only claude/codex commands get proxy env vars
# (via shell wrapper functions in remote ~/.bashrc), git/pip/npm etc. go direct
```

Codex is installed globally in the `claude` conda env on SuperPod (`npm install -g @openai/codex`).

## Critical Gotchas

- **vpn-slice must exist at `.venv/bin/vpn-slice`** — openconnect calls it as a script. If missing, VPN connects but routing breaks and SSH port 22 is unreachable despite ping working.
- **VPN script uses system python** (`python3 hkust-vpn.py`) but references `.venv/bin/vpn-slice` as path. The system python needs `pyotp` and `playwright` installed (or use .venv python).
- **Clash proxy on port 7890** must be running locally before VPN connects (openconnect uses it as `--proxy`).
- `.env` has real passwords and TOTP secret — never commit, never log.
- SSH config entry `Host superpod` is auto-synced by `spod` on every run (via `ensureSSHConfig()`).
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
