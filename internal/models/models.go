package models

import (
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"time"

	"gorm.io/gorm"
)

// JSONMap 用于把 map[string]any 存为 JSON 字段。
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

// User 保存账户和存储配额。
type User struct {
	ID           uint   `gorm:"primaryKey"`
	Username     string `gorm:"uniqueIndex;size:64;not null"`
	PasswordHash string `gorm:"size:128;not null"`
	Role         string `gorm:"size:16;not null;default:'user'"` // admin / user
	StorageQuota int64  `gorm:"not null;default:0"`              // 字节数，0 表示不限
	CreatedAt    time.Time
	UpdatedAt    time.Time
	DeletedAt    gorm.DeletedAt `gorm:"index"`
}

// Vault 是一个独立的 Obsidian 笔记仓库。同步 revision、文件路径和配置均按 Vault 隔离。
type Vault struct {
	ID           string `gorm:"primaryKey;size:36"`
	OwnerID      uint   `gorm:"index;not null"`
	Name         string `gorm:"size:128;not null"`
	Description  string `gorm:"size:512"`
	IsDefault    bool   `gorm:"not null;default:false"`
	StorageQuota int64  `gorm:"not null;default:0"`
	StorageUsed  int64  `gorm:"not null;default:0"`
	CreatedAt    time.Time
	UpdatedAt    time.Time
	ArchivedAt   gorm.DeletedAt `gorm:"index"`
}

// VaultSetting 保存仓库级配置。设备侧同步间隔等设置仍保留在插件本地。
type VaultSetting struct {
	VaultID           string  `gorm:"primaryKey;size:36"`
	ThemeName         string  `gorm:"size:64;not null;default:'default'"`
	ThemeConfig       JSONMap `gorm:"type:json"`
	CustomHeader      string  `gorm:"type:text"`
	CustomFooter      string  `gorm:"type:text"`
	KeepDirectoryTree bool    `gorm:"not null;default:true"`
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// VaultSyncState 保存某个 Vault 当前的服务端单调递增 revision。
type VaultSyncState struct {
	VaultID           string `gorm:"primaryKey;size:36"`
	HeadRevision      int64  `gorm:"not null;default:0"`
	CompactedRevision int64  `gorm:"not null;default:0"`
	UpdatedAt         time.Time
}

// ClientDevice 是用户的一台客户端设备。
type ClientDevice struct {
	ID         uint   `gorm:"primaryKey"`
	UserID     uint   `gorm:"index;not null;uniqueIndex:idx_user_client"`
	ClientID   string `gorm:"size:64;not null;uniqueIndex:idx_user_client"`
	Name       string `gorm:"size:128"`
	LastSeenAt time.Time
	RevokedAt  sql.NullTime `gorm:"index"`
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// DeviceVault 记录设备对 Vault 的同步进度，便于诊断和后续墓碑回收。
type DeviceVault struct {
	ID         uint   `gorm:"primaryKey"`
	UserID     uint   `gorm:"index;not null;uniqueIndex:idx_device_vault"`
	ClientID   string `gorm:"size:64;not null;uniqueIndex:idx_device_vault"`
	VaultID    string `gorm:"size:36;not null;uniqueIndex:idx_device_vault"`
	LastCursor int64  `gorm:"not null;default:0"`
	LastSyncAt time.Time
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// StorageIssue 记录无法自动修复的存储异常，修复后仍保留记录。
type StorageIssue struct {
	ID          uint   `gorm:"primaryKey"`
	VaultID     string `gorm:"index;size:36;not null"`
	FileID      uint   `gorm:"index"`
	StorageKey  string `gorm:"index;size:1024;not null"`
	Kind        string `gorm:"index;size:32;not null"`
	Detail      string `gorm:"type:text"`
	FirstSeenAt time.Time
	LastSeenAt  time.Time
	ResolvedAt  sql.NullTime `gorm:"index"`
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// UserSetting 保存兼容旧版本的用户级配置。
type UserSetting struct {
	UserID            uint    `gorm:"uniqueIndex;not null"`
	SyncInterval      int     `gorm:"not null;default:300"` // 秒，默认 5 分钟
	ThemeName         string  `gorm:"size:64;not null;default:'default'"`
	ThemeConfig       JSONMap `gorm:"type:json"`
	CustomHeader      string  `gorm:"type:text"`
	CustomFooter      string  `gorm:"type:text"`
	KeepDirectoryTree bool    `gorm:"not null;default:true"`
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// File 保存文件元数据和同步墓碑。
type File struct {
	ID                 uint         `gorm:"primaryKey"`
	UserID             uint         `gorm:"index;not null;uniqueIndex:idx_user_vault_path"`
	VaultID            string       `gorm:"index;size:36;uniqueIndex:idx_user_vault_path"`
	Path               string       `gorm:"index;size:512;not null;uniqueIndex:idx_user_vault_path"` // Vault 内相对路径
	Type               string       `gorm:"size:16;not null"`                                        // markdown / attachment / config
	Hash               string       `gorm:"size:64"`                                                 // SHA256
	Size               int64        `gorm:"not null;default:0"`
	MTime              int64        `gorm:"not null"` // 客户端最后修改时间戳
	Revision           int64        `gorm:"index;not null;default:0"`
	IsDeleted          bool         `gorm:"not null;default:false"` // 同步墓碑；正文已从存储移除
	DeletedAt          sql.NullTime `gorm:"index"`                  // 删除发生时间
	StorageKey         string       `gorm:"size:1024"`
	LastWriterClientID string       `gorm:"size:64"`
	LastOperationID    string       `gorm:"size:64"`
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

// Share 保存文章或文件夹的公开分享信息。
type Share struct {
	ShareID    string    `gorm:"primaryKey;size:32"`
	UserID     uint      `gorm:"index;not null"`
	VaultID    string    `gorm:"index;size:36"`
	TargetPath string    `gorm:"size:512;not null"`
	IsFolder   bool      `gorm:"not null;default:false"`
	AllowCopy  bool      `gorm:"not null;default:false"`
	Views      int       `gorm:"not null;default:0"`
	CreatedAt  time.Time `gorm:"index"`
	UpdatedAt  time.Time
}

// Collaboration 保存文件协作关系。
type Collaboration struct {
	ID             uint   `gorm:"primaryKey"`
	VaultID        string `gorm:"index;size:36"`
	FileID         uint   `gorm:"index;not null"`
	OwnerID        uint   `gorm:"index;not null"`
	CollaboratorID uint   `gorm:"index;not null"`
	Status         string `gorm:"size:16;not null;default:'pending'"` // pending / accepted
	CreatedAt      time.Time
	UpdatedAt      time.Time
}
