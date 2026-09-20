package subscription

import (
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"v2aynn-web/internal/config"
)

// 这两个用例覆盖的是用户实际报障的场景：在设置里把「订阅刷新间隔」设为 0
// （界面标签明确写着「0=禁用自动刷新」），但后台仍按旧间隔继续拉订阅。
//
// 根因是 Poll 原先只在进程启动时读一次 SubRefresh 并据此建一个固定 ticker，
// 运行时改配置对已经在跑的 ticker 没有任何影响，必须重启服务才停。
//
// 做法：起一个本地 HTTP 服务器冒充订阅源，数它被请求了几次。

// probe 起一个冒充订阅源的服务器，返回计数器读取函数。
func probe(t *testing.T) (url string, hits func() int64) {
	t.Helper()
	var n int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&n, 1)
		_, _ = w.Write([]byte("probe"))
	}))
	t.Cleanup(srv.Close)
	return srv.URL, func() int64 { return atomic.LoadInt64(&n) }
}

// newPollCfg 造一份指向探针服务器的配置。
// SubRefresh=1（秒）是能观察到的最短间隔，Poll 只校验 <=0 与上限，不校验下限。
func newPollCfg(t *testing.T, url string) *config.Config {
	t.Helper()
	cfg, err := config.Load(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatalf("准备配置失败: %v", err)
	}
	cfg.SubRefresh = 1
	cfg.Groups = []config.Group{{ID: "g1", Name: "probe", URL: url}}
	return cfg
}

// shortenPollTick 把复查周期缩到 50ms，否则每个用例都要干等 5 秒。
// 用 Cleanup 还原，避免污染同包内其他用例。
func shortenPollTick(t *testing.T) {
	t.Helper()
	old := pollTick
	pollTick = 50 * time.Millisecond
	t.Cleanup(func() { pollTick = old })
}

// silenceLog 屏蔽拉取过程的大量日志，让测试输出可读。
func silenceLog(t *testing.T) {
	t.Helper()
	old := log.Writer()
	log.SetOutput(io.Discard)
	t.Cleanup(func() { log.SetOutput(old) })
}

func setRefresh(cfg *config.Config, n int) {
	cfg.Lock()
	cfg.SubRefresh = n
	cfg.Unlock()
}

func waitHits(hits func() int64, want int64, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if hits() >= want {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

// TestPollStopsWhenSubRefreshDisabledAtRuntime 运行时把间隔改成 0 后，必须真的停。
func TestPollStopsWhenSubRefreshDisabledAtRuntime(t *testing.T) {
	shortenPollTick(t)
	silenceLog(t)
	url, hits := probe(t)
	cfg := newPollCfg(t, url)

	go Poll(cfg)

	// 先确认探针链路是通的：能观察到第一次拉取
	if !waitHits(hits, 1, 3*time.Second) {
		t.Fatalf("3 秒内一次拉取都没发生，探针本身没跑起来（hits=%d）", hits())
	}

	// 等价于在设置面板里把「订阅刷新间隔」设为 0 并保存
	setRefresh(cfg, 0)

	time.Sleep(300 * time.Millisecond) // 放过可能正在进行的这一次
	base := hits()

	time.Sleep(2 * time.Second) // 原间隔 1 秒，这段时间内本不该再有拉取
	if after := hits(); after != base {
		t.Errorf("SubRefresh 已改为 0（禁用），后台仍在拉取：%d 次 -> %d 次\n"+
			"说明 Poll 没有复查配置，必须重启服务才停", base, after)
	}
}

// TestPollResumesWhenSubRefreshReEnabled 禁用后再重新启用，必须能恢复拉取。
// 防止修成「一旦禁用就再也不看了」。
func TestPollResumesWhenSubRefreshReEnabled(t *testing.T) {
	shortenPollTick(t)
	silenceLog(t)
	url, hits := probe(t)
	cfg := newPollCfg(t, url)

	go Poll(cfg)

	if !waitHits(hits, 1, 3*time.Second) {
		t.Fatalf("3 秒内一次拉取都没发生，探针本身没跑起来（hits=%d）", hits())
	}

	setRefresh(cfg, 0)
	time.Sleep(300 * time.Millisecond)
	stopped := hits()
	time.Sleep(time.Second)
	if got := hits(); got != stopped {
		t.Fatalf("禁用后仍在拉取（%d -> %d），本用例前提不成立", stopped, got)
	}

	// 重新启用，应当恢复拉取
	setRefresh(cfg, 1)
	if !waitHits(hits, stopped+1, 3*time.Second) {
		t.Errorf("重新启用（SubRefresh=1）后 3 秒内没有恢复拉取（停在 %d 次）", stopped)
	}
}
