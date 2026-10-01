package mihomo

import (
	"fmt"
	"log"
	"math/rand"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DelayTester 能测单个家宽节点延迟的后端。
//
// 家宽内核管理器与测速探针都满足它 —— web 层据此在两者之间挑一个，
// 不必关心到底是谁在测。
type DelayTester interface {
	ProxyDelay(name string, timeoutMs int) (int, error)
}

// FrontSwitcher 能固定前置通道组的后端（DelayTester 的扩展）。
//
// 单独一个接口、并且用类型断言按需取用，是为了让「换前置」这件事只在真正
// 支持的后端上发生。接口拆开还有个好处：单元测试里的替身可以只实现
// DelayTester，不被迫实现一堆用不到的写操作。
type FrontSwitcher interface {
	SetFront(group, name string) error
}

// 编译期确认两个后端都满足 web 层要用到的全部接口。
// 少实现一个不会报错，只会在运行期悄悄退化成「没有这个能力」。
var (
	_ DelayTester    = (*Manager)(nil)
	_ DelayTester    = (*Prober)(nil)
	_ FrontSwitcher  = (*Manager)(nil)
	_ FrontSwitcher  = (*Prober)(nil)
	_ GroupNowTester = (*Manager)(nil)
	_ GroupNowTester = (*Prober)(nil)
	_ DelayReader    = (*Manager)(nil)
	_ DelayReader    = (*Prober)(nil)
)

// ClashSweepConcurrency 家宽分组测速的并发数。
//
// 全部家宽节点的出口都塞在同一条前置通道里，并发越高越容易互相踩踏 ——
// 这正是「测出来的数字全是乱的」的根因。4 是在「别把通道挤爆」与
// 「整组别测太久」之间的折中。
const ClashSweepConcurrency = 4

// MeasureDelay 测一个家宽节点的延迟，返回毫秒；测不通返回 -1。
//
// 测通之后会再测一次取较小值。原因：第一次请求要现场建立 OpenVPN 隧道，
// 测到的其实是握手耗时（常常几秒），完全不能代表这条链路的延迟；
// 第二次走的是已经建好的隧道，才是真实数字。只有已经测通的节点才会走
// 第二次，所以不会把前置通道再挤一遍。
func MeasureDelay(t DelayTester, name string, timeoutMs int) int {
	ms, err := t.ProxyDelay(name, timeoutMs)
	if err != nil {
		return -1
	}
	if ms2, err2 := t.ProxyDelay(name, timeoutMs); err2 == nil && ms2 < ms {
		ms = ms2
	}
	return ms
}

// SweepJitterMax 每个节点开测前的最大随机等待。
//
// 并发一放出去，几个节点会在同一瞬间去抢同一条前置通道建隧道，互相踩踏之后
// 测到的数字是噪声而不是节点本身。每个 worker 开测前随机等一小会儿把压力摊平，
// 代价只是整轮多花几百毫秒。做法对齐 Clash Verge Rev（它对每个节点随机等 0–200ms）。
var SweepJitterMax = 200 * time.Millisecond

// sweepJitter 返回一个节点开测前要等多久。
//
// 单独抽成变量是为了让测试能换成确定实现 —— 直接对随机数断言会变成偶发失败的用例。
var sweepJitter = func() time.Duration {
	if SweepJitterMax <= 0 {
		return 0
	}
	return time.Duration(rand.Int63n(int64(SweepJitterMax)))
}

// SweepDelay 限并发地逐个测家宽节点的延迟，返回 节点名→毫秒（-1 表示不通）。
//
// 为什么不用内核的「整组测延迟」接口（/group/<组名>/delay）：那个接口把组里
// 所有节点一次性并发全打出去。家宽节点的出口全都塞在同一条前置通道里，
// 70 多个 OpenVPN 握手同时挤进去会把通道撑爆 —— 大部分节点超时、少数挤过去
// 的延迟巨大且随机，每次测结果都不一样。限并发把压力摊开，测出来的数字
// 才反映节点本身，而不是互相踩踏的噪声。
func SweepDelay(t DelayTester, names []string, timeoutMs, concurrency int) map[string]int {
	if concurrency < 1 {
		concurrency = 1
	}
	out := make(map[string]int, len(names))
	var mu sync.Mutex
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	for _, n := range names {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			sem <- struct{}{}
			// 错峰放在占住名额之后：真正会互相踩的是同时在测的那几个，
			// 让它们的开测时刻散开才有意义。
			if d := sweepJitter(); d > 0 {
				time.Sleep(d)
			}
			ms := MeasureDelay(t, name, timeoutMs)
			<-sem
			mu.Lock()
			out[name] = ms
			mu.Unlock()
		}(n)
	}
	wg.Wait()
	return out
}

// CountAlive 数这批节点里有几个能连通，数到 limit 个就立刻收工。
//
// 用途是「拿几个家宽节点当探针，判断前置通道此刻能不能承载家宽链」。
// 与 SweepDelay 的两点差别：
//   - 不取第二次。这里只问通不通，不关心真实延迟。
//   - 数够 limit 就收工 —— 「够多」是结论性证据，没必要把剩下的样本全等完。
//     全都不够时才需要等满，那时每份样本都要耗到超时。
//
// 返回 min(实际连通数, limit)。
//
// ★ 为什么不提供「有一个通就返回 true」的版本：盒子实测「联通-01」这个前置
// 在同一批 6 个样本上通 0 个、整组 65 个节点只测出 6 个；换成「优选域名-04」
// 同样 6 个样本能通 5 个。可见「勉强有 1 个通」和「能承载大半样本」差别巨大，
// 判据必须看比例，不能看有无。
//
// 并发与错峰沿用整组测速那一套：样本虽小，也挤在同一条前置通道上。
func CountAlive(t DelayTester, names []string, limit, timeoutMs, concurrency int) int {
	if len(names) == 0 || limit <= 0 {
		return 0
	}
	if concurrency < 1 {
		concurrency = 1
	}
	sem := make(chan struct{}, concurrency)
	var mu sync.Mutex
	var wg sync.WaitGroup
	alive := 0
	for _, n := range names {
		// 先占名额再复查「够了没」：等名额期间可能有别的样本已经测通了，
		// 这时该让出名额直接收工，而不是照样再打一发。
		sem <- struct{}{}
		mu.Lock()
		stop := alive >= limit
		mu.Unlock()
		if stop {
			<-sem
			break
		}
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			defer func() { <-sem }()
			if d := sweepJitter(); d > 0 {
				time.Sleep(d)
			}
			if _, err := t.ProxyDelay(name, timeoutMs); err == nil {
				mu.Lock()
				alive++
				mu.Unlock()
			}
		}(n)
	}
	wg.Wait()
	if alive > limit {
		alive = limit
	}
	return alive
}

// freePort 找一个当前空闲的本地端口。
//
// 先 Listen 再立刻关掉，存在极小的被抢占窗口 —— 但探针端口本来就是临时的，
// 真被抢了启动会失败、Ensure 返回错误，用户重试即可，不会留下坏状态。
func freePort() (int, error) {
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		return 0, err
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port, nil
}

// probeMinFreeMB 允许测速探针常驻所需的最小可用内存（MB）。
//
// 盒子只有 1 GB 内存且没有 swap，两个内核同时常驻是笔实打实的开销。
// 低于这个数就不常驻：每轮测速结束后立刻把探针收掉，把内存还回去 ——
// 代价只是下次测速多等十几秒启动，功能不受影响。
const probeMinFreeMB = 150

// memAvailableMB 读 /proc/meminfo 里的 MemAvailable（MB）。
//
// 读不到（非 Linux、或格式变了）时第二个返回 false，调用方按「内存充足」
// 处理 —— 这只是省内存的优化，不该因为读不到就把功能关掉。
func memAvailableMB() (int, bool) {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, false
	}
	for _, ln := range strings.Split(string(b), "\n") {
		if !strings.HasPrefix(ln, "MemAvailable:") {
			continue
		}
		f := strings.Fields(ln)
		if len(f) < 2 {
			return 0, false
		}
		kb, err := strconv.Atoi(f[1])
		if err != nil {
			return 0, false
		}
		return kb / 1024, true
	}
	return 0, false
}

// ProberCanReside 当前可用内存够不够让探针常驻。
//
// 读不到内存信息（非 Linux）时按「够」处理 —— 这只是省内存的优化，
// 不该因为读不到就把功能降级。
func ProberCanReside() bool {
	mb, ok := memAvailableMB()
	return !ok || mb >= probeMinFreeMB
}

// probeDirName 探针实例的工作目录名（位于家宽内核的 dataDir 下）
const probeDirName = "probe"

// probeConfName 探针实例的配置文件
const probeConfName = "config.yaml"

// probeStartTimeout 探针从启动到控制接口可用的等待上限。
// mihomo 要先加载 geo 数据、解析 70 多个节点、建策略组，盒子上要几秒。
const probeStartTimeout = 40 * time.Second

// Prober 家宽测速探针 —— 一个跑在备用端口上的独立 mihomo 实例。
//
// 为什么需要它：家宽内核与 xray 抢同一组端口（10808/10810/12345），同一时刻
// 只能跑一个。用户连着 xray 时家宽内核根本不在跑，「测一下家宽节点」就只能是
// 「先把连接切到家宽」—— 节点要是不通，用户的连接当场就断，而且再也没法测别的。
//
// 探针用一组备用端口，与正在服务流量的内核互不干扰：连着 xray 也能测家宽，
// 且完全不动用户当前的连接。它只被 external-controller 调用，不承载任何流量，
// 所以 allow-lan 关掉、端口也只绑本机。
type Prober struct {
	// busy 串行化 Ensure / Stop —— 它们要起进程、做网络 IO，不能并发跑。
	busy sync.Mutex
	// mu 保护下面这些状态字段。
	mu sync.Mutex

	bin  string
	base string // 家宽内核的 dataDir（订阅原文与 geo 数据都在这里）
	dir  string // 探针自己的工作目录

	cmd     *exec.Cmd
	running bool
	groupID string // 当前装在里面的是哪个家宽分组
	// testURL 这个分组测延迟该用的地址，从订阅原文里读出来（见 TestURLOf）。
	// 在 Ensure 里算一次存下来即可 —— 一轮测速每个节点要调用两次，
	// 没必要每次都重扫一遍订阅原文。
	testURL string

	socks, http, redir, ctrl int
	// resident 是否常驻。内存紧张、或家宽内核正在服务流量时为 false，
	// 此时每轮测速结束后由调用方 Stop()，把内存还回去。
	resident bool
	// startTimeout 启动等待上限，0 表示用 probeStartTimeout。仅测试用。
	startTimeout time.Duration
}

// NewProber 创建探针。baseDataDir 必须是家宽内核用的那个 dataDir ——
// 订阅原文与 geo 数据都在它下面。
func NewProber(baseDataDir string) *Prober {
	return &Prober{
		bin:  DefaultBinPath,
		base: baseDataDir,
		dir:  filepath.Join(baseDataDir, probeDirName),
	}
}

// SetBin 覆盖 mihomo 二进制路径（测试与非标准部署环境使用）
func (p *Prober) SetBin(b string) { p.bin = b }

// IsRunning 探针进程是否在跑
func (p *Prober) IsRunning() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.running
}

// Group 探针里当前装着的家宽分组 ID（没装任何分组时为空）
func (p *Prober) Group() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.groupID
}

// Resident 探针当前是否常驻
func (p *Prober) Resident() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.resident
}

// ControlAddr 探针的 external-controller 地址（测试用）
func (p *Prober) ControlAddr() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.ctrlAddrLocked()
}

func (p *Prober) ctrlAddrLocked() string { return fmt.Sprintf("127.0.0.1:%d", p.ctrl) }

// Ensure 确保探针里装着指定分组的配置、且进程在跑。
//
// 同一个分组重复调用是廉价的：已经在跑就直接返回，只更新常驻标记。
// 换分组必须整个重来 —— mihomo 一次只加载一份配置。
func (p *Prober) Ensure(groupID string, resident bool) error {
	p.busy.Lock()
	defer p.busy.Unlock()

	p.mu.Lock()
	already := p.running && p.groupID == groupID
	if already {
		p.resident = resident
	}
	p.mu.Unlock()
	if already {
		return nil
	}

	p.kill()

	if err := p.checkEnv(); err != nil {
		return err
	}
	body, err := readSubFile(p.base, groupID)
	if err != nil {
		return fmt.Errorf("读不到这个家宽分组的订阅原文，请先点「更新」重新拉取")
	}
	if len(parseSubscription(string(body)).nodes) == 0 {
		return fmt.Errorf("订阅里没有 openvpn 节点（cfnew 的链接需要带 target=vg 参数）")
	}
	// 测速地址从订阅里读，别写死 —— 见 TestURLOf 的说明。
	testURL := TestURLOf(body)

	// 端口每次重来都重新挑：上一次用过的端口可能已经被别的进程占走
	var ports [4]int
	for i := range ports {
		if ports[i], err = freePort(); err != nil {
			return err
		}
	}
	socks, httpPort, redir, ctrl := ports[0], ports[1], ports[2], ports[3]

	p.mu.Lock()
	p.socks, p.http, p.redir, p.ctrl = socks, httpPort, redir, ctrl
	// 分组标记先清空，等真起来、控制口也通了才认 —— 中途任何一步失败
	// 都不能留下「看起来装好了其实没装」的状态，那会让后续测速去问一个
	// 不存在的内核要数据。
	p.groupID = ""
	p.testURL = ""
	p.resident = resident
	timeout := p.startTimeout
	p.mu.Unlock()
	if timeout <= 0 {
		timeout = probeStartTimeout
	}

	if err := p.prepareDir(); err != nil {
		return err
	}
	conf, err := rewriteConfig(string(body), confPorts{
		socks: socks, http: httpPort, redir: redir, ctrl: ctrl,
		// 探针不承载任何流量，端口不该被局域网里任何设备误连上
		allowLan: false,
	})
	if err != nil {
		return err
	}
	confPath := filepath.Join(p.dir, probeConfName)
	if err := os.WriteFile(confPath, []byte(conf), 0644); err != nil {
		return err
	}

	cmd := exec.Command(p.bin, "-d", p.dir, "-f", confPath)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("启动测速探针失败: %w", err)
	}
	p.mu.Lock()
	p.cmd = cmd
	p.running = true
	p.mu.Unlock()
	go p.reap(cmd)

	if err := p.waitReady(timeout); err != nil {
		p.kill()
		return fmt.Errorf("测速探针没能在 %s 内就绪（%w）", timeout, err)
	}
	// 就绪之后才认这个分组
	p.mu.Lock()
	p.groupID = groupID
	p.testURL = testURL
	p.mu.Unlock()
	log.Printf("家宽测速探针已就绪（分组 %s，控制口 %d）", groupID, ctrl)
	return nil
}

// Stop 停掉探针进程
func (p *Prober) Stop() {
	p.busy.Lock()
	defer p.busy.Unlock()
	p.kill()
}

// kill 杀掉探针进程并清空状态。调用方需持有 p.busy。
func (p *Prober) kill() {
	p.mu.Lock()
	cmd := p.cmd
	p.cmd = nil
	p.running = false
	p.groupID = ""
	p.testURL = ""
	p.mu.Unlock()
	if cmd != nil && cmd.Process != nil {
		// 不在这里 Wait，由 reap goroutine 收尸，避免重复 Wait panic
		_ = cmd.Process.Kill()
	}
}

// reap 收尸。刻意不做自愈重启：探针只是测速工具，挂了下次 Ensure 会重新拉起，
// 而自愈循环会在内存紧张的盒子上雪上加霜。
func (p *Prober) reap(cmd *exec.Cmd) {
	_ = cmd.Wait()
	p.mu.Lock()
	if p.cmd == cmd {
		p.cmd = nil
		p.running = false
	}
	p.mu.Unlock()
}

// checkEnv 探针启动前的环境检查。
func (p *Prober) checkEnv() error {
	if _, err := os.Stat(p.bin); err != nil {
		return fmt.Errorf("找不到 mihomo 内核(%s)，请先把内核文件部署到该路径", p.bin)
	}
	if _, err := os.Stat(filepath.Join(p.base, GeoIPMetaDB)); err != nil {
		return fmt.Errorf("缺少 GeoIP 库 %s，测速探针无法启动", GeoIPMetaDB)
	}
	return nil
}

// prepareDir 准备探针的工作目录，并把 geo 数据拷过去。
//
// 为什么不直接用家宽内核的 dataDir：两个 mihomo 进程会争同一份 cache.db
// 与运行配置。geo 数据是只读的，复制一份即可。
//
// 为什么用复制而不是软链：实测（Windows）`os.Symlink` 会**返回 nil 却建出一个
// 0 字节的普通文件**。mihomo 拿到空 geo 数据会直接崩，而且没有任何报错线索。
// 复制虽然多占几 MB，但行为可预期 —— 这种「静默坏掉」的坑不值得为省几 MB 去踩。
func (p *Prober) prepareDir() error {
	if err := os.MkdirAll(p.dir, 0755); err != nil {
		return err
	}
	for _, f := range []string{GeoIPMetaDB, GeoFile} {
		src := filepath.Join(p.base, f)
		si, err := os.Stat(src)
		if err != nil {
			continue // 源文件不在就不拷；mihomo 只在实际用到时才需要它
		}
		dst := filepath.Join(p.dir, f)
		// 源文件没变就跳过（geo 数据体积不小，别每次测速都拷一遍）
		if di, err := os.Stat(dst); err == nil &&
			di.Size() == si.Size() && !si.ModTime().After(di.ModTime()) {
			continue
		}
		b, err := os.ReadFile(src)
		if err != nil {
			continue
		}
		if err := os.WriteFile(dst, b, 0644); err != nil {
			return fmt.Errorf("准备探针的 %s 失败: %w", f, err)
		}
	}
	return nil
}

// waitReady 轮询等待探针的控制接口就绪
func (p *Prober) waitReady(timeout time.Duration) error {
	addr := p.ControlAddr()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := ctrlVersion(addr); err == nil {
			return nil
		}
		time.Sleep(400 * time.Millisecond)
	}
	return fmt.Errorf("等待探针控制接口就绪超时")
}

// ProxyDelay 测单个家宽节点的延迟（DelayTester 接口）
func (p *Prober) ProxyDelay(name string, timeoutMs int) (int, error) {
	p.mu.Lock()
	running, addr, testURL := p.running, p.ctrlAddrLocked(), p.testURL
	p.mu.Unlock()
	if !running {
		return 0, fmt.Errorf("测速探针没在跑")
	}
	return ctrlProxyDelay(addr, name, timeoutMs, testURL)
}

// ProxyNow 读探针里某个策略组当前选中的节点名（GroupNowTester 接口）。
func (p *Prober) ProxyNow(name string) (string, error) {
	p.mu.Lock()
	running, addr := p.running, p.ctrlAddrLocked()
	p.mu.Unlock()
	if !running {
		return "", fmt.Errorf("测速探针没在跑")
	}
	return ctrlProxyNow(addr, name)
}

// SetFront 固定探针里前置通道组的节点（FrontSwitcher 接口）。
//
// 测速时通常是探针在测（用户还连着 xray、家宽内核根本没跑），所以「换一个
// 能承载家宽链的前置」必须换到探针身上 —— 换到家宽内核上对这次测速毫无影响。
func (p *Prober) SetFront(group, name string) error {
	p.mu.Lock()
	running, addr := p.running, p.ctrlAddrLocked()
	p.mu.Unlock()
	if !running {
		return fmt.Errorf("测速探针没在跑")
	}
	return ctrlPutProxy(addr, group, name)
}

// DelaysOf 读探针缓存里这些代理的最近延迟（不触发测速，DelayReader 接口）。
func (p *Prober) DelaysOf(names []string) map[string]int {
	p.mu.Lock()
	running, addr := p.running, p.ctrlAddrLocked()
	p.mu.Unlock()
	if !running {
		return nil
	}
	return ctrlProxyDelays(addr, nameSet(names))
}

// Delays 读探针缓存里这些节点的最近延迟（不触发测速）。
// 探针里装的不是这些节点所属的分组时返回 nil。
func (p *Prober) Delays(groupID string, names []string) map[string]int {
	p.mu.Lock()
	running, group := p.running, p.groupID
	p.mu.Unlock()
	if !running || group != groupID {
		return nil
	}
	return p.DelaysOf(names)
}
