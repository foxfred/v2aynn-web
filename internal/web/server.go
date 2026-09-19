package web

import (
	"bufio"
	"crypto/tls"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"math"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"v2aynn-web/internal/config"
	"v2aynn-web/internal/subscription"
	"v2aynn-web/internal/v2ray"
)

//go:embed static/index.html
var indexHTML string

//go:embed static/*
var staticFS embed.FS

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
	mux.HandleFunc("GET /api/groups", w.apiGroups)
	mux.HandleFunc("POST /api/group/add", w.apiAddGroup)
	mux.HandleFunc("POST /api/group/{id}/del", w.apiDelGroup)
	mux.HandleFunc("POST /api/group/{id}/update", w.apiUpdateGroup)
	mux.HandleFunc("POST /api/group/{id}/fetch", w.apiFetchGroup)
	mux.HandleFunc("GET /api/group/{id}/nodes", w.apiGroupNodes)
	mux.HandleFunc("POST /api/node/add", w.apiAddNode)
	mux.HandleFunc("POST /api/node/import", w.apiImportNode)
	mux.HandleFunc("POST /api/node/{id}/del", w.apiDelNode)
	mux.HandleFunc("POST /api/node/{id}", w.apiSwitch)
	mux.HandleFunc("POST /api/ping/{id}", w.apiPing)
	mux.HandleFunc("POST /api/ping/group/{id}", w.apiPingGroup)
	mux.HandleFunc("POST /api/speed", w.apiSpeed)
	mux.HandleFunc("POST /api/sort", w.apiSort)
	mux.HandleFunc("POST /api/proxy/start", w.apiStart)
	mux.HandleFunc("POST /api/proxy/stop", w.apiStop)
	mux.HandleFunc("GET /api/settings", w.apiGetSettings)
	mux.HandleFunc("POST /api/settings", w.apiSettings)
	mux.HandleFunc("GET /api/backup", w.apiBackup)
	mux.HandleFunc("POST /api/restore", w.apiRestore)
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

// apiGroups 返回所有分组概览（不含节点列表，仅含节点数）
func (w *WebServer) apiGroups(rw http.ResponseWriter, r *http.Request) {
	w.cfg.Lock()
	type groupSummary struct {
		ID        string `json:"id"`
		Name      string `json:"name"`
		URL       string `json:"url"`
		NodeCount int    `json:"nodeCount"`
		LastFetch string `json:"lastFetch"`
		SubProxy  string `json:"subProxy"`
	}
	groups := make([]groupSummary, len(w.cfg.Groups))
	for i, g := range w.cfg.Groups {
		groups[i] = groupSummary{
			ID: g.ID, Name: g.Name, URL: g.URL,
			NodeCount: len(g.Nodes), LastFetch: g.LastFetch,
			SubProxy: g.SubProxy,
		}
	}
	activeGrp := w.cfg.ActiveGrp
	w.cfg.Unlock()
	w.writeJSON(rw, map[string]interface{}{
		"groups":    groups,
		"activeGrp": activeGrp,
	})
}

// apiGroupNodes 返回指定分组的节点列表
func (w *WebServer) apiGroupNodes(rw http.ResponseWriter, r *http.Request) {
	gid := r.PathValue("id")
	w.cfg.Lock()
	var nodes []config.Node
	active := w.cfg.ActiveNode
	sortOrder := w.cfg.SortOrder
	for _, g := range w.cfg.Groups {
		if g.ID == gid {
			nodes = make([]config.Node, len(g.Nodes))
			copy(nodes, g.Nodes)
			break
		}
	}
	w.cfg.Unlock()
	if nodes == nil {
		w.writeJSON(rw, map[string]interface{}{"nodes": []config.Node{}, "active": active, "sort": sortOrder})
		return
	}

	switch sortOrder {
	case "ping_asc":
		sortNodes(nodes, true)
	case "ping_desc":
		sortNodes(nodes, false)
	}

	w.writeJSON(rw, map[string]interface{}{"nodes": nodes, "active": active, "sort": sortOrder, "groupId": gid})
}

// apiAddGroup 添加订阅分组
func (w *WebServer) apiAddGroup(rw http.ResponseWriter, r *http.Request) {
	var req struct {
		Name     string `json:"name"`
		URL      string `json:"url"`
		SubProxy string `json:"subProxy"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.writeJSON(rw, map[string]string{"error": "bad json"})
		return
	}
	if req.Name == "" || req.URL == "" {
		w.writeJSON(rw, map[string]string{"error": "name and url required"})
		return
	}
	id := fmt.Sprintf("%d", time.Now().UnixNano())
	w.cfg.Lock()
	w.cfg.Groups = append(w.cfg.Groups, config.Group{
		ID:   id,
		Name: req.Name, URL: req.URL, SubProxy: req.SubProxy,
		Nodes: []config.Node{},
	})
	_ = w.cfg.Save()
	w.cfg.Unlock()
	// 异步拉取
	go subscription.FetchGroup(w.cfg, id)
	w.writeJSON(rw, map[string]string{"id": id, "ok": "true"})
}

// apiDelGroup 删除订阅分组
func (w *WebServer) apiDelGroup(rw http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == config.DefaultGroupID {
		w.writeJSON(rw, map[string]string{"error": "不能删除默认分组"})
		return
	}
	w.cfg.Lock()
	groups := make([]config.Group, 0)
	for _, g := range w.cfg.Groups {
		if g.ID != id {
			groups = append(groups, g)
		}
	}
	w.cfg.Groups = groups
	if w.cfg.ActiveGrp == id {
		w.cfg.ActiveGrp = ""
	}
	_ = w.cfg.Save()
	w.cfg.Unlock()
	w.writeJSON(rw, map[string]string{"ok": "true"})
}

// apiUpdateGroup 更新订阅分组（名称/订阅URL/订阅代理），仅限有URL的订阅分组
func (w *WebServer) apiUpdateGroup(rw http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == config.DefaultGroupID {
		w.writeJSON(rw, map[string]string{"error": "默认分组不支持编辑"})
		return
	}
	var req struct {
		Name     string `json:"name"`
		URL      string `json:"url"`
		SubProxy string `json:"subProxy"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.writeJSON(rw, map[string]string{"error": "bad json"})
		return
	}
	w.cfg.Lock()
	updated := false
	for i := range w.cfg.Groups {
		if w.cfg.Groups[i].ID == id {
			if req.Name != "" {
				w.cfg.Groups[i].Name = req.Name
			}
			if req.URL != "" {
				w.cfg.Groups[i].URL = req.URL
			}
			w.cfg.Groups[i].SubProxy = req.SubProxy
			w.cfg.Groups[i].LastFetch = ""
			w.cfg.Groups[i].Nodes = []config.Node{}
			updated = true
			break
		}
	}
	_ = w.cfg.Save()
	w.cfg.Unlock()
	if !updated {
		w.writeJSON(rw, map[string]string{"error": "分组不存在"})
		return
	}
	// 更新后异步拉取新订阅
	go subscription.FetchGroup(w.cfg, id)
	w.writeJSON(rw, map[string]string{"ok": "true"})
}

// apiFetchGroup 拉取指定分组
func (w *WebServer) apiFetchGroup(rw http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	go subscription.FetchGroup(w.cfg, id)
	w.writeJSON(rw, map[string]string{"ok": "true"})
}

// apiAddNode 创建手动节点（默认分组）
func (w *WebServer) apiAddNode(rw http.ResponseWriter, r *http.Request) {
	var req struct {
		Name        string `json:"name"`
		Protocol    string `json:"protocol"`
		Server      string `json:"server"`
		Port        string `json:"port"`
		UUID        string `json:"uuid"`
		Password    string `json:"password"`
		Method      string `json:"method"`
		Network     string `json:"network"`
		TLS         string `json:"tls"`
		SNI         string `json:"sni"`
		Path        string `json:"path"`
		RequestHost string `json:"reqHost"`
		HeaderType  string `json:"headerType"`
		Security    string `json:"security"`
		AlterID     string `json:"alterId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.writeJSON(rw, map[string]string{"error": "bad json"})
		return
	}
	if req.Protocol == "" || req.Server == "" || req.Port == "" {
		w.writeJSON(rw, map[string]string{"error": "protocol/server/port required"})
		return
	}
	id := fmt.Sprintf("%d", time.Now().UnixNano())
	node := config.Node{
		ID: id, Name: req.Name, Protocol: req.Protocol,
		Server: req.Server, Port: req.Port,
		UUID: req.UUID, Password: req.Password, Method: req.Method,
		Network: req.Network, TLS: req.TLS, SNI: req.SNI,
		Path: req.Path, RequestHost: req.RequestHost,
		HeaderType: req.HeaderType, Security: req.Security, AlterID: req.AlterID,
		Ping: 0, Speed: 0,
	}
	w.cfg.Lock()
	w.cfg.EnsureDefaultGroup()
	for i := range w.cfg.Groups {
		if w.cfg.Groups[i].ID == config.DefaultGroupID {
			w.cfg.Groups[i].Nodes = append([]config.Node{node}, w.cfg.Groups[i].Nodes...)
			break
		}
	}
	_ = w.cfg.Save()
	w.cfg.Unlock()
	w.writeJSON(rw, map[string]string{"id": id, "ok": "true"})
}

// apiImportNode 导入节点到默认分组
func (w *WebServer) apiImportNode(rw http.ResponseWriter, r *http.Request) {
	var req struct {
		URLs []string `json:"urls"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.writeJSON(rw, map[string]string{"error": "bad json"})
		return
	}
	if len(req.URLs) == 0 {
		w.writeJSON(rw, map[string]string{"error": "urls required"})
		return
	}

	var added []string
	w.cfg.Lock()
	w.cfg.EnsureDefaultGroup()
	for _, raw := range req.URLs {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		if n, err := subscription.Parse(raw); err == nil {
			for i := range w.cfg.Groups {
				if w.cfg.Groups[i].ID == config.DefaultGroupID {
					w.cfg.Groups[i].Nodes = append([]config.Node{n}, w.cfg.Groups[i].Nodes...)
					break
				}
			}
			added = append(added, n.Name)
			continue
		}
		for _, line := range strings.Split(raw, "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			if n, err := subscription.Parse(line); err == nil {
				for i := range w.cfg.Groups {
					if w.cfg.Groups[i].ID == config.DefaultGroupID {
						w.cfg.Groups[i].Nodes = append([]config.Node{n}, w.cfg.Groups[i].Nodes...)
						break
					}
				}
				added = append(added, n.Name)
			}
		}
	}
	_ = w.cfg.Save()
	w.cfg.Unlock()

	w.writeJSON(rw, map[string]interface{}{
		"ok":    "true",
		"count": len(added),
		"names": added,
	})
}

// apiDelNode 删除节点
func (w *WebServer) apiDelNode(rw http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	w.cfg.Lock()
	w.cfg.RemoveNode(id)
	if w.cfg.ActiveNode == id {
		w.cfg.ActiveNode = ""
		w.cfg.ActiveGrp = ""
	}
	_ = w.cfg.Save()
	w.cfg.Unlock()
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
	node, gid := w.cfg.FindNode(id)
	w.cfg.Unlock()
	if node == nil {
		w.writeJSON(rw, map[string]interface{}{"id": id, "ms": -1})
		return
	}
	nodeCopy := *node
	ms, reachable := probeNode(nodeCopy, 3500*time.Millisecond)
	if !reachable {
		ms = -1
	}
	w.cfg.Lock()
	if n, _ := w.cfg.FindNode(id); n != nil {
		n.Ping = int(ms)
		// 单节点测速属高频可丢失写入，只标脏，由后台每5秒合并落盘一次
		w.cfg.MarkDirty()
	}
	w.cfg.Unlock()
	w.writeJSON(rw, map[string]interface{}{"id": id, "ms": ms, "groupId": gid})
}

// apiPingGroup 测速指定分组的全部节点
func (w *WebServer) apiPingGroup(rw http.ResponseWriter, r *http.Request) {
	gid := r.PathValue("id")
	w.cfg.Lock()
	var nodes []config.Node
	for _, g := range w.cfg.Groups {
		if g.ID == gid {
			nodes = make([]config.Node, len(g.Nodes))
			copy(nodes, g.Nodes)
			break
		}
	}
	w.cfg.Unlock()

	ch := make(chan map[string]interface{}, len(nodes))
	sem := make(chan struct{}, 5)
	for _, node := range nodes {
		go func(n config.Node) {
			sem <- struct{}{}
			key := n.Server + ":" + n.Port + ":" + n.Protocol
			ms, reachable := probeNode(n, 3500*time.Millisecond)
			if !reachable {
				ms = -1
			}
			<-sem
			ch <- map[string]interface{}{"id": n.ID, "ms": ms, "key": key}
		}(node)
	}

	pingMap := make(map[string]int)
	results := make([]map[string]interface{}, 0, len(nodes))
	for i := 0; i < len(nodes); i++ {
		r := <-ch
		results = append(results, map[string]interface{}{"id": r["id"], "ms": r["ms"]})
		ms := r["ms"].(int64)
		if ms != 0 {
			pingMap[r["key"].(string)] = int(ms)
		}
	}

	w.cfg.Lock()
	for gi := range w.cfg.Groups {
		if w.cfg.Groups[gi].ID == gid {
			for ni := range w.cfg.Groups[gi].Nodes {
				key := w.cfg.Groups[gi].Nodes[ni].Server + ":" + w.cfg.Groups[gi].Nodes[ni].Port + ":" + w.cfg.Groups[gi].Nodes[ni].Protocol
				if p, ok := pingMap[key]; ok {
					w.cfg.Groups[gi].Nodes[ni].Ping = p
				}
			}
			break
		}
	}
	_ = w.cfg.Save()
	w.cfg.Unlock()

	w.writeJSON(rw, results)
}

func (w *WebServer) apiSpeed(rw http.ResponseWriter, r *http.Request) {
	if !w.v2m.IsRunning() {
		w.writeJSON(rw, map[string]interface{}{"ok": false, "error": "代理未运行，请先启动", "mbps": 0})
		return
	}
	target := w.cfg.SpeedURL
	if target == "" {
		target = speedTestURL
	}
	proxyURL, _ := url.Parse(fmt.Sprintf("http://127.0.0.1:%d", w.cfg.HttpPort))
	client := &http.Client{
		Timeout: 45 * time.Second,
		Transport: &http.Transport{
			Proxy: http.ProxyURL(proxyURL),
		},
	}

	start := time.Now()
	resp, err := client.Get(target)
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

	w.cfg.Lock()
	if n, _ := w.cfg.FindNode(w.cfg.ActiveNode); n != nil {
		n.Speed = mbps
		_ = w.cfg.Save()
	}
	w.cfg.Unlock()

	w.writeJSON(rw, map[string]interface{}{
		"ok": true, "mbps": mbps, "bytes": n, "ms": elapsed.Milliseconds(),
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

func (w *WebServer) apiGetSettings(rw http.ResponseWriter, r *http.Request) {
	w.cfg.Lock()
	st := map[string]interface{}{
		"socksPort": w.cfg.SocksPort, "httpPort": w.cfg.HttpPort,
		"listenAddr": w.cfg.ListenAddr, "subRefresh": w.cfg.SubRefresh,
		"subProxy": w.cfg.SubProxy, "proxyMode": w.cfg.ProxyMode,
		"speedURL": w.cfg.SpeedURL, "autoFailover": w.cfg.FailoverEnabled(),
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

	if err := w.applySettings(req); err != nil {
		w.writeJSON(rw, map[string]string{"error": err.Error()})
		return
	}

	// 端口/监听地址/代理模式变化需要重启代理才能生效
	if w.v2m.IsRunning() {
		w.v2m.Stop()
		time.Sleep(300 * time.Millisecond)
		_ = w.v2m.Start()
	}
	w.writeJSON(rw, map[string]string{"ok": "true"})
}

// applySettings 校验并应用设置请求，全部通过后写盘。
//
// 两条硬性要求，改动本函数时必须保持：
//
//  1. 必须用 defer 释放锁。裸 w.cfg.Unlock() 在 panic 时执行不到，配置锁会永久无法释放，
//     整个服务（所有 API + 前端）会彻底挂死且无法自愈。历史上 v.(float64) 裸断言
//     在字段类型不符时 panic，一个畸形请求即可打死服务。
//
//  2. 取值一律走带 ok 判断的类型转换，禁止 v.(float64) / v.(string) 裸断言。
//     若中途 panic，配置会处于"改了一半且未落盘"的不一致状态，比直接报错更糟。
func (w *WebServer) applySettings(req map[string]interface{}) error {
	w.cfg.Lock()
	defer w.cfg.Unlock()

	if v, ok := req["socksPort"]; ok {
		n, err := toPort(v, "SOCKS5 端口")
		if err != nil {
			return err
		}
		w.cfg.SocksPort = n
	}
	if v, ok := req["httpPort"]; ok {
		n, err := toPort(v, "HTTP 代理端口")
		if err != nil {
			return err
		}
		w.cfg.HttpPort = n
	}
	if v, ok := req["listenAddr"]; ok {
		s, err := toStr(v, "Web 监听地址")
		if err != nil {
			return err
		}
		if s != "" {
			w.cfg.ListenAddr = s
		}
	}
	if v, ok := req["subRefresh"]; ok {
		n, err := toInt(v, "订阅刷新间隔")
		if err != nil {
			return err
		}
		// 0 是合法值（禁用自动刷新），不能按"空值即缺失"处理。
		// 1-9 秒视为无意义的过于频繁刷新，直接拒绝而不是静默接受。
		if n != 0 && (n < subscription.MinSubRefresh || n > subscription.MaxSafeRefresh) {
			return fmt.Errorf("订阅刷新间隔需为 0（禁用自动刷新）或 %d-%d 秒",
				subscription.MinSubRefresh, subscription.MaxSafeRefresh)
		}
		w.cfg.SubRefresh = n
	}
	if v, ok := req["subProxy"]; ok {
		s, err := toStr(v, "全局订阅代理")
		if err != nil {
			return err
		}
		w.cfg.SubProxy = s
	}
	if v, ok := req["proxyMode"]; ok {
		s, err := toStr(v, "代理模式")
		if err != nil {
			return err
		}
		switch s {
		case "smart", "global", "direct":
			w.cfg.ProxyMode = s
		default:
			return fmt.Errorf("代理模式只能是 smart / global / direct")
		}
	}
	if v, ok := req["speedURL"]; ok {
		s, err := toStr(v, "测速URL")
		if err != nil {
			return err
		}
		w.cfg.SpeedURL = s
	}
	if v, ok := req["autoFailover"]; ok {
		b, isBool := v.(bool)
		if !isBool {
			return fmt.Errorf("自动故障转移开关必须是布尔值")
		}
		w.cfg.AutoFailover = &b
	}

	return w.cfg.Save()
}

// toInt 把 JSON 解出的值安全转成 int。
// encoding/json 会把所有数字解成 float64，故只接受 float64。
func toInt(v interface{}, field string) (int, error) {
	f, ok := v.(float64)
	if !ok {
		return 0, fmt.Errorf("%s必须是数字", field)
	}
	if math.IsNaN(f) || math.IsInf(f, 0) || f != math.Trunc(f) {
		return 0, fmt.Errorf("%s必须是整数", field)
	}
	if f < math.MinInt32 || f > math.MaxInt32 {
		return 0, fmt.Errorf("%s超出允许范围", field)
	}
	return int(f), nil
}

// toStr 安全取字符串字段，类型不符时返回错误而非 panic
func toStr(v interface{}, field string) (string, error) {
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("%s必须是字符串", field)
	}
	return s, nil
}

// toPort 取端口并校验范围（1-65535）
func toPort(v interface{}, field string) (int, error) {
	n, err := toInt(v, field)
	if err != nil {
		return 0, err
	}
	if n < 1 || n > 65535 {
		return 0, fmt.Errorf("%s需在 1-65535 之间", field)
	}
	return n, nil
}

// apiBackup 导出当前配置为可下载的 JSON 文件
func (w *WebServer) apiBackup(rw http.ResponseWriter, r *http.Request) {
	w.cfg.Lock()
	b, err := json.MarshalIndent(w.cfg, "", "  ")
	w.cfg.Unlock()
	if err != nil {
		w.writeJSON(rw, map[string]string{"error": "序列化失败"})
		return
	}
	rw.Header().Set("Content-Type", "application/json")
	rw.Header().Set("Content-Disposition",
		"attachment; filename=v2aynn-backup-"+time.Now().Format("20060102-150405")+".json")
	_, _ = rw.Write(b)
}

// apiRestore 用上传的 JSON 覆盖当前配置（校验后热替换内存配置并写盘）
func (w *WebServer) apiRestore(rw http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 16<<20))
	if err != nil {
		w.writeJSON(rw, map[string]string{"error": "读取上传内容失败"})
		return
	}
	var nc config.Config
	if err := json.Unmarshal(body, &nc); err != nil {
		w.writeJSON(rw, map[string]string{"error": "不是合法的配置文件"})
		return
	}
	if len(nc.Groups) == 0 {
		w.writeJSON(rw, map[string]string{"error": "配置里没有任何分组，已拒绝导入"})
		return
	}

	w.cfg.Lock()
	w.cfg.Restore(&nc)
	err = w.cfg.Save()
	w.cfg.Unlock()
	if err != nil {
		w.writeJSON(rw, map[string]string{"error": "写入配置失败: " + err.Error()})
		return
	}

	// 恢复后原节点可能已失效，重启代理以套用新配置
	if w.v2m.IsRunning() {
		w.v2m.Stop()
		time.Sleep(300 * time.Millisecond)
		_ = w.v2m.Start()
	}
	w.writeJSON(rw, map[string]interface{}{"ok": "true", "groups": len(nc.Groups)})
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

func probeNode(n config.Node, timeout time.Duration) (int64, bool) {
	addr := net.JoinHostPort(n.Server, n.Port)
	useTLS := n.TLS == "tls" || n.TLS == "xtls" || n.TLS == "reality" ||
		n.Protocol == "trojan" || n.Security == "tls" || n.Security == "reality"

	if ms, reach := probeOnce(n, addr, useTLS, timeout); reach {
		return ms, true
	}
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
		sni := n.SNI
		if sni == "" {
			sni = n.RequestHost
		}
		if sni == "" {
			sni = n.Server
		}
		tconn := tls.Client(conn, &tls.Config{
			ServerName:         sni,
			InsecureSkipVerify: true,
			MinVersion:         tls.VersionTLS12,
		})
		if err := tconn.Handshake(); err != nil {
			return -1, false
		}
		br := bufio.NewReader(tconn)
		tconn.SetReadDeadline(time.Now().Add(250 * time.Millisecond))
		_, _ = br.ReadByte()
		elapsed := time.Since(start).Milliseconds()
		if elapsed < 1 {
			elapsed = 1
		}
		return elapsed, true
	}

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
