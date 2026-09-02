// Handler 把 Updater 暴露为 /api/admin 下的 HTTP 接口。
// 所有路由都经过 auth.Middleware 与 RequireAdmin 双重守卫，仅管理员可访问。
package update

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/oss/oss-server/internal/auth"
	"github.com/oss/oss-server/internal/config"
	"github.com/oss/oss-server/internal/version"
)

// Handler 持有 update 路由依赖。
type Handler struct {
	DB  *gorm.DB
	Cfg *config.Config
	up  *Updater
	mgr *Manager
	svc *Service
}

// NewHandler 创建 update handler。
func NewHandler(db *gorm.DB, cfg *config.Config, up *Updater) *Handler {
	h := &Handler{DB: db, Cfg: cfg, up: up}
	// Manager rooted at DataDir for durable checked candidates.
	if cfg != nil && cfg.Storage.DataDir != "" && up != nil {
		if mgr, err := NewManager(cfg.Storage.DataDir); err == nil {
			h.mgr = mgr
			h.svc = NewService(mgr, up, cfg)
			if up.onUpdated != nil {
				h.svc.SetOnShutdown(up.onUpdated)
			}
		}
	}
	return h
}

// NewHandlerWithService 便于测试注入 Manager/Service。
func NewHandlerWithService(db *gorm.DB, cfg *config.Config, up *Updater, mgr *Manager, svc *Service) *Handler {
	return &Handler{DB: db, Cfg: cfg, up: up, mgr: mgr, svc: svc}
}

// Register 挂载管理员更新路由。
func (h *Handler) Register(r *gin.Engine) {
	g := r.Group("/api/admin", auth.Middleware(h.DB, h.Cfg), h.requireAdmin)
	{
		g.GET("/version", h.getVersion)
		g.GET("/update/check", h.check)
		g.GET("/update/status", h.status)
		g.POST("/update", h.trigger)
	}
}

func (h *Handler) requireAdmin(c *gin.Context) {
	if _, ok := auth.RequireAdmin(c); !ok {
		c.Abort()
	}
}

// getVersion 返回当前版本。
func (h *Handler) getVersion(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"version": version.Version,
		"env":     config.Env(),
	})
}

// check 通过选定更新源检查 latest release，严格校验后创建 durable Manager check_id。
func (h *Handler) check(c *gin.Context) {
	if h.mgr == nil || h.up == nil || h.svc == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "update service not initialized"})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()
	source := c.Query("download_source")
	if source == "" {
		source = c.Query("source")
	}
	customProxy := c.Query("download_proxy")
	if customProxy == "" {
		customProxy = c.Query("proxy")
	}
	info, err := h.svc.CheckWithSource(ctx, source, customProxy)
	if err != nil {
		code := http.StatusBadGateway
		if errors.Is(err, errRateLimited) {
			code = http.StatusTooManyRequests
		}
		if errors.Is(err, ErrNoRelease) {
			c.JSON(http.StatusOK, gin.H{"update_available": false, "note": "上游仓库暂无 Release"})
			return
		}
		if errors.Is(err, ErrInvalidURL) {
			code = http.StatusBadRequest
		}
		c.JSON(code, gin.H{"error": err.Error()})
		return
	}
	if info.CheckID == "" && !info.UpdateAvailable {
		c.JSON(http.StatusOK, gin.H{
			"update_available": false,
			"current_version":  info.CurrentVersion,
			"latest_version":   info.LatestVersion,
			"note":             info.Note,
		})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"check_id":         info.CheckID,
		"candidate":        info.Candidate,
		"current_version":  info.CurrentVersion,
		"latest_version":   info.LatestVersion,
		"update_available": info.UpdateAvailable,
		"release_url":      info.ReleaseURL,
		"expires_at":       info.ExpiresAt,
	})
}

// status 返回更新器当前状态。
func (h *Handler) status(c *gin.Context) {
	c.JSON(http.StatusOK, h.up.Status())
}

// trigger 手动触发更新。流程：使用 Manager 已校验候选下载并 helper 交接，
// 成功后异步触发关闭回调（仅 helper 启动成功后）。
func (h *Handler) trigger(c *gin.Context) {
	var req struct {
		CheckID          string `json:"check_id"`
		CheckID2         string `json:"checkId"`
		Version          string `json:"version"`
		ExpectedVersion  string `json:"expected_version"`
		ExpectedVersion2 string `json:"expectedVersion"`
		DownloadSource   string `json:"download_source"`
		DownloadProxy    string `json:"download_proxy"`
	}
	// 兼容两种命名
	if err := c.ShouldBindJSON(&req); err != nil {
		req.CheckID = c.Query("check_id")
		if req.CheckID == "" {
			req.CheckID = c.Query("checkId")
		}
		req.Version = c.Query("version")
		if req.Version == "" {
			req.Version = c.Query("expected_version")
		}
	} else {
		if req.CheckID == "" {
			req.CheckID = req.CheckID2
		}
		if req.CheckID == "" {
			req.CheckID = c.Query("check_id")
		}
		if req.Version == "" {
			req.Version = req.ExpectedVersion
		}
		if req.Version == "" {
			req.Version = req.ExpectedVersion2
		}
		if req.Version == "" {
			req.Version = c.Query("version")
			if req.Version == "" {
				req.Version = c.Query("expected_version")
			}
		}
	}
	if req.CheckID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"ok": false, "code": "check_not_found", "error": "check_id is required"})
		return
	}
	if req.Version == "" {
		c.JSON(http.StatusBadRequest, gin.H{"ok": false, "code": "invalid_version", "error": "expected version is required"})
		return
	}
	if h.svc == nil || h.mgr == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"ok": false, "code": "service_unavailable", "error": "update service not initialized"})
		return
	}
	// 严格校验 expected version 与 candidate 版本一致，防止 stale/mismatched check_id
	if cand, err := h.mgr.ValidateChecked(req.CheckID); err == nil {
		if cand.Version != req.Version {
			c.JSON(http.StatusConflict, gin.H{"ok": false, "code": "check_mismatch", "error": "version mismatch: candidate " + cand.Version + " != expected " + req.Version})
			return
		}
	} else {
		code := http.StatusBadGateway
		if errors.Is(err, ErrCheckNotFound) {
			code = http.StatusNotFound
		} else if errors.Is(err, ErrCheckExpired) {
			code = http.StatusGone
		}
		c.JSON(code, gin.H{"ok": false, "code": http.StatusText(code), "error": err.Error()})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Minute)
	defer cancel()
	op, err := h.svc.StartHelperUpdate(ctx, req.CheckID, req.DownloadSource, req.DownloadProxy)
	if err != nil {
		code := http.StatusBadGateway
		msg := err.Error()
		if errors.Is(err, ErrCheckNotFound) {
			code = http.StatusNotFound
		} else if errors.Is(err, ErrCheckExpired) {
			code = http.StatusGone
		} else if errors.Is(err, ErrAlreadyInProgress) {
			code = http.StatusConflict
		} else if errors.Is(err, ErrInvalidVersion) || errors.Is(err, ErrInvalidAsset) || errors.Is(err, ErrInvalidSize) || errors.Is(err, ErrInvalidURL) {
			code = http.StatusBadRequest
		}
		// typed unsupported platform also maps to 400
		if errors.Is(err, ErrUnsupportedPlatform) {
			code = http.StatusBadRequest
		}
		responseCode := http.StatusText(code)
		if errors.Is(err, ErrExternalUpdate) {
			code = http.StatusBadRequest
			responseCode = string(CodeExternalUpdate)
		}
		c.JSON(code, gin.H{"ok": false, "code": responseCode, "error": msg})
		return
	}
	c.JSON(http.StatusAccepted, gin.H{
		"ok":        true,
		"code":      "accepted",
		"operation": toPublic(*op),
		"version":   op.Candidate.Version,
	})
}
