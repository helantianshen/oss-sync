package syncapi

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/oss/oss-server/internal/auth"
	"github.com/oss/oss-server/internal/devices"
	"github.com/oss/oss-server/internal/filestore"
	"github.com/oss/oss-server/internal/models"
	"github.com/oss/oss-server/internal/synclock"
)

const (
	defaultManifestLimit = 500
	maxManifestLimit     = 2000
)

var errRevisionConflict = errors.New("sync revision conflict")

type V2FileMeta struct {
	Path       string `json:"path"`
	Type       string `json:"type"`
	Hash       string `json:"hash"`
	Size       int64  `json:"size"`
	MTime      int64  `json:"mtime"`
	Revision   int64  `json:"revision"`
	Deleted    bool   `json:"deleted"`
	ServerTime int64  `json:"server_time,omitempty"`
}

type v2ManifestResponse struct {
	SnapshotRevision  int64        `json:"snapshot_revision"`
	CompactedRevision int64        `json:"compacted_revision"`
	NextCursor        int64        `json:"next_cursor"`
	HasMore           bool         `json:"has_more"`
	RecoverySnapshot  bool         `json:"recovery_snapshot"`
	ServerTime        int64        `json:"server_time"`
	Files             []V2FileMeta `json:"files"`
}

type v2DeleteRequest struct {
	Path         string `json:"path"`
	BaseRevision int64  `json:"base_revision"`
	ClientID     string `json:"client_id"`
	OperationID  string `json:"operation_id"`
	ClientMTime  int64  `json:"client_mtime"`
}

type v2RenameRequest struct {
	OldPath        string `json:"old_path"`
	NewPath        string `json:"new_path"`
	BaseRevision   int64  `json:"base_revision"`
	TargetRevision int64  `json:"target_revision"`
	ClientID       string `json:"client_id"`
	OperationID    string `json:"operation_id"`
	ClientMTime    int64  `json:"client_mtime"`
}

type v2RenameResponse struct {
	Old V2FileMeta `json:"old"`
	New V2FileMeta `json:"new"`
}

type v2AckRequest struct {
	ClientID string `json:"client_id"`
	Cursor   int64  `json:"cursor"`
}

type revisionSignal struct {
	mu sync.Mutex
	ch chan struct{}
}

func (h *Handler) V2Manifest(c *gin.Context) {
	h.v2ListChanges(c, true)
}

func (h *Handler) V2Changes(c *gin.Context) {
	h.v2ListChanges(c, false)
}

func (h *Handler) v2ListChanges(c *gin.Context, manifest bool) {
	u, vault, ok := h.requireV2Vault(c)
	if !ok {
		return
	}
	clientID := h.requestClientID(c, c.Query("client_id"))
	if !h.requireActiveDevice(c, u.ID, clientID) {
		return
	}
	after, err := parseInt64Default(c.Query("after"), 0)
	if err != nil || after < 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "after must be a non-negative integer"})
		return
	}
	limit, err := parseIntDefault(c.Query("limit"), defaultManifestLimit)
	if err != nil || limit <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "limit must be a positive integer"})
		return
	}
	if limit > maxManifestLimit {
		limit = maxManifestLimit
	}

	waitSeconds, err := parseIntDefault(c.Query("wait"), 0)
	if err != nil || waitSeconds < 0 || waitSeconds > 30 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "wait must be between 0 and 30 seconds"})
		return
	}
	signal := h.revisionChannel(vault.ID)
	var state models.VaultSyncState
	if err := h.DB.Where("vault_id = ?", vault.ID).First(&state).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if waitSeconds > 0 && after >= state.HeadRevision {
		timer := time.NewTimer(time.Duration(waitSeconds) * time.Second)
		defer timer.Stop()
		select {
		case <-signal:
		case <-timer.C:
		case <-c.Request.Context().Done():
			return
		}
	}

	vaultLock := h.vaultLock(vault.ID)
	vaultLock.Lock()
	defer vaultLock.Unlock()
	if !h.recordDeviceActivity(c, u.ID, vault.ID, clientID) {
		return
	}
	if err := h.DB.Where("vault_id = ?", vault.ID).First(&state).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if !manifest && after < state.CompactedRevision {
		c.JSON(http.StatusGone, gin.H{
			"error":              "sync history has been compacted; a recovery snapshot is required",
			"code":               "history_compacted",
			"compacted_revision": state.CompactedRevision,
			"head_revision":      state.HeadRevision,
			"reset_required":     true,
		})
		return
	}

	var rows []models.File
	if err := h.DB.Where(
		"user_id = ? AND vault_id = ? AND revision > ? AND revision <= ?",
		u.ID, vault.ID, after, state.HeadRevision,
	).Order("revision asc").Limit(limit + 1).Find(&rows).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	hasMore := len(rows) > limit
	if hasMore {
		rows = rows[:limit]
	}
	files := make([]V2FileMeta, 0, len(rows))
	nextCursor := after
	for _, row := range rows {
		files = append(files, v2Meta(row))
		if row.Revision > nextCursor {
			nextCursor = row.Revision
		}
	}
	if !hasMore {
		nextCursor = state.HeadRevision
	}
	c.JSON(http.StatusOK, v2ManifestResponse{
		SnapshotRevision:  state.HeadRevision,
		CompactedRevision: state.CompactedRevision,
		NextCursor:        nextCursor,
		HasMore:           hasMore,
		RecoverySnapshot:  manifest && state.CompactedRevision > 0,
		ServerTime:        time.Now().UnixMilli(),
		Files:             files,
	})
}

func (h *Handler) V2Ack(c *gin.Context) {
	u, vault, ok := h.requireV2Vault(c)
	if !ok {
		return
	}
	var req v2AckRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	req.ClientID = h.requestClientID(c, req.ClientID)
	if req.ClientID == "" || req.Cursor < 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid sync acknowledgement"})
		return
	}

	vaultLock := h.vaultLock(vault.ID)
	vaultLock.Lock()
	defer vaultLock.Unlock()
	var state models.VaultSyncState
	if err := h.DB.Where("vault_id = ?", vault.ID).First(&state).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if req.Cursor > state.HeadRevision {
		c.JSON(http.StatusBadRequest, gin.H{"error": "cursor is ahead of the vault revision"})
		return
	}
	if err := devices.Touch(
		h.DB,
		u.ID,
		vault.ID,
		req.ClientID,
		devices.DecodeDeviceName(c.GetHeader(devices.DeviceNameHeader)),
		&req.Cursor,
		time.Now(),
	); err != nil {
		h.writeDeviceError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"cursor":             req.Cursor,
		"head_revision":      state.HeadRevision,
		"compacted_revision": state.CompactedRevision,
	})
}

func (h *Handler) V2Upload(c *gin.Context) {
	u, vault, ok := h.requireV2Vault(c)
	if !ok {
		return
	}
	if !strings.HasPrefix(c.GetHeader("Content-Type"), "application/octet-stream") {
		c.JSON(http.StatusUnsupportedMediaType, gin.H{"error": "Content-Type must be application/octet-stream"})
		return
	}
	path, valid := normalizeRelativePath(c.Query("path"))
	if !valid {
		c.JSON(http.StatusBadRequest, gin.H{"error": "path contains illegal segments"})
		return
	}
	baseRevision, err := parseInt64Default(c.Query("base_revision"), 0)
	if err != nil || baseRevision < 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "base_revision must be a non-negative integer"})
		return
	}
	clientMTime, err := parseInt64Default(c.Query("mtime"), time.Now().UnixMilli())
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "mtime must be integer milliseconds"})
		return
	}
	clientID := h.requestClientID(c, c.Query("client_id"))
	operationID := cleanIdentifier(c.Query("operation_id"))
	if clientID == "" || operationID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "client_id and operation_id are required"})
		return
	}
	if !h.requireActiveDevice(c, u.ID, clientID) {
		return
	}
	declaredHash := strings.ToLower(strings.TrimSpace(c.Query("hash")))
	if declaredHash != "" && (len(declaredHash) != 64 || !isHex(declaredHash)) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "hash must be a SHA256 hex string"})
		return
	}

	maxBytes := h.maxUploadBytes()
	if c.Request.ContentLength > maxBytes {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "file exceeds configured size limit"})
		return
	}

	tmpDir := filepath.Join(h.Cfg.Storage.DataDir, "vaults", vault.ID, "tmp")
	if err := os.MkdirAll(tmpDir, 0o755); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	tmp, err := os.CreateTemp(tmpDir, "upload-*")
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	hasher := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(tmp, hasher), io.LimitReader(c.Request.Body, maxBytes+1))
	closeErr := tmp.Close()
	if copyErr != nil || closeErr != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to store upload"})
		return
	}
	if written > maxBytes {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "file exceeds configured size limit"})
		return
	}
	actualHash := hex.EncodeToString(hasher.Sum(nil))
	if declaredHash != "" && declaredHash != actualHash {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "content hash mismatch"})
		return
	}

	vaultLock := h.vaultLock(vault.ID)
	vaultLock.Lock()
	defer vaultLock.Unlock()
	if !h.recordDeviceActivity(c, u.ID, vault.ID, clientID) {
		return
	}
	pathLock := h.pathLock(vault.ID + ":" + path)
	pathLock.Lock()
	defer pathLock.Unlock()

	var result models.File
	var conflict models.File
	targetKey := filestore.VaultStorageKey(vault.ID, path)
	targetPath := filepath.Join(h.Cfg.Storage.DataDir, filepath.FromSlash(targetKey))
	var backupPath string
	moved := false

	err = h.DB.Transaction(func(tx *gorm.DB) error {
		current, exists, err := lockedFile(tx, u.ID, vault.ID, path)
		if err != nil {
			return err
		}
		if exists && current.LastWriterClientID == clientID && current.LastOperationID == operationID {
			result = current
			return nil
		}
		currentRevision := int64(0)
		if exists {
			currentRevision = current.Revision
		} else if baseRevision > 0 {
			compacted, err := vaultCompactedRevision(tx, vault.ID)
			if err != nil {
				return err
			}
			if baseRevision <= compacted {
				currentRevision = baseRevision
			}
		}
		if currentRevision != baseRevision {
			if exists {
				conflict = current
			}
			return errRevisionConflict
		}
		if exists && !current.IsDeleted && current.Hash == actualHash {
			result = current
			return nil
		}

		if err := ensureVaultQuota(tx, vault.ID, written, current, exists); err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(targetPath), 0o755); err != nil {
			return err
		}
		if _, err := os.Stat(targetPath); err == nil {
			backupPath = targetPath + ".backup-" + uuid.NewString()
			if err := os.Rename(targetPath, backupPath); err != nil {
				return err
			}
		}
		if err := os.Rename(tmpPath, targetPath); err != nil {
			if backupPath != "" {
				_ = os.Rename(backupPath, targetPath)
			}
			return err
		}
		moved = true

		revision, err := nextVaultRevision(tx, vault.ID)
		if err != nil {
			return err
		}
		now := time.Now()
		if !exists {
			current = models.File{UserID: u.ID, VaultID: vault.ID, Path: path}
		}
		current.Type = classifyFile(path)
		current.Hash = actualHash
		current.Size = written
		current.MTime = clientMTime
		current.Revision = revision
		current.IsDeleted = false
		current.DeletedAt = sql.NullTime{}
		current.StorageKey = targetKey
		current.LastWriterClientID = clientID
		current.LastOperationID = operationID
		current.UpdatedAt = now
		if exists {
			if err := tx.Save(&current).Error; err != nil {
				return err
			}
		} else if err := tx.Create(&current).Error; err != nil {
			return err
		}
		result = current
		return nil
	})
	if err != nil {
		if moved {
			_ = os.Remove(targetPath)
			if backupPath != "" {
				_ = os.Rename(backupPath, targetPath)
			}
		}
		if errors.Is(err, errRevisionConflict) {
			c.JSON(http.StatusConflict, gin.H{"error": "revision conflict", "current": v2Meta(conflict)})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if backupPath != "" {
		_ = os.Remove(backupPath)
	}
	h.notifyRevision(vault.ID)
	meta := v2Meta(result)
	meta.ServerTime = time.Now().UnixMilli()
	c.JSON(http.StatusOK, meta)
}

func (h *Handler) V2Download(c *gin.Context) {
	u, vault, ok := h.requireV2Vault(c)
	if !ok {
		return
	}
	path, valid := normalizeRelativePath(c.Query("path"))
	if !valid {
		c.JSON(http.StatusBadRequest, gin.H{"error": "path contains illegal segments"})
		return
	}
	clientID := h.requestClientID(c, c.Query("client_id"))
	if !h.requireActiveDevice(c, u.ID, clientID) {
		return
	}
	expectedRevision, err := parseInt64Default(c.Query("revision"), 0)
	if err != nil || expectedRevision < 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "revision must be a non-negative integer"})
		return
	}
	var file models.File
	if err := h.DB.Where("user_id = ? AND vault_id = ? AND path = ?", u.ID, vault.ID, path).First(&file).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "file not found"})
		return
	}
	if expectedRevision > 0 && file.Revision != expectedRevision {
		c.JSON(http.StatusConflict, gin.H{"error": "revision changed", "current": v2Meta(file)})
		return
	}
	if file.IsDeleted {
		c.JSON(http.StatusGone, gin.H{"error": "file has been deleted", "current": v2Meta(file)})
		return
	}
	abs := h.fileDiskPath(file)
	fh, err := os.Open(abs)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "file missing on disk"})
		return
	}
	defer fh.Close()
	stat, err := fh.Stat()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.Header("Content-Type", "application/octet-stream")
	c.Header("Content-Length", strconv.FormatInt(stat.Size(), 10))
	c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filepath.Base(path)))
	c.Header("ETag", `"`+file.Hash+`"`)
	c.Header("X-OSS-Hash", file.Hash)
	c.Header("X-OSS-MTime", strconv.FormatInt(file.MTime, 10))
	c.Header("X-OSS-Revision", strconv.FormatInt(file.Revision, 10))
	c.Status(http.StatusOK)
	_, _ = io.Copy(c.Writer, fh)
}

func (h *Handler) V2Delete(c *gin.Context) {
	u, vault, ok := h.requireV2Vault(c)
	if !ok {
		return
	}
	var req v2DeleteRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	var valid bool
	req.Path, valid = normalizeRelativePath(req.Path)
	req.ClientID = h.requestClientID(c, req.ClientID)
	req.OperationID = cleanIdentifier(req.OperationID)
	if !valid || req.ClientID == "" || req.OperationID == "" || req.BaseRevision < 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid delete request"})
		return
	}
	if !h.requireActiveDevice(c, u.ID, req.ClientID) {
		return
	}

	vaultLock := h.vaultLock(vault.ID)
	vaultLock.Lock()
	defer vaultLock.Unlock()
	if !h.recordDeviceActivity(c, u.ID, vault.ID, req.ClientID) {
		return
	}
	pathLock := h.pathLock(vault.ID + ":" + req.Path)
	pathLock.Lock()
	defer pathLock.Unlock()
	var result models.File
	var conflict models.File
	changed := false
	targetPath := ""
	stagedPath := ""
	err := h.DB.Transaction(func(tx *gorm.DB) error {
		current, exists, err := lockedFile(tx, u.ID, vault.ID, req.Path)
		if err != nil {
			return err
		}
		if !exists {
			compacted, err := vaultCompactedRevision(tx, vault.ID)
			if err != nil {
				return err
			}
			if req.BaseRevision > compacted {
				return gorm.ErrRecordNotFound
			}
			result = models.File{
				UserID:    u.ID,
				VaultID:   vault.ID,
				Path:      req.Path,
				Type:      classifyFile(req.Path),
				MTime:     req.ClientMTime,
				Revision:  req.BaseRevision,
				IsDeleted: true,
			}
			return nil
		}
		if current.LastWriterClientID == req.ClientID && current.LastOperationID == req.OperationID {
			result = current
			return nil
		}
		if current.Revision != req.BaseRevision {
			conflict = current
			return errRevisionConflict
		}
		if current.IsDeleted {
			result = current
			return nil
		}
		targetPath = h.fileDiskPath(current)
		stagedPath, err = stageDeletedContent(targetPath)
		if err != nil {
			return err
		}
		revision, err := nextVaultRevision(tx, vault.ID)
		if err != nil {
			return err
		}
		now := time.Now()
		current.Revision = revision
		current.IsDeleted = true
		current.DeletedAt = sql.NullTime{Time: now, Valid: true}
		current.MTime = req.ClientMTime
		current.LastWriterClientID = req.ClientID
		current.LastOperationID = req.OperationID
		if err := tx.Save(&current).Error; err != nil {
			return err
		}
		if err := tx.Model(&models.Vault{}).Where("id = ?", vault.ID).
			UpdateColumn("storage_used", gorm.Expr("CASE WHEN storage_used >= ? THEN storage_used - ? ELSE 0 END", current.Size, current.Size)).Error; err != nil {
			return err
		}
		result = current
		changed = true
		return nil
	})
	if err != nil {
		restoreStagedContent(targetPath, stagedPath)
	}
	if errors.Is(err, errRevisionConflict) {
		c.JSON(http.StatusConflict, gin.H{"error": "revision conflict", "current": v2Meta(conflict)})
		return
	}
	if errors.Is(err, gorm.ErrRecordNotFound) {
		c.JSON(http.StatusNotFound, gin.H{"error": "file not found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if targetPath == "" && result.StorageKey != "" {
		targetPath = h.fileDiskPath(result)
	}
	discardDeletedContent(targetPath, stagedPath)
	if changed {
		h.notifyRevision(vault.ID)
	}
	meta := v2Meta(result)
	meta.ServerTime = time.Now().UnixMilli()
	c.JSON(http.StatusOK, meta)
}

func (h *Handler) V2Rename(c *gin.Context) {
	u, vault, ok := h.requireV2Vault(c)
	if !ok {
		return
	}
	var req v2RenameRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	var oldValid, newValid bool
	req.OldPath, oldValid = normalizeRelativePath(req.OldPath)
	req.NewPath, newValid = normalizeRelativePath(req.NewPath)
	req.ClientID = h.requestClientID(c, req.ClientID)
	req.OperationID = cleanIdentifier(req.OperationID)
	if !oldValid || !newValid ||
		req.OldPath == req.NewPath || req.ClientID == "" || req.OperationID == "" ||
		req.BaseRevision < 0 || req.TargetRevision < 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid rename request"})
		return
	}
	if !h.requireActiveDevice(c, u.ID, req.ClientID) {
		return
	}

	vaultLock := h.vaultLock(vault.ID)
	vaultLock.Lock()
	defer vaultLock.Unlock()
	if !h.recordDeviceActivity(c, u.ID, vault.ID, req.ClientID) {
		return
	}
	unlock := h.lockTwoPaths(vault.ID+":"+req.OldPath, vault.ID+":"+req.NewPath)
	defer unlock()
	var oldResult, newResult, conflict models.File
	var conflictPath string
	oldDisk := ""
	newDisk := ""
	moved := false
	err := h.DB.Transaction(func(tx *gorm.DB) error {
		oldFile, oldExists, err := lockedFile(tx, u.ID, vault.ID, req.OldPath)
		if err != nil {
			return err
		}
		if !oldExists {
			return gorm.ErrRecordNotFound
		}
		if oldFile.LastWriterClientID == req.ClientID && oldFile.LastOperationID == req.OperationID {
			oldResult = oldFile
			if err := tx.Where("user_id = ? AND vault_id = ? AND path = ?", u.ID, vault.ID, req.NewPath).First(&newResult).Error; err != nil {
				return err
			}
			return nil
		}
		if oldFile.Revision != req.BaseRevision || oldFile.IsDeleted {
			conflict = oldFile
			conflictPath = req.OldPath
			return errRevisionConflict
		}
		target, targetExists, err := lockedFile(tx, u.ID, vault.ID, req.NewPath)
		if err != nil {
			return err
		}
		targetRevision := int64(0)
		if targetExists {
			targetRevision = target.Revision
		} else if req.TargetRevision > 0 {
			compacted, err := vaultCompactedRevision(tx, vault.ID)
			if err != nil {
				return err
			}
			if req.TargetRevision <= compacted {
				targetRevision = req.TargetRevision
			}
		}
		if targetRevision != req.TargetRevision {
			conflict = target
			conflictPath = req.NewPath
			return errRevisionConflict
		}
		if targetExists && !target.IsDeleted {
			conflict = target
			conflictPath = req.NewPath
			return errRevisionConflict
		}

		oldDisk = h.fileDiskPath(oldFile)
		newKey := filestore.VaultStorageKey(vault.ID, req.NewPath)
		newDisk = filepath.Join(h.Cfg.Storage.DataDir, filepath.FromSlash(newKey))
		if err := os.MkdirAll(filepath.Dir(newDisk), 0o755); err != nil {
			return err
		}
		if err := os.Rename(oldDisk, newDisk); err != nil {
			return err
		}
		moved = true

		oldRevision, err := nextVaultRevision(tx, vault.ID)
		if err != nil {
			return err
		}
		newRevision, err := nextVaultRevision(tx, vault.ID)
		if err != nil {
			return err
		}
		now := time.Now()
		oldFile.Revision = oldRevision
		oldFile.IsDeleted = true
		oldFile.DeletedAt = sql.NullTime{Time: now, Valid: true}
		oldFile.StorageKey = ""
		oldFile.LastWriterClientID = req.ClientID
		oldFile.LastOperationID = req.OperationID
		if err := tx.Save(&oldFile).Error; err != nil {
			return err
		}

		if !targetExists {
			target = models.File{UserID: u.ID, VaultID: vault.ID, Path: req.NewPath}
		}
		target.Type = classifyFile(req.NewPath)
		target.Hash = oldFile.Hash
		target.Size = oldFile.Size
		target.MTime = req.ClientMTime
		target.Revision = newRevision
		target.IsDeleted = false
		target.DeletedAt = sql.NullTime{}
		target.StorageKey = newKey
		target.LastWriterClientID = req.ClientID
		target.LastOperationID = req.OperationID
		if targetExists {
			if err := tx.Save(&target).Error; err != nil {
				return err
			}
		} else if err := tx.Create(&target).Error; err != nil {
			return err
		}
		oldResult = oldFile
		newResult = target
		return nil
	})
	if err != nil {
		if moved {
			_ = os.Rename(newDisk, oldDisk)
		}
		if errors.Is(err, errRevisionConflict) {
			c.JSON(http.StatusConflict, gin.H{"error": "revision conflict", "path": conflictPath, "current": v2Meta(conflict)})
			return
		}
		if errors.Is(err, gorm.ErrRecordNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "source file not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	h.notifyRevision(vault.ID)
	c.JSON(http.StatusOK, v2RenameResponse{Old: v2Meta(oldResult), New: v2Meta(newResult)})
}

func (h *Handler) requireV2Vault(c *gin.Context) (*models.User, models.Vault, bool) {
	u, ok := auth.RequireUser(c)
	if !ok {
		return nil, models.Vault{}, false
	}
	var vault models.Vault
	if err := h.DB.Where("id = ? AND owner_id = ?", c.Param("vault_id"), u.ID).First(&vault).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "vault not found"})
		return nil, models.Vault{}, false
	}
	return u, vault, true
}

func lockedFile(tx *gorm.DB, userID uint, vaultID, path string) (models.File, bool, error) {
	var file models.File
	err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("user_id = ? AND vault_id = ? AND path = ?", userID, vaultID, path).
		First(&file).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return models.File{}, false, nil
	}
	return file, err == nil, err
}

func nextVaultRevision(tx *gorm.DB, vaultID string) (int64, error) {
	var state models.VaultSyncState
	err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("vault_id = ?", vaultID).First(&state).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		state = models.VaultSyncState{VaultID: vaultID}
		if err := tx.Create(&state).Error; err != nil {
			return 0, err
		}
	} else if err != nil {
		return 0, err
	}
	state.HeadRevision++
	if err := tx.Model(&models.VaultSyncState{}).Where("vault_id = ?", vaultID).
		Updates(map[string]any{"head_revision": state.HeadRevision, "updated_at": time.Now()}).Error; err != nil {
		return 0, err
	}
	return state.HeadRevision, nil
}

func vaultCompactedRevision(tx *gorm.DB, vaultID string) (int64, error) {
	var state models.VaultSyncState
	if err := tx.Select("compacted_revision").Where("vault_id = ?", vaultID).
		First(&state).Error; err != nil {
		return 0, err
	}
	return state.CompactedRevision, nil
}

func defaultVaultID(db *gorm.DB, userID uint) (string, error) {
	var vault models.Vault
	err := db.Where("owner_id = ? AND is_default = ?", userID, true).First(&vault).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		err = db.Where("owner_id = ?", userID).Order("created_at asc").First(&vault).Error
	}
	if err != nil {
		return "", fmt.Errorf("default vault not found: %w", err)
	}
	return vault.ID, nil
}

func ensureVaultQuota(tx *gorm.DB, vaultID string, newSize int64, current models.File, exists bool) error {
	var vault models.Vault
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", vaultID).First(&vault).Error; err != nil {
		return err
	}
	oldLiveSize := int64(0)
	if exists && !current.IsDeleted {
		oldLiveSize = current.Size
	}
	nextUsed := vault.StorageUsed - oldLiveSize + newSize
	if nextUsed < 0 {
		nextUsed = 0
	}
	if vault.StorageQuota > 0 && nextUsed > vault.StorageQuota {
		return fmt.Errorf("vault storage quota exceeded")
	}
	return tx.Model(&models.Vault{}).Where("id = ?", vaultID).UpdateColumn("storage_used", nextUsed).Error
}

func (h *Handler) maxUploadBytes() int64 {
	maxMB := h.Cfg.Server.MaxFileSizeMB
	if maxMB <= 0 {
		maxMB = fallbackMaxFileSizeMB
	}
	return maxMB << 20
}

func (h *Handler) fileDiskPath(file models.File) string {
	return filestore.DiskPath(h.Cfg.Storage.DataDir, file)
}

func (h *Handler) pathLock(key string) *sync.Mutex {
	return synclock.Path(key)
}

func (h *Handler) vaultLock(vaultID string) *sync.Mutex {
	return synclock.Vault(vaultID)
}

func (h *Handler) revisionChannel(vaultID string) <-chan struct{} {
	value, _ := h.signals.LoadOrStore(vaultID, &revisionSignal{ch: make(chan struct{})})
	signal := value.(*revisionSignal)
	signal.mu.Lock()
	defer signal.mu.Unlock()
	return signal.ch
}

func (h *Handler) notifyRevision(vaultID string) {
	value, _ := h.signals.LoadOrStore(vaultID, &revisionSignal{ch: make(chan struct{})})
	signal := value.(*revisionSignal)
	signal.mu.Lock()
	close(signal.ch)
	signal.ch = make(chan struct{})
	signal.mu.Unlock()
}

func (h *Handler) lockTwoPaths(a, b string) func() {
	if a > b {
		a, b = b, a
	}
	first := h.pathLock(a)
	second := h.pathLock(b)
	first.Lock()
	second.Lock()
	return func() {
		second.Unlock()
		first.Unlock()
	}
}

func (h *Handler) requireActiveDevice(c *gin.Context, userID uint, clientID string) bool {
	if clientID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "client_id is required"})
		return false
	}
	if err := devices.CheckActive(h.DB, userID, clientID); err != nil {
		h.writeDeviceError(c, err)
		return false
	}
	return true
}

func (h *Handler) recordDeviceActivity(
	c *gin.Context,
	userID uint,
	vaultID, clientID string,
) bool {
	err := devices.Touch(
		h.DB,
		userID,
		vaultID,
		clientID,
		devices.DecodeDeviceName(c.GetHeader(devices.DeviceNameHeader)),
		nil,
		time.Now(),
	)
	if err != nil {
		h.writeDeviceError(c, err)
		return false
	}
	return true
}

func (h *Handler) writeDeviceError(c *gin.Context, err error) {
	if errors.Is(err, devices.ErrRevoked) {
		c.JSON(http.StatusForbidden, gin.H{
			"error": "this device has been revoked",
			"code":  "device_revoked",
		})
		return
	}
	c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
}

func (h *Handler) requestClientID(c *gin.Context, fallback string) string {
	if header := c.GetHeader(devices.ClientIDHeader); header != "" {
		return devices.NormalizeClientID(header)
	}
	return devices.NormalizeClientID(fallback)
}

func v2Meta(file models.File) V2FileMeta {
	return V2FileMeta{
		Path:     file.Path,
		Type:     file.Type,
		Hash:     file.Hash,
		Size:     file.Size,
		MTime:    file.MTime,
		Revision: file.Revision,
		Deleted:  file.IsDeleted,
	}
}

func parseInt64Default(raw string, fallback int64) (int64, error) {
	if raw == "" {
		return fallback, nil
	}
	return strconv.ParseInt(raw, 10, 64)
}

func parseIntDefault(raw string, fallback int) (int, error) {
	if raw == "" {
		return fallback, nil
	}
	return strconv.Atoi(raw)
}

func cleanIdentifier(value string) string {
	return devices.NormalizeClientID(value)
}

func isHex(value string) bool {
	_, err := hex.DecodeString(value)
	return err == nil
}
