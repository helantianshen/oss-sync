package settingspolicy

import (
	"fmt"

	"github.com/oss/oss-server/internal/models"
)

const (
	defaultLongPollWaitSec = 30
	defaultSyncDebounceSec = 3
	defaultRecycleBinDays  = 30
	defaultUploadSizeBytes = int64(100 << 20)
	hardMaxDebounceSec     = 3600
	hardMaxRecycleBinDays  = 3650
	hardMaxHistoryDays     = 3650
)

type Limits struct {
	LongPollWaitSec      int
	SyncDebounceSec      int
	RecycleBinDays       int
	HistoryRetentionDays int
	VaultStorageBytes    int64
	UploadSizeBytes      int64
}

type Preferences struct {
	LongPollWaitSec   int
	SyncDebounceSec   int
	RecycleBinDays    int
	VaultStorageBytes int64
	UploadSizeBytes   int64
}

type Effective struct {
	LongPollWaitSec   int
	SyncDebounceSec   int
	RecycleBinDays    int
	VaultStorageBytes int64
	UploadSizeBytes   int64
}

type PreferenceError struct {
	Field   string
	Value   int64
	Minimum int64
	Maximum int64
}

func (e *PreferenceError) Error() string {
	return fmt.Sprintf("%s must be between %d and %d, got %d", e.Field, e.Minimum, e.Maximum, e.Value)
}

func LimitsFor(system models.SystemSetting, configUploadBytes int64) Limits {
	uploadLimit := configUploadBytes
	if uploadLimit <= 0 {
		uploadLimit = defaultUploadSizeBytes
	}
	if system.MaxUploadSizeBytes > 0 && system.MaxUploadSizeBytes < uploadLimit {
		uploadLimit = system.MaxUploadSizeBytes
	}
	return Limits{
		LongPollWaitSec:      bounded(system.MaxLongPollWaitSec, defaultLongPollWaitSec, 1, defaultLongPollWaitSec),
		SyncDebounceSec:      bounded(system.MaxSyncDebounceSec, 300, defaultSyncDebounceSec, hardMaxDebounceSec),
		RecycleBinDays:       bounded(system.MaxRecycleBinDays, hardMaxRecycleBinDays, 1, hardMaxRecycleBinDays),
		HistoryRetentionDays: bounded(system.HistoryRetentionDays, 0, 0, hardMaxHistoryDays),
		VaultStorageBytes:    maxInt64(system.MaxVaultStorageBytes, 0),
		UploadSizeBytes:      uploadLimit,
	}
}

func Resolve(system models.SystemSetting, user models.UserSetting, configUploadBytes int64) Effective {
	limits := LimitsFor(system, configUploadBytes)
	recycleDefault := bounded(system.DefaultRecycleBinDays, defaultRecycleBinDays, 1, limits.RecycleBinDays)
	return Effective{
		LongPollWaitSec: bounded(user.LongPollWaitSec, defaultLongPollWaitSec, 1, limits.LongPollWaitSec),
		SyncDebounceSec: bounded(user.SyncDebounceSec, defaultSyncDebounceSec, defaultSyncDebounceSec, limits.SyncDebounceSec),
		RecycleBinDays:  bounded(user.DefaultRecycleBinDays, recycleDefault, 1, limits.RecycleBinDays),
		VaultStorageBytes: inheritedLimit(
			user.VaultStorageBytes,
			limits.VaultStorageBytes,
		),
		UploadSizeBytes: inheritedLimit(user.UploadSizeBytes, limits.UploadSizeBytes),
	}
}

func ValidatePreferences(preferences Preferences, limits Limits) error {
	checks := []struct {
		field   string
		value   int64
		minimum int64
		maximum int64
		inherit bool
	}{
		{field: "long_poll_wait_sec", value: int64(preferences.LongPollWaitSec), minimum: 1, maximum: int64(limits.LongPollWaitSec)},
		{field: "sync_debounce_sec", value: int64(preferences.SyncDebounceSec), minimum: defaultSyncDebounceSec, maximum: int64(limits.SyncDebounceSec)},
		{field: "default_recycle_bin_days", value: int64(preferences.RecycleBinDays), minimum: 1, maximum: int64(limits.RecycleBinDays), inherit: true},
		{field: "vault_storage_bytes", value: preferences.VaultStorageBytes, minimum: 1, maximum: limits.VaultStorageBytes, inherit: true},
		{field: "upload_size_bytes", value: preferences.UploadSizeBytes, minimum: 1, maximum: limits.UploadSizeBytes, inherit: true},
	}
	for _, check := range checks {
		if check.inherit && check.value == 0 {
			continue
		}
		if check.value < check.minimum || (check.maximum > 0 && check.value > check.maximum) {
			return &PreferenceError{
				Field: check.field, Value: check.value, Minimum: check.minimum, Maximum: check.maximum,
			}
		}
	}
	return nil
}

func bounded(value, fallback, minimum, maximum int) int {
	if value <= 0 {
		value = fallback
	}
	if value < minimum {
		return minimum
	}
	if value > maximum {
		return maximum
	}
	return value
}

func inheritedLimit(preference, limit int64) int64 {
	if preference <= 0 {
		return limit
	}
	if limit > 0 && preference > limit {
		return limit
	}
	return preference
}

func maxInt64(value, minimum int64) int64 {
	if value < minimum {
		return minimum
	}
	return value
}
