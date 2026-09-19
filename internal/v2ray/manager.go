package v2ray

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"sort"
	"sync"
	"time"

	"v2aynn-web/internal/config"
)

type Manager struct {
	mu      sync.Mutex
	cfg     *config.Config
	dataDir string
	xrayBin string
	cmd     *exec.Cmd
	running bool
	// stopRequested 记录「有人显式要求停止」，由 Stop() 置位、Start() 清除。
	//
	// 为什么需要它：进程退出后 watch 会把 running 置回 false，而 Stop() 同样把
	// running 置为 false —— 两种状态无法区分。只凭 running/cmd 判断的话，
	// 用户在 5 秒自愈窗口内点「停止」，xray 仍会被 watch 自己拉起来，
	// 停止功能形同虚设。有这个标记才能区分「进程崩了，该自愈」与「用户要停，别自愈」。
	stopRequested bool
	activeName    string // 缓存实际启动节点的名称，避免依赖 cfg.Nodes 反查
	activeNodeID  string

	restartCount int       // 连续重启次数
	lastRestart  time.Time // 上次重启时间
	restartMu    sync.Mutex
}

func NewManager(dataDir string) *Manager {
	return &Manager{dataDir: dataDir, xrayBin: "/usr/local/bin/xray"}
}

// SetXrayBin 覆盖 xray 二进制路径（测试或异常环境下使用）
func (m *Manager) SetXrayBin(p string) { m.xrayBin = p }

func (m *Manager) SetConfig(cfg *config.Config) {
	m.cfg = cfg
}

func (m *Manager) IsRunning() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.running
}

func (m *Manager) Start() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.running {
		return nil
	}
	// 这是一次显式启动，清除「已要求停止」标记，允许后续崩溃重新自愈
	m.stopRequested = false
	if m.cfg.ActiveNode == "" {
		return fmt.Errorf("未设置当前节点")
	}

	var node *config.Node
	m.cfg.Lock()
	if n, _ := m.cfg.FindNode(m.cfg.ActiveNode); n != nil {
		node = n
	}
	m.cfg.Unlock()
	if node == nil {
		return fmt.Errorf("激活节点(%s)不在节点列表中", m.cfg.ActiveNode)
	}
	// 缓存实际启动节点信息，供 Status 状态栏显示，避免受 Fetch 后台刷新影响
	m.activeName = node.Name
	m.activeNodeID = node.ID

	v2cfg := generateConfig(node, m.cfg.SocksPort, m.cfg.HttpPort, m.cfg.ProxyMode)
	cfgPath := m.dataDir + "/v2ray.json"
	b, err := json.MarshalIndent(v2cfg, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(cfgPath, b, 0644); err != nil {
		return err
	}

	m.cmd = exec.Command(m.xrayBin, "-config", cfgPath)
	if err := m.cmd.Start(); err != nil {
		return err
	}
	m.running = true
	// 启动成功，重置重启计数
	m.restartMu.Lock()
	m.restartCount = 0
	m.lastRestart = time.Time{}
	m.restartMu.Unlock()
	go m.watch(m.cmd)
	return nil
}

// watch 监控 xray 进程，异常退出时自动重启（最多3次，超过则放弃）
func (m *Manager) watch(cmd *exec.Cmd) {
	_ = cmd.Wait()
	m.mu.Lock()
	isCurrent := m.cmd == cmd
	if isCurrent {
		m.running = false
		m.cmd = nil
	}
	m.mu.Unlock()
	// 只有"退出的正是当前进程"才进入自愈流程。Stop()/SwitchNode() 会把 m.cmd 置 nil，
	// 此时 isCurrent 为 false，说明这次退出是主动停机的结果，不应重启。
	if !isCurrent {
		return
	}

	// 防无限重启：3次内如果始终撑不过30秒，判定为配置问题，放弃
	m.restartMu.Lock()
	now := time.Now()
	if now.Sub(m.lastRestart) > 30*time.Second {
		m.restartCount = 0 // 上次重启已过30s，重置计数
	}
	m.restartCount++
	m.lastRestart = now
	restartCount := m.restartCount
	// 统一在此处释放 restartMu，不要挪到分支内部：
	// 下面的 tryFailover → SwitchNode 内部会再次获取它，持锁进入必然死锁。
	// 原实现是在放弃分支里手工 Unlock，这种写法在后续改动中很容易被漏掉。
	m.restartMu.Unlock()

	if restartCount > 3 {
		// 当前节点已判定不可用，先尝试自动故障转移，而不是直接放弃
		if m.tryFailover() {
			return
		}
		log.Printf("xray 连续重启 %d 次失败，放弃自动重启，请检查节点配置", restartCount)
		return
	}

	log.Printf("xray 异常退出 (%d/3)，5秒后自动重启", restartCount)
	time.Sleep(5 * time.Second)

	// 临重启前在锁内确认此刻到底该不该自愈。三点要点：
	//
	//  1. 原实现在 m.mu.Unlock() 之后直接读 `!m.running`，而 m.running 由
	//     Start()/Stop() 在锁内并发写入 —— 无锁读 + 锁内写 = 数据竞争。
	//     目标平台是 ARM64（弱内存模型），不能依赖这种读法的偶然正确性。
	//     m.running / m.cmd / m.stopRequested 的一切访问都必须在 m.mu 内进行。
	//
	//  2. 判定时机要尽量靠后。并发的显式 Start()（用户点启动、改端口、恢复配置、
	//     开机重试）若发生在判定之后就会被漏看，于是把"用户有意重启"误记成
	//     "节点故障"；重启计数累积到 4 次还会触发一次不必要的自动故障转移。
	//     放到 5 秒等待之后再判定，能观察到的窗口最大。
	//
	//  3. 必须显式判断 stopRequested。仅凭 running/cmd 无法区分"进程崩了"与
	//     "用户点了停止"—— 两者都是 running=false、cmd=nil。原实现在 5 秒自愈
	//     窗口内点停止后 xray 仍会被拉起，停止功能形同虚设。
	m.mu.Lock()
	stopped := m.stopRequested
	alreadyRunning := m.running || m.cmd != nil
	m.mu.Unlock()
	if stopped {
		log.Printf("已收到停止指令，取消本次自愈重启")
		return
	}
	if alreadyRunning {
		log.Printf("xray 已由其他操作启动，跳过本次自愈")
		return
	}

	_ = m.Start()
}

// tryFailover 当前节点连续失败后，自动切换到其他可用节点。
// 优先选择已测速可达（ping>0）且延迟最低的节点，最多尝试 3 个。
// 返回 true 表示已成功切换到新节点。
func (m *Manager) tryFailover() bool {
	if !m.cfg.FailoverEnabled() {
		return false
	}

	m.cfg.Lock()
	cur := m.cfg.ActiveNode
	var cands []config.Node
	for _, n := range m.cfg.AllNodes() {
		if n.ID != cur {
			cands = append(cands, n)
		}
	}
	m.cfg.Unlock()

	if len(cands) == 0 {
		return false
	}
	// 可达节点优先，其次按延迟升序
	sort.SliceStable(cands, func(i, j int) bool {
		pi, pj := cands[i].Ping, cands[j].Ping
		vi, vj := pi > 0, pj > 0
		if vi != vj {
			return vi
		}
		if !vi {
			return false
		}
		return pi < pj
	})

	const maxTry = 3
	tried := 0
	for i := 0; i < len(cands) && i < maxTry; i++ {
		tried++
		log.Printf("自动故障转移: 尝试切换到[%s]", cands[i].Name)
		if err := m.SwitchNode(cands[i].ID); err == nil {
			log.Printf("自动故障转移成功 -> [%s]", cands[i].Name)
			return true
		}
		time.Sleep(2 * time.Second)
	}
	log.Printf("自动故障转移失败: 已尝试 %d 个节点均未成功", tried)
	return false
}

func (m *Manager) Stop() {
	m.mu.Lock()
	defer m.mu.Unlock()
	// 必须先置位再置 running=false：watch 会用这个标记区分
	// 「进程崩了该自愈」与「用户点了停止不该自愈」。否则在 5 秒自愈窗口内点停止，
	// xray 会被自己拉起来（原缺陷，见 TestStopDoesNotTriggerRestart）。
	m.stopRequested = true
	if m.cmd != nil && m.cmd.Process != nil {
		m.cmd.Process.Kill()
		// 不在此处 Wait，由 watch goroutine 收尸，避免重复 Wait panic
	}
	m.running = false
	m.cmd = nil
}

func (m *Manager) SwitchNode(id string) error {
	// 先确认节点存在，避免把 ActiveNode 设置为已不存在的 ID（订阅刷新竞态下会报"不在节点列表中"）
	m.cfg.Lock()
	found := false
	var grpID string
	if _, grpID = m.cfg.FindNode(id); grpID != "" {
		found = true
	}
	if !found {
		m.cfg.Unlock()
		return fmt.Errorf("节点(%s)不在节点列表中", id)
	}
	m.cfg.ActiveNode = id
	m.cfg.ActiveGrp = grpID
	_ = m.cfg.Save()
	m.cfg.Unlock()
	// 重置重启计数
	m.restartMu.Lock()
	m.restartCount = 0
	m.lastRestart = time.Time{}
	m.restartMu.Unlock()
	m.Stop()
	time.Sleep(300 * time.Millisecond)
	return m.Start()
}

func (m *Manager) Status() map[string]interface{} {
	m.mu.Lock()
	defer m.mu.Unlock()
	// 优先用缓存的启动节点名；兜底再反查 cfg.Nodes
	activeName := m.activeName
	if activeName == "" {
		m.cfg.Lock()
		if n, _ := m.cfg.FindNode(m.cfg.ActiveNode); n != nil {
			activeName = n.Name
		}
		m.cfg.Unlock()
	}
	return map[string]interface{}{
		"running":    m.running,
		"activeNode": m.cfg.ActiveNode,
		"activeName": activeName,
	}
}

func generateConfig(n *config.Node, socksPort, httpPort int, proxyMode string) map[string]interface{} {
	cfg := map[string]interface{}{
		"log": map[string]interface{}{"loglevel": "warning"},
		"dns": map[string]interface{}{
			"servers": []interface{}{
				"1.1.1.1",
				"8.8.8.8",
				"localhost",
			},
		},
		"inbounds": []interface{}{
			map[string]interface{}{
				"port": socksPort, "listen": "0.0.0.0", "protocol": "socks",
				"settings": map[string]interface{}{"auth": "noauth", "udp": true},
				"sniffing": map[string]interface{}{"enabled": true},
			},
			map[string]interface{}{
				"port": httpPort, "listen": "0.0.0.0", "protocol": "http",
				"settings": map[string]interface{}{"auth": "noauth"},
				"sniffing": map[string]interface{}{"enabled": true},
			},
			map[string]interface{}{
				"port": 12345, "listen": "0.0.0.0", "protocol": "dokodemo-door",
				"settings": map[string]interface{}{"network": "tcp,udp", "followRedirect": true},
				"sniffing": map[string]interface{}{"enabled": true, "destOverride": []string{"http", "tls", "quic"}},
			},
		},
		"outbounds": []interface{}{
			generateOutbound(n),
			map[string]interface{}{"protocol": "freedom", "tag": "direct"},
		},
	}
	// 分流模式必须显式区分。历史上这里只判断 `proxyMode != "global"`，
	// 导致 direct 落进与 smart 相同的分支 —— 界面选「直连」实际仍在按国内/国外分流，
	// 是个静默的空操作。三种模式现在各有独立分支：
	//
	//   smart  —— 国内域名/IP 直连，其余走代理
	//   global —— 全部走代理（不加 routing，xray 默认使用第一个出站）
	//   direct —— 全部直连（所有流量指向 freedom 出站，即真直连）
	//
	// 未知取值回落到 smart（与 web 层白名单校验互为兜底，防手工改配置文件写错）。
	switch proxyMode {
	case "global":
		// 不加 routing 规则：xray 默认把所有流量交给第一个出站（代理节点）
	case "direct":
		// 兜底规则：不带任何匹配条件的 field 规则会命中全部流量。
		// 只带 network 是为了同时覆盖 TCP 与 UDP，避免只直连 TCP 而 UDP 仍走代理。
		cfg["routing"] = map[string]interface{}{
			"rules": []interface{}{
				map[string]interface{}{
					"type": "field", "outboundTag": "direct",
					"network": "tcp,udp",
				},
			},
		}
	default: // smart
		// 智能分流: 国内域名和IP直连, 其余走代理
		cfg["routing"] = map[string]interface{}{
			"rules": []interface{}{
				map[string]interface{}{
					"type": "field", "outboundTag": "direct",
					"domain": []interface{}{"geosite:cn"},
				},
				map[string]interface{}{
					"type": "field", "outboundTag": "direct",
					"ip": []interface{}{"geoip:cn", "geoip:private"},
				},
			},
		}
	}
	return cfg
}

func generateOutbound(n *config.Node) map[string]interface{} {
	out := map[string]interface{}{
		"protocol": n.Protocol,
		"settings": map[string]interface{}{},
	}
	s := out["settings"].(map[string]interface{})

	switch n.Protocol {
	case "vmess":
		sec := "auto"
		if n.Security != "" {
			sec = n.Security
		}
		s["vnext"] = []interface{}{
			map[string]interface{}{
				"address": n.Server, "port": toInt(n.Port, 443),
				"users": []interface{}{
					map[string]interface{}{
						"id": n.UUID, "alterId": toInt(n.AlterID, 0), "security": sec,
					},
				},
			},
		}
	case "vless":
		enc := "none"
		if n.Security != "" {
			enc = n.Security
		}
		s["vnext"] = []interface{}{
			map[string]interface{}{
				"address": n.Server, "port": toInt(n.Port, 443),
				"users": []interface{}{
					map[string]interface{}{
						"id": n.UUID, "encryption": enc,
					},
				},
			},
		}
	case "trojan":
		s["servers"] = []interface{}{
			map[string]interface{}{
				"address": n.Server, "port": toInt(n.Port, 443),
				"password": n.Password,
			},
		}
	case "ss":
		method := "aes-256-gcm"
		if n.Method != "" {
			method = n.Method
		}
		s["servers"] = []interface{}{
			map[string]interface{}{
				"address": n.Server, "port": toInt(n.Port, 8388),
				"method": method, "password": n.Password,
			},
		}
	}

	net := "tcp"
	if n.Network != "" {
		net = n.Network
	}
	tlsSec := "none"
	if n.TLS == "tls" || n.TLS == "xtls" {
		tlsSec = n.TLS
	}
	if n.SNI != "" && tlsSec == "none" {
		tlsSec = "tls"
	}

	stream := map[string]interface{}{"network": net, "security": tlsSec}
	if tlsSec != "none" {
		tls := map[string]interface{}{}
		if n.SNI != "" {
			tls["serverName"] = n.SNI
		}
		stream["tlsSettings"] = tls
	}
	if n.HeaderType != "" {
		h := map[string]interface{}{"type": n.HeaderType}
		if n.RequestHost != "" {
			h["request"] = map[string]interface{}{
				"headers": map[string]interface{}{
					"Host": []interface{}{n.RequestHost},
				},
			}
		}
		stream["tcpSettings"] = map[string]interface{}{"header": h}
	}
	if n.Path != "" && net == "ws" {
		ws := map[string]interface{}{"path": n.Path}
		if n.RequestHost != "" {
			ws["headers"] = map[string]interface{}{"Host": n.RequestHost}
		}
		stream["wsSettings"] = ws
	}

	out["streamSettings"] = stream
	return out
}

func toInt(s string, def int) int {
	n, ok := 0, false
	for _, c := range s {
		if c >= '0' && c <= '9' {
			n = n*10 + int(c-'0')
			ok = true
		}
	}
	if !ok {
		return def
	}
	return n
}
