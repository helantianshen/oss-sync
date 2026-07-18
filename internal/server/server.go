package server

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/oss/oss-server/internal/auth"
	"github.com/oss/oss-server/internal/blog"
	"github.com/oss/oss-server/internal/config"
	"github.com/oss/oss-server/internal/devices"
	"github.com/oss/oss-server/internal/models"
	"github.com/oss/oss-server/internal/shares"
	"github.com/oss/oss-server/internal/syncapi"
	"github.com/oss/oss-server/internal/vaults"
)

// Server 持有运行期依赖：配置、DB、磁盘根。
type Server struct {
	Cfg *config.Config
	DB  *gorm.DB
}

// New 创建 Server 实例，确保磁盘根目录存在。
func New(cfg *config.Config, db *gorm.DB) (*Server, error) {
	if err := os.MkdirAll(filepath.Join(cfg.Storage.DataDir), 0o755); err != nil {
		return nil, fmt.Errorf("创建数据目录失败: %w", err)
	}
	return &Server{Cfg: cfg, DB: db}, nil
}

// Router 构建 Gin 路由和中间件。
func (s *Server) Router() *gin.Engine {
	gin.SetMode(s.Cfg.Server.Mode)
	r := gin.New()
	r.Use(gin.Recovery())
	r.Use(gin.Logger())

	// 超出内存上限的 multipart 内容由 Gin 写入临时文件。
	r.MaxMultipartMemory = s.Cfg.Server.MaxMultipartMemoryMB << 20

	r.GET("/healthz", s.healthz)
	r.GET("/readyz", s.readyz)

	authH := auth.NewHandler(s.DB, s.Cfg)
	authH.Register(r)

	vaultsH := vaults.New(s.DB, s.Cfg)
	vaultsH.Register(r)

	devicesH := devices.New(s.DB, s.Cfg)
	devicesH.Register(r)

	syncH := syncapi.New(s.DB, s.Cfg)
	syncH.Register(r)

	blogH, err := blog.New(s.DB, s.Cfg)
	if err != nil {
		panic("blog.New: " + err.Error())
	}
	blogH.Register(r)

	sharesH := shares.New(s.DB, s.Cfg)
	sharesH.Register(r)

	return r
}

func (s *Server) healthz(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"status":  "ok",
		"env":     config.Env(),
		"version": "0.1.0",
	})
}

func (s *Server) readyz(c *gin.Context) {
	sqlDB, err := s.DB.DB()
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"ready": false, "error": err.Error()})
		return
	}
	if err := sqlDB.Ping(); err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"ready": false, "error": err.Error()})
		return
	}
	var openStorageIssues int64
	if err := s.DB.Model(&models.StorageIssue{}).
		Where("resolved_at IS NULL AND kind IN ?", []string{"missing", "hash_mismatch"}).
		Count(&openStorageIssues).Error; err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"ready": false, "error": err.Error()})
		return
	}
	if openStorageIssues > 0 {
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"ready":               false,
			"open_storage_issues": openStorageIssues,
		})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ready": true, "open_storage_issues": 0})
}
