#!/bin/bash
# VPN 隧道实时测速
# 用法: ./speedtest.sh [秒数]  (默认 60 秒)

DURATION=${1:-60}

RX1=$(awk '/tun0/{print $2}' /proc/net/dev)
echo "[*] 测速中... (${DURATION}s)"
sleep "$DURATION"
RX2=$(awk '/tun0/{print $2}' /proc/net/dev)

BYTES=$(( RX2 - RX1 ))
MB=$(echo "scale=1; $BYTES/1024/1024" | bc)
SPEED=$(echo "scale=2; $BYTES/1024/1024/$DURATION" | bc)

echo "[+] ${DURATION}s 接收: ${MB} MB"
echo "[+] 平均速度: ${SPEED} MB/s"
