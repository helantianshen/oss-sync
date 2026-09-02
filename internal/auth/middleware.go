// Package auth 提供用户注册、登录和请求鉴权。
//
// Middleware 同时支持 Bearer JWT 与 Basic 认证。
// 任何 handler 用 auth.RequireUser(c) 取当前用户。
package auth

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/oss/oss-server/internal/config"
	"github.com/oss/oss-server/internal/deviceauth"
	"github.com/oss/oss-server/internal/jwt"
	"github.com/oss/oss-server/internal/models"
)

// ContextKey 是 gin 上下文中当前用户信息的键。
const ContextKeyCurrentUser = "oss.current_user"

// ContextKeyIdentity 是携带用户与可选设备身份的上下文键。
const ContextKeyIdentity = "oss.auth_identity"

var (
	errNoAuth       = errors.New("missing Authorization header")
	errBadScheme    = errors.New("unsupported auth scheme")
	errBadCred      = errors.New("invalid credentials")
	errUserNotFound = errors.New("user not found")
)

// Identity 在请求上下文中携带已认证用户与可选的设备绑定。
type Identity struct {
	User     *models.User
	DeviceID jwt.DeviceID
	HasDID   bool
	Claims   *jwt.Claims
}

// CurrentUser 从 gin 上下文取出当前已认证用户。未认证返回 nil。
func CurrentUser(c *gin.Context) *models.User {
	v, ok := c.Get(ContextKeyCurrentUser)
	if !ok {
		return nil
	}
	u, _ := v.(*models.User)
	return u
}

// CurrentIdentity 从上下文取出完整身份（用户 + 可选设备）。
func CurrentIdentity(c *gin.Context) *Identity {
	v, ok := c.Get(ContextKeyIdentity)
	if !ok {
		return nil
	}
	id, _ := v.(*Identity)
	return id
}

// CurrentDeviceID 取出当前请求的设备绑定（若有）。
func CurrentDeviceID(c *gin.Context) (jwt.DeviceID, bool) {
	id := CurrentIdentity(c)
	if id == nil || !id.HasDID {
		return "", false
	}
	return id.DeviceID, true
}

// RequireUser 守卫：未认证返 401，已认证返回 user。
func RequireUser(c *gin.Context) (*models.User, bool) {
	u := CurrentUser(c)
	if u == nil {
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return nil, false
	}
	return u, true
}

// RequireAdmin 守卫：要求当前用户是管理员。
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

// RequireDeviceID 校验当前请求的设备绑定并与 supplied 比较。
// 仅返回 claim 里的身份：缺失返回 401 device_identity_required，
// 提供的 ID 非法或不一致返回 401 device_identity_mismatch，
// 均设置 WWW-Authenticate 头。
// 每一个非空 supplied 都必须与绑定一致；空字符串被忽略，
// 无非空 supplied 时直接返回绑定。
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

// Middleware 解析 Authorization 头并拒绝未认证请求。
func Middleware(db *gorm.DB, cfg *config.Config) gin.HandlerFunc {
	return func(c *gin.Context) {
		ident, err := authenticateAny(db, cfg, c.GetHeader("Authorization"))
		if err != nil {
			abortUnauthorized(c, err)
			return
		}
		c.Set(ContextKeyCurrentUser, ident.User)
		c.Set(ContextKeyIdentity, ident)
		c.Next()
	}
}

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

func abortUnauthorized(c *gin.Context, err error) {
	body := gin.H{"error": "unauthorized: " + err.Error()}
	if errors.Is(err, jwt.ErrExpired) {
		body["code"] = "token_expired"
	}
	c.AbortWithStatusJSON(http.StatusUnauthorized, body)
}
