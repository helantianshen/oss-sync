package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/oss/oss-server/internal/config"
	"github.com/oss/oss-server/internal/cron"
	"github.com/oss/oss-server/internal/database"
	"github.com/oss/oss-server/internal/server"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("加载配置失败: %v", err)
	}
	log.Printf("[OSS] 当前环境 OSS_ENV=%s, db driver=%s", config.Env(), cfg.Database.Driver)

	db, err := database.Init(cfg)
	if err != nil {
		log.Fatalf("初始化数据库失败: %v", err)
	}

	if err := database.AutoMigrate(db); err != nil {
		log.Fatalf("AutoMigrate 失败: %v", err)
	}

	// Phase 3：首个 admin 通过 POST /api/auth/register（dev 环境空表放行）创建。
	// 旧 SeedDevUser 已移除。

	srv, err := server.New(cfg, db)
	if err != nil {
		log.Fatalf("初始化 server 失败: %v", err)
	}

	router := srv.Router()
	addr := fmt.Sprintf("%s:%d", cfg.Server.Host, cfg.Server.Port)
	httpSrv := &http.Server{
		Addr:    addr,
		Handler: router,
	}

	// Phase 6：定时任务（回收站清理 + 孤儿附件清理，每日 03:00）。
	sched := cron.NewScheduler(db, cfg)
	sched.Register()
	sched.Start()
	defer func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = sched.Stop(stopCtx)
	}()

	go func() {
		log.Printf("[OSS] HTTP 监听 %s", addr)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("ListenAndServe 失败: %v", err)
		}
	}()

	// 优雅关闭
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	log.Println("[OSS] 收到退出信号，正在优雅关闭...")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(ctx); err != nil {
		log.Fatalf("HTTP Shutdown 失败: %v", err)
	}
	if sqlDB, err := db.DB(); err == nil {
		_ = sqlDB.Close()
	}
	log.Println("[OSS] 已退出")
}
