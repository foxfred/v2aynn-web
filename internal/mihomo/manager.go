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

	// nodeCounts 各分组解析出的节点数。侧栏每个家宽分组都要显示节点数，
	// 但只有「当前生效」那个的节点真在内存里，所以这里单独记一份。
	nodeCounts map[string]int

	restartCount int
	lastRestart  time.Time
	restartMu    sync.Mutex

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

// CheckEnv 启动前检查内核与 geo 数据是否就位。
//
// 缺 geo 数据时 mihomo 会尝试联网从 GitHub 下载，墙内会卡满 90 秒超时，
// 期间代理端口完全不监听 —— 现象上就像「进程在跑但代理没工作」，是最难排查
// 的一种失败。所以宁可启动前硬检查，给一句明确的提示。
//
// geosite.dat 只在订阅规则真的引用了 GEOSITE 时才必需：没引用的话 mihomo
// 根本不会去加载它，缺了也不影响启动。cfnew 的默认订阅只用 GEOIP，所以
// 多数情况下只需要 geoip.metadb 一个文件。
func (m *Manager) CheckEnv() error {
	if _, err := os.Stat(m.bin); err != nil {
		return fmt.Errorf("找不到 mihomo 内核(%s)，请先把内核文件部署到该路径", m.bin)
	}
	if _, err := os.Stat(filepath.Join(m.dataDir, GeoIPMetaDB)); err != nil {
		return fmt.Errorf("缺少 GeoIP 库 %s（应放在 %s/）。缺这个文件时 mihomo 会联网下载并卡死 90 秒，期间代理端口不会监听",
			GeoIPMetaDB, m.dataDir)
	}
	if m.usesGeosite() {
		if _, err := os.Stat(filepath.Join(m.dataDir, GeoFile)); err != nil {
			return fmt.Errorf("订阅规则用到了 GEOSITE，但缺少 %s（应放在 %s/）", GeoFile, m.dataDir)
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
		return fmt.Errorf("家宽节点列表为空，请重新拉取订阅")
	}
	confPath := filepath.Join(m.dataDir, confFileName)
	if _, err := os.Stat(confPath); err != nil {
		return fmt.Errorf("家宽配置尚未生成，请先在左侧用「添加订阅分组」添加家宽订阅")
	}

	// 恢复上次选中的节点名（内存态，进程重启后靠 config 里的 ClashNode 还原）
	if m.cfg != nil {
		m.cfg.Lock()
		m.activeName = m.cfg.ClashNode
		m.cfg.Unlock()
	}
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
	want := m.activeName
	if want != "" {
		go func() {
			if err := m.waitControl(25 * time.Second); err != nil {
				log.Printf("mihomo 控制接口未就绪，跳过节点恢复: %v", err)
				return
			}
			if err := m.applyNode(want); err != nil {
				log.Printf("恢复家宽节点[%s]失败: %v", want, err)
				return
			}
			log.Printf("已恢复家宽节点[%s]", want)
		}()
	}

	go m.watch(m.cmd)
	log.Printf("mihomo 已启动 (节点 %d 个)", len(m.nodes))
	return nil
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
func (m *Manager) Status() map[string]interface{} {
	m.mu.Lock()
	defer m.mu.Unlock()
	activeNode := ""
	if m.activeName != "" {
		activeNode = config.ClashNodeID(m.loadedGroup, m.activeName)
	}
	return map[string]interface{}{
		"running":     m.running,
		"kernel":      config.KernelMihomo,
		"activeNode":  activeNode,
		"activeName":  m.activeName,
		"loadedGroup": m.loadedGroup,
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

// FrontGroup 家宽订阅里 openvpn 节点链式转发所依赖的前置组名。
// 取不到时返回空串（订阅结构变了，或这个分组还没拉过订阅）。
func (m *Manager) FrontGroup(groupID string) string {
	body, err := readSubFile(m.dataDir, groupID)
	if err != nil {
		return ""
	}
	return FrontGroupOf(body)
}

// UnloadGroup 删除某个家宽分组留下的文件。
//
// 如果删掉的正是当前生效的那份，运行配置会一起清掉，并且必须让内核停下来 ——
// 否则 mihomo 还在跑一份已经不存在的分组的配置，用户会一头雾水。
func (m *Manager) UnloadGroup(groupID string) {
	_ = os.Remove(filepath.Join(m.dataDir, subFileNameFor(groupID)))

	m.mu.Lock()
	wasActive := m.loadedGroup == groupID
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

// ensureActiveConfig 确保「当前应当生效的家宽分组」的配置已就绪。
func (m *Manager) ensureActiveConfig() error {
	groupID, subURL, proxy := m.activeGroupInfo()
	if groupID == "" {
		return fmt.Errorf("还没有家宽订阅分组，请在左侧用「添加订阅分组」添加")
	}
	return m.LoadGroup(groupID, subURL, proxy)
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

// delayTestURL 测延迟的目标地址。
// 与订阅里各策略组用的 url 保持一致（gstatic 204），
// 这样界面显示的延迟和内核自动选择节点的依据是同一个。
const delayTestURL = "http://www.gstatic.com/generate_204"

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

// ensureActiveNodeLocked 修正「上次选中的家宽节点已消失」的情况。
//
// 家宽订阅源换节点很频繁，上次选的节点下次刷新就没了。不处理的话，
// 启动后异步恢复那步会一直失败，界面还挂着一个不存在的「当前使用中」。
// 这里改用订阅里的第一个节点顶上，并把结果落盘，免得每次重启都重复一遍。
//
// 调用方需持有 m.mu，且 m.nodes 非空。
func (m *Manager) ensureActiveNodeLocked() {
	if m.activeName == "" || m.hasNodeLocked(m.activeName) {
		return
	}
	old := m.activeName
	m.activeName = m.nodes[0]
	log.Printf("上次选中的家宽节点[%s]已不在订阅中，改用[%s]", old, m.activeName)
	if m.cfg == nil {
		return
	}
	m.cfg.Lock()
	m.cfg.ClashNode = m.activeName
	_ = m.cfg.Save()
	m.cfg.Unlock()
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
	known := make(map[string]bool, len(m.nodes))
	for _, n := range m.nodes {
		known[n] = true
	}
	m.mu.Unlock()
	return ctrlProxyDelays(m.controlAddr(), known)
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
	return ctrlProxyDelay(m.controlAddr(), name, timeoutMs)
}

// ctrlProxyDelay 走 external-controller 测单个代理（节点或策略组）的延迟。
//
// name 传策略组名也成立 —— 测前置通道时用的就是这条路。
func ctrlProxyDelay(addr, name string, timeoutMs int) (int, error) {
	u := fmt.Sprintf("http://%s/proxies/%s/delay?timeout=%d&url=%s",
		addr, url.PathEscape(name), timeoutMs, url.QueryEscape(delayTestURL))
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
	return ctrlGroupDelay(m.controlAddr(), group, timeoutMs, known)
}

// ctrlGroupDelay 走 external-controller 测一个策略组里所有节点的延迟。
// known 用于过滤掉不属于本分组的条目（组里万一混了别的节点，不要污染界面）；
// 传 nil 表示不过滤。
func ctrlGroupDelay(addr, group string, timeoutMs int, known map[string]bool) (map[string]int, error) {
	if group == "" {
		return nil, fmt.Errorf("未识别到家宽节点选择组")
	}
	u := fmt.Sprintf("http://%s/group/%s/delay?timeout=%d&url=%s",
		addr, url.PathEscape(group), timeoutMs, url.QueryEscape(delayTestURL))
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
	body, err := json.Marshal(map[string]string{"name": name})
	if err != nil {
		return err
	}
	u := fmt.Sprintf("http://%s/proxies/%s", m.controlAddr(), url.PathEscape(group))
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
