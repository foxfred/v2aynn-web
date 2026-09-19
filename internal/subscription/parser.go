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

// 订阅自动刷新的取值边界（web 层校验与 Poll 保护共用，避免魔数分散）
const (
	// MinSubRefresh 最小刷新间隔（秒）。低于此值视为无意义的过于频繁刷新。
	// 注意：0 不在此范围内，但 0 是合法值，表示"禁用自动刷新"。
	MinSubRefresh = 10
	// MaxSafeRefresh 最大刷新间隔（秒，1天）。超过此值视为禁用。
	// 同时是防溢出的保护上限：time.Duration(n) * time.Second 在 n 极大时会溢出为负数，
	// 导致 time.NewTicker panic("non-positive interval")，服务启动即崩溃。
	MaxSafeRefresh = 86400
)

func Poll(cfg *config.Config) {
	// 0 表示禁用；超过上限（含历史遗留的 MaxInt64 "禁用"标记）同样跳过，
	// 既避免无意义轮询，也避免 time.Duration 溢出导致 NewTicker panic
	if cfg.SubRefresh <= 0 || cfg.SubRefresh > MaxSafeRefresh {
		if cfg.SubRefresh > MaxSafeRefresh {
			log.Printf("Poll: 自动更新已禁用(SubRefresh=%d 超出上限%d秒)", cfg.SubRefresh, MaxSafeRefresh)
		}
		return
	}
	ticker := time.NewTicker(time.Duration(cfg.SubRefresh) * time.Second)
	for range ticker.C {
		FetchAll(cfg)
	}
}

// FetchAll 拉取所有订阅分组
func FetchAll(cfg *config.Config) {
	cfg.Lock()
	groups := make([]config.Group, len(cfg.Groups))
	copy(groups, cfg.Groups)
	cfg.Unlock()

	for i := range groups {
		g := &groups[i]
		if g.URL == "" {
			continue // 手动节点分组跳过
		}
		proxy := cfg.SubProxy
		if g.SubProxy != "" {
			proxy = g.SubProxy
		}
		nodes, err := fetchOne(*g, proxy)
		if err != nil {
			log.Printf("Fetch: 分组[%s]拉取失败，保留旧节点: %v", g.Name, err)
			continue
		}
		// 保留旧节点的 ping/speed 数据
		oldPings := make(map[string]int)
		oldSpeeds := make(map[string]float64)
		for _, n := range g.Nodes {
			key := n.Server + ":" + n.Port + ":" + n.Protocol
			if n.Ping != 0 {
				oldPings[key] = n.Ping
			}
			if n.Speed > 0 {
				oldSpeeds[key] = n.Speed
			}
		}
		for i := range nodes {
			key := nodes[i].Server + ":" + nodes[i].Port + ":" + nodes[i].Protocol
			if p, ok := oldPings[key]; ok {
				nodes[i].Ping = p
			}
			if s, ok := oldSpeeds[key]; ok {
				nodes[i].Speed = s
			}
		}
		g.Nodes = nodes
		g.LastFetch = time.Now().Format("2006-01-02 15:04:05")
		log.Printf("Fetch: 分组[%s]拉取到%d个节点", g.Name, len(nodes))
	}

	// 写回
	cfg.Lock()
	cfg.Groups = groups
	_ = cfg.Save()
	cfg.Unlock()
}

// FetchGroup 拉取单个分组（API调用用）
func FetchGroup(cfg *config.Config, groupID string) {
	cfg.Lock()
	var g *config.Group
	for i := range cfg.Groups {
		if cfg.Groups[i].ID == groupID {
			g = &cfg.Groups[i]
			break
		}
	}
	cfg.Unlock()
	if g == nil || g.URL == "" {
		return
	}
	proxy := cfg.SubProxy
	if g.SubProxy != "" {
		proxy = g.SubProxy
	}
	nodes, err := fetchOne(*g, proxy)
	if err != nil {
		log.Printf("FetchGroup: 分组[%s]拉取失败: %v", g.Name, err)
		return
	}
	oldPings := make(map[string]int)
	oldSpeeds := make(map[string]float64)
	for _, n := range g.Nodes {
		key := n.Server + ":" + n.Port + ":" + n.Protocol
		if n.Ping != 0 {
			oldPings[key] = n.Ping
		}
		if n.Speed > 0 {
			oldSpeeds[key] = n.Speed
		}
	}
	for i := range nodes {
		key := nodes[i].Server + ":" + nodes[i].Port + ":" + nodes[i].Protocol
		if p, ok := oldPings[key]; ok {
			nodes[i].Ping = p
		}
		if s, ok := oldSpeeds[key]; ok {
			nodes[i].Speed = s
		}
	}
	cfg.Lock()
	for i := range cfg.Groups {
		if cfg.Groups[i].ID == groupID {
			cfg.Groups[i].Nodes = nodes
			cfg.Groups[i].LastFetch = time.Now().Format("2006-01-02 15:04:05")
			break
		}
	}
	_ = cfg.Save()
	cfg.Unlock()
	log.Printf("FetchGroup: 分组[%s]拉取到%d个节点", g.Name, len(nodes))
}

func fetchOne(g config.Group, proxyURL string) ([]config.Node, error) {
	transport := &http.Transport{}
	if proxyURL != "" {
		if p, err := url.Parse(proxyURL); err == nil {
			transport.Proxy = http.ProxyURL(p)
		}
	}
	client := &http.Client{Timeout: 15 * time.Second, Transport: transport}
	resp, err := client.Get(g.URL)
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
	log.Printf("fetchOne[%s]: 原始响应前%d字节: %q", g.Name, previewLen, raw[:previewLen])

	// 支持 Xray JSON 订阅
	if jnodes, ok := parseXrayJSON(raw, g.ID); ok {
		log.Printf("fetchOne[%s]: Xray JSON 订阅, 解析出 %d 个节点", g.Name, len(jnodes))
		return jnodes, nil
	}

	var nodes []config.Node
	lines := strings.Split(strings.TrimSpace(raw), "\n")
	log.Printf("fetchOne[%s]: 共%d行", g.Name, len(lines))

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if n, err := parse(line, g.ID); err == nil {
			nodes = append(nodes, n)
			continue
		}
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
			log.Printf("fetchOne[%s]: base64解码失败, 行前80字节: %q", g.Name, line[:min(len(line), 80)])
			continue
		}
		for _, subline := range strings.Split(decoded, "\n") {
			subline = strings.TrimSpace(subline)
			if subline == "" {
				continue
			}
			if n, err := parse(subline, g.ID); err == nil {
				nodes = append(nodes, n)
			} else {
				log.Printf("fetchOne[%s]: 解析行失败: %v, 前80字节: %q", g.Name, err, subline[:min(len(subline), 80)])
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
		ID:      config.NewUUID(),
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

// Parse 从单个节点URL解析出一个Node
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

// parseXrayJSON 解析 Xray 原生 JSON 订阅
func parseXrayJSON(raw, subID string) ([]config.Node, bool) {
	trimmed := strings.TrimSpace(raw)
	var data interface{}
	if err := json.Unmarshal([]byte(trimmed), &data); err != nil {
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

func parseXrayConfig(cfg map[string]interface{}, subID string) (config.Node, bool) {
	outs, ok := cfg["outbounds"].([]interface{})
	if !ok || len(outs) == 0 {
		return config.Node{}, false
	}
	var ob map[string]interface{}
	for _, o := range outs {
		om, ok := o.(map[string]interface{})
		if !ok {
			continue
		}
		if tag, ok := om["tag"].(string); ok && tag == "proxy" {
			ob = om
			break
		}
	}
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
		LastSeen: time.Now().Format("2006-01-02 15:04:05"),
	}
	rawProto, _ := ob["protocol"].(string)
	n.Protocol = rawProto
	if n.Protocol == "shadowsocks" {
		n.Protocol = "ss"
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
				n.Security = jStr(u, "security")
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
	n.TLS = jStr(ss, "security")
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
