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
// 每试一个都要拿真实家宽节点验一轮（同上，最坏十几秒）。4 个足够覆盖「当前那个
// 刚好挂了、往下换几个总能碰到能用的」；换 4 个都不行的话，问题基本不在前置上，
// 而是这批家宽节点集体掉线。
const clashFrontTryMax = 4

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

// ensureFrontUsable 确认家宽链的「前置通道」此刻能不能承载家宽链，不能就换一个能用的。
//
// 返回一句给用户看的说明；返回空串表示前置本来就是好的（什么都没做）。
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
// 所以这里不看前置节点自己的延迟，而是拿真实的家宽节点当探针：有几个能连通就
// 说明前置可用；一个都连不通就换下一个候选重试。
func (w *WebServer) ensureFrontUsable(groupID, front string, tester mihomo.DelayTester, members, sample []string) string {
	if len(sample) == 0 {
		return ""
	}
	if mihomo.AnyAlive(tester, sample, clashDelayTimeout, mihomo.ClashSweepConcurrency) {
		return ""
	}

	// 前置不行。先问内核此刻在用哪个 —— 它才是真正在扛的那一个，也是要排除掉的候选。
	cur := currentFront(tester, front)
	sw, ok := tester.(mihomo.FrontSwitcher)
	if !ok {
		return fmt.Sprintf("前置通道[%s]（当前节点[%s]）承载不了家宽链，而这次测速用的后端不支持更换前置",
			front, orUnknown(cur))
	}

	cands := pickFrontCandidates(members, cachedDelays(tester, members), cur)
	tried := make([]string, 0, clashFrontTryMax)
	for _, c := range cands {
		if len(tried) >= clashFrontTryMax {
			break
		}
		if err := sw.SetFront(front, c); err != nil {
			continue
		}
		if mihomo.AnyAlive(tester, sample, clashDelayTimeout, mihomo.ClashSweepConcurrency) {
			w.pinFront(groupID, c)
			log.Printf("前置通道[%s]原节点[%s]带不动家宽链，已换成[%s]并固定", front, cur, c)
			return fmt.Sprintf("前置通道[%s]原来用的节点[%s]承载不了家宽链，已自动换成[%s]并固定住",
				front, orUnknown(cur), c)
		}
		tried = append(tried, c)
	}

	// 全都不行：把前置拨回原来那个，别留下一个「我们随手挑的」状态。
	if cur != "" {
		_ = sw.SetFront(front, cur)
	}
	if len(tried) == 0 {
		return fmt.Sprintf("前置通道[%s]当前节点[%s]承载不了家宽链，订阅里又没有别的候选可换",
			front, orUnknown(cur))
	}
	return fmt.Sprintf("前置通道[%s]当前节点[%s]承载不了家宽链，又试了 %d 个候选（%s）同样不行 —— 大概率是这批家宽节点集体掉线，点「更新」换一批再试",
		front, orUnknown(cur), len(tried), strings.Join(tried, "、"))
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
