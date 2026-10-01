package main

import (
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"v2aynn-web/internal/config"
	"v2aynn-web/internal/mihomo"
	"v2aynn-web/internal/subscription"
	"v2aynn-web/internal/v2ray"
	"v2aynn-web/internal/web"
)

func main() {
	dataDir := os.Getenv("DATA_DIR")
	if dataDir == "" {
		dataDir = "/app/data"
	}
	os.MkdirAll(dataDir, 0755)

	cfg, err := config.Load(dataDir + "/config.json")
	if err != nil {
		log.Fatal(err)
	}

	v2m := v2ray.NewManager(dataDir)
	// 家宽（mihomo）内核。与 xray 共用同一个 dataDir —— mihomo 的 -d 参数
	// 会指向这里，geo 数据也要求放在该目录下。
	mhm := mihomo.NewManager(dataDir)
	srv := web.NewServer(cfg, v2m, mhm)

	// 定时拉取订阅（后台）：首轮同步拉取放在web服务启动前完成
	go subscription.Poll(cfg)

	// 合并落盘：测速结果等高频改动只标脏，由此处每5秒合并写一次，减少磁盘写入
	go func() {
		for range time.Tick(5 * time.Second) {
			cfg.Lock()
			cfg.Flush()
			cfg.Unlock()
		}
	}()

	// 开机自动启动代理。
	//
	// 内核统一之后流量一律由 mihomo 承载：普通节点由本程序生成 Clash 配置
	// （BuildNormalConfig），家宽节点直接改写订阅原文，两者落到同一个运行配置
	// 文件，由 config.Kernel（当前生效的节点类型）决定加载哪一份。
	// xray 那套仍然保留（v2m），但只在没部署 mihomo 的降级场景里才用得上。
	go func() {
		// 补齐本地还没有订阅原文的家宽分组（从旧版升级上来时会有这种：
		// 家宽地址是从老配置里迁移出来的，订阅原文得真拉过一次才有）。
		// 放后台跑：它要联网，不该拖住代理启动。
		go mhm.EnsureGroupSubs()

		cfg.Lock()
		kind := cfg.ActiveKind()
		activeNode := cfg.ActiveNode
		hasClashGroup := cfg.ClashEnabled()
		cfg.Unlock()

		if kind == config.KindClash && hasClashGroup {
			// 配置由 mihomo 内核自己准备：优先用磁盘上已有的订阅原文，
			// 本地没有才联网拉一次（见 Manager.ensureActiveConfig）。
			// 这里不重复判断「有没有配置」，免得两处逻辑各说各话。
			startWithRetry(mhm, "家宽代理")
			return
		}

		if activeNode == "" {
			// 从没选过节点（全新安装，或家宽分组被删干净了）：不擅自启动代理
			return
		}
		// 首次拉取订阅（同步），确保节点ID有效后再启动
		subscription.FetchAll(cfg)
		// 订阅刷新后节点 ID 会整批变化（ID 是按内容派生的），原来选中的节点可能
		// 已经不存在了。先把选择修正到一个真实存在的节点上并生成运行配置 ——
		// 这一步纯本地、不依赖内核，所以必须放在启动内核之前：内核文件万一没部署好
		// 起不来，配置里的选择也已经是修好的，界面不会挂着一个不存在的「当前使用中」。
		if err := mhm.PrepareNormalConfig(); err != nil {
			log.Printf("准备普通节点配置失败: %v", err)
		}
		startWithRetry(mhm, "代理")
		// 普通节点跑着的时候，把家宽测速探针也预热起来 ——
		// 用户接下来最可能做的事就是测家宽，提前拉起能省掉十几秒等待。
		// 内存不够时 WarmProbe 自己会跳过（见 ProberCanReside）。
		srv.WarmProbe()
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		addr := fmt.Sprintf("%s:%d", cfg.ListenAddr, cfg.WebPort)
		log.Printf("v2aynn-web -> %s", addr)
		log.Fatal(srv.Run(addr))
	}()

	<-sig
	v2m.Stop()
	mhm.Stop()
	// 测速探针也是个独立进程，不显式收掉会变成孤儿一直占着内存
	srv.StopProbe()
}

// kernelStarter 能被重试启动的内核
type kernelStarter interface{ Start() error }

// startWithRetry 带重试地启动内核。
// 低配盒子上启动偶发失败（端口还没释放、上游 DNS 未就绪），重试几次比直接放弃实用。
//
// 但只对**可能自愈**的错误重试。像「配置里根本挑不出可用节点」「内核文件压根没部署」
// 这种确定性失败，重试 5 次只是白等 15 秒 —— 期间代理一直是停的，用户会觉得程序卡住了。
func startWithRetry(k kernelStarter, label string) {
	const maxAttempt = 5
	for attempt := 1; attempt <= maxAttempt; attempt++ {
		err := k.Start()
		if err == nil {
			log.Printf("自动启动%s成功", label)
			return
		}
		log.Printf("自动启动%s失败(第%d次): %v", label, attempt, err)

		if errors.Is(err, v2ray.ErrNoUsableNode) || errors.Is(err, mihomo.ErrKernelMissing) {
			log.Printf("自动启动%s放弃: 这是环境/配置问题，重试不会改变结果", label)
			return
		}
		if attempt < maxAttempt {
			time.Sleep(3 * time.Second)
		}
	}
	log.Printf("自动启动%s失败: 重试%d次均未成功", label, maxAttempt)
}
