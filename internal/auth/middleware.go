package auth

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/helantianshen/oss-sync/internal/config"
	"github.com/helantianshen/oss-sync/internal/deviceauth"
	"github.com/helantianshen/oss-sync/internal/jwt"
	"github.com/helantianshen/oss-sync/internal/models"
)

// ContextKeyCurrentUser 在 Gin 上下文中存已认证用户
const ContextKeyCurrentUser = "oss.current_user"

// ContextKeyIdentity 在 Gin 上下文中存已认证用户与可选设备绑定
const ContextKeyIdentity = "oss.auth_identity"

var (
	errNoAuth          = errors.New("missing Authorization header")
	errBadScheme       = errors.New("unsupported auth scheme")
	errBadCred         = errors.New("invalid credentials")
	errUserNotFound    = errors.New("user not found")
	errAuthUnavailable = errors.New("authentication temporarily unavailable")
)

// Identity 为认证中间件建立的请求身份
type Identity struct {
	User     *models.User
	DeviceID jwt.DeviceID
	HasDID   bool
	Claims   *jwt.Claims
}

// CurrentUser 返回已认证用户，匿名请求返回 nil
func CurrentUser(c *gin.Context) *models.User {
	v, ok := c.Get(ContextKeyCurrentUser)
	if !ok {
		return nil
	}
	u, _ := v.(*models.User)
	return u
}

// CurrentIdentity 返回完整的已认证身份，不存在时返回 nil
func CurrentIdentity(c *gin.Context) *Identity {
	v, ok := c.Get(ContextKeyIdentity)
	if !ok {
		return nil
	}
	id, _ := v.(*Identity)
	return id
}

// CurrentDeviceID 返回请求附带的设备绑定
func CurrentDeviceID(c *gin.Context) (jwt.DeviceID, bool) {
	id := CurrentIdentity(c)
	if id == nil || !id.HasDID {
		return "", false
	}
	return id.DeviceID, true
}

// RequireUser 在无已认证用户时以 401 中断请求
func RequireUser(c *gin.Context) (*models.User, bool) {
	u := CurrentUser(c)
	if u == nil {
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return nil, false
	}
	return u, true
}

// RequireAdmin 要求已认证用户为 admin；未登录返回 401，非 admin 返回 403
func RequireAdmin(c *gin.Context) (*models.User, bool) {
	u := CurrentUser(c)
	if u == nil {
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return nil, false
	}
	if u.Role != "admin" {
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "insufficient permission", "code": "not_admin"})
		return nil, false
	}
	return u, true
}

// RequireDeviceID 按令牌设备绑定校验传入的客户端 ID；缺绑定返回 device_identity_required，
// 非法或不一致返回 device_identity_mismatch 并设置 WWW-Authenticate
func RequireDeviceID(c *gin.Context, supplied ...string) (jwt.DeviceID, bool) {
	did, ok := CurrentDeviceID(c)
	if !ok {
		c.Header("WWW-Authenticate", `Bearer error="invalid_token"`)
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "unauthorized", "code": "device_identity_required"})
		return "", false
	}
	for _, s := range supplied {
		if s == "" {
			continue
		}
		normalized := deviceauth.NormalizeClientID(s)
		if normalized == "" || normalized != string(did) {
			c.Header("WWW-Authenticate", `Bearer error="invalid_token"`)
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "unauthorized", "code": "device_identity_mismatch"})
			return "", false
		}
	}
	return did, true
}

// Middleware 认证请求，失败时中断后续链路
func Middleware(db *gorm.DB, cfg *config.Config) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !Authenticate(c, db, cfg) {
			return
		}
		c.Next()
	}
}

// Authenticate 应用请求凭据但不推进 Gin 链路
func Authenticate(c *gin.Context, db *gorm.DB, cfg *config.Config) bool {
	ident, err := authenticateAny(db, cfg, c.GetHeader("Authorization"))
	if err != nil {
		abortUnauthorized(c, err)
		return false
	}
	c.Set(ContextKeyCurrentUser, ident.User)
	c.Set(ContextKeyIdentity, ident)
	return true
}

// OptionalMiddleware 仅在携带凭据时认证请求
func OptionalMiddleware(db *gorm.DB, cfg *config.Config) gin.HandlerFunc {
	return func(c *gin.Context) {
		header := c.GetHeader("Authorization")
		if header == "" {
			c.Next()
			return
		}
		ident, err := authenticateAny(db, cfg, header)
		if err != nil {
			abortUnauthorized(c, err)
			return
		}
		c.Set(ContextKeyCurrentUser, ident.User)
		c.Set(ContextKeyIdentity, ident)
		c.Next()
	}
}

// abortUnauthorized 区分凭据错误与暂时无法验证凭据。
func abortUnauthorized(c *gin.Context, err error) {
	if errors.Is(err, errAuthUnavailable) {
		c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"error": errAuthUnavailable.Error(), "code": "auth_unavailable"})
		return
	}
	body := gin.H{"error": "unauthorized: " + err.Error()}
	switch {
	case errors.Is(err, jwt.ErrExpired):
		body["code"] = "token_expired"
	case errors.Is(err, errNoAuth):
		body["code"] = "authorization_missing"
	case errors.Is(err, errBadCred), errors.Is(err, errUserNotFound):
		body["code"] = "token_invalid"
	}
	c.AbortWithStatusJSON(http.StatusUnauthorized, body)
}
