package subscription

import (
	"encoding/base64"
	"encoding/json"
	"errors"
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

// pollTick 是「复查配置」的周期，不是拉取间隔。
// 为什么要复查而不是一次算好间隔，见 Poll 的说明。
// 声明为变量而非常量，是为了让测试能把它缩短，否则每个用例都得干等 5 秒。
var pollTick = 5 * time.Second

// Poll 按配置的间隔自动拉取订阅，直到进程退出。
//
// 这里刻意**不用**「启动时按 SubRefresh 建一个固定 ticker」的写法：
// 那样配置只在进程启动那一刻被读一次，用户之后在设置里把间隔改成 0（禁用）
// 或改成别的值，已经在跑的 ticker 根本不会变，必须重启服务才生效 ——
// 表现就是「界面明明写着已禁用，后台还在偷偷刷新订阅」。
//
// 现在改成每 pollTick 复查一次配置：禁用后立即停止拉取，改间隔或重新启用
// 也都在 pollTick 之内生效，不需要重启服务。
func Poll(cfg *config.Config) {
	var elapsed time.Duration
	overLimitLogged := false

	for range time.Tick(pollTick) {
		cfg.Lock()
		n := cfg.SubRefresh
		cfg.Unlock()

		// 0 表示禁用；超过上限（含历史遗留的 MaxInt64 "禁用"标记）同样视为禁用，
		// 既避免无意义轮询，也避免 time.Duration 溢出
		if n <= 0 || n > MaxSafeRefresh {
			// 只在状态发生变化时打一次日志，否则每 5 秒刷一条会淹掉日志
			if n > MaxSafeRefresh && !overLimitLogged {
				log.Printf("Poll: 自动更新已禁用(SubRefresh=%d 超出上限%d秒)", n, MaxSafeRefresh)
				overLimitLogged = true
			}
			elapsed = 0
			continue
		}
		overLimitLogged = false

		elapsed += pollTick
		if elapsed < time.Duration(n)*time.Second {
			continue
		}
		elapsed = 0
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
		if g.IsClash() {
			continue // 家宽分组的刷新由 mihomo 内核那条路负责，不走这里
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
		// 把旧节点的身份与测速结果接到新拉到的节点上（见 carryOver）
		g.Nodes = carryOver(g.Nodes, nodes)
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
	if errors.Is(err, ErrIsClashSub) {
		// 这是家宽订阅。把它标记成家宽分组，节点交给 mihomo 内核 ——
		// 本程序没有这些节点的 Node 结构，硬塞进来还会被 xray 的故障转移
		// 当成候选节点（那是另一套协议，切过去必然连不上）。
		cfg.Lock()
		for i := range cfg.Groups {
			if cfg.Groups[i].ID == groupID {
				cfg.Groups[i].Kind = config.GroupKindClash
				cfg.Groups[i].Nodes = nil
				break
			}
		}
		_ = cfg.Save()
		cfg.Unlock()
		log.Printf("FetchGroup: 分组[%s]识别为家宽(Clash)订阅，已交给 mihomo 内核", g.Name)
		return
	}
	if err != nil {
		log.Printf("FetchGroup: 分组[%s]拉取失败: %v", g.Name, err)
		return
	}
	cfg.Lock()
	for i := range cfg.Groups {
		if cfg.Groups[i].ID == groupID {
			// 用锁里的当前节点做迁移，而不是函数开头抓的那个指针 —— 拉订阅
			// 要联网、耗时好几秒，期间 cfg.Groups 可能已经被整批换过了
			// （FetchAll 结尾就是 cfg.Groups = groups），老指针指向的是旧数组。
			cfg.Groups[i].Nodes = carryOver(cfg.Groups[i].Nodes, nodes)
			cfg.Groups[i].LastFetch = time.Now().Format("2006-01-02 15:04:05")
			break
		}
	}
	_ = cfg.Save()
	cfg.Unlock()
	log.Printf("FetchGroup: 分组[%s]拉取到%d个节点", g.Name, len(nodes))
}

// strictKey 节点的稳定身份键：协议 + 服务器 + 端口 + 凭证。
//
// 为什么不直接用 server:port:protocol：同一个订阅里两个节点指向同一服务器并不
// 罕见（盒子实测 BPB 分组 105 个节点只有 104 个唯一 server:port:protocol），
// 光按它匹配会让两个节点共用一份测速结果。
//
// 为什么把凭证算进去：订阅源改备注（换显示名）时节点其实没变，不该换身份；
// 而 UUID / 密码才是节点的真实身份。ss 用 Password，trojan 用 Password，
// vmess / vless 用 UUID，正好各占一个。
func strictKey(n config.Node) string {
	return n.Protocol + "|" + n.Server + "|" + n.Port + "|" + n.UUID + "|" + n.Password
}

// looseKey 节点的宽松身份键：协议 + 服务器 + 端口。
//
// 只用来接测速结果，不用来接 ID —— 理由见 carryOver。
func looseKey(n config.Node) string {
	return n.Protocol + "|" + n.Server + "|" + n.Port
}

// carryOver 把旧节点上的身份与测速结果接到新拉到的节点上。
//
// 订阅每次刷新都是重新解析出来的，节点 ID 也重新生成（config.NewUUID()）。
// 如果就这么整批替换掉旧列表，会有两个后果：
//
//	① 配置里记的 ActiveNode 指向的 ID 已经不存在了 —— 重启时
//	   ensureActiveNodeLocked / Start 会把用户选中的节点换成列表里的第一个，
//	   用户看到的就是「我选定的节点自己变了」；
//	② 界面上测速结果与节点的关联断掉（界面上是拿 ID 当 key 的）。
//
// 所以这里按稳定身份把旧节点的 ID / Ping / Speed 原样接过来。ID 沿用之后，
// 一次订阅刷新不再等于「所有节点都换了人」，用户的选择能原地保留。
//
// 两层匹配，各有分工：
//
//	严格键（含凭证）命中 → ID、Ping、Speed 全部沿用；
//	严格键失配但宽松键命中 → 只沿用 Ping、Speed，ID 重新生成。
//
// 第二层不能省：凭证被订阅源轮换、而服务器没变时严格键会失配，那正是老代码
// 唯一能用的匹配方式，去掉就等于把「测速保留」做窄了。
//
// ID 只在「新列表里该严格键唯一、旧列表里也唯一」时才沿用 —— 否则会出现两个
// 节点共用一个 ID，之后 FindNode / ActiveNode 都会命中错的那个。
func carryOver(old, fresh []config.Node) []config.Node {
	strict := make(map[string]config.Node, len(old))
	strictDup := make(map[string]bool)
	for _, n := range old {
		k := strictKey(n)
		if _, ok := strict[k]; ok {
			strictDup[k] = true
			continue
		}
		strict[k] = n
	}

	oldPings := make(map[string]int, len(old))
	oldSpeeds := make(map[string]float64, len(old))
	for _, n := range old {
		k := looseKey(n)
		if n.Ping != 0 {
			oldPings[k] = n.Ping
		}
		if n.Speed > 0 {
			oldSpeeds[k] = n.Speed
		}
	}

	freshCount := make(map[string]int, len(fresh))
	for _, n := range fresh {
		freshCount[strictKey(n)]++
	}

	for i := range fresh {
		k := strictKey(fresh[i])
		if o, ok := strict[k]; ok && freshCount[k] == 1 && !strictDup[k] {
			fresh[i].ID = o.ID
			fresh[i].Ping = o.Ping
			fresh[i].Speed = o.Speed
			continue
		}
		lk := looseKey(fresh[i])
		if p, ok := oldPings[lk]; ok {
			fresh[i].Ping = p
		}
		if s, ok := oldSpeeds[lk]; ok {
			fresh[i].Speed = s
		}
	}
	return fresh
}

// ErrIsClashSub 表示拉回来的不是节点列表，而是一份 Clash 家宽配置。
//
// 家宽订阅（cfnew 的 target=vg 链接）是一份完整 Clash 配置，节点类型是 openvpn，
// 必须整份交给 mihomo 内核跑，普通解析器对它无能为力。识别出来后由 FetchGroup
// 把分组标记成家宽类型，后续刷新就只走内核那条路了。
var ErrIsClashSub = errors.New("这是 Clash 家宽订阅，已交给 mihomo 内核")

// looksLikeClashVPN 判断订阅原文是不是 cfnew 那种「家宽」Clash 配置。
//
// 判据取两条同时成立：顶层有 proxies: 段，且里面出现了 type: openvpn。
// 只看 proxies: 不够 —— 普通 Clash 机场订阅也有这一段，但它们的节点是
// vless/vmess，本程序解析不了，得给用户一句明确的说明而不是静默 0 个节点。
func looksLikeClashVPN(raw string) bool {
	return strings.Contains(raw, "proxies:") && strings.Contains(raw, "type: openvpn")
}

// looksLikeClash 判断订阅原文是不是任意 Clash 配置（用于给出更准确的错误提示）
func looksLikeClash(raw string) bool {
	return strings.Contains(raw, "proxies:") && strings.Contains(raw, "proxy-groups:")
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

	// 家宽订阅：整份 Clash 配置，交给 mihomo 内核，不走下面的节点解析
	if looksLikeClashVPN(raw) {
		return nil, ErrIsClashSub
	}
	if looksLikeClash(raw) {
		return nil, fmt.Errorf("这是 Clash 格式订阅，但里面没有家宽(openvpn)节点；本程序只支持 cfnew 的家宽订阅（链接需带 target=vg）")
	}

	// 支持 Xray JSON 订阅
	if jnodes, ok := parseXrayJSON(raw); ok {
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
		if n, err := parse(line); err == nil {
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
			if n, err := parse(subline); err == nil {
				nodes = append(nodes, n)
			} else {
				log.Printf("fetchOne[%s]: 解析行失败: %v, 前80字节: %q", g.Name, err, subline[:min(len(subline), 80)])
			}
		}
	}
	// 按 server:port:protocol 去重。订阅源自身常有重复条目，同一订阅里不同名称
	// 指向同一服务器的情况也不少见。去重后节点数更少，列表更清爽、全量测速更快，
	// 也避免同一节点被重复测速浪费流量。
	//
	// 放在这里而不是各个调用方，是为了让 FetchAll 与 FetchGroup 两条路径都生效
	// —— dedupNodes 曾长期定义了却无人调用，导致 README 宣传的"自动去重"实际未生效。
	return dedupNodes(nodes), nil
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

func parse(raw string) (config.Node, error) {
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
	return parse(raw)
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
func parseXrayJSON(raw string) ([]config.Node, bool) {
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
		if n, ok := parseXrayConfig(cfg); ok {
			nodes = append(nodes, n)
		}
	}
	if len(nodes) == 0 {
		return nil, false
	}
	return nodes, true
}

func parseXrayConfig(cfg map[string]interface{}) (config.Node, bool) {
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
