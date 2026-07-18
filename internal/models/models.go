package models

import (
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"time"

	"gorm.io/gorm"
)

// JSONMap 用于把 map[string]any 存为 JSON 字段（如 ThemeConfig）。
// GORM 默认支持 datatypes.JSON，此处手写以减少依赖。
type JSONMap map[string]any

func (j JSONMap) Value() (driver.Value, error) {
	if j == nil {
		return nil, nil
	}
	return json.Marshal(j)
}

func (j *JSONMap) Scan(value any) error {
	if value == nil {
		*j = nil
		return nil
	}
	var b []byte
	switch v := value.(type) {
	case []byte:
		b = v
	case string:
		b = []byte(v)
	default:
		return errors.New("JSONMap.Scan: 不支持的类型")
	}
	if len(b) == 0 {
		return nil
	}
	return json.Unmarshal(b, j)
}

func (j JSONMap) GormDataType() string {
	return "json"
}

// User 用户表 (PRD 一、1)
type User struct {
	ID           uint           `gorm:"primaryKey"`
	Username     string         `gorm:"uniqueIndex;size:64;not null"`
	PasswordHash string         `gorm:"size:128;not null"`
	Role         string         `gorm:"size:16;not null;default:'user'"` // admin / user
	StorageQuota int64          `gorm:"not null;default:0"`               // 字节数，0 表示不限
	CreatedAt    time.Time
	UpdatedAt    time.Time
	DeletedAt    gorm.DeletedAt `gorm:"index"`
}

// UserSetting 用户配置表 (PRD 一、2)
type UserSetting struct {
	UserID           uint   `gorm:"uniqueIndex;not null"`
	SyncInterval     int    `gorm:"not null;default:300"`        // 秒，默认 5 分钟
	RecycleBinDays   *int   `gorm:"default:30"`                  // 0 = 直接硬删除；nil → 30
	ThemeName        string `gorm:"size:64;not null;default:'default'"`
	ThemeConfig      JSONMap `gorm:"type:json"`
	CustomHeader     string `gorm:"type:text"`
	CustomFooter     string `gorm:"type:text"`
	KeepDirectoryTree bool  `gorm:"not null;default:true"`
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// File 文件元数据表 (PRD 一、3)
type File struct {
	ID        uint           `gorm:"primaryKey"`
	UserID    uint           `gorm:"index;not null"`
	Path      string         `gorm:"index;size:512;not null"` // 库内相对路径
	Type      string         `gorm:"size:16;not null"`        // markdown / attachment / config
	Hash      string         `gorm:"size:64"`                 // SHA256
	MTime     int64          `gorm:"not null"`                // 客户端最后修改时间戳（LWW 核心凭据）
	IsDeleted bool           `gorm:"not null;default:false"`  // 软删除标记（移入回收站）
	DeletedAt sql.NullTime   `gorm:"index"`                    // 回收站到期清理凭据
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Share 分享记录表 (PRD 一、4)
// 主键为 ShareID (string，uuid 或 6 位短链)，非自增。
type Share struct {
	ShareID    string    `gorm:"primaryKey;size:32"`
	UserID     uint      `gorm:"index;not null"`
	TargetPath string    `gorm:"size:512;not null"`
	IsFolder   bool      `gorm:"not null;default:false"`
	AllowCopy  bool      `gorm:"not null;default:false"`
	Views      int       `gorm:"not null;default:0"`
	CreatedAt  time.Time `gorm:"index"`
	UpdatedAt  time.Time
}

// Collaboration 协作表 (PRD 一、5)
type Collaboration struct {
	ID            uint      `gorm:"primaryKey"`
	FileID        uint      `gorm:"index;not null"`
	OwnerID       uint      `gorm:"index;not null"`
	CollaboratorID uint     `gorm:"index;not null"`
	Status        string    `gorm:"size:16;not null;default:'pending'"` // pending / accepted
	CreatedAt     time.Time
	UpdatedAt     time.Time
}
