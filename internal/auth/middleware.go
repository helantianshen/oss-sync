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
	"github.com/oss/oss-server/internal/models"
)

// ContextKey 是 gin 上下文中当前用户信息的键。
const ContextKeyCurrentUser = "oss.current_user"

var (
	errNoAuth       = errors.New("missing Authorization header")
	errBadScheme    = errors.New("unsupported auth scheme")
	errBadCred      = errors.New("invalid credentials")
	errUserNotFound = errors.New("user not found")
)

// CurrentUser 从 gin 上下文取出当前已认证用户。未认证返回 nil。
func CurrentUser(c *gin.Context) *models.User {
	v, ok := c.Get(ContextKeyCurrentUser)
	if !ok {
		return nil
	}
	u, _ := v.(*models.User)
	return u
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

// Middleware 解析 Authorization 头并拒绝未认证请求。
func Middleware(db *gorm.DB, cfg *config.Config) gin.HandlerFunc {
	return func(c *gin.Context) {
		user, err := authenticateAny(db, cfg, c.GetHeader("Authorization"))
		if err != nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "unauthorized: " + err.Error()})
			return
		}
		c.Set(ContextKeyCurrentUser, user)
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
		user, err := authenticateAny(db, cfg, header)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "unauthorized: " + err.Error()})
			return
		}
		c.Set(ContextKeyCurrentUser, user)
		c.Next()
	}
}
