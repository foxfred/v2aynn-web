package web

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

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

// newClashTestServer 造一个带家宽通道的 WebServer。
// 订阅源用本地假服务，既避免测试依赖外网，也能顺带校验 UA 伪装。
func newClashTestServer(t *testing.T) (*WebServer, *config.Config, *mihomo.Manager) {
	t.Helper()
	dir := t.TempDir()
	cfg, err := config.Load(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatalf("加载测试配置失败: %v", err)
	}

	fake := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		// 订阅服务端按 UA 决定返回 Clash 配置还是普通文本，UA 不对就拿不到家宽节点
		if !strings.Contains(strings.ToLower(r.Header.Get("User-Agent")), "clash") {
			_, _ = rw.Write([]byte("plain text, not a clash config"))
			return
		}
		_, _ = rw.Write([]byte(clashSubSample))
	}))
	t.Cleanup(fake.Close)

	cfg.Lock()
	cfg.ClashSubURL = fake.URL
	cfg.Unlock()

	v2m := v2ray.NewManager(dir)
	mhm := mihomo.NewManager(dir)
	return NewServer(cfg, v2m, mhm), cfg, mhm
}

func TestFetchClashSub(t *testing.T) {
	s, cfg, mhm := newClashTestServer(t)

	if err := s.fetchClashSub(); err != nil {
		t.Fatalf("拉取家宽订阅失败: %v", err)
	}

	if n := len(mhm.Nodes()); n != 2 {
		t.Fatalf("家宽节点数 = %d, 期望 2", n)
	}
	cfg.Lock()
	cached := len(cfg.ClashNodes)
	cfg.Unlock()
	if cached != 2 {
		t.Errorf("config.ClashNodes = %d, 期望 2", cached)
	}
	// 配置文件必须真的落盘，否则内核启动时会找不到配置
	if !mhm.HasConfig() {
		t.Error("家宽配置未落盘")
	}
	if mhm.SubLastFetch() == "" {
		t.Error("订阅时间未记录")
	}
}

func TestFetchClashSubRejectsNonClashResponse(t *testing.T) {
	dir := t.TempDir()
	cfg, err := config.Load(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatalf("加载测试配置失败: %v", err)
	}
	fake := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		_, _ = rw.Write([]byte("not yaml at all"))
	}))
	t.Cleanup(fake.Close)
	cfg.Lock()
	cfg.ClashSubURL = fake.URL
	cfg.Unlock()

	s := NewServer(cfg, v2ray.NewManager(dir), mihomo.NewManager(dir))
	if err := s.fetchClashSub(); err == nil {
		t.Error("订阅内容里没有 openvpn 节点时应当报错")
	}
}

func TestClashGroupAppearsInGroups(t *testing.T) {
	s, _, _ := newClashTestServer(t)
	if err := s.fetchClashSub(); err != nil {
		t.Fatalf("拉取家宽订阅失败: %v", err)
	}

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
		if resp.Groups[i].ID == config.ClashGroupID {
			clash = &resp.Groups[i]
		}
	}
	if clash == nil {
		t.Fatal("分组列表里没有家宽分组")
	}
	if !clash.Clash || clash.NodeCount != 2 {
		t.Errorf("家宽分组属性不对: %+v", *clash)
	}
}

// 没配家宽订阅时，界面上不应出现家宽分组
func TestNoClashGroupWithoutSubscription(t *testing.T) {
	dir := t.TempDir()
	cfg, err := config.Load(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatalf("加载测试配置失败: %v", err)
	}
	s := NewServer(cfg, v2ray.NewManager(dir), mihomo.NewManager(dir))

	rec := httptest.NewRecorder()
	s.apiGroups(rec, httptest.NewRequest("GET", "/api/groups", nil))

	if strings.Contains(rec.Body.String(), config.ClashGroupID) {
		t.Errorf("未配置家宽时不应出现家宽分组: %s", rec.Body.String())
	}
}

func TestClashNodesList(t *testing.T) {
	s, _, _ := newClashTestServer(t)
	if err := s.fetchClashSub(); err != nil {
		t.Fatalf("拉取家宽订阅失败: %v", err)
	}

	req := httptest.NewRequest("GET", "/api/group/x/nodes", nil)
	req.SetPathValue("id", config.ClashGroupID)
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
	// ID 必须带前缀，服务端与前端都靠它分流
	if resp.Nodes[0].ID != config.ClashNodeIDPrefix+resp.Nodes[0].Name {
		t.Errorf("节点 ID 未带前缀: %q", resp.Nodes[0].ID)
	}
	if resp.Nodes[0].Protocol != "openvpn" {
		t.Errorf("节点协议 = %q, 期望 openvpn", resp.Nodes[0].Protocol)
	}
	// 内核没跑家宽时不应有激活项，否则界面会同时标出两个「当前使用中」
	if resp.Active != "" {
		t.Errorf("未启用家宽时 active 应为空, 得到 %q", resp.Active)
	}
}

// 家宽节点的测速由 mihomo 内核代劳（本程序的 TCP 探测对 OpenVPN 隧道没意义）。
// 内核没跑起来时，接口必须如实报错并给出 ms=-1，让界面显示「超时」而不是假装成功。
func TestPingClashNodeWithoutKernel(t *testing.T) {
	s, _, _ := newClashTestServer(t)
	req := httptest.NewRequest("POST", "/api/ping/x", nil)
	req.SetPathValue("id", config.ClashNodeIDPrefix+"🏠 JP-家宽-01")
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
	if resp.MS != -1 {
		t.Errorf("内核不可用时 ms 应为 -1, 得到 %d", resp.MS)
	}
	if resp.Error == "" {
		t.Error("内核不可用时应当返回 error，方便用户排查")
	}
	if resp.ID != config.ClashNodeIDPrefix+"🏠 JP-家宽-01" {
		t.Errorf("返回的 id 必须原样带回（界面靠它定位行）: %q", resp.ID)
	}
}

// 整组测速走的是另一条路径：内核不可用时不能 panic，也不能返回 200 空结果，
// 必须让界面能区分「全都不通」和「内核没起来」。
func TestPingClashGroupWithoutKernel(t *testing.T) {
	s, _, _ := newClashTestServer(t)
	req := httptest.NewRequest("POST", "/api/ping/group/"+config.ClashGroupID, nil)
	req.SetPathValue("id", config.ClashGroupID)
	rec := httptest.NewRecorder()
	s.apiPingGroup(rec, req)

	var resp struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if resp.Error == "" {
		t.Error("内核不可用时整组测速应当返回 error")
	}
}

// 点家宽节点要把内核标记切到 mihomo，并把选中的节点名落盘。
// 这里故意把内核路径指向不存在的文件，避免测试真的拉起进程。
func TestSwitchToClashNodeSetsMihomoKernel(t *testing.T) {
	s, cfg, mhm := newClashTestServer(t)
	if err := s.fetchClashSub(); err != nil {
		t.Fatalf("拉取家宽订阅失败: %v", err)
	}
	mhm.SetMihomoBin(filepath.Join(t.TempDir(), "no-such-mihomo"))

	req := httptest.NewRequest("POST", "/api/node/x", nil)
	req.SetPathValue("id", config.ClashNodeIDPrefix+"🏠 JP-家宽-01")
	rec := httptest.NewRecorder()
	s.apiSwitch(rec, req)

	var resp map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["error"] == "" {
		t.Error("内核不存在时应当把错误返回给界面")
	}

	cfg.Lock()
	kernel, node := cfg.Kernel, cfg.ClashNode
	cfg.Unlock()
	if kernel != config.KernelMihomo {
		t.Errorf("内核 = %q, 期望 mihomo", kernel)
	}
	if node != "🏠 JP-家宽-01" {
		t.Errorf("ClashNode = %q", node)
	}
}

// 点普通节点要把内核标记切回 xray（节点不存在导致启动失败也不影响这个标记）
func TestSwitchToPlainNodeSetsXrayKernel(t *testing.T) {
	s, cfg, _ := newClashTestServer(t)
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

func TestApplySettingsClashSubURL(t *testing.T) {
	s, cfg, _ := newClashTestServer(t)

	if err := s.applySettings(map[string]interface{}{"clashSubUrl": "  http://a/b  "}); err != nil {
		t.Fatalf("设置家宽地址失败: %v", err)
	}
	cfg.Lock()
	got := cfg.ClashSubURL
	cfg.Unlock()
	if got != "http://a/b" {
		t.Errorf("clashSubUrl = %q, 期望去掉首尾空格", got)
	}

	// 空串必须能清空，否则用户永远关不掉家宽
	if err := s.applySettings(map[string]interface{}{"clashSubUrl": ""}); err != nil {
		t.Fatalf("清空家宽地址失败: %v", err)
	}
	cfg.Lock()
	got = cfg.ClashSubURL
	cfg.Unlock()
	if got != "" {
		t.Errorf("clashSubUrl = %q, 期望被清空", got)
	}

	// 类型不符要报错而不是 panic（历史死锁缺陷的同类场景）
	if err := s.applySettings(map[string]interface{}{"clashSubUrl": 123}); err == nil {
		t.Error("数字冒充字符串时应当报错")
	}
}

// TestClashRoutes 走真实路由表，验证家宽端点确实注册成功、路径没写错。
// 用 ServeHTTP + ResponseRecorder 而不是起真实端口，测试不依赖网络。
func TestClashRoutes(t *testing.T) {
	s, _, _ := newClashTestServer(t)
	if err := s.fetchClashSub(); err != nil {
		t.Fatalf("拉取家宽订阅失败: %v", err)
	}
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

	// 设置接口要回显家宽地址
	rec = do("GET", "/api/settings", "")
	var set map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &set); err != nil {
		t.Fatalf("settings 解析失败: %v", err)
	}
	if sub, _ := set["clashSubUrl"].(string); sub == "" {
		t.Errorf("settings 未回显 clashSubUrl: %v", set)
	}
	if set["clashNodeCount"] != float64(2) {
		t.Errorf("clashNodeCount = %v, 期望 2", set["clashNodeCount"])
	}

	// 家宽节点列表
	rec = do("GET", "/api/group/"+config.ClashGroupID+"/nodes", "")
	if !strings.Contains(rec.Body.String(), config.ClashNodeIDPrefix) {
		t.Errorf("家宽节点列表异常: %s", rec.Body.String())
	}

	// 手动刷新端点
	rec = do("POST", "/api/clash/fetch", "")
	var fr map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &fr); err != nil {
		t.Fatalf("clash/fetch 解析失败: %v", err)
	}
	if fr["ok"] != "true" || fr["count"] != float64(2) {
		t.Errorf("刷新家宽订阅异常: %s", rec.Body.String())
	}
}
