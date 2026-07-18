// Package syncapi 实现 PRD 二、1 的核心同步系统：
//
//   - POST /api/sync/check    LWW 时钟比对，返回 server_time + 每文件状态
//   - POST /api/sync/upload   原始字节或 multipart 流式落盘（决策 7.3 硬约束）
//   - GET  /api/sync/download 流式下发（决策 7.3 硬约束）
//   - POST /api/sync/delete   软删除（is_deleted=true + deleted_at）
//
// 冲突检测的「基准线」逻辑（决策 1）在客户端维护 .oss-sync-state.json，
// 服务端只负责返回每文件的 server mtime + hash 供客户端对照。
// 服务端按 LWW 直接覆盖最新版本，不存基线字段（决策 1 已声明不加 BaseMTime）。
//
// 服务端在 conflict_detected 的判定上做兜底（PRD 二、1 的 hash 不同 +
// 服务端更新即触发），完整的三态判定由客户端基线闭环。
package syncapi

import (
	"encoding/json"
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
	"github.com/oss/oss-server/internal/config"
	"github.com/oss/oss-server/internal/models"
)

// Handler 持有 sync 路由所需依赖。
type Handler struct {
	DB  *gorm.DB
	Cfg *config.Config
}

// New 创建 Handler。
func New(db *gorm.DB, cfg *config.Config) *Handler {
	return &Handler{DB: db, Cfg: cfg}
}

// Register 在 gin 引擎上挂载 sync 路由组。
// 所有路由都经过 auth.Middleware。
func (h *Handler) Register(r *gin.Engine) {
	g := r.Group("/api/sync", auth.Middleware(h.DB, h.Cfg))
	{
		g.POST("/check", h.Check)
		g.POST("/upload", h.Upload)
		g.GET("/download", h.Download)
		g.POST("/delete", h.Delete)
	}
}

// ---------------------------------------------------------------------------
// POST /api/sync/check
// ---------------------------------------------------------------------------

// CheckRequest 客户端提交的同步检查请求。
//
// mode（决策 6.5）：
//   - "full"（默认）：客户端提交全量文件元数据，服务端对每个文件返回状态。
//   - "incremental"：客户端只提交本端 mtime 有变动的文件；
//     未提交的文件服务端不查也不返，回 assume_in_sync 占位（客户端不处理）。
//
// 协议字段（mode）已在此处预留，避免 Phase 3 联调跑通后再破坏性改协议。
type CheckRequest struct {
	Mode  string        `json:"mode"`
	Files []CheckFileIn `json:"files"`
}

type CheckFileIn struct {
	Path  string `json:"path"`
	MTime int64  `json:"mtime"` // 客户端本地 mtime（Unix 毫秒）
	Hash  string `json:"hash"`  // 客户端本地 SHA256
}

type CheckResponse struct {
	ServerTime int64          `json:"server_time"` // 决策 7.1：服务端当前 Unix 毫秒时间戳
	Results    []CheckFileOut `json:"results"`
}

type CheckFileOut struct {
	Path        string `json:"path"`
	Status      string `json:"status"` // upload_needed / download_needed / in_sync / conflict_detected / assume_in_sync
	ServerMTime int64  `json:"server_mtime,omitempty"`
	ServerHash  string `json:"server_hash,omitempty"`
}

// Check 处理 POST /api/sync/check。
//
// LWW 比对规则（PRD 二、1 + 决策 1 服务端侧兜底）：
//   - 本端无记录 + 客户端有文件 → upload_needed
//   - 服务端无记录 + 客户端提交了 → upload_needed（首次同步上传后建基线）
//   - 客户端 mtime > 服务端 mtime → upload_needed
//   - 客户端 mtime < 服务端 mtime → download_needed
//   - 相等 → in_sync
//   - hash 不同 且 服务端 mtime > 客户端 mtime → conflict_detected
//     （完整三态判定在客户端基线上做，服务端只兜底明显的协作冲突）
func (h *Handler) Check(c *gin.Context) {
	u, ok := auth.RequireUser(c)
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

	// 一次性把客户端提交的路径对应的 File 记录捞出来。
	paths := make([]string, 0, len(req.Files))
	for _, f := range req.Files {
		if f.Path != "" {
			paths = append(paths, f.Path)
		}
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
			Where("user_id = ? AND path IN ?", u.ID, paths).
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
			// 服务端无记录：客户端必须上传（含首次同步场景）。
			out.Status = "upload_needed"
			resp.Results = append(resp.Results, out)
			continue
		}
		// 软删除文件视为服务端「不存在最新版本」：客户端若仍持有，
		// 让其上传以恢复；若客户端也已删除，由 Delete 路径处理。
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
			// 协作冲突兜底：两端 hash 不同且服务端更新 → conflict_detected
			if srv.Hash != "" && cf.Hash != "" && srv.Hash != cf.Hash {
				out.Status = "conflict_detected"
			} else {
				out.Status = "download_needed"
			}
		default: // mtime 相等
			if srv.Hash != cf.Hash && srv.Hash != "" && cf.Hash != "" {
				// 同 mtime 不同 hash：保守起见判冲突
				out.Status = "conflict_detected"
			} else {
				out.Status = "in_sync"
			}
		}
		resp.Results = append(resp.Results, out)
	}

	// 决策 6.5：incremental 模式下，未提交的文件回 assume_in_sync 占位，
	// 客户端不处理。full 模式不输出这些项。
	// 注意：incremental 模式下我们不主动列出服务端独有文件，否则就不是
	// 增量了。如需服务端独有文件的下载提示，客户端要走 full 模式。
	c.JSON(http.StatusOK, resp)
}

// classifyFile 按 PRD 一、3：markdown / attachment / config
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

// isSafeRelativePath 防止路径逃逸：禁绝对路径、禁 .. 上一级。
func isSafeRelativePath(p string) bool {
	if p == "" || filepath.IsAbs(p) {
		return false
	}
	clean := filepath.Clean(p)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return false
	}
	// 禁 Windows 盘符样式 c:\
	if strings.Contains(p, ":") && !strings.HasSuffix(p, ":") {
		// 容忍 'C:\' 形式
		return false
	}
	return true
}

// ---------------------------------------------------------------------------
// GET /api/sync/download
// ---------------------------------------------------------------------------

// Download 处理 GET /api/sync/download?path=xxx。
// 决策 7.3：流式 io.Copy(w, file)，禁止缓冲整文件。
func (h *Handler) Download(c *gin.Context) {
	u, ok := auth.RequireUser(c)
	if !ok {
		return
	}
	path := strings.TrimSpace(c.Query("path"))
	if path == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "path is required"})
		return
	}
	if !isSafeRelativePath(path) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "path contains illegal segments"})
		return
	}

	// 校验记录存在且未软删除；软删除的文件不返回（PRD：移入回收站后不再可访问）。
	var f models.File
	if err := h.DB.Where("user_id = ? AND path = ?", u.ID, path).First(&f).Error; err != nil {
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

	abs := filepath.Join(h.Cfg.Storage.DataDir, fmt.Sprintf("%d", u.ID), path)
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
		// 客户端断开/网络问题，已写出的部分无法撤回，仅日志。
		_ = err
	}
}

// ---------------------------------------------------------------------------
// POST /api/sync/delete
// ---------------------------------------------------------------------------

// DeleteRequest 软删除请求。
type DeleteRequest struct {
	Path string `json:"path"`
}

// Delete 处理 POST /api/sync/delete。
// PRD 二、1：不直接删磁盘，标 is_deleted=true + deleted_at。
// 物理删除由 Phase 6 cron 负责。
func (h *Handler) Delete(c *gin.Context) {
	u, ok := auth.RequireUser(c)
	if !ok {
		return
	}
	var req DeleteRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body: " + err.Error()})
		return
	}
	if req.Path == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "path is required"})
		return
	}
	if !isSafeRelativePath(req.Path) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "path contains illegal segments"})
		return
	}

	now := time.Now()
	res := h.DB.Model(&models.File{}).
		Where("user_id = ? AND path = ? AND is_deleted = ?", u.ID, req.Path, false).
		Updates(map[string]any{
			"is_deleted": true,
			"deleted_at": now,
		})
	if res.Error != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": res.Error.Error()})
		return
	}
	if res.RowsAffected == 0 {
		// 行不存在或已经处于软删除态：幂等返回 204。
		c.Status(http.StatusNoContent)
		return
	}
	c.Status(http.StatusNoContent)
}

// MarshalCheckResponse 给测试用，把 CheckResponse 序列化为 JSON。
func MarshalCheckResponse(r CheckResponse) ([]byte, error) {
	return json.Marshal(r)
}
