#!/bin/bash
#============================================================
# 透明代理（网关模式）开启脚本 - 在盒子上执行一次
# 开启后: 电脑改网关为盒子IP, 全局走代理, 无需装v2rayN
# 用法: sudo bash transparent.sh  enable|disable
#============================================================
set -e

LAN_IF=$(ip route | grep default | awk '{print $5}' | head -1)
BOX_IP=$(hostname -I | awk '{print $1}')
TPORT=12345

if [ "$1" = "disable" ]; then
  echo "关闭透明代理..."
  iptables -t nat -D PREROUTING -i "$LAN_IF" -j V2AYNN 2>/dev/null || true
  iptables -t nat -F V2AYNN 2>/dev/null || true
  iptables -t nat -X V2AYNN 2>/dev/null || true
  iptables -t nat -D POSTROUTING -s 192.168.0.0/16 -j MASQUERADE 2>/dev/null || true
  sysctl -w net.ipv4.ip_forward=0 > /dev/null 2>&1 || true
  sed -i '/net.ipv4.ip_forward=1/d' /etc/sysctl.conf 2>/dev/null || true
  echo "已关闭。电脑把网关改回原路由器IP即可。"
  exit 0
fi

echo "=========================================="
echo "开启透明代理（网关模式）"
echo "出口网卡: $LAN_IF   盒子IP: $BOX_IP"
echo "=========================================="

# 1. 开启IP转发
sysctl -w net.ipv4.ip_forward=1 > /dev/null
if ! grep -q "net.ipv4.ip_forward=1" /etc/sysctl.conf; then
  echo "net.ipv4.ip_forward=1" >> /etc/sysctl.conf
fi

# 2. 清理旧规则（先删引用再删链，保证可重复执行）
iptables -t nat -D PREROUTING -i "$LAN_IF" -j V2AYNN 2>/dev/null || true
iptables -t nat -F V2AYNN 2>/dev/null || true
iptables -t nat -X V2AYNN 2>/dev/null || true

# 3. 创建新链
iptables -t nat -N V2AYNN

# 访问盒子自身的流量放行（管理页面8000/测速等不受影响）
iptables -t nat -A V2AYNN -d "$BOX_IP" -j RETURN
# 内网地址直连
iptables -t nat -A V2AYNN -d 192.168.0.0/16 -j RETURN
iptables -t nat -A V2AYNN -d 10.0.0.0/8 -j RETURN
iptables -t nat -A V2AYNN -d 172.16.0.0/12 -j RETURN
iptables -t nat -A V2AYNN -d 127.0.0.0/8 -j RETURN
# 其余TCP重定向到xray透明端口
iptables -t nat -A V2AYNN -p tcp -j REDIRECT --to-ports "$TPORT"

# 4. 应用到入站（只处理其他设备发给盒子的流量）
iptables -t nat -A PREROUTING -i "$LAN_IF" -j V2AYNN

# 5. 出口NAT（电脑流量经盒子出去时改写源IP）
iptables -t nat -A POSTROUTING -s 192.168.0.0/16 -j MASQUERADE

echo ""
echo "=========================================="
echo " 已开启！电脑端设置："
echo " ----------------------------------------"
echo " IP:   保持原IP ($BOX_IP 同网段)"
echo " 网关: $BOX_IP   (改为盒子IP)"
echo " DNS:  原路由器IP 或 223.5.5.5"
echo "=========================================="
echo "关闭: sudo bash $0 disable"