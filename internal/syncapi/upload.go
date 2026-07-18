package syncapi

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/oss/oss-server/internal/auth"
	"github.com/oss/oss-server/internal/models"
)

const fallbackMaxFileSizeMB = 100

// Upload 接收 Obsidian 原始字节流，同时兼容 multipart/form-data 客户端。
func (h *Handler) Upload(c *gin.Context) {
	u, ok := auth.RequireUser(c)
	if !ok {
		return
	}

	rawUpload := strings.HasPrefix(c.GetHeader("Content-Type"), "application/octet-stream")
	path := strings.TrimSpace(c.PostForm("path"))
	mtimeText := c.PostForm("mtime")
	if rawUpload {
		path = strings.TrimSpace(c.Query("path"))
		mtimeText = c.Query("mtime")
	}
	if path == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "path is required"})
		return
	}
	if mtimeText == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "mtime is required"})
		return
	}
	var mtime int64
	if _, err := fmt.Sscanf(mtimeText, "%d", &mtime); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "mtime must be integer milliseconds"})
		return
	}
	if !isSafeRelativePath(path) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "path contains illegal segments"})
		return
	}

	maxBytes := int64(h.Cfg.Server.MaxFileSizeMB)
	if maxBytes <= 0 {
		maxBytes = fallbackMaxFileSizeMB
	}
	maxBytes *= 1 << 20
	src, declaredSize, err := uploadSource(c, rawUpload)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	defer src.Close()
	if declaredSize > maxBytes {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{
			"error": fmt.Sprintf("file size %d exceeds limit %d bytes", declaredSize, maxBytes),
		})
		return
	}

	userDir := filepath.Join(h.Cfg.Storage.DataDir, fmt.Sprintf("%d", u.ID))
	targetPath := filepath.Join(userDir, path)
	if err := os.MkdirAll(filepath.Dir(targetPath), 0o755); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "mkdir failed: " + err.Error()})
		return
	}

	tmpPath := targetPath + ".oss-tmp"
	dst, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "open target failed: " + err.Error()})
		return
	}
	hasher := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(dst, hasher), io.LimitReader(src, maxBytes+1))
	if copyErr != nil {
		closeAndRemove(dst, tmpPath)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "io.Copy failed: " + copyErr.Error()})
		return
	}
	if written > maxBytes {
		closeAndRemove(dst, tmpPath)
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "file exceeds configured size limit"})
		return
	}
	if err := dst.Close(); err != nil {
		os.Remove(tmpPath)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "close tmp failed: " + err.Error()})
		return
	}
	if err := os.Rename(tmpPath, targetPath); err != nil {
		os.Remove(tmpPath)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "rename failed: " + err.Error()})
		return
	}

	hash := hex.EncodeToString(hasher.Sum(nil))
	if err := h.upsertFile(u.ID, path, classifyFile(path), hash, mtime); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "db upsert failed: " + err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"path": path, "hash": hash, "mtime": mtime, "server_time": time.Now().UnixMilli(),
	})
}

func uploadSource(c *gin.Context, rawUpload bool) (io.ReadCloser, int64, error) {
	if rawUpload {
		return c.Request.Body, c.Request.ContentLength, nil
	}
	fileHeader, err := c.FormFile("file")
	if err != nil {
		return nil, 0, fmt.Errorf("file is required: %w", err)
	}
	src, err := fileHeader.Open()
	if err != nil {
		return nil, 0, fmt.Errorf("open uploaded file failed: %w", err)
	}
	return src, fileHeader.Size, nil
}

func (h *Handler) upsertFile(userID uint, path, fileType, hash string, mtime int64) error {
	return h.DB.Transaction(func(tx *gorm.DB) error {
		var existing models.File
		err := tx.Where("user_id = ? AND path = ?", userID, path).First(&existing).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return tx.Create(&models.File{
				UserID: userID, Path: path, Type: fileType, Hash: hash, MTime: mtime,
			}).Error
		}
		if err != nil {
			return err
		}
		return tx.Model(&models.File{}).Where("id = ?", existing.ID).Updates(map[string]any{
			"type": fileType, "hash": hash, "m_time": mtime, "is_deleted": false, "deleted_at": nil,
		}).Error
	})
}

func closeAndRemove(file *os.File, path string) {
	_ = file.Close()
	_ = os.Remove(path)
}
