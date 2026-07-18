package vaults

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
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

func (h *Handler) Register(r *gin.Engine) {
	g := r.Group("/api/vaults", auth.Middleware(h.DB, h.Cfg))
	{
		g.GET("", h.List)
		g.POST("", h.Create)
		g.GET("/:vault_id", h.Get)
		g.PATCH("/:vault_id", h.Update)
		g.DELETE("/:vault_id", h.Archive)
	}
}

type createRequest struct {
	Name        string `json:"name" binding:"required,min=1,max=128"`
	Description string `json:"description" binding:"max=512"`
}

type updateRequest struct {
	Name        *string `json:"name"`
	Description *string `json:"description"`
}

type vaultOut struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Description  string `json:"description"`
	IsDefault    bool   `json:"is_default"`
	StorageQuota int64  `json:"storage_quota"`
	StorageUsed  int64  `json:"storage_used"`
	HeadRevision int64  `json:"head_revision"`
	CreatedAt    string `json:"created_at"`
	UpdatedAt    string `json:"updated_at"`
}

func (h *Handler) List(c *gin.Context) {
	u, ok := auth.RequireUser(c)
	if !ok {
		return
	}
	var rows []models.Vault
	if err := h.DB.Where("owner_id = ?", u.ID).Order("is_default desc, created_at asc").Find(&rows).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	out := make([]vaultOut, 0, len(rows))
	for _, row := range rows {
		out = append(out, h.toOut(row))
	}
	c.JSON(http.StatusOK, gin.H{"vaults": out})
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
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name is required"})
		return
	}
	vault := models.Vault{
		ID:          uuid.NewString(),
		OwnerID:     u.ID,
		Name:        req.Name,
		Description: strings.TrimSpace(req.Description),
	}
	if err := h.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&vault).Error; err != nil {
			return err
		}
		if err := tx.Create(&models.VaultSetting{VaultID: vault.ID}).Error; err != nil {
			return err
		}
		return tx.Create(&models.VaultSyncState{VaultID: vault.ID}).Error
	}); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusCreated, h.toOut(vault))
}

func (h *Handler) Get(c *gin.Context) {
	u, ok := auth.RequireUser(c)
	if !ok {
		return
	}
	vault, ok := h.requireOwned(c, u.ID)
	if !ok {
		return
	}
	c.JSON(http.StatusOK, h.toOut(vault))
}

func (h *Handler) Update(c *gin.Context) {
	u, ok := auth.RequireUser(c)
	if !ok {
		return
	}
	vault, ok := h.requireOwned(c, u.ID)
	if !ok {
		return
	}
	var req updateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	updates := map[string]any{}
	if req.Name != nil {
		name := strings.TrimSpace(*req.Name)
		if name == "" || len(name) > 128 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "name must be 1-128 characters"})
			return
		}
		updates["name"] = name
		vault.Name = name
	}
	if req.Description != nil {
		description := strings.TrimSpace(*req.Description)
		if len(description) > 512 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "description must be at most 512 characters"})
			return
		}
		updates["description"] = description
		vault.Description = description
	}
	if len(updates) > 0 {
		if err := h.DB.Model(&models.Vault{}).Where("id = ? AND owner_id = ?", vault.ID, u.ID).Updates(updates).Error; err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		if err := h.DB.Where("id = ? AND owner_id = ?", vault.ID, u.ID).First(&vault).Error; err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
	}
	c.JSON(http.StatusOK, h.toOut(vault))
}

func (h *Handler) Archive(c *gin.Context) {
	u, ok := auth.RequireUser(c)
	if !ok {
		return
	}
	vault, ok := h.requireOwned(c, u.ID)
	if !ok {
		return
	}
	if vault.IsDefault {
		c.JSON(http.StatusConflict, gin.H{"error": "default vault cannot be archived"})
		return
	}
	if err := h.DB.Delete(&vault).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.Status(http.StatusNoContent)
}

func (h *Handler) requireOwned(c *gin.Context, userID uint) (models.Vault, bool) {
	var vault models.Vault
	if err := h.DB.Where("id = ? AND owner_id = ?", c.Param("vault_id"), userID).First(&vault).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "vault not found"})
		return models.Vault{}, false
	}
	return vault, true
}

func (h *Handler) toOut(vault models.Vault) vaultOut {
	var state models.VaultSyncState
	_ = h.DB.Where("vault_id = ?", vault.ID).First(&state).Error
	return vaultOut{
		ID:           vault.ID,
		Name:         vault.Name,
		Description:  vault.Description,
		IsDefault:    vault.IsDefault,
		StorageQuota: vault.StorageQuota,
		StorageUsed:  vault.StorageUsed,
		HeadRevision: state.HeadRevision,
		CreatedAt:    vault.CreatedAt.Format(time.RFC3339),
		UpdatedAt:    vault.UpdatedAt.Format(time.RFC3339),
	}
}
