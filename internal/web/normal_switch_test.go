package web

import (
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"v2aynn-web/internal/config"
)

// 内核统一（阶段二）：普通节点也交给 mihomo。
// 本文件覆盖 web 层这一侧的分派 —— 点普通节点要把生效类型切成普通节点，
// 并让内核换成普通节点配置；状态接口要能分别报告「内核」与「生效的节点类型」。

func seedNormalGroup(t *testing.T, cfg *config.Config) {
	t.Helper()
	cfg.Lock()
	cfg.Groups = append(cfg.Groups, config.Group{
		ID: "nrm", Name: "订阅",
		Nodes: []config.Node{
			{ID: "n1", Name: "联通-01", Protocol: "trojan", Server: "1.1.1.1",
				Port: "443", Password: "p", Network: "ws", Path: "/"},
			{ID: "n2", Name: "移动-02", Protocol: "vless", Server: "2.2.2.2",
				Port: "443", UUID: "u-1", Network: "ws", Path: "/"},
		},
	})
	cfg.Unlock()
}

// 点普通节点：生效类型要切成普通节点，内核要换成普通节点配置，
// 而且不能还挂着上一个家宽分组（留着会让状态接口拼出假的家宽节点 ID）。
func TestSwitchPlainNodeLoadsNormalConfigIntoMihomo(t *testing.T) {
	s, cfg, mhm, url := newClashTestServer(t)
	seedClashGroup(t, cfg, mhm, "g1", "家宽", url)
	seedNormalGroup(t, cfg)
	// 内核路径指向不存在的文件：Start 必然失败，但配置生成与状态标记都要先完成。
	// 这样测试既不依赖真进程，又能验证「换了配置」这件事确实发生了。
	mhm.SetMihomoBin(filepath.Join(t.TempDir(), "no-such-mihomo"))

	cfg.Lock()
	cfg.Kernel = config.KindClash
	cfg.ActiveGrp = "g1"
	cfg.ClashNode = "🏠 JP-家宽-01"
	cfg.Unlock()

	req := httptest.NewRequest("POST", "/api/node/x", nil)
	req.SetPathValue("id", "n1")
	rec := httptest.NewRecorder()
	s.apiSwitch(rec, req)

	cfg.Lock()
	kind := cfg.Kernel
	cfg.Unlock()
	if kind != config.KindNormal {
		t.Errorf("生效类型 = %q, 期望普通节点", kind)
	}
	if !mhm.IsNormalMode() {
		t.Error("内核应当换成普通节点配置")
	}
	if got := mhm.LoadedGroup(); got != "" {
		t.Errorf("普通模式下不该还挂着家宽分组 %q", got)
	}
}

// 换配置路径把探针收掉后，必须把它补回来。
//
// 这条路径会先 prober.Stop()（重启内核期间省内存），早期漏了配对的预热 ⇒
// 从家宽切回普通节点后探针不会自动起来，用户下次点「测速」要多等十几秒。
// 2026-10-01 真机复现：切回后进程里只剩主内核，日志里连失败信息都没有（压根没调用）。
//
// 预热刻意用 defer 而不是写在 Start 之后 —— 本用例把内核路径指向不存在的文件让
// Start 必然失败，顺带覆盖「即使 Start 失败，探针也不能被留在停止状态」。
func TestSwitchPlainNodeConfigPathWarmsProbe(t *testing.T) {
	s, cfg, mhm, url := newClashTestServer(t)
	seedClashGroup(t, cfg, mhm, "g1", "家宽", url)
	seedNormalGroup(t, cfg)
	mhm.SetMihomoBin(filepath.Join(t.TempDir(), "no-such-mihomo"))

	cfg.Lock()
	cfg.Kernel = config.KindClash
	cfg.ActiveGrp = "g1"
	cfg.ClashNode = "🏠 JP-家宽-01"
	cfg.Unlock()

	f := s.prober.(*fakeProber)
	req := httptest.NewRequest("POST", "/api/node/x", nil)
	req.SetPathValue("id", "n1")
	s.apiSwitch(httptest.NewRecorder(), req)

	// WarmProbe 在后台 goroutine 里跑，等它落地
	deadline := time.Now().Add(2 * time.Second)
	for f.ensureCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if f.ensureCount() == 0 {
		t.Fatal("换配置路径收掉探针后没有补回来（WarmProbe 漏调用）")
	}
	if !f.IsRunning() {
		t.Error("探针应当已被重新拉起")
	}
}

// 状态接口：kernel 是真实在跑的内核（统一后恒为 mihomo），
// nodeKind 才是当前生效的节点类型 —— 界面靠后者标出「[家宽]」。
func TestStatusReportsKernelAndNodeKind(t *testing.T) {
	s, cfg, _, _ := newClashTestServer(t)

	read := func() map[string]interface{} {
		rec := httptest.NewRecorder()
		s.apiStatus(rec, httptest.NewRequest("GET", "/api/status", nil))
		var st map[string]interface{}
		if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
			t.Fatalf("status 解析失败: %v", err)
		}
		return st
	}

	cfg.Lock()
	cfg.Kernel = config.KindNormal
	cfg.Unlock()
	st := read()
	if st["kernel"] != config.KindClash {
		t.Errorf("kernel = %v, 期望 mihomo（统一后只有一个内核）", st["kernel"])
	}
	if st["nodeKind"] != config.KindNormal {
		t.Errorf("nodeKind = %v, 期望普通节点", st["nodeKind"])
	}

	cfg.Lock()
	cfg.Kernel = config.KindClash
	cfg.ClashNode = "🏠 JP-家宽-01"
	cfg.ActiveGrp = "g1"
	cfg.Unlock()
	st = read()
	if st["nodeKind"] != config.KindClash {
		t.Errorf("nodeKind = %v, 期望家宽", st["nodeKind"])
	}
}

// 普通模式下的真实测速要记到普通节点上 —— 与家宽模式相反，
// 记错字段会让两个节点的数字同时失真（这正是用户报过的「测速不准」）。
func TestSpeedInNormalModeSavedToPlainNode(t *testing.T) {
	s, cfg, _, _ := newClashTestServer(t)
	seedNormalGroup(t, cfg)

	cfg.Lock()
	cfg.Kernel = config.KindNormal
	cfg.ActiveNode = "n1"
	cfg.ActiveGrp = "g1"
	cfg.ClashNode = "🏠 JP-家宽-01"
	cfg.Unlock()

	s.saveSpeed(20)

	cfg.Lock()
	var got float64
	for _, g := range cfg.Groups {
		for _, n := range g.Nodes {
			if n.ID == "n1" {
				got = n.Speed
			}
		}
	}
	cfg.Unlock()
	if got != 20 {
		t.Errorf("普通节点速度 = %v, 期望 20", got)
	}
}
