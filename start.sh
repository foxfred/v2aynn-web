#!/bin/bash
# v2aynn-web 快速启动（无需编译，无需 Docker）
cd "$(dirname "$0")"

# 安装 xray 到系统
sudo cp xray /usr/local/bin/xray
sudo chmod +x /usr/local/bin/xray
sudo mkdir -p /usr/local/share/xray
sudo cp geoip.dat geosite.dat /usr/local/share/xray/

# 启动
chmod +x v2aynn-web
mkdir -p data
DATA_DIR="$(pwd)/data" nohup ./v2aynn-web > v2aynn-web.log 2>&1 &
echo $! > v2aynn-web.pid

IP=$(hostname -I | awk '{print $1}')
echo "================================"
echo " Web GUI: http://$IP:8000"
echo " SOCKS5:  $IP:10808"
echo " HTTP:    $IP:10810"
echo "================================"