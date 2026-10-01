#!/bin/bash
# download-assets.sh - 下载 v2aynn-web 运行所需的第三方资源
#
# 用法: bash download-assets.sh [arch] [--with-xray]
#       arch 默认为 arm64，可选 amd64
#       --with-xray  额外下载 xray（只在需要回滚到「双内核」旧版本时才用）
#
# 2026-10-01 内核统一之后，普通节点与家宽节点都由 mihomo 承载，
# xray 不再是运行必需 —— 但代码与二进制暂时保留作回滚保险，所以这里
# 仍提供可选的 xray 下载，默认不装。
set -e

ARCH="arm64"
WITH_XRAY=0
for a in "$@"; do
  case "$a" in
    --with-xray) WITH_XRAY=1 ;;
    arm64|amd64) ARCH="$a" ;;
    *) echo "未知参数: $a"; exit 1 ;;
  esac
done

MIHOMO_VERSION="v1.19.31"   # 需 1.19.25 以上，低版本不认 openvpn 节点
XRAY_VERSION="v26.3.27"     # 仅 --with-xray 时使用

echo "================================================"
echo " 下载 mihomo ${MIHOMO_VERSION} (${ARCH}) + geo 数据"
echo "================================================"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

# 1. mihomo 内核（唯一内核，必需）
echo "[1/3] 下载 mihomo 内核..."
curl -fL -o "$TMP/mihomo.gz" \
  "https://github.com/MetaCubeX/mihomo/releases/download/${MIHOMO_VERSION}/mihomo-linux-${ARCH}-${MIHOMO_VERSION}.gz"
gunzip -f "$TMP/mihomo.gz"
mv "$TMP/mihomo" ./mihomo
chmod +x mihomo

# 2. mihomo 专用 geo 数据（必需）
#    geoip.metadb 是硬依赖：缺了 mihomo 会去 GitHub 下载，墙内卡满 90 秒超时，
#    期间代理端口完全不监听，看起来就像「启动失败」。
#    geosite.dat 用于 GEOSITE,cn,DIRECT 分流规则，普通节点的 smart 模式要用。
#    注意：这两个文件与 xray 的 geoip.dat / geosite.dat **同名但格式不同，不能混用**。
echo "[2/3] 下载 mihomo geo 数据..."
curl -fL -o geoip.metadb \
  "https://github.com/MetaCubeX/meta-rules-dat/releases/download/latest/geoip.metadb"
curl -fL -o geosite.dat \
  "https://github.com/MetaCubeX/meta-rules-dat/releases/download/latest/geosite.dat"

# 3. 可选：xray（回滚保险，默认不装）
if [ "$WITH_XRAY" = "1" ]; then
  echo "[3/3] 下载 xray（可选，用于回滚）..."
  curl -fL -o "$TMP/xray.zip" \
    "https://github.com/XTLS/Xray-core/releases/download/${XRAY_VERSION}/Xray-linux-${ARCH}.zip"
  unzip -o "$TMP/xray.zip" xray -d . >/dev/null
  chmod +x xray
  # xray 用的 geo 数据（格式与 mihomo 的不同）
  curl -fL -o geoip.dat "https://github.com/v2fly/geoip/releases/latest/download/geoip.dat"
  curl -fL -o geosite.dat.xray "https://github.com/v2fly/domain-list-community/releases/latest/download/dlc.dat"
  echo "  ⚠️ xray 的 geosite 已下载为 geosite.dat.xray —— 它会覆盖 mihomo 的 geosite.dat，"
  echo "     需要回滚时再手动改名成 geosite.dat。"
else
  echo "[3/3] 跳过 xray（内核统一后不再需要；要装请加 --with-xray）"
fi

echo ""
echo "完成！资源文件已就绪:"
ls -lh mihomo geoip.metadb geosite.dat 2>/dev/null
[ "$WITH_XRAY" = "1" ] && ls -lh xray geoip.dat 2>/dev/null
echo ""
echo "把 mihomo 放到 /usr/local/bin/，geoip.metadb 与 geosite.dat 放到数据目录（DATA_DIR）。"
