package webui

import (
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/oss/oss-server/internal/auth"
	"github.com/oss/oss-server/internal/config"
	"github.com/oss/oss-server/internal/models"
	"github.com/oss/oss-server/internal/settingspolicy"
)

const (
	hardMaxLongPollWaitSec      = 30
	hardMaxSyncDebounceSec      = 3600
	hardMaxRecycleBinDays       = 3650
	hardMaxHistoryRetentionDays = 3650
	bytesPerMegabyte            = int64(1 << 20)
)

type adminSystemData struct {
	RegistrationEnabled    bool
	CustomFragmentsEnabled bool
	SyncMode               settingspolicy.SyncMode
	DefaultRecycleBinDays  int
	MaxLongPollWaitSec     int
	MaxSyncDebounceSec     int
	MaxRecycleBinDays      int
	HistoryRetentionDays   int
	MaxVaultStorageMB      int64
	MaxUploadSizeMB        int64
	ConfigMaxUploadSizeMB  int64
	Backups                []adminBackupRow
	Metrics                systemMetrics
	UserCount              int64
	VaultCount             int64
	DeviceCount            int64
	VaultStorageUsed       int64
	RecentHistory          []models.FileHistory
	RecentDevices          []recentDeviceRow
	DatabaseDriver         string
	DatabaseDSN            string
	Error                  string
	Saved                  bool
}

type adminSystemInput struct {
	CustomFragmentsEnabled bool
	SyncMode               settingspolicy.SyncMode
	DefaultRecycleBinDays  int
	MaxLongPollWaitSec     int
	MaxSyncDebounceSec     int
	MaxRecycleBinDays      int
	HistoryRetentionDays   int
	MaxVaultStorageBytes   int64
	MaxUploadSizeBytes     int64
}

type adminSystemInputError struct {
	Field   string
	Message string
}

func (e *adminSystemInputError) Error() string {
	return fmt.Sprintf("%s: %s", e.Field, e.Message)
}

func (h *Handler) adminSystemPage(c *gin.Context) {
	data := adminSystemData{
		SyncMode: settingspolicy.SyncModeUserChoice,
		Error:    c.Query("error"),
		Saved:    c.Query("saved") == "1",
	}
	if enabled, err := auth.RegistrationEnabled(h.DB, h.Cfg.Auth.AllowAnonymousRegistration); err == nil {
		data.RegistrationEnabled = enabled
	}
	var setting models.SystemSetting
	if err := h.DB.Where("id = 1").First(&setting).Error; err == nil {
		data.CustomFragmentsEnabled = setting.CustomFragmentsEnabled
		if mode, parseErr := settingspolicy.ParseSyncMode(setting.SyncMode); parseErr == nil {
			data.SyncMode = mode
		}
	}
	if !data.CustomFragmentsEnabled {
		data.CustomFragmentsEnabled = settingspolicy.CustomFragmentsEnabled(h.DB)
	}
	configUploadBytes := h.configuredMaxUploadBytes()
	limits := settingspolicy.LimitsFor(setting, configUploadBytes)
	effective := settingspolicy.Resolve(setting, models.UserSetting{}, configUploadBytes)
	data.DefaultRecycleBinDays = effective.RecycleBinDays
	data.MaxLongPollWaitSec = limits.LongPollWaitSec
	data.MaxSyncDebounceSec = limits.SyncDebounceSec
	data.MaxRecycleBinDays = limits.RecycleBinDays
	data.HistoryRetentionDays = limits.HistoryRetentionDays
	data.MaxVaultStorageMB = limits.VaultStorageBytes / bytesPerMegabyte
	data.MaxUploadSizeMB = limits.UploadSizeBytes / bytesPerMegabyte
	data.ConfigMaxUploadSizeMB = configUploadBytes / bytesPerMegabyte
	data.DatabaseDriver = h.Cfg.Database.Driver
	data.DatabaseDSN = databaseStatusDSN(h.Cfg.Database.Driver, h.Cfg.Database.DSN)

	h.render(c, http.StatusOK, "admin-system", h.t(c, "page.admin_system"), "admin", "admin-system", data)
}

func (h *Handler) adminDataPage(c *gin.Context) {
	data := adminSystemData{
		Error: c.Query("error"),
	}
	if rows, err := h.backupRows(); err == nil {
		data.Backups = rows
	}
	data.Metrics = readSystemMetrics(h.Cfg.Storage.DataDir)
	_ = h.DB.Model(&models.User{}).Count(&data.UserCount).Error
	_ = h.DB.Model(&models.Vault{}).Count(&data.VaultCount).Error
	_ = h.DB.Model(&models.ClientDevice{}).Count(&data.DeviceCount).Error
	_ = h.DB.Model(&models.Vault{}).Select("COALESCE(SUM(storage_used), 0)").Scan(&data.VaultStorageUsed).Error
	_ = h.DB.Order("created_at desc").Limit(10).Find(&data.RecentHistory).Error
	h.loadAdminRecentDevices(&data)
	h.render(c, http.StatusOK, "admin-data", h.t(c, "page.admin_data"), "admin", "admin-data", data)
}

func (h *Handler) loadAdminRecentDevices(data *adminSystemData) {
	var syncs []models.DeviceVault
	if err := h.DB.Order("last_sync_at desc").Limit(8).Find(&syncs).Error; err != nil {
		return
	}
	for _, sync := range syncs {
		var device models.ClientDevice
		var vault models.Vault
		if h.DB.Where("user_id = ? AND client_id = ?", sync.UserID, sync.ClientID).First(&device).Error != nil ||
			h.DB.Where("id = ?", sync.VaultID).First(&vault).Error != nil {
			continue
		}
		data.RecentDevices = append(data.RecentDevices, recentDeviceRow{DeviceName: device.Name, VaultName: vault.Name, LastSyncAt: sync.LastSyncAt})
	}
}

func databaseStatusDSN(driver, dsn string) string {
	if driver != "postgres" {
		return dsn
	}
	parsed, err := url.Parse(dsn)
	if err != nil {
		return "已配置"
	}
	if parsed.User != nil {
		username := parsed.User.Username()
		if username != "" {
			parsed.User = url.User(username)
		} else {
			parsed.User = nil
		}
	}
	parsed.RawQuery = ""
	return parsed.String()
}

func (h *Handler) adminSaveSystem(c *gin.Context) {
	input, err := parseAdminSystemInput(c.Request.PostForm, h.configuredMaxUploadBytes())
	if err != nil {
		c.Redirect(http.StatusSeeOther, "/dashboard/admin/system?error="+url.QueryEscape(err.Error()))
		return
	}
	enabled := c.PostForm("registration_enabled") == "on"
	err = h.DB.Transaction(func(tx *gorm.DB) error {
		var setting models.SystemSetting
		findErr := tx.Where("id = 1").First(&setting).Error
		if errorsIsNotFound(findErr) {
			setting = models.SystemSetting{ID: 1}
			if err := tx.Create(&setting).Error; err != nil {
				return err
			}
		} else if findErr != nil {
			return findErr
		}
		return tx.Model(&models.SystemSetting{}).Where("id = 1").Updates(map[string]any{
			"registration_enabled":     enabled,
			"custom_fragments_enabled": input.CustomFragmentsEnabled,
			"sync_mode":                string(input.SyncMode),
			"default_recycle_bin_days": input.DefaultRecycleBinDays,
			"max_long_poll_wait_sec":   input.MaxLongPollWaitSec,
			"max_sync_debounce_sec":    input.MaxSyncDebounceSec,
			"max_recycle_bin_days":     input.MaxRecycleBinDays,
			"history_retention_days":   input.HistoryRetentionDays,
			"max_vault_storage_bytes":  input.MaxVaultStorageBytes,
			"max_upload_size_bytes":    input.MaxUploadSizeBytes,
		}).Error
	})
	if err != nil {
		c.Redirect(http.StatusSeeOther, "/dashboard/admin/system?error="+url.QueryEscape(h.t(c, "err.save_system_settings_failed")))
		return
	}
	c.Redirect(http.StatusSeeOther, "/dashboard/admin/system?saved=1")
}

func (h *Handler) adminSaveDatabase(c *gin.Context) {
	if err := config.SaveDatabaseConfig(c.PostForm("database_driver"), c.PostForm("database_dsn")); err != nil {
		c.Redirect(http.StatusSeeOther, "/dashboard/admin/system?error="+url.QueryEscape(err.Error()))
		return
	}
	c.Redirect(http.StatusSeeOther, "/dashboard/admin/system?saved=1")
}

func parseAdminSystemInput(form url.Values, configMaxUploadBytes int64) (adminSystemInput, error) {
	customFragmentsEnabled := form.Get("custom_fragments_enabled") == "on"
	syncMode, err := settingspolicy.ParseSyncMode(form.Get("sync_mode"))
	if err != nil {
		return adminSystemInput{}, invalidSystemField("sync_mode", "必须选择有效同步模式")
	}
	defaultRecycle, err := parseSystemInteger(form, "default_recycle_bin_days")
	if err != nil {
		return adminSystemInput{}, err
	}
	maxLongPoll, err := parseSystemInteger(form, "max_long_poll_wait_sec")
	if err != nil {
		return adminSystemInput{}, err
	}
	maxDebounce, err := parseSystemInteger(form, "max_sync_debounce_sec")
	if err != nil {
		return adminSystemInput{}, err
	}
	maxRecycle, err := parseSystemInteger(form, "max_recycle_bin_days")
	if err != nil {
		return adminSystemInput{}, err
	}
	historyRetention, err := parseSystemInteger(form, "history_retention_days")
	if err != nil {
		return adminSystemInput{}, err
	}
	maxVaultMB, err := parseSystemInteger(form, "max_vault_storage_mb")
	if err != nil {
		return adminSystemInput{}, err
	}
	maxUploadMB, err := parseSystemInteger(form, "max_upload_size_mb")
	if err != nil {
		return adminSystemInput{}, err
	}
	configMaxUploadMB := configMaxUploadBytes / bytesPerMegabyte
	switch {
	case maxLongPoll < 1 || maxLongPoll > hardMaxLongPollWaitSec:
		return adminSystemInput{}, invalidSystemField("max_long_poll_wait_sec", "必须为 1-30 秒")
	case maxDebounce < 3 || maxDebounce > hardMaxSyncDebounceSec:
		return adminSystemInput{}, invalidSystemField("max_sync_debounce_sec", "必须为 3-3600 秒")
	case maxRecycle < 1 || maxRecycle > hardMaxRecycleBinDays:
		return adminSystemInput{}, invalidSystemField("max_recycle_bin_days", "必须为 1-3650 天")
	case historyRetention < 0 || historyRetention > hardMaxHistoryRetentionDays:
		return adminSystemInput{}, invalidSystemField("history_retention_days", "必须为 0-3650 天")
	case defaultRecycle < 1 || defaultRecycle > maxRecycle:
		return adminSystemInput{}, invalidSystemField("default_recycle_bin_days", "不得超过回收站保留上限")
	case maxVaultMB < 0:
		return adminSystemInput{}, invalidSystemField("max_vault_storage_mb", "不能为负数")
	case maxUploadMB < 1 || maxUploadMB > configMaxUploadMB:
		return adminSystemInput{}, invalidSystemField("max_upload_size_mb", "不得超过部署配置上限")
	}
	return adminSystemInput{
		CustomFragmentsEnabled: customFragmentsEnabled,
		SyncMode:               syncMode,
		DefaultRecycleBinDays:  int(defaultRecycle),
		MaxLongPollWaitSec:     int(maxLongPoll),
		MaxRecycleBinDays:      int(maxRecycle),
		HistoryRetentionDays:   int(historyRetention),
		MaxSyncDebounceSec:     int(maxDebounce),
		MaxVaultStorageBytes:   maxVaultMB * bytesPerMegabyte,
		MaxUploadSizeBytes:     maxUploadMB * bytesPerMegabyte,
	}, nil
}

func parseSystemInteger(form url.Values, field string) (int64, error) {
	value, err := strconv.ParseInt(strings.TrimSpace(form.Get(field)), 10, 64)
	if err != nil || value > (int64(^uint64(0)>>1)/bytesPerMegabyte) {
		return 0, invalidSystemField(field, "必须为有效整数")
	}
	return value, nil
}

func invalidSystemField(field, message string) error {
	return &adminSystemInputError{Field: field, Message: message}
}

func (h *Handler) configuredMaxUploadBytes() int64 {
	maxMB := h.Cfg.Server.MaxFileSizeMB
	if maxMB <= 0 {
		maxMB = 100
	}
	return maxMB * bytesPerMegabyte
}
