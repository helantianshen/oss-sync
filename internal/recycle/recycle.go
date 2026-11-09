// Package recycle 将删除内容移入 Vault 回收站并保留墓碑
package recycle

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gorm.io/gorm"

	"github.com/helantianshen/oss-sync/internal/models"
	"github.com/helantianshen/oss-sync/internal/settingspolicy"
)

// ErrRetentionExpired 表示文件已过回收站保留期
var ErrRetentionExpired = errors.New("recycle retention has expired")

// RetentionDays 返回策略解析后的回收站保留天数，仓库设置可进一步缩短保留期
func RetentionDays(db *gorm.DB, vaultID string) (int, error) {
	var setting models.VaultSetting
	err := db.Where("vault_id = ?", vaultID).First(&setting).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return 0, err
	}
	effective, err := settingspolicy.EffectiveForVault(db, vaultID, 0)
	if err != nil {
		return 0, err
	}
	days := effective.RecycleBinDays
	if setting.RecycleBinDays > 0 && setting.RecycleBinDays < days {
		days = setting.RecycleBinDays
	}
	return days, nil
}

// ExpiresAt 返回回收站文件的保留期到期时间
func ExpiresAt(file models.File, retentionDays int) time.Time {
	if !file.DeletedAt.Valid {
		return time.Time{}
	}
	return file.DeletedAt.Time.Add(time.Duration(retentionDays) * 24 * time.Hour)
}

// CanRestore 判断文件在指定时间是否仍可恢复
func CanRestore(file models.File, retentionDays int, now time.Time) bool {
	expiresAt := ExpiresAt(file, retentionDays)
	return !expiresAt.IsZero() && now.Before(expiresAt)
}

// CheckRestorable 校验回收站文件的恢复资格
func CheckRestorable(db *gorm.DB, file models.File, now time.Time) error {
	retentionDays, err := RetentionDays(db, file.VaultID)
	if err != nil {
		return err
	}
	if !CanRestore(file, retentionDays, now) {
		return ErrRetentionExpired
	}
	return nil
}

// Key 返回某 File 在回收站中的存储键
func Key(vaultID string, fileID uint) string {
	return filepath.ToSlash(filepath.Join("vaults", vaultID, "recycle", strconv.FormatUint(uint64(fileID), 10)))
}

func IsStorageKey(vaultID, key string) bool {
	return vaultID != "" && strings.HasPrefix(filepath.ToSlash(key), "vaults/"+vaultID+"/recycle/")
}

// DiskPath 返回回收站正文的绝对路径
func DiskPath(dataDir string, file models.File) string {
	if file.StorageKey == "" {
		return ""
	}
	return filepath.Join(dataDir, filepath.FromSlash(file.StorageKey))
}

// MoveIn 将 contentPath 移入回收站，返回回收站存储键
func MoveIn(dataDir, vaultID string, fileID uint, contentPath string) (string, error) {
	key := Key(vaultID, fileID)
	dest := filepath.Join(dataDir, filepath.FromSlash(key))
	if err := os.MkdirAll(filepath.Dir(dest), 0o750); err != nil {
		return "", err
	}
	if err := os.Rename(contentPath, dest); err != nil {
		if os.IsNotExist(err) {
			return key, nil
		}
		return "", err
	}
	return key, nil
}

// MoveOut 将回收站正文移回 contentPath
func MoveOut(dataDir string, file models.File, contentPath string) error {
	src := DiskPath(dataDir, file)
	if src == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(contentPath), 0o750); err != nil {
		return err
	}
	if err := os.Rename(src, contentPath); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	return nil
}

// Remove 删除回收站正文，忽略不存在的情况
func Remove(dataDir string, file models.File) error {
	abs := DiskPath(dataDir, file)
	if abs == "" {
		return nil
	}
	err := os.Remove(abs)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// CleanupVault 删除某仓库的全部回收站正文
func CleanupVault(dataDir, vaultID string) error {
	return os.RemoveAll(filepath.Join(dataDir, "vaults", vaultID, "recycle"))
}
