package mihomo

// 普通节点（trojan / vless / vmess / ss）到 Clash 配置的转换。
//
// 与家宽那条路径（rewriteConfig）的区别：cfnew 家宽订阅给的本来就是一份完整的
// Clash 配置，所以那边只需「拿原文、改顶层 7 个键」；而普通订阅给的是一串
// trojan:// / vless:// 链接，程序已经把它解析成了结构化的 config.Node，
// 没有 Clash 原文可用，所以这里要从零生成 proxies / proxy-groups / rules 三段。
//
// 为什么不用 YAML 库：项目一直保持「零外部依赖」（单二进制、无 go.sum）。
// 这份配置的结构是我们自己造的、字段完全可控，手写生成器足够，
// 不值得为此引入 gopkg.in/yaml.v3。
//
// 现状：本文件属于「内核统一」改造的**阶段一**，只提供能力，尚未接入任何调用方
// （线上仍由 xray 跑普通节点）。见 docs/内核统一改造方案.md。

import (
	"fmt"
	"hash/fnv"
	"strconv"
	"strings"

	"v2aynn-web/internal/config"
)

// NormalGroupName 普通节点生成的策略组默认名。
const NormalGroupName = "节点选择"

// NormalNodesSig 给一批普通节点算一个短签名，用来判断「节点集合变了没有」。
//
// 为什么需要它：切节点是高频操作，而 mihomo 只有换了配置才需要重启（十几秒）。
// 节点集合没变时只拨一下策略组就行（毫秒级），变了才值得重写配置并重启内核。
//
// 只覆盖会影响生成结果的字段；用 FNV-1a 拼一遍即可 —— 这里只做相等比较，
// 不需要抗碰撞，也不该为它引入一个哈希依赖。
func NormalNodesSig(nodes []config.Node) string {
	h := fnv.New64a()
	for _, n := range nodes {
		fmt.Fprintf(h, "%s\x00%s\x00%s\x00%s\x00%s\x00%s\x00%s\x00%s\x00%s\x00%s\x00%s\x00%s\x00%s\x00%s\n",
			n.ID, n.Name, n.Protocol, n.Server, n.Port, n.UUID, n.Password,
			n.Method, n.Network, n.TLS, n.SNI, n.Path, n.RequestHost, n.Security)
	}
	return strconv.FormatUint(h.Sum64(), 16)
}

// NormalOpts 生成普通节点配置时的可调项。零值即合理默认。
type NormalOpts struct {
	SocksPort int
	HTTPPort  int
	RedirPort int
	CtrlPort  int
	AllowLan  bool
	// ProxyMode 分流模式：smart（国内直连）/ global（全代理）/ direct（全直连）。
	// 空值与未知取值都回落到 smart，与 xray 那边的兜底保持一致。
	ProxyMode string
	// GroupName 策略组名，空则用 NormalGroupName。
	GroupName string
}

// NormalConfig 生成的普通节点配置及其附带信息。
type NormalConfig struct {
	// YAML 可直接写入 confFileName 交给 mihomo（-f 参数）。
	YAML []byte
	// NameToID 把 Clash 配置里的节点名映射回 config.Node.ID。
	//
	// 节点重名时会给后面的加 " #2" 后缀（Clash 要求节点名唯一），
	// 所以切节点时不能拿 Node.Name 直接去内核里找，必须走这份映射。
	NameToID map[string]string
	// Order 节点在配置里的排列顺序（NameToID 的键序）。
	Order []string
	// Skipped 因协议不支持或关键字段缺失而没写进配置的节点名。
	Skipped []string
}

// BuildNormalConfig 把普通节点列表转成一份完整的 mihomo 配置。
//
// nodes 为空时返回错误：Clash 的策略组不允许没有成员，把空配置交给内核
// 只会得到一句难懂的解析错误，不如在这里就说清楚。
func BuildNormalConfig(nodes []config.Node, o NormalOpts) (*NormalConfig, error) {
	if len(nodes) == 0 {
		return nil, fmt.Errorf("没有可用的普通节点")
	}

	socks, http, redir, ctrl := o.SocksPort, o.HTTPPort, o.RedirPort, o.CtrlPort
	if socks <= 0 {
		socks = 10808
	}
	if http <= 0 {
		http = 10810
	}
	if redir <= 0 {
		redir = RedirPort
	}
	if ctrl <= 0 {
		ctrl = ControlPort
	}
	group := o.GroupName
	if group == "" {
		group = NormalGroupName
	}

	proxies := make([]any, 0, len(nodes))
	names := make([]any, 0, len(nodes))
	nameToID := make(map[string]string, len(nodes))
	order := make([]string, 0, len(nodes))
	var skipped []string
	used := make(map[string]int, len(nodes))

	for _, n := range nodes {
		p, ok := nodeToProxy(n)
		if !ok {
			skipped = append(skipped, n.Name)
			continue
		}
		name := uniqueName(n.Name, used)
		proxies = append(proxies, append(ykv{{"name", name}}, p...))
		names = append(names, name)
		nameToID[name] = n.ID
		order = append(order, name)
	}
	if len(proxies) == 0 {
		return nil, fmt.Errorf("没有可转换的节点（支持 trojan / vless / vmess / ss）")
	}

	doc := ykv{
		{"port", http},        // HTTP 代理入口，与 xray 的 http inbound 对齐
		{"socks-port", socks}, // SOCKS 入口，与 xray 的 socks inbound 对齐
		{"mixed-port", 0},     // 关掉混合端口，避免多监听一个口
		{"allow-lan", o.AllowLan},
		{"redir-port", redir}, // 透明代理入口，与 transparent.sh 的 REDIRECT 一致
		{"mode", "rule"},
		{"log-level", "warning"}, // 盒子上少写日志
		{"ipv6", false},
		{"external-controller", "127.0.0.1:" + strconv.Itoa(ctrl)},
		{"unified-delay", true},
		{"tcp-concurrent", true},
		{"dns", ykv{
			{"enable", true},
			{"ipv6", false},
			// redir-host 而不是 fake-ip：与 xray 的行为最接近，
			// 且规则里用 GEOSITE / GEOIP 时不需要额外处理 fake-ip 映射。
			{"enhanced-mode", "redir-host"},
			{"nameserver", []any{"223.5.5.5", "119.29.29.29"}},
			{"fallback", []any{"1.1.1.1", "8.8.8.8"}},
		}},
		{"proxies", proxies},
		{"proxy-groups", []any{
			ykv{
				{"name", group},
				{"type", "select"},
				{"proxies", names},
			},
		}},
		{"rules", normalRules(o.ProxyMode, group)},
	}

	var b strings.Builder
	emitYAML(&b, doc, 0)

	return &NormalConfig{
		YAML:     []byte(b.String()),
		NameToID: nameToID,
		Order:    order,
		Skipped:  skipped,
	}, nil
}

// nodeToProxy 把一个 config.Node 转成 Clash 的 proxy 条目。
// 返回的 ykv **不含 name** —— 名字由调用方填，因为要先做唯一化。
// 第二个返回值为 false 表示这个节点转不了（协议不支持，或缺关键字段）。
func nodeToProxy(n config.Node) (ykv, bool) {
	proto := strings.ToLower(strings.TrimSpace(n.Protocol))
	server := strings.TrimSpace(n.Server)
	if server == "" {
		return nil, false
	}
	port := atoiDefault(n.Port, 443)

	net := strings.ToLower(strings.TrimSpace(n.Network))
	tls := strings.ToLower(strings.TrimSpace(n.TLS))
	sni := strings.TrimSpace(n.SNI)
	host := strings.TrimSpace(n.RequestHost)
	if host == "" {
		host = sni
	}
	path := n.Path
	if path == "" {
		path = "/"
	}

	p := ykv{
		{"type", proto},
		{"server", server},
		{"port", port},
	}

	switch proto {
	case "trojan":
		if n.Password == "" {
			return nil, false
		}
		p = append(p, ykv{
			{"password", n.Password},
			{"skip-cert-verify", true},
		}...)
		if sni != "" {
			p = append(p, ykv{{"sni", sni}}...)
		}
	case "vless":
		if n.UUID == "" {
			return nil, false
		}
		p = append(p, ykv{
			{"uuid", n.UUID},
			{"udp", true},
			{"skip-cert-verify", true},
			{"client-fingerprint", "chrome"},
		}...)
		if tls != "" && tls != "none" {
			p = append(p, ykv{{"tls", true}}...)
		}
		if sni != "" {
			p = append(p, ykv{{"servername", sni}}...)
		}
	case "vmess":
		if n.UUID == "" {
			return nil, false
		}
		cipher := n.Security
		if cipher == "" {
			cipher = "auto"
		}
		p = append(p, ykv{
			{"uuid", n.UUID},
			{"alterId", atoiDefault(n.AlterID, 0)},
			{"cipher", cipher},
			{"udp", true},
			{"skip-cert-verify", true},
		}...)
		if tls == "tls" {
			p = append(p, ykv{{"tls", true}}...)
		}
		if sni != "" {
			p = append(p, ykv{{"servername", sni}}...)
		}
	case "ss":
		if n.Password == "" {
			return nil, false
		}
		method := n.Method
		if method == "" {
			method = "aes-256-gcm"
		}
		p = append(p, ykv{
			{"cipher", method},
			{"password", n.Password},
			{"udp", true},
		}...)
	default:
		return nil, false
	}

	switch net {
	case "ws":
		p = append(p, ykv{{"network", "ws"}}...)
		opts := ykv{{"path", path}}
		if host != "" {
			opts = append(opts, ykv{{"headers", ykv{{"Host", host}}}}...)
		}
		p = append(p, ykv{{"ws-opts", opts}}...)
	case "grpc":
		p = append(p, ykv{
			{"network", "grpc"},
			{"grpc-opts", ykv{{"grpc-service-name", strings.TrimPrefix(path, "/")}}},
		}...)
	case "h2", "http":
		opts := ykv{{"path", path}}
		if host != "" {
			opts = append(opts, ykv{{"host", []any{host}}}...)
		}
		p = append(p, ykv{{"network", "h2"}, {"h2-opts", opts}}...)
	}
	// net 为空或 "tcp" 时不写 network —— Clash 的默认值就是 tcp
	return p, true
}

// normalRules 按分流模式生成 Clash 规则。
//
// 与 xray 的对应关系（proxyMode 的语义完全一致）：
//
//	smart  —— 国内域名/IP 直连，其余走代理
//	global —— 全部走代理
//	direct —— 全部直连
//
// 注意 smart 模式用到了 GEOSITE，mihomo 需要 geosite.dat 才加载得起来
// （见 Manager.CheckEnv 与 usesGeosite）。
func normalRules(mode, group string) []any {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "global":
		return []any{"MATCH," + group}
	case "direct":
		return []any{"MATCH,DIRECT"}
	default: // smart
		return []any{
			"GEOIP,private,DIRECT,no-resolve",
			"GEOSITE,cn,DIRECT",
			"GEOIP,CN,DIRECT",
			"MATCH," + group,
		}
	}
}

// uniqueName 保证节点名在配置里唯一 —— Clash 的 proxy 名不允许重复，
// 重名会让内核直接报解析错误。重名时从第二个开始加 " #N" 后缀。
func uniqueName(name string, used map[string]int) string {
	if strings.TrimSpace(name) == "" {
		name = "节点"
	}
	used[name]++
	if used[name] == 1 {
		return name
	}
	// "名字 #2" 本身也可能已被占用，继续往后找
	for i := used[name]; ; i++ {
		cand := fmt.Sprintf("%s #%d", name, i)
		if used[cand] == 0 {
			used[cand] = 1
			return cand
		}
	}
}

// atoiDefault 从可能带非数字字符的字符串里取数字，取不到时返回默认值。
// 端口 / alterId 在 Node 里都是字符串（订阅原文里什么样就存什么样）。
func atoiDefault(s string, def int) int {
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

// --- YAML 生成（只覆盖我们生成配置用到的子集，不追求通用）---

// yEntry 一个 YAML 映射条目。
type yEntry struct {
	k string
	v any
}

// ykv 一个有序的 YAML 映射。
//
// 用切片而不是 map：键顺序虽然无语义，但生成结果必须**稳定** ——
// 同样的输入永远产出同样的字节，便于比对与测试。
type ykv []yEntry

// emitYAML 把 ykv / []any / 标量渲染成 YAML 文本。
func emitYAML(b *strings.Builder, v any, indent int) {
	pad := strings.Repeat("  ", indent)
	switch x := v.(type) {
	case ykv:
		for _, e := range x {
			emitKey(b, pad, e.k, e.v, indent)
		}
	case []any:
		for _, it := range x {
			switch e := it.(type) {
			case ykv:
				if len(e) == 0 {
					b.WriteString(pad + "- {}\n")
					continue
				}
				emitListItem(b, pad, e[0].k, e[0].v, indent)
				for _, rest := range e[1:] {
					emitKey(b, pad+"  ", rest.k, rest.v, indent+1)
				}
			case []any:
				b.WriteString(pad + "-\n")
				emitYAML(b, e, indent+1)
			default:
				b.WriteString(pad + "- " + yamlScalar(it) + "\n")
			}
		}
	}
}

// emitKey 渲染「键: 值」，值为嵌套结构时另起一行并缩进。
func emitKey(b *strings.Builder, pad, k string, v any, indent int) {
	switch v.(type) {
	case ykv, []any:
		b.WriteString(pad + k + ":\n")
		emitYAML(b, v, indent+1)
	default:
		b.WriteString(pad + k + ": " + yamlScalar(v) + "\n")
	}
}

// emitListItem 渲染列表项「- 键: 值」—— 第一个键跟在短横线后面。
func emitListItem(b *strings.Builder, pad, k string, v any, indent int) {
	switch v.(type) {
	case ykv, []any:
		b.WriteString(pad + "- " + k + ":\n")
		emitYAML(b, v, indent+2)
	default:
		b.WriteString(pad + "- " + k + ": " + yamlScalar(v) + "\n")
	}
}

// yamlScalar 渲染一个 YAML 标量。
func yamlScalar(v any) string {
	switch x := v.(type) {
	case nil:
		return "null"
	case bool:
		if x {
			return "true"
		}
		return "false"
	case int:
		return strconv.Itoa(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case string:
		return quoteYAML(x)
	default:
		return quoteYAML(fmt.Sprint(x))
	}
}

// quoteYAML 用双引号包裹字符串并转义。
//
// 一律加引号，而不是「需要时才加」：节点名里 emoji、冒号、井号、方括号都常见，
// 逐个判断「要不要加引号」比无脑加更容易出错，而多一对引号没有任何代价。
func quoteYAML(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 2)
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}
