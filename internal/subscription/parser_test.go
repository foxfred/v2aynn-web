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
