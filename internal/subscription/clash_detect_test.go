package subscription

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"

	"v2aynn-web/internal/config"
)

// cfnew 家宽订阅的精简版，组结构照真实订阅：url-test 组当 dialer、
// select 组列家宽节点、顶层 select 组被规则 MATCH 指向。
const clashSubBody = `mixed-port: 7890
proxies:
  - name: "🏠 JP-家宽-01"
    type: openvpn
    server: 1.2.3.4
    port: 1776
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
rules:
  - MATCH,🏠 家宽节点
`

// 同样有 proxies / proxy-groups 段，但里面没有 openvpn 节点 ——
// 是别人的 Clash 订阅，不是 cfnew 的家宽订阅。
const plainClashBody = `proxies:
  - name: "JP-01"
    type: ss
    server: 1.2.3.4
    port: 8388
    cipher: aes-128-gcm
    password: x
proxy-groups:
  - name: "节点选择"
    type: select
    proxies:
      - "JP-01"
`

func newTestConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg, err := config.Load(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatalf("加载测试配置失败: %v", err)
	}
	return cfg
}

func addGroup(t *testing.T, cfg *config.Config, id, name, url string, kind string) {
	t.Helper()
	cfg.Lock()
	cfg.Groups = append(cfg.Groups, config.Group{
		ID: id, Name: name, URL: url, Kind: kind, Nodes: []config.Node{},
	})
	cfg.Unlock()
}

// 「添加订阅分组」这条路径要能自己认出 cfnew 的家宽订阅：它是一份完整的
// Clash 配置，必须整份交给 mihomo 内核，不能当节点列表去解析。
func TestFetchGroupMarksClashSub(t *testing.T) {
	cfg := newTestConfig(t)
	fake := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		_, _ = rw.Write([]byte(clashSubBody))
	}))
	defer fake.Close()

	addGroup(t, cfg, "g1", "家宽", fake.URL, config.GroupKindNormal)
	FetchGroup(cfg, "g1")

	cfg.Lock()
	defer cfg.Unlock()
	if cfg.FindClashGroup("g1") == nil {
		t.Fatal("家宽订阅没有被识别出来，会被当成普通订阅去解析节点")
	}
	// Nodes 必须留空：混进 AllNodes() 会被 xray 的故障转移当成候选节点
	if n := len(cfg.AllNodes()); n != 0 {
		t.Errorf("家宽分组贡献了 %d 个节点，应当为 0", n)
	}
}

// 别人的 Clash 订阅（没有 openvpn 节点）不能被误标成家宽分组：
// 标错了以后它会被丢给 mihomo，而内核里根本解析不出节点，整个分组直接变空。
func TestFetchGroupKeepsPlainClashNormal(t *testing.T) {
	cfg := newTestConfig(t)
	fake := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		_, _ = rw.Write([]byte(plainClashBody))
	}))
	defer fake.Close()

	addGroup(t, cfg, "g1", "普通Clash", fake.URL, config.GroupKindNormal)
	FetchGroup(cfg, "g1")

	cfg.Lock()
	defer cfg.Unlock()
	if cfg.FindClashGroup("g1") != nil {
		t.Error("没有 openvpn 节点的 Clash 订阅被误标成了家宽分组")
	}
}

// 定时刷新（FetchAll）必须跳过家宽分组：那些订阅只有 mihomo 认得，
// 走这里的解析器只会白拉一次再报错，白白刷日志。
func TestFetchAllSkipsClashGroups(t *testing.T) {
	cfg := newTestConfig(t)
	var hits int32
	fake := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		_, _ = rw.Write([]byte(clashSubBody))
	}))
	defer fake.Close()

	addGroup(t, cfg, "g1", "家宽", fake.URL, config.GroupKindClash)
	FetchAll(cfg)

	if n := atomic.LoadInt32(&hits); n != 0 {
		t.Errorf("FetchAll 不该去拉家宽分组，实际请求了 %d 次", n)
	}
}

func TestLooksLikeClashDetection(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		isVPN   bool
		isClash bool
	}{
		{"家宽订阅", clashSubBody, true, true},
		{"普通Clash订阅", plainClashBody, false, true},
		{"base64节点列表", "dmxlc3M6Ly9leGFtcGxlLmNvbTo0NDM=", false, false},
		{"空内容", "", false, false},
		{"只有proxies没有proxy-groups", "proxies:\n  - name: a\n", false, false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := looksLikeClashVPN(c.body); got != c.isVPN {
				t.Errorf("looksLikeClashVPN = %v, 期望 %v", got, c.isVPN)
			}
			if got := looksLikeClash(c.body); got != c.isClash {
				t.Errorf("looksLikeClash = %v, 期望 %v", got, c.isClash)
			}
		})
	}
}
