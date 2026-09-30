package main

import (
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

	// 开机自动启动代理：按上次使用的内核决定启哪个（两个内核抢同一组端口，
	// 只能起一个）。普通节点走 xray，家宽节点走 mihomo。
	go func() {
		cfg.Lock()
		kernel := cfg.CurrentKernel()
		activeNode := cfg.ActiveNode
		subURL := cfg.ClashSubURL
		subProxy := cfg.SubProxy
		cfg.Unlock()

		if kernel == config.KernelMihomo && subURL != "" {
			// 本地还没有家宽配置时先拉一次；已有就直接启动，
			// 免得每次开机都白等一次网络请求。
			if !mhm.HasConfig() {
				if _, err := mhm.FetchSubscription(subURL, subProxy); err != nil {
					log.Printf("开机拉取家宽订阅失败: %v", err)
				}
			}
			startWithRetry(mhm, "家宽内核")
			return
		}

		if activeNode == "" {
			return
		}
		// 首次拉取订阅（同步），确保节点ID有效后再启动
		subscription.FetchAll(cfg)
		startWithRetry(v2m, "代理")
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
}

// kernelStarter 能被重试启动的内核
type kernelStarter interface{ Start() error }

// startWithRetry 带重试地启动内核。
// 低配盒子上启动偶发失败（端口还没释放、上游 DNS 未就绪），重试几次比直接放弃实用。
func startWithRetry(k kernelStarter, label string) {
	for attempt := 1; attempt <= 5; attempt++ {
		if err := k.Start(); err == nil {
			log.Printf("自动启动%s成功", label)
			return
		} else {
			log.Printf("自动启动%s失败(第%d次): %v", label, attempt, err)
		}
		time.Sleep(3 * time.Second)
	}
	log.Printf("自动启动%s失败: 重试5次均未成功", label)
}
