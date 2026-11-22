package syncapi

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/helantianshen/oss-sync/internal/auth"
	"github.com/helantianshen/oss-sync/internal/deviceauth"
	"github.com/helantianshen/oss-sync/internal/models"
	"github.com/helantianshen/oss-sync/internal/settingspolicy"
)

// strategyResponse 是 GET /api/vaults/:vault_id/sync/strategy 的响应
type strategyResponse struct {
	Policy                  string `json:"policy"`
	EffectiveMode           string `json:"effective_mode"`
	MinDebounceSec          int    `json:"min_debounce_sec"`
	LongPollWaitSec         int    `json:"long_poll_wait_sec"`
	CommunityPluginSettings bool   `json:"community_plugin_settings"`
}

// V2Strategy 处理 GET /api/vaults/:vault_id/sync/strategy
// effective_mode 由服务端根据仓库策略和客户端选择计算
func (h *Handler) V2Strategy(c *gin.Context) {
	u, vault, ok := h.requireV2Vault(c)
	if !ok {
		return
	}
	did, ok := auth.RequireDeviceID(c, c.GetHeader(deviceauth.ClientIDHeader), c.Query("client_id"))
	if !ok {
		return
	}
	if err := deviceauth.CheckVaultAccess(h.DB, u.ID, string(did), vault.ID); err != nil {
		h.writeDeviceAuthError(c, err)
		return
	}
	if !h.recordDeviceActivityWithDID(c, u.ID, vault.ID, did) {
		return
	}
	timing, err := settingspolicy.EffectiveForUser(h.DB, u.ID, int64(h.Cfg.Server.MaxFileSizeMB)<<20)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	policy := settingspolicy.SyncModeUserChoice
	var system models.SystemSetting
	if err := h.DB.Select("sync_mode").First(&system, 1).Error; err != nil {
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "读取同步策略失败"})
			return
		}
	} else {
		policy, err = settingspolicy.ParseSyncMode(system.SyncMode)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "同步策略配置无效"})
			return
		}
	}

	effective := policy
	if policy == settingspolicy.SyncModeUserChoice {
		pref, parseErr := settingspolicy.ParseSyncMode(c.Query("mode"))
		if parseErr != nil {
			effective = settingspolicy.SyncModeShortPoll
		} else {
			switch pref {
			case settingspolicy.SyncModeLongPoll:
				effective = settingspolicy.SyncModeLongPoll
			default:
				effective = settingspolicy.SyncModeShortPoll
			}
		}
	}
	c.JSON(http.StatusOK, strategyResponse{
		Policy:                  string(policy),
		EffectiveMode:           string(effective),
		MinDebounceSec:          timing.SyncDebounceSec,
		LongPollWaitSec:         timing.LongPollWaitSec,
		CommunityPluginSettings: true,
	})
}
