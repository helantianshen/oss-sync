// Package database 打开配置的 SQL 后端并执行迁移
package database

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/logger"

	"github.com/helantianshen/oss-sync/internal/config"
	"github.com/helantianshen/oss-sync/internal/models"
)

// Init 打开配置的 SQLite 或 PostgreSQL 连接，SQLite 父目录由初始化器创建
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
	dsn, err := sqliteConnectionString(cfg.Database.DSN)
	if err != nil {
		return nil, err
	}
	filePath, _, _ := strings.Cut(cfg.Database.DSN, "?")
	if dir := filepath.Dir(filePath); !strings.HasPrefix(filePath, "file:") && dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("创建 SQLite 目录 %s 失败: %w", dir, err)
		}
	}
	gormLogLevel := logger.Warn
	if config.Env() == "dev" {
		gormLogLevel = logger.Info
	}
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{
		Logger:         logger.Default.LogMode(gormLogLevel),
		TranslateError: true,
	})
	if err != nil {
		return nil, fmt.Errorf("打开 SQLite 失败: %w", err)
	}
	return db, nil
}

// SQLite 的延迟事务先读后写时可能立即争锁失败，busy_timeout 不能消除这类升级死锁。
// 默认在事务开始时取得写意向锁；驱动已有的锁等待由此可以正常生效。
func sqliteConnectionString(dsn string) (string, error) {
	_, query, hasQuery := strings.Cut(dsn, "?")
	options, err := url.ParseQuery(query)
	if err != nil {
		return "", fmt.Errorf("SQLite 连接参数无效: %w", err)
	}
	if options.Has("_txlock") {
		return dsn, nil
	}
	separator := "?"
	if hasQuery {
		separator = "&"
	}
	return dsn + separator + "_txlock=immediate", nil
}

func initPostgres(cfg *config.Config) (*gorm.DB, error) {
	gormLogLevel := logger.Warn
	if config.Env() == "dev" {
		gormLogLevel = logger.Info
	}
	db, err := gorm.Open(postgres.Open(cfg.Database.DSN), &gorm.Config{
		Logger:         logger.Default.LogMode(gormLogLevel),
		TranslateError: true,
	})
	if err != nil {
		return nil, fmt.Errorf("连接 PostgreSQL 失败: %w", err)
	}
	return db, nil
}

// AutoMigrate 注册模型并为旧数据执行兼容回填
func AutoMigrate(db *gorm.DB) error {
	if err := db.AutoMigrate(
		&models.User{},
		&models.SystemSetting{},
		&models.Vault{},
		&models.VaultMember{},
		&models.VaultBackup{},
		&models.VaultSetting{},
		&models.VaultSyncState{},
		&models.ClientDevice{},
		&models.DeviceVault{},
		&models.DeviceVaultAccess{},
		&models.StorageIssue{},
		&models.UserSetting{},
		&models.File{},
		&models.Share{},
		&models.Collaboration{},
		&models.FileHistory{},
		&models.ServerPlugin{},
		&models.ServerPluginAssociation{},
		&models.ServerPluginMigration{},
		&models.VaultPluginSetting{},
	); err != nil {
		return fmt.Errorf("AutoMigrate 失败: %w", err)
	}
	if err := backfillLegacyVaults(db); err != nil {
		return err
	}
	if err := backfillVaultRevisions(db); err != nil {
		return err
	}
	if err := backfillDeviceStates(db); err != nil {
		return err
	}
	if err := backfillVaultSettings(db); err != nil {
		return err
	}
	return backfillServerPluginMetadata(db)
}

func backfillServerPluginMetadata(db *gorm.DB) error {
	if err := db.Model(&models.ServerPlugin{}).
		Where("runtime = '' OR runtime IS NULL").
		Update("runtime", "wasm").Error; err != nil {
		return fmt.Errorf("回填插件运行时失败: %w", err)
	}
	if err := db.Model(&models.ServerPlugin{}).
		Where("payload_hash = '' OR payload_hash IS NULL").
		Updates(map[string]any{
			"payload_hash": gorm.Expr("wasm_hash"),
			"payload_size": gorm.Expr("wasm_size"),
		}).Error; err != nil {
		return fmt.Errorf("回填插件载荷元数据失败: %w", err)
	}
	return nil
}

// backfillLegacyVaults 只为已有无 Vault 文件的账号创建默认 Vault
func backfillLegacyVaults(db *gorm.DB) error {
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
				needsVault, err := hasVaultlessLegacyContent(tx, user.ID)
				if err != nil {
					return err
				}
				if !needsVault {
					return nil
				}
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

func hasVaultlessLegacyContent(tx *gorm.DB, userID uint) (bool, error) {
	queries := []struct {
		model any
		where string
	}{
		{model: &models.File{}, where: "user_id = ? AND (vault_id = '' OR vault_id IS NULL)"},
		{model: &models.Share{}, where: "user_id = ? AND (vault_id = '' OR vault_id IS NULL)"},
		{model: &models.Collaboration{}, where: "owner_id = ? AND (vault_id = '' OR vault_id IS NULL)"},
	}
	for _, query := range queries {
		var count int64
		if err := tx.Model(query.model).Where(query.where, userID).Count(&count).Error; err != nil {
			return false, err
		}
		if count > 0 {
			return true, nil
		}
	}
	return false, nil
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

// backfillDeviceStates 补齐设备状态与已有同步绑定的 Vault 授权
func backfillDeviceStates(db *gorm.DB) error {
	if err := db.Model(&models.ClientDevice{}).
		Where("status = '' AND revoked_at IS NULL").
		Update("status", "approved").Error; err != nil {
		return fmt.Errorf("回填已批准设备失败: %w", err)
	}
	if err := db.Model(&models.ClientDevice{}).
		Where("status = '' AND revoked_at IS NOT NULL").
		Update("status", "revoked").Error; err != nil {
		return fmt.Errorf("回填已吊销设备失败: %w", err)
	}

	var bindings []models.DeviceVault
	if err := db.Find(&bindings).Error; err != nil {
		return fmt.Errorf("查询旧设备同步绑定失败: %w", err)
	}
	for _, binding := range bindings {
		grantedAt := binding.CreatedAt
		if grantedAt.IsZero() {
			grantedAt = time.Now()
		}
		if err := db.Clauses(clause.OnConflict{DoNothing: true}).
			Create(&models.DeviceVaultAccess{
				UserID:          binding.UserID,
				ClientID:        binding.ClientID,
				VaultID:         binding.VaultID,
				GrantedByUserID: binding.UserID,
				GrantedAt:       grantedAt,
			}).Error; err != nil {
			return fmt.Errorf("回填设备仓库授权失败: %w", err)
		}
	}
	return nil
}

// backfillVaultSettings 为已有仓库补齐新默认值
func backfillVaultSettings(db *gorm.DB) error {
	if err := db.Model(&models.VaultSetting{}).
		Where("recycle_bin_days = 0 OR recycle_bin_days IS NULL").
		Update("recycle_bin_days", 30).Error; err != nil {
		return fmt.Errorf("回填仓库回收站保留期失败: %w", err)
	}
	return nil
}
