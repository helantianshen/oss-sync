package webui

import (
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/helantianshen/oss-sync/internal/auth"
	"github.com/helantianshen/oss-sync/internal/consoletheme"
	"github.com/helantianshen/oss-sync/internal/models"
	"github.com/helantianshen/oss-sync/internal/settingspolicy"
)

type accountData struct {
	Username                string
	Role                    string
	CreatedAt               time.Time
	LongPollWaitSec         int
	SyncDebounceSec         int
	DefaultRecycleBinDays   int
	VaultStorageMB          int64
	UploadSizeMB            int64
	MaxLongPollWaitSec      int
	MaxSyncDebounceSec      int
	MaxRecycleBinDays       int
	MaxVaultStorageMB       int64
	MaxUploadSizeMB         int64
	Error                   string
	PasswordSaved           bool
	PreferenceSettingsSaved bool
	ConsoleThemeName        string
	ConsoleThemes           []consoletheme.Info
	ConsoleThemeSaved       bool
	WebLanguage             string
	PageSize                int
}

// accountData 实现 paginatedData，使账户页能显示当前每页条数
func (d accountData) paginationPage() Page {
	return Page{Page: 1, PageSize: d.PageSize, Total: 0, TotalPages: 1}
}

func (d accountData) paginationBasePath() string { return "/dashboard/account" }

type userPreferencesInputError struct {
	Message string
}

func (e *userPreferencesInputError) Error() string {
	return e.Message
}

func (h *Handler) accountPage(c *gin.Context) {
	user := h.webUser(c)
	data, err := h.loadAccountData(user)
	if err != nil {
		data.Error = h.t(c, "err.load_account_settings_failed")
		h.render(c, http.StatusInternalServerError, "account", h.t(c, "page.account"), "account", "account", data)
		return
	}
	data.Error = c.Query("error")
	data.PasswordSaved = c.Query("saved") == "1"
	data.PreferenceSettingsSaved = c.Query("settings_saved") == "1"
	data.ConsoleThemeSaved = c.Query("theme_saved") == "1"
	h.render(c, http.StatusOK, "account", h.t(c, "page.account"), "account", "account", data)
}

func (h *Handler) saveAccountSettings(c *gin.Context) {
	user := h.webUser(c)
	var system models.SystemSetting
	if err := h.DB.Where("id = 1").First(&system).Error; err != nil && !errorsIsNotFound(err) {
		c.Redirect(http.StatusSeeOther, "/dashboard/account?error="+url.QueryEscape(h.t(c, "err.load_limits_failed")))
		return
	}
	limits := settingspolicy.LimitsFor(system, h.configuredMaxUploadBytes())
	preferences, err := parseUserPreferences(c.Request.PostForm, limits)
	if err != nil {
		c.Redirect(http.StatusSeeOther, "/dashboard/account?error="+url.QueryEscape(err.Error()))
		return
	}
	err = h.DB.Transaction(func(tx *gorm.DB) error {
		var setting models.UserSetting
		findErr := tx.Where("user_id = ?", user.ID).First(&setting).Error
		if errorsIsNotFound(findErr) {
			setting = models.UserSetting{UserID: user.ID}
			if err := tx.Create(&setting).Error; err != nil {
				return err
			}
		} else if findErr != nil {
			return findErr
		}
		return tx.Model(&models.UserSetting{}).Where("user_id = ?", user.ID).Updates(map[string]any{
			"long_poll_wait_sec":       preferences.LongPollWaitSec,
			"sync_debounce_sec":        preferences.SyncDebounceSec,
			"default_recycle_bin_days": preferences.RecycleBinDays,
			"vault_storage_bytes":      preferences.VaultStorageBytes,
			"upload_size_bytes":        preferences.UploadSizeBytes,
			"page_size":                resolvePageSize(c.PostForm("page_size"), ""),
		}).Error
	})
	if err != nil {
		c.Redirect(http.StatusSeeOther, "/dashboard/account?error="+url.QueryEscape(h.t(c, "err.save_user_settings_failed")))
		return
	}
	c.Redirect(http.StatusSeeOther, "/dashboard/account?settings_saved=1#sync-settings")
}

func (h *Handler) loadAccountData(user *models.User) (accountData, error) {
	data := accountData{Username: user.Username, Role: user.Role, CreatedAt: user.CreatedAt}
	var system models.SystemSetting
	if err := h.DB.Where("id = 1").First(&system).Error; err != nil && !errorsIsNotFound(err) {
		return data, fmt.Errorf("load system settings: %w", err)
	}
	var setting models.UserSetting
	if err := h.DB.Where("user_id = ?", user.ID).First(&setting).Error; err != nil && !errorsIsNotFound(err) {
		return data, fmt.Errorf("load user settings: %w", err)
	}
	configUploadBytes := h.configuredMaxUploadBytes()
	limits := settingspolicy.LimitsFor(system, configUploadBytes)
	effective := settingspolicy.Resolve(system, setting, configUploadBytes)
	var themes []consoletheme.Info
	if h.pluginManager != nil {
		_, consoleOptions := h.pluginManager.EnabledThemeOptions()
		for _, option := range consoleOptions {
			source := "plugin"
			if option.Builtin {
				source = "builtin"
			}
			themes = append(themes, consoletheme.Info{Name: option.Name, DisplayName: option.Label, Source: source})
		}
	} else {
		var err error
		themes, err = consoletheme.List(h.Cfg.Storage.DataDir)
		if err != nil {
			return data, fmt.Errorf("load console themes: %w", err)
		}
	}
	data.ConsoleThemes = themes
	data.ConsoleThemeName = setting.ConsoleThemeName
	data.WebLanguage = setting.WebLanguage
	if data.WebLanguage != "en" {
		data.WebLanguage = "zh"
	}
	data.PageSize = resolvePageSize("", strconv.Itoa(setting.PageSize))
	if data.ConsoleThemeName == "" || !consoletheme.Exists(h.Cfg.Storage.DataDir, data.ConsoleThemeName) {
		data.ConsoleThemeName = consoletheme.BuiltinDefault
	}
	data.LongPollWaitSec = effective.LongPollWaitSec
	data.SyncDebounceSec = effective.SyncDebounceSec
	data.DefaultRecycleBinDays = effective.RecycleBinDays
	data.VaultStorageMB = effective.VaultStorageBytes / bytesPerMegabyte
	data.UploadSizeMB = effective.UploadSizeBytes / bytesPerMegabyte
	data.MaxLongPollWaitSec = limits.LongPollWaitSec
	data.MaxSyncDebounceSec = limits.SyncDebounceSec
	data.MaxRecycleBinDays = limits.RecycleBinDays
	data.MaxVaultStorageMB = limits.VaultStorageBytes / bytesPerMegabyte
	data.MaxUploadSizeMB = limits.UploadSizeBytes / bytesPerMegabyte
	return data, nil
}

func (h *Handler) saveAccountLanguage(c *gin.Context) {
	language := c.PostForm("web_language")
	if language != "zh" && language != "en" {
		c.Redirect(http.StatusSeeOther, "/dashboard/account?error="+url.QueryEscape(h.t(c, "err.invalid_language")))
		return
	}
	if err := h.DB.Model(&models.UserSetting{}).Where("user_id = ?", h.webUser(c).ID).
		Update("web_language", language).Error; err != nil {
		c.Redirect(http.StatusSeeOther, "/dashboard/account?error="+url.QueryEscape(h.t(c, "err.save_language_failed")))
		return
	}
	c.SetCookie(webLanguageCookie, language, 365*24*60*60, "/", "", requestIsHTTPS(c), false)
	c.Redirect(http.StatusSeeOther, "/dashboard/account?settings_saved=1#language")
}

func parseUserPreferences(form url.Values, limits settingspolicy.Limits) (settingspolicy.Preferences, error) {
	longPoll, err := parsePreferenceInteger(form, "long_poll_wait_sec")
	if err != nil {
		return settingspolicy.Preferences{}, err
	}
	debounce, err := parsePreferenceInteger(form, "sync_debounce_sec")
	if err != nil {
		return settingspolicy.Preferences{}, err
	}
	recycleDays, err := parsePreferenceInteger(form, "default_recycle_bin_days")
	if err != nil {
		return settingspolicy.Preferences{}, err
	}
	vaultMB, err := parsePreferenceInteger(form, "vault_storage_mb")
	if err != nil {
		return settingspolicy.Preferences{}, err
	}
	uploadMB, err := parsePreferenceInteger(form, "upload_size_mb")
	if err != nil {
		return settingspolicy.Preferences{}, err
	}
	preferences := settingspolicy.Preferences{
		LongPollWaitSec:   int(longPoll),
		SyncDebounceSec:   int(debounce),
		RecycleBinDays:    int(recycleDays),
		VaultStorageBytes: vaultMB * bytesPerMegabyte,
		UploadSizeBytes:   uploadMB * bytesPerMegabyte,
	}
	if err := settingspolicy.ValidatePreferences(preferences, limits); err != nil {
		return settingspolicy.Preferences{}, &userPreferencesInputError{Message: "用户设置超出允许范围"}
	}
	return preferences, nil
}

func parsePreferenceInteger(form url.Values, field string) (int64, error) {
	raw := strings.TrimSpace(form.Get(field))
	if raw == "" && field == "vault_storage_mb" {
		return 0, nil
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value < 0 || value > (int64(^uint64(0)>>1)/bytesPerMegabyte) {
		return 0, &userPreferencesInputError{Message: field + " 必须为有效非负整数"}
	}
	return value, nil
}

func (h *Handler) changePassword(c *gin.Context) {
	user := h.webUser(c)
	oldPassword := c.PostForm("old_password")
	newPassword := c.PostForm("new_password")
	confirmation := c.PostForm("new_password_confirm")
	if newPassword != confirmation {
		h.renderAccountError(c, user, http.StatusBadRequest, h.t(c, "err.new_password_mismatch"))
		return
	}
	if err := auth.ChangePassword(h.DB, user.ID, oldPassword, newPassword); err != nil {
		h.renderAccountError(c, user, http.StatusBadRequest, err.Error())
		return
	}
	if err := h.DB.Where("id = ?", user.ID).First(user).Error; err != nil {
		h.renderAccountError(c, user, http.StatusInternalServerError, h.t(c, "err.password_updated_session_failed"))
		return
	}
	h.setSessionCookie(c, user)
	c.Redirect(http.StatusSeeOther, "/dashboard/account?saved=1#password")
}

func (h *Handler) renderAccountError(c *gin.Context, user *models.User, status int, message string) {
	data, err := h.loadAccountData(user)
	if err != nil {
		data = accountData{Username: user.Username, Role: user.Role, CreatedAt: user.CreatedAt}
	}
	data.Error = message
	h.render(c, status, "account", h.t(c, "page.account"), "account", "account", data)
}
