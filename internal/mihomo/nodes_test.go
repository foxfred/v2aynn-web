package mihomo

import (
	"strings"
	"testing"

	"v2aynn-web/internal/config"
)

// render 把一个 ykv 渲染成 YAML 文本（去掉末尾换行），供断言用。
func render(v any) string {
	var b strings.Builder
	emitYAML(&b, v, 0)
	return strings.TrimRight(b.String(), "\n")
}

func TestNodeToProxyTrojanWS(t *testing.T) {
	n := config.Node{
		ID: "id1", Name: "联通-SJC-03", Protocol: "trojan",
		Server: "104.17.150.47", Port: "443", Password: "foxfred",
		Network: "ws", TLS: "tls", SNI: "cfnew.zjk.dpdns.org",
		Path: "/?ed=2048", RequestHost: "cfnew.zjk.dpdns.org",
	}
	p, ok := nodeToProxy(n)
	if !ok {
		t.Fatal("trojan 节点应当能转换")
	}
	want := strings.Join([]string{
		`type: "trojan"`,
		`server: "104.17.150.47"`,
		`port: 443`,
		`password: "foxfred"`,
		`skip-cert-verify: true`,
		`sni: "cfnew.zjk.dpdns.org"`,
		`network: "ws"`,
		`ws-opts:`,
		`  path: "/?ed=2048"`,
		`  headers:`,
		`    Host: "cfnew.zjk.dpdns.org"`,
	}, "\n")
	if got := render(p); got != want {
		t.Errorf("trojan+ws 转换结果不符\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// vless 无 TLS 时不能写 tls: true，否则内核会尝试握手一个明文端口
func TestNodeToProxyVlessWSNoTLS(t *testing.T) {
	n := config.Node{
		ID: "id2", Name: "优选域名-27", Protocol: "vless",
		Server: "104.21.214.112", Port: "80", UUID: "1c5da85b-98a4",
		Network: "ws", TLS: "", Path: "/?ed=2048", RequestHost: "lingdu.dpdns.org",
	}
	p, ok := nodeToProxy(n)
	if !ok {
		t.Fatal("vless 节点应当能转换")
	}
	got := render(p)
	if strings.Contains(got, "tls:") {
		t.Errorf("TLS 为空时不该出现 tls 字段\n%s", got)
	}
	if strings.Contains(got, "servername") {
		t.Errorf("SNI 为空时不该出现 servername\n%s", got)
	}
	for _, want := range []string{
		`uuid: "1c5da85b-98a4"`, `port: 80`, `udp: true`,
		`client-fingerprint: "chrome"`, `Host: "lingdu.dpdns.org"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("缺少 %q\n%s", want, got)
		}
	}
}

func TestNodeToProxyVlessGRPCTLS(t *testing.T) {
	n := config.Node{
		ID: "id3", Name: "grpc", Protocol: "vless", Server: "1.2.3.4", Port: "443",
		UUID: "u-1", Network: "grpc", TLS: "tls", SNI: "g.example.com", Path: "/svc",
	}
	p, ok := nodeToProxy(n)
	if !ok {
		t.Fatal("vless+grpc 应当能转换")
	}
	got := render(p)
	for _, want := range []string{
		`tls: true`, `servername: "g.example.com"`,
		`network: "grpc"`, `grpc-service-name: "svc"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("缺少 %q\n%s", want, got)
		}
	}
}

// vmess 走 tcp：不该写 network 字段（Clash 默认就是 tcp）
func TestNodeToProxyVMessTCP(t *testing.T) {
	n := config.Node{
		ID: "id4", Name: "vm", Protocol: "vmess", Server: "5.6.7.8", Port: "8080",
		UUID: "u-2", AlterID: "64", Security: "aes-128-gcm", Network: "tcp",
	}
	p, ok := nodeToProxy(n)
	if !ok {
		t.Fatal("vmess 应当能转换")
	}
	got := render(p)
	for _, want := range []string{`cipher: "aes-128-gcm"`, `alterId: 64`, `port: 8080`} {
		if !strings.Contains(got, want) {
			t.Errorf("缺少 %q\n%s", want, got)
		}
	}
	if strings.Contains(got, "network:") {
		t.Errorf("tcp 传输不该写 network 字段\n%s", got)
	}
}

func TestNodeToProxySS(t *testing.T) {
	n := config.Node{
		ID: "id5", Name: "ss", Protocol: "ss", Server: "9.9.9.9",
		Port: "8388", Password: "pw", Method: "chacha20-ietf-poly1305",
	}
	p, ok := nodeToProxy(n)
	if !ok {
		t.Fatal("ss 应当能转换")
	}
	got := render(p)
	for _, want := range []string{
		`cipher: "chacha20-ietf-poly1305"`, `password: "pw"`, `port: 8388`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("缺少 %q\n%s", want, got)
		}
	}
}

// 转不了的节点必须明确返回 false，而不是生成一条连不上的配置
func TestNodeToProxyRejectsBadNodes(t *testing.T) {
	cases := []struct {
		name string
		node config.Node
	}{
		{"缺 server", config.Node{Protocol: "trojan", Port: "443", Password: "pw"}},
		{"未知协议", config.Node{Protocol: "wireguard", Server: "1.1.1.1", Port: "443"}},
		{"协议为空", config.Node{Server: "1.1.1.1", Port: "443"}},
		{"trojan 缺密码", config.Node{Protocol: "trojan", Server: "1.1.1.1", Port: "443"}},
		{"vless 缺 uuid", config.Node{Protocol: "vless", Server: "1.1.1.1", Port: "443"}},
		{"ss 缺密码", config.Node{Protocol: "ss", Server: "1.1.1.1", Port: "443", Method: "aes-256-gcm"}},
	}
	for _, c := range cases {
		if _, ok := nodeToProxy(c.node); ok {
			t.Errorf("[%s] 应当转换失败", c.name)
		}
	}
}

func TestUniqueName(t *testing.T) {
	used := map[string]int{}
	cases := []struct{ in, want string }{
		{"A", "A"},
		{"A", "A #2"},
		{"A", "A #3"},
		{"B", "B"},
		{"", "节点"},
		{"", "节点 #2"},
	}
	for i, c := range cases {
		if got := uniqueName(c.in, used); got != c.want {
			t.Errorf("第 %d 个: uniqueName(%q) = %q, 期望 %q", i, c.in, got, c.want)
		}
	}
}

func TestNormalRules(t *testing.T) {
	cases := []struct {
		mode string
		want []string
	}{
		{"smart", []string{"GEOIP,private,DIRECT,no-resolve", "GEOSITE,cn,DIRECT", "GEOIP,CN,DIRECT", "MATCH,节点选择"}},
		{"", []string{"GEOIP,private,DIRECT,no-resolve", "GEOSITE,cn,DIRECT", "GEOIP,CN,DIRECT", "MATCH,节点选择"}},
		{"unknown", []string{"GEOIP,private,DIRECT,no-resolve", "GEOSITE,cn,DIRECT", "GEOIP,CN,DIRECT", "MATCH,节点选择"}},
		{"global", []string{"MATCH,节点选择"}},
		{"direct", []string{"MATCH,DIRECT"}},
		{"DIRECT", []string{"MATCH,DIRECT"}},
	}
	for _, c := range cases {
		got := normalRules(c.mode, "节点选择")
		if len(got) != len(c.want) {
			t.Errorf("[%s] 规则条数 %d, 期望 %d (%v)", c.mode, len(got), len(c.want), got)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("[%s] 第 %d 条 = %v, 期望 %v", c.mode, i, got[i], c.want[i])
			}
		}
	}
}

func TestBuildNormalConfigEmpty(t *testing.T) {
	if _, err := BuildNormalConfig(nil, NormalOpts{}); err == nil {
		t.Fatal("空节点列表应当报错")
	}
	if _, err := BuildNormalConfig([]config.Node{{Protocol: "wireguard", Server: "1.1.1.1"}}, NormalOpts{}); err == nil {
		t.Fatal("全部节点都转不了时应当报错")
	}
}

// 完整的黄金输出：把格式锁死，避免以后改动无意中破坏 YAML 结构
func TestBuildNormalConfigGolden(t *testing.T) {
	nodes := []config.Node{
		{ID: "id-a", Name: "联通-SJC-03", Protocol: "trojan", Server: "104.17.150.47",
			Port: "443", Password: "foxfred", Network: "ws", TLS: "tls",
			SNI: "cfnew.zjk.dpdns.org", Path: "/?ed=2048", RequestHost: "cfnew.zjk.dpdns.org"},
		{ID: "id-b", Name: "优选域名-27", Protocol: "vless", Server: "104.21.214.112",
			Port: "80", UUID: "1c5da85b", Network: "ws", Path: "/?ed=2048",
			RequestHost: "lingdu.dpdns.org"},
	}
	nc, err := BuildNormalConfig(nodes, NormalOpts{
		SocksPort: 10808, HTTPPort: 10810, AllowLan: true, ProxyMode: "smart",
	})
	if err != nil {
		t.Fatalf("生成失败: %v", err)
	}
	want := strings.Join([]string{
		`port: 10810`,
		`socks-port: 10808`,
		`mixed-port: 0`,
		`allow-lan: true`,
		`redir-port: 12345`,
		`mode: "rule"`,
		`log-level: "warning"`,
		`ipv6: false`,
		`external-controller: "127.0.0.1:19090"`,
		`unified-delay: true`,
		`tcp-concurrent: true`,
		`dns:`,
		`  enable: true`,
		`  ipv6: false`,
		`  enhanced-mode: "redir-host"`,
		`  nameserver:`,
		`    - "223.5.5.5"`,
		`    - "119.29.29.29"`,
		`  fallback:`,
		`    - "1.1.1.1"`,
		`    - "8.8.8.8"`,
		`proxies:`,
		`  - name: "联通-SJC-03"`,
		`    type: "trojan"`,
		`    server: "104.17.150.47"`,
		`    port: 443`,
		`    password: "foxfred"`,
		`    skip-cert-verify: true`,
		`    sni: "cfnew.zjk.dpdns.org"`,
		`    network: "ws"`,
		`    ws-opts:`,
		`      path: "/?ed=2048"`,
		`      headers:`,
		`        Host: "cfnew.zjk.dpdns.org"`,
		`  - name: "优选域名-27"`,
		`    type: "vless"`,
		`    server: "104.21.214.112"`,
		`    port: 80`,
		`    uuid: "1c5da85b"`,
		`    udp: true`,
		`    skip-cert-verify: true`,
		`    client-fingerprint: "chrome"`,
		`    network: "ws"`,
		`    ws-opts:`,
		`      path: "/?ed=2048"`,
		`      headers:`,
		`        Host: "lingdu.dpdns.org"`,
		`proxy-groups:`,
		`  - name: "节点选择"`,
		`    type: "select"`,
		`    proxies:`,
		`      - "联通-SJC-03"`,
		`      - "优选域名-27"`,
		`rules:`,
		`  - "GEOIP,private,DIRECT,no-resolve"`,
		`  - "GEOSITE,cn,DIRECT"`,
		`  - "GEOIP,CN,DIRECT"`,
		`  - "MATCH,节点选择"`,
	}, "\n") + "\n"
	if got := string(nc.YAML); got != want {
		t.Errorf("生成的配置与期望不符\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
	if len(nc.Order) != 2 || nc.Order[0] != "联通-SJC-03" || nc.Order[1] != "优选域名-27" {
		t.Errorf("节点顺序不对: %v", nc.Order)
	}
	if nc.NameToID["联通-SJC-03"] != "id-a" || nc.NameToID["优选域名-27"] != "id-b" {
		t.Errorf("名字到 ID 的映射不对: %v", nc.NameToID)
	}
}

// 重名节点必须被改成唯一名字，同时映射仍然指回各自的 Node.ID
func TestBuildNormalConfigDuplicateNames(t *testing.T) {
	nodes := []config.Node{
		{ID: "a", Name: "同名", Protocol: "trojan", Server: "1.1.1.1", Port: "443", Password: "p"},
		{ID: "b", Name: "同名", Protocol: "trojan", Server: "2.2.2.2", Port: "443", Password: "p"},
	}
	nc, err := BuildNormalConfig(nodes, NormalOpts{})
	if err != nil {
		t.Fatalf("生成失败: %v", err)
	}
	if len(nc.Order) != 2 {
		t.Fatalf("应当有两个节点, 实际 %v", nc.Order)
	}
	if nc.Order[0] == nc.Order[1] {
		t.Fatalf("重名节点没有被改成唯一名字: %v", nc.Order)
	}
	if nc.NameToID[nc.Order[0]] != "a" || nc.NameToID[nc.Order[1]] != "b" {
		t.Errorf("映射不对: %v", nc.NameToID)
	}
	// 两个名字都必须真的出现在配置里
	for _, name := range nc.Order {
		if !strings.Contains(string(nc.YAML), name) {
			t.Errorf("配置里找不到节点名 %q", name)
		}
	}
}

// 转不了的节点要记进 Skipped，其余节点照常生成
func TestBuildNormalConfigRecordsSkipped(t *testing.T) {
	nodes := []config.Node{
		{ID: "a", Name: "好的", Protocol: "trojan", Server: "1.1.1.1", Port: "443", Password: "p"},
		{ID: "b", Name: "坏的", Protocol: "wireguard", Server: "2.2.2.2", Port: "443"},
	}
	nc, err := BuildNormalConfig(nodes, NormalOpts{})
	if err != nil {
		t.Fatalf("生成失败: %v", err)
	}
	if len(nc.Skipped) != 1 || nc.Skipped[0] != "坏的" {
		t.Errorf("Skipped = %v, 期望 [坏的]", nc.Skipped)
	}
	if len(nc.Order) != 1 || nc.Order[0] != "好的" {
		t.Errorf("Order = %v, 期望 [好的]", nc.Order)
	}
}

// 节点名里的引号 / 反斜杠必须被转义，否则生成的 YAML 会被解析成别的意思
func TestBuildNormalConfigEscapesNames(t *testing.T) {
	nodes := []config.Node{
		{ID: "a", Name: `he said "hi" \ end`, Protocol: "trojan", Server: "1.1.1.1",
			Port: "443", Password: "p"},
	}
	nc, err := BuildNormalConfig(nodes, NormalOpts{})
	if err != nil {
		t.Fatalf("生成失败: %v", err)
	}
	got := string(nc.YAML)
	if !strings.Contains(got, `name: "he said \"hi\" \\ end"`) {
		t.Errorf("节点名没有正确转义\n%s", got)
	}
}

// 端口缺省值：不传时应当回落到与 xray 一致的 10808 / 10810
func TestBuildNormalConfigDefaultPorts(t *testing.T) {
	nodes := []config.Node{
		{ID: "a", Name: "n", Protocol: "trojan", Server: "1.1.1.1", Port: "443", Password: "p"},
	}
	nc, err := BuildNormalConfig(nodes, NormalOpts{})
	if err != nil {
		t.Fatalf("生成失败: %v", err)
	}
	got := string(nc.YAML)
	for _, want := range []string{"port: 10810", "socks-port: 10808", "redir-port: 12345",
		"external-controller: \"127.0.0.1:19090\""} {
		if !strings.Contains(got, want) {
			t.Errorf("缺少 %q\n%s", want, got)
		}
	}
}

// 端口字符串带杂质（订阅里偶尔出现 "443 " 之类）时也要取到数字
func TestAtoiDefault(t *testing.T) {
	cases := []struct {
		in   string
		def  int
		want int
	}{
		{"443", 1, 443},
		{"", 1, 1},
		{"abc", 7, 7},
		{" 8080 ", 1, 8080},
		{"443s", 1, 443},
	}
	for _, c := range cases {
		if got := atoiDefault(c.in, c.def); got != c.want {
			t.Errorf("atoiDefault(%q, %d) = %d, 期望 %d", c.in, c.def, got, c.want)
		}
	}
}
