package filestore

import (
	"path/filepath"
	"strconv"

	"github.com/oss/oss-server/internal/models"
)

// VaultStorageKey 返回 Vault 文件的标准存储键。
func VaultStorageKey(vaultID, relativePath string) string {
	return filepath.ToSlash(filepath.Join("vaults", vaultID, "files", filepath.FromSlash(relativePath)))
}

// DiskPath 兼容标准 Vault 存储路径和旧版用户目录。
func DiskPath(dataDir string, file models.File) string {
	if file.StorageKey != "" {
		return filepath.Join(dataDir, filepath.FromSlash(file.StorageKey))
	}
	return filepath.Join(
		dataDir,
		strconv.FormatUint(uint64(file.UserID), 10),
		filepath.FromSlash(file.Path),
	)
}
