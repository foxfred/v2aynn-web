package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"v2aynn-web/internal/config"
	"v2aynn-web/internal/mihomo"
	"v2aynn-web/internal/v2ray"
)

// clashSubSample 精简版 cfnew 家宽订阅，组结构与真实订阅一致：
// url-test 组当 dialer、select 组列家宽节点、顶层 select 组被规则 MATCH 指向。
const clashSubSample = `mixed-port: 7890
allow-lan: false
mode: rule
log-level: info
external-controller: 127.0.0.1:9090

proxies:
  - name: "🏠 JP-家宽-01"
    type: openvpn
    server: 1.2.3.4
    port: 1776
    dialer-proxy: "⚡ CF前置"
  - name: "🏠 KR-家宽-01"
    type: openvpn
    server: 5.6.7.8
    port: 1323
    dialer-proxy: "⚡ CF前置"

proxy-groups:
  - name: "⚡ CF前置"
    type: url-test
    proxies:
      - "优选域名-01"
  - name: "🏠 家宽节点"
    type: select
    proxies:
      - "🏠 JP-家宽-01"
      - "🏠 KR-家宽-01"
  - name: "🚀 节点选择"
    type: select
    proxies:
      - "🏠 家宽节点"
      - DIRECT

rules:
  - MATCH,🚀 节点选择
`

// newClashTestServerWith 造一个带家宽通道的 WebServer。
//
// payload 是假订阅源返回的内容，测「识别失败」的用例可以换成普通文本。
// 订阅源用本地假服务，测试不依赖外网。
//
// 假服务刻意「不看 UA、一律返回 payload」：这是照 cfnew 的真实行为写的 ——
// 2026-09-30 实测 target=vg 链接在 Go 默认 UA 与 clash-verge UA 下返回的是
// 同一份 Clash YAML（节点数 76 vs 73 只是采样不同，格式一致），也就是说
// 返回什么格式由 URL 上的 target 参数决定，与 UA 无关。
// 别把它改成「UA 不对就返回别的」—— 那样测的就不是真实行为了。
func newClashTestServerWith(t *testing.T, payload string) (*WebServer, *config.Config, *mihomo.Manager, string) {
	t.Helper()
	dir := t.TempDir()
	cfg, err := config.Load(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatalf("加载测试配置失败: %v", err)
	}

	fake := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		_, _ = rw.Write([]byte(payload))
	}))
	t.Cleanup(fake.Close)

	v2m := v2ray.NewManager(dir)
	mhm := mihomo.NewManager(dir)
	s := NewServer(cfg, v2m, mhm)
	// 真实探针要在备用端口上拉起一个 mihomo 进程，单元测试里跑不起来，
	// 统一换成替身。探针本身的行为由 mihomo 包的用例覆盖。
	s.prober = newFakeProber()
	return s, cfg, mhm, fake.URL
}

// fakeProber 测速探针的替身：按节点名查一张预设的延迟表，
// 表里没有的名字视为测不通（与真实内核的行为一致）。
type fakeProber struct {
	mu       sync.Mutex
	group    string
	running  bool
	startErr error
	delays   map[string]int
}

func newFakeProber() *fakeProber {
	return &fakeProber{delays: map[string]int{
		// 前置通道默认是通的 —— 真实场景里前置不通时所有家宽节点都不通，
		// 那条路径由 TestPingClashGroupFailsFastWhenFrontDown 单独覆盖。
		"⚡ CF前置":     120,
		"🏠 JP-家宽-01": 480,
		"🏠 KR-家宽-01": 620,
	}}
}

func (f *fakeProber) Ensure(groupID string, resident bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.startErr != nil {
		return f.startErr
	}
	f.group, f.running = groupID, true
	return nil
}

func (f *fakeProber) Stop() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.running, f.group = false, ""
}

func (f *fakeProber) Group() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.group
}

func (f *fakeProber) IsRunning() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.running
}

func (f *fakeProber) ProxyDelay(name string, timeoutMs int) (int, error) {
	f.mu.Lock()
	ms, ok := f.delays[name]
	f.mu.Unlock()
	if !ok || ms <= 0 {
		return 0, fmt.Errorf("节点无响应")
	}
	return ms, nil
}

func newClashTestServer(t *testing.T) (*WebServer, *config.Config, *mihomo.Manager, string) {
	t.Helper()
	return newClashTestServerWith(t, clashSubSample)
}

// seedClashGroup 往配置里塞一个家宽分组，并把它加载成当前生效的配置。
//
// 这等价于用户「添加订阅分组」之后再点其中一个节点之后的状态 ——
// 只有被点过的那份配置才会生成可运行的 mihomo 配置。
func seedClashGroup(t *testing.T, cfg *config.Config, mhm *mihomo.Manager, id, name, url string) {
	t.Helper()
	cfg.Lock()
	cfg.Groups = append(cfg.Groups, config.Group{
		ID: id, Name: name, URL: url,
		Kind: config.GroupKindClash, Nodes: []config.Node{},
	})
	cfg.Unlock()
	if err := mhm.LoadGroup(id, url, ""); err != nil {
		t.Fatalf("加载家宽分组失败: %v", err)
	}
}

// waitFor 轮询等待条件成立。新增分组的拉取在后台 goroutine 里跑，
// 测试必须等它跑完再断言，否则会随机失败。
func waitFor(t *testing.T, timeout time.Duration, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

// 「添加订阅分组」这一个入口要能同时吃下普通订阅与家宽订阅：
// 拉回来的内容像 cfnew 家宽配置时，自动把分组标记成家宽类型并交给 mihomo 内核。
func TestAddGroupDetectsClashSub(t *testing.T) {
	s, cfg, mhm, url := newClashTestServer(t)

	req := httptest.NewRequest("POST", "/api/group/add",
		strings.NewReader(`{"name":"家宽","url":"`+url+`"}`))
	rec := httptest.NewRecorder()
	s.apiAddGroup(rec, req)

	var resp map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	id := resp["id"]
	if id == "" {
		t.Fatalf("添加分组未返回 id: %s", rec.Body.String())
	}

	if !waitFor(t, 5*time.Second, func() bool {
		cfg.Lock()
		defer cfg.Unlock()
		return cfg.FindClashGroup(id) != nil
	}) {
		t.Fatal("分组没有被识别为家宽订阅")
	}
	// 内核那边也要把订阅原文落下来，否则侧栏会一直显示 0 个节点
	if !waitFor(t, 5*time.Second, func() bool { return mhm.GroupNodeCount(id) == 2 }) {
		t.Fatalf("家宽节点数 = %d, 期望 2", mhm.GroupNodeCount(id))
	}
	if mhm.SubLastFetch(id) == "" {
		t.Error("订阅时间未记录")
	}
	// 还没点过任何节点，所以运行配置不该生成 —— 生成它是「启用」那一步的事
	if mhm.HasConfig() {
		t.Error("尚未启用任何家宽节点，不该生成运行配置")
	}

	// 家宽分组的 Nodes 必须留空：混进 AllNodes() 会被 xray 的故障转移
	// 当成候选节点，那是另一套协议，切过去必然连不上。
	cfg.Lock()
	g := cfg.FindClashGroup(id)
	nodes := len(g.Nodes)
	cfg.Unlock()
	if nodes != 0 {
		t.Errorf("家宽分组的 Nodes = %d, 期望 0", nodes)
	}
}

// 拉回来的不是 Clash 配置时，分组要保持普通类型，不能被误标成家宽
func TestAddGroupKeepsNormalSubNormal(t *testing.T) {
	s, cfg, mhm, url := newClashTestServerWith(t, "this is not a subscription at all")

	req := httptest.NewRequest("POST", "/api/group/add",
		strings.NewReader(`{"name":"普通","url":"`+url+`"}`))
	rec := httptest.NewRecorder()
	s.apiAddGroup(rec, req)

	var resp map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	time.Sleep(400 * time.Millisecond) // 等后台拉取跑完

	cfg.Lock()
	isClash := cfg.FindClashGroup(resp["id"]) != nil
	cfg.Unlock()
	if isClash {
		t.Error("普通订阅被误标成了家宽分组")
	}
	if n := mhm.GroupNodeCount(resp["id"]); n != 0 {
		t.Errorf("普通订阅不该进 mihomo，节点数 = %d", n)
	}
}

func TestClashGroupAppearsInGroups(t *testing.T) {
	s, cfg, mhm, url := newClashTestServer(t)
	seedClashGroup(t, cfg, mhm, "g1", "家宽", url)

	rec := httptest.NewRecorder()
	s.apiGroups(rec, httptest.NewRequest("GET", "/api/groups", nil))

	var resp struct {
		Groups []groupSummary `json:"groups"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	var clash *groupSummary
	for i := range resp.Groups {
		if resp.Groups[i].ID == "g1" {
			clash = &resp.Groups[i]
		}
	}
	if clash == nil {
		t.Fatal("分组列表里没有家宽分组")
	}
	if !clash.Clash {
		t.Error("家宽分组没被标成 clash，前端会按普通分组渲染")
	}
	// 节点数必须来自内核侧，不能是 g.Nodes —— 家宽分组的 Nodes 恒为空
	if clash.NodeCount != 2 {
		t.Errorf("节点数 = %d, 期望 2", clash.NodeCount)
	}
	if clash.LastFetch == "" {
		t.Error("拉取时间应来自内核侧的订阅文件")
	}
}

// 没配家宽订阅时，界面上不应凭空出现家宽分组
func TestNoClashGroupWithoutSubscription(t *testing.T) {
	dir := t.TempDir()
	cfg, err := config.Load(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatalf("加载测试配置失败: %v", err)
	}
	s := NewServer(cfg, v2ray.NewManager(dir), mihomo.NewManager(dir))

	rec := httptest.NewRecorder()
	s.apiGroups(rec, httptest.NewRequest("GET", "/api/groups", nil))

	var resp struct {
		Groups []groupSummary `json:"groups"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	for _, g := range resp.Groups {
		if g.Clash {
			t.Errorf("未配置家宽时不应出现家宽分组: %+v", g)
		}
	}
}

func TestClashNodesList(t *testing.T) {
	s, cfg, mhm, url := newClashTestServer(t)
	seedClashGroup(t, cfg, mhm, "g1", "家宽", url)

	req := httptest.NewRequest("GET", "/api/group/g1/nodes", nil)
	req.SetPathValue("id", "g1")
	rec := httptest.NewRecorder()
	s.apiGroupNodes(rec, req)

	var resp struct {
		Nodes  []config.Node `json:"nodes"`
		Active string        `json:"active"`
		Clash  bool          `json:"clash"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if !resp.Clash || len(resp.Nodes) != 2 {
		t.Fatalf("家宽节点列表异常: %+v", resp)
	}
	// ID 必须同时带分组与节点名：点节点时要知道该加载哪一份配置
	want := config.ClashNodeID("g1", resp.Nodes[0].Name)
	if resp.Nodes[0].ID != want {
		t.Errorf("节点 ID = %q, 期望 %q", resp.Nodes[0].ID, want)
	}
	if !strings.HasPrefix(resp.Nodes[0].ID, config.ClashNodeIDPrefix+"g1:") {
		t.Errorf("节点 ID 里没带分组: %q", resp.Nodes[0].ID)
	}
	if resp.Nodes[0].Protocol != "openvpn" {
		t.Errorf("节点协议 = %q, 期望 openvpn", resp.Nodes[0].Protocol)
	}
	// 内核没跑家宽时不应有激活项，否则界面会同时标出两个「当前使用中」
	if resp.Active != "" {
		t.Errorf("未启用家宽时 active 应为空, 得到 %q", resp.Active)
	}
}

// 未生效的家宽分组也必须能列出节点。列表里没有节点，用户就没法点任何一个
// 来启用它 —— 这个分组就永远启用不了。节点名现场从订阅原文里解析。
func TestClashNodesOfInactiveGroupStillListed(t *testing.T) {
	s, cfg, mhm, url := newClashTestServer(t)
	seedClashGroup(t, cfg, mhm, "g1", "家宽A", url)

	cfg.Lock()
	cfg.Groups = append(cfg.Groups, config.Group{
		ID: "g2", Name: "家宽B", URL: url,
		Kind: config.GroupKindClash, Nodes: []config.Node{},
	})
	cfg.Unlock()
	// 只把订阅存下来备用，不切过去
	if _, err := mhm.FetchSubscription("g2", url, ""); err != nil {
		t.Fatalf("拉取第二个家宽分组失败: %v", err)
	}

	req := httptest.NewRequest("GET", "/api/group/g2/nodes", nil)
	req.SetPathValue("id", "g2")
	rec := httptest.NewRecorder()
	s.apiGroupNodes(rec, req)

	var resp struct {
		Nodes  []config.Node `json:"nodes"`
		Active string        `json:"active"`
		Clash  bool          `json:"clash"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if !resp.Clash {
		t.Error("g2 应当是家宽分组")
	}
	if len(resp.Nodes) != 2 {
		t.Fatalf("未生效的家宽分组也应当列出节点, 得到 %d 个", len(resp.Nodes))
	}
	// ID 里的分组必须是 g2，不能串成内核当前加载的 g1
	if !strings.HasPrefix(resp.Nodes[0].ID, config.ClashNodeIDPrefix+"g2:") {
		t.Errorf("节点 ID 里的分组串了: %q", resp.Nodes[0].ID)
	}
	// 但「当前使用中」不能标 —— 内核跑的是 g1，标出来就是假的
	if resp.Active != "" {
		t.Errorf("未生效的分组不该有激活项, 得到 %q", resp.Active)
	}
	// 侧栏的节点数也照常
	if n := mhm.GroupNodeCount("g2"); n != 2 {
		t.Errorf("g2 节点数 = %d, 期望 2", n)
	}
}

// 这是本次修复的核心：用户连着 xray 节点时也必须能测家宽节点，
// 而且**不能**因此把当前连接切走。
//
// 修复前：家宽内核与 xray 抢同一组端口，同一时刻只能跑一个，所以
// 「测家宽」被绑成了「先切到家宽」—— 节点要是不通，用户的连接当场就断，
// 而且再也测不了别的节点。现在测速走探针（备用端口），与当前连接无关。
func TestPingClashNodeWorksWhileXrayIsActive(t *testing.T) {
	s, cfg, mhm, url := newClashTestServer(t)
	seedClashGroup(t, cfg, mhm, "g1", "家宽", url)
	// 内核侧压根没跑：bin 指向不存在的文件，模拟「用户正在用 xray」
	mhm.SetMihomoBin(filepath.Join(t.TempDir(), "no-such-mihomo"))

	cfg.Lock()
	cfg.Kernel = config.KernelXray
	cfg.ActiveNode = "普通节点ID"
	cfg.Unlock()

	req := httptest.NewRequest("POST", "/api/ping/x", nil)
	req.SetPathValue("id", config.ClashNodeID("g1", "🏠 JP-家宽-01"))
	rec := httptest.NewRecorder()
	s.apiPing(rec, req)

	var resp struct {
		ID    string `json:"id"`
		MS    int    `json:"ms"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if resp.Error != "" {
		t.Fatalf("连着 xray 时测家宽不该报错: %s", resp.Error)
	}
	if resp.MS != 480 {
		t.Errorf("ms = %d, 期望探针返回的 480", resp.MS)
	}
	if resp.ID != config.ClashNodeID("g1", "🏠 JP-家宽-01") {
		t.Errorf("返回的 id 必须原样带回（界面靠它定位行）: %q", resp.ID)
	}
	// 关键：当前连接必须原封不动
	cfg.Lock()
	kernel, active := cfg.Kernel, cfg.ActiveNode
	cfg.Unlock()
	if kernel != config.KernelXray || active != "普通节点ID" {
		t.Errorf("测速不该动当前连接: kernel=%q activeNode=%q", kernel, active)
	}
}

// 内核里装的是 A 分组，测 B 分组的节点也要能测 ——
// 探针里换一份配置就行，正在服务流量的那份一点都不用动。
func TestPingClashNodeOfInactiveGroupUsesProber(t *testing.T) {
	s, cfg, mhm, url := newClashTestServer(t)
	seedClashGroup(t, cfg, mhm, "g1", "家宽A", url)

	cfg.Lock()
	cfg.Groups = append(cfg.Groups, config.Group{
		ID: "g2", Name: "家宽B", URL: url,
		Kind: config.GroupKindClash, Nodes: []config.Node{},
	})
	cfg.Unlock()
	if _, err := mhm.FetchSubscription("g2", url, ""); err != nil {
		t.Fatalf("拉取第二个家宽分组失败: %v", err)
	}

	req := httptest.NewRequest("POST", "/api/ping/x", nil)
	req.SetPathValue("id", config.ClashNodeID("g2", "🏠 KR-家宽-01"))
	rec := httptest.NewRecorder()
	s.apiPing(rec, req)

	var resp struct {
		MS    int    `json:"ms"`
		Error string `json:"error"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Error != "" || resp.MS != 620 {
		t.Errorf("ms=%d error=%q, 期望 ms=620 无错误", resp.MS, resp.Error)
	}
	// 内核里那份配置不能被换掉：它可能正在服务用户的流量
	if got := mhm.LoadedGroup(); got != "g1" {
		t.Errorf("LoadedGroup = %q, 期望仍是 g1", got)
	}
}

// 探针起不来时必须如实报错并给出 ms=-1，让界面显示「超时」而不是假装成功；
// 而且错误信息要能看懂，不能是 `dial tcp ...: connection refused`。
func TestPingClashNodeReportsProberFailure(t *testing.T) {
	s, cfg, mhm, url := newClashTestServer(t)
	seedClashGroup(t, cfg, mhm, "g1", "家宽", url)
	const want = "找不到 mihomo 内核(/usr/local/bin/mihomo)，请先把内核文件部署到该路径"
	s.prober.(*fakeProber).startErr = errors.New(want)

	req := httptest.NewRequest("POST", "/api/ping/x", nil)
	req.SetPathValue("id", config.ClashNodeID("g1", "🏠 JP-家宽-01"))
	rec := httptest.NewRecorder()
	s.apiPing(rec, req)

	var resp struct {
		MS    int    `json:"ms"`
		Error string `json:"error"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.MS != -1 {
		t.Errorf("探针不可用时 ms 应为 -1, 得到 %d", resp.MS)
	}
	if resp.Error != want {
		t.Errorf("应当把探针起不来的原因原样告诉用户, 实际: %q", resp.Error)
	}
}

// 整组测速也走探针，内核没跑照样能测；结果要落到分组的探针表里 ——
// 落盘之后，重启、或这个分组没被内核加载时，界面上仍能看到上次的数字。
func TestPingClashGroupUsesProberAndPersists(t *testing.T) {
	s, cfg, mhm, url := newClashTestServer(t)
	seedClashGroup(t, cfg, mhm, "g1", "家宽", url)
	mhm.SetMihomoBin(filepath.Join(t.TempDir(), "no-such-mihomo"))

	req := httptest.NewRequest("POST", "/api/ping/group/g1", nil)
	req.SetPathValue("id", "g1")
	rec := httptest.NewRecorder()
	s.apiPingGroup(rec, req)

	var res []map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("解析响应失败: %v (body=%s)", err, rec.Body.String())
	}
	if len(res) != 2 {
		t.Fatalf("应当返回 2 个节点的结果, 得到 %d", len(res))
	}

	cfg.Lock()
	g := cfg.FindClashGroup("g1")
	p, ok := g.Probe("🏠 JP-家宽-01")
	cfg.Unlock()
	if !ok || p.MS != 480 {
		t.Errorf("测速结果没落盘: %+v ok=%v", p, ok)
	}
}

// 前置通道不通时，所有家宽节点都不可能通。必须直接说清楚，
// 别让用户对着 70 多个超时发呆 —— 那正是「数字乱」的一部分。
func TestPingClashGroupFailsFastWhenFrontDown(t *testing.T) {
	s, cfg, mhm, url := newClashTestServer(t)
	seedClashGroup(t, cfg, mhm, "g1", "家宽", url)
	f := s.prober.(*fakeProber)
	f.mu.Lock()
	delete(f.delays, "⚡ CF前置")
	f.mu.Unlock()

	req := httptest.NewRequest("POST", "/api/ping/group/g1", nil)
	req.SetPathValue("id", "g1")
	rec := httptest.NewRecorder()
	s.apiPingGroup(rec, req)

	var resp map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if !strings.Contains(resp["error"], "前置通道") {
		t.Errorf("应当明确指出前置通道不通, 实际: %q", resp["error"])
	}
	// 前置不通时不该白测一轮：节点表里不应留下任何结果
	cfg.Lock()
	g := cfg.FindClashGroup("g1")
	_, has := g.Probe("🏠 JP-家宽-01")
	cfg.Unlock()
	if has {
		t.Error("前置不通时不该产出节点结果")
	}
}

// 家宽节点的延迟与速度存在分组的探针表里，列表必须把它读出来 ——
// 内核没加载这个分组时也要能显示（重启后同理）。
func TestClashNodesShowStoredProbes(t *testing.T) {
	s, cfg, mhm, url := newClashTestServer(t)
	seedClashGroup(t, cfg, mhm, "g1", "家宽", url)

	cfg.Lock()
	g := cfg.FindClashGroup("g1")
	g.SetProbeMS("🏠 JP-家宽-01", 480)
	g.SetProbeSpeed("🏠 JP-家宽-01", 12.5)
	g.SetProbeMS("🏠 KR-家宽-01", -1)
	cfg.Unlock()

	req := httptest.NewRequest("GET", "/api/group/g1/nodes", nil)
	req.SetPathValue("id", "g1")
	rec := httptest.NewRecorder()
	s.apiGroupNodes(rec, req)

	var resp struct {
		Nodes []config.Node `json:"nodes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	byName := map[string]config.Node{}
	for _, n := range resp.Nodes {
		byName[n.Name] = n
	}
	if got := byName["🏠 JP-家宽-01"]; got.Ping != 480 || got.Speed != 12.5 {
		t.Errorf("JP 节点 ping/speed = %d/%v, 期望 480/12.5", got.Ping, got.Speed)
	}
	if got := byName["🏠 KR-家宽-01"]; got.Ping != -1 {
		t.Errorf("KR 节点应显示为超时(-1), 得到 %d", got.Ping)
	}
}

// 「真实测速」在家宽模式下必须把数字记到**家宽节点**上。
//
// 修复前它写的是 cfg.ActiveNode —— 而那个字段在家宽模式下还停在上一个普通
// 节点上，于是家宽测出来的速度被记到了普通节点头上，两个节点的数字同时失真。
func TestSpeedInClashModeSavedToClashNode(t *testing.T) {
	s, cfg, mhm, url := newClashTestServer(t)
	seedClashGroup(t, cfg, mhm, "g1", "家宽", url)

	cfg.Lock()
	cfg.Groups[0].Nodes = []config.Node{{ID: "plain1", Name: "普通节点"}}
	cfg.ActiveNode = "plain1" // 上一个普通节点，家宽模式下依然停在这里
	cfg.Kernel = config.KernelMihomo
	cfg.ActiveGrp = "g1"
	cfg.ClashNode = "🏠 JP-家宽-01"
	cfg.Unlock()

	s.saveSpeed(12.5)

	cfg.Lock()
	var plainSpeed float64
	for _, n := range cfg.Groups[0].Nodes {
		if n.ID == "plain1" {
			plainSpeed = n.Speed
		}
	}
	p, ok := cfg.FindClashGroup("g1").Probe("🏠 JP-家宽-01")
	cfg.Unlock()

	if !ok || p.MBPS != 12.5 {
		t.Errorf("家宽节点没记上速度: %+v ok=%v", p, ok)
	}
	if plainSpeed != 0 {
		t.Errorf("普通节点被写入了家宽测出来的速度 %v —— 这正是要修的那个 bug", plainSpeed)
	}
}

// 点家宽节点要把内核标记切到 mihomo，并把生效分组与节点名一起落盘。
// 这里故意把内核路径指向不存在的文件，避免测试真的拉起进程。
func TestSwitchToClashNodeSetsMihomoKernel(t *testing.T) {
	s, cfg, mhm, url := newClashTestServer(t)
	seedClashGroup(t, cfg, mhm, "g1", "家宽", url)
	mhm.SetMihomoBin(filepath.Join(t.TempDir(), "no-such-mihomo"))

	req := httptest.NewRequest("POST", "/api/node/x", nil)
	req.SetPathValue("id", config.ClashNodeID("g1", "🏠 JP-家宽-01"))
	rec := httptest.NewRecorder()
	s.apiSwitch(rec, req)

	var resp map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["error"] == "" {
		t.Error("内核不存在时应当把错误返回给界面")
	}

	cfg.Lock()
	kernel, node, grp := cfg.Kernel, cfg.ClashNode, cfg.ActiveGrp
	cfg.Unlock()
	if kernel != config.KernelMihomo {
		t.Errorf("内核 = %q, 期望 mihomo", kernel)
	}
	if node != "🏠 JP-家宽-01" {
		t.Errorf("ClashNode = %q", node)
	}
	// ActiveGrp 必须一起记下来，否则重启后无从知道该加载哪个家宽分组的配置
	if grp != "g1" {
		t.Errorf("ActiveGrp = %q, 期望 g1", grp)
	}
}

// 跨分组点节点时必须换配置：mihomo 一次只加载一份，
// 不换的话会拿 B 的节点名去 A 的策略组里找，必然失败。
func TestSwitchClashNodeAcrossGroupsLoadsNewConfig(t *testing.T) {
	s, cfg, mhm, url := newClashTestServer(t)
	seedClashGroup(t, cfg, mhm, "g1", "家宽A", url)

	cfg.Lock()
	cfg.Groups = append(cfg.Groups, config.Group{
		ID: "g2", Name: "家宽B", URL: url,
		Kind: config.GroupKindClash, Nodes: []config.Node{},
	})
	cfg.Unlock()
	if _, err := mhm.FetchSubscription("g2", url, ""); err != nil {
		t.Fatalf("拉取 g2 失败: %v", err)
	}
	mhm.SetMihomoBin(filepath.Join(t.TempDir(), "no-such-mihomo"))

	req := httptest.NewRequest("POST", "/api/node/x", nil)
	req.SetPathValue("id", config.ClashNodeID("g2", "🏠 KR-家宽-01"))
	rec := httptest.NewRecorder()
	s.apiSwitch(rec, req)

	if got := mhm.LoadedGroup(); got != "g2" {
		t.Errorf("LoadedGroup = %q, 期望切到 g2", got)
	}
	if !mhm.HasConfig() {
		t.Error("切换分组后必须重新生成运行配置")
	}
	cfg.Lock()
	grp, node := cfg.ActiveGrp, cfg.ClashNode
	cfg.Unlock()
	if grp != "g2" || node != "🏠 KR-家宽-01" {
		t.Errorf("生效分组/节点 = %q/%q, 期望 g2/🏠 KR-家宽-01", grp, node)
	}
}

// 点普通节点要把内核标记切回 xray（节点不存在导致启动失败也不影响这个标记）
func TestSwitchToPlainNodeSetsXrayKernel(t *testing.T) {
	s, cfg, _, _ := newClashTestServer(t)
	cfg.Lock()
	cfg.Kernel = config.KernelMihomo
	cfg.Unlock()

	req := httptest.NewRequest("POST", "/api/node/nope", nil)
	req.SetPathValue("id", "nope")
	rec := httptest.NewRecorder()
	s.apiSwitch(rec, req)

	cfg.Lock()
	kernel := cfg.Kernel
	cfg.Unlock()
	if kernel != config.KernelXray {
		t.Errorf("内核 = %q, 期望 xray", kernel)
	}
}

// 删掉正在生效的家宽分组时，必须把内核切回 xray 并清干净内核侧的文件。
// 否则 mihomo 会继续跑一份已经不存在的分组的配置，而界面上已经看不到它了。
func TestDelClashGroupCleansUp(t *testing.T) {
	s, cfg, mhm, url := newClashTestServer(t)
	seedClashGroup(t, cfg, mhm, "g1", "家宽", url)

	cfg.Lock()
	cfg.Kernel = config.KernelMihomo
	cfg.ActiveGrp = "g1"
	cfg.ClashNode = "🏠 JP-家宽-01"
	cfg.Unlock()

	req := httptest.NewRequest("POST", "/api/group/g1/del", nil)
	req.SetPathValue("id", "g1")
	rec := httptest.NewRecorder()
	s.apiDelGroup(rec, req)

	cfg.Lock()
	kernel, node, grp := cfg.Kernel, cfg.ClashNode, cfg.ActiveGrp
	hasGroup := cfg.FindClashGroup("g1") != nil
	cfg.Unlock()

	if hasGroup {
		t.Error("分组没被删掉")
	}
	if kernel != config.KernelXray {
		t.Errorf("内核 = %q, 删掉生效的家宽分组后必须切回 xray", kernel)
	}
	if node != "" || grp != "" {
		t.Errorf("残留 ClashNode=%q ActiveGrp=%q", node, grp)
	}
	if n := mhm.GroupNodeCount("g1"); n != 0 {
		t.Errorf("内核侧节点数没清零: %d", n)
	}
	if got := mhm.LoadedGroup(); got != "" {
		t.Errorf("LoadedGroup = %q, 期望清空", got)
	}
	if mhm.HasConfig() {
		t.Error("运行配置没清掉，会残留成一个看不见的幽灵分组")
	}
}

// 家宽订阅已经改从「添加订阅分组」进，设置接口不再接收 clashSubUrl。
// 老前端可能还在发这个字段，必须忽略而不是报错 —— 否则用户点保存会莫名失败。
func TestSettingsIgnoresClashSubUrl(t *testing.T) {
	s, cfg, _, _ := newClashTestServer(t)

	if err := s.applySettings(map[string]interface{}{
		"clashSubUrl": "http://a/b",
		"socksPort":   float64(10809),
	}); err != nil {
		t.Fatalf("带 clashSubUrl 的设置请求不该报错: %v", err)
	}

	cfg.Lock()
	sub, port, groups := cfg.ClashSubURL, cfg.SocksPort, len(cfg.ClashGroups())
	cfg.Unlock()
	if sub != "" {
		t.Errorf("clashSubUrl = %q, 期望被忽略", sub)
	}
	if port != 10809 {
		t.Errorf("SOCKS5 端口 = %d, 同一请求里的其它字段仍应生效", port)
	}
	if groups != 0 {
		t.Error("设置接口不该凭空造出家宽分组")
	}
}

// TestClashRoutes 走真实路由表，验证端点确实注册成功、路径没写错。
// 用 ServeHTTP + ResponseRecorder 而不是起真实端口，测试不依赖网络。
func TestClashRoutes(t *testing.T) {
	s, cfg, mhm, url := newClashTestServer(t)
	seedClashGroup(t, cfg, mhm, "g1", "家宽", url)
	mux := s.newMux()

	do := func(method, path, body string) *httptest.ResponseRecorder {
		var rd io.Reader
		if body != "" {
			rd = strings.NewReader(body)
		}
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(method, path, rd))
		return rec
	}

	// 首页要能出（内联资源都嵌在 HTML 里，页面本身就是前端）
	if rec := do("GET", "/", ""); rec.Code != 200 || !strings.Contains(rec.Body.String(), "v2aynn-web") {
		t.Errorf("首页异常: code=%d body=%.80s", rec.Code, rec.Body.String())
	}

	// 状态接口要带内核标记与家宽开关
	rec := do("GET", "/api/status", "")
	var st map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatalf("status 解析失败: %v", err)
	}
	if st["kernel"] != config.KernelXray {
		t.Errorf("默认内核 = %v, 期望 xray", st["kernel"])
	}
	if st["clashEnabled"] != true {
		t.Errorf("clashEnabled = %v, 期望 true", st["clashEnabled"])
	}

	// 设置接口要报告家宽分组数量（家宽订阅本身已不在这里配置）
	rec = do("GET", "/api/settings", "")
	var set map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &set); err != nil {
		t.Fatalf("settings 解析失败: %v", err)
	}
	if set["clashGroupCount"] != float64(1) {
		t.Errorf("clashGroupCount = %v, 期望 1", set["clashGroupCount"])
	}

	// 分组列表要把家宽分组标出来
	rec = do("GET", "/api/groups", "")
	if !strings.Contains(rec.Body.String(), `"clash":true`) {
		t.Errorf("分组列表没标出家宽分组: %s", rec.Body.String())
	}

	// 家宽节点列表：ID 里必须带分组
	rec = do("GET", "/api/group/g1/nodes", "")
	if !strings.Contains(rec.Body.String(), config.ClashNodeIDPrefix+"g1:") {
		t.Errorf("家宽节点列表异常: %s", rec.Body.String())
	}

	// 整组测速：走探针，内核没跑也能测（这正是本次修复的核心行为）
	rec = do("POST", "/api/ping/group/g1", "")
	var pg []map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &pg); err != nil {
		t.Fatalf("整组测速解析失败: %v (body=%s)", err, rec.Body.String())
	}
	if len(pg) != 2 {
		t.Errorf("整组测速应返回 2 个节点的结果, 得到 %d: %s", len(pg), rec.Body.String())
	}
}
