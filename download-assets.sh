#!/bin/bash
# download-assets.sh - 下载 v2aynn-web 运行所需的第三方资源
#
# 用法: bash download-assets.sh [arch] [--with-xray]
#       arch 默认为 arm64，可选 amd64
#       --with-xray  额外下载 xray（只在需要回滚到「双内核」旧版本时才用）
#
# 环境变量:
#       ASSETS_BASE  资源前缀，默认 https://github.com
#                    用于走镜像站，或离线验证脚本本身（指向本地 HTTP 服务）
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
BASE="${ASSETS_BASE:-https://github.com}"

echo "================================================"
echo " 下载 mihomo ${MIHOMO_VERSION} (${ARCH}) + geo 数据"
echo " 资源前缀: ${BASE}"
echo "================================================"

TMP="$(mktemp -d)"
# 结尾的 `|| true` 不能省：trap 里的失败在 `set -e` 下会变成脚本的退出码，
# 于是「东西全下好了」也会报失败。清理失败本来就不该让整个脚本判成失败。
trap 'rm -rf "$TMP" 2>/dev/null || true' EXIT

# 从 GitHub 拉东西在国内多半要挂代理，而代理后面的节点质量参差 —— 实测过
# 「下到一半彻底不动」「18 分钟还卡在第一步」这些情况。所以：
#   --speed-limit/--speed-time  速度低于 1KB/s 持续 30 秒就掐掉，交给 --retry 重来
#   --retry / --connect-timeout 抖动自动重来，最多 4 次；连不上 15 秒就放弃这次
CURL_OPTS=(-fL --connect-timeout 15 --retry 3 --retry-delay 2 --speed-limit 1024 --speed-time 30)

# fetch <url> <输出文件>
#
# 先问一下远端大小，分三种情况处理 —— 这里的判断顺序很讲究：
#   本地 == 远端  → 已下全，跳过（重复跑脚本不用重下）
#   本地 <  远端  → 上次没下完，**保留文件**让 `-C -` 接着下（删掉就等于每次重来）
#   本地 >  远端  → 本地这份是脏的（比如换了版本）。必须删：`-C -` 会带着
#                   `Range: bytes=<比远端还大的偏移>-` 去请求，服务端回 416，
#                   `-f` 让 curl 直接失败 —— 于是「重复跑一遍脚本」反而报错。
fetch() {
  local url="$1" out="$2" want have
  want="$(curl -fsIL --connect-timeout 15 --max-time 60 "$url" 2>/dev/null \
          | tr -d '\r' | awk 'tolower($1)=="content-length:"{n=$2} END{print n}')"
  if [ -f "$out" ]; then
    have="$(wc -c < "$out" | tr -d ' ')"
    if [ -n "$want" ] && [ "$have" = "$want" ]; then
      echo "  $(basename "$out") 已是完整文件（$want 字节），跳过"
      return 0
    fi
    # 远端大小拿不到时无从判断，只能删了重下，避免踩 416
    if [ -z "$want" ] || [ "$have" -gt "$want" ]; then
      rm -f "$out"
    else
      echo "  $(basename "$out") 上次下到 $have/$want 字节，接着下"
    fi
  fi
  curl "${CURL_OPTS[@]}" -C - -o "$out" "$url"
}

# 1. mihomo 内核（唯一内核，必需）
echo "[1/3] 下载 mihomo 内核..."
fetch "${BASE}/MetaCubeX/mihomo/releases/download/${MIHOMO_VERSION}/mihomo-linux-${ARCH}-${MIHOMO_VERSION}.gz" "$TMP/mihomo.gz"
gunzip -f "$TMP/mihomo.gz"
mv "$TMP/mihomo" ./mihomo
chmod +x mihomo

# 2. mihomo 专用 geo 数据（必需）
#    geoip.metadb 是硬依赖：缺了 mihomo 会去 GitHub 下载，墙内卡满 90 秒超时，
#    期间代理端口完全不监听，看起来就像「启动失败」。
#    geosite.dat 用于 GEOSITE,cn,DIRECT 分流规则，普通节点的 smart 模式要用。
#    注意：这两个文件与 xray 的 geoip.dat / geosite.dat **同名但格式不同，不能混用**。
echo "[2/3] 下载 mihomo geo 数据..."
fetch "${BASE}/MetaCubeX/meta-rules-dat/releases/download/latest/geoip.metadb" geoip.metadb
fetch "${BASE}/MetaCubeX/meta-rules-dat/releases/download/latest/geosite.dat" geosite.dat

# 3. 可选：xray（回滚保险，默认不装）
if [ "$WITH_XRAY" = "1" ]; then
  echo "[3/3] 下载 xray（可选，用于回滚）..."
  fetch "${BASE}/XTLS/Xray-core/releases/download/${XRAY_VERSION}/Xray-linux-${ARCH}.zip" "$TMP/xray.zip"
  unzip -o "$TMP/xray.zip" xray -d . >/dev/null
  chmod +x xray
  # xray 用的 geo 数据（格式与 mihomo 的不同）
  fetch "${BASE}/v2fly/geoip/releases/latest/download/geoip.dat" geoip.dat
  fetch "${BASE}/v2fly/domain-list-community/releases/latest/download/dlc.dat" geosite.dat.xray
  echo "  ⚠️ xray 的 geosite 已下载为 geosite.dat.xray —— 它会覆盖 mihomo 的 geosite.dat，"
  echo "     需要回滚时再手动改名成 geosite.dat。"
else
  echo "[3/3] 跳过 xray（内核统一后不再需要；要装请加 --with-xray）"
fi

echo ""
echo "完成！资源文件已就绪:"
ls -lh mihomo geoip.metadb geosite.dat 2>/dev/null || true
if [ "$WITH_XRAY" = "1" ]; then
  ls -lh xray geoip.dat 2>/dev/null || true
fi
echo ""
echo "把 mihomo 放到 /usr/local/bin/，geoip.metadb 与 geosite.dat 放到数据目录（DATA_DIR）。"
