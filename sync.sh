#!/bin/bash
# 从 superpod 并行拉取 trains 和 trains_big5 到 E 盘
# 支持断点续传，VPN 断了重连后再跑一次即可

set -e

# 加载 .env
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
if [[ -f "$SCRIPT_DIR/.env" ]]; then
  set -a; source "$SCRIPT_DIR/.env"; set +a
fi

SSH_USER="${SUPERPOD_USER:?Set SUPERPOD_USER in .env or environment}"
SRC="$SSH_USER@superpod.ust.hk:/project/hdtaccuracy/trains"
DST="/mnt/e/hdtaccuracy/trains/"

# 先杀掉已有的 rsync
pkill -f "rsync -rlP.*hdtaccuracy" 2>/dev/null || true
sleep 1

mkdir -p "$DST"

# trains 子目录并行
for dir in converted_hf choice-sft-full gsspo gsspo_cot llama-2-7b-tky nyc-llama2-7 gspo_cot choice-sft code-llama-tky code-llama-nyc ppo llama2-7 sft codeT5-tky codeT5-nyc grpo_cot grpo gspo; do
  rsync -rlP --partial --inplace --no-times --no-perms "$SRC/$dir" "$DST" 2>/dev/null &
done

# trains_big5
rsync -rlP --partial --inplace --no-times --no-perms \
  "$SSH_USER@superpod.ust.hk:/project/hdtaccuracy/trains_big5" /mnt/e/hdtaccuracy/ 2>/dev/null &

sleep 3
COUNT=$(ps aux | grep "rsync -rlP" | grep -v grep | wc -l)
echo "[+] 启动了 $COUNT 路 rsync"
echo "[*] 查看进度: du -sh /mnt/e/hdtaccuracy/trains/ /mnt/e/hdtaccuracy/trains_big5/"
echo "[*] 杀掉所有: pkill -f 'rsync -rlP.*hdtaccuracy'"
