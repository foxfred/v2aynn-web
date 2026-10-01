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
    url: https://www.gstatic.com/generate_204
    proxies:
      - "优选域名-01"
      - "联通-09"
      - "联通-07"
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

// clashSubWithNodes 造一份含 n 个家宽节点的精简订阅，组结构与 clashSubSample 一致。
//
// 用来测「按通过比例判断前置好不好」—— 样本太小（2 个节点）时比例没有意义。
func clashSubWithNodes(n int) string {
	var b strings.Builder
	b.WriteString("mixed-port: 7890\nmode: rule\nexternal-controller: 127.0.0.1:9090\n\nproxies:\n")
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&b, "  - name: \"🏠 N-%02d\"\n    type: openvpn\n    server: 10.0.0.%d\n    port: 1776\n    dialer-proxy: \"⚡ CF前置\"\n", i, i)
	}
	b.WriteString("proxy-groups:\n  - name: \"⚡ CF前置\"\n    type: url-test\n    url: https://www.gstatic.com/generate_204\n    proxies:\n      - \"优选域名-01\"\n      - \"联通-09\"\n      - \"联通-07\"\n  - name: \"🏠 家宽节点\"\n    type: select\n    proxies:\n")
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&b, "      - \"🏠 N-%02d\"\n", i)
	}
	b.WriteString("  - name: \"🚀 节点选择\"\n    type: select\n    proxies:\n      - \"🏠 家宽节点\"\n      - DIRECT\n\nrules:\n  - MATCH,🚀 节点选择\n")
	return b.String()
}

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
	// nows 各策略组「当前在用哪个节点」。真实内核里这是 /proxies/{组} 的 now 字段。
	nows map[string]string
	// probe 覆盖 ProxyDelay 的行为。nil 时按 delays 表查。
	//
	// 需要「前置选错 ⇒ 家宽节点全挂」这类因果链的用例用它 —— 直接写一个闭包，
	// 比在替身里堆一堆开关清楚得多。
	probe func(name string) (int, error)
	// frontSet 记录 SetFront 的调用序列（含最后那次「拨回原样」）。
	// front 是「我们让它用哪个」，空串表示没动过。
	frontSet []string
	front    string
	// refuse 记下「内核不会采用」的节点。
	//
	// 真实内核里 url-test 组的固定只是「偏好」：被固定的节点若在它自己的账本里
	// 是「不活」（判据 = 能不能直接访问订阅里写的那个测速地址），PUT 过去 now
	// 纹丝不动。盒子实测 216 个 CF 前置里约一半如此（2026-10-01）。
	refuse map[string]bool
	// probes 记录每次 ProxyDelay 的「内核当时实际在用哪个前置 + 被量的节点名」，
	// 用 "<前置>|<节点>" 存。用来验「内核不采用的候选压根没去量」——
	// 拒收时那次测量其实仍走在旧前置上，这个记录能把它抓出来。
	probes []string
	// frontGroup 前置组的组名（SetFront 时记下），拼 probes 时用。
	frontGroup string
	// ensured 记录每次 Ensure 的调用（组 ID），与成败无关。
	//
	// 用来验「某条路径到底有没有预热探针」：换配置路径会先把探针 Stop 掉，
	// 漏了配对的预热时这里就是空的（2026-10-01 真机踩过）。
	ensured []string
}

func newFakeProber() *fakeProber {
	return &fakeProber{
		frontGroup: "⚡ CF前置",
		delays: map[string]int{
			"🏠 JP-家宽-01": 480,
			"🏠 KR-家宽-01": 620,
		},
		nows: map[string]string{
			// 前置组当前用的节点。刻意让它**不在** delays 表里 ——
			// 这正是「拿组名去测会落到一个说不清是谁的节点」那个坑的替身。
			"⚡ CF前置": "优选域名-01",
		},
	}
}

// currentFront 「我们让它用哪个节点」（没动过时是空串）。
// 注意它与 effectiveFront 的区别 —— 前者是意图，后者是内核的实际行为。
func (f *fakeProber) currentFront() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.front
}

// effectiveFront 内核此刻**实际**在用哪个前置节点（对应真实内核 /proxies 的 now）。
//
// 与 currentFront 的区别正是本文件要测的那个坑：url-test 组固定节点时会先检查
// 该节点在自己账本里活不活，不活就静默忽略、回退到它自己挑的那个。此时
// currentFront 变了，effectiveFront 没变。
func (f *fakeProber) effectiveFront(group string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.nows[group]
}

// frontCalls 返回 SetFront 的调用序列副本。
func (f *fakeProber) frontCalls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.frontSet...)
}

func (f *fakeProber) Ensure(groupID string, resident bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ensured = append(f.ensured, groupID)
	if f.startErr != nil {
		return f.startErr
	}
	f.group, f.running = groupID, true
	return nil
}

// ensureCount 至今被预热过几次。换配置路径漏了预热时恒为 0。
func (f *fakeProber) ensureCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.ensured)
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
	fn := f.probe
	ms, ok := f.delays[name]
	f.probes = append(f.probes, f.nows[f.frontGroup]+"|"+name)
	f.mu.Unlock()
	if fn != nil {
		return fn(name)
	}
	if !ok || ms <= 0 {
		return 0, fmt.Errorf("节点无响应")
	}
	return ms, nil
}

// probeCountUnder 数「测量发生时内核实际在用 front 这个前置」的次数。
//
// 内核拒收某个候选时，那一次测量其实仍走在旧前置上 —— 这个计数能把它抓出来。
func (f *fakeProber) probeCountUnder(front string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, p := range f.probes {
		if strings.HasPrefix(p, front+"|") {
			n++
		}
	}
	return n
}

// SetFront 对应真实内核的 PUT /proxies/{组}（内核侧是 ForceSet）。
//
// ★ 「固定」不等于「生效」：url-test 组只会在被固定的节点「活着」时才采用它，
// 否则静默回退到自己挑的那个。refuse 里的节点用来模拟后者 —— 此时 frontSet
// 记下了这次调用（PUT 确实发出去了），但 nows 不动（内核没采用）。
func (f *fakeProber) SetFront(group, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.frontSet = append(f.frontSet, name)
	f.frontGroup = group
	f.front = name
	if f.refuse[name] {
		return nil
	}
	if f.nows == nil {
		f.nows = map[string]string{}
	}
	f.nows[group] = name
	return nil
}

// DelaysOf 对应读内核缓存的 /proxies（只读，不触发测速）。
func (f *fakeProber) DelaysOf(names []string) map[string]int {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[string]int, len(names))
	for _, n := range names {
		if ms, ok := f.delays[n]; ok {
			out[n] = ms
		}
	}
	return out
}

// ProxyNow 对应真实内核 /proxies/{组} 的 now 字段。
func (f *fakeProber) ProxyNow(name string) (string, error) {
	f.mu.Lock()
	now, ok := f.nows[name]
	f.mu.Unlock()
	if !ok || now == "" {
		return "", fmt.Errorf("内核没报出[%s]当前用哪个节点", name)
	}
	return now, nil
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

// ★ 回归：家宽节点自己测得出延迟时，绝不能因为「前置通道探测」把整轮结果挡掉。
//
// 这正是「网关检测不稳定」的根因：内核的 /proxies/{组}/delay 走的是组的 fast()，
// 而 fast() 在组里还没有延迟历史时**无条件取成员列表的第一个节点、且不检查它死活**。
// 于是「探前置通道」实际变成了「探订阅里第一个 CF 节点」—— 它恰好挂着就误报
// 「前置不通」，另外两百多个明明好好的；订阅一刷新节点顺序变了，同一个分组
// 又时好时坏。
//
// 这里让 delays 表里**没有**任何以组名出现的前置条目（等价于 fast() 挑中的那个
// 节点测不通），而两个家宽节点都正常 —— 期望照常返回节点结果，不报错。
func TestPingClashGroupNotBlockedByFrontProbe(t *testing.T) {
	s, cfg, mhm, url := newClashTestServer(t)
	seedClashGroup(t, cfg, mhm, "g1", "家宽", url)

	req := httptest.NewRequest("POST", "/api/ping/group/g1", nil)
	req.SetPathValue("id", "g1")
	rec := httptest.NewRecorder()
	s.apiPingGroup(rec, req)

	var res []map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("应当返回节点结果数组，实际 body=%s", rec.Body.String())
	}
	if len(res) != 2 {
		t.Fatalf("应当返回 2 个节点的结果, 得到 %d (body=%s)", len(res), rec.Body.String())
	}
}

// ★ 核心：前置通道被内核自动选成了一个「带不动家宽链」的节点时，整组测速
// 必须自己换一个能用的，而不是把「全部超时」丢给用户。
//
// 这是用户报的「家宽测速测出来的活链接，选用后一会儿就失效了，再测全部家宽
// 都是超时」的根因修复。全部家宽节点的出口都挤在前置通道组的同一个节点上，
// 而那个组是 url-test —— 内核按「它自己访问 gstatic 快不快」挑，这个指标与
// 「能不能承载一条 OpenVPN 长连接」毫无关系（盒子实测：自己 171ms 的带不动
// 家宽链、197ms 的反而能）。选错了 70 多个节点一起陪葬。
func TestPingClashGroupSwitchesToWorkingFront(t *testing.T) {
	s, cfg, mhm, url := newClashTestServer(t)
	seedClashGroup(t, cfg, mhm, "g1", "家宽", url)

	f := s.prober.(*fakeProber)
	f.mu.Lock()
	// 内核此刻用的是「优选域名-01」（延迟最低的那个），但它带不动家宽链；
	// 「联通-09」自己稍慢，却能承载家宽链。
	f.nows = map[string]string{"⚡ CF前置": "优选域名-01"}
	f.delays = map[string]int{"优选域名-01": 120, "联通-09": 190, "联通-07": 260}
	f.probe = func(name string) (int, error) {
		if name == "🏠 JP-家宽-01" || name == "🏠 KR-家宽-01" {
			if f.currentFront() == "联通-09" {
				return 500, nil
			}
			return 0, errors.New("节点无响应")
		}
		return 120, nil
	}
	f.mu.Unlock()

	req := httptest.NewRequest("POST", "/api/ping/group/g1", nil)
	req.SetPathValue("id", "g1")
	rec := httptest.NewRecorder()
	s.apiPingGroup(rec, req)

	var res []map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("换前置之后应当照常返回节点结果, 实际 body=%s", rec.Body.String())
	}
	if len(res) != 2 {
		t.Fatalf("应当返回 2 个节点的结果, 得到 %d (body=%s)", len(res), rec.Body.String())
	}

	// 候选按「内核记录的延迟从低到高」排，第一个就通，所以只该动一次
	if calls := f.frontCalls(); len(calls) != 1 || calls[0] != "联通-09" {
		t.Errorf("SetFront 调用序列 = %v, 期望只切一次到 联通-09", calls)
	}
	// 必须落盘：测速走的是探针，用户点节点后跑的是内核，不落盘内核会自己挑回错的
	cfg.Lock()
	pinned := cfg.FindClashGroup("g1").FrontNode
	cfg.Unlock()
	if pinned != "联通-09" {
		t.Errorf("换好的前置没落盘, FrontNode = %q", pinned)
	}
}

// 换遍了候选还是不通时，如实说清试过谁，并提示「换一批节点」——
// 这种情况大概率是这批家宽节点集体掉线，而不是前置的问题。
func TestPingClashGroupReportsWhenNoFrontWorks(t *testing.T) {
	s, cfg, mhm, url := newClashTestServer(t)
	seedClashGroup(t, cfg, mhm, "g1", "家宽", url)

	f := s.prober.(*fakeProber)
	f.mu.Lock()
	f.nows = map[string]string{"⚡ CF前置": "优选域名-01"}
	f.delays = map[string]int{"优选域名-01": 120}
	// 不管换成哪个前置，家宽节点都连不上
	f.probe = func(name string) (int, error) { return 0, errors.New("节点无响应") }
	f.mu.Unlock()

	req := httptest.NewRequest("POST", "/api/ping/group/g1", nil)
	req.SetPathValue("id", "g1")
	rec := httptest.NewRecorder()
	s.apiPingGroup(rec, req)

	var resp map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	msg := resp["error"]
	for _, want := range []string{"优选域名-01", "联通-09", "联通-07", "更新"} {
		if !strings.Contains(msg, want) {
			t.Errorf("提示里应当包含 %q, 实际: %q", want, msg)
		}
	}
	// 全都不行时要把前置拨回原来那个，别留下一个「我们随手挑的」状态
	if got := f.currentFront(); got != "优选域名-01" {
		t.Errorf("试遍候选都不行后应把前置拨回原样, 实际停在 %q", got)
	}
	// 全都不通时不该留下半截结果
	cfg.Lock()
	g := cfg.FindClashGroup("g1")
	_, has := g.Probe("🏠 JP-家宽-01")
	pinned := g.FrontNode
	cfg.Unlock()
	if has {
		t.Error("全都不通时不该产出节点结果")
	}
	if pinned != "" {
		t.Errorf("没验出可用前置时不该落盘, FrontNode = %q", pinned)
	}
}

// 前置本来就是好的（样本里能连通）时什么都不动 —— 换前置是全局动作，
// 没有真凭实据不能做。
func TestPingClashGroupKeepsFrontWhenItWorks(t *testing.T) {
	s, cfg, mhm, url := newClashTestServer(t)
	seedClashGroup(t, cfg, mhm, "g1", "家宽", url)

	f := s.prober.(*fakeProber)
	f.mu.Lock()
	f.probe = func(name string) (int, error) { return 500, nil }
	f.mu.Unlock()

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
	if calls := f.frontCalls(); len(calls) != 0 {
		t.Errorf("前置正常时不该动它, 实际调用 %v", calls)
	}
	cfg.Lock()
	pinned := cfg.FindClashGroup("g1").FrontNode
	cfg.Unlock()
	if pinned != "" {
		t.Errorf("没换前置时不该落盘, FrontNode = %q", pinned)
	}
}

// ★ 盒子实测出来的回归：前置「勉强能用」也必须换掉。
//
// 判据是**通过比例**，不是「有没有通」。盒子实测（2026-10-01）：
//
//	前置 = 联通-01（内核自动挑的）  同一批 6 个样本通 0 个，整组 65 个只测出 6 个
//	前置 = 优选域名-03             同一批 6 个样本通 4 个
//	前置 = 优选域名-04             同一批 6 个样本通 5 个
//
// 只要求「有一个通就算前置可用」，就会把 `联通-01` 这种放过去 ——
// 表现出来就是「测速测出来的活节点没几个」，修复在最常见的场景下等于没生效。
func TestPingClashGroupSwitchesWhenFrontIsBarelyUsable(t *testing.T) {
	s, cfg, mhm, url := newClashTestServerWith(t, clashSubWithNodes(6))
	seedClashGroup(t, cfg, mhm, "g1", "家宽", url)

	f := s.prober.(*fakeProber)
	f.mu.Lock()
	f.nows = map[string]string{"⚡ CF前置": "优选域名-01"}
	f.delays = map[string]int{"优选域名-01": 120}
	f.probe = func(name string) (int, error) {
		if !strings.HasPrefix(name, "🏠 ") {
			return 120, nil // CF 节点自己的延迟，与家宽链无关
		}
		// 内核自动挑的那个前置：6 个样本只放 2 个过去（不到一半）
		// 换成 联通-09 之后：全都通
		if f.currentFront() == "联通-09" {
			return 500, nil
		}
		if name == "🏠 N-01" || name == "🏠 N-02" {
			return 500, nil
		}
		return 0, errors.New("节点无响应")
	}
	f.mu.Unlock()

	req := httptest.NewRequest("POST", "/api/ping/group/g1", nil)
	req.SetPathValue("id", "g1")
	rec := httptest.NewRecorder()
	s.apiPingGroup(rec, req)

	var res []map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("换前置之后应当照常返回节点结果, 实际 body=%s", rec.Body.String())
	}
	if len(res) != 6 {
		t.Fatalf("应当返回 6 个节点的结果, 得到 %d (body=%s)", len(res), rec.Body.String())
	}
	if got := f.currentFront(); got != "联通-09" {
		t.Errorf("前置只通 2/6（不到一半）时必须换掉, 实际停在 %q", got)
	}
	cfg.Lock()
	pinned := cfg.FindClashGroup("g1").FrontNode
	cfg.Unlock()
	if pinned != "联通-09" {
		t.Errorf("换好的前置没落盘, FrontNode = %q", pinned)
	}
}

// ★ 盒子实测出来的第二个坑：url-test 组的「固定」只是偏好，不是强制。
//
// mihomo v1.19.31 adapter/outboundgroup/urltest.go 的 fast() 会先看被固定的节点
// 在自己账本里活不活（判据 = 能不能直接访问订阅里写的那个测速地址），不活就
// **静默忽略**、回退到它自己挑的那个。盒子实测 216 个 CF 前置里约一半 PUT 过去
// now 纹丝不动（它们的延迟测试直接返回 503）。
//
// 不确认就会出大问题：「换上前置 c → 量样本」这一步量到的其实是内核回退后仍在
// 用的那个节点，候选之间的比较全成噪声 —— 于是一个真正能用的前置可能被判成
// 「没用」而放弃。所以内核不采用的候选必须跳过、继续试下一个。
func TestPingClashGroupSkipsFrontKernelRefuses(t *testing.T) {
	s, cfg, mhm, url := newClashTestServer(t)
	seedClashGroup(t, cfg, mhm, "g1", "家宽", url)

	f := s.prober.(*fakeProber)
	f.mu.Lock()
	f.nows = map[string]string{"⚡ CF前置": "优选域名-01"}
	// 候选按内核记录的延迟从低到高排：联通-09(190) 在 联通-07(260) 前面
	f.delays = map[string]int{"优选域名-01": 120, "联通-09": 190, "联通-07": 260}
	// 内核不采用「联通-09」，只接受「联通-07」
	f.refuse = map[string]bool{"联通-09": true}
	f.probe = func(name string) (int, error) {
		if !strings.HasPrefix(name, "🏠 ") {
			return 120, nil
		}
		if f.effectiveFront("⚡ CF前置") == "联通-07" {
			return 500, nil
		}
		return 0, errors.New("节点无响应")
	}
	f.mu.Unlock()

	req := httptest.NewRequest("POST", "/api/ping/group/g1", nil)
	req.SetPathValue("id", "g1")
	rec := httptest.NewRecorder()
	s.apiPingGroup(rec, req)

	var res []map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("应当照常返回节点结果, 实际 body=%s", rec.Body.String())
	}
	if len(res) != 2 {
		t.Fatalf("应当返回 2 个节点的结果, 得到 %d (body=%s)", len(res), rec.Body.String())
	}
	if got := f.effectiveFront("⚡ CF前置"); got != "联通-07" {
		t.Errorf("内核不采用的候选必须跳过、继续试下一个, 实际生效的是 %q", got)
	}
	cfg.Lock()
	pinned := cfg.FindClashGroup("g1").FrontNode
	cfg.Unlock()
	if pinned != "联通-07" {
		t.Errorf("真正生效的那个前置才该落盘, FrontNode = %q", pinned)
	}
	// ★ 被内核拒收的候选压根不该去量它的样本：量出来的是内核回退后仍在用的
	// 那个节点（= 当前前置），除了白等十几秒、把「试过谁」的记录搞乱之外毫无意义。
	// 判据是「在内核实际还用着旧前置时做了几次测量」—— 只有最初那一次体检该是
	// 这个状态（样本 2 个 ⇒ 2 次）；漏掉这个判断就会多出一轮 = 4 次。
	if n := f.probeCountUnder("优选域名-01"); n != 2 {
		t.Errorf("内核实际仍用旧前置时测量了 %d 次, 期望 2 次 —— 内核不采用的候选不该被测量", n)
	}
}

// 候选全都被内核拒收时，提示语要说清是「内核没用它们」，而不是「它们更差」——
// 两者的处理建议完全不同（前者换一批前置，后者换一批家宽节点）。
func TestPingClashGroupReportsWhenKernelRefusesAllCandidates(t *testing.T) {
	s, cfg, mhm, url := newClashTestServer(t)
	seedClashGroup(t, cfg, mhm, "g1", "家宽", url)

	f := s.prober.(*fakeProber)
	f.mu.Lock()
	f.nows = map[string]string{"⚡ CF前置": "优选域名-01"}
	f.delays = map[string]int{"优选域名-01": 120, "联通-09": 190, "联通-07": 260}
	f.refuse = map[string]bool{"联通-09": true, "联通-07": true}
	f.probe = func(name string) (int, error) { return 0, errors.New("节点无响应") }
	f.mu.Unlock()

	req := httptest.NewRequest("POST", "/api/ping/group/g1", nil)
	req.SetPathValue("id", "g1")
	rec := httptest.NewRecorder()
	s.apiPingGroup(rec, req)

	var resp map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	msg := resp["error"]
	for _, want := range []string{"内核一个都没采用", "联通-09", "联通-07"} {
		if !strings.Contains(msg, want) {
			t.Errorf("提示里应当包含 %q, 实际: %q", want, msg)
		}
	}
	// 一个都没生效 ⇒ 不该落盘
	cfg.Lock()
	pinned := cfg.FindClashGroup("g1").FrontNode
	cfg.Unlock()
	if pinned != "" {
		t.Errorf("没有候选真正生效时不该落盘, FrontNode = %q", pinned)
	}
}

// 「够用」的门槛是样本的一半（向上取整）。
func TestFrontMinAliveIsHalf(t *testing.T) {
	for n, want := range map[int]int{0: 0, 1: 1, 2: 1, 3: 2, 4: 2, 6: 3, 8: 4} {
		if got := frontMinAlive(n); got != want {
			t.Errorf("frontMinAlive(%d) = %d, 期望 %d", n, got, want)
		}
	}
}

// spreadSample 要等距取样：订阅里节点按地区聚堆排，取前 n 个会全落在同一个
// 国家的同一个服务端上，那一批集体掉线时会被误判成「前置通道不通」。
func TestSpreadSampleSpreadsAcrossRegions(t *testing.T) {
	names := []string{
		"🏠 JP-家宽-01", "🏠 JP-家宽-02", "🏠 JP-家宽-03",
		"🏠 KR-家宽-01", "🏠 KR-家宽-02", "🏠 KR-家宽-03",
		"🏠 TH-家宽-01", "🏠 TH-家宽-02", "🏠 TH-家宽-03",
	}
	got := spreadSample(names, 3)
	if len(got) != 3 {
		t.Fatalf("样本数 = %d, 期望 3 (%v)", len(got), got)
	}
	// 等距步长 3：0/3/6 —— 恰好覆盖三个地区
	want := []string{"🏠 JP-家宽-01", "🏠 KR-家宽-01", "🏠 TH-家宽-01"}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("样本[%d] = %q, 期望 %q（应当分散到不同地区）", i, got[i], want[i])
		}
	}

	// 节点数比样本数还少时全给出来，不能返回空的
	short := spreadSample([]string{"a", "b"}, 8)
	if len(short) != 2 {
		t.Errorf("节点不够时应当全给出来, 得到 %v", short)
	}
	if spreadSample(nil, 8) != nil {
		t.Error("空列表应当返回 nil")
	}
}

// 前置候选：按内核记录的延迟从低到高排，没测过的排最后，明确测过不通的丢掉，
// 当前正在用的那个也排除掉（已经试过了）。
func TestPickFrontCandidatesSkipsDeadAndCurrent(t *testing.T) {
	members := []string{"优选域名-01", "联通-09", "联通-07", "挂了-01", "没测过-01"}
	delays := map[string]int{
		"优选域名-01": 120,
		"联通-09":   260,
		"联通-07":   190,
		"挂了-01":   0, // 测过但不通
	}
	got := pickFrontCandidates(members, delays, "优选域名-01")
	want := []string{"联通-07", "联通-09", "没测过-01"}
	if len(got) != len(want) {
		t.Fatalf("候选 = %v, 期望 %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("候选[%d] = %q, 期望 %q", i, got[i], want[i])
		}
	}
	// 没有内核缓存时退化成订阅顺序，不能崩
	if got := pickFrontCandidates(members, nil, ""); len(got) != len(members) {
		t.Errorf("没有延迟数据时应当保留全部候选, 得到 %v", got)
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

// 内核记录里的 0 不能覆盖我们自己扫出来的延迟。
//
// 家宽节点除了 select 组，还挂在订阅的 fallback 组（🏠 家宽自动）下，而
// mihomo 的整组健康检查**没有并发限制** —— 它会把几十个节点一次性全打出去，
// 这些家宽节点的出口又全挤在同一条前置通道里，一挤爆就大批失败，失败会往
// history 里写一条 {"delay":0}（2026-10-01 盒子实测确认）。
//
// 若让这条 0 覆盖掉我们自己的扫描结果，界面就会把明明测通的节点显示成
// 「超时」—— 用户看到的就是「以前有延迟的节点全变超时了」。
// 我们自己的扫描是限并发 4 + 每节点错峰 0–200ms 的，对家宽链更可信。
func TestClashNodePingKernelZeroDoesNotOverrideStored(t *testing.T) {
	cases := []struct {
		name    string
		stored  int
		live    int
		hasLive bool
		want    int
	}{
		{"内核记 0（fallback 健康检查失败）不能盖掉我们测到的 480", 480, 0, true, 480},
		{"内核记负数同样不能盖掉", 480, -1, true, 480},
		{"内核确实测到延迟时以内核为准", 480, 300, true, 300},
		{"内核没这个节点的记录时用存下来的", 480, 0, false, 480},
		{"两边都没测到 -> 0（界面留空）", 0, 0, true, 0},
		{"我们测过不通(-1) 且内核没记录 -> 保持 -1", -1, 0, false, -1},
	}
	for _, c := range cases {
		if got := clashNodePing(c.stored, c.live, c.hasLive); got != c.want {
			t.Errorf("%s: clashNodePing(%d,%d,%v) = %d, 期望 %d",
				c.name, c.stored, c.live, c.hasLive, got, c.want)
		}
	}
}

// 家宽分组的「延迟↑ / 延迟↓」排序必须生效。
//
// 前端是**纯服务端排序**（下拉框只发一次 /api/sort，然后重拉列表），而
// apiGroupNodes 对家宽分组会直接短路到 writeClashNodes —— 修复前那里压根没读
// SortOrder，还把响应里的 sort 写死成空串，于是家宽分组点排序没有任何反应。
func TestClashNodesRespectSortOrder(t *testing.T) {
	s, cfg, mhm, url := newClashTestServer(t)
	seedClashGroup(t, cfg, mhm, "g1", "家宽", url)

	// 故意让「延迟顺序」与「订阅顺序」相反，排序生效与否一眼可辨。
	cfg.Lock()
	g := cfg.FindClashGroup("g1")
	g.SetProbeMS("🏠 JP-家宽-01", 620)
	g.SetProbeMS("🏠 KR-家宽-01", 480)
	cfg.Unlock()

	fetch := func(t *testing.T) (names []string, sortField string) {
		t.Helper()
		req := httptest.NewRequest("GET", "/api/group/g1/nodes", nil)
		req.SetPathValue("id", "g1")
		rec := httptest.NewRecorder()
		s.apiGroupNodes(rec, req)
		var resp struct {
			Nodes []config.Node `json:"nodes"`
			Sort  string        `json:"sort"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("解析响应失败: %v", err)
		}
		for _, n := range resp.Nodes {
			names = append(names, n.Name)
		}
		return names, resp.Sort
	}
	setOrder := func(o string) {
		cfg.Lock()
		cfg.SortOrder = o
		cfg.Unlock()
	}

	// 默认（不排序）：原样保持订阅顺序，sort 如实回显空串
	def, sortField := fetch(t)
	if sortField != "" {
		t.Errorf("默认排序时响应里的 sort 应为空串, 得到 %q", sortField)
	}
	if len(def) != 2 {
		t.Fatalf("家宽节点数 = %d, 期望 2", len(def))
	}
	if def[0] != "🏠 JP-家宽-01" || def[1] != "🏠 KR-家宽-01" {
		t.Fatalf("不排序时应原样返回订阅顺序, 得到 %v", def)
	}

	// 延迟↑：480ms 的 KR 必须排到 620ms 的 JP 前面（与订阅顺序相反）
	setOrder("ping_asc")
	asc, sortField := fetch(t)
	if sortField != "ping_asc" {
		t.Errorf("响应里的 sort = %q, 期望 ping_asc", sortField)
	}
	if len(asc) != 2 || asc[0] != "🏠 KR-家宽-01" {
		t.Errorf("延迟↑ 没生效, 得到 %v（期望 KR 在前）", asc)
	}

	// 延迟↓：反过来
	setOrder("ping_desc")
	desc, _ := fetch(t)
	if len(desc) != 2 || desc[0] != "🏠 JP-家宽-01" {
		t.Errorf("延迟↓ 没生效, 得到 %v（期望 JP 在前）", desc)
	}

	// 测过但不通(-1) 必须沉底；从没测过(0) 同理，由同一个比较器保证
	cfg.Lock()
	g = cfg.FindClashGroup("g1")
	g.SetProbeMS("🏠 KR-家宽-01", -1)
	cfg.Unlock()
	setOrder("ping_asc")
	asc, _ = fetch(t)
	if len(asc) != 2 || asc[0] != "🏠 JP-家宽-01" || asc[1] != "🏠 KR-家宽-01" {
		t.Errorf("超时节点应沉底, 得到 %v", asc)
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

	// 状态接口要带内核标记、生效的节点类型与家宽开关
	rec := do("GET", "/api/status", "")
	var st map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatalf("status 解析失败: %v", err)
	}
	// 内核统一之后只有一个内核在跑，kernel 恒为 mihomo；
	// 「当前生效的是普通节点」改由 nodeKind 表达。
	if st["kernel"] != config.KindClash {
		t.Errorf("内核 = %v, 期望 mihomo（统一后只有一个内核）", st["kernel"])
	}
	if st["nodeKind"] != config.KindNormal {
		t.Errorf("默认生效的节点类型 = %v, 期望 xray(普通节点)", st["nodeKind"])
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
