package webui

import (
	"bytes"
	"html/template"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/oss/oss-server/internal/settingspolicy"
)

func TestParseAdminSystemInput_whenValuesAreWithinHardLimits_convertsMegabytesToBytes(t *testing.T) {
	t.Parallel()

	// Given
	form := url.Values{
		"sync_mode":                {"long_poll"},
		"default_recycle_bin_days": {"30"},
		"max_long_poll_wait_sec":   {"20"},
		"max_sync_debounce_sec":    {"60"},
		"max_recycle_bin_days":     {"90"},
		"history_retention_days":   {"30"},
		"max_vault_storage_mb":     {"10240"},
		"max_upload_size_mb":       {"50"},
		"custom_fragments_enabled": {"on"},
	}

	// When
	input, err := parseAdminSystemInput(form, 100<<20)

	// Then
	if err != nil {
		t.Fatalf("parse admin system input: %v", err)
	}
	if input.MaxVaultStorageBytes != 10240<<20 {
		t.Errorf("vault storage bytes = %d, want %d", input.MaxVaultStorageBytes, int64(10240<<20))
	}
	if input.MaxUploadSizeBytes != 50<<20 {
		t.Errorf("upload bytes = %d, want %d", input.MaxUploadSizeBytes, int64(50<<20))
	}
	if input.SyncMode != settingspolicy.SyncModeLongPoll {
		t.Errorf("sync mode = %q, want %q", input.SyncMode, settingspolicy.SyncModeLongPoll)
	}
	if !input.CustomFragmentsEnabled {
		t.Fatal("custom fragments enabled should be true when checkbox is checked")
	}
	if input.HistoryRetentionDays != 30 {
		t.Errorf("history retention days = %d, want 30", input.HistoryRetentionDays)
	}
}

func TestParseAdminSystemInput_whenDefaultRecycleExceedsCeiling_returnsError(t *testing.T) {
	t.Parallel()

	// Given
	form := url.Values{
		"sync_mode":                {"user_choice"},
		"default_recycle_bin_days": {"91"},
		"max_long_poll_wait_sec":   {"20"},
		"max_sync_debounce_sec":    {"60"},
		"max_recycle_bin_days":     {"90"},
		"max_vault_storage_mb":     {"0"},
		"max_upload_size_mb":       {"50"},
	}

	// When
	_, err := parseAdminSystemInput(form, 100<<20)

	// Then
	if err == nil {
		t.Fatal("parseAdminSystemInput returned nil, want recycle ceiling error")
	}
}

func TestAdminSystemTemplate_whenRendered_exposesAdministratorCeilingControls(t *testing.T) {
	t.Parallel()

	// Given
	tpl, err := template.New("web").Funcs(template.FuncMap{
		"formatBytes": formatBytes,
		"timeFmt":     func(value time.Time) string { return value.Format("2006-01-02 15:04") },
	}).ParseFS(webFS, "templates/admin_system.html")
	if err != nil {
		t.Fatalf("parse admin system template: %v", err)
	}
	data := struct {
		Layout layoutData
		Data   map[string]any
	}{
		Layout: layoutData{CSRF: "csrf-token"},
		Data: map[string]any{
			"RegistrationEnabled":    true,
			"SyncMode":               "long_poll",
			"CustomFragmentsEnabled": true,
			"DefaultRecycleBinDays":  30,
			"MaxLongPollWaitSec":     20,
			"MaxSyncDebounceSec":     60,
			"MaxRecycleBinDays":      90,
			"MaxVaultStorageMB":      int64(10240),
			"MaxUploadSizeMB":        int64(50),
			"ConfigMaxUploadSizeMB":  int64(100),
		},
	}

	// When
	var rendered bytes.Buffer
	if err := tpl.ExecuteTemplate(&rendered, "admin-system", data); err != nil {
		t.Fatalf("render admin system template: %v", err)
	}

	// Then
	page := rendered.String()
	for _, field := range []string{
		"custom_fragments_enabled",
		"sync_mode",
		"max_long_poll_wait_sec",
		"max_sync_debounce_sec",
		"max_recycle_bin_days",
		"max_vault_storage_mb",
		"max_upload_size_mb",
	} {
		if !strings.Contains(page, `name="`+field+`"`) {
			t.Errorf("admin system template missing field %s", field)
		}
	}
	if strings.Contains(page, `name="public_home_vault_id"`) {
		t.Error("admin system template still exposes the obsolete single-Vault public homepage selector")
	}
	if !strings.Contains(page, `action="/dashboard/admin/system/database"`) {
		t.Error("admin system template missing database configuration form")
	}
}
