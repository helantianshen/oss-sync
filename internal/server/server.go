// Package server 提供进程级依赖与兼容入口
package server

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/helantianshen/oss-sync/internal/auth"
	"github.com/helantianshen/oss-sync/internal/config"
	"github.com/helantianshen/oss-sync/internal/serverbackup"
	"github.com/helantianshen/oss-sync/internal/serverplugin"
	"github.com/helantianshen/oss-sync/internal/transport/http/router"
	"github.com/helantianshen/oss-sync/internal/update"
)

// Server 持有进程依赖，HTTP 组装在 transport/http/router 中实现
type Server struct {
	Cfg *config.Config
	DB  *gorm.DB
	// Updater 提供 /api/admin/version 与 /api/admin/update/* 所需能力
	Updater *update.Updater
	// UpdateService 接收更新辅助进程的异步关闭信号
	UpdateService *update.Service
	// PluginManager 管理管理员安装的 WASM 与可执行扩展
	PluginManager *serverplugin.Manager
	Backups       *serverbackup.Manager
}

// New 初始化服务端依赖并确保 storage 根目录存在
func New(cfg *config.Config, db *gorm.DB) (*Server, error) {
	if err := auth.EnsureDatabaseJWTSecret(db, cfg); err != nil {
		return nil, fmt.Errorf("初始化数据库 JWT 密钥失败: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(cfg.Storage.DataDir), 0o755); err != nil {
		return nil, fmt.Errorf("创建数据目录失败: %w", err)
	}
	return &Server{Cfg: cfg, DB: db, Backups: serverbackup.New(db, cfg.Storage.DataDir, cfg.Storage.MaxTotalSizeBytes())}, nil
}

// Router 保留无错误返回的历史 API，HTTP 组装委托 router.Build 实现
// Build 失败属于启动错误，直接 panic，避免返回不完整的路由
func (s *Server) Router() *gin.Engine {
	r, err := router.Build(router.Dependencies{
		Cfg:           s.Cfg,
		DB:            s.DB,
		Updater:       s.Updater,
		UpdateService: s.UpdateService,
		PluginManager: s.PluginManager,
		Backups:       s.Backups,
	})
	if err != nil {
		panic("router.Build: " + err.Error())
	}
	return r
}
