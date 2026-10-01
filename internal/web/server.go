package web

import (
	"bufio"
	"crypto/tls"
	"embed"
	"encoding/json"
	"errors"
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

// clashProber 家宽测速探针（跑在备用端口上的独立 mihomo 实例）。
//
// 抽成接口只为一件事：真实实现要在备用端口上拉起一个 mihomo 进程，
// 单元测试里跑不起来，得能塞替身进去。
type clashProber interface {
	mihomo.DelayTester
	Ensure(groupID string, resident bool) error
	Stop()
	Group() string
	IsRunning() bool
}

type WebServer struct {
	cfg *config.Config
	v2m *v2ray.Manager
	// mhm 家宽（mihomo）内核管理器。为 nil 表示本次运行没启用家宽通道
	// （单元测试场景），所有家宽相关入口都要先判空。
	mhm *mihomo.Manager
	// prober 家宽测速探针。家宽通道未启用时为 nil。
	prober clashProber
}

func NewServer(cfg *config.Config, v2m *v2ray.Manager, mhm *mihomo.Manager) *WebServer {
	v2m.SetConfig(cfg)
	w := &WebServer{cfg: cfg, v2m: v2m, mhm: mhm}
	if mhm != nil {
		mhm.SetConfig(cfg)
		w.prober = mihomo.NewProber(mhm.DataDir())
	}
	return w
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

// stopAllKernels 停掉两个内核与测速探针。切换内核、改端口、恢复配置时使用 ——
// 两个内核抢同一组端口，不先停干净就会启动失败；探针虽然用备用端口，
// 但留着它会白占一份内存，用户点了「停止代理」就该一并收掉。
func (w *WebServer) stopAllKernels() {
	w.v2m.Stop()
	if w.clashReady() {
		w.mhm.Stop()
	}
	if w.prober != nil {
		w.prober.Stop()
	}
}

// StopProbe 停掉测速探针。进程退出时必须调用，否则会留下一个孤儿 mihomo。
func (w *WebServer) StopProbe() {
	if w.prober != nil {
		w.prober.Stop()
	}
}

// proberResident 测速探针是否常驻。
//
// 常驻省下每次测速十几秒的启动等待，代价是一份常驻内存。两种情况不常驻：
//   - 内存紧张（盒子上只有 1 GB 且没有 swap）：探针用完就收，把内存还回去；
//   - 家宽内核正在服务流量：再养一个 mihomo 纯属浪费，测完就收。
func (w *WebServer) proberResident() bool {
	if w.kernelName() == config.KernelMihomo {
		return false
	}
	return mihomo.ProberCanReside()
}

// releaseProbe 非常驻的探针用完就收，把内存还回去。
func (w *WebServer) releaseProbe() {
	if w.prober != nil && w.prober.IsRunning() && !w.proberResident() {
		w.prober.Stop()
	}
}

// WarmProbe 后台把测速探针预热起来。
//
// 用户接下来最可能做的事就是测家宽，提前把探针拉起来能省掉那十几秒等待。
// 必须在后台跑：起一个内核要十几秒，不能拖住调用方（启动流程或切换请求）。
func (w *WebServer) WarmProbe() {
	if w.prober == nil || !w.clashReady() || !w.proberResident() {
		return
	}
	w.cfg.Lock()
	g := w.cfg.ActiveClashGroup()
	groupID := ""
	if g != nil {
		groupID = g.ID
	}
	w.cfg.Unlock()
	if groupID == "" {
		return
	}
	go func() {
		if err := w.prober.Ensure(groupID, true); err != nil {
			log.Printf("预热家宽测速探针失败: %v", err)
		}
	}()
}

// clashTester 挑一个能测家宽节点的后端。
//
// 优先用正在服务流量的那个家宽内核 —— 内核里跑的正是这个分组时，它既是
// 最真实的被测对象，又不额外花一份内存。
//
// 其余情况一律用探针：探针跑在备用端口上，不动用户当前的连接，
// 「连着普通节点时也能测家宽」这件事才成立。
func (w *WebServer) clashTester(groupID string) (mihomo.DelayTester, error) {
	if !w.clashReady() {
		return nil, errors.New("家宽通道未启用")
	}
	if w.mhm.IsRunning() && w.mhm.LoadedGroup() == groupID {
		return w.mhm, nil
	}
	if w.prober == nil {
		return nil, errors.New("测速探针不可用")
	}
	if err := w.prober.Ensure(groupID, w.proberResident()); err != nil {
		return nil, err
	}
	return w.prober, nil
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
	groups := make([]groupSummary, 0, len(w.cfg.Groups))
	clashIDs := make([]string, 0, 1)
	for _, g := range w.cfg.Groups {
		groups = append(groups, groupSummary{
			ID: g.ID, Name: g.Name, URL: g.URL,
			NodeCount: len(g.Nodes), LastFetch: g.LastFetch,
			SubProxy: g.SubProxy, Clash: g.IsClash(),
		})
		if g.IsClash() {
			clashIDs = append(clashIDs, g.ID)
		}
	}
	activeGrp := w.cfg.ActiveGrp
	clashNode := w.cfg.ClashNode
	kernel := w.cfg.CurrentKernel()
	w.cfg.Unlock()

	// 家宽分组的 Nodes 恒为空（节点由 mihomo 管），节点数与拉取时间得问内核。
	// 这一步必须放在释放配置锁之后：内核那边存在「持内核锁再拿配置锁」的路径
	// （Manager.ensureActiveNodeLocked），这里若反过来「持配置锁去拿内核锁」，
	// 两边就会各持一把互相等，形成 ABBA 死锁 —— 界面直接卡死。
	if w.clashReady() && len(clashIDs) > 0 {
		idx := make(map[string]int, len(groups))
		for i := range groups {
			idx[groups[i].ID] = i
		}
		for _, id := range clashIDs {
			i, ok := idx[id]
			if !ok {
				continue
			}
			groups[i].NodeCount = w.mhm.GroupNodeCount(id)
			groups[i].LastFetch = w.mhm.SubLastFetch(id)
		}
	}

	// 家宽模式下把高亮挪到家宽分组，否则界面会把普通分组标成「当前使用中」
	if kernel == config.KernelMihomo && clashNode != "" && !w.isClashGroup(activeGrp) {
		activeGrp = ""
	}

	w.writeJSON(rw, map[string]interface{}{
		"groups":    groups,
		"activeGrp": activeGrp,
		"kernel":    kernel,
	})
}

// isClashGroup 该分组 ID 是否对应一个家宽分组
func (w *WebServer) isClashGroup(id string) bool {
	if !w.clashReady() || id == "" {
		return false
	}
	w.cfg.Lock()
	defer w.cfg.Unlock()
	return w.cfg.FindClashGroup(id) != nil
}

// apiGroupNodes 返回指定分组的节点列表
func (w *WebServer) apiGroupNodes(rw http.ResponseWriter, r *http.Request) {
	gid := r.PathValue("id")
	if w.isClashGroup(gid) {
		w.writeClashNodes(rw, gid)
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

// writeClashNodes 返回某个家宽分组的节点列表。
//
// 家宽节点不由本程序管理，没有 Node 结构；但界面复用同一套节点列表渲染，
// 所以这里造一份「够用就好」的 Node：ID 形如 clash:<分组ID>:<节点名>
// （服务端与前端都靠前缀识别），名称与协议填上，其余留空。
//
// 未生效的分组也要能列出节点 —— 列表里没有节点，用户就没法点任何一个来
// 启用它，这个分组就永远启用不了。节点名由内核管理器现场解析订阅原文得到。
//
// 排序也在这里做：前端是纯服务端排序（下拉框只发 /api/sort 再重拉列表），
// 所以家宽分组必须自己按 SortOrder 排一遍，否则「延迟↑/↓」点了没有任何反应。
func (w *WebServer) writeClashNodes(rw http.ResponseWriter, groupID string) {
	if !w.clashReady() {
		w.cfg.Lock()
		sortOrder := w.cfg.SortOrder
		w.cfg.Unlock()
		w.writeJSON(rw, map[string]interface{}{
			"nodes": []config.Node{}, "active": "", "sort": sortOrder,
			"groupId": groupID, "clash": true,
		})
		return
	}

	w.cfg.Lock()
	clashNode := w.cfg.ClashNode
	activeGrp := w.cfg.ActiveGrp
	kernel := w.cfg.CurrentKernel()
	sortOrder := w.cfg.SortOrder
	// 上次测速存下来的结果。家宽节点没有 Node 结构，延迟与速度都存在分组的
	// Probes 表里 —— 存下来才能在重启之后、以及没被内核加载的分组上显示出来。
	stored := map[string]config.ProbeInfo{}
	if g := w.cfg.FindClashGroup(groupID); g != nil {
		for k, v := range g.Probes {
			stored[k] = v
		}
	}
	w.cfg.Unlock()

	names := w.mhm.GroupNodes(groupID)

	// 内核的实时记录比存下来的更可信，但它只有「当前生效」那个分组才有 ——
	// 内核一次只加载一份配置。别的分组一律用存下来的那份。
	// 这个调用只读内核缓存、不触发测速，所以每次加载列表都可以放心调用。
	var live map[string]int
	if w.mhm.LoadedGroup() == groupID {
		live = w.mhm.ProxyDelays()
	}

	nodes := make([]config.Node, 0, len(names))
	for _, n := range names {
		// 0 = 从没测过（界面留空）；-1 = 测过但不通（界面显示超时）
		ping, speed := 0, 0.0
		if p, ok := stored[n]; ok {
			ping, speed = p.MS, p.MBPS
		}
		if d, ok := live[n]; ok {
			if d > 0 {
				ping = d
			} else {
				ping = -1
			}
		}
		nodes = append(nodes, config.Node{
			ID:       config.ClashNodeID(groupID, n),
			Name:     n,
			Protocol: "openvpn",
			Ping:     ping,
			Speed:    speed,
		})
	}
	// 只有内核确实在家宽模式、且选中的节点属于这个分组时才标「当前使用中」，
	// 否则会出现普通节点与家宽节点同时被标成激活的矛盾状态。
	active := ""
	if kernel == config.KernelMihomo && clashNode != "" && activeGrp == groupID {
		active = config.ClashNodeID(groupID, clashNode)
	}

	// 与普通节点走同一个比较器：Ping > 0 才算「测到了」，0（没测过）与
	// -1（测过但不通）一律沉底。家宽节点的 ping 语义与此完全一致。
	switch sortOrder {
	case "ping_asc":
		sortNodes(nodes, true)
	case "ping_desc":
		sortNodes(nodes, false)
	}

	w.writeJSON(rw, map[string]interface{}{
		"nodes": nodes, "active": active, "sort": sortOrder,
		"groupId": groupID, "clash": true,
	})
}

// clashDelayTimeout 家宽节点测延迟的超时（毫秒）。
// 比普通节点宽松得多：每个节点都要现场建立一条 OpenVPN over Cloudflare 隧道。
const clashDelayTimeout = 8000

// clashFrontTimeout 测前置通道的超时（毫秒）。
// 前置通道只有一跳、比节点本身快，给同样的上限足够。
const clashFrontTimeout = 8000

// pingClashNode 测单个家宽节点的延迟。
//
// 家宽节点的出口是一条 OpenVPN 隧道，本程序那套 TCP/TLS 握手探测对它没有意义
// —— 探到的只是 CF 前置节点，和隧道能不能用是两回事。所以必须交给内核来测。
//
// 注意这里**不再要求「家宽内核正在跑」**：测速走探针实例，跑在备用端口上，
// 所以用户连着普通节点时也能测家宽，且不会断掉当前连接。
func (w *WebServer) pingClashNode(rw http.ResponseWriter, id string) {
	groupID, name, ok := config.ParseClashNodeID(id)
	if !ok {
		w.writeJSON(rw, map[string]interface{}{"id": id, "ms": -1, "error": "家宽节点 ID 格式不对"})
		return
	}
	tester, err := w.clashTester(groupID)
	if err != nil {
		w.writeJSON(rw, map[string]interface{}{"id": id, "ms": -1, "error": err.Error()})
		return
	}
	ms := mihomo.MeasureDelay(tester, name, clashDelayTimeout)
	w.saveProbeMS(groupID, name, ms)
	w.releaseProbe()
	w.writeJSON(rw, map[string]interface{}{"id": id, "ms": ms})
}

// pingClashGroup 测整个家宽分组的延迟。
//
// 分组里可能有 70 多个节点、每个都要现场建隧道，整体耗时可能一两分钟，
// 所以前端要给出「较慢」的等待提示。
func (w *WebServer) pingClashGroup(rw http.ResponseWriter, groupID string) {
	delays, err := w.probeClashGroup(groupID)
	if err != nil {
		// 返回 error 而不是空数组：空数组会被界面当成「全都不通」，
		// 掩盖掉「前置通道不通」这类真实原因。
		w.writeJSON(rw, map[string]string{"error": err.Error()})
		return
	}
	w.saveProbeMSAll(groupID, delays)
	results := make([]map[string]interface{}, 0, len(delays))
	for name, ms := range delays {
		results = append(results, map[string]interface{}{
			"id": config.ClashNodeID(groupID, name), "ms": ms,
		})
	}
	w.releaseProbe()
	w.writeJSON(rw, results)
}

// probeClashGroup 测一个家宽分组里所有节点的延迟，返回 节点名→毫秒（-1 表示不通）。
//
// 早先的写法是「先探一次前置通道，不通就直接返回」，现在改成「先照实测完，
// 全都测不通时才回头追究原因」。原因是原来那版会误挡：
//   - 拿组名去问 /proxies/{组}/delay，内核走的是组的 fast()，而 fast() 在组里
//     还没有延迟历史时**无条件取成员列表的第一个节点、且不检查它死活**。于是
//     「探前置通道」实际变成了「探订阅里第一个 CF 节点」—— 它恰好挂着就误报
//     「前置不通」，另外两百多个明明好好的。
//   - 订阅一刷新节点顺序就变，同一个分组一会儿能测一会儿不能测，
//     表现出来就是「网关检测不稳定」。
func (w *WebServer) probeClashGroup(groupID string) (map[string]int, error) {
	names := w.mhm.GroupNodes(groupID)
	if len(names) == 0 {
		return nil, fmt.Errorf("这个分组还没有节点，请先点「更新」拉取订阅")
	}
	tester, err := w.clashTester(groupID)
	if err != nil {
		return nil, err
	}

	delays := mihomo.SweepDelay(tester, names, clashDelayTimeout, mihomo.ClashSweepConcurrency)

	// 有节点能测通就正常返回 —— 家宽节点掉线是常态，掉几个不算故障，
	// 界面按节点逐个显示即可。
	if !allUnreachable(delays) {
		return delays, nil
	}
	// 一个都测不通才值得追究：家宽节点的出口全挤在同一条前置通道上，
	// 前置不通时 70 多个节点会全超时，白等两分钟还看不出所以然。
	return nil, w.diagnoseFront(groupID, tester)
}

// allUnreachable 结果里是不是一个测通的都没有。
func allUnreachable(delays map[string]int) bool {
	if len(delays) == 0 {
		return true
	}
	for _, ms := range delays {
		if ms > 0 {
			return false
		}
	}
	return true
}

// diagnoseFront 家宽节点全都不通时，回头查一下前置通道，给一句能照着做的说明。
func (w *WebServer) diagnoseFront(groupID string, tester mihomo.DelayTester) error {
	front := w.mhm.FrontGroup(groupID)
	if front == "" {
		return fmt.Errorf("这批家宽节点全都连不上，点「更新」换一批再试")
	}
	// 先问内核「前置组此刻在用哪个节点」，再测那一个 —— 直接拿组名去测会被
	// fast() 落到「成员列表第一个」，那代表不了前置通道当前的真实状态。
	target := front
	if nt, ok := tester.(mihomo.GroupNowTester); ok {
		if now, err := nt.ProxyNow(front); err == nil && now != "" {
			target = now
		}
	}
	ms, err := tester.ProxyDelay(target, clashFrontTimeout)
	if err != nil {
		return fmt.Errorf("家宽节点全部不通，前置通道[%s]当前的节点[%s]也测不通 —— 先把它修好再测（家宽节点的出口都走它）", front, target)
	}
	return fmt.Errorf("前置通道[%s]正常（当前节点[%s]，%dms），但这批家宽节点全都连不上 —— 家宽节点掉线是常态，点「更新」换一批再试", front, target, ms)
}

// saveProbeMS 记下一个家宽节点的延迟
func (w *WebServer) saveProbeMS(groupID, name string, ms int) {
	w.cfg.Lock()
	defer w.cfg.Unlock()
	if g := w.cfg.FindClashGroup(groupID); g != nil {
		g.SetProbeMS(name, ms)
		_ = w.cfg.Save()
	}
}

// saveProbeMSAll 记下一批家宽节点的延迟。一次落盘 —— 70 多个节点逐个存太浪费。
func (w *WebServer) saveProbeMSAll(groupID string, res map[string]int) {
	w.cfg.Lock()
	defer w.cfg.Unlock()
	g := w.cfg.FindClashGroup(groupID)
	if g == nil {
		return
	}
	for name, ms := range res {
		g.SetProbeMS(name, ms)
	}
	_ = w.cfg.Save()
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
	// 异步拉取。新建的分组是普通订阅还是家宽订阅，只有拉回来才知道 ——
	// 拉取过程会自己识别，并把分组标记成对应的类型。
	go w.fetchNewGroup(id)
	w.writeJSON(rw, map[string]string{"id": id, "ok": "true"})
}

// fetchNewGroup 拉取一个刚新建分组的订阅。
//
// 分两步走：先让普通订阅解析器试一次 —— 它会认出 cfnew 那种家宽订阅，并把
// 分组标记成家宽类型；确认是家宽之后，再交给 mihomo 内核拉一遍，把订阅原文
// 落到 dataDir 下（内核只认自己目录里的文件），顺带统计出节点数，
// 否则侧栏会一直显示 0 个节点。
//
// 代价是家宽订阅会被下载两次。这点开销只在「新增分组」时发生一次，换来的是
// 「添加订阅分组」这一个入口同时支持两种订阅，用户不必先分辨自己手上的是哪种。
func (w *WebServer) fetchNewGroup(id string) {
	subscription.FetchGroup(w.cfg, id)

	if !w.isClashGroup(id) || !w.clashReady() {
		return
	}
	w.cfg.Lock()
	g := w.cfg.FindClashGroup(id)
	subURL, proxy := "", w.cfg.SubProxy
	if g != nil {
		subURL = g.URL
		if g.SubProxy != "" {
			proxy = g.SubProxy
		}
	}
	w.cfg.Unlock()
	if subURL == "" {
		return
	}
	if _, err := w.mhm.FetchSubscription(id, subURL, proxy); err != nil {
		log.Printf("新增家宽分组[%s]拉取失败: %v", id, err)
		return
	}
	log.Printf("新增家宽分组[%s]已交给 mihomo 内核", id)
}

// apiDelGroup 删除订阅分组
func (w *WebServer) apiDelGroup(rw http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == config.DefaultGroupID {
		w.writeJSON(rw, map[string]string{"error": "不能删除默认分组"})
		return
	}
	// 先判定类型（内部要读配置锁），再动配置
	isClash := w.isClashGroup(id)

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
	// 删掉正在生效的家宽分组时，必须把内核切回 xray：否则 mihomo 会继续跑着
	// 一份已经不存在的分组的配置，而界面上已经看不到这个分组了，用户无从关闭它。
	fallbackToXray := isClash && w.cfg.Kernel == config.KernelMihomo
	if fallbackToXray {
		w.cfg.Kernel = config.KernelXray
		w.cfg.ClashNode = ""
	}
	hasNormalNode := w.cfg.ActiveNode != ""
	_ = w.cfg.Save()
	w.cfg.Unlock()

	if isClash && w.clashReady() {
		// 清掉订阅原文与运行配置；若删的正是当前生效那份，内核会被一起停掉
		w.mhm.UnloadGroup(id)
		// 探针里装的可能正是这个分组，而它的订阅原文已经删了，留着没意义
		if w.prober != nil && w.prober.Group() == id {
			w.prober.Stop()
		}
		if fallbackToXray && hasNormalNode {
			time.Sleep(300 * time.Millisecond)
			_ = w.v2m.Start()
			w.WarmProbe()
		}
	}
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
	isClash := w.isClashGroup(id)
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

	if isClash {
		// 家宽分组：订阅原文要落到内核目录里，走 mihomo 那条路重拉
		if !w.clashReady() {
			w.writeJSON(rw, map[string]string{"error": "家宽通道未启用"})
			return
		}
		subURL, proxy := w.clashGroupFetchArgs(id)
		if err := w.mhm.Reload(id, subURL, proxy); err != nil {
			w.writeJSON(rw, map[string]string{"error": err.Error()})
			return
		}
		w.writeJSON(rw, map[string]string{"ok": "true"})
		return
	}

	// 更新后异步拉取新订阅
	go subscription.FetchGroup(w.cfg, id)
	w.writeJSON(rw, map[string]string{"ok": "true"})
}

// clashGroupFetchArgs 取出某个家宽分组拉订阅要用的地址与代理。
// 分组自己的订阅代理为空时回落到全局订阅代理（与普通订阅分组规则一致）。
func (w *WebServer) clashGroupFetchArgs(id string) (subURL, proxy string) {
	w.cfg.Lock()
	defer w.cfg.Unlock()
	proxy = w.cfg.SubProxy
	if g := w.cfg.FindClashGroup(id); g != nil {
		subURL = g.URL
		if g.SubProxy != "" {
			proxy = g.SubProxy
		}
	}
	return
}

// apiFetchGroup 拉取指定分组
func (w *WebServer) apiFetchGroup(rw http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if w.isClashGroup(id) {
		if !w.clashReady() {
			w.writeJSON(rw, map[string]string{"error": "家宽通道未启用"})
			return
		}
		subURL, proxy := w.clashGroupFetchArgs(id)
		// 同步拉取：家宽订阅的原文与运行配置都要落到内核目录，前端也得拿到
		// 结果才能刷新节点数。若内核正跑着这个分组，这里会顺带重启内核，
		// 耗时十几秒 —— 界面上「更新」按钮已有等待文案。
		if err := w.mhm.Reload(id, subURL, proxy); err != nil {
			w.writeJSON(rw, map[string]string{"error": err.Error()})
			return
		}
		w.writeJSON(rw, map[string]interface{}{"ok": "true", "count": w.mhm.GroupNodeCount(id)})
		return
	}
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
//   - 普通节点 ID                -> 走 xray 内核
//   - "clash:<分组ID>:<节点名>"   -> 走 mihomo 内核（家宽）
//
// 内核由节点类型自动决定，界面上不设额外的内核开关：点普通节点就用 xray，
// 点家宽节点就用 mihomo。两个内核抢同一组端口，切换前必须先把另一个停干净。
//
// 家宽这边比 xray 多一层：mihomo 同一时刻只加载一份配置，所以跨分组点节点时
// 必须先换配置再重启内核；同一个分组内换节点只需拨一下策略组，快得多。
func (w *WebServer) apiSwitch(rw http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	if groupID, name, ok := config.ParseClashNodeID(id); ok {
		w.switchClashNode(rw, groupID, name)
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
	// 接下来用户最可能做的事就是测家宽，趁现在把探针预热起来。
	// 后台跑，不拖住这次切换请求。
	w.WarmProbe()
	w.writeJSON(rw, map[string]string{"ok": "true"})
}

// switchClashNode 启用某个家宽分组里的某个节点。
func (w *WebServer) switchClashNode(rw http.ResponseWriter, groupID, name string) {
	if !w.clashReady() {
		w.writeJSON(rw, map[string]string{"error": "家宽通道未启用"})
		return
	}

	w.cfg.Lock()
	exists := w.cfg.FindClashGroup(groupID) != nil
	if exists {
		w.cfg.Kernel = config.KernelMihomo
		w.cfg.ActiveGrp = groupID
		w.cfg.ClashNode = name
	}
	_ = w.cfg.Save()
	w.cfg.Unlock()
	if !exists {
		w.writeJSON(rw, map[string]string{"error": "这个家宽分组已不存在，请刷新页面后重试"})
		return
	}

	// 两个内核抢同一组端口，先把 xray 停干净
	w.v2m.Stop()
	// 家宽内核马上要起来服务流量了，测速探针就得让位 ——
	// 再养一个 mihomo 纯属白占内存，而且它那份配置也不是当前在用的。
	if w.prober != nil {
		w.prober.Stop()
	}

	if w.mhm.LoadedGroup() == groupID {
		// 这个分组的配置已经就位。内核在跑就只拨一下策略组，不重启 ——
		// 重启一次要十几秒，而组内换节点本来是一瞬间的事。
		if w.mhm.IsRunning() {
			if err := w.mhm.SwitchNode(name); err != nil {
				w.writeJSON(rw, map[string]string{"error": err.Error()})
				return
			}
			w.writeJSON(rw, map[string]string{"ok": "true"})
			return
		}
		// 配置在磁盘上但内核没跑：Start 会自动套用刚写进配置的节点名
		if err := w.mhm.Start(); err != nil {
			w.writeJSON(rw, map[string]string{"error": err.Error()})
			return
		}
		w.writeJSON(rw, map[string]string{"ok": "true"})
		return
	}

	// 跨分组：mihomo 一次只加载一份配置，必须换配置再重启
	w.mhm.Stop()
	subURL, proxy := w.clashGroupFetchArgs(groupID)
	if err := w.mhm.LoadGroup(groupID, subURL, proxy); err != nil {
		w.writeJSON(rw, map[string]string{"error": err.Error()})
		return
	}
	if err := w.mhm.Start(); err != nil {
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
	if w.isClashGroup(gid) {
		w.pingClashGroup(rw, gid)
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

	w.saveSpeed(mbps)

	w.writeJSON(rw, map[string]interface{}{
		"ok": true, "mbps": mbps, "bytes": n, "ms": elapsed.Milliseconds(),
	})
}

// saveSpeed 把真实测速的结果记到「当前真正在用的那个节点」上。
//
// 家宽模式下绝不能写 cfg.ActiveNode —— 那个字段在家宽模式下还停在上一个普通
// 节点上（切家宽节点时不会改它），写进去等于把家宽测出来的速度记到普通节点
// 头上，两个节点的数字同时变得不可信。这正是用户报的「测速不准」。
func (w *WebServer) saveSpeed(mbps float64) {
	w.cfg.Lock()
	defer w.cfg.Unlock()
	if w.cfg.CurrentKernel() == config.KernelMihomo {
		if g := w.cfg.FindClashGroup(w.cfg.ActiveGrp); g != nil {
			g.SetProbeSpeed(w.cfg.ClashNode, mbps)
			_ = w.cfg.Save()
		}
		return
	}
	if n, _ := w.cfg.FindNode(w.cfg.ActiveNode); n != nil {
		n.Speed = mbps
		_ = w.cfg.Save()
	}
}

func (w *WebServer) apiStart(rw http.ResponseWriter, r *http.Request) {
	if err := w.activeKernel().Start(); err != nil {
		w.writeJSON(rw, map[string]string{"error": err.Error()})
		return
	}
	w.WarmProbe()
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
		"kernel": w.cfg.CurrentKernel(),
		// 家宽订阅不再从这里配，但界面要能告诉用户「你已经有几个家宽分组」，
		// 免得他在设置里找不到家宽入口时以为是程序坏了。
		"clashGroupCount": len(w.cfg.ClashGroups()),
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

	if err := w.applySettings(req); err != nil {
		w.writeJSON(rw, map[string]string{"error": err.Error()})
		return
	}

	// 端口 / 监听地址 / 代理模式变化需要重启当前内核才能生效。
	// 家宽订阅不在这里配置 —— 它是普通订阅分组的一种，改动走分组接口。
	if k := w.activeKernel(); k.IsRunning() {
		w.stopAllKernels()
		time.Sleep(300 * time.Millisecond)
		_ = k.Start()
	}
	// stopAllKernels 把测速探针也收了，这里补回来
	w.WarmProbe()
	w.writeJSON(rw, map[string]interface{}{"ok": "true"})
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
	// 家宽订阅地址不在这里设置 —— 家宽订阅就是普通订阅分组的一种
	// （Group.Kind = clash），走 /api/group/add 与 /api/group/{id}/update。
	// 若请求里带了 clashSubUrl，直接忽略：老版本前端可能还在发这个字段，
	// 忽略比报错更稳妥（用户点保存不会因为一个过期字段而失败）。

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
	// 内核不能写死用 v2m —— 备份里可能带着家宽分组，恢复后应当跑 mihomo。
	// 备份只带分组（订阅地址），不带家宽的订阅原文与运行配置（那是内核自己的文件），
	// 所以首次启动时由 mihomo 自己按需联网拉一次（见 Manager.ensureActiveConfig）。
	wasRunning := w.activeKernel().IsRunning()
	w.stopAllKernels()
	if wasRunning {
		time.Sleep(300 * time.Millisecond)
		_ = w.activeKernel().Start()
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
