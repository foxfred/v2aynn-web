# v2aynn-web

低资源 Linux 设备（TV 盒子 / 路由器 / 树莓派）上的代理订阅管理器。基于 Xray-core，提供 Web 管理界面 + SOCKS5 / HTTP / 透明代理。

> **🤖 本项目由 AI 编程生成**（Claude），代码、文档、部署脚本均为人机协作的 AI 编程产物。

---

## ✨ 功能特点

- **多订阅聚合**：支持多个订阅源合并管理，可配置订阅代理，支持周期自动刷新或关闭
- **协议支持**：vmess / vless / trojan / ss 四种主流协议自动解析
- **三种代理入口**：SOCKS5 (默认 10808) + HTTP (默认 10810) + 透明代理 (12345)，一键启停
- **智能分流**：国内域名/IP 直连（geosite:cn / geoip:cn），海外走代理；可切换全局模式
- **节点管理**：
  - 全部并发测速（每批 5 个），结果实时刷新
  - 延迟列独立显示，支持按延迟升/降序排序并**持久化保存**
  - 自动去重（相同 服务器:端口:协议 只保留一个）
  - 当前连接节点高亮
- **纯单页 Web 界面**：内联 CSS/JS，无任何外部依赖，移动端 / 电视浏览器友好
- **可靠性**：
  - 断电重启自动重连（带 5 次重试）
  - xray 进程崩溃自动拉起
  - 测试数据（ping/sort）随配置持久化，跨重启不丢失
- **零 Go 外部依赖**：仅使用标准库，单二进制 ~5MB
- **无需 Docker**：`start.sh` 一键启动，自动安装 xray 与 geo 数据

---

## 🖥 技术栈

| 组件 | 说明 |
|------|------|
| Go | 单二进制 Web 服务（仅标准库） |
| Xray-core | 代理内核（VMess/VLESS/Trojan/Shadowsocks） |
| Alpine/Linux | arm64 / amd64 均可，低至 64MB 内存可用 |

---

## 🚀 快速开始

### 方式一：一键脚本（推荐）

```bash
# 1. 下载 xray + geo 数据（约 60MB，仅首次需要）
bash download-assets.sh        # 默认 arm64；amd64: bash download-assets.sh amd64

# 2. 启动
bash start.sh

# 3. 浏览器打开
#    http://<设备IP>:8000
```

### 方式二：systemd 服务（开机自启）

```ini
# /etc/systemd/system/v2aynn-web.service
[Unit]
Description=v2aynn-web proxy gateway
After=network.target

[Service]
Type=simple
Environment=DATA_DIR=/opt/v2aynn-web/data
ExecStart=/opt/v2aynn-web/v2aynn-web
Restart=always

[Install]
WantedBy=multi-user.target
```

```bash
sudo systemctl enable --now v2aynn-web
```

### 方式三：本地编译

```bash
# 要求 Go 1.22+
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -ldflags="-s -w" -o v2aynn-web ./cmd/server/
```

---

## 🔌 使用说明

1. **添加订阅**：在首页「订阅」栏填入名称和订阅 URL，点击添加
2. **测速排序**：点击「全部测速」测试所有节点延迟；点击延迟列标题按延迟排序（排序结果自动保存）
3. **切换节点**：点击节点即可切换，已连接节点有蓝框高亮
4. **透明代理**：可选，将局域网设备网关指向本机即可

---

## ⚙️ 端口一览

| 端口 | 用途 |
|------|------|
| 8000 | Web 管理界面 |
| 10808 | SOCKS5 代理 |
| 10810 | HTTP 代理 |
| 12345 | 透明代理（dokodemo-door） |

---

## 📦 项目结构

```
v2aynn-web/
├── cmd/server/main.go        # 入口：配置加载、订阅轮询、自动启动
├── internal/
│   ├── config/               # 配置结构、JSON 持久化、UUID
│   ├── subscription/         # 订阅拉取、协议解析、去重
│   ├── v2ray/                # xray 进程管理、配置生成
│   └── web/                  # HTTP API + 内嵌单页前端
├── download-assets.sh        # 下载 xray + geo 资源
├── start.sh                  # 一键启动
└── transparent.sh            # 透明代理开关脚本
```

---

## ⚖️ 免责声明

- 本项目仅用于学习与个人技术研究，请遵守所在地法律法规
- 含有的 Xray-core、pxy 数据等为第三方开源组件，版权归各自作者所有，请通过 `download-assets.sh` 从官方源获取