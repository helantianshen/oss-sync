package settingspolicy

import (
	"errors"
	"fmt"

	"gorm.io/gorm"

	"github.com/oss/oss-server/internal/models"
)

func EffectiveForUser(db *gorm.DB, userID uint, configUploadBytes int64) (Effective, error) {
	var system models.SystemSetting
	if err := db.First(&system, 1).Error; err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return Effective{}, fmt.Errorf("load system settings: %w", err)
	}

	var user models.UserSetting
	if err := db.Where("user_id = ?", userID).First(&user).Error; err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return Effective{}, fmt.Errorf("load user settings: %w", err)
	}

	return Resolve(system, user, configUploadBytes), nil
}

func EffectiveForVault(db *gorm.DB, vaultID string, configUploadBytes int64) (Effective, error) {
	var vault models.Vault
	if err := db.Select("owner_id").Where("id = ?", vaultID).First(&vault).Error; err != nil {
		return Effective{}, fmt.Errorf("load vault owner: %w", err)
	}
	return EffectiveForUser(db, vault.OwnerID, configUploadBytes)
}

func CustomFragmentsEnabled(db *gorm.DB) bool {
	var system models.SystemSetting
	if err := db.First(&system, 1).Error; err != nil {
		return false
	}
	return system.CustomFragmentsEnabled
}

// HistoryRetentionDaysForVault 返回仓库所属用户的文件历史保留天数。
// 0 表示不清理。
func HistoryRetentionDaysForVault(db *gorm.DB, vaultID string) (int, error) {
	var vault models.Vault
	if err := db.Select("owner_id").Where("id = ?", vaultID).First(&vault).Error; err != nil {
		return 0, fmt.Errorf("load vault owner: %w", err)
	}
	var system models.SystemSetting
	if err := db.First(&system, 1).Error; err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return 0, fmt.Errorf("load system settings: %w", err)
	}
	limits := LimitsFor(system, 0)
	return limits.HistoryRetentionDays, nil
}
