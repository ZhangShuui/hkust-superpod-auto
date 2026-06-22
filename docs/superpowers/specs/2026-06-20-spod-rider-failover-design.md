# spod rider — 多登录节点故障转移登录（设计）

日期：2026-06-20
状态：设计已与用户对齐，待评审 → 进入实现计划

## 背景与问题

SuperPod 有两个登录节点：

| 节点 | 内网 IP | relay 进程 | 反向隧道上游 |
|------|---------|-----------|-------------|
| slogin-01 | 10.22.4.12 | 在（NFS 共享 home，两节点都跑） | **活**（provider 的 `ssh -R` 落在这） |
| slogin-02 | 10.22.4.13 | 在 | **死**（`17140` 是空 LISTEN，无 ESTAB） |

两节点真实地址全是内网（`10.22.x` / `10.24.x`），没有 `143.89` 地址。`superpod.ust.hk` 只解析到一个 VIP `143.89.184.2`，它是多节点负载均衡入口（证据：从 SuperPod 内部经 SOCKS 连这个 VIP 会落到 slogin-02）。

由此：

- **provider（你，经 VPN）**：只有 VIP 单入口，落到哪个节点由 LB 决定，**选不了节点**；但 slogin-01 宕机时，`spod-tunnel`/`spod-socks`（systemd）会经 VIP 自动重连、LB 转到 slogin-02、relay 跟着过去——**provider 侧自动兜底，无需手动**。
- **rider（同事，经 provider 的公网 SOCKS）**：能用内网 IP 直接点名节点，但若登到 relay 死的节点（隧道不在那），codex/claude 会报 `Proxy connection failed: HTTP CONNECT response missing status line`（已实测复现：slogin-02 经本地 relay `curl chatgpt` 得 `CONNECT aborted / 000`；slogin-01 得 `405`）。
- relay 的“活节点”= provider 隧道的当前落点，是被动结果，rider 应**探测并跟随**，而不是写死某个 IP。

## 目标

1. rider 一条命令 `spod rider` 自动连到“relay 活的”节点的 tmux；slogin-01 宕机、provider 被 LB 转到 slogin-02 时，rider 自动跟到 slogin-02，**不手动改 IP**。
2. **贴合现有 spod**：复用 rider 基建 / 端口算法 / ssh config / tmux 逻辑，改动集中在“一个子命令 + 一个函数”。
3. provider 侧零改动。

## 非目标（YAGNI）

- 不改 provider 侧（VIP + systemd 已自动兜底）。
- 不做 provider 经 VPN 的节点选择（VPN 单 VIP 入口，做不了也不需要）。
- 不做持久化失败计数 / 状态文件（每次连接现场探测活节点即可，无需跨进程状态）。
- 不给 slogin-02 自动建第二条隧道（本设计靠“跟随活节点”，不改隧道拓扑）。

## 复用的现有机制

- **rider 基建**：`ensureVPN()`（`main.go:463`）已有 `SPOD_NO_VPN=1` → 跳过本地 VPN 检查、借 provider 的 VPN/隧道/relay；`SUPERPOD_SSH_PROXY` → `ensureSSHConfig()` 写出经 SOCKS 的 `ProxyCommand nc -X 5 -x <proxy> %h %p`。
- **端口算法**：`ensurePorts()` → `relayPort = 18000 + uid%1000`（远端 UID 推导）。
- **连接**：`ensureSSHConfig()` 写 `~/.ssh/config` 的 `Host superpod`；`sshInteractive("tmux attach -t <name> 2>/dev/null || tmux new -s <name>")`。

## 设计

### 1. 新增子命令 `case "rider"`

在 `main()` 的子命令 switch 中新增 `case "rider":`，调用 `riderConnect()`。其余子命令、无参 `spod`（provider 默认连接）行为完全不变。

### 2. `riderConnect()` 流程

1. **rider 模式**：等效 `SPOD_NO_VPN=1`——跳过 `ensureVPN()` / `ensureTunnel()` / `ensureSocks()`（这些是 provider 的，rider 借用，不自建）。
2. **校验**：要求 `SUPERPOD_SSH_PROXY` 已配（rider 必须经 provider 的 SOCKS）；未配则报错并提示。
3. `ensurePorts()` 算出 `relayPort`。
4. `host := pickActiveNode(candidates)`——选 relay 活的节点。
5. 把 `host` 作为有效 `SUPERPOD_HOST`，调 `ensureSSHConfig()` 写好 `Host superpod`。
6. `sshInteractive("tmux attach -t <name> 2>/dev/null || tmux new -s <name>")` 连进去（沿用现有 session 命名）。

### 3. `pickActiveNode(candidates)`（唯一新逻辑）

- **候选来源**：`SUPERPOD_HOSTS`（空格分隔）；留空则用内置默认 `["10.22.4.12", "10.22.4.13"]`（slogin-01 优先）。
- 按序对每个 `node` 重试至多 `RETRIES = 3` 次：
  - 经 SOCKS 非交互 ssh 到 `node`（`ConnectTimeout`、`BatchMode=yes`），在节点上跑探测：
    ```
    curl -sf -x http://127.0.0.1:<relayPort> --max-time 8 \
         -o /dev/null -w '%{http_code}' \
         https://chatgpt.com/backend-api/codex/responses
    ```
  - `http_code == 405` ⇒ relay 端到端活（已到 OpenAI）⇒ 返回该 `node`。
  - 否则短暂 sleep 后重试（迟滞，避免抖动误切；这就是用户要的“连失败几次才切”）。
- 所有候选都不活 ⇒ 返回空并 `warn`：「活节点未就绪（provider 隧道可能在重连），稍等重试」，**不连**（连了 codex 也用不了）。
- **判据用 `405`（relay 到 OpenAI 端到端通）而非“22 端口可连”**：目标是 codex 真能用，光连上没意义（slogin-02 能连但 relay 死）。

### 4. SSH 连接复用细节

- 探测 ssh 与登录 ssh 都经同一 `SUPERPOD_SSH_PROXY`（`nc -X 5 -x` SOCKS）。
- 用 `ControlMaster=auto` + 临时 `ControlPath` 复用同一条 TCP，把一次 `spod rider` 的 SSH 连接数降到最小（避免被 SuperPod 限流/封号，见 `feedback_superpod_ssh_session`）。
- ⚠️ 注意与 systemd 隧道相反：隧道要 `ControlPath=none`（长连接须独立，否则秒退 flap）；rider 登录探测要复用。两个场景不冲突。

## 错误处理 / 边界情况

- `SUPERPOD_SSH_PROXY` 未配：报错，提示 rider 必须经 provider 的 SOCKS（例：`SUPERPOD_SSH_PROXY=<provider-ip>:1080`）。
- 候选节点全不活：`warn` + 不连 + 建议稍等重试（provider 隧道收敛中）。
- 候选只有一个：退化为“探测该节点活否，活则连，否则报错”。
- 向后兼容：不影响无参 `spod` 及其他子命令；provider 不用 `rider` 子命令则毫无变化。

## 测试

- **集成（沿用现有验证手段）**：
  - `pickActiveNode` 对 `10.22.4.12` 探测应返回它（当前 slogin-01 relay 活，`curl 405` 已实测）。
  - 首选临时换成坏/不可达 IP ⇒ 应跳过并选 `10.22.4.13`；若 `.13` relay 死 ⇒ 报“都不活”。
  - `SUPERPOD_SSH_PROXY` 未配 ⇒ 报错退出。
- **手动端到端**：同事机器配好 `.env` 后 `spod rider` ⇒ 落 slogin-01 tmux，codex 可用。

## rider 侧配置汇总（`.env`）

```
SUPERPOD_SSH_PROXY=192.168.77.179:1080   # provider 的 SOCKS（必填）
SUPERPOD_USER=szhangfa
SUPERPOD_HOSTS="10.22.4.12 10.22.4.13"    # 可选，留空用内置默认
# SPOD_NO_VPN 由 rider 子命令内部隐含，无需手配
```

provider 侧：无需任何改动。
