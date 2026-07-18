package database

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/glebarez/sqlite"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/oss/oss-server/internal/config"
	"github.com/oss/oss-server/internal/models"
)

// Init 根据配置初始化 GORM 连接。
// 决策 0：默认 SQLite，必须用 GORM 编写，保证仅改 DSN 即可切 PostgreSQL。
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

// AutoMigrate 注册所有模型并执行迁移。
// 5 个模型：User / UserSetting / File / Share / Collaboration（对应 PRD 一、1-5）。
func AutoMigrate(db *gorm.DB) error {
	if err := db.AutoMigrate(
		&models.User{},
		&models.UserSetting{},
		&models.File{},
		&models.Share{},
		&models.Collaboration{},
	); err != nil {
		return fmt.Errorf("AutoMigrate 失败: %w", err)
	}
	return nil
}
