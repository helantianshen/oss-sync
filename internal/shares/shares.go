package shares

import (
	"crypto/rand"
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/oss/oss-server/internal/auth"
	"github.com/oss/oss-server/internal/config"
	"github.com/oss/oss-server/internal/models"
)

type Handler struct {
	DB  *gorm.DB
	Cfg *config.Config
}

func New(db *gorm.DB, cfg *config.Config) *Handler {
	return &Handler{DB: db, Cfg: cfg}
}

const (
	shareIDLen    = 6
	maxIDAttempts = 8
)

var base62Alphabet = []byte("0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz")

type createRequest struct {
	TargetPath         string `json:"target_path" binding:"required"`
	IsFolder           bool   `json:"is_folder"`
	AllowCopy          bool   `json:"allow_copy"`
	RecursiveBacklinks bool   `json:"recursive_backlinks"`
}

type shareOut struct {
	ShareID    string `json:"share_id"`
	TargetPath string `json:"target_path"`
	IsFolder   bool   `json:"is_folder"`
	AllowCopy  bool   `json:"allow_copy"`
	Views      int    `json:"views"`
	URL        string `json:"url"`
	CreatedAt  string `json:"created_at"`
}

type createResponse struct {
	ShareID    string     `json:"share_id"`
	URL        string     `json:"url"`
	TargetPath string     `json:"target_path"`
	IsFolder   bool       `json:"is_folder"`
	Extra      []shareOut `json:"extra,omitempty"`
}

func (h *Handler) Register(r *gin.Engine) {
	g := r.Group("/api/shares", auth.Middleware(h.DB, h.Cfg))
	{
		g.POST("", h.Create)
		g.GET("", h.List)
		g.GET("/:id", h.Get)
		g.DELETE("/:id", h.Delete)
	}
}

func (h *Handler) Create(c *gin.Context) {
	u, ok := auth.RequireUser(c)
	if !ok {
		return
	}
	var req createRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	req.TargetPath = strings.TrimSpace(req.TargetPath)
	if req.TargetPath == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "target_path is required"})
		return
	}
	if !isSafeSharePath(req.TargetPath) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "target_path contains illegal segments"})
		return
	}

	extra := []shareOut{}
	if req.RecursiveBacklinks && !req.IsFolder {
		links, err := h.collectBacklinks(u.ID, req.TargetPath)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		for _, p := range links {
			so, err := h.createOne(u.ID, p, false, req.AllowCopy)
			if err != nil {
				continue
			}
			extra = append(extra, so)
		}
	}

	so, err := h.createOne(u.ID, req.TargetPath, req.IsFolder, req.AllowCopy)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, createResponse{
		ShareID:    so.ShareID,
		URL:        so.URL,
		TargetPath: so.TargetPath,
		IsFolder:   so.IsFolder,
		Extra:      extra,
	})
}

func (h *Handler) createOne(userID uint, targetPath string, isFolder, allowCopy bool) (shareOut, error) {
	if !isFolder {
		var cnt int64
		h.DB.Model(&models.File{}).
			Where("user_id = ? AND path = ? AND is_deleted = ?", userID, targetPath, false).
			Count(&cnt)
		if cnt == 0 {
			return shareOut{}, fmt.Errorf("file not found: %s", targetPath)
		}
	}

	var shareID string
	var lastErr error
	for attempt := 0; attempt < maxIDAttempts; attempt++ {
		id, err := genShareID()
		if err != nil {
			lastErr = err
			continue
		}
		rec := models.Share{
			ShareID:    id,
			UserID:     userID,
			TargetPath: targetPath,
			IsFolder:   isFolder,
			AllowCopy:  allowCopy,
		}
		err = h.DB.Create(&rec).Error
		if err == nil {
			shareID = id
			lastErr = nil
			break
		}
		lastErr = err
	}
	if lastErr != nil {
		return shareOut{}, lastErr
	}
	return shareOut{
		ShareID:    shareID,
		TargetPath: targetPath,
		IsFolder:   isFolder,
		AllowCopy:  allowCopy,
		URL:        "/p/" + shareID,
	}, nil
}

func (h *Handler) List(c *gin.Context) {
	u, ok := auth.RequireUser(c)
	if !ok {
		return
	}
	var rows []models.Share
	h.DB.Where("user_id = ?", u.ID).Order("created_at desc").Find(&rows)
	out := make([]shareOut, 0, len(rows))
	for _, r := range rows {
		out = append(out, toOut(r))
	}
	c.JSON(http.StatusOK, gin.H{"shares": out})
}

func (h *Handler) Get(c *gin.Context) {
	u, ok := auth.RequireUser(c)
	if !ok {
		return
	}
	id := c.Param("id")
	var s models.Share
	if err := h.DB.Where("share_id = ? AND user_id = ?", id, u.ID).First(&s).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "share not found"})
		return
	}
	c.JSON(http.StatusOK, toOut(s))
}

func (h *Handler) Delete(c *gin.Context) {
	u, ok := auth.RequireUser(c)
	if !ok {
		return
	}
	id := c.Param("id")
	res := h.DB.Where("share_id = ? AND user_id = ?", id, u.ID).Delete(&models.Share{})
	if res.Error != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": res.Error.Error()})
		return
	}
	if res.RowsAffected == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "share not found"})
		return
	}
	c.Status(http.StatusNoContent)
}

func toOut(s models.Share) shareOut {
	created := ""
	if !s.CreatedAt.IsZero() {
		created = s.CreatedAt.Format("2006-01-02T15:04:05Z07:00")
	}
	return shareOut{
		ShareID:    s.ShareID,
		TargetPath: s.TargetPath,
		IsFolder:   s.IsFolder,
		AllowCopy:  s.AllowCopy,
		Views:      s.Views,
		URL:        "/p/" + s.ShareID,
		CreatedAt:  created,
	}
}

func genShareID() (string, error) {
	b := make([]byte, shareIDLen)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	out := make([]byte, shareIDLen)
	for i, x := range b {
		out[i] = base62Alphabet[int(x)%len(base62Alphabet)]
	}
	return string(out), nil
}
