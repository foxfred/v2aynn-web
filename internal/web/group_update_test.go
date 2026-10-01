package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"v2aynn-web/internal/config"
)

// hangServer 起一个「永远不返回」的订阅源。
//
// 为什么需要它：apiUpdateGroup 处理完同步部分后会 go subscription.FetchGroup(...)
// 异步去拉订阅。如果让它真的拉回来，用例断言的就不再是「更新接口干了什么」，
// 而是「拉取 + carryOver 干了什么」，而且时序不确定。让请求挂住，状态就停在
// 同步部分刚做完的样子，断言才稳定。
func hangServer(t *testing.T) string {
	t.Helper()
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		<-release
	}))
	t.Cleanup(func() {
		close(release)
		srv.Close()
	})
	return srv.URL
}

// seedSubGroup 塞一个带订阅地址、带测速结果的普通分组。
func seedSubGroup(t *testing.T, cfg *config.Config, url string) {
	t.Helper()
	cfg.Lock()
	cfg.Groups = append(cfg.Groups, config.Group{
		ID: "sub1", Name: "订阅", URL: url, SubProxy: "http://127.0.0.1:10810",
		Nodes: []config.Node{
			{ID: "n1", Name: "联通-01", Protocol: "vless", Server: "1.1.1.1", Port: "443",
				UUID: "u1", Ping: 88},
			{ID: "n2", Name: "移动-02", Protocol: "vless", Server: "2.2.2.2", Port: "443",
				UUID: "u2", Ping: -1},
		},
	})
	cfg.Unlock()
}

func postGroupUpdate(t *testing.T, s *WebServer, body string) {
	t.Helper()
	req := httptest.NewRequest("POST", "/api/group/sub1/update", strings.NewReader(body))
	req.SetPathValue("id", "sub1")
	req.Header.Set("Content-Type", "application/json")
	s.apiUpdateGroup(httptest.NewRecorder(), req)
}

func groupByID(t *testing.T, cfg *config.Config, id string) config.Group {
	t.Helper()
	cfg.Lock()
	defer cfg.Unlock()
	for _, g := range cfg.Groups {
		if g.ID == id {
			return g
		}
	}
	t.Fatalf("分组 %s 不存在", id)
	return config.Group{}
}

// 只改订阅代理（订阅地址没变）时，必须保留整组节点与测速结果。
//
// 这条守的是用户报的「已经测过速的链接不要轻易刷新没了」：原来是无条件
// cfg.Groups[i].Nodes = []config.Node{}，于是「只想换一下订阅代理」也会把
// 整组节点连同测过的延迟一起清掉，而随后的异步拉取又救不回来（旧数据源
// 已经被清空了）。2026-10-01 真机实测：改 new3.1 的订阅代理，
// 288 个节点、165 条测速结果直接清成 0。
func TestUpdateGroupKeepsNodesWhenOnlyProxyChanges(t *testing.T) {
	url := hangServer(t)
	s, cfg, _, _ := newClashTestServer(t)
	seedSubGroup(t, cfg, url)

	postGroupUpdate(t, s, `{"name":"订阅","url":"`+url+`","subProxy":""}`)

	g := groupByID(t, cfg, "sub1")
	if g.SubProxy != "" {
		t.Errorf("订阅代理应当被清空，实际 %q", g.SubProxy)
	}
	if len(g.Nodes) != 2 {
		t.Fatalf("只改订阅代理不该清空节点，期望 2 个，实际 %d 个", len(g.Nodes))
	}
	if g.Nodes[0].Ping != 88 || g.Nodes[1].Ping != -1 {
		t.Errorf("测速结果应当原样保留（含 -1 超时标记），实际 ping=%d/%d",
			g.Nodes[0].Ping, g.Nodes[1].Ping)
	}
}

// 订阅地址真的换了才清空节点 —— 换了源之后旧节点已无意义。
func TestUpdateGroupClearsNodesWhenURLChanges(t *testing.T) {
	old := hangServer(t)
	s, cfg, _, _ := newClashTestServer(t)
	seedSubGroup(t, cfg, old)

	newURL := hangServer(t) + "/another"
	postGroupUpdate(t, s, `{"name":"订阅","url":"`+newURL+`","subProxy":""}`)

	g := groupByID(t, cfg, "sub1")
	if g.URL != newURL {
		t.Errorf("订阅地址应当已更新为 %q，实际 %q", newURL, g.URL)
	}
	if len(g.Nodes) != 0 {
		t.Errorf("换了订阅地址应当清空旧节点，实际还剩 %d 个", len(g.Nodes))
	}
}
