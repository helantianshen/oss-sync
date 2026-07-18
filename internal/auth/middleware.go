// Package auth 提供 Phase 3 起正式的 JWT 鉴权 + 注册/登录 API。
//
// Middleware 同时支持 Bearer JWT 与 Basic（Phase 2 占位保留）。
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

// ContextKey 是 gin 上下文中存当前用户信息的键。
// Phase 2 占位实现与本文件 Phase 3 共用，业务层调用不变。
const ContextKeyCurrentUser = "oss.current_user"

// 阶段性错误，handler.go 与 middleware.go 共用。
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

// Middleware 解析 Authorization 头，支持 Bearer JWT 与 Basic（Phase 2 占位保留）。
// 实际的解析逻辑见 handler.go 的 authenticateAny。
// 注意：此处签名与 Phase 2 不同，多了 cfg 参数。
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
