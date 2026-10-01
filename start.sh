#!/bin/bash
# v2aynn-web 快速启动（无需编译，无需 Docker）
cd "$(dirname "$0")"

DATA_DIR="$(pwd)/data"
mkdir -p "$DATA_DIR"

# 1. 安装 mihomo 内核
#    2026-10-01 内核统一后，普通节点与家宽节点都由 mihomo 承载，它是唯一内核。
#    程序固定从 /usr/local/bin/mihomo 找内核（internal/mihomo.DefaultBinPath）。
if [ -f mihomo ]; then
  sudo cp mihomo /usr/local/bin/mihomo
  sudo chmod +x /usr/local/bin/mihomo
else
  echo "⚠️  当前目录没有 mihomo 文件，请先执行：bash download-assets.sh"
fi

# 2. 安装 geo 数据到「数据目录」
#    程序用 -d 把数据目录指定为内核的工作目录，内核从这里读 geo。
#    缺 geoip.metadb 时 mihomo 会联网去 GitHub 下载并卡死 90 秒，期间代理端口不监听。
for f in geoip.metadb geosite.dat geosite.db; do
  if [ -f "$f" ]; then
    cp -f "$f" "$DATA_DIR/"
  fi
done

# 3. 回滚保险：只有当前目录确实放了 xray 才装（旧版双内核方案回滚时才需要，平时不用）
if [ -f xray ]; then
  sudo cp xray /usr/local/bin/xray
  sudo chmod +x /usr/local/bin/xray
  sudo mkdir -p /usr/local/share/xray
  sudo cp geoip.dat geosite.dat /usr/local/share/xray/ 2>/dev/null || true
fi

# 4. 启动
chmod +x v2aynn-web
DATA_DIR="$DATA_DIR" nohup ./v2aynn-web > v2aynn-web.log 2>&1 &
echo $! > v2aynn-web.pid

IP=$(hostname -I | awk '{print $1}')
echo "================================"
echo " Web GUI: http://$IP:8000"
echo " SOCKS5:  $IP:10808"
echo " HTTP:    $IP:10810"
echo "================================"
