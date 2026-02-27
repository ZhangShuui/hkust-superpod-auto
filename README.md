# HKUST SuperPod Auto

Automation tools for connecting to and working on the HKUST SuperPod HPC cluster from a local machine (WSL2).

## What's Included

### [VPN Auto-Connect](./HKUST-VPN-README.md)
Fully automated HKUST VPN connection with split tunneling. Handles Microsoft SSO + TOTP MFA via Playwright browser automation, then establishes an openconnect tunnel that only routes school traffic through VPN — Claude Code, browsers, and other tools remain unaffected.

### [SuperPod Session Management](./SUPERPOD-SESSION-README.md)
Guide for managing persistent interactive SLURM sessions on the SuperPod using Claude Code's MCP interactive terminal. Covers the full workflow: SSH to login node, check cluster status, launch containerized compute nodes (Pyxis/Enroot), and work with GPUs.

## Quick Start

```bash
# 1. Start VPN (auto-reconnect, zero interaction)
python3 hkust-vpn.py

# 2. In Claude Code, use the hkust-superpod-session skill to:
#    - SSH to login node
#    - Check cluster availability (sinfo, savail)
#    - Launch a containerized GPU session (srun)
#    - Work on the compute node
```

## Requirements

- WSL2 (Ubuntu 24.04)
- Python 3.10+
- openconnect, vpn-slice
- Playwright + Chromium
- Claude Code with [mcp-interactive-terminal](https://github.com/amol21p/mcp-interactive-terminal)
