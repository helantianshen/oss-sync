// Package models 提供后端各模块共用的 GORM 持久化模型
package models

import (
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"time"

	"gorm.io/gorm"
)

// JSONMap 在数据库 JSON 列中存放任意 JSON 值
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

// User 存放账号标识、角色、密码哈希与存储配额
type User struct {
	ID           uint   `gorm:"primaryKey"`
	Username     string `gorm:"uniqueIndex;size:64;not null"`
	PasswordHash string `gorm:"size:128;not null"`
	Role         string `gorm:"size:16;not null;default:'user'"` // 取值 admin / user
	StorageQuota int64  `gorm:"not null;default:0"`              // 字节数，0 表示不限
	// TokenVersion 随密码修改递增，使旧 JWT 失效
	TokenVersion uint `gorm:"not null;default:0"`
	CreatedAt    time.Time
	UpdatedAt    time.Time
	DeletedAt    gorm.DeletedAt `gorm:"index"`
}

// SystemSetting 存放管理员维护的服务端设置，单例固定 ID 1，已保存的选择不被部署默认值覆盖
type SystemSetting struct {
	ID                     uint `gorm:"primaryKey"`
	RegistrationEnabled    bool `gorm:"not null"`
	CustomFragmentsEnabled bool `gorm:"not null;default:false"`
	// JWTSecret 首次启动时生成，不取入库文件中的值，须保持稳定以使进程重启后会话仍然有效
	JWTSecret string `gorm:"type:text"`
	// PublicHomeVaultID 仅为无损读取旧数据保留，根路由忽略该字段
	PublicHomeVaultID string `gorm:"size:36"`
	// DefaultRecycleBinDays 为系统默认保留天数
	DefaultRecycleBinDays int    `gorm:"not null;default:30"`
	SyncMode              string `gorm:"size:32;not null;default:'user_choice'"`
	MaxLongPollWaitSec    int    `gorm:"not null;default:30"`
	MaxSyncDebounceSec    int    `gorm:"not null;default:300"`
	MaxRecycleBinDays     int    `gorm:"not null;default:3650"`
	// HistoryRetentionDays 控制快照清理，0 表示不清理
	HistoryRetentionDays int   `gorm:"not null;default:0"`
	MaxVaultStorageBytes int64 `gorm:"not null;default:0"`
	MaxUploadSizeBytes   int64 `gorm:"not null;default:0"`
	CreatedAt            time.Time
	UpdatedAt            time.Time
}

// Vault 为隔离的 Obsidian 仓库，文件、修订与设置均按 Vault 划分
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

// VaultMember 授予非所有者账号访问 Vault 的权限，所有权仍在 Vault.OwnerID 上，不可经成员管理移除
type VaultMember struct {
	ID        uint   `gorm:"primaryKey"`
	VaultID   string `gorm:"size:36;not null;uniqueIndex:idx_vault_member"`
	UserID    uint   `gorm:"not null;uniqueIndex:idx_vault_member"`
	Role      string `gorm:"size:16;not null"` // 取值 manager / participant
	CreatedAt time.Time
	UpdatedAt time.Time
}

// VaultBackup 记录 Vault 彻底删除前生成的便携归档，仅 admin 可下载或删除
type VaultBackup struct {
	ID        string `gorm:"primaryKey;size:36"`
	VaultID   string `gorm:"index;size:36;not null"`
	OwnerID   uint   `gorm:"index;not null"`
	VaultName string `gorm:"size:128;not null"`
	FileName  string `gorm:"size:255;not null"`
	Size      int64  `gorm:"not null;default:0"`
	CreatedAt time.Time
}

// VaultSetting 存放 Vault 级展示与保留设置
type VaultSetting struct {
	VaultID           string  `gorm:"primaryKey;size:36"`
	ThemeName         string  `gorm:"size:64;not null;default:'default'"`
	ThemeConfig       JSONMap `gorm:"type:json"`
	CustomHeader      string  `gorm:"type:text"`
	CustomFooter      string  `gorm:"type:text"`
	KeepDirectoryTree bool    `gorm:"not null;default:true"`
	// RecycleBinDays 仓库回收站保留天数，0 表示继承系统默认值
	RecycleBinDays int `gorm:"not null;default:0"`
	// IsPublicBlog 控制 /b/:vault_id 访问与根目录发现
	IsPublicBlog bool `gorm:"not null;default:false"`
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// VaultSyncState 存放 Vault 单调递增的服务端修订号
type VaultSyncState struct {
	VaultID           string `gorm:"primaryKey;size:36"`
	HeadRevision      int64  `gorm:"not null;default:0"`
	CompactedRevision int64  `gorm:"not null;default:0"`
	UpdatedAt         time.Time
}

// ClientDevice 表示属于某用户的客户端设备
type ClientDevice struct {
	ID       uint   `gorm:"primaryKey"`
	UserID   uint   `gorm:"index;not null;uniqueIndex:idx_user_client"`
	ClientID string `gorm:"size:64;not null;uniqueIndex:idx_user_client"`
	Name     string `gorm:"size:128"`
	// Status 取值 pending、approved 或 revoked
	Status string `gorm:"size:16;not null;default:'pending'"`
	// ApprovedAt 与 ApprovedByUserID 记录审批事件
	ApprovedAt       time.Time
	ApprovedByUserID *uint `gorm:"index"`
	LastSeenAt       time.Time
	RevokedAt        sql.NullTime `gorm:"index"`
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// DeviceVaultAccess 授予设备同步单个 Vault 的权限，不表示同步进度，进度存于 DeviceVault
type DeviceVaultAccess struct {
	ID              uint   `gorm:"primaryKey"`
	UserID          uint   `gorm:"not null;uniqueIndex:idx_user_client_vault"`
	ClientID        string `gorm:"size:64;not null;uniqueIndex:idx_user_client_vault"`
	VaultID         string `gorm:"size:36;not null;uniqueIndex:idx_user_client_vault"`
	GrantedByUserID uint   `gorm:"not null"`
	GrantedAt       time.Time
	CreatedAt       time.Time
}

// DeviceVault 存放设备在单个 Vault 上的同步游标
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

// StorageIssue 记录检出的数据库与文件系统一致性问题
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

// UserSetting 存放用户级同步参数与展示偏好，由 settingspolicy 与 Web 控制台直接读取
type UserSetting struct {
	UserID                uint    `gorm:"uniqueIndex;not null"`
	SyncInterval          int     `gorm:"not null;default:300"` // 单位为秒，默认 300（五分钟）
	LongPollWaitSec       int     `gorm:"not null;default:30"`
	SyncDebounceSec       int     `gorm:"not null;default:3"`
	DefaultRecycleBinDays int     `gorm:"not null;default:0"`
	VaultStorageBytes     int64   `gorm:"not null;default:0"`
	UploadSizeBytes       int64   `gorm:"not null;default:0"`
	ThemeName             string  `gorm:"size:64;not null;default:'default'"`
	ConsoleThemeName      string  `gorm:"size:64;not null;default:'default'"`
	WebLanguage           string  `gorm:"size:2;not null;default:'zh'"`
	ThemeConfig           JSONMap `gorm:"type:json"`
	CustomHeader          string  `gorm:"type:text"`
	CustomFooter          string  `gorm:"type:text"`
	KeepDirectoryTree     bool    `gorm:"not null;default:true"`
	// PageSize 是控制台列表每页条数，0 表示使用默认值
	PageSize    int       `gorm:"not null;default:0"`
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// File 存放 Vault 维度的文件元数据与同步墓碑
type File struct {
	ID                 uint         `gorm:"primaryKey"`
	UserID             uint         `gorm:"index;not null;uniqueIndex:idx_user_vault_path"`
	VaultID            string       `gorm:"index;size:36;uniqueIndex:idx_user_vault_path"`
	Path               string       `gorm:"index;size:512;not null;uniqueIndex:idx_user_vault_path"` // Vault 相对路径
	Type               string       `gorm:"size:16;not null"`                                        // 取值 markdown、attachment 或 config
	Hash               string       `gorm:"size:64"`                                                 // SHA-256 哈希
	Size               int64        `gorm:"not null;default:0"`
	MTime              int64        `gorm:"not null"` // 客户端修改时间戳
	Revision           int64        `gorm:"index;not null;default:0"`
	IsDeleted          bool         `gorm:"not null;default:false"` // 逻辑删除标记，正文可能仍保留在回收站
	DeletedAt          sql.NullTime `gorm:"index"`                  // 删除时间
	StorageKey         string       `gorm:"size:1024"`
	LastWriterClientID string       `gorm:"size:64"`
	LastOperationID    string       `gorm:"size:64"`
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

// Share 保存文章或文件夹的公开分享信息
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

// Collaboration 保存文件协作关系
type Collaboration struct {
	ID             uint   `gorm:"primaryKey"`
	VaultID        string `gorm:"index;size:36"`
	FileID         uint   `gorm:"index;not null"`
	OwnerID        uint   `gorm:"index;not null"`
	CollaboratorID uint   `gorm:"index;not null"`
	Status         string `gorm:"size:16;not null;default:'pending'"` // 取值 pending / accepted
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// FileHistory 保存文件的修改记录；ContentKey 指向 gzip 快照，create 无快照
type FileHistory struct {
	ID       uint   `gorm:"primaryKey"`
	VaultID  string `gorm:"size:36;not null;index:idx_history_vault_path"`
	FilePath string `gorm:"size:512;not null;index:idx_history_vault_path"`
	// PreviousPath 重命名时的旧路径
	PreviousPath string `gorm:"size:512"`
	// Action 操作类型：create / modify / delete / restore / rename
	Action   string `gorm:"size:32;not null"`
	Revision int64  `gorm:"not null"`
	// Version 每个文件路径下的版本序号
	Version int `gorm:"not null;default:1"`
	// ContentKey 快照存储键，create 记录为空
	ContentKey string    `gorm:"size:1024"`
	Hash       string    `gorm:"size:64"`
	Size       int64     `gorm:"not null;default:0"`
	Username   string    `gorm:"size:64"`
	DeviceName string    `gorm:"size:128"`
	ClientID   string    `gorm:"size:64"`
	CreatedAt  time.Time `gorm:"index"`
}
