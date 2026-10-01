package mihomo

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// delayTesterFunc 把普通函数适配成 DelayTester，测试里用来数调用次数。
type delayTesterFunc func(name string, timeoutMs int) (int, error)

func (f delayTesterFunc) ProxyDelay(name string, timeoutMs int) (int, error) {
	return f(name, timeoutMs)
}

// 限并发是这次修复的核心之一：内核的「整组测延迟」会把 70 多个节点一次性
// 并发全打出去，而它们的出口全塞在同一条前置通道里 —— 挤爆之后大部分超时、
// 少数挤过去的延迟巨大且随机，每次结果都不一样（用户报的「数字乱」）。
func TestSweepDelayBoundedAndComplete(t *testing.T) {
	names := make([]string, 20)
	for i := range names {
		names[i] = fmt.Sprintf("n%02d", i)
	}

	var mu sync.Mutex
	inFlight, peak, calls := 0, 0, 0
	d := delayTesterFunc(func(name string, timeoutMs int) (int, error) {
		mu.Lock()
		inFlight++
		calls++
		if inFlight > peak {
			peak = inFlight
		}
		mu.Unlock()

		time.Sleep(10 * time.Millisecond)

		mu.Lock()
		inFlight--
		mu.Unlock()

		if name == "n07" {
			return 0, fmt.Errorf("节点无响应")
		}
		return 100, nil
	})

	got := SweepDelay(d, names, 1000, 3)

	if len(got) != len(names) {
		t.Fatalf("结果数 = %d, 期望 %d", len(got), len(names))
	}
	if got["n07"] != -1 {
		t.Errorf("测不通的节点应当是 -1, 得到 %d", got["n07"])
	}
	if got["n00"] != 100 {
		t.Errorf("n00 = %d, 期望 100", got["n00"])
	}
	if peak > 3 {
		t.Errorf("并发峰值 = %d, 不应超过 3 —— 超了就会把前置通道挤爆", peak)
	}
	if calls <= len(names) {
		t.Errorf("测通的节点应当复测一次取较小值, 总调用 = %d（%d 个节点）", calls, len(names))
	}
}

// 首次请求要现场建 OpenVPN 隧道，测到的是握手耗时；复测一次取较小值，
// 拿到的才是真实链路延迟。测不通的节点不能复测 —— 那只是白等一次超时。
func TestMeasureDelayTakesBestOfTwo(t *testing.T) {
	counts := map[string]int{}
	d := delayTesterFunc(func(name string, timeoutMs int) (int, error) {
		counts[name]++
		switch name {
		case "warm":
			if counts[name] == 1 {
				return 3000, nil // 第一次：建隧道
			}
			return 200, nil // 第二次：走已建好的隧道
		case "cold":
			return 5000, nil // 两次一样
		default:
			return 0, fmt.Errorf("节点无响应")
		}
	})

	if got := MeasureDelay(d, "warm", 8000); got != 200 {
		t.Errorf("warm = %d, 期望取复测后的较小值 200", got)
	}
	if counts["warm"] != 2 {
		t.Errorf("测通的节点应当复测一次, 实际调用 %d 次", counts["warm"])
	}
	if got := MeasureDelay(d, "cold", 8000); got != 5000 {
		t.Errorf("cold = %d, 期望 5000", got)
	}
	if got := MeasureDelay(d, "dead", 8000); got != -1 {
		t.Errorf("测不通应当是 -1, 得到 %d", got)
	}
	if counts["dead"] != 1 {
		t.Errorf("测不通的节点不该复测（白等一次超时）, 实际调用 %d 次", counts["dead"])
	}
}

// 前置通道组名要从 dialer-proxy 里取出来。它在 YAML 里可能写在 type 之前
// 也可能之后，两种顺序都要认。
func TestParseSubscriptionExtractsFrontGroup(t *testing.T) {
	src := `proxies:
  - name: "HW1"
    dialer-proxy: "⚡ CF前置"
    type: openvpn
  - name: "HW2"
    type: openvpn
    dialer-proxy: "⚡ CF前置"
proxy-groups:
  - name: "⚡ CF前置"
    type: url-test
    proxies:
      - "优选域名-01"
  - name: "家宽组"
    type: select
    proxies:
      - "HW1"
      - "HW2"
`
	info := parseSubscription(src)
	if info.frontGroup != "⚡ CF前置" {
		t.Errorf("frontGroup = %q, 期望 ⚡ CF前置", info.frontGroup)
	}
	if got := FrontGroupOf([]byte(src)); got != "⚡ CF前置" {
		t.Errorf("FrontGroupOf = %q", got)
	}
	// 没有 dialer-proxy 的订阅（比如普通 Clash 配置）不能瞎认
	if got := FrontGroupOf([]byte("proxies:\n  - name: A\n    type: ss\n")); got != "" {
		t.Errorf("没有 dialer-proxy 时应当返回空串, 得到 %q", got)
	}
}

// 探针启动前的环境校验：缺什么就明确说什么，不能一路走到「起不来还不说为什么」。
func TestProberEnsureReportsMissingPieces(t *testing.T) {
	base := t.TempDir()
	p := NewProber(base)

	if err := p.Ensure("g1", false); err == nil || !strings.Contains(err.Error(), "mihomo 内核") {
		t.Errorf("缺内核时应当说明, 得到 %v", err)
	}

	bin := filepath.Join(base, "mihomo")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0755); err != nil {
		t.Fatal(err)
	}
	p.SetBin(bin)
	if err := p.Ensure("g1", false); err == nil || !strings.Contains(err.Error(), GeoIPMetaDB) {
		t.Errorf("缺 geo 数据时应当说明（否则 mihomo 会联网下载卡满 90 秒）, 得到 %v", err)
	}

	if err := os.WriteFile(filepath.Join(base, GeoIPMetaDB), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := p.Ensure("g1", false); err == nil || !strings.Contains(err.Error(), "订阅原文") {
		t.Errorf("缺订阅原文时应当提示去点「更新」, 得到 %v", err)
	}

	if p.IsRunning() {
		t.Error("校验失败时不该留下运行中的探针")
	}
	if p.Group() != "" {
		t.Errorf("校验失败后不该留下分组标记: %q", p.Group())
	}
}

// 进程起不来（或控制口一直不监听）时必须报错并清理干净，不能留下孤儿进程。
func TestProberEnsureCleansUpWhenStartFails(t *testing.T) {
	base := t.TempDir()
	bin := filepath.Join(base, "mihomo")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 1\n"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, GeoIPMetaDB), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, subFileNameFor("g1")), []byte(sampleSub), 0644); err != nil {
		t.Fatal(err)
	}

	p := NewProber(base)
	p.SetBin(bin)
	p.startTimeout = 300 * time.Millisecond // 别让测试真等 40 秒

	if err := p.Ensure("g1", false); err == nil {
		t.Fatal("探针起不来时 Ensure 应当报错")
	}
	if p.IsRunning() {
		t.Error("启动失败后不该留下运行中的进程")
	}
	if p.Group() != "" {
		t.Errorf("启动失败后不该留下分组标记: %q", p.Group())
	}
}

// 探针有自己的工作目录，geo 数据链过去即可 ——
// 与家宽内核共用 dataDir 会让两个 mihomo 争同一份 cache.db 与运行配置。
func TestProberPrepareDirLinksGeo(t *testing.T) {
	base := t.TempDir()
	if err := os.WriteFile(filepath.Join(base, GeoIPMetaDB), []byte("geo"), 0644); err != nil {
		t.Fatal(err)
	}
	p := NewProber(base)
	if err := p.prepareDir(); err != nil {
		t.Fatalf("prepareDir 失败: %v", err)
	}

	b, err := os.ReadFile(filepath.Join(p.dir, GeoIPMetaDB))
	if err != nil || string(b) != "geo" {
		t.Errorf("探针目录里读不到 geo 数据: err=%v 内容=%q", err, b)
	}
	// 探针目录必须在 dataDir 下面（这样订阅原文与它才是同一个基准路径）
	if !strings.HasPrefix(p.dir, base) {
		t.Errorf("探针目录 %q 不在 dataDir %q 下", p.dir, base)
	}
	// 不能反过来把探针的配置文件写进家宽内核的目录
	if _, err := os.Stat(filepath.Join(base, probeConfName)); err == nil {
		t.Error("探针配置不该写到家宽内核的目录里")
	}
}

func TestFreePortReturnsUsablePort(t *testing.T) {
	port, err := freePort()
	if err != nil {
		t.Fatalf("freePort 失败: %v", err)
	}
	if port <= 0 || port > 65535 {
		t.Fatalf("端口越界: %d", port)
	}
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		t.Fatalf("挑出来的端口 %d 实际不可用: %v", port, err)
	}
	_ = ln.Close()
}

// 探针没在跑时必须立刻报错，而不是让调用方傻等一次连接超时。
func TestProberProxyDelayRequiresRunning(t *testing.T) {
	p := NewProber(t.TempDir())
	if _, err := p.ProxyDelay("HW1", 1000); err == nil {
		t.Error("探针没在跑时应当报错")
	}
}

// 内存判断只用于「要不要让探针常驻」这个优化，读不到就该按「够用」处理，
// 不能因为读不到 /proc/meminfo 就把功能降级。
func TestProberCanReside(t *testing.T) {
	mb, ok := memAvailableMB()
	if !ok {
		t.Skip("非 Linux 环境没有 /proc/meminfo")
	}
	if mb <= 0 {
		t.Errorf("可用内存 = %d MB, 不合理", mb)
	}
	if mb >= probeMinFreeMB && !ProberCanReside() {
		t.Errorf("可用 %d MB 已超过阈值 %d，应当允许常驻", mb, probeMinFreeMB)
	}
}

// 每个节点开测前都要错峰一次 —— 同时开测的几个节点会一起抢同一条前置通道，
// 互相踩踏之后测出来的就是噪声。对齐 Clash Verge Rev 的做法。
//
// 这里把抖动换成「返回 0」的确定实现：既验证了它每个节点都调用了一次，
// 又不会为了等随机睡眠把用例拖慢（也避免对随机数断言造成偶发失败）。
func TestSweepDelayAppliesJitterBeforeEachMeasure(t *testing.T) {
	old := sweepJitter
	defer func() { sweepJitter = old }()

	var mu sync.Mutex
	calls := 0
	sweepJitter = func() time.Duration {
		mu.Lock()
		calls++
		mu.Unlock()
		return 0
	}

	d := delayTesterFunc(func(name string, timeoutMs int) (int, error) { return 100, nil })
	names := []string{"a", "b", "c", "d"}
	SweepDelay(d, names, 1000, 2)

	if calls != len(names) {
		t.Errorf("每个节点开测前都该错峰一次, 期望 %d 次, 实际 %d", len(names), calls)
	}
}

// 错峰等待不能设太大：70 多个节点累加起来会把整轮测速拖得很难受。
func TestSweepJitterStaysSmall(t *testing.T) {
	if SweepJitterMax <= 0 {
		t.Fatal("错峰等待被关掉了 —— 并发测速会重新变成互相踩踏")
	}
	if SweepJitterMax > 500*time.Millisecond {
		t.Errorf("错峰等待 %v 太长，70 多个节点累加会明显拖慢整轮测速", SweepJitterMax)
	}
}

// 测速地址必须跟着订阅走：cfnew 给组写的是 https://，早先我们硬编码成 http://，
// 正好踩中 mihomo 那条「unified-delay 下用 HTTP 可能测不通」的官方警告，
// 表现就是数字忽大忽小、时有时无。
func TestTestURLOfPrefersFrontGroupURL(t *testing.T) {
	src := `proxies:
  - name: "HW1"
    type: openvpn
    dialer-proxy: "⚡ CF前置"
proxy-groups:
  - name: "⚡ CF前置"
    type: url-test
    url: https://www.gstatic.com/generate_204
    proxies:
      - "A"
  - name: "家宽组"
    type: select
    proxies:
      - "HW1"
`
	if got := TestURLOf([]byte(src)); got != "https://www.gstatic.com/generate_204" {
		t.Errorf("TestURLOf = %q, 期望取前置组自己写的 url", got)
	}
}

// 前置组没写 url（比如它是 select 组）时，退而取别的组写的地址；
// 一个都没有才用兜底值 —— 而兜底值必须是 https。
func TestTestURLOfFallsBack(t *testing.T) {
	noURL := `proxies:
  - name: "HW1"
    type: openvpn
    dialer-proxy: "⚡ CF前置"
proxy-groups:
  - name: "⚡ CF前置"
    type: url-test
    proxies:
      - "A"
`
	if got := TestURLOf([]byte(noURL)); got != defaultTestURL {
		t.Errorf("组里都没写 url 时应当退回默认值 %q, 得到 %q", defaultTestURL, got)
	}

	otherURL := `proxies:
  - name: "HW1"
    type: openvpn
    dialer-proxy: "⚡ CF前置"
proxy-groups:
  - name: "⚡ CF前置"
    type: select
    proxies:
      - "A"
  - name: "别的组"
    type: url-test
    url: "https://cp.cloudflare.com/generate_204"
    proxies:
      - "A"
`
	if got := TestURLOf([]byte(otherURL)); got != "https://cp.cloudflare.com/generate_204" {
		t.Errorf("前置组没写 url 时应取其他组写的, 得到 %q", got)
	}
}

// 兜底地址一旦变成 http:// 就等于把刚修好的坑又踩回去，钉死它。
func TestDefaultTestURLIsHTTPS(t *testing.T) {
	if !strings.HasPrefix(defaultTestURL, "https://") {
		t.Errorf("兜底测速地址必须是 https（unified-delay 下用 http 会测不通）, 得到 %q", defaultTestURL)
	}
}
