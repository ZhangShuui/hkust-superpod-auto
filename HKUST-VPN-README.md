# HKUST VPN Auto-Connect

一键连接 HKUST VPN，全自动完成 Microsoft SSO + TOTP MFA 认证，通过 split tunneling 只让指定主机走 VPN，不影响其他网络连接。

## 原理

```
hkust-vpn.py
    │
    ├─ Playwright (Chromium) 自动完成 Microsoft 登录 + TOTP
    │   ├─ 填邮箱 → 填密码 → 选 TOTP → 填验证码 → 确认
    │   └─ 提取 DSID session cookie
    │
    └─ openconnect + vpn-slice 建立 VPN 隧道
        ├─ 只有指定 host (如 superpod.ust.hk) 走 VPN
        └─ 其他流量 (Claude API, 浏览器等) 不受影响
```

## 前置条件

### 系统依赖

```bash
sudo apt install openconnect libnss3 libxcomposite1 libxdamage1 \
    libxrandr2 libxtst6 libgbm1 libasound2t64 libxkbcommon0
```

### Python 依赖

```bash
pip install pyotp playwright vpn-slice
playwright install chromium
sudo playwright install-deps chromium
```

### TOTP 密钥（一次性设置）

1. 打开 https://mysignins.microsoft.com/security-info
2. 点 "Add sign-in method" → "Authenticator app"
3. 选 "I want to use a different authenticator app"
4. 在 QR 码页面点 "Can't scan the image?"
5. **复制 Base32 密钥**（只显示一次）
6. 同时用手机 Authenticator 扫码作为备份

## 使用方法

### 首次运行

```bash
python3 ~/wkspace/stuffs/hkust-vpn.py --setup
```

按提示输入：
- HKUST 密码
- TOTP 密钥（Base32）
- 本机 sudo 密码

凭据保存在 `~/.config/hkust-vpn/credentials.json`（权限 600）。

### 日常使用

```bash
# 一键连接（默认开启自动重连，session 过期后自动重新登录）
python3 ~/wkspace/stuffs/hkust-vpn.py

# 连上后 VPN 进程会在前台运行，另开终端操作：
ssh szhangfa@superpod.ust.hk

# 断开 VPN：在 hkust-vpn.py 所在终端按 Ctrl+C（按两次彻底退出）
```

### 全部参数

```
用法: hkust-vpn.py [-h] [-u USER] [--proxy PROXY] [--no-proxy]
                    [--hosts HOSTS [HOSTS ...]] [--cookie COOKIE]
                    [--setup] [--headless]

参数:
  -u, --user USER       ITSC 账号 (默认: szhangfa@connect.ust.hk)
  --proxy PROXY         HTTP 代理 (默认: http://127.0.0.1:7890)
  --no-proxy            不使用代理
  --hosts HOSTS         走 VPN 的主机列表 (默认: superpod.ust.hk)
  --cookie COOKIE       跳过登录，直接用 DSID cookie
  --setup               重新输入凭据
  --headless            无浏览器窗口（完全静默）
  --no-reconnect        禁用自动重连（仅单次 session）
```

### 常用示例

```bash
# 多个主机走 VPN
python3 hkust-vpn.py --hosts superpod.ust.hk other-server.ust.hk

# 无浏览器窗口（后台运行）
python3 hkust-vpn.py --headless

# 不走代理直连（在校内网络时）
python3 hkust-vpn.py --no-proxy

# 已有 cookie，跳过浏览器登录
python3 hkust-vpn.py --cookie "3bd3654e8cb5b982bc2a..."

# 更换密码或 TOTP 密钥
python3 hkust-vpn.py --setup

# 单次连接（不自动重连）
python3 hkust-vpn.py --no-reconnect

# 无浏览器窗口（最佳挂机方式，自动重连默认开启）
python3 hkust-vpn.py --headless
```

## VPN Session 限制

HKUST VPN 有以下时间限制：

- **空闲超时**: 30 分钟无活动自动断开
- **最大 Session 时长**: 240 分钟（4 小时）

默认自动重连，过期后自动重新登录并重连，无需手动操作。使用 `--no-reconnect` 可禁用。

## 文件说明

```
~/wkspace/stuffs/hkust-vpn.py           # 主脚本
~/.config/hkust-vpn/credentials.json     # 凭据存储 (chmod 600)
~/.claude/skills/ssh-remote/SKILL.md     # Claude Code SSH skill
```

## 工作流程

```
                你的电脑 (WSL2)
                    │
        ┌───────────┼───────────┐
        │           │           │
   Claude API   hkust-vpn.py   浏览器
   (直连网络)      │           (直连网络)
                   │
              openconnect
              + vpn-slice
                   │
              ┌────┴────┐
              │ VPN 隧道 │  ← 只有 superpod.ust.hk 走这条
              └────┬────┘
                   │
            HKUST 内网
                   │
           superpod.ust.hk
```

## 故障排查

### 登录失败

调试截图自动保存在 `/tmp/hkust-vpn-debug-*.png`，查看具体卡在哪一步。

### vpn-slice not found

脚本已硬编码路径 `/home/shurui/anaconda3/bin/vpn-slice`。如果 anaconda 路径变了，修改脚本中的 `VPN_SLICE_PATH`。

### 连接超时

确认 Clash 代理正在运行（`http://127.0.0.1:7890`），或使用 `--no-proxy`（校内网络时）。

### DSID cookie 过期

HKUST VPN session 最长 4 小时，过期后脚本会自动重新登录并重连（默认行为）。

### TOTP 验证码错误

确保系统时间准确：

```bash
# 检查时间
date

# 如果时间不对，同步
sudo ntpdate pool.ntp.org
```

## 安全注意事项

- 凭据文件权限为 600，仅本人可读
- TOTP 密钥等同于密码，勿泄露
- 如果密钥泄露，立即去 https://mysignins.microsoft.com/security-info 删除旧的 TOTP 并重新生成
- 建议手机 Authenticator 保留一份 TOTP 作为备用登录方式
