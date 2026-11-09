// Package main 启动 OSS Sync 服务并负责进程退出
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

	"github.com/helantianshen/oss-sync/internal/auth"
	"github.com/helantianshen/oss-sync/internal/config"
	"github.com/helantianshen/oss-sync/internal/cron"
	"github.com/helantianshen/oss-sync/internal/database"
	"github.com/helantianshen/oss-sync/internal/reconcile"
	"github.com/helantianshen/oss-sync/internal/server"
	"github.com/helantianshen/oss-sync/internal/serverplugin"
	"github.com/helantianshen/oss-sync/internal/update"
	"github.com/helantianshen/oss-sync/internal/version"

	"gorm.io/gorm"
)

func main() {
	// 更新辅助进程不加载常规配置与数据库，直接执行后退出
	if ok, marker := update.IsHelperInvocation(); ok {
		code := update.RunHelper(marker)
		os.Exit(code)
	}
	// --version 命令用于校验已下载的服务端二进制版本
	if len(os.Args) > 1 && os.Args[1] == "--version" {
		fmt.Println(version.Version)
		return
	}

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("加载配置失败: %v", err)
	}

	updater, err := update.NewUpdater(cfg)
	if err != nil {
		log.Fatalf("初始化更新器失败: %v", err)
	}

	// 交接调用成功返回后请求主进程关闭，helper 等待主进程退出后执行替换
	var updateSvc *update.Service
	if mgr, err := update.NewManager(cfg.Storage.DataDir); err == nil {
		updateSvc = update.NewService(mgr, updater, cfg)
		// 校验当前操作与标记后恢复未完成的交接，覆盖写标记后崩溃且尚未启动辅助进程的情况
		if n, err := update.ResumePendingHandoffs(updater.ExecPath()); err != nil {
			log.Printf("[OSS] 恢复待处理更新失败: %v", err)
		} else if n > 0 {
			log.Printf("[OSS] 已恢复 %d 个待处理更新，等待 helper 完成", n)
		}
		// 宿主机托管更新：重启后依据 result.json 收敛活跃操作
		updateSvc.ReconcileManagedUpdate()
	} else {
		log.Printf("[OSS] 创建更新管理器失败: %v", err)
	}

	db, err := database.Init(cfg)
	if err != nil {
		log.Fatalf("初始化数据库失败: %v", err)
	}

	if err := database.AutoMigrate(db); err != nil {
		log.Fatalf("AutoMigrate 失败: %v", err)
	}
	if err := auth.EnsureRegistrationSetting(db, cfg.Auth.AllowAnonymousRegistration); err != nil {
		log.Fatalf("初始化注册设置失败: %v", err)
	}
	if _, err := reconcile.New(db, cfg).Run(true); err != nil {
		log.Printf("[OSS] 启动存储对账失败: %v", err)
	}

	pluginManager, err := serverplugin.NewManager(context.Background(), db, cfg.Storage.DataDir)
	if err != nil {
		log.Fatalf("初始化服务插件管理器失败: %v", err)
	}
	defer func() {
		if err := pluginManager.Close(context.Background()); err != nil {
			log.Printf("[OSS] 关闭服务插件管理器失败: %v", err)
		}
	}()
	if err := pluginManager.LoadEnabled(context.Background()); err != nil {
		log.Printf("[OSS] 加载服务插件失败: %v", err)
	}

	updateDone := make(chan struct{})
	if updateSvc != nil {
		updateSvc.SetOnShutdown(func() {
			select {
			case <-updateDone:
			default:
				close(updateDone)
			}
		})
	}

	srv, err := server.New(cfg, db)
	if err != nil {
		log.Fatalf("初始化 server 失败: %v", err)
	}
	srv.Updater = updater
	srv.UpdateService = updateSvc
	srv.PluginManager = pluginManager

	router := srv.Router()
	addr := fmt.Sprintf("%s:%d", cfg.Server.Host, cfg.Server.Port)
	httpSrv := &http.Server{
		Addr:    addr,
		Handler: router,
	}

	sched := cron.NewScheduler(db, cfg)
	sched.SetBackups(srv.Backups)
	sched.Register()
	if err := pluginManager.RegisterTasks(sched); err != nil {
		log.Printf("[OSS] 注册插件任务失败: %v", err)
	}
	sched.Start()

	go func() {
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("[OSS] ListenAndServe 失败: %v", err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	select {
	case <-quit:
		log.Printf("[OSS] 收到退出信号，开始优雅关闭")
	case <-updateDone:
		log.Printf("[OSS] 收到更新完成信号，开始优雅关闭")
	}
	shutdownGracefully(httpSrv, sched, db)
}

// shutdownGracefully 按定时任务、HTTP 流量、数据库的顺序停止
func shutdownGracefully(httpSrv *http.Server, sched *cron.Scheduler, db *gorm.DB) {
	stopCtx, cancelStop := context.WithTimeout(context.Background(), 5*time.Second)
	if err := sched.Stop(stopCtx); err != nil {
		log.Printf("[OSS] 停止 cron 失败: %v", err)
	}
	cancelStop()

	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 10*time.Second)
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		log.Printf("HTTP Shutdown 失败: %v", err)
	}
	cancelShutdown()

	if sqlDB, err := db.DB(); err == nil {
		_ = sqlDB.Close()
	}
}
