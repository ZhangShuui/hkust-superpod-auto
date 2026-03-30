# HKUST SuperPod Auto

一站式工具链，从本地 WSL2 连接 HKUST SuperPod HPC 集群并在上面使用 Claude Code / Codex。

## `spod` CLI

Go 编写的统一入口，管理 VPN、SSH 隧道、tmux 多会话。

```bash
# ── VPN ──
spod vpn              # 启动 VPN（后台 headless，自动重连+退避）
spod vpn stop         # 停止 VPN
spod vpn status       # 查看 VPN + SuperPod 可达性
spod vpn log          # 实时查看 VPN 日志

# ── 会话 ──
spod                  # 交互选择 / 新建会话
spod <name>           # 连到指定会话（不存在则创建）
spod new [name]       # 创建新会话（自动编号 spod-1, spod-2...）
spod ls               # 列出所有会话
spod kill <name>      # 关掉指定会话
spod killall          # 关掉所有会话

# ── 工具 ──
spod sync <r> <l>     # 从 SuperPod 并行 rsync 到本地
spod sync stop        # 停止所有 rsync
spod speed [秒]       # VPN 隧道测速（默认 60s）
spod tunnel           # 启动 / 检查 SSH 反向隧道
spod tunnel stop      # 关闭隧道
spod ssh              # 裸 SSH（不进 tmux）

# ── Windows 接入 ──
spod vscode           # 一键配置 Windows VS Code Remote-SSH
spod socks            # 启动 SOCKS5 代理（Windows 可通过 127.0.0.1:1080 接入）
spod socks status     # 查看 SOCKS5 代理状态
spod socks stop       # 关闭 SOCKS5 代理
```

## Quick Start

### 首次设置

```bash
# 1. 克隆项目
git clone https://github.com/ZhangShuui/hkust-superpod-auto.git
cd hkust-superpod-auto

# 2. 安装依赖
sudo apt install openconnect autossh golang-go
pip install pyotp playwright vpn-slice
playwright install chromium

# 3. 编译安装 spod
cd cmd/spod && go build -o spod . && cp spod ~/.local/bin/ && cd ../..

# 4. 配置
cp .env.example .env        # 编辑 .env，填入 ITSC ID
ln -sf "$(pwd)/.env" ~/.config/spod/.env  # 全局可用（可选）

# 5. 配置 SSH
cat >> ~/.ssh/config << 'EOF'
Host superpod
    HostName superpod.ust.hk
    User <your-itsc-id>
    ServerAliveInterval 15
    ServerAliveCountMax 4
    TCPKeepAlive yes
EOF

# 6. 首次 VPN 登录（交互式输入密码 + TOTP 密钥）
python3 hkust-vpn.py --setup

# 7. SuperPod 上配置 Claude Code（见下方）
```

### 日常使用

```bash
spod vpn              # 启动 VPN（后台，自动重连）
spod                  # 连接 SuperPod → tmux 会话
claude                # 在 SuperPod 上使用 Claude Code
codex                 # 在 SuperPod 上使用 Codex

# 断线了？
spod                  # 重连，tmux 保住了进程
```

## 架构

```
本地 (WSL2)
    │
    ├─ spod vpn ─── hkust-vpn.py ─── openconnect + vpn-slice ─── HKUST 内网
    │                                                                  │
    ├─ Clash (:7890) ◄──── autossh 反向隧道 ◄──────────── SuperPod (:17897)
    │                                                          │
    ├─ spod socks ─── autossh -D 0.0.0.0:1080 ─── SOCKS5 代理
    │                       ▲                           │
    │                       │                     Windows 可用
    │                  Windows SSH / 浏览器 / VS Code
    │
    └─ spod ─── SSH + tmux ──────────────────────────── SuperPod
                                                               │
                                                         Claude Code / Codex
                                                    (API → :17897 → 隧道 → Clash → Anthropic / OpenAI)
```

## 核心能力

| 功能 | 实现 |
|------|------|
| VPN 自动登录 | Playwright 完成 Microsoft SSO + TOTP MFA |
| VPN 自动重连 | 指数退避重试（10s→20s→40s→80s），5 次失败后退出 |
| Split tunneling | openconnect + vpn-slice，只有学校流量走 VPN |
| SSH 反向隧道 | autossh 维护，断线自动重建 |
| tmux 多会话 | SSH 断了进程不丢，`spod` 直接恢复 |
| 防 Broken pipe | 15s 心跳探测，60s 容忍网络抖动 |
| VPN 健康检查 | 后台每 60s TCP 探测 SuperPod:22 |
| 日志持久化 | vpn.log 轮转（5MB × 3 份） |
| Windows 接入 | SOCKS5 代理让 Windows SSH/浏览器/VS Code 共享 WSL VPN 隧道 |
| VS Code 一键配置 | `spod vscode` 自动配 SSH config、公钥、SOCKS 代理 |
| SSH 自动重试 | 网络抖动时自动重试 3 次（2s→4s→8s 退避） |
| 精准代理 | 只有 claude/codex 走隧道代理，git/pip 等直连 |

## Windows 接入 SuperPod

WSL 建立 VPN 后，Windows 可通过 SOCKS5 代理访问 SuperPod 内网。

### VS Code Remote-SSH（推荐）

一条命令自动完成所有配置：

```bash
spod vscode
```

自动完成：
- 启动 SOCKS5 代理
- 查询 SuperPod 内网 IP（绕过 hairpin NAT）
- 检测并添加 Windows SSH 公钥到 SuperPod
- 写入 Windows SSH 配置（`C:\Users\<你>\.ssh\config`）

之后在 VS Code 中：**Ctrl+Shift+P → Remote-SSH: Connect to Host → superpod**

> 前置条件：[Git for Windows](https://git-scm.com/download/win)（提供 `connect.exe`）、VS Code [Remote-SSH 扩展](https://marketplace.visualstudio.com/items?itemName=ms-vscode-remote.remote-ssh)

### 浏览器访问内网

```bash
spod socks   # 启动 SOCKS5 代理（如果 spod vscode 已启动则无需重复）
```

浏览器设置 SOCKS5 代理 `127.0.0.1:1080` 即可访问 SuperPod 内网 Web 服务（Jupyter、Grafana 等）。推荐使用 SwitchyOmega 等浏览器扩展按域名切换代理。

## SuperPod 环境配置

首次使用前需在 SuperPod 上配置 Claude Code 和 Codex：

```bash
ssh superpod

# 创建 Claude 环境
module load Anaconda3/2023.09-0
conda create -n claude nodejs=20 -y
conda activate claude
npm install -g @anthropic-ai/claude-code
npm install -g @openai/codex

# 写入 .bashrc
echo 'conda activate claude' >> ~/.bashrc
```

> **注意**：代理配置由 `spod` 自动管理 — 只有 `claude` 和 `codex` 命令会走反向隧道代理，其他命令（git、pip、npm 等）直连网络，无需手动配置。

### 同步本地凭证到 SuperPod

Codex 使用 ChatGPT 账号认证（非 API key），需要把本地登录凭证传到 SuperPod：

```bash
# 本地先登录 Codex（如果还没登录过）
codex login

# 同步凭证到 SuperPod
ssh superpod 'mkdir -p ~/.codex'
scp ~/.codex/auth.json ~/.codex/config.toml superpod:~/.codex/
```

凭证有过期时间，如果 SuperPod 上 Codex 报认证错误，重新在本地 `codex login` 后再 scp 一次。

## 前置条件

- WSL2 (Ubuntu)
- Python 3.10+ / Go 1.22+
- openconnect / autossh / tmux
- Playwright + Chromium
- Clash 代理（本地 7897 端口）

## 配置

所有工具从 `.env` 读取配置（`cp .env.example .env`）：

| 变量 | 用途 | 使用者 |
|------|------|--------|
| `HKUST_USER` | ITSC 账号 | VPN 脚本 |
| `SUPERPOD_USER` | SSH 用户名 | sync.sh |
| `SUPERPOD_HOST` | 主机名 | VPN 脚本 |
| `CLASH_PORT` | 本地 Clash 端口 | spod + VPN |
| `TUNNEL_PORT` | 反向隧道端口 | spod |
| `SOCKS_PORT` | SOCKS5 代理端口（默认 1080） | spod socks |
| `SPOD_SSH_HOST` | SSH Host 别名 | spod |
| `VPN_SCRIPT` | VPN 脚本路径 | spod |

## 项目结构

```
├── cmd/spod/           # spod CLI (Go) — 统一入口
│   ├── main.go         #   VPN / 隧道 / 会话 / sync / speedtest
│   └── go.mod
├── hkust-vpn.py        # VPN 自动连接（Playwright + openconnect）
├── .env.example        # 配置模板
├── pyproject.toml      # Python 依赖声明
└── docs/
    ├── vpn.md          # VPN 详细文档
    ├── slurm.md        # SLURM 会话管理指南
    └── sync.md         # 文件同步文档
```

## 其他文档

- [VPN 详细文档](./docs/vpn.md) — 登录流程、参数、故障排查
- [SuperPod 会话管理](./docs/slurm.md) — SLURM 容器化 GPU 会话
- [文件同步](./docs/sync.md) — rsync 并行下载训练数据
