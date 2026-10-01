package subscription

import (
	"testing"

	"v2aynn-web/internal/config"
)

// TestDedupNodesRemovesDuplicates 同一 server:port:protocol 只保留第一个。
func TestDedupNodesRemovesDuplicates(t *testing.T) {
	in := []config.Node{
		{ID: "a", Name: "节点A", Server: "1.2.3.4", Port: "443", Protocol: "trojan", Ping: 50},
		{ID: "b", Name: "节点B", Server: "1.2.3.4", Port: "443", Protocol: "trojan", Ping: 99},
		{ID: "c", Name: "节点C", Server: "5.6.7.8", Port: "443", Protocol: "trojan"},
	}

	out := dedupNodes(in)

	if len(out) != 2 {
		t.Fatalf("去重后应有 2 个节点，实际 %d 个: %+v", len(out), out)
	}
	// 保留的应是第一个出现的（含其 ping 值），而不是后出现的
	if out[0].ID != "a" {
		t.Errorf("应保留第一个出现的节点，实际保留 %q", out[0].ID)
	}
	if out[0].Ping != 50 {
		t.Errorf("应保留第一个节点的 ping=50，实际 %d", out[0].Ping)
	}
	if out[1].ID != "c" {
		t.Errorf("第二个不同节点应保留 %q，实际 %q", "c", out[1].ID)
	}
}

// TestDedupNodesDistinguishesByAllThreeFields 三个字段任一不同都算不同节点。
// 只按 server 去重会误删合法节点，是最容易犯的错。
func TestDedupNodesDistinguishesByAllThreeFields(t *testing.T) {
	cases := []struct {
		name string
		a, b config.Node
	}{
		{
			name: "端口不同",
			a:    config.Node{Server: "1.2.3.4", Port: "443", Protocol: "vless"},
			b:    config.Node{Server: "1.2.3.4", Port: "80", Protocol: "vless"},
		},
		{
			name: "协议不同",
			a:    config.Node{Server: "1.2.3.4", Port: "443", Protocol: "vless"},
			b:    config.Node{Server: "1.2.3.4", Port: "443", Protocol: "trojan"},
		},
		{
			name: "服务器不同",
			a:    config.Node{Server: "1.2.3.4", Port: "443", Protocol: "vless"},
			b:    config.Node{Server: "5.6.7.8", Port: "443", Protocol: "vless"},
		},
	}

	for _, tc := range cases {
		out := dedupNodes([]config.Node{tc.a, tc.b})
		if len(out) != 2 {
			t.Errorf("%s: 应保留 2 个节点，实际 %d 个（误判为重复）", tc.name, len(out))
		}
	}
}

// TestDedupNodesPreservesOrder 去重后保持原有出现顺序（列表顺序对用户可见）。
func TestDedupNodesPreservesOrder(t *testing.T) {
	in := []config.Node{
		{ID: "1", Server: "a.com", Port: "443", Protocol: "vless"},
		{ID: "2", Server: "b.com", Port: "443", Protocol: "vless"},
		{ID: "3", Server: "a.com", Port: "443", Protocol: "vless"}, // 重复
		{ID: "4", Server: "c.com", Port: "443", Protocol: "vless"},
	}

	out := dedupNodes(in)

	want := []string{"1", "2", "4"}
	if len(out) != len(want) {
		t.Fatalf("应有 %d 个节点，实际 %d 个", len(want), len(out))
	}
	for i, id := range want {
		if out[i].ID != id {
			t.Errorf("第 %d 个节点应为 %q，实际 %q（顺序被打乱）", i, id, out[i].ID)
		}
	}
}

// TestDedupNodesEdgeCases 空列表与单元素列表不应出问题。
func TestDedupNodesEdgeCases(t *testing.T) {
	if out := dedupNodes(nil); len(out) != 0 {
		t.Errorf("nil 输入应返回空切片，实际 %d 个", len(out))
	}
	if out := dedupNodes([]config.Node{}); len(out) != 0 {
		t.Errorf("空切片输入应返回空切片，实际 %d 个", len(out))
	}

	one := []config.Node{{ID: "x", Server: "1.1.1.1", Port: "443", Protocol: "vmess"}}
	out := dedupNodes(one)
	if len(out) != 1 || out[0].ID != "x" {
		t.Errorf("单元素输入应原样返回，实际 %+v", out)
	}
}

// TestDedupNodesAllSame 全部重复时只留一个，且不返回 nil。
func TestDedupNodesAllSame(t *testing.T) {
	in := []config.Node{
		{ID: "1", Server: "s", Port: "443", Protocol: "vless"},
		{ID: "2", Server: "s", Port: "443", Protocol: "vless"},
		{ID: "3", Server: "s", Port: "443", Protocol: "vless"},
	}

	out := dedupNodes(in)

	if len(out) != 1 {
		t.Fatalf("全部重复时应只剩 1 个，实际 %d 个", len(out))
	}
	if out[0].ID != "1" {
		t.Errorf("应保留第一个，实际 %q", out[0].ID)
	}
}

// TestDedupNodesDoesNotMutateInput 去重不应修改传入切片
// （调用方可能仍持有原切片，被就地改写会造成难以排查的问题）。
func TestDedupNodesDoesNotMutateInput(t *testing.T) {
	in := []config.Node{
		{ID: "1", Server: "a", Port: "443", Protocol: "vless"},
		{ID: "2", Server: "a", Port: "443", Protocol: "vless"},
		{ID: "3", Server: "b", Port: "443", Protocol: "vless"},
	}

	_ = dedupNodes(in)

	if len(in) != 3 {
		t.Errorf("传入切片长度被改动：期望 3，实际 %d", len(in))
	}
	if in[1].ID != "2" || in[2].ID != "3" {
		t.Errorf("传入切片内容被就地改写: %+v", in)
	}
}

// ---- carryOver：订阅刷新时把节点身份与测速结果接过去 ----
//
// 这一组用例的背景（2026-10-01 真机反馈）：
//   ① 「连接中的节点会自己变」—— 每次刷新节点 ID 都重新生成，重启时
//      配置里记的 ActiveNode 指向的 ID 已不存在，于是被换成列表第一个；
//   ② 「已经测速的节点，订阅刷新后就没了」。
// 修法就是 carryOver：按稳定身份把旧节点的 ID / Ping / Speed 接到新节点上。

// carryOld 造一个「上一次刷新的结果」：id-1 测到 88ms、id-2 测到 -1（超时）。
func carryOld() []config.Node {
	return []config.Node{
		{ID: "id-1", Name: "香港-01", Protocol: "vless", Server: "a.com", Port: "443",
			UUID: "uuid-a", Ping: 88, Speed: 12.5},
		{ID: "id-2", Name: "日本-01", Protocol: "vless", Server: "b.com", Port: "443",
			UUID: "uuid-b", Ping: -1},
	}
}

// carryFresh 造一个「这一次刷新的结果」：内容一样，但 ID 是重新生成的
// （真实路径里就是 config.NewUUID()），名字被订阅源改了一个。
func carryFresh() []config.Node {
	return []config.Node{
		{ID: "new-1", Name: "香港-01", Protocol: "vless", Server: "a.com", Port: "443", UUID: "uuid-a"},
		{ID: "new-2", Name: "日本-东京", Protocol: "vless", Server: "b.com", Port: "443", UUID: "uuid-b"},
	}
}

// 同一个节点（服务器+凭证都没变）刷新后必须沿用旧 ID 和测速结果。
// ID 沿用了，配置里的 ActiveNode 才不会失效 —— 这是「节点不再自己变」的关键。
func TestCarryOverKeepsIDAndMeasurements(t *testing.T) {
	out := carryOver(carryOld(), carryFresh())

	if len(out) != 2 {
		t.Fatalf("节点数应为 2，实际 %d", len(out))
	}
	if out[0].ID != "id-1" {
		t.Errorf("同凭证节点的 ID 应当沿用 id-1，实际 %q（ID 变了会让 ActiveNode 失效）", out[0].ID)
	}
	if out[0].Ping != 88 || out[0].Speed != 12.5 {
		t.Errorf("测速结果应当沿用 ping=88/speed=12.5，实际 ping=%d/speed=%v", out[0].Ping, out[0].Speed)
	}
	// -1 表示「测过但不通」，也必须跟着走，否则界面上的「超时」会变回空白
	if out[1].ID != "id-2" {
		t.Errorf("改名（日本-01 → 日本-东京）不该改变身份，期望 id-2，实际 %q", out[1].ID)
	}
	if out[1].Ping != -1 {
		t.Errorf("「超时」结果应当沿用 -1，实际 %d", out[1].Ping)
	}
	// 新列表里的名字以订阅为准，不能被旧名字盖回去
	if out[1].Name != "日本-东京" {
		t.Errorf("节点名应取订阅里的新值，实际 %q", out[1].Name)
	}
}

// 凭证被订阅源轮换、服务器没变时：测速结果要保住，但 ID 不能沿用
// （凭证变了就是另一个节点，沿用 ID 会让 ActiveNode 指向错的节点）。
func TestCarryOverKeepsMeasurementsWhenCredentialRotates(t *testing.T) {
	fresh := []config.Node{
		{ID: "new-1", Name: "香港-01", Protocol: "vless", Server: "a.com", Port: "443", UUID: "uuid-换过了"},
	}

	out := carryOver(carryOld(), fresh)

	if out[0].ID != "new-1" {
		t.Errorf("凭证变了不该沿用旧 ID，期望 new-1，实际 %q", out[0].ID)
	}
	if out[0].Ping != 88 || out[0].Speed != 12.5 {
		t.Errorf("服务器没变时测速结果应当保住，实际 ping=%d/speed=%v", out[0].Ping, out[0].Speed)
	}
}

// 旧列表里同一严格键出现多次时不许沿用 ID —— 否则两个节点共用一个 ID，
// FindNode / ActiveNode 都会命中错的那个。
func TestCarryOverRefusesAmbiguousID(t *testing.T) {
	old := []config.Node{
		{ID: "id-1", Protocol: "vless", Server: "a.com", Port: "443", UUID: "u"},
		{ID: "id-2", Protocol: "vless", Server: "a.com", Port: "443", UUID: "u"},
	}
	fresh := []config.Node{
		{ID: "new-1", Protocol: "vless", Server: "a.com", Port: "443", UUID: "u"},
	}

	out := carryOver(old, fresh)

	if out[0].ID != "new-1" {
		t.Errorf("身份有歧义时应当保留新 ID，实际 %q", out[0].ID)
	}
}

// 新出现的节点不该被误改：拿自己的新 ID，测速结果保持「没测过」。
func TestCarryOverLeavesBrandNewNodeAlone(t *testing.T) {
	fresh := []config.Node{
		{ID: "new-9", Name: "新加坡-01", Protocol: "vless", Server: "c.com", Port: "443", UUID: "uuid-c"},
	}

	out := carryOver(carryOld(), fresh)

	if out[0].ID != "new-9" {
		t.Errorf("全新节点的 ID 不该被改动，实际 %q", out[0].ID)
	}
	if out[0].Ping != 0 || out[0].Speed != 0 {
		t.Errorf("全新节点不该有测速结果，实际 ping=%d/speed=%v", out[0].Ping, out[0].Speed)
	}
}

// 严格键用「协议+服务器+端口+凭证」：端口不同就是不同节点，
// 不能因为服务器相同就把测速结果串过去。
func TestStrictKeyDistinguishesByPort(t *testing.T) {
	a := config.Node{Protocol: "vless", Server: "a.com", Port: "443", UUID: "u"}
	b := config.Node{Protocol: "vless", Server: "a.com", Port: "8443", UUID: "u"}
	if strictKey(a) == strictKey(b) {
		t.Errorf("端口不同不该算出同一个身份键: %q", strictKey(a))
	}
}
