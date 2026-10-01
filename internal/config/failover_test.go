package config

import "testing"

// 替补候选的排序标准：可达（Ping>0）的按延迟升序排前面，
// 未测（0）与不可达（-1）保持原顺序排在后面。
//
// xray 的进程看护和 mihomo 的节点看护共用这一份实现 —— 两处各写一份迟早会漂移，
// 表现为「进程看护挑的和节点看护挑的不是同一个」。
func TestFailoverOrderPrefersReachableThenLatency(t *testing.T) {
	all := []Node{
		{ID: "a", Name: "未测", Ping: 0},
		{ID: "b", Name: "慢", Ping: 300},
		{ID: "c", Name: "快", Ping: 80},
		{ID: "d", Name: "不通", Ping: -1},
	}
	got := FailoverOrder(all, "", nil)
	want := []string{"c", "b", "a", "d"}
	if len(got) != len(want) {
		t.Fatalf("候选数 = %d, 期望 %d", len(got), len(want))
	}
	for i := range want {
		if got[i].ID != want[i] {
			t.Errorf("第 %d 个候选 = %q, 期望 %q（整体 %v）", i, got[i].ID, want[i], ids(got))
		}
	}
}

func TestFailoverOrderExcludesCurrentAndTried(t *testing.T) {
	all := []Node{
		{ID: "a", Ping: 10},
		{ID: "b", Ping: 20},
		{ID: "c", Ping: 30},
	}
	got := FailoverOrder(all, "a", map[string]bool{"b": true})
	if len(got) != 1 || got[0].ID != "c" {
		t.Errorf("排除当前节点与试过的节点后应当只剩 c，实际 %v", ids(got))
	}
}

func TestFailoverOrderEmptyWhenAllExcluded(t *testing.T) {
	all := []Node{{ID: "a", Ping: 10}, {ID: "b", Ping: 20}}
	got := FailoverOrder(all, "a", map[string]bool{"b": true})
	if len(got) != 0 {
		t.Errorf("全被排除时应当返回空，实际 %v", ids(got))
	}
}

func ids(ns []Node) []string {
	out := make([]string, 0, len(ns))
	for _, n := range ns {
		out = append(out, n.ID)
	}
	return out
}
