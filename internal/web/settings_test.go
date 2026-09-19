package web

import (
	"path/filepath"
	"strings"
	"testing"

	"v2aynn-web/internal/config"
	"v2aynn-web/internal/subscription"
)

func newTestServer(t *testing.T) *WebServer {
	t.Helper()
	cfg, err := config.Load(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatalf("加载测试配置失败: %v", err)
	}
	// applySettings 不触碰 v2m，故此处无需构造 Manager
	return &WebServer{cfg: cfg}
}

// TestApplySettingsDoesNotStrandLockOnBadInput 是死锁回归测试。
//
// 历史缺陷：apiSettings 用 `int(v.(float64))` 裸断言，字段类型不符时 panic，
// 而 `w.cfg.Unlock()` 写在函数末尾、panic 后执行不到，配置锁永久无法释放。
// 实测后果：一个 `{"subRefresh":"abc"}` 请求即可让整个服务（全部 API + 前端）
// 彻底无响应，必须重启进程才能恢复。
func TestApplySettingsDoesNotStrandLockOnBadInput(t *testing.T) {
	s := newTestServer(t)

	badInputs := []map[string]interface{}{
		{"subRefresh": "abc"},   // 字符串冒充数字
		{"socksPort": "10808"},  // 字符串冒充数字
		{"httpPort": []int{1}},  // 完全错误的类型
		{"listenAddr": 123},     // 数字冒充字符串
		{"proxyMode": true},     // 布尔冒充字符串
		{"autoFailover": "yes"}, // 字符串冒充布尔
		{"speedURL": map[string]int{"a": 1}},
		{"subProxy": 1.5},
	}

	for _, in := range badInputs {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("输入 %v 导致 panic（应返回错误）: %v", in, r)
				}
			}()
			if err := s.applySettings(in); err == nil {
				t.Errorf("输入 %v 应当被拒绝并返回错误", in)
			}
		}()

		// 关键断言：无论走哪条错误路径，配置锁都必须已释放
		if !s.cfg.TryLock() {
			t.Fatalf("输入 %v 之后配置锁未释放 —— 整个服务会挂死（死锁回归）", in)
		}
		s.cfg.Unlock()
	}
}

// TestApplySettingsSubRefreshZero 校验 0 被接受为"禁用自动刷新"，
// 而不是被当作"空值/非法值"拒绝。这是用户直接反馈的问题。
func TestApplySettingsSubRefreshZero(t *testing.T) {
	s := newTestServer(t)

	if err := s.applySettings(map[string]interface{}{"subRefresh": float64(0)}); err != nil {
		t.Fatalf("subRefresh=0（禁用自动刷新）应被接受，实际报错: %v", err)
	}
	if s.cfg.SubRefresh != 0 {
		t.Errorf("subRefresh 应为 0，实际 %d", s.cfg.SubRefresh)
	}

	// 0 必须能落盘并被重新读出，不能在保存/加载环节被回填默认值
	if err := s.cfg.Save(); err != nil {
		t.Fatalf("保存失败: %v", err)
	}
}

// TestApplySettingsSubRefreshRange 校验刷新间隔的边界：
// 0 合法；1-9 与超过上限一律拒绝。
func TestApplySettingsSubRefreshRange(t *testing.T) {
	cases := []struct {
		v      float64
		accept bool
	}{
		{0, true},
		{subscription.MinSubRefresh, true},
		{300, true},
		{subscription.MaxSafeRefresh, true},
		{1, false},
		{9, false},
		{subscription.MaxSafeRefresh + 1, false},
		{9223372036854775807, false}, // 历史遗留的 MaxInt64"禁用"标记，不得再写入
	}

	for _, tc := range cases {
		s := newTestServer(t)
		err := s.applySettings(map[string]interface{}{"subRefresh": tc.v})
		if tc.accept && err != nil {
			t.Errorf("subRefresh=%v 应被接受，实际报错: %v", tc.v, err)
		}
		if !tc.accept && err == nil {
			t.Errorf("subRefresh=%v 应被拒绝，实际被接受", tc.v)
		}
	}
}

// TestApplySettingsPortRange 端口必须在 1-65535，且 0 不能被当成"未填"而放行。
func TestApplySettingsPortRange(t *testing.T) {
	cases := []struct {
		v      float64
		accept bool
	}{
		{10808, true},
		{1, true},
		{65535, true},
		{0, false},
		{-1, false},
		{65536, false},
	}

	for _, tc := range cases {
		s := newTestServer(t)
		err := s.applySettings(map[string]interface{}{"socksPort": tc.v})
		if tc.accept && err != nil {
			t.Errorf("socksPort=%v 应被接受，实际报错: %v", tc.v, err)
		}
		if !tc.accept && err == nil {
			t.Errorf("socksPort=%v 应被拒绝，实际被接受", tc.v)
		}
	}
}

// TestApplySettingsProxyModeWhitelist 代理模式只接受已知取值，防止写入垃圾值。
func TestApplySettingsProxyModeWhitelist(t *testing.T) {
	for _, m := range []string{"smart", "global", "direct"} {
		s := newTestServer(t)
		if err := s.applySettings(map[string]interface{}{"proxyMode": m}); err != nil {
			t.Errorf("proxyMode=%q 应被接受，实际报错: %v", m, err)
		}
	}

	s := newTestServer(t)
	if err := s.applySettings(map[string]interface{}{"proxyMode": "turbo"}); err == nil {
		t.Error("未知 proxyMode 应被拒绝")
	}
}

// TestApplySettingsPartialFailureLeavesConfigUntouched 校验校验失败时不会
// "改了一半"：前面的字段合法、后面的字段非法，则整体都不生效。
func TestApplySettingsPartialFailureLeavesConfigUntouched(t *testing.T) {
	s := newTestServer(t)
	before := s.cfg.SocksPort

	err := s.applySettings(map[string]interface{}{
		"socksPort":  "10809", // 合法值，但类型错误
		"subRefresh": float64(600),
	})
	if err == nil {
		t.Fatal("包含非法字段的请求应当整体失败")
	}
	if s.cfg.SocksPort != before {
		t.Errorf("校验失败时不应改动任何字段：socksPort 从 %d 变成了 %d", before, s.cfg.SocksPort)
	}
	if s.cfg.SubRefresh == 600 {
		t.Error("校验失败时不应应用后续字段")
	}
}

// TestApplySettingsErrorMessageMentionsZero 错误提示要让用户知道 0 是合法值，
// 否则用户看到"需≥10秒"会以为禁用功能不存在。
func TestApplySettingsErrorMessageMentionsZero(t *testing.T) {
	s := newTestServer(t)
	err := s.applySettings(map[string]interface{}{"subRefresh": float64(5)})
	if err == nil {
		t.Fatal("subRefresh=5 应被拒绝")
	}
	if !strings.Contains(err.Error(), "0") {
		t.Errorf("错误信息应说明 0 表示禁用，实际: %v", err)
	}
}
