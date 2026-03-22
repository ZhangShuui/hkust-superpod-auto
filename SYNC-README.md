# SuperPod 文件同步工具

从 HKUST SuperPod 拉取文件到本地移动硬盘（E 盘），通过 VPN 隧道传输。

## 前置条件

- VPN 已连接：`uv run python hkust-vpn.py`
- E 盘已挂载：`sudo mkdir -p /mnt/e && sudo mount -t drvfs E: /mnt/e`
- SSH 免密登录已配置：`ssh-copy-id <your-itsc-id>@superpod.ust.hk`

## 同步脚本

```bash
# 启动并行下载（自动杀掉旧进程）
./sync.sh

# 查看进度
du -sh /mnt/e/hdtaccuracy/trains/ /mnt/e/hdtaccuracy/trains_big5/

# 停止所有下载
pkill -f 'rsync -rlP.*hdtaccuracy'
```

支持断点续传，VPN 断了重连后再跑 `./sync.sh` 即可继续。

## 测速脚本

```bash
# 默认测 60 秒
./speedtest.sh

# 指定时长（秒）
./speedtest.sh 30
```

## 下载内容

| 目录 | 大小 | 路径 |
|------|------|------|
| trains | 1.7T | `/project/hdtaccuracy/trains/` → `/mnt/e/hdtaccuracy/trains/` |
| trains_big5 | 885G | `/project/hdtaccuracy/trains_big5/` → `/mnt/e/hdtaccuracy/trains_big5/` |

## 速度参考

| Clash 节点 | 速度 |
|------------|------|
| 默认（节点选择/自动） | ~5 MB/s |
| HK\|Hy2 | ~0.6 MB/s |
| TW\|Hy2 | ~0.1 MB/s |
| DIRECT | ~0.7 MB/s |

结论：不要给 `ust.hk` 加单独的 Clash 规则，走默认「节点选择」最快。

## rsync 参数说明

```
-r          递归
-l          保留符号链接
-P          显示进度 + 保留部分传输文件
--partial   断点续传
--inplace   直接写入目标文件（兼容 drvfs）
--no-times  不设置时间戳（兼容 Windows 文件系统）
--no-perms  不设置权限（兼容 Windows 文件系统）
```
