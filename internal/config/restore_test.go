package config

import (
	"encoding/json"
	"reflect"
	"testing"
)

// TestRestoreCopiesEveryExportedField 用反射遍历 Config 的所有导出字段，
// 断言 Restore() 会把源配置里的非零值原样搬到目标配置。
//
// 存在意义：Restore 采用"手工逐字段拷贝"的写法，新增字段时极易漏拷，
// 而漏拷的表现是"恢复备份后某个设置悄悄变回默认值"——编译期无任何提示。
// 该测试能在新增字段而忘记同步 Restore 时立刻失败。
func TestRestoreCopiesEveryExportedField(t *testing.T) {
	off := false

	src := &Config{
		WebPort:      18099,
		SocksPort:    12080,
		HttpPort:     12081,
		ListenAddr:   "127.0.0.1",
		SubRefresh:   0, // 0 是合法值（禁用自动刷新），必须原样保留
		SubProxy:     "socks5://127.0.0.1:1080",
		ProxyMode:    "global",
		ActiveNode:   "node-a",
		ActiveGrp:    "grp-a",
		SortOrder:    "speed",
		SpeedURL:     "https://example.com/speed",
		AutoFailover: &off,
		Groups: []Group{
			{ID: DefaultGroupID, Name: "手动节点", Nodes: []Node{{ID: "node-a", Name: "A"}}},
		},
	}

	dst := &Config{}
	dst.Restore(src)

	sv := reflect.ValueOf(src).Elem()
	dv := reflect.ValueOf(dst).Elem()
	st := sv.Type()

	for i := 0; i < st.NumField(); i++ {
		f := st.Field(i)
		if !f.IsExported() {
			continue // path / dirty 等内部字段不应被拷贝
		}
		got := dv.Field(i)
		want := sv.Field(i)

		// *bool 语义：nil 表示"未设置"，非 nil 必须整体拷贝（含指向 false）
		if f.Type.Kind() == reflect.Ptr {
			if want.IsNil() {
				continue
			}
			if got.IsNil() {
				t.Errorf("字段 %s 未被 Restore 拷贝：源=%v 目标=nil", f.Name, want.Elem())
				continue
			}
			if !reflect.DeepEqual(got.Elem().Interface(), want.Elem().Interface()) {
				t.Errorf("字段 %s 拷贝后值不符：源=%v 目标=%v", f.Name, want.Elem(), got.Elem())
			}
			continue
		}

		if !reflect.DeepEqual(got.Interface(), want.Interface()) {
			t.Errorf("字段 %s 拷贝后值不符：源=%v 目标=%v", f.Name, want.Interface(), got.Interface())
		}
	}
}

// TestRestoreJSONRoundTrip 模拟真实备份/恢复路径：
// 结构体 -> JSON -> 结构体 -> Restore，验证全程无字段丢失。
func TestRestoreJSONRoundTrip(t *testing.T) {
	off := false
	src := &Config{
		WebPort: 18099, SocksPort: 12080, HttpPort: 12081,
		ListenAddr: "127.0.0.1", SubRefresh: 0,
		ProxyMode: "global", SpeedURL: "https://example.com/speed",
		AutoFailover: &off,
		Groups: []Group{
			{ID: DefaultGroupID, Name: "往返测试", Nodes: []Node{{ID: "n1", Name: "N1", Protocol: "trojan"}}},
		},
	}

	raw, err := json.Marshal(src)
	if err != nil {
		t.Fatalf("序列化备份失败: %v", err)
	}

	var parsed Config
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("解析备份失败: %v", err)
	}

	dst := &Config{}
	dst.Restore(&parsed)

	if dst.SubRefresh != 0 {
		t.Errorf("subRefresh 往返后应为 0（禁用），实际 %d", dst.SubRefresh)
	}
	if dst.AutoFailover == nil {
		t.Fatal("autoFailover=false 往返后丢失（变回 nil=默认开启）")
	}
	if *dst.AutoFailover != false {
		t.Errorf("autoFailover 往返后应为 false，实际 %v", *dst.AutoFailover)
	}
	if dst.SocksPort != 12080 || dst.HttpPort != 12081 {
		t.Errorf("端口往返丢失: socks=%d http=%d", dst.SocksPort, dst.HttpPort)
	}
	if len(dst.Groups) != 1 || dst.Groups[0].Name != "往返测试" {
		t.Errorf("分组往返丢失: %+v", dst.Groups)
	}
}

// TestRestoreFillsDefaultsForMissingFields 备份文件缺字段时应回落默认值，
// 而不是把目标配置写成 0/空串（会导致监听 0 端口等异常）。
func TestRestoreFillsDefaultsForMissingFields(t *testing.T) {
	dst := &Config{}
	dst.Restore(&Config{Groups: []Group{{ID: DefaultGroupID, Name: "空"}}})

	if dst.WebPort != 8000 {
		t.Errorf("缺失 webPort 应回落 8000，实际 %d", dst.WebPort)
	}
	if dst.SocksPort != 10808 {
		t.Errorf("缺失 socksPort 应回落 10808，实际 %d", dst.SocksPort)
	}
	if dst.HttpPort != 10810 {
		t.Errorf("缺失 httpPort 应回落 10810，实际 %d", dst.HttpPort)
	}
	if dst.ListenAddr != "0.0.0.0" {
		t.Errorf("缺失 listenAddr 应回落 0.0.0.0，实际 %q", dst.ListenAddr)
	}
	if dst.ProxyMode != "smart" {
		t.Errorf("缺失 proxyMode 应回落 smart，实际 %q", dst.ProxyMode)
	}
	if dst.SpeedURL == "" {
		t.Error("缺失 speedURL 应回落默认测速地址")
	}
}

// TestFailoverEnabledTriState 校验 *bool 三态语义：nil=默认开启，显式值优先。
func TestFailoverEnabledTriState(t *testing.T) {
	on, off := true, false

	cases := []struct {
		name string
		v    *bool
		want bool
	}{
		{"nil 默认开启", nil, true},
		{"显式开启", &on, true},
		{"显式关闭", &off, false},
	}
	for _, tc := range cases {
		c := &Config{AutoFailover: tc.v}
		if got := c.FailoverEnabled(); got != tc.want {
			t.Errorf("%s: FailoverEnabled()=%v, 期望 %v", tc.name, got, tc.want)
		}
	}
}

// TestEnsureDefaultGroupIdempotent 默认分组不能重复插入。
func TestEnsureDefaultGroupIdempotent(t *testing.T) {
	c := &Config{}
	c.EnsureDefaultGroup()
	c.EnsureDefaultGroup()
	c.EnsureDefaultGroup()

	n := 0
	for _, g := range c.Groups {
		if g.ID == DefaultGroupID {
			n++
		}
	}
	if n != 1 {
		t.Errorf("默认分组应恰好存在 1 个，实际 %d 个", n)
	}
}
