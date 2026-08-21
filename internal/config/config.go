package config

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
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
	ID       string `json:"id"`
	SubID    string `json:"subID"`
	Name     string `json:"name"`
	Protocol string `json:"protocol"`
	Server   string `json:"server"`
	Port     string `json:"port"`
	UUID     string `json:"uuid"`
	Password string `json:"password"`
	Method   string `json:"method"`
	Network  string `json:"network"`
	TLS      string `json:"tls"`
	SNI      string `json:"sni"`
	Path     string `json:"path"`
	RequestHost string `json:"reqHost"`
	HeaderType  string `json:"headerType"`
	Security  string `json:"security"`
	AlterID  string `json:"alterId"`
	RawLink  string `json:"rawLink"`
	LastSeen string `json:"lastSeen"`
	Ping     int     `json:"ping"`
	Speed    float64 `json:"speed"`
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
}

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
	Ping        int     `json:"ping"`     // TCP/TLS握手延迟(ms)，-1=超时，0=未测
	Speed       float64 `json:"speed"`    // 真实下载速度 Mbps，0=未测，-1=超时/不可测
}

// Config 主配置
type Config struct {
	sync.Mutex
	WebPort    int     `json:"webPort"`
	SocksPort  int     `json:"socksPort"`
	HttpPort   int     `json:"httpPort"`
	ListenAddr string  `json:"listenAddr"`
	SubRefresh int     `json:"subRefresh"`
	SubProxy   string  `json:"subProxy"`  // 全局订阅代理
	ProxyMode  string  `json:"proxyMode"`
	ActiveNode string  `json:"activeNode"` // 当前激活节点ID
	ActiveGrp  string  `json:"activeGrp"`  // 当前激活节点所在分组ID
	SortOrder  string  `json:"sortOrder"`
	SpeedURL   string  `json:"speedURL"`
	Groups     []Group `json:"groups"`     // 分组列表，替代原来的 Subs + Nodes
	path       string
}

// 默认分组ID（手动节点/导入节点存放于此）
const DefaultGroupID = "__default__"

// FindGroup 按ID查找分组
func (c *Config) FindGroup(id string) *Group {
	for i := range c.Groups {
		if c.Groups[i].ID == id {
			return &c.Groups[i]
		}
	}
	return nil
}

// AllNodes 返回所有分组的全部节点（扁平化）
func (c *Config) AllNodes() []Node {
	var all []Node
	for _, g := range c.Groups {
		all = append(all, g.Nodes...)
	}
	return all
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
	return c, nil
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