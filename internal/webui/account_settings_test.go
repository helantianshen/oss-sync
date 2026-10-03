package webui

import (
	"bytes"
	"html/template"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/helantianshen/oss-sync/internal/models"
	"github.com/helantianshen/oss-sync/internal/settingspolicy"
)

func TestParseUserPreferences_whenValuesAreWithinCeilings_convertsMegabytesToBytes(t *testing.T) {
	t.Parallel()

	limits := settingspolicy.Limits{
		LongPollWaitSec: 20, SyncDebounceSec: 60, RecycleBinDays: 90,
		VaultStorageBytes: 10 << 30, UploadSizeBytes: 50 << 20,
	}
	form := url.Values{
		"long_poll_wait_sec":       {"15"},
		"sync_debounce_sec":        {"10"},
		"default_recycle_bin_days": {"45"},
		"vault_storage_mb":         {"5120"},
		"upload_size_mb":           {"25"},
	}

	preferences, err := parseUserPreferences(form, limits)

	if err != nil {
		t.Fatalf("parse user preferences: %v", err)
	}
	if preferences.VaultStorageBytes != 5120<<20 {
		t.Errorf("vault bytes = %d, want %d", preferences.VaultStorageBytes, int64(5120<<20))
	}
	if preferences.UploadSizeBytes != 25<<20 {
		t.Errorf("upload bytes = %d, want %d", preferences.UploadSizeBytes, int64(25<<20))
	}
}

func TestParseUserPreferences_whenCapacityIsEmpty_usesInheritedZero(t *testing.T) {
	t.Parallel()
	limits := settingspolicy.Limits{LongPollWaitSec: 20, SyncDebounceSec: 60, RecycleBinDays: 90, VaultStorageBytes: 10 << 30, UploadSizeBytes: 50 << 20}
	form := url.Values{"long_poll_wait_sec": {"15"}, "sync_debounce_sec": {"10"}, "default_recycle_bin_days": {"45"}, "vault_storage_mb": {""}, "upload_size_mb": {"25"}}
	preferences, err := parseUserPreferences(form, limits)
	if err != nil {
		t.Fatalf("parse empty inherited capacity: %v", err)
	}
	if preferences.VaultStorageBytes != 0 {
		t.Fatalf("vault bytes = %d, want 0", preferences.VaultStorageBytes)
	}
}

func TestParseUserPreferences_whenValueExceedsAdministratorCeiling_returnsError(t *testing.T) {
	t.Parallel()

	limits := settingspolicy.Limits{
		LongPollWaitSec: 20, SyncDebounceSec: 60, RecycleBinDays: 90,
		VaultStorageBytes: 10 << 30, UploadSizeBytes: 50 << 20,
	}
	form := url.Values{
		"long_poll_wait_sec":       {"21"},
		"sync_debounce_sec":        {"10"},
		"default_recycle_bin_days": {"45"},
		"vault_storage_mb":         {"5120"},
		"upload_size_mb":           {"25"},
	}

	_, err := parseUserPreferences(form, limits)

	if err == nil {
		t.Fatal("parseUserPreferences returned nil, want ceiling error")
	}
	if strings.Contains(err.Error(), "管理员") {
		t.Fatalf("user-facing preference error exposes administrator wording: %q", err)
	}
}

func TestAccountTemplate_whenRendered_exposesConstrainedPreferenceControls(t *testing.T) {
	t.Parallel()

	tpl, err := template.New("web").Funcs(templateFuncs()).ParseFS(webFS, "templates/account.html")
	if err != nil {
		t.Fatalf("parse account template: %v", err)
	}
	data := struct {
		Layout layoutData
		Data   map[string]any
	}{
		Layout: layoutData{CSRF: "csrf-token", Language: "zh"},
		Data: map[string]any{
			"Username": "member", "Role": "user", "CreatedAt": time.Now(),
			"LongPollWaitSec": 15, "SyncDebounceSec": 10, "DefaultRecycleBinDays": 45,
			"VaultStorageMB": int64(5120), "UploadSizeMB": int64(25),
			"MaxLongPollWaitSec": 20, "MaxSyncDebounceSec": 60, "MaxRecycleBinDays": 90,
			"MaxVaultStorageMB": int64(10240), "MaxUploadSizeMB": int64(50),
		},
	}

	var rendered bytes.Buffer
	if err := tpl.ExecuteTemplate(&rendered, "account", data); err != nil {
		t.Fatalf("render account template: %v", err)
	}

	page := rendered.String()
	for _, field := range []string{
		"long_poll_wait_sec",
		"sync_debounce_sec",
		"default_recycle_bin_days",
		"vault_storage_mb",
		"upload_size_mb",
	} {
		if !strings.Contains(page, `name="`+field+`"`) {
			t.Errorf("account template missing field %s", field)
		}
	}
	if !strings.Contains(page, "管理员上限：10240 MiB") || !strings.Contains(page, "管理员上限：50 MiB") {
		t.Error("account template does not show administrator limits")
	}
}

func TestAccountTemplate_whenRenderedWithEnglish_exposesEnglishCopy(t *testing.T) {
	t.Parallel()

	tpl, err := template.New("web").Funcs(templateFuncs()).ParseFS(webFS, "templates/account.html")
	if err != nil {
		t.Fatalf("parse account template: %v", err)
	}
	data := struct {
		Layout layoutData
		Data   map[string]any
	}{
		Layout: layoutData{CSRF: "csrf-token", Language: "en"},
		Data: map[string]any{
			"Username": "member", "Role": "user", "CreatedAt": time.Now(),
			"LongPollWaitSec": 15, "SyncDebounceSec": 10, "DefaultRecycleBinDays": 45,
			"VaultStorageMB": int64(5120), "UploadSizeMB": int64(25),
			"MaxLongPollWaitSec": 20, "MaxSyncDebounceSec": 60, "MaxRecycleBinDays": 90,
			"MaxVaultStorageMB": int64(10240), "MaxUploadSizeMB": int64(50),
		},
	}

	var rendered bytes.Buffer
	if err := tpl.ExecuteTemplate(&rendered, "account", data); err != nil {
		t.Fatalf("render account template: %v", err)
	}

	page := rendered.String()
	if !strings.Contains(page, "Sync and storage preferences") {
		t.Error("en render missing Sync and storage preferences")
	}
	if strings.Contains(page, "同步与存储偏好") {
		t.Error("en render contains zh text 同步与存储偏好")
	}
}

func TestSaveAccountSettings_whenCapacityBlank_savesWithInheritedDefault(t *testing.T) {
	db, cfg, _ := newWebUITestDB(t)
	h, err := New(db, cfg)
	if err != nil {
		t.Fatalf("new handler: %v", err)
	}
	user := createTestUserWithHash(t, db, "capacity-member", "user")
	session, csrf := issueWebSession(t, cfg, user)
	form := url.Values{
		"_csrf":                    {csrf},
		"long_poll_wait_sec":       {"15"},
		"sync_debounce_sec":        {"10"},
		"default_recycle_bin_days": {"45"},
		"vault_storage_mb":         {""},
		"upload_size_mb":           {"25"},
	}
	w := doWebRequest(t, h, http.MethodPost, "/dashboard/account/settings", form, session, csrf, false)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("save with blank capacity = %d, want 303; body %s", w.Code, w.Body.String())
	}
	if location := w.Header().Get("Location"); !strings.Contains(location, "settings_saved=1") {
		t.Fatalf("blank capacity save redirected to %q, want settings_saved=1", location)
	}
	var setting models.UserSetting
	if err := db.Where("user_id = ?", user.ID).First(&setting).Error; err != nil {
		t.Fatalf("load stored setting: %v", err)
	}
	if setting.VaultStorageBytes != 0 {
		t.Fatalf("stored vault bytes = %d, want 0 (inherited)", setting.VaultStorageBytes)
	}
}
