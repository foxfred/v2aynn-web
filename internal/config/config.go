package config

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

// 旧格式字段（仅用于迁移，不持久化）
type legacyConfig struct {
	Subs  []legacySub  `json:"subs"`
	Nodes []legacyNode `json:"nodes"`
}

type legacySub struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	URL  string `json:"url"`
}

type legacyNode struct {
	ID          string  `json:"id"`
	SubID       string  `json:"subID"`
	Name        string  `json:"name"`
	Protocol    string  `json:"protocol"`
	Server      string  `json:"server"`
	Port        string  `json:"port"`
	UUID        string  `json:"uuid"`
	Password    string  `json:"password"`
	Method      string  `json:"method"`
	Network     string  `json:"network"`
	TLS         string  `json:"tls"`
	SNI         string  `json:"sni"`
	Path        string  `json:"path"`
	RequestHost string  `json:"reqHost"`
	HeaderType  string  `json:"headerType"`
	Security    string  `json:"security"`
	AlterID     string  `json:"alterId"`
	RawLink     string  `json:"rawLink"`
	LastSeen    string  `json:"lastSeen"`
	Ping        int     `json:"ping"`
	Speed       float64 `json:"speed"`
}

// --- 分组模型（参考 v2rayN 的 Group + ProfileItem 设计）---

// Group 订阅分组，包含该订阅源下的所有节点
// 每个订阅源对应一个 Group，手动节点放在 SubID="" 的默认分组中
type Group struct {
	ID        string `json:"id"`        // 分组ID，订阅源的ID与之相同
	Name      string `json:"name"`      // 分组显示名称
	URL       string `json:"url"`       // 订阅URL，空=手动节点分组
	SubProxy  string `json:"subProxy"`  // 该分组的订阅代理（可选覆盖全局）
	LastFetch string `json:"lastFetch"` // 上次拉取时间
	Nodes     []Node `json:"nodes"`     // 该分组下的节点列表
	// Kind 分组类型，见 GroupKind* 常量。
	//
	// 家宽订阅（Clash 格式）与普通订阅（vless/vmess 链接列表）是两种东西：
	// 前者是一份完整配置（节点 + 策略组 + 分流规则），必须整个交给 mihomo 内核；
	// 后者只是一串节点，解析后并入 xray 的节点池。
	// 所以家宽分组的 Nodes 恒为空 —— 它的节点由 mihomo 管理，不放进这里，
	// 免得混进 AllNodes() 被 xray 的故障转移当成候选节点。
	Kind string `json:"kind,omitempty"`

	// FrontNode 前置通道组被固定到哪个节点。
	//
	// 前置通道是家宽链的第一跳：全部家宽节点的出口都挤在它的同一个节点上。
	// 它在订阅里是 url-test，内核按「它自己访问 gstatic 快不快」自动选 ——
	// 而这个指标与「能不能承载一条 OpenVPN 长连接」毫无关系（盒子实测：
	// 自己 171ms 的带不动家宽链，197ms 的反而能），并且订阅没写 lazy、
	// mihomo 默认 lazy=true，前置组没有直接流量就不再复查，选错了永远不纠正。
	//
	// 所以测速时我们会拿真实的家宽节点试出「哪个前置能跑通」，记在这里，
	// 内核每次启动时重新固定一遍。空串表示还没测出过可用的前置，按订阅的
	// 自动选择走。
	FrontNode string `json:"frontNode,omitempty"`

	// Probes 家宽节点的测速结果，按节点名索引。只对家宽分组有意义。
	//
	// 为什么要单独一张表：家宽分组的 Nodes 恒为空，Node.Ping / Node.Speed
	// 没有地方安放。没有它的时候，「真实测速」测出来的数字只能写回
	// cfg.ActiveNode —— 而那个字段在家宽模式下还停在上一个普通节点上，
	// 于是家宽测出来的速度被记到了普通节点头上，家宽节点自己永远显示不出速度。
	Probes map[string]ProbeInfo `json:"probes,omitempty"`
}

// ProbeInfo 一个家宽节点的测速结果。
type ProbeInfo struct {
	// MS 延迟毫秒。-1 表示测过但不通；0 表示从没测过（界面留空）。
	MS int `json:"ms"`
	// MBPS 真实测速的下载速度（Mbps）。0 表示没测过。
	MBPS float64 `json:"mbps,omitempty"`
	// TS 最后一次测速时间（Unix 秒）。0 表示没测过。
	TS int64 `json:"ts,omitempty"`
}

// SetProbeMS 记下一个家宽节点的延迟。ms 传 -1 表示测过但不通。
//
// 注意必须「取出副本 → 改 → 写回」：map 里的 struct 是不可寻址的，
// 直接 p.MS = ms 改的是副本，写不回去。
func (g *Group) SetProbeMS(name string, ms int) {
	if name == "" {
		return
	}
	if g.Probes == nil {
		g.Probes = map[string]ProbeInfo{}
	}
	p := g.Probes[name]
	p.MS = ms
	p.TS = time.Now().Unix()
	g.Probes[name] = p
}

// SetProbeSpeed 记下一个家宽节点的真实下载速度（Mbps）。
func (g *Group) SetProbeSpeed(name string, mbps float64) {
	if name == "" || mbps <= 0 {
		return
	}
	if g.Probes == nil {
		g.Probes = map[string]ProbeInfo{}
	}
	p := g.Probes[name]
	p.MBPS = mbps
	p.TS = time.Now().Unix()
	g.Probes[name] = p
}

// Probe 取某个家宽节点的测速结果，第二个返回值为 false 表示没测过。
func (g *Group) Probe(name string) (ProbeInfo, bool) {
	p, ok := g.Probes[name]
	return p, ok
}

// 分组类型。空串等价 GroupKindNormal，保持老配置向后兼容。
const (
	GroupKindNormal = ""      // 普通订阅：解析出 xray 节点
	GroupKindClash  = "clash" // 家宽订阅：整份 Clash 配置交给 mihomo 内核
)

// IsClash 该分组是否为家宽订阅
func (g *Group) IsClash() bool { return g.Kind == GroupKindClash }

// Node 代理节点
type Node struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	Protocol    string  `json:"protocol"`
	Server      string  `json:"server"`
	Port        string  `json:"port"`
	UUID        string  `json:"uuid"`
	Password    string  `json:"password"`
	Method      string  `json:"method"`
	Network     string  `json:"network"`
	TLS         string  `json:"tls"`
	SNI         string  `json:"sni"`
	Path        string  `json:"path"`
	RequestHost string  `json:"reqHost"`
	HeaderType  string  `json:"headerType"`
	Security    string  `json:"security"`
	AlterID     string  `json:"alterId"`
	RawLink     string  `json:"rawLink"`
	LastSeen    string  `json:"lastSeen"`
	Ping        int     `json:"ping"`  // TCP/TLS握手延迟(ms)，-1=超时，0=未测
	Speed       float64 `json:"speed"` // 真实下载速度 Mbps，0=未测，-1=超时/不可测
}

// Config 主配置
type Config struct {
	sync.Mutex
	WebPort    int    `json:"webPort"`
	SocksPort  int    `json:"socksPort"`
	HttpPort   int    `json:"httpPort"`
	ListenAddr string `json:"listenAddr"`
	SubRefresh int    `json:"subRefresh"`
	SubProxy   string `json:"subProxy"` // 全局订阅代理
	ProxyMode  string `json:"proxyMode"`
	ActiveNode string `json:"activeNode"` // 当前激活节点ID
	ActiveGrp  string `json:"activeGrp"`  // 当前激活节点所在分组ID
	SortOrder  string `json:"sortOrder"`
	SpeedURL   string `json:"speedURL"`
	// AutoFailover 节点连续失败时是否自动切换到其他可用节点。
	// 用指针区分"未设置"与"显式关闭"：nil 表示默认开启。
	AutoFailover *bool   `json:"autoFailover,omitempty"`
	Groups       []Group `json:"groups"` // 分组列表，替代原来的 Subs + Nodes

	// --- 家宽（Clash / mihomo 内核）相关字段 ---
	//
	// 背景：cfnew 的「家宽链式」订阅是 Clash 格式，节点类型为 openvpn，
	// 并靠 mihomo 的 dialer-proxy 做链式转发。xray 内核既没有 openvpn 出站、
	// 也没有 dialer-proxy 的等价能力，所以这部分必须交给 mihomo 内核跑。
	//
	//   ClashSubURL —— 【已废弃，仅用于读取老配置】早期家宽地址只能填在设置面板里，
	//                  现在改成普通订阅分组（Group.Kind = clash）承载。
	//                  Load() 会把老值迁移成分组，之后这个字段恒为空。
	//   Kernel      —— 当前实际在跑的内核。"xray"（空值等价）或 "mihomo"。
	//                  两个内核监听同样的端口，因此同一时刻只能跑一个。
	//   ClashNode   —— 家宽模式下当前选中的节点名（配合 ActiveGrp 定位到具体分组）。
	ClashSubURL string `json:"clashSubUrl,omitempty"`
	Kernel      string `json:"kernel,omitempty"`
	ClashNode   string `json:"clashNode,omitempty"`
	path        string
	dirty       bool // 存在未落盘的高频改动（如测速结果），由后台 Flush 合并写入
}

// 默认分组ID（手动节点/导入节点存放于此）
const DefaultGroupID = "__default__"

// AllNodes 返回所有分组的全部节点（扁平化）
func (c *Config) AllNodes() []Node {
	var all []Node
	for _, g := range c.Groups {
		all = append(all, g.Nodes...)
	}
	return all
}

// NormalNodes 返回所有**普通**分组的节点，家宽分组一律跳过。
//
// 内核统一后普通节点也要交给 mihomo，而 mihomo 一次只加载一份配置，
// 所以生成配置时必须把全部普通分组的节点合在一起 —— 与旧版 xray 的行为一致
// （xray 那套的 AllNodes 也是全量合并，只是那时家宽节点不存在 Node 结构）。
//
// 家宽分组的 Nodes 恒为空，跳过只是把意图写明，避免以后有人给家宽分组塞节点。
// 调用方需持有 c 的锁。
func (c *Config) NormalNodes() []Node {
	var all []Node
	for _, g := range c.Groups {
		if g.IsClash() {
			continue
		}
		all = append(all, g.Nodes...)
	}
	return all
}

// FailoverEnabled 自动故障转移是否开启（未显式设置时默认开启）
func (c *Config) FailoverEnabled() bool {
	return c.AutoFailover == nil || *c.AutoFailover
}

// FindNode 按ID在所有分组中查找节点
func (c *Config) FindNode(id string) (*Node, string) {
	for gi := range c.Groups {
		for ni := range c.Groups[gi].Nodes {
			if c.Groups[gi].Nodes[ni].ID == id {
				return &c.Groups[gi].Nodes[ni], c.Groups[gi].ID
			}
		}
	}
	return nil, ""
}

// RemoveNode 从所有分组中移除指定ID的节点
func (c *Config) RemoveNode(id string) {
	for gi := range c.Groups {
		nodes := c.Groups[gi].Nodes
		for ni := range nodes {
			if nodes[ni].ID == id {
				c.Groups[gi].Nodes = append(nodes[:ni], nodes[ni+1:]...)
				return
			}
		}
	}
}

// EnsureDefaultGroup 确保默认分组存在（手动节点使用）
func (c *Config) EnsureDefaultGroup() {
	for _, g := range c.Groups {
		if g.ID == DefaultGroupID {
			return
		}
	}
	c.Groups = append([]Group{{
		ID:    DefaultGroupID,
		Name:  "手动节点",
		Nodes: []Node{},
	}}, c.Groups...)
}

// legacyToNode 旧格式节点转为新格式节点
func legacyToNode(n legacyNode) Node {
	return Node{
		ID: n.ID, Name: n.Name,
		Protocol: n.Protocol, Server: n.Server, Port: n.Port,
		UUID: n.UUID, Password: n.Password, Method: n.Method,
		Network: n.Network, TLS: n.TLS, SNI: n.SNI,
		Path: n.Path, RequestHost: n.RequestHost,
		HeaderType: n.HeaderType, Security: n.Security, AlterID: n.AlterID,
		RawLink: n.RawLink, LastSeen: n.LastSeen,
		Ping: n.Ping, Speed: n.Speed,
	}
}

// --- 加载/保存 ---

func Load(path string) (*Config, error) {
	c := &Config{
		WebPort: 8000, SocksPort: 10808, HttpPort: 10810,
		ListenAddr: "0.0.0.0", SubRefresh: 300,
		SubProxy: "", ProxyMode: "smart",
		SpeedURL: "https://speed.cloudflare.com/__down?bytes=2000000",
		Groups:   []Group{},
	}
	c.path = path
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			c.EnsureDefaultGroup()
			return c, nil
		}
		return nil, err
	}
	if err := json.Unmarshal(data, c); err != nil {
		fmt.Printf("配置文件损坏(%v)，使用默认配置启动\n", err)
		*c = Config{
			WebPort: 8000, SocksPort: 10808, HttpPort: 10810,
			ListenAddr: "0.0.0.0", SubRefresh: 300,
			SubProxy: "", ProxyMode: "smart",
			SpeedURL: "https://speed.cloudflare.com/__down?bytes=2000000",
			Groups:   []Group{}, path: path,
		}
		c.EnsureDefaultGroup()
		return c, nil
	}

	// 旧格式迁移：如果 Groups 为空但旧字段有数据，做迁移
	if len(c.Groups) == 0 {
		var old legacyConfig
		if err := json.Unmarshal(data, &old); err == nil {
			if len(old.Subs) > 0 || len(old.Nodes) > 0 {
				fmt.Printf("检测到旧配置格式，迁移到分组结构...\n")
				// 每个订阅转为独立分组
				for _, s := range old.Subs {
					g := Group{ID: s.ID, Name: s.Name, URL: s.URL, Nodes: []Node{}}
					var grpNodes []Node
					for _, n := range old.Nodes {
						if n.SubID == s.ID {
							grpNodes = append(grpNodes, legacyToNode(n))
						}
					}
					g.Nodes = grpNodes
					c.Groups = append(c.Groups, g)
				}
				// 手动节点（SubID为空）放入默认分组
				var manualNodes []Node
				for _, n := range old.Nodes {
					if n.SubID == "" {
						manualNodes = append(manualNodes, legacyToNode(n))
					}
				}
				if len(manualNodes) > 0 {
					c.Groups = append([]Group{{
						ID:    DefaultGroupID,
						Name:  "手动节点",
						Nodes: manualNodes,
					}}, c.Groups...)
				}
				_ = c.Save()
				fmt.Printf("迁移完成: %d个分组, %d个节点\n", len(c.Groups), len(c.AllNodes()))
			}
		}
	}

	c.EnsureDefaultGroup()
	// 家宽地址迁移：早期家宽订阅只能填在设置面板里（ClashSubURL），
	// 现在改成普通订阅分组承载。老配置在第一次加载时自动转过去。
	if c.ClashSubURL != "" {
		c.MigrateClashSubURL()
	}
	return c, nil
}

// MigrateClashSubURL 把废弃的 ClashSubURL 转成家宽分组，并清空该字段。
//
// 为什么要单独做迁移：用户已经填过的地址不能让他重填一遍。
// 如果他还手动建过一个同地址的普通分组（很常见 —— 填进设置面板之前
// 都会先在「添加订阅分组」里试一下），就把那个分组直接转正，
// 免得侧栏里出现两个地址一模一样的条目。
//
// 调用方需持有 c 的锁。
func (c *Config) MigrateClashSubURL() {
	url := c.ClashSubURL
	c.ClashSubURL = ""
	if url == "" {
		return
	}
	for i := range c.Groups {
		if c.Groups[i].URL != url {
			continue
		}
		// 同地址的已有分组直接转正。清掉 Nodes：家宽分组的节点由 mihomo 管，
		// 之前误解析出来的空列表留着只会让人困惑。
		c.Groups[i].Kind = GroupKindClash
		c.Groups[i].Nodes = nil
		fmt.Printf("家宽订阅已迁移到已有分组[%s]\n", c.Groups[i].Name)
		_ = c.Save()
		return
	}
	c.Groups = append(c.Groups, Group{
		ID:   fmt.Sprintf("%d", time.Now().UnixNano()),
		Name: "家宽", URL: url, Kind: GroupKindClash,
		Nodes: []Node{},
	})
	fmt.Printf("家宽订阅已迁移为新分组[家宽]\n")
	_ = c.Save()
}

func (c *Config) Save() error {
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		fmt.Printf("Save: json序列化失败: %v\n", err)
		return err
	}
	if c.path == "" {
		err = fmt.Errorf("Save: 路径为空")
		fmt.Printf("%v\n", err)
		return err
	}
	if err := os.WriteFile(c.path, b, 0644); err != nil {
		fmt.Printf("Save: 写入失败(%s): %v\n", c.path, err)
		return err
	}
	fmt.Printf("Save: 已保存到 %s (%d bytes)\n", c.path, len(b))
	return nil
}

// MarkDirty 标记存在待落盘改动（仅标脏，不写盘）
// 用于测速结果这类高频、可丢失的写入，避免每测一个节点就全量写一次磁盘。
// 调用方需持有 c 的锁。
func (c *Config) MarkDirty() {
	c.dirty = true
}

// Flush 若存在待落盘改动则写盘，返回是否真正写入
// 调用方需持有 c 的锁。
func (c *Config) Flush() bool {
	if !c.dirty {
		return false
	}
	c.dirty = false
	_ = c.Save()
	return true
}

// Restore 用外部配置替换当前内存配置（配置文件路径保持不变）
// 逐字段拷贝，不复制内嵌的 sync.Mutex；缺失字段回落到默认值。
// 调用方需持有 c 的锁。
func (c *Config) Restore(other *Config) {
	pick := func(v, def int) int {
		if v > 0 {
			return v
		}
		return def
	}
	pickS := func(v, def string) string {
		if v != "" {
			return v
		}
		return def
	}

	c.WebPort = pick(other.WebPort, 8000)
	c.SocksPort = pick(other.SocksPort, 10808)
	c.HttpPort = pick(other.HttpPort, 10810)
	c.ListenAddr = pickS(other.ListenAddr, "0.0.0.0")
	// subRefresh 不能套用 pick：0 是合法值（表示禁用自动刷新），必须原样保留
	c.SubRefresh = other.SubRefresh
	c.SubProxy = other.SubProxy
	c.ProxyMode = pickS(other.ProxyMode, "smart")
	c.ActiveNode = other.ActiveNode
	c.ActiveGrp = other.ActiveGrp
	c.SortOrder = other.SortOrder
	c.SpeedURL = pickS(other.SpeedURL, "https://speed.cloudflare.com/__down?bytes=2000000")
	// AutoFailover 是 *bool：nil 表示"未设置"（默认开启），非 nil 表示用户显式选择。
	// 必须整体拷贝，否则导入一份"已关闭故障转移"的备份会被静默改回默认开启。
	c.AutoFailover = other.AutoFailover
	c.Groups = other.Groups

	// 家宽字段同样整体拷贝：Kernel / ClashNode 为空都是合法状态，
	// 不能套用 pickS 之类的"空值回落默认"，否则导入一份不含家宽的备份
	// 会被静默改回某个默认值。
	c.ClashSubURL = other.ClashSubURL
	c.Kernel = other.Kernel
	c.ClashNode = other.ClashNode

	c.EnsureDefaultGroup()
	// 导入的可能是旧版备份（家宽地址还在 ClashSubURL 里），一并迁移
	if c.ClashSubURL != "" {
		c.MigrateClashSubURL()
	}
}

// --- 家宽相关辅助 ---

// ClashEnabled 是否配置了家宽订阅（存在任意一个家宽分组）。
// 调用方需持有 c 的锁。
func (c *Config) ClashEnabled() bool {
	return len(c.ClashGroups()) > 0
}

// ClashGroups 返回所有家宽分组（副本，调用方可安全持有）。
// 调用方需持有 c 的锁。
func (c *Config) ClashGroups() []Group {
	var out []Group
	for _, g := range c.Groups {
		if g.IsClash() {
			out = append(out, g)
		}
	}
	return out
}

// FindClashGroup 按 ID 找家宽分组，找不到返回 nil。
// 返回的是切片内元素的指针，调用方需持有 c 的锁。
func (c *Config) FindClashGroup(id string) *Group {
	for i := range c.Groups {
		if c.Groups[i].ID == id && c.Groups[i].IsClash() {
			return &c.Groups[i]
		}
	}
	return nil
}

// ActiveClashGroup 返回当前应当生效的家宽分组。
//
// 优先用 ActiveGrp 定位 —— 用户点了哪个分组里的节点，就该用哪份配置；
// 该分组已被删除或还没选过时，回落到第一个家宽分组。
// 一个都没有时返回 nil。调用方需持有 c 的锁。
func (c *Config) ActiveClashGroup() *Group {
	if g := c.FindClashGroup(c.ActiveGrp); g != nil {
		return g
	}
	for i := range c.Groups {
		if c.Groups[i].IsClash() {
			return &c.Groups[i]
		}
	}
	return nil
}

// ActiveKind 返回当前生效的**节点类型**：KindNormal（普通节点）或 KindClash（家宽节点）。
//
// 名字里的「Kernel」是历史遗留。早期两个内核按节点类型分派（普通走 xray、家宽走
// mihomo），所以这个字段等价于「哪个内核在跑」。内核统一之后**永远只有 mihomo 在跑**，
// 该字段不再表示「哪个内核」，而是「现在加载的是哪一份配置」——
// 普通节点配置还是某个家宽分组的订阅原文。
//
// 之所以保留 Kernel 字段名与 "xray"/"mihomo" 这两个取值：config.json 里存的就是它们，
// 改名会让老配置读不出来，而这两个值现在只当作「普通 / 家宽」的标签用。
//
// 空值与任何未知取值都回落到普通节点（与旧版行为一致）。调用方需持有 c 的锁。
func (c *Config) ActiveKind() string {
	if c.Kernel == KindClash {
		return KindClash
	}
	return KindNormal
}

// 节点类型常量。
//
// 历史上这两个值对应两个内核，所以字面量仍是 "xray"/"mihomo"（config.json 兼容）。
// 内核统一后它们只表示「当前生效的节点类型」：
//
//	KindNormal —— 普通节点（trojan/vless/vmess/ss），由 BuildNormalConfig 生成配置
//	KindClash  —— 家宽节点，直接改写订阅原文
const (
	KindNormal = "xray"
	KindClash  = "mihomo"

	// 旧名，保留给既有调用方与测试，语义同 KindNormal / KindClash。
	KernelXray   = KindNormal
	KernelMihomo = KindClash
)

// ClashNodeIDPrefix 家宽节点在界面上的 ID 前缀。
//
// 家宽节点由 mihomo 管理，本程序没有它们的 Node 结构，但界面切换节点
// 走的是统一的 /api/node/{id} 接口。给家宽节点造一个
// "clash:<分组ID>:<节点名>" 形式的 ID，服务端按前缀分流即可，
// 前端无需为家宽单独写一套切换逻辑。
//
// 为什么要把分组 ID 编进 ID 里：家宽分组可以有好几个，而 mihomo 同一时刻
// 只加载其中一份配置。点节点时必须知道它属于哪个分组，才能先把对应配置加载好。
// 分组 ID 是纯数字（UnixNano），不含冒号，所以按第一个冒号切分即可还原。
const ClashNodeIDPrefix = "clash:"

// ClashNodeID 拼一个家宽节点的界面 ID
func ClashNodeID(groupID, name string) string {
	return ClashNodeIDPrefix + groupID + ":" + name
}

// ParseClashNodeID 拆解家宽节点 ID，返回分组 ID 与节点名。
// 不是家宽 ID 时 ok 为 false。
func ParseClashNodeID(id string) (groupID, name string, ok bool) {
	if len(id) <= len(ClashNodeIDPrefix) || id[:len(ClashNodeIDPrefix)] != ClashNodeIDPrefix {
		return "", "", false
	}
	rest := id[len(ClashNodeIDPrefix):]
	i := strings.IndexByte(rest, ':')
	// 分组 ID 不能为空。空 ID 会让调用方静默回落到「默认家宽分组」，
	// 于是用户点的是 B 分组的节点，实际加载的却是 A 分组的配置 ——
	// 界面显示与真实出口对不上，且没有任何报错可查。
	if i <= 0 {
		return "", "", false
	}
	return rest[:i], rest[i+1:], true
}
