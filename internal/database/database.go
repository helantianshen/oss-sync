package database

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/glebarez/sqlite"
	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/oss/oss-server/internal/config"
	"github.com/oss/oss-server/internal/models"
)

// Init 根据配置初始化 GORM 连接。
// SQLite 的 DSN 是文件路径；上层会确保父目录存在。
func Init(cfg *config.Config) (*gorm.DB, error) {
	switch cfg.Database.Driver {
	case "sqlite":
		return initSQLite(cfg)
	case "postgres":
		return initPostgres(cfg)
	default:
		return nil, fmt.Errorf("不支持的数据库驱动: %s", cfg.Database.Driver)
	}
}

func initSQLite(cfg *config.Config) (*gorm.DB, error) {
	dsn := cfg.Database.DSN
	if dir := filepath.Dir(dsn); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("创建 SQLite 目录 %s 失败: %w", dir, err)
		}
	}
	gormLogLevel := logger.Warn
	if config.Env() == "dev" {
		gormLogLevel = logger.Info
	}
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{
		Logger: logger.Default.LogMode(gormLogLevel),
	})
	if err != nil {
		return nil, fmt.Errorf("打开 SQLite 失败: %w", err)
	}
	return db, nil
}

func initPostgres(cfg *config.Config) (*gorm.DB, error) {
	gormLogLevel := logger.Warn
	if config.Env() == "dev" {
		gormLogLevel = logger.Info
	}
	db, err := gorm.Open(postgres.Open(cfg.Database.DSN), &gorm.Config{
		Logger: logger.Default.LogMode(gormLogLevel),
	})
	if err != nil {
		return nil, fmt.Errorf("连接 PostgreSQL 失败: %w", err)
	}
	return db, nil
}

// AutoMigrate 注册模型，并补齐旧数据的默认 Vault 和 revision。
func AutoMigrate(db *gorm.DB) error {
	if err := db.AutoMigrate(
		&models.User{},
		&models.Vault{},
		&models.VaultSetting{},
		&models.VaultSyncState{},
		&models.ClientDevice{},
		&models.DeviceVault{},
		&models.StorageIssue{},
		&models.UserSetting{},
		&models.File{},
		&models.Share{},
		&models.Collaboration{},
	); err != nil {
		return fmt.Errorf("AutoMigrate 失败: %w", err)
	}
	if err := ensureDefaultVaults(db); err != nil {
		return err
	}
	return backfillVaultRevisions(db)
}

func ensureDefaultVaults(db *gorm.DB) error {
	var users []models.User
	if err := db.Find(&users).Error; err != nil {
		return fmt.Errorf("查询用户以初始化默认 Vault 失败: %w", err)
	}
	for _, user := range users {
		if err := db.Transaction(func(tx *gorm.DB) error {
			var vault models.Vault
			err := tx.Where("owner_id = ? AND is_default = ?", user.ID, true).First(&vault).Error
			if errors.Is(err, gorm.ErrRecordNotFound) {
				err = tx.Where("owner_id = ?", user.ID).Order("created_at asc").First(&vault).Error
			}
			if errors.Is(err, gorm.ErrRecordNotFound) {
				vault = models.Vault{
					ID:        uuid.NewString(),
					OwnerID:   user.ID,
					Name:      "Default",
					IsDefault: true,
				}
				if err := tx.Create(&vault).Error; err != nil {
					return err
				}
			} else if err != nil {
				return err
			}
			if !vault.IsDefault {
				if err := tx.Model(&models.Vault{}).Where("id = ?", vault.ID).
					Update("is_default", true).Error; err != nil {
					return err
				}
			}
			if err := tx.FirstOrCreate(
				&models.VaultSetting{},
				models.VaultSetting{VaultID: vault.ID},
			).Error; err != nil {
				return err
			}
			if err := tx.FirstOrCreate(
				&models.VaultSyncState{},
				models.VaultSyncState{VaultID: vault.ID},
			).Error; err != nil {
				return err
			}
			if err := tx.Model(&models.File{}).
				Where("user_id = ? AND (vault_id = '' OR vault_id IS NULL)", user.ID).
				Update("vault_id", vault.ID).Error; err != nil {
				return err
			}
			if err := tx.Model(&models.Share{}).
				Where("user_id = ? AND (vault_id = '' OR vault_id IS NULL)", user.ID).
				Update("vault_id", vault.ID).Error; err != nil {
				return err
			}
			return tx.Model(&models.Collaboration{}).
				Where("owner_id = ? AND (vault_id = '' OR vault_id IS NULL)", user.ID).
				Update("vault_id", vault.ID).Error
		}); err != nil {
			return fmt.Errorf("为用户 %d 创建默认 Vault 失败: %w", user.ID, err)
		}
	}
	return nil
}

func backfillVaultRevisions(db *gorm.DB) error {
	var vaults []models.Vault
	if err := db.Unscoped().Find(&vaults).Error; err != nil {
		return fmt.Errorf("查询 Vault 以回填 revision 失败: %w", err)
	}
	for _, vault := range vaults {
		if err := db.Transaction(func(tx *gorm.DB) error {
			var state models.VaultSyncState
			if err := tx.FirstOrCreate(
				&state,
				models.VaultSyncState{VaultID: vault.ID},
			).Error; err != nil {
				return err
			}

			var maxRevision int64
			if err := tx.Model(&models.File{}).
				Where("vault_id = ?", vault.ID).
				Select("COALESCE(MAX(revision), 0)").
				Scan(&maxRevision).Error; err != nil {
				return err
			}
			head := state.HeadRevision
			if maxRevision > head {
				head = maxRevision
			}

			var legacyFiles []models.File
			if err := tx.Where("vault_id = ? AND revision = 0", vault.ID).
				Order("id asc").
				Find(&legacyFiles).Error; err != nil {
				return err
			}
			for _, file := range legacyFiles {
				head++
				if err := tx.Model(&models.File{}).Where("id = ?", file.ID).
					Update("revision", head).Error; err != nil {
					return err
				}
			}

			var storageUsed int64
			if err := tx.Model(&models.File{}).
				Where("vault_id = ? AND is_deleted = ?", vault.ID, false).
				Select("COALESCE(SUM(size), 0)").
				Scan(&storageUsed).Error; err != nil {
				return err
			}
			if err := tx.Model(&models.Vault{}).Unscoped().Where("id = ?", vault.ID).
				Update("storage_used", storageUsed).Error; err != nil {
				return err
			}
			return tx.Model(&models.VaultSyncState{}).Where("vault_id = ?", vault.ID).
				Update("head_revision", head).Error
		}); err != nil {
			return fmt.Errorf("回填 Vault %s revision 失败: %w", vault.ID, err)
		}
	}
	return nil
}
