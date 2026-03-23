# HKUST SuperPod Session Management

通过 Claude Code + MCP 交互式终端，管理 HKUST SuperPod 上的持久 SLURM 容器会话。

## 架构

```
Local (WSL2)
    │
    ├─ hkust-vpn.py (split tunnel VPN)
    │
    └─ Claude Code + mcp-interactive-terminal
         │
         ├─ create_session → SSH 到登录节点
         ├─ send_command → module load slurm
         ├─ send_command → srun (容器化计算节点)
         └─ send_command → 在计算节点上执行任务（GPU 可用）
```

## 前置条件

### 1. VPN

参见 [HKUST-VPN-README.md](./HKUST-VPN-README.md)。

### 2. MCP 交互式终端

```bash
claude mcp add terminal --scope user -- npx -y mcp-interactive-terminal
```

安装后 Claude Code 获得持久终端能力：`create_session` / `send_command` / `read_output` / `send_control`。

### 3. SSH 免密登录

```bash
ssh-keygen -t ed25519  # 如果还没有 key
ssh-copy-id <your-user>@superpod.ust.hk
```

### 4. 禁用 MOTD（推荐）

登录后大量 banner 信息会干扰输出解析：

```bash
ssh <your-user>@superpod.ust.hk "touch ~/.hushlogin"
```

## 使用流程

### Step 1: 检查 VPN

```bash
ping -c 1 -W 3 superpod.ust.hk
```

### Step 2: SSH 到登录节点

```
create_session(command="ssh", args=["<your-user>@superpod.ust.hk"])
```

登录节点 **仅用于** 编辑文件、提交任务、查看队列。**禁止** 在登录节点上跑 GPU/计算任务。

### Step 3: 检查集群状态

```bash
sinfo                        # 节点状态 (idle/mixed/allocated/drain)
savail -p normal             # normal 分区 GPU 可用情况
savail -p preempt            # preempt 分区 GPU 可用情况
squeue -u $USER              # 自己的任务队列
```

根据 `sinfo` 结果，将 `drain` / `down` 状态的节点加入 `--exclude`。

### Step 4: 加载 SLURM 模块

```bash
module load slurm
```

### Step 5: 启动容器化计算节点

```bash
LOCAL_IMAGE="/path/to/your/container.img"

srun --account <your-account> \
     --exclude=<bad-nodes> \
     --partition normal --nodes 1 --gpus <N> \
     --container-image "$LOCAL_IMAGE" \
     --no-container-mount-home \
     --container-mounts /home/$USER:/home/$USER \
     --container-workdir /home/$USER \
     --container-remap-root \
     --container-writable \
     --container-env=PYXI_DISABLE_DEFAULT_MOUNTS=1 \
     --container-save "$LOCAL_IMAGE" \
     --pty bash
```

关键参数：
- `--partition`: `normal`（60% 补贴）或 `preempt`（70% 补贴，1 小时保障窗口）
- `--gpus`: GPU 数量
- `--container-image`: Enroot 容器镜像（本地 .img 或 docker:// URI）
- `--container-remap-root`: 容器内 root 权限
- `--container-writable` + `--container-save`: 容器修改持久化

### Step 6: 在计算节点上工作

进入容器后拥有 root 权限和 GPU 访问。会话持久——工作目录、环境变量、运行中的进程在命令之间保持。

```bash
nvidia-smi                   # 检查 GPU
python train.py              # 跑训练
pip install <package>        # 安装包（容器内 root）
```

### Step 7: 释放资源

```bash
exit                         # 退出容器，释放 GPU
```

## 处理 srun 超时

srun 加载容器镜像需要 **2-5 分钟**，MCP 终端最大 timeout 为 60s。必须使用轮询：

1. `send_command(timeout_ms=60000)` — 大概率返回 `is_complete: false`
2. 等 30s，`read_output` 检查进度
3. 重复直到看到 shell prompt（如 `root@dgx-XX:`）
4. 不要在第一次 read_output 没 prompt 时就认为失败

## 处理问题节点

有时 srun 分配到故障节点（容器加载失败、GPU 错误、卡住）。

**不要硬等。** 处理方式：
1. `send_control(ctrl+c)` 取消
2. 记下分配到的节点名（srun 输出中可见）
3. 加入 `--exclude` 重试
4. 如果某节点持续出问题，永久加入 exclude 列表

## 常用命令速查

### 登录节点

| 命令 | 用途 |
|------|------|
| `sinfo` | 所有节点状态 |
| `squeue` | 所有任务队列 |
| `squeue -u $USER` | 自己的任务 |
| `savail -p <partition>` | 分区 GPU 可用情况 |
| `squota` | CPU/GPU 小时用量 |
| `scancel <jobid>` | 取消任务 |

### 计算节点（容器内）

| 命令 | 用途 |
|------|------|
| `nvidia-smi` | GPU 状态 |
| `nvidia-smi -L` | 列出所有 GPU |
| `python train.py` | 跑训练 |
| `pip install <pkg>` | 安装包（root 权限） |

## 集群信息

- 节点类型：DGX（8x NVIDIA H800 80GB per node）
- 交互式任务限制：480 分钟（8 小时）
- 超过 8 小时的任务用 `sbatch` 提交
- 每周三 10:00-12:00 例行维护（登录节点可能重启）
- 分区补贴：normal 60%、preempt 70%（1 小时保障执行窗口）
