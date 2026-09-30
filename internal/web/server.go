package web

import (
	"bufio"
	"crypto/tls"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"math"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"v2aynn-web/internal/config"
	"v2aynn-web/internal/mihomo"
	"v2aynn-web/internal/subscription"
	"v2aynn-web/internal/v2ray"
)

//go:embed static/index.html
var indexHTML string

//go:embed static/*
var staticFS embed.FS

const speedTestURL = "https://speed.cloudflare.com/__down?bytes=2000000"

// kernel 抽象两个内核管理器的公共能力。
//
// 本程序同时支持 xray 与 mihomo 两个内核：普通节点走 xray，家宽节点走 mihomo。
// 两者监听同一组端口，所以同一时刻只有一个在跑。web 层凡是「启停/查状态」这类
// 与内核无关的操作，都通过这个接口按当前内核分派，避免到处写 if/else。
//
// 注意 SwitchNode 没有放进接口：两个内核的入参语义完全不同
// （xray 收节点 ID，mihomo 收家宽节点名），混在一起只会写错。
type kernel interface {
	IsRunning() bool
	Start() error
	Stop()
	Status() map[string]interface{}
}

type WebServer struct {
	cfg *config.Config
	v2m *v2ray.Manager
	// mhm 家宽（mihomo）内核管理器。为 nil 表示本次运行没启用家宽通道
	// （单元测试场景），所有家宽相关入口都要先判空。
	mhm *mihomo.Manager
}

func NewServer(cfg *config.Config, v2m *v2ray.Manager, mhm *mihomo.Manager) *WebServer {
	v2m.SetConfig(cfg)
	if mhm != nil {
		mhm.SetConfig(cfg)
	}
	return &WebServer{cfg: cfg, v2m: v2m, mhm: mhm}
}

// clashReady 家宽通道是否可用
func (w *WebServer) clashReady() bool { return w.mhm != nil }

// kernelName 当前应当运行的内核
func (w *WebServer) kernelName() string {
	w.cfg.Lock()
	defer w.cfg.Unlock()
	return w.cfg.CurrentKernel()
}

// activeKernel 返回当前应当运行的内核管理器。家宽通道未启用时永远返回 xray。
func (w *WebServer) activeKernel() kernel {
	if w.kernelName() == config.KernelMihomo && w.clashReady() {
		return w.mhm
	}
	return w.v2m
}

// stopAllKernels 停掉两个内核。切换内核、改端口、恢复配置时使用 ——
// 两个内核抢同一组端口，不先停干净就会启动失败。
func (w *WebServer) stopAllKernels() {
	w.v2m.Stop()
	if w.clashReady() {
		w.mhm.Stop()
	}
}

func (w *WebServer) Run(addr string) error {
	return http.ListenAndServe(addr, corsHandler(w.newMux()))
}

// newMux 构造完整路由表。单独抽出来是为了让测试能走真实路由 ——
// 端点漏注册、路径写错这类问题只有过一遍真实 mux 才测得出来。
func (w *WebServer) newMux() *http.ServeMux {
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
	mux.HandleFunc("POST /api/clash/fetch", w.apiClashFetch)
	mux.HandleFunc("GET /api/backup", w.apiBackup)
	mux.HandleFunc("POST /api/restore", w.apiRestore)
	subFS, _ := fs.Sub(staticFS, "static")
	mux.Handle("/static/", http.FileServer(http.FS(subFS)))
	mux.HandleFunc("/", w.apiIndex)

	return mux
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
	st := w.activeKernel().Status()
	// kernel 用于界面标注当前在跑哪个内核；clashEnabled 用于决定是否显示家宽分组。
	st["kernel"] = w.kernelName()
	st["clashEnabled"] = w.clashReady() && w.clashConfigured()
	w.writeJSON(rw, st)
}

// clashConfigured 是否已配置家宽订阅（未注入家宽通道时恒为 false）
func (w *WebServer) clashConfigured() bool {
	if !w.clashReady() {
		return false
	}
	w.cfg.Lock()
	defer w.cfg.Unlock()
	return w.cfg.ClashEnabled()
}

// groupSummary 分组概览（不含节点列表，仅含节点数）
type groupSummary struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	URL       string `json:"url"`
	NodeCount int    `json:"nodeCount"`
	LastFetch string `json:"lastFetch"`
	SubProxy  string `json:"subProxy"`
	// Clash 标记这是家宽虚拟分组：节点由 mihomo 内核管理，
	// 不参与订阅刷新、测速、排序、删除等普通分组才有的操作。
	Clash bool `json:"clash,omitempty"`
}

// apiGroups 返回所有分组概览（不含节点列表，仅含节点数）
func (w *WebServer) apiGroups(rw http.ResponseWriter, r *http.Request) {
	w.cfg.Lock()
	groups := make([]groupSummary, 0, len(w.cfg.Groups)+1)
	for _, g := range w.cfg.Groups {
		groups = append(groups, groupSummary{
			ID: g.ID, Name: g.Name, URL: g.URL,
			NodeCount: len(g.Nodes), LastFetch: g.LastFetch,
			SubProxy: g.SubProxy,
		})
	}
	activeGrp := w.cfg.ActiveGrp
	clashSub := w.cfg.ClashSubURL
	clashNode := w.cfg.ClashNode
	kernel := w.cfg.CurrentKernel()
	w.cfg.Unlock()

	// 家宽虚拟分组。只在配置了家宽订阅、且订阅里确实解析出节点时才出现，
	// 这样没启用家宽的用户界面上完全看不到它的痕迹。
	if w.clashReady() && clashSub != "" {
		if n := len(w.mhm.Nodes()); n > 0 {
			groups = append(groups, groupSummary{
				ID: config.ClashGroupID, Name: "家宽节点",
				NodeCount: n, LastFetch: w.mhm.SubLastFetch(),
				Clash: true,
			})
			// 家宽模式下高亮家宽分组，否则界面会把普通分组标成「当前使用中」
			if kernel == config.KernelMihomo && clashNode != "" {
				activeGrp = config.ClashGroupID
			}
		}
	}

	w.writeJSON(rw, map[string]interface{}{
		"groups":    groups,
		"activeGrp": activeGrp,
		"kernel":    kernel,
	})
}

// apiGroupNodes 返回指定分组的节点列表
func (w *WebServer) apiGroupNodes(rw http.ResponseWriter, r *http.Request) {
	gid := r.PathValue("id")
	if gid == config.ClashGroupID {
		w.writeClashNodes(rw)
		return
	}
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

// writeClashNodes 返回家宽节点列表。
//
// 家宽节点不由本程序管理，没有 Node 结构；但界面复用同一套节点列表渲染，
// 所以这里造一份「够用就好」的 Node：ID 带 clash: 前缀（服务端与前端都靠它识别），
// 名称与协议填上，其余留空。前端据此隐藏测速按钮等只对普通节点有意义的操作。
func (w *WebServer) writeClashNodes(rw http.ResponseWriter) {
	if !w.clashReady() {
		w.writeJSON(rw, map[string]interface{}{
			"nodes": []config.Node{}, "active": "", "sort": "",
			"groupId": config.ClashGroupID, "clash": true,
		})
		return
	}

	w.cfg.Lock()
	clashNode := w.cfg.ClashNode
	kernel := w.cfg.CurrentKernel()
	w.cfg.Unlock()

	names := w.mhm.Nodes()
	// 附带内核记录的历史延迟。这个调用只读内核缓存、不触发测速，
	// 所以每次加载列表都可以放心调用。
	delays := w.mhm.ProxyDelays()
	nodes := make([]config.Node, 0, len(names))
	for _, n := range names {
		// 0 = 内核从没测过（界面留空）；-1 = 测过但不通（界面显示超时）
		ping := 0
		if d, ok := delays[n]; ok {
			if d > 0 {
				ping = d
			} else {
				ping = -1
			}
		}
		nodes = append(nodes, config.Node{
			ID:       config.ClashNodeIDPrefix + n,
			Name:     n,
			Protocol: "openvpn",
			Ping:     ping,
		})
	}
	// 只有内核确实在家宽模式时才标「当前使用中」，否则会出现
	// 普通节点与家宽节点同时被标成激活的矛盾状态。
	active := ""
	if kernel == config.KernelMihomo && clashNode != "" {
		active = config.ClashNodeIDPrefix + clashNode
	}
	w.writeJSON(rw, map[string]interface{}{
		"nodes": nodes, "active": active, "sort": "",
		"groupId": config.ClashGroupID, "clash": true,
	})
}

// clashDelayTimeout 家宽节点测延迟的超时（毫秒）。
// 比普通节点宽松得多：每个节点都要现场建立一条 OpenVPN over Cloudflare 隧道。
const clashDelayTimeout = 8000

// pingClashNode 让内核测单个家宽节点的延迟。
//
// 家宽节点的出口是一条 OpenVPN 隧道，本程序那套 TCP/TLS 握手探测对它没有意义
// —— 探到的只是 CF 前置节点，和隧道能不能用是两回事。所以必须交给内核来测。
func (w *WebServer) pingClashNode(rw http.ResponseWriter, id string) {
	if !w.clashReady() {
		w.writeJSON(rw, map[string]interface{}{"id": id, "ms": -1, "error": "家宽通道未启用"})
		return
	}
	name := strings.TrimPrefix(id, config.ClashNodeIDPrefix)
	ms, err := w.mhm.ProxyDelay(name, clashDelayTimeout)
	if err != nil {
		w.writeJSON(rw, map[string]interface{}{"id": id, "ms": -1, "error": err.Error()})
		return
	}
	w.writeJSON(rw, map[string]interface{}{"id": id, "ms": ms})
}

// pingClashGroup 让内核并发测整个家宽组的延迟。
//
// 并发调度在内核里做，比本程序逐个调用高效得多。但 71 个节点每个都要现场建隧道，
// 整体耗时可能一两分钟，所以前端要给出「较慢」的等待提示。
func (w *WebServer) pingClashGroup(rw http.ResponseWriter) {
	if !w.clashReady() {
		// 返回 error 而不是空数组：空数组会被界面当成「全都不通」，
		// 掩盖掉「家宽根本没启用」这个真实原因。
		w.writeJSON(rw, map[string]string{"error": "家宽通道未启用"})
		return
	}
	delays, err := w.mhm.GroupDelay(w.mhm.NodeGroup(), clashDelayTimeout)
	if err != nil {
		w.writeJSON(rw, map[string]string{"error": err.Error()})
		return
	}
	results := make([]map[string]interface{}, 0, len(delays))
	for name, d := range delays {
		ms := d
		if ms <= 0 {
			ms = -1 // 内核把测不通记为 0，界面统一用 -1 表示超时
		}
		results = append(results, map[string]interface{}{
			"id": config.ClashNodeIDPrefix + name, "ms": ms,
		})
	}
	w.writeJSON(rw, results)
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

// apiSwitch 切换当前使用的节点。
//
// 入参 id 有两种形态，服务端按前缀分流：
//   - 普通节点 ID        -> 走 xray 内核
//   - "clash:<节点名>"   -> 走 mihomo 内核（家宽）
//
// 内核由节点类型自动决定，界面上不设额外的内核开关：点普通节点就用 xray，
// 点家宽节点就用 mihomo。两个内核抢同一组端口，切换前必须先把另一个停干净。
func (w *WebServer) apiSwitch(rw http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	if strings.HasPrefix(id, config.ClashNodeIDPrefix) {
		if !w.clashReady() {
			w.writeJSON(rw, map[string]string{"error": "家宽通道未启用"})
			return
		}
		name := strings.TrimPrefix(id, config.ClashNodeIDPrefix)

		w.cfg.Lock()
		w.cfg.Kernel = config.KernelMihomo
		w.cfg.ClashNode = name
		_ = w.cfg.Save()
		w.cfg.Unlock()

		w.v2m.Stop()
		if w.mhm.IsRunning() {
			if err := w.mhm.SwitchNode(name); err != nil {
				w.writeJSON(rw, map[string]string{"error": err.Error()})
				return
			}
		} else {
			// 首次进入家宽模式：Start() 会自动套用 cfg.ClashNode，不必再切一次
			if err := w.mhm.Start(); err != nil {
				w.writeJSON(rw, map[string]string{"error": err.Error()})
				return
			}
		}
		w.writeJSON(rw, map[string]string{"ok": "true"})
		return
	}

	// 普通节点：切回 xray 内核
	w.cfg.Lock()
	w.cfg.Kernel = config.KernelXray
	w.cfg.Unlock()
	if w.clashReady() {
		w.mhm.Stop()
	}
	if err := w.v2m.SwitchNode(id); err != nil {
		w.writeJSON(rw, map[string]string{"error": err.Error()})
		return
	}
	w.writeJSON(rw, map[string]string{"ok": "true"})
}

func (w *WebServer) apiPing(rw http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if strings.HasPrefix(id, config.ClashNodeIDPrefix) {
		w.pingClashNode(rw, id)
		return
	}
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
	if gid == config.ClashGroupID {
		w.pingClashGroup(rw)
		return
	}
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
	if !w.activeKernel().IsRunning() {
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
	if err := w.activeKernel().Start(); err != nil {
		w.writeJSON(rw, map[string]string{"error": err.Error()})
		return
	}
	w.writeJSON(rw, map[string]string{"ok": "true"})
}

func (w *WebServer) apiStop(rw http.ResponseWriter, r *http.Request) {
	// 两个内核都停。用户点的是「停止代理」，不该关心当前跑的是哪个内核；
	// 只停一个的话，另一个会继续占着端口，下次启动必然失败。
	w.stopAllKernels()
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
		"clashSubUrl": w.cfg.ClashSubURL, "kernel": w.cfg.CurrentKernel(),
		"clashNodeCount": len(w.cfg.ClashNodes),
	}
	w.cfg.Unlock()
	st["clashAvailable"] = w.clashReady()
	w.writeJSON(rw, st)
}

func (w *WebServer) apiSettings(rw http.ResponseWriter, r *http.Request) {
	var req map[string]interface{}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.writeJSON(rw, map[string]string{"error": "bad json"})
		return
	}

	w.cfg.Lock()
	oldSub := w.cfg.ClashSubURL
	w.cfg.Unlock()

	if err := w.applySettings(req); err != nil {
		w.writeJSON(rw, map[string]string{"error": err.Error()})
		return
	}

	w.cfg.Lock()
	newSub := w.cfg.ClashSubURL
	w.cfg.Unlock()
	subChanged := newSub != oldSub

	resp := map[string]interface{}{"ok": "true"}

	switch {
	case subChanged && newSub == "":
		// 清空家宽订阅 = 关闭家宽通道，把内核切回 xray。
		// 否则 mihomo 会继续跑着，而界面上已经看不到家宽分组，用户无从关闭它。
		w.cfg.Lock()
		w.cfg.Kernel = config.KernelXray
		w.cfg.ClashNodes = nil
		w.cfg.ClashNode = ""
		_ = w.cfg.Save()
		hasNode := w.cfg.ActiveNode != ""
		w.cfg.Unlock()

		w.stopAllKernels()
		if hasNode {
			time.Sleep(300 * time.Millisecond)
			_ = w.v2m.Start()
		}
		resp["clashNodeCount"] = 0

	case subChanged && newSub != "" && w.clashReady():
		// 家宽地址变了：同步拉一次。这样保存后界面上立刻能看到家宽分组，
		// 拉取失败也能当场把错误回给用户，而不是等定时刷新时才暴露。
		if err := w.fetchClashSub(); err != nil {
			resp["clashError"] = err.Error()
		}
		w.cfg.Lock()
		resp["clashNodeCount"] = len(w.cfg.ClashNodes)
		w.cfg.Unlock()

	default:
		// 端口/监听地址/代理模式变化需要重启当前内核才能生效
		if k := w.activeKernel(); k.IsRunning() {
			w.stopAllKernels()
			time.Sleep(300 * time.Millisecond)
			_ = k.Start()
		}
	}
	w.writeJSON(rw, resp)
}

// fetchClashSub 拉取家宽订阅、更新节点缓存，并在内核正处于家宽模式时重启它。
// 由「保存设置」与「手动刷新」两处调用。
func (w *WebServer) fetchClashSub() error {
	if !w.clashReady() {
		return fmt.Errorf("家宽通道未启用")
	}
	w.cfg.Lock()
	subURL, proxy := w.cfg.ClashSubURL, w.cfg.SubProxy
	w.cfg.Unlock()
	if subURL == "" {
		return fmt.Errorf("家宽订阅地址为空")
	}

	nodes, err := w.mhm.FetchSubscription(subURL, proxy)
	if err != nil {
		log.Printf("拉取家宽订阅失败: %v", err)
		return err
	}

	w.cfg.Lock()
	w.cfg.ClashNodes = nodes
	kernel := w.cfg.CurrentKernel()
	_ = w.cfg.Save()
	w.cfg.Unlock()

	// 内核正在跑家宽模式时重启一次，让它加载新配置。
	// 没跑就不用管 —— 用户下次点家宽节点时会自然用上新配置。
	if kernel == config.KernelMihomo && w.mhm.IsRunning() {
		w.mhm.Stop()
		time.Sleep(500 * time.Millisecond)
		if err := w.mhm.Start(); err != nil {
			log.Printf("家宽内核重启失败: %v", err)
			return err
		}
	}
	return nil
}

// apiClashFetch 手动刷新家宽订阅
func (w *WebServer) apiClashFetch(rw http.ResponseWriter, r *http.Request) {
	if err := w.fetchClashSub(); err != nil {
		w.writeJSON(rw, map[string]string{"error": err.Error()})
		return
	}
	w.cfg.Lock()
	n := len(w.cfg.ClashNodes)
	w.cfg.Unlock()
	w.writeJSON(rw, map[string]interface{}{"ok": "true", "count": n})
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
	if v, ok := req["clashSubUrl"]; ok {
		s, err := toStr(v, "家宽订阅地址")
		if err != nil {
			return err
		}
		// 空串是合法值（表示关闭家宽通道），不能按"空值即缺失"跳过 ——
		// 否则用户永远没法把已经配上的家宽关掉。
		w.cfg.ClashSubURL = strings.TrimSpace(s)
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

	// 恢复后原节点可能已失效，按新配置的内核重启代理。
	// 内核不能写死用 v2m —— 备份里可能带着家宽配置，恢复后应当跑 mihomo。
	if w.clashReady() && w.clashConfigured() && !w.mhm.HasConfig() {
		// 备份里有家宽订阅地址但本地还没生成过配置：先拉订阅，
		// fetchClashSub 拉完会自己按需重启内核。
		go func() { _ = w.fetchClashSub() }()
	} else {
		wasRunning := w.activeKernel().IsRunning()
		w.stopAllKernels()
		if wasRunning {
			time.Sleep(300 * time.Millisecond)
			_ = w.activeKernel().Start()
		}
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
