package devices

import (
	"database/sql"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/oss/oss-server/internal/auth"
	"github.com/oss/oss-server/internal/config"
	"github.com/oss/oss-server/internal/models"
)

var ErrRevoked = errors.New("device has been revoked")

const (
	ClientIDHeader   = "X-OSS-Client-ID"
	DeviceNameHeader = "X-OSS-Device-Name"
)

type Handler struct {
	DB  *gorm.DB
	Cfg *config.Config
	now func() time.Time
}

func New(db *gorm.DB, cfg *config.Config) *Handler {
	return &Handler{DB: db, Cfg: cfg, now: time.Now}
}

func (h *Handler) Register(r *gin.Engine) {
	g := r.Group("/api/devices", auth.Middleware(h.DB, h.Cfg))
	{
		g.GET("", h.List)
		g.PATCH("/:client_id", h.Rename)
		g.DELETE("/:client_id", h.Revoke)
	}
}

type VaultCursorOut struct {
	VaultID        string `json:"vault_id"`
	VaultName      string `json:"vault_name"`
	LastCursor     int64  `json:"last_cursor"`
	HeadRevision   int64  `json:"head_revision"`
	PendingChanges int64  `json:"pending_changes"`
	LastSyncAt     string `json:"last_sync_at,omitempty"`
}

type DeviceOut struct {
	ClientID   string           `json:"client_id"`
	Name       string           `json:"name"`
	LastSeenAt string           `json:"last_seen_at"`
	CreatedAt  string           `json:"created_at"`
	RevokedAt  string           `json:"revoked_at,omitempty"`
	Stale      bool             `json:"stale"`
	IsCurrent  bool             `json:"is_current"`
	Vaults     []VaultCursorOut `json:"vaults"`
}

func (h *Handler) List(c *gin.Context) {
	user, ok := auth.RequireUser(c)
	if !ok {
		return
	}
	currentID := NormalizeClientID(c.GetHeader(ClientIDHeader))
	if currentID != "" {
		if err := Touch(h.DB, user.ID, "", currentID, DecodeDeviceName(c.GetHeader(DeviceNameHeader)), nil, h.now()); err != nil &&
			!errors.Is(err, ErrRevoked) {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
	}

	query := h.DB.Where("user_id = ?", user.ID)
	if c.Query("include_revoked") != "true" {
		query = query.Where("revoked_at IS NULL")
	}
	var rows []models.ClientDevice
	if err := query.Order("last_seen_at desc, created_at desc").Find(&rows).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	staleBefore := h.now().AddDate(0, 0, -h.Cfg.Sync.EffectiveDeviceStaleDays())
	out := make([]DeviceOut, 0, len(rows))
	for _, row := range rows {
		device := DeviceOut{
			ClientID:   row.ClientID,
			Name:       row.Name,
			LastSeenAt: formatTime(row.LastSeenAt),
			CreatedAt:  formatTime(row.CreatedAt),
			Stale:      row.RevokedAt.Valid || row.LastSeenAt.Before(staleBefore),
			IsCurrent:  row.ClientID == currentID,
			Vaults:     []VaultCursorOut{},
		}
		if row.RevokedAt.Valid {
			device.RevokedAt = formatTime(row.RevokedAt.Time)
		}
		var bindings []models.DeviceVault
		if err := h.DB.Where("user_id = ? AND client_id = ?", user.ID, row.ClientID).
			Order("vault_id asc").Find(&bindings).Error; err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		for _, binding := range bindings {
			var vault models.Vault
			if err := h.DB.Unscoped().Where("id = ? AND owner_id = ?", binding.VaultID, user.ID).
				First(&vault).Error; err != nil {
				continue
			}
			var state models.VaultSyncState
			_ = h.DB.Where("vault_id = ?", binding.VaultID).First(&state).Error
			pending := state.HeadRevision - binding.LastCursor
			if pending < 0 {
				pending = 0
			}
			device.Vaults = append(device.Vaults, VaultCursorOut{
				VaultID:        binding.VaultID,
				VaultName:      vault.Name,
				LastCursor:     binding.LastCursor,
				HeadRevision:   state.HeadRevision,
				PendingChanges: pending,
				LastSyncAt:     formatTime(binding.LastSyncAt),
			})
		}
		out = append(out, device)
	}
	c.JSON(http.StatusOK, gin.H{
		"devices":          out,
		"stale_after_days": h.Cfg.Sync.EffectiveDeviceStaleDays(),
	})
}

type renameRequest struct {
	Name string `json:"name"`
}

func (h *Handler) Rename(c *gin.Context) {
	user, ok := auth.RequireUser(c)
	if !ok {
		return
	}
	clientID := NormalizeClientID(c.Param("client_id"))
	var req renameRequest
	if clientID == "" || c.ShouldBindJSON(&req) != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid device rename request"})
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" || len([]rune(req.Name)) > 128 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name must be 1-128 characters"})
		return
	}
	result := h.DB.Model(&models.ClientDevice{}).
		Where("user_id = ? AND client_id = ? AND revoked_at IS NULL", user.ID, clientID).
		Update("name", req.Name)
	if result.Error != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": result.Error.Error()})
		return
	}
	if result.RowsAffected == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "device not found"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"client_id": clientID, "name": req.Name})
}

func (h *Handler) Revoke(c *gin.Context) {
	user, ok := auth.RequireUser(c)
	if !ok {
		return
	}
	clientID := NormalizeClientID(c.Param("client_id"))
	if clientID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid client id"})
		return
	}
	if clientID == NormalizeClientID(c.GetHeader(ClientIDHeader)) {
		c.JSON(http.StatusConflict, gin.H{"error": "current device cannot revoke itself"})
		return
	}
	now := h.now()
	result := h.DB.Model(&models.ClientDevice{}).
		Where("user_id = ? AND client_id = ? AND revoked_at IS NULL", user.ID, clientID).
		Update("revoked_at", now)
	if result.Error != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": result.Error.Error()})
		return
	}
	if result.RowsAffected == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "device not found"})
		return
	}
	c.Status(http.StatusNoContent)
}

func Touch(
	db *gorm.DB,
	userID uint,
	vaultID, clientID, deviceName string,
	acknowledgedCursor *int64,
	now time.Time,
) error {
	clientID = NormalizeClientID(clientID)
	if clientID == "" {
		return errors.New("invalid client id")
	}
	deviceName = strings.TrimSpace(deviceName)
	deviceName = truncateRunes(deviceName, 128)

	return db.Transaction(func(tx *gorm.DB) error {
		var device models.ClientDevice
		err := tx.Where("user_id = ? AND client_id = ?", userID, clientID).First(&device).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			device = models.ClientDevice{
				UserID:     userID,
				ClientID:   clientID,
				Name:       deviceName,
				LastSeenAt: now,
			}
			if err := tx.Create(&device).Error; err != nil {
				return err
			}
		} else if err != nil {
			return err
		} else {
			if device.RevokedAt.Valid {
				return ErrRevoked
			}
			updates := map[string]any{"last_seen_at": now}
			if device.Name == "" && deviceName != "" {
				updates["name"] = deviceName
			}
			if err := tx.Model(&models.ClientDevice{}).Where("id = ?", device.ID).
				Updates(updates).Error; err != nil {
				return err
			}
		}

		if vaultID == "" {
			return nil
		}
		var binding models.DeviceVault
		err = tx.Where(
			"user_id = ? AND client_id = ? AND vault_id = ?",
			userID, clientID, vaultID,
		).First(&binding).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			binding = models.DeviceVault{
				UserID:   userID,
				ClientID: clientID,
				VaultID:  vaultID,
			}
			if acknowledgedCursor != nil {
				binding.LastCursor = *acknowledgedCursor
				binding.LastSyncAt = now
			}
			return tx.Create(&binding).Error
		}
		if err != nil {
			return err
		}
		if acknowledgedCursor == nil {
			return nil
		}
		return tx.Model(&models.DeviceVault{}).Where("id = ?", binding.ID).Updates(map[string]any{
			"last_cursor": gorm.Expr(
				"CASE WHEN last_cursor < ? THEN ? ELSE last_cursor END",
				*acknowledgedCursor,
				*acknowledgedCursor,
			),
			"last_sync_at": now,
		}).Error
	})
}

func CheckActive(db *gorm.DB, userID uint, clientID string) error {
	clientID = NormalizeClientID(clientID)
	if clientID == "" {
		return errors.New("invalid client id")
	}
	var device models.ClientDevice
	err := db.Select("id", "revoked_at").
		Where("user_id = ? AND client_id = ?", userID, clientID).
		First(&device).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if device.RevokedAt.Valid {
		return ErrRevoked
	}
	return nil
}

func MinActiveCursor(
	db *gorm.DB,
	vaultID string,
	staleBefore time.Time,
) (int64, int64, error) {
	type cursorAggregate struct {
		Count     int64
		MinCursor sql.NullInt64
	}
	var aggregate cursorAggregate
	err := db.Table("device_vaults AS dv").
		Select("COUNT(*) AS count, MIN(dv.last_cursor) AS min_cursor").
		Joins(
			"JOIN client_devices AS cd ON cd.user_id = dv.user_id AND cd.client_id = dv.client_id",
		).
		Where(
			"dv.vault_id = ? AND cd.revoked_at IS NULL AND cd.last_seen_at >= ?",
			vaultID,
			staleBefore,
		).
		Scan(&aggregate).Error
	if err != nil {
		return 0, 0, err
	}
	if !aggregate.MinCursor.Valid {
		return 0, aggregate.Count, nil
	}
	return aggregate.MinCursor.Int64, aggregate.Count, nil
}

func NormalizeClientID(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 64 {
		return ""
	}
	for _, r := range value {
		if !(r == '-' || r == '_' || r == '.' ||
			(r >= '0' && r <= '9') ||
			(r >= 'a' && r <= 'z') ||
			(r >= 'A' && r <= 'Z')) {
			return ""
		}
	}
	return value
}

func truncateRunes(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit])
}

func DecodeDeviceName(value string) string {
	decoded, err := url.QueryUnescape(value)
	if err != nil {
		return strings.TrimSpace(value)
	}
	return strings.TrimSpace(decoded)
}

func formatTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339)
}
