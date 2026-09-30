package config

import (
	"os"
	"path/filepath"
	"testing"
)

func writeConfigFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(body), 0644); err != nil {
		t.Fatalf("写入测试配置失败: %v", err)
	}
	return path
}

// 老版本把家宽订阅地址存在设置面板的 ClashSubURL 里。现在改成普通订阅分组
// 承载，加载时必须自动迁移 —— 用户填过的地址不能让他重填一遍。
//
// 而且他多半在建分组时已经手动填过一次同样的地址（那是最自然的试探方式），
// 迁移要把那个分组直接转正，而不是再补一个一模一样的条目。
func TestMigrateClashSubURLReusesExistingGroup(t *testing.T) {
	path := writeConfigFile(t, `{
	  "clashSubUrl": "https://example.org/sub?target=vg",
	  "groups": [
	    {"id": "g1", "name": "我的家宽", "url": "https://example.org/sub?target=vg", "nodes": []}
	  ]
	}`)

	c, err := Load(path)
	if err != nil {
		t.Fatalf("加载配置失败: %v", err)
	}

	if c.ClashSubURL != "" {
		t.Errorf("ClashSubURL = %q, 迁移后应当清空", c.ClashSubURL)
	}
	g := c.FindClashGroup("g1")
	if g == nil {
		t.Fatal("同地址的已有分组没有被转正")
	}
	if g.Name != "我的家宽" {
		t.Errorf("分组名 = %q, 迁移不该改名", g.Name)
	}
	// 只应有一个家宽分组，不能额外造一个重复的
	if n := len(c.ClashGroups()); n != 1 {
		t.Errorf("家宽分组数 = %d, 期望 1", n)
	}
}

// 没有同地址的分组时，迁移要新建一个，名字就叫「家宽」
func TestMigrateClashSubURLCreatesNewGroup(t *testing.T) {
	path := writeConfigFile(t, `{
	  "clashSubUrl": "https://example.org/sub?target=vg",
	  "groups": [
	    {"id": "g1", "name": "普通订阅", "url": "https://other.example.org/sub", "nodes": []}
	  ]
	}`)

	c, err := Load(path)
	if err != nil {
		t.Fatalf("加载配置失败: %v", err)
	}

	if c.ClashSubURL != "" {
		t.Errorf("ClashSubURL = %q, 迁移后应当清空", c.ClashSubURL)
	}
	groups := c.ClashGroups()
	if len(groups) != 1 {
		t.Fatalf("家宽分组数 = %d, 期望 1", len(groups))
	}
	if groups[0].URL != "https://example.org/sub?target=vg" {
		t.Errorf("新建家宽分组的地址 = %q", groups[0].URL)
	}
	if groups[0].Name != "家宽" {
		t.Errorf("新建家宽分组名 = %q, 期望「家宽」", groups[0].Name)
	}
	// 普通分组不能被误伤
	if c.FindClashGroup("g1") != nil {
		t.Error("普通分组被误标成了家宽")
	}
}

// 没有家宽地址的老配置不该被动手脚
func TestMigrateClashSubURLNoop(t *testing.T) {
	path := writeConfigFile(t, `{
	  "groups": [{"id": "g1", "name": "普通订阅", "url": "https://a.example.org/sub", "nodes": []}]
	}`)

	c, err := Load(path)
	if err != nil {
		t.Fatalf("加载配置失败: %v", err)
	}
	if n := len(c.ClashGroups()); n != 0 {
		t.Errorf("不该凭空出现家宽分组，得到 %d 个", n)
	}
}

// 家宽节点在界面上是 "clash:<分组ID>:<节点名>"。分组 ID 是纯数字（UnixNano），
// 节点名则可能带各种符号 —— 解析必须按「第一个冒号」切分，不能按最后一个。
func TestParseClashNodeID(t *testing.T) {
	cases := []struct {
		name  string
		id    string
		gid   string
		node  string
		valid bool
	}{
		{"常规", ClashNodeID("1712345678901234567", "🏠 JP-家宽-01"), "1712345678901234567", "🏠 JP-家宽-01", true},
		{"节点名含冒号", ClashNodeID("123", "a:b:c"), "123", "a:b:c", true},
		{"节点名含斜杠与空格", ClashNodeID("123", "香港 01/A"), "123", "香港 01/A", true},
		{"只有前缀", "clash:", "", "", false},
		{"缺节点名", "clash:123", "", "", false},
		{"缺分组", ClashNodeIDPrefix + ":name", "", "", false},
		{"普通节点ID", "8f3c1b2a-0000-0000-0000-000000000000", "", "", false},
		{"空串", "", "", "", false},
		{"前缀不对", "clasher:1:2", "", "", false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gid, node, ok := ParseClashNodeID(c.id)
			if ok != c.valid {
				t.Fatalf("ParseClashNodeID(%q) ok = %v, 期望 %v", c.id, ok, c.valid)
			}
			if !c.valid {
				return
			}
			if gid != c.gid || node != c.node {
				t.Errorf("ParseClashNodeID(%q) = (%q, %q), 期望 (%q, %q)",
					c.id, gid, node, c.gid, c.node)
			}
		})
	}
}

// ClashNodeID 与 ParseClashNodeID 必须互为逆运算，否则「点节点」会定位错分组
func TestClashNodeIDRoundTrip(t *testing.T) {
	for _, gid := range []string{"1", "1712345678901234567", "__default__"} {
		for _, node := range []string{"🏠 JP-家宽-01", "a:b", "带 空格 的名字", "中文"} {
			id := ClashNodeID(gid, node)
			gotG, gotN, ok := ParseClashNodeID(id)
			if !ok || gotG != gid || gotN != node {
				t.Errorf("往返失败: ClashNodeID(%q,%q)=%q -> (%q,%q,%v)",
					gid, node, id, gotG, gotN, ok)
			}
		}
	}
}

// ActiveClashGroup 决定「该加载哪一份配置」：优先用用户点过的那份，
// 它被删了就回落到第一个家宽分组，一个都没有时返回 nil。
func TestActiveClashGroupFallback(t *testing.T) {
	c := &Config{Groups: []Group{
		{ID: "n1", Name: "普通", URL: "https://a/sub"},
		{ID: "c1", Name: "家宽A", URL: "https://a/vg", Kind: GroupKindClash},
		{ID: "c2", Name: "家宽B", URL: "https://b/vg", Kind: GroupKindClash},
	}}

	if g := c.ActiveClashGroup(); g == nil || g.ID != "c1" {
		t.Errorf("未选过时应当回落到第一个家宽分组, 得到 %v", g)
	}

	c.ActiveGrp = "c2"
	if g := c.ActiveClashGroup(); g == nil || g.ID != "c2" {
		t.Errorf("选过之后应当用选中的那个, 得到 %v", g)
	}

	// ActiveGrp 指向一个普通分组时也要回落到家宽分组，
	// 否则用户点完普通节点再点家宽节点会找不到配置
	c.ActiveGrp = "n1"
	if g := c.ActiveClashGroup(); g == nil || g.ID != "c1" {
		t.Errorf("ActiveGrp 指向普通分组时应当回落, 得到 %v", g)
	}

	c.ActiveGrp = "gone"
	if g := c.ActiveClashGroup(); g == nil || g.ID != "c1" {
		t.Errorf("ActiveGrp 指向已删分组时应当回落, 得到 %v", g)
	}
}

// 家宽分组的节点恒为空，绝不能混进 AllNodes()：
// 那些是 openvpn 节点，被 xray 的故障转移选中后必然连不上。
func TestClashGroupNodesStayOutOfAllNodes(t *testing.T) {
	c := &Config{Groups: []Group{
		{ID: "c1", Name: "家宽", URL: "https://a/vg", Kind: GroupKindClash, Nodes: nil},
		{ID: "g1", Name: "普通", Nodes: []Node{{ID: "n1"}, {ID: "n2"}}},
	}}
	if n := len(c.AllNodes()); n != 2 {
		t.Errorf("AllNodes 数量 = %d, 期望 2（家宽分组不该贡献节点）", n)
	}
	if !c.ClashEnabled() {
		t.Error("ClashEnabled 应当为 true")
	}
	if g := c.FindClashGroup("g1"); g != nil {
		t.Error("普通分组不该被 FindClashGroup 命中")
	}
}
