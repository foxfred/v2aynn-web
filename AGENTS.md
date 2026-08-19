# v2aynn-web — 项目交接文档

> 本文档面向接手此项目的 AI Agent，涵盖项目架构、代码组织、已完成的修改及未解决问题的完整说明。
>
> **项目性质**：纯 AI 编程生成（Claude），所有代码、文档、部署脚本均为人机协作产物。

---

## 一、项目概述

低资源 Linux 设备（TV 盒子 / 路由器 / 树莓派）上的代理订阅管理器。基于 Xray-core，提供 Web 管理界面 + SOCKS5 / HTTP / 透明代理。

**核心约束**：
- 零外部 Go 依赖（仅标准库）
- 单二进制 ~5MB，arm64 低配设备可用（64MB 内存以上）
- 前端单页 HTML（内联 CSS/JS，无 npm/node）

---

## 二、目录结构

```
v2aynn-web/
├── cmd/server/main.go              # 入口：配置加载、订阅轮询、自动启动重试
├── internal/
│   ├── config/
│   │   ├── config.go                # Config 结构体、JSON 加载/保存、默认值
│   │   └── uuid.go                  # UUID v4 生成（用于节点 ID）
│   ├── subscription/
│   │   └── parser.go                # 订阅拉取、协议解析(vless/vmess/trojan/ss)、去重、JSON订阅支持
│   ├── v2ray/
│   │   └── manager.go               # xray 进程管理、启动/停止/切换、配置生成、崩溃自愈
│   └── web/
│       ├── server.go                # HTTP API 路由、测速(probeNode/probeOnce)、排序、设置
│       ├── probe_test.go            # 测速算法单测（可达TCP/TLS/不可达/TLS不匹配）
│       └── static/index.html        # 单页前端（内联 CSS/JS）
├── download-assets.sh               # 下载 xray + geo 数据
├── start.sh                         # 一键启动脚本
├── transparent.sh                   # 透明代理 iptables 脚本
├── go.mod                           # Go 模块定义
├── README.md                        # 用户文档 + 更新记录
├── AGENTS.md                        # 本文档
└── .gitignore
```

---

## 三、核心架构与数据流

### 3.1 启动流程

```
main.go
  │
  ├── config.Load("/app/data/config.json")
  │     └── 从磁盘加载 JSON，字段缺失用默认值（webPort=8000, socksPort=10808, httpPort=10810）
  │
  ├── v2ray.NewManager(dataDir)
  │     └── 进程管理器，默认 xray 路径 /usr/local/bin/xray
  │
  ├── web.NewServer(cfg, v2m)
  │     └── 创建 HTTP 服务实例
  │
  ├── go subscription.Poll(cfg)
  │     └── 后台定时器：每 cfg.SubRefresh 秒执行一次 Fetch(cfg)
  │
  ├── go func() { 自动启动代理 }
  │     ├── subscription.Fetch(cfg)  // 同步拉取订阅，确保节点有效
  │     └── v2m.Start()              // 重试 5 次，每次间隔 3s
  │
  ├── srv.Run("0.0.0.0:8000")
  │     └── HTTP 服务（路由见 3.2）
  │
  └── <-sig  // 等待退出信号
```

### 3.2 API 路由表

代码位置：`internal/web/server.go` 的 `Run()` 方法

| 方法 | 路径 | 处理函数 | 说明 |
|------|------|---------|------|
| GET | `/api/status` | apiStatus | 运行状态、当前节点名 |
| GET | `/api/nodes` | apiNodes | 节点列表（按 sortOrder 排序后返回） |
| GET | `/api/subs` | apiSubs | 订阅列表 |
| POST | `/api/subs` | apiAddSub | 添加订阅 |
| POST | `/api/subs/{id}` | apiDelSub | 删除订阅 |
| POST | `/api/fetch` | apiFetch | 触发订阅拉取（goroutine） |
| POST | `/api/node/{id}` | apiSwitch | 切换节点 |
| POST | `/api/ping/{id}` | apiPing | 单节点测速 |
| POST | `/api/pingall` | apiPingAll | 全部节点测速（并发 5） |
| POST | `/api/sort` | apiSort | 保存排序方式（persistent） |
| POST | `/api/proxy/start` | apiStart | 启动代理 |
| POST | `/api/proxy/stop` | apiStop | 停止代理 |
| GET | `/api/settings` | apiGetSettings | 设置读取 |
| POST | `/api/settings` | apiSettings | 保存设置 |

### 3.3 数据持久化

所有数据保存在 `config.json`（默认路径 `/app/data/config.json`，可通过 `DATA_DIR` 环境变量覆盖）：

```json
{
  "webPort": 8000,
  "socksPort": 10808,
  "httpPort": 10810,
  "listenAddr": "0.0.0.0",
  "subRefresh": 300,     // 订阅自动刷新间隔(秒)，0=禁用
  "subProxy": "",        // 订阅代理（解决盒子无法直接访问订阅源）
  "proxyMode": "smart",  // smart/global
  "activeNode": "uuid",  // 当前激活节点 ID
  "sortOrder": "ping_asc", // 排序方式: ""/ping_asc/ping_desc
  "subs": [{ "id":"", "name":"", "url":"" }],
  "nodes": [{ "id":"", "name":"", "server":"", "port":"", "protocol":"",
              "ping":0, "tls":"", "sni":"", "network":"", ... }]
}
```

**关键说明**：
- 节点 `id` 每次 `Fetch()` 重新生成（UUID v4），旧 ID 失效
- 跨 Fetch 周期保留数据通过 `server:port:protocol` 复合 key 匹配
- `ping` 值会保留（含 `-1` 超时标记），不会因订阅刷新退化为 0

---

## 四、已完成的修改（按时间顺序）

### 4.1 基础功能（原始项目）

- 多订阅聚合、vmess/vless/trojan/ss 解析
- SOCKS5(10808) + HTTP(10810) + 透明代理(12345)
- 智能分流（geosite:cn/geoip:cn）+ 全局模式切换
- xray 进程崩溃自动重启（watch goroutine）
- systemd 开机自启支持

### 4.2 节点列表优化（8月14日）

- **独立滚动区域**：节点列表改为固定高度+滚动条（约 20 行可见），不撑满整个页面
- **延迟列独立显示**：延迟单独一列，与节点名称/操作按钮分开
- **排序持久化**：支持按延迟升序/降序排序，排序方式保存到 config.json，重启后保留
- 超时/未测节点排序在最后

### 4.3 断电重启自动连接（8月14日）

- **根因**：`Fetch()` 替换节点时生成新 UUID，`ActiveNode` 指向旧 ID 无法匹配
- **修复**：`Fetch()` 在替换节点时按 `server:port:protocol` 重映射 `ActiveNode` 到新 ID
- 自动启动重试 5 次（失败后 3 秒间隔），避免因 xray 启动竞态导致失败
- **Manager 新增字段**：`activeName` 缓存启动节点名，`Status()` 优先返回缓存

### 4.4 节点去重（8月14日）

- `Fetch()` 后用 `dedupNodes()` 按 `server:port:protocol` 去重，只保留第一个
- 前端已无影响（服务端去重后返回）

### 4.5 测速算法重做（8月16日，两次迭代）

**第一次**：TCP+TLS 握手 + 3 次采样取最快（`probeNode`/`probeOnce`）
- TLS 节点（tls/xtls/reality/trojan）做真实 TLS 握手
- 非 TLS 节点做 TCP connect + 短读
- 超时用红色 "超时" 标注，刷新后持久保留

**第二次**：采样优化为首次成功即返回，仅失败重试一次
- 可达节点 1 次完成，不可达节点最多 2 次
- 批量测速超时从 3.5s 降到 2s
- 前端逐条实时显示 + 进度计数 + setTimeout(0) 让出渲染

**第三次**（8月19日）：容错修复
- 后端 `apiPing`：节点 ID 被替换后返回 `ms:-1` 而非 error
- 前端 `pingAll`：逐节点 `try/catch`，单节点失败不中断整批
- `setPing` 对 undefined/异常结果兜底

### 4.6 状态栏节点名消失（8月16日）

- **根因**：`Status()` 从 `cfg.Nodes` 反查 `activeName`，`Fetch()` 反查失败返回空
- **修复**：`Manager` 增加 `activeName` 缓存字段，`Start()` 启动时记录
- 前端 `#st` 加 `flex:1; overflow:hidden; text-overflow:ellipsis`

### 4.7 手动更新订阅按钮（8月16日）

- 订阅标题旁新增「更新订阅」按钮
- `updateSub()` 函数：触发 `/api/fetch` → 刷新订阅列表 → 刷新节点列表
- 带 "更新中..." 禁用反馈

### 4.8 支持 Xray JSON 订阅解析（8月19日）

- **新增 `parseXrayJSON`**：支持 BPB 面板等 `?app=xray` 格式的 JSON 数组订阅
- **新增 `parseXrayConfig`**：从单份 Xray 配置提取节点（vless/vmess/trojan/ss）
- 支持字段：address/port/id/password/method + ws/grpc/tcp/h2 流设置 + TLS/REALITY SNI
- `fetchOne` 优先尝试 JSON 解析，失败自动回退到原行解析逻辑
- 已用真实 BPB 订阅（105 节点：vless 53 + trojan 52）验证通过

### 4.9 其他修复

- **配置文件损坏容错**：`config.Load()` 中 JSON 解析失败时用默认值启动，不崩溃
- **Save() 直接写盘**：去掉原子写入（`os.Rename`），改为 `os.WriteFile` 直接写入（避免 Windows/Linux 跨文件系统问题）
- **Fetch() 存盘条件**：`len(all) == 0` 时跳过保存，防止空配置覆盖已有数据
- **Manager xrayBin 路径可配置**：`SetXrayBin()` 方法，默认 `/usr/local/bin/xray`
- **超时前端显示**：`ping` 值分级显示（>0 绿色延迟、-1 红色超时、0 空）

---

## 五、测速算法详解

### 5.1 当前的策略

**本机网络直连节点**（TCP connect + 可选 TLS 握手），不是"节点测速到某网络"。

```
probeNode(n, timeout)
  │
  ├── useTLS = (TLS=="tls"|"xtls"|"reality" || protocol=="trojan" || security=="tls"|"reality")
  │
  ├── 第一次 probeOnce
  │     ├── net.DialTimeout("tcp", server:port, timeout)
  │     │     └── 失败 → (-1, false)
  │     ├── (useTLS) tls.Client(conn).Handshake()
  │     │     └── 失败 → (-1, false)
  │     ├── (useTLS) ReadByte() 250ms 超时（验证协议栈活跃）
  │     ├── 非TLS/vless/vmess: Read() 200ms 超时（验证服务器响应）
  │     └── 成功 → (elapsed_ms, true)
  │
  ├── 成功 → 直接返回 (ms, true)
  │
  └── 失败 → 第二次 probeOnce（重试一次防抖动）
        └── 成功/失败 → 返回
```

### 5.2 局限性

- **盒子在墙内时，测速国际节点可能大量超时**（DNS 污染、国际线路 QOS）
- **TCP 通 ≠ 代理协议能工作**（端口通但 vmess 配置错误、TLS 握手失败）
- **域名节点**：`net.DialTimeout` 做 DNS 解析，如果盒子 DNS 被封则失败
- **测速不经过代理**：即使盒子开了透明代理，本机发出的 TCP 连接不走 iptables 透明代理

### 5.3 被放弃的方案

**临时 xray 进程做 HTTP 204 探测**（`ProbeNode`→ `v2ray/probe.go`，已删除）：
- 为每个节点启动独立 xray 进程，通过它的 HTTP 代理请求 `cp.cloudflare.com/generate_204`
- **放弃原因**：低配 arm64 盒子同时启动多个 xray 进程导致全部超时，且进程开销大
- 代码已删除（`internal/v2ray/probe.go` 和 `internal/v2ray/probe_test.go`）

---

## 六、已知问题与待优化

### 6.1 未解决

1. **测速不能真正验证代理可用性**：当前 TCP 方式只能测"端口通不通"，不能测"代理协议能否转发流量"
   - 改进方向：可用性测速 + 延迟测速分离，可用性测速复用 xray 临时实例（但需要解决低配设备性能问题）
2. **断电重启后需手动切换节点**：自动启动有重试机制，但如果在 Fetch 完成前 xray 已启动，可能找不到节点
   - 改进方向：`main.go` 中等待 Fetch 完成后再启动代理
3. **全部测速耗时较长**：104 节点 × 2 次重试 × 2s 超时 / 5 并发 ≈ 40-80s
   - 改进方向：前端改用 `/api/pingall` 一次批量请求，减少 HTTP 往返开销

### 6.2 设计决策说明

| 决策 | 原因 |
|------|------|
| 零外部依赖 | 减少编译/部署复杂度，适合低配设备 |
| 前端单页内联 | 无 npm/node，单二进制部署，移动端友好 |
| 节点 UUID 每次 Fetch 重新生成 | 避免节点 ID 膨胀，订阅更新自动清理过期节点 |
| 跨周期匹配用 server:port:protocol | 不依赖 UUID，订阅更新后节点仍能匹配 |
| 测速结果持久化到 config.json | 简单可靠，无额外文件，跨重启不丢失 |
| 非原子写入（去掉 os.Rename） | 解决跨文件系统问题，简化错误处理 |

---

## 七、构建与部署

### 7.1 本地编译

```bash
# Linux arm64（电视盒子）
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -ldflags="-s -w" -o v2aynn-web ./cmd/server/

# Linux amd64
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -ldflags="-s -w" -o v2aynn-web ./cmd/server/

# Windows（本地测试）
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -ldflags="-s -w" -o v2aynn-web.exe ./cmd/server/
```

### 7.2 部署到盒子

```bash
cd /opt/v2aynn-web
systemctl stop v2aynn-web
curl -o v2aynn-web.new http://192.168.5.90:9999/pkg/v2aynn-web
mv v2aynn-web.new v2aynn-web && chmod +x v2aynn-web
systemctl start v2aynn-web
```

### 7.3 首次部署

```bash
bash download-assets.sh   # 下载 xray + geo 数据（约 60MB）
bash start.sh             # 一键启动
```

### 7.4 systemd 服务

```ini
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

---

## 八、提交历史速查

```
2ff3b6a 更新 README 更新记录 (2026-08-19)
ce71d3c 修复测速信息丢失问题
2afd527 支持 Xray JSON 订阅解析 (BPB 面板 ?app=xray)
3775f04 优化测速速度与实时显示 + README 更新说明
da2b24b 修复测速全部超时 + 新增手动更新订阅按钮
e32c64d 修复长期运行三问题：状态栏、测速算法、排序持久化
728d91a Update README.md (网页端编辑)
309f859 v2aynn-web: 低资源Linux设备代理订阅管理器 (AI编程生成)
```

---

## 九、关键代码位置速查

| 功能 | 文件 | 行号(大约) |
|------|------|-----------|
| 路由注册 | `internal/web/server.go` | 33-53 |
| 节点列表排序 | `internal/web/server.go` | 72-89 |
| 排序持久化 | `internal/web/server.go` | 91-109 |
| 单节点测速 | `internal/web/server.go` | 192-222 |
| 全部测速 | `internal/web/server.go` | 224-277 |
| TCP+TLS 探测核心 | `internal/web/server.go` | 356-424 |
| 订阅拉取解析 | `internal/subscription/parser.go` | 101-131 |
| Xray JSON 解析 | `internal/subscription/parser.go` | 391-505 |
| 节点去重 | `internal/subscription/parser.go` | 55-65 |
| xray 进程管理 | `internal/v2ray/manager.go` | 37-77 |
| xray 崩溃自愈 | `internal/v2ray/manager.go` | 79-94 |
| 配置加载/保存 | `internal/config/config.go` | 54-82 |
| 状态栏 | `internal/web/static/index.html` | 45 |
| 节点列表渲染 | `internal/web/static/index.html` | 73 |
| 全部测速前端 | `internal/web/static/index.html` | 86 |
| 更新订阅 | `internal/web/static/index.html` | 77 |
| 自动启动 | `cmd/server/main.go` | 36-52 |

---

## 十、测试

```bash
# 运行全部测速单测
go test ./internal/web/ -run TestProbe -v

# 测试用例：
#   TestProbeReachableTCP  - TCP 可达节点（本地模拟服务器）
#   TestProbeReachableTLS  - TLS 可达节点（本地自签证书）
#   TestProbeUnreachable   - 不可达节点（端口 1）
#   TestProbeTLSMismatch   - TLS 握手失败（明文服务器返回乱码）

# 全部包编译
go build ./...

# 全部包 vet
go vet ./internal/... ./cmd/...
```

---

## 附：常见问题排查

### Q: 盒子显示"无节点"
1. 检查订阅 URL 是否填对
2. 设置「订阅代理」（如果盒子不能直接访问订阅源）
3. 查看日志：`journalctl -u v2aynn-web --no-pager -n 30 | grep fetch`
4. 若日志显示 "Xray JSON 订阅" 则解析成功，否则检查网络

### Q: 测速全部超时
1. 确认盒子网络能直连节点（测速不走代理）
2. 检查节点 server 域名是否能解析：`ping 节点域名`
3. 在网页「设置」里把「刷新间隔」改大或设为 0 禁用自动刷新

### Q: 断电重启后代理不自动运行
1. 检查 `systemctl status v2aynn-web` 是否 active
2. 查看日志是否有 "自动启动代理失败" 及重试次数
3. 当前有 5 次重试，每次间隔 3 秒

### Q: 测速信息丢失/卡"测速中"
1. 检查前端是否因为单节点请求失败导致整批中断（已在 8月19日 修复）
2. 若仍出现，检查浏览器开发者工具的网络请求是否有失败的 `/api/ping/{id}`