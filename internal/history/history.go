// Package history 提供文件版本快照、修改记录和文本 diff。
//
// 快照以 gzip 形式存放在 data/vaults/<vault>/history/ 下，
// 数据库仅保存元数据。create 操作不生成快照。
package history

import (
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gorm.io/gorm"

	"github.com/oss/oss-server/internal/models"
)

// Actor 描述一次写入操作的操作者信息。
type Actor struct {
	Username   string
	DeviceName string
	ClientID   string
}

// 操作类型常量。
const (
	ActionCreate  = "create"
	ActionModify  = "modify"
	ActionDelete  = "delete"
	ActionRestore = "restore"
	ActionRename  = "rename"
)

// Dir 返回某仓库的历史快照目录。
func Dir(dataDir, vaultID string) string {
	return filepath.Join(dataDir, "vaults", vaultID, "history")
}

// ContentKey 由快照哈希生成稳定存储键。
func ContentKey(vaultID, hash string) string {
	return filepath.ToSlash(filepath.Join("vaults", vaultID, "history", hash+".gz"))
}

// DiskPath 返回快照在磁盘上的绝对路径。
func DiskPath(dataDir, contentKey string) string {
	return filepath.Join(dataDir, filepath.FromSlash(contentKey))
}

// StoreSnapshot 将 contentPath 压缩为 gzip 快照，返回存储键、哈希和大小。
func StoreSnapshot(dataDir, vaultID, contentPath string) (string, string, int64, error) {
	src, err := os.Open(contentPath)
	if err != nil {
		if os.IsNotExist(err) {
			return "", "", 0, nil
		}
		return "", "", 0, err
	}
	defer src.Close()

	dir := Dir(dataDir, vaultID)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", "", 0, err
	}
	tmp, err := os.CreateTemp(dir, ".snapshot-*")
	if err != nil {
		return "", "", 0, err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	hasher := sha256.New()
	gz := gzip.NewWriter(io.MultiWriter(tmp, hasher))
	written, copyErr := io.Copy(gz, src)
	closeGzErr := gz.Close()
	closeTmpErr := tmp.Close()
	if copyErr != nil || closeGzErr != nil || closeTmpErr != nil {
		return "", "", 0, errors.New("failed to write history snapshot")
	}
	hash := hex.EncodeToString(hasher.Sum(nil))
	key := ContentKey(vaultID, hash)
	dest := DiskPath(dataDir, key)
	if err := os.Rename(tmpPath, dest); err != nil {
		if !os.IsExist(err) {
			return "", "", 0, err
		}
	}
	return key, hash, written, nil
}

// Record 记录一次修改历史。contentPath 为空表示无快照（create）。
// db 可为事务，保证 revision 与快照元数据一致性。
func Record(db *gorm.DB, dataDir, vaultID string, actor Actor, action, filePath, prevPath, contentPath string, revision int64) error {
	if filePath == "" {
		return nil
	}
	contentKey, hash, size, err := StoreSnapshot(dataDir, vaultID, contentPath)
	if err != nil {
		return err
	}
	var version int64
	if err := db.Model(&models.FileHistory{}).
		Where("vault_id = ? AND file_path = ?", vaultID, filePath).
		Count(&version).Error; err != nil {
		return err
	}
	row := models.FileHistory{
		VaultID:      vaultID,
		FilePath:     filePath,
		PreviousPath: prevPath,
		Action:       action,
		Revision:     revision,
		Version:      int(version) + 1,
		ContentKey:   contentKey,
		Hash:         hash,
		Size:         size,
		Username:     actor.Username,
		DeviceName:   actor.DeviceName,
		ClientID:     actor.ClientID,
	}
	return db.Create(&row).Error
}

// ReadSnapshot 读取快照内容（解压）。返回 nil 表示无快照。
func ReadSnapshot(dataDir, contentKey string) ([]byte, error) {
	if contentKey == "" {
		return nil, nil
	}
	f, err := os.Open(DiskPath(dataDir, contentKey))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, err
	}
	defer gz.Close()
	return io.ReadAll(gz)
}

// IsText 依据扩展名判断快照是否可作文本 diff。
func IsText(path string) bool {
	lower := strings.ToLower(path)
	for _, ext := range []string{".md", ".txt", ".json", ".yaml", ".yml", ".css", ".js", ".html", ".csv", ".xml", ".ts", ".go"} {
		if strings.HasSuffix(lower, ext) {
			return true
		}
	}
	return false
}

// DiffLines 返回 old -> new 的简易逐行 diff（- 为旧行，+ 为新行）。
func DiffLines(oldContent, newContent []byte) []string {
	oldLines := splitLines(oldContent)
	newLines := splitLines(newContent)
	return lcsDiff(oldLines, newLines)
}

func splitLines(b []byte) []string {
	s := string(b)
	if s == "" {
		return nil
	}
	s = strings.TrimSuffix(s, "\n")
	return strings.Split(s, "\n")
}

func lcsDiff(oldLines, newLines []string) []string {
	n, m := len(oldLines), len(newLines)
	dp := make([][]int, n+1)
	for i := range dp {
		dp[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if oldLines[i] == newLines[j] {
				dp[i][j] = dp[i+1][j+1] + 1
			} else {
				if dp[i+1][j] > dp[i][j+1] {
					dp[i][j] = dp[i+1][j]
				} else {
					dp[i][j] = dp[i][j+1]
				}
			}
		}
	}
	out := make([]string, 0, n+m)
	i, j := 0, 0
	for i < n && j < m {
		if oldLines[i] == newLines[j] {
			out = append(out, " "+oldLines[i])
			i++
			j++
		} else if dp[i+1][j] >= dp[i][j+1] {
			out = append(out, "-"+oldLines[i])
			i++
		} else {
			out = append(out, "+"+newLines[j])
			j++
		}
	}
	for ; i < n; i++ {
		out = append(out, "-"+oldLines[i])
	}
	for ; j < m; j++ {
		out = append(out, "+"+newLines[j])
	}
	return out
}

// CleanupVault 删除某仓库的全部历史快照文件。
func CleanupVault(dataDir, vaultID string) error {
	return os.RemoveAll(Dir(dataDir, vaultID))
}

// CleanupExpired 删除超过保留期的文件历史记录及其快照文件。
// retentionDays 为 0 时不清理。删除快照文件前会确认没有其他记录引用同一 ContentKey。
func CleanupExpired(db *gorm.DB, dataDir, vaultID string, retentionDays int, now time.Time) error {
	if retentionDays <= 0 {
		return nil
	}
	cutoff := now.AddDate(0, 0, -retentionDays)

	// 查找过期的历史记录。
	var expired []models.FileHistory
	if err := db.Where("vault_id = ? AND created_at < ?", vaultID, cutoff).
		Find(&expired).Error; err != nil {
		return err
	}
	if len(expired) == 0 {
		return nil
	}

	// 收集待删除的 ContentKey，检查是否被未过期记录引用。
	keysToDelete := make(map[string]struct{})
	for _, h := range expired {
		if h.ContentKey == "" {
			continue
		}
		keysToDelete[h.ContentKey] = struct{}{}
	}
	if len(keysToDelete) > 0 {
		keys := make([]string, 0, len(keysToDelete))
		for k := range keysToDelete {
			keys = append(keys, k)
		}
		// 排除被未过期记录引用的 ContentKey。
		var stillReferenced []string
		if err := db.Model(&models.FileHistory{}).
			Where("vault_id = ? AND content_key IN ? AND created_at >= ?", vaultID, keys, cutoff).
			Distinct("content_key").
			Pluck("content_key", &stillReferenced).Error; err != nil {
			return err
		}
		for _, k := range stillReferenced {
			delete(keysToDelete, k)
		}
	}

	// 先删除无引用的快照文件，全部成功后再删数据库记录。
	// 若文件删除失败，DB 行保留，下次 cron 可重试，避免孤儿快照永久泄漏。
	for k := range keysToDelete {
		diskPath := DiskPath(dataDir, k)
		if err := os.Remove(diskPath); err != nil && !os.IsNotExist(err) {
			return err
		}
	}

	// 删除数据库记录。
	ids := make([]uint, len(expired))
	for i, h := range expired {
		ids[i] = h.ID
	}
	if err := db.Where("id IN ?", ids).Delete(&models.FileHistory{}).Error; err != nil {
		return err
	}
	return nil
}
