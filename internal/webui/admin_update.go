// 管理更新
package webui

import (
	"context"
	"errors"
	"net/http"
	"runtime"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/oss/oss-server/internal/config"
	"github.com/oss/oss-server/internal/update"
	"github.com/oss/oss-server/internal/version"
)

// adminUpdateStatus 供模板与 JSON 状态接口共用。
type adminUpdateStatus struct {
	CurrentVersion string                   `json:"current_version"`
	Commit         string                   `json:"commit,omitempty"`
	BuiltAt        string                   `json:"built_at,omitempty"`
	Env            string                   `json:"env"`
	GOOS           string                   `json:"goos"`
	GOARCH         string                   `json:"goarch"`
	CapabilityOK   bool                     `json:"capability_ok"`
	CapabilityErr  string                   `json:"capability_error,omitempty"`
	ExternalUpdate bool                     `json:"external_update"`
	DownloadSource string                   `json:"download_source"`
	DownloadProxy  string                   `json:"download_proxy,omitempty"`
	Active         *update.PublicOperation  `json:"active,omitempty"`
	History        []update.PublicOperation `json:"history"`
	IsUpdating     bool                     `json:"is_updating"`
}

// buildUpdateStatus 返回当前版本与持久化操作状态（不含本地路径/GitHub 元数据）。
func (h *Handler) buildUpdateStatus() adminUpdateStatus {
	s := adminUpdateStatus{
		CurrentVersion: version.Version,
		Commit:         version.Commit,
		BuiltAt:        version.BuiltAt,
		Env:            config.Env(),
		GOOS:           runtime.GOOS,
		GOARCH:         runtime.GOARCH,
	}
	if h.Cfg != nil {
		s.DownloadSource = h.Cfg.Update.EffectiveDownloadSource()
		s.DownloadProxy = h.Cfg.Update.EffectiveDownloadProxy()
	}
	if h.updateSvc != nil && h.updateSvc.Manager() != nil {
		ms := h.updateSvc.Manager().CurrentStatus()
		s.Active = ms.Active
		s.History = ms.History
		if s.Active != nil {
			s.IsUpdating = true
		}
	} else if h.updater != nil {
		st := h.updater.Status()
		s.IsUpdating = st.UpdateInProgress
		if st.State == update.StateInProgress {
			s.IsUpdating = true
		}
	}
	if h.updater == nil {
		s.CapabilityErr = "update service not initialized"
	} else if err := update.CheckCurrentCapability(h.updater.ExecPath()); err != nil {
		s.ExternalUpdate = update.IsExternalUpdateError(err)
		if !s.ExternalUpdate {
			s.CapabilityErr = err.Error()
		}
	} else {
		s.CapabilityOK = true
	}
	return s
}

// adminUpdateStatusJSON 供前端轮询；GET /dashboard/admin/system/update/status
func (h *Handler) adminUpdateStatusJSON(c *gin.Context) {
	s := h.buildUpdateStatus()
	c.JSON(http.StatusOK, s)
}

// adminUpdateCheck 处理 POST /dashboard/admin/system/update/check
func (h *Handler) adminUpdateCheck(c *gin.Context) {
	if h.updateSvc == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"ok": false, "code": "service_unavailable", "error": h.t(c, "err.update_service_unavailable")})
		return
	}
	if st := h.buildUpdateStatus(); st.IsUpdating {
		c.JSON(http.StatusConflict, gin.H{"ok": false, "code": "already_in_progress", "error": h.t(c, "admin.update_already_in_progress")})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()
	source := strings.TrimSpace(c.PostForm("download_source"))
	if source == "" {
		source = strings.TrimSpace(c.PostForm("source"))
	}
	customProxy := strings.TrimSpace(c.PostForm("download_proxy"))
	if customProxy == "" {
		customProxy = strings.TrimSpace(c.PostForm("proxy"))
	}
	info, err := h.updateSvc.CheckWithSource(ctx, source, customProxy)
	if err != nil {
		code := http.StatusBadGateway
		if strings.Contains(strings.ToLower(err.Error()), "rate limited") || strings.Contains(err.Error(), "请求过于频繁") {
			code = http.StatusTooManyRequests
		}
		if errors.Is(err, update.ErrCheckNotFound) {
			code = http.StatusNotFound
		}
		if errors.Is(err, update.ErrInvalidURL) {
			code = http.StatusBadRequest
		}
		c.JSON(code, gin.H{"ok": false, "code": "check_failed", "error": err.Error()})
		return
	}
	if info.CheckID == "" && !info.UpdateAvailable {
		c.JSON(http.StatusOK, gin.H{
			"ok":               true,
			"code":             "no_release",
			"current_version":  info.CurrentVersion,
			"latest_version":   info.LatestVersion,
			"update_available": false,
			"note":             info.Note,
		})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"ok":               true,
		"code":             "checked",
		"check_id":         info.CheckID,
		"candidate":        info.Candidate,
		"current_version":  info.CurrentVersion,
		"latest_version":   info.LatestVersion,
		"update_available": info.UpdateAvailable,
		"release_url":      info.ReleaseURL,
		"expires_at":       info.ExpiresAt,
	})
}

// adminUpdateTrigger 处理 POST /dashboard/admin/system/update
func (h *Handler) adminUpdateTrigger(c *gin.Context) {
	if h.updateSvc == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"ok": false, "code": "service_unavailable", "error": h.t(c, "err.update_service_unavailable")})
		return
	}
	checkID := strings.TrimSpace(c.PostForm("check_id"))
	if checkID == "" {
		checkID = strings.TrimSpace(c.PostForm("checkId"))
	}
	// JSON body fallback for fetch()
	if checkID == "" && strings.Contains(c.GetHeader("Content-Type"), "application/json") {
		var j struct {
			CheckID  string `json:"check_id"`
			CheckID2 string `json:"checkId"`
		}
		_ = c.ShouldBindJSON(&j)
		if j.CheckID != "" {
			checkID = j.CheckID
		} else {
			checkID = j.CheckID2
		}
	}
	expected := strings.TrimSpace(c.PostForm("expected_version"))
	if expected == "" {
		expected = strings.TrimSpace(c.PostForm("expectedVersion"))
	}
	if expected == "" {
		expected = strings.TrimSpace(c.PostForm("version"))
	}
	if expected == "" && strings.Contains(c.GetHeader("Content-Type"), "application/json") {
		// already consumed body above; try query fallback
		expected = strings.TrimSpace(c.Query("expected_version"))
		if expected == "" {
			expected = strings.TrimSpace(c.Query("version"))
		}
	}
	downloadSource := strings.TrimSpace(c.PostForm("download_source"))
	customProxy := strings.TrimSpace(c.PostForm("download_proxy"))
	confirm := strings.TrimSpace(c.PostForm("confirm"))
	if confirm == "" {
		confirm = strings.TrimSpace(c.PostForm("_confirm"))
	}
	if confirm != "on" && confirm != "true" && confirm != "1" {
		c.JSON(http.StatusBadRequest, gin.H{"ok": false, "code": "confirm_required", "error": h.t(c, "admin.update_confirm_required")})
		return
	}
	if checkID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"ok": false, "code": "check_not_found", "error": h.t(c, "admin.update_missing_check_id")})
		return
	}
	if expected == "" {
		c.JSON(http.StatusBadRequest, gin.H{"ok": false, "code": "invalid_version", "error": h.t(c, "admin.update_missing_version")})
		return
	}
	if cand, err := h.updateSvc.Manager().ValidateChecked(checkID); err == nil {
		if cand.Version != expected {
			c.JSON(http.StatusConflict, gin.H{"ok": false, "code": "check_mismatch", "error": h.t(c, "admin.update_version_mismatch") + ": candidate " + cand.Version + " != expected " + expected})
			return
		}
	} else {
		code := http.StatusBadGateway
		if errors.Is(err, update.ErrCheckNotFound) {
			code = http.StatusNotFound
		} else if errors.Is(err, update.ErrCheckExpired) {
			code = http.StatusGone
		}
		c.JSON(code, gin.H{"ok": false, "code": "check_invalid", "error": err.Error()})
		return
	}
	if err := update.CheckCurrentCapability(h.updater.ExecPath()); err != nil {
		if update.IsExternalUpdateError(err) {
			c.JSON(http.StatusBadRequest, gin.H{"ok": false, "code": string(update.CodeExternalUpdate), "error": h.t(c, "admin.update_external_required")})
			return
		}
		c.JSON(http.StatusBadRequest, gin.H{"ok": false, "code": "capability", "error": err.Error()})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Minute)
	defer cancel()
	op, err := h.updateSvc.StartHelperUpdate(ctx, checkID, downloadSource, customProxy)
	if err != nil {
		code := http.StatusBadGateway
		msg := err.Error()
		if errors.Is(err, update.ErrCheckNotFound) {
			code = http.StatusNotFound
		} else if errors.Is(err, update.ErrCheckExpired) {
			code = http.StatusGone
		} else if errors.Is(err, update.ErrAlreadyInProgress) {
			code = http.StatusConflict
		} else if errors.Is(err, update.ErrInvalidVersion) || errors.Is(err, update.ErrInvalidAsset) || errors.Is(err, update.ErrInvalidSize) || errors.Is(err, update.ErrInvalidURL) {
			code = http.StatusBadRequest
		}
		if errors.Is(err, update.ErrUnsupportedPlatform) {
			code = http.StatusBadRequest
		}
		c.JSON(code, gin.H{"ok": false, "code": "trigger_failed", "error": msg})
		return
	}
	c.JSON(http.StatusAccepted, gin.H{
		"ok":        true,
		"code":      "accepted",
		"operation": toPublicForWeb(*op),
		"version":   op.Candidate.Version,
	})
}

func toPublicForWeb(op update.Operation) map[string]any {
	ver := ""
	if op.Candidate != nil {
		ver = op.Candidate.Version
	}
	return map[string]any{
		"id":         op.ID,
		"state":      string(op.State),
		"version":    ver,
		"started_at": op.StartedAt.Unix(),
		"updated_at": op.UpdatedAt.Unix(),
		"error":      op.Error,
	}
}
