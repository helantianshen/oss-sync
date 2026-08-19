package syncapi

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/oss/oss-server/internal/auth"
	"github.com/oss/oss-server/internal/collaboration"
	"github.com/oss/oss-server/internal/deviceauth"
	"github.com/oss/oss-server/internal/filestore"
	"github.com/oss/oss-server/internal/history"
	"github.com/oss/oss-server/internal/models"
	"github.com/oss/oss-server/internal/vaultaccess"
)

// collabOut 协作关系输出。
type collabOut struct {
	ID            uint   `json:"id"`
	FileID        uint   `json:"file_id"`
	VaultID       string `json:"vault_id"`
	FilePath      string `json:"file_path"`
	OwnerID       uint   `json:"owner_id"`
	OwnerUsername string `json:"owner_username"`
	CollabID      uint   `json:"collaborator_id"`
	CollabName    string `json:"collaborator_username"`
	Status        string `json:"status"`
	CreatedAt     string `json:"created_at"`
}

func (h *Handler) collabOuts(rows []models.Collaboration) ([]collabOut, error) {
	ids := make([]uint, 0, len(rows)*2)
	for _, r := range rows {
		ids = append(ids, r.OwnerID, r.CollaboratorID)
	}
	usernames := map[uint]string{}
	if len(ids) > 0 {
		var users []models.User
		if err := h.DB.Where("id IN ?", ids).Find(&users).Error; err != nil {
			return nil, err
		}
		for _, u := range users {
			usernames[u.ID] = u.Username
		}
	}
	fileIDs := make([]uint, 0, len(rows))
	for _, r := range rows {
		fileIDs = append(fileIDs, r.FileID)
	}
	paths := map[uint]string{}
	if len(fileIDs) > 0 {
		var files []models.File
		if err := h.DB.Where("id IN ?", fileIDs).Find(&files).Error; err != nil {
			return nil, err
		}
		for _, f := range files {
			paths[f.ID] = f.Path
		}
	}
	out := make([]collabOut, 0, len(rows))
	for _, r := range rows {
		out = append(out, collabOut{
			ID: r.ID, FileID: r.FileID, VaultID: r.VaultID, FilePath: paths[r.FileID],
			OwnerID: r.OwnerID, OwnerUsername: usernames[r.OwnerID],
			CollabID: r.CollaboratorID, CollabName: usernames[r.CollaboratorID],
			Status: r.Status, CreatedAt: r.CreatedAt.Format(time.RFC3339),
		})
	}
	return out, nil
}

// CollabList 列出当前用户在该仓库的协作关系（owner/manager 看全部，协作者看与自己相关）。
func (h *Handler) CollabList(c *gin.Context) {
	u, ok := auth.RequireUser(c)
	if !ok {
		return
	}
	vaultID := c.Param("vault_id")
	var vault models.Vault
	if err := h.DB.Where("id = ?", vaultID).First(&vault).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "vault not found"})
		return
	}
	var rows []models.Collaboration
	var err error
	if u.Role == "admin" || vault.OwnerID == u.ID {
		rows, err = h.collab.ListForVault(vault.ID)
	} else {
		err = h.DB.Where("vault_id = ? AND (owner_id = ? OR collaborator_id = ?)",
			vault.ID, u.ID, u.ID).Find(&rows).Error
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	incoming, err := h.collab.ListForUser(u.ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	seen := make(map[uint]struct{}, len(rows)+len(incoming))
	for _, row := range rows {
		seen[row.ID] = struct{}{}
	}
	for _, row := range incoming {
		if _, exists := seen[row.ID]; exists {
			continue
		}
		rows = append(rows, row)
		seen[row.ID] = struct{}{}
	}
	out, err := h.collabOuts(rows)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"collaborations": out})
}

// CollabInbox lists collaborations received by the authenticated user across Vaults.
func (h *Handler) CollabInbox(c *gin.Context) {
	u, ok := auth.RequireUser(c)
	if !ok {
		return
	}
	rows, err := h.collab.ListForUser(u.ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	out, err := h.collabOuts(rows)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"collaborations": out})
}

// CollabInvite 邀请用户协作 Markdown 文件。
func (h *Handler) CollabInvite(c *gin.Context) {
	u, vault, _, ok := h.requireVaultActor(c)
	if !ok {
		return
	}
	var req struct {
		FilePath string `json:"file_path" binding:"required"`
		Username string `json:"username" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	req.FilePath, _ = normalizeRelativePath(req.FilePath)
	if req.FilePath == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid file path"})
		return
	}
	row, err := h.collab.Invite(u.ID, vault.ID, req.FilePath, req.Username)
	if err != nil {
		status := http.StatusBadRequest
		switch {
		case errors.Is(err, collaboration.ErrNotOwnerOrManager):
			status = http.StatusForbidden
		case errors.Is(err, collaboration.ErrDuplicate):
			status = http.StatusConflict
		case errors.Is(err, collaboration.ErrFileNotFound):
			status = http.StatusNotFound
		}
		c.JSON(status, gin.H{"error": err.Error()})
		return
	}
	// 通知邀请双方，并唤醒旧客户端绑定的 Vault 事件通道。
	h.publishCollaborationEvent(collaboration.Event{
		VaultID: vault.ID, FileID: row.FileID, Kind: "invited", At: time.Now().UnixMilli(),
	}, []uint{row.OwnerID, row.CollaboratorID})
	outs, _ := h.collabOuts([]models.Collaboration{*row})
	if len(outs) > 0 {
		c.JSON(http.StatusOK, outs[0])
		return
	}
	c.JSON(http.StatusOK, gin.H{})
}

// CollabRespond 被邀请者接受或拒绝。协作者即使尚未获得 Vault 成员资格也能响应邀请。
func (h *Handler) CollabRespond(c *gin.Context) {
	u, ok := auth.RequireUser(c)
	if !ok {
		return
	}
	collabID, err := strconv.ParseUint(c.Param("collab_id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid collaboration id"})
		return
	}
	accept := c.PostForm("accept") == "true"
	var req struct {
		Accept bool `json:"accept"`
	}
	if c.Request.Method == http.MethodPost && c.Request.ContentLength > 0 {
		_ = c.ShouldBindJSON(&req)
		accept = req.Accept
	}
	if err := h.collab.Respond(u.ID, uint(collabID), accept); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	var row models.Collaboration
	if err := h.DB.First(&row, uint(collabID)).Error; err == nil {
		kind := "revoked"
		if accept {
			kind = "changed"
		}
		h.publishCollaborationEvent(collaboration.Event{
			VaultID: row.VaultID, FileID: row.FileID, Kind: kind, At: time.Now().UnixMilli(),
		}, []uint{row.OwnerID, row.CollaboratorID})
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

// CollabRevoke 撤回邀请或解除协作（owner/manager）。
func (h *Handler) CollabRevoke(c *gin.Context) {
	u, _, _, ok := h.requireVaultActor(c)
	if !ok {
		return
	}
	collabID, err := strconv.ParseUint(c.Param("collab_id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid collaboration id"})
		return
	}
	var row models.Collaboration
	if err := h.DB.First(&row, uint(collabID)).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "collaboration not found"})
		return
	}
	if err := h.collab.Revoke(u.ID, uint(collabID)); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	h.publishCollaborationEvent(collaboration.Event{
		VaultID: row.VaultID, FileID: row.FileID, Kind: "revoked", At: time.Now().UnixMilli(),
	}, []uint{row.OwnerID, row.CollaboratorID})
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

// CollabLeave lets an accepted collaborator actively end their collaboration.
func (h *Handler) CollabLeave(c *gin.Context) {
	u, ok := auth.RequireUser(c)
	if !ok {
		return
	}
	collabID, err := strconv.ParseUint(c.Param("collab_id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid collaboration id"})
		return
	}
	var row models.Collaboration
	if err := h.DB.First(&row, uint(collabID)).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "collaboration not found"})
		return
	}
	if err := h.collab.Leave(u.ID, uint(collabID)); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	h.publishCollaborationEvent(collaboration.Event{
		VaultID: row.VaultID, FileID: row.FileID, Kind: "revoked", At: time.Now().UnixMilli(),
	}, []uint{row.OwnerID, row.CollaboratorID})
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

// CollabUpload 协作者上传正文（以协作者身份写入原文件）。
func (h *Handler) CollabUpload(c *gin.Context) {
	u, ok := auth.RequireUser(c)
	if !ok {
		return
	}
	vaultID := c.Param("vault_id")
	var vault models.Vault
	if err := h.DB.Where("id = ?", vaultID).First(&vault).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "vault not found"})
		return
	}
	fileID, err := strconv.ParseUint(c.Param("file_id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid file id"})
		return
	}
	// 校验协作关系。
	var file models.File
	if err := h.DB.Where("id = ? AND vault_id = ?", uint(fileID), vault.ID).First(&file).Error; err != nil {
		file, err = h.acceptedCollaborationFile(u.ID, uint(fileID))
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "file not found"})
			return
		}
		vault = models.Vault{}
		if err := h.DB.Where("id = ?", file.VaultID).First(&vault).Error; err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "vault not found"})
			return
		}
	}
	var collabRow models.Collaboration
	if err := h.DB.Where("vault_id = ? AND file_id = ? AND collaborator_id = ? AND status = ?",
		vault.ID, uint(fileID), u.ID, collaboration.StatusAccepted).First(&collabRow).Error; err != nil {
		c.JSON(http.StatusForbidden, gin.H{"error": "你不是该文件的协作者"})
		return
	}
	// 读取正文。
	var req struct {
		Content string `json:"content" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	content := []byte(req.Content)

	clientID := h.requestClientID(c, c.Query("client_id"))
	deviceName := deviceauth.DecodeDeviceName(c.GetHeader(deviceauth.DeviceNameHeader))
	if deviceName == "" {
		deviceName = "网页控制台"
	}

	vaultLock := h.vaultLock(vault.ID)
	vaultLock.Lock()
	defer vaultLock.Unlock()
	pathLock := h.pathLock(vault.ID + ":" + file.Path)
	pathLock.Lock()
	defer pathLock.Unlock()

	err = h.DB.Transaction(func(tx *gorm.DB) error {
		revision, err := nextVaultRevision(tx, vault.ID)
		if err != nil {
			return err
		}
		target := filepath.Join(h.Cfg.Storage.DataDir, filepath.FromSlash(filestore.VaultStorageKey(vault.ID, file.Path)))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		// 原子写入：先写临时文件，再 rename 到目标路径，
		// 避免进程崩溃时留下部分写入的损坏文件。
		tmp := target + ".tmp"
		if err := os.WriteFile(tmp, content, 0o644); err != nil {
			return err
		}
		if err := os.Rename(tmp, target); err != nil {
			_ = os.Remove(tmp)
			return err
		}
		sum := sha256.Sum256(content)
		now := time.Now()
		updates := map[string]any{
			"revision":    revision,
			"is_deleted":  false,
			"deleted_at":  nil,
			"hash":        fmt.Sprintf("%x", sum),
			"size":        len(content),
			"m_time":      now.UnixMilli(),
			"storage_key": filestore.VaultStorageKey(vault.ID, file.Path),
			"updated_at":  now,
		}
		if err := tx.Model(&models.File{}).Where("id = ?", file.ID).Updates(updates).Error; err != nil {
			return err
		}
		actor := history.Actor{
			Username:   u.Username,
			DeviceName: deviceName,
			ClientID:   clientID,
		}
		return history.Record(tx, h.Cfg.Storage.DataDir, vault.ID, actor,
			history.ActionModify, file.Path, "", target, revision)
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	h.notifyRevision(vault.ID)
	h.publishCollaborationEvent(collaboration.Event{
		VaultID: vault.ID, FileID: uint(fileID), FilePath: file.Path,
		Kind: "changed", At: time.Now().UnixMilli(),
	}, h.collaborationEventUsers(vault.ID, uint(fileID)))
	c.JSON(http.StatusOK, gin.H{"status": "ok", "file_path": file.Path})
}

// CollabSSE 建立 SSE 事件流。由于 EventSource 不能带 Authorization 头，
// HTTPS 或本机回环连接允许短期 JWT 查询参数 token + client_id。
func (h *Handler) CollabSSE(c *gin.Context) {
	user, vault, ok := h.collabSSEAuthorize(c)
	if !ok {
		return
	}
	ch, _ := h.broker.Subscribe(vault.ID)
	defer h.broker.Unsubscribe(vault.ID, ch)

	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-store")
	c.Header("Referrer-Policy", "no-referrer")
	c.Header("X-Accel-Buffering", "no")
	c.Status(http.StatusOK)

	// 先发一条 ready 事件。
	fmt.Fprintf(c.Writer, "event: ready\ndata: {\"vault_id\":%q}\n\n", vault.ID)
	c.Writer.Flush()

	heartbeat := time.NewTicker(20 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case ev := <-ch:
			if ev.VaultID != vault.ID {
				continue
			}
			fmt.Fprintf(c.Writer, "event: %s\ndata: {\"file_id\":%d,\"file_path\":%q,\"vault_id\":%q}\n\n",
				ev.Kind, ev.FileID, ev.FilePath, ev.VaultID)
			c.Writer.Flush()
		case <-heartbeat.C:
			fmt.Fprint(c.Writer, ": keepalive\n\n")
			c.Writer.Flush()
		case <-c.Request.Context().Done():
			_ = user
			return
		}
	}
}

// CollabPoll 长轮询协作事件：wait=30，返回 changed/revoked。
func (h *Handler) CollabPoll(c *gin.Context) {
	_, vault, ok := h.collabSSEAuthorize(c)
	if !ok {
		return
	}
	last, err := parseInt64Default(c.Query("after"), 0)
	if err != nil || last < 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid after"})
		return
	}
	waitSec, err := parseIntDefault(c.Query("wait"), 30)
	if err != nil || waitSec < 0 || waitSec > 30 {
		waitSec = 30
	}
	version, changed := h.broker.WaitVersion(vault.ID, last, time.Duration(waitSec)*time.Second)
	if changed {
		c.JSON(http.StatusOK, gin.H{"changed": true, "version": version, "vault_id": vault.ID})
		return
	}
	c.JSON(http.StatusOK, gin.H{"changed": false, "version": version, "vault_id": vault.ID})
}

// collabSSEAuthorize 校验 SSE/轮询身份：Bearer 或短期 token 查询参数。
func (h *Handler) collabSSEAuthorize(c *gin.Context) (*models.User, models.Vault, bool) {
	user, ok := h.collabEventUser(c)
	if !ok {
		return nil, models.Vault{}, false
	}
	vault, _, err := vaultaccess.Resolve(h.DB, user.ID, c.Param("vault_id"))
	if err != nil {
		// 允许参与协作的用户订阅事件（无需 Vault 成员资格）。
		var collabCount int64
		if err2 := h.DB.Model(&models.Collaboration{}).
			Where("vault_id = ? AND collaborator_id = ?", c.Param("vault_id"), user.ID).
			Count(&collabCount).Error; err2 != nil || collabCount == 0 {
			c.AbortWithStatus(http.StatusNotFound)
			return nil, models.Vault{}, false
		}
		if err2 := h.DB.Where("id = ?", c.Param("vault_id")).First(&vault).Error; err2 != nil {
			c.AbortWithStatus(http.StatusNotFound)
			return nil, models.Vault{}, false
		}
	}
	return user, vault, true
}

var _ = gorm.ErrRecordNotFound
var _ = vaultaccess.RoleOwner
