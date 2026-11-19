// Package shares 提供公开分享的创建与管理接口
package shares

import (
	"crypto/rand"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"

	"github.com/helantianshen/oss-sync/internal/auth"
	"github.com/helantianshen/oss-sync/internal/config"
	"github.com/helantianshen/oss-sync/internal/models"
	"github.com/helantianshen/oss-sync/internal/synclock"
	"github.com/helantianshen/oss-sync/internal/vaultaccess"
)

// Handler 管理 Vault 文件分享
type Handler struct {
	DB  *gorm.DB
	Cfg *config.Config
}

// New 创建分享路由处理器
func New(db *gorm.DB, cfg *config.Config) *Handler {
	return &Handler{DB: db, Cfg: cfg}
}

const (
	shareIDLen    = 6
	maxIDAttempts = 8
)

var base62Alphabet = []byte("0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz")

type createRequest struct {
	VaultID            string `json:"vault_id"`
	TargetPath         string `json:"target_path" binding:"required"`
	IsFolder           bool   `json:"is_folder"`
	AllowCopy          bool   `json:"allow_copy"`
	RecursiveBacklinks bool   `json:"recursive_backlinks"`
	// Visibility 取 public / password / login，缺省 public
	Visibility string `json:"visibility"`
	// SharePassword 是 password 可见性的访问密码明文，仅创建时接收
	SharePassword string `json:"share_password"`
}

type updateRequest struct {
	AllowCopy  *bool   `json:"allow_copy"`
	TargetPath *string `json:"target_path"`
	// Visibility 与 SharePassword 可单独或一起更新；password 模式必须提供新密码
	Visibility    string `json:"visibility"`
	SharePassword string `json:"share_password"`
}

type shareOut struct {
	ShareID    string `json:"share_id"`
	VaultID    string `json:"vault_id"`
	TargetPath string `json:"target_path"`
	IsFolder   bool   `json:"is_folder"`
	AllowCopy  bool   `json:"allow_copy"`
	Visibility string `json:"visibility"`
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
		g.PATCH("/:id", h.Update)
		g.DELETE("/:id", h.Delete)
	}
}

// CreateWeb 是网页控制台使用的分享创建服务，校验文件存在并生成分享 ID
func (h *Handler) CreateWeb(userID uint, vaultID, targetPath string, isFolder, allowCopy bool, access ShareAccess) (string, error) {
	targetPath = strings.TrimSpace(targetPath)
	if targetPath == "" || !isSafeSharePath(targetPath) {
		return "", fmt.Errorf("路径包含非法内容")
	}
	so, err := h.createOne(userID, vaultID, targetPath, isFolder, allowCopy, access)
	if err != nil {
		return "", err
	}
	return so.ShareID, nil
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
	vault, role, err := h.resolveVault(u.ID, strings.TrimSpace(req.VaultID))
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "vault not found"})
		return
	}
	if !vaultaccess.CanManage(role) {
		c.JSON(http.StatusForbidden, gin.H{"error": "insufficient vault permission"})
		return
	}
	vaultID := vault.ID

	extra := []shareOut{}
	if req.RecursiveBacklinks && !req.IsFolder {
		links, err := h.collectBacklinks(vault.OwnerID, vaultID, req.TargetPath)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		for _, p := range links {
			so, err := h.createOne(vault.OwnerID, vaultID, p, false, req.AllowCopy, ShareAccess{Visibility: req.Visibility, Password: req.SharePassword})
			if err != nil {
				continue
			}
			extra = append(extra, so)
		}
	}

	so, err := h.createOne(vault.OwnerID, vaultID, req.TargetPath, req.IsFolder, req.AllowCopy, ShareAccess{Visibility: req.Visibility, Password: req.SharePassword})
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

// ShareAccess 描述一次分享创建时的访问控制意图
// Visibility 取 public / password / login；password 必须提供明文 Password
type ShareAccess struct {
	Visibility string
	Password   string
}

// normalizeShareAccess 校验并规范化访问控制，password 模式必须带密码
func normalizeShareAccess(access ShareAccess) (string, string, error) {
	visibility := strings.TrimSpace(access.Visibility)
	switch visibility {
	case "", "public":
		return "public", "", nil
	case "password":
		password := access.Password
		if strings.TrimSpace(password) == "" {
			return "", "", errors.New("密码访问必须设置密码")
		}
		hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
		if err != nil {
			return "", "", err
		}
		return visibility, string(hash), nil
	case "login":
		return visibility, "", nil
	default:
		return "", "", fmt.Errorf("不支持的分享可见范围: %s", visibility)
	}
}

func (h *Handler) createOne(
	userID uint,
	vaultID, targetPath string,
	isFolder, allowCopy bool,
	access ShareAccess,
) (shareOut, error) {
	if !isFolder {
		lock := synclock.Vault(vaultID)
		lock.Lock()
		defer lock.Unlock()
		var cnt int64
		err := h.DB.Model(&models.File{}).
			Where(
				"user_id = ? AND vault_id = ? AND path = ? AND is_deleted = ?",
				userID, vaultID, targetPath, false,
			).
			Count(&cnt).Error
		if err != nil {
			return shareOut{}, err
		}
		if cnt == 0 {
			return shareOut{}, fmt.Errorf("file not found: %s", targetPath)
		}
	}

	visibility, passwordHash, err := normalizeShareAccess(access)
	if err != nil {
		return shareOut{}, err
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
			ShareID:       id,
			UserID:        userID,
			VaultID:       vaultID,
			TargetPath:    targetPath,
			IsFolder:      isFolder,
			AllowCopy:     allowCopy,
			Visibility:    visibility,
			SharePassword: passwordHash,
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
		VaultID:    vaultID,
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
	query := h.DB.Where("1 = 0")
	if vaultID := strings.TrimSpace(c.Query("vault_id")); vaultID != "" {
		vault, _, err := h.resolveVault(u.ID, vaultID)
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "vault not found"})
			return
		}
		query = h.DB.Where("vault_id = ?", vault.ID)
	} else {
		var owned []models.Vault
		if err := h.DB.Where("owner_id = ?", u.ID).Find(&owned).Error; err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		ids := make([]string, 0, len(owned))
		for _, vault := range owned {
			ids = append(ids, vault.ID)
		}
		var members []models.VaultMember
		if err := h.DB.Where("user_id = ?", u.ID).Find(&members).Error; err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		for _, member := range members {
			ids = append(ids, member.VaultID)
		}
		if len(ids) > 0 {
			query = h.DB.Where("vault_id IN ?", ids)
		}
	}
	query.Order("created_at desc").Find(&rows)
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
	if err := h.DB.Where("share_id = ?", id).First(&s).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "share not found"})
		return
	}
	_, role, err := h.resolveVault(u.ID, s.VaultID)
	if err != nil || !vaultaccess.CanManage(role) {
		c.JSON(http.StatusNotFound, gin.H{"error": "share not found"})
		return
	}
	c.JSON(http.StatusOK, toOut(s))
}

func (h *Handler) Update(c *gin.Context) {
	u, ok := auth.RequireUser(c)
	if !ok {
		return
	}
	var req updateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request", "code": "invalid_share_update"})
		return
	}
	if req.AllowCopy == nil && req.Visibility == "" && req.TargetPath == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "no share fields to update", "code": "invalid_share_update"})
		return
	}
	var share models.Share
	if err := h.DB.Where("share_id = ?", c.Param("id")).First(&share).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "share not found", "code": "share_not_found"})
		return
	}
	_, role, err := h.resolveVault(u.ID, share.VaultID)
	if err != nil || !vaultaccess.CanManage(role) {
		c.JSON(http.StatusNotFound, gin.H{"error": "share not found", "code": "share_not_found"})
		return
	}
	updates := map[string]any{}
	if req.TargetPath != nil {
		path := *req.TargetPath
		if share.IsFolder || path == "" || !isSafeSharePath(path) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid share target", "code": "invalid_share_target"})
			return
		}
		lock := synclock.Vault(share.VaultID)
		lock.Lock()
		defer lock.Unlock()
		var file models.File
		err := h.DB.Select("id").Where("user_id = ? AND vault_id = ? AND path = ? AND is_deleted = ?", share.UserID, share.VaultID, path, false).Take(&file).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "share target has not been synced", "code": "share_target_missing"})
			return
		}
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "could not verify share target", "code": "share_update_failed"})
			return
		}
		share.TargetPath = path
		updates["target_path"] = path
	}
	if req.AllowCopy != nil {
		share.AllowCopy = *req.AllowCopy
		updates["allow_copy"] = share.AllowCopy
	}
	if req.Visibility != "" {
		visibility, passwordHash, accessErr := normalizeShareAccess(ShareAccess{
			Visibility: req.Visibility,
			Password:   req.SharePassword,
		})
		if accessErr != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": accessErr.Error(), "code": "invalid_share_visibility"})
			return
		}
		share.Visibility = visibility
		share.SharePassword = passwordHash
		updates["visibility"] = visibility
		updates["share_password"] = passwordHash
	}
	result := h.DB.Model(&share).Updates(updates)
	if result.Error != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "update share failed", "code": "share_update_failed"})
		return
	}
	if result.RowsAffected == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "share not found", "code": "share_not_found"})
		return
	}
	c.JSON(http.StatusOK, toOut(share))
}

func (h *Handler) Delete(c *gin.Context) {
	u, ok := auth.RequireUser(c)
	if !ok {
		return
	}
	id := c.Param("id")
	var share models.Share
	if err := h.DB.Where("share_id = ?", id).First(&share).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "share not found"})
		return
	}
	_, role, err := h.resolveVault(u.ID, share.VaultID)
	if err != nil || !vaultaccess.CanManage(role) {
		c.JSON(http.StatusNotFound, gin.H{"error": "share not found"})
		return
	}
	res := h.DB.Where("share_id = ?", id).Delete(&models.Share{})
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
	visibility := s.Visibility
	if visibility == "" {
		visibility = "public"
	}
	return shareOut{
		ShareID:    s.ShareID,
		VaultID:    s.VaultID,
		TargetPath: s.TargetPath,
		IsFolder:   s.IsFolder,
		AllowCopy:  s.AllowCopy,
		Visibility: visibility,
		Views:      s.Views,
		URL:        "/p/" + s.ShareID,
		CreatedAt:  created,
	}
}

func (h *Handler) resolveVault(userID uint, requested string) (models.Vault, string, error) {
	var vault models.Vault
	if requested != "" {
		return vaultaccess.Resolve(h.DB, userID, requested)
	}
	if err := h.DB.Where("owner_id = ?", userID).Order("is_default desc, created_at asc").First(&vault).Error; err == nil {
		return vault, vaultaccess.RoleOwner, nil
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		return models.Vault{}, "", err
	}
	var member models.VaultMember
	if err := h.DB.Where("user_id = ?", userID).Order("created_at asc").First(&member).Error; err != nil {
		return models.Vault{}, "", err
	}
	return vaultaccess.Resolve(h.DB, userID, member.VaultID)
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
