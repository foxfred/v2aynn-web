package mihomo

import (
	"path/filepath"
	"testing"

	"v2aynn-web/internal/config"
)

// 家宽订阅源换节点很频繁，上次选中的节点下次刷新就没了。
// 启动前必须修正，否则异步恢复那步会一直失败，界面还挂着一个不存在的「当前使用中」。

func TestEnsureActiveNodeReplacesStaleName(t *testing.T) {
	dir := t.TempDir()
	cfg, err := config.Load(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatalf("加载配置失败: %v", err)
	}
	cfg.ClashNode = "🏠 已经没了的节点"

	m := NewManager(dir)
	m.SetConfig(cfg)
	m.mu.Lock()
	m.nodes = []string{"🏠 JP-家宽-01", "🏠 KR-家宽-01"}
	m.activeName = cfg.ClashNode
	m.mu.Unlock()

	m.ensureActiveNodeLocked()

	if m.activeName != "🏠 JP-家宽-01" {
		t.Errorf("activeName = %q, 期望改用订阅里的第一个节点", m.activeName)
	}
	cfg.Lock()
	got := cfg.ClashNode
	cfg.Unlock()
	if got != "🏠 JP-家宽-01" {
		t.Errorf("ClashNode 未同步更新 = %q", got)
	}
	// 必须落盘：否则每次重启都要重挑一遍，界面也一直显示旧名
	reloaded, err := config.Load(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatalf("重新加载配置失败: %v", err)
	}
	reloaded.Lock()
	saved := reloaded.ClashNode
	reloaded.Unlock()
	if saved != "🏠 JP-家宽-01" {
		t.Errorf("落盘后的 ClashNode = %q, 期望「🏠 JP-家宽-01」", saved)
	}
}

// 节点还在时不能乱动用户的选择。
func TestEnsureActiveNodeKeepsValidName(t *testing.T) {
	dir := t.TempDir()
	cfg, err := config.Load(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatalf("加载配置失败: %v", err)
	}
	cfg.ClashNode = "🏠 KR-家宽-01"

	m := NewManager(dir)
	m.SetConfig(cfg)
	m.mu.Lock()
	m.nodes = []string{"🏠 JP-家宽-01", "🏠 KR-家宽-01"}
	m.activeName = "🏠 KR-家宽-01"
	m.mu.Unlock()

	m.ensureActiveNodeLocked()

	if m.activeName != "🏠 KR-家宽-01" {
		t.Errorf("用户选中的节点被改成了 %q", m.activeName)
	}
}

// 从没选过节点时不能「替他选一个」—— 订阅里的 fallback 组自己会挑，
// 而且界面不该凭空冒出一个「当前使用中」。
func TestEnsureActiveNodeDoesNotPickWhenUnset(t *testing.T) {
	dir := t.TempDir()
	cfg, err := config.Load(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatalf("加载配置失败: %v", err)
	}

	m := NewManager(dir)
	m.SetConfig(cfg)
	m.mu.Lock()
	m.nodes = []string{"🏠 JP-家宽-01"}
	m.activeName = ""
	m.mu.Unlock()

	m.ensureActiveNodeLocked()

	if m.activeName != "" {
		t.Errorf("未选过节点时不该自动挑一个，得到 %q", m.activeName)
	}
	cfg.Lock()
	got := cfg.ClashNode
	cfg.Unlock()
	if got != "" {
		t.Errorf("ClashNode 被改成了 %q", got)
	}
}

// 没有 cfg 时（单测里常见）不能 panic。
func TestEnsureActiveNodeWithoutConfig(t *testing.T) {
	m := NewManager(t.TempDir())
	m.mu.Lock()
	m.nodes = []string{"🏠 JP-家宽-01"}
	m.activeName = "已消失"
	m.mu.Unlock()

	m.ensureActiveNodeLocked() // 不应 panic

	if m.activeName != "🏠 JP-家宽-01" {
		t.Errorf("activeName = %q", m.activeName)
	}
}
