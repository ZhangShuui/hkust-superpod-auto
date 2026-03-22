# HKUST SuperPod Auto

Automation tools for connecting to and working on the HKUST SuperPod HPC cluster from a local machine (WSL2).

## What's Included

### `spod` — SuperPod 会话管理 CLI

Go 编写的一键连接工具，管理 SSH 隧道 + tmux 多会话。

```bash
spod                # 交互选择 / 新建会话
spod new            # 新建 spod-1, spod-2...
spod new dev        # 新建 spod-dev
spod dev            # 连到 spod-dev（没有就创建）
spod ls             # 列出所有会话
spod kill dev       # 关掉 spod-dev
spod killall        # 全关
spod tunnel         # 只管隧道
spod tunnel stop    # 关隧道
spod ssh            # 裸 SSH（不进 tmux）
```

核心能力：
- **autossh 反向隧道**：自动维护 SuperPod → 本地 Clash 代理的隧道，断线自动重建
- **tmux 多会话**：SSH 断了进程不丢，重连后 `spod` 直接恢复
- **防 Broken pipe**：15s 心跳探测，60s 容忍网络抖动

### [VPN Auto-Connect](./HKUST-VPN-README.md)

全自动 HKUST VPN 连接：Playwright 完成 Microsoft SSO + TOTP MFA，openconnect + vpn-slice split tunneling，只有学校流量走 VPN。

### [SuperPod Session Management](./SUPERPOD-SESSION-README.md)

SuperPod SLURM 容器化 GPU 会话管理指南（Claude Code + MCP 交互式终端）。

### [文件同步](./SYNC-README.md)

从 SuperPod 拉取训练数据到本地，支持并行下载和断点续传。

## Quick Start

### 首次设置

```bash
# 1. 克隆项目
git clone https://github.com/ZhangShuui/hkust-superpod-auto.git
cd hkust-superpod-auto

# 2. 安装 Python 依赖
pip install pyotp playwright vpn-slice
playwright install chromium

# 3. 安装系统依赖
sudo apt install openconnect autossh

# 4. 编译安装 spod CLI
cd cmd/spod && go build -o spod . && cp spod ~/.local/bin/ && cd ../..

# 5. 配置环境变量
cp .env.example .env
# 编辑 .env，填入你的 ITSC ID 等信息

# 6. 配置 SSH (~/.ssh/config)
cat >> ~/.ssh/config << 'EOF'
Host superpod
    HostName superpod.ust.hk
    User <your-itsc-id>
    ServerAliveInterval 15
    ServerAliveCountMax 4
    TCPKeepAlive yes
EOF

# 7. 首次 VPN 登录（输入密码、TOTP 密钥）
python3 hkust-vpn.py --setup

# 8. 配置 SuperPod 上的 Claude Code 环境（见下方「SuperPod 环境配置」）
```

### 日常使用

```bash
# 终端 1：启动 VPN（挂着不动）
python3 hkust-vpn.py

# 终端 2：一键连接 SuperPod
spod

# SuperPod 上运行 Claude Code
claude
```

## 架构

```
本地 (WSL2)
    │
    ├─ hkust-vpn.py ─── openconnect + vpn-slice ─── HKUST 内网
    │                                                     │
    ├─ Clash (7897) ◄── autossh 反向隧道 ◄──── SuperPod:17897
    │                                                     │
    └─ spod CLI ── SSH + tmux ──────────────────── SuperPod
                                                          │
                                                    Claude Code
                                                    (API → 17897 → 隧道 → Clash → Anthropic)
```

## 安装

### 前置条件

- WSL2 (Ubuntu)
- Python 3.10+ / openconnect / vpn-slice / Playwright
- Go 1.22+ (编译 spod)
- autossh / tmux (SuperPod 已有 tmux)
- Clash 代理 (本地 7897 端口)

### 编译安装 spod

```bash
cd cmd/spod
go build -o spod .
cp spod ~/.local/bin/   # 确保 ~/.local/bin 在 PATH 中
```

### SuperPod 环境配置

首次使用前需要在 SuperPod 上配置 Claude Code 环境：

```bash
# SSH 到 SuperPod
ssh superpod

# 加载 Anaconda 并创建 Claude 环境
module load Anaconda3/2023.09-0
conda create -n claude nodejs=20 -y
conda activate claude

# 安装 Claude Code
npm install -g @anthropic-ai/claude-code

# 添加到 .bashrc（自动激活）
echo 'conda activate claude' >> ~/.bashrc

# 设置代理环境变量（通过反向隧道走本地 Clash）
cat >> ~/.bashrc << 'EOF'

# Proxy via SSH reverse tunnel (local Clash)
export http_proxy=http://127.0.0.1:17897
export https_proxy=http://127.0.0.1:17897
export HTTP_PROXY=http://127.0.0.1:17897
export HTTPS_PROXY=http://127.0.0.1:17897
EOF
```

### SSH 配置

`~/.ssh/config` 已自动配置：

```
Host superpod
    HostName superpod.ust.hk
    User <your-itsc-id>
    ServerAliveInterval 15
    ServerAliveCountMax 4
    TCPKeepAlive yes
```

## 日常工作流

```bash
# 终端 1：启动 VPN（挂着不动）
python3 hkust-vpn.py

# 终端 2：连接 SuperPod
spod                    # 交互选择或新建会话
claude                  # 在 SuperPod 上用 Claude Code

# 断线了？
spod                    # 重新连接，tmux 保住了之前的 Claude Code

# 开第二个会话
spod new                # 自动编号 spod-2

# 查看所有会话
spod ls
```

## 项目结构

```
├── cmd/spod/           # spod CLI (Go)
│   ├── main.go
│   └── go.mod
├── hkust-vpn.py        # VPN 自动连接脚本
├── .env.example        # 配置模板
├── sync.sh             # SuperPod 文件同步
├── speedtest.sh        # VPN 测速
├── HKUST-VPN-README.md
├── SUPERPOD-SESSION-README.md
└── SYNC-README.md
```
