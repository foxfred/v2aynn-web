package v2ray

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"v2aynn-web/internal/config"
)

// TestMain 让本测试二进制可以「自复用」为假 xray 进程。
//
// 做法：设置 V2AYNN_FAKE_XRAY=1 后重新执行自身，在 flag 解析之前就退出，
// 并把一次「启动」追加记录到 V2AYNN_FAKE_XRAY_LOG。
// 好处是不依赖任何外部二进制、也不需要 C 编译器，就能在本地真实跑通
// 「进程启动 → 异常退出 → watch 自愈重启」这条链路。
func TestMain(m *testing.M) {
	if os.Getenv("V2AYNN_FAKE_XRAY") == "1" {
		if p := os.Getenv("V2AYNN_FAKE_XRAY_LOG"); p != "" {
			if f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644); err == nil {
				_, _ = f.WriteString("started\n")
				_ = f.Close()
			}
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func countStarts(path string) int {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	return strings.Count(string(b), "started")
}

// waitForStarts 轮询等待日志中出现至少 want 次启动记录。
func waitForStarts(t *testing.T, path string, want int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if countStarts(path) >= want {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("等待第 %d 次启动超时（实际 %d 次）：watch 自愈重启可能已失效",
		want, countStarts(path))
}

func testConfig() *config.Config {
	return &config.Config{
		ActiveNode: "n1",
		Groups: []config.Group{{
			ID: config.DefaultGroupID, Name: "手动节点",
			Nodes: []config.Node{{
				ID: "n1", Name: "测试节点", Protocol: "trojan",
				Server: "example.com", Port: "443", Password: "pw",
			}},
		}},
	}
}

// TestWatchRestartsDeadProcess 验证 watch() 的自愈重启链路在改动后仍然工作。
//
// 为什么必须有这个测试：为消除 m.running 的数据竞争，重启判定被挪到了 5 秒等待
// 之后，并新增了 alreadyRunning 判断。这类改动写错的最坏后果是「xray 挂了再也不会
// 自动拉起」—— 恰恰是这台盒子最依赖的可靠性功能，而且单元测试的静态断言发现不了。
//
// 手法：把 xrayBin 指向测试二进制自身（配合 TestMain 的自复用），假 xray 每次启动
// 都往日志追加一行后立刻退出，从而触发 watch 的自愈重启。
//
// 注意：重启计数到 4 次后会走 tryFailover；本用例配置里只有当前节点，
// 候选为空 → 放弃重启，因此循环会自行停止，不会无限跑下去。
func TestWatchRestartsDeadProcess(t *testing.T) {
	if testing.Short() {
		t.Skip("watch 内含 5 秒等待，短模式跳过")
	}
	dir := t.TempDir()
	logPath := filepath.Join(dir, "starts.log")

	m := NewManager(t.TempDir())
	m.SetXrayBin(os.Args[0]) // 用测试二进制自身当假 xray
	t.Setenv("V2AYNN_FAKE_XRAY", "1")
	t.Setenv("V2AYNN_FAKE_XRAY_LOG", logPath)
	m.SetConfig(testConfig())

	if err := m.Start(); err != nil {
		t.Fatalf("首次启动失败: %v", err)
	}
	defer m.Stop()

	// 1) 首次启动应被记录
	waitForStarts(t, logPath, 1, 5*time.Second)

	// 2) 假 xray 立刻退出 → watch 等待 5 秒后应自动重启，日志出现第 2 行。
	//    自愈链路若被改坏，这里会超时失败。
	waitForStarts(t, logPath, 2, 12*time.Second)
}

// TestStopDoesNotTriggerRestart 验证「主动停机不触发自愈重启」。
//
// 这条语义靠 isCurrent 判断维持：Stop() 会把 m.cmd 置 nil，于是进程退出时
// watch 看到 isCurrent=false 就直接返回。若该判断被破坏，用户点「停止」后
// xray 会被自己拉起来，停止功能形同虚设。
func TestStopDoesNotTriggerRestart(t *testing.T) {
	if testing.Short() {
		t.Skip("含等待，短模式跳过")
	}
	dir := t.TempDir()
	logPath := filepath.Join(dir, "starts.log")

	m := NewManager(t.TempDir())
	m.SetXrayBin(os.Args[0])
	t.Setenv("V2AYNN_FAKE_XRAY", "1")
	t.Setenv("V2AYNN_FAKE_XRAY_LOG", logPath)
	m.SetConfig(testConfig())

	if err := m.Start(); err != nil {
		t.Fatalf("首次启动失败: %v", err)
	}
	waitForStarts(t, logPath, 1, 5*time.Second)

	m.Stop()
	before := countStarts(logPath)

	// 超过一个完整的自愈周期（5 秒）后，启动次数不应再增加
	time.Sleep(7 * time.Second)
	if after := countStarts(logPath); after != before {
		t.Errorf("主动 Stop() 后仍被自愈拉起：启动次数 %d → %d", before, after)
	}
}

// shortLivedCmd 启动一个立即退出的进程，用于驱动 watch() 走「进程异常退出」分支。
func shortLivedCmd(t *testing.T) *exec.Cmd {
	t.Helper()
	var c *exec.Cmd
	if runtime.GOOS == "windows" {
		c = exec.Command("cmd", "/c", "exit", "0")
	} else {
		c = exec.Command("sh", "-c", "true")
	}
	if err := c.Start(); err != nil {
		t.Skipf("无法启动测试进程，跳过: %v", err)
	}
	return c
}

// TestWatchDoesNotRaceOnRunningFlag 是数据竞争回归测试，**必须用 `go test -race` 才能发现**。
//
// 历史缺陷：watch() 在 `m.mu.Unlock()` 之后直接读 `!m.running`，
// 而 m.running 由 Start()/Stop() 在锁内并发写入。无锁读 + 锁内写 = 数据竞争。
// 目标平台是 ARM64（弱内存模型），更不该依赖这种读法的偶然正确性。
//
// 后果不是崩溃，而是**静默的误判**：漏看并发的 Start() 会把「用户有意重启」
// 误记成「节点故障」，重启计数累积到 4 次还会触发一次不必要的自动故障转移。
//
// 用法：
//
//	go test -race ./internal/v2ray/ -run TestWatchDoesNotRace
//
// 修复前应打印 WARNING: DATA RACE，修复后应干净通过。
func TestWatchDoesNotRaceOnRunningFlag(t *testing.T) {
	// 必须显式跳过，否则在普通 go test 下这个测试会「永远通过」——
	// 竞争检测器没启用时它什么都证明不了。详见 race_on_test.go。
	if !raceEnabled {
		t.Skip("未启用 -race，本测试无法证明任何事；请用 go test -race 运行")
	}
	if testing.Short() {
		t.Skip("watch 内含 5 秒等待，短模式跳过")
	}
	m := NewManager(t.TempDir())
	// ActiveNode 留空 → watch 末尾的自愈 Start() 会立即返回错误，不会真的拉起进程
	m.SetConfig(&config.Config{})

	// 先让「写者」跑起来，确保 watch() 做无锁读时它一定处于运行状态，
	// 否则这个测试会变成「有时能测出、有时测不出」的不稳定测试。
	stop := make(chan struct{})
	var writer sync.WaitGroup
	writer.Add(1)
	go func() {
		defer writer.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			// 这正是 Start()/Stop() 在锁内对 m.running 做的事
			m.mu.Lock()
			m.running = i%2 == 0
			m.mu.Unlock()
		}
	}()
	time.Sleep(50 * time.Millisecond)

	const watchers = 8
	var wg sync.WaitGroup
	for i := 0; i < watchers; i++ {
		c := shortLivedCmd(t)
		// 让 isCurrent 成立，watch 才会走到后面那段读 m.running 的代码
		m.mu.Lock()
		m.cmd = c
		m.running = true
		m.mu.Unlock()

		wg.Add(1)
		go func(c *exec.Cmd) {
			defer wg.Done()
			m.watch(c)
		}(c)
	}

	wg.Wait()
	close(stop)
	writer.Wait()
}
