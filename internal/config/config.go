package config

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
)

type Config struct {
	sync.Mutex
	WebPort    int      `json:"webPort"`
	SocksPort  int      `json:"socksPort"`
	HttpPort   int      `json:"httpPort"`
	ListenAddr string   `json:"listenAddr"`
	SubRefresh int      `json:"subRefresh"`
	SubProxy   string   `json:"subProxy"`
	ProxyMode  string   `json:"proxyMode"`
	ActiveNode string   `json:"activeNode"`
	SortOrder  string   `json:"sortOrder"`
	Subs       []Sub    `json:"subs"`
	Nodes      []Node   `json:"nodes"`
	path       string
}

type Sub struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	URL  string `json:"url"`
}

type Node struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	SubID       string `json:"subID"`
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
	RawLink     string `json:"rawLink"`
	LastSeen    string `json:"lastSeen"`
	Ping        int    `json:"ping"`
}

func Load(path string) (*Config, error) {
	c := &Config{
		WebPort: 8000, SocksPort: 10808, HttpPort: 10810,
		ListenAddr: "0.0.0.0", SubRefresh: 300,
		SubProxy: "", ProxyMode: "smart",
		Subs: []Sub{}, Nodes: []Node{},
	}
	c.path = path
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return c, nil
		}
		return nil, err
	}
	if err := json.Unmarshal(data, c); err != nil {
		// 配置文件损坏时用默认值启动，避免服务崩溃
		fmt.Printf("配置文件损坏(%v)，使用默认配置启动\n", err)
		*c = Config{
			WebPort: 8000, SocksPort: 10808, HttpPort: 10810,
			ListenAddr: "0.0.0.0", SubRefresh: 300,
			SubProxy: "", ProxyMode: "smart",
			Subs: []Sub{}, Nodes: []Node{}, path: path,
		}
		return c, nil
	}
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