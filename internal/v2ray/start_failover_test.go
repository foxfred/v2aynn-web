package v2ray

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"v2aynn-web/internal/config"
)

// 这一组测试覆盖「开机时发现原激活节点已消失」这条路径。
//
// 背景：节点 ID 是按内容派生的，订阅源一改，ID 全变，cfg.ActiveNode 指向的
// 旧 ID 就不在列表里了。原实现在这里直接报错返回，后果是**盒子重启后代理起不来**，
// 用户必须手动点一个节点 —— 而盒子同时是全屋的出口，等于断网。
// 现在改成复用已有的自动故障转移，挑一个替补顶上。

// failoverNodes 造一批带不同 ping 值的节点，用于验证挑选顺序。
func failoverNodes() []config.Node {
	return []config.Node{
		{ID: "gone", Name: "已消失的旧节点", Ping: 0},
		{ID: "slow", Name: "慢但通", Ping: 900},
		{ID: "fast", Name: "快且通", Ping: 120},
		{ID: "dead", Name: "不通", Ping: -1},
		{ID: "untested", Name: "没测过", Ping: 0},
	}
}

// 排序规则：可达（ping>0）优先 → 延迟升序；不通的和没测过的排在后面。
func TestFailoverOrderPrefersReachableAndFastest(t *testing.T) {
	got := failoverOrder(failoverNodes(), "gone")

	if len(got) != 4 {
		t.Fatalf("候选数 = %d, 期望 4（应排除 exclude）", len(got))
	}
	if got[0].ID != "fast" {
		t.Errorf("首选 = %q, 期望 fast（可达且延迟最低）", got[0].ID)
	}
	if got[1].ID != "slow" {
		t.Errorf("次选 = %q, 期望 slow", got[1].ID)
	}
	// dead(ping=-1) 与 untested(ping=0) 都不可用，必须排在可达节点之后
	for i := 2; i < len(got); i++ {
		if got[i].Ping > 0 {
			t.Errorf("第 %d 位不该是可达节点: %+v", i, got[i])
		}
	}
	for _, n := range got {
		if n.ID == "gone" {
			t.Error("exclude 的节点不应出现在候选里")
		}
	}
}

// 原激活节点已消失时，Start() 必须自动改用替补节点并把结果落盘。
//
// 用测试二进制自身当假 xray（配合 TestMain 的自复用），这样不用真的装 xray
// 就能跑通「挑选 → 落盘 → 拉起进程」整条链路。
func TestStartPicksFallbackWhenActiveNodeMissing(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("加载配置失败: %v", err)
	}
	cfg.ActiveNode = "已经不存在了的ID"
	cfg.Groups = []config.Group{{
		ID: config.DefaultGroupID, Name: "手动节点",
		Nodes: []config.Node{
			{ID: "dead", Name: "不通的", Protocol: "trojan", Server: "a.com", Port: "443", Password: "pw", Ping: -1},
			{ID: "good", Name: "能用的", Protocol: "trojan", Server: "b.com", Port: "443", Password: "pw", Ping: 88},
		},
	}}

	m := NewManager(dir)
	m.SetXrayBin(os.Args[0])
	t.Setenv("V2AYNN_FAKE_XRAY", "1")
	t.Setenv("V2AYNN_FAKE_XRAY_LOG", filepath.Join(dir, "starts.log"))
	m.SetConfig(cfg)

	if err := m.Start(); err != nil {
		t.Fatalf("原节点消失时 Start() 不应失败: %v", err)
	}
	defer m.Stop()

	cfg.Lock()
	got := cfg.ActiveNode
	grp := cfg.ActiveGrp
	cfg.Unlock()
	if got != "good" {
		t.Errorf("自动替补后 ActiveNode = %q, 期望 good（可达优先）", got)
	}
	if grp != config.DefaultGroupID {
		t.Errorf("ActiveGrp = %q, 期望跟着一起更新为 %q", grp, config.DefaultGroupID)
	}
	// 必须落盘：否则下次重启又要重挑一遍，而且界面上显示的仍是旧 ID
	if _, err := os.Stat(cfgPath); err != nil {
		t.Errorf("替补结果未落盘: %v", err)
	}
	// 内存里缓存的激活名也要跟着更新，状态栏才不会显示旧节点
	if name := m.activeName; name != "能用的" {
		t.Errorf("activeName = %q, 期望「能用的」", name)
	}
}

// 一个候选都没有时，必须返回哨兵错误，让开机重试循环能提前收手。
func TestStartWithoutAnyNodeReturnsSentinel(t *testing.T) {
	dir := t.TempDir()
	cfg, err := config.Load(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatalf("加载配置失败: %v", err)
	}
	cfg.ActiveNode = "n1"
	cfg.Groups = nil // 一个节点都没有

	m := NewManager(dir)
	m.SetXrayBin(os.Args[0])
	t.Setenv("V2AYNN_FAKE_XRAY", "1")
	m.SetConfig(cfg)

	err = m.Start()
	if err == nil {
		m.Stop()
		t.Fatal("没有任何节点时 Start() 应当报错")
	}
	if !errors.Is(err, ErrNoUsableNode) {
		t.Errorf("错误未被 ErrNoUsableNode 标记，重试循环无法识别: %v", err)
	}
}

// 关掉自动故障转移开关，**不影响**开机自愈：原节点消失时仍然要挑一个顶上。
//
// 这条曾经写成「关掉开关就不许换、直接报错」，2026-10-01 改掉了。理由：
// 开关管的是「运行中探测失败就自动换节点」这个用户偏好；而「上次选中的节点
// 已经不在列表里」不换就起不来，后果是盒子重启后代理起不来、全屋断网。
// 一个偏好开关不该把「能不能上网」也一起关掉。
//
// 真正该保持不变的是：原节点**还在**的时候不许动用户的选择（见
// TestStartKeepsExistingActiveNode）。
func TestStartReplacesStaleNodeEvenWhenFailoverDisabled(t *testing.T) {
	off := false
	dir := t.TempDir()
	cfg, err := config.Load(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatalf("加载配置失败: %v", err)
	}
	cfg.ActiveNode = "已经不存在了的ID"
	cfg.AutoFailover = &off
	cfg.Groups = []config.Group{{
		ID: config.DefaultGroupID, Name: "手动节点",
		Nodes: []config.Node{{ID: "good", Name: "能用的", Protocol: "trojan", Server: "b.com", Port: "443", Password: "pw"}},
	}}

	m := NewManager(dir)
	m.SetXrayBin(os.Args[0])
	t.Setenv("V2AYNN_FAKE_XRAY", "1")
	m.SetConfig(cfg)

	if err := m.Start(); err != nil {
		t.Fatalf("原节点消失时应当挑替补顶上而不是报错（否则盒子重启就断网）: %v", err)
	}
	defer m.Stop()

	cfg.Lock()
	got := cfg.ActiveNode
	cfg.Unlock()
	if got != "good" {
		t.Errorf("原节点已消失时 ActiveNode = %q, 期望替补成 good", got)
	}
}

// 正常情况（原节点还在）不能被这段新逻辑影响：不该换节点、不该报错。
func TestStartKeepsExistingActiveNode(t *testing.T) {
	dir := t.TempDir()
	cfg, err := config.Load(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatalf("加载配置失败: %v", err)
	}
	cfg.ActiveNode = "slow"
	cfg.Groups = []config.Group{{
		ID: config.DefaultGroupID, Name: "手动节点",
		Nodes: []config.Node{
			{ID: "slow", Name: "慢的", Protocol: "trojan", Server: "a.com", Port: "443", Password: "pw", Ping: 900},
			{ID: "fast", Name: "快的", Protocol: "trojan", Server: "b.com", Port: "443", Password: "pw", Ping: 50},
		},
	}}

	m := NewManager(dir)
	m.SetXrayBin(os.Args[0])
	t.Setenv("V2AYNN_FAKE_XRAY", "1")
	t.Setenv("V2AYNN_FAKE_XRAY_LOG", filepath.Join(dir, "starts.log"))
	m.SetConfig(cfg)

	if err := m.Start(); err != nil {
		t.Fatalf("Start() 失败: %v", err)
	}
	defer m.Stop()

	cfg.Lock()
	got := cfg.ActiveNode
	cfg.Unlock()
	if got != "slow" {
		t.Errorf("原节点还在时不该换节点，ActiveNode = %q", got)
	}
}
