// Package auth 提供账号端点与请求身份认证，支持 Bearer 与 Basic 两种凭据
//
//	POST /api/auth/register 按数据库策略注册账号
//	POST /api/auth/login 返回 API 令牌
package auth

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"

	"github.com/helantianshen/oss-sync/internal/config"
	"github.com/helantianshen/oss-sync/internal/deviceauth"
	"github.com/helantianshen/oss-sync/internal/jwt"
	"github.com/helantianshen/oss-sync/internal/models"
)

// Handler 持有认证路由所需的依赖
type Handler struct {
	DB            *gorm.DB
	Cfg           *config.Config
	loginLimit    *AttemptLimiter
	registerLimit *AttemptLimiter
}

// NewHandler 创建带端点限流的认证处理器
func NewHandler(db *gorm.DB, cfg *config.Config) *Handler {
	return &Handler{DB: db, Cfg: cfg, loginLimit: NewAttemptLimiter(8, time.Minute), registerLimit: NewAttemptLimiter(5, time.Minute)}
}

// Register 挂载认证与账号路由
func (h *Handler) Register(r *gin.Engine) {
	g := r.Group("/api/auth")
	{
		g.GET("/status", h.Status)
		g.POST("/register", OptionalMiddleware(h.DB, h.Cfg), h.RegisterUser)
		g.POST("/login", h.Login)
		g.GET("/device-status", Middleware(h.DB, h.Cfg), h.DeviceStatus)
	}

	accountGroup := r.Group("/api/account", Middleware(h.DB, h.Cfg))
	{
		accountGroup.POST("/password", h.ChangePassword)
	}
}

// RegisterRequest 是账号注册请求
type RegisterRequest struct {
	Username string `json:"username" binding:"required,min=3,max=64"`
	Password string `json:"password" binding:"required,min=8,max=128"`
	Role     string `json:"role"` // 可空，默认 user
}

// LoginRequest 是账号登录请求
type LoginRequest struct {
	Username string `json:"username" binding:"required"`
	Password string `json:"password" binding:"required"`
}

// AuthResponse 是登录成功后的身份信息
type AuthResponse struct {
	Token     string `json:"token"`
	ExpiresIn int64  `json:"expires_in"` // 秒
	UserID    uint   `json:"user_id"`
	Username  string `json:"username"`
	Role      string `json:"role"`
	// DeviceStatus 仅在插件设备待审批时返回
	DeviceStatus string `json:"device_status,omitempty"`
	// DeviceName 为服务端确认的设备名
	DeviceName string `json:"device_name,omitempty"`
}

// RegisterUser 处理账号创建；匿名注册只能创建普通用户，首个账号或 admin 调用除外
func (h *Handler) RegisterUser(c *gin.Context) {
	if !h.registerLimit.Allow("register:" + c.ClientIP()) {
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "too many registration attempts; try again later"})
		return
	}

	var req RegisterRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "用户名或密码不符合要求（用户名 3-64 位，密码至少 8 位且不超过 72 字节）: " + err.Error(),
		})
		return
	}
	if err := ValidateAccountInput(req.Username, req.Password); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	cur := CurrentUser(c)
	var u *models.User
	var err error
	if cur == nil {
		enabled, err := RegistrationEnabled(h.DB, h.Cfg.Auth.AllowAnonymousRegistration)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "读取注册设置失败"})
			return
		}
		if !enabled {
			c.JSON(http.StatusForbidden, gin.H{
				"error": "管理员已关闭新用户注册",
				"code":  "registration_closed",
			})
			return
		}
		// 首账号角色分配跨 Web 与 API 入口串行化
		u, err = CreateAccountForAnonymousRegistration(h.DB, req.Username, req.Password)
		if err != nil {
			if IsUsernameTakenError(err) {
				c.JSON(http.StatusConflict, gin.H{"error": "username already exists"})
			} else {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create account"})
			}
			return
		}
	} else {
		if cur.Role != "admin" {
			c.JSON(http.StatusForbidden, gin.H{
				"error": "仅 admin 可创建其他用户，当前用户 " + cur.Username + " 无权限",
				"code":  "not_admin",
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
		u, err = CreateAccount(h.DB, req.Username, req.Password, role)
		if err != nil {
			if IsUsernameTakenError(err) {
				c.JSON(http.StatusConflict, gin.H{"error": "username already exists"})
			} else {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create account"})
			}
			return
		}
	}

	token, expiresIn, err := IssueToken(h.Cfg, *u)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "sign failed: " + err.Error()})
		return
	}
	c.JSON(http.StatusOK, AuthResponse{
		Token:     token,
		ExpiresIn: expiresIn,
		UserID:    u.ID,
		Username:  u.Username,
		Role:      u.Role,
	})
}

func (h *Handler) Status(c *gin.Context) {
	var adminCount int64
	if err := h.DB.Model(&models.User{}).Where("role = ?", "admin").Count(&adminCount).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to query auth status"})
		return
	}
	enabled, err := RegistrationEnabled(h.DB, h.Cfg.Auth.AllowAnonymousRegistration)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to query registration status"})
		return
	}
	mode := "closed"
	if enabled {
		mode = "open"
	}
	c.JSON(http.StatusOK, gin.H{
		"needs_first_admin":    adminCount == 0,
		"registration_enabled": enabled,
		"registration_mode":    mode,
		"registration_url":     "/register",
		"admin_url":            "/admin",
	})
}

// Login 处理 POST /api/auth/login，设备登录时登记设备并返回绑定令牌
func (h *Handler) Login(c *gin.Context) {
	if !h.loginLimit.Allow("login:" + c.ClientIP()) {
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "too many login attempts; try again later"})
		return
	}
	var req LoginRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	rawClientID := c.GetHeader(deviceauth.ClientIDHeader)
	trimmedClientID := strings.TrimSpace(rawClientID)
	isDeviceLogin := false
	var clientID string
	if trimmedClientID != "" {
		normalized := deviceauth.NormalizeClientID(rawClientID)
		if normalized == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid client id", "code": "invalid_client_id"})
			return
		}
		isDeviceLogin = true
		clientID = normalized
	}
	u, err := AuthenticateCredentials(h.DB, req.Username, req.Password)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid credentials"})
		return
	}
	if !isDeviceLogin {
		token, expiresIn, err := IssueToken(h.Cfg, *u)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "sign failed: " + err.Error()})
			return
		}
		c.JSON(http.StatusOK, AuthResponse{
			Token:     token,
			ExpiresIn: expiresIn,
			UserID:    u.ID,
			Username:  u.Username,
			Role:      u.Role,
		})
		return
	}
	deviceName := deviceauth.DecodeDeviceName(c.GetHeader(deviceauth.DeviceNameHeader))
	status, err := deviceauth.RegisterDevice(h.DB, u.ID, clientID, deviceName, time.Now())
	if err != nil {
		if errors.Is(err, deviceauth.ErrRevoked) {
			c.JSON(http.StatusForbidden, gin.H{"error": "this device has been revoked", "code": "device_revoked"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "登记设备失败: " + err.Error()})
		return
	}
	token, expiresIn, err := IssueDeviceToken(h.Cfg, *u, jwt.DeviceID(clientID))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "sign failed: " + err.Error()})
		return
	}
	c.JSON(http.StatusOK, AuthResponse{
		Token:        token,
		ExpiresIn:    expiresIn,
		UserID:       u.ID,
		Username:     u.Username,
		Role:         u.Role,
		DeviceStatus: status,
		DeviceName:   deviceName,
	})
}

// DeviceStatus 供插件客户端轮询设备审批状态
func (h *Handler) DeviceStatus(c *gin.Context) {
	did, ok := RequireDeviceID(c, c.Query("client_id"), c.GetHeader(deviceauth.ClientIDHeader))
	if !ok {
		return
	}
	u, ok := RequireUser(c)
	if !ok {
		return
	}
	clientID := string(did)
	status, name, err := deviceauth.GetDevice(h.DB, u.ID, clientID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "device not found"})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"status":      status,
		"device_name": name,
	})
}

// changePasswordRequest 为已登录改密路由的请求体
type changePasswordRequest struct {
	OldPassword     string `json:"old_password" binding:"required"`
	NewPassword     string `json:"new_password" binding:"required"`
	ConfirmPassword string `json:"confirm_password" binding:"required"`
}

// ChangePassword 处理 POST /api/account/password，旧 token 版本失效后返回替换令牌
func (h *Handler) ChangePassword(c *gin.Context) {
	u, ok := RequireUser(c)
	if !ok {
		return
	}
	var req changePasswordRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if req.NewPassword != req.ConfirmPassword {
		c.JSON(http.StatusBadRequest, gin.H{"error": "两次输入的新密码不一致", "code": "password_mismatch"})
		return
	}
	if err := ChangePassword(h.DB, u.ID, req.OldPassword, req.NewPassword); err != nil {
		if errors.Is(err, errBadCred) {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "旧密码不正确", "code": "wrong_old_password"})
			return
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	// 重新读取用户（token 版本已递增），签发新 token 保持当前会话
	var updated models.User
	if err := h.DB.First(&updated, u.ID).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "读取用户失败"})
		return
	}
	token, expiresIn, err := IssueToken(h.Cfg, updated)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "签发新会话失败"})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"message":    "密码已更新",
		"token":      token,
		"expires_in": expiresIn,
	})
}

func authenticateAny(db *gorm.DB, cfg *config.Config, header string) (*Identity, error) {
	if header == "" {
		return nil, errNoAuth
	}
	switch {
	case strings.HasPrefix(header, "Bearer "):
		return authenticateBearerIdentity(db, cfg, header[len("Bearer "):])
	case strings.HasPrefix(header, "Basic "):
		user, pass, ok := parseBasic(header[len("Basic "):])
		if !ok || pass == "" {
			return nil, errBadCred
		}
		var u models.User
		if err := db.Where("username = ?", user).First(&u).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil, errUserNotFound
			}
			return nil, errAuthUnavailable
		}
		if u.PasswordHash == "" || bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(pass)) != nil {
			return nil, errBadCred
		}
		return &Identity{User: &u}, nil
	default:
		return nil, errBadScheme
	}
}

func authenticateBearer(db *gorm.DB, cfg *config.Config, token string) (*models.User, error) {
	ident, err := authenticateBearerIdentity(db, cfg, token)
	if err != nil {
		return nil, err
	}
	return ident.User, nil
}

func authenticateBearerIdentity(db *gorm.DB, cfg *config.Config, token string) (*Identity, error) {
	claims, err := jwt.Parse(cfg.Auth.JWTSecret, token)
	if err != nil {
		if errors.Is(err, jwt.ErrInvalidToken) || errors.Is(err, jwt.ErrExpired) {
			return nil, errors.Join(errBadCred, err)
		}
		return nil, errAuthUnavailable
	}
	if claims.DeviceID != "" {
		normalized := deviceauth.NormalizeClientID(string(claims.DeviceID))
		if normalized == "" || normalized != string(claims.DeviceID) {
			return nil, errors.Join(errBadCred, errors.New("invalid device identity"))
		}
	}
	var u models.User
	if err := db.First(&u, claims.UserID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, errUserNotFound
		}
		return nil, errAuthUnavailable
	}
	if claims.TokenVersion != u.TokenVersion {
		return nil, errBadCred
	}
	ident := &Identity{
		User:   &u,
		Claims: claims,
	}
	if claims.DeviceID != "" {
		ident.DeviceID = claims.DeviceID
		ident.HasDID = true
	}
	return ident, nil
}
