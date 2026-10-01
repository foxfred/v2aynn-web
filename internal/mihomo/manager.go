// Package mihomo 管理 mihomo（Clash.Meta）内核进程，用于跑 cfnew 的「家宽」订阅。
//
// 为什么要单独开一个内核：家宽订阅里的节点类型是 openvpn，并且用 dialer-proxy
// 把 OpenVPN 流量塞进 Cloudflare 通道做链式转发。这两样能力 xray 内核都不具备
// （既没有 openvpn 出站，也没有 dialer-proxy 的等价物），所以家宽只能交给 mihomo。
// 两个内核监听同一组端口，因此同一时刻只运行其中一个。
//
// 与 internal/v2ray 刻意保持一致的几处设计：
//   - stopRequested 标记区分「进程崩了该自愈」与「用户点了停止不该自愈」；
//   - watch 最多自愈 3 次，超过则放弃并写日志；
//   - 配置改写走行级替换而不是 YAML 解析重建，保住本项目「零外部依赖」的特性。
package mihomo

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"v2aynn-web/internal/config"
)

// 内核与数据的默认位置。mihomo 二进制与 geo 数据都不随源码分发，
// 需要在部署时单独放到目标机上（见 README「家宽模式」一节）。
const (
	// DefaultBinPath mihomo 内核默认路径，与 xray 放在同一目录便于统一部署。
	DefaultBinPath = "/usr/local/bin/mihomo"

	// ControlPort mihomo 的 external-controller 监听端口。
	// 只绑回环，供本程序查询状态与切换节点用，不对外开放。
	ControlPort = 19090

	// RedirPort 透明代理（iptables REDIRECT）入口端口。
	// 必须与 transparent.sh 里写死的 12345 一致，也与 xray 的
	// dokodemo-door 入站端口保持一致 —— 换内核时 iptables 规则不用动。
	RedirPort = 12345

	// GeoIPMetaDB mihomo 专用的 GeoIP 数据库文件名。
	//
	// 注意：它与项目根目录里给 xray 用的 geoip.dat 不是同一种格式，不能混用。
	// 缺这个文件时 mihomo 会尝试联网从 GitHub 下载，墙内会卡满 90 秒超时，
	// 期间代理端口完全不监听 —— 现象上就像「进程起来了但代理没工作」，
	// 是最难排查的一种失败。所以启动前必须硬检查。
	GeoIPMetaDB = "geoip.metadb"

	// GeoFile mihomo 的 geosite 数据文件名（与 xray 的 geosite.dat 同名但格式不同）。
	GeoFile = "geosite.dat"

	// confFileName 改写后交给 mihomo 实际运行的配置。
	//
	// 只有一份：mihomo 一次只能加载一份配置（-f 只接一个文件），
	// 所以家宽分组可以有多个，但同一时刻只有「当前生效」的那份会写到这里。
	confFileName = "clash-config.yaml"

	// legacySubFileName 早期只有「一个家宽订阅」时用的订阅原文文件名。
	// 保留它是为了升级后不必重新联网拉一次订阅 —— 盒子开机时代理还没起来，
	// 拉订阅大概率失败，那就成死锁了。
	legacySubFileName = "clash-sub.yaml"

	// --- 普通节点健康看护（自动故障转移）的节奏 ---
	//
	// 普通节点配置里只有一个 select 组，而 select 组不会自己换节点 ——
	// 当前节点挂了，mihomo 会老老实实继续用它，用户的网就是不通。
	// xray 时代靠「节点不通 → 进程起不来 → 崩溃 4 次 → tryFailover」兜住，
	// mihomo 加载配置时不校验节点连通性，这条链根本不存在，所以要自己看护。
	//
	// 节奏参考 xray 那套：它要等进程崩溃 4 次，实际也是几十秒到一分多钟才动作。
	// 30 秒探一次、连续 3 次失败才换，等于给 90 秒的容忍窗口 ——
	// 既不会因为一次网络抖动就换节点，也不会让用户干等太久。

	// healthProbeInterval 两次探测之间的间隔。
	healthProbeInterval = 30 * time.Second
	// healthFailThreshold 连续失败几次才判定当前节点不可用。
	healthFailThreshold = 3
	// healthProbeTimeoutMs 单次探测的超时（毫秒），由内核侧计时。
	healthProbeTimeoutMs = 6000
	// healthMaxTry 一轮故障里最多尝试换几个节点，防止在坏节点之间反复横跳。
	healthMaxTry = 3
	// healthMaxTotalTry 一轮故障里累计最多试几个节点，超了就放弃等待人工介入。
	//
	// 没有这个上限的话，万一整份订阅都挂了（810 个节点全不通），程序会一个接一个
	// 换下去，90 秒换一个、换满 20 个小时 —— 用户看到的就是「节点名自己在乱跳」。
	// 试满 8 个还不通，基本可以断定不是节点的问题（订阅过期 / 本地断网），
	// 停下来比继续折腾更有用。探测一旦成功就会自动复位，不会卡死。
	healthMaxTotalTry = 8

	// subFilePrefix / subFileSuffix 每个家宽分组各存一份订阅原文，便于排查与离线重载。
	subFilePrefix = "clash-sub-"
	subFileSuffix = ".yaml"
)

// subFileNameFor 某个家宽分组的订阅原文文件名。
// 分组 ID 是纯数字（UnixNano），直接拼进文件名是安全的。
func subFileNameFor(groupID string) string {
	return subFilePrefix + groupID + subFileSuffix
}

// Manager mihomo 内核管理器。
// 并发约定与 v2ray.Manager 一致：m.mu 保护本结构体字段，
// cfg 自身的字段由 cfg 内嵌的 Mutex 保护，两者不可交叉持有。
type Manager struct {
	mu      sync.Mutex
	cfg     *config.Config
	dataDir string
	bin     string
	cmd     *exec.Cmd
	running bool

	// stopRequested 由 Stop() 置位、Start() 清除，见包注释。
	stopRequested bool

	// 以下几项由订阅解析得到，属于「配置派生数据」，不写进 config.json ——
	// 进程重启后可以从 dataDir 下的订阅原文重新解析出来。
	//
	// 注意它们是**当前生效的那份配置**的派生结果，不是所有家宽分组的。
	// 家宽分组可以有好几个，但 mihomo 一次只加载一份（见 loadedGroup）。
	loadedGroup string   // 当前已加载的家宽分组 ID
	nodes       []string // 家宽（openvpn）节点名，按订阅里的顺序
	nodeGroup   string   // 家宽节点所属的 select 组名，手动切换节点时用
	topGroup    string   // 顶层主 select 组名，成员包含 nodeGroup
	activeName  string   // 当前选中的家宽节点名

	// normalMode 当前加载的是「普通节点配置」（由 BuildNormalConfig 生成），
	// 而不是某个家宽分组的订阅原文。
	//
	// 两种配置共用同一个 confFileName：mihomo 一次只加载一份，落到同一个文件
	// 最省事，Start()/watch()/自愈那套也就完全不用分叉。
	normalMode bool
	// normalSig 生成当前普通节点配置时那批节点的签名，用来判断「要不要重写配置」。
	// 切节点是高频操作，节点集合没变时只拨一下策略组即可，不必重启内核。
	normalSig string
	// normalByID / normalByName 普通节点在「界面 ID」与「配置里的节点名」之间的映射。
	//
	// 必须有这两张表：Clash 要求节点名唯一，重名节点在生成配置时会被加上 " #N"
	// 后缀，所以界面上点的是节点 ID，而内核里认的是那个被改过的名字。
	normalByID   map[string]normalNodeRef
	normalByName map[string]string

	// nodeCounts 各分组解析出的节点数。侧栏每个家宽分组都要显示节点数，
	// 但只有「当前生效」那个的节点真在内存里，所以这里单独记一份。
	nodeCounts map[string]int

	restartCount int
	lastRestart  time.Time
	restartMu    sync.Mutex

	// --- 普通节点健康看护（自动故障转移）---
	//
	// healthMu 单独一把锁，不塞进 m.mu：探测本身要发 HTTP 请求，
	// 持着内核锁做网络 IO 会把 Status()/Nodes() 这些读接口一起卡住。
	healthMu   sync.Mutex
	healthOn   bool            // 看护循环是否已启动（只启一次）
	failCount  int             // 当前节点连续探测失败次数
	failTried  map[string]bool // 本轮故障里已经试过的节点 ID，避免来回换同一个
	failGaveUp bool            // 本轮已放弃，避免每 30 秒重复刷同一句日志

	// ctrlAddr 覆盖 external-controller 的地址，仅测试用。
	// 留空时用 127.0.0.1:ControlPort（生产路径永远走这个）。
	ctrlAddr string
}

// NewManager 创建 mihomo 管理器。dataDir 必须与 xray 用的是同一个目录，
// 因为 geo 数据要放在该目录下供 mihomo 的 -d 参数读取。
func NewManager(dataDir string) *Manager {
	return &Manager{dataDir: dataDir, bin: DefaultBinPath}
}

// SetMihomoBin 覆盖 mihomo 二进制路径（测试或非标准部署环境使用）
func (m *Manager) SetMihomoBin(p string) { m.bin = p }

func (m *Manager) SetConfig(cfg *config.Config) {
	m.cfg = cfg
	// 配置就位后立刻从磁盘恢复一次家宽解析结果。放在这里而不是 NewManager：
	// NewManager 时还不知道哪个分组是当前生效的。
	m.restoreFromDisk()
}

// DataDir 返回数据目录（web 层展示用）
func (m *Manager) DataDir() string { return m.dataDir }

// HasConfig 是否已经生成过家宽配置。
// 用于区分「可以直接启动内核」与「必须先拉一次订阅」两种情况 ——
// 进程刚重启时内存里没有解析结果，但磁盘上可能已有可用配置。
func (m *Manager) HasConfig() bool {
	_, err := os.Stat(filepath.Join(m.dataDir, confFileName))
	return err == nil
}

// SubLastFetch 返回某个家宽分组订阅的最后更新时间（界面展示用），
// 从未拉取过时返回空串。
func (m *Manager) SubLastFetch(groupID string) string {
	fi, err := os.Stat(filepath.Join(m.dataDir, subFileNameFor(groupID)))
	if err != nil {
		// 升级前的老配置只有一份全局订阅原文，也算这个分组拉过
		if fi, err = os.Stat(filepath.Join(m.dataDir, legacySubFileName)); err != nil {
			return ""
		}
	}
	return fi.ModTime().Format("2006-01-02 15:04:05")
}

// LoadedGroup 返回当前已加载的家宽分组 ID（未加载任何分组时为空）
func (m *Manager) LoadedGroup() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.loadedGroup
}

func (m *Manager) IsRunning() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.running
}

// ports 读取当前配置里的入站端口。cfg 可能尚未注入（单元测试场景），
// 此时回落到与 config.Load 一致的默认值。
func (m *Manager) ports() (socksPort, httpPort int) {
	if m.cfg == nil {
		return 10808, 10810
	}
	m.cfg.Lock()
	defer m.cfg.Unlock()
	socksPort, httpPort = m.cfg.SocksPort, m.cfg.HttpPort
	if socksPort <= 0 {
		socksPort = 10808
	}
	if httpPort <= 0 {
		httpPort = 10810
	}
	return
}

// ErrKernelMissing 内核或 geo 数据没部署好。
//
// 这是一类**确定性失败**：文件不在，重试多少次都还是不在。调用方（开机自动启动）
// 靠它提前收手，而不是白等 5 轮重试 —— 那期间代理一直是停的，用户只会觉得程序卡住。
var ErrKernelMissing = errors.New("mihomo 内核未就绪")

// CheckEnv 启动前检查内核与 geo 数据是否就位。
//
// 缺 geo 数据时 mihomo 会尝试联网从 GitHub 下载，墙内会卡满 90 秒超时，
// 期间代理端口完全不监听 —— 现象上就像「进程在跑但代理没工作」，是最难排查
// 的一种失败。所以宁可启动前硬检查，给一句明确的提示。
//
// geosite.dat 只在订阅规则真的引用了 GEOSITE 时才必需：没引用的话 mihomo
// 根本不会去加载它，缺了也不影响启动。cfnew 的默认订阅只用 GEOIP，所以
// 多数情况下只需要 geoip.metadb 一个文件。普通节点那份配置用到了 GEOSITE，
// 由 usesGeosite 自动识别。
//
// 所有错误都包了 ErrKernelMissing，方便调用方区分「环境没准备好」与「偶发失败」。
func (m *Manager) CheckEnv() error {
	if _, err := os.Stat(m.bin); err != nil {
		return fmt.Errorf("%w: 找不到 mihomo 内核(%s)，请先把内核文件部署到该路径",
			ErrKernelMissing, m.bin)
	}
	if _, err := os.Stat(filepath.Join(m.dataDir, GeoIPMetaDB)); err != nil {
		return fmt.Errorf("%w: 缺少 GeoIP 库 %s（应放在 %s/）。缺这个文件时 mihomo 会联网下载并卡死 90 秒，期间代理端口不会监听",
			ErrKernelMissing, GeoIPMetaDB, m.dataDir)
	}
	if m.usesGeosite() {
		if _, err := os.Stat(filepath.Join(m.dataDir, GeoFile)); err != nil {
			return fmt.Errorf("%w: 规则用到了 GEOSITE，但缺少 %s（应放在 %s/）",
				ErrKernelMissing, GeoFile, m.dataDir)
		}
	}
	return nil
}

// usesGeosite 订阅规则里是否引用了 GEOSITE
func (m *Manager) usesGeosite() bool {
	b, err := os.ReadFile(filepath.Join(m.dataDir, confFileName))
	if err != nil {
		return false
	}
	return bytes.Contains(b, []byte("GEOSITE"))
}

// Start 生成/复用配置并拉起 mihomo 进程。
// 注意 mihomo 必须显式传 -d 指定工作目录：不传会回落到 ~/.config/mihomo，
// 放在 dataDir 下的 geo 数据就找不到了（实测踩过这个坑）。
func (m *Manager) Start() error {
	m.mu.Lock()
	if m.running {
		m.mu.Unlock()
		return nil
	}
	// 这是一次显式启动，清除「已要求停止」标记，允许后续崩溃重新自愈
	m.stopRequested = false
	m.mu.Unlock()

	if err := m.CheckEnv(); err != nil {
		return err
	}
	// 先把配置准备好再进临界区。这一步可能要联网拉订阅，耗时不可控，
	// 持着 m.mu 做网络 IO 会把 Status()/Nodes() 这些读接口一起卡住。
	if err := m.ensureActiveConfig(); err != nil {
		return err
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	// 准备配置期间可能已被别的 goroutine 启动，复查一次
	if m.running {
		return nil
	}
	if len(m.nodes) == 0 {
		return fmt.Errorf("节点列表为空，请先添加订阅分组或重新拉取订阅")
	}
	confPath := filepath.Join(m.dataDir, confFileName)
	if _, err := os.Stat(confPath); err != nil {
		return fmt.Errorf("运行配置尚未生成，请先添加订阅分组")
	}

	// 恢复上次选中的节点名（内存态，进程重启后靠 config 里的 ActiveNode/ClashNode 还原）
	m.activeName = m.persistedActiveNameLocked()
	m.ensureActiveNodeLocked()

	m.cmd = exec.Command(m.bin, "-d", m.dataDir, "-f", confPath)
	if err := m.cmd.Start(); err != nil {
		return err
	}
	m.running = true
	m.restartMu.Lock()
	m.restartCount = 0
	m.lastRestart = time.Time{}
	m.restartMu.Unlock()

	// 异步把上次选中的节点重新应用一遍。mihomo 重启后策略组会回到配置里的
	// 默认成员，不重放的话用户会发现「重启后自己选的节点被换回去了」。
	//
	// 前置通道也必须一起重放，所以这个 goroutine 与「有没有选中节点」无关 ——
	// 内核启动时会自己按 url-test 给前置组挑一个，而那个判据与「能不能承载
	// 家宽链」无关，挑错了家宽节点会集体失效（见 applyFront）。
	want := m.activeName
	go func() {
		if err := m.waitControl(25 * time.Second); err != nil {
			log.Printf("mihomo 控制接口未就绪，跳过节点恢复: %v", err)
			return
		}
		m.restoreSelection(want)
	}()

	go m.watch(m.cmd)
	m.ensureHealthLoop()
	log.Printf("mihomo 已启动 (节点 %d 个)", len(m.nodes))
	return nil
}

// ensureHealthLoop 启动普通节点健康看护，只会起一次。
//
// 放在 Start 而不是 NewManager：没启动过内核的 Manager（大量单元测试、
// 以及未部署内核的环境）不该挂着一个每 30 秒醒一次的 goroutine。
func (m *Manager) ensureHealthLoop() {
	m.healthMu.Lock()
	defer m.healthMu.Unlock()
	if m.healthOn {
		return
	}
	m.healthOn = true
	go m.healthLoop()
}

// healthLoop 定期检查「当前普通节点还能不能用」，不行就自动换一个。
func (m *Manager) healthLoop() {
	t := time.NewTicker(healthProbeInterval)
	defer t.Stop()
	for range t.C {
		m.healthTick()
	}
}

// healthTick 一次健康检查。
//
// 探测走内核控制口的 /proxies/{节点名}/delay —— 那是内核**直连该节点**发一个
// HEAD 请求，不经过规则分流。这一点很关键：走本程序的代理口去探测的话，
// 像 www.gstatic.com 这种被墙内 DNS 污染成国内 IP 的目标会命中 GEOIP,CN,DIRECT
// 走直连，节点明明是死的也会「探测成功」（2026-10-01 真机踩过）。
func (m *Manager) healthTick() {
	if m.cfg == nil {
		return
	}
	m.cfg.Lock()
	enabled := m.cfg.FailoverEnabled()
	m.cfg.Unlock()
	if !enabled {
		m.clearFailover()
		return
	}
	// 家宽模式不管：家宽配置是订阅原文，里面有内核自己的 url-test 组负责往下换。
	// 内核没在跑时也不管：那时探测必然失败，会把失败计数刷满。
	if !m.IsNormalMode() || !m.IsRunning() {
		m.clearFailover()
		return
	}

	name, ok := m.activeNormalName()
	if !ok || name == "" {
		return
	}

	_, err := ctrlProxyDelay(m.controlAddr(), name, healthProbeTimeoutMs, defaultTestURL)
	if err == nil {
		m.noteHealthy(name)
		return
	}

	n := m.bumpFail(name, err)
	if n < healthFailThreshold {
		return
	}
	m.tryFailover()
}

// activeNormalName 当前普通节点在**内核里**的名字（不是界面上的 ID）。
func (m *Manager) activeNormalName() (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.normalMode {
		return "", false
	}
	return m.activeName, true
}

func (m *Manager) noteHealthy(name string) {
	m.healthMu.Lock()
	wasFailing := m.failCount > 0
	m.failCount = 0
	m.failTried = nil
	m.failGaveUp = false
	m.healthMu.Unlock()
	if wasFailing {
		log.Printf("节点[%s]已恢复", name)
	}
}

// bumpFail 记一次失败并返回累计次数。只在刚达到阈值那一次写日志，
// 之后每 30 秒重复刷同一句会把日志淹掉。
func (m *Manager) bumpFail(name string, cause error) int {
	m.healthMu.Lock()
	m.failCount++
	n := m.failCount
	m.healthMu.Unlock()
	if n == healthFailThreshold {
		log.Printf("节点[%s]连续 %d 次探测失败(%v)，尝试自动故障转移", name, n, cause)
	}
	return n
}

// clearFailover 清掉看护状态（用户关了开关、切到家宽、内核停了）。
func (m *Manager) clearFailover() {
	m.healthMu.Lock()
	m.failCount = 0
	m.failTried = nil
	m.failGaveUp = false
	m.healthMu.Unlock()
}

// tryFailover 当前节点连续失败后，自动切换到其他可用节点。
//
// 与 xray 那套的差别：xray 是「进程起不来」触发，这里是「内核探测不通」触发；
// 候选排序（可达优先 → 延迟升序）与最多尝试次数保持一致，都走 config.FailoverOrder。
func (m *Manager) tryFailover() {
	m.healthMu.Lock()
	if m.failGaveUp {
		m.healthMu.Unlock()
		return
	}
	if len(m.failTried) >= healthMaxTotalTry {
		m.failGaveUp = true
		m.healthMu.Unlock()
		log.Printf("自动故障转移: 已连续试过 %d 个节点都不通，停止自动切换，请检查订阅或网络后手动选择节点",
			healthMaxTotalTry)
		return
	}
	tried := make(map[string]bool, len(m.failTried)+healthMaxTry)
	for id := range m.failTried {
		tried[id] = true
	}
	m.healthMu.Unlock()

	m.cfg.Lock()
	all := m.cfg.NormalNodes()
	cur := m.cfg.ActiveNode
	m.cfg.Unlock()

	// 把「正在失败的这个」也记进已试列表。只记切换目标是不够的：
	// 换走之后再挑候选时，原节点会重新回到候选里，于是又换回去，
	// 在两个坏节点之间来回横跳（单测 TestFailoverMovesOnToUntriedNodes 抓到过）。
	if cur != "" {
		m.markTried(cur)
	}

	cands := config.FailoverOrder(all, cur, tried)
	if len(cands) == 0 {
		m.healthMu.Lock()
		m.failGaveUp = true
		m.healthMu.Unlock()
		log.Printf("自动故障转移: 已经没有没试过的节点了，停止自动切换，请手动选择节点")
		return
	}

	if len(cands) > healthMaxTry {
		cands = cands[:healthMaxTry]
	}
	for _, n := range cands {
		m.markTried(n.ID)
		log.Printf("自动故障转移: 尝试切换到[%s]", n.Name)
		if err := m.SwitchNormalNode(n.ID); err != nil {
			log.Printf("自动故障转移: 切到[%s]失败: %v", n.Name, err)
			continue
		}
		log.Printf("自动故障转移成功 -> [%s]", n.Name)
		// 只清失败计数，**不清 failTried** —— 新节点还没验证过，
		// 万一是坏的，下一轮要从「没试过的」里接着挑，而不是又回到刚才那个。
		m.healthMu.Lock()
		m.failCount = 0
		m.healthMu.Unlock()
		return
	}
	log.Printf("自动故障转移失败: 本轮尝试的 %d 个节点都没切成功", len(cands))
}

func (m *Manager) markTried(id string) {
	m.healthMu.Lock()
	if m.failTried == nil {
		m.failTried = map[string]bool{}
	}
	m.failTried[id] = true
	m.healthMu.Unlock()
}

// restoreSelection 内核就绪之后，把用户选的节点与测速验过的前置重新应用一遍。
//
// 单独抽出来是为了能被测试直接调用 —— 它整条都在跟内核说话，只有拆出来
// 才测得到「两个重放都真的做了、顺序也对」。
func (m *Manager) restoreSelection(want string) {
	// 两种模式都会走到这里，日志里要分得清是哪一种 —— 内核统一之后普通节点
	// 也由这个函数重放，再写死「家宽节点」会让人在排查时找错方向。
	m.mu.Lock()
	kind := "普通"
	if !m.normalMode {
		kind = "家宽"
	}
	m.mu.Unlock()

	if want != "" {
		if err := m.applyNode(want); err != nil {
			log.Printf("恢复%s节点[%s]失败: %v", kind, want, err)
		} else {
			log.Printf("已恢复%s节点[%s]", kind, want)
		}
	}
	if err := m.applyFront(); err != nil {
		log.Printf("恢复前置通道失败: %v", err)
	}
}

// watch 监控 mihomo 进程。家宽节点掉线由 mihomo 自己的 fallback 组负责往下换，
// 所以这里只做「崩了拉起来」，不做节点级故障转移。
func (m *Manager) watch(cmd *exec.Cmd) {
	_ = cmd.Wait()

	m.mu.Lock()
	isCurrent := m.cmd == cmd
	if isCurrent {
		m.running = false
		m.cmd = nil
	}
	m.mu.Unlock()
	// 只有「退出的正是当前进程」才进入自愈流程。Stop() 会把 m.cmd 置 nil，
	// 此时 isCurrent 为 false，说明这次退出是主动停机的结果，不应重启。
	if !isCurrent {
		return
	}

	// 防无限重启：3 次内如果始终撑不过 30 秒，判定为配置/环境问题，放弃
	m.restartMu.Lock()
	now := time.Now()
	if now.Sub(m.lastRestart) > 30*time.Second {
		m.restartCount = 0
	}
	m.restartCount++
	m.lastRestart = now
	n := m.restartCount
	m.restartMu.Unlock()

	if n > 3 {
		log.Printf("mihomo 连续重启 %d 次失败，放弃自动重启，请检查家宽订阅与 geo 数据", n)
		return
	}

	log.Printf("mihomo 异常退出 (%d/3)，5 秒后自动重启", n)
	time.Sleep(5 * time.Second)

	// 临重启前在锁内确认此刻到底该不该自愈：
	// 既可能是用户刚点了停止（stopRequested），也可能是别的操作已经把进程
	// 拉起来了（改端口、切内核、恢复配置）。两者都必须跳过。
	m.mu.Lock()
	stopped := m.stopRequested
	already := m.running || m.cmd != nil
	m.mu.Unlock()
	if stopped {
		log.Printf("已收到停止指令，取消本次自愈重启")
		return
	}
	if already {
		log.Printf("mihomo 已由其他操作启动，跳过本次自愈")
		return
	}

	_ = m.Start()
}

// Stop 停止 mihomo 进程。先置 stopRequested 再置 running=false，
// 否则在 5 秒自愈窗口内点停止会被 watch 自己拉起来。
func (m *Manager) Stop() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stopRequested = true
	if m.cmd != nil && m.cmd.Process != nil {
		m.cmd.Process.Kill()
		// 不在此处 Wait，由 watch goroutine 收尸，避免重复 Wait panic
	}
	m.running = false
	m.cmd = nil
	// 内核停了就没有「当前节点通不通」可言，把看护状态清干净 ——
	// 否则下次启动会带着上一次的失败计数，可能刚起来就误判并换节点。
	m.clearFailover()
}

// SwitchNode 切换家宽节点。name 为订阅里的节点名。
//
// 走 external-controller 热切换而不是重启进程：重启会让 OpenVPN 重新握手，
// 代价远高于一次 API 调用。切换结果落盘到 config.ClashNode，供重启后恢复。
func (m *Manager) SwitchNode(name string) error {
	if err := m.applyNode(name); err != nil {
		return err
	}
	if m.cfg != nil {
		m.cfg.Lock()
		m.cfg.ClashNode = name
		_ = m.cfg.Save()
		m.cfg.Unlock()
	}
	return nil
}

// applyNode 只做内核侧的切换，不落盘。
func (m *Manager) applyNode(name string) error {
	m.mu.Lock()
	nodeGroup, topGroup := m.nodeGroup, m.topGroup
	m.mu.Unlock()

	if nodeGroup == "" {
		return fmt.Errorf("未识别到家宽节点选择组，请重新拉取订阅")
	}
	// 1) 把家宽选择组拨到指定节点
	if err := m.putProxy(nodeGroup, name); err != nil {
		return fmt.Errorf("切换家宽节点失败: %w", err)
	}
	// 2) 订阅默认让顶层主组指向「家宽自动」（fallback 组）。手动选具体节点时
	//    必须把顶层组也拨到手动组上，否则流量仍走自动选择，用户的选择形同虚设。
	if topGroup != "" && topGroup != nodeGroup {
		if err := m.putProxy(topGroup, nodeGroup); err != nil {
			return fmt.Errorf("切换主策略组失败: %w", err)
		}
	}
	m.mu.Lock()
	m.activeName = name
	m.mu.Unlock()
	return nil
}

// Nodes 返回家宽节点名列表（副本，调用方可安全持有）。
// 返回的是「当前生效分组」的节点。
func (m *Manager) Nodes() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, len(m.nodes))
	copy(out, m.nodes)
	return out
}

// GroupNodes 返回某个家宽分组的节点名列表。
//
// 当前生效那个分组直接用内存里的解析结果；别的分组现场读订阅原文解析。
//
// 为什么不能只认内存：新增的家宽分组还没被「启用」，内存里没有它的节点，
// 界面上就会显示空列表 —— 而列表里没有节点，用户就没法点任何一个来启用它，
// 这个分组于是永远启用不了（先有鸡还是先有蛋）。
//
// 只读文件、不联网，可以放心在每次加载列表时调用。
func (m *Manager) GroupNodes(groupID string) []string {
	m.mu.Lock()
	if m.loadedGroup == groupID && len(m.nodes) > 0 {
		out := make([]string, len(m.nodes))
		copy(out, m.nodes)
		m.mu.Unlock()
		return out
	}
	m.mu.Unlock()

	body, err := m.readSub(groupID)
	if err != nil {
		return nil
	}
	return parseSubscription(string(body)).nodes
}

// GroupNodeCount 返回某个家宽分组的节点数（界面列表展示用）。
// 只读内存缓存；缓存由 restoreFromDisk 与 FetchSubscription 填充。
func (m *Manager) GroupNodeCount(groupID string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.nodeCounts[groupID]
}

// Status 返回状态，字段名与 v2ray.Manager.Status 对齐，web 层可直接透传。
//
// 普通模式下 activeNode 是节点 ID、activeName 是节点显示名 —— 与 v2ray 那套
// 完全一致，界面不用区分当前跑的是哪种配置。
func (m *Manager) Status() map[string]interface{} {
	m.mu.Lock()
	defer m.mu.Unlock()
	activeNode, activeName := "", m.activeName
	if m.normalMode {
		activeNode = m.normalByName[m.activeName]
		activeName = m.normalByID[activeNode].display
	} else if m.activeName != "" {
		activeNode = config.ClashNodeID(m.loadedGroup, m.activeName)
	}
	return map[string]interface{}{
		"running":     m.running,
		"kernel":      config.KindClash,
		"activeNode":  activeNode,
		"activeName":  activeName,
		"loadedGroup": m.loadedGroup,
		"normalMode":  m.normalMode,
	}
}

// --- 订阅拉取与配置生成 ---

// FetchSubscription 拉取指定家宽分组的订阅，落盘订阅原文；
// 若这个分组正是当前生效的那份，顺手把可运行的 mihomo 配置一起重写。
// 返回家宽节点名列表。
//
// groupID 必须非空：订阅原文按分组各存一份，才能在家宽分组之间来回切换
// 而不用每次重新联网拉取。
func (m *Manager) FetchSubscription(groupID, subURL, proxy string) ([]string, error) {
	if strings.TrimSpace(groupID) == "" {
		return nil, fmt.Errorf("缺少家宽分组 ID")
	}
	if strings.TrimSpace(subURL) == "" {
		return nil, fmt.Errorf("家宽订阅地址为空")
	}
	body, err := httpGet(subURL, proxy)
	if err != nil {
		return nil, fmt.Errorf("拉取家宽订阅失败: %w", err)
	}
	if len(body) == 0 {
		return nil, fmt.Errorf("家宽订阅内容为空")
	}

	info := parseSubscription(string(body))
	if len(info.nodes) == 0 {
		return nil, fmt.Errorf("订阅里没有 openvpn 节点（cfnew 的链接需要带 target=vg 参数）")
	}

	if err := os.MkdirAll(m.dataDir, 0755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(m.dataDir, subFileNameFor(groupID)), body, 0644); err != nil {
		return nil, err
	}

	m.mu.Lock()
	// 只有「当前生效」那个分组的解析结果与运行配置需要跟着更新；
	// 别的分组只是把订阅存下来备用，等真正切过去时再生成配置。
	//
	// 注意判定必须是 loadedGroup == groupID，不能放宽成「还没加载过任何分组」——
	// 那样新增第二个家宽分组时会把它误认成当前生效的那份，于是运行配置被悄悄
	// 换成新分组，而 cfg 里记录的生效分组还是旧的，两边对不上。
	isActive := m.loadedGroup == groupID
	if m.nodeCounts == nil {
		m.nodeCounts = map[string]int{}
	}
	m.nodeCounts[groupID] = len(info.nodes)
	if isActive {
		m.nodes = info.nodes
		m.nodeGroup = info.nodeGroup
		m.topGroup = info.topGroup
	}
	m.mu.Unlock()

	if isActive {
		if err := m.writeConf(body); err != nil {
			return nil, err
		}
	}

	log.Printf("家宽订阅已更新[分组 %s]: %d 个节点, 手动选择组=%q, 顶层主组=%q",
		groupID, len(info.nodes), info.nodeGroup, info.topGroup)
	return info.nodes, nil
}

// LoadGroup 把指定家宽分组加载为「当前生效」的配置：
// 优先用本地已存的订阅原文，没有再联网拉一次。
//
// 已经有别的分组生效时会切换过去（重写 clash-config.yaml）。调用方负责
// 在需要时重启内核 —— 本方法只管文件，不动进程。
func (m *Manager) LoadGroup(groupID, subURL, proxy string) error {
	if strings.TrimSpace(groupID) == "" {
		return fmt.Errorf("缺少家宽分组 ID")
	}

	m.mu.Lock()
	already := m.loadedGroup == groupID && len(m.nodes) > 0
	m.mu.Unlock()
	// 解析结果在内存里还不够，运行配置也得真的在磁盘上 ——
	// restoreFromDisk 只恢复内存，不会重写 clash-config.yaml。
	if already && m.HasConfig() {
		return nil
	}

	body, err := m.readSub(groupID)
	if err != nil {
		// 本地没有缓存，联网拉一次（顺带把订阅原文落盘）。
		// 注意拉完必须重新读一遍再往下走：FetchSubscription 只负责存文件，
		// 生成运行配置是下面这段的事，直接 return 会留下「有订阅原文但没有
		// 可运行配置」的状态，接着 Start 就会失败。
		if _, ferr := m.FetchSubscription(groupID, subURL, proxy); ferr != nil {
			return ferr
		}
		if body, err = m.readSub(groupID); err != nil {
			return err
		}
	}

	info := parseSubscription(string(body))
	if len(info.nodes) == 0 {
		return fmt.Errorf("订阅里没有 openvpn 节点（cfnew 的链接需要带 target=vg 参数）")
	}
	if err := m.writeConf(body); err != nil {
		return err
	}
	m.mu.Lock()
	m.loadedGroup = groupID
	m.nodes = info.nodes
	m.nodeGroup = info.nodeGroup
	m.topGroup = info.topGroup
	// 切回家宽模式，必须把普通节点那套映射一起清掉。不清的话 Status()、
	// 选节点恢复、ensureActiveNodeLocked 会继续按普通模式的表去查，
	// 查出来全是空值 —— 表现为「切到家宽后界面没有高亮、配置也不落盘」。
	m.normalMode = false
	m.normalByID, m.normalByName = nil, nil
	m.normalSig = ""
	if m.nodeCounts == nil {
		m.nodeCounts = map[string]int{}
	}
	m.nodeCounts[groupID] = len(info.nodes)
	m.mu.Unlock()
	log.Printf("家宽分组 %s 已加载 (%d 个节点)", groupID, len(info.nodes))
	return nil
}

// readSub 读取某个分组的订阅原文（见 readSubFile）。
func (m *Manager) readSub(groupID string) ([]byte, error) {
	return readSubFile(m.dataDir, groupID)
}

// normalNodeRef 一个普通节点在生成配置之后对应的两个名字。
type normalNodeRef struct {
	clash   string // 配置里的节点名（重名时会带 " #N" 后缀）
	display string // 界面显示用的原始节点名
}

// IsNormalMode 当前加载的是不是普通节点配置。
func (m *Manager) IsNormalMode() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.normalMode
}

// LoadNormal 用普通节点生成一份 mihomo 配置并落盘。
//
// 与 LoadGroup 的区别：家宽订阅给的本来就是一份完整的 Clash 配置，只需改顶层
// 7 个键；普通订阅给的是一串 trojan:// / vless:// 链接，程序已把它们解析成
// config.Node，没有 Clash 原文可用，所以整份配置都要自己生成（见 BuildNormalConfig）。
//
// 只写文件、不动进程 —— 与 LoadGroup 一致，调用方负责在需要时重启内核。
func (m *Manager) LoadNormal(nodes []config.Node, mode string) error {
	if len(nodes) == 0 {
		return fmt.Errorf("还没有可用的普通节点，请先添加订阅或导入节点")
	}
	socksPort, httpPort := m.ports()
	nc, err := BuildNormalConfig(nodes, NormalOpts{
		SocksPort: socksPort,
		HTTPPort:  httpPort,
		RedirPort: RedirPort,
		CtrlPort:  ControlPort,
		// 与 xray 那套保持一致：入站监听 0.0.0.0，网关模式下局域网设备才能用
		AllowLan:  true,
		ProxyMode: mode,
	})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(m.dataDir, 0755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(m.dataDir, confFileName), nc.YAML, 0644); err != nil {
		return err
	}

	// 生成配置时重名节点会被改名（加 " #N" 后缀），所以必须把「节点 ID」
	// 与「配置里真正用的名字」两张表都建出来：界面点的是 ID，内核认的是名字。
	display := make(map[string]string, len(nodes))
	for _, n := range nodes {
		display[n.ID] = n.Name
	}
	byID := make(map[string]normalNodeRef, len(nc.NameToID))
	byName := make(map[string]string, len(nc.NameToID))
	for clashName, id := range nc.NameToID {
		byID[id] = normalNodeRef{clash: clashName, display: display[id]}
		byName[clashName] = id
	}

	m.mu.Lock()
	m.normalMode = true
	m.normalSig = NormalNodesSig(nodes)
	m.normalByID, m.normalByName = byID, byName
	// 普通模式没有家宽分组，这些字段必须清空 —— 留着会让 Status() 拼出一个
	// 形如 "clash::节点名" 的假 ID，界面上的高亮会串到不存在的行上。
	m.loadedGroup = ""
	m.nodes = append([]string(nil), nc.Order...)
	m.nodeGroup = NormalGroupName
	m.topGroup = NormalGroupName
	// 还原上次选中的节点，并修好「它已经不在列表里」的情况（见 ensureActiveNodeLocked）。
	// 放在生成配置这一步是刻意的：修复是纯本地操作，不该依赖内核能不能起来。
	m.activeName = m.persistedActiveNameLocked()
	m.ensureActiveNodeLocked()
	m.mu.Unlock()

	if len(nc.Skipped) > 0 {
		log.Printf("普通节点配置已生成 (%d 个节点, 模式 %s, %d 个无法转换已跳过)",
			len(nc.Order), mode, len(nc.Skipped))
	} else {
		log.Printf("普通节点配置已生成 (%d 个节点, 模式 %s)", len(nc.Order), mode)
	}
	return nil
}

// NormalConfigUpToDate 当前加载的普通节点配置是否就是用这批节点生成的。
// 不是普通模式、或节点集合变了，都返回 false。
func (m *Manager) NormalConfigUpToDate(nodes []config.Node) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.normalMode && m.normalSig == NormalNodesSig(nodes)
}

// SetActiveNormalNode 记录「用户选了哪个普通节点」并落盘，但**不碰内核**。
//
// 为什么需要这一步：换配置那条路径要 Stop -> 改配置 -> Start，而 Start 会在
// 内核就绪后异步重放 cfg.ActiveNode。若不在 Start 之前把选择落盘，重放就会把
// 上一个节点又拨回来，用户点了没反应（或过几秒被改回去）。
//
// 只写文件/内存，不做网络调用，所以内核没跑时也能安全调用。
func (m *Manager) SetActiveNormalNode(nodeID string) error {
	m.mu.Lock()
	ref, ok := m.normalByID[nodeID]
	if ok {
		m.activeName = ref.clash
	}
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("这个节点不在当前配置里（可能已被删除，刷新页面后重试）")
	}
	if m.cfg != nil {
		m.cfg.Lock()
		m.cfg.ActiveNode = nodeID
		_ = m.cfg.Save()
		m.cfg.Unlock()
	}
	return nil
}

// SwitchNormalNode 在普通节点配置里切换当前使用的节点。nodeID 是界面上的节点 ID。
//
// 与家宽不同，这里**不重启内核**：普通节点配置只有一个 select 组，
// 拨一下组就是全部工作，毫秒级。要求内核已经在跑（控制口可用）。
func (m *Manager) SwitchNormalNode(nodeID string) error {
	if err := m.SetActiveNormalNode(nodeID); err != nil {
		return err
	}
	m.mu.Lock()
	name := m.activeName
	m.mu.Unlock()
	return m.applyNode(name)
}

// ActiveNormalNodeID 当前选中的普通节点 ID（没有时为空）。
func (m *Manager) ActiveNormalNodeID() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.normalMode {
		return ""
	}
	return m.normalByName[m.activeName]
}

// readSubFile 读取某个分组的订阅原文。
//
// 找不到分组自己的文件时会回落到升级前的那份全局文件（clash-sub.yaml）：
// 老用户升级上来时磁盘上只有那一份，能读到就不用再联网拉一次 ——
// 而盒子刚开机时代理还没起来，联网拉订阅大概率失败，那就成死锁了。
// 读到之后顺手复制到新文件名，下次直接命中。
func readSubFile(dataDir, groupID string) ([]byte, error) {
	b, err := os.ReadFile(filepath.Join(dataDir, subFileNameFor(groupID)))
	if err == nil {
		return b, nil
	}
	legacy, lerr := os.ReadFile(filepath.Join(dataDir, legacySubFileName))
	if lerr != nil {
		return nil, err
	}
	_ = os.WriteFile(filepath.Join(dataDir, subFileNameFor(groupID)), legacy, 0644)
	log.Printf("家宽订阅原文沿用升级前的 %s（已复制给分组 %s）", legacySubFileName, groupID)
	return legacy, nil
}

// writeConf 按订阅原文生成 mihomo 运行配置（只改顶层 7 个键，见 rewriteConfig）。
func (m *Manager) writeConf(body []byte) error {
	socksPort, httpPort := m.ports()
	conf, err := rewriteConfig(string(body), confPorts{
		socks: socksPort, http: httpPort,
		redir: RedirPort, ctrl: ControlPort, allowLan: true,
	})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(m.dataDir, 0755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(m.dataDir, confFileName), []byte(conf), 0644)
}

// SubBody 读取某个家宽分组的订阅原文（不联网）。
// 探针要用它生成自己那份配置，所以暴露出去。
func (m *Manager) SubBody(groupID string) ([]byte, error) {
	return readSubFile(m.dataDir, groupID)
}

// FrontInfo 读出某个分组的前置通道：组名 + 它的成员列表。
//
// 两样东西一起返回是因为调用方（web 层的测速流程）通常两样都要，而
// 解析一遍 110 KB 的订阅原文并不便宜。取不到时两样都是零值。
func (m *Manager) FrontInfo(groupID string) (name string, members []string) {
	body, err := readSubFile(m.dataDir, groupID)
	if err != nil {
		return "", nil
	}
	info := parseSubscription(string(body))
	return info.frontGroup, info.frontMembers
}

// FrontGroup 家宽订阅里 openvpn 节点链式转发所依赖的前置组名。
// 取不到时返回空串（订阅结构变了，或这个分组还没拉过订阅）。
func (m *Manager) FrontGroup(groupID string) string {
	name, _ := m.FrontInfo(groupID)
	return name
}

// SetFront 把前置通道组固定到指定节点。
//
// 前置组在订阅里是 url-test，本来由内核按「自己访问测速地址快不快」自动选；
// 这里用 PUT /proxies 把它固定住（内核侧是 ForceSet），因为那个判据与
// 「能不能承载家宽链」无关 —— 实测证据见 web 层 ensureFrontUsable 的说明。
func (m *Manager) SetFront(group, name string) error {
	return m.putProxy(group, name)
}

// applyFront 把「测速时验过的那个前置节点」重新固定到前置组上。
//
// 为什么每次内核启动都要重放：前置组是 url-test，内核启动时会自己做一次
// 健康检查、按「它自己访问 gstatic 快不快」挑一个。那个判据与「能不能承载
// 一条 OpenVPN 长连接」无关（实测：自己 171ms 的带不动家宽链、197ms 的反而
// 能），而且订阅没写 lazy、mihomo 默认 lazy=true，前置组没有直接流量就不再
// 复查 —— 挑错了永远不会自己纠正。不重放的话就会出现「测速时明明好好的，
// 选上没一会儿就失效」，因为测速用的是探针、用户点节点后跑的是内核，两边
// 各自挑各自的前置。
//
// 没有记录、或记录的那个节点已经不在订阅里（订阅换节点很频繁）时什么都不做，
// 让内核按订阅的自动选择走。
func (m *Manager) applyFront() error {
	node, front := m.pinnedFront()
	if node == "" {
		return nil
	}
	if err := m.putProxy(front, node); err != nil {
		return fmt.Errorf("固定前置通道[%s]到[%s]失败: %w", front, node, err)
	}
	// ★ 固定只是「偏好」，不是「强制」。内核 fast() 会先检查这个节点在它自己
	// 的账本里活不活（判据是能不能直接访问订阅里写的那个测速地址），不活就
	// 静默忽略、继续用它自己挑的那个 —— 盒子实测 216 个 CF 前置里约一半如此。
	// 这里读一次 now 把结论写进日志，免得「明明固定了却没生效」变成查不出来的悬案。
	if now, err := m.ProxyNow(front); err == nil && now != node {
		log.Printf("前置通道[%s]已按记录固定到[%s]，但内核此刻仍在用[%s] —— 内核认为[%s]不活（它的判据是能不能直接访问订阅里的测速地址，与能否承载家宽链无关）",
			front, node, now, node)
	}
	return nil
}

// pinnedFront 读出「当前生效分组」记录的前置节点与它所属的前置组名。
// 两者任一为空都表示没有可重放的记录。
func (m *Manager) pinnedFront() (node, front string) {
	groupID := m.LoadedGroup()
	if groupID == "" || m.cfg == nil {
		return "", ""
	}
	m.cfg.Lock()
	if g := m.cfg.FindClashGroup(groupID); g != nil {
		node = g.FrontNode
	}
	m.cfg.Unlock()
	if node == "" {
		return "", ""
	}
	// 记录必须仍然成立：订阅刷新后组名和成员都可能变
	name, members := m.FrontInfo(groupID)
	if name == "" {
		return "", ""
	}
	for _, n := range members {
		if n == node {
			return node, name
		}
	}
	return "", ""
}

// UnloadGroup 删除某个家宽分组留下的文件。
//
// 如果删掉的正是当前生效的那份，运行配置会一起清掉，并且必须让内核停下来 ——
// 否则 mihomo 还在跑一份已经不存在的分组的配置，用户会一头雾水。
func (m *Manager) UnloadGroup(groupID string) {
	_ = os.Remove(filepath.Join(m.dataDir, subFileNameFor(groupID)))

	m.mu.Lock()
	// 普通模式与家宽分组互不相干：删家宽分组不该动普通节点那份配置。
	// 此时 loadedGroup 恒为空，下面的比较自然为 false。
	wasActive := !m.normalMode && m.loadedGroup == groupID
	if wasActive {
		m.loadedGroup = ""
		m.nodes = nil
		m.nodeGroup = ""
		m.topGroup = ""
		m.activeName = ""
	}
	delete(m.nodeCounts, groupID)
	m.mu.Unlock()

	if !wasActive {
		return
	}
	_ = os.Remove(filepath.Join(m.dataDir, confFileName))
	if m.IsRunning() {
		m.Stop()
	}
	log.Printf("家宽分组 %s 已删除，相关配置已清理", groupID)
}

// EnsureGroupSubs 为「本地还没有订阅原文」的家宽分组补拉一次。
//
// 什么时候会缺：从旧版本升级上来时，家宽地址是从配置里的 ClashSubURL 迁移
// 出来的，而订阅原文（clash-sub-<分组ID>.yaml）要真拉过一次才有。不补的话，
// 用户在侧栏点进这个分组会看到空列表 —— 而列表里没有节点就没法点任何一个来
// 启用它，这个分组就永远启用不了。
//
// 已有原文的分组一律跳过：开机不该为每个分组都发一次网络请求。
// 失败只记日志、不返回错误：用户随时可以点「更新全部」重来，没必要因此
// 让启动流程报错。
//
// 会联网，调用方应放在后台 goroutine 里，别拖住代理启动。
func (m *Manager) EnsureGroupSubs() {
	if m.cfg == nil {
		return
	}
	m.cfg.Lock()
	type job struct{ id, name, url, proxy string }
	jobs := make([]job, 0)
	globalProxy := m.cfg.SubProxy
	for _, g := range m.cfg.ClashGroups() {
		if g.URL == "" {
			continue
		}
		proxy := globalProxy
		if g.SubProxy != "" {
			proxy = g.SubProxy
		}
		jobs = append(jobs, job{id: g.ID, name: g.Name, url: g.URL, proxy: proxy})
	}
	m.cfg.Unlock()

	for _, j := range jobs {
		if _, err := os.Stat(filepath.Join(m.dataDir, subFileNameFor(j.id))); err == nil {
			continue // 本地已有原文，交给「更新全部」按需刷新即可
		}
		if _, err := m.FetchSubscription(j.id, j.url, j.proxy); err != nil {
			log.Printf("家宽分组[%s]订阅补拉失败: %v", j.name, err)
			continue
		}
		log.Printf("家宽分组[%s]订阅已补齐", j.name)
	}
}

// Reload 重新拉取指定分组的订阅，并在内核正在跑这个分组时重启它以加载新配置。
//
// 只刷新「当前生效」那个分组才会重启内核：mihomo 一次只加载一份配置，
// 刷新别的分组只是把订阅原文存下来备用，重启一次要十几秒且毫无意义。
func (m *Manager) Reload(groupID, subURL, proxy string) error {
	if _, err := m.FetchSubscription(groupID, subURL, proxy); err != nil {
		return err
	}
	if !m.IsRunning() || m.LoadedGroup() != groupID {
		return nil
	}
	m.Stop()
	time.Sleep(500 * time.Millisecond)
	return m.Start()
}

// ensureActiveConfig 确保「当前应当生效的那份配置」已就绪。
//
// 内核统一之后，mihomo 要跑两种配置之一：普通节点配置，或某个家宽分组的订阅原文。
// 由 config.ActiveKind 决定，两者都落进同一个 confFileName。
//
// 家宽那种还多一层兜底：配的是家宽但一个家宽分组都没有时（比如刚把最后一个删掉），
// 回落到普通节点 —— 总比什么都不加载、代理直接起不来强。
func (m *Manager) ensureActiveConfig() error {
	kind := config.KindNormal
	if m.cfg != nil {
		m.cfg.Lock()
		kind = m.cfg.ActiveKind()
		m.cfg.Unlock()
	}
	if kind == config.KindClash {
		groupID, subURL, proxy := m.activeGroupInfo()
		if groupID != "" {
			return m.LoadGroup(groupID, subURL, proxy)
		}
		log.Printf("配置指向家宽但没有任何家宽分组，回落到普通节点")
	}
	return m.ensureNormalConfig()
}

// PrepareNormalConfig 用当前配置里的全部普通节点生成运行配置，并顺带修好
// 「上次选中的节点已经不在列表里」的情况。纯本地操作：不联网、不需要内核在跑。
//
// 为什么要单独暴露出来给开机流程用：修配置这件事必须在**拉起内核之前**完成。
// 内核启动（Start）第一步是 CheckEnv，内核文件没部署好就直接返回了，根本走不到
// 生成配置那一步 —— 于是配置里会一直留着一个已经不存在的节点 ID，界面挂着一个
// 不存在的「当前使用中」，用户还得手动去点一次。
func (m *Manager) PrepareNormalConfig() error {
	return m.ensureNormalConfig()
}

// ensureNormalConfig 用当前配置里的全部普通节点生成运行配置。
func (m *Manager) ensureNormalConfig() error {
	if m.cfg == nil {
		return fmt.Errorf("配置尚未注入")
	}
	m.cfg.Lock()
	nodes := m.cfg.NormalNodes()
	mode := m.cfg.ProxyMode
	m.cfg.Unlock()
	if len(nodes) == 0 {
		return fmt.Errorf("还没有可用节点，请先添加订阅分组或导入节点")
	}
	return m.LoadNormal(nodes, mode)
}

// activeGroupInfo 读出当前应当生效的家宽分组的 ID / 订阅地址 / 订阅代理。
// 三者都为空表示一个家宽分组都没有。
func (m *Manager) activeGroupInfo() (groupID, subURL, proxy string) {
	if m.cfg == nil {
		return "", "", ""
	}
	m.cfg.Lock()
	defer m.cfg.Unlock()
	proxy = m.cfg.SubProxy
	if g := m.cfg.ActiveClashGroup(); g != nil {
		groupID, subURL = g.ID, g.URL
		if g.SubProxy != "" {
			proxy = g.SubProxy
		}
	}
	return
}

// restoreFromDisk 从磁盘上的订阅原文恢复解析结果（只读文件，不联网）。
//
// 为什么需要它：进程刚起来时内存里没有任何解析结果，而界面每次刷新都会问
// 「这个家宽分组有哪些节点」。不在这里恢复的话，重启后家宽分组会显示成空的，
// 用户得手动点一次「更新」才出来 —— 这正是修掉的那个 bug。
//
// 顺带把所有家宽分组的节点数都统计出来：侧栏每个分组都要显示节点数，
// 而只有「当前生效」那个的节点是加载在内存里的。
//
// 刻意不联网：本方法在启动时调用，不能因为上游不可达就把启动挂住。
func (m *Manager) restoreFromDisk() {
	m.mu.Lock()
	if m.cfg == nil {
		m.mu.Unlock()
		return
	}
	m.mu.Unlock()

	m.cfg.Lock()
	groups := m.cfg.ClashGroups()
	activeID := ""
	if g := m.cfg.ActiveClashGroup(); g != nil {
		activeID = g.ID
	}
	m.cfg.Unlock()
	if len(groups) == 0 {
		return
	}

	counts := make(map[string]int, len(groups))
	var activeInfo subInfo
	activeOK := false
	for _, g := range groups {
		body, err := m.readSub(g.ID)
		if err != nil {
			continue
		}
		info := parseSubscription(string(body))
		counts[g.ID] = len(info.nodes)
		if g.ID == activeID && len(info.nodes) > 0 {
			activeInfo, activeOK = info, true
		}
	}

	m.mu.Lock()
	m.nodeCounts = counts
	if activeOK && len(m.nodes) == 0 {
		m.loadedGroup = activeID
		m.nodes = activeInfo.nodes
		m.nodeGroup = activeInfo.nodeGroup
		m.topGroup = activeInfo.topGroup
	}
	m.mu.Unlock()
	if activeOK {
		log.Printf("已从磁盘恢复家宽分组 %s 的解析结果 (%d 个节点)", activeID, len(activeInfo.nodes))
	}
}

// httpGet 拉取订阅。UA 伪装成 Clash 客户端 —— 部分订阅服务端按 UA 决定
// 返回 Clash 配置还是 base64 节点列表，UA 不对就拿不到家宽节点。
func httpGet(rawURL, proxy string) ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "clash-verge/v2.0.0")
	req.Header.Set("Accept", "*/*")

	tr := &http.Transport{}
	if proxy != "" {
		if pu, err := url.Parse(proxy); err == nil {
			tr.Proxy = http.ProxyURL(pu)
		}
	}
	cli := &http.Client{Timeout: 90 * time.Second, Transport: tr}
	resp, err := cli.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("订阅服务返回 %s", resp.Status)
	}
	// 家宽订阅约 110 KB，给 16 MB 上限防止意外拉到大文件
	return io.ReadAll(io.LimitReader(resp.Body, 16<<20))
}

// --- external-controller 客户端 ---

var (
	apiClient = &http.Client{Timeout: 10 * time.Second}

	// stateClient 读内核状态用（/proxies、/version）。
	// 短超时是刻意的：加载家宽节点列表时每次都要读一遍历史延迟，
	// 不能让内核的慢响应把界面拖住。
	stateClient = &http.Client{Timeout: 3 * time.Second}

	// delayClient 单个节点测延迟。家宽节点要现场建立 OpenVPN 隧道，
	// 比普通节点慢得多，超时给宽一些。
	delayClient = &http.Client{Timeout: 30 * time.Second}

	// groupDelayClient 整组测延迟。内核内部会并发跑，71 个节点可能要一两分钟。
	groupDelayClient = &http.Client{Timeout: 180 * time.Second}
)

// defaultTestURL 测延迟的兜底目标地址 —— 订阅里读不到组自己的 url 时用它。
//
// 必须是 https。订阅开了 unified-delay（cfnew 默认开）时，mihomo 会对同一个
// 地址连发两次 HEAD 请求；地址是 http:// 时官方源码会打印
//
//	It is recommended to use HTTPS ... using HTTP may result in failed tests
//
// 表现就是数字忽大忽小、时有时无。cfnew 订阅自己给组写的就是 https://。
const defaultTestURL = "https://www.gstatic.com/generate_204"

// TestURLOf 从订阅原文里挑出「测延迟该用的目标地址」。
//
// 优先用前置通道组自己写的 url：家宽节点的出口全挤在那条链上，订阅就是拿这个
// 地址判定链子好坏的，我们跟着用同一个，界面上的数字才和内核自动选点的依据一致。
// 前置组没写 url（比如它是 select 组）时，退而取第一个写了 url 的组。
//
// 为什么从订阅里读而不是写死：写死等于把订阅的配置抄一份进自己代码，订阅换了
// 测试地址我们无从知晓；早先硬编码的 http:// 更是正好踩中上面那条官方警告。
func TestURLOf(subBody []byte) string {
	info := parseSubscription(string(subBody))
	if u := info.groupURLs[info.frontGroup]; u != "" {
		return u
	}
	for _, name := range info.groupOrder {
		if u := info.groupURLs[name]; u != "" {
			return u
		}
	}
	return defaultTestURL
}

// TestURL 取某个家宽分组测延迟要用的地址。读不到订阅原文就退回默认值。
func (m *Manager) TestURL(groupID string) string {
	body, err := readSubFile(m.dataDir, groupID)
	if err != nil {
		return defaultTestURL
	}
	return TestURLOf(body)
}

func (m *Manager) controlAddr() string {
	m.mu.Lock()
	addr := m.ctrlAddr
	m.mu.Unlock()
	if addr != "" {
		return addr
	}
	return fmt.Sprintf("127.0.0.1:%d", ControlPort)
}

// hasNodeLocked 判断某个家宽节点名是否在当前订阅里。调用方需持有 m.mu。
func (m *Manager) hasNodeLocked(name string) bool {
	for _, n := range m.nodes {
		if n == name {
			return true
		}
	}
	return false
}

// persistedActiveNameLocked 把配置里记的「上次选的节点」还原成配置内使用的节点名。
//
// 普通模式与家宽模式存的字段不同：家宽存的是节点名本身（ClashNode），
// 普通节点存的是节点 ID（ActiveNode），得先换成生成配置时用的名字
// —— 那个名字可能带 " #N" 后缀（重名节点被改过名）。
//
// 调用方需持有 m.mu；本方法内部会取配置锁，符合「内核锁先于配置锁」的约定。
func (m *Manager) persistedActiveNameLocked() string {
	if m.cfg == nil {
		return ""
	}
	m.cfg.Lock()
	defer m.cfg.Unlock()
	if m.normalMode {
		return m.normalByID[m.cfg.ActiveNode].clash
	}
	return m.cfg.ClashNode
}

// ensureActiveNodeLocked 修正「上次选中的节点已消失」的情况。
//
// 订阅刷新后节点会变（家宽源换节点很频繁；普通节点的 ID 是按内容派生的，
// 源站一改整批失效）。不处理的话，启动后异步恢复那步会一直失败，界面还挂着
// 一个不存在的「当前使用中」；更糟的是盒子重启后代理起不来，用户直接断网。
// 这里改用列表里的第一个节点顶上，并把结果落盘，免得每次重启都重复一遍。
//
// 「从没选过」与「选过但没了」都会让 activeName 变成空串，但处理方式相反：
// 前者不动（订阅里的 fallback 组自己会挑，界面也不该凭空冒出一个「当前使用中」），
// 后者必须替补。所以判据不能只看 activeName —— 普通模式里节点 ID 找不到对应
// 名字时，映射结果本来就是空串，光看 activeName 会把「选过但没了」误判成
// 「从没选过」，自愈逻辑整个失效。得回头看一眼配置里到底记没记。
//
// 调用方需持有 m.mu，且 m.nodes 非空。
func (m *Manager) ensureActiveNodeLocked() {
	if len(m.nodes) == 0 {
		return
	}
	if m.activeName != "" && m.hasNodeLocked(m.activeName) {
		return // 选中的节点还在，别动用户的选择
	}
	selected := m.selectedInConfigLocked()
	if m.activeName == "" && selected == "" {
		return // 从没选过：不替用户做决定
	}
	old := m.activeName
	if old == "" {
		// 普通模式下 activeName 已经映射失败成了空串，日志里报节点 ID 才有线索
		old = selected
	}
	m.activeName = m.nodes[0]
	log.Printf("激活节点(%s)已不在节点列表中，自动改用[%s]", old, m.activeName)
	if m.cfg == nil {
		return
	}
	m.cfg.Lock()
	if m.normalMode {
		m.cfg.ActiveNode = m.normalByName[m.activeName]
	} else {
		m.cfg.ClashNode = m.activeName
	}
	_ = m.cfg.Save()
	m.cfg.Unlock()
}

// selectedInConfigLocked 读出配置里记的「上次选中的节点」。
// 普通模式存的是节点 ID（ActiveNode），家宽模式存的是节点名（ClashNode）。
//
// 调用方需持有 m.mu；本方法内部会取配置锁，符合「内核锁先于配置锁」的约定。
func (m *Manager) selectedInConfigLocked() string {
	if m.cfg == nil {
		return ""
	}
	m.cfg.Lock()
	defer m.cfg.Unlock()
	if m.normalMode {
		return m.cfg.ActiveNode
	}
	return m.cfg.ClashNode
}

// NodeGroup 返回家宽节点所属的手动选择组名（内核测速时用）
func (m *Manager) NodeGroup() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.nodeGroup
}

// proxyInfo /proxies 接口返回的单个代理条目
type proxyInfo struct {
	Type    string `json:"type"`
	Name    string `json:"name"`
	Alive   bool   `json:"alive"`
	History []struct {
		Time  string `json:"time"`
		Delay int    `json:"delay"`
	} `json:"history"`
}

// ProxyDelays 返回内核记录的各家宽节点最近一次延迟（毫秒）。
//
// 值的含义：
//   - 不在 map 里 —— 内核从没测过这个节点（界面显示为空白）
//   - 0           —— 测过但不通（界面显示为超时）
//   - 正数        —— 延迟毫秒数
//
// 这个调用不触发任何测速，只读缓存，所以可以放心在每次加载列表时调用。
func (m *Manager) ProxyDelays() map[string]int {
	m.mu.Lock()
	names := append([]string(nil), m.nodes...)
	m.mu.Unlock()
	return m.DelaysOf(names)
}

// DelaysOf 读内核记录里这些代理的最近延迟（只读缓存，不触发测速）。
func (m *Manager) DelaysOf(names []string) map[string]int {
	return ctrlProxyDelays(m.controlAddr(), nameSet(names))
}

// nameSet 把名字列表转成集合，给 ctrlProxyDelays 过滤用。
func nameSet(names []string) map[string]bool {
	out := make(map[string]bool, len(names))
	for _, n := range names {
		out[n] = true
	}
	return out
}

// ctrlProxyDelays 读内核记录的各代理最近一次延迟。不触发测速，只读缓存。
// 内核管理器与测速探针共用，区别只在控制口地址。
func ctrlProxyDelays(addr string, known map[string]bool) map[string]int {
	if len(known) == 0 {
		return nil
	}
	resp, err := stateClient.Get("http://" + addr + "/proxies")
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	var body struct {
		Proxies map[string]proxyInfo `json:"proxies"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil
	}

	out := map[string]int{}
	for name, p := range body.Proxies {
		if !known[name] || len(p.History) == 0 {
			continue
		}
		out[name] = p.History[len(p.History)-1].Delay
	}
	return out
}

// ProxyDelay 让内核测单个家宽节点的延迟，返回毫秒。
//
// 必须由内核测而不是本程序 TCP 探测：家宽节点的出口是一条 OpenVPN over
// Cloudflare 的隧道，探测它自己的 IP:端口完全反映不出隧道是否可用。
func (m *Manager) ProxyDelay(name string, timeoutMs int) (int, error) {
	return ctrlProxyDelay(m.controlAddr(), name, timeoutMs, m.TestURL(m.LoadedGroup()))
}

// ctrlProxyDelay 走 external-controller 测单个代理（节点或策略组）的延迟。
//
// name 传策略组名也成立 —— 但它走的是组的 fast()，在组里还没有延迟历史时
// 取的是成员列表里的第一个节点、且不检查它死活（见 ProxyNow 的说明）。
// 想知道「这个组现在实际在用哪个节点」，用 ProxyNow。
func ctrlProxyDelay(addr, name string, timeoutMs int, testURL string) (int, error) {
	if testURL == "" {
		testURL = defaultTestURL
	}
	u := fmt.Sprintf("http://%s/proxies/%s/delay?timeout=%d&url=%s",
		addr, url.PathEscape(name), timeoutMs, url.QueryEscape(testURL))
	resp, err := delayClient.Get(u)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	var r struct {
		Delay   int    `json:"delay"`
		Message string `json:"message"`
	}
	_ = json.Unmarshal(body, &r)
	if resp.StatusCode != http.StatusOK {
		// 测不通时内核返回 4xx/5xx + {"message":"..."}
		if r.Message != "" {
			return 0, fmt.Errorf("%s", r.Message)
		}
		return 0, fmt.Errorf("内核返回 %s", resp.Status)
	}
	if r.Delay <= 0 {
		return 0, fmt.Errorf("节点无响应")
	}
	return r.Delay, nil
}

// GroupNowTester 能报出某个策略组「此刻实际在用哪个节点」的后端。
//
// 单独一个接口而不是塞进 DelayTester：它只有诊断路径用得上，不是测速的必需能力。
type GroupNowTester interface {
	ProxyNow(name string) (string, error)
}

// DelayReader 能读出「内核记录的这些代理最近延迟」的后端（只读缓存，不触发测速）。
//
// 同样单独一个接口：只有「挑前置候选」用得上。
type DelayReader interface {
	DelaysOf(names []string) map[string]int
}

// ProxyNow 读内核里某个策略组当前选中的节点名。
//
// 为什么要专门读它，而不是直接拿组名去测延迟：`/proxies/{组}/delay` 走的是组的
// fast()，而 fast() 在组里还没有延迟历史时**无条件取成员列表的第一个节点**，
// 且不检查它是否存活。于是「测前置通道」实际变成了「测订阅里第一个 CF 节点」——
// 它恰好挂着就误报「前置不通」，另外两百多个明明好好的；订阅一刷新节点顺序变了，
// 同一个分组又时好时坏。先问内核「你现在用的是谁」再去测，结论才站得住。
func (m *Manager) ProxyNow(name string) (string, error) {
	return ctrlProxyNow(m.controlAddr(), name)
}

// ctrlProxyNow 走 external-controller 读某个代理/策略组的 now 字段。
func ctrlProxyNow(addr, name string) (string, error) {
	resp, err := stateClient.Get("http://" + addr + "/proxies/" + url.PathEscape(name))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("内核返回 %s", resp.Status)
	}
	var r struct {
		Now string `json:"now"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&r); err != nil {
		return "", err
	}
	if r.Now == "" {
		return "", fmt.Errorf("内核没报出[%s]当前用哪个节点", name)
	}
	return r.Now, nil
}

// GroupDelay 让内核并发测试整个策略组里所有节点的延迟。
//
// ⚠️ 整组并发测速会把组里所有节点一次性全打出去。家宽节点共用同一条前置
// 通道，节点一多就互相踩踏：大部分超时、少数挤过去的延迟巨大且随机，
// 每次结果都不一样。所以**家宽分组不要用这个方法**，走 sweepDelay（限并发）。
// 现在只留给「测前置通道本身」用。
func (m *Manager) GroupDelay(group string, timeoutMs int) (map[string]int, error) {
	m.mu.Lock()
	known := make(map[string]bool, len(m.nodes))
	for _, n := range m.nodes {
		known[n] = true
	}
	m.mu.Unlock()
	return ctrlGroupDelay(m.controlAddr(), group, timeoutMs, known, m.TestURL(m.LoadedGroup()))
}

// ctrlGroupDelay 走 external-controller 测一个策略组里所有节点的延迟。
// known 用于过滤掉不属于本分组的条目（组里万一混了别的节点，不要污染界面）；
// 传 nil 表示不过滤。
func ctrlGroupDelay(addr, group string, timeoutMs int, known map[string]bool, testURL string) (map[string]int, error) {
	if group == "" {
		return nil, fmt.Errorf("未识别到家宽节点选择组")
	}
	if testURL == "" {
		testURL = defaultTestURL
	}
	u := fmt.Sprintf("http://%s/group/%s/delay?timeout=%d&url=%s",
		addr, url.PathEscape(group), timeoutMs, url.QueryEscape(testURL))
	resp, err := groupDelayClient.Get(u)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("内核返回 %s", resp.Status)
	}

	var raw map[string]int
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, err
	}
	out := make(map[string]int, len(raw))
	for name, d := range raw {
		if known == nil || known[name] {
			out[name] = d
		}
	}
	return out, nil
}

// putProxy 通过 external-controller 切换某个策略组的当前选择。
// group 是组名（可能是中文或 emoji），必须做路径转义。
func (m *Manager) putProxy(group, name string) error {
	return ctrlPutProxy(m.controlAddr(), group, name)
}

// ctrlPutProxy 是 putProxy 的实现体。单独抽出来是为了让探针也能用 ——
// 探针是另一个 mihomo 进程，控制口地址不一样，但协议完全相同。
//
// 内核侧这个调用走 ForceSet：会把 url-test / fallback 这类自动组的自动选择
// 覆盖掉，直到下一次重启或显式清空。
func ctrlPutProxy(addr, group, name string) error {
	body, err := json.Marshal(map[string]string{"name": name})
	if err != nil {
		return err
	}
	u := fmt.Sprintf("http://%s/proxies/%s", addr, url.PathEscape(group))
	req, err := http.NewRequest(http.MethodPut, u, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := apiClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("内核返回 %s", resp.Status)
	}
	return nil
}

// waitControl 轮询等待控制接口就绪（mihomo 起来后还要加载 geo、建组，需要几秒）
func (m *Manager) waitControl(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := m.version(); err == nil {
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("等待内核控制接口就绪超时")
}

func (m *Manager) version() (string, error) {
	return ctrlVersion(m.controlAddr())
}

// ctrlVersion 读内核版本，用来判断控制接口是否已经就绪。
// 内核管理器与测速探针共用，区别只在控制口地址。
func ctrlVersion(addr string) (string, error) {
	resp, err := apiClient.Get("http://" + addr + "/version")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("内核返回 %s", resp.Status)
	}
	var v struct {
		Version string `json:"version"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		return "", err
	}
	return v.Version, nil
}

// --- 订阅解析（行级扫描，不引入 YAML 依赖）---

var (
	// 列表项起始：形如 `  - name: "xxx"`，捕获缩进与名称
	reItemName = regexp.MustCompile(`^(\s*)-\s+name:\s*(.+?)\s*$`)
	// 子键：形如 `    type: openvpn` / `    proxies:`
	reSubKey = regexp.MustCompile(`^\s+([A-Za-z0-9_-]+):\s*(.*?)\s*$`)
	// 纯列表项：形如 `      - "优选域名-01"`
	reListItem = regexp.MustCompile(`^\s+-\s+(.+?)\s*$`)
)

// topKey 识别顶层键（行首无缩进、非注释），返回键名。
func topKey(ln string) (string, bool) {
	if ln == "" || ln[0] == ' ' || ln[0] == '\t' || ln[0] == '#' {
		return "", false
	}
	i := strings.IndexByte(ln, ':')
	if i <= 0 {
		return "", false
	}
	k := ln[:i]
	for _, c := range k {
		ok := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') || c == '-' || c == '_'
		if !ok {
			return "", false
		}
	}
	return k, true
}

// unquote 去掉 YAML 字符串两端的引号
func unquote(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
			return s[1 : len(s)-1]
		}
	}
	return s
}

// subInfo 订阅解析结果
type subInfo struct {
	nodes     []string
	nodeGroup string
	topGroup  string
	// frontGroup 前置通道组名 —— openvpn 节点靠 dialer-proxy 指向它做链式转发。
	// 全部家宽节点的出口都挤在这一个组上，所以它不通时所有家宽节点都不通。
	frontGroup string
	// frontMembers 前置组的成员列表。换前置时要从里面挑候选。
	frontMembers []string
	// groupURLs 各策略组自己声明的测速地址（组名 → url）。
	// 测延迟时跟着订阅走，别自己写死 —— 见 TestURLOf。
	groupURLs map[string]string
	// groupOrder 组名按在订阅里出现的顺序，取 url 时用它保证结果稳定。
	groupOrder []string
}

// FrontGroupOf 从家宽订阅原文里取出前置通道组名。取不到返回空串。
func FrontGroupOf(subBody []byte) string {
	return parseSubscription(string(subBody)).frontGroup
}

// parseSubscription 从 Clash 订阅原文里提取家宽节点名与策略组结构。
//
// 只做行级扫描，不解析完整 YAML —— 原因见包注释。解析目标有四：
//  1. 所有 type: openvpn 的节点名（这些就是家宽节点）；
//  2. 成员全是家宽节点的 select 组 —— 手动切换节点就切它；
//  3. 成员里包含上面那个组的 select 组 —— 顶层主组，规则 MATCH 指向它；
//  4. 家宽节点 dialer-proxy 指向的组 —— 前置通道，测速前要先确认它通。
//
// 组名不硬编码，因为订阅结构随时可能变；识别不出来时返回空串，
// 调用方据此提示用户「重新拉取订阅」而不是静默地切错组。
func parseSubscription(src string) subInfo {
	var (
		info    subInfo
		section string

		pNames  []string // 节点名，按出现顺序
		pTypes  []string // 与 pNames 一一对应
		pDialer []string // 与 pNames 一一对应：dialer-proxy 指向的组名
		curP    = -1

		gNames      []string
		gTypes      []string
		gMembers    [][]string
		gURLs       []string
		curG        = -1
		groupIndent = -1
		inMembers   bool
	)

	for _, raw := range strings.Split(src, "\n") {
		if k, ok := topKey(raw); ok {
			// 切换顶层段，重置该段的状态
			section = k
			curP, curG, groupIndent, inMembers = -1, -1, -1, false
			continue
		}
		switch section {
		case "proxies":
			if m := reItemName.FindStringSubmatch(raw); m != nil {
				pNames = append(pNames, unquote(m[2]))
				pTypes = append(pTypes, "")
				pDialer = append(pDialer, "")
				curP = len(pNames) - 1
				continue
			}
			if curP >= 0 {
				if m := reSubKey.FindStringSubmatch(raw); m != nil {
					switch m[1] {
					case "type":
						pTypes[curP] = unquote(m[2])
					case "dialer-proxy":
						// type 与 dialer-proxy 的先后顺序在 YAML 里不固定，
						// 所以两个都先记下来，等扫完再配对（见函数尾部）。
						pDialer[curP] = unquote(m[2])
					}
				}
			}

		case "proxy-groups":
			// 组起始行与成员行的缩进不同：靠 groupIndent 区分。
			if m := reItemName.FindStringSubmatch(raw); m != nil {
				indent := len(m[1])
				if groupIndent < 0 || indent <= groupIndent {
					groupIndent = indent
					gNames = append(gNames, unquote(m[2]))
					gTypes = append(gTypes, "")
					gMembers = append(gMembers, nil)
					gURLs = append(gURLs, "")
					curG = len(gNames) - 1
					inMembers = false
					continue
				}
			}
			if curG < 0 {
				continue
			}
			if m := reSubKey.FindStringSubmatch(raw); m != nil {
				switch m[1] {
				case "proxies":
					inMembers = true
				case "type":
					gTypes[curG] = unquote(m[2])
					inMembers = false
				case "url":
					// 组自己声明的测速地址。测延迟时优先用它 —— 见 TestURLOf。
					gURLs[curG] = unquote(m[2])
					inMembers = false
				default:
					inMembers = false
				}
				continue
			}
			if inMembers {
				if m := reListItem.FindStringSubmatch(raw); m != nil {
					gMembers[curG] = append(gMembers[curG], unquote(m[1]))
				}
			}
		}
	}

	// 1) 家宽节点；顺带统计它们 dialer-proxy 指向的组
	isOVPN := map[string]bool{}
	dialerHits := map[string]int{}
	for i, n := range pNames {
		if strings.EqualFold(pTypes[i], "openvpn") {
			info.nodes = append(info.nodes, n)
			isOVPN[n] = true
			if i < len(pDialer) && pDialer[i] != "" {
				dialerHits[pDialer[i]]++
			}
		}
	}
	if len(info.nodes) == 0 {
		return info
	}

	// 1b) 前置通道：取家宽节点引用最多的那个 dialer-proxy 目标。
	//     理论上所有家宽节点指向同一个组，取最多只是为了订阅里混了
	//     个别直连节点时不至于取错。
	bestHit := 0
	for name, hits := range dialerHits {
		if hits > bestHit {
			info.frontGroup, bestHit = name, hits
		}
	}

	// 1c) 前置组的成员 —— 换前置时要从里面挑候选。
	if info.frontGroup != "" {
		for i, n := range gNames {
			if n == info.frontGroup {
				info.frontMembers = append([]string(nil), gMembers[i]...)
				break
			}
		}
	}

	// 2) 手动选择组：成员「全部是家宽节点」的 select 组。
	//    注意 fallback/url-test 组（如订阅里的「家宽自动」）不算 —— 它们是
	//    自动组，切了也会被下一次测速覆盖掉。取成员最多的那个。
	best := -1
	for i := range gNames {
		if !strings.EqualFold(gTypes[i], "select") {
			continue
		}
		all, any := true, false
		for _, mem := range gMembers[i] {
			if isOVPN[mem] {
				any = true
			} else {
				all = false
			}
		}
		if !any || !all {
			continue
		}
		if best < 0 || len(gMembers[i]) > len(gMembers[best]) {
			best = i
		}
	}
	if best >= 0 {
		info.nodeGroup = gNames[best]
	}

	// 3) 顶层主组：成员里包含 nodeGroup 的 select 组。
	//    规则通常是 `MATCH,<顶层组>`，所以它才真正决定流量去向。
	if info.nodeGroup != "" {
		for i := range gNames {
			if !strings.EqualFold(gTypes[i], "select") || gNames[i] == info.nodeGroup {
				continue
			}
			for _, mem := range gMembers[i] {
				if mem == info.nodeGroup {
					info.topGroup = gNames[i]
					break
				}
			}
			if info.topGroup != "" {
				break
			}
		}
	}

	// 4) 各组声明的测速地址，按出现顺序记下来。
	//    不提前 return 空 map —— TestURLOf 要能区分「组没写 url」与「压根没解析到」。
	info.groupURLs = make(map[string]string, len(gNames))
	for i, n := range gNames {
		info.groupOrder = append(info.groupOrder, n)
		if i < len(gURLs) && gURLs[i] != "" {
			info.groupURLs[n] = gURLs[i]
		}
	}
	return info
}

// --- 配置改写 ---

// confPorts 交给内核的运行配置里要写死的顶层键。
//
// 抽成结构体是因为现在有两处要生成配置，端口取法不同：
//   - 服务流量的家宽内核用 cfg 里的端口（与 xray 一致，用户端配置不用改）；
//   - 只用来测速的探针用一组备用端口（不能和正在服务流量的内核抢）。
type confPorts struct {
	socks, http, redir, ctrl int
	// allowLan 是否监听 0.0.0.0。服务流量的内核要开（透明代理/局域网共享），
	// 只用来测速的探针要关 —— 它的端口不该被局域网里任何设备误连上。
	allowLan bool
}

// rewriteConfig 把订阅原文改写成可以直接交给 mihomo 运行的配置。
//
// 只动顶层那几个简单键（都在文件头部），proxies / proxy-groups / rules
// 一个字节都不碰 —— openvpn 节点的 dialer-proxy 指向的是一个策略组，
// 一旦用 YAML 解析再重建，这种结构关系极易丢失，整条家宽链会静默失效。
//
// 端口取值与 xray 保持完全一致，这样切换内核时用户端配置（透明代理规则、
// 浏览器代理设置）都不用改。
func rewriteConfig(src string, p confPorts) (string, error) {
	type kv struct{ k, v string }
	want := []kv{
		{"port", strconv.Itoa(p.http)},                // HTTP 代理入口
		{"socks-port", strconv.Itoa(p.socks)},         // SOCKS 代理入口
		{"mixed-port", "0"},                           // 关掉订阅默认的混合端口，避免多监听一个口
		{"allow-lan", strconv.FormatBool(p.allowLan)}, // 透明代理/局域网共享需要
		{"redir-port", strconv.Itoa(p.redir)},         // 透明代理入口，与 transparent.sh 一致
		{"log-level", "warning"},                      // 盒子上少写日志
		{"external-controller", fmt.Sprintf("127.0.0.1:%d", p.ctrl)},
	}

	lines := strings.Split(src, "\n")
	hit := map[string]bool{}
	for i, ln := range lines {
		k, ok := topKey(ln)
		if !ok {
			continue
		}
		for _, w := range want {
			if k == w.k {
				lines[i] = w.k + ": " + w.v
				hit[w.k] = true
				break
			}
		}
	}

	// 订阅里没有的键（如 port / socks-port / redir-port）插到 proxies: 之前。
	// 顶层键顺序对 YAML 无意义，插在一起最省事也最不容易出错。
	var miss []string
	for _, w := range want {
		if !hit[w.k] {
			miss = append(miss, w.k+": "+w.v)
		}
	}
	if len(miss) > 0 {
		idx := -1
		for i, ln := range lines {
			if k, ok := topKey(ln); ok && k == "proxies" {
				idx = i
				break
			}
		}
		if idx < 0 {
			return "", fmt.Errorf("订阅内容异常：找不到顶层 proxies: 段")
		}
		tail := append([]string{}, lines[idx:]...)
		lines = append(lines[:idx], append(miss, tail...)...)
	}

	return strings.Join(lines, "\n"), nil
}
