// Package router 从功能处理器组装 HTTP 传输层
// 端点行为归各功能包所有，本包负责注册顺序、进程级中间件与健康检查
package router

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/helantianshen/oss-sync/internal/admin"
	"github.com/helantianshen/oss-sync/internal/auth"
	"github.com/helantianshen/oss-sync/internal/blog"
	"github.com/helantianshen/oss-sync/internal/config"
	"github.com/helantianshen/oss-sync/internal/devices"
	"github.com/helantianshen/oss-sync/internal/models"
	"github.com/helantianshen/oss-sync/internal/serverbackup"
	"github.com/helantianshen/oss-sync/internal/serverplugin"
	"github.com/helantianshen/oss-sync/internal/shares"
	"github.com/helantianshen/oss-sync/internal/syncapi"
	"github.com/helantianshen/oss-sync/internal/transport/http/middleware"
	"github.com/helantianshen/oss-sync/internal/update"
	"github.com/helantianshen/oss-sync/internal/vaults"
	"github.com/helantianshen/oss-sync/internal/version"
	"github.com/helantianshen/oss-sync/internal/webui"
)

// Dependencies 声明路由组装所需的运行时服务，显式边界避免处理器直接触及进程入口
type Dependencies struct {
	Cfg           *config.Config
	DB            *gorm.DB
	Updater       *update.Updater
	UpdateService *update.Service
	PluginManager *serverplugin.Manager
	Backups       *serverbackup.Manager
}

// Build 创建 Gin 引擎，安装进程级中间件并注册启用的功能路由
func Build(deps Dependencies) (*gin.Engine, error) {
	if deps.Cfg == nil {
		return nil, fmt.Errorf("router: nil config")
	}
	if deps.DB == nil {
		return nil, fmt.Errorf("router: nil database")
	}

	gin.SetMode(deps.Cfg.Server.Mode)
	r := gin.New()
	r.Use(gin.Recovery())
	r.Use(middleware.AccessLogger())
	if deps.Backups == nil {
		deps.Backups = serverbackup.New(deps.DB, deps.Cfg.Storage.DataDir, deps.Cfg.Storage.MaxTotalSizeBytes())
	}
	r.Use(backupGate(deps.Backups))
	if deps.PluginManager != nil {
		r.Use(deps.PluginManager.Middleware(deps.Cfg))
	}

	// Gin 将大于此值的 multipart 请求体写入临时文件
	r.MaxMultipartMemory = deps.Cfg.Server.MaxMultipartMemoryMB << 20
	registerHealthRoutes(r, deps.DB)

	authH := auth.NewHandler(deps.DB, deps.Cfg)
	authH.Register(r)

	adminH := admin.New(deps.DB, deps.Cfg)
	adminH.Register(r)

	webH, err := webui.New(deps.DB, deps.Cfg)
	if err != nil {
		return nil, fmt.Errorf("webui.New: %w", err)
	}
	if deps.UpdateService != nil && deps.Updater != nil {
		webH.SetUpdateService(deps.UpdateService, deps.Updater)
	}
	if deps.PluginManager != nil {
		webH.SetPluginManager(deps.PluginManager)
	}
	webH.SetBackups(deps.Backups)
	webH.Register(r)

	vaultsH := vaults.New(deps.DB, deps.Cfg)
	vaultsH.Register(r)

	devicesH := devices.New(deps.DB, deps.Cfg)
	devicesH.Register(r)

	syncH := syncapi.New(deps.DB, deps.Cfg)
	syncH.Register(r)
	webH.SetFileWriter(syncH)

	blogH, err := blog.New(deps.DB, deps.Cfg)
	if err != nil {
		return nil, fmt.Errorf("blog.New: %w", err)
	}
	if deps.PluginManager != nil {
		deps.PluginManager.SetFileWriter(syncH)
		blogH.SetPluginHooks(deps.PluginManager)
		blogH.SetPluginDataHooks(deps.PluginManager)
		// 内置主题的声明键由 serverplugin 提供；该包已依赖 blog，不能反向引用
		blogH.SetBuiltinSettingKeysProvider(func() map[string]struct{} {
			keys := make(map[string]struct{}, 16)
			for _, manifest := range serverplugin.BuiltinManifests() {
				for _, field := range manifest.Settings {
					if field.Key != "" {
						keys[field.Key] = struct{}{}
					}
				}
			}
			return keys
		})
	}
	blogH.Register(r)

	sharesH := shares.New(deps.DB, deps.Cfg)
	sharesH.Register(r)

	if deps.Updater != nil {
		var updateH *update.Handler
		if deps.UpdateService != nil {
			updateH = update.NewHandlerWithService(deps.DB, deps.Cfg, deps.Updater, deps.UpdateService.Manager(), deps.UpdateService)
		} else {
			updateH = update.NewHandler(deps.DB, deps.Cfg, deps.Updater)
		}
		updateH.Register(r)
	}
	if deps.PluginManager != nil {
		deps.PluginManager.RegisterRoutes(r, deps.Cfg)
	}

	return r, nil
}

func backupGate(backups *serverbackup.Manager) gin.HandlerFunc {
	return func(c *gin.Context) {
		route := c.FullPath()
		if route == "/dashboard/admin/server-backups/import" {
			c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, serverbackup.MaxUploadBytes+(1<<20))
		}
		if strings.HasPrefix(route, "/dashboard/admin/server-backups") ||
			strings.HasSuffix(route, "/collaborations/stream") || route == "/healthz" {
			c.Next()
			return
		}
		if !backups.TryRead() {
			c.Header("Retry-After", "5")
			c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"error": "server backup or restore is in progress"})
			return
		}
		defer backups.EndRead()
		c.Next()
	}
}

// registerHealthRoutes 在功能路由注册前暴露存活与就绪检查
func registerHealthRoutes(r *gin.Engine, db *gorm.DB) {
	r.GET("/healthz", healthz)
	r.GET("/readyz", readyz(db))
}

func healthz(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"status":  "ok",
		"env":     config.Env(),
		"version": version.Version,
	})
}

func readyz(db *gorm.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		sqlDB, err := db.DB()
		if err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"ready": false, "version": version.Version, "error": err.Error()})
			return
		}
		if err := sqlDB.Ping(); err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"ready": false, "version": version.Version, "error": err.Error()})
			return
		}

		var openStorageIssues int64
		if err := db.Model(&models.StorageIssue{}).
			Where("resolved_at IS NULL AND kind IN ?", []string{"missing", "hash_mismatch"}).
			Count(&openStorageIssues).Error; err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"ready": false, "version": version.Version, "error": err.Error()})
			return
		}
		if openStorageIssues > 0 {
			c.JSON(http.StatusServiceUnavailable, gin.H{
				"ready":               false,
				"version":             version.Version,
				"open_storage_issues": openStorageIssues,
			})
			return
		}
		c.JSON(http.StatusOK, gin.H{"ready": true, "version": version.Version, "open_storage_issues": 0})
	}
}
