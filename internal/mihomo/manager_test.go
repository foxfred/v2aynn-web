package mihomo

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"v2aynn-web/internal/config"
)

// sampleSub 是脱敏后的 cfnew 家宽订阅样本，结构与真实订阅一致：
// 一个 url-test 组当 dialer、一个 fallback 自动组、一个 select 手动组，
// 以及规则 MATCH 指向的顶层 select 组。
const sampleSub = `mixed-port: 7890
allow-lan: false
mode: rule
log-level: info
ipv6: false
external-controller: 127.0.0.1:9090
dns:
  enable: true
  enhanced-mode: fake-ip
  nameserver:
    - https://223.5.5.5/dns-query

proxies:
  - name: "优选域名-01"
    type: vless
    server: "example.com"
    port: 443
    network: ws
  - name: "联通-09"
    type: vless
    server: "example.net"
    port: 443
    network: ws
  - name: "🏠 JP-家宽-01"
    type: openvpn
    server: 1.2.3.4
    port: 1776
    proto: tcp
    dialer-proxy: "⚡ CF前置"
  - name: "🏠 KR-家宽-01"
    type: openvpn
    server: 5.6.7.8
    port: 1323
    proto: tcp
    dialer-proxy: "⚡ CF前置"

proxy-groups:
  - name: "⚡ CF前置"
    type: url-test
    url: https://www.gstatic.com/generate_204
    interval: 300
    proxies:
      - "优选域名-01"
      - "联通-09"
  - name: "🏠 家宽自动"
    type: fallback
    url: https://www.gstatic.com/generate_204
    interval: 1800
    proxies:
      - "🏠 JP-家宽-01"
      - "🏠 KR-家宽-01"
  - name: "🏠 家宽节点"
    type: select
    proxies:
      - "🏠 JP-家宽-01"
      - "🏠 KR-家宽-01"
  - name: "🚀 节点选择"
    type: select
    proxies:
      - "🏠 家宽自动"
      - "🏠 家宽节点"
      - "⚡ CF前置"
      - DIRECT

rules:
  - GEOIP,CN,DIRECT,no-resolve
  - MATCH,🚀 节点选择
`

func TestParseSubscription(t *testing.T) {
	info := parseSubscription(sampleSub)

	wantNodes := []string{"🏠 JP-家宽-01", "🏠 KR-家宽-01"}
	if len(info.nodes) != len(wantNodes) {
		t.Fatalf("家宽节点数 = %d (%v), 期望 %d", len(info.nodes), info.nodes, len(wantNodes))
	}
	for i, n := range wantNodes {
		if info.nodes[i] != n {
			t.Errorf("节点[%d] = %q, 期望 %q", i, info.nodes[i], n)
		}
	}

	// 手动选择组必须挑 select 类型的那个，而不是 fallback 的「家宽自动」：
	// fallback 组切了会被下一次测速覆盖，切它没有意义。
	if info.nodeGroup != "🏠 家宽节点" {
		t.Errorf("nodeGroup = %q, 期望 %q", info.nodeGroup, "🏠 家宽节点")
	}
	if info.topGroup != "🚀 节点选择" {
		t.Errorf("topGroup = %q, 期望 %q", info.topGroup, "🚀 节点选择")
	}

	// 前置通道组名与它的成员都要能取到 —— 换前置时要从成员里挑候选。
	if info.frontGroup != "⚡ CF前置" {
		t.Errorf("frontGroup = %q, 期望 %q", info.frontGroup, "⚡ CF前置")
	}
	wantMembers := []string{"优选域名-01", "联通-09"}
	if len(info.frontMembers) != len(wantMembers) {
		t.Fatalf("frontMembers = %v, 期望 %v", info.frontMembers, wantMembers)
	}
	for i, n := range wantMembers {
		if info.frontMembers[i] != n {
			t.Errorf("frontMembers[%d] = %q, 期望 %q", i, info.frontMembers[i], n)
		}
	}
}

// 订阅里没有 openvpn 节点时（例如链接漏了 target=vg），
// 必须返回空结果让调用方报错，而不是猜一个组名出来。
func TestParseSubscriptionNoOpenVPN(t *testing.T) {
	src := `proxies:
  - name: "A"
    type: vless
    server: "example.com"
proxy-groups:
  - name: "手动"
    type: select
    proxies:
      - "A"
rules:
  - MATCH,手动
`
	info := parseSubscription(src)
	if len(info.nodes) != 0 {
		t.Errorf("不应解析出节点, 得到 %v", info.nodes)
	}
	if info.nodeGroup != "" || info.topGroup != "" {
		t.Errorf("不应识别出组, 得到 nodeGroup=%q topGroup=%q", info.nodeGroup, info.topGroup)
	}
}

// 顶层组的识别不能误伤：成员里没有 nodeGroup 的 select 组不算顶层组。
func TestParseSubscriptionTopGroupRequiresMembership(t *testing.T) {
	src := `proxies:
  - name: "HW1"
    type: openvpn
  - name: "HW2"
    type: openvpn
proxy-groups:
  - name: "无关组"
    type: select
    proxies:
      - DIRECT
  - name: "家宽组"
    type: select
    proxies:
      - "HW1"
      - "HW2"
`
	info := parseSubscription(src)
	if info.nodeGroup != "家宽组" {
		t.Fatalf("nodeGroup = %q, 期望 %q", info.nodeGroup, "家宽组")
	}
	if info.topGroup != "" {
		t.Errorf("不应识别出顶层组, 得到 %q", info.topGroup)
	}
}

func TestRewriteConfig(t *testing.T) {
	out, err := rewriteConfig(sampleSub, confPorts{
		socks: 10808, http: 10810, redir: 12345, ctrl: 19090, allowLan: true,
	})
	if err != nil {
		t.Fatalf("rewriteConfig 失败: %v", err)
	}

	got := topValues(out)
	want := map[string]string{
		"port":                "10810",
		"socks-port":          "10808",
		"mixed-port":          "0",
		"allow-lan":           "true",
		"redir-port":          "12345",
		"log-level":           "warning",
		"external-controller": "127.0.0.1:19090",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("顶层键 %s = %q, 期望 %q", k, got[k], v)
		}
	}

	// 订阅原文里没有 port/socks-port/redir-port，应当被插入 3 行
	if n := strings.Count(out, "\n"); n != strings.Count(sampleSub, "\n")+3 {
		t.Errorf("改写后行数 = %d, 期望 %d", n, strings.Count(sampleSub, "\n")+3)
	}

	// 关键：proxies / proxy-groups / rules 一个字节都不能动。
	// dialer-proxy 指向策略组，是整条家宽链的命脉，丢了就静默失效。
	for _, keep := range []string{
		`  - name: "🏠 JP-家宽-01"`,
		`    type: openvpn`,
		`    dialer-proxy: "⚡ CF前置"`,
		`  - name: "🏠 家宽自动"`,
		`    type: fallback`,
		`  - name: "🚀 节点选择"`,
		`  - MATCH,🚀 节点选择`,
	} {
		if !strings.Contains(out, keep) {
			t.Errorf("改写后丢失了原配置内容: %q", keep)
		}
	}
	if n := strings.Count(out, "dialer-proxy:"); n != 2 {
		t.Errorf("dialer-proxy 出现 %d 次, 期望 2 次", n)
	}
}

// 改写必须幂等：对已改写过的配置再跑一次，结果应当逐字节相同。
// 这条能挡住「重复保存设置导致配置文件不断膨胀」这类问题。
func TestRewriteConfigIdempotent(t *testing.T) {
	once, err := rewriteConfig(sampleSub, confPorts{socks: 10808, http: 10810, redir: 12345, ctrl: 19090, allowLan: true})
	if err != nil {
		t.Fatalf("第一次改写失败: %v", err)
	}
	twice, err := rewriteConfig(once, confPorts{socks: 10808, http: 10810, redir: 12345, ctrl: 19090, allowLan: true})
	if err != nil {
		t.Fatalf("第二次改写失败: %v", err)
	}
	if once != twice {
		t.Errorf("改写不幂等:\n第一次行数=%d 第二次行数=%d",
			strings.Count(once, "\n"), strings.Count(twice, "\n"))
	}
}

func TestRewriteConfigNoProxies(t *testing.T) {
	if _, err := rewriteConfig("mode: rule\n", confPorts{socks: 10808, http: 10810, redir: 12345, ctrl: 19090, allowLan: true}); err == nil {
		t.Error("缺少 proxies: 段时应当报错")
	}
}

func TestTopKey(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"proxies:", "proxies", true},
		{"mixed-port: 7890", "mixed-port", true},
		{"dialer-proxy: x", "dialer-proxy", true},
		{"  - name: \"A\"", "", false}, // 有缩进
		{"# 注释", "", false},
		{"", "", false},
		{"- name: A", "", false}, // 列表项不是顶层键
		{"https://x", "https", true},
	}
	for _, c := range cases {
		got, ok := topKey(c.in)
		if ok != c.ok || got != c.want {
			t.Errorf("topKey(%q) = (%q, %v), 期望 (%q, %v)", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestUnquote(t *testing.T) {
	cases := map[string]string{
		`"abc"`:   "abc",
		`'abc'`:   "abc",
		`abc`:     "abc",
		`"a"`:     "a",
		`"`:       `"`,
		`  "a"  `: "a",
	}
	for in, want := range cases {
		if got := unquote(in); got != want {
			t.Errorf("unquote(%q) = %q, 期望 %q", in, got, want)
		}
	}
}

// TestCheckEnv 覆盖「缺文件时能提前报错」这条防线。
// 缺 geo 数据时 mihomo 会联网下载并卡满 90 秒，现象是进程在跑但端口不监听，
// 所以必须在启动前拦住并给出明确提示。
func TestCheckEnv(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(dir)

	if err := m.CheckEnv(); err == nil {
		t.Error("内核不存在时应当报错")
	}

	bin := filepath.Join(dir, "mihomo")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0755); err != nil {
		t.Fatal(err)
	}
	m.SetMihomoBin(bin)

	if err := m.CheckEnv(); err == nil {
		t.Error("缺少 geoip.metadb 时应当报错")
	}

	if err := os.WriteFile(filepath.Join(dir, GeoIPMetaDB), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	// 配置里没有 GEOSITE 规则时，geosite.dat 不是必需的
	if err := m.CheckEnv(); err != nil {
		t.Errorf("只需要 geoip.metadb 时不应报错: %v", err)
	}

	if err := os.WriteFile(filepath.Join(dir, confFileName),
		[]byte("rules:\n  - GEOSITE,cn,DIRECT\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := m.CheckEnv(); err == nil {
		t.Error("规则用到 GEOSITE 且缺 geosite.dat 时应当报错")
	}

	if err := os.WriteFile(filepath.Join(dir, GeoFile), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := m.CheckEnv(); err != nil {
		t.Errorf("补齐 geosite.dat 后不应报错: %v", err)
	}
}

func topValues(s string) map[string]string {
	m := map[string]string{}
	for _, ln := range strings.Split(s, "\n") {
		k, ok := topKey(ln)
		if !ok {
			continue
		}
		if i := strings.IndexByte(ln, ':'); i > 0 {
			m[k] = strings.TrimSpace(ln[i+1:])
		}
	}
	return m
}

// 用真实订阅做冒烟测试。sim-data 是本地调试目录、不进仓库，
// 所以文件不存在时跳过，不阻塞 CI。
func TestParseRealSubscription(t *testing.T) {
	const path = "../../sim-data/_cfnew/sub.yaml"
	b, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("本地无真实订阅样本，跳过: %v", err)
	}
	info := parseSubscription(string(b))
	if len(info.nodes) == 0 {
		t.Fatal("真实订阅里没解析出家宽节点")
	}
	if info.nodeGroup == "" {
		t.Error("真实订阅里没识别出手动选择组")
	}
	if info.topGroup == "" {
		t.Error("真实订阅里没识别出顶层主组")
	}
	t.Logf("真实订阅: %d 个家宽节点, 手动组=%q, 顶层组=%q",
		len(info.nodes), info.nodeGroup, info.topGroup)

	out, err := rewriteConfig(string(b), confPorts{socks: 10808, http: 10810, redir: 12345, ctrl: 19090, allowLan: true})
	if err != nil {
		t.Fatalf("真实订阅改写失败: %v", err)
	}
	// 改写后必须仍能解析出同样的结构
	info2 := parseSubscription(out)
	if len(info2.nodes) != len(info.nodes) || info2.nodeGroup != info.nodeGroup || info2.topGroup != info.topGroup {
		t.Errorf("改写后结构变化: 节点 %d->%d, 手动组 %q->%q, 顶层组 %q->%q",
			len(info.nodes), len(info2.nodes), info.nodeGroup, info2.nodeGroup, info.topGroup, info2.topGroup)
	}
	if n := strings.Count(out, "dialer-proxy:"); n != strings.Count(string(b), "dialer-proxy:") {
		t.Errorf("dialer-proxy 数量变化: %d -> %d",
			strings.Count(string(b), "dialer-proxy:"), n)
	}
}

// --- 以下是家宽节点测速相关测试 ---
//
// 家宽节点的延迟必须由 mihomo 内核来测（本程序的 TCP 探测只能探到 CF 前置节点，
// 反映不出 OpenVPN 隧道是否可用）。这里用假 external-controller 覆盖客户端逻辑。

// newFakeKernel 造一个假的 external-controller，并把管理器的控制地址指过去。
func newFakeKernel(t *testing.T, handler http.HandlerFunc) *Manager {
	t.Helper()
	fake := httptest.NewServer(handler)
	t.Cleanup(fake.Close)

	m := NewManager(t.TempDir())
	m.mu.Lock()
	m.nodes = []string{"🏠 JP-家宽-01", "🏠 KR-家宽-01"}
	m.nodeGroup = "🏠 家宽节点"
	m.topGroup = "🚀 节点选择"
	m.ctrlAddr = strings.TrimPrefix(fake.URL, "http://")
	m.mu.Unlock()
	return m
}

// 读内核缓存：内核记了历史就照实返回，没测过的节点不出现在结果里。
func TestProxyDelaysReadsKernelHistory(t *testing.T) {
	m := newFakeKernel(t, func(rw http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/proxies" {
			t.Errorf("路径 = %q, 期望 /proxies", r.URL.Path)
		}
		_, _ = rw.Write([]byte(`{"proxies":{
			"🏠 JP-家宽-01":{"type":"OpenVPN","name":"🏠 JP-家宽-01","alive":true,
				"history":[{"time":"2026-09-30T20:00:00Z","delay":1500},{"time":"2026-09-30T20:01:00Z","delay":1200}]},
			"🏠 KR-家宽-01":{"type":"OpenVPN","name":"🏠 KR-家宽-01","alive":false,
				"history":[{"time":"2026-09-30T20:01:00Z","delay":0}]},
			"🏠 家宽节点":{"type":"Selector","name":"🏠 家宽节点","alive":true},
			"优选域名-01":{"type":"Vless","name":"优选域名-01","alive":true,
				"history":[{"time":"2026-09-30T20:01:00Z","delay":42}]}
		}}`))
	})

	got := m.ProxyDelays()
	if got["🏠 JP-家宽-01"] != 1200 {
		t.Errorf("JP 延迟 = %d, 期望取最后一次 1200", got["🏠 JP-家宽-01"])
	}
	if v, ok := got["🏠 KR-家宽-01"]; !ok || v != 0 {
		t.Errorf("KR 应当返回 0（测过但不通），得到 %v/%v", v, ok)
	}
	// 策略组和普通节点都不是家宽节点，不能污染界面
	if _, ok := got["🏠 家宽节点"]; ok {
		t.Error("策略组不应出现在家宽节点延迟里")
	}
	if _, ok := got["优选域名-01"]; ok {
		t.Error("非家宽节点不应出现在家宽节点延迟里")
	}
}

// 内核没跑（端口不通）时必须安静地返回 nil，不能 panic、不能报错刷屏
func TestProxyDelaysSilentWhenKernelDown(t *testing.T) {
	m := NewManager(t.TempDir())
	m.mu.Lock()
	m.nodes = []string{"🏠 JP-家宽-01"}
	m.ctrlAddr = "127.0.0.1:1" // 几乎必然连不上
	m.mu.Unlock()

	if got := m.ProxyDelays(); got != nil {
		t.Errorf("内核不可用时应返回 nil, 得到 %v", got)
	}
}

// 单节点测速：内核返回 delay 就照实转成毫秒
func TestProxyDelayOK(t *testing.T) {
	var gotQuery string
	m := newFakeKernel(t, func(rw http.ResponseWriter, r *http.Request) {
		// 节点名带 emoji 和空格，必须正确转义后再拼进路径
		if r.URL.Path != "/proxies/🏠 JP-家宽-01/delay" {
			t.Errorf("路径 = %q", r.URL.Path)
		}
		gotQuery = r.URL.RawQuery
		_, _ = rw.Write([]byte(`{"delay":1234}`))
	})

	ms, err := m.ProxyDelay("🏠 JP-家宽-01", 8000)
	if err != nil {
		t.Fatalf("测速失败: %v", err)
	}
	if ms != 1234 {
		t.Errorf("延迟 = %d, 期望 1234", ms)
	}
	if !strings.Contains(gotQuery, "timeout=8000") {
		t.Errorf("超时未透传: %q", gotQuery)
	}
	if !strings.Contains(gotQuery, "generate_204") {
		t.Errorf("测速地址不对: %q", gotQuery)
	}
}

// 测不通时内核返回 4xx + message，必须把 message 原样带出来给用户看
func TestProxyDelayReportsKernelMessage(t *testing.T) {
	m := newFakeKernel(t, func(rw http.ResponseWriter, r *http.Request) {
		rw.WriteHeader(http.StatusRequestTimeout)
		_, _ = rw.Write([]byte(`{"message":"An error occurred in the delay test"}`))
	})

	ms, err := m.ProxyDelay("🏠 JP-家宽-01", 8000)
	if err == nil {
		t.Fatal("测不通时必须返回 error")
	}
	if ms != 0 {
		t.Errorf("失败时延迟应为 0, 得到 %d", ms)
	}
	if !strings.Contains(err.Error(), "delay test") {
		t.Errorf("内核给的原因被吞掉了: %v", err)
	}
}

// 内核返回 200 但 delay<=0（节点无响应）也要当成失败，不能显示成「0ms」
func TestProxyDelayZeroMeansUnreachable(t *testing.T) {
	m := newFakeKernel(t, func(rw http.ResponseWriter, r *http.Request) {
		_, _ = rw.Write([]byte(`{"delay":0}`))
	})

	if _, err := m.ProxyDelay("🏠 JP-家宽-01", 8000); err == nil {
		t.Error("delay=0 应当视为无响应")
	}
}

// 整组测速：内核返回的是 map，非家宽节点要被过滤掉
func TestGroupDelayFiltersNonClashNodes(t *testing.T) {
	m := newFakeKernel(t, func(rw http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/group/🏠 家宽节点/delay" {
			t.Errorf("路径 = %q", r.URL.Path)
		}
		_, _ = rw.Write([]byte(`{
			"🏠 JP-家宽-01":1800,
			"🏠 KR-家宽-01":0,
			"优选域名-01":42
		}`))
	})

	got, err := m.GroupDelay("🏠 家宽节点", 8000)
	if err != nil {
		t.Fatalf("整组测速失败: %v", err)
	}
	if got["🏠 JP-家宽-01"] != 1800 {
		t.Errorf("JP 延迟 = %d, 期望 1800", got["🏠 JP-家宽-01"])
	}
	if v, ok := got["🏠 KR-家宽-01"]; !ok || v != 0 {
		t.Errorf("KR 应当保留 0 值（表示测不通）: %v/%v", v, ok)
	}
	if _, ok := got["优选域名-01"]; ok {
		t.Error("非家宽节点混进了整组测速结果")
	}
}

// 没识别出手动选择组时（比如订阅格式变了）要明确报错，而不是默默返回空结果
func TestGroupDelayWithoutNodeGroup(t *testing.T) {
	m := newFakeKernel(t, func(rw http.ResponseWriter, r *http.Request) {
		t.Error("组名为空时不应发起请求")
	})
	m.mu.Lock()
	m.nodeGroup = ""
	m.mu.Unlock()

	if _, err := m.GroupDelay("", 8000); err == nil {
		t.Error("组名为空时必须报错")
	}
}

// NodeGroup 要能安全并发读（界面每次刷新都会调）
func TestNodeGroupAccessor(t *testing.T) {
	m := newFakeKernel(t, func(rw http.ResponseWriter, r *http.Request) {})
	if got := m.NodeGroup(); got != "🏠 家宽节点" {
		t.Errorf("NodeGroup() = %q", got)
	}
}

// --- 前置通道（家宽链的第一跳）相关测试 ---
//
// 全部家宽节点的出口都挤在前置通道组的同一个节点上。那个组在订阅里是
// url-test，内核按「它自己访问 gstatic 快不快」挑 —— 这个判据与「能不能
// 承载一条 OpenVPN 长连接」毫无关系，而且订阅没写 lazy、mihomo 默认
// lazy=true，选错了也永远不自己纠正。
//
// 所以测速时会拿真实家宽节点试出可用的前置并落盘，内核每次启动重放一遍。
// 不重放的话就是「测速时明明好好的，选上没一会儿就失效」—— 测速走探针、
// 用户点节点后跑内核，两边各自挑各自的前置。

// newFakeKernelWithSub 造一个带订阅原文与配置的假内核环境。
func newFakeKernelWithSub(t *testing.T, frontNode string) (*Manager, *config.Config, *httptest.Server, *string) {
	t.Helper()
	dir := t.TempDir()
	cfg, err := config.Load(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatalf("加载配置失败: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, subFileNameFor("g1")), []byte(sampleSub), 0644); err != nil {
		t.Fatalf("写入订阅原文失败: %v", err)
	}
	cfg.Lock()
	cfg.Groups = []config.Group{{
		ID: "g1", Name: "家宽", Kind: config.GroupKindClash,
		Nodes: []config.Node{}, FrontNode: frontNode,
	}}
	cfg.Unlock()

	var mu sync.Mutex
	lastPath := new(string)
	fake := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		mu.Lock()
		// 记原始请求行：r.URL.Path 已被服务端解码，验不了「组名里的 emoji
		// 与空格有没有被正确转义」。
		*lastPath = r.RequestURI
		mu.Unlock()
		rw.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(fake.Close)

	m := NewManager(dir)
	m.SetConfig(cfg)
	m.mu.Lock()
	m.loadedGroup = "g1"
	m.nodes = []string{"🏠 JP-家宽-01", "🏠 KR-家宽-01"}
	m.nodeGroup = "🏠 家宽节点"
	m.topGroup = "🚀 节点选择"
	m.ctrlAddr = strings.TrimPrefix(fake.URL, "http://")
	m.mu.Unlock()
	return m, cfg, fake, lastPath
}

// 测速验过的前置必须在内核启动后重放 —— 否则内核自己挑的那个（判据是错的）
// 会把用户选中的家宽节点一起带死。
func TestApplyFrontReplaysPinnedFront(t *testing.T) {
	m, _, _, lastPath := newFakeKernelWithSub(t, "联通-09")

	if err := m.applyFront(); err != nil {
		t.Fatalf("applyFront 失败: %v", err)
	}
	// 组名带 emoji 与空格，必须转义后再拼进路径
	want := "/proxies/" + url.PathEscape("⚡ CF前置")
	if got := *lastPath; got != want {
		t.Errorf("请求行 = %q, 期望 %q", got, want)
	}
}

// 从没测出过可用的前置时不能乱动：让内核按订阅的自动选择走。
func TestApplyFrontNoopWithoutPin(t *testing.T) {
	m, _, _, lastPath := newFakeKernelWithSub(t, "")

	if err := m.applyFront(); err != nil {
		t.Fatalf("applyFront 失败: %v", err)
	}
	if *lastPath != "" {
		t.Errorf("没有记录时不该发起任何请求, 实际 %q", *lastPath)
	}
}

// 记录的那个节点已经不在订阅里（订阅换节点很频繁）时也要安静地跳过，
// 否则 PUT 会被内核拒绝、日志里刷一条看不懂的错。
func TestApplyFrontSkipsStalePin(t *testing.T) {
	m, _, _, lastPath := newFakeKernelWithSub(t, "早就没了的节点")

	if err := m.applyFront(); err != nil {
		t.Fatalf("applyFront 失败: %v", err)
	}
	if *lastPath != "" {
		t.Errorf("记录已失效时不该发起请求, 实际 %q", *lastPath)
	}
}

// FrontInfo 一次把组名和成员都给出来 —— 测速流程两样都要，解析一遍就够。
func TestFrontInfoReturnsNameAndMembers(t *testing.T) {
	m, _, _, _ := newFakeKernelWithSub(t, "")

	name, members := m.FrontInfo("g1")
	if name != "⚡ CF前置" {
		t.Errorf("组名 = %q", name)
	}
	if len(members) != 2 || members[0] != "优选域名-01" {
		t.Errorf("成员 = %v", members)
	}
	if n, mem := m.FrontInfo("不存在"); n != "" || mem != nil {
		t.Errorf("分组不存在时应返回零值, 得到 %q/%v", n, mem)
	}
}

// SetFront 必须把节点名放进请求体（内核侧靠它做 ForceSet）。
func TestSetFrontSendsName(t *testing.T) {
	var gotBody string
	fake := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		rw.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(fake.Close)

	m := NewManager(t.TempDir())
	m.mu.Lock()
	m.ctrlAddr = strings.TrimPrefix(fake.URL, "http://")
	m.mu.Unlock()

	if err := m.SetFront("⚡ CF前置", "联通-09"); err != nil {
		t.Fatalf("SetFront 失败: %v", err)
	}
	if !strings.Contains(gotBody, "联通-09") {
		t.Errorf("请求体 = %q, 期望含节点名", gotBody)
	}
}

// restoreSelection 必须把「用户选的节点」和「测速验过的前置」两样都重放，
// 而且节点先、前置后 —— 前置是节点能通的先决条件，顺序反了会出现一小段
// 「节点已切过去但前置还是错的」窗口。
func TestRestoreSelectionReplaysNodeThenFront(t *testing.T) {
	m, _, _, _ := newFakeKernelWithSub(t, "联通-09")

	var mu sync.Mutex
	var calls []string
	fake := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		mu.Lock()
		calls = append(calls, r.RequestURI)
		mu.Unlock()
		rw.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(fake.Close)
	m.mu.Lock()
	m.ctrlAddr = strings.TrimPrefix(fake.URL, "http://")
	m.mu.Unlock()

	m.restoreSelection("🏠 KR-家宽-01")

	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 3 {
		t.Fatalf("应当有 3 次 PUT（手动组、顶层组、前置组）, 实际 %v", calls)
	}
	if !strings.Contains(calls[0], url.PathEscape("🏠 家宽节点")) {
		t.Errorf("第 1 次应当切手动选择组, 实际 %q", calls[0])
	}
	if !strings.Contains(calls[1], url.PathEscape("🚀 节点选择")) {
		t.Errorf("第 2 次应当切顶层主组, 实际 %q", calls[1])
	}
	if !strings.Contains(calls[2], url.PathEscape("⚡ CF前置")) {
		t.Errorf("第 3 次应当重放前置, 实际 %q", calls[2])
	}
}
