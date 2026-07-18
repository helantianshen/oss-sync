package reconcile

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/oss/oss-server/internal/config"
	"github.com/oss/oss-server/internal/database"
	"github.com/oss/oss-server/internal/filestore"
	"github.com/oss/oss-server/internal/models"
)

func setupReconciler(t *testing.T) (*Reconciler, *gorm.DB, string, models.Vault) {
	t.Helper()
	dataDir := t.TempDir()
	db, err := gorm.Open(
		sqlite.Open(filepath.Join(dataDir, "test.db")),
		&gorm.Config{Logger: logger.Default.LogMode(logger.Silent)},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.AutoMigrate(db); err != nil {
		t.Fatal(err)
	}
	user := models.User{Username: "owner", PasswordHash: "hash", Role: "user"}
	if err := db.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	vault := models.Vault{ID: "vault-a", OwnerID: user.ID, Name: "A", IsDefault: true}
	if err := db.Create(&vault).Error; err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Storage: config.StorageConfig{DataDir: dataDir}}
	reconciler := New(db, cfg)
	reconciler.now = func() time.Time {
		return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	}
	return reconciler, db, dataDir, vault
}

func TestRunRestoresVerifiedBackup(t *testing.T) {
	reconciler, db, dataDir, vault := setupReconciler(t)
	file := storedFile(vault, "Notes/A.md", "old")
	if err := db.Create(&file).Error; err != nil {
		t.Fatal(err)
	}
	target := filestore.DiskPath(dataDir, file)
	writeDisk(t, target, "new")
	backup := target + ".backup-crash"
	writeDisk(t, backup, "old")

	report, err := reconciler.Run(true)
	if err != nil {
		t.Fatal(err)
	}
	if report.FilesRepaired != 1 {
		t.Fatalf("repaired=%d want 1", report.FilesRepaired)
	}
	raw, err := os.ReadFile(target)
	if err != nil || string(raw) != "old" {
		t.Fatalf("restored target=%q err=%v", raw, err)
	}
	if _, err := os.Stat(backup); !os.IsNotExist(err) {
		t.Fatalf("backup was not consumed: %v", err)
	}
}

func TestRunRecoversRenameCrashFromOrphan(t *testing.T) {
	reconciler, db, dataDir, vault := setupReconciler(t)
	file := storedFile(vault, "Notes/Old.md", "content")
	if err := db.Create(&file).Error; err != nil {
		t.Fatal(err)
	}
	orphan := filepath.Join(dataDir, "vaults", vault.ID, "files", "Notes", "New.md")
	writeDisk(t, orphan, "content")

	report, err := reconciler.Run(true)
	if err != nil {
		t.Fatal(err)
	}
	if report.FilesRepaired != 1 {
		t.Fatalf("repaired=%d want 1", report.FilesRepaired)
	}
	target := filestore.DiskPath(dataDir, file)
	raw, err := os.ReadFile(target)
	if err != nil || string(raw) != "content" {
		t.Fatalf("recovered target=%q err=%v", raw, err)
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatalf("orphan source remains: %v", err)
	}
}

func TestRunRecordsAndResolvesMissingFile(t *testing.T) {
	reconciler, db, dataDir, vault := setupReconciler(t)
	file := storedFile(vault, "Missing.md", "content")
	if err := db.Create(&file).Error; err != nil {
		t.Fatal(err)
	}

	report, err := reconciler.Run(true)
	if err != nil {
		t.Fatal(err)
	}
	if report.OpenIssues != 1 {
		t.Fatalf("open issues=%d want 1", report.OpenIssues)
	}
	var issue models.StorageIssue
	if err := db.Where("kind = ? AND resolved_at IS NULL", "missing").First(&issue).Error; err != nil {
		t.Fatal(err)
	}

	writeDisk(t, filestore.DiskPath(dataDir, file), "content")
	report, err = reconciler.Run(true)
	if err != nil {
		t.Fatal(err)
	}
	if report.OpenIssues != 0 {
		t.Fatalf("open issues after repair=%d", report.OpenIssues)
	}
	if err := db.First(&issue, issue.ID).Error; err != nil || !issue.ResolvedAt.Valid {
		t.Fatalf("issue was not resolved: %+v err=%v", issue, err)
	}
}

func TestRunCleansTempsAndQuarantinesOrphans(t *testing.T) {
	reconciler, _, dataDir, vault := setupReconciler(t)
	tmp := filepath.Join(dataDir, "vaults", vault.ID, "tmp", "upload-crash")
	orphan := filepath.Join(dataDir, "vaults", vault.ID, "files", "orphan.bin")
	writeDisk(t, tmp, "partial")
	writeDisk(t, orphan, "orphan")

	report, err := reconciler.Run(true)
	if err != nil {
		t.Fatal(err)
	}
	if report.TempsRemoved != 1 || report.FilesQuarantined != 1 {
		t.Fatalf("unexpected report: %+v", report)
	}
	if _, err := os.Stat(tmp); !os.IsNotExist(err) {
		t.Fatalf("temp remains: %v", err)
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatalf("orphan remains in canonical storage: %v", err)
	}
	matches, err := filepath.Glob(filepath.Join(dataDir, "recovery", vault.ID, "*", "files", "orphan.bin"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("quarantined orphan not found: %v err=%v", matches, err)
	}
}

func TestRunRemovesContentRetainedForDeletedRows(t *testing.T) {
	reconciler, db, dataDir, vault := setupReconciler(t)
	file := storedFile(vault, "Deleted.md", "deleted-content")
	file.IsDeleted = true
	file.DeletedAt = sql.NullTime{Time: time.Now(), Valid: true}
	if err := db.Create(&file).Error; err != nil {
		t.Fatal(err)
	}
	target := filestore.DiskPath(dataDir, file)
	writeDisk(t, target, "deleted-content")
	backup := target + ".backup-delete-crash"
	writeDisk(t, backup, "deleted-content")

	report, err := reconciler.Run(true)
	if err != nil {
		t.Fatal(err)
	}
	if report.DeletedRemoved != 2 {
		t.Fatalf("deleted content removed=%d want 2", report.DeletedRemoved)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("deleted canonical content remains: %v", err)
	}
	if _, err := os.Stat(backup); !os.IsNotExist(err) {
		t.Fatalf("deleted backup content remains: %v", err)
	}
}

func storedFile(vault models.Vault, path, content string) models.File {
	digest := sha256.Sum256([]byte(content))
	return models.File{
		UserID:     vault.OwnerID,
		VaultID:    vault.ID,
		Path:       path,
		Type:       "markdown",
		Hash:       hex.EncodeToString(digest[:]),
		Size:       int64(len(content)),
		Revision:   1,
		StorageKey: filestore.VaultStorageKey(vault.ID, path),
	}
}

func writeDisk(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
