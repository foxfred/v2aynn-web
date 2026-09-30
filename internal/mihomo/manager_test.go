package mihomo

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
	out, err := rewriteConfig(sampleSub, 10808, 10810, 19090)
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
	once, err := rewriteConfig(sampleSub, 10808, 10810, 19090)
	if err != nil {
		t.Fatalf("第一次改写失败: %v", err)
	}
	twice, err := rewriteConfig(once, 10808, 10810, 19090)
	if err != nil {
		t.Fatalf("第二次改写失败: %v", err)
	}
	if once != twice {
		t.Errorf("改写不幂等:\n第一次行数=%d 第二次行数=%d",
			strings.Count(once, "\n"), strings.Count(twice, "\n"))
	}
}

func TestRewriteConfigNoProxies(t *testing.T) {
	if _, err := rewriteConfig("mode: rule\n", 10808, 10810, 19090); err == nil {
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

	out, err := rewriteConfig(string(b), 10808, 10810, 19090)
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
