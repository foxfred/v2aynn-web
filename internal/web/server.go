package web

import (
	"bufio"
	"crypto/tls"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"sort"
	"time"

	"v2aynn-web/internal/config"
	"v2aynn-web/internal/subscription"
	"v2aynn-web/internal/v2ray"
)

//go:embed static/index.html
var indexHTML string

//go:embed static/*
var staticFS embed.FS

// speedTestURL 真实下载测速用的目标（Cloudflare 2MB 文件，全球 CDN 稳定可达）
const speedTestURL = "https://speed.cloudflare.com/__down?bytes=2000000"

type WebServer struct {
	cfg *config.Config
	v2m *v2ray.Manager
}

func NewServer(cfg *config.Config, v2m *v2ray.Manager) *WebServer {
	v2m.SetConfig(cfg)
	return &WebServer{cfg: cfg, v2m: v2m}
}

func (w *WebServer) Run(addr string) error {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/status", w.apiStatus)
	mux.HandleFunc("GET /api/nodes", w.apiNodes)
	mux.HandleFunc("GET /api/subs", w.apiSubs)
	mux.HandleFunc("POST /api/subs", w.apiAddSub)
	mux.HandleFunc("POST /api/subs/{id}", w.apiDelSub)
	mux.HandleFunc("POST /api/fetch", w.apiFetch)
	mux.HandleFunc("POST /api/node/{id}", w.apiSwitch)
	mux.HandleFunc("POST /api/ping/{id}", w.apiPing)
	mux.HandleFunc("POST /api/pingall", w.apiPingAll)
	mux.HandleFunc("POST /api/speed", w.apiSpeed)
	mux.HandleFunc("POST /api/sort", w.apiSort)
	mux.HandleFunc("POST /api/proxy/start", w.apiStart)
	mux.HandleFunc("POST /api/proxy/stop", w.apiStop)
	mux.HandleFunc("GET /api/settings", w.apiGetSettings)
	mux.HandleFunc("POST /api/settings", w.apiSettings)
	subFS, _ := fs.Sub(staticFS, "static")
	mux.Handle("/static/", http.FileServer(http.FS(subFS)))
	mux.HandleFunc("/", w.apiIndex)

	return http.ListenAndServe(addr, corsHandler(mux))
}

func (w *WebServer) apiIndex(rw http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(rw, r)
		return
	}
	rw.Header().Set("Content-Type", "text/html")
	rw.Write([]byte(indexHTML))
}

func (w *WebServer) apiStatus(rw http.ResponseWriter, r *http.Request) {
	w.writeJSON(rw, w.v2m.Status())
}

func (w *WebServer) apiNodes(rw http.ResponseWriter, r *http.Request) {
	w.cfg.Lock()
	nodes := make([]config.Node, len(w.cfg.Nodes))
	copy(nodes, w.cfg.Nodes)
	active := w.cfg.ActiveNode
	sortOrder := w.cfg.SortOrder
	w.cfg.Unlock()

	// 按延迟排序: ping>0 的节点按数字排序放前面，超时(-1)/未测(0)放最后
	switch sortOrder {
	case "ping_asc":
		sortNodes(nodes, true)
	case "ping_desc":
		sortNodes(nodes, false)
	}

	w.writeJSON(rw, map[string]interface{}{"nodes": nodes, "active": active, "sort": sortOrder})
}

// apiSort 保存排序方式
func (w *WebServer) apiSort(rw http.ResponseWriter, r *http.Request) {
	var req struct {
		Order string `json:"order"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.writeJSON(rw, map[string]string{"error": "bad json"})
		return
	}
	if req.Order != "" && req.Order != "ping_asc" && req.Order != "ping_desc" {
		w.writeJSON(rw, map[string]string{"error": "bad order"})
		return
	}
	w.cfg.Lock()
	w.cfg.SortOrder = req.Order
	_ = w.cfg.Save()
	w.cfg.Unlock()
	w.writeJSON(rw, map[string]string{"ok": "true"})
}

// sortNodes 对节点按ping排序，valid ping优先，asc=true升序否则降序
func sortNodes(nodes []config.Node, asc bool) {
	sort.SliceStable(nodes, func(i, j int) bool {
		pi, pj := nodes[i].Ping, nodes[j].Ping
		vi, vj := pi > 0, pj > 0
		if vi != vj {
			return vi
		}
		if !vi {
			return false
		}
		if asc {
			return pi < pj
		}
		return pi > pj
	})
}

func (w *WebServer) apiSubs(rw http.ResponseWriter, r *http.Request) {
	w.cfg.Lock()
	subs := make([]config.Sub, len(w.cfg.Subs))
	copy(subs, w.cfg.Subs)
	w.cfg.Unlock()
	w.writeJSON(rw, subs)
}

func (w *WebServer) apiAddSub(rw http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
		URL  string `json:"url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.writeJSON(rw, map[string]string{"error": "bad json"})
		return
	}
	w.cfg.Lock()
	w.cfg.Subs = append(w.cfg.Subs, config.Sub{
		ID:   fmt.Sprintf("%d", time.Now().UnixNano()),
		Name: req.Name, URL: req.URL,
	})
	_ = w.cfg.Save()
	w.cfg.Unlock()
	w.writeJSON(rw, map[string]string{"ok": "true"})
}

func (w *WebServer) apiDelSub(rw http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	w.cfg.Lock()
	subs := make([]config.Sub, 0)
	for _, s := range w.cfg.Subs {
		if s.ID != id {
			subs = append(subs, s)
		}
	}
	w.cfg.Subs = subs
	nodes := make([]config.Node, 0)
	for _, n := range w.cfg.Nodes {
		if n.SubID != id {
			nodes = append(nodes, n)
		}
	}
	w.cfg.Nodes = nodes
	_ = w.cfg.Save()
	w.cfg.Unlock()
	w.writeJSON(rw, map[string]string{"ok": "true"})
}

func (w *WebServer) apiFetch(rw http.ResponseWriter, r *http.Request) {
	go subscription.Fetch(w.cfg)
	w.writeJSON(rw, map[string]string{"ok": "true"})
}

func (w *WebServer) apiSwitch(rw http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := w.v2m.SwitchNode(id); err != nil {
		w.writeJSON(rw, map[string]string{"error": err.Error()})
		return
	}
	w.writeJSON(rw, map[string]string{"ok": "true"})
}

func (w *WebServer) apiPing(rw http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	w.cfg.Lock()
	var node *config.Node
	for i := range w.cfg.Nodes {
		if w.cfg.Nodes[i].ID == id {
			node = &w.cfg.Nodes[i]
			break
		}
	}
	w.cfg.Unlock()
	if node == nil {
		// 节点在测速期间被订阅刷新替换为新ID: 返回超时而非 error,
		// 避免前端拿到 undefined 结果显示空白("丢失测速信息")
		w.writeJSON(rw, map[string]interface{}{"id": id, "ms": -1})
		return
	}
	nodeCopy := *node
	ms, reachable := probeNode(nodeCopy, 3500*time.Millisecond)
	if !reachable {
		ms = -1
	}
	w.cfg.Lock()
	for i := range w.cfg.Nodes {
		if w.cfg.Nodes[i].ID == id {
			w.cfg.Nodes[i].Ping = int(ms)
			break
		}
	}
	_ = w.cfg.Save()
	w.cfg.Unlock()
	w.writeJSON(rw, map[string]interface{}{"id": id, "ms": ms})
}

func (w *WebServer) apiPingAll(rw http.ResponseWriter, r *http.Request) {
	w.cfg.Lock()
	nodes := make([]config.Node, len(w.cfg.Nodes))
	copy(nodes, w.cfg.Nodes)
	w.cfg.Unlock()

	type result struct {
		id   string
		ms   int64
		name string
		key  string // server:port:protocol, 用于跨Fetch周期匹配
	}
	ch := make(chan result, len(nodes))
	sem := make(chan struct{}, 5) // 最多5个并发

	for _, node := range nodes {
		go func(n config.Node) {
			sem <- struct{}{}
			key := n.Server + ":" + n.Port + ":" + n.Protocol
			ms, reachable := probeNode(n, 3500*time.Millisecond)
			if !reachable {
				ms = -1
			}
			<-sem
			ch <- result{id: n.ID, ms: ms, name: n.Name, key: key}
		}(node)
	}

	results := make([]map[string]interface{}, 0, len(nodes))
	pingMap := make(map[string]int) // key -> ms, 用key而非ID确保跨Fetch正确
	for i := 0; i < len(nodes); i++ {
		r := <-ch
		results = append(results, map[string]interface{}{
			"id": r.id, "ms": r.ms, "name": r.name,
		})
		// 保存所有非零结果（含 -1 超时），确保测速后超时节点持久化为超时而非旧值
		if r.ms != 0 {
			pingMap[r.key] = int(r.ms)
		}
	}

	// 保存测速结果到配置（按 server:port:protocol 匹配，跨Fetch周期安全）
	w.cfg.Lock()
	for i := range w.cfg.Nodes {
		key := w.cfg.Nodes[i].Server + ":" + w.cfg.Nodes[i].Port + ":" + w.cfg.Nodes[i].Protocol
		if p, ok := pingMap[key]; ok {
			w.cfg.Nodes[i].Ping = p
		}
	}
	_ = w.cfg.Save()
	w.cfg.Unlock()

	w.writeJSON(rw, results)
}

// apiSpeed 对【当前激活节点】做真实下载测速。
// 真实吞吐必须穿过节点隧道，而 xray 只以激活节点运行，故只能测正在用的节点。
// 通过本地 HTTP 代理端口把流量导入隧道，下载 Cloudflare 2MB 文件计算 Mbps。
func (w *WebServer) apiSpeed(rw http.ResponseWriter, r *http.Request) {
	if !w.v2m.IsRunning() {
		w.writeJSON(rw, map[string]interface{}{"ok": false, "error": "代理未运行，请先启动", "mbps": 0})
		return
	}
	proxyURL, _ := url.Parse(fmt.Sprintf("http://127.0.0.1:%d", w.cfg.HttpPort))
	client := &http.Client{
		Timeout: 15 * time.Second,
		Transport: &http.Transport{
			Proxy: http.ProxyURL(proxyURL),
		},
	}

	start := time.Now()
	resp, err := client.Get(speedTestURL)
	if err != nil {
		w.writeJSON(rw, map[string]interface{}{"ok": false, "error": "下载失败: " + err.Error(), "mbps": 0})
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		w.writeJSON(rw, map[string]interface{}{"ok": false, "error": fmt.Sprintf("HTTP %d", resp.StatusCode), "mbps": 0})
		return
	}

	n, err := io.Copy(io.Discard, resp.Body)
	elapsed := time.Since(start)
	if err != nil {
		w.writeJSON(rw, map[string]interface{}{"ok": false, "error": "下载中断: " + err.Error(), "mbps": 0})
		return
	}
	if elapsed <= 0 {
		w.writeJSON(rw, map[string]interface{}{"ok": false, "error": "耗时异常", "mbps": 0})
		return
	}
	mbps := float64(n) * 8.0 / 1e6 / elapsed.Seconds()

	// 把测得的真实速度写回激活节点，列表里也能看到
	w.cfg.Lock()
	for i := range w.cfg.Nodes {
		if w.cfg.Nodes[i].ID == w.cfg.ActiveNode {
			w.cfg.Nodes[i].Speed = mbps
			break
		}
	}
	_ = w.cfg.Save()
	w.cfg.Unlock()

	w.writeJSON(rw, map[string]interface{}{
		"ok":    true,
		"mbps":  mbps,
		"bytes": n,
		"ms":    elapsed.Milliseconds(),
	})
}

func (w *WebServer) apiStart(rw http.ResponseWriter, r *http.Request) {
	if err := w.v2m.Start(); err != nil {
		w.writeJSON(rw, map[string]string{"error": err.Error()})
		return
	}
	w.writeJSON(rw, map[string]string{"ok": "true"})
}

func (w *WebServer) apiStop(rw http.ResponseWriter, r *http.Request) {
	w.v2m.Stop()
	w.writeJSON(rw, map[string]string{"ok": "true"})
}

func (w *WebServer) apiGetSettings(rw http.ResponseWriter, r *http.Request) {
	w.cfg.Lock()
	st := map[string]interface{}{
		"socksPort": w.cfg.SocksPort, "httpPort": w.cfg.HttpPort,
		"listenAddr": w.cfg.ListenAddr, "subRefresh": w.cfg.SubRefresh,
		"subProxy": w.cfg.SubProxy, "proxyMode": w.cfg.ProxyMode,
	}
	w.cfg.Unlock()
	w.writeJSON(rw, st)
}

func (w *WebServer) apiSettings(rw http.ResponseWriter, r *http.Request) {
	var req map[string]interface{}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.writeJSON(rw, map[string]string{"error": "bad json"})
		return
	}
	w.cfg.Lock()
	if v, ok := req["socksPort"]; ok {
		w.cfg.SocksPort = int(v.(float64))
	}
	if v, ok := req["httpPort"]; ok {
		w.cfg.HttpPort = int(v.(float64))
	}
	if v, ok := req["listenAddr"]; ok {
		w.cfg.ListenAddr = v.(string)
	}
	if v, ok := req["subRefresh"]; ok {
		w.cfg.SubRefresh = int(v.(float64))
	}
	if v, ok := req["subProxy"]; ok {
		w.cfg.SubProxy = v.(string)
	}
	if v, ok := req["proxyMode"]; ok {
		w.cfg.ProxyMode = v.(string)
	}
	_ = w.cfg.Save()
	w.cfg.Unlock()
	if w.v2m.IsRunning() {
		w.v2m.Stop()
		time.Sleep(300 * time.Millisecond)
		_ = w.v2m.Start()
	}
	w.writeJSON(rw, map[string]string{"ok": "true"})
}

func (w *WebServer) writeJSON(rw http.ResponseWriter, data interface{}) {
	rw.Header().Set("Content-Type", "application/json")
	rw.Header().Set("Access-Control-Allow-Origin", "*")
	json.NewEncoder(rw).Encode(data)
}

func corsHandler(h http.Handler) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		rw.Header().Set("Access-Control-Allow-Origin", "*")
		rw.Header().Set("Access-Control-Allow-Methods", "GET,POST,OPTIONS")
		rw.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == "OPTIONS" {
			return
		}
		h.ServeHTTP(rw, r)
	})
}

// probeNode 测速：返回 (延迟ms, 是否可达)。
// 方案：TCP 连接 + (若启用TLS)真实握手。这是【连通性】信号，用于筛掉死节点。
// 采样策略：首次成功立即返回（快的节点一次搞定）；仅失败时重试一次，
// 显著加快全量测速，避免每个节点的 3 次等待叠加。
func probeNode(n config.Node, timeout time.Duration) (int64, bool) {
	addr := net.JoinHostPort(n.Server, n.Port)
	useTLS := n.TLS == "tls" || n.TLS == "xtls" || n.TLS == "reality" ||
		n.Protocol == "trojan" || n.Security == "tls" || n.Security == "reality"

	// 第一次尝试
	if ms, reach := probeOnce(n, addr, useTLS, timeout); reach {
		return ms, true
	}
	// 首次失败（可能瞬时网络抖动），再试一次
	if ms, reach := probeOnce(n, addr, useTLS, timeout); reach {
		return ms, true
	}
	return -1, false
}

func probeOnce(n config.Node, addr string, useTLS bool, timeout time.Duration) (int64, bool) {
	start := time.Now()
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return -1, false
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(timeout))

	if useTLS {
		// TLS 握手验证：能完成握手说明节点的 TLS 层真实可用
		sni := n.SNI
		if sni == "" {
			sni = n.RequestHost
		}
		if sni == "" {
			sni = n.Server
		}
		tconn := tls.Client(conn, &tls.Config{
			ServerName:         sni,
			InsecureSkipVerify: true, // 节点IP与证书未必匹配，只验证握手
			MinVersion:         tls.VersionTLS12,
		})
		if err := tconn.Handshake(); err != nil {
			return -1, false
		}
		// 读服务器首字节验证协议栈活跃；多数服务器不会主动推送，超时也算可达
		br := bufio.NewReader(tconn)
		tconn.SetReadDeadline(time.Now().Add(250 * time.Millisecond))
		_, _ = br.ReadByte()
		elapsed := time.Since(start).Milliseconds()
		if elapsed < 1 {
			elapsed = 1
		}
		return elapsed, true
	}

	// 非 TLS：TCP 连上即认为可达（vless/vmess 服务器不会主动推送数据）
	if n.Protocol == "vmess" || n.Protocol == "vless" {
		conn.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
		buf := make([]byte, 1)
		_, _ = conn.Read(buf)
	}
	elapsed := time.Since(start).Milliseconds()
	if elapsed < 1 {
		elapsed = 1
	}
	return elapsed, true
}
