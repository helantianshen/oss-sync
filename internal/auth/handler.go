// Package auth 提供 Phase 3 起正式的 JWT 鉴权 + 注册/登录 API。
//
//	POST /api/auth/register  注册（prod 默认关闭，仅 admin 可调用，或 dev 开放）
//	POST /api/auth/login      登录，返回 JWT
//
// Middleware 同时支持 Bearer JWT 与 Basic（Phase 2 占位保留）。
// 任何 handler 用 auth.RequireUser(c) 取当前用户。
package auth

import (
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"

	"github.com/oss/oss-server/internal/config"
	"github.com/oss/oss-server/internal/jwt"
	"github.com/oss/oss-server/internal/models"
)

// Handler 持有 auth 路由所需依赖。
type Handler struct {
	DB         *gorm.DB
	Cfg        *config.Config
	registerMu sync.Mutex
}

// NewHandler 创建 auth handler。
func NewHandler(db *gorm.DB, cfg *config.Config) *Handler {
	return &Handler{DB: db, Cfg: cfg}
}

// Register 在 gin 引擎上挂载 auth 路由组。
func (h *Handler) Register(r *gin.Engine) {
	g := r.Group("/api/auth")
	{
		g.GET("/status", h.Status)
		g.POST("/register", OptionalMiddleware(h.DB, h.Cfg), h.RegisterUser)
		g.POST("/login", h.Login)
	}
}

// --- DTO ---

type RegisterRequest struct {
	Username string `json:"username" binding:"required,min=3,max=64"`
	Password string `json:"password" binding:"required,min=8,max=128"`
	Role     string `json:"role"` // 可空，默认 user
}

type LoginRequest struct {
	Username string `json:"username" binding:"required"`
	Password string `json:"password" binding:"required"`
}

type AuthResponse struct {
	Token     string `json:"token"`
	ExpiresIn int64  `json:"expires_in"` // 秒
	UserID    uint   `json:"user_id"`
	Username  string `json:"username"`
	Role      string `json:"role"`
}

// RegisterUser 处理 POST /api/auth/register。
// 生产环境默认要求已有 admin 身份（防止任意注册）。
// dev 环境 + users 表为空时允许首次注册（即建首个 admin）。
func (h *Handler) RegisterUser(c *gin.Context) {
	h.registerMu.Lock()
	defer h.registerMu.Unlock()

	var req RegisterRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "用户名或密码不符合要求（用户名 3-64 位，密码 8-128 位）: " + err.Error(),
		})
		return
	}
	role := strings.ToLower(req.Role)
	if role == "" {
		role = "user"
	}
	if role != "admin" && role != "user" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "role 必须为 admin 或 user"})
		return
	}

	allowFirstAdmin := false
	if config.Env() == "dev" {
		var n int64
		h.DB.Model(&models.User{}).Count(&n)
		if n == 0 {
			allowFirstAdmin = true
			role = "admin"
		}
	}
	if !allowFirstAdmin {
		cur := CurrentUser(c)
		if cur == nil {
			c.JSON(http.StatusUnauthorized, gin.H{
				"error":      "首个 admin 已存在，匿名注册已关闭。请先用 admin 登录后再注册新用户",
				"code":       "first_admin_exists",
				"hint":       "login_first",
				"need_admin": true,
			})
			return
		}
		if cur.Role != "admin" {
			c.JSON(http.StatusForbidden, gin.H{
				"error": "仅 admin 可注册新用户，当前用户 " + cur.Username + " 无权限",
				"code":  "not_admin",
			})
			return
		}
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "hash failed: " + err.Error()})
		return
	}
	u := models.User{
		Username:     req.Username,
		PasswordHash: string(hash),
		Role:         role,
		StorageQuota: 0,
	}
	if err := h.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&u).Error; err != nil {
			return err
		}
		return tx.Create(&models.UserSetting{UserID: u.ID}).Error
	}); err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": "username already exists"})
		return
	}

	token, err := jwt.Sign(h.Cfg.Auth.JWTSecret, jwt.Claims{
		UserID:   u.ID,
		Username: u.Username,
		Role:     u.Role,
	}, time.Duration(h.Cfg.Auth.JWTTTLHours)*time.Hour)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "sign failed: " + err.Error()})
		return
	}
	c.JSON(http.StatusOK, AuthResponse{
		Token:     token,
		ExpiresIn: int64(h.Cfg.Auth.JWTTTLHours) * 3600,
		UserID:    u.ID,
		Username:  u.Username,
		Role:      u.Role,
	})
}

func (h *Handler) Status(c *gin.Context) {
	var count int64
	if err := h.DB.Model(&models.User{}).Count(&count).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to query auth status"})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"needs_first_admin": config.Env() == "dev" && count == 0,
		"registration_mode": registrationMode(count),
	})
}

func registrationMode(userCount int64) string {
	if config.Env() == "dev" && userCount == 0 {
		return "first_admin"
	}
	return "admin_only"
}

// Login 处理 POST /api/auth/login。
func (h *Handler) Login(c *gin.Context) {
	var req LoginRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	var u models.User
	if err := h.DB.Where("username = ?", req.Username).First(&u).Error; err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid credentials"})
		return
	}
	if u.PasswordHash == "" || bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(req.Password)) != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid credentials"})
		return
	}
	token, err := jwt.Sign(h.Cfg.Auth.JWTSecret, jwt.Claims{
		UserID:   u.ID,
		Username: u.Username,
		Role:     u.Role,
	}, time.Duration(h.Cfg.Auth.JWTTTLHours)*time.Hour)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "sign failed: " + err.Error()})
		return
	}
	c.JSON(http.StatusOK, AuthResponse{
		Token:     token,
		ExpiresIn: int64(h.Cfg.Auth.JWTTTLHours) * 3600,
		UserID:    u.ID,
		Username:  u.Username,
		Role:      u.Role,
	})
}

// --- Middleware ---
//
// Middleware 实现见 middleware.go，这里只保留依赖说明：
// authenticateAny 同时支持 Bearer JWT 与 Basic（Phase 2 占位保留）。
// 任何 handler 用 auth.RequireUser(c) 取当前用户。

func authenticateAny(db *gorm.DB, cfg *config.Config, header string) (*models.User, error) {
	if header == "" {
		return nil, errNoAuth
	}
	switch {
	case strings.HasPrefix(header, "Bearer "):
		return authenticateBearer(db, cfg, header[len("Bearer "):])
	case strings.HasPrefix(header, "Basic "):
		user, pass, ok := parseBasic(header[len("Basic "):])
		if !ok || pass == "" {
			return nil, errBadCred
		}
		var u models.User
		if err := db.Where("username = ?", user).First(&u).Error; err != nil {
			return nil, errUserNotFound
		}
		if u.PasswordHash == "" || bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(pass)) != nil {
			return nil, errBadCred
		}
		return &u, nil
	default:
		return nil, errBadScheme
	}
}

func authenticateBearer(db *gorm.DB, cfg *config.Config, token string) (*models.User, error) {
	claims, err := jwt.Parse(cfg.Auth.JWTSecret, token)
	if err != nil {
		return nil, errors.Join(errBadCred, err)
	}
	var u models.User
	if err := db.First(&u, claims.UserID).Error; err != nil {
		return nil, errUserNotFound
	}
	return &u, nil
}
