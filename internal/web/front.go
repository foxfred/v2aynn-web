package web

import (
	"fmt"
	"log"
	"sort"
	"strings"

	"v2aynn-web/internal/mihomo"
)

// clashFrontSampleCount 判断「前置通道此刻能不能承载家宽链」时，抽几个家宽节点做样本。
//
// 为什么不能只看一两个：家宽节点自己掉线是常态（盒子实测一轮 62 个里 36 个挂），
// 样本太小会把「这个节点自己挂了」误判成「前置通道不通」，于是白白换掉一个本来
// 好用的前置 —— 而换前置是全局动作，代价比多等十几秒大得多。
//
// 为什么也不能看太多：每份样本都要现场建一条 OpenVPN 隧道，全都不通时每份都要
// 耗到超时。8 份 × 8 秒 ÷ 并发 4 ≈ 16 秒，是可接受的代价。
const clashFrontSampleCount = 8

// clashFrontTryMax 一轮测速里最多试几个前置候选。
//
// 每试一个都要拿真实家宽节点验一轮。找到「够用」的就立刻收工（见 frontUsable），
// 所以这个上限只在「连着试好几个都不行」时才会用到。
const clashFrontTryMax = 5

// frontMinAlive 样本里至少要有几个能连通，才算这个前置「够用」。
//
// 判据是比例（一半），不是「有没有」—— 这是盒子实测出来的教训：
//
//	前置 = 联通-01（内核自动挑的）   同一批 6 个样本通 0 个，整组 65 个只测出 6 个
//	前置 = 优选域名-01              同一批 6 个样本通 2 个
//	前置 = 优选域名-03              同一批 6 个样本通 4 个
//	前置 = 优选域名-04              同一批 6 个样本通 5 个
//
// 「勉强有 1 个通」的前置和「能承载大半样本」的前置，对用户的差别是
// 「测出 6 个节点」和「测出 50 个节点」。所以只要没到一半就换。
func frontMinAlive(n int) int {
	if n <= 0 {
		return 0
	}
	return (n + 1) / 2 // 8 个样本 → 4 个
}

// countAlive 数样本里有几个能连通（最多数到 need 个就收工）。
func countAlive(tester mihomo.DelayTester, sample []string) int {
	return mihomo.CountAlive(tester, sample, frontMinAlive(len(sample)),
		clashDelayTimeout, mihomo.ClashSweepConcurrency)
}

// spreadSample 从节点列表里等距挑 n 个，让样本尽量落在不同地区。
//
// 为什么不直接取前 n 个：订阅里节点是按地区聚堆排的（「🏠 JP-家宽-01…
// 🏠 JP-家宽-20」挨在一起），前 8 个很可能落在同一个国家、同一个 OpenVPN
// 服务端上。那一批集体掉线时会被误判成「前置通道不通」，白白换掉一个好用的前置。
// 等距取样让样本散到不同的服务端上，彼此独立得多。
func spreadSample(names []string, n int) []string {
	if n <= 0 || len(names) == 0 {
		return nil
	}
	if len(names) <= n {
		out := make([]string, len(names))
		copy(out, names)
		return out
	}
	step := len(names) / n
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, names[i*step])
	}
	return out
}

// pickFrontCandidates 给「换前置」排一个候选顺序。
//
// 排序依据是内核记录里这些 CF 节点自己的延迟（只读缓存，不触发测速）：测过的、
// 延迟低的先试，从没测过的排后面（没有证据，先别浪费一轮）。
//
// ★ 这个顺序只是「先试谁」，不是判据。实测同一个节点自己 171ms 却带不动家宽链、
// 另一个 197ms 反而能 —— 到底行不行只能拿真实家宽节点试出来（见 ensureFrontUsable）。
// 明确测过不通的（延迟 0）直接丢掉，省一轮十几秒。
//
// exclude 用来剔掉「内核此刻正在用的那个」——它已经试过了。
func pickFrontCandidates(members []string, delays map[string]int, exclude ...string) []string {
	skip := make(map[string]bool, len(exclude))
	for _, e := range exclude {
		if e != "" {
			skip[e] = true
		}
	}
	var measured, untested []string
	for _, name := range members {
		if name == "" || skip[name] {
			continue
		}
		ms, known := delays[name]
		switch {
		case known && ms > 0:
			measured = append(measured, name)
		case !known:
			untested = append(untested, name)
		}
	}
	sort.SliceStable(measured, func(i, j int) bool {
		return delays[measured[i]] < delays[measured[j]]
	})
	return append(measured, untested...)
}

// currentFront 问内核「前置组此刻在用哪个节点」。
//
// 不能拿组名去测延迟来代替这一步：`/proxies/{组}/delay` 走的是组的 fast()，而
// fast() 在组里还没有延迟历史时**无条件取成员列表的第一个节点、且不检查它死活**，
// 于是「当前节点」会变成一个说不清是谁的东西。
func currentFront(tester mihomo.DelayTester, front string) string {
	nt, ok := tester.(mihomo.GroupNowTester)
	if !ok {
		return ""
	}
	now, err := nt.ProxyNow(front)
	if err != nil {
		return ""
	}
	return now
}

// frontSwitchTookEffect 确认刚才那次「换前置」在内核里真的生效了。
//
// ★ 为什么必须确认 —— url-test 组的固定只是「偏好」，不是「强制」。
// mihomo v1.19.31 adapter/outboundgroup/urltest.go 里 fast() 的逻辑是：
//
//	if u.selected != "" {                              // u.selected = 我们固定的
//	    for _, proxy := range proxies {
//	        if !proxy.AliveForTestUrl(u.testUrl) { continue }   // 不活就跳过
//	        if proxy.Name() == u.selected { return proxy, nil }
//	    }
//	}
//	... 回退到「延迟最低的活节点」；一个活的都没有就用成员列表第一个
//
// 也就是说，被固定的节点如果在内核自己的账本里是「不活」，内核会**静默忽略**
// 我们的固定值、继续用它自己挑的那个。而那条判据（能不能直接访问订阅里写的那个
// 测速地址，一般是 gstatic）与「能不能承载一条 OpenVPN 长连接」完全是两回事 ——
// 正是这个错配造成了「一个前置选错、70 多个家宽节点一起陪葬」。
//
// 盒子实测（2026-10-01，216 个 CF 前置）：PUT 过去后大约一半的节点 now 纹丝不动，
// 它们在自己的延迟测试里返回 503。
//
// 不确认会出大问题：ensureFrontUsable 里「换上前置 c → 量样本」这一步，量到的
// 其实是内核回退后仍在用的那个节点，候选之间的比较全成了噪声。
func frontSwitchTookEffect(tester mihomo.DelayTester, front, want string) bool {
	nt, ok := tester.(mihomo.GroupNowTester)
	if !ok {
		// 后端报不出「此刻在用哪个节点」。这时只能假定切换生效 —— 把「测不了」
		// 当成「换不成」会直接把所有候选丢掉，反而更糟。
		return true
	}
	now, err := nt.ProxyNow(front)
	if err != nil {
		return true
	}
	return now == want
}

// cachedDelays 读内核缓存里这些节点的最近延迟（只读缓存，不触发测速）。
// 后端不支持就返回 nil，候选会退化成「按订阅顺序试」。
func cachedDelays(tester mihomo.DelayTester, names []string) map[string]int {
	r, ok := tester.(mihomo.DelayReader)
	if !ok {
		return nil
	}
	return r.DelaysOf(names)
}

// orUnknown 给提示语用的兜底名字。
func orUnknown(name string) string {
	if name == "" {
		return "未知"
	}
	return name
}

// ensureFrontUsable 确认家宽链的「前置通道」此刻能不能承载家宽链，不够用就换一个更好的。
//
// 返回一句给用户看的说明；返回空串表示前置本来就够用（什么都没做）。
//
// 背景 —— 盒子实测的根因（2026-10-01）：
//
//	全部家宽节点的出口都挤在前置通道组的同一个节点上。那个组在订阅里是 url-test，
//	内核按「它自己访问 gstatic 快不快」挑，而这个指标与「能不能承载一条 OpenVPN
//	长连接」毫无关系。同一批家宽节点只换前置实测：
//	    联通-09      自己 171ms（更快）→ 家宽只通 1/4
//	    优选域名-01   自己 197ms（稍慢）→ 家宽通 3/4
//	而且订阅没写 lazy、mihomo 默认 lazy=true ⇒ 前置组没有直接流量就不做健康检查，
//	选错了也永远不自己纠正（实测 10.5 小时没再检查过一次）。结果就是「一个前置
//	节点选错，70 多个家宽节点一起陪葬」，用户看到的现象是「测出来的活节点，
//	选上没一会儿就全失效了」。
//
// 所以这里不看前置节点自己的延迟，而是拿真实的家宽节点当探针，按**通过比例**
// 判断（见 frontMinAlive）：够一半就什么都不动；不够就换候选，找到更好的就固定住。
//
// ★ 换候选时必须确认那次切换真的生效了（见 frontSwitchTookEffect）：url-test 组的
// 固定只是「偏好」，被固定的节点若在内核自己那套判据下「不活」，内核会静默忽略、
// 继续用它自己挑的那个。不确认就会「换上前置 c、量到的却是别的节点」，候选之间的
// 比较全成噪声 —— 这条是盒子实测踩出来的（216 个 CF 前置里约一半 PUT 过去 now 不动）。
func (w *WebServer) ensureFrontUsable(groupID, front string, tester mihomo.DelayTester, members, sample []string) string {
	if len(sample) == 0 {
		return ""
	}
	need := frontMinAlive(len(sample))
	aliveNow := countAlive(tester, sample)
	if aliveNow >= need {
		return ""
	}

	// 前置不够用。先问内核此刻在用哪个 —— 它才是真正在扛的那一个，也是要排除掉的候选。
	cur := currentFront(tester, front)
	sw, ok := tester.(mihomo.FrontSwitcher)
	if !ok {
		return fmt.Sprintf("前置通道[%s]（当前节点[%s]）只连通了 %d/%d 个样本节点，而这次测速用的后端不支持更换前置",
			front, orUnknown(cur), aliveNow, len(sample))
	}

	cands := pickFrontCandidates(members, cachedDelays(tester, members), cur)
	tried := make([]string, 0, clashFrontTryMax)
	// refused 记下「内核根本不采用」的候选（它们在内核自己那套判据下是不活的）。
	// 单独记一份是为了能把结论如实告诉用户：这不是「这些候选更差」，而是
	// 「内核压根没用它们」—— 两者的处理建议完全不同。
	refused := make([]string, 0, clashFrontTryMax)
	best, bestAlive := cur, aliveNow
	// lastSet 记住内核此刻实际被拨到了哪个候选上，免得收尾时做一次多余的切换。
	lastSet := cur
	for _, c := range cands {
		// refused 也占名额：每个候选都要打一次 PUT + 一次查询，全都不采用时
		// 不加限制会把 200 多个成员挨个试一遍。
		if len(tried)+len(refused) >= clashFrontTryMax {
			break
		}
		if err := sw.SetFront(front, c); err != nil {
			continue
		}
		lastSet = c
		if !frontSwitchTookEffect(tester, front, c) {
			refused = append(refused, c)
			continue
		}
		n := countAlive(tester, sample)
		if n > bestAlive {
			best, bestAlive = c, n
		}
		if n >= need {
			// 够用了，立刻收工 —— 没必要把剩下的候选也试一遍
			break
		}
		tried = append(tried, fmt.Sprintf("%s(%d/%d)", c, n, len(sample)))
	}

	// 试出来的最好那个比原来强就换上并固定；否则把前置拨回原样，
	// 别留下一个「我们随手挑的」状态。
	if best != cur && bestAlive > aliveNow {
		if lastSet != best {
			_ = sw.SetFront(front, best)
		}
		w.pinFront(groupID, best)
		log.Printf("前置通道[%s]原节点[%s]只连通 %d/%d 个样本，已换成[%s]（%d/%d）并固定",
			front, cur, aliveNow, len(sample), best, bestAlive, len(sample))
		if bestAlive >= need {
			return fmt.Sprintf("前置通道[%s]原来用的节点[%s]只能连通 %d/%d 个样本节点，已自动换成[%s]（%d/%d）并固定住",
				front, orUnknown(cur), aliveNow, len(sample), best, bestAlive, len(sample))
		}
		return fmt.Sprintf("前置通道[%s]原来用的节点[%s]只能连通 %d/%d 个样本节点，已换成目前最好的[%s]（%d/%d）并固定住",
			front, orUnknown(cur), aliveNow, len(sample), best, bestAlive, len(sample))
	}
	if cur != "" && lastSet != cur {
		_ = sw.SetFront(front, cur)
	}
	if len(tried) == 0 {
		if len(refused) > 0 {
			return fmt.Sprintf("前置通道[%s]当前节点[%s]只连通了 %d/%d 个样本节点；又试了 %d 个候选（%s），内核一个都没采用 —— 内核判断前置好不好用的是「能不能直接访问订阅里写的那个测速地址」，和能不能承载家宽链是两回事，这些候选没过的是前一条。点「更新」换一批订阅再试",
				front, orUnknown(cur), aliveNow, len(sample), len(refused), strings.Join(refused, "、"))
		}
		return fmt.Sprintf("前置通道[%s]当前节点[%s]只连通了 %d/%d 个样本节点，订阅里又没有别的候选可换",
			front, orUnknown(cur), aliveNow, len(sample))
	}
	return fmt.Sprintf("前置通道[%s]当前节点[%s]只连通了 %d/%d 个样本节点，又试了 %d 个候选（%s）都没更好 —— 大概率是这批家宽节点集体掉线，点「更新」换一批再试",
		front, orUnknown(cur), aliveNow, len(sample), len(tried), strings.Join(tried, "、"))
}

// pinFront 把「验过的前置节点」记进分组配置。
//
// 为什么必须落盘：测速时通常是探针在测（用户还连着 xray，家宽内核根本没跑），
// 而用户点节点之后真正干活的是内核 —— 内核启动时会自己按 url-test 给前置组挑一个
// （判据又是错的），于是「测速时明明好好的，选上没一会儿就失效」。落盘后由
// Manager.Start 里的 applyFront 重新应用，两边用的就是同一个前置了。
func (w *WebServer) pinFront(groupID, node string) {
	if node == "" {
		return
	}
	w.cfg.Lock()
	defer w.cfg.Unlock()
	if g := w.cfg.FindClashGroup(groupID); g != nil {
		g.FrontNode = node
		_ = w.cfg.Save()
	}
}
