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
	"github.com/oss/oss-server/internal/filestore"
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
	path := c.PostForm("path")
	mtimeText := c.PostForm("mtime")
	if rawUpload {
		path = c.Query("path")
		mtimeText = c.Query("mtime")
	}
	path, valid := normalizeRelativePath(path)
	if !valid {
		c.JSON(http.StatusBadRequest, gin.H{"error": "path contains illegal segments"})
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

	vaultID, err := defaultVaultID(h.DB, u.ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	storageKey := filestore.VaultStorageKey(vaultID, path)
	targetPath := filepath.Join(h.Cfg.Storage.DataDir, filepath.FromSlash(storageKey))
	if err := os.MkdirAll(filepath.Dir(targetPath), 0o755); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "mkdir failed: " + err.Error()})
		return
	}

	dst, err := os.CreateTemp(filepath.Dir(targetPath), ".oss-upload-*")
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "open target failed: " + err.Error()})
		return
	}
	tmpPath := dst.Name()
	defer os.Remove(tmpPath)
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
		c.JSON(http.StatusInternalServerError, gin.H{"error": "close tmp failed: " + err.Error()})
		return
	}
	vaultLock := h.vaultLock(vaultID)
	vaultLock.Lock()
	defer vaultLock.Unlock()
	pathLock := h.pathLock(vaultID + ":" + path)
	pathLock.Lock()
	defer pathLock.Unlock()
	backupPath := tmpPath + ".backup"
	if _, err := os.Stat(targetPath); err == nil {
		if err := os.Rename(targetPath, backupPath); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "backup target failed: " + err.Error()})
			return
		}
	}
	if err := os.Rename(tmpPath, targetPath); err != nil {
		if _, backupErr := os.Stat(backupPath); backupErr == nil {
			_ = os.Rename(backupPath, targetPath)
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "rename failed: " + err.Error()})
		return
	}

	hash := hex.EncodeToString(hasher.Sum(nil))
	saved, err := h.upsertFile(u.ID, vaultID, path, classifyFile(path), hash, storageKey, mtime, written)
	if err != nil {
		_ = os.Remove(targetPath)
		if _, backupErr := os.Stat(backupPath); backupErr == nil {
			_ = os.Rename(backupPath, targetPath)
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "db upsert failed: " + err.Error()})
		return
	}
	_ = os.Remove(backupPath)
	h.notifyRevision(saved.VaultID)
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

func (h *Handler) upsertFile(
	userID uint,
	vaultID, path, fileType, hash, storageKey string,
	mtime, size int64,
) (models.File, error) {
	var saved models.File
	err := h.DB.Transaction(func(tx *gorm.DB) error {
		var existing models.File
		err := tx.Where("user_id = ? AND vault_id = ? AND path = ?", userID, vaultID, path).First(&existing).Error
		exists := err == nil
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		if err := ensureVaultQuota(tx, vaultID, size, existing, exists); err != nil {
			return err
		}
		revision, revisionErr := nextVaultRevision(tx, vaultID)
		if revisionErr != nil {
			return revisionErr
		}
		if !exists {
			saved = models.File{
				UserID: userID, VaultID: vaultID, Path: path, Type: fileType,
				Hash: hash, MTime: mtime, Size: size, Revision: revision, StorageKey: storageKey,
			}
			return tx.Create(&saved).Error
		}
		if err := tx.Model(&models.File{}).Where("id = ?", existing.ID).Updates(map[string]any{
			"type": fileType, "hash": hash, "m_time": mtime, "size": size,
			"revision": revision, "is_deleted": false, "deleted_at": nil, "storage_key": storageKey,
		}).Error; err != nil {
			return err
		}
		existing.Type = fileType
		existing.Hash = hash
		existing.MTime = mtime
		existing.Size = size
		existing.Revision = revision
		existing.IsDeleted = false
		existing.StorageKey = storageKey
		saved = existing
		return nil
	})
	return saved, err
}

func closeAndRemove(file *os.File, path string) {
	_ = file.Close()
	_ = os.Remove(path)
}
