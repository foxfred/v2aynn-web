package mihomo

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"v2aynn-web/internal/config"
)

// 内核统一（阶段二）之后，普通节点也由 mihomo 承载：
// 由 BuildNormalConfig 生成配置、写进与家宽共用的那个运行配置文件，
// 再靠 external-controller 拨策略组来切节点。
//
// 本文件覆盖「普通模式」这一侧的加载、切换与状态还原。
// 生成器本身的正确性由 nodes_test.go 覆盖。

func newNormalTestManager(t *testing.T) (*Manager, *config.Config, string) {
	t.Helper()
	dir := t.TempDir()
	cfg, err := config.Load(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatalf("加载配置失败: %v", err)
	}
	m := NewManager(dir)
	m.SetConfig(cfg)
	return m, cfg, dir
}

// plainNodes 三个普通节点，其中前两个**故意重名** ——
// 真实订阅里就是这样（802 个节点里 332 个重名），而 Clash 要求节点名唯一。
func plainNodes() []config.Node {
	return []config.Node{
		{ID: "n1", Name: "联通-01", Protocol: "trojan", Server: "1.1.1.1", Port: "443",
			Password: "p", Network: "ws", Path: "/?ed=2048", RequestHost: "a.example.com"},
		{ID: "n2", Name: "联通-01", Protocol: "vless", Server: "2.2.2.2", Port: "443",
			UUID: "u-1", Network: "ws", Path: "/?ed=2048"},
		{ID: "n3", Name: "移动-02", Protocol: "trojan", Server: "3.3.3.3", Port: "8443",
			Password: "p2"},
	}
}

func TestLoadNormalGeneratesConfigAndMaps(t *testing.T) {
	m, _, dir := newNormalTestManager(t)
	if err := m.LoadNormal(plainNodes(), "smart"); err != nil {
		t.Fatalf("生成普通节点配置失败: %v", err)
	}

	if !m.IsNormalMode() {
		t.Error("加载普通节点后应当进入普通模式")
	}
	// 普通模式没有家宽分组。留着旧值会让 Status() 拼出 "clash::节点名" 这种假 ID，
	// 界面高亮会串到不存在的行上。
	if g := m.LoadedGroup(); g != "" {
		t.Errorf("普通模式下不该挂着家宽分组: %q", g)
	}

	// 运行配置必须真的落盘 —— Start() 读的就是这个文件
	b, err := os.ReadFile(filepath.Join(dir, confFileName))
	if err != nil {
		t.Fatalf("运行配置没写出来: %v", err)
	}
	for _, want := range []string{"proxies:", "proxy-groups:", "rules:", "GEOSITE,cn,DIRECT"} {
		if !bytes.Contains(b, []byte(want)) {
			t.Errorf("配置里缺少 %q", want)
		}
	}

	// 重名节点必须被改名，且两个名字各自映射回自己的节点 ID
	m.mu.Lock()
	ref1, ref2, ref3 := m.normalByID["n1"], m.normalByID["n2"], m.normalByID["n3"]
	nodeGroup, topGroup, names := m.nodeGroup, m.topGroup, append([]string(nil), m.nodes...)
	m.mu.Unlock()

	if ref1.clash != "联通-01" {
		t.Errorf("n1 的配置名 = %q, 期望原名", ref1.clash)
	}
	if ref2.clash != "联通-01 #2" {
		t.Errorf("重名的 n2 应当带后缀，实际 %q", ref2.clash)
	}
	if ref2.display != "联通-01" {
		t.Errorf("n2 的界面显示名 = %q, 期望原名（界面不该看到后缀）", ref2.display)
	}
	if ref3.display != "移动-02" {
		t.Errorf("n3 的显示名 = %q", ref3.display)
	}
	// 普通节点配置只有一个 select 组，两层组名相同 —— 这决定了切节点只需一步
	if nodeGroup != NormalGroupName || topGroup != NormalGroupName {
		t.Errorf("策略组 = %q/%q, 期望都是 %q", nodeGroup, topGroup, NormalGroupName)
	}
	if len(names) != 3 {
		t.Errorf("节点数 = %d, 期望 3", len(names))
	}
}

// 分流模式要真的写进配置，否则界面上切「直连/全局」会变成静默空操作。
func TestLoadNormalWritesProxyMode(t *testing.T) {
	for _, tc := range []struct {
		mode string
		want string
		bad  string
	}{
		{"smart", "GEOSITE,cn,DIRECT", "MATCH,DIRECT"},
		{"global", "MATCH,节点选择", "GEOSITE,cn,DIRECT"},
		{"direct", "MATCH,DIRECT", "GEOSITE,cn,DIRECT"},
	} {
		m, _, dir := newNormalTestManager(t)
		if err := m.LoadNormal(plainNodes(), tc.mode); err != nil {
			t.Fatalf("[%s] 生成失败: %v", tc.mode, err)
		}
		b, err := os.ReadFile(filepath.Join(dir, confFileName))
		if err != nil {
			t.Fatalf("[%s] 读配置失败: %v", tc.mode, err)
		}
		if !bytes.Contains(b, []byte(tc.want)) {
			t.Errorf("[%s] 配置里缺少 %q", tc.mode, tc.want)
		}
		if bytes.Contains(b, []byte(tc.bad)) {
			t.Errorf("[%s] 配置里不该出现 %q", tc.mode, tc.bad)
		}
	}
}

// 切节点是高频操作：节点集合没变时必须能判定「配置还是最新的」，
// 否则每点一次节点都会重启一次内核（十几秒），体验无法接受。
func TestNormalConfigUpToDate(t *testing.T) {
	m, _, _ := newNormalTestManager(t)
	nodes := plainNodes()

	if m.NormalConfigUpToDate(nodes) {
		t.Error("还没加载过，不该算最新")
	}
	if err := m.LoadNormal(nodes, "smart"); err != nil {
		t.Fatalf("生成失败: %v", err)
	}
	if !m.NormalConfigUpToDate(nodes) {
		t.Error("刚生成的配置应当算最新")
	}

	changed := plainNodes()
	changed[0].Server = "9.9.9.9"
	if m.NormalConfigUpToDate(changed) {
		t.Error("节点变了必须判定为过期，否则不会重写配置")
	}

	// 家宽模式下载的不是普通节点配置
	m.mu.Lock()
	m.normalMode = false
	m.mu.Unlock()
	if m.NormalConfigUpToDate(nodes) {
		t.Error("家宽模式下不该认为普通配置是最新的")
	}
}

// 内核里认的是「配置里的节点名」（重名会带后缀），而界面点的是节点 ID。
// 这层映射错了，用户点 B 的节点会被切到 A 上，且没有任何报错。
func TestSetActiveNormalNodeMapsIDToConfigName(t *testing.T) {
	m, cfg, dir := newNormalTestManager(t)
	if err := m.LoadNormal(plainNodes(), "smart"); err != nil {
		t.Fatalf("生成失败: %v", err)
	}

	if err := m.SetActiveNormalNode("n2"); err != nil {
		t.Fatalf("选中重名节点失败: %v", err)
	}
	m.mu.Lock()
	active := m.activeName
	m.mu.Unlock()
	if active != "联通-01 #2" {
		t.Errorf("内核侧名字 = %q, 期望带后缀的「联通-01 #2」", active)
	}

	cfg.Lock()
	saved := cfg.ActiveNode
	cfg.Unlock()
	if saved != "n2" {
		t.Errorf("落盘的 ActiveNode = %q, 期望节点 ID n2", saved)
	}
	reloaded, err := config.Load(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatalf("重新加载配置失败: %v", err)
	}
	reloaded.Lock()
	persisted := reloaded.ActiveNode
	reloaded.Unlock()
	if persisted != "n2" {
		t.Errorf("落盘后的 ActiveNode = %q, 期望 n2", persisted)
	}

	// 不存在的节点必须被拒，且不能改坏已有的选择
	if err := m.SetActiveNormalNode("nope"); err == nil {
		t.Error("未知节点应当报错")
	}
	cfg.Lock()
	after := cfg.ActiveNode
	cfg.Unlock()
	if after != "n2" {
		t.Errorf("被拒之后 ActiveNode 不该变动, 实际 %q", after)
	}
}

// Status 的字段要与 v2ray.Manager.Status 对齐：
// activeNode 是节点 ID（界面靠它定位行），activeName 是显示名。
func TestStatusInNormalMode(t *testing.T) {
	m, _, _ := newNormalTestManager(t)
	if err := m.LoadNormal(plainNodes(), "smart"); err != nil {
		t.Fatalf("生成失败: %v", err)
	}
	if err := m.SetActiveNormalNode("n3"); err != nil {
		t.Fatalf("选中节点失败: %v", err)
	}

	st := m.Status()
	if st["kernel"] != config.KindClash {
		t.Errorf("kernel = %v, 期望 mihomo（统一后只有一个内核）", st["kernel"])
	}
	if st["normalMode"] != true {
		t.Errorf("normalMode = %v, 期望 true", st["normalMode"])
	}
	if st["activeNode"] != "n3" {
		t.Errorf("activeNode = %v, 期望节点 ID n3", st["activeNode"])
	}
	if st["activeName"] != "移动-02" {
		t.Errorf("activeName = %v, 期望显示名「移动-02」", st["activeName"])
	}
}

// 普通模式下「上次选的节点没了」也要能自愈，并且落盘到 ActiveNode 而不是 ClashNode
// —— 写错字段会让重启后恢复到一个不相干的节点上。
func TestEnsureActiveNodeLockedNormalMode(t *testing.T) {
	m, cfg, dir := newNormalTestManager(t)
	if err := m.LoadNormal(plainNodes(), "smart"); err != nil {
		t.Fatalf("生成失败: %v", err)
	}
	m.mu.Lock()
	m.activeName = "已经没了的节点"
	m.mu.Unlock()

	m.ensureActiveNodeLocked()

	cfg.Lock()
	got := cfg.ActiveNode
	clashNode := cfg.ClashNode
	cfg.Unlock()
	if got != "n1" {
		t.Errorf("ActiveNode = %q, 期望改用列表第一个节点 n1", got)
	}
	if clashNode != "" {
		t.Errorf("普通模式不该写 ClashNode, 实际 %q", clashNode)
	}
	reloaded, err := config.Load(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatalf("重新加载配置失败: %v", err)
	}
	reloaded.Lock()
	persisted := reloaded.ActiveNode
	reloaded.Unlock()
	if persisted != "n1" {
		t.Errorf("落盘后的 ActiveNode = %q, 期望 n1", persisted)
	}
}

// ensureActiveConfig 是「加载哪份配置」的唯一判据：
// 生效类型是普通节点就生成普通配置，不看有没有家宽分组。
func TestEnsureActiveConfigFollowsKind(t *testing.T) {
	m, cfg, _ := newNormalTestManager(t)
	cfg.Lock()
	cfg.Groups = append(cfg.Groups, config.Group{
		ID: "g1", Name: "家宽", Kind: config.GroupKindClash,
	})
	cfg.Groups = append(cfg.Groups, config.Group{
		ID: "nrm", Name: "订阅", Nodes: plainNodes(),
	})
	cfg.Kernel = config.KindNormal
	cfg.Unlock()

	if err := m.ensureActiveConfig(); err != nil {
		t.Fatalf("生成普通节点配置失败: %v", err)
	}
	if !m.IsNormalMode() {
		t.Error("生效类型是普通节点时应当加载普通节点配置")
	}
}

// 配的是家宽但一个家宽分组都没有（比如刚把最后一个删掉）时，
// 必须回落到普通节点，而不是报错让代理直接起不来。
func TestEnsureActiveConfigFallsBackToNormal(t *testing.T) {
	m, cfg, _ := newNormalTestManager(t)
	cfg.Lock()
	cfg.Groups = append(cfg.Groups, config.Group{
		ID: "nrm", Name: "订阅", Nodes: plainNodes(),
	})
	cfg.Kernel = config.KindClash
	cfg.Unlock()

	if err := m.ensureActiveConfig(); err != nil {
		t.Fatalf("没有家宽分组时应当回落到普通节点而不是报错: %v", err)
	}
	if !m.IsNormalMode() {
		t.Error("应当回落到普通节点配置")
	}
}

// 订阅刷新后节点 ID 会整批变化（ID 按内容派生），原来选中的节点可能已经不存在。
// 生成配置时必须当场修好并落盘 —— 否则盒子重启后代理起不来，用户的网直接断了。
//
// 这里的坑在于：「选过但没了」和「从没选过」在还原成配置内节点名之后**都是空串**
// （旧 ID 查不到映射），光看那个空串会把前者误判成后者，自愈逻辑整个跳过。
func TestLoadNormalRepairsVanishedActiveNode(t *testing.T) {
	m, cfg, dir := newNormalTestManager(t)
	cfg.Lock()
	cfg.ActiveNode = "已经消失了的节点ID"
	cfg.Unlock()

	if err := m.LoadNormal(plainNodes(), "smart"); err != nil {
		t.Fatalf("生成失败: %v", err)
	}

	cfg.Lock()
	got := cfg.ActiveNode
	cfg.Unlock()
	if got != "n1" {
		t.Errorf("ActiveNode = %q, 期望自动改用列表第一个节点 n1", got)
	}
	m.mu.Lock()
	name := m.activeName
	m.mu.Unlock()
	if name != "联通-01" {
		t.Errorf("activeName = %q, 期望 n1 的配置名「联通-01」", name)
	}

	// 必须落盘：否则每次重启都要重挑一遍，界面也一直挂着不存在的「当前使用中」
	reloaded, err := config.Load(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatalf("重新加载配置失败: %v", err)
	}
	reloaded.Lock()
	persisted := reloaded.ActiveNode
	reloaded.Unlock()
	if persisted != "n1" {
		t.Errorf("落盘后的 ActiveNode = %q, 期望 n1", persisted)
	}
}

// 从没选过节点时不能替用户做决定：界面不该凭空冒出一个「当前使用中」。
func TestLoadNormalKeepsUnsetActiveNode(t *testing.T) {
	m, cfg, _ := newNormalTestManager(t)
	if err := m.LoadNormal(plainNodes(), "smart"); err != nil {
		t.Fatalf("生成失败: %v", err)
	}
	cfg.Lock()
	got := cfg.ActiveNode
	cfg.Unlock()
	if got != "" {
		t.Errorf("从没选过节点时不该自动挑一个，ActiveNode 被改成 %q", got)
	}
	m.mu.Lock()
	name := m.activeName
	m.mu.Unlock()
	if name != "" {
		t.Errorf("activeName = %q, 期望为空", name)
	}
}

// 普通模式与家宽模式共用同一个 Manager。切回家宽时若不把普通节点那套映射清掉，
// Status()、选节点恢复、ensureActiveNodeLocked 会继续按普通模式的表查，
// 查出来全是空值 —— 表现为「切到家宽后界面没有高亮、配置也不落盘」。
func TestLoadGroupResetsNormalMode(t *testing.T) {
	m, cfg, dir := newNormalTestManager(t)
	if err := os.WriteFile(filepath.Join(dir, subFileNameFor("g1")), []byte(sampleSub), 0644); err != nil {
		t.Fatalf("写入订阅原文失败: %v", err)
	}
	cfg.Lock()
	cfg.Groups = []config.Group{{ID: "g1", Name: "家宽", Kind: config.GroupKindClash}}
	cfg.Unlock()

	if err := m.LoadNormal(plainNodes(), "smart"); err != nil {
		t.Fatalf("生成失败: %v", err)
	}
	if err := m.LoadGroup("g1", "", ""); err != nil {
		t.Fatalf("加载家宽分组失败: %v", err)
	}

	if m.IsNormalMode() {
		t.Error("加载家宽分组后不该还停在普通模式")
	}
	m.mu.Lock()
	byID, byName := m.normalByID, m.normalByName
	m.mu.Unlock()
	if byID != nil || byName != nil {
		t.Error("切回家宽后必须清掉普通节点的映射表")
	}
}

// 一个节点都转不了时必须明确报错，而不是生成一份空配置交给内核。
func TestLoadNormalRejectsEmpty(t *testing.T) {
	m, _, _ := newNormalTestManager(t)
	if err := m.LoadNormal(nil, "smart"); err == nil {
		t.Error("空节点列表应当报错")
	}
	if err := m.LoadNormal([]config.Node{
		{ID: "x", Name: "不支持的协议", Protocol: "wireguard", Server: "1.1.1.1"},
	}, "smart"); err == nil {
		t.Error("全部节点都转不了时应当报错")
	}
}
