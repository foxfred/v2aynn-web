package v2ray

import (
	"reflect"
	"testing"

	"v2aynn-web/internal/config"
)

func testNode() *config.Node {
	return &config.Node{
		ID: "n1", Name: "测试节点", Protocol: "trojan",
		Server: "example.com", Port: "443", Password: "pw",
	}
}

// routingRules 取出 generateConfig 结果里的 routing.rules。
// 返回 nil 表示配置中不存在 routing（global 模式的预期形态）。
func routingRules(cfg map[string]interface{}) []interface{} {
	rt, ok := cfg["routing"].(map[string]interface{})
	if !ok {
		return nil
	}
	rules, _ := rt["rules"].([]interface{})
	return rules
}

// hasDirectRuleMatching 判断是否存在一条指向 direct 出站、且匹配给定字段的规则。
func hasDirectRuleMatching(rules []interface{}, key string) bool {
	for _, r := range rules {
		m, ok := r.(map[string]interface{})
		if !ok {
			continue
		}
		if m["outboundTag"] != "direct" {
			continue
		}
		if _, ok := m[key]; ok {
			return true
		}
	}
	return false
}

func TestGenerateConfigSmartRoutesCNThroughDirect(t *testing.T) {
	rules := routingRules(generateConfig(testNode(), 10808, 10810, "smart"))
	if len(rules) == 0 {
		t.Fatal("smart 模式应当生成 routing 规则")
	}
	if !hasDirectRuleMatching(rules, "domain") {
		t.Error("smart 模式缺少按域名分流的 geosite:cn 直连规则")
	}
	if !hasDirectRuleMatching(rules, "ip") {
		t.Error("smart 模式缺少按 IP 分流的 geoip:cn 直连规则")
	}
}

func TestGenerateConfigGlobalHasNoRouting(t *testing.T) {
	cfg := generateConfig(testNode(), 10808, 10810, "global")
	if _, ok := cfg["routing"]; ok {
		t.Error("global 模式不应生成 routing —— 不配 routing 时 xray 默认全部走第一个出站（代理）")
	}
}

func TestGenerateConfigDirectRoutesEverythingDirect(t *testing.T) {
	cfg := generateConfig(testNode(), 10808, 10810, "direct")
	rules := routingRules(cfg)
	if len(rules) != 1 {
		t.Fatalf("direct 模式应当只有一条兜底规则，实际 %d 条", len(rules))
	}
	m, ok := rules[0].(map[string]interface{})
	if !ok {
		t.Fatal("规则不是 map")
	}
	if m["outboundTag"] != "direct" {
		t.Errorf("direct 模式兜底规则的出站应为 direct，实际 %v", m["outboundTag"])
	}
	// 兜底规则不能带 domain/ip 匹配条件，否则就不是"全部直连"
	if _, ok := m["domain"]; ok {
		t.Error("direct 模式的兜底规则不应带 domain 条件")
	}
	if _, ok := m["ip"]; ok {
		t.Error("direct 模式的兜底规则不应带 ip 条件")
	}
	// 必须同时覆盖 TCP 与 UDP，否则 UDP 流量仍会走代理
	if m["network"] != "tcp,udp" {
		t.Errorf("direct 兜底规则应覆盖 tcp,udp，实际 %v", m["network"])
	}
}

// TestGenerateConfigDirectDiffersFromSmart 是本缺陷的回归测试。
//
// 历史缺陷：generateConfig 只判断 `proxyMode != "global"`，direct 因此落进
// 与 smart 完全相同的分支 —— 界面选「直连」，实际仍在按 geosite:cn/geoip:cn
// 分流，国外流量照旧走代理节点。功能看起来"有"，实际是空操作。
//
// 只断言"direct 有 routing"是不够的（smart 也有）。必须断言两者**不相等**，
// 否则一旦有人把 direct 重新并回默认分支，测试仍会通过。
func TestGenerateConfigDirectDiffersFromSmart(t *testing.T) {
	direct := generateConfig(testNode(), 10808, 10810, "direct")
	smart := generateConfig(testNode(), 10808, 10810, "smart")

	dr := routingRules(direct)
	sr := routingRules(smart)
	if reflect.DeepEqual(dr, sr) {
		t.Fatal("direct 与 smart 生成了相同的 routing —— 「直连」又退化成了空操作")
	}
	if hasDirectRuleMatching(dr, "domain") || hasDirectRuleMatching(dr, "ip") {
		t.Error("direct 模式不应包含按域名/IP 分流的规则（那是 smart 的行为）")
	}
	if !hasDirectRuleMatching(sr, "domain") {
		t.Error("smart 模式应当保留 geosite:cn 分流规则")
	}
}

func TestGenerateConfigUnknownModeFallsBackToSmart(t *testing.T) {
	unknown := routingRules(generateConfig(testNode(), 10808, 10810, "whatever"))
	smart := routingRules(generateConfig(testNode(), 10808, 10810, "smart"))
	if !reflect.DeepEqual(unknown, smart) {
		t.Error("未知模式应回落到 smart 行为")
	}
}

// 三种模式都必须保留 freedom(direct) 出站，否则 routing 里的 direct 标签会指向不存在的出站，
// xray 启动即报错。
func TestGenerateConfigAlwaysKeepsDirectOutbound(t *testing.T) {
	for _, mode := range []string{"smart", "global", "direct"} {
		cfg := generateConfig(testNode(), 10808, 10810, mode)
		outs, ok := cfg["outbounds"].([]interface{})
		if !ok {
			t.Fatalf("%s: outbounds 缺失或类型错误", mode)
		}
		found := false
		for _, o := range outs {
			if m, ok := o.(map[string]interface{}); ok && m["tag"] == "direct" {
				found = true
			}
		}
		if !found {
			t.Errorf("%s: 缺少 tag=direct 的 freedom 出站", mode)
		}
	}
}

// 端口必须按传入值落到 inbounds 上，防止改分流逻辑时误伤端口。
func TestGenerateConfigAppliesPorts(t *testing.T) {
	cfg := generateConfig(testNode(), 12345, 12346, "smart")
	ins, ok := cfg["inbounds"].([]interface{})
	if !ok {
		t.Fatal("inbounds 缺失或类型错误")
	}
	var socks, http bool
	for _, in := range ins {
		m, ok := in.(map[string]interface{})
		if !ok {
			continue
		}
		switch m["protocol"] {
		case "socks":
			socks = m["port"] == 12345
		case "http":
			http = m["port"] == 12346
		}
	}
	if !socks {
		t.Error("socks 入站端口未使用传入值")
	}
	if !http {
		t.Error("http 入站端口未使用传入值")
	}
}
