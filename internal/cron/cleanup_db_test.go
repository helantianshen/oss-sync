package cron

import (
	"database/sql"
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

func sqlNullTime(t time.Time) sql.NullTime {
	return sql.NullTime{Time: t, Valid: true}
}

func setupCleanup(t *testing.T, fixedNow time.Time) (*Cleanup, *gorm.DB, string) {
	t.Helper()
	dataDir := t.TempDir()
	dbPath := filepath.Join(dataDir, "t.db")
	db, err := gorm.Open(sqlite.Open(dbPath), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	if err := database.AutoMigrate(db); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Storage: config.StorageConfig{DataDir: dataDir}}
	c := NewCleanup(db, cfg)
	c.now = func() time.Time { return fixedNow }
	return c, db, dataDir
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCompactTombstones_RemovesLegacyDeletedContent(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	c, db, dataDir := setupCleanup(t, now)

	db.Create(&models.User{ID: 1, Username: "u"})

	filePath := "Notes/Old.md"
	abs := filepath.Join(dataDir, "1", "Notes", "Old.md")
	writeFile(t, abs, "old content")
	db.Create(&models.File{
		UserID: 1, Path: filePath, Type: "markdown", Hash: "h", MTime: 1,
		IsDeleted: true, DeletedAt: sqlNullTime(now),
	})

	if err := c.CompactTombstones(); err != nil {
		t.Fatalf("purge: %v", err)
	}

	if _, err := os.Stat(abs); !os.IsNotExist(err) {
		t.Errorf("expected disk file removed, got err=%v", err)
	}
	var cnt int64
	db.Unscoped().Model(&models.File{}).Where("path = ?", filePath).Count(&cnt)
	if cnt != 0 {
		t.Errorf("expected DB row hard-deleted, got %d", cnt)
	}
}

func TestCompactTombstones_DoesNotRetainRecentDeletion(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	c, db, dataDir := setupCleanup(t, now)

	db.Create(&models.User{ID: 1, Username: "u"})

	filePath := "Notes/Recent.md"
	abs := filepath.Join(dataDir, "1", "Notes", "Recent.md")
	writeFile(t, abs, "recent")
	db.Create(&models.File{
		UserID: 1, Path: filePath, Type: "markdown", Hash: "h", MTime: 1,
		IsDeleted: true, DeletedAt: sqlNullTime(now),
	})

	if err := c.CompactTombstones(); err != nil {
		t.Fatalf("purge: %v", err)
	}
	if _, err := os.Stat(abs); !os.IsNotExist(err) {
		t.Errorf("recently deleted content was retained: %v", err)
	}
	var cnt int64
	db.Unscoped().Model(&models.File{}).Where("path = ?", filePath).Count(&cnt)
	if cnt != 0 {
		t.Errorf("legacy tombstone without devices was retained: %d", cnt)
	}
}

func TestCompactTombstones_WithoutActiveDevices(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	c, db, dataDir := setupCleanup(t, now)

	user := models.User{ID: 1, Username: "u"}
	db.Create(&user)
	vault := models.Vault{ID: "vault-a", OwnerID: user.ID, Name: "A", IsDefault: true}
	db.Create(&vault)

	file := models.File{
		UserID:     user.ID,
		VaultID:    vault.ID,
		Path:       "Notes/Deleted.md",
		Type:       "markdown",
		Hash:       "h",
		Size:       7,
		MTime:      1,
		Revision:   3,
		IsDeleted:  true,
		DeletedAt:  sqlNullTime(now),
		StorageKey: filestore.VaultStorageKey(vault.ID, "Notes/Deleted.md"),
	}
	abs := filestore.DiskPath(dataDir, file)
	writeFile(t, abs, "deleted")
	db.Create(&file)

	if err := c.CompactTombstones(); err != nil {
		t.Fatalf("purge: %v", err)
	}
	if _, err := os.Stat(abs); !os.IsNotExist(err) {
		t.Fatalf("expected physical content removed, got %v", err)
	}
	var count int64
	if err := db.Model(&models.File{}).Where("id = ?", file.ID).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("tombstone was retained without any active device")
	}
	var state models.VaultSyncState
	if err := db.Where("vault_id = ?", vault.ID).First(&state).Error; err != nil {
		t.Fatal(err)
	}
	if state.CompactedRevision != file.Revision {
		t.Fatalf("compacted revision=%d want %d", state.CompactedRevision, file.Revision)
	}
}

func TestCompactTombstones_WaitsForDeviceAcknowledgement(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	c, db, dataDir := setupCleanup(t, now)

	user := models.User{ID: 1, Username: "u"}
	db.Create(&user)
	vault := models.Vault{ID: "vault-a", OwnerID: user.ID, Name: "A", IsDefault: true}
	db.Create(&vault)
	db.Create(&models.VaultSyncState{VaultID: vault.ID, HeadRevision: 3})
	db.Create(&models.ClientDevice{
		UserID: user.ID, ClientID: "device-a", LastSeenAt: now,
	})
	db.Create(&models.DeviceVault{
		UserID: user.ID, ClientID: "device-a", VaultID: vault.ID, LastCursor: 2,
	})

	file := models.File{
		UserID: user.ID, VaultID: vault.ID, Path: "Notes/Deleted.md",
		Type: "markdown", Hash: "h", Size: 7, Revision: 3, IsDeleted: true,
		DeletedAt:  sqlNullTime(now),
		StorageKey: filestore.VaultStorageKey(vault.ID, "Notes/Deleted.md"),
	}
	abs := filestore.DiskPath(dataDir, file)
	writeFile(t, abs, "deleted")
	db.Create(&file)

	if err := c.CompactTombstones(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(abs); !os.IsNotExist(err) {
		t.Fatalf("expired physical content was not removed: %v", err)
	}
	var count int64
	db.Model(&models.File{}).Where("id = ?", file.ID).Count(&count)
	if count != 1 {
		t.Fatal("unacknowledged tombstone was compacted")
	}

	db.Model(&models.DeviceVault{}).
		Where("vault_id = ? AND client_id = ?", vault.ID, "device-a").
		Update("last_cursor", 3)
	if err := c.CompactTombstones(); err != nil {
		t.Fatal(err)
	}
	db.Model(&models.File{}).Where("id = ?", file.ID).Count(&count)
	if count != 0 {
		t.Fatal("acknowledged tombstone was not compacted")
	}
}

func TestCompactTombstones_IgnoresStaleDevice(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	c, db, _ := setupCleanup(t, now)
	c.Cfg.Sync.DeviceStaleDays = 30

	user := models.User{ID: 1, Username: "u"}
	db.Create(&user)
	vault := models.Vault{ID: "vault-a", OwnerID: user.ID, Name: "A", IsDefault: true}
	db.Create(&vault)
	db.Create(&models.VaultSyncState{VaultID: vault.ID, HeadRevision: 4})
	db.Create(&models.ClientDevice{
		UserID: user.ID, ClientID: "stale-device", LastSeenAt: now.AddDate(0, 0, -31),
	})
	db.Create(&models.DeviceVault{
		UserID: user.ID, ClientID: "stale-device", VaultID: vault.ID, LastCursor: 0,
	})
	file := models.File{
		UserID: user.ID, VaultID: vault.ID, Path: "Deleted.md", Type: "markdown",
		Revision: 4, IsDeleted: true, DeletedAt: sqlNullTime(now),
	}
	db.Create(&file)

	if err := c.CompactTombstones(); err != nil {
		t.Fatal(err)
	}
	var count int64
	db.Model(&models.File{}).Where("id = ?", file.ID).Count(&count)
	if count != 0 {
		t.Fatal("stale device incorrectly blocked compaction")
	}
}

func TestPurgeOrphanAttachments_RemovesUnreferencedOld(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	c, db, dataDir := setupCleanup(t, now)

	db.Create(&models.User{ID: 1, Username: "u"})

	mdPath := "Notes/Doc.md"
	mdAbs := filepath.Join(dataDir, "1", "Notes", "Doc.md")
	writeFile(t, mdAbs, "# Doc\n![kept](kept.png)")
	db.Create(&models.File{UserID: 1, Path: mdPath, Type: "markdown", Hash: "h", MTime: 1})

	keptAbs := filepath.Join(dataDir, "1", "Notes", "kept.png")
	writeFile(t, keptAbs, "img")
	setFileMtime(t, keptAbs, now.Add(-48*time.Hour))
	db.Create(&models.File{UserID: 1, Path: "Notes/kept.png", Type: "attachment", Hash: "k", MTime: 1})

	orphanAbs := filepath.Join(dataDir, "1", "orphan.png")
	writeFile(t, orphanAbs, "img")
	setFileMtime(t, orphanAbs, now.Add(-48*time.Hour))
	db.Create(&models.File{UserID: 1, Path: "orphan.png", Type: "attachment", Hash: "o", MTime: 1})

	if err := c.PurgeOrphanAttachments(); err != nil {
		t.Fatalf("purge orphans: %v", err)
	}

	if _, err := os.Stat(keptAbs); err != nil {
		t.Errorf("referenced attachment should be kept, got err=%v", err)
	}
	if _, err := os.Stat(orphanAbs); !os.IsNotExist(err) {
		t.Errorf("orphan should be removed, got err=%v", err)
	}
	var cnt int64
	db.Unscoped().Model(&models.File{}).Where("path = ?", "orphan.png").Count(&cnt)
	if cnt != 0 {
		t.Errorf("orphan DB row should be deleted, got %d", cnt)
	}
}

func TestPurgeOrphanAttachments_GracePeriodKeepsNew(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	c, db, dataDir := setupCleanup(t, now)

	db.Create(&models.User{ID: 1, Username: "u"})
	mdAbs := filepath.Join(dataDir, "1", "Doc.md")
	writeFile(t, mdAbs, "# Doc")
	db.Create(&models.File{UserID: 1, Path: "Doc.md", Type: "markdown", Hash: "h", MTime: 1})

	orphanAbs := filepath.Join(dataDir, "1", "new.png")
	writeFile(t, orphanAbs, "img")
	setFileMtime(t, orphanAbs, now.Add(-1*time.Hour))
	db.Create(&models.File{UserID: 1, Path: "new.png", Type: "attachment", Hash: "n", MTime: 1})

	if err := c.PurgeOrphanAttachments(); err != nil {
		t.Fatalf("purge: %v", err)
	}
	if _, err := os.Stat(orphanAbs); err != nil {
		t.Errorf("new orphan within 24h grace should be kept, got err=%v", err)
	}
}

func TestPurgeOrphanAttachments_IsolatesVaultReferences(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	c, db, dataDir := setupCleanup(t, now)

	user := models.User{ID: 1, Username: "u"}
	db.Create(&user)
	vaultA := models.Vault{ID: "vault-a", OwnerID: user.ID, Name: "A", IsDefault: true}
	vaultB := models.Vault{ID: "vault-b", OwnerID: user.ID, Name: "B"}
	db.Create(&vaultA)
	db.Create(&vaultB)

	mdA := models.File{
		UserID: user.ID, VaultID: vaultA.ID, Path: "Doc.md", Type: "markdown",
		StorageKey: filestore.VaultStorageKey(vaultA.ID, "Doc.md"),
	}
	attachmentA := models.File{
		UserID: user.ID, VaultID: vaultA.ID, Path: "asset.png", Type: "attachment",
		StorageKey: filestore.VaultStorageKey(vaultA.ID, "asset.png"),
	}
	attachmentB := models.File{
		UserID: user.ID, VaultID: vaultB.ID, Path: "asset.png", Type: "attachment",
		StorageKey: filestore.VaultStorageKey(vaultB.ID, "asset.png"),
	}
	for _, file := range []*models.File{&mdA, &attachmentA, &attachmentB} {
		db.Create(file)
	}
	writeFile(t, filestore.DiskPath(dataDir, mdA), "![](asset.png)")
	writeFile(t, filestore.DiskPath(dataDir, attachmentA), "a")
	writeFile(t, filestore.DiskPath(dataDir, attachmentB), "b")
	setFileMtime(t, filestore.DiskPath(dataDir, attachmentA), now.Add(-48*time.Hour))
	setFileMtime(t, filestore.DiskPath(dataDir, attachmentB), now.Add(-48*time.Hour))

	if err := c.PurgeOrphanAttachments(); err != nil {
		t.Fatalf("purge: %v", err)
	}
	if _, err := os.Stat(filestore.DiskPath(dataDir, attachmentA)); err != nil {
		t.Fatalf("referenced attachment in vault A was removed: %v", err)
	}
	if _, err := os.Stat(filestore.DiskPath(dataDir, attachmentB)); !os.IsNotExist(err) {
		t.Fatalf("orphan in vault B was retained: %v", err)
	}
}

func TestScheduler_RegisterAndStop(t *testing.T) {
	c, db, _ := setupCleanup(t, time.Now())
	_ = c
	s := NewScheduler(db, &config.Config{Storage: config.StorageConfig{DataDir: t.TempDir()}})
	s.Register()
	s.Start()
	if err := s.Stop(nil); err != nil {
		t.Errorf("stop: %v", err)
	}
}

func setFileMtime(t *testing.T, path string, mtime time.Time) {
	t.Helper()
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}
