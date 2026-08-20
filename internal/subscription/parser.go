package subscription

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"v2aynn-web/internal/config"
)

func Poll(cfg *config.Config) {
	if cfg.SubRefresh <= 0 {
		log.Printf("Poll: 自动更新已禁用(SubRefresh=%d)", cfg.SubRefresh)
		return
	}
	ticker := time.NewTicker(time.Duration(cfg.SubRefresh) * time.Second)
	for range ticker.C {
		Fetch(cfg)
	}
}

func Fetch(cfg *config.Config) {
	log.Printf("Fetch: 开始拉取订阅")

	var all []config.Node
	cfg.Lock()
	subs := make([]config.Sub, len(cfg.Subs))
	copy(subs, cfg.Subs)
	oldNodes := make([]config.Node, len(cfg.Nodes))
	copy(oldNodes, cfg.Nodes)
	cfg.Unlock()

	// 快照当前节点，按订阅分组：某订阅拉取失败时保留其旧节点，避免整表清空
	// 注意：手动节点（SubID=""）不在此处理，改在写回时从【实时】cfg.Nodes 合并，
	// 防止 Fetch 快照早于导入导致新导入节点被覆盖丢失（见下方锁内合并逻辑）
	oldBySub := make(map[string][]config.Node)
	for _, n := range oldNodes {
		if n.SubID != "" {
			oldBySub[n.SubID] = append(oldBySub[n.SubID], n)
		}
	}

	for _, sub := range subs {
		nodes, err := fetchOne(sub, cfg.SubProxy)
		if err != nil {
			// 拉取失败：保留该订阅原有节点，防止其节点被清空
			log.Printf("Fetch: 订阅[%s]拉取失败，保留旧节点: %v", sub.Name, err)
			if old, ok := oldBySub[sub.ID]; ok {
				all = append(all, old...)
			}
			continue
		} else {
			log.Printf("Fetch: 订阅[%s]拉取到%d个节点", sub.Name, len(nodes))
		}
		all = append(all, nodes...)
	}
	log.Printf("Fetch: 共拉取到%d个节点(去重前)", len(all))

	// 去重: 按 server:port:protocol 只保留第一个
	all = dedupNodes(all)
	log.Printf("Fetch: 去重后%d个节点", len(all))

	// 没拉到节点时跳过保存，防止覆盖已有数据
	if len(all) == 0 {
		log.Printf("Fetch: 未拉到节点，跳过保存")
		return
	}

	cfg.Lock()
	// 手动节点(SubID="")永不随订阅刷新丢弃：
	// 在【锁内】读取实时 cfg.Nodes 合并（而非 Fetch 开始时的快照），
	// 确保 Fetch 进行期间新导入/添加的节点也不会被覆盖丢失。
	// 手动节点排在最前，去重时优先于同 server:port:protocol 的订阅节点。
	var manual []config.Node
	seenM := make(map[string]bool)
	for _, n := range cfg.Nodes {
		if n.SubID == "" {
			key := n.Server + ":" + n.Port + ":" + n.Protocol
			if !seenM[key] {
				seenM[key] = true
				manual = append(manual, n)
			}
		}
	}
	all = append(manual, all...)
	all = dedupNodes(all)
	if len(manual) > 0 {
		log.Printf("Fetch: 合并%d个手动节点, 去重后共%d个节点", len(manual), len(all))
	}

	// Preserve ping data and find old ActiveNode key
	// 注意：保留所有非零 ping（含 -1 超时标记），否则 fetch 刷新后超时节点会退化为"未测"，导致排序变动
	oldPings := make(map[string]int)
	oldActiveKey := ""
	for _, n := range cfg.Nodes {
		key := n.Server + ":" + n.Port + ":" + n.Protocol
		if n.Ping != 0 {
			oldPings[key] = n.Ping
		}
		if n.ID == cfg.ActiveNode {
			oldActiveKey = key
		}
	}
	for i := range all {
		key := all[i].Server + ":" + all[i].Port + ":" + all[i].Protocol
		if p, ok := oldPings[key]; ok {
			all[i].Ping = p
		}
	}
	cfg.Nodes = all

	// Remap ActiveNode by server:port:protocol key (Fix Bug 2)
	if oldActiveKey != "" {
		found := false
		for _, n := range all {
			key := n.Server + ":" + n.Port + ":" + n.Protocol
			if key == oldActiveKey {
				cfg.ActiveNode = n.ID
				found = true
				break
			}
		}
		if !found {
			cfg.ActiveNode = ""
		}
	}

	_ = cfg.Save()
	cfg.Unlock()
}

func fetchOne(sub config.Sub, proxyURL string) ([]config.Node, error) {
	transport := &http.Transport{}
	if proxyURL != "" {
		if p, err := url.Parse(proxyURL); err == nil {
			transport.Proxy = http.ProxyURL(p)
		}
	}
	client := &http.Client{Timeout: 15 * time.Second, Transport: transport}
	resp, err := client.Get(sub.URL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	raw := string(body)
	previewLen := len(raw)
	if previewLen > 100 {
		previewLen = 100
	}
	log.Printf("fetchOne[%s]: 原始响应前%d字节: %q", sub.Name, previewLen, raw[:previewLen])

	// 支持 Xray JSON 订阅 (如 BPB 面板的 ?app=xray):
	// 在原有链接列表逻辑之前优先尝试 JSON 解析。非 JSON 订阅返回 false，自动回退到原有逻辑，不影响其他订阅源。
	if jnodes, ok := parseXrayJSON(raw, sub.ID); ok {
		log.Printf("fetchOne[%s]: Xray JSON 订阅, 解析出 %d 个节点", sub.Name, len(jnodes))
		return jnodes, nil
	}

	var nodes []config.Node
	lines := strings.Split(strings.TrimSpace(raw), "\n")
	log.Printf("fetchOne[%s]: 共%d行", sub.Name, len(lines))

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// Try parsing directly first
		if n, err := parse(line, sub.ID); err == nil {
			nodes = append(nodes, n)
			continue
		}
		// Try base64 decode per line (common subscription format)
		decoded := line
		decodedOK := false
		if b, err := base64.StdEncoding.DecodeString(line); err == nil {
			decoded = string(b)
			decodedOK = true
		} else {
			padded := line
			if mod := len(padded) % 4; mod != 0 {
				padded += strings.Repeat("=", 4-mod)
			}
			if b, err := base64.StdEncoding.DecodeString(padded); err == nil {
				decoded = string(b)
				decodedOK = true
			}
		}
		if !decodedOK {
			log.Printf("fetchOne[%s]: base64解码失败, 行前80字节: %q", sub.Name, line[:min(len(line), 80)])
			continue
		}
		// Try split by newlines (decoded might be multi-line)
		for _, subline := range strings.Split(decoded, "\n") {
			subline = strings.TrimSpace(subline)
			if subline == "" {
				continue
			}
			if n, err := parse(subline, sub.ID); err == nil {
				nodes = append(nodes, n)
			} else {
				log.Printf("fetchOne[%s]: 解析行失败: %v, 前80字节: %q", sub.Name, err, subline[:min(len(subline), 80)])
			}
		}
	}
	return nodes, nil
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// dedupNodes 按 server:port:protocol 去重，保持第一个出现的顺序
func dedupNodes(nodes []config.Node) []config.Node {
	seen := make(map[string]bool, len(nodes))
	out := make([]config.Node, 0, len(nodes))
	for _, n := range nodes {
		key := n.Server + ":" + n.Port + ":" + n.Protocol
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, n)
	}
	return out
}

func parse(raw, subID string) (config.Node, error) {
	raw = strings.TrimSpace(raw)
	n := config.Node{
		ID: config.NewUUID(), SubID: subID,
		RawLink: raw, LastSeen: time.Now().Format("2006-01-02 15:04:05"),
	}
	switch {
	case strings.HasPrefix(raw, "vmess://"):
		return parseVmess(raw, n)
	case strings.HasPrefix(raw, "vless://"):
		return parseVless(raw, n)
	case strings.HasPrefix(raw, "trojan://"):
		return parseTrojan(raw, n)
	case strings.HasPrefix(raw, "ss://"):
		return parseSS(raw, n)
	default:
		return n, fmt.Errorf("unsupported")
	}
}

// Parse 从单个节点URL解析出一个Node。空subID=手动添加。
// 支持 vless://, vmess://, trojan://, ss:// 四种协议。
func Parse(raw string) (config.Node, error) {
	return parse(raw, "")
}

func parseVmess(link string, n config.Node) (config.Node, error) {
	s := strings.TrimPrefix(link, "vmess://")
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		s += strings.Repeat("=", 4-len(s)%4)
		b, err = base64.StdEncoding.DecodeString(s)
		if err != nil {
			return n, err
		}
	}
	var vm struct {
		Add      string `json:"add"`
		Port     string `json:"port"`
		ID       string `json:"id"`
		Type     string `json:"type"`
		Net      string `json:"net"`
		Path     string `json:"path"`
		Host     string `json:"host"`
		Ps       string `json:"ps"`
		TLS      string `json:"tls"`
		SNI      string `json:"sni"`
		Security string `json:"security"`
		AlterID  string `json:"alterId"`
	}
	if err := json.Unmarshal(b, &vm); err != nil {
		return n, err
	}
	n.Protocol = "vmess"
	n.Server = vm.Add
	n.Port = vm.Port
	n.UUID = vm.ID
	n.Network = vm.Net
	n.TLS = vm.TLS
	n.SNI = vm.SNI
	n.Security = vm.Security
	n.AlterID = vm.AlterID
	n.RequestHost = vm.Host
	n.Path = vm.Path
	n.HeaderType = vm.Type
	if vm.Ps != "" {
		n.Name = vm.Ps
	}
	if n.Name == "" {
		n.Name = n.Server + ":" + n.Port
	}
	return n, nil
}

func parseVless(link string, n config.Node) (config.Node, error) {
	u, err := url.Parse(link)
	if err != nil {
		return n, err
	}
	n.Protocol = "vless"
	n.Server = u.Hostname()
	n.Port = u.Port()
	if n.Port == "" {
		n.Port = "443"
	}
	n.UUID = u.User.Username()
	p := u.Query()
	if v := p.Get("type"); v != "" {
		n.Network = v
	}
	if v := p.Get("security"); v != "" {
		n.TLS = v
	}
	if v := p.Get("sni"); v != "" {
		n.SNI = v
	}
	if v := p.Get("path"); v != "" {
		n.Path = v
	}
	if v := p.Get("host"); v != "" {
		n.RequestHost = v
	}
	if v := p.Get("headerType"); v != "" {
		n.HeaderType = v
	}
	if u.Fragment != "" {
		n.Name = u.Fragment
	}
	if n.Name == "" {
		n.Name = n.Server + ":" + n.Port
	}
	return n, nil
}

func parseTrojan(link string, n config.Node) (config.Node, error) {
	u, err := url.Parse(link)
	if err != nil {
		return n, err
	}
	n.Protocol = "trojan"
	n.Server = u.Hostname()
	n.Port = u.Port()
	if n.Port == "" {
		n.Port = "443"
	}
	n.Password = u.User.Username()
	n.TLS = "tls"
	p := u.Query()
	if v := p.Get("type"); v != "" {
		n.Network = v
	}
	if v := p.Get("sni"); v != "" {
		n.SNI = v
	}
	if v := p.Get("path"); v != "" {
		n.Path = v
	}
	if v := p.Get("host"); v != "" {
		n.RequestHost = v
	}
	if v := p.Get("headerType"); v != "" {
		n.HeaderType = v
	}
	if u.Fragment != "" {
		n.Name = u.Fragment
	}
	if n.Name == "" {
		n.Name = n.Server + ":" + n.Port
	}
	return n, nil
}

func parseSS(link string, n config.Node) (config.Node, error) {
	s := strings.TrimPrefix(link, "ss://")
	n.Protocol = "ss"
	n.Network = "tcp"
	if idx := strings.Index(s, "@"); idx > 0 {
		cred := s[:idx]
		host := s[idx+1:]
		if uidx := strings.Index(cred, ":"); uidx > 0 {
			n.Method = cred[:uidx]
			n.Password = cred[uidx+1:]
		}
		if hidx := strings.LastIndex(host, ":"); hidx > 0 {
			n.Server = host[:hidx]
			n.Port = host[hidx+1:]
		}
	} else {
		b, err := base64.StdEncoding.DecodeString(s)
		if err != nil {
			return n, err
		}
		parts := strings.SplitN(string(b), "@", 2)
		if len(parts) == 2 {
			cred := strings.SplitN(parts[0], ":", 2)
			if len(cred) == 2 {
				n.Method = cred[0]
				n.Password = cred[1]
			}
			host := parts[1]
			if hidx := strings.LastIndex(host, ":"); hidx > 0 {
				n.Server = host[:hidx]
				n.Port = host[hidx+1:]
			}
		}
	}
	if n.Name == "" {
		n.Name = n.Server + ":" + n.Port
	}
	return n, nil
}

// parseXrayJSON 解析 Xray 原生 JSON 订阅 (如 BPB 面板的 ?app=xray)。
// 支持两种形态: JSON 数组(每元素一份完整配置) 或 单个 JSON 对象(部分端点返回整段 base64 的单份配置)。
// 返回 (nodes, true) 表示已按 JSON 处理; (_, false) 表示不是 JSON 订阅，调用方应回退到原有链接列表逻辑。
func parseXrayJSON(raw, subID string) ([]config.Node, bool) {
	trimmed := strings.TrimSpace(raw)
	var data interface{}
	if err := json.Unmarshal([]byte(trimmed), &data); err != nil {
		// 整段 base64 解码后再试 (部分 BPB 端点返回 base64 单份配置)
		dec, derr := base64.StdEncoding.DecodeString(trimmed)
		if derr != nil {
			pad := trimmed
			if mod := len(pad) % 4; mod != 0 {
				pad += strings.Repeat("=", 4-mod)
			}
			dec, derr = base64.StdEncoding.DecodeString(pad)
		}
		if derr != nil {
			return nil, false
		}
		if err := json.Unmarshal(dec, &data); err != nil {
			return nil, false
		}
	}

	var configs []map[string]interface{}
	switch v := data.(type) {
	case []interface{}:
		for _, e := range v {
			if m, ok := e.(map[string]interface{}); ok {
				configs = append(configs, m)
			}
		}
	case map[string]interface{}:
		configs = append(configs, v)
	default:
		return nil, false
	}
	if len(configs) == 0 {
		return nil, false
	}

	nodes := make([]config.Node, 0, len(configs))
	for _, cfg := range configs {
		if n, ok := parseXrayConfig(cfg, subID); ok {
			nodes = append(nodes, n)
		}
	}
	if len(nodes) == 0 {
		return nil, false
	}
	return nodes, true
}

// parseXrayConfig 从单份 Xray 配置对象中提取一个代理节点。
func parseXrayConfig(cfg map[string]interface{}, subID string) (config.Node, bool) {
	outs, ok := cfg["outbounds"].([]interface{})
	if !ok || len(outs) == 0 {
		return config.Node{}, false
	}
	// 优先选 tag="proxy" 的出站（BPB 配置通常有 proxy/dns/freedom 三个）
	// 没有 proxy tag 再选第一个受支持的协议
	var ob map[string]interface{}
	for _, o := range outs {
		om, ok := o.(map[string]interface{})
		if !ok {
			continue
		}
		// 先找 proxy tag
		if tag, ok := om["tag"].(string); ok && tag == "proxy" {
			ob = om
			break
		}
	}
	// fallback: 取第一个受支持的协议出站
	if ob == nil {
		for _, o := range outs {
			om, ok := o.(map[string]interface{})
			if !ok {
				continue
			}
			if proto, _ := om["protocol"].(string); proto == "vless" || proto == "vmess" || proto == "trojan" || proto == "shadowsocks" {
				ob = om
				break
			}
		}
	}
	if ob == nil {
		return config.Node{}, false
	}

	n := config.Node{
		ID:       config.NewUUID(),
		SubID:    subID,
		LastSeen: time.Now().Format("2006-01-02 15:04:05"),
	}
	rawProto, _ := ob["protocol"].(string)
	n.Protocol = rawProto
	if n.Protocol == "shadowsocks" {
		n.Protocol = "ss" // 对齐 manager.go 的 generateOutbound 分支
	}
	n.Name = jStr(cfg, "remarks")
	if n.Name == "" {
		if tag, ok := ob["tag"].(string); ok && tag != "" {
			n.Name = tag
		}
	}

	settings := jMap(ob, "settings")
	ss := jMap(ob, "streamSettings")

	switch rawProto {
	case "vless", "vmess":
		if vnext := jSlice(settings, "vnext"); len(vnext) > 0 {
			v := jMapI(vnext[0])
			n.Server = jStr(v, "address")
			n.Port = strconv.Itoa(int(jNum(v, "port")))
			if users := jSlice(v, "users"); len(users) > 0 {
				u := jMapI(users[0])
				n.UUID = jStr(u, "id")
				n.Security = jStr(u, "security") // vmess 加密方式 / vless 通常 none
			}
		}
	case "trojan", "shadowsocks":
		if servers := jSlice(settings, "servers"); len(servers) > 0 {
			s := jMapI(servers[0])
			n.Server = jStr(s, "address")
			n.Port = strconv.Itoa(int(jNum(s, "port")))
			n.Password = jStr(s, "password")
			if rawProto == "shadowsocks" {
				n.Method = jStr(s, "method")
			}
		}
	}

	n.Network = jStr(ss, "network")
	if n.Network == "" {
		n.Network = "tcp"
	}
	n.TLS = jStr(ss, "security") // "none" / "tls" / "reality"
	if n.TLS == "none" {
		n.TLS = ""
	}

	switch n.Network {
	case "ws":
		ws := jMap(ss, "wsSettings")
		n.RequestHost = jStr(ws, "host")
		n.Path = jStr(ws, "path")
	case "grpc":
		g := jMap(ss, "grpcSettings")
		n.RequestHost = jStr(g, "authority")
		n.Path = jStr(g, "serviceName")
	case "tcp":
		if hdr := jMap(jMap(ss, "tcpSettings"), "header"); jStr(hdr, "type") != "" {
			n.HeaderType = jStr(hdr, "type")
		}
	case "h2":
		h2 := jMap(ss, "httpSettings")
		n.RequestHost = jStr(h2, "host")
		n.Path = jStr(h2, "path")
	}

	// TLS / reality 的 serverName -> SNI
	if n.TLS == "tls" {
		n.SNI = jStr(jMap(ss, "tlsSettings"), "serverName")
	} else if n.TLS == "reality" {
		n.SNI = jStr(jMap(ss, "realitySettings"), "serverName")
	}

	if n.Name == "" {
		n.Name = n.Server + ":" + n.Port
	}
	return n, true
}

// ---- JSON 取值辅助 ----

func jStr(m map[string]interface{}, keys ...string) string {
	cur := interface{}(m)
	for _, k := range keys {
		mm, ok := cur.(map[string]interface{})
		if !ok {
			return ""
		}
		cur = mm[k]
	}
	s, _ := cur.(string)
	return s
}

func jNum(m map[string]interface{}, keys ...string) float64 {
	cur := interface{}(m)
	for _, k := range keys {
		mm, ok := cur.(map[string]interface{})
		if !ok {
			return 0
		}
		cur = mm[k]
	}
	f, _ := cur.(float64)
	return f
}

func jMap(m map[string]interface{}, keys ...string) map[string]interface{} {
	cur := interface{}(m)
	for _, k := range keys {
		mm, ok := cur.(map[string]interface{})
		if !ok {
			return nil
		}
		cur = mm[k]
	}
	mm, _ := cur.(map[string]interface{})
	return mm
}

func jSlice(m map[string]interface{}, keys ...string) []interface{} {
	cur := interface{}(m)
	for _, k := range keys {
		mm, ok := cur.(map[string]interface{})
		if !ok {
			return nil
		}
		cur = mm[k]
	}
	s, _ := cur.([]interface{})
	return s
}

func jMapI(v interface{}) map[string]interface{} {
	m, _ := v.(map[string]interface{})
	return m
}
