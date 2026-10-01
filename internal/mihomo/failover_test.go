package mihomo

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"v2aynn-web/internal/config"
)

// 普通节点的自动故障转移。
//
// 背景：普通节点配置里只有一个 select 组，而 select 组**不会**自己换节点 ——
// 当前节点挂了，mihomo 会老老实实继续用它，用户的网就是不通。xray 时代靠
// 「节点不通 → 进程起不来 → 崩溃 4 次 → tryFailover」兜住，mihomo 加载配置时
// 不校验节点连通性，这条链根本不存在，所以由 healthTick 自己看护。
//
// 本文件只测「看护的决策」，不拉真内核：探测走假控制口，切换走 applyNode
// 发 PUT —— 两者都是 HTTP，假服务端足够还原。

// fakeKernelCtrl 一个「哪些节点是死的」可以随时改的假内核控制口。
//
//	GET  /proxies/{节点名}/delay  死的回 503 {"message":"Timeout"}，活的回 200 {"delay":N}
//	PUT  /proxies/{组名}          回 204，并记下请求体里要拨到哪个节点
type fakeKernelCtrl struct {
	mu     sync.Mutex
	dead   map[string]bool
	picked []string // 依次收到的「拨到哪个节点」
}

func newFakeKernelCtrl(dead map[string]bool) *fakeKernelCtrl {
	d := make(map[string]bool, len(dead))
	for k, v := range dead {
		d[k] = v
	}
	return &fakeKernelCtrl{dead: d}
}

func (f *fakeKernelCtrl) setDead(name string, dead bool) {
	f.mu.Lock()
	f.dead[name] = dead
	f.mu.Unlock()
}

func (f *fakeKernelCtrl) switches() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.picked...)
}

func (f *fakeKernelCtrl) serveHTTP(rw http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPut:
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<16))
		var req struct {
			Name string `json:"name"`
		}
		_ = json.Unmarshal(body, &req)
		f.mu.Lock()
		f.picked = append(f.picked, req.Name)
		f.mu.Unlock()
		rw.WriteHeader(http.StatusNoContent)

	case http.MethodGet:
		_, _ = io.Copy(io.Discard, r.Body)
		name := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/proxies/"), "/delay")
		f.mu.Lock()
		dead := f.dead[name]
		f.mu.Unlock()
		rw.Header().Set("Content-Type", "application/json")
		if dead {
			rw.WriteHeader(http.StatusServiceUnavailable)
			_, _ = rw.Write([]byte(`{"message":"Timeout"}`))
			return
		}
		_, _ = rw.Write([]byte(`{"delay":120}`))

	default:
		_, _ = io.Copy(io.Discard, r.Body)
		rw.WriteHeader(http.StatusNoContent)
	}
}

// newFailoverTestManager 造一个「配置里有节点、已加载普通配置、选中 n1、内核在跑」的 Manager。
//
// 必须把节点真的塞进 cfg.Groups：healthTick 挑替补走的是 cfg.NormalNodes()，
// 只调 LoadNormal 生成配置文件是不够的（生产路径里 ensureNormalConfig 传的就是
// cfg.NormalNodes()，测试要还原这一点，否则候选永远是空、故障转移根本触发不了）。
//
// running 直接置位而不是真去拉进程：healthTick 只看这个标记，
// 拉进程会让单测依赖 mihomo 二进制。
//
// ★ 必须显式把 AutoFailover 置为 true：开关的默认值是**关闭**
// （2026-10-01 改的，见 config.FailoverEnabled），不打开的话 healthTick
// 第一行就 return，这一组用例全部会变成「什么都没发生」。
func newFailoverTestManager(t *testing.T, dead map[string]bool) (*Manager, *config.Config, *fakeKernelCtrl) {
	t.Helper()
	m, cfg, _ := newNormalTestManager(t)

	on := true
	cfg.Lock()
	cfg.AutoFailover = &on
	cfg.Groups = []config.Group{{ID: "g1", Name: "测试分组", Nodes: plainNodes()}}
	cfg.Unlock()

	if err := m.LoadNormal(cfg.NormalNodes(), "smart"); err != nil {
		t.Fatalf("生成普通节点配置失败: %v", err)
	}
	if err := m.SetActiveNormalNode("n1"); err != nil {
		t.Fatalf("选中 n1 失败: %v", err)
	}

	f := newFakeKernelCtrl(dead)
	srv := httptest.NewServer(http.HandlerFunc(f.serveHTTP))
	t.Cleanup(srv.Close)

	m.mu.Lock()
	m.ctrlAddr = strings.TrimPrefix(srv.URL, "http://")
	m.running = true
	m.mu.Unlock()
	return m, cfg, f
}

func rawActiveName(m *Manager) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.activeName
}

func failCount(m *Manager) int {
	m.healthMu.Lock()
	defer m.healthMu.Unlock()
	return m.failCount
}

// 一次网络抖动不该动用户的选择：没到阈值前必须保持原节点。
func TestHealthTickSwitchesOnlyAfterThreshold(t *testing.T) {
	m, _, f := newFailoverTestManager(t, map[string]bool{"联通-01": true})

	for i := 0; i < healthFailThreshold-1; i++ {
		m.healthTick()
	}
	if got := rawActiveName(m); got != "联通-01" {
		t.Fatalf("才失败 %d 次就换节点了（现在是 %q），阈值应当是 %d",
			healthFailThreshold-1, got, healthFailThreshold)
	}
	if n := len(f.switches()); n != 0 {
		t.Fatalf("没到阈值就不该向内核发切换请求，实际发了 %d 次", n)
	}

	m.healthTick()
	if got := rawActiveName(m); got == "联通-01" {
		t.Errorf("连续失败 %d 次后应当换节点，实际还是 %q", healthFailThreshold, got)
	}
	if len(f.switches()) == 0 {
		t.Error("换节点应当向内核发过 PUT")
	}
}

// 节点一直好好的就不该有任何动作，也不该把失败计数攒起来。
func TestHealthTickKeepsNodeWhileHealthy(t *testing.T) {
	m, _, f := newFailoverTestManager(t, nil)

	for i := 0; i < healthFailThreshold*3; i++ {
		m.healthTick()
	}
	if got := rawActiveName(m); got != "联通-01" {
		t.Errorf("节点一直通，不该换节点，实际变成 %q", got)
	}
	if n := len(f.switches()); n != 0 {
		t.Errorf("节点一直通，不该发切换请求，实际发了 %d 次", n)
	}
	if n := failCount(m); n != 0 {
		t.Errorf("探测成功应当把失败计数清零，实际是 %d", n)
	}
}

// 恢复一次就清零：中间通了一下，之前的失败不该继续累加到阈值。
func TestHealthTickRecoveryResetsFailureCount(t *testing.T) {
	m, _, f := newFailoverTestManager(t, map[string]bool{"联通-01": true})

	m.healthTick()
	m.healthTick()
	if n := failCount(m); n != 2 {
		t.Fatalf("两次失败后计数应当是 2，实际 %d", n)
	}

	f.setDead("联通-01", false)
	m.healthTick()
	if n := failCount(m); n != 0 {
		t.Fatalf("探测恢复后计数应当清零，实际 %d", n)
	}

	f.setDead("联通-01", true)
	m.healthTick()
	m.healthTick()
	if got := rawActiveName(m); got != "联通-01" {
		t.Errorf("清零后重新累计 2 次还没到阈值，不该换节点，实际变成 %q", got)
	}
}

// 用户在设置里关了自动故障转移，就一次都不该换。
func TestHealthTickSkipsWhenFailoverDisabled(t *testing.T) {
	m, cfg, _ := newFailoverTestManager(t, map[string]bool{"联通-01": true})

	off := false
	cfg.Lock()
	cfg.AutoFailover = &off
	cfg.Unlock()

	for i := 0; i < healthFailThreshold+3; i++ {
		m.healthTick()
	}
	if got := rawActiveName(m); got != "联通-01" {
		t.Errorf("关掉开关后不该换节点，实际变成 %q", got)
	}
}

// 家宽模式下不管：那边是订阅原文，里面有内核自己的 url-test 组负责往下换。
func TestHealthTickSkipsInClashMode(t *testing.T) {
	m, _, _ := newFailoverTestManager(t, map[string]bool{"联通-01": true})

	m.mu.Lock()
	m.normalMode = false
	m.mu.Unlock()

	for i := 0; i < healthFailThreshold+3; i++ {
		m.healthTick()
	}
	if got := rawActiveName(m); got != "联通-01" {
		t.Errorf("家宽模式下不该动普通节点的选择，实际变成 %q", got)
	}
}

// 内核没在跑时探测必然失败，不能把失败计数刷满并误判。
func TestHealthTickSkipsWhenKernelNotRunning(t *testing.T) {
	m, _, _ := newFailoverTestManager(t, map[string]bool{"联通-01": true})

	m.mu.Lock()
	m.running = false
	m.mu.Unlock()

	for i := 0; i < healthFailThreshold+3; i++ {
		m.healthTick()
	}
	if n := failCount(m); n != 0 {
		t.Errorf("内核没在跑时不该累计失败，实际 %d", n)
	}
	if got := rawActiveName(m); got != "联通-01" {
		t.Errorf("内核没在跑时不该换节点，实际变成 %q", got)
	}
}

// 换过去的新节点也是死的，下一轮要接着往「没试过的」里挑，
// 而不是在最初那个节点和新节点之间来回跳。
func TestFailoverMovesOnToUntriedNodes(t *testing.T) {
	m, _, _ := newFailoverTestManager(t, map[string]bool{
		"联通-01":    true,
		"联通-01 #2": true,
	})

	if got := m.ActiveNormalNodeID(); got != "n1" {
		t.Fatalf("前置条件不成立：起始应当是 n1，实际 %q", got)
	}

	for i := 0; i < healthFailThreshold; i++ {
		m.healthTick()
	}
	if got := m.ActiveNormalNodeID(); got != "n2" {
		t.Fatalf("第一次换节点应当换到候选里第一个（n2），实际 %q", got)
	}

	for i := 0; i < healthFailThreshold; i++ {
		m.healthTick()
	}
	got := m.ActiveNormalNodeID()
	if got == "n1" {
		t.Error("不该换回最初那个节点 —— 说明候选里没排除「正在失败的那个」")
	}
	if got != "n3" {
		t.Errorf("三个节点里只剩 n3 没试过，应当换到 n3，实际 %q", got)
	}
}

// 整份订阅都挂了的时候要停下来，不能一个接一个换满 800 个节点。
func TestFailoverGivesUpAfterMaxTotalTry(t *testing.T) {
	m, _, _ := newFailoverTestManager(t, map[string]bool{
		"联通-01":    true,
		"联通-01 #2": true,
		"移动-02":    true,
	})

	// 三个节点全死，最多试满三轮就会没候选可挑
	for i := 0; i < healthFailThreshold*6; i++ {
		m.healthTick()
	}
	m.healthMu.Lock()
	gaveUp := m.failGaveUp
	m.healthMu.Unlock()
	if !gaveUp {
		t.Error("没有候选节点之后应当标记放弃，避免无限换下去")
	}

	// 放弃之后不该再换
	before := rawActiveName(m)
	for i := 0; i < healthFailThreshold; i++ {
		m.healthTick()
	}
	if got := rawActiveName(m); got != before {
		t.Errorf("已放弃后不该再换节点：%q -> %q", before, got)
	}
}

// 探测成功要把「放弃」状态也解掉，否则一次全站故障之后就永久瘫着不看了。
func TestHealthTickClearsGaveUpOnRecovery(t *testing.T) {
	m, _, f := newFailoverTestManager(t, map[string]bool{
		"联通-01":    true,
		"联通-01 #2": true,
		"移动-02":    true,
	})
	for i := 0; i < healthFailThreshold*6; i++ {
		m.healthTick()
	}
	m.healthMu.Lock()
	gaveUp := m.failGaveUp
	m.healthMu.Unlock()
	if !gaveUp {
		t.Fatal("前置条件不成立：应当已经放弃")
	}

	// 让当前节点恢复
	f.setDead(rawActiveName(m), false)
	m.healthTick()

	m.healthMu.Lock()
	defer m.healthMu.Unlock()
	if m.failGaveUp {
		t.Error("探测恢复后应当解除放弃状态")
	}
	if len(m.failTried) != 0 {
		t.Errorf("探测恢复后应当清空已试列表，实际还有 %d 个", len(m.failTried))
	}
}
