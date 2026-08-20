package v2ray

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"sync"
	"time"

	"v2aynn-web/internal/config"
)

type Manager struct {
	mu           sync.Mutex
	cfg          *config.Config
	dataDir      string
	xrayBin      string
	cmd          *exec.Cmd
	running      bool
	activeName   string // 缓存实际启动节点的名称，避免依赖 cfg.Nodes 反查
	activeNodeID string

	restartCount    int       // 连续重启次数
	lastRestart     time.Time // 上次重启时间
	restartMu       sync.Mutex
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
	if m.cfg.ActiveNode == "" {
		return fmt.Errorf("未设置当前节点")
	}

	var node *config.Node
	m.cfg.Lock()
	for i := range m.cfg.Nodes {
		if m.cfg.Nodes[i].ID == m.cfg.ActiveNode {
			node = &m.cfg.Nodes[i]
			break
		}
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
	if !m.running && isCurrent {
		// 防无限重启：3次内如果始终撑不过30秒，判定为配置问题，放弃
		m.restartMu.Lock()
		now := time.Now()
		if now.Sub(m.lastRestart) > 30*time.Second {
			m.restartCount = 0 // 上次重启已过30s，重置计数
		}
		m.restartCount++
		m.lastRestart = now
		if m.restartCount > 3 {
			log.Printf("xray 连续重启 %d 次失败，放弃自动重启，请检查节点配置", m.restartCount)
			m.restartMu.Unlock()
			return
		}
		restartCount := m.restartCount
		m.restartMu.Unlock()

		log.Printf("xray 异常退出 (%d/3)，5秒后自动重启", restartCount)
		time.Sleep(5 * time.Second)
		_ = m.Start()
	}
}

func (m *Manager) Stop() {
	m.mu.Lock()
	defer m.mu.Unlock()
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
	for _, n := range m.cfg.Nodes {
		if n.ID == id {
			found = true
			break
		}
	}
	if !found {
		m.cfg.Unlock()
		return fmt.Errorf("节点(%s)不在节点列表中", id)
	}
	m.cfg.ActiveNode = id
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
		for _, n := range m.cfg.Nodes {
			if n.ID == m.cfg.ActiveNode {
				activeName = n.Name
				break
			}
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
	// 智能分流: 国内域名和IP直连, 其余走代理
	if proxyMode != "global" {
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