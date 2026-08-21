package main

import (
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"v2aynn-web/internal/config"
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
	srv := web.NewServer(cfg, v2m)

	// 定时拉取订阅（后台）：首轮同步拉取放在web服务启动前完成
	go subscription.Poll(cfg)

	// 开机自动启动代理：先同步拉取确保节点有效，再启动（带重试）
	go func() {
		if cfg.ActiveNode == "" {
			return
		}
		// 首次拉取订阅（同步），确保节点ID有效后再启动
		subscription.FetchAll(cfg)
		for attempt := 1; attempt <= 5; attempt++ {
			err := v2m.Start()
			if err == nil {
				log.Printf("自动启动代理成功")
				return
			}
			log.Printf("自动启动代理失败(第%d次): %v", attempt, err)
			time.Sleep(3 * time.Second)
		}
		log.Printf("自动启动代理失败: 重试5次均未成功")
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
}