#!/bin/bash
# download-assets.sh - 下载 v2aynn-web 运行所需的第三方资源
# 用法: bash download-assets.sh [arch]   arch 默认为 arm64
set -e

ARCH="${1:-arm64}"
VERSION="v26.3.27"   # Xray-core 版本，按需调整

echo "================================================"
echo " 下载 Xray-core ${VERSION} (${ARCH}) + geo 数据"
echo "================================================"

# 1. Xray-core 二进制
echo "[1/2] 下载 xray 核心..."
XRAY_URL="https://github.com/XTLS/Xray-core/releases/download/${VERSION}/Xray-linux-${ARCH}.zip"
curl -L -o /tmp/xray.zip "$XRAY_URL"
unzip -o /tmp/xray.zip xray -d . 2>/dev/null || unzip -o /tmp/xray.zip
chmod +x xray

# 2. geoip.dat / geosite.dat
echo "[2/2] 下载 geo 数据..."
curl -L -o /tmp/geoip.zip "https://github.com/v2fly/geoip/releases/latest/download/geoip.dat"
curl -L -o /tmp/geosite.zip "https://github.com/v2fly/domain-list-community/releases/latest/download/dlc.dat"
mv /tmp/geoip.zip geoip.dat 2>/dev/null || true
# dlc.dat 即 geosite 的新名，兼容旧版本名
if [ -f /tmp/geosite.zip ]; then
  cp /tmp/geosite.zip geosite.dat
fi

rm -f /tmp/xray.zip /tmp/geoip.zip /tmp/geosite.zip

echo "完成！资源文件已就绪:"
ls -lh xray geoip.dat geosite.dat