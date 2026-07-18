package database

import (
	"path/filepath"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/oss/oss-server/internal/models"
)

func TestAutoMigrateCreatesDefaultVaultAndBackfillsLegacyRows(t *testing.T) {
	db, err := gorm.Open(
		sqlite.Open(filepath.Join(t.TempDir(), "migration.db")),
		&gorm.Config{Logger: logger.Default.LogMode(logger.Silent)},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := AutoMigrate(db); err != nil {
		t.Fatal(err)
	}

	user := models.User{Username: "legacy", PasswordHash: "hash", Role: "user"}
	if err := db.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	file := models.File{
		UserID: user.ID,
		Path:   "Notes/Legacy.md",
		Type:   "markdown",
		Hash:   "legacy-hash",
		Size:   42,
		MTime:  1,
	}
	if err := db.Create(&file).Error; err != nil {
		t.Fatal(err)
	}
	share := models.Share{
		ShareID:    "legacy",
		UserID:     user.ID,
		TargetPath: file.Path,
	}
	if err := db.Create(&share).Error; err != nil {
		t.Fatal(err)
	}

	if err := AutoMigrate(db); err != nil {
		t.Fatal(err)
	}

	var vault models.Vault
	if err := db.Where("owner_id = ? AND is_default = ?", user.ID, true).First(&vault).Error; err != nil {
		t.Fatalf("default vault: %v", err)
	}
	var migratedFile models.File
	if err := db.First(&migratedFile, file.ID).Error; err != nil {
		t.Fatal(err)
	}
	if migratedFile.VaultID != vault.ID || migratedFile.Revision <= 0 {
		t.Fatalf("file was not backfilled: %+v", migratedFile)
	}
	var migratedShare models.Share
	if err := db.First(&migratedShare, "share_id = ?", share.ShareID).Error; err != nil {
		t.Fatal(err)
	}
	if migratedShare.VaultID != vault.ID {
		t.Fatalf("share vault=%q want %q", migratedShare.VaultID, vault.ID)
	}
	var state models.VaultSyncState
	if err := db.First(&state, "vault_id = ?", vault.ID).Error; err != nil {
		t.Fatal(err)
	}
	if state.HeadRevision != migratedFile.Revision {
		t.Fatalf("head=%d file revision=%d", state.HeadRevision, migratedFile.Revision)
	}
	if vault.StorageUsed != file.Size {
		t.Fatalf("storage_used=%d want %d", vault.StorageUsed, file.Size)
	}

	firstRevision := migratedFile.Revision
	if err := AutoMigrate(db); err != nil {
		t.Fatal(err)
	}
	if err := db.First(&migratedFile, file.ID).Error; err != nil {
		t.Fatal(err)
	}
	if migratedFile.Revision != firstRevision {
		t.Fatalf("migration is not idempotent: revision=%d want %d", migratedFile.Revision, firstRevision)
	}
}
