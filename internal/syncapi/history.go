package syncapi

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/helantianshen/oss-sync/internal/auth"
	"github.com/helantianshen/oss-sync/internal/collaboration"
	"github.com/helantianshen/oss-sync/internal/deviceauth"
	"github.com/helantianshen/oss-sync/internal/filestore"
	"github.com/helantianshen/oss-sync/internal/history"
	"github.com/helantianshen/oss-sync/internal/jwt"
	"github.com/helantianshen/oss-sync/internal/models"
	"github.com/helantianshen/oss-sync/internal/recycle"
	"github.com/helantianshen/oss-sync/internal/storagequota"
	"github.com/helantianshen/oss-sync/internal/vaultaccess"
)

func hashBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// requireVaultActor 解析当前用户与仓库角色
// 插件请求携带 client_id 时必须通过设备仓库授权；网页请求只需用户 Vault 权限
// 管理员即使不是仓库成员也以 admin 角色获得审计访问
func (h *Handler) requireVaultActor(c *gin.Context) (*models.User, models.Vault, string, bool) {
	u, ok := auth.RequireUser(c)
	if !ok {
		return nil, models.Vault{}, "", false
	}
	vault, role, err := vaultaccess.Resolve(h.DB, u.ID, c.Param("vault_id"))
	if err != nil {
		if u.Role == "admin" {
			if err := h.DB.Where("id = ?", c.Param("vault_id")).First(&vault).Error; err != nil {
				c.JSON(http.StatusNotFound, gin.H{"error": "vault not found"})
				return nil, models.Vault{}, "", false
			}
			role = vaultaccess.RoleAdmin
		} else {
			c.JSON(http.StatusNotFound, gin.H{"error": "vault not found"})
			return nil, models.Vault{}, "", false
		}
	}
	if clientID := h.requestClientID(c, c.Query("client_id")); clientID != "" {
		if !h.requireDeviceVaultAccess(c, u.ID, vault.ID, clientID) {
			return nil, models.Vault{}, "", false
		}
	}
	return u, vault, role, true
}

func (h *Handler) requireVaultActorSync(c *gin.Context) (*models.User, models.Vault, string, jwt.DeviceID, bool) {
	u, ok := auth.RequireUser(c)
	if !ok {
		return nil, models.Vault{}, "", "", false
	}
	did, ok := auth.RequireDeviceID(c, c.GetHeader(deviceauth.ClientIDHeader), c.Query("client_id"))
	if !ok {
		return nil, models.Vault{}, "", "", false
	}
	vault, role, err := vaultaccess.Resolve(h.DB, u.ID, c.Param("vault_id"))
	if err != nil {
		if u.Role == "admin" {
			if err := h.DB.Where("id = ?", c.Param("vault_id")).First(&vault).Error; err != nil {
				c.JSON(http.StatusNotFound, gin.H{"error": "vault not found"})
				return nil, models.Vault{}, "", "", false
			}
			role = vaultaccess.RoleAdmin
		} else {
			c.JSON(http.StatusNotFound, gin.H{"error": "vault not found"})
			return nil, models.Vault{}, "", "", false
		}
	}
	if err := deviceauth.CheckVaultAccess(h.DB, u.ID, string(did), vault.ID); err != nil {
		h.writeDeviceAuthError(c, err)
		return nil, models.Vault{}, "", "", false
	}
	if !h.recordDeviceActivityWithDID(c, u.ID, vault.ID, did) {
		return nil, models.Vault{}, "", "", false
	}
	return u, vault, role, did, true
}

func (h *Handler) requireHistoryReader(c *gin.Context, path string) (*models.User, models.Vault, string, bool) {
	u, ok := auth.RequireUser(c)
	if !ok {
		return nil, models.Vault{}, "", false
	}
	vault, role, err := vaultaccess.Resolve(h.DB, u.ID, c.Param("vault_id"))
	if err == nil || u.Role == "admin" {
		if err != nil {
			if err := h.DB.Where("id = ?", c.Param("vault_id")).First(&vault).Error; err != nil {
				c.JSON(http.StatusNotFound, gin.H{"error": "vault not found"})
				return nil, models.Vault{}, "", false
			}
			role = vaultaccess.RoleAdmin
		}
		if clientID := h.requestClientID(c, c.Query("client_id")); clientID != "" &&
			!h.requireDeviceVaultAccess(c, u.ID, vault.ID, clientID) {
			return nil, models.Vault{}, "", false
		}
		return u, vault, role, true
	}
	var file models.File
	if err := h.DB.Where("vault_id = ? AND path = ?", c.Param("vault_id"), path).First(&file).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "vault not found"})
		return nil, models.Vault{}, "", false
	}
	var count int64
	if err := h.DB.Model(&models.Collaboration{}).Where(
		"vault_id = ? AND file_id = ? AND collaborator_id = ? AND status = ? AND allow_history_view = ?",
		file.VaultID, file.ID, u.ID, collaboration.StatusAccepted, true,
	).Count(&count).Error; err != nil || count == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "vault not found"})
		return nil, models.Vault{}, "", false
	}
	if err := h.DB.Where("id = ?", file.VaultID).First(&vault).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "vault not found"})
		return nil, models.Vault{}, "", false
	}
	return u, vault, "collaborator", true
}

func (h *Handler) requireHistoryReaderSync(c *gin.Context, path string) (*models.User, models.Vault, string, jwt.DeviceID, bool) {
	u, ok := auth.RequireUser(c)
	if !ok {
		return nil, models.Vault{}, "", "", false
	}
	did, ok := auth.RequireDeviceID(c, c.GetHeader(deviceauth.ClientIDHeader), c.Query("client_id"))
	if !ok {
		return nil, models.Vault{}, "", "", false
	}
	vault, role, err := vaultaccess.Resolve(h.DB, u.ID, c.Param("vault_id"))
	if err == nil || u.Role == "admin" {
		if err != nil {
			if err := h.DB.Where("id = ?", c.Param("vault_id")).First(&vault).Error; err != nil {
				c.JSON(http.StatusNotFound, gin.H{"error": "vault not found"})
				return nil, models.Vault{}, "", "", false
			}
			role = vaultaccess.RoleAdmin
		}
		if err := deviceauth.CheckVaultAccess(h.DB, u.ID, string(did), vault.ID); err != nil {
			h.writeDeviceAuthError(c, err)
			return nil, models.Vault{}, "", "", false
		}
		if !h.recordDeviceActivityWithDID(c, u.ID, vault.ID, did) {
			return nil, models.Vault{}, "", "", false
		}
		return u, vault, role, did, true
	}
	var file models.File
	if err := h.DB.Where("vault_id = ? AND path = ?", c.Param("vault_id"), path).First(&file).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "vault not found"})
		return nil, models.Vault{}, "", "", false
	}
	var count int64
	if err := h.DB.Model(&models.Collaboration{}).Where(
		"vault_id = ? AND file_id = ? AND collaborator_id = ? AND status = ? AND allow_history_view = ?",
		file.VaultID, file.ID, u.ID, collaboration.StatusAccepted, true,
	).Count(&count).Error; err != nil || count == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "vault not found"})
		return nil, models.Vault{}, "", "", false
	}
	if err := h.DB.Where("id = ?", file.VaultID).First(&vault).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "vault not found"})
		return nil, models.Vault{}, "", "", false
	}
	if err := deviceauth.CheckApproved(h.DB, u.ID, string(did)); err != nil {
		h.writeDeviceAuthError(c, err)
		return nil, models.Vault{}, "", "", false
	}
	if !h.recordDeviceActivityWithDID(c, u.ID, vault.ID, did) {
		return nil, models.Vault{}, "", "", false
	}
	return u, vault, "collaborator", did, true
}

// historyActor 构造历史操作者信息
func (h *Handler) historyActor(c *gin.Context, u *models.User) history.Actor {
	deviceName := deviceauth.DecodeDeviceName(c.GetHeader(deviceauth.DeviceNameHeader))
	if deviceName == "" {
		deviceName = "网页控制台"
	}
	return history.Actor{
		Username:   u.Username,
		DeviceName: deviceName,
		ClientID:   h.requestClientID(c, c.Query("client_id")),
	}
}

func (h *Handler) historyActorWithDID(c *gin.Context, u *models.User, did jwt.DeviceID) history.Actor {
	deviceName := deviceauth.DecodeDeviceName(c.GetHeader(deviceauth.DeviceNameHeader))
	if deviceName == "" {
		deviceName = "网页控制台"
	}
	return history.Actor{
		Username:   u.Username,
		DeviceName: deviceName,
		ClientID:   string(did),
	}
}

type historyOut struct {
	ID           uint   `json:"id"`
	FilePath     string `json:"file_path"`
	PreviousPath string `json:"previous_path,omitempty"`
	Action       string `json:"action"`
	Version      int    `json:"version"`
	Revision     int64  `json:"revision"`
	Username     string `json:"username"`
	DeviceName   string `json:"device_name"`
	HasSnapshot  bool   `json:"has_snapshot"`
	CreatedAt    string `json:"created_at"`
}

func toHistoryOut(row models.FileHistory) historyOut {
	return historyOut{
		ID: row.ID, FilePath: row.FilePath, PreviousPath: row.PreviousPath,
		Action: row.Action, Version: row.Version, Revision: row.Revision,
		Username: row.Username, DeviceName: row.DeviceName,
		HasSnapshot: row.ContentKey != "",
		CreatedAt:   row.CreatedAt.UTC().Format(time.RFC3339),
	}
}

// V2HistoryList 处理 GET /api/vaults/:vault_id/sync/history?path=xxx
func (h *Handler) V2HistoryList(c *gin.Context) {
	path, valid := normalizeRelativePath(c.Query("path"))
	if !valid {
		c.JSON(http.StatusBadRequest, gin.H{"error": "path is required"})
		return
	}
	_, vault, _, _, ok := h.requireHistoryReaderSync(c, path)
	if !ok {
		return
	}
	var rows []models.FileHistory
	if err := h.DB.Where("vault_id = ? AND file_path = ?", vault.ID, path).
		Order("version desc").Find(&rows).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	out := make([]historyOut, 0, len(rows))
	for _, row := range rows {
		out = append(out, toHistoryOut(row))
	}
	c.JSON(http.StatusOK, gin.H{"history": out})
}

type historyDetailOut struct {
	historyOut
	Content string   `json:"content,omitempty"`
	Diff    []string `json:"diff,omitempty"`
	IsText  bool     `json:"is_text"`
	// Mode 本次 diff 的对比基准：current 对当前文件，last 对上一版本
	Mode string `json:"mode"`
}

// V2HistoryDetail 处理 GET /api/vaults/:vault_id/sync/history/:history_id?mode=last|current&path=xxx
func (h *Handler) V2HistoryDetail(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("history_id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid history id"})
		return
	}
	var row models.FileHistory
	if err := h.DB.Where("id = ? AND vault_id = ?", id, c.Param("vault_id")).First(&row).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "history not found"})
		return
	}
	_, vault, _, _, ok := h.requireHistoryReaderSync(c, row.FilePath)
	if !ok {
		return
	}
	snapshot, err := history.ReadSnapshot(h.Cfg.Storage.DataDir, row.ContentKey)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	isText := history.IsText(row.FilePath)
	out := historyDetailOut{historyOut: toHistoryOut(row), IsText: isText}
	if isText && snapshot != nil {
		out.Content = string(snapshot)
	}

	// 计算 diff：mode=current 对比当前文件，否则对比上一版本
	var base []byte
	mode := "last"
	switch c.Query("mode") {
	case "current":
		mode = "current"
		base = h.currentFileContent(vault.ID, row.FilePath)
	default:
		var prev models.FileHistory
		if err := h.DB.Where("vault_id = ? AND file_path = ? AND version = ?",
			vault.ID, row.FilePath, row.Version-1).First(&prev).Error; err == nil {
			base, _ = history.ReadSnapshot(h.Cfg.Storage.DataDir, prev.ContentKey)
		}
	}
	if isText {
		out.Diff = history.DiffLines(base, snapshot)
	}
	out.Mode = mode
	c.JSON(http.StatusOK, out)
}

// currentFileContent 读取当前文件的磁盘内容（不存在返回 nil）
func (h *Handler) currentFileContent(vaultID, path string) []byte {
	var file models.File
	if err := h.DB.Where("vault_id = ? AND path = ? AND is_deleted = ?", vaultID, path, false).
		First(&file).Error; err != nil {
		return nil
	}
	raw, err := os.ReadFile(filestore.DiskPath(h.Cfg.Storage.DataDir, file))
	if err != nil {
		return nil
	}
	return raw
}

// V2HistoryRestore 处理 POST /api/vaults/:vault_id/sync/history/:history_id/restore?path=xxx
// 仅 owner / manager / 管理员可恢复
func (h *Handler) V2HistoryRestore(c *gin.Context) {
	u, vault, role, did, ok := h.requireVaultActorSync(c)
	if !ok {
		return
	}
	if !vaultaccess.CanManage(role) {
		c.JSON(http.StatusForbidden, gin.H{"error": "insufficient vault permission"})
		return
	}
	id, err := strconv.ParseUint(c.Param("history_id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid history id"})
		return
	}
	var row models.FileHistory
	if err := h.DB.Where("id = ? AND vault_id = ?", id, vault.ID).First(&row).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "history not found"})
		return
	}
	content, err := history.ReadSnapshot(h.Cfg.Storage.DataDir, row.ContentKey)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if content == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "this history record has no snapshot", "code": "no_snapshot"})
		return
	}
	meta, err := h.writeFileFromBytes(vault, row.FilePath, content, h.historyActorWithDID(c, u, did), history.ActionRestore, "")
	if err != nil {
		h.writeWriteError(c, err)
		return
	}
	meta.ServerTime = time.Now().UnixMilli()
	c.JSON(http.StatusOK, meta)
}

// writeFileFromBytes 将字节内容作为新版本写入仓库文件，并记录历史
// prevPath 用于重命名记录；prevContentPath 为写入前需要快照的旧正文路径（可空）
func (h *Handler) writeFileFromBytes(
	vault models.Vault,
	path string,
	content []byte,
	actor history.Actor,
	action, prevPath string,
) (V2FileMeta, error) {
	var valid bool
	path, valid = normalizeRelativePath(path)
	if !valid {
		return V2FileMeta{}, errors.New("path contains illegal segments")
	}
	vaultLock := h.vaultLock(vault.ID)
	vaultLock.Lock()
	defer vaultLock.Unlock()

	targetKey := filestore.VaultStorageKey(vault.ID, path)
	targetPath := filepath.Join(h.Cfg.Storage.DataDir, filepath.FromSlash(targetKey))
	tmpPath := targetPath + ".restore-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	if err := os.MkdirAll(filepath.Dir(targetPath), 0o750); err != nil {
		return V2FileMeta{}, err
	}
	if err := os.WriteFile(tmpPath, content, 0o640); err != nil {
		return V2FileMeta{}, err
	}
	defer os.Remove(tmpPath)

	var result models.File
	prevDisk := ""
	err := storagequota.WithinLimit(h.Cfg.Storage.DataDir, h.Cfg.Storage.MaxTotalSizeBytes(), historySnapshotReserve(targetPath), func() error {
		return h.DB.Transaction(func(tx *gorm.DB) error {
			current, exists, err := lockedFile(tx, vault.OwnerID, vault.ID, path)
			if err != nil {
				return err
			}
			if exists && !current.IsDeleted {
				prevDisk = h.fileDiskPath(current)
			}
			backupPath := ""
			if _, err := os.Stat(targetPath); err == nil {
				backupPath = targetPath + ".backup-restore-" + strconv.FormatInt(time.Now().UnixNano(), 10)
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
			revision, err := nextVaultRevision(tx, vault.ID)
			if err != nil {
				return err
			}
			if !exists {
				current = models.File{UserID: vault.OwnerID, VaultID: vault.ID, Path: path}
			}
			if current.Type != "config" || !isCommunityPluginSettingsPath(path) {
				current.Type = classifyFile(path)
			}
			current.Hash = hashBytes(content)
			current.Size = int64(len(content))
			current.MTime = time.Now().UnixMilli()
			current.Revision = revision
			current.IsDeleted = false
			current.DeletedAt = sql.NullTime{}
			current.StorageKey = targetKey
			current.LastWriterClientID = actor.ClientID
			current.LastOperationID = "restore-" + strconv.FormatInt(revision, 10)
			if exists {
				if err := tx.Save(&current).Error; err != nil {
					return err
				}
			} else if err := tx.Create(&current).Error; err != nil {
				return err
			}
			// 快照当前正文作为恢复前版本
			if err := history.Record(tx, h.Cfg.Storage.DataDir, vault.ID, actor, action, path, prevPath, prevDisk, revision); err != nil {
				return err
			}
			result = current
			return nil
		})
	})
	if err != nil {
		if _, statErr := os.Stat(targetPath); statErr == nil {
			_ = os.Remove(targetPath)
		}
		return V2FileMeta{}, err
	}
	h.notifyRevision(vault.ID)
	return v2Meta(result), nil
}

// writeWriteError 统一输出写入错误
func (h *Handler) writeWriteError(c *gin.Context, err error) {
	if errors.Is(err, storagequota.ErrExceeded) {
		c.JSON(http.StatusInsufficientStorage, gin.H{"error": "project storage quota exceeded", "code": "project_storage_quota_exceeded"})
		return
	}
	if errors.Is(err, errRevisionConflict) {
		c.JSON(http.StatusConflict, gin.H{"error": "revision conflict"})
		return
	}
	c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
}

// RecycleList 处理 GET /api/vaults/:vault_id/recycle-bin
func (h *Handler) RecycleList(c *gin.Context) {
	_, vault, _, _, ok := h.requireVaultActorSync(c)
	if !ok {
		return
	}
	var files []models.File
	if err := h.DB.Where("vault_id = ? AND is_deleted = ? AND storage_key <> ''", vault.ID, true).
		Order("deleted_at desc").Find(&files).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	retention, err := recycle.RetentionDays(h.DB, vault.ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	now := time.Now()
	out := make([]gin.H, 0, len(files))
	for _, f := range files {
		expiresAt := recycle.ExpiresAt(f, retention)
		remaining := expiresAt.Sub(now)
		out = append(out, gin.H{
			"id":                f.ID,
			"path":              f.Path,
			"type":              f.Type,
			"size":              f.Size,
			"deleted_at":        formatHistoryTime(f.DeletedAt.Time),
			"expires_at":        expiresAt.UTC().Format(time.RFC3339),
			"remaining_seconds": int64(remaining / time.Second),
			"can_restore":       remaining > 0,
		})
	}
	c.JSON(http.StatusOK, gin.H{"files": out})
}

// RecycleRestore 处理 POST /api/vaults/:vault_id/recycle-bin/:file_id/restore
func (h *Handler) RecycleRestore(c *gin.Context) {
	u, vault, role, did, ok := h.requireVaultActorSync(c)
	if !ok {
		return
	}
	if !vaultaccess.CanManage(role) {
		c.JSON(http.StatusForbidden, gin.H{"error": "insufficient vault permission"})
		return
	}
	var file models.File
	if err := h.DB.Where("id = ? AND vault_id = ? AND is_deleted = ?", c.Param("file_id"), vault.ID, true).
		First(&file).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "file not found"})
		return
	}
	if err := recycle.CheckRestorable(h.DB, file, time.Now()); err != nil {
		if errors.Is(err, recycle.ErrRetentionExpired) {
			c.JSON(http.StatusGone, gin.H{"error": "recycle retention has expired", "code": "recycle_retention_expired"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	vaultLock := h.vaultLock(vault.ID)
	vaultLock.Lock()
	defer vaultLock.Unlock()

	newKey := filestore.VaultStorageKey(vault.ID, file.Path)
	newPath := filepath.Join(h.Cfg.Storage.DataDir, filepath.FromSlash(newKey))
	actor := h.historyActorWithDID(c, u, did)
	err := h.DB.Transaction(func(tx *gorm.DB) error {
		// 从回收站移回正文
		if err := recycle.MoveOut(h.Cfg.Storage.DataDir, file, newPath); err != nil {
			return err
		}
		revision, err := nextVaultRevision(tx, vault.ID)
		if err != nil {
			return err
		}
		updates := map[string]any{
			"is_deleted":            false,
			"deleted_at":            nil,
			"storage_key":           newKey,
			"revision":              revision,
			"last_writer_client_id": actor.ClientID,
		}
		if err := tx.Model(&models.File{}).Where("id = ?", file.ID).Updates(updates).Error; err != nil {
			return err
		}
		if err := tx.Model(&models.Vault{}).Where("id = ?", vault.ID).
			UpdateColumn("storage_used", gorm.Expr("storage_used + ?", file.Size)).Error; err != nil {
			return err
		}
		return history.Record(tx, h.Cfg.Storage.DataDir, vault.ID, actor, history.ActionRestore, file.Path, "", "", revision)
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	h.notifyRevision(vault.ID)
	c.JSON(http.StatusOK, gin.H{"message": "file restored"})
}

// RecycleDelete 处理 POST /api/vaults/:vault_id/recycle-bin/:file_id/delete（永久删除）
func (h *Handler) RecycleDelete(c *gin.Context) {
	_, vault, role, _, ok := h.requireVaultActorSync(c)
	if !ok {
		return
	}
	if !vaultaccess.CanDelete(role) {
		c.JSON(http.StatusForbidden, gin.H{"error": "insufficient vault permission"})
		return
	}
	var file models.File
	if err := h.DB.Where("id = ? AND vault_id = ? AND is_deleted = ?", c.Param("file_id"), vault.ID, true).
		First(&file).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "file not found"})
		return
	}

	vaultLock := h.vaultLock(vault.ID)
	vaultLock.Lock()
	defer vaultLock.Unlock()
	err := h.DB.Transaction(func(tx *gorm.DB) error {
		var state models.VaultSyncState
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("vault_id = ?", vault.ID).
			First(&state).Error; err != nil {
			return err
		}
		if file.Revision > state.CompactedRevision {
			if err := tx.Model(&models.VaultSyncState{}).Where("vault_id = ?", vault.ID).
				Update("compacted_revision", file.Revision).Error; err != nil {
				return err
			}
		}
		return tx.Unscoped().Delete(&file).Error
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if err := recycle.Remove(h.Cfg.Storage.DataDir, file); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	h.notifyRevision(vault.ID)
	c.JSON(http.StatusOK, gin.H{"message": "file permanently deleted"})
}

func formatHistoryTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}
