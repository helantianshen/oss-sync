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
	"github.com/oss/oss-server/internal/shares"
	"github.com/oss/oss-server/internal/syncapi"
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

// Router 构建 Gin 路由树。Phase 1 只挂载健康检查与 readiness 探针；
// Phase 2 起追加 /api/sync/* 路由组。
func (s *Server) Router() *gin.Engine {
	gin.SetMode(s.Cfg.Server.Mode)
	r := gin.New()
	r.Use(gin.Recovery())
	r.Use(gin.Logger())

	// 决策 7.3：multipart 内存上限。超出部分自动落临时文件，不影响流式拷贝。
	r.MaxMultipartMemory = s.Cfg.Server.MaxMultipartMemoryMB << 20

	r.GET("/healthz", s.healthz)
	r.GET("/readyz", s.readyz)

	// Phase 3：挂载注册/登录 API。
	authH := auth.NewHandler(s.DB, s.Cfg)
	authH.Register(r)

	// Phase 2：挂载同步核心 API（鉴权升级为 JWT）。
	syncH := syncapi.New(s.DB, s.Cfg)
	syncH.Register(r)

	// Phase 4：挂载博客渲染路由（公开，不走 JWT）。
	blogH, err := blog.New(s.DB, s.Cfg)
	if err != nil {
		panic("blog.New: " + err.Error())
	}
	blogH.Register(r)

	// Phase 5：挂载分享管理 API（JWT 鉴权）。
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
	// 通过 ping DB 判断是否就绪
	sqlDB, err := s.DB.DB()
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"ready": false, "error": err.Error()})
		return
	}
	if err := sqlDB.Ping(); err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"ready": false, "error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ready": true})
}
