// Package syncapi 提供文件同步接口
//
//   - POST /api/sync/check    比较客户端与服务端文件元数据
//   - POST /api/sync/upload   接收原始字节或 multipart 文件
//   - GET  /api/sync/download 下载文件
//   - POST /api/sync/delete   删除正文并写入同步墓碑
//
// 新客户端使用 /api/vaults/:vault_id/sync 下的 revision 协议；旧接口保留
// 用于兼容已有客户端
package syncapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	pathpkg "path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/helantianshen/oss-sync/internal/auth"
	"github.com/helantianshen/oss-sync/internal/collaboration"
	"github.com/helantianshen/oss-sync/internal/config"
	"github.com/helantianshen/oss-sync/internal/deviceauth"
	"github.com/helantianshen/oss-sync/internal/filestore"
	"github.com/helantianshen/oss-sync/internal/jwt"
	"github.com/helantianshen/oss-sync/internal/models"
)

// Handler 持有同步路由所需的数据库、修订信号和协作事件
type Handler struct {
	DB       *gorm.DB
	Cfg      *config.Config
	signals  sync.Map
	broker   *collaboration.Broker
	collab   *collaboration.Service
	presence collaborationPresence
}

// New 为同一服务实例创建共享的同步处理器
func New(db *gorm.DB, cfg *config.Config) *Handler {
	return &Handler{
		DB: db, Cfg: cfg,
		broker: collaboration.NewBroker(),
		collab: collaboration.New(db),
	}
}

// Register 挂载需要身份认证的同步路由
func (h *Handler) Register(r *gin.Engine) {
	g := r.Group("/api/sync", auth.Middleware(h.DB, h.Cfg))
	{
		g.POST("/check", h.Check)
		g.POST("/upload", h.Upload)
		g.GET("/download", h.Download)
		g.POST("/delete", h.Delete)
	}

	v2 := r.Group("/api/vaults/:vault_id/sync", auth.Middleware(h.DB, h.Cfg))
	{
		v2.GET("/manifest", h.V2Manifest)
		v2.GET("/changes", h.V2Changes)
		v2.POST("/ack", h.V2Ack)
		v2.POST("/upload", h.V2Upload)
		v2.GET("/download", h.V2Download)
		v2.GET("/delta-base", h.V2DeltaBase)
		v2.POST("/download-delta", h.V2DownloadDelta)
		v2.POST("/delete", h.V2Delete)
		v2.POST("/rename", h.V2Rename)
		v2.GET("/strategy", h.V2Strategy)
		v2.GET("/history", h.V2HistoryList)
		v2.GET("/history/:history_id", h.V2HistoryDetail)
		v2.POST("/history/:history_id/restore", h.V2HistoryRestore)
	}

	recycle := r.Group("/api/vaults/:vault_id/recycle-bin", auth.Middleware(h.DB, h.Cfg))
	{
		recycle.GET("", h.RecycleList)
		recycle.POST("/:file_id/restore", h.RecycleRestore)
		recycle.POST("/:file_id/delete", h.RecycleDelete)
	}

	collab := r.Group("/api/vaults/:vault_id/collaborations", auth.Middleware(h.DB, h.Cfg))
	{
		collab.GET("", h.CollabList)
		collab.POST("", h.CollabInvite)
		collab.POST("/:collab_id/respond", h.CollabRespond)
		collab.POST("/:collab_id/revoke", h.CollabRevoke)
		collab.POST("/:collab_id/history-view", h.CollabSetHistoryView)
		collab.POST("/:collab_id/leave", h.CollabLeave)
		collab.GET("/files/:file_id/content", h.CollabContent)
		collab.POST("/files/:file_id/upload", h.CollabUpload)
		collab.POST("/files/:file_id/presence", h.CollabPresence)
		collab.GET("/files/:file_id/presence", h.CollabPresence)
	}
	collabInbox := r.Group("/api/collaborations", auth.Middleware(h.DB, h.Cfg))
	{
		collabInbox.GET("", h.CollabInbox)
		collabInbox.GET("/poll", h.CollabAccountPoll)
	}
	collabAccountEvents := r.Group("/api/collaborations", allowObsidianDesktopOrigin())
	{
		collabAccountEvents.GET("/stream", h.CollabAccountSSE)
	}
	// SSE 与长轮询自行鉴权（支持 EventSource 的短期 token 查询参数），不套用 Bearer 中间件
	collabEvents := r.Group("/api/vaults/:vault_id/collaborations", allowObsidianDesktopOrigin())
	{
		collabEvents.GET("/stream", h.CollabSSE)
		collabEvents.GET("/poll", h.CollabPoll)
	}
}

// CheckRequest 客户端提交的同步检查请求
//
// mode：
//   - "full"（默认）：客户端提交全量文件元数据，服务端对每个文件返回状态
//   - "incremental"：客户端只提交本地有变化的文件
type CheckRequest struct {
	Mode  string        `json:"mode"`
	Files []CheckFileIn `json:"files"`
}

// CheckFileIn 是客户端提交的单文件元数据
type CheckFileIn struct {
	Path  string `json:"path"`
	MTime int64  `json:"mtime"` // 客户端本地 mtime（Unix 毫秒）
	Hash  string `json:"hash"`  // 客户端本地 SHA256
}

// CheckResponse 是批量同步检查结果
type CheckResponse struct {
	ServerTime int64          `json:"server_time"` // Unix 毫秒时间戳
	Results    []CheckFileOut `json:"results"`
}

// CheckFileOut 表示单文件的同步决策
type CheckFileOut struct {
	Path        string `json:"path"`
	Status      string `json:"status"` // 取值：upload_needed、download_needed、in_sync、conflict_detected、assume_in_sync
	ServerMTime int64  `json:"server_mtime,omitempty"`
	ServerHash  string `json:"server_hash,omitempty"`
}

// Check 按修改时间和哈希比较客户端与服务端文件：
//   - 本端无记录 + 客户端有文件 -> upload_needed
//   - 服务端无记录 + 客户端提交了 -> upload_needed（首次同步上传后建基线）
//   - 客户端 mtime 大于服务端 mtime -> upload_needed
//   - 客户端 mtime 小于服务端 mtime -> download_needed
//   - 相等 -> in_sync
//   - 哈希不同且服务端 mtime 更大 -> conflict_detected
func (h *Handler) Check(c *gin.Context) {
	u, ok := auth.RequireUser(c)
	if !ok {
		return
	}
	did, ok := auth.RequireDeviceID(c, c.GetHeader(deviceauth.ClientIDHeader), c.Query("client_id"))
	if !ok {
		return
	}

	var req CheckRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body: " + err.Error()})
		return
	}
	if req.Mode == "" {
		req.Mode = "full"
	}
	if req.Mode != "full" && req.Mode != "incremental" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "mode must be 'full' or 'incremental'"})
		return
	}
	vaultID, err := defaultVaultID(h.DB, u.ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if err := deviceauth.CheckVaultAccess(h.DB, u.ID, string(did), vaultID); err != nil {
		h.writeDeviceAuthError(c, err)
		return
	}
	if !h.recordDeviceActivityWithDID(c, u.ID, vaultID, did) {
		return
	}

	paths := make([]string, 0, len(req.Files))
	for i := range req.Files {
		normalized, valid := normalizeRelativePath(req.Files[i].Path)
		if !valid {
			c.JSON(http.StatusBadRequest, gin.H{"error": "path contains illegal segments"})
			return
		}
		req.Files[i].Path = normalized
		paths = append(paths, normalized)
	}
	type fileRow struct {
		Path      string
		MTime     int64
		Hash      string
		IsDeleted bool
	}
	rows := []fileRow{}
	if len(paths) > 0 {
		h.DB.Model(&models.File{}).
			Select("path", "m_time as m_time", "hash", "is_deleted").
			Where("user_id = ? AND vault_id = ? AND path IN ?", u.ID, vaultID, paths).
			Find(&rows)
	}
	idx := map[string]fileRow{}
	for _, r := range rows {
		idx[r.Path] = r
	}

	resp := CheckResponse{
		ServerTime: time.Now().UnixMilli(),
		Results:    make([]CheckFileOut, 0, len(req.Files)),
	}

	for _, cf := range req.Files {
		srv, hasSrv := idx[cf.Path]
		out := CheckFileOut{Path: cf.Path}

		if !hasSrv {
			out.Status = "upload_needed"
			resp.Results = append(resp.Results, out)
			continue
		}
		if srv.IsDeleted {
			out.Status = "upload_needed"
			resp.Results = append(resp.Results, out)
			continue
		}
		out.ServerMTime = srv.MTime
		out.ServerHash = srv.Hash

		switch {
		case cf.MTime > srv.MTime:
			out.Status = "upload_needed"
		case cf.MTime < srv.MTime:
			if srv.Hash != "" && cf.Hash != "" && srv.Hash != cf.Hash {
				out.Status = "conflict_detected"
			} else {
				out.Status = "download_needed"
			}
		default:
			if srv.Hash != cf.Hash && srv.Hash != "" && cf.Hash != "" {
				out.Status = "conflict_detected"
			} else {
				out.Status = "in_sync"
			}
		}
		resp.Results = append(resp.Results, out)
	}

	c.JSON(http.StatusOK, resp)
}

// classifyFile 把文件分为 markdown、attachment 和 config
//   - markdown: *.md
//   - config  : 路径以 .obsidian/ 开头
//   - attachment: 其余（图片、pdf、mp4 等）
func classifyFile(path string) string {
	lower := strings.ToLower(path)
	if strings.HasSuffix(lower, ".md") {
		return "markdown"
	}
	if strings.HasPrefix(lower, ".obsidian/") {
		return "config"
	}
	return "attachment"
}

// isSafeRelativePath 禁止路径逃逸和设备本地状态文件进入同步接口
func isSafeRelativePath(p string) bool {
	_, ok := normalizeRelativePath(p)
	return ok
}

func normalizeRelativePath(p string) (string, bool) {
	p = strings.TrimSpace(strings.ReplaceAll(p, "\\", "/"))
	if p == "" || strings.HasPrefix(p, "/") || filepath.IsAbs(p) || strings.Contains(p, ":") {
		return "", false
	}
	clean := pathpkg.Clean(p)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", false
	}
	if clean == "" || isDeviceLocalObsidianPath(clean) {
		return "", false
	}
	return clean, true
}

// isDeviceLocalObsidianPath 排除工作区、缓存与插件本地设置，其他 Obsidian 配置仍可同步
func isDeviceLocalObsidianPath(p string) bool {
	if p == ".obsidian/workspace.json" || p == ".obsidian/workspace-mobile.json" || p == ".obsidian/cache" || strings.HasPrefix(p, ".obsidian/cache/") {
		return true
	}
	parts := strings.Split(p, "/")
	return len(parts) == 4 && parts[0] == ".obsidian" && parts[1] == "plugins" && parts[3] == "data.json"
}

// Download 处理 GET /api/sync/download?path=xxx
func (h *Handler) Download(c *gin.Context) {
	u, ok := auth.RequireUser(c)
	if !ok {
		return
	}
	did, ok := auth.RequireDeviceID(c, c.GetHeader(deviceauth.ClientIDHeader), c.Query("client_id"))
	if !ok {
		return
	}
	path, valid := normalizeRelativePath(c.Query("path"))
	if !valid {
		c.JSON(http.StatusBadRequest, gin.H{"error": "path is required"})
		return
	}

	vaultID, err := defaultVaultID(h.DB, u.ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if err := deviceauth.CheckVaultAccess(h.DB, u.ID, string(did), vaultID); err != nil {
		h.writeDeviceAuthError(c, err)
		return
	}
	if !h.recordDeviceActivityWithDID(c, u.ID, vaultID, did) {
		return
	}
	var f models.File
	if err := h.DB.Where("user_id = ? AND vault_id = ? AND path = ?", u.ID, vaultID, path).First(&f).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "file not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if f.IsDeleted {
		c.JSON(http.StatusGone, gin.H{"error": "file has been deleted"})
		return
	}

	abs := filestore.DiskPath(h.Cfg.Storage.DataDir, f)
	fh, err := os.Open(abs)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "file missing on disk: " + err.Error()})
		return
	}
	defer fh.Close()
	stat, err := fh.Stat()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "stat failed: " + err.Error()})
		return
	}

	c.Header("Content-Type", "application/octet-stream")
	c.Header("Content-Length", fmt.Sprintf("%d", stat.Size()))
	c.Header("Content-Disposition", "attachment; filename=\""+filepath.Base(path)+"\"")
	c.Header("X-OSS-MTime", fmt.Sprintf("%d", f.MTime))
	c.Header("X-OSS-Hash", f.Hash)
	c.Status(http.StatusOK)
	if _, err := io.Copy(c.Writer, fh); err != nil {
		_ = err
	}
}

// DeleteRequest 删除请求
type DeleteRequest struct {
	Path string `json:"path"`
}

// Delete 处理 POST /api/sync/delete
// 正文立即从服务端存储移除；数据库墓碑保留到设备游标允许压缩
func (h *Handler) Delete(c *gin.Context) {
	u, ok := auth.RequireUser(c)
	if !ok {
		return
	}
	var rawDelete map[string]any
	if err := c.ShouldBindJSON(&rawDelete); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body: " + err.Error()})
		return
	}
	pathRaw, _ := rawDelete["path"].(string)
	suppliedClientID, _ := rawDelete["client_id"].(string)
	did, ok := auth.RequireDeviceID(c, c.GetHeader(deviceauth.ClientIDHeader), c.Query("client_id"), suppliedClientID)
	if !ok {
		return
	}
	var req DeleteRequest
	if pathRaw != "" {
		req.Path = pathRaw
	}
	normalized, valid := normalizeRelativePath(pathRaw)
	if !valid {
		c.JSON(http.StatusBadRequest, gin.H{"error": "path contains illegal segments"})
		return
	}
	req.Path = normalized

	vaultID, err := defaultVaultID(h.DB, u.ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if err := deviceauth.CheckVaultAccess(h.DB, u.ID, string(did), vaultID); err != nil {
		h.writeDeviceAuthError(c, err)
		return
	}
	if !h.recordDeviceActivityWithDID(c, u.ID, vaultID, did) {
		return
	}
	vaultLock := h.vaultLock(vaultID)
	vaultLock.Lock()
	defer vaultLock.Unlock()
	pathLock := h.pathLock(vaultID + ":" + req.Path)
	pathLock.Lock()
	defer pathLock.Unlock()
	rowsAffected := int64(0)
	targetPath := ""
	stagedPath := ""
	err = h.DB.Transaction(func(tx *gorm.DB) error {
		var file models.File
		if err := tx.Where(
			"user_id = ? AND vault_id = ? AND path = ?",
			u.ID, vaultID, req.Path,
		).First(&file).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil
			}
			return err
		}
		targetPath = h.fileDiskPath(file)
		if file.IsDeleted {
			return nil
		}
		var err error
		stagedPath, err = stageDeletedContent(targetPath)
		if err != nil {
			return err
		}
		revision, err := nextVaultRevision(tx, vaultID)
		if err != nil {
			return err
		}
		now := time.Now()
		result := tx.Model(&models.File{}).Where("id = ?", file.ID).Updates(map[string]any{
			"is_deleted": true,
			"deleted_at": now,
			"revision":   revision,
		})
		rowsAffected = result.RowsAffected
		if result.Error != nil || result.RowsAffected == 0 {
			return result.Error
		}
		return tx.Model(&models.Vault{}).Where("id = ?", vaultID).
			UpdateColumn(
				"storage_used",
				gorm.Expr(
					"CASE WHEN storage_used >= ? THEN storage_used - ? ELSE 0 END",
					file.Size,
					file.Size,
				),
			).Error
	})
	if err != nil {
		restoreStagedContent(targetPath, stagedPath)
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	discardDeletedContent(targetPath, stagedPath)
	if rowsAffected == 0 {
		c.Status(http.StatusNoContent)
		return
	}
	h.notifyRevision(vaultID)
	c.Status(http.StatusNoContent)
}

func stageDeletedContent(targetPath string) (string, error) {
	if targetPath == "" {
		return "", nil
	}
	stagedPath := targetPath + ".backup-delete-" + uuid.NewString()
	if err := os.Rename(targetPath, stagedPath); err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	return stagedPath, nil
}

func restoreStagedContent(targetPath, stagedPath string) {
	if targetPath == "" || stagedPath == "" {
		return
	}
	_ = os.Rename(stagedPath, targetPath)
}

func discardDeletedContent(targetPath, stagedPath string) {
	if stagedPath != "" {
		_ = os.Remove(stagedPath)
		return
	}
	if targetPath != "" {
		_ = os.Remove(targetPath)
	}
}

// MarshalCheckResponse 给测试用，把 CheckResponse 序列化为 JSON
func MarshalCheckResponse(r CheckResponse) ([]byte, error) {
	return json.Marshal(r)
}

func (h *Handler) recordDeviceActivityWithDID(c *gin.Context, userID uint, vaultID string, did jwt.DeviceID) bool {
	err := deviceauth.Touch(
		h.DB,
		userID,
		vaultID,
		string(did),
		deviceauth.DecodeDeviceName(c.GetHeader(deviceauth.DeviceNameHeader)),
		nil,
		time.Now(),
	)
	if err != nil {
		h.writeDeviceError(c, err)
		return false
	}
	return true
}
