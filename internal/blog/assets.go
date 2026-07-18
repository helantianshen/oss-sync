package blog

import (
	"embed"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/oss/oss-server/internal/markdown"
	"github.com/oss/oss-server/internal/models"
)

//go:embed assets/default/*
var defaultThemeFS embed.FS

type blogAssetResolver struct{ shareID string }

func (r blogAssetResolver) ResolveAsset(reference string) string {
	if isRemoteReference(reference) {
		return reference
	}
	escaped := strings.ReplaceAll(url.QueryEscape(reference), "+", "%20")
	escaped = strings.ReplaceAll(escaped, "%2F", "/")
	return "/assets/" + r.shareID + "?ref=" + escaped
}

func (h *Handler) handleSharedAsset(c *gin.Context) {
	var share models.Share
	if err := h.DB.Where("share_id = ?", c.Param("share_id")).First(&share).Error; err != nil {
		c.Status(http.StatusNotFound)
		return
	}
	reference := strings.TrimSpace(c.Query("ref"))
	if reference == "" || isRemoteReference(reference) || !h.shareReferencesAsset(share, reference) {
		c.Status(http.StatusNotFound)
		return
	}
	file, err := h.resolveAssetFile(share.UserID, reference)
	if err != nil {
		c.Status(http.StatusNotFound)
		return
	}
	abs := filepath.Join(h.Cfg.Storage.DataDir, fmt.Sprintf("%d", share.UserID), file.Path)
	c.File(abs)
}

func (h *Handler) shareReferencesAsset(share models.Share, reference string) bool {
	if !share.IsFolder {
		return h.markdownReferencesAsset(share.UserID, share.TargetPath, reference)
	}
	prefix := strings.TrimSuffix(share.TargetPath, "/") + "/"
	var files []models.File
	if err := h.DB.Where("user_id = ? AND path LIKE ? AND is_deleted = ? AND type = ?",
		share.UserID, prefix+"%", false, "markdown").Find(&files).Error; err != nil {
		return false
	}
	for _, file := range files {
		if h.markdownReferencesAsset(share.UserID, file.Path, reference) {
			return true
		}
	}
	return false
}

func (h *Handler) markdownReferencesAsset(userID uint, markdownPath, reference string) bool {
	abs := filepath.Join(h.Cfg.Storage.DataDir, fmt.Sprintf("%d", userID), markdownPath)
	raw, err := readFile(abs)
	if err != nil {
		return false
	}
	references, err := markdown.ReferencedAssets(string(raw))
	if err != nil {
		return false
	}
	return slices.Contains(references, reference)
}

func (h *Handler) resolveAssetFile(userID uint, reference string) (models.File, error) {
	clean := strings.TrimPrefix(path.Clean("/"+reference), "/")
	var file models.File
	if strings.Contains(clean, "/") {
		err := h.DB.Where("user_id = ? AND is_deleted = ? AND type = ? AND path = ?",
			userID, false, "attachment", clean).First(&file).Error
		return file, err
	}
	err := h.DB.Where(
		"user_id = ? AND is_deleted = ? AND type = ? AND (path = ? OR path = ? OR path LIKE ?)",
		userID, false, "attachment", clean, clean, "%/"+clean,
	).Order("m_time desc").First(&file).Error
	return file, err
}

func (h *Handler) serveDefaultTheme(c *gin.Context, filename string) bool {
	if filename != "style.css" && filename != "theme.js" {
		return false
	}
	content, err := defaultThemeFS.ReadFile("assets/default/" + filename)
	if err != nil {
		return false
	}
	contentType := "application/javascript; charset=utf-8"
	if filename == "style.css" {
		contentType = "text/css; charset=utf-8"
	}
	c.Data(http.StatusOK, contentType, content)
	return true
}

func isRemoteReference(reference string) bool {
	lower := strings.ToLower(reference)
	return strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://") || strings.HasPrefix(lower, "data:")
}
